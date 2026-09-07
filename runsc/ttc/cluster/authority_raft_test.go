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
	"path/filepath"
	"testing"
	"time"
)

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
	at := time.Now().UTC()
	command, err := NewAuthorityCommand("placement-create", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := node.Apply(context.Background(), command)
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

func TestAuthorityRaftFollowerRejectsLocalMutation(t *testing.T) {
	var node AuthorityRaftNode
	_, err := node.Apply(context.Background(), AuthorityCommand{})
	var notLeader *AuthorityNotLeaderError
	if !errors.As(err, &notLeader) {
		t.Fatalf("Apply error = %v, want not-leader", err)
	}
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
	at := time.Now().UTC()
	command, err := NewAuthorityCommand("replicated-place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := leader.Apply(ctx, command); err != nil || result.Err() != nil {
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
	command, err = NewAuthorityCommand("post-failover-place", time.Now().UTC(), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "second", PhysicalContainer: "family", PhysicalBranch: "second",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementPaused,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := leader.Apply(ctx, command); err != nil || result.Err() != nil {
		t.Fatalf("post-failover apply: result=%+v err=%v", result, err)
	}
}
