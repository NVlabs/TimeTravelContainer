// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"maps"
	"slices"
	"testing"
)

func TestAuthorityQuerySelectsOnlyItsRecords(t *testing.T) {
	state := NewAuthorityState()
	state.Revision = 7
	state.SharedGC = SharedGCLease{NodeID: "node-a", Generation: 3}
	for _, key := range []string{logicalBranchKey("fam-a", "main"), logicalBranchKey("fam-a", "b1"), logicalBranchKey("fam-b", "main")} {
		state.Placements[key] = PlacementRecord{OwnerNode: key}
		state.Branches[key] = BranchAuthority{Generation: 1}
	}
	state.Nodes["node-a"] = NodeRecord{NodeID: "node-a"}
	state.Nodes["node-b"] = NodeRecord{NodeID: "node-b"}
	state.Transactions["tx-a"] = Transaction{}
	state.VolatileOrigins[4] = SnapshotOriginProvider{}
	state.VolatileOrigins[5] = SnapshotOriginProvider{}
	state.Requests["r-a"] = appliedAuthorityRequest{}
	state.RequestOrder = []string{"r-a"}
	fsm := &AuthorityFSM{state: state}

	keys := func(state *AuthorityState) map[string][]string {
		got := map[string][]string{
			"nodes":        slices.Sorted(maps.Keys(state.Nodes)),
			"placements":   slices.Sorted(maps.Keys(state.Placements)),
			"branches":     slices.Sorted(maps.Keys(state.Branches)),
			"transactions": slices.Sorted(maps.Keys(state.Transactions)),
		}
		for key, value := range got {
			if len(value) == 0 {
				delete(got, key)
			}
		}
		return got
	}
	main := logicalBranchKey("fam-a", "main")
	for _, test := range []struct {
		name  string
		query AuthorityQuery
		want  map[string][]string
	}{
		{"nothing", AuthorityQuery{}, map[string][]string{}},
		{"one placement and its branch", AuthorityQuery{Placements: Only(main), Branches: Only(main)},
			map[string][]string{"placements": {main}, "branches": {main}}},
		{"a missing key", AuthorityQuery{Nodes: Only("node-c"), Transactions: Only("tx-a")},
			map[string][]string{"transactions": {"tx-a"}}},
		{"one family's", AuthorityQuery{Placements: All[string](), Nodes: All[string](), Family: "fam-a"},
			map[string][]string{
				"placements": {logicalBranchKey("fam-a", "b1"), main},
				"nodes":      {"node-a", "node-b"},
			}},
	} {
		got, err := fsm.query(test.query)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if !maps.EqualFunc(keys(got), test.want, slices.Equal) {
			t.Errorf("%s: got %v, want %v", test.name, keys(got), test.want)
		}
		if got.Revision != 7 || got.SharedGC.Generation != 3 || len(got.Requests) != 0 || len(got.RequestOrder) != 0 {
			t.Errorf("%s: counters, lease or requests wrong: %d, %+v, %v, %v", test.name, got.Revision, got.SharedGC, got.Requests, got.RequestOrder)
		}
	}

	whole, err := fsm.copyState()
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Placements) != 3 || len(whole.Nodes) != 2 || len(whole.VolatileOrigins) != 2 || len(whole.Requests) != 0 {
		t.Fatalf("the whole state has %d placements, %d nodes, %d origins, %d requests", len(whole.Placements), len(whole.Nodes), len(whole.VolatileOrigins), len(whole.Requests))
	}
	// A read is a copy.
	record := whole.Placements[main]
	record.OwnerNode = "changed"
	whole.Placements[main] = record
	if state.Placements[main].OwnerNode != main {
		t.Fatal("changing a read changed the state")
	}
}

func TestAuthorityQueryOwnerKeepsTheNodesPlacements(t *testing.T) {
	state := NewAuthorityState()
	mine, theirs := logicalBranchKey("fam-a", "main"), logicalBranchKey("fam-b", "main")
	state.Placements[mine] = PlacementRecord{FamilyID: "fam-a", BranchID: "main", OwnerNode: "node-a"}
	state.Placements[theirs] = PlacementRecord{FamilyID: "fam-b", BranchID: "main", OwnerNode: "node-b"}
	fsm := &AuthorityFSM{state: state}
	got, err := fsm.query(ExecutionGrantsOf("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Placements) != 1 || got.Placements[mine].OwnerNode != "node-a" {
		t.Fatalf("node-a's grants have placements %v", got.Placements)
	}
	// A revoked grant's placement is read whoever owns it now.
	got, err = fsm.query(RevokedGrants([]string{theirs}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Placements) != 1 || got.Placements[theirs].OwnerNode != "node-b" {
		t.Fatalf("a revoked grant reads placements %v", got.Placements)
	}
}
