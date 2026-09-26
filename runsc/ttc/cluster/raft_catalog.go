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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

// RaftCatalog presents TTC's typed membership, placement, transaction, and
// snapshot APIs over one replicated authority. All mutations are Raft
// proposals and all reads are linearizable leader reads.
type RaftCatalog struct {
	node         *AuthorityRaftNode
	leaderClient func(string) (*Client, error)
}

func NewRaftCatalog(node *AuthorityRaftNode) (*RaftCatalog, error) {
	if node == nil {
		return nil, errors.New("nil Raft authority node")
	}
	return &RaftCatalog{node: node}, nil
}

// SetLeaderClientResolver enables follower daemons to forward authority reads
// and proposals without making the caller aware of Raft leadership.
func (c *RaftCatalog) SetLeaderClientResolver(resolve func(string) (*Client, error)) {
	c.leaderClient = resolve
}

func (c *RaftCatalog) propose(ctx context.Context, requestID string, kind AuthorityCommandKind, payload any) (AuthorityResult, error) {
	deadline := time.Now().Add(15 * time.Second)
	result, err := c.node.Propose(ctx, requestID, kind, payload)
	for err != nil && time.Now().Before(deadline) {
		var redirect *AuthorityNotLeaderError
		if !errors.As(err, &redirect) {
			// A follower may know the elected leader before that daemon has
			// finished opening its HTTP control endpoint during a simultaneous
			// restart. The request ID makes a lost or refused forwarded proposal
			// safe to retry through the local Raft view.
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
		if c.leaderClient == nil {
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
		// LeaderWithID may briefly be empty while a removed learner catches up
		// through the configuration entry that re-admits it. Let the resolver
		// choose any configured seed in that case; a non-leader seed returns the
		// ordinary redirect carrying the elected leader's identity.
		peer, resolveErr := c.leaderClient(redirect.LeaderID)
		if resolveErr != nil {
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
		encoded, encodeErr := json.Marshal(payload)
		if encodeErr != nil {
			return AuthorityResult{}, encodeErr
		}
		result, err = peer.ProposeAuthority(ctx, AuthorityProposal{RequestID: requestID, Kind: kind, Payload: encoded})
	}
	return result, err
}

func (c *RaftCatalog) linearizableState(ctx context.Context) (*AuthorityState, error) {
	deadline := time.Now().Add(15 * time.Second)
	state, err := c.node.LinearizableState(ctx)
	for err != nil && time.Now().Before(deadline) {
		var redirect *AuthorityNotLeaderError
		if !errors.As(err, &redirect) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableState(ctx)
			continue
		}
		if c.leaderClient == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableState(ctx)
			continue
		}
		peer, resolveErr := c.leaderClient(redirect.LeaderID)
		if resolveErr != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableState(ctx)
			continue
		}
		state, err = peer.AuthorityState(ctx)
	}
	if err == nil {
		normalizeAuthorityLeaseClock(state, time.Now().UTC())
	}
	return state, err
}

func normalizeAuthorityLeaseClock(state *AuthorityState, localNow time.Time) {
	if state == nil || state.LeaderObservedAt.IsZero() {
		return
	}
	leaderNow := state.LeaderObservedAt
	for id, record := range state.Nodes {
		record.LeaseExpiresAt = localNow.Add(record.LeaseExpiresAt.Sub(leaderNow))
		record.UpdatedAt = localNow.Add(record.UpdatedAt.Sub(leaderNow))
		record.StartedAt = localNow.Add(record.StartedAt.Sub(leaderNow))
		for operation, reservation := range record.IncomingReservations {
			reservation.ExpiresAt = localNow.Add(reservation.ExpiresAt.Sub(leaderNow))
			if !reservation.CompletedAt.IsZero() {
				reservation.CompletedAt = localNow.Add(reservation.CompletedAt.Sub(leaderNow))
			}
			record.IncomingReservations[operation] = reservation
		}
		state.Nodes[id] = record
	}
	if !state.SharedGC.ExpiresAt.IsZero() {
		state.SharedGC.ExpiresAt = localNow.Add(state.SharedGC.ExpiresAt.Sub(leaderNow))
	}
	state.LeaderObservedAt = localNow
}

var _ NodeCatalog = (*RaftCatalog)(nil)

func newAuthorityRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(random[:]), nil
}

func applyRaftValue[T any](catalog *RaftCatalog, _ time.Time, kind AuthorityCommandKind, payload any) (*T, error) {
	id, err := newAuthorityRequestID()
	if err != nil {
		return nil, err
	}
	result, err := catalog.propose(context.Background(), id, kind, payload)
	if err != nil {
		return nil, err
	}
	if err := result.Err(); err != nil {
		return nil, err
	}
	var value T
	if err := json.Unmarshal(result.Value, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func applyRaftNoValue(catalog *RaftCatalog, _ time.Time, kind AuthorityCommandKind, payload any) error {
	id, err := newAuthorityRequestID()
	if err != nil {
		return err
	}
	result, err := catalog.propose(context.Background(), id, kind, payload)
	if err != nil {
		return err
	}
	return result.Err()
}

func (c *RaftCatalog) state() (*AuthorityState, error) {
	return c.linearizableState(context.Background())
}

// AuthorityState returns a linearizable copy of the complete bounded cluster
// control state. Bulk objects and object-level references are deliberately not
// part of this snapshot.
func (c *RaftCatalog) AuthorityState() (*AuthorityState, error) {
	return c.state()
}

// LinearizableState is AuthorityState with caller-controlled cancellation.
// Admission probes use it on every peer: followers transparently forward to
// the current leader through the catalog's configured resolver, so a lagging
// local FSM cannot reject an identity that registration already committed.
func (c *RaftCatalog) LinearizableState(ctx context.Context) (*AuthorityState, error) {
	return c.linearizableState(ctx)
}

// AppliedTransaction is a local, non-linearizable monitor read that avoids
// copying the complete authority state.
func (c *RaftCatalog) AppliedTransaction(id string) (Transaction, bool) {
	if c == nil || c.node == nil {
		return Transaction{}, false
	}
	return c.node.AppliedTransaction(id)
}

// AppliedNode is the node-record counterpart to AppliedTransaction.
func (c *RaftCatalog) AppliedNode(id string) (NodeRecord, bool) {
	if c == nil || c.node == nil {
		return NodeRecord{}, false
	}
	return c.node.AppliedNode(id)
}

// AllocateSnapshotOrigins allocates globally monotonic IDs and records this
// node as their exact volatile provider in one Raft command.
func (c *RaftCatalog) AllocateSnapshotOrigins(node, incarnation string, count uint32) (SnapshotAllocation, error) {
	value, err := applyRaftValue[SnapshotAllocation](c, time.Now().UTC(), AuthoritySnapshotAllocate, AuthoritySnapshotPayload{
		NodeID: node, Incarnation: incarnation, Count: count,
	})
	if err != nil {
		return SnapshotAllocation{}, err
	}
	return *value, nil
}

// AbandonSnapshotOrigin removes an unused allocation from the volatile
// provider directory. IDs remain below NextSnapshotID and are never reused;
// this operation is only for rolling back work that published no root.
func (c *RaftCatalog) AbandonSnapshotOrigin(node, incarnation string, snapshotID uint64) error {
	return applyRaftNoValue(c, time.Now().UTC(), AuthoritySnapshotAbandon, AuthoritySnapshotPayload{
		NodeID: node, Incarnation: incarnation, SnapshotID: snapshotID,
	})
}

// PublishSnapshotOrigin commits the final provider-to-shared authority switch.
// The caller must durably install and validate the shared manifest first.
func (c *RaftCatalog) PublishSnapshotOrigin(node, incarnation string, snapshotID uint64) error {
	return applyRaftNoValue(c, time.Now().UTC(), AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: node, Incarnation: incarnation, SnapshotID: snapshotID,
	})
}

// SnapshotProvider returns the exact volatile provider. found=false means the
// origin must resolve from its committed shared manifest.
func (c *RaftCatalog) SnapshotProvider(snapshotID uint64) (provider SnapshotOriginProvider, found bool, err error) {
	state, err := c.state()
	if err != nil {
		return SnapshotOriginProvider{}, false, err
	}
	if snapshotID == 0 || snapshotID >= state.NextSnapshotID {
		return SnapshotOriginProvider{}, false, os.ErrNotExist
	}
	if _, retired := state.RetiredOrigins[snapshotID]; retired {
		return SnapshotOriginProvider{}, false, os.ErrNotExist
	}
	if state.AbandonedOrigins[snapshotID] {
		return SnapshotOriginProvider{}, false, os.ErrNotExist
	}
	provider, found = state.VolatileOrigins[snapshotID]
	return provider, found, nil
}

// AcquireSharedGCLease becomes the sole shared-pack mutator for at most lease.
// A new acquisition always receives a fresh generation, including after an
// explicit release by the same daemon.
func (c *RaftCatalog) AcquireSharedGCLease(node, incarnation string, lease time.Duration) (SharedGCLease, error) {
	value, err := applyRaftValue[SharedGCLease](c, time.Now().UTC(), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: node, Incarnation: incarnation, Lease: lease,
	})
	if err != nil {
		return SharedGCLease{}, err
	}
	return *value, nil
}

// RenewSharedGCLease extends an unexpired generation owned by this daemon.
func (c *RaftCatalog) RenewSharedGCLease(lease SharedGCLease, duration time.Duration) (SharedGCLease, error) {
	value, err := applyRaftValue[SharedGCLease](c, time.Now().UTC(), AuthoritySharedGCRenew, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation, Lease: duration,
	})
	if err != nil {
		return SharedGCLease{}, err
	}
	return *value, nil
}

// ReleaseSharedGCLease relinquishes one exact generation. A stale holder can
// never release a successor's lease.
func (c *RaftCatalog) ReleaseSharedGCLease(lease SharedGCLease) error {
	return applyRaftNoValue(c, time.Now().UTC(), AuthoritySharedGCRelease, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation,
	})
}

// RetireSharedSnapshot permanently removes one proven-unreachable shared
// origin from routing before its physical packs are deleted.
func (c *RaftCatalog) RetireSharedSnapshot(lease SharedGCLease, snapshotID uint64) (SharedPackRetirement, error) {
	value, err := applyRaftValue[SharedPackRetirement](c, time.Now().UTC(), AuthoritySharedGCRetire, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation, SnapshotID: snapshotID,
	})
	if err != nil {
		return SharedPackRetirement{}, err
	}
	return *value, nil
}

// SharedGCLease returns the current replicated lease. Callers must still
// compare ExpiresAt with the normalized local observation time.
func (c *RaftCatalog) SharedGCLease() (SharedGCLease, error) {
	state, err := c.state()
	if err != nil {
		return SharedGCLease{}, err
	}
	return state.SharedGC, nil
}

// RetiredSharedSnapshots returns permanently fenced origins in stable order.
// Coordinators use this to resume physical deletion after a crash.
func (c *RaftCatalog) RetiredSharedSnapshots() ([]SharedPackRetirement, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	result := make([]SharedPackRetirement, 0, len(state.RetiredOrigins))
	for _, retirement := range state.RetiredOrigins {
		result = append(result, retirement)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SnapshotID < result[j].SnapshotID })
	return result, nil
}

// EnsurePublicationPreparing commits the user-visible lifecycle transition
// before any asynchronous shared bytes are written. The branch remains owned
// by its current node until CommitPublication atomically installs durable
// roots and releases placement.
func (c *RaftCatalog) EnsurePublicationPreparing(family, branch, publicationID, lifecycle string) (*PublicationRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	key := publicationKey(family, branch, publicationID)
	if record, exists := state.Publications[key]; exists {
		if record.PublicationID != publicationID || record.Lifecycle != lifecycle {
			return nil, errors.New("publication identity has conflicting authority")
		}
		if record.State == PublicationCanceled || record.State == PublicationFailed {
			return nil, errors.New("publication identity is not usable")
		}
		return &record, nil
	}
	branchAuthority, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return applyRaftValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: family, Branch: branch, PublicationID: publicationID, Lifecycle: lifecycle,
		BranchGeneration: branchAuthority.Generation,
	})
}

// EnsurePublicationReady authorizes one immutable publication and records that its
// node-local capture fence is complete. It is intentionally retryable after a
// daemon crash; no shared bytes may be uploaded before it succeeds.
func (c *RaftCatalog) EnsurePublicationReady(family, branch, publicationID, lifecycle string, captureRoots DurableRoots) (*PublicationRecord, error) {
	recordPointer, err := c.EnsurePublicationPreparing(family, branch, publicationID, lifecycle)
	if err != nil {
		return nil, err
	}
	record := *recordPointer
	switch record.State {
	case PublicationPreparing:
		return applyRaftValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationReady, AuthorityPublicationPayload{
			Family: family, Branch: branch, PublicationID: record.PublicationID,
			BranchGeneration: record.BranchGeneration, Revision: record.Revision, Roots: captureRoots,
		})
	case PublicationReady, PublicationCommitted:
		if record.State == PublicationReady && record.CandidateRoots != captureRoots {
			return nil, errors.New("publication capture roots differ")
		}
		return &record, nil
	default:
		return nil, errors.New("publication identity is not usable")
	}
}

// CommitPublication atomically installs immutable roots and releases runtime
// placement. A worker cannot resurrect a removed/superseded branch because
// both the branch incarnation and publication identity are checked by Raft.
func (c *RaftCatalog) CommitPublication(family, branch, publicationID string, roots DurableRoots, providerNode, providerIncarnation string, snapshotOrigins []uint64) (*PublicationRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Publications[publicationKey(family, branch, publicationID)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.State == PublicationCommitted {
		if record.CandidateRoots != roots {
			return nil, errors.New("committed publication roots differ")
		}
		return &record, nil
	}
	return applyRaftValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: family, Branch: branch, PublicationID: record.PublicationID,
		BranchGeneration: record.BranchGeneration,
		Revision:         record.Revision, Roots: roots, ProviderNode: providerNode, ProviderIncarnation: providerIncarnation,
		SnapshotOrigins: append([]uint64(nil), snapshotOrigins...),
	})
}

// CancelPublication fences an uncommitted worker. A successful publication is
// immutable and remains reachable from the committed branch roots.
func (c *RaftCatalog) CancelPublication(family, branch, publicationID string) error {
	state, err := c.state()
	if err != nil {
		return err
	}
	record, ok := state.Publications[publicationKey(family, branch, publicationID)]
	if !ok || record.State == PublicationCommitted || record.State == PublicationCanceled {
		return nil
	}
	return applyRaftNoValue(c, time.Now().UTC(), AuthorityPublicationCancel, AuthorityPublicationPayload{
		Family: family, Branch: branch, PublicationID: record.PublicationID,
		BranchGeneration: record.BranchGeneration, Revision: record.Revision,
	})
}

// Publication returns one immutable publication's authoritative state.
func (c *RaftCatalog) Publication(family, branch, publicationID string) (PublicationRecord, bool, error) {
	state, err := c.state()
	if err != nil {
		return PublicationRecord{}, false, err
	}
	record, ok := state.Publications[publicationKey(family, branch, publicationID)]
	return record, ok, nil
}

// MarkBranchCorrupted permanently fences a branch whose authoritative state
// has been lost. Its placement and pending publications are revoked in the
// same log entry.
func (c *RaftCatalog) MarkBranchCorrupted(family, branch, reason string) (*BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.Corrupted {
		return &record, nil
	}
	return applyRaftValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchCorrupt, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation, Failure: reason,
	})
}

// DiscardDehydratedMemory converts a durable process snapshot into an exited
// filesystem snapshot without materializing it. The generation change fences
// a concurrent rehydrate, and removing the memory root makes its immutable
// chunks eligible for ordinary shared-GC tracing.
func (c *RaftCatalog) DiscardDehydratedMemory(family, branch string) (*BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.Lifecycle == "exited" && record.Roots.MemoryRoot == "" && record.Roots.MemorySnapshot == 0 {
		return &record, nil
	}
	return applyRaftValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchDiscardMemory, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation,
	})
}

// Branches returns a linearizable snapshot of logical branch authority. Bulk
// memory and filesystem objects are intentionally absent; only lifecycle,
// fencing generations, durable roots, and coarse volatile dependencies live
// in the replicated control plane.
func (c *RaftCatalog) Branches() ([]BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	return state.BranchList(), nil
}

// Branch returns one linearizable logical branch record.
func (c *RaftCatalog) Branch(family, branch string) (*BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

// VolatileDependencyForwarded reports whether transactionID's former target
// has been superseded by an in-flight downstream migration. It is evaluated
// from one applied Raft snapshot so placement and dependency edges cannot be
// observed from different revisions.
func (c *RaftCatalog) VolatileDependencyForwarded(family, branch, transactionID, formerNode string) (bool, error) {
	state, err := c.state()
	if err != nil {
		return false, err
	}
	return volatileDependencyForwarded(state, family, branch, transactionID, formerNode), nil
}

// RemoveCorruptedBranch removes a permanently fenced branch from authority.
// Healthy branches must use the ordinary lifecycle/placement transaction.
func (c *RaftCatalog) RemoveCorruptedBranch(family, branch string) error {
	record, err := c.Branch(family, branch)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !record.Corrupted {
		return errors.New("branch is not corrupted")
	}
	err = applyRaftNoValue(c, time.Now().UTC(), AuthorityBranchRemoveCorrupted, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation,
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// InheritBranchDependencies copies the source's current coarse dependency
// edges exactly once when a local CoW child is enrolled. Parent is only an
// input to this creation event; it does not become an ownership relationship.
func (c *RaftCatalog) InheritBranchDependencies(family, branch, sourceBranch string) (*BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.DependenciesInherited {
		return &record, nil
	}
	return applyRaftValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchInherit, AuthorityBranchInheritPayload{
		Family: family, Branch: branch, SourceBranch: sourceBranch, BranchGeneration: record.Generation,
	})
}

func (c *RaftCatalog) ExpireNode(nodeID, incarnation string, revision uint64) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, time.Now().UTC(), AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: nodeID, Incarnation: incarnation, Revision: revision,
	})
}

// RemoveNode permanently removes a cleanly retired daemon incarnation from
// cluster discovery. It is distinct from expiry: abnormal, expired identities
// remain visible for diagnosis until an operator completes a clean retirement.
func (c *RaftCatalog) RemoveNode(nodeID, incarnation string, revision uint64) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, time.Now().UTC(), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: nodeID, Incarnation: incarnation, Revision: revision,
	})
}

func (c *RaftCatalog) ConfigureClusterPolicy(expectedRevision uint64, policy ClusterRuntimePolicy) (*ClusterRuntimePolicy, error) {
	return applyRaftValue[ClusterRuntimePolicy](c, time.Now().UTC(), AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{
		ExpectedRevision: expectedRevision, Policy: policy,
	})
}

func (c *RaftCatalog) Register(record NodeRecord, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: lease})
}

func (c *RaftCatalog) Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityNodeRenew, AuthorityNodeRenewPayload{NodeID: nodeID, Incarnation: incarnation, MemoryCurrentBytes: current, LeaseDuration: lease})
}

// HeartbeatState renews a node's execution lease and returns a local authority
// snapshot that includes the committed renewal. Unlike a subsequent
// LinearizableState call, this never adds another leader round trip after the
// lease has already been granted. Lease guardians use the returned snapshot to
// enforce any preceding execution-grant revocation before accepting the new
// local deadline.
func (c *RaftCatalog) HeartbeatState(ctx context.Context, nodeID, incarnation string, current uint64, lease time.Duration) (*NodeRecord, *AuthorityState, error) {
	id, err := newAuthorityRequestID()
	if err != nil {
		return nil, nil, err
	}
	result, err := c.propose(ctx, id, AuthorityNodeRenew, AuthorityNodeRenewPayload{
		NodeID: nodeID, Incarnation: incarnation, MemoryCurrentBytes: current, LeaseDuration: lease,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := result.Err(); err != nil {
		return nil, nil, err
	}
	var record NodeRecord
	if err := json.Unmarshal(result.Value, &record); err != nil {
		return nil, nil, err
	}
	// A forwarded proposal may receive the leader's response just before this
	// follower applies the same committed entry. Prefer the local FSM so an
	// ordinary healthy follower adds no round trip. A newly re-admitted learner
	// may still be replaying the removal/rejoin portion of its WAL, however; in
	// that bounded state it must use a linearizable leader snapshot rather than
	// self-fence merely because local application trails the granted lease.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	localDeadline := time.Now().Add(100 * time.Millisecond)
	for {
		state, err := c.node.AppliedState()
		if err != nil {
			return nil, nil, err
		}
		if state.Revision >= result.Revision {
			return &record, state, nil
		}
		if !time.Now().Before(localDeadline) {
			authoritative, err := c.linearizableState(ctx)
			if err != nil {
				return nil, nil, err
			}
			if authoritative.Revision < result.Revision {
				return nil, nil, errors.New("linearizable authority state predates committed heartbeat")
			}
			return &record, authoritative, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *RaftCatalog) ReserveIncoming(nodeID, incarnation, operation, sourceNode string, bytes uint64, now time.Time, ttl time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityCapacityReserve, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation, SourceNode: sourceNode, Bytes: bytes, TTL: ttl})
}

func (c *RaftCatalog) ReleaseIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityCapacityRelease, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation})
}

func (c *RaftCatalog) CompleteIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityCapacityComplete, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation})
}

func (c *RaftCatalog) Transition(nodeID, incarnation string, next NodeState, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: nodeID, Incarnation: incarnation, State: next, LeaseDuration: lease})
}

func (c *RaftCatalog) Lookup(nodeID string) (*NodeRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Nodes[nodeID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

func (c *RaftCatalog) List() ([]NodeRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	records := make([]NodeRecord, 0, len(state.Nodes))
	for _, record := range state.Nodes {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].NodeID < records[j].NodeID })
	return records, nil
}

func (c *RaftCatalog) Create(record PlacementRecord, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: record})
}

func (c *RaftCatalog) BeginMigration(family, branch, owner, incarnation string, generation uint64, operation, target string, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementBegin, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, Operation: operation, Target: target})
}

func (c *RaftCatalog) AdoptOwnerIncarnation(family, branch, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementAdopt, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, OldIncarnation: oldIncarnation, NewIncarnation: newIncarnation, Generation: generation})
}

func (c *RaftCatalog) RecoverSourceMigration(family, branch, operation, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementRecover, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation, Owner: owner, OldIncarnation: oldIncarnation, NewIncarnation: newIncarnation, Generation: generation})
}

func (c *RaftCatalog) CommitMigration(family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementCommit, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation, Target: target, TargetIncarnation: targetIncarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch})
}

// CutoverMigration atomically validates the published target transaction and
// transfers logical placement authority. No filesystem catalog can implement
// this safely; clustered TTC always uses the single Raft authority.
func (c *RaftCatalog) CutoverMigration(transactionID string, transactionRevision uint64, family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string) (*MigrationCutoverResult, error) {
	return applyRaftValue[MigrationCutoverResult](c, time.Now().UTC(), AuthorityMigrationCutover, AuthorityMigrationCutoverPayload{
		TransactionID: transactionID, TransactionRevision: transactionRevision,
		Family: family, Branch: branch, Operation: operation, Target: target,
		TargetIncarnation: targetIncarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch,
	})
}

func (c *RaftCatalog) AbortMigration(family, branch, operation string, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementAbort, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation})
}

func (c *RaftCatalog) Unplace(family, branch, owner, incarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementUnplace, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation})
}

func (c *RaftCatalog) ClaimUnplaced(family, branch, owner, incarnation, physicalContainer, physicalBranch string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementClaim, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch, Generation: generation, State: state})
}

func (c *RaftCatalog) UpdateStableState(family, branch, owner, incarnation string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementStableState, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, State: state})
}

func (c *RaftCatalog) SetQuarantine(family, branch, owner, incarnation string, generation uint64, quarantined bool, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementQuarantine, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, Quarantined: quarantined})
}

func (c *RaftCatalog) Rename(family, branch, newBranch, owner, incarnation, physicalBranch string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyRaftValue[PlacementRecord](c, now, AuthorityPlacementRename, AuthorityPlacementPayload{
		Family: family, Branch: branch, NewBranch: newBranch,
		Owner: owner, Incarnation: incarnation, PhysicalBranch: physicalBranch,
		Generation: generation,
	})
}

func (c *RaftCatalog) Remove(family, branch string, generation uint64) error {
	err := applyRaftNoValue(c, time.Now().UTC(), AuthorityPlacementRemove, AuthorityPlacementPayload{Family: family, Branch: branch, Generation: generation})
	// Reconciliation is intentionally performed by every node from independent
	// linearizable snapshots. Another node may remove the same stale placement
	// first; absence is the desired final state and matches PlacementStore's
	// idempotent Remove contract.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *RaftCatalog) LookupPlacement(family, branch string) (*PlacementRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Placements[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if authority, ok := state.Branches[logicalBranchKey(family, branch)]; ok && authority.Corrupted {
		record.State = PlacementCorrupted
		record.OwnerNode, record.OwnerIncarnation = "", ""
		record.PhysicalContainer, record.PhysicalBranch = "", ""
		record.OperationID, record.TargetNode, record.ResumeState = "", "", ""
	}
	return &record, nil
}

// Lookup implements PlacementCatalog. Go cannot overload NodeCatalog.Lookup,
// so callers use a typed PlacementCatalog view whose method is provided by the
// placementCatalogAdapter below.
type raftPlacementCatalog struct{ *RaftCatalog }

var _ PlacementCatalog = raftPlacementCatalog{}

func (r raftPlacementCatalog) Lookup(family, branch string) (*PlacementRecord, error) {
	return r.LookupPlacement(family, branch)
}

func (r raftPlacementCatalog) List() ([]PlacementRecord, error) {
	state, err := r.state()
	if err != nil {
		return nil, err
	}
	return placementRecords(state), nil
}

// ListWithOfflineOwners is List, together with the nodes that are offline,
// read from the same authority state. A running or paused branch of an
// offline node is unavailable until the node returns or is removed.
func (r raftPlacementCatalog) ListWithOfflineOwners() ([]PlacementRecord, map[string]bool, error) {
	state, err := r.state()
	if err != nil {
		return nil, nil, err
	}
	return state.PlacementList(), state.OfflineNodes(), nil
}

// PlacementList returns the placements of s ordered by family and branch, as
// the placement catalog lists them.
func (s *AuthorityState) PlacementList() []PlacementRecord {
	return placementRecords(s)
}

// OfflineNodes returns the nodes of s that are offline.
func (s *AuthorityState) OfflineNodes() map[string]bool {
	offline := make(map[string]bool)
	for id, node := range s.Nodes {
		if node.State == NodeOffline {
			offline[id] = true
		}
	}
	return offline
}

// BranchList returns the branch records of s ordered by family and branch,
// as Branches lists them.
func (s *AuthorityState) BranchList() []BranchAuthority {
	records := make([]BranchAuthority, 0, len(s.Branches))
	for _, record := range s.Branches {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].FamilyID != records[j].FamilyID {
			return records[i].FamilyID < records[j].FamilyID
		}
		return records[i].BranchID < records[j].BranchID
	})
	return records
}

func placementRecords(state *AuthorityState) []PlacementRecord {
	records := make([]PlacementRecord, 0, len(state.Placements))
	for key, record := range state.Placements {
		if authority, ok := state.Branches[key]; ok && authority.Corrupted {
			record.State = PlacementCorrupted
			record.OwnerNode, record.OwnerIncarnation = "", ""
			record.PhysicalContainer, record.PhysicalBranch = "", ""
			record.OperationID, record.TargetNode, record.ResumeState = "", "", ""
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].FamilyID != records[j].FamilyID {
			return records[i].FamilyID < records[j].FamilyID
		}
		return records[i].BranchID < records[j].BranchID
	})
	return records
}

// Placements returns the placement-specific view. NodeCatalog and
// PlacementCatalog both historically used method names Lookup and List with
// incompatible signatures.
func (c *RaftCatalog) Placements() PlacementCatalog { return raftPlacementCatalog{c} }

func (c *RaftCatalog) CreateTransaction(tx *Transaction) error {
	if tx == nil {
		return errors.New("nil transaction")
	}
	return applyRaftNoValue(c, tx.CreatedAt, AuthorityTransactionCreate, AuthorityTransactionPayload{Transaction: *tx})
}

func (c *RaftCatalog) LoadTransaction(id string) (*Transaction, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	tx, ok := state.Transactions[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &tx, nil
}

func (c *RaftCatalog) AdvanceTransaction(id string, revision uint64, next TransactionState, failure string, metrics *TransferMetrics, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](c, now, AuthorityTransactionAdvance, AuthorityTransactionPayload{ID: id, Revision: revision, Next: next, Failure: failure, Metrics: metrics})
}

type raftTransactionCatalog struct{ *RaftCatalog }

var _ TransactionCatalog = raftTransactionCatalog{}
var _ AtomicTargetPublisher = raftTransactionCatalog{}

func (r raftTransactionCatalog) Create(tx *Transaction) error         { return r.CreateTransaction(tx) }
func (r raftTransactionCatalog) Load(id string) (*Transaction, error) { return r.LoadTransaction(id) }
func (r raftTransactionCatalog) Advance(id string, revision uint64, next TransactionState, failure string, now time.Time) (*Transaction, error) {
	return r.AdvanceTransaction(id, revision, next, failure, nil, now)
}
func (r raftTransactionCatalog) AdvanceIndependent(id string, revision uint64, metrics TransferMetrics, now time.Time) (*Transaction, error) {
	return r.AdvanceTransaction(id, revision, TransactionIndependent, "", &metrics, now)
}
func (r raftTransactionCatalog) Publish(id string, revision uint64, desiredPaused, activated bool, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, now, AuthorityTransactionPublish, AuthorityTransactionPayload{ID: id, Revision: revision, DesiredPaused: desiredPaused, Activated: activated})
}
func (r raftTransactionCatalog) PublishMigrationTarget(id string, revision uint64, desiredPaused bool, physicalContainer, physicalBranch, targetIncarnation string) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, time.Now().UTC(), AuthorityTransactionPublish, AuthorityTransactionPayload{
		ID: id, Revision: revision, DesiredPaused: desiredPaused,
		PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch, TargetIncarnation: targetIncarnation,
	})
}
func (r raftTransactionCatalog) PublishTarget(id string, revision uint64, desiredPaused bool, family, branch, physicalContainer, physicalBranch, targetIncarnation string) (*Transaction, error) {
	result, err := applyRaftValue[ForkPublishResult](r.RaftCatalog, time.Now().UTC(), AuthorityForkPublish, AuthorityForkPublishPayload{
		TransactionID: id, TransactionRevision: revision, DesiredPaused: desiredPaused,
		Family: family, Branch: branch, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch,
		TargetIncarnation: targetIncarnation,
	})
	if err != nil {
		return nil, err
	}
	return &result.Transaction, nil
}
func (r raftTransactionCatalog) MarkActivated(id string, revision uint64, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, now, AuthorityTransactionActivate, AuthorityTransactionPayload{ID: id, Revision: revision})
}
func (r raftTransactionCatalog) BindSession(id string, revision uint64, digest string, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, now, AuthorityTransactionSession, AuthorityTransactionPayload{ID: id, Revision: revision, Digest: digest})
}
func (r raftTransactionCatalog) BindGraphHandoff(id string, revision uint64, graph GraphHandoffDescriptor, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, now, AuthorityTransactionHandoff, AuthorityTransactionPayload{ID: id, Revision: revision, Graph: &graph})
}
func (r raftTransactionCatalog) Delete(id string, revision uint64) error {
	return applyRaftNoValue(r.RaftCatalog, time.Now().UTC(), AuthorityTransactionRetire, AuthorityTransactionPayload{ID: id, Revision: revision})
}
func (r raftTransactionCatalog) List() ([]Transaction, error) {
	state, err := r.state()
	if err != nil {
		return nil, err
	}
	records := make([]Transaction, 0, len(state.Transactions))
	for _, tx := range state.Transactions {
		records = append(records, tx)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Identity.TransactionID < records[j].Identity.TransactionID })
	return records, nil
}

// Transactions returns the transaction-specific view.
func (c *RaftCatalog) Transactions() TransactionCatalog { return raftTransactionCatalog{c} }
