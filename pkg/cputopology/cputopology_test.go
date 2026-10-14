// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cputopology

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCPUListRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		list string
		cpus []int
	}{
		{"", nil},
		{"0", []int{0}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0-1,4,6-7", []int{0, 1, 4, 6, 7}},
		{"52-53,0-1", []int{0, 1, 52, 53}},
	} {
		got, err := ParseCPUList(tc.list)
		if err != nil || !reflect.DeepEqual(got, tc.cpus) {
			t.Errorf("ParseCPUList(%q) = %v, %v; want %v", tc.list, got, err, tc.cpus)
		}
		if want := FormatCPUList(tc.cpus); tc.list != "52-53,0-1" && want != tc.list {
			t.Errorf("FormatCPUList(%v) = %q, want %q", tc.cpus, want, tc.list)
		}
	}
	if got := FormatCPUList([]int{53, 0, 52, 1}); got != "0-1,52-53" {
		t.Errorf("FormatCPUList of an unsorted set = %q", got)
	}
	for _, bad := range []string{"a", "3-1", "-1", "1-"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Errorf("ParseCPUList(%q) succeeded", bad)
		}
	}
}

// twoSockets is a host of two sockets, each of two cores of two threads, and
// a memory node each, as Linux numbers them: a core's second threads after
// every core's first.
func twoSockets(allowed string) Topology {
	t := Topology{Mode: Host, Allowed: allowed}
	for id := 0; id < 8; id++ {
		pkg := (id % 4) / 2
		core := id % 2
		first := pkg*2 + core
		t.CPUs = append(t.CPUs, CPU{
			ID: id, Package: pkg, Core: core, Node: pkg, APICID: uint32(pkg<<4 | core<<1 | id/4),
			Threads:     FormatCPUList([]int{first, first + 4}),
			PackageCPUs: FormatCPUList([]int{pkg * 2, pkg*2 + 1, pkg*2 + 4, pkg*2 + 5}),
		})
	}
	t.Nodes = []Node{
		{ID: 0, CPUs: "0-1,4-5", MemoryBytes: 1 << 30, Distances: []int{10, 21}},
		{ID: 1, CPUs: "2-3,6-7", MemoryBytes: 1 << 30, Distances: []int{21, 10}},
	}
	return t
}

func TestTopologyValidateAndFingerprint(t *testing.T) {
	host := twoSockets("0-7")
	if err := host.Validate(); err != nil {
		t.Fatalf("a two-socket host: %v", err)
	}
	parsed, err := Parse(host.Marshal())
	if err != nil || !reflect.DeepEqual(parsed, host) {
		t.Fatalf("Parse(Marshal()) = %+v, %v", parsed, err)
	}
	if got := host.AllowedNodes(); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("AllowedNodes() = %v, want both nodes", got)
	}
	bound := twoSockets("0-1,4-5")
	if got := bound.AllowedNodes(); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("AllowedNodes() of a daemon bound to node 0 = %v", got)
	}
	// A host whose daemon may use other CPUs, or whose nodes have other
	// amounts of memory, shows a guest the same layout.
	bound.Nodes[1].MemoryBytes = 2 << 30
	if bound.Fingerprint() != host.Fingerprint() {
		t.Errorf("fingerprints differ by allowed CPUs or node memory")
	}
	moved := twoSockets("0-7")
	moved.CPUs[3].Node, moved.CPUs[2].Node = 0, 0
	if moved.Fingerprint() == host.Fingerprint() {
		t.Errorf("a host whose CPUs are on other nodes has the same fingerprint")
	}
	if (Topology{Mode: Single}).Validate() != nil || (Topology{Mode: Single}).Fingerprint() != Single {
		t.Errorf("the single topology is invalid or fingerprinted as something else")
	}
	for name, bad := range map[string]Topology{
		"unknown mode":      {Mode: "numa"},
		"single with CPUs":  {Mode: Single, CPUs: host.CPUs},
		"host without CPUs": {Mode: Host, Allowed: "0"},
		"no allowed CPUs":   twoSockets(""),
		"allowed unknown":   twoSockets("0-8"),
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded", name)
		}
	}
}

func TestReadHostTopology(t *testing.T) {
	dir := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want := twoSockets("0-1,4-5")
	write("system/cpu/online", "0-7\n")
	var cpuinfo strings.Builder
	for _, cpu := range want.CPUs {
		top := fmt.Sprintf("system/cpu/cpu%d/topology/", cpu.ID)
		write(top+"physical_package_id", fmt.Sprintf("%d\n", cpu.Package))
		write(top+"die_id", "0\n")
		write(top+"core_id", fmt.Sprintf("%d\n", cpu.Core))
		write(top+"thread_siblings_list", cpu.Threads+"\n")
		write(top+"package_cpus_list", cpu.PackageCPUs+"\n")
		fmt.Fprintf(&cpuinfo, "processor\t: %d\nvendor_id\t: GenuineIntel\napicid\t\t: %d\n\n", cpu.ID, cpu.APICID)
	}
	for _, node := range want.Nodes {
		base := fmt.Sprintf("system/node/node%d/", node.ID)
		write(base+"cpulist", node.CPUs+"\n")
		write(base+"distance", strings.Trim(fmt.Sprint(node.Distances), "[]")+"\n")
		write(base+"meminfo", fmt.Sprintf("Node %d MemTotal:       %d kB\nNode %d MemFree: 1 kB\n", node.ID, node.MemoryBytes>>10, node.ID))
	}
	write("proc/cpuinfo", cpuinfo.String())
	sysRoot, procRoot = filepath.Join(dir, "system"), filepath.Join(dir, "proc")
	defer func() { sysRoot, procRoot = "/sys/devices/system", "/proc" }()
	got, err := readHostTopology([]int{0, 1, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readHostTopology() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPackageCounts(t *testing.T) {
	topology := twoSockets("0-7")
	for _, cpu := range []int{0, 3, 6} {
		if cpus, cores := topology.PackageCounts(cpu); cpus != 4 || cores != 2 {
			t.Errorf("PackageCounts(%d) = %d CPUs, %d cores; want 4, 2", cpu, cpus, cores)
		}
	}
	if cpus, cores := topology.PackageCounts(8); cpus != 1 || cores != 1 {
		t.Errorf("PackageCounts of an unknown CPU = %d CPUs, %d cores; want 1, 1", cpus, cores)
	}
}
