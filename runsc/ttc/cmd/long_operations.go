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
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// A long operation is one whose duration grows with the memory or storage it
// handles: making a dehydration durable, or removing a family. It must not
// hold the daemon's lifecycle lock, or every other command and every listing
// waits for it for minutes. Instead it marks its family busy, gives the lock
// up, does its work, and takes the lock back to finish. Meanwhile listings
// show the family in the operation's transient state, commands that would
// change the family wait for it to finish, as they wait for the lock, and so
// does background lifecycle work on it; reconciliation and maintenance leave
// it alone.

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

// refusal is the error for a second long operation on the family.
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

// waitIdleUnless is waitIdle, except that it also returns when canceled is
// closed. It reports whether the family became idle.
func (o *familyOperations) waitIdleUnless(id string, canceled <-chan struct{}) bool {
	if canceled != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-canceled:
				o.mu.Lock()
				if o.ended != nil {
					o.ended.Broadcast()
				}
				o.mu.Unlock()
			case <-stop:
			}
		}()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended == nil {
		o.ended = sync.NewCond(&o.mu)
	}
	for o.byID[id] != nil {
		select {
		case <-canceled:
			return false
		default:
		}
		o.ended.Wait()
	}
	return true
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

// daemonCommandFamilyTargets returns the families a mutating command names.
// Each of these commands names a branch as FAMILY@BRANCH, and cp as
// FAMILY@BRANCH:PATH; rm, whose flags take no values, also names families
// alone. Other operands name new branches, images or host paths. It errs
// toward naming too much: a flag's value may look like a branch.
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
		if i := strings.Index(arg, "@"); i > 0 {
			names = append(names, arg[:i])
		} else if i < 0 && args[0] == "rm" {
			names = append(names, arg)
		}
	}
	return names
}

// configureLongOperations lets b's long operations give up the lifecycle
// lock that hold holds for b's request, and b wait without it for another
// request's.
func (s *daemonServer) configureLongOperations(b *runtimeBackend, hold *lifecycleHold) {
	b.operations = &s.operations
	b.suspendLifecycle = suspendWith(&s.operations, hold)
	b.lifecycleHold = hold
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
	resume, err := b.releaseLifecycleLock(info, op)
	if err != nil {
		return err
	}
	defer resume()
	return work()
}

// releaseLifecycleLock is withoutLifecycleLock for work that goes on until
// its caller calls what it returns, which takes the lock back, once.
func (b *runtimeBackend) releaseLifecycleLock(info ttcbundle.ContainerInfo, op longOperation) (func(), error) {
	if b.suspendLifecycle == nil || !b.lifecycleLockHeld {
		return func() {}, nil
	}
	resume, err := b.suspendLifecycle(&familyOperation{longOperation: op, id: info.ID, name: info.Name, started: time.Now(), owner: b})
	if err != nil {
		return nil, err
	}
	b.lifecycleLockHeld = false
	var once sync.Once
	return func() {
		once.Do(func() {
			resume()
			b.lifecycleLockHeld = true
		})
	}, nil
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

// waitBusy makes a mutating request that would change a family another
// request's long operation is working on, and any request for a family being
// removed, whose storage is going, wait for the operation to finish, as it
// would wait for the lifecycle lock, and without it. It reports whether it
// waited, in which case the family may have changed or gone meanwhile.
func (b *runtimeBackend) waitBusy(id string) (bool, error) {
	waited := false
	for {
		op, ok := b.operations.lookup(id)
		if !ok || op.owner == b || !(b.mutating && !op.admits(b.commandName) || op.state == "removing") {
			return waited, nil
		}
		// Waiting with the lock would deadlock: the operation takes the lock
		// back to finish.
		if b.lifecycleLockHeld && b.lifecycleHold == nil {
			return waited, op.refusal()
		}
		if b.lifecycleLockHeld {
			b.lifecycleHold.unlock()
		}
		idle := b.operations.waitIdleUnless(id, b.clientGone)
		if b.lifecycleLockHeld {
			b.lifecycleHold.relock()
		}
		if !idle {
			return waited, fmt.Errorf("the client left while %s", op.waiting())
		}
		waited = true
	}
}

// waiting says what a request waits for.
func (op familyOperation) waiting() string {
	subject := op.name
	if op.branch != "" {
		subject += "@" + op.branch
	}
	return fmt.Sprintf("%s was %s", subject, op.state)
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
	background.lifecycleHold = nil
	background.heldFamilies = nil
	background.mutating = false
	background.commandName = ""
	background.removals = nil
	return &background
}

// holdFamilyLifecycle takes the lifecycle lock for background work on the
// family with this ID, as lockFamilyLifecycle does, and lets the long
// operations of that work give the lock up as a request's can. It returns
// what ends the hold.
func (b *runtimeBackend) holdFamilyLifecycle(id string) func() {
	if b.lockFamily != nil {
		return b.holdFamilyLock(id)
	}
	release := b.lockFamilyLifecycle(id)
	if b.operations == nil {
		return release
	}
	hold := &lifecycleHold{acquire: b.lockBackgroundLifecycle, release: release}
	held, suspend, previous := b.lifecycleLockHeld, b.suspendLifecycle, b.lifecycleHold
	b.lifecycleLockHeld, b.suspendLifecycle, b.lifecycleHold = true, suspendWith(b.operations, hold), hold
	return func() {
		b.lifecycleLockHeld, b.suspendLifecycle, b.lifecycleHold = held, suspend, previous
		hold.unlock()
	}
}

// holdFamilyLock is holdFamilyLifecycle on a standalone daemon: the family's
// lock, which a long operation of the work keeps, unless b holds it already,
// and the lifecycle lock shared, which such an operation gives up.
func (b *runtimeBackend) holdFamilyLock(id string) func() {
	releaseFamily := func() {}
	previousFamilies := b.heldFamilies
	if !slices.Contains(b.heldFamilies, id) {
		releaseFamily = b.lockFamily(id)
		b.heldFamilies = append(append([]string(nil), b.heldFamilies...), id)
	}
	hold := &lifecycleHold{acquire: b.lockSharedLifecycle, release: b.lockSharedLifecycle()}
	held, suspend, previousHold := b.lifecycleLockHeld, b.suspendLifecycle, b.lifecycleHold
	b.lifecycleLockHeld, b.lifecycleHold = true, hold
	if b.operations != nil {
		b.suspendLifecycle = suspendWith(b.operations, hold)
	}
	return func() {
		b.lifecycleLockHeld, b.suspendLifecycle, b.lifecycleHold, b.heldFamilies = held, suspend, previousHold, previousFamilies
		hold.unlock()
		releaseFamily()
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

// frozenForAuthorityHandoff reports whether err is the refusal of a snapshot
// whose ledger is frozen while its provider hands it to the shared store: a
// refusal the end of that publication lifts.
func frozenForAuthorityHandoff(err error) bool {
	return err != nil && (errors.Is(err, ttcsnapshot.ErrSnapshotLedgerFrozen) || strings.Contains(err.Error(), "frozen for authority handoff"))
}

// forkHandoffWait bounds how long a fork waits out authority handoffs. A
// handoff that must retry its publication stays frozen through the
// publication queue's backoff, which grows to 256 seconds.
const forkHandoffWait = 5 * time.Minute

// forkThroughHandoffs runs fork, and while it is refused because a snapshot
// the source shares is frozen for an authority handoff, waits without the
// lifecycle lock and runs it again. A refused fork changes nothing, and the
// publication that ends a handoff runs in its own worker.
func (b *runtimeBackend) forkThroughHandoffs(info ttcbundle.ContainerInfo, branch string, fork func() ([]byte, func(bool) error, error)) ([]byte, func(bool) error, error) {
	deadline := time.Now().Add(forkHandoffWait)
	delay := 100 * time.Millisecond
	for {
		output, finish, err := fork()
		if err == nil || !frozenForAuthorityHandoff(err) || time.Now().After(deadline) {
			return output, finish, err
		}
		if err := b.withoutLifecycleLock(info, longOperation{branch: branch, state: "forking"}, func() error {
			time.Sleep(delay)
			return nil
		}); err != nil {
			return nil, nil, err
		}
		delay = min(2*delay, 2*time.Second)
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
