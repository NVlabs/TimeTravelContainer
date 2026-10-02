// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"fmt"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

// testExternalHost is the host address of the tests' external memory. Nothing
// maps it: the tests' VMs only record their mappings.
const testExternalHost = uintptr(0x7000_0000_0000)

func TestExternalFrames(t *testing.T) {
	vm := &ramTestVM{}
	revoker := &ramTestRevoker{}
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: newRAMTestHost(), Revoker: revoker, IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}

	// The frames of two pages in one host slab map the whole slab, readable
	// and writable, never executable.
	frames, err := ram.externalFrames(testExternalHost+uintptr(GuestFrameSize), 2*GuestFrameSize)
	if err != nil {
		t.Fatalf("externalFrames: %v", err)
	}
	if len(vm.maps) != 1 || vm.maps[0] != (ramTestMap{gpa: 0x100000, address: testExternalHost, length: HostSlabSize, access: Stage2Read | Stage2Write}) {
		t.Fatalf("stage-2 mappings %+v, want the slab at 0x100000, read-write", vm.maps)
	}
	if len(frames) != 2 || frames[0].GPA != 0x100000+GuestFrameSize || frames[1].GPA != 0x100000+2*GuestFrameSize || frames[0].ID == frames[1].ID {
		t.Fatalf("frames %+v", frames)
	}
	// Frames of the same memory are the same frames, and a range that
	// crosses into the next slab maps only that slab.
	again, err := ram.externalFrames(testExternalHost+uintptr(2*GuestFrameSize), 4*GuestFrameSize)
	if err != nil {
		t.Fatalf("externalFrames: %v", err)
	}
	if again[0].ID != frames[1].ID {
		t.Errorf("the same page has frames %d and %d", frames[1].ID, again[0].ID)
	}
	if len(vm.maps) != 2 || vm.maps[1].address != testExternalHost+uintptr(HostSlabSize) || vm.maps[1].access != Stage2Read|Stage2Write {
		t.Fatalf("stage-2 mappings %+v", vm.maps)
	}
	if m := ram.Metrics(); m.HostResidentBytes != 0 || m.IPABytesConsumed != 2*HostSlabSize {
		t.Errorf("metrics %+v: external memory is not resident in GuestRAM, but consumes IPA", m)
	}
	if _, err := ram.externalFrames(testExternalHost+1, GuestFrameSize); err == nil {
		t.Error("externalFrames accepted an unaligned address")
	}

	// A new epoch maps external slabs read-write too.
	next := &ramTestVM{}
	if err := ram.RebindStage2(next); err != nil {
		t.Fatalf("RebindStage2: %v", err)
	}
	for _, m := range next.maps {
		if m.access != Stage2Read|Stage2Write {
			t.Errorf("the new epoch maps %+v", m)
		}
	}
	rebound, err := ram.externalFrames(testExternalHost, GuestFrameSize)
	if err != nil {
		t.Fatal(err)
	}
	if want := next.maps[0].gpa; rebound[0].GPA != want {
		t.Errorf("after the new epoch, the frame is at %#x, want %#x", rebound[0].GPA, want)
	}

	// Release revokes the stage-1 mappings, and unmaps the slabs.
	ref := MappingRef{AddressSpaceID: 1, VirtualAddress: 0x1000}
	if err := ram.AddMapping(frames[0].ID, ref); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if err := ram.releaseExternal(testExternalHost, 2*HostSlabSize); err != nil {
		t.Fatalf("releaseExternal: %v", err)
	}
	if revoker.callCount() != 1 || fmt.Sprint(revoker.calls[0][frames[0].ID]) != fmt.Sprint([]MappingRef{ref}) {
		t.Errorf("revocations %+v", revoker.calls)
	}
	if len(next.unmaps) != 2 {
		t.Errorf("stage-2 unmappings %+v, want both slabs", next.unmaps)
	}
	if err := ram.AddMapping(frames[1].ID, ref); err == nil {
		t.Error("mapped a frame of released memory")
	}
	if len(ram.external) != 0 || len(ram.slabs) != 0 {
		t.Errorf("%d external slabs and %d slabs remain", len(ram.external), len(ram.slabs))
	}
}

func TestCompactionSkipsExternalSlabs(t *testing.T) {
	ram, backing, _, host, _ := newRAMTestBacking(t, 1<<24)
	// Two half-full slabs, which compaction packs into one.
	for _, offset := range []uint64{0, 2 * GuestFrameSize, 4 * GuestFrameSize, 6 * GuestFrameSize, 8 * GuestFrameSize} {
		if _, err := backing.ResolveFrames(memmap.FileRange{Start: offset, End: offset + GuestFrameSize}, hostarch.Read); err != nil {
			t.Fatal(err)
		}
	}
	if err := backing.Decommit(memmap.FileRange{Start: 2 * GuestFrameSize, End: 3 * GuestFrameSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := ram.externalFrames(testExternalHost, 2*HostSlabSize); err != nil {
		t.Fatal(err)
	}
	if _, err := ram.Compact(context.Background()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for _, c := range host.copies {
		if c.src >= testExternalHost || c.dst >= testExternalHost {
			t.Errorf("compaction copied external memory: %+v", c)
		}
	}
	if len(ram.external) != 2 {
		t.Errorf("%d external slabs remain, want 2", len(ram.external))
	}
	for _, slab := range ram.external {
		if slab.inPartial || ram.allocationSlab == slab {
			t.Error("an external slab is a candidate for allocation")
		}
	}
}

// testHostMemory is a memmap.HostMemoryFile at testExternalHost.
type testHostMemory struct {
	memmap.NoBufferedIOFallback
	memmap.DefaultMemoryType
	size  uint64
	hooks []func()
}

func (*testHostMemory) IncRef(memmap.FileRange, uint32) {}
func (*testHostMemory) DecRef(memmap.FileRange)         {}
func (*testHostMemory) MapInternal(memmap.FileRange, hostarch.AccessType) (safemem.BlockSeq, error) {
	return safemem.BlockSeq{}, fmt.Errorf("not mapped")
}
func (*testHostMemory) DataFD(memmap.FileRange) (int, error) { return -1, fmt.Errorf("no descriptor") }
func (*testHostMemory) FD() int                              { return -1 }
func (m *testHostMemory) HostAddress(fr memmap.FileRange) (uintptr, error) {
	if fr.End > m.size {
		return 0, fmt.Errorf("%v is beyond %d bytes", fr, m.size)
	}
	return testExternalHost + uintptr(fr.Start), nil
}
func (m *testHostMemory) OnRelease(f func()) { m.hooks = append(m.hooks, f) }

func (m *testHostMemory) release() {
	for _, f := range m.hooks {
		f()
	}
	m.hooks = nil
}

func TestMapHostMemoryInPlace(t *testing.T) {
	vm := &relocationTestVM{}
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: newRAMTestHost(), IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p := &Platform{
		engine:       newWorkerPoolTestEngine(pool),
		ram:          ram,
		spaces:       make(map[uint64]*addressSpace),
		staging:      &stagingCache{},
		hostMemories: make(map[memmap.HostMemoryFile]hostRange),
	}
	p.monitor = NewMonitor(p.engine, monitorPoolExpander(vm))
	as := indexed(&addressSpace{id: 1, platform: p, mappings: make(map[hostarch.Addr]addressMapping)})
	p.spaces[as.id] = as
	ram.SetRevoker(p)

	file := &testHostMemory{size: 2 * HostSlabSize}
	const addr = hostarch.Addr(0x400000)
	fr := memmap.FileRange{Start: GuestFrameSize, End: 3 * GuestFrameSize}
	if err := as.MapFile(addr, file, fr, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	// The guest's pages are the host memory's, not staged copies.
	vm.mu.Lock()
	var applied []monitorabi.Operation
	for _, c := range vm.commands {
		if c.Opcode == monitorabi.OpcodeApply {
			applied = append(applied, c.Operations...)
		}
	}
	vm.mu.Unlock()
	if len(applied) != 1 || applied[0].VA != uint64(addr) || applied[0].GPA != 0x100000+GuestFrameSize || applied[0].Length != 2*GuestFrameSize {
		t.Errorf("monitor operations %+v, want the external frames", applied)
	}
	if len(file.hooks) != 1 {
		t.Errorf("%d release hooks, want 1", len(file.hooks))
	}
	if err := as.MapFile(addr, file, fr, hostarch.AnyAccess, false); err == nil {
		t.Error("mapped shared host memory executable")
	}
	// Memory that is not whole host slabs is refused.
	short := &testHostMemory{size: HostSlabSize + GuestFrameSize}
	if err := as.MapFile(addr+0x100000, short, memmap.FileRange{Start: HostSlabSize, End: HostSlabSize + GuestFrameSize}, hostarch.Read, false); err == nil {
		t.Error("mapped shared host memory that ends within a host slab")
	}

	as.Unmap(addr, fr.Length())
	file.release()
	if len(ram.external) != 0 {
		t.Errorf("%d external slabs remain after release", len(ram.external))
	}
	if _, ok := p.hostMemories[file]; ok {
		t.Error("the platform still tracks released memory")
	}
	var _ pgalloc.FrameID
}
