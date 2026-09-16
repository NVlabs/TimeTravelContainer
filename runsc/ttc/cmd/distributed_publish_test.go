// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestProviderBaselineReleaseWaitsForSharedAuthority(t *testing.T) {
	object := ttcsnapshot.ObjectID{Snapshot: 17, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	state := &cluster.AuthorityState{VolatileOrigins: map[uint64]cluster.SnapshotProvider{17: {NodeID: "node-a"}}}
	for _, name := range []string{"root/filesystem/branch-a", "pin/open/branch-a"} {
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("volatile provider baseline %q was releasable", name)
		}
		delete(state.VolatileOrigins, 17)
		if !providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("shared provider baseline %q was not releasable", name)
		}
		state.VolatileOrigins[17] = cluster.SnapshotProvider{NodeID: "node-a"}
	}
	delete(state.VolatileOrigins, 17)
	for _, name := range []string{"remote/17/operation/00000000", "edge/parent/0000", "root/bridge/edge/parent/0000", "root/cold/memory"} {
		if providerBaselineCanBeReleased(state, name, object) {
			t.Fatalf("non-provider reference %q was releasable", name)
		}
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
		if err := ledger.ImportOrigin(origin, []ttcsnapshot.Object{{ID: root, Kind: ttcsnapshot.ObjectKindMetadata, State: ttcsnapshot.ObjectLive}}, references); err != nil {
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

func TestPublicationBridgePinsProviderRootsAcrossHandoff(t *testing.T) {
	provider := ttcsnapshot.NewReferenceLedger()
	shared := ttcsnapshot.NewReferenceLedger()
	origin := ttcsnapshot.SnapshotID(17)
	root := ttcsnapshot.ObjectID{Snapshot: origin, Kind: ttcsnapshot.ObjectKindMetadata, Ordinal: 1}
	objects := []ttcsnapshot.Object{{ID: root, Kind: ttcsnapshot.ObjectKindMetadata, State: ttcsnapshot.ObjectLive}}
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
	if matched := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(bridge.Operation); !matched {
		t.Fatalf("publication bridge operation %q is not a valid cluster operation ID", bridge.Operation)
	}
	if err := shared.ApplyReference(bridge.Operation, bridge.Name, bridge.Object, true); err != nil {
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

func TestDistributedSourceMonitorPollingIsBounded(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	finishes := make([]func(), 256)
	for i := range finishes {
		finishes[i] = runtime.beginSourceMonitor()
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
	for _, finish := range finishes {
		finish()
	}
	if got := runtime.monitorCount.Load(); got != 0 {
		t.Fatalf("monitor count=%d, want 0", got)
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
