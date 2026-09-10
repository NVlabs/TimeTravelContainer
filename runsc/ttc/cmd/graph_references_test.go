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
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestGraphReferenceOperationIsBranchScoped(t *testing.T) {
	info := ttcbundle.ContainerInfo{DistributedForkTransactionID: "family-transaction"}
	initial := ttcbundle.BranchInfo{ID: "remote"}
	if got := graphReferenceOperationForBranch(info, initial); got != "family-transaction" {
		t.Fatalf("initial operation=%q, want transaction fallback", got)
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
		Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "op", Container: "family", Branch: "branch",
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

func TestGraphReferenceIntentCrashPrefixes(t *testing.T) {
	for failAt := 0; failAt < 2; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "references", "operation.json")
			intent := graphReferenceIntent{
				Version: graphReferenceIntentVersion, Transaction: "tx", Operation: "operation",
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
