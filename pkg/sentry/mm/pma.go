// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"fmt"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safecopy"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// existingPMAsLocked checks that pmas exist for all addresses in ar, and
// support access of type (at, ignorePermissions). If so, it returns an
// iterator to the pma containing ar.Start. Otherwise it returns a terminal
// iterator.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - ar.Length() != 0.
func (mm *MemoryManager) existingPMAsLocked(ar hostarch.AddrRange, at hostarch.AccessType, ignorePermissions bool, needInternalMappings bool) pmaIterator {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
	}

	first := mm.pmas.FindSegment(ar.Start)
	pseg := first
	for pseg.Ok() {
		pma := pseg.ValuePtr()
		perms := pma.effectivePerms
		if ignorePermissions {
			perms = pma.maxPerms
		}
		if !perms.SupersetOf(at) {
			return pmaIterator{}
		}
		if needInternalMappings && pma.internalMappings.IsEmpty() {
			return pmaIterator{}
		}

		if ar.End <= pseg.End() {
			return first
		}
		pseg, _ = pseg.NextNonEmpty()
	}

	// Ran out of pmas before reaching ar.End.
	return pmaIterator{}
}

// existingVecPMAsLocked returns true if pmas exist for all addresses in ars,
// and support access of type (at, ignorePermissions).
//
// Preconditions: mm.activeMu must be locked.
func (mm *MemoryManager) existingVecPMAsLocked(ars hostarch.AddrRangeSeq, at hostarch.AccessType, ignorePermissions bool, needInternalMappings bool) bool {
	for ; !ars.IsEmpty(); ars = ars.Tail() {
		if ar := ars.Head(); ar.Length() != 0 && !mm.existingPMAsLocked(ar, at, ignorePermissions, needInternalMappings).Ok() {
			return false
		}
	}
	return true
}

// getPMAsLocked ensures that pmas exist for all addresses in ar, and support
// access of type at. It returns:
//
//   - An iterator to the pma containing ar.Start. If no pma contains ar.Start,
//     the iterator is unspecified.
//
//   - An iterator to the gap after the last pma containing an address in ar. If
//     pmas exist for no addresses in ar, the iterator is to a gap that begins
//     before ar.Start.
//
//   - An error that is non-nil if pmas exist for only a subset of ar.
//
// If callerIndirectCommit is true, the caller of getPMAsLocked will shortly
// commit all pages in ar without using the caller's page tables, in the same
// sense as pgalloc.AllocateCallerIndirectCommit.
//
// If forPin is true, the caller is Pin, and copy-on-write pmas will be broken
// even if at.Write is false, so that pinned pages remain coherent with the
// application's mappings; compare Linux's
// mm/internal.h:gup_must_unshare(FOLL_PIN|FOLL_LONGTERM).
//
// Preconditions:
//   - mm.mappingMu must be locked.
//   - mm.activeMu must be locked for writing.
//   - ar.Length() != 0.
//   - vseg.Range().Contains(ar.Start).
//   - vmas must exist for all addresses in ar, and support accesses of type at
//     (i.e. permission checks must have been performed against vmas).
func (mm *MemoryManager) getPMAsLocked(ctx context.Context, vseg vmaIterator, ar hostarch.AddrRange, at hostarch.AccessType, callerIndirectCommit, forPin bool) (pmaIterator, pmaGapIterator, error) {
	return mm.getPMAsRemappingLocked(ctx, vseg, ar, at, callerIndirectCommit, forPin, nil)
}

// faultRemap is lent to getPMAsRemappingLocked by a caller that maps ar into
// the AddressSpace as soon as it returns, as the page-fault handler does.
//
// A pma replaced inside ar, as breaking copy-on-write replaces one, is left
// mapped to what it replaced instead of being unmapped: MapFile replaces
// existing mappings, so the unmap would be a second platform round trip on
// every copy-on-write fault for nothing. The references to what was replaced
// wait in decRefs until the caller has mapped ar or unmapped it. Until then
// the old translation can still be read through by the faulting thread's
// siblings, and a page released early can be taken over and written in place
// by the branch it was shared with.
type faultRemap struct {
	ar      hostarch.AddrRange
	decRefs *pendingFileDecRefs

	// inserted spans the pmas that the fault inserted: pages it allocated,
	// copied or translated. Pages they merge with are mapped already, and
	// mapping them again would drop their host page tables; see
	// mapASWindowLocked.
	inserted hostarch.AddrRange
}

// noteInserted records in remap, if any, that the fault inserted pmas for ar.
func (remap *faultRemap) noteInserted(ar hostarch.AddrRange) {
	if remap == nil {
		return
	}
	if remap.inserted.Length() == 0 {
		remap.inserted = ar
		return
	}
	remap.inserted.Start = min(remap.inserted.Start, ar.Start)
	remap.inserted.End = max(remap.inserted.End, ar.End)
}

// release drops the references remap held.
//
// Preconditions: remap.ar has been mapped into the AddressSpace or unmapped
// from it since getPMAsRemappingLocked returned.
func (remap *faultRemap) release() {
	remap.decRefs.Cleanup()
	remap.decRefs = nil
}

// getPMAsRemappingLocked is getPMAsLocked for a caller that maps remap.ar into
// the AddressSpace as soon as it returns. See faultRemap.
func (mm *MemoryManager) getPMAsRemappingLocked(ctx context.Context, vseg vmaIterator, ar hostarch.AddrRange, at hostarch.AccessType, callerIndirectCommit, forPin bool, remap *faultRemap) (pmaIterator, pmaGapIterator, error) {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
		if !vseg.Ok() {
			panic("terminal vma iterator")
		}
		if !vseg.Range().Contains(ar.Start) {
			panic(fmt.Sprintf("initial vma %v does not cover start of ar %v", vseg.Range(), ar))
		}
	}

	// Page-align ar so that all AddrRanges are aligned.
	end, ok := ar.End.RoundUp()
	var alignerr error
	if !ok {
		end = ar.End.RoundDown()
		alignerr = linuxerr.EFAULT
	}
	ar = hostarch.AddrRange{ar.Start.RoundDown(), end}

	pstart, pend, perr := mm.getPMAsInternalLocked(ctx, vseg, ar, at, callerIndirectCommit, forPin, remap)
	if pend.Start() <= ar.Start {
		return pmaIterator{}, pend, perr
	}
	// getPMAsInternalLocked may not have returned pstart due to iterator
	// invalidation.
	if !pstart.Ok() {
		pstart = mm.findOrSeekPrevUpperBoundPMA(ar.Start, pend)
	}
	if perr != nil {
		return pstart, pend, perr
	}
	return pstart, pend, alignerr
}

// getVecPMAsLocked ensures that pmas exist for all addresses in ars, and
// support access of type at. It returns the subset of ars for which pmas
// exist. If this is not equal to ars, it returns a non-nil error explaining
// why.
//
// Preconditions:
//   - mm.mappingMu must be locked.
//   - mm.activeMu must be locked for writing.
//   - vmas must exist for all addresses in ars, and support accesses of type at
//     (i.e. permission checks must have been performed against vmas).
func (mm *MemoryManager) getVecPMAsLocked(ctx context.Context, ars hostarch.AddrRangeSeq, at hostarch.AccessType, callerIndirectCommit bool) (hostarch.AddrRangeSeq, error) {
	for arsit := ars; !arsit.IsEmpty(); arsit = arsit.Tail() {
		ar := arsit.Head()
		if ar.Length() == 0 {
			continue
		}
		if checkInvariants {
			if !ar.WellFormed() {
				panic(fmt.Sprintf("invalid ar: %v", ar))
			}
		}

		// Page-align ar so that all AddrRanges are aligned.
		end, ok := ar.End.RoundUp()
		var alignerr error
		if !ok {
			end = ar.End.RoundDown()
			alignerr = linuxerr.EFAULT
		}
		ar = hostarch.AddrRange{ar.Start.RoundDown(), end}

		_, pend, perr := mm.getPMAsInternalLocked(ctx, mm.vmas.FindSegment(ar.Start), ar, at, callerIndirectCommit, false /* forPin */, nil)
		if perr != nil {
			return truncatedAddrRangeSeq(ars, arsit, pend.Start()), perr
		}
		if alignerr != nil {
			return truncatedAddrRangeSeq(ars, arsit, pend.Start()), alignerr
		}
	}

	return ars, nil
}

// Gets the default memory file offset allocation direction aligned with
// default address space allocation direction.
func (mm *MemoryManager) getDefaultAllocationDirection() pgalloc.Direction {
	if mm.layout.DefaultDirection == arch.MmapTopDown {
		return pgalloc.TopDown
	}
	return pgalloc.BottomUp
}

// Gets the memory file offset allocation direction based on address space allocation direction
// and memory access order.
func (mm *MemoryManager) getAllocationDirection(ar hostarch.AddrRange, vma *vma) pgalloc.Direction {
	lastFault := atomic.LoadUintptr(&vma.lastFault)
	arStart := uintptr(ar.Start)
	// If this VMA does not have last page fault or last page fault equals to arStart,
	// use the default allocation direction.
	if lastFault == 0 || lastFault == arStart {
		return mm.getDefaultAllocationDirection()
	}
	// Detect cases where memory is accessed downwards and change memory file
	// allocation order to increase the chances that pages are coalesced.
	if arStart < lastFault {
		return pgalloc.TopDown
	}
	return pgalloc.BottomUp
}

// getPMAsInternalLocked is equivalent to getPMAsLocked, with the following
// exceptions:
//
//   - getPMAsInternalLocked returns a pmaIterator on a best-effort basis (that
//     is, the returned iterator may be terminal, even if a pma that contains
//     ar.Start exists). Returning this iterator on a best-effort basis allows
//     callers that require it to use it when it's cheaply available, while also
//     avoiding the overhead of retrieving it when it's not.
//
//   - getPMAsInternalLocked additionally requires that ar is page-aligned.
//     getPMAsInternalLocked is an implementation helper for getPMAsLocked and
//     getVecPMAsLocked; other clients should call one of those instead.
func (mm *MemoryManager) getPMAsInternalLocked(ctx context.Context, vseg vmaIterator, ar hostarch.AddrRange, at hostarch.AccessType, callerIndirectCommit, forPin bool, remap *faultRemap) (pmaIterator, pmaGapIterator, error) {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 || !ar.IsPageAligned() {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
		if !vseg.Ok() {
			panic("terminal vma iterator")
		}
		if !vseg.Range().Contains(ar.Start) {
			panic(fmt.Sprintf("initial vma %v does not cover start of ar %v", vseg.Range(), ar))
		}
	}
	var pfdrs *pendingFileDecRefs
	defer func() { // must be a closure to avoid evaluating pfdrs immediately
		if remap != nil {
			// Released by the caller once it has remapped remap.ar.
			if checkInvariants && remap.decRefs != nil {
				panic("faultRemap lent twice")
			}
			remap.decRefs = pfdrs
			return
		}
		pfdrs.Cleanup()
	}()
	var unmapAR hostarch.AddrRange
	defer func() {
		// The caller maps remap.ar immediately, replacing whatever is there.
		// Only what lies outside it needs an explicit unmap.
		if remap == nil || !unmapAR.Overlaps(remap.ar) {
			mm.unmapASLocked(unmapAR)
			return
		}
		if unmapAR.Start < remap.ar.Start {
			mm.unmapASLocked(hostarch.AddrRange{Start: unmapAR.Start, End: remap.ar.Start})
		}
		if unmapAR.End > remap.ar.End {
			mm.unmapASLocked(hostarch.AddrRange{Start: remap.ar.End, End: unmapAR.End})
		}
	}()

	vma := vseg.ValuePtr()
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	allocDir := mm.getAllocationDirection(ar, vma)
	atomic.StoreUintptr(&vma.lastFault, uintptr(ar.Start))

	// Limit the range we allocate to ar, aligned to hugepage boundaries.
	hugeMaskAR := hugepageAligned(ar)
	// The range in which we iterate vmas and pmas is still limited to ar, to
	// ensure that we don't allocate or COW-break a pma we don't need.
	pseg, pgap := mm.pmas.Find(ar.Start)
	pstart := pseg
	for {
		// Get pmas for this vma.
		vsegAR := vseg.Range().Intersect(ar)
		vma := vseg.ValuePtr()
	pmaLoop:
		for {
			switch {
			case pgap.Ok() && pgap.Start() < vsegAR.End:
				// Need a pma here.
				optAR := vseg.Range().Intersect(pgap.Range())
				if checkInvariants {
					if optAR.Length() == 0 {
						panic(fmt.Sprintf("vseg %v and pgap %v do not overlap", vseg, pgap))
					}
				}
				if vma.mappable == nil {
					// Private anonymous mappings get pmas by allocating.
					// The allocated range is limited to ar, expanded to
					// hugepage alignment. This is done even if the allocation
					// will not be hugepage-backed, in an attempt to reduce
					// application page faults (that trap into the sentry) by
					// creating AddressSpace mappings in advance.
					allocAR := optAR.Intersect(hugeMaskAR)
					if mm.homeOwner != 0 {
						// A home holds one block of addresses; a later
						// iteration of this loop allocates the rest.
						if end := homeBlockEnd(allocAR.Start); end < allocAR.End {
							allocAR.End = end
						}
					}
					// Don't back stacks with huge pages due to low utilization
					// and because they're often fragmented by copy-on-write.
					mayHuge := mm.mf.HugepagesEnabled() && !vma.growsDown && !vma.isStack
					huge := false
					if mayHuge {
						if allocAR.IsHugePageAligned() {
							huge = true
						} else {
							startHugeRoundUp, ok := allocAR.Start.HugeRoundUp()
							endHugeRoundDown := allocAR.End.HugeRoundDown()
							if ok && startHugeRoundUp < endHugeRoundDown {
								// allocAR contains at least one full aligned
								// huge page.
								if allocAR.Start != startHugeRoundUp {
									// Shorten allocAR to exclude full huge
									// pages, so that a later iteration of this
									// loop can allocate huge pages for the
									// hugepage-aligned subrange.
									allocAR.End = startHugeRoundUp
								} else {
									// Shorten allocAR to include only full
									// huge pages, so that a later iteration of
									// this loop can allocate small pages for
									// the sub-hugepage tail of allocAR.
									allocAR.End = endHugeRoundDown
									huge = true
								}
							}
						}
					}
					allocOpts := pgalloc.AllocOpts{
						Kind:             usage.Anonymous,
						MemCgID:          memCgID,
						Mode:             pgalloc.AllocateUncommitted,
						Huge:             huge,
						LogicalHugePages: huge,
						Dir:              allocDir,
						Placement:        placementOf(ctx, vma, allocAR.Start),
					}
					// If the allocation is hugepage-backed and
					// callerIndirectCommit is true, the caller will commit every
					// allocated huge page. If the allocation is not
					// hugepage-backed, the caller won't commit every allocated
					// page since hugeMaskAR is ar expanded to huge alignment,
					// unless only one page in optAR falls into the huge page.
					if callerIndirectCommit && (huge || allocAR.Length() == hostarch.PageSize) {
						allocOpts.Mode = pgalloc.AllocateCallerIndirectCommit
					}
					fr, err := mm.allocatePrivateLocked(allocAR, allocOpts)
					if err != nil {
						return pstart, pgap, err
					}
					if checkInvariants {
						if !fr.WellFormed() || fr.Length() != uint64(allocAR.Length()) {
							panic(fmt.Sprintf("Allocate(%v) returned invalid FileRange %v", allocAR.Length(), fr))
						}
					}
					mm.addRSSLocked(allocAR)
					remap.noteInserted(allocAR)
					pseg, pgap = mm.pmas.Insert(pgap, allocAR, pma{
						file:           mm.mf,
						off:            fr.Start,
						translatePerms: hostarch.AnyAccess,
						effectivePerms: vma.effectivePerms,
						maxPerms:       vma.maxPerms,
						// Since we just allocated this memory and have the
						// only reference, the new pma does not need
						// copy-on-write.
						private: true,
						huge:    huge,
					}).NextNonEmpty()
					pstart = pmaIterator{} // iterators invalidated
				} else {
					// Other mappings get pmas by translating.
					optMR := vseg.mappableRangeOf(optAR)
					reqAR := optAR.Intersect(ar)
					reqMR := vseg.mappableRangeOf(reqAR)
					perms := at
					if vma.private {
						// This pma will be copy-on-write; don't require write
						// permission, but do require read permission to
						// facilitate the copy.
						//
						// If at.Write is true, we will need to break
						// copy-on-write immediately, which occurs after
						// translation below.
						perms.Read = true
						perms.Write = false
					}
					ts, err := vma.mappable.Translate(ctx, reqMR, optMR, perms)
					if checkInvariants {
						if err := memmap.CheckTranslateResult(reqMR, optMR, perms, ts, err); err != nil {
							panic(fmt.Sprintf("Mappable(%T).Translate(%v, %v, %v): %v", vma.mappable, reqMR, optMR, perms, err))
						}
					}
					// Install a pma for each translation.
					if len(ts) == 0 {
						return pstart, pgap, err
					}
					pstart = pmaIterator{} // iterators invalidated
					for _, t := range ts {
						newpmaAR := vseg.addrRangeOf(t.Source)
						newpma := pma{
							file:           t.File,
							off:            t.Offset,
							translatePerms: t.Perms,
							effectivePerms: vma.effectivePerms.Intersect(t.Perms),
							maxPerms:       vma.maxPerms.Intersect(t.Perms),
						}
						if vma.private {
							newpma.effectivePerms.Write = false
							newpma.maxPerms.Write = false
							newpma.needCOW = true
						}
						mm.addRSSLocked(newpmaAR)
						t.File.IncRef(t.FileRange(), memCgID)
						remap.noteInserted(newpmaAR)
						// This is valid because memmap.Mappable.Translate is
						// required to return Translations in increasing
						// Translation.Source order.
						pseg = mm.pmas.Insert(pgap, newpmaAR, newpma)
						pgap = pseg.NextGap()
					}
					// The error returned by Translate is only significant if
					// it occurred before ar.End.
					if err != nil && vseg.addrRangeOf(ts[len(ts)-1].Source).End < ar.End {
						return pstart, pgap, err
					}
					// Rewind pseg to the first pma inserted and continue the
					// loop to check if we need to break copy-on-write.
					pseg, pgap = mm.findOrSeekPrevUpperBoundPMA(vseg.addrRangeOf(ts[0].Source).Start, pgap), pmaGapIterator{}
					continue
				}

			case pseg.Ok() && pseg.Start() < vsegAR.End:
				oldpma := pseg.ValuePtr()
				if (at.Write || forPin) && mm.isPMACopyOnWriteLocked(vseg, pseg) {
					// Break copy-on-write by copying.
					if checkInvariants {
						if !oldpma.maxPerms.Read {
							panic(fmt.Sprintf("pma %v needs to be copied for writing, but is not readable: %v", pseg.Range(), oldpma))
						}
					}
					// A 2 MiB page that mm maps whole is copied, or owned,
					// whole.
					addr := max(pseg.Start(), ar.Start)
					if block, page, ok := mm.hugePageBlockLocked(pseg, addr); ok {
						if seg, ok := mm.breakHugePageCOWLocked(ctx, block, page, addr, memCgID, allocDir, &unmapAR, &pfdrs); ok {
							remap.noteInserted(block)
							pstart = pmaIterator{} // iterators invalidated
							pseg, pgap = seg, pmaGapIterator{}
							continue
						}
					}
					// A page that mm alone holds, as one a fork shared until
					// the fork copied it, is mm's to write, as Linux reuses a
					// page that only the writer maps: take the pages written
					// over in place, though other pages of their pma are still
					// shared, rather than copy them. They take new versions,
					// as a copy would.
					if oldpma.private {
						ownAR := pseg.Range().Intersect(ar)
						if ownFR := pseg.fileRangeOf(ownAR); mm.mf.OwnForWrite(ownFR) {
							if mm.homeOwner != 0 {
								// Its 2 MiB may be all mm's now.
								mm.noteCollapsibleLocked(ownAR, ownFR)
							}
							pseg = mm.pmas.Isolate(pseg, ownAR)
							owned := pseg.ValuePtr()
							owned.needCOW = false
							owned.effectivePerms = vma.effectivePerms
							owned.maxPerms = vma.maxPerms
							pstart = pmaIterator{} // iterators invalidated
							pseg, pgap = pseg.NextNonEmpty()
							continue
						}
					}
					var copyAR hostarch.AddrRange
					if vma.effectivePerms.Execute {
						// The majority of copy-on-write breaks on executable
						// pages come from:
						//
						//	- The ELF loader, which must zero out bytes on the
						//		last page of each segment after the end of the
						//		segment.
						//
						//	- gdb's use of ptrace to insert breakpoints.
						//
						// Neither of these cases has enough spatial locality
						// to benefit from copying nearby pages, so if the vma
						// is executable, only copy the pages required.
						copyAR = pseg.Range().Intersect(ar)
					} else if vma.growsDown || vma.isStack {
						// In most cases, the new process will not use most of
						// its stack before exiting or invoking execve(); it is
						// especially unlikely to return very far down its call
						// stack, since async-signal-safety concerns in
						// multithreaded programs prevent the new process from
						// being able to do much. So only copy up to one page
						// before and after the pages required.
						stackMaskAR := ar
						if newStart := stackMaskAR.Start - hostarch.PageSize; newStart < stackMaskAR.Start {
							stackMaskAR.Start = newStart
						}
						if newEnd := stackMaskAR.End + hostarch.PageSize; newEnd > stackMaskAR.End {
							stackMaskAR.End = newEnd
						}
						copyAR = pseg.Range().Intersect(stackMaskAR)
					} else {
						// Copy what was asked for and no more. A page fault
						// asks for more when it continues a sequential run;
						// see writeFaultRangeLocked.
						copyAR = pseg.Range().Intersect(ar)
					}
					// Get internal mappings from the pma to copy from.
					if err := pseg.getInternalMappingsLocked(); err != nil {
						return pstart, pseg.PrevGap(), err
					}
					// Copy the pmas that follow in ar along with it, when they
					// need copying too, into the same allocation. Page-sized
					// copies split pmas finely, and copying the pieces one at
					// a time would keep them split and cost an allocation and
					// a mapping apiece. The copy stays in the 2 MiB of the
					// address space that it starts in: a 2 MiB page that mm
					// maps whole in the next is copied whole.
					limit := vsegAR.End
					if boundary, ok := copyAR.Start.HugeRoundDown().AddLength(hostarch.HugePageSize); ok && boundary < limit && mm.mf.HugepagesEnabled() {
						limit = boundary
					}
					copyAR.End = min(copyAR.End, limit)
					for next := pseg.NextSegment(); next.Ok() && next.Start() == copyAR.End && copyAR.End < limit; next = next.NextSegment() {
						if !mm.isPMACopyOnWriteLocked(vseg, next) || next.getInternalMappingsLocked() != nil {
							break
						}
						copyAR.End = min(next.End(), limit)
					}
					// Copy contents.
					huge := mm.mf.HugepagesEnabled() && copyAR.IsHugePageAligned()
					reader := safemem.BlockSeqReader{Blocks: mm.internalMappingsLocked(pseg, copyAR)}
					fr, err := mm.allocatePrivateLocked(copyAR, pgalloc.AllocOpts{
						Kind:       usage.Anonymous,
						MemCgID:    memCgID,
						Mode:       pgalloc.AllocateAndWritePopulate,
						Huge:       huge,
						Dir:        allocDir,
						ReaderFunc: reader.ReadToBlocks,
						// A copy goes where the page that the faulting task
						// writes would, not where the original is.
						Placement: placementOf(ctx, vma, copyAR.Start),
					})
					if _, ok := err.(safecopy.BusError); ok {
						// If we got SIGBUS during the copy, deliver SIGBUS to
						// userspace (instead of SIGSEGV) if we're breaking
						// copy-on-write due to application page fault.
						err = &memmap.BusError{err}
					}
					if fr.Length() == 0 {
						return pstart, pseg.PrevGap(), err
					}
					// Replace the pmas with a copy in the part of the address
					// range where copying was successful. This doesn't change
					// RSS.
					copyAR.End = copyAR.Start + hostarch.Addr(fr.Length())
					if err := mm.adoptCOWIdentitiesLocked(pseg, copyAR, fr); err != nil {
						mm.mf.DecRef(fr)
						return pstart, pseg.PrevGap(), fmt.Errorf("adopting logical page identities during CoW: %w", err)
					}
					unmapAR = joinAddrRanges(unmapAR, copyAR)
					for seg := pseg; seg.Ok() && seg.Start() < copyAR.End; seg = seg.NextSegment() {
						pfdrs = appendPendingCOWFileDecRef(pfdrs, seg.ValuePtr().file, seg.fileRangeOf(seg.Range().Intersect(copyAR)))
					}
					// Insert merges the copy with its neighbors if it can.
					remap.noteInserted(copyAR)
					pseg = mm.pmas.Insert(mm.pmas.RemoveRange(copyAR), copyAR, pma{
						file:           mm.mf,
						off:            fr.Start,
						translatePerms: hostarch.AnyAccess,
						effectivePerms: vma.effectivePerms,
						maxPerms:       vma.maxPerms,
						private:        true,
						huge:           huge,
					})
					pstart = pmaIterator{} // iterators invalidated
					// The error returned by AllocateAndFill is only
					// significant if it occurred before ar.End.
					if err != nil && pseg.End() < ar.End {
						return pstart, pseg.NextGap(), err
					}
					// Ensure pseg and pgap are correct for the next iteration
					// of the loop.
					pseg, pgap = pseg.NextNonEmpty()
				} else if !oldpma.translatePerms.SupersetOf(at) {
					// Get new pmas (with sufficient permissions) by calling
					// memmap.Mappable.Translate again.
					if checkInvariants {
						if oldpma.private {
							panic(fmt.Sprintf("private pma %v has non-maximal pma.translatePerms: %v", pseg.Range(), oldpma))
						}
					}
					// Allow the entire pma to be replaced.
					optAR := pseg.Range()
					optMR := vseg.mappableRangeOf(optAR)
					reqAR := optAR.Intersect(ar)
					reqMR := vseg.mappableRangeOf(reqAR)
					perms := oldpma.translatePerms.Union(at)
					ts, err := vma.mappable.Translate(ctx, reqMR, optMR, perms)
					if checkInvariants {
						if err := memmap.CheckTranslateResult(reqMR, optMR, perms, ts, err); err != nil {
							panic(fmt.Sprintf("Mappable(%T).Translate(%v, %v, %v): %v", vma.mappable, reqMR, optMR, perms, err))
						}
					}
					// Remove the part of the existing pma covered by new
					// Translations, then insert new pmas. This doesn't change
					// RSS.
					if len(ts) == 0 {
						return pstart, pseg.PrevGap(), err
					}
					transMR := memmap.MappableRange{ts[0].Source.Start, ts[len(ts)-1].Source.End}
					transAR := vseg.addrRangeOf(transMR)
					pseg = mm.pmas.Isolate(pseg, transAR)
					unmapAR = joinAddrRanges(unmapAR, transAR)
					pfdrs = appendPendingFileDecRef(pfdrs, pseg.ValuePtr().file, pseg.fileRange())
					pgap = mm.pmas.Remove(pseg)
					pstart = pmaIterator{} // iterators invalidated
					for _, t := range ts {
						newpmaAR := vseg.addrRangeOf(t.Source)
						newpma := pma{
							file:           t.File,
							off:            t.Offset,
							translatePerms: t.Perms,
							effectivePerms: vma.effectivePerms.Intersect(t.Perms),
							maxPerms:       vma.maxPerms.Intersect(t.Perms),
						}
						if vma.private {
							newpma.effectivePerms.Write = false
							newpma.maxPerms.Write = false
							newpma.needCOW = true
						}
						t.File.IncRef(t.FileRange(), memCgID)
						remap.noteInserted(newpmaAR)
						pseg = mm.pmas.Insert(pgap, newpmaAR, newpma)
						pgap = pseg.NextGap()
					}
					// The error returned by Translate is only significant if
					// it occurred before ar.End.
					if err != nil && pseg.End() < ar.End {
						return pstart, pgap, err
					}
					// Ensure pseg and pgap are correct for the next iteration
					// of the loop.
					if pgap.Range().Length() == 0 {
						pseg, pgap = pgap.NextSegment(), pmaGapIterator{}
					} else {
						pseg = pmaIterator{}
					}
				} else {
					// We have a usable pma; continue.
					pseg, pgap = pseg.NextNonEmpty()
				}

			default:
				break pmaLoop
			}
		}
		// Go to the next vma.
		if ar.End <= vseg.End() {
			if pgap.Ok() {
				return pstart, pgap, nil
			}
			return pstart, pseg.PrevGap(), nil
		}
		vseg = vseg.NextSegment()
	}
}

// writeFaultRangeLocked returns the range a write fault at ar should make
// usable: ar itself, or more when the fault continues a sequential run of
// write faults in a private mapping.
//
// Breaking copy-on-write of base pages copies only the range asked for. gVisor
// used to widen every copy to the surrounding huge page, so one written byte
// duplicated 2 MiB: a TTC fork and its source diverged by 512 pages for every
// page either one wrote, and every copied page received a new content version,
// so each later capture or transfer saw 512 changed pages where one had
// changed. A 2 MiB page, which is one page with one version, is copied whole
// instead; see hugePageBlockLocked.
//
// Copying one page per fault is exact but not free. Each fault is a round
// trip to the platform, which the huge-page copy amortized: a program
// rewriting a buffer front to back after a fork took thirteen times as long.
// So a write fault that begins where the previous one in this vma ended asks
// for twice as many pages as that one did, up to a huge page and never past
// the next huge-page boundary. Scattered writes copy one page each; a sweep
// asks for a whole huge page by its tenth fault and then one per fault.
//
// The range spans pmas, because page-sized copies split pmas finely: a sweep
// over memory that scattered writes copied earlier would otherwise fault once
// per piece. It spans only pmas that already exist, because a gap in a file
// mapping must be translated, and a translation that failed past the faulting
// page would fail the fault.
//
// Preconditions:
//   - mm.mappingMu must be locked.
//   - mm.activeMu must be locked for writing.
//   - ar is the page that faulted, and vseg.Range().Contains(ar.Start).
func (mm *MemoryManager) writeFaultRangeLocked(vseg vmaIterator, ar hostarch.AddrRange) hostarch.AddrRange {
	vma := vseg.ValuePtr()
	if !vma.private || vma.effectivePerms.Execute || vma.growsDown || vma.isStack {
		// Shared mappings never copy; executable and stack mappings copy by
		// their own rules in getPMAsInternalLocked.
		return ar
	}
	pages := uintptr(1)
	if uintptr(ar.Start) == atomic.LoadUintptr(&vma.writeRunEnd) {
		pages = min(2*atomic.LoadUintptr(&vma.writeRunPages), uintptr(hostarch.HugePageSize/hostarch.PageSize))
	}
	end := ar.End
	if pages > 1 {
		limit := vseg.End()
		if boundary, ok := ar.Start.HugeRoundUp(); ok && boundary > ar.Start && boundary < limit {
			limit = boundary
		}
		if want, ok := ar.Start.AddLength(uint64(pages) * hostarch.PageSize); ok && want < limit {
			limit = want
		}
		covered := ar.Start
		for pseg := mm.pmas.FindSegment(ar.Start); pseg.Ok() && pseg.Start() <= covered && covered < limit; pseg = pseg.NextSegment() {
			covered = pseg.End()
		}
		end = max(end, min(covered, limit))
	}
	atomic.StoreUintptr(&vma.writeRunEnd, uintptr(end))
	atomic.StoreUintptr(&vma.writeRunPages, pages)
	return hostarch.AddrRange{Start: ar.Start, End: end}
}

// adoptCOWIdentitiesLocked gives the pages of fr, a copy of the pmas in ar
// beginning with pseg, the logical identities of the pages they copy.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - pseg.Range().Contains(ar.Start), and pmas exist for all of ar.
//   - fr.Length() == ar.Length().
func (mm *MemoryManager) adoptCOWIdentitiesLocked(pseg pmaIterator, ar hostarch.AddrRange, fr memmap.FileRange) error {
	for ; pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		sourceMF, ok := pseg.ValuePtr().file.(*pgalloc.MemoryFile)
		if !ok {
			continue
		}
		segAR := pseg.Range().Intersect(ar)
		target := memmap.FileRange{
			Start: fr.Start + uint64(segAR.Start-ar.Start),
			End:   fr.Start + uint64(segAR.End-ar.Start),
		}
		if err := mm.mf.AdoptCOWIdentities(sourceMF, pseg.fileRangeOf(segAR), target); err != nil {
			return err
		}
	}
	return nil
}

// hugePageBlockLocked returns the 2 MiB page of pseg's file that the page at
// addr is part of, and the 2 MiB of the address space that maps it, if mm
// maps it whole, in order, at a 2 MiB-aligned address, in private pmas. A
// write takes such a page whole, as Linux does a huge page that a process maps
// with one page table entry; it takes a 2 MiB page that mm maps otherwise, in
// part or out of place, a base page at a time.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - pseg.Range().Contains(addr).
func (mm *MemoryManager) hugePageBlockLocked(pseg pmaIterator, addr hostarch.Addr) (hostarch.AddrRange, memmap.FileRange, bool) {
	p := pseg.ValuePtr()
	file, ok := p.file.(*pgalloc.MemoryFile)
	if !ok || !p.private {
		return hostarch.AddrRange{}, memmap.FileRange{}, false
	}
	addr = addr.RoundDown()
	off := p.off + uint64(addr-pseg.Start())
	page, ok := file.HugePageAt(off)
	if !ok || uint64(addr) < off-page.Start {
		return hostarch.AddrRange{}, memmap.FileRange{}, false
	}
	block := hostarch.AddrRange{Start: addr - hostarch.Addr(off-page.Start)}
	end, ok := block.Start.AddLength(hostarch.HugePageSize)
	if !ok || !block.Start.IsHugePageAligned() {
		return hostarch.AddrRange{}, memmap.FileRange{}, false
	}
	block.End = end
	covered := block.Start
	for seg := mm.pmas.FindSegment(block.Start); covered < block.End; seg = seg.NextSegment() {
		if !seg.Ok() || seg.Start() > covered {
			return hostarch.AddrRange{}, memmap.FileRange{}, false
		}
		if q := seg.ValuePtr(); q.file != p.file || !q.private || q.off+uint64(covered-seg.Start()) != page.Start+uint64(covered-block.Start) {
			return hostarch.AddrRange{}, memmap.FileRange{}, false
		}
		covered = seg.End()
	}
	return block, page, true
}

// breakHugePageCOWLocked breaks copy-on-write on page, a 2 MiB page that mm
// maps whole at block (see hugePageBlockLocked), as one page: it takes
// ownership of the page if mm holds the only reference on it, and otherwise
// copies it whole into a new 2 MiB page, which takes its identity and one new
// version. It returns the pma that then maps addr, or false if it could do
// neither, for the caller to copy what it needs a base page at a time.
//
// Preconditions:
//   - mm.mappingMu must be locked.
//   - mm.activeMu must be locked for writing.
//   - block.Contains(addr).
//   - The caller unmaps *unmapAR from the AddressSpace and then releases
//     *pfdrs.
func (mm *MemoryManager) breakHugePageCOWLocked(ctx context.Context, block hostarch.AddrRange, page memmap.FileRange, addr hostarch.Addr, memCgID uint32, allocDir pgalloc.Direction, unmapAR *hostarch.AddrRange, pfdrs **pendingFileDecRefs) (pmaIterator, bool) {
	first := mm.pmas.FindSegment(block.Start)
	file := first.ValuePtr().file
	if file == memmap.File(mm.mf) && mm.mf.OwnForWrite(page) {
		vseg := mm.vmas.FindSegment(block.Start)
		for seg := first; seg.Ok() && seg.Start() < block.End; seg = seg.NextSegment() {
			seg = mm.pmas.Isolate(seg, block)
			for vseg.End() <= seg.Start() {
				vseg = vseg.NextSegment()
			}
			p := seg.ValuePtr()
			p.needCOW = false
			// p.private => p.translatePerms == hostarch.AnyAccess
			p.effectivePerms = vseg.ValuePtr().effectivePerms
			p.maxPerms = vseg.ValuePtr().maxPerms
		}
		return mm.pmas.FindSegment(addr), true
	}
	source, ok := file.(*pgalloc.MemoryFile)
	if !ok {
		return pmaIterator{}, false
	}
	for seg := first; seg.Ok() && seg.Start() < block.End; seg = seg.NextSegment() {
		// A part that mm owns, which a device's DMA may use, stays; see
		// prepareLiveFork.
		if !seg.ValuePtr().needCOW || seg.getInternalMappingsLocked() != nil {
			return pmaIterator{}, false
		}
	}
	// The original is read through its home's huge view where it has one,
	// with one page table entry, rather than 512 through the Sentry's own
	// mapping of the home.
	original, viewed := source.HugePageMapping(page)
	if !viewed {
		original = mm.internalMappingsLocked(first, block)
	}
	reader := safemem.BlockSeqReader{Blocks: original}
	huge := mm.mf.HugepagesEnabled()
	fr, err := mm.allocatePrivateLocked(block, pgalloc.AllocOpts{
		Kind:             usage.Anonymous,
		MemCgID:          memCgID,
		Mode:             pgalloc.AllocateAndWritePopulate,
		Huge:             huge,
		LogicalHugePages: huge,
		Dir:              allocDir,
		ReaderFunc:       reader.ReadToBlocks,
		// A copy goes where the page that the faulting task writes would,
		// not where the original is.
		Placement: placementOf(ctx, mm.vmas.FindSegment(addr).ValuePtr(), addr),
	})
	if err != nil || fr.Length() != hostarch.HugePageSize {
		if fr.Length() != 0 {
			mm.mf.DecRef(fr)
		}
		return pmaIterator{}, false
	}
	if err := mm.mf.AdoptCOWIdentities(source, page, fr); err != nil {
		mm.mf.DecRef(fr)
		return pmaIterator{}, false
	}
	// Replace the pmas with the copy, each part of it with the permissions
	// of the vma it is in. This doesn't change RSS.
	*unmapAR = joinAddrRanges(*unmapAR, block)
	*pfdrs = appendPendingCOWFileDecRef(*pfdrs, file, page)
	pgap := mm.pmas.RemoveRange(block)
	for vseg := mm.vmas.FindSegment(block.Start); vseg.Ok() && vseg.Start() < block.End; vseg = vseg.NextSegment() {
		ar := vseg.Range().Intersect(block)
		vma := vseg.ValuePtr()
		pgap = mm.pmas.Insert(pgap, ar, pma{
			file:           mm.mf,
			off:            fr.Start + uint64(ar.Start-block.Start),
			translatePerms: hostarch.AnyAccess,
			effectivePerms: vma.effectivePerms,
			maxPerms:       vma.maxPerms,
			private:        true,
			huge:           huge,
		}).NextGap()
	}
	return mm.pmas.FindSegment(addr), true
}

// ownedRangeLocked returns the range of mm.mf that a write to pseg, a private
// pma, takes ownership of: pseg's, widened at either end to the whole of a 2
// MiB page that pseg maps in part where mm maps that page whole (see
// hugePageBlockLocked), as when mprotect has split the pma that mapped it.
// Ownership of a 2 MiB page that mm maps in part splits it into base pages
// instead; see pgalloc.MemoryFile.OwnForWrite.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - pseg.ValuePtr().private.
func (mm *MemoryManager) ownedRangeLocked(pseg pmaIterator) memmap.FileRange {
	fr := pseg.fileRange()
	if !hostarch.IsHugePageAligned(fr.Start) {
		if _, page, ok := mm.hugePageBlockLocked(pseg, pseg.Start()); ok {
			fr.Start = page.Start
		}
	}
	if !hostarch.IsHugePageAligned(fr.End) {
		if _, page, ok := mm.hugePageBlockLocked(pseg, pseg.End()-hostarch.PageSize); ok {
			fr.End = page.End
		}
	}
	return fr
}

func hugepageAligned(ar hostarch.AddrRange) hostarch.AddrRange {
	aligned := hostarch.AddrRange{ar.Start.HugeRoundDown(), ar.End}
	if end, ok := ar.End.HugeRoundUp(); ok {
		aligned.End = end
	}
	if checkInvariants {
		if !aligned.IsSupersetOf(ar) {
			panic(fmt.Sprintf("aligned AddrRange %#v is not a superset of ar %#v", aligned, ar))
		}
	}
	return aligned
}

// isPMACopyOnWriteLocked returns true if the contents of the pma represented
// by pseg must be copied to a new private pma to be written to.
//
// If the pma is a copy-on-write private pma, and holds the only reference on
// the memory it maps, isPMACopyOnWriteLocked will take ownership of the memory
// and update the pma to indicate that it does not require copy-on-write.
//
// Preconditions:
//   - vseg.Range().IsSupersetOf(pseg.Range()).
//   - mm.mappingMu must be locked.
//   - mm.activeMu must be locked for writing.
func (mm *MemoryManager) isPMACopyOnWriteLocked(vseg vmaIterator, pseg pmaIterator) bool {
	pma := pseg.ValuePtr()
	if !pma.needCOW {
		return false
	}
	if !pma.private {
		return true
	}
	// If we have the only reference on private memory to be copied, just take
	// ownership of it instead of copying. If we do hold the only reference,
	// additional references can only be taken by mm.Fork(), which is excluded
	// by mm.activeMu, so this isn't racy. Its pages take new content versions,
	// as a copy's would: a capture may have recorded their bytes while they
	// were shared. A 2 MiB page that pma maps in part is owned whole, or
	// split into base pages; see ownedRangeLocked.
	if mm.mf.OwnForWrite(mm.ownedRangeLocked(pseg)) {
		pma.needCOW = false
		// pma.private => pma.translatePerms == hostarch.AnyAccess
		vma := vseg.ValuePtr()
		pma.effectivePerms = vma.effectivePerms
		pma.maxPerms = vma.maxPerms
		return false
	}
	return true
}

// Invalidate implements memmap.MappingSpace.Invalidate.
func (mm *MemoryManager) Invalidate(ar hostarch.AddrRange, opts memmap.InvalidateOpts) {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 || !ar.IsPageAligned() {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
	}

	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	if mm.captureInvalidations {
		mm.capturedInvalidations = append(mm.capturedInvalidations, invalidateArgs{ar, opts})
		return
	}
	mm.invalidateLocked(ar, opts.InvalidatePrivate, true)
}

// invalidateLocked removes pmas and AddressSpace mappings of those pmas for
// addresses in ar.
//
// Preconditions:
//   - mm.activeMu must be locked for writing.
//   - ar.Length() != 0.
//   - ar must be page-aligned.
func (mm *MemoryManager) invalidateLocked(ar hostarch.AddrRange, invalidatePrivate, invalidateShared bool) {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 || !ar.IsPageAligned() {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
	}

	var didUnmapAS bool
	pseg := mm.pmas.LowerBoundSegment(ar.Start)
	for pseg.Ok() && pseg.Start() < ar.End {
		pma := pseg.ValuePtr()
		if (invalidatePrivate && pma.private) || (invalidateShared && !pma.private) {
			pseg = mm.pmas.Isolate(pseg, ar)
			pma = pseg.ValuePtr()
			if !didUnmapAS {
				// Unmap all of ar, not just pseg.Range(), to minimize host
				// syscalls. AddressSpace mappings must be removed before
				// pma.file.DecRef().
				//
				// Note that we do more than just ar here, and extrapolate
				// to the end of any previous region that we may have mapped.
				// This is done to ensure that lower layers can fully invalidate
				// intermediate pagetable pages during the unmap.
				var unmapAR hostarch.AddrRange
				if prev := pseg.PrevSegment(); prev.Ok() {
					unmapAR.Start = prev.End()
				} else {
					unmapAR.Start = mm.layout.MinAddr
				}
				if last := mm.pmas.LowerBoundSegment(ar.End); last.Ok() {
					if last.Start() < ar.End {
						unmapAR.End = ar.End
					} else {
						unmapAR.End = last.Start()
					}
				} else {
					unmapAR.End = mm.layout.MaxAddr
				}
				mm.unmapASLocked(unmapAR)
				didUnmapAS = true
			}
			mm.removeRSSLocked(pseg.Range())
			pma.file.DecRef(pseg.fileRange())
			pseg = mm.pmas.Remove(pseg).NextSegment()
		} else {
			pseg = pseg.NextSegment()
		}
	}
}

// Pin returns the memmap.File ranges currently mapped by addresses in ar in
// mm, acquiring a reference on the returned ranges which the caller must
// release by calling Unpin. If not all addresses are mapped, Pin returns a
// non-nil error. Note that Pin may return both a non-empty slice of
// PinnedRanges and a non-nil error.
//
// Pin does not prevent mapped ranges from changing, making it unsuitable for
// most I/O. It should only be used in contexts that would use
// pin_user_pages(FOLL_LONGTERM) in the Linux kernel. Like Linux, Pin breaks
// copy-on-write on the pinned range (even if at.Write is false). This also
// yields the invariant that pinned pages in private mappings live in non-CoW
// pmas with >1 reference.
//
// Preconditions:
//   - ar.Length() != 0.
//   - ar must be page-aligned.
func (mm *MemoryManager) Pin(ctx context.Context, ar hostarch.AddrRange, at hostarch.AccessType, ignorePermissions bool) ([]PinnedRange, error) {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 || !ar.IsPageAligned() {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
	}

	// Ensure that we have usable vmas.
	mm.mappingMu.RLock()
	vseg, vend, verr := mm.getVMAsLocked(ctx, ar, at, ignorePermissions)
	if vendaddr := vend.Start(); vendaddr < ar.End {
		if vendaddr <= ar.Start {
			mm.mappingMu.RUnlock()
			return nil, verr
		}
		ar.End = vendaddr
	}

	// Ensure that we have usable pmas.
	mm.activeMu.Lock()
	mm.hasPinned = true
	pseg, pend, perr := mm.getPMAsLocked(ctx, vseg, ar, at, false /* callerIndirectCommit */, true /* forPin */)
	mm.mappingMu.RUnlock()
	if pendaddr := pend.Start(); pendaddr < ar.End {
		if pendaddr <= ar.Start {
			mm.activeMu.Unlock()
			return nil, perr
		}
		ar.End = pendaddr
	}

	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	// Gather pmas.
	var prs []PinnedRange
	for pseg.Ok() && pseg.Start() < ar.End {
		psar := pseg.Range().Intersect(ar)
		f := pseg.ValuePtr().file
		fr := pseg.fileRangeOf(psar)
		f.IncRef(fr, memCgID)
		prs = append(prs, PinnedRange{
			Source: psar,
			File:   f,
			Offset: fr.Start,
		})
		pseg = pseg.NextSegment()
	}
	mm.activeMu.Unlock()

	// Return the first error in order of progress through ar.
	if perr != nil {
		return prs, perr
	}
	return prs, verr
}

// PinnedRanges are returned by MemoryManager.Pin.
type PinnedRange struct {
	// Source is the corresponding range of addresses.
	Source hostarch.AddrRange

	// File is the mapped file.
	File memmap.File

	// Offset is the offset into File at which this PinnedRange begins.
	Offset uint64
}

// FileRange returns the memmap.File offsets mapped by pr.
func (pr PinnedRange) FileRange() memmap.FileRange {
	return memmap.FileRange{pr.Offset, pr.Offset + uint64(pr.Source.Length())}
}

// Unpin releases the reference held by prs.
func Unpin(prs []PinnedRange) {
	for i := range prs {
		prs[i].File.DecRef(prs[i].FileRange())
	}
}

// movePMAsLocked moves all pmas in oldAR to newAR.
//
// Preconditions:
//   - mm.activeMu must be locked for writing.
//   - oldAR.Length() != 0.
//   - oldAR.Length() <= newAR.Length().
//   - !oldAR.Overlaps(newAR).
//   - mm.pmas.IsEmptyRange(newAR).
//   - oldAR and newAR must be page-aligned.
func (mm *MemoryManager) movePMAsLocked(oldAR, newAR hostarch.AddrRange) {
	// The pages keep their offsets, out of the new addresses' slots.
	mm.markFoldableLocked()
	if checkInvariants {
		if !oldAR.WellFormed() || oldAR.Length() == 0 || !oldAR.IsPageAligned() {
			panic(fmt.Sprintf("invalid oldAR: %v", oldAR))
		}
		if !newAR.WellFormed() || newAR.Length() == 0 || !newAR.IsPageAligned() {
			panic(fmt.Sprintf("invalid newAR: %v", newAR))
		}
		if oldAR.Length() > newAR.Length() {
			panic(fmt.Sprintf("old address range %v may contain pmas that will not fit in new address range %v", oldAR, newAR))
		}
		if oldAR.Overlaps(newAR) {
			panic(fmt.Sprintf("old and new address ranges overlap: %v, %v", oldAR, newAR))
		}
		// mm.pmas.IsEmptyRange is checked by mm.pmas.Insert.
	}

	type movedPMA struct {
		oldAR hostarch.AddrRange
		pma   pma
	}
	var movedPMAs []movedPMA
	pseg := mm.pmas.LowerBoundSegment(oldAR.Start)
	for pseg.Ok() && pseg.Start() < oldAR.End {
		pseg = mm.pmas.Isolate(pseg, oldAR)
		movedPMAs = append(movedPMAs, movedPMA{
			oldAR: pseg.Range(),
			pma:   pseg.Value(),
		})
		pseg = mm.pmas.Remove(pseg).NextSegment()
		// No RSS change is needed since we're re-inserting the same pmas
		// below.
	}

	off := newAR.Start - oldAR.Start
	pgap := mm.pmas.FindGap(newAR.Start)
	for i := range movedPMAs {
		mpma := &movedPMAs[i]
		pmaNewAR := hostarch.AddrRange{mpma.oldAR.Start + off, mpma.oldAR.End + off}
		pgap = mm.pmas.Insert(pgap, pmaNewAR, mpma.pma).NextGap()
	}

	mm.unmapASLocked(oldAR)
}

// internalMappingsLocked returns cached internal mappings for addresses in ar.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - While mm.activeMu was locked, a call to
//     existingPMAsLocked(needInternalMappings=true) succeeded for all
//     addresses in ar.
//   - ar.Length() != 0.
//   - pseg.Range().Contains(ar.Start).
func (mm *MemoryManager) internalMappingsLocked(pseg pmaIterator, ar hostarch.AddrRange) safemem.BlockSeq {
	if checkInvariants {
		if !ar.WellFormed() || ar.Length() == 0 {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
		if !pseg.Range().Contains(ar.Start) {
			panic(fmt.Sprintf("initial pma %v does not cover start of ar %v", pseg.Range(), ar))
		}
	}

	if ar.End <= pseg.End() {
		// Since only one pma is involved, we can use pma.internalMappings
		// directly, avoiding a slice allocation.
		offset := uint64(ar.Start - pseg.Start())
		return pseg.ValuePtr().internalMappings.DropFirst64(offset).TakeFirst64(uint64(ar.Length()))
	}

	var ims []safemem.Block
	for {
		pr := pseg.Range().Intersect(ar)
		for pims := pseg.ValuePtr().internalMappings.DropFirst64(uint64(pr.Start - pseg.Start())).TakeFirst64(uint64(pr.Length())); !pims.IsEmpty(); pims = pims.Tail() {
			ims = append(ims, pims.Head())
		}
		if ar.End <= pseg.End() {
			break
		}
		pseg = pseg.NextSegment()
	}
	return safemem.BlockSeqFromSlice(ims)
}

// DropInternalMappingsForCompaction discards cached Sentry host mappings
// without changing PMAs or application page tables. Callers must stop every
// task that can use mm before calling this method; activeMu then drains any
// in-progress Sentry I/O that is using one of the cached slices.
func (mm *MemoryManager) DropInternalMappingsForCompaction() {
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	for pseg := mm.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
		pseg.ValuePtr().internalMappings = safemem.BlockSeq{}
	}
}

// vecInternalMappingsLocked returns cached internal mappings for addresses in
// ars.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - While mm.activeMu was locked, a call to
//     existingVecPMAsLocked(needInternalMappings=true) succeeded for all
//     addresses in ars.
func (mm *MemoryManager) vecInternalMappingsLocked(ars hostarch.AddrRangeSeq) safemem.BlockSeq {
	var ims []safemem.Block
	for ; !ars.IsEmpty(); ars = ars.Tail() {
		ar := ars.Head()
		if ar.Length() == 0 {
			continue
		}
		for pims := mm.internalMappingsLocked(mm.pmas.FindSegment(ar.Start), ar); !pims.IsEmpty(); pims = pims.Tail() {
			ims = append(ims, pims.Head())
		}
	}
	return safemem.BlockSeqFromSlice(ims)
}

// addRSSLocked updates the current and maximum resident set size of a
// MemoryManager to reflect the insertion of a pma at ar.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) addRSSLocked(ar hostarch.AddrRange) {
	mm.curRSS += uint64(ar.Length())
	if mm.curRSS > mm.maxRSS {
		mm.maxRSS = mm.curRSS
	}
}

// removeRSSLocked updates the current resident set size of a MemoryManager to
// reflect the removal of a pma at ar.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) removeRSSLocked(ar hostarch.AddrRange) {
	mm.curRSS -= uint64(ar.Length())
}

// pmaSetFunctions implements segment.Functions for pmaSet.
type pmaSetFunctions struct{}

func (pmaSetFunctions) MinKey() hostarch.Addr {
	return 0
}

func (pmaSetFunctions) MaxKey() hostarch.Addr {
	return ^hostarch.Addr(0)
}

func (pmaSetFunctions) ClearValue(pma *pma) {
	pma.file = nil
	pma.internalMappings = safemem.BlockSeq{}
}

func (pmaSetFunctions) Merge(ar1 hostarch.AddrRange, pma1 pma, ar2 hostarch.AddrRange, pma2 pma) (pma, bool) {
	if pma1.file != pma2.file ||
		pma1.off+uint64(ar1.Length()) != pma2.off ||
		pma1.translatePerms != pma2.translatePerms ||
		pma1.effectivePerms != pma2.effectivePerms ||
		pma1.maxPerms != pma2.maxPerms ||
		pma1.needCOW != pma2.needCOW ||
		pma1.private != pma2.private ||
		pma1.huge != pma2.huge {
		return pma{}, false
	}

	// Discard internal mappings instead of trying to merge them, since merging
	// them requires an allocation and getting them again from the
	// memmap.File might not.
	pma1.internalMappings = safemem.BlockSeq{}
	return pma1, true
}

func (pmaSetFunctions) Split(ar hostarch.AddrRange, p pma, split hostarch.Addr) (pma, pma) {
	newlen1 := uint64(split - ar.Start)
	p2 := p
	p2.off += newlen1
	if !p.internalMappings.IsEmpty() {
		p.internalMappings = p.internalMappings.TakeFirst64(newlen1)
		p2.internalMappings = p2.internalMappings.DropFirst64(newlen1)
	}
	return p, p2
}

// findOrSeekPrevUpperBoundPMA returns mm.pmas.UpperBoundSegment(addr), but may do
// so by scanning linearly backward from pgap.
//
// Preconditions:
//   - mm.activeMu must be locked.
//   - addr <= pgap.Start().
func (mm *MemoryManager) findOrSeekPrevUpperBoundPMA(addr hostarch.Addr, pgap pmaGapIterator) pmaIterator {
	if checkInvariants {
		if !pgap.Ok() {
			panic("terminal pma iterator")
		}
		if addr > pgap.Start() {
			panic(fmt.Sprintf("can't seek backward to %#x from %#x", addr, pgap.Start()))
		}
	}
	// Optimistically check if pgap.PrevSegment() is the PMA we're looking for,
	// which is the case if findOrSeekPrevUpperBoundPMA is called to find the
	// start of a range containing only a single PMA.
	if pseg := pgap.PrevSegment(); pseg.Start() <= addr {
		return pseg
	}
	return mm.pmas.UpperBoundSegment(addr)
}

// getInternalMappingsLocked ensures that pseg.ValuePtr().internalMappings is
// non-empty.
//
// Preconditions: mm.activeMu must be locked for writing.
func (pseg pmaIterator) getInternalMappingsLocked() error {
	pma := pseg.ValuePtr()
	if pma.internalMappings.IsEmpty() {
		// This must use maxPerms (instead of perms) because some permission
		// constraints are only visible to vmas; for example, mappings of
		// read-only files have vma.maxPerms.Write unset, but this may not be
		// visible to the memmap.Mappable.
		perms := pma.maxPerms
		// We will never execute application code through an internal mapping.
		perms.Execute = false
		ims, err := memmap.MapInternalForPMA(pma.file, pseg.fileRange(), perms)
		if err != nil {
			return err
		}
		pma.internalMappings = ims
	}
	return nil
}

func (pseg pmaIterator) fileRange() memmap.FileRange {
	return pseg.fileRangeOf(pseg.Range())
}

// Preconditions:
//   - pseg.Range().IsSupersetOf(ar).
//   - ar.Length != 0.
func (pseg pmaIterator) fileRangeOf(ar hostarch.AddrRange) memmap.FileRange {
	if checkInvariants {
		if !pseg.Ok() {
			panic("terminal pma iterator")
		}
		if !ar.WellFormed() || ar.Length() == 0 {
			panic(fmt.Sprintf("invalid ar: %v", ar))
		}
		if !pseg.Range().IsSupersetOf(ar) {
			panic(fmt.Sprintf("ar %v out of bounds %v", ar, pseg.Range()))
		}
	}

	pma := pseg.ValuePtr()
	pstart := pseg.Start()
	return memmap.FileRange{pma.off + uint64(ar.Start-pstart), pma.off + uint64(ar.End-pstart)}
}

// joinAddrRanges returns the smallest hostarch.AddrRange that is a superset of
// both ar1 and ar2. If either ar1 or ar2 have length 0, joinAddrRanges returns
// the other range. If both ar1 and ar2 have length 0, joinAddrRanges returns
// an unspecified range with length 0.
func joinAddrRanges(ar1, ar2 hostarch.AddrRange) hostarch.AddrRange {
	if ar1.Length() == 0 {
		return ar2
	}
	if ar2.Length() == 0 {
		return ar1
	}
	ar := ar1
	if ar.Start > ar2.Start {
		ar.Start = ar2.Start
	}
	if ar.End < ar2.End {
		ar.End = ar2.End
	}
	if checkInvariants {
		if !ar.IsSupersetOf(ar1) || !ar.IsSupersetOf(ar2) {
			panic(fmt.Sprintf("%v is not a superset of both %v and %v", ar, ar1, ar2))
		}
	}
	return ar
}

// pendingFileDecRefs accumulates released memmap.FileRange references so that
// calls to memmap.File.DecRef() can occur without holding locks.
type pendingFileDecRefs struct {
	slice []pendingFileDecRef
}

type pendingFileDecRef struct {
	file memmap.File
	fr   memmap.FileRange
	cow  bool
}

var pendingFileDecRefsPool = sync.Pool{
	New: func() any {
		return &pendingFileDecRefs{}
	},
}

func appendPendingFileDecRef(pfdrs *pendingFileDecRefs, file memmap.File, fr memmap.FileRange) *pendingFileDecRefs {
	if pfdrs == nil {
		pfdrs = pendingFileDecRefsPool.Get().(*pendingFileDecRefs)
	}
	pfdrs.slice = append(pfdrs.slice, pendingFileDecRef{file: file, fr: fr})
	return pfdrs
}

func appendPendingCOWFileDecRef(pfdrs *pendingFileDecRefs, file memmap.File, fr memmap.FileRange) *pendingFileDecRefs {
	pfdrs = appendPendingFileDecRef(pfdrs, file, fr)
	pfdrs.slice[len(pfdrs.slice)-1].cow = true
	return pfdrs
}

// Cleanup releases all references accumulated by pfdrs and releases ownership
// of pfdrs. pfdrs may be nil.
//
// Preconditions: No AddressSpace ranges may be awaiting unmapping (since such
// ranges may refer to memmap.File pages that will be dropped.)
func (pfdrs *pendingFileDecRefs) Cleanup() {
	if pfdrs == nil {
		return
	}
	for i := range pfdrs.slice {
		pfdr := &pfdrs.slice[i]
		if pfdr.cow {
			if mf, ok := pfdr.file.(*pgalloc.MemoryFile); ok {
				mf.NotifyRemoteCOW(pfdr.fr)
			}
		}
		pfdr.file.DecRef(pfdr.fr)
		pfdr.file = nil // allow GC
		pfdr.cow = false
	}
	pfdrs.slice = pfdrs.slice[:0]
	pendingFileDecRefsPool.Put(pfdrs)
}
