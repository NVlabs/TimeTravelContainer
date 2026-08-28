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
	"sync/atomic"
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

func TestHostMirrorShadowsOnlyImmutableMainImageData(t *testing.T) {
	tests := []struct {
		name     string
		identity ring0Stage2Identity
		want     bool
	}{
		{
			name:     "main image read-only",
			identity: ring0Stage2Identity{mainImage: true, access: vmm.Stage2Read | vmm.Stage2Execute},
			want:     true,
		},
		{
			name:     "main image writable",
			identity: ring0Stage2Identity{mainImage: true, access: vmm.Stage2Read | vmm.Stage2Write},
		},
		{
			name: "read-only data plane",
			// GuestRAM is commonly mapped read-only into Sentry while EL0 retains
			// a writable mapping to the same frame. It must remain coherent.
			identity: ring0Stage2Identity{access: vmm.Stage2Read},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ring0HostMirrorNeedsShadow(test.identity); got != test.want {
				t.Fatalf("ring0HostMirrorNeedsShadow(%+v) = %t, want %t", test.identity, got, test.want)
			}
		})
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
	if region.userTag != 240 {
		t.Fatalf("heap region at %#x has Darwin VM tag %d, want TTC runtime tag 240", target, region.userTag)
	}
	runtime.KeepAlive(allocation)

	if region, ok, err := ring0HostRegionAt(0); err != nil || ok {
		t.Fatalf("Darwin VM region at zero = %+v, ok=%t, err=%v; want absent", region, ok, err)
	}
}

func TestHostSlabCanMirrorRequiresOwnedLifetime(t *testing.T) {
	engine := &ring0Engine{vm: &hvfVM{}}
	tests := []struct {
		name   string
		region ring0HostRegion
		want   bool
	}{
		{name: "main image", region: ring0HostRegion{mainImage: true, externalPager: true}, want: true},
		{name: "runtime arena", region: ring0HostRegion{userTag: 240}, want: true},
		{name: "bridge", region: ring0HostRegion{userTag: 241}, want: true},
		{name: "VM allocation", region: ring0HostRegion{userTag: 242}, want: true},
		{name: "pthread stack", region: ring0HostRegion{userTag: 30}, want: true},
		{name: "untagged anonymous", region: ring0HostRegion{}, want: false},
		{name: "file-backed runtime tag", region: ring0HostRegion{userTag: 240, externalPager: true}, want: false},
		{name: "unowned application tag", region: ring0HostRegion{userTag: 239}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := engine.hostSlabCanMirror(test.region, 0x10000); got != test.want {
				t.Fatalf("hostSlabCanMirror(%+v) = %t, want %t", test.region, got, test.want)
			}
		})
	}
}

func TestFlushHostExecutionCacheReleasesAllocatorMappings(t *testing.T) {
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

	allocation := make([]byte, vmm.HostSlabSize)
	target := uintptr(unsafe.Pointer(&allocation[len(allocation)/2]))
	engine.requestHostMirror(target)
	if err := engine.extendHostMirror(context.Background()); err != nil {
		t.Fatal(err)
	}
	foundAllocatorMapping := false
	for _, slab := range engine.stage2Slabs {
		if !slab.mainImage && slab.access&vmm.Stage2Write != 0 && slab.lease != nil {
			foundAllocatorMapping = true
			break
		}
	}
	if !foundAllocatorMapping {
		t.Fatal("Sentry allocator working set contains no writable stage-2 lease")
	}
	if err := engine.FlushHostExecutionCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	for address, slab := range engine.stage2Slabs {
		if !slab.mainImage {
			t.Fatalf("allocator-owned Sentry slab %#x remained after execution-cache flush", address)
		}
	}
	runtime.KeepAlive(allocation)
}

func TestHostMemoryDecommitRetiresSentryMirror(t *testing.T) {
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

	allocator := backend.HostMemory()
	host, err := allocator.Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = allocator.Release(host, vmm.HostSlabSize)
		}
	}()
	engine.requestHostMirror(host)
	if err := engine.extendHostMirror(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.stage2Slabs[host]; !ok {
		t.Fatalf("host slab %#x was not imported into the Sentry mirror", host)
	}
	for va := uint64(host); va < uint64(host)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
		if _, ok := engine.stage1Access[va]; !ok {
			t.Fatalf("host page %#x has no Sentry stage-1 mapping", va)
		}
	}

	if err := allocator.Decommit(host, vmm.HostSlabSize); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.stage2Slabs[host]; ok {
		t.Fatalf("host slab %#x remained mirrored after decommit", host)
	}
	for va := uint64(host); va < uint64(host)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
		if _, ok := engine.stage1Access[va]; ok {
			t.Fatalf("host page %#x remained in Sentry stage-1 after decommit", va)
		}
	}
	if err := allocator.Release(host, vmm.HostSlabSize); err != nil {
		t.Fatal(err)
	}
	released = true
}

func TestStage1FaultAccess(t *testing.T) {
	const (
		instructionAbort = uint64(0x20) << 26
		dataAbort        = uint64(0x24) << 26
		write            = uint64(1) << 6
		cacheMaintenance = uint64(1) << 8
	)
	tests := []struct {
		name     string
		syndrome uint64
		want     vmm.Stage2Access
	}{
		{name: "instruction", syndrome: instructionAbort, want: vmm.Stage2Execute},
		{name: "load", syndrome: dataAbort, want: vmm.Stage2Read},
		{name: "store", syndrome: dataAbort | write, want: vmm.Stage2Write},
		{name: "cache maintenance", syndrome: dataAbort | write | cacheMaintenance, want: vmm.Stage2Read},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stage1FaultAccess(test.syndrome); got != test.want {
				t.Fatalf("stage1FaultAccess(%#x) = %#x, want %#x", test.syndrome, got, test.want)
			}
		})
	}
}

func TestSwitchApplicationGuestTrapAddress(t *testing.T) {
	if got, want := ring0asm.SwitchApplicationGuestTrapAddress(), ring0asm.SwitchApplicationGuestAddress()+4; got != want {
		t.Fatalf("guest-switch trap address = %#x, want %#x", got, want)
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

func TestTranslationMutationQuiescesCPUBoundApplication(t *testing.T) {
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
		t.Fatalf("translation mutation did not quiesce CPU-bound application: %v", err)
	}
	response, err := monitorabi.UnmarshalResponse(responseBytes)
	if err != nil || response.Status != monitorabi.StatusOK {
		t.Fatalf("monitor response=%+v err=%v", response, err)
	}
	application := <-applicationDone
	if application.err != nil || application.exit.Reason != vmm.ExitFault ||
		application.exit.Address != loopVA || application.exit.Access != vmm.Stage2Execute {
		t.Fatalf("CPU-bound application after execute permission removal exit=%+v err=%v", application.exit, application.err)
	}
}

func TestRuntimeMutationRebindsTrackedStage2WhileGuestRuns(t *testing.T) {
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
	defer engine.Close()

	trackedHost, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(trackedHost, vmm.HostSlabSize)
	tracked := unsafe.Slice((*byte)(unsafe.Pointer(trackedHost)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint64(tracked, 0x1122334455667788)
	trackedGPA := GuestRAMIPAStart - vmm.HostSlabSize
	if err := vm.mapRing0MirrorStage2Source(uintptr(trackedGPA), trackedHost,
		vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write); err != nil {
		t.Fatal(err)
	}
	defer vm.unmapRing0MirrorStage2GPA(trackedGPA, vmm.HostSlabSize)

	codeHost, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(codeHost, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, codeHost, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	code := unsafe.Slice((*byte)(unsafe.Pointer(codeHost)), int(vmm.HostSlabSize))
	// reader: ldr x1, literal; ldr x0, [x1]; movz x8, #172; svc #0.
	binary.LittleEndian.PutUint32(code[0:], 0x58000081)
	binary.LittleEndian.PutUint32(code[4:], 0xf9400020)
	binary.LittleEndian.PutUint32(code[8:], 0xd2801588)
	binary.LittleEndian.PutUint32(code[12:], 0xd4000001)
	// spinner: b .
	binary.LittleEndian.PutUint32(code[vmm.LogicalPageSize:], 0x14000000)

	const (
		spaceID = uint64(43)
		codeVA  = uint64(0xb00000)
		dataVA  = uint64(0xc00000)
		loopVA  = uint64(0xb01000)
	)
	binary.LittleEndian.PutUint64(code[16:], dataVA)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{
			{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize},
			{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: loopVA, GPA: GuestRAMIPAStart + vmm.LogicalPageSize, Length: vmm.LogicalPageSize},
			{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessWrite, VA: dataVA, GPA: trackedGPA, Length: vmm.LogicalPageSize},
		},
	})

	readValue := func() uint64 {
		engine.BeginApplicationResidency()
		defer func() {
			if err := engine.EndApplicationResidency(); err != nil {
				t.Errorf("ending reader residency: %v", err)
			}
		}()
		registers := &vmm.ARM64State{PC: codeVA}
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: spaceID, Registers: registers})
		if err != nil || exit.Reason != vmm.ExitSyscall {
			t.Fatalf("reader exit=%+v err=%v", exit, err)
		}
		return registers.X[0]
	}
	if got := readValue(); got != 0x1122334455667788 {
		t.Fatalf("initial guest value=%#x", got)
	}

	type loopResult struct {
		exit vmm.Exit
		err  error
	}
	loopDone := make(chan loopResult, 1)
	go func() {
		engine.BeginApplicationResidency()
		registers := &vmm.ARM64State{PC: loopVA}
		exit, runErr := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: spaceID, Registers: registers})
		loopDone <- loopResult{exit: exit, err: errors.Join(runErr, engine.EndApplicationResidency())}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cpus := engine.pinApplicationCPUs()
		inGuest := false
		for _, cpu := range cpus {
			inGuest = inGuest || cpu.bridge.inGuest()
		}
		engine.unpinApplicationCPUs(cpus)
		if inGuest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spinner did not enter guest")
		}
		time.Sleep(100 * time.Microsecond)
	}

	if err := replaceRing0TrackedHostMapping(trackedHost, vmm.HostSlabSize, 0x8877665544332211); err != nil {
		t.Fatal(err)
	}
	select {
	case loop := <-loopDone:
		t.Fatalf("runtime mapping rendezvous leaked an application exit: exit=%+v err=%v", loop.exit, loop.err)
	default:
	}
	engine.InterruptApplications()
	var loop loopResult
	select {
	case loop = <-loopDone:
	case <-time.After(3 * time.Second):
		cpus := engine.pinApplicationCPUs()
		detail := "no application vCPU"
		if len(cpus) != 0 {
			cpu := cpus[0]
			state := unsafe.Pointer(cpu.vcpu.stateHost)
			detail = fmt.Sprintf("vCPU=%d active=%t entry=%t in_guest=%t cancel=%d exit=%d pending_interrupt_exit=%d external=%d cancellations=%d cancel_pc=%#x cancel_pstate=%#x",
				cpu.vcpu.id, cpu.applicationActive.Load(), cpu.entryActive.Load(),
				cpu.bridge.inGuest(),
				atomic.LoadUint64((*uint64)(unsafe.Add(state, stateCancelRequested))),
				atomic.LoadUint64((*uint64)(unsafe.Add(state, stateExitKind))),
				atomic.LoadUint64((*uint64)(unsafe.Add(state, statePendingInterruptExitKind))),
				atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(cpu.bridge.pointer), ring0BridgeExternalInterruptOffset))),
				cpu.bridge.cancelCount(), cpu.bridge.lastCancelPC(), cpu.bridge.lastCancelPState())
		}
		engine.unpinApplicationCPUs(cpus)
		t.Fatalf("spinner did not stop after explicit application interrupt: %s", detail)
	}
	if loop.err != nil || loop.exit.Reason != vmm.ExitInterrupted {
		t.Fatalf("spinner exit=%+v err=%v", loop.exit, loop.err)
	}
	if got := readValue(); got != 0x8877665544332211 {
		t.Fatalf("guest value after MAP_FIXED=%#x, want %#x", got, uint64(0x8877665544332211))
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
					errs <- fmt.Errorf("worker %d iteration %d application exit=%+v registers={x8=%d pc=%#x} code=%#x err=%v", worker, iteration, exit, registers.X[8], registers.PC, binary.LittleEndian.Uint64(memory[:8]), err)
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

// TestApplicationSyscallBurstStaysInGuest makes the shared-Sentry contract
// quantitative. Linux SVC exits return directly to the waiting Go
// continuation at EL1; only the first entry may cross the Darwin/HVF boundary.
func TestApplicationSyscallBurstStaysInGuest(t *testing.T) {
	hostPID := os.Getpid()
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
	engine.BeginApplicationResidency()
	defer func() {
		if err := engine.EndApplicationResidency(); err != nil {
			t.Errorf("ending application residency: %v", err)
		}
	}()

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
		spaceID = uint64(94)
		codeVA  = uint64(0xb00000)
		calls   = 4096
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})
	registers := &vmm.ARM64State{}
	for i := 0; i < calls; i++ {
		registers.PC = codeVA
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
			t.Fatalf("application call %d exit=%+v pc=%#x err=%v", i, exit, registers.PC, err)
		}
	}
	value, ok := engine.cpusByTID.Load(hosttid.Current())
	if !ok {
		t.Fatal("resident application vCPU is absent")
	}
	stats := value.(*ring0ApplicationCPU).bridge.stats()
	bridgeAddress := uintptr(unsafe.Pointer(value.(*ring0ApplicationCPU).bridge.pointer))
	bridgeRegion, present, err := ring0HostRegionAt(bridgeAddress)
	if err != nil || !present || bridgeRegion.userTag != 241 {
		t.Fatalf("bridge region at %#x = %+v, present=%t, err=%v; want TTC bridge tag 241", bridgeAddress, bridgeRegion, present, err)
	}
	// Runtime scheduling, stack growth, or allocator maintenance may require a
	// bounded Darwin service while the benchmark runs. What must never return is
	// the old one-host-exit-per-application-syscall behavior.
	if stats.hostReturns > calls/128 {
		region, present, regionErr := ring0HostRegionAt(ring0asm.CurrentSP())
		state := unsafe.Slice((*byte)(unsafe.Pointer(value.(*ring0ApplicationCPU).vcpu.stateHost)), vmm.HostSlabSize)
		far := binary.LittleEndian.Uint64(state[stateHostGateFAR:])
		faultRegion, faultPresent, faultErr := ring0HostRegionAt(uintptr(far))
		t.Fatalf("host returns scaled with %d guest syscalls: stats=%+v stack-region=%+v present=%t err=%v last-far=%#x fault-region=%+v present=%t err=%v", calls, stats, region, present, regionErr, far, faultRegion, faultPresent, faultErr)
	}
	// A final-iteration demand import may have returned this continuation to
	// Darwin after the last application exit. Re-enter once so the explicit SVC
	// below measures the guest-to-host service boundary, not host-native SVC.
	if ring0asm.CurrentEL() == 0 {
		registers.PC = codeVA
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
			t.Fatalf("demand-import re-entry exit=%+v pc=%#x err=%v", exit, registers.PC, err)
		}
		stats = value.(*ring0ApplicationCPU).bridge.stats()
	}
	beforeHostService := stats
	if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
		t.Fatalf("host service getpid=%d, want %d", got, hostPID)
	}
	stats = value.(*ring0ApplicationCPU).bridge.stats()
	if stats.guestEntries != beforeHostService.guestEntries ||
		stats.hostGates != beforeHostService.hostGates+1 ||
		stats.redpills != beforeHostService.redpills ||
		stats.hostReturns != beforeHostService.hostReturns+1 {
		t.Fatalf("explicit host service did not cross exactly one host gate: stats=%+v", stats)
	}
	beforeReentry := stats
	registers.PC = codeVA
	exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+8 {
		t.Fatalf("post-host-service application exit=%+v pc=%#x err=%v", exit, registers.PC, err)
	}
	stats = value.(*ring0ApplicationCPU).bridge.stats()
	if stats.guestEntries != beforeReentry.guestEntries+1 ||
		stats.hostGates != beforeReentry.hostGates ||
		stats.redpills != beforeReentry.redpills ||
		stats.hostReturns != beforeReentry.hostReturns {
		t.Fatalf("post-host-service re-entry did not reuse the residency: stats=%+v", stats)
	}
}

// TestCPUboundApplicationDoesNotStarveGoScheduler verifies the execution
// contract that replaces runtime.entersyscall for shared EL1 continuations.
// With one P, only the native HVF preemption tick can return a CPU-bound Linux
// task to a Go safepoint and allow this test goroutine to run again.
func TestCPUboundApplicationDoesNotStarveGoScheduler(t *testing.T) {
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
		spaceID = uint64(95)
		codeVA  = uint64(0xc00000)
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})

	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		engine.BeginApplicationResidency()
		close(entered)
		registers := &vmm.ARM64State{PC: codeVA}
		exit, runErr := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		endErr := engine.EndApplicationResidency()
		if runErr == nil && exit.Reason != vmm.ExitInterrupted {
			runErr = fmt.Errorf("CPU-bound application exit=%+v, want interrupted", exit)
		}
		done <- errors.Join(runErr, endErr)
	}()
	<-entered
	// Closing entered makes this goroutine runnable without yielding the worker.
	// Reaching this point therefore proves that native preemption returned the
	// worker to a runtime safepoint while it owned the only P.
	engine.InterruptApplications()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativePreemptionPreservesApplicationRegisters(t *testing.T) {
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
	engine.BeginApplicationResidency()
	defer func() {
		if err := engine.EndApplicationResidency(); err != nil {
			t.Errorf("ending application residency: %v", err)
		}
	}()

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
		spaceID    = uint64(96)
		codeVA     = uint64(0xd00000)
		iterations = 64
	)
	runRing0InternalMonitor(t, engine, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	runRing0InternalMonitor(t, engine, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})

	for iteration := 0; iteration < iterations; iteration++ {
		registers := &vmm.ARM64State{
			PC:     codeVA,
			SP:     0x70000000 + uint64(iteration)*0x1000,
			PState: uint64(iteration&0xf) << 28,
			TPIDR:  0x12345000 + uint64(iteration),
		}
		for index := range registers.X {
			registers.X[index] = uint64(iteration+1)<<32 | uint64(index+1)
		}
		// FPSR/FPCR occupy the first eight bytes and mask architecturally
		// reserved bits on restore; compare the SIMD register payload exactly.
		for index := 8; index < len(registers.FP); index++ {
			registers.FP[index] = byte(iteration*3 + index)
		}
		want := *registers
		interrupted := make(chan struct{})
		go func() {
			time.Sleep(25 * time.Millisecond)
			engine.InterruptApplications()
			close(interrupted)
		}()
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		<-interrupted
		if err != nil || exit.Reason != vmm.ExitInterrupted || *registers != want {
			t.Fatalf("preempted application iteration %d: exit=%+v registers=%+v want=%+v err=%v", iteration, exit, registers, &want, err)
		}
	}
}

func BenchmarkRing0ApplicationSwitch(b *testing.B) {
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
	if value, ok := engine.cpusByTID.Load(hosttid.Current()); ok {
		stats := value.(*ring0ApplicationCPU).bridge.stats()
		b.ReportMetric(float64(stats.guestEntries)/float64(b.N), "guest-entries/op")
		b.ReportMetric(float64(stats.hostGates)/float64(b.N), "host-gates/op")
		b.ReportMetric(float64(stats.redpills)/float64(b.N), "redpills/op")
		b.ReportMetric(float64(stats.hostReturns)/float64(b.N), "host-returns/op")
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

// BenchmarkRing0ApplicationSwitchParallel measures the backend-owned world
// switch without Sentry syscall work while every host application CPU is busy.
// Keep this separate from the serial benchmark: a regression here identifies
// Hypervisor.framework/vCPU coordination rather than Linux syscall contention.
func BenchmarkRing0ApplicationSwitchParallel(b *testing.B) {
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
