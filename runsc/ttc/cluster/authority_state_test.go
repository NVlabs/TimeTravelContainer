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

func testAuthorityNode(at time.Time) NodeRecord {
	capabilities := testCapabilities("node-a")
	return NodeRecord{
		NodeID: "node-a", Incarnation: "incarnation-a", ControlURL: "https://node-a:7443",
		ControlServerName: "node-a", Capabilities: capabilities, MemoryTargetBytes: 8 << 30,
		StartedAt: at,
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

func TestAuthorityMigrationCutoverFencesOldOwner(t *testing.T) {
	at := time.Unix(2000, 0).UTC()
	state := NewAuthorityState()
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}}))
	begin := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "begin", at.Add(time.Second), AuthorityPlacementBegin, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Owner: "node-a", Incarnation: "incarnation-a", Generation: created.Generation, Operation: "migration-1", Target: "node-b",
	}))
	committed := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "commit", at.Add(2*time.Second), AuthorityPlacementCommit, AuthorityPlacementPayload{
		Family: "family", Branch: "main", Operation: "migration-1", Target: "node-b", TargetIncarnation: "incarnation-b", PhysicalContainer: "family", PhysicalBranch: "main",
	}))
	if committed.OwnerNode != "node-b" || committed.Generation != begin.Generation+1 || committed.State != PlacementRunning {
		t.Fatalf("bad migration cutover: %+v", committed)
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
	created := decodeAuthorityResultForTest[PlacementRecord](t, applyAuthorityForTest(t, state, "place", at, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementPaused,
	}}))
	begin := decodeAuthorityResultForTest[PublicationRecord](t, applyAuthorityForTest(t, state, "publish-begin", at.Add(time.Second), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: "family", Branch: "main", Operation: "publication-1", BranchGeneration: created.Generation, StorageGeneration: 1,
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
}

func TestAuthoritySnapshotRoundTrip(t *testing.T) {
	at := time.Unix(4000, 0).UTC()
	state := NewAuthorityState()
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
