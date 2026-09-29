// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestDistributedRepositoryCachePathIsPrivateAndStable(t *testing.T) {
	active := t.TempDir()
	first := distributedRepositoryCachePath(active, "physical-a", "/run/ttc/volumes/logical-volume")
	if first != distributedRepositoryCachePath(active, "physical-a", "/run/ttc/volumes/logical-volume") {
		t.Fatal("repository cache path is not deterministic")
	}
	wantParent := filepath.Join(active, "containers", "physical-a", "repositories", "remote")
	if filepath.Dir(first) != wantParent {
		t.Fatalf("repository cache parent=%q, want %q", filepath.Dir(first), wantParent)
	}
	if first == distributedRepositoryCachePath(active, "physical-a", "/run/ttc/volumes/other") {
		t.Fatal("different repository sources alias the same cache path")
	}
}

type testSnapshotProviderLookup struct {
	found bool
	err   error
}

func (lookup testSnapshotProviderLookup) SnapshotProvider(snapshot uint64) (cluster.SnapshotOriginProvider, bool, error) {
	return cluster.SnapshotOriginProvider{SnapshotID: snapshot, NodeID: "node-a"}, lookup.found, lookup.err
}

// A rehydration may import its branch while the provider that published the
// branch has yet to complete the handoff of its snapshot: the superseded
// branch it discards keeps its root in the frozen snapshot until then, and
// the import, which replaces the root, waits for the handoff rather than
// failing.
func TestRemoteRepositoryImportWaitsOutAHandoff(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreateWithOrigin("family", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "payload", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, []byte("payload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(ttcvolume.MainBranch, "returning", 101, 102); err != nil {
		t.Fatal(err)
	}
	descriptor, err := repository.DescribeRemoteSnapshot("returning")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishOriginHandoff(context.Background(), shared, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch("returning"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ImportRemoteBranchDescriptorWithOrigins(descriptor, "returning", 103, 104); !frozenForAuthorityHandoff(err) {
		t.Fatalf("import during the handoff = %v, want it refused as frozen", err)
	}
	done := make(chan error, 1)
	go func() {
		imported, created, err := importRemoteRepositoryBranch(manager, repository.Path(), manager.Capability().Mode, "peer-0", descriptor, "returning", []ttcsnapshot.SnapshotID{103, 104})
		if err == nil {
			if created {
				err = errors.New("the import created a repository where there was one")
			}
			err = errors.Join(err, imported.Close())
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("import across the handoff: %v", err)
	}
	if _, err := repository.Branch("returning"); err != nil {
		t.Fatalf("imported branch: %v", err)
	}
}

func TestSnapshotOriginAuthorityDistinguishesTombstones(t *testing.T) {
	for _, test := range []struct {
		name     string
		lookup   testSnapshotProviderLookup
		terminal bool
		wantErr  bool
	}{
		{name: "volatile", lookup: testSnapshotProviderLookup{found: true}},
		{name: "shared", lookup: testSnapshotProviderLookup{}},
		{name: "terminal", lookup: testSnapshotProviderLookup{err: os.ErrNotExist}, terminal: true},
		{name: "authority failure", lookup: testSnapshotProviderLookup{err: errors.New("raft unavailable")}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, volatile, terminal, err := snapshotOriginAuthority(test.lookup, 17)
			if volatile != test.lookup.found || terminal != test.terminal || (err != nil) != test.wantErr {
				t.Fatalf("snapshotOriginAuthority() = (volatile=%t, terminal=%t, err=%v), want (volatile=%t, terminal=%t, error=%t)", volatile, terminal, err, test.lookup.found, test.terminal, test.wantErr)
			}
		})
	}
}

func TestProviderBaselineReleaseWaitsForSharedAuthority(t *testing.T) {
	object := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	state := &cluster.AuthorityState{VolatileOrigins: map[uint64]cluster.SnapshotOriginProvider{17: {NodeID: "node-a"}}}
	for _, name := range []string{"root/filesystem/branch-a", "pin/open/branch-a"} {
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("volatile provider baseline %q was releasable", name)
		}
		delete(state.VolatileOrigins, 17)
		if !providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("shared provider baseline %q was not releasable", name)
		}
		state.VolatileOrigins[17] = cluster.SnapshotOriginProvider{NodeID: "node-a"}
	}
	delete(state.VolatileOrigins, 17)
	for _, name := range []string{"remote/17/operation/00000000", "edge/parent/0000", "root/bridge/edge/parent/0000", "root/cold/memory"} {
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("non-provider reference %q was releasable", name)
		}
	}
}

func TestInactiveGraphReferenceReconciliationReleasesOnlyFencedClaims(t *testing.T) {
	sharedDead := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	sharedLive := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 2}
	volatileDead := ttcsnapshot.ObjectID{Snapshot: 19, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	current := map[string]ttcsnapshot.ObjectID{
		"remote/17/dead/00000000":    sharedDead,
		"remote/17/live/00000000":    sharedLive,
		"remote/19/handoff/00000000": volatileDead,
		"root/cold-memory/17/stable": sharedDead,
	}
	desired := map[string]ttcsnapshot.ObjectID{"remote/17/live/00000000": sharedLive}
	state := cluster.NewAuthorityState()
	state.VolatileOrigins[19] = cluster.SnapshotOriginProvider{NodeID: "provider"}
	mutations, err := inactiveGraphReferenceMutations(state, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 1 || mutations[0].Name != "remote/17/dead/00000000" || mutations[0].Retain {
		t.Fatalf("inactive mutations=%+v, want one release for the fenced shared claim", mutations)
	}
	ledger := ttcsnapshot.NewReferenceLedger()
	objects := []ttcsnapshot.Object{
		{ID: sharedDead, State: ttcsnapshot.ObjectLive},
		{ID: sharedLive, State: ttcsnapshot.ObjectLive},
	}
	if err := ledger.ImportOrigin(sharedDead.Snapshot, objects, map[string]ttcsnapshot.ObjectID{
		"remote/17/dead/00000000":    sharedDead,
		"remote/17/live/00000000":    sharedLive,
		"root/cold-memory/17/stable": sharedDead,
	}); err != nil {
		t.Fatal(err)
	}
	if err := applySharedReferenceBatches(ledger, mutations); err != nil {
		t.Fatal(err)
	}
	references := ledger.References()
	if _, ok := references["remote/17/dead/00000000"]; ok {
		t.Fatal("fenced node claim remains in the shared ledger")
	}
	if references["remote/17/live/00000000"] != sharedLive || references["root/cold-memory/17/stable"] != sharedDead {
		t.Fatalf("reconciliation changed live references: %+v", references)
	}
}

func TestTerminalSourceAction(t *testing.T) {
	tests := []struct {
		name   string
		tx     *cluster.Transaction
		err    error
		action string
		done   bool
	}{
		{name: "independent", tx: &cluster.Transaction{State: cluster.TransactionIndependent}, action: "distributed-complete", done: true},
		{name: "canceled", tx: &cluster.Transaction{State: cluster.TransactionCanceled}, action: "distributed-cancel", done: true},
		{name: "failed", tx: &cluster.Transaction{State: cluster.TransactionFailed}, action: "distributed-cancel", done: true},
		{name: "retired", err: os.ErrNotExist, action: "distributed-cancel", done: true},
		{name: "published", tx: &cluster.Transaction{State: cluster.TransactionPublished}},
		{name: "read error", err: errors.New("raft unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, done := terminalSourceAction(test.tx, test.err)
			if action != test.action || done != test.done {
				t.Fatalf("terminalSourceAction() = (%q, %t), want (%q, %t)", action, done, test.action, test.done)
			}
		})
	}
}

func TestColdReferenceIdentityIsScopedToPublication(t *testing.T) {
	first := coldBranchReferenceIdentity("family", "main", "publication-first")
	second := coldBranchReferenceIdentity("family", "main", "publication-second")
	if first == second {
		t.Fatal("two durable activations reused one reference identity")
	}
	origin := ttcsnapshot.SnapshotID(17)
	object := ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	if coldMemoryReferenceName(origin, first, "root/control", object) == coldMemoryReferenceName(origin, second, "root/control", object) {
		t.Fatal("two durable memory activations reused one named reference")
	}
	// Ready and Committed both carry the same publication ID, so the candidate
	// pin and final branch pin are deliberately identical across the atomic Raft
	// transition.
	if got := coldBranchReferenceIdentity("family", "main", "publication-first"); got != first {
		t.Fatalf("stable publication identity=%q, want %q", got, first)
	}
}

func TestProviderBaselineReferencesAreOriginScoped(t *testing.T) {
	ledger := ttcsnapshot.NewReferenceLedger()
	const providerName = "root/filesystem-base/stable-branch"
	for _, origin := range []ttcsnapshot.SnapshotID{11, 13} {
		root := ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
		references := providerBaselineReferences(origin, map[string]ttcsnapshot.ObjectID{providerName: root})
		if err := ledger.ImportOrigin(origin, []ttcsnapshot.Object{{ID: root, State: ttcsnapshot.ObjectLive}}, references); err != nil {
			t.Fatalf("import origin %d: %v", origin, err)
		}
		wantName := providerBaselineReferenceName(origin, providerName)
		if got := ledger.References()[wantName]; got != root {
			t.Fatalf("origin %d provider reference=%s, want %s", origin, got, root)
		}
	}
	if first, second := providerBaselineReferenceName(11, providerName), providerBaselineReferenceName(13, providerName); first == second {
		t.Fatalf("provider generations reused reference name %q", first)
	}
	bridge := "root/bridge/edge/parent/0000"
	if got := providerBaselineReferenceName(13, bridge); got != bridge {
		t.Fatalf("structural bridge was namespaced as %q", got)
	}
}

func TestManifestReferenceLivenessFollowsParentAuthority(t *testing.T) {
	state := &cluster.AuthorityState{
		NextSnapshotID:   64,
		VolatileOrigins:  map[uint64]cluster.SnapshotOriginProvider{},
		AbandonedOrigins: map[uint64]bool{},
		RetiredOrigins:   map[uint64]cluster.SharedPackRetirement{},
	}
	parent := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 3}
	edge := "edge/" + parent.String() + "/0004"
	for _, name := range []string{edge, "root/bridge/" + edge, "root/branch/main"} {
		if live, err := manifestReferenceIsLive(state, name); err != nil || !live {
			t.Fatalf("live reference %q = (%t, %v), want (true, nil)", name, live, err)
		}
	}
	state.AbandonedOrigins[17] = true
	if live, err := manifestReferenceIsLive(state, edge); err != nil || live {
		t.Fatalf("abandoned parent edge = (%t, %v), want (false, nil)", live, err)
	}
	delete(state.AbandonedOrigins, 17)
	state.RetiredOrigins[17] = cluster.SharedPackRetirement{SnapshotID: 17, GCGeneration: 1, RetiredAt: time.Unix(1, 0)}
	if live, err := manifestReferenceIsLive(state, edge); err != nil || live {
		t.Fatalf("retired parent edge = (%t, %v), want (false, nil)", live, err)
	}
	if _, err := manifestReferenceIsLive(state, "edge/not-an-object/0001"); err == nil {
		t.Fatal("malformed structural edge was accepted")
	}
	future := ttcsnapshot.ObjectID{Snapshot: 64, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 1}
	if _, err := manifestReferenceIsLive(state, "edge/"+future.String()+"/0000"); err == nil {
		t.Fatal("unallocated parent origin was accepted")
	}
}

func TestPublicationBridgePinsProviderRootsAcrossHandoff(t *testing.T) {
	provider := ttcsnapshot.NewReferenceLedger()
	shared := ttcsnapshot.NewReferenceLedger()
	origin := ttcsnapshot.SnapshotID(17)
	root := ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	objects := []ttcsnapshot.Object{{ID: root, State: ttcsnapshot.ObjectLive}}
	baseline := map[string]ttcsnapshot.ObjectID{"root/branch/main": root}
	if err := provider.ImportOrigin(origin, objects, baseline); err != nil {
		t.Fatal(err)
	}
	if err := shared.ImportOrigin(origin, objects, baseline); err != nil {
		t.Fatal(err)
	}
	bridges := publicationBridgeReferences(origin, baseline)
	if len(bridges) != 1 {
		t.Fatalf("bridges=%+v, want one", bridges)
	}
	bridge := bridges[0]
	if isProviderBaselineReference(bridge.Name) {
		t.Fatalf("publication bridge %q was classified as a disposable provider baseline", bridge.Name)
	}
	if matched := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(bridge.RetainOperation); !matched {
		t.Fatalf("publication bridge operation %q is not a valid cluster operation ID", bridge.RetainOperation)
	}
	if err := shared.ApplyReference(bridge.RetainOperation, bridge.Name, bridge.Object, true); err != nil {
		t.Fatal(err)
	}
	// This is the mutation that raced the bulk pack write: the provider's
	// branch root disappeared before the final publication commit.
	if err := shared.ApplyHandoff(origin, 0, []ttcsnapshot.SnapshotReferenceMutation{{
		Sequence: 1, Operation: "release-provider-root", Name: "root/branch/main", Object: root, Retain: false,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := shared.ApplyReference("retain-cold-root", "root/cold/branch", root, true); err != nil {
		t.Fatalf("cold root could not replace publication bridge: %v", err)
	}
	if err := shared.ApplyReference("release-publication-bridge", bridge.Name, root, false); err != nil {
		t.Fatal(err)
	}
	inspection := shared.Inspect()
	if got := inspection.Objects[root]; got != 1 {
		t.Fatalf("root refs=%d, want one durable cold root; inspection=%+v", got, inspection)
	}
}

func TestPublicationBridgeIntentLifetime(t *testing.T) {
	state := cluster.NewAuthorityState()
	state.Transactions["fork-live"] = cluster.Transaction{State: cluster.TransactionPublished}
	transaction := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: 17,
		Kind: sharedPublicationBridgeTransaction, Transaction: "fork-live", CreatedAt: time.Now(),
	}
	if !publicationBridgeIntentNeeded(state, transaction) {
		t.Fatal("nonterminal graph handoff did not preserve its publication bridge")
	}
	completed := state.Transactions["fork-live"]
	completed.State = cluster.TransactionIndependent
	state.Transactions["fork-live"] = completed
	if publicationBridgeIntentNeeded(state, transaction) {
		t.Fatal("terminal graph handoff retained its publication bridge")
	}

	publication := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: 19,
		Kind: sharedPublicationBridgePublication, Family: "family", Branch: "main", PublicationID: "publish-1", CreatedAt: time.Now(),
	}
	state.Publications["arbitrary-map-key"] = cluster.PublicationRecord{
		FamilyID: "family", BranchID: "main", PublicationID: "publish-1", State: cluster.PublicationReady,
	}
	if !publicationBridgeIntentNeeded(state, publication) {
		t.Fatal("ready branch publication did not preserve its publication bridge")
	}
	record := state.Publications["arbitrary-map-key"]
	record.State = cluster.PublicationCommitted
	state.Publications["arbitrary-map-key"] = record
	if publicationBridgeIntentNeeded(state, publication) {
		t.Fatal("committed branch publication retained its publication bridge")
	}
}

func TestPublicationBridgeIntentRoundTrip(t *testing.T) {
	root := t.TempDir()
	intent := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: 23,
		Kind: sharedPublicationBridgeTransaction, Transaction: "fork-roundtrip", CreatedAt: time.Now().UTC(),
	}
	if err := persistSharedPublicationBridgeIntent(root, intent); err != nil {
		t.Fatal(err)
	}
	intents, err := loadSharedPublicationBridgeIntents(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Snapshot != intent.Snapshot || intents[0].Transaction != intent.Transaction {
		t.Fatalf("loaded intents=%+v, want %+v", intents, intent)
	}
	if err := removeSharedPublicationBridgeIntent(root, ttcsnapshot.SnapshotID(intent.Snapshot)); err != nil {
		t.Fatal(err)
	}
	intents, err = loadSharedPublicationBridgeIntents(root)
	if err != nil || len(intents) != 0 {
		t.Fatalf("intents after removal=%+v, err=%v", intents, err)
	}
}

func TestPublicationBridgeIntentAdoptsTerminalTransaction(t *testing.T) {
	root := t.TempDir()
	old := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: 29,
		Kind: sharedPublicationBridgeTransaction, Transaction: "fork-finished", CreatedAt: time.Now().UTC(),
	}
	if err := persistSharedPublicationBridgeIntent(root, old); err != nil {
		t.Fatal(err)
	}
	replacement := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: old.Snapshot,
		Kind: sharedPublicationBridgeOrigin, CreatedAt: old.CreatedAt.Add(time.Second),
	}
	state := cluster.NewAuthorityState()
	state.VolatileOrigins[old.Snapshot] = cluster.SnapshotOriginProvider{NodeID: "node-a"}
	state.Transactions[old.Transaction] = cluster.Transaction{State: cluster.TransactionPublished}
	if err := persistSharedPublicationBridgeIntentWithAuthority(root, replacement, state); err == nil {
		t.Fatal("live transaction bridge was adopted")
	}
	finished := state.Transactions[old.Transaction]
	finished.State = cluster.TransactionIndependent
	state.Transactions[old.Transaction] = finished
	if err := persistSharedPublicationBridgeIntentWithAuthority(root, replacement, state); err != nil {
		t.Fatalf("adopting terminal transaction bridge: %v", err)
	}
	intents, err := loadSharedPublicationBridgeIntents(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Kind != sharedPublicationBridgeOrigin || intents[0].Snapshot != old.Snapshot {
		t.Fatalf("adopted intents=%+v, want origin bridge for snapshot %d", intents, old.Snapshot)
	}
}

func TestSharedSnapshotReaderScopesRestartedRelayToManifestOrigins(t *testing.T) {
	root := ttcsnapshot.ObjectID{Snapshot: 7, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	reader := &sharedSnapshotObjectReader{
		allowed:        map[ttcsnapshot.ObjectID]struct{}{root: {}},
		allowedOrigins: map[ttcsnapshot.SnapshotID]struct{}{3: {}},
	}
	if !reader.authorize(root) {
		t.Fatal("root was not authorized")
	}
	// A restarted resolver may request a deep immutable source object directly,
	// without first walking from the exported root in this relay session.
	deepSource := ttcsnapshot.ObjectID{Snapshot: 3, Kind: ttcsnapshot.ObjectKindFile, Ordinal: 4096}
	if !reader.authorize(deepSource) {
		t.Fatal("object from a manifest origin was not authorized")
	}
	outside := ttcsnapshot.ObjectID{Snapshot: 4, Kind: ttcsnapshot.ObjectKindFile, Ordinal: 4096}
	if reader.authorize(outside) {
		t.Fatal("object outside the manifest origin scope was authorized")
	}
}

func TestConfigMemFDSupportsWideBatchPayload(t *testing.T) {
	payload := bytes.Repeat([]byte("wide-sibling-batch\n"), 1<<16)
	file, err := newConfigMemFD("ttc-config-test", payload)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes, want %d", len(got), len(payload))
	}
}

func TestRunBoundedBatchCapsConcurrentWork(t *testing.T) {
	const (
		items = 97
		limit = 7
	)
	var active atomic.Int32
	var maximum atomic.Int32
	var completed atomic.Int32
	runBoundedBatch(items, limit, func(int) {
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		completed.Add(1)
	})
	if got := completed.Load(); got != items {
		t.Fatalf("completed %d items, want %d", got, items)
	}
	if got := maximum.Load(); got <= 1 || got > limit {
		t.Fatalf("maximum concurrency=%d, want 2..%d", got, limit)
	}
}

func TestDistributedHandoffSchedulerCapsConcurrentWork(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	const items = 31
	var active atomic.Int32
	var maximum atomic.Int32
	completed := make(chan struct{}, items)
	release := make(chan struct{})
	for range items {
		runtime.scheduleHandoff(func() {
			current := active.Add(1)
			for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
			}
			<-release
			active.Add(-1)
			completed <- struct{}{}
		})
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() != distributedHandoffConcurrency && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := active.Load(); got != distributedHandoffConcurrency {
		t.Fatalf("active handoffs=%d, want %d", got, distributedHandoffConcurrency)
	}
	close(release)
	for range items {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("scheduled handoffs did not drain")
		}
	}
	if got := maximum.Load(); got != distributedHandoffConcurrency {
		t.Fatalf("maximum handoff concurrency=%d, want %d", got, distributedHandoffConcurrency)
	}
}

// A handoff scheduled for later is pending meanwhile, so that retirement waits
// for it, but leaves every worker to the handoffs scheduled now.
func TestDelayedHandoffHoldsNoWorker(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	delayed := make(chan struct{})
	runtime.scheduleHandoffAfter(200*time.Millisecond, func() { close(delayed) })
	if pending := runtime.handoffCount.Load(); pending != 1 {
		t.Fatalf("%d handoffs pending, want the delayed one", pending)
	}
	started := make(chan struct{}, distributedHandoffConcurrency)
	release := make(chan struct{})
	for i := 0; i < distributedHandoffConcurrency; i++ {
		runtime.scheduleHandoff(func() {
			started <- struct{}{}
			<-release
		})
	}
	for i := 0; i < distributedHandoffConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%d of %d handoffs scheduled now started while a delayed one waited", i, distributedHandoffConcurrency)
		}
	}
	close(release)
	select {
	case <-delayed:
	case <-time.After(5 * time.Second):
		t.Fatal("the delayed handoff never ran")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.handoffCount.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d handoffs still pending after all ran", runtime.handoffCount.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDistributedSourceMonitorPollingIsBounded(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	finish := make(chan struct{})
	for range 256 {
		runtime.scheduleSourceMonitor(func() { <-finish })
	}
	if got := runtime.sourceMonitorInterval(); got != 16*time.Second {
		t.Fatalf("256-monitor interval=%s, want 16s", got)
	}
	first, ok := runtime.trySourcePoll()
	if !ok {
		t.Fatal("first source poll was rejected")
	}
	second, ok := runtime.trySourcePoll()
	if !ok {
		t.Fatal("second source poll was rejected")
	}
	if release, ok := runtime.trySourcePoll(); ok {
		release()
		t.Fatal("source poll concurrency exceeded its bound")
	}
	first()
	if release, ok := runtime.trySourcePoll(); !ok {
		t.Fatal("source poll slot was not released")
	} else {
		release()
	}
	second()
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for runtime.monitorCount.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("monitor count=%d, want 0", runtime.monitorCount.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDistributedSourceCleanupIsSerializedPerFamily(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	unlock := runtime.lockSourceFamily("family-a")
	entered := make(chan struct{})
	released := make(chan struct{})
	go func() {
		defer close(released)
		defer runtime.lockSourceFamily("family-a")()
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("same-family cleanup entered concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	// Another family must not be serialized behind family-a.
	other := runtime.lockSourceFamily("family-b")
	other()
	unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("same-family cleanup did not resume")
	}
	<-released
}

func TestDistributedSourceReclamationIsSerializedAcrossFamilies(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	unlock := runtime.lockReclamation()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer runtime.lockReclamation()()
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("repository-wide reclamation entered concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repository-wide reclamation did not resume")
	}
	<-done
}

func TestDistributedForegroundCapturePreemptsQueuedHandoffs(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	active := make(chan struct{}, distributedHandoffConcurrency)
	releaseActive := make(chan struct{})
	for range distributedHandoffConcurrency {
		runtime.scheduleHandoff(func() {
			finish := runtime.beginBackgroundSnapshotWork()
			defer finish()
			active <- struct{}{}
			<-releaseActive
		})
	}
	for range distributedHandoffConcurrency {
		select {
		case <-active:
		case <-time.After(time.Second):
			t.Fatal("initial handoffs did not start")
		}
	}

	foregroundEntered := make(chan struct{})
	releaseForeground := make(chan struct{})
	go func() {
		finish := runtime.beginForegroundCapture()
		close(foregroundEntered)
		<-releaseForeground
		finish()
	}()
	select {
	case <-foregroundEntered:
		t.Fatal("foreground entered while active handoffs held the gate")
	case <-time.After(20 * time.Millisecond):
	}

	queuedStarted := make(chan struct{})
	runtime.scheduleHandoff(func() {
		finish := runtime.beginBackgroundSnapshotWork()
		defer finish()
		close(queuedStarted)
	})
	close(releaseActive)
	select {
	case <-foregroundEntered:
	case <-time.After(time.Second):
		t.Fatal("foreground did not enter after active handoffs completed")
	}
	select {
	case <-queuedStarted:
		t.Fatal("queued handoff passed a waiting foreground capture")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseForeground)
	select {
	case <-queuedStarted:
	case <-time.After(time.Second):
		t.Fatal("queued handoff did not resume after foreground capture")
	}
}

func TestDistributedColdCollectionCoalescesRequests(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	collect := func() {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	runtime.scheduleColdCollection(collect)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cold collection did not start")
	}
	for range 100 {
		runtime.scheduleColdCollection(collect)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for (runtime.collectActive.Load() || runtime.collectNeeded.Load()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runtime.collectActive.Load() || runtime.collectNeeded.Load() {
		t.Fatal("coalesced cold collection did not quiesce")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("cold collection ran %d times, want one active plus one coalesced pass", got)
	}
}

func TestPublishRemoteTargetRetriesPausedAuthorityGap(t *testing.T) {
	const transactionID = "fork-publish-retry"
	var publishes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tx := cluster.Transaction{State: cluster.TransactionTargetPaused, Revision: 7}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forks/"+transactionID:
			_ = json.NewEncoder(w).Encode(tx)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/forks/"+transactionID+"/publish":
			if publishes.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "remote fork target node has no valid execution authority"})
				return
			}
			tx.State = cluster.TransactionPublished
			tx.Revision++
			_ = json.NewEncoder(w).Encode(tx)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	peer, err := cluster.NewPlainClient(server.URL, "source", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := publishRemoteTarget(ctx, peer, transactionID, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != cluster.TransactionPublished || publishes.Load() != 2 {
		t.Fatalf("publish result=(%s, attempts=%d), want (published, 2)", tx.State, publishes.Load())
	}
}

func TestActivateRemoteMigrationRetriesAdvancedIndependentRevision(t *testing.T) {
	const transactionID = "fork-activate-retry"
	var activates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tx := cluster.Transaction{
			Identity: cluster.ForkIdentity{TransactionID: transactionID, Migration: true},
			State:    cluster.TransactionIndependent, Revision: 8, OwnershipCommitted: true,
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forks/"+transactionID:
			_ = json.NewEncoder(w).Encode(tx)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/forks/"+transactionID+"/activate":
			if activates.Add(1) == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "stale transaction revision"})
				return
			}
			tx.Activated = true
			tx.Revision++
			_ = json.NewEncoder(w).Encode(tx)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	peer, err := cluster.NewPlainClient(server.URL, "source", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := activateRemoteMigration(ctx, peer, transactionID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !tx.Activated || tx.Revision != 9 || activates.Load() != 2 {
		t.Fatalf("activation result=(activated=%t, revision=%d, attempts=%d), want (true, 9, 2)", tx.Activated, tx.Revision, activates.Load())
	}
}

func TestCancelRemoteForkTargetRetriesAdvancedRevision(t *testing.T) {
	const transactionID = "fork-cancel-retry"
	var cancels atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tx := cluster.Transaction{
			Identity: cluster.ForkIdentity{TransactionID: transactionID},
			State:    cluster.TransactionPublished, Revision: 8,
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forks/"+transactionID:
			_ = json.NewEncoder(w).Encode(tx)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/forks/"+transactionID+"/cancel":
			var request cluster.CancelRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			attempt := cancels.Add(1)
			if attempt == 1 {
				if request.Revision != 7 {
					t.Errorf("first cancellation revision=%d, want 7", request.Revision)
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "stale transaction revision"})
				return
			}
			if request.Revision != 8 {
				t.Errorf("retried cancellation revision=%d, want 8", request.Revision)
			}
			tx.State = cluster.TransactionCanceled
			tx.Revision = 9
			_ = json.NewEncoder(w).Encode(tx)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	peer, err := cluster.NewPlainClient(server.URL, "source", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := cancelRemoteForkTarget(ctx, peer, transactionID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != cluster.TransactionCanceled || tx.Revision != 9 || cancels.Load() != 2 {
		t.Fatalf("cancellation result=(%s, revision=%d, attempts=%d), want (canceled, 9, 2)", tx.State, tx.Revision, cancels.Load())
	}
}

func TestReserveRemoteTargetWaitsForNewLeaderLease(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := reserveRemoteTarget(ctx, "node-b", func(node string) (*cluster.NodeRecord, error) {
		if node != "node-b" {
			t.Fatalf("lookup node=%q, want node-b", node)
		}
		return &cluster.NodeRecord{NodeID: node, State: cluster.NodeReady}, nil
	}, func(context.Context) (*cluster.Transaction, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("cluster peer: transaction target is not accepting placement")
		}
		return &cluster.Transaction{State: cluster.TransactionTargetReserved}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != cluster.TransactionTargetReserved || attempts.Load() != 3 {
		t.Fatalf("reservation=(%s, attempts=%d), want (target-reserved, 3)", tx.State, attempts.Load())
	}
}

func TestReserveRemoteTargetDoesNotRetryDrainingNode(t *testing.T) {
	var attempts atomic.Int32
	want := errors.New("cluster peer: transaction target is not accepting placement")
	_, err := reserveRemoteTarget(context.Background(), "node-b", func(string) (*cluster.NodeRecord, error) {
		return &cluster.NodeRecord{NodeID: "node-b", State: cluster.NodeDraining}, nil
	}, func(context.Context) (*cluster.Transaction, error) {
		attempts.Add(1)
		return nil, want
	})
	if !errors.Is(err, want) || attempts.Load() != 1 {
		t.Fatalf("reservation error=%v attempts=%d, want original error after one attempt", err, attempts.Load())
	}
}
