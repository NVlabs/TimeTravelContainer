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
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

type graphRelaySource map[ttcsnapshot.ObjectID][]byte

func (source graphRelaySource) Fetch(_ context.Context, id ttcsnapshot.ObjectID) (io.ReadCloser, uint64, error) {
	data, ok := source[id]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), uint64(len(data)), nil
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
	data, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
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
	data[0] ^= 1
	if _, err := Decode(data); err == nil {
		t.Fatal("invalid chunk header passed structural validation")
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
	encoded, err := Encode(chunk)
	if err != nil {
		t.Fatal(err)
	}
	chunkID := graphCaptureObjectID(1)
	page := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(chunk.Pages[0].Slot)}
	stored := []StoredBinding{
		{Binding: pgalloc.CapturedPageBinding{OldOffset: 0, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
		{Binding: pgalloc.CapturedPageBinding{OldOffset: hostarch.PageSize, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, Object: chunkID},
	}
	var stream bytes.Buffer
	if err := writeGraphCaptureHeader(&stream, stored, 1); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphCaptureObject(&stream, chunkID, encoded); err != nil {
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

func TestGraphImportReusesStableMemoryPages(t *testing.T) {
	chunk := testChunk()
	object := graphCaptureObjectID(1)
	stored := make([]StoredBinding, 0, len(chunk.Pages))
	for _, page := range chunk.Pages {
		logicalPage := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)}
		stored = append(stored, StoredBinding{Binding: pgalloc.CapturedPageBinding{
			OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 2,
			Page: logicalPage, Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, Object: object})
	}
	manifest, err := CompactBindings(stored)
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
	defer data.Close()
	firstOrigin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := importSnapshotGraph(graph, data, firstOrigin, manifest, "reuse/first", func(ID) (Chunk, error) { return chunk, nil })
	if err != nil {
		t.Fatal(err)
	}
	firstPages, err := graph.MemoryPages(first.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	chunksBefore := 0
	for _, object := range graph.Inspect().Objects {
		if object.ID.Kind == ttcsnapshot.ObjectKindMemoryChunk {
			chunksBefore++
		}
	}
	secondOrigin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	second, err := importSnapshotGraph(graph, data, secondOrigin, manifest, "reuse/second", func(ID) (Chunk, error) { return chunk, nil })
	if err != nil {
		t.Fatal(err)
	}
	secondPages, err := graph.MemoryPages(second.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secondPages, firstPages) {
		t.Fatalf("second capture pages=%v, want reused PageIDs %v", secondPages, firstPages)
	}
	chunksAfter := 0
	for _, object := range graph.Inspect().Objects {
		if object.ID.Kind == ttcsnapshot.ObjectKindMemoryChunk {
			chunksAfter++
		}
	}
	if chunksAfter != chunksBefore {
		t.Fatalf("second capture stored %d chunks, want unchanged %d", chunksAfter, chunksBefore)
	}
}

func TestGraphImportCheckpointBoundsRecoveryJournal(t *testing.T) {
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	if _, err := graph.AllocateWorking(); err != nil {
		t.Fatal(err)
	}
	before := graph.JournalBytes()
	if before <= int64(len("TTCSGJ2\n")) {
		t.Fatalf("graph mutation did not grow journal: %d", before)
	}
	if err := checkpointGraphImportIfNeeded(graph, before+1); err != nil {
		t.Fatal(err)
	}
	if got := graph.JournalBytes(); got != before {
		t.Fatalf("journal checkpointed below threshold: got %d, want %d", got, before)
	}
	if err := checkpointGraphImportIfNeeded(graph, before); err != nil {
		t.Fatal(err)
	}
	if got := graph.JournalBytes(); got >= before {
		t.Fatalf("journal remained at %d bytes after checkpoint, was %d", got, before)
	}
}

func TestGraphImportDoesNotReuseSharedCachePages(t *testing.T) {
	chunk := testChunk()
	object := graphCaptureObjectID(1)
	stored := make([]StoredBinding, 0, len(chunk.Pages))
	for _, page := range chunk.Pages {
		stored = append(stored, StoredBinding{Binding: pgalloc.CapturedPageBinding{
			OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 2,
			Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
			Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, Object: object})
	}
	manifest, err := CompactBindings(stored)
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
	defer data.Close()
	firstOrigin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := importSnapshotGraph(graph, data, firstOrigin, manifest, "shared/first", func(ID) (Chunk, error) { return chunk, nil })
	if err != nil {
		t.Fatal(err)
	}
	firstPages, err := graph.MemoryPages(first.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(firstOrigin, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := graph.MarkShared(firstOrigin); err != nil {
		t.Fatal(err)
	}
	secondOrigin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	second, err := importSnapshotGraphWithReusableStates(graph, data, secondOrigin, manifest, "shared/second", func(ID) (Chunk, error) { return chunk, nil }, []ttcsnapshot.SnapshotState{
		ttcsnapshot.SnapshotWorking, ttcsnapshot.SnapshotSealedVolatile,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondPages, err := graph.MemoryPages(second.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(secondPages, firstPages) {
		t.Fatalf("cluster capture reused shared-cache PageIDs %v", secondPages)
	}
	for _, page := range secondPages {
		if page.Snapshot != secondOrigin {
			t.Fatalf("second capture page %s belongs to shared origin %d", page, firstOrigin)
		}
	}
}

func TestManifestCompactsDenseChunkAndRoundTrips(t *testing.T) {
	allocator := testAllocator(11)
	versionAllocator := testAllocator(22)
	logical := pgalloc.LogicalChunkID{Allocator: allocator, Sequence: 9}
	object := graphCaptureObjectID(9)
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
	data, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 512 {
		t.Fatalf("one dense chunk manifest uses %d bytes, want less than 512", len(data))
	}
	decoded, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("decoded manifest = %+v, want %+v", decoded, manifest)
	}
	if expanded := decoded.Expand(); !reflect.DeepEqual(expanded, stored) {
		t.Fatalf("expanded bindings differ from source")
	}
	data[0] ^= 1
	if _, err := DecodeManifest(data); err == nil {
		t.Fatal("invalid manifest header passed structural validation")
	}
}
