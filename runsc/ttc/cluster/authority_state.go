// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/ttcremote"
)

const (
	// AuthorityFormatVersion is the durable authority-state format. It is
	// deliberately independent from the peer data-plane protocol version.
	// Version 11 is the first kept in the authority log, which names terms
	// epochs; version 12 keeps the shared snapshot origins, not the dead ones.
	AuthorityFormatVersion = 12
	maxRememberedRequests  = 65536
	maxSharedGCLease       = 30 * time.Second
)

// AuthorityCommandKind identifies one deterministic state-machine mutation.
type AuthorityCommandKind string

const (
	AuthorityNodeRegister          AuthorityCommandKind = "node-register"
	AuthorityNodeRenew             AuthorityCommandKind = "node-renew"
	AuthorityNodeTransition        AuthorityCommandKind = "node-transition"
	AuthorityNodeExpire            AuthorityCommandKind = "node-expire"
	AuthorityNodeRemove            AuthorityCommandKind = "node-remove"
	AuthorityClusterPolicySet      AuthorityCommandKind = "cluster-policy-set"
	AuthorityCapacityReserve       AuthorityCommandKind = "capacity-reserve"
	AuthorityCapacityComplete      AuthorityCommandKind = "capacity-complete"
	AuthorityCapacityRelease       AuthorityCommandKind = "capacity-release"
	AuthorityPlacementCreate       AuthorityCommandKind = "placement-create"
	AuthorityPlacementBegin        AuthorityCommandKind = "placement-begin-migration"
	AuthorityPlacementAdopt        AuthorityCommandKind = "placement-adopt-incarnation"
	AuthorityPlacementRecover      AuthorityCommandKind = "placement-recover-source"
	AuthorityPlacementCommit       AuthorityCommandKind = "placement-commit-migration"
	AuthorityPlacementAbort        AuthorityCommandKind = "placement-abort-migration"
	AuthorityPlacementUnplace      AuthorityCommandKind = "placement-unplace"
	AuthorityPlacementClaim        AuthorityCommandKind = "placement-claim-unplaced"
	AuthorityPlacementStableState  AuthorityCommandKind = "placement-update-state"
	AuthorityPlacementQuarantine   AuthorityCommandKind = "placement-set-quarantine"
	AuthorityPlacementRename       AuthorityCommandKind = "placement-rename"
	AuthorityPlacementRemove       AuthorityCommandKind = "placement-remove"
	AuthorityTransactionCreate     AuthorityCommandKind = "transaction-create"
	AuthorityTransactionAdvance    AuthorityCommandKind = "transaction-advance"
	AuthorityTransactionPublish    AuthorityCommandKind = "transaction-publish"
	AuthorityTransactionActivate   AuthorityCommandKind = "transaction-activate"
	AuthorityTransactionSession    AuthorityCommandKind = "transaction-bind-session"
	AuthorityTransactionHandoff    AuthorityCommandKind = "transaction-bind-handoff"
	AuthorityTransactionRetire     AuthorityCommandKind = "transaction-retire"
	AuthorityMigrationCutover      AuthorityCommandKind = "migration-cutover"
	AuthorityForkPublish           AuthorityCommandKind = "fork-publish"
	AuthorityPublicationBegin      AuthorityCommandKind = "publication-begin"
	AuthorityPublicationReady      AuthorityCommandKind = "publication-capture-ready"
	AuthorityPublicationCommit     AuthorityCommandKind = "publication-commit-roots"
	AuthorityPublicationFail       AuthorityCommandKind = "publication-fail"
	AuthorityPublicationCancel     AuthorityCommandKind = "publication-cancel"
	AuthorityBranchCorrupt         AuthorityCommandKind = "branch-mark-corrupted"
	AuthorityBranchInherit         AuthorityCommandKind = "branch-inherit-dependencies"
	AuthorityBranchDiscardMemory   AuthorityCommandKind = "branch-discard-memory"
	AuthorityBranchRemoveCorrupted AuthorityCommandKind = "branch-remove-corrupted"
	AuthoritySnapshotAllocate      AuthorityCommandKind = "snapshot-allocate"
	AuthoritySnapshotAbandon       AuthorityCommandKind = "snapshot-abandon"
	AuthoritySnapshotPublish       AuthorityCommandKind = "snapshot-publish"
	AuthoritySharedGCAcquire       AuthorityCommandKind = "shared-gc-acquire"
	AuthoritySharedGCRenew         AuthorityCommandKind = "shared-gc-renew"
	AuthoritySharedGCRelease       AuthorityCommandKind = "shared-gc-release"
	AuthoritySharedGCRetire        AuthorityCommandKind = "shared-gc-retire-origin"
	AuthoritySharedGCForget        AuthorityCommandKind = "shared-gc-forget-retirements"
)

// AuthorityCommand is the sole mutation envelope the authority logs. IssuedAt is
// assigned by the authority's holder before proposal; applying it never
// consults a clock, so replaying the log gives the same state.
type AuthorityCommand struct {
	Version   int                  `json:"version"`
	RequestID string               `json:"request_id"`
	IssuedAt  time.Time            `json:"issued_at"`
	Epoch     uint64               `json:"epoch"`
	Kind      AuthorityCommandKind `json:"kind"`
	Payload   json.RawMessage      `json:"payload"`
}

// NewAuthorityCommand builds a bounded deterministic command.
func NewAuthorityCommand(requestID string, at time.Time, kind AuthorityCommandKind, payload any) (AuthorityCommand, error) {
	if !transactionIDPattern.MatchString(requestID) {
		return AuthorityCommand{}, fmt.Errorf("invalid authority request ID %q", requestID)
	}
	if at.IsZero() {
		return AuthorityCommand{}, errors.New("authority command time is zero")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return AuthorityCommand{}, err
	}
	if len(data) > maxTransactionRecordBytes {
		return AuthorityCommand{}, fmt.Errorf("authority command payload is %d bytes, limit is %d", len(data), maxTransactionRecordBytes)
	}
	return AuthorityCommand{Version: AuthorityFormatVersion, RequestID: requestID, IssuedAt: at.UTC(), Epoch: 1, Kind: kind, Payload: data}, nil
}

// AuthorityResult is the stable result of an applied request. State-dependent
// rejections are logged too, so replaying the same request cannot change a
// failure into success after unrelated state changes.
type AuthorityResult struct {
	Revision uint64          `json:"revision"`
	Value    json.RawMessage `json:"value,omitempty"`
	Error    string          `json:"error,omitempty"`
}

func (r AuthorityResult) Err() error {
	if r.Error == "" {
		return nil
	}
	return errors.New(r.Error)
}

type appliedAuthorityRequest struct {
	Digest string          `json:"digest"`
	Result AuthorityResult `json:"result"`
}

// DurableRoots are immutable bulk-data roots. The authority stores these small names,
// never the pages, file extents, or packs reached through them.
type DurableRoots struct {
	MemoryRoot      string `json:"memory_root,omitempty"`
	MemorySnapshot  uint64 `json:"memory_snapshot,omitempty"`
	FilesystemRoot  string `json:"filesystem_root,omitempty"`
	ObjectSetDigest string `json:"object_set_digest,omitempty"`
}

// SnapshotOriginProvider is the sole routing entry for an origin that is not
// yet fully authoritative in shared storage. Absence means readers resolve
// through the committed shared manifest.
type SnapshotOriginProvider struct {
	SnapshotID uint64 `json:"snapshot_id"`
	NodeID     string `json:"node_id"`
}

// SnapshotAllocation is the stable result of one allocation request.
type SnapshotAllocation struct {
	First uint64 `json:"first"`
	Count uint32 `json:"count"`
}

// SharedGCLease gives exactly one live daemon authority to mutate shared
// snapshot packs and the reference ledger rebuilt from them. Expiry stops
// collection; it never transfers object authority or permits another daemon
// to continue a previous holder's local collection plan.
type SharedGCLease struct {
	NodeID      string    `json:"node_id"`
	Incarnation string    `json:"incarnation"`
	Generation  uint64    `json:"generation"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// SharedPackRetirement permanently fences an unreachable origin against a
// late publisher. Physical deletion may be retried by later GC coordinators.
type SharedPackRetirement struct {
	SnapshotID   uint64    `json:"snapshot_id"`
	GCGeneration uint64    `json:"gc_generation"`
	RetiredAt    time.Time `json:"retired_at"`
}

// BranchAuthority is the cluster-wide authority record that is not already
// represented by PlacementRecord.
type BranchAuthority struct {
	FamilyID         string       `json:"family_id"`
	BranchID         string       `json:"branch_id"`
	Generation       uint64       `json:"generation"`
	PublicationID    string       `json:"publication_id,omitempty"`
	Lifecycle        string       `json:"lifecycle"`
	Roots            DurableRoots `json:"roots,omitempty"`
	Corrupted        bool         `json:"corrupted,omitempty"`
	CorruptionReason string       `json:"corruption_reason,omitempty"`
	// VolatileDependencies are coarse authority edges, not page records. The
	// immutable transaction describes the detailed memory/filesystem snapshot.
	// An edge disappears only when that transaction commits independence.
	VolatileDependencies  []VolatileDependency `json:"volatile_dependencies,omitempty"`
	DependenciesInherited bool                 `json:"dependencies_inherited,omitempty"`
	UpdatedAt             time.Time            `json:"updated_at"`
}

// VolatileDependency identifies state whose only guaranteed source is one
// exact daemon incarnation. Byte-level accounting remains in the data plane.
type VolatileDependency struct {
	TransactionID     string `json:"transaction_id"`
	SourceNode        string `json:"source_node"`
	SourceIncarnation string `json:"source_incarnation"`
}

// PublicationState is the lifecycle of one authorized immutable-root publish.
type PublicationState string

const (
	PublicationPreparing PublicationState = "preparing"
	PublicationReady     PublicationState = "capture-ready"
	PublicationCommitted PublicationState = "committed"
	PublicationFailed    PublicationState = "failed"
	PublicationCanceled  PublicationState = "canceled"
)

// PublicationRecord fences asynchronous upload completion by the exact branch
// incarnation and immutable publication identity that authorized it.
type PublicationRecord struct {
	FamilyID          string           `json:"family_id"`
	BranchID          string           `json:"branch_id"`
	BranchGeneration  uint64           `json:"branch_generation"`
	PublicationID     string           `json:"publication_id"`
	Lifecycle         string           `json:"lifecycle"`
	PreviousLifecycle string           `json:"previous_lifecycle,omitempty"`
	State             PublicationState `json:"state"`
	CandidateRoots    DurableRoots     `json:"candidate_roots,omitempty"`
	Failure           string           `json:"failure,omitempty"`
	Revision          uint64           `json:"revision"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// AuthorityState is snapshotted by the authority. Maps are keyed by stable logical
// identities, never local paths.
type AuthorityState struct {
	Version                int    `json:"version"`
	Revision               uint64 `json:"revision"`
	NextSnapshotID         uint64 `json:"next_snapshot_id"`
	NextSharedGCGeneration uint64 `json:"next_shared_gc_generation"`
	// HolderObservedAt is populated only on a linearizable read response. It is
	// zero in the applied state and snapshots, and lets remote readers convert
	// lease deadlines without assuming wall-clock synchronization.
	HolderObservedAt time.Time                    `json:"holder_observed_at,omitempty"`
	ClusterPolicy    ClusterRuntimePolicy         `json:"cluster_policy,omitempty"`
	Nodes            map[string]NodeRecord        `json:"nodes,omitempty"`
	RetiredNodes     map[string]string            `json:"retired_nodes,omitempty"`
	Placements       map[string]PlacementRecord   `json:"placements,omitempty"`
	Transactions     map[string]Transaction       `json:"transactions,omitempty"`
	Branches         map[string]BranchAuthority   `json:"branches,omitempty"`
	Publications     map[string]PublicationRecord `json:"publications,omitempty"`
	// A snapshot origin the authority allocated is volatile, its
	// provider's, until it is published to the shared root, and shared then
	// until shared GC retires it; or dead, once abandoned or retired. Only the
	// volatile and shared ones are kept: see OriginDead.
	VolatileOrigins map[uint64]SnapshotOriginProvider `json:"volatile_origins,omitempty"`
	SharedOrigins   map[uint64]bool                   `json:"shared_origins,omitempty"`
	SharedGC        SharedGCLease                     `json:"shared_gc,omitempty"`
	// RetiredOrigins has the retirements whose deletion shared GC has yet to
	// complete, which it then forgets.
	RetiredOrigins map[uint64]SharedPackRetirement    `json:"retired_origins,omitempty"`
	Requests       map[string]appliedAuthorityRequest `json:"requests,omitempty"`
	RequestOrder   []string                           `json:"request_order,omitempty"`
}

// NewAuthorityState creates an empty cluster state.
func NewAuthorityState() *AuthorityState {
	state := &AuthorityState{Version: AuthorityFormatVersion, NextSnapshotID: 1, NextSharedGCGeneration: 1}
	state.initMaps()
	return state
}

// OriginDead reports whether snapshot origin id, which the authority
// allocated, can never be read again: its provider abandoned it, or shared
// GC retired it. A view answers only for the origins its query named among
// both the volatile and the shared ones; see OriginsQuery.
func (s *AuthorityState) OriginDead(id uint64) bool {
	if id == 0 || id >= s.NextSnapshotID {
		return false
	}
	if _, volatile := s.VolatileOrigins[id]; volatile {
		return false
	}
	return !s.SharedOrigins[id]
}

func (s *AuthorityState) initMaps() {
	if s.Nodes == nil {
		s.Nodes = make(map[string]NodeRecord)
	}
	if s.RetiredNodes == nil {
		s.RetiredNodes = make(map[string]string)
	}
	if s.Placements == nil {
		s.Placements = make(map[string]PlacementRecord)
	}
	if s.Transactions == nil {
		s.Transactions = make(map[string]Transaction)
	}
	if s.Branches == nil {
		s.Branches = make(map[string]BranchAuthority)
	}
	if s.Publications == nil {
		s.Publications = make(map[string]PublicationRecord)
	}
	if s.VolatileOrigins == nil {
		s.VolatileOrigins = make(map[uint64]SnapshotOriginProvider)
	}
	if s.SharedOrigins == nil {
		s.SharedOrigins = make(map[uint64]bool)
	}
	if s.RetiredOrigins == nil {
		s.RetiredOrigins = make(map[uint64]SharedPackRetirement)
	}
	if s.NextSnapshotID == 0 {
		s.NextSnapshotID = 1
	}
	if s.NextSharedGCGeneration == 0 {
		s.NextSharedGCGeneration = 1
	}
	if s.Requests == nil {
		s.Requests = make(map[string]appliedAuthorityRequest)
	}
}

func logicalBranchKey(family, branch string) string { return family + "\x00" + branch }
func publicationKey(family, branch, publicationID string) string {
	return family + "\x00" + branch + "\x00" + publicationID
}

func appendVolatileDependency(dependencies []VolatileDependency, dependency VolatileDependency) []VolatileDependency {
	for _, existing := range dependencies {
		if existing.TransactionID == dependency.TransactionID {
			return dependencies
		}
	}
	return append(append([]VolatileDependency(nil), dependencies...), dependency)
}

func removeVolatileDependency(dependencies []VolatileDependency, transactionID string) []VolatileDependency {
	result := make([]VolatileDependency, 0, len(dependencies))
	for _, dependency := range dependencies {
		if dependency.TransactionID != transactionID {
			result = append(result, dependency)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func volatileDependencyReferenced(branches map[string]BranchAuthority, transactionID string) bool {
	for _, branch := range branches {
		for _, dependency := range branch.VolatileDependencies {
			if dependency.TransactionID == transactionID {
				return true
			}
		}
	}
	return false
}

// volatileDependencyForwarded reports the narrow interval in which a branch
// has moved off formerNode through a newer migration, but that migration has
// not yet committed independence and atomically retired its inherited
// dependencies. The former target runtime may already be absent in this
// interval; the predecessor source must wait for the successor fence rather
// than treating that absence as corruption.
func volatileDependencyForwarded(state *AuthorityState, family, branch, transactionID, formerNode string) bool {
	if state == nil || family == "" || branch == "" || transactionID == "" || formerNode == "" {
		return false
	}
	authority, ok := state.Branches[logicalBranchKey(family, branch)]
	if !ok {
		return false
	}
	predecessor, successor := false, false
	for _, dependency := range authority.VolatileDependencies {
		if dependency.TransactionID == transactionID {
			predecessor = true
			continue
		}
		if dependency.SourceNode != formerNode {
			continue
		}
		tx, ok := state.Transactions[dependency.TransactionID]
		if ok && tx.Identity.Migration && tx.OwnershipCommitted &&
			tx.Identity.SourceContainer == family && tx.Identity.SourceBranch == branch &&
			(tx.State == TransactionPublished || tx.State == TransactionDraining) {
			successor = true
		}
	}
	placement, ok := state.Placements[logicalBranchKey(family, branch)]
	return predecessor && successor && ok && placement.OwnerNode != formerNode
}

func authorityCommandDigest(command AuthorityCommand) (string, error) {
	copy := command
	// Request identity binds semantic input, not the holder-generated envelope.
	// A proposal whose committed response was lost may be retried after time has
	// advanced or the authority changed hands; both retries must resolve to the original
	// result without being mistaken for request-ID reuse with a new operation.
	copy.RequestID = ""
	copy.IssuedAt = time.Time{}
	copy.Epoch = 0
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Apply applies exactly one committed authority command. It is deterministic: it
// performs no I/O, randomness, or wall-clock reads.
func (s *AuthorityState) Apply(command AuthorityCommand) AuthorityResult {
	if s == nil {
		return AuthorityResult{Error: "nil authority state"}
	}
	s.initMaps()
	digest, err := authorityCommandDigest(command)
	if err != nil {
		return AuthorityResult{Revision: s.Revision, Error: err.Error()}
	}
	if previous, ok := s.Requests[command.RequestID]; ok {
		if previous.Digest != digest {
			return AuthorityResult{Revision: s.Revision, Error: fmt.Sprintf("authority request %q was reused with different input", command.RequestID)}
		}
		return previous.Result
	}
	if command.Version != AuthorityFormatVersion || !transactionIDPattern.MatchString(command.RequestID) || command.IssuedAt.IsZero() || command.Epoch == 0 {
		return AuthorityResult{Revision: s.Revision, Error: "invalid authority command envelope"}
	}

	// Clone only the maps this command may change. The authority serializes Apply calls,
	// map values are immutable structs, and command handlers copy nested mutable
	// fields before editing them. This preserves rollback-on-rejection without
	// serializing the entire cluster state for every heartbeat.
	next := *s
	next.cloneMutationMaps(command.Kind)
	value, applyErr := next.apply(command)
	next.Revision = s.Revision + 1
	result := AuthorityResult{Revision: next.Revision, Value: value}
	if applyErr != nil {
		// Rejections are ordered and remembered but otherwise leave state intact.
		next = *s
		next.initMaps()
		next.Revision = s.Revision + 1
		result = AuthorityResult{Revision: next.Revision, Error: applyErr.Error()}
	}
	next.remember(command.RequestID, digest, result)
	*s = next
	return result
}

func cloneAuthorityMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (s *AuthorityState) cloneMutationMaps(kind AuthorityCommandKind) {
	switch kind {
	case AuthorityNodeRenew, AuthorityCapacityReserve, AuthorityCapacityComplete, AuthorityCapacityRelease:
		s.Nodes = cloneAuthorityMap(s.Nodes)
	case AuthorityNodeRegister, AuthorityNodeTransition:
		s.Nodes = cloneAuthorityMap(s.Nodes)
		if kind == AuthorityNodeRegister {
			s.RetiredNodes = cloneAuthorityMap(s.RetiredNodes)
			// Registering another incarnation abandons the origins the
			// previous one provided.
			s.VolatileOrigins = cloneAuthorityMap(s.VolatileOrigins)
		}
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Branches = cloneAuthorityMap(s.Branches)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Publications = cloneAuthorityMap(s.Publications)
	case AuthorityNodeExpire:
		s.Nodes = cloneAuthorityMap(s.Nodes)
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Branches = cloneAuthorityMap(s.Branches)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Publications = cloneAuthorityMap(s.Publications)
		s.VolatileOrigins = cloneAuthorityMap(s.VolatileOrigins)
	case AuthorityNodeRemove:
		s.Nodes = cloneAuthorityMap(s.Nodes)
		s.RetiredNodes = cloneAuthorityMap(s.RetiredNodes)
		// Removal gives up what the incarnation held before it checks that
		// nothing remains, and may then be refused.
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Branches = cloneAuthorityMap(s.Branches)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Publications = cloneAuthorityMap(s.Publications)
		s.VolatileOrigins = cloneAuthorityMap(s.VolatileOrigins)
	case AuthorityClusterPolicySet:
		// ClusterPolicy is copied by value with AuthorityState.
	case AuthorityPlacementCreate, AuthorityPlacementBegin, AuthorityPlacementAdopt, AuthorityPlacementRecover,
		AuthorityPlacementCommit, AuthorityPlacementAbort, AuthorityPlacementUnplace, AuthorityPlacementClaim,
		AuthorityPlacementStableState, AuthorityPlacementQuarantine, AuthorityPlacementRename, AuthorityPlacementRemove:
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Branches = cloneAuthorityMap(s.Branches)
		if kind == AuthorityPlacementRename {
			s.Publications = cloneAuthorityMap(s.Publications)
		}
	case AuthorityTransactionCreate, AuthorityTransactionPublish,
		AuthorityTransactionActivate, AuthorityTransactionSession, AuthorityTransactionHandoff,
		AuthorityTransactionRetire:
		s.Transactions = cloneAuthorityMap(s.Transactions)
	case AuthorityTransactionAdvance:
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Branches = cloneAuthorityMap(s.Branches)
	case AuthorityMigrationCutover:
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Branches = cloneAuthorityMap(s.Branches)
	case AuthorityForkPublish:
		s.Transactions = cloneAuthorityMap(s.Transactions)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Branches = cloneAuthorityMap(s.Branches)
	case AuthorityPublicationBegin, AuthorityPublicationReady, AuthorityPublicationCommit,
		AuthorityPublicationFail, AuthorityPublicationCancel:
		s.Publications = cloneAuthorityMap(s.Publications)
		s.Branches = cloneAuthorityMap(s.Branches)
		if kind == AuthorityPublicationCommit {
			s.Placements = cloneAuthorityMap(s.Placements)
			// A commit publishes the origins its snapshot names.
			s.VolatileOrigins = cloneAuthorityMap(s.VolatileOrigins)
			s.SharedOrigins = cloneAuthorityMap(s.SharedOrigins)
		}
	case AuthorityBranchCorrupt, AuthorityBranchInherit, AuthorityBranchDiscardMemory, AuthorityBranchRemoveCorrupted:
		s.Branches = cloneAuthorityMap(s.Branches)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Publications = cloneAuthorityMap(s.Publications)
	case AuthoritySnapshotAllocate, AuthoritySnapshotAbandon, AuthoritySnapshotPublish:
		s.VolatileOrigins = cloneAuthorityMap(s.VolatileOrigins)
		if kind == AuthoritySnapshotPublish {
			s.SharedOrigins = cloneAuthorityMap(s.SharedOrigins)
		}
	case AuthoritySharedGCAcquire, AuthoritySharedGCRenew, AuthoritySharedGCRelease:
		// SharedGC is copied by value with AuthorityState.
	case AuthoritySharedGCRetire:
		s.SharedOrigins = cloneAuthorityMap(s.SharedOrigins)
		s.RetiredOrigins = cloneAuthorityMap(s.RetiredOrigins)
	case AuthoritySharedGCForget:
		s.RetiredOrigins = cloneAuthorityMap(s.RetiredOrigins)
	}
}

func (s *AuthorityState) remember(id, digest string, result AuthorityResult) {
	s.Requests[id] = appliedAuthorityRequest{Digest: digest, Result: result}
	s.RequestOrder = append(s.RequestOrder, id)
	if len(s.RequestOrder) > maxRememberedRequests {
		delete(s.Requests, s.RequestOrder[0])
		// Reslicing keeps a command's cost constant: append moves the window
		// to a new array once it reaches the end of its old one. A state a
		// command replaced keeps its own window of the shared array.
		s.RequestOrder = s.RequestOrder[1:]
	}
}

func decodeAuthorityPayload[T any](command AuthorityCommand) (T, error) {
	var payload T
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return payload, errors.New("authority payload has trailing data")
		}
		return payload, fmt.Errorf("decoding trailing authority payload: %w", err)
	}
	return payload, nil
}

func authorityValue(value any) (json.RawMessage, error) { return json.Marshal(value) }

// AuthorityNodeRegisterPayload carries a node registration command.
type AuthorityNodeRegisterPayload struct {
	Record        NodeRecord    `json:"record"`
	LeaseUntil    time.Time     `json:"lease_until,omitempty"`
	LeaseDuration time.Duration `json:"lease_duration,omitempty"`
}
type AuthorityNodeRenewPayload struct {
	NodeID, Incarnation string
	MemoryCurrentBytes  uint64
	LeaseUntil          time.Time
	LeaseDuration       time.Duration
}
type AuthorityNodeTransitionPayload struct {
	NodeID, Incarnation string
	State               NodeState
	LeaseUntil          time.Time
	LeaseDuration       time.Duration
}
type AuthorityNodeExpirePayload struct {
	NodeID, Incarnation string
	Revision            uint64
}

type AuthorityNodeRemovePayload struct {
	NodeID, Incarnation string
	Revision            uint64
}

// ClusterRuntimePolicy is the small operational policy the authority keeps.
// It may change only while no daemon has a live execution lease. CPU state is
// intentionally absent: the CPU profile is an immutable snapshot ABI.
type ClusterRuntimePolicy struct {
	Version         int              `json:"version"`
	Revision        uint64           `json:"revision"`
	PagingPolicy    ttcremote.Policy `json:"paging_policy"`
	LeaseDuration   time.Duration    `json:"lease_duration_ns"`
	MaxTransactions int              `json:"max_transactions"`
	AutoRebalance   bool             `json:"auto_rebalance"`
}

func (p ClusterRuntimePolicy) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported cluster runtime policy version %d", p.Version)
	}
	if p.LeaseDuration <= 0 {
		return errors.New("cluster lease duration must be positive")
	}
	if p.MaxTransactions <= 0 {
		return errors.New("cluster transaction limit must be positive")
	}
	return p.PagingPolicy.Validate()
}

type AuthorityClusterPolicyPayload struct {
	ExpectedRevision uint64               `json:"expected_revision"`
	Policy           ClusterRuntimePolicy `json:"policy"`
}
type AuthorityCapacityPayload struct {
	NodeID, Incarnation, OperationID, SourceNode string
	Bytes                                        uint64
	ExpiresAt                                    time.Time
	TTL                                          time.Duration
}

// AuthorityPlacementPayload carries a placement command. Unused fields must be empty and are ignored only
// by command kinds for which they have no meaning.
type AuthorityPlacementPayload struct {
	Record                               PlacementRecord `json:"record,omitempty"`
	Family, Branch, NewBranch            string
	Owner, Incarnation                   string
	OldIncarnation, NewIncarnation       string
	Generation                           uint64
	Operation, Target, TargetIncarnation string
	PhysicalContainer, PhysicalBranch    string
	State                                PlacementState
	Quarantined                          bool
}

// AuthorityTransactionPayload carries a distributed transaction command.
type AuthorityTransactionPayload struct {
	Transaction                       Transaction             `json:"transaction,omitempty"`
	ID                                string                  `json:"id,omitempty"`
	Revision                          uint64                  `json:"revision,omitempty"`
	Next                              TransactionState        `json:"next,omitempty"`
	Failure                           string                  `json:"failure,omitempty"`
	Digest                            string                  `json:"digest,omitempty"`
	Metrics                           *TransferMetrics        `json:"metrics,omitempty"`
	Graph                             *GraphHandoffDescriptor `json:"graph,omitempty"`
	DesiredPaused                     bool                    `json:"desired_paused,omitempty"`
	Activated                         bool                    `json:"activated,omitempty"`
	PhysicalContainer, PhysicalBranch string
	TargetIncarnation                 string
}

// AuthorityPublicationPayload carries a durable publication command.
type AuthorityPublicationPayload struct {
	Family, Branch, PublicationID     string
	Lifecycle                         string
	BranchGeneration, Revision        uint64
	Roots                             DurableRoots
	ProviderNode, ProviderIncarnation string
	SnapshotOrigins                   []uint64
	Failure                           string
}

type AuthorityBranchInheritPayload struct {
	Family, Branch, SourceBranch string
	BranchGeneration             uint64
}

type AuthoritySnapshotPayload struct {
	NodeID, Incarnation string
	Count               uint32
	SnapshotID          uint64
}

type AuthoritySharedGCPayload struct {
	NodeID, Incarnation string
	Generation          uint64
	Lease               time.Duration
	SnapshotID          uint64
	// SnapshotIDs names the retirements a forget forgets.
	SnapshotIDs []uint64 `json:",omitempty"`
}

// AuthorityMigrationCutoverPayload joins the transaction and placement CASes
// that used to be committed separately. The target runtime is already paused
// and locally published when this command is proposed; applying it makes that
// target the sole authoritative owner in one revision.
type AuthorityMigrationCutoverPayload struct {
	TransactionID, Family, Branch        string
	TransactionRevision                  uint64
	Operation, Target, TargetIncarnation string
	PhysicalContainer, PhysicalBranch    string
}

// MigrationCutoverResult is returned by AuthorityMigrationCutover.
type MigrationCutoverResult struct {
	Transaction Transaction     `json:"transaction"`
	Placement   PlacementRecord `json:"placement"`
}

// AuthorityForkPublishPayload atomically makes a restored remote-fork target
// visible and assigns its first placement generation.
type AuthorityForkPublishPayload struct {
	TransactionID, TargetIncarnation  string
	Family, Branch, PhysicalContainer string
	PhysicalBranch                    string
	TransactionRevision               uint64
	DesiredPaused                     bool
}

// ForkPublishResult is returned by AuthorityForkPublish.
type ForkPublishResult struct {
	Transaction Transaction     `json:"transaction"`
	Placement   PlacementRecord `json:"placement"`
}

func (s *AuthorityState) apply(command AuthorityCommand) (json.RawMessage, error) {
	switch command.Kind {
	case AuthorityNodeRegister, AuthorityNodeRenew, AuthorityNodeTransition, AuthorityNodeExpire, AuthorityNodeRemove,
		AuthorityCapacityReserve, AuthorityCapacityComplete, AuthorityCapacityRelease:
		return s.applyNode(command)
	case AuthorityClusterPolicySet:
		return s.applyClusterPolicy(command)
	case AuthorityPlacementCreate, AuthorityPlacementBegin, AuthorityPlacementAdopt, AuthorityPlacementRecover, AuthorityPlacementCommit,
		AuthorityPlacementAbort, AuthorityPlacementUnplace, AuthorityPlacementClaim,
		AuthorityPlacementStableState, AuthorityPlacementQuarantine, AuthorityPlacementRename, AuthorityPlacementRemove:
		return s.applyPlacement(command)
	case AuthorityTransactionCreate, AuthorityTransactionAdvance, AuthorityTransactionPublish,
		AuthorityTransactionActivate, AuthorityTransactionSession, AuthorityTransactionHandoff,
		AuthorityTransactionRetire:
		return s.applyTransaction(command)
	case AuthorityMigrationCutover:
		return s.applyMigrationCutover(command)
	case AuthorityForkPublish:
		return s.applyForkPublish(command)
	case AuthorityPublicationBegin, AuthorityPublicationReady, AuthorityPublicationCommit,
		AuthorityPublicationFail, AuthorityPublicationCancel:
		return s.applyPublication(command)
	case AuthorityBranchCorrupt, AuthorityBranchInherit, AuthorityBranchDiscardMemory, AuthorityBranchRemoveCorrupted:
		return s.applyBranch(command)
	case AuthoritySnapshotAllocate, AuthoritySnapshotAbandon, AuthoritySnapshotPublish:
		return s.applySnapshot(command)
	case AuthoritySharedGCAcquire, AuthoritySharedGCRenew, AuthoritySharedGCRelease, AuthoritySharedGCRetire, AuthoritySharedGCForget:
		return s.applySharedGC(command)
	default:
		return nil, fmt.Errorf("unknown authority command %q", command.Kind)
	}
}

func (s *AuthorityState) applyClusterPolicy(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityClusterPolicyPayload](command)
	if err != nil {
		return nil, err
	}
	if err := p.Policy.Validate(); err != nil {
		return nil, err
	}
	if s.ClusterPolicy.Revision != p.ExpectedRevision {
		return nil, errors.New("stale cluster runtime policy revision")
	}
	for _, node := range s.Nodes {
		if node.State != NodeOffline && node.LeaseActive(command.IssuedAt) {
			return nil, fmt.Errorf("cluster runtime policy cannot change while node %q has a live execution lease", node.NodeID)
		}
	}
	p.Policy.Revision = s.ClusterPolicy.Revision + 1
	s.ClusterPolicy = p.Policy
	return authorityValue(p.Policy)
}

func (s *AuthorityState) applySharedGC(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthoritySharedGCPayload](command)
	if err != nil {
		return nil, err
	}
	if err := s.requireNodeIncarnation(p.NodeID, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
		return nil, err
	}
	switch command.Kind {
	case AuthoritySharedGCAcquire:
		if p.Generation != 0 || p.SnapshotID != 0 || p.Lease <= 0 || p.Lease > maxSharedGCLease {
			return nil, errors.New("invalid shared GC lease acquisition")
		}
		if !s.SharedGC.ExpiresAt.IsZero() && command.IssuedAt.Before(s.SharedGC.ExpiresAt) &&
			(s.SharedGC.NodeID != p.NodeID || s.SharedGC.Incarnation != p.Incarnation) {
			return nil, errors.New("shared GC lease is owned by another daemon")
		}
		generation := s.NextSharedGCGeneration
		if generation == 0 || generation == math.MaxUint64 {
			return nil, errors.New("shared GC lease generation exhausted")
		}
		s.NextSharedGCGeneration++
		s.SharedGC = SharedGCLease{
			NodeID: p.NodeID, Incarnation: p.Incarnation, Generation: generation,
			ExpiresAt: command.IssuedAt.Add(p.Lease),
		}
		return authorityValue(s.SharedGC)
	case AuthoritySharedGCRenew:
		if p.Generation == 0 || p.SnapshotID != 0 || p.Lease <= 0 || p.Lease > maxSharedGCLease ||
			s.SharedGC.NodeID != p.NodeID || s.SharedGC.Incarnation != p.Incarnation || s.SharedGC.Generation != p.Generation ||
			!command.IssuedAt.Before(s.SharedGC.ExpiresAt) {
			return nil, errors.New("shared GC lease is not renewable")
		}
		s.SharedGC.ExpiresAt = command.IssuedAt.Add(p.Lease)
		return authorityValue(s.SharedGC)
	case AuthoritySharedGCRelease:
		if p.Lease != 0 || p.SnapshotID != 0 || p.Generation == 0 {
			return nil, errors.New("invalid shared GC lease release")
		}
		if s.SharedGC.NodeID == "" {
			return authorityValue(SharedGCLease{})
		}
		if s.SharedGC.NodeID != p.NodeID || s.SharedGC.Incarnation != p.Incarnation || s.SharedGC.Generation != p.Generation {
			return nil, errors.New("shared GC lease release is stale")
		}
		s.SharedGC = SharedGCLease{}
		return authorityValue(s.SharedGC)
	case AuthoritySharedGCRetire:
		if p.Lease != 0 || p.Generation == 0 || p.SnapshotID == 0 || p.SnapshotID >= s.NextSnapshotID ||
			s.SharedGC.NodeID != p.NodeID || s.SharedGC.Incarnation != p.Incarnation || s.SharedGC.Generation != p.Generation ||
			!command.IssuedAt.Before(s.SharedGC.ExpiresAt) {
			return nil, errors.New("shared GC retirement lacks a current lease")
		}
		if _, volatile := s.VolatileOrigins[p.SnapshotID]; volatile {
			return nil, errors.New("cannot retire a volatile snapshot origin")
		}
		if retirement, retired := s.RetiredOrigins[p.SnapshotID]; retired {
			return authorityValue(retirement)
		}
		if !s.SharedOrigins[p.SnapshotID] {
			return nil, errors.New("cannot retire a dead snapshot origin")
		}
		for _, branch := range s.Branches {
			if !branch.Corrupted && branch.Roots.MemorySnapshot == p.SnapshotID {
				return nil, errors.New("cannot retire a snapshot referenced by a branch root")
			}
		}
		for _, publication := range s.Publications {
			if (publication.State == PublicationPreparing || publication.State == PublicationReady) && publication.CandidateRoots.MemorySnapshot == p.SnapshotID {
				return nil, errors.New("cannot retire a snapshot referenced by an in-flight publication")
			}
		}
		retirement := SharedPackRetirement{SnapshotID: p.SnapshotID, GCGeneration: p.Generation, RetiredAt: command.IssuedAt}
		delete(s.SharedOrigins, p.SnapshotID)
		s.RetiredOrigins[p.SnapshotID] = retirement
		return authorityValue(retirement)
	case AuthoritySharedGCForget:
		if p.Lease != 0 || p.Generation == 0 || p.SnapshotID != 0 || len(p.SnapshotIDs) == 0 ||
			s.SharedGC.NodeID != p.NodeID || s.SharedGC.Incarnation != p.Incarnation || s.SharedGC.Generation != p.Generation ||
			!command.IssuedAt.Before(s.SharedGC.ExpiresAt) {
			return nil, errors.New("forgetting shared GC retirements lacks a current lease")
		}
		// What is forgotten is dead: neither volatile nor shared.
		for _, id := range p.SnapshotIDs {
			delete(s.RetiredOrigins, id)
		}
		return authorityValue(len(p.SnapshotIDs))
	default:
		panic("unreachable")
	}
}

func (s *AuthorityState) applySnapshot(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthoritySnapshotPayload](command)
	if err != nil {
		return nil, err
	}
	if err := s.requireNodeIncarnation(p.NodeID, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
		return nil, err
	}
	switch command.Kind {
	case AuthoritySnapshotAllocate:
		if p.Count == 0 || p.Count > 4096 {
			return nil, errors.New("snapshot allocation count is outside 1..4096")
		}
		if s.NextSnapshotID == 0 || uint64(p.Count) > math.MaxUint64-s.NextSnapshotID {
			return nil, errors.New("cluster snapshot ID space exhausted")
		}
		allocation := SnapshotAllocation{First: s.NextSnapshotID, Count: p.Count}
		for offset := uint64(0); offset < uint64(p.Count); offset++ {
			id := s.NextSnapshotID + offset
			s.VolatileOrigins[id] = SnapshotOriginProvider{SnapshotID: id, NodeID: p.NodeID}
		}
		s.NextSnapshotID += uint64(p.Count)
		return authorityValue(allocation)
	case AuthoritySnapshotAbandon:
		if p.SnapshotID == 0 || p.SnapshotID >= s.NextSnapshotID || p.Count != 0 {
			return nil, errors.New("invalid snapshot abandonment request")
		}
		provider, ok := s.VolatileOrigins[p.SnapshotID]
		if !ok {
			if s.OriginDead(p.SnapshotID) {
				return authorityValue(SnapshotOriginProvider{SnapshotID: p.SnapshotID})
			}
			return nil, errors.New("snapshot origin is already published")
		}
		if provider.NodeID != p.NodeID {
			return nil, errors.New("snapshot origin is owned by another provider")
		}
		delete(s.VolatileOrigins, p.SnapshotID)
		return authorityValue(SnapshotOriginProvider{SnapshotID: p.SnapshotID})
	case AuthoritySnapshotPublish:
		if p.SnapshotID == 0 || p.SnapshotID >= s.NextSnapshotID || p.Count != 0 {
			return nil, errors.New("invalid snapshot publication request")
		}
		provider, ok := s.VolatileOrigins[p.SnapshotID]
		if !ok {
			if s.SharedOrigins[p.SnapshotID] {
				// A retry after the node-to-shared handoff committed.
				return authorityValue(SnapshotOriginProvider{SnapshotID: p.SnapshotID})
			}
			return nil, errors.New("snapshot origin was abandoned or retired")
		}
		if provider.NodeID != p.NodeID {
			return nil, errors.New("snapshot origin is owned by another provider")
		}
		delete(s.VolatileOrigins, p.SnapshotID)
		s.SharedOrigins[p.SnapshotID] = true
		return authorityValue(SnapshotOriginProvider{SnapshotID: p.SnapshotID})
	default:
		panic("unreachable")
	}
}

func (s *AuthorityState) applyForkPublish(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityForkPublishPayload](command)
	if err != nil {
		return nil, err
	}
	tx, ok := s.Transactions[p.TransactionID]
	if !ok {
		return nil, os.ErrNotExist
	}
	if tx.Revision != p.TransactionRevision || tx.State != TransactionTargetPaused || tx.Identity.Migration {
		return nil, errors.New("remote fork transaction is not ready to publish")
	}
	node, ok := s.Nodes[tx.Identity.TargetNodeID]
	if !ok || node.Incarnation != p.TargetIncarnation || node.LeaseEpoch != command.Epoch || !node.LeaseActive(command.IssuedAt) || (node.State != NodeReady && node.State != NodePressured) {
		return nil, errors.New("remote fork target node has no valid execution authority")
	}
	if err := tx.Advance(TransactionPublished, "", command.IssuedAt); err != nil {
		return nil, err
	}
	// The target is physically paused until this placement grant commits.
	// A requested running target is activated as a separate, idempotent
	// post-commit step; a requested paused target already reached its state.
	tx.DesiredPaused, tx.Activated = p.DesiredPaused, p.DesiredPaused
	if !transactionIDPattern.MatchString(p.Family) || !transactionIDPattern.MatchString(p.Branch) ||
		!transactionIDPattern.MatchString(p.PhysicalContainer) || !transactionIDPattern.MatchString(p.PhysicalBranch) || p.Branch != tx.Identity.VisibleTargetBranch() {
		return nil, errors.New("remote fork publication has invalid physical identity")
	}
	key := logicalBranchKey(p.Family, p.Branch)
	if _, exists := s.Placements[key]; exists {
		return nil, os.ErrExist
	}
	state := PlacementRunning
	if p.DesiredPaused {
		state = PlacementPaused
	}
	placement := PlacementRecord{
		FamilyID: p.Family, BranchID: p.Branch,
		PhysicalContainer: p.PhysicalContainer, PhysicalBranch: p.PhysicalBranch,
		OwnerNode: tx.Identity.TargetNodeID, OwnerIncarnation: p.TargetIncarnation,
		Generation: 1, State: state, Revision: 1, UpdatedAt: command.IssuedAt,
	}
	if source, ok := s.Placements[logicalBranchKey(tx.Identity.SourceContainer, tx.Identity.SourceBranch)]; ok {
		placement.FamilyName = source.FamilyName
		placement.ImageReference = source.ImageReference
		placement.ImageDigest = source.ImageDigest
		placement.ImageSize = source.ImageSize
		placement.LiveForkEligible = source.LiveForkEligible
		placement.LiveForkWaiver = source.LiveForkWaiver
		placement.FamilyCreatedAt = source.FamilyCreatedAt
		placement.ParentBranch = tx.Identity.SourceBranch
		placement.BranchCreatedAt = command.IssuedAt
		placement.Quarantined = source.Quarantined
	}
	if err := placement.validate(); err != nil {
		return nil, err
	}
	s.Transactions[p.TransactionID] = tx
	s.Placements[key] = placement
	dependencies := []VolatileDependency(nil)
	if tx.SourceNeeded {
		dependencies = []VolatileDependency{{
			TransactionID: tx.Identity.TransactionID, SourceNode: tx.Identity.SourceNodeID,
			SourceIncarnation: tx.SourceIncarnation,
		}}
	}
	s.Branches[key] = BranchAuthority{
		FamilyID: placement.FamilyID, BranchID: placement.BranchID,
		Generation: 1, Lifecycle: string(state), VolatileDependencies: dependencies,
		DependenciesInherited: true, UpdatedAt: command.IssuedAt,
	}
	return authorityValue(ForkPublishResult{Transaction: tx, Placement: placement})
}

func (s *AuthorityState) applyMigrationCutover(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityMigrationCutoverPayload](command)
	if err != nil {
		return nil, err
	}
	tx, ok := s.Transactions[p.TransactionID]
	if !ok {
		return nil, os.ErrNotExist
	}
	if tx.Revision != p.TransactionRevision || tx.State != TransactionPublished || !tx.Identity.Migration || tx.Activated || tx.OwnershipCommitted {
		return nil, errors.New("migration transaction is not published and awaiting cutover")
	}
	if tx.Identity.TransactionID != p.Operation || tx.Identity.SourceContainer != p.Family || tx.Identity.SourceBranch != p.Branch ||
		tx.Identity.TargetNodeID != p.Target || tx.TargetPhysicalContainer != p.PhysicalContainer ||
		tx.TargetPhysicalBranch != p.PhysicalBranch || tx.TargetPhysicalIncarnation != p.TargetIncarnation {
		return nil, errors.New("migration transaction does not match placement cutover")
	}
	if err := s.requireNodeIncarnation(p.Target, p.TargetIncarnation, command.IssuedAt, command.Epoch, false); err != nil {
		return nil, fmt.Errorf("migration target: %w", err)
	}
	key := logicalBranchKey(p.Family, p.Branch)
	placement, ok := s.Placements[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if placement.State != PlacementMigrating || placement.OperationID != p.Operation || placement.TargetNode != p.Target {
		return nil, errors.New("migration operation no longer owns placement cutover")
	}
	placement.OwnerNode, placement.OwnerIncarnation = p.Target, p.TargetIncarnation
	placement.PhysicalContainer, placement.PhysicalBranch = p.PhysicalContainer, p.PhysicalBranch
	placement.Generation++
	placement.State = placement.ResumeState
	placement.ResumeState = ""
	placement.LastOperationID = p.Operation
	placement.OperationID, placement.TargetNode = "", ""
	placement.Revision++
	placement.UpdatedAt = command.IssuedAt
	if err := placement.validate(); err != nil {
		return nil, err
	}
	s.Placements[key] = placement
	tx.OwnershipCommitted = true
	tx.Revision++
	tx.UpdatedAt = command.IssuedAt
	if branch, ok := s.Branches[key]; ok {
		// Freeze the transitive dependencies represented by the source state at
		// cutover. Newer migrations append their own edge afterward and must not
		// be settled when this transaction becomes independent.
		tx.InheritedDependencies = tx.InheritedDependencies[:0]
		for _, dependency := range branch.VolatileDependencies {
			if dependency.TransactionID != tx.Identity.TransactionID {
				tx.InheritedDependencies = append(tx.InheritedDependencies, dependency.TransactionID)
			}
		}
		if tx.SourceNeeded {
			branch.VolatileDependencies = appendVolatileDependency(branch.VolatileDependencies, VolatileDependency{
				TransactionID: tx.Identity.TransactionID, SourceNode: tx.Identity.SourceNodeID,
				SourceIncarnation: tx.SourceIncarnation,
			})
		}
		branch.Generation = placement.Generation
		branch.Lifecycle = string(placement.State)
		branch.UpdatedAt = command.IssuedAt
		s.Branches[key] = branch
	}
	s.Transactions[p.TransactionID] = tx
	return authorityValue(MigrationCutoverResult{Transaction: tx, Placement: placement})
}

func (s *AuthorityState) applyNode(command AuthorityCommand) (json.RawMessage, error) {
	switch command.Kind {
	case AuthorityNodeRegister:
		p, err := decodeAuthorityPayload[AuthorityNodeRegisterPayload](command)
		if err != nil {
			return nil, err
		}
		leaseUntil := p.LeaseUntil
		if p.LeaseDuration > 0 {
			leaseUntil = command.IssuedAt.Add(p.LeaseDuration)
		}
		if !leaseUntil.After(command.IssuedAt) {
			return nil, errors.New("node registration lease does not extend beyond command time")
		}
		record := p.Record
		if current, ok := s.Nodes[record.NodeID]; ok {
			if current.Incarnation != record.Incarnation && current.LeaseActive(command.IssuedAt) {
				return nil, fmt.Errorf("node %q is owned by live incarnation %q", record.NodeID, current.Incarnation)
			}
			if current.Incarnation != record.Incarnation {
				s.invalidateNodeIncarnation(current.NodeID, current.Incarnation, false,
					"source or target daemon incarnation ended before volatile state became durable", command.IssuedAt)
				// A shared-GC lease is fenced by the daemon incarnation as well as
				// its generation. Re-registration must revoke the old holder in the
				// same authority transition; otherwise the authority's state contains a
				// lease that no registered incarnation can legally exercise.
				if s.SharedGC.NodeID == current.NodeID && s.SharedGC.Incarnation == current.Incarnation {
					s.SharedGC = SharedGCLease{}
				}
			}
			record.Revision = current.Revision + 1
			if current.Incarnation == record.Incarnation {
				record.StartedAt, record.State = current.StartedAt, current.State
				record.IncomingReservations, record.IncomingReservedBytes = current.IncomingReservations, current.IncomingReservedBytes
			}
		} else {
			record.Revision = 1
		}
		record.State, record.UpdatedAt, record.LeaseExpiresAt, record.LeaseEpoch = NodeJoining, command.IssuedAt, leaseUntil, command.Epoch
		record.LeaseDuration = leaseUntil.Sub(command.IssuedAt)
		if record.StartedAt.IsZero() {
			record.StartedAt = command.IssuedAt
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[record.NodeID] = record
		delete(s.RetiredNodes, record.NodeID)
		return authorityValue(record)
	case AuthorityNodeRenew:
		p, err := decodeAuthorityPayload[AuthorityNodeRenewPayload](command)
		if err != nil {
			return nil, err
		}
		record, ok := s.Nodes[p.NodeID]
		if !ok {
			return nil, os.ErrNotExist
		}
		if record.Incarnation != p.Incarnation || record.State == NodeOffline {
			return nil, errors.New("stale or offline node incarnation")
		}
		record.IncomingReservations = cloneAuthorityMap(record.IncomingReservations)
		leaseUntil := p.LeaseUntil
		if p.LeaseDuration > 0 {
			leaseUntil = command.IssuedAt.Add(p.LeaseDuration)
		}
		if !leaseUntil.After(command.IssuedAt) {
			return nil, errors.New("node renewal lease does not extend beyond command time")
		}
		record.MemoryCurrentBytes = p.MemoryCurrentBytes
		pruneIncomingReservations(&record, command.IssuedAt)
		updateNodePressureState(&record)
		record.Revision++
		record.UpdatedAt, record.LeaseExpiresAt, record.LeaseEpoch = command.IssuedAt, leaseUntil, command.Epoch
		record.LeaseDuration = leaseUntil.Sub(command.IssuedAt)
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[p.NodeID] = record
		return authorityValue(record)
	case AuthorityNodeTransition:
		p, err := decodeAuthorityPayload[AuthorityNodeTransitionPayload](command)
		if err != nil {
			return nil, err
		}
		record, ok := s.Nodes[p.NodeID]
		if !ok {
			return nil, os.ErrNotExist
		}
		if record.Incarnation != p.Incarnation {
			return nil, errors.New("stale node incarnation")
		}
		if record.LeaseEpoch != command.Epoch {
			return nil, errors.New("node has not renewed with the current authority holder")
		}
		if p.State != record.State && !nodeTransitions[record.State][p.State] {
			return nil, fmt.Errorf("illegal node transition %s -> %s", record.State, p.State)
		}
		record.State = p.State
		record.Revision++
		record.UpdatedAt = command.IssuedAt
		if p.State == NodeOffline {
			record.LeaseExpiresAt = command.IssuedAt
		} else if p.LeaseDuration > 0 {
			record.LeaseExpiresAt = command.IssuedAt.Add(p.LeaseDuration)
			record.LeaseDuration = p.LeaseDuration
		} else if p.LeaseUntil.After(command.IssuedAt) {
			record.LeaseExpiresAt = p.LeaseUntil
			record.LeaseDuration = p.LeaseUntil.Sub(command.IssuedAt)
		} else {
			return nil, errors.New("node transition lease does not extend beyond command time")
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[p.NodeID] = record
		if p.State == NodeOffline && s.SharedGC.NodeID == p.NodeID && s.SharedGC.Incarnation == p.Incarnation {
			// Clean retirement is an incarnation fence just like expiry or
			// replacement. Revoke shared-data mutation authority in the same
			// authority command so a successor does not have to wait for a lease owned
			// by a process that has deliberately gone away. The retiring daemon
			// completes its own collection before proposing NodeOffline.
			s.SharedGC = SharedGCLease{}
		}
		if p.State == NodeReady || p.State == NodePressured {
			// Recovery adopts every locally retained placement while the new
			// incarnation is joining. Anything left behind when it becomes
			// schedulable has no executor and is no longer recoverable.
			for key, placement := range s.Placements {
				if placement.OwnerNode == p.NodeID && placement.OwnerIncarnation != p.Incarnation {
					s.markBranchCorrupted(key, "branch owner incarnation was not recovered before node admission", command.IssuedAt)
				}
			}
		}
		return authorityValue(record)
	case AuthorityNodeExpire:
		p, err := decodeAuthorityPayload[AuthorityNodeExpirePayload](command)
		if err != nil {
			return nil, err
		}
		record, ok := s.Nodes[p.NodeID]
		if !ok {
			return nil, os.ErrNotExist
		}
		if record.Incarnation != p.Incarnation || record.Revision != p.Revision {
			return nil, errors.New("stale node expiry fence")
		}
		if record.State == NodeOffline {
			return authorityValue(record)
		}
		if command.IssuedAt.Before(record.LeaseExpiresAt) {
			return nil, errors.New("node execution lease has not expired")
		}
		record.State = NodeOffline
		record.Revision++
		record.UpdatedAt, record.LeaseExpiresAt = command.IssuedAt, command.IssuedAt
		s.Nodes[p.NodeID] = record
		s.suspendNodeIncarnation(p.NodeID, p.Incarnation,
			"node stopped renewing its execution lease while this was in flight", command.IssuedAt)
		return authorityValue(record)
	case AuthorityNodeRemove:
		p, err := decodeAuthorityPayload[AuthorityNodeRemovePayload](command)
		if err != nil {
			return nil, err
		}
		record, ok := s.Nodes[p.NodeID]
		if !ok {
			return nil, os.ErrNotExist
		}
		if record.Incarnation != p.Incarnation || record.Revision != p.Revision {
			return nil, errors.New("stale node retirement fence")
		}
		if record.State != NodeOffline {
			return nil, errors.New("node must be offline before removing its membership record")
		}
		// An expired node is only unavailable until it returns; removing it
		// is what gives up what it held.
		s.invalidateNodeIncarnation(p.NodeID, p.Incarnation, true,
			"its node was removed while offline", command.IssuedAt)
		if s.SharedGC.NodeID == p.NodeID && s.SharedGC.Incarnation == p.Incarnation {
			return nil, errors.New("node still owns the shared GC lease")
		}
		for _, placement := range s.Placements {
			if placement.OwnerNode == p.NodeID && placement.OwnerIncarnation == p.Incarnation {
				return nil, errors.New("node still owns branch placements")
			}
		}
		for _, tx := range s.Transactions {
			if tx.Terminal() {
				continue
			}
			if (tx.Identity.SourceNodeID == p.NodeID && tx.SourceIncarnation == p.Incarnation && tx.SourceNeeded) ||
				(tx.Identity.TargetNodeID == p.NodeID && tx.TargetIncarnation == p.Incarnation) {
				return nil, errors.New("node still participates in a nonterminal distributed transaction")
			}
		}
		for _, branch := range s.Branches {
			for _, dependency := range branch.VolatileDependencies {
				if dependency.SourceNode == p.NodeID && dependency.SourceIncarnation == p.Incarnation {
					return nil, errors.New("node still provides volatile branch state")
				}
			}
		}
		for _, origin := range s.VolatileOrigins {
			if origin.NodeID == p.NodeID {
				return nil, errors.New("node still provides volatile snapshot origins")
			}
		}
		for _, other := range s.Nodes {
			for _, reservation := range other.IncomingReservations {
				if reservation.SourceNode == p.NodeID && reservation.CompletedAt.IsZero() {
					return nil, errors.New("node still owns an incoming-capacity reservation")
				}
			}
		}
		delete(s.Nodes, p.NodeID)
		s.RetiredNodes[p.NodeID] = p.Incarnation
		return authorityValue(record)
	default:
		p, err := decodeAuthorityPayload[AuthorityCapacityPayload](command)
		if err != nil {
			return nil, err
		}
		record, ok := s.Nodes[p.NodeID]
		if !ok {
			return nil, os.ErrNotExist
		}
		if record.Incarnation != p.Incarnation {
			return nil, errors.New("stale node incarnation")
		}
		if record.LeaseEpoch != command.Epoch || !record.LeaseActive(command.IssuedAt) {
			return nil, errors.New("node capacity authority has not been renewed in the current term")
		}
		record.IncomingReservations = cloneAuthorityMap(record.IncomingReservations)
		pruneIncomingReservations(&record, command.IssuedAt)
		switch command.Kind {
		case AuthorityCapacityReserve:
			expiresAt := p.ExpiresAt
			if p.TTL > 0 {
				expiresAt = command.IssuedAt.Add(p.TTL)
			}
			if !record.AcceptsPlacement(command.IssuedAt) || !transactionIDPattern.MatchString(p.OperationID) || !transactionIDPattern.MatchString(p.SourceNode) || p.Bytes == 0 || !expiresAt.After(command.IssuedAt) {
				return nil, errors.New("invalid or unavailable capacity reservation")
			}
			if old, ok := record.IncomingReservations[p.OperationID]; ok {
				if old.SourceNode != p.SourceNode || old.Bytes != p.Bytes {
					return nil, errors.New("capacity operation reused with different input")
				}
				return authorityValue(record)
			}
			if record.MemoryCurrentBytes > ^uint64(0)-record.IncomingReservedBytes-p.Bytes {
				return nil, errors.New("capacity reservation overflows")
			}
			projected := record.MemoryCurrentBytes + record.IncomingReservedBytes + p.Bytes
			if record.MemoryTargetBytes != 0 && projected > record.MemoryTargetBytes {
				return nil, errors.New("capacity target exceeded")
			}
			if record.IncomingReservations == nil {
				record.IncomingReservations = make(map[string]IncomingReservation)
			}
			record.IncomingReservations[p.OperationID] = IncomingReservation{SourceNode: p.SourceNode, Bytes: p.Bytes, AccountedAtBytes: projected, ExpiresAt: expiresAt}
			record.IncomingReservedBytes += p.Bytes
		case AuthorityCapacityComplete:
			if old, ok := record.IncomingReservations[p.OperationID]; ok && old.CompletedAt.IsZero() {
				old.CompletedAt = command.IssuedAt
				record.IncomingReservations[p.OperationID] = old
			}
		case AuthorityCapacityRelease:
			delete(record.IncomingReservations, p.OperationID)
			pruneIncomingReservations(&record, command.IssuedAt)
		}
		record.Revision++
		record.UpdatedAt = command.IssuedAt
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[p.NodeID] = record
		return authorityValue(record)
	}
}

// suspendNodeIncarnation settles what an incarnation whose lease expired was
// in the middle of. A node that stopped renewing is unavailable, not lost: it
// may be cut off, or its daemon restarting, with its branches stopped in
// memory and on disk. Operations in flight through it fail, and branches it
// had not finished constructing go back to be built elsewhere. Its branches,
// the snapshots it provides, and forks still paging from it wait for it to
// return as the same incarnation, or for its removal to give them up.
func (s *AuthorityState) suspendNodeIncarnation(nodeID, incarnation, reason string, at time.Time) {
	for id, tx := range s.Transactions {
		// A published fork or migration exists, and only pages what it has not
		// yet copied from its source; it waits too.
		if tx.Terminal() || tx.State == TransactionPublished || tx.State == TransactionDraining {
			continue
		}
		source := tx.Identity.SourceNodeID == nodeID && tx.SourceIncarnation == incarnation
		target := tx.Identity.TargetNodeID == nodeID && tx.TargetIncarnation == incarnation
		if !source && !target {
			continue
		}
		tx.State, tx.Failure, tx.SourceNeeded = TransactionFailed, reason, false
		tx.Revision++
		tx.UpdatedAt = at
		s.Transactions[id] = tx
	}
	for key, placement := range s.Placements {
		if placement.OwnerNode != nodeID || placement.OwnerIncarnation != incarnation || placement.State != PlacementMaterializing {
			continue
		}
		s.releaseMaterializingPlacement(key, placement, at)
	}
}

// releaseMaterializingPlacement revokes the claim of a node that never
// finished constructing a branch, so that another may build it again: the
// construction had not consumed the branch's durable roots.
func (s *AuthorityState) releaseMaterializingPlacement(key string, placement PlacementRecord, at time.Time) {
	placement.OwnerNode, placement.OwnerIncarnation = "", ""
	placement.PhysicalContainer, placement.PhysicalBranch = "", ""
	placement.State = PlacementUnplaced
	placement.Generation++
	placement.Revision++
	placement.UpdatedAt = at
	s.Placements[key] = placement
	if branch, ok := s.Branches[key]; ok {
		branch.Generation = placement.Generation
		branch.Lifecycle = string(PlacementUnplaced)
		branch.UpdatedAt = at
		s.Branches[key] = branch
	}
}

func (s *AuthorityState) invalidateNodeIncarnation(nodeID, incarnation string, includePlacements bool, reason string, at time.Time) {
	for id, tx := range s.Transactions {
		lostSource := tx.SourceNeeded && tx.Identity.SourceNodeID == nodeID && tx.SourceIncarnation == incarnation
		lostTarget := !tx.Terminal() && tx.Identity.TargetNodeID == nodeID && tx.TargetIncarnation == incarnation
		if !lostSource && !lostTarget {
			continue
		}
		tx.State, tx.Failure, tx.SourceNeeded = TransactionFailed, reason, false
		tx.Revision++
		tx.UpdatedAt = at
		s.Transactions[id] = tx
	}
	for key, branch := range s.Branches {
		lost := false
		if includePlacements {
			if placement, ok := s.Placements[key]; ok && placement.OwnerNode == nodeID && placement.OwnerIncarnation == incarnation {
				if placement.State == PlacementMaterializing {
					s.releaseMaterializingPlacement(key, placement, at)
				} else {
					lost = true
				}
			}
		}
		for _, dependency := range branch.VolatileDependencies {
			if dependency.SourceNode == nodeID && dependency.SourceIncarnation == incarnation {
				lost = true
				break
			}
		}
		if lost {
			s.markBranchCorrupted(key, reason, at)
		}
	}
	// Snapshot providers are fenced by daemon incarnation even though the
	// compact provider index only stores the stable node ID. Once an
	// incarnation is lost, its unpublished objects can never become shared
	// authority. Permanently abandon those IDs in the same authority transition so
	// node removal is possible and a replacement incarnation cannot publish
	// objects allocated by its predecessor.
	for id, provider := range s.VolatileOrigins {
		if provider.NodeID == nodeID {
			delete(s.VolatileOrigins, id)
		}
	}
}

func (s *AuthorityState) applyPlacement(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityPlacementPayload](command)
	if err != nil {
		return nil, err
	}
	if command.Kind == AuthorityPlacementCreate {
		record := p.Record
		if record.State != PlacementUnplaced {
			if err := s.requireNodeIncarnation(record.OwnerNode, record.OwnerIncarnation, command.IssuedAt, command.Epoch, true); err != nil {
				return nil, fmt.Errorf("placement owner: %w", err)
			}
		}
		key := logicalBranchKey(record.FamilyID, record.BranchID)
		if _, ok := s.Placements[key]; ok {
			return nil, os.ErrExist
		}
		record.Generation, record.Revision, record.UpdatedAt = 1, 1, command.IssuedAt
		if err := record.validate(); err != nil {
			return nil, err
		}
		s.Placements[key] = record
		if _, ok := s.Branches[key]; !ok {
			lifecycle := string(record.State)
			// A materializing placement created by create/run already has a
			// prepared filesystem but has not executed guest code. Expose the
			// stable Docker-compatible lifecycle until launch promotes it.
			if record.State == PlacementMaterializing {
				lifecycle = "created"
			}
			s.Branches[key] = BranchAuthority{FamilyID: record.FamilyID, BranchID: record.BranchID, Generation: 1, Lifecycle: lifecycle, UpdatedAt: command.IssuedAt}
		}
		return authorityValue(record)
	}
	key := logicalBranchKey(p.Family, p.Branch)
	record, ok := s.Placements[key]
	if !ok {
		if command.Kind == AuthorityPlacementRemove {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	switch command.Kind {
	case AuthorityPlacementRename:
		if !transactionIDPattern.MatchString(p.NewBranch) {
			return nil, errors.New("invalid replacement branch ID")
		}
		if p.NewBranch == p.Branch {
			return authorityValue(record)
		}
		newKey := logicalBranchKey(p.Family, p.NewBranch)
		if _, exists := s.Placements[newKey]; exists {
			return nil, os.ErrExist
		}
		if record.Generation != p.Generation || record.State == PlacementMigrating || record.State == PlacementMaterializing || record.State == PlacementCorrupted {
			return nil, errors.New("stale or unstable placement cannot be renamed")
		}
		if record.State == PlacementRunning || record.State == PlacementPaused {
			if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
				return nil, fmt.Errorf("placement owner: %w", err)
			}
			if record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation || !transactionIDPattern.MatchString(p.PhysicalBranch) {
				return nil, errors.New("rename does not match the physical placement owner")
			}
		} else if record.State != PlacementUnplaced {
			return nil, fmt.Errorf("branch is %s, not renameable", record.State)
		}
		branch, exists := s.Branches[key]
		if !exists {
			return nil, errors.New("placement has no branch authority")
		}
		if branch.PublicationID != "" {
			publication, exists := s.Publications[publicationKey(p.Family, p.Branch, branch.PublicationID)]
			if !exists || (publication.State != PublicationCommitted && publication.State != PublicationCanceled) {
				return nil, errors.New("branch has an active publication")
			}
		}

		delete(s.Placements, key)
		record.BranchID = p.NewBranch
		if record.State == PlacementRunning || record.State == PlacementPaused {
			record.PhysicalBranch = p.PhysicalBranch
		}
		record.Generation++
		record.Revision++
		record.UpdatedAt = command.IssuedAt
		if err := record.validate(); err != nil {
			return nil, err
		}
		s.Placements[newKey] = record

		delete(s.Branches, key)
		branch.BranchID = p.NewBranch
		branch.Generation = record.Generation
		branch.UpdatedAt = command.IssuedAt
		s.Branches[newKey] = branch

		for childKey, child := range s.Placements {
			if child.FamilyID != p.Family || child.ParentBranch != p.Branch {
				continue
			}
			child.ParentBranch = p.NewBranch
			child.Revision++
			child.UpdatedAt = command.IssuedAt
			s.Placements[childKey] = child
		}
		for oldPublicationKey, publication := range s.Publications {
			if publication.FamilyID != p.Family || publication.BranchID != p.Branch {
				continue
			}
			delete(s.Publications, oldPublicationKey)
			publication.BranchID = p.NewBranch
			publication.UpdatedAt = command.IssuedAt
			s.Publications[publicationKey(p.Family, p.NewBranch, publication.PublicationID)] = publication
		}
		return authorityValue(record)
	case AuthorityPlacementBegin:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
			return nil, fmt.Errorf("migration source: %w", err)
		}
		if target, ok := s.Nodes[p.Target]; !ok || target.LeaseEpoch != command.Epoch || !target.AcceptsPlacement(command.IssuedAt) {
			return nil, errors.New("migration target has no active placement lease")
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation {
			return nil, errors.New("stale placement owner or generation")
		}
		if record.State == PlacementMigrating && record.OperationID == p.Operation && record.TargetNode == p.Target {
			return authorityValue(record)
		}
		if record.State != PlacementRunning && record.State != PlacementPaused {
			return nil, fmt.Errorf("branch is %s, not migratable", record.State)
		}
		record.ResumeState, record.State, record.OperationID, record.TargetNode = record.State, PlacementMigrating, p.Operation, p.Target
	case AuthorityPlacementAdopt:
		if err := s.requireRecoveringNodeIncarnation(p.Owner, p.NewIncarnation, command.IssuedAt, command.Epoch); err != nil {
			return nil, fmt.Errorf("adopted owner: %w", err)
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.OldIncarnation || (record.State != PlacementRunning && record.State != PlacementPaused) {
			return nil, errors.New("stale placement cannot adopt node incarnation")
		}
		record.OwnerIncarnation = p.NewIncarnation
		record.Generation++
	case AuthorityPlacementRecover:
		if err := s.requireRecoveringNodeIncarnation(p.Owner, p.NewIncarnation, command.IssuedAt, command.Epoch); err != nil {
			return nil, fmt.Errorf("recovered owner: %w", err)
		}
		if record.OwnerNode == p.Owner && record.OwnerIncarnation == p.NewIncarnation && record.LastOperationID == p.Operation && record.OperationID == "" {
			return authorityValue(record)
		}
		if record.State != PlacementMigrating || record.OperationID != p.Operation || record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.OldIncarnation {
			return nil, errors.New("migration operation no longer owns source recovery")
		}
		record.OwnerIncarnation = p.NewIncarnation
		record.Generation++
		record.State = record.ResumeState
		record.ResumeState = ""
		record.LastOperationID = p.Operation
		record.OperationID, record.TargetNode = "", ""
	case AuthorityPlacementCommit:
		if err := s.requireNodeIncarnation(p.Target, p.TargetIncarnation, command.IssuedAt, command.Epoch, false); err != nil {
			return nil, fmt.Errorf("migration target: %w", err)
		}
		if record.OwnerNode == p.Target && record.LastOperationID == p.Operation && record.OperationID == "" {
			return authorityValue(record)
		}
		if record.State != PlacementMigrating || record.OperationID != p.Operation || record.TargetNode != p.Target {
			return nil, errors.New("migration operation no longer owns cutover")
		}
		record.OwnerNode, record.OwnerIncarnation = p.Target, p.TargetIncarnation
		record.PhysicalContainer, record.PhysicalBranch = p.PhysicalContainer, p.PhysicalBranch
		record.Generation++
		record.State = record.ResumeState
		record.ResumeState = ""
		record.LastOperationID = p.Operation
		record.OperationID, record.TargetNode = "", ""
	case AuthorityPlacementAbort:
		if record.State != PlacementMigrating || record.OperationID != p.Operation {
			return nil, errors.New("migration operation no longer owns rollback")
		}
		record.State = record.ResumeState
		record.ResumeState, record.OperationID, record.TargetNode = "", "", ""
	case AuthorityPlacementUnplace:
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation || record.State == PlacementMigrating {
			return nil, errors.New("stale or migrating placement cannot be released")
		}
		record.OwnerNode, record.OwnerIncarnation, record.PhysicalContainer, record.PhysicalBranch = "", "", "", ""
		record.State = PlacementUnplaced
		record.Generation++
		record.LastOperationID = ""
	case AuthorityPlacementClaim:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.Epoch, true); err != nil {
			return nil, fmt.Errorf("claimed owner: %w", err)
		}
		if record.State != PlacementUnplaced || record.Generation != p.Generation {
			return nil, errors.New("branch is placed or generation is stale")
		}
		if p.State != PlacementRunning && p.State != PlacementPaused && p.State != PlacementMaterializing {
			return nil, errors.New("invalid claimed state")
		}
		record.OwnerNode, record.OwnerIncarnation, record.PhysicalContainer, record.PhysicalBranch = p.Owner, p.Incarnation, p.PhysicalContainer, p.PhysicalBranch
		record.State = p.State
		record.Generation++
	case AuthorityPlacementStableState:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
			return nil, fmt.Errorf("placement owner: %w", err)
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation || (record.State != PlacementRunning && record.State != PlacementPaused && record.State != PlacementMaterializing) {
			return nil, errors.New("stale or unstable placement")
		}
		if p.State != PlacementRunning && p.State != PlacementPaused {
			return nil, errors.New("invalid stable state")
		}
		record.State = p.State
	case AuthorityPlacementQuarantine:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.Epoch, false); err != nil {
			return nil, fmt.Errorf("placement owner: %w", err)
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation ||
			(record.State != PlacementRunning && record.State != PlacementPaused) {
			return nil, errors.New("stale or non-stable placement cannot change quarantine policy")
		}
		if record.Quarantined == p.Quarantined {
			return authorityValue(record)
		}
		record.Quarantined = p.Quarantined
	case AuthorityPlacementRemove:
		if record.Generation != p.Generation || record.State == PlacementMigrating {
			return nil, errors.New("stale or migrating placement cannot be removed")
		}
		delete(s.Placements, key)
		for childKey, child := range s.Placements {
			if child.FamilyID == record.FamilyID && child.ParentBranch == record.BranchID {
				child.ParentBranch = record.ParentBranch
				s.Placements[childKey] = child
			}
		}
		delete(s.Branches, key)
		// Publication records are identity fences only while their logical
		// branch exists. Keeping terminal history after removal would make the
		// bounded authority state grow with every stop/dehydrate/name-reuse cycle.
		for publicationKey, publication := range s.Publications {
			if publication.FamilyID == p.Family && publication.BranchID == p.Branch {
				delete(s.Publications, publicationKey)
			}
		}
		return nil, nil
	}
	record.Revision++
	record.UpdatedAt = command.IssuedAt
	if err := record.validate(); err != nil {
		return nil, err
	}
	s.Placements[key] = record
	if branch, ok := s.Branches[key]; ok {
		branch.Generation = record.Generation
		if !(command.Kind == AuthorityPlacementAdopt && record.State == PlacementMaterializing && branch.Lifecycle == "created") {
			branch.Lifecycle = string(record.State)
		}
		if command.Kind == AuthorityPlacementClaim {
			// Claiming a durable snapshot creates a new mutable runtime. The
			// committed publication remains immutable history, not the identity
			// of the next stop/dehydrate transaction.
			branch.PublicationID = ""
		}
		if command.Kind == AuthorityPlacementStableState {
			// Materialization establishes a mutable runtime, but it deliberately
			// does not eagerly read every immutable page and file from the cold
			// snapshot. Keep those roots as the runtime's backing roots until a
			// later publication atomically replaces them or branch removal drops
			// them. In particular, filesystem relays do not create per-file
			// reference intents: clearing Roots here would allow shared GC to
			// reclaim a pack while the live runtime could still fault from it.
			//
			// The committed publication record is only transaction history and can
			// still be discarded. BranchAuthority is the single reachability root
			// for the active branch.
			for publicationKey, publication := range s.Publications {
				if publication.FamilyID == branch.FamilyID && publication.BranchID == branch.BranchID && publication.State == PublicationCommitted {
					delete(s.Publications, publicationKey)
				}
			}
		}
		branch.UpdatedAt = command.IssuedAt
		s.Branches[key] = branch
	}
	return authorityValue(record)
}

// NoExecutionLease is the authority's refusal of a command for a node whose
// incarnation has no active execution lease in the authority's term.
const NoExecutionLease = "node incarnation has no active execution lease"

// IsNoExecutionLease reports whether err is the authority's refusal of a
// command for a node without an active execution lease: one the node may
// remedy by renewing its lease.
func IsNoExecutionLease(err error) bool {
	return err != nil && strings.Contains(err.Error(), NoExecutionLease)
}

func (s *AuthorityState) requireNodeIncarnation(nodeID, incarnation string, at time.Time, term uint64, accepting bool) error {
	node, ok := s.Nodes[nodeID]
	if !ok || node.Incarnation != incarnation || node.LeaseEpoch != term || !node.LeaseActive(at) {
		return errors.New(NoExecutionLease)
	}
	if accepting && !node.AcceptsPlacement(at) {
		return errors.New("node is not accepting placement")
	}
	if !accepting && node.State != NodeReady && node.State != NodePressured && node.State != NodeDraining {
		return fmt.Errorf("node state %s has no execution authority", node.State)
	}
	return nil
}

func (s *AuthorityState) requireRecoveringNodeIncarnation(nodeID, incarnation string, at time.Time, term uint64) error {
	node, ok := s.Nodes[nodeID]
	if !ok || node.Incarnation != incarnation || node.LeaseEpoch != term || !node.LeaseActive(at) {
		return errors.New("node incarnation has no active recovery lease")
	}
	if node.State != NodeJoining && node.State != NodeReady && node.State != NodePressured && node.State != NodeDraining {
		return fmt.Errorf("node state %s has no recovery authority", node.State)
	}
	return nil
}

func (s *AuthorityState) applyTransaction(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityTransactionPayload](command)
	if err != nil {
		return nil, err
	}
	if command.Kind == AuthorityTransactionCreate {
		tx := p.Transaction
		if tx.State != TransactionPreparing || tx.Revision != 1 || tx.CreatedAt.IsZero() || tx.UpdatedAt.IsZero() {
			return nil, errors.New("invalid new transaction")
		}
		if err := tx.Identity.Validate(); err != nil {
			return nil, err
		}
		if _, ok := s.Transactions[tx.Identity.TransactionID]; ok {
			return nil, os.ErrExist
		}
		if tx.SourceIncarnation != "" || tx.TargetIncarnation != "" {
			return nil, errors.New("new transaction supplied node incarnation authority")
		}
		if (tx.SourcePhysicalContainer == "") != (tx.SourcePhysicalBranch == "") ||
			(tx.SourcePhysicalContainer != "" && (!transactionIDPattern.MatchString(tx.SourcePhysicalContainer) || !transactionIDPattern.MatchString(tx.SourcePhysicalBranch))) {
			return nil, errors.New("new transaction supplied invalid physical source")
		}
		source, ok := s.Nodes[tx.Identity.SourceNodeID]
		if !ok || source.LeaseEpoch != command.Epoch || !source.LeaseActive(command.IssuedAt) || (source.State != NodeReady && source.State != NodePressured && source.State != NodeDraining) {
			return nil, errors.New("transaction source has no execution authority")
		}
		target, ok := s.Nodes[tx.Identity.TargetNodeID]
		if !ok || target.LeaseEpoch != command.Epoch || !target.AcceptsPlacement(command.IssuedAt) {
			return nil, errors.New("transaction target is not accepting placement")
		}
		tx.SourceIncarnation, tx.TargetIncarnation = source.Incarnation, target.Incarnation
		if tx.Identity.Migration {
			if tx.SourcePhysicalContainer != "" {
				return nil, errors.New("migration transaction supplied physical source outside placement")
			}
			placement, ok := s.Placements[logicalBranchKey(tx.Identity.SourceContainer, tx.Identity.SourceBranch)]
			if !ok || placement.State != PlacementMigrating || placement.OperationID != tx.Identity.TransactionID ||
				placement.OwnerNode != tx.Identity.SourceNodeID || placement.OwnerIncarnation != source.Incarnation ||
				placement.PhysicalContainer == "" || placement.PhysicalBranch == "" {
				return nil, errors.New("migration transaction has no matching physical source placement")
			}
			tx.SourcePhysicalContainer, tx.SourcePhysicalBranch = placement.PhysicalContainer, placement.PhysicalBranch
		}
		s.Transactions[tx.Identity.TransactionID] = tx
		return authorityValue(tx)
	}
	tx, ok := s.Transactions[p.ID]
	if !ok {
		return nil, os.ErrNotExist
	}
	switch command.Kind {
	case AuthorityTransactionRetire:
		if tx.Revision != p.Revision {
			return nil, errors.New("stale transaction revision")
		}
		if !tx.Terminal() {
			return nil, errors.New("nonterminal transaction cannot be retired")
		}
		for _, branch := range s.Branches {
			for _, dependency := range branch.VolatileDependencies {
				if dependency.TransactionID == p.ID {
					return nil, errors.New("transaction remains a volatile branch dependency")
				}
			}
		}
		for _, placement := range s.Placements {
			if placement.OperationID == p.ID || placement.LastOperationID == p.ID {
				return nil, errors.New("transaction remains a placement operation")
			}
			if transactionTargetsPlacement(tx, placement) {
				return nil, errors.New("transaction target placement still exists")
			}
		}
		delete(s.Transactions, p.ID)
		return nil, nil
	case AuthorityTransactionAdvance:
		// A migration whose branch was removed is canceled instead: nothing
		// reads its target any more, and draining the rest of the branch into
		// a target already deleted would only copy it.
		if tx.OwnershipCommitted && (p.Next == TransactionCanceled || p.Next == TransactionFailed) && tx.State != p.Next &&
			(p.Next == TransactionFailed || s.migrationBranchExists(tx)) {
			return nil, errors.New("authoritative migration target cannot be canceled or failed")
		}
		if tx.State == p.Next {
			if p.Next == TransactionFailed && tx.Failure != p.Failure {
				return nil, errors.New("failed retry changed diagnostic")
			}
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision {
			return nil, errors.New("stale transaction revision")
		}
		if p.Metrics != nil {
			if p.Next != TransactionIndependent {
				return nil, errors.New("metrics require independent transition")
			}
			if err := p.Metrics.Validate(); err != nil {
				return nil, err
			}
		}
		if err := tx.Advance(p.Next, p.Failure, command.IssuedAt); err != nil {
			return nil, err
		}
		if p.Metrics != nil {
			copy := *p.Metrics
			tx.Metrics = &copy
		}
		if p.Next == TransactionIndependent {
			for key, branch := range s.Branches {
				updated := removeVolatileDependency(branch.VolatileDependencies, tx.Identity.TransactionID)
				// A migration may capture a branch that is itself still backed by
				// older remote-fork or migration transactions. The exact inherited
				// set was frozen at cutover. Remove only those ancestors here. A
				// newer migration may already have appended another dependency to
				// the same logical branch and must complete its own release fence.
				if tx.Identity.Migration && tx.OwnershipCommitted &&
					branch.FamilyID == tx.Identity.SourceContainer && branch.BranchID == tx.Identity.SourceBranch {
					for _, inheritedID := range tx.InheritedDependencies {
						updated = removeVolatileDependency(updated, inheritedID)
					}
				}
				if len(updated) != len(branch.VolatileDependencies) {
					branch.VolatileDependencies = updated
					branch.UpdatedAt = command.IssuedAt
					s.Branches[key] = branch
				}
			}
			// A predecessor can be released only when no sibling branch retains
			// its edge. Advancing it here is safe: the old target is no longer an
			// execution owner, and the newly independent migration target is a
			// complete replacement for every transitive read it could perform.
			// Source and target cleanup workers observe this committed terminal
			// state and release their process-local sessions idempotently.
			for _, inheritedID := range tx.InheritedDependencies {
				if inheritedID == tx.Identity.TransactionID || volatileDependencyReferenced(s.Branches, inheritedID) {
					continue
				}
				predecessor, ok := s.Transactions[inheritedID]
				if !ok || !predecessor.SourceNeeded ||
					(predecessor.State != TransactionPublished && predecessor.State != TransactionDraining) {
					continue
				}
				if err := predecessor.Advance(TransactionIndependent, "", command.IssuedAt); err != nil {
					return nil, fmt.Errorf("settling inherited dependency %q: %w", inheritedID, err)
				}
				s.Transactions[inheritedID] = predecessor
			}
		}
	case AuthorityTransactionPublish:
		if tx.State == TransactionPublished {
			if tx.DesiredPaused != p.DesiredPaused || tx.Activated != p.Activated ||
				tx.TargetPhysicalContainer != p.PhysicalContainer || tx.TargetPhysicalBranch != p.PhysicalBranch ||
				tx.TargetPhysicalIncarnation != p.TargetIncarnation {
				return nil, errors.New("publication retry changed activation")
			}
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision || tx.State != TransactionTargetPaused {
			return nil, errors.New("transaction is not publishable")
		}
		if err := tx.Advance(TransactionPublished, "", command.IssuedAt); err != nil {
			return nil, err
		}
		if tx.Identity.Migration {
			if !transactionIDPattern.MatchString(p.PhysicalContainer) || !transactionIDPattern.MatchString(p.PhysicalBranch) || p.TargetIncarnation == "" {
				return nil, errors.New("migration publication has invalid physical identity")
			}
		} else if p.PhysicalContainer != "" || p.PhysicalBranch != "" || p.TargetIncarnation != "" {
			return nil, errors.New("ordinary transaction publication unexpectedly names a physical target")
		}
		tx.DesiredPaused, tx.Activated = p.DesiredPaused, p.Activated
		tx.TargetPhysicalContainer, tx.TargetPhysicalBranch = p.PhysicalContainer, p.PhysicalBranch
		tx.TargetPhysicalIncarnation = p.TargetIncarnation
	case AuthorityTransactionActivate:
		if tx.Activated {
			return authorityValue(tx)
		}
		activatable := tx.State == TransactionPublished || tx.State == TransactionDraining || tx.State == TransactionIndependent
		if tx.Revision != p.Revision || !activatable || (tx.Identity.Migration && !tx.OwnershipCommitted) {
			return nil, errors.New("transaction cannot activate")
		}
		tx.Activated = true
		tx.Revision++
		tx.UpdatedAt = command.IssuedAt
	case AuthorityTransactionSession:
		if err := validateSHA256Digest(p.Digest); err != nil {
			return nil, err
		}
		if tx.SessionDigest != "" {
			if tx.SessionDigest != p.Digest {
				return nil, errors.New("transaction bound to another session")
			}
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision || tx.State != TransactionTargetReserved {
			return nil, errors.New("transaction is not reserving a session")
		}
		tx.SessionDigest = p.Digest
		if err := tx.Advance(TransactionSourceCaptured, "", command.IssuedAt); err != nil {
			return nil, err
		}
	case AuthorityTransactionHandoff:
		if p.Graph == nil {
			return nil, errors.New("missing graph handoff")
		}
		if err := p.Graph.validate(); err != nil {
			return nil, err
		}
		if tx.GraphHandoff != nil {
			if *tx.GraphHandoff != *p.Graph {
				return nil, errors.New("transaction bound to another handoff")
			}
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision || tx.State != TransactionPublished {
			return nil, errors.New("transaction is not ready for handoff")
		}
		copy := *p.Graph
		tx.GraphHandoff = &copy
		tx.UpdatedAt = command.IssuedAt
	}
	s.Transactions[p.ID] = tx
	return authorityValue(tx)
}

// migrationBranchExists reports whether the branch a migration moved still
// exists: a placement names the migration or is its branch, or a branch
// depends on it.
func (s *AuthorityState) migrationBranchExists(tx Transaction) bool {
	for _, placement := range s.Placements {
		if placement.OperationID == tx.Identity.TransactionID || placement.LastOperationID == tx.Identity.TransactionID || transactionTargetsPlacement(tx, placement) {
			return true
		}
	}
	return volatileDependencyReferenced(s.Branches, tx.Identity.TransactionID)
}

func transactionTargetsPlacement(tx Transaction, placement PlacementRecord) bool {
	if tx.Identity.Migration {
		return placement.FamilyID == tx.Identity.SourceContainer && placement.BranchID == tx.Identity.SourceBranch
	}
	if placement.BranchID != tx.Identity.VisibleTargetBranch() {
		return false
	}
	return placement.FamilyID == tx.Identity.TargetContainer ||
		placement.PhysicalContainer == tx.Identity.TargetContainer ||
		placement.FamilyID == tx.Identity.SourceContainer ||
		placement.PhysicalContainer == tx.Identity.SourceContainer
}

// TransactionTargetsPlacement reports whether placement is the stable logical
// branch represented by tx. Distributed recovery uses this to distinguish a
// transaction that was originally delivered to a node from a branch that has
// since been dehydrated, rehydrated, or migrated elsewhere.
//
// TargetNodeID alone is historical: it never changes after transaction
// creation. Placement ownership is the execution-authority source of truth.
func TransactionTargetsPlacement(tx Transaction, placement PlacementRecord) bool {
	return transactionTargetsPlacement(tx, placement)
}

func validateDurableRoots(roots DurableRoots) error {
	for name, value := range map[string]string{"memory": roots.MemoryRoot, "object-set": roots.ObjectSetDigest} {
		if value != "" {
			if err := validateSHA256Digest(value); err != nil {
				return fmt.Errorf("%s root: %w", name, err)
			}
		}
	}
	if roots.FilesystemRoot != "" {
		clean := filepath.Clean(roots.FilesystemRoot)
		if filepath.IsAbs(roots.FilesystemRoot) || clean != roots.FilesystemRoot || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("filesystem root is not a clean shared-root-relative manifest")
		}
	}
	if roots.MemoryRoot != "" && roots.MemorySnapshot != 0 {
		return errors.New("durable memory authority names both a digest and a snapshot")
	}
	if roots.MemoryRoot == "" && roots.MemorySnapshot == 0 && roots.FilesystemRoot == "" {
		return errors.New("durable roots are empty")
	}
	return nil
}

func (s *AuthorityState) validateSnapshotRootAuthority(roots DurableRoots) error {
	if roots.MemorySnapshot == 0 {
		return nil
	}
	if roots.MemorySnapshot >= s.NextSnapshotID {
		return errors.New("durable memory snapshot was not allocated")
	}
	if s.OriginDead(roots.MemorySnapshot) {
		return errors.New("durable memory snapshot was abandoned or retired")
	}
	return nil
}

func (s *AuthorityState) applyPublication(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityPublicationPayload](command)
	if err != nil {
		return nil, err
	}
	branchKey := logicalBranchKey(p.Family, p.Branch)
	branch, ok := s.Branches[branchKey]
	if !ok && command.Kind != AuthorityPublicationCancel {
		return nil, os.ErrNotExist
	}
	key := publicationKey(p.Family, p.Branch, p.PublicationID)
	if command.Kind == AuthorityPublicationBegin {
		if branch.Corrupted || branch.Generation != p.BranchGeneration || !transactionIDPattern.MatchString(p.PublicationID) || (p.Lifecycle != "exited" && p.Lifecycle != "dehydrated") {
			return nil, errors.New("stale or invalid publication authorization")
		}
		if old, ok := s.Publications[key]; ok {
			if old.PublicationID != p.PublicationID || old.BranchGeneration != p.BranchGeneration {
				return nil, errors.New("publication identity reused")
			}
			return authorityValue(old)
		}
		if branch.PublicationID != "" && branch.PublicationID != p.PublicationID {
			return nil, errors.New("branch already has an active publication")
		}
		record := PublicationRecord{FamilyID: p.Family, BranchID: p.Branch, BranchGeneration: p.BranchGeneration, PublicationID: p.PublicationID, Lifecycle: p.Lifecycle, PreviousLifecycle: branch.Lifecycle, State: PublicationPreparing, Revision: 1, UpdatedAt: command.IssuedAt}
		s.Publications[key] = record
		branch.PublicationID = p.PublicationID
		if p.Lifecycle == "dehydrated" {
			branch.Lifecycle = "dehydrating"
		} else {
			branch.Lifecycle = p.Lifecycle
		}
		branch.UpdatedAt = command.IssuedAt
		s.Branches[branchKey] = branch
		return authorityValue(record)
	}
	record, ok := s.Publications[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.PublicationID != p.PublicationID || record.BranchGeneration != p.BranchGeneration || record.Revision != p.Revision {
		return nil, errors.New("stale publication authority")
	}
	switch command.Kind {
	case AuthorityPublicationReady:
		if record.State != PublicationPreparing {
			return nil, errors.New("publication capture is not preparing")
		}
		if p.Roots != (DurableRoots{}) {
			if err := validateDurableRoots(p.Roots); err != nil {
				return nil, fmt.Errorf("publication capture roots: %w", err)
			}
			if err := s.validateSnapshotRootAuthority(p.Roots); err != nil {
				return nil, fmt.Errorf("publication capture roots: %w", err)
			}
		}
		record.State, record.CandidateRoots = PublicationReady, p.Roots
	case AuthorityPublicationCommit:
		if record.State != PublicationReady {
			return nil, errors.New("publication capture is not ready")
		}
		if err := validateDurableRoots(p.Roots); err != nil {
			return nil, err
		}
		if err := s.validateSnapshotRootAuthority(p.Roots); err != nil {
			return nil, err
		}
		if record.CandidateRoots.MemorySnapshot != 0 && record.CandidateRoots.MemorySnapshot != p.Roots.MemorySnapshot {
			return nil, errors.New("publication memory snapshot differs from capture fence")
		}
		if record.CandidateRoots.MemoryRoot != "" && record.CandidateRoots.MemoryRoot != p.Roots.MemoryRoot {
			return nil, errors.New("publication memory root differs from capture fence")
		}
		if len(p.SnapshotOrigins) != 0 {
			if err := s.requireNodeIncarnation(p.ProviderNode, p.ProviderIncarnation, command.IssuedAt, command.Epoch, false); err != nil {
				return nil, fmt.Errorf("publication provider: %w", err)
			}
			seen := make(map[uint64]struct{}, len(p.SnapshotOrigins))
			for _, origin := range p.SnapshotOrigins {
				if origin == 0 || origin >= s.NextSnapshotID {
					return nil, fmt.Errorf("publication snapshot origin %d was not allocated", origin)
				}
				if _, duplicate := seen[origin]; duplicate {
					return nil, fmt.Errorf("publication repeats snapshot origin %d", origin)
				}
				seen[origin] = struct{}{}
				provider, ok := s.VolatileOrigins[origin]
				if !ok || provider.NodeID != p.ProviderNode {
					return nil, fmt.Errorf("publication snapshot origin %d is not owned by provider %q", origin, p.ProviderNode)
				}
			}
		}
		current := s.Branches[branchKey]
		if current.Generation != record.BranchGeneration || current.PublicationID != record.PublicationID || current.Corrupted {
			return nil, errors.New("publication authority was superseded")
		}
		record.State, record.CandidateRoots = PublicationCommitted, p.Roots
		placement, ok := s.Placements[branchKey]
		if !ok || placement.Generation != record.BranchGeneration || placement.State == PlacementMigrating {
			return nil, errors.New("publication cannot release stale or migrating placement")
		}
		placement.OwnerNode, placement.OwnerIncarnation, placement.PhysicalContainer, placement.PhysicalBranch = "", "", "", ""
		placement.State = PlacementUnplaced
		placement.Generation++
		placement.Revision++
		placement.LastOperationID, placement.OperationID, placement.TargetNode, placement.ResumeState = "", "", "", ""
		placement.UpdatedAt = command.IssuedAt
		if err := placement.validate(); err != nil {
			return nil, err
		}
		s.Placements[branchKey] = placement
		current.Generation, current.PublicationID = placement.Generation, record.PublicationID
		current.Roots, current.Lifecycle, current.UpdatedAt = p.Roots, record.Lifecycle, command.IssuedAt
		s.Branches[branchKey] = current
		for _, origin := range p.SnapshotOrigins {
			delete(s.VolatileOrigins, origin)
			s.SharedOrigins[origin] = true
		}
	case AuthorityPublicationFail:
		if p.Failure == "" || record.State == PublicationCommitted || record.State == PublicationCanceled {
			return nil, errors.New("invalid publication failure")
		}
		record.State, record.Failure = PublicationFailed, p.Failure
		current := s.Branches[branchKey]
		if current.Generation == record.BranchGeneration && !current.Corrupted {
			current.Lifecycle, current.UpdatedAt = "failed", command.IssuedAt
			s.Branches[branchKey] = current
		}
	case AuthorityPublicationCancel:
		if record.State == PublicationCommitted {
			return nil, errors.New("committed publication cannot be canceled")
		}
		record.State = PublicationCanceled
		current := s.Branches[branchKey]
		if current.Generation == record.BranchGeneration && !current.Corrupted && current.PublicationID == record.PublicationID {
			if record.Lifecycle == "dehydrated" {
				current.Lifecycle = record.PreviousLifecycle
			} else {
				current.Lifecycle = record.Lifecycle
			}
			current.PublicationID = ""
			current.UpdatedAt = command.IssuedAt
			s.Branches[branchKey] = current
		}
	}
	record.Revision++
	record.UpdatedAt = command.IssuedAt
	s.Publications[key] = record
	return authorityValue(record)
}

func (s *AuthorityState) applyBranch(command AuthorityCommand) (json.RawMessage, error) {
	if command.Kind == AuthorityBranchInherit {
		p, err := decodeAuthorityPayload[AuthorityBranchInheritPayload](command)
		if err != nil {
			return nil, err
		}
		key := logicalBranchKey(p.Family, p.Branch)
		branch, ok := s.Branches[key]
		if !ok {
			return nil, os.ErrNotExist
		}
		if branch.Generation != p.BranchGeneration {
			return nil, errors.New("stale branch generation")
		}
		if branch.DependenciesInherited {
			return authorityValue(branch)
		}
		if p.SourceBranch != "" {
			source, ok := s.Branches[logicalBranchKey(p.Family, p.SourceBranch)]
			if !ok {
				return nil, errors.New("dependency source branch does not exist")
			}
			branch.VolatileDependencies = append([]VolatileDependency(nil), source.VolatileDependencies...)
		}
		branch.DependenciesInherited = true
		branch.UpdatedAt = command.IssuedAt
		s.Branches[key] = branch
		return authorityValue(branch)
	}
	p, err := decodeAuthorityPayload[AuthorityPublicationPayload](command)
	if err != nil {
		return nil, err
	}
	key := logicalBranchKey(p.Family, p.Branch)
	branch, ok := s.Branches[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if branch.Generation != p.BranchGeneration {
		return nil, errors.New("stale branch generation")
	}
	switch command.Kind {
	case AuthorityBranchCorrupt:
		if p.Failure == "" {
			return nil, errors.New("corruption requires a reason")
		}
		branch, err = s.corruptBranchClosure(key, p.Failure, command.IssuedAt)
		if err != nil {
			return nil, err
		}
		return authorityValue(branch)
	case AuthorityBranchDiscardMemory:
		placement, ok := s.Placements[key]
		if !ok || placement.Generation != branch.Generation || placement.State != PlacementUnplaced {
			return nil, errors.New("dehydrated branch does not have a stable unplaced generation")
		}
		if branch.Corrupted || branch.Lifecycle != "dehydrated" || branch.Roots.FilesystemRoot == "" || len(branch.VolatileDependencies) != 0 {
			return nil, errors.New("branch does not have a durable dehydrated snapshot")
		}
		branch.Generation++
		branch.Lifecycle = "exited"
		branch.PublicationID = ""
		branch.Roots.MemoryRoot = ""
		branch.Roots.MemorySnapshot = 0
		branch.UpdatedAt = command.IssuedAt
		placement.Generation = branch.Generation
		placement.Revision++
		placement.UpdatedAt = command.IssuedAt
		if err := placement.validate(); err != nil {
			return nil, err
		}
		s.Branches[key] = branch
		s.Placements[key] = placement
		for publicationKey, publication := range s.Publications {
			if publication.FamilyID == p.Family && publication.BranchID == p.Branch && publication.State == PublicationCommitted {
				delete(s.Publications, publicationKey)
			}
		}
		return authorityValue(branch)
	case AuthorityBranchRemoveCorrupted:
		if !branch.Corrupted {
			return nil, errors.New("branch is not corrupted")
		}
		parent := ""
		if placement, ok := s.Placements[key]; ok {
			parent = placement.ParentBranch
		}
		delete(s.Branches, key)
		delete(s.Placements, key)
		for childKey, child := range s.Placements {
			if child.FamilyID == p.Family && child.ParentBranch == p.Branch {
				child.ParentBranch = parent
				s.Placements[childKey] = child
			}
		}
		for key, publication := range s.Publications {
			if publication.FamilyID == p.Family && publication.BranchID == p.Branch {
				delete(s.Publications, key)
			}
		}
		return nil, nil
	}
	return nil, errors.New("invalid branch command")
}

// corruptBranchClosure fences a branch and resolves distributed transactions
// whose volatile state can no longer be completed. A failed target does not
// automatically retire a transaction while a healthy descendant still uses
// the same source session. Losing the source branch is different: every
// branch backed by that transaction has lost its only volatile authority and
// is corrupted in the same authority command.
func (s *AuthorityState) corruptBranchClosure(initialKey, reason string, at time.Time) (BranchAuthority, error) {
	queue := []string{initialKey}
	seen := make(map[string]struct{})
	// sourceLost distinguishes a dead provider from a dead consumer. A dead
	// consumer may share its transaction with surviving locally-forked peers.
	candidates := make(map[string]bool)
	for len(queue) != 0 {
		key := queue[0]
		queue = queue[1:]
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		branch, ok := s.Branches[key]
		if !ok {
			continue
		}
		for _, dependency := range branch.VolatileDependencies {
			if dependency.TransactionID != "" {
				if _, ok := candidates[dependency.TransactionID]; !ok {
					candidates[dependency.TransactionID] = false
				}
			}
		}
		for id, tx := range s.Transactions {
			if tx.Terminal() {
				continue
			}
			sourceKey := logicalBranchKey(tx.Identity.SourceContainer, tx.Identity.SourceBranch)
			targetKey := logicalBranchKey(tx.Identity.TargetContainer, tx.Identity.VisibleTargetBranch())
			if sourceKey == key {
				candidates[id] = true
				for dependentKey, dependent := range s.Branches {
					if branchHasVolatileDependency(dependent, id) {
						queue = append(queue, dependentKey)
					}
				}
			} else if targetKey == key {
				if _, ok := candidates[id]; !ok {
					candidates[id] = false
				}
			}
		}
		s.markBranchCorrupted(key, reason, at)
	}
	for id, sourceLost := range candidates {
		tx, ok := s.Transactions[id]
		if !ok || tx.Terminal() {
			continue
		}
		if !sourceLost {
			referenced := false
			for _, branch := range s.Branches {
				if !branch.Corrupted && branchHasVolatileDependency(branch, id) {
					referenced = true
					break
				}
			}
			if referenced {
				continue
			}
		}
		if err := tx.Advance(TransactionFailed, reason, at); err != nil {
			return BranchAuthority{}, fmt.Errorf("failing transaction %q after branch corruption: %w", id, err)
		}
		s.Transactions[id] = tx
	}
	branch, ok := s.Branches[initialKey]
	if !ok {
		return BranchAuthority{}, os.ErrNotExist
	}
	return branch, nil
}

func branchHasVolatileDependency(branch BranchAuthority, transactionID string) bool {
	for _, dependency := range branch.VolatileDependencies {
		if dependency.TransactionID == transactionID {
			return true
		}
	}
	return false
}

func (s *AuthorityState) markBranchCorrupted(key, reason string, at time.Time) BranchAuthority {
	branch := s.Branches[key]
	if branch.Corrupted {
		return branch
	}
	branch.Corrupted, branch.CorruptionReason, branch.Lifecycle = true, reason, "corrupted"
	branch.PublicationID = ""
	// Corrupted is terminal except for removal. Its former roots must no longer
	// pin shared objects or participate in provider recovery; those objects may
	// be retired immediately. Keeping the stale roots makes a later
	// snapshot invalid as soon as shared GC performs that permitted retirement,
	// which can take every daemon offline while trying to restore the state.
	branch.Roots = DurableRoots{}
	branch.VolatileDependencies = nil
	branch.Generation++
	branch.UpdatedAt = at
	s.Branches[key] = branch
	if placement, ok := s.Placements[key]; ok {
		placement.OwnerNode, placement.OwnerIncarnation, placement.PhysicalContainer, placement.PhysicalBranch = "", "", "", ""
		placement.State = PlacementCorrupted
		placement.ResumeState, placement.OperationID, placement.LastOperationID, placement.TargetNode = "", "", "", ""
		placement.Generation++
		placement.Revision++
		placement.UpdatedAt = at
		s.Placements[key] = placement
	}
	for publicationKey, publication := range s.Publications {
		if publication.FamilyID == branch.FamilyID && publication.BranchID == branch.BranchID && publication.State != PublicationCommitted {
			publication.State = PublicationCanceled
			publication.Revision++
			publication.UpdatedAt = at
			s.Publications[publicationKey] = publication
		}
	}
	return branch
}

// SnapshotBytes returns a deterministic-enough JSON representation for the
// authority's snapshots. Map ordering is defined by encoding/json.
func (s *AuthorityState) SnapshotBytes() ([]byte, error) {
	if s == nil || s.Version != AuthorityFormatVersion {
		return nil, errors.New("invalid authority state")
	}
	return json.Marshal(s)
}

// RestoreAuthorityState validates a decoded authority snapshot before it
// replaces live state.
func RestoreAuthorityState(data []byte) (*AuthorityState, error) {
	var state AuthorityState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if state.Version != AuthorityFormatVersion {
		return nil, fmt.Errorf("authority snapshot version is %d, want %d", state.Version, AuthorityFormatVersion)
	}
	if state.NextSnapshotID == 0 || state.NextSharedGCGeneration == 0 {
		return nil, errors.New("authority snapshot has invalid monotonic allocator state")
	}
	state.initMaps()
	if state.ClusterPolicy.Revision != 0 {
		if err := state.ClusterPolicy.Validate(); err != nil {
			return nil, fmt.Errorf("authority snapshot cluster runtime policy: %w", err)
		}
	}
	for _, node := range state.Nodes {
		if err := node.Validate(); err != nil {
			return nil, err
		}
	}
	for nodeID, incarnation := range state.RetiredNodes {
		if !transactionIDPattern.MatchString(nodeID) || !transactionIDPattern.MatchString(incarnation) {
			return nil, errors.New("authority snapshot has invalid retired node identity")
		}
		if _, live := state.Nodes[nodeID]; live {
			return nil, errors.New("authority snapshot node is both registered and retired")
		}
	}
	for _, placement := range state.Placements {
		if err := placement.validate(); err != nil {
			return nil, err
		}
	}
	for key, branch := range state.Branches {
		if key != logicalBranchKey(branch.FamilyID, branch.BranchID) ||
			!transactionIDPattern.MatchString(branch.FamilyID) || !transactionIDPattern.MatchString(branch.BranchID) ||
			branch.Generation == 0 || branch.UpdatedAt.IsZero() {
			return nil, errors.New("authority snapshot has invalid branch authority")
		}
		if branch.PublicationID != "" && !transactionIDPattern.MatchString(branch.PublicationID) {
			return nil, errors.New("authority snapshot branch has invalid publication identity")
		}
		if branch.Corrupted != (branch.Lifecycle == "corrupted") || (branch.Corrupted && branch.CorruptionReason == "") {
			return nil, errors.New("authority snapshot has inconsistent branch corruption state")
		}
		// Format-5 snapshots created before corrupted roots were cleared may
		// contain roots whose objects shared GC has already retired. Corrupted
		// branches support only removal, so normalize those dead references
		// instead of making the whole cluster unrestorable.
		if branch.Corrupted {
			branch.Roots = DurableRoots{}
			branch.VolatileDependencies = nil
			state.Branches[key] = branch
		}
		if branch.Roots != (DurableRoots{}) {
			if err := validateDurableRoots(branch.Roots); err != nil {
				return nil, fmt.Errorf("authority snapshot branch roots: %w", err)
			}
			if err := state.validateSnapshotRootAuthority(branch.Roots); err != nil {
				return nil, fmt.Errorf("authority snapshot branch roots: %w", err)
			}
		}
		for _, dependency := range branch.VolatileDependencies {
			if !transactionIDPattern.MatchString(dependency.TransactionID) ||
				!transactionIDPattern.MatchString(dependency.SourceNode) ||
				!transactionIDPattern.MatchString(dependency.SourceIncarnation) {
				return nil, errors.New("authority snapshot branch has invalid volatile dependency")
			}
			if _, ok := state.Transactions[dependency.TransactionID]; !ok {
				return nil, errors.New("authority snapshot branch references missing transaction")
			}
		}
	}
	for key, publication := range state.Publications {
		if key != publicationKey(publication.FamilyID, publication.BranchID, publication.PublicationID) ||
			!transactionIDPattern.MatchString(publication.FamilyID) || !transactionIDPattern.MatchString(publication.BranchID) ||
			!transactionIDPattern.MatchString(publication.PublicationID) || publication.BranchGeneration == 0 ||
			publication.Revision == 0 || publication.UpdatedAt.IsZero() ||
			(publication.Lifecycle != "exited" && publication.Lifecycle != "dehydrated") {
			return nil, errors.New("authority snapshot has invalid publication record")
		}
		switch publication.State {
		case PublicationPreparing:
			if publication.CandidateRoots != (DurableRoots{}) || publication.Failure != "" {
				return nil, errors.New("authority snapshot preparing publication retains output")
			}
		case PublicationReady:
			if publication.Failure != "" {
				return nil, errors.New("authority snapshot ready publication retains failure")
			}
			if publication.CandidateRoots != (DurableRoots{}) {
				if err := validateDurableRoots(publication.CandidateRoots); err != nil {
					return nil, fmt.Errorf("authority snapshot ready publication roots: %w", err)
				}
				if err := state.validateSnapshotRootAuthority(publication.CandidateRoots); err != nil {
					return nil, fmt.Errorf("authority snapshot ready publication roots: %w", err)
				}
			}
		case PublicationCommitted:
			if publication.Failure != "" {
				return nil, errors.New("authority snapshot committed publication retains failure")
			}
			if err := validateDurableRoots(publication.CandidateRoots); err != nil {
				return nil, fmt.Errorf("authority snapshot committed publication roots: %w", err)
			}
			if err := state.validateSnapshotRootAuthority(publication.CandidateRoots); err != nil {
				return nil, fmt.Errorf("authority snapshot committed publication roots: %w", err)
			}
		case PublicationFailed:
			if publication.Failure == "" {
				return nil, errors.New("authority snapshot failed publication lacks failure")
			}
			if publication.CandidateRoots != (DurableRoots{}) {
				if err := validateDurableRoots(publication.CandidateRoots); err != nil {
					return nil, fmt.Errorf("authority snapshot failed publication roots: %w", err)
				}
				if err := state.validateSnapshotRootAuthority(publication.CandidateRoots); err != nil {
					return nil, fmt.Errorf("authority snapshot failed publication roots: %w", err)
				}
			}
		case PublicationCanceled:
			if publication.Failure != "" {
				return nil, errors.New("authority snapshot canceled publication retains failure")
			}
		default:
			return nil, errors.New("authority snapshot has unknown publication state")
		}
		branch, ok := state.Branches[logicalBranchKey(publication.FamilyID, publication.BranchID)]
		if !ok {
			return nil, errors.New("authority snapshot publication references missing branch")
		}
		if publication.State == PublicationPreparing || publication.State == PublicationReady || publication.State == PublicationFailed {
			if branch.Generation != publication.BranchGeneration || branch.PublicationID != publication.PublicationID {
				return nil, errors.New("authority snapshot active publication is not fenced by its branch")
			}
		}
	}
	for _, branch := range state.Branches {
		if branch.PublicationID == "" {
			continue
		}
		publication, ok := state.Publications[publicationKey(branch.FamilyID, branch.BranchID, branch.PublicationID)]
		if !ok || (publication.State != PublicationPreparing && publication.State != PublicationReady &&
			publication.State != PublicationFailed && publication.State != PublicationCommitted) {
			return nil, errors.New("authority snapshot branch references missing or inactive publication")
		}
	}
	for id, tx := range state.Transactions {
		if id != tx.Identity.TransactionID {
			return nil, errors.New("transaction snapshot key mismatch")
		}
		if err := tx.Identity.Validate(); err != nil {
			return nil, err
		}
		if len(tx.InheritedDependencies) != 0 && !tx.Identity.Migration {
			return nil, errors.New("non-migration transaction has inherited dependencies")
		}
		if (tx.SourcePhysicalContainer == "") != (tx.SourcePhysicalBranch == "") ||
			(tx.SourcePhysicalContainer != "" &&
				(!transactionIDPattern.MatchString(tx.SourcePhysicalContainer) || !transactionIDPattern.MatchString(tx.SourcePhysicalBranch))) {
			return nil, errors.New("transaction snapshot has invalid physical source identity")
		}
		seenInherited := make(map[string]struct{}, len(tx.InheritedDependencies))
		for _, dependency := range tx.InheritedDependencies {
			if !transactionIDPattern.MatchString(dependency) || dependency == tx.Identity.TransactionID {
				return nil, errors.New("transaction snapshot has invalid inherited dependency")
			}
			if _, duplicate := seenInherited[dependency]; duplicate {
				return nil, errors.New("transaction snapshot repeats inherited dependency")
			}
			seenInherited[dependency] = struct{}{}
		}
	}
	for id, provider := range state.VolatileOrigins {
		if id == 0 || id >= state.NextSnapshotID || provider.SnapshotID != id || provider.NodeID == "" {
			return nil, errors.New("authority snapshot has invalid volatile origin")
		}
	}
	for id, shared := range state.SharedOrigins {
		if !shared || id == 0 || id >= state.NextSnapshotID {
			return nil, errors.New("authority snapshot has invalid shared origin")
		}
		if _, volatile := state.VolatileOrigins[id]; volatile {
			return nil, errors.New("authority snapshot marks an origin volatile and shared")
		}
	}
	for id, retirement := range state.RetiredOrigins {
		if id == 0 || id >= state.NextSnapshotID || retirement.SnapshotID != id || retirement.GCGeneration == 0 ||
			retirement.GCGeneration >= state.NextSharedGCGeneration || retirement.RetiredAt.IsZero() {
			return nil, errors.New("authority snapshot has invalid shared-pack retirement")
		}
		if !state.OriginDead(id) {
			return nil, errors.New("authority snapshot retires a live origin")
		}
	}
	if lease := state.SharedGC; lease.NodeID != "" {
		node, ok := state.Nodes[lease.NodeID]
		if !ok || lease.Incarnation == "" || node.Incarnation != lease.Incarnation || lease.Generation == 0 ||
			lease.Generation >= state.NextSharedGCGeneration || lease.ExpiresAt.IsZero() {
			return nil, errors.New("authority snapshot has invalid shared GC lease")
		}
	} else if lease.Incarnation != "" || lease.Generation != 0 || !lease.ExpiresAt.IsZero() {
		return nil, errors.New("authority snapshot has incomplete shared GC lease")
	}
	if len(state.RequestOrder) != len(state.Requests) {
		return nil, errors.New("authority request index is inconsistent")
	}
	seen := make(map[string]bool, len(state.RequestOrder))
	for _, id := range state.RequestOrder {
		if seen[id] {
			return nil, errors.New("duplicate authority request")
		}
		seen[id] = true
		if _, ok := state.Requests[id]; !ok {
			return nil, errors.New("authority request order references missing request")
		}
	}
	return &state, nil
}

// SortedAuthorityPlacementKeys is useful to deterministic reconcilers and
// tests without exposing the map's iteration order.
func (s *AuthorityState) SortedAuthorityPlacementKeys() []string {
	keys := make([]string, 0, len(s.Placements))
	for key := range s.Placements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
