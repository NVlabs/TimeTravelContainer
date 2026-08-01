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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const maxTransactionRecordBytes = 1 << 20

// Store durably records distributed-fork transactions on one ttcd node.
// Publication is atomic and every update uses optimistic revision checking.
type Store struct {
	dir string
	mu  sync.Mutex
}

// OpenStore opens or creates a private transaction directory.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("cluster transaction directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating cluster transaction directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("cluster transaction directory %q is not a private directory", dir)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if !transactionIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid transaction ID %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Create atomically installs the initial transaction without replacing an
// existing transaction of the same ID.
func (s *Store) Create(tx *Transaction) error {
	if tx == nil || tx.State != TransactionPreparing || tx.Revision != 1 {
		return errors.New("cluster store create requires a new preparing transaction")
	}
	if err := tx.Identity.Validate(); err != nil {
		return err
	}
	path, err := s.path(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := marshalTransaction(tx)
	if err != nil {
		return err
	}
	temp, err := s.writeTemp(data)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := os.Link(temp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("transaction %q already exists", tx.Identity.TransactionID)
		}
		return fmt.Errorf("publishing transaction: %w", err)
	}
	return syncDirectory(s.dir)
}

// Load reads and validates one transaction record.
func (s *Store) Load(id string) (*Transaction, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadTransaction(path, id)
}

// Advance loads revision, applies one state transition, and atomically
// replaces the record. A revision mismatch makes retries explicit.
func (s *Store) Advance(id string, revision uint64, next TransactionState, failure string, now time.Time) (*Transaction, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := loadTransaction(path, id)
	if err != nil {
		return nil, err
	}
	if tx.State == next {
		if next == TransactionFailed && tx.Failure != failure {
			return nil, errors.New("idempotent failed transition changed its diagnostic")
		}
		return tx, nil
	}
	if tx.Revision != revision {
		return nil, fmt.Errorf("transaction %q revision is %d, not %d", id, tx.Revision, revision)
	}
	if err := tx.Advance(next, failure, now); err != nil {
		return nil, err
	}
	data, err := marshalTransaction(tx)
	if err != nil {
		return nil, err
	}
	if err := s.replace(path, data); err != nil {
		return nil, err
	}
	return tx, nil
}

// BindSession atomically records the data-plane descriptor digest and advances
// a reserved target to source-captured. The secret descriptor itself is never
// written to the transaction store.
func (s *Store) BindSession(id string, revision uint64, digest string, now time.Time) (*Transaction, error) {
	if err := validateSHA256Digest(digest); err != nil {
		return nil, fmt.Errorf("session digest: %w", err)
	}
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := loadTransaction(path, id)
	if err != nil {
		return nil, err
	}
	if tx.SessionDigest != "" {
		if tx.SessionDigest != digest {
			return nil, fmt.Errorf("transaction %q is bound to another data-plane session", id)
		}
		if tx.State == TransactionSourceCaptured {
			return tx, nil
		}
		return nil, fmt.Errorf("transaction %q already passed session binding in state %s", id, tx.State)
	}
	switch tx.State {
	case TransactionSourceCaptured, TransactionTargetRestoring, TransactionTargetPaused,
		TransactionPublished, TransactionDraining, TransactionIndependent:
		if tx.SessionDigest == "" {
			return nil, fmt.Errorf("transaction %q in state %s has no session digest", id, tx.State)
		}
	}
	if tx.Revision != revision {
		return nil, fmt.Errorf("transaction %q revision is %d, not %d", id, tx.Revision, revision)
	}
	if tx.State != TransactionTargetReserved {
		return nil, fmt.Errorf("transaction %q is %s, not target-reserved", id, tx.State)
	}
	tx.SessionDigest = digest
	if err := tx.Advance(TransactionSourceCaptured, "", now); err != nil {
		return nil, err
	}
	data, err := marshalTransaction(tx)
	if err != nil {
		return nil, err
	}
	if err := s.replace(path, data); err != nil {
		return nil, err
	}
	return tx, nil
}

// List returns all valid records ordered by transaction ID.
func (s *Store) List() ([]Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	transactions := make([]Transaction, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		tx, err := loadTransaction(filepath.Join(s.dir, name), id)
		if err != nil {
			return nil, err
		}
		transactions = append(transactions, *tx)
	}
	sort.Slice(transactions, func(i, j int) bool {
		return transactions[i].Identity.TransactionID < transactions[j].Identity.TransactionID
	})
	return transactions, nil
}

// Delete removes a terminal record. Runtime resources must be cleaned before
// this call; deleting a nonterminal dependency is rejected.
func (s *Store) Delete(id string, revision uint64) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := loadTransaction(path, id)
	if err != nil {
		return err
	}
	if tx.Revision != revision {
		return fmt.Errorf("transaction %q revision is %d, not %d", id, tx.Revision, revision)
	}
	if !tx.Terminal() {
		return fmt.Errorf("transaction %q in state %s is not terminal", id, tx.State)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *Store) writeTemp(data []byte) (string, error) {
	file, err := os.CreateTemp(s.dir, ".transaction-*.tmp")
	if err != nil {
		return "", err
	}
	name := file.Name()
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func (s *Store) replace(path string, data []byte) error {
	temp, err := s.writeTemp(data)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("replacing transaction: %w", err)
	}
	return syncDirectory(s.dir)
}

func marshalTransaction(tx *Transaction) ([]byte, error) {
	data, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > maxTransactionRecordBytes {
		return nil, errors.New("transaction record exceeds size limit")
	}
	return data, nil
}

func loadTransaction(path, id string) (*Transaction, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTransactionRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTransactionRecordBytes {
		return nil, fmt.Errorf("transaction %q exceeds size limit", id)
	}
	var tx Transaction
	if err := json.Unmarshal(data, &tx); err != nil {
		return nil, fmt.Errorf("decoding transaction %q: %w", id, err)
	}
	if tx.Identity.TransactionID != id || tx.Revision == 0 || tx.CreatedAt.IsZero() || tx.UpdatedAt.Before(tx.CreatedAt) {
		return nil, fmt.Errorf("transaction %q has inconsistent identity, revision, or timestamps", id)
	}
	if err := tx.Identity.Validate(); err != nil {
		return nil, fmt.Errorf("transaction %q identity: %w", id, err)
	}
	if tx.SessionDigest != "" {
		if err := validateSHA256Digest(tx.SessionDigest); err != nil {
			return nil, fmt.Errorf("transaction %q session digest: %w", id, err)
		}
	}
	if _, known := transactionTransitions[tx.State]; !known && !tx.Terminal() {
		return nil, fmt.Errorf("transaction %q has unknown state %q", id, tx.State)
	}
	return &tx, nil
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
