// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package fdbudget keeps the Sentry's host file descriptors within the host's
// limit on them (RLIMIT_NOFILE), so that nothing a guest does runs into it.
//
// What guests open costs the Sentry host descriptors where it keeps host
// objects for them, such as the data objects of TTC volume files, each of
// which it can open again. Their holders register those descriptors
// (Opened). When a descriptor that the Sentry opens shows it near its limit,
// the budget asks idle resources that hold descriptors to release them (see
// AddShrinker), and closes the least recently used registered descriptors
// that are not in use, so that descriptors that the Sentry cannot open again
// always find room. A holder opens a descriptor that the budget closed again
// when it next needs it, as a fault brings back a page that was evicted.
package fdbudget

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// Holder holds a host file descriptor that it can open again.
type Holder interface {
	// CloseIdleFD closes the holder's descriptor, unless it is in use, and
	// reports whether it did. The holder must then open it again before it
	// next uses it, and must not call Closed for it.
	//
	// The budget's lock is held, which holders take while holding their own
	// locks (in Opened and Closed): CloseIdleFD may only try the holder's
	// locks, never wait for them, and must not call into the budget.
	CloseIdleFD() bool
}

// Entry registers a holder's descriptor with the budget. Holders embed one;
// the zero value is not registered.
type Entry struct {
	// holder is the registered holder, or nil if the entry is not
	// registered. holder, prev and next are protected by budget.mu.
	holder Holder
	prev   *Entry
	next   *Entry

	// referenced is set when the descriptor is used, and cleared when the
	// clock hand passes it (see evictLocked).
	referenced atomicbitops.Bool

	// evicted is set while the descriptor is closed by the budget, so that
	// opening it again counts as a reopen. Protected by budget.mu.
	evicted bool
}

const (
	// maxReserve is the most descriptors that the budget keeps free for
	// those that the Sentry cannot open again.
	maxReserve = 1024

	// minBatch is the fewest descriptors that an eviction closes, so that
	// evictions are rare even when the limit is low.
	minBatch = 16

	// unknownBatch is how many descriptors an eviction closes while the
	// limit is unknown, when the host refuses a descriptor (see Retry).
	unknownBatch = 64
)

var budget struct {
	mu sync.Mutex

	// hand is the clock hand, the registered entry that the next eviction
	// looks at first, or nil if no entry is registered.
	hand *Entry

	// registered is the number of registered entries.
	registered int

	// shrinkers release idle resources that hold descriptors.
	shrinkers []func(n int) int

	// warned is set once Retry has found nothing to close.
	warned bool
}

var (
	// high is the lowest descriptor that shows the Sentry near its limit,
	// or 0 while the limit is unknown. The host gives out the lowest free
	// descriptor, so an open that returns high or above means that every
	// descriptor below it is in use.
	high atomicbitops.Int64

	// batch is how many descriptors an eviction closes.
	batch atomicbitops.Int64

	// evictions and reopens count the descriptors that the budget closed,
	// and those of them opened again.
	evictions atomicbitops.Uint64
	reopens   atomicbitops.Uint64
)

// Init sets the budget from the Sentry's limit on host file descriptors. The
// Sentry calls it once it has set that limit, while it may still read it.
func Init() {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		log.Warningf("fdbudget: reading RLIMIT_NOFILE: %v", err)
		return
	}
	SetLimit(limit.Cur)
}

// SetLimit sets the budget for a limit of n host file descriptors.
func SetLimit(n uint64) {
	n = min(n, math.MaxInt32)
	reserve := min(maxReserve, n/4)
	h := int64(n - reserve)
	high.Store(h)
	batch.Store(max(h/8, minBatch))
	log.Infof("fdbudget: limit of %d host file descriptors; descriptors from %d close idle ones", n, h)
}

// Opened registers e, the entry of h, which has just opened descriptor fd, as
// recently used. If fd shows the Sentry near its limit, it then closes
// descriptors that others hold and do not use.
//
// Preconditions: e is not registered.
func Opened(e *Entry, h Holder, fd int) {
	e.referenced.Store(true)
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if e.holder != nil {
		panic("fdbudget: an entry registered twice")
	}
	e.holder = h
	insertLocked(e)
	if e.evicted {
		e.evicted = false
		reopens.Add(1)
	}
	if near(fd) {
		evictLocked(int(batch.Load()))
	}
}

// Note records that the Sentry has opened descriptor fd, which it cannot open
// again, and closes idle descriptors if fd shows the Sentry near its limit.
func Note(fd int) {
	if !near(fd) {
		return
	}
	budget.mu.Lock()
	evictLocked(int(batch.Load()))
	budget.mu.Unlock()
}

// near returns whether descriptor fd shows the Sentry near its limit.
func near(fd int) bool {
	h := high.Load()
	return h > 0 && int64(fd) >= h
}

// Used marks e's descriptor as recently used.
func (e *Entry) Used() {
	if !e.referenced.RacyLoad() {
		e.referenced.Store(true)
	}
}

// Closed unregisters e, whose holder has closed its descriptor. The budget
// may have closed it already, and unregistered e.
func Closed(e *Entry) {
	budget.mu.Lock()
	if e.holder != nil {
		removeLocked(e)
	}
	e.evicted = false
	budget.mu.Unlock()
}

// Retry reports whether to try again an open that failed with err. If the
// host refused the Sentry another descriptor (EMFILE, or ENFILE for the
// host's limit on all processes), it closes idle descriptors first, and the
// open is worth trying again if it closed any.
func Retry(err error) bool {
	if !errors.Is(err, unix.EMFILE) && !errors.Is(err, unix.ENFILE) {
		return false
	}
	n := int(batch.Load())
	if n == 0 {
		n = unknownBatch
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if evictLocked(n) != 0 {
		return true
	}
	if !budget.warned {
		budget.warned = true
		log.Warningf("fdbudget: the host refused a file descriptor (%v), and no idle one could be closed", err)
	}
	return false
}

// AddShrinker registers shrink, which releases up to n idle resources that
// hold descriptors, and returns how many it released. It is the budget's
// first resort when the Sentry nears its limit. shrink runs with the budget's
// lock held, as Holder.CloseIdleFD does, and must follow its rules.
func AddShrinker(shrink func(n int) int) {
	budget.mu.Lock()
	budget.shrinkers = append(budget.shrinkers, shrink)
	budget.mu.Unlock()
}

// Stats returns how many descriptors the budget has closed, and how many of
// those their holders have opened again.
func Stats() (evicted, reopened uint64) {
	return evictions.Load(), reopens.Load()
}

// insertLocked registers e just behind the clock hand, which reaches it
// last.
//
// +checklocks:budget.mu
func insertLocked(e *Entry) {
	if h := budget.hand; h == nil {
		e.prev, e.next = e, e
		budget.hand = e
	} else {
		e.next = h
		e.prev = h.prev
		h.prev.next = e
		h.prev = e
	}
	budget.registered++
}

// removeLocked unregisters e.
//
// +checklocks:budget.mu
func removeLocked(e *Entry) {
	if e.next == e {
		budget.hand = nil
	} else {
		e.prev.next = e.next
		e.next.prev = e.prev
		if budget.hand == e {
			budget.hand = e.next
		}
	}
	e.holder, e.prev, e.next = nil, nil, nil
	budget.registered--
}

// evictLocked closes up to n idle descriptors, and returns how many it
// closed: first those of idle resources, then registered descriptors that
// have not been used since the clock hand last passed them.
//
// +checklocks:budget.mu
func evictLocked(n int) int {
	closed := 0
	for _, shrink := range budget.shrinkers {
		if closed >= n {
			break
		}
		closed += shrink(n - closed)
	}
	// The hand goes round at most twice: the first time, it may only clear
	// what it passes of its referenced bits.
	for steps := 2 * budget.registered; closed < n && steps > 0 && budget.hand != nil; steps-- {
		e := budget.hand
		budget.hand = e.next
		if e.referenced.Load() {
			e.referenced.Store(false)
			continue
		}
		if !e.holder.CloseIdleFD() {
			continue
		}
		removeLocked(e)
		e.evicted = true
		closed++
	}
	if closed != 0 {
		if evictions.Add(uint64(closed)) == uint64(closed) {
			log.Infof("fdbudget: the Sentry nears its limit of host file descriptors; closed %d idle ones", closed)
		} else {
			log.Debugf("fdbudget: closed %d idle host file descriptors", closed)
		}
	}
	return closed
}
