// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// fillSlotsInRandomOrder allocates the base pages of fr, in a home, one at a
// time in a random order, and writes b to them.
func fillSlotsInRandomOrder(t *testing.T, mf *MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	pages := int(fr.Length() / hostarch.PageSize)
	for _, page := range rand.New(rand.NewSource(int64(fr.Start))).Perm(pages) {
		slot := slotRange(memmap.FileRange{Start: fr.Start}, uint64(page), 1)
		if _, err := mf.AllocateAt(slot, AllocOpts{Kind: usage.Anonymous}); err != nil {
			t.Fatalf("AllocateAt(%v): %v", slot, err)
		}
		fillPages(t, mf, slot, b)
	}
}

// A 2 MiB range of a home whose base pages one holder holds alone, all
// present, becomes one 2 MiB page with one new version; one whose pages are
// held by others too stays as it is, as does one the host has no huge page
// for.
func TestCollapseHuge(t *testing.T) {
	var collapsed []memmap.FileRange
	defer SetCollapseHostForTest(func(_ uintptr, fr memmap.FileRange) error {
		collapsed = append(collapsed, fr)
		return nil
	})()
	mf := newHomeTestMemoryFile(t, 4)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	fr := slotRange(home, pagesPerHugePage(), pagesPerHugePage())
	if mf.SlotsFull(fr) {
		t.Fatal("empty slots are full")
	}
	fillSlotsInRandomOrder(t, mf, fr, 5)
	if !mf.SlotsFull(fr) {
		t.Fatal("filled slots are not full")
	}

	// Another holder keeps the range in base pages.
	shared := slotRange(fr, 7, 1)
	mf.IncRef(shared, 0)
	if collapsed, _ := mf.CollapseHuge(fr); collapsed {
		t.Error("collapsed a range of which another holds a page")
	}
	mf.DecRef(shared)

	if collapsed, _ := mf.CollapseHuge(fr); !collapsed {
		t.Fatal("CollapseHuge failed")
	}
	if len(collapsed) != 1 || collapsed[0] != fr {
		t.Errorf("the host collapsed %v, want %v", collapsed, fr)
	}
	if !mf.IsHugePages(fr) {
		t.Error("the collapsed range is not a 2 MiB page")
	}
	ids, versions := pageIdentities(t, mf, fr)
	for i := range ids {
		if ids[i].Sequence != ids[0].Sequence+uint64(i) || versions[i].Sequence != versions[0].Sequence+uint64(i) {
			t.Fatalf("base page %d is %v version %v, not consecutive from %v version %v", i, ids[i], versions[i], ids[0], versions[0])
		}
	}
	if ids[0].Sequence%pagesPerHugePage() != 0 || versions[0].Sequence%pagesPerHugePage() != 0 {
		t.Errorf("the 2 MiB page's identity %v and version %v are not aligned", ids[0], versions[0])
	}
	if !pagesHold(t, mf, fr, 5) {
		t.Error("the collapsed page lost its contents")
	}
	if collapsed, _ := mf.CollapseHuge(fr); collapsed {
		t.Error("collapsed a 2 MiB page")
	}

	other := slotRange(home, 3*pagesPerHugePage(), pagesPerHugePage())
	fillSlotsInRandomOrder(t, mf, other, 6)
	// A host without a free huge page may have one later.
	restore := SetCollapseHostForTest(func(uintptr, memmap.FileRange) error { return unix.ENOMEM })
	if collapsed, retry := mf.CollapseHuge(other); collapsed || !retry {
		t.Errorf("CollapseHuge without a free huge page = %t, retry %t; want false, retry", collapsed, retry)
	}
	restore()
	// A range the host refuses is not retried; others still may be collapsed.
	restore = SetCollapseHostForTest(func(uintptr, memmap.FileRange) error { return unix.EINVAL })
	if collapsed, retry := mf.CollapseHuge(other); collapsed || retry || !mf.MayCollapse() {
		t.Errorf("CollapseHuge of a range the host refuses = %t, retry %t, collapse on %t; want false, no retry, on", collapsed, retry, mf.MayCollapse())
	}
	restore()
	// A host that cannot collapse turns collapse off.
	restore = SetCollapseHostForTest(func(uintptr, memmap.FileRange) error { return errNoHostCollapse })
	if collapsed, retry := mf.CollapseHuge(other); collapsed || retry || mf.MayCollapse() {
		t.Error("collapse is on on a host that cannot collapse")
	}
	restore()
	if !mf.MayCollapse() {
		t.Error("collapse is off once the host may collapse again")
	}
	mf.DecRef(fr)
	mf.DecRef(other)
}

// pmdMappedKB returns how many kB of this process's mapping at addr the host
// maps with huge pages.
func pmdMappedKB(t *testing.T, addr uintptr) uint64 {
	t.Helper()
	smaps, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		t.Fatal(err)
	}
	in := false
	var kb uint64
	for _, line := range strings.Split(string(smaps), "\n") {
		var start, end uintptr
		if n, _ := fmt.Sscanf(line, "%x-%x", &start, &end); n == 2 {
			in = start <= addr && addr < end
			continue
		}
		var v uint64
		if n, _ := fmt.Sscanf(line, "ShmemPmdMapped: %d kB", &v); in && n == 1 {
			kb += v
		}
	}
	return kb
}

// The host backs a collapsed range of a shared memory file with a huge page,
// with its base pages' contents, which a mapping at an aligned address maps
// as one: whatever address the host would pick for a mapping of the file, and
// though the host maps the huge page in the mapping it collapses through only
// if that has a page table (Linux 6.1). It collapses one 2 MiB range through a
// huge view, and the other through a mapping of its own.
func TestCollapseFileRange(t *testing.T) {
	fd, err := unix.MemfdCreate("ttc-collapse", 0)
	if err != nil {
		t.Skipf("memfd_create: %v", err)
	}
	defer unix.Close(fd)
	const length = 2 * hostarch.HugePageSize
	if err := unix.Ftruncate(fd, length); err != nil {
		t.Fatal(err)
	}
	m, err := unix.Mmap(fd, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(m)
	for i := 0; i < length; i += hostarch.PageSize {
		m[i] = byte(i / hostarch.PageSize)
	}
	view, err := mapHugeView(uintptr(fd), 0, length, false)
	if err != nil {
		t.Fatalf("mapHugeView: %v", err)
	}
	defer unix.Syscall(unix.SYS_MUNMAP, view, length, 0)
	viewed := memmap.FileRange{Start: 0, End: hostarch.HugePageSize}
	alone := memmap.FileRange{Start: hostarch.HugePageSize, End: length}
	for _, c := range []struct {
		fr   memmap.FileRange
		view uintptr
	}{{viewed, view}, {alone, 0}} {
		switch err := collapseFileRange(uintptr(fd), c.fr, c.view); {
		case errors.Is(err, errNoHostCollapse):
			t.Skip("the host does not collapse base pages into huge pages")
		case errors.Is(err, unix.ENOMEM), errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EBUSY):
			t.Skipf("the host has no huge page now: %v", err)
		case err != nil:
			t.Fatalf("collapseFileRange(%v, view %#x): %v", c.fr, c.view, err)
		}
	}
	mapped := mappingSlice(view, length)
	for i := 0; i < len(mapped); i += hostarch.PageSize {
		if want := byte(i / hostarch.PageSize); mapped[i] != want {
			t.Fatalf("byte %#x of the collapsed ranges is %d, want %d", i, mapped[i], want)
		}
	}
	if kb := pmdMappedKB(t, view); kb != length/1024 {
		t.Errorf("the collapsed ranges map in the view as %d kB of huge pages, want %d", kb, length/1024)
	}
}
