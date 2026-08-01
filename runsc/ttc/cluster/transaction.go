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
	ImageDigest     string `json:"image_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	RepositoryID    string `json:"repository_id"`
}

// Validate rejects ambiguous identities before any target reservation.
func (i ForkIdentity) Validate() error {
	if !transactionIDPattern.MatchString(i.TransactionID) {
		return fmt.Errorf("invalid transaction ID %q", i.TransactionID)
	}
	if i.SourceNodeID == "" || i.TargetNodeID == "" || i.SourceNodeID == i.TargetNodeID {
		return errors.New("fork identity requires distinct source and target node IDs")
	}
	if i.SourceContainer == "" || i.SourceBranch == "" || i.TargetContainer == "" || i.TargetBranch == "" || i.RepositoryID == "" {
		return errors.New("fork identity omits a container, branch, or repository field")
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
	Identity     ForkIdentity           `json:"identity"`
	Negotiated   NegotiatedCapabilities `json:"negotiated"`
	State        TransactionState       `json:"state"`
	Revision     uint64                 `json:"revision"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
	Failure      string                 `json:"failure,omitempty"`
	SourceNeeded bool                   `json:"source_needed"`
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
