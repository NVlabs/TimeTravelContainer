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

//go:build !pagesize_64k

package pgalloc

import (
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func sparseTestInstall(objectByte byte) SparseChunkInstall {
	var object SparseChunkObjectID
	object[0] = objectByte
	var allocator LogicalAllocatorID
	allocator[0] = 1
	return SparseChunkInstall{
		Object: object, Kind: usage.Anonymous,
		Pages: []SparseChunkPage{
			{Position: 3, Slot: 3, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 3}, Version: PageVersionID{Allocator: allocator, Sequence: 30}, References: 2},
			{Position: 4, Slot: 4, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 4}, Version: PageVersionID{Allocator: allocator, Sequence: 31}, References: 1},
		},
	}
}

func TestInstallSparseChunksReusesResidentObject(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	install := sparseTestInstall(1)
	firstInstall, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstInstall.Commit(); err != nil {
		t.Fatal(err)
	}
	first := firstInstall.NewRanges()[install.Object]
	if first.Length() != SparseChunkBytes {
		t.Fatalf("resident chunk range is %v", first)
	}
	if got, ok := mf.SparseChunkRange(install.Object); !ok || got != first {
		t.Fatalf("SparseChunkRange=(%v,%t), want %v", got, ok, first)
	}
	for _, page := range install.Pages {
		offset := first.Start + uint64(page.Position)*hostarch.PageSize
		gotPage, gotVersion, ok := mf.LogicalPageVersionAt(offset)
		if !ok || gotPage != page.Page || gotVersion != page.Version {
			t.Fatalf("slot %d identity=(%+v,%+v,%t), want (%+v,%+v,true)", page.Slot, gotPage, gotVersion, ok, page.Page, page.Version)
		}
	}
	reusedInstall, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := reusedInstall.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(reusedInstall.NewRanges()) != 0 {
		t.Fatalf("reused import allocated ranges: %v", reusedInstall.NewRanges())
	}
	if got, _ := mf.SparseChunkRange(install.Object); got != first {
		t.Fatalf("reused object moved from %v to %v", first, got)
	}
}

// Branches restored from one immutable chunk may each use a different subset
// of its pages. A later branch reuses the resident chunk and records the
// identities of the slots only it uses; a slot both use must agree.
func TestInstallSparseChunksReusesResidentObjectForAnotherSubset(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	first := sparseTestInstall(1)
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{first})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	resident := installation.NewRanges()[first.Object]
	allocator := first.Pages[0].Page.Allocator
	second := first
	second.Pages = []SparseChunkPage{
		first.Pages[1],
		{Position: 9, Slot: 9, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 9}, Version: PageVersionID{Allocator: allocator, Sequence: 40}, References: 1},
	}
	reused, err := mf.InstallSparseChunks([]SparseChunkInstall{second})
	if err != nil {
		t.Fatalf("a different subset of a resident chunk was refused: %v", err)
	}
	if err := reused.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(reused.NewRanges()) != 0 {
		t.Fatalf("reusing a resident chunk allocated ranges: %v", reused.NewRanges())
	}
	for _, page := range append(append([]SparseChunkPage(nil), first.Pages...), second.Pages[1]) {
		gotPage, gotVersion, ok := mf.LogicalPageVersionAt(resident.Start + uint64(page.Position)*hostarch.PageSize)
		if !ok || gotPage != page.Page || gotVersion != page.Version {
			t.Fatalf("slot %d identity=(%+v,%+v,%t), want (%+v,%+v,true)", page.Slot, gotPage, gotVersion, ok, page.Page, page.Version)
		}
	}
	conflicting := first
	conflicting.Pages = []SparseChunkPage{first.Pages[0]}
	conflicting.Pages[0].Version.Sequence++
	if refused, err := mf.InstallSparseChunks([]SparseChunkInstall{conflicting}); err == nil {
		_ = refused.Abort()
		t.Fatal("a resident chunk accepted a different version of a page it holds")
	}
}

// A chunk may pack pages of several logical chunks, each at a position of
// its own: a page's identity is recorded where its bytes are.
func TestInstallSparseChunksPlacesPackedPagesByPosition(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	var object SparseChunkObjectID
	object[0] = 5
	var allocator LogicalAllocatorID
	allocator[0] = 1
	install := SparseChunkInstall{
		Object: object, Kind: usage.Anonymous,
		Pages: []SparseChunkPage{
			{Position: 0, Slot: 200, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 200}, Version: PageVersionID{Allocator: allocator, Sequence: 50}, References: 1},
			{Position: 1, Slot: 3, Page: LogicalPageID{Allocator: allocator, Sequence: 9*logicalPagesPerChunk + 3}, Version: PageVersionID{Allocator: allocator, Sequence: 51}, References: 1},
		},
	}
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	resident := installation.NewRanges()[object]
	for _, page := range install.Pages {
		gotPage, gotVersion, ok := mf.LogicalPageVersionAt(resident.Start + uint64(page.Position)*hostarch.PageSize)
		if !ok || gotPage != page.Page || gotVersion != page.Version {
			t.Fatalf("page %d identity=(%+v,%+v,%t), want (%+v,%+v,true)", page.Position, gotPage, gotVersion, ok, page.Page, page.Version)
		}
	}
}

func TestInstallRemoteBindingsReusesPageVersionChunk(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	var allocator LogicalAllocatorID
	allocator[0] = 3
	bindings := make([]CapturedPageBinding, 2)
	for i := range bindings {
		page := LogicalPageID{Allocator: allocator, Sequence: uint64(40 + i)}
		chunk, slot := page.Chunk()
		bindings[i] = CapturedPageBinding{
			OldOffset: uint64(i) * hostarch.PageSize, StreamOffset: uint64(i) * hostarch.PageSize,
			References: 1, Page: page, Version: PageVersionID{Allocator: allocator, Sequence: uint64(80 + i)},
			Chunk: chunk, Slot: slot,
		}
	}
	first, relocation, loads, err := mf.InstallRemoteBindings(bindings, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) != 1 || loads[0].Target.Length() != 2*hostarch.PageSize {
		t.Fatalf("first load ranges=%+v", loads)
	}
	if loads[0].logicalChunk == nil || len(loads[0].logicalChunk.ranges) != len(bindings) {
		t.Fatalf("first load lost exact logical-chunk membership: %+v", loads)
	}
	firstOffset, ok := relocation.Page(checkpoint.ResourceID{}, 0)
	if !ok {
		t.Fatal("first relocation is missing")
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	second, secondRelocation, loads, err := mf.InstallRemoteBindings(bindings, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) != 0 {
		t.Fatalf("reused version requested reads: %+v", loads)
	}
	if secondOffset, ok := secondRelocation.Page(checkpoint.ResourceID{}, 0); !ok || secondOffset != firstOffset {
		t.Fatalf("reused relocation=(%#x,%t), want %#x", secondOffset, ok, firstOffset)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	changed := append([]CapturedPageBinding(nil), bindings...)
	changed[0].Version.Sequence++
	third, _, loads, err := mf.InstallRemoteBindings(changed, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) == 0 {
		t.Fatal("changed page version reused stale resident chunk")
	}
	if err := third.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLoadsPreservePackedStreamOrder(t *testing.T) {
	page := uint64(hostarch.PageSize)
	firstChunk := &logicalChunkLoad{}
	secondChunk := &logicalChunkLoad{}
	thirdChunk := &logicalChunkLoad{}
	loads := []RemoteLoadRange{
		{Target: memmap.FileRange{Start: 30 * page, End: 31 * page}, StreamOffset: 2 * page, logicalChunk: thirdChunk},
		{Target: memmap.FileRange{Start: 50 * page, End: 51 * page}, StreamOffset: 0, logicalChunk: firstChunk},
		{Target: memmap.FileRange{Start: 10 * page, End: 11 * page}, StreamOffset: page, logicalChunk: secondChunk},
		{Target: memmap.FileRange{Start: 31 * page, End: 32 * page}, StreamOffset: 3 * page, logicalChunk: thirdChunk},
	}
	got := orderAndCoalesceRemoteLoads(loads)
	if len(got) != 3 {
		t.Fatalf("ordered loads=%+v, want three ranges", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].StreamOffset+got[i-1].Target.Length() != got[i].StreamOffset {
			t.Fatalf("packed stream is not contiguous: %+v", got)
		}
	}
	if got[0].Target.Start != 50*page || got[1].Target.Start != 10*page {
		t.Fatalf("loads were ordered by destination instead of stream: %+v", got)
	}
	if got[2].Target != (memmap.FileRange{Start: 30 * page, End: 32 * page}) {
		t.Fatalf("adjacent destination and stream ranges were not coalesced: %+v", got)
	}
	if got[0].logicalChunk != firstChunk || got[1].logicalChunk != secondChunk || got[2].logicalChunk != thirdChunk {
		t.Fatalf("logical chunk grouping was not preserved: %+v", got)
	}
}

func TestSparseRelocationSplitsDiscontiguousDestination(t *testing.T) {
	resource := checkpoint.ResourceID{Path: "/tmpfs"}
	relocation := NewSparseRelocation()
	if err := relocation.Add(resource, 0, 10*hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if err := relocation.Add(resource, hostarch.PageSize, 20*hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	source := memmap.FileRange{Start: 0, End: 2 * hostarch.PageSize}
	runs, err := relocation.Runs(resource, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("Runs=%v, want two ranges", runs)
	}
	if _, err := relocation.Range(resource, source); err == nil {
		t.Fatal("Range accepted a discontiguous relocation")
	}
}

func TestSparseReferenceTransactionCommitAndAbort(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{sparseTestInstall(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	chunk := installation.NewRanges()[sparseTestInstall(1).Object]
	page := memmap.FileRange{Start: chunk.Start + 3*hostarch.PageSize, End: chunk.Start + 4*hostarch.PageSize}
	if !mf.HasUniqueRef(page) {
		t.Fatal("new sparse chunk does not have exactly its cache reference")
	}

	aborted := NewSparseReferenceTransaction()
	aborted.Add(mf, page, 0)
	aborted.Abort()
	if !mf.HasUniqueRef(page) {
		t.Fatal("aborted reference transaction changed allocator references")
	}

	committed := NewSparseReferenceTransaction()
	committed.Add(mf, page, 0)
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	if mf.HasUniqueRef(page) {
		t.Fatal("committed reference transaction did not add the graph reference")
	}
	mf.DecRef(page)
	if !mf.HasUniqueRef(page) {
		t.Fatal("released graph reference did not leave the cache reference")
	}

	abortedRange, err := mf.Allocate(hostarch.PageSize, AllocOpts{Mode: AllocateAndCommit})
	if err != nil {
		t.Fatal(err)
	}
	abortedAllocation := NewSparseReferenceTransaction()
	abortedAllocation.AddAllocation(mf, abortedRange, 0)
	abortedAllocation.Abort()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("aborted adopted allocation retained a reference")
			}
		}()
		mf.IncRef(abortedRange, 0)
	}()

	committedRange, err := mf.Allocate(hostarch.PageSize, AllocOpts{Mode: AllocateAndCommit})
	if err != nil {
		t.Fatal(err)
	}
	committedAllocation := NewSparseReferenceTransaction()
	committedAllocation.AddAllocation(mf, committedRange, 0)
	if err := committedAllocation.Commit(); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(committedRange) {
		t.Fatal("committed adopted allocation changed its initial owner reference")
	}
	mf.DecRef(committedRange)
}

func TestReclaimUnusedSparseChunks(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	install := sparseTestInstall(1)
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	resident, ok := mf.SparseChunkRange(install.Object)
	if !ok {
		t.Fatal("installed chunk is not resident")
	}
	page := install.Pages[0]
	pageRange := memmap.FileRange{Start: resident.Start + uint64(page.Position)*hostarch.PageSize, End: resident.Start + uint64(page.Position+1)*hostarch.PageSize}
	mf.IncRef(pageRange, 0)
	if got := mf.ReclaimUnusedSparseChunks(); got != 0 {
		t.Fatalf("reclaimed %d chunks while a branch reference remained", got)
	}
	mf.DecRef(pageRange)
	if got := mf.ReclaimUnusedSparseChunks(); got != 1 {
		t.Fatalf("reclaimed %d chunks, want 1", got)
	}
	if _, ok := mf.SparseChunkRange(install.Object); ok {
		t.Fatal("reclaimed chunk remains in residency map")
	}
	if reclaimed := mf.ReclaimedSparseChunks(); len(reclaimed) != 1 || reclaimed[0] != install.Object {
		t.Fatalf("reclaimed chunks are %x, want only %x", reclaimed, install.Object)
	}
	// A chunk installed again is in use again.
	installation, err = mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	if reclaimed := mf.ReclaimedSparseChunks(); len(reclaimed) != 0 {
		t.Fatalf("a reinstalled chunk is still reported reclaimed: %x", reclaimed)
	}
}

// A unit nobody uses is reclaimed though a demand-only loader, which loads
// only what is touched, still holds its pages as not loaded: the loader
// forgets them and finishes.
func TestReclaimUnusedSparseChunksWithDemandLoader(t *testing.T) {
	const pagesPerChunk = SparseChunkBytes / hostarch.PageSize
	source, sourceFR := newRemoteTestMemoryFile(t, pagesPerChunk)
	fillRemoteTestRange(t, source, sourceFR, make([]byte, sourceFR.Length()))
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	pages, err := StartAsyncPagesFileLoadWithOpts(NewLoopbackRemoteReader(snapshot, SparseChunkBytes, 1, 2), nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	install := sparseTestInstall(1)
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	unit, ok := mf.SparseChunkRange(install.Object)
	if !ok {
		t.Fatal("installed chunk is not resident")
	}
	if err := mf.AttachSparseChunkPageLoad(pages, []RemoteLoadRange{{Target: unit, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	if got := mf.ReclaimUnusedSparseChunks(); got != 1 {
		t.Fatalf("reclaimed %d chunks with a demand loader attached, want 1", got)
	}
	if err := pages.Wait(); err != nil {
		t.Fatal(err)
	}
	if stats := pages.Stats(); !stats.Done || stats.BytesOutstanding != 0 || stats.BytesLoaded != 0 {
		t.Fatalf("loader after its only unit was reclaimed: %+v", stats)
	}
	if mf.IsAsyncLoading() {
		t.Fatal("the file retained a loader with nothing to load")
	}
}

// A unit an installation reuses stays until the installation is committed:
// the owners it restores have yet to take their references.
func TestReclaimUnusedSparseChunksSparesAnInstallationInProgress(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	install := sparseTestInstall(1)
	first, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	second, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if got := mf.ReclaimUnusedSparseChunks(); got != 0 {
		t.Fatalf("reclaimed %d chunks an installation reuses", got)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := mf.ReclaimUnusedSparseChunks(); got != 1 {
		t.Fatalf("reclaimed %d chunks once the installation committed, want 1", got)
	}
}
