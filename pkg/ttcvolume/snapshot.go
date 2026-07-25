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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

const snapshotFormatVersion = 1

// SnapshotInfo describes one self-contained repository branch snapshot.
type SnapshotInfo struct {
	FormatVersion int       `json:"format_version"`
	RepositoryID  string    `json:"repository_id"`
	BranchID      string    `json:"branch_id"`
	Root          string    `json:"root"`
	NextInode     uint64    `json:"next_inode"`
	CreatedAt     time.Time `json:"created_at"`
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
	for _, name := range []string{"nodes", "objects"} {
		if err := os.Mkdir(filepath.Join(destination, name), 0700); err != nil {
			return SnapshotInfo{}, err
		}
	}

	nodes := make(map[string][]byte)
	objects := make(map[string]struct{})
	var visit func(string) error
	visit = func(id string) error {
		if _, ok := nodes[id]; ok {
			return nil
		}
		node, err := r.getTreeNode(id)
		if err != nil {
			return err
		}
		data, err := r.readFile(filepath.Join("nodes", id))
		if err != nil {
			return err
		}
		nodes[id] = data
		if len(node.Value) != 0 {
			var inode Inode
			if err := json.Unmarshal(node.Value, &inode); err == nil && inode.Number != 0 && inode.Type != "" && inode.Object != "" {
				if !validID(inode.Object) {
					return fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
				}
				objects[inode.Object] = struct{}{}
			}
		}
		children := make([]string, 0, len(node.Children))
		for _, child := range node.Children {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(branch.Root); err != nil {
		return SnapshotInfo{}, fmt.Errorf("walking snapshot tree: %w", err)
	}

	nodeIDs := make([]string, 0, len(nodes))
	for id := range nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		if err := os.WriteFile(filepath.Join(destination, "nodes", id), nodes[id], 0600); err != nil {
			return SnapshotInfo{}, err
		}
	}
	objectIDs := make([]string, 0, len(objects))
	for id := range objects {
		objectIDs = append(objectIDs, id)
	}
	sort.Strings(objectIDs)
	for _, id := range objectIDs {
		if err := snapshotCopyFile(filepath.Join(r.path, "objects", id), filepath.Join(destination, "objects", id), r.copyMode == CopyModeReflink); err != nil {
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
	metadataPath := filepath.Join(source, "snapshot.json")
	if err := requireSnapshotRegularFile(metadataPath); err != nil {
		return SnapshotInfo{}, err
	}
	var snapshot SnapshotInfo
	if err := readJSON(metadataPath, &snapshot); err != nil {
		return SnapshotInfo{}, err
	}
	if snapshot.FormatVersion != snapshotFormatVersion || !validID(snapshot.RepositoryID) || !validSnapshotNodeID(snapshot.Root) || snapshot.NextInode <= rootInode {
		return SnapshotInfo{}, fmt.Errorf("invalid TTC volume snapshot metadata %+v", snapshot)
	}
	for _, kind := range []string{"nodes", "objects"} {
		directory := filepath.Join(source, kind)
		if err := requireSnapshotDirectory(directory); err != nil {
			return SnapshotInfo{}, err
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return SnapshotInfo{}, err
		}
		for _, entry := range entries {
			valid := validID(entry.Name())
			if kind == "nodes" {
				decoded, err := hex.DecodeString(entry.Name())
				valid = err == nil && len(decoded) == 32
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return SnapshotInfo{}, err
			}
			if !entryInfo.Mode().IsRegular() || !valid {
				return SnapshotInfo{}, fmt.Errorf("invalid snapshot %s entry %q", kind, entry.Name())
			}
			if err := snapshotCopyFile(
				filepath.Join(source, kind, entry.Name()),
				filepath.Join(r.path, kind, entry.Name()),
				r.copyMode == CopyModeReflink,
			); err != nil {
				if errors.Is(err, os.ErrExist) && kind == "nodes" {
					continue
				}
				return SnapshotInfo{}, err
			}
		}
	}
	if err := r.validateSnapshotTree(snapshot.Root); err != nil {
		return SnapshotInfo{}, fmt.Errorf("validating imported snapshot root: %w", err)
	}
	r.info.NextInode = snapshot.NextInode
	if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
		return SnapshotInfo{}, err
	}
	branch := Branch{
		ID:         mainBranch,
		Root:       snapshot.Root,
		Generation: 1,
		CreatedAt:  time.Now().UTC(),
	}
	if err := r.writeBranch(branch); err != nil {
		return SnapshotInfo{}, err
	}
	return snapshot, nil
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

func validSnapshotNodeID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32
}

func (r *Repository) validateSnapshotTree(root string) error {
	visited := make(map[string]struct{})
	var walk func(string) error
	walk = func(id string) error {
		if _, ok := visited[id]; ok {
			return nil
		}
		visited[id] = struct{}{}
		node, err := r.getTreeNode(id)
		if err != nil {
			return err
		}
		if len(node.Value) != 0 {
			var inode Inode
			if err := json.Unmarshal(node.Value, &inode); err == nil && inode.Number != 0 && inode.Type != "" && inode.Object != "" {
				if !validID(inode.Object) {
					return fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
				}
				file, err := r.openFile(filepath.Join("objects", inode.Object), os.O_RDONLY, 0)
				if err != nil {
					return fmt.Errorf("opening inode %d object %q: %w", inode.Number, inode.Object, err)
				}
				if err := file.Close(); err != nil {
					return err
				}
			}
		}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}
