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

// A file that MemoryFileOpts.ReserveMappings asks for it maps its chunks in one
// reserved address range, at the range's start plus their offsets: the host
// holds a mapping of it per run of chunks with the same advice, however many
// times it grows, where a file without one holds a mapping per growth. Its
// pages read and write through them as through any, and the range goes with
// the file.
func TestReserveMappings(t *testing.T) {
	for _, reserve := range []bool{false, true} {
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
			ReserveMappings:         reserve,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Five growths: a small chunk, a huge one, and three homes, which are
		// small chunks. Small chunks are advised MADV_NOHUGEPAGE and huge ones
		// not, so that the reserved range holds three runs.
		small, err := mf.Allocate(hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
		if err != nil {
			t.Fatal(err)
		}
		huge, err := mf.Allocate(hostarch.HugePageSize, AllocOpts{Kind: usage.Anonymous, Huge: true})
		if err != nil {
			t.Fatal(err)
		}
		owner := NewHomeOwner()
		var slots []memmap.FileRange
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
		}
		want := 5
		if reserve {
			want = 3
		}
		if got := fileMappings(t, name); got != want {
			t.Errorf("reserve %t: %d mappings of the file after 5 growths, want %d", reserve, got, want)
		}
		for i, fr := range append([]memmap.FileRange{small, huge}, slots...) {
			fillPages(t, mf, fr, byte(i+1))
		}
		for i, fr := range append([]memmap.FileRange{small, huge}, slots...) {
			if !pagesHold(t, mf, fr, byte(i+1)) {
				t.Errorf("reserve %t: pages %v lost what was written to them", reserve, fr)
			}
			mf.DecRef(fr)
		}
		mf.Destroy()
		for deadline := time.Now().Add(10 * time.Second); fileMappings(t, name) != 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("reserve %t: the destroyed file is still mapped %d times", reserve, fileMappings(t, name))
			}
		}
	}
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
		ReserveMappings:         true,
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
		fr, err := mf.AllocateAt(slotRange(home, i*pagesPerHugePage+1, 1), AllocOpts{Kind: usage.Anonymous})
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
