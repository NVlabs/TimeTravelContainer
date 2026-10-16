// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package systrap

import (
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// stubDeaths tells the Platform's users of each subprocess in use whose stub
// process dies.
//
// The host may kill a stub apart from the Sentry, as its out-of-memory killer
// does when the stub holds the most memory resident. Killing a stub frees none
// of its pages, which the Sentry's MemoryFile holds until the address space is
// released, so the tasks using it must exit at once, those blocked in the
// Sentry too, which would otherwise see their stub gone only when they next
// ran, if ever: until then their memory stayed held.
//
// Every stub is a child of the Sentry (see thread.createStub), so the Sentry
// gets a SIGCHLD when one dies.
type stubDeaths struct {
	mu sync.Mutex

	// inUse are the subprocesses that are AddressSpaces.
	//
	// +checklocks:mu
	inUse map[*subprocess]struct{}

	// handlers are told of each subprocess in inUse that dies.
	//
	// +checklocks:mu
	handlers map[*stubDeathHandler]struct{}
}

type stubDeathHandler struct {
	f func(platform.AddressSpace)
}

var deaths = stubDeaths{
	inUse:    make(map[*subprocess]struct{}),
	handlers: make(map[*stubDeathHandler]struct{}),
}

// watch starts telling the handlers of the stubs that die.
func (d *stubDeaths) watch() {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, unix.SIGCHLD)
	go func() { // S/R-SAFE: platform-related.
		for range sigchld {
			d.scan()
		}
	}()
}

func (d *stubDeaths) add(s *subprocess) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inUse[s] = struct{}{}
}

func (d *stubDeaths) remove(s *subprocess) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inUse, s)
}

// scan tells the handlers of each subprocess in use whose stub died.
func (d *stubDeaths) scan() {
	d.mu.Lock()
	inUse := make([]*subprocess, 0, len(d.inUse))
	for s := range d.inUse {
		inUse = append(inUse, s)
	}
	d.mu.Unlock()
	for _, s := range inUse {
		if s.alive() {
			continue
		}
		d.mu.Lock()
		_, ok := d.inUse[s]
		delete(d.inUse, s)
		handlers := make([]*stubDeathHandler, 0, len(d.handlers))
		for h := range d.handlers {
			handlers = append(handlers, h)
		}
		d.mu.Unlock()
		if !ok {
			// Released meanwhile.
			continue
		}
		log.Warningf("The stub process %d died; killing the tasks it ran.", s.syscallThread.thread.tgid)
		for _, h := range handlers {
			h.f(s)
		}
	}
}

// NotifyAddressSpaceDeath implements
// platform.AddressSpaceDeathNotifier.NotifyAddressSpaceDeath.
func (*Systrap) NotifyAddressSpaceDeath(f func(platform.AddressSpace)) (stop func()) {
	h := &stubDeathHandler{f: f}
	deaths.mu.Lock()
	deaths.handlers[h] = struct{}{}
	deaths.mu.Unlock()
	return func() {
		deaths.mu.Lock()
		delete(deaths.handlers, h)
		deaths.mu.Unlock()
	}
}

// stubDied returns true if s's stub is dead, so that r, the panic of a ptrace
// operation on it, is no bug of the Sentry's: the operation failed because the
// stub was killed, and the tasks using s die for it.
func stubDied(s *subprocess, r any) bool {
	if s.alive() {
		return false
	}
	log.Warningf("A ptrace operation on the stub process %d failed, as it died: %v", s.syscallThread.thread.tgid, r)
	return true
}
