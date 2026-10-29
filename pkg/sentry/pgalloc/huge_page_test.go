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
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
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
	if runs[0].First.Sequence%pagesPerHugePage() != 0 || runs[0].FirstVersion.Sequence%pagesPerHugePage() != 0 {
		t.Fatalf("2 MiB pages start at identity %+v and version %+v, want multiples of %d", runs[0].First, runs[0].FirstVersion, pagesPerHugePage())
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
		runs[0].FirstVersion.Sequence%pagesPerHugePage() != 0 {
		t.Fatalf("an owned 2 MiB page has runs %+v, want one 2 MiB page with a new version, a multiple of %d", runs, pagesPerHugePage())
	}
	gotIDs, gotVersions := basePageVersions(t, f, fr)
	if !reflect.DeepEqual(gotIDs, ids) {
		t.Fatal("OwnForWrite changed identities")
	}
	if !reflect.DeepEqual(gotVersions[pagesPerHugePage():], versions[pagesPerHugePage():]) {
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
	for i := pagesPerHugePage(); i < 2*pagesPerHugePage(); i++ {
		if owned := i == pagesPerHugePage()+4 || i == pagesPerHugePage()+5; owned == (gotVersions[i] == renewed[i]) {
			t.Fatalf("base page %d of the split page has version %+v, had %+v; owned: %t", i-pagesPerHugePage(), gotVersions[i], renewed[i], owned)
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
		runs[0].FirstVersion == versions[0] || runs[0].FirstVersion.Sequence%pagesPerHugePage() != 0 {
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

// newHugeRemoteTestMemoryFile returns a MemoryFile, backed by a file, whose
// huge allocations may be 2 MiB pages.
func newHugeRemoteTestMemoryFile(t *testing.T) *MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-huge-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	return mf
}

// 2 MiB pages stay 2 MiB pages through the allocator metadata that a remote
// fork or a dehydration saves.
func TestRemoteAllocatorMetadataPreserves2MiBPages(t *testing.T) {
	source := newHugeRemoteTestMemoryFile(t)
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

// A capture for a remote fork binds a 2 MiB page that it holds whole once,
// with the most references any part of it has, and the stream it orders
// holds it in one piece, a unit that pushes take whole. One for a
// dehydration binds its base pages.
func TestHugePageSnapshotLayoutBinds2MiBPagesWhole(t *testing.T) {
	f := newHugeRemoteTestMemoryFile(t)
	defer f.Destroy()
	hp, err := f.Allocate(hugepage, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate, Huge: true, LogicalHugePages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(hp)
	small, err := f.Allocate(2*page, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(small)
	ids, versions := basePageVersions(t, f, hp)
	capture := NewMemoryRangeCapture()
	// One owner maps the 2 MiB page whole, another its first half again.
	for _, fr := range []memmap.FileRange{hp, {Start: hp.Start, End: hp.Start + hugepage/2}, small} {
		if err := capture.Record(f, fr); err != nil {
			t.Fatal(err)
		}
	}
	layout, err := capture.HugePageSnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}
	var huge []CapturedPageBinding
	for _, binding := range layout.Bindings {
		if binding.Huge {
			huge = append(huge, binding)
		}
	}
	if len(huge) != 1 || len(layout.Bindings) != 3 {
		t.Fatalf("bindings %+v, want one of the 2 MiB page and two of base pages", layout.Bindings)
	}
	if b := huge[0]; b.OldOffset != hp.Start || b.Page != ids[0] || b.Version != versions[0] || b.References != 2 || b.Length() != hugepage || !ValidHugePageBinding(&b) {
		t.Fatalf("2 MiB page binding %+v, want offset %#x, page %+v, version %+v and 2 references", b, hp.Start, ids[0], versions[0])
	}
	expanded := BasePageBindings(layout.Bindings)
	if len(expanded) != int(pagesPerHugePage())+2 {
		t.Fatalf("%d base pages, want %d", len(expanded), pagesPerHugePage()+2)
	}
	for _, binding := range expanded {
		if k := (binding.OldOffset - hp.Start) / page; binding.OldOffset >= hp.Start && binding.OldOffset < hp.End &&
			(binding.Huge || binding.Page != ids[k] || binding.Version != versions[k] || binding.StreamOffset != huge[0].StreamOffset+k*page) {
			t.Fatalf("base page %d of the 2 MiB page is bound as %+v", k, binding)
		}
	}
	if base, err := capture.SnapshotLayout(); err != nil || len(base.Bindings) != int(pagesPerHugePage())+2 {
		t.Fatalf("a dehydration's capture has %d bindings (%v), want %d base pages", len(base.Bindings), err, pagesPerHugePage()+2)
	}

	ordered, err := layout.ChunkOrdered()
	if err != nil {
		t.Fatal(err)
	}
	if first := ordered.Bindings[0]; !first.Huge || first.StreamOffset != 0 || ordered.Bindings[1].StreamOffset != hugepage || ordered.Bindings[2].StreamOffset != hugepage+page {
		t.Fatalf("ordered bindings %+v, want the 2 MiB page first in 2 MiB of the stream", ordered.Bindings)
	}
	snapshot, err := NewRemoteSnapshot(ordered.Regions)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if !snapshot.UsesAtomicPushChunks() {
		t.Fatal("a chunk-ordered snapshot does not push whole units")
	}
	if got, err := snapshot.AtomicPushRanges([]RemotePushHint{{StreamOffset: 5 * page, Length: page}}, hugepage); err != nil || len(got) != 1 || got[0] != (RemotePushHint{StreamOffset: 0, Length: hugepage}) {
		t.Fatalf("a push of a page of the 2 MiB page is %+v (%v), want the whole page", got, err)
	}
	if got, err := snapshot.AtomicPushRanges([]RemotePushHint{{StreamOffset: 5 * page, Length: page}}, SparseChunkBytes); err != nil || len(got) != 0 {
		t.Fatalf("pushes of 1 MiB push %+v (%v) of a 2 MiB page, want none", got, err)
	}
	if got, err := snapshot.AtomicPushRanges([]RemotePushHint{{StreamOffset: hugepage + page, Length: page}}, hugepage); err != nil || len(got) != 1 || got[0] != (RemotePushHint{StreamOffset: hugepage, Length: 2 * page}) {
		t.Fatalf("a push of a base page is %+v (%v), want its logical chunk's", got, err)
	}
}

// A remote fork's target installs a 2 MiB page in an aligned 2 MiB page, as
// one page, read in one piece, which a later fork of it reuses; a target
// without huge pages installs its base pages.
func TestInstallRemoteBindingsInstalls2MiBPagesWhole(t *testing.T) {
	var allocator LogicalAllocatorID
	allocator[0] = 5
	page2M := LogicalPageID{Allocator: allocator, Sequence: 3 * pagesPerHugePage()}
	version2M := PageVersionID{Allocator: allocator, Sequence: 7 * pagesPerHugePage()}
	smallPage := LogicalPageID{Allocator: allocator, Sequence: 9*pagesPerHugePage() + 1}
	bindings := []CapturedPageBinding{
		{OldOffset: 4 * hugepage, StreamOffset: 0, References: 1, Page: page2M, Version: version2M, Huge: true},
		{OldOffset: 0, StreamOffset: hugepage, References: 1, Page: smallPage, Version: PageVersionID{Allocator: allocator, Sequence: 99}},
	}
	for i := range bindings {
		bindings[i].Chunk, bindings[i].Slot = bindings[i].Page.Chunk()
	}
	old := memmap.FileRange{Start: 4 * hugepage, End: 5 * hugepage}

	target := newHugeRemoteTestMemoryFile(t)
	defer target.Destroy()
	installation, relocation, loads, err := target.InstallRemoteBindings(bindings, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	relocated, err := relocation.Range(checkpoint.ResourceID{}, old)
	if err != nil {
		t.Fatal(err)
	}
	if !target.IsHugePages(relocated) {
		t.Fatalf("the 2 MiB page is installed at %v, not as one", relocated)
	}
	if runs := target.LogicalPageRanges(relocated); len(runs) != 1 || runs[0].First != page2M || runs[0].FirstVersion != version2M {
		t.Fatalf("the installed 2 MiB page has runs %+v, want %+v at %+v", runs, page2M, version2M)
	}
	found := false
	for _, load := range loads {
		if load.Target == relocated {
			found = load.StreamOffset == 0 && load.logicalChunk != nil && len(load.logicalChunk.ranges) == 1
		}
	}
	if !found || len(loads) != 2 {
		t.Fatalf("loads %+v, want the 2 MiB page in one piece and a base page", loads)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}

	again, againRelocation, loads, err := target.InstallRemoteBindings(bindings[:1], usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := againRelocation.Range(checkpoint.ResourceID{}, old); err != nil || got != relocated || len(loads) != 0 {
		t.Fatalf("a second fork installs the 2 MiB page at %v (%v) with loads %+v, want %v reused", got, err, loads, relocated)
	}
	if err := again.Commit(); err != nil {
		t.Fatal(err)
	}

	plain := newEmptyRemoteTestMemoryFile(t)
	defer plain.Destroy()
	plainInstallation, plainRelocation, loads, err := plain.InstallRemoteBindings(bindings[:1], usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer plainInstallation.Abort()
	runs, err := plainRelocation.Runs(checkpoint.ResourceID{}, old)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		for _, logical := range plain.LogicalPageRanges(run) {
			if logical.Huge {
				t.Fatalf("a target without huge pages installed %+v as a 2 MiB page", logical)
			}
		}
	}
	units := make(map[*logicalChunkLoad]uint64)
	for _, load := range loads {
		units[load.logicalChunk] += load.Target.Length()
	}
	if len(units) != 2 {
		t.Fatalf("a target without huge pages loads the 2 MiB page in units %+v, want two logical chunks", units)
	}
}
