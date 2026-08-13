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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
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
	if reusedID, err := repository.Publish(nil, bindings); err != nil || reusedID != manifestID {
		t.Fatalf("reusing published objects = %s, %v; want %s", reusedID, err, manifestID)
	}
	badBindings := append([]StoredBinding(nil), bindings...)
	badBindings[0].Binding.Version.Sequence++
	if _, err := repository.Publish(nil, badBindings); err == nil {
		t.Fatal("repository accepted a manifest binding absent from its object")
	}
	const branch = "feature/one@remote"
	if err := repository.SetBranch(branch, manifestID); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.Branch(branch); err != nil || got != manifestID {
		t.Fatalf("Branch = %s, %v; want %s", got, err, manifestID)
	}
	if roots, err := repository.ListBranches(); err != nil || len(roots) != 1 || roots[branch] != manifestID {
		t.Fatalf("ListBranches = %+v, %v; want %q=%s", roots, err, branch, manifestID)
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
	if roots, err := repository.ListBranches(); err != nil || len(roots) != 0 {
		t.Fatalf("ListBranches after delete = %+v, %v; want empty", roots, err)
	}
}

func TestMemoryRepositoryImportManifest(t *testing.T) {
	source, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chunk := testChunk()
	pages := make([]SourcePage, len(chunk.Pages))
	for i, page := range chunk.Pages {
		pages[i] = SourcePage{Binding: pgalloc.CapturedPageBinding{
			OldOffset: uint64(i) * hostarch.PageSize, References: 1,
			Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
			Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, Zero: page.Zero, Data: page.Data}
	}
	objects, bindings, err := Assemble(pages)
	if err != nil {
		t.Fatal(err)
	}
	id, err := source.Publish(objects, bindings)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := destination.ImportManifest(source, id)
	if err != nil {
		t.Fatal(err)
	}
	if imported != id {
		t.Fatalf("imported manifest = %s, want %s", imported, id)
	}
	manifest, err := destination.LoadManifest(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.Expand(), bindings) {
		t.Fatal("imported bindings differ from source")
	}
	for _, object := range objects {
		if _, err := destination.chunks.Get(object.ID); err != nil {
			t.Fatalf("imported object %s: %v", object.ID, err)
		}
	}
	// Re-import must safely reuse immutable content.
	for _, object := range objects {
		_, path := source.chunks.path(object.ID)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := destination.ImportManifest(source, id); err != nil {
		t.Fatalf("idempotent import without source object reads: %v", err)
	}
}

func TestMemoryRepositoryImportManifestConcurrentGarbageCollection(t *testing.T) {
	source, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	destination, err := OpenMemoryRepository(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := OpenMemoryRepository(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Make import long enough for an independently-opened repository to run
	// several collections while objects are arriving. The transaction written
	// before the first object must protect the incomplete graph until SetBranch
	// replaces it with a durable root.
	const chunks = 16
	allocator := testAllocator(40)
	versionAllocator := testAllocator(80)
	pages := make([]SourcePage, 0, chunks*PagesPerChunk)
	for chunkIndex := 0; chunkIndex < chunks; chunkIndex++ {
		logical := pgalloc.LogicalChunkID{Allocator: allocator, Sequence: uint64(chunkIndex)}
		for slot := 0; slot < PagesPerChunk; slot++ {
			sequence := uint64(chunkIndex*PagesPerChunk + slot)
			pages = append(pages, SourcePage{
				Binding: pgalloc.CapturedPageBinding{
					OldOffset: sequence * hostarch.PageSize, References: 1,
					Page:    pgalloc.LogicalPageID{Allocator: allocator, Sequence: sequence},
					Version: pgalloc.PageVersionID{Allocator: versionAllocator, Sequence: sequence + 1},
					Chunk:   logical, Slot: uint8(slot),
				},
				Data: bytes.Repeat([]byte{byte(chunkIndex + slot)}, hostarch.PageSize),
			})
		}
	}
	objects, bindings, err := Assemble(pages)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := source.Publish(objects, bindings)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	collectorErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				collectorErr <- nil
				return
			default:
			}
			if _, err := collector.CollectGarbage(GCOptions{}); err != nil {
				collectorErr <- err
				return
			}
		}
	}()
	imported, err := destination.ImportManifest(source, manifestID)
	if err == nil {
		err = destination.SetBranch("remote", imported)
	}
	close(stop)
	if collectorResult := <-collectorErr; collectorResult != nil {
		t.Fatal(collectorResult)
	}
	if err != nil {
		t.Fatal(err)
	}
	if imported != manifestID {
		t.Fatalf("imported manifest = %s, want %s", imported, manifestID)
	}
	if err := destination.validateManifestObjects(manifestID); err != nil {
		t.Fatalf("collector reclaimed an imported object: %v", err)
	}
}

func TestInstallManifestRelocatesAndReadsWholeChunk(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chunk := testChunk()
	resource := checkpoint.ResourceID{Path: "/memory"}
	source := make([]SourcePage, len(chunk.Pages))
	for i, page := range chunk.Pages {
		source[i] = SourcePage{
			Binding: pgalloc.CapturedPageBinding{
				ResourceID: resource, OldOffset: uint64(i) * hostarch.PageSize, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
				Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
			},
			Zero: page.Zero, Data: page.Data,
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
	file, err := os.CreateTemp("", "ttc-chunk-install-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		ResourceID: resource, DelayedEviction: pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround: true, DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	installed, err := repository.InstallManifest(mf, manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.Chunks) != 1 {
		t.Fatalf("installed %d new chunks, want 1", len(installed.Chunks))
	}
	if err := installed.Commit(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewChunkReader(repository, installed.Chunks)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make([]byte, pgalloc.SparseChunkBytes)
	if n, err := reader.ReadAt(decoded, 0); err != nil || n != len(decoded) {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	for i, page := range chunk.Pages {
		gotOffset, ok := installed.Relocation.Page(resource, uint64(i)*hostarch.PageSize)
		wantOffset := installed.Chunks[0].Target.Start + uint64(page.Slot)*hostarch.PageSize
		if !ok || gotOffset != wantOffset {
			t.Fatalf("page %d relocation=(%#x,%t), want %#x", i, gotOffset, ok, wantOffset)
		}
		got := decoded[uint64(page.Slot)*hostarch.PageSize : (uint64(page.Slot)+1)*hostarch.PageSize]
		if page.Zero {
			if !bytes.Equal(got, make([]byte, hostarch.PageSize)) {
				t.Fatalf("zero slot %d is nonzero", page.Slot)
			}
		} else if !bytes.Equal(got, page.Data) {
			t.Fatalf("slot %d payload mismatch", page.Slot)
		}
	}
	reused, err := repository.InstallManifest(mf, manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reused.Chunks) != 0 {
		t.Fatalf("reinstall allocated chunks: %+v", reused.Chunks)
	}
	if err := reused.Commit(); err != nil {
		t.Fatal(err)
	}
}

func newChunkTestMemoryFile(t *testing.T, resource checkpoint.ResourceID) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-chunk-memory-file-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		ResourceID: resource, DelayedEviction: pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround: true, DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

func TestInstallManifestSetUsesResourceMemoryFiles(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resources := []checkpoint.ResourceID{{}, {Path: "/private-tmpfs"}}
	objects := make([]Object, 0, len(resources))
	bindings := make([]StoredBinding, 0, len(resources))
	files := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
	for i, resource := range resources {
		chunk := testChunk()
		chunk.Logical.Allocator = testAllocator(byte(40 + i))
		chunk.Logical.Sequence += uint64(i)
		chunk.Pages = chunk.Pages[:1]
		chunk.Pages[0].Data = bytes.Repeat([]byte{byte(0x70 + i)}, hostarch.PageSize)
		data, object, err := Encode(chunk)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, Object{ID: object, Logical: chunk.Logical, Data: data})
		bindings = append(bindings, StoredBinding{
			Binding: pgalloc.CapturedPageBinding{
				ResourceID: resource, OldOffset: 0, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(chunk.Pages[0].Slot)},
				Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot,
			},
			Object: object,
		})
		mf := newChunkTestMemoryFile(t, resource)
		defer mf.Destroy()
		files[resource] = mf
	}
	manifestID, err := repository.Publish(objects, bindings)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.LoadManifest(manifestID)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := repository.InstallManifestSet(files, manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.Chunks) != len(resources) {
		t.Fatalf("installed %d chunks, want %d", len(installed.Chunks), len(resources))
	}
	for _, resource := range resources {
		offset, ok := installed.Relocation.Page(resource, 0)
		if !ok {
			t.Fatalf("missing relocation for %s", resource)
		}
		found := false
		for _, chunk := range installed.Chunks {
			if chunk.ResourceID == resource {
				found = true
				if chunk.MemoryFile != files[resource] || offset < chunk.Target.Start || offset >= chunk.Target.End {
					t.Fatalf("chunk for %s installed into wrong MemoryFile/range: %+v, offset=%#x", resource, chunk, offset)
				}
			}
		}
		if !found {
			t.Fatalf("no installed chunk for %s", resource)
		}
	}
	if err := installed.Commit(); err != nil {
		t.Fatal(err)
	}
}

func publishLifecycleManifest(t *testing.T, repository *MemoryRepository, fill byte) ManifestID {
	t.Helper()
	chunk := testChunk()
	chunk.Pages[0].Data = bytes.Repeat([]byte{fill}, hostarch.PageSize)
	data, object, err := Encode(chunk)
	if err != nil {
		t.Fatal(err)
	}
	binding := StoredBinding{
		Binding: pgalloc.CapturedPageBinding{
			ResourceID: checkpoint.ResourceID{Path: "/memory"}, OldOffset: 0, References: 1,
			Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(chunk.Pages[0].Slot)},
			Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot,
		},
		Object: object,
	}
	id, err := repository.Publish([]Object{{ID: object, Logical: chunk.Logical, Data: data}}, []StoredBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPackedManifestReaderPreservesDistributedStreamLayout(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x5a)
	reader, err := NewPackedManifestReader(repository, manifest, []pgalloc.RemoteLoadRange{{
		Target: memmap.FileRange{Start: 0, End: hostarch.PageSize}, StreamOffset: 2 * hostarch.PageSize,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := make([]byte, hostarch.PageSize)
	if n, err := reader.ReadAt(got, 2*hostarch.PageSize); err != nil || n != len(got) {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{0x5a}, len(got))) {
		t.Fatal("packed reader returned incorrect page contents")
	}
	if _, err := reader.ReadAt(make([]byte, hostarch.PageSize), 0); !errors.Is(err, io.EOF) {
		t.Fatalf("unmapped packed offset error=%v, want EOF", err)
	}
}

func TestDonatedRepositoryPublishesAndReadsRelativeToFD(t *testing.T) {
	root := t.TempDir()
	owner, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	donated, err := OpenMemoryRepositoryFD(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, donated, 0x42)
	loaded, err := donated.LoadManifest(manifest)
	if err != nil || len(loaded.Runs) != 1 {
		t.Fatalf("donated LoadManifest: runs=%d err=%v", len(loaded.Runs), err)
	}
	if _, err := donated.chunks.Get(loaded.Runs[0].Object); err != nil {
		t.Fatalf("donated chunk Get: %v", err)
	}
	if err := donated.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.SetBranch("branch", manifest); err != nil {
		t.Fatalf("rooting donated publication: %v", err)
	}
	if got, err := owner.Branch("branch"); err != nil || got != manifest {
		t.Fatalf("owner branch=(%v, %v), want %v", got, err, manifest)
	}
}

func TestRepositoryLeaseProtectsThenReclaimsManifest(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x31)
	if err := repository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(manifest, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteBranch("branch"); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 0 || got.Manifests != 0 {
		t.Fatalf("GC with active lease = %+v, %v", got, err)
	}
	if err := lease.Renew(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 1 || got.Manifests != 1 {
		t.Fatalf("GC after lease release = %+v, %v", got, err)
	}
	if err := repository.validateManifestObjects(manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reclaimed manifest remains available: %v", err)
	}
}

func TestRepositoryConsumptionReclaimsInstalledChunk(t *testing.T) {
	root := t.TempDir()
	repository, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x71)
	loaded, err := repository.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object := loaded.Runs[0].Object
	if err := repository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkConsumed("branch", manifest, []ID{object}); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkConsumed("branch", manifest, []ID{object}); err != nil {
		t.Fatalf("idempotent consumption update: %v", err)
	}
	result, err := repository.CollectGarbage(GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Chunks != 1 || result.Manifests != 0 {
		t.Fatalf("consumption GC = %+v, want one chunk and retained manifest", result)
	}
	if _, err := repository.LoadManifest(manifest); err != nil {
		t.Fatalf("consumption reclaimed branch manifest: %v", err)
	}
	if err := repository.chunks.Contains(object); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed object remains: %v", err)
	}

	// A new root replaces the old consumption generation conservatively.
	newManifest := publishLifecycleManifest(t, repository, 0x72)
	if err := repository.SetBranch("branch", newManifest); err != nil {
		t.Fatal(err)
	}
	newLoaded, err := repository.LoadManifest(newManifest)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 0 {
		t.Fatalf("replacement root lost unconsumed object: %+v, %v", got, err)
	}
	if err := repository.chunks.Contains(newLoaded.Runs[0].Object); err != nil {
		t.Fatalf("replacement object unavailable: %v", err)
	}
}

func TestDonatedRepositoryRecordsConsumption(t *testing.T) {
	root := t.TempDir()
	owner, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, owner, 0x73)
	loaded, err := owner.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	donated, err := OpenMemoryRepositoryFD(dir)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := donated.MarkConsumed("branch", manifest, []ID{loaded.Runs[0].Object}); err != nil {
		t.Fatal(err)
	}
	if err := donated.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := owner.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 1 {
		t.Fatalf("donated consumption GC = %+v, %v", got, err)
	}
}

func TestConsumptionTrackerWaitsForRestoreCommit(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x74)
	loaded, err := repository.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object := loaded.Runs[0].Object
	if err := repository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	reader, err := NewOwnedConsumingChunkReader(repository, []InstalledChunk{{
		Object: object, Target: memmap.FileRange{Start: 0, End: pgalloc.SparseChunkBytes}, StreamOffset: 0,
	}}, "branch", manifest)
	if err != nil {
		t.Fatal(err)
	}
	reader.RangeLoaded(0, pgalloc.SparseChunkBytes)
	time.Sleep(150 * time.Millisecond)
	record, err := repository.readConsumption("branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Objects) != 0 {
		t.Fatalf("uncommitted restore recorded consumption: %+v", record.Objects)
	}
	reader.ActivateConsumption(nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		record, err = repository.readConsumption("branch")
		if err == nil {
			if _, ok := record.Objects[object]; ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("committed consumption was not persisted: %+v, %v", record, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPublicationTransactionExpires(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x32)
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 0 || got.Manifests != 0 {
		t.Fatalf("GC with publication transaction = %+v, %v", got, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	entries, err := os.ReadDir(repository.txnRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Chtimes(filepath.Join(repository.txnRoot, entry.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repository.CollectGarbage(GCOptions{Now: time.Now(), TransactionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunks != 1 || got.Manifests != 1 || got.ExpiredTransactions != 1 {
		t.Fatalf("expired transaction GC = %+v", got)
	}
	if err := repository.validateManifestObjects(manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired publication remains available: %v", err)
	}
}

func TestRepositoryExpiredLeaseDoesNotProtect(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x33)
	if err := repository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(manifest, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteBranch("branch"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lease.path, old, old); err != nil {
		t.Fatal(err)
	}
	got, err := repository.CollectGarbage(GCOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunks != 1 || got.Manifests != 1 || got.ExpiredLeases != 1 {
		t.Fatalf("expired lease GC = %+v", got)
	}
	if err := lease.Renew(); err == nil {
		t.Fatal("renewed a lease after its manifest was reclaimed")
	}
}

func TestRepositoryGarbageCollectionWaitsForDemandReader(t *testing.T) {
	root := t.TempDir()
	readerRepository, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, readerRepository, 0x35)
	if err := readerRepository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	if err := readerRepository.DeleteBranch("branch"); err != nil {
		t.Fatal(err)
	}

	readLock, err := readerRepository.lockGarbageCollection(unix.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := collector.CollectGarbage(GCOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("garbage collection completed while a demand-reader lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := unlockGarbageCollection(readLock); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("garbage collection did not continue after demand-reader unlock")
	}
}

func TestRepositoryLeaseRenewConcurrentGarbageCollection(t *testing.T) {
	root := t.TempDir()
	owner, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, owner, 0x34)
	if err := owner.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	lease, err := owner.AcquireLease(manifest, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.DeleteBranch("branch"); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 2)
	go func() {
		for i := 0; i < 100; i++ {
			if err := lease.Renew(); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()
	go func() {
		for i := 0; i < 100; i++ {
			if got, err := collector.CollectGarbage(GCOptions{}); err != nil {
				errCh <- err
				return
			} else if got.Chunks != 0 || got.Manifests != 0 {
				errCh <- fmt.Errorf("collector reclaimed leased data: %+v", got)
				return
			}
		}
		errCh <- nil
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got, err := collector.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 1 || got.Manifests != 1 {
		t.Fatalf("final GC = %+v, %v", got, err)
	}
}

func TestRepositoryCorruptRootFailsClosed(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x35)
	if err := repository.SetBranch("branch", manifest); err != nil {
		t.Fatal(err)
	}
	path, err := repository.branchPath("branch")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CollectGarbage(GCOptions{}); err == nil {
		t.Fatal("GC accepted a corrupt durable root")
	}
	if err := repository.validateManifestObjects(manifest); err != nil {
		t.Fatalf("fail-closed GC modified rooted data: %v", err)
	}
}

func TestRepositoryOpenRecoversInterruptedCollection(t *testing.T) {
	root := t.TempDir()
	repository, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x36)
	loaded, err := repository.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object := loaded.Runs[0].Object
	manifestDir, manifestPath := repository.manifestPath(manifest)
	_, objectPath := repository.chunks.path(object)
	quarantine := filepath.Join(repository.trashRoot, "interrupted")
	if err := os.Mkdir(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifestPath, filepath.Join(quarantine, "manifest-"+manifest.String())); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(objectPath, filepath.Join(quarantine, "chunk-"+object.String())); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * DefaultTransactionTTL)
	if err := os.Chtimes(quarantine, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestDir); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.validateManifestObjects(manifest); err != nil {
		t.Fatalf("recovered repository is incomplete: %v", err)
	}
	entries, err := os.ReadDir(recovered.trashRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("recovery left trash entries: %v", entries)
	}
}
