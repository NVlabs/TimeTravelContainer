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
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

func runRing0InternalMonitor(t testing.TB, engine *ring0Engine, command monitorabi.Command) {
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

func TestApplicationWaiterQueuePreservesFIFO(t *testing.T) {
	engine := &ring0Engine{}
	first := engine.enqueueApplicationWaiterLocked()
	second := engine.enqueueApplicationWaiterLocked()
	third := engine.enqueueApplicationWaiterLocked()
	if got := engine.applicationWaiters.Load(); got != 3 {
		t.Fatalf("application waiters = %d, want 3", got)
	}
	engine.removeApplicationWaiterLocked(second)
	if len(engine.cpuWaiters) != 2 || engine.cpuWaiters[0] != first || engine.cpuWaiters[1] != third {
		t.Fatalf("waiter queue after middle removal = %p, want [%p %p]", engine.cpuWaiters, first, third)
	}
	engine.removeApplicationWaiterLocked(first)
	if len(engine.cpuWaiters) != 1 || engine.cpuWaiters[0] != third {
		t.Fatalf("waiter queue after head removal = %p, want [%p]", engine.cpuWaiters, third)
	}
	engine.removeApplicationWaiterLocked(third)
	if got := engine.applicationWaiters.Load(); got != 0 || len(engine.cpuWaiters) != 0 {
		t.Fatalf("application waiters after drain = %d queue=%p", got, engine.cpuWaiters)
	}
}

func TestRequestHostMirrorIgnoresAddressesOutsideSentryVA(t *testing.T) {
	engine := &ring0Engine{pendingMirror: make(map[uintptr]struct{})}
	engine.requestHostMirror(0)
	engine.requestHostMirror(uintptr(ring0HostVALimit))
	if got := len(engine.pendingMirror); got != 0 {
		t.Fatalf("out-of-range host mirror requests queued %d slabs, want 0", got)
	}

	const address = uintptr(0x12345)
	engine.requestHostMirror(address)
	want := address &^ uintptr(vmm.HostSlabSize-1)
	if _, ok := engine.pendingMirror[want]; !ok || len(engine.pendingMirror) != 1 {
		t.Fatalf("valid host mirror request queued %+v, want only %#x", engine.pendingMirror, want)
	}
}

func TestRing0HostRegionAt(t *testing.T) {
	allocation := make([]byte, vmm.HostSlabSize)
	target := uintptr(unsafe.Pointer(&allocation[len(allocation)/2]))
	region, ok, err := ring0HostRegionAt(target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no Darwin VM region contains heap address %#x", target)
	}
	if target < region.address || uint64(target-region.address) >= region.length {
		t.Fatalf("region [%#x, %#x) does not contain %#x", region.address, region.address+uintptr(region.length), target)
	}
	if region.protection&ring0VMWrite == 0 {
		t.Fatalf("heap region at %#x has protection %#x, want writable", target, region.protection)
	}
	runtime.KeepAlive(allocation)

	if region, ok, err := ring0HostRegionAt(0); err != nil || ok {
		t.Fatalf("Darwin VM region at zero = %+v, ok=%t, err=%v; want absent", region, ok, err)
	}
}

func TestNativeResumeTrapAddress(t *testing.T) {
	if got, want := ring0asm.NativeResumeTrapAddress(), ring0asm.NativeResumeAddress()+4; got != want {
		t.Fatalf("native-resume trap address = %#x, want %#x", got, want)
	}
}

func TestSwitchApplicationGuestTrapAddress(t *testing.T) {
	if got, want := ring0asm.SwitchApplicationGuestTrapAddress(), ring0asm.SwitchApplicationGuestAddress()+4; got != want {
		t.Fatalf("guest-switch trap address = %#x, want %#x", got, want)
	}
}

func TestDirectApplicationModePromotionAndRetry(t *testing.T) {
	mode := &ring0ApplicationMode{}
	for run := uint64(1); run < applicationEL1PromotionRuns; run++ {
		recordDirectApplicationRun(mode)
		if mode.promoted.Load() {
			t.Fatalf("application mode promoted after %d direct runs, want %d", run, applicationEL1PromotionRuns)
		}
	}
	recordDirectApplicationRun(mode)
	if !mode.promoted.Load() {
		t.Fatalf("application mode did not promote after %d direct entries", applicationEL1PromotionRuns)
	}

	mode.cooldown.Store(applicationEL1PromotionRuns)
	for range applicationEL1PromotionRuns {
		recordDirectApplicationRun(mode)
	}
	if got := mode.cooldown.Load(); got != 0 {
		t.Fatalf("application cooldown after retry entry = %d, want 0", got)
	}
}

func TestApplicationResidencyYieldsOnlyUnderContention(t *testing.T) {
	engine := &ring0Engine{concurrency: 2}
	engine.cpusByID[0] = &ring0ApplicationCPU{tid: hosttid.Current(), residentSince: time.Now().Add(-applicationResidencyQuantum)}
	engine.applicationCPUs.Store(2)
	if engine.ShouldYieldApplicationResidency() {
		t.Fatal("fully leased application pool requested an uncontended yield")
	}
	engine.applicationWaiters.Store(1)
	if !engine.ShouldYieldApplicationResidency() {
		t.Fatal("contended application pool retained an expired residency")
	}
}

func TestFaultResidencyReservesHalfPool(t *testing.T) {
	single := &ring0Engine{concurrency: 1}
	single.applicationCPUs.Store(1)
	if single.ShouldRetainApplicationFaultResidency() {
		t.Fatal("single-vCPU pool cannot retain a fault residency and reserve progress capacity")
	}
	engine := &ring0Engine{concurrency: 8}
	engine.applicationCPUs.Store(4)
	if !engine.ShouldRetainApplicationFaultResidency() {
		t.Fatal("four fault residencies should fit in half of an eight-vCPU pool")
	}
	engine.applicationCPUs.Store(5)
	if engine.ShouldRetainApplicationFaultResidency() {
		t.Fatal("fifth fault residency should be released to preserve progress capacity")
	}
}

func TestApplicationWaiterInterruptsCPUBoundIncumbent(t *testing.T) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()

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
	binary.LittleEndian.PutUint32(memory[0:], 0x14000000)                     // b .
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize:], 0xd2801588)   // movz x8, #172
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize+4:], 0xd4000001) // svc #0

	const (
		spaceID   = uint64(41)
		loopVA    = uint64(0xa00000)
		syscallVA = uint64(0xa01000)
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{
			{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: loopVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize},
			{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: syscallVA, GPA: GuestRAMIPAStart + vmm.LogicalPageSize, Length: vmm.LogicalPageSize},
		},
	})

	type applicationResult struct {
		exit vmm.Exit
		regs *vmm.ARM64State
		err  error
	}
	incumbentDone := make(chan applicationResult, 1)
	go func() {
		engine.BeginApplicationResidency()
		regs := &vmm.ARM64State{PC: loopVA}
		exit, runErr := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: spaceID, Registers: regs})
		endErr := engine.EndApplicationResidency()
		incumbentDone <- applicationResult{exit: exit, regs: regs, err: errors.Join(runErr, endErr)}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		engine.cpuMu.Lock()
		active := engine.applicationCPUCountLocked() == 1 && engine.cpusByID[0].applicationActive.Load()
		engine.cpuMu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CPU-bound incumbent did not enter EL0")
		}
		time.Sleep(100 * time.Microsecond)
	}

	waiterDone := make(chan applicationResult, 1)
	go func() {
		engine.BeginApplicationResidency()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		regs := &vmm.ARM64State{PC: syscallVA}
		exit, runErr := engine.RunApplication(ctx, vmm.ApplicationState{AddressSpaceID: spaceID, Registers: regs})
		endErr := engine.EndApplicationResidency()
		waiterDone <- applicationResult{exit: exit, regs: regs, err: errors.Join(runErr, endErr)}
	}()

	incumbent := <-incumbentDone
	if incumbent.err != nil || incumbent.exit.Reason != vmm.ExitInterrupted {
		t.Fatalf("CPU-bound incumbent exit=%+v registers=%+v err=%v", incumbent.exit, incumbent.regs, incumbent.err)
	}
	waiter := <-waiterDone
	if waiter.err != nil || waiter.exit.Reason != vmm.ExitSyscall || waiter.regs.X[8] != 172 || waiter.regs.PC != syscallVA+8 {
		t.Fatalf("waiter exit=%+v registers={x8=%d pc=%#x} err=%v", waiter.exit, waiter.regs.X[8], waiter.regs.PC, waiter.err)
	}
}

func TestTranslationMutationInterruptsCPUBoundApplication(t *testing.T) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()

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
	binary.LittleEndian.PutUint32(memory, 0x14000000) // b .

	const (
		spaceID = uint64(42)
		loopVA  = uint64(0xa00000)
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: loopVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})

	type applicationResult struct {
		exit vmm.Exit
		err  error
	}
	applicationDone := make(chan applicationResult, 1)
	go func() {
		engine.BeginApplicationResidency()
		registers := &vmm.ARM64State{PC: loopVA}
		exit, runErr := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		applicationDone <- applicationResult{exit: exit, err: errors.Join(runErr, engine.EndApplicationResidency())}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		engine.cpuMu.Lock()
		cpu := engine.cpusByID[0]
		inGuest := cpu != nil && cpu.applicationActive.Load() && cpu.bridge.inGuest()
		engine.cpuMu.Unlock()
		if inGuest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CPU-bound application did not enter HVF")
		}
		time.Sleep(100 * time.Microsecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := monitorabi.Command{
		Sequence: 1,
		Opcode:   monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationProtect, Access: monitorabi.AccessRead,
			VA: loopVA, Length: vmm.LogicalPageSize,
		}},
	}
	encoded, err := monitorabi.MarshalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, err := engine.RunMonitor(ctx, encoded)
	if err != nil {
		t.Fatalf("translation mutation did not interrupt CPU-bound application: %v", err)
	}
	response, err := monitorabi.UnmarshalResponse(responseBytes)
	if err != nil || response.Status != monitorabi.StatusOK {
		t.Fatalf("monitor response=%+v err=%v", response, err)
	}
	application := <-applicationDone
	if application.err != nil || application.exit.Reason != vmm.ExitInterrupted {
		t.Fatalf("CPU-bound application exit=%+v err=%v", application.exit, application.err)
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
	replace, err := monitorabi.MarshalCommand(monitorabi.Command{
		Sequence:       50,
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{
			{
				Type:   monitorabi.OperationUnmap,
				VA:     codeVA,
				Length: vmm.LogicalPageSize,
			},
			{
				Type:   monitorabi.OperationMap,
				Access: monitorabi.AccessRead | monitorabi.AccessExecute,
				VA:     codeVA,
				GPA:    GuestRAMIPAStart,
				Length: vmm.LogicalPageSize,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const (
		workers    = 2
		iterations = 100
	)
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			engine.BeginApplicationResidency()
			defer func() {
				if err := engine.EndApplicationResidency(); err != nil {
					errs <- fmt.Errorf("worker %d ending residency: %w", worker, err)
				}
			}()
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
				responseBytes, err := engine.RunMonitor(context.Background(), replace)
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
				// lock contention, the next application entry must reuse this task's
				// owner-affine vCPU without reconstructing it.
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

func TestMonitorRecyclesDestroyedAddressSpaceTables(t *testing.T) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()

	initialPools := len(vm.monitor.pools)
	const (
		cycles  = 1500
		spaceID = uint64(71)
	)
	for cycle := 0; cycle < cycles; cycle++ {
		runRing0InternalMonitor(t, engine, monitorabi.Command{
			Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID,
		})
		runRing0InternalMonitor(t, engine, monitorabi.Command{
			Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
			Operations: []monitorabi.Operation{{
				Type: monitorabi.OperationMap, Access: monitorabi.AccessRead,
				VA: 0x400000, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
			}},
		})
		runRing0InternalMonitor(t, engine, monitorabi.Command{
			Opcode: monitorabi.OpcodeDestroyAddressSpace, AddressSpaceID: spaceID,
		})
	}
	if got := len(vm.monitor.pools); got != initialPools {
		t.Fatalf("monitor pool regions after %d create/map/destroy cycles = %d, want %d", cycles, got, initialPools)
	}
}

func BenchmarkRing0ApplicationFastPath(b *testing.B) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		b.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 1)
	if err != nil {
		b.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()
	engine.BeginApplicationResidency()
	defer func() {
		if err := engine.EndApplicationResidency(); err != nil {
			b.Errorf("ending application residency: %v", err)
		}
	}()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		b.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		b.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(91)
		codeVA  = uint64(0xb00000)
	)
	run := func(sequence uint64, command monitorabi.Command) {
		command.Sequence = sequence
		encoded, err := monitorabi.MarshalCommand(command)
		if err != nil {
			b.Fatal(err)
		}
		responseBytes, err := engine.RunMonitor(context.Background(), encoded)
		if err != nil {
			b.Fatal(err)
		}
		response, err := monitorabi.UnmarshalResponse(responseBytes)
		if err != nil || response.Status != monitorabi.StatusOK {
			b.Fatalf("monitor response=%+v err=%v", response, err)
		}
	}
	run(1, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	run(2, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})
	registers := &vmm.ARM64State{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registers.PC = codeVA
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
			b.Fatalf("application exit=%+v pc=%#x err=%v", exit, registers.PC, err)
		}
	}
}

// BenchmarkRing0ApplicationColdResidency measures the full cost paid when a
// Linux task blocks, execs, or exits between application entries. Unlike the
// fast-path benchmark, each iteration creates and retires the owner-affine HVF
// vCPU. Keep this benchmark representative of process churn: optimizing the
// syscall world switch while leaving residency turnover expensive still makes
// fork/exec workloads unusable.
func BenchmarkRing0ApplicationColdResidency(b *testing.B) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		b.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	engineInterface, err := NewVMEngine(vm, 1)
	if err != nil {
		b.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		b.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		b.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(93)
		codeVA  = uint64(0xb00000)
	)
	runRing0InternalMonitor(b, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(b, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})
	registers := &vmm.ARM64State{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.BeginApplicationResidency()
		registers.PC = codeVA
		exit, runErr := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		endErr := engine.EndApplicationResidency()
		if runErr != nil || endErr != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
			b.Fatalf("cold application exit=%+v pc=%#x runErr=%v endErr=%v", exit, registers.PC, runErr, endErr)
		}
	}
}

// BenchmarkRing0ApplicationFastPathParallel measures the backend-owned world
// switch without Sentry syscall work while every host application CPU is busy.
// Keep this separate from the serial benchmark: a regression here identifies
// Hypervisor.framework/vCPU coordination rather than Linux syscall contention.
func BenchmarkRing0ApplicationFastPathParallel(b *testing.B) {
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		b.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	vm := vmInterface.(*hvfVM)
	defer vm.Close()
	vcpus := min(runtime.GOMAXPROCS(0), ring0MonitorVCPUID)
	engineInterface, err := NewVMEngine(vm, vcpus)
	if err != nil {
		b.Fatal(err)
	}
	engine := engineInterface.(*ring0Engine)
	defer engine.Close()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		b.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		b.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(92)
		codeVA  = uint64(0xb00000)
	)
	runRing0InternalMonitor(b, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(b, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		engine.BeginApplicationResidency()
		defer func() {
			if err := engine.EndApplicationResidency(); err != nil {
				b.Errorf("ending parallel application residency: %v", err)
			}
		}()
		registers := &vmm.ARM64State{}
		for pb.Next() {
			registers.PC = codeVA
			for {
				exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
					AddressSpaceID: spaceID,
					Registers:      registers,
				})
				if err == nil && exit.Reason == vmm.ExitInterrupted {
					continue
				}
				if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
					b.Errorf("parallel application exit=%+v pc=%#x err=%v", exit, registers.PC, err)
					return
				}
				break
			}
		}
	})
}
