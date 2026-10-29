// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

const (
	page     = hostarch.PageSize
	hugepage = hostarch.HugePageSize
)

// existingSegment represents a range of pages in a test MemoryFile that is not
// void or free.
type existingSegment struct {
	start uint64
	end   uint64
	state int
}

// Possible values for existingSegment.state:
const (
	existingUnspecified = iota
	existingUsed
	existingWaste
	existingReleasing // or sub-releasing
)

func TestMemoryFileOwnerLifetime(t *testing.T) {
	f := &MemoryFile{}
	f.initFields()

	f.IncOwner()
	f.Destroy()
	f.mu.Lock()
	if got := f.owners; got != 1 {
		t.Errorf("owners after first Destroy: got %d, want 1", got)
	}
	if f.destroyed {
		t.Error("MemoryFile destroyed while an owner remains")
	}
	f.mu.Unlock()

	f.Destroy()
	f.mu.Lock()
	if got := f.owners; got != 0 {
		t.Errorf("owners after final Destroy: got %d, want 0", got)
	}
	if !f.destroyed {
		t.Error("MemoryFile not destroyed after final owner")
	}
	f.mu.Unlock()
}

func newLogicalPageTestMemoryFile() *MemoryFile {
	f := &MemoryFile{
		opts: MemoryFileOpts{DisableMemoryAccounting: true},
	}
	f.initFields()
	chunks := []chunkInfo{{huge: false}}
	f.chunks.Store(&chunks)
	f.unfreeSmall.RemoveRange(memmap.FileRange{0, chunkSize})
	return f
}

func TestLogicalPageIDsAllocationAndOffsetReuse(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	first, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 2 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	id0, ok := f.LogicalPageIDAt(first.Start)
	if !ok {
		t.Fatal("first allocated page has no logical ID")
	}
	id1, ok := f.LogicalPageIDAt(first.Start + page)
	if !ok {
		t.Fatal("second allocated page has no logical ID")
	}
	if id0.Allocator != id1.Allocator || id1.Sequence != id0.Sequence+1 {
		t.Fatalf("allocation IDs are not contiguous: first=%+v second=%+v", id0, id1)
	}

	f.IncRef(first, 0)
	f.DecRef(first)
	if got, ok := f.LogicalPageIDAt(first.Start); !ok || got != id0 {
		t.Fatalf("ID changed while a reference remains: got=%+v ok=%v want=%+v", got, ok, id0)
	}
	f.DecRef(first)
	if got, ok := f.LogicalPageIDAt(first.Start); ok {
		t.Fatalf("released page retained logical ID %+v", got)
	}

	recycled, err := f.findAllocatableAndMarkUsed(&allocState{
		length:     2 * page,
		opts:       AllocOpts{Dir: BottomUp},
		willCommit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recycled != first {
		t.Fatalf("recycled range=%v, want original range=%v", recycled, first)
	}
	recycledID, ok := f.LogicalPageIDAt(recycled.Start)
	if !ok {
		t.Fatal("recycled page has no logical ID")
	}
	if recycledID == id0 || recycledID.Sequence <= id1.Sequence {
		t.Fatalf("recycled offset reused a retired identity: old=%+v new=%+v", id0, recycledID)
	}
	f.DecRef(recycled)
}

// A holder that takes pages over to write them in place gives them new content
// versions under the same logical pages, as a copy would, and only if it holds
// them alone: a capture may hold their bytes under the versions they had.
func TestOwnForWriteRenewsVersions(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 3 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	identities := func() (pages []LogicalPageID, versions []PageVersionID) {
		for offset := fr.Start; offset < fr.End; offset += page {
			id, version, ok := f.LogicalPageVersionAt(offset)
			if !ok {
				t.Fatalf("page at %#x has no identity", offset)
			}
			pages, versions = append(pages, id), append(versions, version)
		}
		return pages, versions
	}
	pages, versions := identities()

	f.IncRef(fr, 0)
	if f.OwnForWrite(fr) {
		t.Fatal("OwnForWrite took over pages another reference holds")
	}
	if gotPages, gotVersions := identities(); !reflect.DeepEqual(gotPages, pages) || !reflect.DeepEqual(gotVersions, versions) {
		t.Fatalf("refused OwnForWrite changed identities: %+v %+v, want %+v %+v", gotPages, gotVersions, pages, versions)
	}
	f.DecRef(fr)

	if !f.OwnForWrite(fr) {
		t.Fatal("OwnForWrite refused pages held alone")
	}
	gotPages, gotVersions := identities()
	if !reflect.DeepEqual(gotPages, pages) {
		t.Fatalf("OwnForWrite changed logical pages: %+v, want %+v", gotPages, pages)
	}
	for index, version := range gotVersions {
		for _, old := range versions {
			if version == old {
				t.Fatalf("page %d kept version %+v", index, version)
			}
		}
		if index != 0 && version.Sequence != gotVersions[index-1].Sequence+1 {
			t.Fatalf("renewed versions are not contiguous: %+v", gotVersions)
		}
	}
	f.DecRef(fr)
}

func TestLogicalPageChunkCoordinates(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: (logicalPagesPerChunk + 2) * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := f.LogicalPageIDAt(fr.Start)
	lastInChunk, _ := f.LogicalPageIDAt(fr.Start + (logicalPagesPerChunk-1)*page)
	firstNext, _ := f.LogicalPageIDAt(fr.Start + logicalPagesPerChunk*page)
	chunk0, slot0 := first.Chunk()
	chunkLast, slotLast := lastInChunk.Chunk()
	chunk1, slot1 := firstNext.Chunk()
	if chunk0 != chunkLast || slotLast != 255 {
		t.Fatalf("first logical chunk boundary is wrong: first=(%+v,%d) last=(%+v,%d)", chunk0, slot0, chunkLast, slotLast)
	}
	if chunk1.Allocator != chunk0.Allocator || chunk1.Sequence != chunk0.Sequence+1 || slot1 != 0 {
		t.Fatalf("next logical chunk boundary is wrong: first=(%+v,%d) next=(%+v,%d)", chunk0, slot0, chunk1, slot1)
	}
	f.DecRef(fr)
}

func TestMemoryRangeCaptureCoalescesAndBindsPages(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 4 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	capture := NewMemoryRangeCapture()
	if err := capture.Record(f, memmap.FileRange{Start: fr.Start + page, End: fr.End}); err != nil {
		t.Fatal(err)
	}
	if err := capture.Record(f, memmap.FileRange{Start: fr.Start, End: fr.Start + 2*page}); err != nil {
		t.Fatal(err)
	}
	ranges := capture.Ranges()
	if len(ranges) != 1 || ranges[0].MemoryFile != f || ranges[0].Range != fr {
		t.Fatalf("captured ranges = %+v, want one range %+v", ranges, fr)
	}
	bindings, err := capture.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 4 {
		t.Fatalf("got %d bindings, want 4", len(bindings))
	}
	for i, binding := range bindings {
		pageID, versionID, ok := f.LogicalPageVersionAt(fr.Start + uint64(i)*page)
		if !ok {
			t.Fatalf("page %d has no identities", i)
		}
		chunkID, slot := pageID.Chunk()
		if binding.OldOffset != fr.Start+uint64(i)*page || binding.Page != pageID || binding.Version != versionID || binding.Chunk != chunkID || binding.Slot != slot {
			t.Fatalf("binding %d = %+v, want offset=%#x page=%+v version=%+v chunk=%+v slot=%d", i, binding, fr.Start+uint64(i)*page, pageID, versionID, chunkID, slot)
		}
		wantReferences := uint64(1)
		if i == 1 {
			wantReferences = 2
		}
		if binding.References != wantReferences {
			t.Fatalf("binding %d has %d references, want %d", i, binding.References, wantReferences)
		}
		if binding.StreamOffset != uint64(i)*page {
			t.Fatalf("binding %d has stream offset %#x, want %#x", i, binding.StreamOffset, uint64(i)*page)
		}
	}
	f.DecRef(fr)
}

func TestMemoryRangeCaptureSnapshotPinsOnlyCapturedPages(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 4 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	capturedFR := memmap.FileRange{Start: fr.Start + page, End: fr.End - page}
	capture := NewMemoryRangeCapture()
	if err := capture.Record(f, capturedFR); err != nil {
		t.Fatal(err)
	}
	layout, err := capture.SnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Regions) != 1 || layout.Regions[0].Range != capturedFR || len(layout.Bindings) != 2 {
		t.Fatalf("snapshot layout = %+v", layout)
	}
	snapshot, err := NewRemoteSnapshot(layout.Regions)
	if err != nil {
		t.Fatal(err)
	}
	if f.HasUniqueRef(capturedFR) {
		t.Fatal("captured pages remained uniquely referenced after snapshot")
	}
	uncaptured := memmap.FileRange{Start: fr.Start, End: fr.Start + page}
	if !f.HasUniqueRef(uncaptured) {
		t.Fatal("snapshot pinned an uncaptured sibling page")
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.HasUniqueRef(capturedFR) {
		t.Fatal("closing snapshot did not release captured pages")
	}
	f.DecRef(fr)
}

func TestCapturedMemoryLayoutChunkOrdered(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 3 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(fr)
	type identity struct {
		page    LogicalPageID
		version PageVersionID
		chunk   LogicalChunkID
		slot    uint8
	}
	identities := make([]identity, 3)
	for i := range identities {
		pageID, versionID, ok := f.LogicalPageVersionAt(fr.Start + uint64(i)*page)
		if !ok {
			t.Fatalf("page %d has no logical identity", i)
		}
		chunk, slot := pageID.Chunk()
		identities[i] = identity{page: pageID, version: versionID, chunk: chunk, slot: slot}
	}
	// Model CoW relocation: logical pages 2, 0, 1 occupy physical offsets 0,
	// 1, 2. Address order therefore splits the logical chunk, while chunk order
	// must produce stream offsets 0, 1, 2 without changing physical sources.
	physicalToLogical := []int{2, 0, 1}
	layout := CapturedMemoryLayout{Regions: []RemoteSnapshotRegion{{
		ID: 1, File: f, Range: fr,
	}}}
	for physical, logical := range physicalToLogical {
		id := identities[logical]
		layout.Bindings = append(layout.Bindings, CapturedPageBinding{
			OldOffset: fr.Start + uint64(physical)*page, StreamOffset: uint64(physical) * page,
			References: 1, Page: id.page, Version: id.version, Chunk: id.chunk, Slot: id.slot,
		})
	}
	ordered, err := layout.ChunkOrdered()
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered.Bindings) != 3 || len(ordered.Regions) != 2 {
		t.Fatalf("ordered layout=%+v", ordered)
	}
	wantPhysical := []uint64{fr.Start + page, fr.Start + 2*page, fr.Start}
	for i, binding := range ordered.Bindings {
		if binding.Page != identities[i].page || binding.OldOffset != wantPhysical[i] || binding.StreamOffset != uint64(i)*page {
			t.Fatalf("binding %d=%+v, want page=%+v old=%#x stream=%#x", i, binding, identities[i].page, wantPhysical[i], uint64(i)*page)
		}
	}
}

func TestCapturedMemoryLayoutChunkOrderedAllowsDivergedCOWAliases(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	source, err := f.findAllocatableAndMarkUsed(&allocState{
		length: page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.findAllocatableAndMarkUsed(&allocState{
		length: page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(target)
	defer f.DecRef(source)
	if err := f.AdoptCOWIdentities(f, source, target); err != nil {
		t.Fatal(err)
	}
	sourcePage, sourceVersion, ok := f.LogicalPageVersionAt(source.Start)
	if !ok {
		t.Fatal("source page has no identities")
	}
	targetPage, targetVersion, ok := f.LogicalPageVersionAt(target.Start)
	if !ok {
		t.Fatal("target page has no identities")
	}
	if sourcePage != targetPage || sourceVersion == targetVersion {
		t.Fatalf("CoW identities source=(%+v,%+v) target=(%+v,%+v)", sourcePage, sourceVersion, targetPage, targetVersion)
	}
	chunk, slot := sourcePage.Chunk()
	layout := CapturedMemoryLayout{
		Regions: []RemoteSnapshotRegion{{ID: 1, File: f, Range: memmap.FileRange{Start: source.Start, End: target.End}}},
		Bindings: []CapturedPageBinding{
			{OldOffset: source.Start, References: 1, Page: sourcePage, Version: sourceVersion, Chunk: chunk, Slot: slot},
			{OldOffset: target.Start, References: 1, Page: targetPage, Version: targetVersion, Chunk: chunk, Slot: slot},
		},
	}
	ordered, err := layout.ChunkOrdered()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ordered.Bindings); got != 2 {
		t.Fatalf("got %d bindings, want 2", got)
	}
	if ordered.Bindings[0].Page != ordered.Bindings[1].Page || ordered.Bindings[0].Version == ordered.Bindings[1].Version {
		t.Fatalf("ordered bindings lost divergent CoW versions: %+v", ordered.Bindings)
	}
}

func TestMemoryRangeCaptureRejectsRetiredPage(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	capture := NewMemoryRangeCapture()
	if err := capture.Record(f, fr); err != nil {
		t.Fatal(err)
	}
	f.DecRef(fr)
	if _, err := capture.Bindings(); err == nil {
		t.Fatal("Bindings succeeded after the captured page was retired")
	}
}

func TestAdoptCOWIdentitiesPreservesPageAndChangesVersion(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	source, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 2 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 2 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceID, sourceVersion, ok := f.LogicalPageVersionAt(source.Start)
	if !ok {
		t.Fatal("source page has no identities")
	}
	transientTargetID, _, ok := f.LogicalPageVersionAt(target.Start)
	if !ok {
		t.Fatal("target page has no initial identities")
	}
	if err := f.AdoptCOWIdentities(f, source, target); err != nil {
		t.Fatal(err)
	}
	targetID, targetVersion, ok := f.LogicalPageVersionAt(target.Start)
	if !ok {
		t.Fatal("target page has no adopted identities")
	}
	if targetID != sourceID {
		t.Fatalf("CoW changed logical page ID: source=%+v target=%+v", sourceID, targetID)
	}
	if targetVersion == sourceVersion {
		t.Fatalf("CoW retained content version: source=%+v target=%+v", sourceVersion, targetVersion)
	}
	if targetID == transientTargetID {
		t.Fatalf("CoW retained target allocation's transient logical ID: %+v", targetID)
	}
	gotSourceID, gotSourceVersion, ok := f.LogicalPageVersionAt(source.Start)
	if !ok || gotSourceID != sourceID || gotSourceVersion != sourceVersion {
		t.Fatalf("CoW changed source identities: got=(%+v,%+v,%v) want=(%+v,%+v,true)", gotSourceID, gotSourceVersion, ok, sourceID, sourceVersion)
	}
	f.DecRef(target)
	f.DecRef(source)
}

// An allocation whose caller may commit only some of its pages recycles waste
// pages if it is small, zeroing what their last user left in them, and takes
// free pages otherwise.
func TestAllocateUncommittedRecyclesSmallWaste(t *testing.T) {
	for _, test := range []struct {
		length  uint64
		recycle bool
	}{
		{length: 2 * page, recycle: true},
		{length: maxRecycledUncommittedBytes, recycle: true},
		{length: maxRecycledUncommittedBytes + page, recycle: false},
	} {
		f := newEmptyRemoteTestMemoryFile(t)
		// Keep the releaser from releasing the waste before it can be recycled.
		f.mu.Lock()
		f.holdReleaserLocked()
		f.mu.Unlock()
		used, err := f.Allocate(test.length, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
		if err != nil {
			t.Fatal(err)
		}
		dsts, err := f.MapInternal(used, hostarch.Write)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := safemem.CopySeq(dsts, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(bytes.Repeat([]byte{0x5a}, int(test.length))))); err != nil {
			t.Fatal(err)
		}
		f.DecRef(used)
		got, err := f.Allocate(test.length, AllocOpts{Kind: usage.Anonymous, Mode: AllocateUncommitted})
		if err != nil {
			t.Fatal(err)
		}
		if recycled := got == used; recycled != test.recycle {
			t.Errorf("allocation of %d bytes: got %v from waste %v, recycled=%t, want %t", test.length, got, used, recycled, test.recycle)
		}
		srcs, err := f.MapInternal(got, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, test.length)
		if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), srcs); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, make([]byte, test.length)) {
			t.Errorf("allocation of %d bytes holds data its pages had as waste", test.length)
		}
		f.DecRef(got)
		f.mu.Lock()
		f.unholdReleaserLocked()
		f.mu.Unlock()
		f.Destroy()
	}
}

// The releaser releases runs of waste of a huge page or more before smaller
// ones, and, among either, higher ones first.
func TestReleaserReleasesLargeRunsFirst(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	for _, fr := range []memmap.FileRange{
		{0, hugepage},
		{hugepage + page, hugepage + 2*page},
		{2 * hugepage, 3*hugepage + page},
		{chunkSize - page, chunkSize},
	} {
		f.unwasteSmall.RemoveRange(fr)
	}
	for _, want := range []memmap.FileRange{
		{2 * hugepage, 3*hugepage + page},
		{0, hugepage},
		{chunkSize - page, chunkSize},
		{hugepage + page, hugepage + 2*page},
	} {
		if fr, kind, ok := f.nextReleaseLocked(); !ok || kind != releaseSmall || fr != want {
			t.Fatalf("nextReleaseLocked() = %v, %d, %t, want %v, %d, true", fr, kind, ok, want, releaseSmall)
		}
	}
	if fr, _, ok := f.nextReleaseLocked(); ok {
		t.Fatalf("nextReleaseLocked() = %v with no waste left", fr)
	}
}

func TestFindAllocatable(t *testing.T) {
	for _, test := range []struct {
		name string
		// Initial state:
		chunkHuge []bool
		existing  []existingSegment
		// Allocation parameters:
		length  uint64
		huge    bool
		recycle bool
		dir     Direction
		// Expected outcome:
		want uint64
	}{
		{
			name:   "initial small allocation, bottom-up",
			length: page,
			want:   0,
		},
		{
			name:   "initial small allocation, top-down",
			length: page,
			dir:    TopDown,
			want:   chunkSize - page,
		},
		{
			name:   "initial small allocation, multiple pages, top-down",
			length: 2 * page,
			dir:    TopDown,
			want:   chunkSize - 2*page,
		},
		{
			name:    "initial small allocation, recycling enabled, bottom-up",
			length:  page,
			recycle: true,
			want:    0,
		},
		{
			name:   "initial huge allocation, bottom-up",
			length: hugepage,
			huge:   true,
			want:   0,
		},
		{
			name:   "initial huge allocation, top-down",
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - hugepage,
		},
		{
			name:   "initial huge allocation, multiple pages, top-down",
			length: 2 * hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 2*hugepage,
		},
		{
			name:    "initial huge allocation, recycling enabled, bottom-up",
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    0,
		},
		{
			name:      "huge allocation uses huge pages in new chunk",
			chunkHuge: []bool{false},
			length:    hugepage,
			huge:      true,
			want:      chunkSize,
		},
		{
			name:      "huge allocation uses huge pages in existing chunk",
			chunkHuge: []bool{false, true},
			length:    hugepage,
			huge:      true,
			want:      chunkSize,
		},
		{
			name:      "hugepage-sized non-huge allocation uses small pages in new chunk",
			chunkHuge: []bool{true},
			length:    hugepage,
			want:      chunkSize,
		},
		{
			name:      "hugepage-sized non-huge allocation uses small pages in existing chunk",
			chunkHuge: []bool{true, false},
			length:    hugepage,
			want:      chunkSize,
		},
		{
			name:      "bottom-up small allocation begins at start of file",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{page, 2 * page, existingUsed},
			},
			length: page,
			want:   0,
		},
		{
			name:      "top-down small allocation begins at end of last chunk",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - 2*page, chunkSize - page, existingUsed},
			},
			length: page,
			dir:    TopDown,
			want:   chunkSize - page,
		},
		{
			name:      "bottom-up huge allocation begins at start of file",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{hugepage, 2 * hugepage, existingUsed},
			},
			length: hugepage,
			huge:   true,
			want:   0,
		},
		{
			name:      "top-down huge allocation begins at end of last chunk",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - 2*hugepage, chunkSize - hugepage, existingUsed},
			},
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - hugepage,
		},
		{
			name:      "bottom-up small allocation can extend multiple chunks",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize/2 - page, chunkSize / 2, existingUsed},
			},
			length: 2*chunkSize + page,
			want:   chunkSize / 2,
		},
		{
			name:      "top-down small allocation can extend multiple chunks",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize/2 - page, chunkSize / 2, existingUsed},
			},
			length: 2*chunkSize + page,
			dir:    TopDown,
			want:   chunkSize - page,
		},
		{
			name:      "bottom-up huge allocation can extend multiple chunks",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize/2 - hugepage, chunkSize / 2, existingUsed},
			},
			length: 2*chunkSize + hugepage,
			huge:   true,
			want:   chunkSize / 2,
		},
		{
			name:      "top-down huge allocation can extend multiple chunks",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize/2 - hugepage, chunkSize / 2, existingUsed},
			},
			length: 2*chunkSize + hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - hugepage,
		},
		{
			name:      "bottom-up small allocation finds first free gap",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingUsed},
				{2 * page, 3 * page, existingUsed},
			},
			length: page,
			want:   page,
		},
		{
			name:      "top-down small allocation finds last free gap",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingUsed},
				{chunkSize - 3*page, chunkSize - 2*page, existingUsed},
			},
			length: page,
			dir:    TopDown,
			want:   chunkSize - 2*page,
		},
		{
			name:      "bottom-up huge allocation finds first free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingUsed},
				{2 * hugepage, 3 * hugepage, existingUsed},
			},
			length: hugepage,
			huge:   true,
			want:   hugepage,
		},
		{
			name:      "top-down huge allocation finds last free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingUsed},
				{chunkSize - 3*hugepage, chunkSize - 2*hugepage, existingUsed},
			},
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 2*hugepage,
		},
		{
			name:      "bottom-up small allocation skips undersized free gap",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingUsed},
				{2 * page, 3 * page, existingUsed},
			},
			length: 2 * page,
			want:   3 * page,
		},
		{
			name:      "top-down small allocation skips undersized free gap",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingUsed},
				{chunkSize - 3*page, chunkSize - 2*page, existingUsed},
			},
			length: 2 * page,
			dir:    TopDown,
			want:   chunkSize - 5*page,
		},
		{
			name:      "bottom-up huge allocation skips undersized free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingUsed},
				{2 * hugepage, 3 * hugepage, existingUsed},
			},
			length: 2 * hugepage,
			huge:   true,
			want:   3 * hugepage,
		},
		{
			name:      "top-down huge allocation skips undersized free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingUsed},
				{chunkSize - 3*hugepage, chunkSize - 2*hugepage, existingUsed},
			},
			length: 2 * hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 5*hugepage,
		},
		{
			name:      "recycling bottom-up small allocation skips used pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingUsed},
			},
			length:  page,
			recycle: true,
			want:    page,
		},
		{
			name:      "recycling top-down small allocation skips used pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingUsed},
			},
			length:  page,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*page,
		},
		{
			name:      "recycling bottom-up huge allocation skips used pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    hugepage,
		},
		{
			name:      "recycling top-down huge allocation skips used pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*hugepage,
		},
		{
			name:      "non-recycling bottom-up small allocation skips waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, maxRecycledUncommittedBytes + page, existingWaste},
			},
			length: maxRecycledUncommittedBytes + page,
			want:   maxRecycledUncommittedBytes + page,
		},
		{
			name:      "non-recycling top-down small allocation skips waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - (maxRecycledUncommittedBytes + page), chunkSize, existingWaste},
			},
			length: maxRecycledUncommittedBytes + page,
			dir:    TopDown,
			want:   chunkSize - 2*(maxRecycledUncommittedBytes+page),
		},
		{
			name:      "uncommitted bottom-up allocation of few small pages recycles waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingWaste},
			},
			length: page,
			want:   0,
		},
		{
			name:      "uncommitted top-down allocation of few small pages recycles waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingWaste},
			},
			length: page,
			dir:    TopDown,
			want:   chunkSize - page,
		},
		{
			name:      "non-recycling bottom-up huge allocation skips waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingWaste},
			},
			length: hugepage,
			huge:   true,
			want:   hugepage,
		},
		{
			name:      "non-recycling top-down huge allocation skips waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingWaste},
			},
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 2*hugepage,
		},
		{
			name:      "recycling bottom-up small allocation recycles waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingWaste},
			},
			length:  page,
			recycle: true,
			want:    0,
		},
		{
			name:      "recycling top-down small allocation recycles waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingWaste},
			},
			length:  page,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - page,
		},
		{
			name:      "recycling bottom-up huge allocation recycles waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    0,
		},
		{
			name:      "recycling top-down huge allocation recycles waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - hugepage,
		},
		{
			name:      "recycling bottom-up huge allocation aligns within unaligned waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, page, existingUsed},
				{page, 2 * hugepage, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    hugepage,
		},
		{
			name:      "recycling top-down huge allocation aligns within unaligned waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - 2*hugepage, chunkSize - page, existingWaste},
				{chunkSize - page, chunkSize, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*hugepage,
		},
		{
			name:      "recycling bottom-up huge allocation aligns within barely large enough waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{hugepage - page, 2 * hugepage, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    hugepage,
		},
		{
			name:      "recycling top-down huge allocation aligns within barely large enough waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - 2*hugepage, chunkSize - hugepage + page, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*hugepage,
		},
		{
			name:      "recycling bottom-up huge allocation skips unaligned waste pages that can't fit",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, page, existingUsed},
				{page, hugepage + page, existingWaste},
				{hugepage + page, 2 * hugepage, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    2 * hugepage,
		},
		{
			name:      "recycling bottom-up huge allocation skips to later waste pages that fit",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, page, existingUsed},
				{page, hugepage + page, existingWaste},
				{hugepage + page, 2 * hugepage, existingUsed},
				{2 * hugepage, 4 * hugepage, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    2 * hugepage,
		},
		{
			name:      "recycling top-down huge allocation skips to earlier waste pages that fit",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - 4*hugepage, chunkSize - 2*hugepage, existingWaste},
				{chunkSize - 2*hugepage, chunkSize - hugepage - page, existingUsed},
				{chunkSize - hugepage - page, chunkSize - page, existingWaste},
				{chunkSize - page, chunkSize, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 3*hugepage,
		},
		{
			name:      "non-recycling bottom-up small allocation skips releasing pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingReleasing},
			},
			length: page,
			want:   page,
		},
		{
			name:      "non-recycling top-down small allocation skips releasing pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingReleasing},
			},
			length: page,
			dir:    TopDown,
			want:   chunkSize - 2*page,
		},
		{
			name:      "non-recycling bottom-up huge allocation skips releasing pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingReleasing},
			},
			length: hugepage,
			huge:   true,
			want:   hugepage,
		},
		{
			name:      "non-recycling top-down huge allocation skips releasing pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingReleasing},
			},
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 2*hugepage,
		},
		{
			name:      "recycling bottom-up small allocation skips releasing pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{0, page, existingReleasing},
			},
			length:  page,
			recycle: true,
			want:    page,
		},
		{
			name:      "recycling top-down small allocation skips releasing pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingReleasing},
			},
			length:  page,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*page,
		},
		{
			name:      "recycling bottom-up huge allocation skips releasing pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage, existingReleasing},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    hugepage,
		},
		{
			name:      "recycling top-down huge allocation skips releasing pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage, chunkSize, existingReleasing},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*hugepage,
		},
		{
			name:      "bottom-up huge allocation skips the unaligned start of a free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, hugepage + page, existingUsed},
			},
			length: hugepage,
			huge:   true,
			want:   2 * hugepage,
		},
		{
			name:      "top-down huge allocation skips the unaligned end of a free gap",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - hugepage - page, chunkSize, existingUsed},
			},
			length: hugepage,
			huge:   true,
			dir:    TopDown,
			want:   chunkSize - 3*hugepage,
		},
		{
			name:      "recycling bottom-up huge allocation recycles only aligned waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{0, page, existingUsed},
				{page, 2 * hugepage, existingWaste},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			want:    hugepage,
		},
		{
			name:      "recycling top-down huge allocation recycles only aligned waste pages",
			chunkHuge: []bool{true},
			existing: []existingSegment{
				{chunkSize - 2*hugepage, chunkSize - page, existingWaste},
				{chunkSize - page, chunkSize, existingUsed},
			},
			length:  hugepage,
			huge:    true,
			recycle: true,
			dir:     TopDown,
			want:    chunkSize - 2*hugepage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Build the fake MemoryFile.
			f := &MemoryFile{
				opts: MemoryFileOpts{
					ExpectHugepages:         true,
					DisableMemoryAccounting: true,
				},
			}
			f.initFields()
			chunks := make([]chunkInfo, len(test.chunkHuge))
			for i, huge := range test.chunkHuge {
				chunks[i].huge = huge
				chunkFR := memmap.FileRange{uint64(i) * chunkSize, uint64(i+1) * chunkSize}
				if huge {
					f.unfreeHuge.RemoveRange(chunkFR)
				} else {
					f.unfreeSmall.RemoveRange(chunkFR)
				}
			}
			f.chunks.Store(&chunks)
			for _, es := range test.existing {
				f.forEachChunk(memmap.FileRange{es.start, es.end}, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
					unwaste, unfree := &f.unwasteSmall, &f.unfreeSmall
					if chunk.huge {
						unwaste, unfree = &f.unwasteHuge, &f.unfreeHuge
					}
					switch es.state {
					case existingUsed:
						unfree.InsertRange(chunkFR, unfreeInfo{refs: 1})
					case existingWaste:
						unfree.InsertRange(chunkFR, unfreeInfo{refs: 0})
						unwaste.RemoveRange(chunkFR)
					case existingReleasing:
						unfree.InsertRange(chunkFR, unfreeInfo{refs: 0})
					default:
						t.Fatalf("existingSegment %+v has unknown state", es)
					}
					f.memAcct.InsertRange(chunkFR, memAcctInfo{
						wasteOrReleasing: es.state != existingUsed,
					})
					return true
				})
			}

			// Perform the test allocation.
			alloc := allocState{
				length: test.length,
				opts: AllocOpts{
					Huge: test.huge,
					Dir:  test.dir,
				},
				huge: test.huge,
			}
			if test.recycle {
				alloc.opts.Mode = AllocateCallerIndirectCommit
				alloc.willCommit = true
			}
			fr, err := f.findAllocatableAndMarkUsed(&alloc)
			if err != nil {
				t.Fatalf("findAllocatableAndMarkUsed(%+v): failed: %v, want: %#x\n%v", alloc, err, test.want, f)
			}
			if fr.Start != test.want {
				t.Errorf("findAllocatableAndMarkUsed(%+v): got: start=%#x, want: %#x\n%v", alloc, fr.Start, test.want, f)
			}
			if wantEnd := test.want + test.length; fr.End != wantEnd {
				t.Errorf("findAllocatableAndMarkUsed(%+v): got: end=%#x, want: %#x\n%v", alloc, fr.End, wantEnd, f)
			}
		})
	}
}
