// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package systrap

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// stubAppMappings returns the host mappings of sp's stub at application
// addresses: their ranges, in order.
func stubAppMappings(t *testing.T, sp *subprocess) []hostarch.AddrRange {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", sp.syscallThread.thread.tid))
	if err != nil {
		t.Fatal(err)
	}
	var ars []hostarch.AddrRange
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var start, end uint64
		if _, err := fmt.Sscanf(line, "%x-%x", &start, &end); err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		if end <= uint64(stubStart) || start >= uint64(stubEnd) && start < uint64(maximumUserAddress) {
			ars = append(ars, hostarch.AddrRange{hostarch.Addr(start), hostarch.Addr(end)})
		}
	}
	return ars
}

// newBudgetTest returns a platform and an address space whose stub keeps at
// most high host mappings, then low.
func newBudgetTest(t *testing.T, high, low int) (*Systrap, *subprocess) {
	t.Helper()
	s, err := New(platform.Options{DisableSyscallPatching: true, DisableFastPath: true})
	if err != nil {
		t.Fatalf("failed to create Systrap platform: %v", err)
	}
	oldHigh, oldLow := hostMappingHigh, hostMappingLow
	hostMappingHigh, hostMappingLow = high, low
	t.Cleanup(func() { hostMappingHigh, hostMappingLow = oldHigh, oldLow })
	as, err := s.NewAddressSpace()
	if err != nil {
		t.Fatalf("NewAddressSpace: %v", err)
	}
	t.Cleanup(as.Release)
	sp := as.(*subprocess)
	if n := len(stubAppMappings(t, sp)); n != 0 {
		t.Fatalf("a new stub has %d mappings of application addresses", n)
	}
	return s, sp
}

func allocate(t *testing.T, s *Systrap, pages uint64) memmap.FileRange {
	t.Helper()
	fr, err := s.memoryFile.Allocate(pages*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.System})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { s.memoryFile.DecRef(fr) })
	return fr
}

func countOf(sp *subprocess) int {
	sp.hostMaps.mu.Lock()
	defer sp.hostMaps.mu.Unlock()
	return sp.hostMaps.count
}

// The set merges and splits mappings as the host does, so that it counts
// what the stub has.
func TestHostMappingsCountAsHost(t *testing.T) {
	s, sp := newBudgetTest(t, 1<<30, 1<<29)
	fr := allocate(t, s, 16)
	base := hostarch.Addr(0x10000000)
	check := func(what string, want int) {
		t.Helper()
		host := len(stubAppMappings(t, sp))
		if got := countOf(sp); got != host || host != want {
			t.Errorf("%s: the set counts %d mappings, the host has %d, want %d", what, got, host, want)
		}
	}
	// Pages of contiguous offsets merge, in any order.
	for _, i := range []uint64{0, 2, 1, 5, 4, 3} {
		addr := base + hostarch.Addr(i*hostarch.PageSize)
		page := memmap.FileRange{fr.Start + i*hostarch.PageSize, fr.Start + (i+1)*hostarch.PageSize}
		if err := sp.MapFile(addr, s.memoryFile, page, hostarch.ReadWrite, false); err != nil {
			t.Fatalf("MapFile: %v", err)
		}
	}
	check("contiguous pages", 1)
	// Another protection does not merge.
	page := memmap.FileRange{fr.Start + 6*hostarch.PageSize, fr.Start + 7*hostarch.PageSize}
	if err := sp.MapFile(base+6*hostarch.PageSize, s.memoryFile, page, hostarch.Read, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	check("another protection", 2)
	// Nor does a page of another offset.
	if err := sp.MapFile(base+7*hostarch.PageSize, s.memoryFile, page, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	check("another offset", 3)
	// Unmapping a page in the middle splits a mapping.
	sp.Unmap(base+2*hostarch.PageSize, hostarch.PageSize)
	check("a hole", 4)
	// Mapping it again merges it back.
	page = memmap.FileRange{fr.Start + 2*hostarch.PageSize, fr.Start + 3*hostarch.PageSize}
	if err := sp.MapFile(base+2*hostarch.PageSize, s.memoryFile, page, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	check("the hole filled", 3)
	// Replacing a mapping's middle with one of another offset splits it in
	// three.
	page = memmap.FileRange{fr.Start + 9*hostarch.PageSize, fr.Start + 10*hostarch.PageSize}
	if err := sp.MapFile(base+3*hostarch.PageSize, s.memoryFile, page, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	check("a middle replaced", 5)
	sp.Unmap(base, 16*hostarch.PageSize)
	check("all unmapped", 0)
}

// A stub that maps more than its budget of mappings that do not merge keeps
// within it, unmapping those that it mapped longest ago.
func TestHostMappingsStayWithinBudget(t *testing.T) {
	const high, low, mappings = 1000, 800, 3000
	s, sp := newBudgetTest(t, high, low)
	fr := allocate(t, s, 1)
	base := hostarch.Addr(0x10000000)
	addr := func(i int) hostarch.Addr {
		// Every other page, so that no two adjoin.
		return base + hostarch.Addr(2*i)*hostarch.PageSize
	}
	evictions := hostMappingEvictions.Load()
	most := 0
	for i := 0; i < mappings; i++ {
		if err := sp.MapFile(addr(i), s.memoryFile, fr, hostarch.Read, false); err != nil {
			t.Fatalf("MapFile %d: %v", i, err)
		}
		if i%97 == 0 {
			most = max(most, len(stubAppMappings(t, sp)))
		}
	}
	host := stubAppMappings(t, sp)
	if got := countOf(sp); got != len(host) {
		t.Errorf("the set counts %d mappings, the host has %d", got, len(host))
	}
	if most > high+1 || len(host) > high+1 {
		t.Errorf("the stub had up to %d mappings, and %d at the end, with a budget of %d", most, len(host), high)
	}
	if evicted := hostMappingEvictions.Load() - evictions; evicted < mappings-high {
		t.Errorf("unmapped %d mappings, want at least %d", evicted, mappings-high)
	}
	// The most recent mappings remain; the first went first.
	mapped := make(map[hostarch.Addr]bool)
	for _, ar := range host {
		mapped[ar.Start] = true
	}
	for i := mappings - low/2; i < mappings; i++ {
		if !mapped[addr(i)] {
			t.Fatalf("mapping %d of %d, among the most recent, is gone", i, mappings)
		}
	}
	if mapped[addr(0)] {
		t.Errorf("the first mapping remains")
	}
}

// Large mappings stay mapped longer than small ones made at the same time.
func TestHostMappingsEvictSmallFirst(t *testing.T) {
	const high, low = 100, 80
	s, sp := newBudgetTest(t, high, low)
	small := allocate(t, s, 1)
	large := allocate(t, s, 1024) // 4 MiB
	base := hostarch.Addr(0x10000000)
	largeAt := base + 0x10000000
	if err := sp.MapFile(largeAt, s.memoryFile, large, hostarch.Read, false); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	// The last of these finds the stub at its budget, and unmaps the oldest
	// fifth of them, small mappings made after the large one, which is 4
	// MiB: it would take a second fault to map again.
	for i := 0; i < high; i++ {
		if err := sp.MapFile(base+hostarch.Addr(2*i)*hostarch.PageSize, s.memoryFile, small, hostarch.Read, false); err != nil {
			t.Fatalf("MapFile %d: %v", i, err)
		}
	}
	found := false
	for _, ar := range stubAppMappings(t, sp) {
		if ar.Start == largeAt {
			found = true
		}
	}
	if !found {
		t.Errorf("the large mapping, made first, was unmapped before small ones made after it")
	}
}

// BenchmarkHostMappingsMapped measures what recording a mapping costs: pages
// that merge with what they adjoin, as a fault of private memory in homes
// maps, and pages that do not, as copies after a live fork map.
func BenchmarkHostMappingsMapped(b *testing.B) {
	for _, merge := range []bool{true, false} {
		b.Run(map[bool]string{true: "merging", false: "apart"}[merge], func(b *testing.B) {
			var h hostMappings
			h.mu.Lock()
			defer h.mu.Unlock()
			const pages = 1 << 18
			for i := 0; i < b.N; i++ {
				page := uint64(i % pages)
				if page == 0 {
					h.resetLocked()
				}
				addr := hostarch.Addr(0x10000000) + hostarch.Addr(2*page)*hostarch.PageSize
				off := uint64(0)
				if merge {
					addr = hostarch.Addr(0x10000000) + hostarch.Addr(page)*hostarch.PageSize
					off = page * hostarch.PageSize
				}
				h.mappedLocked(hostarch.AddrRange{addr, addr + hostarch.PageSize}, 3, off, hostarch.ReadWrite)
			}
		})
	}
}
