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
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func TestAuthorityPlainStreamToken(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &authorityStreamLayer{Listener: listener, advertise: authorityRaftAddress(listener.Addr().String()), authToken: "secret"}
	client := &authorityStreamLayer{authToken: "secret"}
	accepted := make(chan error, 1)
	go func() {
		conn, err := server.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	conn, err := client.Dial(raft.ServerAddress(listener.Addr().String()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("accepting matching token: %v", err)
	}
	_ = listener.Close()

	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server = &authorityStreamLayer{Listener: listener, advertise: authorityRaftAddress(listener.Addr().String()), authToken: "secret"}
	client = &authorityStreamLayer{authToken: "wrong"}
	accepted = make(chan error, 1)
	go func() {
		conn, err := server.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	conn, err = client.Dial(raft.ServerAddress(listener.Addr().String()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-accepted; err == nil || !strings.Contains(err.Error(), "authentication token") {
		t.Fatalf("accepting wrong token error=%v", err)
	}
}

func TestValidateRaftServerAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7444", "node-a@127.0.0.1:7444"} {
		if err := validateRaftServerAddress(address); err != nil {
			t.Errorf("validateRaftServerAddress(%q): %v", address, err)
		}
	}
	for _, address := range []string{"", "127.0.0.1", "@127.0.0.1:7444"} {
		if err := validateRaftServerAddress(address); err == nil {
			t.Errorf("validateRaftServerAddress(%q) unexpectedly succeeded", address)
		}
	}
}

func newAuthorityRaftTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	files := newTestTLSFiles(t)
	server, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	client, err := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	return server, client
}

func waitAuthorityLeader(t *testing.T, nodes ...*AuthorityRaftNode) *AuthorityRaftNode {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, node := range nodes {
			if node.IsLeader() {
				return node
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Raft group did not elect a leader")
	return nil
}

func registerReadyRaftNode(t *testing.T, leader *AuthorityRaftNode, id, incarnation string) {
	t.Helper()
	at := time.Now().UTC()
	node := testAuthorityNode(at)
	node.NodeID, node.Incarnation, node.Capabilities.NodeID = id, incarnation, id
	for _, entry := range []struct {
		id      string
		kind    AuthorityCommandKind
		payload any
	}{
		{"register-state-" + id, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: node, LeaseDuration: time.Hour}},
		{"ready-state-" + id, AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: id, Incarnation: incarnation, State: NodeReady, LeaseDuration: time.Hour}},
	} {
		result, err := leader.Propose(context.Background(), entry.id, entry.kind, entry.payload)
		if err != nil || result.Err() != nil {
			t.Fatalf("applying %s: result=%+v err=%v", entry.kind, result, err)
		}
	}
}

func TestAuthorityFSMReadCopyOmitsReplayHistory(t *testing.T) {
	fsm := NewAuthorityFSM()
	fsm.state.Revision = 7
	fsm.state.Requests["request-1"] = appliedAuthorityRequest{Digest: "digest"}
	fsm.state.RequestOrder = []string{"request-1"}

	state, err := fsm.copyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 7 {
		t.Fatalf("read copy lost authority state: %+v", state)
	}
	if len(state.Requests) != 0 || len(state.RequestOrder) != 0 {
		t.Fatalf("read copy exposed replay history: requests=%d order=%d", len(state.Requests), len(state.RequestOrder))
	}
	if len(fsm.state.Requests) != 1 || len(fsm.state.RequestOrder) != 1 {
		t.Fatal("read copy mutated replicated replay history")
	}
}

func TestAuthorityRaftSingleNodeLinearizableApply(t *testing.T) {
	serverTLS, clientTLS := newAuthorityRaftTLS(t)
	node, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
		NodeID: "node-a", LocalDir: filepath.Join(t.TempDir(), "raft"),
		ListenAddress: "127.0.0.1:0", AdvertiseHost: "127.0.0.1:0", ServerName: "localhost",
		ServerTLS: serverTLS, ClientTLS: clientTLS, Bootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	waitAuthorityLeader(t, node)
	registerReadyRaftNode(t, node, "node-a", "incarnation-a")
	result, err := node.Propose(context.Background(), "placement-create", AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	state, err := node.LinearizableState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	placement := state.Placements[logicalBranchKey("family", "main")]
	if placement.OwnerNode != "node-a" || placement.Generation != 1 {
		t.Fatalf("unexpected replicated placement: %+v", placement)
	}
}

func TestRaftCatalogHeartbeatReturnsLocallyAppliedState(t *testing.T) {
	serverTLS, clientTLS := newAuthorityRaftTLS(t)
	node, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
		NodeID: "node-a", LocalDir: filepath.Join(t.TempDir(), "raft"),
		ListenAddress: "127.0.0.1:0", AdvertiseHost: "127.0.0.1:0", ServerName: "localhost",
		ServerTLS: serverTLS, ClientTLS: clientTLS, Bootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	waitAuthorityLeader(t, node)
	registerReadyRaftNode(t, node, "node-a", "incarnation-a")
	catalog, err := NewRaftCatalog(node)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	record, state, err := catalog.HeartbeatState(ctx, "node-a", "incarnation-a", 1234, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if record.MemoryCurrentBytes != 1234 || record.LeaseTerm == 0 {
		t.Fatalf("unexpected renewed record: %+v", record)
	}
	applied, ok := state.Nodes["node-a"]
	if !ok || applied.Revision != record.Revision || applied.MemoryCurrentBytes != record.MemoryCurrentBytes {
		t.Fatalf("returned state does not include renewal: record=%+v state=%+v", record, applied)
	}
}

func TestAuthorityRaftFollowerRejectsLocalMutation(t *testing.T) {
	var node AuthorityRaftNode
	_, err := node.Apply(context.Background(), AuthorityCommand{})
	var notLeader *AuthorityNotLeaderError
	if !errors.As(err, &notLeader) {
		t.Fatalf("Apply error = %v, want not-leader", err)
	}
}

func TestAuthorityRaftPromotesOnlyReadyLearner(t *testing.T) {
	serverTLS, clientTLS := newAuthorityRaftTLS(t)
	open := func(id string, bootstrap bool) *AuthorityRaftNode {
		t.Helper()
		node, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
			NodeID: id, LocalDir: filepath.Join(t.TempDir(), "raft"),
			ListenAddress: "127.0.0.1:0", AdvertiseHost: "127.0.0.1:0", ServerName: "localhost",
			ServerTLS: serverTLS, ClientTLS: clientTLS, Bootstrap: bootstrap,
		})
		if err != nil {
			t.Fatal(err)
		}
		return node
	}
	a, b := open("node-a", true), open("node-b", false)
	defer a.Close()
	defer b.Close()
	leader := waitAuthorityLeader(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := leader.AddNonvoter(ctx, b.id, b.Address()); err != nil {
		t.Fatal(err)
	}
	if err := leader.ReconcileVoters(ctx, 3); err != nil {
		t.Fatal(err)
	}
	configuration, err := leader.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range configuration.Servers {
		if server.ID == "node-b" && server.Suffrage != raft.Nonvoter {
			t.Fatalf("unregistered learner was promoted: %+v", server)
		}
	}
	at := time.Now().UTC()
	record := testAuthorityNode(at)
	record.NodeID, record.Incarnation, record.Capabilities.NodeID = "node-b", "incarnation-b", "node-b"
	record.RaftAddress = b.Address()
	for _, entry := range []struct {
		id      string
		kind    AuthorityCommandKind
		payload any
	}{
		{"register-ready-learner", AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: time.Hour}},
		{"ready-learner", AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: record.NodeID, Incarnation: record.Incarnation, State: NodeReady, LeaseDuration: time.Hour}},
	} {
		result, err := leader.Propose(ctx, entry.id, entry.kind, entry.payload)
		if err != nil || result.Err() != nil {
			t.Fatalf("%s: result=%+v err=%v", entry.kind, result, err)
		}
	}
	if err := leader.ReconcileVoters(ctx, 3); err != nil {
		t.Fatal(err)
	}
	configuration, err = leader.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range configuration.Servers {
		if server.ID == "node-b" {
			if server.Suffrage != raft.Voter {
				t.Fatalf("ready learner was not promoted: %+v", server)
			}
			return
		}
	}
	t.Fatal("ready learner disappeared from configuration")
}

func TestAuthorityRaftThreeNodeReplicationAndFailover(t *testing.T) {
	serverTLS, clientTLS := newAuthorityRaftTLS(t)
	open := func(id string, bootstrap bool) *AuthorityRaftNode {
		t.Helper()
		node, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
			NodeID: id, LocalDir: filepath.Join(t.TempDir(), "raft"),
			ListenAddress: "127.0.0.1:0", AdvertiseHost: "127.0.0.1:0", ServerName: "localhost",
			ServerTLS: serverTLS, ClientTLS: clientTLS, Bootstrap: bootstrap,
		})
		if err != nil {
			t.Fatal(err)
		}
		return node
	}
	a, b, c := open("node-a", true), open("node-b", false), open("node-c", false)
	closedA := false
	defer func() {
		if !closedA {
			_ = a.Close()
		}
		_ = b.Close()
		_ = c.Close()
	}()
	leader := waitAuthorityLeader(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, follower := range []*AuthorityRaftNode{b, c} {
		if err := leader.AddNonvoter(ctx, follower.id, follower.Address()); err != nil {
			t.Fatalf("adding nonvoter %s: %v", follower.id, err)
		}
		if err := leader.Promote(ctx, follower.id, follower.Address()); err != nil {
			t.Fatalf("promoting %s: %v", follower.id, err)
		}
	}
	registerReadyRaftNode(t, leader, "node-a", "incarnation-a")
	registerReadyRaftNode(t, leader, "node-b", "incarnation-b")
	result, err := leader.Propose(ctx, "replicated-place", AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	if err != nil || result.Err() != nil {
		t.Fatalf("replicated apply: result=%+v err=%v", result, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, err := c.fsm.copyState()
		if err != nil {
			t.Fatal(err)
		}
		if state.Placements[logicalBranchKey("family", "main")].OwnerNode == "node-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower did not apply replicated placement")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	closedA = true
	leader = waitAuthorityLeader(t, b, c)
	result, err = leader.Propose(ctx, "post-failover-place-stale", AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "second", PhysicalContainer: "family", PhysicalBranch: "second",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementPaused,
	}})
	if err != nil || result.Err() == nil {
		t.Fatalf("prior-term lease authorized placement: result=%+v err=%v", result, err)
	}
	result, err = leader.Propose(ctx, "renew-after-failover", AuthorityNodeRenew, AuthorityNodeRenewPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", LeaseDuration: time.Hour,
	})
	if err != nil || result.Err() != nil {
		t.Fatalf("renewing in new term: result=%+v err=%v", result, err)
	}
	result, err = leader.Propose(ctx, "post-failover-place", AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "second", PhysicalContainer: "family", PhysicalBranch: "second",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementPaused,
	}})
	if err != nil || result.Err() != nil {
		t.Fatalf("post-failover apply: result=%+v err=%v", result, err)
	}
}
