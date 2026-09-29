// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSharedBlockRoundTripAndTornBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	want := AuthorityRecord{Epoch: 7, NodeID: "node-a", Incarnation: "i-1", Role: RoleCompute, ControlURL: "http://node-a:7443"}
	if err := writeSharedBlock(path, want); err != nil {
		t.Fatal(err)
	}
	// A shorter record written over a longer one leaves no trace of it.
	shorter := AuthorityRecord{Epoch: 8, NodeID: "b"}
	if err := writeSharedBlock(path, shorter); err != nil {
		t.Fatal(err)
	}
	var got AuthorityRecord
	if err := readSharedBlock(path, &got); err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 8 || got.NodeID != "b" || got.ControlURL != "" {
		t.Fatalf("read %+v, want %+v", got, shorter)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := readSharedBlock(path, &got); err == nil {
		t.Fatal("read a torn block")
	}
}

func TestClaimEpochIsExclusive(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	if latest, err := shared.LatestClaim(); err != nil || latest != 0 {
		t.Fatalf("LatestClaim() = %d, %v, want 0", latest, err)
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range []string{"node-a", "node-b", "node-c", "node-d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := shared.ClaimEpoch(AuthorityRecord{Epoch: 3, NodeID: node})
			if err != nil {
				t.Error(err)
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d nodes claimed epoch 3, want 1", wins)
	}
	if latest, err := shared.LatestClaim(); err != nil || latest != 3 {
		t.Fatalf("LatestClaim() = %d, %v, want 3", latest, err)
	}
}

func TestHeartbeatAdvancesAndLivenessSeesItStop(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	heartbeat, err := StartHeartbeat(shared, MemberRecord{NodeID: "node-a", Incarnation: "i-1", Role: RoleCompute})
	if err != nil {
		t.Fatal(err)
	}
	if !heartbeat.Held() {
		t.Fatal("a daemon whose heartbeat landed does not hold its lease")
	}
	var liveness Liveness
	first, err := shared.ReadMember("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if still := liveness.StillFor(first); still != 0 {
		t.Fatalf("a counter seen once stood still for %s", still)
	}
	time.Sleep(HeartbeatInterval + 500*time.Millisecond)
	advanced, err := shared.ReadMember("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Counter <= first.Counter {
		t.Fatalf("counter %d did not advance past %d", advanced.Counter, first.Counter)
	}
	if still := liveness.StillFor(advanced); still != 0 {
		t.Fatalf("an advanced counter stood still for %s", still)
	}
	heartbeat.Stop()
	stopped, err := shared.ReadMember("node-a")
	if err != nil {
		t.Fatal(err)
	}
	liveness.StillFor(stopped)
	time.Sleep(100 * time.Millisecond)
	if still := liveness.StillFor(stopped); still < 100*time.Millisecond {
		t.Fatalf("a stopped counter stood still for %s, want at least 100ms", still)
	}
	// The node's next daemon continues its counter.
	next, err := StartHeartbeat(shared, MemberRecord{NodeID: "node-a", Incarnation: "i-2", Role: RoleCompute})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	if record := next.Record(); record.Counter <= stopped.Counter {
		t.Fatalf("the next daemon's counter %d restarted below %d", record.Counter, stopped.Counter)
	}
}

// ageLiveness makes liveness have seen every member's counter stand still
// for AuthorityTakeover.
func ageLiveness(liveness *Liveness) {
	liveness.mu.Lock()
	defer liveness.mu.Unlock()
	for id, seen := range liveness.seen {
		seen.since = time.Now().Add(-AuthorityTakeover)
		liveness.seen[id] = seen
	}
}

func startTestMember(t *testing.T, shared SharedCluster, node, incarnation, role string) (*Heartbeat, AuthorityRecord) {
	t.Helper()
	heartbeat, err := StartHeartbeat(shared, MemberRecord{NodeID: node, Incarnation: incarnation, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(heartbeat.Stop)
	return heartbeat, AuthorityRecord{NodeID: node, Incarnation: incarnation, Role: role, ControlURL: "http://" + node + ":7443"}
}

func TestDecideAuthorityStartsAClusterOnce(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	type result struct {
		decision AuthorityDecision
		err      error
	}
	results := make(chan result, 3)
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		_, self := startTestMember(t, shared, node, "i-1", RoleCompute)
		go func() {
			var liveness Liveness
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			decision, err := shared.DecideAuthority(ctx, self, &liveness)
			results <- result{decision, err}
		}()
	}
	holders := 0
	var holder string
	var joined []string
	for range 3 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.decision.Hold {
			holders++
			holder = r.decision.Record.NodeID
			if r.decision.Record.Epoch != 1 || r.decision.Record.Previous != 0 {
				t.Fatalf("the first authority holds %+v, want epoch 1 from nothing", r.decision.Record)
			}
		} else {
			joined = append(joined, r.decision.Record.NodeID)
		}
	}
	if holders != 1 {
		t.Fatalf("%d daemons hold the new cluster's authority, want 1", holders)
	}
	for _, node := range joined {
		if node != holder {
			t.Fatalf("a daemon joins %s, not the holder %s", node, holder)
		}
	}
}

func TestDecideAuthorityKeepsAHolderAndTakesOverFromAStoppedOne(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	aHeartbeat, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var aLiveness Liveness
	ctx := context.Background()
	decision, err := shared.DecideAuthority(ctx, a, &aLiveness)
	if err != nil || !decision.Hold {
		t.Fatalf("the first daemon decided %+v, %v", decision, err)
	}
	// Restarted, it holds it still.
	if again, err := shared.DecideAuthority(ctx, a, &aLiveness); err != nil || !again.Hold || again.Record.Epoch != 1 {
		t.Fatalf("the restarted holder decided %+v, %v", again, err)
	}
	_, b := startTestMember(t, shared, "node-b", "i-1", RoleCompute)
	var bLiveness Liveness
	if joined, err := shared.DecideAuthority(ctx, b, &bLiveness); err != nil || joined.Hold || joined.Record.NodeID != "node-a" {
		t.Fatalf("a daemon beside a live holder decided %+v, %v", joined, err)
	}
	// Its counter stands still.
	aHeartbeat.Stop()
	shared.DecideAuthority(ctx, b, &bLiveness)
	ageLiveness(&bLiveness)
	taken, err := shared.DecideAuthority(ctx, b, &bLiveness)
	if err != nil || !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Previous != 1 || taken.Record.NodeID != "node-b" {
		t.Fatalf("a daemon beside a stopped holder decided %+v, %v", taken, err)
	}
	// The old holder, back, joins the new one.
	back, _ := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	defer back.Stop()
	if rejoined, err := shared.DecideAuthority(ctx, a, &aLiveness); err != nil || rejoined.Hold || rejoined.Record.NodeID != "node-b" {
		t.Fatalf("the old holder, back, decided %+v, %v", rejoined, err)
	}
}

// A daemon that stops cleanly leaves: a reader takes it for stopped at once,
// without waiting for its counter to stand still, and takes the authority it
// held.
func TestDecideAuthorityTakesOverAtOnceFromAHolderThatLeft(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	aHeartbeat, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var aLiveness Liveness
	ctx := context.Background()
	if decision, err := shared.DecideAuthority(ctx, a, &aLiveness); err != nil || !decision.Hold {
		t.Fatalf("the first daemon decided %+v, %v", decision, err)
	}
	_, b := startTestMember(t, shared, "node-b", "i-1", RoleCompute)
	var bLiveness Liveness
	if joined, err := shared.DecideAuthority(ctx, b, &bLiveness); err != nil || joined.Hold {
		t.Fatalf("a daemon beside a live holder decided %+v, %v", joined, err)
	}
	if err := aHeartbeat.Leave(); err != nil {
		t.Fatal(err)
	}
	left, err := shared.ReadMember("node-a")
	if err != nil || !left.Stopped {
		t.Fatalf("the member record of a daemon that left is %+v, %v", left, err)
	}
	if bLiveness.Advancing(left) {
		t.Fatal("a daemon that left is advancing")
	}
	taken, err := shared.DecideAuthority(ctx, b, &bLiveness)
	if err != nil || !taken.Hold || taken.Record.NodeID != "node-b" || taken.Record.Previous != 1 {
		t.Fatalf("a daemon beside a holder that left decided %+v, %v", taken, err)
	}
}

func TestDecideAuthorityTakesOverFromAReplacedIncarnation(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	aHeartbeat, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var liveness Liveness
	ctx := context.Background()
	if decision, err := shared.DecideAuthority(ctx, a, &liveness); err != nil || !decision.Hold {
		t.Fatalf("decided %+v, %v", decision, err)
	}
	// Its node comes back without its local root, a new incarnation.
	aHeartbeat.Stop()
	_, replaced := startTestMember(t, shared, "node-a", "i-2", RoleCompute)
	taken, err := shared.DecideAuthority(ctx, replaced, &liveness)
	if err != nil || !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Incarnation != "i-2" {
		t.Fatalf("the new incarnation decided %+v, %v", taken, err)
	}
}

func TestDecideAuthorityTakesItFromAControlDaemonWhichGivesItUp(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	loginHeartbeat, login := startTestMember(t, shared, "login", "i-1", RoleControl)
	var liveness Liveness
	ctx := context.Background()
	held, err := shared.DecideAuthority(ctx, login, &liveness)
	if err != nil || !held.Hold {
		t.Fatalf("the login daemon alone decided %+v, %v", held, err)
	}
	// A compute daemon waits for the control daemon to give the authority
	// up, rather than join it.
	_, compute := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var computeLiveness Liveness
	decisions := make(chan AuthorityDecision, 1)
	go func() {
		taken, err := shared.DecideAuthority(ctx, compute, &computeLiveness)
		if err != nil {
			t.Error(err)
		}
		decisions <- taken
	}()
	select {
	case taken := <-decisions:
		t.Fatalf("a compute daemon beside a control holder decided %+v before it gave the authority up", taken)
	case <-time.After(3 * HeartbeatInterval / 2):
	}
	if err := loginHeartbeat.Update(func(record *MemberRecord) { record.Released = held.Record.Epoch }, true); err != nil {
		t.Fatal(err)
	}
	taken := <-decisions
	if !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Previous != 1 {
		t.Fatalf("a compute daemon decided %+v once the control daemon gave the authority up", taken)
	}
	if err := shared.AwaitRelease(ctx, held.Record, &computeLiveness); err != nil {
		t.Fatal(err)
	}
	// A control daemon does not take it from a compute one.
	if joined, err := shared.DecideAuthority(ctx, login, &liveness); err != nil || joined.Hold {
		t.Fatalf("the control daemon decided %+v, %v", joined, err)
	}
}

func TestLivenessAdvancingNeedsACounterSeenToChange(t *testing.T) {
	var liveness Liveness
	record := MemberRecord{NodeID: "node-a", Incarnation: "i-1", Counter: 7}
	if liveness.Advancing(record) {
		t.Fatal("a counter seen once is advancing")
	}
	if liveness.Advancing(record) {
		t.Fatal("a counter that stood still is advancing")
	}
	record.Counter++
	if !liveness.Advancing(record) {
		t.Fatal("a counter that changed is not advancing")
	}
	// Another incarnation's counter starts over.
	record.Incarnation, record.Counter = "i-2", 1
	if liveness.Advancing(record) {
		t.Fatal("a new incarnation's counter is advancing when first seen")
	}
}

// A Liveness takes what another on the host saw of a counter for what it saw
// itself, until it sees the counter itself.
func TestLivenessAssumesWhatAnotherSaw(t *testing.T) {
	var gateway Liveness
	record := MemberRecord{NodeID: "node-a", Incarnation: "i-1", Counter: 7}
	gateway.StillFor(record)
	time.Sleep(50 * time.Millisecond)
	seen := gateway.Stillness(record)
	if seen.Still < 50*time.Millisecond || seen.Counter != 7 || seen.Incarnation != "i-1" {
		t.Fatalf("the gateway saw %+v", seen)
	}
	var holder Liveness
	holder.Assume(seen)
	if still := holder.StillFor(record); still < 50*time.Millisecond {
		t.Fatalf("the holder saw the counter still for %s, want what the gateway saw", still)
	}
	record.Counter++
	if still := holder.StillFor(record); still != 0 {
		t.Fatalf("a counter that moved stood still for %s", still)
	}
	// What the holder saw itself stands.
	holder.Assume(seen)
	if still := holder.StillFor(record); still > 10*time.Millisecond {
		t.Fatalf("an assumption replaced what the holder saw: still for %s", still)
	}
}

func TestDecideAuthorityWaitsForAClaimInProgress(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	_, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	claim := a
	claim.Epoch = 1
	if won, err := shared.ClaimEpoch(claim); err != nil || !won {
		t.Fatalf("ClaimEpoch() = %t, %v", won, err)
	}
	_, b := startTestMember(t, shared, "node-b", "i-1", RoleCompute)
	var liveness Liveness
	ctx, cancel := context.WithTimeout(context.Background(), 3*HeartbeatInterval/2)
	defer cancel()
	if decision, err := shared.DecideAuthority(ctx, b, &liveness); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a daemon beside a claim in progress decided %+v, %v, want it to wait", decision, err)
	}
	// The claimant restarted before it recorded the claim: it holds it.
	held, err := shared.DecideAuthority(context.Background(), a, &liveness)
	if err != nil || !held.Hold || held.Record.Epoch != 1 {
		t.Fatalf("the claimant decided %+v, %v", held, err)
	}
	if joined, err := shared.DecideAuthority(context.Background(), b, &liveness); err != nil || joined.Hold || joined.Record.NodeID != "node-a" {
		t.Fatalf("a daemon beside the recorded claim decided %+v, %v", joined, err)
	}
}

// The daemon's path: an epoch's holder stops, and the next epoch's holder
// takes its state over.
func TestEpochHoldersTakeOver(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	first := holdTestEpoch(t, shared, "node-a", 1, 0)
	registerReadyNode(t, first.node, "node-a", "i-1")
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	first.heartbeat.Stop()
	second := holdTestEpoch(t, shared, "node-b", 2, 1)
	state, err := second.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := state.Nodes["node-a"]; !ok || record.State != NodeReady {
		t.Fatalf("epoch 2 has node-a as %+v (%t)", record, ok)
	}
	// A read only asks whether the node still holds the authority, and
	// writes nothing.
	segment := lastSegment(t, shared.EpochDir(2))
	before, err := os.Stat(segment)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := second.node.LinearizableState(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if after, err := os.Stat(segment); err != nil || after.Size() != before.Size() {
		t.Fatalf("reads wrote the log from %d to %v bytes, %v", before.Size(), after.Size(), err)
	}
	// Epoch 3's holder would prune what came before epoch 2.
	if err := shared.PruneEpochs(2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shared.EpochDir(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("epoch 1's state remains: %v", err)
	}
	if latest, err := shared.LatestClaim(); err != nil || latest != 2 {
		t.Fatalf("latest claim %d, %v, want 2", latest, err)
	}
	if !shared.Prepared(2) {
		t.Fatal("epoch 2's state was pruned")
	}
}

// A holder view reads the record again only once what it read is older than
// its caller allows.
func TestHolderViewKeepsTheRecordItRead(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	view := &HolderView{Cluster: shared}
	if _, recorded, err := view.Record(time.Second); err != nil || recorded {
		t.Fatalf("the record of a new cluster: %t, %v", recorded, err)
	}
	if err := shared.WriteAuthority(AuthorityRecord{Epoch: 1, NodeID: "node-a", Incarnation: "i-1"}); err != nil {
		t.Fatal(err)
	}
	if _, recorded, err := view.Record(time.Hour); err != nil || recorded {
		t.Fatalf("the view read the record again within its age: %t, %v", recorded, err)
	}
	record, recorded, err := view.Record(0)
	if err != nil || !recorded || record.NodeID != "node-a" {
		t.Fatalf("the record read again is %+v, %t, %v, want node-a's", record, recorded, err)
	}
}

// A read of the authority record that fails proves nothing: the holder reads
// it again while its lease holds, rather than give the authority up.
func TestAuthorityHolderRereadsARecordItCannotRead(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	heartbeat, self := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	self.Epoch = 1
	if err := shared.WriteAuthority(self); err != nil {
		t.Fatal(err)
	}
	holder := AuthorityHolder{Cluster: shared, Heartbeat: heartbeat, Record: self}
	if err := os.Chmod(shared.recordPath(), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := shared.ReadAuthority(); err == nil {
		t.Skip("the record is readable without permission, as it is to root")
	}
	held := make(chan error, 1)
	go func() { held <- holder.Held() }()
	time.Sleep(4 * heldReadRetry)
	select {
	case err := <-held:
		t.Fatalf("the check ended while the record was unreadable: %v", err)
	default:
	}
	if err := os.Chmod(shared.recordPath(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-held; err != nil {
		t.Fatalf("the check failed once the record was readable: %v", err)
	}
}

// A cluster that keeps its records' files open reads what another writes to
// them, and reads on once it closes them.
func TestSharedClusterKeepsFilesOpen(t *testing.T) {
	root := t.TempDir()
	shared := NewSharedCluster(root).KeepFilesOpen()
	other := NewSharedCluster(root)
	if err := shared.WriteAuthority(AuthorityRecord{Epoch: 1, NodeID: "node-a", Incarnation: "i-1"}); err != nil {
		t.Fatal(err)
	}
	for epoch := uint64(1); epoch < 4; epoch++ {
		if epoch > 1 {
			if err := other.WriteAuthority(AuthorityRecord{Epoch: epoch, NodeID: "node-b", Incarnation: "i-1"}); err != nil {
				t.Fatal(err)
			}
		}
		record, ok, err := shared.ReadAuthority()
		if err != nil || !ok || record.Epoch != epoch {
			t.Fatalf("read epoch %d, %t, %v; want %d", record.Epoch, ok, err, epoch)
		}
	}
	if len(shared.files.files) != 2 {
		t.Fatalf("%d files kept open, want the record's, for reading and for writing", len(shared.files.files))
	}
	shared.CloseFiles()
	if record, ok, err := shared.ReadAuthority(); err != nil || !ok || record.Epoch != 3 {
		t.Fatalf("read epoch %d, %t, %v after closing the files; want 3", record.Epoch, ok, err)
	}
	if len(shared.files.files) != 0 {
		t.Fatal("a file was kept open after the files were closed")
	}
}
