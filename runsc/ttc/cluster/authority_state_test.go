// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func applyAuthorityForTest(t *testing.T, state *AuthorityState, id string, at time.Time, kind AuthorityCommandKind, payload any) AuthorityResult {
	t.Helper()
	command, err := NewAuthorityCommand(id, at, kind, payload)
	if err != nil {
		t.Fatalf("NewAuthorityCommand: %v", err)
	}
	result := state.Apply(command)
	if err := result.Err(); err != nil {
		t.Fatalf("Apply(%s): %v", kind, err)
	}
	return result
}

func decodeAuthorityResultForTest[T any](t *testing.T, result AuthorityResult) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.Value, &value); err != nil {
		t.Fatalf("decoding authority result: %v", err)
	}
	return value
}

func TestAuthorityTransactionPayloadJSONKeepsFailureAndDigestDistinct(t *testing.T) {
	want := AuthorityTransactionPayload{
		Failure:       "restore failed",
		Digest:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DesiredPaused: true,
		Activated:     true,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got AuthorityTransactionPayload
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Failure != want.Failure || got.Digest != want.Digest || got.DesiredPaused != want.DesiredPaused || got.Activated != want.Activated {
		t.Fatalf("JSON round trip got %+v, want %+v (encoded %s)", got, want, encoded)
	}
}

func testAuthorityNode(at time.Time) NodeRecord {
	capabilities := testCapabilities("node-a")
	return NodeRecord{
		NodeID: "node-a", Incarnation: "incarnation-a", ControlURL: "https://node-a:7443",
		ControlServerName: "node-a", Capabilities: capabilities, MemoryTargetBytes: 8 << 30,
		StartedAt: at,
	}
}

func registerReadyAuthorityNode(t *testing.T, state *AuthorityState, id, incarnation string, at time.Time) {
	t.Helper()
	node := testAuthorityNode(at)
	node.NodeID, node.Incarnation, node.Capabilities.NodeID = id, incarnation, id
	registered := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "register-"+id+"-"+incarnation, at, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: node, LeaseDuration: time.Hour}))
	applyAuthorityForTest(t, state, "ready-"+id+"-"+incarnation, at.Add(time.Millisecond), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: registered.NodeID, Incarnation: registered.Incarnation, State: NodeReady, LeaseDuration: time.Hour,
	})
}

func TestAuthorityNodeRenewDerivesPressureStateAtomically(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)

	pressured := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "renew-pressured", at.Add(2*time.Millisecond), AuthorityNodeRenew, AuthorityNodeRenewPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", MemoryCurrentBytes: 9 << 30, LeaseDuration: time.Hour,
	}))
	if pressured.State != NodePressured {
		t.Fatalf("renewed state got %q, want %q", pressured.State, NodePressured)
	}

	// Pressure uses 10% hysteresis, so merely returning below the target does
	// not flap the node back to ready.
	stillPressured := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "renew-hysteresis", at.Add(3*time.Millisecond), AuthorityNodeRenew, AuthorityNodeRenewPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", MemoryCurrentBytes: 15 << 29, LeaseDuration: time.Hour,
	}))
	if stillPressured.State != NodePressured {
		t.Fatalf("state inside hysteresis got %q, want %q", stillPressured.State, NodePressured)
	}

	ready := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "renew-ready", at.Add(4*time.Millisecond), AuthorityNodeRenew, AuthorityNodeRenewPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", MemoryCurrentBytes: 7 << 30, LeaseDuration: time.Hour,
	}))
	if ready.State != NodeReady {
		t.Fatalf("state below hysteresis got %q, want %q", ready.State, NodeReady)
	}
}

func TestAuthorityCommandIdempotencyAndInputReuse(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	state := NewAuthorityState()
	payload := AuthorityNodeRegisterPayload{Record: testAuthorityNode(at), LeaseUntil: at.Add(15 * time.Second)}
	command, err := NewAuthorityCommand("register-a", at, AuthorityNodeRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	first := state.Apply(command)
	if err := first.Err(); err != nil {
		t.Fatal(err)
	}
	second := state.Apply(command)
	if first.Revision != second.Revision || state.Revision != first.Revision {
		t.Fatalf("idempotent retry advanced revision: first=%d second=%d state=%d", first.Revision, second.Revision, state.Revision)
	}
	reenveloped := command
	reenveloped.IssuedAt = command.IssuedAt.Add(time.Minute)
	reenveloped.Epoch++
	third := state.Apply(reenveloped)
	if first.Revision != third.Revision || state.Revision != first.Revision {
		t.Fatalf("re-enveloped retry advanced revision: first=%d third=%d state=%d", first.Revision, third.Revision, state.Revision)
	}
	payload.LeaseUntil = at.Add(30 * time.Second)
	changed, _ := NewAuthorityCommand("register-a", at, AuthorityNodeRegister, payload)
	result := state.Apply(changed)
	if result.Err() == nil {
		t.Fatal("request ID reuse with different input succeeded")
	}
	if state.Revision != first.Revision {
		t.Fatalf("conflicting retry advanced revision to %d", state.Revision)
	}
}

func TestAuthoritySnapshotOriginAllocationAndPublish(t *testing.T) {
	at := time.Unix(1500, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	payload := AuthoritySnapshotPayload{NodeID: "node-a", Incarnation: "incarnation-a", Count: 3}
	command, err := NewAuthorityCommand("allocate-snapshots", at, AuthoritySnapshotAllocate, payload)
	if err != nil {
		t.Fatal(err)
	}
	first := state.Apply(command)
	if err := first.Err(); err != nil {
		t.Fatal(err)
	}
	allocation := decodeAuthorityResultForTest[SnapshotAllocation](t, first)
	if allocation.First != 1 || allocation.Count != 3 || state.NextSnapshotID != 4 {
		t.Fatalf("allocation=%+v next=%d", allocation, state.NextSnapshotID)
	}
	if retry := state.Apply(command); retry.Revision != first.Revision || state.NextSnapshotID != 4 {
		t.Fatalf("idempotent allocation changed state: first=%+v retry=%+v next=%d", first, retry, state.NextSnapshotID)
	}
	for id := uint64(1); id <= 3; id++ {
		if provider := state.VolatileOrigins[id]; provider.SnapshotID != id || provider.NodeID != "node-a" {
			t.Fatalf("provider %d=%+v", id, provider)
		}
	}
	applyAuthorityForTest(t, state, "publish-snapshot-2", at.Add(time.Second), AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", SnapshotID: 2,
	})
	if _, ok := state.VolatileOrigins[2]; ok {
		t.Fatal("published snapshot retained a volatile provider")
	}
	if provider := state.VolatileOrigins[1]; provider.NodeID != "node-a" {
		t.Fatal("publishing one origin changed another")
	}
	abandon := AuthoritySnapshotPayload{NodeID: "node-a", Incarnation: "incarnation-a", SnapshotID: 3}
	applyAuthorityForTest(t, state, "abandon-snapshot-3", at.Add(2*time.Second), AuthoritySnapshotAbandon, abandon)
	applyAuthorityForTest(t, state, "abandon-snapshot-3-retry", at.Add(3*time.Second), AuthoritySnapshotAbandon, abandon)
	if _, ok := state.VolatileOrigins[3]; ok {
		t.Fatal("abandoned snapshot retained a volatile provider")
	}
	if !state.AbandonedOrigins[3] {
		t.Fatal("abandoned snapshot has no permanent authority tombstone")
	}
	publishAbandoned, err := NewAuthorityCommand("publish-abandoned-3", at.Add(4*time.Second), AuthoritySnapshotPublish, abandon)
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(publishAbandoned); result.Err() == nil {
		t.Fatal("published an abandoned snapshot origin")
	}
	abandonPublished, err := NewAuthorityCommand("abandon-published-2", at.Add(5*time.Second), AuthoritySnapshotAbandon, AuthoritySnapshotPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", SnapshotID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(abandonPublished); result.Err() == nil {
		t.Fatal("abandoned a published snapshot origin")
	}
	if state.NextSnapshotID != 4 {
		t.Fatalf("abandonment reused or rewound snapshot IDs: next=%d", state.NextSnapshotID)
	}
}

func TestAuthoritySharedGCLeaseFencesGenerations(t *testing.T) {
	at := time.Unix(1700, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at.Add(-time.Second))

	first := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "gc-acquire-a", at, AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Lease: 10 * time.Second,
	}))
	if first.Generation != 1 || first.NodeID != "node-a" || !first.ExpiresAt.Equal(at.Add(10*time.Second)) {
		t.Fatalf("first lease = %+v", first)
	}
	contender, err := NewAuthorityCommand("gc-acquire-b-early", at.Add(time.Second), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", Lease: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(contender); result.Err() == nil {
		t.Fatal("contender acquired an unexpired lease")
	}
	renewed := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "gc-renew-a", at.Add(2*time.Second), AuthoritySharedGCRenew, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Generation: first.Generation, Lease: 12 * time.Second,
	}))
	if renewed.Generation != first.Generation || !renewed.ExpiresAt.Equal(at.Add(14*time.Second)) {
		t.Fatalf("renewed lease = %+v", renewed)
	}
	second := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "gc-acquire-b-expired", at.Add(14*time.Second), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", Lease: 10 * time.Second,
	}))
	if second.Generation != first.Generation+1 || second.NodeID != "node-b" {
		t.Fatalf("successor lease = %+v", second)
	}
	staleRelease, err := NewAuthorityCommand("gc-release-a-stale", at.Add(15*time.Second), AuthoritySharedGCRelease, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Generation: first.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(staleRelease); result.Err() == nil {
		t.Fatal("stale owner released successor lease")
	}
	allocation := decodeAuthorityResultForTest[SnapshotAllocation](t, applyAuthorityForTest(t, state, "gc-allocate-origin", at.Add(15*time.Second), AuthoritySnapshotAllocate, AuthoritySnapshotPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", Count: 1,
	}))
	applyAuthorityForTest(t, state, "gc-publish-origin", at.Add(15*time.Second), AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", SnapshotID: allocation.First,
	})
	retirement := decodeAuthorityResultForTest[SharedPackRetirement](t, applyAuthorityForTest(t, state, "gc-retire-origin", at.Add(15*time.Second), AuthoritySharedGCRetire, AuthoritySharedGCPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", Generation: second.Generation, SnapshotID: allocation.First,
	}))
	if retirement.SnapshotID != allocation.First || retirement.GCGeneration != second.Generation {
		t.Fatalf("retirement = %+v", retirement)
	}
	latePublish, err := NewAuthorityCommand("gc-late-publish", at.Add(15*time.Second), AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", SnapshotID: allocation.First,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(latePublish); result.Err() == nil {
		t.Fatal("late publisher resurrected a retired origin")
	}
	applyAuthorityForTest(t, state, "gc-release-b", at.Add(16*time.Second), AuthoritySharedGCRelease, AuthoritySharedGCPayload{
		NodeID: "node-b", Incarnation: "incarnation-b", Generation: second.Generation,
	})
	if state.SharedGC.NodeID != "" || state.NextSharedGCGeneration != 3 {
		t.Fatalf("released state = %+v, next=%d", state.SharedGC, state.NextSharedGCGeneration)
	}
}

func TestAuthoritySharedGCRetirementRejectsControlPlaneRoots(t *testing.T) {
	at := time.Unix(1750, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	allocation := decodeAuthorityResultForTest[SnapshotAllocation](t, applyAuthorityForTest(t, state, "allocate", at, AuthoritySnapshotAllocate, AuthoritySnapshotPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Count: 1,
	}))
	applyAuthorityForTest(t, state, "publish", at, AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", SnapshotID: allocation.First,
	})
	lease := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "lease", at, AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Lease: 10 * time.Second,
	}))
	branchKey := logicalBranchKey("family", "main")
	state.Branches[branchKey] = BranchAuthority{FamilyID: "family", BranchID: "main", Generation: 1, Roots: DurableRoots{MemorySnapshot: allocation.First}}
	retire := func(id string) AuthorityResult {
		command, err := NewAuthorityCommand(id, at.Add(time.Second), AuthoritySharedGCRetire, AuthoritySharedGCPayload{
			NodeID: "node-a", Incarnation: "incarnation-a", Generation: lease.Generation, SnapshotID: allocation.First,
		})
		if err != nil {
			t.Fatal(err)
		}
		return state.Apply(command)
	}
	if err := retire("retire-branch-root").Err(); err == nil {
		t.Fatal("retired snapshot still selected by a branch root")
	}
	delete(state.Branches, branchKey)
	state.Publications["family/main/1"] = PublicationRecord{
		FamilyID: "family", BranchID: "main", State: PublicationReady,
		CandidateRoots: DurableRoots{MemorySnapshot: allocation.First},
	}
	if err := retire("retire-publication-root").Err(); err == nil {
		t.Fatal("retired snapshot still selected by an in-flight publication")
	}
	delete(state.Publications, "family/main/1")
	if err := retire("retire-unreferenced").Err(); err != nil {
		t.Fatalf("retiring unreferenced snapshot: %v", err)
	}
}

func TestAuthorityNodeReincarnationRevokesSharedGCLease(t *testing.T) {
	at := time.Unix(1800, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-2*time.Hour))
	lease := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "gc-acquire-old", at.Add(-90*time.Minute), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Lease: 10 * time.Second,
	}))
	if lease.NodeID == "" {
		t.Fatal("old incarnation did not acquire shared GC lease")
	}
	replacement := testAuthorityNode(at)
	replacement.NodeID, replacement.Incarnation, replacement.Capabilities.NodeID = "node-a", "incarnation-new", "node-a"
	applyAuthorityForTest(t, state, "register-new-incarnation", at, AuthorityNodeRegister, AuthorityNodeRegisterPayload{
		Record: replacement, LeaseDuration: time.Hour,
	})
	if state.SharedGC != (SharedGCLease{}) {
		t.Fatalf("reincarnation retained stale shared GC lease: %+v", state.SharedGC)
	}
	if _, err := RestoreAuthorityState(mustAuthoritySnapshotForTest(t, state)); err != nil {
		t.Fatalf("state after reincarnation is not restorable: %v", err)
	}
}

func TestAuthorityCleanNodeRetirementRevokesSharedGCLease(t *testing.T) {
	at := time.Unix(1850, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	lease := decodeAuthorityResultForTest[SharedGCLease](t, applyAuthorityForTest(t, state, "gc-acquire", at.Add(time.Second), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Lease: 10 * time.Second,
	}))
	if lease.NodeID == "" {
		t.Fatal("node did not acquire shared GC lease")
	}
	applyAuthorityForTest(t, state, "draining", at.Add(2*time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeDraining, LeaseDuration: time.Hour,
	})
	applyAuthorityForTest(t, state, "offline", at.Add(3*time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeOffline,
	})
	if state.SharedGC != (SharedGCLease{}) {
		t.Fatalf("clean retirement retained stale shared GC lease: %+v", state.SharedGC)
	}
	if state.NextSharedGCGeneration != lease.Generation+1 {
		t.Fatalf("clean retirement changed generation sequence: next=%d, lease=%d", state.NextSharedGCGeneration, lease.Generation)
	}
}

func TestAuthorityCleanNodeRetirementRemovesDiscoveryRecord(t *testing.T) {
	at := time.Unix(1875, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	applyAuthorityForTest(t, state, "draining", at.Add(time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeDraining, LeaseDuration: time.Hour,
	})
	applyAuthorityForTest(t, state, "offline", at.Add(2*time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeOffline,
	})
	record := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "remove", at.Add(3*time.Second), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: record.NodeID, Incarnation: record.Incarnation, Revision: record.Revision,
	})
	if _, ok := state.Nodes["node-a"]; ok {
		t.Fatal("cleanly retired node remains in discovery state")
	}
	if got := state.RetiredNodes["node-a"]; got != "incarnation-a" {
		t.Fatalf("retired-node fence=%q, want incarnation-a", got)
	}
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-b", at.Add(4*time.Second))
	if _, ok := state.RetiredNodes["node-a"]; ok {
		t.Fatal("new node incarnation retained obsolete retirement fence")
	}
}

func TestAuthorityClusterPolicyChangesOnlyWhenFullyStopped(t *testing.T) {
	at := time.Unix(1880, 0).UTC()
	state := NewAuthorityState()
	policy := ClusterRuntimePolicy{
		Version: 1, PagingPolicy: testCapabilities("node-a").PagingPolicy,
		LeaseDuration: 15 * time.Second, MaxTransactions: 32, AutoRebalance: true,
	}
	applyAuthorityForTest(t, state, "policy-initial", at, AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{Policy: policy})
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(time.Second))
	changed := policy
	changed.AutoRebalance = false
	command, err := NewAuthorityCommand("policy-live", at.Add(2*time.Second), AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{
		ExpectedRevision: state.ClusterPolicy.Revision, Policy: changed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(command).Err(); err == nil || !strings.Contains(err.Error(), "live execution lease") {
		t.Fatalf("live policy change error=%v", err)
	}
	applyAuthorityForTest(t, state, "draining", at.Add(3*time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeDraining, LeaseDuration: time.Hour,
	})
	applyAuthorityForTest(t, state, "offline", at.Add(4*time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeOffline,
	})
	updated := decodeAuthorityResultForTest[ClusterRuntimePolicy](t, applyAuthorityForTest(t, state, "policy-stopped", at.Add(5*time.Second), AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{
		ExpectedRevision: state.ClusterPolicy.Revision, Policy: changed,
	}))
	if updated.AutoRebalance || updated.Revision != 2 {
		t.Fatalf("updated policy=%+v", updated)
	}
}

func mustAuthoritySnapshotForTest(t *testing.T, state *AuthorityState) []byte {
	t.Helper()
	data, err := state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAuthorityMigrationCutoverFencesOldOwner(t *testing.T) {
	at := time.Unix(2000, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	quarantined := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "quarantine", at.Add(500*time.Millisecond), AuthorityPlacementQuarantine, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a", Generation: created.Generation, Quarantined: true,
	}))
	if !quarantined.Quarantined || quarantined.Generation != created.Generation {
		t.Fatalf("quarantine transition=%+v", quarantined)
	}
	begin := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "begin", at.Add(time.Second), AuthorityPlacementBegin, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a", Generation: created.Generation, Operation: "migration-1", Target: "node-b",
	}))
	state.Transactions["migration-1"] = Transaction{
		Identity: ForkIdentity{
			TransactionID: "migration-1", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "family",
			TargetBranch: "physical-main", TargetVisibleBranch: "main", Migration: true,
		},
		State: TransactionPublished, Revision: 6,
		TargetPhysicalContainer: "physical-main", TargetPhysicalBranch: "main", TargetPhysicalIncarnation: "incarnation-b",
	}
	cutover := decodeAuthorityResultForTest[MigrationCutoverResult](t, applyAuthorityForTest(t, state, "cutover", at.Add(2*time.Second), AuthorityMigrationCutover, AuthorityMigrationCutoverPayload{
		TransactionID: "migration-1", TransactionRevision: 6,
		Family: "family", Branch: "main", Operation: "migration-1", Target: "node-b",
		TargetIncarnation: "incarnation-b", PhysicalContainer: "physical-main", PhysicalBranch: "main",
	}))
	committed := cutover.Placement
	if committed.OwnerNode != "node-b" || committed.Generation != begin.Generation+1 || committed.State != PlacementRunning || !committed.Quarantined {
		t.Fatalf("bad atomic migration cutover: %+v", cutover)
	}
	stale, _ := NewAuthorityCommand("stale-state", at.Add(3*time.Second), AuthorityPlacementStableState, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a", Generation: begin.Generation, State: PlacementPaused,
	})
	if result := state.Apply(stale); result.Err() == nil {
		t.Fatal("old placement owner mutated committed target")
	}
	got := state.Placements[logicalBranchKey("family", "main")]
	if got.OwnerNode != "node-b" || got.Generation != committed.Generation || got.State != PlacementRunning {
		t.Fatalf("stale mutation changed placement: %+v", got)
	}
}

func TestAuthorityPlacementRenameMovesLogicalStateAndReleasesOldName(t *testing.T) {
	at := time.Unix(2250, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "runtime-a", PhysicalBranch: "child",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	applyAuthorityForTest(t, state, "place-grandchild", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "grandchild", ParentBranch: "child", PhysicalContainer: "runtime-a", PhysicalBranch: "grandchild",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}})
	oldKey := logicalBranchKey("family", "child")
	branch := state.Branches[oldKey]
	branch.PublicationID = "publication-1"
	state.Branches[oldKey] = branch
	state.Publications[publicationKey("family", "child", "publication-1")] = PublicationRecord{
		FamilyID: "family", BranchID: "child", BranchGeneration: created.Generation,
		PublicationID: "publication-1", Lifecycle: "dehydrated", State: PublicationCommitted,
		Revision: 3, UpdatedAt: at,
	}

	renamed := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "rename", at.Add(time.Second), AuthorityPlacementRename, AuthorityPlacementPayload{
		Family: "family", Branch: "child", NewBranch: "renamed", Owner: "node-a", Incarnation: "incarnation-a",
		PhysicalBranch: "renamed", Generation: created.Generation,
	}))
	if renamed.BranchID != "renamed" || renamed.PhysicalBranch != "renamed" || renamed.Generation != created.Generation+1 {
		t.Fatalf("renamed placement=%+v", renamed)
	}
	if _, ok := state.Placements[oldKey]; ok {
		t.Fatal("old placement key remains after rename")
	}
	newKey := logicalBranchKey("family", "renamed")
	if authority := state.Branches[newKey]; authority.BranchID != "renamed" || authority.Generation != renamed.Generation {
		t.Fatalf("renamed branch authority=%+v", authority)
	}
	if child := state.Placements[logicalBranchKey("family", "grandchild")]; child.ParentBranch != "renamed" {
		t.Fatalf("child parent after rename=%q, want renamed", child.ParentBranch)
	}
	if _, ok := state.Publications[publicationKey("family", "child", "publication-1")]; ok {
		t.Fatal("old publication key remains after rename")
	}
	if publication := state.Publications[publicationKey("family", "renamed", "publication-1")]; publication.BranchID != "renamed" {
		t.Fatalf("renamed publication=%+v", publication)
	}
	// The visible name is not an immutable lineage identity. It is immediately
	// reusable by another branch once the atomic rename commits.
	reused := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "reuse", at.Add(2*time.Second), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "runtime-a", PhysicalBranch: "child",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	if reused.BranchID != "child" {
		t.Fatalf("reused placement=%+v", reused)
	}
	mustAuthoritySnapshotForTest(t, state)
}

func TestAuthorityMigrationTransactionBindsPhysicalSource(t *testing.T) {
	at := time.Unix(2500, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "source", "incarnation-a", at.Add(-time.Second))
	registerReadyAuthorityNode(t, state, "target", "incarnation-b", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "app", BranchID: "main", PhysicalContainer: "runtime-a", PhysicalBranch: "migration-old",
		OwnerNode: "source", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	applyAuthorityForTest(t, state, "begin", at.Add(time.Second), AuthorityPlacementBegin, AuthorityPlacementPayload{
		Family: "app", Branch: "main", Owner: "source", Incarnation: "incarnation-a", Generation: created.Generation, Operation: "migration-next", Target: "target",
	})
	identity := testForkIdentity()
	identity.TransactionID = "migration-next"
	identity.Migration = true
	identity.TargetContainer = identity.SourceContainer
	identity.TargetBranch = "migration-next"
	identity.TargetVisibleBranch = identity.SourceBranch
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := NewTransaction(identity, negotiated, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	createdTx := decodeAuthorityResultForTest[Transaction](t, applyAuthorityForTest(t, state, "transaction", at.Add(time.Second), AuthorityTransactionCreate, AuthorityTransactionPayload{Transaction: *tx}))
	if createdTx.SourcePhysicalContainer != "runtime-a" || createdTx.SourcePhysicalBranch != "migration-old" {
		t.Fatalf("transaction physical source = %s@%s, want runtime-a@migration-old", createdTx.SourcePhysicalContainer, createdTx.SourcePhysicalBranch)
	}
}

func TestAuthorityForkTransactionRetainsReportedPhysicalSource(t *testing.T) {
	at := time.Unix(2600, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "source", "incarnation-a", at.Add(-time.Second))
	registerReadyAuthorityNode(t, state, "target", "incarnation-b", at.Add(-time.Second))
	identity := testForkIdentity()
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := NewTransaction(identity, negotiated, at)
	if err != nil {
		t.Fatal(err)
	}
	tx.SourcePhysicalContainer = "cold-cache"
	tx.SourcePhysicalBranch = "cold-branch"
	created := decodeAuthorityResultForTest[Transaction](t, applyAuthorityForTest(t, state, "transaction", at, AuthorityTransactionCreate, AuthorityTransactionPayload{Transaction: *tx}))
	if created.SourcePhysicalContainer != "cold-cache" || created.SourcePhysicalBranch != "cold-branch" {
		t.Fatalf("transaction physical source = %s@%s, want cold-cache@cold-branch", created.SourcePhysicalContainer, created.SourcePhysicalBranch)
	}
	mustAuthoritySnapshotForTest(t, state)
}

func TestAuthorityPublicationIdentityPreventsLateResurrection(t *testing.T) {
	at := time.Unix(3000, 0).UTC()
	state := NewAuthorityState()
	state.NextSnapshotID = 8
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", Lifecycle: "dehydrated", BranchGeneration: created.Generation,
	}))
	conflicting, _ := NewAuthorityCommand("publish-conflict", at.Add(1500*time.Millisecond), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-2", Lifecycle: "dehydrated", BranchGeneration: created.Generation,
	})
	if result := state.Apply(conflicting); result.Err() == nil {
		t.Fatal("a second publication bypassed the active immutable identity fence")
	}
	ready := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-ready", at.Add(2*time.Second), AuthorityPublicationReady, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", BranchGeneration: created.Generation, Revision: begin.Revision,
		Roots: DurableRoots{MemorySnapshot: 7},
	}))
	if ready.CandidateRoots.MemorySnapshot != 7 {
		t.Fatalf("capture-ready publication did not fence snapshot 7: %+v", ready)
	}
	branchKey := logicalBranchKey("family", "main")
	withRoots := state.Branches[branchKey]
	withRoots.Roots = DurableRoots{MemorySnapshot: 7, FilesystemRoot: "snapshots/7/filesystem.manifest"}
	withRoots.VolatileDependencies = []VolatileDependency{{TransactionID: "migration-1", SourceNode: "node-a", SourceIncarnation: "incarnation-a"}}
	state.Branches[branchKey] = withRoots
	applyAuthorityForTest(t, state, "corrupt", at.Add(3*time.Second), AuthorityBranchCorrupt, AuthorityPublicationPayload{
		Family: "family", Branch: "main", BranchGeneration: created.Generation, Failure: "authoritative node-local state was lost",
	})
	late, _ := NewAuthorityCommand("publish-late", at.Add(4*time.Second), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", BranchGeneration: created.Generation, Revision: ready.Revision,
		Roots: DurableRoots{MemoryRoot: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if result := state.Apply(late); result.Err() == nil {
		t.Fatal("late publisher resurrected corrupted branch")
	}
	branch := state.Branches[branchKey]
	if !branch.Corrupted || branch.Roots != (DurableRoots{}) || len(branch.VolatileDependencies) != 0 {
		t.Fatalf("corrupted branch was changed: %+v", branch)
	}
	if placement := state.Placements[logicalBranchKey("family", "main")]; placement.State != PlacementCorrupted {
		t.Fatalf("corrupted branch retained placement state %+v", placement)
	}
}

func TestAuthorityCorruptionSettlesVolatileTransactionClosure(t *testing.T) {
	at := time.Unix(3200, 0).UTC()
	newState := func() *AuthorityState {
		state := NewAuthorityState()
		state.Transactions["fork-1"] = Transaction{
			Identity: ForkIdentity{
				TransactionID: "fork-1", SourceContainer: "source", SourceBranch: "main",
				TargetContainer: "target", TargetBranch: "hidden", TargetVisibleBranch: "main",
			},
			State: TransactionPublished, SourceNeeded: true, Revision: 7,
			CreatedAt: at, UpdatedAt: at,
		}
		for _, identity := range [][2]string{{"source", "main"}, {"target", "main"}, {"target", "child"}} {
			key := logicalBranchKey(identity[0], identity[1])
			state.Branches[key] = BranchAuthority{
				FamilyID: identity[0], BranchID: identity[1], Generation: 1,
				Lifecycle: "running", UpdatedAt: at,
			}
			state.Placements[key] = PlacementRecord{
				FamilyID: identity[0], BranchID: identity[1], Generation: 1,
				State: PlacementRunning, Revision: 1, UpdatedAt: at,
			}
		}
		dependency := VolatileDependency{TransactionID: "fork-1", SourceNode: "source-node", SourceIncarnation: "source-incarnation"}
		main := state.Branches[logicalBranchKey("target", "main")]
		main.VolatileDependencies = []VolatileDependency{dependency}
		state.Branches[logicalBranchKey("target", "main")] = main
		child := state.Branches[logicalBranchKey("target", "child")]
		child.VolatileDependencies = []VolatileDependency{dependency}
		state.Branches[logicalBranchKey("target", "child")] = child
		return state
	}

	t.Run("target keeps transaction for healthy descendant", func(t *testing.T) {
		state := newState()
		applyAuthorityForTest(t, state, "corrupt-target", at.Add(time.Second), AuthorityBranchCorrupt, AuthorityPublicationPayload{
			Family: "target", Branch: "main", BranchGeneration: 1, Failure: "target runtime exited",
		})
		if tx := state.Transactions["fork-1"]; tx.State != TransactionPublished || !tx.SourceNeeded {
			t.Fatalf("healthy descendant lost source transaction: %+v", tx)
		}
		if child := state.Branches[logicalBranchKey("target", "child")]; child.Corrupted {
			t.Fatalf("healthy descendant was corrupted: %+v", child)
		}
		applyAuthorityForTest(t, state, "corrupt-child", at.Add(2*time.Second), AuthorityBranchCorrupt, AuthorityPublicationPayload{
			Family: "target", Branch: "child", BranchGeneration: 1, Failure: "target family exited",
		})
		if tx := state.Transactions["fork-1"]; tx.State != TransactionFailed || tx.SourceNeeded {
			t.Fatalf("unreferenced target transaction remained live: %+v", tx)
		}
	})

	t.Run("source loss corrupts exact dependent closure", func(t *testing.T) {
		state := newState()
		applyAuthorityForTest(t, state, "corrupt-source", at.Add(time.Second), AuthorityBranchCorrupt, AuthorityPublicationPayload{
			Family: "source", Branch: "main", BranchGeneration: 1, Failure: "source runtime exited",
		})
		for _, key := range []string{logicalBranchKey("source", "main"), logicalBranchKey("target", "main"), logicalBranchKey("target", "child")} {
			if branch := state.Branches[key]; !branch.Corrupted {
				t.Fatalf("dependency closure retained branch %q: %+v", key, branch)
			}
		}
		if tx := state.Transactions["fork-1"]; tx.State != TransactionFailed || tx.SourceNeeded {
			t.Fatalf("source-loss transaction remained live: %+v", tx)
		}
	})
}

func TestRestoreNormalizesLegacyCorruptedRoots(t *testing.T) {
	at := time.Unix(3250, 0).UTC()
	state := NewAuthorityState()
	state.NextSnapshotID = 8
	state.NextSharedGCGeneration = 2
	state.RetiredOrigins[7] = SharedPackRetirement{SnapshotID: 7, GCGeneration: 1, RetiredAt: at}
	key := logicalBranchKey("family", "main")
	state.Branches[key] = BranchAuthority{
		FamilyID: "family", BranchID: "main", Generation: 2, Lifecycle: "corrupted",
		Corrupted: true, CorruptionReason: "provider lost", UpdatedAt: at,
		Roots:                DurableRoots{MemorySnapshot: 7, FilesystemRoot: "snapshots/7/filesystem.manifest"},
		VolatileDependencies: []VolatileDependency{{TransactionID: "old-transaction", SourceNode: "old-node", SourceIncarnation: "old-incarnation"}},
	}
	data, err := state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreAuthorityState(data)
	if err != nil {
		t.Fatalf("RestoreAuthorityState rejected removable corrupted metadata: %v", err)
	}
	branch := restored.Branches[key]
	if branch.Roots != (DurableRoots{}) || len(branch.VolatileDependencies) != 0 {
		t.Fatalf("legacy corrupted references survived restore: %+v", branch)
	}
}

func TestAuthorityPublicationUnplacesHealthyBranch(t *testing.T) {
	at := time.Unix(3500, 0).UTC()
	state := NewAuthorityState()
	state.NextSnapshotID = 8
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	state.VolatileOrigins[6] = SnapshotOriginProvider{SnapshotID: 6, NodeID: "node-a"}
	state.VolatileOrigins[7] = SnapshotOriginProvider{SnapshotID: 7, NodeID: "node-a"}
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", Lifecycle: "exited", BranchGeneration: created.Generation,
	}))
	if begin.PreviousLifecycle != "paused" || state.Branches[logicalBranchKey("family", "main")].Lifecycle != "exited" {
		t.Fatalf("publication begin did not expose terminal transition: record=%+v branch=%+v", begin, state.Branches[logicalBranchKey("family", "main")])
	}
	ready := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-ready", at.Add(2*time.Second), AuthorityPublicationReady, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", BranchGeneration: created.Generation, Revision: begin.Revision,
		Roots: DurableRoots{MemorySnapshot: 7},
	}))
	applyAuthorityForTest(t, state, "publish-commit", at.Add(3*time.Second), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", BranchGeneration: created.Generation, Revision: ready.Revision,
		Roots:        DurableRoots{MemorySnapshot: 7, FilesystemRoot: "cold/manifests/family/main/0000000000000001.json"},
		ProviderNode: "node-a", ProviderIncarnation: "incarnation-a", SnapshotOrigins: []uint64{6, 7},
	})
	placement := state.Placements[logicalBranchKey("family", "main")]
	if placement.State != PlacementUnplaced {
		t.Fatalf("durable healthy branch placement = %+v, want unplaced", placement)
	}
	branch := state.Branches[logicalBranchKey("family", "main")]
	if branch.Corrupted || branch.Lifecycle != "exited" || branch.Roots.MemorySnapshot != 7 {
		t.Fatalf("durable healthy branch authority = %+v", branch)
	}
	if _, ok := state.VolatileOrigins[6]; ok {
		t.Fatal("filesystem origin remained volatile after atomic publication")
	}
	if _, ok := state.VolatileOrigins[7]; ok {
		t.Fatal("memory origin remained volatile after atomic publication")
	}
	claimed := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "claim-publication", at.Add(4*time.Second), AuthorityPlacementClaim, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a",
		PhysicalContainer: "family", PhysicalBranch: "main", Generation: placement.Generation, State: PlacementMaterializing,
	}))
	materializing := state.Branches[logicalBranchKey("family", "main")]
	if materializing.PublicationID != "" || materializing.Roots.MemorySnapshot != 7 {
		t.Fatalf("materializing branch lost its retry roots or publication fence: %+v", materializing)
	}
	applyAuthorityForTest(t, state, "stabilize-publication", at.Add(5*time.Second), AuthorityPlacementStableState, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a", Generation: claimed.Generation, State: PlacementRunning,
	})
	active := state.Branches[logicalBranchKey("family", "main")]
	if active.Roots.MemorySnapshot != 7 || active.Roots.FilesystemRoot != "cold/manifests/family/main/0000000000000001.json" {
		t.Fatalf("active branch lost lazy backing roots: %+v", active)
	}
	if len(state.Publications) != 0 {
		t.Fatalf("active branch retained committed publication history: %+v", state.Publications)
	}
	encoded, err := state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreAuthorityState(encoded); err != nil {
		t.Fatalf("restoring authority after consumed snapshot retirement: %v", err)
	}
}

func TestAuthorityDiscardDehydratedMemoryPreservesFilesystem(t *testing.T) {
	at := time.Unix(3550, 0).UTC()
	state := NewAuthorityState()
	state.NextSnapshotID = 8
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "cold", PhysicalContainer: "physical", PhysicalBranch: "cold",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "cold", PublicationID: "publication-cold", Lifecycle: "dehydrated", BranchGeneration: created.Generation,
	}))
	ready := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-ready", at.Add(2*time.Second), AuthorityPublicationReady, AuthorityPublicationPayload{
		Family: "family", Branch: "cold", PublicationID: "publication-cold", BranchGeneration: created.Generation, Revision: begin.Revision,
		Roots: DurableRoots{MemorySnapshot: 7},
	}))
	applyAuthorityForTest(t, state, "publish-commit", at.Add(3*time.Second), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: "family", Branch: "cold", PublicationID: "publication-cold", BranchGeneration: created.Generation, Revision: ready.Revision,
		Roots: DurableRoots{MemorySnapshot: 7, FilesystemRoot: "cold/manifests/family/cold/publication-cold.json"},
	})
	committed := state.Branches[logicalBranchKey("family", "cold")]
	discarded := decodeAuthorityResultForTest[BranchAuthority](t, applyAuthorityForTest(t, state, "discard-memory", at.Add(4*time.Second), AuthorityBranchDiscardMemory, AuthorityPublicationPayload{
		Family: "family", Branch: "cold", BranchGeneration: committed.Generation,
	}))
	if discarded.Lifecycle != "exited" || discarded.Roots.MemorySnapshot != 0 || discarded.Roots.MemoryRoot != "" ||
		discarded.Roots.FilesystemRoot != "cold/manifests/family/cold/publication-cold.json" || discarded.PublicationID != "" {
		t.Fatalf("discarded authority = %+v", discarded)
	}
	placement := state.Placements[logicalBranchKey("family", "cold")]
	if placement.State != PlacementUnplaced || placement.Generation != discarded.Generation {
		t.Fatalf("discarded placement = %+v, branch = %+v", placement, discarded)
	}
	if len(state.Publications) != 0 {
		t.Fatalf("discard retained committed publication history: %+v", state.Publications)
	}
	stale, _ := NewAuthorityCommand("discard-memory-stale", at.Add(5*time.Second), AuthorityBranchDiscardMemory, AuthorityPublicationPayload{
		Family: "family", Branch: "cold", BranchGeneration: committed.Generation,
	})
	if result := state.Apply(stale); result.Err() == nil {
		t.Fatal("stale memory discard unexpectedly succeeded")
	}
}

func TestAuthorityCanceledDehydrationRestoresPriorLifecycle(t *testing.T) {
	at := time.Unix(3600, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-1", Lifecycle: "dehydrated", BranchGeneration: created.Generation,
	}))
	if begin.PreviousLifecycle != "running" || state.Branches[logicalBranchKey("family", "main")].Lifecycle != "dehydrating" {
		t.Fatalf("dehydration transition=%+v branch=%+v", begin, state.Branches[logicalBranchKey("family", "main")])
	}
	applyAuthorityForTest(t, state, "publish-cancel", at.Add(2*time.Second), AuthorityPublicationCancel, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: begin.PublicationID, BranchGeneration: created.Generation,
		Revision: begin.Revision,
	})
	branch := state.Branches[logicalBranchKey("family", "main")]
	if branch.Lifecycle != "running" || branch.PublicationID != "" {
		t.Fatalf("canceled dehydration branch=%+v, want running with no publication", branch)
	}
	applyAuthorityForTest(t, state, "publish-new", at.Add(3*time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-2", Lifecycle: "dehydrated", BranchGeneration: created.Generation,
	})
}

func TestAuthorityCanCancelSupersededPublicationWithoutMutatingCurrent(t *testing.T) {
	at := time.Unix(3650, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	old := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-old", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: "publication-old", Lifecycle: "exited", BranchGeneration: created.Generation,
	}))
	branchKey := logicalBranchKey("family", "main")
	current := state.Branches[branchKey]
	current.PublicationID = "publication-new"
	state.Branches[branchKey] = current
	applyAuthorityForTest(t, state, "cancel-old", at.Add(2*time.Second), AuthorityPublicationCancel, AuthorityPublicationPayload{
		Family: "family", Branch: "main", PublicationID: old.PublicationID,
		BranchGeneration: old.BranchGeneration, Revision: old.Revision,
	})
	if got := state.Publications[publicationKey("family", "main", "publication-old")].State; got != PublicationCanceled {
		t.Fatalf("old publication state = %q, want canceled", got)
	}
	if got := state.Branches[branchKey].PublicationID; got != "publication-new" {
		t.Fatalf("canceling old publication changed current identity to %q", got)
	}
}

func TestAuthorityForkPublishCreatesPlacementAtomically(t *testing.T) {
	at := time.Unix(3500, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at)
	state.Placements[logicalBranchKey("source", "main")] = PlacementRecord{Quarantined: true}
	state.Transactions["fork-1"] = Transaction{
		Identity: ForkIdentity{
			TransactionID: "fork-1", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "source", SourceBranch: "main", TargetContainer: "friendly-name",
			TargetBranch: "hidden-fork", TargetVisibleBranch: "child",
		},
		State: TransactionTargetPaused, Revision: 5,
	}
	result := decodeAuthorityResultForTest[ForkPublishResult](t, applyAuthorityForTest(t, state, "publish-fork", at.Add(2*time.Second), AuthorityForkPublish, AuthorityForkPublishPayload{
		TransactionID: "fork-1", TransactionRevision: 5, DesiredPaused: false,
		Family: "physical-family", Branch: "child", PhysicalContainer: "physical-family", PhysicalBranch: "child",
		TargetIncarnation: "incarnation-b",
	}))
	if result.Transaction.State != TransactionPublished || result.Transaction.Activated || result.Placement.OwnerNode != "node-b" || result.Placement.State != PlacementRunning || !result.Placement.Quarantined {
		t.Fatalf("bad atomic fork publication: %+v", result)
	}
	if state.Revision != 3 { // register, ready, publish
		t.Fatalf("fork publication did not consume one authority revision: state=%d", state.Revision)
	}
	activated := decodeAuthorityResultForTest[Transaction](t, applyAuthorityForTest(t, state, "activate-fork", at.Add(3*time.Second), AuthorityTransactionActivate, AuthorityTransactionPayload{
		ID: "fork-1", Revision: result.Transaction.Revision,
	}))
	if !activated.Activated || activated.State != TransactionPublished {
		t.Fatalf("activated fork = %+v", activated)
	}
}

func TestAuthorityActivationMayRecoverAfterIndependence(t *testing.T) {
	at := time.Unix(2100, 0)
	state := NewAuthorityState()
	state.Transactions["fork-1"] = Transaction{
		Identity: ForkIdentity{TransactionID: "fork-1", SourceNodeID: "node-a", TargetNodeID: "node-b"},
		State:    TransactionIndependent, Revision: 8, CreatedAt: at, UpdatedAt: at,
	}
	activated := decodeAuthorityResultForTest[Transaction](t, applyAuthorityForTest(t, state, "activate-late", at.Add(time.Second), AuthorityTransactionActivate, AuthorityTransactionPayload{
		ID: "fork-1", Revision: 8,
	}))
	if !activated.Activated || activated.State != TransactionIndependent || activated.Revision != 9 {
		t.Fatalf("late activation = %+v", activated)
	}
}

func TestAuthorityRetiresTerminalTransactionOnlyAfterTargetRemoval(t *testing.T) {
	at := time.Unix(3600, 0).UTC()
	state := NewAuthorityState()
	state.Transactions["fork-1"] = Transaction{
		Identity: ForkIdentity{
			TransactionID: "fork-1", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "friendly-name",
			TargetBranch: "hidden", TargetVisibleBranch: "child",
		},
		State: TransactionIndependent, Revision: 9, CreatedAt: at, UpdatedAt: at,
	}
	key := logicalBranchKey("family", "child")
	state.Placements[key] = PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "family", PhysicalBranch: "child",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", Generation: 1,
		State: PlacementRunning, Revision: 1, UpdatedAt: at,
	}
	state.Branches[key] = BranchAuthority{FamilyID: "family", BranchID: "child", Generation: 1, Lifecycle: "running", UpdatedAt: at}
	publication := PublicationRecord{FamilyID: "family", BranchID: "child", BranchGeneration: 1, PublicationID: "publication-7", State: PublicationCanceled}
	state.Publications[publicationKey("family", "child", "publication-7")] = publication
	command, err := NewAuthorityCommand("retire-live", at.Add(time.Second), AuthorityTransactionRetire, AuthorityTransactionPayload{ID: "fork-1", Revision: 9})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(command); result.Err() == nil {
		t.Fatal("retired transaction while its target placement existed")
	}
	applyAuthorityForTest(t, state, "remove-target", at.Add(2*time.Second), AuthorityPlacementRemove, AuthorityPlacementPayload{
		Family: "family", Branch: "child", Generation: 1,
	})
	if len(state.Publications) != 0 {
		t.Fatalf("branch removal retained terminal publication history: %+v", state.Publications)
	}
	applyAuthorityForTest(t, state, "retire-removed", at.Add(3*time.Second), AuthorityTransactionRetire, AuthorityTransactionPayload{ID: "fork-1", Revision: 9})
	if _, ok := state.Transactions["fork-1"]; ok {
		t.Fatal("terminal transaction remains after target removal")
	}
}

func TestAuthorityRemovingCorruptedBranchReparentsChildren(t *testing.T) {
	at := time.Unix(3625, 0).UTC()
	state := NewAuthorityState()
	parentKey := logicalBranchKey("family", "parent")
	childKey := logicalBranchKey("family", "child")
	grandchildKey := logicalBranchKey("family", "grandchild")
	state.Branches[parentKey] = BranchAuthority{FamilyID: "family", BranchID: "parent", Generation: 1, Lifecycle: "running", UpdatedAt: at}
	state.Branches[childKey] = BranchAuthority{FamilyID: "family", BranchID: "child", Generation: 2, Lifecycle: "corrupted", Corrupted: true, UpdatedAt: at}
	state.Branches[grandchildKey] = BranchAuthority{FamilyID: "family", BranchID: "grandchild", Generation: 1, Lifecycle: "running", UpdatedAt: at}
	state.Placements[parentKey] = PlacementRecord{FamilyID: "family", BranchID: "parent", Generation: 1, Revision: 1, State: PlacementUnplaced, UpdatedAt: at}
	state.Placements[childKey] = PlacementRecord{FamilyID: "family", BranchID: "child", ParentBranch: "parent", Generation: 2, Revision: 2, State: PlacementCorrupted, UpdatedAt: at}
	state.Placements[grandchildKey] = PlacementRecord{FamilyID: "family", BranchID: "grandchild", ParentBranch: "child", Generation: 1, Revision: 1, State: PlacementUnplaced, UpdatedAt: at}
	applyAuthorityForTest(t, state, "remove-corrupted-child", at.Add(time.Second), AuthorityBranchRemoveCorrupted, AuthorityPublicationPayload{
		Family: "family", Branch: "child", BranchGeneration: 2,
	})
	if _, ok := state.Placements[childKey]; ok {
		t.Fatal("corrupted placement remained after removal")
	}
	if got := state.Placements[grandchildKey].ParentBranch; got != "parent" {
		t.Fatalf("grandchild parent=%q, want parent", got)
	}
}

func TestAuthorityRejectsRetirementWithVolatileDependency(t *testing.T) {
	at := time.Unix(3650, 0).UTC()
	state := NewAuthorityState()
	state.Transactions["failed-1"] = Transaction{
		Identity: ForkIdentity{TransactionID: "failed-1"},
		State:    TransactionFailed, Revision: 4, CreatedAt: at, UpdatedAt: at,
	}
	state.Branches[logicalBranchKey("family", "child")] = BranchAuthority{
		FamilyID: "family", BranchID: "child", Generation: 1, Lifecycle: "running",
		VolatileDependencies: []VolatileDependency{{TransactionID: "failed-1", SourceNode: "node-a", SourceIncarnation: "incarnation-a"}},
		UpdatedAt:            at,
	}
	command, err := NewAuthorityCommand("retire-dependent", at.Add(time.Second), AuthorityTransactionRetire, AuthorityTransactionPayload{ID: "failed-1", Revision: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result := state.Apply(command); result.Err() == nil {
		t.Fatal("retired transaction while a volatile dependency referenced it")
	}
}

// A source whose lease expired only makes forks still paging from it wait;
// removing it corrupts exactly those.
func TestAuthorityRemovalCorruptsExactDependencyClosure(t *testing.T) {
	at := time.Unix(3700, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at)
	state.Transactions["fork-1"] = Transaction{
		Identity: ForkIdentity{
			TransactionID: "fork-1", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "source", SourceBranch: "main", TargetContainer: "target",
			TargetBranch: "hidden", TargetVisibleBranch: "main",
		},
		SourceIncarnation: "incarnation-a", TargetIncarnation: "incarnation-b",
		SourceNeeded: true, State: TransactionTargetPaused, Revision: 5,
	}
	applyAuthorityForTest(t, state, "publish-fork", at.Add(2*time.Second), AuthorityForkPublish, AuthorityForkPublishPayload{
		TransactionID: "fork-1", TransactionRevision: 5,
		Family: "target", Branch: "main", PhysicalContainer: "target", PhysicalBranch: "main",
		TargetIncarnation: "incarnation-b",
	})
	childPlacement := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place-child", at.Add(3*time.Second), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "target", BranchID: "child", PhysicalContainer: "target", PhysicalBranch: "child",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementRunning,
	}}))
	applyAuthorityForTest(t, state, "inherit-child", at.Add(4*time.Second), AuthorityBranchInherit, AuthorityBranchInheritPayload{
		Family: "target", Branch: "child", SourceBranch: "main", BranchGeneration: childPlacement.Generation,
	})
	independentPlacement := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place-independent", at.Add(5*time.Second), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "other", BranchID: "main", PhysicalContainer: "other", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementRunning,
	}}))
	applyAuthorityForTest(t, state, "inherit-independent", at.Add(6*time.Second), AuthorityBranchInherit, AuthorityBranchInheritPayload{
		Family: "other", Branch: "main", BranchGeneration: independentPlacement.Generation,
	})

	source := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "expire-source", source.LeaseExpiresAt, AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: source.NodeID, Incarnation: source.Incarnation, Revision: source.Revision,
	})
	for _, key := range []string{logicalBranchKey("target", "main"), logicalBranchKey("target", "child"), logicalBranchKey("other", "main")} {
		if branch := state.Branches[key]; branch.Corrupted {
			t.Fatalf("branch %q was corrupted by its source's expiry: %+v", key, branch)
		}
	}
	if tx := state.Transactions["fork-1"]; tx.State == TransactionFailed || !tx.SourceNeeded {
		t.Fatalf("expiry gave up a published fork's source: %+v", tx)
	}
	offline := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "remove-source", offline.UpdatedAt.Add(time.Second), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: offline.NodeID, Incarnation: offline.Incarnation, Revision: offline.Revision,
	})
	for _, key := range []string{logicalBranchKey("target", "main"), logicalBranchKey("target", "child")} {
		if branch := state.Branches[key]; !branch.Corrupted {
			t.Fatalf("dependent branch %q survived its source's removal: %+v", key, branch)
		}
	}
	if branch := state.Branches[logicalBranchKey("other", "main")]; branch.Corrupted {
		t.Fatalf("independent branch was corrupted: %+v", branch)
	}
	if tx := state.Transactions["fork-1"]; tx.State != TransactionFailed || tx.SourceNeeded {
		t.Fatalf("source-lost transaction remains usable: %+v", tx)
	}
}

// An expired provider keeps its unpublished origins for when it returns;
// removing it abandons them.
func TestAuthorityRemovalAbandonsProviderOrigins(t *testing.T) {
	at := time.Unix(3750, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	allocation := decodeAuthorityResultForTest[SnapshotAllocation](t, applyAuthorityForTest(t, state, "allocate", at.Add(time.Second), AuthoritySnapshotAllocate, AuthoritySnapshotPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", Count: 2,
	}))
	node := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "expire", node.LeaseExpiresAt, AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: node.NodeID, Incarnation: node.Incarnation, Revision: node.Revision,
	})
	for id := allocation.First; id < allocation.First+uint64(allocation.Count); id++ {
		if _, ok := state.VolatileOrigins[id]; !ok || state.AbandonedOrigins[id] {
			t.Fatalf("expiry gave up volatile origin %d", id)
		}
	}
	offline := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "remove", offline.UpdatedAt.Add(time.Second), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: offline.NodeID, Incarnation: offline.Incarnation, Revision: offline.Revision,
	})
	if _, ok := state.Nodes["node-a"]; ok {
		t.Fatal("expired provider remains in membership after removal")
	}
	for id := allocation.First; id < allocation.First+uint64(allocation.Count); id++ {
		if _, ok := state.VolatileOrigins[id]; ok || !state.AbandonedOrigins[id] {
			t.Fatalf("removed provider's origin %d lacks abandonment fence", id)
		}
	}
	if _, err := RestoreAuthorityState(mustAuthoritySnapshotForTest(t, state)); err != nil {
		t.Fatalf("state after provider removal is not restorable: %v", err)
	}
}

func TestAuthorityIndependenceDropsDependencyBeforeExpiry(t *testing.T) {
	at := time.Unix(3800, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at)
	state.Transactions["fork-1"] = Transaction{
		Identity: ForkIdentity{
			TransactionID: "fork-1", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "source", SourceBranch: "main", TargetContainer: "target",
			TargetBranch: "hidden", TargetVisibleBranch: "main",
		},
		SourceIncarnation: "incarnation-a", TargetIncarnation: "incarnation-b",
		SourceNeeded: true, State: TransactionTargetPaused, Revision: 5,
	}
	published := decodeAuthorityResultForTest[ForkPublishResult](t, applyAuthorityForTest(t, state, "publish-fork", at.Add(2*time.Second), AuthorityForkPublish, AuthorityForkPublishPayload{
		TransactionID: "fork-1", TransactionRevision: 5,
		Family: "target", Branch: "main", PhysicalContainer: "target", PhysicalBranch: "main",
		TargetIncarnation: "incarnation-b",
	}))
	applyAuthorityForTest(t, state, "independent", at.Add(3*time.Second), AuthorityTransactionAdvance, AuthorityTransactionPayload{
		ID: "fork-1", Revision: published.Transaction.Revision, Next: TransactionIndependent,
		Metrics: &TransferMetrics{PagesTotal: 1, PagesReleased: 1},
	})
	if dependencies := state.Branches[logicalBranchKey("target", "main")].VolatileDependencies; len(dependencies) != 0 {
		t.Fatalf("independent branch retains dependencies: %+v", dependencies)
	}
	source := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "expire-source", source.LeaseExpiresAt, AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: source.NodeID, Incarnation: source.Incarnation, Revision: source.Revision,
	})
	if branch := state.Branches[logicalBranchKey("target", "main")]; branch.Corrupted {
		t.Fatalf("independent branch was corrupted: %+v", branch)
	}
}

func TestAuthorityMigrationIndependenceSettlesUnreferencedPredecessor(t *testing.T) {
	at := time.Unix(3850, 0).UTC()
	state := NewAuthorityState()
	predecessor := Transaction{
		Identity: ForkIdentity{
			TransactionID: "fork-old", SourceNodeID: "node-a", TargetNodeID: "node-b",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "display-name",
			TargetBranch: "old-hidden", TargetVisibleBranch: "main",
		},
		SourceNeeded: true, State: TransactionPublished, Revision: 6, CreatedAt: at, UpdatedAt: at,
	}
	migration := Transaction{
		Identity: ForkIdentity{
			TransactionID: "move-new", SourceNodeID: "node-b", TargetNodeID: "node-c",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "family",
			TargetBranch: "move-hidden", TargetVisibleBranch: "main", Migration: true,
		},
		SourceNeeded: true, State: TransactionDraining, Revision: 8, CreatedAt: at.Add(time.Second), UpdatedAt: at.Add(time.Second),
		OwnershipCommitted: true, InheritedDependencies: []string{predecessor.Identity.TransactionID},
	}
	successor := Transaction{
		Identity: ForkIdentity{
			TransactionID: "move-later", SourceNodeID: "node-c", TargetNodeID: "node-d",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "family",
			TargetBranch: "later-hidden", TargetVisibleBranch: "main", Migration: true,
		},
		SourceNeeded: true, State: TransactionPublished, Revision: 6, CreatedAt: at.Add(2 * time.Second), UpdatedAt: at.Add(2 * time.Second),
		OwnershipCommitted: true,
	}
	state.Transactions[predecessor.Identity.TransactionID] = predecessor
	state.Transactions[migration.Identity.TransactionID] = migration
	state.Transactions[successor.Identity.TransactionID] = successor
	key := logicalBranchKey("family", "main")
	state.Branches[key] = BranchAuthority{
		FamilyID: "family", BranchID: "main", Generation: 3, Lifecycle: "running",
		VolatileDependencies: []VolatileDependency{
			{TransactionID: predecessor.Identity.TransactionID, SourceNode: "node-a", SourceIncarnation: "inc-a"},
			{TransactionID: migration.Identity.TransactionID, SourceNode: "node-b", SourceIncarnation: "inc-b"},
			{TransactionID: successor.Identity.TransactionID, SourceNode: "node-c", SourceIncarnation: "inc-c"},
		},
		DependenciesInherited: true, UpdatedAt: at,
	}
	state.Placements[key] = PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-c", OwnerIncarnation: "inc-c", Generation: 3, State: PlacementRunning, Revision: 3, UpdatedAt: at,
	}
	if !volatileDependencyForwarded(state, "family", "main", predecessor.Identity.TransactionID, "node-b") {
		t.Fatal("predecessor dependency was not recognized as forwarded")
	}
	applyAuthorityForTest(t, state, "independent-migration", at.Add(3*time.Second), AuthorityTransactionAdvance, AuthorityTransactionPayload{
		ID: migration.Identity.TransactionID, Revision: migration.Revision, Next: TransactionIndependent,
		Metrics: &TransferMetrics{PagesTotal: 2, PagesReleased: 2},
	})
	if dependencies := state.Branches[key].VolatileDependencies; len(dependencies) != 1 || dependencies[0].TransactionID != successor.Identity.TransactionID {
		t.Fatalf("independent migrated branch did not preserve its newer dependency: %+v", dependencies)
	}
	if got := state.Transactions[predecessor.Identity.TransactionID]; got.State != TransactionIndependent || got.SourceNeeded {
		t.Fatalf("unreferenced predecessor remains source-backed: %+v", got)
	}
	if volatileDependencyForwarded(state, "family", "main", predecessor.Identity.TransactionID, "node-b") {
		t.Fatal("settled predecessor remained classified as forwarded")
	}
	if got := state.Transactions[successor.Identity.TransactionID]; got.State != TransactionPublished || !got.SourceNeeded {
		t.Fatalf("newer migration was prematurely settled: %+v", got)
	}
}

// A committed migration cannot be canceled while its branch exists, and can
// once removal has deleted the branch: nothing reads its target any more.
func TestAuthorityCommittedMigrationCancelsOnlyOnceItsBranchIsRemoved(t *testing.T) {
	at := time.Unix(3860, 0).UTC()
	state := NewAuthorityState()
	migration := Transaction{
		Identity: ForkIdentity{
			TransactionID: "move-removed", SourceNodeID: "node-b", TargetNodeID: "node-c",
			SourceContainer: "family", SourceBranch: "main", TargetContainer: "family",
			TargetBranch: "move-hidden", TargetVisibleBranch: "main", Migration: true,
		},
		SourceNeeded: true, State: TransactionDraining, Revision: 8, CreatedAt: at, UpdatedAt: at,
		OwnershipCommitted: true,
	}
	state.Transactions[migration.Identity.TransactionID] = migration
	key := logicalBranchKey("family", "main")
	state.Branches[key] = BranchAuthority{
		FamilyID: "family", BranchID: "main", Generation: 3, Lifecycle: "running", UpdatedAt: at,
		VolatileDependencies: []VolatileDependency{{TransactionID: migration.Identity.TransactionID, SourceNode: "node-b", SourceIncarnation: "inc-b"}},
	}
	state.Placements[key] = PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-c", OwnerIncarnation: "inc-c", Generation: 3, State: PlacementRunning, Revision: 3, UpdatedAt: at,
		LastOperationID: migration.Identity.TransactionID,
	}
	cancel := func(id string, next TransactionState, failure string) AuthorityResult {
		command, err := NewAuthorityCommand(id, at.Add(time.Second), AuthorityTransactionAdvance, AuthorityTransactionPayload{
			ID: migration.Identity.TransactionID, Revision: migration.Revision, Next: next, Failure: failure,
		})
		if err != nil {
			t.Fatalf("NewAuthorityCommand: %v", err)
		}
		return state.Apply(command)
	}
	if err := cancel("cancel-live", TransactionCanceled, "").Err(); err == nil {
		t.Fatal("a committed migration whose branch exists was canceled")
	}
	delete(state.Placements, key)
	delete(state.Branches, key)
	if err := cancel("fail-removed", TransactionFailed, "lost").Err(); err == nil {
		t.Fatal("a committed migration was failed")
	}
	if err := cancel("cancel-removed", TransactionCanceled, "").Err(); err != nil {
		t.Fatalf("canceling the migration of a removed branch: %v", err)
	}
	if got := state.Transactions[migration.Identity.TransactionID]; got.State != TransactionCanceled || got.SourceNeeded {
		t.Fatalf("canceled migration is %+v", got)
	}
}

func TestAuthorityMigrationIndependenceRetainsSharedPredecessor(t *testing.T) {
	at := time.Unix(3875, 0).UTC()
	state := NewAuthorityState()
	predecessor := Transaction{
		Identity:     ForkIdentity{TransactionID: "fork-shared"},
		SourceNeeded: true, State: TransactionPublished, Revision: 4, CreatedAt: at, UpdatedAt: at,
	}
	migration := Transaction{
		Identity: ForkIdentity{
			TransactionID: "move-one", SourceContainer: "family", SourceBranch: "main",
			TargetVisibleBranch: "main", Migration: true,
		},
		SourceNeeded: true, State: TransactionPublished, Revision: 7, CreatedAt: at, UpdatedAt: at,
		OwnershipCommitted: true, InheritedDependencies: []string{predecessor.Identity.TransactionID},
	}
	state.Transactions[predecessor.Identity.TransactionID] = predecessor
	state.Transactions[migration.Identity.TransactionID] = migration
	dependency := VolatileDependency{TransactionID: predecessor.Identity.TransactionID, SourceNode: "node-a", SourceIncarnation: "inc-a"}
	state.Branches[logicalBranchKey("family", "main")] = BranchAuthority{
		FamilyID: "family", BranchID: "main", VolatileDependencies: []VolatileDependency{
			dependency, {TransactionID: migration.Identity.TransactionID, SourceNode: "node-b", SourceIncarnation: "inc-b"},
		},
	}
	state.Branches[logicalBranchKey("family", "sibling")] = BranchAuthority{
		FamilyID: "family", BranchID: "sibling", VolatileDependencies: []VolatileDependency{dependency},
	}
	applyAuthorityForTest(t, state, "independent-one", at.Add(time.Second), AuthorityTransactionAdvance, AuthorityTransactionPayload{
		ID: migration.Identity.TransactionID, Revision: migration.Revision, Next: TransactionIndependent,
	})
	if dependencies := state.Branches[logicalBranchKey("family", "main")].VolatileDependencies; len(dependencies) != 0 {
		t.Fatalf("migrated branch retains dependencies: %+v", dependencies)
	}
	if got := state.Transactions[predecessor.Identity.TransactionID]; got.State != TransactionPublished || !got.SourceNeeded {
		t.Fatalf("shared predecessor was released: %+v", got)
	}
	if dependencies := state.Branches[logicalBranchKey("family", "sibling")].VolatileDependencies; len(dependencies) != 1 || dependencies[0].TransactionID != predecessor.Identity.TransactionID {
		t.Fatalf("sibling dependency changed: %+v", dependencies)
	}
}

// A node whose lease expired has its branches back, unharmed, when it
// returns as the same incarnation; only its removal loses them.
func TestAuthorityExpiryKeepsBranchesForTheIncarnationToReturn(t *testing.T) {
	at := time.Unix(3950, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	applyAuthorityForTest(t, state, "place", at.Add(time.Second), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	key := logicalBranchKey("family", "main")
	expire := func(id string) {
		t.Helper()
		node := state.Nodes["node-a"]
		applyAuthorityForTest(t, state, id, node.LeaseExpiresAt, AuthorityNodeExpire, AuthorityNodeExpirePayload{
			NodeID: node.NodeID, Incarnation: node.Incarnation, Revision: node.Revision,
		})
	}
	expire("expire")
	if node := state.Nodes["node-a"]; node.State != NodeOffline {
		t.Fatalf("expired node is %s", node.State)
	}
	if placement := state.Placements[key]; placement.State != PlacementRunning || placement.OwnerIncarnation != "incarnation-a" || state.Branches[key].Corrupted {
		t.Fatalf("expiry took the branch from its node: placement=%+v branch=%+v", placement, state.Branches[key])
	}

	// The same incarnation returns, recovers as it joins, and is admitted
	// with its branch.
	back := state.Nodes["node-a"].LeaseExpiresAt.Add(time.Minute)
	record := testAuthorityNode(back)
	record.NodeID, record.Incarnation, record.Capabilities.NodeID = "node-a", "incarnation-a", "node-a"
	rejoined := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "register-again", back, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: time.Hour}))
	if rejoined.State != NodeJoining {
		t.Fatalf("returning incarnation is %s, want %s", rejoined.State, NodeJoining)
	}
	applyAuthorityForTest(t, state, "ready-again", back.Add(time.Second), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: "node-a", Incarnation: "incarnation-a", State: NodeReady, LeaseDuration: time.Hour,
	})
	if placement := state.Placements[key]; placement.State != PlacementRunning || placement.OwnerIncarnation != "incarnation-a" || state.Branches[key].Corrupted {
		t.Fatalf("the returned node lost its branch: placement=%+v branch=%+v", placement, state.Branches[key])
	}

	// Removal is what gives the branch up.
	expire("expire-again")
	offline := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "remove", offline.UpdatedAt.Add(time.Second), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: offline.NodeID, Incarnation: offline.Incarnation, Revision: offline.Revision,
	})
	if _, ok := state.Nodes["node-a"]; ok {
		t.Fatal("removed node is still a member")
	}
	if placement := state.Placements[key]; placement.State != PlacementCorrupted || placement.OwnerNode != "" || !state.Branches[key].Corrupted {
		t.Fatalf("removing the node kept its branch: placement=%+v branch=%+v", placement, state.Branches[key])
	}
}

func TestAuthorityExpiryRevokesMaterializingWithoutCorruption(t *testing.T) {
	at := time.Unix(3900, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at)
	unplaced := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at.Add(time.Second), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", State: PlacementUnplaced,
	}}))
	applyAuthorityForTest(t, state, "claim", at.Add(2*time.Second), AuthorityPlacementClaim, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a",
		PhysicalContainer: "family", PhysicalBranch: "main", Generation: unplaced.Generation, State: PlacementMaterializing,
	})
	node := state.Nodes["node-a"]
	applyAuthorityForTest(t, state, "expire", node.LeaseExpiresAt, AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: node.NodeID, Incarnation: node.Incarnation, Revision: node.Revision,
	})
	placement := state.Placements[logicalBranchKey("family", "main")]
	branch := state.Branches[logicalBranchKey("family", "main")]
	if placement.State != PlacementUnplaced || placement.OwnerNode != "" || placement.OwnerIncarnation != "" {
		t.Fatalf("expired materializer retained ownership: %+v", placement)
	}
	if branch.Corrupted || branch.Lifecycle != string(PlacementUnplaced) || branch.Generation != placement.Generation {
		t.Fatalf("durable branch was damaged by materializer expiry: branch=%+v placement=%+v", branch, placement)
	}
}

func TestAuthoritySnapshotRoundTrip(t *testing.T) {
	at := time.Unix(4000, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	data, err := state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreAuthorityState(data)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := restored.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(reencoded) {
		t.Fatalf("snapshot changed across round trip\nfirst: %s\nsecond: %s", data, reencoded)
	}
}

func TestAuthoritySnapshotRejectsUnfencedPublication(t *testing.T) {
	at := time.Unix(4050, 0).UTC()
	state := NewAuthorityState()
	key := logicalBranchKey("family", "main")
	state.Branches[key] = BranchAuthority{
		FamilyID: "family", BranchID: "main", Generation: 1,
		PublicationID: "publication-one", Lifecycle: "dehydrating", UpdatedAt: at,
	}
	state.Publications[publicationKey("family", "main", "publication-one")] = PublicationRecord{
		FamilyID: "family", BranchID: "main", BranchGeneration: 1,
		PublicationID: "publication-one", Lifecycle: "dehydrated", PreviousLifecycle: "paused",
		State: PublicationReady, Revision: 2, UpdatedAt: at,
	}
	data, err := state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreAuthorityState(data); err != nil {
		t.Fatalf("valid publication fence was rejected: %v", err)
	}
	branch := state.Branches[key]
	branch.PublicationID = "publication-missing"
	state.Branches[key] = branch
	data, err = state.SnapshotBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreAuthorityState(data); err == nil {
		t.Fatal("snapshot restored a branch with a missing publication fence")
	}
}
