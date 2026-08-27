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
#include <stdint.h>
#include <string.h>

#include "pkg/sentry/platform/hvf/monitor/monitor_layout.h"

static void ttc_monitor_init_control(
		void *memory,
		uint64_t code_gpa, uint64_t code_va, uint64_t code_length,
		uint64_t control_gpa, uint64_t control_va,
		uint64_t states_gpa, uint64_t states_va, uint64_t state_count,
		uint64_t pool_gpa, uint64_t pool_va, uint64_t pool_length,
		uint64_t pool_aperture_length,
		uint64_t command_entry_va, uint64_t application_entry_va,
		uint64_t vector_va) {
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
	ctl->command_entry_va = command_entry_va;
	ctl->application_entry_va = application_entry_va;
	ctl->vector_va = vector_va;
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
	// 48-bit TTBR0/TTBR1, 4K granules, inner-shareable WBWA, 40-bit IPA,
	// and 16-bit ASIDs.
	uint64_t tcr = 16ULL | (1ULL << 8) | (1ULL << 10) | (3ULL << 12) |
		(16ULL << 16) | (1ULL << 24) | (1ULL << 26) | (3ULL << 28) |
		(2ULL << 30) | (2ULL << 32) | (1ULL << 36);
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TCR_EL1, tcr)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_MAIR_EL1, 0xff)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR1_EL1, ttbr1)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TTBR0_EL1, 0)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_VBAR_EL1, vector_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_TPIDR_EL1, state_va)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SP_EL1, state_va + TTC_STATE_STACK_TOP)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_CPACR_EL1, 3ULL << 20)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_CNTKCTL_EL1, 2)) != HV_SUCCESS) return ret;
	return hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, 0x34909185);
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

static hv_return_t ttc_monitor_load_fp(hv_vcpu_t vcpu, const uint8_t *fp) {
	uint32_t fpsr, fpcr;
	hv_return_t ret;
	memcpy(&fpsr, fp, sizeof(fpsr));
	memcpy(&fpcr, fp + 4, sizeof(fpcr));
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_FPSR, fpsr)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_set_reg(vcpu, HV_REG_FPCR, fpcr)) != HV_SUCCESS) return ret;
	for (int i = 0; i < 32; ++i) {
		hv_simd_fp_uchar16_t value;
		memcpy(&value, fp + 8 + i * 16, 16);
		if ((ret = hv_vcpu_set_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), value)) != HV_SUCCESS) return ret;
	}
	return HV_SUCCESS;
}

static hv_return_t ttc_monitor_save_fp(hv_vcpu_t vcpu, uint8_t *fp) {
	uint64_t fpsr = 0, fpcr = 0;
	hv_return_t ret;
	if ((ret = hv_vcpu_get_reg(vcpu, HV_REG_FPSR, &fpsr)) != HV_SUCCESS) return ret;
	if ((ret = hv_vcpu_get_reg(vcpu, HV_REG_FPCR, &fpcr)) != HV_SUCCESS) return ret;
	memcpy(fp, &fpsr, 4);
	memcpy(fp + 4, &fpcr, 4);
	for (int i = 0; i < 32; ++i) {
		hv_simd_fp_uchar16_t value;
		if ((ret = hv_vcpu_get_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), &value)) != HV_SUCCESS) return ret;
		memcpy(fp + 8 + i * 16, &value, 16);
	}
	return HV_SUCCESS;
}

static hv_return_t ttc_monitor_cancel_location(hv_vcpu_t vcpu,
						uint64_t *cpsr, uint64_t *pc) {
	hv_return_t ret;
	if ((ret = hv_vcpu_get_reg(vcpu, HV_REG_CPSR, cpsr)) != HV_SUCCESS) return ret;
	return hv_vcpu_get_reg(vcpu, HV_REG_PC, pc);
}

static hv_return_t ttc_monitor_cancel_pc(hv_vcpu_t vcpu, uint64_t *pc) {
	return hv_vcpu_get_reg(vcpu, HV_REG_PC, pc);
}
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

const (
	monitorCodeGPA        = uint64(0x00010000)
	monitorCodeCapacity   = uint64(0x00010000)
	monitorControlGPA     = uint64(0x00020000)
	monitorStatesGPA      = uint64(0x00024000)
	monitorMaxVCPUs       = 64
	monitorStatesLength   = uint64(monitorMaxVCPUs) * vmm.HostSlabSize
	monitorPoolGPA        = uint64(0x00200000)
	monitorPoolLength     = uint64(16 << 20)
	monitorCodeVA         = uint64(0xffff000000000000)
	monitorControlVA      = monitorCodeVA + monitorCodeCapacity
	monitorStatesVA       = monitorControlVA + vmm.HostSlabSize
	monitorPoolVA         = monitorCodeVA + 0x00200000
	monitorCommandMax     = 64 + 256*40
	stateCommandLength    = 0x0010
	stateCommand          = 0x0040
	stateResponse         = 0x2880
	stateAddressSpaceID   = 0x2900
	stateX0               = 0x2908
	stateSP               = 0x2a00
	statePC               = 0x2a08
	statePState           = 0x2a10
	stateTPIDR            = 0x2a18
	stateESR              = 0x2a20
	stateFAR              = 0x2a28
	stateExitKind         = 0x2a30
	stateInternalESR      = 0x2a38
	stateInternalFAR      = 0x2a40
	stateInternalELR      = 0x2a48
	stateInternalVector   = 0x2a50
	stateInternalPState   = 0x2a58
	stateHostInputPState  = 0x2a60
	stateHostInputPC      = 0x2a68
	stateHostInputSpace   = 0x2a70
	monitorHVCBoot        = 0x70
	monitorHVCCommand     = 0x71
	monitorHVCApplication = 0x72
	monitorHVCInternal    = 0x7f
	arm64SPSRAlwaysRES0   = uint64(0xffffffff0cdfe020)
)

func validApplicationPState(pstate uint64) bool {
	return pstate&arm64SPSRAlwaysRES0 == 0 && pstate&0xf == 0 && pstate&0x3c0 == 0
}

// GuestRAMIPAStart is the first GPA available to GuestRAM. All lower GPAs are
// permanently reserved for the monitor execution cache for the VM epoch.
const GuestRAMIPAStart = uint64(0x10000000)

type monitorMemory struct {
	codeHost    uintptr
	controlHost uintptr
	statesHost  uintptr
	poolHost    uintptr
	codeLength  uint64

	bootstrapOffset   uint64
	cancelOffset      uint64
	commandOffset     uint64
	applicationOffset uint64
	vectorOffset      uint64
	vectorEndOffset   uint64
	syncHandlerOffset uint64
	syncHandlerEnd    uint64
	irqHandlerOffset  uint64
	irqHandlerEnd     uint64
	unexpectedOffset  uint64

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
	m := &monitorMemory{codeLength: roundUp(imageLength, vmm.HostSlabSize)}
	m.bootstrapOffset = embeddedMonitorBootstrapOffset
	m.cancelOffset = embeddedMonitorCancelOffset
	m.commandOffset = embeddedMonitorCommandOffset
	m.applicationOffset = embeddedMonitorApplicationOffset
	m.vectorOffset = embeddedMonitorVectorOffset
	m.vectorEndOffset = embeddedMonitorVectorEndOffset
	m.syncHandlerOffset = embeddedMonitorSyncHandlerOffset
	m.syncHandlerEnd = embeddedMonitorSyncHandlerEndOffset
	m.irqHandlerOffset = embeddedMonitorIRQHandlerOffset
	m.irqHandlerEnd = embeddedMonitorIRQHandlerEndOffset
	m.unexpectedOffset = embeddedMonitorUnexpectedHandlerOffset
	for name, offset := range map[string]uint64{
		"bootstrap":          m.bootstrapOffset,
		"cancel":             m.cancelOffset,
		"command":            m.commandOffset,
		"application":        m.applicationOffset,
		"vectors":            m.vectorOffset,
		"vectors-end":        m.vectorEndOffset,
		"sync-handler":       m.syncHandlerOffset,
		"sync-handler-end":   m.syncHandlerEnd,
		"irq-handler":        m.irqHandlerOffset,
		"irq-handler-end":    m.irqHandlerEnd,
		"unexpected-handler": m.unexpectedOffset,
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
		C.uint64_t(GuestRAMIPAStart-monitorPoolGPA),
		C.uint64_t(monitorCodeVA+m.commandOffset),
		C.uint64_t(monitorCodeVA+m.applicationOffset),
		C.uint64_t(monitorCodeVA+m.vectorOffset),
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

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}

func (v *hvfVCPU) ensureBooted() error {
	if v.booted {
		return nil
	}
	C.ttc_monitor_init_state(unsafe.Pointer(v.stateHost), C.uint64_t(monitorControlVA), C.uint64_t(monitorControlGPA))
	if status := C.ttc_monitor_set_boot_registers(v.handle,
		C.uint64_t(monitorCodeGPA+v.monitor.bootstrapOffset),
		C.uint64_t(monitorControlGPA),
		C.uint64_t(v.stateGPA+vmm.HostSlabSize)); status != C.HV_SUCCESS {
		return fmt.Errorf("setting EL1 monitor bootstrap registers: %d", status)
	}
	if _, err := v.runUntilHVC(context.Background(), monitorHVCBoot, true); err != nil {
		return fmt.Errorf("booting EL1 monitor: %w", err)
	}
	ttbr1 := uint64(C.ttc_monitor_ttbr1(unsafe.Pointer(v.monitor.controlHost)))
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

func (v *hvfVCPU) runUntilHVC(ctx context.Context, expected uint64, resumeCanceled bool) (bool, error) {
	wasCanceled := false
	applicationIRQPending := false
	deassertApplicationIRQ := func() error {
		if !applicationIRQPending {
			return nil
		}
		applicationIRQPending = false
		if status := C.hv_vcpu_set_pending_interrupt(v.handle, C.HV_INTERRUPT_TYPE_IRQ, C.bool(false)); status != C.HV_SUCCESS {
			return fmt.Errorf("deasserting HVF application interrupt: %d", status)
		}
		return nil
	}
	defer func() {
		_ = deassertApplicationIRQ()
	}()
	for {
		// HVF clears the pending-interrupt input every time hv_vcpu_run
		// returns, including when another queued hv_vcpus_exit request wins
		// the race and produces HV_EXIT_REASON_CANCELED. Reassert the IRQ
		// before every retry until the monitor has acknowledged it. Setting
		// this input here also satisfies HVF's owning-thread requirement.
		if applicationIRQPending {
			if status := C.hv_vcpu_set_pending_interrupt(v.handle, C.HV_INTERRUPT_TYPE_IRQ, C.bool(true)); status != C.HV_SUCCESS {
				return false, fmt.Errorf("asserting HVF application interrupt: %d", status)
			}
		}
		C.ttc_monitor_fence()
		status := C.hv_vcpu_run(v.handle)
		C.ttc_monitor_fence()
		if status != C.HV_SUCCESS {
			return wasCanceled, fmt.Errorf("hv_vcpu_run failed: %d", status)
		}
		switch C.ttc_monitor_exit_reason(v.exit) {
		case C.HV_EXIT_REASON_EXCEPTION:
			syndrome := uint64(C.ttc_monitor_exit_syndrome(v.exit))
			if syndrome>>26&0x3f != 0x16 {
				return wasCanceled, fmt.Errorf("EL1 monitor unexpected exception syndrome %#x", syndrome)
			}
			immediate := syndrome & 0xffff
			if immediate == monitorHVCInternal {
				var cpsr, pc C.uint64_t
				if status := C.ttc_monitor_cancel_location(v.handle, &cpsr, &pc); status == C.HV_SUCCESS {
					state := unsafe.Slice((*byte)(unsafe.Pointer(v.stateHost)), vmm.HostSlabSize)
					return wasCanceled, fmt.Errorf("EL1 monitor reported an internal failure at CPSR %#x PC %#x: vector=%d ESR=%#x FAR=%#x ELR=%#x input-PSTATE=%#x host-input={space=%d PC=%#x PSTATE=%#x}",
						uint64(cpsr), uint64(pc),
						binary.LittleEndian.Uint64(state[stateInternalVector:]),
						binary.LittleEndian.Uint64(state[stateInternalESR:]),
						binary.LittleEndian.Uint64(state[stateInternalFAR:]),
						binary.LittleEndian.Uint64(state[stateInternalELR:]),
						binary.LittleEndian.Uint64(state[stateInternalPState:]),
						binary.LittleEndian.Uint64(state[stateHostInputSpace:]),
						binary.LittleEndian.Uint64(state[stateHostInputPC:]),
						binary.LittleEndian.Uint64(state[stateHostInputPState:]))
				}
				return wasCanceled, fmt.Errorf("EL1 monitor reported an internal failure")
			}
			if immediate != expected {
				return wasCanceled, fmt.Errorf("EL1 monitor HVC %#x, want %#x", immediate, expected)
			}
			if err := deassertApplicationIRQ(); err != nil {
				return false, err
			}
			return wasCanceled, nil
		case C.HV_EXIT_REASON_CANCELED:
			if !resumeCanceled && expected == monitorHVCApplication {
				location, err := v.applicationCanceledLocation()
				if err != nil {
					return false, err
				}
				switch location {
				case applicationCancelHandler:
					// The interrupt caught the vCPU after an EL0 exception had
					// entered the monitor, but before the handler reached its HVC.
					// Finish the bounded register save and report the real guest
					// exit.
					continue
				case applicationCancelEntry:
					// EL1 entry runs with DAIF masked. No EL0 instruction has run,
					// so acknowledge cancellation using the untouched input state
					// instead of injecting an IRQ that HVF may consume while masked.
					if status := C.ttc_monitor_set_entry(v.handle,
						C.uint64_t(monitorCodeVA+v.monitor.cancelOffset), C.uint64_t(v.stateVA)); status != C.HV_SUCCESS {
						return false, fmt.Errorf("setting EL1 monitor cancellation entry: %d", status)
					}
					continue
				}
				applicationIRQPending = true
				// Resume without reading application registers through HVF.
				// The pending virtual IRQ takes an architected lower-EL
				// exception, and the EL1 handler snapshots exact GP, PC,
				// PSTATE, TLS, and FP-visible state before returning to host.
				continue
			}
			wasCanceled = true
			if !resumeCanceled {
				return true, nil
			}
			// Monitor transactions must reach their synchronous acknowledgement
			// even if their caller is canceled; resume at the interrupted PC.
			continue
		case C.HV_EXIT_REASON_VTIMER_ACTIVATED:
			C.hv_vcpu_set_vtimer_mask(v.handle, C.bool(true))
			continue
		default:
			if err := ctx.Err(); err != nil && !resumeCanceled {
				return wasCanceled, err
			}
			return wasCanceled, fmt.Errorf("EL1 monitor unexpected HVF exit reason %d", C.ttc_monitor_exit_reason(v.exit))
		}
	}
}

type applicationCancelLocation uint8

const (
	applicationCancelEL0 applicationCancelLocation = iota
	applicationCancelEntry
	applicationCancelHandler
)

// applicationCanceledLocation distinguishes EL0 from the two EL1 regions
// that can be observed during application execution. A canceled exception
// handler is resumed to its HVC. A canceled entry is redirected to a bounded
// cancellation acknowledgement because the application has not executed.
func (v *hvfVCPU) applicationCanceledLocation() (applicationCancelLocation, error) {
	var pc C.uint64_t
	if status := C.ttc_monitor_cancel_pc(v.handle, &pc); status != C.HV_SUCCESS {
		return applicationCancelEL0, fmt.Errorf("reading canceled EL1 monitor location: %d", status)
	}
	location := uint64(pc)
	if location < monitorCodeVA || location >= monitorCodeVA+v.monitor.codeLength {
		return applicationCancelEL0, nil
	}
	offset := location - monitorCodeVA
	if (offset >= v.monitor.vectorOffset && offset < v.monitor.vectorEndOffset) ||
		(offset >= v.monitor.syncHandlerOffset && offset < v.monitor.syncHandlerEnd) ||
		(offset >= v.monitor.irqHandlerOffset && offset < v.monitor.irqHandlerEnd) ||
		offset >= v.monitor.unexpectedOffset {
		return applicationCancelHandler, nil
	}
	return applicationCancelEntry, nil
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
	_, err := v.runUntilHVC(ctx, monitorHVCCommand, true)
	if err != nil {
		return nil, err
	}
	response := make([]byte, 32)
	copy(response, state[stateResponse:stateResponse+32])
	return response, nil
}

func (v *hvfVCPU) Run(ctx context.Context, application any) (vmm.Exit, error) {
	run, ok := application.(vmm.ApplicationState)
	if !ok {
		return vmm.Exit{}, fmt.Errorf("HVF application state has type %T, want vmm.ApplicationState", application)
	}
	registers, ok := run.Registers.(*vmm.ARM64State)
	if !ok {
		return vmm.Exit{}, fmt.Errorf("HVF ARM64 register state has type %T, want *vmm.ARM64State", run.Registers)
	}
	if err := v.ensureBooted(); err != nil {
		return vmm.Exit{}, err
	}
	if !validApplicationPState(registers.PState) {
		return vmm.Exit{}, fmt.Errorf("invalid ARM64 application PSTATE %#x before EL0 entry (PC=%#x)", registers.PState, registers.PC)
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(v.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint64(state[stateAddressSpaceID:], run.AddressSpaceID)
	binary.LittleEndian.PutUint64(state[stateHostInputSpace:], run.AddressSpaceID)
	binary.LittleEndian.PutUint64(state[stateHostInputPC:], registers.PC)
	binary.LittleEndian.PutUint64(state[stateHostInputPState:], registers.PState)
	for i, value := range registers.X {
		binary.LittleEndian.PutUint64(state[stateX0+i*8:], value)
	}
	binary.LittleEndian.PutUint64(state[stateSP:], registers.SP)
	binary.LittleEndian.PutUint64(state[statePC:], registers.PC)
	binary.LittleEndian.PutUint64(state[statePState:], registers.PState&^0xf)
	binary.LittleEndian.PutUint64(state[stateTPIDR:], registers.TPIDR)
	binary.LittleEndian.PutUint64(state[stateExitKind:], 0)
	if status := C.ttc_monitor_load_fp(v.handle, (*C.uint8_t)(unsafe.Pointer(&registers.FP[0]))); status != C.HV_SUCCESS {
		return vmm.Exit{}, fmt.Errorf("loading ARM64 application FP/SIMD state: %d", status)
	}
	if status := C.ttc_monitor_set_entry(v.handle,
		C.uint64_t(monitorCodeVA+v.monitor.applicationOffset), C.uint64_t(v.stateVA)); status != C.HV_SUCCESS {
		return vmm.Exit{}, fmt.Errorf("setting EL1 monitor application entry: %d", status)
	}
	canceled, err := v.runUntilHVC(ctx, monitorHVCApplication, false)
	if err != nil {
		return vmm.Exit{}, err
	}
	for i := range registers.X {
		registers.X[i] = binary.LittleEndian.Uint64(state[stateX0+i*8:])
	}
	registers.SP = binary.LittleEndian.Uint64(state[stateSP:])
	registers.PC = binary.LittleEndian.Uint64(state[statePC:])
	rawPState := binary.LittleEndian.Uint64(state[statePState:])
	if !validApplicationPState(rawPState) {
		return vmm.Exit{}, fmt.Errorf("EL1 monitor saved invalid ARM64 application PSTATE %#x after exit kind %d ESR=%#x PC=%#x",
			rawPState, binary.LittleEndian.Uint64(state[stateExitKind:]),
			binary.LittleEndian.Uint64(state[stateESR:]),
			binary.LittleEndian.Uint64(state[statePC:]))
	}
	registers.PState = rawPState
	registers.TPIDR = binary.LittleEndian.Uint64(state[stateTPIDR:])
	if status := C.ttc_monitor_save_fp(v.handle, (*C.uint8_t)(unsafe.Pointer(&registers.FP[0]))); status != C.HV_SUCCESS {
		return vmm.Exit{}, fmt.Errorf("saving ARM64 application FP/SIMD state: %d", status)
	}
	if canceled {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
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
		if ec != 0x20 && ec != 0x21 && ec != 0x24 && ec != 0x25 {
			return vmm.Exit{Reason: vmm.ExitUnknown, Syndrome: syndrome, Address: registers.PC}, nil
		}
		return vmm.Exit{Reason: vmm.ExitFault, Syndrome: syndrome, Address: address, Access: access}, nil
	case 3:
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	default:
		return vmm.Exit{}, errors.New("EL1 monitor returned no application exit state")
	}
}
