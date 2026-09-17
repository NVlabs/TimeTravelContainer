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
// repository. Namespace metadata is stored in the immutable snapshot graph;
// regular-file contents use graph-identified whole-file objects.
package ttcvolume

import (
	"archive/tar"
	"context"
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
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	formatVersion = 3
	mainBranch    = "main"
	rootInode     = uint64(1)

	// Bound obsolete whole-file payloads and graph WAL growth even when a
	// workload never calls fsync(2). The durability fence is amortized across a
	// large batch rather than imposed on every namespace mutation.
	reclaimBatchObjects = 512
	reclaimJournalBytes = 8 << 20
)

// ErrFinalBranch indicates that deleting a branch would leave a repository
// without an authoritative root. Family-private repository owners may respond
// by removing the repository itself; shared named-volume callers must retain
// its final branch.
var ErrFinalBranch = errors.New("cannot remove the repository's final branch")

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
	// CopyModeFull copies the complete file when FICLONE is unavailable.
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

// OpenManager creates the volume store and detects its file-copy policy.
// FICLONE is an optimization only; filesystems that do not support it use
// whole-file copies without requiring daemon configuration.
func OpenManager(dataRoot string) (*Manager, error) {
	if dataRoot == "" {
		return nil, errors.New("volume manager data root must not be empty")
	}
	volumes := filepath.Join(dataRoot, "filesystems", "volumes")
	if err := os.MkdirAll(volumes, 0700); err != nil {
		return nil, fmt.Errorf("creating volume object store: %w", err)
	}
	capability, err := probeCapability(volumes)
	if err != nil {
		return nil, err
	}
	return &Manager{root: dataRoot, volumes: volumes, capability: capability}, nil
}

// Capability returns the verified copy policy for this manager.
func (m *Manager) Capability() Capability {
	return m.capability
}

func probeCapability(root string) (Capability, error) {
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
		capability.Mode = CopyModeFull
		return capability, nil
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

// Branch identifies one immutable filesystem root and the exclusive working
// snapshot origin used by its next metadata and whole-file CoW mutations.
type Branch struct {
	ID                    string `json:"id"`
	Root                  string `json:"root"`
	BaseRoot              string `json:"base_root,omitempty"`
	RemoteCatalog         string `json:"remote_catalog,omitempty"`
	RemoteCatalogSnapshot uint64 `json:"remote_catalog_snapshot,omitempty"`
	// RemoteLogicalBytes is the constant-time byte aggregate from the
	// authoritative foreign filesystem root. It is needed only while Root is
	// still an unresolved deferred stub; after the root is expanded, its graph
	// metadata carries the current aggregate directly.
	RemoteLogicalBytes uint64    `json:"remote_logical_bytes,omitempty"`
	WorkingSnapshot    uint64    `json:"working_snapshot"`
	Generation         uint64    `json:"generation"`
	CreatedAt          time.Time `json:"created_at"`
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

// DirectoryItem combines a directory entry with its inode metadata. It lets
// filesystem implementations serve readdir without instantiating or looking
// up each child individually.
type DirectoryItem struct {
	Entry DirectoryEntry
	Inode Inode
}

// PathChange describes one namespace path changed in a branch's writable
// metadata layer relative to the immutable layer it inherited at the most
// recent fork. Kind is add, modify, or delete.
type PathChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Type string `json:"type,omitempty"`
}

// Repository is one named persistent volume.
type Repository struct {
	path     string
	dir      *os.File
	copyMode CopyMode
	info     VolumeInfo
	graph    *ttcsnapshot.Store
	data     *ttcsnapshot.LocalDataStore

	// Ordinary Linux namespace mutations are not durable until fsync(2),
	// sync(2), or filesystem teardown. Track journal and pointer files that
	// still need their contents made durable.
	dirtyMu    sync.Mutex
	dirtyFiles map[string]struct{}
	dirtyDirs  map[string]struct{}
	// dirtyBranches names branch pointer files whose Root field trails the
	// authoritative graph root. Publishing these on fsync/lifecycle fences
	// avoids an atomic temp-file+rename for every guest inode mutation.
	dirtyBranches map[string]*branchMetadata
	// dirtyData is a per-object mutation generation. Object installation and
	// subsequent writes increment it; SyncMetadata clears only the generation
	// it actually synchronized so a concurrent write cannot be lost from the
	// next durability fence.
	dirtyData map[string]uint64
	// pendingMetadata coalesces ordinary guest metadata mutations into bounded
	// logical graph transactions. The live branchMetadata view is updated
	// synchronously, while at most metadataBatchOperations operations remain to
	// be path-copied into the immutable graph at a fork or durability fence.
	pendingMetadata map[string]*metadataBatch
	// File IDs are reserved in bounded groups and complete installed bulk
	// objects are published to the graph together at the next metadata-batch
	// fence. This removes two graph journal transactions from every create(2).
	reservedFileIDs map[uint64][]ttcsnapshot.ObjectID
	pendingBulk     map[string]ttcsnapshot.ReservedObject
	// pendingReclaims contains bulk objects whose graph records have been
	// removed by committed path-copy mutations. Their bytes cannot be unlinked
	// until the graph WAL is durable: crash recovery may otherwise select the
	// preceding root and find its payload missing.
	pendingReclaims map[string]ttcsnapshot.ObjectID
	knownDirs       map[string]struct{}

	// metadata is the decoded view of each locally-used branch. The journal is
	// authoritative and is replayed when a branch first becomes active.
	metadataMu sync.Mutex
	metadata   map[string]*branchMetadata
	allocNext  uint64
	allocEnd   uint64

	// viewMu keeps a read-side metadata traversal on one immutable graph
	// generation. Writers replace a branch root and may immediately reclaim the
	// preceding path-copied nodes; without this barrier a reader can select the
	// old root immediately before it disappears. Readers remain concurrent with
	// each other, and bulk data I/O continues after this short metadata section.
	viewMu      sync.RWMutex
	operationMu sync.Mutex
	lockFile    *os.File
	// activeLock holds a shared lifecycle lease for descriptor-backed
	// repositories donated to a live Sentry. Manager-side garbage collection
	// takes the same file exclusively and non-blockingly, so it cannot mistake
	// the Sentry's intentionally batched in-memory graph tail for unreachable
	// data. Path-backed manager repositories do not retain this lease.
	activeLock *os.File

	branchMu          sync.Mutex
	branchCache       map[string]cachedBranch
	remoteLifecycleMu sync.Mutex
	remoteMu          sync.Mutex
	remotes           map[*remoteObjectClient]struct{}
	remoteByObject    map[string]*remoteObjectClient
	remoteByCatalog   map[string]*remoteObjectClient

	// materializationMu serializes installation of remotely inherited objects
	// with runtime backing-cache invalidation. Hooks are invoked while this
	// mutex is held, immediately before the complete object is installed.
	materializationMu       sync.Mutex
	materializationHooks    map[uint64]func(string)
	nextMaterializationHook uint64
}

// RegisterMaterializationHook registers a callback that revokes runtime-local
// file descriptors and mappings backed by object before remote materialization
// replaces its placeholder inode. The returned function unregisters the hook
// and waits for an in-flight invocation to finish.
func (r *Repository) RegisterMaterializationHook(hook func(object string)) func() {
	if hook == nil {
		return func() {}
	}
	r.materializationMu.Lock()
	if r.materializationHooks == nil {
		r.materializationHooks = make(map[uint64]func(string))
	}
	r.nextMaterializationHook++
	id := r.nextMaterializationHook
	r.materializationHooks[id] = hook
	r.materializationMu.Unlock()
	return func() {
		r.materializationMu.Lock()
		delete(r.materializationHooks, id)
		r.materializationMu.Unlock()
	}
}

func (r *Repository) replaceRemoteObject(id ttcsnapshot.ObjectID, object string, reader io.Reader, size int64) error {
	r.materializationMu.Lock()
	defer r.materializationMu.Unlock()
	for _, hook := range r.materializationHooks {
		hook(object)
	}
	return r.data.ReplaceDeferred(id, reader, size)
}

type cachedBranch struct {
	branch    Branch
	device    uint64
	inode     uint64
	size      int64
	mtimeNsec int64
}

// VolatileOrigins returns every sealed provider-owned origin in deterministic
// order. Working origins are mutable and shared origins are already durable.
func (r *Repository) VolatileOrigins() []ttcsnapshot.SnapshotID {
	inspection := r.graph.Inspect()
	origins := make([]ttcsnapshot.SnapshotID, 0, len(inspection.Snapshots))
	for _, snapshot := range inspection.Snapshots {
		if snapshot.State == ttcsnapshot.SnapshotSealedVolatile {
			origins = append(origins, snapshot.ID)
		}
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	return origins
}

// SnapshotOrigins returns every origin represented by this repository's
// authoritative graph. This includes working origins: in a cluster their IDs
// have already been allocated by Raft even though their objects remain mutable.
// Callers use this inventory to reconcile provider-directory entries after a
// crash or repository removal; it does not expose object-level references.
func (r *Repository) SnapshotOrigins() []ttcsnapshot.SnapshotID {
	inspection := r.graph.Inspect()
	origins := make([]ttcsnapshot.SnapshotID, 0, len(inspection.Snapshots))
	for _, snapshot := range inspection.Snapshots {
		origins = append(origins, snapshot.ID)
	}
	return origins
}

// PrepareBranchPublication makes every local origin reachable from branch's
// immutable root publishable, then returns the provider-owned origins in
// deterministic order. A lazily imported filesystem uses a separate working
// catalog origin for bounded metadata expansion. Before a durable root may
// reference that catalog, all remaining remote objects are materialized and
// the catalog is sealed. This is an asynchronous publication operation; it is
// deliberately outside the stop/dehydration capture fence.
//
// Materialization is repository-wide because locally forked branches may
// share the same catalog origin. Once complete, sealing that origin is safe:
// all of its objects are immutable and no branch needs further expansion.
func (r *Repository) PrepareBranchPublication(branchID string) ([]ttcsnapshot.SnapshotID, error) {
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	if err := r.materializeRemoteObjects(); err != nil {
		return nil, fmt.Errorf("materializing lazy filesystem dependencies: %w", err)
	}
	var origins []ttcsnapshot.SnapshotID
	err := r.withLock(func() error {
		branch, err := r.readBranch(branchID)
		if err != nil {
			return err
		}
		// A manager-side publisher may open the repository after the Sentry has
		// completed its independence barrier. It has no live remote client to
		// detach, so retire the durable catalog marker only after independently
		// proving that every branch sharing it has local namespace metadata and
		// file bytes. The catalog expansion origin is repository-wide; leaving a
		// sibling bound to it before sealing would make the sibling's next fork
		// attempt to mutate an immutable origin.
		if branch.RemoteCatalog != "" {
			catalog := branch.RemoteCatalog
			directory, err := r.openFile("branches", os.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				return err
			}
			entries, readErr := directory.ReadDir(-1)
			closeErr := directory.Close()
			if readErr != nil || closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			var independent []Branch
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				id := strings.TrimSuffix(entry.Name(), ".json")
				if validateName(id, "branch") != nil {
					continue
				}
				candidate, err := r.readBranch(id)
				if err != nil {
					return err
				}
				if candidate.RemoteCatalog != catalog {
					continue
				}
				objects, err := r.branchObjects(candidate)
				if err != nil {
					return fmt.Errorf("proving branch %q filesystem independence: %w", candidate.ID, err)
				}
				for object := range objects {
					pending, err := r.remoteObjectPending(object)
					if err != nil {
						return err
					}
					if pending {
						return fmt.Errorf("branch %q still depends on remote filesystem object %s", candidate.ID, object)
					}
				}
				independent = append(independent, candidate)
			}
			for _, candidate := range independent {
				candidate.RemoteCatalog = ""
				candidate.RemoteCatalogSnapshot = 0
				candidate.RemoteLogicalBytes = 0
				if err := r.writeBranch(candidate); err != nil {
					return fmt.Errorf("retiring branch %q remote filesystem catalog: %w", candidate.ID, err)
				}
				if candidate.ID == branchID {
					branch = candidate
				}
			}
		}
		root, ok := r.graph.Root(graphRootName(branchID))
		if !ok {
			return os.ErrNotExist
		}
		wanted := make(map[ttcsnapshot.SnapshotID]struct{})
		dependencies := make(map[ttcsnapshot.SnapshotID]map[ttcsnapshot.SnapshotID]struct{})
		seen := make(map[ttcsnapshot.ObjectID]struct{})
		stack := []ttcsnapshot.ObjectID{root}
		for len(stack) != 0 {
			index := len(stack) - 1
			id := stack[index]
			stack = stack[:index]
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			object, ok := r.graph.Object(id)
			if !ok || object.State != ttcsnapshot.ObjectLive || object.Deferred {
				return fmt.Errorf("filesystem graph object %s is missing, deferred, or not live", id)
			}
			snapshot, ok := r.graph.Snapshot(id.Snapshot)
			if !ok {
				return fmt.Errorf("filesystem graph object %s has no origin", id)
			}
			switch snapshot.State {
			case ttcsnapshot.SnapshotWorking:
				// The branch's writable origin must have been sealed by its
				// capture transaction before asynchronous publication begins.
				// A distinct reachable working origin is the lazy catalog.
				if id.Snapshot == ttcsnapshot.SnapshotID(branch.WorkingSnapshot) {
					return fmt.Errorf("filesystem root still references writable origin %d", id.Snapshot)
				}
				if err := r.graph.Seal(id.Snapshot, ttcsnapshot.SnapshotSealedVolatile); err != nil {
					return fmt.Errorf("sealing filesystem catalog origin %d: %w", id.Snapshot, err)
				}
				wanted[id.Snapshot] = struct{}{}
			case ttcsnapshot.SnapshotSealedVolatile:
				wanted[id.Snapshot] = struct{}{}
			case ttcsnapshot.SnapshotShared:
				// Shared origins are already durable and are retained through
				// explicit cross-origin edges in the shared reference ledger.
			case ttcsnapshot.SnapshotSealedLocal:
				return fmt.Errorf("cluster publication reached standalone origin %d", id.Snapshot)
			default:
				return fmt.Errorf("filesystem graph origin %d is %s", id.Snapshot, snapshot.State)
			}
			for _, edge := range object.Children {
				if edge.Child.Snapshot != id.Snapshot {
					if dependencies[id.Snapshot] == nil {
						dependencies[id.Snapshot] = make(map[ttcsnapshot.SnapshotID]struct{})
					}
					dependencies[id.Snapshot][edge.Child.Snapshot] = struct{}{}
				}
				stack = append(stack, edge.Child)
			}
			if !object.SourceObject.IsZero() {
				// SourceObject identifies a foreign immutable alias. It is not
				// necessarily present in the node-local graph, and publication
				// path-compresses it through the shared/peer source.
				if _, local := r.graph.Object(object.SourceObject); local {
					if object.SourceObject.Snapshot != id.Snapshot {
						if dependencies[id.Snapshot] == nil {
							dependencies[id.Snapshot] = make(map[ttcsnapshot.SnapshotID]struct{})
						}
						dependencies[id.Snapshot][object.SourceObject.Snapshot] = struct{}{}
					}
					stack = append(stack, object.SourceObject)
				}
			}
		}
		// Handoff dependencies before their parents. Otherwise importing a
		// parent into the shared ledger may need to retain a child that is still
		// provider-owned, forcing a transient cross-store bridge and deadlocking
		// publication before the child can itself become shared.
		ordered := make([]ttcsnapshot.SnapshotID, 0, len(wanted))
		for origin := range wanted {
			ordered = append(ordered, origin)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		visiting := make(map[ttcsnapshot.SnapshotID]bool)
		visited := make(map[ttcsnapshot.SnapshotID]bool)
		var visit func(ttcsnapshot.SnapshotID) error
		visit = func(origin ttcsnapshot.SnapshotID) error {
			if visited[origin] {
				return nil
			}
			if visiting[origin] {
				return fmt.Errorf("filesystem snapshot origins contain a dependency cycle at %d", origin)
			}
			visiting[origin] = true
			children := make([]ttcsnapshot.SnapshotID, 0, len(dependencies[origin]))
			for child := range dependencies[origin] {
				if _, publish := wanted[child]; publish {
					children = append(children, child)
				}
			}
			sort.Slice(children, func(i, j int) bool { return children[i] < children[j] })
			for _, child := range children {
				if err := visit(child); err != nil {
					return err
				}
			}
			visiting[origin] = false
			visited[origin] = true
			origins = append(origins, origin)
			return nil
		}
		for _, origin := range ordered {
			if err := visit(origin); err != nil {
				return err
			}
		}
		return nil
	})
	return origins, err
}

// BranchVolatileOrigins reports the already-sealed provider-owned immutable
// origins reachable from branch. It performs no materialization or sealing;
// callers preparing a durable publication must use PrepareBranchPublication.
func (r *Repository) BranchVolatileOrigins(branchID string) ([]ttcsnapshot.SnapshotID, error) {
	root, ok := r.graph.Root(graphRootName(branchID))
	if !ok {
		return nil, os.ErrNotExist
	}
	wanted := make(map[ttcsnapshot.SnapshotID]struct{})
	seen := make(map[ttcsnapshot.ObjectID]struct{})
	stack := []ttcsnapshot.ObjectID{root}
	for len(stack) != 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		object, ok := r.graph.Object(id)
		if !ok {
			// A deferred source already owned by shared storage is deliberately
			// absent from this node-local cache graph. Shared publication resolves
			// that immutable identity from its pack when path-compressing the stub.
			continue
		}
		if object.State != ttcsnapshot.ObjectLive {
			return nil, fmt.Errorf("filesystem graph object %s is missing or not live", id)
		}
		snapshot, ok := r.graph.Snapshot(id.Snapshot)
		if ok && snapshot.State == ttcsnapshot.SnapshotSealedVolatile {
			wanted[id.Snapshot] = struct{}{}
		}
		for _, edge := range object.Children {
			stack = append(stack, edge.Child)
		}
		if object.Deferred && !object.SourceObject.IsZero() {
			stack = append(stack, object.SourceObject)
		}
	}
	origins := make([]ttcsnapshot.SnapshotID, 0, len(wanted))
	for origin := range wanted {
		origins = append(origins, origin)
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	return origins, nil
}

// PublishOriginHandoff writes one immutable repository origin to the common
// shared pack store while retaining provider-side reference authority.
func (r *Repository) PublishOriginHandoff(ctx context.Context, shared *ttcsnapshot.SharedPackStore, origin ttcsnapshot.SnapshotID) error {
	if shared == nil {
		return errors.New("shared snapshot pack store is unavailable")
	}
	if err := r.SyncMetadata(); err != nil {
		return err
	}
	// Install the handoff baseline while retaining the same cross-process
	// writer fence as live Sentry mutations. The bulk pack write below is
	// intentionally outside this short fence; reference changes that race it
	// are recorded in the handoff suffix and consumed by FreezeOriginHandoff.
	if err := r.withLock(func() error {
		_, _, _, err := r.graph.BeginSnapshotLedgerHandoff(origin)
		return err
	}); err != nil {
		return err
	}
	return shared.PublishHandoff(ctx, r.graph, origin, r.data)
}

// FreezeOriginHandoff closes the provider reference-mutation suffix.
func (r *Repository) FreezeOriginHandoff(origin ttcsnapshot.SnapshotID) ([]ttcsnapshot.SnapshotReferenceMutation, error) {
	var changes []ttcsnapshot.SnapshotReferenceMutation
	err := r.withLock(func() error {
		if _, _, err := r.graph.FreezeSnapshotLedger(origin); err != nil {
			return err
		}
		var frozen bool
		var err error
		changes, _, frozen, err = r.graph.SnapshotLedgerHandoffChanges(origin, 0)
		if err != nil {
			return err
		}
		if !frozen {
			return fmt.Errorf("filesystem snapshot origin %d handoff is not frozen", origin)
		}
		return nil
	})
	return changes, err
}

// CompleteOriginHandoff changes the local graph into a non-authoritative
// shared cache view after Raft has committed the provider-to-shared switch.
func (r *Repository) CompleteOriginHandoff(origin ttcsnapshot.SnapshotID) error {
	return r.withLock(func() error {
		if err := r.graph.MarkShared(origin); err != nil {
			return err
		}
		return r.graph.UnfreezeSnapshotLedger(origin)
	})
}

// CancelOriginHandoff restores ordinary provider-side mutation and collection
// after a publication was canceled before Raft routed origin to shared
// storage. It is idempotent so crash recovery may repeat cleanup.
func (r *Repository) CancelOriginHandoff(origin ttcsnapshot.SnapshotID) error {
	return r.withLock(func() error {
		return r.graph.UnfreezeSnapshotLedger(origin)
	})
}

// OriginEmpty reports whether origin owns no remaining graph objects. Missing
// origin metadata is equivalent to an already-collected empty origin.
func (r *Repository) OriginEmpty(origin ttcsnapshot.SnapshotID) (bool, error) {
	objects, err := r.graph.SnapshotObjects(origin)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return len(objects) == 0, err
}

// Create creates a new volume with an empty main branch.
func (m *Manager) Create(name string) (*Repository, error) {
	return m.create(name, 0)
}

// CreateWithOrigin creates a named clustered repository whose initial working
// origin was already allocated by the cluster authority state machine.
func (m *Manager) CreateWithOrigin(name string, working ttcsnapshot.SnapshotID) (*Repository, error) {
	if working == 0 {
		return nil, errors.New("clustered repository requires an allocated working snapshot origin")
	}
	return m.create(name, working)
}

func (m *Manager) create(name string, working ttcsnapshot.SnapshotID) (*Repository, error) {
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
	return m.createAt(path, name, id, working)
}

// CreatePrivate creates a repository at path without adding it to the named
// volume catalog. TTC uses this for a container-family-owned rootfs upper.
func (m *Manager) CreatePrivate(path, name string) (*Repository, error) {
	return m.createPrivate(path, name, 0)
}

// CreatePrivateWithOrigin creates a family-private clustered repository using
// an initial working origin committed by Raft.
func (m *Manager) CreatePrivateWithOrigin(path, name string, working ttcsnapshot.SnapshotID) (*Repository, error) {
	if working == 0 {
		return nil, errors.New("clustered repository requires an allocated working snapshot origin")
	}
	return m.createPrivate(path, name, working)
}

func (m *Manager) createPrivate(path, name string, working ttcsnapshot.SnapshotID) (*Repository, error) {
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
	return m.createAt(path, name, id, working)
}

func (m *Manager) createAt(path, name, id string, workingOrigin ttcsnapshot.SnapshotID) (*Repository, error) {
	for _, dir := range []string{path, filepath.Join(path, "staging"), filepath.Join(path, "branches"), filepath.Join(path, "graph"), filepath.Join(path, "data")} {
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
	working := workingOrigin
	if err := repo.withLock(func() error {
		if working == 0 {
			working, err = repo.graph.AllocateWorking()
		} else {
			err = repo.graph.AdoptWorking(working)
		}
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		rootMeta := Inode{
			Number: rootInode, Type: "directory", Mode: 0755, Nlink: 2,
			AtimeNsec: now, BtimeNsec: now, CtimeNsec: now, MtimeNsec: now,
		}
		branch := Branch{ID: mainBranch, WorkingSnapshot: uint64(working), Generation: 1, CreatedAt: time.Now().UTC()}
		state := newBranchMetadata(branch)
		if err := repo.commitMetadata(&state.branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &rootMeta}}); err != nil {
			return err
		}
		return repo.writeBranch(state.branch)
	}); err != nil {
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
	if version != strconv.Itoa(formatVersion) {
		return nil, fmt.Errorf("private repository uses unsupported format %q", strings.TrimSpace(string(format)))
	}
	repository, err := m.openPath(path, info)
	if err != nil {
		return nil, err
	}
	if err := repository.recoverGraphState(); err != nil {
		repository.Close()
		return nil, fmt.Errorf("recovering private repository: %w", err)
	}
	return repository, nil
}

// List returns all valid snapshot-graph-era volumes.
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
		if version != strconv.Itoa(formatVersion) {
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
			repository, err := m.openPath(path, info)
			if err != nil {
				return nil, err
			}
			if err := repository.recoverGraphState(); err != nil {
				repository.Close()
				return nil, fmt.Errorf("recovering volume %q: %w", info.Name, err)
			}
			return repository, nil
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

// RemovePrivate removes a repository addressed by a trusted path beneath this
// manager's root. The caller is responsible for proving that the repository is
// family-private rather than a named volume.
func (m *Manager) RemovePrivate(path string) error {
	path = filepath.Clean(path)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("private repository path %q is outside data root %q", path, m.root)
	}
	return os.RemoveAll(path)
}

func (m *Manager) openPath(path string, info VolumeInfo) (*Repository, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	graph, err := ttcsnapshot.OpenExternallyCoordinated(filepath.Join(path, "graph"))
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("opening repository snapshot graph: %w", err)
	}
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(path, "data"))
	if err != nil {
		graph.Close()
		dir.Close()
		return nil, fmt.Errorf("opening repository data store: %w", err)
	}
	return &Repository{path: path, dir: dir, copyMode: m.capability.Mode, info: info, graph: graph, data: data}, nil
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
	graphPath := fmt.Sprintf("/proc/self/fd/%d/graph", repo.dir.Fd())
	repo.graph, err = ttcsnapshot.OpenExternallyCoordinated(graphPath)
	if err != nil {
		repo.Close()
		return nil, fmt.Errorf("opening donated repository snapshot graph: %w", err)
	}
	dataFD, openErr := unix.Openat(int(repo.dir.Fd()), "data", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if openErr != nil {
		repo.Close()
		return nil, fmt.Errorf("opening donated repository data directory: %w", openErr)
	}
	dataDirectory := os.NewFile(uintptr(dataFD), "ttc-volume-data")
	repo.data, err = ttcsnapshot.OpenLocalDataStoreFD(dataDirectory)
	_ = dataDirectory.Close()
	if err != nil {
		repo.Close()
		return nil, fmt.Errorf("opening donated repository data store: %w", err)
	}
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
	if version != strconv.Itoa(formatVersion) {
		repo.Close()
		return nil, fmt.Errorf("unsupported repository format %q", strings.TrimSpace(string(format)))
	}
	if err := repo.recoverGraphState(); err != nil {
		repo.Close()
		return nil, fmt.Errorf("recovering donated repository: %w", err)
	}
	activeFD, err := unix.Openat(int(repo.dir.Fd()), "active.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		repo.Close()
		return nil, fmt.Errorf("opening donated repository lifecycle lock: %w", err)
	}
	repo.activeLock = os.NewFile(uintptr(activeFD), "ttc-volume-active")
	if err := unix.Flock(activeFD, unix.LOCK_SH); err != nil {
		repo.Close()
		return nil, fmt.Errorf("locking donated repository lifecycle: %w", err)
	}
	return repo, nil
}

// OpenRepository opens a trusted path-backed repository. Unlike
// OpenRepositoryFD, the returned Repository may perform garbage collection.
func OpenRepository(path string, mode CopyMode) (*Repository, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("repository path %q is not absolute", path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	repository, err := OpenRepositoryFD(dir, mode)
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	// OpenRepositoryFD deliberately keeps the bulk store descriptor-only for
	// the isolated Sentry. Manager-side operations such as snapshot export need
	// a trusted host path for reflink/copy source selection and garbage
	// collection. Reopen only the bulk store in path-backed mode; retaining the
	// descriptor-only instance made objectPath return data/invalid even though
	// the referenced object existed.
	if err := repository.data.Close(); err != nil {
		repository.Close()
		return nil, err
	}
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(path, "data"))
	if err != nil {
		repository.Close()
		return nil, fmt.Errorf("opening path-backed repository data store: %w", err)
	}
	repository.data = data
	repository.path = path
	// OpenRepositoryFD conservatively assumes that its descriptor was donated
	// to a Sentry. This wrapper is explicitly manager-side and must not make its
	// own maintenance handle look like an active runtime.
	if err := repository.releaseActiveLock(); err != nil {
		repository.Close()
		return nil, fmt.Errorf("releasing manager repository lifecycle lock: %w", err)
	}
	return repository, nil
}

func (r *Repository) releaseActiveLock() error {
	if r == nil || r.activeLock == nil {
		return nil
	}
	err := errors.Join(unix.Flock(int(r.activeLock.Fd()), unix.LOCK_UN), r.activeLock.Close())
	r.activeLock = nil
	return err
}

// Close releases every host resource owned by the repository.
func (r *Repository) Close() error {
	if r == nil {
		return nil
	}
	// Repository close is a lifecycle durability fence. In particular, do not
	// strand the bounded process-local metadata tail when a Sentry exits.
	var syncErr error
	if r.graph != nil && r.data != nil {
		syncErr = r.SyncMetadata()
	}
	r.remoteLifecycleMu.Lock()
	defer r.remoteLifecycleMu.Unlock()
	r.remoteMu.Lock()
	var remoteErr error
	remotes := make([]*remoteObjectClient, 0, len(r.remotes))
	for remote := range r.remotes {
		remotes = append(remotes, remote)
	}
	r.remotes = nil
	r.remoteByObject = nil
	r.remoteByCatalog = nil
	r.remoteMu.Unlock()
	for _, remote := range remotes {
		remote.users.Wait()
		remote.mu.Lock()
		remoteErr = errors.Join(remoteErr, remote.conn.Close())
		remote.mu.Unlock()
	}
	var graphErr error
	if r.graph != nil {
		graphErr = r.graph.Close()
		r.graph = nil
	}
	var dataErr error
	if r.data != nil {
		dataErr = r.data.Close()
		r.data = nil
	}
	var lockErr error
	if r.lockFile != nil {
		lockErr = r.lockFile.Close()
		r.lockFile = nil
	}
	activeErr := r.releaseActiveLock()
	var dirErr error
	if r.dir != nil {
		dirErr = r.dir.Close()
		r.dir = nil
	}
	return errors.Join(syncErr, lockErr, activeErr, remoteErr, graphErr, dataErr, dirErr)
}

// Directory duplicates the repository directory descriptor for donation to a
// Sentry process or another trusted repository client.
func (r *Repository) Directory() (*os.File, error) {
	if r == nil || r.dir == nil {
		return nil, errors.New("repository is closed")
	}
	// A donated descriptor may be opened by another process, which cannot see
	// this Repository instance's bounded in-memory metadata tail.
	if err := r.SyncMetadata(); err != nil {
		return nil, err
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
		entries, err := r.metadataDirectory(state, rootInode)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("regular repository root requires an empty namespace")
		}
		object, err := r.createObject(branch.WorkingSnapshot, "")
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		root = Inode{
			Number: rootInode, Type: "regular", Mode: mode & 07777,
			UID: uid, GID: gid, Nlink: 1, Object: object,
			DataGeneration: branch.WorkingSnapshot,
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

func (r *Repository) withLock(fn func() error) (result error) {
	r.viewMu.Lock()
	defer r.viewMu.Unlock()
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
	if r.graph != nil {
		if err := r.graph.BeginExternalTransaction(); err != nil {
			return err
		}
		defer func() {
			result = errors.Join(result, r.graph.EndExternalTransaction())
		}()
	}
	return fn()
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
	if branch.ID != id || branch.Root == "" || branch.WorkingSnapshot == 0 || branch.Generation == 0 {
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

// recoverGraphState reconciles the graph journal, branch pointer files, and
// transaction pins after an interrupted repository mutation. The graph
// transaction is always committed before its small branch pointer file, so a
// graph root is authoritative when the two disagree. Conversely, a graph root
// without a published branch pointer is an unpublished transaction and is
// released.
//
// A fork may also seal the source working origin before either branch pointer
// is published. Such a branch receives a fresh working origin during recovery;
// sealed origins are never reopened or reused.
func (r *Repository) recoverGraphState() error {
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
		branches := make(map[string]struct{})
		sort.Strings(names)
		for _, name := range names {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if err := validateName(id, "branch"); err != nil {
				return fmt.Errorf("invalid branch pointer %q: %w", name, err)
			}
			branch, err := r.readBranch(id)
			if err != nil {
				return err
			}
			root, ok := r.graph.Root(graphRootName(id))
			if !ok {
				return fmt.Errorf("branch %q has no snapshot graph root", id)
			}
			changed := false
			if branch.Root != root.String() {
				branch.Root = root.String()
				changed = true
			}
			base, hasBase := r.graph.Root(graphBaseRootName(id))
			if hasBase && branch.BaseRoot != base.String() {
				branch.BaseRoot = base.String()
				changed = true
			}
			if !hasBase && branch.BaseRoot != "" {
				return fmt.Errorf("branch %q has a diff base without an authoritative graph root", id)
			}
			working, ok := r.graph.Snapshot(ttcsnapshot.SnapshotID(branch.WorkingSnapshot))
			if !ok {
				return fmt.Errorf("branch %q references missing working snapshot %d", id, branch.WorkingSnapshot)
			}
			if working.State != ttcsnapshot.SnapshotWorking {
				// The only repository operation that seals a working origin is a
				// fork. Preserve its diff boundary even if publication stopped
				// between the graph transaction and the branch pointer rename.
				if !hasBase {
					if err := r.graph.SetRoot(graphBaseRootName(id), root); err != nil {
						return fmt.Errorf("retaining recovered diff base for branch %q: %w", id, err)
					}
					branch.BaseRoot = root.String()
				}
				fresh, err := r.graph.AllocateWorking()
				if err != nil {
					return err
				}
				branch.WorkingSnapshot = uint64(fresh)
				branch.Generation++
				changed = true
			}
			if changed {
				if err := r.writeBranch(branch); err != nil {
					return err
				}
			}
			branches[id] = struct{}{}
		}

		inspection := r.graph.Inspect()
		rootNames := make([]string, 0, len(inspection.Roots))
		for name := range inspection.Roots {
			rootNames = append(rootNames, name)
		}
		sort.Strings(rootNames)
		var orphanRoots []string
		for _, name := range rootNames {
			id := ""
			switch {
			case strings.HasPrefix(name, "filesystem/"):
				id = strings.TrimPrefix(name, "filesystem/")
			case strings.HasPrefix(name, "filesystem-base/"):
				id = strings.TrimPrefix(name, "filesystem-base/")
			default:
				return fmt.Errorf("repository graph contains unexpected root %q", name)
			}
			if _, ok := branches[id]; ok {
				continue
			}
			orphanRoots = append(orphanRoots, name)
		}
		if err := r.graph.DeleteRoots(orphanRoots); err != nil {
			return fmt.Errorf("releasing unpublished graph roots: %w", err)
		}

		pinNames := make([]string, 0, len(inspection.Pins))
		for name := range inspection.Pins {
			if strings.HasPrefix(name, "construct/") {
				pinNames = append(pinNames, name)
			}
		}
		sort.Strings(pinNames)
		if err := r.graph.DeletePins(pinNames); err != nil {
			return fmt.Errorf("releasing interrupted construction pins: %w", err)
		}
		return nil
	})
}

// Branch returns a branch root.
func (r *Repository) Branch(id string) (Branch, error) {
	if err := r.withLock(func() error { return r.flushMetadataBatchLocked(id) }); err != nil {
		return Branch{}, err
	}
	branch, err := r.readBranch(id)
	if err != nil {
		return Branch{}, err
	}
	if err := r.graph.Reload(); err != nil {
		return Branch{}, err
	}
	root, ok := r.graph.Root(graphRootName(id))
	if !ok {
		return Branch{}, fmt.Errorf("branch %q has no snapshot graph root", id)
	}
	branch.Root = root.String()
	return branch, nil
}

// RemoveBranch removes a branch pointer after its Kernel no longer exists.
// The final pointer, regardless of its name, may only be removed by deleting
// the repository itself. This matters for elastic TTC: a node-local private
// repository may contain only a scoped branch such as b-<digest>, rather than
// the canonical main pointer. Allowing that pointer to be unlinked first
// leaves an empty repository that cannot distinguish a committed deletion
// from an interrupted one. Immutable graph objects are reclaimed separately
// by GarbageCollect.
func (r *Repository) RemoveBranch(id string) error {
	if err := validateName(id, "branch"); err != nil {
		return err
	}
	return r.withLock(func() error {
		branch, err := r.readBranch(id)
		if err != nil {
			return err
		}
		if err := r.flushMetadataBatchLocked(id); err != nil {
			return fmt.Errorf("publishing branch metadata before removal: %w", err)
		}
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
			return ErrFinalBranch
		}
		if err := r.unlink(filepath.Join("branches", id+".json")); err != nil {
			return err
		}
		r.branchMu.Lock()
		delete(r.branchCache, id)
		r.branchMu.Unlock()
		if err := r.syncDirectory("branches"); err != nil {
			return err
		}
		roots := []string{graphRootName(id)}
		if branch.BaseRoot != "" {
			roots = append(roots, graphBaseRootName(id))
		}
		if err := r.graph.DeleteRoots(roots); err != nil {
			return err
		}
		r.dirtyMu.Lock()
		delete(r.reservedFileIDs, branch.WorkingSnapshot)
		delete(r.dirtyBranches, id)
		delete(r.pendingMetadata, id)
		delete(r.dirtyFiles, filepath.Join("branches", id+".json"))
		r.dirtyMu.Unlock()
		return nil
	})
}

// RemoveBranchAndCollect is the runtime-side teardown operation for a branch
// whose Kernel and open file descriptions have already been released. It
// removes the persistent root before collecting, so surviving branches are
// protected solely by their own graph edges. Collection may be deferred when
// another Sentry has the same repository open; daemon-side cleanup remains an
// idempotent fallback.
func (r *Repository) RemoveBranchAndCollect(id string) error {
	if err := r.RemoveBranch(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := r.GarbageCollectActive()
	return err
}

// ForgetBranch releases process-local caches for a branch whose Kernel has
// been released. It does not alter the persistent branch pointer; ttcd removes
// that pointer separately, or a later rehydration reloads it on demand.
func (r *Repository) ForgetBranch(id string) {
	if r == nil || validateName(id, "branch") != nil {
		return
	}
	r.viewMu.Lock()
	defer r.viewMu.Unlock()
	r.operationMu.Lock()
	defer r.operationMu.Unlock()

	r.metadataMu.Lock()
	if state := r.metadata[id]; state != nil {
		delete(r.metadata, id)
	}
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
	return r.beginFork(source, child, nil)
}

// BeginForkWithOrigins performs the same O(1) repository fork using the exact
// fresh source and child working origins already committed by cluster Raft.
func (r *Repository) BeginForkWithOrigins(source, child string, sourceWorking, childWorking ttcsnapshot.SnapshotID) (*ForkTransaction, error) {
	if sourceWorking == 0 || childWorking == 0 || sourceWorking == childWorking {
		return nil, errors.New("clustered repository fork requires two distinct working snapshot origins")
	}
	return r.beginFork(source, child, []ttcsnapshot.SnapshotID{sourceWorking, childWorking})
}

func (r *Repository) beginFork(source, child string, externalOrigins []ttcsnapshot.SnapshotID) (*ForkTransaction, error) {
	if err := validateName(child, "branch"); err != nil {
		return nil, err
	}
	var tx ForkTransaction
	err := r.withLock(func() error {
		sourceBranch, err := r.readBranch(source)
		if err != nil {
			return err
		}
		// A fork clones the immutable graph root, so first publish the bounded
		// live metadata tail. This work is capped by metadataBatchOperations and
		// does not grow with the branch's lifetime mutation count.
		if err := r.flushMetadataBatchLocked(source); err != nil {
			return fmt.Errorf("publishing source metadata before fork: %w", err)
		}
		// Root advances are published to the graph immediately and summarized
		// in the branch pointer only at durability fences. Fork from the
		// authoritative graph root, never from a possibly lagging summary.
		sourceRoot, ok := r.graph.Root(graphRootName(source))
		if !ok {
			return fmt.Errorf("branch %q has no snapshot graph root", source)
		}
		sourceBranch.Root = sourceRoot.String()
		if _, err := r.readBranch(child); err == nil {
			return fmt.Errorf("branch %q already exists", child)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tx.source = sourceBranch
		sealedOrigin := sourceBranch.WorkingSnapshot
		copies := map[string]string{
			graphRootName(child):      graphRootName(source),
			graphBaseRootName(source): graphRootName(source),
			graphBaseRootName(child):  graphRootName(source),
		}
		var working []ttcsnapshot.SnapshotID
		if len(externalOrigins) == 0 {
			working, err = r.graph.SealAndFork(
				ttcsnapshot.SnapshotID(sourceBranch.WorkingSnapshot),
				ttcsnapshot.SnapshotSealedLocal, copies, 2,
			)
		} else {
			working = externalOrigins
			err = r.graph.SealAndForkWithOrigins(
				ttcsnapshot.SnapshotID(sourceBranch.WorkingSnapshot),
				ttcsnapshot.SnapshotSealedVolatile, copies, working,
			)
		}
		if err != nil {
			return err
		}
		r.dirtyMu.Lock()
		delete(r.reservedFileIDs, sealedOrigin)
		r.dirtyMu.Unlock()
		sourceBranch.WorkingSnapshot = uint64(working[0])
		sourceBranch.BaseRoot = sourceBranch.Root
		sourceBranch.Generation++
		childBranch := Branch{
			ID: child, Root: sourceBranch.Root, BaseRoot: sourceBranch.Root, WorkingSnapshot: uint64(working[1]),
			RemoteCatalog: sourceBranch.RemoteCatalog, RemoteCatalogSnapshot: sourceBranch.RemoteCatalogSnapshot,
			RemoteLogicalBytes: sourceBranch.RemoteLogicalBytes,
			Generation:         sourceBranch.Generation, CreatedAt: time.Now().UTC(),
		}
		// Publish the source's new generation first. If the process crashes
		// before the child pointer appears, the only effect is a conservative
		// first-write clone in the source. Publishing the child first would let
		// the old-generation source mutate an object now visible to the child.
		if err := r.writeBranch(sourceBranch); err != nil {
			return err
		}
		r.dirtyMu.Lock()
		delete(r.dirtyBranches, source)
		r.dirtyMu.Unlock()
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
			if rootErr := r.graph.DeleteRoots([]string{graphRootName(child), graphBaseRootName(child)}); rootErr != nil {
				return fmt.Errorf("publishing child branch: %w (releasing child graph root also failed: %v)", err, rootErr)
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

// Abort removes the child branch. The source retains its freshly allocated
// working origin: snapshot IDs are never reused and sealed origins never move
// backward to working.
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
		return tx.repo.graph.DeleteRoots([]string{graphRootName(tx.child.ID), graphBaseRootName(tx.child.ID)})
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

// ForkWithOrigins creates and commits a clustered fork using exact
// Raft-allocated fresh working origins.
func (r *Repository) ForkWithOrigins(source, child string, sourceWorking, childWorking uint64) error {
	tx, err := r.BeginForkWithOrigins(source, child, ttcsnapshot.SnapshotID(sourceWorking), ttcsnapshot.SnapshotID(childWorking))
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
			return fmt.Errorf("branch %q entry %d/%q already exists: %w", branchID, parent, name, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		object, err := r.createObject(branch.WorkingSnapshot, "")
		if err != nil {
			return fmt.Errorf("creating branch %q entry %d/%q data object: %w", branchID, parent, name, err)
		}
		created, err = r.allocateInodeLocked()
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		inode := Inode{
			Number: created, Type: "regular", Mode: mode & 07777,
			UID: uid, GID: gid, Nlink: 1, Object: object,
			DataGeneration: branch.WorkingSnapshot,
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

// Lookup resolves a child name.
func (r *Repository) Lookup(branchID string, parent uint64, name string) (uint64, error) {
	if err := validateEntryName(name); err != nil {
		return 0, err
	}
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
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
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
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
			return fmt.Errorf("looking up branch %q inode %d for write: %w", branchID, ino, err)
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
		entries, err := r.metadataDirectory(state, ino)
		if err != nil {
			return err
		}
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
				entries, err := r.metadataDirectory(state, replacedIno)
				if err != nil {
					return err
				}
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

// UpdateMetadataBatch publishes coalesced metadata changes in one journal
// transaction. Identity, type, link count, data object, generation, and
// regular-file size remain repository-owned.
func (r *Repository) UpdateMetadataBatch(branchID string, updates []Inode) error {
	if len(updates) == 0 {
		return nil
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
		ops := make([]metadataOp, 0, len(updates))
		seen := make(map[uint64]struct{}, len(updates))
		for _, updated := range updates {
			if _, ok := seen[updated.Number]; ok {
				return fmt.Errorf("duplicate metadata update for inode %d", updated.Number)
			}
			seen[updated.Number] = struct{}{}
			current, err := r.metadataInode(state, updated.Number)
			if err != nil {
				return err
			}
			mergeMutableMetadata(&current, updated)
			ops = append(ops, metadataOp{Kind: metadataSetInode, Inode: &current})
		}
		return r.commitMetadata(&branch, state, ops)
	})
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
		mergeMutableMetadata(&current, updated)
		if current.Type == "regular" && growSize && updated.Size > current.Size {
			current.Size = updated.Size
		}
		return r.commitMetadata(&branch, state, []metadataOp{{Kind: metadataSetInode, Inode: &current}})
	})
}

func mergeMutableMetadata(current *Inode, updated Inode) {
	current.Mode = updated.Mode & 07777
	current.UID = updated.UID
	current.GID = updated.GID
	current.AtimeNsec = updated.AtimeNsec
	current.BtimeNsec = updated.BtimeNsec
	current.CtimeNsec = updated.CtimeNsec
	current.MtimeNsec = updated.MtimeNsec
	current.Xattrs = updated.Xattrs
	if current.Type == "symlink" {
		current.Target = updated.Target
	}
}

func (r *Repository) ensurePrivate(branch *Branch, state *branchMetadata, inode *Inode) (bool, error) {
	if inode.Type != "regular" {
		return false, syscall.EINVAL
	}
	if !validObjectID(inode.Object) {
		return false, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
	}
	if inode.DataGeneration == branch.WorkingSnapshot {
		return false, nil
	}
	// Materialize a remotely inherited immutable file before creating its
	// private writable generation.
	if err := r.ensureObjectRange(inode.Object, inode.Size, 0, inode.Size); err != nil {
		return false, err
	}
	object, err := r.createObject(branch.WorkingSnapshot, inode.Object)
	if err != nil {
		return false, err
	}
	inode.Object = object
	inode.DataGeneration = branch.WorkingSnapshot
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
			return fmt.Errorf("opening branch %q inode %d object %q for write: %w", branchID, ino, inode.Object, err)
		}
		written, err = file.WriteAt(data, offset)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := r.MarkDataDirty(inode.Object); err != nil {
			return err
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
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
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
	if !validObjectID(inode.Object) {
		return 0, fmt.Errorf("inode %d has invalid object ID %q", inode.Number, inode.Object)
	}
	if length := int64(len(data)); length != 0 {
		if offset < inode.Size && length > inode.Size-offset {
			length = inode.Size - offset
		}
		if length > 0 {
			if err := r.ensureObjectRange(inode.Object, inode.Size, offset, length); err != nil {
				return 0, err
			}
		}
	}
	file, err := r.openObject(inode.Object, os.O_RDONLY)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.ReadAt(data, offset)
}

// CoWCallback is invoked after a repository mutation publishes a private data
// object, but before the repository mutation lock is released. It allows a
// filesystem client to drop descriptors and mappings of the formerly selected
// object before garbage collection can consider that object unreachable.
// Implementations must not reenter the Repository.
type CoWCallback func()

// OpenDataFile opens the host data object selected by branchID and ino.
// Writable opens first pass through the branch-generation CoW gate. Callers
// must close the returned descriptor and must revoke every writable descriptor
// and mapping before BeginFork.
func (r *Repository) OpenDataFile(branchID string, ino uint64, writable bool) (*os.File, Inode, bool, error) {
	return r.OpenDataFileWithCoW(branchID, ino, writable, nil)
}

// OpenDataFileWithCoW is OpenDataFile with notification of an object CoW.
func (r *Repository) OpenDataFileWithCoW(branchID string, ino uint64, writable bool, onCoW CoWCallback) (*os.File, Inode, bool, error) {
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
			if cloned && onCoW != nil {
				onCoW()
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
		r.viewMu.RLock()
		err = open()
		r.viewMu.RUnlock()
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
		return fmt.Errorf("resolving branch %q inode %d for data sync: %w", branchID, ino, err)
	}
	defer file.Close()
	if dataOnly {
		if err := unix.Fdatasync(int(file.Fd())); err != nil {
			return fmt.Errorf("fdatasync branch %q inode %d: %w", branchID, ino, err)
		}
		return nil
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("fsync branch %q inode %d: %w", branchID, ino, err)
	}
	return nil
}

// MarkDataDirty records that a writable object may contain bytes newer than
// the last repository durability fence. The object identity is supplied by
// the filesystem after CoW selection, avoiding a metadata-tree lookup on each
// write(2).
func (r *Repository) MarkDataDirty(object string) error {
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil || id.Kind != ttcsnapshot.ObjectKindFile {
		return fmt.Errorf("invalid dirty repository object %q", object)
	}
	r.dirtyMu.Lock()
	if r.dirtyData == nil {
		r.dirtyData = make(map[string]uint64)
	}
	r.dirtyData[object]++
	r.dirtyMu.Unlock()
	return nil
}

// SyncMetadata makes repository namespace and inode metadata durable.
func (r *Repository) SyncMetadata() error {
	return r.withLock(r.syncMetadataLocked)
}

// PublishMetadata makes this repository handle's bounded metadata batches
// visible through the immutable snapshot graph without creating a data or
// metadata durability boundary. Manager-side observations such as TTC diff
// use this after asking the live Sentry to publish its process-local tail.
// Guest fsync(2), sync(2), fork, and lifecycle operations continue to use
// SyncMetadata when persistence is required.
func (r *Repository) PublishMetadata() error {
	return r.withLock(r.flushAllMetadataBatchesLocked)
}

// syncMetadataLocked persists files before the directories that make their
// current names reachable. Repository mutations and this flush are serialized
// by the repository flock, so a successful call covers every mutation that
// completed before it began.
func (r *Repository) syncMetadataLocked() error {
	if err := r.flushAllMetadataBatchesLocked(); err != nil {
		return fmt.Errorf("publishing repository metadata batches: %w", err)
	}
	// Branch pointer files are recovery summaries. The graph root is visible
	// immediately and remains authoritative between durability fences.
	r.dirtyMu.Lock()
	dirtyData := make(map[string]uint64, len(r.dirtyData))
	for name, generation := range r.dirtyData {
		if _, reclaiming := r.pendingReclaims[name]; !reclaiming {
			dirtyData[name] = generation
		}
	}
	dirtyBranches := make([]string, 0, len(r.dirtyBranches))
	for id := range r.dirtyBranches {
		dirtyBranches = append(dirtyBranches, id)
	}
	r.dirtyMu.Unlock()
	dataIDs := make([]ttcsnapshot.ObjectID, 0, len(dirtyData))
	for name := range dirtyData {
		id, err := ttcsnapshot.ParseObjectID(name)
		if err != nil || id.Kind != ttcsnapshot.ObjectKindFile {
			return fmt.Errorf("invalid dirty repository object %q", name)
		}
		dataIDs = append(dataIDs, id)
	}
	sort.Slice(dataIDs, func(i, j int) bool { return dataIDs[i].String() < dataIDs[j].String() })
	// A guest durability boundary first persists every new or modified payload
	// and the complete directory path that publishes it. Only then may the
	// graph WAL making those immutable IDs reachable become durable.
	if err := r.data.SyncObjects(dataIDs); err != nil {
		return fmt.Errorf("syncing repository data objects: %w", err)
	}
	sort.Strings(dirtyBranches)
	r.dirtyMu.Lock()
	var rootDirty []string
	for name := range r.dirtyFiles {
		if !strings.HasPrefix(name, "branches/") {
			rootDirty = append(rootDirty, name)
		}
	}
	dirtyDirs := make([]string, 0, len(r.dirtyDirs))
	for name := range r.dirtyDirs {
		dirtyDirs = append(dirtyDirs, name)
	}
	r.dirtyMu.Unlock()
	sort.Strings(rootDirty)
	// Children must precede parents so every object name is durable before a
	// journal transaction that makes it reachable.
	sort.Slice(dirtyDirs, func(i, j int) bool {
		return strings.Count(dirtyDirs[i], string(filepath.Separator)) > strings.Count(dirtyDirs[j], string(filepath.Separator))
	})
	syncFiles := func(names []string) error {
		for _, name := range names {
			file, err := r.openFile(name, os.O_RDONLY, 0)
			if err != nil {
				return fmt.Errorf("opening dirty repository file %q: %w", name, err)
			}
			err = file.Sync()
			closeErr := file.Close()
			if err != nil {
				return fmt.Errorf("syncing dirty repository file %q: %w", name, err)
			}
			if closeErr != nil {
				return fmt.Errorf("closing dirty repository file %q: %w", name, closeErr)
			}
		}
		return nil
	}
	if err := syncFiles(rootDirty); err != nil {
		return err
	}
	// Persist directory entries only after the files they name. The graph WAL
	// is the final reachability barrier, so every immutable object must already
	// be openable if that WAL survives a crash.
	for _, name := range dirtyDirs {
		if name == "." {
			continue
		}
		if err := r.syncDirectory(name); err != nil {
			return fmt.Errorf("syncing dirty repository directory %q: %w", name, err)
		}
	}
	// The graph journal names the immutable metadata and data objects made
	// durable above. Persist it before publishing branch pointer files.
	if err := r.graph.Sync(); err != nil {
		return fmt.Errorf("syncing repository snapshot graph: %w", err)
	}
	r.dirtyMu.Lock()
	for name, generation := range dirtyData {
		if r.dirtyData[name] == generation {
			delete(r.dirtyData, name)
		}
	}
	r.dirtyMu.Unlock()
	// The graph WAL is now a durable proof that these immutable payloads are
	// unreachable. Delete them directly by ID instead of scanning the complete
	// object store; a crash before an unlink merely leaves harmless garbage for
	// startup reconciliation.
	r.dirtyMu.Lock()
	reclaims := make([]ttcsnapshot.ObjectID, 0, len(r.pendingReclaims))
	for _, id := range r.pendingReclaims {
		reclaims = append(reclaims, id)
	}
	r.dirtyMu.Unlock()
	sort.Slice(reclaims, func(i, j int) bool { return reclaims[i].String() < reclaims[j].String() })
	for _, id := range reclaims {
		if err := r.data.Remove(id); err != nil {
			return fmt.Errorf("removing reclaimed repository object %s: %w", id, err)
		}
		if id.Kind == ttcsnapshot.ObjectKindFile {
			if err := r.clearRemoteObjectPending(id.String()); err != nil {
				return fmt.Errorf("removing reclaimed remote marker %s: %w", id, err)
			}
		}
		r.dirtyMu.Lock()
		delete(r.pendingReclaims, id.String())
		delete(r.dirtyData, id.String())
		r.dirtyMu.Unlock()
	}
	for _, id := range dirtyBranches {
		r.dirtyMu.Lock()
		state := r.dirtyBranches[id]
		r.dirtyMu.Unlock()
		if state == nil {
			return fmt.Errorf("dirty branch %q has no process-local metadata", id)
		}
		state.mu.RLock()
		branch := state.branch
		state.mu.RUnlock()
		if err := r.writeBranch(branch); err != nil {
			return err
		}
	}
	// writeBranch above adds the newly published summaries to dirtyFiles.
	r.dirtyMu.Lock()
	var branchDirty []string
	for name := range r.dirtyFiles {
		if strings.HasPrefix(name, "branches/") {
			branchDirty = append(branchDirty, name)
		}
	}
	r.dirtyMu.Unlock()
	sort.Strings(branchDirty)
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
	for _, id := range dirtyBranches {
		delete(r.dirtyBranches, id)
	}
	r.dirtyMu.Unlock()
	return nil
}

// Truncate applies the same first-writer CoW rule as WriteAt.
func (r *Repository) Truncate(branchID string, ino uint64, size int64) (bool, error) {
	return r.TruncateWithCoW(branchID, ino, size, nil)
}

// TruncateWithCoW is Truncate with notification of an object CoW.
func (r *Repository) TruncateWithCoW(branchID string, ino uint64, size int64, onCoW CoWCallback) (bool, error) {
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
		if cloned && onCoW != nil {
			onCoW()
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
		if err := r.MarkDataDirty(inode.Object); err != nil {
			return err
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
	return r.AllocateWithCoW(branchID, ino, offset, length, nil)
}

// AllocateWithCoW is Allocate with notification of an object CoW.
func (r *Repository) AllocateWithCoW(branchID string, ino uint64, offset, length int64, onCoW CoWCallback) (bool, error) {
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
		if cloned && onCoW != nil {
			onCoW()
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
		if err := r.MarkDataDirty(inode.Object); err != nil {
			return err
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
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	branch, err := r.readBranch(branchID)
	if err != nil {
		return nil, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return nil, err
	}
	return r.metadataDirectory(state, parent)
}

// ListDirectoryItems returns a directory's entries and inode metadata in
// deterministic name order from one coherent metadata snapshot.
func (r *Repository) ListDirectoryItems(branchID string, parent uint64) ([]DirectoryItem, error) {
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	branch, err := r.readBranch(branchID)
	if err != nil {
		return nil, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return nil, err
	}
	entries, err := r.metadataDirectory(state, parent)
	if err != nil {
		return nil, err
	}
	items := make([]DirectoryItem, 0, len(entries))
	for _, entry := range entries {
		inode, err := r.metadataInode(state, entry.Inode)
		if err != nil {
			return nil, fmt.Errorf("directory entry %q references inode %d: %w", entry.Name, entry.Inode, err)
		}
		items = append(items, DirectoryItem{Entry: entry, Inode: inode})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Entry.Name < items[j].Entry.Name })
	return items, nil
}

// BranchUsage returns the number of persistent inodes and page-rounded bytes
// occupied by regular files in a branch.
func (r *Repository) BranchUsage(branchID string) (uint64, uint64, error) {
	branch, err := r.Branch(branchID)
	if err != nil {
		return 0, 0, err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return 0, 0, err
	}
	if err := r.completeMetadata(state); err != nil {
		return 0, 0, err
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	var bytes uint64
	pageSize := uint64(os.Getpagesize())
	for _, inode := range state.inodes {
		if inode.Type == "regular" && inode.Size > 0 {
			size := uint64(inode.Size)
			bytes += (size + pageSize - 1) &^ (pageSize - 1)
		}
	}
	return uint64(len(state.inodes)), bytes, nil
}

// BranchDataUsage accounts logical regular-file data referenced by one branch.
type BranchDataUsage struct {
	ID           string `json:"id"`
	Inodes       uint64 `json:"inodes"`
	PrivateBytes uint64 `json:"private_bytes"`
	SharedBytes  uint64 `json:"shared_bytes"`
	TotalBytes   uint64 `json:"total_bytes"`
}

// BranchSetDataUsage accounts file-object sharing across a selected branch set.
// UniqueBytes counts each immutable data object once across the set.
type BranchSetDataUsage struct {
	Branches    []BranchDataUsage `json:"branches"`
	UniqueBytes uint64            `json:"unique_bytes"`
}

// BranchSetUsage returns file-data sharing for branchIDs without walking the
// guest namespace or materializing files. Sharing is based on immutable TTC
// object identity, and therefore reflects TTC's file-level CoW boundary.
func (r *Repository) BranchSetUsage(branchIDs []string) (BranchSetDataUsage, error) {
	ids := append([]string(nil), branchIDs...)
	sort.Strings(ids)
	type snapshot struct {
		inodes  uint64
		objects map[string]uint64
	}
	snapshots := make(map[string]snapshot, len(ids))
	objectBranches := make(map[string]uint64)
	objectSizes := make(map[string]uint64)
	pageSize := uint64(os.Getpagesize())
	for _, id := range ids {
		if _, exists := snapshots[id]; exists {
			return BranchSetDataUsage{}, fmt.Errorf("duplicate branch %q in usage set", id)
		}
		branch, err := r.Branch(id)
		if err != nil {
			return BranchSetDataUsage{}, err
		}
		state, err := r.metadataForBranch(branch)
		if err != nil {
			return BranchSetDataUsage{}, err
		}
		if err := r.completeMetadata(state); err != nil {
			return BranchSetDataUsage{}, err
		}
		state.mu.RLock()
		current := snapshot{inodes: uint64(len(state.inodes)), objects: make(map[string]uint64)}
		for _, inode := range state.inodes {
			if inode.Type != "regular" || inode.Size <= 0 {
				continue
			}
			if inode.Object == "" {
				state.mu.RUnlock()
				return BranchSetDataUsage{}, fmt.Errorf("branch %q inode %d has data without an object", id, inode.Number)
			}
			size := (uint64(inode.Size) + pageSize - 1) &^ (pageSize - 1)
			if size > current.objects[inode.Object] {
				current.objects[inode.Object] = size
			}
		}
		state.mu.RUnlock()
		snapshots[id] = current
		for object, size := range current.objects {
			objectBranches[object]++
			if size > objectSizes[object] {
				objectSizes[object] = size
			}
		}
	}
	result := BranchSetDataUsage{Branches: make([]BranchDataUsage, 0, len(ids))}
	for _, id := range ids {
		snapshot := snapshots[id]
		usage := BranchDataUsage{ID: id, Inodes: snapshot.inodes}
		for object, size := range snapshot.objects {
			if objectBranches[object] > 1 {
				usage.SharedBytes += size
			} else {
				usage.PrivateBytes += size
			}
		}
		usage.TotalBytes = usage.PrivateBytes + usage.SharedBytes
		result.Branches = append(result.Branches, usage)
	}
	for _, size := range objectSizes {
		result.UniqueBytes += size
	}
	return result, nil
}

// BranchDiff returns paths changed by branchID relative to the immutable graph
// root captured at its most recent fork. It reads TTC metadata; it does not
// walk materialized host or guest directory trees.
func (r *Repository) BranchDiff(branchID string) ([]PathChange, error) {
	branch, err := r.Branch(branchID)
	if err != nil {
		return nil, err
	}
	current, err := r.metadataForBranch(branch)
	if err != nil {
		return nil, err
	}
	if err := r.completeMetadata(current); err != nil {
		return nil, err
	}
	current.mu.RLock()
	currentPaths, err := metadataPaths(current)
	current.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	base := newBranchMetadata(Branch{ID: branchID + "-diff-base"})
	if branch.BaseRoot != "" {
		root, err := ttcsnapshot.ParseObjectID(branch.BaseRoot)
		if err != nil {
			return nil, err
		}
		inodes, entries, err := r.graph.FilesystemRecords(root)
		if err != nil {
			return nil, err
		}
		for _, record := range inodes {
			inode := inodeFromGraph(record)
			base.inodes[inode.Number] = inode
		}
		for _, record := range entries {
			entry := DirectoryEntry{Name: record.Name, Inode: record.Inode}
			base.entries[metadataEntryKey{Parent: record.Parent, Name: record.Name}] = entry
			directory := base.directories[record.Parent]
			if directory == nil {
				directory = make(map[string]DirectoryEntry)
				base.directories[record.Parent] = directory
			}
			directory[record.Name] = entry
		}
	}
	basePaths, err := metadataPaths(base)
	if err != nil {
		return nil, err
	}

	all := make(map[string]struct{}, len(currentPaths)+len(basePaths))
	for path := range currentPaths {
		all[path] = struct{}{}
	}
	for path := range basePaths {
		all[path] = struct{}{}
	}
	changes := make([]PathChange, 0)
	for path := range all {
		currentInode, currentOK := currentPaths[path]
		baseInode, baseOK := basePaths[path]
		// A whiteout is an entry that says a name is not there. It is present
		// in the layer, so without reading it as what it means every deletion
		// of a directory looked like a character device being added.
		currentGone := currentOK && isWhiteout(currentInode)
		baseGone := baseOK && isWhiteout(baseInode)
		switch {
		case currentGone && baseGone:
			// Hidden before and hidden now: nothing happened to it here.
		case currentGone:
			// What went away is what the layer underneath had. When that is
			// below the branch entirely — an image's own directory, say — the
			// deletion is still certain and the type is not, so it is left
			// unsaid rather than reported as the whiteout's own.
			gone := PathChange{Path: path, Kind: "delete"}
			if baseOK {
				gone.Type = baseInode.Type
			}
			changes = append(changes, gone)
		case baseGone && currentOK:
			// It was hidden and now it is not, which is the path coming back.
			changes = append(changes, PathChange{Path: path, Kind: "add", Type: currentInode.Type})
		case currentOK && !baseOK:
			changes = append(changes, PathChange{Path: path, Kind: "add", Type: currentInode.Type})
		case !currentOK && baseOK:
			changes = append(changes, PathChange{Path: path, Kind: "delete", Type: baseInode.Type})
		case currentOK && baseOK && !sameDiffInode(currentInode, baseInode):
			changes = append(changes, PathChange{Path: path, Kind: "modify", Type: currentInode.Type})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// WriteLayerTar writes the complete branch-visible repository namespace as an
// uncompressed OCI layer. For a rootfs-upper repository this is the complete
// set of changes relative to the immutable image, including overlay whiteout
// entries. Open Unix sockets are runtime state and are intentionally omitted.
func (r *Repository) WriteLayerTar(branchID string, output io.Writer) error {
	branch, err := r.Branch(branchID)
	if err != nil {
		return err
	}
	state, err := r.metadataForBranch(branch)
	if err != nil {
		return err
	}
	if err := r.completeMetadata(state); err != nil {
		return err
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	writer := tar.NewWriter(output)
	hardlinks := make(map[uint64]string)
	writeInode := func(name string, inode Inode) error {
		if inode.Type == "socket" {
			return nil
		}
		// Overlayfs stores a whiteout in its writable layer as a 0:0 character
		// device at the hidden entry's name. OCI layers encode the same operation
		// as an empty regular file named .wh.<entry>.
		whiteout := isWhiteout(inode)
		archiveName := name
		if whiteout {
			archiveName = whiteoutArchiveName(name)
		}
		header := &tar.Header{
			Name: archiveName, Mode: int64(inode.Mode), Uid: int(inode.UID), Gid: int(inode.GID),
			ModTime: time.Unix(0, inode.MtimeNsec), AccessTime: time.Unix(0, inode.AtimeNsec),
			ChangeTime: time.Unix(0, inode.CtimeNsec), Format: tar.FormatPAX,
		}
		opaque := inode.Type == "directory" && isOverlayOpaque(inode.Xattrs)
		if !whiteout && len(inode.Xattrs) != 0 {
			header.PAXRecords = make(map[string]string, len(inode.Xattrs))
			for key, value := range inode.Xattrs {
				if isOverlayOpaqueXattr(key) {
					continue
				}
				header.PAXRecords["SCHILY.xattr."+key] = value
			}
			if len(header.PAXRecords) == 0 {
				header.PAXRecords = nil
			}
		}
		switch {
		case whiteout:
			header.Typeflag = tar.TypeReg
			header.Mode = 0600
		case inode.Type == "regular":
			if first, ok := hardlinks[inode.Number]; ok {
				header.Typeflag = tar.TypeLink
				header.Linkname = first
			} else {
				hardlinks[inode.Number] = name
				header.Typeflag = tar.TypeReg
				header.Size = inode.Size
			}
		case inode.Type == "directory":
			header.Typeflag = tar.TypeDir
			if !strings.HasSuffix(header.Name, "/") {
				header.Name += "/"
			}
		case inode.Type == "symlink":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = inode.Target
		case inode.Type == "fifo":
			header.Typeflag = tar.TypeFifo
		case inode.Type == "char":
			header.Typeflag = tar.TypeChar
			header.Devmajor = int64(inode.DevMajor)
			header.Devminor = int64(inode.DevMinor)
		case inode.Type == "block":
			header.Typeflag = tar.TypeBlock
			header.Devmajor = int64(inode.DevMajor)
			header.Devminor = int64(inode.DevMinor)
		default:
			return fmt.Errorf("inode %d has unsupported layer type %q", inode.Number, inode.Type)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if opaque {
			opaqueHeader := &tar.Header{
				Name:       opaqueWhiteoutArchiveName(name),
				Mode:       0600,
				Uid:        int(inode.UID),
				Gid:        int(inode.GID),
				ModTime:    time.Unix(0, inode.MtimeNsec),
				AccessTime: time.Unix(0, inode.AtimeNsec),
				ChangeTime: time.Unix(0, inode.CtimeNsec),
				Typeflag:   tar.TypeReg,
				Format:     tar.FormatPAX,
			}
			if err := writer.WriteHeader(opaqueHeader); err != nil {
				return err
			}
		}
		if inode.Type == "regular" && !whiteout && header.Typeflag != tar.TypeLink {
			file, err := r.openObject(inode.Object, os.O_RDONLY)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(writer, file, inode.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	}
	root, ok := state.inodes[rootInode]
	if !ok {
		return errors.New("repository branch has no root inode")
	}
	if err := writeInode(".", root); err != nil {
		return err
	}
	var walk func(uint64, string) error
	walk = func(parent uint64, prefix string) error {
		entries := state.directories[parent]
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := entries[name]
			inode, ok := state.inodes[entry.Inode]
			if !ok {
				return fmt.Errorf("directory entry %q references missing inode %d", name, entry.Inode)
			}
			archiveName := name
			if prefix != "" {
				archiveName = prefix + "/" + name
			}
			if err := writeInode(archiveName, inode); err != nil {
				return err
			}
			if inode.Type == "directory" {
				if err := walk(inode.Number, archiveName); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(rootInode, ""); err != nil {
		return err
	}
	return writer.Close()
}

func whiteoutArchiveName(name string) string {
	if index := strings.LastIndexByte(name, '/'); index >= 0 {
		return name[:index+1] + ".wh." + name[index+1:]
	}
	return ".wh." + name
}

func opaqueWhiteoutArchiveName(directory string) string {
	directory = strings.TrimSuffix(directory, "/")
	if directory == "." || directory == "" {
		return ".wh..wh..opq"
	}
	return directory + "/.wh..wh..opq"
}

// isWhiteout reports whether an inode is overlayfs's way of saying a name is
// gone: a character device with device number 0:0 and no permission bits.
//
// Named rather than written out where it is needed. It was written out in one
// place — the layer writer, which learned to turn these into `.wh.` entries —
// and not in the other, so `diff` went on reporting a deleted directory as the
// character device it literally is. A deletion that a person cannot see in
// `diff` is a deletion they go looking for somewhere else.
func isWhiteout(inode Inode) bool {
	return inode.Type == "char" && inode.Mode&07777 == 0 &&
		inode.DevMajor == 0 && inode.DevMinor == 0
}

func isOverlayOpaque(xattrs map[string]string) bool {
	for key, value := range xattrs {
		if isOverlayOpaqueXattr(key) && value == "y" {
			return true
		}
	}
	return false
}

func isOverlayOpaqueXattr(name string) bool {
	return name == "trusted.overlay.opaque" || name == "user.overlay.opaque"
}

func metadataPaths(state *branchMetadata) (map[string]Inode, error) {
	paths := make(map[string]Inode, len(state.entries)+1)
	if root, ok := state.inodes[rootInode]; ok {
		paths["/"] = root
	}
	visiting := make(map[uint64]bool)
	var walk func(uint64, string) error
	walk = func(parent uint64, prefix string) error {
		if visiting[parent] {
			return fmt.Errorf("directory cycle at inode %d", parent)
		}
		visiting[parent] = true
		defer delete(visiting, parent)
		entries := state.directories[parent]
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := entries[name]
			inode, ok := state.inodes[entry.Inode]
			if !ok {
				return fmt.Errorf("directory entry %q references missing inode %d", name, entry.Inode)
			}
			path := "/" + name
			if prefix != "" && prefix != "/" {
				path = prefix + "/" + name
			}
			paths[path] = inode
			if inode.Type == "directory" {
				if err := walk(inode.Number, path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if _, ok := state.inodes[rootInode]; ok {
		if err := walk(rootInode, ""); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func sameDiffInode(a, b Inode) bool {
	// Reads may update atime without changing branch contents. Everything else
	// is guest-visible metadata or regular-file identity and belongs in diff.
	a.AtimeNsec = 0
	b.AtimeNsec = 0
	return a.Number == b.Number && a.Type == b.Type && a.Mode == b.Mode &&
		a.UID == b.UID && a.GID == b.GID && a.Nlink == b.Nlink &&
		a.Size == b.Size && a.Object == b.Object && a.DataGeneration == b.DataGeneration &&
		a.Target == b.Target && a.DevMajor == b.DevMajor && a.DevMinor == b.DevMinor &&
		a.BtimeNsec == b.BtimeNsec && a.CtimeNsec == b.CtimeNsec && a.MtimeNsec == b.MtimeNsec &&
		mapsEqual(a.Xattrs, b.Xattrs)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func (r *Repository) createObject(origin uint64, source string) (string, error) {
	if origin == 0 {
		return "", errors.New("cannot create a file without a working snapshot")
	}
	id, err := r.reserveFileObjectID(origin)
	if err != nil {
		return "", err
	}
	var length int64
	if source == "" {
		if err := r.data.InstallDeferred(id, strings.NewReader(""), 0); err != nil {
			return "", err
		}
	} else {
		sourceID, err := ttcsnapshot.ParseObjectID(source)
		if err != nil || sourceID.Kind != ttcsnapshot.ObjectKindFile {
			return "", fmt.Errorf("invalid source object ID %q", source)
		}
		r.dirtyMu.Lock()
		pending, pendingOK := r.pendingBulk[source]
		r.dirtyMu.Unlock()
		if pendingOK {
			length = int64(pending.BulkLength)
		} else {
			object, ok := r.graph.Object(sourceID)
			if !ok {
				return "", fmt.Errorf("source object %q is absent from snapshot graph", source)
			}
			length = int64(object.BulkLength)
		}
		if err := r.data.CloneDeferred(sourceID, id, r.copyMode == CopyModeReflink); err != nil {
			return "", fmt.Errorf("privatizing volume object %q: %w", source, err)
		}
	}
	r.dirtyMu.Lock()
	if r.pendingBulk == nil {
		r.pendingBulk = make(map[string]ttcsnapshot.ReservedObject)
	}
	r.pendingBulk[id.String()] = ttcsnapshot.ReservedObject{ID: id, Pin: graphConstructionPin(id), BulkLength: uint64(length)}
	r.dirtyMu.Unlock()
	if err := r.MarkDataDirty(id.String()); err != nil {
		return "", err
	}
	return id.String(), nil
}

const fileObjectReservationBatch = 256

func (r *Repository) reserveFileObjectID(origin uint64) (ttcsnapshot.ObjectID, error) {
	r.dirtyMu.Lock()
	pool := r.reservedFileIDs[origin]
	if len(pool) != 0 {
		id := pool[0]
		r.reservedFileIDs[origin] = pool[1:]
		r.dirtyMu.Unlock()
		return id, nil
	}
	r.dirtyMu.Unlock()
	kinds := make([]ttcsnapshot.ObjectKind, fileObjectReservationBatch)
	for index := range kinds {
		kinds[index] = ttcsnapshot.ObjectKindFile
	}
	ids, err := r.graph.ReserveObjects(ttcsnapshot.SnapshotID(origin), kinds)
	if err != nil {
		return ttcsnapshot.ObjectID{}, err
	}
	r.dirtyMu.Lock()
	if r.reservedFileIDs == nil {
		r.reservedFileIDs = make(map[uint64][]ttcsnapshot.ObjectID)
	}
	r.reservedFileIDs[origin] = ids[1:]
	r.dirtyMu.Unlock()
	return ids[0], nil
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

func validObjectID(value string) bool {
	id, err := ttcsnapshot.ParseObjectID(value)
	return err == nil && id.Kind == ttcsnapshot.ObjectKindFile
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

func (r *Repository) remoteObjectMarkerPath(id string) (string, error) {
	if !validObjectID(id) {
		return "", fmt.Errorf("invalid object ID %q", id)
	}
	return filepath.Join("remote", id[:2], id[2:4], id), nil
}

// markRemoteObjectPending records that an object's apparent holes are absent
// remote data, not ordinary sparse zero ranges. The marker is repository-wide
// because object IDs are immutable and content-addressed; once one reader has
// materialized the object, every branch referencing it is independent.
func (r *Repository) markRemoteObjectPending(id, source string) error {
	if !validObjectID(source) {
		return fmt.Errorf("invalid source object ID %q", source)
	}
	marker, err := r.remoteObjectMarkerPath(id)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(marker), 0700); err != nil {
		return err
	}
	file, err := r.openFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.WriteString(source + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	r.markDirtyFile(marker)
	return nil
}

func (r *Repository) remoteSourceObject(id string) (string, bool, error) {
	marker, err := r.remoteObjectMarkerPath(id)
	if err != nil {
		return "", false, err
	}
	data, err := r.readFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	source := strings.TrimSpace(string(data))
	if !validObjectID(source) {
		return "", false, fmt.Errorf("remote marker for %q has invalid source %q", id, source)
	}
	return source, true, nil
}

func (r *Repository) remoteObjectPending(id string) (bool, error) {
	marker, err := r.remoteObjectMarkerPath(id)
	if err != nil {
		return false, err
	}
	file, err := r.openFile(marker, os.O_RDONLY, 0)
	if err == nil {
		return true, file.Close()
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (r *Repository) clearRemoteObjectPending(id string) error {
	marker, err := r.remoteObjectMarkerPath(id)
	if err != nil {
		return err
	}
	err = unix.Unlinkat(int(r.dir.Fd()), marker, 0)
	if errors.Is(err, os.ErrNotExist) {
		r.dirtyMu.Lock()
		delete(r.dirtyFiles, marker)
		r.dirtyMu.Unlock()
		return nil
	}
	if err == nil {
		// markRemoteObjectPending may have queued the marker itself for the next
		// repository durability fence. Once the complete immutable object has
		// replaced its remote placeholder, the marker is intentionally absent;
		// retaining its pathname here would make an unrelated later fsync try to
		// reopen the deleted file and incorrectly return ENOENT to the guest.
		r.dirtyMu.Lock()
		delete(r.dirtyFiles, marker)
		r.dirtyMu.Unlock()
		r.markDirtyDirectory(filepath.Dir(marker))
	}
	return err
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
