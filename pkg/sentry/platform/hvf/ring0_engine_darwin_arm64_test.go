// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64 && cgo

package hvf

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

func runRing0InternalMonitor(t *testing.T, engine *ring0Engine, command monitorabi.Command) {
	t.Helper()
	command.Sequence = 1
	encoded, err := monitorabi.MarshalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, err := engine.RunMonitor(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	response, err := monitorabi.UnmarshalResponse(responseBytes)
	if err != nil || response.Sequence != command.Sequence || response.Status != monitorabi.StatusOK {
		t.Fatalf("monitor response=%+v err=%v", response, err)
	}
}

func TestHostStage1ReconcileOperations(t *testing.T) {
	current := map[uint64]monitorabi.Access{
		0x1000: monitorabi.AccessRead,
		0x2000: monitorabi.AccessRead,
		0x3000: monitorabi.AccessRead,
		0x4000: monitorabi.AccessRead | monitorabi.AccessWrite,
		0x7000: monitorabi.AccessRead,
	}
	desired := map[uint64]monitorabi.Access{
		0x1000: monitorabi.AccessRead,
		0x2000: monitorabi.AccessRead | monitorabi.AccessWrite,
		0x3000: monitorabi.AccessRead | monitorabi.AccessWrite,
		0x5000: monitorabi.AccessRead | monitorabi.AccessExecute,
		0x6000: monitorabi.AccessRead | monitorabi.AccessExecute,
	}
	desiredGPA := map[uint64]uint64{0x1000: 0x11000, 0x2000: 0x12000, 0x3000: 0x13000, 0x5000: 0x25000, 0x6000: 0x26000}
	got := hostStage1ReconcileOperations(current, desired, desiredGPA)
	want := []monitorabi.Operation{
		{Type: monitorabi.OperationProtect, Access: monitorabi.AccessRead | monitorabi.AccessWrite, VA: 0x2000, Length: 0x2000},
		{Type: monitorabi.OperationUnmap, VA: 0x4000, Length: 0x1000},
		{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: 0x5000, GPA: 0x25000, Length: 0x2000},
		{Type: monitorabi.OperationUnmap, VA: 0x7000, Length: 0x1000},
	}
	if len(got) != len(want) {
		t.Fatalf("reconcile operations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reconcile operation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHostStage1UnmapOperationsCoalesceSlabs(t *testing.T) {
	pages := map[uint64]struct{}{0x1000: {}, 0x2000: {}, 0x4000: {}, 0x5000: {}, 0x6000: {}}
	got := hostStage1UnmapOperations(pages)
	want := []monitorabi.Operation{
		{Type: monitorabi.OperationUnmap, VA: 0x1000, Length: 0x2000},
		{Type: monitorabi.OperationUnmap, VA: 0x4000, Length: 0x3000},
	}
	if len(got) != len(want) {
		t.Fatalf("unmap operations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unmap operation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRing0HostCopyContainsVanishedRegion(t *testing.T) {
	allocator := hostMemory{}
	source, err := allocator.Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := allocator.Reserve(vmm.HostSlabSize)
	if err != nil {
		_ = allocator.Release(source, vmm.HostSlabSize)
		t.Fatal(err)
	}
	defer allocator.Release(destination, vmm.HostSlabSize)
	if err := allocator.Release(source, vmm.HostSlabSize); err != nil {
		t.Fatal(err)
	}
	if err := copyRing0HostMemory(destination, source, vmm.HostSlabSize); err == nil {
		t.Fatal("Mach copy unexpectedly accepted a vanished source region")
	}
}

func TestDropSkippedHostSlabs(t *testing.T) {
	const skipped = uintptr(0x4000)
	desiredSlabs := map[uintptr]ring0Stage2Identity{
		skipped: {objectID: 1},
		0x8000:  {objectID: 2},
	}
	desiredAccess := make(map[uint64]monitorabi.Access)
	for va := uint64(skipped); va < uint64(skipped)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
		desiredAccess[va] = monitorabi.AccessRead
	}
	desiredAccess[0x8000] = monitorabi.AccessRead
	dropSkippedHostSlabs([]uintptr{skipped}, desiredSlabs, desiredAccess)
	if _, ok := desiredSlabs[skipped]; ok {
		t.Fatal("skipped slab remains desired")
	}
	for va := uint64(skipped); va < uint64(skipped)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
		if _, ok := desiredAccess[va]; ok {
			t.Fatalf("skipped stage-1 page %#x remains desired", va)
		}
	}
	if _, ok := desiredSlabs[0x8000]; !ok {
		t.Fatal("unrelated slab was removed")
	}
}

func TestRing0ConcurrentSyscallMonitorAndRecycle(t *testing.T) {
	hostPID := os.Getpid()
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 2)
	if err != nil {
		t.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(40)
		codeVA  = uint64(0x900000)
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: spaceID,
	})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     codeVA,
			GPA:    GuestRAMIPAStart,
			Length: vmm.LogicalPageSize,
		}},
	})
	activate, err := monitorabi.MarshalCommand(monitorabi.Command{
		Sequence:       50,
		Opcode:         monitorabi.OpcodeActivate,
		AddressSpaceID: spaceID,
	})
	if err != nil {
		t.Fatal(err)
	}

	const (
		workers    = 2
		iterations = 20
	)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				registers := &vmm.ARM64State{PC: codeVA}
				exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
					AddressSpaceID: spaceID,
					Registers:      registers,
				})
				if err != nil || exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
					errs <- fmt.Errorf("worker %d iteration %d application exit=%+v registers={x8=%d pc=%#x} err=%v", worker, iteration, exit, registers.X[8], registers.PC, err)
					return
				}
				responseBytes, err := engine.RunMonitor(context.Background(), activate)
				if err != nil {
					errs <- fmt.Errorf("worker %d iteration %d monitor: %w", worker, iteration, err)
					return
				}
				response, err := monitorabi.UnmarshalResponse(responseBytes)
				if err != nil || response.Sequence != 50 || response.Status != monitorabi.StatusOK {
					errs <- fmt.Errorf("worker %d iteration %d monitor response=%+v err=%v", worker, iteration, response, err)
					return
				}
				// Whether the monitor transaction stayed direct or redpilled due to
				// lock contention, finish this iteration at host EL0. The next entry
				// must recycle an owner-destroyed vCPU ID safely.
				if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
					errs <- fmt.Errorf("worker %d iteration %d getpid=%d, want %d", worker, iteration, got, hostPID)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
}
