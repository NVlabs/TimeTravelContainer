// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// fileMappings returns how many mappings of the deleted file name the process
// holds.
func fileMappings(t *testing.T, name string) int {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(maps), " "+name+" (deleted)\n")
}

// A file maps each growth's chunks together, where the host places them, and
// a home chunk is a growth of its own: the host holds a mapping of the file
// per growth, and one more for a home's huge view once it holds a 2 MiB page.
// Pages read and write through them as through any, and the mappings go with
// the file.
func TestChunkMappings(t *testing.T) {
	file, err := os.CreateTemp("", "ttc-mappings-")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	mf, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
		AdviseNoHugepage:        true,
		MaxHomes:                4,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Five growths: a small chunk, a huge one, and three homes, which are
	// small chunks.
	small, err := mf.Allocate(hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	huge, err := mf.Allocate(hostarch.HugePageSize, AllocOpts{Kind: usage.Anonymous, Huge: true})
	if err != nil {
		t.Fatal(err)
	}
	owner := NewHomeOwner()
	var slots, homes []memmap.FileRange
	for i := 0; i < 3; i++ {
		home, err := mf.ReserveHome(owner)
		if err != nil {
			t.Fatal(err)
		}
		fr, err := mf.AllocateAt(memmap.FileRange{Start: home.Start, End: home.Start + hostarch.PageSize}, AllocOpts{Kind: usage.Anonymous})
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, fr)
		homes = append(homes, home)
	}
	if got := fileMappings(t, name); got != 5 {
		t.Errorf("%d mappings of the file after 5 growths, want 5", got)
	}
	// A 2 MiB page in the last home maps its huge view.
	page, err := mf.AllocateAt(slotRange(homes[2], pagesPerHugePage(), pagesPerHugePage()), AllocOpts{Kind: usage.Anonymous, Huge: true})
	if err != nil {
		t.Fatal(err)
	}
	slots = append(slots, page)
	if got := fileMappings(t, name); got != 6 {
		t.Errorf("%d mappings of the file once a home holds a 2 MiB page, want 6", got)
	}
	for i, fr := range append([]memmap.FileRange{small, huge}, slots...) {
		fillPages(t, mf, fr, byte(i+1))
	}
	for i, fr := range append([]memmap.FileRange{small, huge}, slots...) {
		if !pagesHold(t, mf, fr, byte(i+1)) {
			t.Errorf("pages %v lost what was written to them", fr)
		}
		mf.DecRef(fr)
	}
	mf.Destroy()
	for deadline := time.Now().Add(10 * time.Second); fileMappings(t, name) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the destroyed file is still mapped %d times", fileMappings(t, name))
		}
	}
}

// fillReader returns a ReaderFunc that reads bytes b.
func fillReader(b byte) safemem.ReaderFunc {
	return func(dsts safemem.BlockSeq) (uint64, error) {
		var n uint64
		for rem := dsts; !rem.IsEmpty(); rem = rem.Tail() {
			bs := rem.Head().ToSlice()
			for i := range bs {
				bs[i] = b
			}
			n += uint64(len(bs))
		}
		return n, nil
	}
}

// blocksHold returns true if every byte of bs is b.
func blocksHold(bs safemem.BlockSeq, b byte) bool {
	for rem := bs; !rem.IsEmpty(); rem = rem.Tail() {
		for _, got := range rem.Head().ToSlice() {
			if got != b {
				return false
			}
		}
	}
	return true
}

// A home's 2 MiB pages are created, filled and read through its huge view,
// mapped at an address aligned to huge pages once the home holds one; its base
// pages never are, and a home with none has no view. The view serves the same
// pages as the file's own mapping, and stays where it is once the home is
// spared and taken again.
func TestHugeView(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	owner := NewHomeOwner()
	home, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	i := home.Start / chunkSize
	view := func() uintptr {
		mf.mu.Lock()
		defer mf.mu.Unlock()
		return mf.homes[i].hugeView
	}
	base, err := mf.AllocateAt(slotRange(home, 3, 1), AllocOpts{Kind: usage.Anonymous, ReaderFunc: fillReader(1)})
	if err != nil {
		t.Fatal(err)
	}
	if view() != 0 {
		t.Fatal("a home that holds only a base page has a huge view")
	}
	if _, ok := mf.HugePageMapping(base); ok {
		t.Error("a base page has a huge mapping")
	}
	page, err := mf.AllocateAt(slotRange(home, 2*pagesPerHugePage(), pagesPerHugePage()), AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true, ReaderFunc: fillReader(2)})
	if err != nil {
		t.Fatal(err)
	}
	addr := view()
	if addr == 0 || addr%hostarch.HugePageSize != 0 {
		t.Fatalf("a home that holds a 2 MiB page has huge view %#x, want one aligned to huge pages", addr)
	}
	if !pagesHold(t, mf, page, 2) || !pagesHold(t, mf, base, 1) {
		t.Fatal("the pages filled through the huge view and the file's mapping do not hold what was written")
	}
	viewed, ok := mf.HugePageMapping(page)
	if !ok || !blocksHold(viewed, 2) || viewed.NumBytes() != hostarch.HugePageSize {
		t.Fatalf("the 2 MiB page's huge mapping: %v, %t", viewed, ok)
	}
	if got, want := viewed.Head().Addr(), addr+uintptr(2*hostarch.HugePageSize); got != want {
		t.Errorf("the 2 MiB page's huge mapping is at %#x, want %#x in the view", got, want)
	}
	fillPages(t, mf, page, 3)
	if !blocksHold(viewed, 3) {
		t.Error("the huge view does not see what the file's mapping wrote")
	}
	mf.DecRef(base)
	mf.DecRef(page)
	mf.DisownHome(owner, home)
	waitForRelease(t, mf)
	if view() != addr {
		t.Fatalf("the spared home's huge view moved from %#x to %#x", addr, view())
	}
	again, err := mf.ReserveHome(NewHomeOwner())
	if err != nil || again != home {
		t.Fatalf("ReserveHome: %v, %v, want the spared home %v", again, err, home)
	}
	page, err = mf.AllocateAt(slotRange(home, 2*pagesPerHugePage(), pagesPerHugePage()), AllocOpts{Kind: usage.Anonymous, Huge: true, LogicalHugePages: true, ReaderFunc: fillReader(4)})
	if err != nil {
		t.Fatal(err)
	}
	if !pagesHold(t, mf, page, 4) {
		t.Error("a 2 MiB page filled through the remapped huge view does not hold what was written")
	}
	mf.DecRef(page)
}

// vmPTE returns how many KiB of page tables this process has.
func vmPTE(t *testing.T) uint64 {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		var kib uint64
		if n, _ := fmt.Sscanf(line, "VmPTE: %d kB", &kib); n == 1 {
			return kib
		}
	}
	t.Fatal("no VmPTE in /proc/self/status")
	return 0
}

// The host keeps the page tables of a mapping after its pages are gone. A
// home that is spared, having lost its owner and its pages, is mapped afresh,
// which drops the page tables of the file's mapping of it; its slots read and
// write as before once taken again.
func TestSparedHomeDropsPageTables(t *testing.T) {
	file, err := os.CreateTemp("", "ttc-spared-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	mf, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
		AdviseNoHugepage:        true,
		MaxHomes:                4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	owner := NewHomeOwner()
	home, err := mf.ReserveHome(owner)
	if err != nil {
		t.Fatal(err)
	}
	// A page in each of 256 2 MiB of the home, written through the file's
	// mapping, which takes a page table for each.
	const regions = 256
	var pages []memmap.FileRange
	for i := uint64(0); i < regions; i++ {
		fr, err := mf.AllocateAt(slotRange(home, i*pagesPerHugePage()+1, 1), AllocOpts{Kind: usage.Anonymous})
		if err != nil {
			t.Fatal(err)
		}
		fillPages(t, mf, fr, 1)
		pages = append(pages, fr)
	}
	before := vmPTE(t)
	for _, fr := range pages {
		mf.DecRef(fr)
	}
	mf.DisownHome(owner, home)
	waitForRelease(t, mf)
	if after := vmPTE(t); after+regions*hostarch.PageSize/1024*3/4 > before {
		t.Errorf("page tables went from %d KiB to %d KiB once the home was spared, want about %d KiB fewer", before, after, regions*hostarch.PageSize/1024)
	}

	again, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	fr, err := mf.AllocateAt(slotRange(again, 1, 1), AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	fillPages(t, mf, fr, 7)
	if !pagesHold(t, mf, fr, 7) {
		t.Error("a page of a home mapped afresh lost what was written to it")
	}
	mf.DecRef(fr)
}
