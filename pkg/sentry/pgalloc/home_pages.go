// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"fmt"
	"math"
	"slices"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// Home page tables.
//
// pgalloc keeps the state of its pages (references, identities and versions,
// accounting) in segment sets keyed by file offset, whose neighbouring
// segments merge where their values continue one another. Pages that Allocate
// allocates in the order of their offsets merge. Pages at their slots in a home
// are allocated and copied in the order their address space writes them, so
// that copies on write at random left a segment per page in each set, and each
// operation on a page searched sets of millions. A home keeps its pages' state
// in a table of its own instead, by slot, in blocks of 2 MiB: each block holds
// the runs of its pages whose states follow from one, in order, so that a
// block allocated whole is one run, and an operation on a page searches the
// runs of its block alone. The sets hold the rest of the file, and a home's
// pages only while f's state is saved or loaded (see exportHomePagesLocked).
// See g3doc/proposals/ttc_address_ordered_memory.md.

const (
	// blocksPerHome is how many 2 MiB blocks a home has.
	blocksPerHome = chunkSize / hostarch.HugePageSize

	// blocksPerRow is how many blocks a table allocates pointers for at once.
	blocksPerRow = 32

	// rowsPerHome is how many rows of blocks a home has.
	rowsPerHome = (blocksPerHome + blocksPerRow - 1) / blocksPerRow
)

// pageState is a home table's state of a page, or of the first page of a run
// whose pages' states follow from it (see plus).
type pageState struct {
	// refs is how many references the page has: none for a free slot or for
	// waste.
	refs uint32

	// ctx holds the index of the page's context in MemoryFile.pageContexts,
	// which is 0 for a free slot, and its flags (pageWaste, pageCommitted).
	ctx uint32

	// id and ver are the sequences of the page's identity and version, of the
	// allocators its context names, if the page is in use, and 0 otherwise.
	id, ver uint64
}

const (
	// pageWaste marks waste: a page whose references are gone, not yet
	// released (see memAcctInfo.wasteOrReleasing).
	pageWaste = 1 << 31

	// pageCommitted marks a page known to be committed (see
	// memAcctInfo.knownCommitted).
	pageCommitted = 1 << 30

	// pageContextMask is the bits of pageState.ctx that index a context.
	pageContextMask = 1<<24 - 1
)

// inUse returns true if the page is in use.
func (ps pageState) inUse() bool {
	return ps.refs != 0
}

// free returns true if the page is a free slot: neither in use nor waste.
func (ps pageState) free() bool {
	return ps.ctx == 0
}

// waste returns true if the page is waste.
func (ps pageState) waste() bool {
	return ps.ctx&pageWaste != 0
}

// committed returns true if the page is known to be committed.
func (ps pageState) committed() bool {
	return ps.ctx&pageCommitted != 0
}

// context returns the index of the page's context.
func (ps pageState) context() uint32 {
	return ps.ctx & pageContextMask
}

// plus returns the state of the page n pages after one with state ps, in a run
// whose pages' states follow from its: the same, but for identities and
// versions n later.
func (ps pageState) plus(n uint64) pageState {
	if ps.refs != 0 {
		ps.id += n
		ps.ver += n
	}
	return ps
}

// pageContext is what pages of a home share besides their references,
// identities and versions: their accounting (see memAcctInfo), and the
// allocators of their identities and versions.
type pageContext struct {
	kind             usage.MemoryKind
	memCgID          uint32
	allocator        LogicalAllocatorID
	versionAllocator LogicalAllocatorID
}

// pageContextLocked returns the index of c in f.pageContexts, adding it there
// if it is not.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) pageContextLocked(c pageContext) uint32 {
	if i, ok := f.pageContextIndex[c]; ok {
		return i
	}
	if len(f.pageContexts) == 0 {
		// Index 0 is that of a free slot, which has no context.
		f.pageContexts = append(f.pageContexts, pageContext{})
		f.pageContextIndex = make(map[pageContext]uint32)
	}
	i := uint32(len(f.pageContexts))
	if i > pageContextMask {
		panic(fmt.Sprintf("home page contexts exhausted adding %+v", c))
	}
	f.pageContexts = append(f.pageContexts, c)
	f.pageContextIndex[c] = i
	return i
}

// pageRun is a run of a block's pages, from its page start to its page end,
// whose states follow from ps, that of the first (see pageState.plus).
type pageRun struct {
	start, end uint16
	ps         pageState
}

// pages returns how many pages the run holds.
func (r *pageRun) pages() uint64 {
	return uint64(r.end - r.start)
}

// continuedBy returns true if next's pages follow r's: as long as r is one run
// with them.
func (r *pageRun) continuedBy(next *pageRun) bool {
	return r.end == next.start && r.ps.plus(r.pages()) == next.ps
}

// homeBlock is a home table's state of the pages of a 2 MiB block.
type homeBlock struct {
	// runs holds the runs of the block's pages that are not free slots, in
	// order, each as long as it can be: none is continued by the next.
	runs []pageRun

	// huge is true if the block's pages are one 2 MiB page (see
	// logicalPageInfo.Huge): they are all in use, with the identities and
	// versions of one.
	huge bool

	// commitSeq is MemoryFile.commitSeq when a page of the block last stopped
	// being known to be committed, or later: as memAcctInfo.commitSeq is, for
	// all of the block's pages.
	commitSeq uint64
}

// find returns the index of the first of the block's runs that ends after page
// k.
func (blk *homeBlock) find(k uint64) int {
	lo, hi := 0, len(blk.runs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if uint64(blk.runs[mid].end) <= k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// page returns the state of the block's page k.
func (blk *homeBlock) page(k uint64) pageState {
	if i := blk.find(k); i < len(blk.runs) && uint64(blk.runs[i].start) <= k {
		r := &blk.runs[i]
		return r.ps.plus(k - uint64(r.start))
	}
	return pageState{}
}

// pages returns how many of the block's pages are not free slots.
func (blk *homeBlock) pages() uint64 {
	n := uint64(0)
	for i := range blk.runs {
		n += blk.runs[i].pages()
	}
	return n
}

// block returns the state of the pages of the home's block b, or nil if they
// are free slots.
func (hc *homeChunk) block(b uint64) *homeBlock {
	if row := hc.rows[b/blocksPerRow]; row != nil {
		return row[b%blocksPerRow]
	}
	return nil
}

// setBlock makes blk the state of the pages of the home's block b; nil makes
// them free slots.
func (hc *homeChunk) setBlock(b uint64, blk *homeBlock) {
	row := hc.rows[b/blocksPerRow]
	if row == nil {
		if blk == nil {
			return
		}
		row = new([blocksPerRow]*homeBlock)
		hc.rows[b/blocksPerRow] = row
	}
	row[b%blocksPerRow] = blk
	if blk == nil && *row == ([blocksPerRow]*homeBlock{}) {
		hc.rows[b/blocksPerRow] = nil
	}
}

// homeBlocksLocked calls fn with the state of each block of 2 MiB that fr, in
// one home chunk, overlaps, of those whose pages are not all free slots, and
// whether fr covers it whole.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeBlocksLocked(fr memmap.FileRange, fn func(blk *homeBlock, whole bool)) {
	hc := f.homes[fr.Start/chunkSize]
	base := fr.Start &^ chunkMask
	for b := (fr.Start - base) / hostarch.HugePageSize; base+b*hostarch.HugePageSize < fr.End; b++ {
		if blk := hc.block(b); blk != nil {
			start := base + b*hostarch.HugePageSize
			fn(blk, fr.Start <= start && start+hostarch.HugePageSize <= fr.End)
		}
	}
}

// visitHomePagesLocked calls fn with the states of the pages of fr, which lies
// in one home chunk, in order: n pages from off, the first with state ps and
// the others following from it (see pageState.plus). It stops if fn returns
// false.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) visitHomePagesLocked(fr memmap.FileRange, fn func(off, n uint64, ps pageState) bool) {
	hc := f.homes[fr.Start/chunkSize]
	base := fr.Start &^ chunkMask
	for off := fr.Start; off < fr.End; {
		b := (off - base) / hostarch.HugePageSize
		start := base + b*hostarch.HugePageSize
		end := min(fr.End, start+hostarch.HugePageSize)
		k, hi := (off-start)/hostarch.PageSize, (end-start)/hostarch.PageSize
		if blk := hc.block(b); blk != nil {
			for i := blk.find(k); i < len(blk.runs) && uint64(blk.runs[i].start) < hi; i++ {
				r := &blk.runs[i]
				if s := uint64(r.start); s > k {
					// Free slots.
					if !fn(start+k*hostarch.PageSize, s-k, pageState{}) {
						return
					}
					k = s
				}
				e := min(uint64(r.end), hi)
				if !fn(start+k*hostarch.PageSize, e-k, r.ps.plus(k-uint64(r.start))) {
					return
				}
				k = e
			}
		}
		if k < hi && !fn(start+k*hostarch.PageSize, hi-k, pageState{}) {
			return
		}
		off = end
	}
}

// updateHomePagesLocked calls fn with the states of the pages of fr, which
// lies in one home chunk, in order, for fn to return their new states: the n
// pages from off share ps as in visitHomePagesLocked, and the state fn returns
// as one. A 2 MiB page any of whose pages fn takes out of use is base pages from
// then on.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) updateHomePagesLocked(fr memmap.FileRange, fn func(off, n uint64, ps pageState) pageState) {
	hc := f.homes[fr.Start/chunkSize]
	base := fr.Start &^ chunkMask
	for off := fr.Start; off < fr.End; {
		b := (off - base) / hostarch.HugePageSize
		start := base + b*hostarch.HugePageSize
		end := min(fr.End, start+hostarch.HugePageSize)
		lo, hi := (off-start)/hostarch.PageSize, (end-start)/hostarch.PageSize
		blk := hc.block(b)
		if blk != nil && len(blk.runs) == 1 && lo == 0 && hi == pagesPerHugePage() && blk.runs[0].start == 0 && uint64(blk.runs[0].end) == pagesPerHugePage() {
			// A block of one run, all of which changes as one, as a fork's
			// references over a block written whole do.
			r := &blk.runs[0]
			ps := fn(start, pagesPerHugePage(), r.ps)
			if ps.free() {
				hc.setBlock(b, nil)
			} else {
				if r.ps.inUse() && !ps.inUse() {
					blk.huge = false
				}
				r.ps = ps
			}
			off = end
			continue
		}
		var runs []pageRun
		if blk != nil {
			runs = blk.runs
		}
		// The runs that [lo, hi) overlaps are runs[i0:i1]. They are replaced
		// by mid: what of them lies outside [lo, hi), and the pages in it,
		// changed, that are not free slots.
		i0 := 0
		if blk != nil {
			i0 = blk.find(lo)
		}
		i1 := i0
		var buf [4]pageRun
		mid := buf[:0]
		add := func(r pageRun) {
			if r.ps.free() {
				return
			}
			if n := len(mid); n != 0 && mid[n-1].continuedBy(&r) {
				mid[n-1].end = r.end
				return
			}
			mid = append(mid, r)
		}
		huge := blk != nil && blk.huge
		change := func(s, e uint64, was pageState) {
			ps := fn(start+s*hostarch.PageSize, e-s, was)
			if was.inUse() && !ps.inUse() {
				huge = false
			}
			add(pageRun{start: uint16(s), end: uint16(e), ps: ps})
		}
		k := lo
		for ; i1 < len(runs) && uint64(runs[i1].start) < hi; i1++ {
			r := runs[i1]
			if s := uint64(r.start); s > k {
				change(k, s, pageState{})
				k = s
			} else if s < k {
				// The part of the run before lo stays as it is.
				add(pageRun{start: r.start, end: uint16(k), ps: r.ps})
			}
			e := min(uint64(r.end), hi)
			change(k, e, r.ps.plus(k-uint64(r.start)))
			if e < uint64(r.end) {
				// So does the part after hi.
				add(pageRun{start: uint16(e), end: r.end, ps: r.ps.plus(e - uint64(r.start))})
			}
			k = e
		}
		if k < hi {
			change(k, hi, pageState{})
		}
		// Merge mid with the runs around it, where they continue each other.
		if i0 > 0 && len(mid) != 0 && runs[i0-1].continuedBy(&mid[0]) {
			i0--
			mid[0].start = runs[i0].start
			mid[0].ps = runs[i0].ps
		}
		if i1 < len(runs) && len(mid) != 0 && mid[len(mid)-1].continuedBy(&runs[i1]) {
			mid[len(mid)-1].end = runs[i1].end
			i1++
		}
		switch {
		case blk == nil && len(mid) == 0:
		case blk == nil:
			hc.setBlock(b, &homeBlock{runs: append([]pageRun(nil), mid...), huge: huge})
		case i0 == 0 && i1 == len(runs) && len(mid) == 0:
			hc.setBlock(b, nil)
		default:
			blk.runs = slices.Replace(runs, i0, i1, mid...)
			blk.huge = huge
		}
		off = end
	}
}

// splitHomes calls fn with each part of fr, in order: each part in a home
// chunk, one chunk at a time, with home true; and each run of other chunks with
// home false.
func (f *MemoryFile) splitHomes(fr memmap.FileRange, fn func(part memmap.FileRange, home bool)) {
	chunks := f.chunksLoad()
	isHome := func(off uint64) bool {
		i := off / chunkSize
		return i < uint64(len(chunks)) && chunks[i].isHome()
	}
	for start := fr.Start; start < fr.End; {
		home := isHome(start)
		end := min(fr.End, (start&^chunkMask)+chunkSize)
		for !home && end < fr.End && !isHome(end) {
			end = min(fr.End, end+chunkSize)
		}
		fn(memmap.FileRange{Start: start, End: end}, home)
		start = end
	}
}

// homeRun accumulates runs of pages for a caller that walks a home's table.
type homeRun struct {
	fr memmap.FileRange
	ps pageState
}

// add adds n pages from off, of state ps, to the run if they continue it, and
// otherwise returns false.
func (r *homeRun) add(off, n uint64, ps pageState) bool {
	if r.fr.Length() == 0 || r.fr.End != off || r.ps.plus(r.fr.Length()/hostarch.PageSize) != ps {
		return false
	}
	r.fr.End += n * hostarch.PageSize
	return true
}

// homeRunsLocked calls fn with each run of the pages of fr, in one home chunk,
// that are not free slots and whose states follow from its first page's, and
// that lie in one block of 2 MiB.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeRunsLocked(fr memmap.FileRange, fn func(fr memmap.FileRange, ps pageState)) {
	var run homeRun
	f.visitHomePagesLocked(fr, func(off, n uint64, ps pageState) bool {
		if hostarch.IsHugePageAligned(off) || ps.free() || !run.add(off, n, ps) {
			if run.fr.Length() != 0 {
				fn(run.fr, run.ps)
			}
			run = homeRun{}
			if !ps.free() {
				run = homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: ps}
			}
		}
		return true
	})
	if run.fr.Length() != 0 {
		fn(run.fr, run.ps)
	}
}

// homeRefsLocked calls fn with each run of the pages of fr, in one home chunk,
// that have the same number of references, none for free slots and waste. It
// stops if fn returns false.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeRefsLocked(fr memmap.FileRange, fn func(fr memmap.FileRange, refs uint64) bool) {
	var run memmap.FileRange
	var refs uint32
	stopped := false
	f.visitHomePagesLocked(fr, func(off, n uint64, ps pageState) bool {
		if run.Length() != 0 && run.End == off && ps.refs == refs {
			run.End += n * hostarch.PageSize
			return true
		}
		if run.Length() != 0 && !fn(run, uint64(refs)) {
			stopped = true
			return false
		}
		run, refs = memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps.refs
		return true
	})
	if !stopped && run.Length() != 0 {
		fn(run, uint64(refs))
	}
}

// visitRefsLocked calls fn with each run of the pages of fr that have the same
// number of references: none for free pages, waste, and the pages of chunks of
// the other size. It stops if fn returns false.
//
// Preconditions: f.mu must be locked; the file's chunks cover fr.
func (f *MemoryFile) visitRefsLocked(fr memmap.FileRange, fn func(fr memmap.FileRange, refs uint64) bool) {
	more := true
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		if chunk.isHome() {
			f.homeRefsLocked(chunkFR, func(fr memmap.FileRange, refs uint64) bool {
				more = fn(fr, refs)
				return more
			})
			return more
		}
		unfree := &f.unfreeSmall
		if chunk.huge {
			unfree = &f.unfreeHuge
		}
		unfree.VisitFullRange(chunkFR, func(seg unfreeIterator) bool {
			more = fn(seg.Range().Intersect(chunkFR), seg.ValuePtr().refs)
			return more
		})
		return more
	})
}

// incRefHomeLocked adds a reference to each page of fr, in one home chunk.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) incRefHomeLocked(fr memmap.FileRange) {
	f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
		if ps.refs == 0 || ps.refs == math.MaxUint32 {
			panic(fmt.Sprintf("IncRef(%v) called with %d references on pages %#x+%d", fr, ps.refs, off, n))
		}
		ps.refs++
		return ps
	})
}

// decRefHomeLocked drops a reference to each page of fr, in one home chunk:
// as DecRef does, the pages left with none become waste. It returns true if
// any did.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) decRefHomeLocked(fr memmap.FileRange) bool {
	// The runs of pages that become waste, with the contexts they had.
	var wasted []homeRun
	var lastCtx, lastWaste uint32
	f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
		if ps.refs == 0 {
			panic(fmt.Sprintf("DecRef(%v) called with 0 references on pages %#x+%d", fr, off, n))
		}
		ps.refs--
		if ps.refs != 0 {
			return ps
		}
		if k := len(wasted) - 1; k < 0 || !wasted[k].add(off, n, pageState{ctx: ps.ctx}) {
			wasted = append(wasted, homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: pageState{ctx: ps.ctx}})
		}
		// Waste is accounted as System until it is recycled or released.
		if ps.context() != lastCtx {
			lastCtx = ps.context()
			lastWaste = f.pageContextLocked(pageContext{kind: usage.System, memCgID: f.pageContexts[lastCtx].memCgID})
		}
		return pageState{ctx: lastWaste | pageWaste | ps.ctx&pageCommitted}
	})
	for _, run := range wasted {
		f.unwasteHome.RemoveFullRange(run.fr)
		if c := f.pageContexts[run.ps.context()]; run.ps.committed() && !f.opts.DisableMemoryAccounting {
			usage.MemoryAccounting.Move(run.fr.Length(), usage.System, c.kind, c.memCgID)
		}
		f.cancelWasteLoadsLocked(run.fr)
	}
	return len(wasted) != 0
}

// hasUniqueRefHomeLocked returns true if each page of fr, in one home chunk,
// has one reference.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) hasUniqueRefHomeLocked(fr memmap.FileRange) bool {
	unique := true
	f.visitHomePagesLocked(fr, func(_, _ uint64, ps pageState) bool {
		unique = ps.refs == 1
		return unique
	})
	return unique
}

// homeCommitSeqLocked sets the commitSeq of each block that fr, in one home
// chunk, overlaps, to f.commitSeq.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeCommitSeqLocked(fr memmap.FileRange) {
	f.homeBlocksLocked(fr, func(blk *homeBlock, _ bool) {
		blk.commitSeq = f.commitSeq
	})
}

// decommitHomeLocked marks the pages of fr, in one home chunk, uncommitted, as
// Decommit marks those of other chunks.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) decommitHomeLocked(fr memmap.FileRange) {
	var decommitted []homeRun
	f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
		if !ps.committed() {
			return ps
		}
		ps.ctx &^= pageCommitted
		if k := len(decommitted) - 1; k < 0 || !decommitted[k].add(off, n, pageState{ctx: ps.ctx}) {
			decommitted = append(decommitted, homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: pageState{ctx: ps.ctx}})
		}
		return ps
	})
	// Invalidate any observations made by concurrent calls to
	// f.updateUsageLocked().
	f.homeCommitSeqLocked(fr)
	for _, run := range decommitted {
		f.knownCommittedBytes -= run.fr.Length()
		if c := f.pageContexts[run.ps.context()]; !f.opts.DisableMemoryAccounting {
			usage.MemoryAccounting.Dec(run.fr.Length(), c.kind, c.memCgID)
		}
	}
}

// releaseHomeLocked makes the pages of fr, waste of home chunks that the
// releaser has just released, free slots, accounted to no one.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) releaseHomeLocked(fr memmap.FileRange) {
	f.splitHomes(fr, func(part memmap.FileRange, _ bool) {
		var released []homeRun
		f.updateHomePagesLocked(part, func(off, n uint64, ps pageState) pageState {
			if !ps.waste() {
				panic(fmt.Sprintf("released pages %#x+%d are not waste: %+v\n%s", off, n, ps, f.stringLocked()))
			}
			st := pageState{ctx: ps.ctx}
			if k := len(released) - 1; k < 0 || !released[k].add(off, n, st) {
				released = append(released, homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: st})
			}
			return pageState{}
		})
		for _, run := range released {
			if run.ps.committed() {
				f.knownCommittedBytes -= run.fr.Length()
				if c := f.pageContexts[run.ps.context()]; !f.opts.DisableMemoryAccounting {
					usage.MemoryAccounting.Dec(run.fr.Length(), c.kind, c.memCgID)
				}
			}
			f.accountLocked(-int64(run.fr.Length()))
		}
	})
}

// accountingAtLocked returns the memory kind and memory cgroup that the page at
// off is accounted to, and false if it is accounted to none.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) accountingAtLocked(off uint64) (usage.MemoryKind, uint32, bool) {
	if chunks := f.chunksLoad(); off/chunkSize < uint64(len(chunks)) && chunks[off/chunkSize].isHome() {
		var ps pageState
		f.visitHomePagesLocked(memmap.FileRange{Start: off, End: off + hostarch.PageSize}, func(_, _ uint64, s pageState) bool {
			ps = s
			return false
		})
		if ps.free() {
			return 0, 0, false
		}
		c := f.pageContexts[ps.context()]
		return c.kind, c.memCgID, true
	}
	if seg := f.memAcct.FindSegment(off); seg.Ok() {
		return seg.ValuePtr().kind, seg.ValuePtr().memCgID, true
	}
	return 0, 0, false
}

// setHomeIdentitiesLocked gives the pages of fr, in one home chunk and in
// use, the identities and versions that info gives them, as one run (see
// logicalPageInfo): the blocks of 2 MiB that fr covers whole are 2 MiB pages if
// info.Huge, and those it covers in part base pages.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) setHomeIdentitiesLocked(fr memmap.FileRange, info logicalPageInfo) {
	var lastCtx, lastNew uint32
	f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
		if !ps.inUse() {
			panic(fmt.Sprintf("identities %+v for %v, whose pages %#x+%d are not in use: %+v", info, fr, off, n, ps))
		}
		if ps.context() != lastCtx {
			lastCtx = ps.context()
			c := f.pageContexts[lastCtx]
			c.allocator, c.versionAllocator = info.Allocator, info.VersionAllocator
			lastNew = f.pageContextLocked(c)
		}
		delta := (off - fr.Start) / hostarch.PageSize
		ps.ctx = lastNew | ps.ctx&pageCommitted
		ps.id = info.FirstSequence + delta
		ps.ver = info.FirstVersionSequence + delta
		return ps
	})
	f.homeBlocksLocked(fr, func(blk *homeBlock, whole bool) {
		blk.huge = whole && info.Huge
	})
}

// homeLogicalPageRangesLocked appends to ranges the identity runs of the pages
// of fr, in one home chunk, as logicalPageRangesLocked returns those of other
// chunks, and returns it.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeLogicalPageRangesLocked(fr memmap.FileRange, ranges []LogicalPageRange) []LogicalPageRange {
	hc := f.homes[fr.Start/chunkSize]
	base := fr.Start &^ chunkMask
	first := len(ranges)
	f.visitHomePagesLocked(fr, func(off, n uint64, ps pageState) bool {
		if !ps.inUse() {
			return true
		}
		c := f.pageContexts[ps.context()]
		huge := hc.block((off - base) / hostarch.HugePageSize).huge
		if k := len(ranges) - 1; k >= first {
			last := &ranges[k]
			pages := last.Range.Length() / hostarch.PageSize
			if last.Range.End == off && last.Huge == huge &&
				last.First.Allocator == c.allocator && last.First.Sequence+pages == ps.id &&
				last.FirstVersion.Allocator == c.versionAllocator && last.FirstVersion.Sequence+pages == ps.ver {
				last.Range.End += n * hostarch.PageSize
				return true
			}
		}
		ranges = append(ranges, LogicalPageRange{
			Range:        memmap.FileRange{Start: off, End: off + n*hostarch.PageSize},
			First:        LogicalPageID{Allocator: c.allocator, Sequence: ps.id},
			FirstVersion: PageVersionID{Allocator: c.versionAllocator, Sequence: ps.ver},
			Huge:         huge,
		})
		return true
	})
	return ranges
}

// isHugeHomeLocked returns true if fr, in one home chunk, is in 2 MiB pages.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) isHugeHomeLocked(fr memmap.FileRange) bool {
	hc := f.homes[fr.Start/chunkSize]
	base := fr.Start &^ chunkMask
	for b := (fr.Start - base) / hostarch.HugePageSize; base+b*hostarch.HugePageSize < fr.End; b++ {
		if blk := hc.block(b); blk == nil || !blk.huge {
			return false
		}
	}
	return true
}

// splitHugeHomeLocked splits the 2 MiB pages that fr, in one home chunk, is in,
// wholly or in part, into base pages; see splitHugePagesLocked.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) splitHugeHomeLocked(fr memmap.FileRange) {
	f.homeBlocksLocked(fr, func(blk *homeBlock, _ bool) {
		blk.huge = false
	})
}

// markHomePagesUsedLocked makes fr, the free slots and waste of one home chunk,
// used, with one reference each, accounted as kind to memCgID, and with
// identities and versions consecutive from first and firstVersion: 2 MiB
// pages where it covers a block of 2 MiB whole, if huge. It returns the bytes
// of fr that were free slots.
//
// Preconditions: f.mu must be locked; no page of fr is in use or being released.
func (f *MemoryFile) markHomePagesUsedLocked(fr memmap.FileRange, kind usage.MemoryKind, memCgID uint32, first LogicalPageID, firstVersion PageVersionID, huge bool) uint64 {
	ctx := f.pageContextLocked(pageContext{kind: kind, memCgID: memCgID, allocator: first.Allocator, versionAllocator: firstVersion.Allocator})
	// The runs of waste recycled, with the contexts they had.
	var recycled []homeRun
	var freeBytes uint64
	f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
		switch {
		case ps.inUse():
			panic(fmt.Sprintf("marking pages %#x+%d of %v used, which are in use: %+v", off, n, fr, ps))
		case ps.waste():
			st := pageState{ctx: ps.ctx}
			if k := len(recycled) - 1; k < 0 || !recycled[k].add(off, n, st) {
				recycled = append(recycled, homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: st})
			}
		default:
			freeBytes += n * hostarch.PageSize
		}
		delta := (off - fr.Start) / hostarch.PageSize
		return pageState{refs: 1, ctx: ctx, id: first.Sequence + delta, ver: firstVersion.Sequence + delta}
	})
	f.homeCommitSeqLocked(fr)
	f.homeBlocksLocked(fr, func(blk *homeBlock, whole bool) {
		blk.huge = whole && huge
	})
	// Waste is accounted already, as System, and recycled as
	// findAllocatableAndMarkUsed recycles it; free slots are not accounted.
	for _, run := range recycled {
		if run.ps.committed() {
			f.knownCommittedBytes -= run.fr.Length()
			if c := f.pageContexts[run.ps.context()]; !f.opts.DisableMemoryAccounting {
				usage.MemoryAccounting.Dec(run.fr.Length(), usage.System, c.memCgID)
			}
		}
	}
	f.accountLocked(int64(freeBytes))
	return freeBytes
}

// homeWasteLocked returns the runs of fr, in one home chunk, that are waste.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeWasteLocked(fr memmap.FileRange) []memmap.FileRange {
	var waste []memmap.FileRange
	f.visitHomePagesLocked(fr, func(off, n uint64, ps pageState) bool {
		if !ps.waste() {
			return true
		}
		end := off + n*hostarch.PageSize
		if k := len(waste) - 1; k >= 0 && waste[k].End == off {
			waste[k].End = end
		} else {
			waste = append(waste, memmap.FileRange{Start: off, End: end})
		}
		return true
	})
	return waste
}

// updateHomeUsageLocked marks the pages of home chunk i that are in use,
// accounted to a memory cgroup of memCgIDs (any if it is nil), and committed,
// as updateUsageLocked marks those of other chunks. It returns false if f.mu
// was unlocked while a save started.
//
// Preconditions: f.mu must be locked; it may be unlocked and reacquired.
func (f *MemoryFile) updateHomeUsageLocked(i uint64, memCgIDs map[uint32]struct{}, buf *[]byte) (bool, error) {
	cr := chunkRange(i)
	candidate := func(ps pageState) bool {
		if !ps.inUse() || ps.committed() {
			return false
		}
		if memCgIDs != nil {
			if _, ok := memCgIDs[f.pageContexts[ps.context()].memCgID]; !ok {
				return false
			}
		}
		return true
	}
	// Ask the host only about the span of the pages that may be newly
	// committed.
	var scan memmap.FileRange
	f.visitHomePagesLocked(cr, func(off, n uint64, ps pageState) bool {
		if candidate(ps) {
			if scan.Length() == 0 {
				scan.Start = off
			}
			scan.End = off + n*hostarch.PageSize
		}
		return true
	})
	if scan.Length() == 0 {
		return true, nil
	}
	pages := int(scan.Length() / hostarch.PageSize)
	if len(*buf) < pages {
		*buf = make([]byte, pages)
	}
	lastCommitSeq := f.commitSeq
	f.commitSeq++
	f.mu.Unlock()
	var err error
	if f.backing != nil {
		err = f.backing.QueryCommitment(scan, (*buf)[:pages])
	} else {
		err = mincore(f.chunksLoad()[i].sliceAt(scan), (*buf)[:pages])
	}
	f.mu.Lock()
	if err != nil {
		return true, err
	}
	if f.isSaving {
		return false, nil
	}
	// The table may have changed while f.mu was unlocked: pages taken out of
	// use, or decommitted, which makes their blocks' commitSeq too late.
	hc := f.homes[i]
	var committed []homeRun
	for k := 0; k < pages; {
		if (*buf)[k]&0x1 == 0 {
			k++
			continue
		}
		j := k + 1
		for j < pages && (*buf)[j]&0x1 != 0 {
			j++
		}
		fr := memmap.FileRange{Start: scan.Start + uint64(k)*hostarch.PageSize, End: scan.Start + uint64(j)*hostarch.PageSize}
		f.updateHomePagesLocked(fr, func(off, n uint64, ps pageState) pageState {
			if !candidate(ps) || hc.block((off-cr.Start)/hostarch.HugePageSize).commitSeq > lastCommitSeq {
				return ps
			}
			ps.ctx |= pageCommitted
			st := pageState{ctx: ps.ctx}
			if r := len(committed) - 1; r < 0 || !committed[r].add(off, n, st) {
				committed = append(committed, homeRun{fr: memmap.FileRange{Start: off, End: off + n*hostarch.PageSize}, ps: st})
			}
			return ps
		})
		k = j + 1
	}
	for _, run := range committed {
		f.knownCommittedBytes += run.fr.Length()
		if c := f.pageContexts[run.ps.context()]; !f.opts.DisableMemoryAccounting {
			usage.MemoryAccounting.Inc(run.fr.Length(), c.kind, c.memCgID)
		}
	}
	return true, nil
}

// exportHomePagesLocked moves the state of the home chunks' pages from their
// tables into the sets, which hold that of the file's other pages, for a save
// of f's state that saves the sets, and returns a function that moves it back.
// Until then, f's pages must not be allocated, referenced or freed: only the
// sets hold their state.
//
// Preconditions: f.mu must be locked, until the returned function is called.
func (f *MemoryFile) exportHomePagesLocked() func() {
	for i, hc := range f.homes {
		cr := chunkRange(i)
		f.homeRunsLocked(cr, func(fr memmap.FileRange, ps pageState) {
			blk := hc.block((fr.Start - cr.Start) / hostarch.HugePageSize)
			c := f.pageContexts[ps.context()]
			if ps.inUse() {
				f.unfreeSmall.MutateFullRange(fr, func(seg unfreeIterator) bool {
					seg.ValuePtr().refs = uint64(ps.refs)
					return true
				})
				f.logicalPages.InsertRange(fr, logicalPageInfo{
					Allocator:            c.allocator,
					FirstSequence:        ps.id,
					VersionAllocator:     c.versionAllocator,
					FirstVersionSequence: ps.ver,
					Huge:                 blk.huge,
				})
			}
			ma := memAcctInfo{
				kind:             c.kind,
				memCgID:          c.memCgID,
				knownCommitted:   ps.committed(),
				wasteOrReleasing: ps.waste(),
			}
			if !ma.knownCommitted {
				ma.commitSeq = blk.commitSeq
			}
			f.memAcct.InsertRange(fr, ma)
		})
		hc.rows = [rowsPerHome]*[blocksPerRow]*homeBlock{}
	}
	return f.importHomePagesLocked
}

// importHomePagesLocked moves the state of the home chunks' pages from the
// sets, where a load of f's state or exportHomePagesLocked left it, into their
// tables.
//
// Preconditions: f.mu must be locked; the home chunks' tables are empty.
func (f *MemoryFile) importHomePagesLocked() {
	for i := range f.homes {
		cr := chunkRange(i)
		for ma := f.memAcct.LowerBoundSegment(cr.Start); ma.Ok() && ma.Start() < cr.End; ma = ma.NextSegment() {
			f.importHomeAccountedLocked(ma.Range().Intersect(cr), ma.Value())
		}
		// A 2 MiB page may be split in the other sets, where parts of it
		// differ in references or accounting.
		for lp := f.logicalPages.LowerBoundSegment(cr.Start); lp.Ok() && lp.Start() < cr.End; lp = lp.NextSegment() {
			if lp.ValuePtr().Huge {
				f.homeBlocksLocked(lp.Range().Intersect(cr), func(blk *homeBlock, whole bool) {
					blk.huge = whole
				})
			}
		}
		// The sets hold nothing of a home but its slots, void in unfreeSmall.
		f.unfreeSmall.MutateFullRange(cr, func(seg unfreeIterator) bool {
			seg.ValuePtr().refs = 0
			return true
		})
		f.logicalPages.RemoveRange(cr)
		f.memAcct.RemoveRange(cr)
	}
}

// importHomeAccountedLocked puts the state of the pages of fr, in one home
// chunk, which the sets hold and account as ma, into the home's table.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) importHomeAccountedLocked(fr memmap.FileRange, ma memAcctInfo) {
	flags := uint32(0)
	if ma.knownCommitted {
		flags |= pageCommitted
	}
	if ma.wasteOrReleasing {
		flags |= pageWaste
	}
	for at := fr.Start; at < fr.End; {
		uf := f.unfreeSmall.FindSegment(at)
		if !uf.Ok() {
			panic(fmt.Sprintf("home page %#x is accounted, but void in unfreeSmall\n%s", at, f.stringLocked()))
		}
		end := min(fr.End, uf.End())
		refs := uf.ValuePtr().refs
		if (refs == 0) != ma.wasteOrReleasing || refs > math.MaxUint32 {
			panic(fmt.Sprintf("home page %#x has %d references, accounted as %+v", at, refs, ma))
		}
		if refs == 0 {
			ctx := f.pageContextLocked(pageContext{kind: ma.kind, memCgID: ma.memCgID}) | flags
			f.updateHomePagesLocked(memmap.FileRange{Start: at, End: end}, func(_, _ uint64, _ pageState) pageState {
				return pageState{ctx: ctx}
			})
			at = end
			continue
		}
		info := logicalPageInfo{Allocator: f.logicalAllocator, VersionAllocator: f.logicalAllocator}
		if lp := f.logicalPages.FindSegment(at); lp.Ok() {
			end = min(end, lp.End())
			info = lp.Value()
			delta := (at - lp.Start()) / hostarch.PageSize
			info.FirstSequence += delta
			info.FirstVersionSequence += delta
		} else {
			// A page in use has an identity; should one not, it takes a new
			// one, as one restored from an image older than identities does
			// (see ensureLogicalPagesLocked).
			if lp := f.logicalPages.LowerBoundSegment(at); lp.Ok() {
				end = min(end, lp.Start())
			}
			info.FirstSequence, info.FirstVersionSequence = f.newIdentitiesLocked(memmap.FileRange{Start: at, End: end}, false)
		}
		ctx := f.pageContextLocked(pageContext{kind: ma.kind, memCgID: ma.memCgID, allocator: info.Allocator, versionAllocator: info.VersionAllocator}) | flags
		start := at
		f.updateHomePagesLocked(memmap.FileRange{Start: at, End: end}, func(off, _ uint64, _ pageState) pageState {
			delta := (off - start) / hostarch.PageSize
			return pageState{refs: uint32(refs), ctx: ctx, id: info.FirstSequence + delta, ver: info.FirstVersionSequence + delta}
		})
		at = end
	}
	f.homeBlocksLocked(fr, func(blk *homeBlock, _ bool) {
		blk.commitSeq = max(blk.commitSeq, ma.commitSeq)
	})
}

// homePagesString returns a summary of the home chunks' tables.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homePagesString() string {
	var blocks, runs, huge int
	for _, hc := range f.homes {
		for _, row := range hc.rows {
			if row == nil {
				continue
			}
			for _, blk := range row {
				if blk == nil {
					continue
				}
				blocks++
				runs += len(blk.runs)
				if blk.huge {
					huge++
				}
			}
		}
	}
	return fmt.Sprintf("home pages: %d blocks of 2 MiB (%d 2 MiB pages) in %d runs, %d contexts\n", blocks, huge, runs, len(f.pageContexts))
}
