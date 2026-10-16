// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// A MemoryFile counts a page from its allocation, in a home's slot too, until
// its release, and says when it may hold more than its commit limit.
func TestCommitLimit(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 1)
	page := uint64(hostarch.PageSize)
	accounted := func(want uint64, over bool) {
		t.Helper()
		if got := mf.AccountedBytes(); got != want*page {
			t.Errorf("AccountedBytes() = %d pages, want %d", got/page, want)
		}
		if got := mf.OverCommitLimit(); got != over {
			t.Errorf("OverCommitLimit() = %t, want %t", got, over)
		}
	}
	mf.SetCommitLimit(3 * page)
	accounted(0, false)
	fr, err := mf.Allocate(2*page, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	accounted(2, false)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	slots := slotRange(home, 10, 2)
	if _, err := mf.AllocateAt(slots, AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatal(err)
	}
	accounted(4, true)
	// Freed pages count until they are released, as they stay committed.
	mf.DecRef(slots)
	accounted(4, true)
	waitForRelease(t, mf)
	accounted(2, false)
	mf.SetCommitLimit(page)
	accounted(2, true)
	mf.SetCommitLimit(0)
	accounted(2, false)
	mf.DecRef(fr)
	waitForRelease(t, mf)
	accounted(0, false)
}
