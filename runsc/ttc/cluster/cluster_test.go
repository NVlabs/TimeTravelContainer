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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcremote"
)

func testCapabilities(node string) Capabilities {
	return Capabilities{
		NodeID: node, ProtocolMin: 1, ProtocolMax: 2, BuildID: "test-build",
		Architecture: "amd64", BasePageSize: 4096, MaxTransferSize: 2 << 20,
		RepositoryID: "shared-repository",
		Features: []string{
			FeatureRemotePull, FeatureReversePush, FeatureAckBatch,
			FeatureNetworkRecreate, FeatureRuntimePrefetch, FeatureFilesystemBranch,
			FeatureSharedMemorySnapshot,
		},
	}
}

func TestNegotiateCapabilities(t *testing.T) {
	source, target := testCapabilities("source"), testCapabilities("target")
	source.PagingPolicy = ttcremote.DefaultPolicy()
	target.PagingPolicy = ttcremote.DefaultPolicy()
	source.PagingPolicy.Push = ttcremote.PushEagerDrain
	target.PagingPolicy.TransferBytes = 128 << 10
	target.PagingPolicy.MaxOutstandingBytes = 256 << 10
	target.PagingPolicy.MaxConcurrency = 16
	target.ProtocolMax = 1
	target.MaxTransferSize = 64 << 10
	target.Features = append(append([]string(nil), target.Features[:1]...), target.Features[2:]...)
	negotiated, err := Negotiate(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if negotiated.Protocol != 1 || negotiated.MaxTransferSize != 64<<10 || len(negotiated.Features) != len(target.Features) ||
		negotiated.PagingPolicy.TransferBytes != 64<<10 || negotiated.PagingPolicy.MaxConcurrency != 4 ||
		negotiated.PagingPolicy.Push != ttcremote.PushEagerDrain {
		t.Fatalf("negotiated=%+v", negotiated)
	}
}

func TestLegacyCapabilitiesReceiveDefaultPagingPolicy(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := negotiated.PagingPolicy.TransferBytes, uint64(64<<10); got != want {
		t.Fatalf("transfer bytes=%d, want %d", got, want)
	}
	if got, want := negotiated.PagingPolicy.PageSize, uint64(4096); got != want {
		t.Fatalf("page size=%d, want %d", got, want)
	}
}

func TestNegotiateRejectsRepositoryAndRequiredFeatureMismatch(t *testing.T) {
	source, target := testCapabilities("source"), testCapabilities("target")
	target.RepositoryID = "other"
	if _, err := Negotiate(source, target); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("repository mismatch error=%v", err)
	}
	target.RepositoryID = source.RepositoryID
	target.Features = target.Features[:len(target.Features)-2]
	if _, err := Negotiate(source, target); err == nil || !strings.Contains(err.Error(), FeatureFilesystemBranch) {
		t.Fatalf("feature mismatch error=%v", err)
	}
}

func TestRemoteSessionDescriptorSharedSnapshotPath(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	base := RemoteSessionDescriptor{
		Address: "127.0.0.1:1", SnapshotToken: strings.Repeat("11", 16),
		MetadataBytes: 1, StreamBytes: 4096, MaxTransferBytes: 4096,
		SharedSnapshot: "cluster/memory-snapshots/tx.mem",
	}
	if err := base.validate(negotiated); err != nil {
		t.Fatalf("valid shared snapshot: %v", err)
	}
	for _, path := range []string{"/tmp/tx.mem", "../tx.mem", "cluster/../tx.mem", "."} {
		invalid := base
		invalid.SharedSnapshot = path
		if err := invalid.validate(negotiated); err == nil {
			t.Errorf("accepted invalid shared snapshot path %q", path)
		}
	}
}

func TestRemoteSessionDescriptorSparseRepository(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	base := RemoteSessionDescriptor{
		Address: "127.0.0.1:1", SnapshotToken: strings.Repeat("11", 16),
		MetadataBytes: 1, StreamBytes: 4096, MaxTransferBytes: 4096,
		SparseRepository: "containers/family/memory/chunks", SparseManifest: strings.Repeat("ab", 32), SparseBranch: "b-remote",
	}
	if err := base.validate(negotiated); err != nil {
		t.Fatalf("valid sparse repository: %v", err)
	}
	for _, mutate := range []func(*RemoteSessionDescriptor){
		func(d *RemoteSessionDescriptor) { d.SparseManifest = "" },
		func(d *RemoteSessionDescriptor) { d.SparseBranch = "" },
		func(d *RemoteSessionDescriptor) { d.SparseBranch = "../remote" },
		func(d *RemoteSessionDescriptor) { d.SparseManifest = "xyz" },
		func(d *RemoteSessionDescriptor) { d.SparseRepository = "../chunks" },
		func(d *RemoteSessionDescriptor) { d.SharedSnapshot = "cluster/snapshot.mem" },
	} {
		invalid := base
		mutate(&invalid)
		if err := invalid.validate(negotiated); err == nil {
			t.Fatalf("accepted invalid sparse descriptor: %+v", invalid)
		}
	}
}

func testForkIdentity() ForkIdentity {
	digest := "sha256:" + strings.Repeat("1", 64)
	return ForkIdentity{
		TransactionID: "tx-1", SourceNodeID: "source", TargetNodeID: "target",
		SourceContainer: "app", SourceBranch: "main", TargetContainer: "app-copy", TargetBranch: "main",
		ImageDigest: digest, RootfsDigest: digest, RepositoryID: "shared-repository",
	}
}

func TestTransactionLifecycleAndIdempotency(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	tx, err := NewTransaction(testForkIdentity(), negotiated, now)
	if err != nil {
		t.Fatal(err)
	}
	states := []TransactionState{
		TransactionTargetReserved, TransactionSourceCaptured, TransactionTargetRestoring,
		TransactionTargetPaused, TransactionPublished, TransactionDraining, TransactionIndependent,
	}
	for i, state := range states {
		now = now.Add(time.Second)
		if err := tx.Advance(state, "", now); err != nil {
			t.Fatalf("transition %d to %s: %v", i, state, err)
		}
		before := tx.Revision
		if err := tx.Advance(state, "", now); err != nil || tx.Revision != before {
			t.Fatalf("idempotent transition to %s: revision=%d err=%v", state, tx.Revision, err)
		}
	}
	if !tx.Terminal() || tx.SourceNeeded {
		t.Fatalf("terminal transaction=%+v", tx)
	}
	if err := tx.Advance(TransactionPublished, "", now.Add(time.Second)); err == nil {
		t.Fatal("terminal transaction accepted a backward transition")
	}
}

func TestTransactionFailureRequiresDiagnostic(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := NewTransaction(testForkIdentity(), negotiated, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Advance(TransactionFailed, "", time.Unix(1001, 0)); err == nil {
		t.Fatal("failed transition without diagnostic succeeded")
	}
	if err := tx.Advance(TransactionFailed, "target launch failed", time.Unix(1001, 0)); err != nil {
		t.Fatal(err)
	}
	if !tx.Terminal() || tx.SourceNeeded || tx.Failure == "" {
		t.Fatalf("failed transaction=%+v", tx)
	}
}

func TestTransactionStoreLifecycleAndRevisionChecks(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	tx, err := NewTransaction(testForkIdentity(), negotiated, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(tx); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(tx); err == nil {
		t.Fatal("duplicate transaction create succeeded")
	}
	if _, err := store.Advance(tx.Identity.TransactionID, 999, TransactionTargetReserved, "", now.Add(time.Second)); err == nil {
		t.Fatal("stale transaction revision succeeded")
	}
	stored, err := store.Advance(tx.Identity.TransactionID, 1, TransactionTargetReserved, "", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != TransactionTargetReserved || stored.Revision != 2 {
		t.Fatalf("advanced transaction=%+v", stored)
	}
	bound, err := store.BindSession(stored.Identity.TransactionID, stored.Revision, "sha256:"+strings.Repeat("a", 64), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != TransactionSourceCaptured || bound.SessionDigest == "" {
		t.Fatalf("bound transaction=%+v", bound)
	}
	// Reopening the store must observe the atomically replaced record.
	reopened, err := OpenStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(tx.Identity.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != bound.State || loaded.Revision != bound.Revision {
		t.Fatalf("reloaded=%+v, want %+v", loaded, bound)
	}
	if err := reopened.Delete(loaded.Identity.TransactionID, loaded.Revision); err == nil {
		t.Fatal("deleted a nonterminal transaction")
	}
	failed, err := reopened.Advance(loaded.Identity.TransactionID, loaded.Revision, TransactionFailed, "injected", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Delete(failed.Identity.TransactionID, failed.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(failed.Identity.TransactionID); err == nil {
		t.Fatal("deleted transaction remained loadable")
	}
}

func TestTransactionStoreListsInStableOrder(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tx-z", "tx-a"} {
		identity := testForkIdentity()
		identity.TransactionID = id
		tx, err := NewTransaction(identity, negotiated, time.Unix(1000, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Create(tx); err != nil {
			t.Fatal(err)
		}
	}
	transactions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 || transactions[0].Identity.TransactionID != "tx-a" || transactions[1].Identity.TransactionID != "tx-z" {
		t.Fatalf("transactions=%+v", transactions)
	}
}
