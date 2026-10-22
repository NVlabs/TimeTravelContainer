// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// The file's mappings.
//
// The file grows a chunk or a few at a time, and the Sentry maps each growth's
// chunks together, where the host places them: it holds a mapping of the file
// per growth. A home chunk is a growth of its own (see ReserveHome), so that
// each home has an address range of its own. The Sentry's mapping of a home is
// advised against huge pages where pgalloc advises against them
// (MemoryFileOpts.AdviseNoHugepage), so that a 4 KiB page placed in an empty
// 2 MiB range gets a 4 KiB host page. A home that holds 2 MiB pages has a
// second mapping, its huge view, without that advice, through which the
// Sentry creates, fills and reads its 2 MiB pages, which the host maps there
// with one page table entry each (see hugeViewLocked). Nothing touches a
// home's 4 KiB pages through its huge view, which would have the host put a
// huge page over their range. The Sentry thus holds at most two mappings of a
// chunk. See g3doc/proposals/ttc_address_ordered_memory.md.

// mapChunksLocked maps the length bytes of f's file from offset off, whole
// chunks, where the host places them, and returns the mapping's address.
//
// Preconditions: f.mu must be locked, or f not yet shared; f.file != nil.
func (f *MemoryFile) mapChunksLocked(off, length uint64) (uintptr, error) {
	m, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, uintptr(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, f.file.Fd(), uintptr(off))
	if errno != 0 {
		return 0, errno
	}
	return m, nil
}

// remapChunkLocked maps the length bytes of f's file from offset off afresh,
// at addr, where they are mapped already. The host drops the page tables of
// the old mapping.
//
// Preconditions: f.mu must be locked; f.file != nil.
func (f *MemoryFile) remapChunkLocked(addr uintptr, off, length uint64) error {
	_, _, errno := unix.Syscall6(unix.SYS_MMAP, addr, uintptr(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED, f.file.Fd(), uintptr(off))
	if errno != 0 {
		return errno
	}
	return nil
}

// hugeViewLocked returns the address of the huge view of home chunk i, mapping
// it if the home has none yet: a mapping of the chunk at an address aligned to
// huge pages, with the advice that allows them. It returns 0 if the host has no
// room for it, or f's pages are not in a file of its own.
//
// Preconditions: f.mu must be locked; f.homes[i] != nil.
func (f *MemoryFile) hugeViewLocked(i uint64) uintptr {
	hc := f.homes[i]
	if hc.hugeView != 0 || hc.noHugeView || f.file == nil || f.backing != nil {
		return hc.hugeView
	}
	addr, err := mapHugeView(f.file.Fd(), i*chunkSize, chunkSize, f.opts.AdviseHugepage)
	if err != nil {
		log.Warningf("MemoryFile: mapping the huge view of home chunk %d: %v; its 2 MiB pages go through mappings of their own", i, err)
		hc.noHugeView = true
		return 0
	}
	hc.hugeView = addr
	return addr
}

// hugeMappingLocked returns a mapping of fr, whole 2 MiB pages of one home
// chunk, through its huge view, through which the Sentry accesses each with
// one page table entry rather than 512, as through its own mapping of the
// chunk. It returns false if fr is not such pages, or the home has no huge
// view. The mapping stays valid while the caller holds fr's pages.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) hugeMappingLocked(fr memmap.FileRange) (safemem.BlockSeq, bool) {
	i := fr.Start / chunkSize
	if fr.Length() == 0 || !hostarch.IsHugePageAligned(fr.Start) || !hostarch.IsHugePageAligned(fr.End) ||
		(fr.End-1)/chunkSize != i || f.homes[i] == nil || !f.isHugeLocked(fr) {
		return safemem.BlockSeq{}, false
	}
	view := f.hugeViewLocked(i)
	if view == 0 {
		return safemem.BlockSeq{}, false
	}
	return safemem.BlockSeqOf(safemem.BlockFromSafeSlice(mappingSlice(view+uintptr(fr.Start-i*chunkSize), uintptr(fr.Length())))), true
}

// HugePageMapping returns a mapping of fr, whole 2 MiB pages of one home chunk
// that its caller holds, through which the Sentry reads them with one page
// table entry each: a copy on write of a 2 MiB page reads its original so.
// It returns false if fr is not such pages; see hugeMappingLocked.
func (f *MemoryFile) HugePageMapping(fr memmap.FileRange) (safemem.BlockSeq, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hugeMappingLocked(fr)
}

// unmapHugeViewsLocked unmaps the huge views of f's home chunks.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) unmapHugeViewsLocked() {
	for i, hc := range f.homes {
		if hc.hugeView == 0 {
			continue
		}
		if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, hc.hugeView, chunkSize, 0); errno != 0 {
			log.Warningf("Failed to unmap the huge view %#x of MemoryFile home chunk %d: %v", hc.hugeView, i, errno)
		}
		hc.hugeView = 0
	}
}

// populateHugeView commits mapped, whole 2 MiB pages in a home's huge view,
// which the host backs with huge pages there, and if reader is not nil fills
// them with what reader reads, through the view, which has the pages' table
// entries: copying through the Sentry's own mapping of the home would fault
// on each base page. It returns how many bytes reader filled, and its error;
// committed is true, as populateHugeFileRange's is when its mapping succeeds.
func populateHugeView(mapped []byte, reader safemem.ReaderFunc) (filled uint64, committed bool, err error) {
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
