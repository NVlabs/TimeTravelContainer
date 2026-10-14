// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cputopology describes the CPU and memory-node layout a sandbox
// shows its guest. Under the single topology, the guest sees one socket and
// one memory node over the CPUs the sandbox may use, which the host
// schedules as it likes. Under the host topology, it sees the host's own
// sockets, cores, threads and memory nodes, by their host numbers, and its
// affinity and memory policies bind its threads and its memory to them.
package cputopology

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// Single is the topology of one socket and one memory node.
	Single = "single"
	// Host is the host's own topology.
	Host = "host"
)

// Topology is the layout a sandbox's guest sees.
type Topology struct {
	// Mode is Single or Host.
	Mode string `json:"mode"`

	// CPUs are the host's CPUs, indexed by their numbers. Only the host
	// topology has them.
	CPUs []CPU `json:"cpus,omitempty"`

	// Nodes are the host's memory nodes, in order of their numbers. Only the
	// host topology has them.
	Nodes []Node `json:"nodes,omitempty"`

	// Allowed lists, as a CPU list, the host CPUs the sandbox may run on: a
	// guest thread's affinity is limited to them, as a Linux task's is to its
	// cpuset. Only the host topology has it.
	Allowed string `json:"allowed,omitempty"`
}

// CPU is one host CPU, a hardware thread.
type CPU struct {
	ID      int    `json:"id"`
	Package int    `json:"package"`
	Die     int    `json:"die"`
	Core    int    `json:"core"`
	Node    int    `json:"node"`
	APICID  uint32 `json:"apic_id"`
	// Threads lists the CPUs of the same core, as a CPU list.
	Threads string `json:"threads"`
	// PackageCPUs lists the CPUs of the same package, as a CPU list.
	PackageCPUs string `json:"package_cpus"`
}

// Node is one memory node.
type Node struct {
	ID int `json:"id"`
	// CPUs lists the node's CPUs, as a CPU list.
	CPUs string `json:"cpus"`
	// MemoryBytes is the node's memory.
	MemoryBytes uint64 `json:"memory_bytes"`
	// Distances are the node's distances to every node, in order.
	Distances []int `json:"distances"`
}

// Parse parses a topology as Marshal formats it, and validates it.
func Parse(data string) (Topology, error) {
	var t Topology
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return Topology{}, fmt.Errorf("parsing CPU topology: %w", err)
	}
	return t, t.Validate()
}

// Marshal formats t for Parse.
func (t Topology) Marshal() string {
	data, err := json.Marshal(t)
	if err != nil {
		panic(fmt.Sprintf("marshaling CPU topology: %v", err))
	}
	return string(data)
}

// Validate checks that t is a topology a sandbox can show.
func (t Topology) Validate() error {
	switch t.Mode {
	case Single:
		if len(t.CPUs) != 0 || len(t.Nodes) != 0 || t.Allowed != "" {
			return errors.New("the single CPU topology has no host CPUs or nodes")
		}
		return nil
	case Host:
	default:
		return fmt.Errorf("unknown CPU topology %q; it is single or host", t.Mode)
	}
	if len(t.CPUs) == 0 || len(t.Nodes) == 0 {
		return errors.New("the host CPU topology has no CPUs or no memory nodes")
	}
	for i, cpu := range t.CPUs {
		if cpu.ID != i {
			return fmt.Errorf("host CPU %d is recorded as CPU %d", i, cpu.ID)
		}
		if cpu.Node < 0 || cpu.Node >= len(t.Nodes) {
			return fmt.Errorf("host CPU %d is on unknown node %d", i, cpu.Node)
		}
		for _, list := range []string{cpu.Threads, cpu.PackageCPUs} {
			if _, err := t.cpuList(list); err != nil {
				return fmt.Errorf("host CPU %d: %w", i, err)
			}
		}
	}
	for i, node := range t.Nodes {
		if node.ID != i {
			return fmt.Errorf("memory node %d is recorded as node %d", i, node.ID)
		}
		if len(node.Distances) != len(t.Nodes) {
			return fmt.Errorf("memory node %d has %d distances for %d nodes", i, len(node.Distances), len(t.Nodes))
		}
		if _, err := t.cpuList(node.CPUs); err != nil {
			return fmt.Errorf("memory node %d: %w", i, err)
		}
	}
	allowed, err := t.cpuList(t.Allowed)
	if err != nil {
		return fmt.Errorf("allowed CPUs: %w", err)
	}
	if len(allowed) == 0 {
		return errors.New("the host CPU topology allows no CPUs")
	}
	return nil
}

// cpuList is ParseCPUList, of CPUs that t has.
func (t Topology) cpuList(list string) ([]int, error) {
	cpus, err := ParseCPUList(list)
	if err != nil {
		return nil, err
	}
	for _, cpu := range cpus {
		if cpu >= len(t.CPUs) {
			return nil, fmt.Errorf("CPU %d is not a host CPU", cpu)
		}
	}
	return cpus, nil
}

// AllowedCPUs returns the CPUs that t allows.
func (t Topology) AllowedCPUs() []int {
	cpus, _ := ParseCPUList(t.Allowed)
	return cpus
}

// NodeCPUs returns the CPUs of memory node node.
func (t Topology) NodeCPUs(node int) []int {
	if node < 0 || node >= len(t.Nodes) {
		return nil
	}
	cpus, _ := ParseCPUList(t.Nodes[node].CPUs)
	return cpus
}

// NodeOf returns the memory node of cpu, or 0 if t does not know it.
func (t Topology) NodeOf(cpu int) int {
	if cpu < 0 || cpu >= len(t.CPUs) {
		return 0
	}
	return t.CPUs[cpu].Node
}

// PackageCounts returns how many CPUs, and how many cores, the package of cpu
// has.
func (t Topology) PackageCounts(cpu int) (cpus, cores int) {
	if cpu < 0 || cpu >= len(t.CPUs) {
		return 1, 1
	}
	packageCPUs, _ := ParseCPUList(t.CPUs[cpu].PackageCPUs)
	threads, _ := ParseCPUList(t.CPUs[cpu].Threads)
	cpus = max(len(packageCPUs), 1)
	return cpus, max(cpus/max(len(threads), 1), 1)
}

// AllowedNodes returns the memory nodes of the CPUs that t allows, in order.
func (t Topology) AllowedNodes() []int {
	var nodes []int
	for _, cpu := range t.AllowedCPUs() {
		if node := t.NodeOf(cpu); !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}
	slices.Sort(nodes)
	return nodes
}

// Fingerprint identifies the layout a guest of t sees: two hosts whose
// topologies have one fingerprint show a guest the same CPUs, cores,
// packages and nodes, by the same numbers, so that a guest moved from one to
// the other finds its affinities meaning what they meant. It leaves out the
// CPUs a sandbox may use and how much memory each node has, which differ
// between hosts without changing what a CPU or node number means.
func (t Topology) Fingerprint() string {
	if t.Mode != Host {
		return t.Mode
	}
	var b strings.Builder
	b.WriteString(Host)
	for _, cpu := range t.CPUs {
		fmt.Fprintf(&b, ";%d:%d.%d.%d.%d.%x/%s/%s", cpu.ID, cpu.Package, cpu.Die, cpu.Core, cpu.Node, cpu.APICID, cpu.Threads, cpu.PackageCPUs)
	}
	for _, node := range t.Nodes {
		fmt.Fprintf(&b, ";n%d:%s/%v", node.ID, node.CPUs, node.Distances)
	}
	return b.String()
}

// ParseCPUList parses a Linux CPU list, such as "0-25,52-77", into its CPU
// numbers, in order. The empty list has none.
func ParseCPUList(list string) ([]int, error) {
	list = strings.TrimSpace(list)
	var cpus []int
	if list == "" {
		return nil, nil
	}
	for part := range strings.SplitSeq(list, ",") {
		first, last, isRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(first)
		if err != nil || lo < 0 {
			return nil, fmt.Errorf("invalid CPU list %q", list)
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(last); err != nil || hi < lo {
				return nil, fmt.Errorf("invalid CPU list %q", list)
			}
		}
		for cpu := lo; cpu <= hi; cpu++ {
			cpus = append(cpus, cpu)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}

// FormatCPUList formats cpus as a Linux CPU list, such as "0-25,52-77".
func FormatCPUList(cpus []int) string {
	sorted := slices.Clone(cpus)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var b strings.Builder
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if b.Len() != 0 {
			b.WriteByte(',')
		}
		if i == j {
			fmt.Fprintf(&b, "%d", sorted[i])
		} else {
			fmt.Fprintf(&b, "%d-%d", sorted[i], sorted[j])
		}
		i = j + 1
	}
	return b.String()
}
