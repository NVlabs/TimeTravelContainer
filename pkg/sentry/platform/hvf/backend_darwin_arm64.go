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

// Package hvf adapts Apple Hypervisor.framework to vmplatform's narrow VMM
// interfaces. Linux memory management and stage-1 page tables do not live in
// this package.
package hvf

/*
#cgo CFLAGS: -mmacosx-version-min=15.0
#cgo LDFLAGS: -framework Hypervisor -framework CoreFoundation -mmacosx-version-min=15.0
#include <Hypervisor/Hypervisor.h>
#include <CoreFoundation/CoreFoundation.h>
#include <libkern/OSCacheControl.h>
#include <stdint.h>
#include <sys/mman.h>
#include <string.h>

static void *ttc_mmap_anon(size_t length) {
	void *p = mmap(NULL, length, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	return p == MAP_FAILED ? NULL : p;
}

static int ttc_madvise_free(void *address, size_t length) {
	return madvise(address, length, MADV_DONTNEED);
}

// Anonymous Darwin mappings initially reference shared zero backing. A host
// write after hv_vm_map may COW the HVA without changing HVF's leased physical
// page. Force every logical subpage through a value-changing write fault
// before it can become stage-2 backing; restoring the original byte preserves
// allocator semantics while preventing zero-store elision or subpage laziness.
static void ttc_commit_host_pages(void *address, size_t length) {
	volatile uint8_t *bytes = (volatile uint8_t *)address;
	for (size_t offset = 0; offset < length; offset += 4096) {
		uint8_t original = bytes[offset];
		bytes[offset] = (uint8_t)(original ^ UINT8_C(0xff));
		bytes[offset] = original;
	}
}

static void ttc_invalidate_host_instruction_cache(void *address, size_t length) {
	sys_dcache_flush(address, length);
	sys_icache_invalidate(address, length);
}
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

// Backend implements vmplatform.Backend for Apple Silicon macOS 15+.
type Backend struct {
	hostState *hostMemoryState
}

// hostMemoryState connects VMM-owned host allocations to the Sentry mirror
// that may alias them. Go runtime VM hooks cover allocator arenas, but they do
// not observe GuestRAM's explicit mmap/madvise/munmap lifecycle.
type hostMemoryState struct {
	mu     sync.RWMutex
	engine *ring0Engine
}

func (s *hostMemoryState) register(engine *ring0Engine) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil && s.engine != engine {
		return fmt.Errorf("HVF host memory already has a Sentry mirror owner")
	}
	s.engine = engine
	return nil
}

func (s *hostMemoryState) unregister(engine *ring0Engine) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.engine == engine {
		s.engine = nil
	}
	s.mu.Unlock()
}

func (s *hostMemoryState) retire(address uintptr, length uint64) error {
	if s == nil {
		return nil
	}
	// Keep the engine alive and prevent Close from dismantling its monitor
	// while it removes the secondary alias.
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.engine == nil {
		return nil
	}
	return s.engine.retireHostMirrorRange(context.Background(), address, length)
}

// NewBackend returns the baseline Hypervisor.framework backend. It never asks
// for the macOS 26 4K IPA extension; stage-2 mappings are always 16 KiB.
func NewBackend() *Backend { return &Backend{hostState: &hostMemoryState{}} }

// HostMemory returns the 16 KiB-compatible anonymous host allocator used by
// GuestRAM.
func (b *Backend) HostMemory() vmm.HostMemory { return hostMemory{state: b.hostState} }

// CreateVM implements vmplatform.Backend.CreateVM.
func (b *Backend) CreateVM(context.Context) (vmm.VM, error) {
	config := C.hv_vm_config_create()
	if config == nil {
		return nil, fmt.Errorf("hv_vm_config_create failed")
	}
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	if status := C.hv_vm_config_set_ipa_size(config, 40); status != C.HV_SUCCESS {
		return nil, fmt.Errorf("hv_vm_config_set_ipa_size(40) failed: %d", status)
	}
	if status := C.hv_vm_create(config); status != C.HV_SUCCESS {
		return nil, fmt.Errorf("hv_vm_create failed: %d", status)
	}
	vm := &hvfVM{hostState: b.hostState}
	monitor, err := newMonitorMemory(vm)
	if err != nil {
		_ = C.hv_vm_destroy()
		return nil, fmt.Errorf("initializing EL1 monitor memory: %w", err)
	}
	vm.monitor = monitor
	return vm, nil
}

type hvfVM struct {
	mu        sync.Mutex
	monitor   *monitorMemory
	hostState *hostMemoryState
	stage2    map[uint64]hvfStage2Slab
	closed    bool
}

type hvfStage2Slab struct {
	host   uintptr
	access vmm.Stage2Access
}

// canonicalGPAForHost returns an existing VM-owned stage-2 lease for one host
// slab. Sentry identity mappings must reuse this GPA rather than mapping the
// same HVA a second time: HVF does not guarantee coherent access through two
// independently installed GPA aliases of mutable memory.
func (v *hvfVM) canonicalGPAForHost(host uintptr) (uint64, vmm.Stage2Access, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.monitor != nil {
		m := v.monitor
		for _, region := range []struct {
			host   uintptr
			gpa    uint64
			length uint64
			access vmm.Stage2Access
		}{
			{m.codeHost, monitorCodeGPA, monitorCodeCapacity, vmm.Stage2Read | vmm.Stage2Execute},
			{m.controlHost, monitorControlGPA, vmm.HostSlabSize, vmm.Stage2Read | vmm.Stage2Write},
			{m.statesHost, monitorStatesGPA, monitorStatesLength, vmm.Stage2Read | vmm.Stage2Write},
		} {
			if host >= region.host && uint64(host-region.host) < region.length {
				return region.gpa + uint64(host-region.host), region.access, true
			}
		}
		for _, pool := range m.pools {
			if host >= pool.host && uint64(host-pool.host) < pool.length {
				return pool.gpa + uint64(host-pool.host), vmm.Stage2Read | vmm.Stage2Write, true
			}
		}
	}
	for gpa, slab := range v.stage2 {
		if slab.host == host {
			return gpa, slab.access, true
		}
	}
	return 0, 0, false
}

func (v *hvfVM) MapStage2(gpa uint64, hostAddress uintptr, length uint64, access vmm.Stage2Access) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	if gpa < SentryMirrorIPAStart {
		return fmt.Errorf("HVF GPA %#x overlaps the private EL1 monitor range [0, %#x)", gpa, SentryMirrorIPAStart)
	}
	if err := v.mapStage2Locked(gpa, hostAddress, length, access); err != nil {
		return err
	}
	if v.stage2 == nil {
		v.stage2 = make(map[uint64]hvfStage2Slab)
	}
	for offset := uint64(0); offset < length; offset += vmm.HostSlabSize {
		v.stage2[gpa+offset] = hvfStage2Slab{host: hostAddress + uintptr(offset), access: access}
	}
	return nil
}

// mapStage2Locked maps monitor-private or GuestRAM memory. The caller owns
// v.mu, or is constructing the VM before it becomes externally visible.
func (v *hvfVM) mapStage2Locked(gpa uint64, hostAddress uintptr, length uint64, access vmm.Stage2Access) error {
	if gpa%vmm.HostSlabSize != 0 || uint64(hostAddress)%vmm.HostSlabSize != 0 || length == 0 || length%vmm.HostSlabSize != 0 {
		return fmt.Errorf("unaligned HVF stage-2 map gpa=%#x host=%#x length=%#x", gpa, hostAddress, length)
	}
	// hv_vm_map must never lease Darwin's shared anonymous zero backing. A
	// later host write could otherwise COW the HVA while HVF continues to fetch
	// from the original zero page. Enforce private physical backing at the
	// backend boundary so direct VMM callers receive the same invariant as
	// GuestRAM and monitor allocations.
	C.ttc_commit_host_pages(unsafe.Pointer(hostAddress), C.size_t(length))
	return mapNativeStage2(gpa, hostAddress, length, access, false)
}

func (v *hvfVM) UnmapStage2(gpa, length uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	if gpa < SentryMirrorIPAStart {
		return fmt.Errorf("HVF GPA %#x overlaps the private EL1 monitor range [0, %#x)", gpa, SentryMirrorIPAStart)
	}
	if err := v.unmapStage2Locked(gpa, length); err != nil {
		return err
	}
	for offset := uint64(0); offset < length; offset += vmm.HostSlabSize {
		delete(v.stage2, gpa+offset)
	}
	return nil
}

// invalidateGuestCode performs the writer-side Darwin cache operation for
// bytes populated through a host alias before EL1 publishes execute access.
// Guest-side DC/IC maintenance alone cannot clean a dirty host cache line on a
// different physical CPU.
func (v *hvfVM) invalidateGuestCode(gpa, length uint64) error {
	if length == 0 || gpa%vmm.LogicalPageSize != 0 || length%vmm.LogicalPageSize != 0 || gpa+length < gpa {
		return fmt.Errorf("invalid executable guest range [%#x, %#x)", gpa, gpa+length)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for current, end := gpa, gpa+length; current < end; {
		slabGPA := current &^ (vmm.HostSlabSize - 1)
		slab, ok := v.stage2[slabGPA]
		if !ok {
			return fmt.Errorf("executable guest GPA %#x has no tracked stage-2 slab", current)
		}
		offset := current - slabGPA
		chunk := vmm.HostSlabSize - offset
		if remaining := end - current; chunk > remaining {
			chunk = remaining
		}
		C.ttc_invalidate_host_instruction_cache(unsafe.Pointer(slab.host+uintptr(offset)), C.size_t(chunk))
		current += chunk
	}
	return nil
}

// unmapStage2Locked unmaps monitor-private or GuestRAM memory. The caller owns
// v.mu, or is rolling back VM construction before publication.
func (v *hvfVM) unmapStage2Locked(gpa, length uint64) error {
	if gpa%vmm.HostSlabSize != 0 || length == 0 || length%vmm.HostSlabSize != 0 {
		return fmt.Errorf("unaligned HVF stage-2 unmap gpa=%#x length=%#x", gpa, length)
	}
	return unmapNativeStage2(gpa, length, false)
}

func (v *hvfVM) CreateVCPU(id int) (vmm.VCPU, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, fmt.Errorf("HVF VM is closed")
	}
	var (
		vcpu C.hv_vcpu_t
		exit *C.hv_vcpu_exit_t
	)
	if status := C.hv_vcpu_create(&vcpu, &exit, nil); status != C.HV_SUCCESS {
		return nil, fmt.Errorf("hv_vcpu_create(%d) failed: %d", id, status)
	}
	stateHost, stateGPA, stateVA, err := v.monitor.state(id)
	if err != nil {
		_ = C.hv_vcpu_destroy(vcpu)
		return nil, err
	}
	return &hvfVCPU{
		id:        id,
		handle:    vcpu,
		exit:      exit,
		monitor:   v.monitor,
		stateHost: stateHost,
		stateGPA:  stateGPA,
		stateVA:   stateVA,
	}, nil
}

func (v *hvfVM) PrepareMonitorPoolExpansion(ctx context.Context) (vmm.MonitorPoolExpansion, error) {
	if err := ctx.Err(); err != nil {
		return vmm.MonitorPoolExpansion{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.monitor == nil {
		return vmm.MonitorPoolExpansion{}, fmt.Errorf("HVF VM is closed")
	}
	m := v.monitor
	if len(m.pools) == 0 || !m.pools[len(m.pools)-1].accepted {
		return vmm.MonitorPoolExpansion{}, fmt.Errorf("another monitor pool expansion is pending")
	}
	gpa := monitorPoolGPA + uint64(len(m.pools))*monitorPoolLength
	if gpa > SentryMirrorIPAStart-monitorPoolLength {
		return vmm.MonitorPoolExpansion{}, fmt.Errorf("private EL1 monitor pool aperture is exhausted")
	}
	host, err := (hostMemory{}).Reserve(monitorPoolLength)
	if err != nil {
		return vmm.MonitorPoolExpansion{}, err
	}
	if err := v.mapStage2Locked(gpa, host, monitorPoolLength, vmm.Stage2Read|vmm.Stage2Write); err != nil {
		_ = (hostMemory{}).Release(host, monitorPoolLength)
		return vmm.MonitorPoolExpansion{}, err
	}
	m.mapped = append(m.mapped, monitorStage2Mapping{gpa: gpa, length: monitorPoolLength})
	m.pools = append(m.pools, monitorPoolMemory{host: host, gpa: gpa, length: monitorPoolLength})
	return vmm.MonitorPoolExpansion{GPA: gpa, Length: monitorPoolLength}, nil
}

func (v *hvfVM) CompleteMonitorPoolExpansion(expansion vmm.MonitorPoolExpansion, accepted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.monitor == nil {
		return fmt.Errorf("HVF VM is closed")
	}
	m := v.monitor
	if len(m.pools) <= 1 {
		return fmt.Errorf("no monitor pool expansion is pending")
	}
	pool := &m.pools[len(m.pools)-1]
	if pool.accepted || pool.gpa != expansion.GPA || pool.length != expansion.Length {
		return fmt.Errorf("monitor pool expansion %#x/%#x is not pending", expansion.GPA, expansion.Length)
	}
	if accepted {
		pool.accepted = true
		return nil
	}
	if err := v.unmapStage2Locked(pool.gpa, pool.length); err != nil {
		return fmt.Errorf("rolling back monitor pool stage-2 lease: %w", err)
	}
	if err := (hostMemory{}).Release(pool.host, pool.length); err != nil {
		return fmt.Errorf("releasing rejected monitor pool memory: %w", err)
	}
	m.pools = m.pools[:len(m.pools)-1]
	for i := len(m.mapped) - 1; i >= 0; i-- {
		if m.mapped[i].gpa == expansion.GPA && m.mapped[i].length == expansion.Length {
			m.mapped = append(m.mapped[:i], m.mapped[i+1:]...)
			break
		}
	}
	return nil
}

func (v *hvfVM) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	var firstErr error
	if v.monitor != nil {
		if err := v.monitor.unmapLocked(v); err != nil {
			firstErr = err
		}
		v.monitor.releaseHost()
		v.monitor = nil
	}
	if status := C.hv_vm_destroy(); status != C.HV_SUCCESS {
		if firstErr == nil {
			firstErr = fmt.Errorf("hv_vm_destroy failed: %d", status)
		}
	}
	v.closed = true
	return firstErr
}

type hvfVCPU struct {
	mu     sync.Mutex
	id     int
	handle C.hv_vcpu_t
	exit   *C.hv_vcpu_exit_t

	monitor   *monitorMemory
	stateHost uintptr
	stateGPA  uint64
	stateVA   uint64
	booted    bool

	closed bool
}

func (v *hvfVCPU) RequestExit() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	if status := C.hv_vcpus_exit(&v.handle, 1); status != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpus_exit failed: %d", status)
	}
	return nil
}

func (v *hvfVCPU) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	if status := C.hv_vcpu_destroy(v.handle); status != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_destroy failed: %d", status)
	}
	v.closed = true
	return nil
}

// closeStaleOwner retires a vCPU after an architectural CurrentEL probe has
// proven that its recorded guest residency is stale on the owning host thread.
// The one-shot ring0 bridge may already have destroyed the HVF object before
// publishing its final bookkeeping through signal return; HV_BAD_ARGUMENT is
// idempotent only in this narrowly qualified path.
func (v *hvfVCPU) closeStaleOwner() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	status := C.hv_vcpu_destroy(v.handle)
	if status != C.HV_SUCCESS && status != C.HV_BAD_ARGUMENT {
		return fmt.Errorf("hv_vcpu_destroy failed: %d", status)
	}
	v.closed = true
	return nil
}

type hostMemory struct {
	state *hostMemoryState
}

func (hostMemory) Reserve(length uint64) (uintptr, error) {
	if length == 0 || length%vmm.HostSlabSize != 0 {
		return 0, fmt.Errorf("invalid HVF host reservation length %#x", length)
	}
	address := C.ttc_mmap_anon(C.size_t(length))
	if address == nil {
		return 0, fmt.Errorf("mmap anonymous reservation of %#x bytes failed", length)
	}
	return uintptr(address), nil
}

func (hostMemory) Commit(address uintptr, length uint64) error {
	if err := validateHostRange(address, length); err != nil {
		return err
	}
	C.ttc_commit_host_pages(unsafe.Pointer(address), C.size_t(length))
	return nil
}

func (h hostMemory) Decommit(address uintptr, length uint64) error {
	if err := validateHostRange(address, length); err != nil {
		return err
	}
	if err := h.state.retire(address, length); err != nil {
		return fmt.Errorf("retiring Sentry alias before host decommit: %w", err)
	}
	if result := C.ttc_madvise_free(unsafe.Pointer(address), C.size_t(length)); result != 0 {
		return fmt.Errorf("madvise(MADV_DONTNEED, %#x, %#x) failed", address, length)
	}
	return nil
}

func (hostMemory) Zero(address uintptr, length uint64) error {
	if address%uintptr(vmm.LogicalPageSize) != 0 || length == 0 || length%vmm.LogicalPageSize != 0 {
		return fmt.Errorf("invalid GuestRAM zero range host=%#x length=%#x", address, length)
	}
	C.memset(unsafe.Pointer(address), 0, C.size_t(length))
	return nil
}

func (hostMemory) Copy(dst, src uintptr, length uint64) error {
	if dst%uintptr(vmm.LogicalPageSize) != 0 || src%uintptr(vmm.LogicalPageSize) != 0 || length == 0 || length%vmm.LogicalPageSize != 0 {
		return fmt.Errorf("invalid GuestRAM copy range dst=%#x src=%#x length=%#x", dst, src, length)
	}
	C.memmove(unsafe.Pointer(dst), unsafe.Pointer(src), C.size_t(length))
	return nil
}

func (h hostMemory) Release(address uintptr, length uint64) error {
	if err := validateHostRange(address, length); err != nil {
		return err
	}
	if err := h.state.retire(address, length); err != nil {
		return fmt.Errorf("retiring Sentry alias before host release: %w", err)
	}
	if result := C.munmap(unsafe.Pointer(address), C.size_t(length)); result != 0 {
		return fmt.Errorf("munmap(%#x, %#x) failed", address, length)
	}
	return nil
}

func validateHostRange(address uintptr, length uint64) error {
	if address%uintptr(vmm.HostSlabSize) != 0 || length == 0 || length%vmm.HostSlabSize != 0 {
		return fmt.Errorf("invalid 16K host range address=%#x length=%#x", address, length)
	}
	return nil
}
