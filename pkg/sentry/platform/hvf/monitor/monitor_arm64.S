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

// ttc_monitor_enter_application_asm restores the complete EL0 GP image from
// x0 and enters at the supplied ELR/SPSR. FP/SIMD state is transported by the
// architecture-specific HVF register adapter.
.globl _ttc_monitor_enter_application_asm
_ttc_monitor_enter_application_asm:
  // A fixed worker vCPU may run a different Linux task on every entry. An
  // exclusive reservation belongs to that execution context, not to the
  // physical vCPU; retaining it across contexts can make an unrelated STXR
  // succeed and corrupt userspace atomics.
  clrex
  mov x18, x0
  add x18, x18, #2, lsl #12
  add x18, x18, #0x908
  ldr x16, [x18, #TTC_APP_SP_OFFSET]
  msr sp_el0, x16
  ldr x16, [x18, #TTC_APP_PC_OFFSET]
  msr elr_el1, x16
  ldr x16, [x18, #TTC_APP_PSTATE_OFFSET]
  msr spsr_el1, x16
  ldr x16, [x18, #TTC_APP_TPIDR_OFFSET]
  msr tpidr_el0, x16
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

// Host cancellation may catch application entry while EL1 still has DAIF
// masked. HVF samples and clears pending-interrupt inputs on each run, so an
// IRQ injected in that state can be lost before the subsequent ERET reaches
// EL0. Since no application instruction has executed yet, preserve the input
// register image and acknowledge the interruption directly from EL1.
.globl _ttc_monitor_cancel_application
_ttc_monitor_cancel_application:
  mov x16, #TTC_EXIT_INTERRUPTED
  str x16, [x0, #TTC_STATE_EXIT_KIND_OFFSET]
  dsb ishst
  hvc #0x72
1: b 1b

// Exception vectors. Only lower-EL AArch64 synchronous exceptions are normal;
// every other vector exits as an internal monitor failure.
.p2align 11
.globl _ttc_monitor_vectors
_ttc_monitor_vectors:
.set ttc_vector_number, 0
.rept 8
  mov x16, #ttc_vector_number
  b _ttc_monitor_unexpected_handler
  .space 120
  .set ttc_vector_number, ttc_vector_number + 1
.endr

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

.set ttc_vector_number, 10
.rept 6
  mov x16, #ttc_vector_number
  b _ttc_monitor_unexpected_handler
  .space 120
  .set ttc_vector_number, ttc_vector_number + 1
.endr

.globl _ttc_monitor_vectors_end
_ttc_monitor_vectors_end:

.globl _ttc_monitor_el0_sync_handler
_ttc_monitor_el0_sync_handler:
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
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
  mov x17, #TTC_EXIT_FAULT
  mov x18, #TTC_EXIT_SYSCALL
  csel x17, x18, x17, eq
  str x17, [x16, #TTC_APP_EXIT_KIND_OFFSET]
  hvc #0x72
1: b 1b
.globl _ttc_monitor_el0_sync_handler_end
_ttc_monitor_el0_sync_handler_end:

.globl _ttc_monitor_el0_irq_handler
_ttc_monitor_el0_irq_handler:
  sub sp, sp, #32
  stp x16, x17, [sp, #0]
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
  str xzr, [x16, #TTC_APP_ESR_OFFSET]
  str xzr, [x16, #TTC_APP_FAR_OFFSET]
  mov x17, #TTC_EXIT_INTERRUPTED
  str x17, [x16, #TTC_APP_EXIT_KIND_OFFSET]
  hvc #0x72
1: b 1b
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
