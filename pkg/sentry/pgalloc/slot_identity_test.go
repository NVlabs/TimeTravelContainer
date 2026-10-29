// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"io"
	"math/rand"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// pageID returns the identity of the page at off.
func pageID(t *testing.T, mf *MemoryFile, off uint64) LogicalPageID {
	t.Helper()
	id, ok := mf.LogicalPageIDAt(off)
	if !ok {
		t.Fatalf("page %#x has no identity", off)
	}
	return id
}

// Pages allocated afresh in a home take the identities of their slots, in
// whatever order they are allocated: a window's start plus the slot's index,
// a 2 MiB page's aligned. A slot taken afresh again takes a new window, so that
// no identity repeats; a copy does not take its slot's.
func TestHomeSlotIdentities(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	opts := AllocOpts{Kind: usage.Anonymous}
	for _, page := range rand.New(rand.NewSource(1)).Perm(64) {
		if _, err := mf.AllocateAt(slotRange(home, 100+uint64(page), 1), opts); err != nil {
			t.Fatal(err)
		}
	}
	first := pageID(t, mf, slotRange(home, 100, 1).Start)
	if first.Sequence%pagesPerChunk != 100 {
		t.Fatalf("slot 100's identity %v is not its window's start plus 100", first)
	}
	window := first.Sequence - 100
	for slot := uint64(100); slot < 164; slot++ {
		if id := pageID(t, mf, slotRange(home, slot, 1).Start); id.Sequence != window+slot || id.Allocator != first.Allocator {
			t.Fatalf("slot %d's identity is %v, want sequence %d", slot, id, window+slot)
		}
	}
	huge, err := mf.AllocateAt(slotRange(home, pagesPerHugePage(), pagesPerHugePage()), AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true})
	if err != nil {
		t.Fatal(err)
	}
	if id := pageID(t, mf, huge.Start); id.Sequence != window+pagesPerHugePage() || !mf.IsHugePages(huge) {
		t.Errorf("the 2 MiB page at slot %d has identity %v (2 MiB page: %t), want sequence %d", pagesPerHugePage(), id, mf.IsHugePages(huge), window+pagesPerHugePage())
	}

	again := slotRange(home, 120, 1)
	mf.DecRef(again)
	waitForRelease(t, mf)
	if _, err := mf.AllocateAt(again, opts); err != nil {
		t.Fatal(err)
	}
	if id := pageID(t, mf, again.Start); id.Sequence == window+120 || id.Sequence%pagesPerChunk != 120 {
		t.Errorf("slot 120 taken afresh again has identity %v: want its slot's in a new window", id)
	}
	if id := pageID(t, mf, slotRange(home, 121, 1).Start); id.Sequence != window+121 {
		t.Errorf("slot 121's identity changed to %v", id)
	}
	// A home has a few windows; a slot taken afresh more often than that
	// takes an identity of no slot.
	seen := map[uint64]bool{window + 120: true, pageID(t, mf, again.Start).Sequence: true}
	for i := 0; i < maxSlotWindows; i++ {
		mf.DecRef(again)
		waitForRelease(t, mf)
		if _, err := mf.AllocateAt(again, opts); err != nil {
			t.Fatal(err)
		}
		id := pageID(t, mf, again.Start)
		if seen[id.Sequence] {
			t.Fatalf("slot 120 taken afresh again took identity %v once more", id)
		}
		seen[id.Sequence] = true
		if slotted := id.Sequence%pagesPerChunk == 120; slotted != (i < maxSlotWindows-2) {
			t.Errorf("slot 120 taken afresh %d times has identity %v (its slot's: %t)", i+3, id, slotted)
		}
	}

	zeroes := safemem.ReaderFunc(func(dsts safemem.BlockSeq) (uint64, error) {
		n, err := safemem.ZeroSeq(dsts)
		if err == nil && n == 0 {
			err = io.EOF
		}
		return n, err
	})
	copied, err := mf.AllocateAt(slotRange(home, 2000, 1), AllocOpts{Kind: usage.Anonymous, ReaderFunc: zeroes})
	if err != nil {
		t.Fatal(err)
	}
	if id := pageID(t, mf, copied.Start); id.Sequence%pagesPerChunk == 2000 {
		t.Errorf("a copy took its slot's identity %v", id)
	}
}

// slotUnit returns the residency unit whose pages are those of logical chunk
// base's 256 pages, at the positions their identities give.
func slotUnit(objectByte byte, allocator LogicalAllocatorID, base uint64) SparseChunkInstall {
	var object SparseChunkObjectID
	object[0] = objectByte
	return SparseChunkInstall{
		Object: object, Kind: usage.Anonymous,
		Runs: []SparseChunkRun{{
			Position: 0, Pages: logicalPagesPerChunk,
			Page:    LogicalPageID{Allocator: allocator, Sequence: base},
			Version: PageVersionID{Allocator: allocator, Sequence: base + 7},
		}},
	}
}

// A residency unit whose pages' identities are their slots' is installed at
// those slots, in a home of its window, so that the units of a window are in
// identity order, as the address space that saved them had its pages; one that
// packs pages of several logical chunks is installed anywhere. A home of units
// that are all released is spared.
func TestInstallSparseChunksAtSlots(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 8)
	var allocator LogicalAllocatorID
	allocator[0] = 9
	window := uint64(3 * pagesPerChunk)
	first := slotUnit(1, allocator, window+5*logicalPagesPerChunk)
	second := slotUnit(2, allocator, window+6*logicalPagesPerChunk)
	// Pages of two logical chunks, packed together.
	packed := SparseChunkInstall{Kind: usage.Anonymous, Runs: []SparseChunkRun{
		{Position: 0, Pages: 1, Page: LogicalPageID{Allocator: allocator, Sequence: 9*logicalPagesPerChunk + 17}, Version: PageVersionID{Allocator: allocator, Sequence: 100}},
		{Position: 1, Pages: 1, Page: LogicalPageID{Allocator: allocator, Sequence: 40*logicalPagesPerChunk + 3}, Version: PageVersionID{Allocator: allocator, Sequence: 200}},
	}}
	packed.Object[0] = 3
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{first, second, packed})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	r1, _ := mf.SparseChunkRange(first.Object)
	r2, _ := mf.SparseChunkRange(second.Object)
	r3, _ := mf.SparseChunkRange(packed.Object)
	home, owner, ok := mf.HomeOf(r1.Start)
	if !ok || owner != 0 {
		t.Fatalf("the unit at %v is in home %v (%t) with owner %d, want a home without one", r1, home, ok, owner)
	}
	if got, want := r1.Start-home.Start, uint64(5*logicalPagesPerChunk)*hostarch.PageSize; got != want {
		t.Errorf("the unit is at %#x of its home, want its slots at %#x", got, want)
	}
	if r2.Start != r1.End {
		t.Errorf("the next logical chunk's unit is at %v, want right after %v", r2, r1)
	}
	for p := uint64(0); p < logicalPagesPerChunk; p += 51 {
		if id := pageID(t, mf, r1.Start+p*hostarch.PageSize); id.Sequence != window+5*logicalPagesPerChunk+p {
			t.Errorf("position %d has identity %v", p, id)
		}
	}
	if _, _, ok := mf.HomeOf(r3.Start); ok {
		t.Errorf("the packed unit is in a home, at %v", r3)
	}
	if n := mf.ReclaimUnusedSparseChunks(); n != 3 {
		t.Fatalf("reclaimed %d units, want 3", n)
	}
	waitForRelease(t, mf)
	mf.mu.Lock()
	units := len(mf.unitHomes)
	mf.mu.Unlock()
	if units != 0 {
		t.Errorf("%d windows still have homes of units once all are released", units)
	}
}
