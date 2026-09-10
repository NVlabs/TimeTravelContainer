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
	"encoding/json"
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
	registered := decodeAuthorityResultForTest[NodeRecord](t, applyAuthorityForTest(t, state, "register-"+id, at, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: node, LeaseDuration: time.Hour}))
	applyAuthorityForTest(t, state, "ready-"+id, at.Add(time.Millisecond), AuthorityNodeTransition, AuthorityNodeTransitionPayload{
		NodeID: registered.NodeID, Incarnation: registered.Incarnation, State: NodeReady, LeaseDuration: time.Hour,
	})
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
	reenveloped.LeaderTerm++
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
	}
	cutover := decodeAuthorityResultForTest[MigrationCutoverResult](t, applyAuthorityForTest(t, state, "cutover", at.Add(2*time.Second), AuthorityMigrationCutover, AuthorityMigrationCutoverPayload{
		TransactionID: "migration-1", TransactionRevision: 6,
		Family: "family", Branch: "main", Operation: "migration-1", Target: "node-b",
		TargetIncarnation: "incarnation-b", PhysicalContainer: "family", PhysicalBranch: "main",
	}))
	committed := cutover.Placement
	if committed.OwnerNode != "node-b" || committed.Generation != begin.Generation+1 || committed.State != PlacementRunning {
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

func TestAuthorityPublicationGenerationPreventsLateResurrection(t *testing.T) {
	at := time.Unix(3000, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", Lifecycle: "dehydrated", BranchGeneration: created.Generation, StorageGeneration: 1,
	}))
	ready := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-ready", at.Add(2*time.Second), AuthorityPublicationReady, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", BranchGeneration: created.Generation, StorageGeneration: 1, Revision: begin.Revision,
	}))
	applyAuthorityForTest(t, state, "corrupt", at.Add(3*time.Second), AuthorityBranchCorrupt, AuthorityPublicationPayload{
		Family: "family", Branch: "main", BranchGeneration: created.Generation, Failure: "authoritative node-local state was lost",
	})
	late, _ := NewAuthorityCommand("publish-late", at.Add(4*time.Second), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", BranchGeneration: created.Generation, StorageGeneration: 1, Revision: ready.Revision,
		Roots: DurableRoots{MemoryRoot: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if result := state.Apply(late); result.Err() == nil {
		t.Fatal("late publisher resurrected corrupted branch")
	}
	branch := state.Branches[logicalBranchKey("family", "main")]
	if !branch.Corrupted || branch.Roots.MemoryRoot != "" {
		t.Fatalf("corrupted branch was changed: %+v", branch)
	}
	if placement := state.Placements[logicalBranchKey("family", "main")]; placement.State != PlacementCorrupted {
		t.Fatalf("corrupted branch retained placement state %+v", placement)
	}
}

func TestAuthorityPublicationUnplacesHealthyBranch(t *testing.T) {
	at := time.Unix(3500, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-a", "incarnation-a", at.Add(-time.Second))
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", Lifecycle: "exited", BranchGeneration: created.Generation, StorageGeneration: 1,
	}))
	ready := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-ready", at.Add(2*time.Second), AuthorityPublicationReady, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", BranchGeneration: created.Generation, StorageGeneration: 1, Revision: begin.Revision,
	}))
	applyAuthorityForTest(t, state, "publish-commit", at.Add(3*time.Second), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", BranchGeneration: created.Generation, StorageGeneration: 1, Revision: ready.Revision,
		Roots: DurableRoots{MemorySnapshot: 7, FilesystemRoot: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	placement := state.Placements[logicalBranchKey("family", "main")]
	if placement.State != PlacementUnplaced {
		t.Fatalf("durable healthy branch placement = %+v, want unplaced", placement)
	}
	branch := state.Branches[logicalBranchKey("family", "main")]
	if branch.Corrupted || branch.Lifecycle != "exited" || branch.Roots.MemorySnapshot != 7 {
		t.Fatalf("durable healthy branch authority = %+v", branch)
	}
}

func TestAuthorityForkPublishCreatesPlacementAtomically(t *testing.T) {
	at := time.Unix(3500, 0).UTC()
	state := NewAuthorityState()
	registerReadyAuthorityNode(t, state, "node-b", "incarnation-b", at)
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
	if result.Transaction.State != TransactionPublished || result.Transaction.Activated || result.Placement.OwnerNode != "node-b" || result.Placement.State != PlacementRunning {
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

func TestAuthorityExpiryCorruptsExactDependencyClosure(t *testing.T) {
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
	for _, key := range []string{logicalBranchKey("target", "main"), logicalBranchKey("target", "child")} {
		if branch := state.Branches[key]; !branch.Corrupted {
			t.Fatalf("dependent branch %q survived source expiry: %+v", key, branch)
		}
	}
	if branch := state.Branches[logicalBranchKey("other", "main")]; branch.Corrupted {
		t.Fatalf("independent branch was corrupted: %+v", branch)
	}
	if tx := state.Transactions["fork-1"]; tx.State != TransactionFailed || tx.SourceNeeded {
		t.Fatalf("source-lost transaction remains usable: %+v", tx)
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
