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

// reserveAddressRange reserves an address range, of no memory, for a file's
// chunk mappings (see MemoryFile.mapChunksLocked): the largest of the sizes it
// tries, from maxMappingReservation down to minMappingReservation, that the
// host has room for, aligned to huge pages, so that the file's huge pages map
// as huge pages there. It returns a zero length if none fits.
func reserveAddressRange() (base, length uintptr) {
	for length := uintptr(maxMappingReservation); length >= minMappingReservation; length >>= 1 {
		addr, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, length+hostarch.HugePageSize, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE, ^uintptr(0) /* fd */, 0)
		if errno != 0 {
			continue
		}
		base := hostarch.Addr(addr).MustHugeRoundUp()
		// What the alignment leaves at either end is not the range's.
		if head := uintptr(base) - addr; head != 0 {
			unix.Syscall(unix.SYS_MUNMAP, addr, head, 0)
		}
		if tail := hostarch.HugePageSize - (uintptr(base) - addr); tail != 0 {
			unix.Syscall(unix.SYS_MUNMAP, uintptr(base)+length, tail, 0)
		}
		return uintptr(base), length
	}
	return 0, 0
}

// releaseAddressRange releases the address range at base of length bytes, and
// whatever is mapped in it.
func releaseAddressRange(base, length uintptr) {
	if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, base, length, 0); errno != 0 {
		log.Warningf("Failed to release the address range %#x of %d bytes: %v", base, length, errno)
	}
}

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
