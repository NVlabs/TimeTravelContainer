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
	"os"
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

// pausingObjectSource serves an object whose reader stops halfway until
// released.
type pausingObjectSource struct {
	data    []byte
	halfway chan struct{}
	once    sync.Once
	release chan struct{}
}

func (source *pausingObjectSource) Fetch(context.Context, ObjectID) (io.ReadCloser, uint64, error) {
	return io.NopCloser(&pausingReader{source: source}), uint64(len(source.data)), nil
}

type pausingReader struct {
	source *pausingObjectSource
	offset int
	paused bool
}

func (reader *pausingReader) Read(p []byte) (int, error) {
	data := reader.source.data
	if reader.offset == len(data) {
		return 0, io.EOF
	}
	if reader.offset != 0 && !reader.paused {
		reader.paused = true
		reader.source.once.Do(func() { close(reader.source.halfway) })
		<-reader.source.release
	}
	end := len(data)
	if reader.offset == 0 {
		end = len(data) / 2
	}
	n := copy(p, data[reader.offset:end])
	reader.offset += n
	return n, nil
}

// An object of an immutable catalog is not in the local graph, and a sweep of
// the store takes what the graph does not hold. Resolving one must install and
// open it whole while sweeps go on: one took the temporary file the object was
// being written to, or the object before it was open.
func TestResolverInstallsAnObjectTheGraphDoesNotHoldDespiteSweeps(t *testing.T) {
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
	data := bytes.Repeat([]byte("catalog-object"), 8192)
	object := Object{ID: ObjectID{Snapshot: 77, Kind: ObjectKindFile, Ordinal: 3}, State: ObjectLive, BulkLength: uint64(len(data))}
	shared := &pausingObjectSource{data: data, halfway: make(chan struct{}), release: make(chan struct{})}
	resolver, err := NewResolver(graph, cache, testOriginLocator{}, testPeerSource{}, shared)
	if err != nil {
		t.Fatal(err)
	}
	type resolved struct {
		file *os.File
		err  error
	}
	done := make(chan resolved, 1)
	go func() {
		file, err := resolver.ResolveKnown(context.Background(), object)
		done <- resolved{file, err}
	}()
	<-shared.halfway
	swept := make(chan error, 1)
	go func() {
		_, _, err := cache.ReclaimUnreferenced(graph)
		swept <- err
	}()
	select {
	case err := <-swept:
		t.Fatalf("a sweep crossed the installation of an object: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(shared.release)
	result := <-done
	if result.err != nil {
		t.Fatalf("resolving the object: %v", result.err)
	}
	defer result.file.Close()
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	// The sweep has taken the object, which the open file still reads.
	got, err := io.ReadAll(result.file)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the resolved object reads %d bytes, %v; want %d", len(got), err, len(data))
	}
	file, err := resolver.ResolveKnown(context.Background(), object)
	if err != nil {
		t.Fatalf("resolving the object after a sweep took it: %v", err)
	}
	file.Close()
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
