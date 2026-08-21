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
	"math/bits"
	"sort"
)

// PlacementMode describes the hard condition that a placement must satisfy.
type PlacementMode string

const (
	// PlacementRelievePressure moves only enough work to bring SourceNode at or
	// below its target. A movable branch may remain on SourceNode.
	PlacementRelievePressure PlacementMode = "relieve-pressure"
	// PlacementEvacuate moves every movable branch away from SourceNode. It is
	// used by node retirement.
	PlacementEvacuate PlacementMode = "evacuate"
)

// AffinityResource is a scheduling-only hyperedge. Unlike SharedResource, it
// affects placement quality but does not contribute to memory load.
type AffinityResource struct {
	ID         string   `json:"id"`
	Weight     uint64   `json:"weight"`
	Branches   []string `json:"branches"`
	FixedNodes []string `json:"fixed_nodes,omitempty"`
}

// FixedTerminalProblem is a reduced cluster placement problem. Branches are
// the only movable vertices. FixedNodes on an edge are terminal vertices that
// summarize all incident, immovable branches already placed on those nodes.
// Nodes and branches outside this window never become solver variables.
type FixedTerminalProblem struct {
	Mode           PlacementMode      `json:"mode"`
	SourceNode     string             `json:"source_node"`
	Nodes          []NodeResource     `json:"nodes"`
	Branches       []BranchResource   `json:"branches"`
	Shared         []SharedResource   `json:"shared,omitempty"`
	Affinities     []AffinityResource `json:"affinities,omitempty"`
	ForbiddenNodes []string           `json:"forbidden_nodes,omitempty"`
}

// PlacementMove is one branch relocation selected by the planner.
type PlacementMove struct {
	Branch   string `json:"branch"`
	FromNode string `json:"from_node"`
	ToNode   string `json:"to_node"`
}

// FixedTerminalPlan is a deterministic multi-destination placement result.
type FixedTerminalPlan struct {
	SourceNode            string            `json:"source_node"`
	Moves                 []PlacementMove   `json:"moves"`
	NodeAfterBytes        map[string]uint64 `json:"node_after_bytes"`
	EstimatedConnectivity uint64            `json:"estimated_connectivity_bytes"`
	EstimatedMigration    uint64            `json:"estimated_migration_bytes"`
}

type plannerEdge struct {
	id       string
	weight   uint64
	load     bool
	branches []string
	fixed    map[string]bool
}

type plannerUnit struct {
	id       string
	branches []string
	allowed  map[string]bool
}

type fixedPlanner struct {
	problem           FixedTerminalProblem
	nodes             map[string]NodeResource
	branches          map[string]BranchResource
	location          map[string]string
	load              map[string]uint64
	edges             []plannerEdge
	edgeOccupancy     []map[string]int
	edgesByBranch     map[string][]int
	connectivityBytes uint64
	migrationBytes    uint64
	units             []plannerUnit
	unitByBranch      map[string]int
	forbidden         map[string]bool
}

// PlanFixedTerminals solves a reduced, fixed-terminal hypergraph placement.
// It first repairs the event's hard constraint with deterministic grouped
// moves, then performs a deterministic FM-style single-unit refinement pass.
func PlanFixedTerminals(problem FixedTerminalProblem) (FixedTerminalPlan, error) {
	p, err := newFixedPlanner(problem)
	if err != nil {
		return FixedTerminalPlan{}, err
	}
	for !p.hardConditionSatisfied() {
		move, ok := p.bestRepairMove()
		if !ok {
			return p.plan(), fmt.Errorf("no feasible placement satisfies %s for node %q", problem.Mode, problem.SourceNode)
		}
		p.apply(move.unit, move.to)
	}
	// Once feasible, accept only strict objective improvements. Capacity and
	// event constraints remain hard throughout refinement.
	for {
		move, ok := p.bestRefinementMove()
		if !ok {
			break
		}
		p.apply(move.unit, move.to)
	}
	return p.plan(), nil
}

func newFixedPlanner(problem FixedTerminalProblem) (*fixedPlanner, error) {
	if problem.Mode != PlacementRelievePressure && problem.Mode != PlacementEvacuate {
		return nil, fmt.Errorf("unknown placement mode %q", problem.Mode)
	}
	if problem.SourceNode == "" {
		return nil, errors.New("placement source node is empty")
	}
	p := &fixedPlanner{
		problem: problem, nodes: make(map[string]NodeResource), branches: make(map[string]BranchResource),
		location: make(map[string]string), load: make(map[string]uint64), forbidden: make(map[string]bool),
		edgesByBranch: make(map[string][]int), unitByBranch: make(map[string]int),
	}
	for _, id := range problem.ForbiddenNodes {
		p.forbidden[id] = true
	}
	if problem.Mode == PlacementEvacuate {
		p.forbidden[problem.SourceNode] = true
	}
	for _, node := range problem.Nodes {
		if node.ID == "" || node.TargetBytes == 0 {
			return nil, errors.New("placement node lacks identity or target")
		}
		if _, ok := p.nodes[node.ID]; ok {
			return nil, fmt.Errorf("duplicate placement node %q", node.ID)
		}
		p.nodes[node.ID] = node
		p.load[node.ID] = saturatingAdd(node.CurrentBytes, node.ReservedBytes)
	}
	if _, ok := p.nodes[problem.SourceNode]; !ok {
		return nil, fmt.Errorf("source node %q is absent", problem.SourceNode)
	}
	cohorts := make(map[string][]string)
	for _, branch := range problem.Branches {
		if branch.ID == "" || branch.NodeID == "" {
			return nil, errors.New("branch resource lacks identity or node")
		}
		if !branch.Movable {
			return nil, fmt.Errorf("fixed branch %q must be contracted into edge terminals", branch.ID)
		}
		if branch.NodeID != problem.SourceNode {
			return nil, fmt.Errorf("movable branch %q is on %q, outside source window %q", branch.ID, branch.NodeID, problem.SourceNode)
		}
		if _, ok := p.branches[branch.ID]; ok {
			return nil, fmt.Errorf("duplicate branch resource %q", branch.ID)
		}
		p.branches[branch.ID] = branch
		p.location[branch.ID] = branch.NodeID
		cohort := branch.Cohort
		if cohort == "" {
			cohort = "\x00" + branch.ID
		}
		cohorts[cohort] = append(cohorts[cohort], branch.ID)
	}
	for cohort, ids := range cohorts {
		sort.Strings(ids)
		allowed := make(map[string]bool)
		first := true
		for _, id := range ids {
			branch := p.branches[id]
			candidate := make(map[string]bool)
			if len(branch.AllowedNodes) == 0 {
				for node := range p.nodes {
					candidate[node] = true
				}
			} else {
				for _, node := range branch.AllowedNodes {
					if _, ok := p.nodes[node]; !ok {
						return nil, fmt.Errorf("branch %q allows unknown node %q", id, node)
					}
					candidate[node] = true
				}
			}
			if first {
				allowed = candidate
				first = false
			} else {
				for node := range allowed {
					if !candidate[node] {
						delete(allowed, node)
					}
				}
			}
		}
		if len(allowed) == 0 {
			return nil, fmt.Errorf("hard co-location cohort %q has no common allowed node", cohort)
		}
		p.units = append(p.units, plannerUnit{id: cohort, branches: ids, allowed: allowed})
	}
	sort.Slice(p.units, func(i, j int) bool { return p.units[i].id < p.units[j].id })
	for index, unit := range p.units {
		for _, branch := range unit.branches {
			p.unitByBranch[branch] = index
		}
	}
	for _, edge := range problem.Shared {
		converted, err := p.edge(edge.ID, edge.Bytes, edge.Branches, edge.FixedNodes, true)
		if err != nil {
			return nil, err
		}
		p.edges = append(p.edges, converted)
	}
	for _, edge := range problem.Affinities {
		converted, err := p.edge(edge.ID, edge.Weight, edge.Branches, edge.FixedNodes, false)
		if err != nil {
			return nil, err
		}
		p.edges = append(p.edges, converted)
	}
	sort.Slice(p.edges, func(i, j int) bool {
		if p.edges[i].id != p.edges[j].id {
			return p.edges[i].id < p.edges[j].id
		}
		return !p.edges[i].load && p.edges[j].load
	})
	p.edgeOccupancy = make([]map[string]int, len(p.edges))
	for index, edge := range p.edges {
		occupancy := make(map[string]int, len(edge.fixed)+len(edge.branches))
		for node := range edge.fixed {
			occupancy[node]++
		}
		for _, branch := range edge.branches {
			occupancy[p.location[branch]]++
			p.edgesByBranch[branch] = append(p.edgesByBranch[branch], index)
		}
		p.edgeOccupancy[index] = occupancy
		p.connectivityBytes = saturatingAdd(p.connectivityBytes, edgeConnectivity(edge.weight, occupancy))
	}
	return p, nil
}

func (p *fixedPlanner) edge(id string, weight uint64, branches, fixedNodes []string, load bool) (plannerEdge, error) {
	if id == "" || weight == 0 {
		return plannerEdge{}, errors.New("placement edge lacks identity or weight")
	}
	e := plannerEdge{id: id, weight: weight, load: load, fixed: make(map[string]bool)}
	seen := make(map[string]bool)
	for _, branch := range branches {
		if _, ok := p.branches[branch]; !ok {
			return plannerEdge{}, fmt.Errorf("edge %q contains non-movable branch %q; contract it to a fixed node", id, branch)
		}
		if !seen[branch] {
			seen[branch] = true
			e.branches = append(e.branches, branch)
		}
	}
	for _, node := range fixedNodes {
		if _, ok := p.nodes[node]; !ok {
			return plannerEdge{}, fmt.Errorf("edge %q contains unknown fixed terminal %q", id, node)
		}
		e.fixed[node] = true
	}
	if len(e.branches) == 0 {
		return plannerEdge{}, fmt.Errorf("edge %q is fixed-only and must be omitted from the reduced problem", id)
	}
	sort.Strings(e.branches)
	return e, nil
}

type proposedMove struct {
	unit       plannerUnit
	to         string
	from       string
	freed      uint64
	added      uint64
	beforeCost uint64
	afterCost  uint64
}

func (p *fixedPlanner) bestRepairMove() (proposedMove, bool) {
	var best proposedMove
	found := false
	for _, unit := range p.repairUnits() {
		from := p.unitLocation(unit)
		if from != p.problem.SourceNode {
			continue
		}
		for _, to := range sortedAllowed(unit.allowed) {
			if to == from || p.forbidden[to] {
				continue
			}
			candidate, ok := p.evaluate(unit, to)
			if !ok || (p.problem.Mode == PlacementRelievePressure && candidate.freed == 0) {
				continue
			}
			if !found || betterRepair(candidate, best) {
				best, found = candidate, true
			}
		}
	}
	return best, found
}

// repairUnits adds bounded whole-hyperedge groups to the contracted hard
// cohorts. This avoids rejecting the first apparently expensive sibling move
// when moving all incident siblings together releases or reuses the edge.
func (p *fixedPlanner) repairUnits() []plannerUnit {
	result := append([]plannerUnit(nil), p.units...)
	seen := make(map[string]bool, len(result))
	for _, unit := range result {
		seen[stringsForIDs(unit.branches)] = true
	}
	for _, edge := range p.edges {
		if !edge.load {
			continue
		}
		unitIDs := make(map[int]bool)
		allOnSource := true
		for _, branch := range edge.branches {
			if p.location[branch] != p.problem.SourceNode {
				allOnSource = false
				break
			}
			unitIDs[p.unitByBranch[branch]] = true
		}
		if !allOnSource || len(unitIDs) <= 1 || len(unitIDs) > 64 {
			continue
		}
		indexes := make([]int, 0, len(unitIDs))
		for index := range unitIDs {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		group := plannerUnit{id: "edge:" + edge.id, allowed: make(map[string]bool)}
		first := true
		for _, index := range indexes {
			unit := p.units[index]
			group.branches = append(group.branches, unit.branches...)
			if first {
				for node, ok := range unit.allowed {
					group.allowed[node] = ok
				}
				first = false
			} else {
				for node := range group.allowed {
					if !unit.allowed[node] {
						delete(group.allowed, node)
					}
				}
			}
		}
		sort.Strings(group.branches)
		key := stringsForIDs(group.branches)
		if len(group.allowed) != 0 && !seen[key] {
			seen[key] = true
			result = append(result, group)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result
}

func betterRepair(candidate, best proposedMove) bool {
	// Prefer more source relief per byte of destination growth plus increased
	// connectivity. Objective improvements offset destination cost.
	cost := repairCost(candidate)
	bestCost := repairCost(best)
	if cost == 0 || bestCost == 0 {
		if cost != bestCost {
			return cost == 0
		}
	} else {
		leftHi, leftLo := bits.Mul64(candidate.freed, bestCost)
		rightHi, rightLo := bits.Mul64(best.freed, cost)
		if leftHi != rightHi || leftLo != rightLo {
			return leftHi > rightHi || (leftHi == rightHi && leftLo > rightLo)
		}
	}
	if candidate.afterCost != best.afterCost {
		return candidate.afterCost < best.afterCost
	}
	if candidate.added != best.added {
		return candidate.added < best.added
	}
	return candidateKey(candidate) < candidateKey(best)
}

func repairCost(move proposedMove) uint64 {
	cost := move.added
	if move.afterCost >= move.beforeCost {
		return saturatingAdd(cost, move.afterCost-move.beforeCost)
	}
	benefit := move.beforeCost - move.afterCost
	if benefit >= cost {
		return 0
	}
	return cost - benefit
}

func (p *fixedPlanner) bestRefinementMove() (proposedMove, bool) {
	var best proposedMove
	found := false
	for _, unit := range p.units {
		from := p.unitLocation(unit)
		for _, to := range sortedAllowed(unit.allowed) {
			if to == from || p.forbidden[to] {
				continue
			}
			candidate, ok := p.evaluate(unit, to)
			if !ok || candidate.afterCost >= candidate.beforeCost {
				continue
			}
			p.apply(unit, to)
			feasible := p.hardConditionSatisfied()
			p.apply(unit, from)
			if !feasible {
				continue
			}
			if !found || candidate.beforeCost-candidate.afterCost > best.beforeCost-best.afterCost ||
				(candidate.beforeCost-candidate.afterCost == best.beforeCost-best.afterCost && candidateKey(candidate) < candidateKey(best)) {
				best, found = candidate, true
			}
		}
	}
	return best, found
}

func candidateKey(move proposedMove) string {
	return move.unit.id + "\x00" + move.to
}

func (p *fixedPlanner) evaluate(unit plannerUnit, to string) (proposedMove, bool) {
	from := p.unitLocation(unit)
	if from == "" || from == to {
		return proposedMove{}, false
	}
	beforeLoad := cloneLoads(p.load)
	beforeCost := p.objective()
	p.apply(unit, to)
	afterCost := p.objective()
	valid := p.load[to] <= p.nodes[to].TargetBytes
	afterLoad := cloneLoads(p.load)
	p.apply(unit, from)
	if !valid {
		return proposedMove{}, false
	}
	return proposedMove{
		unit: unit, to: to, from: from,
		freed:      saturatingSub(beforeLoad[from], afterLoad[from]),
		added:      saturatingSub(afterLoad[to], beforeLoad[to]),
		beforeCost: beforeCost, afterCost: afterCost,
	}, true
}

func (p *fixedPlanner) apply(unit plannerUnit, to string) {
	from := p.unitLocation(unit)
	if from == "" || from == to {
		return
	}
	touched := make(map[int]struct{})
	for _, id := range unit.branches {
		for _, edge := range p.edgesByBranch[id] {
			touched[edge] = struct{}{}
		}
	}
	touchedIDs := make([]int, 0, len(touched))
	for edge := range touched {
		touchedIDs = append(touchedIDs, edge)
	}
	sort.Ints(touchedIDs)
	beforePresence := make(map[int]map[string]bool, len(touchedIDs))
	beforeConnectivity := make(map[int]uint64, len(touchedIDs))
	for _, index := range touchedIDs {
		beforePresence[index] = occupiedNodes(p.edgeOccupancy[index])
		beforeConnectivity[index] = edgeConnectivity(p.edges[index].weight, p.edgeOccupancy[index])
	}
	for _, id := range unit.branches {
		branch := p.branches[id]
		if from == branch.NodeID {
			p.migrationBytes = saturatingAdd(p.migrationBytes, branch.PrivateBytes)
		}
		if to == branch.NodeID {
			p.migrationBytes = saturatingSub(p.migrationBytes, branch.PrivateBytes)
		}
		p.load[from] = saturatingSub(p.load[from], branch.PrivateBytes)
		p.load[to] = saturatingAdd(p.load[to], branch.PrivateBytes)
		p.location[id] = to
		for _, edge := range p.edgesByBranch[id] {
			p.edgeOccupancy[edge][from]--
			if p.edgeOccupancy[edge][from] == 0 {
				delete(p.edgeOccupancy[edge], from)
			}
			p.edgeOccupancy[edge][to]++
		}
	}
	for _, index := range touchedIDs {
		edge := p.edges[index]
		p.connectivityBytes = saturatingSub(p.connectivityBytes, beforeConnectivity[index])
		p.connectivityBytes = saturatingAdd(p.connectivityBytes, edgeConnectivity(edge.weight, p.edgeOccupancy[index]))
		if !edge.load {
			continue
		}
		afterPresence := occupiedNodes(p.edgeOccupancy[index])
		for node := range p.nodes {
			if beforePresence[index][node] && !afterPresence[node] {
				p.load[node] = saturatingSub(p.load[node], edge.weight)
			} else if !beforePresence[index][node] && afterPresence[node] {
				p.load[node] = saturatingAdd(p.load[node], edge.weight)
			}
		}
	}
}

func occupiedNodes(occupancy map[string]int) map[string]bool {
	result := make(map[string]bool, len(occupancy))
	for node, count := range occupancy {
		if count > 0 {
			result[node] = true
		}
	}
	return result
}

func (p *fixedPlanner) connectivity() uint64 {
	return p.connectivityBytes
}

func (p *fixedPlanner) objective() uint64 {
	return saturatingAdd(p.connectivityBytes, p.migrationBytes)
}

func edgeConnectivity(weight uint64, occupancy map[string]int) uint64 {
	if len(occupancy) <= 1 {
		return 0
	}
	return saturatingMultiply(weight, uint64(len(occupancy)-1))
}

func (p *fixedPlanner) hardConditionSatisfied() bool {
	if p.problem.Mode == PlacementRelievePressure {
		return p.load[p.problem.SourceNode] <= p.nodes[p.problem.SourceNode].TargetBytes
	}
	for _, branch := range p.problem.Branches {
		if p.location[branch.ID] == p.problem.SourceNode {
			return false
		}
	}
	return true
}

func (p *fixedPlanner) unitLocation(unit plannerUnit) string {
	location := ""
	for _, id := range unit.branches {
		if location == "" {
			location = p.location[id]
		} else if p.location[id] != location {
			panic("hard co-location cohort was split")
		}
	}
	return location
}

func (p *fixedPlanner) plan() FixedTerminalPlan {
	plan := FixedTerminalPlan{
		SourceNode: p.problem.SourceNode, NodeAfterBytes: cloneLoads(p.load),
		EstimatedConnectivity: p.connectivity(),
	}
	for _, branch := range p.problem.Branches {
		to := p.location[branch.ID]
		if to == branch.NodeID {
			continue
		}
		plan.Moves = append(plan.Moves, PlacementMove{Branch: branch.ID, FromNode: branch.NodeID, ToNode: to})
		plan.EstimatedMigration = saturatingAdd(plan.EstimatedMigration, branch.PrivateBytes)
	}
	sort.Slice(plan.Moves, func(i, j int) bool {
		if plan.Moves[i].ToNode != plan.Moves[j].ToNode {
			return plan.Moves[i].ToNode < plan.Moves[j].ToNode
		}
		return plan.Moves[i].Branch < plan.Moves[j].Branch
	})
	return plan
}

func sortedAllowed(allowed map[string]bool) []string {
	result := make([]string, 0, len(allowed))
	for node, ok := range allowed {
		if ok {
			result = append(result, node)
		}
	}
	sort.Strings(result)
	return result
}

func cloneLoads(load map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(load))
	for node, value := range load {
		result[node] = value
	}
	return result
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func saturatingSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func saturatingMultiply(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}
