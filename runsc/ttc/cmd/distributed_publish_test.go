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
	"strings"
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
		{name: "authority failure", lookup: testSnapshotProviderLookup{err: errors.New("authority unavailable")}, wantErr: true},
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
	state := &cluster.AuthorityState{
		NextSnapshotID:  64,
		VolatileOrigins: map[uint64]cluster.SnapshotOriginProvider{17: {NodeID: "node-a"}},
		SharedOrigins:   map[uint64]bool{},
	}
	publish := func() {
		delete(state.VolatileOrigins, 17)
		state.SharedOrigins[17] = true
	}
	for _, name := range []string{"root/filesystem/branch-a", "pin/open/branch-a"} {
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("volatile provider baseline %q was releasable", name)
		}
		publish()
		if !providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("shared provider baseline %q was not releasable", name)
		}
		delete(state.SharedOrigins, 17)
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("dead provider baseline %q was releasable", name)
		}
		state.VolatileOrigins[17] = cluster.SnapshotOriginProvider{NodeID: "node-a"}
	}
	publish()
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

func TestInactiveClaimReconciliationKeepsAClaimRetainedMeanwhile(t *testing.T) {
	inherited := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	handoff := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 2}
	ledger := ttcsnapshot.NewReferenceLedger()
	if err := ledger.ImportOrigin(17, []ttcsnapshot.Object{
		{ID: inherited, State: ttcsnapshot.ObjectLive},
		{ID: handoff, State: ttcsnapshot.ObjectLive},
	}, map[string]ttcsnapshot.ObjectID{
		"remote/17/fenced/00000000":   inherited,
		"remote/17/released/00000000": inherited,
		"root/cold-memory/17/stable":  handoff,
	}); err != nil {
		t.Fatal(err)
	}
	state := cluster.NewAuthorityState()
	state.NextSnapshotID = 18
	state.SharedOrigins[17] = true
	withLedger := func(fn func(*ttcsnapshot.ReferenceLedger) error) error { return fn(ledger) }
	claims := func() (*cluster.AuthorityState, map[string]ttcsnapshot.ObjectID, error) {
		// While the nodes' inventories are read, which name neither: a remote
		// fork's handoff writes its claim's intent after its node's was read
		// and retains the claim, and another claim's owner releases it.
		if err := ledger.ApplyReference("retain-handoff", "remote/17/handoff/00000000", handoff, true); err != nil {
			return nil, nil, err
		}
		if err := ledger.ApplyReference("release-released", "remote/17/released/00000000", inherited, false); err != nil {
			return nil, nil, err
		}
		return state, map[string]ttcsnapshot.ObjectID{}, nil
	}
	if err := reconcileInactiveClaims(withLedger, claims); err != nil {
		t.Fatal(err)
	}
	references := ledger.References()
	if references["remote/17/handoff/00000000"] != handoff {
		t.Fatalf("the claim retained while the inventories were read was released: %+v", references)
	}
	if _, ok := references["remote/17/fenced/00000000"]; ok {
		t.Fatalf("the claim no inventory names was kept: %+v", references)
	}
}

// A branch's cold reference names come back: running from the same roots
// twice, between two publications or after a rehydration canceled one, a
// branch names them alike. Each reconciliation pass retains and releases
// them anew, and the roots' origin keeps its reference.
func TestColdReferenceNamesComeBack(t *testing.T) {
	root := ttcsnapshot.ObjectID{Snapshot: 1, Ordinal: 6, Kind: ttcsnapshot.ObjectKindMetadata}
	ledger := ttcsnapshot.NewReferenceLedger()
	const baseline = "root/provider/1/baseline"
	if err := ledger.ImportOrigin(1, []ttcsnapshot.Object{{ID: root, State: ttcsnapshot.ObjectLive}}, map[string]ttcsnapshot.ObjectID{baseline: root}); err != nil {
		t.Fatal(err)
	}
	const running, published = "root/cold/running", "root/cold/published"
	previous := baseline
	for pass, name := range []string{running, published, running, published, running} {
		desired := map[string]ttcsnapshot.ObjectID{name: root}
		mutations := coldReferenceMutations(desired, ledger.References(), []string{name}, []string{previous}, uint64(pass+1))
		if err := applySharedReferenceBatches(ledger, mutations); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
		previous = name
		if references := ledger.References(); len(references) != 1 || references[name] != root {
			t.Fatalf("after pass %d to %q, references are %v", pass+1, name, references)
		}
	}
	if collected, err := ledger.Collect(16); err != nil || collected != 0 {
		t.Fatalf("collection of a referenced origin = (%d, %v), want nothing", collected, err)
	}
}

// A coordinator that rebuilt its ledger while a publication's origin was
// still volatile, its provider's import having reached the ledger before,
// holds the origin in no ledger once the publication makes it shared:
// reconciliation imports it, as a rebuild would, before it retains the
// publication's roots, which every pass failed to retain before.
func TestReconciliationImportsAnOriginTheLedgerMissed(t *testing.T) {
	ledger := ttcsnapshot.NewReferenceLedger()
	old := ttcsnapshot.ObjectID{Snapshot: 1, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	if err := ledger.ImportOrigin(1, []ttcsnapshot.Object{{ID: old, State: ttcsnapshot.ObjectLive}}, map[string]ttcsnapshot.ObjectID{"root/cold/old": old}); err != nil {
		t.Fatal(err)
	}
	root := ttcsnapshot.ObjectID{Snapshot: 2, Ordinal: 3, Kind: ttcsnapshot.ObjectKindMetadata}
	state := &cluster.AuthorityState{NextSnapshotID: 4, SharedOrigins: map[uint64]bool{1: true, 2: true}}
	desired := map[string]ttcsnapshot.ObjectID{"root/cold/old": old, "root/cold/new": root}
	retain := []string{"root/cold/new"}
	if err := applySharedReferenceBatches(ledger, coldReferenceMutations(desired, ledger.References(), retain, nil, 1)); err == nil || !strings.Contains(err.Error(), "cannot be retained") {
		t.Fatalf("retaining a root of an origin the ledger missed = %v, want it refused", err)
	}
	missed := missedSharedOrigins(state, ledger.Origins(), desired, retain)
	if len(missed) != 1 || len(missed[2]) != 1 || missed[2]["root/cold/new"] != root {
		t.Fatalf("missed origins = %v, want origin 2's new root", missed)
	}
	references, err := missedOriginBaseline(state, 2, map[string]ttcsnapshot.ObjectID{"root/memory": root}, missed[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportOrigin(2, []ttcsnapshot.Object{{ID: root, State: ttcsnapshot.ObjectLive}}, references); err != nil {
		t.Fatal(err)
	}
	if err := applySharedReferenceBatches(ledger, coldReferenceMutations(desired, ledger.References(), retain, nil, 2)); err != nil {
		t.Fatalf("retaining the root once its origin is imported: %v", err)
	}
	if got := ledger.References()["root/cold/new"]; got != root {
		t.Fatalf("the publication's root selects %s, want %s", got, root)
	}

	// An origin still volatile is its provider's, and one retired is gone:
	// neither is imported so.
	volatile := ttcsnapshot.ObjectID{Snapshot: 3, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	state.SharedOrigins[3] = true
	state.VolatileOrigins = map[uint64]cluster.SnapshotOriginProvider{3: {SnapshotID: 3, NodeID: "node-a"}}
	if missed := missedSharedOrigins(state, ledger.Origins(), map[string]ttcsnapshot.ObjectID{"root/cold/volatile": volatile}, []string{"root/cold/volatile"}); len(missed) != 0 {
		t.Fatalf("a volatile origin was taken for missed: %v", missed)
	}
	delete(state.VolatileOrigins, 3)
	state.RetiredOrigins = map[uint64]cluster.SharedPackRetirement{3: {}}
	if missed := missedSharedOrigins(state, ledger.Origins(), map[string]ttcsnapshot.ObjectID{"root/cold/volatile": volatile}, []string{"root/cold/volatile"}); len(missed) != 0 {
		t.Fatalf("a retired origin was taken for missed: %v", missed)
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
		{name: "read error", err: errors.New("authority unavailable")},
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
	// pin and final branch pin are deliberately identical across the atomic
	// authority transition.
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
		NextSnapshotID:  64,
		VolatileOrigins: map[uint64]cluster.SnapshotOriginProvider{},
		SharedOrigins:   map[uint64]bool{17: true},
		RetiredOrigins:  map[uint64]cluster.SharedPackRetirement{},
	}
	parent := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMemoryNode, Ordinal: 3}
	edge := "edge/" + parent.String() + "/0004"
	for _, name := range []string{edge, "root/bridge/" + edge, "root/branch/main"} {
		if live, err := manifestReferenceIsLive(state, name); err != nil || !live {
			t.Fatalf("live reference %q = (%t, %v), want (true, nil)", name, live, err)
		}
	}
	// Neither volatile nor shared: abandoned, or retired.
	delete(state.SharedOrigins, 17)
	if live, err := manifestReferenceIsLive(state, edge); err != nil || live {
		t.Fatalf("dead parent edge = (%t, %v), want (false, nil)", live, err)
	}
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

// A bridge its owner removes after the shared GC listed the bridges, but
// before the GC read it, is gone: the GC passes over it rather than failing.
func TestPublicationBridgeIntentsPassOverOneRemovedSinceListed(t *testing.T) {
	root := t.TempDir()
	intent := sharedPublicationBridgeIntent{
		Version: sharedPublicationBridgeIntentVersion, Snapshot: 23,
		Kind: sharedPublicationBridgeTransaction, Transaction: "fork-kept", CreatedAt: time.Now().UTC(),
	}
	if err := persistSharedPublicationBridgeIntent(root, intent); err != nil {
		t.Fatal(err)
	}
	// A name the directory lists that no longer opens, as one removed since.
	removed := sharedPublicationBridgeIntentPath(root, ttcsnapshot.SnapshotID(42))
	if err := os.Symlink(removed+".gone", removed); err != nil {
		t.Fatal(err)
	}
	intents, err := loadSharedPublicationBridgeIntents(root)
	if err != nil {
		t.Fatalf("loading bridges with one removed since listed: %v", err)
	}
	if len(intents) != 1 || intents[0].Snapshot != intent.Snapshot {
		t.Fatalf("loaded intents=%+v, want only snapshot %d's", intents, intent.Snapshot)
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

// Captures share the snapshot handoff gate, and a final switch of an
// origin's references holds it alone: it waits for the captures under way,
// and new captures wait for it.
func TestSnapshotCapturesShareTheHandoffGate(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	first := runtime.beginCapture()
	second, ok := runtime.tryBeginCapture()
	if !ok {
		t.Fatal("a capture waited for another")
	}
	switched := make(chan func())
	go func() { switched <- runtime.beginFinalSnapshotSwitch() }()
	select {
	case <-switched:
		t.Fatal("a final switch passed captures under way")
	case <-time.After(20 * time.Millisecond):
	}
	if finish, ok := runtime.tryBeginCapture(); ok {
		finish()
		t.Fatal("a capture passed a waiting final switch")
	}
	later := make(chan func())
	go func() { later <- runtime.beginCapture() }()
	first()
	second()
	var finishSwitch func()
	select {
	case finishSwitch = <-switched:
	case <-time.After(time.Second):
		t.Fatal("the final switch did not start once the captures ended")
	}
	select {
	case <-later:
		t.Fatal("a capture started during the final switch")
	case <-time.After(20 * time.Millisecond):
	}
	finishSwitch()
	select {
	case finish := <-later:
		finish()
	case <-time.After(time.Second):
		t.Fatal("the waiting capture did not start after the switch")
	}
	abandoned := runtime.beginCapture()
	if _, ok := runtime.beginFinalSnapshotSwitchUnless(func() bool { return true }); ok {
		t.Fatal("an abandoned switch took the gate from a capture")
	}
	abandoned()
}

// A publication attempt that a cancellation waits for stops waiting for an
// origin's lock, and for the snapshot handoff gate, once its context ends:
// their holders may wait for what the cancellation's caller holds.
func TestOriginPublicationGivesWayToItsCancellation(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	// waits ends begin's context once begin has waited a while.
	waits := func(what string, begin func(context.Context) (func(), error)) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			finish, err := begin(ctx)
			if err == nil {
				finish()
			}
			result <- err
		}()
		select {
		case err := <-result:
			t.Fatalf("%s did not wait: %v", what, err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s ended with %v, want %v", what, err, context.Canceled)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s went on waiting once its context ended", what)
		}
	}
	unlock := runtime.lockSnapshotOrigin(7)
	waits("the origin's lock", func(ctx context.Context) (func(), error) {
		return runtime.lockSnapshotOriginContext(ctx, 7)
	})
	unlock()
	finishCapture := runtime.beginCapture()
	waits("the final switch", runtime.beginFinalSnapshotSwitchContext)
	// Not canceled, the switch takes the gate once the capture ends.
	switched := make(chan error, 1)
	ctx, end := context.WithCancel(context.Background())
	defer end()
	go func() {
		finish, err := runtime.beginFinalSnapshotSwitchContext(ctx)
		if err == nil {
			finish()
		}
		switched <- err
	}()
	finishCapture()
	select {
	case err := <-switched:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the final switch did not take the gate once the capture ended")
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
	peer, err := cluster.NewPlainClient(server.URL, "source", "test-cluster-token", time.Second)
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
	peer, err := cluster.NewPlainClient(server.URL, "source", "test-cluster-token", time.Second)
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
	peer, err := cluster.NewPlainClient(server.URL, "source", "test-cluster-token", time.Second)
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

func TestReserveRemoteTargetWaitsForNewEpochLease(t *testing.T) {
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
