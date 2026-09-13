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
	"strconv"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	snapshotFormatVersion       = 2
	remoteSnapshotFormatVersion = 5
)

// SnapshotInfo describes one self-contained repository branch snapshot.
type SnapshotInfo struct {
	FormatVersion int       `json:"format_version"`
	RepositoryID  string    `json:"repository_id"`
	BranchID      string    `json:"branch_id"`
	Root          string    `json:"root"`
	NextInode     uint64    `json:"next_inode"`
	LogicalBytes  uint64    `json:"logical_bytes"`
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

// RemoteSnapshotDescriptor is the bounded, path-free metadata needed to
// install an immutable filesystem root on another node. It intentionally does
// not contain file bytes or a host path. It preserves immutable object
// identities and bounded B-tree structure while regular-file bytes are resolved
// by ObjectID through the transaction-fenced provider selected by the caller.
type RemoteSnapshotDescriptor struct {
	Snapshot SnapshotInfo         `json:"snapshot"`
	Root     ttcsnapshot.ObjectID `json:"root"`
}

// NewRemoteSnapshotDescriptor reconstructs a wire descriptor after the
// cluster layer has validated its path-free scalar fields.
func NewRemoteSnapshotDescriptor(repositoryID, branchID, root string, nextInode, logicalBytes uint64) (RemoteSnapshotDescriptor, error) {
	object, err := ttcsnapshot.ParseObjectID(root)
	if err != nil || object.Kind != ttcsnapshot.ObjectKindMetadata {
		return RemoteSnapshotDescriptor{}, fmt.Errorf("invalid remote filesystem root %q", root)
	}
	return RemoteSnapshotDescriptor{
		Snapshot: SnapshotInfo{
			FormatVersion: remoteSnapshotFormatVersion, RepositoryID: repositoryID,
			BranchID: branchID, Root: root, NextInode: nextInode,
			LogicalBytes: logicalBytes, CreatedAt: time.Now().UTC(),
		},
		Root: object,
	}, nil
}

// DescribeRemoteSnapshot returns the immutable root descriptor for branchID
// without writing a staging directory or walking the filesystem tree.
func (r *Repository) DescribeRemoteSnapshot(branchID string) (RemoteSnapshotDescriptor, error) {
	if r == nil || r.path == "" {
		return RemoteSnapshotDescriptor{}, errors.New("remote snapshot description requires a manager-side repository")
	}
	branch, err := r.Branch(branchID)
	if err != nil {
		return RemoteSnapshotDescriptor{}, err
	}
	info := r.Info()
	root, ok := r.graph.Root(graphRootName(branchID))
	if !ok {
		return RemoteSnapshotDescriptor{}, fmt.Errorf("branch %q has no snapshot graph root", branchID)
	}
	rootObject, ok := r.graph.Object(root)
	if !ok || rootObject.State != ttcsnapshot.ObjectLive || rootObject.ID.Kind != ttcsnapshot.ObjectKindMetadata {
		return RemoteSnapshotDescriptor{}, errors.New("remote filesystem graph omits its live root metadata")
	}
	logicalBytes := branch.RemoteLogicalBytes
	if !rootObject.Deferred {
		logicalBytes, err = r.graph.FilesystemBulkBytes(root)
		if err != nil {
			return RemoteSnapshotDescriptor{}, fmt.Errorf("reading filesystem byte aggregate: %w", err)
		}
	}
	return RemoteSnapshotDescriptor{
		Snapshot: SnapshotInfo{
			FormatVersion: remoteSnapshotFormatVersion,
			RepositoryID:  info.ID,
			BranchID:      branch.ID,
			Root:          root.String(),
			NextInode:     info.NextInode,
			LogicalBytes:  logicalBytes,
			CreatedAt:     time.Now().UTC(),
		},
		Root: root,
	}, nil
}

// ImportRemoteBranchDescriptor installs descriptor as a deferred immutable
// graph root without a temporary host directory or shared-storage write.
func (r *Repository) ImportRemoteBranchDescriptor(descriptor RemoteSnapshotDescriptor, targetBranch string) error {
	return r.importRemoteBranchDescriptor(descriptor, targetBranch, 0, 0)
}

// ImportRemoteBranchDescriptorWithOrigins installs a deferred root using
// exact Raft-allocated working and catalog origins.
func (r *Repository) ImportRemoteBranchDescriptorWithOrigins(descriptor RemoteSnapshotDescriptor, targetBranch string, working, catalogWorking ttcsnapshot.SnapshotID) error {
	if working == 0 || catalogWorking == 0 || working == catalogWorking {
		return errors.New("clustered remote filesystem import requires two distinct working origins")
	}
	return r.importRemoteBranchDescriptor(descriptor, targetBranch, working, catalogWorking)
}

func (r *Repository) importRemoteBranchDescriptor(descriptor RemoteSnapshotDescriptor, targetBranch string, externalWorking, externalCatalog ttcsnapshot.SnapshotID) error {
	if err := validateName(targetBranch, "branch"); err != nil {
		return err
	}
	snapshot := descriptor.Snapshot
	if snapshot.FormatVersion != remoteSnapshotFormatVersion || !validID(snapshot.RepositoryID) || snapshot.NextInode <= rootInode ||
		descriptor.Root.String() != snapshot.Root || descriptor.Root.Kind != ttcsnapshot.ObjectKindMetadata {
		return errors.New("invalid remote graph snapshot descriptor")
	}
	return r.withLock(func() error {
		if _, err := r.readBranch(targetBranch); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		working, catalogWorking := externalWorking, externalCatalog
		var err error
		if working == 0 {
			working, err = r.graph.AllocateWorking()
			if err == nil {
				catalogWorking, err = r.graph.AllocateWorking()
			}
		} else {
			err = r.graph.AdoptWorking(working)
			if err == nil {
				err = r.graph.AdoptWorking(catalogWorking)
			}
		}
		if err != nil {
			return err
		}
		if snapshot.NextInode > r.info.NextInode {
			r.info.NextInode = snapshot.NextInode
			if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
				return err
			}
		}
		// A deferred catalog is a local expansion namespace, not merely the
		// identity of the foreign root. Independent imports of the same foreign
		// root may coexist in one repository (for example, remote fan-out into
		// sibling branches). They have distinct range streams and distinct
		// catalog-working origins. Reusing the repository/root pair would make
		// ImportDeferredFilesystemRoot reuse an object owned by the first
		// origin, leaving the later origin empty and eligible for collection;
		// it would also make the later resolver replace the first one. Include
		// the never-reused expansion origin to keep both ownership and resolver
		// lifetime exact. Local forks continue to share this catalog string and
		// its expansion origin intentionally.
		catalog := fmt.Sprintf("%s:%s:%d", snapshot.RepositoryID, snapshot.Root, catalogWorking)
		root, err := r.graph.ImportDeferredFilesystemRoot(catalogWorking, graphRootName(targetBranch), catalog, descriptor.Root)
		if err != nil {
			return err
		}
		if err := r.graph.SetRoot(graphBaseRootName(targetBranch), root); err != nil {
			return err
		}
		branch := Branch{ID: targetBranch, Root: root.String(), BaseRoot: root.String(), RemoteCatalog: catalog, RemoteCatalogSnapshot: uint64(catalogWorking), RemoteLogicalBytes: snapshot.LogicalBytes, WorkingSnapshot: uint64(working), Generation: 1, CreatedAt: time.Now().UTC()}
		return r.writeBranch(branch)
	})
}

// ExportSnapshot writes the immutable metadata and regular-file objects
// reachable from branch into a new directory. It never serializes unrelated
// branches.
func (r *Repository) ExportSnapshot(branchID, destination string) (SnapshotInfo, error) {
	return r.exportSnapshot(branchID, destination)
}

func (r *Repository) exportSnapshot(branchID, destination string) (SnapshotInfo, error) {
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
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("parsing filesystem graph root: %w", err)
	}
	snapshot.LogicalBytes, err = r.graph.FilesystemBulkBytes(root)
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("reading filesystem byte aggregate: %w", err)
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

// ImportPrivateRemoteDescriptor creates a private repository whose first
// branch is backed by descriptor.
func (m *Manager) ImportPrivateRemoteDescriptor(path, name string, descriptor RemoteSnapshotDescriptor, targetBranch string) (*Repository, error) {
	return m.importPrivateRemoteDescriptor(path, name, descriptor, targetBranch, 0, 0)
}

// ImportPrivateRemoteDescriptorWithOrigins creates a clustered private
// repository and deferred branch using exact branch and catalog origins
// allocated by Raft. No dummy main branch or local snapshot origin is created.
func (m *Manager) ImportPrivateRemoteDescriptorWithOrigins(path, name string, descriptor RemoteSnapshotDescriptor, targetBranch string, working, catalogWorking ttcsnapshot.SnapshotID) (*Repository, error) {
	if working == 0 || catalogWorking == 0 || working == catalogWorking {
		return nil, errors.New("clustered private remote import requires two distinct working origins")
	}
	return m.importPrivateRemoteDescriptor(path, name, descriptor, targetBranch, working, catalogWorking)
}

func (m *Manager) importPrivateRemoteDescriptor(path, name string, descriptor RemoteSnapshotDescriptor, targetBranch string, working, catalogWorking ttcsnapshot.SnapshotID) (*Repository, error) {
	if err := validateName(targetBranch, "branch"); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("private repository path %q already exists", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(path)+".remote-import-")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(stage); err != nil {
		return nil, err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	for _, dir := range []string{stage, filepath.Join(stage, "staging"), filepath.Join(stage, "branches"), filepath.Join(stage, "graph"), filepath.Join(stage, "data")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return nil, fmt.Errorf("creating remote volume repository: %w", err)
		}
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	info := VolumeInfo{ID: id, Name: name, CreatedAt: time.Now().UTC(), NextInode: rootInode + 1}
	repository, err := m.openPath(stage, info)
	if err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(stage, "staging"), filepath.Join(stage, "volume.json"), info); err != nil {
		repository.Close()
		return nil, err
	}
	var importErr error
	if working == 0 {
		importErr = repository.ImportRemoteBranchDescriptor(descriptor, targetBranch)
	} else {
		importErr = repository.ImportRemoteBranchDescriptorWithOrigins(descriptor, targetBranch, working, catalogWorking)
	}
	if importErr == nil {
		importErr = atomicWriteFile(filepath.Join(stage, "staging"), filepath.Join(stage, "format"), []byte(strconv.Itoa(formatVersion)+"\n"), 0600)
	}
	if importErr == nil {
		importErr = repository.SyncMetadata()
	}
	closeErr := repository.Close()
	if importErr != nil || closeErr != nil {
		return nil, errors.Join(importErr, closeErr)
	}
	if err := os.Rename(stage, path); err != nil {
		return nil, err
	}
	keepStage = true
	repository, err = m.OpenPrivate(path)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	return repository, nil
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
	return r.importPortableSnapshotMode(source, mainBranch)
}

func (r *Repository) importPortableSnapshotMode(source, targetBranch string) (SnapshotInfo, error) {
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
	if err := r.importGraphFiles(&metadata, objectsDir, objects, ttcsnapshot.SnapshotID(branch.WorkingSnapshot)); err != nil {
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

// importGraphFiles re-origins imported whole-file objects into the target
// repository's working snapshot.
func (r *Repository) importGraphFiles(metadata *snapshotMetadata, objectsDir string, objects map[string]int64, working ttcsnapshot.SnapshotID) error {
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
		file, err := os.Open(filepath.Join(objectsDir, source))
		if err != nil {
			return err
		}
		err = r.data.Install(id, file, size)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if err := r.graph.CommitReservedObject(id, graphConstructionPin(id), uint64(size), nil, nil); err != nil {
			_ = r.data.Remove(id)
			return err
		}
		translated[source] = id.String()
	}
	for index := range metadata.Inodes {
		if target, ok := translated[metadata.Inodes[index].Object]; ok {
			metadata.Inodes[index].Object = target
			metadata.Inodes[index].DataGeneration = uint64(working)
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
