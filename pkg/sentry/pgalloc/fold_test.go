// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"errors"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// pageIdentities returns the identities and versions of fr's pages.
func pageIdentities(t *testing.T, mf *MemoryFile, fr memmap.FileRange) ([]LogicalPageID, []PageVersionID) {
	t.Helper()
	var ids []LogicalPageID
	var versions []PageVersionID
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		id, version, ok := mf.LogicalPageVersionAt(off)
		if !ok {
			t.Fatalf("page %#x has no identity", off)
		}
		ids = append(ids, id)
		versions = append(versions, version)
	}
	return ids, versions
}

// A page that its holder alone holds moves to free slots of a home with its
// contents, identity and version; one that something else holds does not, nor
// one whose destination is taken. Its old slot is free once its holder lets it
// go.
func TestMovePages(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	owner := NewHomeOwner()
	home, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	layer, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	opts := AllocOpts{Kind: usage.Anonymous}
	src, err := mf.AllocateAt(slotRange(layer, 8, 4), opts)
	if err != nil {
		t.Fatal(err)
	}
	fillPages(t, mf, src, 7)
	ids, versions := pageIdentities(t, mf, src)
	dst := slotRange(home, 8, 4)

	// Another reference, or a device's DMA, keeps the pages where they are.
	mf.IncRef(src, 0)
	if err := mf.MovePages(src, dst); !errors.Is(err, ErrNotMovable) {
		t.Errorf("MovePages of pages with two references: %v, want ErrNotMovable", err)
	}
	mf.DecRef(src)
	mf.PinForDMA(src)
	if err := mf.MovePages(src, dst); !errors.Is(err, ErrNotMovable) {
		t.Errorf("MovePages of pages pinned for DMA: %v, want ErrNotMovable", err)
	}
	mf.UnpinForDMA(src)

	// A taken slot keeps them too, and nothing is allocated.
	taken, err := mf.AllocateAt(slotRange(home, 11, 1), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := mf.MovePages(src, dst); !errors.Is(err, ErrSlotsInUse) {
		t.Errorf("MovePages to a taken slot: %v, want ErrSlotsInUse", err)
	}
	if free := mf.FreeSlots(dst); len(free) != 1 || free[0] != slotRange(home, 8, 3) {
		t.Errorf("FreeSlots(%v) = %v, want the 3 slots before the taken one", dst, free)
	}
	mf.DecRef(taken)
	waitForRelease(t, mf)

	if err := mf.MovePages(src, dst); err != nil {
		t.Fatalf("MovePages: %v", err)
	}
	if !pagesHold(t, mf, dst, 7) {
		t.Error("the moved pages lost their contents")
	}
	gotIDs, gotVersions := pageIdentities(t, mf, dst)
	for i := range ids {
		if gotIDs[i] != ids[i] || gotVersions[i] != versions[i] {
			t.Errorf("page %d was %v version %v, is %v version %v", i, ids[i], versions[i], gotIDs[i], gotVersions[i])
		}
	}
	mf.DecRef(src)
	waitForRelease(t, mf)
	if free := mf.FreeSlots(src); len(free) != 1 || free[0] != src {
		t.Errorf("the old slots %v are not free once let go: %v", src, free)
	}
	mf.DecRef(dst)
}

// A 2 MiB page moved to an aligned slot stays a 2 MiB page, with its identity
// and version.
func TestMovePages2MiB(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	owner := NewHomeOwner()
	home, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	layer, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	src, err := mf.AllocateAt(slotRange(layer, pagesPerHugePage(), pagesPerHugePage()), AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true})
	if err != nil {
		t.Fatal(err)
	}
	if !mf.IsHugePages(src) {
		t.Fatal("the allocation is not a 2 MiB page")
	}
	fillPages(t, mf, src, 9)
	ids, versions := pageIdentities(t, mf, src)
	dst := slotRange(home, pagesPerHugePage(), pagesPerHugePage())
	if err := mf.MovePages(src, dst); err != nil {
		t.Fatalf("MovePages: %v", err)
	}
	mf.DecRef(src)
	if !mf.IsHugePages(dst) {
		t.Error("the moved 2 MiB page is not one")
	}
	if !pagesHold(t, mf, dst, 9) {
		t.Error("the moved page lost its contents")
	}
	gotIDs, gotVersions := pageIdentities(t, mf, dst)
	for i := range ids {
		if gotIDs[i] != ids[i] || gotVersions[i] != versions[i] {
			t.Fatalf("base page %d was %v version %v, is %v version %v", i, ids[i], versions[i], gotIDs[i], gotVersions[i])
		}
	}
	mf.DecRef(dst)
}

// A home whose owner gave it up while others still use its pages is an orphan,
// which another may adopt, and then no other.
func TestAdoptHome(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	first, second, third := NewHomeOwner(), NewHomeOwner(), NewHomeOwner()
	home, err := mf.ReserveHome(first)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := mf.AllocateAt(slotRange(home, 0, 1), AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	if err := mf.AdoptHome(second, home); err == nil {
		t.Error("AdoptHome took a home that has an owner")
	}
	mf.DisownHome(first, home)
	if err := mf.AdoptHome(second, home); err != nil {
		t.Fatalf("AdoptHome of an orphan: %v", err)
	}
	if _, owner, _ := mf.HomeOf(home.Start); owner != second {
		t.Errorf("the adopted home's owner is %d, want %d", owner, second)
	}
	if err := mf.AdoptHome(third, home); err == nil {
		t.Error("AdoptHome took a home that was adopted")
	}
	mf.DecRef(fr)
	mf.DisownHome(second, home)
}
