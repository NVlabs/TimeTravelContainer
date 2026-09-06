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
	"sync"
)

const (
	remoteProtocolVersion = 1
	remoteHeaderSize      = 88
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
	object    [16]byte
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
	copy(dst[60:76], h.object[:])
	binary.BigEndian.PutUint64(dst[76:84], h.offset)
	binary.BigEndian.PutUint32(dst[84:88], h.length)
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
	copy(header.object[:], encoded[60:76])
	header.offset = binary.BigEndian.Uint64(encoded[76:84])
	header.length = binary.BigEndian.Uint32(encoded[84:88])
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
	Objects    map[string]int64
}

// ServeRemoteObjects serves range requests until conn closes. The token and
// object allowlist fence a client to one captured fork generation.
func ServeRemoteObjects(conn io.ReadWriteCloser, token RemoteToken, sources []RemoteObjectSource) error {
	defer conn.Close()
	byKey := make(map[RemoteSourceKey]RemoteObjectSource, len(sources))
	for _, source := range sources {
		if source.Repository == nil || len(source.Objects) == 0 {
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
		object := fmt.Sprintf("%x", request.object[:])
		size, allowed := source.Objects[object]
		if !ok || !allowed || request.offset >= uint64(size) || uint64(request.length) > uint64(size)-request.offset {
			return writeRemoteError(conn, request, "filesystem object or range is outside the captured generation")
		}
		payload := make([]byte, request.length)
		if err := source.Repository.ensureObjectRange(object, size, int64(request.offset), int64(request.length)); err != nil {
			return writeRemoteError(conn, request, err.Error())
		}
		file, err := source.Repository.openObject(object, os.O_RDONLY)
		if err != nil {
			return writeRemoteError(conn, request, err.Error())
		}
		n, readErr := file.ReadAt(payload, int64(request.offset))
		closeErr := file.Close()
		if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(payload)) {
			return writeRemoteError(conn, request, readErr.Error())
		}
		if closeErr != nil || n != len(payload) {
			return writeRemoteError(conn, request, fmt.Sprintf("short filesystem object read: %d/%d: %v", n, len(payload), closeErr))
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
	repository *Repository
}

// AttachRemoteObjectSource installs a donated, pre-connected range stream.
// The Sentry performs no socket creation or host path traversal.
func (r *Repository) AttachRemoteObjectSource(conn io.ReadWriteCloser, token RemoteToken, source RemoteSourceKey, branchID string) error {
	if r == nil || conn == nil {
		return errors.New("remote filesystem source requires repository and connection")
	}
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	if r.remote != nil {
		return errors.New("remote filesystem source is already attached")
	}
	branch, err := r.readBranch(branchID)
	if err != nil {
		return err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return err
	}
	state.mu.RLock()
	objects := make(map[string]int64)
	for _, inode := range state.inodes {
		if inode.Type == "regular" && inode.Object != "" {
			objects[inode.Object] = inode.Size
		}
	}
	state.mu.RUnlock()
	r.remote = &remoteObjectClient{conn: conn, token: token, source: source, next: 1, fetched: make(map[string]map[uint64]struct{}), objects: objects, repository: r}
	return nil
}

// MaterializeRemoteObjects fetches all still-remote objects for a clean
// source handoff or local durable publication.
func (r *Repository) MaterializeRemoteObjects() error {
	r.remoteMu.Lock()
	remote := r.remote
	r.remoteMu.Unlock()
	if remote == nil {
		return nil
	}
	for object, size := range remote.objects {
		if err := remote.ensure(object, size, 0, size); err != nil {
			return err
		}
	}
	// Serialize behind any reader that obtained remote before the final extent
	// completed. Once detached, all future reads are provably local and closing
	// the stream is the target's source-independence acknowledgement.
	remote.mu.Lock()
	r.remoteMu.Lock()
	if r.remote == remote {
		r.remote = nil
	}
	r.remoteMu.Unlock()
	err := remote.conn.Close()
	remote.mu.Unlock()
	return err
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
	remote := r.remote
	r.remoteMu.Unlock()
	if remote == nil || length == 0 {
		return nil
	}
	return remote.ensure(object, size, offset, length)
}

func (c *remoteObjectClient) ensure(object string, size, offset, length int64) error {
	if !validID(object) || size < 0 || offset < 0 || length < 0 || offset > size || length > size-offset {
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
	return nil
}

func (c *remoteObjectClient) pull(object string, offset uint64, length uint32) ([]byte, error) {
	decoded, err := decodeID(object)
	if err != nil {
		return nil, err
	}
	c.next++
	request := remoteHeader{type_: remotePull, token: c.token, requestID: c.next, source: c.source, offset: offset, length: length}
	copy(request.object[:], decoded)
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

func decodeID(id string) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid object ID %q", id)
	}
	decoded := make([]byte, 16)
	for i := 0; i < 16; i++ {
		var value byte
		for _, digit := range []byte{id[2*i], id[2*i+1]} {
			value <<= 4
			switch {
			case digit >= '0' && digit <= '9':
				value |= digit - '0'
			case digit >= 'a' && digit <= 'f':
				value |= digit - 'a' + 10
			default:
				return nil, fmt.Errorf("invalid object ID %q", id)
			}
		}
		decoded[i] = value
	}
	return decoded, nil
}
