// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cputopology

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// sysRoot and procRoot are where HostTopology reads the host's layout.
var (
	sysRoot  = "/sys/devices/system"
	procRoot = "/proc"
)

// HostTopology reads the host's topology, from sysfs and /proc/cpuinfo, with
// the CPUs that the calling process may run on as the allowed CPUs.
func HostTopology() (Topology, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return Topology{}, fmt.Errorf("reading this process's CPU affinity: %w", err)
	}
	var allowed []int
	for cpu := 0; cpu < len(set)*64; cpu++ {
		if set.IsSet(cpu) {
			allowed = append(allowed, cpu)
		}
	}
	return readHostTopology(allowed)
}

// AllowedNodeCount returns the number of memory nodes that the CPUs the
// calling process may run on belong to, or 1 if the host's nodes cannot be
// read: a host without NUMA has one.
func AllowedNodeCount() int {
	t, err := HostTopology()
	if err != nil {
		return 1
	}
	return len(t.AllowedNodes())
}

func readHostTopology(allowed []int) (Topology, error) {
	online, err := readCPUListFile(filepath.Join(sysRoot, "cpu", "online"))
	if err != nil {
		return Topology{}, err
	}
	for i, cpu := range online {
		if cpu != i {
			return Topology{}, fmt.Errorf("host CPU %d is offline: the host topology needs CPUs 0 to %d online", i, online[len(online)-1])
		}
	}
	apicIDs, err := readAPICIDs(filepath.Join(procRoot, "cpuinfo"))
	if err != nil {
		return Topology{}, err
	}
	t := Topology{Mode: Host, Allowed: FormatCPUList(allowed)}
	nodeOf := make(map[int]int)
	nodeDirs, _ := filepath.Glob(filepath.Join(sysRoot, "node", "node[0-9]*"))
	var nodeIDs []int
	for _, dir := range nodeDirs {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "node"))
		if err == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}
	slices.Sort(nodeIDs)
	if len(nodeIDs) == 0 {
		// A kernel without NUMA support shows no nodes: all is node 0.
		t.Nodes = []Node{{ID: 0, CPUs: FormatCPUList(online), MemoryBytes: hostMemory(), Distances: []int{10}}}
		for _, cpu := range online {
			nodeOf[cpu] = 0
		}
	}
	for i, id := range nodeIDs {
		if id != i {
			return Topology{}, fmt.Errorf("the host's memory nodes are not numbered from 0 without gaps: node %d", id)
		}
		dir := filepath.Join(sysRoot, "node", fmt.Sprintf("node%d", id))
		cpus, err := readCPUListFile(filepath.Join(dir, "cpulist"))
		if err != nil {
			return Topology{}, err
		}
		for _, cpu := range cpus {
			nodeOf[cpu] = id
		}
		distances, err := readInts(filepath.Join(dir, "distance"))
		if err != nil {
			return Topology{}, err
		}
		t.Nodes = append(t.Nodes, Node{ID: id, CPUs: FormatCPUList(cpus), MemoryBytes: nodeMemory(dir, id), Distances: distances})
	}
	for _, id := range online {
		dir := filepath.Join(sysRoot, "cpu", fmt.Sprintf("cpu%d", id), "topology")
		cpu := CPU{ID: id, Node: nodeOf[id], APICID: apicIDs[id]}
		if cpu.Package, err = readInt(filepath.Join(dir, "physical_package_id")); err != nil {
			return Topology{}, err
		}
		if cpu.Core, err = readInt(filepath.Join(dir, "core_id")); err != nil {
			return Topology{}, err
		}
		// die_id is new in Linux 5.2; a CPU without one has one die per package.
		if die, err := readInt(filepath.Join(dir, "die_id")); err == nil && die >= 0 {
			cpu.Die = die
		}
		threads, err := readCPUListFile(filepath.Join(dir, "thread_siblings_list"))
		if err != nil {
			return Topology{}, err
		}
		cpu.Threads = FormatCPUList(threads)
		packageCPUs, err := readCPUListFile(filepath.Join(dir, "package_cpus_list"))
		if err != nil {
			// package_cpus_list is new in Linux 5.6; core_siblings_list is its old name.
			if packageCPUs, err = readCPUListFile(filepath.Join(dir, "core_siblings_list")); err != nil {
				return Topology{}, err
			}
		}
		cpu.PackageCPUs = FormatCPUList(packageCPUs)
		t.CPUs = append(t.CPUs, cpu)
	}
	if err := t.Validate(); err != nil {
		return Topology{}, fmt.Errorf("reading the host's CPU topology: %w", err)
	}
	return t, nil
}

func readCPUListFile(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cpus, err := ParseCPUList(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cpus, nil
}

func readInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

func readInts(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var values []int
	for _, field := range strings.Fields(string(data)) {
		value, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		values = append(values, value)
	}
	return values, nil
}

// readAPICIDs returns each CPU's initial APIC ID, from /proc/cpuinfo, which
// shows them on x86 only: elsewhere a CPU's is its number.
func readAPICIDs(path string) (map[int]uint32, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ids := make(map[int]uint32)
	processor := -1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "processor":
			if processor, err = strconv.Atoi(value); err != nil {
				processor = -1
			} else {
				ids[processor] = uint32(processor)
			}
		case "apicid":
			if id, err := strconv.ParseUint(value, 10, 32); err == nil && processor >= 0 {
				ids[processor] = uint32(id)
			}
		}
	}
	return ids, scanner.Err()
}

// nodeMemory returns node id's memory, from dir/meminfo.
func nodeMemory(dir string, id int) uint64 {
	data, err := os.ReadFile(filepath.Join(dir, "meminfo"))
	if err != nil {
		return 0
	}
	prefix := fmt.Sprintf("Node %d MemTotal:", id)
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
					return kb << 10
				}
			}
		}
	}
	return 0
}

// hostMemory returns the host's memory, for a kernel that shows no nodes.
func hostMemory() uint64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
