// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Collapse.
//
// An aligned 2 MiB range of an address space whose 512 base pages are all
// private, in one home at their slots, and written alike, is laid out as a 2
// MiB page: a fold step collapses it into one (pgalloc.MemoryFile.CollapseHuge),
// which the host backs with a huge page, as Linux's khugepaged collapses base
// pages. Only ranges whose pages are all present are collapsed: Linux would
// also fill those freed with zeroes, which would undo the application's
// frees. Copies on write after a fork, and frees in part, leave memory in base
// pages; collapse returns what is whole again to huge pages. See
// g3doc/proposals/ttc_address_ordered_memory.md.

// noteCollapsibleLocked records the aligned 2 MiB ranges of ar, which mm has
// just placed in fr, a range of a home, as candidates for collapse, if fr
// fills their slots.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) noteCollapsibleLocked(ar hostarch.AddrRange, fr memmap.FileRange) {
	if !mm.mf.MayCollapse() {
		return
	}
	if _, _, ok := mm.mf.HomeOf(fr.Start); !ok {
		return
	}
	home := fr.Start &^ (homeBlockSize - 1)
	noted := false
	for start := ar.Start.HugeRoundDown(); start < ar.End; start += hostarch.HugePageSize {
		slots := memmap.FileRange{Start: home + uint64(start)%homeBlockSize}
		slots.End = slots.Start + hostarch.HugePageSize
		if _, ok := mm.fold.collapse[start]; ok || !mm.mf.SlotsFull(slots) {
			continue
		}
		mm.noteCollapseLocked(start)
		noted = true
	}
	if noted {
		// It is collapsed once it has waited collapseAge.
		requestFoldsLater(mm.mf)
	}
}

// noteCollapseLocked records the aligned 2 MiB range at start as a candidate
// for collapse.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) noteCollapseLocked(start hostarch.Addr) {
	if mm.fold.collapse == nil {
		mm.fold.collapse = make(map[hostarch.Addr]collapseCandidate)
	}
	if _, ok := mm.fold.collapse[start]; !ok {
		mm.fold.collapse[start] = collapseCandidate{since: time.Now()}
	}
}

// noteCollapsibleBlockLocked records the aligned 2 MiB ranges of block that
// mm's private pages fill as candidates for collapse.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) noteCollapsibleBlockLocked(block hostarch.AddrRange) {
	if !mm.mf.MayCollapse() {
		return
	}
	noted := false
	for pseg := mm.pmas.LowerBoundSegment(block.Start); pseg.Ok() && pseg.Start() < block.End; pseg = pseg.NextSegment() {
		if p := pseg.ValuePtr(); !p.private || p.file != mm.mf || p.needCOW {
			continue
		}
		ar := pseg.Range().Intersect(block)
		for start := ar.Start.HugeRoundDown(); start < ar.End; start += hostarch.HugePageSize {
			if _, ok := mm.fold.collapse[start]; ok {
				continue
			}
			if _, ok := mm.collapsibleLocked(hostarch.AddrRange{Start: start, End: start + hostarch.HugePageSize}); ok {
				mm.noteCollapseLocked(start)
				noted = true
			}
		}
	}
	if noted {
		// It is collapsed once it has waited collapseAge.
		requestFoldsLater(mm.mf)
	}
}

// collapsibleLocked returns the file range that mm maps the aligned 2 MiB
// range ar to, and true, if ar is in one anonymous private mapping that may
// have huge pages, and its pages are private, written alike (no copy on write
// to come) and contiguous at aligned slots: what a 2 MiB page's would be.
//
// Preconditions: mm.mappingMu and mm.activeMu must be locked.
func (mm *MemoryManager) collapsibleLocked(ar hostarch.AddrRange) (memmap.FileRange, bool) {
	vseg := mm.vmas.FindSegment(ar.Start)
	if !vseg.Ok() || !vseg.Range().IsSupersetOf(ar) {
		return memmap.FileRange{}, false
	}
	if vma := vseg.ValuePtr(); vma.mappable != nil || !vma.private || vma.growsDown || vma.isStack {
		return memmap.FileRange{}, false
	}
	var fr memmap.FileRange
	var first *pma
	at := ar.Start
	for pseg := mm.pmas.FindSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		p := pseg.ValuePtr()
		if pseg.Start() > at || !p.private || p.file != mm.mf || p.needCOW {
			return memmap.FileRange{}, false
		}
		if first == nil {
			first = p
		} else if p.effectivePerms != first.effectivePerms || p.maxPerms != first.maxPerms {
			return memmap.FileRange{}, false
		}
		pieceFR := pseg.fileRangeOf(pseg.Range().Intersect(ar))
		if fr.Length() == 0 {
			fr = pieceFR
		} else if pieceFR.Start != fr.End {
			return memmap.FileRange{}, false
		} else {
			fr.End = pieceFR.End
		}
		at = pseg.End()
	}
	if at < ar.End || fr.Length() != hostarch.HugePageSize || !hostarch.IsHugePageAligned(fr.Start) {
		return memmap.FileRange{}, false
	}
	// A pma split from a 2 MiB page says huge still: what the pages are is
	// the file's to say.
	if _, _, ok := mm.mf.HomeOf(fr.Start); !ok || mm.mf.IsHugePages(fr) {
		return memmap.FileRange{}, false
	}
	return fr, true
}

// maxWaitingCollapses is how many candidates for collapse an address space
// keeps waiting for discarded pages to be written again.
const maxWaitingCollapses = 4096

// maxCollapseAttempts is how often a candidate for collapse is tried while the
// host has no huge page for it.
const maxCollapseAttempts = 8

// collapseAge is how long a candidate for collapse waits before it is
// collapsed, as Linux's khugepaged scans every 10 s: a short-lived process's
// memory, which goes soon, is not copied into huge pages first. Tests change
// it.
var collapseAge = 10 * time.Second

// collapseCandidate is a range that may be collapsed into a 2 MiB page: since
// when, and how often the host had no huge page for it.
type collapseCandidate struct {
	since    time.Time
	attempts uint8
}

// collapseLocked collapses ar, a candidate for collapse, if it still is one
// and mm holds its pages alone, and returns true if it did. wait is true if ar
// may be collapsed once a page of it that its application discarded is
// written again, which mm sees only by looking again.
//
// Preconditions: mm.mappingMu and mm.activeMu must be locked; activeMu for
// writing.
func (mm *MemoryManager) collapseLocked(ar hostarch.AddrRange) (collapsed, wait bool) {
	fr, ok := mm.collapsibleLocked(ar)
	if !ok || !mm.mf.Movable(fr) {
		return false, false
	}
	if !mm.mf.Resident(fr) {
		return false, true
	}
	// The host copies the base pages into a huge page, and the platform maps
	// the huge page in their place.
	mm.unmapASLocked(ar)
	if collapsed, retry := mm.mf.CollapseHuge(fr); !collapsed {
		if retry {
			c := mm.fold.collapse[ar.Start]
			c.attempts++
			mm.fold.collapse[ar.Start] = c
			retry = c.attempts < maxCollapseAttempts
		}
		return false, retry
	}
	for pseg := mm.pmas.FindSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		pseg = mm.pmas.Isolate(pseg, ar)
		pseg.ValuePtr().huge = true
	}
	mm.pmas.MergeInsideRange(ar)
	mm.pmas.MergeOutsideRange(ar)
	return true, false
}

// noteDecommittedLocked records the aligned 2 MiB ranges of ar, which mm
// decommitted in place, as candidates for collapse once the application writes
// them again.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) noteDecommittedLocked(ar hostarch.AddrRange) {
	if mm.homeOwner == 0 || !mm.mf.MayCollapse() {
		return
	}
	noted := false
	for start := ar.Start.HugeRoundDown(); start < ar.End; start += hostarch.HugePageSize {
		if len(mm.fold.collapse) < maxWaitingCollapses {
			mm.noteCollapseLocked(start)
			noted = true
		}
	}
	if noted {
		// A pass looks at them once they have waited collapseAge.
		requestFoldsLater(mm.mf)
	}
}

// CollapseWaiting returns true if mm has candidates for collapse that wait for
// pages its application discarded to be written again; see FoldStep.
func (mm *MemoryManager) CollapseWaiting() bool {
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	return len(mm.fold.collapse) != 0
}
