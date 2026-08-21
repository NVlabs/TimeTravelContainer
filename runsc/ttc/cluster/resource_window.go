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
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"gvisor.dev/gvisor/pkg/hostarch"
)

// PlannerSnapshot is one node's compressed resident resource summary.
type PlannerSnapshot struct {
	NodeID      string                  `json:"node_id"`
	Branches    []PlannerBranchSnapshot `json:"branches"`
	VersionRuns []PlannerVersionRun     `json:"version_runs,omitempty"`
}

// PlannerBranchSnapshot accounts one active branch on a node.
type PlannerBranchSnapshot struct {
	ID           string   `json:"id"`
	TotalBytes   uint64   `json:"total_bytes"`
	Cohort       string   `json:"cohort,omitempty"`
	AllowedNodes []string `json:"allowed_nodes,omitempty"`
}

// PlannerVersionRun is a contiguous sequence of resident 4 KiB content
// versions. Allocator is a lowercase hexadecimal 128-bit allocator ID.
type PlannerVersionRun struct {
	Branch        string `json:"branch"`
	Allocator     string `json:"allocator"`
	FirstSequence uint64 `json:"first_sequence"`
	Pages         uint64 `json:"pages"`
}

type versionEvent struct {
	member string
	delta  int
}

type versionMember struct {
	node   string
	branch string
}

// BuildFixedTerminalProblem reduces cluster-wide summaries to an event-local
// fixed-terminal problem. Only branches resident on source become variables;
// every other resident branch is collapsed to its node terminal.
func BuildFixedTerminalProblem(mode PlacementMode, source string, nodes []NodeResource, snapshots []PlannerSnapshot, allowedTargets []string) (FixedTerminalProblem, error) {
	if source == "" {
		return FixedTerminalProblem{}, errors.New("resource window source is empty")
	}
	problem := FixedTerminalProblem{Mode: mode, SourceNode: source, Nodes: append([]NodeResource(nil), nodes...)}
	nodeSet := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		nodeSet[node.ID] = true
	}
	if !nodeSet[source] {
		return FixedTerminalProblem{}, fmt.Errorf("resource window source %q is absent", source)
	}
	allowed := make(map[string]bool)
	if len(allowedTargets) == 0 {
		for node := range nodeSet {
			allowed[node] = true
		}
	} else {
		allowed[source] = true
		for _, node := range allowedTargets {
			if !nodeSet[node] {
				return FixedTerminalProblem{}, fmt.Errorf("allowed target %q is absent", node)
			}
			allowed[node] = true
		}
	}
	allowedIDs := sortedBoolKeys(allowed)
	members := make(map[string]versionMember)
	movable := make(map[string]*BranchResource)
	events := make(map[string]map[uint64][]versionEvent)
	seenSnapshots := make(map[string]bool)
	for _, snapshot := range snapshots {
		if !nodeSet[snapshot.NodeID] {
			return FixedTerminalProblem{}, fmt.Errorf("resource snapshot belongs to unknown node %q", snapshot.NodeID)
		}
		if seenSnapshots[snapshot.NodeID] {
			return FixedTerminalProblem{}, fmt.Errorf("duplicate resource snapshot for node %q", snapshot.NodeID)
		}
		seenSnapshots[snapshot.NodeID] = true
		branchSet := make(map[string]bool, len(snapshot.Branches))
		for _, branch := range snapshot.Branches {
			if branch.ID == "" || branchSet[branch.ID] {
				return FixedTerminalProblem{}, fmt.Errorf("invalid or duplicate branch %q on node %q", branch.ID, snapshot.NodeID)
			}
			branchSet[branch.ID] = true
			key := snapshot.NodeID + "\x00" + branch.ID
			members[key] = versionMember{node: snapshot.NodeID, branch: branch.ID}
			if snapshot.NodeID == source {
				copyAllowed := append([]string(nil), allowedIDs...)
				resource := BranchResource{
					ID: branch.ID, NodeID: source, PrivateBytes: branch.TotalBytes, Movable: true,
					Cohort: branch.Cohort, AllowedNodes: copyAllowed,
				}
				movable[branch.ID] = &resource
			}
		}
		for _, run := range snapshot.VersionRuns {
			if !branchSet[run.Branch] || len(run.Allocator) != 32 || run.Pages == 0 || run.FirstSequence > math.MaxUint64-run.Pages {
				return FixedTerminalProblem{}, fmt.Errorf("invalid resident version run for %q on node %q", run.Branch, snapshot.NodeID)
			}
			for _, char := range run.Allocator {
				if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
					return FixedTerminalProblem{}, fmt.Errorf("resident version allocator %q is not lowercase hexadecimal", run.Allocator)
				}
			}
			bySequence := events[run.Allocator]
			if bySequence == nil {
				bySequence = make(map[uint64][]versionEvent)
				events[run.Allocator] = bySequence
			}
			member := snapshot.NodeID + "\x00" + run.Branch
			bySequence[run.FirstSequence] = append(bySequence[run.FirstSequence], versionEvent{member: member, delta: 1})
			bySequence[run.FirstSequence+run.Pages] = append(bySequence[run.FirstSequence+run.Pages], versionEvent{member: member, delta: -1})
		}
	}
	if _, ok := seenSnapshots[source]; !ok {
		return FixedTerminalProblem{}, fmt.Errorf("resource snapshot for source node %q is absent", source)
	}
	type membership struct {
		branches []string
		fixed    []string
		bytes    uint64
	}
	weighted := make(map[string]*membership)
	allocators := make([]string, 0, len(events))
	for allocator := range events {
		allocators = append(allocators, allocator)
	}
	sort.Strings(allocators)
	for _, allocator := range allocators {
		bySequence := events[allocator]
		positions := make([]uint64, 0, len(bySequence))
		for position := range bySequence {
			positions = append(positions, position)
		}
		sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
		active := make(map[string]int)
		for index, position := range positions {
			if index != 0 && positions[index-1] < position {
				movableIDs := make(map[string]bool)
				fixedNodes := make(map[string]bool)
				activeMembers := 0
				for key, count := range active {
					if count <= 0 {
						continue
					}
					activeMembers++
					member := members[key]
					if member.node == source {
						if _, ok := movable[member.branch]; ok {
							movableIDs[member.branch] = true
						} else {
							fixedNodes[member.node] = true
						}
					} else {
						fixedNodes[member.node] = true
					}
				}
				if len(movableIDs) != 0 && activeMembers > 1 {
					branchIDs := sortedBoolKeys(movableIDs)
					fixedIDs := sortedBoolKeys(fixedNodes)
					signature := strings.Join(branchIDs, "\x00") + "\x01" + strings.Join(fixedIDs, "\x00")
					entry := weighted[signature]
					if entry == nil {
						entry = &membership{branches: branchIDs, fixed: fixedIDs}
						weighted[signature] = entry
					}
					pages := position - positions[index-1]
					if pages > math.MaxUint64/uint64(hostarch.PageSize) {
						return FixedTerminalProblem{}, errors.New("resident version interval byte size overflows")
					}
					entry.bytes = saturatingAdd(entry.bytes, pages*uint64(hostarch.PageSize))
				}
			}
			for _, event := range bySequence[position] {
				active[event.member] += event.delta
				if active[event.member] < 0 || active[event.member] > 1 {
					return FixedTerminalProblem{}, fmt.Errorf("overlapping or unbalanced resident version runs for %q", event.member)
				}
				if active[event.member] == 0 {
					delete(active, event.member)
				}
			}
		}
		if len(active) != 0 {
			return FixedTerminalProblem{}, fmt.Errorf("resident version runs for allocator %q are unbalanced", allocator)
		}
	}
	signatures := make([]string, 0, len(weighted))
	for signature := range weighted {
		signatures = append(signatures, signature)
	}
	sort.Strings(signatures)
	for index, signature := range signatures {
		entry := weighted[signature]
		for _, branch := range entry.branches {
			resource := movable[branch]
			if resource.PrivateBytes < entry.bytes {
				return FixedTerminalProblem{}, fmt.Errorf("shared resident bytes exceed total bytes for branch %q", branch)
			}
			resource.PrivateBytes -= entry.bytes
		}
		problem.Shared = append(problem.Shared, SharedResource{
			ID: fmt.Sprintf("resident-version-%04d", index), Bytes: entry.bytes,
			Branches: entry.branches, FixedNodes: entry.fixed,
		})
	}
	branchIDs := make([]string, 0, len(movable))
	for branch := range movable {
		branchIDs = append(branchIDs, branch)
	}
	sort.Strings(branchIDs)
	for _, branch := range branchIDs {
		problem.Branches = append(problem.Branches, *movable[branch])
	}
	return problem, nil
}

func sortedBoolKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value, present := range values {
		if present {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
