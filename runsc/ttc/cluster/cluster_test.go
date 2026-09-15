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
	policy := ttcremote.DefaultPolicy()
	policy.PageSize = 4096
	return Capabilities{
		NodeID: node, ProtocolMin: 1, ProtocolMax: 2, BuildID: "test-build",
		Architecture: "amd64", CPUProfile: "portable", CPUFeatures: []string{"sse", "sse2"}, BasePageSize: 4096, MaxTransferSize: 2 << 20,
		RepositoryID: "shared-repository",
		Features: []string{
			FeatureRemotePull, FeatureReversePush, FeatureAckBatch,
			FeatureNetworkRecreate, FeatureRuntimePrefetch, FeatureFilesystemBranch,
			FeatureSharedMemorySnapshot,
		},
		PagingPolicy: policy,
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

func TestCapabilitiesRequireExplicitPagingPolicy(t *testing.T) {
	capabilities := testCapabilities("source")
	capabilities.PagingPolicy = ttcremote.Policy{}
	if err := capabilities.Validate(); err == nil {
		t.Fatal("capabilities without an explicit paging policy were accepted")
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

func TestNegotiateRejectsCPUProfileMismatch(t *testing.T) {
	source, target := testCapabilities("source"), testCapabilities("target")
	target.CPUFeatures = []string{"avx512f", "sse", "sse2"}
	if _, err := Negotiate(source, target); err == nil || !strings.Contains(err.Error(), "CPU profile") {
		t.Fatalf("CPU feature mismatch error=%v", err)
	}
	target = testCapabilities("target")
	target.CPUProfile = "host"
	if _, err := Negotiate(source, target); err == nil || !strings.Contains(err.Error(), "CPU profile") {
		t.Fatalf("CPU profile-name mismatch error=%v", err)
	}
}

func TestRemoteSessionDescriptorGraphMetadata(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	base := RemoteSessionDescriptor{
		MetadataBytes: 128, StreamBytes: 1 << 20, MaxTransferBytes: 4096,
		SharedMetadata: "cluster/memory-metadata/tx.state",
		GraphManifest:  "cluster/memory-manifests/tx.manifest", GraphSnapshot: 7,
		GraphRootObjects: []string{"0000000000000007-03-0000000000000001"}, DurableDehydration: true,
	}
	if err := base.validate(negotiated); err != nil {
		t.Fatalf("valid graph metadata descriptor: %v", err)
	}
	for _, mutate := range []func(*RemoteSessionDescriptor){
		func(d *RemoteSessionDescriptor) { d.GraphManifest = "../manifest" },
		func(d *RemoteSessionDescriptor) { d.SharedMetadata = "" },
		func(d *RemoteSessionDescriptor) { d.DurableDehydration = false },
		func(d *RemoteSessionDescriptor) { d.GraphSnapshot = 0 },
		func(d *RemoteSessionDescriptor) { d.GraphRootObjects = nil },
	} {
		invalid := base
		mutate(&invalid)
		if err := invalid.validate(negotiated); err == nil {
			t.Fatalf("accepted invalid graph descriptor: %+v", invalid)
		}
	}
}

func TestRemoteSessionDescriptorSharedMemoryWithVolatileFilesystem(t *testing.T) {
	negotiated, err := Negotiate(testCapabilities("source"), testCapabilities("target"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := RemoteSessionDescriptor{
		Address: "127.0.0.1:1234", SnapshotToken: strings.Repeat("11", 16),
		MetadataBytes: 128, StreamBytes: 4096, MaxTransferBytes: 4096,
		SharedMetadata: "cluster/memory-metadata/tx.state", DurableDehydration: true,
		GraphManifest: "cluster/memory-manifests/tx.manifest", GraphSnapshot: 7,
		GraphRootObjects: []string{"0000000000000007-03-0000000000000001"},
		FilesystemRoots: []FilesystemRootDescriptor{{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "fedcba9876543210fedcba9876543210", BranchID: "branch-a",
			Root: "0000000000000008-07-0000000000000001", NextInode: 2, LogicalBytes: 1 << 20,
		}}, FilesystemBytes: 1 << 20,
	}
	if err := descriptor.validate(negotiated); err != nil {
		t.Fatalf("valid shared-memory/volatile-filesystem descriptor: %v", err)
	}
	invalid := descriptor
	invalid.SnapshotToken = "bad"
	if err := invalid.validate(negotiated); err == nil {
		t.Fatal("accepted invalid filesystem range token")
	}
	invalid = descriptor
	invalid.FilesystemRoots[0].LogicalBytes++
	if err := invalid.validate(negotiated); err == nil {
		t.Fatal("accepted mismatched filesystem dependency bytes")
	}
}

func testForkIdentity() ForkIdentity {
	digest := "sha256:" + strings.Repeat("1", 64)
	return ForkIdentity{
		TransactionID: "tx-1", SourceNodeID: "source", TargetNodeID: "target",
		SourceContainer: "app", SourceBranch: "main", TargetContainer: "app-copy", TargetBranch: "main",
		TargetVisibleBranch: "main",
		ImageDigest:         digest, RootfsDigest: digest, RepositoryID: "shared-repository",
	}
}

func TestMigrationIdentityMustPreserveLogicalBranch(t *testing.T) {
	identity := testForkIdentity()
	identity.Migration = true
	identity.TargetContainer = identity.SourceContainer
	identity.TargetVisibleBranch = identity.SourceBranch
	if err := identity.Validate(); err != nil {
		t.Fatalf("valid migration identity rejected: %v", err)
	}
	identity.TargetContainer = "other-family"
	if err := identity.Validate(); err == nil {
		t.Fatal("migration into another family was accepted")
	}
	identity.TargetContainer = identity.SourceContainer
	identity.TargetVisibleBranch = "other-branch"
	if err := identity.Validate(); err == nil {
		t.Fatal("migration that changes the branch identity was accepted")
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
