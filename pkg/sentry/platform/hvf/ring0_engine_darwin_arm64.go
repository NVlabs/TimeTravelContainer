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
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

const (
	ring0SentryAddressSpaceID = ^uint64(0)
	ring0HostVALimit          = uint64(1 << 48)
	ring0HostMirrorGPAStart   = SentryMirrorIPAStart
	// Demand-map enough neighboring Go stack/heap memory to amortize host-gate
	// faults without turning every new stack sample into a multi-megabyte stage-1
	// transaction. A 2 MiB radius made concurrent exec startup retain and sort
	// thousands of 16K slabs, eventually timing out application switches.
	ring0HostWorkingSetRadius = uint64(256 << 10)
	ring0MonitorVCPUID        = 63
	stateApplicationFP        = 0x3000
	stateRing0Entry           = 0x3340
)

type ring0ApplicationCPU struct {
	vcpu              *hvfVCPU
	bridge            *ring0Bridge
	tid               uint64
	uses              int
	applicationActive atomic.Bool
}

type ring0Stage2Identity struct {
	objectID     uint64
	objectOffset uint64
	access       vmm.Stage2Access
}

type ring0Stage2Lease struct {
	hostAddress uintptr
	length      uint64
	shadow      bool
	refs        int
}

type ring0Stage2Slab struct {
	ring0Stage2Identity
	lease *ring0Stage2Lease
	gpa   uint64
}

// ring0Engine keeps hypervisor mechanics below vmplatform.VMEngine. GuestRAM,
// application roots, staging, checkpoint policy, and Linux MM remain in the
// portable layer; this type owns only Darwin continuation transfer, the
// Sentry execution mirror, and vCPU residency.
type ring0Engine struct {
	vm          *hvfVM
	monitorPool *vmplatform.WorkerPool
	monitor     *vmplatform.Monitor
	concurrency int
	monitorMu   sync.Mutex

	// entryMu excludes new application entries while refreshHostMirror
	// redpills existing Sentry continuations and replaces host-memory leases.
	// It does not protect application roots or GuestRAM.
	entryMu       sync.RWMutex
	mirrorMu      sync.Mutex
	stage2Slabs   map[uintptr]ring0Stage2Slab
	stage1Access  map[uint64]monitorabi.Access
	nextMirrorGPA uint64
	faultMu       sync.Mutex
	pendingMirror map[uintptr]struct{}
	sentryTTBR0   uint64

	cpuMu    sync.Mutex
	cpusByID [ring0MonitorVCPUID]*ring0ApplicationCPU
	cpuIDs   []bool
	closed   bool
	closeMu  sync.Mutex
	closeErr error
}

// NewVMEngine constructs the Darwin Sentry-at-EL1 execution policy for the
// vmplatform/hvf registration package.
func NewVMEngine(vmInterface vmm.VM, vcpus int) (vmplatform.VMEngine, error) {
	vm, ok := vmInterface.(*hvfVM)
	if !ok {
		return nil, fmt.Errorf("HVF ring0 engine requires *hvfVM, got %T", vmInterface)
	}
	if vcpus <= 0 {
		return nil, fmt.Errorf("invalid HVF ring0 vCPU count %d", vcpus)
	}
	if vcpus > ring0MonitorVCPUID {
		vcpus = ring0MonitorVCPUID
	}
	if err := installRing0SignalBridge(); err != nil {
		return nil, err
	}
	monitorPool, err := vmplatform.NewWorkerPoolWithIDBase(vm, 1, ring0MonitorVCPUID)
	if err != nil {
		return nil, fmt.Errorf("creating HVF ring0 monitor vCPU: %w", err)
	}
	e := &ring0Engine{
		vm:            vm,
		monitorPool:   monitorPool,
		concurrency:   vcpus,
		stage2Slabs:   make(map[uintptr]ring0Stage2Slab),
		stage1Access:  make(map[uint64]monitorabi.Access),
		nextMirrorGPA: ring0HostMirrorGPAStart,
		pendingMirror: make(map[uintptr]struct{}),
		cpuIDs:        make([]bool, vcpus),
	}
	var expander vmm.MonitorPoolExpander
	if candidate, ok := vmInterface.(vmm.MonitorPoolExpander); ok {
		expander = candidate
	}
	e.monitor = vmplatform.NewMonitor(e, expander)
	if err := e.monitor.Execute(context.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: ring0SentryAddressSpaceID,
		Flags:          monitorabi.AddressSpaceFlagSentry,
	}); err != nil {
		monitorPool.Close()
		return nil, fmt.Errorf("creating monitor-owned Sentry address space: %w", err)
	}
	if err := e.refreshHostMirror(context.Background()); err != nil {
		cleanupErr := e.releaseHostMirror()
		monitorPool.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	e.sentryTTBR0 = vm.monitorAddressSpaceTTBR(ring0SentryAddressSpaceID)
	if e.sentryTTBR0 == 0 {
		monitorPool.Close()
		return nil, fmt.Errorf("monitor did not publish the Sentry TTBR0")
	}
	return e, nil
}

func (e *ring0Engine) RunMonitor(ctx context.Context, encoded []byte) ([]byte, error) {
	e.monitorMu.Lock()
	defer e.monitorMu.Unlock()
	if len(encoded) == 0 || len(encoded) > monitorCommandMax {
		return nil, fmt.Errorf("invalid EL1 monitor command length %d", len(encoded))
	}
	// Page-table transactions use a dedicated monitor vCPU. If the caller is a
	// Sentry continuation at EL1, redpill it first; at host EL0 this HVC-shaped
	// trap is skipped by the SIGILL bridge. Application syscalls remain entirely
	// in EL1 and do not take this path.
	ring0asm.EnsureHost()
	value, err := e.monitorPool.Do(ctx, func(ctx context.Context, vcpu vmm.VCPU) (any, error) {
		return vcpu.RunMonitor(ctx, encoded)
	})
	if err != nil {
		return nil, err
	}
	response, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("HVF ring0 monitor returned %T, want []byte", value)
	}
	return response, nil
}

func (e *ring0Engine) findApplicationCPUByTIDLocked(tid uint64) *ring0ApplicationCPU {
	for _, cpu := range e.cpusByID {
		if cpu != nil && cpu.tid == tid {
			return cpu
		}
	}
	return nil
}

func (e *ring0Engine) applicationCPUCountLocked() int {
	count := 0
	for _, cpu := range e.cpusByID {
		if cpu != nil {
			count++
		}
	}
	return count
}

func (e *ring0Engine) RunApplication(ctx context.Context, application vmm.ApplicationState) (retExit vmm.Exit, retErr error) {
	defer func() {
		if retErr == nil {
			return
		}
		// Sentry boot reserves fd 3 for --panic-log-fd. Platform failures are
		// normally converted into a guest SIGKILL, so without this diagnostic the
		// client sees only status 137 or control-socket EOF. write(2) is safe here
		// and deliberately avoids creating an os.File that could close the fd.
		message := []byte("ttc hvf application error: " + retErr.Error() + "\n")
		_, _ = syscall.Write(3, message)
	}()
	registers, ok := application.Registers.(*vmm.ARM64State)
	if !ok {
		return vmm.Exit{}, fmt.Errorf("HVF ring0 ARM64 state has type %T, want *vmm.ARM64State", application.Registers)
	}
	if !validApplicationPState(registers.PState) {
		return vmm.Exit{}, fmt.Errorf("invalid ARM64 application PSTATE %#x before ring0 EL0 entry", registers.PState)
	}
	cpu, wasGuest, err := e.getApplicationCPU(ctx)
	if err != nil {
		return vmm.Exit{}, err
	}
	cleanupEnabled := true
	cleanupCPU := func() {
		cpu.applicationActive.Store(false)
		atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested)), 0)
		e.releaseApplicationCPU(cpu)
		runtime.UnlockOSThread()
	}
	defer func() {
		if cleanupEnabled {
			cleanupCPU()
		}
	}()
	if err := ctx.Err(); err != nil {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	if !wasGuest {
		// The post-EL0 fast path touches these control objects before it can
		// return to ordinary Sentry code and demand-learn any other heap page.
		// Lease them up front so bridge bookkeeping itself cannot force a
		// host-service redpill on every syscall.
		e.requestHostMirror(uintptr(unsafe.Pointer(e)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.vcpu)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.bridge)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.bridge.pointer)))
		e.requestHostMirror(cpu.vcpu.stateHost)
		e.requestHostMirror(uintptr(unsafe.Pointer(registers)))
		g, m, p, mcache := hosttid.RuntimePointers()
		e.requestHostMirror(g)
		e.requestHostMirror(m)
		e.requestHostMirror(p)
		e.requestHostMirror(mcache)
		e.requestHostMirror(hosttid.RuntimeTLSBase())
		if err := e.refreshHostMirror(ctx); err != nil {
			return vmm.Exit{}, errors.Join(err, e.retireUnenteredApplicationCPU(cpu))
		}
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint64(state[stateAddressSpaceID:], application.AddressSpaceID)
	for i, value := range registers.X {
		binary.LittleEndian.PutUint64(state[stateX0+i*8:], value)
	}
	binary.LittleEndian.PutUint64(state[stateSP:], registers.SP)
	binary.LittleEndian.PutUint64(state[statePC:], registers.PC)
	binary.LittleEndian.PutUint64(state[statePState:], registers.PState&^0xf)
	binary.LittleEndian.PutUint64(state[stateTPIDR:], registers.TPIDR)
	binary.LittleEndian.PutUint64(state[stateExitKind:], 0)
	binary.LittleEndian.PutUint64(state[stateRing0Entry:],
		monitorCodeVA+cpu.vcpu.monitor.ring0ApplicationOffset)
	copy(state[stateApplicationFP:stateApplicationFP+len(registers.FP)], registers.FP[:])
	if err := ctx.Err(); err != nil {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	e.entryMu.RLock()
	e.cpuMu.Lock()
	closed := e.closed
	e.cpuMu.Unlock()
	if closed {
		e.entryMu.RUnlock()
		if err := e.retireUnenteredApplicationCPU(cpu); err != nil {
			return vmm.Exit{}, err
		}
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	if wasGuest && ring0asm.CurrentEL() == 0 {
		// The Sentry continuation may redpill while preparing state or waiting
		// for entryMu. Release this now-host-side residency and select again; the
		// next attempt will retire any stale owner record before creating a vCPU.
		e.entryMu.RUnlock()
		cleanupEnabled = false
		cleanupCPU()
		return e.RunApplication(ctx, application)
	}
	if !wasGuest {
		// Qualify the owner-affine vCPU only after excluding host-mirror and
		// stage-2 reconciliation. Keep this immediately adjacent to native entry
		// so a successful write probe describes the context that enters HVF.
		if err := cpu.bridge.preflight(); err != nil {
			e.entryMu.RUnlock()
			return vmm.Exit{}, errors.Join(err, e.retireUnenteredApplicationCPU(cpu))
		}
		ring0asm.SwitchApplication(unsafe.Pointer(cpu.bridge.pointer), cpu.vcpu.stateVA,
			monitorCodeVA+cpu.vcpu.monitor.ring0ApplicationOffset,
			cpu.bridge.hostStackTop(), ring0NativeEntryAddress(),
			cpu.bridge.bootstrap())
	} else {
		// The Sentry continuation is already executing on this vCPU at EL1.
		// Calling the host-entry trampoline here would itself fault through the
		// host gate and attempt a nested HVF operation from macOS EL0.
		ring0asm.SwitchApplicationGuest()
	}
	e.entryMu.RUnlock()
	if err := cpu.bridge.error("HVF ring0 application bridge failed"); err != nil {
		postSignalErr := cpu.bridge.probeOwner()
		retireErr := e.retireUnenteredApplicationCPU(cpu)
		return vmm.Exit{}, errors.Join(err, postSignalErr, retireErr)
	}
	for i := range registers.X {
		registers.X[i] = binary.LittleEndian.Uint64(state[stateX0+i*8:])
	}
	registers.SP = binary.LittleEndian.Uint64(state[stateSP:])
	registers.PC = binary.LittleEndian.Uint64(state[statePC:])
	registers.PState = binary.LittleEndian.Uint64(state[statePState:])
	registers.TPIDR = binary.LittleEndian.Uint64(state[stateTPIDR:])
	copy(registers.FP[:], state[stateApplicationFP:stateApplicationFP+len(registers.FP)])
	exitKind := binary.LittleEndian.Uint64(state[stateExitKind:])
	syndrome := binary.LittleEndian.Uint64(state[stateESR:])
	address := binary.LittleEndian.Uint64(state[stateFAR:])
	switch exitKind {
	case 1:
		return vmm.Exit{Reason: vmm.ExitSyscall, Syndrome: syndrome}, nil
	case 2:
		ec := syndrome >> 26 & 0x3f
		access := vmm.Stage2Read
		if ec == 0x20 || ec == 0x21 {
			access = vmm.Stage2Execute
		} else if syndrome&(1<<6) != 0 {
			access = vmm.Stage2Write
		}
		return vmm.Exit{Reason: vmm.ExitFault, Syndrome: syndrome, Address: address, Access: access}, nil
	case 3:
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	default:
		if atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))) != 0 {
			return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
		}
		if err := ctx.Err(); err != nil {
			return vmm.Exit{}, err
		}
		return vmm.Exit{}, errors.New("EL1 monitor returned no ring0 application exit state")
	}
}

func (e *ring0Engine) requestHostMirror(address uintptr) {
	if uint64(address) >= ring0HostVALimit {
		return
	}
	e.faultMu.Lock()
	e.pendingMirror[address&^uintptr(vmm.HostSlabSize-1)] = struct{}{}
	e.faultMu.Unlock()
}

// retireUnenteredApplicationCPU destroys a vCPU that was created by the
// current owner thread but never crossed the bluepill boundary. Such a vCPU
// cannot use the signal handler's normal owner-affine retirement path.
func (e *ring0Engine) retireUnenteredApplicationCPU(cpu *ring0ApplicationCPU) error {
	if cpu.bridge.inGuest() || cpu.bridge.destroyed() {
		return nil
	}
	if err := cpu.vcpu.Close(); err != nil {
		return fmt.Errorf("retiring unentered owner-affine HVF ring0 vCPU %d: %w", cpu.vcpu.id, err)
	}
	cpu.bridge.markDestroyed()
	return nil
}

func (e *ring0Engine) getApplicationCPU(ctx context.Context) (*ring0ApplicationCPU, bool, error) {
	runtime.LockOSThread()
	tid := hosttid.Current()
	atGuestEL1 := ring0asm.CurrentEL() != 0

retry:
	e.cpuMu.Lock()
	reapedOwner := false
	for {
		stale := e.findApplicationCPUByTIDLocked(tid)
		if stale == nil || !stale.bridge.destroyed() {
			break
		}
		if stale.uses == 0 {
			e.reapApplicationCPULocked(stale)
			reapedOwner = true
			break
		}
		// Host-mirror reconciliation and Close pin bridges while issuing an
		// interrupt without cpuMu. A destroyed owner record cannot be recycled
		// until that short-lived reference drains.
		e.cpuMu.Unlock()
		runtime.Gosched()
		e.cpuMu.Lock()
	}
	if reapedOwner {
		// The cancellation/restore transition can complete after the CurrentEL
		// sample at function entry but before cpuMu is acquired. The destroyed
		// owner record is authoritative; resample after reaping it rather than
		// carrying a stale EL1 observation into the no-owner check below.
		e.cpuMu.Unlock()
		atGuestEL1 = ring0asm.CurrentEL() != 0
		goto retry
	}
	e.reapDestroyedCPUsLocked()
	if e.closed {
		e.cpuMu.Unlock()
		runtime.UnlockOSThread()
		return nil, false, fmt.Errorf("HVF ring0 engine is closed")
	}
	if cpu := e.findApplicationCPUByTIDLocked(tid); cpu != nil {
		if cpu.bridge.destroyed() {
			// CurrentEL was sampled before taking cpuMu. A concurrent mirror
			// reconciliation may cancel this continuation in that interval; the
			// owner C loop then destroys the vCPU and restores the continuation at
			// host EL0. Reap that completed residency transition and sample the
			// architectural state again instead of killing the guest task for an
			// internally consistent false->true publication race.
			if cpu.uses != 0 {
				e.cpuMu.Unlock()
				runtime.Gosched()
				atGuestEL1 = ring0asm.CurrentEL() != 0
				goto retry
			}
			e.reapApplicationCPULocked(cpu)
			e.cpuMu.Unlock()
			atGuestEL1 = ring0asm.CurrentEL() != 0
			if atGuestEL1 {
				runtime.UnlockOSThread()
				return nil, false, fmt.Errorf("guest EL1 continuation on host thread %d retained no vCPU after completed cancellation", tid)
			}
			goto retry
		}
		if !atGuestEL1 {
			// Go may schedule a different continuation on an owner thread after
			// the prior Sentry continuation redpilled. Architectural CurrentEL is
			// authoritative: retire the stale residency record on its owner before
			// creating a replacement vCPU.
			cpu.uses++
			e.cpuMu.Unlock()
			closeErr := cpu.vcpu.closeStaleOwner()
			cpu.bridge.markDestroyed()
			e.cpuMu.Lock()
			cpu.uses--
			e.reapApplicationCPULocked(cpu)
			e.cpuMu.Unlock()
			if closeErr != nil {
				runtime.UnlockOSThread()
				return nil, false, fmt.Errorf("retiring stale host-side HVF ring0 vCPU %d: %w", cpu.vcpu.id, closeErr)
			}
			goto retry
		}
		// CurrentEL is the authority for the calling continuation. in_guest is a
		// cross-thread hint used to decide whether hv_vcpu_exit is useful; the
		// host-return path clears it before its final SIGILL publishes destroyed.
		// A continuation interrupted between the CurrentEL probe and cpuMu may
		// therefore observe false here during that publication window. Keep the
		// owner record and let RunApplication's final CurrentEL check below either
		// enter it at EL1 or retry after the host return completes.
		cpu.uses++
		cpu.applicationActive.Store(true)
		e.cpuMu.Unlock()
		return cpu, true, nil
	}
	if atGuestEL1 {
		e.cpuMu.Unlock()
		if ring0asm.CurrentEL() == 0 {
			// A cancellation may have redpilled this continuation after the
			// entry sample while reapDestroyedCPUsLocked removed its final owner
			// record. Retry as a host continuation.
			atGuestEL1 = false
			goto retry
		}
		runtime.UnlockOSThread()
		return nil, false, fmt.Errorf("guest EL1 continuation on host thread %d has no resident HVF vCPU", tid)
	}
	id := -1
	for candidate, used := range e.cpuIDs {
		if !used {
			id = candidate
			break
		}
	}
	if id < 0 {
		// A Go continuation may migrate after returning from RunApplication,
		// leaving the previous host thread resident in EL1 without an active
		// application. Reclaim one such continuation under pressure: HVF
		// cancellation snapshots it and returns it to host execution on its
		// owner thread before the vCPU ID is recycled.
		var reclaim *ring0ApplicationCPU
		for _, candidate := range e.cpusByID {
			if candidate != nil && !candidate.applicationActive.Load() &&
				candidate.bridge.inGuest() && !candidate.bridge.destroyed() {
				reclaim = candidate
				break
			}
		}
		if reclaim != nil {
			reclaim.uses++
			e.cpuMu.Unlock()
			runtime.UnlockOSThread()
			requestErr := reclaim.vcpu.RequestExit()
			deadline := time.Now().Add(5 * time.Second)
			for requestErr == nil && !reclaim.bridge.destroyed() &&
				time.Now().Before(deadline) && ctx.Err() == nil {
				runtime.Gosched()
			}
			destroyed := reclaim.bridge.destroyed()
			e.cpuMu.Lock()
			reclaim.uses--
			e.reapApplicationCPULocked(reclaim)
			e.cpuMu.Unlock()
			if requestErr != nil {
				return nil, false, fmt.Errorf("interrupting inactive HVF ring0 vCPU %d for reuse: %w", reclaim.vcpu.id, requestErr)
			}
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if !destroyed {
				return nil, false, fmt.Errorf("timed out returning inactive HVF ring0 vCPU %d to host for reuse", reclaim.vcpu.id)
			}
			runtime.LockOSThread()
			tid = hosttid.Current()
			atGuestEL1 = ring0asm.CurrentEL() != 0
			goto retry
		}
		e.cpuMu.Unlock()
		runtime.UnlockOSThread()
		// All resident continuations are currently inside application entries.
		// Wait without pinning an otherwise idle OS thread; the next retry may
		// run on a different host thread and must therefore refresh its TID.
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(100 * time.Microsecond):
		}
		runtime.LockOSThread()
		tid = hosttid.Current()
		atGuestEL1 = ring0asm.CurrentEL() != 0
		goto retry
	}
	e.cpuIDs[id] = true
	e.cpuMu.Unlock()
	vcpuInterface, err := e.vm.CreateVCPU(id)
	if err != nil {
		e.cpuMu.Lock()
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		runtime.UnlockOSThread()
		return nil, false, err
	}
	vcpu := vcpuInterface.(*hvfVCPU)
	bridge, err := vcpu.newRing0Bridge(e.sentryTTBR0, true)
	if err != nil {
		_ = vcpu.Close()
		e.cpuMu.Lock()
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		runtime.UnlockOSThread()
		return nil, false, err
	}
	cpu := &ring0ApplicationCPU{vcpu: vcpu, bridge: bridge, tid: tid, uses: 1}
	cpu.applicationActive.Store(true)
	atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(vcpu.stateHost), stateCancelRequested)), 0)
	e.cpuMu.Lock()
	if e.closed {
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		bridge.close()
		_ = vcpu.Close()
		runtime.UnlockOSThread()
		return nil, false, fmt.Errorf("HVF ring0 engine closed while creating vCPU")
	}
	e.cpusByID[id] = cpu
	e.cpuMu.Unlock()
	return cpu, false, nil
}

func (e *ring0Engine) releaseApplicationCPU(cpu *ring0ApplicationCPU) {
	e.cpuMu.Lock()
	if cpu.uses > 0 {
		cpu.uses--
	}
	e.reapApplicationCPULocked(cpu)
	e.cpuMu.Unlock()
}

// pinApplicationCPUs returns a stable snapshot of every application bridge.
// A bridge is freed by reapDestroyedCPUsLocked only when uses reaches zero,
// so callers may invoke RequestExit after dropping cpuMu without racing a
// concurrent application return that retires its owner-affine vCPU.
func (e *ring0Engine) pinApplicationCPUs() []*ring0ApplicationCPU {
	e.cpuMu.Lock()
	defer e.cpuMu.Unlock()
	cpus := make([]*ring0ApplicationCPU, 0, e.concurrency)
	for _, cpu := range e.cpusByID {
		if cpu == nil {
			continue
		}
		cpu.uses++
		cpus = append(cpus, cpu)
	}
	return cpus
}

func (e *ring0Engine) unpinApplicationCPUs(cpus []*ring0ApplicationCPU) {
	for _, cpu := range cpus {
		e.releaseApplicationCPU(cpu)
	}
}

// reapDestroyedCPUsLocked retires bridges only after their owning signal
// handler has destroyed the underlying HVF vCPU. This makes the small fixed
// vCPU-ID/state-page set recyclable across Go's changing host-thread set
// without ever invoking an owner-affine HVF operation from the wrong thread.
func (e *ring0Engine) reapDestroyedCPUsLocked() {
	for _, cpu := range e.cpusByID {
		if cpu == nil {
			continue
		}
		e.reapApplicationCPULocked(cpu)
	}
}

func (e *ring0Engine) reapApplicationCPULocked(cpu *ring0ApplicationCPU) {
	if !cpu.bridge.destroyed() || cpu.uses != 0 {
		return
	}
	e.learnHostGateFault(cpu)
	if cpu.vcpu.id >= 0 && cpu.vcpu.id < len(e.cpuIDs) {
		e.cpuIDs[cpu.vcpu.id] = false
		e.cpusByID[cpu.vcpu.id] = nil
	}
	cpu.bridge.close()
}

// learnHostGateFault expands the Sentry execution mirror after an EL1 data
// abort entered the host-service gate. Calls to external instructions are
// deliberately absent from the Sentry root and therefore do not add mappings.
func (e *ring0Engine) learnHostGateFault(cpu *ring0ApplicationCPU) {
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	esr := binary.LittleEndian.Uint64(state[stateHostGateESR:])
	far := binary.LittleEndian.Uint64(state[stateHostGateFAR:])
	clear(state[stateHostGateESR : stateHostGateFAR+8])
	if esr>>26&0x3f != 0x25 || far >= ring0HostVALimit {
		return
	}
	e.requestHostMirror(uintptr(far))
}

func (e *ring0Engine) refreshHostMirror(ctx context.Context) error {
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	e.entryMu.Lock()
	defer e.entryMu.Unlock()
	if err := e.quiesceGuestContinuations(ctx, 5*time.Second); err != nil {
		return fmt.Errorf("quiescing Sentry continuations for host-memory reconciliation: %w", err)
	}
	regions, err := snapshotRing0HostRegions(ring0HostVALimit)
	if err != nil {
		return err
	}
	desiredSlabs := make(map[uintptr]ring0Stage2Identity)
	desiredAccess := make(map[uint64]monitorabi.Access)
	// Existing leases are retained exactly. They must not be treated as new
	// working-set samples: expanding a window around every retained slab on
	// each reconciliation grows the mirror by another radius per pass, until a
	// routine exec ends up copying most of the Go heap.
	retainedSlabs := make([]uintptr, 0, len(e.stage2Slabs))
	for address := range e.stage2Slabs {
		retainedSlabs = append(retainedSlabs, address)
	}
	sort.Slice(retainedSlabs, func(i, j int) bool { return retainedSlabs[i] < retainedSlabs[j] })
	workingSetSlabs := make(map[uintptr]struct{}, 3)
	addWorkingSetSlab := func(address uintptr) {
		// Retained mappings already cover this sample and are copied into the
		// desired set below. Expanding another window around the same slab only
		// increases reconciliation work; host-gate faults add genuinely new slabs.
		if _, mapped := e.stage2Slabs[address]; !mapped {
			workingSetSlabs[address] = struct{}{}
		}
	}
	// The guest-only tail of SwitchApplication contains nested calls after the
	// bluepill. Go's ARM64 call frame may move SP across a 16K boundary even
	// when the sampled SP is exactly at the start of the current stack slab.
	// Map both adjacent slabs so that tail cannot take a host-service data
	// abort and resume at host EL0 before it has returned to ordinary Go code.
	stackSlab := ring0asm.CurrentSP() &^ uintptr(vmm.HostSlabSize-1)
	addWorkingSetSlab(stackSlab)
	if stackSlab >= uintptr(vmm.HostSlabSize) {
		addWorkingSetSlab(stackSlab - uintptr(vmm.HostSlabSize))
	}
	if stackSlab <= ^uintptr(0)-uintptr(vmm.HostSlabSize) {
		addWorkingSetSlab(stackSlab + uintptr(vmm.HostSlabSize))
	}
	pendingSlabs := make(map[uintptr]struct{})
	e.faultMu.Lock()
	for address := range e.pendingMirror {
		pendingSlabs[address] = struct{}{}
		addWorkingSetSlab(address)
	}
	e.faultMu.Unlock()
	workingSetAddresses := make([]uintptr, 0, len(workingSetSlabs))
	for address := range workingSetSlabs {
		workingSetAddresses = append(workingSetAddresses, address)
	}
	sort.Slice(workingSetAddresses, func(i, j int) bool { return workingSetAddresses[i] < workingSetAddresses[j] })
	forEachInRegion := func(addresses []uintptr, start, end uintptr, fn func(uintptr)) {
		index := sort.Search(len(addresses), func(i int) bool { return addresses[i] >= start })
		for index < len(addresses) && addresses[index] < end {
			fn(addresses[index])
			index++
		}
	}
	for _, region := range regions {
		access := monitorabi.AccessRead
		if region.protection&ring0VMWrite != 0 {
			access |= monitorabi.AccessWrite
		}
		if region.protection&ring0VMExecute != 0 && region.mainImage {
			access |= monitorabi.AccessExecute
		}
		stage2Access := vmm.Stage2Access(0)
		if region.protection&ring0VMRead != 0 {
			stage2Access |= vmm.Stage2Read
		}
		if region.protection&ring0VMWrite != 0 {
			stage2Access |= vmm.Stage2Write
		}
		if region.protection&ring0VMExecute != 0 && region.mainImage {
			stage2Access |= vmm.Stage2Execute
		}
		if stage2Access == 0 {
			continue
		}
		addSlab := func(address uintptr) {
			offset := uint64(address - region.address)
			desiredSlabs[address] = ring0Stage2Identity{
				objectID:     region.objectID,
				objectOffset: region.objectOffset + offset,
				access:       stage2Access,
			}
			for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
				desiredAccess[va] = access
			}
		}
		if region.mainImage {
			for offset := uint64(0); offset < region.length; offset += vmm.HostSlabSize {
				addSlab(region.address + uintptr(offset))
			}
			continue
		}
		regionEnd := region.address + uintptr(region.length)
		forEachInRegion(retainedSlabs, region.address, regionEnd, addSlab)
		forEachInRegion(workingSetAddresses, region.address, regionEnd, func(address uintptr) {
			windowStart := address
			if uint64(address-region.address) > ring0HostWorkingSetRadius {
				windowStart = address - uintptr(ring0HostWorkingSetRadius)
			} else {
				windowStart = region.address
			}
			windowEnd := address + uintptr(vmm.HostSlabSize) + uintptr(ring0HostWorkingSetRadius)
			if windowEnd < address || windowEnd > regionEnd {
				windowEnd = regionEnd
			}
			for slab := windowStart; slab < windowEnd; slab += uintptr(vmm.HostSlabSize) {
				addSlab(slab)
			}
		})
	}

	// Completely new stage-2 slabs can be installed before their stage-1
	// leaves. Replaced slabs must first lose all stage-1 references so HVF
	// never observes a live leaf while its backing VM object changes.
	var newSlabs, replacedOrRemoved []uintptr
	for address, desired := range desiredSlabs {
		old, mapped := e.stage2Slabs[address]
		if !mapped {
			newSlabs = append(newSlabs, address)
			continue
		}
		// A zero Mach object ID cannot prove identity across snapshots, so
		// replace it conservatively instead of retaining a potentially stale
		// HVF lease after host VA reuse.
		if desired.objectID == 0 || old.ring0Stage2Identity != desired {
			replacedOrRemoved = append(replacedOrRemoved, address)
		}
	}
	for address := range e.stage2Slabs {
		if _, desired := desiredSlabs[address]; !desired {
			replacedOrRemoved = append(replacedOrRemoved, address)
		}
	}
	skipped, err := e.mapHostStage2Slabs(newSlabs, desiredSlabs)
	if err != nil {
		return err
	}
	dropSkippedHostSlabs(skipped, desiredSlabs, desiredAccess)
	if len(replacedOrRemoved) != 0 {
		pages := make(map[uint64]struct{})
		for _, address := range replacedOrRemoved {
			for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
				if _, mapped := e.stage1Access[va]; mapped {
					pages[va] = struct{}{}
				}
			}
		}
		if err := e.applyHostStage1(ctx, hostStage1UnmapOperations(pages)); err != nil {
			return err
		}
		if err := e.unmapHostStage2Slabs(replacedOrRemoved); err != nil {
			return err
		}
		var replacements []uintptr
		for _, address := range replacedOrRemoved {
			if _, desired := desiredSlabs[address]; desired {
				replacements = append(replacements, address)
			}
		}
		skipped, err := e.mapHostStage2Slabs(replacements, desiredSlabs)
		if err != nil {
			return err
		}
		dropSkippedHostSlabs(skipped, desiredSlabs, desiredAccess)
	}
	desiredGPA := make(map[uint64]uint64, len(desiredAccess))
	for va := range desiredAccess {
		slabAddress := uintptr(va &^ (vmm.HostSlabSize - 1))
		slab, ok := e.stage2Slabs[slabAddress]
		if !ok {
			return fmt.Errorf("Sentry host VA %#x has no stage-2 slab", va)
		}
		desiredGPA[va] = slab.gpa + va - uint64(slabAddress)
	}
	if err := e.applyHostStage1(ctx, hostStage1ReconcileOperations(e.stage1Access, desiredAccess, desiredGPA)); err != nil {
		return err
	}
	// Only retire demand samples after their slab is live. A Mach snapshot race
	// may have caused an immutable slab to be skipped; preserving that sample
	// lets the next host-gate retry it.
	e.faultMu.Lock()
	for address := range pendingSlabs {
		if _, mapped := desiredSlabs[address]; mapped {
			delete(e.pendingMirror, address)
		}
	}
	e.faultMu.Unlock()
	return nil
}

func (e *ring0Engine) quiesceGuestContinuations(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		cpus := e.pinApplicationCPUs()
		active := 0
		var requestErr error
		for _, cpu := range cpus {
			if err := cpu.bridge.error(fmt.Sprintf("ring0 vCPU %d bridge failed", cpu.vcpu.id)); err != nil {
				requestErr = err
				break
			}
			if cpu.bridge.inGuest() && !cpu.bridge.destroyed() {
				active++
				if err := cpu.vcpu.RequestExit(); err != nil {
					requestErr = fmt.Errorf("requesting ring0 vCPU %d exit: %w", cpu.vcpu.id, err)
					break
				}
			}
		}
		e.unpinApplicationCPUs(cpus)
		if requestErr != nil {
			return requestErr
		}
		if active == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d guest continuations", active)
		}
		runtime.Gosched()
	}
}

func dropSkippedHostSlabs(addresses []uintptr, desiredSlabs map[uintptr]ring0Stage2Identity, desiredAccess map[uint64]monitorabi.Access) {
	for _, address := range addresses {
		delete(desiredSlabs, address)
		for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
			delete(desiredAccess, va)
		}
	}
}

func (e *ring0Engine) mapHostStage2Slabs(addresses []uintptr, desired map[uintptr]ring0Stage2Identity) ([]uintptr, error) {
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	var skipped []uintptr
	for start := 0; start < len(addresses); {
		identity := desired[addresses[start]]
		shadow := identity.access&vmm.Stage2Write == 0
		end := start + 1
		// Copy immutable non-image mappings one slab at a time. These mappings
		// are often short-lived runtime metadata; a per-slab Mach copy lets a
		// stale neighbor be discarded without losing the requested live slab.
		for !shadow && end < len(addresses) &&
			addresses[end] == addresses[end-1]+uintptr(vmm.HostSlabSize) &&
			desired[addresses[end]].access == identity.access &&
			desired[addresses[end]].access&vmm.Stage2Write != 0 {
			end++
		}
		address := addresses[start]
		length := uint64(end-start) * vmm.HostSlabSize
		if e.nextMirrorGPA > GuestRAMIPAStart-length {
			return skipped, fmt.Errorf("Sentry host mirror exhausted GPA aperture [%#x, %#x)", uint64(ring0HostMirrorGPAStart), uint64(GuestRAMIPAStart))
		}
		gpa := e.nextMirrorGPA
		hostAddress := address
		if shadow {
			var err error
			hostAddress, err = (hostMemory{}).Reserve(length)
			if err != nil {
				return skipped, fmt.Errorf("allocating Sentry shadow for [%#x, %#x): %w", address, address+uintptr(length), err)
			}
			if err := copyRing0HostMemory(hostAddress, address, length); err != nil {
				_ = (hostMemory{}).Release(hostAddress, length)
				skipped = append(skipped, addresses[start:end]...)
				start = end
				continue
			}
		}
		if err := e.vm.mapRing0Stage2Source(uintptr(gpa), hostAddress, length, identity.access); err != nil {
			if shadow {
				_ = (hostMemory{}).Release(hostAddress, length)
			}
			return skipped, fmt.Errorf("mapping Sentry stage-2 identity range [%#x, %#x): %w", address, address+uintptr(length), err)
		}
		e.nextMirrorGPA += length
		lease := &ring0Stage2Lease{hostAddress: hostAddress, length: length, shadow: shadow, refs: end - start}
		for index, mapped := range addresses[start:end] {
			e.stage2Slabs[mapped] = ring0Stage2Slab{
				ring0Stage2Identity: desired[mapped], lease: lease,
				gpa: gpa + uint64(index)*vmm.HostSlabSize,
			}
		}
		start = end
	}
	return skipped, nil
}

func (e *ring0Engine) unmapHostStage2Slabs(addresses []uintptr) error {
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	for start := 0; start < len(addresses); {
		end := start + 1
		for end < len(addresses) &&
			addresses[end] == addresses[end-1]+uintptr(vmm.HostSlabSize) &&
			e.stage2Slabs[addresses[end]].gpa == e.stage2Slabs[addresses[end-1]].gpa+vmm.HostSlabSize {
			end++
		}
		address := addresses[start]
		length := uint64(end-start) * vmm.HostSlabSize
		gpa := e.stage2Slabs[address].gpa
		if err := e.vm.unmapRing0Stage2GPA(gpa, length); err != nil {
			return fmt.Errorf("unmapping Sentry stage-2 range gpa=[%#x, %#x) host=[%#x, %#x): %w", gpa, gpa+length, address, address+uintptr(length), err)
		}
		var released []*ring0Stage2Lease
		for _, unmapped := range addresses[start:end] {
			lease := e.stage2Slabs[unmapped].lease
			delete(e.stage2Slabs, unmapped)
			if lease != nil {
				lease.refs--
				if lease.refs == 0 && lease.shadow {
					released = append(released, lease)
				}
			}
		}
		for _, lease := range released {
			if err := (hostMemory{}).Release(lease.hostAddress, lease.length); err != nil {
				return fmt.Errorf("releasing Sentry stage-2 shadow [%#x, %#x): %w", lease.hostAddress, lease.hostAddress+uintptr(lease.length), err)
			}
		}
		start = end
	}
	return nil
}

func (e *ring0Engine) releaseHostMirror() error {
	addresses := make([]uintptr, 0, len(e.stage2Slabs))
	for address := range e.stage2Slabs {
		addresses = append(addresses, address)
	}
	return e.unmapHostStage2Slabs(addresses)
}

func hostStage1UnmapOperations(pages map[uint64]struct{}) []monitorabi.Operation {
	addresses := make([]uint64, 0, len(pages))
	for address := range pages {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	operations := make([]monitorabi.Operation, 0, len(addresses))
	for start := 0; start < len(addresses); {
		end := start + 1
		for end < len(addresses) && addresses[end] == addresses[end-1]+vmm.LogicalPageSize {
			end++
		}
		operations = append(operations, monitorabi.Operation{
			Type: monitorabi.OperationUnmap, VA: addresses[start],
			Length: uint64(end-start) * vmm.LogicalPageSize,
		})
		start = end
	}
	return operations
}

func hostStage1ReconcileOperations(current, desired map[uint64]monitorabi.Access, desiredGPA map[uint64]uint64) []monitorabi.Operation {
	addresses := make([]uint64, 0, len(current)+len(desired))
	seen := make(map[uint64]struct{}, len(current)+len(desired))
	for address := range current {
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	for address := range desired {
		if _, ok := seen[address]; !ok {
			addresses = append(addresses, address)
		}
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	operations := make([]monitorabi.Operation, 0)
	for index := 0; index < len(addresses); {
		address := addresses[index]
		old, mapped := current[address]
		want, wanted := desired[address]
		var opType monitorabi.OperationType
		switch {
		case !wanted:
			opType = monitorabi.OperationUnmap
		case !mapped:
			opType = monitorabi.OperationMap
		case old != want:
			opType = monitorabi.OperationProtect
		default:
			index++
			continue
		}
		end := index + 1
		for end < len(addresses) && addresses[end] == addresses[end-1]+vmm.LogicalPageSize {
			next := addresses[end]
			nextOld, nextMapped := current[next]
			nextWant, nextWanted := desired[next]
			if opType == monitorabi.OperationUnmap && nextWanted ||
				opType == monitorabi.OperationMap && (nextMapped || !nextWanted || nextWant != want || desiredGPA[next] != desiredGPA[address]+uint64(end-index)*vmm.LogicalPageSize) ||
				opType == monitorabi.OperationProtect && (!nextMapped || !nextWanted || nextOld == nextWant || nextWant != want) {
				break
			}
			end++
		}
		operation := monitorabi.Operation{Type: opType, VA: address, Length: uint64(end-index) * vmm.LogicalPageSize}
		if opType != monitorabi.OperationUnmap {
			operation.Access = want
		}
		if opType == monitorabi.OperationMap {
			operation.GPA = desiredGPA[address]
		}
		operations = append(operations, operation)
		index = end
	}
	return operations
}

func (e *ring0Engine) applyHostStage1(ctx context.Context, operations []monitorabi.Operation) error {
	operations = splitHostStage1Operations(operations)
	for start := 0; start < len(operations); {
		end := start
		pages := 0
		for end < len(operations) && end-start < monitorabi.MaxOperations {
			operationPages := int(operations[end].Length / vmm.LogicalPageSize)
			if end != start && pages+operationPages > monitorabi.MaxOperations {
				break
			}
			pages += operationPages
			end++
		}
		batch := operations[start:end]
		if err := e.monitor.Execute(ctx, monitorabi.Command{
			Opcode: monitorabi.OpcodeApply, AddressSpaceID: ring0SentryAddressSpaceID,
			Operations: batch,
		}); err != nil {
			return fmt.Errorf("reconciling Sentry stage-1 identity mappings: %w", err)
		}
		for _, operation := range batch {
			for va := operation.VA; va < operation.VA+operation.Length; va += vmm.LogicalPageSize {
				if operation.Type == monitorabi.OperationUnmap {
					delete(e.stage1Access, va)
				} else {
					e.stage1Access[va] = operation.Access
				}
			}
		}
		start = end
	}
	return nil
}

func splitHostStage1Operations(operations []monitorabi.Operation) []monitorabi.Operation {
	const maxLength = uint64(monitorabi.MaxOperations) * vmm.LogicalPageSize
	result := make([]monitorabi.Operation, 0, len(operations))
	for _, operation := range operations {
		for offset := uint64(0); offset < operation.Length; offset += maxLength {
			part := operation
			part.VA += offset
			if part.Type == monitorabi.OperationMap {
				part.GPA += offset
			}
			part.Length = min(maxLength, operation.Length-offset)
			result = append(result, part)
		}
	}
	return result
}

func (e *ring0Engine) InterruptApplications() {
	cpus := e.pinApplicationCPUs()
	defer e.unpinApplicationCPUs(cpus)
	lastRequest := make(map[*ring0ApplicationCPU]time.Time, len(cpus))
	for _, cpu := range cpus {
		cancel := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))
		if cpu.applicationActive.Load() && !cpu.bridge.destroyed() {
			atomic.StoreUint64(cancel, 1)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		pending := false
		for _, cpu := range cpus {
			if !cpu.applicationActive.Load() || !cpu.bridge.inGuest() || cpu.bridge.destroyed() {
				continue
			}
			exitKind := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateExitKind))
			if atomic.LoadUint64(exitKind) != 0 {
				continue
			}
			pending = true
			if last := lastRequest[cpu]; last.IsZero() || time.Since(last) >= time.Millisecond {
				if err := cpu.vcpu.RequestExit(); err != nil {
					return
				}
				lastRequest[cpu] = time.Now()
			}
		}
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		runtime.Gosched()
	}
}

func (e *ring0Engine) ConcurrencyCount() int { return e.concurrency }

func (e *ring0Engine) Close() (retErr error) {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	e.cpuMu.Lock()
	if e.closed {
		retErr = e.closeErr
		e.cpuMu.Unlock()
		return retErr
	}
	e.closed = true
	e.cpuMu.Unlock()
	defer func() {
		e.monitorPool.Close()
		e.closeErr = retErr
	}()

	// A ring0 application vCPU remains inside hv_vcpu_run while its Sentry
	// continuation executes at EL1. Requesting an exit returns that continuation
	// through the owner thread's signal handler, which destroys the vCPU there.
	// Wait for the release acknowledgement before destroying the VM epoch.
	deadline := time.Now().Add(5 * time.Second)
	lastExitRequest := make(map[*ring0ApplicationCPU]time.Time)
	for {
		e.cpuMu.Lock()
		e.reapDestroyedCPUsLocked()
		remainingCount := e.applicationCPUCountLocked()
		e.cpuMu.Unlock()
		if remainingCount == 0 {
			break
		}
		remaining := e.pinApplicationCPUs()
		var requestErr error
		var waitDetail string
		for _, cpu := range remaining {
			if cpu.bridge.destroyed() {
				continue
			}
			if err := cpu.bridge.error(fmt.Sprintf("destroying owner-affine HVF ring0 vCPU %d after bridge failure", cpu.vcpu.id)); err != nil {
				requestErr = err
				break
			}
			state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
			applicationExited := binary.LittleEndian.Uint64(state[stateExitKind:]) != 0
			waitDetail = fmt.Sprintf("vCPU=%d in_guest=%t destroyed=%t cancellations=%d cancel_pc=%#x cancel_pstate=%#x application_exit=%d",
				cpu.vcpu.id, cpu.bridge.inGuest(), cpu.bridge.destroyed(),
				cpu.bridge.cancelCount(), cpu.bridge.lastCancelPC(),
				cpu.bridge.lastCancelPState(),
				binary.LittleEndian.Uint64(state[stateExitKind:]))
			lastRequest := lastExitRequest[cpu]
			// Avoid repeatedly winning hv_vcpu_run's cancellation race before
			// the injected EL0 IRQ can be delivered. Once the monitor has
			// published an application exit, request the second, EL1 redpill
			// promptly; otherwise retry only if the first request caught monitor
			// entry rather than the application.
			if !lastRequest.IsZero() && !applicationExited && time.Since(lastRequest) < 10*time.Millisecond {
				continue
			}
			if err := cpu.vcpu.RequestExit(); err != nil {
				requestErr = fmt.Errorf("requesting owner-affine HVF ring0 vCPU %d exit: %w", cpu.vcpu.id, err)
				break
			}
			lastExitRequest[cpu] = time.Now()
		}
		e.unpinApplicationCPUs(remaining)
		if requestErr != nil {
			return requestErr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d owner-affine HVF ring0 vCPUs to retire: %s", remainingCount, waitDetail)
		}
		time.Sleep(time.Millisecond)
	}
	// No new RunApplication can pass its closed check, and every prior reader
	// has now returned through its owner thread. Exclude any reconciliation
	// that began before closed was published while tearing down the mirror.
	e.entryMu.Lock()
	defer e.entryMu.Unlock()
	if err := e.releaseHostMirror(); err != nil {
		return err
	}
	return nil
}

var _ vmplatform.VMEngine = (*ring0Engine)(nil)
