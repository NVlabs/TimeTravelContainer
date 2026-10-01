// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
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
		capturedBinding(pgalloc.CapturedPageBinding{OldOffset: 0, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, chunkID),
		capturedBinding(pgalloc.CapturedPageBinding{OldOffset: hostarch.PageSize, References: 2, Page: page, Version: chunk.Pages[0].Version, Chunk: chunk.Logical, Slot: chunk.Pages[0].Slot}, chunkID),
	}
	var stream bytes.Buffer
	if err := writeStoredCaptureHeader(&stream, graphCaptureMagic, stored, 1); err != nil {
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
	locations := memoryLocations(t, graph, result.Roots[0].Root)
	if len(locations) != 2 || locations[1] != locations[0] {
		t.Fatalf("aliased pages are at %v, want both in one place", locations)
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

// memoryLocations returns where the memory tree at root maps each page index.
func memoryLocations(t *testing.T, graph *ttcsnapshot.Store, root ttcsnapshot.ObjectID) map[uint64]ttcsnapshot.MemoryPageLocation {
	t.Helper()
	entries, err := graph.MemoryPageEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	locations := make(map[uint64]ttcsnapshot.MemoryPageLocation, len(entries))
	for _, entry := range entries {
		locations[entry.Index] = entry.Location
	}
	return locations
}

// storedChunk binds every page of chunk at the page index of its slot.
func storedChunk(chunk Chunk, object ID) []StoredBinding {
	stored := make([]StoredBinding, 0, len(chunk.Pages))
	for _, page := range chunk.Pages {
		stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
			OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 1,
			Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
			Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, object))
	}
	return stored
}

// filteredSentry plays the Sentry's part of a filtered capture of chunk over
// conn, and returns the daemon's answer to its manifest.
func filteredSentry(conn net.Conn, chunk Chunk, object ID) <-chan [slotBitmapBytes]byte {
	answered := make(chan [slotBitmapBytes]byte, 1)
	go func() {
		defer conn.Close()
		var answer [slotBitmapBytes]byte
		defer func() { answered <- answer }()
		if err := writeStoredCaptureHeader(conn, graphCaptureFilteredMagic, storedChunk(chunk, object), 1); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, answer[:]); err != nil {
			return
		}
		sent := Chunk{Logical: chunk.Logical}
		for _, page := range chunk.Pages {
			if hasBit(&answer, page.Slot) {
				sent.Pages = append(sent.Pages, page)
			}
		}
		if len(sent.Pages) == 0 {
			return
		}
		encoded, err := Encode(sent)
		if err != nil {
			return
		}
		_ = writeGraphCaptureObject(conn, object, encoded)
	}()
	return answered
}

// A filtered capture sends only the pages the graph lacks: all of them the
// first time, and only the rewritten one when a later capture shares the rest.
// A capture writes each object as the chunk's encoding, the pages read from
// the snapshot: those that are not zero follow the chunk's header, in slot
// order, as Encode makes it.
func TestGraphCaptureWritesTheChunkEncoding(t *testing.T) {
	file, err := os.CreateTemp("", "ttc-capture-memory-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer mf.Destroy()
	// A page, a zero page, two pages, and in another chunk one more.
	contents := [][]byte{
		bytes.Repeat([]byte{1}, hostarch.PageSize),
		make([]byte, hostarch.PageSize),
		bytes.Repeat([]byte{3}, hostarch.PageSize),
		bytes.Repeat([]byte{4}, hostarch.PageSize),
		bytes.Repeat([]byte{5}, hostarch.PageSize),
	}
	fr, err := mf.Allocate(uint64(len(contents))*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.DecRef(fr)
	for i, page := range contents {
		if _, err := file.WriteAt(page, int64(fr.Start)+int64(i)*hostarch.PageSize); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	chunks := []pgalloc.LogicalChunkID{{Allocator: testAllocator(1), Sequence: 7}, {Allocator: testAllocator(1), Sequence: 8}}
	slots := []uint8{0, 1, 2, 9, 3}
	want := map[pgalloc.LogicalChunkID]*Chunk{}
	var bindings []pgalloc.CapturedPageBinding
	for i := range contents {
		chunk := chunks[i/4]
		version := pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: uint64(i + 1)}
		page := pgalloc.LogicalPageID{Allocator: chunk.Allocator, Sequence: chunk.Sequence*PagesPerChunk + uint64(slots[i])}
		bindings = append(bindings, pgalloc.CapturedPageBinding{Page: page, Chunk: chunk, Slot: slots[i], Version: version, StreamOffset: uint64(i) * hostarch.PageSize, References: 1})
		if want[chunk] == nil {
			want[chunk] = &Chunk{Logical: chunk}
		}
		encoded := Page{Slot: slots[i], Version: version, Data: contents[i]}
		if i == 1 {
			encoded = Page{Slot: slots[i], Version: version, Zero: true}
		}
		want[chunk].Pages = append(want[chunk].Pages, encoded)
	}
	var stream bytes.Buffer
	if err := WriteGraphCapture(&stream, snapshot, bindings); err != nil {
		t.Fatal(err)
	}
	groups, manifest, err := planGraphCapture(bindings)
	if err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	if err := writeGraphCaptureHeader(&expected, graphCaptureMagic, manifest, len(groups)); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		encoded, err := Encode(*want[group.logical])
		if err != nil {
			t.Fatal(err)
		}
		if err := writeGraphCaptureObject(&expected, group.id, encoded); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(stream.Bytes(), expected.Bytes()) {
		t.Fatalf("the capture wrote %d bytes that differ from the %d its chunks encode to", stream.Len(), expected.Len())
	}
}

func TestFilteredGraphCaptureSendsOnlyWhatTheGraphLacks(t *testing.T) {
	first := testChunk()
	second := testChunk()
	second.Pages[2] = Page{Slot: 255, Version: pgalloc.PageVersionID{Allocator: testAllocator(3), Sequence: 1000}, Data: bytes.Repeat([]byte{0x77}, hostarch.PageSize)}
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
	object := graphCaptureObjectID(1)
	var pages []map[uint64]ttcsnapshot.MemoryPageLocation
	for i, capture := range []Chunk{first, second} {
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		daemon, sentry := net.Pipe()
		answered := filteredSentry(sentry, capture, object)
		result, err := ImportGraphCapture(daemon, graph, data, origin, fmt.Sprintf("filtered/%d", i))
		daemon.Close()
		answer := <-answered
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		var want [slotBitmapBytes]byte
		for _, page := range capture.Pages {
			if i == 0 || page.Slot == 255 {
				setBit(&want, page.Slot)
			}
		}
		if answer != want {
			t.Fatalf("capture %d asked for slots %x, want %x", i, answer, want)
		}
		mapped := memoryLocations(t, graph, result.Roots[0].Root)
		if len(mapped) != 3 {
			t.Fatalf("capture %d maps %v, want three pages", i, mapped)
		}
		pages = append(pages, mapped)
	}
	for _, index := range []uint64{0, 17} {
		if pages[1][index] != pages[0][index] {
			t.Fatalf("page %d was stored again: %v, first %v", index, pages[1][index], pages[0][index])
		}
	}
	if pages[1][255] == pages[0][255] {
		t.Fatal("the rewritten page shares the first capture's")
	}
}

// A capture shares the pages of a shared image that its graph adopted, from
// another node's graph, only while the caller names the image's origin.
func TestFilteredGraphCaptureSharesAnAdoptedImage(t *testing.T) {
	first := testChunk()
	second := testChunk()
	second.Pages[2] = Page{Slot: 255, Version: pgalloc.PageVersionID{Allocator: testAllocator(3), Sequence: 1000}, Data: bytes.Repeat([]byte{0x77}, hostarch.PageSize)}
	object := graphCaptureObjectID(1)
	open := func(name string) (*ttcsnapshot.Store, *ttcsnapshot.LocalDataStore) {
		directory := filepath.Join(t.TempDir(), name)
		graph, err := ttcsnapshot.Open(filepath.Join(directory, "graph"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { graph.Close() })
		data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(directory, "objects"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { data.Close() })
		return graph, data
	}
	capture := func(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, prefix string, chunk Chunk, reuse ttcsnapshot.MemoryReuse) (GraphImportResult, [slotBitmapBytes]byte) {
		daemon, sentry := net.Pipe()
		answered := filteredSentry(sentry, chunk, object)
		result, err := ImportGraphCaptureReusing(daemon, graph, data, origin, prefix, reuse)
		daemon.Close()
		answer := <-answered
		if err != nil {
			t.Fatalf("capture into %s: %v", prefix, err)
		}
		return result, answer
	}
	// The image, as another node published it.
	provider, providerData := open("provider")
	if err := provider.AdoptWorking(100); err != nil {
		t.Fatal(err)
	}
	image, _ := capture(provider, providerData, 100, "image", first, ttcsnapshot.MemoryReuse{})
	if err := provider.Seal(100, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	objects, err := provider.SnapshotObjects(100)
	if err != nil {
		t.Fatal(err)
	}
	var tables, chunks []ttcsnapshot.Object
	for _, object := range objects {
		switch object.ID.Kind {
		case ttcsnapshot.ObjectKindMemoryTable:
			tables = append(tables, object)
		case ttcsnapshot.ObjectKindMemoryChunk:
			chunks = append(chunks, object)
		}
	}
	shared := memoryLocations(t, provider, image.Roots[0].Root)
	graph, data := open("consumer")
	pins, err := graph.AdoptSharedMemory("capture/adopted/0/held", tables, chunks)
	if err != nil {
		t.Fatal(err)
	}
	volatile := ttcsnapshot.MemoryReuse{States: []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotSealedVolatile}}
	for i, test := range []struct {
		reuse ttcsnapshot.MemoryReuse
		asks  []uint8
	}{
		{reuse: volatile, asks: []uint8{0, 17, 255}},
		{reuse: ttcsnapshot.MemoryReuse{States: volatile.States, Origins: []ttcsnapshot.SnapshotID{100}}, asks: []uint8{255}},
	} {
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		result, answer := capture(graph, data, origin, fmt.Sprintf("capture/%d", i), second, test.reuse)
		var want [slotBitmapBytes]byte
		for _, slot := range test.asks {
			setBit(&want, slot)
		}
		if answer != want {
			t.Fatalf("capture %d asked for slots %x, want %x", i, answer, want)
		}
		mapped := memoryLocations(t, graph, result.Roots[0].Root)
		for _, index := range []uint64{0, 17} {
			if reused := mapped[index] == shared[index]; reused != (len(test.asks) == 1) {
				t.Fatalf("capture %d maps page %d at %v, the image at %v", i, index, mapped[index], shared[index])
			}
		}
	}
	if err := graph.DeletePins(pins); err != nil {
		t.Fatal(err)
	}
	if _, edges := graph.OriginReferences(100); !edges {
		t.Fatal("the capture's references of the image are not reported")
	}
}

func TestFilteredGraphCaptureNeedsATwoWayStream(t *testing.T) {
	chunk := testChunk()
	var stream bytes.Buffer
	if err := writeStoredCaptureHeader(&stream, graphCaptureFilteredMagic, storedChunk(chunk, graphCaptureObjectID(1)), 1); err != nil {
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
	if _, err := ImportGraphCapture(bytes.NewReader(stream.Bytes()), graph, data, origin, "one-way/memory"); err == nil {
		t.Fatal("a filtered capture imported from a one-way stream")
	}
}

// A capture that sends every page, from a Sentry predating filtering, still
// imports when the graph holds all of its pages: its objects are read past.
func TestUnfilteredGraphCaptureOfHeldPagesIsReadPast(t *testing.T) {
	chunk := testChunk()
	object := graphCaptureObjectID(1)
	encoded, err := Encode(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := writeStoredCaptureHeader(&stream, graphCaptureMagic, storedChunk(chunk, object), 1); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphCaptureObject(&stream, object, encoded); err != nil {
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
	for i := range 2 {
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ImportGraphCapture(bytes.NewReader(stream.Bytes()), graph, data, origin, fmt.Sprintf("full/%d", i)); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	trailing := append(append([]byte(nil), stream.Bytes()...), 1)
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportGraphCapture(bytes.NewReader(trailing), graph, data, origin, "full/trailing"); err == nil {
		t.Fatal("a capture with trailing data imported")
	}
	if graph.HasRootUnder("full/trailing") {
		t.Fatal("a failed import left its roots")
	}
}

func TestGraphImportReusesStableMemoryPages(t *testing.T) {
	chunk := testChunk()
	object := graphCaptureObjectID(1)
	stored := make([]StoredBinding, 0, len(chunk.Pages))
	for _, page := range chunk.Pages {
		logicalPage := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)}
		stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
			OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 2,
			Page: logicalPage, Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, object))
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
	firstPages := memoryLocations(t, graph, first.Roots[0].Root)
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
	secondPages := memoryLocations(t, graph, second.Roots[0].Root)
	if !reflect.DeepEqual(secondPages, firstPages) {
		t.Fatalf("second capture pages=%v, want them shared with %v", secondPages, firstPages)
	}
	if second.Roots[0].Root == first.Roots[0].Root {
		t.Fatal("two captures share a root")
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

// A capture's new chunk object holds only the pages the graph did not have,
// packed from its start in slot order.
func TestGraphImportStoresOnlyNewPages(t *testing.T) {
	first := testChunk()
	second := testChunk()
	second.Pages[2] = Page{Slot: 255, Version: pgalloc.PageVersionID{Allocator: testAllocator(3), Sequence: 1000}, Data: bytes.Repeat([]byte{0x77}, hostarch.PageSize)}
	manifestFor := func(chunk Chunk) Manifest {
		object := graphCaptureObjectID(1)
		stored := make([]StoredBinding, 0, len(chunk.Pages))
		for _, page := range chunk.Pages {
			logicalPage := pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)}
			stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
				OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 1,
				Page: logicalPage, Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
			}, object))
		}
		manifest, err := CompactBindings(stored)
		if err != nil {
			t.Fatal(err)
		}
		return manifest
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
	chunks := func() map[ttcsnapshot.ObjectID]struct{} {
		ids := make(map[ttcsnapshot.ObjectID]struct{})
		for _, object := range graph.Inspect().Objects {
			if object.ID.Kind == ttcsnapshot.ObjectKindMemoryChunk {
				ids[object.ID] = struct{}{}
			}
		}
		return ids
	}
	for i, chunk := range []Chunk{first, second} {
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		before := chunks()
		if _, err := importSnapshotGraph(graph, data, origin, manifestFor(chunk), fmt.Sprintf("new/%d", i), func(ID) (Chunk, error) { return chunk, nil }); err != nil {
			t.Fatal(err)
		}
		var added []ttcsnapshot.ObjectID
		for id := range chunks() {
			if _, ok := before[id]; !ok {
				added = append(added, id)
			}
		}
		if len(added) != 1 {
			t.Fatalf("capture %d added chunk objects %v, want one", i, added)
		}
		file, err := data.Open(added[0], ttcsnapshot.MemoryChunkBytes)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := make([]byte, ttcsnapshot.MemoryChunkBytes)
		position := 0
		for _, page := range chunk.Pages {
			if i == 0 || page.Slot == 255 {
				copy(want[position*hostarch.PageSize:], page.Data)
				position++
			}
		}
		if !bytes.Equal(raw, want) {
			t.Fatalf("capture %d stored a chunk object with other pages than its new ones", i)
		}
	}
}

// The pages the graph lacks from several capture objects share new chunks,
// packed in object and slot order. The manifest a rehydration builds says
// where in its chunk each page is, which a version 2 manifest encodes.
func TestGraphImportPacksNewPagesOfSeveralObjects(t *testing.T) {
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
	type captured struct {
		id    ID
		chunk Chunk
	}
	objects := []captured{
		{graphCaptureObjectID(1), Chunk{Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: 5}, Pages: []Page{
			{Slot: 9, Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: 1}, Data: bytes.Repeat([]byte{0x11}, hostarch.PageSize)},
		}}},
		{graphCaptureObjectID(2), Chunk{Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: 6}, Pages: []Page{
			{Slot: 200, Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: 2}, Data: bytes.Repeat([]byte{0x22}, hostarch.PageSize)},
		}}},
	}
	var stored []StoredBinding
	byID := make(map[ID]Chunk)
	for _, object := range objects {
		byID[object.id] = object.chunk
		for _, page := range object.chunk.Pages {
			index := object.chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)
			stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
				OldOffset: index * hostarch.PageSize, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: object.chunk.Logical.Allocator, Sequence: index},
				Version: page.Version, Chunk: object.chunk.Logical, Slot: page.Slot,
			}, object.id))
		}
	}
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	result, err := importSnapshotGraph(graph, data, origin, manifest, "packed", func(id ID) (Chunk, error) { return byID[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	locations := memoryLocations(t, graph, result.Roots[0].Root)
	first, second := locations[5*PagesPerChunk+9], locations[6*PagesPerChunk+200]
	if first.Chunk != second.Chunk || first.Page != 0 || second.Page != 1 {
		t.Fatalf("new pages at %+v and %+v, want pages 0 and 1 of one chunk", first, second)
	}
	file, err := data.Open(first.Chunk, ttcsnapshot.MemoryChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, ttcsnapshot.MemoryChunkBytes)
	copy(want, objects[0].chunk.Pages[0].Data)
	copy(want[hostarch.PageSize:], objects[1].chunk.Pages[0].Data)
	if !bytes.Equal(raw, want) {
		t.Fatal("the packed chunk does not hold the two pages at its first two pages")
	}

	built, err := BuildGraphManifest(graph, result.Roots)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range built.Manifest.Runs {
		if want := map[uint8]uint8{9: 0, 200: 1}[run.FirstSlot]; run.Pages != 1 || run.ObjectPage != want {
			t.Fatalf("rehydration manifest run %+v, want slot 9 at page 0 and slot 200 at page 1", run)
		}
	}
	encoded, err := EncodeManifest(built.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if version := binary.LittleEndian.Uint32(encoded[len(manifestMagic):]); version != manifestVersion {
		t.Fatalf("a packed manifest encodes as version %d, want %d", version, manifestVersion)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, built.Manifest) {
		t.Fatalf("decoded %+v, encoded %+v", decoded, built.Manifest)
	}
}

// compactedGraphManifest is the manifest CompactBindings makes of the pages
// roots map, each bound to where the graph holds it.
func compactedGraphManifest(t *testing.T, graph *ttcsnapshot.Store, roots []GraphMemoryRoot) Manifest {
	t.Helper()
	var stored []StoredBinding
	for _, root := range roots {
		entries, err := graph.MemoryPageEntries(root.Root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			page := pgalloc.LogicalPageID{Allocator: pgalloc.LogicalAllocatorID(entry.Identity.LogicalAllocator), Sequence: entry.Identity.LogicalSequence}
			chunk, slot := page.Chunk()
			stored = append(stored, StoredBinding{
				Binding: pgalloc.CapturedPageBinding{
					ResourceID: root.ResourceID, OldOffset: entry.Index * hostarch.PageSize, References: entry.Identity.References, Page: page,
					Version: pgalloc.PageVersionID{Allocator: pgalloc.LogicalAllocatorID(entry.Identity.VersionAllocator), Sequence: entry.Identity.VersionSequence},
					Chunk:   chunk, Slot: slot,
				},
				Object: graphChunkResidencyID(entry.Location.Chunk), ObjectPage: entry.Location.Page,
			})
		}
	}
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// A rehydration's manifest has the runs CompactBindings makes of the pages its
// roots map, whatever the roots' order: a run ends where a page's resource,
// references, chunk, logical chunk, offset, identities or place in its chunk
// do not follow the page before.
func TestBuildGraphManifestCompactsPages(t *testing.T) {
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
	var stored []StoredBinding
	byID := make(map[ID]Chunk)
	// capture adds an object holding the pages of logical chunk sequence at
	// slots [first, end), mapped from page index base + slot.
	capture := func(resource checkpoint.ResourceID, sequence, base uint64, first, end int, references, version func(int) uint64) {
		id := graphCaptureObjectID(uint64(len(byID) + 1))
		chunk := Chunk{Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: sequence}}
		for slot := first; slot < end; slot++ {
			pageVersion := pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: version(slot)}
			chunk.Pages = append(chunk.Pages, Page{Slot: uint8(slot), Version: pageVersion, Data: bytes.Repeat([]byte{byte(slot) + 1}, hostarch.PageSize)})
			stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
				ResourceID: resource, OldOffset: (base + uint64(slot)) * hostarch.PageSize, References: references(slot),
				Page:    pgalloc.LogicalPageID{Allocator: testAllocator(1), Sequence: sequence*PagesPerChunk + uint64(slot)},
				Version: pageVersion, Chunk: chunk.Logical, Slot: uint8(slot),
			}, id))
		}
		byID[id] = chunk
	}
	one := func(int) uint64 { return 1 }
	consecutive := func(slot int) uint64 { return 1 + uint64(slot) }
	capture(checkpoint.ResourceID{}, 5, 1000, 0, 120, func(slot int) uint64 { return 1 + uint64(slot/60) }, func(slot int) uint64 {
		if slot >= 100 {
			return 500 + uint64(slot)
		}
		return 1 + uint64(slot)
	})
	capture(checkpoint.ResourceID{}, 6, 1256, 0, 10, one, consecutive)
	// Only their logical chunks part these two.
	capture(checkpoint.ResourceID{}, 9, 4000, 250, 256, one, consecutive)
	capture(checkpoint.ResourceID{}, 10, 4256, 0, 4, one, func(slot int) uint64 { return 257 + uint64(slot) })
	capture(checkpoint.ResourceID{Path: "/tmpfs"}, 7, 0, 3, 41, one, consecutive)
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	result, err := importSnapshotGraph(graph, data, origin, manifest, "compacted", func(id ID) (Chunk, error) { return byID[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Roots) != 2 {
		t.Fatalf("imported roots %+v, want two", result.Roots)
	}
	want := compactedGraphManifest(t, graph, result.Roots)
	if len(want.Runs) < 6 {
		t.Fatalf("the pages compact to %d runs, too few to tell", len(want.Runs))
	}
	for _, roots := range [][]GraphMemoryRoot{result.Roots, {result.Roots[1], result.Roots[0]}} {
		built, err := BuildGraphManifest(graph, roots)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(built.Manifest, want) {
			t.Fatalf("built manifest %+v, want %+v", built.Manifest, want)
		}
		for _, run := range built.Manifest.Runs {
			if object, ok := built.Chunks[run.Object]; !ok || graphChunkResidencyID(object) != run.Object {
				t.Fatalf("run %+v has chunk (%v, %t)", run, object, ok)
			}
		}
	}
	if _, err := BuildGraphManifest(graph, append(slices.Clone(result.Roots), result.Roots[0])); err == nil {
		t.Fatal("two roots of one resource were enumerated")
	}
}

// An object's new pages share one chunk, as rehydration loads memory a logical
// chunk at a time: an object that does not fit where the chunk has room
// starts the next one.
func TestGraphImportKeepsAnObjectsNewPagesInOneChunk(t *testing.T) {
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
	var stored []StoredBinding
	byID := make(map[ID]Chunk)
	for object := uint64(0); object < 3; object++ {
		id := graphCaptureObjectID(object + 1)
		chunk := Chunk{Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: 10 + object}}
		for slot := 0; slot < 100; slot++ {
			chunk.Pages = append(chunk.Pages, Page{Slot: uint8(slot), Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: object*1000 + uint64(slot)}, Data: bytes.Repeat([]byte{byte(object + 1)}, hostarch.PageSize)})
			index := chunk.Logical.Sequence*PagesPerChunk + uint64(slot)
			stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
				OldOffset: index * hostarch.PageSize, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: index},
				Version: chunk.Pages[slot].Version, Chunk: chunk.Logical, Slot: uint8(slot),
			}, id))
		}
		byID[id] = chunk
	}
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	result, err := importSnapshotGraph(graph, data, origin, manifest, "aligned", func(id ID) (Chunk, error) { return byID[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	locations := memoryLocations(t, graph, result.Roots[0].Root)
	first := func(object uint64) ttcsnapshot.MemoryPageLocation { return locations[(10+object)*PagesPerChunk] }
	last := func(object uint64) ttcsnapshot.MemoryPageLocation { return locations[(10+object)*PagesPerChunk+99] }
	for object := uint64(0); object < 3; object++ {
		if first(object).Chunk != last(object).Chunk {
			t.Fatalf("object %d's pages span chunks: %+v to %+v", object, first(object), last(object))
		}
	}
	if first(1).Chunk != first(0).Chunk || first(0).Page != 0 || first(1).Page != 100 {
		t.Fatalf("the first two objects at %+v and %+v, want pages 0 and 100 of one chunk", first(0), first(1))
	}
	if first(2).Chunk == first(0).Chunk || first(2).Page != 0 {
		t.Fatalf("the third object at %+v, want page 0 of a chunk of its own", first(2))
	}
}

// A chunk holds the pages of one resource, as a rehydration installs it in
// that resource's MemoryFile: the pages of the next resource start a chunk of
// their own, though they would fit where the last one has room.
func TestGraphImportKeepsResourcesInChunksOfTheirOwn(t *testing.T) {
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
	var stored []StoredBinding
	byID := make(map[ID]Chunk)
	resources := []checkpoint.ResourceID{{}, {Path: "/tmpfs"}}
	for object := uint64(0); object < 2; object++ {
		id := graphCaptureObjectID(object + 1)
		chunk := Chunk{Logical: pgalloc.LogicalChunkID{Allocator: testAllocator(1), Sequence: 10 + object}}
		for slot := 0; slot < 10; slot++ {
			chunk.Pages = append(chunk.Pages, Page{Slot: uint8(slot), Version: pgalloc.PageVersionID{Allocator: testAllocator(2), Sequence: object*1000 + uint64(slot)}, Data: bytes.Repeat([]byte{byte(object + 1)}, hostarch.PageSize)})
			index := chunk.Logical.Sequence*PagesPerChunk + uint64(slot)
			stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
				ResourceID: resources[object], OldOffset: index * hostarch.PageSize, References: 1,
				Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: index},
				Version: chunk.Pages[slot].Version, Chunk: chunk.Logical, Slot: uint8(slot),
			}, id))
		}
		byID[id] = chunk
	}
	manifest, err := CompactBindings(stored)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	result, err := importSnapshotGraph(graph, data, origin, manifest, "apart", func(id ID) (Chunk, error) { return byID[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Roots) != 2 {
		t.Fatalf("imported roots %+v, want two", result.Roots)
	}
	chunks := make(map[ttcsnapshot.ObjectID]checkpoint.ResourceID)
	for _, root := range result.Roots {
		for _, location := range memoryLocations(t, graph, root.Root) {
			if other, ok := chunks[location.Chunk]; ok && other != root.ResourceID {
				t.Fatalf("chunk %s holds pages of %s and %s", location.Chunk, other, root.ResourceID)
			}
			chunks[location.Chunk] = root.ResourceID
		}
	}
	if _, err := BuildGraphManifest(graph, result.Roots); err != nil {
		t.Fatalf("the image does not rehydrate: %v", err)
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
		stored = append(stored, capturedBinding(pgalloc.CapturedPageBinding{
			OldOffset: uint64(page.Slot) * hostarch.PageSize, References: 2,
			Page:    pgalloc.LogicalPageID{Allocator: chunk.Logical.Allocator, Sequence: chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)},
			Version: page.Version, Chunk: chunk.Logical, Slot: page.Slot,
		}, object))
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
	firstPages := memoryLocations(t, graph, first.Roots[0].Root)
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
	secondPages := memoryLocations(t, graph, second.Roots[0].Root)
	if reflect.DeepEqual(secondPages, firstPages) {
		t.Fatalf("cluster capture shared shared-cache pages %v", secondPages)
	}
	for index, page := range secondPages {
		if page.Chunk.Snapshot != secondOrigin {
			t.Fatalf("second capture page %d is in chunk %s of shared origin %d", index, page.Chunk, firstOrigin)
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
		stored[i] = capturedBinding(pgalloc.CapturedPageBinding{
			OldOffset:  uint64(i) * hostarch.PageSize,
			References: 2,
			Page:       pgalloc.LogicalPageID{Allocator: allocator, Sequence: logical.Sequence*PagesPerChunk + uint64(i)},
			Version:    pgalloc.PageVersionID{Allocator: versionAllocator, Sequence: 1000 + uint64(i)},
			Chunk:      logical,
			Slot:       uint8(i),
		}, object)
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

// A manifest whose runs are out of order still plans an object's pages in slot
// order and a resource's mappings in index order, maps a page index mapped
// twice alike once, and refuses one mapped to two pages.
func TestCaptureImportPlanOrdersPagesAndMappings(t *testing.T) {
	logical := pgalloc.LogicalChunkID{Allocator: testAllocator(11), Sequence: 9}
	object := graphCaptureObjectID(9)
	run := func(index uint64, slot uint8, pages uint16) ManifestRun {
		return ManifestRun{
			OldOffset:    index * hostarch.PageSize,
			References:   1,
			FirstPage:    pgalloc.LogicalPageID{Allocator: logical.Allocator, Sequence: logical.Sequence*PagesPerChunk + uint64(slot)},
			FirstVersion: pgalloc.PageVersionID{Allocator: testAllocator(22), Sequence: 1000 + uint64(slot)},
			Logical:      logical,
			FirstSlot:    slot,
			Pages:        pages,
			Object:       object,
			ObjectPage:   slot,
		}
	}
	manifest := Manifest{Runs: []ManifestRun{run(8, 200, 2), run(0, 3, 2), run(8, 200, 1)}}
	objects, resources, mappings, err := planCaptureImport(manifest, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || len(resources) != 1 {
		t.Fatalf("planned %d objects and %d resources, want one of each", len(objects), len(resources))
	}
	var slots []uint8
	for index := range objects[0].slots {
		slots = append(slots, objects[0].slots[index].number())
	}
	if want := []uint8{3, 4, 200, 201}; !reflect.DeepEqual(slots, want) {
		t.Fatalf("planned slots %v, want %v", slots, want)
	}
	var indexes []uint64
	for _, mapping := range mappings {
		indexes = append(indexes, mapping.index)
		if page := objects[0].slot(mapping.slot); page.number() != mapping.slot || page.identity.VersionSequence != 1000+uint64(mapping.slot) {
			t.Fatalf("page index %d maps slot %d to %+v", mapping.index, mapping.slot, page.identity)
		}
	}
	if want := []uint64{0, 1, 8, 9}; !reflect.DeepEqual(indexes, want) {
		t.Fatalf("planned page indexes %v, want %v", indexes, want)
	}
	manifest.Runs = append(manifest.Runs, run(8, 7, 1))
	if _, _, _, err := planCaptureImport(manifest, "plan"); err == nil {
		t.Fatal("a page index mapped to two pages was planned")
	}
}

// BenchmarkInstallManifestSet installs the manifest of 1.5 GiB of memory,
// 1,536 chunks of 256 pages, as a rehydration does.
func BenchmarkInstallManifestSet(b *testing.B) {
	const chunks = 1536
	allocator := testAllocator(31)
	manifest := Manifest{Runs: make([]ManifestRun, 0, chunks)}
	for chunk := uint64(0); chunk < chunks; chunk++ {
		logical := pgalloc.LogicalChunkID{Allocator: allocator, Sequence: chunk + 1}
		manifest.Runs = append(manifest.Runs, ManifestRun{
			OldOffset: chunk * PagesPerChunk * hostarch.PageSize, References: 1,
			FirstPage:    pgalloc.LogicalPageID{Allocator: allocator, Sequence: logical.Sequence * PagesPerChunk},
			FirstVersion: pgalloc.PageVersionID{Allocator: testAllocator(32), Sequence: 1 + chunk*PagesPerChunk},
			Logical:      logical, Pages: PagesPerChunk, Object: graphCaptureObjectID(chunk + 1),
		})
	}
	file, err := os.CreateTemp("", "ttc-install-")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		b.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction: pgalloc.DelayedEvictionDisabled, DisableIMAWorkAround: true, DisableMemoryAccounting: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mf.Destroy()
	files := map[checkpoint.ResourceID]*pgalloc.MemoryFile{{}: mf}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := InstallManifestSet(files, manifest, usage.Anonymous, 0)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := result.Abort(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

// sortedCaptureGroup is a graphCaptureGroup as planGraphCaptureBySorting
// makes it, holding its bindings.
type sortedCaptureGroup struct {
	id       ID
	logical  pgalloc.LogicalChunkID
	bindings []pgalloc.CapturedPageBinding
	used     [32]byte
}

// writeStoredCaptureHeader writes the header of a capture of stored.
func writeStoredCaptureHeader(w io.Writer, magic [16]byte, stored []StoredBinding, objectCount int) error {
	manifest, err := CompactBindings(stored)
	if err != nil {
		return err
	}
	return writeGraphCaptureHeader(w, magic, manifest, objectCount)
}

// planGraphCaptureBySorting is planGraphCapture as it was, sorting the
// bindings' indexes by comparing the bindings: the plan it must keep.
func planGraphCaptureBySorting(bindings []pgalloc.CapturedPageBinding) ([]sortedCaptureGroup, []StoredBinding, error) {
	// Visit the bindings in logical chunk and slot order through their
	// indexes: sorting the bindings themselves moved every 100-byte record
	// many times.
	order := make([]int32, len(bindings))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(a, b int32) int {
		left, right := &bindings[a], &bindings[b]
		if c := compareAllocator(left.Chunk.Allocator, right.Chunk.Allocator); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Chunk.Sequence, right.Chunk.Sequence); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Slot, right.Slot); c != 0 {
			return c
		}
		if c := compareAllocator(left.Version.Allocator, right.Version.Allocator); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Version.Sequence, right.Version.Sequence); c != 0 {
			return c
		}
		return cmp.Compare(left.OldOffset, right.OldOffset)
	})
	groups := make([]sortedCaptureGroup, 0)
	stored := make([]StoredBinding, len(bindings))
	// The order keeps a logical chunk's bindings together, and a page's of
	// one version next to each other: chunkGroups are the groups of the
	// chunk being visited, and previous the binding before.
	var chunkGroups []int
	var previous *pgalloc.CapturedPageBinding
	for _, index := range order {
		binding := &bindings[index]
		chunk, slot := binding.Page.Chunk()
		if binding.Chunk != chunk || binding.Slot != slot || binding.References == 0 || binding.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, nil, fmt.Errorf("invalid captured page binding: %+v", *binding)
		}
		if previous == nil || previous.Chunk != binding.Chunk {
			chunkGroups = chunkGroups[:0]
		} else if previous.Page == binding.Page && previous.Version == binding.Version {
			return nil, nil, fmt.Errorf("captured page %+v repeats version %+v", binding.Page, binding.Version)
		}
		previous = binding
		groupIndex := -1
		for _, candidate := range chunkGroups {
			if !hasBit(&groups[candidate].used, binding.Slot) {
				groupIndex = candidate
				break
			}
		}
		if groupIndex < 0 {
			groupIndex = len(groups)
			id := graphCaptureObjectID(uint64(groupIndex + 1))
			groups = append(groups, sortedCaptureGroup{id: id, logical: binding.Chunk})
			chunkGroups = append(chunkGroups, groupIndex)
		}
		group := &groups[groupIndex]
		setBit(&group.used, binding.Slot)
		group.bindings = append(group.bindings, *binding)
		stored[index] = capturedBinding(*binding, group.id)
	}
	return groups, stored, nil
}

// captureBindings returns bindings of pages of chunks logical chunks, of
// allocators allocators, visited a chunk at a time in a random order, as a
// rehydrated branch's resident units are. Unless full, a quarter of the slots
// are empty, and some pages are bound in two versions.
func captureBindings(chunks, allocators int, full bool, seed int64) []pgalloc.CapturedPageBinding {
	random := rand.New(rand.NewSource(seed))
	var bindings []pgalloc.CapturedPageBinding
	offset := uint64(0)
	for _, chunk := range random.Perm(chunks) {
		logical := pgalloc.LogicalChunkID{Allocator: testAllocator(byte(1 + 32*(chunk%allocators))), Sequence: uint64(chunk/allocators + 1)}
		for slot := 0; slot < PagesPerChunk; slot++ {
			versions := 1
			if !full {
				if random.Intn(4) == 0 {
					continue
				}
				if random.Intn(16) == 0 {
					versions = 2
				}
			}
			for version := 0; version < versions; version++ {
				bindings = append(bindings, pgalloc.CapturedPageBinding{
					OldOffset: offset, StreamOffset: offset, References: 1,
					Page:    pgalloc.LogicalPageID{Allocator: logical.Allocator, Sequence: logical.Sequence*PagesPerChunk + uint64(slot)},
					Version: pgalloc.PageVersionID{Allocator: testAllocator(200), Sequence: uint64(chunk*2*PagesPerChunk + version*PagesPerChunk + slot)},
					Chunk:   logical, Slot: uint8(slot),
				})
				offset += hostarch.PageSize
			}
		}
	}
	return bindings
}

// A capture's plan groups its bindings, and names its objects, in logical
// chunk, slot and version order, whatever order the bindings come in.
func TestPlanGraphCaptureKeepsItsOrder(t *testing.T) {
	for seed := int64(0); seed < 6; seed++ {
		bindings := captureBindings(40, 1+int(seed)%3, false, seed)
		if seed%2 == 1 {
			rand.New(rand.NewSource(seed)).Shuffle(len(bindings), func(i, j int) { bindings[i], bindings[j] = bindings[j], bindings[i] })
		}
		wantGroups, wantStored, err := planGraphCaptureBySorting(bindings)
		if err != nil {
			t.Fatal(err)
		}
		wantManifest, err := CompactBindings(wantStored)
		if err != nil {
			t.Fatal(err)
		}
		groups, manifest, err := planGraphCapture(bindings)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != len(wantGroups) {
			t.Fatalf("seed %d: %d groups, want %d", seed, len(groups), len(wantGroups))
		}
		for i := range groups {
			group, want := &groups[i], &wantGroups[i]
			if group.id != want.id || group.logical != want.logical || group.used != want.used || len(group.bindings) != len(want.bindings) {
				t.Fatalf("seed %d: group %d differs from the one sorting the bindings makes", seed, i)
			}
			for j, index := range group.bindings {
				if bindings[index] != want.bindings[j] {
					t.Fatalf("seed %d: group %d binding %d is %+v, want %+v", seed, i, j, bindings[index], want.bindings[j])
				}
			}
		}
		if !reflect.DeepEqual(manifest, wantManifest) {
			t.Fatalf("seed %d: the manifest differs from the one sorting the bindings makes", seed)
		}
	}
}

// BenchmarkPlanGraphCapture plans the capture of 1.5 GiB, 1,550 logical
// chunks of 256 pages.
func BenchmarkPlanGraphCapture(b *testing.B) {
	bindings := captureBindings(1550, 1, true, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := planGraphCapture(bindings); err != nil {
			b.Fatal(err)
		}
	}
}

// writeGraphCaptureObject writes an object of a capture stream, its data
// encoded whole.
func writeGraphCaptureObject(w io.Writer, id ID, data []byte) error {
	if len(data) == 0 || len(data) > maxGraphCaptureObjectBytes {
		return fmt.Errorf("graph memory object %s has invalid size %d", id, len(data))
	}
	if err := writeGraphCaptureBytes(w, id[:]); err != nil {
		return err
	}
	if err := writeGraphCaptureUint64(w, uint64(len(data))); err != nil {
		return err
	}
	return writeGraphCaptureBytes(w, data)
}
