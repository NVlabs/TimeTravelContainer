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

package ttcvolume

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const snapshotFormatVersion = 2

// SnapshotInfo describes one self-contained repository branch snapshot.
type SnapshotInfo struct {
	FormatVersion int       `json:"format_version"`
	RepositoryID  string    `json:"repository_id"`
	BranchID      string    `json:"branch_id"`
	Root          string    `json:"root"`
	NextInode     uint64    `json:"next_inode"`
	CreatedAt     time.Time `json:"created_at"`
}

type snapshotMetadata struct {
	Inodes  []Inode                  `json:"inodes"`
	Entries []snapshotDirectoryEntry `json:"entries"`
}

type snapshotDirectoryEntry struct {
	Parent uint64 `json:"parent"`
	DirectoryEntry
}

// ExportSnapshot writes the immutable metadata and regular-file objects
// reachable from branch into a new directory. It never serializes unrelated
// branches.
func (r *Repository) ExportSnapshot(branchID, destination string) (SnapshotInfo, error) {
	if r == nil || r.path == "" {
		return SnapshotInfo{}, errors.New("snapshot export requires a manager-side repository")
	}
	branch, err := r.Branch(branchID)
	if err != nil {
		return SnapshotInfo{}, err
	}
	info := r.Info()
	snapshot := SnapshotInfo{
		FormatVersion: snapshotFormatVersion,
		RepositoryID:  info.ID,
		BranchID:      branch.ID,
		Root:          branch.Root,
		NextInode:     info.NextInode,
		CreatedAt:     time.Now().UTC(),
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return SnapshotInfo{}, fmt.Errorf("creating snapshot parent directory: %w", err)
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return SnapshotInfo{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	if err := os.Mkdir(filepath.Join(destination, "objects"), 0700); err != nil {
		return SnapshotInfo{}, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return SnapshotInfo{}, err
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	metadata := snapshotMetadata{
		Inodes:  make([]Inode, 0, len(state.inodes)),
		Entries: make([]snapshotDirectoryEntry, 0, len(state.entries)),
	}
	objects := make(map[string]struct{})
	for _, inode := range state.inodes {
		metadata.Inodes = append(metadata.Inodes, inode)
		if inode.Object != "" {
			if !validID(inode.Object) {
				return SnapshotInfo{}, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
			}
			objects[inode.Object] = struct{}{}
		}
	}
	for key, entry := range state.entries {
		metadata.Entries = append(metadata.Entries, snapshotDirectoryEntry{Parent: key.Parent, DirectoryEntry: entry})
	}
	sort.Slice(metadata.Inodes, func(i, j int) bool { return metadata.Inodes[i].Number < metadata.Inodes[j].Number })
	sort.Slice(metadata.Entries, func(i, j int) bool {
		if metadata.Entries[i].Parent != metadata.Entries[j].Parent {
			return metadata.Entries[i].Parent < metadata.Entries[j].Parent
		}
		return metadata.Entries[i].Name < metadata.Entries[j].Name
	})
	if err := writeJSONAtomic(destination, filepath.Join(destination, "metadata.json"), metadata); err != nil {
		return SnapshotInfo{}, err
	}

	objectIDs := make([]string, 0, len(objects))
	for id := range objects {
		objectIDs = append(objectIDs, id)
	}
	sort.Strings(objectIDs)
	if err := r.exportSnapshotObjects(objectIDs, filepath.Join(destination, "objects")); err != nil {
		return SnapshotInfo{}, err
	}
	if err := writeJSONAtomic(destination, filepath.Join(destination, "snapshot.json"), snapshot); err != nil {
		return SnapshotInfo{}, err
	}
	keep = true
	return snapshot, nil
}

// exportSnapshotObjects stages immutable branch objects concurrently. The
// exported directory is an intermediate representation: checkpoint.Pack()
// fsyncs the atomically published archive, so flushing every individual APFS
// clone here adds no durability and makes large package trees prohibitively
// slow. Importing a snapshot still requests durable object copies below.
func (r *Repository) exportSnapshotObjects(objectIDs []string, destination string) error {
	workers := min(runtime.GOMAXPROCS(0), 16)
	if workers > len(objectIDs) {
		workers = len(objectIDs)
	}
	if workers == 0 {
		return nil
	}

	var next atomic.Uint64
	var failed atomic.Bool
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for !failed.Load() {
				index := int(next.Add(1) - 1)
				if index >= len(objectIDs) {
					return
				}
				id := objectIDs[index]
				source := filepath.Join(r.path, r.objectPath(id))
				if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
					source = filepath.Join(r.path, "objects", id)
				}
				if err := snapshotCopyFile(source, filepath.Join(destination, id), r.copyMode == CopyModeReflink, false); err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("copying object %q: %w", id, err)
						failed.Store(true)
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// ImportPrivateSnapshot creates a private repository and installs snapshot as
// its main branch. The new repository has a new identity; RestoreID reconnects
// checkpoint state to it.
func (m *Manager) ImportPrivateSnapshot(path, name, source string) (*Repository, SnapshotInfo, error) {
	repository, err := m.CreatePrivate(path, name)
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	if snapshot, err := repository.importSnapshot(source); err != nil {
		repository.Close()
		_ = os.RemoveAll(path)
		return nil, SnapshotInfo{}, err
	} else {
		return repository, snapshot, nil
	}
}

// ImportSnapshot creates a catalogued managed volume from snapshot.
func (m *Manager) ImportSnapshot(name, source string) (*Repository, SnapshotInfo, error) {
	repository, err := m.Create(name)
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	path := repository.path
	if snapshot, err := repository.importSnapshot(source); err != nil {
		repository.Close()
		_ = os.RemoveAll(path)
		return nil, SnapshotInfo{}, err
	} else {
		return repository, snapshot, nil
	}
}

func (r *Repository) importSnapshot(source string) (SnapshotInfo, error) {
	if err := requireSnapshotDirectory(source); err != nil {
		return SnapshotInfo{}, err
	}
	snapshotPath := filepath.Join(source, "snapshot.json")
	if err := requireSnapshotRegularFile(snapshotPath); err != nil {
		return SnapshotInfo{}, err
	}
	var snapshot SnapshotInfo
	if err := readJSON(snapshotPath, &snapshot); err != nil {
		return SnapshotInfo{}, err
	}
	if snapshot.FormatVersion != snapshotFormatVersion || !validID(snapshot.RepositoryID) || !validID(snapshot.Root) || snapshot.NextInode <= rootInode {
		return SnapshotInfo{}, fmt.Errorf("invalid TTC volume snapshot metadata %+v", snapshot)
	}
	metadataPath := filepath.Join(source, "metadata.json")
	if err := requireSnapshotRegularFile(metadataPath); err != nil {
		return SnapshotInfo{}, err
	}
	var metadata snapshotMetadata
	if err := readJSON(metadataPath, &metadata); err != nil {
		return SnapshotInfo{}, err
	}
	inodes := make(map[uint64]Inode, len(metadata.Inodes))
	objects := make(map[string]struct{})
	for _, inode := range metadata.Inodes {
		if inode.Number == 0 || inode.Type == "" {
			return SnapshotInfo{}, fmt.Errorf("invalid snapshot inode %+v", inode)
		}
		if _, exists := inodes[inode.Number]; exists {
			return SnapshotInfo{}, fmt.Errorf("duplicate snapshot inode %d", inode.Number)
		}
		inodes[inode.Number] = inode
		if inode.Object != "" {
			if !validID(inode.Object) {
				return SnapshotInfo{}, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
			}
			objects[inode.Object] = struct{}{}
		}
	}
	if _, ok := inodes[rootInode]; !ok {
		return SnapshotInfo{}, errors.New("snapshot omits root inode")
	}
	seenEntries := make(map[metadataEntryKey]struct{}, len(metadata.Entries))
	for _, entry := range metadata.Entries {
		key := metadataEntryKey{Parent: entry.Parent, Name: entry.Name}
		if validateEntryName(entry.Name) != nil || entry.Parent == 0 || entry.Inode == 0 {
			return SnapshotInfo{}, fmt.Errorf("invalid snapshot directory entry %+v", entry)
		}
		if _, ok := inodes[entry.Parent]; !ok {
			return SnapshotInfo{}, fmt.Errorf("directory entry %q has missing parent inode %d", entry.Name, entry.Parent)
		}
		if _, ok := inodes[entry.Inode]; !ok {
			return SnapshotInfo{}, fmt.Errorf("directory entry %q has missing inode %d", entry.Name, entry.Inode)
		}
		if _, exists := seenEntries[key]; exists {
			return SnapshotInfo{}, fmt.Errorf("duplicate directory entry %d/%q", entry.Parent, entry.Name)
		}
		seenEntries[key] = struct{}{}
	}
	objectsDir := filepath.Join(source, "objects")
	if err := requireSnapshotDirectory(objectsDir); err != nil {
		return SnapshotInfo{}, err
	}
	if err := r.importSnapshotObjects(objectsDir, objects); err != nil {
		return SnapshotInfo{}, err
	}
	r.info.NextInode = snapshot.NextInode
	if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
		return SnapshotInfo{}, err
	}
	layer, err := r.createMetadataLayer("")
	if err != nil {
		return SnapshotInfo{}, err
	}
	branch := Branch{
		ID:         mainBranch,
		Root:       layer,
		Generation: 1,
		CreatedAt:  time.Now().UTC(),
	}
	state := newBranchMetadata(branch)
	const transactionOps = 1000
	ops := make([]metadataOp, 0, transactionOps)
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		if err := r.commitMetadata(&branch, state, ops); err != nil {
			return err
		}
		ops = ops[:0]
		return nil
	}
	for i := range metadata.Inodes {
		ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &metadata.Inodes[i]})
		if len(ops) == cap(ops) {
			if err := flush(); err != nil {
				return SnapshotInfo{}, err
			}
		}
	}
	for i := range metadata.Entries {
		item := &metadata.Entries[i]
		ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: item.Parent, Entry: &item.DirectoryEntry})
		if len(ops) == cap(ops) {
			if err := flush(); err != nil {
				return SnapshotInfo{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return SnapshotInfo{}, err
	}
	if err := r.writeBranch(branch); err != nil {
		return SnapshotInfo{}, err
	}
	return snapshot, nil
}

// importSnapshotObjects materializes independent repository objects in
// parallel. The unpacked archive is already the durable source of truth for
// this transaction; per-object fsyncs add minutes to package-heavy restores
// without making the not-yet-published repository observable.
func (r *Repository) importSnapshotObjects(source string, objects map[string]struct{}) error {
	objectIDs := make([]string, 0, len(objects))
	for id := range objects {
		objectIDs = append(objectIDs, id)
	}
	sort.Strings(objectIDs)
	workers := min(max(runtime.GOMAXPROCS(0), 16), 32)
	if workers > len(objectIDs) {
		workers = len(objectIDs)
	}
	if workers == 0 {
		return nil
	}

	var next atomic.Uint64
	var failed atomic.Bool
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for !failed.Load() {
				index := int(next.Add(1) - 1)
				if index >= len(objectIDs) {
					return
				}
				id := objectIDs[index]
				destination := filepath.Join(r.path, r.objectPath(id))
				err := os.MkdirAll(filepath.Dir(destination), 0700)
				if err == nil {
					err = snapshotCopyFile(filepath.Join(source, id), destination, r.copyMode == CopyModeReflink, false)
				}
				if err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("importing object %q: %w", id, err)
						failed.Store(true)
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func snapshotCopyFile(source, destination string, reflink, durable bool) error {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	src := os.NewFile(uintptr(fd), source)
	defer src.Close()
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if dst != nil {
			_ = dst.Close()
		}
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	if reflink {
		if cloned, err := cloneFile(src, dst, unix.AT_FDCWD, destination); err == nil {
			dst = cloned
			if durable {
				if err := dst.Sync(); err != nil {
					return err
				}
			}
			keep = true
			return nil
		} else {
			dst = cloned
			if dst == nil {
				return err
			}
		}
	}
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if durable {
		if err := dst.Sync(); err != nil {
			return err
		}
	}
	keep = true
	return nil
}

func requireSnapshotDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("snapshot path %q is not a directory", path)
	}
	return nil
}

func requireSnapshotRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot path %q is not a regular file", path)
	}
	return nil
}
