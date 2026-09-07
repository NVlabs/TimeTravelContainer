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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

const (
	// AuthorityFormatVersion is the durable replicated-state format. It is
	// deliberately independent from the peer data-plane protocol version.
	AuthorityFormatVersion = 1
	maxRememberedRequests  = 65536
)

// AuthorityCommandKind identifies one deterministic state-machine mutation.
type AuthorityCommandKind string

const (
	AuthorityNodeRegister          AuthorityCommandKind = "node-register"
	AuthorityNodeRenew             AuthorityCommandKind = "node-renew"
	AuthorityNodeTransition        AuthorityCommandKind = "node-transition"
	AuthorityNodeExpire            AuthorityCommandKind = "node-expire"
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
	AuthorityPlacementRemove       AuthorityCommandKind = "placement-remove"
	AuthorityTransactionCreate     AuthorityCommandKind = "transaction-create"
	AuthorityTransactionAdvance    AuthorityCommandKind = "transaction-advance"
	AuthorityTransactionPublish    AuthorityCommandKind = "transaction-publish"
	AuthorityTransactionActivate   AuthorityCommandKind = "transaction-activate"
	AuthorityTransactionSession    AuthorityCommandKind = "transaction-bind-session"
	AuthorityTransactionHandoff    AuthorityCommandKind = "transaction-bind-handoff"
	AuthorityMigrationCutover      AuthorityCommandKind = "migration-cutover"
	AuthorityForkPublish           AuthorityCommandKind = "fork-publish"
	AuthorityPublicationBegin      AuthorityCommandKind = "publication-begin"
	AuthorityPublicationReady      AuthorityCommandKind = "publication-capture-ready"
	AuthorityPublicationCommit     AuthorityCommandKind = "publication-commit-roots"
	AuthorityPublicationFail       AuthorityCommandKind = "publication-fail"
	AuthorityPublicationCancel     AuthorityCommandKind = "publication-cancel"
	AuthorityBranchCorrupt         AuthorityCommandKind = "branch-mark-corrupted"
	AuthorityBranchInherit         AuthorityCommandKind = "branch-inherit-dependencies"
	AuthorityBranchRemoveCorrupted AuthorityCommandKind = "branch-remove-corrupted"
)

// AuthorityCommand is the sole replicated mutation envelope. IssuedAt is
// assigned by the Raft leader before proposal; followers never consult their
// clocks while applying it.
type AuthorityCommand struct {
	Version    int                  `json:"version"`
	RequestID  string               `json:"request_id"`
	IssuedAt   time.Time            `json:"issued_at"`
	LeaderTerm uint64               `json:"leader_term"`
	Kind       AuthorityCommandKind `json:"kind"`
	Payload    json.RawMessage      `json:"payload"`
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
	return AuthorityCommand{Version: AuthorityFormatVersion, RequestID: requestID, IssuedAt: at.UTC(), LeaderTerm: 1, Kind: kind, Payload: data}, nil
}

// AuthorityResult is the stable result of an applied request. State-dependent
// rejections are replicated too, so replaying the same request cannot change a
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

// DurableRoots are immutable bulk-data roots. Raft stores these small names,
// never the pages, file extents, or packs reached through them.
type DurableRoots struct {
	MemoryRoot      string `json:"memory_root,omitempty"`
	FilesystemRoot  string `json:"filesystem_root,omitempty"`
	ObjectSetDigest string `json:"object_set_digest,omitempty"`
}

// BranchAuthority is the cluster-wide authority record that is not already
// represented by PlacementRecord.
type BranchAuthority struct {
	FamilyID          string       `json:"family_id"`
	BranchID          string       `json:"branch_id"`
	Generation        uint64       `json:"generation"`
	StorageGeneration uint64       `json:"storage_generation"`
	Lifecycle         string       `json:"lifecycle"`
	Roots             DurableRoots `json:"roots,omitempty"`
	Corrupted         bool         `json:"corrupted,omitempty"`
	CorruptionReason  string       `json:"corruption_reason,omitempty"`
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
// and storage generations that authorized it.
type PublicationRecord struct {
	FamilyID          string           `json:"family_id"`
	BranchID          string           `json:"branch_id"`
	BranchGeneration  uint64           `json:"branch_generation"`
	StorageGeneration uint64           `json:"storage_generation"`
	OperationID       string           `json:"operation_id"`
	Lifecycle         string           `json:"lifecycle"`
	State             PublicationState `json:"state"`
	CandidateRoots    DurableRoots     `json:"candidate_roots,omitempty"`
	Failure           string           `json:"failure,omitempty"`
	Revision          uint64           `json:"revision"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// AuthorityState is snapshotted by Raft. Maps are keyed by stable logical
// identities, never local paths.
type AuthorityState struct {
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
	// LeaderObservedAt is populated only on a linearizable read response. It is
	// zero in the replicated FSM and snapshots, and lets remote readers convert
	// lease deadlines without assuming wall-clock synchronization.
	LeaderObservedAt time.Time                          `json:"leader_observed_at,omitempty"`
	Nodes            map[string]NodeRecord              `json:"nodes,omitempty"`
	Placements       map[string]PlacementRecord         `json:"placements,omitempty"`
	Transactions     map[string]Transaction             `json:"transactions,omitempty"`
	Branches         map[string]BranchAuthority         `json:"branches,omitempty"`
	Publications     map[string]PublicationRecord       `json:"publications,omitempty"`
	Requests         map[string]appliedAuthorityRequest `json:"requests,omitempty"`
	RequestOrder     []string                           `json:"request_order,omitempty"`
}

// NewAuthorityState creates an empty cluster state.
func NewAuthorityState() *AuthorityState {
	state := &AuthorityState{Version: AuthorityFormatVersion}
	state.initMaps()
	return state
}

func (s *AuthorityState) initMaps() {
	if s.Nodes == nil {
		s.Nodes = make(map[string]NodeRecord)
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
	if s.Requests == nil {
		s.Requests = make(map[string]appliedAuthorityRequest)
	}
}

func logicalBranchKey(family, branch string) string { return family + "\x00" + branch }
func publicationKey(family, branch string, generation uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%020d", family, branch, generation)
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

func authorityCommandDigest(command AuthorityCommand) (string, error) {
	copy := command
	copy.RequestID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Apply applies exactly one committed Raft command. It is deterministic: it
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
	if command.Version != AuthorityFormatVersion || !transactionIDPattern.MatchString(command.RequestID) || command.IssuedAt.IsZero() || command.LeaderTerm == 0 {
		return AuthorityResult{Revision: s.Revision, Error: "invalid authority command envelope"}
	}

	// Clone only the maps this command may change. Raft serializes Apply calls,
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
	case AuthorityPlacementCreate, AuthorityPlacementBegin, AuthorityPlacementAdopt, AuthorityPlacementRecover,
		AuthorityPlacementCommit, AuthorityPlacementAbort, AuthorityPlacementUnplace, AuthorityPlacementClaim,
		AuthorityPlacementStableState, AuthorityPlacementRemove:
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Branches = cloneAuthorityMap(s.Branches)
	case AuthorityTransactionCreate, AuthorityTransactionPublish,
		AuthorityTransactionActivate, AuthorityTransactionSession, AuthorityTransactionHandoff:
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
		}
	case AuthorityBranchCorrupt, AuthorityBranchInherit, AuthorityBranchRemoveCorrupted:
		s.Branches = cloneAuthorityMap(s.Branches)
		s.Placements = cloneAuthorityMap(s.Placements)
		s.Publications = cloneAuthorityMap(s.Publications)
	}
}

func (s *AuthorityState) remember(id, digest string, result AuthorityResult) {
	s.Requests[id] = appliedAuthorityRequest{Digest: digest, Result: result}
	s.RequestOrder = append(s.RequestOrder, id)
	if len(s.RequestOrder) > maxRememberedRequests {
		oldest := s.RequestOrder[0]
		s.RequestOrder = append([]string(nil), s.RequestOrder[1:]...)
		delete(s.Requests, oldest)
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

// Node command payloads.
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
type AuthorityCapacityPayload struct {
	NodeID, Incarnation, OperationID, SourceNode string
	Bytes                                        uint64
	ExpiresAt                                    time.Time
	TTL                                          time.Duration
}

// Placement command payload. Unused fields must be empty and are ignored only
// by command kinds for which they have no meaning.
type AuthorityPlacementPayload struct {
	Record                               PlacementRecord `json:"record,omitempty"`
	Family, Branch, Owner, Incarnation   string
	OldIncarnation, NewIncarnation       string
	Generation                           uint64
	Operation, Target, TargetIncarnation string
	PhysicalContainer, PhysicalBranch    string
	State                                PlacementState
}

// Transaction command payload.
type AuthorityTransactionPayload struct {
	Transaction   Transaction              `json:"transaction,omitempty"`
	ID            string                   `json:"id,omitempty"`
	Revision      uint64                   `json:"revision,omitempty"`
	Next          TransactionState         `json:"next,omitempty"`
	Failure       string                   `json:"failure,omitempty"`
	Digest        string                   `json:"digest,omitempty"`
	Metrics       *TransferMetrics         `json:"metrics,omitempty"`
	Sparse        *SparseHandoffDescriptor `json:"sparse,omitempty"`
	DesiredPaused bool                     `json:"desired_paused,omitempty"`
	Activated     bool                     `json:"activated,omitempty"`
}

// Publication command payload.
type AuthorityPublicationPayload struct {
	Family, Branch, Operation                     string
	Lifecycle                                     string
	BranchGeneration, StorageGeneration, Revision uint64
	Roots                                         DurableRoots
	Failure                                       string
}

type AuthorityBranchInheritPayload struct {
	Family, Branch, SourceBranch string
	BranchGeneration             uint64
}

// AuthorityMigrationCutoverPayload joins the transaction and placement CASes
// that used to be committed separately. The target runtime is already paused
// and locally published when this command is proposed; applying it makes that
// target the sole authoritative owner in one Raft revision.
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
	case AuthorityNodeRegister, AuthorityNodeRenew, AuthorityNodeTransition, AuthorityNodeExpire,
		AuthorityCapacityReserve, AuthorityCapacityComplete, AuthorityCapacityRelease:
		return s.applyNode(command)
	case AuthorityPlacementCreate, AuthorityPlacementBegin, AuthorityPlacementAdopt, AuthorityPlacementRecover, AuthorityPlacementCommit,
		AuthorityPlacementAbort, AuthorityPlacementUnplace, AuthorityPlacementClaim,
		AuthorityPlacementStableState, AuthorityPlacementRemove:
		return s.applyPlacement(command)
	case AuthorityTransactionCreate, AuthorityTransactionAdvance, AuthorityTransactionPublish,
		AuthorityTransactionActivate, AuthorityTransactionSession, AuthorityTransactionHandoff:
		return s.applyTransaction(command)
	case AuthorityMigrationCutover:
		return s.applyMigrationCutover(command)
	case AuthorityForkPublish:
		return s.applyForkPublish(command)
	case AuthorityPublicationBegin, AuthorityPublicationReady, AuthorityPublicationCommit,
		AuthorityPublicationFail, AuthorityPublicationCancel:
		return s.applyPublication(command)
	case AuthorityBranchCorrupt, AuthorityBranchInherit, AuthorityBranchRemoveCorrupted:
		return s.applyBranch(command)
	default:
		return nil, fmt.Errorf("unknown authority command %q", command.Kind)
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
	if !ok || node.Incarnation != p.TargetIncarnation || node.LeaseTerm != command.LeaderTerm || !node.LeaseActive(command.IssuedAt) || (node.State != NodeReady && node.State != NodePressured) {
		return nil, errors.New("remote fork target node has no valid execution authority")
	}
	if err := tx.Advance(TransactionPublished, "", command.IssuedAt); err != nil {
		return nil, err
	}
	tx.DesiredPaused, tx.Activated = p.DesiredPaused, true
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
		tx.Identity.TargetNodeID != p.Target || tx.Identity.TargetContainer != p.PhysicalContainer || tx.Identity.VisibleTargetBranch() != p.PhysicalBranch {
		return nil, errors.New("migration transaction does not match placement cutover")
	}
	if err := s.requireNodeIncarnation(p.Target, p.TargetIncarnation, command.IssuedAt, command.LeaderTerm, false); err != nil {
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
	s.Transactions[p.TransactionID] = tx
	if branch, ok := s.Branches[key]; ok {
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
			}
			record.Revision = current.Revision + 1
			if current.Incarnation == record.Incarnation {
				record.StartedAt, record.State = current.StartedAt, current.State
				record.IncomingReservations, record.IncomingReservedBytes = current.IncomingReservations, current.IncomingReservedBytes
			}
		} else {
			record.Revision = 1
		}
		record.State, record.UpdatedAt, record.LeaseExpiresAt, record.LeaseTerm = NodeJoining, command.IssuedAt, leaseUntil, command.LeaderTerm
		record.LeaseDuration = leaseUntil.Sub(command.IssuedAt)
		if record.StartedAt.IsZero() {
			record.StartedAt = command.IssuedAt
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[record.NodeID] = record
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
		record.Revision++
		record.UpdatedAt, record.LeaseExpiresAt, record.LeaseTerm = command.IssuedAt, leaseUntil, command.LeaderTerm
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
		if record.LeaseTerm != command.LeaderTerm {
			return nil, errors.New("node has not renewed with the current Raft leader")
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
		s.invalidateNodeIncarnation(p.NodeID, p.Incarnation, true,
			"authoritative node state was lost after its execution lease expired", command.IssuedAt)
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
		if record.LeaseTerm != command.LeaderTerm || !record.LeaseActive(command.IssuedAt) {
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
				lost = true
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
}

func (s *AuthorityState) applyPlacement(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityPlacementPayload](command)
	if err != nil {
		return nil, err
	}
	if command.Kind == AuthorityPlacementCreate {
		record := p.Record
		if record.State != PlacementUnplaced {
			if err := s.requireNodeIncarnation(record.OwnerNode, record.OwnerIncarnation, command.IssuedAt, command.LeaderTerm, true); err != nil {
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
			s.Branches[key] = BranchAuthority{FamilyID: record.FamilyID, BranchID: record.BranchID, Generation: 1, Lifecycle: string(record.State), UpdatedAt: command.IssuedAt}
		}
		return authorityValue(record)
	}
	key := logicalBranchKey(p.Family, p.Branch)
	record, ok := s.Placements[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	switch command.Kind {
	case AuthorityPlacementBegin:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.LeaderTerm, false); err != nil {
			return nil, fmt.Errorf("migration source: %w", err)
		}
		if target, ok := s.Nodes[p.Target]; !ok || target.LeaseTerm != command.LeaderTerm || !target.AcceptsPlacement(command.IssuedAt) {
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
		if err := s.requireRecoveringNodeIncarnation(p.Owner, p.NewIncarnation, command.IssuedAt, command.LeaderTerm); err != nil {
			return nil, fmt.Errorf("adopted owner: %w", err)
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.OldIncarnation || (record.State != PlacementRunning && record.State != PlacementPaused) {
			return nil, errors.New("stale placement cannot adopt node incarnation")
		}
		record.OwnerIncarnation = p.NewIncarnation
		record.Generation++
	case AuthorityPlacementRecover:
		if err := s.requireRecoveringNodeIncarnation(p.Owner, p.NewIncarnation, command.IssuedAt, command.LeaderTerm); err != nil {
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
		if err := s.requireNodeIncarnation(p.Target, p.TargetIncarnation, command.IssuedAt, command.LeaderTerm, false); err != nil {
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
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.LeaderTerm, true); err != nil {
			return nil, fmt.Errorf("claimed owner: %w", err)
		}
		if record.State != PlacementUnplaced || record.Generation != p.Generation {
			return nil, errors.New("branch is placed or generation is stale")
		}
		if p.State != PlacementRunning && p.State != PlacementPaused {
			return nil, errors.New("invalid claimed state")
		}
		record.OwnerNode, record.OwnerIncarnation, record.PhysicalContainer, record.PhysicalBranch = p.Owner, p.Incarnation, p.PhysicalContainer, p.PhysicalBranch
		record.State = p.State
		record.Generation++
	case AuthorityPlacementStableState:
		if err := s.requireNodeIncarnation(p.Owner, p.Incarnation, command.IssuedAt, command.LeaderTerm, false); err != nil {
			return nil, fmt.Errorf("placement owner: %w", err)
		}
		if record.Generation != p.Generation || record.OwnerNode != p.Owner || record.OwnerIncarnation != p.Incarnation || (record.State != PlacementRunning && record.State != PlacementPaused) {
			return nil, errors.New("stale or unstable placement")
		}
		if p.State != PlacementRunning && p.State != PlacementPaused {
			return nil, errors.New("invalid stable state")
		}
		record.State = p.State
	case AuthorityPlacementRemove:
		if record.Generation != p.Generation || record.State == PlacementMigrating {
			return nil, errors.New("stale or migrating placement cannot be removed")
		}
		delete(s.Placements, key)
		delete(s.Branches, key)
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
		branch.Lifecycle = string(record.State)
		branch.UpdatedAt = command.IssuedAt
		s.Branches[key] = branch
	}
	return authorityValue(record)
}

func (s *AuthorityState) requireNodeIncarnation(nodeID, incarnation string, at time.Time, term uint64, accepting bool) error {
	node, ok := s.Nodes[nodeID]
	if !ok || node.Incarnation != incarnation || node.LeaseTerm != term || !node.LeaseActive(at) {
		return errors.New("node incarnation has no active execution lease")
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
	if !ok || node.Incarnation != incarnation || node.LeaseTerm != term || !node.LeaseActive(at) {
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
			return nil, errors.New("new transaction supplied node incarnations")
		}
		source, ok := s.Nodes[tx.Identity.SourceNodeID]
		if !ok || source.LeaseTerm != command.LeaderTerm || !source.LeaseActive(command.IssuedAt) || (source.State != NodeReady && source.State != NodePressured && source.State != NodeDraining) {
			return nil, errors.New("transaction source has no execution authority")
		}
		target, ok := s.Nodes[tx.Identity.TargetNodeID]
		if !ok || target.LeaseTerm != command.LeaderTerm || !target.AcceptsPlacement(command.IssuedAt) {
			return nil, errors.New("transaction target is not accepting placement")
		}
		tx.SourceIncarnation, tx.TargetIncarnation = source.Incarnation, target.Incarnation
		s.Transactions[tx.Identity.TransactionID] = tx
		return authorityValue(tx)
	}
	tx, ok := s.Transactions[p.ID]
	if !ok {
		return nil, os.ErrNotExist
	}
	switch command.Kind {
	case AuthorityTransactionAdvance:
		if tx.OwnershipCommitted && (p.Next == TransactionCanceled || p.Next == TransactionFailed) {
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
				if len(updated) != len(branch.VolatileDependencies) {
					branch.VolatileDependencies = updated
					branch.UpdatedAt = command.IssuedAt
					s.Branches[key] = branch
				}
			}
		}
	case AuthorityTransactionPublish:
		if tx.State == TransactionPublished {
			if tx.DesiredPaused != p.DesiredPaused || tx.Activated != p.Activated {
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
		tx.DesiredPaused, tx.Activated = p.DesiredPaused, p.Activated
	case AuthorityTransactionActivate:
		if tx.Activated {
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision || tx.State != TransactionPublished || !tx.Identity.Migration || !tx.OwnershipCommitted {
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
		if p.Sparse == nil {
			return nil, errors.New("missing sparse handoff")
		}
		if err := p.Sparse.validate(); err != nil {
			return nil, err
		}
		if tx.SparseHandoff != nil {
			if *tx.SparseHandoff != *p.Sparse {
				return nil, errors.New("transaction bound to another handoff")
			}
			return authorityValue(tx)
		}
		if tx.Revision != p.Revision || tx.State != TransactionPublished {
			return nil, errors.New("transaction is not ready for handoff")
		}
		copy := *p.Sparse
		tx.SparseHandoff = &copy
		tx.UpdatedAt = command.IssuedAt
	}
	s.Transactions[p.ID] = tx
	return authorityValue(tx)
}

func validateDurableRoots(roots DurableRoots) error {
	for name, value := range map[string]string{"memory": roots.MemoryRoot, "filesystem": roots.FilesystemRoot, "object-set": roots.ObjectSetDigest} {
		if value != "" {
			if err := validateSHA256Digest(value); err != nil {
				return fmt.Errorf("%s root: %w", name, err)
			}
		}
	}
	if roots.MemoryRoot == "" && roots.FilesystemRoot == "" {
		return errors.New("durable roots are empty")
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
	key := publicationKey(p.Family, p.Branch, p.StorageGeneration)
	if command.Kind == AuthorityPublicationBegin {
		if branch.Corrupted || branch.Generation != p.BranchGeneration || p.StorageGeneration <= branch.StorageGeneration || !transactionIDPattern.MatchString(p.Operation) || (p.Lifecycle != "exited" && p.Lifecycle != "dehydrated") {
			return nil, errors.New("stale or invalid publication authorization")
		}
		if old, ok := s.Publications[key]; ok {
			if old.OperationID != p.Operation || old.BranchGeneration != p.BranchGeneration {
				return nil, errors.New("publication generation reused")
			}
			return authorityValue(old)
		}
		record := PublicationRecord{FamilyID: p.Family, BranchID: p.Branch, BranchGeneration: p.BranchGeneration, StorageGeneration: p.StorageGeneration, OperationID: p.Operation, Lifecycle: p.Lifecycle, State: PublicationPreparing, Revision: 1, UpdatedAt: command.IssuedAt}
		s.Publications[key] = record
		return authorityValue(record)
	}
	record, ok := s.Publications[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if record.OperationID != p.Operation || record.BranchGeneration != p.BranchGeneration || record.Revision != p.Revision {
		return nil, errors.New("stale publication authority")
	}
	switch command.Kind {
	case AuthorityPublicationReady:
		if record.State != PublicationPreparing {
			return nil, errors.New("publication capture is not preparing")
		}
		record.State = PublicationReady
	case AuthorityPublicationCommit:
		if record.State != PublicationReady {
			return nil, errors.New("publication capture is not ready")
		}
		if err := validateDurableRoots(p.Roots); err != nil {
			return nil, err
		}
		current := s.Branches[branchKey]
		if current.Generation != record.BranchGeneration || current.StorageGeneration >= record.StorageGeneration || current.Corrupted {
			return nil, errors.New("publication authority was superseded")
		}
		record.State, record.CandidateRoots = PublicationCommitted, p.Roots
		placement, ok := s.Placements[branchKey]
		if !ok || placement.Generation != record.BranchGeneration || placement.State == PlacementMigrating {
			return nil, errors.New("publication cannot release stale or migrating placement")
		}
		placement.OwnerNode, placement.OwnerIncarnation, placement.PhysicalContainer, placement.PhysicalBranch = "", "", "", ""
		placement.State = PlacementCorrupted
		placement.Generation++
		placement.Revision++
		placement.LastOperationID, placement.OperationID, placement.TargetNode, placement.ResumeState = "", "", "", ""
		placement.UpdatedAt = command.IssuedAt
		if err := placement.validate(); err != nil {
			return nil, err
		}
		s.Placements[branchKey] = placement
		current.Generation, current.StorageGeneration = placement.Generation, record.StorageGeneration
		current.Roots, current.Lifecycle, current.UpdatedAt = p.Roots, record.Lifecycle, command.IssuedAt
		s.Branches[branchKey] = current
	case AuthorityPublicationFail:
		if p.Failure == "" || record.State == PublicationCommitted || record.State == PublicationCanceled {
			return nil, errors.New("invalid publication failure")
		}
		record.State, record.Failure = PublicationFailed, p.Failure
	case AuthorityPublicationCancel:
		if record.State == PublicationCommitted {
			return nil, errors.New("committed publication cannot be canceled")
		}
		record.State = PublicationCanceled
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
		branch = s.markBranchCorrupted(key, p.Failure, command.IssuedAt)
		return authorityValue(branch)
	case AuthorityBranchRemoveCorrupted:
		if !branch.Corrupted {
			return nil, errors.New("branch is not corrupted")
		}
		delete(s.Branches, key)
		delete(s.Placements, key)
		for key, publication := range s.Publications {
			if publication.FamilyID == p.Family && publication.BranchID == p.Branch {
				delete(s.Publications, key)
			}
		}
		return nil, nil
	}
	return nil, errors.New("invalid branch command")
}

func (s *AuthorityState) markBranchCorrupted(key, reason string, at time.Time) BranchAuthority {
	branch := s.Branches[key]
	if branch.Corrupted {
		return branch
	}
	branch.Corrupted, branch.CorruptionReason, branch.Lifecycle = true, reason, "corrupted"
	branch.Generation++
	branch.UpdatedAt = at
	s.Branches[key] = branch
	if placement, ok := s.Placements[key]; ok {
		placement.OwnerNode, placement.OwnerIncarnation, placement.PhysicalContainer, placement.PhysicalBranch = "", "", "", ""
		placement.State = PlacementUnplaced
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

// SnapshotBytes returns a deterministic-enough JSON representation for Raft
// snapshots. Map ordering is defined by encoding/json.
func (s *AuthorityState) SnapshotBytes() ([]byte, error) {
	if s == nil || s.Version != AuthorityFormatVersion {
		return nil, errors.New("invalid authority state")
	}
	return json.Marshal(s)
}

// RestoreAuthorityState validates a decoded Raft snapshot before it replaces
// live state.
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
	state.initMaps()
	for _, node := range state.Nodes {
		if err := node.Validate(); err != nil {
			return nil, err
		}
	}
	for _, placement := range state.Placements {
		if err := placement.validate(); err != nil {
			return nil, err
		}
	}
	for id, tx := range state.Transactions {
		if id != tx.Identity.TransactionID {
			return nil, errors.New("transaction snapshot key mismatch")
		}
		if err := tx.Identity.Validate(); err != nil {
			return nil, err
		}
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
