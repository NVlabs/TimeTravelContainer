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
		"remote/1/op/00000000": firstObject,
		"remote/2/op/00000000": secondObject,
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
	if err := releasePublishedProviderGraphRoots(graph, nil, first); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root("remote/1/op/00000000"); ok {
		t.Fatal("published provider root remains")
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
	if !graphReferenceIntentShouldRelease(branch, &cluster.Transaction{State: cluster.TransactionPublished}, nil, false) {
		t.Fatal("orphaned branch intent was retained")
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
	} {
		if !isSharedReferenceCoordinatorTransition(errors.New(message)) {
			t.Fatalf("%q was not classified as coordinator turnover", message)
		}
	}
	if isSharedReferenceCoordinatorTransition(errors.New("snapshot root is missing")) {
		t.Fatal("permanent graph corruption was classified as coordinator turnover")
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
