// Copyright 2018 The gVisor Authors.
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

#include <asm/prctl.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include <atomic>
#include <thread>  // NOLINT
#include <vector>

#include <cerrno>
#include <cstdint>
#include <string>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "test/util/fs_util.h"
#include "test/util/posix_error.h"
#include "test/util/test_util.h"

// glibc does not provide a prototype for arch_prctl() so declare it here.
extern "C" int arch_prctl(int code, uintptr_t addr);

namespace gvisor {
namespace testing {

namespace {

TEST(ArchPrctlTest, GetSetFS) {
  uintptr_t orig;
  const uintptr_t kNonCanonicalFsbase = 0x4141414142424242;

  // Get the original FS.base and then set it to the same value (this is
  // intentional because FS.base is the TLS pointer so we cannot change it
  // arbitrarily).
  ASSERT_THAT(arch_prctl(ARCH_GET_FS, reinterpret_cast<uintptr_t>(&orig)),
              SyscallSucceeds());
  ASSERT_THAT(arch_prctl(ARCH_SET_FS, orig), SyscallSucceeds());

  // Trying to set FS.base to a non-canonical value should return an error.
  ASSERT_THAT(arch_prctl(ARCH_SET_FS, kNonCanonicalFsbase),
              SyscallFailsWithErrno(EPERM));
}

TEST(ArchPrctlTest, GetSetGS) {
  uintptr_t orig;
  const int get_result =
      arch_prctl(ARCH_GET_GS, reinterpret_cast<uintptr_t>(&orig));
  if (get_result == -1 && errno == EINVAL) {
    GTEST_SKIP() << "execution platform reserves GS";
  }
  ASSERT_THAT(get_result, SyscallSucceeds());

  uint64_t value = 0x0123456789abcdef;
  ASSERT_THAT(arch_prctl(ARCH_SET_GS, reinterpret_cast<uintptr_t>(&value)),
              SyscallSucceeds());

  uint64_t loaded;
  asm volatile("movq %%gs:0, %0" : "=r"(loaded));
  EXPECT_EQ(loaded, value);

  uintptr_t current;
  EXPECT_THAT(arch_prctl(ARCH_GET_GS,
                         reinterpret_cast<uintptr_t>(&current)),
              SyscallSucceeds());
  EXPECT_EQ(current, reinterpret_cast<uintptr_t>(&value));

  ASSERT_THAT(arch_prctl(ARCH_SET_GS, orig), SyscallSucceeds());
}

// PatchableGetppid calls getppid with the instruction sequence that systrap
// patches: mov $SYS_getppid, %eax; syscall.
long PatchableGetppid() {
  long ret;
  asm volatile("mov %1, %%eax\n\tsyscall"
               : "=a"(ret)
               : "i"(SYS_getppid)
               : "rcx", "r11", "memory");
  return ret;
}

// GSRound has threads make patched system calls, sets GS, which ends systrap's
// syscall patching for the address space, and has some of the threads then
// take a page fault at each of many pages with an index in RCX. It returns 0
// if every fault left RCX and R11 as they were.
int GSRound() {
  constexpr int kWorkers = 16;
  constexpr int kVictims = 8;
  constexpr int kPages = 2000;
  std::atomic<int> phase(0);
  std::atomic<int> result(0);
  std::vector<std::thread> threads;
  for (int i = 0; i < kWorkers; i++) {
    threads.emplace_back([&] {
      while (phase.load() == 0) {
        PatchableGetppid();
      }
    });
  }
  for (int i = 0; i < kVictims; i++) {
    threads.emplace_back([&] {
      while (phase.load() == 0) {
        PatchableGetppid();
      }
      const size_t len = kPages * kPageSize;
      char* base = static_cast<char*>(mmap(nullptr, len, PROT_READ | PROT_WRITE,
                                           MAP_PRIVATE | MAP_ANONYMOUS, -1, 0));
      if (base == MAP_FAILED) {
        result.store(2);
        return;
      }
      for (int p = 0; p < kPages; p++) {
        uint64_t rcx, r11;
        // The first write to each page faults, and is retried with the
        // registers that the thread resumes with.
        asm volatile(
            "xor %%ecx, %%ecx\n\t"
            "mov $0x1234, %%r11d\n\t"
            "movb $1, (%[page],%%rcx,1)\n\t"
            "mov %%rcx, %[rcx]\n\t"
            "mov %%r11, %[r11]\n\t"
            : [rcx] "=r"(rcx), [r11] "=r"(r11)
            : [page] "r"(base + p * kPageSize)
            : "rcx", "r11", "memory");
        if (rcx != 0 || r11 != 0x1234) {
          result.store(1);
          break;
        }
      }
      munmap(base, len);
    });
  }
  struct timespec ts = {.tv_sec = 0, .tv_nsec = 100 * 1000 * 1000};
  nanosleep(&ts, nullptr);
  uint64_t value = 0;
  if (arch_prctl(ARCH_SET_GS, reinterpret_cast<uintptr_t>(&value)) != 0) {
    result.store(3);
  }
  phase.store(1);
  for (auto& t : threads) {
    t.join();
  }
  return result.load();
}

// A thread that owns GS resumes from a fault with its registers intact while
// other threads make system calls that systrap patches. A systrap stub thread
// that entered the fast system call handler before GS was set resumes
// whichever context is ready, not only one that returns from a system call.
TEST(ArchPrctlTest, GSOwnerRegistersSurviveFaults) {
  uintptr_t orig;
  if (arch_prctl(ARCH_GET_GS, reinterpret_cast<uintptr_t>(&orig)) == -1 &&
      errno == EINVAL) {
    GTEST_SKIP() << "execution platform reserves GS";
  }
  // Each round sets GS in a child of its own, whose address space has syscall
  // patching enabled until then. A round finds the race often, not always.
  // The test finds nothing in a process that set GS before it, as
  // GetSetGS does, whose children have no patching to end.
  for (int round = 0; round < 20; round++) {
    pid_t pid = fork();
    if (pid == 0) {
      _exit(GSRound());
    }
    ASSERT_THAT(pid, SyscallSucceeds());
    int status;
    ASSERT_THAT(RetryEINTR(waitpid)(pid, &status, 0),
                SyscallSucceedsWithValue(pid));
    ASSERT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0)
        << "round " << round << ": status " << status;
  }
}

// Tests that /proc/cpuinfo advertised to the workload has FSGSBASE disabled
// under Systrap, so workloads do not execute WRGSBASE directly.
TEST(ArchPrctlTest, FSGSBaseDisabledInCpuInfo) {
  SKIP_IF(GvisorPlatform() != Platform::kSystrap);
  std::string cpuinfo;
  ASSERT_NO_ERRNO(GetContents("/proc/cpuinfo", &cpuinfo));
  EXPECT_THAT(cpuinfo,
              ::testing::Not(::testing::ContainsRegex(R"(\bfsgsbase\b)")))
      << "gVisor must not advertise fsgsbase in /proc/cpuinfo under systrap";
}

}  // namespace

}  // namespace testing
}  // namespace gvisor
