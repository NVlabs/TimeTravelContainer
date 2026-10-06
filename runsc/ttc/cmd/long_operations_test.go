// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// A long operation gives the lifecycle lock up while it works, so other
// requests go on, and takes it back to finish. Meanwhile a request that would
// change its family waits for it to finish, and it alone may change it.
func TestLongOperationWorksWithoutTheLifecycleLock(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	hold := &lifecycleHold{acquire: func() func() { return server.lockDataRoot(root) }, release: server.lockDataRoot(root)}
	owner := &runtimeBackend{lifecycleLockHeld: true, mutating: true}
	server.configureLongOperations(owner, hold)
	other := &runtimeBackend{mutating: true, operations: &server.operations}
	reader := &runtimeBackend{operations: &server.operations}
	info := ttcbundle.ContainerInfo{ID: "0123abcd", Name: "big"}
	worked := false
	otherDone := make(chan error, 1)
	if err := owner.withoutLifecycleLock(info, longOperation{branch: "main", state: "dehydrating", shown: true}, func() error {
		worked = true
		unlock, locked := server.tryLockDataRoot(root)
		if !locked {
			t.Error("the lifecycle lock is held while the long operation works")
		}
		unlock()
		if owner.lifecycleLockHeld {
			t.Error("the owner believes it still holds the lifecycle lock")
		}
		if op, ok := server.operations.lookup("big"); !ok || op.state != "dehydrating" || op.branch != "main" {
			t.Errorf("operation on big=(%+v,%t), want dehydrating big@main", op, ok)
		}
		go func() {
			waited, err := other.waitBusy(info.ID)
			if err == nil && !waited {
				err = errors.New("it did not wait")
			}
			otherDone <- err
		}()
		select {
		case err := <-otherDone:
			t.Errorf("another mutating request did not wait for the operation: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if waited, err := reader.waitBusy(info.ID); waited || err != nil {
			t.Errorf("a reading request waited: %v", err)
		}
		if waited, err := owner.waitBusy(info.ID); waited || err != nil {
			t.Errorf("the owner waited for its own family: %v", err)
		}
		if op, busy := server.operations.target([]string{"rm", "-f", "big"}); !busy || op.name != "big" {
			t.Errorf("rm -f big was not recognized as a command on the busy family")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("the long operation did not run")
	}
	select {
	case err := <-otherDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting request still waits after the operation finished")
	}
	if !owner.lifecycleLockHeld {
		t.Error("the owner does not know it holds the lifecycle lock again")
	}
	if _, locked := server.tryLockDataRoot(root); locked {
		t.Fatal("the long operation did not take the lifecycle lock back")
	}
	if server.operations.any() {
		t.Error("the long operation is still recorded after it finished")
	}
	hold.unlock()
	unlock, locked := server.tryLockDataRoot(root)
	if !locked {
		t.Fatal("the request's hold did not release the lifecycle lock")
	}
	unlock()
}

// Outside a request that holds the lifecycle lock, a long operation simply
// runs.
func TestLongOperationWithoutAHoldJustRuns(t *testing.T) {
	b := &runtimeBackend{}
	worked := false
	if err := b.withoutLifecycleLock(ttcbundle.ContainerInfo{ID: "id", Name: "name"}, longOperation{state: "removing", shown: true}, func() error {
		worked = true
		return nil
	}); err != nil || !worked {
		t.Fatalf("withoutLifecycleLock=(%v), ran %t", err, worked)
	}
}

func TestFamilyTargetsNameWhatAMutationWouldChange(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"rm", "-f", "big"}, []string{"big"}},
		{[]string{"rm", "-f", "big@b2", "small"}, []string{"big", "small"}},
		{[]string{"dehydrate", "big@main", "small@main"}, []string{"big", "small"}},
		{[]string{"container", "stop", "-t", "3", "big@f1"}, []string{"big"}},
		{[]string{"cp", "/host/file", "big@main:/tmp/file"}, []string{"big"}},
		{[]string{"fork", "big@main", "child"}, []string{"big"}},
		{[]string{"commit", "big@main", "small:1"}, []string{"big"}},
		{[]string{"ps", "-a"}, nil},
		{[]string{"run", "-d", "--name", "big", "alpine"}, nil},
		{[]string{"exec", "big@main", "true"}, nil},
	} {
		if got := daemonCommandFamilyTargets(test.args); !reflect.DeepEqual(got, test.want) {
			t.Errorf("daemonCommandFamilyTargets(%q)=%q, want %q", test.args, got, test.want)
		}
	}
}

// A node's retirement or rebalance keeps the lifecycle lock through all its
// branches' long operations, so that nothing slips in between them.
func TestNodeCommandsKeepTheLifecycleLock(t *testing.T) {
	for _, test := range []struct {
		args []string
		keep bool
	}{
		{[]string{"node", "stop", "--dehydrate-all", "n1"}, true},
		{[]string{"node", "rebalance"}, true},
		{[]string{"dehydrate", "big"}, false},
		{[]string{"container", "rm", "-f", "big"}, false},
	} {
		if got := daemonCommandKeepsLifecycleLock(test.args); got != test.keep {
			t.Errorf("daemonCommandKeepsLifecycleLock(%q)=%t, want %t", test.args, got, test.keep)
		}
	}
}

// Listings show a long operation's transient state for the branch it works
// on, or for the whole family.
func TestListingsShowLongOperations(t *testing.T) {
	var operations familyOperations
	if err := operations.begin(&familyOperation{id: "a", name: "alpha", longOperation: longOperation{branch: "main", state: "dehydrating", shown: true}}); err != nil {
		t.Fatal(err)
	}
	if err := operations.begin(&familyOperation{id: "b", name: "beta", longOperation: longOperation{state: "removing", shown: true}}); err != nil {
		t.Fatal(err)
	}
	if err := operations.begin(&familyOperation{id: "c", name: "gamma", longOperation: longOperation{branch: "main", state: "checkpointing", hidden: "checkpoint-1"}}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{operations: &operations}
	families := []Family{
		{ID: "a", State: "running", Branches: []Branch{{ID: "main", State: "dehydrated"}, {ID: "f1", State: "running"}}},
		{ID: "b", State: "exited", Branches: []Branch{{ID: "main", State: "dehydrated"}, {ID: "f1", State: "exited"}}},
		{ID: "c", State: "running", Branches: []Branch{{ID: "checkpoint-1", State: "paused"}, {ID: "main", State: "running"}}},
	}
	b.showLongOperations(families)
	got := []string{families[0].Branches[0].State, families[0].Branches[1].State, families[0].State,
		families[1].Branches[0].State, families[1].Branches[1].State, families[1].State, families[2].State}
	want := []string{"dehydrating", "running", "running", "removing", "removing", "removing", "running"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("states %q, want %q", got, want)
	}
	if len(families[2].Branches) != 1 || families[2].Branches[0].ID != "main" {
		t.Fatalf("a checkpoint's hidden branch is listed: %+v", families[2].Branches)
	}
}

// Background lifecycle work on a family waits, without the lifecycle lock,
// for a long operation working on it to finish.
func TestBackgroundWorkWaitsForALongOperation(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	op := &familyOperation{id: "fam", name: "fam", longOperation: longOperation{state: "removing", shown: true}}
	if err := server.operations.begin(op); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{operations: &server.operations, lockLifecycle: func() func() { return server.lockDataRoot(root) }}
	locked := make(chan func(), 1)
	go func() { locked <- b.lockFamilyLifecycle("fam") }()
	select {
	case <-locked:
		t.Fatal("background work on a busy family took the lifecycle lock")
	case <-time.After(100 * time.Millisecond):
	}
	if unlock, free := server.tryLockDataRoot(root); !free {
		t.Fatal("background work waiting for a busy family holds the lifecycle lock")
	} else {
		unlock()
	}
	server.operations.end(op)
	select {
	case unlock := <-locked:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("background work did not go on once the long operation finished")
	}
}

// Background work on a family holds the lifecycle lock so that its long
// operations can give it up, as a request's can.
func TestBackgroundWorkCanGiveUpTheLifecycleLock(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	b := &runtimeBackend{operations: &server.operations, lockLifecycle: func() func() { return server.lockDataRoot(root) }}
	end := b.holdFamilyLifecycle("fam")
	if _, locked := server.tryLockDataRoot(root); locked {
		t.Fatal("holdFamilyLifecycle did not take the lifecycle lock")
	}
	if err := b.withoutLifecycleLock(ttcbundle.ContainerInfo{ID: "fam", Name: "fam"}, longOperation{state: "dehydrating", shown: true}, func() error {
		unlock, locked := server.tryLockDataRoot(root)
		if !locked {
			t.Error("the lifecycle lock is held while background work's long operation works")
		} else {
			unlock()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, locked := server.tryLockDataRoot(root); locked {
		t.Fatal("the long operation did not take the lifecycle lock back")
	}
	end()
	unlock, locked := server.tryLockDataRoot(root)
	if !locked {
		t.Fatal("ending the hold did not release the lifecycle lock")
	}
	unlock()
	if b.lifecycleLockHeld || b.suspendLifecycle != nil {
		t.Error("ending the hold did not restore the backend")
	}
}

// Background work that outlives a request starts with no lifecycle lock of its
// own, and cannot give up the request's.
func TestBackgroundCopiesHoldNoLifecycleLock(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	hold := &lifecycleHold{acquire: func() func() { return server.lockDataRoot(root) }, release: server.lockDataRoot(root)}
	request := &runtimeBackend{lifecycleLockHeld: true, mutating: true, commandName: "migrate"}
	server.configureLongOperations(request, hold)
	background := request.forBackground()
	if background.lifecycleLockHeld || background.suspendLifecycle != nil || background.mutating || background.commandName != "" {
		t.Errorf("background copy: lock held %t, can suspend %t, mutating %t, command %q",
			background.lifecycleLockHeld, background.suspendLifecycle != nil, background.mutating, background.commandName)
	}
	if background.operations != request.operations {
		t.Error("the background copy does not see the long operations")
	}
	if err := background.withoutLifecycleLock(ttcbundle.ContainerInfo{ID: "fam", Name: "fam"}, longOperation{state: "migrating"}, func() error {
		if unlock, locked := server.tryLockDataRoot(root); locked {
			unlock()
			t.Error("the background copy gave up the request's lifecycle lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hold.unlock()
}

// A migration's source lock can be tried, so that a monitor holding the
// lifecycle lock need not wait for a settlement that holds the source lock.
func TestSourceFamilyLockCanBeTried(t *testing.T) {
	var source distributedSourceRuntime
	unlock := source.lockSourceFamily("fam")
	if _, locked := source.tryLockSourceFamily("fam"); locked {
		t.Fatal("a held source lock was taken again")
	}
	unlock()
	unlock, locked := source.tryLockSourceFamily("fam")
	if !locked {
		t.Fatal("a free source lock was not taken")
	}
	unlock()
}

// Background work across families waits, without the lifecycle lock, until no
// long operation is in progress.
func TestBackgroundWorkAcrossFamiliesWaitsForEveryLongOperation(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	ops := []*familyOperation{
		{id: "a", name: "a", longOperation: longOperation{state: "removing", shown: true}},
		{id: "b", name: "b", longOperation: longOperation{branch: "main", state: "stopping"}},
	}
	for _, op := range ops {
		if err := server.operations.begin(op); err != nil {
			t.Fatal(err)
		}
	}
	locked := make(chan func(), 1)
	go func() { locked <- server.lockDataRootIdle(root) }()
	for _, op := range ops {
		select {
		case <-locked:
			t.Fatal("background work took the lifecycle lock while a long operation works")
		case <-time.After(100 * time.Millisecond):
		}
		if unlock, free := server.tryLockDataRoot(root); !free {
			t.Fatal("background work waiting for long operations holds the lifecycle lock")
		} else {
			unlock()
		}
		server.operations.end(op)
	}
	select {
	case unlock := <-locked:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("background work did not go on once every long operation finished")
	}
}

// A branch being stopped may be killed, as a kill during `docker stop` ends
// the grace period; nothing else goes through, and nothing goes through any
// other long operation.
func TestOnlyAKillGoesThroughAStop(t *testing.T) {
	var operations familyOperations
	stopping := &familyOperation{id: "s", name: "stopping", longOperation: longOperation{branch: "main", state: "stopping"}}
	dehydrating := &familyOperation{id: "d", name: "dehydrating", longOperation: longOperation{branch: "main", state: "dehydrating", shown: true}}
	for _, op := range []*familyOperation{stopping, dehydrating} {
		if err := operations.begin(op); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		args []string
		busy bool
	}{
		{[]string{"kill", "stopping@main"}, false},
		{[]string{"container", "kill", "--signal", "KILL", "stopping@main"}, false},
		{[]string{"rm", "-f", "stopping"}, true},
		{[]string{"stop", "stopping@main"}, true},
		{[]string{"kill", "dehydrating@main"}, true},
	} {
		if _, busy := operations.target(test.args); busy != test.busy {
			t.Errorf("target(%q) busy=%t, want %t", test.args, busy, test.busy)
		}
	}
	killer := &runtimeBackend{mutating: true, commandName: "kill", operations: &operations}
	if waited, err := killer.waitBusy("s"); waited || err != nil {
		t.Errorf("a kill of a stopping branch waited: %v", err)
	}
	waitsUntil(t, killer, "d", func() { operations.end(dehydrating) })
	remover := &runtimeBackend{mutating: true, commandName: "rm", operations: &operations}
	waitsUntil(t, remover, "s", func() { operations.end(stopping) })
}

// waitsUntil checks that b waits for the long operation on the family id, and
// goes on once end ends it.
func waitsUntil(t *testing.T, b *runtimeBackend, id string, end func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		waited, err := b.waitBusy(id)
		if err == nil && !waited {
			err = errors.New("it did not wait")
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("%q did not wait for the operation on %s: %v", b.commandName, id, err)
	case <-time.After(50 * time.Millisecond):
	}
	end()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%q still waits after the operation on %s ended", b.commandName, id)
	}
}

// Every other request waits for a family being removed, reading ones too: its
// storage is going.
func TestNothingReadsAFamilyBeingRemoved(t *testing.T) {
	var operations familyOperations
	removing := &familyOperation{id: "r", name: "removing", longOperation: longOperation{state: "removing", shown: true}}
	dehydrating := &familyOperation{id: "d", name: "dehydrating", longOperation: longOperation{branch: "main", state: "dehydrating", shown: true}}
	for _, op := range []*familyOperation{removing, dehydrating} {
		if err := operations.begin(op); err != nil {
			t.Fatal(err)
		}
	}
	reader := &runtimeBackend{commandName: "diff", operations: &operations}
	if waited, err := reader.waitBusy("d"); waited || err != nil {
		t.Errorf("a reader of a family being dehydrated waited: %v", err)
	}
	waitsUntil(t, reader, "r", func() { operations.end(removing) })
}

// A request waiting for another's long operation stops waiting when its client
// leaves.
func TestAWaitEndsWithItsClient(t *testing.T) {
	var operations familyOperations
	op := &familyOperation{id: "d", name: "dehydrating", longOperation: longOperation{branch: "main", state: "dehydrating", shown: true}}
	if err := operations.begin(op); err != nil {
		t.Fatal(err)
	}
	canceled := make(chan struct{})
	waiter := &runtimeBackend{mutating: true, commandName: "pause", operations: &operations, clientGone: canceled}
	done := make(chan error, 1)
	go func() {
		_, err := waiter.waitBusy("d")
		done <- err
	}()
	close(canceled)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "dehydrating@main was dehydrating") {
			t.Errorf("a wait whose client left ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wait whose client left still waits")
	}
}

// A fork refused by a frozen authority handoff is run again once the handoff
// is over, without the lifecycle lock meanwhile; any other failure is final.
func TestForkWaitsOutAHandoff(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	hold := &lifecycleHold{acquire: func() func() { return server.lockDataRoot(root) }, release: server.lockDataRoot(root)}
	b := &runtimeBackend{lifecycleLockHeld: true, mutating: true}
	server.configureLongOperations(b, hold)
	defer hold.unlock()
	info := ttcbundle.ContainerInfo{ID: "id", Name: "s"}
	attempts := 0
	output, _, err := b.forkThroughHandoffs(info, "main", func() ([]byte, func(bool) error, error) {
		attempts++
		if attempts < 3 {
			if _, locked := server.tryLockDataRoot(root); locked {
				t.Error("a fork attempt ran without the lifecycle lock")
			}
			return nil, nil, errors.New("publishing source metadata before fork: snapshot 79 references are frozen for authority handoff: snapshot ledger is frozen for authority handoff")
		}
		return []byte("forked"), func(bool) error { return nil }, nil
	})
	if err != nil || string(output) != "forked" || attempts != 3 {
		t.Fatalf("forkThroughHandoffs=(%q,%v) after %d attempts, want forked after 3", output, err, attempts)
	}
	attempts = 0
	if _, _, err := b.forkThroughHandoffs(info, "main", func() ([]byte, func(bool) error, error) {
		attempts++
		return nil, nil, errors.New("branch \"a1\" already exists")
	}); err == nil || attempts != 1 {
		t.Fatalf("an ordinary failure was retried: %d attempts, %v", attempts, err)
	}
}

func TestFrozenForAuthorityHandoff(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("importing live graph memory: %w", fmt.Errorf("snapshot 6 references are frozen for authority handoff: %w", ttcsnapshot.ErrSnapshotLedgerFrozen)), true},
		// A peer's refusal reaches this node as its message alone.
		{errors.New("cluster peer: snapshot 6 objects are frozen for authority handoff: snapshot ledger is frozen for authority handoff"), true},
		{errors.New("importing live graph memory: no space left on device"), false},
		{nil, false},
	} {
		if got := frozenForAuthorityHandoff(test.err); got != test.want {
			t.Errorf("frozenForAuthorityHandoff(%v) = %t, want %t", test.err, got, test.want)
		}
	}
}

// A capture that finds the snapshot handoff gate taken waits for it without
// the lifecycle lock: the gate's holder, a dehydration that streamed its
// memory without the lock, takes the lock back before it lets the gate go.
func TestCaptureWaitsForTheGateWithoutTheLifecycleLock(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	source := &distributedSourceRuntime{}
	hold := &lifecycleHold{acquire: func() func() { return server.lockDataRoot(root) }, release: server.lockDataRoot(root)}
	b := &runtimeBackend{distributedSource: source, lifecycleLockHeld: true, mutating: true}
	server.configureLongOperations(b, hold)
	defer hold.unlock()
	info := ttcbundle.ContainerInfo{ID: "id", Name: "s"}

	finish, err := b.beginForegroundCapture(info, longOperation{branch: "main", state: "forking"})
	if err != nil {
		t.Fatal(err)
	}
	if unlock, locked := server.tryLockDataRoot(root); locked {
		unlock()
		t.Error("a capture that found the gate free gave the lifecycle lock up")
	}
	finish()

	// A capture that streamed without the lifecycle lock waits to take it
	// back, holding the gate; a final switch waits for the gate, and so holds
	// new captures back.
	finishStreamed := source.beginCapture()
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		unlock := server.lockDataRoot(root)
		finishStreamed()
		unlock()
	}()
	switched := make(chan struct{})
	go func() {
		defer close(switched)
		source.beginFinalSnapshotSwitch()()
	}()
	for {
		finish, ok := source.tryBeginCapture()
		if !ok {
			break
		}
		finish()
		time.Sleep(time.Millisecond)
	}
	captured := make(chan error, 1)
	go func() {
		finish, err := b.beginForegroundCapture(info, longOperation{branch: "main", state: "forking"})
		if err == nil {
			finish()
		}
		captured <- err
	}()
	select {
	case err := <-captured:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the capture waits for the gate holding the lock that the gate's holder waits for")
	}
	<-streamed
	<-switched
	if !b.lifecycleLockHeld {
		t.Error("the capture does not know it holds the lifecycle lock again")
	}
	if unlock, locked := server.tryLockDataRoot(root); locked {
		unlock()
		t.Error("the capture did not take the lifecycle lock back")
	}
}

// A command making branches in a runtime releases it, and waits for the
// branches being made there, as if its own were not: a fork of a dehydrated
// branch, making its children, found the runtime idle and waited for its own
// children, holding the family's lifecycle lock.
func TestReleaseAndMakingLeaveOutTheCallersOwnBranches(t *testing.T) {
	var operations familyOperations
	made := operations.beginMaking("web", "child")
	if _, ok := operations.beginRelease("web"); ok {
		t.Fatal("a runtime another command makes a branch in was released")
	}
	release, ok := operations.beginRelease("web", "child")
	if !ok {
		t.Fatal("a command's own branches kept it from releasing the runtime")
	}
	release()
	waited := make(chan struct{})
	go func() {
		operations.waitMadeIn("web", "child")
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a command waited for its own branches to be made")
	}
	other := operations.beginMaking("web", "other")
	if _, ok := operations.beginRelease("web", "child"); ok {
		t.Fatal("a runtime another command makes a branch in was released")
	}
	waited = make(chan struct{})
	go func() {
		operations.waitMadeIn("web", "child")
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("waitMadeIn returned while another command made a branch")
	case <-time.After(50 * time.Millisecond):
	}
	other()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waitMadeIn did not return once the other branch was made")
	}
	// Another command making a branch of the same name keeps the runtime.
	same := operations.beginMaking("web", "child")
	if _, ok := operations.beginRelease("web", "child"); ok {
		t.Fatal("a runtime another command makes the same branch in was released")
	}
	same()
	made()
}

// A runtime a command is making a branch in is not released, and a command
// making a branch in a runtime being released waits for the release: a remote
// fork imports its branch into the family's Sentry on the node without the
// node's lifecycle lock.
func TestReleaseAndMakingExcludeEachOther(t *testing.T) {
	var operations familyOperations
	made := operations.beginMaking("web", "b1")
	if _, ok := operations.beginRelease("web"); ok {
		t.Fatal("a runtime a command makes a branch in was released")
	}
	if release, ok := operations.beginRelease("other"); !ok {
		t.Fatal("another runtime's release was refused")
	} else {
		release()
	}
	waited := make(chan struct{})
	go func() {
		operations.waitMadeIn("web")
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("waitMadeIn returned while a branch was being made")
	case <-time.After(50 * time.Millisecond):
	}
	made()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waitMadeIn did not return once the branch was made")
	}
	release, ok := operations.beginRelease("web")
	if !ok {
		t.Fatal("a runtime no command makes a branch in was not released")
	}
	making := make(chan func(), 1)
	go func() { making <- operations.beginMaking("web", "b2") }()
	select {
	case done := <-making:
		done()
		t.Fatal("a command made a branch in a runtime being released")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case done := <-making:
		done()
	case <-time.After(5 * time.Second):
		t.Fatal("making a branch did not go on once the release ended")
	}
}
