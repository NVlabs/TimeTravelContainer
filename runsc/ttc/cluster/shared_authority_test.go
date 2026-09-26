// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
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

func TestFencedLogStoreStopsWhenTheAuthorityIsLost(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	heartbeat, err := StartHeartbeat(shared, MemberRecord{NodeID: "node-a", Incarnation: "i-1", Role: RoleCompute})
	if err != nil {
		t.Fatal(err)
	}
	defer heartbeat.Stop()
	record := AuthorityRecord{Epoch: 3, NodeID: "node-a", Incarnation: "i-1", Role: RoleCompute}
	holder := AuthorityHolder{Cluster: shared, Heartbeat: heartbeat, Record: record}
	store := fencedLogStore{LogStore: raft.NewInmemStore(), held: holder.Held}
	entry := func(index uint64) *raft.Log { return &raft.Log{Index: index, Term: 1, Type: raft.LogCommand} }

	// With no record yet, and then over a stale claimant's, it records its
	// own epoch.
	if err := store.StoreLog(entry(1)); err != nil {
		t.Fatal(err)
	}
	if err := shared.WriteAuthority(AuthorityRecord{Epoch: 2, NodeID: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreLog(entry(2)); err != nil {
		t.Fatal(err)
	}
	if current, _, err := shared.ReadAuthority(); err != nil || current.Epoch != 3 || current.NodeID != "node-a" {
		t.Fatalf("authority record %+v, %v, want epoch 3's restored", current, err)
	}

	// A later epoch takes the authority.
	if err := shared.WriteAuthority(AuthorityRecord{Epoch: 4, NodeID: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreLog(entry(3)); !errors.Is(err, ErrAuthorityLost) {
		t.Fatalf("a write after a later epoch took the authority = %v, want ErrAuthorityLost", err)
	}
	if last, err := store.LastIndex(); err != nil || last != 2 {
		t.Fatalf("last index %d, %v, want 2: the write was not refused before it was made", last, err)
	}

	// Or the lease lapses.
	if err := shared.WriteAuthority(record); err != nil {
		t.Fatal(err)
	}
	heartbeat.Stop()
	heartbeat.mu.Lock()
	heartbeat.landed = time.Now().Add(-LeaseHold)
	heartbeat.mu.Unlock()
	if err := store.StoreLog(entry(3)); !errors.Is(err, ErrAuthorityLost) {
		t.Fatalf("a write after the lease lapsed = %v, want ErrAuthorityLost", err)
	}
}

// openTestEpoch runs the authority of epoch as its one voter.
func openTestEpoch(t *testing.T, shared SharedCluster, epoch uint64, self raft.Server) (*raft.Raft, *AuthorityFSM, func()) {
	t.Helper()
	dir := shared.RaftDir(epoch)
	store, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := raft.NewFileSnapshotStore(dir, authoritySnapshotRetain, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport(self.Address)
	config := authorityRaftConfig(string(self.ID), 50*time.Millisecond)
	fsm := NewAuthorityFSM()
	engine, err := raft.NewRaft(config, fsm, store, store, snapshots, transport)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for engine.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatalf("the voter of epoch %d was not elected", epoch)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return engine, fsm, func() {
		if err := engine.Shutdown().Error(); err != nil {
			t.Error(err)
		}
		_ = transport.Close()
		_ = store.Close()
	}
}

func TestPrepareEpochCarriesTheAuthorityToOneVoter(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	first := raft.Server{ID: "node-a", Address: "node-a:7443", Suffrage: raft.Voter}
	if err := shared.PrepareEpoch("", 1, first); err != nil {
		t.Fatal(err)
	}
	engine, _, closeFirst := openTestEpoch(t, shared, 1, first)
	at := time.Now().UTC()
	node := testAuthorityNode(at)
	command, err := NewAuthorityCommand("register-node-a", at, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: node, LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(data, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	// Some of the state is in a snapshot, and some only in the log.
	if err := engine.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	command, err = NewAuthorityCommand("ready-node-a", at.Add(time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: node.NodeID, Incarnation: node.Incarnation, State: NodeReady, LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if data, err = json.Marshal(command); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(data, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	closeFirst()

	second := raft.Server{ID: "node-b", Address: "node-b:7443", Suffrage: raft.Voter}
	if err := shared.PrepareEpoch(shared.RaftDir(1), 2, second); err != nil {
		t.Fatal(err)
	}
	engine, fsm, closeSecond := openTestEpoch(t, shared, 2, second)
	defer closeSecond()
	state, err := fsm.copyState()
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := state.Nodes["node-a"]; !ok || record.State != NodeReady {
		t.Fatalf("epoch 2's authority has node-a as %+v (%t), want it ready", record, ok)
	}
	future := engine.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatal(err)
	}
	if servers := future.Configuration().Servers; len(servers) != 1 || servers[0].ID != "node-b" || servers[0].Suffrage != raft.Voter {
		t.Fatalf("epoch 2's configuration is %+v, want node-b alone", servers)
	}
	// Epoch 1's state is as its authority left it.
	source, err := raftboltdb.NewBoltStore(filepath.Join(shared.RaftDir(1), "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if last, err := source.LastIndex(); err != nil || last == 0 {
		t.Fatalf("epoch 1's log ends at %d, %v", last, err)
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
			decision, err := shared.DecideAuthority(ctx, self, false, &liveness)
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
	decision, err := shared.DecideAuthority(ctx, a, false, &aLiveness)
	if err != nil || !decision.Hold {
		t.Fatalf("the first daemon decided %+v, %v", decision, err)
	}
	// Restarted, it holds it still.
	if again, err := shared.DecideAuthority(ctx, a, false, &aLiveness); err != nil || !again.Hold || again.Record.Epoch != 1 {
		t.Fatalf("the restarted holder decided %+v, %v", again, err)
	}
	_, b := startTestMember(t, shared, "node-b", "i-1", RoleCompute)
	var bLiveness Liveness
	if joined, err := shared.DecideAuthority(ctx, b, false, &bLiveness); err != nil || joined.Hold || joined.Record.NodeID != "node-a" {
		t.Fatalf("a daemon beside a live holder decided %+v, %v", joined, err)
	}
	// Its counter stands still.
	aHeartbeat.Stop()
	shared.DecideAuthority(ctx, b, false, &bLiveness)
	ageLiveness(&bLiveness)
	taken, err := shared.DecideAuthority(ctx, b, false, &bLiveness)
	if err != nil || !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Previous != 1 || taken.Record.NodeID != "node-b" {
		t.Fatalf("a daemon beside a stopped holder decided %+v, %v", taken, err)
	}
	// The old holder, back, joins the new one.
	back, _ := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	defer back.Stop()
	if rejoined, err := shared.DecideAuthority(ctx, a, false, &aLiveness); err != nil || rejoined.Hold || rejoined.Record.NodeID != "node-b" {
		t.Fatalf("the old holder, back, decided %+v, %v", rejoined, err)
	}
}

func TestDecideAuthorityTakesOverFromAReplacedIncarnation(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	aHeartbeat, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var liveness Liveness
	ctx := context.Background()
	if decision, err := shared.DecideAuthority(ctx, a, false, &liveness); err != nil || !decision.Hold {
		t.Fatalf("decided %+v, %v", decision, err)
	}
	// Its node comes back without its local root, a new incarnation.
	aHeartbeat.Stop()
	_, replaced := startTestMember(t, shared, "node-a", "i-2", RoleCompute)
	taken, err := shared.DecideAuthority(ctx, replaced, false, &liveness)
	if err != nil || !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Incarnation != "i-2" {
		t.Fatalf("the new incarnation decided %+v, %v", taken, err)
	}
}

func TestDecideAuthorityTakesItFromAControlDaemonWhichGivesItUp(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	loginHeartbeat, login := startTestMember(t, shared, "login", "i-1", RoleControl)
	var liveness Liveness
	ctx := context.Background()
	held, err := shared.DecideAuthority(ctx, login, false, &liveness)
	if err != nil || !held.Hold {
		t.Fatalf("the login daemon alone decided %+v, %v", held, err)
	}
	_, compute := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	taken, err := shared.DecideAuthority(ctx, compute, false, &liveness)
	if err != nil || !taken.Hold || taken.Record.Epoch != 2 || taken.Record.Previous != 1 {
		t.Fatalf("a compute daemon beside a control holder decided %+v, %v", taken, err)
	}
	released := make(chan error, 1)
	go func() { released <- shared.AwaitRelease(ctx, held.Record, taken.Record.Epoch, &liveness) }()
	select {
	case err := <-released:
		t.Fatalf("AwaitRelease returned %v before the control daemon gave the authority up", err)
	case <-time.After(3 * HeartbeatInterval / 2):
	}
	if err := loginHeartbeat.Update(func(record *MemberRecord) { record.Released = held.Record.Epoch }, true); err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	// A control daemon does not take it from a compute one.
	if joined, err := shared.DecideAuthority(ctx, login, false, &liveness); err != nil || joined.Hold {
		t.Fatalf("the control daemon decided %+v, %v", joined, err)
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
	if decision, err := shared.DecideAuthority(ctx, b, false, &liveness); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a daemon beside a claim in progress decided %+v, %v, want it to wait", decision, err)
	}
	// The claimant restarted before it recorded the claim: it holds it.
	held, err := shared.DecideAuthority(context.Background(), a, false, &liveness)
	if err != nil || !held.Hold || held.Record.Epoch != 1 {
		t.Fatalf("the claimant decided %+v, %v", held, err)
	}
	if joined, err := shared.DecideAuthority(context.Background(), b, false, &liveness); err != nil || joined.Hold || joined.Record.NodeID != "node-a" {
		t.Fatalf("a daemon beside the recorded claim decided %+v, %v", joined, err)
	}
}

func TestDecideAuthorityKeepsAnOlderClustersRaftState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".ttc-raft-cluster.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shared := NewSharedCluster(root)
	_, a := startTestMember(t, shared, "node-a", "i-1", RoleCompute)
	var liveness Liveness
	if _, err := shared.DecideAuthority(context.Background(), a, false, &liveness); !errors.Is(err, ErrNoAuthorityState) {
		t.Fatalf("a daemon with none of the older cluster's Raft state decided %v, want ErrNoAuthorityState", err)
	}
	held, err := shared.DecideAuthority(context.Background(), a, true, &liveness)
	if err != nil || !held.Hold || held.Record.Epoch != 1 || held.Record.Previous != 0 {
		t.Fatalf("a daemon with some decided %+v, %v", held, err)
	}
	if err := shared.PrepareEpoch("", 1, raft.Server{ID: "node-a", Address: "node-a:1", Suffrage: raft.Voter}); err != nil {
		t.Fatal(err)
	}
	if err := shared.PrepareEpoch("", 1, raft.Server{ID: "node-a", Address: "node-a:1", Suffrage: raft.Voter}); err == nil {
		t.Fatal("made the Raft state of a made epoch again")
	}
}

// The daemon's path: an epoch's holder runs on the network, and the next
// epoch's holder takes its state over.
func TestEpochHoldersTakeOverOnTheNetwork(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	hold := func(node string, epoch, previous uint64) (*AuthorityRaftNode, *Heartbeat) {
		t.Helper()
		heartbeat, self := startTestMember(t, shared, node, "i-1", RoleCompute)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		self.RaftAddress = listener.Addr().String()
		self.Epoch, self.Previous = epoch, previous
		if won, err := shared.ClaimEpoch(self); err != nil || !won {
			t.Fatalf("claiming epoch %d: %t, %v", epoch, won, err)
		}
		if err := shared.WriteAuthority(self); err != nil {
			t.Fatal(err)
		}
		source := ""
		if previous != 0 {
			source = shared.RaftDir(previous)
		}
		if err := shared.PrepareEpochFor(source, epoch, node, self.RaftAddress); err != nil {
			t.Fatal(err)
		}
		holder := AuthorityHolder{Cluster: shared, Heartbeat: heartbeat, Record: self}
		raftNode, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
			NodeID: node, Listener: listener, AdvertiseHost: self.RaftAddress,
			EpochDir: shared.RaftDir(epoch), Holder: holder.Held,
		})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for !raftNode.IsLeader() {
			if time.Now().After(deadline) {
				t.Fatalf("the holder of epoch %d was not elected", epoch)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return raftNode, heartbeat
	}
	first, firstHeartbeat := hold("node-a", 1, 0)
	registerReadyRaftNode(t, first, "node-a", "i-1")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	firstHeartbeat.Stop()
	second, _ := hold("node-b", 2, 1)
	defer second.Close()
	state, err := second.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := state.Nodes["node-a"]; !ok || record.State != NodeReady {
		t.Fatalf("epoch 2 has node-a as %+v (%t)", record, ok)
	}
	// A read takes one barrier a term, and then only asks whether the node
	// still holds the authority.
	if _, err := second.LinearizableState(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := second.engine().raft.LastIndex()
	for range 3 {
		if _, err := second.LinearizableState(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if after := second.engine().raft.LastIndex(); after != before {
		t.Fatalf("reads wrote the log from %d to %d", before, after)
	}
	// Epoch 3's holder would prune what came before epoch 2.
	if err := shared.PruneEpochs(2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shared.RaftDir(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("epoch 1's Raft state remains: %v", err)
	}
	if latest, err := shared.LatestClaim(); err != nil || latest != 2 {
		t.Fatalf("latest claim %d, %v, want 2", latest, err)
	}
	if !shared.Prepared(2) {
		t.Fatal("epoch 2's Raft state was pruned")
	}
}
