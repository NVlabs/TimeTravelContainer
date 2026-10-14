// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sys

import (
	"fmt"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cputopology"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// hostCPUInos is how many inodes a CPU's directory holds under the host CPU
// topology: itself, its node link, its topology, and the topology's thirteen
// files.
const hostCPUInos = 16

// hostCPUDir returns the directory of cpu, numbered from ino, under the host
// CPU topology t: its place in the host's packages, dies and cores, as Linux
// shows them, and a link to its memory node.
func (d *cpusDir) hostCPUDir(ctx context.Context, t *cputopology.Topology, cpu int, ino uint64) kernfs.Inode {
	c := t.CPUs[cpu]
	n := uint(len(t.CPUs))
	threads, _ := cputopology.ParseCPUList(c.Threads)
	pkg, _ := cputopology.ParseCPUList(c.PackageCPUs)
	var die []int
	for _, other := range pkg {
		if t.CPUs[other].Die == c.Die {
			die = append(die, other)
		}
	}
	file := func(off uint64, data string) kernfs.Inode {
		return d.fs.newStaticFileWithIno(ctx, d.owner, ino+off, defaultSysMode, data+"\n")
	}
	return d.fs.newDirWithIno(ctx, d.owner, ino, map[string]kernfs.Inode{
		fmt.Sprintf("node%d", c.Node): kernfs.NewStaticSymlink(ctx, d.owner, linux.UNNAMED_MAJOR, d.fs.devMinor, ino+1, fmt.Sprintf("../../node/node%d", c.Node)),
		"topology": d.fs.newDirWithIno(ctx, d.owner, ino+2, map[string]kernfs.Inode{
			"physical_package_id":  file(3, strconv.Itoa(c.Package)),
			"die_id":               file(4, strconv.Itoa(c.Die)),
			"core_id":              file(5, strconv.Itoa(c.Core)),
			"core_cpus":            file(6, cpuMaskString(threads, n)),
			"core_cpus_list":       file(7, cputopology.FormatCPUList(threads)),
			"thread_siblings":      file(8, cpuMaskString(threads, n)),
			"thread_siblings_list": file(9, cputopology.FormatCPUList(threads)),
			"core_siblings":        file(10, cpuMaskString(pkg, n)),
			"core_siblings_list":   file(11, cputopology.FormatCPUList(pkg)),
			"package_cpus":         file(12, cpuMaskString(pkg, n)),
			"package_cpus_list":    file(13, cputopology.FormatCPUList(pkg)),
			"die_cpus":             file(14, cpuMaskString(die, n)),
			"die_cpus_list":        file(15, cputopology.FormatCPUList(die)),
		}),
	})
}

// newHostNodeDir returns /sys/devices/system/node under the host CPU topology
// t: the host's memory nodes, their CPUs, distances and sizes.
func (fs *filesystem) newHostNodeDir(ctx context.Context, creds *auth.Credentials, t *cputopology.Topology) kernfs.Inode {
	n := uint(len(t.CPUs))
	var all, withCPUs, withMemory []int
	entries := make(map[string]kernfs.Inode)
	for _, node := range t.Nodes {
		all = append(all, node.ID)
		cpus, _ := cputopology.ParseCPUList(node.CPUs)
		if len(cpus) != 0 {
			withCPUs = append(withCPUs, node.ID)
		}
		if node.MemoryBytes != 0 {
			withMemory = append(withMemory, node.ID)
		}
		distances := make([]string, len(node.Distances))
		for i, distance := range node.Distances {
			distances[i] = strconv.Itoa(distance)
		}
		// The sandbox knows nothing of what else uses the node's memory.
		kb := node.MemoryBytes >> 10
		meminfo := fmt.Sprintf("Node %d MemTotal:       %8d kB\nNode %d MemFree:        %8d kB\nNode %d MemUsed:        %8d kB\n", node.ID, kb, node.ID, kb, node.ID, 0)
		entries[fmt.Sprintf("node%d", node.ID)] = fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
			"cpumap":   fs.newStaticFile(ctx, creds, defaultSysMode, cpuMaskString(cpus, n)+"\n"),
			"cpulist":  fs.newStaticFile(ctx, creds, defaultSysMode, cputopology.FormatCPUList(cpus)+"\n"),
			"distance": fs.newStaticFile(ctx, creds, defaultSysMode, strings.Join(distances, " ")+"\n"),
			"meminfo":  fs.newStaticFile(ctx, creds, defaultSysMode, meminfo),
		})
	}
	for name, nodes := range map[string][]int{
		"online":            all,
		"possible":          all,
		"has_cpu":           withCPUs,
		"has_memory":        withMemory,
		"has_normal_memory": withMemory,
	} {
		entries[name] = fs.newStaticFile(ctx, creds, defaultSysMode, cputopology.FormatCPUList(nodes)+"\n")
	}
	return fs.newDir(ctx, creds, defaultSysDirMode, entries)
}

// cpuMaskString returns a "hex format ASCII string" of the CPUs cpus of n, as
// Linux's lib/bitmap.c:bitmap_print_to_pagebuf(list=false) formats one: in
// groups of 32 bits, the most significant first.
func cpuMaskString(cpus []int, n uint) string {
	words := make([]uint32, (n+31)/32)
	for _, cpu := range cpus {
		if uint(cpu) < n {
			words[cpu/32] |= 1 << (cpu % 32)
		}
	}
	var b strings.Builder
	for i := len(words) - 1; i >= 0; i-- {
		bits := uint(32)
		if i == len(words)-1 && n%32 != 0 {
			bits = n % 32
		}
		if b.Len() != 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%0*x", (bits+3)/4, words[i])
	}
	return b.String()
}
