// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
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

// FoldRequestHook is called, if set, when address spaces on a MemoryFile may
// have private pages to fold: when an address space releases its pages, which
// others of its fork group may then hold alone, and when the host has no room
// for another of an address space's mappings. group is the fork group of the
// address spaces that may (see MemoryManager.FoldGroup), or 0 if any may.
var FoldRequestHook func(mf *pgalloc.MemoryFile, group uint64)

// FoldRetryHook is called, if set, with a MemoryFile whose address spaces
// have candidates for collapse that have yet to wait collapseAge, so that a
// fold pass looks at them once they have.
var FoldRetryHook func(*pgalloc.MemoryFile)

// requestFolds calls FoldRequestHook with mf and group.
func requestFolds(mf *pgalloc.MemoryFile, group uint64) {
	if hook := FoldRequestHook; hook != nil && mf != nil {
		hook(mf, group)
	}
}

// requestFoldsLater calls FoldRetryHook with mf.
func requestFoldsLater(mf *pgalloc.MemoryFile) {
	if hook := FoldRetryHook; hook != nil && mf != nil {
		hook(mf)
	}
}

// foldGroups numbers fork groups; see FoldGroup.
var foldGroups atomicbitops.Uint64

// newFoldGroup returns a new fork group.
func newFoldGroup() uint64 {
	return foldGroups.Add(1)
}

// FoldGroup returns mm's fork group: the address spaces that forks and live
// forks made from one another. Only they share private pages with mm, and
// only their releases may leave mm holding alone pages it shared. It is 0,
// any, for an address space restored from a save and those made from it: a
// save keeps no fork groups, and such an address space may share pages with
// any other restored with it. Its releases ask every address space to fold,
// and it folds when all are asked to.
func (mm *MemoryManager) FoldGroup() uint64 {
	return mm.foldGroup
}

// maxFoldBlocksPerStep is how many blocks a FoldStep plans at most, so that it
// holds an address space's activeMu briefly however little it moves.
const maxFoldBlocksPerStep = 8

// foldResidueWait is how long an address space waits, after two passes in a
// row that moved nothing and left pages that others held, before a pass looks
// at them again, however often it is asked to: what a live fork shares with
// its source changes only as they write, and a pass holds the address space's
// activeMu while it plans and looks. A live fork rewriting a quarter of 1 GiB
// while its source forked and execed took 20% longer than before homes without
// it, as each exec asked their group to fold, and 4% with it. One such pass is
// common, and says little: a fork may share what a fold had yet to move.
var foldResidueWait = 10 * time.Second

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

	// moved is how many bytes the current pass moved; idle is how many
	// passes in a row moved nothing and left pages out of their home, and
	// idleAt when the last of them ended. deferred is true if a pass was
	// asked for that waits for foldResidueWait after idleAt; see
	// FoldDeferred.
	moved    uint64
	idle     int
	idleAt   time.Time
	deferred bool

	// cursor is where the next step starts.
	cursor hostarch.Addr

	// block is the block the current plan folds, target the home it folds
	// into, and pieces what it still has to move.
	block  hostarch.AddrRange
	target memmap.FileRange
	pieces []foldPiece

	// collapse holds the aligned 2 MiB ranges that may be collapsed into 2
	// MiB pages, by start; see collapse.go.
	collapse map[hostarch.Addr]collapseCandidate

	// collapsing is true once the current pass has listed the candidates for
	// collapse it visits, and unvisited are those it has yet to.
	collapsing bool
	unvisited  []hostarch.Addr
}

// markFoldableLocked notes that mm may have private pages to fold.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) markFoldableLocked() {
	mm.fold.foldable = true
}

// FoldStep moves at most about limit bytes of mm's private pages to their
// slots in the homes that hold most of them, from where its last call
// stopped. It returns how much work it did, counting the bytes it moved and
// a page for each range it looked at but left, and true once it has been
// through all of mm's addresses: a later call starts over, if mm may have
// private pages to fold then, and foldResidueWait has passed since the last
// pass if the last two moved nothing and left pages that others held (see
// FoldDeferred).
func (mm *MemoryManager) FoldStep(limit uint64) (uint64, bool) {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	fs := &mm.fold
	if mm.homeOwner == 0 || mm.users.Load() == 0 {
		return 0, true
	}
	var moved uint64
	// Candidates for collapse are few. Those that have yet to wait
	// collapseAge, or wait for discarded pages to be written again, stay, for
	// a later pass: see CollapseWaiting.
	if !fs.collapsing {
		fs.collapsing = true
		fs.unvisited = fs.unvisited[:0]
		for start, c := range fs.collapse {
			if time.Since(c.since) >= collapseAge {
				fs.unvisited = append(fs.unvisited, start)
			}
		}
	}
	for len(fs.unvisited) != 0 {
		if moved >= limit {
			return moved, false
		}
		start := fs.unvisited[0]
		fs.unvisited = fs.unvisited[1:]
		collapsed, wait := mm.collapseLocked(hostarch.AddrRange{Start: start, End: start + hostarch.HugePageSize})
		if collapsed {
			moved += hostarch.HugePageSize
		}
		if !wait {
			delete(fs.collapse, start)
		}
	}
	if !fs.foldable {
		fs.collapsing = false
		return moved, true
	}
	if fs.cursor == 0 && fs.block.Length() == 0 {
		// A pass starts.
		if fs.idle >= 2 && time.Since(fs.idleAt) < foldResidueWait {
			fs.deferred = true
			fs.collapsing = false
			return moved, true
		}
		fs.deferred = false
		fs.moved = 0
	}
	for planned := 0; moved < limit && planned < maxFoldBlocksPerStep; {
		if len(fs.pieces) == 0 && fs.block.Length() == 0 {
			block, ok := mm.nextFoldBlockLocked(fs.cursor)
			if !ok {
				fs.cursor = 0
				fs.foldable = fs.residue
				if fs.residue && fs.moved == 0 {
					fs.idle++
					fs.idleAt = time.Now()
				} else {
					fs.idle = 0
				}
				fs.residue = false
				fs.collapsing = false
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

// FoldDeferred returns true if a pass that mm was asked for waits for
// foldResidueWait to pass since its last, which, as the one before it, moved
// nothing and left pages that others held, so that a pass must be asked for
// again then.
func (mm *MemoryManager) FoldDeferred() bool {
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	return mm.fold.deferred
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
// much work it did, as FoldStep counts it: looking at a piece that something
// else still holds costs about as much as moving a page, and while a fork
// lives, most of its pieces are such.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) foldPiecesLocked(limit uint64) uint64 {
	fs := &mm.fold
	var done uint64
	for len(fs.pieces) != 0 && done < limit {
		piece := fs.pieces[0]
		fs.pieces = fs.pieces[1:]
		if !mm.mf.Movable(piece.src) {
			// What a page still holds stays.
			fs.residue = true
			done += hostarch.PageSize
			continue
		}
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
				done += dst.Length()
				fs.moved += dst.Length()
			} else {
				fs.residue = true
			}
		}
		if len(free) == 0 {
			done += hostarch.PageSize
		}
	}
	return done
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
	mm.noteCollapsibleBlockLocked(block)
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
