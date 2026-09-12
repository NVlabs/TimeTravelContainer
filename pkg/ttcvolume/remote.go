// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	remoteProtocolVersion  = 4
	remoteHeaderSize       = 112
	remoteExtentBytes      = 1 << 20
	remoteMaxErrorBytes    = 4096
	remoteMaxMetadataBytes = 5 << 20
)

var remoteMagic = [4]byte{'T', 'T', 'C', 'F'}

// RemoteToken fences filesystem reads to one distributed fork transaction.
type RemoteToken [16]byte

// RemoteSourceKey is the stable digest of a repository's logical mount source.
type RemoteSourceKey [32]byte

type remoteMessageType uint8

const (
	remotePull remoteMessageType = iota + 1
	remoteData
	remoteError
	remoteMetadataPull
)

type remoteHeader struct {
	type_     remoteMessageType
	token     RemoteToken
	requestID uint32
	source    RemoteSourceKey
	object    [40]byte
	offset    uint64
	length    uint32
}

func (h remoteHeader) marshal(dst []byte) {
	copy(dst[:4], remoteMagic[:])
	dst[4] = remoteProtocolVersion
	dst[5] = byte(h.type_)
	dst[6], dst[7] = 0, 0
	copy(dst[8:24], h.token[:])
	binary.BigEndian.PutUint32(dst[24:28], h.requestID)
	copy(dst[28:60], h.source[:])
	copy(dst[60:100], h.object[:])
	binary.BigEndian.PutUint64(dst[100:108], h.offset)
	binary.BigEndian.PutUint32(dst[108:112], h.length)
}

func readRemoteHeader(reader io.Reader) (remoteHeader, error) {
	var encoded [remoteHeaderSize]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return remoteHeader{}, err
	}
	if string(encoded[:4]) != string(remoteMagic[:]) || encoded[4] != remoteProtocolVersion || encoded[6] != 0 || encoded[7] != 0 {
		return remoteHeader{}, errors.New("invalid TTC filesystem range header")
	}
	var header remoteHeader
	header.type_ = remoteMessageType(encoded[5])
	copy(header.token[:], encoded[8:24])
	header.requestID = binary.BigEndian.Uint32(encoded[24:28])
	copy(header.source[:], encoded[28:60])
	copy(header.object[:], encoded[60:100])
	header.offset = binary.BigEndian.Uint64(encoded[100:108])
	header.length = binary.BigEndian.Uint32(encoded[108:112])
	return header, nil
}

func writeRemoteMessage(writer io.Writer, header remoteHeader, payload []byte) error {
	var encoded [remoteHeaderSize]byte
	header.length = uint32(len(payload))
	header.marshal(encoded[:])
	if err := writeAll(writer, encoded[:]); err != nil {
		return err
	}
	if err := writeAll(writer, payload); err != nil {
		return err
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

// RemoteObjectSource authorizes the immutable objects visible through one
// sealed repository generation. Objects maps object IDs to their exact sizes.
type RemoteObjectSource struct {
	Key        RemoteSourceKey
	Repository *Repository
	// Reader may serve immutable content from a shared pack rather than a live
	// repository. Exactly one of Reader and Repository must be set.
	Reader  RemoteObjectReader
	Catalog RemoteObjectCatalog
	Objects map[string]int64
}

// RemoteObjectCatalog resolves immutable graph metadata without exposing the
// provider's filesystem layout. Shared snapshot packs and live repositories
// both implement this boundary.
type RemoteObjectCatalog interface {
	RemoteObject(ttcsnapshot.ObjectID) (ttcsnapshot.Object, error)
}

type repositoryObjectCatalog struct{ repository *Repository }

func (c repositoryObjectCatalog) RemoteObject(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	return c.repository.remoteGraphObject(id)
}

func (r *Repository) remoteGraphObject(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	if r == nil || r.graph == nil {
		return ttcsnapshot.Object{}, os.ErrInvalid
	}
	object, err := r.graph.ResolveObject(id)
	if err != nil {
		return ttcsnapshot.Object{}, err
	}
	snapshot, ok := r.graph.Snapshot(id.Snapshot)
	// Catalog-expansion origins stay working so later pathname faults can
	// allocate more local stubs. Expanded objects are nevertheless immutable:
	// they retain a foreign identity and are never modified in place. Ordinary
	// branch-working objects remain ineligible until the branch is sealed.
	if !ok || (snapshot.State == ttcsnapshot.SnapshotWorking && object.SourceCatalog == "") || object.State != ttcsnapshot.ObjectLive {
		return ttcsnapshot.Object{}, fmt.Errorf("filesystem object %s is not in an immutable generation", id)
	}
	// A repository may itself be a lazy target and then become the source of a
	// later hop before any guest pathname has reached this file. Resolving graph
	// metadata alone does not run prepareDeferredInode, so register the bulk
	// object's inherited source here as well. Otherwise the later hop receives a
	// valid immutable object ID with no relay mapping, and a returning A-to-B-to-A
	// path fails as a missing local file (or waits forever on its own relay).
	if object.ID.Kind == ttcsnapshot.ObjectKindFile {
		if err := r.prepareDeferredFileObject(object.ID, int64(object.BulkLength)); err != nil {
			return ttcsnapshot.Object{}, err
		}
	}
	return object, nil
}

// RemoteObjectReader reads an immutable object range. Implementations may be
// backed by a repository, an indexed pack chain, or another fenced source.
type RemoteObjectReader interface {
	ReadObjectRange(object string, destination []byte, offset int64) (int, error)
}

type repositoryObjectReader struct{ repository *Repository }

func (r repositoryObjectReader) ReadObjectRange(object string, destination []byte, offset int64) (int, error) {
	if r.repository == nil {
		return 0, os.ErrInvalid
	}
	file, err := r.repository.openObject(object, os.O_RDONLY)
	if err != nil {
		return 0, err
	}
	n, readErr := file.ReadAt(destination, offset)
	closeErr := file.Close()
	if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(destination)) {
		return n, errors.Join(readErr, closeErr)
	}
	return n, closeErr
}

// ServeRemoteObjects serves range requests until conn closes. The token and
// object allowlist fence a client to one captured fork generation.
func ServeRemoteObjects(conn io.ReadWriteCloser, token RemoteToken, sources []RemoteObjectSource) error {
	defer conn.Close()
	byKey := make(map[RemoteSourceKey]RemoteObjectSource, len(sources))
	for _, source := range sources {
		if source.Reader == nil && source.Repository != nil {
			source.Reader = repositoryObjectReader{repository: source.Repository}
		}
		if source.Catalog == nil && source.Repository != nil {
			source.Catalog = repositoryObjectCatalog{repository: source.Repository}
		}
		if source.Reader == nil {
			continue
		}
		byKey[source.Key] = source
	}
	for {
		request, err := readRemoteHeader(conn)
		if err != nil {
			return err
		}
		if request.token != token {
			return writeRemoteFailure(conn, request, "invalid or unauthorized filesystem range request")
		}
		source, ok := byKey[request.source]
		object := strings.TrimRight(string(request.object[:]), "\x00")
		id, parseErr := ttcsnapshot.ParseObjectID(object)
		if !ok || parseErr != nil {
			return writeRemoteFailure(conn, request, "filesystem object or range is outside the captured generation")
		}
		if request.type_ == remoteMetadataPull {
			if request.offset != 0 || request.length != 0 || source.Catalog == nil {
				return writeRemoteFailure(conn, request, "invalid filesystem metadata request")
			}
			metadata, err := source.Catalog.RemoteObject(id)
			if err != nil {
				return writeRemoteFailure(conn, request, err.Error())
			}
			payload, err := json.Marshal(metadata)
			if err != nil || len(payload) > remoteMaxMetadataBytes {
				return writeRemoteFailure(conn, request, "filesystem metadata response is too large")
			}
			response := request
			response.type_ = remoteData
			if err := writeRemoteMessage(conn, response, payload); err != nil {
				return err
			}
			continue
		}
		if request.type_ != remotePull || request.length == 0 || request.length > remoteExtentBytes || request.offset%remoteExtentBytes != 0 || id.Kind != ttcsnapshot.ObjectKindFile {
			return writeRemoteFailure(conn, request, "invalid or unauthorized filesystem range request")
		}
		size, allowed := source.Objects[object]
		if !allowed && source.Catalog != nil {
			metadata, err := source.Catalog.RemoteObject(id)
			if err == nil && metadata.BulkLength <= uint64(^uint64(0)>>1) {
				size, allowed = int64(metadata.BulkLength), true
			}
		}
		if !allowed || request.offset >= uint64(size) || uint64(request.length) > uint64(size)-request.offset {
			return writeRemoteFailure(conn, request, "filesystem object or range is outside the captured generation")
		}
		payload := make([]byte, request.length)
		if source.Repository != nil {
			if err := source.Repository.ensureObjectRange(object, size, int64(request.offset), int64(request.length)); err != nil {
				return writeRemoteFailure(conn, request, err.Error())
			}
		}
		n, readErr := source.Reader.ReadObjectRange(object, payload, int64(request.offset))
		if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(payload)) {
			return writeRemoteFailure(conn, request, readErr.Error())
		}
		if n != len(payload) {
			return writeRemoteFailure(conn, request, fmt.Sprintf("short filesystem object read: %d/%d", n, len(payload)))
		}
		response := request
		response.type_ = remoteData
		if err := writeRemoteMessage(conn, response, payload); err != nil {
			return err
		}
	}
}

func writeRemoteError(writer io.Writer, request remoteHeader, message string) error {
	if len(message) > remoteMaxErrorBytes {
		message = message[:remoteMaxErrorBytes]
	}
	request.type_ = remoteError
	return writeRemoteMessage(writer, request, []byte(message))
}

func writeRemoteFailure(writer io.Writer, request remoteHeader, message string) error {
	if err := writeRemoteError(writer, request, message); err != nil {
		return err
	}
	return errors.New(message)
}

type remoteObjectClient struct {
	mu         sync.Mutex
	users      sync.WaitGroup
	conn       io.ReadWriteCloser
	token      RemoteToken
	source     RemoteSourceKey
	next       uint32
	complete   map[string]bool
	objects    map[string]int64
	sources    map[string]string
	repository *Repository
	branchID   string
	catalog    string
}

func (r *Repository) prepareDeferredInode(inode *Inode) error {
	if inode == nil || inode.Type != "regular" || inode.Object == "" {
		return nil
	}
	id, err := ttcsnapshot.ParseObjectID(inode.Object)
	if err != nil {
		return err
	}
	if err := r.prepareDeferredFileObject(id, inode.Size); err != nil {
		return err
	}
	if _, _, deferred := r.graph.DeferredSource(id); deferred {
		// A deferred file is immutable foreign data. Its first write must always
		// path-copy into the branch working generation.
		inode.DataGeneration = 0
	}
	return nil
}

func (r *Repository) prepareDeferredFileObject(id ttcsnapshot.ObjectID, size int64) error {
	catalog, sourceID, deferred := r.graph.DeferredSource(id)
	if !deferred {
		return nil
	}
	object, err := r.graph.ResolveObject(id)
	if err != nil {
		return err
	}
	if size < 0 || object.ID.Kind != ttcsnapshot.ObjectKindFile || object.BulkLength != uint64(size) || sourceID.Kind != ttcsnapshot.ObjectKindFile {
		return fmt.Errorf("deferred file %s metadata disagrees with length %d", id, size)
	}
	pending, err := r.remoteObjectPending(id.String())
	if err != nil {
		return err
	}
	file, openErr := r.data.Open(id, size)
	if openErr == nil {
		openErr = file.Close()
	}
	if openErr == nil && !pending {
		return nil
	} else if openErr != nil && !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	r.remoteMu.Lock()
	remote := r.remoteByCatalog[catalog]
	if remote != nil {
		remote.objects[id.String()] = size
		remote.sources[id.String()] = sourceID.String()
		r.remoteByObject[id.String()] = remote
	}
	r.remoteMu.Unlock()
	if remote == nil {
		return fmt.Errorf("deferred filesystem catalog %q has no attached authoritative source", catalog)
	}
	if err := r.markRemoteObjectPending(id.String(), sourceID.String()); err != nil {
		return err
	}
	return nil
}

// AttachRemoteObjectSource installs a donated, pre-connected range stream.
// The Sentry performs no socket creation or host path traversal.
func (r *Repository) AttachRemoteObjectSource(conn io.ReadWriteCloser, token RemoteToken, source RemoteSourceKey, branchID string) error {
	if r == nil || conn == nil {
		return errors.New("remote filesystem source requires repository and connection")
	}
	// A source attachment changes the resolver set consumed by the
	// repository-wide independence barrier. Serialize the two operations so a
	// concurrent sibling migration cannot have its newly attached catalog
	// detached by an older branch's drain.
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	branch, err := r.Branch(branchID)
	if err != nil {
		return err
	}
	remote := &remoteObjectClient{conn: conn, token: token, source: source, next: 1, complete: make(map[string]bool), objects: make(map[string]int64), sources: make(map[string]string), repository: r, branchID: branchID, catalog: branch.RemoteCatalog}
	if branch.RemoteCatalog != "" {
		if branch.RemoteCatalogSnapshot == 0 {
			return errors.New("remote filesystem branch has no catalog expansion origin")
		}
		if err := r.graph.AttachDeferredResolver(branch.RemoteCatalog, ttcsnapshot.SnapshotID(branch.RemoteCatalogSnapshot), remote.pullMetadata); err != nil {
			return err
		}
		r.remoteMu.Lock()
		if r.remotes == nil {
			r.remotes = make(map[*remoteObjectClient]struct{})
			r.remoteByObject = make(map[string]*remoteObjectClient)
			r.remoteByCatalog = make(map[string]*remoteObjectClient)
		}
		r.remotes[remote] = struct{}{}
		r.remoteByCatalog[branch.RemoteCatalog] = remote
		r.remoteMu.Unlock()
		return nil
	}
	objects, err := r.BranchObjects(branchID)
	if err != nil {
		return err
	}
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	if r.remotes == nil {
		r.remotes = make(map[*remoteObjectClient]struct{})
		r.remoteByObject = make(map[string]*remoteObjectClient)
		r.remoteByCatalog = make(map[string]*remoteObjectClient)
	}
	for object, size := range objects {
		sourceObject, pending, err := r.remoteSourceObject(object)
		if err != nil {
			return err
		}
		if !pending {
			continue
		}
		remote.objects[object] = size
		remote.sources[object] = sourceObject
		// The immutable graph object identifies the authorized source bytes.
		// Prefer the newest live attachment over a reader retained by an older
		// branch.
		r.remoteByObject[object] = remote
	}
	// Target-local IDs are not content hashes. A newer attachment for the
	// same immutable source identity can nevertheless take over every sparse
	// local placeholder backed by that source.
	for localObject, owner := range r.remoteByObject {
		if owner == remote {
			continue
		}
		sourceObject, pending, err := r.remoteSourceObject(localObject)
		if err != nil {
			return err
		}
		if !pending {
			continue
		}
		matched := false
		for _, candidate := range remote.sources {
			if candidate == sourceObject {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		id, err := ttcsnapshot.ParseObjectID(localObject)
		if err != nil {
			return err
		}
		object, ok := r.graph.Object(id)
		if !ok {
			return fmt.Errorf("remote target object %s is absent from graph", localObject)
		}
		remote.objects[localObject] = int64(object.BulkLength)
		remote.sources[localObject] = sourceObject
		r.remoteByObject[localObject] = remote
	}
	if len(remote.objects) == 0 {
		return conn.Close()
	}
	r.remotes[remote] = struct{}{}
	return nil
}

// BranchObjects returns the immutable object allowlist referenced by branch.
func (r *Repository) BranchObjects(branchID string) (map[string]int64, error) {
	branch, err := r.Branch(branchID)
	if err != nil {
		return nil, err
	}
	return r.branchObjects(branch)
}

func (r *Repository) branchObjects(branch Branch) (map[string]int64, error) {
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return nil, err
	}
	if err := r.completeMetadata(state); err != nil {
		return nil, err
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	objects := make(map[string]int64)
	for _, inode := range state.inodes {
		if inode.Type == "regular" && inode.Object != "" {
			objects[inode.Object] = inode.Size
		}
	}
	return objects, nil
}

// BranchRemoteSourceObjects returns the immutable source identities needed to
// serve every lazy object reachable from branchID. Portable shared-pack
// imports allocate new node-local object IDs, so the source identity recorded
// in each pending marker (rather than the local placeholder name) is the key
// understood by the pack reader.
func (r *Repository) BranchRemoteSourceObjects(branchID string) (map[string]int64, error) {
	objects, err := r.BranchObjects(branchID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(objects))
	for object, size := range objects {
		source, pending, err := r.remoteSourceObject(object)
		if err != nil {
			return nil, err
		}
		if pending {
			if previous, ok := result[source]; ok && previous != size {
				return nil, fmt.Errorf("remote source object %s has inconsistent sizes %d and %d", source, previous, size)
			}
			result[source] = size
		}
	}
	return result, nil
}

// MaterializeRemoteObjects fetches all still-remote objects for a clean
// source handoff or local durable publication.
func (r *Repository) MaterializeRemoteObjects() error {
	// The cross-instance repository lock is also a guest-mutation fence. A
	// running target may otherwise path-copy an unexpanded deferred child after
	// the scan below but before its resolver is detached, leaving an apparently
	// independent branch that still needs the source.
	return r.withLock(r.materializeRemoteObjectsLocked)
}

func (r *Repository) materializeRemoteObjectsLocked() error {
	// Multiple distributed transactions in one physical Sentry share this
	// repository. Their eager drains may arrive concurrently, but detaching
	// remote catalogs is a repository-wide barrier and must execute exactly one
	// at a time.
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	// Resolve every still-foreign namespace before detaching its provider.
	// This is an explicit independence barrier and is allowed to be O(total
	// metadata); ordinary remote start and pathname access remain lazy.
	r.remoteMu.Lock()
	var deferredBranches []string
	for remote := range r.remotes {
		if remote.catalog != "" {
			deferredBranches = append(deferredBranches, remote.branchID)
		}
	}
	r.remoteMu.Unlock()
	for _, branch := range deferredBranches {
		// MaterializeRemoteObjects holds the repository operation lock, so using
		// the public Branch helper here would recursively acquire it. Flush the
		// bounded metadata tail directly and construct the same authoritative
		// graph-root view while the mutation fence remains held.
		if err := r.flushMetadataBatchLocked(branch); err != nil {
			return err
		}
		branchRecord, err := r.readBranch(branch)
		if err != nil {
			return err
		}
		root, ok := r.graph.Root(graphRootName(branch))
		if !ok {
			return fmt.Errorf("branch %q has no snapshot graph root", branch)
		}
		branchRecord.Root = root.String()
		if _, err := r.branchObjects(branchRecord); err != nil {
			return err
		}
	}
	r.remoteMu.Lock()
	remotes := make([]*remoteObjectClient, 0, len(r.remotes))
	for remote := range r.remotes {
		remotes = append(remotes, remote)
	}
	r.remoteMu.Unlock()
	if len(remotes) == 0 {
		return nil
	}
	for _, remote := range remotes {
		// A newer branch may have replaced this client's source attachment. Do
		// not contact a stale connection for it; the current owner below will
		// materialize the same immutable bytes.
		r.remoteMu.Lock()
		owned := make(map[string]int64)
		for object, size := range remote.objects {
			if r.remoteByObject[object] == remote {
				owned[object] = size
			}
		}
		r.remoteMu.Unlock()
		for object, size := range owned {
			if err := remote.ensure(object, size, 0, size); err != nil {
				return err
			}
		}
		// Serialize behind any reader that obtained remote before the final
		// extent completed. Once detached, all future reads are local.
		remote.mu.Lock()
		r.remoteMu.Lock()
		delete(r.remotes, remote)
		for object, owner := range r.remoteByObject {
			if owner == remote {
				delete(r.remoteByObject, object)
			}
		}
		for catalog, owner := range r.remoteByCatalog {
			if owner == remote {
				delete(r.remoteByCatalog, catalog)
				r.graph.DetachDeferredResolver(catalog)
			}
		}
		r.remoteMu.Unlock()
		// A reader may have selected this client immediately before it was
		// removed from remoteByObject. Wait for every such pinned selection
		// before closing the stream; c.mu alone cannot distinguish a queued
		// pre-detach reader from a new use of an already closed connection.
		remote.mu.Unlock()
		remote.users.Wait()
		remote.mu.Lock()
		err := remote.conn.Close()
		remote.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *remoteObjectClient) pullMetadata(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	payload, err := c.request(remoteMetadataPull, id.String(), 0, 0)
	if err != nil {
		return ttcsnapshot.Object{}, err
	}
	var object ttcsnapshot.Object
	if err := json.Unmarshal(payload, &object); err != nil {
		return ttcsnapshot.Object{}, err
	}
	if object.ID != id {
		return ttcsnapshot.Object{}, fmt.Errorf("remote filesystem metadata identifies %s, want %s", object.ID, id)
	}
	return object, nil
}

// EnsureDataRange materializes the complete immutable file on its first guest
// access. The transport remains bounded to 1 MiB messages, but cache state is
// deliberately only absent or complete: TTC does not depend on sparse host
// files or expose a partly populated file to the guest.
func (r *Repository) EnsureDataRange(branchID string, ino uint64, offset, length int64) error {
	if offset < 0 || length < 0 {
		return os.ErrInvalid
	}
	branch, err := r.readBranch(branchID)
	if err != nil {
		return err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return err
	}
	inode, err := r.metadataInode(state, ino)
	if err != nil {
		return err
	}
	if inode.Type != "regular" {
		return os.ErrInvalid
	}
	if offset >= inode.Size || length == 0 {
		return nil
	}
	if length > inode.Size-offset {
		length = inode.Size - offset
	}
	return r.ensureObjectRange(inode.Object, inode.Size, offset, length)
}

func (r *Repository) ensureObjectRange(object string, size, offset, length int64) error {
	// Empty files have no remote bytes to materialize. Return before pinning the
	// reader; otherwise the unmatched WaitGroup reference makes source handoff
	// wait forever whenever a fork captures a file between O_TRUNC and write.
	if length == 0 {
		return nil
	}
	r.remoteMu.Lock()
	remote := r.remoteByObject[object]
	if remote != nil {
		remote.users.Add(1)
	}
	r.remoteMu.Unlock()
	if remote == nil {
		pending, err := r.remoteObjectPending(object)
		if err != nil {
			return err
		}
		if pending {
			return fmt.Errorf("remote filesystem object %q has no attached authoritative source", object)
		}
		return nil
	}
	defer remote.users.Done()
	return remote.ensure(object, size, offset, length)
}

func (c *remoteObjectClient) ensure(object string, size, offset, length int64) error {
	if !validObjectID(object) || size < 0 || offset < 0 || length < 0 || offset > size || length > size-offset {
		return errors.New("invalid remote filesystem object range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.complete[object] {
		return nil
	}
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		return err
	}
	reader := &remoteWholeFileReader{client: c, object: object, size: uint64(size)}
	if err := c.repository.replaceRemoteObject(id, object, reader, size); err != nil {
		return err
	}
	if err := c.repository.clearRemoteObjectPending(object); err != nil {
		return err
	}
	c.complete[object] = true
	return nil
}

type remoteWholeFileReader struct {
	client *remoteObjectClient
	object string
	size   uint64
	offset uint64
	buffer []byte
}

func (reader *remoteWholeFileReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	if len(reader.buffer) == 0 {
		if reader.offset == reader.size {
			return 0, io.EOF
		}
		want := uint64(remoteExtentBytes)
		if remaining := reader.size - reader.offset; want > remaining {
			want = remaining
		}
		data, err := reader.client.pull(reader.object, reader.offset, uint32(want))
		if err != nil {
			return 0, err
		}
		reader.buffer = data
	}
	n := copy(destination, reader.buffer)
	reader.buffer = reader.buffer[n:]
	reader.offset += uint64(n)
	return n, nil
}

func (c *remoteObjectClient) pull(object string, offset uint64, length uint32) ([]byte, error) {
	// sources is populated while deferred graph nodes are expanded. Those
	// expansions may run concurrently with a first data read for another file.
	// The repository remote lock protects the per-client object/source maps;
	// c.mu separately serializes the byte stream and request IDs.
	c.repository.remoteMu.Lock()
	remoteObject := c.sources[object]
	c.repository.remoteMu.Unlock()
	if !validObjectID(remoteObject) || len(remoteObject) > 40 {
		return nil, fmt.Errorf("invalid object ID %q", object)
	}
	return c.request(remotePull, remoteObject, offset, length)
}

func (c *remoteObjectClient) request(kind remoteMessageType, object string, offset uint64, length uint32) ([]byte, error) {
	if len(object) > len(remoteHeader{}.object) {
		return nil, errors.New("remote filesystem object identity is too long")
	}
	c.next++
	request := remoteHeader{type_: kind, token: c.token, requestID: c.next, source: c.source, offset: offset, length: length}
	copy(request.object[:], object)
	var encoded [remoteHeaderSize]byte
	request.marshal(encoded[:])
	if err := writeAll(c.conn, encoded[:]); err != nil {
		return nil, err
	}
	response, err := readRemoteHeader(c.conn)
	if err != nil {
		return nil, err
	}
	maxResponse := uint32(remoteExtentBytes)
	if kind == remoteMetadataPull {
		maxResponse = uint32(remoteMaxMetadataBytes)
	}
	if response.token != c.token || response.requestID != request.requestID || response.source != c.source || response.object != request.object || response.offset != offset || response.length > maxResponse {
		return nil, errors.New("remote filesystem response identity mismatch")
	}
	payload := make([]byte, response.length)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return nil, err
	}
	if response.type_ == remoteError {
		return nil, errors.New(string(payload))
	}
	if response.type_ != remoteData || (kind == remotePull && response.length != length) {
		return nil, errors.New("invalid remote filesystem data response")
	}
	return payload, nil
}
