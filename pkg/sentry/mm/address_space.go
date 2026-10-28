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
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// AddressSpace returns the platform.AddressSpace bound to mm.
//
// Preconditions: mm.users != 0.
func (mm *MemoryManager) AddressSpace() platform.AddressSpace {
	return mm.as
}

// UsesAddressSpace returns true if as is mm's AddressSpace.
func (mm *MemoryManager) UsesAddressSpace(as platform.AddressSpace) bool {
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	return mm.as == as
}

// mapASLocked maps addresses in ar into mm.as.
//
// Preconditions:
//   - ar.Length() != 0.
//   - ar must be page-aligned.
//   - pseg == mm.pmas.LowerBoundSegment(ar.Start).
//
// +checklocksread:mm.activeMu
func (mm *MemoryManager) mapASLocked(ctx context.Context, pseg pmaIterator, ar hostarch.AddrRange, platformEffect memmap.MMapPlatformEffect) error {
	return mm.mapASWindowLocked(ctx, pseg, ar, platformEffect, hostarch.AddrRange{})
}

// mapASExactLocked is mapASLocked, but an address space with homes maps only
// ar. Its callers have just made ar usable (a small mmap they populate, or
// memory the Sentry copies into), and the pmas of ar may have merged with
// neighbors that are mapped already (see mapASWindowLocked).
//
// Preconditions: as for mapASLocked.
func (mm *MemoryManager) mapASExactLocked(ctx context.Context, pseg pmaIterator, ar hostarch.AddrRange, platformEffect memmap.MMapPlatformEffect) error {
	var window hostarch.AddrRange
	if mm.homeOwner != 0 {
		window = ar
	}
	return mm.mapASWindowLocked(ctx, pseg, ar, platformEffect, window)
}

// mapASWindowLocked is mapASLocked, but maps nothing outside window, unless
// window is empty; an address space with homes maps nothing outside the 2 MiB
// around ar then.
//
// A fault that inserted pmas maps only what it inserted and the pages it
// faulted (see HandleUserFault): a pma it inserted may merge with pmas mapped
// already, as an address space's private pages do whatever order they come in
// (see allocatePrivateLocked), and a platform that maps whole pmas (MapUnit 0)
// would map those again, which drops their host page tables and makes the
// application fault on each of their pages once more. The host merges the
// new mapping with theirs. An address space with homes has the same problem
// with every other mapping it makes, such as populating a small mmap.
//
// Preconditions: as for mapASLocked; window, if not empty, contains ar.
func (mm *MemoryManager) mapASWindowLocked(ctx context.Context, pseg pmaIterator, ar hostarch.AddrRange, platformEffect memmap.MMapPlatformEffect, window hostarch.AddrRange) error {
	if platformEffect == memmap.PlatformEffectCommit && ar.Length() > (1<<30) {
		// FIXME(b/445932215, b/445939339): Don't precommit very large ranges
		// via platform.AddressSpace.MapFile() for now, since holding locks
		// while doing so causes problems.
		platformEffect = memmap.PlatformEffectPopulate
	}
	// By default, map entire pmas at a time, under the assumption that there
	// is no cost to mapping more of a pma than necessary.
	mapAR := hostarch.AddrRange{0, ^hostarch.Addr(hostarch.PageSize - 1)}
	setMapUnit := func(mapUnit uint64) {
		mapMask := hostarch.Addr(mapUnit - 1)
		mapAR.Start = ar.Start &^ mapMask
		// If rounding ar.End up overflows, just keep the existing mapAR.End.
		if end := (ar.End + mapMask) &^ mapMask; end >= ar.End {
			mapAR.End = end
		}
	}
	if platformEffect != memmap.PlatformEffectDefault {
		// When explicitly committing, only map ar, since overmapping may incur
		// unexpected resource usage. When explicitly populating, do the same
		// since an underlying device file may be sensitive to the mapped
		// range.
		mapAR = ar
	} else if mapUnit := mm.p.MapUnit(); mapUnit != 0 {
		// Limit the range we map to ar, aligned to mapUnit.
		setMapUnit(mapUnit)
	} else if mf, ok := pseg.ValuePtr().file.(*pgalloc.MemoryFile); ok && mf.IsAsyncLoading() {
		// Demand-only remote loading must not map an entire PMA: MapFile may
		// call MemoryFile.MapInternal for the complete mapping and thereby
		// materialize untouched pages. Follow the active prefetch window so
		// transfer granularity remains a policy choice. Ordinary background
		// checkpoint restore retains its historical 32 MiB cap.
		if unit := mf.AsyncLoadingMapUnit(pseg.ValuePtr().off + uint64(max(ar.Start, pseg.Start())-pseg.Start())); unit != 0 {
			setMapUnit(unit)
		} else {
			setMapUnit(32 << 20)
		}
	}
	if window.Length() == 0 && mm.homeOwner != 0 {
		// An address space's private pages at adjacent slots of its homes
		// make one pma, however many allocations made them: a whole pma
		// may be all of a process's memory, mapped already, and mapping it
		// again for a page would drop the host page tables of all of it.
		// Map as much as an allocation makes (see getPMAsInternalLocked).
		window = hugepageAligned(ar)
	}
	if window.Length() != 0 {
		mapAR = mapAR.Intersect(window)
	}
	if checkInvariants {
		if !mapAR.IsSupersetOf(ar) {
			panic(fmt.Sprintf("mapAR %#v is not a superset of ar %#v", mapAR, ar))
		}
	}

	// Since this checks ar.End and not mapAR.End, we will never map a pma that
	// is not required.
	for pseg.Ok() && pseg.Start() < ar.End {
		pma := pseg.ValuePtr()
		pmaAR := pseg.Range()
		pmaMapAR := pmaAR.Intersect(mapAR)
		perms := pma.effectivePerms
		if pma.needCOW {
			perms.Write = false
		}
		if perms.Any() { // MapFile precondition
			// If the length of the mapping exceeds singleMapThreshold, call
			// AddressSpace.MapFile() on singleMapThreshold-aligned chunks so
			// we can check ctx.Killed() reasonably frequently.
			const singleMapThreshold = 1 << 30
			if mf, ok := pma.file.(*pgalloc.MemoryFile); ok && mf.IsAsyncLoading() {
				// Pages this maps that a remote fork or a restore has yet
				// to load go where the task's own would.
				mf.PlaceUnloaded(pseg.fileRangeOf(pmaMapAR), taskPlacement(ctx, pmaMapAR.Start))
			}
			if pmaMapAR.Length() <= singleMapThreshold {
				if err := mm.mapFileLocked(pmaMapAR.Start, pma.file, pseg.fileRangeOf(pmaMapAR), perms, platformEffect == memmap.PlatformEffectCommit); err != nil {
					return err
				}
				if ctx.Killed() {
					return linuxerr.EINTR
				}
			} else {
				for windowStart := pmaMapAR.Start &^ (singleMapThreshold - 1); windowStart < pmaMapAR.End; windowStart += singleMapThreshold {
					windowAR := hostarch.AddrRange{windowStart, windowStart + singleMapThreshold}
					thisMapAR := pmaMapAR.Intersect(windowAR)
					if err := mm.mapFileLocked(thisMapAR.Start, pma.file, pseg.fileRangeOf(thisMapAR), perms, platformEffect == memmap.PlatformEffectCommit); err != nil {
						return err
					}
					if ctx.Killed() {
						return linuxerr.EINTR
					}
				}
			}
		}
		pseg = pseg.NextSegment()
	}
	return nil
}

// mapFileLocked is mm.as.MapFile, but if the host has no room for another
// mapping (ENOMEM), it unmaps all of the address space, which faults map
// again, and tries once more. A platform that maps each pma with a host
// mapping of its own runs out of them on a host whose vm.max_map_count is
// low: copy-on-write that copies scattered 4 KiB pages leaves a pma for each,
// and a fork's random writes over 1 GB of them took all 262144 of a host's,
// failing its next fault with SIGSEGV.
//
// Preconditions: mm.activeMu must be locked.
func (mm *MemoryManager) mapFileLocked(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	err := mm.as.MapFile(addr, f, fr, at, precommit)
	if errors.Is(err, unix.ENOMEM) {
		// A fold may make fewer mappings of what mm maps.
		if mm.homeOwner != 0 {
			mm.fold.foldable = true
			requestFolds(mm.mf, mm.foldGroup)
		}
		mm.unmapASLocked(mm.applicationAddrRange())
		err = mm.as.MapFile(addr, f, fr, at, precommit)
	}
	return err
}

// unmapASLocked removes all AddressSpace mappings for addresses in ar.
//
// +checklocksread:mm.activeMu
func (mm *MemoryManager) unmapASLocked(ar hostarch.AddrRange) {
	if ar.Length() == 0 {
		return
	}
	if mm.as == nil {
		// Being called from mm.DecUsers() after AddressSpace release.
		return
	}
	// unmapASLocked doesn't require vmas or pmas to exist for ar, so it can be
	// passed ranges that include addresses that can't be mapped by the
	// application.
	ar = ar.Intersect(mm.applicationAddrRange())
	mm.as.Unmap(ar.Start, uint64(ar.Length()))
}
