// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcvolume

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
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

// AttachImmutableMetadataSource reattaches the authoritative metadata catalog
// for a deferred branch without installing a bulk-data transport. Manager-side
// observations such as diff and resource accounting only traverse immutable
// inode and directory metadata; they must not materialize regular-file bytes or
// depend on the Sentry's process-local relay attachment.
//
// Resolver attachments are intentionally transient. The durable branch record
// retains both the catalog identity and its expansion origin, so every process
// that opens the repository can reconstruct this attachment independently.
func (r *Repository) AttachImmutableMetadataSource(branchID string, loader ttcsnapshot.ImmutableObjectLoader) error {
	if r == nil || loader == nil {
		return errors.New("immutable filesystem metadata source is unavailable")
	}
	branch, err := r.Branch(branchID)
	if err != nil {
		return err
	}
	if branch.RemoteCatalog == "" {
		return nil
	}
	if branch.RemoteCatalogSnapshot == 0 {
		return errors.New("remote filesystem branch has no catalog expansion origin")
	}
	return r.graph.AttachDeferredResolver(branch.RemoteCatalog, ttcsnapshot.SnapshotID(branch.RemoteCatalogSnapshot), loader)
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
	mu    sync.Mutex
	users sync.WaitGroup
	// conn is the stream to the source; local, instead, a source read in
	// this process.
	conn        io.ReadWriteCloser
	local       *RemoteObjectSource
	terminalErr error
	token       RemoteToken
	source      RemoteSourceKey
	next        uint32
	complete    map[string]bool
	objects     map[string]int64
	sources     map[string]string
	repository  *Repository
	branchID    string
	catalog     string
	// catalogOrigin is the snapshot that expands catalog.
	catalogOrigin ttcsnapshot.SnapshotID
	// transaction is whether the source serves a remote fork's import, its
	// catalog not an image of the shared store: its provider serves it only
	// while the fork's transaction lasts.
	transaction bool
}

// HoldsTransactionSources reports whether a source still attached to r
// serves a remote fork's import: until it is drained, r has files that no
// other source can provide once the fork's transaction ends.
func (r *Repository) HoldsTransactionSources() bool {
	if r == nil {
		return false
	}
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	for remote := range r.remotes {
		if remote.transaction {
			return true
		}
	}
	return false
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
	if size == 0 {
		// An empty file has no bytes to fetch: install it here, if it is not
		// here yet, rather than through its source, and retire any marker an
		// earlier look left. No file was bound to one absent, and one present
		// is complete: no runtime state needs invalidating.
		if _, err := r.data.InstallEmptyDeferred(id); err != nil {
			return err
		}
		return r.retireRemoteObjectPending(id.String())
	}
	if local, err := r.deferredObjectLocal(id, size); err != nil || local {
		return err
	}
	return r.registerDeferredFileObject(id, size, catalog, sourceID)
}

// deferredObjectLocal reports whether a deferred file object's bytes are all
// installed: its file exists, and no pending marker says that what it lacks
// is still to come from its source.
func (r *Repository) deferredObjectLocal(id ttcsnapshot.ObjectID, size int64) (bool, error) {
	pending, err := r.remoteObjectPending(id.String())
	if err != nil || pending {
		return false, err
	}
	file, err := r.data.Open(id, size)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

// registerDeferredFileObject marks a deferred file object pending and
// resolves it through the source attached for its catalog, unless its bytes
// have become local since the caller looked.
func (r *Repository) registerDeferredFileObject(id ttcsnapshot.ObjectID, size int64, catalog string, sourceID ttcsnapshot.ObjectID) error {
	// Publish the durable pending marker and its live resolver atomically with
	// respect to repository-wide source detachment. In the old order, an eager
	// independence drain could observe the new object in remote.objects, fetch
	// and detach the source, and only then race with markRemoteObjectPending.
	// That left a fresh pending marker whose sole authoritative connection had
	// already been closed. Holding remoteMu makes the marker+resolver pair appear
	// before the materializer's final scan, or makes this operation observe that
	// the provider has already detached.
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	// A fetch of the object, a reader's or a drain's, may have ended since the
	// caller looked; a drain then detaches the source. A fetch installs the
	// bytes and then retires the marker under remoteMarkerMu: looking again
	// under it, this neither fails for a file whose bytes are here nor marks
	// one pending again, a marker no source would retire, on which every later
	// read failed once the source detached.
	r.remoteMarkerMu.Lock()
	defer r.remoteMarkerMu.Unlock()
	if local, err := r.deferredObjectLocal(id, size); err != nil || local {
		return err
	}
	remote := r.remoteByCatalog[catalog]
	if remote == nil {
		return fmt.Errorf("deferred filesystem catalog %q has no attached authoritative source", catalog)
	}
	if err := r.markRemoteObjectPending(id.String(), sourceID.String()); err != nil {
		return err
	}
	remote.objects[id.String()] = size
	remote.sources[id.String()] = sourceID.String()
	r.remoteByObject[id.String()] = remote
	return nil
}

// AttachRemoteObjectSource installs a donated, pre-connected range stream.
// The Sentry performs no socket creation or host path traversal.
func (r *Repository) AttachRemoteObjectSource(conn io.ReadWriteCloser, token RemoteToken, source RemoteSourceKey, branchID string) error {
	if r == nil || conn == nil {
		return errors.New("remote filesystem source requires repository and connection")
	}
	_, err := r.attachObjectSource(&remoteObjectClient{conn: conn, token: token, source: source}, branchID)
	return err
}

// AttachRemoteObjectSourceUndoable is AttachRemoteObjectSource, and returns
// what undoes the attachment, for a restore that fails after it: the source
// takes over the files and the catalog that earlier attachments of the same
// source serve, the newest attachment preferred, and one left attached once
// its restore failed, its connection closed, failed them all. A remote
// fork's import whose peer exited as it restored left the branch imported
// before it unable to be drained, "deferred filesystem catalog ... has no
// attached authoritative source". undo detaches the source and gives what it
// took over back to the sources it took it from, if they are still attached.
func (r *Repository) AttachRemoteObjectSourceUndoable(conn io.ReadWriteCloser, token RemoteToken, source RemoteSourceKey, branchID string) (func(), error) {
	if r == nil || conn == nil {
		return func() {}, errors.New("remote filesystem source requires repository and connection")
	}
	attachment, err := r.attachObjectSource(&remoteObjectClient{conn: conn, token: token, source: source}, branchID)
	if err != nil || attachment == nil {
		return func() {}, err
	}
	var once sync.Once
	return func() { once.Do(attachment.undo) }, nil
}

// remoteAttachment is what attaching remote displaced: the earlier owners of
// the objects it took over, and of its catalog.
type remoteAttachment struct {
	repository *Repository
	remote     *remoteObjectClient
	// catalog is remote's catalog, and previousCatalog its owner before.
	catalog         string
	previousCatalog *remoteObjectClient
	displaced       map[string]*remoteObjectClient
}

// undo detaches the attachment's source, as if it had never been attached.
func (a *remoteAttachment) undo() {
	r, remote := a.repository, a.remote
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	// Serialize behind a reader using the source, as a drain's detach does.
	remote.mu.Lock()
	r.remoteMu.Lock()
	attached := func(client *remoteObjectClient) bool {
		_, ok := r.remotes[client]
		return client != nil && ok
	}
	delete(r.remotes, remote)
	for object, owner := range r.remoteByObject {
		if owner != remote {
			continue
		}
		// An object found through the catalog since is the catalog's.
		heir := a.displaced[object]
		if heir == nil {
			heir = a.previousCatalog
		}
		if !attached(heir) {
			delete(r.remoteByObject, object)
			continue
		}
		if _, ok := heir.objects[object]; !ok {
			heir.objects[object] = remote.objects[object]
			heir.sources[object] = remote.sources[object]
		}
		r.remoteByObject[object] = heir
	}
	if a.catalog != "" && r.remoteByCatalog[a.catalog] == remote {
		if attached(a.previousCatalog) {
			r.remoteByCatalog[a.catalog] = a.previousCatalog
			_ = r.graph.AttachDeferredResolver(a.catalog, a.previousCatalog.catalogOrigin, a.previousCatalog.pullMetadata)
		} else {
			delete(r.remoteByCatalog, a.catalog)
			r.graph.DetachDeferredResolver(a.catalog)
		}
	}
	r.remoteMu.Unlock()
	remote.mu.Unlock()
	_ = remote.close()
}

// AttachLocalObjectSource attaches source, which this process reads itself,
// as AttachRemoteObjectSource attaches one served over a stream: a manager
// reading shared packs needs no relay process, protocol and encoding
// between, which took a quarter of the time of making a lazily imported
// tree of 100,000 files independent. source serves by Reader and Catalog.
func (r *Repository) AttachLocalObjectSource(source RemoteObjectSource, branchID string) error {
	if r == nil || source.Reader == nil || source.Catalog == nil {
		return errors.New("local filesystem source requires repository, reader and catalog")
	}
	_, err := r.attachObjectSource(&remoteObjectClient{local: &source, source: source.Key}, branchID)
	return err
}

// attachObjectSource attaches remote as the source of branchID's remote
// files, and returns what it displaced, nil if it attached nothing.
func (r *Repository) attachObjectSource(remote *remoteObjectClient, branchID string) (*remoteAttachment, error) {
	// A source attachment changes the resolver set consumed by the
	// repository-wide independence barrier. Serialize the two operations so a
	// concurrent sibling migration cannot have its newly attached catalog
	// detached by an older branch's drain.
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	branch, err := r.Branch(branchID)
	if err != nil {
		return nil, err
	}
	remote.next, remote.complete, remote.objects, remote.sources = 1, make(map[string]bool), make(map[string]int64), make(map[string]string)
	remote.repository, remote.branchID, remote.catalog = r, branchID, branch.RemoteCatalog
	remote.transaction = branch.RemoteCatalog != "" && !branch.RemoteShared
	attachment := &remoteAttachment{repository: r, remote: remote, catalog: branch.RemoteCatalog, displaced: make(map[string]*remoteObjectClient)}
	if branch.RemoteCatalog != "" {
		if branch.RemoteCatalogSnapshot == 0 {
			return nil, errors.New("remote filesystem branch has no catalog expansion origin")
		}
		remote.catalogOrigin = ttcsnapshot.SnapshotID(branch.RemoteCatalogSnapshot)
		if err := r.graph.AttachDeferredResolver(branch.RemoteCatalog, remote.catalogOrigin, remote.pullMetadata); err != nil {
			return nil, err
		}
		r.remoteMu.Lock()
		if r.remotes == nil {
			r.remotes = make(map[*remoteObjectClient]struct{})
			r.remoteByObject = make(map[string]*remoteObjectClient)
			r.remoteByCatalog = make(map[string]*remoteObjectClient)
		}
		r.remotes[remote] = struct{}{}
		attachment.previousCatalog = r.remoteByCatalog[branch.RemoteCatalog]
		r.remoteByCatalog[branch.RemoteCatalog] = remote
		r.remoteMu.Unlock()
		return attachment, nil
	}
	objects, err := r.BranchObjects(branchID)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		if !pending {
			continue
		}
		remote.objects[object] = size
		remote.sources[object] = sourceObject
		// The immutable graph object identifies the authorized source bytes.
		// Prefer the newest live attachment over a reader retained by an older
		// branch.
		if previous := r.remoteByObject[object]; previous != nil {
			attachment.displaced[object] = previous
		}
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
			return nil, err
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
			return nil, err
		}
		object, ok := r.graph.Object(id)
		if !ok {
			return nil, fmt.Errorf("remote target object %s is absent from graph", localObject)
		}
		remote.objects[localObject] = int64(object.BulkLength)
		remote.sources[localObject] = sourceObject
		attachment.displaced[localObject] = owner
		r.remoteByObject[localObject] = remote
	}
	if len(remote.objects) == 0 {
		return nil, remote.close()
	}
	r.remotes[remote] = struct{}{}
	return attachment, nil
}

// PinRemoteRestore prevents repository-wide remote materialization from
// detaching a source while a peer restore may still expand its lazy metadata
// catalog. Source attachment remains available while pinned. The returned
// release function is idempotent.
func (r *Repository) PinRemoteRestore() func() {
	if r == nil {
		return func() {}
	}
	r.remoteLifecycleMu.Lock()
	if r.remoteLifecycleCV == nil {
		r.remoteLifecycleCV = sync.NewCond(&r.remoteLifecycleMu)
	}
	r.remoteRestorePins++
	r.remoteLifecycleMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.remoteLifecycleMu.Lock()
			if r.remoteRestorePins == 0 {
				panic("ttcvolume: unbalanced remote restore pin")
			}
			r.remoteRestorePins--
			if r.remoteRestorePins == 0 {
				r.remoteLifecycleCV.Broadcast()
			}
			r.remoteLifecycleMu.Unlock()
		})
	}
}

// lockRemoteLifecycleForMaterialization acquires remoteLifecycleMu after all
// peer restores that can discover new lazy objects have completed.
//
// +checklocksacquire:r.remoteLifecycleMu
func (r *Repository) lockRemoteLifecycleForMaterialization() {
	r.remoteLifecycleMu.Lock()
	if r.remoteLifecycleCV == nil {
		r.remoteLifecycleCV = sync.NewCond(&r.remoteLifecycleMu)
	}
	for r.remoteRestorePins != 0 {
		r.remoteLifecycleCV.Wait()
	}
}

// BranchObjects returns the immutable object allowlist referenced by branch.
func (r *Repository) BranchObjects(branchID string) (map[string]int64, error) {
	branch, err := r.Branch(branchID)
	if err != nil {
		return nil, err
	}
	return r.branchObjects(branch)
}

// ResolveBranchMetadata expands the immutable namespace roots needed to
// observe branch, including its diff base, while a live remote catalog is
// attached. It deliberately leaves regular-file contents remote: callers such
// as ttc diff need complete inode and directory metadata, not file bytes.
//
// Resolved objects become ordinary persistent graph objects, so a manager
// process can subsequently reopen the repository and perform the observation
// without owning the Sentry's process-local peer connection.
func (r *Repository) ResolveBranchMetadata(branchID string) (resultErr error) {
	branch, err := r.Branch(branchID)
	if err != nil {
		return err
	}
	names := []string{graphRootName(branchID)}
	if branch.BaseRoot != "" {
		names = append(names, graphBaseRootName(branchID))
	}
	roots, pins, err := r.pinGraphRoots(names)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, r.releaseGraphObservation(pins)) }()
	for _, name := range names {
		if _, _, err := r.graph.FilesystemRecords(roots[name]); err != nil {
			return fmt.Errorf("resolving branch %q filesystem metadata root %q: %w", branchID, name, err)
		}
	}
	return nil
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
// source handoff, or a repository that must stand alone.
func (r *Repository) MaterializeRemoteObjects() error {
	// Multiple distributed transactions in one physical Sentry share this
	// repository. Their eager drains may arrive concurrently, but detaching
	// remote catalogs is a repository-wide barrier and must execute exactly one
	// at a time.
	r.lockRemoteLifecycleForMaterialization()
	defer r.remoteLifecycleMu.Unlock()
	return r.materializeRemoteObjects(false)
}

// materializeRemoteObjects implements MaterializeRemoteObjects while the
// caller retains remoteLifecycleMu across a larger lifecycle transaction.
// With keepShared, it leaves the catalogs that expand images of the shared
// store as they are, lazy: a publication refers to their sources.
//
// Preconditions: remoteLifecycleMu is held.
//
// +checklocks:r.remoteLifecycleMu
func (r *Repository) materializeRemoteObjects(keepShared bool) error {
	for {
		var work []remoteMaterialization
		var detached []*remoteObjectClient
		if err := r.withLock(func() error {
			var err error
			work, detached, err = r.materializeRemoteObjectsPassLocked(keepShared)
			return err
		}); err != nil {
			return err
		}
		if len(work) != 0 {
			// Runtime materialization hooks acquire file and mapping locks. Guest
			// writes acquire those locks before entering repository mutation, so
			// invoking a hook while withLock is held would invert the order and
			// deadlock. Immutable bytes may be fetched and atomically installed
			// outside the mutation fence; the next pass revalidates the graph.
			for _, item := range work {
				if err := item.remote.ensure(item.object, item.size, 0, item.size); err != nil {
					return err
				}
			}
			continue
		}
		var closeErr error
		for _, remote := range detached {
			// A reader may have selected this client immediately before it was
			// removed from remoteByObject. Wait for every such pinned selection
			// before closing the stream; remoteMu prevents a new user from being
			// added after the detach pass.
			remote.users.Wait()
			remote.mu.Lock()
			closeErr = errors.Join(closeErr, remote.close())
			remote.mu.Unlock()
		}
		return closeErr
	}
}

type remoteMaterialization struct {
	remote *remoteObjectClient
	object string
	size   int64
}

// materializeRemoteObjectsPassLocked expands every deferred namespace and
// returns unresolved bulk objects. If none remain, it detaches every provider
// while the repository mutation fence is still held. With keepShared, it
// passes over the catalogs that expand images of the shared store.
//
// Preconditions: Repository.withLock and remoteLifecycleMu are held.
func (r *Repository) materializeRemoteObjectsPassLocked(keepShared bool) ([]remoteMaterialization, []*remoteObjectClient, error) {
	// Resolve every still-foreign namespace before detaching its provider.
	// This is an explicit independence barrier and is allowed to be O(total
	// metadata); ordinary remote start and pathname access remain lazy.
	r.remoteMu.Lock()
	deferredCatalogs := make(map[string]struct{})
	for remote := range r.remotes {
		if remote.catalog != "" {
			deferredCatalogs[remote.catalog] = struct{}{}
		}
	}
	r.remoteMu.Unlock()
	var deferredBranches []string
	kept := make(map[string]bool)
	if len(deferredCatalogs) != 0 {
		directory, err := r.openFile("branches", os.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, nil, err
		}
		entries, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if readErr != nil || closeErr != nil {
			return nil, nil, errors.Join(readErr, closeErr)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if validateName(id, "branch") != nil {
				continue
			}
			branch, err := r.readBranch(id)
			if err != nil {
				return nil, nil, err
			}
			if _, ok := deferredCatalogs[branch.RemoteCatalog]; ok {
				if keepShared && branch.RemoteShared {
					kept[branch.RemoteCatalog] = true
					continue
				}
				deferredBranches = append(deferredBranches, id)
			}
		}
		sort.Strings(deferredBranches)
	}
	for _, branch := range deferredBranches {
		// MaterializeRemoteObjects holds the repository operation lock, so using
		// the public Branch helper here would recursively acquire it. Flush the
		// bounded metadata tail directly and construct the same authoritative
		// graph-root view while the mutation fence remains held.
		if err := r.flushMetadataBatchLocked(branch); err != nil {
			return nil, nil, err
		}
		branchRecord, err := r.readBranch(branch)
		if err != nil {
			return nil, nil, err
		}
		root, ok := r.graph.Root(graphRootName(branch))
		if !ok {
			return nil, nil, fmt.Errorf("branch %q has no snapshot graph root", branch)
		}
		branchRecord.Root = root.String()
		objects, err := r.branchObjects(branchRecord)
		if err != nil {
			return nil, nil, err
		}
		// The current namespace does not necessarily retain metadata that exists
		// only in the branch's diff base (most notably a deleted file). Resolve
		// that immutable root before the repository-wide provider detach too.
		// FilesystemRecords expands metadata only; regular-file objects are
		// registered below so the ordinary bulk materialization pass also makes
		// the complete graph independent.
		if branchRecord.BaseRoot != "" {
			baseRoot, ok := r.graph.Root(graphBaseRootName(branch))
			if !ok {
				return nil, nil, fmt.Errorf("branch %q has no filesystem diff-base root", branch)
			}
			// A base that is the tree itself, as a branch rehydrated and made
			// durable again unchanged has, has no file the walk of the tree
			// did not prepare: walking it again took 1 s for 100,000 files.
			if baseRoot == root {
				continue
			}
			inodes, _, err := r.graph.FilesystemRecords(baseRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving branch %q filesystem diff base: %w", branch, err)
			}
			for _, inode := range inodes {
				if inode.Type != "regular" || inode.File.IsZero() {
					continue
				}
				// The walk of the current namespace prepared the files the base
				// shares with it: every one, in a tree of unchanged files.
				if _, prepared := objects[inode.File.String()]; prepared {
					continue
				}
				if err := r.prepareDeferredFileObject(inode.File, int64(inode.Size)); err != nil {
					return nil, nil, fmt.Errorf("registering branch %q diff-base file %s: %w", branch, inode.File, err)
				}
			}
		}
	}
	r.remoteMu.Lock()
	remotes := make([]*remoteObjectClient, 0, len(r.remotes))
	for remote := range r.remotes {
		// A shared image's source stays, for the reads still to come.
		if !kept[remote.catalog] {
			remotes = append(remotes, remote)
		}
	}
	r.remoteMu.Unlock()
	if len(remotes) == 0 {
		return nil, nil, nil
	}
	var work []remoteMaterialization
	for _, remote := range remotes {
		// A newer branch may have replaced this client's source attachment. Do
		// not contact a stale connection for it; the current owner below will
		// materialize the same immutable bytes.
		remote.mu.Lock()
		r.remoteMu.Lock()
		for object, size := range remote.objects {
			if r.remoteByObject[object] == remote && !remote.complete[object] {
				work = append(work, remoteMaterialization{remote: remote, object: object, size: size})
			}
		}
		r.remoteMu.Unlock()
		remote.mu.Unlock()
	}
	if len(work) != 0 {
		sort.Slice(work, func(i, j int) bool {
			if work[i].remote.branchID != work[j].remote.branchID {
				return work[i].remote.branchID < work[j].remote.branchID
			}
			return work[i].object < work[j].object
		})
		return work, nil, nil
	}
	for _, remote := range remotes {
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
		remote.mu.Unlock()
	}
	return nil, remotes, nil
}

// close closes the client's stream, if it has one. Once others may use the
// client, c.mu must be held.
func (c *remoteObjectClient) close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *remoteObjectClient) pullMetadata(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	if c.local != nil {
		object, err := c.local.Catalog.RemoteObject(id)
		if err != nil {
			return ttcsnapshot.Object{}, err
		}
		if object.ID != id {
			return ttcsnapshot.Object{}, fmt.Errorf("local filesystem metadata identifies %s, want %s", object.ID, id)
		}
		return object, nil
	}
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
	// Resolve the immutable object while the branch view is stable, but never
	// retain viewMu while fetching it. A fetch serializes on the remote client
	// and invokes runtime cache-invalidation hooks. Guest writes take those
	// runtime locks before entering a repository mutation (and therefore before
	// taking viewMu for writing), so blocking on the remote client under the
	// read lock creates a three-way inversion with an eager materializer:
	//
	//   guest read:       viewMu.RLock -> remote client
	//   materializer:     remote client -> runtime file lock
	//   guest write:      runtime file lock -> viewMu.Lock
	//
	// Object IDs and their lengths are immutable. remote.users, acquired by
	// ensureObjectRange, pins the selected provider after this metadata lookup,
	// so the bulk transfer needs no branch-view lock.
	var inode Inode
	err := func() error {
		r.viewMu.RLock()
		defer r.viewMu.RUnlock()
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err = r.metadataInode(state, ino)
		return err
	}()
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
	// The pending marker participates in the repository durability set. Keep
	// its unlink in the same operation domain as SyncMetadata: otherwise a
	// concurrent fsync can snapshot the marker from dirtyFiles, materialization
	// can unlink it, and fsync then reports a spurious ENOENT to the guest. This
	// is particularly visible as SQLITE_IOERR_FSYNC during live migration.
	//
	// Do not take the full repository mutation fence here. ReadAt may retain
	// viewMu for reading across this fetch, while withLock needs it for writing.
	// operationMu is the narrower common lock with SyncMetadata and preserves
	// the repository's viewMu -> operationMu order.
	if err := c.repository.retireRemoteObjectPending(object); err != nil {
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
	if c.local != nil {
		payload := make([]byte, length)
		n, err := c.local.Reader.ReadObjectRange(remoteObject, payload, int64(offset))
		if err != nil && !(errors.Is(err, io.EOF) && n == len(payload)) {
			return nil, err
		}
		if n != len(payload) {
			return nil, fmt.Errorf("short filesystem object read: %d/%d", n, len(payload))
		}
		return payload, nil
	}
	return c.request(remotePull, remoteObject, offset, length)
}

func (c *remoteObjectClient) request(kind remoteMessageType, object string, offset uint64, length uint32) ([]byte, error) {
	if c.terminalErr != nil {
		return nil, c.terminalErr
	}
	fail := func(err error) ([]byte, error) {
		if err == nil {
			err = errors.New("remote filesystem protocol failed")
		}
		c.terminalErr = fmt.Errorf("remote filesystem source became unavailable: %w", err)
		return nil, c.terminalErr
	}
	if len(object) > len(remoteHeader{}.object) {
		return nil, errors.New("remote filesystem object identity is too long")
	}
	c.next++
	request := remoteHeader{type_: kind, token: c.token, requestID: c.next, source: c.source, offset: offset, length: length}
	copy(request.object[:], object)
	var encoded [remoteHeaderSize]byte
	request.marshal(encoded[:])
	if err := writeAll(c.conn, encoded[:]); err != nil {
		return fail(err)
	}
	response, err := readRemoteHeader(c.conn)
	if err != nil {
		return fail(err)
	}
	maxResponse := uint32(remoteExtentBytes)
	if kind == remoteMetadataPull {
		maxResponse = uint32(remoteMaxMetadataBytes)
	}
	if response.token != c.token || response.requestID != request.requestID || response.source != c.source || response.object != request.object || response.offset != offset || response.length > maxResponse {
		return fail(errors.New("remote filesystem response identity mismatch"))
	}
	payload := make([]byte, response.length)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return fail(err)
	}
	if response.type_ == remoteError {
		return fail(errors.New(string(payload)))
	}
	if response.type_ != remoteData || (kind == remotePull && response.length != length) {
		return fail(errors.New("invalid remote filesystem data response"))
	}
	return payload, nil
}
