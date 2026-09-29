// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package util

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// MirrorMapping duplicates a host mapping at destination without unmapping
// the source.
func MirrorMapping(source, length, destination uintptr) unix.Errno {
	_, _, errno := unix.RawSyscall6(unix.SYS_MREMAP, source, 0, length, linux.MREMAP_MAYMOVE|linux.MREMAP_FIXED, destination, 0)
	return errno
}
