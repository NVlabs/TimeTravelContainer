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

package config

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/seccomp"
)

// timeTravelNATFilters permits the socket forms used by Go's net package for
// TTC's Sentry-local user-mode TCP/UDP proxy. hostInetFilters already permits
// the subsequent operations and equivalent sockets with explicit IPPROTO_*;
// Go intentionally passes protocol 0 and lets the host kernel select it.
func timeTravelNATFilters() seccomp.SyscallRules {
	flags := uintptr(unix.SOCK_NONBLOCK | unix.SOCK_CLOEXEC)
	return seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_SOCKET: seccomp.Or{
			seccomp.PerArg{seccomp.EqualTo(unix.AF_INET), seccomp.EqualTo(uintptr(unix.SOCK_STREAM) | flags), seccomp.EqualTo(0)},
			seccomp.PerArg{seccomp.EqualTo(unix.AF_INET6), seccomp.EqualTo(uintptr(unix.SOCK_STREAM) | flags), seccomp.EqualTo(0)},
			seccomp.PerArg{seccomp.EqualTo(unix.AF_INET), seccomp.EqualTo(uintptr(unix.SOCK_DGRAM) | flags), seccomp.EqualTo(0)},
			seccomp.PerArg{seccomp.EqualTo(unix.AF_INET6), seccomp.EqualTo(uintptr(unix.SOCK_DGRAM) | flags), seccomp.EqualTo(0)},
		},
	})
}
