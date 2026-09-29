// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	goContext "context"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

func TestRelocateFileRangeSetPreservesTreeLinks(t *testing.T) {
	ctx := contexttest.Context(t)
	mf := pgalloc.MemoryFileFromContext(ctx)
	const pages = 64
	var set FileRangeSet
	whole := memmap.MappableRange{Start: 0, End: pages * hostarch.PageSize}
	set.Insert(set.FindGap(0), whole, 0)

	relocation := pgalloc.NewSparseRelocation()
	for page := uint64(0); page < pages; page++ {
		old := page * hostarch.PageSize
		// A gap between every destination page prevents the relocated extents
		// from merging, forcing the generated set to have internal nodes.
		if err := relocation.Add(mf.ResourceID(), old, 2*old); err != nil {
			t.Fatal(err)
		}
	}
	restoreCtx := goContext.WithValue(ctx, pgalloc.CtxSparseRelocation, relocation)
	if err := RelocateFileRangeSet(restoreCtx, mf, &set); err != nil {
		t.Fatalf("RelocateFileRangeSet: %v", err)
	}

	segments := 0
	for seg := set.FirstSegment(); seg.Ok(); {
		segments++
		gap := set.Remove(seg)
		if !gap.Ok() {
			break
		}
		seg = gap.NextSegment()
	}
	if segments != pages {
		t.Fatalf("removed %d relocated segments, want %d", segments, pages)
	}
	if !set.IsEmpty() {
		t.Fatal("relocated set is not empty after removing all segments")
	}
}
