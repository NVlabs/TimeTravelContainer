// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// A long operation is one whose duration grows with the memory or storage it
// handles: making a dehydration durable, or removing a family. It must not
// hold the daemon's lifecycle lock, or every other command and every listing
// waits for it for minutes. Instead it marks its family busy, gives the lock
// up, does its work, and takes the lock back to finish. Meanwhile listings
// show the family in the operation's transient state, commands that would
// change the family are refused, background lifecycle work on it waits, and
// reconciliation and maintenance leave it alone.

// lifecycleHold is one daemon request's hold on the lifecycle lock, which a
// long operation may give up and take back.
type lifecycleHold struct {
	mu      sync.Mutex
	acquire func() func()
	// release unlocks the lock, or is nil while the request does not hold
	// it.
	release func()
}

// unlock releases the lock if the request holds it.
func (h *lifecycleHold) unlock() {
	h.mu.Lock()
	release := h.release
	h.release = nil
	h.mu.Unlock()
	if release != nil {
		release()
	}
}

// relock takes the lock back if the request gave it up.
func (h *lifecycleHold) relock() {
	h.mu.Lock()
	held := h.release != nil
	h.mu.Unlock()
	if held {
		return
	}
	release := h.acquire()
	h.mu.Lock()
	h.release = release
	h.mu.Unlock()
}

// longOperation describes what a long operation does to its family.
type longOperation struct {
	// branch is the branch it works on, or "" for the whole family.
	branch string
	// state is what it is doing: "dehydrating", "removing".
	state string
	// shown makes listings show state as the state of the branch, or of
	// every branch of the family. An operation that leaves the family as it
	// was, like a checkpoint, is not shown.
	shown bool
	// hidden is a runtime branch of the operation's own, which listings
	// must not show.
	hidden string
}

// familyOperation is one long operation in progress.
type familyOperation struct {
	longOperation
	id      string
	name    string
	started time.Time
	owner   *runtimeBackend
}

// refusal is the error for a command that would change the family.
func (op familyOperation) refusal() error {
	subject := op.name
	if op.branch != "" {
		subject += "@" + op.branch
	}
	return fmt.Errorf("%s is %s; try again when that finishes", subject, op.state)
}

// familyOperations records the families long operations are working on.
type familyOperations struct {
	mu    sync.Mutex
	ended *sync.Cond
	byID  map[string]*familyOperation
}

func (o *familyOperations) begin(op *familyOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if current, ok := o.byID[op.id]; ok {
		return current.refusal()
	}
	if o.byID == nil {
		o.byID = make(map[string]*familyOperation)
	}
	o.byID[op.id] = op
	return nil
}

func (o *familyOperations) end(op *familyOperation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byID[op.id] == op {
		delete(o.byID, op.id)
	}
	if o.ended != nil {
		o.ended.Broadcast()
	}
}

// lookup finds the operation working on the family with this ID or name.
func (o *familyOperations) lookup(idOrName string) (familyOperation, bool) {
	if o == nil || idOrName == "" {
		return familyOperation{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range o.byID {
		if op.id == idOrName || op.name == idOrName {
			return *op, true
		}
	}
	return familyOperation{}, false
}

// any reports whether any long operation is in progress.
func (o *familyOperations) any() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.byID) != 0
}

// waitIdle blocks until no long operation works on the family with this ID.
func (o *familyOperations) waitIdle(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended == nil {
		o.ended = sync.NewCond(&o.mu)
	}
	for o.byID[id] != nil {
		o.ended.Wait()
	}
}

// waitAllIdle blocks until no long operation is in progress.
func (o *familyOperations) waitAllIdle() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended == nil {
		o.ended = sync.NewCond(&o.mu)
	}
	for len(o.byID) != 0 {
		o.ended.Wait()
	}
}

// target returns an operation working on a family that the command in args
// names as one it would change, unless the operation admits the command.
func (o *familyOperations) target(args []string) (familyOperation, bool) {
	if !o.any() {
		return familyOperation{}, false
	}
	command := daemonCommandName(args)
	for _, name := range daemonCommandFamilyTargets(args) {
		if op, ok := o.lookup(name); ok && !op.admits(command) {
			return op, true
		}
	}
	return familyOperation{}, false
}

// daemonCommandName is the command args runs, past a "container" prefix.
func daemonCommandName(args []string) string {
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// daemonCommandKeepsLifecycleLock identifies the commands whose long operations
// must not give the lifecycle lock up. A node command stops, dehydrates or
// migrates branch after branch as one retirement or rebalance, and no other
// command may slip in between them.
func daemonCommandKeepsLifecycleLock(args []string) bool {
	return daemonCommandName(args) == "node"
}

// daemonCommandFamilyTargets returns the families a mutating command names:
// the container part of each operand of the commands that take containers.
// It errs toward naming too much: a flag's value may be taken for an operand.
func daemonCommandFamilyTargets(args []string) []string {
	if !daemonCommandNeedsMutationLock(args) {
		return nil
	}
	if args[0] == "container" {
		args = args[1:]
	}
	switch args[0] {
	case "rm", "stop", "start", "restart", "kill", "pause", "unpause", "resume", "fork",
		"dehydrate", "rehydrate", "checkpoint", "migrate", "commit", "quarantine",
		"unquarantine", "cp", "port":
	default:
		return nil
	}
	var names []string
	for _, arg := range args[1:] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		if i := strings.IndexAny(arg, "@:"); i == 0 {
			continue
		} else if i > 0 {
			arg = arg[:i]
		}
		names = append(names, arg)
	}
	return names
}

// configureLongOperations lets b's long operations give up the lifecycle
// lock that hold holds for b's request.
func (s *daemonServer) configureLongOperations(b *runtimeBackend, hold *lifecycleHold) {
	b.operations = &s.operations
	b.suspendLifecycle = suspendWith(&s.operations, hold)
}

// lockDataRootIdle is lockDataRoot for background work across families, which
// must not act on one a long operation is working on: it waits, without the
// lock, until no long operation is in progress.
func (s *daemonServer) lockDataRootIdle(dataRoot string) func() {
	for {
		unlock := s.lockDataRoot(dataRoot)
		if !s.operations.any() {
			return unlock
		}
		unlock()
		s.operations.waitAllIdle()
	}
}

// suspendWith returns how a long operation gives up the lifecycle lock that
// hold holds: it marks the family busy, releases the lock, and returns what
// takes the lock back and clears the mark.
func suspendWith(operations *familyOperations, hold *lifecycleHold) func(*familyOperation) (func(), error) {
	return func(op *familyOperation) (func(), error) {
		if err := operations.begin(op); err != nil {
			return nil, err
		}
		hold.unlock()
		return func() {
			// Take the lock back before clearing the mark, so that no
			// command sees the family idle before the operation has
			// finished with it.
			hold.relock()
			operations.end(op)
		}, nil
	}
}

// withoutLifecycleLock runs work, which takes time in proportion to the
// memory or storage it handles, without the lifecycle lock, doing what op
// describes to the family info. Outside a daemon request that holds the
// lock, it just runs work.
func (b *runtimeBackend) withoutLifecycleLock(info ttcbundle.ContainerInfo, op longOperation, work func() error) error {
	if b.suspendLifecycle == nil || !b.lifecycleLockHeld {
		return work()
	}
	resume, err := b.suspendLifecycle(&familyOperation{longOperation: op, id: info.ID, name: info.Name, started: time.Now(), owner: b})
	if err != nil {
		return err
	}
	b.lifecycleLockHeld = false
	defer func() {
		resume()
		b.lifecycleLockHeld = true
	}()
	return work()
}

// withoutLifecycleLock is runtimeBackend.withoutLifecycleLock for a command
// that works through the backend interface: target's family is resolved, and
// a backend that cannot give the lock up just runs work.
func withoutLifecycleLock(b backend, target selector, op longOperation, work func() error) error {
	runtime, ok := b.(*runtimeBackend)
	if !ok || runtime.suspendLifecycle == nil || !runtime.lifecycleLockHeld {
		return work()
	}
	info, err := runtime.resolveContainer(target.container)
	if err != nil {
		return err
	}
	return runtime.withoutLifecycleLock(info, op, work)
}

// refuseBusy fails a mutating request that would change a family another
// request's long operation is working on, and any request for a family being
// removed, whose storage is going.
func (b *runtimeBackend) refuseBusy(id string) error {
	op, ok := b.operations.lookup(id)
	if !ok || op.owner == b {
		return nil
	}
	if (b.mutating && !op.admits(b.commandName)) || op.state == "removing" {
		return op.refusal()
	}
	return nil
}

// admits reports whether command may run while the operation works: only a
// kill of a branch being stopped, which ends the stop's grace period as a
// kill during `docker stop` does.
func (op familyOperation) admits(command string) bool {
	return command == "kill" && op.state == "stopping"
}

// forBackground returns a copy of b for background work that outlives b's
// request: its output goes nowhere, and it holds no lifecycle lock, nor can
// give one up, until it takes one of its own.
func (b *runtimeBackend) forBackground() *runtimeBackend {
	background := *b
	background.stdin = strings.NewReader("")
	background.stdout = io.Discard
	background.stderr = io.Discard
	background.lifecycleLockHeld = false
	background.lifecycleReady = nil
	background.suspendLifecycle = nil
	background.mutating = false
	background.commandName = ""
	return &background
}

// holdFamilyLifecycle takes the lifecycle lock for background work on the
// family with this ID, as lockFamilyLifecycle does, and lets the long
// operations of that work give the lock up as a request's can. It returns
// what ends the hold.
func (b *runtimeBackend) holdFamilyLifecycle(id string) func() {
	release := b.lockFamilyLifecycle(id)
	if b.operations == nil {
		return release
	}
	hold := &lifecycleHold{acquire: b.lockBackgroundLifecycle, release: release}
	held, suspend := b.lifecycleLockHeld, b.suspendLifecycle
	b.lifecycleLockHeld, b.suspendLifecycle = true, suspendWith(b.operations, hold)
	return func() {
		b.lifecycleLockHeld, b.suspendLifecycle = held, suspend
		hold.unlock()
	}
}

// lockFamilyLifecycle is lockBackgroundLifecycle for background work on one
// family: it waits, without the lock, for a long operation working on the
// family to finish, unless the operation is b's own.
func (b *runtimeBackend) lockFamilyLifecycle(id string) func() {
	for {
		unlock := b.lockBackgroundLifecycle()
		if op, busy := b.operations.lookup(id); !busy || op.owner == b {
			return unlock
		}
		unlock()
		b.operations.waitIdle(id)
	}
}

// imageOperation is the stand-in family of a long image operation: no
// command names it, but while it works maintenance waits.
func (b *runtimeBackend) imageOperation() ttcbundle.ContainerInfo {
	return ttcbundle.ContainerInfo{ID: fmt.Sprintf("image:%p", b)}
}

// familyID returns the ID of the family container names or identifies, or
// container itself if no family does.
func (b *runtimeBackend) familyID(container string) string {
	if info, err := b.resolveContainer(container); err == nil {
		return info.ID
	}
	return container
}

// showLongOperations shows, in families, the transient state of every long
// operation in progress.
func (b *runtimeBackend) showLongOperations(families []Family) {
	if !b.operations.any() {
		return
	}
	for i := range families {
		op, ok := b.operations.lookup(families[i].ID)
		if !ok {
			continue
		}
		branches := families[i].Branches[:0]
		for _, branch := range families[i].Branches {
			if op.hidden != "" && branch.ID == op.hidden {
				continue
			}
			if op.shown && (op.branch == "" || branch.ID == op.branch) {
				branch.State = op.state
			}
			branches = append(branches, branch)
		}
		families[i].Branches = branches
		families[i].State = familyStateFromBranches(families[i].Branches)
	}
}
