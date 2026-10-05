// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHolder is a node holding the authority of an epoch of a cluster in a
// temporary shared root.
type testHolder struct {
	node      *AuthorityNode
	shared    SharedCluster
	heartbeat *Heartbeat
	record    AuthorityRecord
}

// holdTestEpoch makes node, whose heartbeat runs in shared, the holder of
// epoch, prepared from previous.
func holdTestEpoch(t *testing.T, shared SharedCluster, id string, epoch, previous uint64) *testHolder {
	t.Helper()
	heartbeat, self := startTestMember(t, shared, id, "i-1", RoleCompute)
	self.Epoch, self.Previous = epoch, previous
	if won, err := shared.ClaimEpoch(self); err != nil || !won {
		t.Fatalf("claiming epoch %d: %t, %v", epoch, won, err)
	}
	if err := shared.WriteAuthority(self); err != nil {
		t.Fatal(err)
	}
	if err := shared.PrepareEpoch(previous, epoch); err != nil {
		t.Fatal(err)
	}
	holder := AuthorityHolder{Cluster: shared, Heartbeat: heartbeat, Record: self}
	node := NewAuthorityNode(id, 0, shared.ReadAuthority)
	if err := node.Hold(AuthorityOptions{NodeID: id, Dir: shared.EpochDir(epoch), Epoch: epoch, Held: holder.Held}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	return &testHolder{node: node, shared: shared, heartbeat: heartbeat, record: self}
}

func newTestHolder(t *testing.T) *testHolder {
	t.Helper()
	return holdTestEpoch(t, NewSharedCluster(t.TempDir()), "node-a", 1, 0)
}

func registerReadyNode(t *testing.T, holder *AuthorityNode, id, incarnation string) {
	t.Helper()
	at := time.Now().UTC()
	node := testAuthorityNode(at)
	node.NodeID, node.Incarnation, node.Capabilities.NodeID = id, incarnation, id
	for _, entry := range []struct {
		id      string
		kind    AuthorityCommandKind
		payload any
	}{
		{"register-state-" + id, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: node, LeaseDuration: time.Hour}},
		{"ready-state-" + id, AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: id, Incarnation: incarnation, State: NodeReady, LeaseDuration: time.Hour}},
	} {
		result, err := holder.Propose(context.Background(), entry.id, entry.kind, entry.payload)
		if err != nil || result.Err() != nil {
			t.Fatalf("applying %s: result=%+v err=%v", entry.kind, result, err)
		}
	}
}

func TestAuthorityAppliedObjectLookups(t *testing.T) {
	holder := newTestHolder(t)
	engine := holder.node.current.Load()
	engine.fsm.mu.Lock()
	engine.fsm.state.Transactions["fork-1"] = Transaction{Identity: ForkIdentity{TransactionID: "fork-1"}, Revision: 7}
	engine.fsm.state.Nodes["node-a"] = NodeRecord{NodeID: "node-a", Incarnation: "inc-a", State: NodeReady}
	engine.fsm.mu.Unlock()
	tx, ok := holder.node.AppliedTransaction("fork-1")
	if !ok || tx.Revision != 7 {
		t.Fatalf("transaction=(%+v, %t), want revision 7", tx, ok)
	}
	record, ok := holder.node.AppliedNode("node-a")
	if !ok || record.Incarnation != "inc-a" || record.State != NodeReady {
		t.Fatalf("node=(%+v, %t), want ready inc-a", record, ok)
	}
	if _, ok := holder.node.AppliedTransaction("missing"); ok {
		t.Fatal("missing transaction was reported present")
	}
}

// A holder that has held the authority for less than a node's lease waits it
// out before it lets another incarnation of the node register, for the old
// incarnation may still run what its lease let it; not for an incarnation
// that is offline.
func TestRegisteringAnotherIncarnationWaitsOnlyForOneThatMayRun(t *testing.T) {
	node := newTestHolder(t).node
	propose := func(id string, kind AuthorityCommandKind, payload any) error {
		result, err := node.Propose(context.Background(), id, kind, payload)
		if err != nil {
			return err
		}
		return result.Err()
	}
	register := func(id, incarnation string) error {
		record := testAuthorityNode(time.Now().UTC())
		record.NodeID, record.Incarnation, record.Capabilities.NodeID = id, incarnation, id
		return propose("register-"+id+"-"+incarnation, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: time.Hour})
	}
	registerReadyNode(t, node, "node-b", "i-1")
	if err := register("node-b", "i-2"); err == nil || !strings.Contains(err.Error(), "self-fence") {
		t.Fatalf("registering another incarnation of a ready node: %v, want it to wait", err)
	}
	if err := propose("offline-node-b", AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: "node-b", Incarnation: "i-1", State: NodeOffline, LeaseDuration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := register("node-b", "i-2"); err != nil {
		t.Fatalf("registering another incarnation of an offline node: %v", err)
	}
}

func TestAuthorityFSMReadCopyOmitsReplayHistory(t *testing.T) {
	fsm := NewAuthorityFSM()
	fsm.state.Revision = 7
	fsm.state.Requests["request-1"] = appliedAuthorityRequest{Digest: "digest"}
	fsm.state.RequestOrder = []string{"request-1"}

	state, err := fsm.copyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 7 {
		t.Fatalf("read copy lost authority state: %+v", state)
	}
	if len(state.Requests) != 0 || len(state.RequestOrder) != 0 {
		t.Fatalf("read copy exposed replay history: requests=%d order=%d", len(state.Requests), len(state.RequestOrder))
	}
	if len(fsm.state.Requests) != 1 || len(fsm.state.RequestOrder) != 1 {
		t.Fatal("read copy mutated the authority's replay history")
	}
}

// The state remembers the last maxRememberedRequests requests, and a state
// a command replaced keeps the window it had.
func TestAuthorityStateRemembersAWindowOfRequests(t *testing.T) {
	state := NewAuthorityState()
	for i := 0; i < maxRememberedRequests; i++ {
		state.remember(fmt.Sprintf("r-%d", i), "digest", AuthorityResult{})
	}
	before := *state
	for i := maxRememberedRequests; i < maxRememberedRequests+100; i++ {
		state.remember(fmt.Sprintf("r-%d", i), "digest", AuthorityResult{})
	}
	if len(state.Requests) != maxRememberedRequests || len(state.RequestOrder) != maxRememberedRequests {
		t.Fatalf("remembered %d requests in an order of %d, want %d", len(state.Requests), len(state.RequestOrder), maxRememberedRequests)
	}
	if state.RequestOrder[0] != "r-100" || state.RequestOrder[maxRememberedRequests-1] != fmt.Sprintf("r-%d", maxRememberedRequests+99) {
		t.Fatalf("window is %s to %s, want r-100 to r-%d", state.RequestOrder[0], state.RequestOrder[maxRememberedRequests-1], maxRememberedRequests+99)
	}
	if _, ok := state.Requests["r-99"]; ok {
		t.Fatal("remembered a request older than the window")
	}
	if before.RequestOrder[0] != "r-0" || before.RequestOrder[len(before.RequestOrder)-1] != fmt.Sprintf("r-%d", maxRememberedRequests-1) {
		t.Fatalf("the earlier state's window changed to %s to %s", before.RequestOrder[0], before.RequestOrder[len(before.RequestOrder)-1])
	}
}

// One check serves the callers that came while the one before it ran, and
// none is served by a check that began before it called.
func TestHeldFenceBatchesChecks(t *testing.T) {
	var fence heldFence
	var mu sync.Mutex
	checks := 0
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	held := func() error {
		mu.Lock()
		checks++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- fence.wait(held) }()
	<-started
	var arrived, waiters sync.WaitGroup
	later := make(chan error, 5)
	for i := 0; i < 5; i++ {
		arrived.Add(1)
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			arrived.Done()
			later <- fence.wait(held)
		}()
	}
	// Let the five callers queue behind the running check.
	arrived.Wait()
	time.Sleep(50 * time.Millisecond)
	release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case <-later:
		t.Fatal("a caller was served by the check that ran when it called")
	case <-started:
	}
	release <- struct{}{}
	waiters.Wait()
	close(later)
	for err := range later {
		if err != nil {
			t.Fatal(err)
		}
	}
	if checks != 2 {
		t.Fatalf("%d checks for six callers in two rounds, want 2", checks)
	}
}

// A holder another node took the authority from sends requests to that node,
// as a node that never held it does: the batch that finds the authority gone,
// every request after it, and reads of what it applied.
func TestAuthorityLostRedirectsToTheHolder(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	successor := AuthorityRecord{Epoch: 2, Previous: 1, NodeID: "node-b", Incarnation: "i-1", ControlURL: "http://node-b"}
	if err := holder.shared.WriteAuthority(successor); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, err := holder.node.Propose(context.Background(), fmt.Sprintf("after-%d", i), AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
			FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
			OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
		}})
		var redirect *AuthorityNotHolderError
		if !errors.As(err, &redirect) || redirect.HolderID != "node-b" || redirect.ControlURL != "http://node-b" {
			t.Fatalf("proposal %d after the takeover: %v, want a redirect to node-b", i, err)
		}
	}
	if holder.node.Holds() {
		t.Fatal("the node still holds the authority")
	}
	var redirect *AuthorityNotHolderError
	if _, err := holder.node.LinearizableState(context.Background()); !errors.As(err, &redirect) || redirect.HolderID != "node-b" {
		t.Fatalf("read after the takeover: %v, want a redirect to node-b", err)
	}
	if _, err := holder.node.AppliedState(); !errors.As(err, &redirect) {
		t.Fatalf("applied state after the takeover: %v, want a redirect", err)
	}
	if _, ok := holder.node.AppliedNode("node-a"); ok {
		t.Fatal("the node answered from the state it applied before the takeover")
	}
}

// A holder that gives the authority up answers as a node that never held it,
// before it records that it did.
func TestAuthorityRelinquished(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	holder.node.Relinquish()
	if holder.node.Holds() {
		t.Fatal("the node still holds the authority it gave up")
	}
	var redirect *AuthorityNotHolderError
	if _, err := holder.node.LinearizableState(context.Background()); !errors.As(err, &redirect) {
		t.Fatalf("a read after the authority was given up = %v, want a redirect", err)
	}
}

func TestAuthorityLinearizableApply(t *testing.T) {
	node := newTestHolder(t).node
	registerReadyNode(t, node, "node-a", "incarnation-a")
	result, err := node.Propose(context.Background(), "placement-create", AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	state, err := node.LinearizableState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	placement := state.Placements[logicalBranchKey("family", "main")]
	if placement.OwnerNode != "node-a" || placement.Generation != 1 {
		t.Fatalf("unexpected placement: %+v", placement)
	}
	catalog, err := NewAuthorityCatalog(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Remove("family", "main", placement.Generation); err != nil {
		t.Fatalf("first placement removal: %v", err)
	}
	if err := catalog.Remove("family", "main", placement.Generation); err != nil {
		t.Fatalf("idempotent placement removal: %v", err)
	}
	created, err := catalog.Create(PlacementRecord{
		FamilyID: "family", BranchID: "doomed", PhysicalContainer: "physical-doomed", PhysicalBranch: "doomed",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}, time.Now())
	if err != nil {
		t.Fatalf("creating doomed branch: %v", err)
	}
	if _, err := catalog.MarkBranchCorrupted("family", "doomed", "provider lost"); err != nil {
		t.Fatalf("corrupting branch: %v", err)
	}
	if err := catalog.RemoveCorruptedBranch("family", "doomed"); err != nil {
		t.Fatalf("first corrupted branch removal: %v", err)
	}
	if err := catalog.RemoveCorruptedBranch("family", "doomed"); err != nil {
		t.Fatalf("idempotent corrupted branch removal: %v", err)
	}
	if created.Generation == 0 {
		t.Fatal("created doomed branch without a generation")
	}
}

// A command the authority refuses for its node's lease renews the lease, once
// for the commands refused at once, and is proposed again; one refused for
// another reason is not.
func TestAuthorityCatalogRenewsTheLeaseACommandWasRefusedFor(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-b", "incarnation-b")
	catalog, err := NewAuthorityCatalog(holder.node)
	if err != nil {
		t.Fatal(err)
	}
	lapse := func() {
		t.Helper()
		if _, err := catalog.Heartbeat("node-b", "incarnation-b", 0, time.Now(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	place := func(branch string) error {
		_, err := catalog.Create(PlacementRecord{
			FamilyID: "family", BranchID: branch, PhysicalContainer: "family", PhysicalBranch: branch,
			OwnerNode: "node-b", OwnerIncarnation: "incarnation-b", State: PlacementMaterializing,
		}, time.Now())
		return err
	}
	lapse()
	if err := place("refused"); !IsNoExecutionLease(err) {
		t.Fatalf("a placement on a lapsed lease, renewing none: %v, want a refusal for the lease", err)
	}
	var mu sync.Mutex
	renewals := 0
	catalog.SetLeaseRenewal(func() error {
		mu.Lock()
		renewals++
		mu.Unlock()
		_, err := catalog.Heartbeat("node-b", "incarnation-b", 0, time.Now(), time.Hour)
		return err
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return renewals
	}
	if err := place("main"); err != nil {
		t.Fatalf("a placement on a lapsed lease: %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
	if err := place("main"); err == nil || IsNoExecutionLease(err) {
		t.Fatalf("a placement of a branch placed already: %v, want a refusal other than for the lease", err)
	}
	if n := count(); n != 1 {
		t.Errorf("a refusal for another reason renewed the lease: %d renewals, want 1", n)
	}
	lapse()
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = place(fmt.Sprintf("b%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("placement b%d, one of 8 at once on a lapsed lease: %v", i, err)
		}
	}
	if n := count(); n != 2 {
		t.Errorf("%d renewals after 8 commands at once on a lapsed lease, want 2", n)
	}
}

// A publication's capture is made ready by the command that queues it and by
// the queue's worker, at once: both succeed.
func TestAuthorityCatalogEnsurePublicationReadyConcurrently(t *testing.T) {
	node := newTestHolder(t).node
	registerReadyNode(t, node, "node-a", "incarnation-a")
	catalog, err := NewAuthorityCatalog(node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	roots := DurableRoots{FilesystemRoot: "cold/manifests/family/main/0000000000000001.json"}
	const callers = 8
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			record, err := catalog.EnsurePublicationReady("family", "main", "publication-1", "exited", roots)
			if err == nil && record.State != PublicationReady {
				err = fmt.Errorf("publication is %s, want ready", record.State)
			}
			errs <- err
		}()
	}
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("making a capture ready concurrently: %v", err)
		}
	}
}

func TestAuthorityCatalogHeartbeatReturnsRenewedState(t *testing.T) {
	node := newTestHolder(t).node
	registerReadyNode(t, node, "node-a", "incarnation-a")
	catalog, err := NewAuthorityCatalog(node)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	record, state, err := catalog.HeartbeatState(ctx, "node-a", "incarnation-a", 1234, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if record.MemoryCurrentBytes != 1234 || record.LeaseEpoch == 0 {
		t.Fatalf("unexpected renewed record: %+v", record)
	}
	applied, ok := state.Nodes["node-a"]
	if !ok || applied.Revision != record.Revision || applied.MemoryCurrentBytes != record.MemoryCurrentBytes {
		t.Fatalf("returned state does not include renewal: record=%+v state=%+v", record, applied)
	}
}

// A node that does not hold the authority names the one that does.
func TestAuthorityNodeWithoutTheAuthorityRedirects(t *testing.T) {
	holder := AuthorityRecord{Epoch: 3, NodeID: "node-b", ControlURL: "http://127.0.0.1:1", ControlServerName: "node-b"}
	node := NewAuthorityNode("node-a", 0, func() (AuthorityRecord, bool, error) { return holder, true, nil })
	for name, call := range map[string]func() error{
		"apply": func() error { _, err := node.Apply(context.Background(), AuthorityCommand{}); return err },
		"propose": func() error {
			_, err := node.Propose(context.Background(), "r-1", AuthorityPlacementCreate, AuthorityPlacementPayload{})
			return err
		},
		"read": func() error { _, err := node.LinearizableState(context.Background()); return err },
	} {
		var redirect *AuthorityNotHolderError
		if err := call(); !errors.As(err, &redirect) || redirect.HolderID != "node-b" || redirect.ControlURL != holder.ControlURL {
			t.Errorf("%s: %v, want a redirect to node-b", name, err)
		}
	}
	if node.Holds() {
		t.Fatal("a node holding nothing holds the authority")
	}
}

// The holder acknowledges a write only while it holds its lease and no later
// epoch has taken the authority; a record of an earlier epoch, which a
// stalled claimant wrote over its own, it restores.
func TestAuthorityHolderStopsWhenTheAuthorityIsLost(t *testing.T) {
	holder := newTestHolder(t)
	node := holder.node
	propose := func(id string) error {
		result, err := node.Propose(context.Background(), id, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
			FamilyID: "family", BranchID: id, PhysicalContainer: "family", PhysicalBranch: id,
			OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
		}})
		if err != nil {
			return err
		}
		return result.Err()
	}
	registerReadyNode(t, node, "node-a", "incarnation-a")
	if err := holder.shared.WriteAuthority(AuthorityRecord{Epoch: 0, NodeID: "node-c"}); err != nil {
		t.Fatal(err)
	}
	if err := propose("b1"); err != nil {
		t.Fatal(err)
	}
	if current, _, err := holder.shared.ReadAuthority(); err != nil || current.Epoch != 1 || current.NodeID != "node-a" {
		t.Fatalf("authority record %+v, %v, want epoch 1's restored", current, err)
	}
	// A later epoch takes the authority.
	if err := holder.shared.WriteAuthority(AuthorityRecord{Epoch: 2, NodeID: "node-b"}); err != nil {
		t.Fatal(err)
	}
	var redirect *AuthorityNotHolderError
	if err := propose("b2"); !errors.As(err, &redirect) || redirect.HolderID != "node-b" {
		t.Fatalf("a write after a later epoch took the authority = %v, want a redirect to node-b", err)
	}
	if node.Holds() {
		t.Fatal("the holder still holds the authority it lost")
	}
	if _, err := node.LinearizableState(context.Background()); !errors.As(err, &redirect) || redirect.HolderID != "node-b" {
		t.Fatalf("a read after the authority was lost = %v, want a redirect to node-b", err)
	}
}

func TestAuthorityHolderStopsWhenItsLeaseLapses(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	holder.heartbeat.Stop()
	holder.heartbeat.mu.Lock()
	holder.heartbeat.landed = time.Now().Add(-LeaseHold)
	holder.heartbeat.mu.Unlock()
	// The record still names the node: the authority has no other holder to
	// send the read to.
	var redirect *AuthorityNotHolderError
	if _, err := holder.node.LinearizableState(context.Background()); !errors.As(err, &redirect) || redirect.HolderID != "" {
		t.Fatalf("a read after the lease lapsed = %v, want a redirect to no holder", err)
	}
	if holder.node.Holds() {
		t.Fatal("the holder still holds the authority after its lease lapsed")
	}
}

// Commands proposed at once all commit, a batch at a time, and the log holds
// them in the order the holder applied them.
func TestAuthorityConcurrentProposalsReplayAlike(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	const callers = 64
	var group sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			branch := fmt.Sprintf("b%d", i)
			result, err := holder.node.Propose(context.Background(), "create-"+branch, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
				FamilyID: "family", BranchID: branch, PhysicalContainer: "family", PhysicalBranch: branch,
				OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
			}})
			if err == nil {
				err = result.Err()
			}
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	want, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.node.Close(); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := loadAuthorityState(holder.shared.EpochDir(1), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != want.Revision || len(state.Placements) != callers {
		t.Fatalf("replayed revision %d with %d placements, want %d with %d", state.Revision, len(state.Placements), want.Revision, callers)
	}
}
