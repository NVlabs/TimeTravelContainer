// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// newHugeMemoryFile returns a MemoryFile, backed by a file, whose huge
// allocations may be 2 MiB pages.
func newHugeMemoryFile(t *testing.T) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-chunk-huge-")
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
		ExpectHugepages:         true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(mf.Destroy)
	return mf
}

// hugePageContents is what page k of the test's 2 MiB page holds: k+1 in
// each byte, but for page 300, zero.
func hugePageContents() []byte {
	data := make([]byte, hostarch.HugePageSize)
	for k := 0; k < 2*PagesPerChunk; k++ {
		if k != 300 {
			for i := 0; i < hostarch.PageSize; i++ {
				data[k*hostarch.PageSize+i] = byte(k + 1)
			}
		}
	}
	return data
}

// captureHugePage captures fr of mf for dehydration, and returns the capture
// stream: of one 2 MiB page if huge, and of base pages otherwise.
func captureHugePage(t *testing.T, mf *pgalloc.MemoryFile, fr memmap.FileRange, huge bool) []byte {
	t.Helper()
	capture := pgalloc.NewMemoryRangeCapture()
	if err := capture.Record(mf, fr); err != nil {
		t.Fatal(err)
	}
	layout, err := capture.HugePageSnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}
	if huge && (len(layout.Bindings) != 1 || !layout.Bindings[0].Huge) {
		t.Fatalf("the capture binds %+v, want one 2 MiB page", layout.Bindings)
	}
	if !huge && (len(layout.Bindings) != 2*PagesPerChunk || slices.ContainsFunc(layout.Bindings, func(binding pgalloc.CapturedPageBinding) bool { return binding.Huge })) {
		t.Fatalf("the capture binds %d pages, some of them 2 MiB pages, want %d base pages", len(layout.Bindings), 2*PagesPerChunk)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(layout.Regions)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var stream bytes.Buffer
	if err := WriteGraphCapture(&stream, snapshot, layout.Bindings); err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// A dehydration stores a 2 MiB page whole: its halves, each in a chunk of its
// own, mapped by tables that record the page's identity once. A rehydration's
// manifest names its halves as such, and installs them as one 2 MiB page, read
// from the halves one after the other. A second capture of the page shares
// it, and one of its base pages, once it is split, the pages of its halves.
func TestGraphCaptureStoresA2MiBPageWhole(t *testing.T) {
	source := newHugeMemoryFile(t)
	fr, err := source.Allocate(hostarch.HugePageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate, Huge: true, LogicalHugePages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer source.DecRef(fr)
	want := hugePageContents()
	dst, err := source.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(dst, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want))); err != nil || n != uint64(len(want)) {
		t.Fatalf("filling the 2 MiB page: %d bytes, %v", n, err)
	}
	runs := source.LogicalPageRanges(fr)
	if len(runs) != 1 || !runs[0].Huge {
		t.Fatalf("source runs %+v, want one 2 MiB page", runs)
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
	result, err := ImportGraphCapture(bytes.NewReader(captureHugePage(t, source, fr, true)), graph, data, origin, "first/memory")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Roots) != 1 {
		t.Fatalf("import %+v, want one root", result)
	}
	entries, err := graph.MemoryPageEntries(result.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2*PagesPerChunk {
		t.Fatalf("the image maps %d pages, want %d", len(entries), 2*PagesPerChunk)
	}
	var halves [2]ttcsnapshot.ObjectID
	for k, entry := range entries {
		half := k / PagesPerChunk
		if k%PagesPerChunk == 0 {
			halves[half] = entry.Location.Chunk
		}
		if !entry.Huge || !entry.Location.Huge || entry.Location.Chunk != halves[half] || int(entry.Location.Page) != k%PagesPerChunk ||
			entry.Identity.LogicalSequence != runs[0].First.Sequence+uint64(k) || entry.Identity.VersionSequence != runs[0].FirstVersion.Sequence+uint64(k) {
			t.Fatalf("page %d of the image is %+v", k, entry)
		}
	}
	if halves[0] == halves[1] {
		t.Fatal("the halves of the 2 MiB page share a chunk")
	}

	// Restore installs it whole.
	built, err := BuildGraphManifest(graph, result.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Manifest.Runs) != 2 || !built.Manifest.Runs[0].Huge || !built.Manifest.Runs[1].Huge {
		t.Fatalf("the image's manifest runs %+v, want the two halves of a 2 MiB page", built.Manifest.Runs)
	}
	encoded, err := EncodeManifest(built.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeManifest(encoded); err != nil || len(decoded.Runs) != 2 || !decoded.Runs[0].Huge || !decoded.Runs[1].Huge {
		t.Fatalf("the manifest decodes as %+v (%v)", decoded, err)
	}
	target := newHugeMemoryFile(t)
	installation, err := InstallManifestSet(map[checkpoint.ResourceID]*pgalloc.MemoryFile{{}: target}, built.Manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer installation.Abort()
	relocated, err := installation.Relocation.Range(checkpoint.ResourceID{}, fr)
	if err != nil {
		t.Fatal(err)
	}
	if !target.IsHugePages(relocated) {
		t.Fatalf("the 2 MiB page is installed at %v, not as one", relocated)
	}
	if got := target.LogicalPageRanges(relocated); len(got) != 1 || got[0].First != runs[0].First || got[0].FirstVersion != runs[0].FirstVersion {
		t.Fatalf("the installed page has runs %+v, want %+v", got, runs)
	}
	if len(installation.Chunks) != 2 || installation.Chunks[0].Target.Start != relocated.Start || installation.Chunks[1].Target.End != relocated.End ||
		installation.Chunks[1].StreamOffset != installation.Chunks[0].StreamOffset+pgalloc.SparseChunkBytes {
		t.Fatalf("the installed page is read from %+v, want its halves one after the other", installation.Chunks)
	}
	reader, err := NewGraphChunkReader(data, built.Chunks, installation.Chunks, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := make([]byte, hostarch.HugePageSize)
	if n, err := reader.ReadAt(got, 0); (err != nil && err != io.EOF) || n != len(got) {
		t.Fatalf("reading the 2 MiB page: %d bytes, %v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the stored 2 MiB page differs from the captured one")
	}
	// Once nothing uses it, the page's unit is reclaimed, and reported by
	// the chunks of its halves, which an image may then let go of.
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	if reclaimed := target.ReclaimUnusedSparseChunks(); reclaimed != 1 {
		t.Fatalf("reclaimed %d units, want the 2 MiB page's", reclaimed)
	}
	reported := make(map[ttcsnapshot.ObjectID]bool)
	for _, object := range target.ReclaimedSparseChunks() {
		if chunk, err := GraphChunkObject(ID(object)); err == nil {
			reported[chunk] = true
		}
	}
	if len(reported) != 2 || !reported[halves[0]] || !reported[halves[1]] {
		t.Fatalf("the reclaimed 2 MiB page reports chunks %v, want its halves %v", reported, halves)
	}

	// A second capture of the same page shares both halves.
	second, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	before := graph.Stats().Objects
	again, err := ImportGraphCapture(bytes.NewReader(captureHugePage(t, source, fr, true)), graph, data, second, "second/memory")
	if err != nil {
		t.Fatal(err)
	}
	if created := graph.Stats().Objects - before; created > 6 {
		t.Fatalf("a second capture of the 2 MiB page created %d objects, want the nodes above its tables only", created)
	}
	againEntries, err := graph.MemoryPageEntries(again.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if againEntries[0].Location.Chunk != halves[0] || againEntries[PagesPerChunk].Location.Chunk != halves[1] {
		t.Fatal("a second capture of the 2 MiB page stored it again")
	}

	// Once part of it is freed, the page splits into its base pages, which
	// keep its identities: a capture of them shares the pages of its halves,
	// as base pages, which a rehydration installs so.
	source.SplitPartlyCoveredHugePages(memmap.FileRange{Start: fr.Start, End: fr.Start + hostarch.PageSize})
	third, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	before = graph.Stats().Objects
	split, err := ImportGraphCapture(bytes.NewReader(captureHugePage(t, source, fr, false)), graph, data, third, "third/memory")
	if err != nil {
		t.Fatalf("capturing the base pages of a split 2 MiB page: %v", err)
	}
	splitEntries, err := graph.MemoryPageEntries(split.Roots[0].Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(splitEntries) != 2*PagesPerChunk {
		t.Fatalf("the split page's image maps %d pages, want %d", len(splitEntries), 2*PagesPerChunk)
	}
	for k, entry := range splitEntries {
		if entry.Huge || entry.Location != (ttcsnapshot.MemoryPageLocation{Chunk: halves[k/PagesPerChunk], Page: uint8(k % PagesPerChunk)}) || entry.Identity != entries[k].Identity {
			t.Fatalf("page %d of the split page's image is %+v", k, entry)
		}
	}
	if created := graph.Stats().Objects - before; created > 2+6 {
		t.Fatalf("a capture of the split page created %d objects, want its tables and the nodes above them only", created)
	}
	builtSplit, err := BuildGraphManifest(graph, split.Roots)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range builtSplit.Manifest.Runs {
		if run.Huge {
			t.Fatalf("the split page's manifest has run %+v of a half of a 2 MiB page", run)
		}
	}
	splitTarget := newHugeMemoryFile(t)
	splitInstallation, err := InstallManifestSet(map[checkpoint.ResourceID]*pgalloc.MemoryFile{{}: splitTarget}, builtSplit.Manifest, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer splitInstallation.Abort()
	splitReader, err := NewGraphChunkReader(data, builtSplit.Chunks, splitInstallation.Chunks, false)
	if err != nil {
		t.Fatal(err)
	}
	defer splitReader.Close()
	if len(splitInstallation.Chunks) != 2 {
		t.Fatalf("the split page is read from %+v, want its halves", splitInstallation.Chunks)
	}
	for _, chunk := range splitInstallation.Chunks {
		half := slices.Index(halves[:], builtSplit.Chunks[chunk.Object])
		if half < 0 {
			t.Fatalf("the split page is read from chunk %s, none of its halves", builtSplit.Chunks[chunk.Object])
		}
		got := make([]byte, SparseHalfBytes)
		if n, err := splitReader.ReadAt(got, int64(chunk.StreamOffset)); (err != nil && err != io.EOF) || n != len(got) {
			t.Fatalf("reading half %d of the split page: %d bytes, %v", half, n, err)
		}
		if !bytes.Equal(got, want[half*SparseHalfBytes:(half+1)*SparseHalfBytes]) {
			t.Fatalf("half %d of the split page differs from the captured one", half)
		}
	}
}

// hugeManifestRuns returns the runs of the halves of a 2 MiB page at offset
// old, of identity page and version, in objects lower and upper.
func hugeManifestRuns(old uint64, page pgalloc.LogicalPageID, version pgalloc.PageVersionID, lower, upper ID) []ManifestRun {
	runs := make([]ManifestRun, 2)
	for half := range runs {
		first := page
		first.Sequence += uint64(half) * PagesPerChunk
		firstVersion := version
		firstVersion.Sequence += uint64(half) * PagesPerChunk
		logical, slot := first.Chunk()
		runs[half] = ManifestRun{
			OldOffset: old + uint64(half)*SparseHalfBytes, References: 1, FirstPage: first, FirstVersion: firstVersion,
			Logical: logical, FirstSlot: slot, Pages: PagesPerChunk, Object: lower, Huge: true,
		}
	}
	runs[1].Object = upper
	return runs
}

// A manifest with halves of 2 MiB pages is of version 3, which says which runs
// they are; it lists them together, or is refused. One without is of the
// version it was.
func TestManifestOf2MiBPages(t *testing.T) {
	allocator := testAllocator(7)
	page := pgalloc.LogicalPageID{Allocator: allocator, Sequence: 4 * 2 * PagesPerChunk}
	version := pgalloc.PageVersionID{Allocator: allocator, Sequence: 9 * 2 * PagesPerChunk}
	var lower, upper ID
	lower[0], upper[0] = 1, 2
	runs := hugeManifestRuns(6*hostarch.HugePageSize, page, version, lower, upper)
	encoded, err := EncodeManifest(Manifest{Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(encoded[len(manifestMagic):]); got != manifestVersion {
		t.Fatalf("a manifest of a 2 MiB page encodes as version %d, want %d", got, manifestVersion)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Runs, runs) {
		t.Fatalf("decoded runs %+v, want %+v", decoded.Runs, runs)
	}
	for name, bad := range map[string][]ManifestRun{
		"a lone lower half":    runs[:1],
		"a lone upper half":    runs[1:],
		"halves apart":         {runs[0], {OldOffset: 0, References: 1, FirstPage: pgalloc.LogicalPageID{Allocator: allocator, Sequence: 3}, FirstVersion: version, Logical: pgalloc.LogicalChunkID{Allocator: allocator}, FirstSlot: 3, Pages: 1, Object: lower}, runs[1]},
		"halves of one object": hugeManifestRuns(6*hostarch.HugePageSize, page, version, lower, lower),
		"an unaligned page":    hugeManifestRuns(6*hostarch.HugePageSize+SparseHalfBytes, page, version, lower, upper),
	} {
		if _, err := EncodeManifest(Manifest{Runs: bad}); err == nil {
			t.Errorf("%s was encoded", name)
		}
	}
	flags := len(encoded) - 1
	unknown := bytes.Clone(encoded)
	unknown[flags] |= 0x80
	if _, err := DecodeManifest(unknown); err == nil {
		t.Error("a run with unknown flags was decoded")
	}
	// Without 2 MiB pages, a manifest is of the version it was.
	plain := runs[0]
	plain.Huge, plain.Pages = false, 3
	if encoded, err := EncodeManifest(Manifest{Runs: []ManifestRun{plain}}); err != nil || binary.LittleEndian.Uint32(encoded[len(manifestMagic):]) != manifestVersionSlotted {
		t.Fatalf("a manifest of base pages encodes as %v, want version %d", err, manifestVersionSlotted)
	}
}
