// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"fmt"
	"testing"
)

func TestImmutableCatalogReadsMemoryWithoutOwningReferences(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	root := commitTestMemory(t, store, origin, "memory/root", 123)
	want, err := store.MemoryPageEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.SnapshotObjects(origin)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewImmutableCatalog(objects)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.MemoryPageEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != want[0] || entries[0].Index != 123 || entries[0].Identity != testPageIdentity(123) {
		t.Fatalf("catalog entries = %+v, want %+v", entries, want)
	}
	if object, ok := catalog.Object(root); !ok || object.Refs == 0 {
		t.Fatalf("catalog did not preserve immutable metadata: (%+v,%t)", object, ok)
	}
}

func TestResolveImmutableCatalogFollowsOriginEdgesOnce(t *testing.T) {
	old := ObjectID{Snapshot: 1, Kind: ObjectKindFile, Ordinal: 1}
	root := ObjectID{Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 1}
	sections := map[SnapshotID][]Object{
		1: {{ID: old, State: ObjectLive, Refs: 7, BulkLength: 4096}},
		2: {{ID: root, State: ObjectLive, Refs: 1, Children: []Edge{{Slot: 0, Child: old}}}},
	}
	loads := make(map[SnapshotID]int)
	catalog, err := ResolveImmutableCatalog([]ObjectID{root, root}, func(origin SnapshotID) ([]Object, error) {
		loads[origin]++
		return sections[origin], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if loads[1] != 1 || loads[2] != 1 {
		t.Fatalf("origin loads = %v, want each exactly once", loads)
	}
	if _, ok := catalog.Object(root); !ok {
		t.Fatal("resolved catalog omitted root origin")
	}
	if object, ok := catalog.Object(old); !ok || object.BulkLength != 4096 {
		t.Fatalf("resolved catalog old object = (%+v, %t)", object, ok)
	}
}

func TestReachableObjectsExcludesUnrelatedOriginObjects(t *testing.T) {
	leaf := Object{ID: ObjectID{Snapshot: 1, Kind: ObjectKindFile, Ordinal: 1}, State: ObjectLive, Refs: 1, BulkLength: 4}
	root := Object{ID: ObjectID{Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 1}, State: ObjectLive, Refs: 1, Children: []Edge{{Slot: 0, Child: leaf.ID}}}
	unrelated := Object{ID: ObjectID{Snapshot: 2, Kind: ObjectKindFile, Ordinal: 2}, State: ObjectLive, Refs: 1, BulkLength: 9}
	catalog, err := NewImmutableCatalog([]Object{leaf, root, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := ReachableObjects(catalog, []ObjectID{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[0].ID != leaf.ID || objects[1].ID != root.ID {
		t.Fatalf("reachable objects = %+v, want leaf and root only", objects)
	}
}

func TestImmutableCatalogReadsFilesystemWithoutOwningReferences(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.CreateObject(origin, "file", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.ApplyFilesystem(origin, "filesystem/main", []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}},
		{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, Size: 12, File: file}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "hello", Inode: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.SnapshotObjects(origin)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewImmutableCatalog(objects)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok, err := catalog.FilesystemLookup(root, 1, "hello")
	if err != nil || !ok || entry.Inode != 2 {
		t.Fatalf("lookup = (%+v, %t, %v), want inode 2", entry, ok, err)
	}
	inode, ok, err := catalog.FilesystemInode(root, entry.Inode)
	if err != nil || !ok || inode.File != file || inode.Size != 12 {
		t.Fatalf("inode = (%+v, %t, %v), want file %s", inode, ok, err, file)
	}
	entries, err := catalog.FilesystemDirectory(root, 1)
	if err != nil || len(entries) != 1 || entries[0] != entry {
		t.Fatalf("directory = (%+v, %v), want [%+v]", entries, err, entry)
	}
}

func TestLazyImmutableCatalogLoadsOnlyLookupPath(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	ops := []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}}}
	for index := uint64(0); index < 400; index++ {
		file, err := store.CreateObject(origin, fmt.Sprintf("file/%04d", index), ObjectKindFile, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		inode := index + 2
		ops = append(ops,
			FilesystemOperation{PutInode: &InodeRecord{Number: inode, Type: "regular", Mode: 0644, Nlink: 1, Size: int64(index), File: file}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%04d", index), Inode: inode}},
		)
	}
	root, err := store.ApplyFilesystem(origin, "filesystem/main", ops)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.SnapshotObjects(origin)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[ObjectID]Object, len(objects))
	for _, object := range objects {
		byID[object.ID] = object
	}
	loads := 0
	catalog, err := NewLazyImmutableCatalog(func(id ObjectID) (Object, error) {
		loads++
		object, ok := byID[id]
		if !ok {
			return Object{}, fmt.Errorf("missing %s", id)
		}
		return object, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok, err := catalog.FilesystemLookup(root, 1, "file-0200")
	if err != nil || !ok || entry.Inode != 202 {
		t.Fatalf("lookup = (%+v,%t,%v), want inode 202", entry, ok, err)
	}
	inode, ok, err := catalog.FilesystemInode(root, entry.Inode)
	if err != nil || !ok || inode.Number != 202 {
		t.Fatalf("inode = (%+v,%t,%v), want 202", inode, ok, err)
	}
	if loads != catalog.LoadedObjects() {
		t.Fatalf("loader calls=%d, cached=%d", loads, catalog.LoadedObjects())
	}
	if loads >= len(objects)/4 {
		t.Fatalf("point lookup loaded %d of %d metadata objects", loads, len(objects))
	}
	// A repeated lookup is served entirely from the immutable local cache.
	before := loads
	if _, _, err := catalog.FilesystemLookup(root, 1, "file-0200"); err != nil {
		t.Fatal(err)
	}
	if loads != before {
		t.Fatalf("repeated lookup issued %d additional loads", loads-before)
	}
}
