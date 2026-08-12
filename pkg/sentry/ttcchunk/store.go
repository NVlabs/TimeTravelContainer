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

package ttcchunk

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store is an immutable content-addressed loose chunk store. The first two ID
// bytes select a shard, avoiding a single huge host directory.
type Store struct{ root string }

// OpenStore opens or creates a private loose chunk store.
func OpenStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("TTC memory chunk store root is empty")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("creating TTC memory chunk store: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) path(id ID) (dir, path string) {
	name := id.String()
	dir = filepath.Join(s.root, name[:4])
	return dir, filepath.Join(dir, name+".chunk")
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Put encodes and durably publishes chunk. Concurrent or repeated publication
// of identical content is idempotent.
func (s *Store) Put(chunk Chunk) (ID, error) {
	data, id, err := Encode(chunk)
	if err != nil {
		return ID{}, err
	}
	if err := s.PutObject(Object{ID: id, Logical: chunk.Logical, Data: data}); err != nil {
		return ID{}, err
	}
	return id, nil
}

// PutObject durably publishes a pre-encoded object returned by Assemble or
// ReadSnapshot. It validates both the content address and logical coordinate.
func (s *Store) PutObject(object Object) error {
	decoded, err := Decode(object.Data, &object.ID)
	if err != nil {
		return err
	}
	if decoded.Logical != object.Logical {
		return fmt.Errorf("TTC memory object %s has logical coordinate %+v, want %+v", object.ID, decoded.Logical, object.Logical)
	}
	data, id := object.Data, object.ID
	dir, path := s.path(id)
	if err := os.Mkdir(dir, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	} else if err := syncDirectory(s.root); err != nil {
		return fmt.Errorf("synchronizing TTC memory chunk root: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".publish-")
	if err != nil {
		return err
	}
	tempName := tmp.Name()
	defer os.Remove(tempName)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Link(tempName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publishing TTC memory chunk: %w", err)
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("verifying existing TTC memory chunk: %w", readErr)
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("existing TTC memory chunk %s does not match its content address", id)
		}
		return nil
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("synchronizing TTC memory chunk shard: %w", err)
	}
	return nil
}

// Get verifies and decodes id.
func (s *Store) Get(id ID) (Chunk, error) {
	_, path := s.path(id)
	file, err := os.Open(path)
	if err != nil {
		return Chunk{}, err
	}
	defer file.Close()
	return Read(file, &id)
}

// Open returns the immutable encoded chunk after verifying its content ID.
func (s *Store) Open(id ID) (*os.File, error) {
	_, path := s.path(id)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := Read(file, &id); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
