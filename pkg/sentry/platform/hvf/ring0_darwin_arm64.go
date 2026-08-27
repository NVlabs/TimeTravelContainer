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
#include <errno.h>
#include <mach/arm/thread_status.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#include <pthread.h>
#include <signal.h>
#include <stddef.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/ucontext.h>
#include <unistd.h>

#include "pkg/sentry/platform/hvf/monitor/monitor_layout.h"

#define TTC_RING0_BRIDGE_MAGIC UINT64_C(0x3145474449524354)
#define TTC_RING0_HVC_REDPILL UINT64_C(0x73)
#define TTC_RING0_HVC_HOST_GATE UINT64_C(0x74)
#define TTC_RING0_HOST_STACK_SIZE (UINT64_C(64) << 10)

struct ttc_ring0_region {
	uint64_t address;
	uint64_t length;
	uint64_t object_offset;
	uint64_t object_id;
	uint32_t protection;
	uint32_t user_tag;
};

static int ttc_ring0_snapshot_regions(uint64_t limit,
		struct ttc_ring0_region **regions_out, size_t *count_out) {
	struct ttc_ring0_region *regions = NULL;
	size_t count = 0;
	size_t capacity = 0;
	mach_vm_address_t address = 0;
	natural_t depth = 0;
	while (address < limit) {
		mach_vm_size_t length = 0;
		vm_region_submap_info_data_64_t info;
		mach_msg_type_number_t info_count = VM_REGION_SUBMAP_INFO_COUNT_64;
		kern_return_t kr = mach_vm_region_recurse(mach_task_self(),
				&address, &length, &depth,
				(vm_region_recurse_info_t)&info, &info_count);
		if (kr == KERN_INVALID_ADDRESS) break;
		if (kr != KERN_SUCCESS) {
			free(regions);
			return kr;
		}
		if (address >= limit) break;
		if (info.is_submap) {
			++depth;
			continue;
		}
		if (length == 0 || address + length < address) {
			free(regions);
			return EOVERFLOW;
		}
		if (length > limit - address) length = limit - address;
		if (info.protection != VM_PROT_NONE) {
			if (count == capacity) {
				size_t next = capacity == 0 ? 128 : capacity * 2;
				void *replacement = realloc(regions,
						next * sizeof(*regions));
				if (replacement == NULL) {
					free(regions);
					return ENOMEM;
				}
				regions = replacement;
				capacity = next;
			}
			regions[count++] = (struct ttc_ring0_region){
				.address = address,
				.length = length,
				.object_offset = info.offset,
				.object_id = info.object_id_full != 0 ?
					info.object_id_full : info.object_id,
				.protection = info.protection,
				.user_tag = info.user_tag,
			};
		}
		address += length;
	}
	*regions_out = regions;
	*count_out = count;
	return 0;
}

static int ttc_ring0_main_image(uint64_t address, uint64_t length) {
	const struct mach_header_64 *header =
		(const struct mach_header_64 *)_dyld_get_image_header(0);
	if (header == NULL || header->magic != MH_MAGIC_64) return 0;
	intptr_t slide = _dyld_get_image_vmaddr_slide(0);
	const uint8_t *command_bytes = (const uint8_t *)(header + 1);
	for (uint32_t i = 0; i < header->ncmds; ++i) {
		const struct load_command *command =
			(const struct load_command *)command_bytes;
		if (command->cmdsize < sizeof(*command)) return 0;
		if (command->cmd == LC_SEGMENT_64 &&
				command->cmdsize >= sizeof(struct segment_command_64)) {
			const struct segment_command_64 *segment =
				(const struct segment_command_64 *)command;
			uint64_t start = segment->vmaddr + (uint64_t)slide;
			uint64_t end = start + segment->vmsize;
			if (segment->initprot != VM_PROT_NONE && end >= start &&
					address >= start && address < end &&
					length <= end - address) {
				return 1;
			}
		}
		command_bytes += command->cmdsize;
	}
	return 0;
}

// ttc_ring0_copy_host_memory copies through Mach instead of dereferencing the
// source directly. Host VM regions may disappear after the region snapshot as
// the Go runtime grows and scavenges arenas. mach_vm_read_overwrite reports
// that race as an error; memmove would terminate the Sentry with SIGBUS.
static kern_return_t ttc_ring0_copy_host_memory(uintptr_t destination,
		uintptr_t source, uint64_t length) {
	mach_vm_size_t copied = 0;
	kern_return_t status = mach_vm_read_overwrite(mach_task_self(),
			(mach_vm_address_t)source, (mach_vm_size_t)length,
			(mach_vm_address_t)destination, &copied);
	if (status != KERN_SUCCESS) return status;
	return copied == (mach_vm_size_t)length ? KERN_SUCCESS : KERN_FAILURE;
}

struct ttc_ring0_bridge {
	uint64_t magic;
	hv_vcpu_t vcpu;
	const hv_vcpu_exit_t *exit;
	uint64_t ttbr0;
	uint64_t state_va;
	uintptr_t state_host;
	uint32_t destroy_on_host;
	_Atomic uint32_t error;
	_Atomic uint32_t error_phase;
	_Atomic uint32_t in_guest;
	_Atomic uint32_t destroyed;
	_Atomic uint64_t last_x0;
	_Atomic uint64_t last_host_x0;
	_Atomic uint64_t last_cancel_pc;
	_Atomic uint64_t last_cancel_pstate;
	_Atomic uint64_t cancel_count;
	uint64_t owner_tid;
	_Atomic uint64_t preflight_tid;
	_Atomic uint64_t entry_tid;
	_Atomic uint32_t preflight_status;
	_Atomic uint32_t preflight_stack_status;
	_Atomic uint32_t native_probe_status;
	ucontext_t host_context;
	struct __darwin_mcontext64 host_mcontext;
	void *host_stack;
	size_t host_stack_size;
	uint64_t bootstrap[9];
};

struct ttc_ring0_bootstrap {
	uint64_t pc;
	uint64_t sp;
	uint64_t fp;
	uint64_t lr;
	uint64_t x8;
	uint64_t x9;
	uint64_t x18;
	uint64_t x28;
	uint64_t x19;
};

_Static_assert(sizeof(struct ttc_ring0_bootstrap) == 72,
		"ring0 bootstrap ABI changed");

#define TTC_RING0_BRIDGE_ERROR_OFFSET \
	offsetof(struct ttc_ring0_bridge, error)
#define TTC_RING0_BRIDGE_ERROR_PHASE_OFFSET \
	offsetof(struct ttc_ring0_bridge, error_phase)
#define TTC_RING0_BRIDGE_IN_GUEST_OFFSET \
	offsetof(struct ttc_ring0_bridge, in_guest)
#define TTC_RING0_BRIDGE_DESTROYED_OFFSET \
	offsetof(struct ttc_ring0_bridge, destroyed)
#define TTC_RING0_BRIDGE_LAST_X0_OFFSET \
	offsetof(struct ttc_ring0_bridge, last_x0)
#define TTC_RING0_BRIDGE_LAST_HOST_X0_OFFSET \
	offsetof(struct ttc_ring0_bridge, last_host_x0)
#define TTC_RING0_BRIDGE_LAST_CANCEL_PC_OFFSET \
	offsetof(struct ttc_ring0_bridge, last_cancel_pc)
#define TTC_RING0_BRIDGE_LAST_CANCEL_PSTATE_OFFSET \
	offsetof(struct ttc_ring0_bridge, last_cancel_pstate)
#define TTC_RING0_BRIDGE_CANCEL_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, cancel_count)

static uintptr_t ttc_ring0_get_pc(const arm_thread_state64_t *ss) {
	return __darwin_arm_thread_state64_get_pc(*ss);
}

static uintptr_t ttc_ring0_get_sp(const arm_thread_state64_t *ss) {
	return __darwin_arm_thread_state64_get_sp(*ss);
}

static uintptr_t ttc_ring0_get_fp(const arm_thread_state64_t *ss) {
	return __darwin_arm_thread_state64_get_fp(*ss);
}

static uintptr_t ttc_ring0_get_lr(const arm_thread_state64_t *ss) {
	return __darwin_arm_thread_state64_get_lr(*ss);
}

static void ttc_ring0_set_pc(arm_thread_state64_t *ss, uintptr_t value) {
	__darwin_arm_thread_state64_set_pc_fptr(*ss, (void (*)(void))value);
}

static void ttc_ring0_set_sp(arm_thread_state64_t *ss, uintptr_t value) {
	__darwin_arm_thread_state64_set_sp(*ss, value);
}

static void ttc_ring0_set_fp(arm_thread_state64_t *ss, uintptr_t value) {
	__darwin_arm_thread_state64_set_fp(*ss, value);
}

static void ttc_ring0_set_lr(arm_thread_state64_t *ss, uintptr_t value) {
	__darwin_arm_thread_state64_set_lr_fptr(*ss, (void (*)(void))value);
}

static uint64_t ttc_ring0_read_tpidr_el0(void) {
	uint64_t value;
	__asm__ volatile("mrs %0, tpidr_el0" : "=r"(value));
	return value;
}

static uint64_t ttc_ring0_read_tpidrro_el0(void) {
	uint64_t value;
	__asm__ volatile("mrs %0, tpidrro_el0" : "=r"(value));
	return value;
}

static void ttc_ring0_write_tpidr_el0(uint64_t value) {
	__asm__ volatile("msr tpidr_el0, %0" : : "r"(value));
}

static size_t ttc_ring0_append_literal(char *out, size_t offset,
		const char *literal) {
	while (*literal != '\0') out[offset++] = *literal++;
	return offset;
}

static size_t ttc_ring0_append_hex(char *out, size_t offset, uint64_t value) {
	static const char digits[] = "0123456789abcdef";
	out[offset++] = '0';
	out[offset++] = 'x';
	for (int shift = 60; shift >= 0; shift -= 4) {
		out[offset++] = digits[(value >> shift) & 15];
	}
	return offset;
}

static uint64_t ttc_ring0_state_u64(const struct ttc_ring0_bridge *bridge,
		size_t offset) {
	uint64_t value = 0;
	__builtin_memcpy(&value,
			(const uint8_t *)bridge->state_host + offset, sizeof(value));
	return value;
}

static void ttc_ring0_fatal_diagnostic(struct ttc_ring0_bridge *bridge,
		uint32_t status) {
	char out[768];
	size_t n = 0;
#define TTC_DIAG_FIELD(label, value) do { \
	n = ttc_ring0_append_literal(out, n, label); \
	n = ttc_ring0_append_hex(out, n, (uint64_t)(value)); \
} while (0)
	n = ttc_ring0_append_literal(out, n, "ttc ring0 bridge error: ");
	TTC_DIAG_FIELD("status=", status);
	TTC_DIAG_FIELD(" phase=", atomic_load_explicit(&bridge->error_phase,
			memory_order_acquire));
	TTC_DIAG_FIELD(" vcpu=", bridge->vcpu);
	TTC_DIAG_FIELD(" owner_tid=", bridge->owner_tid);
	TTC_DIAG_FIELD(" preflight_tid=", atomic_load_explicit(
			&bridge->preflight_tid, memory_order_acquire));
	TTC_DIAG_FIELD(" entry_tid=", atomic_load_explicit(
			&bridge->entry_tid, memory_order_acquire));
	TTC_DIAG_FIELD(" preflight_status=", atomic_load_explicit(
			&bridge->preflight_status, memory_order_acquire));
	TTC_DIAG_FIELD(" preflight_stack_status=", atomic_load_explicit(
			&bridge->preflight_stack_status, memory_order_acquire));
	TTC_DIAG_FIELD(" native_probe_status=", atomic_load_explicit(
			&bridge->native_probe_status, memory_order_acquire));
	TTC_DIAG_FIELD(" reason=", bridge->exit->reason);
	TTC_DIAG_FIELD(" syndrome=", bridge->exit->exception.syndrome);
	TTC_DIAG_FIELD(" exit=", ttc_ring0_state_u64(bridge,
			TTC_STATE_EXIT_KIND_OFFSET));
	TTC_DIAG_FIELD(" app_esr=", ttc_ring0_state_u64(bridge,
			TTC_STATE_ESR_OFFSET));
	TTC_DIAG_FIELD(" internal_esr=", ttc_ring0_state_u64(bridge,
			TTC_STATE_INTERNAL_ESR_OFFSET));
	TTC_DIAG_FIELD(" internal_elr=", ttc_ring0_state_u64(bridge,
			TTC_STATE_INTERNAL_ELR_OFFSET));
	TTC_DIAG_FIELD(" internal_vector=", ttc_ring0_state_u64(bridge,
			TTC_STATE_INTERNAL_VECTOR_OFFSET));
	TTC_DIAG_FIELD(" host_gate_esr=", ttc_ring0_state_u64(bridge,
			TTC_STATE_HOST_GATE_ESR_OFFSET));
	TTC_DIAG_FIELD(" host_gate_far=", ttc_ring0_state_u64(bridge,
			TTC_STATE_HOST_GATE_FAR_OFFSET));
	TTC_DIAG_FIELD(" host_gate_pc=", ttc_ring0_state_u64(bridge,
			TTC_STATE_HOST_GATE_PC_OFFSET));
#undef TTC_DIAG_FIELD
	out[n++] = '\n';
	(void)write(STDERR_FILENO, out, n);
}

enum ttc_ring0_error_phase {
	TTC_RING0_PHASE_NONE = 0,
	TTC_RING0_PHASE_LOAD_X0 = 0x100,
	TTC_RING0_PHASE_LOAD_X29 = 0x11d,
	TTC_RING0_PHASE_LOAD_X30 = 0x11e,
	TTC_RING0_PHASE_LOAD_SP = 0x120,
	TTC_RING0_PHASE_LOAD_PC = 0x121,
	TTC_RING0_PHASE_LOAD_CPSR = 0x122,
	TTC_RING0_PHASE_LOAD_Q0 = 0x200,
	TTC_RING0_PHASE_LOAD_FPSR = 0x220,
	TTC_RING0_PHASE_LOAD_FPCR = 0x221,
	TTC_RING0_PHASE_LOAD_TPIDR_EL0 = 0x300,
	TTC_RING0_PHASE_LOAD_TPIDRRO_EL0 = 0x301,
	TTC_RING0_PHASE_LOAD_TTBR0_EL1 = 0x302,
	TTC_RING0_PHASE_RUN = 0x400,
	TTC_RING0_PHASE_CANCEL_PSTATE = 0x401,
	TTC_RING0_PHASE_CANCEL_PC = 0x402,
	TTC_RING0_PHASE_CANCEL_IRQ = 0x403,
	TTC_RING0_PHASE_SAVE_CONTEXT = 0x500,
	TTC_RING0_PHASE_UNEXPECTED_EXIT = 0x600,
};

static int ttc_ring0_load_context(struct ttc_ring0_bridge *bridge,
			ucontext_t *ucontext, uint32_t *phase_out) {
	arm_thread_state64_t *ss = &ucontext->uc_mcontext->__ss;
	arm_neon_state64_t *ns = &ucontext->uc_mcontext->__ns;
	hv_return_t status;
	*phase_out = TTC_RING0_PHASE_NONE;
	for (int i = 0; i < 29; ++i) {
		*phase_out = TTC_RING0_PHASE_LOAD_X0 + (uint32_t)i;
		status = hv_vcpu_set_reg(bridge->vcpu, (hv_reg_t)(HV_REG_X0 + i),
				ss->__x[i]);
		if (status != HV_SUCCESS) return status;
	}
	*phase_out = TTC_RING0_PHASE_LOAD_X29;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_X29,
			ttc_ring0_get_fp(ss))) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_X30;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_X30,
			ttc_ring0_get_lr(ss))) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_SP;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_SP_EL1,
			ttc_ring0_get_sp(ss))) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_PC;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_PC,
			ttc_ring0_get_pc(ss))) != HV_SUCCESS) return status;
	uint64_t pstate = ((uint64_t)ss->__cpsr & UINT64_C(0xf0000000)) |
		UINT64_C(0x3c5);  // EL1h, DAIF masked.
	*phase_out = TTC_RING0_PHASE_LOAD_CPSR;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_CPSR, pstate)) !=
			HV_SUCCESS) return status;
	for (int i = 0; i < 32; ++i) {
		hv_simd_fp_uchar16_t value;
		__builtin_memcpy(&value, &ns->__v[i], sizeof(value));
		*phase_out = TTC_RING0_PHASE_LOAD_Q0 + (uint32_t)i;
		status = hv_vcpu_set_simd_fp_reg(bridge->vcpu,
				(hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), value);
		if (status != HV_SUCCESS) return status;
	}
	*phase_out = TTC_RING0_PHASE_LOAD_FPSR;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_FPSR,
			ns->__fpsr)) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_FPCR;
	if ((status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_FPCR,
			ns->__fpcr)) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_TPIDR_EL0;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TPIDR_EL0,
			ttc_ring0_read_tpidr_el0())) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_TPIDRRO_EL0;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TPIDRRO_EL0,
			ttc_ring0_read_tpidrro_el0())) != HV_SUCCESS) return status;
	*phase_out = TTC_RING0_PHASE_LOAD_TTBR0_EL1;
	return hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TTBR0_EL1,
			bridge->ttbr0);
}

static int ttc_ring0_save_context(struct ttc_ring0_bridge *bridge,
		ucontext_t *ucontext, int advance_pc) {
	arm_thread_state64_t *ss = &ucontext->uc_mcontext->__ss;
	arm_neon_state64_t *ns = &ucontext->uc_mcontext->__ns;
	hv_return_t status;
	uint64_t value;
	for (int i = 0; i < 29; ++i) {
		status = hv_vcpu_get_reg(bridge->vcpu, (hv_reg_t)(HV_REG_X0 + i),
				&ss->__x[i]);
		if (status != HV_SUCCESS) return status;
	}
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_X29, &value)) !=
			HV_SUCCESS) return status;
	ttc_ring0_set_fp(ss, value);
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_X30, &value)) !=
			HV_SUCCESS) return status;
	ttc_ring0_set_lr(ss, value);
	if ((status = hv_vcpu_get_sys_reg(bridge->vcpu, HV_SYS_REG_SP_EL1,
			&value)) !=
			HV_SUCCESS) return status;
	ttc_ring0_set_sp(ss, value);
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_PC, &value)) !=
			HV_SUCCESS) return status;
	// Hypervisor.framework reports HVC exits with PC at the following
	// instruction. Other exit classes may request an explicit adjustment.
	ttc_ring0_set_pc(ss, value + (advance_pc ? 4 : 0));
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_CPSR, &value)) !=
			HV_SUCCESS) return status;
	ss->__cpsr = (ss->__cpsr & ~UINT32_C(0xf0000000)) |
		(uint32_t)(value & UINT64_C(0xf0000000));
	for (int i = 0; i < 32; ++i) {
		hv_simd_fp_uchar16_t fp;
		status = hv_vcpu_get_simd_fp_reg(bridge->vcpu,
				(hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), &fp);
		if (status != HV_SUCCESS) return status;
		__builtin_memcpy(&ns->__v[i], &fp, sizeof(fp));
	}
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_FPSR, &value)) !=
			HV_SUCCESS) return status;
	ns->__fpsr = (uint32_t)value;
	if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_FPCR, &value)) !=
			HV_SUCCESS) return status;
	ns->__fpcr = (uint32_t)value;
	if ((status = hv_vcpu_get_sys_reg(bridge->vcpu, HV_SYS_REG_TPIDR_EL0,
			&value)) != HV_SUCCESS) return status;
	ttc_ring0_write_tpidr_el0(value);
	return HV_SUCCESS;
}

// Hypervisor.framework requires a vCPU to be destroyed by its creating host
// thread. Every ring0 bridge invocation runs on that owner thread, so retire
// recyclable application vCPUs here, after their guest continuation has been
// copied back into the host signal context. Validation bridges opt out because
// they deliberately perform multiple entries on one vCPU.
static void ttc_ring0_finish_host_return(struct ttc_ring0_bridge *bridge) {
	if (bridge->destroy_on_host != 0) {
		hv_return_t status = hv_vcpu_destroy(bridge->vcpu);
		if (status != HV_SUCCESS) {
			atomic_store_explicit(&bridge->error, (uint32_t)status,
					memory_order_release);
			atomic_store_explicit(&bridge->in_guest, 0,
					memory_order_release);
			return;
		}
		atomic_store_explicit(&bridge->in_guest, 0,
				memory_order_release);
		// The restore SIGILL handler still needs the bridge-owned context.
		// It publishes destroyed only after copying that final state into the
		// kernel signal frame, at which point another thread may free bridge.
		return;
	}
	atomic_store_explicit(&bridge->in_guest, 0, memory_order_release);
}

// After the first SIGILL returns, host_entry runs outside signal context and
// may safely call Hypervisor.framework. A second controlled SIGILL gives the
// kernel a fresh signal-return frame into which the handler copies the final
// continuation. This is intentionally not setcontext(3): Darwin's deprecated
// implementation only restores callee-saved registers and forces X0 to zero.
// Kernel signal return restores the complete GP, SIMD, PC, SP, and flags state.
static void ttc_ring0_restore_trap(struct ttc_ring0_bridge *bridge)
		__attribute__((naked, noinline, noreturn));

static void ttc_ring0_restore_trap(
		__attribute__((unused)) struct ttc_ring0_bridge *bridge) {
	__asm__ volatile("mrs x1, tpidr_el1");
}

static void ttc_ring0_resume_host(struct ttc_ring0_bridge *bridge)
		__attribute__((noreturn));

static void ttc_ring0_resume_host(struct ttc_ring0_bridge *bridge) {
	ttc_ring0_restore_trap(bridge);
}

static uintptr_t ttc_ring0_strip_instruction_pointer(uintptr_t address) {
	// Apple Silicon implements address authentication even for an arm64 (rather
	// than arm64e) process, and Clang may materialize a signed C function
	// pointer. Exception PCs are canonical addresses, so compare canonical forms.
	__asm__ volatile("xpaci %0" : "+r"(address));
	return address;
}

// A partial continuation transfer cannot safely resume the guest-only tail
// of the assembly caller at host EL0. Restore the original context and mark
// X11 so the assembly wrapper returns directly to Go with bridge.error.
static void ttc_ring0_fail_host_return(struct ttc_ring0_bridge *bridge,
			ucontext_t *ucontext, const arm_thread_state64_t *host_ss,
			const arm_neon_state64_t *host_ns, uint32_t status,
			uint32_t phase) __attribute__((noreturn));

static void ttc_ring0_fail_host_return(struct ttc_ring0_bridge *bridge,
			ucontext_t *ucontext, const arm_thread_state64_t *host_ss,
			const arm_neon_state64_t *host_ns, uint32_t status,
			uint32_t phase) {
	ucontext->uc_mcontext->__ss = *host_ss;
	ucontext->uc_mcontext->__ns = *host_ns;
	ttc_ring0_set_pc(&ucontext->uc_mcontext->__ss,
			ttc_ring0_get_lr(host_ss));
	// bluepillTrap initializes X11 to zero. Its callers test this marker before
	// executing any guest-only tail after the controlled transfer returns.
	ucontext->uc_mcontext->__ss.__x[11] = 1;
	atomic_store_explicit(&bridge->error, status, memory_order_release);
	atomic_store_explicit(&bridge->error_phase, phase, memory_order_release);
	atomic_store_explicit(&bridge->in_guest, 0, memory_order_release);
	ttc_ring0_fatal_diagnostic(bridge, status);
	ttc_ring0_resume_host(bridge);
}

// ttc_ring0_host_entry runs after sigreturn, not inside the SIGILL handler.
// Hypervisor.framework register and run APIs are not signal-safe and may
// intermittently reject an otherwise valid owner-thread vCPU when invoked
// directly by a signal handler.
static void ttc_ring0_host_entry(struct ttc_ring0_bridge *bridge)
		__attribute__((noreturn));

static void ttc_ring0_host_entry(struct ttc_ring0_bridge *bridge) {
	ucontext_t *ucontext = &bridge->host_context;
	arm_thread_state64_t host_ss = ucontext->uc_mcontext->__ss;
	arm_neon_state64_t host_ns = ucontext->uc_mcontext->__ns;
	uint32_t phase = TTC_RING0_PHASE_NONE;
	int status = ttc_ring0_load_context(bridge, ucontext, &phase);
	if (status != HV_SUCCESS) {
		ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
				(uint32_t)status, phase);
	}
	atomic_store_explicit(&bridge->in_guest, 1, memory_order_release);
	for (;;) {
		status = hv_vcpu_run(bridge->vcpu);
		if (status != HV_SUCCESS) {
			ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
					&host_ns, (uint32_t)status, TTC_RING0_PHASE_RUN);
		}
		if (bridge->exit->reason == HV_EXIT_REASON_CANCELED) {
			uint64_t pstate = 0;
			status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_CPSR, &pstate);
			if (status != HV_SUCCESS) {
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_CANCEL_PSTATE);
			}
			uint64_t pc = 0;
			if ((status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_PC, &pc)) !=
					HV_SUCCESS) {
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_CANCEL_PC);
			}
			atomic_store_explicit(&bridge->last_cancel_pc, pc,
					memory_order_release);
			atomic_store_explicit(&bridge->last_cancel_pstate, pstate,
					memory_order_release);
			atomic_fetch_add_explicit(&bridge->cancel_count, 1,
					memory_order_release);
			if ((pstate & 0xf) == 0) {
				// Convert cancellation during EL0 execution to an architected
				// IRQ. The monitor snapshots the application and restores the
				// waiting Sentry continuation before execution proceeds.
				status = hv_vcpu_set_pending_interrupt(bridge->vcpu,
						HV_INTERRUPT_TYPE_IRQ, true);
				if (status == HV_SUCCESS) continue;
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_CANCEL_IRQ);
			}
			// A cancellation caught while an EL1 exception handler is restoring
			// the Sentry continuation must finish in the VM. Exporting a monitor
			// PC as a Darwin continuation would resume at an unmapped high VA.
			if (pc >= TTC_MONITOR_CODE_VA &&
					pc - TTC_MONITOR_CODE_VA < TTC_MONITOR_CODE_CAPACITY) {
				continue;
			}
			status = ttc_ring0_save_context(bridge, ucontext, 0);
			if (status != HV_SUCCESS) {
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_SAVE_CONTEXT);
			}
			ttc_ring0_finish_host_return(bridge);
			ttc_ring0_resume_host(bridge);
		}
		if (bridge->exit->reason == HV_EXIT_REASON_VTIMER_ACTIVATED) {
			(void)hv_vcpu_set_vtimer_mask(bridge->vcpu, true);
			continue;
		}
		uint64_t hvc = bridge->exit->exception.syndrome & 0xffff;
		if (bridge->exit->reason != HV_EXIT_REASON_EXCEPTION ||
				((bridge->exit->exception.syndrome >> 26) & 0x3f) != 0x16 ||
				(hvc != TTC_RING0_HVC_REDPILL &&
				 hvc != TTC_RING0_HVC_HOST_GATE)) {
			ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
					UINT32_MAX, TTC_RING0_PHASE_UNEXPECTED_EXIT);
		}
		status = ttc_ring0_save_context(bridge, ucontext, 0);
		uint64_t diagnostic_x0 = 0;
		if (hv_vcpu_get_reg(bridge->vcpu, HV_REG_X0,
				&diagnostic_x0) == HV_SUCCESS) {
			atomic_store_explicit(&bridge->last_x0, diagnostic_x0,
					memory_order_release);
		}
		atomic_store_explicit(&bridge->last_host_x0,
			ucontext->uc_mcontext->__ss.__x[0], memory_order_release);
		if (status == HV_SUCCESS && hvc == TTC_RING0_HVC_HOST_GATE) {
			// The EL1 vector published the pre-exception host continuation in the
			// monitor state page before issuing HVC. GP/FP/TLS came from the vCPU;
			// only PC and NZCV must reflect the exception input rather than the
			// monitor gateway itself.
			atomic_thread_fence(memory_order_acquire);
			const uint8_t *state = (const uint8_t *)bridge->state_host;
			uint64_t host_pc;
			uint64_t host_pstate;
			__builtin_memcpy(&host_pc,
					state + TTC_STATE_HOST_GATE_PC_OFFSET, sizeof(host_pc));
			__builtin_memcpy(&host_pstate,
					state + TTC_STATE_HOST_GATE_PSTATE_OFFSET,
					sizeof(host_pstate));
			if (host_pc >= TTC_MONITOR_CODE_VA &&
					host_pc - TTC_MONITOR_CODE_VA < TTC_MONITOR_CODE_CAPACITY) {
				// A current-EL fault in monitor code is never a Darwin service
				// request. Exporting its high VA to host EL0 would obscure the
				// original fault with a secondary SIGSEGV. Return the original
				// host continuation with a bridge error instead, so Sentry's
				// panic log and TTC's runtime diagnostic preserve the ESR/FAR/PC.
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
						UINT32_MAX, TTC_RING0_PHASE_UNEXPECTED_EXIT);
			}
			ttc_ring0_set_pc(&ucontext->uc_mcontext->__ss,
					(uintptr_t)host_pc);
			ucontext->uc_mcontext->__ss.__cpsr =
				(ucontext->uc_mcontext->__ss.__cpsr &
				 ~UINT32_C(0xf0000000)) |
				(uint32_t)(host_pstate & UINT64_C(0xf0000000));
		}
		if (status != HV_SUCCESS) {
			ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
					(uint32_t)status, TTC_RING0_PHASE_SAVE_CONTEXT);
		}
		ttc_ring0_finish_host_return(bridge);
		ttc_ring0_resume_host(bridge);
	}
}

static uintptr_t ttc_ring0_bluepill_pc;
static uintptr_t ttc_ring0_host_gate_pc;
static uintptr_t ttc_ring0_restore_pc;
static uintptr_t ttc_ring0_current_el_pc;
static struct sigaction ttc_ring0_previous_sigill;
static _Atomic uint32_t ttc_ring0_handler_installed;

static void ttc_ring0_forward_sigill(int signo, siginfo_t *info,
		void *context) {
	if ((ttc_ring0_previous_sigill.sa_flags & SA_SIGINFO) != 0) {
		ttc_ring0_previous_sigill.sa_sigaction(signo, info, context);
	} else if (ttc_ring0_previous_sigill.sa_handler == SIG_DFL ||
			ttc_ring0_previous_sigill.sa_handler == SIG_IGN) {
		// Restore the prior disposition and let the faulting instruction be
		// retried under it. This path is never used for a bridge instruction.
		sigaction(SIGILL, &ttc_ring0_previous_sigill, NULL);
	} else {
		ttc_ring0_previous_sigill.sa_handler(signo);
	}
}

static void ttc_ring0_sigill(int signo, siginfo_t *info, void *context) {
	ucontext_t *ucontext = (ucontext_t *)context;
	arm_thread_state64_t *ss = &ucontext->uc_mcontext->__ss;
	uintptr_t pc = ttc_ring0_get_pc(ss);
	if (pc == ttc_ring0_restore_pc) {
		struct ttc_ring0_bridge *bridge =
			(struct ttc_ring0_bridge *)(uintptr_t)ss->__x[0];
		if (bridge == NULL || bridge->magic != TTC_RING0_BRIDGE_MAGIC) {
			ttc_ring0_forward_sigill(signo, info, context);
			return;
		}
		// Keep the kernel-owned mcontext pointer from this signal frame. The
		// stored ucontext points at bridge-owned memory and is only the source
		// image for the final kernel sigreturn.
		struct __darwin_mcontext64 *live_mcontext = ucontext->uc_mcontext;
		*live_mcontext = bridge->host_mcontext;
		ucontext->uc_mcontext = live_mcontext;
		// Preserve the restore signal frame's kernel-owned metadata, but restore
		// the signal mask that was active at the original continuation.
		ucontext->uc_sigmask = bridge->host_context.uc_sigmask;
		if (bridge->destroy_on_host != 0) {
			// This is the final bridge access. Release pairs with the engine's
			// acquire before it recycles the vCPU ID and frees bridge.
			atomic_store_explicit(&bridge->destroyed, 1,
					memory_order_release);
		}
		return;
	}
	if (pc == ttc_ring0_host_gate_pc) {
		// HVC is undefined at host EL0. Skipping it makes EnsureHost an
		// inexpensive no-op for continuations that already redpilled.
		ttc_ring0_set_pc(ss, pc + 4);
		return;
	}
	if (pc == ttc_ring0_current_el_pc) {
		// CurrentEL is deliberately executed before selecting or creating an
		// owner-affine vCPU. At host EL0, report zero and skip the trapped MRS.
		ss->__x[0] = 0;
		ttc_ring0_set_pc(ss, pc + 4);
		return;
	}
	if (pc != ttc_ring0_bluepill_pc) {
		ttc_ring0_forward_sigill(signo, info, context);
		return;
	}
	struct ttc_ring0_bridge *bridge =
		(struct ttc_ring0_bridge *)(uintptr_t)ss->__x[8];
	if (bridge == NULL || bridge->magic != TTC_RING0_BRIDGE_MAGIC) {
		ttc_ring0_forward_sigill(signo, info, context);
		return;
	}
	// Copy the signal-provided context into bridge-owned storage before
	// sigreturn invalidates the kernel's frame. The handler performs no HVF
	// operation; normal host execution resumes in ttc_ring0_host_entry.
	bridge->host_context = *ucontext;
	bridge->host_mcontext = *ucontext->uc_mcontext;
	bridge->host_context.uc_mcontext = &bridge->host_mcontext;
	// host_entry is deliberately outside Go's registered gsignal stack. Keep
	// asynchronous runtime and process signals from nesting there; the second
	// controlled SIGILL remains unblocked so it can restore the saved mask and
	// continuation through the kernel's signal-return path.
	sigfillset(&ucontext->uc_sigmask);
	sigdelset(&ucontext->uc_sigmask, SIGILL);
	// Do not invoke an ordinary C function on the interrupted Go stack. It has
	// no cgo transition frame and may overwrite Go's caller metadata. The
	// bridge owns a native stack exclusively for this post-sigreturn path.
	ss->__x[0] = (uintptr_t)bridge;
	ttc_ring0_set_sp(ss, (uintptr_t)bridge->host_stack +
			bridge->host_stack_size);
	ttc_ring0_set_fp(ss, 0);
	ttc_ring0_set_lr(ss, 0);
	ttc_ring0_set_pc(ss, (uintptr_t)&ttc_ring0_host_entry);
}

static int ttc_ring0_install_sigill(uintptr_t bluepill_pc,
		uintptr_t host_gate_pc, uintptr_t current_el_pc) {
	if (atomic_load_explicit(&ttc_ring0_handler_installed,
			memory_order_acquire) != 0) {
		return ttc_ring0_bluepill_pc == bluepill_pc &&
			ttc_ring0_host_gate_pc == host_gate_pc &&
			ttc_ring0_current_el_pc == current_el_pc ? 0 : EBUSY;
	}
	struct sigaction action;
	__builtin_memset(&action, 0, sizeof(action));
	action.sa_sigaction = ttc_ring0_sigill;
	action.sa_flags = SA_SIGINFO | SA_NODEFER | SA_ONSTACK;
	sigfillset(&action.sa_mask);
	if (sigaction(SIGILL, &action, &ttc_ring0_previous_sigill) != 0) {
		return errno;
	}
	ttc_ring0_bluepill_pc = bluepill_pc;
	ttc_ring0_host_gate_pc = host_gate_pc;
	ttc_ring0_current_el_pc = current_el_pc;
	ttc_ring0_restore_pc = ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_restore_trap);
	atomic_store_explicit(&ttc_ring0_handler_installed, 1,
			memory_order_release);
	return 0;
}

static struct ttc_ring0_bridge *ttc_ring0_bridge_create(hv_vcpu_t vcpu,
		const hv_vcpu_exit_t *exit, uint64_t ttbr0, uint64_t state_va,
		uintptr_t state_host, int destroy_on_host) {
	struct ttc_ring0_bridge *bridge = calloc(1, sizeof(*bridge));
	if (bridge == NULL) return NULL;
	bridge->host_stack_size = TTC_RING0_HOST_STACK_SIZE;
	bridge->host_stack = mmap(NULL, bridge->host_stack_size,
			PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	if (bridge->host_stack == MAP_FAILED) {
		free(bridge);
		return NULL;
	}
	bridge->magic = TTC_RING0_BRIDGE_MAGIC;
	bridge->vcpu = vcpu;
	bridge->exit = exit;
	bridge->ttbr0 = ttbr0;
	bridge->state_va = state_va;
	bridge->state_host = state_host;
	bridge->destroy_on_host = destroy_on_host != 0;
	(void)pthread_threadid_np(NULL, &bridge->owner_tid);
	(void)pthread_sigmask(SIG_SETMASK, NULL, &bridge->host_context.uc_sigmask);
	return bridge;
}

static hv_return_t ttc_ring0_bridge_probe_reg(
		struct ttc_ring0_bridge *bridge) {
	uint64_t value = 0;
	hv_return_t status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_X0, &value);
	if (status != HV_SUCCESS) return status;
	return hv_vcpu_set_reg(bridge->vcpu, HV_REG_X0, value);
}

typedef hv_return_t (*ttc_ring0_stack_call_fn)(
		struct ttc_ring0_bridge *bridge);

// Run fn on stack_top while preserving the caller's stack and return address.
// This is diagnostic scaffolding for HVF's undocumented calling-context
// requirements; all arguments arrive in the standard ARM64 ABI registers.
static hv_return_t ttc_ring0_call_on_stack(
		__attribute__((unused)) struct ttc_ring0_bridge *bridge,
		__attribute__((unused)) uintptr_t stack_top,
		__attribute__((unused)) ttc_ring0_stack_call_fn fn)
		__attribute__((naked, noinline));

static hv_return_t ttc_ring0_call_on_stack(
		__attribute__((unused)) struct ttc_ring0_bridge *bridge,
		__attribute__((unused)) uintptr_t stack_top,
		__attribute__((unused)) ttc_ring0_stack_call_fn fn) {
	__asm__ volatile(
		"mov x9, sp\n"
		"mov sp, x1\n"
		"stp x9, x30, [sp, #-16]!\n"
		"blr x2\n"
		"ldp x9, x30, [sp], #16\n"
		"mov sp, x9\n"
		"ret\n");
}

static hv_return_t ttc_ring0_bridge_preflight(
		struct ttc_ring0_bridge *bridge) {
	uint64_t tid = 0;
	(void)pthread_threadid_np(NULL, &tid);
	atomic_store_explicit(&bridge->preflight_tid, tid,
			memory_order_release);
	hv_return_t status = ttc_ring0_bridge_probe_reg(bridge);
	atomic_store_explicit(&bridge->preflight_status, (uint32_t)status,
			memory_order_release);
	// Match host_entry's private stack while retaining an otherwise ordinary
	// cgo call. This distinguishes a stack-context restriction from the signal
	// transition itself in diagnostics.
	hv_return_t stack_status = ttc_ring0_call_on_stack(bridge,
			(uintptr_t)bridge->host_stack + bridge->host_stack_size,
			ttc_ring0_bridge_probe_reg);
	atomic_store_explicit(&bridge->preflight_stack_status,
			(uint32_t)stack_status, memory_order_release);
	return status;
}

static uint64_t ttc_ring0_bridge_owner_tid(
		const struct ttc_ring0_bridge *bridge) {
	return bridge->owner_tid;
}

static uint64_t ttc_ring0_bridge_preflight_tid(
		const struct ttc_ring0_bridge *bridge) {
	return atomic_load_explicit(&bridge->preflight_tid,
			memory_order_acquire);
}

static uintptr_t ttc_ring0_bridge_host_stack_top(
		const struct ttc_ring0_bridge *bridge) {
	return (uintptr_t)bridge->host_stack + bridge->host_stack_size;
}

static uintptr_t ttc_ring0_bridge_bootstrap(
		struct ttc_ring0_bridge *bridge) {
	return (uintptr_t)&bridge->bootstrap[0];
}

static uintptr_t ttc_ring0_bridge_probe_reg_address(void) {
	return ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_bridge_probe_reg);
}

// Enter HVF without first crossing a POSIX signal frame. In production each
// application bridge is one-shot: host_entry destroys the vCPU before the
// restore SIGILL, so any HVF state XNU associates with signal delivery is no
// longer observable on a subsequent entry.
static void ttc_ring0_native_entry(struct ttc_ring0_bridge *bridge,
		const struct ttc_ring0_bootstrap *bootstrap)
		__attribute__((noreturn));

static void ttc_ring0_native_entry(struct ttc_ring0_bridge *bridge,
		const struct ttc_ring0_bootstrap *bootstrap) {
	hv_return_t native_probe_status = ttc_ring0_bridge_probe_reg(bridge);
	atomic_store_explicit(&bridge->native_probe_status,
			(uint32_t)native_probe_status, memory_order_release);
	__builtin_memset(&bridge->host_mcontext, 0, sizeof(bridge->host_mcontext));
	bridge->host_context.uc_mcontext = &bridge->host_mcontext;
	arm_thread_state64_t *ss = &bridge->host_mcontext.__ss;
	ss->__x[8] = bootstrap->x8;
	ss->__x[9] = bootstrap->x9;
	ss->__x[18] = bootstrap->x18;
	ss->__x[28] = bootstrap->x28;
	ss->__x[19] = bootstrap->x19;
	ttc_ring0_set_pc(ss, bootstrap->pc);
	ttc_ring0_set_sp(ss, bootstrap->sp);
	ttc_ring0_set_fp(ss, bootstrap->fp);
	ttc_ring0_set_lr(ss, bootstrap->lr);

	ttc_ring0_host_entry(bridge);
}

static uintptr_t ttc_ring0_native_entry_address(void) {
	return ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_native_entry);
}

// ttc_ring0_bridge_probe_owner verifies that the bridge's vCPU is still
// associated with the calling host thread. Hypervisor.framework reports
// HV_BAD_ARGUMENT for register access through a stale handle or from a
// non-owning thread, which is otherwise indistinguishable once the controlled
// SIGILL transfer has begun.
static hv_return_t ttc_ring0_bridge_probe_owner(
		const struct ttc_ring0_bridge *bridge) {
	uint64_t value;
	return hv_vcpu_get_reg(bridge->vcpu, HV_REG_X0, &value);
}

static void ttc_ring0_bridge_destroy(struct ttc_ring0_bridge *bridge) {
	if (bridge == NULL) return;
	bridge->magic = 0;
	if (bridge->host_stack != NULL && bridge->host_stack != MAP_FAILED) {
		(void)munmap(bridge->host_stack, bridge->host_stack_size);
	}
	free(bridge);
}

static uint64_t ttc_monitor_address_space_ttbr(const void *memory,
		uint64_t id) {
	const struct ttc_monitor_control *ctl =
		(const struct ttc_monitor_control *)memory;
	for (uint64_t i = 0; i < TTC_MONITOR_MAX_ADDRESS_SPACES; ++i) {
		if (ctl->spaces[i].active && ctl->spaces[i].id == id) {
			return ctl->spaces[i].root_gpa |
				((uint64_t)ctl->spaces[i].asid << 48);
		}
	}
	return 0;
}
*/
import "C"

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

type ring0Bridge struct {
	pointer *C.struct_ttc_ring0_bridge
}

const (
	ring0BridgeErrorOffset            = uintptr(C.TTC_RING0_BRIDGE_ERROR_OFFSET)
	ring0BridgeErrorPhaseOffset       = uintptr(C.TTC_RING0_BRIDGE_ERROR_PHASE_OFFSET)
	ring0BridgeInGuestOffset          = uintptr(C.TTC_RING0_BRIDGE_IN_GUEST_OFFSET)
	ring0BridgeDestroyedOffset        = uintptr(C.TTC_RING0_BRIDGE_DESTROYED_OFFSET)
	ring0BridgeLastX0Offset           = uintptr(C.TTC_RING0_BRIDGE_LAST_X0_OFFSET)
	ring0BridgeLastHostX0Offset       = uintptr(C.TTC_RING0_BRIDGE_LAST_HOST_X0_OFFSET)
	ring0BridgeLastCancelPCOffset     = uintptr(C.TTC_RING0_BRIDGE_LAST_CANCEL_PC_OFFSET)
	ring0BridgeLastCancelPStateOffset = uintptr(C.TTC_RING0_BRIDGE_LAST_CANCEL_PSTATE_OFFSET)
	ring0BridgeCancelCountOffset      = uintptr(C.TTC_RING0_BRIDGE_CANCEL_COUNT_OFFSET)
)

type ring0HostRegion struct {
	address      uintptr
	length       uint64
	objectOffset uint64
	objectID     uint64
	protection   uint32
	mainImage    bool
}

const (
	ring0VMRead    = uint32(1)
	ring0VMWrite   = uint32(2)
	ring0VMExecute = uint32(4)
)

func snapshotRing0HostRegions(limit uint64) ([]ring0HostRegion, error) {
	var (
		regions *C.struct_ttc_ring0_region
		count   C.size_t
	)
	if status := C.ttc_ring0_snapshot_regions(C.uint64_t(limit), &regions, &count); status != 0 {
		return nil, fmt.Errorf("snapshotting Darwin VM regions: status %d", int(status))
	}
	defer C.free(unsafe.Pointer(regions))
	if uint64(count) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("Darwin VM region count %d overflows int", uint64(count))
	}
	snapshot := unsafe.Slice(regions, int(count))
	result := make([]ring0HostRegion, 0, len(snapshot))
	for _, region := range snapshot {
		address := uint64(region.address)
		length := uint64(region.length)
		if length == 0 || address%vmm.HostSlabSize != 0 || length%vmm.HostSlabSize != 0 || address+length < address || address+length > limit {
			return nil, fmt.Errorf("invalid Darwin VM region [%#x, %#x)", address, address+length)
		}
		result = append(result, ring0HostRegion{
			address:      uintptr(address),
			length:       length,
			objectOffset: uint64(region.object_offset),
			objectID:     uint64(region.object_id),
			protection:   uint32(region.protection),
			mainImage:    C.ttc_ring0_main_image(region.address, region.length) != 0,
		})
	}
	return result, nil
}

func copyRing0HostMemory(destination, source uintptr, length uint64) error {
	if status := C.ttc_ring0_copy_host_memory(C.uintptr_t(destination), C.uintptr_t(source), C.uint64_t(length)); status != C.KERN_SUCCESS {
		return fmt.Errorf("Mach copy [%#x, %#x) to %#x failed: %d", source, source+uintptr(length), destination, int(status))
	}
	return nil
}

func installRing0SignalBridge() error {
	if errno := C.ttc_ring0_install_sigill(
		C.uintptr_t(ring0asm.BluepillTrapAddress()),
		C.uintptr_t(ring0asm.EnsureHostTrapAddress()),
		C.uintptr_t(ring0asm.CurrentELTrapAddress())); errno != 0 {
		return fmt.Errorf("installing HVF ring0 SIGILL bridge: errno %d", int(errno))
	}
	return nil
}

func (v *hvfVCPU) newRing0Bridge(ttbr0 uint64, destroyOnHost bool) (*ring0Bridge, error) {
	if err := v.ensureBooted(); err != nil {
		return nil, err
	}
	pointer := C.ttc_ring0_bridge_create(
		v.handle,
		v.exit,
		C.uint64_t(ttbr0),
		C.uint64_t(v.stateVA),
		C.uintptr_t(v.stateHost),
		C.int(boolToInt(destroyOnHost)),
	)
	if pointer == nil {
		return nil, fmt.Errorf("allocating HVF ring0 bridge")
	}
	return &ring0Bridge{pointer: pointer}, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (b *ring0Bridge) close() {
	if b.pointer != nil {
		C.ttc_ring0_bridge_destroy(b.pointer)
		b.pointer = nil
	}
}

func (b *ring0Bridge) inGuest() bool {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeInGuestOffset))) != 0
}

func (b *ring0Bridge) destroyed() bool {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeDestroyedOffset))) != 0
}

func (b *ring0Bridge) markDestroyed() {
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeInGuestOffset)), 0)
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeDestroyedOffset)), 1)
}

func (b *ring0Bridge) errorStatus() uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeErrorOffset)))
}

func (b *ring0Bridge) errorPhase() uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeErrorPhaseOffset)))
}

func ring0ErrorPhaseName(phase uint32) string {
	switch {
	case phase >= 0x100 && phase <= 0x11c:
		return fmt.Sprintf("loading X%d", phase-0x100)
	case phase == 0x11d:
		return "loading X29"
	case phase == 0x11e:
		return "loading X30"
	case phase == 0x120:
		return "loading SP_EL1"
	case phase == 0x121:
		return "loading PC"
	case phase == 0x122:
		return "loading CPSR"
	case phase >= 0x200 && phase <= 0x21f:
		return fmt.Sprintf("loading Q%d", phase-0x200)
	case phase == 0x220:
		return "loading FPSR"
	case phase == 0x221:
		return "loading FPCR"
	case phase == 0x300:
		return "loading TPIDR_EL0"
	case phase == 0x301:
		return "loading TPIDRRO_EL0"
	case phase == 0x302:
		return "loading TTBR0_EL1"
	case phase == 0x400:
		return "running vCPU"
	case phase == 0x401:
		return "reading canceled-vCPU PSTATE"
	case phase == 0x402:
		return "reading canceled-vCPU PC"
	case phase == 0x403:
		return "injecting cancellation IRQ"
	case phase == 0x500:
		return "saving host continuation"
	case phase == 0x600:
		return "handling unexpected vCPU exit"
	default:
		return fmt.Sprintf("unknown phase %#x", phase)
	}
}

func (b *ring0Bridge) error(prefix string) error {
	status := b.errorStatus()
	if status == 0 {
		return nil
	}
	phase := b.errorPhase()
	if phase == C.TTC_RING0_PHASE_UNEXPECTED_EXIT {
		return fmt.Errorf("%s: status %#x while %s: host_gate_esr=%#x host_gate_far=%#x host_gate_pc=%#x internal_esr=%#x internal_elr=%#x internal_vector=%#x",
			prefix, status, ring0ErrorPhaseName(phase),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_HOST_GATE_ESR_OFFSET)),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_HOST_GATE_FAR_OFFSET)),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_HOST_GATE_PC_OFFSET)),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_INTERNAL_ESR_OFFSET)),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_INTERNAL_ELR_OFFSET)),
			uint64(C.ttc_ring0_state_u64(b.pointer, C.TTC_STATE_INTERNAL_VECTOR_OFFSET)))
	}
	return fmt.Errorf("%s: status %#x while %s", prefix, status, ring0ErrorPhaseName(phase))
}

func (b *ring0Bridge) probeOwner() error {
	status := uint32(C.ttc_ring0_bridge_probe_owner(b.pointer))
	if status == 0 {
		return nil
	}
	return fmt.Errorf("HVF ring0 vCPU owner probe failed: status %#x", status)
}

func (b *ring0Bridge) preflight() error {
	status := uint32(C.ttc_ring0_bridge_preflight(b.pointer))
	if status == 0 {
		rawStatus := ring0asm.RawCall(unsafe.Pointer(b.pointer),
			uintptr(C.ttc_ring0_bridge_host_stack_top(b.pointer)),
			uintptr(C.ttc_ring0_bridge_probe_reg_address()))
		if rawStatus == 0 {
			return nil
		}
		return fmt.Errorf("HVF ring0 raw-call preflight failed: status %#x", rawStatus)
	}
	return fmt.Errorf("HVF ring0 vCPU preflight failed: status %#x owner_tid=%d current_tid=%d",
		status,
		uint64(C.ttc_ring0_bridge_owner_tid(b.pointer)),
		uint64(C.ttc_ring0_bridge_preflight_tid(b.pointer)))
}

func (b *ring0Bridge) hostStackTop() uintptr {
	return uintptr(C.ttc_ring0_bridge_host_stack_top(b.pointer))
}

func (b *ring0Bridge) bootstrap() uintptr {
	return uintptr(C.ttc_ring0_bridge_bootstrap(b.pointer))
}

func ring0NativeEntryAddress() uintptr {
	return uintptr(C.ttc_ring0_native_entry_address())
}

func (b *ring0Bridge) lastX0() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeLastX0Offset)))
}

func (b *ring0Bridge) lastHostX0() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeLastHostX0Offset)))
}

func (b *ring0Bridge) lastCancelPC() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeLastCancelPCOffset)))
}

func (b *ring0Bridge) lastCancelPState() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeLastCancelPStateOffset)))
}

func (b *ring0Bridge) cancelCount() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeCancelCountOffset)))
}

func (b *ring0Bridge) probe(stateVA uint64) (uint64, error) {
	got := ring0asm.Probe(unsafe.Pointer(b.pointer), stateVA)
	if err := b.error("HVF ring0 signal bridge failed"); err != nil {
		return 0, err
	}
	return got, nil
}

func (b *ring0Bridge) applicationProbe(stateVA, monitorEntry uint64) (uint64, error) {
	got := ring0asm.ApplicationProbe(unsafe.Pointer(b.pointer), stateVA, monitorEntry)
	if err := b.error("HVF ring0 application bridge failed"); err != nil {
		return 0, err
	}
	return got, nil
}

func (v *hvfVM) mapRing0Stage2(hostAddress uintptr, length uint64, access vmm.Stage2Access) error {
	return v.mapRing0Stage2Source(hostAddress, hostAddress, length, access)
}

func (v *hvfVM) mapRing0Stage2Source(gpa, hostAddress uintptr, length uint64, access vmm.Stage2Access) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	return v.mapStage2Locked(uint64(gpa), hostAddress, length, access)
}

func (v *hvfVM) unmapRing0Stage2(hostAddress uintptr, length uint64) error {
	return v.unmapRing0Stage2GPA(uint64(hostAddress), length)
}

func (v *hvfVM) unmapRing0Stage2GPA(gpa uint64, length uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	return v.unmapStage2Locked(gpa, length)
}

func (v *hvfVM) monitorAddressSpaceTTBR(id uint64) uint64 {
	return uint64(C.ttc_monitor_address_space_ttbr(unsafe.Pointer(v.monitor.controlHost), C.uint64_t(id)))
}

// ValidateRing0Continuation performs a real host-EL0 to guest-EL1 and back
// continuation transfer. It is intentionally small enough to use during
// engine qualification: failure means the host cannot safely enable the
// Sentry-at-EL1 execution policy and must not silently fall back after startup.
func ValidateRing0Continuation(vmInterface vmm.VM) error {
	vm, ok := vmInterface.(*hvfVM)
	if !ok {
		return fmt.Errorf("ring0 continuation validation requires an HVF VM, got %T", vmInterface)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	vcpuInterface, err := vm.CreateVCPU(0)
	if err != nil {
		return err
	}
	vcpu := vcpuInterface.(*hvfVCPU)
	defer vcpu.Close()

	runMonitor := func(sequence uint64, command monitorabi.Command) error {
		command.Sequence = sequence
		encoded, err := monitorabi.MarshalCommand(command)
		if err != nil {
			return err
		}
		responseBytes, err := vcpu.RunMonitor(context.Background(), encoded)
		if err != nil {
			return err
		}
		response, err := monitorabi.UnmarshalResponse(responseBytes)
		if err != nil {
			return err
		}
		if response.Sequence != sequence || response.Status != monitorabi.StatusOK {
			return fmt.Errorf("monitor response = %+v", response)
		}
		return nil
	}

	const sentryAddressSpaceID = uint64(1)
	if err := runMonitor(1, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: sentryAddressSpaceID,
		Flags:          monitorabi.AddressSpaceFlagSentry,
	}); err != nil {
		return err
	}

	slabs := make(map[uintptr]vmm.Stage2Access)
	addSlab := func(address uintptr, access vmm.Stage2Access) {
		base := address &^ uintptr(vmm.HostSlabSize-1)
		slabs[base] |= access
	}
	addSlab(ring0asm.BluepillTrapAddress(), vmm.Stage2Read|vmm.Stage2Execute)
	addSlab(ring0asm.ProbeAddress(), vmm.Stage2Read|vmm.Stage2Execute)
	addSlab(ring0asm.ApplicationProbeAddress(), vmm.Stage2Read|vmm.Stage2Execute)
	addSlab(ring0asm.CurrentSP(), vmm.Stage2Read|vmm.Stage2Write)

	mapped := make(map[uintptr]uint64, len(slabs))
	type shadowMapping struct {
		host   uintptr
		length uint64
	}
	var shadows []shadowMapping
	defer func() {
		for _, gpa := range mapped {
			_ = vm.unmapRing0Stage2GPA(gpa, vmm.HostSlabSize)
		}
		for _, shadow := range shadows {
			_ = (hostMemory{}).Release(shadow.host, shadow.length)
		}
	}()
	nextGPA := uint64(ring0HostMirrorGPAStart)
	for address, access := range slabs {
		gpa := nextGPA
		nextGPA += vmm.HostSlabSize
		hostAddress := address
		if access&vmm.Stage2Write == 0 {
			hostAddress, err = (hostMemory{}).Reserve(vmm.HostSlabSize)
			if err != nil {
				return err
			}
			if err := (hostMemory{}).Copy(hostAddress, address, vmm.HostSlabSize); err != nil {
				_ = (hostMemory{}).Release(hostAddress, vmm.HostSlabSize)
				return err
			}
			shadows = append(shadows, shadowMapping{host: hostAddress, length: vmm.HostSlabSize})
		}
		if err := vm.mapRing0Stage2Source(uintptr(gpa), hostAddress, vmm.HostSlabSize, access); err != nil {
			return fmt.Errorf("mapping ring0 stage-2 slab %#x: %w", address, err)
		}
		mapped[address] = gpa
	}

	sequence := uint64(2)
	for address, stage2Access := range slabs {
		access := monitorabi.AccessRead
		if stage2Access&vmm.Stage2Write != 0 {
			access |= monitorabi.AccessWrite
		}
		if stage2Access&vmm.Stage2Execute != 0 {
			access |= monitorabi.AccessExecute
		}
		if err := runMonitor(sequence, monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: sentryAddressSpaceID,
			Operations: []monitorabi.Operation{{
				Type:   monitorabi.OperationMap,
				Access: access,
				VA:     uint64(address),
				GPA:    mapped[address],
				Length: vmm.HostSlabSize,
			}},
		}); err != nil {
			return err
		}
		sequence++
	}

	ttbr0 := vm.monitorAddressSpaceTTBR(sentryAddressSpaceID)
	if ttbr0 == 0 {
		return fmt.Errorf("monitor did not publish the Sentry TTBR0")
	}
	if err := installRing0SignalBridge(); err != nil {
		return err
	}
	bridge, err := vcpu.newRing0Bridge(ttbr0, false)
	if err != nil {
		return err
	}
	defer bridge.close()
	got, err := bridge.probe(vcpu.stateVA)
	if err != nil {
		return err
	}
	if got != 1 {
		return fmt.Errorf("continuation observed EL%d, want EL1 (vCPU x0=%#x host ucontext x0=%#x at redpill)", got, bridge.lastX0(), bridge.lastHostX0())
	}
	const (
		applicationAddressSpaceID = uint64(2)
		applicationVA             = uint64(0x400000)
	)
	if err := runMonitor(sequence, monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: applicationAddressSpaceID,
	}); err != nil {
		return err
	}
	sequence++
	applicationHost, err := (hostMemory{}).Reserve(vmm.HostSlabSize)
	if err != nil {
		return err
	}
	defer (hostMemory{}).Release(applicationHost, vmm.HostSlabSize)
	if err := vm.MapStage2(GuestRAMIPAStart, applicationHost, vmm.HostSlabSize,
		vmm.Stage2Read|vmm.Stage2Write|vmm.Stage2Execute); err != nil {
		return err
	}
	defer vm.UnmapStage2(GuestRAMIPAStart, vmm.HostSlabSize)
	applicationBytes := unsafe.Slice((*byte)(unsafe.Pointer(applicationHost)), int(vmm.HostSlabSize))
	binary.LittleEndian.PutUint32(applicationBytes[0:], 0xd2801588) // movz x8, #172 (getpid)
	binary.LittleEndian.PutUint32(applicationBytes[4:], 0xd4000001) // svc #0
	if err := runMonitor(sequence, monitorabi.Command{
		Opcode:         monitorabi.OpcodeApply,
		AddressSpaceID: applicationAddressSpaceID,
		Operations: []monitorabi.Operation{{
			Type:   monitorabi.OperationMap,
			Access: monitorabi.AccessRead | monitorabi.AccessExecute,
			VA:     applicationVA,
			GPA:    GuestRAMIPAStart,
			Length: vmm.LogicalPageSize,
		}},
	}); err != nil {
		return err
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(vcpu.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint64(state[stateAddressSpaceID:], applicationAddressSpaceID)
	binary.LittleEndian.PutUint64(state[statePC:], applicationVA)
	binary.LittleEndian.PutUint64(state[statePState:], 0)
	binary.LittleEndian.PutUint64(state[stateExitKind:], 0)
	watchdogResult := make(chan string, 1)
	watchdog := time.AfterFunc(time.Second, func() {
		diagnostic := fmt.Sprintf("exit=%d app_esr=%#x app_far=%#x sentry_pc=%#x sentry_sp=%#x internal_esr=%#x internal_far=%#x internal_elr=%#x internal_vector=%d",
			binary.LittleEndian.Uint64(state[stateExitKind:]),
			binary.LittleEndian.Uint64(state[stateESR:]),
			binary.LittleEndian.Uint64(state[stateFAR:]),
			binary.LittleEndian.Uint64(state[stateSentryPC:]),
			binary.LittleEndian.Uint64(state[stateSentrySP:]),
			binary.LittleEndian.Uint64(state[stateInternalESR:]),
			binary.LittleEndian.Uint64(state[stateInternalFAR:]),
			binary.LittleEndian.Uint64(state[stateInternalELR:]),
			binary.LittleEndian.Uint64(state[stateInternalVector:]))
		requestErr := vcpu.RequestExit()
		diagnostic += fmt.Sprintf(" cancellation={err=%v count=%d pc=%#x pstate=%#x}",
			requestErr, bridge.cancelCount(), bridge.lastCancelPC(), bridge.lastCancelPState())
		watchdogResult <- diagnostic
	})
	got, err = bridge.applicationProbe(vcpu.stateVA,
		monitorCodeVA+vcpu.monitor.ring0ApplicationOffset)
	if !watchdog.Stop() {
		return fmt.Errorf("ring0 application probe timed out: %s", <-watchdogResult)
	}
	if err != nil {
		return err
	}
	if got != 1 {
		return fmt.Errorf("application return observed EL%d, want EL1", got)
	}
	if exitKind := binary.LittleEndian.Uint64(state[stateExitKind:]); exitKind != 1 {
		return fmt.Errorf("ring0 application exit kind %d, want syscall", exitKind)
	}
	if syscallNumber := binary.LittleEndian.Uint64(state[stateX0+8*8:]); syscallNumber != 172 {
		return fmt.Errorf("ring0 application syscall number %d, want 172", syscallNumber)
	}
	if pc := binary.LittleEndian.Uint64(state[statePC:]); pc != applicationVA+8 {
		return fmt.Errorf("ring0 application PC %#x, want %#x", pc, applicationVA+8)
	}
	return nil
}
