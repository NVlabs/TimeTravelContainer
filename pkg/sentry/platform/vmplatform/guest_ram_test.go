// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
	"unsafe"

	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

type ramTestVM struct {
	maps      []ramTestMap
	unmaps    []ramTestMap
	mapErr    error
	failMapAt int
}

type ramTestMap struct {
	gpa     uint64
	address uintptr
	length  uint64
	access  Stage2Access
}

func (v *ramTestVM) MapStage2(gpa uint64, address uintptr, length uint64, access Stage2Access) error {
	v.maps = append(v.maps, ramTestMap{gpa: gpa, address: address, length: length, access: access})
	if v.failMapAt != 0 && len(v.maps) == v.failMapAt {
		return errors.New("injected stage-2 map failure")
	}
	return v.mapErr
}
func (v *ramTestVM) UnmapStage2(gpa, length uint64) error {
	v.unmaps = append(v.unmaps, ramTestMap{gpa: gpa, length: length})
	return nil
}
func (*ramTestVM) CreateVCPU(int) (VCPU, error) { return nil, nil }
func (*ramTestVM) Close() error                 { return nil }

type ramTestHost struct {
	next         uintptr
	commits      []ramTestMap
	decommits    []ramTestMap
	reservations map[uintptr]uint64
	decommitErr  error
	zeroErr      error
	copies       []ramTestCopy
}

type ramTestCopy struct {
	dst, src uintptr
	length   uint64
}

func newRAMTestHost() *ramTestHost {
	return &ramTestHost{next: 0x100000000, reservations: make(map[uintptr]uint64)}
}

func (h *ramTestHost) Reserve(length uint64) (uintptr, error) {
	address := h.next
	h.next += uintptr(length)
	h.reservations[address] = length
	return address, nil
}
func (h *ramTestHost) Commit(address uintptr, length uint64) error {
	h.commits = append(h.commits, ramTestMap{address: address, length: length})
	return nil
}
func (h *ramTestHost) Decommit(address uintptr, length uint64) error {
	h.decommits = append(h.decommits, ramTestMap{address: address, length: length})
	return h.decommitErr
}
func (h *ramTestHost) Zero(uintptr, uint64) error { return h.zeroErr }
func (h *ramTestHost) Copy(dst, src uintptr, length uint64) error {
	h.copies = append(h.copies, ramTestCopy{dst: dst, src: src, length: length})
	return nil
}
func (h *ramTestHost) Release(address uintptr, _ uint64) error {
	delete(h.reservations, address)
	return nil
}

type ramTestRevoker struct {
	mu      sync.Mutex
	calls   []map[pgalloc.FrameID][]MappingRef
	started chan struct{}
	release chan struct{}
}

type ramTestRelocator struct {
	calls []map[pgalloc.FrameID]FrameRelocation
	err   error
}

func (r *ramTestRelocator) RelocateFrames(_ context.Context, relocations map[pgalloc.FrameID]FrameRelocation) error {
	copyOfRelocations := make(map[pgalloc.FrameID]FrameRelocation, len(relocations))
	for id, relocation := range relocations {
		copyOfRelocations[id] = relocation
	}
	r.calls = append(r.calls, copyOfRelocations)
	return r.err
}

func (r *ramTestRevoker) RevokeFrames(_ context.Context, refs map[pgalloc.FrameID][]MappingRef) error {
	r.mu.Lock()
	r.calls = append(r.calls, refs)
	started := r.started
	release := r.release
	r.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return nil
}

func (r *ramTestRevoker) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newRAMTestBacking(t *testing.T, ipaSize uint64) (*GuestRAM, pgalloc.Backing, *ramTestVM, *ramTestHost, *ramTestRevoker) {
	t.Helper()
	vm := &ramTestVM{}
	host := newRAMTestHost()
	revoker := &ramTestRevoker{}
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: host, Revoker: revoker, IPAStart: 0x100000, IPASize: ipaSize})
	if err != nil {
		t.Fatalf("NewGuestRAM: %v", err)
	}
	backing, err := ram.NewBacking()
	if err != nil {
		t.Fatalf("NewBacking: %v", err)
	}
	if mappings, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil || len(mappings) != 1 {
		t.Fatalf("Grow = (%v, %v), want one mapping", mappings, err)
	}
	return ram, backing, vm, host, revoker
}

func TestGuestRAMQueryCommitment(t *testing.T) {
	_, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	fr := memmap.FileRange{Start: 0, End: 4 * GuestFrameSize}
	committed := bytes.Repeat([]byte{0xff}, 4)
	if err := backing.QueryCommitment(fr, committed); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 0, 0}; !bytes.Equal(committed, want) {
		t.Fatalf("initial commitment = %v, want %v", committed, want)
	}
	for _, offset := range []uint64{0, 2 * GuestFrameSize} {
		if _, err := backing.ResolveFrames(memmap.FileRange{Start: offset, End: offset + GuestFrameSize}, hostarch.Read); err != nil {
			t.Fatal(err)
		}
	}
	if err := backing.QueryCommitment(fr, committed); err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 0, 1, 0}; !bytes.Equal(committed, want) {
		t.Fatalf("commitment = %v, want %v", committed, want)
	}
	if err := backing.Decommit(memmap.FileRange{Start: 0, End: GuestFrameSize}); err != nil {
		t.Fatal(err)
	}
	committed = bytes.Repeat([]byte{0xff}, 4)
	if err := backing.QueryCommitment(fr, committed); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 1, 0}; !bytes.Equal(committed, want) {
		t.Fatalf("commitment after decommit = %v, want %v", committed, want)
	}
	if err := backing.QueryCommitment(fr, committed[:3]); err == nil {
		t.Fatal("QueryCommitment accepted a short bitmap")
	}
}

func TestGuestRAMMemoryFileUpdateUsage(t *testing.T) {
	ram, err := NewGuestRAM(GuestRAMConfig{
		VM: &ramTestVM{}, Host: newRAMTestHost(), IPAStart: 0x100000, IPASize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		BackingProvider:         ram,
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	fr, err := mf.Allocate(4*GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateUncommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.DecRef(fr)

	for _, offset := range []uint64{fr.Start, fr.Start + 2*GuestFrameSize} {
		page := memmap.FileRange{Start: offset, End: offset + GuestFrameSize}
		if _, err := mf.ResolveBackingFrames(page, hostarch.Read); err != nil {
			t.Fatal(err)
		}
	}
	// /proc/meminfo calls UpdateUsage before reading global accounting. A
	// relocatable GuestRAM MemoryFile has no stable chunk mapping for mincore.
	if err := mf.UpdateUsage(map[uint32]struct{}{0: {}}); err != nil {
		t.Fatalf("UpdateUsage with sparse GuestRAM commitment: %v", err)
	}
	if got, err := mf.TotalUsage(); err != nil || got != 2*GuestFrameSize {
		t.Fatalf("TotalUsage = (%d, %v), want (%d, nil)", got, err, 2*GuestFrameSize)
	}

	page := memmap.FileRange{Start: fr.Start + GuestFrameSize, End: fr.Start + 2*GuestFrameSize}
	if _, err := mf.ResolveBackingFrames(page, hostarch.Read); err != nil {
		t.Fatal(err)
	}
	if err := mf.UpdateUsage(map[uint32]struct{}{0: {}}); err != nil {
		t.Fatalf("UpdateUsage after additional GuestRAM commitment: %v", err)
	}
}

func TestGuestRAMFourFramesShareSlab(t *testing.T) {
	ram, backing, vm, _, _ := newRAMTestBacking(t, 1<<20)
	fr := memmap.FileRange{Start: 0, End: HostSlabSize}
	frames, err := backing.ResolveFrames(fr, hostarch.AnyAccess)
	if err != nil {
		t.Fatalf("ResolveFrames: %v", err)
	}
	if len(frames) != int(FramesPerSlab) {
		t.Fatalf("got %d frames, want %d", len(frames), FramesPerSlab)
	}
	if len(vm.maps) != 1 || vm.maps[0].length != HostSlabSize {
		t.Fatalf("stage-2 maps = %+v, want one 16 KiB map", vm.maps)
	}
	for i, frame := range frames {
		if frame.GPA != vm.maps[0].gpa+uint64(i)*GuestFrameSize {
			t.Errorf("frame %d GPA = %#x, want slab GPA + offset", i, frame.GPA)
		}
		if i != 0 && frame.ID == frames[i-1].ID {
			t.Errorf("frames %d and %d share ID %d", i-1, i, frame.ID)
		}
	}
	metrics := ram.Metrics()
	if metrics.LogicalBytes != HostSlabSize || metrics.HostResidentBytes != HostSlabSize {
		t.Fatalf("metrics = %+v, want logical and resident 16 KiB", metrics)
	}
}

func TestGuestRAMCoalescesAdjacentStage2Releases(t *testing.T) {
	ram, backing, vm, host, _ := newRAMTestBacking(t, 1<<20)
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.ReadWrite); err != nil {
		t.Fatal(err)
	}
	if err := backing.Decommit(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}); err != nil {
		t.Fatal(err)
	}
	if len(vm.unmaps) != 1 || vm.unmaps[0].length != 2*HostSlabSize {
		t.Fatalf("stage-2 unmaps = %+v, want one contiguous %d-byte unmap", vm.unmaps, 2*HostSlabSize)
	}
	if len(host.decommits) != 2 {
		t.Fatalf("host decommits = %d, want one per reservation", len(host.decommits))
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != 0 || metrics.HostResidentBytes != 0 {
		t.Fatalf("metrics after adjacent release = %+v", metrics)
	}
}

func TestGuestRAMPacksFramesAcrossBackingLifetimes(t *testing.T) {
	vm := &ramTestVM{}
	host := newRAMTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: host, IPAStart: 0x100000, IPASize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	newBacking := func() pgalloc.Backing {
		backing, err := ram.NewBacking()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
			t.Fatal(err)
		}
		return backing
	}
	first, second := newBacking(), newBacking()
	firstFrames, err := first.ResolveFrames(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	secondFrames, err := second.ResolveFrames(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if firstFrames[0].GPA/HostSlabSize != secondFrames[0].GPA/HostSlabSize {
		t.Fatalf("separate backings used separate slabs: %#x and %#x", firstFrames[0].GPA, secondFrames[0].GPA)
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != 2*GuestFrameSize || metrics.HostResidentBytes != HostSlabSize {
		t.Fatalf("packed metrics = %+v", metrics)
	}
	if err := first.Destroy(); err != nil {
		t.Fatal(err)
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != GuestFrameSize || metrics.HostResidentBytes != HostSlabSize {
		t.Fatalf("metrics after first backing destroy = %+v", metrics)
	}
	if err := second.Destroy(); err != nil {
		t.Fatal(err)
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != 0 || metrics.HostResidentBytes != 0 {
		t.Fatalf("metrics after both backing destroys = %+v", metrics)
	}
}

func TestGuestRAMIndexesOnlyPartialSlabs(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	frames, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	ram.mu.Lock()
	if got := len(ram.partialSlabs); got != 0 {
		ram.mu.Unlock()
		t.Fatalf("partial slab count after filling two slabs = %d, want 0", got)
	}
	ram.mu.Unlock()

	if err := backing.Decommit(memmap.FileRange{Start: 0, End: GuestFrameSize}); err != nil {
		t.Fatal(err)
	}
	ram.mu.Lock()
	if got := len(ram.partialSlabs); got != 1 {
		ram.mu.Unlock()
		t.Fatalf("partial slab count after creating one hole = %d, want 1", got)
	}
	ram.mu.Unlock()

	second, err := ram.NewBacking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		t.Fatal(err)
	}
	reused, err := second.ResolveFrames(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if reused[0].GPA != frames[0].GPA {
		t.Fatalf("replacement frame GPA = %#x, want dense reuse of %#x", reused[0].GPA, frames[0].GPA)
	}
	ram.mu.Lock()
	if got := len(ram.partialSlabs); got != 0 {
		ram.mu.Unlock()
		t.Fatalf("partial slab count after refilling hole = %d, want 0", got)
	}
	ram.mu.Unlock()
}

func TestGuestRAMCompactionRelocatesStableFramesAndReleasesSlab(t *testing.T) {
	ram, backing, vm, host, _ := newRAMTestBacking(t, 1<<20)
	relocator := &ramTestRelocator{}
	ram.SetRelocator(relocator)
	all, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 3 * HostSlabSize}, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	// Leave three slabs with occupancies 3, 3, and 2. The last slab's two
	// live frames fit exactly into the holes in the first two slabs.
	for _, index := range []uint64{3, 7, 10, 11} {
		if err := backing.Decommit(memmap.FileRange{Start: index * GuestFrameSize, End: (index + 1) * GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	ref := MappingRef{AddressSpaceID: 7, VirtualAddress: 0x4000}
	if err := ram.AddMapping(all[8].ID, ref); err != nil {
		t.Fatal(err)
	}
	oldGPA := all[8].GPA
	result, err := ram.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if result.FramesMoved != 2 || result.SlabsReleased != 1 || result.BytesCopied != 2*GuestFrameSize {
		t.Fatalf("Compact result = %+v, want 2 frames and 1 slab", result)
	}
	if len(host.copies) != 2 {
		t.Fatalf("host copies = %d, want 2", len(host.copies))
	}
	if len(relocator.calls) != 1 {
		t.Fatalf("relocator calls = %d, want 1", len(relocator.calls))
	}
	relocation, ok := relocator.calls[0][all[8].ID]
	if !ok || relocation.OldGPA != oldGPA || len(relocation.References) != 1 || relocation.References[0] != ref {
		t.Fatalf("frame relocation = %+v, want old GPA %#x and ref %+v", relocation, oldGPA, ref)
	}
	rebound, err := backing.ResolveFrames(memmap.FileRange{Start: 8 * GuestFrameSize, End: 9 * GuestFrameSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if rebound[0].ID != all[8].ID || rebound[0].GPA != relocation.NewGPA || rebound[0].GPA == oldGPA {
		t.Fatalf("rebound frame = %+v, relocation = %+v", rebound[0], relocation)
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != 8*GuestFrameSize || metrics.HostResidentBytes != 2*HostSlabSize {
		t.Fatalf("post-compaction metrics = %+v", metrics)
	}
	if len(vm.unmaps) != 1 || vm.unmaps[0].gpa != oldGPA {
		t.Fatalf("stage-2 unmaps = %+v, want source slab at %#x", vm.unmaps, oldGPA)
	}
}

func TestGuestRAMCompactionRelocationFailurePreservesLayout(t *testing.T) {
	ram, backing, vm, _, _ := newRAMTestBacking(t, 1<<20)
	relocator := &ramTestRelocator{err: errors.New("injected relocation failure")}
	ram.SetRelocator(relocator)
	all, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []uint64{2, 3, 6, 7} {
		if err := backing.Decommit(memmap.FileRange{Start: index * GuestFrameSize, End: (index + 1) * GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ram.Compact(context.Background()); err == nil {
		t.Fatal("Compact succeeded despite relocator failure")
	}
	rebound, err := backing.ResolveFrames(memmap.FileRange{Start: 4 * GuestFrameSize, End: 5 * GuestFrameSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if rebound[0].ID != all[4].ID || rebound[0].GPA != all[4].GPA {
		t.Fatalf("frame moved after failed relocation: before=%+v after=%+v", all[4], rebound[0])
	}
	if len(vm.unmaps) != 0 {
		t.Fatalf("failed compaction unmapped slabs: %+v", vm.unmaps)
	}
}

func TestGuestRAMCompactionPreservesFrameData(t *testing.T) {
	host := newStagingTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: host, IPAStart: 0x100000, IPASize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	backing, err := ram.NewBacking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		t.Fatal(err)
	}
	frames, lease, err := backing.AcquireMapping(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames {
		page := unsafe.Slice((*byte)(unsafe.Pointer(frame.HostAddress)), int(GuestFrameSize))
		page[0], page[len(page)-1] = byte(i+1), byte(0x80+i)
	}
	lease.Release()
	for _, index := range []uint64{2, 3, 6, 7} {
		if err := backing.Decommit(memmap.FileRange{Start: index * GuestFrameSize, End: (index + 1) * GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ram.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, index := range []uint64{0, 1, 4, 5} {
		mapped, lease, err := backing.AcquireMapping(memmap.FileRange{Start: index * GuestFrameSize, End: (index + 1) * GuestFrameSize}, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		page := unsafe.Slice((*byte)(unsafe.Pointer(mapped[0].HostAddress)), int(GuestFrameSize))
		if page[0] != byte(index+1) || page[len(page)-1] != byte(0x80+index) {
			lease.Release()
			t.Fatalf("frame %d data changed: first=%#x last=%#x", index, page[0], page[len(page)-1])
		}
		lease.Release()
	}
}

func TestGuestRAMCompactionWaitsForHostMappingLease(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	for _, index := range []uint64{2, 3, 6, 7} {
		if err := backing.Decommit(memmap.FileRange{Start: index * GuestFrameSize, End: (index + 1) * GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	_, lease, err := backing.AcquireMapping(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ram.Compact(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		lease.Release()
		t.Fatalf("Compact completed while mapping lease was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Compact did not continue after mapping lease release")
	}
}

func TestGuestRAMDecommitWaitsForHostMappingLease(t *testing.T) {
	_, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	_, lease, err := backing.AcquireMapping(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- backing.Decommit(memmap.FileRange{Start: 0, End: GuestFrameSize})
	}()
	select {
	case err := <-done:
		lease.Release()
		t.Fatalf("Decommit completed while mapping lease was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Decommit did not continue after mapping lease release")
	}
}

func TestGuestRAMBatchMappingUsesSingleLease(t *testing.T) {
	_, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	batchBacking, ok := backing.(interface {
		AcquireMappings([]pgalloc.BackingMappingRequest) ([][]pgalloc.BackingFrame, memmap.InternalMappingLease, error)
	})
	if !ok {
		t.Fatal("GuestRAM backing does not implement batch mapping")
	}
	mappings, lease, err := batchBacking.AcquireMappings([]pgalloc.BackingMappingRequest{
		{Range: memmap.FileRange{Start: 0, End: GuestFrameSize}, Access: hostarch.Read},
		{Range: memmap.FileRange{Start: GuestFrameSize, End: 2 * GuestFrameSize}, Access: hostarch.Write},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		lease.Release()
		t.Fatalf("batch mapping count = %d, want 2", len(mappings))
	}
	if len(mappings[0]) != 1 || len(mappings[1]) != 1 {
		lease.Release()
		t.Fatalf("batch mapping lengths = [%d, %d], want [1, 1]", len(mappings[0]), len(mappings[1]))
	}
	done := make(chan error, 1)
	go func() {
		done <- backing.Decommit(memmap.FileRange{Start: 0, End: GuestFrameSize})
	}()
	select {
	case err := <-done:
		lease.Release()
		t.Fatalf("Decommit completed while batch lease was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Decommit did not continue after batch lease release")
	}
}

func TestGuestRAMDestroyWaitsForHostMappingLease(t *testing.T) {
	_, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	_, lease, err := backing.AcquireMapping(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- backing.Destroy()
	}()
	select {
	case err := <-done:
		lease.Release()
		t.Fatalf("Destroy completed while mapping lease was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Destroy did not continue after mapping lease release")
	}
}

func TestGuestRAMMemoryFileAsyncSaveRestoreUsesLeasedMappings(t *testing.T) {
	host := newStagingTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: host, IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	newMemoryFile := func() *pgalloc.MemoryFile {
		mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
			BackingProvider:         ram,
			DelayedEviction:         pgalloc.DelayedEvictionDisabled,
			DisableMemoryAccounting: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return mf
	}
	source := newMemoryFile()
	fr, err := source.Allocate(8*GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, fr.Length())
	for i := range want {
		want[i] = byte((i*131 + 17) & 0xff)
	}
	dst, lease, err := memmap.MapInternalWithLease(source, fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(dst, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want))); err != nil || n != uint64(len(want)) {
		lease.Release()
		t.Fatalf("populating source: n=%d err=%v", n, err)
	}
	lease.Release()

	var metadata, pages bytes.Buffer
	saveDone := make(chan error, 1)
	saver, err := pgalloc.StartAsyncPagesFileSave(stateio.NewIOWriter(&pages, 4*GuestFrameSize, 4, 2), func(err error) {
		saveDone <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveTo(gcontext.Background(), &metadata, &pgalloc.SaveOpts{PagesFile: saver}); err != nil {
		t.Fatal(err)
	}
	saver.MemoryFilesDone()
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}

	target := newMemoryFile()
	loader, err := pgalloc.StartAsyncPagesFileLoad(stateio.NewIOReader(bytes.NewReader(pages.Bytes()), 4*GuestFrameSize, 4, 2), nil, nil, pgalloc.AsyncPagesFileLoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.LoadFrom(gcontext.Background(), bytes.NewReader(metadata.Bytes()), &pgalloc.LoadOpts{PagesFile: loader}); err != nil {
		loader.Cancel()
		t.Fatal(err)
	}
	loader.MemoryFilesDone()
	if err := loader.Wait(); err != nil {
		t.Fatal(err)
	}
	gotBlocks, gotLease, err := memmap.MapInternalWithLease(target, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), gotBlocks); err != nil || n != uint64(len(got)) {
		gotLease.Release()
		t.Fatalf("reading restored data: n=%d err=%v", n, err)
	}
	gotLease.Release()
	if !bytes.Equal(got, want) {
		t.Fatal("asynchronously restored GuestRAM data differs from source")
	}
	target.DecRef(fr)
	target.Destroy()
	source.DecRef(fr)
	source.Destroy()
}

func TestGuestRAMMemoryFileSavePreservesSparseAndResidentUnknownPages(t *testing.T) {
	host := newStagingTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: host, IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		BackingProvider:         ram,
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fr, err := mf.Allocate(2*GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateUncommitted})
	if err != nil {
		t.Fatal(err)
	}
	// Materialize only the first zero page without updating MemoryFile's
	// commitment accounting. A live sibling may be using exactly this kind of
	// frame while another branch is checkpointed.
	resident := memmap.FileRange{Start: fr.Start, End: fr.Start + GuestFrameSize}
	_, lease, err := memmap.MapInternalWithLease(mf, resident, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	before := ram.Metrics()
	if before.LogicalBytes != GuestFrameSize || before.HostResidentBytes != HostSlabSize {
		t.Fatalf("GuestRAM before save = %+v, want one logical frame and one slab", before)
	}

	var checkpoint bytes.Buffer
	if err := mf.SaveTo(gcontext.Background(), &checkpoint, &pgalloc.SaveOpts{}); err != nil {
		t.Fatal(err)
	}
	after := ram.Metrics()
	if after.LogicalBytes != before.LogicalBytes || after.HostResidentBytes != before.HostResidentBytes {
		t.Fatalf("GuestRAM after sparse save = %+v, want unchanged %+v", after, before)
	}
	mf.DecRef(fr)
	mf.Destroy()
}

func TestGuestRAMResolveBackingFramesWaitsForRemotePage(t *testing.T) {
	newMemoryFile := func() *pgalloc.MemoryFile {
		ram, err := NewGuestRAM(GuestRAMConfig{
			VM: &ramTestVM{}, Host: newStagingTestHost(), IPAStart: 0x100000, IPASize: 1 << 24,
		})
		if err != nil {
			t.Fatal(err)
		}
		mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
			BackingProvider: ram, DelayedEviction: pgalloc.DelayedEvictionDisabled, DisableMemoryAccounting: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return mf
	}

	source := newMemoryFile()
	sourceFR, err := source.Allocate(GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xa7}, int(GuestFrameSize))
	blocks, lease, err := memmap.MapInternalWithLease(source, sourceFR, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want))); err != nil || n != GuestFrameSize {
		lease.Release()
		t.Fatalf("populating source: n=%d err=%v", n, err)
	}
	lease.Release()
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}

	target := newMemoryFile()
	targetFR, err := target.Allocate(GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateUncommitted})
	if err != nil {
		t.Fatal(err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, GuestFrameSize, 1, 1)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []pgalloc.RemoteLoadRange{{Target: targetFR}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()

	// This is the path used by VM AddressSpace.Map, including executable
	// mappings. It must not expose the target's initially empty GuestRAM frame.
	frames, err := target.ResolveBackingFrames(targetFR, hostarch.Execute)
	if err != nil {
		t.Fatal(err)
	}
	got := unsafe.Slice((*byte)(unsafe.Pointer(frames[0].HostAddress)), int(GuestFrameSize))
	if !bytes.Equal(got, want) {
		t.Fatal("ResolveBackingFrames exposed a frame before its remote contents were installed")
	}
	if err := pages.Wait(); err != nil {
		t.Fatal(err)
	}
	target.DecRef(targetFR)
	target.Destroy()
	source.DecRef(sourceFR)
	source.Destroy()
}

func TestGuestRAMReclaimsOnlyEmptySlabAndRetiresGPA(t *testing.T) {
	ram, backing, vm, _, revoker := newRAMTestBacking(t, 1<<20)
	frames, err := backing.ResolveFrames(memmap.FileRange{0, HostSlabSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	firstGPA := frames[0].GPA
	ref := MappingRef{AddressSpaceID: 9, VirtualAddress: 0x4000}
	if err := ram.AddMapping(frames[0].ID, ref); err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < FramesPerSlab-1; i++ {
		start := i * GuestFrameSize
		if err := backing.Decommit(memmap.FileRange{start, start + GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
		if len(vm.unmaps) != 0 {
			t.Fatalf("slab unmapped with live neighbor after frame %d", i)
		}
	}
	last := (FramesPerSlab - 1) * GuestFrameSize
	if err := backing.Decommit(memmap.FileRange{last, last + GuestFrameSize}); err != nil {
		t.Fatal(err)
	}
	if len(vm.unmaps) != 1 || revoker.callCount() != 1 {
		t.Fatalf("unmaps=%d revocations=%d, want one each", len(vm.unmaps), revoker.callCount())
	}
	if got := ram.Metrics().HostResidentBytes; got != 0 {
		t.Fatalf("resident bytes after reclaim = %d, want 0", got)
	}
	newFrame, err := backing.ResolveFrames(memmap.FileRange{0, GuestFrameSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if newFrame[0].GPA <= firstGPA || newFrame[0].ID == frames[0].ID {
		t.Fatalf("reused retired execution identity: old=%+v new=%+v", frames[0], newFrame[0])
	}
}

func TestGuestRAMPartialDecommitRevokesBeforeFrameReuse(t *testing.T) {
	ram, backing, vm, _, revoker := newRAMTestBacking(t, 1<<20)
	frames, err := backing.ResolveFrames(memmap.FileRange{0, HostSlabSize}, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	ref := MappingRef{AddressSpaceID: 17, VirtualAddress: 0x8000}
	if err := ram.AddMapping(frames[0].ID, ref); err != nil {
		t.Fatal(err)
	}
	revoker.started = make(chan struct{})
	revoker.release = make(chan struct{})
	decommitDone := make(chan error, 1)
	go func() {
		decommitDone <- backing.Decommit(memmap.FileRange{0, GuestFrameSize})
	}()
	<-revoker.started

	resolveDone := make(chan error, 1)
	go func() {
		_, err := backing.ResolveFrames(memmap.FileRange{0, GuestFrameSize}, hostarch.Write)
		resolveDone <- err
	}()
	select {
	case err := <-resolveDone:
		t.Fatalf("ResolveFrames completed before revocation acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if len(vm.unmaps) != 0 {
		t.Fatalf("partial decommit unmapped live 16K slab: %+v", vm.unmaps)
	}
	close(revoker.release)
	if err := <-decommitDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resolveDone; err != nil {
		t.Fatal(err)
	}
	if revoker.callCount() != 1 {
		t.Fatalf("revocation calls = %d, want 1", revoker.callCount())
	}
}

func TestGuestRAMEpochRollover(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 4*HostSlabSize)
	var replacement *ramTestVM
	ram.SetRollover(func(ram *GuestRAM) error {
		replacement = &ramTestVM{}
		return ram.RebindStage2(replacement)
	})
	for i := uint64(0); i < 2; i++ {
		start := i * HostSlabSize
		if _, err := backing.ResolveFrames(memmap.FileRange{start, start + GuestFrameSize}, hostarch.Read); err != nil {
			t.Fatal(err)
		}
		if err := backing.Decommit(memmap.FileRange{start, start + GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{2 * HostSlabSize, 2*HostSlabSize + GuestFrameSize}, hostarch.Read); err != nil {
		t.Fatalf("ResolveFrames triggering rollover: %v", err)
	}
	// The rollover runs in the background, outside the caller's locks.
	waitForGuestRAMEpoch(t, ram, 2)
	ram.mu.Lock()
	defer ram.mu.Unlock()
	if replacement == nil {
		t.Fatalf("rollover did not rebind stage 2: metrics=%+v", ram.Metrics())
	}
}

func waitForGuestRAMEpoch(t *testing.T, ram *GuestRAM, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ram.Metrics().Epoch != want {
		if time.Now().After(deadline) {
			t.Fatalf("GuestRAM epoch did not reach %d: %+v", want, ram.Metrics())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestGuestRAMRolloverDoesNotRunUnderCallerLocks checks that the allocation
// that crosses the rollover threshold does not run the rollover itself. The
// caller may hold GuestRAM leases and staging or address-space locks that the
// rollover's epoch rebuild waits behind.
func TestGuestRAMRolloverDoesNotRunUnderCallerLocks(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 4*HostSlabSize)
	release := make(chan struct{})
	started := make(chan struct{})
	ram.SetRollover(func(ram *GuestRAM) error {
		close(started)
		<-release
		return ram.RebindStage2(&ramTestVM{})
	})
	for i := uint64(0); i < 2; i++ {
		start := i * HostSlabSize
		if _, err := backing.ResolveFrames(memmap.FileRange{start, start + GuestFrameSize}, hostarch.Read); err != nil {
			t.Fatal(err)
		}
		if err := backing.Decommit(memmap.FileRange{start, start + GuestFrameSize}); err != nil {
			t.Fatal(err)
		}
	}
	frames, lease, err := backing.AcquireMapping(memmap.FileRange{2 * HostSlabSize, 2*HostSlabSize + GuestFrameSize}, hostarch.Read)
	if err != nil {
		t.Fatalf("allocation crossing the rollover threshold: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("rollover did not start")
	}
	// The rollover waits for this test, which still holds its lease.
	lease.Release()
	close(release)
	waitForGuestRAMEpoch(t, ram, 2)
}

func TestGuestRAMEpochRebindRollsBackBeforePublishingGPAs(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	var original []pgalloc.BackingFrame
	for i := uint64(0); i < FramesPerSlab+1; i++ {
		start := i * GuestFrameSize
		frames, err := backing.ResolveFrames(memmap.FileRange{start, start + GuestFrameSize}, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		original = append(original, frames[0])
	}
	replacement := &ramTestVM{failMapAt: 2}
	if err := ram.RebindStage2(replacement); err == nil {
		t.Fatal("RebindStage2 unexpectedly succeeded")
	}
	for i := uint64(0); i < FramesPerSlab+1; i++ {
		start := i * GuestFrameSize
		frames, err := backing.ResolveFrames(memmap.FileRange{start, start + GuestFrameSize}, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		if frames[0].GPA != original[i].GPA {
			t.Fatalf("frame %d GPA changed after failed rebuild: got %#x, want %#x", i, frames[0].GPA, original[i].GPA)
		}
	}
	if got := ram.Metrics().Epoch; got != 1 {
		t.Fatalf("epoch after failed rebuild = %d, want 1", got)
	}
}

func TestGuestRAMRestoresStage2WhenHostDecommitFails(t *testing.T) {
	ram, backing, vm, host, _ := newRAMTestBacking(t, 1<<20)
	if _, err := backing.ResolveFrames(memmap.FileRange{0, GuestFrameSize}, hostarch.Read); err != nil {
		t.Fatal(err)
	}
	host.decommitErr = errors.New("injected decommit failure")
	if err := backing.Decommit(memmap.FileRange{0, GuestFrameSize}); err == nil {
		t.Fatal("Decommit unexpectedly succeeded")
	}
	if len(vm.unmaps) != 1 || len(vm.maps) != 2 {
		t.Fatalf("unmaps=%d maps=%d, want stage-2 unmap followed by rollback map", len(vm.unmaps), len(vm.maps))
	}
	if got := ram.Metrics().HostResidentBytes; got != HostSlabSize {
		t.Fatalf("resident bytes after rollback = %d, want %d", got, HostSlabSize)
	}
	host.decommitErr = nil
	if err := backing.Decommit(memmap.FileRange{0, GuestFrameSize}); err != nil {
		t.Fatalf("Decommit after clearing failure: %v", err)
	}
}

func TestGuestRAMRestoresCoalescedStage2RangeWhenHostDecommitFails(t *testing.T) {
	ram, backing, vm, host, _ := newRAMTestBacking(t, 1<<20)
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}, hostarch.Read); err != nil {
		t.Fatal(err)
	}
	host.decommitErr = errors.New("injected decommit failure")
	if err := backing.Decommit(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}); err == nil {
		t.Fatal("Decommit unexpectedly succeeded")
	}
	if len(vm.unmaps) != 1 || vm.unmaps[0].length != 2*HostSlabSize {
		t.Fatalf("stage-2 unmaps = %+v, want one coalesced range", vm.unmaps)
	}
	if len(vm.maps) != 4 {
		t.Fatalf("stage-2 maps = %d, want two initial maps and two rollback maps", len(vm.maps))
	}
	if got := ram.Metrics().HostResidentBytes; got != 2*HostSlabSize {
		t.Fatalf("resident bytes after coalesced rollback = %d, want %d", got, 2*HostSlabSize)
	}
	host.decommitErr = nil
	if err := backing.Decommit(memmap.FileRange{Start: 0, End: 2 * HostSlabSize}); err != nil {
		t.Fatalf("Decommit after clearing failure: %v", err)
	}
}

func TestGuestRAMDecommitRollsBackLogicalStateWhenZeroFails(t *testing.T) {
	ram, backing, _, host, _ := newRAMTestBacking(t, 1<<20)
	if _, err := backing.ResolveFrames(memmap.FileRange{0, GuestFrameSize}, hostarch.Read); err != nil {
		t.Fatal(err)
	}
	host.zeroErr = errors.New("injected zero failure")
	if err := backing.Decommit(memmap.FileRange{0, GuestFrameSize}); err == nil {
		t.Fatal("Decommit unexpectedly succeeded")
	}
	if got := ram.Metrics().LogicalBytes; got != GuestFrameSize {
		t.Fatalf("logical bytes after failed zero = %d, want %d", got, GuestFrameSize)
	}
}

func TestGuestRAMLogicalLimit(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<30)
	if err := ram.SetLogicalLimit(GuestFrameSize); err != nil {
		t.Fatal(err)
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write); err != nil {
		t.Fatalf("first frame within limit: %v", err)
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: GuestFrameSize, End: 2 * GuestFrameSize}, hostarch.Write); err == nil {
		t.Fatal("allocation beyond logical limit succeeded")
	}
	if err := ram.SetLogicalLimit(GuestFrameSize - 1); err == nil {
		t.Fatal("limit below current use succeeded")
	}
	metrics := ram.Metrics()
	if metrics.LogicalBytes != GuestFrameSize || metrics.LogicalLimitBytes != GuestFrameSize {
		t.Fatalf("metrics=%+v", metrics)
	}
	if err := backing.Decommit(memmap.FileRange{Start: 0, End: GuestFrameSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: GuestFrameSize, End: 2 * GuestFrameSize}, hostarch.Write); err != nil {
		t.Fatalf("allocation after reclaim: %v", err)
	}
}

// checkPartialSlabIndex compares GuestRAM's partial-slab accounting with a
// recount, and its choice of allocation slab with a full scan.
func checkPartialSlabIndex(t *testing.T, ram *GuestRAM) {
	t.Helper()
	ram.mu.Lock()
	defer ram.mu.Unlock()
	var free uint64
	var best *guestSlab
	for _, slab := range ram.slabs {
		occupancy := slabOccupancy(slab)
		partial := !slab.releasing && occupancy < FramesPerSlab
		if _, ok := ram.partialSlabs[slab.key]; ok != partial || slab.inPartial != partial {
			t.Fatalf("slab %d partial membership = %t/%t, want %t", slab.key, ok, slab.inPartial, partial)
		}
		if !partial {
			continue
		}
		if slab.partialOccupancy != occupancy {
			t.Fatalf("slab %d recorded occupancy %d, want %d", slab.key, slab.partialOccupancy, occupancy)
		}
		free += FramesPerSlab - occupancy
		if betterAllocationSlab(slab, best) {
			best = slab
		}
	}
	if ram.partialFree != free {
		t.Fatalf("partialFree = %d, want %d", ram.partialFree, free)
	}
	ram.allocationSlab = nil
	if got := ram.selectAllocationSlabLocked(); got != best {
		t.Fatalf("selected slab %v, want %v", got, best)
	}
}

// TestGuestRAMPartialSlabIndex checks the partial-slab accounting across
// random allocation and decommit of individual frames.
func TestGuestRAMPartialSlabIndex(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<30)
	const frames = 256
	rng := rand.New(rand.NewSource(1))
	resident := make(map[uint64]bool)
	for i := 0; i < 4000; i++ {
		offset := uint64(rng.Intn(frames)) * GuestFrameSize
		fr := memmap.FileRange{Start: offset, End: offset + GuestFrameSize}
		if resident[offset] {
			if err := backing.Decommit(fr); err != nil {
				t.Fatal(err)
			}
			delete(resident, offset)
		} else {
			if _, err := backing.ResolveFrames(fr, hostarch.Write); err != nil {
				t.Fatal(err)
			}
			resident[offset] = true
		}
		if i%50 == 0 {
			checkPartialSlabIndex(t, ram)
		}
	}
	checkPartialSlabIndex(t, ram)
}

// BenchmarkGuestRAMFragmentedAllocation allocates frames when most slabs are
// partially used, as after memory churn. Selecting a slab used to scan every
// partial slab under GuestRAM.mu.
func BenchmarkGuestRAMFragmentedAllocation(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: newRAMTestHost(), Revoker: &ramTestRevoker{}, IPAStart: 0x100000, IPASize: 1 << 34})
		if err != nil {
			b.Fatal(err)
		}
		backing, err := ram.NewBacking()
		if err != nil {
			b.Fatal(err)
		}
		const slabs = 8192
		if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
			b.Fatal(err)
		}
		// Keep one frame of every slab resident.
		for offset := uint64(0); offset < slabs*HostSlabSize; offset += GuestFrameSize {
			if _, err := backing.ResolveFrames(memmap.FileRange{Start: offset, End: offset + GuestFrameSize}, hostarch.Write); err != nil {
				b.Fatal(err)
			}
		}
		for offset := uint64(0); offset < slabs*HostSlabSize; offset += GuestFrameSize {
			if offset%HostSlabSize != 0 {
				if err := backing.Decommit(memmap.FileRange{Start: offset, End: offset + GuestFrameSize}); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StartTimer()
		for offset := uint64(slabs * HostSlabSize); offset < 2*slabs*HostSlabSize; offset += GuestFrameSize {
			if _, err := backing.ResolveFrames(memmap.FileRange{Start: offset, End: offset + GuestFrameSize}, hostarch.Write); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// TestGuestRAMDecommitDoesNotBlockLeases checks that a Decommit waiting for a
// lease that could cover its frames delays no other lease, not even one
// taken by the holder of that lease.
func TestGuestRAMDecommitDoesNotBlockLeases(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	held := memmap.FileRange{Start: 0, End: GuestFrameSize}
	freed := memmap.FileRange{Start: GuestFrameSize, End: 2 * GuestFrameSize}
	if _, err := backing.ResolveFrames(freed, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	_, lease, err := backing.AcquireMapping(held, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	decommitted := make(chan error, 1)
	go func() { decommitted <- backing.Decommit(freed) }()
	deadline := time.Now().Add(5 * time.Second)
	for ram.drainsWaiting.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Decommit never waited for the outstanding lease")
		}
		time.Sleep(time.Millisecond)
	}
	// Another lease, as a holder of the first might take.
	_, other, err := backing.AcquireMapping(held, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	other.Release()
	select {
	case err := <-decommitted:
		t.Fatalf("Decommit finished (%v) while a lease that could cover its frame was held", err)
	default:
	}
	lease.Release()
	select {
	case err := <-decommitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Decommit did not finish after the lease was released")
	}
}

// TestGuestRAMTryAcquireMappingDoesNotWaitForDestroy checks that a caller
// holding a lease can see a pending Destroy instead of queueing behind it.
func TestGuestRAMTryAcquireMappingDoesNotWaitForDestroy(t *testing.T) {
	ram, backing, _, _, _ := newRAMTestBacking(t, 1<<20)
	tryMapper := backing.(interface {
		TryAcquireMapping(memmap.FileRange, hostarch.AccessType) ([]pgalloc.BackingFrame, memmap.InternalMappingLease, bool, error)
	})
	held := memmap.FileRange{Start: 0, End: GuestFrameSize}
	_, lease, err := backing.AcquireMapping(held, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	destroyed := startPendingDestroy(t, ram)
	if _, _, acquired, err := tryMapper.TryAcquireMapping(held, hostarch.Write); err != nil || acquired {
		t.Fatalf("TryAcquireMapping with a Destroy pending = (%t, %v), want not acquired", acquired, err)
	}
	lease.Release()
	waitForError(t, destroyed, "Destroy")
	if _, other, acquired, err := tryMapper.TryAcquireMapping(held, hostarch.Write); err != nil || !acquired {
		t.Fatalf("TryAcquireMapping after Destroy = (%t, %v), want acquired", acquired, err)
	} else {
		other.Release()
	}
}

// gatedReaderAt holds each read until the test grants a permit.
type gatedReaderAt struct {
	io.ReaderAt
	arrived chan struct{}
	permits chan struct{}
}

func (g *gatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	g.arrived <- struct{}{}
	<-g.permits
	return g.ReaderAt.ReadAt(p, off)
}

// gatedWriter holds each write until the test grants a permit.
type gatedWriter struct {
	io.Writer
	arrived chan struct{}
	permits chan struct{}
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	g.arrived <- struct{}{}
	<-g.permits
	return g.Writer.Write(p)
}

func waitForSignal(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForError(t *testing.T, c <-chan error, what string) {
	t.Helper()
	select {
	case err := <-c:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func newGuestRAMTestMemoryFile(t *testing.T, ram *GuestRAM) *pgalloc.MemoryFile {
	t.Helper()
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		BackingProvider:         ram,
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

// populateGuestRAMTestMemoryFile allocates frames of mf and fills each with a
// distinct pattern, which it returns.
func populateGuestRAMTestMemoryFile(t *testing.T, mf *pgalloc.MemoryFile, frames uint64) (memmap.FileRange, []byte) {
	t.Helper()
	fr, err := mf.Allocate(frames*GuestFrameSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, fr.Length())
	for i := range want {
		want[i] = byte((i*131 + 17 + i/int(GuestFrameSize)*7) & 0xff)
	}
	dst, lease, err := memmap.MapInternalWithLease(mf, fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if n, err := safemem.CopySeq(dst, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want))); err != nil || n != uint64(len(want)) {
		t.Fatalf("populating source: n=%d err=%v", n, err)
	}
	return fr, want
}

// startPendingDestroy starts destroying an unrelated backing, which excludes
// every lease, and returns once it is waiting for outstanding leases.
func startPendingDestroy(t *testing.T, ram *GuestRAM) <-chan error {
	t.Helper()
	backing, err := ram.NewBacking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		t.Fatal(err)
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: GuestFrameSize}, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- backing.Destroy() }()
	deadline := time.Now().Add(5 * time.Second)
	for ram.accessMu.TryRLock() {
		ram.accessMu.RUnlock() // +checklocksforce: TryRLock succeeded.
		if time.Now().After(deadline) {
			t.Fatal("Destroy never waited for outstanding leases")
		}
		time.Sleep(time.Millisecond)
	}
	return done
}

// TestGuestRAMAsyncLoadWithPendingExclusion checks that async page loading
// makes progress while an exclusive operation, such as a Destroy, waits for the
// leases of in-flight reads. The loader used to wait for a new lease behind
// such an operation, then a Decommit, so it could not complete the reads the
// operation was waiting for, hanging restores.
func TestGuestRAMAsyncLoadWithPendingExclusion(t *testing.T) {
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: newStagingTestHost(), IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	source := newGuestRAMTestMemoryFile(t, ram)
	fr, want := populateGuestRAMTestMemoryFile(t, source, 8)
	var metadata, pages bytes.Buffer
	saveDone := make(chan error, 1)
	saver, err := pgalloc.StartAsyncPagesFileSave(stateio.NewIOWriter(&pages, 4*GuestFrameSize, 4, 2), func(err error) {
		saveDone <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveTo(gcontext.Background(), &metadata, &pgalloc.SaveOpts{PagesFile: saver}); err != nil {
		t.Fatal(err)
	}
	saver.MemoryFilesDone()
	waitForError(t, saveDone, "saving pages")

	// Read one frame at a time, two reads at once.
	reader := &gatedReaderAt{
		ReaderAt: bytes.NewReader(pages.Bytes()),
		arrived:  make(chan struct{}, 64),
		permits:  make(chan struct{}),
	}
	target := newGuestRAMTestMemoryFile(t, ram)
	loader, err := pgalloc.StartAsyncPagesFileLoad(stateio.NewIOReader(reader, GuestFrameSize, 1, 2), nil, nil, pgalloc.AsyncPagesFileLoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.LoadFrom(gcontext.Background(), bytes.NewReader(metadata.Bytes()), &pgalloc.LoadOpts{PagesFile: loader}); err != nil {
		loader.Cancel()
		t.Fatal(err)
	}
	loader.MemoryFilesDone()
	for range 2 {
		waitForSignal(t, reader.arrived, "an in-flight read")
	}
	destroyed := startPendingDestroy(t, ram)
	// Complete one read, so that the loader maps the next frame while the
	// other read still holds its lease.
	reader.permits <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	close(reader.permits)
	loaded := make(chan error, 1)
	go func() { loaded <- loader.Wait() }()
	waitForError(t, loaded, "async page load")
	waitForError(t, destroyed, "Destroy")

	gotBlocks, gotLease, err := memmap.MapInternalWithLease(target, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), gotBlocks)
	gotLease.Release()
	if err != nil || n != uint64(len(got)) {
		t.Fatalf("reading restored data: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("asynchronously restored GuestRAM data differs from source")
	}
	target.DecRef(fr)
	target.Destroy()
	source.DecRef(fr)
	source.Destroy()
}

// TestGuestRAMAsyncSaveWithPendingExclusion is
// TestGuestRAMAsyncLoadWithPendingExclusion for async page saving.
func TestGuestRAMAsyncSaveWithPendingExclusion(t *testing.T) {
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: newStagingTestHost(), IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	source := newGuestRAMTestMemoryFile(t, ram)
	fr, want := populateGuestRAMTestMemoryFile(t, source, 8)

	// Write one frame at a time, with two writes queued.
	var pages bytes.Buffer
	writer := &gatedWriter{
		Writer:  &pages,
		arrived: make(chan struct{}, 64),
		permits: make(chan struct{}),
	}
	saveDone := make(chan error, 1)
	saver, err := pgalloc.StartAsyncPagesFileSave(stateio.NewIOWriter(writer, GuestFrameSize, 1, 2), func(err error) {
		saveDone <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	var metadata bytes.Buffer
	saved := make(chan error, 1)
	go func() {
		saved <- source.SaveTo(gcontext.Background(), &metadata, &pgalloc.SaveOpts{PagesFile: saver})
	}()
	waitForSignal(t, writer.arrived, "an in-flight write")
	destroyed := startPendingDestroy(t, ram)
	// Complete one write, so that the saver maps the next frame while the
	// queued write still holds its lease.
	writer.permits <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	close(writer.permits)
	waitForError(t, saved, "SaveTo")
	saver.MemoryFilesDone()
	waitForError(t, saveDone, "async page save")
	waitForError(t, destroyed, "Destroy")

	target := newGuestRAMTestMemoryFile(t, ram)
	loader, err := pgalloc.StartAsyncPagesFileLoad(stateio.NewIOReader(bytes.NewReader(pages.Bytes()), 4*GuestFrameSize, 4, 2), nil, nil, pgalloc.AsyncPagesFileLoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.LoadFrom(gcontext.Background(), bytes.NewReader(metadata.Bytes()), &pgalloc.LoadOpts{PagesFile: loader}); err != nil {
		loader.Cancel()
		t.Fatal(err)
	}
	loader.MemoryFilesDone()
	if err := loader.Wait(); err != nil {
		t.Fatal(err)
	}
	gotBlocks, gotLease, err := memmap.MapInternalWithLease(target, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), gotBlocks)
	gotLease.Release()
	if err != nil || n != uint64(len(got)) {
		t.Fatalf("reading restored data: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("GuestRAM data saved with a pending Destroy differs from source")
	}
	target.DecRef(fr)
	target.Destroy()
	source.DecRef(fr)
	source.Destroy()
}

// acquireLeaseLikeApplication stands in for an application task, such as a
// FUSE server, that needs a mapping lease while an exclusive operation may be
// waiting for leases that the caller holds. It returns an error instead of
// waiting for the caller.
func acquireLeaseLikeApplication(ram *GuestRAM) error {
	backing, err := ram.NewBacking()
	if err != nil {
		return err
	}
	if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		return err
	}
	held := memmap.FileRange{Start: 0, End: GuestFrameSize}
	if _, err := backing.ResolveFrames(held, hostarch.Write); err != nil {
		return err
	}
	other, err := ram.NewBacking()
	if err != nil {
		return err
	}
	if _, err := other.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		return err
	}
	if _, err := other.ResolveFrames(held, hostarch.Write); err != nil {
		return err
	}
	destroyed := make(chan error, 1)
	go func() { destroyed <- other.Destroy() }()
	// Wait for the Destroy to finish, or to wait for held leases.
	for deadline := time.Now().Add(5 * time.Second); ; {
		select {
		case err := <-destroyed:
			if err != nil {
				return err
			}
			_, lease, err := backing.AcquireMapping(held, hostarch.Read)
			if err == nil {
				lease.Release()
			}
			return err
		default:
		}
		if !ram.accessMu.TryRLock() {
			break
		}
		ram.accessMu.RUnlock() // +checklocksforce: TryRLock succeeded.
		if time.Now().After(deadline) {
			return errors.New("Destroy neither finished nor waited for leases")
		}
		time.Sleep(time.Millisecond)
	}
	acquired := make(chan error, 1)
	go func() {
		_, lease, err := backing.AcquireMapping(held, hostarch.Read)
		if err == nil {
			lease.Release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		return err
	case <-time.After(time.Second):
		return errors.New("a new mapping lease waited behind a Destroy that waited for a lease held by the caller")
	}
}

// TestGuestRAMPageCacheIOWithApplicationWaits checks that FUSE-style page
// cache fills and writeback don't hold a mapping lease while they wait for an
// application task: the task may need a lease of its own while an exclusive
// operation waits for the held one.
func TestGuestRAMPageCacheIOWithApplicationWaits(t *testing.T) {
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: newStagingTestHost(), IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	mf := newGuestRAMTestMemoryFile(t, ram)
	// Span more than one writeback buffer and end mid-page.
	size := uint64(1<<20 + 3*GuestFrameSize - 100)
	want := make([]byte, size)
	for i := range want {
		want[i] = byte((i*131 + 17 + i/int(GuestFrameSize)*7) & 0xff)
	}
	ctx := gcontext.Background()
	mr := memmap.MappableRange{Start: 0, End: hostarch.MustPageRoundUp(size)}

	var cache fsutil.FileRangeSet
	if _, err := cache.Fill(ctx, mr, mr, size, mf, pgalloc.AllocOpts{
		Kind:                        usage.PageCache,
		Mode:                        pgalloc.AllocateAndWritePopulate,
		ReaderMayWaitForApplication: true,
	}, func(ctx gcontext.Context, dsts safemem.BlockSeq, offset uint64) (uint64, error) {
		if err := acquireLeaseLikeApplication(ram); err != nil {
			return 0, err
		}
		return safemem.CopySeq(dsts, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want[offset:])))
	}); err != nil && err != io.EOF {
		t.Fatalf("Fill: %v", err)
	}
	if cache.SpanRange(mr) != mr.Length() {
		t.Fatalf("Fill cached %d bytes, want %d", cache.SpanRange(mr), mr.Length())
	}
	for seg := cache.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		blocks, lease, err := memmap.MapInternalWithLease(mf, seg.FileRange(), hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, seg.Range().Length())
		n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), blocks)
		lease.Release()
		if err != nil || n != uint64(len(got)) {
			t.Fatalf("reading cache: n=%d err=%v", n, err)
		}
		wantSeg := make([]byte, len(got))
		if seg.Start() < size {
			copy(wantSeg, want[seg.Start():])
		}
		if !bytes.Equal(got, wantSeg) {
			t.Fatalf("cache contents of %v differ from the file", seg.Range())
		}
	}

	var dirty fsutil.DirtySet
	dirty.MarkDirty(mr)
	written := make([]byte, size)
	var writtenBytes uint64
	if err := fsutil.SyncDirty(ctx, mr, &cache, &dirty, size, mf, func(ctx gcontext.Context, srcs safemem.BlockSeq, offset uint64) (uint64, error) {
		if err := acquireLeaseLikeApplication(ram); err != nil {
			return 0, err
		}
		n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(written[offset:])), srcs)
		writtenBytes += n
		return n, err
	}); err != nil {
		t.Fatalf("SyncDirty: %v", err)
	}
	if writtenBytes != size || !bytes.Equal(written, want) {
		t.Fatalf("writeback wrote %d bytes that differ from the cache, want %d", writtenBytes, size)
	}
	cache.DropAll(mf)
	mf.Destroy()
}

// batchReleaseTestHost is a ramTestHost that releases many ranges at once.
type batchReleaseTestHost struct {
	*ramTestHost
	batches [][]HostRange
}

func (h *batchReleaseTestHost) ReleaseRanges(ranges []HostRange) error {
	h.batches = append(h.batches, append([]HostRange(nil), ranges...))
	for _, r := range ranges {
		if _, ok := h.reservations[r.Address]; !ok {
			return fmt.Errorf("releasing unreserved range %#x", r.Address)
		}
	}
	for _, r := range ranges {
		if err := h.Release(r.Address, r.Length); err != nil {
			return err
		}
	}
	return nil
}

// TestGuestRAMReleasesSlabsTogether checks that GuestRAM releases the slabs
// that a decommit empties together when the host can, and can then allocate
// frames again.
func TestGuestRAMReleasesSlabsTogether(t *testing.T) {
	host := &batchReleaseTestHost{ramTestHost: newRAMTestHost()}
	ram, err := NewGuestRAM(GuestRAMConfig{VM: &ramTestVM{}, Host: host, Revoker: &ramTestRevoker{}, IPAStart: 0x100000, IPASize: 1 << 24})
	if err != nil {
		t.Fatal(err)
	}
	backing, err := ram.NewBacking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		t.Fatal(err)
	}
	fr := memmap.FileRange{Start: 0, End: 4 * FramesPerSlab * GuestFrameSize}
	if _, err := backing.ResolveFrames(fr, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	if got := ram.Metrics().HostResidentBytes; got != 4*HostSlabSize {
		t.Fatalf("HostResidentBytes = %#x, want four slabs", got)
	}
	if err := backing.Decommit(fr); err != nil {
		t.Fatal(err)
	}
	if len(host.batches) != 1 || len(host.batches[0]) != 4 {
		t.Fatalf("released slabs in batches %v, want one batch of four", host.batches)
	}
	if got := ram.Metrics().HostResidentBytes; got != 0 {
		t.Errorf("HostResidentBytes after decommit = %#x, want 0", got)
	}
	if len(host.reservations) != 0 {
		t.Errorf("%d host reservations remain after release", len(host.reservations))
	}
	if _, err := backing.ResolveFrames(fr, hostarch.Write); err != nil {
		t.Fatalf("allocating after the release: %v", err)
	}
}

// slabTestHost tracks reserved host memory 16 KiB at a time, since munmap(2)
// can release part of a reservation.
type slabTestHost struct {
	*ramTestHost
	reserved map[uintptr]struct{}
}

func newSlabTestHost() *slabTestHost {
	return &slabTestHost{ramTestHost: newRAMTestHost(), reserved: make(map[uintptr]struct{})}
}

func (h *slabTestHost) Reserve(length uint64) (uintptr, error) {
	address, err := h.ramTestHost.Reserve(length)
	for offset := uint64(0); offset < length; offset += HostSlabSize {
		h.reserved[address+uintptr(offset)] = struct{}{}
	}
	return address, err
}

func (h *slabTestHost) Release(address uintptr, length uint64) error {
	for offset := uint64(0); offset < length; offset += HostSlabSize {
		if _, ok := h.reserved[address+uintptr(offset)]; !ok {
			return fmt.Errorf("releasing unreserved host memory at %#x", address+uintptr(offset))
		}
		delete(h.reserved, address+uintptr(offset))
	}
	return nil
}

// TestGuestRAMAllocatesSlabsInChunks checks that GuestRAM reserves and maps
// slabs a chunk at a time, but still releases them one at a time.
func TestGuestRAMAllocatesSlabsInChunks(t *testing.T) {
	vm := &ramTestVM{}
	host := newSlabTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: host, Revoker: &ramTestRevoker{}, IPAStart: 1 << 30, IPASize: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	backing, err := ram.NewBacking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Grow(0, pgalloc.BackingChunkSize, false); err != nil {
		t.Fatal(err)
	}
	const slabBytes = FramesPerSlab * GuestFrameSize
	const chunkBytes = maxSlabChunkSlabs * HostSlabSize
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 0, End: 5 * slabBytes}, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	if len(vm.maps) != 1 || vm.maps[0].length != chunkBytes {
		t.Fatalf("stage-2 maps %+v, want one of %#x bytes", vm.maps, chunkBytes)
	}
	if m := ram.Metrics(); m.HostResidentBytes != 5*HostSlabSize || m.IPABytesConsumed != chunkBytes {
		t.Fatalf("metrics %+v, want five resident slabs and one chunk of IPA", m)
	}

	// Emptying one slab releases just that slab.
	if err := backing.Decommit(memmap.FileRange{Start: 2 * slabBytes, End: 3 * slabBytes}); err != nil {
		t.Fatal(err)
	}
	if len(vm.unmaps) != 1 || vm.unmaps[0].gpa != vm.maps[0].gpa+2*HostSlabSize || vm.unmaps[0].length != HostSlabSize {
		t.Fatalf("stage-2 unmaps %+v, want the third slab of the chunk", vm.unmaps)
	}
	if got, want := len(host.reserved), maxSlabChunkSlabs-1; got != want {
		t.Fatalf("%d host slabs reserved, want %d", got, want)
	}

	// Using up the chunk maps another.
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: 5 * slabBytes, End: (5 + maxSlabChunkSlabs) * slabBytes}, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	if len(vm.maps) != 2 || vm.maps[1].gpa != vm.maps[0].gpa+chunkBytes {
		t.Fatalf("stage-2 maps %+v, want a second chunk after the first", vm.maps)
	}

	// A new epoch releases the unused slabs of the current chunk, which were
	// mapped only in the old VM, and maps a new chunk when it needs one.
	newVM := &ramTestVM{}
	if err := ram.RebindStage2(newVM); err != nil {
		t.Fatal(err)
	}
	live := uint64(len(ram.slabs))
	if got := uint64(len(host.reserved)); got != live {
		t.Fatalf("%d host slabs reserved after rebinding, want the %d live ones", got, live)
	}
	if _, err := backing.ResolveFrames(memmap.FileRange{Start: (5 + maxSlabChunkSlabs) * slabBytes, End: (6 + maxSlabChunkSlabs) * slabBytes}, hostarch.Write); err != nil {
		t.Fatal(err)
	}
	if n := len(newVM.maps); n != int(live)+1 || newVM.maps[n-1].length != chunkBytes {
		t.Fatalf("new VM maps %+v, want the live slabs and then a new chunk", newVM.maps)
	}
}
