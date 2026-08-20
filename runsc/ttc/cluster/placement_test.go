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

package cluster

import (
	"sync"
	"testing"
	"time"
)

func TestPlacementMigrationFencesConcurrentCutovers(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "physical-a", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		op  string
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, operation := range []string{"move-one", "move-two"} {
		go func(operation string) {
			start.Wait()
			_, err := store.BeginMigration("family", "main", "node-a", "boot-a", created.Generation, operation, "node-b", now.Add(time.Second))
			results <- result{operation, err}
		}(operation)
	}
	start.Done()
	winner := ""
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("two migration operations acquired the same generation")
			}
			winner = result.op
		}
	}
	if winner == "" {
		t.Fatal("no migration operation acquired the placement")
	}
	committed, err := store.CommitMigration("family", "main", winner, "node-b", "boot-b", "physical-b", "main", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if committed.OwnerNode != "node-b" || committed.Generation != 2 || committed.State != PlacementRunning {
		t.Fatalf("committed placement=%+v", committed)
	}
	if _, err := store.BeginMigration("family", "main", "node-a", "boot-a", 1, "stale", "node-c", now.Add(3*time.Second)); err == nil {
		t.Fatal("stale owner acquired migrated placement")
	}
}

func TestPlacementAdoptsRestartedOwnerIncarnation(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-old", State: PlacementPaused,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := store.AdoptOwnerIncarnation("family", "main", "node-a", "boot-old", "boot-new", created.Generation, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if adopted.OwnerIncarnation != "boot-new" || adopted.Generation != created.Generation+1 || adopted.State != PlacementPaused {
		t.Fatalf("adopted=%+v", adopted)
	}
	if _, err := store.AdoptOwnerIncarnation("family", "main", "node-a", "boot-old", "boot-stale", created.Generation, now.Add(2*time.Second)); err == nil {
		t.Fatal("stale incarnation adopted placement")
	}
}
