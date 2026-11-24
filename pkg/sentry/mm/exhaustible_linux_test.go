// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// testMemoryManagerWithExhaustibleMemory returns a MemoryManager whose memory
// file is backed by a new memfd, and a function that exhausts the memory file
// by sealing the memfd against growth and allocating all of its free pages,
// causing later allocations from it to fail. Since the memory file releases
// freed pages asynchronously, pages freed before calling exhaust may become
// allocatable after it returns, so callers must not free memory from the
// memory file before calling exhaust.
func testMemoryManagerWithExhaustibleMemory(ctx context.Context, t *testing.T) (*MemoryManager, func()) {
	const name = "mm-test-memory"
	memfd, err := memutil.CreateMemFD(name, unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatalf("CreateMemFD got err %v want nil", err)
	}
	memfile := os.NewFile(uintptr(memfd), name)
	mf, err := pgalloc.NewMemoryFile(memfile, pgalloc.MemoryFileOpts{
		DisableMemoryAccounting: true,
	})
	if err != nil {
		memfile.Close()
		t.Fatalf("NewMemoryFile got err %v want nil", err)
	}
	mm := testMemoryManager(ctx, t)
	mm.mf = mf // mm has no mappings yet.

	exhaust := func() {
		t.Helper()
		if _, err := unix.FcntlInt(uintptr(memfd), unix.F_ADD_SEALS, unix.F_SEAL_GROW); err != nil {
			t.Fatalf("fcntl(F_ADD_SEALS, F_SEAL_GROW) got err %v want nil", err)
		}
		var frs []memmap.FileRange
		t.Cleanup(func() {
			for _, fr := range frs {
				mf.DecRef(fr)
			}
		})
		// Since mf can no longer grow, this terminates once all of its free
		// pages are allocated. The allocated pages are never committed.
		for length := uint64(1 << 30); length >= pageSize; length /= 2 {
			for {
				fr, err := mf.Allocate(length, pgalloc.AllocOpts{})
				if err != nil {
					break
				}
				frs = append(frs, fr)
			}
		}
	}
	return mm, exhaust
}
