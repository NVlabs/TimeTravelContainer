// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
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
	alloc := f.newAllocState(length, AllocOpts{Dir: dir, Placement: NUMAPlacement{Valid: valid, Node: node}})
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
	// An allocation that asks for no node takes room in chunks bound to
	// none: a chunk bound to a node holds only pages placed there.
	b := allocateOn(t, f, 2*page, BottomUp, 0, false)
	if nodeOf(t, f, b) != 0 || b.Start/chunkSize == a.Start/chunkSize {
		t.Errorf("an unplaced allocation is %v, in a chunk of node %d; want an unbound chunk", b, nodeOf(t, f, b)-1)
	}
	// A node the file does not have is none.
	if fr := allocateOn(t, f, 2*page, BottomUp, 5, true); nodeOf(t, f, fr) != 0 || fr.Start != b.End {
		t.Errorf("an allocation for a node the file does not place on is %v, want it after %v", fr, b)
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

// A sparse unit that a fault asks to place before it is loaded is bound once,
// before its loader fills it.
func TestPlaceUnloadedBindsAnUnloadedUnitOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("binding memory to NUMA nodes is Linux's, as /proc/self/maps, which shows it")
	}
	const pagesPerChunk = SparseChunkBytes / hostarch.PageSize
	source, sourceFR := newRemoteTestMemoryFile(t, pagesPerChunk)
	fillRemoteTestRange(t, source, sourceFR, bytes.Repeat([]byte{7}, int(sourceFR.Length())))
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	pages, err := StartAsyncPagesFileLoadWithOpts(NewLoopbackRemoteReader(snapshot, SparseChunkBytes, 1, 2), nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	// Node 0 exists on every host.
	mf.opts.NUMANodes = 1
	install := sparseTestInstall(1)
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	unit, ok := mf.SparseChunkRange(install.Object)
	if !ok {
		t.Fatal("installed chunk is not resident")
	}
	if err := mf.AttachSparseChunkPageLoad(pages, []RemoteLoadRange{{Target: unit, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	placed := func() bool {
		amfl := (*mf.sparseAsyncLoads.Load())[0]
		amfl.pf.mu.Lock()
		defer amfl.pf.mu.Unlock()
		seg := amfl.unloaded.FindSegment(unit.Start)
		return seg.Ok() && seg.ValuePtr().placed && seg.Range() == unit
	}
	// The file's mappings, which a binding of part of one would split. Those
	// of earlier tests' files, which go once their releasers have ended, do
	// not count.
	mappings := func() int {
		maps, err := os.ReadFile("/proc/self/maps")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(maps), " "+mf.File().Name()+" (deleted)\n")
	}
	before := mappings()
	// A page of the unit places all of it.
	mf.PlaceUnloaded(memmap.FileRange{Start: unit.Start + hostarch.PageSize, End: unit.Start + 2*hostarch.PageSize}, NUMAPlacement{Valid: true, Node: 0})
	if !placed() {
		t.Fatal("the unit was not placed")
	}
	if after := mappings(); after != before {
		t.Errorf("placing the unit made the file's %d mappings %d", before, after)
	}
	// Asking again changes nothing, and the unit loads as before.
	mf.PlaceUnloaded(unit, NUMAPlacement{Valid: true, Node: 0})
	dsts, err := mf.MapInternal(unit, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, unit.Length())
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), dsts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{7}, int(unit.Length()))) {
		t.Fatal("the placed unit did not load its pages")
	}
}
