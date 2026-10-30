// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package systrap

import (
	"math/bits"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sync"
)

// The host limits how many mappings a process may have (vm.max_map_count),
// and so how many of an address space's mappings its stub may map. Guests
// never see that limit: address spaces have as many mappings as they make,
// and their stubs map them only as faults need them, a cache that the stub
// unmaps parts of before it reaches the limit. It unmaps the mappings that
// faults made least recently, small ones first, which cost the least to map
// again: a fault maps them again, as it does what it never mapped.
//
// To know how many mappings a stub has, which the host does not tell, each
// stub keeps a set of its mappings (hostMappings), merged as the host merges
// them: the host merges a mapping with an adjacent one of the same open file,
// at the adjacent offset, with the same protection. The set merges no less
// than the host, so that each of its segments is one or more whole host
// mappings, and unmapping it can never split one, which the host would
// refuse a process at its limit. Where the host merges less, the stub has
// more mappings than the set counts, and if it reaches the limit, mm unmaps
// all of the address space (see mm.MemoryManager.mapFileLocked).
//
// Every fault of every stub records what it maps, so the common cases cost one
// lookup of the set: a mapping of addresses that the stub has not mapped, as
// a fault makes, and an unmapping of addresses that it has not.

const (
	// maxHostMappingReserve is the most mappings that the budget leaves to
	// those of the stub's own, which it does not count.
	maxHostMappingReserve = 4096

	// smallHostMapping is the largest mapping that eviction prefers: a fault
	// maps at most 2 MiB of an address space with homes, so mapping a small
	// one again costs a fault at most.
	smallHostMapping = hostarch.HugePageSize
)

var (
	// hostMappingHigh is how many mappings of its application addresses a
	// stub may have before mapping more unmaps some, down to
	// hostMappingLow. Both are set by initHostMappingBudget.
	hostMappingHigh int
	hostMappingLow  int

	// hostMappingEvictions counts the mappings that stubs unmapped to stay
	// within the host's limit.
	hostMappingEvictions atomicbitops.Uint64
)

// initHostMappingBudget sets the stubs' budget of host mappings from the
// host's limit (see hostmm.MaxMapCount).
func initHostMappingBudget() {
	limit := hostmm.MaxMapCount()
	reserve := min(maxHostMappingReserve, limit/4)
	budget := limit - reserve
	hostMappingHigh = budget - budget/10
	hostMappingLow = budget - budget/4
	log.Infof("Stubs keep at most %d host mappings of application memory (host limit %d)", hostMappingHigh, limit)
}

// hostMapping is a host mapping of a stub: the value of a hostMappingSet
// segment.
type hostMapping struct {
	// fd is the descriptor of the file that it maps, which identifies the
	// open file while the mapping is made (see hostMappingSetFunctions.Merge).
	fd int32

	// at is its protection.
	at hostarch.AccessType

	// off is the file offset that its start maps.
	off uint64

	// mapped is when it was last made, in counts of the stub's mappings
	// (hostMappings.clock): when a fault last needed it.
	mapped uint64
}

type hostMappingSetFunctions struct{}

func (hostMappingSetFunctions) MinKey() hostarch.Addr {
	return 0
}

func (hostMappingSetFunctions) MaxKey() hostarch.Addr {
	return ^hostarch.Addr(0)
}

func (hostMappingSetFunctions) ClearValue(*hostMapping) {
}

// Merge implements segment.Functions.Merge, as the host merges mappings.
//
// Two descriptors of one open file (dup) would merge in the host and not in
// the set, but the Sentry maps each file through one descriptor; and a
// descriptor closed and reused for another file while mappings of the first
// remain would merge in the set and not in the host, which is the safe way
// to be wrong.
func (hostMappingSetFunctions) Merge(ar1 hostarch.AddrRange, m1 hostMapping, ar2 hostarch.AddrRange, m2 hostMapping) (hostMapping, bool) {
	if !merges(ar1, &m1, &m2) {
		return hostMapping{}, false
	}
	m1.mapped = max(m1.mapped, m2.mapped)
	return m1, true
}

// merges returns whether the host merges m1, which maps ar1, with m2, which
// maps the addresses that follow ar1.
func merges(ar1 hostarch.AddrRange, m1, m2 *hostMapping) bool {
	return m1.fd == m2.fd && m1.at == m2.at && m1.off+uint64(ar1.Length()) == m2.off
}

func (hostMappingSetFunctions) Split(ar hostarch.AddrRange, m hostMapping, split hostarch.Addr) (hostMapping, hostMapping) {
	m2 := m
	m2.off += uint64(split - ar.Start)
	return m, m2
}

// hostMappings tracks the host mappings of a stub's application addresses.
type hostMappings struct {
	// mu protects the fields below, and orders the stub's mmaps and munmaps
	// with them. It precedes subprocess.syscallThreadMu.
	mu sync.Mutex

	// set holds the stub's mappings, and count how many segments it has.
	set   hostMappingSet
	count int

	// pressure is set while count is at least half of hostMappingHigh (see
	// MappingPressure).
	pressure atomicbitops.Bool

	// clock counts the stub's mappings (see hostMapping.mapped).
	clock uint64

	// hand is where eviction looks for mappings to unmap first, after
	// where the last one stopped, so that it unmaps evenly.
	hand hostarch.Addr
}

// aroundLocked returns how many segments intersect or adjoin ar.
//
// +checklocks:h.mu
func (h *hostMappings) aroundLocked(ar hostarch.AddrRange) int {
	start, end := ar.Start, ar.End
	if start != 0 {
		start--
	}
	if end != ^hostarch.Addr(0) {
		end++
	}
	n := 0
	for seg := h.set.LowerBoundSegment(start); seg.Ok() && seg.Start() < end; seg = seg.NextSegment() {
		n++
	}
	return n
}

// resetLocked records that the stub has no mappings of application
// addresses.
//
// +checklocks:h.mu
func (h *hostMappings) resetLocked() {
	h.set.RemoveAll()
	h.addLocked(-h.count)
}

// mappedLocked records that the stub has mapped ar to fd at offset off with
// access at.
//
// +checklocks:h.mu
func (h *hostMappings) mappedLocked(ar hostarch.AddrRange, fd int, off uint64, at hostarch.AccessType) {
	h.clock++
	m := hostMapping{fd: int32(fd), at: at, off: off, mapped: h.clock}
	if seg, gap := h.set.Find(ar.Start); !seg.Ok() && gap.End() >= ar.End {
		// Addresses that the stub had not mapped: one mapping more, less
		// each neighbor that it merges with.
		n := 1
		if prev := gap.PrevSegment(); prev.Ok() && prev.End() == ar.Start && merges(prev.Range(), prev.ValuePtr(), &m) {
			n--
		}
		if next := gap.NextSegment(); next.Ok() && next.Start() == ar.End && merges(ar, &m, next.ValuePtr()) {
			n--
		}
		h.set.Insert(gap, ar, m)
		h.addLocked(n)
		return
	}
	before := h.aroundLocked(ar)
	gap := h.set.RemoveRange(ar)
	h.set.Insert(gap, ar, m)
	h.addLocked(h.aroundLocked(ar) - before)
}

// addLocked adds n to the count of the stub's mappings.
//
// +checklocks:h.mu
func (h *hostMappings) addLocked(n int) {
	h.count += n
	if pressure := hostMappingHigh != 0 && h.count >= hostMappingHigh/2; pressure != h.pressure.RacyLoad() {
		h.pressure.Store(pressure)
	}
}

// unmappedLocked records that the stub has unmapped ar.
//
// +checklocks:h.mu
func (h *hostMappings) unmappedLocked(ar hostarch.AddrRange) {
	seg, gap := h.set.Find(ar.Start)
	if !seg.Ok() && gap.End() >= ar.End {
		return // The stub had not mapped them.
	}
	if h.set.FirstSegment().Start() >= ar.Start && h.set.LastSegment().End() <= ar.End {
		// All of them, as when an address space goes.
		h.resetLocked()
		return
	}
	// One mapping less for each that it removes, but one more for each that
	// it cuts at an end, which keeps its part outside ar: a mapping cut at
	// both ends becomes two.
	n := 0
	if seg.Ok() && seg.Start() < ar.Start {
		n++
	}
	if last := h.set.FindSegment(ar.End - 1); last.Ok() && last.End() > ar.End {
		n++
	}
	h.set.RemoveRangeWith(ar, func(hostMappingIterator) { n-- })
	h.addLocked(n)
}

// ageClasses is the number of classes of mappings, in the order that eviction
// unmaps them: by how long ago they were made, in powers of two of the clock,
// oldest first, and small ones before large ones made as long ago.
const ageClasses = 64

// classOf returns the class of a mapping of ar made at mapped, by clock now:
// the floor of log2 of its age, counted down from the oldest, and counted
// down less for a mapping larger than smallHostMapping, by log2 of how many
// faults mapping it again whole would take.
func classOf(ar hostarch.AddrRange, mapped, now uint64) int {
	class := ageClasses - bits.Len64(now-mapped)
	if ar.Length() > smallHostMapping {
		class += bits.Len64((uint64(ar.Length()) - 1) / smallHostMapping)
	}
	return min(class, ageClasses-1)
}

// evictLocked unmaps n or more of the stub's mappings, other than those in
// keep (which the caller is about to map or unmap), and returns how many it
// unmapped. It unmaps runs of adjacent victims at once.
//
// +checklocks:h.mu
func (h *hostMappings) evictLocked(s *subprocess, n int, keep hostarch.AddrRange) int {
	// Count the mappings of each class, and find the classes to unmap: all
	// of those below last, and quota of last.
	var classes [ageClasses]int
	for seg := h.set.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if !seg.Range().Overlaps(keep) {
			classes[classOf(seg.Range(), seg.ValuePtr().mapped, h.clock)]++
		}
	}
	last, below := 0, 0
	for ; last < ageClasses-1 && below+classes[last] < n; last++ {
		below += classes[last]
	}
	quota := n - below
	// Find runs of victims, from the hand on, then from the start; a run
	// ends at a mapping that is not a victim, and at the stub's own
	// addresses.
	var (
		runs    []hostarch.AddrRange
		run     hostarch.AddrRange
		evicted int
	)
	endRun := func() {
		if run.Length() != 0 {
			runs = append(runs, run)
			run = hostarch.AddrRange{}
		}
	}
	hand := h.hand
	for pass := 0; pass < 2 && evicted < n; pass++ {
		seg, end := h.set.LowerBoundSegment(hand), ^hostarch.Addr(0)
		if pass == 1 {
			seg, end = h.set.FirstSegment(), hand
		}
		for ; seg.Ok() && seg.Start() < end && evicted < n; seg = seg.NextSegment() {
			ar := seg.Range()
			victim := false
			if !ar.Overlaps(keep) {
				switch c := classOf(ar, seg.ValuePtr().mapped, h.clock); {
				case c < last:
					victim = true
				case c == last && quota > 0:
					victim = true
					quota--
				}
			}
			if !victim || run.End <= hostarch.Addr(stubStart) && ar.Start >= hostarch.Addr(stubEnd) {
				endRun()
			}
			if !victim {
				continue
			}
			if run.Length() == 0 {
				run = ar
			} else {
				run.End = ar.End
			}
			evicted++
			h.hand = ar.End
		}
		endRun()
	}
	for _, run := range runs {
		if err := s.munmap(run.Start, uint64(run.Length())); err != nil && err != errDeadSubprocess {
			log.Warningf("Unmapping %v to stay within the host's limit on mappings: %v", run, err)
		}
		h.unmappedLocked(run)
	}
	hostMappingEvictions.Add(uint64(evicted))
	return evicted
}

// MappingPressure implements platform.MappingPressure.MappingPressure.
func (s *subprocess) MappingPressure() bool {
	return s.hostMaps.pressure.Load()
}

// makeRoomLocked unmaps stub mappings that faults made least recently if the
// stub has as many as its budget allows, before it maps or unmaps ar.
//
// +checklocks:h.mu
func (h *hostMappings) makeRoomLocked(s *subprocess, ar hostarch.AddrRange) {
	if h.count < hostMappingHigh || hostMappingHigh == 0 {
		return
	}
	n := h.count - hostMappingLow
	before := h.count
	evicted := h.evictLocked(s, n, ar)
	if total := hostMappingEvictions.Load(); total == uint64(evicted) {
		log.Infof("A stub reached %d of the host's mappings; unmapped %d that faults made least recently, leaving %d", before, evicted, h.count)
	} else {
		log.Debugf("Stub at %d host mappings unmapped %d, leaving %d", before, evicted, h.count)
	}
}
