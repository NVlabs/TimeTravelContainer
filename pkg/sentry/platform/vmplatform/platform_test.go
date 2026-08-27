// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vmplatform

import (
	"context"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

type relocationTestVM struct {
	mu          sync.Mutex
	commands    []monitorabi.Command
	applyCount  int
	failApplyAt int
}

func (*relocationTestVM) MapStage2(uint64, uintptr, uint64, Stage2Access) error { return nil }
func (*relocationTestVM) UnmapStage2(uint64, uint64) error                      { return nil }
func (v *relocationTestVM) CreateVCPU(int) (VCPU, error) {
	return relocationTestVCPU{vm: v}, nil
}
func (*relocationTestVM) Close() error { return nil }

type relocationTestVCPU struct{ vm *relocationTestVM }

func (relocationTestVCPU) Run(context.Context, any) (Exit, error) { return Exit{}, nil }
func (v relocationTestVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
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

func TestPreForkDoesNotBlockAddressSpaceMutation(t *testing.T) {
	as := &addressSpace{}
	as.PreFork()
	defer as.PostFork()
	done := make(chan struct{})
	go func() {
		as.mu.Lock()
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
	p := &Platform{
		pool:    pool,
		monitor: NewMonitor(pool),
		ram:     &GuestRAM{frames: make(map[pgalloc.FrameID]*guestFrame)},
		spaces:  make(map[uint64]*addressSpace),
	}
	return p, pool.Close
}

func TestAddressSpaceUnmapSkipsSparseHoles(t *testing.T) {
	vm := &relocationTestVM{}
	p, closePlatform := newAddressSpaceTestPlatform(t, vm)
	defer closePlatform()
	as := &addressSpace{id: 1, platform: p, mappings: map[hostarch.Addr]addressMapping{
		0x2000:      {frame: 1}, // Outside the invalidated range.
		0x4000:      {frame: 2},
		0x5000:      {frame: 3},
		0x100000000: {frame: 4},
	}}

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

func TestAddressSpaceUnmapRecordsAcknowledgedBatches(t *testing.T) {
	vm := &relocationTestVM{failApplyAt: 2}
	p, closePlatform := newAddressSpaceTestPlatform(t, vm)
	defer closePlatform()
	as := &addressSpace{id: 1, platform: p, mappings: make(map[hostarch.Addr]addressMapping)}
	for i := 0; i <= monitorabi.MaxOperations; i++ {
		as.mappings[hostarch.Addr(i+1)*hostarch.PageSize] = addressMapping{frame: pgalloc.FrameID(i + 1)}
	}

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
	p := &Platform{pool: pool, monitor: NewMonitor(pool), spaces: make(map[uint64]*addressSpace)}
	frame := pgalloc.FrameID(44)
	for id, va := range map[uint64]hostarch.Addr{1: 0x4000, 2: 0x8000} {
		p.spaces[id] = &addressSpace{id: id, platform: p, mappings: map[hostarch.Addr]addressMapping{
			va: {frame: frame, access: hostarch.ReadWrite},
		}}
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
	p := &Platform{pool: pool, monitor: NewMonitor(pool), spaces: make(map[uint64]*addressSpace)}
	frame := pgalloc.FrameID(45)
	for id, va := range map[uint64]hostarch.Addr{1: 0x4000, 2: 0x8000} {
		p.spaces[id] = &addressSpace{id: id, platform: p, mappings: map[hostarch.Addr]addressMapping{
			va: {frame: frame, access: hostarch.Read},
		}}
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
