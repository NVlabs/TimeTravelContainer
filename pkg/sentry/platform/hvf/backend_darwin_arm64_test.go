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

package hvf

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
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
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	vmarm64 "gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/arm64"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func runMonitorCommand(t *testing.T, vcpu vmm.VCPU, sequence uint64, command monitorabi.Command) {
	t.Helper()
	command.Sequence = sequence
	encoded, err := monitorabi.MarshalCommand(command)
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	responseBytes, err := vcpu.RunMonitor(context.Background(), encoded)
	if err != nil {
		t.Fatalf("RunMonitor: %v", err)
	}
	response, err := monitorabi.UnmarshalResponse(responseBytes)
	if err != nil {
		t.Fatalf("UnmarshalResponse: %v", err)
	}
	if response.Sequence != sequence || response.Status != monitorabi.StatusOK {
		t.Fatalf("monitor response = %+v", response)
	}
}

func TestMonitorExecutesFourLogicalPagesInOneHostSlab(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	backend := NewBackend()
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
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)

	// movz x8, #172 (getpid); svc #0. The sentry receives the syscall exit
	// before emulating it, so this validates EL0 entry and exception forwarding.
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	for offset := 0; offset < int(vmm.HostSlabSize); offset += int(vmm.LogicalPageSize) {
		binary.LittleEndian.PutUint32(memory[offset:], 0xd2801588)
		binary.LittleEndian.PutUint32(memory[offset+4:], 0xd4000001)
	}

	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		t.Fatal(err)
	}
	defer vcpu.Close()
	runMonitorCommand(t, vcpu, 1, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: 1})
	for page := uint64(0); page < 4; page++ {
		va := uint64(0x400000) + page*vmm.LogicalPageSize
		runMonitorCommand(t, vcpu, 2+page, monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: 1,
			Operations: []monitorabi.Operation{{
				Type:   monitorabi.OperationMap,
				Access: monitorabi.AccessRead | monitorabi.AccessExecute,
				VA:     va,
				GPA:    GuestRAMIPAStart + page*vmm.LogicalPageSize,
				Length: vmm.LogicalPageSize,
			}},
		})
		registers := &vmm.ARM64State{PC: va}
		exit, err := vcpu.Run(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
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
	runMonitorCommand(t, vcpu, 10, monitorabi.Command{
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
	runMonitorCommand(t, vcpu, 11, monitorabi.Command{
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
	exit, err := vcpu.Run(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
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
	runMonitorCommand(t, vcpu, 12, monitorabi.Command{
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
	runMonitorCommand(t, vcpu, 13, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: 1,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationProtect,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     loopVA,
			Length: vmm.LogicalPageSize,
		}},
	})
	stopInterrupts := make(chan struct{})
	interruptErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := vcpu.RequestExit(); err != nil {
					interruptErr <- err
					return
				}
			case <-stopInterrupts:
				interruptErr <- nil
				return
			}
		}
	}()
	registers = &vmm.ARM64State{PC: loopVA}
	exit, err = vcpu.Run(context.Background(), vmm.ApplicationState{AddressSpaceID: 1, Registers: registers})
	close(stopInterrupts)
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

func startTLBTortureVCPU(t *testing.T, vm vmm.VM, id int) chan<- tlbTortureJob {
	t.Helper()
	jobs := make(chan tlbTortureJob)
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		vcpu, err := vm.CreateVCPU(id)
		ready <- err
		if err != nil {
			return
		}
		defer vcpu.Close()
		for job := range jobs {
			registers := &vmm.ARM64State{PC: job.va}
			exit, err := vcpu.Run(context.Background(), vmm.ApplicationState{AddressSpaceID: job.addressSpace, Registers: registers})
			if err == nil && (exit.Reason != vmm.ExitSyscall || registers.X[8] != job.wantSyscall || registers.PC != job.va+8) {
				err = fmt.Errorf("exit=%+v x8=%d pc=%#x, want syscall=%d pc=%#x", exit, registers.X[8], registers.PC, job.wantSyscall, job.va+8)
			}
			job.done <- err
		}
	}()
	if err := <-ready; err != nil {
		close(jobs)
		t.Fatalf("CreateVCPU(%d): %v", id, err)
	}
	return jobs
}

func TestMonitorMultiVCPUTranslationReplacementTorture(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	backend := NewBackend()
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
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	// Two immutable sentinels occupy independent 4K frames in one 16K slab.
	// Every observed syscall number therefore identifies the translated GPA.
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588)                     // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001)                     // svc #0
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize:], 0xd28015a8)   // movz x8, #173
	binary.LittleEndian.PutUint32(memory[vmm.LogicalPageSize+4:], 0xd4000001) // svc #0

	monitorVCPU, err := vm.CreateVCPU(0)
	if err != nil {
		t.Fatal(err)
	}
	defer monitorVCPU.Close()
	const (
		spaceID = uint64(1)
		va      = uint64(0x500000)
	)
	sequence := uint64(1)
	runMonitorCommand(t, monitorVCPU, sequence, monitorabi.Command{Opcode: monitorabi.OpcodeCreateAddressSpace, AddressSpaceID: spaceID})
	sequence++
	workers := []chan<- tlbTortureJob{
		startTLBTortureVCPU(t, vm, 1),
		startTLBTortureVCPU(t, vm, 2),
	}
	defer func() {
		for _, worker := range workers {
			close(worker)
		}
	}()
	done := make(chan error, len(workers))
	for iteration := 0; iteration < 300; iteration++ {
		page := uint64(iteration & 1)
		gpa := GuestRAMIPAStart + page*vmm.LogicalPageSize
		want := uint64(172) + page
		if iteration%17 == 0 && iteration != 0 {
			runMonitorCommand(t, monitorVCPU, sequence, monitorabi.Command{
				Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
				Operations: []monitorabi.Operation{{Type: monitorabi.OperationUnmap, VA: va, Length: vmm.LogicalPageSize}},
			})
			sequence++
		}
		runMonitorCommand(t, monitorVCPU, sequence, monitorabi.Command{
			Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
			Operations: []monitorabi.Operation{{Type: monitorabi.OperationMap, Access: monitorabi.AccessRead | monitorabi.AccessExecute, VA: va, GPA: gpa, Length: vmm.LogicalPageSize}},
		})
		sequence++
		if iteration%23 == 0 {
			runMonitorCommand(t, monitorVCPU, sequence, monitorabi.Command{
				Opcode: monitorabi.OpcodeApply, AddressSpaceID: spaceID,
				Operations: []monitorabi.Operation{{Type: monitorabi.OperationProtect, Access: monitorabi.AccessRead, VA: va, Length: vmm.LogicalPageSize}},
			})
			sequence++
			runMonitorCommand(t, monitorVCPU, sequence, monitorabi.Command{
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

func TestPlatformCompactionRelocatesExecutingFrame(t *testing.T) {
	backend := NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		t.Skipf("Hypervisor.framework unavailable: %v", err)
	}
	p, err := vmplatform.New(vmplatform.Config{
		VM: vm, HostMemory: backend.HostMemory(), Architecture: vmarm64.Architecture{},
		VCPUs: 2, IPAStart: GuestRAMIPAStart, IPASize: 1 << 30, PlatformName: "hvf-test",
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
