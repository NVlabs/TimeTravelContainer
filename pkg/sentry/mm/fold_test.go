// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/usermem"
)

// foldAll runs memMgr's fold steps until a pass ends.
func foldAll(t *testing.T, memMgr *MemoryManager) {
	t.Helper()
	for i := 0; ; i++ {
		if _, done := memMgr.FoldStep(1 << 30); done {
			return
		}
		if i > 1000 {
			t.Fatal("a fold pass does not end")
		}
	}
}

// foldedPage is a page's identity and version, and the byte it starts with.
type foldedPage struct {
	id      pgalloc.LogicalPageID
	version pgalloc.PageVersionID
	b       byte
}

// foldedPages returns the identities, versions and first bytes of memMgr's
// pages pages from addr.
func foldedPages(ctx context.Context, t *testing.T, memMgr *MemoryManager, addr hostarch.Addr, pages int) []foldedPage {
	t.Helper()
	vs := make([]foldedPage, pages)
	for page := range vs {
		pageAddr := addr + hostarch.Addr(page)*hostarch.PageSize
		id, version, ok := memMgr.mf.LogicalPageVersionAt(fileOffsetAt(t, memMgr, pageAddr))
		if !ok {
			t.Fatalf("page %d has no identity", page)
		}
		var b [1]byte
		if _, err := memMgr.CopyIn(ctx, pageAddr, b[:], usermem.IOOpts{}); err != nil {
			t.Fatalf("reading page %d: %v", page, err)
		}
		vs[page] = foldedPage{id: id, version: version, b: b[0]}
	}
	return vs
}

// writePages writes byte b to every other page of memMgr's pages pages from
// addr, from page first on.
func writePages(ctx context.Context, t *testing.T, memMgr *MemoryManager, addr hostarch.Addr, first, pages int, b byte) {
	t.Helper()
	for page := first; page < pages; page += 2 {
		if _, err := memMgr.CopyOut(ctx, addr+hostarch.Addr(page)*hostarch.PageSize, []byte{b}, usermem.IOOpts{}); err != nil {
			t.Fatalf("writing page %d: %v", page, err)
		}
	}
}

// A parent that wrote pages while its fork shared them has its copies in a
// layer; once the fork is gone and the parent has written the rest, in place,
// its pages alternate between two homes. A fold moves them into one, at their
// slots, with their contents, identities and versions, and the address space
// maps them with one pma again.
func TestFoldAfterForkReleased(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 64
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	home := checkAddressOrdered(t, parent, addr, pages)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	// Copies of the even pages go to a layer.
	writePages(ctx, t, parent, addr, 0, pages, 2)
	child.DecUsers(ctx)
	// The odd pages are the parent's alone: written in place.
	writePages(ctx, t, parent, addr, 1, pages, 3)
	ar := hostarch.AddrRange{Start: addr, End: addr + pages*hostarch.PageSize}
	if n := countPMAs(parent, ar); n < pages/2 {
		t.Fatalf("before the fold, %d pmas map the parent's pages, want at least %d", n, pages/2)
	}
	before := foldedPages(ctx, t, parent, addr, pages)
	foldAll(t, parent)
	if got := checkAddressOrdered(t, parent, addr, pages); got != home {
		t.Errorf("the pages were folded into %#x, want the home %#x that held half of them first", got, home)
	}
	after := foldedPages(ctx, t, parent, addr, pages)
	for page := range before {
		if after[page] != before[page] {
			t.Errorf("page %d was %+v before the fold, %+v after", page, before[page], after[page])
		}
		if want := byte(2 + page%2); after[page].b != want {
			t.Errorf("page %d holds %d, want %d", page, after[page].b, want)
		}
	}
	if homes := parent.homes[uint64(addr)/homeBlockSize]; len(homes) != 1 || homes[0].Start != home {
		t.Errorf("the parent's homes for the block are %v, want only %#x", homes, home)
	}
}

// A fork whose parent is gone holds alone the pages it did not write, in the
// parent's home, an orphan. A fold adopts that home, which holds most of the
// fork's pages, and moves the fork's own copies there.
func TestFoldAdoptsReleasedParentsHome(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	const pages = 64
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	home := checkAddressOrdered(t, parent, addr, pages)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	// The child's copies of the even pages of the first quarter go to its
	// own home.
	writePages(ctx, t, child, addr, 0, pages/4, 2)
	parent.DecUsers(ctx)
	if _, owner, ok := child.mf.HomeOf(home); !ok || owner != 0 {
		t.Fatalf("the released parent's home %#x has owner %d, want none", home, owner)
	}
	writePages(ctx, t, child, addr, 1, pages, 3)
	foldAll(t, child)
	if got := checkAddressOrdered(t, child, addr, pages); got != home {
		t.Errorf("the child's pages were folded into %#x, want the parent's home %#x, which held most", got, home)
	}
	if _, owner, _ := child.mf.HomeOf(home); owner != child.homeOwner {
		t.Errorf("the parent's home %#x has owner %d, want the child, %d", home, owner, child.homeOwner)
	}
	for page, v := range foldedPages(ctx, t, child, addr, pages) {
		want := byte(1)
		switch {
		case page%2 == 1:
			want = 3
		case page < pages/4:
			want = 2
		}
		if v.b != want {
			t.Errorf("page %d holds %d, want %d", page, v.b, want)
		}
	}
}

// Pages that a fork still shares stay where they are: a fold moves only what
// its address space holds alone, and leaves the rest for a later fold.
func TestFoldLeavesSharedPages(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 32
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	writePages(ctx, t, parent, addr, 0, pages, 2)
	before := foldedPages(ctx, t, parent, addr, pages)
	childBefore := foldedPages(ctx, t, child, addr, pages)
	offsets := make([]uint64, pages)
	for page := range offsets {
		offsets[page] = fileOffsetAt(t, parent, addr+hostarch.Addr(page)*hostarch.PageSize)
	}
	foldAll(t, parent)
	foldAll(t, child)
	for page := range offsets {
		if off := fileOffsetAt(t, parent, addr+hostarch.Addr(page)*hostarch.PageSize); off != offsets[page] {
			t.Errorf("page %d moved from %#x to %#x, though its slot holds a page the child shares", page, offsets[page], off)
		}
	}
	if after := foldedPages(ctx, t, parent, addr, pages); len(after) != len(before) {
		t.Fatal("pages lost")
	} else {
		for page := range after {
			if after[page] != before[page] {
				t.Errorf("parent's page %d was %+v, is %+v", page, before[page], after[page])
			}
		}
	}
	for page, v := range foldedPages(ctx, t, child, addr, pages) {
		if v != childBefore[page] {
			t.Errorf("child's page %d was %+v, is %+v", page, childBefore[page], v)
		}
	}
	parent.activeMu.RLock()
	foldable := parent.fold.foldable
	parent.activeMu.RUnlock()
	if !foldable {
		t.Error("the parent left pages out of their home but no longer asks to fold")
	}
}

// Two passes in a row that moved nothing and left pages that a fork shares
// put off the next for foldResidueWait, however soon it is asked for, and it
// says so, so that it is asked for again then. One such pass does not.
func TestFoldWaitsAfterLeavingSharedPages(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 32
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	// Copies of the even pages go to a layer: the child shares what their
	// slots hold.
	writePages(ctx, t, parent, addr, 0, pages, 2)
	for pass := 1; pass <= 2; pass++ {
		if n, done := parent.FoldStep(1 << 30); n == 0 || !done || parent.FoldDeferred() {
			t.Fatalf("pass %d did %d bytes of work, done %t, put off %t; want it to look at the copies and leave them", pass, n, done, parent.FoldDeferred())
		}
	}
	if n, done := parent.FoldStep(1 << 30); n != 0 || !done || !parent.FoldDeferred() {
		t.Errorf("a third pass asked for at once did %d bytes of work, done %t, put off %t; want it put off", n, done, parent.FoldDeferred())
	}
	defer func(wait time.Duration) { foldResidueWait = wait }(foldResidueWait)
	foldResidueWait = 0
	if n, _ := parent.FoldStep(1 << 30); n == 0 || parent.FoldDeferred() {
		t.Errorf("a pass asked for once foldResidueWait passed did %d bytes of work, put off %t; want it to look again", n, parent.FoldDeferred())
	}
}

// A 2 MiB page freed in part splits into base pages at their slots. Once the
// freed page is written again, its 2 MiB is whole and the address space's
// alone, and a fold step collapses it into a 2 MiB page again, mapped by one
// pma, with what it held.
func TestCollapseRefilled2MiBPage(t *testing.T) {
	if unix.Getpagesize() != hostarch.PageSize {
		t.Skip("a host page holds several base pages, and is resident while any is")
	}
	defer func(age time.Duration) { collapseAge = age }(collapseAge)
	var collapsed []memmap.FileRange
	defer pgalloc.SetCollapseHostForTest(func(_ uintptr, fr memmap.FileRange) error {
		collapsed = append(collapsed, fr)
		return nil
	})()
	ctx, memMgr := homeTestMemoryManager(t, true)
	defer memMgr.DecUsers(ctx)
	addr := mapPrivate(ctx, t, memMgr, 4*hostarch.HugePageSize)
	start := addr.MustHugeRoundUp()
	ar := hostarch.AddrRange{Start: start, End: start + hostarch.HugePageSize}
	const pages = hostarch.HugePageSize / hostarch.PageSize
	for page := 0; page < pages; page++ {
		if _, err := memMgr.CopyOut(ctx, start+hostarch.Addr(page)*hostarch.PageSize, []byte{1}, usermem.IOOpts{}); err != nil {
			t.Fatalf("writing page %d: %v", page, err)
		}
	}
	fr := memmap.FileRange{Start: fileOffsetAt(t, memMgr, start)}
	fr.End = fr.Start + hostarch.HugePageSize
	if !memMgr.mf.IsHugePages(fr) {
		t.Fatalf("the written 2 MiB at %#x is not a 2 MiB page", start)
	}
	const freed = 7
	if err := memMgr.Decommit(start+freed*hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatalf("Decommit: %v", err)
	}
	if memMgr.mf.IsHugePages(fr) {
		t.Fatal("a 2 MiB page freed in part is still one")
	}
	// Its 2 MiB waits for the freed page to be written again: collapsing it
	// now would fill that page with zeroes.
	collapseAge = 0
	foldAll(t, memMgr)
	if len(collapsed) != 0 {
		t.Fatalf("collapsed %v with a page freed", collapsed)
	}
	if !memMgr.CollapseWaiting() {
		t.Fatal("the freed 2 MiB does not wait to be collapsed")
	}
	if _, err := memMgr.CopyOut(ctx, start+freed*hostarch.PageSize, []byte{2}, usermem.IOOpts{}); err != nil {
		t.Fatalf("writing the freed page again: %v", err)
	}
	if off := fileOffsetAt(t, memMgr, start+freed*hostarch.PageSize); off != fr.Start+freed*hostarch.PageSize {
		t.Fatalf("the page written again is at %#x, not at its slot %#x", off, fr.Start+freed*hostarch.PageSize)
	}
	// A candidate waits collapseAge first, as a short-lived process's memory
	// goes before it is worth collapsing.
	collapseAge = time.Hour
	foldAll(t, memMgr)
	if len(collapsed) != 0 || !memMgr.CollapseWaiting() {
		t.Fatalf("collapsed %v before its candidate waited", collapsed)
	}
	collapseAge = 0
	foldAll(t, memMgr)
	if len(collapsed) != 1 || collapsed[0] != fr {
		t.Fatalf("collapsed %v, want %v", collapsed, fr)
	}
	if !memMgr.mf.IsHugePages(fr) {
		t.Error("the refilled 2 MiB is not a 2 MiB page")
	}
	if n := countPMAs(memMgr, ar); n != 1 {
		t.Errorf("the collapsed 2 MiB is mapped by %d pmas, want 1", n)
	}
	for page := 0; page < pages; page++ {
		var b [1]byte
		if _, err := memMgr.CopyIn(ctx, start+hostarch.Addr(page)*hostarch.PageSize, b[:], usermem.IOOpts{}); err != nil {
			t.Fatalf("reading page %d: %v", page, err)
		}
		want := byte(1)
		if page == freed {
			want = 2
		}
		if b[0] != want {
			t.Fatalf("page %d holds %d, want %d", page, b[0], want)
		}
	}
}

// A live fork's clone is of its source's fork group, and a new address space
// of a group of its own. An address space that releases its pages asks only
// its group to fold: only its group shared pages with it. An address space
// restored from a save, and its fork, are of group 0, any, as a save keeps no
// groups, and ask every group.
func TestFoldGroups(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	other := testMemoryManager(ctx, t)
	defer other.DecUsers(ctx)
	if parent.FoldGroup() == 0 || other.FoldGroup() == parent.FoldGroup() {
		t.Fatalf("new address spaces are of groups %d and %d, want a group each", parent.FoldGroup(), other.FoldGroup())
	}
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	if child.FoldGroup() != parent.FoldGroup() {
		t.Errorf("a live fork's clone is of group %d, want its source's, %d", child.FoldGroup(), parent.FoldGroup())
	}
	var asked []uint64
	defer func(hook func(*pgalloc.MemoryFile, uint64)) { FoldRequestHook = hook }(FoldRequestHook)
	FoldRequestHook = func(_ *pgalloc.MemoryFile, group uint64) { asked = append(asked, group) }
	child.DecUsers(ctx)
	if len(asked) != 1 || asked[0] != parent.FoldGroup() {
		t.Errorf("the released clone asked groups %v to fold, want its own, %d", asked, parent.FoldGroup())
	}

	parent.mf.MarkSavable()
	var metadata bytes.Buffer
	if _, err := state.Save(ctx, &metadata, parent); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	restored := &MemoryManager{}
	if _, err := state.Load(ctx, &metadata, restored); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	fork, err := restored.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if restored.FoldGroup() != 0 || fork.FoldGroup() != 0 {
		t.Errorf("a restored address space and its fork are of groups %d and %d, want 0, any", restored.FoldGroup(), fork.FoldGroup())
	}
	asked = nil
	fork.DecUsers(ctx)
	restored.DecUsers(ctx)
	if len(asked) != 2 || asked[0] != 0 || asked[1] != 0 {
		t.Errorf("the released restored address space and its fork asked groups %v to fold, want 0, any, each", asked)
	}
}

// residentVersions returns the versions of the pages that ranges hold, as
// ttc stats once counted them, page by page.
func residentVersions(ranges []pgalloc.CapturedMemoryRange) map[pgalloc.PageVersionID]struct{} {
	versions := make(map[pgalloc.PageVersionID]struct{})
	for _, r := range ranges {
		for _, run := range r.MemoryFile.LogicalPageRanges(r.Range) {
			for page := uint64(0); page < run.Range.Length()/hostarch.PageSize; page++ {
				versions[pgalloc.PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + page}] = struct{}{}
			}
		}
	}
	return versions
}

// residentPages returns the pages that ranges of one MemoryFile hold, by
// offset.
func residentPages(ranges []pgalloc.CapturedMemoryRange) map[uint64]struct{} {
	pages := make(map[uint64]struct{})
	for _, r := range ranges {
		for off := r.Range.Start; off < r.Range.End; off += hostarch.PageSize {
			pages[off] = struct{}{}
		}
	}
	return pages
}

// Two address spaces share a page exactly where their mappings' ranges of the
// MemoryFile overlap, as they share a version: a copy on write gives the copy
// a version of its own, and a fold moves a page with its version. So that ttc
// stats can count shared pages by ranges rather than versions.
func TestResidentRangesShareAsVersions(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 64
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	check := func(when string) {
		t.Helper()
		parentRanges, childRanges := parent.AppendResidentFileRanges(nil), child.AppendResidentFileRanges(nil)
		parentVersions, childVersions := residentVersions(parentRanges), residentVersions(childRanges)
		parentPages, childPages := residentPages(parentRanges), residentPages(childRanges)
		if len(parentVersions) != len(parentPages) || len(childVersions) != len(childPages) {
			t.Fatalf("%s: %d and %d versions in %d and %d pages, want one each", when, len(parentVersions), len(childVersions), len(parentPages), len(childPages))
		}
		sharedVersions, sharedPages := 0, 0
		for version := range parentVersions {
			if _, ok := childVersions[version]; ok {
				sharedVersions++
			}
		}
		for page := range parentPages {
			if _, ok := childPages[page]; ok {
				sharedPages++
			}
		}
		if sharedVersions != sharedPages {
			t.Errorf("%s: %d shared versions, but %d shared pages", when, sharedVersions, sharedPages)
		}
	}
	check("cloned")
	// The parent's copies of the even pages go to a layer; the child's of
	// some odd ones to its home.
	writePages(ctx, t, parent, addr, 0, pages, 2)
	writePages(ctx, t, child, addr, 1, pages/2, 3)
	check("after copies on write")
	child.DecUsers(ctx)
	writePages(ctx, t, parent, addr, 1, pages, 4)
	foldAll(t, parent)
	ranges := parent.AppendResidentFileRanges(nil)
	if versions, pages := residentVersions(ranges), residentPages(ranges); len(versions) != len(pages) {
		t.Errorf("after a fold, %d versions in %d pages, want one each", len(versions), len(pages))
	}
}

// An address space whose fork took over pages in place, at slots of the
// address space's home, cannot fold its own copies of them there while a
// later fork holds them. Once that fork's fold moves them to a home of its
// own, it asks their group to fold again, and the address space's next fold
// puts its pages in one home: whichever of the two folds first.
func TestFoldAfterAForkLeavesItsSlots(t *testing.T) {
	ctx, main := homeTestMemoryManager(t, false)
	defer main.DecUsers(ctx)
	const pages = 64
	addr := mapPrivate(ctx, t, main, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, main, addr, pages, 1)
	home := checkAddressOrdered(t, main, addr, pages)
	x, err := cloneMemoryManagerMetadata(ctx, main)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	// main's copies of the even pages go to a layer: x shares the
	// originals. x then holds those alone, and takes them over in place,
	// at main's slots; it copies the odd ones, which main shares.
	writePages(ctx, t, main, addr, 0, pages, 2)
	writePagesInRandomOrder(ctx, t, x, addr, pages, 3)
	y, err := cloneMemoryManagerMetadata(ctx, x)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer y.DecUsers(ctx)
	x.DecUsers(ctx)
	// main folds first: y holds the pages at the slots of its copies.
	foldAll(t, main)
	if off := fileOffsetAt(t, main, addr); off == home {
		t.Fatalf("main's copy of page 0 is at its slot %#x though y holds the page there", off)
	}
	var asked []uint64
	defer func(hook func(*pgalloc.MemoryFile, uint64)) { FoldRequestHook = hook }(FoldRequestHook)
	FoldRequestHook = func(_ *pgalloc.MemoryFile, group uint64) { asked = append(asked, group) }
	// y folds its pages into x's home, which it adopts, from main's slots.
	foldAll(t, y)
	if !slices.Contains(asked, main.FoldGroup()) {
		t.Fatalf("y's fold out of main's home asked groups %v to fold, want main's, %d", asked, main.FoldGroup())
	}
	// main writes the odd pages, its own now, in place, as the address
	// space that TestFoldAfterForkReleased folds does.
	writePages(ctx, t, main, addr, 1, pages, 4)
	foldAll(t, main)
	if got := checkAddressOrdered(t, main, addr, pages); got != home {
		t.Errorf("main's pages were folded into %#x, want its home %#x", got, home)
	}
	for page, v := range foldedPages(ctx, t, main, addr, pages) {
		if want := byte(2 + 2*(page%2)); v.b != want {
			t.Errorf("main's page %d holds %d, want %d", page, v.b, want)
		}
	}
}
