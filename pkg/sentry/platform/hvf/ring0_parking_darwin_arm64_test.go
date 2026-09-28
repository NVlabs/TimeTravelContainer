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
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

// parkingTestEngine runs residencies of an application that makes one
// system call.
type parkingTestEngine struct {
	vm      *hvfVM
	engine  *ring0Engine
	spaceID uint64
	codeVA  uint64
}

func newParkingTestEngine(t *testing.T, vcpus int) *parkingTestEngine {
	t.Helper()
	backend := NewBackend()
	vmInterface, err := backend.CreateVM(context.Background())
	if err != nil {
		skipWithoutHypervisor(t, err)
	}
	vm := vmInterface.(*hvfVM)
	engineInterface, err := NewVMEngine(vm, vcpus)
	if err != nil {
		vm.Close()
		t.Fatal(err)
	}
	p := &parkingTestEngine{vm: vm, engine: engineInterface.(*ring0Engine), spaceID: 50, codeVA: 0x900000}
	host, err := backend.HostMemory().Reserve(vmm.HostSlabSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Close retires every vCPU, parked or not; otherwise the VM, which
		// the next test needs to create anew, cannot be destroyed.
		p.engine.Close()
		vm.Close()
		backend.HostMemory().Release(host, vmm.HostSlabSize)
	})
	if err := vm.MapStage2(GuestRAMIPAStart, host, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		t.Fatal(err)
	}
	memory := unsafe.Slice((*byte)(unsafe.Pointer(host)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(memory[0:], 0xd2801588) // movz x8, #172
	binary.LittleEndian.PutUint32(memory[4:], 0xd4000001) // svc #0
	runRing0InternalMonitor(t, p.engine, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: p.spaceID,
	})
	runRing0InternalMonitor(t, p.engine, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: p.spaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     p.codeVA,
			GPA:    GuestRAMIPAStart,
			Length: vmm.LogicalPageSize,
		}},
	})
	return p
}

// residency runs one residency of the application, on the calling
// goroutine's thread.
func (p *parkingTestEngine) residency() error {
	p.engine.BeginApplicationResidency()
	registers := &vmm.ARM64State{PC: p.codeVA}
	exit, err := p.engine.RunApplication(context.Background(), vmm.ApplicationState{
		AddressSpaceID: p.spaceID,
		Registers:      registers,
	})
	endErr := p.engine.EndApplicationResidency()
	if err != nil {
		return err
	}
	if exit.Reason != vmm.ExitSyscall || registers.X[8] != 172 {
		return fmt.Errorf("application exit %+v with x8=%d, want its system call", exit, registers.X[8])
	}
	return endErr
}

// counts returns the number of vCPUs, and of parked ones, once those that
// their owner threads retired are reaped.
func (p *parkingTestEngine) counts() (live, parked int) {
	e := p.engine
	e.cpuMu.Lock()
	defer e.cpuMu.Unlock()
	e.reapDestroyedCPUsLocked()
	return e.applicationCPUCountLocked(), e.parkedCPUs
}

// close closes the engine, then the VM, which fails if any vCPU remains.
func (p *parkingTestEngine) close(t *testing.T) {
	t.Helper()
	start := time.Now()
	if err := p.engine.Close(); err != nil {
		t.Fatalf("closing the engine: %v", err)
	}
	if err := p.vm.Close(); err != nil {
		t.Fatalf("destroying the VM after the engine closed: %v", err)
	}
	t.Logf("closed in %v", time.Since(start))
}

func TestParkedVCPUServesTheThreadsNextResidency(t *testing.T) {
	p := newParkingTestEngine(t, 2)
	const residencies = 20
	result := make(chan error, 1)
	go func() {
		// Keep every residency on one thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		for i := 0; i < residencies; i++ {
			if err := p.residency(); err != nil {
				result <- fmt.Errorf("residency %d: %w", i, err)
				return
			}
		}
		result <- nil
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if created, reused := p.engine.vcpusCreated.Load(), p.engine.vcpusReused.Load(); created != 1 || reused != residencies-1 {
		t.Errorf("%d residencies on one thread created %d vCPUs and reused %d, want 1 and %d", residencies, created, reused, residencies-1)
	}
	if live, parked := p.counts(); live != 1 || parked != 1 {
		t.Errorf("after the residencies: %d vCPUs, %d parked, want the thread's vCPU parked", live, parked)
	}
	// The thread went back to the scheduler with its vCPU parked, so Close
	// has it retire the vCPU at a safe point.
	p.close(t)
}

func TestCloseRetiresVCPUsParkedOnIdleThreads(t *testing.T) {
	const threads = 4
	p := newParkingTestEngine(t, threads)
	var parked, release sync.WaitGroup
	parked.Add(threads)
	release.Add(1)
	results := make(chan error, threads)
	for i := 0; i < threads; i++ {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			err := p.residency()
			parked.Done()
			// Hold the thread until every goroutine has parked a vCPU, so
			// that each parks it on a thread of its own.
			release.Wait()
			results <- err
		}()
	}
	parked.Wait()
	if live, parkedCount := p.counts(); live != threads || parkedCount != threads {
		t.Errorf("%d threads parked %d of %d vCPUs, want one each", threads, parkedCount, live)
	}
	release.Done()
	for i := 0; i < threads; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	p.close(t)
	if requested := p.engine.vcpusRetirementsRequested.Load(); requested == 0 {
		t.Errorf("Close retired the parked vCPUs without asking their threads")
	}
}

func TestAdmissionRetiresParkedVCPU(t *testing.T) {
	p := newParkingTestEngine(t, 2)
	var parked sync.WaitGroup
	parked.Add(2)
	hold := make(chan struct{})
	results := make(chan error, 3)
	for i := 0; i < 2; i++ {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			err := p.residency()
			parked.Done()
			// Sleep in the scheduler, keeping both the thread, locked, and
			// its parked vCPU from the next residency.
			<-hold
			results <- err
		}()
	}
	parked.Wait()
	admitted := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		admitted <- p.residency()
	}()
	// Both vCPU IDs belong to vCPUs parked on sleeping threads. The third
	// residency must have one of them retired to take its ID.
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a residency waited 10s for a vCPU ID held by a parked vCPU")
	}
	if requested := p.engine.vcpusRetirementsRequested.Load(); requested == 0 {
		t.Errorf("the residency was admitted without retiring a parked vCPU")
	}
	close(hold)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	p.close(t)
}

func TestSystemCallRetiresParkedVCPU(t *testing.T) {
	p := newParkingTestEngine(t, 2)
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	// With one P, a goroutine that this one starts runs next on its thread
	// once this one blocks.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := p.residency(); err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		tid := hosttid.Current()
		// The goroutine that parked the vCPU, a task's, makes system calls of
		// its own that end, as when it loads a binary to execute: the vCPU
		// stays parked.
		if _, err := unix.Write(fds[1], []byte{1}); err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		if live, parked := p.counts(); live != 1 || parked != 1 {
			runtime.UnlockOSThread()
			result <- fmt.Errorf("after the parking goroutine's system call: %d vCPUs, %d parked, want the vCPU parked", live, parked)
			return
		}
		// Any other goroutine's system call could block the thread
		// indefinitely, so the thread retires its vCPU first.
		other := make(chan error, 1)
		go func() {
			if got := hosttid.Current(); got != tid {
				other <- fmt.Errorf("the other goroutine ran on thread %d, not %d", got, tid)
				return
			}
			_, err := unix.Write(fds[1], []byte{1})
			other <- err
		}()
		runtime.UnlockOSThread()
		result <- <-other
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if live, parked := p.counts(); live != 0 || parked != 0 {
		t.Errorf("after another goroutine's system call on its thread: %d vCPUs, %d parked, want none", live, parked)
	}
	p.close(t)
}

func TestThreadExitRetiresParkedVCPU(t *testing.T) {
	p := newParkingTestEngine(t, 2)
	result := make(chan error, 1)
	go func() {
		// The goroutine exits locked to its thread, so the thread exits
		// with it, and retires its parked vCPU as it does.
		runtime.LockOSThread()
		result <- p.residency()
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, parked := p.counts()
		if live == 0 && parked == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s after its thread exited: %d vCPUs, %d parked, want none", live, parked)
		}
		time.Sleep(time.Millisecond)
	}
	p.close(t)
}

func TestMonitorCommandLeavesParkedVCPU(t *testing.T) {
	p := newParkingTestEngine(t, 2)
	command, err := monitorabi.MarshalCommand(monitorabi.Command{
		Sequence:       1,
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: p.spaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead,
			VA:     p.codeVA + vmm.LogicalPageSize,
			GPA:    GuestRAMIPAStart + vmm.LogicalPageSize,
			Length: vmm.LogicalPageSize,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := p.residency(); err != nil {
			result <- err
			return
		}
		// A task parks its vCPU before it handles a fault that may block,
		// and then changes its page tables from whichever thread it runs on.
		// That thread's parked vCPU must not serve the change: the task may
		// have found it before moving to another thread, from which
		// Hypervisor.framework rejects the vCPU.
		if cpu := p.engine.pinCurrentIdleApplicationCPU(); cpu != nil {
			p.engine.releaseApplicationCPU(cpu)
			result <- fmt.Errorf("a goroutine outside a residency took its thread's parked vCPU %d", cpu.vcpu.id)
			return
		}
		responseBytes, err := p.engine.RunMonitor(context.Background(), command)
		if err != nil {
			result <- fmt.Errorf("changing page tables with the thread's vCPU parked: %w", err)
			return
		}
		if response, err := monitorabi.UnmarshalResponse(responseBytes); err != nil || response.Status != monitorabi.StatusOK {
			result <- fmt.Errorf("monitor response=%+v err=%v", response, err)
			return
		}
		if live, parked := p.counts(); live != 1 || parked != 1 {
			result <- fmt.Errorf("after the page-table change: %d vCPUs, %d parked, want the vCPU parked", live, parked)
			return
		}
		// The thread's next residency takes the vCPU.
		result <- p.residency()
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if created, reused := p.engine.vcpusCreated.Load(), p.engine.vcpusReused.Load(); created != 1 || reused != 1 {
		t.Errorf("two residencies on one thread created %d vCPUs and reused %d, want 1 and 1", created, reused)
	}
	p.close(t)
}
