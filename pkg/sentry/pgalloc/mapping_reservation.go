// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
)

// The file's mappings.
//
// The host keeps two adjacent mappings of a file in one mapping (VMA) only if
// they map adjacent ranges of the file, with the same advice and node binding,
// and limits how many mappings a process may have. The file grows a chunk at a
// time, and a mapping that the host places for each growth lands apart from
// the others, so that the process holds a mapping per growth: with home chunks
// (see ReserveHome), one per block of addresses that its applications use. A
// file that MemoryFileOpts.ReserveMappings asks for maps its chunks in one
// address range reserved for them instead, each at the range's start plus its
// offset, so that its mappings are one per run of chunks with the same advice
// and node, however many chunks it has. See
// g3doc/proposals/ttc_address_ordered_memory.md.

const (
	// maxMappingReservation and minMappingReservation bound the address range
	// that reserveAddressRange reserves: from what holds every home chunk
	// (16 TiB) and as much again, down to what a host with a small address
	// space has room for. Chunks past it are mapped apart.
	maxMappingReservation = 64 << 40
	minMappingReservation = 64 << 30
)

// mappingReservation is an address range reserved for a file's chunk mappings.
type mappingReservation struct {
	base, length uintptr

	// tried is true once a range was asked for, if none was reserved.
	tried bool
}

// contains returns true if addr is in r.
func (r mappingReservation) contains(addr uintptr) bool {
	return r.length != 0 && addr >= r.base && addr-r.base < r.length
}

// mapChunksLocked maps the length bytes of f's file from offset off, whole
// chunks, and returns the mapping's address: in f's reserved address range,
// at its start plus off, where MemoryFileOpts.ReserveMappings asks for one and
// the range holds them.
//
// Preconditions: f.mu must be locked, or f not yet shared; f.file != nil.
func (f *MemoryFile) mapChunksLocked(off, length uint64) (uintptr, error) {
	if f.opts.ReserveMappings && f.reservation.length == 0 && !f.reservation.tried {
		f.reservation.base, f.reservation.length = reserveAddressRange()
		f.reservation.tried = true
		if f.reservation.length == 0 {
			log.Warningf("MemoryFile: no address range to reserve for its mappings; mapping each growth apart")
		} else {
			log.Infof("MemoryFile: reserved %d GiB of addresses at %#x for its mappings", f.reservation.length>>30, f.reservation.base)
		}
	}
	addr, flags := uintptr(0), uintptr(unix.MAP_SHARED)
	if r := f.reservation; r.length != 0 && off+length >= off && off+length <= uint64(r.length) {
		addr, flags = r.base+uintptr(off), unix.MAP_SHARED|unix.MAP_FIXED
	}
	m, _, errno := unix.Syscall6(unix.SYS_MMAP, addr, uintptr(length), unix.PROT_READ|unix.PROT_WRITE, flags, f.file.Fd(), uintptr(off))
	if errno != 0 {
		return 0, errno
	}
	return m, nil
}
