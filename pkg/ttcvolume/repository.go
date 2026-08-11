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

// Package ttcvolume implements TTC's persistent, branchable managed-volume
// repository. Namespace metadata is stored in transactional, branchable
// journals, while regular-file contents live in opaque host files.
package ttcvolume

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	formatVersion = 2
	mainBranch    = "main"
	rootInode     = uint64(1)
)

// MainBranch is the initial persistent volume branch.
const MainBranch = mainBranch

// RootInode is the virtual inode number of every volume root.
const RootInode = rootInode

// ScopedBranchID maps one user-visible live branch name to a repository branch
// name that cannot collide with the same name in another sandbox family.
func ScopedBranchID(scope, visibleBranchID string) string {
	if scope == "" {
		return visibleBranchID
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + visibleBranchID))
	return fmt.Sprintf("b-%x", sum[:])
}

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

// Branch identifies the immutable metadata layers inherited by a branch and
// its final writable layer. Root aliases the writable layer for compatibility
// with older callers that use it as an opaque change identity.
type Branch struct {
	ID         string    `json:"id"`
	Root       string    `json:"root"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
}

// Inode is branch-resolved virtual inode metadata.
type Inode struct {
	Number         uint64            `json:"number"`
	Type           string            `json:"type"`
	Mode           uint32            `json:"mode"`
	UID            uint32            `json:"uid"`
	GID            uint32            `json:"gid"`
	Nlink          uint32            `json:"nlink"`
	Size           int64             `json:"size,omitempty"`
	Object         string            `json:"object,omitempty"`
	DataGeneration uint64            `json:"data_generation,omitempty"`
	Target         string            `json:"target,omitempty"`
	DevMajor       uint32            `json:"dev_major,omitempty"`
	DevMinor       uint32            `json:"dev_minor,omitempty"`
	AtimeNsec      int64             `json:"atime_nsec,omitempty"`
	BtimeNsec      int64             `json:"btime_nsec,omitempty"`
	CtimeNsec      int64             `json:"ctime_nsec,omitempty"`
	MtimeNsec      int64             `json:"mtime_nsec,omitempty"`
	Xattrs         map[string]string `json:"xattrs,omitempty"`
}

// DirectoryEntry is one persistent name-to-virtual-inode mapping.
type DirectoryEntry struct {
	Name  string `json:"name"`
	Inode uint64 `json:"inode"`
}

// Repository is one named persistent volume.
type Repository struct {
	path     string
	dir      *os.File
	copyMode CopyMode
	info     VolumeInfo

	// Metadata trie nodes are immutable and content-addressed. Keep recently
	// used decoded nodes in memory so namespace-heavy workloads don't reopen
	// and decode every ancestor for every mutation. The cache is bounded since
	// persistent updates continuously create new node identities.
	nodeMu    sync.Mutex
	nodeCache map[string]treeNode
	nodeOrder []string
	nodeNext  int
	packMu    sync.Mutex
	pack      *os.File
	packIndex map[string]packedTreeNode
	packRead  int64

	// Ordinary Linux namespace mutations are not durable until fsync(2),
	// sync(2), or filesystem teardown. Track journal and pointer files that
	// still need their contents made durable.
	dirtyMu    sync.Mutex
	dirtyFiles map[string]struct{}
	dirtyDirs  map[string]struct{}
	knownDirs  map[string]struct{}

	// metadata is the decoded view of each locally-used branch. The journal is
	// authoritative and is replayed when a branch first becomes active.
	metadataMu sync.Mutex
	metadata   map[string]*branchMetadata
	allocNext  uint64
	allocEnd   uint64

	operationMu sync.Mutex
	lockFile    *os.File

	branchMu    sync.Mutex
	branchCache map[string]cachedBranch
	journalMu   sync.Mutex
	journals    map[string]*os.File
}

type cachedBranch struct {
	branch    Branch
	device    uint64
	inode     uint64
	size      int64
	mtimeNsec int64
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
	return m.createAt(path, name, id)
}

// CreatePrivate creates a repository at path without adding it to the named
// volume catalog. TTC uses this for a container-family-owned rootfs upper.
func (m *Manager) CreatePrivate(path, name string) (*Repository, error) {
	if err := validateName(name, "repository"); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("private repository path %q is outside data root %q", path, m.root)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("private repository path %q already exists", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return m.createAt(path, name, id)
}

func (m *Manager) createAt(path, name, id string) (*Repository, error) {
	for _, dir := range []string{path, filepath.Join(path, "objects"), filepath.Join(path, "staging"), filepath.Join(path, "branches"), filepath.Join(path, "layers")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			_ = os.RemoveAll(path)
			return nil, fmt.Errorf("creating volume repository: %w", err)
		}
	}
	info := VolumeInfo{ID: id, Name: name, CreatedAt: time.Now().UTC(), NextInode: rootInode + 1}
	repo, err := m.openPath(path, info)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	keepRepo := false
	defer func() {
		if !keepRepo {
			repo.Close()
		}
	}()
	if err := writeJSONAtomic(filepath.Join(path, "staging"), filepath.Join(path, "volume.json"), info); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	layer, err := repo.createMetadataLayer("")
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	now := time.Now().UnixNano()
	rootMeta := Inode{
		Number: rootInode, Type: "directory", Mode: 0755, Nlink: 2,
		AtimeNsec: now, BtimeNsec: now, CtimeNsec: now, MtimeNsec: now,
	}
	branch := Branch{ID: mainBranch, Root: layer, Generation: 1, CreatedAt: time.Now().UTC()}
	state := newBranchMetadata(branch)
	if err := repo.commitMetadata(&state.branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &rootMeta}}); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := repo.writeBranch(state.branch); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := atomicWriteFile(filepath.Join(path, "staging"), filepath.Join(path, "format"), []byte(strconv.Itoa(formatVersion)+"\n"), 0600); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := repo.SyncMetadata(); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	keepRepo = true
	return repo, nil
}

// OpenPrivate opens a repository created by CreatePrivate.
func (m *Manager) OpenPrivate(path string) (*Repository, error) {
	path = filepath.Clean(path)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("private repository path %q is outside data root %q", path, m.root)
	}
	var info VolumeInfo
	if err := readJSON(filepath.Join(path, "volume.json"), &info); err != nil {
		return nil, err
	}
	if !validID(info.ID) {
		return nil, fmt.Errorf("private repository has invalid identity %q", info.ID)
	}
	format, err := os.ReadFile(filepath.Join(path, "format"))
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(string(format))
	if version != "1" && version != strconv.Itoa(formatVersion) {
		return nil, fmt.Errorf("private repository uses unsupported format %q", strings.TrimSpace(string(format)))
	}
	repository, err := m.openPath(path, info)
	if err != nil {
		return nil, err
	}
	if version == "1" {
		if err := repository.migrateV1(); err != nil {
			repository.Close()
			return nil, fmt.Errorf("migrating private repository to format 2: %w", err)
		}
	}
	return repository, nil
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
		version := strings.TrimSpace(string(format))
		if version != "1" && version != strconv.Itoa(formatVersion) {
			return nil, fmt.Errorf("volume repository %q uses unsupported format %q", entry.Name(), strings.TrimSpace(string(format)))
		}
		var info VolumeInfo
		if err := readJSON(filepath.Join(m.volumes, entry.Name(), "volume.json"), &info); err != nil {
			return nil, fmt.Errorf("loading volume repository %q: %w", entry.Name(), err)
		}
		if info.ID != entry.Name() || !validID(info.ID) {
			return nil, fmt.Errorf("volume repository %q has invalid identity %q", entry.Name(), info.ID)
		}
		if version == "1" {
			repository, err := m.openPath(filepath.Join(m.volumes, entry.Name()), info)
			if err != nil {
				return nil, err
			}
			if err := repository.migrateV1(); err != nil {
				repository.Close()
				return nil, fmt.Errorf("migrating volume repository %q to format 2: %w", info.Name, err)
			}
			if err := repository.Close(); err != nil {
				return nil, err
			}
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
			return m.openPath(path, info)
		}
	}
	return nil, fs.ErrNotExist
}

// Remove deletes an unused volume repository by name or ID.
func (m *Manager) Remove(nameOrID string) error {
	repository, err := m.Open(nameOrID)
	if err != nil {
		return err
	}
	path := repository.path
	repository.Close()
	if path == "" || filepath.Dir(path) != m.volumes {
		return errors.New("invalid volume repository path")
	}
	return os.RemoveAll(path)
}

func (m *Manager) openPath(path string, info VolumeInfo) (*Repository, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Repository{path: path, dir: dir, copyMode: m.capability.Mode, info: info}, nil
}

// OpenRepositoryFD opens a repository from an already-open O_DIRECTORY file.
// The descriptor is duplicated, so the caller retains ownership of dir.
func OpenRepositoryFD(dir *os.File, mode CopyMode) (*Repository, error) {
	if dir == nil {
		return nil, errors.New("nil repository directory")
	}
	dup, err := unix.Dup(int(dir.Fd()))
	if err != nil {
		return nil, err
	}
	repo := &Repository{dir: os.NewFile(uintptr(dup), "ttc-volume-repository"), copyMode: mode}
	if err := repo.readJSON("volume.json", &repo.info); err != nil {
		repo.Close()
		return nil, err
	}
	if !validID(repo.info.ID) {
		repo.Close()
		return nil, fmt.Errorf("invalid repository identity %q", repo.info.ID)
	}
	format, err := repo.readFile("format")
	if err != nil {
		repo.Close()
		return nil, err
	}
	version := strings.TrimSpace(string(format))
	if version != "1" && version != strconv.Itoa(formatVersion) {
		repo.Close()
		return nil, fmt.Errorf("unsupported repository format %q", strings.TrimSpace(string(format)))
	}
	if version == "1" {
		if err := repo.migrateV1(); err != nil {
			repo.Close()
			return nil, fmt.Errorf("migrating donated repository to format 2: %w", err)
		}
	}
	return repo, nil
}

// Close releases the repository directory descriptor.
func (r *Repository) Close() error {
	if r == nil || r.dir == nil {
		return nil
	}
	r.packMu.Lock()
	var packErr error
	if r.pack != nil {
		packErr = r.pack.Close()
		r.pack = nil
	}
	r.packMu.Unlock()
	r.journalMu.Lock()
	var journalErr error
	for layer, journal := range r.journals {
		if err := journal.Close(); err != nil && journalErr == nil {
			journalErr = fmt.Errorf("closing metadata journal %q: %w", layer, err)
		}
	}
	r.journals = nil
	r.journalMu.Unlock()
	var lockErr error
	if r.lockFile != nil {
		lockErr = r.lockFile.Close()
		r.lockFile = nil
	}
	err := r.dir.Close()
	r.dir = nil
	if packErr != nil {
		return packErr
	}
	if lockErr != nil {
		return lockErr
	}
	if journalErr != nil {
		return journalErr
	}
	return err
}

// Directory duplicates the repository directory descriptor for donation to a
// Sentry process or another trusted repository client.
func (r *Repository) Directory() (*os.File, error) {
	if r == nil || r.dir == nil {
		return nil, errors.New("repository is closed")
	}
	dup, err := unix.Dup(int(r.dir.Fd()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dup), "ttc-volume-"+r.info.ID), nil
}

// Info returns persistent identity metadata.
func (r *Repository) Info() VolumeInfo {
	return r.info
}

// InitializeRootRegular converts a newly-created, empty repository root into a
// regular file. It is used for private TTC mounts whose source is a single
// host file. The operation is rejected once the repository has acquired any
// namespace entries or branches.
func (r *Repository) InitializeRootRegular(mode, uid, gid uint32) error {
	return r.withLock(func() error {
		branchDir, err := r.openFile("branches", os.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		names, readErr := branchDir.Readdirnames(-1)
		closeErr := branchDir.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(names) != 1 || names[0] != mainBranch+".json" {
			return errors.New("regular repository root requires a pristine main branch")
		}
		branch, err := r.readBranch(mainBranch)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		root, err := r.metadataInode(state, rootInode)
		if err != nil {
			return err
		}
		if root.Type != "directory" {
			if root.Type == "regular" {
				return nil
			}
			return fmt.Errorf("repository root has unsupported type %q", root.Type)
		}
		entries := r.metadataDirectory(state, rootInode)
		if len(entries) != 0 {
			return errors.New("regular repository root requires an empty namespace")
		}
		object, err := r.createObject("")
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		root = Inode{
			Number: rootInode, Type: "regular", Mode: mode & 07777,
			UID: uid, GID: gid, Nlink: 1, Object: object,
			DataGeneration: branch.Generation,
			AtimeNsec:      now, BtimeNsec: now, CtimeNsec: now, MtimeNsec: now,
		}
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &root}})
	})
}

// HostPath returns the manager-side repository directory. Donated Sentry
// repositories intentionally return an empty path.
func (r *Repository) HostPath() string {
	return r.path
}

func (r *Repository) withLock(fn func() error) error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if r.lockFile == nil {
		lock, err := r.openFile("lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		r.lockFile = lock
	}
	if err := unix.Flock(int(r.lockFile.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(r.lockFile.Fd()), unix.LOCK_UN)
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
	name := filepath.Join("branches", id+".json")
	var stat unix.Stat_t
	if err := unix.Fstatat(int(r.dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Branch{}, err
	}
	r.branchMu.Lock()
	if cached, ok := r.branchCache[id]; ok && cached.device == uint64(stat.Dev) && cached.inode == stat.Ino && cached.size == stat.Size && cached.mtimeNsec == stat.Mtim.Sec*1e9+stat.Mtim.Nsec {
		r.branchMu.Unlock()
		return cached.branch, nil
	}
	r.branchMu.Unlock()
	var branch Branch
	if err := r.readJSON(name, &branch); err != nil {
		return Branch{}, err
	}
	if branch.ID != id || branch.Root == "" || branch.Generation == 0 {
		return Branch{}, fmt.Errorf("invalid branch record for %q", id)
	}
	if err := normalizeBranchLayers(&branch); err != nil {
		return Branch{}, err
	}
	r.branchMu.Lock()
	if r.branchCache == nil {
		r.branchCache = make(map[string]cachedBranch)
	}
	r.branchCache[id] = cachedBranch{
		branch: branch, device: uint64(stat.Dev), inode: stat.Ino,
		size: stat.Size, mtimeNsec: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
	}
	r.branchMu.Unlock()
	return branch, nil
}

func (r *Repository) writeBranch(branch Branch) error {
	if err := r.writeJSONAtomic(filepath.Join("branches", branch.ID+".json"), branch); err != nil {
		return err
	}
	r.branchMu.Lock()
	delete(r.branchCache, branch.ID)
	r.branchMu.Unlock()
	return nil
}

// Branch returns a branch root.
func (r *Repository) Branch(id string) (Branch, error) {
	return r.readBranch(id)
}

// RemoveBranch removes a branch pointer after its Kernel no longer exists.
// The canonical main pointer may be removed only when another branch keeps the
// repository live. Immutable layers and objects are reclaimed separately by
// GarbageCollect.
func (r *Repository) RemoveBranch(id string) error {
	if err := validateName(id, "branch"); err != nil {
		return err
	}
	return r.withLock(func() error {
		if _, err := r.readBranch(id); err != nil {
			return err
		}
		if id == mainBranch {
			branchDir, err := r.openFile("branches", os.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				return err
			}
			names, readErr := branchDir.Readdirnames(-1)
			closeErr := branchDir.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			branchPointers := 0
			for _, name := range names {
				if strings.HasSuffix(name, ".json") {
					branchPointers++
				}
			}
			if branchPointers <= 1 {
				return errors.New("cannot remove the repository's final branch")
			}
		}
		if err := r.unlink(filepath.Join("branches", id+".json")); err != nil {
			return err
		}
		r.branchMu.Lock()
		delete(r.branchCache, id)
		r.branchMu.Unlock()
		return r.syncDirectory("branches")
	})
}

// ForgetBranch releases process-local caches for a branch whose Kernel has
// been released. It does not alter the persistent branch pointer; ttcd removes
// that pointer separately, or a later rehydration reloads it on demand.
func (r *Repository) ForgetBranch(id string) {
	if r == nil || validateName(id, "branch") != nil {
		return
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()

	r.metadataMu.Lock()
	var layers []string
	if state := r.metadata[id]; state != nil {
		state.mu.RLock()
		layers = append(layers, state.layers...)
		state.mu.RUnlock()
		delete(r.metadata, id)
	}
	r.pruneUnreferencedLayerCachesLocked(layers)
	r.metadataMu.Unlock()

	r.branchMu.Lock()
	delete(r.branchCache, id)
	r.branchMu.Unlock()
}

// ReapOrphan removes inode metadata after the Sentry has released the last
// reference to an unlinked inode. Until this point the inode must remain in the
// branch tree so live fork and durable checkpoint can reconstruct open file
// descriptions and mappings.
func (r *Repository) ReapOrphan(branchID string, ino uint64) error {
	if ino == rootInode {
		return errors.New("cannot reap the root inode")
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if inode.Nlink != 0 {
			// Filesystem teardown also drops the Sentry's in-memory link
			// references. The persistent namespace is still authoritative and
			// must remain unchanged in that case.
			return nil
		}
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataDeleteInode, Number: ino}})
	})
}

// ForkTransaction is a published but not yet committed O(1) branch fork.
//
// The source kernel must remain paused until Commit or Abort returns. This
// prevents branch mutations from racing rollback.
type ForkTransaction struct {
	repo      *Repository
	source    Branch
	published Branch
	child     Branch
	done      bool
}

// BeginFork creates child as an O(1) metadata-root fork of source. Advancing
// both branch generations ensures that either side's next regular-file write
// clones.
func (r *Repository) BeginFork(source, child string) (*ForkTransaction, error) {
	if err := validateName(child, "branch"); err != nil {
		return nil, err
	}
	var tx ForkTransaction
	err := r.withLock(func() error {
		sourceBranch, err := r.readBranch(source)
		if err != nil {
			return err
		}
		if _, err := r.readBranch(child); err == nil {
			return fmt.Errorf("branch %q already exists", child)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tx.source = sourceBranch
		// Seal the source's current writable layer by giving each resulting
		// branch a fresh writable layer. Both new manifests retain the sealed
		// layer by reference, so namespace fork cost is independent of its size.
		sharedLayer := sourceBranch.Root
		sourceLayer, err := r.createMetadataLayer(sharedLayer)
		if err != nil {
			return err
		}
		childLayer, err := r.createMetadataLayer(sharedLayer)
		if err != nil {
			return err
		}
		sourceBranch.Root = sourceLayer
		sourceBranch.Generation++
		childBranch := Branch{
			ID: child, Root: childLayer,
			Generation: sourceBranch.Generation, CreatedAt: time.Now().UTC(),
		}
		// Publish the source's new generation first. If the process crashes
		// before the child pointer appears, the only effect is a conservative
		// first-write clone in the source. Publishing the child first would let
		// the old-generation source mutate an object now visible to the child.
		if err := r.writeBranch(sourceBranch); err != nil {
			return err
		}
		if err := r.writeBranch(childBranch); err != nil {
			// writeBranch may fail after its rename succeeded (for example,
			// while fsyncing the directory). Remove the child pointer before
			// restoring the source generation; the opposite order could make a
			// shared object writable through the source while the child exists.
			childPath := filepath.Join("branches", child+".json")
			if removeErr := r.unlink(childPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return fmt.Errorf("publishing child branch: %w (removing possibly-published child also failed: %v)", err, removeErr)
			}
			if syncErr := r.syncDirectory("branches"); syncErr != nil {
				return fmt.Errorf("publishing child branch: %w (syncing child removal also failed: %v)", err, syncErr)
			}
			if rollbackErr := r.writeBranch(tx.source); rollbackErr != nil {
				return fmt.Errorf("publishing child branch: %w (restoring source branch also failed: %v)", err, rollbackErr)
			}
			return err
		}
		tx.repo = r
		tx.published = sourceBranch
		tx.child = childBranch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

// Commit makes a successfully completed fork transaction permanent.
func (tx *ForkTransaction) Commit() error {
	if tx == nil || tx.done {
		return nil
	}
	tx.done = true
	return nil
}

// Abort removes the child branch and restores the exact source branch record.
// It fails closed if either pointer changed after BeginFork.
func (tx *ForkTransaction) Abort() error {
	if tx == nil || tx.done {
		return nil
	}
	err := tx.repo.withLock(func() error {
		source, err := tx.repo.readBranch(tx.source.ID)
		if err != nil {
			return err
		}
		child, err := tx.repo.readBranch(tx.child.ID)
		if err != nil {
			return err
		}
		if source != tx.published {
			return fmt.Errorf("cannot abort fork: source branch %q changed after publication", source.ID)
		}
		if child != tx.child {
			return fmt.Errorf("cannot abort fork: child branch %q changed after publication", child.ID)
		}
		if err := tx.repo.unlink(filepath.Join("branches", tx.child.ID+".json")); err != nil {
			return err
		}
		if err := tx.repo.syncDirectory("branches"); err != nil {
			return err
		}
		// Remove the child before restoring the source generation. A crash in
		// between leaves only a conservative generation bump. Restoring the
		// source first could expose the same mutable object through a child that
		// still exists.
		return tx.repo.writeBranch(tx.source)
	})
	if err != nil {
		return err
	}
	tx.done = true
	return nil
}

// Fork creates and immediately commits an O(1) branch fork.
func (r *Repository) Fork(source, child string) error {
	tx, err := r.BeginFork(source, child)
	if err != nil {
		return err
	}
	return tx.Commit()
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
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		parentInode, err := r.metadataInode(state, parent)
		if err != nil {
			return err
		}
		if parentInode.Type != "directory" {
			return syscall.ENOTDIR
		}
		if _, err := r.metadataLookup(state, parent, name); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		object, err := r.createObject("")
		if err != nil {
			return err
		}
		created, err = r.allocateInodeLocked()
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		inode := Inode{
			Number: created, Type: "regular", Mode: mode & 07777,
			UID: uid, GID: gid, Nlink: 1, Object: object,
			DataGeneration: branch.Generation,
			AtimeNsec:      now, BtimeNsec: now, CtimeNsec: now, MtimeNsec: now,
		}
		entry := DirectoryEntry{Name: name, Inode: created}
		return r.commitMetadata(&branch, state, []metadataOp{
			{Kind: metadataSetInode, Inode: &inode},
			{Kind: metadataSetEntry, Parent: parent, Entry: &entry},
		})
	})
	return created, err
}

// CreateDirectory creates an empty directory.
func (r *Repository) CreateDirectory(branchID string, parent uint64, name string, mode uint32, uid, gid uint32) (uint64, error) {
	return r.createNonRegular(branchID, parent, name, Inode{
		Type: "directory", Mode: mode & 07777, UID: uid, GID: gid, Nlink: 2,
	})
}

// CreateSymlink creates a symbolic link.
func (r *Repository) CreateSymlink(branchID string, parent uint64, name, target string, uid, gid uint32) (uint64, error) {
	if len(target) > 4096 {
		return 0, syscall.ENAMETOOLONG
	}
	return r.createNonRegular(branchID, parent, name, Inode{
		Type: "symlink", Mode: 0777, UID: uid, GID: gid, Nlink: 1, Target: target,
	})
}

// CreateSpecial creates a FIFO, device, or unbound Unix socket inode.
func (r *Repository) CreateSpecial(branchID string, parent uint64, name, kind string, mode uint32, uid, gid, major, minor uint32) (uint64, error) {
	switch kind {
	case "fifo", "char", "block", "socket":
	default:
		return 0, fmt.Errorf("unsupported special inode type %q", kind)
	}
	return r.createNonRegular(branchID, parent, name, Inode{
		Type: kind, Mode: mode & 07777, UID: uid, GID: gid, Nlink: 1,
		DevMajor: major, DevMinor: minor,
	})
}

func (r *Repository) createNonRegular(branchID string, parent uint64, name string, inode Inode) (uint64, error) {
	if err := validateEntryName(name); err != nil {
		return 0, err
	}
	var created uint64
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		parentInode, err := r.metadataInode(state, parent)
		if err != nil {
			return err
		}
		if parentInode.Type != "directory" {
			return syscall.ENOTDIR
		}
		if _, err := r.metadataLookup(state, parent, name); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created, err = r.allocateInodeLocked()
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		inode.Number = created
		inode.AtimeNsec = now
		inode.BtimeNsec = now
		inode.CtimeNsec = now
		inode.MtimeNsec = now
		ops := []metadataOp{{Kind: metadataSetInode, Inode: &inode}}
		if inode.Type == "directory" {
			parentInode.Nlink++
			ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &parentInode})
		}
		entry := DirectoryEntry{Name: name, Inode: created}
		ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: parent, Entry: &entry})
		return r.commitMetadata(&branch, state, ops)
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
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return 0, err
	}
	return r.metadataLookup(state, parent, name)
}

// Inode returns branch-local inode metadata.
func (r *Repository) Inode(branchID string, ino uint64) (Inode, error) {
	branch, err := r.readBranch(branchID)
	if err != nil {
		return Inode{}, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return Inode{}, err
	}
	return r.metadataInode(state, ino)
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
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, target)
		if err != nil {
			return err
		}
		if inode.Type == "directory" {
			return syscall.EPERM
		}
		if _, err := r.metadataLookup(state, parent, name); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		inode.Nlink++
		entry := DirectoryEntry{Name: name, Inode: target}
		return r.commitMetadata(&branch, state, []metadataOp{
			{Kind: metadataSetInode, Inode: &inode},
			{Kind: metadataSetEntry, Parent: parent, Entry: &entry},
		})
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
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		ino, err := r.metadataLookup(state, parent, name)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		if inode.Type == "directory" {
			return syscall.EISDIR
		}
		if inode.Nlink > 1 {
			inode.Nlink--
		} else {
			// Keep zero-link inode metadata in the immutable branch tree.
			// Sentry may still hold an open file description or mapping, and a
			// checkpoint must retain the inode's backing object. The Sentry
			// reaps it after the final in-memory reference is released.
			inode.Nlink = 0
		}
		return r.commitMetadata(&branch, state, []metadataOp{
			{Kind: metadataDeleteEntry, Parent: parent, Name: name},
			{Kind: metadataSetInode, Inode: &inode},
		})
	})
}

// RemoveDirectory removes an empty directory.
func (r *Repository) RemoveDirectory(branchID string, parent uint64, name string) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		ino, err := r.metadataLookup(state, parent, name)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		if inode.Type != "directory" {
			return syscall.ENOTDIR
		}
		entries := r.metadataDirectory(state, ino)
		if len(entries) != 0 {
			return syscall.ENOTEMPTY
		}
		parentInode, err := r.metadataInode(state, parent)
		if err != nil {
			return err
		}
		if parentInode.Type != "directory" {
			return syscall.ENOTDIR
		}
		// An open directory remains observable after unlink. Retain its inode
		// metadata until repository GC, just like regular-file orphans.
		inode.Nlink = 0
		if parentInode.Nlink > 0 {
			parentInode.Nlink--
		}
		return r.commitMetadata(&branch, state, []metadataOp{
			{Kind: metadataDeleteEntry, Parent: parent, Name: name},
			{Kind: metadataSetInode, Inode: &inode},
			{Kind: metadataSetInode, Inode: &parentInode},
		})
	})
}

// Rename atomically moves oldName, optionally replacing newName. Callers are
// responsible for rejecting directory ancestry cycles before invoking it.
func (r *Repository) Rename(branchID string, oldParent uint64, oldName string, newParent uint64, newName string, noReplace bool) error {
	if err := validateEntryName(oldName); err != nil {
		return err
	}
	if err := validateEntryName(newName); err != nil {
		return err
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		oldParentInode, err := r.metadataInode(state, oldParent)
		if err != nil {
			return err
		}
		newParentInode, err := r.metadataInode(state, newParent)
		if err != nil {
			return err
		}
		if oldParentInode.Type != "directory" || newParentInode.Type != "directory" {
			return syscall.ENOTDIR
		}
		movedIno, err := r.metadataLookup(state, oldParent, oldName)
		if err != nil {
			return err
		}
		moved, err := r.metadataInode(state, movedIno)
		if err != nil {
			return err
		}
		replacedIno, replacedErr := r.metadataLookup(state, newParent, newName)
		if replacedErr == nil && noReplace {
			return os.ErrExist
		}
		if replacedErr != nil && !errors.Is(replacedErr, os.ErrNotExist) {
			return replacedErr
		}
		if replacedErr == nil && replacedIno == movedIno {
			return nil
		}
		ops := make([]metadataOp, 0, 6)
		if replacedErr == nil {
			replaced, err := r.metadataInode(state, replacedIno)
			if err != nil {
				return err
			}
			switch {
			case moved.Type == "directory" && replaced.Type != "directory":
				return syscall.ENOTDIR
			case moved.Type != "directory" && replaced.Type == "directory":
				return syscall.EISDIR
			case replaced.Type == "directory":
				entries := r.metadataDirectory(state, replacedIno)
				if len(entries) != 0 {
					return syscall.ENOTEMPTY
				}
			}
			ops = append(ops, metadataOp{Kind: metadataDeleteEntry, Parent: newParent, Name: newName})
			if replaced.Nlink > 1 && replaced.Type != "directory" {
				replaced.Nlink--
			} else {
				// Preserve a replaced-but-open inode for live fork and durable
				// checkpoint restoration. It is no longer reachable by name.
				replaced.Nlink = 0
			}
			ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &replaced})
			if replaced.Type == "directory" && newParentInode.Nlink > 0 {
				newParentInode.Nlink--
			}
		}
		ops = append(ops, metadataOp{Kind: metadataDeleteEntry, Parent: oldParent, Name: oldName})
		newEntry := DirectoryEntry{Name: newName, Inode: movedIno}
		ops = append(ops, metadataOp{Kind: metadataSetEntry, Parent: newParent, Entry: &newEntry})
		if moved.Type == "directory" && oldParent != newParent {
			if oldParentInode.Nlink > 0 {
				oldParentInode.Nlink--
			}
			newParentInode.Nlink++
		}
		if oldParent == newParent {
			oldParentInode = newParentInode
		}
		ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &oldParentInode})
		if newParent != oldParent {
			ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &newParentInode})
		}
		return r.commitMetadata(&branch, state, ops)
	})
}

// UpdateMetadata replaces branch-local metadata fields while retaining
// identity, type, link count, data object and generation.
func (r *Repository) UpdateMetadata(branchID string, updated Inode) error {
	return r.updateMetadata(branchID, updated, false)
}

// UpdateAfterWrite replaces metadata and grows the logical size. The size is
// merged using max so writers through concurrent mounts of one named volume
// cannot accidentally shrink each other's file.
func (r *Repository) UpdateAfterWrite(branchID string, updated Inode) error {
	return r.updateMetadata(branchID, updated, true)
}

func (r *Repository) updateMetadata(branchID string, updated Inode, growSize bool) error {
	return r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		current, err := r.metadataInode(state, updated.Number)
		if err != nil {
			return err
		}
		current.Mode = updated.Mode & 07777
		current.UID = updated.UID
		current.GID = updated.GID
		current.AtimeNsec = updated.AtimeNsec
		current.BtimeNsec = updated.BtimeNsec
		current.CtimeNsec = updated.CtimeNsec
		current.MtimeNsec = updated.MtimeNsec
		current.Xattrs = updated.Xattrs
		if current.Type == "regular" && growSize && updated.Size > current.Size {
			current.Size = updated.Size
		} else if current.Type == "symlink" {
			current.Target = updated.Target
		}
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &current}})
	})
}

func (r *Repository) ensurePrivate(branch *Branch, state *branchMetadata, inode *Inode) (bool, error) {
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
	if err := r.commitMetadata(branch, state, []metadataOp{{Kind: metadataSetInode, Inode: inode}}); err != nil {
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
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		cloned, err = r.ensurePrivate(&branch, state, &inode)
		if err != nil {
			return err
		}
		file, err := r.openObject(inode.Object, os.O_WRONLY)
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
			return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &inode}})
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
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return 0, err
	}
	inode, err := r.metadataInode(state, ino)
	if err != nil {
		return 0, err
	}
	if inode.Type != "regular" {
		return 0, syscall.EINVAL
	}
	if !validID(inode.Object) {
		return 0, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
	}
	file, err := r.openObject(inode.Object, os.O_RDONLY)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.ReadAt(data, offset)
}

// OpenDataFile opens the host data object selected by branchID and ino.
// Writable opens first pass through the branch-generation CoW gate. Callers
// must close the returned descriptor and must revoke every writable descriptor
// and mapping before BeginFork.
func (r *Repository) OpenDataFile(branchID string, ino uint64, writable bool) (*os.File, Inode, bool, error) {
	var (
		file   *os.File
		inode  Inode
		cloned bool
	)
	open := func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err = r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		if inode.Type != "regular" {
			return syscall.EINVAL
		}
		if writable {
			cloned, err = r.ensurePrivate(&branch, state, &inode)
			if err != nil {
				return err
			}
		}
		flags := os.O_RDONLY
		if writable {
			flags = os.O_RDWR
		}
		file, err = r.openObject(inode.Object, flags)
		return err
	}
	var err error
	if writable {
		err = r.withLock(open)
	} else {
		err = open()
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return nil, Inode{}, false, err
	}
	return file, inode, cloned, nil
}

// SyncData makes the selected data object durable.
func (r *Repository) SyncData(branchID string, ino uint64, dataOnly bool) error {
	file, _, _, err := r.OpenDataFile(branchID, ino, false)
	if err != nil {
		return err
	}
	defer file.Close()
	if dataOnly {
		return unix.Fdatasync(int(file.Fd()))
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

// SyncMetadata makes repository namespace and inode metadata durable.
func (r *Repository) SyncMetadata() error {
	return r.withLock(r.syncMetadataLocked)
}

// syncMetadataLocked persists files before the directories that make their
// current names reachable. Repository mutations and this flush are serialized
// by the repository flock, so a successful call covers every mutation that
// completed before it began.
func (r *Repository) syncMetadataLocked() error {
	r.dirtyMu.Lock()
	var journals, rootDirty, branchDirty []string
	for name := range r.dirtyFiles {
		switch {
		case strings.HasSuffix(name, "/journal"):
			journals = append(journals, name)
		case strings.HasPrefix(name, "branches/"):
			branchDirty = append(branchDirty, name)
		default:
			rootDirty = append(rootDirty, name)
		}
	}
	dirtyDirs := make([]string, 0, len(r.dirtyDirs))
	for name := range r.dirtyDirs {
		dirtyDirs = append(dirtyDirs, name)
	}
	r.dirtyMu.Unlock()
	sort.Strings(journals)
	sort.Strings(rootDirty)
	sort.Strings(branchDirty)
	// Children must precede parents so every object name is durable before a
	// journal transaction that makes it reachable.
	sort.Slice(dirtyDirs, func(i, j int) bool {
		return strings.Count(dirtyDirs[i], string(filepath.Separator)) > strings.Count(dirtyDirs[j], string(filepath.Separator))
	})
	syncFiles := func(names []string) error {
		for _, name := range names {
			file, err := r.openFile(name, os.O_RDONLY, 0)
			if err != nil {
				return err
			}
			err = file.Sync()
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	}
	for _, name := range dirtyDirs {
		if name == "." {
			continue
		}
		if err := r.syncDirectory(name); err != nil {
			return err
		}
	}
	if err := syncFiles(rootDirty); err != nil {
		return err
	}
	if err := syncFiles(journals); err != nil {
		return err
	}
	if err := syncFiles(branchDirty); err != nil {
		return err
	}
	if err := r.syncDirectory("branches"); err != nil {
		return err
	}
	if err := r.dir.Sync(); err != nil {
		return err
	}
	r.dirtyMu.Lock()
	for name := range r.dirtyFiles {
		delete(r.dirtyFiles, name)
	}
	for name := range r.dirtyDirs {
		delete(r.dirtyDirs, name)
	}
	r.dirtyMu.Unlock()
	return nil
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
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		cloned, err = r.ensurePrivate(&branch, state, &inode)
		if err != nil {
			return err
		}
		file, err := r.openObject(inode.Object, os.O_WRONLY)
		if err != nil {
			return err
		}
		err = file.Truncate(size)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		inode.Size = size
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &inode}})
	})
	return cloned, err
}

// Allocate reserves host filesystem space for a regular file. The only mode
// currently exposed by gVisor's fallocate syscall is mode 0, which also grows
// the logical file to offset+length.
func (r *Repository) Allocate(branchID string, ino uint64, offset, length int64) (bool, error) {
	if offset < 0 || length <= 0 || offset > int64(^uint64(0)>>1)-length {
		return false, syscall.EINVAL
	}
	var cloned bool
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return err
		}
		inode, err := r.metadataInode(state, ino)
		if err != nil {
			return err
		}
		cloned, err = r.ensurePrivate(&branch, state, &inode)
		if err != nil {
			return err
		}
		file, err := r.openObject(inode.Object, os.O_RDWR)
		if err != nil {
			return err
		}
		err = unix.Fallocate(int(file.Fd()), 0, offset, length)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		end := offset + length
		if end <= inode.Size {
			return nil
		}
		inode.Size = end
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &inode}})
	})
	return cloned, err
}

// ListDirectory returns entries in deterministic name order.
func (r *Repository) ListDirectory(branchID string, parent uint64) ([]DirectoryEntry, error) {
	branch, err := r.readBranch(branchID)
	if err != nil {
		return nil, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return nil, err
	}
	return r.metadataDirectory(state, parent), nil
}

func (r *Repository) createObject(source string) (string, error) {
	if source != "" && !validID(source) {
		return "", fmt.Errorf("invalid source object ID %q", source)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	destination := r.objectPath(id)
	if err := r.mkdirAll(filepath.Dir(destination), 0700); err != nil {
		return "", err
	}
	dst, err := r.openFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		dst.Close()
		if !keep {
			_ = r.unlink(destination)
		}
	}()
	if source != "" {
		src, err := r.openObject(source, os.O_RDONLY)
		if err != nil {
			return "", err
		}
		switch r.copyMode {
		case CopyModeReflink:
			err = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
		case CopyModeFull:
			// Avoid os.File's copy_file_range fast path. Full-copy mode is the
			// explicit portability fallback and the Sentry's host seccomp
			// policy intentionally exposes only read/write for copying data.
			buf := make([]byte, 256<<10)
			for {
				n, readErr := src.Read(buf)
				if n != 0 {
					if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
						err = writeErr
						break
					}
				}
				if readErr != nil {
					if readErr != io.EOF {
						err = readErr
					}
					break
				}
			}
		default:
			err = fmt.Errorf("unknown volume copy mode %q", r.copyMode)
		}
		src.Close()
		if err != nil {
			return "", fmt.Errorf("privatizing volume object %q using %s: %w", source, r.copyMode, err)
		}
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	// The object name is synchronized before any metadata journal that refers
	// to it. Avoid forcing an fsync for every empty file creation.
	r.markDirtyDirectory(filepath.Dir(destination))
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

func (r *Repository) openFile(name string, flags int, mode fs.FileMode) (*os.File, error) {
	if r == nil || r.dir == nil {
		return nil, errors.New("repository is closed")
	}
	if err := validateInternalPath(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(r.dir.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (r *Repository) readFile(name string) ([]byte, error) {
	file, err := r.openFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (r *Repository) readJSON(name string, value any) error {
	data, err := r.readFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decoding repository file %q: %w", name, err)
	}
	return nil
}

func (r *Repository) writeJSONAtomic(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.atomicWriteFile(name, append(data, '\n'), 0600)
}

func (r *Repository) atomicWriteFile(name string, data []byte, mode fs.FileMode) error {
	if err := validateInternalPath(name); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	staging := filepath.Join("staging", "publish-"+id)
	file, err := r.openFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		file.Close()
		if !keep {
			_ = r.unlink(staging)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(r.dir.Fd()), staging, int(r.dir.Fd()), name); err != nil {
		return err
	}
	r.dirtyMu.Lock()
	if r.dirtyFiles == nil {
		r.dirtyFiles = make(map[string]struct{})
	}
	r.dirtyFiles[name] = struct{}{}
	r.dirtyMu.Unlock()
	keep = true
	return nil
}

func (r *Repository) unlink(name string) error {
	if err := validateInternalPath(name); err != nil {
		return err
	}
	return unix.Unlinkat(int(r.dir.Fd()), name, 0)
}

func (r *Repository) syncDirectory(name string) error {
	if name == "." {
		return r.dir.Sync()
	}
	dir, err := r.openFile(name, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateInternalPath(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return fmt.Errorf("invalid repository path %q", name)
	}
	clean := filepath.Clean(name)
	if clean != name || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid repository path %q", name)
	}
	return nil
}
