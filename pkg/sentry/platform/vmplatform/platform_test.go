// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

type relocationTestVM struct {
	mu          sync.Mutex
	commands    []monitorabi.Command
	applyCount  int
	failApplyAt int
	applyOnce   sync.Once
	applyStart  chan struct{}
	applyFinish chan struct{}
}

func (*relocationTestVM) MapStage2(uint64, uintptr, uint64, Stage2Access) error { return nil }
func (*relocationTestVM) UnmapStage2(uint64, uint64) error                      { return nil }
func (v *relocationTestVM) CreateVCPU(int) (VCPU, error) {
	return relocationTestVCPU{vm: v}, nil
}
func (*relocationTestVM) Close() error { return nil }

type relocationTestVCPU struct{ vm *relocationTestVM }

type flushCountingEngine struct {
	*workerPoolTestEngine
	flushes int
}

func (e *flushCountingEngine) FlushHostExecutionCache(context.Context) error {
	e.flushes++
	return nil
}

func TestMapUnitFitsOneMonitorTransaction(t *testing.T) {
	if got, want := (&Platform{}).MapUnit(), uint64(monitorabi.MaxOperations*hostarch.PageSize); got != want {
		t.Fatalf("MapUnit = %#x, want one monitor transaction (%#x)", got, want)
	}
}

func (v relocationTestVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
	}
	if command.Opcode == monitorabi.OpcodeApply && v.vm.applyStart != nil {
		v.vm.applyOnce.Do(func() { close(v.vm.applyStart) })
		if v.vm.applyFinish != nil {
			<-v.vm.applyFinish
		}
	}
	v.vm.mu.Lock()
	v.vm.commands = append(v.vm.commands, command)
	status := monitorabi.StatusOK
	if command.Opcode == monitorabi.OpcodeApply {
		v.vm.applyCount++
		if v.vm.applyCount == v.vm.failApplyAt {
			status = monitorabi.StatusInternal
		}
	}
	v.vm.mu.Unlock()
	return monitorabi.MarshalResponse(monitorabi.Response{Sequence: command.Sequence, Status: status}), nil
}

func TestAddressSpaceMapPinsFramesUntilReverseMapIsVisible(t *testing.T) {
	vm := &relocationTestVM{applyStart: make(chan struct{}), applyFinish: make(chan struct{})}
	host := newRAMTestHost()
	ram, err := NewGuestRAM(GuestRAMConfig{VM: vm, Host: host, IPAStart: 0x100000, IPASize: 1 << 20})
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
	fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateUncommitted})
	if err != nil {
		t.Fatal(err)
	}

	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer func() {
		select {
		case <-vm.applyFinish:
		default:
			close(vm.applyFinish)
		}
	}()
	p := &Platform{
		engine:  newWorkerPoolTestEngine(pool),
		ram:     ram,
		spaces:  make(map[uint64]*addressSpace),
		staging: &stagingCache{},
	}
	p.monitor = NewMonitor(p.engine, monitorPoolExpander(vm))
	as := indexed(&addressSpace{id: 1, platform: p, mappings: make(map[hostarch.Addr]addressMapping)})
	p.spaces[as.id] = as
	ram.SetRevoker(p)

	mapDone := make(chan error, 1)
	go func() {
		mapDone <- as.MapFile(0x4000, mf, fr, hostarch.Read, false)
	}()
	select {
	case <-vm.applyStart:
	case <-time.After(time.Second):
		t.Fatal("stage-1 map did not reach the monitor")
	}
	decommitDone := make(chan struct{})
	go func() {
		mf.Decommit(memmap.FileRange{Start: fr.Start, End: fr.End})
		close(decommitDone)
	}()
	select {
	case <-decommitDone:
		t.Fatal("Decommit retired a frame while its stage-1 map transaction was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(vm.applyFinish)
	if err := <-mapDone; err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	select {
	case <-decommitDone:
	case <-time.After(time.Second):
		t.Fatal("Decommit did not continue after the stage-1 mapping lease was released")
	}
	if len(as.mappings) != 0 {
		t.Fatalf("decommit left stale address-space mappings: %+v", as.mappings)
	}
	mf.DecRef(fr)
	mf.Destroy()
}
func (relocationTestVCPU) RequestExit() error { return nil }
func (relocationTestVCPU) Close() error       { return nil }

func TestPrepareRestoreRequiresReleasedAddressSpaces(t *testing.T) {
	p := &Platform{spaces: map[uint64]*addressSpace{1: {}}}
	if err := p.PrepareRestore(); err == nil {
		t.Fatal("PrepareRestore succeeded with a live bootstrap address space")
	}
	delete(p.spaces, 1)
	if err := p.PrepareRestore(); err != nil {
		t.Fatalf("PrepareRestore after address-space release: %v", err)
	}
	p.closed = true
	if err := p.PrepareRestore(); err == nil {
		t.Fatal("PrepareRestore succeeded on a closed platform")
	}
}

func TestFlushForCheckpointFlushesHostExecutionCache(t *testing.T) {
	vm := &relocationTestVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	engine := &flushCountingEngine{workerPoolTestEngine: newWorkerPoolTestEngine(pool)}
	staging := &stagingCache{pages: make(map[stagingKey]*stagedPage)}
	staging.cond = sync.NewCond(&staging.mu)
	p := &Platform{
		engine:  engine,
		staging: staging,
		spaces:  make(map[uint64]*addressSpace),
	}
	if err := p.FlushForCheckpoint(nil); err != nil {
		t.Fatal(err)
	}
	if engine.flushes != 1 {
		t.Fatalf("host execution-cache flushes = %d, want 1", engine.flushes)
	}
}

func TestPreForkDoesNotBlockAddressSpaceMutation(t *testing.T) {
	as := &addressSpace{}
	as.PreFork()
	defer as.PostFork()
	done := make(chan struct{})
	go func() {
		as.mu.Lock()
		_ = as.released
		as.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PreFork retained the address-space mutation lock; MM CoW unmap would deadlock")
	}
}

func newAddressSpaceTestPlatform(t *testing.T, vm *relocationTestVM) (*Platform, func()) {
	t.Helper()
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	engine := newWorkerPoolTestEngine(pool)
	p := &Platform{
		engine:  engine,
		monitor: NewMonitor(engine, monitorPoolExpander(vm)),
		ram:     &GuestRAM{frames: make(map[pgalloc.FrameID]*guestFrame)},
		spaces:  make(map[uint64]*addressSpace),
	}
	return p, pool.Close
}

func TestAddressSpaceUnmapSkipsSparseHoles(t *testing.T) {
	vm := &relocationTestVM{}
	p, closePlatform := newAddressSpaceTestPlatform(t, vm)
	defer closePlatform()
	as := indexed(&addressSpace{id: 1, platform: p, mappings: map[hostarch.Addr]addressMapping{
		0x2000:      {frame: 1}, // Outside the invalidated range.
		0x4000:      {frame: 2},
		0x5000:      {frame: 3},
		0x100000000: {frame: 4},
	}})

	// This is representative of MemoryManager's extrapolated sparse unmaps:
	// nearly the entire application range contains only three installed leaves.
	as.Unmap(0x3000, (1<<48)-0x3000)

	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.commands) != 1 {
		t.Fatalf("monitor commands = %+v, want one sparse apply", vm.commands)
	}
	ops := vm.commands[0].Operations
	if len(ops) != 2 || ops[0].VA != 0x4000 || ops[0].Length != 2*hostarch.PageSize || ops[1].VA != 0x100000000 || ops[1].Length != hostarch.PageSize {
		t.Fatalf("sparse unmap operations = %+v", ops)
	}
	if len(as.mappings) != 1 {
		t.Fatalf("remaining mappings = %+v, want only out-of-range mapping", as.mappings)
	}
	if _, ok := as.mappings[0x2000]; !ok {
		t.Fatalf("out-of-range mapping was removed: %+v", as.mappings)
	}
}

// TestAddressSpaceForkBatchesUnmaps checks that the unmaps that establish a
// fork's copy-on-write, between PreFork and PostFork, take one monitor
// transaction, rather than one for each range.
func TestAddressSpaceForkBatchesUnmaps(t *testing.T) {
	vm := &relocationTestVM{}
	p, closePlatform := newAddressSpaceTestPlatform(t, vm)
	defer closePlatform()
	as := indexed(&addressSpace{id: 1, platform: p, mappings: map[hostarch.Addr]addressMapping{
		0x2000: {frame: 1},
		0x4000: {frame: 2},
		0x5000: {frame: 3},
		0x8000: {frame: 4},
		0x9000: {frame: 5}, // Not unmapped.
	}})
	as.PreFork()
	as.Unmap(0x8000, hostarch.PageSize)
	as.Unmap(0x2000, hostarch.PageSize)
	as.Unmap(0x4000, 2*hostarch.PageSize)
	// An overlapping range unmaps nothing twice.
	as.Unmap(0x5000, hostarch.PageSize)
	vm.mu.Lock()
	deferred := len(vm.commands)
	vm.mu.Unlock()
	if deferred != 0 {
		t.Fatalf("Unmap issued %d monitor commands during a fork, want none until PostFork", deferred)
	}
	as.PostFork()

	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.commands) != 1 {
		t.Fatalf("monitor commands = %+v, want one", vm.commands)
	}
	ops := vm.commands[0].Operations
	if len(ops) != 3 || ops[0].VA != 0x2000 || ops[1].VA != 0x4000 || ops[1].Length != 2*hostarch.PageSize || ops[2].VA != 0x8000 {
		t.Fatalf("fork unmap operations = %+v", ops)
	}
	if len(as.mappings) != 1 {
		t.Fatalf("remaining mappings = %+v, want only 0x9000", as.mappings)
	}
	// After the fork, Unmap unmaps at once.
	vm.mu.Unlock()
	as.Unmap(0x9000, hostarch.PageSize)
	vm.mu.Lock()
	if len(vm.commands) != 2 || len(as.mappings) != 0 {
		t.Fatalf("after the fork, monitor commands = %d and mappings = %+v", len(vm.commands), as.mappings)
	}
}

func TestAddressSpaceUnmapRecordsAcknowledgedBatches(t *testing.T) {
	vm := &relocationTestVM{failApplyAt: 2}
	p, closePlatform := newAddressSpaceTestPlatform(t, vm)
	defer closePlatform()
	as := &addressSpace{id: 1, platform: p, mappings: make(map[hostarch.Addr]addressMapping)}
	for i := 0; i <= monitorabi.MaxOperations; i++ {
		as.mappings[hostarch.Addr(i+1)*hostarch.PageSize] = addressMapping{frame: pgalloc.FrameID(i + 1)}
	}
	indexed(as)

	as.Unmap(hostarch.PageSize, uint64(monitorabi.MaxOperations+1)*hostarch.PageSize)

	if len(as.mappings) != 1 {
		t.Fatalf("remaining mappings = %d, want failed final batch only", len(as.mappings))
	}
	if _, ok := as.mappings[hostarch.Addr(monitorabi.MaxOperations+1)*hostarch.PageSize]; !ok {
		t.Fatalf("failed final batch was removed: %+v", as.mappings)
	}
	if as.stickyWriteback == nil {
		t.Fatal("monitor failure was not preserved")
	}
}

func TestRelocateFramesReplacesEveryReverseMapping(t *testing.T) {
	vm := &relocationTestVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	engine := newWorkerPoolTestEngine(pool)
	p := &Platform{engine: engine, monitor: NewMonitor(engine, monitorPoolExpander(vm)), spaces: make(map[uint64]*addressSpace)}
	frame := pgalloc.FrameID(44)
	for id, va := range map[uint64]hostarch.Addr{1: 0x4000, 2: 0x8000} {
		p.spaces[id] = indexed(&addressSpace{id: id, platform: p, mappings: map[hostarch.Addr]addressMapping{
			va: {frame: frame, access: hostarch.ReadWrite},
		}})
	}
	err = p.RelocateFrames(context.Background(), map[pgalloc.FrameID]FrameRelocation{
		frame: {OldGPA: 0x100000, NewGPA: 0x204000, References: []MappingRef{
			{AddressSpaceID: 1, VirtualAddress: 0x4000},
			{AddressSpaceID: 2, VirtualAddress: 0x8000},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.commands) != 2 {
		t.Fatalf("monitor commands = %+v, want one per address space", vm.commands)
	}
	for _, command := range vm.commands {
		if len(command.Operations) != 1 || command.Operations[0].Type != monitorabi.OperationMap || command.Operations[0].GPA != 0x204000 {
			t.Fatalf("relocation command = %+v", command)
		}
	}
}

func TestRelocateFramesRollsBackCompletedRoots(t *testing.T) {
	vm := &relocationTestVM{failApplyAt: 2}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	engine := newWorkerPoolTestEngine(pool)
	p := &Platform{engine: engine, monitor: NewMonitor(engine, monitorPoolExpander(vm)), spaces: make(map[uint64]*addressSpace)}
	frame := pgalloc.FrameID(45)
	for id, va := range map[uint64]hostarch.Addr{1: 0x4000, 2: 0x8000} {
		p.spaces[id] = indexed(&addressSpace{id: id, platform: p, mappings: map[hostarch.Addr]addressMapping{
			va: {frame: frame, access: hostarch.Read},
		}})
	}
	err = p.RelocateFrames(context.Background(), map[pgalloc.FrameID]FrameRelocation{
		frame: {OldGPA: 0x104000, NewGPA: 0x208000, References: []MappingRef{
			{AddressSpaceID: 1, VirtualAddress: 0x4000},
			{AddressSpaceID: 2, VirtualAddress: 0x8000},
		}},
	})
	if err == nil {
		t.Fatal("RelocateFrames succeeded despite second-root failure")
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.commands) != 3 {
		t.Fatalf("monitor commands = %+v, want replacement, failure, rollback", vm.commands)
	}
	gotGPAs := []uint64{
		vm.commands[0].Operations[0].GPA,
		vm.commands[1].Operations[0].GPA,
		vm.commands[2].Operations[0].GPA,
	}
	wantGPAs := []uint64{0x208000, 0x208000, 0x104000}
	for i := range wantGPAs {
		if gotGPAs[i] != wantGPAs[i] {
			t.Fatalf("monitor GPA sequence = %#x, want %#x", gotGPAs, wantGPAs)
		}
	}
}

// indexed returns as with the ordered index of its mappings built, as
// NewAddressSpace and MapFile keep it.
func indexed(as *addressSpace) *addressSpace {
	as.mapped = newMappedSet()
	for va := range as.mappings {
		as.mapped.ReplaceOrInsert(va)
	}
	return as
}
