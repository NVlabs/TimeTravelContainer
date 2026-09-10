// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcsnapshot

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testOriginLocator struct {
	node  string
	found bool
}

func (locator testOriginLocator) Provider(context.Context, SnapshotID) (string, bool, error) {
	return locator.node, locator.found, nil
}

type testObjectSource struct {
	data  []byte
	calls atomic.Uint64
	delay time.Duration
}

func (source *testObjectSource) Fetch(context.Context, ObjectID) (io.ReadCloser, uint64, error) {
	source.calls.Add(1)
	if source.delay != 0 {
		time.Sleep(source.delay)
	}
	return io.NopCloser(bytes.NewReader(source.data)), uint64(len(source.data)), nil
}

type testPeerSource struct {
	node   string
	source ObjectSource
}

func (peers testPeerSource) Peer(_ context.Context, node string) (ObjectSource, error) {
	if node != peers.node {
		panic("unexpected provider " + node)
	}
	return peers.source, nil
}

func TestResolverCoalescesOriginDirectedFetch(t *testing.T) {
	dir := t.TempDir()
	graph, err := Open(dir + "/graph")
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	cache, err := OpenLocalDataStore(dir + "/cache")
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	id, err := graph.ReserveObject(origin, ObjectKindFile)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("remote-object"), 8192)
	if err := graph.CommitReservedObject(id, "branch/root", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	peer := &testObjectSource{data: data, delay: 25 * time.Millisecond}
	shared := &testObjectSource{data: data}
	resolver, err := NewResolver(graph, cache, testOriginLocator{node: "node-a", found: true}, testPeerSource{node: "node-a", source: peer}, shared)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			file, err := resolver.Resolve(context.Background(), id)
			if err == nil {
				file.Close()
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := peer.calls.Load(); got != 1 {
		t.Fatalf("peer fetch count=%d, want 1", got)
	}
	if got := shared.calls.Load(); got != 0 {
		t.Fatalf("shared fetch count=%d, want 0", got)
	}
}

func TestResolverUsesSharedForPublishedOrigin(t *testing.T) {
	dir := t.TempDir()
	graph, err := Open(dir + "/graph")
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	cache, err := OpenLocalDataStore(dir + "/cache")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := graph.AllocateWorking()
	id, _ := graph.ReserveObject(origin, ObjectKindFile)
	data := []byte("shared")
	if err := graph.CommitReservedObject(id, "branch/root", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	peer := &testObjectSource{data: data}
	shared := &testObjectSource{data: data}
	resolver, err := NewResolver(graph, cache, testOriginLocator{}, testPeerSource{node: "node-a", source: peer}, shared)
	if err != nil {
		t.Fatal(err)
	}
	file, err := resolver.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resolved shared bytes=%q err=%v", got, err)
	}
	if shared.calls.Load() != 1 || peer.calls.Load() != 0 {
		t.Fatalf("fetch calls shared=%d peer=%d", shared.calls.Load(), peer.calls.Load())
	}
}

func TestResolverUsesReadOnlyCatalogMetadata(t *testing.T) {
	dir := t.TempDir()
	graph, err := Open(dir + "/graph")
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	cache, err := OpenLocalDataStore(dir + "/cache")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("catalog-only shared object")
	object := Object{
		ID:    ObjectID{Snapshot: 91, Kind: ObjectKindFile, Ordinal: 7},
		State: ObjectLive, Refs: 1, BulkLength: uint64(len(data)),
	}
	shared := &testObjectSource{data: data}
	resolver, err := NewResolver(graph, cache, testOriginLocator{}, testPeerSource{node: "unused", source: &testObjectSource{}}, shared)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Object(object.ID); ok {
		t.Fatal("catalog object unexpectedly exists in authoritative graph")
	}
	file, err := resolver.ResolveKnown(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resolved catalog bytes=%q err=%v", got, err)
	}
	if shared.calls.Load() != 1 {
		t.Fatalf("shared fetch count=%d, want 1", shared.calls.Load())
	}
	if _, ok := graph.Object(object.ID); ok {
		t.Fatal("cache resolution claimed reference authority")
	}
}
