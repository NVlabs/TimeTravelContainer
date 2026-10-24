// Copyright 2020 The gVisor Authors.
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

/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef THIRD_PARTY_GVISOR_PKG_SENTRY_PLATFORM_SYSTRAP_SYSMSG_SYSMSG_H_
#define THIRD_PARTY_GVISOR_PKG_SENTRY_PLATFORM_SYSTRAP_SYSMSG_SYSMSG_H_

#include <stdbool.h>
#include <stdint.h>
#include <sys/types.h>
#include <sys/user.h>

#include "sysmsg_offsets.h"  // NOLINT

#if defined(__x86_64__)
// LINT.IfChange
struct arch_state {
  uint32_t xsave_mode;
  uint32_t fp_len;
  uint32_t fsgsbase;
};
// LINT.ThenChange(sysmsg_amd64.go)
#else
// LINT.IfChange
struct arch_state {
  uint32_t fp_len;
};
// LINT.ThenChange(sysmsg_arm64.go)
#endif

// LINT.IfChange
enum thread_state {
  THREAD_STATE_NONE,
  THREAD_STATE_DONE,
  THREAD_STATE_PREP,
  THREAD_STATE_ASLEEP,
  THREAD_STATE_INITIALIZING,
};

struct thread_context;

// sysmsg contains the current state of the sysmsg thread. See: sysmsg.go:Msg
struct sysmsg {
  struct sysmsg *self;
  uint64_t ret_addr;
  uint64_t syshandler;
  uint64_t syshandler_stack;
  uint64_t app_stack;
  uint32_t interrupt;
  uint32_t state;
  struct thread_context *context;

  // The fields above have offsets defined in sysmsg_offsets*.h

  int32_t fault_jump;
  int32_t err;
  int32_t err_additional;
  int32_t err_line;
  uint64_t debug;
  uint32_t thread_id;
  char thread_name[16];
  // placement_id is the placement this thread last confined itself to; see
  // thread_context.placement_id.
  uint32_t placement_id;
  // frame and frame_siginfo locate the signal frame and its siginfo, at the
  // top of the alternate stack, of the last signal the handler resumes a
  // context from. The frame keeps the registers of the context it
  // interrupted until another signal replaces them; frame_scrubbed is set
  // once the thread zeroed them (see scrub_stacks), and makes the handler
  // restore the floating point state of the context it resumes from the
  // context.
  void *frame;
  void *frame_siginfo;
  uint32_t frame_scrubbed;
};

enum context_state {
  CONTEXT_STATE_NONE,
  CONTEXT_STATE_SYSCALL,
  CONTEXT_STATE_FAULT,
  CONTEXT_STATE_SYSCALL_TRAP,
  CONTEXT_STATE_SYSCALL_NEED_TRAP,
  CONTEXT_STATE_UNEXPECTED_DEATH,
  CONTEXT_STATE_INVALID,
};

// thread_context contains the current context of the sysmsg thread.
// See sysmsg.go:SysThreadContext
struct thread_context {
  uint8_t fpstate[MAX_FPSTATE_LEN];
  uint64_t fpstate_changed;
  struct user_regs_struct ptregs;

  // The fields above have offsets defined in sysmsg_offsets*.h

  siginfo_t siginfo;
  int64_t signo;
  uint32_t state;
  uint32_t interrupt;
  uint32_t thread_id;
  uint32_t last_thread_id;
  uint32_t sentry_fast_path;
  // guest_gs is non-zero when ptregs.gs_base, rather than the systrap sysmsg
  // pointer, must be installed while this context executes application code.
  uint32_t guest_gs;
  uint64_t acked_time;
  uint64_t state_changed_time;
  uint64_t tls;
  uint64_t debug;
  uint64_t err;
  char thread_name[16];
  // host_cpu is, as Linux's getcpu vDSO reads it from the GDT, (node << 12) |
  // cpu of the host CPU that last ran the context, which a stub thread
  // records as it gives a context with a placement back to the Sentry.
  uint32_t host_cpu;
  // placement_id names cpu_mask, the host CPUs the context may run on: a
  // stub thread that takes the context confines itself to cpu_mask first,
  // unless the placement it confined itself to last has the same ID. Zero is
  // no placement: the context runs wherever its stub thread does.
  uint32_t placement_id;
  uint64_t cpu_mask[PLACEMENT_CPU_MASK_WORDS];
};

enum stub_error {
  STUB_ERROR_BAD_SYSMSG = 0x0bad0000,
  STUB_ERROR_BAD_THREAD_STATE,
  STUB_ERROR_SPINNING_QUEUE_DECREF,
  STUB_ERROR_ARCH_PRCTL,
  STUB_ERROR_FUTEX,
  STUB_ERROR_BAD_CONTEXT_ID,
  STUB_ERROR_FPSTATE_BAD_HEADER,
};

#ifndef PAGE_SIZE
#define PAGE_SIZE 4096
#endif

#if PAGE_SIZE == 65536
#define PER_THREAD_MEM_SIZE (5 * PAGE_SIZE)
#else
#define PER_THREAD_MEM_SIZE (8 * PAGE_SIZE)
#endif

#define GUARD_SIZE (PAGE_SIZE)
#define MSG_OFFSET_FROM_START (PER_THREAD_MEM_SIZE - PAGE_SIZE)

// The private stack the syscall handler runs on, and the alternate stack the
// signal handler runs on, below the sysmsg structure.
#define PER_THREAD_PRIVATE_STACK_OFFSET GUARD_SIZE
#if PAGE_SIZE == 65536
#define PER_THREAD_PRIVATE_STACK_SIZE (1 * PAGE_SIZE)
#define PER_THREAD_SHARED_STACK_OFFSET (3 * PAGE_SIZE)
#else
#define PER_THREAD_PRIVATE_STACK_SIZE (2 * PAGE_SIZE)
#define PER_THREAD_SHARED_STACK_OFFSET (4 * PAGE_SIZE)
#endif

#define SPINNING_QUEUE_MEM_SIZE PAGE_SIZE
// LINT.ThenChange(sysmsg.go)

#define FAULT_OPCODE 0x06  // "push %es" on x32 and invalid opcode on x64.

#define __stringify_1(x...) #x
#define __stringify(x...) __stringify_1(x)
#define BUILD_BUG_ON(condition) ((void)sizeof(char[1 - 2 * !!(condition)]))

extern uint64_t __export_pr_sched_core;
extern uint64_t __export_deep_sleep_timeout;
extern struct arch_state __export_arch_state;
struct context_queue;
extern struct context_queue *__export_context_queue_addr;

// NOLINTBEGIN(runtime/int)
static void *sysmsg_sp() {
  volatile int p;
  void *sp =
      (struct sysmsg *)(((long)&p) / PER_THREAD_MEM_SIZE * PER_THREAD_MEM_SIZE);

  _Static_assert(
      sizeof(struct sysmsg) < (PER_THREAD_MEM_SIZE - MSG_OFFSET_FROM_START),
      "The sysmsg structure is too big.");
  return sp;
}

static struct sysmsg *sysmsg_addr(void *sp) {
  return (struct sysmsg *)(sp + MSG_OFFSET_FROM_START);
}

// on_alt_stack reports whether addr is in the alternate stack of the thread
// of sysmsg, below the sysmsg structure.
static bool on_alt_stack(struct sysmsg *sysmsg, uint64_t addr) {
  uint64_t start = (uint64_t)sysmsg - MSG_OFFSET_FROM_START +
                   PER_THREAD_SHARED_STACK_OFFSET;
  return addr >= start && addr < (uint64_t)sysmsg;
}

long __syscall(long n, long a1, long a2, long a3, long a4, long a5, long a6);

struct __kernel_timespec;
long sys_futex(uint32_t *addr, int op, int val, struct __kernel_timespec *tv,
               uint32_t *addr2, int val3);

static void __panic(int err, int err_additional, long line) {
  void *sp = sysmsg_sp();
  struct sysmsg *sysmsg = sysmsg_addr(sp);
  struct thread_context *ctx = sysmsg->context;
  sysmsg->err = err;
  sysmsg->err_additional = err_additional;
  sysmsg->err_line = line;
  // Wake up the goroutine waiting on the current context.
  __atomic_store_n(&ctx->state, CONTEXT_STATE_FAULT, __ATOMIC_RELEASE);
  sys_futex(&ctx->state, FUTEX_WAKE, 1, NULL, NULL, 666);
  // crash the stub process.
  //
  // Normal user processes cannot map addresses lower than vm.mmap_min_addr
  // which is usually > 4K. So writing to an address <4K should crash the
  // process with a segfault.
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Warray-bounds"
  *(int *)(line % 4096) = err;
#pragma GCC diagnostic pop
}

void memcpy(uint8_t *dest, uint8_t *src, size_t n);

// zero zeroes n bytes at dest, which are 8-byte aligned, as memset does.
void zero(void *dest, size_t n);

// scrub_frame zeroes the registers that the signal frame of sysmsg->frame
// keeps and the handler does not need to resume a context from it.
void scrub_frame(struct sysmsg *sysmsg);

void __export_start(struct sysmsg *sysmsg, void *_ucontext);

void restore_state(struct sysmsg *sysmsg, struct thread_context *ctx,
                   void *_ucontext);

struct thread_context *switch_context(struct sysmsg *sysmsg,
                                      struct thread_context *ctx,
                                      enum context_state new_context_state);

int wait_state(struct sysmsg *sysmsg, enum thread_state new_thread_state);
void init_new_thread(void);

#define panic(err, err_additional) __panic(err, err_additional, __LINE__)
// NOLINTEND(runtime/int)

#endif  // THIRD_PARTY_GVISOR_PKG_SENTRY_PLATFORM_SYSTRAP_SYSMSG_SYSMSG_H_
