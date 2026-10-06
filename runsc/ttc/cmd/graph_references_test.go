// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
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
			name: "fork of a branch no transaction produced",
			intent: graphReferenceIntent{
				Purpose: graphReferencePurposeBranch, Operation: "branch-2",
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
	branch := graphReferenceIntent{Purpose: graphReferencePurposeBranch, State: "active", Transaction: "fork-1", Operation: "fork-1"}
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

// A local fork's references, whether or not a transaction brought its source
// here, are kept exactly as long as the fork is: rehydrated from a durable
// image, a branch has no transaction, and a transaction that brought its
// ancestor may be canceled or retired while the fork lives on.
func TestDescendantGraphReferenceIntentFollowsItsBranch(t *testing.T) {
	for _, intent := range []graphReferenceIntent{
		{Purpose: graphReferencePurposeBranch, State: "active", Transaction: "fork-1", Operation: "branch-2"},
		{Purpose: graphReferencePurposeBranch, State: "active", Operation: "branch-3"},
	} {
		for _, tx := range []*cluster.Transaction{
			nil,
			{State: cluster.TransactionPublished},
			{State: cluster.TransactionIndependent},
			{State: cluster.TransactionCanceled},
			{State: cluster.TransactionFailed},
		} {
			for _, txErr := range []error{nil, os.ErrNotExist} {
				if graphReferenceIntentShouldRelease(intent, tx, txErr, true) {
					t.Fatalf("intent %+v of a live branch released with transaction %+v (%v)", intent, tx, txErr)
				}
				if !graphReferenceIntentShouldRelease(intent, tx, txErr, false) {
					t.Fatalf("intent %+v of a removed branch kept with transaction %+v (%v)", intent, tx, txErr)
				}
			}
		}
	}
	info := ttcbundle.ContainerInfo{
		Branches: []ttcbundle.BranchInfo{{ID: "c", DistributedGraphReferenceOperation: "branch-3"}},
		RuntimeMemoryBackings: []ttcbundle.RuntimeMemoryBacking{{
			ID: "parked", DistributedGraphReferenceOperation: "fork-1",
		}},
	}
	if !containerHasGraphReferenceIntent(info, graphReferenceIntent{Operation: "branch-3"}) {
		t.Fatal("a branch that no transaction produced does not own its intent")
	}
	if !containerHasGraphReferenceIntent(info, graphReferenceIntent{Transaction: "fork-1", Operation: "fork-1"}) {
		t.Fatal("a parked runtime backing does not own the intent its branch did")
	}
	if containerHasGraphReferenceIntent(info, graphReferenceIntent{Operation: "branch-4"}) {
		t.Fatal("an intent nothing records is owned")
	}
	group := []graphReferenceGroup{{Origin: 1, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}}}
	for _, test := range []struct {
		purpose string
		valid   bool
	}{
		{purpose: graphReferencePurposeBranch, valid: true},
		{purpose: graphReferencePurposeTransfer},
	} {
		path := filepath.Join(t.TempDir(), "intent.json")
		intent := graphReferenceIntent{
			Version: graphReferenceIntentVersion, Operation: "branch-3", Purpose: test.purpose,
			Container: "family", Branch: "c", State: "active", Groups: group, CreatedAt: time.Now().UTC(),
		}
		if err := writeDurableJSON(path, &intent); err != nil {
			t.Fatal(err)
		}
		if _, err := readGraphReferenceIntent(path); (err == nil) != test.valid {
			t.Fatalf("reading a %s intent without a transaction: %v, want valid %t", test.purpose, err, test.valid)
		}
	}
}

// The reconciler loads no transaction for an intent that none authorizes,
// and keeps the intents that a branch or a parked runtime backing owns.
func TestReconcileKeepsOwnedGraphReferenceIntents(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "family")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "family", Name: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{
			{ID: "c", DistributedGraphReferenceOperation: "branch-c"},
			{ID: "d", DistributedForkTransactionID: "fork-1", DistributedGraphReferenceOperation: "branch-d"},
		},
		RuntimeMemoryBackings: []ttcbundle.RuntimeMemoryBacking{{
			ID: "parked", SnapshotID: 1, MemoryRoots: []string{"root"}, MemoryRootObjects: []string{"0000000000000001-03-0000000000000001"},
			DistributedGraphReferenceOperation: "fork-2",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	group := []graphReferenceGroup{{Origin: 1, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000001-03-0000000000000001"}}, Retained: true}}
	intents := []graphReferenceIntent{
		{Operation: "branch-c", Branch: "c"},
		{Transaction: "fork-1", Operation: "branch-d", Branch: "d"},
		{Transaction: "fork-2", Operation: "fork-2", Branch: "parked"},
	}
	paths := make([]string, len(intents))
	for i := range intents {
		intents[i].Version, intents[i].Purpose, intents[i].Container = graphReferenceIntentVersion, graphReferencePurposeBranch, "family"
		intents[i].State, intents[i].Groups, intents[i].CreatedAt = "active", group, time.Now().UTC()
		path, err := graphReferenceIntentPath(root, intents[i].Operation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeDurableJSON(path, &intents[i]); err != nil {
			t.Fatal(err)
		}
		paths[i] = path
	}
	var loaded []string
	backend := &runtimeBackend{dataRoot: root, distributedSource: &distributedSourceRuntime{
		transactions: transactionLoadCatalog{load: func(id string) (*cluster.Transaction, error) {
			loaded = append(loaded, id)
			// The transaction that brought the parked branch here is gone
			// from the authority's history.
			return nil, os.ErrNotExist
		}},
	}}
	if err := backend.reconcileDistributedGraphReferences(true); err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0] != "fork-2" {
		t.Fatalf("loaded transactions %q, want only the parked import's", loaded)
	}
	for _, path := range paths {
		if intent, err := readGraphReferenceIntent(path); err != nil || intent.State != "active" {
			t.Fatalf("owned intent %s is %+v (%v)", filepath.Base(path), intent, err)
		}
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

// A publication asks its node's coordinator election to try for the lease at
// once without waiting on it, however often it asks; the election hears it
// once, and a node without a service is asked nothing.
func TestCoordinatorElectionIsAskedWithoutWaiting(t *testing.T) {
	s := newSharedReferenceService(t.TempDir(), t.TempDir(), "node-a", "incarnation-a", nil, nil, nil, nil)
	for i := 0; i < 3; i++ {
		s.electCoordinator()
	}
	select {
	case <-s.elections():
	default:
		t.Fatal("the election was not asked")
	}
	select {
	case <-s.elections():
		t.Fatal("asked three times at once, the election heard it twice")
	default:
	}
	if s.activeFor(cluster.SharedGCLease{Generation: 1}) {
		t.Error("a service whose ledger is not open is active")
	}
	var none *sharedReferenceService
	none.electCoordinator()
	if none.elections() != nil || none.activeFor(cluster.SharedGCLease{Generation: 1}) {
		t.Error("no service has an election or an open ledger")
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

// A catalog claim is the node's, is reported to the coordinator like any
// claim, and is kept while a branch that its family places on the node names
// its catalog in its repository.
func TestCatalogClaim(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	const catalog = "repo:0000000000000007-03-0000000000000001:9"
	writeBranch := func(id, catalog string, inherited bool) {
		t.Helper()
		data := fmt.Sprintf(`{"id":%q,"remote_catalog":%q,"remote_inherited":%t}`, id, catalog, inherited)
		if err := os.MkdirAll(filepath.Join(repository, "branches"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, "branches", id+".json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	containerDir := filepath.Join(root, "containers", "family")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	repositories := fmt.Sprintf(`[{"source":"/upper","path":%q,"branch":"main","copy_mode":"full-copy","private":true}]`, repository)
	config, err := json.Marshal(map[string]any{"annotations": map[string]string{specutils.AnnotationTTCVolumeRepositories: repositories}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "family", Name: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{
			{ID: "c", NodeID: "node-a"},
			{ID: "p", Unplaced: true, StorageState: "shared"},
			{ID: "e", NodeID: "node-b"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	writeBranch("c", catalog, true)
	writeBranch("p", catalog, false)
	writeBranch("e", catalog, true)
	operation := catalogClaimPrefix + "0000000000000007-op"
	path, err := graphReferenceIntentPath(root, operation)
	if err != nil {
		t.Fatal(err)
	}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: operation, Operation: operation,
		Purpose: graphReferencePurposeCatalog, Container: "family", State: "active", CreatedAt: time.Now().UTC(),
		Groups:     []graphReferenceGroup{{Origin: 7, Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: "0000000000000007-03-0000000000000001"}}, Retained: true}},
		Repository: repository, Catalog: catalog,
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		t.Fatal(err)
	}
	read, err := readGraphReferenceIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	if graphReferenceAuthorizationTransaction(read) != "" {
		t.Fatal("a catalog claim names a transaction to authorize")
	}
	for _, malformed := range []func(*graphReferenceIntent){
		func(intent *graphReferenceIntent) { intent.Catalog = "" },
		func(intent *graphReferenceIntent) { intent.Repository = "" },
		func(intent *graphReferenceIntent) { intent.Container = "" },
		func(intent *graphReferenceIntent) { intent.Branch = "c" },
		func(intent *graphReferenceIntent) {
			intent.Groups[0].Roots = append(intent.Groups[0].Roots, intent.Groups[0].Roots[0])
		},
	} {
		bad := read
		bad.Groups = []graphReferenceGroup{read.Groups[0]}
		bad.Groups[0].Roots = append([]cluster.SnapshotRootBinding(nil), read.Groups[0].Roots...)
		malformed(&bad)
		badPath := filepath.Join(t.TempDir(), "bad.json")
		if err := writeDurableJSON(badPath, &bad); err != nil {
			t.Fatal(err)
		}
		if _, err := readGraphReferenceIntent(badPath); err == nil {
			t.Fatalf("malformed catalog claim %+v was read", bad)
		}
	}
	service := &sharedReferenceService{localRoot: root, nodeID: "node-a", incarnation: "boot-a"}
	inventory, err := service.SnapshotReferenceInventory(context.Background(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Claims) != 1 || inventory.Claims[0].Origin != 7 || inventory.Claims[0].Operation != operation {
		t.Fatalf("inventory claims = %+v, want the catalog claim on origin 7", inventory.Claims)
	}
	backend := &runtimeBackend{dataRoot: root, nodeID: "node-a", distributedSource: &distributedSourceRuntime{}}
	// A second fork finds the claim.
	if err := backend.claimCatalog("family", repository, catalog, ttcsnapshot.ObjectID{Snapshot: 7, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Fatalf("claims after a second fork: %v (%v), want the one", entries, err)
	}
	// Kept while the fork placed here names the catalog it inherited...
	if err := backend.reconcileCatalogClaim(path, read); err != nil {
		t.Fatal(err)
	}
	if kept, err := readGraphReferenceIntent(path); err != nil || kept.State != "active" {
		t.Fatalf("a needed claim is %+v (%v)", kept, err)
	}
	// ... but not once it is published, nor for the branch that imported the
	// image, a copy of the unplaced source, nor for a branch placed on another
	// node.
	writeBranch("c", catalog, false)
	writeBranch("p", catalog, true)
	backend.distributedSource.providerChecks.Delete(operation)
	if needed, err := backend.catalogClaimNeeded(read); err != nil || needed {
		t.Fatalf("claim needed=%t (%v) once only other nodes' branches name the catalog", needed, err)
	}
	if err := backend.reconcileCatalogClaim(path, read); err != nil {
		t.Fatal(err)
	}
	released, err := readGraphReferenceIntent(path)
	if err != nil || released.State != "releasing" || !released.Groups[0].Retained {
		t.Fatalf("an unneeded claim is %+v (%v), want it releasing", released, err)
	}
}

// A fork claims the shared image its branch expands, but not a remote fork's
// or migration's source, which the transaction keeps while the target needs
// it and retires once it is over.
func TestForkCatalogClaimed(t *testing.T) {
	gone := fmt.Errorf("origins: %w", os.ErrNotExist)
	for _, test := range []struct {
		name     string
		branch   ttcvolume.Branch
		volatile bool
		err      error
		claim    bool
		fails    bool
	}{
		{name: "shared image", branch: ttcvolume.Branch{RemoteShared: true}, claim: true},
		{name: "transaction source", volatile: true},
		{name: "ended transaction's source", err: gone},
		{name: "transaction source published since"},
		{name: "retired shared image", branch: ttcvolume.Branch{RemoteShared: true}, err: gone, fails: true},
		{name: "unreachable authority", branch: ttcvolume.Branch{RemoteShared: true}, err: errors.New("authority unavailable"), fails: true},
	} {
		claim, err := forkCatalogClaimed(test.branch, test.volatile, test.err)
		if claim != test.claim || (err != nil) != test.fails {
			t.Errorf("%s: claim=%t err=%v, want claim=%t failing=%t", test.name, claim, err, test.claim, test.fails)
		}
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

// A stale origin whose building ends while the repositories are listed, as a
// rehydration's does once it has installed the origin, which the listing may
// miss, is not an orphan: which origins are building is read first.
func TestStaleOriginOrphansReadsBuildingFirst(t *testing.T) {
	building := map[uint64]bool{7: true}
	inventory := func() (map[ttcsnapshot.SnapshotID]struct{}, error) {
		listing := map[ttcsnapshot.SnapshotID]struct{}{9: {}}
		// Origin 7 is installed in a repository after the listing, and then
		// stops building.
		building[7] = false
		return listing, nil
	}
	live := func(origin uint64, filesystem map[ttcsnapshot.SnapshotID]struct{}) (bool, error) {
		if building[origin] {
			return true, nil
		}
		_, ok := filesystem[ttcsnapshot.SnapshotID(origin)]
		return ok, nil
	}
	orphans, err := staleOriginOrphans([]uint64{7, 8, 9}, func(origin uint64) bool { return building[origin] }, inventory, live)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orphans, []uint64{8}) {
		t.Fatalf("orphans %v, want [8]", orphans)
	}
	if orphans, err := staleOriginOrphans(nil, nil, nil, nil); err != nil || orphans != nil {
		t.Fatalf("no locked origin gave orphans %v, %v", orphans, err)
	}
}
