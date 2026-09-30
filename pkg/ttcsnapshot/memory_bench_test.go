// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"fmt"
	"testing"
)

// benchmarkMemoryImage returns a store whose tree "image/memory" maps tables
// tables of 256 pages, each table's in a chunk of its own, with the tables
// and the pages' identities, and the reuse a capture of a local node makes.
func benchmarkMemoryImage(b *testing.B, tables int) (*Store, []NewMemoryTable, []MemoryPageIdentity, MemoryReuse) {
	b.Helper()
	store, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		b.Fatal(err)
	}
	kinds := make([]ObjectKind, tables)
	for i := range kinds {
		kinds[i] = ObjectKindMemoryChunk
	}
	chunks, err := store.ReserveObjects(origin, kinds)
	if err != nil {
		b.Fatal(err)
	}
	image := make([]NewMemoryTable, 0, tables)
	identities := make([]MemoryPageIdentity, 0, tables*PagesPerMemoryTable)
	for table := 0; table < tables; table++ {
		entries := make([]MemoryTableEntry, PagesPerMemoryTable)
		for slot := range entries {
			sequence := uint64(table*PagesPerMemoryTable + slot)
			identity := MemoryPageIdentity{LogicalAllocator: [16]byte{1}, LogicalSequence: sequence, VersionAllocator: [16]byte{2}, VersionSequence: sequence, References: 1}
			entries[slot] = MemoryTableEntry{Slot: uint8(slot), Location: MemoryPageLocation{Chunk: chunks[table], Page: uint8(slot)}, Identity: identity}
			identities = append(identities, identity)
		}
		image = append(image, NewMemoryTable{Root: "image/memory", Index: uint64(table * PagesPerMemoryTable), Entries: entries})
	}
	for begin := 0; begin < tables; begin += 128 {
		end := min(begin+128, tables)
		if err := store.CommitMemoryTables(origin, image[begin:end], chunks[begin:end]); err != nil {
			b.Fatal(err)
		}
	}
	if err := store.Seal(origin, SnapshotSealedLocal); err != nil {
		b.Fatal(err)
	}
	return store, image, identities, MemoryReuse{States: []SnapshotState{SnapshotWorking, SnapshotSealedLocal}}
}

// BenchmarkHoldMemoryPages finds and holds the pages of 1.5 GiB the graph
// holds, as a capture of a branch that has not changed since its last does.
func BenchmarkHoldMemoryPages(b *testing.B) {
	store, _, identities, reuse := benchmarkMemoryImage(b, 1550)
	defer store.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, pins, err := store.HoldMemoryPagesReusing("held", identities, reuse)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := store.DeletePins(pins); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

// BenchmarkCommitSharedMemoryTables maps the tables of 1.5 GiB the graph
// holds in a tree of their own, sharing every one, as a capture of a branch
// that has not changed since its last does.
func BenchmarkCommitSharedMemoryTables(b *testing.B) {
	store, image, _, reuse := benchmarkMemoryImage(b, 1550)
	defer store.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		origin, err := store.AllocateWorking()
		if err != nil {
			b.Fatal(err)
		}
		root := fmt.Sprintf("copy-%d/memory", i)
		tables := make([]NewMemoryTable, len(image))
		for index, table := range image {
			table.Root = root
			tables[index] = table
		}
		b.StartTimer()
		for begin := 0; begin < len(tables); begin += 128 {
			if err := store.CommitMemoryTablesReusing(origin, tables[begin:min(begin+128, len(tables))], nil, reuse); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		if err := store.ReleaseRoots([]string{root}); err != nil {
			b.Fatal(err)
		}
		for {
			collected, err := store.Collect(4096)
			if err != nil {
				b.Fatal(err)
			}
			if collected == 0 {
				break
			}
		}
		b.StartTimer()
	}
}
