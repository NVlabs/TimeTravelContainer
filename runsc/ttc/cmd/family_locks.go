// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
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

// familyScopedCommands change only the family they name. Removing a family
// also prunes the image roots nothing uses, under the image roots' own lock:
// only commands that create a family add references to them, and those take
// the lifecycle lock exclusively.
var familyScopedCommands = map[string]bool{
	"pause": true, "resume": true, "unpause": true, "quarantine": true, "unquarantine": true,
	"kill": true, "stop": true, "start": true, "restart": true, "fork": true,
	"dehydrate": true, "rehydrate": true, "rm": true, "cp": true,
}

// requestFamilies returns the IDs of the families whose locks the command in
// args takes, in the order to take them, and whether it takes the lifecycle
// lock shared. A daemon that has not yet reconciled its families after
// starting takes the lifecycle lock exclusively for every command.
func (s *daemonServer) requestFamilies(backend *runtimeBackend, args []string) ([]string, bool) {
	clustered := s.distributedSource != nil || s.clusterCapabilities != nil
	if backend.lockFamily == nil && !clustered {
		return nil, false
	}
	if !s.reconciledRoots(backend) {
		return nil, false
	}
	names := daemonCommandFamilyTargets(args)
	if len(names) == 0 {
		return nil, false
	}
	// A name may be a flag's value, which names no family.
	store := backend.containerStore()
	var ids []string
	for _, name := range names {
		if info, err := store.LoadContainer(name); err == nil && !slices.Contains(ids, info.ID) {
			ids = append(ids, info.ID)
		}
	}
	sort.Strings(ids)
	return ids, len(ids) == 1 && familyScopedCommands[daemonCommandName(args)]
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
