// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package pgalloc

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Darwin has no Linux transparent-hugepage advice. GuestRAM manages host
// residency in explicit 16K slabs instead.
func adviseChunkMapping(uintptr, uintptr, bool, bool, bool) {}

func canPopulateMadv() bool              { return false }
func tryPopulateMadv(safemem.Block) bool { return false }

// File growth already establishes the addressable range. APFS allocates data
// blocks on first write, so no separate correctness-critical preallocation is
// required here.
func commitHostFile(int, memmap.FileRange) error { return nil }

// Returning EOPNOTSUPP selects MemoryFile's mandatory manual-zero fallback.
func decommitHostFile(int, memmap.FileRange) error { return unix.EOPNOTSUPP }

// reserveAddressRange reserves nothing: a platform backing maps its chunks
// itself.
func reserveAddressRange() (base, length uintptr) { return 0, 0 }

// releaseAddressRange does nothing, as reserveAddressRange reserves nothing.
func releaseAddressRange(base, length uintptr) {}

// collapseFileRange collapses nothing: a platform backing has no home chunks.
func collapseFileRange(uintptr, memmap.FileRange) error { return errNoHostCollapse }

// populateHugeFileRange does nothing: a platform backing has no home chunks.
func populateHugeFileRange(uintptr, memmap.FileRange, bool, safemem.ReaderFunc) (uint64, bool, error) {
	return 0, false, nil
}
