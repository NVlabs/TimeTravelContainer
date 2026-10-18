// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// Folding.
//
// A fork leaves an address space's private pages in more than one home of a
// block (see allocatePrivateLocked): a copy made while a fork still shares the
// page at its slot goes to another layer, and a fork's pages stay in its
// parent's homes until it writes them. Once what kept them apart is gone, as
// when the fork or the parent is, the platform still maps the address space's
// private pages with a host mapping per run in each home, however finely the
// homes interleave. A fold moves them, at their slots, into the home that
// holds most of them, which the address space adopts if it was a released
// address space's: a page moves with its identity, version and protection,
// its content being the same. See
// g3doc/proposals/ttc_address_ordered_memory.md.

// FoldRequestHook is called, if set, with a MemoryFile whose address spaces
// may have private pages to fold: when an address space releases its pages,
// which others may then hold alone, and when the host has no room for another
// of an address space's mappings.
var FoldRequestHook func(*pgalloc.MemoryFile)

// requestFolds calls FoldRequestHook with mf.
func requestFolds(mf *pgalloc.MemoryFile) {
	if hook := FoldRequestHook; hook != nil && mf != nil {
		hook(mf)
	}
}

// maxFoldBlocksPerStep is how many blocks a FoldStep plans at most, so that it
// holds an address space's activeMu briefly however little it moves.
const maxFoldBlocksPerStep = 8

// foldPiece is a range of an address space's private pages that a fold moves,
// and the file range that the address space's pma mapped it to when the fold
// was planned.
type foldPiece struct {
	ar  hostarch.AddrRange
	src memmap.FileRange
}

// foldState is where an address space's folds are; see FoldStep.
type foldState struct {
	// foldable is true if the address space may have private pages to fold:
	// it placed pages in layers or outside its homes, forked or was forked,
	// or a fold left pages where they were.
	foldable bool

	// residue is true if the current pass left pages out of their home.
	residue bool

	// cursor is where the next step starts.
	cursor hostarch.Addr

	// block is the block the current plan folds, target the home it folds
	// into, and pieces what it still has to move.
	block  hostarch.AddrRange
	target memmap.FileRange
	pieces []foldPiece
}

// markFoldableLocked notes that mm may have private pages to fold.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) markFoldableLocked() {
	mm.fold.foldable = true
}

// FoldStep moves at most about limit bytes of mm's private pages to their
// slots in the homes that hold most of them, from where its last call
// stopped. It returns how many bytes it moved, and true once it has been
// through all of mm's addresses: a later call starts over, if mm may have
// private pages to fold then.
func (mm *MemoryManager) FoldStep(limit uint64) (uint64, bool) {
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	fs := &mm.fold
	if mm.homeOwner == 0 || !fs.foldable || mm.users.Load() == 0 {
		return 0, true
	}
	var moved uint64
	for planned := 0; moved < limit && planned < maxFoldBlocksPerStep; {
		if len(fs.pieces) == 0 && fs.block.Length() == 0 {
			block, ok := mm.nextFoldBlockLocked(fs.cursor)
			if !ok {
				fs.cursor = 0
				fs.foldable = fs.residue
				fs.residue = false
				return moved, true
			}
			mm.planFoldLocked(block)
			planned++
		}
		moved += mm.foldPiecesLocked(limit - moved)
		if len(fs.pieces) != 0 {
			return moved, false
		}
		mm.finishFoldLocked()
	}
	return moved, false
}

// nextFoldBlockLocked returns the first block of homeBlockSize, from the one
// at from on, in which mm has private pages.
//
// Preconditions: mm.activeMu must be locked.
func (mm *MemoryManager) nextFoldBlockLocked(from hostarch.Addr) (hostarch.AddrRange, bool) {
	for pseg := mm.pmas.LowerBoundSegment(from); pseg.Ok(); pseg = pseg.NextSegment() {
		if p := pseg.ValuePtr(); !p.private || p.file != mm.mf {
			continue
		}
		start := max(pseg.Start(), from) &^ (homeBlockSize - 1)
		return hostarch.AddrRange{Start: start, End: homeBlockEnd(start)}, true
	}
	return hostarch.AddrRange{}, false
}

// homePlace is where some of an address space's private pages in a block are:
// a home chunk, its owner, and how many of them it holds.
type homePlace struct {
	home  memmap.FileRange
	owner pgalloc.HomeOwner
	pages uint64
}

// planFoldLocked plans the fold of block: it picks the home that holds most of
// mm's private pages there, of those that mm owns or no one does, adopting it
// if no one does, and lists the pages elsewhere to move into it.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) planFoldLocked(block hostarch.AddrRange) {
	fs := &mm.fold
	fs.block, fs.target, fs.pieces = block, memmap.FileRange{}, nil
	b := uint64(block.Start) / homeBlockSize
	places := make(map[uint64]*homePlace)
	notHome := make(map[uint64]bool)
	mm.forEachPrivatePieceLocked(block, func(ar hostarch.AddrRange, fr memmap.FileRange) {
		chunk := fr.Start / homeBlockSize
		if notHome[chunk] {
			return
		}
		place := places[chunk]
		if place == nil {
			home, owner, ok := mm.mf.HomeOf(fr.Start)
			if !ok {
				notHome[chunk] = true
				return
			}
			place = &homePlace{home: home, owner: owner}
			places[chunk] = place
		}
		place.pages += fr.Length() / hostarch.PageSize
	})
	var target *homePlace
	for _, place := range places {
		if place.owner != mm.homeOwner && place.owner != 0 {
			continue
		}
		if target == nil || place.pages > target.pages || (place.pages == target.pages && place.home.Start < target.home.Start) {
			target = place
		}
	}
	if target == nil {
		homes := mm.homes[b]
		if len(homes) == 0 {
			// mm has no home here to fold into: its pages stay where they are.
			fs.residue = true
			return
		}
		target = &homePlace{home: homes[0], owner: mm.homeOwner}
	}
	if target.owner == 0 {
		if mm.mf.AdoptHome(mm.homeOwner, target.home) != nil {
			fs.residue = true
			return
		}
		target.owner = mm.homeOwner
		if mm.homes == nil {
			mm.homes = make(map[uint64][]memmap.FileRange)
		}
		mm.homes[b] = append(mm.homes[b], target.home)
	}
	// mm places pages in the target first from now on.
	homes := mm.homes[b]
	for i, home := range homes {
		if home == target.home {
			copy(homes[1:i+1], homes[:i])
			homes[0] = home
			break
		}
	}
	fs.target = target.home
	mm.forEachPrivatePieceLocked(block, func(ar hostarch.AddrRange, fr memmap.FileRange) {
		if !fs.target.Contains(fr.Start) {
			fs.pieces = append(fs.pieces, foldPiece{ar: ar, src: fr})
		}
	})
}

// forEachPrivatePieceLocked calls fn with each range of ar that a private pma
// of mm maps to mm.mf, each within one chunk of the file, and the file range it
// maps it to, in order.
//
// Preconditions: mm.activeMu must be locked.
func (mm *MemoryManager) forEachPrivatePieceLocked(ar hostarch.AddrRange, fn func(hostarch.AddrRange, memmap.FileRange)) {
	for pseg := mm.pmas.LowerBoundSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		if p := pseg.ValuePtr(); !p.private || p.file != mm.mf {
			continue
		}
		pieceAR := pseg.Range().Intersect(ar)
		fr := pseg.fileRangeOf(pieceAR)
		for fr.Length() != 0 {
			end := min(fr.End, (fr.Start&^(homeBlockSize-1))+homeBlockSize)
			n := hostarch.Addr(end - fr.Start)
			fn(hostarch.AddrRange{Start: pieceAR.Start, End: pieceAR.Start + n}, memmap.FileRange{Start: fr.Start, End: end})
			pieceAR.Start += n
			fr.Start = end
		}
	}
}

// foldPiecesLocked moves the planned pieces, at most about limit bytes of
// them, into the slots of the fold's target that are free, and returns how
// many bytes it moved.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) foldPiecesLocked(limit uint64) uint64 {
	fs := &mm.fold
	var moved uint64
	for len(fs.pieces) != 0 && moved < limit {
		piece := fs.pieces[0]
		fs.pieces = fs.pieces[1:]
		slots := memmap.FileRange{Start: fs.target.Start + uint64(piece.ar.Start-fs.block.Start)}
		slots.End = slots.Start + piece.src.Length()
		free := mm.mf.FreeSlots(slots)
		if len(free) == 0 || free[0] != slots {
			// What a page still holds stays.
			fs.residue = true
		}
		for _, dst := range free {
			delta := dst.Start - slots.Start
			src := memmap.FileRange{Start: piece.src.Start + delta, End: piece.src.Start + delta + dst.Length()}
			ar := hostarch.AddrRange{Start: piece.ar.Start + hostarch.Addr(delta), End: piece.ar.Start + hostarch.Addr(delta+dst.Length())}
			if mm.movePrivateLocked(ar, src, dst) {
				moved += dst.Length()
			} else {
				fs.residue = true
			}
		}
	}
	return moved
}

// movePrivateLocked moves the private pages that mm maps at ar to src, if mm
// still does and holds them alone, to dst, and returns true if it did.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) movePrivateLocked(ar hostarch.AddrRange, src, dst memmap.FileRange) bool {
	pseg := mm.pmas.FindSegment(ar.Start)
	if !pseg.Ok() || !pseg.Range().IsSupersetOf(ar) {
		return false
	}
	if p := pseg.ValuePtr(); !p.private || p.file != mm.mf || pseg.fileRangeOf(ar) != src {
		return false
	}
	if !mm.mf.Movable(src) {
		return false
	}
	// The application must not write the pages while they are copied, nor
	// reach the old ones once they are freed.
	mm.unmapASLocked(ar)
	if err := mm.mf.MovePages(src, dst); err != nil {
		return false
	}
	pseg = mm.pmas.Isolate(pseg, ar)
	p := pseg.ValuePtr()
	p.off = dst.Start
	p.internalMappings = safemem.BlockSeq{}
	p.huge = mm.mf.IsHugePages(dst)
	mm.mf.DecRef(src)
	return true
}

// finishFoldLocked ends the fold of a block: it merges the pmas that the
// fold's moves made adjacent, and gives up mm's other homes in the block that
// no longer hold any of mm's pages.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) finishFoldLocked() {
	fs := &mm.fold
	block := fs.block
	fs.cursor, fs.block, fs.target, fs.pieces = block.End, hostarch.AddrRange{}, memmap.FileRange{}, nil
	if block.Length() == 0 {
		return
	}
	mm.pmas.MergeInsideRange(block)
	mm.pmas.MergeOutsideRange(block)
	b := uint64(block.Start) / homeBlockSize
	homes := mm.homes[b]
	if len(homes) < 2 {
		return
	}
	used := make(map[memmap.FileRange]bool)
	mm.forEachPrivatePieceLocked(block, func(_ hostarch.AddrRange, fr memmap.FileRange) {
		for _, home := range homes {
			if home.Contains(fr.Start) {
				used[home] = true
			}
		}
	})
	kept := homes[:1]
	for _, home := range homes[1:] {
		if used[home] {
			kept = append(kept, home)
			continue
		}
		mm.mf.DisownHome(mm.homeOwner, home)
	}
	mm.homes[b] = kept
}
