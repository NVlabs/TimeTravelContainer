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
