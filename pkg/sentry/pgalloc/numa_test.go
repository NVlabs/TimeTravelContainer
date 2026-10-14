// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

func newNUMATestMemoryFile(nodes int) *MemoryFile {
	f := &MemoryFile{
		opts: MemoryFileOpts{DisableMemoryAccounting: true, NUMANodes: nodes},
	}
	f.initFields()
	return f
}

// allocateOn allocates length bytes of f, as Allocate does, on node if valid.
func allocateOn(t *testing.T, f *MemoryFile, length uint64, dir Direction, node int, valid bool) memmap.FileRange {
	t.Helper()
	alloc := allocState{
		length: length,
		opts:   AllocOpts{Dir: dir, Placement: NUMAPlacement{Valid: valid, Node: node}},
	}
	alloc.node = f.placementNode(&alloc.opts)
	fr, err := f.findAllocatableAndMarkUsed(&alloc)
	if err != nil {
		t.Fatalf("allocating %d bytes on node %d: %v", length, node, err)
	}
	return fr
}

// nodeOf returns the node, as chunkInfo.node, of the chunks fr is in, which
// must all have one.
func nodeOf(t *testing.T, f *MemoryFile, fr memmap.FileRange) int32 {
	t.Helper()
	chunks := f.chunksLoad()
	node := chunks[fr.Start/chunkSize].node
	for i := fr.Start / chunkSize; i <= (fr.End-1)/chunkSize; i++ {
		if chunks[i].node != node {
			t.Fatalf("%v spans chunks of nodes %d and %d", fr, node-1, chunks[i].node-1)
		}
	}
	return node
}

func TestNUMAPlacementKeepsNodesApart(t *testing.T) {
	f := newNUMATestMemoryFile(2)
	a := allocateOn(t, f, 2*page, BottomUp, 1, true)
	b := allocateOn(t, f, 2*page, BottomUp, 0, true)
	c := allocateOn(t, f, 2*page, BottomUp, 1, true)
	d := allocateOn(t, f, 2*page, TopDown, 0, true)
	for _, test := range []struct {
		fr   memmap.FileRange
		node int32
	}{{a, 2}, {b, 1}, {c, 2}, {d, 1}} {
		if got := nodeOf(t, f, test.fr); got != test.node {
			t.Errorf("%v is on node %d, want %d", test.fr, got-1, test.node-1)
		}
	}
	if c.Start != a.End {
		t.Errorf("the second allocation on node 1 is %v, want it after %v", c, a)
	}
	if a.Start/chunkSize == b.Start/chunkSize {
		t.Errorf("nodes 1 and 0 share a chunk: %v and %v", a, b)
	}
	if d.End != (b.Start/chunkSize+1)*chunkSize {
		t.Errorf("a top-down allocation on node 0 is %v, want it at the end of %v's chunk", d, b)
	}
	if got := len(f.chunksLoad()); got != 2 {
		t.Errorf("%d chunks, want one a node", got)
	}
}

func TestNUMAPlacementLargerThanAChunk(t *testing.T) {
	f := newNUMATestMemoryFile(2)
	allocateOn(t, f, 2*page, BottomUp, 0, true)
	big := allocateOn(t, f, chunkSize+2*page, BottomUp, 1, true)
	if got := nodeOf(t, f, big); got != 2 {
		t.Errorf("%v is on node %d, want 1", big, got-1)
	}
	if big.Start%chunkSize != 0 {
		t.Errorf("%v does not start a new chunk", big)
	}
}

func TestNUMAPlacementUnplacedAndDisabled(t *testing.T) {
	f := newNUMATestMemoryFile(2)
	a := allocateOn(t, f, 2*page, BottomUp, 1, true)
	// An allocation that asks for no node takes room on any.
	if b := allocateOn(t, f, 2*page, BottomUp, 0, false); b.Start != a.End {
		t.Errorf("an unplaced allocation is %v, want it after %v", b, a)
	}
	// A node the file does not have is none.
	if fr := allocateOn(t, f, 2*page, BottomUp, 5, true); nodeOf(t, f, fr) != 2 {
		t.Errorf("an allocation for a node the file does not place on is in chunk %d", fr.Start/chunkSize)
	}
	off := newNUMATestMemoryFile(0)
	if fr := allocateOn(t, off, 2*page, BottomUp, 1, true); nodeOf(t, off, fr) != 0 {
		t.Errorf("a file that places no allocation bound %v to a node", fr)
	}
}

func TestMemoryPolicyPlacement(t *testing.T) {
	const local = 1
	for _, test := range []struct {
		name       string
		task       MemoryPolicy
		mode       linux.NumaPolicy
		nodes      uint64
		idx        uint64
		wantNode   int
		wantPlaced bool
	}{
		{name: "default", task: MemoryPolicy{LocalNode: local}, wantNode: local, wantPlaced: true},
		{name: "task prefers", task: MemoryPolicy{Mode: linux.MPOL_PREFERRED, Nodes: 1 << 2, LocalNode: local}, wantNode: 2, wantPlaced: true},
		{name: "task binds with local", task: MemoryPolicy{Mode: linux.MPOL_BIND, Nodes: 1<<0 | 1<<1, LocalNode: local}, wantNode: local, wantPlaced: true},
		{name: "task binds elsewhere", task: MemoryPolicy{Mode: linux.MPOL_BIND, Nodes: 1<<2 | 1<<3, LocalNode: local}, wantNode: 2, wantPlaced: true},
		{name: "mapping overrides task", task: MemoryPolicy{Mode: linux.MPOL_PREFERRED, Nodes: 1 << 2, LocalNode: local}, mode: linux.MPOL_PREFERRED, nodes: 1 << 3, wantNode: 3, wantPlaced: true},
		{name: "mapping local", task: MemoryPolicy{Mode: linux.MPOL_PREFERRED, Nodes: 1 << 2, LocalNode: local}, mode: linux.MPOL_LOCAL, wantNode: local, wantPlaced: true},
		{name: "interleave 0", task: MemoryPolicy{Mode: linux.MPOL_INTERLEAVE, Nodes: 1<<1 | 1<<3, LocalNode: local}, idx: 0, wantNode: 1, wantPlaced: true},
		{name: "interleave 1", task: MemoryPolicy{Mode: linux.MPOL_INTERLEAVE, Nodes: 1<<1 | 1<<3, LocalNode: local}, idx: 1, wantNode: 3, wantPlaced: true},
		{name: "interleave 2", task: MemoryPolicy{Mode: linux.MPOL_INTERLEAVE | linux.MPOL_F_STATIC_NODES, Nodes: 1<<1 | 1<<3, LocalNode: local}, idx: 2, wantNode: 1, wantPlaced: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.task.Placement(test.mode, test.nodes, test.idx)
			if got.Valid != test.wantPlaced || got.Node != test.wantNode {
				t.Errorf("Placement() = %+v, want node %d (placed %t)", got, test.wantNode, test.wantPlaced)
			}
		})
	}
}
