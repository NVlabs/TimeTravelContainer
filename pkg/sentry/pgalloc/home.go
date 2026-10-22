// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"errors"
	"fmt"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// Home chunks.
//
// A platform that maps each of an address space's ranges with a host mapping
// of its own (systrap) keeps two pages that are adjacent in the address space
// in one host mapping only if they are adjacent in the file too, and the host
// limits how many mappings a process may have. A home is a whole chunk of the
// file that one address space, its owner, reserves for one aligned
// chunk-sized block of its addresses: the page at address a of block b
// belongs at offset a - b.Start of the home, its slot. The owner's pages are
// then in address order whatever order they were allocated in. See
// g3doc/proposals/ttc_address_ordered_memory.md.
//
// A home chunk is a small chunk that Allocate never allocates from. Its free
// slots stay void in unfreeSmall, as the pages of chunks of the other size
// are, and its waste is tracked in unwasteHome rather than unwasteSmall, so
// that only AllocateAt recycles it, into its own slots. The releaser releases
// it as any other waste, back into free slots. Home chunks are marked in
// chunkInfo.node, so that the saved state keeps its fields.

// homeChunkNode marks chunkInfo.node of a home chunk. Node numbers are small,
// so it never marks one of theirs; and a chunk marked so is bound to no node,
// nor matches one that an allocation asks for (findNodeGap).
const homeChunkNode = int32(1) << 30

// isHome reports whether c is a home chunk.
func (c *chunkInfo) isHome() bool {
	return c.node&homeChunkNode != 0
}

// HomeOwner identifies the owner of home chunks, an address space. The zero
// HomeOwner is none.
type HomeOwner uint64

var lastHomeOwner atomicbitops.Uint64

// NewHomeOwner returns a HomeOwner that no other has.
func NewHomeOwner() HomeOwner {
	return HomeOwner(lastHomeOwner.Add(1))
}

// homeChunk is the state of a home chunk.
type homeChunk struct {
	// owner is the address space that allocates the chunk's slots, or zero.
	owner HomeOwner

	// pages is how many of the chunk's pages are used, waste or being
	// released: all but its free slots.
	pages uint64

	// nodes holds, for each 2 MiB of the chunk, one more than the host memory
	// node its pages are bound to, or 0 if they are bound to none. It is nil
	// until a page of the chunk is placed on a node.
	nodes []int32

	// windows are the windows of identities that the chunk's pages allocated
	// afresh take, the first windowCount of them; see homeIdentitiesLocked.
	windows     [maxSlotWindows]slotWindow
	windowCount int

	// unit, if hasUnit, is the window of the residency units that the chunk
	// holds at their slots; see unitSlots.
	unit    unitWindow
	hasUnit bool

	// hugeView is the address of the chunk's huge view, or 0 if it has none
	// yet; noHugeView is true if the host had no room for one. See
	// hugeViewLocked.
	hugeView   uintptr
	noHugeView bool
}

// Identities from slots.
//
// A page allocated afresh in a home takes its identity from its slot: the
// home's window plus the slot's index. A logical chunk of pages, 256
// consecutive identities, is then 1 MiB of consecutive slots, and so of
// consecutive addresses of the address space that placed them; a copy keeps
// its original's identity, at the same slot of another home. A residency unit,
// a logical chunk restored from an image, is installed at its slots in a home
// of its own window (see unitSlots), so that what an address space restores is
// in address order too. A slot taken afresh again takes its identity in the
// home's next window, so that identities never repeat; a region freed and
// taken again whole stays in one window. See
// g3doc/proposals/ttc_address_ordered_memory.md.

// pagesPerChunk is how many base pages a chunk holds: the length of a window.
const pagesPerChunk = chunkSize / hostarch.PageSize

// maxSlotWindows is how many windows a home has at most: a slot taken afresh
// more often than that takes an identity of no slot.
const maxSlotWindows = 4

// slotWindow is a window of identities of a home: start is the first logical
// page sequence, that of the home's first slot, and fresh holds the slots that
// pages allocated afresh took in it.
type slotWindow struct {
	start uint64
	fresh unwasteSet
}

// unitWindow is a window of logical pages.
type unitWindow struct {
	allocator LogicalAllocatorID
	window    uint64
}

// homeIdentitiesLocked gives fr, pages of home chunk i allocated afresh, the
// identities of their slots, in 2 MiB pages if huge, and new versions: those of
// the first of the home's windows in which no page took them afresh before, or
// identities of no slot, as pages out of homes take, if there is none.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) homeIdentitiesLocked(i uint64, fr memmap.FileRange, huge bool) {
	hc := f.homes[i]
	var w *slotWindow
	for k := 0; k < maxSlotWindows; k++ {
		if k == hc.windowCount {
			if pagesPerChunk > hugeSequenceBit-f.nextLogicalPage {
				panic(fmt.Sprintf("logical page sequence exhausted for a window for %v", fr))
			}
			start := (f.nextLogicalPage + pagesPerChunk - 1) / pagesPerChunk * pagesPerChunk
			f.nextLogicalPage = start + pagesPerChunk
			hc.windows[k].start = start
			hc.windowCount++
		}
		if hc.windows[k].fresh.IsEmptyRange(fr) {
			w = &hc.windows[k]
			break
		}
	}
	if w == nil {
		f.assignLogicalPagesLocked(fr, huge)
		return
	}
	w.fresh.InsertRange(fr, unwasteInfo{})
	huge = huge && hostarch.IsHugePageAligned(fr.Start) && hostarch.IsHugePageAligned(fr.End)
	pages := fr.Length() / hostarch.PageSize
	if huge {
		f.nextPageVersion = alignSequence(f.nextPageVersion)
	}
	if pages > hugeSequenceBit-f.nextPageVersion {
		panic(fmt.Sprintf("page version sequence exhausted for %v", fr))
	}
	f.logicalPages.RemoveRange(fr)
	f.logicalPages.InsertRange(fr, logicalPageInfo{
		Allocator:            f.logicalAllocator,
		FirstSequence:        w.start + (fr.Start-i*chunkSize)/hostarch.PageSize,
		VersionAllocator:     f.logicalAllocator,
		FirstVersionSequence: f.nextPageVersion,
		Huge:                 huge,
	})
	f.nextPageVersion += pages
}

// ErrSlotsInUse is returned by AllocateAt when a page of the range it is
// asked to allocate is in use.
var ErrSlotsInUse = errors.New("home slots are in use")

// errNoHome is returned by ReserveHome when the file has no home to spare.
var errNoHome = errors.New("no home chunk to spare")

// HomesEnabled reports whether f has home chunks to reserve; see
// MemoryFileOpts.MaxHomes.
func (f *MemoryFile) HomesEnabled() bool {
	return f.opts.MaxHomes > 0 && f.backing == nil
}

// ReserveHome reserves a home chunk for owner and returns its range: a chunk
// whose slots only AllocateAt allocates, of which owner allocates those that
// are free. Its pages are uncommitted and accounted to no one until they are
// allocated. It fails if f has no home to spare.
func (f *MemoryFile) ReserveHome(owner HomeOwner) (memmap.FileRange, error) {
	if owner == 0 || !f.HomesEnabled() {
		return memmap.FileRange{}, errNoHome
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.reserveHomeLocked(owner)
	if err != nil {
		return memmap.FileRange{}, err
	}
	return chunkRange(i), nil
}

// reserveHomeLocked reserves a home chunk for owner, which may be zero, and
// returns its index.
//
// Preconditions: f.mu must be locked; f.HomesEnabled().
func (f *MemoryFile) reserveHomeLocked(owner HomeOwner) (uint64, error) {
	if n := len(f.freeHomes); n != 0 {
		i := f.freeHomes[n-1]
		f.freeHomes = f.freeHomes[:n-1]
		f.homes[i].owner = owner
		return i, nil
	}
	if len(f.homes) >= f.opts.MaxHomes {
		return 0, errNoHome
	}
	i := uint64(len(f.chunksLoad()))
	// Its pages stay void in both unfree sets: free slots.
	if err := f.appendChunksLocked(1, false /* huge */, homeChunkNode); err != nil {
		return 0, err
	}
	if f.homes == nil {
		f.homes = make(map[uint64]*homeChunk)
	}
	f.homes[i] = &homeChunk{owner: owner}
	return i, nil
}

// DisownHome gives up owner's ownership of the home chunk home. A home with
// no pages left is spared for another owner; one whose pages others still
// use is spared once they are all released.
func (f *MemoryFile) DisownHome(owner HomeOwner, home memmap.FileRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hc := f.homes[home.Start/chunkSize]
	if hc == nil || hc.owner != owner {
		panic(fmt.Sprintf("DisownHome(%d, %v): not its home", owner, home))
	}
	hc.owner = 0
	f.spareHomeIfEmptyLocked(home.Start / chunkSize)
}

// spareHomeIfEmptyLocked spares the home chunk i for another owner if it has
// neither an owner nor pages.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) spareHomeIfEmptyLocked(i uint64) {
	hc := f.homes[i]
	if hc.owner != 0 || hc.pages != 0 {
		return
	}
	// Spared, it starts over.
	for k := range hc.windows[:hc.windowCount] {
		hc.windows[k].fresh.RemoveAll()
	}
	hc.windowCount = 0
	if hc.hasUnit {
		homes := f.unitHomes[hc.unit]
		for j, home := range homes {
			if home == i {
				homes = append(homes[:j], homes[j+1:]...)
				break
			}
		}
		if len(homes) == 0 {
			delete(f.unitHomes, hc.unit)
		} else {
			f.unitHomes[hc.unit] = homes
		}
		hc.hasUnit = false
	}
	f.freeHomes = append(f.freeHomes, i)
	f.remapSparedHomeLocked(i)
}

// remapSparedHomeLocked maps the home chunk i, which has neither an owner nor
// pages, afresh where it is mapped, and its huge view if it has one. The host
// keeps the page tables of the Sentry's mapping of pages once they are gone,
// and an address space's pages spread over its home by their addresses, so
// that a home that address spaces held one after another, as a build's
// processes do, keeps up to 2 MiB of them, however few pages each held.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) remapSparedHomeLocked(i uint64) {
	chunks := f.chunksLoad()
	if f.file == nil || f.backing != nil || i >= uint64(len(chunks)) || chunks[i].mapping == 0 {
		return
	}
	if err := f.remapChunkLocked(chunks[i].mapping, i*chunkSize, chunkSize); err != nil {
		log.Warningf("MemoryFile: mapping spared home chunk %d again: %v", i, err)
		return
	}
	f.madviseChunkMapping(chunks[i].mapping, chunkSize, false)
	if view := f.homes[i].hugeView; view != 0 {
		if err := f.remapChunkLocked(view, i*chunkSize, chunkSize); err != nil {
			log.Warningf("MemoryFile: mapping the huge view of spared home chunk %d again: %v", i, err)
			return
		}
		adviseHugeView(view, chunkSize, f.opts.AdviseHugepage)
	}
}

// unitSlots returns the window of the residency unit of length bytes whose
// pages runs name, and the offset of the unit's slots in a home of that
// window, if the unit's pages are at the slots their identities give: the
// page at each position has the identity of the unit's first slot plus the
// position, and that first slot starts a unit of length bytes.
func unitSlots(runs []SparseChunkRun, length uint64) (unitWindow, uint64, bool) {
	if len(runs) == 0 {
		return unitWindow{}, 0, false
	}
	first := runs[0]
	if first.Page.Sequence < uint64(first.Position) {
		return unitWindow{}, 0, false
	}
	base := first.Page.Sequence - uint64(first.Position)
	for _, run := range runs[1:] {
		if run.Page.Allocator != first.Page.Allocator || run.Page.Sequence < uint64(run.Position) || run.Page.Sequence-uint64(run.Position) != base {
			return unitWindow{}, 0, false
		}
	}
	if base%(length/hostarch.PageSize) != 0 {
		return unitWindow{}, 0, false
	}
	window := base / pagesPerChunk * pagesPerChunk
	return unitWindow{allocator: first.Page.Allocator, window: window}, (base - window) * hostarch.PageSize, true
}

// allocateUnit allocates length bytes for a residency unit whose pages runs
// name, as Allocate does with opts: at the unit's slots in a home of its
// window, where its pages' identities give their slots (see unitSlots) and the
// file has a home to spare, and as Allocate would otherwise. Its caller gives
// the pages their identities.
func (f *MemoryFile) allocateUnit(runs []SparseChunkRun, length uint64, opts AllocOpts) (memmap.FileRange, error) {
	key, off, ok := unitSlots(runs, length)
	if !ok || !f.HomesEnabled() {
		return f.Allocate(length, opts)
	}
	opts.callerIdentities = true
	at := func(i uint64) memmap.FileRange {
		return memmap.FileRange{Start: i*chunkSize + off, End: i*chunkSize + off + length}
	}
	f.mu.Lock()
	homes := append([]uint64(nil), f.unitHomes[key]...)
	f.mu.Unlock()
	for _, i := range homes {
		fr, err := f.AllocateAt(at(i), opts)
		if err == nil || !errors.Is(err, ErrSlotsInUse) {
			return fr, err
		}
	}
	f.mu.Lock()
	i, err := f.reserveHomeLocked(0)
	if err == nil {
		hc := f.homes[i]
		hc.unit, hc.hasUnit = key, true
		if f.unitHomes == nil {
			f.unitHomes = make(map[unitWindow][]uint64)
		}
		f.unitHomes[key] = append(f.unitHomes[key], i)
	}
	f.mu.Unlock()
	if err == nil {
		if fr, err := f.AllocateAt(at(i), opts); err == nil {
			return fr, nil
		}
		f.mu.Lock()
		f.spareHomeIfEmptyLocked(i)
		f.mu.Unlock()
	}
	return f.Allocate(length, opts)
}

// HomeOf returns the home chunk that off is in, and its owner (zero if it has
// none), if off is in one.
func (f *MemoryFile) HomeOf(off uint64) (memmap.FileRange, HomeOwner, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hc, ok := f.homes[off/chunkSize]
	if !ok {
		return memmap.FileRange{}, 0, false
	}
	return chunkRange(off / chunkSize), hc.owner, true
}

func chunkRange(i uint64) memmap.FileRange {
	return memmap.FileRange{Start: i * chunkSize, End: (i + 1) * chunkSize}
}

// AllocateAt allocates fr, which lies in a home chunk, as Allocate allocates
// pages of the length and options given: initially zeroed, with one reference
// on each page held by the caller. It returns ErrSlotsInUse, and allocates
// nothing, if any page of fr is in use; it waits for any being released. As
// Allocate, it returns the part of fr that it allocated if a ReaderFunc
// fails.
//
// A 2 MiB page (opts.Huge, of 2 MiB-aligned pages) is committed through a
// mapping of its own, so that the host backs it with a huge page although
// the Sentry's mapping of a home is that of a small chunk; and the first
// small page in each 2 MiB of a home is committed through that mapping, so
// that no first access, by the application or by fallocate(2), commits a
// huge page there.
//
// Preconditions: fr is page-aligned, not empty, and in one home chunk; if
// opts.Huge, fr is 2 MiB-aligned.
func (f *MemoryFile) AllocateAt(fr memmap.FileRange, opts AllocOpts) (memmap.FileRange, error) {
	if !fr.WellFormed() || fr.Length() == 0 || !hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) || fr.Start/chunkSize != (fr.End-1)/chunkSize ||
		(opts.Huge && (!hostarch.IsHugePageAligned(fr.Start) || !hostarch.IsHugePageAligned(fr.End))) {
		panic(fmt.Sprintf("invalid home allocation %v", fr))
	}
	huge := opts.Huge && f.opts.ExpectHugepages
	node := f.placementNode(&opts)
	f.mu.Lock()
	waste, bind, err := f.markHomeUsedLocked(fr, opts, huge, node)
	var view uintptr
	if err == nil && huge && len(waste) == 0 {
		view = f.hugeViewLocked(fr.Start / chunkSize)
	}
	f.mu.Unlock()
	if err != nil {
		return memmap.FileRange{}, err
	}
	for _, hugeFR := range bind {
		bindFileRange(f.file.Fd(), hugeFR, int(node-1))
	}
	n, filled, err := f.populateHome(fr, opts, huge, waste, view)
	if err != nil && !filled {
		f.DecRef(fr)
		return memmap.FileRange{}, err
	}
	if opts.ReaderFunc == nil {
		return fr, nil
	}
	if !filled {
		dsts, mappingLease, mapErr := f.MapInternalWithLease(fr, hostarch.Write)
		if mapErr != nil {
			f.DecRef(fr)
			return memmap.FileRange{}, mapErr
		}
		n, err = safemem.ReadFullToBlocks(opts.ReaderFunc, dsts)
		if mappingLease != nil {
			mappingLease.Release()
		}
	}
	if un := uint64(hostarch.Addr(n).RoundDown()); un < fr.Length() {
		// Free what the reader did not fill.
		f.DecRef(memmap.FileRange{Start: fr.Start + un, End: fr.End})
		fr.End = fr.Start + un
	}
	return fr, err
}

// markHomeUsedLocked marks fr, in a home chunk, used, as findAllocatableAndMarkUsed
// marks the pages it allocates. It returns the parts of fr that were waste,
// which the caller must zero, and the 2 MiB ranges of fr that the caller
// must bind to node.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) markHomeUsedLocked(fr memmap.FileRange, opts AllocOpts, huge bool, node int32) (waste, bind []memmap.FileRange, err error) {
	i := fr.Start / chunkSize
	hc, ok := f.homes[i]
	if !ok {
		panic(fmt.Sprintf("AllocateAt(%v): not a home chunk", fr))
	}
	// A release in progress is short: wait for it, rather than send the
	// caller elsewhere.
	for f.releasingHome.Overlaps(fr) {
		f.releaserIdle.Wait()
	}
	// Every page must be a free slot or waste: void in unfreeSmall.
	covered := fr.Start
	for seg := f.unfreeSmall.LowerBoundSegment(fr.Start); seg.Ok() && seg.Start() < fr.End; seg = seg.NextSegment() {
		if seg.Start() > covered || seg.ValuePtr().refs != 0 {
			return nil, nil, ErrSlotsInUse
		}
		covered = seg.End()
	}
	if covered < fr.End {
		return nil, nil, ErrSlotsInUse
	}
	for gap := f.unwasteHome.LowerBoundGap(fr.Start); gap.Ok() && gap.Start() < fr.End; gap = gap.NextGap() {
		if wasteFR := gap.Range().Intersect(fr); wasteFR.Length() != 0 {
			waste = append(waste, wasteFR)
		}
	}
	f.unfreeSmall.MutateFullRange(fr, func(ufseg unfreeIterator) bool {
		ufseg.ValuePtr().refs = 1
		return true
	})
	freePages := fr.Length() / hostarch.PageSize
	for _, wasteFR := range waste {
		f.unwasteHome.Insert(f.unwasteHome.FindGap(wasteFR.Start), wasteFR, unwasteInfo{})
		freePages -= wasteFR.Length() / hostarch.PageSize
	}
	hc.pages += freePages
	// Waste pages are accounted already, as System, and recycled as
	// findAllocatableAndMarkUsed recycles them; free slots are not.
	for _, wasteFR := range waste {
		f.memAcct.MutateFullRange(wasteFR, func(maseg memAcctIterator) bool {
			ma := maseg.ValuePtr()
			if ma.knownCommitted {
				malen := maseg.Range().Length()
				ma.knownCommitted = false
				f.knownCommittedBytes -= malen
				if !f.opts.DisableMemoryAccounting {
					usage.MemoryAccounting.Dec(malen, usage.System, ma.memCgID)
				}
			}
			ma.kind = opts.Kind
			ma.memCgID = opts.MemCgID
			ma.wasteOrReleasing = false
			ma.commitSeq = f.commitSeq
			return true
		})
	}
	for _, freeFR := range subtractRanges(fr, waste) {
		f.memAcct.InsertRange(freeFR, memAcctInfo{
			kind: opts.Kind, memCgID: opts.MemCgID, commitSeq: f.commitSeq,
		})
		f.accountLocked(int64(freeFR.Length()))
	}
	if opts.ReaderFunc == nil && !opts.callerIdentities {
		f.homeIdentitiesLocked(i, fr, huge && opts.LogicalHugePages)
	} else {
		f.assignLogicalPagesLocked(fr, huge && opts.LogicalHugePages)
	}
	if node != 0 {
		if hc.nodes == nil {
			hc.nodes = make([]int32, chunkSize/hostarch.HugePageSize)
		}
		for start := hostarch.HugePageRoundDown(fr.Start); start < fr.End; start += hostarch.HugePageSize {
			if k := (start - i*chunkSize) / hostarch.HugePageSize; hc.nodes[k] != node {
				hc.nodes[k] = node
				bind = append(bind, memmap.FileRange{Start: start, End: start + hostarch.HugePageSize})
			}
		}
	}
	return waste, bind, nil
}

// populateHome commits what AllocateAt commits of fr, which it has just
// marked used, and zeroes its waste parts. 2 MiB pages whose slots were all
// free it commits through view, the address of the huge view of fr's home, or
// a mapping of their own if that is 0; and if opts has a ReaderFunc, it fills
// them through it too: filled is true then, n is how many bytes the
// ReaderFunc filled, and err is its error.
func (f *MemoryFile) populateHome(fr memmap.FileRange, opts AllocOpts, huge bool, waste []memmap.FileRange, view uintptr) (n uint64, filled bool, err error) {
	if f.file == nil {
		return 0, false, nil
	}
	if huge && len(waste) == 0 {
		// A 2 MiB page whose slots were all free: commit a huge page.
		var committed bool
		if view != 0 {
			n, committed, err = populateHugeView(mappingSlice(view+uintptr(fr.Start%chunkSize), uintptr(fr.Length())), opts.ReaderFunc)
		} else {
			n, committed, err = populateHugeFileRange(f.file.Fd(), fr, f.opts.AdviseHugepage, opts.ReaderFunc)
		}
		if !committed || opts.ReaderFunc != nil {
			return n, committed, err
		}
	}
	if len(waste) != 0 {
		for _, wasteFR := range waste {
			dsts, lease, err := f.MapInternalWithLease(wasteFR, hostarch.Write)
			if err != nil {
				return 0, false, err
			}
			_, err = safemem.ZeroSeq(dsts)
			if lease != nil {
				lease.Release()
			}
			if err != nil {
				return 0, false, err
			}
		}
	}
	if !huge && f.opts.AdviseNoHugepage {
		// Commit a small page in each 2 MiB of fr through the Sentry's
		// mapping, which is advised MADV_NOHUGEPAGE: the host commits no huge
		// page where it holds a page already.
		for start := fr.Start; start < fr.End; start = hostarch.HugePageRoundDown(start) + hostarch.HugePageSize {
			dsts, lease, err := f.MapInternalWithLease(memmap.FileRange{Start: start, End: start + hostarch.PageSize}, hostarch.Write)
			if err != nil {
				return 0, false, err
			}
			dsts.Head().ToSlice()[0] = 0
			if lease != nil {
				lease.Release()
			}
		}
	}
	switch opts.Mode {
	case AllocateUncommitted, AllocateCallerIndirectCommit:
	case AllocateAndCommit:
		if !huge {
			return 0, false, f.commitFile(fr)
		}
	case AllocateAndWritePopulate:
		if huge {
			return 0, false, nil
		}
		dsts, lease, err := f.MapInternalWithLease(fr, hostarch.Write)
		if err != nil {
			return 0, false, err
		}
		if canPopulate() {
			for rem := dsts; !rem.IsEmpty() && tryPopulate(rem.Head()); rem = rem.Tail() {
			}
		}
		if lease != nil {
			lease.Release()
		}
	default:
		panic(fmt.Sprintf("unknown AllocOpts.Mode %d", opts.Mode))
	}
	return 0, false, nil
}

// subtractRanges returns the parts of fr outside holes, which are sorted, do
// not overlap, and lie in fr.
func subtractRanges(fr memmap.FileRange, holes []memmap.FileRange) []memmap.FileRange {
	var parts []memmap.FileRange
	at := fr.Start
	for _, hole := range holes {
		if hole.Start > at {
			parts = append(parts, memmap.FileRange{Start: at, End: hole.Start})
		}
		at = hole.End
	}
	if at < fr.End {
		parts = append(parts, memmap.FileRange{Start: at, End: fr.End})
	}
	return parts
}

// mergeHomeWasteLocked moves the waste of home chunks into unwasteSmall, for
// an export of f's state that saves unwasteSmall alone, and returns a function
// that moves it back.
//
// Preconditions: f.mu must be locked, until the returned function is called.
func (f *MemoryFile) mergeHomeWasteLocked() func() {
	var waste []memmap.FileRange
	for gap := f.unwasteHome.FirstLargeEnoughGap(1); gap.Ok(); gap = gap.NextLargeEnoughGap(1) {
		waste = append(waste, gap.Range())
	}
	for _, fr := range waste {
		f.unwasteSmall.RemoveFullRange(fr)
	}
	return func() {
		for _, fr := range waste {
			f.unwasteSmall.Insert(f.unwasteSmall.FindGap(fr.Start), fr, unwasteInfo{})
		}
	}
}

// loadHomesLocked rebuilds the state of the home chunks that f's chunks mark,
// after f's state is loaded: the waste that unwasteSmall holds in them moves
// to unwasteHome, and none has an owner.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) loadHomesLocked() {
	f.homes = nil
	f.freeHomes = nil
	chunks := f.chunksLoad()
	for i := range chunks {
		if !chunks[i].isHome() {
			continue
		}
		cr := chunkRange(uint64(i))
		hc := &homeChunk{}
		var waste []memmap.FileRange
		for gap := f.unwasteSmall.LowerBoundGap(cr.Start); gap.Ok() && gap.Start() < cr.End; gap = gap.NextGap() {
			if fr := gap.Range().Intersect(cr); fr.Length() != 0 {
				waste = append(waste, fr)
			}
		}
		for _, fr := range waste {
			f.unwasteSmall.Insert(f.unwasteSmall.FindGap(fr.Start), fr, unwasteInfo{})
			f.unwasteHome.RemoveFullRange(fr)
			hc.pages += fr.Length() / hostarch.PageSize
		}
		f.unfreeSmall.VisitRange(cr, func(ufseg unfreeIterator) bool {
			if ufseg.ValuePtr().refs != 0 {
				hc.pages += ufseg.Range().Length() / hostarch.PageSize
			}
			return true
		})
		if f.homes == nil {
			f.homes = make(map[uint64]*homeChunk)
		}
		f.homes[uint64(i)] = hc
		f.spareHomeIfEmptyLocked(uint64(i))
	}
}
