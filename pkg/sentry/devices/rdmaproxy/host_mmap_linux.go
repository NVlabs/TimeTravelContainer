// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package rdmaproxy

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func mirrorMapping(source, length, destination uintptr) unix.Errno {
	_, _, errno := unix.RawSyscall6(unix.SYS_MREMAP, source, 0, length, linux.MREMAP_MAYMOVE|linux.MREMAP_FIXED, destination, 0)
	return errno
}

func advisePopulateWrite(address, length uintptr) unix.Errno {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, address, length, unix.MADV_POPULATE_WRITE)
	return errno
}
