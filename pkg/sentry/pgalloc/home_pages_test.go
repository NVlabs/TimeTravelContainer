// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// modelPage is what a test expects a MemoryFile to know of a page of a home.
type modelPage struct {
	refs  uint64
	waste bool
	id    LogicalPageID
	ver   PageVersionID
}

// homeModel is a test's model of the pages of a home: the first pages of it.
type homeModel struct {
	t     testing.TB
	mf    *MemoryFile
	home  memmap.FileRange
	pages []modelPage
	// versions holds every version that the file gave a page, which a new
	// version must not repeat.
	versions map[PageVersionID]bool
}

func (m *homeModel) slots(page, pages int) memmap.FileRange {
	return slotRange(m.home, uint64(page), uint64(pages))
}

// learn records the identities and versions of the pages of fr, which the
// file has just given them: each version must be new.
func (m *homeModel) learn(fr memmap.FileRange) {
	m.t.Helper()
	for _, run := range m.mf.LogicalPageRanges(fr) {
		for off := run.Range.Start; off < run.Range.End; off += hostarch.PageSize {
			delta := (off - run.Range.Start) / hostarch.PageSize
			p := &m.pages[(off-m.home.Start)/hostarch.PageSize]
			p.id = LogicalPageID{Allocator: run.First.Allocator, Sequence: run.First.Sequence + delta}
			p.ver = PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + delta}
			if m.versions[p.ver] {
				m.t.Fatalf("page %#x took version %+v again", off, p.ver)
			}
			m.versions[p.ver] = true
		}
	}
}

// all returns true if fn holds for each page of [page, page+pages).
func (m *homeModel) all(page, pages int, fn func(p *modelPage) bool) bool {
	for k := page; k < page+pages; k++ {
		if !fn(&m.pages[k]) {
			return false
		}
	}
	return true
}

// check compares what the file knows of the model's pages with the model.
func (m *homeModel) check(step string) {
	m.t.Helper()
	fr := m.slots(0, len(m.pages))
	// Identities and versions, of the pages in use.
	byPage := make(map[uint64]LogicalPageRange)
	for _, run := range m.mf.LogicalPageRanges(fr) {
		for off := run.Range.Start; off < run.Range.End; off += hostarch.PageSize {
			byPage[off] = run
		}
	}
	m.mf.mu.Lock()
	defer m.mf.mu.Unlock()
	refs := make([]uint64, len(m.pages))
	m.mf.visitRefsLocked(fr, func(run memmap.FileRange, n uint64) bool {
		for off := run.Start; off < run.End; off += hostarch.PageSize {
			refs[(off-m.home.Start)/hostarch.PageSize] = n
		}
		return true
	})
	waste := make(map[uint64]bool)
	for _, run := range m.mf.homeWasteLocked(fr) {
		for off := run.Start; off < run.End; off += hostarch.PageSize {
			waste[off] = true
		}
	}
	for k := range m.pages {
		p := &m.pages[k]
		off := m.home.Start + uint64(k)*hostarch.PageSize
		if refs[k] != p.refs || waste[off] != p.waste {
			m.t.Fatalf("%s: page %d has %d references and waste %t, want %d and %t", step, k, refs[k], waste[off], p.refs, p.waste)
		}
		run, ok := byPage[off]
		if ok != (p.refs != 0) {
			m.t.Fatalf("%s: page %d has an identity: %t, want %t", step, k, ok, p.refs != 0)
		}
		if !ok {
			continue
		}
		delta := (off - run.Range.Start) / hostarch.PageSize
		if id, ver := (LogicalPageID{run.First.Allocator, run.First.Sequence + delta}), (PageVersionID{run.FirstVersion.Allocator, run.FirstVersion.Sequence + delta}); id != p.id || ver != p.ver {
			m.t.Fatalf("%s: page %d has identity %+v and version %+v, want %+v and %+v", step, k, id, ver, p.id, p.ver)
		}
	}
	m.checkTableLocked(step)
}

// checkTableLocked checks the invariants of the home's table, and that the
// file accounts its pages.
func (m *homeModel) checkTableLocked(step string) {
	m.t.Helper()
	hc := m.mf.homes[m.home.Start/chunkSize]
	nonFree := uint64(0)
	for b := uint64(0); b < blocksPerHome; b++ {
		blk := hc.block(b)
		if blk == nil {
			continue
		}
		if len(blk.runs) == 0 {
			m.t.Fatalf("%s: block %d has no runs", step, b)
		}
		for i := range blk.runs {
			r := &blk.runs[i]
			if r.start >= r.end || uint64(r.end) > pagesPerHugePage() || r.ps.free() || (i > 0 && blk.runs[i-1].end > r.start) {
				m.t.Fatalf("%s: block %d has run %d %+v after %+v", step, b, i, *r, blk.runs[max(i-1, 0)])
			}
			if i > 0 && blk.runs[i-1].continuedBy(r) {
				m.t.Fatalf("%s: block %d's run %d %+v continues the one before it, %+v", step, b, i, *r, blk.runs[i-1])
			}
		}
		if blk.huge {
			first := blk.page(0)
			for k := uint64(0); k < pagesPerHugePage(); k++ {
				if ps := blk.page(k); !ps.inUse() || ps.context() != first.context() || ps.id != first.id+k || ps.ver != first.ver+k {
					m.t.Fatalf("%s: block %d is a 2 MiB page, but page %d is %+v after %+v", step, b, k, ps, first)
				}
			}
			if first.id%pagesPerHugePage() != 0 || first.ver%pagesPerHugePage() != 0 {
				m.t.Fatalf("%s: block %d is a 2 MiB page whose sequences %d and %d are not aligned", step, b, first.id, first.ver)
			}
		}
		nonFree += blk.pages()
	}
	if nonFree != hc.pages {
		m.t.Fatalf("%s: the home counts %d pages, its table %d", step, hc.pages, nonFree)
	}
	if m.mf.accountedBytes != nonFree*hostarch.PageSize {
		m.t.Fatalf("%s: the file accounts %d bytes, its home holds %d pages", step, m.mf.accountedBytes, nonFree)
	}
}

// roundTrip moves the home's table into the sets and back, as a save does,
// and checks that the sets held its pages, and that the table is as it was.
func (m *homeModel) roundTrip(step string) {
	m.t.Helper()
	m.mf.mu.Lock()
	hc := m.mf.homes[m.home.Start/chunkSize]
	before := make([]pageState, blocksPerHome*pagesPerHugePage())
	huge := make([]bool, blocksPerHome)
	for b := uint64(0); b < blocksPerHome; b++ {
		if blk := hc.block(b); blk != nil {
			huge[b] = blk.huge
			for k := uint64(0); k < pagesPerHugePage(); k++ {
				before[b*pagesPerHugePage()+k] = blk.page(k)
			}
		}
	}
	undo := m.mf.exportHomePagesLocked()
	for k, p := range m.pages {
		off := m.home.Start + uint64(k)*hostarch.PageSize
		uf := m.mf.unfreeSmall.FindSegment(off)
		if !uf.Ok() || uf.ValuePtr().refs != p.refs {
			m.mf.mu.Unlock()
			m.t.Fatalf("%s: exported page %d has %+v in unfreeSmall, want %d references", step, k, uf.ValuePtr(), p.refs)
		}
		if ma := m.mf.memAcct.FindSegment(off); ma.Ok() != (p.refs != 0 || p.waste) || (ma.Ok() && ma.ValuePtr().wasteOrReleasing != p.waste) {
			m.mf.mu.Unlock()
			m.t.Fatalf("%s: exported page %d is accounted: %t, want %t (waste %t)", step, k, ma.Ok(), p.refs != 0 || p.waste, p.waste)
		}
	}
	undo()
	for b := uint64(0); b < blocksPerHome; b++ {
		blk := hc.block(b)
		if (blk != nil && blk.huge) != huge[b] {
			m.mf.mu.Unlock()
			m.t.Fatalf("%s: block %d's 2 MiB page did not survive the sets", step, b)
		}
		for k := uint64(0); k < pagesPerHugePage(); k++ {
			var ps pageState
			if blk != nil {
				ps = blk.page(k)
			}
			// commitSeq is per block, and may go back to 0 in the sets only for
			// pages known to be committed; nothing else may change.
			if ps != before[b*pagesPerHugePage()+k] {
				m.mf.mu.Unlock()
				m.t.Fatalf("%s: page %d of block %d is %+v after the sets, %+v before", step, k, b, ps, before[b*pagesPerHugePage()+k])
			}
		}
	}
	if seg := m.mf.unfreeSmall.FindSegment(m.home.Start); !seg.Ok() || seg.Range().Intersect(m.home) != m.home || seg.ValuePtr().refs != 0 {
		m.mf.mu.Unlock()
		m.t.Fatalf("%s: unfreeSmall holds %v in the home after the sets", step, seg.Range())
	}
	if !m.mf.logicalPages.IsEmptyRange(m.home) || !m.mf.memAcct.IsEmptyRange(m.home) {
		m.mf.mu.Unlock()
		m.t.Fatalf("%s: the sets keep pages of the home", step)
	}
	m.mf.mu.Unlock()
}

// A home's table agrees with a model of its pages through allocations,
// references, copies, moves, frees, decommits, collapses and releases in
// random order, and survives being moved into the sets and back.
func TestHomePagesModel(t *testing.T) {
	testHomePagesModel(t, 1)
}

// testHomePagesModel is TestHomePagesModel with the random seed given.
func testHomePagesModel(t *testing.T, seed int64) {
	mf := newHomeTestMemoryFile(t, 4)
	defer SetCollapseHostForTest(func(uintptr, memmap.FileRange) error { return nil })()
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	hugePages := int(pagesPerHugePage())
	pages := 8 * hugePages
	m := &homeModel{t: t, mf: mf, home: home, pages: make([]modelPage, pages), versions: make(map[PageVersionID]bool)}
	// Waste stays waste until the model releases it.
	mf.mu.Lock()
	mf.holdReleaserLocked()
	mf.mu.Unlock()
	rng := rand.New(rand.NewSource(seed))
	used := func(p *modelPage) bool { return p.refs != 0 }
	unused := func(p *modelPage) bool { return p.refs == 0 }
	held := func(p *modelPage) bool { return p.refs == 1 }
	// span returns a random run of the model's pages: a whole 2 MiB block one
	// time in eight, and a few pages otherwise.
	span := func() (int, int) {
		if rng.Intn(8) == 0 {
			return rng.Intn(pages/hugePages) * hugePages, hugePages
		}
		n := 1 + rng.Intn(24)
		return rng.Intn(pages - n), n
	}
	// spare returns n pages that no page uses, apart from [page, page+n), if
	// a few tries find them.
	spare := func(page, n int) (int, bool) {
		for try := 0; try < 8; try++ {
			if dst := rng.Intn(pages - n); (dst+n <= page || page+n <= dst) && m.all(dst, n, unused) {
				return dst, true
			}
		}
		return 0, false
	}
	var name string
	// Each step takes a step of one of these kinds, picked by weight; a kind's
	// function returns false if it found nothing to do.
	kinds := []struct {
		name   string
		weight int
		fn     func(page, n int) bool
		done   int
	}{
		{name: "allocate", weight: 3, fn: func(page, n int) bool {
			fr := m.slots(page, n)
			huge := n == hugePages && rng.Intn(2) == 0
			opts := AllocOpts{Kind: usage.Anonymous, Huge: huge, LogicalHugePages: huge}
			if rng.Intn(2) == 0 {
				// As a copy is: filled, and given identities of no slot.
				src := make([]byte, fr.Length())
				reader := safemem.BlockSeqReader{Blocks: safemem.BlockSeqOf(safemem.BlockFromSafeSlice(src))}
				opts.ReaderFunc = reader.ReadToBlocks
			}
			got, err := mf.AllocateAt(fr, opts)
			if !m.all(page, n, unused) {
				if !errors.Is(err, ErrSlotsInUse) {
					t.Fatalf("%s: AllocateAt(%v) over pages in use = %v, %v", name, fr, got, err)
				}
				return false
			}
			if err != nil || got != fr {
				t.Fatalf("%s: AllocateAt(%v) = %v, %v", name, fr, got, err)
			}
			m.all(page, n, func(p *modelPage) bool { *p = modelPage{refs: 1}; return true })
			m.learn(fr)
			return true
		}},
		{name: "reference", weight: 2, fn: func(page, n int) bool {
			if !m.all(page, n, used) {
				return false
			}
			mf.IncRef(m.slots(page, n), 0)
			m.all(page, n, func(p *modelPage) bool { p.refs++; return true })
			return true
		}},
		{name: "drop", weight: 4, fn: func(page, n int) bool {
			if !m.all(page, n, used) {
				return false
			}
			mf.DecRef(m.slots(page, n))
			m.all(page, n, func(p *modelPage) bool {
				if p.refs--; p.refs == 0 {
					*p = modelPage{waste: true}
				}
				return true
			})
			return true
		}},
		{name: "own", weight: 2, fn: func(page, n int) bool {
			// A write in place, by the pages' only holder.
			if !m.all(page, n, used) {
				return false
			}
			fr := m.slots(page, n)
			unique := m.all(page, n, held)
			ids := make([]LogicalPageID, n)
			for k := range ids {
				ids[k] = m.pages[page+k].id
			}
			if got := mf.OwnForWrite(fr); got != unique {
				t.Fatalf("%s: OwnForWrite(%v) = %t, want %t", name, fr, got, unique)
			}
			if !unique {
				return false
			}
			m.learn(fr)
			for k := range ids {
				if m.pages[page+k].id != ids[k] {
					t.Fatalf("%s: OwnForWrite changed page %d's identity", name, page+k)
				}
			}
			return true
		}},
		{name: "copy", weight: 4, fn: func(page, n int) bool {
			// A copy on write, to pages elsewhere in the home.
			dst, ok := spare(page, n)
			if !ok || !m.all(page, n, used) {
				return false
			}
			fr, to := m.slots(page, n), m.slots(dst, n)
			if _, err := mf.AllocateAt(to, AllocOpts{Kind: usage.Anonymous}); err != nil {
				t.Fatalf("%s: AllocateAt(%v): %v", name, to, err)
			}
			m.all(dst, n, func(p *modelPage) bool { *p = modelPage{refs: 1}; return true })
			if err := mf.AdoptCOWIdentities(mf, fr, to); err != nil {
				t.Fatalf("%s: AdoptCOWIdentities(%v, %v): %v", name, fr, to, err)
			}
			m.learn(to)
			for k := 0; k < n; k++ {
				if m.pages[dst+k].id != m.pages[page+k].id {
					t.Fatalf("%s: copy %d took identity %+v, want its original's %+v", name, dst+k, m.pages[dst+k].id, m.pages[page+k].id)
				}
			}
			return true
		}},
		{name: "move", weight: 4, fn: func(page, n int) bool {
			// A fold's move, of pages held alone, to free slots.
			dst, ok := spare(page, n)
			if !ok || !m.all(page, n, held) {
				return false
			}
			fr, to := m.slots(page, n), m.slots(dst, n)
			want := append([]modelPage(nil), m.pages[page:page+n]...)
			if err := mf.MovePages(fr, to); err != nil {
				t.Fatalf("%s: MovePages(%v, %v): %v", name, fr, to, err)
			}
			mf.DecRef(fr)
			copy(m.pages[dst:dst+n], want)
			m.all(page, n, func(p *modelPage) bool { *p = modelPage{waste: true}; return true })
			return true
		}},
		{name: "decommit", weight: 1, fn: func(page, n int) bool {
			if !m.all(page, n, used) {
				return false
			}
			mf.Decommit(m.slots(page, n))
			return true
		}},
		{name: "collapse", weight: 2, fn: func(int, int) bool {
			page := rng.Intn(pages/hugePages) * hugePages
			fr := m.slots(page, hugePages)
			if !m.all(page, hugePages, func(p *modelPage) bool { return p.refs <= 1 }) || mf.IsHugePages(fr) {
				return false
			}
			// Its unused pages are written first, as faults would write them.
			for k := page; k < page+hugePages; {
				if m.pages[k].refs != 0 {
					k++
					continue
				}
				j := k
				for j < page+hugePages && m.pages[j].refs == 0 {
					j++
				}
				if _, err := mf.AllocateAt(m.slots(k, j-k), AllocOpts{Kind: usage.Anonymous}); err != nil {
					t.Fatalf("%s: AllocateAt(%v): %v", name, m.slots(k, j-k), err)
				}
				m.all(k, j-k, func(p *modelPage) bool { *p = modelPage{refs: 1}; return true })
				m.learn(m.slots(k, j-k))
				k = j
			}
			if collapsed, _ := mf.CollapseHuge(fr); !collapsed {
				t.Fatalf("%s: CollapseHuge(%v) did not collapse", name, fr)
			}
			m.learn(fr)
			if !mf.IsHugePages(fr) {
				t.Fatalf("%s: CollapseHuge(%v) left base pages", name, fr)
			}
			return true
		}},
		{name: "release", weight: 1, fn: func(int, int) bool {
			// The releaser releases the waste.
			if !m.all(0, pages, func(p *modelPage) bool { return !p.waste }) {
				mf.mu.Lock()
				mf.unholdReleaserLocked()
				mf.mu.Unlock()
				waitForRelease(t, mf)
				mf.mu.Lock()
				mf.holdReleaserLocked()
				mf.mu.Unlock()
				m.all(0, pages, func(p *modelPage) bool {
					if p.waste {
						*p = modelPage{}
					}
					return true
				})
				return true
			}
			return false
		}},
	}
	total := 0
	for _, kind := range kinds {
		total += kind.weight
	}
	for step := 0; step < 4000; step++ {
		pick := rng.Intn(total)
		k := 0
		for ; pick >= kinds[k].weight; k++ {
			pick -= kinds[k].weight
		}
		page, n := span()
		name = fmt.Sprintf("step %d (%s %d+%d)", step, kinds[k].name, page, n)
		if kinds[k].fn(page, n) {
			kinds[k].done++
		}
		m.check(name)
		if step%100 == 0 {
			m.roundTrip(name)
		}
	}
	mf.mu.Lock()
	mf.unholdReleaserLocked()
	mf.mu.Unlock()
	for _, kind := range kinds {
		if kind.done < 5 {
			t.Errorf("only %d steps of kind %q found something to do", kind.done, kind.name)
		}
	}
}

// A 2 MiB block allocated whole is one run of pages, which a reference over all
// of it keeps whole, a reference dropped from one page splits into three, and
// that page's reference back makes one again. A block whose pages are all
// released keeps nothing.
func TestHomePagesRuns(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 1)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	block := slotRange(home, 0, pagesPerHugePage())
	if _, err := mf.AllocateAt(block, AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatal(err)
	}
	runs := func() int {
		mf.mu.Lock()
		defer mf.mu.Unlock()
		blk := mf.homes[home.Start/chunkSize].block(0)
		if blk == nil {
			return 0
		}
		return len(blk.runs)
	}
	for _, step := range []struct {
		name string
		do   func()
		want int
	}{
		{"a block allocated whole", func() {}, 1},
		{"a reference over all of it", func() { mf.IncRef(block, 0) }, 1},
		{"a reference dropped from one page", func() { mf.DecRef(slotRange(home, 7, 1)) }, 3},
		{"that page's reference back", func() { mf.IncRef(slotRange(home, 7, 1), 0) }, 1},
		{"the references of all of it dropped", func() { mf.DecRef(block); mf.DecRef(block); waitForRelease(t, mf) }, 0},
		{"two pages in an empty block", func() {
			if _, err := mf.AllocateAt(slotRange(home, 40, 2), AllocOpts{Kind: usage.Anonymous}); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"their release", func() { mf.DecRef(slotRange(home, 40, 2)); waitForRelease(t, mf) }, 0},
	} {
		step.do()
		if got := runs(); got != step.want {
			t.Fatalf("after %s, the block has %d runs, want %d", step.name, got, step.want)
		}
	}
}

// A process that forks children one after another, each copying a few of its
// pages and exiting, keeps each block of 2 MiB one run of pages, however many
// children copied its pages before.
func TestHomePagesForkLoop(t *testing.T) {
	mf := newHomeTestMemoryFile(t, 4)
	parent, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	const blocks = 16
	all := slotRange(parent, 0, blocks*pagesPerHugePage())
	for start := all.Start; start < all.End; start += hostarch.HugePageSize {
		if _, err := mf.AllocateAt(memmap.FileRange{Start: start, End: start + hostarch.HugePageSize}, AllocOpts{Kind: usage.Anonymous}); err != nil {
			t.Fatal(err)
		}
	}
	expanded := func() int {
		mf.mu.Lock()
		defer mf.mu.Unlock()
		n := 0
		for b := uint64(0); b < blocks; b++ {
			if blk := mf.homes[parent.Start/chunkSize].block(b); blk == nil || len(blk.runs) != 1 {
				n++
			}
		}
		return n
	}
	rng := rand.New(rand.NewSource(1))
	for child := 0; child < 20; child++ {
		// The fork's references.
		mf.IncRef(all, 0)
		if n := expanded(); n != 0 {
			t.Fatalf("child %d: %d blocks are more than one run after the fork's references", child, n)
		}
		owner := NewHomeOwner()
		home, err := mf.ReserveHome(owner)
		if err != nil {
			t.Fatal(err)
		}
		// The child copies a few pages, and drops its references to them.
		copied := make(map[int]bool)
		for len(copied) < 8 {
			copied[rng.Intn(blocks*int(pagesPerHugePage()))] = true
		}
		var copies []memmap.FileRange
		for k := range copied {
			src, dst := slotRange(parent, uint64(k), 1), slotRange(home, uint64(k), 1)
			if _, err := mf.AllocateAt(dst, AllocOpts{Kind: usage.Anonymous}); err != nil {
				t.Fatal(err)
			}
			if err := mf.AdoptCOWIdentities(mf, src, dst); err != nil {
				t.Fatal(err)
			}
			mf.DecRef(src)
			copies = append(copies, dst)
		}
		// It exits: its references to the rest go, a run at a time, as its
		// pmas held them, and its copies.
		start := 0
		for k := 0; k <= blocks*int(pagesPerHugePage()); k++ {
			if k == blocks*int(pagesPerHugePage()) || copied[k] {
				if k > start {
					mf.DecRef(slotRange(parent, uint64(start), uint64(k-start)))
				}
				start = k + 1
			}
		}
		for _, fr := range copies {
			mf.DecRef(fr)
		}
		mf.DisownHome(owner, home)
		waitForRelease(t, mf)
	}
}

// A save of a MemoryFile with homes, and its load, keep each page of the
// homes: references, identities and versions, 2 MiB pages and contents; and
// the loaded file's homes keep their pages in their tables.
func TestHomePagesSaveLoad(t *testing.T) {
	source := newHomeTestMemoryFile(t, 2)
	home, err := source.ReserveHome(NewHomeOwner())
	if err != nil {
		t.Fatal(err)
	}
	huge := slotRange(home, 0, pagesPerHugePage())
	small := slotRange(home, pagesPerHugePage()+3, 5)
	shared := slotRange(home, 3*pagesPerHugePage(), 9)
	for _, fr := range []memmap.FileRange{huge, small, shared} {
		opts := AllocOpts{Kind: usage.Anonymous}
		if fr == huge {
			opts.Huge, opts.LogicalHugePages = true, true
		}
		if _, err := source.AllocateAt(fr, opts); err != nil {
			t.Fatal(err)
		}
		fillPages(t, source, fr, byte(fr.Start/hostarch.PageSize))
	}
	source.IncRef(shared, 0)
	// One page of the shared run is copied, and written in place.
	source.DecRef(slotRange(home, 3*pagesPerHugePage()+4, 1))
	if !source.OwnForWrite(slotRange(home, 3*pagesPerHugePage()+4, 1)) {
		t.Fatal("a page held alone was not owned")
	}
	all := memmap.FileRange{Start: home.Start, End: shared.End}
	want := source.LogicalPageRanges(all)
	var buf bytes.Buffer
	if err := source.SaveTo(context.Background(), &buf, &SaveOpts{}); err != nil {
		t.Fatal(err)
	}
	// The save leaves the source as it was.
	if got := source.LogicalPageRanges(all); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the save, the source's identities are %+v, want %+v", got, want)
	}

	file, err := os.CreateTemp("", "ttc-home-load-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	target, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
		AdviseNoHugepage:        true,
		MaxHomes:                2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Destroy()
	if err := target.LoadFrom(context.Background(), &buf, &LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := target.LogicalPageRanges(all); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("loaded identities are %+v, want %+v", got, want)
	}
	if !target.IsHugePages(huge) {
		t.Fatal("the loaded 2 MiB page is base pages")
	}
	for _, fr := range []memmap.FileRange{huge, small, shared} {
		if !pagesHold(t, target, fr, byte(fr.Start/hostarch.PageSize)) {
			t.Fatalf("loaded pages %v lost their contents", fr)
		}
	}
	target.mu.Lock()
	unique := target.hasUniqueRefLocked(slotRange(home, 3*pagesPerHugePage()+4, 1))
	sharedRefs := !target.hasUniqueRefLocked(slotRange(home, 3*pagesPerHugePage(), 4))
	inSets := !target.logicalPages.IsEmptyRange(home) || !target.memAcct.IsEmptyRange(home)
	target.mu.Unlock()
	if !unique || !sharedRefs {
		t.Fatalf("loaded references: the copied page held alone %t, the shared ones shared %t", unique, sharedRefs)
	}
	if inSets {
		t.Fatal("the loaded home's pages are in the sets, not its table")
	}
}

// BenchmarkHomeCopyOnWrite copies a quarter of a 256 MiB home that a fork
// shares, in random order, into another home, as the fork's writes do: an
// allocation at each copy's slot, the copy taking its original's identity, and
// the original losing the fork's reference; and then has the parent write the
// pages it holds alone again, in random order.
func BenchmarkHomeCopyOnWrite(b *testing.B) {
	const pages = 256 << 20 / hostarch.PageSize
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		file, err := os.CreateTemp("", "ttc-home-bench-")
		if err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(file.Name()); err != nil {
			b.Fatal(err)
		}
		mf, err := NewMemoryFile(file, MemoryFileOpts{
			DelayedEviction:         DelayedEvictionDisabled,
			DisableIMAWorkAround:    true,
			DisableMemoryAccounting: true,
			AdviseNoHugepage:        true,
			MaxHomes:                2,
		})
		if err != nil {
			b.Fatal(err)
		}
		parent, err := mf.ReserveHome(NewHomeOwner())
		if err != nil {
			b.Fatal(err)
		}
		child, err := mf.ReserveHome(NewHomeOwner())
		if err != nil {
			b.Fatal(err)
		}
		all := slotRange(parent, 0, pages)
		for start := all.Start; start < all.End; start += hostarch.HugePageSize {
			if _, err := mf.AllocateAt(memmap.FileRange{Start: start, End: start + hostarch.HugePageSize}, AllocOpts{Kind: usage.Anonymous}); err != nil {
				b.Fatal(err)
			}
		}
		mf.IncRef(all, 0)
		order := rand.New(rand.NewSource(int64(i))).Perm(pages)[:pages/4]
		b.StartTimer()
		for _, k := range order {
			src, dst := slotRange(parent, uint64(k), 1), slotRange(child, uint64(k), 1)
			if _, err := mf.AllocateAt(dst, AllocOpts{Kind: usage.Anonymous}); err != nil {
				b.Fatal(err)
			}
			if err := mf.AdoptCOWIdentities(mf, src, dst); err != nil {
				b.Fatal(err)
			}
			mf.DecRef(src)
		}
		for _, k := range order {
			if !mf.OwnForWrite(slotRange(parent, uint64(k), 1)) {
				b.Fatal("a page held alone was not owned")
			}
		}
		b.StopTimer()
		mf.Destroy()
	}
}

// BenchmarkHomeFork takes a fork's references to a GiB of a home written whole,
// and drops them again, as a fork and its exec do.
func BenchmarkHomeFork(b *testing.B) {
	mf := newHomeTestMemoryFile(b, 1)
	home, err := mf.ReserveHome(NewHomeOwner())
	if err != nil {
		b.Fatal(err)
	}
	for start := home.Start; start < home.End; start += hostarch.HugePageSize {
		if _, err := mf.AllocateAt(memmap.FileRange{Start: start, End: start + hostarch.HugePageSize}, AllocOpts{Kind: usage.Anonymous}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mf.IncRef(home, 0)
		mf.DecRef(home)
	}
}
