// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import "testing"

func TestApplyPlacementPolicyContractsAndConvertsFixedBranches(t *testing.T) {
	problem := FixedTerminalProblem{
		Mode: PlacementEvacuate, SourceNode: "a",
		Nodes: []NodeResource{{ID: "a", TargetBytes: 100}, {ID: "b", TargetBytes: 100}, {ID: "c", TargetBytes: 100}},
		Branches: []BranchResource{
			{ID: "f@x", NodeID: "a", Movable: true, AllowedNodes: []string{"a", "b", "c"}},
			{ID: "f@y", NodeID: "a", Movable: true, AllowedNodes: []string{"a", "b", "c"}},
		},
	}
	snapshots := []PlannerSnapshot{
		{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x"}, {ID: "f@y"}}},
		{NodeID: "b", Branches: []PlannerBranchSnapshot{{ID: "g@fixed"}}},
		{NodeID: "c"},
	}
	policy := PlacementPolicy{
		Version:        1,
		HardCoLocation: []PlacementPolicyGroup{{ID: "workers", Branches: []string{"f@x", "f@y"}}},
		SoftAffinity:   []PlacementPolicySoftAffinity{{ID: "reuse", Branches: []string{"f@x", "g@fixed"}, Weight: 50}},
	}
	if err := ApplyPlacementPolicy(&problem, snapshots, policy); err != nil {
		t.Fatal(err)
	}
	if problem.Branches[0].Cohort != "workers" || problem.Branches[1].Cohort != "workers" {
		t.Fatalf("hard cohort not applied: %+v", problem.Branches)
	}
	if len(problem.Affinities) != 1 || len(problem.Affinities[0].FixedNodes) != 1 || problem.Affinities[0].FixedNodes[0] != "b" {
		t.Fatalf("fixed branch was not converted to node terminal: %+v", problem.Affinities)
	}
}
