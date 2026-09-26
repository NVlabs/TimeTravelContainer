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

#ifndef GVISOR_PKG_SENTRY_PLATFORM_HVF_MONITOR_LAYOUT_H_
#define GVISOR_PKG_SENTRY_PLATFORM_HVF_MONITOR_LAYOUT_H_

#ifndef __ASSEMBLER__
#include <stdint.h>
#define TTC_MONITOR_MAGIC UINT64_C(0x3130554d4d435454)
#define TTC_MONITOR_CODE_VA UINT64_C(0xffff000000000000)
#else
#define TTC_MONITOR_MAGIC 0x3130554d4d435454
#define TTC_MONITOR_CODE_VA 0xffff000000000000
#endif
#define TTC_MONITOR_CODE_CAPACITY 0x00010000
#define TTC_MONITOR_VERSION 1
#define TTC_MONITOR_PAGE_SIZE UINT64_C(4096)
#define TTC_MONITOR_HOST_SLAB_SIZE UINT64_C(16384)
#define TTC_MONITOR_MAX_OPERATIONS 256
#define TTC_MONITOR_MAX_ADDRESS_SPACES 320
#define TTC_MONITOR_MAX_POOL_REGIONS 64

// Ring0 bridge calls. Application exits use a distinct call so the host can
// replace the monitor HVC PC with the saved Sentry continuation without first
// resuming that continuation at EL1 and faulting through the host-service gate.
#define TTC_RING0_HVC_REDPILL 0x73
#define TTC_RING0_HVC_HOST_GATE 0x74
#define TTC_RING0_HVC_INTERRUPT_ACK 0x75
// Cache maintenance for bytes written through a Darwin HVA. The bridge
// services this HVC in-place and resumes the same EL1 continuation without a
// redpill or full register transfer.
#define TTC_RING0_HVC_CACHE_SYNC 0x76

#define TTC_ADDRESS_SPACE_SENTRY UINT64_C(1)
#define TTC_ADDRESS_SPACE_FLAGS_MASK TTC_ADDRESS_SPACE_SENTRY

#define TTC_MONITOR_COMMAND_MAX_SIZE (64 + TTC_MONITOR_MAX_OPERATIONS * 40)

// Per-vCPU state-page offsets. The command buffer and application register
// image intentionally share one permanently mapped 16 KiB page; monitor
// commands are serialized and never execute while that vCPU is in EL0.
#define TTC_STATE_CONTROL_VA_OFFSET 0x0000
#define TTC_STATE_CONTROL_GPA_OFFSET 0x0008
#define TTC_STATE_COMMAND_LENGTH_OFFSET 0x0010
#define TTC_STATE_COMMAND_OFFSET 0x0040
#define TTC_STATE_RESPONSE_OFFSET 0x2880
#define TTC_STATE_ADDRESS_SPACE_ID_OFFSET 0x2900
#define TTC_STATE_X0_OFFSET 0x2908
#define TTC_STATE_SP_OFFSET 0x2a00
#define TTC_STATE_PC_OFFSET 0x2a08
#define TTC_STATE_PSTATE_OFFSET 0x2a10
#define TTC_STATE_TPIDR_OFFSET 0x2a18
#define TTC_STATE_ESR_OFFSET 0x2a20
#define TTC_STATE_FAR_OFFSET 0x2a28
#define TTC_STATE_EXIT_KIND_OFFSET 0x2a30
#define TTC_STATE_INTERNAL_ESR_OFFSET 0x2a38
#define TTC_STATE_INTERNAL_FAR_OFFSET 0x2a40
#define TTC_STATE_INTERNAL_ELR_OFFSET 0x2a48
#define TTC_STATE_INTERNAL_VECTOR_OFFSET 0x2a50
#define TTC_STATE_INTERNAL_INPUT_PSTATE_OFFSET 0x2a58
#define TTC_STATE_CANCEL_REQUESTED_OFFSET 0x2a60
#define TTC_STATE_TLB_FLUSH_REQUESTED_OFFSET 0x2a68
#define TTC_STATE_PENDING_INTERRUPT_EXIT_KIND_OFFSET 0x2a70
#define TTC_STATE_SENTRY_TTBR0_OFFSET 0x2a80
#define TTC_STATE_SENTRY_X0_OFFSET 0x2a88
#define TTC_STATE_SENTRY_SP_OFFSET 0x2b80
#define TTC_STATE_SENTRY_PC_OFFSET 0x2b88
#define TTC_STATE_SENTRY_PSTATE_OFFSET 0x2b90
#define TTC_STATE_SENTRY_TPIDR_OFFSET 0x2b98
#define TTC_STATE_SENTRY_FP_OFFSET 0x2c00
#define TTC_STATE_APPLICATION_FP_OFFSET 0x3000
#define TTC_STATE_HOST_GATE_PC_OFFSET 0x3300
#define TTC_STATE_HOST_GATE_PSTATE_OFFSET 0x3308
#define TTC_STATE_HOST_GATE_ESR_OFFSET 0x3310
#define TTC_STATE_HOST_GATE_FAR_OFFSET 0x3318
#define TTC_STATE_SENTRY_TPIDRRO_OFFSET 0x3320
#define TTC_STATE_HOST_GATE_X16_OFFSET 0x3328
#define TTC_STATE_HOST_GATE_X17_OFFSET 0x3330
#define TTC_STATE_HOST_GATE_SP_OFFSET 0x3338
#define TTC_STATE_RING0_ENTRY_OFFSET 0x3340
// The Go-side entry trampoline records the Sentry continuation's stack here
// before switching to the permanently mapped per-vCPU monitor stack. The
// monitor must never make its first memory access through a movable Go stack:
// stack growth can occur after the host mirror was sampled.
#define TTC_STATE_RING0_SENTRY_SP_OFFSET 0x3348
// Selected application root retained while entry performs monitor-local cache
// and register work under the Sentry root. TTBR0 switches only immediately
// before the final EL0 ERET.
#define TTC_STATE_APPLICATION_TTBR0_OFFSET 0x3350
// Nonzero while MDSCR_EL1.SS is enabled for the current EL0 entry. Keeping
// this bit in monitor-owned state lets ordinary entries avoid serializing the
// instruction stream merely to write an already-disabled debug register.
#define TTC_STATE_DEBUG_STEP_ACTIVE_OFFSET 0x3358
// Hypervisor.framework defines guest CNTVCT as mach_absolute_time minus the
// VM-wide VTimer offset. Adding it back implements Darwin mach_absolute_time
// while leaving guest CNTVCT in Sentry's host architectural-counter domain.
#define TTC_STATE_VTIMER_OFFSET_OFFSET 0x3360
// Darwin's Go runtime reaches these two functions while Sentry is resident at
// EL1. The host publishes their process-local PCs plus a counter-to-realtime
// calibration so the monitor can provide the Linux-VDSO-equivalent fast path
// without returning to Darwin for ordinary runtime clock reads.
#define TTC_STATE_CLOCK_GETTIME_PC_OFFSET 0x3368
#define TTC_STATE_MACH_ABSOLUTE_TIME_PC_OFFSET 0x3370
#define TTC_STATE_TIMEBASE_NUMER_OFFSET 0x3378
#define TTC_STATE_TIMEBASE_DENOM_OFFSET 0x3380
#define TTC_STATE_REALTIME_OFFSET_NS_OFFSET 0x3388
// Nonzero while a resident Sentry continuation is executing monitor code that
// has no valid Darwin EL0 representation. The native cancellation bridge must
// resume the vCPU until this critical section has returned to ordinary Go.
#define TTC_STATE_GUEST_CRITICAL_OFFSET 0x3390
#define TTC_STATE_FP_Q0_OFFSET 0x0008

#define TTC_STATE_STACK_TOP 0x4000

#define TTC_APP_SP_OFFSET 0x00f8
#define TTC_APP_PC_OFFSET 0x0100
#define TTC_APP_PSTATE_OFFSET 0x0108
#define TTC_APP_TPIDR_OFFSET 0x0110
#define TTC_APP_ESR_OFFSET 0x0118
#define TTC_APP_FAR_OFFSET 0x0120
#define TTC_APP_EXIT_KIND_OFFSET 0x0128

#define TTC_EXIT_NONE 0
#define TTC_EXIT_SYSCALL 1
#define TTC_EXIT_FAULT 2
#define TTC_EXIT_INTERRUPTED 3
#define TTC_EXIT_EXCEPTION 4
#define TTC_EXIT_RETRY 6

#ifndef __ASSEMBLER__

enum ttc_monitor_opcode {
  TTC_OPCODE_INVALID = 0,
  TTC_OPCODE_CREATE_ADDRESS_SPACE = 1,
  TTC_OPCODE_DESTROY_ADDRESS_SPACE = 2,
  TTC_OPCODE_APPLY = 3,
  TTC_OPCODE_ACTIVATE = 4,
  TTC_OPCODE_EXPAND_PAGE_TABLE_POOL = 5,
};

enum ttc_monitor_operation_type {
  TTC_OPERATION_INVALID = 0,
  TTC_OPERATION_MAP = 1,
  TTC_OPERATION_UNMAP = 2,
  TTC_OPERATION_PROTECT = 3,
};

enum ttc_monitor_access {
  TTC_ACCESS_READ = 1,
  TTC_ACCESS_WRITE = 2,
  TTC_ACCESS_EXECUTE = 4,
};

enum ttc_monitor_status {
  TTC_STATUS_OK = 0,
  TTC_STATUS_MALFORMED = 1,
  TTC_STATUS_UNSUPPORTED = 2,
  TTC_STATUS_NO_MEMORY = 3,
  TTC_STATUS_NOT_FOUND = 4,
  TTC_STATUS_CONFLICT = 5,
  TTC_STATUS_INTERNAL = 6,
  // A fixed-size monitor table, such as the address-space table, is full.
  TTC_STATUS_LIMIT = 7,
};

struct ttc_monitor_pool_region {
  uint64_t gpa;
  uint64_t va;
  uint64_t length;
  uint64_t used;
};

struct ttc_monitor_address_space {
  uint64_t id;
  uint64_t root_gpa;
  uint16_t asid;
  uint16_t active;
  uint32_t flags;
};

struct ttc_monitor_mutation {
  uint64_t pte_va;
  uint64_t new_pte;
  uint64_t va;
};

// ttc_monitor_control is initialized by the host before bootstrap, then owned
// by the monitor. Host fields describe only permanently leased stage-2 memory;
// they are not page-table contents.
struct ttc_monitor_control {
  uint64_t magic;
  uint64_t version;
  uint64_t code_gpa;
  uint64_t code_va;
  uint64_t code_length;
  uint64_t control_gpa;
  uint64_t control_va;
  uint64_t states_gpa;
  uint64_t states_va;
  uint64_t state_count;
  uint64_t ttbr1_gpa;
  uint64_t next_pool_va;
  uint64_t pool_aperture_length;
  uint64_t next_asid;
  uint64_t pool_region_count;
  // Head of an intrusive GPA free list. Freed page-table pages store the next
  // GPA in their first word and are zeroed before reuse.
  uint64_t free_page_gpa;
  uint64_t reserved[4];
  struct ttc_monitor_pool_region pool[TTC_MONITOR_MAX_POOL_REGIONS];
  struct ttc_monitor_address_space spaces[TTC_MONITOR_MAX_ADDRESS_SPACES];
  struct ttc_monitor_mutation mutations[TTC_MONITOR_MAX_OPERATIONS];
};

_Static_assert(sizeof(struct ttc_monitor_control) <= TTC_MONITOR_HOST_SLAB_SIZE,
               "monitor control must fit in one host slab");

#endif  // !__ASSEMBLER__

#endif  // GVISOR_PKG_SENTRY_PLATFORM_HVF_MONITOR_LAYOUT_H_
