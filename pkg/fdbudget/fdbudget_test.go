// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fdbudget

import (
	"fmt"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// testHolder holds a pretend descriptor.
type testHolder struct {
	entry Entry
	open  bool
	busy  bool
}

func (h *testHolder) CloseIdleFD() bool {
	if h.busy {
		return false
	}
	h.open = false
	return true
}

func (h *testHolder) use(fd int) {
	if h.open {
		h.entry.Used()
		return
	}
	h.open = true
	Opened(&h.entry, h, fd)
}

func (h *testHolder) close() {
	h.open = false
	Closed(&h.entry)
}

// reset clears the budget's state between tests.
func reset(t *testing.T, limit uint64) {
	t.Helper()
	budget.mu.Lock()
	for budget.hand != nil {
		removeLocked(budget.hand)
	}
	budget.shrinkers = nil
	budget.warned = false
	budget.mu.Unlock()
	high.Store(0)
	batch.Store(0)
	evictions.Store(0)
	reopens.Store(0)
	if limit != 0 {
		SetLimit(limit)
	}
}

func countOpen(hs []*testHolder) int {
	n := 0
	for _, h := range hs {
		if h.open {
			n++
		}
	}
	return n
}

func TestBelowHighClosesNothing(t *testing.T) {
	reset(t, 1000)
	hs := make([]*testHolder, 100)
	for i := range hs {
		hs[i] = &testHolder{}
		hs[i].use(10 + i)
	}
	if n := countOpen(hs); n != len(hs) {
		t.Errorf("open holders: got %d, want %d", n, len(hs))
	}
	if evicted, _ := Stats(); evicted != 0 {
		t.Errorf("evicted: got %d, want 0", evicted)
	}
}

func TestNearLimitClosesLeastRecentlyUsed(t *testing.T) {
	// A limit of 1000 keeps 250 free and closes 93 at a time.
	reset(t, 1000)
	hs := make([]*testHolder, 700)
	for i := range hs {
		hs[i] = &testHolder{}
		hs[i].use(i)
	}
	// The first half is used again; the clock gives it a second chance.
	for _, h := range hs[:350] {
		h.use(0)
	}
	// Clear referenced bits of the second half, as a hand passing would,
	// so that they are the least recently used.
	for _, h := range hs[350:] {
		h.entry.referenced.Store(false)
	}
	trigger := &testHolder{}
	trigger.use(750)
	evicted, _ := Stats()
	if want := uint64(750 / 8); evicted != want {
		t.Fatalf("evicted: got %d, want %d", evicted, want)
	}
	for i, h := range hs[:350] {
		if !h.open {
			t.Fatalf("holder %d, used again, was closed", i)
		}
	}
	if !trigger.open {
		t.Fatalf("the holder that opened was closed")
	}
	// Closed holders open again, and count as reopens.
	for _, h := range hs[350:] {
		h.use(0)
	}
	if _, reopened := Stats(); reopened != evicted {
		t.Errorf("reopened: got %d, want %d", reopened, evicted)
	}
}

func TestBusyHoldersStayOpen(t *testing.T) {
	// A limit of 100 closes 16 at a time, from descriptor 75.
	reset(t, 100)
	hs := make([]*testHolder, 80)
	for i := range hs {
		hs[i] = &testHolder{busy: i%2 == 0}
		hs[i].use(0)
	}
	Note(99)
	for i, h := range hs {
		if h.busy && !h.open {
			t.Fatalf("busy holder %d was closed", i)
		}
	}
	if n := countOpen(hs); n != len(hs)-16 {
		t.Errorf("open holders: got %d, want %d", n, len(hs)-16)
	}
}

func TestClosedHoldersAreForgotten(t *testing.T) {
	reset(t, 100)
	hs := make([]*testHolder, 50)
	for i := range hs {
		hs[i] = &testHolder{}
		hs[i].use(i)
	}
	for _, h := range hs {
		h.close()
	}
	budget.mu.Lock()
	registered, hand := budget.registered, budget.hand
	budget.mu.Unlock()
	if registered != 0 || hand != nil {
		t.Errorf("after closing all: %d registered, hand %p", registered, hand)
	}
	// Closing again, as when the budget closed it first, is harmless.
	hs[0].close()
}

func TestShrinkersGoFirst(t *testing.T) {
	reset(t, 100)
	idle := 10
	AddShrinker(func(n int) int {
		released := min(n, idle)
		idle -= released
		return released
	})
	hs := make([]*testHolder, 20)
	for i := range hs {
		hs[i] = &testHolder{}
		hs[i].use(i)
		hs[i].entry.referenced.Store(false)
	}
	Note(80)
	if idle != 0 {
		t.Errorf("idle resources left: %d", idle)
	}
	if n := countOpen(hs); n != len(hs)-6 {
		t.Errorf("open holders: got %d, want %d", n, len(hs)-6)
	}
}

func TestRetry(t *testing.T) {
	reset(t, 0)
	if Retry(unix.EBADF) {
		t.Errorf("Retry(EBADF) = true")
	}
	if Retry(unix.EMFILE) {
		t.Errorf("Retry(EMFILE) with nothing to close = true")
	}
	h := &testHolder{}
	h.use(3)
	h.entry.referenced.Store(false)
	if !Retry(fmt.Errorf("open: %w", unix.EMFILE)) {
		t.Errorf("Retry(wrapped EMFILE) = false")
	}
	if h.open {
		t.Errorf("Retry did not close the idle holder")
	}
}

// TestConcurrent registers, uses and closes holders from several goroutines
// while descriptors near the limit evict them, for the race detector.
func TestConcurrent(t *testing.T) {
	reset(t, 200)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			hs := make([]*lockedHolder, 50)
			for i := range hs {
				hs[i] = &lockedHolder{}
			}
			for round := 0; round < 200; round++ {
				h := hs[(round*7+g)%len(hs)]
				h.use(100 + (round*13+g)%100)
				if round%11 == 0 {
					h.close()
				}
			}
			for _, h := range hs {
				h.close()
			}
		}(g)
	}
	wg.Wait()
	budget.mu.Lock()
	registered := budget.registered
	budget.mu.Unlock()
	if registered != 0 {
		t.Errorf("registered after all closed: %d", registered)
	}
}

// lockedHolder guards its pretend descriptor with a mutex, as real holders
// do, which CloseIdleFD only tries.
type lockedHolder struct {
	mu    sync.Mutex
	entry Entry
	open  bool
}

func (h *lockedHolder) CloseIdleFD() bool {
	if !h.mu.TryLock() {
		return false
	}
	defer h.mu.Unlock()
	h.open = false
	return true
}

func (h *lockedHolder) use(fd int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		h.entry.Used()
		return
	}
	h.open = true
	Opened(&h.entry, h, fd)
}

func (h *lockedHolder) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		h.open = false
		Closed(&h.entry)
	}
}
