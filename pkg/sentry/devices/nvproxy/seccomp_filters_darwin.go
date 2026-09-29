// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package nvproxy

import (
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"
)

// Filters is empty because Darwin trusted mode installs no seccomp policy and
// nvproxy itself is unsupported on this host.
func Filters(nvconf.DriverCaps) seccomp.SyscallRules { return seccomp.NewSyscallRules() }
