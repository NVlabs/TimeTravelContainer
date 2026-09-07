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
	AuthorityCapacityReserve       AuthorityCommandKind = "capacity-reserve"
	AuthorityCapacityComplete      AuthorityCommandKind = "capacity-complete"
	AuthorityCapacityRelease       AuthorityCommandKind = "capacity-release"
	AuthorityPlacementCreate       AuthorityCommandKind = "placement-create"
	AuthorityPlacementBegin        AuthorityCommandKind = "placement-begin-migration"
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
	AuthorityPublicationBegin      AuthorityCommandKind = "publication-begin"
	AuthorityPublicationReady      AuthorityCommandKind = "publication-capture-ready"
	AuthorityPublicationCommit     AuthorityCommandKind = "publication-commit-roots"
	AuthorityPublicationFail       AuthorityCommandKind = "publication-fail"
	AuthorityPublicationCancel     AuthorityCommandKind = "publication-cancel"
	AuthorityBranchCorrupt         AuthorityCommandKind = "branch-mark-corrupted"
	AuthorityBranchRemoveCorrupted AuthorityCommandKind = "branch-remove-corrupted"
)

// AuthorityCommand is the sole replicated mutation envelope. IssuedAt is
// assigned by the Raft leader before proposal; followers never consult their
// clocks while applying it.
type AuthorityCommand struct {
	Version   int                  `json:"version"`
	RequestID string               `json:"request_id"`
	IssuedAt  time.Time            `json:"issued_at"`
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
	return AuthorityCommand{Version: AuthorityFormatVersion, RequestID: requestID, IssuedAt: at.UTC(), Kind: kind, Payload: data}, nil
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
	UpdatedAt         time.Time    `json:"updated_at"`
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
	State             PublicationState `json:"state"`
	CandidateRoots    DurableRoots     `json:"candidate_roots,omitempty"`
	Failure           string           `json:"failure,omitempty"`
	Revision          uint64           `json:"revision"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// AuthorityState is snapshotted by Raft. Maps are keyed by stable logical
// identities, never local paths.
type AuthorityState struct {
	Version      int                                `json:"version"`
	Revision     uint64                             `json:"revision"`
	Nodes        map[string]NodeRecord              `json:"nodes,omitempty"`
	Placements   map[string]PlacementRecord         `json:"placements,omitempty"`
	Transactions map[string]Transaction             `json:"transactions,omitempty"`
	Branches     map[string]BranchAuthority         `json:"branches,omitempty"`
	Publications map[string]PublicationRecord       `json:"publications,omitempty"`
	Requests     map[string]appliedAuthorityRequest `json:"requests,omitempty"`
	RequestOrder []string                           `json:"request_order,omitempty"`
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
	if command.Version != AuthorityFormatVersion || !transactionIDPattern.MatchString(command.RequestID) || command.IssuedAt.IsZero() {
		return AuthorityResult{Revision: s.Revision, Error: "invalid authority command envelope"}
	}

	// Clone before mutation so a rejected command cannot leave a partial update.
	encoded, cloneErr := json.Marshal(s)
	if cloneErr != nil {
		return AuthorityResult{Revision: s.Revision, Error: cloneErr.Error()}
	}
	var next AuthorityState
	if cloneErr = json.Unmarshal(encoded, &next); cloneErr != nil {
		return AuthorityResult{Revision: s.Revision, Error: cloneErr.Error()}
	}
	next.initMaps()
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
	if decoder.More() {
		return payload, errors.New("authority payload has trailing data")
	}
	return payload, nil
}

func authorityValue(value any) (json.RawMessage, error) { return json.Marshal(value) }

// Node command payloads.
type AuthorityNodeRegisterPayload struct {
	Record     NodeRecord `json:"record"`
	LeaseUntil time.Time  `json:"lease_until"`
}
type AuthorityNodeRenewPayload struct {
	NodeID, Incarnation string
	MemoryCurrentBytes  uint64
	LeaseUntil          time.Time
}
type AuthorityNodeTransitionPayload struct {
	NodeID, Incarnation string
	State               NodeState
	LeaseUntil          time.Time
}
type AuthorityCapacityPayload struct {
	NodeID, Incarnation, OperationID, SourceNode string
	Bytes                                        uint64
	ExpiresAt                                    time.Time
}

// Placement command payload. Unused fields must be empty and are ignored only
// by command kinds for which they have no meaning.
type AuthorityPlacementPayload struct {
	Record                               PlacementRecord `json:"record,omitempty"`
	Family, Branch, Owner, Incarnation   string
	Generation                           uint64
	Operation, Target, TargetIncarnation string
	PhysicalContainer, PhysicalBranch    string
	State                                PlacementState
}

// Transaction command payload.
type AuthorityTransactionPayload struct {
	Transaction              Transaction              `json:"transaction,omitempty"`
	ID                       string                   `json:"id,omitempty"`
	Revision                 uint64                   `json:"revision,omitempty"`
	Next                     TransactionState         `json:"next,omitempty"`
	Failure, Digest          string                   `json:"failure,omitempty"`
	Metrics                  *TransferMetrics         `json:"metrics,omitempty"`
	Sparse                   *SparseHandoffDescriptor `json:"sparse,omitempty"`
	DesiredPaused, Activated bool
}

// Publication command payload.
type AuthorityPublicationPayload struct {
	Family, Branch, Operation                     string
	BranchGeneration, StorageGeneration, Revision uint64
	Roots                                         DurableRoots
	Failure                                       string
}

func (s *AuthorityState) apply(command AuthorityCommand) (json.RawMessage, error) {
	switch command.Kind {
	case AuthorityNodeRegister, AuthorityNodeRenew, AuthorityNodeTransition,
		AuthorityCapacityReserve, AuthorityCapacityComplete, AuthorityCapacityRelease:
		return s.applyNode(command)
	case AuthorityPlacementCreate, AuthorityPlacementBegin, AuthorityPlacementCommit,
		AuthorityPlacementAbort, AuthorityPlacementUnplace, AuthorityPlacementClaim,
		AuthorityPlacementStableState, AuthorityPlacementRemove:
		return s.applyPlacement(command)
	case AuthorityTransactionCreate, AuthorityTransactionAdvance, AuthorityTransactionPublish,
		AuthorityTransactionActivate, AuthorityTransactionSession, AuthorityTransactionHandoff:
		return s.applyTransaction(command)
	case AuthorityPublicationBegin, AuthorityPublicationReady, AuthorityPublicationCommit,
		AuthorityPublicationFail, AuthorityPublicationCancel:
		return s.applyPublication(command)
	case AuthorityBranchCorrupt, AuthorityBranchRemoveCorrupted:
		return s.applyBranch(command)
	default:
		return nil, fmt.Errorf("unknown authority command %q", command.Kind)
	}
}

func (s *AuthorityState) applyNode(command AuthorityCommand) (json.RawMessage, error) {
	switch command.Kind {
	case AuthorityNodeRegister:
		p, err := decodeAuthorityPayload[AuthorityNodeRegisterPayload](command)
		if err != nil {
			return nil, err
		}
		if !p.LeaseUntil.After(command.IssuedAt) {
			return nil, errors.New("node registration lease does not extend beyond command time")
		}
		record := p.Record
		if current, ok := s.Nodes[record.NodeID]; ok {
			if current.Incarnation != record.Incarnation && current.LeaseActive(command.IssuedAt) {
				return nil, fmt.Errorf("node %q is owned by live incarnation %q", record.NodeID, current.Incarnation)
			}
			record.Revision = current.Revision + 1
			if current.Incarnation == record.Incarnation {
				record.StartedAt, record.State = current.StartedAt, current.State
				record.IncomingReservations, record.IncomingReservedBytes = current.IncomingReservations, current.IncomingReservedBytes
			}
		} else {
			record.Revision = 1
		}
		record.State, record.UpdatedAt, record.LeaseExpiresAt = NodeJoining, command.IssuedAt, p.LeaseUntil
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
		if record.Incarnation != p.Incarnation || !record.LeaseActive(command.IssuedAt) {
			return nil, errors.New("stale or expired node incarnation")
		}
		if !p.LeaseUntil.After(command.IssuedAt) {
			return nil, errors.New("node renewal lease does not extend beyond command time")
		}
		record.MemoryCurrentBytes = p.MemoryCurrentBytes
		pruneIncomingReservations(&record, command.IssuedAt)
		record.Revision++
		record.UpdatedAt, record.LeaseExpiresAt = command.IssuedAt, p.LeaseUntil
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
		if p.State != record.State && !nodeTransitions[record.State][p.State] {
			return nil, fmt.Errorf("illegal node transition %s -> %s", record.State, p.State)
		}
		record.State = p.State
		record.Revision++
		record.UpdatedAt = command.IssuedAt
		if p.State == NodeOffline {
			record.LeaseExpiresAt = command.IssuedAt
		} else if p.LeaseUntil.After(command.IssuedAt) {
			record.LeaseExpiresAt = p.LeaseUntil
		} else {
			return nil, errors.New("node transition lease does not extend beyond command time")
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		s.Nodes[p.NodeID] = record
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
		pruneIncomingReservations(&record, command.IssuedAt)
		switch command.Kind {
		case AuthorityCapacityReserve:
			if !record.AcceptsPlacement(command.IssuedAt) || !transactionIDPattern.MatchString(p.OperationID) || !transactionIDPattern.MatchString(p.SourceNode) || p.Bytes == 0 || !p.ExpiresAt.After(command.IssuedAt) {
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
			record.IncomingReservations[p.OperationID] = IncomingReservation{SourceNode: p.SourceNode, Bytes: p.Bytes, AccountedAtBytes: projected, ExpiresAt: p.ExpiresAt}
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

func (s *AuthorityState) applyPlacement(command AuthorityCommand) (json.RawMessage, error) {
	p, err := decodeAuthorityPayload[AuthorityPlacementPayload](command)
	if err != nil {
		return nil, err
	}
	if command.Kind == AuthorityPlacementCreate {
		record := p.Record
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
	case AuthorityPlacementCommit:
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
		s.Transactions[tx.Identity.TransactionID] = tx
		return authorityValue(tx)
	}
	tx, ok := s.Transactions[p.ID]
	if !ok {
		return nil, os.ErrNotExist
	}
	switch command.Kind {
	case AuthorityTransactionAdvance:
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
		if tx.Revision != p.Revision || tx.State != TransactionPublished || !tx.Identity.Migration {
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
	if !ok {
		return nil, os.ErrNotExist
	}
	key := publicationKey(p.Family, p.Branch, p.StorageGeneration)
	if command.Kind == AuthorityPublicationBegin {
		if branch.Corrupted || branch.Generation != p.BranchGeneration || p.StorageGeneration <= branch.StorageGeneration || !transactionIDPattern.MatchString(p.Operation) {
			return nil, errors.New("stale or invalid publication authorization")
		}
		if old, ok := s.Publications[key]; ok {
			if old.OperationID != p.Operation || old.BranchGeneration != p.BranchGeneration {
				return nil, errors.New("publication generation reused")
			}
			return authorityValue(old)
		}
		record := PublicationRecord{FamilyID: p.Family, BranchID: p.Branch, BranchGeneration: p.BranchGeneration, StorageGeneration: p.StorageGeneration, OperationID: p.Operation, State: PublicationPreparing, Revision: 1, UpdatedAt: command.IssuedAt}
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
		current.StorageGeneration, current.Roots, current.UpdatedAt = record.StorageGeneration, p.Roots, command.IssuedAt
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
		branch.Corrupted, branch.CorruptionReason, branch.Lifecycle = true, p.Failure, "corrupted"
		branch.Generation++
		branch.UpdatedAt = command.IssuedAt
		s.Branches[key] = branch
		if placement, ok := s.Placements[key]; ok {
			placement.OwnerNode, placement.OwnerIncarnation, placement.PhysicalContainer, placement.PhysicalBranch = "", "", "", ""
			placement.State = PlacementUnplaced
			placement.Generation++
			placement.Revision++
			placement.UpdatedAt = command.IssuedAt
			s.Placements[key] = placement
		}
		for key, publication := range s.Publications {
			if publication.FamilyID == p.Family && publication.BranchID == p.Branch && publication.State != PublicationCommitted {
				publication.State = PublicationCanceled
				publication.Revision++
				publication.UpdatedAt = command.IssuedAt
				s.Publications[key] = publication
			}
		}
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
