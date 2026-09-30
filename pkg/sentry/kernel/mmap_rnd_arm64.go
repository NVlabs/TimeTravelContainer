// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build arm64

package kernel

// The ranges of /proc/sys/vm/mmap_rnd_bits and mmap_rnd_compat_bits, as
// Linux's arm64 kernels with 4 KiB pages and 48-bit addresses have them.
const (
	MmapRndBitsMin       = 18
	MmapRndBitsMax       = 33
	MmapRndCompatBitsMin = 11
	MmapRndCompatBitsMax = 16
)
