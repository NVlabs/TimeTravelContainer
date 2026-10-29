// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"errors"
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// newHomeTestMemoryFile returns a MemoryFile, backed by a file, that may have
// maxHomes home chunks, and whose huge allocations may be 2 MiB pages.
func newHomeTestMemoryFile(t testing.TB, maxHomes int) *MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-home-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
		AdviseNoHugepage:        true,
		MaxHomes:                maxHomes,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(mf.Destroy)
	return mf
}

// fillPages writes b to every byte of fr.
func fillPages(t *testing.T, mf *MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	dsts, err := mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	for rem := dsts; !rem.IsEmpty(); rem = rem.Tail() {
		bs := rem.Head().ToSlice()
		for i := range bs {
			bs[i] = b
		}
	}
}

// pagesHold reports whether every byte of fr is b.
func pagesHold(t *testing.T, mf *MemoryFile, fr memmap.FileRange, b byte) bool {
	t.Helper()
	srcs, err := mf.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	for rem := srcs; !rem.IsEmpty(); rem = rem.Tail() {
		for _, got := range rem.Head().ToSlice() {
			if got != b {
				return false
			}
		}
	}
	return true
}

// waitForRelease waits until mf's releaser has released all waste.
func waitForRelease(t *testing.T, mf *MemoryFile) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mf.mu.Lock()
		haveWaste := mf.haveWaste
		mf.mu.Unlock()
		if !haveWaste {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("waste was never released")
		}
		time.Sleep(time.Millisecond)
	}
}

func slotRange(home memmap.FileRange, page, pages uint64) memmap.FileRange {
	start := home.Start + page*hostarch.PageSize
	return memmap.FileRange{Start: start, End: start + pages*hostarch.PageSize}
}

// An owner allocates the slots of its home, at the offsets it asks for, and
// only it: Allocate never takes a page of a home, free or waste, and a slot in
// use or being released cannot be allocated again. A freed slot is recycled
// into the same slot, zeroed, or released back into a free slot.
func TestHomeSlots(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	owner := NewHomeOwner()
	home, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	if home.Length() != chunkSize || home.Start%chunkSize != 0 {
		t.Fatalf("home %v is not a chunk", home)
	}
	// Slots out of order, as an address space faults them.
	opts := AllocOpts{Kind: usage.Anonymous}
	for _, page := range []uint64{700, 3, 4, 701} {
		want := slotRange(home, page, 1)
		got, err := mf.AllocateAt(want, opts)
		if err != nil || got != want {
			t.Fatalf("AllocateAt(%v) = %v, %v", want, got, err)
		}
	}
	if _, err := mf.AllocateAt(slotRange(home, 4, 2), opts); !errors.Is(err, ErrSlotsInUse) {
		t.Fatalf("allocating a slot in use: %v, want ErrSlotsInUse", err)
	}

	// Ordinary allocations go elsewhere, and so does recycled waste.
	fillPages(t, mf, slotRange(home, 700, 2), 0xa5)
	mf.DecRef(slotRange(home, 700, 2))
	for i := 0; i < 64; i++ {
		fr, err := mf.Allocate(hostarch.PageSize, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
		if err != nil {
			t.Fatal(err)
		}
		if fr.Overlaps(home) {
			t.Fatalf("Allocate returned %v, in home %v", fr, home)
		}
	}
	got, err := mf.AllocateAt(slotRange(home, 700, 1), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !pagesHold(t, mf, got, 0) {
		t.Fatal("a recycled home slot was not zeroed")
	}

	// Released waste goes back to free slots, not to Allocate.
	mf.DecRef(slotRange(home, 3, 2))
	waitForRelease(t, mf)
	mf.mu.Lock()
	if seg := mf.unfreeSmall.FindSegment(slotRange(home, 3, 1).Start); !seg.Ok() || seg.ValuePtr().refs != 0 {
		mf.mu.Unlock()
		t.Fatal("a released home slot left its home")
	}
	if gap := mf.unwasteHome.FirstLargeEnoughGap(1); gap.Ok() {
		mf.mu.Unlock()
		t.Fatalf("home waste %v remains after release", gap.Range())
	}
	mf.mu.Unlock()
	if _, err := mf.AllocateAt(slotRange(home, 3, 2), opts); err != nil {
		t.Fatalf("allocating released slots: %v", err)
	}
}

// A 2 MiB page is allocated at its aligned slots as one page.
func TestHomeHugePage(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 1)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	fr := slotRange(home, 2*pagesPerHugePage(), pagesPerHugePage())
	got, err := mf.AllocateAt(fr, AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true, Mode: AllocateAndWritePopulate})
	if err != nil || got != fr {
		t.Fatalf("AllocateAt(%v) = %v, %v", fr, got, err)
	}
	if !mf.IsHugePages(fr) {
		t.Fatalf("%v is not a 2 MiB page", fr)
	}
	// A copy fills it from its reader.
	src := make([]byte, hostarch.HugePageSize)
	for i := range src {
		src[i] = 0x5a
	}
	copyFR := slotRange(home, 4*pagesPerHugePage(), pagesPerHugePage())
	got, err = mf.AllocateAt(copyFR, AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true, Mode: AllocateAndWritePopulate,
		ReaderFunc: (&safemem.BlockSeqReader{Blocks: safemem.BlockSeqOf(safemem.BlockFromSafeSlice(src))}).ReadToBlocks})
	if err != nil || got != copyFR || !pagesHold(t, mf, copyFR, 0x5a) {
		t.Fatalf("AllocateAt(%v) with a reader = %v, %v", copyFR, got, err)
	}
}

// A home is its owner's until it disowns it, and is spared for another owner
// once no page of it is left: one whose pages others still use waits for
// them. A file has no more homes than it may.
func TestHomeOwners(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 2)
	first, second := NewHomeOwner(), NewHomeOwner()
	home, err := mf.ReserveHome(first)
	if err != nil {
		t.Fatal(err)
	}
	if got, owner, ok := mf.HomeOf(home.Start + 5*hostarch.PageSize); !ok || got != home || owner != first {
		t.Fatalf("HomeOf = %v, %d, %t", got, owner, ok)
	}
	used := slotRange(home, 10, 1)
	if _, err := mf.AllocateAt(used, AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatal(err)
	}
	mf.DisownHome(first, home)
	other, err := mf.ReserveHome(second)
	if err != nil {
		t.Fatal(err)
	}
	if other == home {
		t.Fatal("a home whose page is in use was reserved again")
	}
	if _, err := mf.ReserveHome(NewHomeOwner()); err == nil {
		t.Fatal("a file reserved more homes than it may")
	}
	mf.DecRef(used)
	waitForRelease(t, mf)
	again, err := mf.ReserveHome(NewHomeOwner())
	if err != nil || again != home {
		t.Fatalf("ReserveHome = %v, %v, want the spared home %v", again, err, home)
	}
}

// An export of the file's allocator, and its import, keep homes, their pages
// and their waste: the waste is released into free slots on the target.
func TestHomeExportImport(t *testing.T) {
	source := newHomeTestMemoryFile(t, 2)
	home, err := source.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	used, waste := slotRange(home, 0, 2), slotRange(home, 8, 1)
	for _, fr := range []memmap.FileRange{used, waste} {
		if _, err := source.AllocateAt(fr, AllocOpts{Kind: usage.Anonymous}); err != nil {
			t.Fatal(err)
		}
	}
	source.mu.Lock()
	source.holdReleaserLocked()
	source.mu.Unlock()
	source.DecRef(waste)
	metadata, _, _, err := ExportMemoryFileMetadataForRemote(context.Background(), source)
	source.mu.Lock()
	source.unholdReleaserLocked()
	source.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	target := newHomeTestMemoryFile(t, 2)
	if err := ImportMemoryFileMetadataForRemote(context.Background(), target, metadata); err != nil {
		t.Fatal(err)
	}
	defer target.DecRef(used)
	waitForRelease(t, target)
	target.mu.Lock()
	hc := target.homes[home.Start/chunkSize]
	pages := uint64(0)
	if hc != nil {
		pages = hc.pages
	}
	target.mu.Unlock()
	if hc == nil || pages != used.Length()/hostarch.PageSize {
		t.Fatalf("the imported home holds %d pages, want %d", pages, used.Length()/hostarch.PageSize)
	}
	if _, err := target.AllocateAt(waste, AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatalf("allocating the released waste's slot: %v", err)
	}
	target.DecRef(waste)
	if _, err := target.AllocateAt(used, AllocOpts{Kind: usage.Anonymous}); !errors.Is(err, ErrSlotsInUse) {
		t.Fatalf("allocating used slots after import: %v, want ErrSlotsInUse", err)
	}
}

// Waste that runs from one home into the next is released in one range, and
// each home counts its own part of it.
func TestHomeReleaseAcrossHomes(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 2)
	firstOwner, secondOwner := NewHomeOwner(), NewHomeOwner()
	first, err := mf.ReserveHome(firstOwner)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mf.ReserveHome(secondOwner)
	if err != nil {
		t.Fatal(err)
	}
	if second.Start != first.End {
		t.Fatalf("homes %v and %v are not adjacent", first, second)
	}
	last := memmap.FileRange{Start: first.End - hostarch.PageSize, End: first.End}
	next := memmap.FileRange{Start: second.Start, End: second.Start + hostarch.PageSize}
	for _, fr := range []memmap.FileRange{last, next} {
		if _, err := mf.AllocateAt(fr, AllocOpts{Kind: usage.Anonymous}); err != nil {
			t.Fatal(err)
		}
	}
	mf.DisownHome(firstOwner, first)
	mf.DisownHome(secondOwner, second)
	mf.mu.Lock()
	mf.holdReleaserLocked()
	mf.mu.Unlock()
	mf.DecRef(last)
	mf.DecRef(next)
	mf.mu.Lock()
	gap := mf.unwasteHome.FirstLargeEnoughGap(1)
	mf.unholdReleaserLocked()
	mf.mu.Unlock()
	if !gap.Ok() || gap.Range() != (memmap.FileRange{Start: last.Start, End: next.End}) {
		t.Fatalf("home waste is %v, want one range across the homes", gap.Range())
	}
	waitForRelease(t, mf)
	mf.mu.Lock()
	defer mf.mu.Unlock()
	for _, home := range []memmap.FileRange{first, second} {
		if pages := mf.homes[home.Start/chunkSize].pages; pages != 0 {
			t.Errorf("home %v counts %d pages after release, want 0", home, pages)
		}
	}
	if len(mf.freeHomes) != 2 {
		t.Errorf("%d homes are spared, want both", len(mf.freeHomes))
	}
}
