// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	stdcontext "context"
	"os"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// newHugePageTestMemoryFile returns a MemoryFile, without a backing file,
// whose huge allocations may be 2 MiB pages.
func newHugePageTestMemoryFile() *MemoryFile {
	f := newLogicalPageTestMemoryFile()
	f.opts.ExpectHugepages = true
	return f
}

// allocateTestPages allocates length bytes of f, as 2 MiB pages if huge.
func allocateTestPages(t *testing.T, f *MemoryFile, length uint64, huge bool) memmap.FileRange {
	t.Helper()
	fr, err := f.findAllocatableAndMarkUsed(&allocState{
		length: length,
		huge:   huge,
		opts:   AllocOpts{Huge: huge, LogicalHugePages: huge, Dir: BottomUp},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fr
}

// basePageVersions returns the identity and version of each base page of fr.
func basePageVersions(t *testing.T, f *MemoryFile, fr memmap.FileRange) ([]LogicalPageID, []PageVersionID) {
	t.Helper()
	var ids []LogicalPageID
	var versions []PageVersionID
	for off := fr.Start; off < fr.End; off += page {
		id, version, ok := f.LogicalPageVersionAt(off)
		if !ok {
			t.Fatalf("page at %#x has no identity", off)
		}
		ids, versions = append(ids, id), append(versions, version)
	}
	return ids, versions
}

// A huge allocation asked for as 2 MiB pages is 2 MiB pages, whose identities
// and versions start at multiples of 512, whatever base pages came before.
func TestHugeAllocationIs2MiBPages(t *testing.T) {
	f := newHugePageTestMemoryFile()
	small := allocateTestPages(t, f, 3*page, false)
	defer f.DecRef(small)
	fr := allocateTestPages(t, f, 2*hugepage, true)
	defer f.DecRef(fr)

	runs := f.LogicalPageRanges(fr)
	if len(runs) != 1 || !runs[0].Huge {
		t.Fatalf("2 MiB pages %v have runs %+v, want one run of 2 MiB pages", fr, runs)
	}
	if runs[0].First.Sequence%pagesPerHugePage != 0 || runs[0].FirstVersion.Sequence%pagesPerHugePage != 0 {
		t.Fatalf("2 MiB pages start at identity %+v and version %+v, want multiples of %d", runs[0].First, runs[0].FirstVersion, pagesPerHugePage)
	}
	if hp, ok := f.HugePageAt(fr.Start + hugepage + 5*page); !ok || hp != (memmap.FileRange{Start: fr.Start + hugepage, End: fr.End}) {
		t.Fatalf("HugePageAt(%#x) = %v, %t; want the second 2 MiB page of %v", fr.Start+hugepage+5*page, hp, ok, fr)
	}
	if _, ok := f.HugePageAt(small.Start); ok {
		t.Fatal("a base page is part of a 2 MiB page")
	}

	// Huge memory not asked for as 2 MiB pages, as files hold, is base pages.
	plain, err := f.findAllocatableAndMarkUsed(&allocState{length: hugepage, huge: true, opts: AllocOpts{Huge: true, Dir: BottomUp}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(plain)
	if runs := f.LogicalPageRanges(plain); len(runs) != 1 || runs[0].Huge {
		t.Fatalf("huge memory not asked for as 2 MiB pages has runs %+v, want one of base pages", runs)
	}
}

// Taking over a whole 2 MiB page gives it one new version and keeps it one
// page. Taking over part of one splits it and renews only that part, as Linux
// splits a huge page that a process maps in part.
func TestOwnForWriteOf2MiBPages(t *testing.T) {
	f := newHugePageTestMemoryFile()
	fr := allocateTestPages(t, f, 2*hugepage, true)
	defer f.DecRef(fr)
	first := memmap.FileRange{Start: fr.Start, End: fr.Start + hugepage}
	second := memmap.FileRange{Start: first.End, End: fr.End}
	ids, versions := basePageVersions(t, f, fr)

	if !f.OwnForWrite(first) {
		t.Fatal("OwnForWrite refused a 2 MiB page held alone")
	}
	if runs := f.LogicalPageRanges(first); len(runs) != 1 || !runs[0].Huge || runs[0].FirstVersion == versions[0] ||
		runs[0].FirstVersion.Sequence%pagesPerHugePage != 0 {
		t.Fatalf("an owned 2 MiB page has runs %+v, want one 2 MiB page with a new version, a multiple of %d", runs, pagesPerHugePage)
	}
	gotIDs, gotVersions := basePageVersions(t, f, fr)
	if !reflect.DeepEqual(gotIDs, ids) {
		t.Fatal("OwnForWrite changed identities")
	}
	if !reflect.DeepEqual(gotVersions[pagesPerHugePage:], versions[pagesPerHugePage:]) {
		t.Fatal("owning one 2 MiB page renewed its neighbour's version")
	}

	part := memmap.FileRange{Start: second.Start + 4*page, End: second.Start + 6*page}
	if !f.OwnForWrite(part) {
		t.Fatal("OwnForWrite refused pages held alone")
	}
	if _, ok := f.HugePageAt(second.Start); ok {
		t.Fatal("a 2 MiB page owned in part is still one page")
	}
	if _, ok := f.HugePageAt(first.Start); !ok {
		t.Fatal("splitting a 2 MiB page split its neighbour")
	}
	renewed := gotVersions
	gotIDs, gotVersions = basePageVersions(t, f, fr)
	if !reflect.DeepEqual(gotIDs, ids) {
		t.Fatal("splitting a 2 MiB page changed identities")
	}
	for i := pagesPerHugePage; i < 2*pagesPerHugePage; i++ {
		if owned := i == pagesPerHugePage+4 || i == pagesPerHugePage+5; owned == (gotVersions[i] == renewed[i]) {
			t.Fatalf("base page %d of the split page has version %+v, had %+v; owned: %t", i-pagesPerHugePage, gotVersions[i], renewed[i], owned)
		}
	}
}

// A 2 MiB page that is freed in part splits into base pages, which keep their
// identities and versions, as Linux splits a huge page unmapped in part.
func TestPartlyFreed2MiBPageSplits(t *testing.T) {
	f := newHugePageTestMemoryFile()
	fr := allocateTestPages(t, f, 2*hugepage, true)
	ids, versions := basePageVersions(t, f, fr)
	freed := memmap.FileRange{Start: fr.Start, End: fr.Start + page}
	f.DecRef(freed)
	rest := memmap.FileRange{Start: freed.End, End: fr.End}
	defer f.DecRef(rest)

	if _, ok := f.LogicalPageIDAt(freed.Start); ok {
		t.Fatal("the freed page kept its identity")
	}
	if _, ok := f.HugePageAt(rest.Start); ok {
		t.Fatal("a 2 MiB page freed in part is still one page")
	}
	if _, ok := f.HugePageAt(fr.Start + hugepage); !ok {
		t.Fatal("freeing part of a 2 MiB page split its neighbour")
	}
	if gotIDs, gotVersions := basePageVersions(t, f, rest); !reflect.DeepEqual(gotIDs, ids[1:]) || !reflect.DeepEqual(gotVersions, versions[1:]) {
		t.Fatal("the rest of a split 2 MiB page changed identities or versions")
	}
}

// A copy of a whole 2 MiB page into a 2 MiB page is one page, with the
// original's identity and one new version. A copy of part of one, or into
// memory not allocated as 2 MiB pages, is base pages.
func TestAdoptCOWIdentitiesOf2MiBPages(t *testing.T) {
	f := newHugePageTestMemoryFile()
	source := allocateTestPages(t, f, hugepage, true)
	defer f.DecRef(source)
	ids, versions := basePageVersions(t, f, source)

	whole := allocateTestPages(t, f, hugepage, true)
	defer f.DecRef(whole)
	if err := f.AdoptCOWIdentities(f, source, whole); err != nil {
		t.Fatal(err)
	}
	if runs := f.LogicalPageRanges(whole); len(runs) != 1 || !runs[0].Huge || runs[0].First != ids[0] ||
		runs[0].FirstVersion == versions[0] || runs[0].FirstVersion.Sequence%pagesPerHugePage != 0 {
		t.Fatalf("a whole copy of 2 MiB page %+v (version %+v) has runs %+v, want one 2 MiB page with its identity and a new version", ids[0], versions[0], runs)
	}

	part := allocateTestPages(t, f, 2*page, false)
	defer f.DecRef(part)
	if err := f.AdoptCOWIdentities(f, memmap.FileRange{Start: source.Start + 7*page, End: source.Start + 9*page}, part); err != nil {
		t.Fatal(err)
	}
	if gotIDs, gotVersions := basePageVersions(t, f, part); gotIDs[0] != ids[7] || gotIDs[1] != ids[8] || gotVersions[0] == versions[7] || gotVersions[1] == versions[8] {
		t.Fatalf("a copy of part of a 2 MiB page has identities %+v and versions %+v", gotIDs, gotVersions)
	}
	if _, ok := f.HugePageAt(part.Start); ok {
		t.Fatal("a copy of part of a 2 MiB page is part of a 2 MiB page")
	}
	if _, ok := f.HugePageAt(source.Start); !ok {
		t.Fatal("copying part of a 2 MiB page split it")
	}

	plain, err := f.findAllocatableAndMarkUsed(&allocState{length: hugepage, huge: true, opts: AllocOpts{Huge: true, Dir: BottomUp}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(plain)
	if err := f.AdoptCOWIdentities(f, source, plain); err != nil {
		t.Fatal(err)
	}
	if runs := f.LogicalPageRanges(plain); len(runs) != 1 || runs[0].Huge || runs[0].First != ids[0] {
		t.Fatalf("a copy into memory not allocated as 2 MiB pages has runs %+v, want base pages with the original's identities", runs)
	}
}

// Images saved before 2 MiB pages existed still load, as base pages: a
// logical page segment saves the fields it saved then, and marks 2 MiB pages
// in one of them.
func TestLogicalPageInfoSavesItsEarlierFields(t *testing.T) {
	if got, want := (&logicalPageInfo{}).StateFields(), []string{"Allocator", "FirstSequence", "VersionAllocator", "FirstVersionSequence"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("logicalPageInfo saves fields %v, want %v", got, want)
	}
	var loaded logicalPageInfo
	loaded.loadFirstSequence(stdcontext.Background(), (&logicalPageInfo{FirstSequence: 1024, Huge: true}).saveFirstSequence())
	if loaded.FirstSequence != 1024 || !loaded.Huge {
		t.Fatalf("2 MiB pages at sequence 1024 load as %+v", loaded)
	}
	var earlier logicalPageInfo
	earlier.loadFirstSequence(stdcontext.Background(), 1024)
	if earlier.FirstSequence != 1024 || earlier.Huge {
		t.Fatalf("base pages saved at sequence 1024 load as %+v", earlier)
	}
}

// 2 MiB pages stay 2 MiB pages through the allocator metadata that a remote
// fork or a dehydration saves.
func TestRemoteAllocatorMetadataPreserves2MiBPages(t *testing.T) {
	file, err := os.CreateTemp("", "ttc-huge-source-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	source, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer source.Destroy()
	fr, err := source.Allocate(hugepage, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate, Huge: true, LogicalHugePages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer source.DecRef(fr)
	want := source.LogicalPageRanges(fr)
	if len(want) != 1 || !want[0].Huge {
		t.Fatalf("source runs %+v, want one 2 MiB page", want)
	}
	metadata, _, _, err := ExportMemoryFileMetadataForRemote(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	target := newEmptyRemoteTestMemoryFile(t)
	defer target.Destroy()
	if err := ImportMemoryFileMetadataForRemote(context.Background(), target, metadata); err != nil {
		t.Fatal(err)
	}
	defer target.DecRef(fr)
	if got := target.LogicalPageRanges(fr); !reflect.DeepEqual(got, want) {
		t.Fatalf("imported runs %+v, want %+v", got, want)
	}
}
