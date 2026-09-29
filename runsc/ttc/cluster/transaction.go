// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// TransactionState is a durable distributed-fork lifecycle state.
type TransactionState string

const (
	TransactionPreparing       TransactionState = "preparing"
	TransactionTargetReserved  TransactionState = "target-reserved"
	TransactionSourceCaptured  TransactionState = "source-captured"
	TransactionTargetRestoring TransactionState = "target-restoring"
	TransactionTargetPaused    TransactionState = "target-paused"
	TransactionPublished       TransactionState = "published"
	TransactionDraining        TransactionState = "draining"
	TransactionIndependent     TransactionState = "independent"
	TransactionCanceled        TransactionState = "canceled"
	TransactionFailed          TransactionState = "failed"
)

var transactionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ForkIdentity binds a transaction to immutable filesystem and image state.
type ForkIdentity struct {
	TransactionID   string `json:"transaction_id"`
	SourceNodeID    string `json:"source_node_id"`
	TargetNodeID    string `json:"target_node_id"`
	SourceContainer string `json:"source_container"`
	SourceBranch    string `json:"source_branch"`
	TargetContainer string `json:"target_container"`
	TargetBranch    string `json:"target_branch"`
	// TargetVisibleBranch is the explicit user-facing branch name on the
	// target. TargetBranch remains the transaction-unique filesystem/state
	// identity embedded in captured metadata.
	TargetVisibleBranch string `json:"target_visible_branch"`
	ImageDigest         string `json:"image_digest"`
	RootfsDigest        string `json:"rootfs_digest"`
	RepositoryID        string `json:"repository_id"`
	// Migration preserves TargetContainer@TargetVisibleBranch instead of
	// publishing an additional logical branch. The target runtime is still a
	// fork-time snapshot internally, but ownership replaces the paused source.
	Migration bool `json:"migration,omitempty"`
}

// VisibleTargetBranch returns the external target-family branch identity.
func (i ForkIdentity) VisibleTargetBranch() string {
	return i.TargetVisibleBranch
}

// TransferMetrics is the final token-free source-side accounting captured
// immediately before a distributed target becomes independent. It is durable
// so experiments remain reproducible after the source session is released.
type TransferMetrics struct {
	TimeToIndependent time.Duration `json:"time_to_independent"`
	PullRequests      uint64        `json:"pull_requests"`
	AckRequests       uint64        `json:"ack_requests"`
	AckBatches        uint64        `json:"ack_batches"`
	PullBytes         uint64        `json:"pull_bytes"`
	MetadataBytes     uint64        `json:"metadata_bytes"`
	PushBytes         uint64        `json:"push_bytes"`
	PushRetries       uint64        `json:"push_retries"`
	PagesTotal        uint64        `json:"pages_total"`
	PagesReleased     uint64        `json:"pages_released"`
	PushHints         uint64        `json:"push_hints"`
	PushHintsDropped  uint64        `json:"push_hints_dropped"`
}

// Validate ensures final accounting cannot claim independence while retaining
// source pages or contain impossible page totals.
func (m TransferMetrics) Validate() error {
	if m.PagesReleased > m.PagesTotal {
		return fmt.Errorf("released pages %d exceed total pages %d", m.PagesReleased, m.PagesTotal)
	}
	if m.PagesReleased != m.PagesTotal {
		return fmt.Errorf("independent metrics retain %d pages", m.PagesTotal-m.PagesReleased)
	}
	if m.TimeToIndependent < 0 {
		return errors.New("time to independence is negative")
	}
	return nil
}

// Validate rejects ambiguous identities before any target reservation.
func (i ForkIdentity) Validate() error {
	if !transactionIDPattern.MatchString(i.TransactionID) {
		return fmt.Errorf("invalid transaction ID %q", i.TransactionID)
	}
	if i.SourceNodeID == "" || i.TargetNodeID == "" || i.SourceNodeID == i.TargetNodeID {
		return errors.New("fork identity requires distinct source and target node IDs")
	}
	if i.SourceContainer == "" || i.SourceBranch == "" || i.TargetContainer == "" || i.TargetBranch == "" || i.TargetVisibleBranch == "" || i.RepositoryID == "" {
		return errors.New("fork identity omits a container, branch, or repository field")
	}
	if i.Migration && (i.TargetContainer != i.SourceContainer || i.VisibleTargetBranch() != i.SourceBranch) {
		return errors.New("migration identity must preserve its container and visible branch")
	}
	if err := validateSHA256Digest(i.ImageDigest); err != nil {
		return fmt.Errorf("image digest: %w", err)
	}
	if err := validateSHA256Digest(i.RootfsDigest); err != nil {
		return fmt.Errorf("rootfs digest: %w", err)
	}
	return nil
}

func validateSHA256Digest(digest string) error {
	const prefix = "sha256:"
	if len(digest) != len(prefix)+sha256.Size*2 || digest[:len(prefix)] != prefix {
		return fmt.Errorf("expected sha256 digest, got %q", digest)
	}
	if _, err := hex.DecodeString(digest[len(prefix):]); err != nil {
		return fmt.Errorf("invalid hexadecimal digest: %w", err)
	}
	return nil
}

// Transaction is the durable control-plane record. A target container is not
// externally visible until State reaches TransactionPublished.
type Transaction struct {
	Identity   ForkIdentity           `json:"identity"`
	Negotiated NegotiatedCapabilities `json:"negotiated"`
	State      TransactionState       `json:"state"`
	Revision   uint64                 `json:"revision"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	Failure    string                 `json:"failure,omitempty"`
	// SourceIncarnation is assigned by the replicated state machine when the
	// transaction is created. Reusing a human node ID cannot satisfy an old
	// target's volatile dependency.
	SourceIncarnation string `json:"source_incarnation,omitempty"`
	TargetIncarnation string `json:"target_incarnation,omitempty"`
	// SourcePhysical* binds the transaction's provider to the exact node-local
	// container selected when the transaction began. For migration this comes
	// from the Raft placement. For an ordinary fork the authenticated source
	// reports it, which is essential when an unplaced cold branch is served
	// from a temporary materialization. SourceBranch remains the stable logical
	// identity and may be reused while the physical provider is still fenced.
	SourcePhysicalContainer string           `json:"source_physical_container,omitempty"`
	SourcePhysicalBranch    string           `json:"source_physical_branch,omitempty"`
	SourceNeeded            bool             `json:"source_needed"`
	Metrics                 *TransferMetrics `json:"metrics,omitempty"`
	// SessionDigest binds retries to the same bearer-token-bearing data-plane
	// descriptor without persisting or exposing that descriptor itself.
	SessionDigest string `json:"session_digest,omitempty"`
	// GraphHandoff is durable and non-secret, allowing either daemon to retry
	// the live-to-shared reader transition after a control-plane restart.
	GraphHandoff *GraphHandoffDescriptor `json:"graph_handoff,omitempty"`
	// DesiredPaused is the source-visible state requested at publication. All
	// distributed targets are first published physically paused and become
	// runnable only after their placement grant commits; Activated records that
	// this final step completed. For a requested paused target, Activated is
	// immediately true.
	DesiredPaused bool `json:"desired_paused,omitempty"`
	Activated     bool `json:"activated,omitempty"`
	// TargetPhysical* is the target-reported runtime identity. It is recorded
	// while a migration target is still paused, before the atomic ownership
	// cutover. A target may reuse a Sentry that already hosts a sibling branch,
	// so the physical container is not necessarily TargetBranch or the
	// transaction ID.
	TargetPhysicalContainer   string `json:"target_physical_container,omitempty"`
	TargetPhysicalBranch      string `json:"target_physical_branch,omitempty"`
	TargetPhysicalIncarnation string `json:"target_physical_incarnation,omitempty"`
	// InheritedDependencies is the exact set of older volatile transactions
	// captured by a migration. It is frozen at ownership cutover, before this
	// transaction's own dependency is appended. A later migration may begin
	// before this one finishes draining; recording the set explicitly prevents
	// the older completion from mistaking that newer dependency for an ancestor
	// and releasing its source prematurely.
	InheritedDependencies []string `json:"inherited_dependencies,omitempty"`
	// OwnershipCommitted is set in the same Raft entry that moves a migrated
	// branch's placement. It separates a physically prepared target from the
	// sole authoritative target before activation.
	OwnershipCommitted bool `json:"ownership_committed,omitempty"`
}

var transactionTransitions = map[TransactionState]map[TransactionState]bool{
	TransactionPreparing:       {TransactionTargetReserved: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionTargetReserved:  {TransactionSourceCaptured: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionSourceCaptured:  {TransactionTargetRestoring: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionTargetRestoring: {TransactionTargetPaused: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionTargetPaused:    {TransactionPublished: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionPublished:       {TransactionDraining: true, TransactionIndependent: true, TransactionCanceled: true, TransactionFailed: true},
	TransactionDraining:        {TransactionIndependent: true, TransactionCanceled: true, TransactionFailed: true},
}

// NewTransaction constructs the first durable record.
func NewTransaction(identity ForkIdentity, negotiated NegotiatedCapabilities, now time.Time) (*Transaction, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if negotiated.Protocol == 0 || negotiated.BasePageSize == 0 || negotiated.MaxTransferSize < negotiated.BasePageSize {
		return nil, errors.New("invalid negotiated capabilities")
	}
	if negotiated.PagingPolicy.PageSize != negotiated.BasePageSize || negotiated.PagingPolicy.TransferBytes > negotiated.MaxTransferSize {
		return nil, errors.New("negotiated paging policy is outside capability bounds")
	}
	if err := negotiated.PagingPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("negotiated paging policy: %w", err)
	}
	if now.IsZero() {
		return nil, errors.New("transaction creation time is zero")
	}
	return &Transaction{
		Identity: identity, Negotiated: negotiated, State: TransactionPreparing,
		Revision: 1, CreatedAt: now, UpdatedAt: now, SourceNeeded: true,
	}, nil
}

// Advance performs one legal, idempotent transition. Failure text is required
// only for TransactionFailed and is rejected for all other states.
func (t *Transaction) Advance(next TransactionState, failure string, now time.Time) error {
	if t == nil {
		return errors.New("nil transaction")
	}
	if next == t.State {
		if next == TransactionFailed && failure != t.Failure {
			return errors.New("idempotent failed transition changed its diagnostic")
		}
		return nil
	}
	if !transactionTransitions[t.State][next] {
		return fmt.Errorf("illegal distributed-fork transition %s -> %s", t.State, next)
	}
	if next == TransactionFailed && failure == "" {
		return errors.New("failed transaction requires a diagnostic")
	}
	if next != TransactionFailed && failure != "" {
		return errors.New("failure diagnostic is valid only for failed state")
	}
	if now.Before(t.UpdatedAt) {
		return errors.New("transaction update time moved backward")
	}
	t.State = next
	t.Revision++
	t.UpdatedAt = now
	t.Failure = failure
	if next == TransactionIndependent || next == TransactionCanceled || next == TransactionFailed {
		t.SourceNeeded = false
	}
	return nil
}

// Terminal reports whether no further lifecycle transition is valid.
func (t *Transaction) Terminal() bool {
	return t != nil && (t.State == TransactionIndependent || t.State == TransactionCanceled || t.State == TransactionFailed)
}
