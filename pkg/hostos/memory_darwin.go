// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostos

import "golang.org/x/sys/unix"

// TotalSystemMemory returns the physical memory installed in the Mac. Runtime
// guest limits are enforced separately by Sentry and GuestRAM.
func TotalSystemMemory() (uint64, error) { return unix.SysctlUint64("hw.memsize") }
