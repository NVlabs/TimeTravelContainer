// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildFixedTerminalProblemIntersectsVersionRuns(t *testing.T) {
	const allocator = "000102030405060708090a0b0c0d0e0f"
	problem, err := BuildFixedTerminalProblem(
		PlacementRelievePressure,
		"a",
		[]NodeResource{{ID: "a", CurrentBytes: 1000, TargetBytes: 500}, {ID: "b", CurrentBytes: 100, TargetBytes: 1000}},
		[]PlannerSnapshot{
			{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x", TotalBytes: 6 * 4096}, {ID: "f@y", TotalBytes: 4 * 4096}}, VersionRuns: []PlannerVersionRun{
				{Branch: "f@x", Allocator: allocator, FirstSequence: 0, Pages: 6},
				{Branch: "f@y", Allocator: allocator, FirstSequence: 2, Pages: 4},
			}},
			{NodeID: "b", Branches: []PlannerBranchSnapshot{{ID: "f@z", TotalBytes: 2 * 4096}}, VersionRuns: []PlannerVersionRun{
				{Branch: "f@z", Allocator: allocator, FirstSequence: 4, Pages: 2},
			}},
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(problem.Shared) != 2 {
		t.Fatalf("shared=%+v", problem.Shared)
	}
	want := []SharedResource{
		{ID: "resident-version-0000", Bytes: 2 * 4096, Branches: []string{"f@x", "f@y"}},
		{ID: "resident-version-0001", Bytes: 2 * 4096, Branches: []string{"f@x", "f@y"}, FixedNodes: []string{"b"}},
	}
	if !reflect.DeepEqual(problem.Shared, want) {
		t.Fatalf("shared=%+v, want %+v", problem.Shared, want)
	}
	if problem.Branches[0].PrivateBytes != 2*4096 || problem.Branches[1].PrivateBytes != 0 {
		t.Fatalf("branches=%+v", problem.Branches)
	}
}

func TestBuildFixedTerminalProblemIntersectsVersionChunks(t *testing.T) {
	const allocator = "000102030405060708090a0b0c0d0e0f"
	bitmap := func(slots ...int) string {
		bytes := make([]byte, 32)
		for _, slot := range slots {
			bytes[slot/8] |= byte(1 << uint(slot%8))
		}
		const digits = "0123456789abcdef"
		var result strings.Builder
		for _, value := range bytes {
			result.WriteByte(digits[value>>4])
			result.WriteByte(digits[value&15])
		}
		return result.String()
	}
	problem, err := BuildFixedTerminalProblem(
		PlacementRelievePressure, "a",
		[]NodeResource{{ID: "a", CurrentBytes: 1000, TargetBytes: 500}, {ID: "b", CurrentBytes: 100, TargetBytes: 1000}},
		[]PlannerSnapshot{
			{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x", TotalBytes: 3 * 4096}, {ID: "f@y", TotalBytes: 2 * 4096}}, VersionChunks: []PlannerVersionChunk{
				{Branch: "f@x", Allocator: allocator, Chunk: 7, Bitmap: bitmap(0, 2, 255)},
				{Branch: "f@y", Allocator: allocator, Chunk: 7, Bitmap: bitmap(2, 255)},
			}},
			{NodeID: "b", Branches: []PlannerBranchSnapshot{{ID: "f@z", TotalBytes: 4096}}, VersionChunks: []PlannerVersionChunk{
				{Branch: "f@z", Allocator: allocator, Chunk: 7, Bitmap: bitmap(255)},
			}},
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []SharedResource{
		{ID: "resident-version-0000", Bytes: 4096, Branches: []string{"f@x", "f@y"}},
		{ID: "resident-version-0001", Bytes: 4096, Branches: []string{"f@x", "f@y"}, FixedNodes: []string{"b"}},
	}
	if !reflect.DeepEqual(problem.Shared, want) {
		t.Fatalf("shared=%+v, want %+v", problem.Shared, want)
	}
}

func TestBuildFixedTerminalProblemRejectsOverlappingRuns(t *testing.T) {
	const allocator = "000102030405060708090a0b0c0d0e0f"
	_, err := BuildFixedTerminalProblem(
		PlacementEvacuate, "a",
		[]NodeResource{{ID: "a", CurrentBytes: 1, TargetBytes: 1}, {ID: "b", TargetBytes: 1}},
		[]PlannerSnapshot{{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x", TotalBytes: 8192}}, VersionRuns: []PlannerVersionRun{
			{Branch: "f@x", Allocator: allocator, FirstSequence: 0, Pages: 2},
			{Branch: "f@x", Allocator: allocator, FirstSequence: 1, Pages: 1},
		}}, {NodeID: "b"}}, nil,
	)
	if err == nil {
		t.Fatal("overlapping version runs were accepted")
	}
}

func TestBuildFixedTerminalProblemFreesRuntimeWithLastBranch(t *testing.T) {
	const mib = 1 << 20
	// A node over its target holds one family's only branch, 256 MiB, and the
	// family's runtime beside it, 100 MiB more: moving the branch frees both.
	nodes := []NodeResource{
		{ID: "n1", CurrentBytes: 3000 * mib, TargetBytes: 64 << 30},
		{ID: "n2", CurrentBytes: 80 * mib, TargetBytes: 64 << 30},
		{ID: "n3", CurrentBytes: 400 * mib, TargetBytes: 128 * mib},
	}
	snapshots := func(runtimes []PlannerRuntime) []PlannerSnapshot {
		return []PlannerSnapshot{
			{NodeID: "n1", Branches: []PlannerBranchSnapshot{{ID: "other@main", TotalBytes: 2000 * mib}}},
			{NodeID: "n2"},
			{NodeID: "n3", Branches: []PlannerBranchSnapshot{{ID: "big@main", TotalBytes: 256 * mib}}, Runtimes: runtimes},
		}
	}
	plan := func(runtimes []PlannerRuntime) (FixedTerminalPlan, error) {
		problem, err := BuildFixedTerminalProblem(PlacementRelievePressure, "n3", nodes, snapshots(runtimes), nil)
		if err != nil {
			t.Fatal(err)
		}
		return PlanFixedTerminals(problem)
	}
	if _, err := plan(nil); err == nil {
		t.Fatal("without the runtime, moving the branch left the node at 144 MiB and still relieved it")
	}
	got, err := plan([]PlannerRuntime{{ID: "r", Family: "big", Bytes: 100 * mib, Branches: []string{"big@main"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Moves) != 1 || got.Moves[0].Branch != "big@main" || got.NodeAfterBytes["n3"] != 44*mib {
		t.Fatalf("plan=%+v", got)
	}
	// The destination spends as much on a runtime of its own.
	if after := got.NodeAfterBytes[got.Moves[0].ToNode]; after != nodes[1].CurrentBytes+356*mib && after != nodes[0].CurrentBytes+356*mib {
		t.Fatalf("destination after=%d, want its current bytes and 356 MiB", after)
	}
}

func TestBuildFixedTerminalProblemRuntimeEdges(t *testing.T) {
	problem, err := BuildFixedTerminalProblem(
		PlacementRelievePressure, "a",
		[]NodeResource{{ID: "a", CurrentBytes: 1000, TargetBytes: 500}, {ID: "b", TargetBytes: 1000}, {ID: "c", TargetBytes: 1000}},
		[]PlannerSnapshot{
			{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x", TotalBytes: 100}, {ID: "f@y", TotalBytes: 100}, {ID: "g@x", TotalBytes: 100}}, Runtimes: []PlannerRuntime{
				{ID: "rf", Family: "f", Bytes: 300, Branches: []string{"f@y", "f@x"}},
				{ID: "rg", Family: "g", Bytes: 0, Branches: []string{"g@x"}},
			}},
			// Node b runs a branch of f: a branch of f arriving there needs no
			// runtime of its own.
			{NodeID: "b", Branches: []PlannerBranchSnapshot{{ID: "f@z", TotalBytes: 100}}},
			{NodeID: "c", Runtimes: []PlannerRuntime{{ID: "rf2", Family: "f", Bytes: 50}}},
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []SharedResource{{ID: "runtime-rf", Bytes: 300, Branches: []string{"f@x", "f@y"}, FixedNodes: []string{"b", "c"}}}
	if !reflect.DeepEqual(problem.Shared, want) {
		t.Fatalf("shared=%+v, want %+v", problem.Shared, want)
	}
	for _, runtimes := range [][]PlannerRuntime{
		{{ID: "rf", Family: "f", Bytes: 1, Branches: []string{"f@w"}}},
		{{ID: "rf", Family: "f", Bytes: 1, Branches: []string{"f@x"}}, {ID: "rf", Family: "f", Bytes: 1, Branches: []string{"f@y"}}},
	} {
		if _, err := BuildFixedTerminalProblem(PlacementRelievePressure, "a",
			[]NodeResource{{ID: "a", CurrentBytes: 1000, TargetBytes: 500}},
			[]PlannerSnapshot{{NodeID: "a", Branches: []PlannerBranchSnapshot{{ID: "f@x"}, {ID: "f@y"}}, Runtimes: runtimes}}, nil); err == nil {
			t.Errorf("runtimes %+v were accepted", runtimes)
		}
	}
}
