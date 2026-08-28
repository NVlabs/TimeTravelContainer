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

//go:build !pagesize_64k

package pgalloc

import (
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
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

func TestCapturedMemoryLayoutPacksLogicalChunksContiguously(t *testing.T) {
	f := newLogicalPageTestMemoryFile()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: 4 * page,
		opts:   AllocOpts{Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(fr)
	capture := NewMemoryRangeCapture()
	if err := capture.Record(f, fr); err != nil {
		t.Fatal(err)
	}
	layout, err := capture.SnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately make physical offset order disagree with logical chunk
	// order. This is representative of CoW versions and sparse imports after a
	// few live migrations.
	var low, high, version LogicalAllocatorID
	low[0], high[0], version[0] = 1, 2, 3
	type logical struct {
		allocator LogicalAllocatorID
		slot      uint64
	}
	identities := []logical{{high, 0}, {low, 1}, {high, 1}, {low, 0}}
	for i := range layout.Bindings {
		identity := identities[i]
		layout.Bindings[i].Page = LogicalPageID{Allocator: identity.allocator, Sequence: identity.slot}
		layout.Bindings[i].Version = PageVersionID{Allocator: version, Sequence: uint64(i + 1)}
		layout.Bindings[i].Chunk, layout.Bindings[i].Slot = layout.Bindings[i].Page.Chunk()
	}
	packed, err := layout.PackByLogicalChunk()
	if err != nil {
		t.Fatal(err)
	}
	wantPhysicalPages := []uint64{3, 1, 0, 2}
	for i, binding := range packed.Bindings {
		if got, want := binding.OldOffset, fr.Start+wantPhysicalPages[i]*page; got != want {
			t.Fatalf("packed binding %d offset=%#x, want %#x", i, got, want)
		}
		if got, want := binding.StreamOffset, uint64(i)*page; got != want {
			t.Fatalf("packed binding %d stream offset=%#x, want %#x", i, got, want)
		}
		if i != 0 && packed.Bindings[i-1].Chunk == binding.Chunk &&
			packed.Bindings[i-1].StreamOffset+page != binding.StreamOffset {
			t.Fatalf("logical chunk is not contiguous at binding %d", i)
		}
	}
	var packedBytes uint64
	for i, region := range packed.Regions {
		if !region.LogicalChunked || region.ID != RemoteRegionID(i+1) {
			t.Fatalf("packed region %d = %+v", i, region)
		}
		packedBytes += region.Range.Length()
	}
	if packedBytes != fr.Length() {
		t.Fatalf("packed regions contain %#x bytes, want %#x", packedBytes, fr.Length())
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
				{0, page, existingWaste},
			},
			length: page,
			want:   page,
		},
		{
			name:      "non-recycling top-down small allocation skips waste pages",
			chunkHuge: []bool{false},
			existing: []existingSegment{
				{chunkSize - page, chunkSize, existingWaste},
			},
			length: page,
			dir:    TopDown,
			want:   chunkSize - 2*page,
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
