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

#include <asm/prctl.h>
#include <sys/prctl.h>

#include "gtest/gtest.h"
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

}  // namespace

}  // namespace testing
}  // namespace gvisor
