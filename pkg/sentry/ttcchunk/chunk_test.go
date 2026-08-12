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
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
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

func TestAssembleSeparatesDivergedSlotVersions(t *testing.T) {
	allocator := testAllocator(1)
	logicalChunk := pgalloc.LogicalChunkID{Allocator: allocator, Sequence: 7}
	pageAt := func(slot uint8, version uint64, fill byte) SourcePage {
		return SourcePage{
			Binding: pgalloc.CapturedPageBinding{
				OldOffset:  uint64(version) * hostarch.PageSize,
				References: 1,
				Page:       pgalloc.LogicalPageID{Allocator: allocator, Sequence: logicalChunk.Sequence*PagesPerChunk + uint64(slot)},
				Version:    pgalloc.PageVersionID{Allocator: testAllocator(5), Sequence: version},
				Chunk:      logicalChunk,
				Slot:       slot,
			},
			Data: bytes.Repeat([]byte{fill}, hostarch.PageSize),
		}
	}
	source := []SourcePage{
		pageAt(3, 30, 0x30),
		pageAt(3, 31, 0x31),
		pageAt(9, 90, 0x90),
	}
	objects, bindings, err := Assemble(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || len(bindings) != 3 {
		t.Fatalf("Assemble produced %d objects and %d bindings, want 2 and 3", len(objects), len(bindings))
	}
	objectForVersion := make(map[uint64]ID)
	for _, binding := range bindings {
		objectForVersion[binding.Binding.Version.Sequence] = binding.Object
	}
	if objectForVersion[30] == objectForVersion[31] {
		t.Fatal("two versions of one logical slot were packed into the same object")
	}
	if objectForVersion[90] != objectForVersion[30] {
		t.Fatal("first-fit deterministic packing did not coalesce a non-conflicting slot")
	}
	for _, object := range objects {
		if got := sha256ID(object.Data); got != object.ID {
			t.Fatalf("object ID = %s, content hash = %s", object.ID, got)
		}
	}
}

func sha256ID(data []byte) ID {
	return ID(sha256.Sum256(data))
}

func TestReadSnapshotBuildsObjectsWithoutAcknowledging(t *testing.T) {
	file, err := os.CreateTemp("", "ttc-chunk-snapshot-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	fr, err := mf.Allocate(2*hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.Anonymous,
		Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.DecRef(fr)
	firstPage := bytes.Repeat([]byte{0x7b}, hostarch.PageSize)
	dst, err := mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(dst.TakeFirst64(hostarch.PageSize), safemem.BlockSeqOf(safemem.BlockFromSafeSlice(firstPage))); err != nil || n != hostarch.PageSize {
		t.Fatalf("writing source page: n=%d err=%v", n, err)
	}
	capture := pgalloc.NewMemoryRangeCapture()
	if err := capture.Record(mf, fr); err != nil {
		t.Fatal(err)
	}
	layout, err := capture.SnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(layout.Regions)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	objects, bindings, err := ReadSnapshot(snapshot, layout.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || len(bindings) != 2 {
		t.Fatalf("ReadSnapshot produced %d objects and %d bindings", len(objects), len(bindings))
	}
	decoded, err := Decode(objects[0].Data, &objects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Pages) != 2 || decoded.Pages[0].Zero || !bytes.Equal(decoded.Pages[0].Data, firstPage) || !decoded.Pages[1].Zero {
		t.Fatalf("decoded snapshot pages = %+v", decoded.Pages)
	}
	if stats := snapshot.Stats(); stats.PagesReleased != 0 || stats.PagesOutstanding != 2 {
		t.Fatalf("ReadSnapshot acknowledged source pages: %+v", stats)
	}
}

func TestManifestCompactsDenseChunkAndRoundTrips(t *testing.T) {
	allocator := testAllocator(11)
	versionAllocator := testAllocator(22)
	logical := pgalloc.LogicalChunkID{Allocator: allocator, Sequence: 9}
	object := ID(sha256.Sum256([]byte("dense-object")))
	stored := make([]StoredBinding, PagesPerChunk)
	for i := range stored {
		stored[i] = StoredBinding{
			Binding: pgalloc.CapturedPageBinding{
				OldOffset:  uint64(i) * hostarch.PageSize,
				References: 2,
				Page:       pgalloc.LogicalPageID{Allocator: allocator, Sequence: logical.Sequence*PagesPerChunk + uint64(i)},
				Version:    pgalloc.PageVersionID{Allocator: versionAllocator, Sequence: 1000 + uint64(i)},
				Chunk:      logical,
				Slot:       uint8(i),
			},
			Object: object,
		}
	}
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Runs) != 1 || manifest.Runs[0].Pages != PagesPerChunk {
		t.Fatalf("dense chunk compacted to %+v, want one 256-page run", manifest.Runs)
	}
	data, id, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 512 {
		t.Fatalf("one dense chunk manifest uses %d bytes, want less than 512", len(data))
	}
	decoded, err := DecodeManifest(data, &id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("decoded manifest = %+v, want %+v", decoded, manifest)
	}
	if expanded := decoded.Expand(); !reflect.DeepEqual(expanded, stored) {
		t.Fatalf("expanded bindings differ from source")
	}
	data[len(data)-1] ^= 1
	if _, err := DecodeManifest(data, &id); err == nil {
		t.Fatal("corrupt manifest passed content-address verification")
	}
}

func TestMemoryRepositoryPublishesDataBeforeMutableBranchRoot(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chunk := testChunk()
	source := make([]SourcePage, len(chunk.Pages))
	for i, page := range chunk.Pages {
		source[i] = SourcePage{
			Binding: pgalloc.CapturedPageBinding{
				OldOffset:  uint64(i) * hostarch.PageSize,
				References: 1,
				Page:       pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
				Version:    page.Version,
				Chunk:      chunk.Logical,
				Slot:       page.Slot,
			},
			Zero: page.Zero,
			Data: page.Data,
		}
	}
	objects, bindings, err := Assemble(source)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := repository.Publish(objects, bindings)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.LoadManifest(manifestID)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Expand(); !reflect.DeepEqual(got, bindings) {
		t.Fatalf("published manifest bindings differ: got=%+v want=%+v", got, bindings)
	}
	const branch = "feature/one@remote"
	if err := repository.SetBranch(branch, manifestID); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.Branch(branch); err != nil || got != manifestID {
		t.Fatalf("Branch = %s, %v; want %s", got, err, manifestID)
	}
	emptyID, err := repository.Publish(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetBranch(branch, emptyID); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.Branch(branch); err != nil || got != emptyID {
		t.Fatalf("replaced Branch = %s, %v; want %s", got, err, emptyID)
	}
	if err := repository.DeleteBranch(branch); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Branch(branch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted Branch error = %v, want not-exist", err)
	}
}
