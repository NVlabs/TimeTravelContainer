// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

// AuthorityCatalog presents TTC's typed membership, placement, transaction,
// and snapshot APIs over the cluster authority. Every mutation is a command
// of its log and every read a linearizable read of its holder.
type AuthorityCatalog struct {
	node *AuthorityNode
	// holderClient returns the client of the authority's holder; retry is
	// set once a request sent to it failed.
	holderClient func(retry bool) (*Client, error)
	// renewLease renews the execution lease of the catalog's node; see
	// SetLeaseRenewal.
	renewLease func() error
	// renewing is held while the lease is renewed; renewedAt is when the
	// last renewal ended.
	renewing  sync.Mutex
	renewedAt time.Time
}

func NewAuthorityCatalog(node *AuthorityNode) (*AuthorityCatalog, error) {
	if node == nil {
		return nil, errors.New("nil cluster authority node")
	}
	return &AuthorityCatalog{node: node}, nil
}

// SetHolderClientResolver lets a daemon that does not hold the authority send
// its reads and proposals to the one that does, the caller unaware. resolve
// is told when a request it sent failed, as it does to a holder that no
// longer holds the authority.
func (c *AuthorityCatalog) SetHolderClientResolver(resolve func(retry bool) (*Client, error)) {
	c.holderClient = resolve
}

// SetLeaseRenewal lets the catalog renew its node's execution lease with
// renew when the authority refuses a command for it, and propose the command
// again. The authority takes a lease only in the term it was renewed in, and
// a node's heartbeat renews it every sixth of the lease: after another node
// took the authority over, every command of the node that needed its lease
// was refused until then, as every run on the other nodes was, for 4.4 s,
// after the holder stopped.
func (c *AuthorityCatalog) SetLeaseRenewal(renew func() error) {
	c.renewLease = renew
}

// proposeRenewing proposes a command, and again, once, if the authority
// refused it for the node's lease and the lease is renewed: by this command,
// or by another refused as it was, since it was first proposed.
func (c *AuthorityCatalog) proposeRenewing(kind AuthorityCommandKind, payload any) (AuthorityResult, error) {
	id, err := newAuthorityRequestID()
	if err != nil {
		return AuthorityResult{}, err
	}
	proposed := time.Now()
	result, err := c.propose(context.Background(), id, kind, payload)
	if err != nil || c.renewLease == nil || kind == AuthorityNodeRenew || !IsNoExecutionLease(result.Err()) {
		return result, err
	}
	c.renewing.Lock()
	renewErr := error(nil)
	if !c.renewedAt.After(proposed) {
		if renewErr = c.renewLease(); renewErr == nil {
			c.renewedAt = time.Now()
		}
	}
	c.renewing.Unlock()
	if renewErr != nil {
		return result, nil
	}
	if id, err = newAuthorityRequestID(); err != nil {
		return AuthorityResult{}, err
	}
	return c.propose(context.Background(), id, kind, payload)
}

func (c *AuthorityCatalog) propose(ctx context.Context, requestID string, kind AuthorityCommandKind, payload any) (AuthorityResult, error) {
	deadline := time.Now().Add(15 * time.Second)
	result, err := c.node.Propose(ctx, requestID, kind, payload)
	forwarded := false
	for err != nil && time.Now().Before(deadline) {
		var redirect *AuthorityNotHolderError
		if !errors.As(err, &redirect) {
			// A new holder may not have opened its control endpoint yet. The
			// request ID makes a lost or refused forwarded proposal safe to
			// retry.
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
		if c.holderClient == nil {
			select {
			case <-ctx.Done():
				return AuthorityResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			result, err = c.node.Propose(ctx, requestID, kind, payload)
			continue
		}
		peer, resolveErr := c.holderClient(forwarded)
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
		attempt, cancel := forwardAttempt(ctx, deadline)
		result, err = peer.ProposeAuthority(attempt, AuthorityProposal{RequestID: requestID, Kind: kind, Payload: encoded})
		cancel()
		forwarded = true
	}
	return result, err
}

// forwardAttemptTimeout bounds one request forwarded to the authority's
// holder: a holder that stalled accepts connections but answers none, and the
// next attempt asks whichever node holds the authority then, as the shared
// root names it. A node's lease renewal must reach the new holder in the few
// seconds between a takeover and the node's self-fence, so this is short; a
// proposal's request ID makes its retry safe.
const forwardAttemptTimeout = 2 * time.Second

func forwardAttempt(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if attempt := time.Now().Add(forwardAttemptTimeout); attempt.Before(deadline) {
		deadline = attempt
	}
	return context.WithDeadline(ctx, deadline)
}

// linearizableQuery reads the records of the authority state that query
// names, linearizably: from this node if it holds the authority, and
// otherwise from the node that does, which sends only those.
func (c *AuthorityCatalog) linearizableQuery(ctx context.Context, query AuthorityQuery) (*AuthorityState, error) {
	deadline := time.Now().Add(15 * time.Second)
	state, err := c.node.LinearizableQuery(ctx, query)
	forwarded := false
	for err != nil && time.Now().Before(deadline) {
		var redirect *AuthorityNotHolderError
		if !errors.As(err, &redirect) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableQuery(ctx, query)
			continue
		}
		if c.holderClient == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableQuery(ctx, query)
			continue
		}
		peer, resolveErr := c.holderClient(forwarded)
		if resolveErr != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			state, err = c.node.LinearizableQuery(ctx, query)
			continue
		}
		attempt, cancel := forwardAttempt(ctx, deadline)
		state, err = peer.AuthorityQuery(attempt, query)
		cancel()
		forwarded = true
	}
	if err == nil {
		normalizeAuthorityLeaseClock(state, time.Now().UTC())
	}
	return state, err
}

func normalizeAuthorityLeaseClock(state *AuthorityState, localNow time.Time) {
	if state == nil || state.HolderObservedAt.IsZero() {
		return
	}
	holderNow := state.HolderObservedAt
	for id, record := range state.Nodes {
		record.LeaseExpiresAt = localNow.Add(record.LeaseExpiresAt.Sub(holderNow))
		record.UpdatedAt = localNow.Add(record.UpdatedAt.Sub(holderNow))
		record.StartedAt = localNow.Add(record.StartedAt.Sub(holderNow))
		for operation, reservation := range record.IncomingReservations {
			reservation.ExpiresAt = localNow.Add(reservation.ExpiresAt.Sub(holderNow))
			if !reservation.CompletedAt.IsZero() {
				reservation.CompletedAt = localNow.Add(reservation.CompletedAt.Sub(holderNow))
			}
			record.IncomingReservations[operation] = reservation
		}
		state.Nodes[id] = record
	}
	if !state.SharedGC.ExpiresAt.IsZero() {
		state.SharedGC.ExpiresAt = localNow.Add(state.SharedGC.ExpiresAt.Sub(holderNow))
	}
	state.HolderObservedAt = localNow
}

var _ NodeCatalog = (*AuthorityCatalog)(nil)

func newAuthorityRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(random[:]), nil
}

func applyAuthorityValue[T any](catalog *AuthorityCatalog, _ time.Time, kind AuthorityCommandKind, payload any) (*T, error) {
	result, err := catalog.proposeRenewing(kind, payload)
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

func applyAuthorityNoValue(catalog *AuthorityCatalog, _ time.Time, kind AuthorityCommandKind, payload any) error {
	result, err := catalog.proposeRenewing(kind, payload)
	if err != nil {
		return err
	}
	return result.Err()
}

func (c *AuthorityCatalog) query(query AuthorityQuery) (*AuthorityState, error) {
	return c.linearizableQuery(context.Background(), query)
}

// AuthorityState returns a linearizable copy of the complete bounded cluster
// control state. Bulk objects and object-level references are deliberately not
// part of this snapshot.
func (c *AuthorityCatalog) AuthorityState() (*AuthorityState, error) {
	return c.query(WholeAuthority)
}

// AuthorityStateOf returns a linearizable copy of the records of the
// authority state that query names.
func (c *AuthorityCatalog) AuthorityStateOf(query AuthorityQuery) (*AuthorityState, error) {
	return c.query(query)
}

// LinearizableState is AuthorityState with caller-controlled cancellation.
// Admission probes use it on every peer: a node that does not hold the
// authority forwards the read to the holder through the catalog's configured
// resolver, so a stale view cannot reject an identity that registration
// already committed.
func (c *AuthorityCatalog) LinearizableState(ctx context.Context) (*AuthorityState, error) {
	return c.linearizableQuery(ctx, WholeAuthority)
}

// AppliedTransaction is a local, non-linearizable monitor read that avoids
// copying the complete authority state.
func (c *AuthorityCatalog) AppliedTransaction(id string) (Transaction, bool) {
	if c == nil || c.node == nil {
		return Transaction{}, false
	}
	return c.node.AppliedTransaction(id)
}

// AppliedNode is the node-record counterpart to AppliedTransaction.
func (c *AuthorityCatalog) AppliedNode(id string) (NodeRecord, bool) {
	if c == nil || c.node == nil {
		return NodeRecord{}, false
	}
	return c.node.AppliedNode(id)
}

// AllocateSnapshotOrigins allocates globally monotonic IDs and records this
// node as their exact volatile provider in one authority command.
func (c *AuthorityCatalog) AllocateSnapshotOrigins(node, incarnation string, count uint32) (SnapshotAllocation, error) {
	value, err := applyAuthorityValue[SnapshotAllocation](c, time.Now().UTC(), AuthoritySnapshotAllocate, AuthoritySnapshotPayload{
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
func (c *AuthorityCatalog) AbandonSnapshotOrigin(node, incarnation string, snapshotID uint64) error {
	return applyAuthorityNoValue(c, time.Now().UTC(), AuthoritySnapshotAbandon, AuthoritySnapshotPayload{
		NodeID: node, Incarnation: incarnation, SnapshotID: snapshotID,
	})
}

// PublishSnapshotOrigin commits the final provider-to-shared authority switch.
// The caller must durably install and validate the shared manifest first.
func (c *AuthorityCatalog) PublishSnapshotOrigin(node, incarnation string, snapshotID uint64) error {
	return applyAuthorityNoValue(c, time.Now().UTC(), AuthoritySnapshotPublish, AuthoritySnapshotPayload{
		NodeID: node, Incarnation: incarnation, SnapshotID: snapshotID,
	})
}

// SnapshotProvider returns the exact volatile provider. found=false means the
// origin must resolve from its committed shared manifest.
func (c *AuthorityCatalog) SnapshotProvider(snapshotID uint64) (provider SnapshotOriginProvider, found bool, err error) {
	state, err := c.query(OriginsQuery(snapshotID))
	if err != nil {
		return SnapshotOriginProvider{}, false, err
	}
	if snapshotID == 0 || snapshotID >= state.NextSnapshotID || state.OriginDead(snapshotID) {
		return SnapshotOriginProvider{}, false, os.ErrNotExist
	}
	provider, found = state.VolatileOrigins[snapshotID]
	return provider, found, nil
}

// AcquireSharedGCLease becomes the sole shared-pack mutator for at most lease.
// A new acquisition always receives a fresh generation, including after an
// explicit release by the same daemon.
func (c *AuthorityCatalog) AcquireSharedGCLease(node, incarnation string, lease time.Duration) (SharedGCLease, error) {
	value, err := applyAuthorityValue[SharedGCLease](c, time.Now().UTC(), AuthoritySharedGCAcquire, AuthoritySharedGCPayload{
		NodeID: node, Incarnation: incarnation, Lease: lease,
	})
	if err != nil {
		return SharedGCLease{}, err
	}
	return *value, nil
}

// RenewSharedGCLease extends an unexpired generation owned by this daemon.
func (c *AuthorityCatalog) RenewSharedGCLease(lease SharedGCLease, duration time.Duration) (SharedGCLease, error) {
	value, err := applyAuthorityValue[SharedGCLease](c, time.Now().UTC(), AuthoritySharedGCRenew, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation, Lease: duration,
	})
	if err != nil {
		return SharedGCLease{}, err
	}
	return *value, nil
}

// ReleaseSharedGCLease relinquishes one exact generation. A stale holder can
// never release a successor's lease.
func (c *AuthorityCatalog) ReleaseSharedGCLease(lease SharedGCLease) error {
	return applyAuthorityNoValue(c, time.Now().UTC(), AuthoritySharedGCRelease, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation,
	})
}

// RetireSharedSnapshot permanently removes one proven-unreachable shared
// origin from routing before its physical packs are deleted.
func (c *AuthorityCatalog) RetireSharedSnapshot(lease SharedGCLease, snapshotID uint64) (SharedPackRetirement, error) {
	value, err := applyAuthorityValue[SharedPackRetirement](c, time.Now().UTC(), AuthoritySharedGCRetire, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation, SnapshotID: snapshotID,
	})
	if err != nil {
		return SharedPackRetirement{}, err
	}
	return *value, nil
}

// SharedGCLease returns the current committed lease. Callers must still
// compare ExpiresAt with the normalized local observation time.
func (c *AuthorityCatalog) SharedGCLease() (SharedGCLease, error) {
	state, err := c.query(AuthorityQuery{})
	if err != nil {
		return SharedGCLease{}, err
	}
	return state.SharedGC, nil
}

// RetiredSharedSnapshots returns, in stable order, the retirements whose
// deletion is yet to complete: a coordinator completes and forgets them,
// resuming the deletion of one that crashed.
func (c *AuthorityCatalog) RetiredSharedSnapshots() ([]SharedPackRetirement, error) {
	state, err := c.query(AuthorityQuery{RetiredOrigins: All[uint64]()})
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

// ForgetSharedRetirements forgets the retirements of snapshots, whose
// deletion is complete; they stay dead.
func (c *AuthorityCatalog) ForgetSharedRetirements(lease SharedGCLease, snapshots []uint64) error {
	return applyAuthorityNoValue(c, time.Now().UTC(), AuthoritySharedGCForget, AuthoritySharedGCPayload{
		NodeID: lease.NodeID, Incarnation: lease.Incarnation, Generation: lease.Generation, SnapshotIDs: snapshots,
	})
}

// EnsurePublicationPreparing commits the user-visible lifecycle transition
// before any asynchronous shared bytes are written. The branch remains owned
// by its current node until CommitPublication atomically installs durable
// roots and releases placement.
func (c *AuthorityCatalog) EnsurePublicationPreparing(family, branch, publicationID, lifecycle string) (*PublicationRecord, error) {
	state, err := c.query(AuthorityQuery{
		Publications: Only(publicationKey(family, branch, publicationID)),
		Branches:     Only(logicalBranchKey(family, branch)),
	})
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
	return applyAuthorityValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationBegin, AuthorityPublicationPayload{
		Family: family, Branch: branch, PublicationID: publicationID, Lifecycle: lifecycle,
		BranchGeneration: branchAuthority.Generation,
	})
}

// EnsurePublicationReady authorizes one immutable publication and records that its
// node-local capture fence is complete. It is intentionally retryable after a
// daemon crash; no shared bytes may be uploaded before it succeeds.
func (c *AuthorityCatalog) EnsurePublicationReady(family, branch, publicationID, lifecycle string, captureRoots DurableRoots) (*PublicationRecord, error) {
	for attempt := 0; ; attempt++ {
		recordPointer, err := c.EnsurePublicationPreparing(family, branch, publicationID, lifecycle)
		if err != nil {
			return nil, err
		}
		record := *recordPointer
		switch record.State {
		case PublicationPreparing:
			ready, err := applyAuthorityValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationReady, AuthorityPublicationPayload{
				Family: family, Branch: branch, PublicationID: record.PublicationID,
				BranchGeneration: record.BranchGeneration, Revision: record.Revision, Roots: captureRoots,
			})
			if err == nil || attempt == 2 {
				return ready, err
			}
			// Another caller may have made the capture ready at the revision
			// read above, as the publication queue's worker does while the
			// command that queued the publication does too: look again.
		case PublicationReady, PublicationCommitted:
			if record.State == PublicationReady && record.CandidateRoots != captureRoots {
				return nil, errors.New("publication capture roots differ")
			}
			return &record, nil
		default:
			return nil, errors.New("publication identity is not usable")
		}
	}
}

// CommitPublication atomically installs immutable roots and releases runtime
// placement. A worker cannot resurrect a removed/superseded branch because
// both the branch incarnation and publication identity are checked by the authority.
func (c *AuthorityCatalog) CommitPublication(family, branch, publicationID string, roots DurableRoots, providerNode, providerIncarnation string, snapshotOrigins []uint64) (*PublicationRecord, error) {
	state, err := c.query(AuthorityQuery{Publications: Only(publicationKey(family, branch, publicationID))})
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
	return applyAuthorityValue[PublicationRecord](c, time.Now().UTC(), AuthorityPublicationCommit, AuthorityPublicationPayload{
		Family: family, Branch: branch, PublicationID: record.PublicationID,
		BranchGeneration: record.BranchGeneration,
		Revision:         record.Revision, Roots: roots, ProviderNode: providerNode, ProviderIncarnation: providerIncarnation,
		SnapshotOrigins: append([]uint64(nil), snapshotOrigins...),
	})
}

// CancelPublication fences an uncommitted worker. A successful publication is
// immutable and remains reachable from the committed branch roots. The
// worker may advance the publication while it is canceled, and a
// cancellation of a revision the publication has left is refused; it is
// proposed again for the revision the publication has then.
func (c *AuthorityCatalog) CancelPublication(family, branch, publicationID string) error {
	_, err := c.CancelUncommittedPublication(family, branch, publicationID)
	return err
}

// CancelUncommittedPublication is CancelPublication, and reports whether the
// publication had committed, which no cancellation undoes.
func (c *AuthorityCatalog) CancelUncommittedPublication(family, branch, publicationID string) (committed bool, err error) {
	key := publicationKey(family, branch, publicationID)
	var refused error
	var revision uint64
	for {
		state, err := c.query(AuthorityQuery{Publications: Only(key)})
		if err != nil {
			return false, err
		}
		record, ok := state.Publications[key]
		if ok && record.State == PublicationCommitted {
			return true, nil
		}
		if !ok || record.State == PublicationCanceled {
			return false, nil
		}
		if refused != nil && record.Revision == revision {
			return false, refused
		}
		revision = record.Revision
		refused = applyAuthorityNoValue(c, time.Now().UTC(), AuthorityPublicationCancel, AuthorityPublicationPayload{
			Family: family, Branch: branch, PublicationID: record.PublicationID,
			BranchGeneration: record.BranchGeneration, Revision: record.Revision,
		})
		if refused == nil {
			return false, nil
		}
	}
}

// Publication returns one immutable publication's authoritative state.
func (c *AuthorityCatalog) Publication(family, branch, publicationID string) (PublicationRecord, bool, error) {
	state, err := c.query(AuthorityQuery{Publications: Only(publicationKey(family, branch, publicationID))})
	if err != nil {
		return PublicationRecord{}, false, err
	}
	record, ok := state.Publications[publicationKey(family, branch, publicationID)]
	return record, ok, nil
}

// MarkBranchCorrupted permanently fences a branch whose authoritative state
// has been lost. Its placement and pending publications are revoked in the
// same log entry.
func (c *AuthorityCatalog) MarkBranchCorrupted(family, branch, reason string) (*BranchAuthority, error) {
	state, err := c.query(AuthorityQuery{Branches: Only(logicalBranchKey(family, branch))})
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
	return applyAuthorityValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchCorrupt, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation, Failure: reason,
	})
}

// DiscardDehydratedMemory converts a durable process snapshot into an exited
// filesystem snapshot without materializing it. The generation change fences
// a concurrent rehydrate, and removing the memory root makes its immutable
// chunks eligible for ordinary shared-GC tracing.
func (c *AuthorityCatalog) DiscardDehydratedMemory(family, branch string) (*BranchAuthority, error) {
	state, err := c.query(AuthorityQuery{Branches: Only(logicalBranchKey(family, branch))})
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
	return applyAuthorityValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchDiscardMemory, AuthorityPublicationPayload{
		Family: family, Branch: branch, BranchGeneration: record.Generation,
	})
}

// Branches returns a linearizable snapshot of logical branch authority. Bulk
// memory and filesystem objects are intentionally absent; only lifecycle,
// fencing generations, durable roots, and coarse volatile dependencies live
// in the authority.
func (c *AuthorityCatalog) Branches() ([]BranchAuthority, error) {
	state, err := c.query(AuthorityQuery{Branches: All[string]()})
	if err != nil {
		return nil, err
	}
	return state.BranchList(), nil
}

// Branch returns one linearizable logical branch record.
func (c *AuthorityCatalog) Branch(family, branch string) (*BranchAuthority, error) {
	state, err := c.query(AuthorityQuery{Branches: Only(logicalBranchKey(family, branch))})
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
// from one applied authority snapshot so placement and dependency edges cannot be
// observed from different revisions.
func (c *AuthorityCatalog) VolatileDependencyForwarded(family, branch, transactionID, formerNode string) (bool, error) {
	key := logicalBranchKey(family, branch)
	state, err := c.query(AuthorityQuery{Branches: Only(key), Placements: Only(key), Transactions: All[string]()})
	if err != nil {
		return false, err
	}
	return volatileDependencyForwarded(state, family, branch, transactionID, formerNode), nil
}

// RemoveCorruptedBranch removes a permanently fenced branch from authority.
// Healthy branches must use the ordinary lifecycle/placement transaction.
func (c *AuthorityCatalog) RemoveCorruptedBranch(family, branch string) error {
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
	err = applyAuthorityNoValue(c, time.Now().UTC(), AuthorityBranchRemoveCorrupted, AuthorityPublicationPayload{
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
func (c *AuthorityCatalog) InheritBranchDependencies(family, branch, sourceBranch string) (*BranchAuthority, error) {
	state, err := c.query(AuthorityQuery{Branches: Only(logicalBranchKey(family, branch))})
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
	return applyAuthorityValue[BranchAuthority](c, time.Now().UTC(), AuthorityBranchInherit, AuthorityBranchInheritPayload{
		Family: family, Branch: branch, SourceBranch: sourceBranch, BranchGeneration: record.Generation,
	})
}

func (c *AuthorityCatalog) ExpireNode(nodeID, incarnation string, revision uint64) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, time.Now().UTC(), AuthorityNodeExpire, AuthorityNodeExpirePayload{
		NodeID: nodeID, Incarnation: incarnation, Revision: revision,
	})
}

// RemoveNode permanently removes a cleanly retired daemon incarnation from
// cluster discovery. It is distinct from expiry: abnormal, expired identities
// remain visible for diagnosis until an operator completes a clean retirement.
func (c *AuthorityCatalog) RemoveNode(nodeID, incarnation string, revision uint64) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, time.Now().UTC(), AuthorityNodeRemove, AuthorityNodeRemovePayload{
		NodeID: nodeID, Incarnation: incarnation, Revision: revision,
	})
}

func (c *AuthorityCatalog) ConfigureClusterPolicy(expectedRevision uint64, policy ClusterRuntimePolicy) (*ClusterRuntimePolicy, error) {
	return applyAuthorityValue[ClusterRuntimePolicy](c, time.Now().UTC(), AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{
		ExpectedRevision: expectedRevision, Policy: policy,
	})
}

func (c *AuthorityCatalog) Register(record NodeRecord, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityNodeRegister, AuthorityNodeRegisterPayload{Record: record, LeaseDuration: lease})
}

func (c *AuthorityCatalog) Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityNodeRenew, AuthorityNodeRenewPayload{NodeID: nodeID, Incarnation: incarnation, MemoryCurrentBytes: current, LeaseDuration: lease})
}

// HeartbeatState renews a node's execution lease and returns a local authority
// snapshot that includes the committed renewal. Unlike a subsequent
// LinearizableState call, this never adds another holder round trip after the
// lease has already been granted. Lease guardians use the returned snapshot to
// enforce any preceding execution-grant revocation before accepting the new
// local deadline.
func (c *AuthorityCatalog) HeartbeatState(ctx context.Context, nodeID, incarnation string, current uint64, lease time.Duration) (*NodeRecord, *AuthorityState, error) {
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
	// The node's execution grants, and its own record, renewed: the holder's
	// state has them already, and another node reads them from the holder.
	query := ExecutionGrantsOf(nodeID)
	query.Nodes = Only(nodeID)
	state, err := c.linearizableQuery(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	if state.Revision < result.Revision {
		return nil, nil, errors.New("linearizable authority state predates committed heartbeat")
	}
	return &record, state, nil
}

func (c *AuthorityCatalog) ReserveIncoming(nodeID, incarnation, operation, sourceNode string, bytes uint64, now time.Time, ttl time.Duration) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityCapacityReserve, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation, SourceNode: sourceNode, Bytes: bytes, TTL: ttl})
}

func (c *AuthorityCatalog) ReleaseIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityCapacityRelease, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation})
}

func (c *AuthorityCatalog) CompleteIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityCapacityComplete, AuthorityCapacityPayload{NodeID: nodeID, Incarnation: incarnation, OperationID: operation})
}

func (c *AuthorityCatalog) Transition(nodeID, incarnation string, next NodeState, now time.Time, lease time.Duration) (*NodeRecord, error) {
	return applyAuthorityValue[NodeRecord](c, now, AuthorityNodeTransition, AuthorityNodeTransitionPayload{NodeID: nodeID, Incarnation: incarnation, State: next, LeaseDuration: lease})
}

func (c *AuthorityCatalog) Lookup(nodeID string) (*NodeRecord, error) {
	state, err := c.query(AuthorityQuery{Nodes: Only(nodeID)})
	if err != nil {
		return nil, err
	}
	record, ok := state.Nodes[nodeID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

func (c *AuthorityCatalog) List() ([]NodeRecord, error) {
	state, err := c.query(AuthorityQuery{Nodes: All[string]()})
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

func (c *AuthorityCatalog) Create(record PlacementRecord, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: record})
}

func (c *AuthorityCatalog) BeginMigration(family, branch, owner, incarnation string, generation uint64, operation, target string, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementBegin, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, Operation: operation, Target: target})
}

func (c *AuthorityCatalog) AdoptOwnerIncarnation(family, branch, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementAdopt, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, OldIncarnation: oldIncarnation, NewIncarnation: newIncarnation, Generation: generation})
}

func (c *AuthorityCatalog) RecoverSourceMigration(family, branch, operation, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementRecover, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation, Owner: owner, OldIncarnation: oldIncarnation, NewIncarnation: newIncarnation, Generation: generation})
}

func (c *AuthorityCatalog) CommitMigration(family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementCommit, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation, Target: target, TargetIncarnation: targetIncarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch})
}

// CutoverMigration atomically validates the published target transaction and
// transfers logical placement authority. No filesystem catalog can implement
// this safely; clustered TTC always uses the cluster authority.
func (c *AuthorityCatalog) CutoverMigration(transactionID string, transactionRevision uint64, family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string) (*MigrationCutoverResult, error) {
	return applyAuthorityValue[MigrationCutoverResult](c, time.Now().UTC(), AuthorityMigrationCutover, AuthorityMigrationCutoverPayload{
		TransactionID: transactionID, TransactionRevision: transactionRevision,
		Family: family, Branch: branch, Operation: operation, Target: target,
		TargetIncarnation: targetIncarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch,
	})
}

func (c *AuthorityCatalog) AbortMigration(family, branch, operation string, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementAbort, AuthorityPlacementPayload{Family: family, Branch: branch, Operation: operation})
}

func (c *AuthorityCatalog) Unplace(family, branch, owner, incarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementUnplace, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation})
}

func (c *AuthorityCatalog) ClaimUnplaced(family, branch, owner, incarnation, physicalContainer, physicalBranch string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementClaim, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch, Generation: generation, State: state})
}

func (c *AuthorityCatalog) UpdateStableState(family, branch, owner, incarnation string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementStableState, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, State: state})
}

func (c *AuthorityCatalog) SetQuarantine(family, branch, owner, incarnation string, generation uint64, quarantined bool, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementQuarantine, AuthorityPlacementPayload{Family: family, Branch: branch, Owner: owner, Incarnation: incarnation, Generation: generation, Quarantined: quarantined})
}

func (c *AuthorityCatalog) Rename(family, branch, newBranch, owner, incarnation, physicalBranch string, generation uint64, now time.Time) (*PlacementRecord, error) {
	return applyAuthorityValue[PlacementRecord](c, now, AuthorityPlacementRename, AuthorityPlacementPayload{
		Family: family, Branch: branch, NewBranch: newBranch,
		Owner: owner, Incarnation: incarnation, PhysicalBranch: physicalBranch,
		Generation: generation,
	})
}

func (c *AuthorityCatalog) Remove(family, branch string, generation uint64) error {
	err := applyAuthorityNoValue(c, time.Now().UTC(), AuthorityPlacementRemove, AuthorityPlacementPayload{Family: family, Branch: branch, Generation: generation})
	// Reconciliation is intentionally performed by every node from independent
	// linearizable snapshots. Another node may remove the same stale placement
	// first; absence is the desired final state and matches PlacementStore's
	// idempotent Remove contract.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *AuthorityCatalog) LookupPlacement(family, branch string) (*PlacementRecord, error) {
	key := logicalBranchKey(family, branch)
	state, err := c.query(AuthorityQuery{Placements: Only(key), Branches: Only(key)})
	if err != nil {
		return nil, err
	}
	return state.PlacementOf(family, branch)
}

// PlacementOf is LookupPlacement of family's branch in s, which must hold the
// branch's placement and authority if the authority has them: a query of the
// family's placements and branches does.
func (s *AuthorityState) PlacementOf(family, branch string) (*PlacementRecord, error) {
	record, ok := s.Placements[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if authority, ok := s.Branches[logicalBranchKey(family, branch)]; ok && authority.Corrupted {
		record.State = PlacementCorrupted
		record.OwnerNode, record.OwnerIncarnation = "", ""
		record.PhysicalContainer, record.PhysicalBranch = "", ""
		record.OperationID, record.TargetNode, record.ResumeState = "", "", ""
	}
	return &record, nil
}

// ResolutionQuery is the query of what resolving a branch of a container
// reads: the branch's placement in its logical family, and its authority in
// the container's family and in the logical one, whose PlacementOf and
// BranchOf are LookupPlacement and Branch of each.
func ResolutionQuery(family, logical, branch string) AuthorityQuery {
	return AuthorityQuery{
		Placements: Only(logicalBranchKey(logical, branch)),
		Branches:   Only(logicalBranchKey(family, branch), logicalBranchKey(logical, branch)),
	}
}

// BranchOf is Branch of family's branch in s, which must hold the branch's
// authority if the authority has it: a query of the family's branches does.
func (s *AuthorityState) BranchOf(family, branch string) (*BranchAuthority, error) {
	record, ok := s.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

// PlacementsWithOwners is the query of every placement, with the branches
// and nodes that qualify it: whether a branch is corrupted, and whether its
// owner is offline.
var PlacementsWithOwners = AuthorityQuery{Placements: All[string](), Branches: All[string](), Nodes: All[string]()}

// ExecutionGrantsOf is the query of what the lease guardian of node reads its
// execution grants from: the placements it owns, and every transaction.
func ExecutionGrantsOf(node string) AuthorityQuery {
	return AuthorityQuery{Placements: All[string](), Owner: node, Transactions: All[string]()}
}

// RevokedGrants is the query of what the lease guardian checks grants that
// disappeared against: their placements and branches, whoever owns them now,
// and every transaction.
func RevokedGrants(keys []string) AuthorityQuery {
	return AuthorityQuery{Placements: Only(keys...), Branches: Only(keys...), Transactions: All[string]()}
}

// Lookup implements PlacementCatalog. Go cannot overload NodeCatalog.Lookup,
// so callers use a typed PlacementCatalog view whose method is provided by the
// placementCatalogAdapter below.
type authorityPlacementCatalog struct{ *AuthorityCatalog }

var _ PlacementCatalog = authorityPlacementCatalog{}

func (r authorityPlacementCatalog) Lookup(family, branch string) (*PlacementRecord, error) {
	return r.LookupPlacement(family, branch)
}

func (r authorityPlacementCatalog) List() ([]PlacementRecord, error) {
	state, err := r.query(AuthorityQuery{Placements: All[string](), Branches: All[string]()})
	if err != nil {
		return nil, err
	}
	return placementRecords(state), nil
}

func (r authorityPlacementCatalog) ListFamily(family string) ([]PlacementRecord, error) {
	state, err := r.query(AuthorityQuery{Placements: All[string](), Branches: All[string](), Family: family})
	if err != nil {
		return nil, err
	}
	return placementRecords(state), nil
}

// ListWithOfflineOwners is List, together with the nodes that are offline,
// read from the same authority state. A running or paused branch of an
// offline node is unavailable until the node returns or is removed.
func (r authorityPlacementCatalog) ListWithOfflineOwners() ([]PlacementRecord, map[string]bool, error) {
	state, err := r.query(PlacementsWithOwners)
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
func (c *AuthorityCatalog) Placements() PlacementCatalog { return authorityPlacementCatalog{c} }

func (c *AuthorityCatalog) CreateTransaction(tx *Transaction) error {
	if tx == nil {
		return errors.New("nil transaction")
	}
	return applyAuthorityNoValue(c, tx.CreatedAt, AuthorityTransactionCreate, AuthorityTransactionPayload{Transaction: *tx})
}

func (c *AuthorityCatalog) LoadTransaction(id string) (*Transaction, error) {
	state, err := c.query(AuthorityQuery{Transactions: Only(id)})
	if err != nil {
		return nil, err
	}
	tx, ok := state.Transactions[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &tx, nil
}

func (c *AuthorityCatalog) AdvanceTransaction(id string, revision uint64, next TransactionState, failure string, metrics *TransferMetrics, now time.Time) (*Transaction, error) {
	return applyAuthorityValue[Transaction](c, now, AuthorityTransactionAdvance, AuthorityTransactionPayload{ID: id, Revision: revision, Next: next, Failure: failure, Metrics: metrics})
}

type authorityTransactionCatalog struct{ *AuthorityCatalog }

var _ TransactionCatalog = authorityTransactionCatalog{}
var _ AtomicTargetPublisher = authorityTransactionCatalog{}

func (r authorityTransactionCatalog) Create(tx *Transaction) error { return r.CreateTransaction(tx) }
func (r authorityTransactionCatalog) Load(id string) (*Transaction, error) {
	return r.LoadTransaction(id)
}
func (r authorityTransactionCatalog) Advance(id string, revision uint64, next TransactionState, failure string, now time.Time) (*Transaction, error) {
	return r.AdvanceTransaction(id, revision, next, failure, nil, now)
}
func (r authorityTransactionCatalog) AdvanceIndependent(id string, revision uint64, metrics TransferMetrics, now time.Time) (*Transaction, error) {
	return r.AdvanceTransaction(id, revision, TransactionIndependent, "", &metrics, now)
}
func (r authorityTransactionCatalog) Publish(id string, revision uint64, desiredPaused, activated bool, now time.Time) (*Transaction, error) {
	return applyAuthorityValue[Transaction](r.AuthorityCatalog, now, AuthorityTransactionPublish, AuthorityTransactionPayload{ID: id, Revision: revision, DesiredPaused: desiredPaused, Activated: activated})
}
func (r authorityTransactionCatalog) PublishMigrationTarget(id string, revision uint64, desiredPaused bool, physicalContainer, physicalBranch, targetIncarnation string) (*Transaction, error) {
	return applyAuthorityValue[Transaction](r.AuthorityCatalog, time.Now().UTC(), AuthorityTransactionPublish, AuthorityTransactionPayload{
		ID: id, Revision: revision, DesiredPaused: desiredPaused,
		PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch, TargetIncarnation: targetIncarnation,
	})
}
func (r authorityTransactionCatalog) PublishTarget(id string, revision uint64, desiredPaused bool, family, branch, physicalContainer, physicalBranch, targetIncarnation string) (*Transaction, error) {
	result, err := applyAuthorityValue[ForkPublishResult](r.AuthorityCatalog, time.Now().UTC(), AuthorityForkPublish, AuthorityForkPublishPayload{
		TransactionID: id, TransactionRevision: revision, DesiredPaused: desiredPaused,
		Family: family, Branch: branch, PhysicalContainer: physicalContainer, PhysicalBranch: physicalBranch,
		TargetIncarnation: targetIncarnation,
	})
	if err != nil {
		return nil, err
	}
	return &result.Transaction, nil
}
func (r authorityTransactionCatalog) MarkActivated(id string, revision uint64, now time.Time) (*Transaction, error) {
	return applyAuthorityValue[Transaction](r.AuthorityCatalog, now, AuthorityTransactionActivate, AuthorityTransactionPayload{ID: id, Revision: revision})
}
func (r authorityTransactionCatalog) BindSession(id string, revision uint64, digest string, now time.Time) (*Transaction, error) {
	return applyAuthorityValue[Transaction](r.AuthorityCatalog, now, AuthorityTransactionSession, AuthorityTransactionPayload{ID: id, Revision: revision, Digest: digest})
}
func (r authorityTransactionCatalog) BindGraphHandoff(id string, revision uint64, graph GraphHandoffDescriptor, now time.Time) (*Transaction, error) {
	return applyAuthorityValue[Transaction](r.AuthorityCatalog, now, AuthorityTransactionHandoff, AuthorityTransactionPayload{ID: id, Revision: revision, Graph: &graph})
}
func (r authorityTransactionCatalog) Delete(id string, revision uint64) error {
	return applyAuthorityNoValue(r.AuthorityCatalog, time.Now().UTC(), AuthorityTransactionRetire, AuthorityTransactionPayload{ID: id, Revision: revision})
}
func (r authorityTransactionCatalog) List() ([]Transaction, error) {
	state, err := r.query(AuthorityQuery{Transactions: All[string]()})
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
func (c *AuthorityCatalog) Transactions() TransactionCatalog { return authorityTransactionCatalog{c} }
