// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package config

import "gvisor.dev/gvisor/pkg/seccomp"

// Darwin runs only in explicitly requested trusted mode and installs no
// seccomp program. Keep the rule-construction API present so common loader
// code and build-time tooling do not acquire host-OS conditionals.
var allowedSyscalls = seccomp.NewSyscallRules()

func controlServerFilters(fd uint32) seccomp.SyscallRules { return seccomp.NewSyscallRules() }
func selfPIDFilters(pid uint64) seccomp.SyscallRules      { return seccomp.NewSyscallRules() }
func hostFilesystemFilters() seccomp.SyscallRules         { return seccomp.NewSyscallRules() }
func ttcVolumeFilters() seccomp.SyscallRules              { return seccomp.NewSyscallRules() }
func cgoFilters() seccomp.SyscallRules                    { return seccomp.NewSyscallRules() }
func profileFilters() seccomp.SyscallRules                { return seccomp.NewSyscallRules() }
func hostInetFilters(allowRawSockets bool) seccomp.SyscallRules {
	return seccomp.NewSyscallRules()
}
func timeTravelNATFilters() seccomp.SyscallRules { return seccomp.NewSyscallRules() }

func archFstatAtSysNo() uintptr          { return 0 }
func archSpecificHotSyscalls() []uintptr { return nil }
func hostHotSyscalls() []uintptr         { return nil }
