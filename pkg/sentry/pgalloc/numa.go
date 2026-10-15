// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"math"
	"math/bits"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// NUMAPlacement asks for the pages of an allocation on one host memory node.
//
// A MemoryFile whose MemoryFileOpts.NUMANodes is not zero binds each chunk it
// places on a node to that node (mbind(2) MPOL_PREFERRED on its mapping,
// which for a shared memory file binds the file's pages, however they are
// mapped), and serves an allocation that asks for a node from that node's
// chunks only. The pages are then on the node whichever thread touches them
// first, and stay there when the file is saved and loaded again, as a
// checkpoint or a fork does, wherever the node exists.
type NUMAPlacement struct {
	// Valid is true if the allocation asks for Node.
	Valid bool
	// Node is the host memory node.
	Node int
}

// MemoryPolicy is the memory policy of a task that allocates memory under a
// host CPU topology, where it is on the host's memory nodes.
type MemoryPolicy struct {
	// Mode is the task's memory policy (set_mempolicy(2)), without mode
	// flags, and Nodes the node mask of its nodes.
	Mode  linux.NumaPolicy
	Nodes uint64
	// LocalNode is the memory node of the host CPU the task last ran on.
	LocalNode int
}

// Placement returns where an allocation for the page at addr, as page index
// idx, goes under the policy of mode and nodes, which a mapping's policy
// (mbind(2)) gives or else p's: on the local node unless the policy says
// otherwise, as Linux places a page on the node of the CPU that faults it.
// Interleaving goes by huge page, the unit of allocation.
func (p MemoryPolicy) Placement(mode linux.NumaPolicy, nodes uint64, idx uint64) NUMAPlacement {
	mode &^= linux.MPOL_MODE_FLAGS
	if mode == linux.MPOL_DEFAULT {
		mode, nodes = p.Mode&^linux.MPOL_MODE_FLAGS, p.Nodes
	}
	local := NUMAPlacement{Valid: true, Node: p.LocalNode}
	if nodes == 0 {
		return local
	}
	switch mode {
	case linux.MPOL_PREFERRED:
		return NUMAPlacement{Valid: true, Node: bits.TrailingZeros64(nodes)}
	case linux.MPOL_BIND:
		if p.LocalNode < 64 && nodes&(1<<p.LocalNode) != 0 {
			return local
		}
		return NUMAPlacement{Valid: true, Node: bits.TrailingZeros64(nodes)}
	case linux.MPOL_INTERLEAVE:
		n := idx % uint64(bits.OnesCount64(nodes))
		for ; n > 0; n-- {
			nodes &= nodes - 1
		}
		return NUMAPlacement{Valid: true, Node: bits.TrailingZeros64(nodes)}
	}
	return local
}

// placementNode returns the chunkInfo.node that opts asks for, or 0 if it asks
// for none or f places no allocation.
func (f *MemoryFile) placementNode(opts *AllocOpts) int32 {
	p := opts.Placement
	if !p.Valid || p.Node < 0 || p.Node >= f.opts.NUMANodes || f.backing != nil {
		return 0
	}
	return int32(p.Node) + 1
}

// PlaceUnloaded asks for the pages of fr that an asynchronous load has yet to
// start filling, with the rest of each sparse unit they are in, to go to p's
// node: a page pulled into a remote fork, or loaded into a restored branch,
// goes to the node of the task whose fault brings it in, as Linux puts a page
// read from a file on the node of the task that faults it in. Pages loaded
// or being loaded already stay where they are.
func (f *MemoryFile) PlaceUnloaded(fr memmap.FileRange, p NUMAPlacement) {
	node := f.placementNode(&AllocOpts{Placement: p})
	if node == 0 || f.file == nil {
		return
	}
	var loads []*asyncMemoryFileLoad
	if amfl := f.asyncPageLoad.Load(); amfl != nil {
		loads = append(loads, amfl)
	}
	if sparse := f.sparseAsyncLoads.Load(); sparse != nil {
		loads = append(loads, *sparse...)
	}
	for _, amfl := range loads {
		if fr.End <= amfl.minUnloaded.Load() {
			continue
		}
		amfl.pf.mu.Lock()
		for seg := amfl.unloaded.LowerBoundSegment(fr.Start); seg.Ok() && seg.Start() < fr.End; seg = seg.NextSegment() {
			if ul := seg.ValuePtr(); ul.started || ul.placed {
				continue
			}
			if seg.ValuePtr().unit == 0 {
				seg = amfl.unloaded.Isolate(seg, fr)
			}
			seg.ValuePtr().placed = true
			bindFileRange(f.file.Fd(), seg.Range(), int(node-1))
		}
		amfl.pf.mu.Unlock()
	}
}

// adviseChunk advises the host of the pages of the chunk mapped at addr: of
// their size, and of the node they are bound to.
func (f *MemoryFile) adviseChunk(addr uintptr, huge bool, node int32) {
	f.madviseChunkMapping(addr, chunkSize, huge)
	if node != 0 && int(node) <= f.opts.NUMANodes {
		bindChunkMapping(addr, chunkSize, int(node-1))
	}
}

// gapIterator is what the gap iterators of unfreeSet and unwasteSet have in
// common.
type gapIterator[G any] interface {
	Ok() bool
	Start() uint64
	End() uint64
	NextLargeEnoughGap(minSize uint64) G
	PrevLargeEnoughGap(minSize uint64) G
}

// largeEnoughGapSet is what unfreeSet and unwasteSet have in common.
type largeEnoughGapSet[G any] interface {
	LowerBoundLargeEnoughGap(min, minSize uint64) G
	UpperBoundLargeEnoughGap(max, minSize uint64) G
}

// fitGap returns the length bytes of [start, end) that an allocation in dir
// takes, aligned to huge pages if huge: a huge allocation that straddled two
// huge pages would be backed by neither, and its 2 MiB pages (see
// AllocOpts.LogicalHugePages) could not be.
func fitGap(start, end uint64, huge bool, length uint64, dir Direction) (memmap.FileRange, bool) {
	if huge {
		start, end = hostarch.MustHugePageRoundUp(start), hostarch.HugePageRoundDown(end)
	}
	if end <= start || end-start < length {
		return memmap.FileRange{}, false
	}
	if dir == BottomUp {
		return memmap.FileRange{Start: start, End: start + length}, true
	}
	return memmap.FileRange{Start: end - length, End: end}, true
}

// findGap returns length bytes of a gap of set, the first such in dir that
// fitGap finds room in.
func findGap[G gapIterator[G], S largeEnoughGapSet[G]](set S, huge bool, length uint64, dir Direction) (memmap.FileRange, bool) {
	if dir == BottomUp {
		for gap := set.LowerBoundLargeEnoughGap(0, length); gap.Ok(); gap = gap.NextLargeEnoughGap(length) {
			if fr, ok := fitGap(gap.Start(), gap.End(), huge, length, dir); ok {
				return fr, true
			}
		}
		return memmap.FileRange{}, false
	}
	for gap := set.UpperBoundLargeEnoughGap(math.MaxUint64, length); gap.Ok(); gap = gap.PrevLargeEnoughGap(length) {
		if fr, ok := fitGap(gap.Start(), gap.End(), huge, length, dir); ok {
			return fr, true
		}
	}
	return memmap.FileRange{}, false
}

// findNodeGap returns length bytes of a gap of set that lie in a run of
// consecutive chunks of chunks that are bound to node and of the given
// hugeness, the first such in dir that fitGap finds room in.
func findNodeGap[G gapIterator[G], S largeEnoughGapSet[G]](set S, chunks []chunkInfo, node int32, huge bool, length uint64, dir Direction) (memmap.FileRange, bool) {
	matches := func(i int) bool {
		return i >= 0 && i < len(chunks) && chunks[i].node == node && chunks[i].huge == huge
	}
	if dir == BottomUp {
		for i := 0; i < len(chunks); {
			if !matches(i) {
				i++
				continue
			}
			j := i + 1
			for matches(j) {
				j++
			}
			lo, hi := uint64(i)*chunkSize, uint64(j)*chunkSize
			gap := set.LowerBoundLargeEnoughGap(lo, length)
			for ; gap.Ok() && gap.Start() < hi; gap = gap.NextLargeEnoughGap(length) {
				if fr, ok := fitGap(max(gap.Start(), lo), min(gap.End(), hi), huge, length, dir); ok {
					return fr, true
				}
			}
			if !gap.Ok() {
				break
			}
			// No chunk before gap's has room.
			i = max(j, int(gap.Start()/chunkSize))
		}
		return memmap.FileRange{}, false
	}
	for j := len(chunks); j > 0; {
		if !matches(j - 1) {
			j--
			continue
		}
		i := j - 1
		for matches(i - 1) {
			i--
		}
		lo, hi := uint64(i)*chunkSize, uint64(j)*chunkSize
		gap := set.UpperBoundLargeEnoughGap(hi-1, length)
		for ; gap.Ok() && gap.End() > lo; gap = gap.PrevLargeEnoughGap(length) {
			if fr, ok := fitGap(max(gap.Start(), lo), min(gap.End(), hi), huge, length, dir); ok {
				return fr, true
			}
		}
		if !gap.Ok() {
			break
		}
		// No chunk after gap's has room.
		j = min(i, int((gap.End()+chunkSize-1)/chunkSize))
	}
	return memmap.FileRange{}, false
}
