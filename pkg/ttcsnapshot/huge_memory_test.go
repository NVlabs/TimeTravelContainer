// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"testing"
)

// hugeHalfEntries returns the entries of half of the 2 MiB page with identity
// page, whose bytes chunk holds.
func hugeHalfEntries(page MemoryPageIdentity, half uint64, chunk ObjectID) []MemoryTableEntry {
	entries := make([]MemoryTableEntry, PagesPerMemoryTable)
	for slot := range entries {
		identity := page
		identity.LogicalSequence += half*PagesPerMemoryTable + uint64(slot)
		identity.VersionSequence += half*PagesPerMemoryTable + uint64(slot)
		entries[slot] = MemoryTableEntry{
			Slot: uint8(slot), Location: MemoryPageLocation{Chunk: chunk, Page: uint8(slot), Huge: true}, Identity: identity, Huge: true,
		}
	}
	return entries
}

// A 2 MiB page is mapped by a table of each of its halves, each of which
// records its identity once and refers to the chunk of its half; the tree
// lists its base pages, marked, with the identities they derive. A capture
// of the page shares its halves, and one of its base pages, once it is
// split, the pages of its halves; but one of a 2 MiB page finds no half
// where the graph holds only base pages.
func TestHugePageTablesMapA2MiBPage(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := store.ReserveObjects(origin, []ObjectKind{ObjectKindMemoryChunk, ObjectKindMemoryChunk, ObjectKindMemoryChunk})
	if err != nil {
		t.Fatal(err)
	}
	page := MemoryPageIdentity{LogicalAllocator: [16]byte{1}, LogicalSequence: 3 * pagesPerHugePage, VersionAllocator: [16]byte{2}, VersionSequence: 5 * pagesPerHugePage, References: 1}
	const index = 7 * pagesPerHugePage
	before := store.Stats().Objects
	if err := store.CommitMemoryTables(origin, []NewMemoryTable{
		{Root: "memory/a", Index: index, Entries: hugeHalfEntries(page, 0, chunks[0])},
		{Root: "memory/a", Index: index + PagesPerMemoryTable, Entries: hugeHalfEntries(page, 1, chunks[1])},
	}, chunks[:2]); err != nil {
		t.Fatal(err)
	}
	// Two chunks, two tables, and five nodes above them.
	if got := store.Stats().Objects - before; got != 2+2+memoryTreeLevels-1 {
		t.Fatalf("mapping a 2 MiB page created %d objects", got)
	}
	check := func(store *Store) {
		t.Helper()
		entries := memoryEntriesByIndex(t, store, "memory/a")
		if len(entries) != pagesPerHugePage {
			t.Fatalf("memory/a maps %d pages, want %d", len(entries), pagesPerHugePage)
		}
		for half := uint64(0); half < 2; half++ {
			for _, want := range hugeHalfEntries(page, half, chunks[half]) {
				got := entries[index+half*PagesPerMemoryTable+uint64(want.Slot)]
				if !got.Huge || got.Location != want.Location || got.Identity != want.Identity {
					t.Fatalf("half %d slot %d is %+v, want %+v", half, want.Slot, got, want)
				}
			}
		}
		halves := 0
		for _, object := range store.Inspect().Objects {
			if object.ID.Kind == ObjectKindMemoryTable && object.Data[0] == memoryTableFormatHugeHalf {
				if len(object.Data) != memoryTableHugeHalfSize {
					t.Fatalf("a table of a half of a 2 MiB page has %d bytes, want %d", len(object.Data), memoryTableHugeHalfSize)
				}
				halves++
			}
		}
		if halves != 2 {
			t.Fatalf("the graph has %d tables of halves of 2 MiB pages, want 2", halves)
		}
	}
	check(store)

	// The halves are shared whole by a 2 MiB page, and page by page by base
	// pages.
	identities := make([]MemoryPageIdentity, 0, pagesPerHugePage)
	huge := make([]bool, 0, pagesPerHugePage)
	for half := uint64(0); half < 2; half++ {
		for _, entry := range hugeHalfEntries(page, half, chunks[half]) {
			identities = append(identities, entry.Identity)
			huge = append(huge, true)
		}
	}
	locations, pins, err := store.HoldMemoryPagesOfReusing("pins/huge", identities, huge, MemoryReuse{})
	if err != nil {
		t.Fatal(err)
	}
	for i, location := range locations {
		if want := (MemoryPageLocation{Chunk: chunks[i/PagesPerMemoryTable], Page: uint8(i % PagesPerMemoryTable), Huge: true}); location != want {
			t.Fatalf("base page %d of the 2 MiB page is held at %+v, want %+v", i, location, want)
		}
	}
	if err := store.DeletePins(pins); err != nil {
		t.Fatal(err)
	}
	// Once the 2 MiB page is split, its base pages, which keep its
	// identities, share the pages of its halves in a table of the first
	// format.
	if locations, pins, err = store.HoldMemoryPagesReusing("pins/base", identities[:PagesPerMemoryTable], MemoryReuse{}); err != nil {
		t.Fatal(err)
	}
	split := make([]MemoryTableEntry, PagesPerMemoryTable)
	for slot, location := range locations {
		if want := (MemoryPageLocation{Chunk: chunks[0], Page: uint8(slot)}); location != want {
			t.Fatalf("base page %d of the split page is held at %+v, want %+v", slot, location, want)
		}
		split[slot] = MemoryTableEntry{Slot: uint8(slot), Location: location, Identity: identities[slot]}
	}
	before = store.Stats().Objects
	if err := store.CommitMemoryTables(origin, []NewMemoryTable{{Root: "memory/split", Index: 0, Entries: split}}, nil); err != nil {
		t.Fatalf("the base pages of a split 2 MiB page do not share its half: %v", err)
	}
	if err := store.DeletePins(pins); err != nil {
		t.Fatal(err)
	}
	// A table and the nodes above it.
	if got := store.Stats().Objects - before; got != 1+memoryTreeLevels-1 {
		t.Fatalf("mapping the base pages of a half of a 2 MiB page created %d objects", got)
	}
	checkSplit := func(store *Store) {
		t.Helper()
		entries := memoryEntriesByIndex(t, store, "memory/split")
		if len(entries) != PagesPerMemoryTable {
			t.Fatalf("memory/split maps %d pages, want %d", len(entries), PagesPerMemoryTable)
		}
		for index, entry := range entries {
			if entry.Huge || entry.Location != (MemoryPageLocation{Chunk: chunks[0], Page: uint8(index)}) || entry.Identity != identities[index] {
				t.Fatalf("base page %d of the split page is %+v", index, entry)
			}
		}
	}
	checkSplit(store)

	// Base pages of another page, in a table of the first format: a 2 MiB
	// page of their identities finds no half of it.
	other := page
	other.LogicalSequence += pagesPerHugePage
	base := make([]MemoryTableEntry, PagesPerMemoryTable)
	otherIdentities := make([]MemoryPageIdentity, PagesPerMemoryTable)
	for slot := range base {
		identity := other
		identity.LogicalSequence += uint64(slot)
		identity.VersionSequence += uint64(slot)
		base[slot] = MemoryTableEntry{Slot: uint8(slot), Location: MemoryPageLocation{Chunk: chunks[2], Page: uint8(slot)}, Identity: identity}
		otherIdentities[slot] = identity
	}
	if err := store.CommitMemoryTables(origin, []NewMemoryTable{{Root: "memory/b", Index: 0, Entries: base}}, chunks[2:]); err != nil {
		t.Fatal(err)
	}
	otherHuge := make([]bool, len(otherIdentities))
	for i := range otherHuge {
		otherHuge[i] = true
	}
	if locations, pins, err = store.HoldMemoryPagesOfReusing("pins/other", otherIdentities, otherHuge, MemoryReuse{}); err != nil || !locations[0].Chunk.IsZero() {
		t.Fatalf("a 2 MiB page found base pages at %+v (%v)", locations[0], err)
	}
	if err := store.DeletePins(pins); err != nil {
		t.Fatal(err)
	}

	// A half whose pages do not continue its page's identity is refused.
	bad := hugeHalfEntries(page, 0, chunks[0])
	bad[9].Identity.VersionSequence++
	if err := store.CommitMemoryTables(origin, []NewMemoryTable{{Root: "memory/c", Index: index, Entries: bad}}, nil); err == nil {
		t.Fatal("a half of a 2 MiB page with a page of another version was committed")
	}
	mixed := hugeHalfEntries(page, 0, chunks[0])
	mixed[5].Huge, mixed[5].Location.Huge = false, false
	if err := store.CommitMemoryTables(origin, []NewMemoryTable{{Root: "memory/c", Index: index, Entries: mixed[5:]}}, nil); err == nil {
		t.Fatal("base pages among pages of a 2 MiB page were committed")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err = Open(directory); err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	check(store)
	checkSplit(store)
}
