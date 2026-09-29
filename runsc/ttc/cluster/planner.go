// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"
)

// BranchResource is one movable hypergraph vertex.
type BranchResource struct {
	ID           string `json:"id"`
	NodeID       string `json:"node_id"`
	PrivateBytes uint64 `json:"private_bytes"`
	Movable      bool   `json:"movable"`
	// Cohort is an optional hard co-location constraint. All branches with the
	// same non-empty cohort are contracted into one movable solver vertex.
	Cohort       string   `json:"cohort,omitempty"`
	AllowedNodes []string `json:"allowed_nodes,omitempty"`
}

// SharedResource is a coarse memory or disk version referenced by branches.
// TTC normally exports 1 MiB memory chunk identities here, not individual
// 4 KiB pages.
type SharedResource struct {
	ID         string   `json:"id"`
	Bytes      uint64   `json:"bytes"`
	Branches   []string `json:"branches"`
	FixedNodes []string `json:"fixed_nodes,omitempty"`
}

type NodeResource struct {
	ID            string `json:"id"`
	CurrentBytes  uint64 `json:"current_bytes"`
	TargetBytes   uint64 `json:"target_bytes"`
	ReservedBytes uint64 `json:"reserved_bytes,omitempty"`
}

// PlacementPlan is a deterministic pressure-relief proposal. Byte estimates
// model branch-attributable deltas; cgroup memory remains the admission truth.
type PlacementPlan struct {
	SourceNode       string   `json:"source_node"`
	TargetNode       string   `json:"target_node"`
	Branches         []string `json:"branches"`
	EstimatedFreed   uint64   `json:"estimated_freed_bytes"`
	EstimatedAdded   uint64   `json:"estimated_added_bytes"`
	EstimatedCut     uint64   `json:"estimated_cut_bytes"`
	SourceAfterBytes uint64   `json:"source_after_bytes"`
	TargetAfterBytes uint64   `json:"target_after_bytes"`
}

// PlanPressureRelief greedily chooses single vertices and whole shared-edge
// groups. Evaluating whole groups prevents a first sibling move from looking
// artificially expensive when its later siblings would reuse the same data.
func PlanPressureRelief(source, target NodeResource, branches []BranchResource, shared []SharedResource) (PlacementPlan, error) {
	if source.ID == "" || target.ID == "" || source.ID == target.ID || source.TargetBytes == 0 || target.TargetBytes == 0 {
		return PlacementPlan{}, errors.New("planner requires distinct nodes with nonzero targets")
	}
	plan := PlacementPlan{SourceNode: source.ID, TargetNode: target.ID, SourceAfterBytes: source.CurrentBytes, TargetAfterBytes: target.CurrentBytes + target.ReservedBytes}
	if plan.SourceAfterBytes <= source.TargetBytes {
		return plan, nil
	}
	byID := make(map[string]BranchResource, len(branches))
	location := make(map[string]string, len(branches))
	for _, branch := range branches {
		if branch.ID == "" || branch.NodeID == "" {
			return PlacementPlan{}, errors.New("branch resource lacks identity or node")
		}
		if _, ok := byID[branch.ID]; ok {
			return PlacementPlan{}, fmt.Errorf("duplicate branch resource %q", branch.ID)
		}
		byID[branch.ID] = branch
		location[branch.ID] = branch.NodeID
	}
	selected := make(map[string]bool)
	for plan.SourceAfterBytes > source.TargetBytes {
		groups := candidateMoveGroups(source.ID, branches, shared, selected)
		var best []string
		var bestFreed, bestAdded, bestCut uint64
		for _, group := range groups {
			freed, added, cut := estimateMove(group, source.ID, target.ID, byID, location, shared)
			if freed == 0 || plan.TargetAfterBytes+added > target.TargetBytes {
				continue
			}
			// Compare freed/(added+cut) without floating point. A zero cost is
			// always preferred; stable IDs break exact ties.
			if best == nil || betterMove(group, freed, added, cut, best, bestFreed, bestAdded, bestCut) {
				best, bestFreed, bestAdded, bestCut = group, freed, added, cut
			}
		}
		if best == nil {
			return plan, fmt.Errorf("no feasible move can reduce node %q below its %d-byte target", source.ID, source.TargetBytes)
		}
		for _, id := range best {
			selected[id] = true
			location[id] = target.ID
			plan.Branches = append(plan.Branches, id)
		}
		plan.EstimatedFreed += bestFreed
		plan.EstimatedAdded += bestAdded
		plan.EstimatedCut += bestCut
		if bestFreed >= plan.SourceAfterBytes {
			plan.SourceAfterBytes = 0
		} else {
			plan.SourceAfterBytes -= bestFreed
		}
		plan.TargetAfterBytes += bestAdded
	}
	sort.Strings(plan.Branches)
	return plan, nil
}

func candidateMoveGroups(source string, branches []BranchResource, shared []SharedResource, selected map[string]bool) [][]string {
	seen := make(map[string]bool)
	var result [][]string
	add := func(group []string) {
		sort.Strings(group)
		key := ""
		for _, id := range group {
			key += "\x00" + id
		}
		if len(group) != 0 && !seen[key] {
			seen[key] = true
			result = append(result, group)
		}
	}
	for _, branch := range branches {
		if branch.NodeID == source && branch.Movable && !selected[branch.ID] {
			add([]string{branch.ID})
		}
	}
	for _, edge := range shared {
		group := make([]string, 0, len(edge.Branches))
		for _, id := range edge.Branches {
			for _, branch := range branches {
				if branch.ID == id && branch.NodeID == source && branch.Movable && !selected[id] {
					group = append(group, id)
					break
				}
			}
		}
		// Bound one scheduling action. Larger fan-outs are reconsidered after
		// each batch with updated residency counts.
		if len(group) > 1 && len(group) <= 64 {
			add(group)
		}
	}
	sort.Slice(result, func(i, j int) bool { return stringsForIDs(result[i]) < stringsForIDs(result[j]) })
	return result
}

func stringsForIDs(ids []string) string {
	result := ""
	for _, id := range ids {
		result += "\x00" + id
	}
	return result
}

func estimateMove(group []string, source, target string, branches map[string]BranchResource, location map[string]string, shared []SharedResource) (freed, added, cut uint64) {
	moving := make(map[string]bool, len(group))
	for _, id := range group {
		moving[id] = true
		freed += branches[id].PrivateBytes
		added += branches[id].PrivateBytes
	}
	for _, edge := range shared {
		beforeSource, beforeTarget := 0, 0
		afterSource, afterTarget := 0, 0
		for _, id := range edge.Branches {
			node := location[id]
			if node == source {
				beforeSource++
			}
			if node == target {
				beforeTarget++
			}
			if moving[id] {
				node = target
			}
			if node == source {
				afterSource++
			}
			if node == target {
				afterTarget++
			}
		}
		if beforeSource != 0 && afterSource == 0 {
			freed += edge.Bytes
		}
		if beforeTarget == 0 && afterTarget != 0 {
			added += edge.Bytes
		}
		if afterSource != 0 && afterTarget != 0 {
			cut += edge.Bytes
		}
	}
	return freed, added, cut
}

func betterMove(group []string, freed, added, cut uint64, best []string, bestFreed, bestAdded, bestCut uint64) bool {
	cost, bestCost := added+cut, bestAdded+bestCut
	if cost == 0 || bestCost == 0 {
		if cost != bestCost {
			return cost == 0
		}
	} else {
		leftHi, leftLo := bits.Mul64(freed, bestCost)
		rightHi, rightLo := bits.Mul64(bestFreed, cost)
		if leftHi != rightHi || leftLo != rightLo {
			return leftHi > rightHi || (leftHi == rightHi && leftLo > rightLo)
		}
	}
	if cut != bestCut {
		return cut < bestCut
	}
	return stringsForIDs(group) < stringsForIDs(best)
}
