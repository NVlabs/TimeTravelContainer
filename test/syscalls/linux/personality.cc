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

#include <sys/personality.h>
#include <sys/auxv.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#include <array>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstdint>
#include <cstring>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "test/util/cleanup.h"
#include "test/util/fs_util.h"
#include "test/util/logging.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {

namespace {

constexpr uint64_t kGetPersonality = 0xffffffff;
constexpr char kAddressReportFD[] = "GVISOR_PERSONALITY_ADDRESS_REPORT_FD";

__attribute__((noinline)) void AddressProbeFunction() {}

struct AddressReport {
  std::array<uintptr_t, 7> addresses;
  std::array<unsigned char, 16> random;
};

TEST(PersonalityTest, DefaultPersonality) {
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
}

TEST(PersonalityTest, SetLinux) {
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(PER_LINUX) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) == PER_LINUX);
              }),
              IsPosixErrorOkAndHolds(0));
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
}

TEST(PersonalityTest, SetBSD) {
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(PER_BSD) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) == PER_BSD);
              }),
              IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, PersonalityIsInheritable) {
  EXPECT_THAT(
      InForkedProcess([&] {
        TEST_CHECK(personality(PER_BSD) == PER_LINUX);
        TEST_CHECK(personality(kGetPersonality) == PER_BSD);
        TEST_CHECK(
            InForkedProcess([&] {
              TEST_CHECK(personality(kGetPersonality) == PER_BSD);
              TEST_CHECK(InForkedProcess([&] {
                           TEST_CHECK(personality(kGetPersonality) == PER_BSD);
                         }).ok());
              TEST_CHECK(personality(PER_HPUX) == PER_BSD);
              TEST_CHECK(InForkedProcess([&] {
                           TEST_CHECK(personality(kGetPersonality) == PER_HPUX);
                         }).ok());
              TEST_CHECK(personality(PER_BSD) == PER_HPUX);
              TEST_CHECK(InForkedProcess([&] {
                           TEST_CHECK(personality(kGetPersonality) == PER_BSD);
                         }).ok());
              TEST_CHECK(personality(kGetPersonality) == PER_BSD);
            }).ok());
      }),
      IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, ChildPersonalityDoesNotAffectParent) {
  EXPECT_THAT(
      InForkedProcess([&] {
        TEST_CHECK(personality(PER_BSD) == PER_LINUX);
        TEST_CHECK(personality(kGetPersonality) == PER_BSD);
        TEST_CHECK(InForkedProcess([&] {
                     TEST_CHECK(personality(PER_HPUX) == PER_BSD);
                     TEST_CHECK(personality(kGetPersonality) == PER_HPUX);
                   }).ok());
        TEST_CHECK(personality(kGetPersonality) == PER_BSD);
      }),
      IsPosixErrorOkAndHolds(0));
  TEST_CHECK(personality(kGetPersonality) == PER_LINUX);
}

TEST(PersonalityTest, SetShortInode) {
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(SHORT_INODE) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) == SHORT_INODE);
              }),
              IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, SetWholeSeconds) {
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(WHOLE_SECONDS) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) == WHOLE_SECONDS);
              }),
              IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, SetAddrNoRandomize) {
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(ADDR_NO_RANDOMIZE) == PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) ==
                           ADDR_NO_RANDOMIZE);
                TEST_CHECK(personality(PER_LINUX) == ADDR_NO_RANDOMIZE);
              }),
              IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, ReportAddresses) {
  const char* fd_string = getenv(kAddressReportFD);
  if (fd_string == nullptr) {
    GTEST_SKIP();
  }
  const int fd = atoi(fd_string);
  int stack;
  void* heap = malloc(1);
  ASSERT_NE(heap, nullptr);
  void* mapping = mmap(nullptr, 4096, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  ASSERT_NE(mapping, MAP_FAILED);
  AddressReport report = {};
  report.addresses = {
      reinterpret_cast<uintptr_t>(&AddressProbeFunction),
      reinterpret_cast<uintptr_t>(&stack),
      reinterpret_cast<uintptr_t>(heap),
      reinterpret_cast<uintptr_t>(mapping),
      static_cast<uintptr_t>(getauxval(AT_SYSINFO_EHDR)),
      static_cast<uintptr_t>(getauxval(AT_RANDOM)),
      0,
  };
  const std::string maps =
      ASSERT_NO_ERRNO_AND_VALUE(GetContents("/proc/self/maps"));
  const size_t marker = maps.find("[usertrap]");
  if (marker != std::string::npos) {
    const size_t line_start = maps.rfind('\n', marker);
    const char* start = maps.c_str() +
                        (line_start == std::string::npos ? 0 : line_start + 1);
    ASSERT_EQ(sscanf(start, "%lx", &report.addresses[6]), 1);
  }
  memcpy(report.random.data(),
         reinterpret_cast<const void*>(getauxval(AT_RANDOM)),
         report.random.size());
  ASSERT_EQ(write(fd, &report, sizeof(report)),
            static_cast<ssize_t>(sizeof(report)));
  ASSERT_EQ(munmap(mapping, 4096), 0);
  free(heap);
}

TEST(PersonalityTest, AddrNoRandomizeMakesExecLayoutDeterministic) {
  SKIP_IF(!IsRunningOnGvisor());
  std::array<AddressReport, 2> results;
  for (auto& result : results) {
    int pipe_fds[2];
    ASSERT_THAT(pipe(pipe_fds), SyscallSucceeds());
    const pid_t child = fork();
    ASSERT_THAT(child, SyscallSucceeds());
    if (child == 0) {
      close(pipe_fds[0]);
      char fd_string[32];
      snprintf(fd_string, sizeof(fd_string), "%d", pipe_fds[1]);
      TEST_PCHECK(setenv(kAddressReportFD, fd_string, 1) == 0);
      TEST_PCHECK(personality(ADDR_NO_RANDOMIZE) != -1);
      execl("/proc/self/exe", "/proc/self/exe",
            "--gtest_filter=PersonalityTest.ReportAddresses", nullptr);
      _exit(127);
    }
    close(pipe_fds[1]);
    size_t done = 0;
    while (done != sizeof(result)) {
      const ssize_t n = read(pipe_fds[0],
                             reinterpret_cast<char*>(&result) + done,
                             sizeof(result) - done);
      ASSERT_GT(n, 0);
      done += static_cast<size_t>(n);
    }
    close(pipe_fds[0]);
    int status;
    ASSERT_THAT(waitpid(child, &status, 0), SyscallSucceedsWithValue(child));
    ASSERT_TRUE(WIFEXITED(status));
    ASSERT_EQ(WEXITSTATUS(status), 0);
  }
  EXPECT_EQ(results[0].addresses, results[1].addresses);
  EXPECT_NE(results[0].random, results[1].random);
}

TEST(PersonalityTest, RandomizeVaSpaceZeroMakesExecLayoutDeterministic) {
  SKIP_IF(!IsRunningOnGvisor());
  constexpr char kPath[] = "/proc/sys/kernel/randomize_va_space";
  const std::string original = ASSERT_NO_ERRNO_AND_VALUE(GetContents(kPath));
  auto restore = Cleanup([original] {
    SetContents("/proc/sys/kernel/randomize_va_space", original);
  });
  ASSERT_NO_ERRNO(SetContents(kPath, "0"));

  std::array<AddressReport, 2> results;
  for (auto& result : results) {
    int pipe_fds[2];
    ASSERT_THAT(pipe(pipe_fds), SyscallSucceeds());
    const pid_t child = fork();
    ASSERT_THAT(child, SyscallSucceeds());
    if (child == 0) {
      close(pipe_fds[0]);
      char fd_string[32];
      snprintf(fd_string, sizeof(fd_string), "%d", pipe_fds[1]);
      TEST_PCHECK(setenv(kAddressReportFD, fd_string, 1) == 0);
      execl("/proc/self/exe", "/proc/self/exe",
            "--gtest_filter=PersonalityTest.ReportAddresses", nullptr);
      _exit(127);
    }
    close(pipe_fds[1]);
    size_t done = 0;
    while (done != sizeof(result)) {
      const ssize_t n = read(pipe_fds[0],
                             reinterpret_cast<char*>(&result) + done,
                             sizeof(result) - done);
      ASSERT_GT(n, 0);
      done += static_cast<size_t>(n);
    }
    close(pipe_fds[0]);
    int status;
    ASSERT_THAT(waitpid(child, &status, 0), SyscallSucceedsWithValue(child));
    ASSERT_TRUE(WIFEXITED(status));
    ASSERT_EQ(WEXITSTATUS(status), 0);
  }
  EXPECT_EQ(results[0].addresses, results[1].addresses);
  EXPECT_NE(results[0].random, results[1].random);
}

TEST(PersonalityTest, SetMultiple) {
  EXPECT_THAT(InForkedProcess([&] {
                TEST_CHECK(personality(PER_BSD | SHORT_INODE | WHOLE_SECONDS) ==
                           PER_LINUX);
                TEST_CHECK(personality(kGetPersonality) ==
                           (PER_BSD | SHORT_INODE | WHOLE_SECONDS));
              }),
              IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, SetThenUnset) {
  EXPECT_THAT(
      InForkedProcess([&] {
        TEST_CHECK(personality(PER_BSD | SHORT_INODE) == PER_LINUX);
        TEST_CHECK(personality(WHOLE_SECONDS | SHORT_INODE) ==
                   (PER_BSD | SHORT_INODE));
        TEST_CHECK(personality(PER_BSD) == (WHOLE_SECONDS | SHORT_INODE));
        TEST_CHECK(personality(kGetPersonality) == PER_BSD);
      }),
      IsPosixErrorOkAndHolds(0));
}

TEST(PersonalityTest, UnsupportedPersonalityBitsAreRejected) {
  SKIP_IF(!IsRunningOnGvisor());
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
  EXPECT_THAT(personality(MMAP_PAGE_ZERO), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(ADDR_LIMIT_3GB), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(ADDR_LIMIT_32BIT), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(ADDR_COMPAT_LAYOUT), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(READ_IMPLIES_EXEC), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(STICKY_TIMEOUTS), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(UNAME26), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(FDPIC_FUNCPTRS), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_LINUX32), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_LINUX32_3GB), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_LINUX_32BIT), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_LINUX_FDPIC), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_RISCOS), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(PER_SOLARIS), SyscallFailsWithErrno(EINVAL));
  EXPECT_THAT(personality(kGetPersonality),
              SyscallSucceedsWithValue(PER_LINUX));
}

}  // namespace

}  // namespace testing
}  // namespace gvisor
