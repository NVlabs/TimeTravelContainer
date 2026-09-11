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
	"sort"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	snapshotFormatVersion       = 2
	remoteSnapshotFormatVersion = 3
)

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

// remoteGraphMetadata is the graph-native control plane for a live remote
// filesystem handoff. It preserves immutable object identities and bounded
// B-tree structure; regular-file bytes remain on the source and are fetched
// through the transaction-fenced object relay.
type remoteGraphMetadata struct {
	Root    ttcsnapshot.ObjectID `json:"root"`
	Objects []ttcsnapshot.Object `json:"objects"`
}

// ExportRemoteSnapshot writes the immutable graph metadata for a sealed
// branch. Regular-file objects are returned as an allowlist and are fetched
// by the target in authenticated 1 MiB ranges. Unlike portable snapshot
// export, this does not flatten inode and directory B-trees.
func (r *Repository) ExportRemoteSnapshot(branchID, destination string) (SnapshotInfo, map[string]int64, error) {
	if r == nil || r.path == "" {
		return SnapshotInfo{}, nil, errors.New("remote snapshot export requires a manager-side repository")
	}
	branch, err := r.Branch(branchID)
	if err != nil {
		return SnapshotInfo{}, nil, err
	}
	info := r.Info()
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return SnapshotInfo{}, nil, fmt.Errorf("parsing filesystem graph root: %w", err)
	}
	objects, err := ttcsnapshot.ReachableObjects(r.graph, []ttcsnapshot.ObjectID{root})
	if err != nil {
		return SnapshotInfo{}, nil, fmt.Errorf("enumerating filesystem graph metadata: %w", err)
	}
	catalog, err := ttcsnapshot.NewImmutableCatalog(objects)
	if err != nil {
		return SnapshotInfo{}, nil, err
	}
	if _, ok, err := catalog.FilesystemInode(root, rootInode); err != nil || !ok {
		return SnapshotInfo{}, nil, errors.Join(errors.New("remote filesystem graph omits its root inode"), err)
	}
	bulk, err := ttcsnapshot.ReachableBulkObjects(catalog, []ttcsnapshot.ObjectID{root})
	if err != nil {
		return SnapshotInfo{}, nil, err
	}
	snapshot := SnapshotInfo{FormatVersion: remoteSnapshotFormatVersion, RepositoryID: info.ID, BranchID: branch.ID, Root: branch.Root, NextInode: info.NextInode, CreatedAt: time.Now().UTC()}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return SnapshotInfo{}, nil, err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return SnapshotInfo{}, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	allow := make(map[string]int64)
	for id, length := range bulk {
		if id.Kind != ttcsnapshot.ObjectKindFile || length > uint64(^uint64(0)>>1) {
			return SnapshotInfo{}, nil, fmt.Errorf("filesystem graph contains invalid bulk object %s", id)
		}
		allow[id.String()] = int64(length)
	}
	graph := remoteGraphMetadata{Root: root, Objects: objects}
	if err := writeJSONAtomic(destination, filepath.Join(destination, "graph.json"), graph); err != nil {
		return SnapshotInfo{}, nil, err
	}
	if err := writeJSONAtomic(destination, filepath.Join(destination, "snapshot.json"), snapshot); err != nil {
		return SnapshotInfo{}, nil, err
	}
	keep = true
	return snapshot, allow, nil
}

func readRemoteGraphSnapshot(source string) (SnapshotInfo, snapshotMetadata, map[string]int64, error) {
	var snapshot SnapshotInfo
	if err := readJSON(filepath.Join(source, "snapshot.json"), &snapshot); err != nil {
		return SnapshotInfo{}, snapshotMetadata{}, nil, err
	}
	if snapshot.FormatVersion != remoteSnapshotFormatVersion || !validID(snapshot.RepositoryID) || snapshot.NextInode <= rootInode {
		return SnapshotInfo{}, snapshotMetadata{}, nil, errors.New("invalid remote graph snapshot identity")
	}
	var graph remoteGraphMetadata
	if err := readJSON(filepath.Join(source, "graph.json"), &graph); err != nil {
		return SnapshotInfo{}, snapshotMetadata{}, nil, err
	}
	if graph.Root.String() != snapshot.Root {
		return SnapshotInfo{}, snapshotMetadata{}, nil, errors.New("remote graph root disagrees with snapshot descriptor")
	}
	catalog, err := ttcsnapshot.NewImmutableCatalog(graph.Objects)
	if err != nil {
		return SnapshotInfo{}, snapshotMetadata{}, nil, err
	}
	inodes, entries, err := catalog.FilesystemRecords(graph.Root)
	if err != nil {
		return SnapshotInfo{}, snapshotMetadata{}, nil, err
	}
	metadata := snapshotMetadata{Inodes: make([]Inode, 0, len(inodes)), Entries: make([]snapshotDirectoryEntry, 0, len(entries))}
	objects := make(map[string]int64)
	for _, record := range inodes {
		inode := inodeFromGraph(record)
		metadata.Inodes = append(metadata.Inodes, inode)
		if inode.Object != "" {
			if inode.Size < 0 {
				return SnapshotInfo{}, snapshotMetadata{}, nil, fmt.Errorf("inode %d has invalid size", inode.Number)
			}
			objects[inode.Object] = inode.Size
		}
	}
	for _, entry := range entries {
		metadata.Entries = append(metadata.Entries, snapshotDirectoryEntry{Parent: entry.Parent, DirectoryEntry: DirectoryEntry{Name: entry.Name, Inode: entry.Inode}})
	}
	return snapshot, metadata, objects, nil
}

// ExportSnapshot writes the immutable metadata and regular-file objects
// reachable from branch into a new directory. It never serializes unrelated
// branches.
func (r *Repository) ExportSnapshot(branchID, destination string) (SnapshotInfo, error) {
	return r.exportSnapshot(branchID, destination, nil)
}

// ExportSnapshotDelta exports full namespace metadata but omits objects that
// are already reachable from an immutable parent pack. Locally changed
// objects remain self-contained in the resulting delta.
func (r *Repository) ExportSnapshotDelta(branchID, destination string, inherited map[string]struct{}) (SnapshotInfo, error) {
	return r.exportSnapshot(branchID, destination, inherited)
}

func (r *Repository) exportSnapshot(branchID, destination string, inherited map[string]struct{}) (SnapshotInfo, error) {
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
	if err := r.completeMetadata(state); err != nil {
		return SnapshotInfo{}, err
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	metadata := snapshotMetadata{
		Inodes:  make([]Inode, 0, len(state.inodes)),
		Entries: make([]snapshotDirectoryEntry, 0, len(state.entries)),
	}
	objects := make(map[string]int64)
	for _, inode := range state.inodes {
		metadata.Inodes = append(metadata.Inodes, inode)
		if inode.Object != "" {
			if !validObjectID(inode.Object) {
				return SnapshotInfo{}, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
			}
			objects[inode.Object] = inode.Size
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
	for _, id := range objectIDs {
		if _, ok := inherited[id]; ok {
			continue
		}
		if pending, err := r.remoteObjectPending(id); err != nil {
			return SnapshotInfo{}, err
		} else if pending {
			// A remotely-backed target object is inherited even though its
			// target-local graph identity differs from the source identity.
			continue
		}
		if err := r.ensureObjectRange(id, objects[id], 0, objects[id]); err != nil {
			return SnapshotInfo{}, fmt.Errorf("materializing remote object %q: %w", id, err)
		}
		source := filepath.Join(r.path, r.objectPath(id))
		if err := snapshotCopyFile(source, filepath.Join(destination, "objects", id), r.copyMode == CopyModeReflink); err != nil {
			return SnapshotInfo{}, fmt.Errorf("copying object %q: %w", id, err)
		}
	}
	if err := writeJSONAtomic(destination, filepath.Join(destination, "snapshot.json"), snapshot); err != nil {
		return SnapshotInfo{}, err
	}
	keep = true
	return snapshot, nil
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

// ImportPrivateRemoteSnapshot installs metadata and sparse local object files
// for a peer generation. targetBranch is the physical branch identity already
// embedded in the captured Kernel.
func (m *Manager) ImportPrivateRemoteSnapshot(path, name, source, targetBranch string) (*Repository, SnapshotInfo, error) {
	if err := validateName(targetBranch, "branch"); err != nil {
		return nil, SnapshotInfo{}, err
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return nil, SnapshotInfo{}, fmt.Errorf("private repository path %q already exists", path)
	} else if !os.IsNotExist(err) {
		return nil, SnapshotInfo{}, err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, SnapshotInfo{}, err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(path)+".remote-import-")
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	if err := os.Remove(stage); err != nil {
		return nil, SnapshotInfo{}, err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	repository, err := m.CreatePrivate(stage, name)
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	snapshot, err := repository.importSnapshotMode(source, true, targetBranch)
	closeErr := repository.Close()
	if err != nil || closeErr != nil {
		return nil, SnapshotInfo{}, errors.Join(err, closeErr)
	}
	// Readers and background GC must see either no repository or its complete
	// format, metadata, branches, and sparse objects. Publishing the populated
	// sibling directory with one rename avoids exposing the creation sequence.
	if err := os.Rename(stage, path); err != nil {
		return nil, SnapshotInfo{}, err
	}
	keepStage = true
	repository, err = m.OpenPrivate(path)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, SnapshotInfo{}, err
	}
	return repository, snapshot, nil
}

// ImportRemoteBranchSnapshot adds a sparse inherited branch to an existing
// repository. This is used when a remote fork joins a family whose Sentry is
// already running on the target node.
func (r *Repository) ImportRemoteBranchSnapshot(source, targetBranch string) (SnapshotInfo, error) {
	if err := validateName(targetBranch, "branch"); err != nil {
		return SnapshotInfo{}, err
	}
	if err := requireSnapshotDirectory(source); err != nil {
		return SnapshotInfo{}, err
	}
	snapshot, metadata, _, err := readRemoteGraphSnapshot(source)
	if err != nil {
		return SnapshotInfo{}, err
	}
	return snapshot, r.withLock(func() error {
		if _, err := r.readBranch(targetBranch); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		objects := make(map[string]int64)
		seenInodes := make(map[uint64]struct{}, len(metadata.Inodes))
		for i := range metadata.Inodes {
			inode := &metadata.Inodes[i]
			if inode.Number == 0 || inode.Type == "" {
				return fmt.Errorf("invalid snapshot inode %+v", *inode)
			}
			if _, ok := seenInodes[inode.Number]; ok {
				return fmt.Errorf("duplicate snapshot inode %d", inode.Number)
			}
			seenInodes[inode.Number] = struct{}{}
			if inode.Type == "regular" {
				if !validObjectID(inode.Object) || inode.Size < 0 {
					return fmt.Errorf("invalid regular inode %d", inode.Number)
				}
				inode.DataGeneration = 0
				objects[inode.Object] = inode.Size
			}
		}
		if _, ok := seenInodes[rootInode]; !ok {
			return errors.New("snapshot omits root inode")
		}
		working, err := r.graph.AllocateWorking()
		if err != nil {
			return err
		}
		if err := r.importGraphFiles(&metadata, "", objects, working, true); err != nil {
			return err
		}
		if snapshot.NextInode > r.info.NextInode {
			r.info.NextInode = snapshot.NextInode
			if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
				return err
			}
		}
		branch := Branch{ID: targetBranch, WorkingSnapshot: uint64(working), Generation: 1, CreatedAt: time.Now().UTC()}
		state := newBranchMetadata(branch)
		ops := make([]metadataOp, 0, len(metadata.Inodes)+len(metadata.Entries))
		for i := range metadata.Inodes {
			ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &metadata.Inodes[i]})
		}
		for i := range metadata.Entries {
			entry := &metadata.Entries[i]
			ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: entry.Parent, Entry: &entry.DirectoryEntry})
		}
		const batch = 1000
		for len(ops) != 0 {
			count := min(len(ops), batch)
			if err := r.commitMetadata(&branch, state, ops[:count]); err != nil {
				return err
			}
			ops = ops[count:]
		}
		if err := r.flushMetadataBatchLocked(targetBranch); err != nil {
			return err
		}
		state.mu.RLock()
		branch = state.branch
		state.mu.RUnlock()
		if err := r.sealRemoteImport(&branch, state); err != nil {
			return err
		}
		return r.writeBranch(branch)
	})
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
	return r.importSnapshotMode(source, false, mainBranch)
}

func (r *Repository) importSnapshotMode(source string, remote bool, targetBranch string) (SnapshotInfo, error) {
	if err := requireSnapshotDirectory(source); err != nil {
		return SnapshotInfo{}, err
	}
	if remote {
		return r.importRemoteSnapshotMode(source, targetBranch)
	}
	snapshotPath := filepath.Join(source, "snapshot.json")
	if err := requireSnapshotRegularFile(snapshotPath); err != nil {
		return SnapshotInfo{}, err
	}
	var snapshot SnapshotInfo
	if err := readJSON(snapshotPath, &snapshot); err != nil {
		return SnapshotInfo{}, err
	}
	if snapshot.FormatVersion != snapshotFormatVersion || !validID(snapshot.RepositoryID) || snapshot.Root == "" || snapshot.NextInode <= rootInode {
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
	objects := make(map[string]int64)
	for _, inode := range metadata.Inodes {
		if inode.Number == 0 || inode.Type == "" {
			return SnapshotInfo{}, fmt.Errorf("invalid snapshot inode %+v", inode)
		}
		if _, exists := inodes[inode.Number]; exists {
			return SnapshotInfo{}, fmt.Errorf("duplicate snapshot inode %d", inode.Number)
		}
		inodes[inode.Number] = inode
		if inode.Object != "" {
			if !validObjectID(inode.Object) {
				return SnapshotInfo{}, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
			}
			if previous, ok := objects[inode.Object]; ok && previous != inode.Size {
				return SnapshotInfo{}, fmt.Errorf("object %q has inconsistent sizes %d and %d", inode.Object, previous, inode.Size)
			}
			objects[inode.Object] = inode.Size
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
	branch, err := r.readBranch(targetBranch)
	if errors.Is(err, os.ErrNotExist) {
		working, allocErr := r.graph.AllocateWorking()
		if allocErr != nil {
			return SnapshotInfo{}, allocErr
		}
		branch = Branch{ID: targetBranch, WorkingSnapshot: uint64(working), Generation: 1, CreatedAt: time.Now().UTC()}
	} else if err != nil {
		return SnapshotInfo{}, err
	}
	if err := r.importGraphFiles(&metadata, objectsDir, objects, ttcsnapshot.SnapshotID(branch.WorkingSnapshot), remote); err != nil {
		return SnapshotInfo{}, err
	}
	r.info.NextInode = snapshot.NextInode
	if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
		return SnapshotInfo{}, err
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
	if err := r.flushMetadataBatchLocked(targetBranch); err != nil {
		return SnapshotInfo{}, err
	}
	state.mu.RLock()
	branch = state.branch
	state.mu.RUnlock()
	if err := r.writeBranch(branch); err != nil {
		return SnapshotInfo{}, err
	}
	return snapshot, nil
}

func (r *Repository) importRemoteSnapshotMode(source, targetBranch string) (SnapshotInfo, error) {
	snapshot, metadata, objects, err := readRemoteGraphSnapshot(source)
	if err != nil {
		return SnapshotInfo{}, err
	}
	for i := range metadata.Inodes {
		if metadata.Inodes[i].Type == "regular" {
			metadata.Inodes[i].DataGeneration = 0
		}
	}
	branch, err := r.readBranch(targetBranch)
	if errors.Is(err, os.ErrNotExist) {
		working, allocErr := r.graph.AllocateWorking()
		if allocErr != nil {
			return SnapshotInfo{}, allocErr
		}
		branch = Branch{ID: targetBranch, WorkingSnapshot: uint64(working), Generation: 1, CreatedAt: time.Now().UTC()}
	} else if err != nil {
		return SnapshotInfo{}, err
	}
	if err := r.importGraphFiles(&metadata, "", objects, ttcsnapshot.SnapshotID(branch.WorkingSnapshot), true); err != nil {
		return SnapshotInfo{}, err
	}
	if snapshot.NextInode > r.info.NextInode {
		r.info.NextInode = snapshot.NextInode
		if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
			return SnapshotInfo{}, err
		}
	}
	state := newBranchMetadata(branch)
	ops := make([]metadataOp, 0, len(metadata.Inodes)+len(metadata.Entries))
	for i := range metadata.Inodes {
		ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &metadata.Inodes[i]})
	}
	for i := range metadata.Entries {
		entry := &metadata.Entries[i]
		ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: entry.Parent, Entry: &entry.DirectoryEntry})
	}
	const batch = 1000
	for len(ops) != 0 {
		count := min(len(ops), batch)
		if err := r.commitMetadata(&branch, state, ops[:count]); err != nil {
			return SnapshotInfo{}, err
		}
		ops = ops[count:]
	}
	if err := r.flushMetadataBatchLocked(targetBranch); err != nil {
		return SnapshotInfo{}, err
	}
	state.mu.RLock()
	branch = state.branch
	state.mu.RUnlock()
	if err := r.sealRemoteImport(&branch, state); err != nil {
		return SnapshotInfo{}, err
	}
	if err := r.writeBranch(branch); err != nil {
		return SnapshotInfo{}, err
	}
	return snapshot, nil
}

func (r *Repository) sealRemoteImport(branch *Branch, state *branchMetadata) error {
	if err := r.graph.Seal(ttcsnapshot.SnapshotID(branch.WorkingSnapshot), ttcsnapshot.SnapshotSealedLocal); err != nil {
		return err
	}
	working, err := r.graph.AllocateWorking()
	if err != nil {
		return err
	}
	branch.WorkingSnapshot = uint64(working)
	branch.Generation++
	state.mu.Lock()
	state.branch = *branch
	state.mu.Unlock()
	return nil
}

// importGraphFiles re-origins imported whole-file objects into the target
// repository's working snapshot. Remote imports keep the source object ID in
// the sparse-object marker so peer reads remain correctly addressed even
// though target-local identities are independent.
func (r *Repository) importGraphFiles(metadata *snapshotMetadata, objectsDir string, objects map[string]int64, working ttcsnapshot.SnapshotID, remote bool) error {
	translated := make(map[string]string, len(objects))
	keys := make([]string, 0, len(objects))
	for source := range objects {
		keys = append(keys, source)
	}
	sort.Strings(keys)
	for _, source := range keys {
		size := objects[source]
		id, err := r.graph.ReserveObject(working, ttcsnapshot.ObjectKindFile)
		if err != nil {
			return err
		}
		if remote {
			path, err := r.data.Path(id)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err == nil {
				err = file.Truncate(size)
			}
			if file != nil {
				err = errors.Join(err, file.Sync(), file.Close())
			}
			if err != nil {
				return err
			}
		} else {
			file, err := os.Open(filepath.Join(objectsDir, source))
			if err != nil {
				return err
			}
			err = r.data.Install(id, file, size)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
		if err := r.graph.CommitReservedObject(id, graphConstructionPin(id), uint64(size), nil, nil); err != nil {
			_ = r.data.Remove(id)
			return err
		}
		if remote && size != 0 {
			if err := r.markRemoteObjectPending(id.String(), source); err != nil {
				return err
			}
		}
		translated[source] = id.String()
	}
	for index := range metadata.Inodes {
		if target, ok := translated[metadata.Inodes[index].Object]; ok {
			metadata.Inodes[index].Object = target
			if remote {
				metadata.Inodes[index].DataGeneration = 0
			} else {
				metadata.Inodes[index].DataGeneration = uint64(working)
			}
		}
	}
	return nil
}

func snapshotCopyFile(source, destination string, reflink bool) error {
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
		_ = dst.Close()
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	if reflink {
		if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err == nil {
			if err := dst.Sync(); err != nil {
				return err
			}
			keep = true
			return nil
		}
	}
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
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
