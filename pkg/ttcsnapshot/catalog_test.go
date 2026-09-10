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

package ttcsnapshot

import "testing"

func TestImmutableCatalogReadsMemoryWithoutOwningReferences(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	identity := MemoryPageIdentity{LogicalSequence: 7, VersionSequence: 9, References: 2}
	identity.LogicalAllocator[0] = 1
	identity.VersionAllocator[0] = 2
	reservation, err := store.ReserveMemoryChunkPages(origin, "capture", []MemoryPageDescriptor{{Slot: 3, Identity: identity}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitMemoryChunk(reservation); err != nil {
		t.Fatal(err)
	}
	page := ObjectID(reservation.Pages[3])
	root, err := store.SetMemoryPage(origin, "memory/root", 123, page)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins(reservation.Pins); err != nil {
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
	pages, err := catalog.MemoryPages(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := pages[123]; got != page || len(pages) != 1 {
		t.Fatalf("catalog pages=%v, want page 123=%s", pages, page)
	}
	chunk, offset, gotIdentity, err := catalog.MemoryPageRecord(page)
	if err != nil {
		t.Fatal(err)
	}
	if ObjectID(chunk) != ObjectID(reservation.Chunk) || offset != 3*memoryPageBytes || gotIdentity != identity {
		t.Fatalf("catalog page=(%s,%d,%+v), want (%s,%d,%+v)", ObjectID(chunk), offset, gotIdentity, ObjectID(reservation.Chunk), 3*memoryPageBytes, identity)
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
