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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var branchPointerMagic = [8]byte{'T', 'T', 'C', 'M', 'R', 'O', 'O', 'T'}

// MemoryRepository is one family-level shared memory namespace. Immutable
// chunks and manifests are content-addressed; branch files are atomic mutable
// pointers to manifests. Lease and transaction directories are created now so
// later GC never needs to infer liveness from process-local reference counts.
type MemoryRepository struct {
	mu           sync.Mutex
	root         string
	chunks       *Store
	manifestRoot string
	branchRoot   string
	leaseRoot    string
	txnRoot      string
	trashRoot    string
}

// OpenMemoryRepository opens or creates a family-level repository.
func OpenMemoryRepository(root string) (*MemoryRepository, error) {
	if root == "" {
		return nil, errors.New("TTC memory repository root is empty")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("creating TTC memory repository: %w", err)
	}
	for _, name := range []string{"chunks", "manifests", "branches", "leases", "transactions", "trash"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return nil, fmt.Errorf("creating TTC memory repository %s directory: %w", name, err)
		}
	}
	if err := recoverRepositoryTrash(root, time.Now(), DefaultTransactionTTL); err != nil {
		return nil, fmt.Errorf("recovering interrupted TTC memory collection: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return nil, fmt.Errorf("synchronizing TTC memory repository: %w", err)
	}
	chunks, err := OpenStore(filepath.Join(root, "chunks"))
	if err != nil {
		return nil, err
	}
	return &MemoryRepository{
		root:         root,
		chunks:       chunks,
		manifestRoot: filepath.Join(root, "manifests"),
		branchRoot:   filepath.Join(root, "branches"),
		leaseRoot:    filepath.Join(root, "leases"),
		txnRoot:      filepath.Join(root, "transactions"),
		trashRoot:    filepath.Join(root, "trash"),
	}, nil
}

func (r *MemoryRepository) manifestPath(id ManifestID) (dir, path string) {
	name := id.String()
	dir = filepath.Join(r.manifestRoot, name[:4])
	return dir, filepath.Join(dir, name+".manifest")
}

func publishImmutable(dir, parent, path string, data []byte) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	} else if err := syncDirectory(parent); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".publish-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
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
	if err := os.Link(name, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, data) {
			return errors.New("immutable TTC memory object differs from existing content")
		}
		return nil
	}
	return syncDirectory(dir)
}

// Publish writes referenced chunks before atomically publishing their compact
// immutable manifest. objects may omit already-published objects.
func (r *MemoryRepository) Publish(objects []Object, bindings []StoredBinding) (ManifestID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	manifest, err := CompactBindings(bindings)
	if err != nil {
		return ManifestID{}, err
	}
	manifestData, manifestID, err := EncodeManifest(manifest)
	if err != nil {
		return ManifestID{}, err
	}
	needed := make(map[ID][]StoredBinding, len(bindings))
	for _, binding := range bindings {
		needed[binding.Object] = append(needed[binding.Object], binding)
	}
	objectIDs := make([]ID, 0, len(needed))
	for id := range needed {
		objectIDs = append(objectIDs, id)
	}
	txnPath, err := r.beginTransaction(manifestID, objectIDs)
	if err != nil {
		return ManifestID{}, fmt.Errorf("beginning memory publication: %w", err)
	}
	keepTransaction := false
	defer func() {
		if !keepTransaction {
			_ = os.Remove(txnPath)
		}
	}()
	provided := make(map[ID]Object, len(objects))
	decoded := make(map[ID]Chunk, len(objects))
	for _, object := range objects {
		if _, ok := provided[object.ID]; ok {
			return ManifestID{}, fmt.Errorf("publication repeats memory object %s", object.ID)
		}
		if _, ok := needed[object.ID]; !ok {
			return ManifestID{}, fmt.Errorf("publication includes unreferenced memory object %s", object.ID)
		}
		chunk, err := Decode(object.Data, &object.ID)
		if err != nil {
			return ManifestID{}, fmt.Errorf("validating memory object %s: %w", object.ID, err)
		}
		if chunk.Logical != object.Logical {
			return ManifestID{}, fmt.Errorf("memory object %s has logical coordinate %+v, want %+v", object.ID, chunk.Logical, object.Logical)
		}
		provided[object.ID] = object
		decoded[object.ID] = chunk
	}
	for objectID, objectBindings := range needed {
		chunk, ok := decoded[objectID]
		if !ok {
			var err error
			chunk, err = r.chunks.Get(objectID)
			if err != nil {
				return ManifestID{}, fmt.Errorf("manifest references unavailable memory object %s: %w", objectID, err)
			}
		}
		pages := make(map[uint8]Page, len(chunk.Pages))
		for _, page := range chunk.Pages {
			pages[page.Slot] = page
		}
		for _, binding := range objectBindings {
			page, ok := pages[binding.Binding.Slot]
			if chunk.Logical != binding.Binding.Chunk || !ok || page.Version != binding.Binding.Version {
				return ManifestID{}, fmt.Errorf("memory object %s does not contain requested page version %+v at (%+v,%d)", objectID, binding.Binding.Version, binding.Binding.Chunk, binding.Binding.Slot)
			}
		}
	}
	for _, object := range objects {
		if err := r.chunks.PutObject(object); err != nil {
			return ManifestID{}, fmt.Errorf("publishing memory object %s: %w", object.ID, err)
		}
		if err := os.Chtimes(txnPath, time.Now(), time.Now()); err != nil {
			return ManifestID{}, fmt.Errorf("refreshing memory publication transaction: %w", err)
		}
	}
	dir, path := r.manifestPath(manifestID)
	if err := publishImmutable(dir, r.manifestRoot, path, manifestData); err != nil {
		return ManifestID{}, fmt.Errorf("publishing memory manifest %s: %w", manifestID, err)
	}
	keepTransaction = true
	return manifestID, nil
}

// LoadManifest verifies and loads an immutable manifest.
func (r *MemoryRepository) LoadManifest(id ManifestID) (Manifest, error) {
	_, path := r.manifestPath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return DecodeManifest(data, &id)
}

func (r *MemoryRepository) branchPath(branch string) (string, error) {
	if branch == "" || len(branch) > 4096 {
		return "", fmt.Errorf("invalid TTC memory branch ID %q", branch)
	}
	return filepath.Join(r.branchRoot, hex.EncodeToString([]byte(branch))+".root"), nil
}

func encodeBranchPointer(id ManifestID) []byte {
	data := append([]byte(nil), branchPointerMagic[:]...)
	data = append(data, id[:]...)
	checksum := sha256.Sum256(data)
	return append(data, checksum[:]...)
}

func decodeBranchPointer(data []byte) (ManifestID, error) {
	var id ManifestID
	if len(data) != len(branchPointerMagic)+len(id)+sha256.Size || !bytes.Equal(data[:len(branchPointerMagic)], branchPointerMagic[:]) {
		return id, errors.New("invalid TTC memory branch pointer")
	}
	want := sha256.Sum256(data[:len(branchPointerMagic)+len(id)])
	if !bytes.Equal(data[len(branchPointerMagic)+len(id):], want[:]) {
		return id, errors.New("TTC memory branch pointer checksum mismatch")
	}
	copy(id[:], data[len(branchPointerMagic):len(branchPointerMagic)+len(id)])
	return id, nil
}

// SetBranch atomically points branch at an already-published manifest.
func (r *MemoryRepository) SetBranch(branch string, id ManifestID) (retErr error) {
	lease, err := r.AcquireLease(id, DefaultLeaseTTL)
	if err != nil {
		return fmt.Errorf("protecting branch manifest %s: %w", id, err)
	}
	defer func() {
		if err := lease.Release(); err != nil && retErr == nil {
			retErr = fmt.Errorf("releasing branch publication lease: %w", err)
		}
	}()
	path, err := r.branchPath(branch)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tmp, err := os.CreateTemp(r.branchRoot, ".branch-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(encodeBranchPointer(id)); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if err := syncDirectory(r.branchRoot); err != nil {
		return err
	}
	return r.removeTransactionsFor(id)
}

// Branch returns the manifest currently named by branch.
func (r *MemoryRepository) Branch(branch string) (ManifestID, error) {
	path, err := r.branchPath(branch)
	if err != nil {
		return ManifestID{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ManifestID{}, err
	}
	id, err := decodeBranchPointer(data)
	if err != nil {
		return ManifestID{}, err
	}
	if _, err := r.LoadManifest(id); err != nil {
		return ManifestID{}, fmt.Errorf("branch %q references unavailable manifest %s: %w", branch, id, err)
	}
	return id, nil
}

// DeleteBranch durably removes a branch root. Immutable reclamation is a
// separate manifest/lease tracing operation.
func (r *MemoryRepository) DeleteBranch(branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	path, err := r.branchPath(branch)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(r.branchRoot)
}
