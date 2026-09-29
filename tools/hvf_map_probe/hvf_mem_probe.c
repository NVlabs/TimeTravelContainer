/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// hvf_mem_probe characterizes the Hypervisor.framework memory primitives that
// a demand-paged HVF memory design relies on: lazy stage-2 population, file
// mappings, stage-2 faults, release, Mach memory entries, and map costs. Each
// case runs in its own process. Guest code runs at EL1 with stage 1 disabled,
// so every guest access is a raw stage-2 access to an IPA.
//
// DANGER: never extend this probe to map pages that the host executes, such
// as an executable's __TEXT, even through a second plain mmap of the file.
// Apple's Secure Page Table Monitor forbids such pages in a stage-2 table and
// panics the host rather than failing the call:
//
//   [SPTM] VIOLATION_ILLEGAL_MAPPING_TYPE: sptm_map_page(sptm.c:485) -
//   ... page_fte->type(0xe) ...
//
// (observed on macOS 26.5.2). hv_vm_map of dyld's own __TEXT address returns
// HV_ERROR instead; a second file mapping bypasses that check.
//
// As with hvf_map_probe, these are qualification observations, not API
// guarantees.

#include <Hypervisor/Hypervisor.h>
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <mach/mach_vm.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#if !defined(__APPLE__) || !defined(__aarch64__)
#error "hvf_mem_probe requires Apple Silicon macOS"
#endif

enum {
  kCodeIPA = 0x40000,
  kDataIPA = 0x10000000,
};

static const uint64_t kHostValue = UINT64_C(0x1111222233334444);
static const uint64_t kHostValue2 = UINT64_C(0xaaaabbbbccccdddd);
static const uint64_t kGuestValue = UINT64_C(0x123456789abcdef0);
static const uint64_t kFileTag = UINT64_C(0xf11e000000000000);

static size_t page_size;
static hv_vcpu_t vcpu;
static hv_vcpu_exit_t *vexit;
static int failures;

typedef struct result {
  bool value;  // exited through hvc #0
  uint64_t x1;
  uint64_t ec;
  uint64_t ipa;
  uint64_t pc;
} result_t;

// Guest code at kCodeIPA:
//   +0 read:  ldr x1, [x0]; hvc #0
//   +8 write: str x1, [x0]; hvc #0
static void setup(void) {
  page_size = (size_t)getpagesize();
  void *code = mmap(NULL, page_size, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANON, -1, 0);
  if (code == MAP_FAILED) {
    perror("mmap(code)");
    exit(1);
  }
  const uint32_t insns[] = {0xf9400001, 0xd4000002, 0xf9000001, 0xd4000002};
  memcpy(code, insns, sizeof(insns));
  if (hv_vm_create(NULL) != HV_SUCCESS ||
      hv_vm_map(code, kCodeIPA, page_size, HV_MEMORY_READ | HV_MEMORY_EXEC) !=
          HV_SUCCESS ||
      hv_vcpu_create(&vcpu, &vexit, NULL) != HV_SUCCESS) {
    fprintf(stderr, "HVF setup failed\n");
    exit(1);
  }
  uint64_t sctlr = 0;
  hv_vcpu_get_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, &sctlr);
  hv_vcpu_set_sys_reg(vcpu, HV_SYS_REG_SCTLR_EL1, sctlr & ~UINT64_C(1));
}

static result_t collect(void) {
  result_t r = {0};
  if (vexit->reason != HV_EXIT_REASON_EXCEPTION) {
    fprintf(stderr, "unexpected exit reason %d\n", vexit->reason);
    exit(1);
  }
  r.ec = (vexit->exception.syndrome >> 26) & 0x3f;
  r.ipa = vexit->exception.physical_address;
  hv_vcpu_get_reg(vcpu, HV_REG_PC, &r.pc);
  if (r.ec == 0x16) {
    r.value = true;
    hv_vcpu_get_reg(vcpu, HV_REG_X1, &r.x1);
  }
  return r;
}

static result_t run(uint64_t pc, uint64_t x0, uint64_t x1) {
  hv_vcpu_set_reg(vcpu, HV_REG_X0, x0);
  hv_vcpu_set_reg(vcpu, HV_REG_X1, x1);
  hv_vcpu_set_reg(vcpu, HV_REG_PC, pc);
  hv_vcpu_set_reg(vcpu, HV_REG_CPSR, 0x3c5);  // EL1h, DAIF masked
  if (hv_vcpu_run(vcpu) != HV_SUCCESS) {
    fprintf(stderr, "hv_vcpu_run failed\n");
    exit(1);
  }
  return collect();
}

// resume continues at the faulting PC without resetting registers.
static result_t resume(void) {
  if (hv_vcpu_run(vcpu) != HV_SUCCESS) {
    fprintf(stderr, "hv_vcpu_run(resume) failed\n");
    exit(1);
  }
  return collect();
}

static result_t gread(uint64_t ipa) { return run(kCodeIPA, ipa, 0); }
static result_t gwrite(uint64_t ipa, uint64_t v) {
  return run(kCodeIPA + 8, ipa, v);
}

static void describe(result_t r, char *buf, size_t n) {
  if (r.value) {
    snprintf(buf, n, "value=0x%016" PRIx64, r.x1);
  } else {
    snprintf(buf, n, "fault ec=0x%02" PRIx64 " ipa=0x%" PRIx64 " pc=0x%" PRIx64,
             r.ec, r.ipa, r.pc);
  }
}

static void guest_expect(const char *step, result_t r, uint64_t want) {
  char buf[128];
  describe(r, buf, sizeof buf);
  bool ok = r.value && r.x1 == want;
  failures += !ok;
  printf("  %-48s %s %s\n", step, buf, ok ? "OK" : "MISMATCH");
}

static void host_expect(const char *step, uint64_t got, uint64_t want) {
  bool ok = got == want;
  failures += !ok;
  printf("  %-48s value=0x%016" PRIx64 " %s\n", step, got,
         ok ? "OK" : "MISMATCH");
}

static uint64_t host_read(const void *p) { return *(volatile const uint64_t *)p; }
static void host_write(void *p, uint64_t v) { *(volatile uint64_t *)p = v; }

static uint64_t footprint(void) {
  task_vm_info_data_t info;
  mach_msg_type_number_t count = TASK_VM_INFO_COUNT;
  if (task_info(mach_task_self(), TASK_VM_INFO, (task_info_t)&info, &count) !=
      KERN_SUCCESS) {
    return 0;
  }
  return info.phys_footprint;
}

static uint64_t wired_bytes(void) {
  vm_statistics64_data_t st;
  mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
  host_statistics64(mach_host_self(), HOST_VM_INFO64, (host_info64_t)&st,
                    &count);
  return (uint64_t)st.wire_count * page_size;
}

static size_t resident_pages(void *p, size_t len) {
  size_t pages = len / page_size, n = 0;
  char *vec = malloc(pages);
  if (vec != NULL && mincore(p, len, vec) == 0) {
    for (size_t i = 0; i < pages; i++) n += (vec[i] & MINCORE_INCORE) != 0;
  }
  free(vec);
  return n;
}

static void report_mib(const char *step, int64_t delta) {
  printf("  %-48s %+.1f MiB\n", step, (double)delta / (1 << 20));
}

static double now_us(void) {
  static mach_timebase_info_data_t tb;
  if (tb.denom == 0) mach_timebase_info(&tb);
  return (double)mach_absolute_time() * tb.numer / tb.denom / 1000.0;
}

static char *anon(size_t len) {
  void *p = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1,
                 0);
  if (p == MAP_FAILED) {
    perror("mmap");
    exit(1);
  }
  return p;
}

static void map_or_die(void *host, uint64_t ipa, size_t len,
                       hv_memory_flags_t flags) {
  hv_return_t s = hv_vm_map(host, ipa, len, flags);
  if (s != HV_SUCCESS) {
    fprintf(stderr, "hv_vm_map failed: 0x%x\n", s);
    exit(1);
  }
}

// fresh: anonymous memory the host never touched before hv_vm_map. Mode 1
// reads a page from the host before mapping it; mode 2 right after.
static void case_fresh(int mode) {
  const size_t len = (size_t)64 << 20;
  char *p = anon(len);
  uint64_t f0 = footprint();
  if (mode == 1) host_expect("host read before hv_vm_map", host_read(p), 0);
  map_or_die(p, kDataIPA, len, HV_MEMORY_READ | HV_MEMORY_WRITE);
  report_mib("footprint after hv_vm_map of untouched 64MiB",
             (int64_t)(footprint() - f0));
  if (mode == 2) host_expect("host read after hv_vm_map", host_read(p), 0);
  guest_expect("guest read of untouched page", gread(kDataIPA), 0);
  gwrite(kDataIPA, kGuestValue);
  host_expect("host read after guest write", host_read(p), kGuestValue);
  host_write(p, kHostValue);
  guest_expect("guest read after host write", gread(kDataIPA), kHostValue);
  host_write(p + page_size, kHostValue2);
  guest_expect("guest read of a host-written page", gread(kDataIPA + page_size),
               kHostValue2);
  char *p2 = p + 2 * page_size;
  host_expect("host read of untouched page 2", host_read(p2), 0);
  gwrite(kDataIPA + 2 * page_size, kGuestValue);
  host_expect("host read after guest write (page 2)", host_read(p2),
              kGuestValue);
  host_write(p2, kHostValue);
  guest_expect("guest read after host write (page 2)",
               gread(kDataIPA + 2 * page_size), kHostValue);
  uint64_t f1 = footprint();
  for (size_t off = 4 * page_size; off < ((size_t)16 << 20); off += page_size) {
    gwrite(kDataIPA + off, off);
  }
  report_mib("footprint after the guest dirtied ~16MiB",
             (int64_t)(footprint() - f1));
  printf("  %-48s %zu of %zu\n", "host-resident pages (mincore)",
         resident_pages(p, len), len / page_size);
}

// data-file-*: mappings of an ordinary data file, which the host never
// executes. See the DANGER note above before mapping anything else.
static void case_data_file(bool shared, bool writable) {
  const size_t len = (size_t)64 << 20;
  const char *tmp = getenv("TMPDIR");
  char path[1024];
  snprintf(path, sizeof path, "%s/hvf-mem-probe-%d.dat",
           tmp != NULL ? tmp : "/tmp", getpid());
  int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);
  if (fd < 0) {
    perror("open");
    exit(1);
  }
  unlink(path);
  char *buf = malloc(page_size);
  for (size_t off = 0; off < len; off += page_size) {
    for (size_t i = 0; i < page_size; i += 8) {
      *(uint64_t *)(buf + i) = kFileTag | (off + i);
    }
    if (pwrite(fd, buf, page_size, (off_t)off) != (ssize_t)page_size) {
      perror("pwrite");
      exit(1);
    }
  }
  free(buf);
  fsync(fd);
  int prot = PROT_READ | (writable ? PROT_WRITE : 0);
  char *p = mmap(NULL, len, prot, shared ? MAP_SHARED : MAP_PRIVATE, fd, 0);
  if (p == MAP_FAILED) {
    perror("mmap(file)");
    exit(1);
  }
  uint64_t f0 = footprint();
  map_or_die(p, kDataIPA, len,
             HV_MEMORY_READ | (writable ? HV_MEMORY_WRITE : 0));
  size_t bad = 0;
  for (size_t off = 0; off < len; off += page_size) {
    result_t r = gread(kDataIPA + off);
    bad += !r.value || r.x1 != (kFileTag | off);
  }
  failures += bad != 0;
  printf("  %-48s %zu bad of %zu\n", "guest read of every file page", bad,
         len / page_size);
  report_mib("footprint after the guest read 64MiB of file",
             (int64_t)(footprint() - f0));
  if (!writable) return;
  host_write(p + page_size, kHostValue);
  guest_expect("guest read after host write (page 1)",
               gread(kDataIPA + page_size), kHostValue);
  gwrite(kDataIPA + 2 * page_size, kGuestValue);
  host_expect("host read after guest write (page 2)",
              host_read(p + 2 * page_size), kGuestValue);
  uint64_t v = 0;
  pread(fd, &v, sizeof v, (off_t)(2 * page_size));
  host_expect(shared ? "file sees the guest write (page 2)"
                     : "file keeps its data (private, page 2)",
              v, shared ? kGuestValue : (kFileTag | (2 * page_size)));
  if (!shared) {
    // Copy-on-write in both directions after the guest mapped the file page.
    guest_expect("guest read of file page 3", gread(kDataIPA + 3 * page_size),
                 kFileTag | (3 * page_size));
    host_write(p + 3 * page_size, kHostValue2);
    guest_expect("guest read after host COW write (page 3)",
                 gread(kDataIPA + 3 * page_size), kHostValue2);
    gwrite(kDataIPA + 4 * page_size, kGuestValue);
    host_expect("host read after guest COW write (page 4)",
                host_read(p + 4 * page_size), kGuestValue);
    return;
  }
  gwrite(kDataIPA + 5 * page_size, kGuestValue);
  struct fpunchhole ph = {.fp_flags = 0,
                          .reserved = 0,
                          .fp_offset = (off_t)(5 * page_size),
                          .fp_length = (off_t)page_size};
  int rc = fcntl(fd, F_PUNCHHOLE, &ph);
  printf("  %-48s %d\n", "F_PUNCHHOLE of page 5 while mapped", rc);
  guest_expect("guest read of the punched page", gread(kDataIPA + 5 * page_size),
               0);
  host_expect("host read of the punched page", host_read(p + 5 * page_size), 0);
}

// demand: stage-2 faults on unmapped IPAs and on read-only mappings.
static void case_demand(void) {
  char *p = anon(page_size);
  host_write(p, kHostValue);
  result_t r = gread(kDataIPA);
  char buf[128];
  describe(r, buf, sizeof buf);
  printf("  %-48s %s\n", "guest read of an unmapped IPA", buf);
  failures += r.value || r.ec != 0x24 || r.ipa != kDataIPA || r.pc != kCodeIPA;
  map_or_die(p, kDataIPA, page_size, HV_MEMORY_READ | HV_MEMORY_WRITE);
  guest_expect("resume at the faulting PC after mapping", resume(), kHostValue);
  hv_vm_protect(kDataIPA, page_size, HV_MEMORY_READ);
  r = gwrite(kDataIPA, kGuestValue);
  describe(r, buf, sizeof buf);
  printf("  %-48s %s\n", "guest write to a read-only mapping", buf);
  failures += r.value || r.ec != 0x24;
  hv_vm_protect(kDataIPA, page_size, HV_MEMORY_READ | HV_MEMORY_WRITE);
  r = resume();
  failures += !r.value;
  host_expect("host sees the write after hv_vm_protect", host_read(p),
              kGuestValue);
  const int n = 20000;
  double t0 = now_us();
  for (int i = 0; i < n; i++) {
    hv_vm_unmap(kDataIPA, page_size);
    if (gread(kDataIPA).value) {
      fprintf(stderr, "expected a stage-2 fault\n");
      exit(1);
    }
    hv_vm_map(p, kDataIPA, page_size, HV_MEMORY_READ | HV_MEMORY_WRITE);
    resume();
  }
  double t1 = now_us();
  for (int i = 0; i < n; i++) gread(kDataIPA);
  double t2 = now_us();
  printf("  %-48s %.2f us\n", "demand fault (unmap, fault, map, resume)",
         (t1 - t0) / n);
  printf("  %-48s %.2f us\n", "plain guest entry and exit", (t2 - t1) / n);
}

// release-*: freeing guest-dirtied memory with and without hv_vm_unmap.
static void case_release(const char *how) {
  const size_t len = (size_t)64 << 20;
  char *p = anon(len);
  map_or_die(p, kDataIPA, len, HV_MEMORY_READ | HV_MEMORY_WRITE);
  uint64_t w0 = wired_bytes(), f0 = footprint();
  for (size_t off = 0; off < len; off += page_size) {
    gwrite(kDataIPA + off, kGuestValue ^ off);
  }
  uint64_t f1 = footprint();
  report_mib("footprint after the guest dirtied 64MiB", (int64_t)(f1 - f0));
  report_mib("system wired change (approximate)",
             (int64_t)(wired_bytes() - w0));
  bool unmapped = strstr(how, "-unmapped") != NULL;
  if (unmapped) hv_vm_unmap(kDataIPA, len);
  if (strstr(how, "dealloc") != NULL) {
    mach_vm_address_t a = (mach_vm_address_t)(uintptr_t)p;
    kern_return_t kr = mach_vm_deallocate(mach_task_self(), a, len);
    if (kr == KERN_SUCCESS) {
      kr = mach_vm_allocate(mach_task_self(), &a, len, VM_FLAGS_FIXED);
    }
    printf("  %-48s %d\n", "mach_vm_deallocate, then allocate at it", kr);
  } else {
    int advice = strstr(how, "reusable") != NULL ? MADV_FREE_REUSABLE
                 : strstr(how, "dontneed") != NULL ? MADV_DONTNEED
                                                  : MADV_FREE;
    if (madvise(p, len, advice) != 0) perror("madvise");
  }
  report_mib("footprint change after the release",
             (int64_t)(footprint() - f1));
  printf("  %-48s %zu of %zu\n", "host-resident pages (mincore)",
         resident_pages(p, len), len / page_size);
  if (!unmapped) {
    // Still mapped: see whether the page kept its contents.
    char buf[128];
    result_t r = gread(kDataIPA + page_size);
    describe(r, buf, sizeof buf);
    printf("  %-48s %s (guest wrote 0x%016" PRIx64 ")\n",
           "guest read of a released page", buf, kGuestValue ^ page_size);
    printf("  %-48s value=0x%016" PRIx64 "\n", "host read of the same page",
           host_read(p + page_size));
    return;
  }
  if (strstr(how, "reusable") != NULL) madvise(p, len, MADV_FREE_REUSE);
  map_or_die(p, kDataIPA, len, HV_MEMORY_READ | HV_MEMORY_WRITE);
  host_write(p + page_size, kHostValue);
  guest_expect("remapped: guest read after host write",
               gread(kDataIPA + page_size), kHostValue);
}

// memory-entry: one named Mach memory object mapped at two host addresses;
// stage 2 maps only the first.
static void case_memory_entry(void) {
  const size_t len = (size_t)64 << 20;
  memory_object_size_t size = len;
  mach_port_t entry = MACH_PORT_NULL;
  kern_return_t kr = mach_make_memory_entry_64(
      mach_task_self(), &size, 0,
      MAP_MEM_NAMED_CREATE | VM_PROT_READ | VM_PROT_WRITE, &entry,
      MACH_PORT_NULL);
  mach_vm_address_t a = 0, b = 0;
  if (kr == KERN_SUCCESS) {
    kr = mach_vm_map(mach_task_self(), &a, len, 0, VM_FLAGS_ANYWHERE, entry, 0,
                     FALSE, VM_PROT_READ | VM_PROT_WRITE,
                     VM_PROT_READ | VM_PROT_WRITE, VM_INHERIT_SHARE);
  }
  if (kr == KERN_SUCCESS) {
    kr = mach_vm_map(mach_task_self(), &b, len, 0, VM_FLAGS_ANYWHERE, entry, 0,
                     FALSE, VM_PROT_READ | VM_PROT_WRITE,
                     VM_PROT_READ | VM_PROT_WRITE, VM_INHERIT_SHARE);
  }
  if (kr != KERN_SUCCESS) {
    fprintf(stderr, "memory entry setup failed: %d\n", kr);
    exit(1);
  }
  char *pa = (char *)(uintptr_t)a, *pb = (char *)(uintptr_t)b;
  uint64_t f0 = footprint();
  map_or_die(pa, kDataIPA, len, HV_MEMORY_READ | HV_MEMORY_WRITE);
  report_mib("footprint after hv_vm_map of the untouched entry",
             (int64_t)(footprint() - f0));
  host_write(pb, kHostValue);
  guest_expect("guest read (alias A) after host write (alias B)",
               gread(kDataIPA), kHostValue);
  gwrite(kDataIPA + page_size, kGuestValue);
  host_expect("host read (alias B) after guest write",
              host_read(pb + page_size), kGuestValue);
  uint64_t f1 = footprint();
  for (size_t off = 2 * page_size; off < ((size_t)32 << 20); off += page_size) {
    gwrite(kDataIPA + off, off);
  }
  report_mib("footprint after the guest dirtied ~32MiB",
             (int64_t)(footprint() - f1));
  uint64_t f2 = footprint();
  madvise(pb, len, MADV_FREE_REUSABLE);
  report_mib("footprint change after REUSABLE through alias B",
             (int64_t)(footprint() - f2));
  char buf[128];
  describe(gread(kDataIPA + 2 * page_size), buf, sizeof buf);
  printf("  %-48s %s\n", "guest read after REUSABLE", buf);
}

// map-cost: hv_vm_map and hv_vm_unmap per 16K slab, and for one large range.
static void case_map_cost(void) {
  const size_t n = 32768;
  char *p = anon(n * page_size);
  double t0 = now_us();
  for (size_t i = 0; i < n; i++) {
    map_or_die(p + i * page_size, kDataIPA + i * page_size, page_size,
               HV_MEMORY_READ | HV_MEMORY_WRITE);
  }
  double t1 = now_us();
  for (size_t i = 0; i < n; i++) hv_vm_unmap(kDataIPA + i * page_size, page_size);
  double t2 = now_us();
  map_or_die(p, kDataIPA, n * page_size, HV_MEMORY_READ | HV_MEMORY_WRITE);
  double t3 = now_us();
  hv_vm_unmap(kDataIPA, n * page_size);
  double t4 = now_us();
  printf("  %-48s %.2f us\n", "hv_vm_map of one 16K slab (32768 of them)",
         (t1 - t0) / n);
  printf("  %-48s %.2f us\n", "hv_vm_unmap of one 16K slab", (t2 - t1) / n);
  printf("  %-48s %.1f us\n", "hv_vm_map of one 512MiB range", t3 - t2);
  printf("  %-48s %.1f us\n", "hv_vm_unmap of one 512MiB range", t4 - t3);
}

static void usage(const char *argv0) {
  fprintf(stderr,
          "usage: %s fresh|fresh-host-read|fresh-map-host-read|"
          "data-file-private-ro|data-file-shared-ro|data-file-private-rw|"
          "data-file-shared-rw|demand|release-reusable|release-dontneed|"
          "release-free|release-reusable-unmapped|release-dontneed-unmapped|"
          "release-dealloc-unmapped|memory-entry|map-cost\n",
          argv0);
}

int main(int argc, char **argv) {
  if (argc != 2) {
    usage(argv[0]);
    return 2;
  }
  const char *c = argv[1];
  setup();
  printf("case=%s pid=%d page_size=%zu\n", c, getpid(), page_size);
  if (page_size != 16384) {
    fprintf(stderr, "host page size is %zu, want 16384\n", page_size);
    return 1;
  }
  if (strcmp(c, "fresh") == 0) {
    case_fresh(0);
  } else if (strcmp(c, "fresh-host-read") == 0) {
    case_fresh(1);
  } else if (strcmp(c, "fresh-map-host-read") == 0) {
    case_fresh(2);
  } else if (strncmp(c, "data-file-", 10) == 0) {
    case_data_file(strstr(c, "shared") != NULL, strstr(c, "-rw") != NULL);
  } else if (strcmp(c, "demand") == 0) {
    case_demand();
  } else if (strncmp(c, "release-", 8) == 0) {
    case_release(c);
  } else if (strcmp(c, "memory-entry") == 0) {
    case_memory_entry();
  } else if (strcmp(c, "map-cost") == 0) {
    case_map_cost();
  } else {
    usage(argv[0]);
    return 2;
  }
  return failures != 0;
}
