// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

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
