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

/*
#cgo CFLAGS: -mmacosx-version-min=15.0
#cgo LDFLAGS: -framework Hypervisor -framework CoreFoundation -mmacosx-version-min=15.0
#include <Hypervisor/Hypervisor.h>
#include <mach/mach_time.h>
#include <stdint.h>
#include <string.h>

#include "pkg/sentry/platform/hvf/monitor/monitor_layout.h"

static void ttc_monitor_init_control(
		void *memory,
		uint64_t code_gpa, uint64_t code_va, uint64_t code_length,
		uint64_t control_gpa, uint64_t control_va,
		uint64_t states_gpa, uint64_t states_va, uint64_t state_count,
		uint64_t pool_gpa, uint64_t pool_va, uint64_t pool_length,
		uint64_t pool_aperture_length) {
	struct ttc_monitor_control *ctl = (struct ttc_monitor_control *)memory;
	memset(ctl, 0, sizeof(*ctl));
	ctl->magic = TTC_MONITOR_MAGIC;
	ctl->version = TTC_MONITOR_VERSION;
	ctl->code_gpa = code_gpa;
	ctl->code_va = code_va;
	ctl->code_length = code_length;
	ctl->control_gpa = control_gpa;
	ctl->control_va = control_va;
	ctl->states_gpa = states_gpa;
	ctl->states_va = states_va;
	ctl->state_count = state_count;
	ctl->next_asid = 1;
	ctl->pool_region_count = 1;
	ctl->pool_aperture_length = pool_aperture_length;
	ctl->pool[0].gpa = pool_gpa;
	ctl->pool[0].va = pool_va;
	ctl->pool[0].length = pool_length;
	ctl->next_pool_va = pool_va + pool_length;
}

static uint64_t ttc_monitor_ttbr1(const void *memory) {
	return ((const struct ttc_monitor_control *)memory)->ttbr1_gpa;
}

static void ttc_monitor_init_state(void *memory, uint64_t control_va,
				   uint64_t control_gpa) {
	memset(memory, 0, TTC_MONITOR_HOST_SLAB_SIZE);
	uint8_t *state = (uint8_t *)memory;
	memcpy(state + TTC_STATE_CONTROL_VA_OFFSET, &control_va, sizeof(control_va));
	memcpy(state + TTC_STATE_CONTROL_GPA_OFFSET, &control_gpa, sizeof(control_gpa));
}

static hv_return_t ttc_monitor_set_boot_registers(hv_vcpu_t vcpu,
						   uint64_t pc,
						   uint64_t control_gpa,
						   uint64_t stack_gpa) {
	hv_return_t ret;
	uint64_t sctlr = 0;
	if ((ret = hv_vcpu_get_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, &sctlr)) != HV_SUCCESS) return ret;
	sctlr &= ~(uint64_t)0x1005; // M, C, I.
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, sctlr)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR0_EL1, 0)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR1_EL1, 0)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SP_EL1, stack_gpa)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_X0, control_gpa)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_PC, pc)) != HV_SUCCESS) return ret;
	return hv_vcpu_set_reg(vcpu, HV_REG_CPSR, 0x3c5); // EL1h, DAIF masked.
}

static hv_return_t ttc_monitor_enable_mmu(hv_vcpu_t vcpu, uint64_t ttbr1,
					   uint64_t vector_va,
					   uint64_t state_va) {
	hv_return_t ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TCR_EL1, TTC_GUEST_TCR_EL1)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_MAIR_EL1, 0xff)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR1_EL1, ttbr1)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR0_EL1, 0)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_VBAR_EL1, vector_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TPIDR_EL1, state_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SP_EL1, state_va + TTC_STATE_STACK_TOP)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_CPACR_EL1, 3ULL << 20)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_CNTKCTL_EL1, 2)) != HV_SUCCESS) return ret;
	// Keep debug exceptions in the guest so the EL1 monitor can preserve the
	// architectural ESR/ELR. Application entry mirrors PSTATE.SS into
	// MDSCR_EL1.SS; initialize it disabled so ordinary bootstrap execution
	// cannot enter the software-step state machine.
	if ((ret = hv_vcpu_set_trap_debug_exceptions(vcpu, false)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_MDSCR_EL1, 0)) != HV_SUCCESS) return ret;
	return hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, TTC_GUEST_SCTLR_EL1);
}

static hv_return_t ttc_monitor_set_entry(hv_vcpu_t vcpu, uint64_t pc,
					  uint64_t state_va) {
	hv_return_t ret;
	// Re-establish the complete per-vCPU EL1 execution context on every host
	// entry. These registers are monitor-owned execution cache; relying on HVF
	// to retain a prior entry's TPIDR/SP lets one worker's exception save land
	// in another worker's shared state after repeated vCPU scheduling.
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TPIDR_EL1, state_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SP_EL1, state_va + TTC_STATE_STACK_TOP)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_X0, state_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_PC, pc)) != HV_SUCCESS) return ret;
	return hv_vcpu_set_reg(vcpu, HV_REG_CPSR, 0x3c5);
}

static uint32_t ttc_monitor_exit_reason(const hv_vcpu_exit_t *exit) {
	return exit->reason;
}

static uint64_t ttc_monitor_exit_syndrome(const hv_vcpu_exit_t *exit) {
	return exit->exception.syndrome;
}

static void ttc_monitor_fence(void) {
	__sync_synchronize();
}

static uint64_t ttc_monitor_host_cntvct(void) {
	uint64_t value;
	__asm__ volatile("isb\n\tmrs %0, cntvct_el0" : "=r"(value));
	return value;
}

// Hypervisor.framework defines guest CNTVCT as mach_absolute_time minus this
// offset. Sentry's clock calibration is created in host EL0 from the host
// architectural counter, whose epoch is not required to match Mach absolute
// time. Choose the tightest of several bracketed samples so every vCPU in this
// VM observes the host CNTVCT domain with only bounded sampling error.
static uint64_t ttc_monitor_vtimer_offset(void) {
	uint64_t best_span = UINT64_MAX;
	uint64_t best_offset = 0;
	for (unsigned int i = 0; i < 16; ++i) {
		uint64_t before = ttc_monitor_host_cntvct();
		uint64_t absolute = mach_absolute_time();
		uint64_t after = ttc_monitor_host_cntvct();
		uint64_t span = after - before;
		if (span < best_span) {
			uint64_t midpoint = before + span / 2;
			best_span = span;
			best_offset = absolute - midpoint;
		}
	}
	return best_offset;
}

static hv_return_t ttc_monitor_cancel_location(hv_vcpu_t vcpu,
						uint64_t *cpsr, uint64_t *pc) {
	hv_return_t ret;
	if ((ret = hv_vcpu_get_reg(vcpu, HV_REG_CPSR, cpsr)) != HV_SUCCESS) return ret;
	return hv_vcpu_get_reg(vcpu, HV_REG_PC, pc);
}

*/
import "C"

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

const (
	monitorCodeGPA                = uint64(0x00010000)
	monitorCodeCapacity           = uint64(0x00010000)
	monitorControlGPA             = uint64(0x00020000)
	monitorStatesGPA              = uint64(0x00024000)
	monitorMaxVCPUs               = 64
	monitorStatesLength           = uint64(monitorMaxVCPUs) * vmm.HostSlabSize
	monitorPoolGPA                = uint64(0x00200000)
	monitorPoolLength             = uint64(16 << 20)
	monitorCodeVA                 = uint64(0xffff000000000000)
	monitorControlVA              = monitorCodeVA + monitorCodeCapacity
	monitorStatesVA               = monitorControlVA + vmm.HostSlabSize
	monitorPoolVA                 = monitorCodeVA + 0x00200000
	monitorCommandMax             = 64 + 256*40
	stateCommandLength            = 0x0010
	stateCommand                  = 0x0040
	stateResponse                 = 0x2880
	stateAddressSpaceID           = 0x2900
	stateX0                       = 0x2908
	stateSP                       = 0x2a00
	statePC                       = 0x2a08
	statePState                   = 0x2a10
	stateTPIDR                    = 0x2a18
	stateESR                      = 0x2a20
	stateFAR                      = 0x2a28
	stateExitKind                 = 0x2a30
	stateInternalESR              = 0x2a38
	stateInternalFAR              = 0x2a40
	stateInternalELR              = 0x2a48
	stateInternalVector           = 0x2a50
	stateInternalPState           = 0x2a58
	stateCancelRequested          = 0x2a60
	statePendingInterruptExitKind = 0x2a70
	stateSentrySP                 = 0x2b80
	stateSentryPC                 = 0x2b88
	stateHostGatePC               = 0x3300
	stateHostGatePState           = 0x3308
	stateHostGateESR              = 0x3310
	stateHostGateFAR              = 0x3318
	monitorHVCBoot                = 0x70
	monitorHVCCommand             = 0x71
	monitorHVCInternal            = 0x7f
	arm64SPSRAlwaysRES0           = uint64(0xffffffff0cdfe020)
)

func validApplicationPState(pstate uint64) bool {
	return pstate&arm64SPSRAlwaysRES0 == 0 && pstate&0xf == 0 && pstate&0x3c0 == 0
}

// SentryMirrorIPAStart is the first GPA outside the monitor's private code,
// control, state, and page-table-pool mappings. The VM engine uses the range
// from here to GuestRAMIPAStart as a disposable mirror of its host Sentry
// continuation.
const SentryMirrorIPAStart = uint64(0x10000000)

// GuestRAMIPAStart partitions the 40-bit IPA space. GuestRAM occupies the
// upper half and cannot collide with the disposable Sentry execution mirror.
const GuestRAMIPAStart = uint64(1 << 39)

type monitorMemory struct {
	bootMu sync.Mutex

	codeHost     uintptr
	controlHost  uintptr
	statesHost   uintptr
	poolHost     uintptr
	codeLength   uint64
	vtimerOffset uint64

	bootstrapOffset        uint64
	commandCallOffset      uint64
	commandOffset          uint64
	ring0ApplicationOffset uint64
	vectorOffset           uint64

	mapped []monitorStage2Mapping
	pools  []monitorPoolMemory
}

type monitorStage2Mapping struct {
	gpa    uint64
	length uint64
}

type monitorPoolMemory struct {
	host     uintptr
	gpa      uint64
	length   uint64
	accepted bool
}

func newMonitorMemory(vm *hvfVM) (*monitorMemory, error) {
	imageLength := uint64(len(embeddedMonitorImage))
	if imageLength == 0 || imageLength > monitorCodeCapacity {
		return nil, fmt.Errorf("invalid embedded EL1 monitor image length %d", imageLength)
	}
	m := &monitorMemory{
		codeLength:   roundUp(imageLength, vmm.HostSlabSize),
		vtimerOffset: uint64(C.ttc_monitor_vtimer_offset()),
	}
	m.bootstrapOffset = embeddedMonitorBootstrapOffset
	m.commandCallOffset = embeddedMonitorCommandCallOffset
	m.commandOffset = embeddedMonitorCommandOffset
	m.ring0ApplicationOffset = embeddedMonitorRing0ApplicationOffset
	m.vectorOffset = embeddedMonitorVectorOffset
	for name, offset := range map[string]uint64{
		"bootstrap":         m.bootstrapOffset,
		"command-call":      m.commandCallOffset,
		"command":           m.commandOffset,
		"ring0-application": m.ring0ApplicationOffset,
		"vectors":           m.vectorOffset,
	} {
		if offset >= imageLength {
			return nil, fmt.Errorf("EL1 monitor %s offset %#x exceeds image length %#x", name, offset, imageLength)
		}
	}

	alloc := hostMemory{}
	var err error
	if m.codeHost, err = alloc.Reserve(monitorCodeCapacity); err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = m.unmapLocked(vm)
			m.releaseHost()
		}
	}()
	if m.controlHost, err = alloc.Reserve(vmm.HostSlabSize); err != nil {
		return nil, err
	}
	if m.statesHost, err = alloc.Reserve(monitorStatesLength); err != nil {
		return nil, err
	}
	if m.poolHost, err = alloc.Reserve(monitorPoolLength); err != nil {
		return nil, err
	}
	for _, region := range []struct {
		address uintptr
		length  uint64
	}{
		{m.codeHost, monitorCodeCapacity},
		{m.controlHost, vmm.HostSlabSize},
		{m.statesHost, monitorStatesLength},
		{m.poolHost, monitorPoolLength},
	} {
		if err := alloc.Commit(region.address, region.length); err != nil {
			return nil, fmt.Errorf("committing EL1 monitor host memory: %w", err)
		}
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(m.codeHost)), int(imageLength)), embeddedMonitorImage[:])

	if err := vm.mapStage2Locked(monitorCodeGPA, m.codeHost, monitorCodeCapacity, vmm.Stage2Read|vmm.Stage2Execute); err != nil {
		return nil, err
	}
	m.mapped = append(m.mapped, monitorStage2Mapping{monitorCodeGPA, monitorCodeCapacity})
	if err := vm.mapStage2Locked(monitorControlGPA, m.controlHost, vmm.HostSlabSize, vmm.Stage2Read|vmm.Stage2Write); err != nil {
		return nil, err
	}
	m.mapped = append(m.mapped, monitorStage2Mapping{monitorControlGPA, vmm.HostSlabSize})
	if err := vm.mapStage2Locked(monitorStatesGPA, m.statesHost, monitorStatesLength, vmm.Stage2Read|vmm.Stage2Write); err != nil {
		return nil, err
	}
	m.mapped = append(m.mapped, monitorStage2Mapping{monitorStatesGPA, monitorStatesLength})
	if err := vm.mapStage2Locked(monitorPoolGPA, m.poolHost, monitorPoolLength, vmm.Stage2Read|vmm.Stage2Write); err != nil {
		return nil, err
	}
	m.mapped = append(m.mapped, monitorStage2Mapping{monitorPoolGPA, monitorPoolLength})
	m.pools = append(m.pools, monitorPoolMemory{
		host:     m.poolHost,
		gpa:      monitorPoolGPA,
		length:   monitorPoolLength,
		accepted: true,
	})
	C.ttc_monitor_init_control(
		unsafe.Pointer(m.controlHost),
		C.uint64_t(monitorCodeGPA), C.uint64_t(monitorCodeVA), C.uint64_t(m.codeLength),
		C.uint64_t(monitorControlGPA), C.uint64_t(monitorControlVA),
		C.uint64_t(monitorStatesGPA), C.uint64_t(monitorStatesVA), C.uint64_t(monitorMaxVCPUs),
		C.uint64_t(monitorPoolGPA), C.uint64_t(monitorPoolVA), C.uint64_t(monitorPoolLength),
		C.uint64_t(SentryMirrorIPAStart-monitorPoolGPA),
	)
	cleanup = false
	return m, nil
}

func (m *monitorMemory) unmapLocked(vm *hvfVM) error {
	var firstErr error
	for i := len(m.mapped) - 1; i >= 0; i-- {
		mapping := m.mapped[i]
		if err := vm.unmapStage2Locked(mapping.gpa, mapping.length); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.mapped = nil
	return firstErr
}

func (m *monitorMemory) releaseHost() {
	alloc := hostMemory{}
	for i := len(m.pools) - 1; i >= 0; i-- {
		pool := m.pools[i]
		if pool.host != 0 {
			_ = alloc.Release(pool.host, pool.length)
		}
	}
	m.pools = nil
	m.poolHost = 0
	if m.statesHost != 0 {
		_ = alloc.Release(m.statesHost, monitorStatesLength)
		m.statesHost = 0
	}
	if m.controlHost != 0 {
		_ = alloc.Release(m.controlHost, vmm.HostSlabSize)
		m.controlHost = 0
	}
	if m.codeHost != 0 {
		_ = alloc.Release(m.codeHost, monitorCodeCapacity)
		m.codeHost = 0
	}
}

func (m *monitorMemory) state(id int) (host uintptr, gpa, va uint64, err error) {
	if id < 0 || id >= monitorMaxVCPUs {
		return 0, 0, 0, fmt.Errorf("HVF vCPU id %d exceeds monitor state capacity", id)
	}
	offset := uint64(id) * vmm.HostSlabSize
	return m.statesHost + uintptr(offset), monitorStatesGPA + offset, monitorStatesVA + offset, nil
}

func (m *monitorMemory) ttbr1() uint64 {
	return uint64(C.ttc_monitor_ttbr1(unsafe.Pointer(m.controlHost)))
}

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}

func (v *hvfVCPU) ensureBooted() error {
	v.monitor.bootMu.Lock()
	defer v.monitor.bootMu.Unlock()
	if v.booted {
		return nil
	}
	// Keep CNTVCT_EL0 in the host architectural-counter domain used by
	// Sentry's calibrated clocks. Hypervisor.framework otherwise exposes the
	// mach_absolute_time domain, whose epoch differs on supported Apple
	// Silicon systems. One VM-wide offset also prevents cross-vCPU skew.
	if status := C.hv_vcpu_set_vtimer_offset(v.handle, C.uint64_t(v.monitor.vtimerOffset)); status != C.HV_SUCCESS {
		return fmt.Errorf("setting EL1 monitor VTimer offset: %d", status)
	}
	C.ttc_monitor_init_state(unsafe.Pointer(v.stateHost), C.uint64_t(monitorControlVA), C.uint64_t(monitorControlGPA))
	if status := C.ttc_monitor_set_boot_registers(v.handle,
		C.uint64_t(monitorCodeGPA+v.monitor.bootstrapOffset),
		C.uint64_t(monitorControlGPA),
		C.uint64_t(v.stateGPA+vmm.HostSlabSize)); status != C.HV_SUCCESS {
		return fmt.Errorf("setting EL1 monitor bootstrap registers: %d", status)
	}
	if err := v.runUntilHVC(context.Background(), monitorHVCBoot, false, 0, 0); err != nil {
		return fmt.Errorf("booting EL1 monitor: %w", err)
	}
	ttbr1 := v.monitor.ttbr1()
	if ttbr1 == 0 {
		return fmt.Errorf("EL1 monitor bootstrap returned an empty TTBR1")
	}
	if status := C.ttc_monitor_enable_mmu(v.handle, C.uint64_t(ttbr1),
		C.uint64_t(monitorCodeVA+v.monitor.vectorOffset), C.uint64_t(v.stateVA)); status != C.HV_SUCCESS {
		return fmt.Errorf("enabling EL1 monitor MMU: %d", status)
	}
	C.hv_vcpu_set_vtimer_mask(v.handle, C.bool(true))
	v.booted = true
	return nil
}

// runUntilHVC runs the vCPU until the monitor issues the expected HVC. If
// pauseGuests is set, application vCPUs are paused meanwhile, and asked for
// the local TLB maintenance tlbFlush for addressSpaceID before they next
// enter.
func (v *hvfVCPU) runUntilHVC(ctx context.Context, expected uint64, pauseGuests bool, tlbFlush, addressSpaceID uint64) error {
	for {
		C.ttc_monitor_fence()
		status := int32(C.HV_SUCCESS)
		if pauseGuests {
			status = runNativeMonitorPaused(uint64(v.handle), unsafe.Pointer(v.exit), tlbFlush, addressSpaceID)
		} else {
			status = int32(C.hv_vcpu_run(v.handle))
		}
		C.ttc_monitor_fence()
		if status != int32(C.HV_SUCCESS) {
			return fmt.Errorf("hv_vcpu_run failed: %d", status)
		}
		switch C.ttc_monitor_exit_reason(v.exit) {
		case C.HV_EXIT_REASON_EXCEPTION:
			syndrome := uint64(C.ttc_monitor_exit_syndrome(v.exit))
			if syndrome>>26&0x3f != 0x16 {
				return fmt.Errorf("EL1 monitor unexpected exception syndrome %#x", syndrome)
			}
			immediate := syndrome & 0xffff
			if immediate == monitorHVCInternal {
				var cpsr, pc C.uint64_t
				if status := C.ttc_monitor_cancel_location(v.handle, &cpsr, &pc); status == C.HV_SUCCESS {
					state := unsafe.Slice((*byte)(unsafe.Pointer(v.stateHost)), vmm.HostSlabSize)
					return fmt.Errorf("EL1 monitor reported an internal failure at CPSR %#x PC %#x: vector=%d ESR=%#x FAR=%#x ELR=%#x input-PSTATE=%#x",
						uint64(cpsr), uint64(pc),
						binary.LittleEndian.Uint64(state[stateInternalVector:]),
						binary.LittleEndian.Uint64(state[stateInternalESR:]),
						binary.LittleEndian.Uint64(state[stateInternalFAR:]),
						binary.LittleEndian.Uint64(state[stateInternalELR:]),
						binary.LittleEndian.Uint64(state[stateInternalPState:]))
				}
				return fmt.Errorf("EL1 monitor reported an internal failure")
			}
			if immediate != expected {
				return fmt.Errorf("EL1 monitor HVC %#x, want %#x", immediate, expected)
			}
			return nil
		case C.HV_EXIT_REASON_CANCELED:
			// Monitor transactions must reach their synchronous acknowledgement
			// even if their caller is canceled; resume at the interrupted PC.
			continue
		case C.HV_EXIT_REASON_VTIMER_ACTIVATED:
			C.hv_vcpu_set_vtimer_mask(v.handle, C.bool(true))
			continue
		default:
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("EL1 monitor unexpected HVF exit reason %d", C.ttc_monitor_exit_reason(v.exit))
		}
	}
}

func (v *hvfVCPU) RunMonitor(ctx context.Context, encoded []byte) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > monitorCommandMax {
		return nil, fmt.Errorf("invalid EL1 monitor command length %d", len(encoded))
	}
	if err := v.ensureBooted(); err != nil {
		return nil, err
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(v.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint32(state[stateCommandLength:], uint32(len(encoded)))
	copy(state[stateCommand:stateCommand+len(encoded)], encoded)
	clear(state[stateResponse : stateResponse+32])
	if status := C.ttc_monitor_set_entry(v.handle,
		C.uint64_t(monitorCodeVA+v.monitor.commandOffset), C.uint64_t(v.stateVA)); status != C.HV_SUCCESS {
		return nil, fmt.Errorf("setting EL1 monitor command entry: %d", status)
	}
	err := v.runUntilHVC(ctx, monitorHVCCommand, true, monitorCommandTLBFlush(encoded), monitorCommandAddressSpace(encoded))
	if err != nil {
		return nil, err
	}
	response := make([]byte, 32)
	copy(response, state[stateResponse:stateResponse+32])
	return response, nil
}
