// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"errors"
	"fmt"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
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
	if n := len(f.freeHomes); n != 0 {
		i := f.freeHomes[n-1]
		f.freeHomes = f.freeHomes[:n-1]
		f.homes[i].owner = owner
		return chunkRange(i), nil
	}
	if len(f.homes) >= f.opts.MaxHomes {
		return memmap.FileRange{}, errNoHome
	}
	i := uint64(len(f.chunksLoad()))
	// Its pages stay void in both unfree sets: free slots.
	if err := f.appendChunksLocked(1, false /* huge */, homeChunkNode); err != nil {
		return memmap.FileRange{}, err
	}
	if f.homes == nil {
		f.homes = make(map[uint64]*homeChunk)
	}
	f.homes[i] = &homeChunk{owner: owner}
	return chunkRange(i), nil
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
	if hc := f.homes[i]; hc.owner == 0 && hc.pages == 0 {
		f.freeHomes = append(f.freeHomes, i)
	}
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
	f.mu.Unlock()
	if err != nil {
		return memmap.FileRange{}, err
	}
	for _, hugeFR := range bind {
		bindFileRange(f.file.Fd(), hugeFR, int(node-1))
	}
	if err := f.populateHome(fr, opts, huge, waste); err != nil {
		f.DecRef(fr)
		return memmap.FileRange{}, err
	}
	if opts.ReaderFunc == nil {
		return fr, nil
	}
	dsts, mappingLease, err := f.MapInternalWithLease(fr, hostarch.Write)
	if err != nil {
		f.DecRef(fr)
		return memmap.FileRange{}, err
	}
	n, err := safemem.ReadFullToBlocks(opts.ReaderFunc, dsts)
	if mappingLease != nil {
		mappingLease.Release()
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
	f.assignLogicalPagesLocked(fr, huge && opts.LogicalHugePages)
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
// marked used, and zeroes its waste parts.
func (f *MemoryFile) populateHome(fr memmap.FileRange, opts AllocOpts, huge bool, waste []memmap.FileRange) error {
	if f.file == nil {
		return nil
	}
	if huge && len(waste) == 0 {
		// A 2 MiB page whose slots were all free: commit a huge page.
		if err := populateHugeFileRange(f.file.Fd(), fr, f.opts.AdviseHugepage); err != nil {
			return err
		}
	}
	if len(waste) != 0 {
		for _, wasteFR := range waste {
			dsts, lease, err := f.MapInternalWithLease(wasteFR, hostarch.Write)
			if err != nil {
				return err
			}
			_, err = safemem.ZeroSeq(dsts)
			if lease != nil {
				lease.Release()
			}
			if err != nil {
				return err
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
				return err
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
			return f.commitFile(fr)
		}
	case AllocateAndWritePopulate:
		if huge {
			return nil
		}
		dsts, lease, err := f.MapInternalWithLease(fr, hostarch.Write)
		if err != nil {
			return err
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
	return nil
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
