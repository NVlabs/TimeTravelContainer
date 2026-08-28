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

//go:build darwin && arm64

package hvf_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	vmarm64 "gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/arm64"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func TestRing0ContinuationTransfer(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()
	if err := hvf.ValidateRing0Continuation(vm); err != nil {
		t.Fatal(err)
	}
}

func runEngineMonitor(t *testing.T, engine vmplatform.VMEngine, sequence uint64, command monitorabi.Command) {
	t.Helper()
	command.Sequence = sequence
	encoded, err := monitorabi.MarshalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, err := engine.RunMonitor(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	response, err := monitorabi.UnmarshalResponse(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if response.Sequence != sequence || response.Status != monitorabi.StatusOK {
		t.Fatalf("monitor response = %+v", response)
	}
}

func beginEngineResidency(t *testing.T, engine vmplatform.VMEngine) func() {
	t.Helper()
	resident, ok := engine.(vmplatform.ApplicationResidencyEngine)
	if !ok {
		t.Fatalf("engine %T does not implement application residency", engine)
	}
	resident.BeginApplicationResidency()
	return func() {
		t.Helper()
		if err := resident.EndApplicationResidency(); err != nil {
			t.Errorf("ending application residency: %v", err)
		}
	}
}

func TestRing0EngineApplicationRoundTrip(t *testing.T) {
	hostPID := os.Getpid()
	if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
		t.Skipf("Darwin direct getpid SVC returned %d, want %d", got, hostPID)
	}
	// Allocate the Sentry clock object before the engine takes its initial host
	// VM snapshot, and run the Darwin runtime clock path once from this stack
	// frame. The assertion below is about the EL1 clock service itself, not the
	// one-time demand import of a Go arena created after bootstrap.
	calibratedClock := sentrytime.NewCalibratedClock(sentrytime.Monotonic)
	_ = time.Now()
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()
	engine, err := hvf.NewVMEngine(vm, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer beginEngineResidency(t, engine)()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(hvf.GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(hvf.GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172 (getpid)
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(10)
		codeVA  = uint64(0x600000)
	)
	runEngineMonitor(t, engine, 1, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: spaceID,
	})
	runEngineMonitor(t, engine, 2, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     codeVA,
			GPA:    hvf.GuestRAMIPAStart,
			Length: vmm.LogicalPageSize,
		}},
	})
	// Encode the host-return command before entering EL1 so this test isolates
	// execution rather than depending on a guest-side Go heap growth.
	hostCommand, err := monitorabi.MarshalCommand(monitorabi.Command{
		Sequence:       3,
		Opcode:         monitorabi.OpcodeActivate,
		AddressSpaceID: spaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	registers := &vmm.ARM64State{PC: codeVA}
	hostCounterBefore := sentrytime.Rdtsc()
	hostWallBefore := time.Now()
	exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
		t.Fatalf("first ring0 SVC exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}
	if got := ring0asm.CurrentEL(); got != 1 {
		t.Fatalf("Sentry syscall continuation resumed at EL%d, want EL1", got)
	}
	guestCounter := sentrytime.Rdtsc()
	if _, err := calibratedClock.GetTime(); err != nil {
		t.Fatalf("reading calibrated clock at EL1: %v", err)
	}
	if got := ring0asm.CurrentEL(); got != 1 {
		t.Fatalf("calibrated EL1 clock read host-gated the Sentry continuation to EL%d", got)
	}
	guestWall := time.Now()
	if got := ring0asm.CurrentEL(); got != 1 {
		t.Fatalf("Darwin runtime wall clock host-gated the Sentry continuation to EL%d", got)
	}
	if delta := guestWall.Sub(hostWallBefore); delta < 0 || delta > 5*time.Second {
		t.Fatalf("EL1 Darwin runtime wall clock advanced by %s, want [0, 5s]", delta)
	}
	// Exercise the resident owner-affine vCPU again. The first application exit
	// resumed this exact Go continuation at EL1, so the second entry does not
	// cross Hypervisor.framework.
	registers.PC = codeVA
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
		t.Fatalf("resident ring0 EL1 SVC exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}
	// A Darwin service redpills only the current Sentry continuation while the
	// owner-affine vCPU remains reusable by the next application entry.
	if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
		t.Fatalf("host-service SVC getpid=%d, want %d", got, hostPID)
	}
	hostCounterAfter := sentrytime.Rdtsc()
	if guestCounter < hostCounterBefore || guestCounter > hostCounterAfter {
		t.Fatalf("guest CNTVCT=%d outside host counter interval [%d, %d]", guestCounter, hostCounterBefore, hostCounterAfter)
	}
	if got := ring0asm.CurrentEL(); got != 0 {
		t.Fatalf("host-service continuation resumed at EL%d, want Darwin EL0", got)
	}
	registers.PC = codeVA
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	// The regular libc path likewise executes at host EL0 without ending the
	// application residency.
	if got := os.Getpid(); got != hostPID {
		t.Fatalf("host-service gate getpid=%d, want %d", got, hostPID)
	}
	// RunMonitor's EnsureHost is a no-op here before its dedicated service vCPU
	// runs.
	responseBytes, hostErr := engine.RunMonitor(context.Background(), hostCommand)
	if hostErr != nil {
		t.Fatalf("returning ring0 continuation to host: %v", hostErr)
	}
	if response, responseErr := monitorabi.UnmarshalResponse(responseBytes); responseErr != nil || response.Status != monitorabi.StatusOK {
		t.Fatalf("host-return monitor response=%+v err=%v", response, responseErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
		t.Fatalf("ring0 instruction-gate exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}
}

func TestRing0EngineForwardsBreakpointAndSoftwareStep(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()
	engine, err := hvf.NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer beginEngineResidency(t, engine)()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(hvf.GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(hvf.GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd4200000)  // brk #0
	binary.LittleEndian.PutUint32(memory[4:], 0xd2802460)  // movz x0, #0x123
	binary.LittleEndian.PutUint32(memory[8:], 0xd2801588)  // movz x8, #172 (getpid)
	binary.LittleEndian.PutUint32(memory[12:], 0xd4000001) // svc #0

	const (
		spaceID = uint64(11)
		codeVA  = uint64(0x610000)
	)
	runEngineMonitor(t, engine, 1, monitorabi.Command{
		Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID,
	})
	runEngineMonitor(t, engine, 2, monitorabi.Command{
		Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA: codeVA, GPA: hvf.GuestRAMIPAStart, Length: vmm.LogicalPageSize,
		}},
	})

	registers := &vmm.ARM64State{PC: codeVA}
	exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID, Registers: registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ec := exit.Syndrome >> 26 & 0x3f; exit.Reason != vmm.ExitException || ec != 0x3c || registers.PC != codeVA {
		t.Fatalf("BRK exit=%+v pc=%#x, want exception EC=0x3c pc=%#x", exit, registers.PC, codeVA)
	}

	// This is the transition GDB uses after restoring the instruction hidden
	// by its software breakpoint. Exactly one instruction must execute before
	// an EC 0x32 software-step exception returns control to the tracer.
	registers.PC = codeVA + 4
	registers.PState |= arch.ARMTrapFlag
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID, Registers: registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ec := exit.Syndrome >> 26 & 0x3f; exit.Reason != vmm.ExitException || ec != 0x32 || registers.PC != codeVA+8 || registers.X[0] != 0x123 {
		t.Fatalf("single-step exit=%+v pc=%#x x0=%#x, want exception EC=0x32 pc=%#x x0=0x123", exit, registers.PC, registers.X[0], codeVA+8)
	}

	registers.PState &^= arch.ARMTrapFlag
	registers.PC = codeVA + 8
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID, Registers: registers,
	})
	if err != nil || exit.Reason != vmm.ExitSyscall || registers.PC != codeVA+16 || registers.X[8] != 172 {
		t.Fatalf("post-step syscall exit=%+v pc=%#x x8=%d err=%v", exit, registers.PC, registers.X[8], err)
	}
}

func TestRing0EngineCloseRetiresOwnerVCPU(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()
	engine, err := hvf.NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(hvf.GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(hvf.GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xf9000020) // str x0, [x1]
	binary.LittleEndian.PutUint32(memory[4:], 0x14000000) // b .

	const (
		spaceID = uint64(20)
		codeVA  = uint64(0x700000)
	)
	runEngineMonitor(t, engine, 1, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: spaceID,
	})
	runEngineMonitor(t, engine, 2, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: spaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessWrite | monitorabi.AccessExecute,
			VA:     codeVA,
			GPA:    hvf.GuestRAMIPAStart,
			Length: vmm.LogicalPageSize,
		}},
	})

	type applicationResult struct {
		exit vmm.Exit
		err  error
	}
	result := make(chan applicationResult, 1)
	go func() {
		resident := engine.(vmplatform.ApplicationResidencyEngine)
		resident.BeginApplicationResidency()
		registers := &vmm.ARM64State{PC: codeVA}
		registers.X[0] = 1
		registers.X[1] = codeVA + 0x100
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		endErr := resident.EndApplicationResidency()
		result <- applicationResult{exit: exit, err: errors.Join(err, endErr)}
	}()
	// Wait until the application has entered its non-terminating EL0 loop.
	// Close must
	// first convert cancellation into an architected IRQ, then redpill the
	// restored Sentry continuation so destruction occurs on the owner thread.
	entered := (*uint64)(unsafe.Pointer(host + 0x100))
	entryDeadline := time.Now().Add(5 * time.Second)
	for atomic.LoadUint64(entered) == 0 {
		if time.Now().After(entryDeadline) {
			t.Fatal("application did not enter EL0")
		}
		runtime.Gosched()
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.exit.Reason != vmm.ExitInterrupted {
			t.Fatalf("application after engine close: exit=%+v err=%v", got.exit, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("application continuation did not return during engine close")
	}
	if err := vm.Close(); err != nil {
		t.Fatalf("closing VM after owner-vCPU retirement: %v", err)
	}
}

func TestMonitorExecutesFourLogicalPagesInOneHostSlab(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()

	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(hvf.GuestRAMIPAStart, host, vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(hvf.GuestRAMIPAStart, vmm.HostSlabSize)

	// movz x8, #172 (getpid); svc #0. The sentry receives the syscall exit
	// before emulating it, so this validates EL0 entry and exception forwarding.
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	for offset := 0; offset < int(vmm.HostSlabSize); offset += int(vmm.LogicalPageSize) {
		binary.LittleEndian.PutUint32(memory[offset:], 0xd2801588)
		binary.LittleEndian.PutUint32(memory[offset+4:], 0xd4000001)
	}

	engine, err := hvf.NewVMEngine(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer beginEngineResidency(t, engine)()
	runEngineMonitor(t, engine, 1, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: 1})
	for page := uint64(0); page < 4; page++ {
		va := uint64(0x400000) + page*vmm.LogicalPageSize
		operations := []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     va,
			GPA:    hvf.GuestRAMIPAStart + page*vmm.LogicalPageSize,
			Length: vmm.LogicalPageSize,
		}}
		if page == 0 {
			// Exercise ordered command semantics and executable-cache resolution:
			// protect must consume the mapping planned earlier in this same batch.
			operations[0].Access = monitorabi.AccessRead | monitorabi.AccessWrite
			operations = append(operations, monitorabi.Operation{
				Type:   monitorabi.OperationProtect,
				Access: monitorabi.AccessRead | monitorabi.AccessExecute,
				VA:     va,
				Length: vmm.LogicalPageSize,
			})
		}
		runEngineMonitor(t, engine, 2+page, monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: 1,
			Operations:     operations,
		})
		registers := &vmm.ARM64State{PC: va}
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
		if err != nil {
			t.Fatalf("running logical page %d: %v", page, err)
		}
		if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != va+8 {
			t.Fatalf("logical page %d exit=%+v x8=%d pc=%#x", page, exit, registers.X[8], registers.PC)
		}
	}

	// Recycle an already executed page through writable and back to executable.
	// The monitor must synchronize the host-populated data with every vCPU's
	// instruction cache before acknowledging the executable mapping.
	runEngineMonitor(t, engine, 10, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: 1,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationProtect,
			Access: monitorabi.AccessRead | monitorabi.AccessWrite,
			VA:     0x400000,
			Length: vmm.LogicalPageSize,
		}},
	})
	binary.LittleEndian.PutUint32(memory, 0xd28015a8) // movz x8, #173 (getppid)
	runEngineMonitor(t, engine, 11, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: 1,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationProtect,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     0x400000,
			Length: vmm.LogicalPageSize,
		}},
	})
	registers := &vmm.ARM64State{PC: 0x400000}
	exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
	if err != nil {
		t.Fatalf("running rewritten executable page: %v", err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 173 || registers.PC != 0x400008 {
		t.Fatalf("rewritten executable exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}

	// Exercise the asynchronous-exit handoff repeatedly. RequestExit may catch
	// the shared EL1 entry trampoline, masked monitor entry, or unmasked EL0
	// execution. All must return a coherent interrupted state
	// without exporting an EL1-only trampoline to Darwin, and the engine must
	// remain usable afterward.
	const loopVA = uint64(0x403000)
	runEngineMonitor(t, engine, 12, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: 1,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationProtect,
			Access: monitorabi.AccessRead | monitorabi.AccessWrite,
			VA:     loopVA,
			Length: vmm.LogicalPageSize,
		}},
	})
	binary.LittleEndian.PutUint32(memory[3*vmm.LogicalPageSize:], 0x14000000) // b .
	runEngineMonitor(t, engine, 13, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: 1,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationProtect,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     loopVA,
			Length: vmm.LogicalPageSize,
		}},
	})
	for iteration := 0; iteration < 256; iteration++ {
		stopInterrupts := make(chan struct{})
		interruptDone := make(chan struct{})
		go func() {
			defer close(interruptDone)
			for {
				engine.InterruptApplications()
				select {
				case <-stopInterrupts:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
		registers = &vmm.ARM64State{PC: loopVA, SP: loopVA + 0x800, TPIDR: 0x12345000 + uint64(iteration)}
		for index := range registers.X {
			registers.X[index] = uint64(iteration+1)<<32 | uint64(index+1)
		}
		for index := 8; index < len(registers.FP); index++ {
			registers.FP[index] = byte(iteration + index)
		}
		wantRegisters := *registers
		exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
		close(stopInterrupts)
		<-interruptDone
		if err != nil || exit.Reason != vmm.ExitInterrupted || *registers != wantRegisters {
			t.Fatalf("interrupted loop iteration %d: exit=%+v registers=%+v err=%v", iteration, exit, registers, err)
		}
	}
}

type tlbTortureJob struct {
	addressSpace uint64
	va           uint64
	wantSyscall  uint64
	done         chan<- error
}

type tlbTortureWorker struct {
	jobs    chan tlbTortureJob
	stopped chan error
}

func startTLBTortureWorker(engine vmplatform.VMEngine, workerID int) tlbTortureWorker {
	jobs := make(chan tlbTortureJob)
	stopped := make(chan error, 1)
	go func() {
		resident := engine.(vmplatform.ApplicationResidencyEngine)
		resident.BeginApplicationResidency()
		for job := range jobs {
			registers := &vmm.ARM64State{PC: job.va}
			exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: job.addressSpace, Registers: registers})
			if err == nil && (exit.Reason != vmm.ExitSyscall || registers.X[8] != job.wantSyscall || registers.PC != job.va+8) {
				err = fmt.Errorf("worker=%d exit=%+v x8=%d pc=%#x, want syscall=%d pc=%#x", workerID, exit, registers.X[8], registers.PC, job.wantSyscall, job.va+8)
			}
			job.done <- err
		}
		stopped <- resident.EndApplicationResidency()
	}()
	return tlbTortureWorker{jobs: jobs, stopped: stopped}
}

func TestMonitorMultiVCPUTranslationReplacementTorture(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer vm.Close()
	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.HostMemory().Release(host, vmm.HostSlabSize)
	if err := vm.MapStage2(hvf.GuestRAMIPAStart, host, vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(hvf.GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	// Two immutable sentinels occupy independent 4K frames in one 16K slab.
	// Every observed syscall number therefore identifies the translated GPA.
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588)                     // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001)                     // svc #0
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize:], 0xd28015a8)   // movz x8, #173
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize+4:], 0xd4000001) // svc #0

	engine, err := hvf.NewVMEngine(vm, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const (
		spaceID = uint64(1)
		va      = uint64(0x500000)
	)
	sequence := uint64(1)
	runEngineMonitor(t, engine, sequence, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	sequence++
	workers := []tlbTortureWorker{
		startTLBTortureWorker(engine, 0),
		startTLBTortureWorker(engine, 1),
	}
	defer func() {
		for _, worker := range workers {
			close(worker.jobs)
		}
		for _, worker := range workers {
			if err := <-worker.stopped; err != nil {
				t.Errorf("stopping TLB torture worker: %v", err)
			}
		}
	}()
	done := make(chan error, len(workers))
	for iteration := 0; iteration < 300; iteration++ {
		page := uint64(iteration & 1)
		gpa := hvf.GuestRAMIPAStart + page*vmm.LogicalPageSize
		want := uint64(172) + page
		if iteration%17 == 0 && iteration != 0 {
			runEngineMonitor(t, engine, sequence, monitorabi.Command{
				Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
				Operations: []monitorabi.Operation{{Type: monitorabi.OperationUnmap, VA: va, Length: vmm.LogicalPageSize}},
			})
			sequence++
		}
		runEngineMonitor(t, engine, sequence, monitorabi.Command{
			Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
			Operations: []monitorabi.Operation{{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: va, GPA: gpa, Length: vmm.LogicalPageSize}},
		})
		sequence++
		if iteration%23 == 0 {
			runEngineMonitor(t, engine, sequence, monitorabi.Command{
				Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
				Operations: []monitorabi.Operation{{Type: monitorabi.OperationProtect, Access: monitorabi.AccessRead, VA: va, Length: vmm.LogicalPageSize}},
			})
			sequence++
			runEngineMonitor(t, engine, sequence, monitorabi.Command{
				Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
				Operations: []monitorabi.Operation{{Type: monitorabi.OperationProtect, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: va, Length: vmm.LogicalPageSize}},
			})
			sequence++
		}
		for _, worker := range workers {
			worker.jobs <- tlbTortureJob{addressSpace: spaceID, va: va, wantSyscall: want, done: done}
		}
		for range workers {
			if err := <-done; err != nil {
				t.Fatalf("iteration %d page %d observed a stale translation: %v", iteration, page, err)
			}
		}
	}
}

type compactionTestMemoryManager struct {
	sentryplatform.MemoryManager
	as sentryplatform.AddressSpace
}

func (m *compactionTestMemoryManager) AddressSpace() sentryplatform.AddressSpace { return m.as }

func TestRing0PlatformCompactionRelocatesExecutingFrame(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	p, err := vmplatform.New(vmplatform.Config{
		VM: vm, HostMemory: backend.HostMemory(), Architecture: vmarm64.Architecture{},
		VCPUs: 2, IPAStart: hvf.GuestRAMIPAStart, IPASize: 1 << 30,
		PlatformName: "hvf-test", VMEngineFactory: hvf.NewVMEngine,
	})
	if err != nil {
		_ = vm.Close()
		t.Fatal(err)
	}
	defer p.Close()

	type allocation struct {
		mf *pgalloc.MemoryFile
		fr memmap.FileRange
	}
	allocations := make([]allocation, 12)
	for i := range allocations {
		mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
			BackingProvider:         p.MemoryFileBackingProvider(),
			DelayedEviction:         pgalloc.DelayedEvictionDisabled,
			DisableMemoryAccounting: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
		if err != nil {
			mf.Destroy()
			t.Fatal(err)
		}
		allocations[i] = allocation{mf: mf, fr: fr}
	}
	// Allocation 8 begins in the third slab and is the executing frame that
	// compaction will move into a hole in one of the first two slabs.
	code := []byte{0x88, 0x15, 0x80, 0xd2, 0x01, 0x00, 0x00, 0xd4} // movz x8,#172; svc #0
	blocks, lease, err := memmap.MapInternalWithLease(allocations[8].mf, allocations[8].fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(code))); err != nil || n != uint64(len(code)) {
		lease.Release()
		t.Fatalf("writing code: n=%d err=%v", n, err)
	}
	lease.Release()
	for _, index := range []int{3, 7, 10, 11} {
		allocations[index].mf.DecRef(allocations[index].fr)
		allocations[index].mf.Destroy()
		allocations[index] = allocation{}
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.GuestRAM().Metrics().LogicalBytes != 8*hostarch.PageSize {
		if time.Now().After(deadline) {
			t.Fatalf("released pages were not reclaimed: %+v", p.GuestRAM().Metrics())
		}
		runtime.Gosched()
	}
	as, err := p.NewAddressSpace()
	if err != nil {
		t.Fatal(err)
	}
	defer as.Release()
	const va = hostarch.Addr(0x600000)
	if err := as.MapFile(va, allocations[8].mf, allocations[8].fr, hostarch.ReadExecute, false); err != nil {
		t.Fatal(err)
	}
	platformCtx := p.NewContext(gcontext.Background())
	defer platformCtx.Release()
	mm := &compactionTestMemoryManager{as: as}
	run := func(label string) {
		ac := arch.New(arch.ARM64)
		ac.SetIP(uintptr(va))
		if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil {
			t.Fatalf("%s execution: %v", label, err)
		}
		if got := ac.Regs.Regs[8]; got != 172 {
			t.Fatalf("%s syscall sentinel = %d, want 172", label, got)
		}
	}
	run("before compaction")
	result, err := p.CompactMemory(gcontext.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FramesMoved != 2 || result.SlabsReleased != 1 {
		t.Fatalf("compaction result = %+v, want two moved frames and one released slab", result)
	}
	run("after compaction")
	as.Unmap(va, hostarch.PageSize)
	for i := range allocations {
		if allocations[i].mf != nil {
			allocations[i].mf.DecRef(allocations[i].fr)
			allocations[i].mf.Destroy()
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for p.GuestRAM().Metrics().LogicalBytes != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("MemoryFiles were not released: %+v", p.GuestRAM().Metrics())
		}
		runtime.Gosched()
	}
}

func TestRing0PlatformEpochRolloverRebuildsExecutingAddressSpace(t *testing.T) {
	backend := hvf.NewBackend()
	newVM := backend.CreateVM
	p, err := vmplatform.New(vmplatform.Config{
		NewVM: newVM, HostMemory: backend.HostMemory(), Architecture: vmarm64.Architecture{},
		VCPUs: 2, IPAStart: hvf.GuestRAMIPAStart, IPASize: 4 * vmplatform.HostSlabSize,
		PlatformName: "hvf-rollover-test", VMEngineFactory: hvf.NewVMEngine,
	})
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	defer p.Close()

	newMemoryFile := func() *pgalloc.MemoryFile {
		mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
			BackingProvider:         p.MemoryFileBackingProvider(),
			DelayedEviction:         pgalloc.DelayedEvictionDisabled,
			DisableMemoryAccounting: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return mf
	}
	codeMF := newMemoryFile()
	defer codeMF.Destroy()
	codeRange, err := codeMF.Allocate(hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer codeMF.DecRef(codeRange)
	code := []byte{0x88, 0x15, 0x80, 0xd2, 0x01, 0x00, 0x00, 0xd4} // movz x8,#172; svc #0
	blocks, lease, err := memmap.MapInternalWithLease(codeMF, codeRange, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(code))); err != nil || n != uint64(len(code)) {
		lease.Release()
		t.Fatalf("writing code: n=%d err=%v", n, err)
	}
	lease.Release()

	as, err := p.NewAddressSpace()
	if err != nil {
		t.Fatal(err)
	}
	defer as.Release()
	const va = hostarch.Addr(0x700000)
	if err := as.MapFile(va, codeMF, codeRange, hostarch.ReadExecute, false); err != nil {
		t.Fatal(err)
	}
	platformCtx := p.NewContext(gcontext.Background())
	defer platformCtx.Release()
	mm := &compactionTestMemoryManager{as: as}
	run := func(label string) {
		ac := arch.New(arch.ARM64)
		ac.SetIP(uintptr(va))
		if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil {
			t.Fatalf("%s execution: %v", label, err)
		}
		if got := ac.Regs.Regs[8]; got != 172 {
			t.Fatalf("%s syscall sentinel = %d, want 172", label, got)
		}
	}
	run("before rollover")

	// Consume a second GPA slab and retire it while keeping the executable
	// frame mapped in the first slab. The next four-frame allocation needs a
	// new slab and therefore reaches the deliberately tiny 75% IPA threshold.
	consumeAndRelease := func() {
		mf := newMemoryFile()
		fr, err := mf.Allocate(4*hostarch.PageSize, pgalloc.AllocOpts{
			Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate,
		})
		if err != nil {
			mf.Destroy()
			t.Fatal(err)
		}
		mf.DecRef(fr)
		mf.Destroy()
	}
	consumeAndRelease()
	deadline := time.Now().Add(5 * time.Second)
	for p.GuestRAM().Metrics().LogicalBytes != hostarch.PageSize {
		if time.Now().After(deadline) {
			t.Fatalf("temporary frames were not reclaimed: %+v", p.GuestRAM().Metrics())
		}
		runtime.Gosched()
	}
	if got := p.GuestRAM().Metrics().IPABytesConsumed; got != 2*vmplatform.HostSlabSize {
		t.Fatalf("IPA consumption before rollover = %#x, want %#x", got, 2*vmplatform.HostSlabSize)
	}

	postRollover := newMemoryFile()
	postRange, err := postRollover.Allocate(4*hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		postRollover.Destroy()
		t.Fatal(err)
	}
	defer func() {
		postRollover.DecRef(postRange)
		postRollover.Destroy()
	}()
	if got := p.GuestRAM().Metrics().Epoch; got != 2 {
		t.Fatalf("VM epoch after threshold allocation = %d, want 2", got)
	}
	run("after rollover")
}

// BenchmarkRing0PlatformSwitch measures the complete backend-independent
// platform.Context boundary around the HVF application world switch. Keep it
// beside BenchmarkRing0ApplicationSwitch so changes can be attributed to
// vmplatform versus the host-specific execution engine.
func BenchmarkRing0PlatformSwitch(b *testing.B) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		b.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	p, err := vmplatform.New(vmplatform.Config{
		VM: vm, HostMemory: backend.HostMemory(), Architecture: vmarm64.Architecture{},
		VCPUs: 1, IPAStart: hvf.GuestRAMIPAStart, IPASize: 1 << 30,
		PlatformName: "hvf-benchmark", VMEngineFactory: hvf.NewVMEngine,
	})
	if err != nil {
		_ = vm.Close()
		b.Fatal(err)
	}
	defer p.Close()

	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		BackingProvider:         p.MemoryFileBackingProvider(),
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mf.Destroy()
	fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mf.DecRef(fr)
	blocks, lease, err := memmap.MapInternalWithLease(mf, fr, hostarch.Write)
	if err != nil {
		b.Fatal(err)
	}
	code := []byte{0x88, 0x15, 0x80, 0xd2, 0x01, 0x00, 0x00, 0xd4} // movz x8,#172; svc #0
	if n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(code))); err != nil || n != uint64(len(code)) {
		lease.Release()
		b.Fatalf("writing code: n=%d err=%v", n, err)
	}
	lease.Release()

	as, err := p.NewAddressSpace()
	if err != nil {
		b.Fatal(err)
	}
	defer as.Release()
	const codeVA = hostarch.Addr(0xb00000)
	if err := as.MapFile(codeVA, mf, fr, hostarch.ReadExecute, false); err != nil {
		b.Fatal(err)
	}
	platformCtx := p.NewContext(gcontext.Background())
	defer platformCtx.Release()
	mm := &compactionTestMemoryManager{as: as}
	ac := arch.New(arch.ARM64)
	run := func() {
		ac.SetIP(uintptr(codeVA))
		if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil {
			b.Fatal(err)
		}
	}
	run() // Establish owner-thread residency before timing.
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		run()
	}
	b.StopTimer()
	if got := ac.Regs.Regs[8]; got != 172 {
		b.Fatalf("syscall sentinel = %d, want 172", got)
	}
}
