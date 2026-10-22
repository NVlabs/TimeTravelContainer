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

// mapHugeView maps the length bytes of the shared memory file fd from offset
// off, a home chunk, at an address aligned to huge pages, advised
// MADV_HUGEPAGE if adviseHuge, and returns its address: the host may map the
// chunk's 2 MiB pages there as huge pages. See MemoryFile.hugeViewLocked.
func mapHugeView(fd uintptr, off, length uint64, adviseHuge bool) (uintptr, error) {
	room := uintptr(length) + hostarch.HugePageSize
	start, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, room, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE, ^uintptr(0) /* fd */, 0)
	if errno != 0 {
		return 0, errno
	}
	addr := uintptr(hostarch.Addr(start).MustHugeRoundUp())
	if _, _, errno := unix.Syscall6(unix.SYS_MMAP, addr, uintptr(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED, fd, uintptr(off)); errno != 0 {
		unix.Syscall(unix.SYS_MUNMAP, start, room, 0)
		return 0, errno
	}
	// What the alignment leaves at either end is not the view's.
	if head := addr - start; head != 0 {
		unix.Syscall(unix.SYS_MUNMAP, start, head, 0)
	}
	if tail := start + room - (addr + uintptr(length)); tail != 0 {
		unix.Syscall(unix.SYS_MUNMAP, addr+uintptr(length), tail, 0)
	}
	adviseHugeView(addr, length, adviseHuge)
	return addr, nil
}

// adviseHugeView advises the huge view at addr, of length bytes, MADV_HUGEPAGE
// if adviseHuge, as a temporary mapping of a 2 MiB page is advised.
func adviseHugeView(addr uintptr, length uint64, adviseHuge bool) {
	if !adviseHuge {
		return
	}
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, uintptr(length), unix.MADV_HUGEPAGE); errno != 0 {
		log.Warningf("madvise(%#x, %d, MADV_HUGEPAGE) failed: %s", addr, length, errno)
	}
}

// collapseFileRange has the host back fr of the shared memory file fd, one
// aligned 2 MiB range, with a huge page, copying its base pages into one, as
// its khugepaged does: MADV_COLLAPSE, through view, the address of fr in the
// huge view of its home, or if view is 0 a mapping of fr alone at an aligned
// address, as the host collapses only what it may map as a huge page there.
// The mapping is touched first: Linux 6.1 maps the huge page in the mapping
// only if that has a page table, and fails the call otherwise, having
// collapsed the range all the same. It returns errNoHostCollapse if the host
// has no MADV_COLLAPSE (before Linux 6.1).
func collapseFileRange(fd uintptr, fr memmap.FileRange, view uintptr) error {
	length := uintptr(fr.Length())
	if view != 0 {
		if _, _, errno := unix.Syscall(unix.SYS_MADVISE, view, hostarch.PageSize, unix.MADV_POPULATE_READ); errno != 0 {
			return collapseError(errno)
		}
		if _, _, errno := unix.Syscall(unix.SYS_MADVISE, view, length, unix.MADV_COLLAPSE); errno != 0 {
			return collapseError(errno)
		}
		return nil
	}
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
// as populateHugeView does through a home's huge view, where the home has none.
// The host aligns a shared memory mapping to huge pages.
func populateHugeFileRange(fd uintptr, fr memmap.FileRange, adviseHuge bool, reader safemem.ReaderFunc) (filled uint64, committed bool, err error) {
	length := uintptr(fr.Length())
	addr, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, fd, uintptr(fr.Start))
	if errno != 0 {
		return 0, false, errno
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
	if !tryPopulateMadv(safemem.BlockFromSafeSlice(mapped)) {
		// Touch each huge page instead.
		for off := 0; off < len(mapped); off += hostarch.HugePageSize {
			mapped[off] = 0
		}
	}
	if reader == nil {
		return 0, true, nil
	}
	filled, err = safemem.ReadFullToBlocks(reader, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(mapped)))
	return filled, true, err
}
