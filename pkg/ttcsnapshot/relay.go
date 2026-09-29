// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	objectRelayVersion = 1
	objectRelayHeader  = 128
	objectRelayMaxID   = 80
)

var objectRelayMagic = [8]byte{'T', 'T', 'C', 'G', 'O', 'B', 'J', '1'}

// ObjectRelayToken fences one donated resolver stream to its restore request.
type ObjectRelayToken [16]byte

// ServeObjectRelay exposes an exact allowlist of immutable objects. It sends
// raw object bytes and declared lengths: object identity and generation
// fencing provide protocol correctness, not a runtime content checksum.
func ServeObjectRelay(conn io.ReadWriteCloser, token ObjectRelayToken, source ObjectSource, allowed map[ObjectID]uint64) error {
	if conn == nil || source == nil || len(allowed) == 0 {
		return errors.New("invalid TTC graph object relay")
	}
	defer conn.Close()
	for {
		var request [objectRelayHeader]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		if string(request[:8]) != string(objectRelayMagic[:]) || request[8] != objectRelayVersion || request[9] != 1 ||
			string(request[16:32]) != string(token[:]) {
			return errors.New("invalid TTC graph object request")
		}
		idLength := int(request[32])
		if idLength == 0 || idLength > objectRelayMaxID {
			return errors.New("invalid TTC graph object identity length")
		}
		id, err := ParseObjectID(string(request[40 : 40+idLength]))
		if err != nil {
			return err
		}
		wanted, ok := allowed[id]
		if !ok {
			return fmt.Errorf("graph object %s is outside the restore catalog", id)
		}
		reader, length, err := source.Fetch(context.Background(), id)
		if err != nil {
			return err
		}
		if length != wanted {
			reader.Close()
			return fmt.Errorf("graph object %s has length %d, want %d", id, length, wanted)
		}
		var response [objectRelayHeader]byte
		copy(response[:8], objectRelayMagic[:])
		response[8], response[9] = objectRelayVersion, 2
		copy(response[16:32], token[:])
		binary.BigEndian.PutUint64(response[32:40], length)
		if err := writeRelayAll(conn, response[:]); err != nil {
			reader.Close()
			return err
		}
		_, copyErr := io.CopyN(conn, reader, int64(length))
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	}
}

func writeRelayAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// ObjectRelayClient reads whole immutable objects over a donated stream.
// Calls are serialized because the compact protocol deliberately has no
// request IDs; callers can donate a pool of streams when parallelism matters.
type ObjectRelayClient struct {
	mu     sync.Mutex
	conn   io.ReadWriteCloser
	token  ObjectRelayToken
	closed bool
}

// NewObjectRelayClient adopts conn.
func NewObjectRelayClient(conn io.ReadWriteCloser, token ObjectRelayToken) (*ObjectRelayClient, error) {
	if conn == nil {
		return nil, errors.New("nil TTC graph object relay")
	}
	return &ObjectRelayClient{conn: conn, token: token}, nil
}

// Fetch returns one exact raw object.
func (client *ObjectRelayClient) Fetch(id ObjectID, length uint64) ([]byte, error) {
	if err := id.validate(); err != nil || length == 0 || length > MemoryChunkBytes {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("invalid TTC graph object relay length")
	}
	identity := id.String()
	if len(identity) > objectRelayMaxID {
		return nil, errors.New("TTC graph object identity exceeds relay limit")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil, errors.New("TTC graph object relay is closed")
	}
	var request [objectRelayHeader]byte
	copy(request[:8], objectRelayMagic[:])
	request[8], request[9] = objectRelayVersion, 1
	copy(request[16:32], client.token[:])
	request[32] = byte(len(identity))
	copy(request[40:], identity)
	if err := writeRelayAll(client.conn, request[:]); err != nil {
		return nil, err
	}
	var response [objectRelayHeader]byte
	if _, err := io.ReadFull(client.conn, response[:]); err != nil {
		return nil, err
	}
	if string(response[:8]) != string(objectRelayMagic[:]) || response[8] != objectRelayVersion || response[9] != 2 ||
		string(response[16:32]) != string(client.token[:]) {
		return nil, errors.New("invalid TTC graph object response")
	}
	got := binary.BigEndian.Uint64(response[32:40])
	if got != length {
		return nil, fmt.Errorf("graph object %s response length is %d, want %d", id, got, length)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(client.conn, data); err != nil {
		return nil, err
	}
	return data, nil
}

// Close releases the donated stream.
func (client *ObjectRelayClient) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil
	}
	client.closed = true
	return client.conn.Close()
}
