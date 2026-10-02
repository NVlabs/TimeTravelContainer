// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
)

// A device reads and writes the pages of a MemoryFile that a driver pinned
// for its DMA directly, at their host physical addresses. A live fork must
// not have its source copy such a page when it writes it, as it does its
// other private pages, which it shares with the fork's children: the device
// would go on using the old page. Its source keeps the pages, and the
// children get copies of them at once, as Linux's fork copies pinned pages
// for its child (see page_needs_cow_for_dma()).

// dmaPins counts the pins of ranges of a MemoryFile for devices' DMA.
type dmaPins struct {
	mu   sync.Mutex
	pins map[memmap.FileRange]int
}

// PinForDMA records that a device's DMA uses fr, until UnpinForDMA(fr).
func (f *MemoryFile) PinForDMA(fr memmap.FileRange) {
	f.dmaPins.mu.Lock()
	defer f.dmaPins.mu.Unlock()
	if f.dmaPins.pins == nil {
		f.dmaPins.pins = make(map[memmap.FileRange]int)
	}
	f.dmaPins.pins[fr]++
}

// UnpinForDMA undoes a call to PinForDMA(fr).
func (f *MemoryFile) UnpinForDMA(fr memmap.FileRange) {
	f.dmaPins.mu.Lock()
	defer f.dmaPins.mu.Unlock()
	if n := f.dmaPins.pins[fr]; n > 1 {
		f.dmaPins.pins[fr] = n - 1
	} else {
		delete(f.dmaPins.pins, fr)
	}
}

// DMAPinned returns whether a device's DMA uses a part of fr.
func (f *MemoryFile) DMAPinned(fr memmap.FileRange) bool {
	f.dmaPins.mu.Lock()
	defer f.dmaPins.mu.Unlock()
	for pinned := range f.dmaPins.pins {
		if pinned.Overlaps(fr) {
			return true
		}
	}
	return false
}

// AnyDMAPinned returns whether a device's DMA uses a part of the file.
func (f *MemoryFile) AnyDMAPinned() bool {
	f.dmaPins.mu.Lock()
	defer f.dmaPins.mu.Unlock()
	return len(f.dmaPins.pins) != 0
}
