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
#include <mach/vm_statistics.h>
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#include <mach/mach_time.h>
#include <libkern/OSCacheControl.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stddef.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/ucontext.h>
#include <time.h>
#include <unistd.h>

#include "pkg/sentry/platform/hvf/monitor/monitor_layout.h"

#define TTC_RING0_BRIDGE_MAGIC UINT64_C(0x3145474449524354)
#define TTC_RING0_HOST_STACK_SIZE (UINT64_C(64) << 10)

struct ttc_ring0_region {
	uint64_t address;
	uint64_t length;
	uint64_t object_offset;
	uint64_t object_id;
	uint32_t protection;
	uint32_t user_tag;
	uint32_t external_pager;
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
				.external_pager = info.external_pager,
			};
		}
		address += length;
	}
	*regions_out = regions;
	*count_out = count;
	return 0;
}

// ttc_ring0_region_at performs the demand-path counterpart of the full
// bootstrap snapshot. mach_vm_region_recurse may return the next region after
// target, so require that the result actually contains target.
static int ttc_ring0_region_at(uint64_t target,
		struct ttc_ring0_region *region_out) {
	mach_vm_address_t address = (mach_vm_address_t)target;
	natural_t depth = 0;
	for (;;) {
		mach_vm_size_t length = 0;
		vm_region_submap_info_data_64_t info;
		mach_msg_type_number_t info_count = VM_REGION_SUBMAP_INFO_COUNT_64;
		kern_return_t kr = mach_vm_region_recurse(mach_task_self(),
				&address, &length, &depth,
				(vm_region_recurse_info_t)&info, &info_count);
		if (kr != KERN_SUCCESS) return kr;
		if (info.is_submap) {
			++depth;
			continue;
		}
		if (length == 0 || address + length < address) return EOVERFLOW;
		if (target < address || target - address >= length ||
				info.protection == VM_PROT_NONE) {
			return KERN_INVALID_ADDRESS;
		}
		*region_out = (struct ttc_ring0_region){
			.address = address,
			.length = length,
			.object_offset = info.offset,
			.object_id = info.object_id_full != 0 ?
				info.object_id_full : info.object_id,
			.protection = info.protection,
			.user_tag = info.user_tag,
			.external_pager = info.external_pager,
		};
		return KERN_SUCCESS;
	}
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

struct ttc_ring0_time_fastpath {
	uint64_t clock_gettime_pc;
	uint64_t mach_absolute_time_pc;
	uint64_t numer;
	uint64_t denom;
	int64_t realtime_offset_ns;
};

#define TTC_RING0_MAX_APPLICATION_VCPUS 63u

struct ttc_ring0_native_domain_slot {
	_Atomic uint32_t allocated;
	_Atomic uint32_t active;
	_Atomic uint32_t critical;
	_Atomic uint64_t tpidrro_el0;
};

// Darwin's C thread-local ABI calls __tlv_get_addr, which is itself a host
// library service and therefore cannot be used by a guest-resident callback.
// Use a fixed process-data table keyed by TPIDRRO_EL0 instead. TPIDRRO_EL0 is
// readable at both host EL0 and guest EL1, and each slot outlives its bridge's
// final guest exit, so the callback requires neither a lock nor a live pointer.
static struct ttc_ring0_native_domain_slot
		ttc_ring0_native_domains[TTC_RING0_MAX_APPLICATION_VCPUS];

struct ttc_runtime_time_query {
	uint32_t version;
	uint32_t clock;
	int64_t seconds;
	int32_t nanoseconds;
	uint32_t reserved;
};

_Static_assert(sizeof(struct ttc_runtime_time_query) == 24,
		"Darwin runtime time hook ABI changed");

static uintptr_t ttc_ring0_strip_instruction_pointer(uintptr_t address);

static uint64_t ttc_ring0_host_cntvct(void) {
	uint64_t value;
	__asm__ volatile("isb\n\tmrs %0, cntvct_el0" : "=r"(value));
	return value;
}

static uint64_t ttc_ring0_scale_ticks(uint64_t ticks, uint64_t numer,
		uint64_t denom) {
	return (ticks / denom) * numer + ((ticks % denom) * numer) / denom;
}

// Darwin traps CurrentEL at host EL0. The SIGILL bridge recognizes the first
// instruction of this naked helper and supplies zero there; at guest EL1 the
// instruction executes normally and returns one. The native-domain table
// remains the fast reject so ordinary host clocks do not pay for this probe.
static uint64_t ttc_ring0_runtime_current_el(void)
		__attribute__((naked, noinline));

static uint64_t ttc_ring0_runtime_current_el(void) {
	__asm__ volatile(
		"mrs x0, CurrentEL\n"
		"lsr x0, x0, #2\n"
		"ret\n");
}

// ttc_ring0_runtime_time_hook runs before Darwin's Go runtime enters
// libcCall. On native threads outside hv_vcpu_run it declines so the stock
// runtime path remains authoritative. During guest execution, TPIDR_EL1 names
// the permanent vCPU state page containing the VM-wide clock calibration.
static int32_t ttc_ring0_runtime_time_hook(
		struct ttc_runtime_time_query *query) {
	uint64_t thread_pointer;
	__asm__ volatile("mrs %0, tpidrro_el0" : "=r"(thread_pointer));
	struct ttc_ring0_native_domain_slot *domain = NULL;
	for (size_t i = 0; i < TTC_RING0_MAX_APPLICATION_VCPUS; ++i) {
		struct ttc_ring0_native_domain_slot *slot =
				&ttc_ring0_native_domains[i];
		if (atomic_load_explicit(&slot->active, memory_order_acquire) != 0 &&
				atomic_load_explicit(&slot->tpidrro_el0,
				memory_order_relaxed) == thread_pointer) {
			domain = slot;
			break;
		}
	}
	if (domain == NULL) return 0;
	// Cancellation can export an arbitrary EL1 continuation to Darwin. Keep the
	// guest-only CurrentEL/TPIDR_EL1 sequence indivisible with respect to that
	// domain transfer. The owner observes this counter after hv_vcpu_run returns
	// and resumes the stopped vCPU until the callback leaves the critical range.
	atomic_fetch_add_explicit(&domain->critical, 1, memory_order_seq_cst);
	if (ttc_ring0_runtime_current_el() == 0) {
		atomic_fetch_sub_explicit(&domain->critical, 1, memory_order_release);
		return 0;
	}
	if (query == NULL || query->version != 1 ||
			(query->clock != 1 && query->clock != 2)) {
		atomic_fetch_sub_explicit(&domain->critical, 1, memory_order_release);
		return -1;
	}
	const uint8_t *state;
	__asm__ volatile("mrs %0, tpidr_el1" : "=r"(state));
	uint64_t numer;
	uint64_t denom;
	uint64_t vtimer_offset;
	int64_t realtime_offset;
	__builtin_memcpy(&numer, state + TTC_STATE_TIMEBASE_NUMER_OFFSET,
			sizeof(numer));
	__builtin_memcpy(&denom, state + TTC_STATE_TIMEBASE_DENOM_OFFSET,
			sizeof(denom));
	__builtin_memcpy(&vtimer_offset,
			state + TTC_STATE_VTIMER_OFFSET_OFFSET, sizeof(vtimer_offset));
	__builtin_memcpy(&realtime_offset,
			state + TTC_STATE_REALTIME_OFFSET_NS_OFFSET,
			sizeof(realtime_offset));
	if (numer == 0 || denom == 0) {
		atomic_fetch_sub_explicit(&domain->critical, 1, memory_order_release);
		return -1;
	}
	uint64_t ticks = ttc_ring0_host_cntvct();
	uint64_t ns;
	if (query->clock == 1) {
		ns = ttc_ring0_scale_ticks(ticks, numer, denom) +
				(uint64_t)realtime_offset;
	} else {
		ns = ttc_ring0_scale_ticks(ticks + vtimer_offset, numer, denom);
	}
	query->seconds = (int64_t)(ns / UINT64_C(1000000000));
	query->nanoseconds = (int32_t)(ns % UINT64_C(1000000000));
	atomic_fetch_sub_explicit(&domain->critical, 1, memory_order_release);
	return 1;
}

static uintptr_t ttc_ring0_runtime_time_hook_address(void) {
	return ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_runtime_time_hook);
}

// ttc_ring0_calibrate_time_fastpath binds the process-local Darwin helper PCs
// and relates host CNTVCT_EL0 to CLOCK_REALTIME. The midpoint of the
// bracketing counter reads bounds calibration error by the duration of one
// host clock_gettime call. HVF applies the VM-wide VTimer offset separately so
// guest CNTVCT remains in this same architectural-counter domain.
static int ttc_ring0_calibrate_time_fastpath(
		struct ttc_ring0_time_fastpath *result) {
	mach_timebase_info_data_t timebase;
	if (mach_timebase_info(&timebase) != KERN_SUCCESS ||
			timebase.numer == 0 || timebase.denom == 0) {
		return EINVAL;
	}
	uint64_t before = ttc_ring0_host_cntvct();
	struct timespec realtime;
	if (clock_gettime(CLOCK_REALTIME, &realtime) != 0) return errno;
	uint64_t after = ttc_ring0_host_cntvct();
	uint64_t midpoint = before + (after - before) / 2;
	__uint128_t counter_ns = (__uint128_t)midpoint * timebase.numer /
		timebase.denom;
	__int128 realtime_ns = (__int128)realtime.tv_sec * UINT64_C(1000000000) +
		realtime.tv_nsec;
	__int128 offset = realtime_ns - counter_ns;
	if (counter_ns > INT64_MAX || offset < INT64_MIN || offset > INT64_MAX) {
		return EOVERFLOW;
	}
	*result = (struct ttc_ring0_time_fastpath){
		.clock_gettime_pc = ttc_ring0_strip_instruction_pointer(
				(uintptr_t)&clock_gettime),
		.mach_absolute_time_pc = ttc_ring0_strip_instruction_pointer(
				(uintptr_t)&mach_absolute_time),
		.numer = timebase.numer,
		.denom = timebase.denom,
		.realtime_offset_ns = (int64_t)offset,
	};
	return 0;
}

struct ttc_ring0_bridge {
	uint64_t magic;
	hv_vcpu_t vcpu;
	const hv_vcpu_exit_t *exit;
	uint64_t ttbr0;
	uint64_t ttbr1;
	uint64_t vector_va;
	uint64_t state_va;
	uintptr_t state_host;
	uintptr_t guest_switch_pc;
	uint32_t destroy_on_host;
	uint32_t native_domain_slot;
	_Atomic uint32_t destroy_requested;
	_Atomic uint32_t external_interrupt_requested;
	_Atomic uint32_t error;
	_Atomic uint32_t error_phase;
	_Atomic uint32_t in_guest;
	_Atomic uint32_t destroyed;
	_Atomic uint64_t last_x0;
	_Atomic uint64_t last_host_x0;
	_Atomic uint64_t last_cancel_pc;
	_Atomic uint64_t last_cancel_pstate;
	_Atomic uint64_t cancel_count;
	// These counters make the execution-domain boundary observable without
	// instrumenting the guest-resident Go path. In particular, an application
	// syscall must not increase any host-return counter: the monitor restores
	// the waiting Sentry continuation directly at EL1.
	_Atomic uint64_t guest_entry_count;
	_Atomic uint64_t host_gate_count;
	_Atomic uint64_t redpill_count;
	_Atomic uint64_t host_return_count;
	_Atomic uint64_t cache_sync_count;
	uint64_t owner_tid;
	_Atomic uint64_t entry_tid;
	ucontext_t host_context;
	struct __darwin_mcontext64 host_mcontext;
	void *host_stack;
	size_t host_stack_size;
	struct ttc_ring0_bridge *registry_next;
};

#define TTC_RING0_HOST_SLAB_SIZE UINT64_C(16384)
#define TTC_RUNTIME_VM_MUTATION_VERSION 1u
#define TTC_RUNTIME_VM_MMAP 1u
#define TTC_RUNTIME_VM_MUNMAP 2u
#define TTC_RUNTIME_VM_MADVISE 3u
#define TTC_RUNTIME_VM_STACK_SUSPEND 4u
#define TTC_RUNTIME_VM_STACK_RESUME 5u
#define TTC_RUNTIME_VM_BEFORE 1u
#define TTC_RUNTIME_VM_AFTER 2u
#define TTC_RUNTIME_MADV_FREE_REUSABLE 7
#define TTC_RUNTIME_MADV_FREE_REUSE 8

// Keep this layout synchronized with runtime.ttcVMMemoryMutation in the
// patched Darwin Go runtime. It deliberately contains no Go pointers.
struct ttc_runtime_vm_mutation {
	uint32_t version;
	uint16_t operation;
	uint16_t phase;
	uintptr_t address;
	uintptr_t length;
	uintptr_t result;
	int32_t protection;
	int32_t flags;
	int32_t fd;
	int32_t error;
	uint32_t offset;
	uint32_t active;
};

_Static_assert(sizeof(struct ttc_runtime_vm_mutation) == 56,
		"Darwin runtime VM mutation ABI changed");

// One record represents one coalesced hv_vm_map lease. registered records the
// GPAs still owned by the Go mirror metadata; live records which of those
// GPAs currently have a stage-2 mapping. A runtime munmap may make a slab
// dormant until a later mmap reuses the same HVA.
struct ttc_ring0_mirror_mapping {
	uintptr_t host;
	uint64_t gpa;
	uint64_t length;
	hv_memory_flags_t access;
	size_t slabs;
	size_t words;
	struct ttc_ring0_mirror_mapping *next;
	uint64_t bits[];
};

static pthread_mutex_t ttc_ring0_mutation_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_mutex_t ttc_ring0_bridge_registry_mu = PTHREAD_MUTEX_INITIALIZER;
// Number of overlapping secondary-MMU transactions that require every EL0
// application and EL1 Sentry continuation to remain stopped. Transactions are
// serialized while they change the stage-2 registry, but an engine-driven
// mirror reconciliation releases mutation_mu between individual HVF calls.
// A count, rather than a boolean, prevents a nested Go-runtime mutation from
// reopening admission before the outer reconciliation is complete.
static _Atomic uint32_t ttc_ring0_guest_pause_count;
static struct ttc_ring0_bridge *ttc_ring0_bridges;
// Bridge records are visible to the shared EL1 Sentry and therefore may gain
// a long-lived stage-2 lease. Recycle their anonymous 16K mappings in place
// instead of unmapping them: Darwin may otherwise reuse the same HVA with a
// different backing object while the engine's mirror registry still owns the
// old HVF lease. The pool is bounded by peak simultaneous vCPU ownership.
static struct ttc_ring0_bridge *ttc_ring0_free_bridges;
static struct ttc_ring0_mirror_mapping *ttc_ring0_mirror_mappings;
static pthread_once_t ttc_ring0_preemption_once = PTHREAD_ONCE_INIT;
static _Atomic int32_t ttc_ring0_preemption_status;

// A resident application keeps its Go G and P active because its exception
// resumes ordinary Sentry Go code directly at EL1. A native tick is required
// to bound the interval in which CPU-bound EL0 execution cannot reach a Go
// safepoint. It intentionally runs without a Go P, just like the host kernel's
// preemption facility used by KVM.
#define TTC_RING0_PREEMPTION_QUANTUM_NS UINT64_C(10000000)

static void *ttc_ring0_preemption_thread(
		__attribute__((unused)) void *argument) {
	const struct timespec quantum = {
		.tv_sec = 0,
		.tv_nsec = TTC_RING0_PREEMPTION_QUANTUM_NS,
	};
	for (;;) {
		struct timespec remaining = quantum;
		while (nanosleep(&remaining, &remaining) != 0 && errno == EINTR) {}
		int status = pthread_mutex_lock(&ttc_ring0_bridge_registry_mu);
		if (status != 0) {
			atomic_store_explicit(&ttc_ring0_preemption_status, status,
					memory_order_release);
			return NULL;
		}
		hv_vcpu_t active[TTC_RING0_MAX_APPLICATION_VCPUS];
		size_t count = 0;
		for (struct ttc_ring0_bridge *bridge = ttc_ring0_bridges;
				bridge != NULL; bridge = bridge->registry_next) {
			if (atomic_load_explicit(&bridge->in_guest,
					memory_order_acquire) == 0 ||
					atomic_load_explicit(&bridge->destroyed,
					memory_order_acquire) != 0) {
				continue;
			}
			_Atomic uint64_t *exit_kind = (_Atomic uint64_t *)(
					bridge->state_host + TTC_STATE_EXIT_KIND_OFFSET);
			if (atomic_load_explicit(exit_kind, memory_order_acquire) != 0) {
				// The application has already returned to Sentry Go code, which
				// reaches ordinary runtime safepoints without a native kick.
				continue;
			}
			if (count == TTC_RING0_MAX_APPLICATION_VCPUS) break;
			active[count++] = bridge->vcpu;
		}
		if (count != 0) {
			// Do not set external_interrupt_requested. The owner bridge turns an
			// EL0 cancellation into TTC_EXIT_RETRY, while an EL1 cancellation
			// exports the exact Sentry continuation so Darwin's Go scheduler can
			// honor pending preemption and GC work.
			(void)hv_vcpus_exit(active, (uint32_t)count);
		}
		pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	}
}

static void ttc_ring0_start_preemption_thread_once(void) {
	pthread_t thread;
	int status = pthread_create(&thread, NULL,
			ttc_ring0_preemption_thread, NULL);
	if (status == 0) status = pthread_detach(thread);
	atomic_store_explicit(&ttc_ring0_preemption_status, status,
			memory_order_release);
}

static int ttc_ring0_start_preemption_thread(void) {
	int status = pthread_once(&ttc_ring0_preemption_once,
			ttc_ring0_start_preemption_thread_once);
	if (status != 0) return status;
	return atomic_load_explicit(&ttc_ring0_preemption_status,
			memory_order_acquire);
}

static struct ttc_ring0_bridge *ttc_ring0_bridge_storage_acquire(void) {
	struct ttc_ring0_bridge *bridge = NULL;
	if (pthread_mutex_lock(&ttc_ring0_bridge_registry_mu) != 0) return NULL;
	if (ttc_ring0_free_bridges != NULL) {
		bridge = ttc_ring0_free_bridges;
		ttc_ring0_free_bridges = bridge->registry_next;
	}
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	if (bridge == NULL) {
		bridge = mmap(NULL, TTC_RING0_HOST_SLAB_SIZE,
				PROT_READ | PROT_WRITE,
				MAP_PRIVATE | MAP_ANON,
				VM_MAKE_TAG(VM_MEMORY_APPLICATION_SPECIFIC_2), 0);
		if (bridge == MAP_FAILED) return NULL;
	}
	__builtin_memset(bridge, 0, TTC_RING0_HOST_SLAB_SIZE);
	return bridge;
}

static void ttc_ring0_bridge_storage_release(
		struct ttc_ring0_bridge *bridge) {
	if (bridge == NULL) return;
	bridge->magic = 0;
	// If the mutex itself is unusable, deliberately retain the mapping rather
	// than munmapping memory that may still have a registered HVF lease.
	if (pthread_mutex_lock(&ttc_ring0_bridge_registry_mu) != 0) return;
	bridge->registry_next = ttc_ring0_free_bridges;
	ttc_ring0_free_bridges = bridge;
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
}

static uint64_t *ttc_ring0_mapping_registered(
		struct ttc_ring0_mirror_mapping *mapping) {
	return &mapping->bits[0];
}

static uint64_t *ttc_ring0_mapping_live(
		struct ttc_ring0_mirror_mapping *mapping) {
	return &mapping->bits[mapping->words];
}

static bool ttc_ring0_bit_test(const uint64_t *bits, size_t index) {
	return (bits[index / 64] & (UINT64_C(1) << (index % 64))) != 0;
}

static void ttc_ring0_bit_set(uint64_t *bits, size_t index) {
	bits[index / 64] |= UINT64_C(1) << (index % 64);
}

static void ttc_ring0_bit_clear(uint64_t *bits, size_t index) {
	bits[index / 64] &= ~(UINT64_C(1) << (index % 64));
}

static bool ttc_ring0_mapping_empty(
		struct ttc_ring0_mirror_mapping *mapping) {
	uint64_t *registered = ttc_ring0_mapping_registered(mapping);
	for (size_t word = 0; word < mapping->words; ++word) {
		if (registered[word] != 0) return false;
	}
	return true;
}

static bool ttc_ring0_ranges_overlap(uintptr_t first, uint64_t first_length,
		uintptr_t second, uintptr_t second_length) {
	if (first_length == 0 || second_length == 0) return false;
	uintptr_t first_end = first + (uintptr_t)first_length;
	uintptr_t second_end = second + second_length;
	if (first_end < first || second_end < second) return true;
	return first < second_end && second < first_end;
}

static bool ttc_ring0_mapping_slab_overlaps(
		const struct ttc_ring0_mirror_mapping *mapping, size_t slab,
		uintptr_t address, uintptr_t length) {
	return ttc_ring0_ranges_overlap(
		mapping->host + slab * TTC_RING0_HOST_SLAB_SIZE,
		TTC_RING0_HOST_SLAB_SIZE, address, length);
}

// Guest admission is a lock-free read side. The second blocked sample closes
// the race with a writer that publishes a guest pause immediately after the
// first sample. The writer cannot finish draining while in_guest remains set.
static void ttc_ring0_admit_guest(struct ttc_ring0_bridge *bridge) {
	for (;;) {
		while (atomic_load_explicit(&ttc_ring0_guest_pause_count,
				memory_order_acquire) != 0) {
			sched_yield();
		}
		atomic_store_explicit(&bridge->in_guest, 1,
				memory_order_seq_cst);
		if (atomic_load_explicit(&ttc_ring0_guest_pause_count,
				memory_order_seq_cst) == 0) {
			return;
		}
		atomic_store_explicit(&bridge->in_guest, 0,
				memory_order_release);
	}
}

// The caller holds mutation_mu, which also stabilizes the mirror registry.
static int ttc_ring0_begin_runtime_mutation_locked(void) {
	int status;
	uint32_t previous = atomic_fetch_add_explicit(
			&ttc_ring0_guest_pause_count, 1, memory_order_seq_cst);
	if (previous == UINT32_MAX) {
		atomic_fetch_sub_explicit(&ttc_ring0_guest_pause_count, 1,
				memory_order_release);
		return EOVERFLOW;
	}
	// mutation_mu serializes pause establishment. A nonzero predecessor has
	// already drained every registered bridge before releasing that mutex.
	if (previous != 0) return 0;

	status = pthread_mutex_lock(&ttc_ring0_bridge_registry_mu);
	if (status != 0) goto fail;
	struct timespec start;
	(void)clock_gettime(CLOCK_MONOTONIC, &start);
	for (;;) {
		hv_vcpu_t active[TTC_RING0_MAX_APPLICATION_VCPUS];
		size_t count = 0;
		for (struct ttc_ring0_bridge *bridge = ttc_ring0_bridges;
				bridge != NULL; bridge = bridge->registry_next) {
			if (atomic_load_explicit(&bridge->in_guest,
					memory_order_acquire) != 0 &&
					atomic_load_explicit(&bridge->destroyed,
					memory_order_acquire) == 0) {
				if (count == TTC_RING0_MAX_APPLICATION_VCPUS) {
					status = EOVERFLOW;
					goto fail_registry;
				}
				active[count++] = bridge->vcpu;
			}
		}
		if (count == 0) break;
		hv_return_t hv_status = hv_vcpus_exit(active, (uint32_t)count);
		if (hv_status != HV_SUCCESS) {
			status = EIO;
			goto fail_registry;
		}
		struct timespec now;
		(void)clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec - start.tv_sec >= 5) {
			status = ETIMEDOUT;
			goto fail_registry;
		}
		sched_yield();
	}
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	return 0;

fail_registry:
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
fail:
	atomic_fetch_sub_explicit(&ttc_ring0_guest_pause_count, 1,
			memory_order_release);
	return status;
}

static void ttc_ring0_end_runtime_mutation(void) {
	atomic_fetch_sub_explicit(&ttc_ring0_guest_pause_count, 1,
			memory_order_release);
	pthread_mutex_unlock(&ttc_ring0_mutation_mu);
}

// Establish an exact-context guest pause while releasing mutation_mu so the
// caller can perform one native operation through the ordinary serialized HVF
// APIs. Nesting with a concurrent runtime VM callback is reference-counted.
static int32_t ttc_ring0_pause_guest_execution(void) {
	int status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
	if (status != 0) return status;
	status = ttc_ring0_begin_runtime_mutation_locked();
	pthread_mutex_unlock(&ttc_ring0_mutation_mu);
	return status;
}

static void ttc_ring0_resume_guest_execution(void) {
	pthread_mutex_lock(&ttc_ring0_mutation_mu);
	atomic_fetch_sub_explicit(&ttc_ring0_guest_pause_count, 1,
			memory_order_release);
	pthread_mutex_unlock(&ttc_ring0_mutation_mu);
}

// Publish a local stage-1 flush to every application vCPU before reopening
// guest admission. The Go-side MMU generation is updated only after this
// native frame returns. Without this publication, a host-resident reader that
// passed entry admission immediately before the pause can resume in the small
// interval between the monitor acknowledgement and that Go update.
static int ttc_ring0_request_application_tlb_flushes(void) {
	int status = pthread_mutex_lock(&ttc_ring0_bridge_registry_mu);
	if (status != 0) return status;
	for (struct ttc_ring0_bridge *bridge = ttc_ring0_bridges;
			bridge != NULL; bridge = bridge->registry_next) {
		if (atomic_load_explicit(&bridge->destroyed,
				memory_order_acquire) != 0) {
			continue;
		}
		_Atomic uint64_t *requested = (_Atomic uint64_t *)(
				bridge->state_host + TTC_STATE_TLB_FLUSH_REQUESTED_OFFSET);
		atomic_store_explicit(requested, 1, memory_order_release);
	}
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	return 0;
}

// Pause only for the native monitor execution interval. Keeping both pause
// endpoints in this C frame is essential: an EL1 Sentry may be stopped in the
// shared Go runtime, so executing Go allocation, channel, or safepoint code
// before reopening admission can deadlock a concurrent GC cycle.
static int32_t ttc_ring0_run_monitor_paused(uint64_t raw_vcpu,
		uintptr_t raw_exit) {
	int status = ttc_ring0_pause_guest_execution();
	if (status != 0) return (int32_t)HV_ERROR;
	hv_vcpu_t vcpu = (hv_vcpu_t)raw_vcpu;
	hv_vcpu_exit_t *exit = (hv_vcpu_exit_t *)raw_exit;
	hv_return_t hv_status;
	for (;;) {
		hv_status = hv_vcpu_run(vcpu);
		if (hv_status != HV_SUCCESS) break;
		if (exit->reason == HV_EXIT_REASON_CANCELED) continue;
		if (exit->reason == HV_EXIT_REASON_VTIMER_ACTIVATED) {
			(void)hv_vcpu_set_vtimer_mask(vcpu, true);
			continue;
		}
		break;
	}
	if (hv_status == HV_SUCCESS) {
		status = ttc_ring0_request_application_tlb_flushes();
		if (status != 0) hv_status = HV_ERROR;
	}
	ttc_ring0_resume_guest_execution();
	return (int32_t)hv_status;
}

static hv_memory_flags_t ttc_ring0_memory_flags(uint32_t access) {
	hv_memory_flags_t flags = 0;
	if ((access & 1u) != 0) flags |= HV_MEMORY_READ;
	if ((access & 2u) != 0) flags |= HV_MEMORY_WRITE;
	if ((access & 4u) != 0) flags |= HV_MEMORY_EXEC;
	return flags;
}

// ttc_ring0_stage2_map serializes every HVF mapping operation with runtime
// mutations. Tracked mappings are direct aliases of mutable Sentry memory.
static hv_return_t ttc_ring0_stage2_map(uintptr_t host, uint64_t gpa,
		uint64_t length, uint32_t access, int tracked) {
	struct ttc_ring0_mirror_mapping *mapping = NULL;
	if (tracked != 0) {
		size_t slabs = (size_t)(length / TTC_RING0_HOST_SLAB_SIZE);
		size_t words = (slabs + 63) / 64;
		if (slabs == 0 || words > (SIZE_MAX - sizeof(*mapping)) /
				(2 * sizeof(uint64_t))) return HV_NO_RESOURCES;
		mapping = calloc(1, sizeof(*mapping) +
				2 * words * sizeof(uint64_t));
		if (mapping == NULL) return HV_NO_RESOURCES;
		mapping->host = host;
		mapping->gpa = gpa;
		mapping->length = length;
		mapping->access = ttc_ring0_memory_flags(access);
		mapping->slabs = slabs;
		mapping->words = words;
		for (size_t slab = 0; slab < slabs; ++slab) {
			ttc_ring0_bit_set(ttc_ring0_mapping_registered(mapping), slab);
			ttc_ring0_bit_set(ttc_ring0_mapping_live(mapping), slab);
		}
	}
	int lock_status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
	if (lock_status != 0) {
		free(mapping);
		return HV_ERROR;
	}
	hv_return_t status = hv_vm_map((void *)host, gpa, (size_t)length,
			ttc_ring0_memory_flags(access));
	if (status == HV_SUCCESS && mapping != NULL) {
		mapping->next = ttc_ring0_mirror_mappings;
		ttc_ring0_mirror_mappings = mapping;
		mapping = NULL;
	}
	pthread_mutex_unlock(&ttc_ring0_mutation_mu);
	free(mapping);
	return status;
}

static hv_return_t ttc_ring0_unmap_live_run(
		struct ttc_ring0_mirror_mapping *mapping, size_t first,
		size_t end) {
	if (first == end) return HV_SUCCESS;
	return hv_vm_unmap(mapping->gpa + first * TTC_RING0_HOST_SLAB_SIZE,
			(end - first) * TTC_RING0_HOST_SLAB_SIZE);
}

static hv_return_t ttc_ring0_stage2_unmap(uint64_t gpa, uint64_t length,
		int tracked) {
	int lock_status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
	if (lock_status != 0) return HV_ERROR;
	if (tracked == 0) {
		hv_return_t status = hv_vm_unmap(gpa, (size_t)length);
		pthread_mutex_unlock(&ttc_ring0_mutation_mu);
		return status;
	}
	uint64_t end_gpa = gpa + length;
	bool found = false;
	hv_return_t result = HV_SUCCESS;
	struct ttc_ring0_mirror_mapping **link = &ttc_ring0_mirror_mappings;
	while (*link != NULL) {
		struct ttc_ring0_mirror_mapping *mapping = *link;
		uint64_t mapping_end = mapping->gpa + mapping->length;
		if (gpa < mapping_end && mapping->gpa < end_gpa) {
			uint64_t overlap_start = gpa > mapping->gpa ? gpa : mapping->gpa;
			uint64_t overlap_end = end_gpa < mapping_end ? end_gpa : mapping_end;
			size_t first = (size_t)((overlap_start - mapping->gpa) /
					TTC_RING0_HOST_SLAB_SIZE);
			size_t end = (size_t)((overlap_end - mapping->gpa) /
					TTC_RING0_HOST_SLAB_SIZE);
			uint64_t *registered = ttc_ring0_mapping_registered(mapping);
			uint64_t *live = ttc_ring0_mapping_live(mapping);
			for (size_t slab = first; slab < end;) {
				if (!ttc_ring0_bit_test(registered, slab)) {
					++slab;
					continue;
				}
				found = true;
				if (ttc_ring0_bit_test(live, slab)) {
					size_t run = slab + 1;
					while (run < end &&
							ttc_ring0_bit_test(registered, run) &&
							ttc_ring0_bit_test(live, run)) ++run;
					result = ttc_ring0_unmap_live_run(mapping, slab, run);
					if (result != HV_SUCCESS) goto done;
					for (size_t index = slab; index < run; ++index) {
						ttc_ring0_bit_clear(live, index);
					}
				}
				ttc_ring0_bit_clear(registered, slab);
				ttc_ring0_bit_clear(live, slab);
				++slab;
			}
		}
		if (ttc_ring0_mapping_empty(mapping)) {
			*link = mapping->next;
			free(mapping);
			continue;
		}
		link = &mapping->next;
	}
	if (!found) result = HV_BAD_ARGUMENT;

done:
	pthread_mutex_unlock(&ttc_ring0_mutation_mu);
	return result;
}

static bool ttc_ring0_mirror_live_overlap(uintptr_t address,
		uintptr_t length) {
	for (struct ttc_ring0_mirror_mapping *mapping =
			ttc_ring0_mirror_mappings; mapping != NULL;
			mapping = mapping->next) {
		uint64_t *registered = ttc_ring0_mapping_registered(mapping);
		uint64_t *live = ttc_ring0_mapping_live(mapping);
		for (size_t slab = 0; slab < mapping->slabs; ++slab) {
			if (ttc_ring0_bit_test(registered, slab) &&
					ttc_ring0_bit_test(live, slab) &&
					ttc_ring0_mapping_slab_overlaps(mapping, slab,
						address, length)) return true;
		}
	}
	return false;
}

static bool ttc_ring0_mirror_dormant_overlap(uintptr_t address,
		uintptr_t length) {
	for (struct ttc_ring0_mirror_mapping *mapping =
			ttc_ring0_mirror_mappings; mapping != NULL;
			mapping = mapping->next) {
		uint64_t *registered = ttc_ring0_mapping_registered(mapping);
		uint64_t *live = ttc_ring0_mapping_live(mapping);
		for (size_t slab = 0; slab < mapping->slabs; ++slab) {
			if (ttc_ring0_bit_test(registered, slab) &&
					!ttc_ring0_bit_test(live, slab) &&
					ttc_ring0_mapping_slab_overlaps(mapping, slab,
						address, length)) return true;
		}
	}
	return false;
}

static hv_return_t ttc_ring0_suspend_mirror(uintptr_t address,
		uintptr_t length) {
	for (struct ttc_ring0_mirror_mapping *mapping =
			ttc_ring0_mirror_mappings; mapping != NULL;
			mapping = mapping->next) {
		uint64_t *registered = ttc_ring0_mapping_registered(mapping);
		uint64_t *live = ttc_ring0_mapping_live(mapping);
		for (size_t slab = 0; slab < mapping->slabs;) {
			if (!ttc_ring0_bit_test(registered, slab) ||
					!ttc_ring0_bit_test(live, slab) ||
					!ttc_ring0_mapping_slab_overlaps(mapping, slab,
						address, length)) {
				++slab;
				continue;
			}
			size_t run = slab + 1;
			while (run < mapping->slabs &&
					ttc_ring0_bit_test(registered, run) &&
					ttc_ring0_bit_test(live, run) &&
					ttc_ring0_mapping_slab_overlaps(mapping, run,
						address, length)) ++run;
			hv_return_t status = ttc_ring0_unmap_live_run(mapping,
					slab, run);
			if (status != HV_SUCCESS) return status;
			for (size_t index = slab; index < run; ++index) {
				ttc_ring0_bit_clear(live, index);
			}
			slab = run;
		}
	}
	return HV_SUCCESS;
}

static bool ttc_ring0_host_slab_accessible(uintptr_t address,
		hv_memory_flags_t access) {
	struct ttc_ring0_region region;
	if (ttc_ring0_region_at(address, &region) != KERN_SUCCESS) return false;
	if (region.address > address || region.length < TTC_RING0_HOST_SLAB_SIZE ||
			address - region.address >
			region.length - TTC_RING0_HOST_SLAB_SIZE) return false;
	if ((access & HV_MEMORY_READ) != 0 &&
			(region.protection & VM_PROT_READ) == 0) return false;
	if ((access & HV_MEMORY_WRITE) != 0 &&
			(region.protection & VM_PROT_WRITE) == 0) return false;
	if ((access & HV_MEMORY_EXEC) != 0 &&
			(region.protection & VM_PROT_EXECUTE) == 0) return false;
	return true;
}

static hv_return_t ttc_ring0_rebind_mirror(uintptr_t address,
		uintptr_t length) {
	for (struct ttc_ring0_mirror_mapping *mapping =
			ttc_ring0_mirror_mappings; mapping != NULL;
			mapping = mapping->next) {
		uint64_t *registered = ttc_ring0_mapping_registered(mapping);
		uint64_t *live = ttc_ring0_mapping_live(mapping);
		for (size_t slab = 0; slab < mapping->slabs;) {
			uintptr_t host = mapping->host +
					slab * TTC_RING0_HOST_SLAB_SIZE;
			if (!ttc_ring0_bit_test(registered, slab) ||
					ttc_ring0_bit_test(live, slab) ||
					!ttc_ring0_mapping_slab_overlaps(mapping, slab,
						address, length) ||
					!ttc_ring0_host_slab_accessible(host,
						mapping->access)) {
				++slab;
				continue;
			}
			size_t run = slab + 1;
			while (run < mapping->slabs) {
				uintptr_t run_host = mapping->host +
						run * TTC_RING0_HOST_SLAB_SIZE;
				if (!ttc_ring0_bit_test(registered, run) ||
						ttc_ring0_bit_test(live, run) ||
						!ttc_ring0_mapping_slab_overlaps(mapping, run,
							address, length) ||
						!ttc_ring0_host_slab_accessible(run_host,
							mapping->access)) break;
				++run;
			}
			hv_return_t status = hv_vm_map((void *)host,
					mapping->gpa + slab * TTC_RING0_HOST_SLAB_SIZE,
					(run - slab) * TTC_RING0_HOST_SLAB_SIZE,
					mapping->access);
			if (status != HV_SUCCESS) return status;
			for (size_t index = slab; index < run; ++index) {
				ttc_ring0_bit_set(live, index);
			}
			slab = run;
		}
	}
	return HV_SUCCESS;
}

static int32_t ttc_ring0_runtime_vm_hook(
		struct ttc_runtime_vm_mutation *mutation) {
	if (mutation == NULL ||
			mutation->version != TTC_RUNTIME_VM_MUTATION_VERSION ||
			mutation->length == 0 ||
			mutation->address + mutation->length < mutation->address ||
			mutation->operation < TTC_RUNTIME_VM_MMAP ||
			mutation->operation > TTC_RUNTIME_VM_STACK_RESUME) return EINVAL;
	if (mutation->operation == TTC_RUNTIME_VM_STACK_SUSPEND ||
			mutation->operation == TTC_RUNTIME_VM_STACK_RESUME) {
		if (mutation->phase != TTC_RUNTIME_VM_BEFORE) return EINVAL;
		int status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
		if (status != 0) return status;
		bool suspend = mutation->operation == TTC_RUNTIME_VM_STACK_SUSPEND;
		bool affected = suspend ?
			ttc_ring0_mirror_live_overlap(mutation->address,
					mutation->length) :
			ttc_ring0_mirror_dormant_overlap(mutation->address,
					mutation->length);
		if (!affected) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return 0;
		}
		status = ttc_ring0_begin_runtime_mutation_locked();
		if (status != 0) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return status;
		}
		hv_return_t hv_status = suspend ?
			ttc_ring0_suspend_mirror(mutation->address, mutation->length) :
			ttc_ring0_rebind_mirror(mutation->address, mutation->length);
		ttc_ring0_end_runtime_mutation();
		return hv_status == HV_SUCCESS ? 0 : EIO;
	}
	if (mutation->phase == TTC_RUNTIME_VM_BEFORE) {
		int status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
		if (status != 0) return status;
		bool live = ttc_ring0_mirror_live_overlap(mutation->address,
				mutation->length);
		// MADV_FREE_REUSABLE deliberately leaves the host VA backed by pages
		// that Darwin may reclaim at any later time. Those stage-2 leases stay
		// dormant until MADV_FREE_REUSE makes the host mapping stable again.
		// Consequently the reuse callback must rendezvous even when every
		// overlapping mirror slab is currently dormant.
		bool reusing = mutation->operation == TTC_RUNTIME_VM_MADVISE &&
			mutation->flags == TTC_RUNTIME_MADV_FREE_REUSE &&
			ttc_ring0_mirror_dormant_overlap(mutation->address,
				mutation->length);
		if (!live && !reusing) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return 0;
		}
		status = ttc_ring0_begin_runtime_mutation_locked();
		if (status != 0) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return status;
		}
		hv_return_t hv_status = live ? ttc_ring0_suspend_mirror(
				mutation->address, mutation->length) : HV_SUCCESS;
		if (hv_status != HV_SUCCESS) {
			ttc_ring0_end_runtime_mutation();
			return EIO;
		}
		mutation->active = 1;
		return 0;
	}
	if (mutation->phase != TTC_RUNTIME_VM_AFTER) return EINVAL;
	bool releasing = mutation->operation == TTC_RUNTIME_VM_MADVISE &&
		mutation->flags == TTC_RUNTIME_MADV_FREE_REUSABLE;

	uintptr_t result_address = mutation->operation == TTC_RUNTIME_VM_MMAP &&
			mutation->error == 0 ? mutation->result : 0;
	if (mutation->active == 0) {
		if (result_address == 0) return 0;
		int status = pthread_mutex_lock(&ttc_ring0_mutation_mu);
		if (status != 0) return status;
		if (!ttc_ring0_mirror_dormant_overlap(result_address,
				mutation->length)) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return 0;
		}
		status = ttc_ring0_begin_runtime_mutation_locked();
		if (status != 0) {
			pthread_mutex_unlock(&ttc_ring0_mutation_mu);
			return status;
		}
	}
	// Do not rebind a FREE_REUSABLE range here. The host call only marks it
	// reclaimable; its physical backing can still change asynchronously after
	// the call returns. MADV_FREE_REUSE's after phase performs the rebind.
	hv_return_t hv_status = releasing ? HV_SUCCESS :
		ttc_ring0_rebind_mirror(mutation->address, mutation->length);
	if (hv_status == HV_SUCCESS && result_address != 0 &&
			result_address != mutation->address) {
		hv_status = ttc_ring0_rebind_mirror(result_address,
				mutation->length);
	}
	mutation->active = 0;
	ttc_ring0_end_runtime_mutation();
	return hv_status == HV_SUCCESS ? 0 : EIO;
}

static uintptr_t ttc_ring0_runtime_vm_hook_address(void) {
	return ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_runtime_vm_hook);
}

// Exercise the same replacement protocol as runtime.mmap(MAP_FIXED) without
// depending on allocator timing. This remains native so the test covers the
// exact ABI, admission gate, lease suspension, and stage-2 rebind path.
static int ttc_ring0_replace_tracked_host_mapping(uintptr_t address,
		size_t length, uint64_t value) {
	struct ttc_runtime_vm_mutation mutation = {
		.version = TTC_RUNTIME_VM_MUTATION_VERSION,
		.operation = TTC_RUNTIME_VM_MMAP,
		.phase = TTC_RUNTIME_VM_BEFORE,
		.address = address,
		.length = length,
		.protection = PROT_READ | PROT_WRITE,
		.flags = MAP_PRIVATE | MAP_ANON | MAP_FIXED,
		.fd = -1,
	};
	int status = ttc_ring0_runtime_vm_hook(&mutation);
	if (status != 0) return status;
	void *replacement = mmap((void *)address, length,
			PROT_READ | PROT_WRITE,
			MAP_PRIVATE | MAP_ANON | MAP_FIXED, -1, 0);
	int map_status = 0;
	if (replacement == MAP_FAILED) {
		map_status = errno;
		mutation.error = map_status;
		mutation.result = 0;
	} else {
		__builtin_memset(replacement, 0, length);
		__builtin_memcpy(replacement, &value, sizeof(value));
		mutation.result = (uintptr_t)replacement;
	}
	mutation.phase = TTC_RUNTIME_VM_AFTER;
	int hook_status = ttc_ring0_runtime_vm_hook(&mutation);
	if (map_status != 0) return map_status;
	return hook_status;
}

// HVF vCPUs and their bridges are owner-thread affine. Keep the active bridge
// in native TLS so a continuation redpilled immediately before the guest-entry
// probe remains identifiable after application registers repurpose X8.
static _Thread_local struct ttc_ring0_bridge *ttc_ring0_active_bridge;

// This is the exact instruction count of SwitchApplicationGuest. A canceled
// vCPU must not export a PC in the entry trampoline as a Darwin continuation:
// its registers and stack already have guest-only meanings. Resuming inside
// the VM is safe because the monitor consumes the pending cancellation.
#define TTC_RING0_GUEST_SWITCH_BYTES (10u * 4u)

static bool ttc_ring0_pc_in_entry_trampoline(
		const struct ttc_ring0_bridge *bridge, uint64_t pc) {
	return pc >= bridge->guest_switch_pc &&
			pc < bridge->guest_switch_pc + TTC_RING0_GUEST_SWITCH_BYTES;
}

#define TTC_RING0_BRIDGE_ERROR_OFFSET \
	offsetof(struct ttc_ring0_bridge, error)
#define TTC_RING0_BRIDGE_DESTROY_REQUESTED_OFFSET \
	offsetof(struct ttc_ring0_bridge, destroy_requested)
#define TTC_RING0_BRIDGE_EXTERNAL_INTERRUPT_REQUESTED_OFFSET \
	offsetof(struct ttc_ring0_bridge, external_interrupt_requested)
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
#define TTC_RING0_BRIDGE_GUEST_ENTRY_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, guest_entry_count)
#define TTC_RING0_BRIDGE_HOST_GATE_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, host_gate_count)
#define TTC_RING0_BRIDGE_REDPILL_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, redpill_count)
#define TTC_RING0_BRIDGE_HOST_RETURN_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, host_return_count)
#define TTC_RING0_BRIDGE_CACHE_SYNC_COUNT_OFFSET \
	offsetof(struct ttc_ring0_bridge, cache_sync_count)

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
	TTC_DIAG_FIELD(" entry_tid=", atomic_load_explicit(
			&bridge->entry_tid, memory_order_acquire));
	TTC_DIAG_FIELD(" reason=", bridge->exit->reason);
	TTC_DIAG_FIELD(" syndrome=", bridge->exit->exception.syndrome);
	TTC_DIAG_FIELD(" virtual_address=",
			bridge->exit->exception.virtual_address);
	TTC_DIAG_FIELD(" physical_address=",
			bridge->exit->exception.physical_address);
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
	// Re-establish the complete EL1 execution regime at every bluepill. HVF may
	// discard system-register state while a vCPU is resident in host EL0; only
	// restoring TTBR0 leaves a later application entry running with a zero TCR,
	// TTBR1, or VBAR. These calls occur once per host residency, never on the
	// guest-resident Linux syscall path.
	uint64_t tcr = 16ULL | (1ULL << 8) | (1ULL << 10) | (3ULL << 12) |
		(16ULL << 16) | (1ULL << 24) | (1ULL << 26) | (3ULL << 28) |
		(2ULL << 30) | (2ULL << 32) | (1ULL << 36);
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TCR_EL1,
			tcr)) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_MAIR_EL1,
			UINT64_C(0xff))) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TTBR1_EL1,
			bridge->ttbr1)) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_VBAR_EL1,
			bridge->vector_va)) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_TPIDR_EL1,
			bridge->state_va)) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_CPACR_EL1,
			UINT64_C(3) << 20)) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_CNTKCTL_EL1,
			UINT64_C(2))) != HV_SUCCESS) return status;
	if ((status = hv_vcpu_set_sys_reg(bridge->vcpu, HV_SYS_REG_SCTLR_EL1,
			UINT64_C(0x34909185))) != HV_SUCCESS) return status;
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
// copied back into the host signal context. A reusable bridge is destroyed only
// when its Sentry task reaches an explicit residency boundary.
static void ttc_ring0_finish_host_return(struct ttc_ring0_bridge *bridge) {
	atomic_fetch_add_explicit(&bridge->host_return_count, 1,
			memory_order_relaxed);
	if (bridge->destroy_on_host != 0 ||
			atomic_load_explicit(&bridge->destroy_requested,
				memory_order_acquire) != 0) {
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
	atomic_fetch_add_explicit(&bridge->guest_entry_count, 1,
			memory_order_relaxed);
	ucontext_t *ucontext = &bridge->host_context;
	arm_thread_state64_t host_ss = ucontext->uc_mcontext->__ss;
	arm_neon_state64_t host_ns = ucontext->uc_mcontext->__ns;
	uint32_t phase = TTC_RING0_PHASE_NONE;
	int status = ttc_ring0_load_context(bridge, ucontext, &phase);
	if (status != HV_SUCCESS) {
		ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
				(uint32_t)status, phase);
	}
	ttc_ring0_admit_guest(bridge);
	for (;;) {
		struct ttc_ring0_native_domain_slot *domain =
				&ttc_ring0_native_domains[bridge->native_domain_slot];
		atomic_store_explicit(&domain->tpidrro_el0,
				ttc_ring0_read_tpidrro_el0(), memory_order_relaxed);
		atomic_store_explicit(&domain->active, 1, memory_order_release);
		status = hv_vcpu_run(bridge->vcpu);
		atomic_store_explicit(&domain->active, 0, memory_order_release);
		if (status != HV_SUCCESS) {
			ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
					&host_ns, (uint32_t)status, TTC_RING0_PHASE_RUN);
		}
		if (bridge->exit->reason == HV_EXIT_REASON_CANCELED) {
			_Atomic uint64_t *guest_critical = (_Atomic uint64_t *)(
					bridge->state_host + TTC_STATE_GUEST_CRITICAL_OFFSET);
			if (atomic_load_explicit(&domain->critical,
					memory_order_acquire) != 0 ||
					atomic_load_explicit(guest_critical,
					memory_order_acquire) != 0) {
				// The stopped PC is inside a guest-only native callback. Any
				// external interrupt, destroy request, or MMU pause remains
				// published; resume until the callback or direct monitor command
				// reaches a host-safe PC.
				continue;
			}
			// A Go runtime mapping mutation is a process-wide secondary-MMU
			// rendezvous, not an application interrupt. hv_vcpu_run has already
			// returned on this owner thread, so publish the vCPU quiescent and
			// leave its complete EL0/EL1 architectural context untouched. The
			// mutation thread can now suspend/rebind the affected stage-2 lease;
			// admission resumes this exact context only after the lease is stable.
			if (atomic_load_explicit(&ttc_ring0_guest_pause_count,
					memory_order_acquire) != 0) {
				uint64_t pstate = 0;
				status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_CPSR,
						&pstate);
				if (status != HV_SUCCESS) {
					ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
							&host_ns, (uint32_t)status,
							TTC_RING0_PHASE_CANCEL_PSTATE);
				}
				if ((pstate & 0xf) == 0) {
					// A page-table transaction cannot resume a stopped EL0
					// context directly: that would bypass the per-vCPU local TLB
					// generation check. Route it through the monitor's IRQ exit;
					// Sentry transparently retries when cancel_requested is zero.
					bool external = atomic_exchange_explicit(
							&bridge->external_interrupt_requested, 0,
							memory_order_acq_rel) != 0;
					uint64_t exit_kind = external || atomic_load_explicit(
							&bridge->destroy_requested, memory_order_acquire) != 0
							? TTC_EXIT_INTERRUPTED : TTC_EXIT_RETRY;
					atomic_store_explicit((_Atomic uint64_t *)((uint8_t *)
							bridge->state_host +
							TTC_STATE_PENDING_INTERRUPT_EXIT_KIND_OFFSET),
							exit_kind, memory_order_release);
					status = hv_vcpu_set_pending_interrupt(bridge->vcpu,
							HV_INTERRUPT_TYPE_IRQ, true);
					if (status != HV_SUCCESS) {
						ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
								&host_ns, (uint32_t)status,
								TTC_RING0_PHASE_CANCEL_IRQ);
					}
				}
				atomic_store_explicit(&bridge->in_guest, 0,
						memory_order_release);
				ttc_ring0_admit_guest(bridge);
				continue;
			}
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
				// Convert cancellation during EL0 execution to an architected IRQ.
				// The native bridge publishes whether the exit is an external Sentry
				// cancellation or an internal MMU retry before asserting the line;
				// the monitor snapshots the application and restores the waiting
				// Sentry continuation.
				bool external = atomic_exchange_explicit(
						&bridge->external_interrupt_requested, 0,
						memory_order_acq_rel) != 0;
				uint64_t exit_kind = external || atomic_load_explicit(
						&bridge->destroy_requested, memory_order_acquire) != 0
						? TTC_EXIT_INTERRUPTED : TTC_EXIT_RETRY;
				atomic_store_explicit((_Atomic uint64_t *)((uint8_t *)
						bridge->state_host +
						TTC_STATE_PENDING_INTERRUPT_EXIT_KIND_OFFSET),
						exit_kind, memory_order_release);
				status = hv_vcpu_set_pending_interrupt(bridge->vcpu,
						HV_INTERRUPT_TYPE_IRQ, true);
				if (status == HV_SUCCESS) continue;
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_CANCEL_IRQ);
			}
			// A cancellation can win while SwitchApplicationGuest has already
			// adopted guest-only register/stack meanings. Exporting that PC as a
			// Darwin continuation would turn the controlled transfer into an
			// unhandled SIGILL. Let the vCPU reach the monitor/application
			// boundary, where the pending cancellation becomes an IRQ or redpill.
			if (ttc_ring0_pc_in_entry_trampoline(bridge, pc)) {
				continue;
			}
			// A cancellation caught while an EL1 exception handler is restoring
			// the Sentry continuation must finish in the VM. Exporting a monitor
			// PC as a Darwin continuation would resume at an unmapped high VA.
			if (pc >= TTC_MONITOR_CODE_VA &&
					pc - TTC_MONITOR_CODE_VA < TTC_MONITOR_CODE_CAPACITY) {
				continue;
			}
			atomic_store_explicit(&bridge->external_interrupt_requested, 0,
					memory_order_release);
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
		if (bridge->exit->reason == HV_EXIT_REASON_EXCEPTION &&
				((bridge->exit->exception.syndrome >> 26) & 0x3f) == 0x16 &&
				hvc == TTC_RING0_HVC_INTERRUPT_ACK) {
			// Virtual IRQ/FIQ inputs are level-triggered. The monitor has saved
			// the interrupted application state and cannot lower an HVF-owned
			// input itself, so acknowledge it here before allowing the handler
			// to restore the waiting Sentry continuation.
			status = hv_vcpu_set_pending_interrupt(bridge->vcpu,
					HV_INTERRUPT_TYPE_IRQ, false);
			if (status == HV_SUCCESS) {
				status = hv_vcpu_set_pending_interrupt(bridge->vcpu,
						HV_INTERRUPT_TYPE_FIQ, false);
			}
			if (status != HV_SUCCESS) {
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_CANCEL_IRQ);
			}
			continue;
		}
		if (bridge->exit->reason == HV_EXIT_REASON_EXCEPTION &&
				((bridge->exit->exception.syndrome >> 26) & 0x3f) == 0x16 &&
				hvc == TTC_RING0_HVC_CACHE_SYNC) {
			uint64_t address;
			uint64_t length;
			status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_X0, &address);
			if (status == HV_SUCCESS) {
				status = hv_vcpu_get_reg(bridge->vcpu, HV_REG_X1, &length);
			}
			struct ttc_ring0_region region;
			bool valid = status == HV_SUCCESS && length != 0 &&
					address + length >= address &&
					ttc_ring0_region_at(address, &region) == KERN_SUCCESS &&
					region.address <= address &&
					address - region.address <= region.length &&
					length <= region.length - (address - region.address) &&
					region.external_pager == 0 &&
					region.user_tag == VM_MEMORY_APPLICATION_SPECIFIC_3 &&
					(region.protection & VM_PROT_READ) != 0;
			if (!valid) {
				// Rejection is an internal protocol result, not a safe point for
				// exporting the guest-critical Go continuation. Return an error in X0;
				// the caller redpills only after restoring its ordinary stack and PC.
				status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_X0, UINT64_MAX);
				if (status != HV_SUCCESS) {
					ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
							&host_ns, (uint32_t)status,
							TTC_RING0_PHASE_UNEXPECTED_EXIT);
				}
				continue;
			}
			// This runs at Darwin EL0 on the vCPU owner thread, but resumes the
			// stopped EL1 PC directly. Unlike a host-service gate it needs no
			// architectural context copy or signal return.
			sys_dcache_flush((void *)(uintptr_t)address, (size_t)length);
			sys_icache_invalidate((void *)(uintptr_t)address, (size_t)length);
			atomic_fetch_add_explicit(&bridge->cache_sync_count, 1,
					memory_order_relaxed);
			status = hv_vcpu_set_reg(bridge->vcpu, HV_REG_X0, 0);
			if (status != HV_SUCCESS) {
				ttc_ring0_fail_host_return(bridge, ucontext, &host_ss,
						&host_ns, (uint32_t)status,
						TTC_RING0_PHASE_UNEXPECTED_EXIT);
			}
			continue;
		}
		if (bridge->exit->reason != HV_EXIT_REASON_EXCEPTION ||
				((bridge->exit->exception.syndrome >> 26) & 0x3f) != 0x16 ||
				(hvc != TTC_RING0_HVC_REDPILL &&
				 hvc != TTC_RING0_HVC_HOST_GATE)) {
			ttc_ring0_fail_host_return(bridge, ucontext, &host_ss, &host_ns,
				UINT32_MAX, TTC_RING0_PHASE_UNEXPECTED_EXIT);
		}
		if (hvc == TTC_RING0_HVC_HOST_GATE) {
			atomic_fetch_add_explicit(&bridge->host_gate_count, 1,
					memory_order_relaxed);
		} else {
			atomic_fetch_add_explicit(&bridge->redpill_count, 1,
					memory_order_relaxed);
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
			// Host gates publish the pre-exception continuation. GP/FP/TLS/SP
			// came from the vCPU; only PC and NZCV must reflect the continuation
			// rather than the monitor HVC itself.
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
static uintptr_t ttc_ring0_guest_switch_pc;
static uintptr_t ttc_ring0_host_gate_pc;
static uintptr_t ttc_ring0_restore_pc;
static uintptr_t ttc_ring0_current_el_pc;
static uintptr_t ttc_ring0_runtime_current_el_pc;
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
		if (bridge->destroy_on_host != 0 ||
				atomic_load_explicit(&bridge->destroy_requested,
					memory_order_acquire) != 0) {
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
	if (pc == ttc_ring0_current_el_pc ||
			pc == ttc_ring0_runtime_current_el_pc) {
		// CurrentEL is deliberately executed before selecting or creating an
		// owner-affine vCPU. At host EL0, report zero and skip the trapped MRS.
		ss->__x[0] = 0;
		ttc_ring0_set_pc(ss, pc + 4);
		return;
	}
	if (pc == ttc_ring0_guest_switch_pc) {
		// A translation mutation can redpill an EL1 continuation after Go's
		// CurrentEL check but before SwitchApplicationGuest reads TPIDR_EL1.
		// Convert the controlled host SIGILL into an interrupted application
		// entry. The application PC was not advanced, so Sentry can retry it
		// after the acknowledged mutation.
		struct ttc_ring0_bridge *bridge = ttc_ring0_active_bridge;
		if (bridge == NULL || bridge->magic != TTC_RING0_BRIDGE_MAGIC) {
			ttc_ring0_forward_sigill(signo, info, context);
			return;
		}
		// This is an internal entry retry, distinct from the monitor's
		// architected EL0 interrupt exit (3).
		uint64_t retry = 5;
		__builtin_memcpy((uint8_t *)bridge->state_host +
				TTC_STATE_EXIT_KIND_OFFSET, &retry, sizeof(retry));
		atomic_thread_fence(memory_order_release);
		ttc_ring0_set_pc(ss, ttc_ring0_get_lr(ss));
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
	// Establish the owner-thread residency before a cancellation can export the
	// continuation back to Darwin.
	// SwitchApplicationGuest's controlled host-side TPIDR_EL1 trap relies on
	// this TLS slot to turn that redpill race into ExitInterrupted.
	ttc_ring0_active_bridge = bridge;
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
		uintptr_t guest_switch_pc,
		uintptr_t host_gate_pc,
		uintptr_t current_el_pc) {
	if (atomic_load_explicit(&ttc_ring0_handler_installed,
			memory_order_acquire) != 0) {
		return ttc_ring0_bluepill_pc == bluepill_pc &&
			ttc_ring0_guest_switch_pc == guest_switch_pc &&
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
	ttc_ring0_guest_switch_pc = guest_switch_pc;
	ttc_ring0_host_gate_pc = host_gate_pc;
	ttc_ring0_current_el_pc = current_el_pc;
	ttc_ring0_runtime_current_el_pc = ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_runtime_current_el);
	ttc_ring0_restore_pc = ttc_ring0_strip_instruction_pointer(
			(uintptr_t)&ttc_ring0_restore_trap);
	atomic_store_explicit(&ttc_ring0_handler_installed, 1,
			memory_order_release);
	return 0;
}

static struct ttc_ring0_bridge *ttc_ring0_bridge_create(hv_vcpu_t vcpu,
		const hv_vcpu_exit_t *exit, uint64_t ttbr0, uint64_t ttbr1,
		uint64_t vector_va, uint64_t state_va,
		uintptr_t state_host, uintptr_t guest_switch_pc, int destroy_on_host) {
	if (ttc_ring0_start_preemption_thread() != 0) return NULL;
	struct ttc_ring0_bridge *bridge = ttc_ring0_bridge_storage_acquire();
	if (bridge == NULL) return NULL;
	bridge->host_stack_size = TTC_RING0_HOST_STACK_SIZE;
	bridge->host_stack = mmap(NULL, bridge->host_stack_size,
			PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	if (bridge->host_stack == MAP_FAILED) {
		ttc_ring0_bridge_storage_release(bridge);
		return NULL;
	}
	bridge->magic = TTC_RING0_BRIDGE_MAGIC;
	bridge->vcpu = vcpu;
	bridge->exit = exit;
	bridge->ttbr0 = ttbr0;
	bridge->ttbr1 = ttbr1;
	bridge->vector_va = vector_va;
	bridge->state_va = state_va;
	bridge->state_host = state_host;
	bridge->guest_switch_pc = guest_switch_pc;
	bridge->destroy_on_host = destroy_on_host != 0;
	bool reserved_domain = false;
	for (uint32_t i = 0; i < TTC_RING0_MAX_APPLICATION_VCPUS; ++i) {
		uint32_t expected = 0;
		if (atomic_compare_exchange_strong_explicit(
				&ttc_ring0_native_domains[i].allocated, &expected, 1,
				memory_order_acq_rel, memory_order_relaxed)) {
			bridge->native_domain_slot = i;
			reserved_domain = true;
			break;
		}
	}
	if (!reserved_domain) {
		(void)munmap(bridge->host_stack, bridge->host_stack_size);
		ttc_ring0_bridge_storage_release(bridge);
		return NULL;
	}
	(void)pthread_threadid_np(NULL, &bridge->owner_tid);
	(void)pthread_sigmask(SIG_SETMASK, NULL, &bridge->host_context.uc_sigmask);
	if (pthread_mutex_lock(&ttc_ring0_bridge_registry_mu) != 0) {
		atomic_store_explicit(
				&ttc_ring0_native_domains[bridge->native_domain_slot].allocated,
				0, memory_order_release);
		(void)munmap(bridge->host_stack, bridge->host_stack_size);
		ttc_ring0_bridge_storage_release(bridge);
		return NULL;
	}
	bridge->registry_next = ttc_ring0_bridges;
	ttc_ring0_bridges = bridge;
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	return bridge;
}

static uint64_t ttc_ring0_bridge_exit_virtual_address(
		const struct ttc_ring0_bridge *bridge) {
	return bridge->exit->exception.virtual_address;
}

static uint64_t ttc_ring0_bridge_exit_physical_address(
		const struct ttc_ring0_bridge *bridge) {
	return bridge->exit->exception.physical_address;
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
	pthread_mutex_lock(&ttc_ring0_bridge_registry_mu);
	struct ttc_ring0_bridge **link = &ttc_ring0_bridges;
	while (*link != NULL && *link != bridge) {
		link = &(*link)->registry_next;
	}
	if (*link == bridge) *link = bridge->registry_next;
	pthread_mutex_unlock(&ttc_ring0_bridge_registry_mu);
	if (ttc_ring0_active_bridge == bridge) ttc_ring0_active_bridge = NULL;
	struct ttc_ring0_native_domain_slot *domain =
			&ttc_ring0_native_domains[bridge->native_domain_slot];
	atomic_store_explicit(&domain->active, 0, memory_order_release);
	atomic_store_explicit(&domain->critical, 0, memory_order_release);
	atomic_store_explicit(&domain->tpidrro_el0, 0, memory_order_relaxed);
	atomic_store_explicit(&domain->allocated, 0, memory_order_release);
	bridge->magic = 0;
	if (bridge->host_stack != NULL && bridge->host_stack != MAP_FAILED) {
		(void)munmap(bridge->host_stack, bridge->host_stack_size);
	}
	ttc_ring0_bridge_storage_release(bridge);
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
	ring0BridgeDestroyRequestedOffset  = uintptr(C.TTC_RING0_BRIDGE_DESTROY_REQUESTED_OFFSET)
	ring0BridgeExternalInterruptOffset = uintptr(C.TTC_RING0_BRIDGE_EXTERNAL_INTERRUPT_REQUESTED_OFFSET)
	ring0BridgeErrorOffset             = uintptr(C.TTC_RING0_BRIDGE_ERROR_OFFSET)
	ring0BridgeErrorPhaseOffset        = uintptr(C.TTC_RING0_BRIDGE_ERROR_PHASE_OFFSET)
	ring0BridgeInGuestOffset           = uintptr(C.TTC_RING0_BRIDGE_IN_GUEST_OFFSET)
	ring0BridgeDestroyedOffset         = uintptr(C.TTC_RING0_BRIDGE_DESTROYED_OFFSET)
	ring0BridgeLastX0Offset            = uintptr(C.TTC_RING0_BRIDGE_LAST_X0_OFFSET)
	ring0BridgeLastHostX0Offset        = uintptr(C.TTC_RING0_BRIDGE_LAST_HOST_X0_OFFSET)
	ring0BridgeLastCancelPCOffset      = uintptr(C.TTC_RING0_BRIDGE_LAST_CANCEL_PC_OFFSET)
	ring0BridgeLastCancelPStateOffset  = uintptr(C.TTC_RING0_BRIDGE_LAST_CANCEL_PSTATE_OFFSET)
	ring0BridgeCancelCountOffset       = uintptr(C.TTC_RING0_BRIDGE_CANCEL_COUNT_OFFSET)
	ring0BridgeGuestEntryCountOffset   = uintptr(C.TTC_RING0_BRIDGE_GUEST_ENTRY_COUNT_OFFSET)
	ring0BridgeHostGateCountOffset     = uintptr(C.TTC_RING0_BRIDGE_HOST_GATE_COUNT_OFFSET)
	ring0BridgeRedpillCountOffset      = uintptr(C.TTC_RING0_BRIDGE_REDPILL_COUNT_OFFSET)
	ring0BridgeHostReturnCountOffset   = uintptr(C.TTC_RING0_BRIDGE_HOST_RETURN_COUNT_OFFSET)
	ring0BridgeCacheSyncCountOffset    = uintptr(C.TTC_RING0_BRIDGE_CACHE_SYNC_COUNT_OFFSET)
)

type ring0HostRegion struct {
	address       uintptr
	length        uint64
	objectOffset  uint64
	objectID      uint64
	protection    uint32
	userTag       uint32
	externalPager bool
	mainImage     bool
}

type ring0TimeFastpath struct {
	clockGettimePC   uint64
	machAbsolutePC   uint64
	numer            uint64
	denom            uint64
	vtimerOffset     uint64
	realtimeOffsetNS int64
}

func calibrateRing0TimeFastpath(vtimerOffset uint64) (ring0TimeFastpath, error) {
	var calibration C.struct_ttc_ring0_time_fastpath
	if errno := C.ttc_ring0_calibrate_time_fastpath(&calibration); errno != 0 {
		return ring0TimeFastpath{}, fmt.Errorf("calibrating EL1 runtime clocks: errno %d", int(errno))
	}
	result := ring0TimeFastpath{
		clockGettimePC:   uint64(calibration.clock_gettime_pc),
		machAbsolutePC:   uint64(calibration.mach_absolute_time_pc),
		numer:            uint64(calibration.numer),
		denom:            uint64(calibration.denom),
		vtimerOffset:     vtimerOffset,
		realtimeOffsetNS: int64(calibration.realtime_offset_ns),
	}
	if result.clockGettimePC == 0 || result.machAbsolutePC == 0 || result.numer == 0 || result.denom == 0 {
		return ring0TimeFastpath{}, fmt.Errorf("invalid EL1 runtime clock calibration: %+v", result)
	}
	return result, nil
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
			address:       uintptr(address),
			length:        length,
			objectOffset:  uint64(region.object_offset),
			objectID:      uint64(region.object_id),
			protection:    uint32(region.protection),
			userTag:       uint32(region.user_tag),
			externalPager: region.external_pager != 0,
			mainImage:     C.ttc_ring0_main_image(region.address, region.length) != 0,
		})
	}
	return result, nil
}

func ring0HostRegionAt(target uintptr) (ring0HostRegion, bool, error) {
	var region C.struct_ttc_ring0_region
	status := C.ttc_ring0_region_at(C.uint64_t(target), &region)
	if status == C.KERN_INVALID_ADDRESS {
		return ring0HostRegion{}, false, nil
	}
	if status != C.KERN_SUCCESS {
		return ring0HostRegion{}, false, fmt.Errorf("looking up Darwin VM region at %#x: status %d", target, int(status))
	}
	address := uint64(region.address)
	length := uint64(region.length)
	if length == 0 || address%vmm.HostSlabSize != 0 || length%vmm.HostSlabSize != 0 ||
		address+length < address || uint64(target) < address || uint64(target)-address >= length ||
		address+length > ring0HostVALimit {
		return ring0HostRegion{}, false, fmt.Errorf("invalid Darwin VM region [%#x, %#x) for %#x", address, address+length, target)
	}
	return ring0HostRegion{
		address:       uintptr(address),
		length:        length,
		objectOffset:  uint64(region.object_offset),
		objectID:      uint64(region.object_id),
		protection:    uint32(region.protection),
		userTag:       uint32(region.user_tag),
		externalPager: region.external_pager != 0,
		mainImage:     C.ttc_ring0_main_image(region.address, region.length) != 0,
	}, true, nil
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
		C.uintptr_t(ring0asm.SwitchApplicationGuestTrapAddress()),
		C.uintptr_t(ring0asm.EnsureHostTrapAddress()),
		C.uintptr_t(ring0asm.CurrentELTrapAddress())); errno != 0 {
		return fmt.Errorf("installing HVF ring0 SIGILL bridge: errno %d", int(errno))
	}
	return nil
}

const ring0RuntimeVMMemoryHookVersion = uint32(1)

func installRing0RuntimeVMMemoryHook() error {
	status := ring0SetVMMemoryHooks(
		ring0RuntimeVMMemoryHookVersion,
		ring0asm.EnsureHostTrapAddress(),
		uintptr(C.ttc_ring0_runtime_vm_hook_address()),
	)
	if status != 0 {
		return fmt.Errorf("installing Darwin runtime VM memory hook ABI v%d: status %d", ring0RuntimeVMMemoryHookVersion, status)
	}
	timeHook := uintptr(C.ttc_ring0_runtime_time_hook_address())
	if status := ring0SetRuntimeTimeHook(1, timeHook); status != 0 {
		return fmt.Errorf("installing Darwin runtime time hook ABI v1: status %d", status)
	}
	return nil
}

func replaceRing0TrackedHostMapping(address uintptr, length uint64, value uint64) error {
	status := C.ttc_ring0_replace_tracked_host_mapping(
		C.uintptr_t(address), C.size_t(length), C.uint64_t(value))
	if status != 0 {
		return fmt.Errorf("replacing tracked Sentry host mapping [%#x, %#x): status %d", address, address+uintptr(length), status)
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
		C.uint64_t(v.monitor.ttbr1()),
		C.uint64_t(monitorCodeVA+v.monitor.vectorOffset),
		C.uint64_t(v.stateVA),
		C.uintptr_t(v.stateHost),
		C.uintptr_t(ring0asm.SwitchApplicationGuestAddress()),
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

func (b *ring0Bridge) requestDestroy() {
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeDestroyRequestedOffset)), 1)
	b.requestExternalInterrupt()
}

func (b *ring0Bridge) destroyRequested() bool {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeDestroyRequestedOffset))) != 0
}

func (b *ring0Bridge) requestExternalInterrupt() {
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeExternalInterruptOffset)), 1)
}

func (b *ring0Bridge) externalInterruptRequested() bool {
	return atomic.LoadUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeExternalInterruptOffset))) != 0
}

func (b *ring0Bridge) clearExternalInterrupt() {
	atomic.StoreUint32((*uint32)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeExternalInterruptOffset)), 0)
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
		return fmt.Errorf("%s: status %#x while %s: exit_va=%#x exit_ipa=%#x host_gate_esr=%#x host_gate_far=%#x host_gate_pc=%#x internal_esr=%#x internal_elr=%#x internal_vector=%#x",
			prefix, status, ring0ErrorPhaseName(phase),
			uint64(C.ttc_ring0_bridge_exit_virtual_address(b.pointer)),
			uint64(C.ttc_ring0_bridge_exit_physical_address(b.pointer)),
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

type ring0BridgeStats struct {
	guestEntries uint64
	hostGates    uint64
	redpills     uint64
	hostReturns  uint64
	cacheSyncs   uint64
}

func (b *ring0Bridge) stats() ring0BridgeStats {
	return ring0BridgeStats{
		guestEntries: atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeGuestEntryCountOffset))),
		hostGates:    atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeHostGateCountOffset))),
		redpills:     atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeRedpillCountOffset))),
		hostReturns:  atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeHostReturnCountOffset))),
		cacheSyncs:   atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(b.pointer), ring0BridgeCacheSyncCountOffset))),
	}
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

// mapRing0MirrorStage2Source installs a direct alias of mutable Sentry host
// memory and registers it with the native runtime-mutation notifier.
func (v *hvfVM) mapRing0MirrorStage2Source(gpa, hostAddress uintptr, length uint64, access vmm.Stage2Access) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	if uint64(gpa)%vmm.HostSlabSize != 0 || uint64(hostAddress)%vmm.HostSlabSize != 0 || length == 0 || length%vmm.HostSlabSize != 0 {
		return fmt.Errorf("unaligned HVF Sentry mirror map gpa=%#x host=%#x length=%#x", gpa, hostAddress, length)
	}
	return mapNativeStage2(uint64(gpa), hostAddress, length, access, true)
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

func (v *hvfVM) unmapRing0MirrorStage2GPA(gpa uint64, length uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("HVF VM is closed")
	}
	if gpa%vmm.HostSlabSize != 0 || length == 0 || length%vmm.HostSlabSize != 0 {
		return fmt.Errorf("unaligned HVF Sentry mirror unmap gpa=%#x length=%#x", gpa, length)
	}
	return unmapNativeStage2(gpa, length, true)
}

func mapNativeStage2(gpa uint64, hostAddress uintptr, length uint64, access vmm.Stage2Access, tracked bool) error {
	status := C.ttc_ring0_stage2_map(C.uintptr_t(hostAddress), C.uint64_t(gpa),
		C.uint64_t(length), C.uint32_t(access), C.int(boolToInt(tracked)))
	if status != C.HV_SUCCESS {
		return fmt.Errorf("hv_vm_map(gpa=%#x, host=%#x, length=%#x, tracked=%t) failed: %d", gpa, hostAddress, length, tracked, status)
	}
	return nil
}

func unmapNativeStage2(gpa, length uint64, tracked bool) error {
	status := C.ttc_ring0_stage2_unmap(C.uint64_t(gpa), C.uint64_t(length), C.int(boolToInt(tracked)))
	if status != C.HV_SUCCESS {
		return fmt.Errorf("hv_vm_unmap(gpa=%#x, length=%#x, tracked=%t) failed: %d", gpa, length, tracked, status)
	}
	return nil
}

func runNativeMonitorPaused(vcpu uint64, exit unsafe.Pointer) int32 {
	return int32(C.ttc_ring0_run_monitor_paused(C.uint64_t(vcpu), C.uintptr_t(uintptr(exit))))
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
		bridge.requestExternalInterrupt()
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
