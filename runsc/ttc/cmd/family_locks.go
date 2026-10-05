// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
)

// A standalone daemon runs commands on different families at once. Each
// family has a lock of its own, which every command that names the family
// takes, and which a long operation keeps while it works, so that the
// family's other commands queue behind it. The lifecycle lock of the data
// root is taken after the families' locks: shared by a command that changes
// only the family it names, and exclusively by any other command, which so
// excludes every command on every family. A cluster node does as a
// standalone daemon does. Each of its family-scoped commands changes its
// family's runtime, record and placements alone, and reconciles only the
// family's placements after (commandReconciledFamilies). A snapshot origin
// that one allocates, for a capture or a repository, is building until the
// capture is over or the repository installs it, and neither the
// reconciliation of the node's origins nor the cleanup of its empty memory
// origins that another runs takes it for a stale one meanwhile; a removal
// rechecks the origins it would abandon with their locks held. With every
// command taking the lifecycle lock alone, a cluster node paused or resumed
// its families 14 times a second however many clients asked.
//
// A command that makes a family, run or create, takes the lifecycle lock
// shared too, and the new family's lock as soon as the family has an ID,
// before anything can name it: see familyMaking. One that would change more
// than the family it makes takes the lifecycle lock alone, as it did, once it
// knows: a daemon made one family at a time, 9 a second on this host's disk
// and 3 a second on a cluster node over the simulated NFS mount, however many
// clients asked, and every command on another family waited for each.

// familyScopedCommands change only the family they name. Removing a family
// also prunes the image roots nothing uses, under the image roots' own lock:
// a command that makes a family and finds its image's root makes the root
// young again under that lock, and a root is pruned only once old.
var familyScopedCommands = map[string]bool{
	"pause": true, "resume": true, "unpause": true, "quarantine": true, "unquarantine": true,
	"kill": true, "stop": true, "start": true, "restart": true, "fork": true,
	"dehydrate": true, "rehydrate": true, "rm": true, "cp": true,
}

// requestFamilies returns the IDs of the families whose locks the command in
// args takes, in the order to take them, and whether it takes the lifecycle
// lock shared. A daemon that has not yet reconciled its families after
// starting takes the lifecycle lock exclusively for every command.
func (s *daemonServer) requestFamilies(backend *runtimeBackend, args []string, claims []daemonPlacementClaim) ([]string, bool) {
	clustered := s.distributedSource != nil || s.clusterCapabilities != nil
	if backend.lockFamily == nil && !clustered {
		return nil, false
	}
	if !s.reconciledRoots(backend) {
		return nil, false
	}
	names := daemonCommandFamilyTargets(args)
	if len(names) == 0 {
		return nil, daemonCommandMakesFamily(args)
	}
	var ids []string
	if family, ok := claimedFamily(names, claims); ok {
		// A command routed here names its family's ID in its claims, which
		// the node it came through read the family's record for: reading
		// it again was a round trip of the cluster's shared root.
		ids = []string{family}
	} else {
		// A name may be a flag's value, which names no family.
		store := backend.containerStore()
		for _, name := range names {
			if info, err := store.LoadContainer(name); err == nil && !slices.Contains(ids, info.ID) {
				ids = append(ids, info.ID)
			}
		}
		sort.Strings(ids)
	}
	return ids, len(ids) == 1 && familyScopedCommands[daemonCommandName(args)]
}

// daemonCommandMakesFamily reports whether args make a family, as run and
// create do.
func daemonCommandMakesFamily(args []string) bool {
	switch daemonCommandName(args) {
	case "run", "create":
		return true
	}
	return false
}

// familyMaking is how a request that makes a family, holding the lifecycle
// lock shared, makes it beside the commands on other families. Only the
// family's name is another command's to take until the family has an ID,
// which nothing else knows: the name is reserved until the request is done,
// and the family's lock taken once it has the ID, which the request then
// holds as it holds the locks of the families it names.
type familyMaking struct {
	server   *daemonServer
	dataRoot string
	hold     *lifecycleHold
	// exclusive takes the lifecycle lock alone.
	exclusive func() func()
	// holding adds a lock the request takes to those it releases when done.
	holding func(release func())
}

// begin readies the making of the family request asks for. A family that
// changes nothing beside itself - no named volume, published port or device -
// is made under its name's reservation, which begin returns the release of;
// for any other, the request takes the lifecycle lock alone.
func (m *familyMaking) begin(request runRequest) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	if !request.makesOnlyItsFamily() {
		m.hold.exclusive(m.exclusive)
		return func() {}, nil
	}
	if request.name == "" {
		return func() {}, nil
	}
	key := dataRootKey(m.dataRoot) + "\x00" + request.name
	s := m.server
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if _, taken := s.namesTaken[key]; taken {
		return nil, fmt.Errorf("container name %q is already in use by a container being made", request.name)
	}
	if s.namesTaken == nil {
		s.namesTaken = make(map[string]struct{})
	}
	s.namesTaken[key] = struct{}{}
	return func() {
		s.mutationMu.Lock()
		delete(s.namesTaken, key)
		s.mutationMu.Unlock()
	}, nil
}

// claim takes the lock of the family with this ID, which b makes, as soon as
// it has the ID. Nothing else knows the ID yet, so nothing holds the lock.
func (m *familyMaking) claim(b *runtimeBackend, id string) error {
	if m == nil {
		return nil
	}
	lock := m.server.familyLock(m.dataRoot, id)
	select {
	case lock <- struct{}{}:
	default:
		return fmt.Errorf("the lock of family %s, being made, is held", id)
	}
	m.holding(func() { <-lock })
	b.heldFamilies = append(append([]string(nil), b.heldFamilies...), id)
	return nil
}

// claimer returns the ID claim that b's making of a family passes to the
// store, or nil.
func (m *familyMaking) claimer(b *runtimeBackend) func(string) error {
	if m == nil {
		return nil
	}
	return func(id string) error { return m.claim(b, id) }
}

// claimedFamily returns the family that the claims of a command naming one
// family, names, all name.
func claimedFamily(names []string, claims []daemonPlacementClaim) (string, bool) {
	if len(claims) == 0 || slices.ContainsFunc(names, func(name string) bool { return name != names[0] }) {
		return "", false
	}
	for _, claim := range claims {
		if claim.FamilyID == "" || claim.FamilyID != claims[0].FamilyID {
			return "", false
		}
	}
	return claims[0].FamilyID, true
}

func dataRootKey(dataRoot string) string {
	key := filepath.Clean(dataRoot)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	return key
}

func (s *daemonServer) lockDataRootShared(dataRoot string) func() {
	lock := s.dataRootLock(dataRoot)
	lock.RLock()
	return lock.RUnlock
}

// familyLock returns the lock of the family with this ID: a channel that
// holds a value while the lock is held, so that taking it can be given up.
func (s *daemonServer) familyLock(dataRoot, id string) chan struct{} {
	key := dataRootKey(dataRoot) + "\x00" + id
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if s.familyLocks == nil {
		s.familyLocks = make(map[string]chan struct{})
	}
	lock := s.familyLocks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		s.familyLocks[key] = lock
	}
	return lock
}

// lockFamilies takes the locks of the families with these IDs, in order,
// unless gone is closed first. It returns what releases them.
func (s *daemonServer) lockFamilies(dataRoot string, ids []string, gone <-chan struct{}) (func(), bool) {
	var held []chan struct{}
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			<-held[i]
		}
		held = nil
	}
	for _, id := range ids {
		lock := s.familyLock(dataRoot, id)
		select {
		case lock <- struct{}{}:
			held = append(held, lock)
		case <-gone:
			release()
			return func() {}, false
		}
	}
	var once sync.Once
	return func() { once.Do(release) }, true
}

// configureFamilyLocks lets a standalone daemon's backend take one family's
// lock, and the lifecycle lock shared, for background work on the family.
func (s *daemonServer) configureFamilyLocks(backend *runtimeBackend) {
	if s.distributedSource != nil || s.clusterCapabilities != nil {
		return
	}
	backend.lockFamily = func(id string) func() {
		release, _ := s.lockFamilies(backend.dataRoot, []string{id}, nil)
		return release
	}
	backend.lockSharedLifecycle = func() func() { return s.lockDataRootShared(backend.dataRoot) }
}
