// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fsutil

import "golang.org/x/sys/unix"

func mmapFileAtExact(_ int, _, _ uint64, _ int) (uintptr, unix.Errno) {
	// Darwin has MAP_FIXED, which destructively replaces mappings, but no
	// MAP_FIXED_NOREPLACE. Exact-offset host mappings are a Linux platform
	// optimization and are not used by the HVF/GuestRAM data plane.
	return 0, unix.EOPNOTSUPP
}
