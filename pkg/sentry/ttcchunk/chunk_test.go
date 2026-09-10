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
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

type remoteReaderFunc func(ID) ([]byte, error)

func (f remoteReaderFunc) ReadMemoryChunk(id ID) ([]byte, error) { return f(id) }

type graphRelaySource map[ttcsnapshot.ObjectID][]byte

func (source graphRelaySource) Fetch(_ context.Context, id ttcsnapshot.ObjectID) (io.ReadCloser, uint64, error) {
	data, ok := source[id]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), uint64(len(data)), nil
}

func TestRemoteChunkFetchedAndCachedOnFirstRead(t *testing.T) {
	logical := pgalloc.LogicalChunkID{Allocator: pgalloc.LogicalAllocatorID{1}, Sequence: 7}
	version := pgalloc.PageVersionID{Allocator: logical.Allocator, Sequence: 9}
	encoded, id, err := Encode(Chunk{Logical: logical, Pages: []Page{{Slot: 3, Version: version, Data: make([]byte, hostarch.PageSize)}}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	created, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := OpenMemoryRepositoryFD(dir)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	var token RemoteToken
	token[0] = 0x42
	done := make(chan error, 1)
	var requests atomic.Int32
	go func() {
		done <- ServeRemoteChunks(server, token, remoteReaderFunc(func(got ID) ([]byte, error) {
			requests.Add(1)
			if got != id {
				t.Fatalf("requested chunk %s, want %s", got, id)
			}
			return encoded, nil
		}), map[ID]struct{}{id: {}})
	}()
	manifest := Manifest{Runs: []ManifestRun{{
		ResourceID: checkpoint.ResourceID{}, OldOffset: 0, References: 1,
		FirstPage:    pgalloc.LogicalPageID{Allocator: logical.Allocator, Sequence: logical.Sequence*PagesPerChunk + 3},
		FirstVersion: version, Logical: logical, FirstSlot: 3, Pages: 1, Object: id,
	}}}
	manifestData, manifestID, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(root, "manifests", manifestID.String()[:4])
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, manifestID.String()+".manifest"), manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemoteChunkSource(client, token, manifestID); err != nil {
		t.Fatal(err)
	}
	mf := newChunkTestMemoryFile(t, checkpoint.ResourceID{})
	installed, err := target.InstallManifest(mf, manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("manifest installation fetched %d remote chunks, want 0", got)
	}
	if err := installed.Abort(); err != nil {
		t.Fatal(err)
	}
	mf.Destroy()
	chunk, err := target.chunks.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Logical != logical {
		t.Fatalf("logical chunk = %+v, want %+v", chunk.Logical, logical)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("first payload read fetched %d remote chunks, want 1", got)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("server exit = %v", err)
	}
	name := id.String()
	if _, err := os.Stat(filepath.Join(root, "chunks", name[:4], name+".chunk")); err != nil {
		t.Fatalf("cached chunk: %v", err)
	}
}

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

func TestImportSnapshotGraphPreservesPageAliasing(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	chunk := testChunk()
	encoded, chunkID, err := Encode(chunk)
	if err != nil {
		t.Fatal(err)
	}
	page := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(chunk.Pages[0].Slot)}
	bindings := []StoredBinding{
		{Binding: pgalloc.CapturedPageBinding{OldOffset: 0, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
		{Binding: pgalloc.CapturedPageBinding{OldOffset: hostarch.PageSize, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
	}
	manifest, err := repository.Publish([]Object{{ID: chunkID, Logical: chunk.Logical, Data: encoded}}, bindings)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	imported, err := repository.ImportSnapshotGraph(graph, data, origin, manifest, "family/main/memory")
	if err != nil {
		t.Fatal(err)
	}
	if imported.Chunks != 1 || imported.Pages != 2 || len(imported.Roots) != 1 {
		t.Fatalf("graph import=%+v, want one chunk, two mappings, and one root", imported)
	}
	first, ok, err := graph.MemoryPage(imported.Roots[0].Root, 0)
	if err != nil || !ok {
		t.Fatalf("first page=(%s,%t,%v)", first, ok, err)
	}
	second, ok, err := graph.MemoryPage(imported.Roots[0].Root, 1)
	if err != nil || !ok || second != first {
		t.Fatalf("aliased page=(%s,%t,%v), want %s", second, ok, err, first)
	}
	graphChunk, offset, identity, err := graph.MemoryPageRecord(first)
	if err != nil || offset != 0 {
		t.Fatalf("page backing=(%s,%d,%v)", ttcsnapshot.ObjectID(graphChunk), offset, err)
	}
	if identity.LogicalAllocator != [16]byte(page.Allocator) || identity.LogicalSequence != page.Sequence || identity.VersionAllocator != [16]byte(chunk.Pages[0].Version.Allocator) || identity.VersionSequence != chunk.Pages[0].Version.Sequence {
		t.Fatalf("page identity=%+v, want logical=%+v version=%+v", identity, page, chunk.Pages[0].Version)
	}
	adapted, err := BuildGraphManifest(graph, imported.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(adapted.Manifest.Runs) != 2 || len(adapted.Chunks) != 1 {
		t.Fatalf("graph manifest has %d runs and %d chunks, want 2 and 1", len(adapted.Manifest.Runs), len(adapted.Chunks))
	}
	if adapted.Manifest.Runs[0].Object != adapted.Manifest.Runs[1].Object || adapted.Manifest.Runs[0].FirstPage != adapted.Manifest.Runs[1].FirstPage {
		t.Fatalf("graph manifest lost aliasing: %+v", adapted.Manifest.Runs)
	}
	objects, err := graph.SnapshotObjects(origin)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ttcsnapshot.NewImmutableCatalog(objects)
	if err != nil {
		t.Fatal(err)
	}
	fromCatalog, err := BuildGraphManifest(catalog, imported.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromCatalog, adapted) {
		t.Fatalf("immutable-catalog manifest=%+v, want %+v", fromCatalog, adapted)
	}
	reader, err := NewGraphChunkReader(data, adapted.Chunks, []InstalledChunk{{
		Object: adapted.Manifest.Runs[0].Object,
		Target: memmap.FileRange{End: pgalloc.SparseChunkBytes},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded := make([]byte, hostarch.PageSize)
	if _, err := reader.ReadAt(loaded, int64(chunk.Pages[0].Slot)*hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, chunk.Pages[0].Data) {
		t.Fatal("graph chunk reader returned different page bytes")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := data.Open(ttcsnapshot.ObjectID(graphChunk), ttcsnapshot.MemoryChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(raw[:hostarch.PageSize], chunk.Pages[0].Data) {
		t.Fatalf("graph chunk payload bytes=%d err=%v", len(raw), err)
	}
}

func TestGraphChunkReaderRelaysMissingObject(t *testing.T) {
	data, err := ttcsnapshot.OpenLocalDataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var chunk ID
	chunk[0] = 1
	object := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMemoryChunk, Ordinal: 3}
	want := bytes.Repeat([]byte{0x6d}, pgalloc.SparseChunkBytes)
	server, client := net.Pipe()
	var token ttcsnapshot.ObjectRelayToken
	token[0] = 9
	done := make(chan error, 1)
	go func() {
		done <- ttcsnapshot.ServeObjectRelay(server, token, graphRelaySource{object: want}, map[ttcsnapshot.ObjectID]uint64{object: uint64(len(want))})
	}()
	remote, err := ttcsnapshot.NewObjectRelayClient(client, token)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewGraphChunkReaderWithRelay(data, map[ID]ttcsnapshot.ObjectID{chunk: object}, []InstalledChunk{{
		Object: chunk,
		Target: memmap.FileRange{End: pgalloc.SparseChunkBytes},
	}}, remote, false)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if _, err := reader.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[:len(got)]) {
		t.Fatal("relayed graph chunk bytes differ")
	}
	if file, err := data.Open(object, int64(len(want))); err != nil {
		t.Fatalf("relayed graph chunk was not installed: %v", err)
	} else {
		file.Close()
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestGraphCaptureStreamImportsWithoutRepository(t *testing.T) {
	chunk := testChunk()
	encoded, chunkID, err := Encode(chunk)
	if err != nil {
		t.Fatal(err)
	}
	page := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(chunk.Pages[0].Slot)}
	stored := []StoredBinding{
		{Binding: pgalloc.CapturedPageBinding{OldOffset: 0, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
		{Binding: pgalloc.CapturedPageBinding{OldOffset: hostarch.PageSize, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
	}
	var stream bytes.Buffer
	if err := writeGraphCapture(&stream, []Object{{ID: chunkID, Logical: chunk.Logical, Data: encoded}}, stored); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ImportGraphCapture(bytes.NewReader(stream.Bytes()), graph, data, origin, "stream/main/memory")
	if err != nil {
		t.Fatal(err)
	}
	if result.Chunks != 1 || result.Pages != 2 || len(result.Roots) != 1 {
		t.Fatalf("graph stream import=%+v, want one chunk, two mappings, and one root", result)
	}
	first, ok, err := graph.MemoryPage(result.Roots[0].Root, 0)
	if err != nil || !ok {
		t.Fatalf("first page=(%s,%t,%v)", first, ok, err)
	}
	second, ok, err := graph.MemoryPage(result.Roots[0].Root, 1)
	if err != nil || !ok || second != first {
		t.Fatalf("aliased page=(%s,%t,%v), want %s", second, ok, err, first)
	}

	for name, input := range map[string][]byte{
		"truncated": stream.Bytes()[:len(stream.Bytes())-1],
		"trailing":  append(append([]byte(nil), stream.Bytes()...), 1),
	} {
		t.Run(name, func(t *testing.T) {
			otherGraph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
			if err != nil {
				t.Fatal(err)
			}
			defer otherGraph.Close()
			otherData, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(t.TempDir(), "objects"))
			if err != nil {
				t.Fatal(err)
			}
			defer otherData.Close()
			otherOrigin, err := otherGraph.AllocateWorking()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportGraphCapture(bytes.NewReader(input), otherGraph, otherData, otherOrigin, "bad/memory"); err == nil {
				t.Fatal("malformed graph capture stream imported successfully")
			}
		})
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

func TestMemoryRepositoryExportBranchIsSelfContained(t *testing.T) {
	source, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	chunk := testChunk()
	pages := make([]SourcePage, len(chunk.Pages))
	for i, page := range chunk.Pages {
		pages[i] = SourcePage{
			Binding: pgalloc.CapturedPageBinding{
				OldOffset: uint64(i) * hostarch.PageSize, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
				Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
			},
			Zero: page.Zero, Data: page.Data,
		}
	}
	objects, bindings, err := Assemble(pages)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Publish(objects, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetBranch("capture", manifest); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot")
	if err := source.ExportBranch("capture", "main", destination); err != nil {
		t.Fatal(err)
	}
	exported, err := OpenMemoryRepository(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer exported.Close()
	if got, err := exported.Branch("main"); err != nil || got != manifest {
		t.Fatalf("exported Branch(main) = %s, %v; want %s", got, err, manifest)
	}
	for _, object := range objects {
		if _, err := exported.chunks.Get(object.ID); err != nil {
			t.Fatalf("exported chunk %s: %v", object.ID, err)
		}
	}
}

func TestMemoryRepositoryLazyBranchAndDeltaPermitInheritedObjects(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifestID := publishLifecycleManifest(t, repository, 0x6d)
	if err := repository.SetBranch("source", manifestID); err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.LoadManifest(manifestID)
	if err != nil {
		t.Fatal(err)
	}
	object := manifest.Runs[0].Object
	delta := filepath.Join(t.TempDir(), "delta")
	if err := repository.ExportBranchDelta("source", "child", delta, map[ID]struct{}{object: {}}); err != nil {
		t.Fatal(err)
	}
	name := object.String()
	if _, err := os.Stat(filepath.Join(delta, "chunks", name[:4], name+".chunk")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited chunk was exported: %v", err)
	}
	deltaRepository, err := OpenMemoryRepository(delta)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := deltaRepository.Branch("child"); err != nil || got != manifestID {
		t.Fatalf("delta branch = (%s, %v), want %s", got, err, manifestID)
	}
	if err := deltaRepository.Close(); err != nil {
		t.Fatal(err)
	}
	_, objectPath := repository.chunks.path(object)
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetBranch("strict-child", manifestID); err == nil {
		t.Fatal("strict branch accepted unavailable object")
	}
	if err := repository.SetBranchLazy("lazy-child", manifestID); err != nil {
		t.Fatalf("SetBranchLazy: %v", err)
	}
	if got, err := repository.Branch("lazy-child"); err != nil || got != manifestID {
		t.Fatalf("lazy branch = (%s, %v), want %s", got, err, manifestID)
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

func TestBranchSetUsageTracksDurableSharedPages(t *testing.T) {
	repository, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x4d)
	for _, branch := range []string{"main", "child"} {
		if err := repository.SetBranch(branch, manifest); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := repository.BranchSetUsage(map[string]string{"main": "main", "child": "child"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.UniqueBytes != hostarch.PageSize || len(usage.Branches) != 2 {
		t.Fatalf("usage=%+v, want one unique page", usage)
	}
	for _, branch := range usage.Branches {
		if branch.PrivateBytes != 0 || branch.SharedBytes != hostarch.PageSize {
			t.Fatalf("branch usage=%+v, want one shared page", branch)
		}
	}
	loaded, err := repository.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "child"} {
		if err := repository.MarkConsumed(branch, manifest, []ID{loaded.Runs[0].Object}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.CollectGarbage(GCOptions{}); err != nil {
		t.Fatal(err)
	}
	usage, err = repository.BranchSetUsage(map[string]string{"main": "main", "child": "child"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.UniqueBytes != 0 {
		t.Fatalf("consumed durable usage=%+v, want zero", usage)
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

func TestConsumptionTrackerLeaseProtectsUnreadObjects(t *testing.T) {
	root := t.TempDir()
	repository, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, repository, 0x75)
	loaded, err := repository.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object := loaded.Runs[0].Object
	if err := repository.SetBranch("branch", manifest); err != nil {
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
	reader, err := NewOwnedConsumingChunkReader(donated, []InstalledChunk{{
		Object: object, Target: memmap.FileRange{Start: 0, End: pgalloc.SparseChunkBytes}, StreamOffset: 0,
	}}, "branch", manifest)
	if err != nil {
		t.Fatal(err)
	}
	reader.ActivateConsumption(nil)
	if err := repository.DeleteBranch("branch"); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 0 || got.Manifests != 0 {
		t.Fatalf("GC with live sparse reader = %+v, %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 1 || got.Manifests != 1 {
		t.Fatalf("GC after sparse reader close = %+v, %v", got, err)
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

func TestRepositoryGarbageCollectionAcceptsLazyMissingObjects(t *testing.T) {
	source, err := OpenMemoryRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishLifecycleManifest(t, source, 0x48)
	loaded, err := source.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object := loaded.Runs[0].Object

	root := t.TempDir()
	target, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ImportRemoteManifest(source, manifest, "lazy"); err != nil {
		t.Fatal(err)
	}
	// Model one demand-fetched object. Other objects named by a general
	// manifest are deliberately absent from the local sparse cache.
	file, err := source.chunks.Open(object)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if err := target.chunks.PutObject(Object{ID: object, Logical: loaded.Runs[0].Logical, Data: data}); err != nil {
		t.Fatal(err)
	}

	collector, err := OpenMemoryRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := target.AcquireLease(manifest, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := collector.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 0 {
		t.Fatalf("lazy GC with reader lease = %+v, %v", got, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := target.MarkConsumed("lazy", manifest, []ID{object}); err != nil {
		t.Fatal(err)
	}
	if got, err := collector.CollectGarbage(GCOptions{}); err != nil || got.Chunks != 1 {
		t.Fatalf("lazy GC after consumption = %+v, %v", got, err)
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
