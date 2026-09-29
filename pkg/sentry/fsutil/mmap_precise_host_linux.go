// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsutil

import "golang.org/x/sys/unix"

func mmapFileAtExact(fd int, offset, length uint64, prot int) (uintptr, unix.Errno) {
	addr, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		uintptr(offset),
		uintptr(length),
		uintptr(prot),
		unix.MAP_SHARED|unix.MAP_FIXED_NOREPLACE,
		uintptr(fd),
		uintptr(offset))
	if errno == 0 && uint64(addr) != offset {
		// The host kernel predates MAP_FIXED_NOREPLACE and the requested
		// address conflicts with an existing mapping.
		errno = unix.EEXIST
		unix.RawSyscall(unix.SYS_MUNMAP, addr, uintptr(length), 0)
	}
	return addr, errno
}
