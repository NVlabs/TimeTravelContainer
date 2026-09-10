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

// RaftCatalog is the compatibility facade used while TTC call sites move from
// three filesystem stores to one replicated authority. All mutations are Raft
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
		if redirect.LeaderID == "" || c.leaderClient == nil {
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
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
		if redirect.LeaderID == "" || c.leaderClient == nil {
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

func publicationOperationID(generation uint64) string {
	return "publication-" + time.Unix(int64(generation), 0).UTC().Format("20060102T150405")
}

// EnsurePublicationReady authorizes one generation and records that its
// node-local capture fence is complete. It is intentionally retryable after a
// daemon crash; no shared bytes may be uploaded before it succeeds.
func (c *RaftCatalog) EnsurePublicationReady(family, branch string, storageGeneration uint64, lifecycle string) (*PublicationRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	key := publicationKey(family, branch, storageGeneration)
	record, exists := state.Publications[key]
	operation := publicationOperationID(storageGeneration)
	if !exists {
		branchAuthority, ok := state.Branches[logicalBranchKey(family, branch)]
		if !ok {
			return nil, os.ErrNotExist
		}
		created, err := applyRaftValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationBegin, AuthorityPublicationPayload{
			Family: family, Branch: branch, Operation: operation, Lifecycle: lifecycle,
			BranchGeneration: branchAuthority.Generation, StorageGeneration: storageGeneration,
		})
		if err != nil {
			return nil, err
		}
		record = *created
	}
	if record.OperationID != operation || record.Lifecycle != lifecycle {
		return nil, errors.New("publication generation has conflicting authority")
	}
	switch record.State {
	case PublicationPreparing:
		return applyRaftValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationReady, AuthorityPublicationPayload{
			Family: family, Branch: branch, Operation: operation,
			BranchGeneration: record.BranchGeneration, StorageGeneration: storageGeneration, Revision: record.Revision,
		})
	case PublicationReady, PublicationCommitted:
		return &record, nil
	default:
		return nil, errors.New("publication generation is not usable")
	}
}

// CommitPublication atomically installs immutable roots and releases runtime
// placement. A worker cannot resurrect a removed/superseded branch because
// both branch and storage generations are checked by the Raft state machine.
func (c *RaftCatalog) CommitPublication(family, branch string, storageGeneration uint64, roots DurableRoots) (*PublicationRecord, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	record, ok := state.Publications[publicationKey(family, branch, storageGeneration)]
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
		Family: family, Branch: branch, Operation: record.OperationID,
		BranchGeneration: record.BranchGeneration, StorageGeneration: storageGeneration,
		Revision: record.Revision, Roots: roots,
	})
}

// CancelPublication fences an uncommitted worker. A successful publication is
// immutable and remains reachable from the committed branch roots.
func (c *RaftCatalog) CancelPublication(family, branch string, storageGeneration uint64) error {
	state, err := c.state()
	if err != nil {
		return err
	}
	record, ok := state.Publications[publicationKey(family, branch, storageGeneration)]
	if !ok || record.State == PublicationCommitted || record.State == PublicationCanceled {
		return nil
	}
	return applyRaftNoValue(c, time.Now().UTC(), AuthorityPublicationCancel, AuthorityPublicationPayload{
		Family: family, Branch: branch, Operation: record.OperationID,
		BranchGeneration: record.BranchGeneration, StorageGeneration: storageGeneration, Revision: record.Revision,
	})
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

// Branches returns a linearizable snapshot of logical branch authority. Bulk
// memory and filesystem objects are intentionally absent; only lifecycle,
// fencing generations, durable roots, and coarse volatile dependencies live
// in the replicated control plane.
func (c *RaftCatalog) Branches() ([]BranchAuthority, error) {
	state, err := c.state()
	if err != nil {
		return nil, err
	}
	records := make([]BranchAuthority, 0, len(state.Branches))
	for _, record := range state.Branches {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].FamilyID != records[j].FamilyID {
			return records[i].FamilyID < records[j].FamilyID
		}
		return records[i].BranchID < records[j].BranchID
	})
	return records, nil
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

// RemoveCorruptedBranch removes a permanently fenced branch from authority.
// Healthy branches must use the ordinary lifecycle/placement transaction.
func (c *RaftCatalog) RemoveCorruptedBranch(family, branch string) error {
	record, err := c.Branch(family, branch)
	if err != nil {
		return err
	}
	if !record.Corrupted {
		return errors.New("branch is not corrupted")
	}
	return applyRaftNoValue(c, time.Now().UTC(), AuthorityBranchRemoveCorrupted, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation,
	})
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

func (c *RaftCatalog) Register(record NodeRecord, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: lease})
}

func (c *RaftCatalog) Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyRaftValue[NodeRecord](c, now, AuthorityNodeRenew, AuthorityNodeRenewPayload{NodeID: nodeID, Incarnation: incarnation, MemoryCurrentBytes: current, LeaseDuration: lease})
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

func (c *RaftCatalog) Remove(family, branch string, generation uint64) error {
	return applyRaftNoValue(c, time.Now().UTC(), AuthorityPlacementRemove, AuthorityPlacementPayload{Family: family, Branch: branch, Generation: generation})
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
	return records, nil
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
func (r raftTransactionCatalog) BindSparseHandoff(id string, revision uint64, sparse SparseHandoffDescriptor, now time.Time) (*Transaction, error) {
	return applyRaftValue[Transaction](r.RaftCatalog, now, AuthorityTransactionHandoff, AuthorityTransactionPayload{ID: id, Revision: revision, Sparse: &sparse})
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
