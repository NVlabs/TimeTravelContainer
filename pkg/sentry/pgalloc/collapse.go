// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Collapse.
//
// A 2 MiB range of a home whose 512 base pages an address space holds alone,
// at their slots, is laid out as a 2 MiB page already: collapsing it makes it
// one, which the host backs with a huge page, as Linux's khugepaged collapses
// a range of base pages. Copies on write after a fork, and frees in part, leave
// memory in base pages; collapse returns what is whole again to huge pages.
// See g3doc/proposals/ttc_address_ordered_memory.md.

// collapseHost has the host back fr of the file fd with a huge page, through
// view, fr's address in the huge view of its home, if that is not 0; tests
// replace it on hosts whose files have none.
var collapseHost = collapseFileRange

// errNoHostCollapse is the error of collapseHost on a host that does not
// collapse base pages into huge pages.
var errNoHostCollapse = errors.New("the host does not collapse base pages into huge pages")

// noCollapse is set once collapseHost returned errNoHostCollapse.
var noCollapse atomicbitops.Bool

// SetCollapseHostForTest has CollapseHuge collapse ranges with fn, which tests
// use on hosts whose files have no huge pages, until the function it returns
// is called.
func SetCollapseHostForTest(fn func(fd uintptr, fr memmap.FileRange) error) func() {
	old := collapseHost
	collapseHost = func(fd uintptr, fr memmap.FileRange, _ uintptr) error { return fn(fd, fr) }
	return func() {
		collapseHost = old
		noCollapse.Store(false)
	}
}

// MayCollapse returns true if f may collapse ranges of base pages into 2 MiB
// pages; see CollapseHuge.
func (f *MemoryFile) MayCollapse() bool {
	return f.opts.ExpectHugepages && f.file != nil && f.backing == nil && !noCollapse.Load()
}

// CollapseHuge makes fr, an aligned 2 MiB range of a home chunk whose base
// pages its caller holds alone, one 2 MiB page: the host backs it with a huge
// page, and it is one page with one version, as a 2 MiB page is (see
// logicalPageInfo.Huge), keeping its pages' identities if they are a 2 MiB
// page's. It returns false, and changes nothing that its caller sees, if
// anything else holds a page of fr (see Movable), it is a 2 MiB page already,
// or the host does not collapse it; retry is true if the host may later, as
// when it has no free huge page now.
//
// Preconditions: fr is an aligned 2 MiB range in a home chunk, whose pages are
// all in use.
func (f *MemoryFile) CollapseHuge(fr memmap.FileRange) (collapsed, retry bool) {
	if !hostarch.IsHugePageAligned(fr.Start) || fr.Length() != hostarch.HugePageSize {
		panic(fmt.Sprintf("invalid collapse range %v", fr))
	}
	if !f.MayCollapse() || !f.Movable(fr) {
		return false, false
	}
	f.mu.Lock()
	ok := f.homes[fr.Start/chunkSize] != nil && !f.isHugeLocked(fr)
	var view uintptr
	if ok {
		if view = f.hugeViewLocked(fr.Start / chunkSize); view != 0 {
			view += uintptr(fr.Start % chunkSize)
		}
	}
	f.mu.Unlock()
	if !ok {
		return false, false
	}
	if err := collapseHost(f.file.Fd(), fr, view); err != nil {
		if errors.Is(err, errNoHostCollapse) && noCollapse.CompareAndSwap(false, true) {
			log.Infof("MemoryFile: %v; collapse is off", err)
		}
		return false, errors.Is(err, unix.ENOMEM) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EBUSY)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collapseIdentitiesLocked(fr)
	return true, false
}

// collapseIdentitiesLocked makes fr, an aligned 2 MiB range, one 2 MiB page
// with a new version: with its pages' identities if they are consecutive from
// a multiple of 512, as a 2 MiB page's are, and with new ones otherwise.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) collapseIdentitiesLocked(fr memmap.FileRange) {
	if 2*pagesPerHugePage() > hugeSequenceBit-f.nextPageVersion || 2*pagesPerHugePage() > hugeSequenceBit-f.nextLogicalPage {
		panic(fmt.Sprintf("page sequence exhausted collapsing %v", fr))
	}
	runs := f.logicalPageRangesLocked(fr)
	first := LogicalPageID{}
	whole := len(runs) != 0 && runs[0].Range.Start == fr.Start && runs[0].First.Sequence%pagesPerHugePage() == 0
	at := fr.Start
	for _, run := range runs {
		want := runs[0].First.Sequence + (run.Range.Start-fr.Start)/hostarch.PageSize
		if run.Range.Start != at || run.First.Allocator != runs[0].First.Allocator || run.First.Sequence != want {
			whole = false
		}
		at = run.Range.End
	}
	if whole && at == fr.End {
		first = runs[0].First
	} else {
		f.nextLogicalPage = alignSequence(f.nextLogicalPage)
		first = LogicalPageID{Allocator: f.logicalAllocator, Sequence: f.nextLogicalPage}
		f.nextLogicalPage += pagesPerHugePage()
	}
	f.setLogicalPagesLocked(fr, logicalPageInfo{
		Allocator:            first.Allocator,
		FirstSequence:        first.Sequence,
		VersionAllocator:     f.logicalAllocator,
		FirstVersionSequence: f.newVersionsLocked(fr, true),
		Huge:                 true,
	})
}

// Resident returns true if every page of fr is in memory: none was discarded,
// as MADV_DONTNEED discards a page, nor is swapped out. A range collapsed with
// a page that is not would have it filled, with zeroes for a page discarded,
// undoing what discarded it.
func (f *MemoryFile) Resident(fr memmap.FileRange) bool {
	if f.file == nil || f.backing != nil {
		return false
	}
	resident := true
	f.forEachMappingSlice(fr, func(s []byte) {
		if !resident {
			return
		}
		vec := make([]byte, (len(s)+hostarch.PageSize-1)/hostarch.PageSize)
		if mincore(s, vec) != nil {
			resident = false
			return
		}
		for _, v := range vec {
			if v&1 == 0 {
				resident = false
				return
			}
		}
	})
	return resident
}

// SlotsFull returns true if every page of fr, in a home chunk, is in use.
func (f *MemoryFile) SlotsFull(fr memmap.FileRange) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := true
	f.homeRefsLocked(fr, func(_ memmap.FileRange, refs uint64) bool {
		full = refs != 0
		return full
	})
	return full
}
