// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcsnapshot

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
)

type relayObjectSource map[ObjectID][]byte

func (source relayObjectSource) Fetch(_ context.Context, id ObjectID) (io.ReadCloser, uint64, error) {
	data, ok := source[id]
	if !ok {
		return nil, 0, io.EOF
	}
	return io.NopCloser(bytes.NewReader(data)), uint64(len(data)), nil
}

func TestObjectRelayTransfersAllowedObject(t *testing.T) {
	server, client := net.Pipe()
	var token ObjectRelayToken
	token[0] = 42
	id := ObjectID{Snapshot: 7, Kind: ObjectKindMemoryChunk, Ordinal: 9}
	want := bytes.Repeat([]byte{0x5a}, 8192)
	done := make(chan error, 1)
	go func() {
		done <- ServeObjectRelay(server, token, relayObjectSource{id: want}, map[ObjectID]uint64{id: uint64(len(want))})
	}()
	remote, err := NewObjectRelayClient(client, token)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Fetch(id, uint64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("relayed object bytes differ")
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && err != io.EOF {
		t.Fatal(err)
	}
}
