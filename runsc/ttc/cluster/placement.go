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
	PlacementRunning   PlacementState = "running"
	PlacementPaused    PlacementState = "paused"
	PlacementMigrating PlacementState = "migrating"
	PlacementUnplaced  PlacementState = "unplaced"
)

// PlacementRecord maps a stable logical branch to its current physical
// runtime. Generation is the fencing token presented by routed commands.
type PlacementRecord struct {
	FamilyID          string         `json:"family_id"`
	BranchID          string         `json:"branch_id"`
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
	case PlacementRunning, PlacementPaused:
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
	case PlacementUnplaced:
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

func (s *PlacementStore) withRecordLock(family, branch string, fn func() error) error {
	key, err := placementKey(family, branch)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(s.dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
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
	if state != PlacementRunning && state != PlacementPaused {
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

func (s *PlacementStore) Lookup(family, branch string) (*PlacementRecord, error) {
	record, err := s.load(family, branch)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *PlacementStore) List() ([]PlacementRecord, error) {
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
