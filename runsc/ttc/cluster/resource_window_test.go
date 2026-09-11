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
