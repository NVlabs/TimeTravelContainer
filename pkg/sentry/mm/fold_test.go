// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
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
