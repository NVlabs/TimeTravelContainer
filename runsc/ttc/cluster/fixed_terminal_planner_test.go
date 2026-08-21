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
	"reflect"
	"testing"
)

func TestFixedTerminalPressureUsesMultipleDestinations(t *testing.T) {
	const mib = uint64(1 << 20)
	problem := FixedTerminalProblem{
		Mode: PlacementRelievePressure, SourceNode: "node-a",
		Nodes: []NodeResource{
			{ID: "node-a", CurrentBytes: 1800 * mib, TargetBytes: 1200 * mib},
			{ID: "node-b", CurrentBytes: 700 * mib, TargetBytes: 1400 * mib},
			{ID: "node-c", CurrentBytes: 700 * mib, TargetBytes: 1400 * mib},
		},
		Branches: []BranchResource{
			{ID: "family@a1", NodeID: "node-a", PrivateBytes: 100 * mib, Movable: true},
			{ID: "family@a2", NodeID: "node-a", PrivateBytes: 100 * mib, Movable: true},
			{ID: "family@b", NodeID: "node-a", PrivateBytes: 300 * mib, Movable: true},
			{ID: "family@c", NodeID: "node-a", PrivateBytes: 300 * mib, Movable: true},
		},
		Shared: []SharedResource{
			{ID: "b-runtime", Bytes: 300 * mib, Branches: []string{"family@b"}, FixedNodes: []string{"node-b"}},
			{ID: "c-runtime", Bytes: 300 * mib, Branches: []string{"family@c"}, FixedNodes: []string{"node-c"}},
			{ID: "a-runtime", Bytes: 400 * mib, Branches: []string{"family@a1", "family@a2"}},
		},
	}
	plan, err := PlanFixedTerminals(problem)
	if err != nil {
		t.Fatal(err)
	}
	want := []PlacementMove{
		{Branch: "family@b", FromNode: "node-a", ToNode: "node-b"},
		{Branch: "family@c", FromNode: "node-a", ToNode: "node-c"},
	}
	if !reflect.DeepEqual(plan.Moves, want) {
		t.Fatalf("moves=%+v, want %+v", plan.Moves, want)
	}
	if got := plan.NodeAfterBytes["node-a"]; got != 1200*mib {
		t.Fatalf("source after=%d, want %d", got, 1200*mib)
	}
}

func TestFixedTerminalEvacuationForbidsSourceAndHonorsCapacity(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementEvacuate, SourceNode: "a",
		Nodes: []NodeResource{
			{ID: "a", CurrentBytes: 230, TargetBytes: 200},
			{ID: "b", CurrentBytes: 80, TargetBytes: 150},
			{ID: "c", CurrentBytes: 50, TargetBytes: 200},
		},
		Branches: []BranchResource{
			{ID: "x", NodeID: "a", PrivateBytes: 60, Movable: true},
			{ID: "y", NodeID: "a", PrivateBytes: 80, Movable: true},
		},
		Shared: []SharedResource{{ID: "x-on-b", Bytes: 10, Branches: []string{"x"}, FixedNodes: []string{"b"}}},
	}
	plan, err := PlanFixedTerminals(problem)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("moves=%+v", plan.Moves)
	}
	for _, move := range plan.Moves {
		if move.ToNode == "a" {
			t.Fatalf("evacuation retained move on source: %+v", move)
		}
	}
	if plan.NodeAfterBytes["b"] > 150 || plan.NodeAfterBytes["c"] > 200 {
		t.Fatalf("overfull plan: %+v", plan.NodeAfterBytes)
	}
}

func TestFixedTerminalHardCohortIsNeverSplit(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementEvacuate, SourceNode: "a",
		Nodes: []NodeResource{
			{ID: "a", CurrentBytes: 100, TargetBytes: 100},
			{ID: "b", CurrentBytes: 0, TargetBytes: 100},
			{ID: "c", CurrentBytes: 0, TargetBytes: 100},
		},
		Branches: []BranchResource{
			{ID: "x", NodeID: "a", PrivateBytes: 40, Movable: true, Cohort: "pair"},
			{ID: "y", NodeID: "a", PrivateBytes: 40, Movable: true, Cohort: "pair"},
		},
	}
	plan, err := PlanFixedTerminals(problem)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) != 2 || plan.Moves[0].ToNode != plan.Moves[1].ToNode {
		t.Fatalf("cohort was split: %+v", plan.Moves)
	}
	if plan.Moves[0].ToNode != "b" {
		t.Fatalf("tie was not deterministic: %+v", plan.Moves)
	}
}

func TestFixedTerminalRetainedPinPreventsFalseSourceFree(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementRelievePressure, SourceNode: "a",
		Nodes: []NodeResource{
			{ID: "a", CurrentBytes: 200, TargetBytes: 100},
			{ID: "b", CurrentBytes: 0, TargetBytes: 500},
		},
		Branches: []BranchResource{{ID: "x", NodeID: "a", PrivateBytes: 10, Movable: true}},
		Shared:   []SharedResource{{ID: "retained", Bytes: 100, Branches: []string{"x"}, FixedNodes: []string{"a"}}},
	}
	if _, err := PlanFixedTerminals(problem); err == nil {
		t.Fatal("planner falsely treated a fixed source pin as reclaimable")
	}
}

func TestFixedTerminalSoftAffinityRefinesDestination(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementEvacuate, SourceNode: "a",
		Nodes: []NodeResource{
			{ID: "a", CurrentBytes: 10, TargetBytes: 10},
			{ID: "b", CurrentBytes: 0, TargetBytes: 100},
			{ID: "c", CurrentBytes: 0, TargetBytes: 100},
		},
		Branches:   []BranchResource{{ID: "x", NodeID: "a", PrivateBytes: 10, Movable: true}},
		Affinities: []AffinityResource{{ID: "prefer-c", Weight: 50, Branches: []string{"x"}, FixedNodes: []string{"c"}}},
	}
	plan, err := PlanFixedTerminals(problem)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].ToNode != "c" {
		t.Fatalf("soft terminal affinity ignored: %+v", plan.Moves)
	}
}

func TestFixedTerminalPlanIsDeterministic(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementEvacuate, SourceNode: "a",
		Nodes: []NodeResource{
			{ID: "c", CurrentBytes: 0, TargetBytes: 100},
			{ID: "a", CurrentBytes: 20, TargetBytes: 20},
			{ID: "b", CurrentBytes: 0, TargetBytes: 100},
		},
		Branches: []BranchResource{
			{ID: "y", NodeID: "a", PrivateBytes: 10, Movable: true},
			{ID: "x", NodeID: "a", PrivateBytes: 10, Movable: true},
		},
	}
	first, err := PlanFixedTerminals(problem)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := PlanFixedTerminals(problem)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d got %+v, first %+v", i, got, first)
		}
	}
}
