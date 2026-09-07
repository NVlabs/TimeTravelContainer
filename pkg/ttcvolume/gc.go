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

package ttcvolume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const metadataCompactionLayerThreshold = 64

// GarbageCollect removes metadata layers and data objects that are unreachable
// from every published branch pointer. Repository mutation and collection use
// the same cross-process flock, so a concurrent Sentry fork or write either
// precedes the reachability scan or follows all removals.
//
// GarbageCollect requires a path-backed Repository opened by Manager. Donated
// Sentry repositories intentionally omit their host path and only unlink
// branch pointers; ttcd performs collection after the Kernel is gone.
func (r *Repository) GarbageCollect() error {
	if r == nil || r.path == "" {
		return errors.New("garbage collection requires a path-backed repository")
	}
	return r.withLock(r.garbageCollectLocked)
}

func (r *Repository) garbageCollectLocked() error {
	branchDir := filepath.Join(r.path, "branches")
	entries, err := os.ReadDir(branchDir)
	if err != nil {
		return err
	}
	var branches []Branch
	liveBranchIDs := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err := validateName(id, "branch"); err != nil {
			return fmt.Errorf("invalid branch pointer %q during garbage collection: %w", entry.Name(), err)
		}
		branch, err := r.readBranch(id)
		if err != nil {
			return err
		}
		branches = append(branches, branch)
		liveBranchIDs[id] = struct{}{}
	}
	if len(branches) == 0 {
		return errors.New("repository has no published branches")
	}
	for i := range branches {
		layers, err := r.metadataLayers(branches[i].Root)
		if err != nil {
			return fmt.Errorf("walking branch %q metadata layers: %w", branches[i].ID, err)
		}
		if len(layers) > metadataCompactionLayerThreshold {
			compacted, err := r.compactBranchMetadataLocked(branches[i])
			if err != nil {
				return fmt.Errorf("compacting branch %q metadata: %w", branches[i].ID, err)
			}
			branches[i] = compacted
		}
	}

	reachableLayers := make(map[string]struct{})
	reachableObjects := make(map[string]struct{})
	for _, branch := range branches {
		layers, err := r.metadataLayers(branch.Root)
		if err != nil {
			return fmt.Errorf("walking branch %q metadata layers: %w", branch.ID, err)
		}
		for _, layer := range layers {
			reachableLayers[layer] = struct{}{}
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return fmt.Errorf("resolving branch %q metadata: %w", branch.ID, err)
		}
		state.mu.RLock()
		for _, inode := range state.inodes {
			if inode.Object != "" {
				reachableObjects[inode.Object] = struct{}{}
			}
		}
		state.mu.RUnlock()
	}

	// Drop decoded state for removed branch IDs before their layers disappear.
	r.metadataMu.Lock()
	for id := range r.metadata {
		if _, ok := liveBranchIDs[id]; !ok {
			delete(r.metadata, id)
		}
	}
	r.metadataMu.Unlock()

	layerEntries, err := os.ReadDir(filepath.Join(r.path, "layers"))
	if err != nil {
		return err
	}
	for _, entry := range layerEntries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		if _, ok := reachableLayers[entry.Name()]; ok {
			// Format 2 briefly wrote per-inode and per-dentry lookup caches.
			// Journals have always been authoritative, so these directories are
			// safe to reclaim when a new runtime next collects the repository.
			for _, obsolete := range []string{"inodes", "dirents"} {
				if err := os.RemoveAll(filepath.Join(r.path, "layers", entry.Name(), obsolete)); err != nil {
					return err
				}
			}
			continue
		}
		r.journalMu.Lock()
		if journal := r.journals[entry.Name()]; journal != nil {
			if err := journal.Close(); err != nil {
				r.journalMu.Unlock()
				return err
			}
			delete(r.journals, entry.Name())
		}
		r.journalMu.Unlock()
		if err := os.RemoveAll(filepath.Join(r.path, "layers", entry.Name())); err != nil {
			return err
		}
	}

	objectsRoot := filepath.Join(r.path, "objects")
	err = filepath.WalkDir(objectsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == objectsRoot {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink in repository object store: %s", path)
		}
		id := entry.Name()
		if !validID(id) {
			return fmt.Errorf("invalid object name %q during garbage collection", id)
		}
		if _, ok := reachableObjects[id]; ok {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return r.clearRemoteObjectPending(id)
	})
	if err != nil {
		return err
	}

	if err := syncHostDirectory(filepath.Join(r.path, "layers")); err != nil {
		return err
	}
	if err := syncHostDirectory(objectsRoot); err != nil {
		return err
	}
	return nil
}

func (r *Repository) compactBranchMetadataLocked(branch Branch) (Branch, error) {
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return Branch{}, err
	}
	state.mu.RLock()
	inodeNumbers := make([]uint64, 0, len(state.inodes))
	for number := range state.inodes {
		inodeNumbers = append(inodeNumbers, number)
	}
	sort.Slice(inodeNumbers, func(i, j int) bool { return inodeNumbers[i] < inodeNumbers[j] })
	entryKeys := make([]metadataEntryKey, 0, len(state.entries))
	for key := range state.entries {
		entryKeys = append(entryKeys, key)
	}
	sort.Slice(entryKeys, func(i, j int) bool {
		if entryKeys[i].Parent != entryKeys[j].Parent {
			return entryKeys[i].Parent < entryKeys[j].Parent
		}
		return entryKeys[i].Name < entryKeys[j].Name
	})
	ops := make([]metadataOp, 0, len(inodeNumbers)+len(entryKeys))
	for _, number := range inodeNumbers {
		inode := state.inodes[number]
		ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &inode})
	}
	for _, key := range entryKeys {
		entry := state.entries[key]
		ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: key.Parent, Entry: &entry})
	}
	state.mu.RUnlock()

	root, err := r.createMetadataLayer("")
	if err != nil {
		return Branch{}, err
	}
	compacted := branch
	compacted.Root = root
	compactedState := newBranchMetadata(compacted)
	compactedState.layers = []string{root}
	const maxOpsPerTransaction = 1000
	for len(ops) != 0 {
		count := len(ops)
		if count > maxOpsPerTransaction {
			count = maxOpsPerTransaction
		}
		if err := r.commitMetadata(&compacted, compactedState, ops[:count]); err != nil {
			return Branch{}, err
		}
		ops = ops[count:]
	}
	if err := r.syncRepositoryFile(filepath.Join("layers", root, "layer.json")); err != nil {
		return Branch{}, err
	}
	if err := r.syncRepositoryFile(filepath.Join("layers", root, "journal")); err != nil {
		return Branch{}, err
	}
	if err := r.syncDirectory(filepath.Join("layers", root)); err != nil {
		return Branch{}, err
	}
	if err := r.syncDirectory("layers"); err != nil {
		return Branch{}, err
	}
	if err := r.writeBranch(compacted); err != nil {
		return Branch{}, err
	}
	if err := r.syncRepositoryFile(filepath.Join("branches", compacted.ID+".json")); err != nil {
		return Branch{}, err
	}
	if err := r.syncDirectory("branches"); err != nil {
		return Branch{}, err
	}
	r.metadataMu.Lock()
	if r.metadata == nil {
		r.metadata = make(map[string]*branchMetadata)
	}
	r.metadata[compacted.ID] = compactedState
	r.metadataMu.Unlock()
	return compacted, nil
}

func (r *Repository) syncRepositoryFile(name string) error {
	file, err := r.openFile(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncHostDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}
