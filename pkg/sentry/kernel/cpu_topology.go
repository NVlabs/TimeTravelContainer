// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/cputopology"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel/sched"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sync"
)

// The largest host topology a kernel shows: a task's placement carries a mask
// of up to platform's 8192 CPUs, and a memory policy a node mask of 64 nodes.
const (
	maxHostTopologyCPUs  = 8192
	maxHostTopologyNodes = 64
)

// hostCPUTopology is the host CPU topology that a kernel shows its tasks.
// Their CPU numbers are the host's, their affinities confine their threads to
// those host CPUs, and their memory goes to the host memory node of the CPU
// that faults it unless a memory policy says otherwise.
type hostCPUTopology struct {
	cputopology.Topology

	// allowed are the host CPUs the sandbox may run on, as a CPUSet of all
	// the host's.
	allowed sched.CPUSet

	// nodeMask is a node mask of the host's memory nodes, and allowedNodes
	// of those of allowed.
	nodeMask     uint64
	allowedNodes uint64

	// placementMu protects placements.
	placementMu sync.Mutex
	// placements are the IDs of the placements tasks have, by their masks;
	// see Task.HostCPUPlacement.
	placements map[string]uint32
}

func newHostCPUTopology(t cputopology.Topology) (*hostCPUTopology, error) {
	if t.Mode != cputopology.Host {
		return nil, fmt.Errorf("CPU topology %q is not the host's", t.Mode)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if len(t.CPUs) > maxHostTopologyCPUs || len(t.Nodes) > maxHostTopologyNodes {
		return nil, fmt.Errorf("the host has %d CPUs and %d memory nodes; a host CPU topology has at most %d and %d", len(t.CPUs), len(t.Nodes), maxHostTopologyCPUs, maxHostTopologyNodes)
	}
	ht := &hostCPUTopology{
		Topology:   t,
		allowed:    sched.NewCPUSet(uint(len(t.CPUs))),
		placements: make(map[string]uint32),
	}
	for _, cpu := range t.AllowedCPUs() {
		ht.allowed.Set(uint(cpu))
	}
	for _, node := range t.Nodes {
		ht.nodeMask |= 1 << node.ID
	}
	for _, node := range t.AllowedNodes() {
		ht.allowedNodes |= 1 << node
	}
	return ht, nil
}

// placementID returns the ID of the placement on the CPUs of mask, the same
// for every equal mask; see platform.HostCPUPlacer.
func (ht *hostCPUTopology) placementID(mask []uint64) uint32 {
	key := make([]byte, 8*len(mask))
	for i, word := range mask {
		binary.LittleEndian.PutUint64(key[8*i:], word)
	}
	ht.placementMu.Lock()
	defer ht.placementMu.Unlock()
	id, ok := ht.placements[string(key)]
	if !ok {
		id = uint32(len(ht.placements) + 1)
		ht.placements[string(key)] = id
	}
	return id
}

// confine returns mask without the CPUs the sandbox may not use, as Linux
// limits a task's affinity to its cpuset.
func (ht *hostCPUTopology) confine(mask sched.CPUSet) sched.CPUSet {
	for i := range mask {
		if i < len(ht.allowed) {
			mask[i] &= ht.allowed[i]
		} else {
			mask[i] = 0
		}
	}
	return mask
}

// HostStatFiles are the host's /proc/stat and /proc/loadavg, from which a
// kernel that shows the host CPU topology shows its tasks how busy the host's
// CPUs are: under the host topology, as on Linux, an application may choose
// where to run its threads by what other work leaves idle.
type HostStatFiles struct {
	Stat, Loadavg *os.File
}

// sandboxHostStatFiles are the host's, for kernels under the host topology.
var sandboxHostStatFiles HostStatFiles

// SetSandboxHostStatFiles gives the kernels of this process's sandbox the
// host's /proc/stat and /proc/loadavg.
func SetSandboxHostStatFiles(files HostStatFiles) {
	sandboxHostStatFiles = files
}

// readHostStatFile returns the contents of a host /proc file, read in one
// pread(2) as a seq_file generates it whole, or nil.
func readHostStatFile(f *os.File) []byte {
	if f == nil {
		return nil
	}
	for size := 64 << 10; size <= 16<<20; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Pread(int(f.Fd()), buf, 0)
		if err != nil {
			return nil
		}
		if n < size {
			return buf[:n]
		}
	}
	return nil
}

// HostCPUStat returns the lines of the host's /proc/stat that tell how its
// CPUs have spent their time, "cpu" and "cpuN", if k shows the host CPU
// topology; or nil.
func (k *Kernel) HostCPUStat() []byte {
	if k.hostTopology == nil {
		return nil
	}
	data := readHostStatFile(sandboxHostStatFiles.Stat)
	var out bytes.Buffer
	for line := range bytes.Lines(data) {
		if bytes.HasPrefix(line, []byte("cpu")) {
			out.Write(line)
		}
	}
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

// HostLoadavg returns the host's load averages over 1, 5 and 15 minutes, as
// /proc/loadavg shows them, if k shows the host CPU topology.
func (k *Kernel) HostLoadavg() (string, bool) {
	if k.hostTopology == nil {
		return "", false
	}
	fields := strings.Fields(string(readHostStatFile(sandboxHostStatFiles.Loadavg)))
	if len(fields) < 3 {
		return "", false
	}
	return strings.Join(fields[:3], " "), true
}

// taskPlacement is what Task.HostCPUPlacement returns.
type taskPlacement struct {
	id   uint32
	mask []uint64
}

// setHostCPUTopology makes k show its tasks the host topology t.
func (k *Kernel) setHostCPUTopology(t cputopology.Topology) error {
	ht, err := newHostCPUTopology(t)
	if err != nil {
		return err
	}
	k.hostTopology = ht
	k.cpuTopology, k.cpuTopologyFingerprint = cputopology.Host, t.Fingerprint()
	return nil
}

// HostCPUTopology returns the host CPU topology k shows its tasks, or nil if
// it shows them the single topology: one socket and one memory node, whose
// CPUs are the sandbox's own, wherever the host runs them.
func (k *Kernel) HostCPUTopology() *cputopology.Topology {
	if k.hostTopology == nil {
		return nil
	}
	return &k.hostTopology.Topology
}

// HostCPUs returns the host CPUs, as a CPUSet of all the host's, that k's
// tasks may run on, or nil if k does not show the host topology.
func (k *Kernel) HostCPUs() sched.CPUSet {
	if k.hostTopology == nil {
		return nil
	}
	return k.hostTopology.allowed.Copy()
}

// HostNodes returns node masks of the host's memory nodes and of those the
// sandbox's CPUs are on; zero if k does not show the host topology.
func (k *Kernel) HostNodes() (all, allowed uint64) {
	if k.hostTopology == nil {
		return 0, 0
	}
	return k.hostTopology.nodeMask, k.hostTopology.allowedNodes
}

// defaultCPUMask returns the CPU mask a task starts with when it inherits
// none: every CPU the sandbox may run on.
func (k *Kernel) defaultCPUMask() sched.CPUSet {
	if k.hostTopology != nil {
		return k.hostTopology.allowed.Copy()
	}
	return sched.NewFullCPUSet(k.applicationCores)
}

// sandboxCPUTopology is the CPU topology the sandbox this process runs
// offers kernels it loads from checkpoints; see SetSandboxCPUTopology.
var sandboxCPUTopology *cputopology.Topology

// SetSandboxCPUTopology sets the CPU topology this process's sandbox offers
// the kernels it loads from checkpoints, nil being the single one: a kernel
// shows its tasks this topology if it was saved with it. A kernel saved with
// the single topology keeps it: its tasks have sized what they keep per CPU
// by its CPU numbers, which must not grow. A kernel saved with the host
// topology of a host whose CPUs, cores, packages and nodes this host does not
// number the same way keeps its tasks' CPU numbers, but they no longer place
// the tasks anywhere. Under the host topology, each task's affinity keeps the
// CPUs the sandbox may use here, or all of them if it has none of them, as
// Linux's does when its cpuset changes.
func SetSandboxCPUTopology(t *cputopology.Topology) {
	sandboxCPUTopology = t
}

// restoreCPUTopology makes k, loaded from a checkpoint, show its tasks the
// sandbox's CPU topology, if it was saved with it.
func (k *Kernel) restoreCPUTopology() {
	k.hostTopology = nil
	t := sandboxCPUTopology
	host := t != nil && t.Mode == cputopology.Host
	switch {
	case k.cpuTopology != cputopology.Host:
		if host {
			log.Infof("The kernel was saved with the single CPU topology, which it keeps")
		}
		return
	case !host:
		log.Warningf("The kernel was saved with the host CPU topology, which the sandbox restoring it does not show: its tasks keep their CPU numbers, which no longer place them")
		return
	case t.Fingerprint() != k.cpuTopologyFingerprint || uint(len(t.CPUs)) != k.applicationCores:
		log.Warningf("The kernel was saved with the host CPU topology of a host this one differs from: its tasks keep their CPU numbers, which no longer place them")
		return
	}
	if err := k.setHostCPUTopology(*t); err != nil {
		log.Warningf("Showing the host CPU topology to a restored kernel: %v", err)
	}
}

// cpuMaskLocked returns t's CPU mask: under the host topology, without the
// CPUs the sandbox may not use, which a mask saved on another host may have,
// or every one it may use if it has none of them.
//
// Preconditions: t.mu is locked.
func (t *Task) cpuMaskLocked() sched.CPUSet {
	ht := t.k.hostTopology
	if ht == nil {
		return t.allowedCPUMask.Copy()
	}
	mask := ht.confine(t.allowedCPUMask.Copy())
	if mask.NumCPUs() == 0 {
		mask = ht.allowed.Copy()
	}
	return mask
}

// HostCPUPlacement implements platform.HostCPUPlacer.HostCPUPlacement.
func (t *Task) HostCPUPlacement() (uint32, []uint64) {
	ht := t.k.hostTopology
	if ht == nil {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.placement == nil {
		cpus := t.cpuMaskLocked()
		mask := make([]uint64, (len(cpus)+7)/8)
		for i, b := range cpus {
			mask[i/8] |= uint64(b) << (8 * (i % 8))
		}
		t.placement = &taskPlacement{id: ht.placementID(mask), mask: mask}
	}
	return t.placement.id, t.placement.mask
}

// hostCPU returns the host CPU that last ran t's application thread, and its
// memory node, under the host topology.
func (t *Task) hostCPU() (cpu, node int32, ok bool) {
	ht := t.k.hostTopology
	if ht == nil {
		return 0, 0, false
	}
	hc, ok := t.p.(platform.HostCPUContext)
	if !ok {
		return 0, 0, false
	}
	cpu, _, ok = hc.LastHostCPU()
	// The stub that records it runs in memory the application can write.
	if !ok || cpu < 0 || int(cpu) >= len(ht.CPUs) {
		return 0, 0, false
	}
	return cpu, int32(ht.NodeOf(int(cpu))), true
}

// CPUNode returns the CPU t runs on, as Task.CPU does, and its memory node.
func (t *Task) CPUNode() (cpu, node int32) {
	cpu = t.CPU()
	if ht := t.k.hostTopology; ht != nil {
		node = int32(ht.NodeOf(int(cpu)))
	}
	return cpu, node
}

// cpuTopologyView returns the CPU topology t sees, for CPUID.
func (t *Task) cpuTopologyView() cpuid.CPUTopologyView {
	if ht := t.k.hostTopology; ht != nil {
		cpu := t.CPU()
		c := ht.CPUs[cpu]
		return cpuid.CPUTopologyView{Host: true, CPU: uint32(cpu), NumCPUs: uint32(len(ht.CPUs)), APICID: c.APICID, Core: uint32(c.Core), Node: uint32(c.Node)}
	}
	cpu := uint32(t.CPU())
	return cpuid.CPUTopologyView{CPU: cpu, NumCPUs: uint32(t.k.applicationCores), APICID: cpu}
}

// memoryPolicy returns t's memory policy for the memory it allocates, under
// the host topology.
func (t *Task) memoryPolicy() any {
	if t.k.hostTopology == nil {
		return nil
	}
	_, node := t.CPUNode()
	policy, nodes := linux.NumaPolicy(t.numaPolicyNow.Load()), t.numaNodeMaskNow.Load()
	if policy&^linux.MPOL_MODE_FLAGS == linux.MPOL_LOCAL {
		policy = linux.MPOL_DEFAULT
	}
	return pgalloc.MemoryPolicy{Mode: policy, Nodes: nodes, LocalNode: int(node)}
}
