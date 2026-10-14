// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"testing"

	"gvisor.dev/gvisor/pkg/cputopology"
	"gvisor.dev/gvisor/pkg/sentry/kernel/sched"
)

// twoSocketTopology is a host of two sockets of two cores of two threads, a
// memory node each, of which the sandbox may use allowed.
func twoSocketTopology(allowed string) cputopology.Topology {
	t := cputopology.Topology{Mode: cputopology.Host, Allowed: allowed}
	for id := 0; id < 8; id++ {
		pkg, core := (id%4)/2, id%2
		first := pkg*2 + core
		t.CPUs = append(t.CPUs, cputopology.CPU{
			ID: id, Package: pkg, Core: core, Node: pkg, APICID: uint32(pkg<<4 | core<<1 | id/4),
			Threads:     cputopology.FormatCPUList([]int{first, first + 4}),
			PackageCPUs: cputopology.FormatCPUList([]int{pkg * 2, pkg*2 + 1, pkg*2 + 4, pkg*2 + 5}),
		})
	}
	t.Nodes = []cputopology.Node{
		{ID: 0, CPUs: "0-1,4-5", MemoryBytes: 1 << 30, Distances: []int{10, 21}},
		{ID: 1, CPUs: "2-3,6-7", MemoryBytes: 1 << 30, Distances: []int{21, 10}},
	}
	return t
}

func cpusOf(mask sched.CPUSet) []int {
	var cpus []int
	mask.ForEachCPU(func(cpu uint) { cpus = append(cpus, int(cpu)) })
	return cpus
}

func TestHostCPUTopologyConfines(t *testing.T) {
	ht, err := newHostCPUTopology(twoSocketTopology("0-1,4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cputopology.FormatCPUList(cpusOf(ht.confine(sched.NewFullCPUSet(8)))); got != "0-1,4" {
		t.Errorf("a mask of every CPU confined to the sandbox's is %s, want 0-1,4", got)
	}
	mask := sched.NewCPUSet(8)
	mask.Set(2)
	mask.Set(3)
	if got := ht.confine(mask).NumCPUs(); got != 0 {
		t.Errorf("a mask of CPUs the sandbox may not use keeps %d", got)
	}
	if ht.nodeMask != 0b11 || ht.allowedNodes != 0b01 {
		t.Errorf("node masks %#b and allowed %#b, want 0b11 and 0b01", ht.nodeMask, ht.allowedNodes)
	}
}

func TestHostCPUTopologyPlacementIDs(t *testing.T) {
	ht, err := newHostCPUTopology(twoSocketTopology("0-7"))
	if err != nil {
		t.Fatal(err)
	}
	a := ht.placementID([]uint64{0b1})
	b := ht.placementID([]uint64{0b10})
	if a == 0 || b == 0 || a == b {
		t.Errorf("placements of CPU 0 and CPU 1 have IDs %d and %d, want distinct and not 0", a, b)
	}
	if again := ht.placementID([]uint64{0b1}); again != a {
		t.Errorf("CPU 0's placement has ID %d, then %d", a, again)
	}
}

func TestHostCPUTopologyRefusesTooLarge(t *testing.T) {
	if _, err := newHostCPUTopology(cputopology.Topology{Mode: cputopology.Single}); err == nil {
		t.Error("newHostCPUTopology accepted the single topology")
	}
}
