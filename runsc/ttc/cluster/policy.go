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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

const maxPlacementPolicyBytes = 1 << 20

// PlacementPolicy supplies optional operator constraints to event-local
// placement. Branch names are exact CONTAINER@BRANCH selectors.
type PlacementPolicy struct {
	Version        int                           `json:"version"`
	AllowedNodes   map[string][]string           `json:"allowed_nodes,omitempty"`
	HardCoLocation []PlacementPolicyGroup        `json:"hard_co_location,omitempty"`
	SoftAffinity   []PlacementPolicySoftAffinity `json:"soft_affinity,omitempty"`
}

// PlacementPolicyGroup contracts all named branches into one hard cohort.
type PlacementPolicyGroup struct {
	ID       string   `json:"id"`
	Branches []string `json:"branches"`
}

// PlacementPolicySoftAffinity adds a weighted connectivity hyperedge. Nodes
// are explicit terminals; named branches outside the movable window are
// automatically converted to terminals at their current nodes.
type PlacementPolicySoftAffinity struct {
	ID       string   `json:"id"`
	Branches []string `json:"branches"`
	Nodes    []string `json:"nodes,omitempty"`
	Weight   uint64   `json:"weight_bytes"`
}

// LoadPlacementPolicy loads a strict policy document. A missing file means no
// operator policy, allowing a shared data root to opt in without daemon flags.
func LoadPlacementPolicy(path string) (PlacementPolicy, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PlacementPolicy{}, nil
	}
	if err != nil {
		return PlacementPolicy{}, err
	}
	if len(data) > maxPlacementPolicyBytes {
		return PlacementPolicy{}, fmt.Errorf("placement policy is %d bytes, limit is %d", len(data), maxPlacementPolicyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy PlacementPolicy
	if err := decoder.Decode(&policy); err != nil {
		return PlacementPolicy{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return PlacementPolicy{}, errors.New("placement policy contains multiple JSON values")
		}
		return PlacementPolicy{}, err
	}
	if policy.Version != 1 {
		return PlacementPolicy{}, fmt.Errorf("unsupported placement policy version %d", policy.Version)
	}
	return policy, nil
}

// ApplyPlacementPolicy translates cluster-global policy into movable vertices
// and fixed terminals in problem.
func ApplyPlacementPolicy(problem *FixedTerminalProblem, snapshots []PlannerSnapshot, policy PlacementPolicy) error {
	if policy.Version == 0 {
		return nil
	}
	if policy.Version != 1 {
		return fmt.Errorf("unsupported placement policy version %d", policy.Version)
	}
	nodes := make(map[string]bool, len(problem.Nodes))
	for _, node := range problem.Nodes {
		nodes[node.ID] = true
	}
	locations := make(map[string]string)
	for _, snapshot := range snapshots {
		for _, branch := range snapshot.Branches {
			if previous := locations[branch.ID]; previous != "" && previous != snapshot.NodeID {
				return fmt.Errorf("placement policy branch %q is resident on both %q and %q", branch.ID, previous, snapshot.NodeID)
			}
			locations[branch.ID] = snapshot.NodeID
		}
	}
	branchIndexes := make(map[string]int, len(problem.Branches))
	for index := range problem.Branches {
		branchIndexes[problem.Branches[index].ID] = index
	}
	for branch, allowed := range policy.AllowedNodes {
		index, movable := branchIndexes[branch]
		if !movable {
			continue
		}
		if len(allowed) == 0 {
			return fmt.Errorf("placement policy gives branch %q an empty allowed-node set", branch)
		}
		seen := make(map[string]bool)
		problem.Branches[index].AllowedNodes = nil
		for _, node := range allowed {
			if !nodes[node] {
				return fmt.Errorf("placement policy allows branch %q on unknown node %q", branch, node)
			}
			if !seen[node] {
				seen[node] = true
				problem.Branches[index].AllowedNodes = append(problem.Branches[index].AllowedNodes, node)
			}
		}
		sort.Strings(problem.Branches[index].AllowedNodes)
	}
	cohortIDs := make(map[string]bool)
	for _, group := range policy.HardCoLocation {
		if group.ID == "" || cohortIDs[group.ID] || len(group.Branches) < 2 {
			return fmt.Errorf("invalid or duplicate hard co-location group %q", group.ID)
		}
		cohortIDs[group.ID] = true
		fixedNodes := make(map[string]bool)
		var movable []int
		for _, branch := range group.Branches {
			if index, ok := branchIndexes[branch]; ok {
				movable = append(movable, index)
			} else if node := locations[branch]; node != "" {
				fixedNodes[node] = true
			}
		}
		if len(fixedNodes) > 1 {
			return fmt.Errorf("hard co-location group %q is already split across fixed nodes", group.ID)
		}
		fixedNode := ""
		for node := range fixedNodes {
			fixedNode = node
		}
		for _, index := range movable {
			branch := &problem.Branches[index]
			if fixedNode != "" {
				allowed := false
				for _, node := range branch.AllowedNodes {
					allowed = allowed || node == fixedNode
				}
				if !allowed {
					return fmt.Errorf("hard co-location group %q requires %q on disallowed node %q", group.ID, branch.ID, fixedNode)
				}
				branch.AllowedNodes = []string{fixedNode}
			} else {
				if branch.Cohort != "" && branch.Cohort != group.ID {
					return fmt.Errorf("branch %q belongs to conflicting hard cohorts %q and %q", branch.ID, branch.Cohort, group.ID)
				}
				branch.Cohort = group.ID
			}
		}
	}
	affinityIDs := make(map[string]bool)
	for _, affinity := range policy.SoftAffinity {
		if affinity.ID == "" || affinityIDs[affinity.ID] || affinity.Weight == 0 {
			return fmt.Errorf("invalid or duplicate soft affinity %q", affinity.ID)
		}
		affinityIDs[affinity.ID] = true
		movableSet := make(map[string]bool)
		fixedNodes := make(map[string]bool)
		for _, branch := range affinity.Branches {
			if _, ok := branchIndexes[branch]; ok {
				movableSet[branch] = true
			} else if node := locations[branch]; node != "" {
				fixedNodes[node] = true
			}
		}
		for _, node := range affinity.Nodes {
			if !nodes[node] {
				return fmt.Errorf("soft affinity %q names unknown node %q", affinity.ID, node)
			}
			fixedNodes[node] = true
		}
		if len(movableSet) != 0 {
			problem.Affinities = append(problem.Affinities, AffinityResource{
				ID: affinity.ID, Weight: affinity.Weight,
				Branches: sortedBoolKeys(movableSet), FixedNodes: sortedBoolKeys(fixedNodes),
			})
		}
	}
	return nil
}
