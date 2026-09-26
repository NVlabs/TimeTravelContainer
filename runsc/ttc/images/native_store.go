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

package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	contentlocal "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const nativeCatalogVersion = 1

type imageRecord struct {
	Info   Info               `json:"info"`
	Target ocispec.Descriptor `json:"target"`
}

type leaseRecord struct {
	Digest string             `json:"digest"`
	Target ocispec.Descriptor `json:"target"`
}

type nativeCatalog struct {
	Version int                    `json:"version"`
	Images  map[string]imageRecord `json:"images"`
	Leases  map[string]leaseRecord `json:"leases"`
}

type nativeStore struct {
	root    string
	content content.Store
	lock    *os.File
	catalog nativeCatalog
}

func openNativeStore(root string) (*nativeStore, error) {
	if root == "" {
		return nil, errors.New("native image store requires a data root")
	}
	storeRoot := filepath.Join(root, "image-store")
	if err := os.MkdirAll(storeRoot, 0700); err != nil {
		return nil, fmt.Errorf("creating native image store: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(storeRoot, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening native image store lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, fmt.Errorf("locking native image store: %w", err)
	}
	contentRoot := filepath.Join(storeRoot, "content")
	if err := os.MkdirAll(filepath.Join(contentRoot, "blobs"), 0700); err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return nil, fmt.Errorf("creating native content store: %w", err)
	}
	contentStore, err := contentlocal.NewStore(contentRoot)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return nil, fmt.Errorf("opening native content store: %w", err)
	}
	store := &nativeStore{root: storeRoot, content: contentStore, lock: lock}
	if err := store.loadCatalog(); err != nil {
		store.close()
		return nil, err
	}
	return store, nil
}

func (s *nativeStore) close() {
	if s.lock == nil {
		return
	}
	// containerd's local content writer removes its ingest directory before
	// releasing every descriptor used by that write. On NFS this may be
	// observed as a failed rmdir even though the directory becomes empty as the
	// writer closes. Prune only empty, direct ingest children at the outer store
	// lifetime boundary; never hide a nonempty or malformed transaction.
	_ = pruneEmptyNativeIngestDirectories(filepath.Join(s.root, "content", "ingest"))
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	_ = s.lock.Close()
	s.lock = nil
}

func pruneEmptyNativeIngestDirectories(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		children, err := os.ReadDir(path)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if len(children) != 0 {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func newNativeCatalog() nativeCatalog {
	return nativeCatalog{
		Version: nativeCatalogVersion,
		Images:  make(map[string]imageRecord),
		Leases:  make(map[string]leaseRecord),
	}
}

func (s *nativeStore) loadCatalog() error {
	data, err := os.ReadFile(filepath.Join(s.root, "catalog.json"))
	if os.IsNotExist(err) {
		s.catalog = newNativeCatalog()
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading native image catalog: %w", err)
	}
	var value nativeCatalog
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decoding native image catalog: %w", err)
	}
	if value.Version != nativeCatalogVersion {
		return fmt.Errorf("native image catalog version %d is unsupported (want %d)", value.Version, nativeCatalogVersion)
	}
	if value.Images == nil {
		value.Images = make(map[string]imageRecord)
	}
	if value.Leases == nil {
		value.Leases = make(map[string]leaseRecord)
	}
	s.catalog = value
	return nil
}

func (s *nativeStore) saveCatalog() error {
	data, err := json.MarshalIndent(&s.catalog, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(s.root, ".catalog.tmp-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
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
	if err := os.Rename(tempName, filepath.Join(s.root, "catalog.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *nativeStore) garbageCollect(ctx context.Context) error {
	reachable := make(map[string]bool)
	var mark func(ocispec.Descriptor) error
	mark = func(desc ocispec.Descriptor) error {
		key := desc.Digest.String()
		if key == "" || reachable[key] {
			return nil
		}
		if _, err := s.content.Info(ctx, desc.Digest); err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		}
		reachable[key] = true
		children, err := containerdimages.Children(ctx, s.content, desc)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := mark(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, image := range s.catalog.Images {
		if err := mark(image.Target); err != nil {
			return fmt.Errorf("marking image %q content: %w", image.Info.Reference, err)
		}
	}
	for id, lease := range s.catalog.Leases {
		if err := mark(lease.Target); err != nil {
			return fmt.Errorf("marking lease %q content: %w", id, err)
		}
	}
	var garbage []ocispec.Descriptor
	if err := s.content.Walk(ctx, func(info content.Info) error {
		if !reachable[info.Digest.String()] {
			garbage = append(garbage, ocispec.Descriptor{Digest: info.Digest})
		}
		return nil
	}); err != nil {
		return fmt.Errorf("enumerating native content: %w", err)
	}
	for _, value := range garbage {
		if err := s.content.Delete(ctx, value.Digest); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("deleting unreachable content %s: %w", value.Digest, err)
		}
	}
	return nil
}
