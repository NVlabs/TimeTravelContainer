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

package ttcchunk

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

func testAllocator(seed byte) pgalloc.LogicalAllocatorID {
	var id pgalloc.LogicalAllocatorID
	for i := range id {
		id[i] = seed + byte(i)
	}
	return id
}

func testChunk() Chunk {
	return Chunk{
		Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: 42},
		Pages: []Page{
			{Slot: 0, Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: 100}, Data: bytes.Repeat([]byte{0xa5}, hostarch.PageSize)},
			{Slot: 17, Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: 117}, Zero: true},
			{Slot: 255, Version: pgalloc.PageVersionID{Allocator: testAllocator(3), Sequence: 999}, Data: bytes.Repeat([]byte{0x5a}, hostarch.PageSize)},
		},
	}
}

func TestChunkRoundTrip(t *testing.T) {
	want := testChunk()
	data, id, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data, &id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Logical != want.Logical || len(got.Pages) != len(want.Pages) {
		t.Fatalf("decoded chunk = %+v, want %+v", got, want)
	}
	for i := range want.Pages {
		if got.Pages[i].Slot != want.Pages[i].Slot || got.Pages[i].Version != want.Pages[i].Version || got.Pages[i].Zero != want.Pages[i].Zero || !bytes.Equal(got.Pages[i].Data, want.Pages[i].Data) {
			t.Fatalf("decoded page %d = %+v, want %+v", i, got.Pages[i], want.Pages[i])
		}
	}
	data[len(data)-1] ^= 1
	if _, err := Decode(data, &id); err == nil {
		t.Fatal("corrupted chunk passed content-address verification")
	}
}

func TestStoreConcurrentIdempotentPutAndCorruption(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	chunk := testChunk()
	const writers = 8
	ids := make(chan ID, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.Put(chunk)
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id ID
	for got := range ids {
		if id == (ID{}) {
			id = got
		} else if got != id {
			t.Fatalf("concurrent Put returned %s and %s", id, got)
		}
	}
	got, err := store.Get(id)
	if err != nil || len(got.Pages) != len(chunk.Pages) {
		t.Fatalf("Get: pages=%d err=%v", len(got.Pages), err)
	}
	dir, path := store.path(id)
	if filepath.Base(dir) != id.String()[:4] {
		t.Fatalf("chunk shard = %q, want %q", filepath.Base(dir), id.String()[:4])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(id); err == nil {
		t.Fatal("Get accepted corrupted immutable chunk")
	}
}
