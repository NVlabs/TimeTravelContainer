// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcchunk

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
)

const (
	chunkRemoteVersion = 1
	chunkRemoteHeader  = 64
)

var chunkRemoteMagic = [8]byte{'T', 'T', 'C', 'M', 'R', 'E', 'M', '1'}

// RemoteToken fences cold-memory reads to one manifest generation.
type RemoteToken [16]byte

// RemoteReader returns one encoded immutable memory chunk.
type RemoteReader interface {
	ReadMemoryChunk(id ID) ([]byte, error)
}

// ServeRemoteChunks serves content-addressed chunks in whole 1 MiB logical
// units. allowed is the exact set referenced by the cold manifest.
func ServeRemoteChunks(conn io.ReadWriteCloser, token RemoteToken, reader RemoteReader, allowed map[ID]struct{}) error {
	defer conn.Close()
	for {
		var request [chunkRemoteHeader]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		if string(request[:8]) != string(chunkRemoteMagic[:]) || request[8] != chunkRemoteVersion || request[9] != 1 {
			return errors.New("invalid TTC cold-memory request")
		}
		var requestToken RemoteToken
		copy(requestToken[:], request[16:32])
		var id ID
		copy(id[:], request[32:64])
		if requestToken != token {
			return errors.New("unauthorized TTC cold-memory request")
		}
		if _, ok := allowed[id]; !ok {
			return errors.New("TTC cold-memory object is outside the captured manifest")
		}
		data, err := reader.ReadMemoryChunk(id)
		if err != nil {
			return err
		}
		if len(data) > maxEncodedChunkBytes {
			return errors.New("TTC cold-memory object is too large")
		}
		var response [chunkRemoteHeader]byte
		copy(response[:8], chunkRemoteMagic[:])
		response[8], response[9] = chunkRemoteVersion, 2
		copy(response[16:32], token[:])
		copy(response[32:64], id[:])
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(data)))
		if err := writeRemoteChunkAll(conn, response[:]); err != nil {
			return err
		}
		if err := writeRemoteChunkAll(conn, length[:]); err != nil {
			return err
		}
		if err := writeRemoteChunkAll(conn, data); err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if err := writeRemoteChunkAll(conn, digest[:]); err != nil {
			return err
		}
	}
}

func writeRemoteChunkAll(writer io.Writer, data []byte) error {
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

type remoteChunkClient struct {
	mu    sync.Mutex
	conn  io.ReadWriteCloser
	token RemoteToken
}

// AttachRemoteChunkSource installs an inherited cold-pack stream for the
// objects referenced by manifest. A family repository may host branches from
// several cold generations, so routing is per immutable object rather than a
// single process-global source.
func (r *MemoryRepository) AttachRemoteChunkSource(conn io.ReadWriteCloser, token RemoteToken, manifestID ManifestID) error {
	if r == nil || conn == nil {
		return os.ErrInvalid
	}
	manifest, err := r.LoadManifest(manifestID)
	if err != nil {
		return err
	}
	objects := make(map[ID]struct{})
	for _, run := range manifest.Runs {
		objects[run.Object] = struct{}{}
	}
	client := &remoteChunkClient{conn: conn, token: token}
	r.remoteMu.Lock()
	if r.remotes == nil {
		r.remotes = make(map[*remoteChunkClient]struct{})
		r.remoteByObject = make(map[ID]*remoteChunkClient)
	}
	assigned := 0
	for object := range objects {
		if _, exists := r.remoteByObject[object]; !exists {
			r.remoteByObject[object] = client
			assigned++
		}
	}
	if assigned != 0 {
		r.remotes[client] = struct{}{}
	}
	r.remoteMu.Unlock()
	if assigned == 0 {
		return conn.Close()
	}
	return nil
}

func (r *MemoryRepository) remoteForObject(id ID) *remoteChunkClient {
	if r == nil {
		return nil
	}
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	return r.remoteByObject[id]
}

func (c *remoteChunkClient) get(id ID) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var request [chunkRemoteHeader]byte
	copy(request[:8], chunkRemoteMagic[:])
	request[8], request[9] = chunkRemoteVersion, 1
	copy(request[16:32], c.token[:])
	copy(request[32:64], id[:])
	if err := writeRemoteChunkAll(c.conn, request[:]); err != nil {
		return nil, err
	}
	var response [chunkRemoteHeader]byte
	if _, err := io.ReadFull(c.conn, response[:]); err != nil {
		return nil, err
	}
	if string(response[:8]) != string(chunkRemoteMagic[:]) || response[8] != chunkRemoteVersion || response[9] != 2 || string(response[16:32]) != string(c.token[:]) || string(response[32:64]) != string(id[:]) {
		return nil, errors.New("invalid TTC cold-memory response")
	}
	var encodedLength [4]byte
	if _, err := io.ReadFull(c.conn, encodedLength[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(encodedLength[:])
	if length == 0 || length > maxEncodedChunkBytes {
		return nil, errors.New("invalid TTC cold-memory response length")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		return nil, err
	}
	var digest [sha256.Size]byte
	if _, err := io.ReadFull(c.conn, digest[:]); err != nil {
		return nil, err
	}
	actual := sha256.Sum256(data)
	if actual != digest || ID(actual) != id {
		return nil, errors.New("TTC cold-memory response checksum mismatch")
	}
	return data, nil
}
