// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package rdmaproxy

import "gvisor.dev/gvisor/pkg/seccomp"

// Filters is empty because RDMA proxying is unsupported in Darwin trusted
// mode, which does not install seccomp filters.
func Filters() seccomp.SyscallRules { return seccomp.NewSyscallRules() }
