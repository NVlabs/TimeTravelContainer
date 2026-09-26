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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestSnapshotReferenceRequesterAuthorization(t *testing.T) {
	tx := &cluster.Transaction{Identity: cluster.ForkIdentity{
		TransactionID: "fork-move", SourceNodeID: "node-source", TargetNodeID: "node-target", Migration: true,
	}}
	for _, test := range []struct {
		name, peer, operation string
		want                  bool
	}{
		{name: "target ordinary", peer: "node-target", operation: "branch-pin", want: true},
		{name: "source transfer", peer: "node-source", operation: "transfer-fork-move", want: true},
		{name: "source ordinary denied", peer: "node-source", operation: "branch-pin"},
		{name: "other transfer denied", peer: "node-other", operation: "transfer-fork-move"},
		{name: "wrong transfer transaction denied", peer: "node-source", operation: "transfer-fork-other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := snapshotReferenceRequesterAuthorized(tx, test.peer, test.operation); got != test.want {
				t.Fatalf("authorized=%t, want %t", got, test.want)
			}
		})
	}
	nonMigration := *tx
	nonMigration.Identity.Migration = false
	if snapshotReferenceRequesterAuthorized(&nonMigration, "node-source", "transfer-fork-move") {
		t.Fatal("non-migration source received transfer authority")
	}
}

func TestGraphReferenceAuthorizationTransaction(t *testing.T) {
	for _, test := range []struct {
		name   string
		intent graphReferenceIntent
		want   string
	}{
		{
			name: "remote import",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeBranch, Transaction: "fork-1", Operation: "fork-1",
			},
			want: "fork-1",
		},
		{
			name: "local descendant",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeBranch, Transaction: "fork-1", Operation: "branch-2",
			},
		},
		{
			name: "sparse handoff replacement",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeBranch, Transaction: "fork-1", Operation: graphHandoffReferenceOperation("fork-1", 42),
			},
			want: "fork-1",
		},
		{
			name: "migration transfer",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeTransfer, Transaction: "fork-1", Operation: "transfer-fork-1",
			},
			want: "fork-1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := graphReferenceAuthorizationTransaction(test.intent); got != test.want {
				t.Fatalf("authorization transaction=%q, want %q", got, test.want)
			}
		})
	}
}

func TestGraphReferenceOperationIsBranchScoped(t *testing.T) {
	info := ttcbundle.ContainerInfo{DistributedForkTransactionID: "family-transaction"}
	initial := ttcbundle.BranchInfo{ID: "remote"}
	if got := graphReferenceOperationForBranch(info, initial); got != "" {
		t.Fatalf("initial operation=%q, want no implicit family fallback", got)
	}
	child := ttcbundle.BranchInfo{
		ID: "local", DistributedForkTransactionID: "family-transaction",
		DistributedGraphReferenceOperation: "branch-independent",
	}
	if got := graphReferenceOperationForBranch(info, child); got != "branch-independent" {
		t.Fatalf("child operation=%q, want independent branch identity", got)
	}
}

func TestGraphReferenceIntentSurvivesRenameAndNameReuse(t *testing.T) {
	intent := graphReferenceIntent{
		Transaction: "fork-original", Operation: "branch-reference", Branch: "original-name",
	}
	info := ttcbundle.ContainerInfo{Branches: []ttcbundle.BranchInfo{
		{
			ID: "renamed", DistributedForkTransactionID: "fork-original",
			DistributedGraphReferenceOperation: "branch-reference",
		},
		{
			ID: "original-name", DistributedForkTransactionID: "fork-reused",
			DistributedGraphReferenceOperation: "other-reference",
		},
	}}
	if !containerHasGraphReferenceIntent(info, intent) {
		t.Fatal("renamed branch no longer owns its graph reference intent")
	}
	info.Branches = info.Branches[1:]
	if containerHasGraphReferenceIntent(info, intent) {
		t.Fatal("reused visible name was mistaken for the original branch")
	}
}

func TestPublishedOriginReleasesOnlyItsProviderRoots(t *testing.T) {
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	first, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	firstObject, err := graph.CreateObject(first, "build/first", ttcsnapshot.ObjectKindMemoryNode, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(first, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	second, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	secondObject, err := graph.CreateObject(second, "build/second", ttcsnapshot.ObjectKindMemoryNode, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(second, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	bindings := map[string]ttcsnapshot.ObjectID{
		"remote/1/op/00000000":           firstObject,
		"memory/published/resource-0000": firstObject,
		"remote/2/op/00000000":           secondObject,
	}
	if err := graph.RetainRoots(bindings); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePins([]string{"build/first", "build/second"}); err != nil {
		t.Fatal(err)
	}
	if err := graph.MarkShared(first); err != nil {
		t.Fatal(err)
	}
	if err := releasePublishedProviderGraphRoots(graph, nil, first, []string{"memory/published/resource-0000"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root("remote/1/op/00000000"); ok {
		t.Fatal("published provider root remains")
	}
	if _, ok := graph.Root("memory/published/resource-0000"); ok {
		t.Fatal("published branch-local root remains")
	}
	if _, ok := graph.Object(firstObject); ok {
		t.Fatal("published provider-only object remains in local graph")
	}
	if got, ok := graph.Root("remote/2/op/00000000"); !ok || got != secondObject {
		t.Fatalf("unrelated provider root=(%s,%t), want %s", got, ok, secondObject)
	}
}

func TestGraphReferenceIdentitySurvivesProviderPublication(t *testing.T) {
	left := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "op", Purpose: graphReferencePurposeBranch, Container: "family", Branch: "branch",
		Groups: []graphReferenceGroup{{Origin: 1, Provider: "node-a", Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}}},
	}
	right := left
	right.Groups = append([]graphReferenceGroup(nil), left.Groups...)
	right.Groups[0].Provider = ""
	if !graphReferenceIntentEqualIdentity(left, right) {
		t.Fatal("node-to-shared provider transition changed operation identity")
	}
	right.Groups[0].Roots = []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000002"}}
	if graphReferenceIntentEqualIdentity(left, right) {
		t.Fatal("different root object reused operation identity")
	}
}

func TestGraphReferenceTransferLifetimeFollowsTransaction(t *testing.T) {
	intent := graphReferenceIntent{Purpose: graphReferencePurposeTransfer, State: "active"}
	for _, state := range []cluster.TransactionState{cluster.TransactionPreparing, cluster.TransactionTargetPaused, cluster.TransactionPublished, cluster.TransactionDraining, cluster.TransactionFailed} {
		if graphReferenceIntentShouldRelease(intent, &cluster.Transaction{State: state}, nil, false) {
			t.Fatalf("transfer intent released in %s", state)
		}
	}
	for _, state := range []cluster.TransactionState{cluster.TransactionIndependent, cluster.TransactionCanceled} {
		if !graphReferenceIntentShouldRelease(intent, &cluster.Transaction{State: state}, nil, true) {
			t.Fatalf("transfer intent retained in %s", state)
		}
	}
	if graphReferenceIntentShouldRelease(intent, nil, os.ErrNotExist, false) {
		t.Fatal("transfer intent released while transaction authority was unavailable")
	}
	branch := graphReferenceIntent{Purpose: graphReferencePurposeBranch, State: "active"}
	for _, state := range []cluster.TransactionState{
		cluster.TransactionPreparing,
		cluster.TransactionTargetReserved,
		cluster.TransactionSourceCaptured,
		cluster.TransactionTargetRestoring,
	} {
		if graphReferenceIntentShouldRelease(branch, &cluster.Transaction{State: state}, nil, false) {
			t.Fatalf("branch intent released before ownership handoff in %s", state)
		}
	}
	for _, state := range []cluster.TransactionState{
		cluster.TransactionTargetPaused,
		cluster.TransactionPublished,
		cluster.TransactionDraining,
		cluster.TransactionIndependent,
		cluster.TransactionCanceled,
		cluster.TransactionFailed,
	} {
		if !graphReferenceIntentShouldRelease(branch, &cluster.Transaction{State: state}, nil, false) {
			t.Fatalf("orphaned branch intent was retained in %s", state)
		}
	}
	if graphReferenceIntentShouldRelease(branch, nil, errors.New("temporary quorum failure"), false) {
		t.Fatal("transient transaction lookup failure released a branch intent")
	}
	if !graphReferenceIntentShouldRelease(branch, nil, os.ErrNotExist, false) {
		t.Fatal("missing transaction and branch retained an unreachable intent")
	}
	if graphReferenceIntentShouldRelease(branch, &cluster.Transaction{State: cluster.TransactionIndependent}, nil, true) {
		t.Fatal("live branch intent was released after transaction independence")
	}
}

func TestGraphReferenceIntentRejectsOldVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intent.json")
	legacy := `{"version":1,"transaction":"tx","operation":"op","container":"family","branch":"child","state":"active","groups":[{"origin":1,"roots":[{"slot":0,"object":"0000000000000001-03-0000000000000001"}]}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGraphReferenceIntent(path); err == nil {
		t.Fatal("old graph reference intent was accepted")
	}
}

func TestSharedReferenceCoordinatorTransitionClassification(t *testing.T) {
	for _, message := range []string{
		"shared reference coordinator has no active lease",
		"cluster peer: this daemon does not hold an active shared reference lease",
		"shared reference lease is stale",
		"cluster target node \"node-a\" is ready or its lease has expired",
		"snapshot 17 references are frozen for authority handoff",
		"snapshot 17 objects are frozen for authority handoff",
	} {
		if !isSharedReferenceCoordinatorTransition(errors.New(message)) {
			t.Fatalf("%q was not classified as coordinator turnover", message)
		}
	}
	if isSharedReferenceCoordinatorTransition(errors.New("snapshot root is missing")) {
		t.Fatal("permanent graph corruption was classified as coordinator turnover")
	}
}

func TestSharedRootReleaseSurvivesCoordinatorReceiptLoss(t *testing.T) {
	ledger := ttcsnapshot.NewReferenceLedger()
	origin := ttcsnapshot.SnapshotID(17)
	object := ttcsnapshot.Object{ID: ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 1}}
	if err := ledger.ImportOrigin(origin, []ttcsnapshot.Object{object}, map[string]ttcsnapshot.ObjectID{"root/source": object.ID}); err != nil {
		t.Fatal(err)
	}
	name := "remote/17/branch-test/00000000"
	roots := map[string]ttcsnapshot.ObjectID{name: object.ID}
	if err := applySharedRootMutation(ledger, []string{name}, roots, "retain", true); err != nil {
		t.Fatal(err)
	}
	if err := applySharedRootMutation(ledger, []string{name}, roots, "release", false); err != nil {
		t.Fatal(err)
	}
	// A rebuilt coordinator has the resulting reference set, but not the old
	// release receipt. Replaying the branch intent must still be a no-op.
	rebuilt := ttcsnapshot.NewReferenceLedger()
	objects, references, ok := ledger.OriginManifest(origin)
	if !ok {
		t.Fatal("released origin disappeared before coordinator rebuild")
	}
	if err := rebuilt.ImportOrigin(origin, objects, references); err != nil {
		t.Fatal(err)
	}
	if err := applySharedRootMutation(rebuilt, []string{name}, roots, "release", false); err != nil {
		t.Fatalf("release replay after coordinator rebuild: %v", err)
	}
}

func TestSharedSingleReleaseSurvivesCoordinatorReceiptLoss(t *testing.T) {
	ledger := ttcsnapshot.NewReferenceLedger()
	origin := ttcsnapshot.SnapshotID(23)
	object := ttcsnapshot.Object{ID: ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 1}}
	if err := ledger.ImportOrigin(origin, []ttcsnapshot.Object{object}, map[string]ttcsnapshot.ObjectID{"root/source": object.ID}); err != nil {
		t.Fatal(err)
	}
	name := "root/memory/branch-test"
	if err := applySharedReferenceMutation(ledger, "retain-single", name, object.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := applySharedReferenceMutation(ledger, "release-single", name, object.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := applySharedReferenceMutation(ledger, "release-single-replayed", name, object.ID, false); err != nil {
		t.Fatalf("release replay against an absent exact name: %v", err)
	}
	if err := applySharedReferenceMutation(ledger, "retain-conflict", name, object.ID, true); err != nil {
		t.Fatal(err)
	}
	wrong := ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 2}
	if err := applySharedReferenceMutation(ledger, "release-wrong-object", name, wrong, false); err == nil {
		t.Fatal("release of a conflicting live binding succeeded")
	}
}

func TestSharedReferenceBatchesLargeReconciliation(t *testing.T) {
	ledger, err := ttcsnapshot.OpenReferenceLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	origin := ttcsnapshot.SnapshotID(29)
	object := ttcsnapshot.Object{ID: ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 1}}
	references := make(map[string]ttcsnapshot.ObjectID, 2050)
	mutations := make([]ttcsnapshot.SnapshotReferenceMutation, 0, 2050)
	for i := 0; i < 2050; i++ {
		name := fmt.Sprintf("edge/page/%04x", i)
		references[name] = object.ID
		mutations = append(mutations, ttcsnapshot.SnapshotReferenceMutation{
			Operation: fmt.Sprintf("release-page-%04x", i), Name: name, Object: object.ID,
		})
	}
	if err := ledger.ImportOrigin(origin, []ttcsnapshot.Object{object}, references); err != nil {
		t.Fatal(err)
	}
	if err := applySharedReferenceBatches(ledger, mutations); err != nil {
		t.Fatal(err)
	}
	if got := len(ledger.References()); got != 0 {
		t.Fatalf("references after batched release = %d, want 0", got)
	}
}

func TestSnapshotReferenceInventoryReservesEveryUnreleasedGroup(t *testing.T) {
	localRoot := t.TempDir()
	service := &sharedReferenceService{localRoot: localRoot, nodeID: "node-a", incarnation: "boot-a"}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "branch-op", Purpose: graphReferencePurposeBranch,
		Container: "family", Branch: "branch", State: "releasing", CreatedAt: time.Unix(1, 0),
		Groups: []graphReferenceGroup{
			{Origin: 1, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}},
			{Origin: 2, Roots: []cluster.SnapshotRootBinding{{Slot: 1, Object: "0000000000000002-03-0000000000000001"}}, Retained: true},
			{Origin: 3, Roots: []cluster.SnapshotRootBinding{{Slot: 2, Object: "0000000000000003-03-0000000000000001"}}, Retained: true, Released: true},
		},
	}
	path, err := graphReferenceIntentPath(localRoot, intent.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		t.Fatal(err)
	}
	inventory, err := service.SnapshotReferenceInventory(context.Background(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	claims := inventory.Claims
	if inventory.NodeID != "node-a" || inventory.Incarnation != "boot-a" {
		t.Fatalf("inventory identity=%s/%s, want node-a/boot-a", inventory.NodeID, inventory.Incarnation)
	}
	if len(claims) != 2 || claims[0].Origin != 1 || claims[1].Origin != 2 || claims[0].Operation != intent.Operation || len(claims[0].Roots) != 1 || claims[0].Roots[0] != intent.Groups[0].Roots[0] || claims[1].Roots[0] != intent.Groups[1].Roots[0] {
		t.Fatalf("claims=%+v, want unreleased origins 1 and 2", claims)
	}
}

func TestSnapshotReferenceInventoryOmitsRetiredCrashPrefix(t *testing.T) {
	intent := graphReferenceIntent{
		Operation: "release-after-ledger-commit",
		Groups: []graphReferenceGroup{
			{Origin: 10, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "000000000000000a-03-0000000000000001"}}, Retained: true},
			{Origin: 11, Roots: []cluster.SnapshotRootBinding{{Slot: 1, Object: "000000000000000b-03-0000000000000001"}}, Retained: true},
		},
	}
	claims := snapshotReferenceClaims(intent, map[uint64]struct{}{11: {}})
	if len(claims) != 1 || claims[0].Origin != 10 {
		t.Fatalf("claims=%+v, want only non-retired origin 10", claims)
	}
}

func TestDiscardAbandonedSnapshotForgetsImportedEmptyOrigin(t *testing.T) {
	root := t.TempDir()
	packs, err := ttcsnapshot.OpenSharedPackStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ledger := ttcsnapshot.NewReferenceLedger()
	origin := ttcsnapshot.SnapshotID(41)
	// This is the publication crash prefix where the coordinator imported the
	// origin, but cancellation won Raft authority before shared publication.
	if err := ledger.ImportOrigin(origin, nil, nil); err != nil {
		t.Fatal(err)
	}
	namespace := filepath.Join(root, "snapshots", "0000000000000029")
	if err := os.MkdirAll(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, "handoff.tmp"), []byte("speculative"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := discardAbandonedSnapshot(packs, ledger, origin); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Inspect(); len(got.Origins) != 0 {
		t.Fatalf("abandoned origin remains in ledger: %+v", got)
	}
	if _, err := os.Stat(namespace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned handoff namespace stat error=%v, want not-exist", err)
	}
	if err := discardAbandonedSnapshot(packs, ledger, origin); err != nil {
		t.Fatalf("idempotent abandoned cleanup: %v", err)
	}
}

func TestGraphReferenceIntentCrashPrefixes(t *testing.T) {
	for failAt := 0; failAt < 2; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "references", "operation.json")
			intent := graphReferenceIntent{
				Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "operation", Purpose: graphReferencePurposeBranch,
				Container: "family", Branch: "child", State: "retaining", CreatedAt: time.Unix(1, 0),
				Groups: []graphReferenceGroup{
					{Origin: 1, Provider: "node-a", Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}},
					{Origin: 2, Provider: "node-b", Roots: []cluster.SnapshotRootBinding{{Slot: 1, Object: "0000000000000002-03-0000000000000001"}}},
				},
			}
			if err := writeDurableJSON(path, &intent); err != nil {
				t.Fatal(err)
			}
			retained := make(map[uint64]bool)
			calls := 0
			failure := errors.New("injected crash after provider commit")
			err := advanceGraphReferenceRetain(path, &intent, func(group graphReferenceGroup) error {
				retained[group.Origin] = true
				if calls == failAt {
					calls++
					return failure
				}
				calls++
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("first retain error=%v, want injected failure", err)
			}
			recovered, err := readGraphReferenceIntent(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := advanceGraphReferenceRetain(path, &recovered, func(group graphReferenceGroup) error {
				// Replaying the provider operation is deliberately a no-op.
				retained[group.Origin] = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			recovered, err = readGraphReferenceIntent(path)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.State != "active" || len(retained) != 2 {
				t.Fatalf("recovered retain=%+v providers=%+v", recovered, retained)
			}

			recovered.State = "releasing"
			if err := writeDurableJSON(path, &recovered); err != nil {
				t.Fatal(err)
			}
			released := make(map[uint64]bool)
			calls = 0
			err = advanceGraphReferenceRelease(path, &recovered, func(group graphReferenceGroup) error {
				released[group.Origin] = true
				if calls == failAt {
					calls++
					return failure
				}
				calls++
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("first release error=%v, want injected failure", err)
			}
			recovered, err = readGraphReferenceIntent(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := advanceGraphReferenceRelease(path, &recovered, func(group graphReferenceGroup) error {
				released[group.Origin] = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(released) != 2 {
				t.Fatalf("released providers=%+v", released)
			}
		})
	}
}
