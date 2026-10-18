// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package pgalloc

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// populateHugeFileRange commits fr of the shared memory file fd, whole 2 MiB
// pages, through a mapping of fr alone, advised MADV_HUGEPAGE if adviseHuge,
// so that the host backs it with huge pages where the Sentry's own mapping of
// fr would get small ones; see MemoryFile.AllocateAt. The host aligns a shared
// memory mapping to huge pages.
func populateHugeFileRange(fd uintptr, fr memmap.FileRange, adviseHuge bool) error {
	length := uintptr(fr.Length())
	addr, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, fd, uintptr(fr.Start))
	if errno != 0 {
		return errno
	}
	defer func() {
		if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, addr, length, 0); errno != 0 {
			log.Warningf("Failed to unmap mapping %#x of MemoryFile range %v: %v", addr, fr, errno)
		}
	}()
	if adviseHuge {
		if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, unix.MADV_HUGEPAGE); errno != 0 {
			log.Warningf("madvise(%#x, %d, MADV_HUGEPAGE) failed: %s", addr, length, errno)
		}
	}
	mapped := mappingSlice(addr, length)
	if tryPopulateMadv(safemem.BlockFromSafeSlice(mapped)) {
		return nil
	}
	// Touch each huge page instead.
	for off := 0; off < len(mapped); off += hostarch.HugePageSize {
		mapped[off] = 0
	}
	return nil
}
