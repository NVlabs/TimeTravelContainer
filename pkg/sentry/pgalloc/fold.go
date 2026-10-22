// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"errors"
	"fmt"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Folding.
//
// A fork leaves an address space's private pages in more than one home: a
// write while the fork shares a page puts the copy in another layer of its
// block, at the same slot, and a fork that writes puts its copies in its own
// homes while the pages it did not write stay in its parent's. Once the pages
// that kept them apart are gone, a fold moves the address space's private
// pages into one home, at their slots, so that they are in address order
// again. A page moves with its identity and version, as its content does not
// change. See g3doc/proposals/ttc_address_ordered_memory.md.

// ErrNotMovable is returned by MovePages when something besides its caller
// holds a page of the range it is asked to move.
var ErrNotMovable = errors.New("pages are held by others")

// errHomeOwned is returned by AdoptHome for a home chunk that has an owner.
var errHomeOwned = errors.New("home chunk has an owner")

// Movable returns true if MovePages may move fr's pages: each has one
// reference, no device uses it for DMA, and none is still to be loaded.
func (f *MemoryFile) Movable(fr memmap.FileRange) bool {
	f.mu.Lock()
	unique := f.hasUniqueRefLocked(fr)
	f.mu.Unlock()
	return unique && !f.DMAPinned(fr) && len(f.pendingPageLoads(fr)) == 0
}

// MovePages moves the pages of src, of which its caller holds the only
// reference, to dst, free slots of a home chunk: it allocates dst as
// AllocateAt does, copies src's contents into it, and gives it src's
// identities and versions, 2 MiB pages staying 2 MiB pages where dst is
// aligned as src is. The caller then holds dst, with one reference, as it held
// src, which it must DecRef once it no longer maps it.
//
// It allocates nothing, and returns ErrNotMovable, if anything else holds a
// page of src (see Movable); and ErrSlotsInUse if a slot of dst is in use.
//
// Preconditions:
//   - src and dst are page-aligned, of the same length, not empty; dst is in
//     one home chunk.
//   - Nothing writes src while MovePages copies it: its caller's own mappings
//     of it are not writable.
func (f *MemoryFile) MovePages(src, dst memmap.FileRange) error {
	if !src.WellFormed() || src.Length() == 0 || src.Length() != dst.Length() ||
		!hostarch.IsPageAligned(src.Start) || !hostarch.IsPageAligned(src.End) || !hostarch.IsPageAligned(dst.Start) {
		panic(fmt.Sprintf("invalid page move %v to %v", src, dst))
	}
	if !f.Movable(src) {
		return ErrNotMovable
	}
	f.mu.Lock()
	// A 2 MiB page that the move takes in part is base pages from now on, in
	// both places.
	f.splitPartlyCoveredHugePagesLocked(src)
	huge := hostarch.HugePageOffset(src.Start) == hostarch.HugePageOffset(dst.Start) && f.isHugeLocked(src)
	opts := AllocOpts{Huge: huge, LogicalHugePages: huge, callerIdentities: true}
	if seg := f.memAcct.FindSegment(src.Start); seg.Ok() {
		opts.Kind, opts.MemCgID = seg.ValuePtr().kind, seg.ValuePtr().memCgID
	}
	// 2 MiB pages read through their home's huge view, with one page table
	// entry each.
	srcs, viewed := safemem.BlockSeq{}, false
	if huge {
		srcs, viewed = f.hugeMappingLocked(src)
	}
	f.mu.Unlock()
	var lease memmap.InternalMappingLease
	if !viewed {
		var err error
		if srcs, lease, err = f.MapInternalWithLease(src, hostarch.Read); err != nil {
			return err
		}
	}
	reader := safemem.BlockSeqReader{Blocks: srcs}
	opts.ReaderFunc = reader.ReadToBlocks
	fr, err := f.AllocateAt(dst, opts)
	if lease != nil {
		lease.Release()
	}
	if err != nil {
		if fr.Length() != 0 {
			f.DecRef(fr)
		}
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moveIdentitiesLocked(src, dst)
	return nil
}

// moveIdentitiesLocked gives dst the identities and versions of src, which has
// the same length. A page of src without an identity leaves its counterpart
// in dst the one it has.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) moveIdentitiesLocked(src, dst memmap.FileRange) {
	aligned := hostarch.HugePageOffset(src.Start) == hostarch.HugePageOffset(dst.Start)
	for _, run := range f.logicalPageRangesLocked(src) {
		start := dst.Start + (run.Range.Start - src.Start)
		to := memmap.FileRange{Start: start, End: start + run.Range.Length()}
		f.logicalPages.RemoveRange(to)
		f.logicalPages.InsertRange(to, logicalPageInfo{
			Allocator:            run.First.Allocator,
			FirstSequence:        run.First.Sequence,
			VersionAllocator:     run.FirstVersion.Allocator,
			FirstVersionSequence: run.FirstVersion.Sequence,
			Huge:                 run.Huge && aligned,
		})
	}
}

// SetFoldNotify has f call fn when references that others held on its pages
// go, as a capture's do when it is closed, so that address spaces may then hold
// them alone and fold them.
func (f *MemoryFile) SetFoldNotify(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.foldNotify = fn
}

// notifyFold calls the function that SetFoldNotify set, if any.
func (f *MemoryFile) notifyFold() {
	f.mu.Lock()
	fn := f.foldNotify
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// FreeSlots returns the parts of fr, in a home chunk, that AllocateAt may
// allocate: those no page uses.
func (f *MemoryFile) FreeSlots(fr memmap.FileRange) []memmap.FileRange {
	f.mu.Lock()
	defer f.mu.Unlock()
	var free []memmap.FileRange
	at := fr.Start
	for seg := f.unfreeSmall.LowerBoundSegment(fr.Start); seg.Ok() && seg.Start() < fr.End; seg = seg.NextSegment() {
		if seg.ValuePtr().refs == 0 {
			continue
		}
		if seg.Start() > at {
			free = append(free, memmap.FileRange{Start: at, End: seg.Start()})
		}
		at = seg.End()
	}
	if at < fr.End {
		free = append(free, memmap.FileRange{Start: at, End: fr.End})
	}
	return free
}

// AdoptHome makes owner the owner of home, a home chunk that has none, an
// orphan, so that owner places its pages there; see ReserveHome.
func (f *MemoryFile) AdoptHome(owner HomeOwner, home memmap.FileRange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := home.Start / chunkSize
	hc := f.homes[i]
	if hc == nil {
		return fmt.Errorf("AdoptHome(%v): not a home chunk", home)
	}
	if hc.owner != 0 {
		return errHomeOwned
	}
	hc.owner = owner
	for j, free := range f.freeHomes {
		if free == i {
			f.freeHomes = append(f.freeHomes[:j], f.freeHomes[j+1:]...)
			break
		}
	}
	return nil
}
