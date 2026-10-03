// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	// A content store makes its ingest directory once, so a root made again
	// under a store kept for it, which its inode may reuse, needs it made here.
	for _, dir := range []string{"blobs", "ingest"} {
		if err := os.MkdirAll(filepath.Join(contentRoot, dir), 0700); err != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			lock.Close()
			return nil, fmt.Errorf("creating native content store: %w", err)
		}
	}
	contentStore, err := openContentStore(contentRoot)
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

// contentStores keeps the content store of each content root the process has
// opened. containerd's NewStore probes the root's file system for fs-verity
// each time, making and removing a directory and a file in it: nine requests
// of a shared root over NFS, four times in a cluster's run. A store holds no
// more than its root, the probe's answer, and the locks of the ingests it is
// writing, which the root's users share as they share the image store's lock.
var contentStores struct {
	mu     sync.Mutex
	byRoot map[string]contentStoreOf
}

// contentStoreOf is the content store of the directory info describes.
type contentStoreOf struct {
	info  os.FileInfo
	store content.Store
}

// newContentStore makes a content store of a root.
var newContentStore = contentlocal.NewStore

// openContentStore returns the content store of root, kept from an earlier
// open of the same directory.
func openContentStore(root string) (content.Store, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	contentStores.mu.Lock()
	defer contentStores.mu.Unlock()
	if kept, ok := contentStores.byRoot[root]; ok && os.SameFile(kept.info, info) {
		return kept.store, nil
	}
	store, err := newContentStore(root)
	if err != nil {
		return nil, err
	}
	if contentStores.byRoot == nil {
		contentStores.byRoot = make(map[string]contentStoreOf)
	}
	contentStores.byRoot[root] = contentStoreOf{info: info, store: store}
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

// keeps reports whether an image or lease of the catalog has target as its
// target. What a target reaches is a function of it, so a root removed whose
// target another keeps leaves no content unreachable: a family removed while
// its image is still tagged, the common case, collects nothing, and need not
// read every image's manifests to find that out.
func (s *nativeStore) keeps(target ocispec.Descriptor) bool {
	if target.Digest == "" {
		return false
	}
	same := func(other ocispec.Descriptor) bool {
		return other.Digest == target.Digest && other.MediaType == target.MediaType
	}
	for _, image := range s.catalog.Images {
		if same(image.Target) {
			return true
		}
	}
	for _, lease := range s.catalog.Leases {
		if same(lease.Target) {
			return true
		}
	}
	return false
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
