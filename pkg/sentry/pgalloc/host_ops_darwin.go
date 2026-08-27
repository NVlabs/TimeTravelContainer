// Copyright 2026 The gVisor Authors.
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

//go:build darwin

package pgalloc

import (
	"os"

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

// APFS clonefile is a whole-file operation; packed snapshots request arbitrary
// extents, so let the caller's explicitly authorized copy fallback handle it.
func cloneHostFileRange(*os.File, *os.File, uint64, uint64, uint64) error {
	return unix.EOPNOTSUPP
}
