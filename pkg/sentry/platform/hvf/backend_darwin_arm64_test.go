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

func TestRing0EngineApplicationRoundTrip(t *testing.T) {
	hostPID := os.Getpid()
	if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
		t.Skipf("Darwin direct getpid SVC returned %d, want %d", got, hostPID)
	}
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
	exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
		t.Fatalf("first ring0 SVC-gate exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}
	// Exercise the resident continuation directly. This second application
	// entry must call the monitor at EL1; attempting the native host trampoline
	// here would create an invalid nested Hypervisor.framework entry.
	registers.PC = codeVA
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 || registers.PC != codeVA+8 {
		t.Fatalf("resident ring0 SVC-gate exit=%+v x8=%d pc=%#x", exit, registers.X[8], registers.PC)
	}
	// RunApplication returned directly to the Sentry continuation at EL1. A
	// direct Darwin SVC must be replayed at host EL0, with its result and return
	// address preserved.
	if got := int(ring0asm.HostSVCGetpid()); got != hostPID {
		t.Fatalf("host-service SVC getpid=%d, want %d", got, hostPID)
	}
	registers.PC = codeVA
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: spaceID,
		Registers:      registers,
	})
	// RunApplication returned directly to the Sentry continuation at EL1. The
	// Darwin libc call below enters an executable that is intentionally NX in
	// the Sentry identity mirror. Its current-EL instruction abort must redpill,
	// execute the service at host EL0, and preserve the calling continuation.
	if got := os.Getpid(); got != hostPID {
		t.Fatalf("host-service gate getpid=%d, want %d", got, hostPID)
	}
	// The host-service gate already redpilled this continuation. RunMonitor's
	// EnsureHost is consequently a no-op before its dedicated service vCPU runs.
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
		registers := &vmm.ARM64State{PC: codeVA}
		registers.X[0] = 1
		registers.X[1] = codeVA + 0x100
		exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{
			AddressSpaceID: spaceID,
			Registers:      registers,
		})
		result <- applicationResult{exit: exit, err: err}
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
	runEngineMonitor(t, engine, 1, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: 1})
	for page := uint64(0); page < 4; page++ {
		va := uint64(0x400000) + page*vmm.LogicalPageSize
		runEngineMonitor(t, engine, 2+page, monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: 1,
			Operations: []monitorabi.Operation{{
				Type:   monitorabi.OperationMap,
				Access: monitorabi.AccessRead | monitorabi.AccessExecute,
				VA:     va,
				GPA:    hvf.GuestRAMIPAStart + page*vmm.LogicalPageSize,
				Length: vmm.LogicalPageSize,
			}},
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
	// either masked EL1 application entry or unmasked EL0 execution; both must
	// return a coherent interrupted state without reading canceled CPSR through
	// HVF, and the vCPU must remain usable afterward.
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
	interruptErr := make(chan error, 1)
	go func() {
		time.Sleep(time.Millisecond)
		engine.InterruptApplications()
		interruptErr <- nil
	}()
	registers = &vmm.ARM64State{PC: loopVA}
	exit, err = engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
	if interruptRequestErr := <-interruptErr; interruptRequestErr != nil {
		t.Fatalf("RequestExit: %v", interruptRequestErr)
	}
	if err != nil || exit.Reason != vmm.ExitInterrupted || registers.PC != loopVA || registers.PState != 0 {
		t.Fatalf("interrupted loop exit=%+v registers=%+v err=%v", exit, registers, err)
	}
}

type tlbTortureJob struct {
	addressSpace uint64
	va           uint64
	wantSyscall  uint64
	done         chan<- error
}

func startTLBTortureWorker(engine vmplatform.VMEngine) chan<- tlbTortureJob {
	jobs := make(chan tlbTortureJob)
	go func() {
		for job := range jobs {
			registers := &vmm.ARM64State{PC: job.va}
			exit, err := engine.RunApplication(context.Background(), vmm.ApplicationState{AddressSpaceID: job.addressSpace, Registers: registers})
			if err == nil && (exit.Reason != vmm.ExitSyscall || registers.X[8] != job.wantSyscall || registers.PC != job.va+8) {
				err = fmt.Errorf("exit=%+v x8=%d pc=%#x, want syscall=%d pc=%#x", exit, registers.X[8], registers.PC, job.wantSyscall, job.va+8)
			}
			job.done <- err
		}
	}()
	return jobs
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
	workers := []chan<- tlbTortureJob{
		startTLBTortureWorker(engine),
		startTLBTortureWorker(engine),
	}
	defer func() {
		for _, worker := range workers {
			close(worker)
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
			worker <- tlbTortureJob{addressSpace: spaceID, va: va, wantSyscall: want, done: done}
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
