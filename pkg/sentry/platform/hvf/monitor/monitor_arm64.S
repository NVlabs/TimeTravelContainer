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

#include "monitor_layout.h"

.macro SAVE_FP_STATE base
  mrs x16, fpsr
  str w16, [\base, #0]
  mrs x16, fpcr
  str w16, [\base, #4]
  add \base, \base, #TTC_STATE_FP_Q0_OFFSET
  stp q0, q1, [\base, #16 * 0]
  stp q2, q3, [\base, #16 * 2]
  stp q4, q5, [\base, #16 * 4]
  stp q6, q7, [\base, #16 * 6]
  stp q8, q9, [\base, #16 * 8]
  stp q10, q11, [\base, #16 * 10]
  stp q12, q13, [\base, #16 * 12]
  stp q14, q15, [\base, #16 * 14]
  stp q16, q17, [\base, #16 * 16]
  stp q18, q19, [\base, #16 * 18]
  stp q20, q21, [\base, #16 * 20]
  stp q22, q23, [\base, #16 * 22]
  stp q24, q25, [\base, #16 * 24]
  stp q26, q27, [\base, #16 * 26]
  stp q28, q29, [\base, #16 * 28]
  stp q30, q31, [\base, #16 * 30]
  sub \base, \base, #TTC_STATE_FP_Q0_OFFSET
.endm

.macro LOAD_FP_STATE base
  ldr w16, [\base, #0]
  msr fpsr, x16
  ldr w16, [\base, #4]
  msr fpcr, x16
  add \base, \base, #TTC_STATE_FP_Q0_OFFSET
  ldp q0, q1, [\base, #16 * 0]
  ldp q2, q3, [\base, #16 * 2]
  ldp q4, q5, [\base, #16 * 4]
  ldp q6, q7, [\base, #16 * 6]
  ldp q8, q9, [\base, #16 * 8]
  ldp q10, q11, [\base, #16 * 10]
  ldp q12, q13, [\base, #16 * 12]
  ldp q14, q15, [\base, #16 * 14]
  ldp q16, q17, [\base, #16 * 16]
  ldp q18, q19, [\base, #16 * 18]
  ldp q20, q21, [\base, #16 * 20]
  ldp q22, q23, [\base, #16 * 22]
  ldp q24, q25, [\base, #16 * 24]
  ldp q26, q27, [\base, #16 * 26]
  ldp q28, q29, [\base, #16 * 28]
  ldp q30, q31, [\base, #16 * 30]
  sub \base, \base, #TTC_STATE_FP_Q0_OFFSET
.endm

.section __TEXT,__ttcmon,regular,pure_instructions
.p2align 14

.globl _ttc_monitor_image_anchor
_ttc_monitor_image_anchor:
  nop

.globl _ttc_monitor_hvc_boot
_ttc_monitor_hvc_boot:
  hvc #0x70
1: b 1b

.globl _ttc_monitor_hvc_command
_ttc_monitor_hvc_command:
  hvc #0x71
1: b 1b

.globl _ttc_monitor_hvc_internal
_ttc_monitor_hvc_internal:
  hvc #0x7f
1: b 1b

.globl _ttc_monitor_dsb_ishst
_ttc_monitor_dsb_ishst:
  dsb ishst
  ret

.globl _ttc_monitor_dsb_ish
_ttc_monitor_dsb_ish:
  dsb ish
  ret

.globl _ttc_monitor_isb
_ttc_monitor_isb:
  isb
  ret

.globl _ttc_monitor_prepare_mmu
_ttc_monitor_prepare_mmu:
  dsb sy
  ic iallu
  dsb sy
  isb
  ret

.globl _ttc_monitor_tlbi_vae1is
_ttc_monitor_tlbi_vae1is:
  tlbi vae1is, x0
  ret

.globl _ttc_monitor_tlbi_aside1is
_ttc_monitor_tlbi_aside1is:
  tlbi aside1is, x0
  ret

.globl _ttc_monitor_write_ttbr0
_ttc_monitor_write_ttbr0:
  msr ttbr0_el1, x0
  isb
  ret

.globl _ttc_monitor_read_ttbr0
_ttc_monitor_read_ttbr0:
  mrs x0, ttbr0_el1
  ret

// ttc_monitor_ring0_application_entry saves the running Sentry continuation,
// switches from its EL1-only TTBR0 to the selected application root, and
// enters EL0. The lower-EL vectors restore its architectural register image
// after a syscall or fault so the same Sentry continuation handles the exit at
// guest EL1. Host-dependent work redpills that continuation explicitly.
.globl _ttc_monitor_ring0_application_entry
_ttc_monitor_ring0_application_entry:
  clrex
  // Sentry populated the application image through its identity alias. Make
  // those stores visible before the monitor consumes the same physical state
  // page through its private TTBR1 alias.
  dsb ish
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
  stp x18, x19, [sp, #16]
  mrs x16, tpidr_el1
  add x17, x16, #2, lsl #12
  add x17, x17, #0xa88
  stp x0, x1, [x17, #0x00]
  stp x2, x3, [x17, #0x10]
  stp x4, x5, [x17, #0x20]
  stp x6, x7, [x17, #0x30]
  stp x8, x9, [x17, #0x40]
  stp x10, x11, [x17, #0x50]
  stp x12, x13, [x17, #0x60]
  stp x14, x15, [x17, #0x70]
  ldp x0, x1, [sp, #0]
  stp x0, x1, [x17, #0x80]
  ldp x0, x1, [sp, #16]
  stp x0, x1, [x17, #0x90]
  stp x20, x21, [x17, #0xa0]
  stp x22, x23, [x17, #0xb0]
  stp x24, x25, [x17, #0xc0]
  stp x26, x27, [x17, #0xd0]
  stp x28, x29, [x17, #0xe0]
  str x30, [x17, #0xf0]
  // The entry trampoline has already switched away from the movable Go
  // stack. Restore records from the original stack value, not this private
  // monitor stack.
  ldr x0, [x16, #TTC_STATE_RING0_SENTRY_SP_OFFSET]
  str x0, [x16, #TTC_STATE_SENTRY_SP_OFFSET]
  str x30, [x16, #TTC_STATE_SENTRY_PC_OFFSET]
  mrs x0, nzcv
  mov x1, #0x3c5
  orr x0, x0, x1
  str x0, [x16, #TTC_STATE_SENTRY_PSTATE_OFFSET]
  mrs x0, tpidr_el0
  str x0, [x16, #TTC_STATE_SENTRY_TPIDR_OFFSET]
  mrs x0, tpidrro_el0
  str x0, [x16, #TTC_STATE_SENTRY_TPIDRRO_OFFSET]
  mrs x0, ttbr0_el1
  str x0, [x16, #TTC_STATE_SENTRY_TTBR0_OFFSET]
  add x17, x16, #2, lsl #12
  add x17, x17, #0xc00
  SAVE_FP_STATE x17
  mrs x0, tpidr_el1
  bl _ttc_monitor_ring0_application_ttbr
  mrs x16, tpidr_el1
  cbz x0, _ttc_monitor_hvc_internal
  str x0, [x16, #TTC_STATE_APPLICATION_TTBR0_OFFSET]
  mrs x18, tpidr_el1
  add x17, x18, #3, lsl #12
  LOAD_FP_STATE x17
  add x18, x18, #2, lsl #12
  add x18, x18, #0x908
  ldr x16, [x18, #TTC_APP_SP_OFFSET]
  msr sp_el0, x16
  ldr x16, [x18, #TTC_APP_PC_OFFSET]
  msr elr_el1, x16
  ldr x16, [x18, #TTC_APP_PSTATE_OFFSET]
  msr spsr_el1, x16
  // Enabling software step requires a debug-register write and an ISB. Keep
  // both off the normal syscall path; application exits clear the monitor-
  // owned active flag before a worker can be reused by an ordinary task.
  tbz x16, #21, 0f
  mov x17, #1
  msr mdscr_el1, x17
  mrs x16, tpidr_el1
  str x17, [x16, #TTC_STATE_DEBUG_STEP_ACTIVE_OFFSET]
  isb
0:
  ldr x16, [x18, #TTC_APP_TPIDR_OFFSET]
  msr tpidr_el0, x16
  // TPIDRRO_EL0 is Darwin host TLS for the running Go thread. It is not part
  // of the Linux application register image and must not be exposed at EL0.
  msr tpidrro_el0, xzr
  mrs x16, tpidr_el1
  // Hypervisor.framework does not reliably propagate a TLBI executed by the
  // dedicated monitor vCPU to a dormant application vCPU. The host publishes
  // the page-table changes that this vCPU hasn't flushed in this permanently
  // mapped state page. Flush locally before the first application entry that
  // observes them; the common syscall path pays nothing when mappings have
  // not changed.
  ldr x17, [x16, #TTC_STATE_TLB_FLUSH_REQUESTED_OFFSET]
  cbz x17, 1f
  str xzr, [x16, #TTC_STATE_TLB_FLUSH_REQUESTED_OFFSET]
  dsb ishst
  // Use the inner-shareable forms even though this rendezvous is local to the
  // entering vCPU. HVF's ARM64 implementation reliably handles the shareable
  // TLBI family used by the monitor's BBM path; the local-only VMALLE1 form
  // can leave an old application leaf resident across vCPU reuse.
  tbnz x17, #0, Lttc_monitor_flush_all_translations
  // Application leaves are non-global, so when only the entered address
  // space changed, flush just its ASID and keep other address spaces' entries.
  ldr x17, [x16, #TTC_STATE_APPLICATION_TTBR0_OFFSET]
  and x17, x17, #0xffff000000000000
  tlbi aside1is, x17
  b Lttc_monitor_translations_flushed
Lttc_monitor_flush_all_translations:
  tlbi vmalle1is
Lttc_monitor_translations_flushed:
  dsb ish
  // The monitor transaction broadcasts IALLUIS, but HVF does not reliably
  // propagate that maintenance to every dormant application vCPU either.
  // Invalidate this worker's instruction cache before it executes through a
  // freshly published executable leaf. Without the local fallback, a recycled
  // worker can sporadically decode the zero-filled line that preceded the host
  // writer's executable mapping.
  ic iallu
  dsb ish
  isb
1:
  ldr x17, [x16, #TTC_STATE_CANCEL_REQUESTED_OFFSET]
  cbnz x17, _ttc_monitor_cancel_before_application
  ldr x17, [x16, #TTC_STATE_APPLICATION_TTBR0_OFFSET]
  msr ttbr0_el1, x17
  isb
  add x17, x16, #TTC_STATE_STACK_TOP
  mov sp, x17
  ldp x0, x1, [x18, #0x00]
  ldp x2, x3, [x18, #0x10]
  ldp x4, x5, [x18, #0x20]
  ldp x6, x7, [x18, #0x30]
  ldp x8, x9, [x18, #0x40]
  ldp x10, x11, [x18, #0x50]
  ldp x12, x13, [x18, #0x60]
  ldp x14, x15, [x18, #0x70]
  ldp x16, x17, [x18, #0x80]
  ldp x19, x20, [x18, #0x98]
  ldp x21, x22, [x18, #0xa8]
  ldp x23, x24, [x18, #0xb8]
  ldp x25, x26, [x18, #0xc8]
  ldp x27, x28, [x18, #0xd8]
  ldp x29, x30, [x18, #0xe8]
  ldr x18, [x18, #0x90]
  eret

_ttc_monitor_cancel_before_application:
  mov x17, #TTC_EXIT_INTERRUPTED
  str x17, [x16, #TTC_STATE_EXIT_KIND_OFFSET]
  b _ttc_monitor_finish_application_exit

// Exception vectors. The Sentry continuation runs at EL1h, so synchronous
// exceptions taken from current EL using SP_EL1 are host-service gates. Lower
// EL synchronous exceptions and IRQs remain application exits. Every other
// vector is an internal monitor failure.
.p2align 11
.globl _ttc_monitor_vectors
_ttc_monitor_vectors:
  // Current EL using SP_EL0.
  mov x16, #0
  b _ttc_monitor_unexpected_handler
  .space 120
  mov x16, #1
  b _ttc_monitor_unexpected_handler
  .space 120
  mov x16, #2
  b _ttc_monitor_unexpected_handler
  .space 120
  mov x16, #3
  b _ttc_monitor_unexpected_handler
  .space 120

  // Current EL using SP_EL1, synchronous vector at VBAR+0x200.
_ttc_monitor_el1_sync:
  b _ttc_monitor_el1_sync_handler
  .space 124

  mov x16, #5
  b _ttc_monitor_unexpected_handler
  .space 120
  mov x16, #6
  b _ttc_monitor_unexpected_handler
  .space 120
  mov x16, #7
  b _ttc_monitor_unexpected_handler
  .space 120

// Lower EL, AArch64 synchronous vector at VBAR+0x400.
_ttc_monitor_el0_sync:
  b _ttc_monitor_el0_sync_handler
  .space 124

// Lower EL, AArch64 IRQ vector at VBAR+0x480. Host-requested exits are
// converted into this architected exception so ELR_EL1/SPSR_EL1 contain an
// exact application PC/PSTATE; HVF's direct CPSR read is not reliable after
// HV_EXIT_REASON_CANCELED.
_ttc_monitor_el0_irq:
  b _ttc_monitor_el0_irq_handler
  .space 124

// Lower EL, AArch64 FIQ vector at VBAR+0x500. HVF cancellation currently uses
// IRQ plus a monitor-owned exit-kind word; retain a complete FIQ snapshot path
// so an unexpected asserted FIQ still returns a precise external interruption.
_ttc_monitor_el0_fiq:
  b _ttc_monitor_el0_fiq_handler
  .space 124

.set ttc_vector_number, 11
.rept 5
  mov x16, #ttc_vector_number
  b _ttc_monitor_unexpected_handler
  .space 120
  .set ttc_vector_number, ttc_vector_number + 1
.endr

.globl _ttc_monitor_vectors_end
_ttc_monitor_vectors_end:

// A Sentry continuation executes directly at EL1 with a host identity mirror
// in TTBR0. Darwin services are intentionally absent from that stage-1 map:
// entering an external executable therefore takes a current-EL instruction
// abort. Direct Darwin SVCs take a current-EL SVC exception. In both cases,
// publish the original host instruction and redpill. The host signal bridge
// copies the complete architectural continuation out of the vCPU and resumes
// the interrupted thread at host EL0. A later application entry bluepills the
// same continuation again after refreshing the identity mirror.
.globl _ttc_monitor_el1_sync_handler
_ttc_monitor_el1_sync_handler:
  // The faulting Sentry SP may itself be the unmapped address that caused this
  // exception (for example, a newly grown Go stack). Do not touch it before
  // switching to the permanently mapped per-vCPU emergency stack. SP_EL0 is
  // scratch while Sentry runs at EL1h; application entry reloads it from the
  // canonical register image.
  msr sp_el0, x18
  mrs x18, tpidr_el1
  str x16, [x18, #TTC_STATE_HOST_GATE_X16_OFFSET]
  str x17, [x18, #TTC_STATE_HOST_GATE_X17_OFFSET]
  mov x17, sp
  str x17, [x18, #TTC_STATE_HOST_GATE_SP_OFFSET]
  add x17, x18, #4, lsl #12
  mov sp, x17
  mrs x17, esr_el1
  str x17, [x18, #TTC_STATE_HOST_GATE_ESR_OFFSET]
  lsr x17, x17, #26
  cmp x17, #0x15
  b.eq 1f
  cmp x17, #0x21
  b.eq _ttc_monitor_el1_instruction_abort
  cmp x17, #0x25
  b.ne 3f
  b _ttc_monitor_el1_fault_gate
_ttc_monitor_el1_instruction_abort:
  mrs x17, elr_el1
  ldr x16, [x18, #TTC_STATE_CLOCK_GETTIME_PC_OFFSET]
  cmp x17, x16
  b.eq _ttc_monitor_fast_clock_gettime
  ldr x16, [x18, #TTC_STATE_MACH_ABSOLUTE_TIME_PC_OFFSET]
  cmp x17, x16
  b.eq _ttc_monitor_fast_mach_absolute_time
_ttc_monitor_el1_fault_gate:
  // Instruction and data abort ELRs identify the faulting instruction.
  mrs x17, far_el1
  str x17, [x18, #TTC_STATE_HOST_GATE_FAR_OFFSET]
  mrs x17, elr_el1
  b 4f
1:
  // SVC ELR identifies the following instruction. Reexecute the SVC under
  // Darwin after returning to host EL0.
  str xzr, [x18, #TTC_STATE_HOST_GATE_FAR_OFFSET]
  mrs x17, elr_el1
  sub x17, x17, #4
4:
  str x17, [x18, #TTC_STATE_HOST_GATE_PC_OFFSET]
  mrs x17, spsr_el1
  str x17, [x18, #TTC_STATE_HOST_GATE_PSTATE_OFFSET]
  dsb ishst
  ldr x16, [x18, #TTC_STATE_HOST_GATE_SP_OFFSET]
  mov sp, x16
  ldr x16, [x18, #TTC_STATE_HOST_GATE_X16_OFFSET]
  ldr x17, [x18, #TTC_STATE_HOST_GATE_X17_OFFSET]
  mrs x18, sp_el0
  hvc #TTC_RING0_HVC_HOST_GATE
5:
  b 5b
3:
  mov x16, #4
  b _ttc_monitor_unexpected_handler

// Darwin's Go runtime implements walltime and nanotime by branching to
// clock_gettime and mach_absolute_time in libSystem. Linux KVM can execute its
// VDSO equivalents directly in the guest; these two services provide the same
// property on Apple Silicon without mapping arm64e host dylib code into EL1.
_ttc_monitor_fast_mach_absolute_time:
  isb
  mrs x0, cntvct_el0
  ldr x16, [x18, #TTC_STATE_VTIMER_OFFSET_OFFSET]
  add x0, x0, x16
  b _ttc_monitor_fast_time_return

_ttc_monitor_fast_clock_gettime:
  // Darwin CLOCK_REALTIME is zero. Unknown clock IDs keep the general host
  // gate semantics rather than risking an ABI-incompatible emulation.
  cbnz x0, _ttc_monitor_el1_fault_gate
  isb
  mrs x16, cntvct_el0
  // Convert ticks without overflowing: (ticks/denom)*numer plus the converted
  // remainder. Apple Silicon's current 125/3 timebase then remains valid for
  // the full uint64 counter lifetime.
  ldr x17, [x18, #TTC_STATE_TIMEBASE_DENOM_OFFSET]
  udiv x0, x16, x17
  msub x16, x0, x17, x16
  ldr x17, [x18, #TTC_STATE_TIMEBASE_NUMER_OFFSET]
  mul x0, x0, x17
  mul x16, x16, x17
  ldr x17, [x18, #TTC_STATE_TIMEBASE_DENOM_OFFSET]
  udiv x16, x16, x17
  add x16, x0, x16
  ldr x17, [x18, #TTC_STATE_REALTIME_OFFSET_NS_OFFSET]
  add x16, x16, x17
  // Split realtime nanoseconds into Darwin's two-int64 timespec.
  movz x17, #0xca00
  movk x17, #0x3b9a, lsl #16
  udiv x0, x16, x17
  msub x16, x0, x17, x16
  str x0, [x1]
  str x16, [x1, #8]
  mov x0, xzr
_ttc_monitor_fast_time_return:
  msr elr_el1, x30
  ldr x16, [x18, #TTC_STATE_HOST_GATE_SP_OFFSET]
  mov sp, x16
  ldr x16, [x18, #TTC_STATE_HOST_GATE_X16_OFFSET]
  ldr x17, [x18, #TTC_STATE_HOST_GATE_X17_OFFSET]
  mrs x18, sp_el0
  eret

.globl _ttc_monitor_el0_sync_handler
_ttc_monitor_el0_sync_handler:
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
  // WFI and WFE (EC 0x01) only hint that the thread has nothing to do, and
  // may complete at any time. Complete them here, as Linux completes a
  // trapped WFI, rather than exiting to the Sentry; SCTLR_EL1.nTWE is clear
  // so that EL0 WFE traps here rather than to HVF.
  mrs x16, esr_el1
  lsr x17, x16, #26
  cmp x17, #0x01
  b.ne Lttc_monitor_el0_sync_not_wfx
  mrs x16, elr_el1
  add x16, x16, #4
  msr elr_el1, x16
  // Like Linux after emulating an instruction, clear SPSR.SS so that a
  // pending single step completes on return.
  mrs x16, spsr_el1
  bic x16, x16, #(1 << 21)
  msr spsr_el1, x16
  ldp x16, x17, [sp, #0]
  add sp, sp, #32
  eret
Lttc_monitor_el0_sync_not_wfx:
  stp x18, x19, [sp, #16]
  mrs x16, tpidr_el1
  add x16, x16, #2, lsl #12
  add x16, x16, #0x908
  stp x0, x1, [x16, #0x00]
  stp x2, x3, [x16, #0x10]
  stp x4, x5, [x16, #0x20]
  stp x6, x7, [x16, #0x30]
  stp x8, x9, [x16, #0x40]
  stp x10, x11, [x16, #0x50]
  stp x12, x13, [x16, #0x60]
  stp x14, x15, [x16, #0x70]
  ldp x17, x18, [sp, #0]
  stp x17, x18, [x16, #0x80]
  ldp x17, x18, [sp, #16]
  stp x17, x18, [x16, #0x90]
  stp x20, x21, [x16, #0xa0]
  stp x22, x23, [x16, #0xb0]
  stp x24, x25, [x16, #0xc0]
  stp x26, x27, [x16, #0xd0]
  stp x28, x29, [x16, #0xe0]
  str x30, [x16, #0xf0]
  add sp, sp, #32
  mrs x17, sp_el0
  str x17, [x16, #TTC_APP_SP_OFFSET]
  mrs x17, elr_el1
  str x17, [x16, #TTC_APP_PC_OFFSET]
  mrs x17, spsr_el1
  str x17, [x16, #TTC_APP_PSTATE_OFFSET]
  mrs x17, tpidr_el0
  str x17, [x16, #TTC_APP_TPIDR_OFFSET]
  mrs x17, esr_el1
  str x17, [x16, #TTC_APP_ESR_OFFSET]
  mrs x18, far_el1
  str x18, [x16, #TTC_APP_FAR_OFFSET]
  lsr x18, x17, #26
  cmp x18, #0x15
  b.eq 1f
  // Only translation, access-flag, and permission aborts are memory faults.
  // In particular, BRK (EC 0x3c) and software step (EC 0x32) must reach
  // ptrace as architectural exceptions rather than fabricated SIGSEGVs.
  cmp x18, #0x20
  b.eq 2f
  cmp x18, #0x24
  b.eq 2f
  mov x17, #TTC_EXIT_EXCEPTION
  b 3f
1:
  mov x17, #TTC_EXIT_SYSCALL
  b 3f
2:
  mov x17, #TTC_EXIT_FAULT
3:
  str x17, [x16, #TTC_APP_EXIT_KIND_OFFSET]
  b _ttc_monitor_finish_application_exit
.globl _ttc_monitor_el0_sync_handler_end
_ttc_monitor_el0_sync_handler_end:

.globl _ttc_monitor_el0_irq_handler
_ttc_monitor_el0_irq_handler:
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
  stp x18, x19, [sp, #16]
  mrs x16, tpidr_el1
  add x18, x16, #2, lsl #12
  add x18, x18, #0xa70
  ldar x19, [x18]
  stlr xzr, [x18]
  cbnz x19, 1f
  mov x19, #TTC_EXIT_RETRY
1:
  b _ttc_monitor_el0_interrupt_handler

.globl _ttc_monitor_el0_fiq_handler
_ttc_monitor_el0_fiq_handler:
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
  stp x18, x19, [sp, #16]
  mov x19, #TTC_EXIT_INTERRUPTED

_ttc_monitor_el0_interrupt_handler:
  mrs x16, tpidr_el1
  add x16, x16, #2, lsl #12
  add x16, x16, #0x908
  str x19, [x16, #TTC_APP_EXIT_KIND_OFFSET]
  stp x0, x1, [x16, #0x00]
  stp x2, x3, [x16, #0x10]
  stp x4, x5, [x16, #0x20]
  stp x6, x7, [x16, #0x30]
  stp x8, x9, [x16, #0x40]
  stp x10, x11, [x16, #0x50]
  stp x12, x13, [x16, #0x60]
  stp x14, x15, [x16, #0x70]
  ldp x17, x18, [sp, #0]
  stp x17, x18, [x16, #0x80]
  ldp x17, x18, [sp, #16]
  stp x17, x18, [x16, #0x90]
  stp x20, x21, [x16, #0xa0]
  stp x22, x23, [x16, #0xb0]
  stp x24, x25, [x16, #0xc0]
  stp x26, x27, [x16, #0xd0]
  stp x28, x29, [x16, #0xe0]
  str x30, [x16, #0xf0]
  add sp, sp, #32
  mrs x17, sp_el0
  str x17, [x16, #TTC_APP_SP_OFFSET]
  mrs x17, elr_el1
  str x17, [x16, #TTC_APP_PC_OFFSET]
  mrs x17, spsr_el1
  str x17, [x16, #TTC_APP_PSTATE_OFFSET]
  mrs x17, tpidr_el0
  str x17, [x16, #TTC_APP_TPIDR_OFFSET]
  str xzr, [x16, #TTC_APP_ESR_OFFSET]
  str xzr, [x16, #TTC_APP_FAR_OFFSET]
  // Hypervisor.framework models virtual IRQ/FIQ inputs as level-triggered.
  // Exit once after snapshotting EL0 so the host can lower the line before
  // this handler restores the Sentry continuation. Otherwise the next EL0
  // entry immediately takes the same interrupt again.
  hvc #TTC_RING0_HVC_INTERRUPT_ACK
  b _ttc_monitor_finish_application_exit

_ttc_monitor_finish_application_exit:
  mrs x17, tpidr_el1
  // MDSCR_EL1.SS is level-sensitive. Clear it before this worker can enter a
  // non-stepped task, but make the normal syscall path pay only a load and a
  // predicted branch when software stepping was never enabled.
  ldr x16, [x17, #TTC_STATE_DEBUG_STEP_ACTIVE_OFFSET]
  cbz x16, 1f
  str xzr, [x17, #TTC_STATE_DEBUG_STEP_ACTIVE_OFFSET]
  msr mdscr_el1, xzr
  isb
1:
  str xzr, [x17, #TTC_STATE_CANCEL_REQUESTED_OFFSET]
  add x18, x17, #3, lsl #12
  SAVE_FP_STATE x18
  // Publish the complete application exit image before switching TTBR0 and
  // resuming Sentry, which reads it through a distinct stage-1 alias.
  dsb ish
  ldr x16, [x17, #TTC_STATE_SENTRY_TTBR0_OFFSET]
  msr ttbr0_el1, x16
  isb
  add x18, x17, #2, lsl #12
  add x18, x18, #0xc00
  LOAD_FP_STATE x18
  ldr x16, [x17, #TTC_STATE_SENTRY_SP_OFFSET]
  mov sp, x16
  ldr x16, [x17, #TTC_STATE_SENTRY_PC_OFFSET]
  msr elr_el1, x16
  ldr x16, [x17, #TTC_STATE_SENTRY_PSTATE_OFFSET]
  msr spsr_el1, x16
  ldr x16, [x17, #TTC_STATE_SENTRY_TPIDR_OFFSET]
  msr tpidr_el0, x16
  ldr x16, [x17, #TTC_STATE_SENTRY_TPIDRRO_OFFSET]
  msr tpidrro_el0, x16
  add x18, x17, #2, lsl #12
  add x18, x18, #0xa88
  ldp x0, x1, [x18, #0x00]
  ldp x2, x3, [x18, #0x10]
  ldp x4, x5, [x18, #0x20]
  ldp x6, x7, [x18, #0x30]
  ldp x8, x9, [x18, #0x40]
  ldp x10, x11, [x18, #0x50]
  ldp x12, x13, [x18, #0x60]
  ldp x14, x15, [x18, #0x70]
  ldp x16, x17, [x18, #0x80]
  ldp x19, x20, [x18, #0x98]
  ldp x21, x22, [x18, #0xa8]
  ldp x23, x24, [x18, #0xb8]
  ldp x25, x26, [x18, #0xc8]
  ldp x27, x28, [x18, #0xd8]
  ldp x29, x30, [x18, #0xe8]
  ldr x18, [x18, #0x90]
  eret
.globl _ttc_monitor_el0_irq_handler_end
_ttc_monitor_el0_irq_handler_end:

.globl _ttc_monitor_unexpected_handler
_ttc_monitor_unexpected_handler:
  mrs x17, tpidr_el1
  mrs x18, esr_el1
  str x18, [x17, #TTC_STATE_INTERNAL_ESR_OFFSET]
  mrs x18, far_el1
  str x18, [x17, #TTC_STATE_INTERNAL_FAR_OFFSET]
  mrs x18, elr_el1
  str x18, [x17, #TTC_STATE_INTERNAL_ELR_OFFSET]
  str x16, [x17, #TTC_STATE_INTERNAL_VECTOR_OFFSET]
  ldr x18, [x17, #TTC_STATE_PSTATE_OFFSET]
  str x18, [x17, #TTC_STATE_INTERNAL_INPUT_PSTATE_OFFSET]
  dsb ishst
  hvc #0x7f
1: b 1b
.globl _ttc_monitor_unexpected_handler_end
_ttc_monitor_unexpected_handler_end:
