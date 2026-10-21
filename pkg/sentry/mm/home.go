// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"errors"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// homeBlockSize is the size of a block of addresses that a home holds (see
// pgalloc.MemoryFile.ReserveHome): a chunk of the memory file.
const homeBlockSize = pgalloc.BackingChunkSize

// maxHomeLayers is how many homes an address space reserves for one block:
// the first, and the layers it places a page in when the page's slot in the
// first still holds a page that a fork shares.
const maxHomeLayers = 4

// initHomes gives mm a home owner of its own, if its MemoryFile has homes.
func (mm *MemoryManager) initHomes() {
	if mm.mf != nil && mm.mf.HomesEnabled() {
		mm.homeOwner = pgalloc.NewHomeOwner()
	}
}

// allocatePrivateLocked allocates memory for the private pages of ar, as
// pgalloc.MemoryFile.Allocate does with opts, at their slots in mm's homes
// where it can: so that pages adjacent in the address space are adjacent in
// the file, and the platform maps them with one host mapping, whatever order
// they are allocated in. See g3doc/proposals/ttc_address_ordered_memory.md.
//
// Preconditions:
//   - mm.activeMu must be locked for writing.
//   - ar is page-aligned and lies in one block of homeBlockSize.
func (mm *MemoryManager) allocatePrivateLocked(ar hostarch.AddrRange, opts pgalloc.AllocOpts) (memmap.FileRange, error) {
	if mm.homeOwner != 0 {
		block := uint64(ar.Start) / homeBlockSize
		off := uint64(ar.Start) % homeBlockSize
		slots := func(home memmap.FileRange) memmap.FileRange {
			return memmap.FileRange{Start: home.Start + off, End: home.Start + off + uint64(ar.Length())}
		}
		homes := mm.homes[block]
		for i, home := range homes {
			if fr, err := mm.mf.AllocateAt(slots(home), opts); !errors.Is(err, pgalloc.ErrSlotsInUse) {
				if i != 0 {
					// Out of the first home, until a fold.
					mm.markFoldableLocked()
				}
				if err == nil && !opts.Huge {
					mm.noteCollapsibleLocked(ar, fr)
				}
				return fr, err
			}
		}
		if len(homes) < maxHomeLayers {
			if home, err := mm.mf.ReserveHome(mm.homeOwner); err == nil {
				if mm.homes == nil {
					mm.homes = make(map[uint64][]memmap.FileRange)
				}
				mm.homes[block] = append(homes, home)
				if len(homes) != 0 {
					mm.markFoldableLocked()
				}
				if fr, err := mm.mf.AllocateAt(slots(home), opts); !errors.Is(err, pgalloc.ErrSlotsInUse) {
					if err == nil && !opts.Huge {
						mm.noteCollapsibleLocked(ar, fr)
					}
					return fr, err
				}
			}
		}
		mm.markFoldableLocked()
	}
	return mm.mf.Allocate(uint64(ar.Length()), opts)
}

// homeBlockEnd returns the end of the block of homeBlockSize that addr is in.
func homeBlockEnd(addr hostarch.Addr) hostarch.Addr {
	return (addr &^ (homeBlockSize - 1)) + homeBlockSize
}

// disownHomesLocked gives up mm's homes, once mm has released its pages: a
// home is spared for another address space once no page of it is left.
//
// Preconditions: mm.activeMu must be locked for writing.
func (mm *MemoryManager) disownHomesLocked() {
	for _, homes := range mm.homes {
		for _, home := range homes {
			mm.mf.DisownHome(mm.homeOwner, home)
		}
	}
	mm.homes = nil
	mm.fold = foldState{}
	if mm.homeOwner != 0 {
		// Others of its fork group may now hold alone the pages they shared
		// with mm.
		requestFolds(mm.mf, mm.foldGroup)
	}
}
