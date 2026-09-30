// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64

package kernel

// The ranges of /proc/sys/vm/mmap_rnd_bits and mmap_rnd_compat_bits, as
// Linux's x86-64 kernels have them.
const (
	MmapRndBitsMin       = 28
	MmapRndBitsMax       = 32
	MmapRndCompatBitsMin = 8
	MmapRndCompatBitsMax = 16
)
