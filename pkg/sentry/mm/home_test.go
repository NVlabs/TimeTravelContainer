// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"math/rand"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/usermem"
)

// homeTestMemoryManager returns a MemoryManager whose MemoryFile has homes,
// and whose huge allocations are 2 MiB pages if huge.
func homeTestMemoryManager(t *testing.T, huge bool) (context.Context, *MemoryManager) {
	t.Helper()
	ctx := contexttest.Context(t)
	mf := newRemoteCloneMemoryFileWithOpts(t, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         huge,
		MaxHomes:                16,
	})
	t.Cleanup(mf.Destroy)
	ctx = context.WithValues(ctx, map[any]any{pgalloc.CtxMemoryFile: mf})
	memMgr := testMemoryManager(ctx, t)
	return ctx, memMgr
}

// writePagesInRandomOrder writes byte b to each of the pages pages of memMgr
// from addr, in a random order.
func writePagesInRandomOrder(ctx context.Context, t *testing.T, memMgr *MemoryManager, addr hostarch.Addr, pages int, b byte) {
	t.Helper()
	for _, page := range rand.New(rand.NewSource(int64(pages))).Perm(pages) {
		if _, err := memMgr.CopyOut(ctx, addr+hostarch.Addr(page)*hostarch.PageSize, []byte{b}, usermem.IOOpts{}); err != nil {
			t.Fatalf("writing page %d: %v", page, err)
		}
	}
}

// checkAddressOrdered checks that memMgr's pages from addr are in one home
// chunk, each at its slot, mapped by one pma, and returns the home's start.
func checkAddressOrdered(t *testing.T, memMgr *MemoryManager, addr hostarch.Addr, pages int) uint64 {
	t.Helper()
	home := fileOffsetAt(t, memMgr, addr) &^ (homeBlockSize - 1)
	for page := 0; page < pages; page++ {
		pageAddr := addr + hostarch.Addr(page)*hostarch.PageSize
		if off := fileOffsetAt(t, memMgr, pageAddr); off != home+uint64(pageAddr)%homeBlockSize {
			t.Fatalf("page %d at %#x is at offset %#x, not at its slot %#x of home %#x", page, pageAddr, off, home+uint64(pageAddr)%homeBlockSize, home)
		}
	}
	ar := hostarch.AddrRange{Start: addr, End: addr + hostarch.Addr(pages)*hostarch.PageSize}
	if n := countPMAs(memMgr, ar); n != 1 {
		t.Fatalf("%v is mapped by %d pmas, want 1", ar, n)
	}
	return home
}

// countPMAs returns how many pmas map ar.
func countPMAs(memMgr *MemoryManager, ar hostarch.AddrRange) int {
	n := 0
	for pseg := memMgr.pmas.LowerBoundSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		n++
	}
	return n
}

// mapPrivate maps length bytes of private anonymous memory in memMgr, and
// faults none of it.
func mapPrivate(ctx context.Context, t *testing.T, memMgr *MemoryManager, length uint64) hostarch.Addr {
	t.Helper()
	addr, err := memMgr.MMap(ctx, memmap.MMapOpts{
		Length:   length,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	return addr
}

// Private pages faulted in a random order are in address order in the file:
// at their slots of one home, mapped by one pma, as the host maps them with
// one mapping.
func TestHomesPlacePrivatePagesInAddressOrder(t *testing.T) {
	ctx, memMgr := homeTestMemoryManager(t, false)
	defer memMgr.DecUsers(ctx)
	const pages = 300
	addr := mapPrivate(ctx, t, memMgr, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, memMgr, addr, pages, 1)
	checkAddressOrdered(t, memMgr, addr, pages)
}

// After a fork, the child's copies go to its own home, at their slots,
// whatever order it writes them in; the parent's go to a second home, a
// layer, since their slots in its first still hold the pages the child
// shares.
func TestHomesForkCopies(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 64
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	parentHome := checkAddressOrdered(t, parent, addr, pages)

	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	writePagesInRandomOrder(ctx, t, child, addr, pages, 2)
	if childHome := checkAddressOrdered(t, child, addr, pages); childHome == parentHome {
		t.Fatalf("the child's copies are in the parent's home %#x", parentHome)
	}

	// The parent's pages are its own again, as the child copied every one;
	// a write takes them over in place.
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 3)
	if home := checkAddressOrdered(t, parent, addr, pages); home != parentHome {
		t.Fatalf("the parent's pages moved from home %#x to %#x", parentHome, home)
	}
}

// A parent that writes pages it still shares with its child copies them into
// a layer: a second home, at the same slots.
func TestHomesLayer(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 32
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	parentHome := checkAddressOrdered(t, parent, addr, pages)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)

	writePagesInRandomOrder(ctx, t, parent, addr, pages, 2)
	if layer := checkAddressOrdered(t, parent, addr, pages); layer == parentHome {
		t.Fatalf("the parent's copies are in its first home %#x, whose slots the child still uses", parentHome)
	}
	for page := 0; page < pages; page++ {
		if off := fileOffsetAt(t, child, addr+hostarch.Addr(page)*hostarch.PageSize); off != parentHome+uint64(addr+hostarch.Addr(page)*hostarch.PageSize)%homeBlockSize {
			t.Fatalf("the child's page %d moved to %#x", page, off)
		}
	}
}

// A 2 MiB page is placed at its aligned slot, as one page.
func TestHomesPlace2MiBPages(t *testing.T) {
	ctx, memMgr := homeTestMemoryManager(t, true)
	defer memMgr.DecUsers(ctx)
	addr := mapPrivate(ctx, t, memMgr, 3*hostarch.HugePageSize)
	base := addr.HugeRoundDown() + hostarch.HugePageSize
	if _, err := memMgr.CopyOut(ctx, base+5*hostarch.PageSize, []byte{1}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	off := fileOffsetAt(t, memMgr, base)
	if off%homeBlockSize != uint64(base)%homeBlockSize {
		t.Fatalf("the 2 MiB page at %#x is at offset %#x, not at its slot", base, off)
	}
	if _, ok := memMgr.mf.HugePageAt(off); !ok {
		t.Fatalf("the memory at %#x is not a 2 MiB page", base)
	}
}

// An address space gives up its homes when it is released.
func TestHomesDisowned(t *testing.T) {
	ctx, memMgr := homeTestMemoryManager(t, false)
	addr := mapPrivate(ctx, t, memMgr, 4*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, memMgr, addr, 4, 1)
	off := fileOffsetAt(t, memMgr, addr)
	if _, owner, ok := memMgr.mf.HomeOf(off); !ok || owner != memMgr.homeOwner {
		t.Fatalf("HomeOf(%#x) = %d, %t, want the address space's home", off, owner, ok)
	}
	mf := memMgr.mf
	memMgr.DecUsers(ctx)
	if _, owner, ok := mf.HomeOf(off); !ok || owner != 0 {
		t.Fatalf("HomeOf(%#x) after release = %d, %t, want a home with no owner", off, owner, ok)
	}
}

// recordingAddressSpace records the longest mapping it was asked for.
type recordingAddressSpace struct {
	platform.AddressSpace
	longest uint64
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *recordingAddressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	as.longest = max(as.longest, fr.Length())
	return as.AddressSpace.MapFile(addr, f, fr, at, precommit)
}

// A fault that inserts pmas maps what it inserted, not the rest of the pma
// they merge with, which is mapped already: mapping it again would drop its
// host page tables. A fault on a pma that exists maps all of it.
func TestFaultMapsOnlyAroundWhatItInserted(t *testing.T) {
	ctx, memMgr := homeTestMemoryManager(t, false)
	defer memMgr.DecUsers(ctx)
	if memMgr.p.MapUnit() != 0 {
		t.Skipf("the platform maps %d bytes at a time", memMgr.p.MapUnit())
	}
	const length = 16 << 20
	addr := mapPrivate(ctx, t, memMgr, length)
	as := &recordingAddressSpace{AddressSpace: memMgr.as}
	memMgr.as = as
	for block := addr; block < addr+length; block += hostarch.HugePageSize {
		if err := memMgr.HandleUserFault(ctx, block, hostarch.Write, 0); err != nil {
			t.Fatal(err)
		}
	}
	memMgr.as = as.AddressSpace
	if n := countPMAs(memMgr, hostarch.AddrRange{Start: addr, End: addr + length}); n != 1 {
		t.Fatalf("%d pmas map the faulted range, want 1", n)
	}
	if as.longest > hostarch.HugePageSize {
		t.Fatalf("a fault that allocated mapped %d bytes, more than the 2 MiB it allocated", as.longest)
	}

	child, err := cloneMemoryManagerMetadata(ctx, memMgr)
	if err != nil {
		t.Fatal(err)
	}
	defer child.DecUsers(ctx)
	childAS := &recordingAddressSpace{AddressSpace: child.as}
	child.as = childAS
	if err := child.HandleUserFault(ctx, addr, hostarch.Read, 0); err != nil {
		t.Fatal(err)
	}
	if childAS.longest != length {
		t.Fatalf("a read fault on an existing pma mapped %d bytes, want all %d", childAS.longest, length)
	}
	// The child's copies, at their slots, merge with each other; each write
	// fault maps the page it copied.
	childAS.longest = 0
	for _, page := range rand.New(rand.NewSource(1)).Perm(64) {
		if err := child.HandleUserFault(ctx, addr+hostarch.Addr(page*7)*hostarch.PageSize, hostarch.Write, 0); err != nil {
			t.Fatal(err)
		}
	}
	child.as = childAS.AddressSpace
	if childAS.longest != hostarch.PageSize {
		t.Fatalf("write faults that copied a page each mapped up to %d bytes, want a page", childAS.longest)
	}
}

// A parent writes a page that its fork has since copied, while the fork
// still shares other pages of the same pma: the parent holds that page alone
// and takes it over in place, at its slot, as Linux reuses a page that only
// the writer maps, rather than copy it into a layer.
func TestHomesParentTakesOverPagesItAloneHolds(t *testing.T) {
	ctx, parent := homeTestMemoryManager(t, false)
	defer parent.DecUsers(ctx)
	const pages = 16
	addr := mapPrivate(ctx, t, parent, pages*hostarch.PageSize)
	writePagesInRandomOrder(ctx, t, parent, addr, pages, 1)
	home := checkAddressOrdered(t, parent, addr, pages)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	// The child copies page 5 only.
	page := addr + 5*hostarch.PageSize
	if _, err := child.CopyOut(ctx, page, []byte{2}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	_, before := pageVersion(t, parent, page)
	if _, err := parent.CopyOut(ctx, page, []byte{3}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	if off := fileOffsetAt(t, parent, page); off != home+uint64(page)%homeBlockSize {
		t.Fatalf("the parent's page moved from its slot to %#x", off)
	}
	if _, after := pageVersion(t, parent, page); after == before {
		t.Fatal("the page the parent took over kept its version")
	}
	// Its other pages are still shared, and a write copies them.
	other := addr + 9*hostarch.PageSize
	if !sharesPage(t, parent, child, other) {
		t.Fatal("a page the child did not write is not shared")
	}
	if _, err := parent.CopyOut(ctx, other, []byte{3}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	if sharesPage(t, parent, child, other) || fileOffsetAt(t, parent, other)&^(homeBlockSize-1) == home {
		t.Fatal("a shared page was written in place")
	}
}
