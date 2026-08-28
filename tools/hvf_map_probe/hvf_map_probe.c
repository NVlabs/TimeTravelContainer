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

// hvf_map_probe characterizes what an HVF stage-2 mapping observes when the
// host mutates the VM region originally passed to hv_vm_map(). Apple does not
// document these mutation semantics. Consequently, none of the observations
// produced by this program are a portable API guarantee.

#include <Hypervisor/Hypervisor.h>
#include <errno.h>
#include <inttypes.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/vm_statistics.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

#if !defined(__APPLE__) || !defined(__aarch64__)
#error "hvf_map_probe requires Apple Silicon macOS"
#endif

enum {
  kCodeIPA = 0x40000,
  kDataIPA = 0x80000,
};

static const uint64_t kInitial = UINT64_C(0x1111222233334444);
static const uint64_t kReplacement = UINT64_C(0xaaaabbbbccccdddd);
static const uint64_t kGuestWrite = UINT64_C(0x123456789abcdef0);

typedef struct probe_vm {
  size_t page_size;
  size_t data_size;
  void *code;
  void *data;
  hv_vcpu_t vcpu;
  hv_vcpu_exit_t *exit;
  bool vm_created;
  bool code_mapped;
  bool data_mapped;
  bool vcpu_created;
} probe_vm_t;

typedef enum guest_result_kind {
  kGuestValue,
  kGuestFault,
  kGuestError,
} guest_result_kind_t;

typedef struct guest_result {
  guest_result_kind_t kind;
  uint64_t value;
  uint64_t syndrome;
  uint64_t physical_address;
  hv_return_t status;
} guest_result_t;

static void print_hv_error(const char *operation, hv_return_t status) {
  fprintf(stderr, "%s failed: hv_return=%d\n", operation, status);
}

static void *map_anon_at(void *address, size_t length, bool fixed) {
  int flags = MAP_PRIVATE | MAP_ANON;
  if (fixed) {
    flags |= MAP_FIXED;
  }
  void *mapped = mmap(address, length, PROT_READ | PROT_WRITE, flags, -1, 0);
  if (mapped == MAP_FAILED) {
    return NULL;
  }
  return mapped;
}

static int probe_vm_init_sized(probe_vm_t *vm, size_t data_size) {
  memset(vm, 0, sizeof(*vm));
  vm->page_size = (size_t)getpagesize();
  if (vm->page_size != 16384) {
    fprintf(stderr, "host page size is %zu, want 16384\n", vm->page_size);
    return -1;
  }
  vm->data_size = data_size;
  if (data_size == 0 || data_size % vm->page_size != 0) {
    fprintf(stderr, "invalid data size %zu\n", data_size);
    return -1;
  }
  vm->code = map_anon_at(NULL, vm->page_size, false);
  vm->data = map_anon_at(NULL, vm->data_size, false);
  if (vm->code == NULL || vm->data == NULL) {
    fprintf(stderr, "mmap: %s\n", strerror(errno));
    return -1;
  }

  // read:  ldr x1, [x0]; hvc #0
  // write: str x1, [x0]; hvc #0
  const uint32_t instructions[] = {
      UINT32_C(0xf9400001), UINT32_C(0xd4000002),
      UINT32_C(0xf9000001), UINT32_C(0xd4000002),
  };
  memcpy(vm->code, instructions, sizeof(instructions));
  __builtin___clear_cache(vm->code, (char *)vm->code + vm->page_size);
  *(volatile uint64_t *)vm->data = kInitial;

  hv_return_t status = hv_vm_create(NULL);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vm_create", status);
    return -1;
  }
  vm->vm_created = true;
  status = hv_vm_map(vm->code, kCodeIPA, vm->page_size,
                     HV_MEMORY_READ | HV_MEMORY_EXEC);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vm_map(code)", status);
    return -1;
  }
  vm->code_mapped = true;
  status = hv_vm_map(vm->data, kDataIPA, vm->data_size,
                     HV_MEMORY_READ | HV_MEMORY_WRITE);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vm_map(data)", status);
    return -1;
  }
  vm->data_mapped = true;
  status = hv_vcpu_create(&vm->vcpu, &vm->exit, NULL);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vcpu_create", status);
    return -1;
  }
  vm->vcpu_created = true;

  uint64_t sctlr = 0;
  status = hv_vcpu_get_sys_reg(vm->vcpu, HV_SYS_REG_SCTLR_EL1, &sctlr);
  if (status == HV_SUCCESS) {
    status = hv_vcpu_set_sys_reg(vm->vcpu, HV_SYS_REG_SCTLR_EL1,
                                 sctlr & ~UINT64_C(1));
  }
  if (status != HV_SUCCESS) {
    print_hv_error("disabling stage-1 MMU", status);
    return -1;
  }
  return 0;
}

static int probe_vm_init(probe_vm_t *vm) {
  return probe_vm_init_sized(vm, (size_t)getpagesize());
}

static int unmap_data_stage2(probe_vm_t *vm) {
  if (!vm->data_mapped) {
    return 0;
  }
  hv_return_t status = hv_vm_unmap(kDataIPA, vm->data_size);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vm_unmap(data)", status);
    return -1;
  }
  vm->data_mapped = false;
  return 0;
}

static int map_data_stage2(probe_vm_t *vm) {
  if (vm->data_mapped) {
    return 0;
  }
  hv_return_t status = hv_vm_map(vm->data, kDataIPA, vm->data_size,
                                 HV_MEMORY_READ | HV_MEMORY_WRITE);
  if (status != HV_SUCCESS) {
    print_hv_error("hv_vm_map(data replacement)", status);
    return -1;
  }
  vm->data_mapped = true;
  return 0;
}

static int probe_vm_destroy(probe_vm_t *vm) {
  int failed = 0;
  if (vm->vcpu_created) {
    hv_return_t status = hv_vcpu_destroy(vm->vcpu);
    if (status != HV_SUCCESS) {
      print_hv_error("hv_vcpu_destroy", status);
      failed = -1;
    }
    vm->vcpu_created = false;
  }
  if (vm->data_mapped && unmap_data_stage2(vm) != 0) {
    failed = -1;
  }
  if (vm->code_mapped) {
    hv_return_t status = hv_vm_unmap(kCodeIPA, vm->page_size);
    if (status != HV_SUCCESS) {
      print_hv_error("hv_vm_unmap(code)", status);
      failed = -1;
    }
    vm->code_mapped = false;
  }
  if (vm->vm_created) {
    hv_return_t status = hv_vm_destroy();
    if (status != HV_SUCCESS) {
      print_hv_error("hv_vm_destroy", status);
      failed = -1;
    }
    vm->vm_created = false;
  }
  if (vm->code != NULL && munmap(vm->code, vm->page_size) != 0) {
    fprintf(stderr, "munmap(code): %s\n", strerror(errno));
    failed = -1;
  }
  if (vm->data != NULL && munmap(vm->data, vm->data_size) != 0) {
    fprintf(stderr, "munmap(data): %s\n", strerror(errno));
    failed = -1;
  }
  return failed;
}

static guest_result_t run_guest(probe_vm_t *vm, uint64_t pc,
                                uint64_t data_ipa, uint64_t x1) {
  guest_result_t result = {.kind = kGuestError};
  hv_return_t status = hv_vcpu_set_reg(vm->vcpu, HV_REG_X0, data_ipa);
  if (status == HV_SUCCESS) {
    status = hv_vcpu_set_reg(vm->vcpu, HV_REG_X1, x1);
  }
  if (status == HV_SUCCESS) {
    status = hv_vcpu_set_reg(vm->vcpu, HV_REG_PC, pc);
  }
  if (status == HV_SUCCESS) {
    // EL1h with D, A, I, and F masked.
    status = hv_vcpu_set_reg(vm->vcpu, HV_REG_CPSR, UINT64_C(0x3c5));
  }
  if (status == HV_SUCCESS) {
    status = hv_vcpu_run(vm->vcpu);
  }
  if (status != HV_SUCCESS) {
    result.status = status;
    return result;
  }
  if (vm->exit->reason != HV_EXIT_REASON_EXCEPTION) {
    result.status = HV_ERROR;
    return result;
  }
  result.syndrome = vm->exit->exception.syndrome;
  result.physical_address = vm->exit->exception.physical_address;
  const uint64_t ec = (result.syndrome >> 26) & UINT64_C(0x3f);
  if (ec != UINT64_C(0x16)) {
    result.kind = kGuestFault;
    return result;
  }
  status = hv_vcpu_get_reg(vm->vcpu, HV_REG_X1, &result.value);
  if (status != HV_SUCCESS) {
    result.status = status;
    return result;
  }
  result.kind = kGuestValue;
  result.status = HV_SUCCESS;
  return result;
}

static guest_result_t guest_read(probe_vm_t *vm) {
  return run_guest(vm, kCodeIPA, kDataIPA, 0);
}

static guest_result_t guest_write(probe_vm_t *vm, uint64_t value) {
  return run_guest(vm, kCodeIPA + 8, kDataIPA, value);
}

static guest_result_t guest_write_at(probe_vm_t *vm, size_t offset,
                                     uint64_t value) {
  return run_guest(vm, kCodeIPA + 8, kDataIPA + offset, value);
}

static void print_guest_result(const char *step, guest_result_t result) {
  switch (result.kind) {
    case kGuestValue:
      printf("step=%s result=value value=0x%016" PRIx64 "\n", step,
             result.value);
      break;
    case kGuestFault:
      printf("step=%s result=fault ec=0x%02" PRIx64
             " syndrome=0x%016" PRIx64 " ipa=0x%016" PRIx64 "\n",
             step, (result.syndrome >> 26) & UINT64_C(0x3f),
             result.syndrome, result.physical_address);
      break;
    case kGuestError:
      printf("step=%s result=error hv_return=%d\n", step, result.status);
      break;
  }
}

static int require_guest_value(const char *step, guest_result_t result,
                               uint64_t want) {
  print_guest_result(step, result);
  if (result.kind != kGuestValue || result.value != want) {
    fprintf(stderr, "%s: guest value mismatch, want 0x%016" PRIx64 "\n",
            step, want);
    return -1;
  }
  return 0;
}

static int print_page_disposition(const char *step, void *address) {
  integer_t disposition = 0;
  integer_t ref_count = 0;
  kern_return_t result = mach_vm_page_query(
      mach_task_self(), (mach_vm_address_t)(uintptr_t)address, &disposition,
      &ref_count);
  if (result != KERN_SUCCESS) {
    fprintf(stderr, "mach_vm_page_query(%s) failed: %d\n", step, result);
    return -1;
  }
  printf("step=%s result=page-disposition disposition=0x%x present=%d "
         "dirty=%d reusable=%d ref_count=%d\n",
         step, disposition, !!(disposition & VM_PAGE_QUERY_PAGE_PRESENT),
         !!(disposition & VM_PAGE_QUERY_PAGE_DIRTY),
         !!(disposition & VM_PAGE_QUERY_PAGE_REUSABLE), ref_count);
  return 0;
}

static int print_region_identity(const char *step, void *target) {
  mach_vm_address_t address = (mach_vm_address_t)(uintptr_t)target;
  natural_t depth = 0;
  for (;;) {
    mach_vm_size_t size = 0;
    vm_region_submap_info_data_64_t info;
    mach_msg_type_number_t count = VM_REGION_SUBMAP_INFO_COUNT_64;
    kern_return_t result = mach_vm_region_recurse(
        mach_task_self(), &address, &size, &depth,
        (vm_region_recurse_info_t)&info, &count);
    if (result != KERN_SUCCESS) {
      fprintf(stderr, "mach_vm_region_recurse(%s) failed: %d\n", step,
              result);
      return -1;
    }
    if (info.is_submap) {
      ++depth;
      continue;
    }
    if ((mach_vm_address_t)(uintptr_t)target < address ||
        (mach_vm_address_t)(uintptr_t)target - address >= size) {
      fprintf(stderr, "mach_vm_region_recurse(%s) returned wrong region\n",
              step);
      return -1;
    }
    uint64_t object_id =
        info.object_id_full != 0 ? info.object_id_full : info.object_id;
    printf("step=%s result=region address=0x%016" PRIx64
           " size=0x%016" PRIx64 " object_id=0x%016" PRIx64
           " object_offset=0x%016" PRIx64 " protection=0x%x\n",
           step, (uint64_t)address, (uint64_t)size, object_id,
           (uint64_t)info.offset, info.protection);
    return 0;
  }
}

static int replace_host_mapping(probe_vm_t *vm, bool separate_unmap) {
  if (vm->data_size != vm->page_size) {
    fprintf(stderr, "replacement requires one data page\n");
    return -1;
  }
  void *address = vm->data;
  if (separate_unmap && munmap(address, vm->page_size) != 0) {
    fprintf(stderr, "munmap(data before replacement): %s\n", strerror(errno));
    return -1;
  }
  void *replacement = map_anon_at(address, vm->page_size, true);
  if (replacement == NULL) {
    fprintf(stderr, "mmap(MAP_FIXED replacement): %s\n", strerror(errno));
    if (separate_unmap) {
      vm->data = NULL;
    }
    return -1;
  }
  vm->data = replacement;
  *(volatile uint64_t *)vm->data = kReplacement;
  return 0;
}

static int count_page_dispositions(probe_vm_t *vm, size_t *present,
                                   size_t *reusable) {
  *present = 0;
  *reusable = 0;
  for (size_t offset = 0; offset < vm->data_size; offset += vm->page_size) {
    integer_t disposition = 0;
    integer_t ref_count = 0;
    kern_return_t result = mach_vm_page_query(
        mach_task_self(),
        (mach_vm_address_t)(uintptr_t)((char *)vm->data + offset),
        &disposition, &ref_count);
    if (result != KERN_SUCCESS) {
      fprintf(stderr, "mach_vm_page_query(offset=%zu) failed: %d\n", offset,
              result);
      return -1;
    }
    if (disposition & VM_PAGE_QUERY_PAGE_PRESENT) {
      ++*present;
    }
    if (disposition & VM_PAGE_QUERY_PAGE_REUSABLE) {
      ++*reusable;
    }
  }
  return 0;
}

// run_reusable_pressure performs a bounded reclamation observation. It never
// accesses target pages after pressure: if XNU has reclaimed a page while the
// HVF mapping exists, touching the stale stage-2 translation would be unsafe.
static int run_reusable_pressure(bool keep_hvf_mapping) {
  const size_t target_size = (size_t)64 << 20;
  const size_t pressure_size = (size_t)2 << 30;
  probe_vm_t vm;
  if (probe_vm_init_sized(&vm, target_size) != 0) {
    probe_vm_destroy(&vm);
    return 1;
  }
  int failed = 0;
  const size_t pages = target_size / vm.page_size;
  if (keep_hvf_mapping) {
    for (size_t page = 0; page < pages; ++page) {
      guest_result_t result =
          guest_write_at(&vm, page * vm.page_size, kGuestWrite ^ page);
      if (result.kind != kGuestValue) {
        print_guest_result("pressure_guest_populate", result);
        failed = -1;
        break;
      }
    }
  } else {
    failed |= unmap_data_stage2(&vm);
    for (size_t page = 0; page < pages; ++page) {
      *(volatile uint64_t *)((char *)vm.data + page * vm.page_size) =
          kGuestWrite ^ page;
    }
  }
  printf("step=pressure_populate result=complete source=%s pages=%zu bytes=%zu\n",
         keep_hvf_mapping ? "guest" : "host-control", pages, target_size);
  if (madvise(vm.data, vm.data_size, MADV_FREE_REUSABLE) != 0) {
    fprintf(stderr, "madvise(MADV_FREE_REUSABLE pressure target): %s\n",
            strerror(errno));
    failed = -1;
  }
  size_t before_present = 0;
  size_t before_reusable = 0;
  failed |= count_page_dispositions(&vm, &before_present, &before_reusable);
  printf("step=before_pressure result=page-count present=%zu reusable=%zu "
         "total=%zu\n",
         before_present, before_reusable, pages);

  void *pressure = map_anon_at(NULL, pressure_size, false);
  if (pressure == NULL) {
    fprintf(stderr, "mmap(pressure): %s\n", strerror(errno));
    failed = -1;
  } else {
    for (size_t offset = 0; offset < pressure_size; offset += vm.page_size) {
      *(volatile uint8_t *)((char *)pressure + offset) = (uint8_t)(offset >> 14);
    }
  }
  size_t after_present = 0;
  size_t after_reusable = 0;
  failed |= count_page_dispositions(&vm, &after_present, &after_reusable);
  printf("step=after_pressure result=page-count present=%zu reusable=%zu "
         "total=%zu reclaimed=%zu pressure_bytes=%zu\n",
         after_present, after_reusable, pages, pages - after_present,
         pressure_size);

  // Break the secondary mapping before any host reuse or target access.
  failed |= unmap_data_stage2(&vm);
  if (madvise(vm.data, vm.data_size, MADV_FREE_REUSE) != 0) {
    fprintf(stderr, "madvise(MADV_FREE_REUSE pressure target): %s\n",
            strerror(errno));
    failed = -1;
  }
  if (pressure != NULL && munmap(pressure, pressure_size) != 0) {
    fprintf(stderr, "munmap(pressure): %s\n", strerror(errno));
    failed = -1;
  }
  failed |= probe_vm_destroy(&vm);
  return failed != 0;
}

static int run_baseline(void) {
  probe_vm_t vm;
  if (probe_vm_init(&vm) != 0) {
    probe_vm_destroy(&vm);
    return 1;
  }
  int failed = 0;
  failed |= require_guest_value("initial", guest_read(&vm), kInitial);
  *(volatile uint64_t *)vm.data = kReplacement;
  failed |= require_guest_value("host_write", guest_read(&vm), kReplacement);
  guest_result_t write_result = guest_write(&vm, kGuestWrite);
  print_guest_result("guest_write", write_result);
  uint64_t host_value = *(volatile uint64_t *)vm.data;
  printf("step=guest_write_host_read result=value value=0x%016" PRIx64 "\n",
         host_value);
  if (write_result.kind != kGuestValue || host_value != kGuestWrite) {
    failed = -1;
  }
  failed |= probe_vm_destroy(&vm);
  return failed != 0;
}

static int run_advice(int advice, const char *name) {
  probe_vm_t vm;
  if (probe_vm_init(&vm) != 0) {
    probe_vm_destroy(&vm);
    return 1;
  }
  int failed = 0;
  if (madvise(vm.data, vm.page_size, advice) != 0) {
    fprintf(stderr, "madvise(%s): %s\n", name, strerror(errno));
    failed = -1;
  }
  *(volatile uint64_t *)vm.data = kReplacement;
  failed |= require_guest_value(name, guest_read(&vm), kReplacement);
  failed |= probe_vm_destroy(&vm);
  return failed != 0;
}

static int run_reusable(bool touch_from_guest) {
  probe_vm_t vm;
  if (probe_vm_init(&vm) != 0) {
    probe_vm_destroy(&vm);
    return 1;
  }
  int failed = 0;
  uint64_t value_before_reusable = kInitial;
  if (touch_from_guest) {
    guest_result_t write_result = guest_write(&vm, kGuestWrite);
    print_guest_result("guest_write_before_reusable", write_result);
    if (write_result.kind != kGuestValue ||
        *(volatile uint64_t *)vm.data != kGuestWrite) {
      failed = -1;
    }
    value_before_reusable = kGuestWrite;
  }
  failed |= print_page_disposition("before_reusable", vm.data);
  if (madvise(vm.data, vm.page_size, MADV_FREE_REUSABLE) != 0) {
    fprintf(stderr, "madvise(MADV_FREE_REUSABLE): %s\n", strerror(errno));
    failed = -1;
  }
  failed |= print_page_disposition("after_reusable", vm.data);
  guest_result_t while_reusable = guest_read(&vm);
  print_guest_result("guest_read_while_reusable", while_reusable);
  if (while_reusable.kind == kGuestError ||
      (while_reusable.kind == kGuestValue &&
       while_reusable.value != value_before_reusable)) {
    failed = -1;
  }
  if (madvise(vm.data, vm.page_size, MADV_FREE_REUSE) != 0) {
    fprintf(stderr, "madvise(MADV_FREE_REUSE): %s\n", strerror(errno));
    failed = -1;
  }
  failed |= print_page_disposition("after_reuse", vm.data);
  *(volatile uint64_t *)vm.data = kReplacement;
  failed |= require_guest_value("host_write_after_reuse", guest_read(&vm),
                                kReplacement);
  failed |= probe_vm_destroy(&vm);
  return failed != 0;
}

static int run_replacement(bool separate_unmap, bool explicit_stage2_remap) {
  probe_vm_t vm;
  if (probe_vm_init(&vm) != 0) {
    probe_vm_destroy(&vm);
    return 1;
  }
  int failed = 0;
  failed |= print_region_identity("region_before_replacement", vm.data);
  if (explicit_stage2_remap && unmap_data_stage2(&vm) != 0) {
    failed = -1;
  }
  if (replace_host_mapping(&vm, separate_unmap) != 0) {
    failed = -1;
  }
  if (explicit_stage2_remap && map_data_stage2(&vm) != 0) {
    failed = -1;
  }
  failed |= print_region_identity("region_after_replacement", vm.data);
  const char *step = explicit_stage2_remap
                         ? "replacement_with_hvf_remap"
                         : (separate_unmap ? "munmap_then_map_fixed"
                                           : "map_fixed");
  guest_result_t result = guest_read(&vm);
  print_guest_result(step, result);
  uint64_t host_value = *(volatile uint64_t *)vm.data;
  printf("step=%s_host_read result=value value=0x%016" PRIx64 "\n", step,
         host_value);
  // The no-remap cases are observations, not conformance tests. They return
  // success for any non-error guest outcome so callers can collect the result
  // on OS versions with different undocumented behavior.
  if (result.kind == kGuestError ||
      (explicit_stage2_remap &&
       (result.kind != kGuestValue || result.value != kReplacement))) {
    failed = -1;
  }
  failed |= probe_vm_destroy(&vm);
  return failed != 0;
}

static void usage(const char *argv0) {
  fprintf(stderr,
          "usage: %s baseline|dontneed|reusable|map-fixed|unmap-remap|"
          "reusable-guest|reusable-pressure|reusable-pressure-host|"
          "explicit-remap\n",
          argv0);
}

int main(int argc, char **argv) {
  if (argc != 2) {
    usage(argv[0]);
    return 2;
  }
  printf("case=%s pid=%d page_size=%d\n", argv[1], getpid(), getpagesize());
  if (strcmp(argv[1], "baseline") == 0) {
    return run_baseline();
  }
  if (strcmp(argv[1], "dontneed") == 0) {
    return run_advice(MADV_DONTNEED, "madv_dontneed");
  }
  if (strcmp(argv[1], "reusable") == 0) {
    return run_reusable(false);
  }
  if (strcmp(argv[1], "reusable-guest") == 0) {
    return run_reusable(true);
  }
  if (strcmp(argv[1], "reusable-pressure") == 0) {
    return run_reusable_pressure(true);
  }
  if (strcmp(argv[1], "reusable-pressure-host") == 0) {
    return run_reusable_pressure(false);
  }
  if (strcmp(argv[1], "map-fixed") == 0) {
    return run_replacement(false, false);
  }
  if (strcmp(argv[1], "unmap-remap") == 0) {
    return run_replacement(true, false);
  }
  if (strcmp(argv[1], "explicit-remap") == 0) {
    return run_replacement(true, true);
  }
  usage(argv[0]);
  return 2;
}
