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

package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxPlacementRecordBytes = 1 << 20

// PlacementState is the catalog lifecycle of one logical branch.
type PlacementState string

const (
	PlacementRunning PlacementState = "running"
	PlacementPaused  PlacementState = "paused"
	// PlacementMaterializing exclusively assigns construction of a durable
	// process-free branch to one node. It is routable for idempotent start or
	// rehydrate retries, but it grants no authority to execute guest code.
	PlacementMaterializing PlacementState = "materializing"
	PlacementMigrating     PlacementState = "migrating"
	PlacementUnplaced      PlacementState = "unplaced"
	PlacementCorrupted     PlacementState = "corrupted"
)

// PlacementRecord maps a stable logical branch to its current physical
// runtime. Generation is the fencing token presented by routed commands.
type PlacementRecord struct {
	FamilyID string `json:"family_id"`
	BranchID string `json:"branch_id"`
	// Display metadata is replicated with placement so name resolution and
	// cluster-wide inventory never depend on shared-filesystem directory-cache
	// convergence. It is not part of execution authority.
	FamilyName       string    `json:"family_name,omitempty"`
	ImageReference   string    `json:"image_reference,omitempty"`
	ImageDigest      string    `json:"image_digest,omitempty"`
	ImageSize        int64     `json:"image_size,omitempty"`
	LiveForkEligible bool      `json:"live_fork_eligible,omitempty"`
	LiveForkWaiver   string    `json:"live_fork_waiver,omitempty"`
	FamilyCreatedAt  time.Time `json:"family_created_at,omitempty"`
	ParentBranch     string    `json:"parent_branch,omitempty"`
	BranchCreatedAt  time.Time `json:"branch_created_at,omitempty"`
	// Quarantined is cluster-wide branch policy. It belongs beside placement
	// authority rather than in one physical runtime's metadata, because that
	// runtime is replaced during migration and may be invisible to control-only
	// nodes.
	Quarantined       bool           `json:"quarantined,omitempty"`
	PhysicalContainer string         `json:"physical_container,omitempty"`
	PhysicalBranch    string         `json:"physical_branch,omitempty"`
	OwnerNode         string         `json:"owner_node,omitempty"`
	OwnerIncarnation  string         `json:"owner_incarnation,omitempty"`
	Generation        uint64         `json:"generation"`
	State             PlacementState `json:"state"`
	ResumeState       PlacementState `json:"resume_state,omitempty"`
	OperationID       string         `json:"operation_id,omitempty"`
	LastOperationID   string         `json:"last_operation_id,omitempty"`
	TargetNode        string         `json:"target_node,omitempty"`
	Revision          uint64         `json:"revision"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

func (r PlacementRecord) validate() error {
	if !transactionIDPattern.MatchString(r.FamilyID) || !transactionIDPattern.MatchString(r.BranchID) {
		return errors.New("invalid placement family or branch ID")
	}
	if r.Generation == 0 || r.Revision == 0 || r.UpdatedAt.IsZero() {
		return errors.New("invalid placement generation, revision, or timestamp")
	}
	if r.LastOperationID != "" && !transactionIDPattern.MatchString(r.LastOperationID) {
		return errors.New("placement has invalid last operation")
	}
	switch r.State {
	case PlacementRunning, PlacementPaused, PlacementMaterializing:
		if !transactionIDPattern.MatchString(r.OwnerNode) || !transactionIDPattern.MatchString(r.OwnerIncarnation) ||
			!transactionIDPattern.MatchString(r.PhysicalContainer) || !transactionIDPattern.MatchString(r.PhysicalBranch) {
			return errors.New("placed branch lacks a valid owner or physical identity")
		}
		if r.OperationID != "" || r.TargetNode != "" || r.ResumeState != "" {
			return errors.New("stable placement retains migration fields")
		}
	case PlacementMigrating:
		if !transactionIDPattern.MatchString(r.OwnerNode) || !transactionIDPattern.MatchString(r.OwnerIncarnation) ||
			!transactionIDPattern.MatchString(r.OperationID) || !transactionIDPattern.MatchString(r.TargetNode) ||
			(r.ResumeState != PlacementRunning && r.ResumeState != PlacementPaused) {
			return errors.New("migrating placement lacks source, target, operation, or resume state")
		}
	case PlacementUnplaced, PlacementCorrupted:
		if r.OwnerNode != "" || r.OwnerIncarnation != "" || r.PhysicalContainer != "" || r.PhysicalBranch != "" ||
			r.OperationID != "" || r.LastOperationID != "" || r.TargetNode != "" || r.ResumeState != "" {
			return errors.New("unplaced branch retains an owner")
		}
	default:
		return fmt.Errorf("invalid placement state %q", r.State)
	}
	return nil
}

// PlacementStore is the initial shared-root implementation of the placement
// catalog. The API is deliberately CAS-shaped so a linearizable service can
// replace the storage implementation without changing migration semantics.
type PlacementStore struct {
	dir string
}

// PlacementCatalog is the linearizable ownership interface consumed by
// routing and migration. PlacementStore is its shared-filesystem backend; a
// replicated service can implement the same CAS contract without changing
// callers.
type PlacementCatalog interface {
	Create(PlacementRecord, time.Time) (*PlacementRecord, error)
	BeginMigration(family, branch, owner, incarnation string, generation uint64, operation, target string, now time.Time) (*PlacementRecord, error)
	AdoptOwnerIncarnation(family, branch, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error)
	RecoverSourceMigration(family, branch, operation, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error)
	CommitMigration(family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string, now time.Time) (*PlacementRecord, error)
	AbortMigration(family, branch, operation string, now time.Time) (*PlacementRecord, error)
	Unplace(family, branch, owner, incarnation string, generation uint64, now time.Time) (*PlacementRecord, error)
	ClaimUnplaced(family, branch, owner, incarnation, physicalContainer, physicalBranch string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error)
	UpdateStableState(family, branch, owner, incarnation string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error)
	SetQuarantine(family, branch, owner, incarnation string, generation uint64, quarantined bool, now time.Time) (*PlacementRecord, error)
	Rename(family, branch, newBranch, owner, incarnation, physicalBranch string, generation uint64, now time.Time) (*PlacementRecord, error)
	Remove(family, branch string, generation uint64) error
	Lookup(family, branch string) (*PlacementRecord, error)
	List() ([]PlacementRecord, error)
	// ListFamily is List for the placements of one family.
	ListFamily(family string) ([]PlacementRecord, error)
}

var _ PlacementCatalog = (*PlacementStore)(nil)

func OpenPlacementStore(dir string) (*PlacementStore, error) {
	if dir == "" {
		return nil, errors.New("placement directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &PlacementStore{dir: dir}, nil
}

func placementKey(family, branch string) (string, error) {
	if !transactionIDPattern.MatchString(family) || !transactionIDPattern.MatchString(branch) {
		return "", errors.New("invalid placement family or branch ID")
	}
	digest := sha256.Sum256([]byte(family + "\x00" + branch))
	return hex.EncodeToString(digest[:]), nil
}

func (s *PlacementStore) recordLockPath(key string) string {
	// A fixed shard set keeps placement metadata bounded when a long-lived
	// family creates and removes many branch names. The catalog lock protects
	// snapshots; collisions here merely serialize unrelated branch mutations.
	return filepath.Join(s.dir, ".record-"+key[:2]+".lock")
}

func (s *PlacementStore) withCatalogLock(exclusive bool, fn func() error) error {
	lock, err := os.OpenFile(filepath.Join(s.dir, ".catalog.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), operation); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *PlacementStore) withRecordLock(family, branch string, fn func() error) error {
	return s.withCatalogLock(false, func() error {
		key, err := placementKey(family, branch)
		if err != nil {
			return err
		}
		lock, err := os.OpenFile(s.recordLockPath(key), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		return fn()
	})
}

func (s *PlacementStore) withRecordReadLock(family, branch string, fn func() error) error {
	return s.withCatalogLock(false, func() error {
		key, err := placementKey(family, branch)
		if err != nil {
			return err
		}
		lock, err := os.OpenFile(s.recordLockPath(key), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
			return err
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		return fn()
	})
}

// Create installs an initial placement if the logical branch is absent.
func (s *PlacementStore) Create(record PlacementRecord, now time.Time) (*PlacementRecord, error) {
	record.Generation = 1
	record.Revision = 1
	record.UpdatedAt = now
	if err := record.validate(); err != nil {
		return nil, err
	}
	var result PlacementRecord
	err := s.withRecordLock(record.FamilyID, record.BranchID, func() error {
		if _, err := s.load(record.FamilyID, record.BranchID); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// BeginMigration fences concurrent moves at an expected ownership generation.
func (s *PlacementStore) BeginMigration(family, branch, owner, incarnation string, generation uint64, operation, target string, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.Generation != generation || record.OwnerNode != owner || record.OwnerIncarnation != incarnation {
			return fmt.Errorf("stale placement owner or generation")
		}
		if record.State != PlacementRunning && record.State != PlacementPaused {
			if record.State == PlacementMigrating && record.OperationID == operation && record.TargetNode == target {
				result = record
				return nil
			}
			return fmt.Errorf("branch is %s, not migratable", record.State)
		}
		record.ResumeState = record.State
		record.State = PlacementMigrating
		record.OperationID = operation
		record.TargetNode = target
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// AdoptOwnerIncarnation fences a branch to a restarted daemon that has already
// reacquired the same node ID. NodeDirectory registration guarantees that the
// previous incarnation's lease expired before the replacement was admitted.
func (s *PlacementStore) AdoptOwnerIncarnation(family, branch, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.Generation != generation || record.OwnerNode != owner || record.OwnerIncarnation != oldIncarnation {
			return errors.New("stale placement cannot adopt node incarnation")
		}
		if record.State != PlacementRunning && record.State != PlacementPaused {
			return fmt.Errorf("placement is %s, not stable", record.State)
		}
		record.OwnerIncarnation = newIncarnation
		record.Generation++
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RecoverSourceMigration rolls an interrupted migration back to its source
// while fencing the placement to the source daemon's new incarnation. The
// caller must first prove that the old owner incarnation is no longer live and
// restore the branch to ResumeState. Combining rollback and incarnation
// adoption in one durable transition prevents another crash from leaving a
// stable placement owned by a dead daemon incarnation.
func (s *PlacementStore) RecoverSourceMigration(family, branch, operation, owner, oldIncarnation, newIncarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.State != PlacementMigrating || record.OperationID != operation || record.Generation != generation ||
			record.OwnerNode != owner || record.OwnerIncarnation != oldIncarnation {
			if record.OwnerNode == owner && record.OwnerIncarnation == newIncarnation &&
				record.LastOperationID == operation && record.OperationID == "" {
				result = record
				return nil
			}
			return errors.New("placement migration operation no longer owns source recovery")
		}
		record.OwnerIncarnation = newIncarnation
		record.Generation++
		record.State = record.ResumeState
		record.ResumeState = ""
		record.LastOperationID = operation
		record.OperationID = ""
		record.TargetNode = ""
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CommitMigration atomically changes the runnable owner and increments the
// fencing generation. Retrying the committed operation is idempotent.
func (s *PlacementStore) CommitMigration(family, branch, operation, target, targetIncarnation, physicalContainer, physicalBranch string, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.State != PlacementMigrating || record.OperationID != operation || record.TargetNode != target {
			if record.OwnerNode == target && record.LastOperationID == operation && record.OperationID == "" {
				result = record
				return nil
			}
			return errors.New("placement migration operation no longer owns cutover")
		}
		record.OwnerNode = target
		record.OwnerIncarnation = targetIncarnation
		record.PhysicalContainer = physicalContainer
		record.PhysicalBranch = physicalBranch
		record.Generation++
		record.State = record.ResumeState
		record.ResumeState = ""
		record.LastOperationID = operation
		record.OperationID = ""
		record.TargetNode = ""
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// AbortMigration restores the source placement if cutover has not occurred.
func (s *PlacementStore) AbortMigration(family, branch, operation string, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.State != PlacementMigrating || record.OperationID != operation {
			return errors.New("placement migration operation no longer owns rollback")
		}
		record.State = record.ResumeState
		record.ResumeState = ""
		record.OperationID = ""
		record.TargetNode = ""
		record.Revision++
		record.UpdatedAt = now
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Unplace releases runtime ownership after durable dehydration or exit.
func (s *PlacementStore) Unplace(family, branch, owner, incarnation string, generation uint64, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.Generation != generation || record.OwnerNode != owner || record.OwnerIncarnation != incarnation || record.State == PlacementMigrating {
			return errors.New("stale or migrating placement cannot be released")
		}
		record.OwnerNode = ""
		record.OwnerIncarnation = ""
		record.PhysicalContainer = ""
		record.PhysicalBranch = ""
		record.LastOperationID = ""
		record.State = PlacementUnplaced
		record.Generation++
		record.Revision++
		record.UpdatedAt = now
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ClaimUnplaced assigns a durable process-free branch to a ready node.
func (s *PlacementStore) ClaimUnplaced(family, branch, owner, incarnation, physicalContainer, physicalBranch string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	if state != PlacementRunning && state != PlacementPaused && state != PlacementMaterializing {
		return nil, fmt.Errorf("invalid claimed placement state %q", state)
	}
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.State != PlacementUnplaced || record.Generation != generation {
			return errors.New("branch is already placed or generation is stale")
		}
		record.OwnerNode = owner
		record.OwnerIncarnation = incarnation
		record.PhysicalContainer = physicalContainer
		record.PhysicalBranch = physicalBranch
		record.State = state
		record.Generation++
		record.Revision++
		record.UpdatedAt = now
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateStableState changes running/paused lifecycle while invalidating routed
// commands that observed the previous state. Ownership itself is unchanged.
func (s *PlacementStore) UpdateStableState(family, branch, owner, incarnation string, generation uint64, state PlacementState, now time.Time) (*PlacementRecord, error) {
	if state != PlacementRunning && state != PlacementPaused {
		return nil, fmt.Errorf("invalid stable placement state %q", state)
	}
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.State == state && record.OwnerNode == owner && record.OwnerIncarnation == incarnation {
			result = record
			return nil
		}
		if record.Generation != generation || record.OwnerNode != owner || record.OwnerIncarnation != incarnation ||
			(record.State != PlacementRunning && record.State != PlacementPaused && record.State != PlacementMaterializing) {
			return errors.New("stale or non-stable placement cannot change lifecycle")
		}
		record.State = state
		record.Generation++
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// SetQuarantine changes externally visible network policy without replacing
// execution authority. A concurrent migration wins or loses as one catalog
// operation, so policy cannot be changed on a stale physical incarnation.
func (s *PlacementStore) SetQuarantine(family, branch, owner, incarnation string, generation uint64, quarantined bool, now time.Time) (*PlacementRecord, error) {
	var result PlacementRecord
	err := s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if record.Generation != generation || record.OwnerNode != owner || record.OwnerIncarnation != incarnation ||
			(record.State != PlacementRunning && record.State != PlacementPaused) {
			return errors.New("stale or non-stable placement cannot change quarantine policy")
		}
		if record.Quarantined == quarantined {
			result = record
			return nil
		}
		record.Quarantined = quarantined
		record.Revision++
		record.UpdatedAt = now
		if err := record.validate(); err != nil {
			return err
		}
		if err := s.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Rename moves a logical placement key without changing the runtime it owns.
// The catalog-wide lock makes absence of the destination and removal of the
// source one atomic filesystem transaction for this legacy implementation.
func (s *PlacementStore) Rename(family, branch, newBranch, owner, incarnation, physicalBranch string, generation uint64, now time.Time) (*PlacementRecord, error) {
	if branch == newBranch {
		return s.Lookup(family, branch)
	}
	var result PlacementRecord
	err := s.withCatalogLock(true, func() error {
		record, err := s.load(family, branch)
		if err != nil {
			return err
		}
		if _, err := s.load(family, newBranch); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if record.Generation != generation || record.State == PlacementMigrating || record.State == PlacementMaterializing || record.State == PlacementCorrupted {
			return errors.New("stale or unstable placement cannot be renamed")
		}
		if record.State == PlacementRunning || record.State == PlacementPaused {
			if record.OwnerNode != owner || record.OwnerIncarnation != incarnation {
				return errors.New("rename does not match the physical placement owner")
			}
			record.PhysicalBranch = physicalBranch
		} else if record.State != PlacementUnplaced {
			return fmt.Errorf("branch is %s, not renameable", record.State)
		}
		oldKey, err := placementKey(family, branch)
		if err != nil {
			return err
		}
		record.BranchID = newBranch
		record.Generation++
		record.Revision++
		record.UpdatedAt = now
		if err := s.store(record); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, oldKey+".json")); err != nil {
			return err
		}
		result = record
		return syncDirectory(s.dir)
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Remove reclaims a placement whose logical branch no longer exists. A
// migration record is never removable: its operation must first commit or
// abort so cleanup cannot erase a live ownership decision.
func (s *PlacementStore) Remove(family, branch string, generation uint64) error {
	return s.withRecordLock(family, branch, func() error {
		record, err := s.load(family, branch)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if record.Generation != generation || record.State == PlacementMigrating {
			return errors.New("stale or migrating placement cannot be removed")
		}
		key, err := placementKey(family, branch)
		if err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(s.dir)
	})
}

func (s *PlacementStore) Lookup(family, branch string) (*PlacementRecord, error) {
	var record PlacementRecord
	err := s.withRecordReadLock(family, branch, func() error {
		var err error
		record, err = s.load(family, branch)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *PlacementStore) List() ([]PlacementRecord, error) {
	var result []PlacementRecord
	err := s.withCatalogLock(true, func() error {
		var err error
		result, err = s.listUnlocked()
		return err
	})
	return result, err
}

func (s *PlacementStore) ListFamily(family string) ([]PlacementRecord, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	var result []PlacementRecord
	for _, record := range records {
		if record.FamilyID == family {
			result = append(result, record)
		}
	}
	return result, nil
}

func (s *PlacementStore) listUnlocked() ([]PlacementRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	result := make([]PlacementRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		record, err := decodePlacement(data)
		if err != nil {
			return nil, err
		}
		key, _ := placementKey(record.FamilyID, record.BranchID)
		if entry.Name() != key+".json" {
			return nil, fmt.Errorf("placement record %q has mismatched identity", entry.Name())
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FamilyID != result[j].FamilyID {
			return result[i].FamilyID < result[j].FamilyID
		}
		return result[i].BranchID < result[j].BranchID
	})
	return result, nil
}

func (s *PlacementStore) load(family, branch string) (PlacementRecord, error) {
	key, err := placementKey(family, branch)
	if err != nil {
		return PlacementRecord{}, err
	}
	file, err := os.Open(filepath.Join(s.dir, key+".json"))
	if err != nil {
		return PlacementRecord{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlacementRecordBytes+1))
	if err != nil {
		return PlacementRecord{}, err
	}
	return decodePlacement(data)
}

func decodePlacement(data []byte) (PlacementRecord, error) {
	if len(data) > maxPlacementRecordBytes {
		return PlacementRecord{}, errors.New("placement record exceeds size limit")
	}
	var record PlacementRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return PlacementRecord{}, err
	}
	if err := record.validate(); err != nil {
		return PlacementRecord{}, err
	}
	return record, nil
}

func (s *PlacementStore) store(record PlacementRecord) error {
	if err := record.validate(); err != nil {
		return err
	}
	key, err := placementKey(record.FamilyID, record.BranchID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(&record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".placement-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(s.dir, key+".json")); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}
