// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"sync"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	remoteProtocolVersion = 2
	remoteHeaderSize      = 112
	remoteExtentBytes     = 1 << 20
	remoteMaxErrorBytes   = 4096
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
)

type remoteHeader struct {
	type_     remoteMessageType
	token     RemoteToken
	requestID uint32
	source    RemoteSourceKey
	object    [40]byte
	offset    uint64
	length    uint32
	checksum  uint32
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
	// Data checksums are encoded as a four-byte trailer after the payload. This
	// keeps the fixed request header naturally aligned and leaves future header
	// growth versioned rather than silently changing offsets.
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
	var trailer [4]byte
	binary.BigEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(payload))
	return writeAll(writer, trailer[:])
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
	Objects map[string]int64
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
		if source.Reader == nil || len(source.Objects) == 0 {
			continue
		}
		byKey[source.Key] = source
	}
	for {
		request, err := readRemoteHeader(conn)
		if err != nil {
			return err
		}
		if request.type_ != remotePull || request.token != token || request.length == 0 || request.length > remoteExtentBytes || request.offset%remoteExtentBytes != 0 {
			return writeRemoteError(conn, request, "invalid or unauthorized filesystem range request")
		}
		source, ok := byKey[request.source]
		object := strings.TrimRight(string(request.object[:]), "\x00")
		size, allowed := source.Objects[object]
		if !ok || !allowed || request.offset >= uint64(size) || uint64(request.length) > uint64(size)-request.offset {
			return writeRemoteError(conn, request, "filesystem object or range is outside the captured generation")
		}
		payload := make([]byte, request.length)
		if source.Repository != nil {
			if err := source.Repository.ensureObjectRange(object, size, int64(request.offset), int64(request.length)); err != nil {
				return writeRemoteError(conn, request, err.Error())
			}
		}
		n, readErr := source.Reader.ReadObjectRange(object, payload, int64(request.offset))
		if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(payload)) {
			return writeRemoteError(conn, request, readErr.Error())
		}
		if n != len(payload) {
			return writeRemoteError(conn, request, fmt.Sprintf("short filesystem object read: %d/%d", n, len(payload)))
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

type remoteObjectClient struct {
	mu         sync.Mutex
	conn       io.ReadWriteCloser
	token      RemoteToken
	source     RemoteSourceKey
	next       uint32
	fetched    map[string]map[uint64]struct{}
	objects    map[string]int64
	sources    map[string]string
	repository *Repository
}

// AttachRemoteObjectSource installs a donated, pre-connected range stream.
// The Sentry performs no socket creation or host path traversal.
func (r *Repository) AttachRemoteObjectSource(conn io.ReadWriteCloser, token RemoteToken, source RemoteSourceKey, branchID string) error {
	if r == nil || conn == nil {
		return errors.New("remote filesystem source requires repository and connection")
	}
	objects, err := r.BranchObjects(branchID)
	if err != nil {
		return err
	}
	remote := &remoteObjectClient{conn: conn, token: token, source: source, next: 1, fetched: make(map[string]map[uint64]struct{}), objects: make(map[string]int64), sources: make(map[string]string), repository: r}
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	if r.remotes == nil {
		r.remotes = make(map[*remoteObjectClient]struct{})
		r.remoteByObject = make(map[string]*remoteObjectClient)
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
		// Object IDs are content-addressed, so any authorized source for the
		// same object is equivalent. Prefer the newest live attachment over a
		// reader retained by an older branch.
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

// MaterializeRemoteObjects fetches all still-remote objects for a clean
// source handoff or local durable publication.
func (r *Repository) MaterializeRemoteObjects() error {
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
		// A newer branch may have replaced this client's ownership of a
		// content-addressed object. Do not contact a stale connection for it;
		// the current owner below will materialize the same immutable bytes.
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
		r.remoteMu.Unlock()
		err := remote.conn.Close()
		remote.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// EnsureDataRange fetches every 1 MiB extent intersecting an arbitrary guest
// read. Already resident extents are served entirely from the target-local
// object file.
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
	r.remoteMu.Lock()
	remote := r.remoteByObject[object]
	r.remoteMu.Unlock()
	if remote == nil || length == 0 {
		return nil
	}
	return remote.ensure(object, size, offset, length)
}

func (c *remoteObjectClient) ensure(object string, size, offset, length int64) error {
	if !validObjectID(object) || size < 0 || offset < 0 || length < 0 || offset > size || length > size-offset {
		return errors.New("invalid remote filesystem object range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	resident := c.fetched[object]
	if resident == nil {
		resident = make(map[uint64]struct{})
		c.fetched[object] = resident
	}
	first := uint64(offset) / remoteExtentBytes
	last := (uint64(offset+length) + remoteExtentBytes - 1) / remoteExtentBytes
	for extent := first; extent < last; extent++ {
		if _, ok := resident[extent]; ok {
			continue
		}
		start := extent * remoteExtentBytes
		want := uint64(remoteExtentBytes)
		if remaining := uint64(size) - start; want > remaining {
			want = remaining
		}
		data, err := c.pull(object, start, uint32(want))
		if err != nil {
			return err
		}
		file, err := c.repository.openObject(object, os.O_WRONLY)
		if err != nil {
			return err
		}
		n, writeErr := file.WriteAt(data, int64(start))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return errors.Join(writeErr, closeErr)
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		resident[extent] = struct{}{}
	}
	needed := (uint64(size) + remoteExtentBytes - 1) / remoteExtentBytes
	if uint64(len(resident)) == needed {
		if err := c.repository.clearRemoteObjectPending(object); err != nil {
			return err
		}
	}
	return nil
}

func (c *remoteObjectClient) pull(object string, offset uint64, length uint32) ([]byte, error) {
	remoteObject := c.sources[object]
	if !validObjectID(remoteObject) || len(remoteObject) > 40 {
		return nil, fmt.Errorf("invalid object ID %q", object)
	}
	c.next++
	request := remoteHeader{type_: remotePull, token: c.token, requestID: c.next, source: c.source, offset: offset, length: length}
	copy(request.object[:], remoteObject)
	var encoded [remoteHeaderSize]byte
	request.marshal(encoded[:])
	if err := writeAll(c.conn, encoded[:]); err != nil {
		return nil, err
	}
	response, err := readRemoteHeader(c.conn)
	if err != nil {
		return nil, err
	}
	if response.token != c.token || response.requestID != request.requestID || response.source != c.source || response.object != request.object || response.offset != offset || response.length > remoteExtentBytes {
		return nil, errors.New("remote filesystem response identity mismatch")
	}
	payload := make([]byte, response.length)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return nil, err
	}
	var trailer [4]byte
	if _, err := io.ReadFull(c.conn, trailer[:]); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(trailer[:]) {
		return nil, errors.New("remote filesystem response checksum mismatch")
	}
	if response.type_ == remoteError {
		return nil, errors.New(string(payload))
	}
	if response.type_ != remoteData || response.length != length {
		return nil, errors.New("invalid remote filesystem data response")
	}
	return payload, nil
}
