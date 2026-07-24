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

// Package volume implements TTC's persistent, branchable managed-volume
// repository. Namespace metadata is stored in an immutable radix trie, while
// regular-file contents live in opaque host files.
package ttcvolume

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	formatVersion = 1
	mainBranch    = "main"
	rootInode     = uint64(1)
)

// MainBranch is the initial persistent volume branch.
const MainBranch = mainBranch

// RootInode is the virtual inode number of every volume root.
const RootInode = rootInode

// CopyMode describes how shared regular-file objects are privatized.
type CopyMode string

const (
	// CopyModeReflink uses FICLONE and leaves extent CoW to the host filesystem.
	CopyModeReflink CopyMode = "reflink"
	// CopyModeFull copies the complete file after explicit daemon opt-in.
	CopyModeFull CopyMode = "full-copy"
)

// Capability is the result of probing the actual volume object store.
type Capability struct {
	Mode           CopyMode `json:"mode"`
	FilesystemType int64    `json:"filesystem_type"`
	Device         uint64   `json:"device"`
}

// Manager owns all volume repositories beneath one TTC data root.
type Manager struct {
	root       string
	volumes    string
	capability Capability
}

// OpenManager creates the volume store and verifies its file-copy policy.
func OpenManager(dataRoot string, allowSlowCopy bool) (*Manager, error) {
	if dataRoot == "" {
		return nil, errors.New("volume manager data root must not be empty")
	}
	volumes := filepath.Join(dataRoot, "volumes-v2")
	if err := os.MkdirAll(volumes, 0700); err != nil {
		return nil, fmt.Errorf("creating volume object store: %w", err)
	}
	capability, err := probeCapability(volumes, allowSlowCopy)
	if err != nil {
		return nil, err
	}
	return &Manager{root: dataRoot, volumes: volumes, capability: capability}, nil
}

// Capability returns the verified copy policy for this manager.
func (m *Manager) Capability() Capability {
	return m.capability
}

func probeCapability(root string, allowSlowCopy bool) (Capability, error) {
	var statfs unix.Statfs_t
	if err := unix.Statfs(root, &statfs); err != nil {
		return Capability{}, fmt.Errorf("stating volume backing filesystem: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return Capability{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Capability{}, errors.New("volume backing filesystem has unsupported stat data")
	}
	capability := Capability{Mode: CopyModeReflink, FilesystemType: int64(statfs.Type), Device: uint64(stat.Dev)}
	probeDir := filepath.Join(root, ".probe")
	if err := os.MkdirAll(probeDir, 0700); err != nil {
		return Capability{}, err
	}
	source, err := os.CreateTemp(probeDir, "source-")
	if err != nil {
		return Capability{}, err
	}
	sourceName := source.Name()
	defer os.Remove(sourceName)
	defer source.Close()
	if _, err := source.Write([]byte("parent")); err != nil {
		return Capability{}, err
	}
	if err := source.Sync(); err != nil {
		return Capability{}, err
	}
	clone, err := os.CreateTemp(probeDir, "clone-")
	if err != nil {
		return Capability{}, err
	}
	cloneName := clone.Name()
	defer os.Remove(cloneName)
	defer clone.Close()
	if err := unix.IoctlFileClone(int(clone.Fd()), int(source.Fd())); err != nil {
		if allowSlowCopy {
			capability.Mode = CopyModeFull
			return capability, nil
		}
		return Capability{}, fmt.Errorf("volume backing filesystem does not support FICLONE: %w; move --data-root to a reflink-capable filesystem or explicitly start ttcd with --allow-slow-volume-copy", err)
	}
	if _, err := clone.WriteAt([]byte("branch"), 0); err != nil {
		return Capability{}, fmt.Errorf("writing reflink probe clone: %w", err)
	}
	got := make([]byte, len("parent"))
	if _, err := source.ReadAt(got, 0); err != nil {
		return Capability{}, fmt.Errorf("reading reflink probe source: %w", err)
	}
	if string(got) != "parent" {
		return Capability{}, errors.New("volume backing filesystem reported FICLONE but clone writes changed the source")
	}
	return capability, nil
}

// VolumeInfo is persistent volume identity metadata.
type VolumeInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	NextInode uint64    `json:"next_inode"`
}

// Branch is an immutable-tree root and branch-local data generation.
type Branch struct {
	ID         string    `json:"id"`
	Parent     string    `json:"parent,omitempty"`
	Root       string    `json:"root"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
}

// Inode is branch-resolved virtual inode metadata.
type Inode struct {
	Number         uint64 `json:"number"`
	Type           string `json:"type"`
	Mode           uint32 `json:"mode"`
	UID            uint32 `json:"uid"`
	GID            uint32 `json:"gid"`
	Nlink          uint32 `json:"nlink"`
	Size           int64  `json:"size,omitempty"`
	Object         string `json:"object,omitempty"`
	DataGeneration uint64 `json:"data_generation,omitempty"`
}

// DirectoryEntry is one persistent name-to-virtual-inode mapping.
type DirectoryEntry struct {
	Name  string `json:"name"`
	Inode uint64 `json:"inode"`
}

// Repository is one named persistent volume.
type Repository struct {
	manager *Manager
	path    string
	info    VolumeInfo
}

// Create creates a new volume with an empty main branch.
func (m *Manager) Create(name string) (*Repository, error) {
	if err := validateName(name, "volume"); err != nil {
		return nil, err
	}
	if volumes, err := m.List(); err != nil {
		return nil, err
	} else {
		for _, volume := range volumes {
			if volume.Name == name {
				return nil, fmt.Errorf("volume %q already exists", name)
			}
		}
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(m.volumes, id)
	for _, dir := range []string{path, filepath.Join(path, "nodes"), filepath.Join(path, "objects"), filepath.Join(path, "staging"), filepath.Join(path, "branches")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			_ = os.RemoveAll(path)
			return nil, fmt.Errorf("creating volume repository: %w", err)
		}
	}
	info := VolumeInfo{ID: id, Name: name, CreatedAt: time.Now().UTC(), NextInode: rootInode + 1}
	repo := &Repository{manager: m, path: path, info: info}
	if err := writeJSONAtomic(filepath.Join(path, "staging"), filepath.Join(path, "volume.json"), info); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	root, err := repo.emptyTree()
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	rootMeta := Inode{Number: rootInode, Type: "directory", Mode: 0755, Nlink: 2}
	root, err = repo.setInode(root, rootMeta)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	branch := Branch{ID: mainBranch, Root: root, Generation: 1, CreatedAt: time.Now().UTC()}
	if err := repo.writeBranch(branch); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := atomicWriteFile(filepath.Join(path, "staging"), filepath.Join(path, "format"), []byte(strconv.Itoa(formatVersion)+"\n"), 0600); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	return repo, nil
}

// List returns all valid v2 volumes.
func (m *Manager) List() ([]VolumeInfo, error) {
	entries, err := os.ReadDir(m.volumes)
	if err != nil {
		return nil, err
	}
	var result []VolumeInfo
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		format, err := os.ReadFile(filepath.Join(m.volumes, entry.Name(), "format"))
		if os.IsNotExist(err) {
			// A repository without its final format marker was never committed.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("loading volume repository %q format: %w", entry.Name(), err)
		}
		if strings.TrimSpace(string(format)) != strconv.Itoa(formatVersion) {
			return nil, fmt.Errorf("volume repository %q uses unsupported format %q", entry.Name(), strings.TrimSpace(string(format)))
		}
		var info VolumeInfo
		if err := readJSON(filepath.Join(m.volumes, entry.Name(), "volume.json"), &info); err != nil {
			return nil, fmt.Errorf("loading volume repository %q: %w", entry.Name(), err)
		}
		if info.ID != entry.Name() || !validID(info.ID) {
			return nil, fmt.Errorf("volume repository %q has invalid identity %q", entry.Name(), info.ID)
		}
		result = append(result, info)
	}
	return result, nil
}

// Open opens a volume by name or opaque ID.
func (m *Manager) Open(nameOrID string) (*Repository, error) {
	volumes, err := m.List()
	if err != nil {
		return nil, err
	}
	for _, info := range volumes {
		if info.Name == nameOrID || info.ID == nameOrID {
			path := filepath.Join(m.volumes, info.ID)
			format, err := os.ReadFile(filepath.Join(path, "format"))
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(string(format)) != strconv.Itoa(formatVersion) {
				return nil, fmt.Errorf("volume %q uses unsupported format %q", info.Name, strings.TrimSpace(string(format)))
			}
			return &Repository{manager: m, path: path, info: info}, nil
		}
	}
	return nil, fs.ErrNotExist
}

// Info returns persistent identity metadata.
func (r *Repository) Info() VolumeInfo {
	return r.info
}

func (r *Repository) withLock(fn func() error) error {
	lock, err := os.OpenFile(filepath.Join(r.path, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := readJSON(filepath.Join(r.path, "volume.json"), &r.info); err != nil {
		return err
	}
	return fn()
}

func inodeKey(ino uint64) []byte {
	return []byte(fmt.Sprintf("i/%016x", ino))
}

func entryKey(parent uint64, name string) []byte {
	return []byte(fmt.Sprintf("d/%016x/%s", parent, name))
}

func entryPrefix(parent uint64) []byte {
	return []byte(fmt.Sprintf("d/%016x/", parent))
}

func (r *Repository) getInode(root string, ino uint64) (Inode, error) {
	raw, ok, err := r.treeGet(root, inodeKey(ino))
	if err != nil {
		return Inode{}, err
	}
	if !ok {
		return Inode{}, fs.ErrNotExist
	}
	var inode Inode
	if err := json.Unmarshal(raw, &inode); err != nil {
		return Inode{}, err
	}
	if inode.Number != ino {
		return Inode{}, fmt.Errorf("inode key %d contains inode %d", ino, inode.Number)
	}
	return inode, nil
}

func (r *Repository) setInode(root string, inode Inode) (string, error) {
	raw, err := json.Marshal(inode)
	if err != nil {
		return "", err
	}
	return r.treeSet(root, inodeKey(inode.Number), raw)
}

func (r *Repository) readBranch(id string) (Branch, error) {
	if err := validateName(id, "branch"); err != nil {
		return Branch{}, err
	}
	var branch Branch
	if err := readJSON(filepath.Join(r.path, "branches", id+".json"), &branch); err != nil {
		return Branch{}, err
	}
	if branch.ID != id || branch.Root == "" || branch.Generation == 0 {
		return Branch{}, fmt.Errorf("invalid branch record for %q", id)
	}
	return branch, nil
}

func (r *Repository) writeBranch(branch Branch) error {
	return writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "branches", branch.ID+".json"), branch)
}

// Branch returns a branch root.
func (r *Repository) Branch(id string) (Branch, error) {
	return r.readBranch(id)
}

// Fork creates child as an O(1) metadata-root fork of source. Advancing both
// branch generations ensures that either side's next regular-file write clones.
func (r *Repository) Fork(source, child string) error {
	if err := validateName(child, "branch"); err != nil {
		return err
	}
	return r.withLock(func() error {
		sourceBranch, err := r.readBranch(source)
		if err != nil {
			return err
		}
		if _, err := r.readBranch(child); err == nil {
			return fmt.Errorf("branch %q already exists", child)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		sourceBranch.Generation++
		childBranch := Branch{
			ID: child, Parent: source, Root: sourceBranch.Root,
			Generation: sourceBranch.Generation, CreatedAt: time.Now().UTC(),
		}
		// Publish the source's new generation first. If the process crashes
		// before the child pointer appears, the only effect is a conservative
		// first-write clone in the source. Publishing the child first would let
		// the old-generation source mutate an object now visible to the child.
		if err := r.writeBranch(sourceBranch); err != nil {
			return err
		}
		return r.writeBranch(childBranch)
	})
}

// CreateRegular creates one empty regular file.
func (r *Repository) CreateRegular(branchID string, parent uint64, name string, mode uint32, uid, gid uint32) (uint64, error) {
	if err := validateEntryName(name); err != nil {
		return 0, err
	}
	var created uint64
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		parentInode, err := r.getInode(branch.Root, parent)
		if err != nil {
			return err
		}
		if parentInode.Type != "directory" {
			return syscall.ENOTDIR
		}
		if _, err := r.lookup(branch.Root, parent, name); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		object, err := r.createObject("")
		if err != nil {
			return err
		}
		created = r.info.NextInode
		r.info.NextInode++
		inode := Inode{
			Number: created, Type: "regular", Mode: mode & 07777,
			UID: uid, GID: gid, Nlink: 1, Object: object,
			DataGeneration: branch.Generation,
		}
		root, err := r.setInode(branch.Root, inode)
		if err != nil {
			return err
		}
		entry := DirectoryEntry{Name: name, Inode: created}
		raw, _ := json.Marshal(entry)
		root, err = r.treeSet(root, entryKey(parent, name), raw)
		if err != nil {
			return err
		}
		branch.Root = root
		if err := writeJSONAtomic(filepath.Join(r.path, "staging"), filepath.Join(r.path, "volume.json"), r.info); err != nil {
			return err
		}
		return r.writeBranch(branch)
	})
	return created, err
}

func (r *Repository) lookup(root string, parent uint64, name string) (uint64, error) {
	raw, ok, err := r.treeGet(root, entryKey(parent, name))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fs.ErrNotExist
	}
	var entry DirectoryEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return 0, err
	}
	if entry.Name != name || entry.Inode == 0 {
		return 0, errors.New("invalid directory entry")
	}
	return entry.Inode, nil
}

// Lookup resolves a child name.
func (r *Repository) Lookup(branchID string, parent uint64, name string) (uint64, error) {
	if err := validateEntryName(name); err != nil {
		return 0, err
	}
	branch, err := r.readBranch(branchID)
	if err != nil {
		return 0, err
	}
	return r.lookup(branch.Root, parent, name)
}

// Inode returns branch-local inode metadata.
func (r *Repository) Inode(branchID string, ino uint64) (Inode, error) {
	branch, err := r.readBranch(branchID)
	if err != nil {
		return Inode{}, err
	}
	return r.getInode(branch.Root, ino)
}

// Link creates a hard link to an existing non-directory inode.
func (r *Repository) Link(branchID string, parent uint64, name string, target uint64) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		inode, err := r.getInode(branch.Root, target)
		if err != nil {
			return err
		}
		if inode.Type == "directory" {
			return syscall.EPERM
		}
		if _, err := r.lookup(branch.Root, parent, name); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		inode.Nlink++
		root, err := r.setInode(branch.Root, inode)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(DirectoryEntry{Name: name, Inode: target})
		root, err = r.treeSet(root, entryKey(parent, name), raw)
		if err != nil {
			return err
		}
		branch.Root = root
		return r.writeBranch(branch)
	})
}

// Unlink removes a non-directory entry. Open-inode lifetime will be integrated
// with Sentry references; object reclamation is deliberately deferred to GC.
func (r *Repository) Unlink(branchID string, parent uint64, name string) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		ino, err := r.lookup(branch.Root, parent, name)
		if err != nil {
			return err
		}
		inode, err := r.getInode(branch.Root, ino)
		if err != nil {
			return err
		}
		if inode.Type == "directory" {
			return syscall.EISDIR
		}
		root, err := r.treeDelete(branch.Root, entryKey(parent, name))
		if err != nil {
			return err
		}
		if inode.Nlink > 1 {
			inode.Nlink--
			root, err = r.setInode(root, inode)
		} else {
			root, err = r.treeDelete(root, inodeKey(ino))
		}
		if err != nil {
			return err
		}
		branch.Root = root
		return r.writeBranch(branch)
	})
}

func (r *Repository) ensurePrivate(branch *Branch, inode *Inode) (bool, error) {
	if inode.Type != "regular" {
		return false, syscall.EINVAL
	}
	if !validID(inode.Object) {
		return false, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
	}
	if inode.DataGeneration == branch.Generation {
		return false, nil
	}
	object, err := r.createObject(inode.Object)
	if err != nil {
		return false, err
	}
	inode.Object = object
	inode.DataGeneration = branch.Generation
	root, err := r.setInode(branch.Root, *inode)
	if err != nil {
		return false, err
	}
	branch.Root = root
	if err := r.writeBranch(*branch); err != nil {
		return false, err
	}
	return true, nil
}

// WriteAt writes directly to a branch-private host object, reflinking the
// complete file first when the inode is shared with another branch generation.
func (r *Repository) WriteAt(branchID string, ino uint64, data []byte, offset int64) (int, bool, error) {
	if offset < 0 {
		return 0, false, syscall.EINVAL
	}
	var written int
	var cloned bool
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		inode, err := r.getInode(branch.Root, ino)
		if err != nil {
			return err
		}
		cloned, err = r.ensurePrivate(&branch, &inode)
		if err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(r.path, "objects", inode.Object), os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		written, err = file.WriteAt(data, offset)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		end := offset + int64(written)
		if end > inode.Size {
			inode.Size = end
			branch.Root, err = r.setInode(branch.Root, inode)
			if err != nil {
				return err
			}
			return r.writeBranch(branch)
		}
		return nil
	})
	return written, cloned, err
}

// ReadAt reads file data from the object selected by the branch's inode table.
func (r *Repository) ReadAt(branchID string, ino uint64, data []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, syscall.EINVAL
	}
	branch, err := r.readBranch(branchID)
	if err != nil {
		return 0, err
	}
	inode, err := r.getInode(branch.Root, ino)
	if err != nil {
		return 0, err
	}
	if inode.Type != "regular" {
		return 0, syscall.EINVAL
	}
	if !validID(inode.Object) {
		return 0, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
	}
	file, err := os.Open(filepath.Join(r.path, "objects", inode.Object))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.ReadAt(data, offset)
}

// Truncate applies the same first-writer CoW rule as WriteAt.
func (r *Repository) Truncate(branchID string, ino uint64, size int64) (bool, error) {
	if size < 0 {
		return false, syscall.EINVAL
	}
	var cloned bool
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		inode, err := r.getInode(branch.Root, ino)
		if err != nil {
			return err
		}
		cloned, err = r.ensurePrivate(&branch, &inode)
		if err != nil {
			return err
		}
		if err := os.Truncate(filepath.Join(r.path, "objects", inode.Object), size); err != nil {
			return err
		}
		inode.Size = size
		branch.Root, err = r.setInode(branch.Root, inode)
		if err != nil {
			return err
		}
		return r.writeBranch(branch)
	})
	return cloned, err
}

// ListDirectory returns entries in deterministic name order.
func (r *Repository) ListDirectory(branchID string, parent uint64) ([]DirectoryEntry, error) {
	branch, err := r.readBranch(branchID)
	if err != nil {
		return nil, err
	}
	values, err := r.treePrefix(branch.Root, entryPrefix(parent))
	if err != nil {
		return nil, err
	}
	entries := make([]DirectoryEntry, 0, len(values))
	for _, raw := range values {
		var entry DirectoryEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (r *Repository) createObject(source string) (string, error) {
	if source != "" && !validID(source) {
		return "", fmt.Errorf("invalid source object ID %q", source)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	staging := filepath.Join(r.path, "staging", "object-"+id)
	destination := filepath.Join(r.path, "objects", id)
	dst, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		dst.Close()
		if !keep {
			os.Remove(staging)
		}
	}()
	if source != "" {
		src, err := os.Open(filepath.Join(r.path, "objects", source))
		if err != nil {
			return "", err
		}
		switch r.manager.capability.Mode {
		case CopyModeReflink:
			err = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
		case CopyModeFull:
			_, err = io.Copy(dst, src)
		default:
			err = fmt.Errorf("unknown volume copy mode %q", r.manager.capability.Mode)
		}
		src.Close()
		if err != nil {
			return "", fmt.Errorf("privatizing volume object %q using %s: %w", source, r.manager.capability.Mode, err)
		}
	}
	if err := dst.Sync(); err != nil {
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(staging, destination); err != nil {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return "", err
	}
	keep = true
	return id, nil
}

func validateName(name, kind string) error {
	if name == "" || name == "." || name == ".." || len(name) > 128 {
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-') {
			return fmt.Errorf("invalid %s name %q", kind, name)
		}
	}
	return nil
}

func validateEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) || len(name) > 255 {
		return fmt.Errorf("invalid directory entry name %q", name)
	}
	return nil
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decoding %q: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(stagingDir, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(stagingDir, path, data, 0600)
}

func atomicWriteFile(stagingDir, path string, data []byte, mode fs.FileMode) error {
	file, err := os.CreateTemp(stagingDir, "publish-")
	if err != nil {
		return err
	}
	staging := file.Name()
	keep := false
	defer func() {
		file.Close()
		if !keep {
			os.Remove(staging)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	keep = true
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
