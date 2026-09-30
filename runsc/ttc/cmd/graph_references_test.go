// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
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
		{
			name: "provider claim",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeProvider, Transaction: "provider-1", Operation: "provider-1",
			},
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

// A handoff retried after its branch's rename names the branch by its new
// name, and must resume the intent it recorded under the old one.
func TestGraphReferenceIdentitySurvivesRename(t *testing.T) {
	recorded := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "handoff-tx-0000000000000001",
		Purpose: graphReferencePurposeBranch, Container: "family", Branch: "remote1",
		Groups: []graphReferenceGroup{{Origin: 1, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}}},
	}
	retry := recorded
	retry.Branch = "renamed"
	if !graphReferenceIntentEqualIdentity(recorded, retry) {
		t.Fatal("a retry after the branch's rename was refused its own intent")
	}
	retry.Container = "other-family"
	if graphReferenceIntentEqualIdentity(recorded, retry) {
		t.Fatal("another family's operation reused the intent's identity")
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
	if graphReferenceIntentShouldRelease(branch, nil, errors.New("temporary authority failure"), false) {
		t.Fatal("transient transaction lookup failure released a branch intent")
	}
	if !graphReferenceIntentShouldRelease(branch, nil, os.ErrNotExist, false) {
		t.Fatal("missing transaction and branch retained an unreachable intent")
	}
	if graphReferenceIntentShouldRelease(branch, &cluster.Transaction{State: cluster.TransactionIndependent}, nil, true) {
		t.Fatal("live branch intent was released after transaction independence")
	}
}

func TestGraphHandoffIntentIsKeptUntilRecorded(t *testing.T) {
	handoff := graphReferenceIntent{
		Purpose: graphReferencePurposeBranch, State: "active",
		Transaction: "tx", Operation: graphHandoffReferenceOperation("tx", 233),
	}
	if graphReferenceIntentShouldRelease(handoff, &cluster.Transaction{State: cluster.TransactionPublished}, nil, false) {
		t.Fatal("a graph handoff's intent was released before the branch recorded it")
	}
	for _, state := range []cluster.TransactionState{
		cluster.TransactionTargetPaused,
		cluster.TransactionDraining,
		cluster.TransactionIndependent,
		cluster.TransactionCanceled,
		cluster.TransactionFailed,
	} {
		if !graphReferenceIntentShouldRelease(handoff, &cluster.Transaction{State: state}, nil, false) {
			t.Fatalf("orphaned graph handoff intent was retained in %s", state)
		}
	}
	other := handoff
	other.Operation = graphHandoffReferenceOperation("other", 233)
	if !graphReferenceIntentShouldRelease(other, &cluster.Transaction{State: cluster.TransactionPublished}, nil, false) {
		t.Fatal("another transaction's orphaned handoff intent was retained")
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
	ledger := ttcsnapshot.NewReferenceLedger()
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

func TestProviderClaimKeepsTheNodesOwnReferences(t *testing.T) {
	localRoot := t.TempDir()
	service := &sharedReferenceService{localRoot: localRoot, nodeID: "node-a", incarnation: "boot-a"}
	publisher := &branchPublisher{nodeID: "node-a", sharedRefs: service}
	object := func(text string) ttcsnapshot.ObjectID {
		id, err := ttcsnapshot.ParseObjectID(text)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	live := object("0000000000000007-03-0000000000000001")
	pinned := object("0000000000000007-03-0000000000000002")
	references := map[string]ttcsnapshot.ObjectID{
		// Two live branches forked from one another share their roots.
		"root/memory/branch/a/resource-0000": live,
		"root/memory/branch/b/resource-0000": live,
		"pin/observe/reader":                 pinned,
		// A consumer keeps its own claim, and the ledger counts bridge edges.
		"root/remote/7/branch-op/00000000":                      pinned,
		"root/bridge/edge/0000000000000009-03-0000000000000001": pinned,
	}
	path, err := publisher.recordProviderReferences(7, "", references)
	if err != nil || path == "" {
		t.Fatalf("recordProviderReferences = %q, %v", path, err)
	}
	intent, err := readGraphReferenceIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []cluster.SnapshotRootBinding{{Slot: 0, Object: live.String()}, {Slot: 1, Object: pinned.String()}}
	if intent.Purpose != graphReferencePurposeProvider || intent.State != "retaining" || len(intent.Groups) != 1 ||
		intent.Groups[0].Origin != 7 || intent.Groups[0].Provider != "node-a" || fmt.Sprint(intent.Groups[0].Roots) != fmt.Sprint(want) {
		t.Fatalf("claim = %+v, want the two own objects of origin 7", intent)
	}
	if graphReferenceAuthorizationTransaction(intent) != "" {
		t.Fatal("a provider claim names a transaction to authorize")
	}
	// A retried handoff finds the claim it recorded.
	if again, err := publisher.recordProviderReferences(7, "", references); err != nil || again != path {
		t.Fatalf("recording again = %q, %v; want %q", again, err, path)
	}
	// A repository's claim on the same origin is another.
	if other, err := publisher.recordProviderReferences(7, "/repositories/rootfs", references); err != nil || other == "" || other == path {
		t.Fatalf("repository claim = %q, %v", other, err)
	}
	inventory, err := service.SnapshotReferenceInventory(context.Background(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Claims) != 2 || inventory.Claims[0].Origin != 7 || inventory.Claims[1].Origin != 7 {
		t.Fatalf("inventory claims = %+v, want both claims on origin 7", inventory.Claims)
	}
	// A graph with no references of its own claims nothing.
	if none, err := publisher.recordProviderReferences(8, "", map[string]ttcsnapshot.ObjectID{
		"root/remote/8/branch-op/00000000": object("0000000000000008-03-0000000000000001"),
	}); err != nil || none != "" {
		t.Fatalf("claim without own references = %q, %v", none, err)
	}
}

// A capture claim is the node's, is reported to the coordinator like any
// claim, and is kept while the node's memory graph refers to an origin its
// image reaches.
func TestCaptureClaim(t *testing.T) {
	localRoot := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CreateObject(origin, "capture/adopted/0/held/x", ttcsnapshot.ObjectKindMetadata, nil, nil); err != nil {
		t.Fatal(err)
	}
	operation := captureClaimPrefix + "0000000000000007-op"
	path, err := graphReferenceIntentPath(localRoot, operation)
	if err != nil {
		t.Fatal(err)
	}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: operation, Operation: operation,
		Purpose: graphReferencePurposeCapture, State: "active", CreatedAt: time.Now().UTC(),
		Groups:  []graphReferenceGroup{{Origin: 7, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000007-04-0000000000000001"}}, Retained: true}},
		Origins: []uint64{7, uint64(origin)},
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		t.Fatal(err)
	}
	read, err := readGraphReferenceIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	if graphReferenceAuthorizationTransaction(read) != "" {
		t.Fatal("a capture claim names a transaction to authorize")
	}
	service := &sharedReferenceService{localRoot: localRoot, nodeID: "node-a", incarnation: "boot-a"}
	inventory, err := service.SnapshotReferenceInventory(context.Background(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Claims) != 1 || inventory.Claims[0].Origin != 7 || inventory.Claims[0].Operation != operation {
		t.Fatalf("inventory claims = %+v, want the capture claim on origin 7", inventory.Claims)
	}
	for _, malformed := range []func(*graphReferenceIntent){
		func(intent *graphReferenceIntent) { intent.Origins = nil },
		func(intent *graphReferenceIntent) { intent.Container = "family" },
		func(intent *graphReferenceIntent) { intent.Groups = append(intent.Groups, intent.Groups[0]) },
	} {
		bad := read
		bad.Groups = append([]graphReferenceGroup(nil), read.Groups...)
		malformed(&bad)
		badPath := filepath.Join(t.TempDir(), "bad.json")
		if err := writeDurableJSON(badPath, &bad); err != nil {
			t.Fatal(err)
		}
		if _, err := readGraphReferenceIntent(badPath); err == nil {
			t.Fatalf("malformed capture claim %+v was read", bad)
		}
	}
	// Kept while the graph pins what an origin of it holds...
	backend := &runtimeBackend{snapshotGraph: graph, distributedSource: &distributedSourceRuntime{}}
	if err := backend.reconcileCaptureClaim(path, read); err != nil {
		t.Fatal(err)
	}
	if kept, err := readGraphReferenceIntent(path); err != nil || kept.State != "active" {
		t.Fatalf("a needed claim is %+v (%v)", kept, err)
	}
	// ... and checked again only later.
	if err := graph.DeletePin("capture/adopted/0/held/x"); err != nil {
		t.Fatal(err)
	}
	if err := backend.reconcileCaptureClaim(path, read); err != nil {
		t.Fatal(err)
	}
	if kept, err := readGraphReferenceIntent(path); err != nil || kept.State != "active" {
		t.Fatalf("a claim checked before its time is %+v (%v)", kept, err)
	}
	backend.distributedSource.providerChecks.Delete(operation)
	if err := backend.reconcileCaptureClaim(path, read); err != nil {
		t.Fatal(err)
	}
	released, err := readGraphReferenceIntent(path)
	if err != nil || released.State != "releasing" || !released.Groups[0].Retained {
		t.Fatalf("an unneeded claim is %+v (%v), want it releasing", released, err)
	}
}

// A capture shares the image its branch was restored from first, then those
// of its family, each once.
func TestBranchCaptureImages(t *testing.T) {
	info := ttcbundle.ContainerInfo{
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", SnapshotID: 7, MemoryRootObjects: []string{"root-7"}},
			{ID: "fork"},
			{ID: "other", SnapshotID: 9, MemoryRootObjects: []string{"root-9"}},
			{ID: "sibling", SnapshotID: 7, MemoryRootObjects: []string{"root-7"}},
		},
		RuntimeMemoryBackings: []ttcbundle.RuntimeMemoryBacking{{ID: "parked", SnapshotID: 11, MemoryRootObjects: []string{"root-11"}}},
	}
	for _, test := range []struct {
		branch string
		want   []uint64
	}{
		{branch: "other", want: []uint64{9, 7, 11}},
		{branch: "fork", want: []uint64{7, 9, 11}},
	} {
		var got []uint64
		for _, image := range branchCaptureImages(info, test.branch) {
			got = append(got, image.snapshot)
		}
		if fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Errorf("images of %s = %v, want %v", test.branch, got, test.want)
		}
	}
	for i := 0; i < 2*captureImagesLimit; i++ {
		info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: fmt.Sprint(i), SnapshotID: uint64(100 + i), MemoryRootObjects: []string{"root"}})
	}
	if images := branchCaptureImages(info, "fork"); len(images) != captureImagesLimit {
		t.Errorf("a capture shares %d images, want at most %d", len(images), captureImagesLimit)
	}
}

func TestProviderReferencesNeeded(t *testing.T) {
	object := ttcsnapshot.ObjectID{Snapshot: 7}
	for _, test := range []struct {
		name       string
		references map[string]ttcsnapshot.ObjectID
		edges      bool
		want       bool
	}{
		{name: "nothing"},
		{name: "a consumer's claim", references: map[string]ttcsnapshot.ObjectID{"root/remote/7/op/00000000": object}},
		{name: "a bridge edge", references: map[string]ttcsnapshot.ObjectID{"root/bridge/edge/x/0001": object}},
		{name: "a root", references: map[string]ttcsnapshot.ObjectID{"root/memory/branch/a/resource-0000": object}, want: true},
		{name: "a pin", references: map[string]ttcsnapshot.ObjectID{"pin/observe/x": object}, want: true},
		{name: "an edge", edges: true, want: true},
	} {
		if got := providerReferencesNeeded(test.references, test.edges); got != test.want {
			t.Errorf("%s: providerReferencesNeeded = %t, want %t", test.name, got, test.want)
		}
	}
}

func TestConsumerClaimRoot(t *testing.T) {
	for root, want := range map[string]bool{
		"remote/7/branch-op/00000000": true,
		"remote/7/branch-op/0000000a": true,
		// A live export's capture roots are no claim.
		"remote/7/tx/resource-0000":         false,
		"remote/8/branch-op/00000000":       false,
		"remote/7/branch-op/0000000g":       false,
		"remote/7//00000000":                false,
		"memory/branch/a/resource-0000":     false,
		"remote/7/branch-op/00000000/extra": false,
	} {
		if got := consumerClaimRoot(root, 7); got != want {
			t.Errorf("consumerClaimRoot(%q, 7) = %t, want %t", root, got, want)
		}
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
	// origin, but cancellation won authority before shared publication.
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

// testGraphOriginPublisher returns a publisher of a new graph, and the
// graph's one origin, sealed and rooted. The publisher's authority has no
// cluster authority, which the tests using it never reach.
func testGraphOriginPublisher(t *testing.T) (*branchPublisher, ttcsnapshot.SnapshotID) {
	t.Helper()
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { graph.Close() })
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	child, err := graph.CreateObject(origin, "build/child", ttcsnapshot.ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/main", child); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/child"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	return &branchPublisher{
		sharedRoot: t.TempDir(), nodeID: "node-a", incarnation: "boot-a",
		authority: &cluster.AuthorityCatalog{}, graph: graph, graphShared: &ttcsnapshot.SharedPackStore{},
		sharedRefs: &sharedReferenceService{}, peers: &distributedSourceRuntime{},
	}, origin
}

// freezeForBranchPublication freezes origin's ledger as a branch publication
// does, and records the publication's bridge.
func freezeForBranchPublication(t *testing.T, publisher *branchPublisher, origin ttcsnapshot.SnapshotID) {
	t.Helper()
	if _, _, err := publisher.graph.FreezeSnapshotLedger(origin); err != nil {
		t.Fatal(err)
	}
	if err := persistSharedPublicationBridgeIntent(publisher.sharedRoot, sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: uint64(origin), Kind: sharedPublicationBridgePublication,
		Family: "family", Branch: "main", PublicationID: "publication", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeFrozenGraphOriginLeavesABranchPublicationItsOwn(t *testing.T) {
	publisher, origin := testGraphOriginPublisher(t)
	if err := publisher.resumeFrozenGraphOrigin(context.Background(), origin); err != nil {
		t.Fatalf("resuming an origin that is not frozen: %v", err)
	}
	freezeForBranchPublication(t, publisher, origin)
	if err := publisher.resumeFrozenGraphOrigin(context.Background(), origin); !errors.Is(err, errFrozenOriginOwned) {
		t.Fatalf("resuming a branch publication's origin = %v, want %v", err, errFrozenOriginOwned)
	}
	if !publisher.graph.SnapshotLedgerFrozen(origin) {
		t.Fatal("the resume released a branch publication's freeze")
	}
}

// One roll-forward at a time completes an origin's frozen handoff, and it
// ends once the handoff is not its to complete.
func TestFrozenOriginRollsForwardOnce(t *testing.T) {
	publisher, origin := testGraphOriginPublisher(t)
	freezeForBranchPublication(t, publisher, origin)
	// Holding the origin keeps the first roll-forward waiting.
	unlock := publisher.peers.lockSnapshotOrigin(origin)
	publisher.rollForwardFrozenOrigin(origin)
	publisher.rollForwardFrozenOrigin(origin)
	if _, running := publisher.peers.rollingForward.Load(origin); !running {
		unlock()
		t.Fatal("no roll-forward is running")
	}
	unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, running := publisher.peers.rollingForward.Load(origin); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the roll-forward of a branch publication's origin did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !publisher.graph.SnapshotLedgerFrozen(origin) {
		t.Fatal("the roll-forward released a branch publication's freeze")
	}
}

// A repository that expanded part of a tree through an earlier relay asks a
// new relay for objects below ones this relay has not served, and in origins
// published before the tree's own: the relay finds them below its root.
func TestSharedSnapshotReaderFindsObjectsBelowItsRoot(t *testing.T) {
	directory := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	local, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(directory, "local"))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(directory, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("file contents")
	file, err := graph.ReserveObject(origin, ttcsnapshot.ObjectKindFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Install(file, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(file, "construction/file", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	directoryObject, err := graph.CreateObject(origin, "construction/directory", ttcsnapshot.ObjectKindMetadata, []byte("directory"), []ttcsnapshot.Edge{{Slot: 0, Child: file}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := graph.CreateObject(origin, "construction/root", ttcsnapshot.ObjectKindMetadata, []byte("root"), []ttcsnapshot.Edge{{Slot: 0, Child: directoryObject}})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := graph.CreateObject(origin, "construction/unrelated", ttcsnapshot.ObjectKindMetadata, []byte("unrelated"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, ttcsnapshot.SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	reader, err := newSharedSnapshotObjectReader(shared, root.String())
	if err != nil {
		t.Fatal(err)
	}
	// The tree's publication made none of its origins.
	reader.allowedOrigins = map[ttcsnapshot.SnapshotID]struct{}{}
	got := make([]byte, len(data))
	if n, err := reader.ReadObjectRange(file.String(), got, 0); err != nil || !bytes.Equal(got[:n], data) {
		t.Fatalf("reading a file below the root, unserved: %q, %v", got[:n], err)
	}
	if _, err := reader.RemoteObject(unrelated); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("an object outside the tree: %v, want permission denied", err)
	}
}

// A publication that changed nothing of its tree hands off no origin, and a
// relay of that tree, as the next publication and a rehydration start, has
// an empty scope: it finds every object below the root.
func TestSharedRelayOriginScope(t *testing.T) {
	if origins, err := parseSharedRelayOriginScope(""); err != nil || len(origins) != 0 {
		t.Fatalf("empty scope = (%v, %v), want no origins", origins, err)
	}
	origins, err := parseSharedRelayOriginScope(fmt.Sprintf("%016x,%016x", 3, 0x2a))
	if err != nil || len(origins) != 2 {
		t.Fatalf("scope of two origins = (%v, %v)", origins, err)
	}
	for _, origin := range []ttcsnapshot.SnapshotID{3, 0x2a} {
		if _, ok := origins[origin]; !ok {
			t.Fatalf("scope %v lacks origin %d", origins, origin)
		}
	}
	for _, invalid := range []string{"0000000000000000", "zz", "3,", ","} {
		if _, err := parseSharedRelayOriginScope(invalid); err == nil {
			t.Fatalf("scope %q was accepted", invalid)
		}
	}
}

// A claim released while a coordinator reads the node's claims is gone from
// the inventory, not an error: its file goes once the ledger let go of it.
func TestSnapshotReferenceInventoryPassesOverReleasedClaims(t *testing.T) {
	localRoot := t.TempDir()
	dir := filepath.Join(localRoot, "graph-references")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A name listed whose file is gone by the time it is read.
	if err := os.Symlink(filepath.Join(dir, "released"), filepath.Join(dir, "provider-0000000000000007-memory-x.json")); err != nil {
		t.Fatal(err)
	}
	service := &sharedReferenceService{localRoot: localRoot, nodeID: "node-a", incarnation: "boot-a"}
	inventory, err := service.SnapshotReferenceInventory(context.Background(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Claims) != 0 {
		t.Fatalf("inventory claims = %+v", inventory.Claims)
	}
}
