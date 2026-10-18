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

// collapseFileRange has the host back fr of the shared memory file fd, one
// aligned 2 MiB range, with a huge page, copying its base pages into one, as
// its khugepaged does: MADV_COLLAPSE, through a mapping of fr alone at an
// aligned address, as the host collapses only what it may map as a huge page
// there. The mapping is touched first: Linux 6.1 maps the huge page in the
// mapping only if that has a page table, and fails the call otherwise, having
// collapsed the range all the same. It returns errNoHostCollapse if the host
// has no MADV_COLLAPSE (before Linux 6.1).
func collapseFileRange(fd uintptr, fr memmap.FileRange) error {
	length := uintptr(fr.Length())
	// Room for the mapping at an aligned address.
	room, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, 2*length, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE, ^uintptr(0) /* fd */, 0)
	if errno != 0 {
		return errno
	}
	defer func() {
		if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, room, 2*length, 0); errno != 0 {
			log.Warningf("Failed to unmap mapping %#x of MemoryFile range %v: %v", room, fr, errno)
		}
	}()
	addr := uintptr(hostarch.Addr(room).MustHugeRoundUp())
	if _, _, errno := unix.Syscall6(unix.SYS_MMAP, addr, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED, fd, uintptr(fr.Start)); errno != 0 {
		return errno
	}
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, hostarch.PageSize, unix.MADV_POPULATE_READ); errno != 0 {
		return collapseError(errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, unix.MADV_COLLAPSE); errno != 0 {
		return collapseError(errno)
	}
	return nil
}

// collapseError returns errno, the error of a call of collapseFileRange, or
// errNoHostCollapse if the host has no MADV_COLLAPSE: the advice, for no
// range, succeeds on a host that has it.
func collapseError(errno unix.Errno) error {
	if errno == unix.EINVAL {
		if _, _, errno := unix.Syscall(unix.SYS_MADVISE, 0, 0, unix.MADV_COLLAPSE); errno != 0 {
			return errNoHostCollapse
		}
	}
	return errno
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
