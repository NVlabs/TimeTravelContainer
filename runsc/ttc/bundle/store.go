// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	"gvisor.dev/gvisor/runsc/ttc/images"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

const (
	imageRootFormatVersion   = "v4"
	containerIDBytes         = 32
	rootfsUpperRepositoryDir = "rootfs-upper"
	hostMountRepositoryDir   = "host-mounts"
	// DefaultLogDriver is the persistent logging policy used when none is specified.
	DefaultLogDriver = "local"
	// DefaultNetworkInterface is the guest's name for the ttc virtual NIC
	// when none is specified: eth0, as Docker names a container's interface.
	DefaultNetworkInterface = "eth0"
	// LegacyNetworkInterface named the ttc virtual NIC of containers created
	// before their records held its name.
	LegacyNetworkInterface = "ttc0"
)

// removedContainerDir is outside the active container namespace so that a
// failed physical deletion cannot make container discovery fail. It remains
// under Store.Root to keep the rename that commits a deletion on one
// filesystem, including when Store.Root is an NFS mount.
const removedContainerDir = "removed-containers"

// removedActiveContainerDir, in a node's own active root, holds the
// node-local state of containers being removed, out of the active
// namespace, so that a reader finds a container's state whole or not at
// all.
const removedActiveContainerDir = "removed-local-containers"

const containerCreationMarker = ".creating"

// containerStagingDir holds unpublished container directories. Container
// metadata is assembled here and atomically renamed into containers/ only
// after the bundle and container record are complete. In particular, this
// prevents a second NFS client from observing a creation-marker unlink before
// it observes the preceding container.json rename and reclaiming a live
// distributed target as an incomplete directory.
const containerStagingDir = "container-staging"

const stalePathReadAttempts = 6

// retryStalePathRead reopens pathname-based metadata after NFS reports
// ESTALE. TTC publishes records with atomic rename; an NFS client may briefly
// retain the replaced inode's file handle even though a fresh pathname lookup
// is already valid. Retrying only ESTALE keeps real corruption and permission
// failures visible while preventing a transient cache entry from aborting a
// fork or making the cluster inventory disappear.
func retryStalePathRead[T any](operation func() (T, error)) (T, error) {
	delay := time.Millisecond
	for attempt := 0; ; attempt++ {
		value, err := operation()
		if err == nil || !errors.Is(err, unix.ESTALE) || attempt+1 == stalePathReadAttempts {
			return value, err
		}
		time.Sleep(delay)
		if delay < 16*time.Millisecond {
			delay *= 2
		}
	}
}

// sharedPathGone reports whether the file or directory at path, which may be
// on a network filesystem, no longer exists. It opens it: an NFS client
// answers a stat from attributes it cached for seconds, but revalidates a
// name it opens, and so finds a directory another node renamed away.
func sharedPathGone(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	_ = file.Close()
	return false
}

func readFileWithStaleRetry(path string) ([]byte, error) {
	return retryStalePathRead(func() ([]byte, error) {
		return os.ReadFile(path)
	})
}

func readDirWithStaleRetry(path string) ([]fs.DirEntry, error) {
	return retryStalePathRead(func() ([]fs.DirEntry, error) {
		return os.ReadDir(path)
	})
}

// Store owns persistent image roots and generated container bundles.
type Store struct {
	Root string
	// CPUFeatures is the daemon-owned, comma-separated guest CPU feature
	// profile installed into every newly created or restored OCI spec.
	CPUFeatures string
	// ActiveRoot holds node-local mutable data for running workloads. Root is
	// the durable catalog and immutable image store and may be a shared
	// filesystem in cluster mode. Empty preserves the standalone layout.
	ActiveRoot string
	// TransientRoot, when non-empty, holds short-lived OCI bundles. Keeping
	// these outside Root matters when Root is NFS: runsc's gofer retains the
	// bundle config descriptor for its lifetime, so unlinking an NFS-backed
	// bundle would leave a .nfs placeholder until the container exits.
	TransientRoot string

	// These hooks are used only by tests that emulate failures such as NFS
	// silly-renamed files. Production stores leave them nil.
	renamePath    func(string, string) error
	removeAllPath func(string) error
}

// ImageCachePruneResult summarizes a node-local capacity pass. BusyBytes are
// complete immutable bytes pinned by an active gofer and therefore never
// eligible for eviction during the pass.
type ImageCachePruneResult struct {
	BeforeBytes  uint64 `json:"before_bytes"`
	AfterBytes   uint64 `json:"after_bytes"`
	EvictedFiles uint64 `json:"evicted_files"`
	EvictedBytes uint64 `json:"evicted_bytes"`
	BusyBytes    uint64 `json:"busy_bytes"`
}

type activeRepositoryMapping struct {
	Path      string `json:"path"`
	RestoreID string `json:"restore_id,omitempty"`
	CopyMode  string `json:"copy_mode,omitempty"`
}

func (s Store) activeRoot() string {
	if s.ActiveRoot != "" {
		return s.ActiveRoot
	}
	return s.Root
}

// hasUnifiedActiveRoot reports whether durable metadata and mutable runtime
// data intentionally share one hierarchy. Standalone TTC uses this layout;
// clustered TTC keeps active data in TTC_LOCAL_ROOT and metadata in TTC_ROOT.
//
// A unified store cannot build a container in container-staging and also put
// its rootfs repository in containers/<id>: creating the repository would
// create the destination of the final directory rename. Such stores therefore
// construct the complete record in place behind containerCreationMarker and
// publish container.json last.
func (s Store) hasUnifiedActiveRoot() bool {
	return s.ActiveRoot != "" && filepath.Clean(s.ActiveRoot) == filepath.Clean(s.Root)
}

func (s Store) activeContainerDir(id string) string {
	return filepath.Join(s.activeRoot(), "containers", id)
}

func (s Store) imageRootDir() string {
	return filepath.Join(s.activeRoot(), "images")
}

var warnedUnreadableContainers sync.Map

// containerNameHints maps a data root and a container name to the ID the name
// last resolved to, so that LoadContainer need not read every container's
// record to find one by name. A hint is used only if the record it names
// still has that name and is listed; otherwise the name is looked up as
// before. Names are unique, which creation checks, so a hint resolves a name
// as the full lookup would.
//
// It also maps a data root and a container ID to the container's identity:
// its name and its family, which never change for an ID: a container is never
// renamed, only its branches are. So a search of every container reads only
// the records it has not read before. The hints of each kind are forgotten
// whenever there are maxContainerNameHints of them.
var containerNameHints struct {
	mu         sync.Mutex
	ids        map[string]string
	identities map[string]containerIdentity
}

// containerIdentity is what never changes of a published container.
type containerIdentity struct {
	// name is the container's name, or empty for a physical runtime, which
	// no lookup by name or ID prefix finds.
	name string
	// family is the ID of the container's logical family.
	family string
}

const maxContainerNameHints = 1 << 16

// generatedContainerID reports whether identifier has the form of an ID TTC
// gives a container: a family's hexadecimal digits, or a distributed target's,
// which its transaction's ID names, "fork-" and hexadecimal digits.
func generatedContainerID(identifier string) bool {
	digits := strings.TrimPrefix(identifier, "fork-")
	if digits == identifier && len(digits) != 2*containerIDBytes || digits == "" {
		return false
	}
	for _, c := range []byte(digits) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func containerNameHint(key string) (string, bool) {
	containerNameHints.mu.Lock()
	defer containerNameHints.mu.Unlock()
	id, ok := containerNameHints.ids[key]
	return id, ok
}

func setContainerNameHint(key, id string) {
	containerNameHints.mu.Lock()
	defer containerNameHints.mu.Unlock()
	if containerNameHints.ids == nil || len(containerNameHints.ids) >= maxContainerNameHints {
		containerNameHints.ids = make(map[string]string)
	}
	containerNameHints.ids[key] = id
}

// publishedContainerIdentities returns the identities of the published
// containers, those ListContainersForPlacement lists, by ID. It reads the
// record of each it has not seen before.
func (s Store) publishedContainerIdentities() (map[string]containerIdentity, error) {
	entries, err := readDirWithStaleRetry(filepath.Join(s.Root, "containers"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	identities := make(map[string]containerIdentity, len(entries))
	var unknown []fs.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		containerNameHints.mu.Lock()
		identity, known := containerNameHints.identities[s.Root+"\x00"+id]
		containerNameHints.mu.Unlock()
		if !known {
			unknown = append(unknown, entry)
			continue
		}
		identities[id] = identity
	}
	// Several at once, as a listing reads them: one after another, the
	// records of the families made elsewhere since were round trips of the
	// shared root in turn.
	loaded := s.loadContainerKeys(unknown)
	for i, entry := range unknown {
		id := entry.Name()
		info, err := loaded[i].info, loaded[i].err
		if err != nil || info.DistributedForkHidden {
			// Unpublished, removed since the directory was read, or
			// unreadable, which a listing skips too; or a hidden target,
			// whose publication is yet to come.
			continue
		}
		identity := containerIdentity{family: info.ID}
		if info.DistributedFamilyID != "" {
			identity.family = info.DistributedFamilyID
		}
		if !info.IsDistributedPhysicalRuntime() {
			identity.name = info.Name
			setContainerNameHint(s.Root+"\x00"+info.Name, id)
		}
		containerNameHints.mu.Lock()
		if containerNameHints.identities == nil || len(containerNameHints.identities) >= maxContainerNameHints {
			containerNameHints.identities = make(map[string]containerIdentity)
		}
		containerNameHints.identities[s.Root+"\x00"+id] = identity
		containerNameHints.mu.Unlock()
		identities[id] = identity
	}
	return identities, nil
}

// checkContainerNameUnused fails if name is the name or the ID of a container.
func (s Store) checkContainerNameUnused(name string) error {
	existing, err := s.containersNamed(name)
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		return fmt.Errorf("container name %q is already in use by %s", name, existing[0].ID)
	}
	return nil
}

// containersNamed returns the containers listContainers(true) lists whose
// name or ID is name, without reading any other container's record.
func (s Store) containersNamed(name string) ([]ContainerInfo, error) {
	identities, err := s.publishedContainerIdentities()
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, identity := range identities {
		if identity.name == name {
			ids = append(ids, id)
		}
	}
	// A physical runtime, published or not, is named by its ID.
	if _, err := os.Lstat(filepath.Join(s.Root, "containers", name)); err == nil && !slices.Contains(ids, name) {
		ids = append(ids, name)
	}
	sort.Strings(ids)
	var containers []ContainerInfo
	for _, id := range ids {
		info, err := s.loadContainerKey(id)
		if err != nil {
			containerDir := filepath.Join(s.Root, "containers", id)
			if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
				continue
			}
			if _, statErr := os.Lstat(containerDir); errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		containers = append(containers, info)
	}
	return containers, nil
}

// FamilyContainerIDs returns the IDs of the published containers of the
// logical family familyID, its own and those of its physical runtimes.
func (s Store) FamilyContainerIDs(familyID string) ([]string, error) {
	identities, err := s.publishedContainerIdentities()
	if err != nil {
		return nil, err
	}
	ids := []string{familyID}
	for id, identity := range identities {
		if id != familyID && identity.family == familyID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// PrepareBranchStartBundle creates a short-lived OCI bundle that starts a
// branch from its persistent filesystem pointer without changing its visible
// branch identity. The canonical container bundle is never modified.
func (s Store) PrepareBranchStartBundle(name, branchID, filesystemBranch string, startPaused bool) (string, func(), error) {
	if branchID == "" || filesystemBranch == "" {
		return "", nil, errors.New("branch start identity and filesystem branch are required")
	}
	return s.prepareBranchBundle(name, branchID, filesystemBranch, false, startPaused)
}

// PrepareTransientBranchStartBundle creates a paused branch-start bundle whose
// anonymous application MemoryFile is private to a short-lived bootstrap
// Sentry. TTC uses this to construct a fresh branch beside an existing family.
func (s Store) PrepareTransientBranchStartBundle(name, branchID, filesystemBranch string) (string, func(), error) {
	if branchID == "" || filesystemBranch == "" {
		return "", nil, errors.New("transient branch start identity and filesystem branch are required")
	}
	return s.prepareBranchBundle(name, branchID, filesystemBranch, false, true)
}

// PrepareDehydrationRestoreBundle creates a short-lived OCI bundle that tells
// runsc to reserve an anonymous application MemoryFile for cold restore and
// opens every TTC-managed repository at filesystemBranch. The canonical
// container bundle is never modified.
func (s Store) PrepareDehydrationRestoreBundle(name, filesystemBranch string) (string, func(), error) {
	if filesystemBranch == "" {
		return "", nil, errors.New("dehydration restore filesystem branch is empty")
	}
	return s.prepareBranchBundle(name, "", filesystemBranch, true, false)
}

// PrepareDistributedTargetBundle creates the node-local OCI view used to boot
// a new hidden remote-fork target. The canonical bundle is shared metadata and
// may still contain another node's image-cache and repository paths.
func (s Store) PrepareDistributedTargetBundle(name, filesystemBranch string) (string, func(), error) {
	if filesystemBranch == "" {
		return "", nil, errors.New("distributed target filesystem identity is empty")
	}
	return s.prepareBranchBundle(name, "", filesystemBranch, true, false)
}

// PrepareDistributedBranchRestoreBundle creates a cold target bundle whose
// bootstrap SandboxFamily already uses the imported branch's visible identity.
// Distributed restore later replaces that placeholder Kernel in place; it
// must not silently fall back to the ordinary "main" bootstrap identity. This
// applies both to migration and to the first physical branch of an existing
// logical family placed on a new node.
func (s Store) PrepareDistributedBranchRestoreBundle(name, branchID, filesystemBranch string) (string, func(), error) {
	if branchID == "" || filesystemBranch == "" {
		return "", nil, errors.New("distributed restore branch or filesystem identity is empty")
	}
	return s.prepareBranchBundle(name, branchID, filesystemBranch, true, false)
}

func (s Store) prepareBranchBundle(name, branchID, filesystemBranch string, distributedTarget, startPaused bool) (string, func(), error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return "", nil, err
	}
	spec, err := s.LoadSpec(info.ID)
	if err != nil {
		return "", nil, err
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	delete(spec.Annotations, specutils.AnnotationTTCMemoryFile)
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return "", nil, err
	}
	for i := range repositories {
		if filesystemBranch != ttcvolume.MainBranch || repositories[i].Branch == "" {
			repositories[i].Branch = filesystemBranch
		}
	}
	if len(repositories) != 0 {
		encoded, err := json.Marshal(repositories)
		if err != nil {
			return "", nil, err
		}
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
	}
	if distributedTarget {
		spec.Annotations[specutils.AnnotationTTCDistributedTarget] = "true"
		if branchID != "" {
			var branch *BranchInfo
			for i := range info.Branches {
				if info.Branches[i].ID == branchID {
					branch = &info.Branches[i]
					break
				}
			}
			if branch == nil {
				// A distributed import is reserved before it becomes a stable
				// BranchInfo. It must nevertheless be the bootstrap identity when
				// this is the first physical Sentry for the family on this node.
				for _, pending := range info.DistributedForkImports {
					physicalBranch := pending.PhysicalBranchID
					if physicalBranch == "" {
						physicalBranch = pending.BranchID
					}
					if physicalBranch == branchID && pending.FilesystemBranch == filesystemBranch {
						branch = &BranchInfo{ID: physicalBranch, ParentID: pending.ParentID, FilesystemBranch: pending.FilesystemBranch}
						break
					}
				}
			}
			if branch == nil {
				return "", nil, fmt.Errorf("branch %q is not recorded", branchID)
			}
			spec.Annotations[specutils.AnnotationTTCInitialBranchID] = branchID
			spec.Annotations[specutils.AnnotationTTCInitialBranchParentID] = branch.ParentID
			spec.Annotations[specutils.AnnotationTTCInitialFilesystemBranch] = filesystemBranch
		}
	} else {
		var branch *BranchInfo
		for i := range info.Branches {
			if info.Branches[i].ID == branchID {
				branch = &info.Branches[i]
				break
			}
		}
		if branch == nil {
			return "", nil, fmt.Errorf("branch %q is not recorded", branchID)
		}
		delete(spec.Annotations, specutils.AnnotationTTCDistributedTarget)
		spec.Annotations[specutils.AnnotationTTCInitialBranchID] = branchID
		spec.Annotations[specutils.AnnotationTTCInitialBranchParentID] = branch.ParentID
		spec.Annotations[specutils.AnnotationTTCInitialFilesystemBranch] = filesystemBranch
		if startPaused {
			spec.Annotations[specutils.AnnotationTTCInitialStartPaused] = "true"
		} else {
			delete(spec.Annotations, specutils.AnnotationTTCInitialStartPaused)
		}
	}
	if spec.Root != nil && !filepath.IsAbs(spec.Root.Path) {
		spec.Root.Path = filepath.Join(info.BundleDir, spec.Root.Path)
	}
	tempParent := filepath.Dir(info.BundleDir)
	if s.TransientRoot != "" {
		tempParent = s.TransientRoot
		if err := os.MkdirAll(tempParent, 0700); err != nil {
			return "", nil, fmt.Errorf("creating transient bundle root: %w", err)
		}
	}
	temp, err := os.MkdirTemp(tempParent, ".branch-start-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	if err := writeJSON(filepath.Join(temp, "config.json"), spec); err != nil {
		cleanup()
		return "", nil, err
	}
	return temp, cleanup, nil
}

// PrepareOptions describes an image-backed OCI bundle.
type PrepareOptions struct {
	SpecOptions
	Reference          string
	Platform           string
	PullPolicy         string
	LogDriver          string
	LogOptions         map[string]string
	ConsoleLogMaxBytes int64
	ConsoleLogMaxFiles int
	OpenStdin          bool
	AutoRemove         bool
	StopTimeout        *int
	NetworkNAT         bool
	NetworkIP          string
	NetworkMAC         string
	NetworkInterface   string
	PublishedPorts     []PortBinding
	WaiveLiveFork      bool
	LiveForkWaiver     string
	Progress           images.ProgressFunc
	// GPUOptions are the family's GPU devices.
	GPUOptions
	// DeviceMounts are read-only mounts of the guest's side of the family's
	// devices, which the daemon provides: user-space drivers, and the files
	// that make the guest's libraries find them.
	DeviceMounts []HostMount
	// DeviceEnv are the environment variables, NAME=VALUE, that make the
	// guest's libraries use the family's devices. A container gets those that
	// its image and options leave unset.
	DeviceEnv []string
	// DeviceNodes are the host's devices that the family's containers have,
	// such as NVIDIA's for --nvproxy, which runsc's device gofer serves.
	DeviceNodes []specs.LinuxDevice
	// VolumeCopyMode is the copy policy already verified by ttcd at startup.
	// Empty permits Store-only callers to probe either supported mode.
	VolumeCopyMode ttcvolume.CopyMode
	NodeID         string
	// AllocateSnapshotOrigins returns globally monotonic working origins in
	// cluster mode. Nil selects the standalone repository-local allocator.
	AllocateSnapshotOrigins func(count uint32) ([]ttcsnapshot.SnapshotID, error)
	// SnapshotManagedVolume snapshots a named volume through its live owning
	// Sentry. This preserves bounded process-local metadata batching and mmap
	// CoW correctness when a second family attaches read-only.
	SnapshotManagedVolume func(ownerContainer, ownerBranch, volumeID, childBranch string, sourceWorking, childWorking ttcsnapshot.SnapshotID) error
}

func allocateRepositoryOrigins(opts *PrepareOptions, count uint32) ([]ttcsnapshot.SnapshotID, error) {
	if opts == nil || opts.AllocateSnapshotOrigins == nil {
		return nil, nil
	}
	origins, err := opts.AllocateSnapshotOrigins(count)
	if err != nil {
		return nil, err
	}
	if len(origins) != int(count) {
		return nil, fmt.Errorf("snapshot allocator returned %d origins, want %d", len(origins), count)
	}
	for i, origin := range origins {
		if origin == 0 {
			return nil, fmt.Errorf("snapshot allocator returned zero origin at index %d", i)
		}
	}
	return origins, nil
}

// Prepared is a generated OCI bundle ready for runsc.
type Prepared struct {
	BundleDir string
	RootFS    string
	Image     images.RootFSInfo
	Container ContainerInfo
}

// RestoreRepository identifies one unpacked ttcvol branch snapshot by its OCI
// repository source.
type RestoreRepository struct {
	Source       string
	SnapshotPath string
}

// RestoreHostMount identifies one manager-owned readonly bind captured in a
// checkpoint.
type RestoreHostMount struct {
	MountIndex   int
	Source       string
	SnapshotPath string
}

// RestoreOptions describes a standalone TTC checkpoint being materialized as
// a new, unrelated container family.
type RestoreOptions struct {
	Name             string
	Container        ContainerInfo
	Spec             *specs.Spec
	Repositories     []RestoreRepository
	HostMounts       []RestoreHostMount
	VolumeCopyMode   ttcvolume.CopyMode
	RestorePaused    bool
	CheckpointBranch string
	NodeID           string
	// AllocateSnapshotOrigins allocates the working origins of a clustered
	// node's repositories, as PrepareOptions' does.
	AllocateSnapshotOrigins func(uint32) ([]ttcsnapshot.SnapshotID, error)
}

// DistributedForkOptions materializes a hidden target bundle that refers to
// filesystem branches created atomically by the source Sentry. The source and
// target stores must name the same shared TTC repository.
type DistributedForkOptions struct {
	SourceContainer string
	// SourcePhysicalBranch identifies the exact source Sentry branch for a
	// migration whose logical branch has already crossed nodes before.
	SourcePhysicalBranch string
	TargetName           string
	TargetBranch         string
	VisibleBranch        string
	ParentBranch         string
	TransactionID        string
	NodeID               string
	SourceNode           string
	Migration            bool
	// AuthoritativeMigration means the target daemon has already validated the
	// exact source, target, and operation against cluster authority. Physical
	// family metadata may retain the branch's former local owner after an
	// earlier migration and must not override that placement decision.
	AuthoritativeMigration bool
	TargetID               string
	// Source and Spec are supplied by the source daemon over the authenticated
	// cluster control connection. They avoid using shared-filesystem directory
	// visibility as a target-reservation synchronization primitive.
	Source *ContainerInfo
	Spec   *specs.Spec
}

// BranchInfo is branch display lineage plus its last known lifecycle state.
type BranchInfo struct {
	ID string `json:"id"`
	// Quarantined disconnects the branch's runtime-managed external network
	// edge while preserving its Sentry-owned network and socket state.
	Quarantined bool `json:"quarantined,omitempty"`
	// NodeID is the branch's physical owner in clustered mode. It is empty for
	// standalone branches and for clustered branches marked Unplaced.
	NodeID string `json:"node_id,omitempty"`
	// Unplaced identifies a durable clustered branch with no physical owner.
	Unplaced bool `json:"unplaced,omitempty"`
	// ParentID is display provenance, not an ownership edge. Removing a branch
	// reparents its direct children to this branch's parent.
	ParentID                     string `json:"parent_id,omitempty"`
	FilesystemBranch             string `json:"filesystem_branch,omitempty"`
	DistributedForkTransactionID string `json:"distributed_fork_transaction_id,omitempty"`
	// DistributedGraphReferenceOperation names this branch's independent,
	// idempotent provider-root batch. Multiple local descendants may share the
	// same authorizing distributed transaction, but must never share this
	// lifetime identity.
	DistributedGraphReferenceOperation string     `json:"distributed_graph_reference_operation,omitempty"`
	DistributedMigration               bool       `json:"distributed_migration,omitempty"`
	State                              string     `json:"state"`
	ExitStatus                         *int       `json:"exit_status,omitempty"`
	ExitedAt                           *time.Time `json:"exited_at,omitempty"`
	// StorageState describes where the authoritative captured snapshot is.
	// It is independent of the process lifecycle in State. StorageResident
	// marks a dehydrated branch whose capture only its family's Sentry holds.
	StorageState             string    `json:"storage_state,omitempty"`
	PublicationID            string    `json:"publication_id,omitempty"`
	StorageError             string    `json:"storage_error,omitempty"`
	StorageManifest          string    `json:"storage_manifest,omitempty"`
	StorageRetryAt           time.Time `json:"storage_retry_at,omitempty"`
	SourceNode               string    `json:"source_node,omitempty"`
	DependentMemoryBytes     uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64    `json:"dependent_filesystem_bytes,omitempty"`
	SnapshotID               uint64    `json:"snapshot_id,omitempty"`
	MemoryRoots              []string  `json:"memory_roots,omitempty"`
	// MemoryRootObjects records the immutable object selected by each logical
	// root name. Names are branch-local handles; object IDs remain stable when
	// authority moves from a node to shared storage.
	MemoryRootObjects []string `json:"memory_root_objects,omitempty"`
	// ControlRoot and ControlRootObject retain the immutable filesystem tree
	// containing a process-free branch's kernel/process metadata. They are
	// present only while a branch is dehydrated; live branches have already
	// consumed this auxiliary snapshot root.
	ControlRoot                 string    `json:"control_root,omitempty"`
	ControlRootObject           string    `json:"control_root_object,omitempty"`
	ForkTotalNS                 int64     `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS            int64     `json:"source_downtime_ns,omitempty"`
	KernelForkNS                int64     `json:"kernel_fork_ns,omitempty"`
	ForkValidationNS            int64     `json:"fork_validation_ns,omitempty"`
	ForkSharedMemoryNS          int64     `json:"fork_shared_memory_ns,omitempty"`
	ForkMappingInvalidationNS   int64     `json:"fork_mapping_invalidation_ns,omitempty"`
	ForkVFSPreparationNS        int64     `json:"fork_vfs_preparation_ns,omitempty"`
	ForkDevicePreparationNS     int64     `json:"fork_device_preparation_ns,omitempty"`
	ForkFSMetadataFlushNS       int64     `json:"fork_fs_metadata_flush_ns,omitempty"`
	ForkFSRuntimeInvalidationNS int64     `json:"fork_fs_runtime_invalidation_ns,omitempty"`
	ForkFSBranchSealNS          int64     `json:"fork_fs_branch_seal_ns,omitempty"`
	ForkFSNamespacePruneNS      int64     `json:"fork_fs_namespace_prune_ns,omitempty"`
	ForkMemoryPreparationNS     int64     `json:"fork_memory_preparation_ns,omitempty"`
	ForkMetadataStreamNS        int64     `json:"fork_metadata_stream_ns,omitempty"`
	ForkMetadataBytes           int64     `json:"fork_metadata_bytes,omitempty"`
	ForkMetadataRecordNS        int64     `json:"fork_metadata_record_ns,omitempty"`
	ForkMetadataObjects         int64     `json:"fork_metadata_objects,omitempty"`
	ForkChildRestoreNS          int64     `json:"fork_child_restore_ns,omitempty"`
	CreatedAt                   time.Time `json:"created_at"`
}

// RuntimeMemoryBacking retains an immutable process-memory graph while a live
// family MemoryFile may still demand pages from it. Unlike BranchInfo, this is
// deliberately family-scoped: destroying a branch can transfer its page
// loader to any surviving sibling, so the backing no longer belongs to the
// branch that originally introduced it.
type RuntimeMemoryBacking struct {
	ID                                 string   `json:"id"`
	SnapshotID                         uint64   `json:"snapshot_id"`
	MemoryRoots                        []string `json:"memory_roots"`
	MemoryRootObjects                  []string `json:"memory_root_objects"`
	ControlRoot                        string   `json:"control_root,omitempty"`
	ControlRootObject                  string   `json:"control_root_object,omitempty"`
	DistributedGraphReferenceOperation string   `json:"distributed_graph_reference_operation,omitempty"`
}

// StorageResident is the StorageState of a dehydrated branch whose capture
// exists only in its family's Sentry on its node.
//
// Dehydration used to make every capture durable at once: its memory went
// into the snapshot graph and its control state into the data root. While
// another branch of the family runs on the same node the Sentry stays up
// anyway, and the capture shares nearly all of its pages with that branch, so
// the copy bought little and cost as much as the branch's whole memory. The
// capture is made durable when the family's last live branch on the node goes
// instead, and until then a lost runtime loses it the way it loses a running
// branch.
const StorageResident = "resident"

// ResidentDehydration reports whether branch is dehydrated and its capture
// only its family's Sentry holds.
func ResidentDehydration(branch BranchInfo) bool {
	return branch.State == "dehydrated" && branch.StorageState == StorageResident
}

// BranchNodeID returns the explicit cluster owner of branchID.
func (info ContainerInfo) BranchNodeID(branchID string) string {
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			if branch.Unplaced {
				return ""
			}
			return branch.NodeID
		}
	}
	return ""
}

// MemoryBranchKey returns the stable directory key used for a branch's
// dehydration metadata and active page sources. It deliberately uses
// the filesystem branch identity rather than the user-visible branch name so
// renaming a branch never changes where its durable memory is stored.
func MemoryBranchKey(branch BranchInfo) string {
	return hex.EncodeToString([]byte(branch.FilesystemBranch))
}

// DistributedForkImport is a reserved but not yet visible branch imported
// into an already-running target family.
type DistributedForkImport struct {
	BranchID string `json:"branch_id"`
	// PhysicalBranchID is the transaction-scoped Sentry branch that implements
	// BranchID. It differs for migrations so an older, paused source incarnation
	// can continue serving sparse pages while a newer incarnation returns to the
	// same Sentry. BranchID remains the stable user-visible authority identity.
	PhysicalBranchID         string    `json:"physical_branch_id,omitempty"`
	Quarantined              bool      `json:"quarantined,omitempty"`
	ParentID                 string    `json:"parent_id,omitempty"`
	FilesystemBranch         string    `json:"filesystem_branch"`
	State                    string    `json:"state"`
	CreatedAt                time.Time `json:"created_at"`
	SourceNode               string    `json:"source_node,omitempty"`
	TargetNode               string    `json:"target_node,omitempty"`
	Migration                bool      `json:"migration,omitempty"`
	AuthoritativeMigration   bool      `json:"authoritative_migration,omitempty"`
	DependentMemoryBytes     uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64    `json:"dependent_filesystem_bytes,omitempty"`
	SnapshotID               uint64    `json:"snapshot_id,omitempty"`
	MemoryRoots              []string  `json:"memory_roots,omitempty"`
	MemoryRootObjects        []string  `json:"memory_root_objects,omitempty"`
	GraphReferenceOperation  string    `json:"graph_reference_operation,omitempty"`
}

// PortBinding describes a TCP listener managed by TTC for one live branch.
// BranchID is always explicit; no branch, including main, is implicit.
type PortBinding struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	BranchID      string `json:"branch_id,omitempty"`
}

// Branch returns the branch targeted by p.
func (p PortBinding) Branch() string {
	return p.BranchID
}

// ContainerInfo is the persistent configuration needed to start a container.
type ContainerInfo struct {
	ID                           string                           `json:"id"`
	Name                         string                           `json:"name"`
	Reference                    string                           `json:"reference"`
	Digest                       string                           `json:"digest"`
	ImageSize                    int64                            `json:"image_size,omitempty"`
	LeaseID                      string                           `json:"lease_id,omitempty"`
	Platform                     string                           `json:"platform"`
	BundleDir                    string                           `json:"bundle_dir"`
	RootFS                       string                           `json:"rootfs"`
	Branches                     []BranchInfo                     `json:"branches,omitempty"`
	RuntimeMemoryBackings        []RuntimeMemoryBacking           `json:"runtime_memory_backings,omitempty"`
	NetworkMode                  string                           `json:"network_mode,omitempty"`
	LiveForkEligible             bool                             `json:"live_fork_eligible"`
	LiveForkWaiver               string                           `json:"live_fork_waiver,omitempty"`
	NetworkNAT                   bool                             `json:"network_nat,omitempty"`
	NetworkIP                    string                           `json:"network_ip,omitempty"`
	NetworkMAC                   string                           `json:"network_mac,omitempty"`
	NetworkInterface             string                           `json:"network_interface,omitempty"`
	PublishedPorts               []PortBinding                    `json:"published_ports,omitempty"`
	LogDriver                    string                           `json:"log_driver,omitempty"`
	LogOptions                   map[string]string                `json:"log_options,omitempty"`
	LogAttributes                map[string]string                `json:"log_attributes,omitempty"`
	ConsoleLogMaxBytes           int64                            `json:"console_log_max_bytes,omitempty"`
	ConsoleLogMaxFiles           int                              `json:"console_log_max_files,omitempty"`
	Terminal                     bool                             `json:"terminal,omitempty"`
	OpenStdin                    bool                             `json:"open_stdin,omitempty"`
	AutoRemove                   bool                             `json:"auto_remove,omitempty"`
	StopSignal                   string                           `json:"stop_signal,omitempty"`
	StopTimeout                  *int                             `json:"stop_timeout,omitempty"`
	PidsLimit                    *int64                           `json:"pids_limit,omitempty"`
	MemoryLimit                  *int64                           `json:"memory_limit,omitempty"`
	MemorySwap                   *int64                           `json:"memory_swap,omitempty"`
	NanoCPUs                     *int64                           `json:"nano_cpus,omitempty"`
	CPUShares                    *uint64                          `json:"cpu_shares,omitempty"`
	CPUPeriod                    *uint64                          `json:"cpu_period,omitempty"`
	CPUQuota                     *int64                           `json:"cpu_quota,omitempty"`
	ShmSize                      *int64                           `json:"shm_size,omitempty"`
	GroupAdd                     []string                         `json:"group_add,omitempty"`
	CapAdd                       []string                         `json:"cap_add,omitempty"`
	CapDrop                      []string                         `json:"cap_drop,omitempty"`
	Ulimits                      []Ulimit                         `json:"ulimits,omitempty"`
	ExtraHosts                   []string                         `json:"extra_hosts,omitempty"`
	DNS                          []string                         `json:"dns,omitempty"`
	DNSSearch                    []string                         `json:"dns_search,omitempty"`
	DNSOptions                   []string                         `json:"dns_options,omitempty"`
	Volumes                      []ManagedVolume                  `json:"volumes,omitempty"`
	RootfsUpperID                string                           `json:"rootfs_upper_id,omitempty"`
	RootfsUpperCopyMode          string                           `json:"rootfs_upper_copy_mode,omitempty"`
	State                        string                           `json:"state"`
	ExitStatus                   *int                             `json:"exit_status,omitempty"`
	OOMKilled                    bool                             `json:"oom_killed,omitempty"`
	ExitError                    string                           `json:"exit_error,omitempty"`
	CreatedAt                    time.Time                        `json:"created_at"`
	StartedAt                    *time.Time                       `json:"started_at,omitempty"`
	ExitedAt                     *time.Time                       `json:"exited_at,omitempty"`
	DistributedForkTransactionID string                           `json:"distributed_fork_transaction_id,omitempty"`
	DistributedForkNodeID        string                           `json:"distributed_fork_node_id,omitempty"`
	DistributedForkHidden        bool                             `json:"distributed_fork_hidden,omitempty"`
	DistributedPhysicalRuntime   bool                             `json:"distributed_physical_runtime,omitempty"`
	DistributedFamilyID          string                           `json:"distributed_family_id,omitempty"`
	DistributedForkImports       map[string]DistributedForkImport `json:"distributed_fork_imports,omitempty"`
	NodeID                       string                           `json:"node_id,omitempty"`
	// DistributedCatalogOnly marks the canonical identity record after its
	// final physical branch has been removed while logical sibling placements
	// still exist in the authority. Such a record intentionally has no local branches;
	// user-visible branch inventory is synthesized from placement authority.
	DistributedCatalogOnly bool `json:"distributed_catalog_only,omitempty"`
	// GPUOptions are the family's GPU devices.
	GPUOptions
}

// TTCNetworkInterface returns the guest's name for the container's ttc
// virtual NIC.
func (info ContainerInfo) TTCNetworkInterface() string {
	if info.NetworkInterface != "" {
		return info.NetworkInterface
	}
	return LegacyNetworkInterface
}

// IsDistributedPhysicalRuntime reports whether info is a node-local physical
// materialization behind a cluster-wide logical family. Transaction identity
// exists only while a fork or migration is in flight, so it cannot by itself
// classify a runtime after publication or a later cold restart.
func (info ContainerInfo) IsDistributedPhysicalRuntime() bool {
	return info.DistributedPhysicalRuntime || info.DistributedForkTransactionID != "" || info.DistributedForkHidden ||
		// Compatibility for physical records created before the explicit role
		// bit: internal physical runtimes are named by their generated ID.
		(info.DistributedFamilyID != "" && info.DistributedFamilyID != info.ID && info.Name == info.ID)
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func buildHostsFile(extraHosts []string) string {
	var result strings.Builder
	result.WriteString("127.0.0.1\tlocalhost\n")
	result.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	result.WriteString("fe00::\tip6-localnet\n")
	result.WriteString("ff00::\tip6-mcastprefix\n")
	result.WriteString("ff02::1\tip6-allnodes\n")
	result.WriteString("ff02::2\tip6-allrouters\n")
	for _, entry := range extraHosts {
		host, address, _ := strings.Cut(entry, ":")
		fmt.Fprintf(&result, "%s\t%s\n", address, host)
	}
	return result.String()
}

func hostResolverConfig(data []byte) (nameservers, search, options []string) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], ";") {
			continue
		}
		switch fields[0] {
		case "nameserver":
			nameservers = append(nameservers, fields[1])
		case "search":
			search = append([]string(nil), fields[1:]...)
		case "domain":
			if len(search) == 0 {
				search = []string{fields[1]}
			}
		case "options":
			options = append(options, fields[1:]...)
		}
	}
	return nameservers, search, options
}

func buildResolvConf(hostData []byte, dns, search, options []string) string {
	return buildResolvConfForNetwork(hostData, dns, search, options, "none")
}

func buildResolvConfForNetwork(hostData []byte, dns, search, options []string, networkMode string) string {
	hostDNS, hostSearch, hostOptions := hostResolverConfig(hostData)
	if len(dns) == 0 {
		dns = hostDNS
	}
	if len(dns) == 0 {
		dns = []string{"127.0.0.1"}
	}
	if networkMode == "ttc" {
		dns = append([]string(nil), dns...)
		for i, value := range dns {
			address, err := netip.ParseAddr(value)
			if err != nil || !address.IsLoopback() {
				continue
			}
			last := byte(2)
			if address.Is4() {
				last = address.As4()[3]
				if last == 1 || last == 15 || last == 255 {
					last = 2
				}
			}
			dns[i] = netip.AddrFrom4([4]byte{10, 0, 2, last}).String()
		}
	}
	if len(search) == 0 {
		search = hostSearch
	}
	if len(options) == 0 {
		options = hostOptions
	}
	var result strings.Builder
	for _, address := range dns {
		fmt.Fprintf(&result, "nameserver %s\n", address)
	}
	if len(search) != 0 {
		fmt.Fprintf(&result, "search %s\n", strings.Join(search, " "))
	}
	if len(options) != 0 {
		fmt.Fprintf(&result, "options %s\n", strings.Join(options, " "))
	}
	return result.String()
}

func materializeNetworkFiles(containerDir string, opts SpecOptions) ([]HostMount, error) {
	networkDir := filepath.Join(containerDir, "network")
	if err := os.Mkdir(networkDir, 0700); err != nil {
		return nil, fmt.Errorf("creating container network configuration: %w", err)
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = opts.Name
	}
	hostResolv, err := readFileWithStaleRetry("/etc/resolv.conf")
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading host resolver configuration: %w", err)
	}
	files := []struct {
		name        string
		destination string
		contents    string
	}{
		{name: "hostname", destination: "/etc/hostname", contents: hostname + "\n"},
		{name: "hosts", destination: "/etc/hosts", contents: buildHostsFile(opts.ExtraHosts)},
		{name: "resolv.conf", destination: "/etc/resolv.conf", contents: buildResolvConfForNetwork(hostResolv, opts.DNS, opts.DNSSearch, opts.DNSOptions, opts.NetworkMode)},
	}
	mounts := make([]HostMount, 0, len(files))
	for _, file := range files {
		name := filepath.Join(networkDir, file.name)
		if err := os.WriteFile(name, []byte(file.contents), 0444); err != nil {
			return nil, fmt.Errorf("writing container network file %q: %w", file.destination, err)
		}
		// ttcd starts with a restrictive umask because most data-root state is
		// private. Docker-generated network files are intentionally readable by
		// every virtual container user, so enforce their final mode explicitly.
		if err := os.Chmod(name, 0444); err != nil {
			return nil, fmt.Errorf("setting permissions on container network file %q: %w", file.destination, err)
		}
		mounts = append(mounts, HostMount{Source: name, Destination: file.destination, Readonly: true, Internal: true})
	}
	return mounts, nil
}

func ensureNetworkMountpoints(rootfs string) error {
	manifestName := ownership.ManifestPath(rootfs)
	manifest, err := ownership.Read(manifestName)
	if err != nil {
		return fmt.Errorf("loading rootfs ownership for network mountpoints: %w", err)
	}
	// Android's images link /etc to /system/etc: the container's mounts on
	// /etc follow the link, and so do their mountpoints.
	etcPath, err := resolveInRootfs(rootfs, "etc")
	if err != nil {
		return fmt.Errorf("resolving rootfs /etc: %w", err)
	}
	etc := filepath.Join(rootfs, etcPath)
	if info, err := os.Lstat(etc); os.IsNotExist(err) {
		if err := os.Mkdir(etc, 0755); err != nil {
			return fmt.Errorf("creating rootfs /etc mountpoint directory: %w", err)
		}
		manifest.Entries[filepath.ToSlash(etcPath)] = ownership.Owner{}
	} else if err != nil {
		return fmt.Errorf("checking rootfs /etc mountpoint directory: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("rootfs /etc is not a directory")
	}
	for _, base := range []string{"hostname", "hosts", "resolv.conf"} {
		name := filepath.Join(etc, base)
		info, err := os.Lstat(name)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return fmt.Errorf("checking rootfs /etc/%s mountpoint: %w", base, err)
		case info.Mode().IsRegular():
			manifest.Entries[filepath.ToSlash(filepath.Join(etcPath, base))] = ownership.Owner{}
			continue
		case info.Mode()&os.ModeSymlink != 0:
			if err := os.Remove(name); err != nil {
				return fmt.Errorf("replacing rootfs /etc/%s symlink: %w", base, err)
			}
		default:
			return fmt.Errorf("rootfs /etc/%s is not a regular file or symlink", base)
		}
		if err := os.WriteFile(name, nil, 0644); err != nil {
			return fmt.Errorf("creating rootfs /etc/%s mountpoint: %w", base, err)
		}
		manifest.Entries[filepath.ToSlash(filepath.Join(etcPath, base))] = ownership.Owner{}
	}
	if err := ownership.Write(manifestName, manifest); err != nil {
		return fmt.Errorf("updating rootfs ownership for network mountpoints: %w", err)
	}
	return nil
}

// resolveInRootfs returns the path, relative to rootfs, that the container
// reaches by name, a path relative to its root: it follows the symbolic
// links on the way, as the container would, without leaving rootfs. The
// components after the first that does not exist are taken as they are.
func resolveInRootfs(rootfs, name string) (string, error) {
	const maxLinks = 40
	links := 0
	missing := false
	var resolved []string
	pending := strings.Split(filepath.ToSlash(name), "/")
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) != 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}
		if missing {
			resolved = append(resolved, component)
			continue
		}
		host := filepath.Join(append([]string{rootfs}, append(resolved, component)...)...)
		info, err := os.Lstat(host)
		if os.IsNotExist(err) {
			missing = true
			resolved = append(resolved, component)
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, component)
			continue
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("%q: %w", name, syscall.ELOOP)
		}
		target, err := os.Readlink(host)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			resolved = nil
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return filepath.Join(resolved...), nil
}

// rehomeInternalMounts gives a distributed target its own copies of TTC's
// generated bind-mounted files. Leaving these paths in the source container
// directory would make the target unstartable after the source is removed.
func rehomeInternalMounts(spec *specs.Spec, targetContainerDir string) error {
	if spec == nil || spec.Annotations == nil {
		return nil
	}
	encoded := spec.Annotations[InternalMountsAnnotation]
	if encoded == "" {
		return nil
	}
	var destinations []string
	if err := json.Unmarshal([]byte(encoded), &destinations); err != nil {
		return fmt.Errorf("decoding TTC internal mounts: %w", err)
	}
	internal := make(map[string]struct{}, len(destinations))
	for _, destination := range destinations {
		internal[filepath.Clean(destination)] = struct{}{}
	}
	directory := filepath.Join(targetContainerDir, "internal-mounts")
	for i := range spec.Mounts {
		mount := &spec.Mounts[i]
		if _, ok := internal[filepath.Clean(mount.Destination)]; !ok {
			continue
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			return fmt.Errorf("reading TTC internal mount %q: %w", mount.Destination, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("TTC internal mount %q is not a regular file", mount.Destination)
		}
		contents, err := readFileWithStaleRetry(mount.Source)
		if err != nil {
			return fmt.Errorf("copying TTC internal mount %q: %w", mount.Destination, err)
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		name := filepath.Join(directory, fmt.Sprintf("%d-%s", i, filepath.Base(mount.Destination)))
		if err := os.WriteFile(name, contents, info.Mode().Perm()); err != nil {
			return fmt.Errorf("materializing TTC internal mount %q: %w", mount.Destination, err)
		}
		mount.Source = name
	}
	return nil
}

// PortableInternalMount is the content of a TTC-generated bind mount without
// its node-local source pathname.
type PortableInternalMount struct {
	Destination string
	Contents    []byte
	Mode        os.FileMode
}

// PortableInternalMounts captures TTC-generated mounts from this store's
// node-local OCI view. It never includes user-supplied host mounts.
func (s Store) PortableInternalMounts(name string) ([]PortableInternalMount, error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return nil, err
	}
	spec, err := s.LoadSpec(info.ID)
	if err != nil {
		return nil, err
	}
	if spec.Annotations == nil || spec.Annotations[InternalMountsAnnotation] == "" {
		return nil, nil
	}
	var destinations []string
	if err := json.Unmarshal([]byte(spec.Annotations[InternalMountsAnnotation]), &destinations); err != nil {
		return nil, fmt.Errorf("decoding TTC internal mounts: %w", err)
	}
	internal := make(map[string]struct{}, len(destinations))
	for _, destination := range destinations {
		internal[filepath.Clean(destination)] = struct{}{}
	}
	result := make([]PortableInternalMount, 0, len(destinations))
	for _, mount := range spec.Mounts {
		if _, ok := internal[filepath.Clean(mount.Destination)]; !ok {
			continue
		}
		contents, err := readFileWithStaleRetry(mount.Source)
		if err != nil {
			return nil, fmt.Errorf("reading TTC internal mount %q: %w", mount.Destination, err)
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			return nil, fmt.Errorf("stating TTC internal mount %q: %w", mount.Destination, err)
		}
		result = append(result, PortableInternalMount{
			Destination: filepath.Clean(mount.Destination), Contents: contents, Mode: info.Mode().Perm(),
		})
	}
	if len(result) != len(internal) {
		return nil, fmt.Errorf("OCI spec describes %d TTC internal mounts but only %d sources", len(internal), len(result))
	}
	return result, nil
}

// InstallPortableInternalMounts replaces node-local sources in a container's
// persistent OCI specification. The files themselves are kept in the active
// node-local root. This is required for a migrated container to act as the
// source of a later remote fork without referring back to the previous node's
// private scratch filesystem.
func (s Store) InstallPortableInternalMounts(name string, mounts []PortableInternalMount) error {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return err
	}
	configPath := filepath.Join(info.BundleDir, "config.json")
	data, err := readFileWithStaleRetry(configPath)
	if err != nil {
		return err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if err := installPortableInternalMounts(&spec, filepath.Join(s.activeContainerDir(info.ID), "internal-mounts"), mounts); err != nil {
		return err
	}
	if s.activeRoot() != s.Root {
		// The canonical OCI bundle is shared cluster metadata. Node-local mount
		// paths must never be written into it: LoadSpec overlays the local paths
		// whenever this node has materialized the portable files.
		return nil
	}
	return writeJSON(configPath, &spec)
}

// MaterializePortableInternalMounts replaces node-local sources in a prepared
// transient bundle with target-local files. The caller must supply exactly the
// destinations declared by the bundle annotation.
func MaterializePortableInternalMounts(bundleDir string, mounts []PortableInternalMount) error {
	return materializePortableInternalMounts(bundleDir, filepath.Join(bundleDir, "internal-mounts"), mounts)
}

func materializePortableInternalMounts(bundleDir, directory string, mounts []PortableInternalMount) error {
	configPath := filepath.Join(bundleDir, "config.json")
	data, err := readFileWithStaleRetry(configPath)
	if err != nil {
		return err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if err := installPortableInternalMounts(&spec, directory, mounts); err != nil {
		return err
	}
	return writeJSON(configPath, &spec)
}

func installPortableInternalMounts(spec *specs.Spec, directory string, mounts []PortableInternalMount) error {
	if spec.Annotations == nil || spec.Annotations[InternalMountsAnnotation] == "" {
		if len(mounts) != 0 {
			return errors.New("remote session supplied undeclared TTC internal mounts")
		}
		return nil
	}
	var destinations []string
	if err := json.Unmarshal([]byte(spec.Annotations[InternalMountsAnnotation]), &destinations); err != nil {
		return fmt.Errorf("decoding TTC internal mounts: %w", err)
	}
	provided := make(map[string]PortableInternalMount, len(mounts))
	for _, mount := range mounts {
		provided[filepath.Clean(mount.Destination)] = mount
	}
	// A reader of the container's spec, which overlays the mounts in
	// directory once it exists, must find all of them: a directory made
	// here is filled under another name, and renamed into place.
	filled := directory
	if _, err := os.Lstat(directory); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
			return err
		}
		staging, err := os.MkdirTemp(filepath.Dir(directory), "."+filepath.Base(directory)+".tmp-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		filled = staging
	} else if err != nil {
		return err
	}
	install := func(into string) (int, error) {
		rewritten := 0
		for i := range spec.Mounts {
			mount := &spec.Mounts[i]
			portable, ok := provided[filepath.Clean(mount.Destination)]
			if !ok {
				continue
			}
			base := fmt.Sprintf("%d-%s", i, filepath.Base(mount.Destination))
			if err := replacePortableInternalMount(filepath.Join(into, base), portable.Contents, portable.Mode.Perm()); err != nil {
				return rewritten, fmt.Errorf("materializing TTC internal mount %q: %w", mount.Destination, err)
			}
			mount.Source = filepath.Join(directory, base)
			rewritten++
		}
		return rewritten, nil
	}
	rewritten, err := install(filled)
	if err != nil {
		return err
	}
	if rewritten != len(destinations) || rewritten != len(provided) {
		return fmt.Errorf("portable TTC internal mounts do not match OCI declarations: declared=%d supplied=%d matched=%d", len(destinations), len(provided), rewritten)
	}
	if filled != directory {
		if err := os.Rename(filled, directory); err != nil {
			// Another installation made the directory meanwhile; the
			// files are replaced in it one by one, each atomically.
			if _, statErr := os.Lstat(directory); statErr != nil {
				return err
			}
			if _, err := install(directory); err != nil {
				return err
			}
		}
	}
	return nil
}

// replacePortableInternalMount atomically installs an immutable internal
// mount. These files are commonly mode 0444, so a later remote fork imported
// into the same physical family cannot update them with os.WriteFile: opening
// the existing inode for truncation fails with EACCES. Replacing the directory
// entry also avoids exposing a partially rewritten hostname/hosts/resolv.conf
// to a concurrently starting branch.
func replacePortableInternalMount(path string, contents []byte, mode os.FileMode) error {
	if existing, err := readFileWithStaleRetry(path); err == nil {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm() == mode && bytes.Equal(existing, contents) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
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
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// ConsolePaths are persistent host-side files for one branch-local console.
// None of these files are donated into the sandbox.
type ConsolePaths struct {
	// FamilyDir is the console directory of the branch's family, where its
	// console follower's socket is.
	FamilyDir      string
	Log            string
	Lock           string
	StdinLock      string
	Error          string
	WaitStatus     string
	RetentionStats string
	PID            string
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func selectLogAttributes(options map[string]string, spec *specs.Spec) (map[string]string, error) {
	labels := make(map[string]string)
	if spec != nil && spec.Annotations != nil {
		if value := AnnotationValue(spec.Annotations, "dev.timetravelcontainer.config.labels"); value != "" {
			if err := json.Unmarshal([]byte(value), &labels); err != nil {
				return nil, fmt.Errorf("decoding labels for log attributes: %w", err)
			}
		}
	}
	environment := make(map[string]string)
	if spec != nil && spec.Process != nil {
		for _, assignment := range spec.Process.Env {
			if key, value, ok := strings.Cut(assignment, "="); ok {
				environment[key] = value
			}
		}
	}
	attributes := make(map[string]string)
	addNames := func(optionNames []string, source map[string]string) {
		for _, optionName := range optionNames {
			for _, name := range strings.Split(options[optionName], ",") {
				name = strings.TrimSpace(name)
				if value, ok := source[name]; ok {
					attributes[name] = value
				}
			}
		}
	}
	addRegex := func(optionNames []string, source map[string]string) error {
		for _, optionName := range optionNames {
			pattern := options[optionName]
			if pattern == "" {
				continue
			}
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("invalid log option %s=%q: %w", optionName, pattern, err)
			}
			for name, value := range source {
				if compiled.MatchString(name) {
					attributes[name] = value
				}
			}
		}
		return nil
	}
	addNames([]string{"labels", "label"}, labels)
	if err := addRegex([]string{"labels-regex", "label-regex"}, labels); err != nil {
		return nil, err
	}
	addNames([]string{"env"}, environment)
	if err := addRegex([]string{"env-regex"}, environment); err != nil {
		return nil, err
	}
	return attributes, nil
}

// Prepare resolves or pulls content, creates or reuses an immutable root, and
// writes a new container bundle. Existing container names are rejected.
func (s Store) Prepare(ctx context.Context, opts PrepareOptions) (Prepared, error) {
	logDriver := opts.LogDriver
	if logDriver == "" {
		logDriver = DefaultLogDriver
	}
	switch logDriver {
	case "local", "json-file", "none":
	default:
		return Prepared{}, fmt.Errorf("unsupported log driver %q", logDriver)
	}
	if opts.StopTimeout != nil && *opts.StopTimeout < -1 {
		return Prepared{}, fmt.Errorf("stop timeout must be -1 or greater")
	}
	if err := validateResourceLimits(opts.SpecOptions); err != nil {
		return Prepared{}, err
	}
	networkMode, err := normalizeNetworkMode(opts.NetworkMode)
	if err != nil {
		return Prepared{}, err
	}
	opts.NetworkMode = networkMode
	if opts.NetworkNAT && networkMode != "ttc" {
		return Prepared{}, fmt.Errorf("NAT requires network mode ttc")
	}
	if (opts.NetworkIP != "" || opts.NetworkMAC != "") && networkMode != "ttc" {
		return Prepared{}, fmt.Errorf("custom IP and MAC addresses require network mode ttc")
	}
	if opts.NetworkInterface != "" {
		if networkMode != "ttc" {
			return Prepared{}, fmt.Errorf("a custom interface name requires network mode ttc")
		}
		if err := config.ValidateNetworkInterfaceName(opts.NetworkInterface); err != nil {
			return Prepared{}, err
		}
	} else if networkMode == "ttc" {
		// Record the name, so that the container keeps it whatever later
		// releases default to.
		opts.NetworkInterface = DefaultNetworkInterface
	}
	if len(opts.PublishedPorts) != 0 && networkMode != "ttc" {
		return Prepared{}, fmt.Errorf("published ports require network mode ttc")
	}
	liveForkEligible := !opts.WaiveLiveFork
	if liveForkEligible && opts.LiveForkWaiver != "" {
		return Prepared{}, fmt.Errorf("live-fork-eligible container cannot record waiver %q", opts.LiveForkWaiver)
	}
	if liveForkEligible && networkMode == "host" {
		return Prepared{}, fmt.Errorf("network mode host is incompatible with live fork eligibility; explicitly waive eligibility during ttc run")
	}
	if !liveForkEligible && opts.LiveForkWaiver == "" {
		return Prepared{}, fmt.Errorf("ineligible container requires an explicit live fork waiver")
	}
	if err := ValidateGPUOptions(opts.GPUOptions, liveForkEligible, runtime.GOOS); err != nil {
		return Prepared{}, err
	}
	opts.SpecOptions.WaiveLiveFork = opts.WaiveLiveFork
	opts.ExtraHosts, err = ParseExtraHosts(opts.ExtraHosts)
	if err != nil {
		return Prepared{}, err
	}
	opts.DNS, err = ParseDNS(opts.DNS)
	if err != nil {
		return Prepared{}, err
	}
	opts.DNSSearch, err = ParseDNSSearch(opts.DNSSearch)
	if err != nil {
		return Prepared{}, err
	}
	opts.DNSOptions, err = ParseDNSOptions(opts.DNSOptions)
	if err != nil {
		return Prepared{}, err
	}
	if err := validateName(opts.Name); err != nil {
		return Prepared{}, err
	}
	if err := s.checkContainerNameUnused(opts.Name); err != nil {
		return Prepared{}, err
	}
	pulled, err := s.resolveImage(ctx, opts.Reference, opts.Platform, opts.PullPolicy, opts.Progress)
	if err != nil {
		return Prepared{}, err
	}
	imageInfo, rootfs, err := s.prepareImageRoot(ctx, pulled, opts.Platform, opts.Progress)
	if err != nil {
		return Prepared{}, err
	}
	var cleanupManagedVolumeSnapshots func()
	opts.ManagedVolumes, cleanupManagedVolumeSnapshots, err = s.resolveManagedVolumes(imageInfo.Image, rootfs, opts)
	if err != nil {
		return Prepared{}, err
	}
	keepManagedVolumeSnapshots := false
	defer func() {
		if !keepManagedVolumeSnapshots && cleanupManagedVolumeSnapshots != nil {
			cleanupManagedVolumeSnapshots()
		}
	}()
	id, containerDir, err := s.createContainerDir()
	if err != nil {
		return Prepared{}, err
	}
	activeContainerDir := s.activeContainerDir(id)
	if err := os.MkdirAll(activeContainerDir, 0700); err != nil {
		s.discardIncompleteContainer(id)
		return Prepared{}, fmt.Errorf("creating active container directory: %w", err)
	}
	keep := false
	leaseID := ""
	defer func() {
		if !keep {
			if leaseID != "" {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = images.DeleteContentLease(cleanupCtx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID})
				cancel()
			}
			s.discardIncompleteContainer(id)
		}
	}()
	rootfsUpperID := ""
	if !opts.ReadonlyRootfs {
		manager, err := ttcvolume.OpenManager(s.activeRoot())
		if err != nil {
			return Prepared{}, fmt.Errorf("opening rootfs upper store: %w", err)
		}
		if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
			return Prepared{}, fmt.Errorf("volume backing capability changed while preparing rootfs: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
		}
		origins, err := allocateRepositoryOrigins(&opts, 1)
		if err != nil {
			return Prepared{}, fmt.Errorf("allocating rootfs working snapshot: %w", err)
		}
		var rootfsUpper *ttcvolume.Repository
		if len(origins) == 0 {
			rootfsUpper, err = manager.CreatePrivate(filepath.Join(activeContainerDir, rootfsUpperRepositoryDir), "rootfs")
		} else {
			rootfsUpper, err = manager.CreatePrivateWithOrigin(filepath.Join(activeContainerDir, rootfsUpperRepositoryDir), "rootfs", origins[0])
		}
		if err != nil {
			return Prepared{}, fmt.Errorf("creating persistent rootfs upper: %w", err)
		}
		opts.RootfsUpperRepositoryPath = rootfsUpper.HostPath()
		opts.RootfsUpperCopyMode = string(manager.Capability().Mode)
		rootfsUpperID = rootfsUpper.Info().ID
		if err := rootfsUpper.Close(); err != nil {
			return Prepared{}, fmt.Errorf("closing persistent rootfs upper: %w", err)
		}
	}
	if err := s.resolveWritableHostMounts(activeContainerDir, &opts); err != nil {
		return Prepared{}, err
	}
	// These files are bind-mounted into the live sandbox and remain open for
	// the lifetime of its gofer. Keep them with the node-local active state,
	// not in the shared catalog. Apart from avoiding hot shared-filesystem I/O,
	// this ensures removing shared metadata never unlinks an open NFS file.
	internalMounts, err := materializeNetworkFiles(activeContainerDir, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	opts.HostMounts = append(opts.HostMounts, internalMounts...)
	for _, mount := range opts.DeviceMounts {
		mount.Readonly = true
		mount.Internal = true
		opts.HostMounts = append(opts.HostMounts, mount)
	}
	opts.SpecOptions.RootFS = rootfs
	if hasHardResourceLimits(opts.SpecOptions) {
		opts.SpecOptions.CgroupsPath = filepath.Join("ttc", id)
	}
	spec, err := BuildSpec(imageInfo.Image, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	spec.Process.Env = addDefaultEnv(spec.Process.Env, opts.DeviceEnv)
	spec.Linux.Devices = append(spec.Linux.Devices, opts.DeviceNodes...)
	if s.CPUFeatures != "" {
		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		spec.Annotations[specutils.AnnotationCPUFeatures] = s.CPUFeatures
	}
	logAttributes, err := selectLogAttributes(opts.LogOptions, spec)
	if err != nil {
		return Prepared{}, err
	}
	shmSize, err := effectiveShmSize(opts.ShmSize)
	if err != nil {
		return Prepared{}, err
	}
	leaseID = images.ContainerLeaseID(id)
	metadata := ContainerInfo{
		ID:                  id,
		Name:                opts.Name,
		Reference:           imageInfo.Reference,
		Digest:              pulled.Digest,
		ImageSize:           pulled.Size,
		LeaseID:             leaseID,
		Platform:            imageInfo.Platform,
		BundleDir:           filepath.Join(s.Root, "containers", id, "bundle"),
		RootFS:              rootfs,
		NetworkMode:         networkMode,
		LiveForkEligible:    liveForkEligible,
		LiveForkWaiver:      opts.LiveForkWaiver,
		NetworkNAT:          opts.NetworkNAT,
		NetworkIP:           opts.NetworkIP,
		NetworkMAC:          opts.NetworkMAC,
		NetworkInterface:    opts.NetworkInterface,
		PublishedPorts:      append([]PortBinding(nil), opts.PublishedPorts...),
		GPUOptions:          opts.GPUOptions,
		LogDriver:           logDriver,
		LogOptions:          cloneStringMap(opts.LogOptions),
		LogAttributes:       logAttributes,
		ConsoleLogMaxBytes:  opts.ConsoleLogMaxBytes,
		ConsoleLogMaxFiles:  opts.ConsoleLogMaxFiles,
		Terminal:            opts.Terminal,
		OpenStdin:           opts.OpenStdin,
		AutoRemove:          opts.AutoRemove,
		StopSignal:          AnnotationValue(spec.Annotations, StopSignalAnnotation),
		StopTimeout:         cloneIntPointer(opts.StopTimeout),
		PidsLimit:           cloneInt64Pointer(opts.PidsLimit),
		MemoryLimit:         cloneInt64Pointer(opts.MemoryLimit),
		MemorySwap:          cloneInt64Pointer(opts.MemorySwap),
		NanoCPUs:            cloneInt64Pointer(opts.NanoCPUs),
		CPUShares:           cloneUint64Pointer(opts.CPUShares),
		CPUPeriod:           cloneUint64Pointer(opts.CPUPeriod),
		CPUQuota:            cloneInt64Pointer(opts.CPUQuota),
		ShmSize:             cloneInt64Pointer(&shmSize),
		GroupAdd:            append([]string(nil), opts.GroupAdd...),
		CapAdd:              append([]string(nil), opts.CapAdd...),
		CapDrop:             append([]string(nil), opts.CapDrop...),
		Ulimits:             append([]Ulimit(nil), opts.Ulimits...),
		ExtraHosts:          append([]string(nil), opts.ExtraHosts...),
		DNS:                 append([]string(nil), opts.DNS...),
		DNSSearch:           append([]string(nil), opts.DNSSearch...),
		DNSOptions:          append([]string(nil), opts.DNSOptions...),
		Volumes:             append([]ManagedVolume(nil), opts.ManagedVolumes...),
		RootfsUpperID:       rootfsUpperID,
		RootfsUpperCopyMode: opts.RootfsUpperCopyMode,
		State:               "created",
		CreatedAt:           time.Now().UTC(),
		NodeID:              opts.NodeID,
	}
	metadata.Branches = []BranchInfo{{ID: "main", NodeID: opts.NodeID, FilesystemBranch: ttcvolume.MainBranch, State: "created", StorageState: "local", CreatedAt: metadata.CreatedAt}}
	if metadata.StopTimeout == nil {
		value := DefaultStopTimeoutSeconds
		metadata.StopTimeout = &value
	}
	if err := s.writeNewContainer(ctx, id, containerDir, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: pulled.Digest}, spec, &metadata); err != nil {
		return Prepared{}, err
	}
	// The commands that follow name it: the node that made it looks for it
	// by name without a search.
	setContainerNameHint(s.Root+"\x00"+metadata.Name, id)
	keep = true
	keepManagedVolumeSnapshots = true
	return Prepared{BundleDir: metadata.BundleDir, RootFS: rootfs, Image: imageInfo, Container: metadata}, nil
}

// PrepareRestore imports a standalone checkpoint's writable filesystems and
// creates a new family bundle locked to the checkpointed image digest.
func (s Store) PrepareRestore(ctx context.Context, opts RestoreOptions) (Prepared, error) {
	if opts.Spec == nil {
		return Prepared{}, errors.New("checkpoint has no OCI spec")
	}
	if err := validateName(opts.Name); err != nil {
		return Prepared{}, err
	}
	if err := s.checkContainerNameUnused(opts.Name); err != nil {
		return Prepared{}, err
	}
	pulled, err := s.resolveImage(ctx, opts.Container.Digest, opts.Container.Platform, "never", nil)
	if err != nil {
		return Prepared{}, fmt.Errorf("resolving checkpoint image %s: %w", opts.Container.Digest, err)
	}
	if pulled.Digest != opts.Container.Digest {
		return Prepared{}, fmt.Errorf("checkpoint image resolved to %s, want %s", pulled.Digest, opts.Container.Digest)
	}
	imageInfo, rootfs, err := s.prepareImageRoot(ctx, pulled, opts.Container.Platform, nil)
	if err != nil {
		return Prepared{}, err
	}
	id, containerDir, err := s.createContainerDir()
	if err != nil {
		return Prepared{}, err
	}
	activeContainerDir := s.activeContainerDir(id)
	if err := os.MkdirAll(activeContainerDir, 0700); err != nil {
		s.discardIncompleteContainer(id)
		return Prepared{}, fmt.Errorf("creating active restore directory: %w", err)
	}
	keep := false
	leaseID := ""
	var importedVolumeIDs []string
	defer func() {
		if keep {
			return
		}
		if manager, err := ttcvolume.OpenManager(s.activeRoot()); err == nil {
			for _, volumeID := range importedVolumeIDs {
				_ = manager.Remove(volumeID)
			}
		}
		if leaseID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = images.DeleteContentLease(cleanupCtx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID})
			cancel()
		}
		s.discardIncompleteContainer(id)
	}()

	specData, err := json.Marshal(opts.Spec)
	if err != nil {
		return Prepared{}, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return Prepared{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(&spec)
	if err != nil {
		return Prepared{}, err
	}
	snapshotBySource := make(map[string]string, len(opts.Repositories))
	for _, snapshot := range opts.Repositories {
		if snapshot.Source == "" || snapshot.SnapshotPath == "" {
			return Prepared{}, errors.New("checkpoint contains an invalid repository snapshot")
		}
		if _, ok := snapshotBySource[snapshot.Source]; ok {
			return Prepared{}, fmt.Errorf("checkpoint contains duplicate repository source %q", snapshot.Source)
		}
		snapshotBySource[snapshot.Source] = snapshot.SnapshotPath
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return Prepared{}, err
	}
	if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
		return Prepared{}, fmt.Errorf("volume backing capability changed during restore: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
	}
	copyMode := manager.Capability().Mode
	// A cluster node's repositories have working origins the cluster
	// authority allocates, as those of a family it creates do.
	importOrigin := func() (ttcsnapshot.SnapshotID, error) {
		if opts.AllocateSnapshotOrigins == nil {
			return 0, nil
		}
		origins, err := opts.AllocateSnapshotOrigins(1)
		if err != nil {
			return 0, err
		}
		if len(origins) != 1 || origins[0] == 0 {
			return 0, fmt.Errorf("snapshot allocator returned %v, want one origin", origins)
		}
		return origins[0], nil
	}
	importPrivate := func(path, name, source string) (*ttcvolume.Repository, ttcvolume.SnapshotInfo, error) {
		origin, err := importOrigin()
		if err != nil || origin == 0 {
			if err != nil {
				return nil, ttcvolume.SnapshotInfo{}, err
			}
			return manager.ImportPrivateSnapshot(path, name, source)
		}
		return manager.ImportPrivateSnapshotWithOrigin(path, name, source, origin)
	}
	importVolume := func(name, source string) (*ttcvolume.Repository, ttcvolume.SnapshotInfo, error) {
		origin, err := importOrigin()
		if err != nil || origin == 0 {
			if err != nil {
				return nil, ttcvolume.SnapshotInfo{}, err
			}
			return manager.ImportSnapshot(name, source)
		}
		return manager.ImportSnapshotWithOrigin(name, source, origin)
	}
	var rootfsUpperID string
	volumeIDMap := make(map[string]ttcvolume.VolumeInfo)
	for index := range repositories {
		repository := &repositories[index]
		snapshotPath := snapshotBySource[repository.Source]
		if snapshotPath == "" {
			return Prepared{}, fmt.Errorf("checkpoint omits repository source %q", repository.Source)
		}
		if repository.Source == specutils.TTCRootfsUpperSource {
			path := filepath.Join(activeContainerDir, rootfsUpperRepositoryDir)
			imported, snapshot, err := importPrivate(path, "rootfs", snapshotPath)
			if err != nil {
				return Prepared{}, fmt.Errorf("importing rootfs upper: %w", err)
			}
			info := imported.Info()
			rootfsUpperID = info.ID
			repository.Path = imported.HostPath()
			repository.Branch = ttcvolume.MainBranch
			repository.CopyMode = string(copyMode)
			repository.RestoreID = snapshot.RepositoryID
			if err := imported.Close(); err != nil {
				return Prepared{}, err
			}
			continue
		}
		if repository.Private {
			path := filepath.Join(activeContainerDir, hostMountRepositoryDir, strconv.Itoa(index))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return Prepared{}, fmt.Errorf("creating restored private repository root: %w", err)
			}
			imported, snapshot, err := importPrivate(path, fmt.Sprintf("host-%d", index), snapshotPath)
			if err != nil {
				return Prepared{}, fmt.Errorf("importing private host-mount repository %q: %w", repository.Source, err)
			}
			repository.Path = imported.HostPath()
			repository.Branch = ttcvolume.MainBranch
			repository.CopyMode = string(copyMode)
			repository.RestoreID = snapshot.RepositoryID
			if err := imported.Close(); err != nil {
				return Prepared{}, err
			}
			continue
		}
		name := fmt.Sprintf("restore-%s-%d", id[:12], index)
		imported, snapshot, err := importVolume(name, snapshotPath)
		if err != nil {
			return Prepared{}, fmt.Errorf("importing managed volume %q: %w", repository.Source, err)
		}
		info := imported.Info()
		importedVolumeIDs = append(importedVolumeIDs, info.ID)
		volumeIDMap[filepath.Base(repository.Source)] = info
		repository.Path = imported.HostPath()
		repository.Branch = ttcvolume.MainBranch
		repository.CopyMode = string(copyMode)
		repository.RestoreID = snapshot.RepositoryID
		if err := imported.Close(); err != nil {
			return Prepared{}, err
		}
	}
	if len(snapshotBySource) != len(repositories) {
		return Prepared{}, fmt.Errorf("checkpoint has %d repository snapshots, OCI spec uses %d", len(snapshotBySource), len(repositories))
	}
	encodedRepositories, err := json.Marshal(repositories)
	if err != nil {
		return Prepared{}, err
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	if s.CPUFeatures != "" {
		spec.Annotations[specutils.AnnotationCPUFeatures] = s.CPUFeatures
	}
	delete(spec.Annotations, specutils.AnnotationTTCMemoryFile)
	if len(repositories) == 0 {
		delete(spec.Annotations, specutils.AnnotationTTCVolumeRepositories)
	} else {
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encodedRepositories)
	}
	if err := rewriteImageRoot(&spec, rootfs); err != nil {
		return Prepared{}, fmt.Errorf("rewriting checkpoint image root: %w", err)
	}
	hostMountByIndex := make(map[int]RestoreHostMount, len(opts.HostMounts))
	for _, mount := range opts.HostMounts {
		if mount.MountIndex < 0 || mount.Source == "" || mount.SnapshotPath == "" {
			return Prepared{}, errors.New("checkpoint contains an invalid host mount")
		}
		if _, ok := hostMountByIndex[mount.MountIndex]; ok {
			return Prepared{}, fmt.Errorf("checkpoint contains duplicate host mount index %d", mount.MountIndex)
		}
		hostMountByIndex[mount.MountIndex] = mount
	}
	for index := range spec.Mounts {
		mount := &spec.Mounts[index]
		snapshot, ok := hostMountByIndex[index]
		if !ok {
			continue
		}
		if mount.Type != "bind" || mount.Source != snapshot.Source {
			return Prepared{}, fmt.Errorf("checkpoint host mount %d does not match OCI spec", index)
		}
		destination := filepath.Join(activeContainerDir, "checkpoint-mounts", strconv.Itoa(index))
		if err := copyRestoreHostPath(snapshot.SnapshotPath, destination); err != nil {
			return Prepared{}, fmt.Errorf("restoring host mount %q: %w", mount.Source, err)
		}
		delete(hostMountByIndex, index)
		mount.Source = destination
	}
	if len(hostMountByIndex) != 0 {
		return Prepared{}, fmt.Errorf("checkpoint contains host mounts not present in OCI spec")
	}
	if spec.Linux != nil && spec.Linux.Resources != nil && spec.Linux.CgroupsPath != "" {
		spec.Linux.CgroupsPath = filepath.Join("ttc", id)
	}

	leaseID = images.ContainerLeaseID(id)
	metadataData, err := json.Marshal(opts.Container)
	if err != nil {
		return Prepared{}, err
	}
	var metadata ContainerInfo
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return Prepared{}, err
	}
	now := time.Now().UTC()
	metadata.ID = id
	metadata.Name = opts.Name
	metadata.Reference = opts.Container.Reference
	metadata.Digest = pulled.Digest
	metadata.ImageSize = pulled.Size
	metadata.LeaseID = leaseID
	metadata.Platform = imageInfo.Platform
	metadata.BundleDir = filepath.Join(s.Root, "containers", id, "bundle")
	metadata.RootFS = rootfs
	metadata.PublishedPorts = nil
	metadata.RootfsUpperID = rootfsUpperID
	metadata.RootfsUpperCopyMode = string(copyMode)
	metadata.State = "created"
	metadata.ExitStatus = nil
	metadata.OOMKilled = false
	metadata.ExitError = ""
	metadata.CreatedAt = now
	metadata.StartedAt = nil
	metadata.ExitedAt = nil
	metadata.Branches = []BranchInfo{{ID: "main", NodeID: opts.NodeID, FilesystemBranch: ttcvolume.MainBranch, State: "created", StorageState: "local", CreatedAt: now}}
	metadata.NodeID = opts.NodeID
	for index := range metadata.Volumes {
		oldID := metadata.Volumes[index].ID
		if info, ok := volumeIDMap[oldID]; ok {
			metadata.Volumes[index].ID = info.ID
			metadata.Volumes[index].Name = info.Name
			metadata.Volumes[index].Anonymous = true
		}
	}
	if err := s.writeNewContainer(ctx, id, containerDir, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: pulled.Digest}, &spec, &metadata); err != nil {
		return Prepared{}, fmt.Errorf("restoring container: %w", err)
	}
	setContainerNameHint(s.Root+"\x00"+metadata.Name, id)
	keep = true
	return Prepared{BundleDir: metadata.BundleDir, RootFS: rootfs, Image: imageInfo, Container: metadata}, nil
}

// PrepareDistributedFork creates target container metadata without copying
// image or writable filesystem contents. Repository branch pointers may not
// exist yet; the source capture transaction publishes them before the target
// Sentry is restored.
func (s Store) PrepareDistributedFork(ctx context.Context, opts DistributedForkOptions) (Prepared, error) {
	if err := validateName(opts.SourceContainer); err != nil {
		return Prepared{}, fmt.Errorf("source container: %w", err)
	}
	if err := validateName(opts.TargetName); err != nil {
		return Prepared{}, fmt.Errorf("target container: %w", err)
	}
	if err := validateObjectName("target branch", opts.TargetBranch); err != nil {
		return Prepared{}, err
	}
	if opts.VisibleBranch == "" {
		opts.VisibleBranch = "main"
	}
	if err := validateObjectName("visible target branch", opts.VisibleBranch); err != nil {
		return Prepared{}, err
	}
	if err := validateObjectName("distributed fork transaction", opts.TransactionID); err != nil {
		return Prepared{}, err
	}
	if opts.NodeID == "" {
		return Prepared{}, errors.New("distributed fork target node ID is empty")
	}
	if opts.Migration && opts.SourceNode == "" {
		return Prepared{}, errors.New("distributed migration source node ID is empty")
	}
	var source ContainerInfo
	if opts.Source != nil {
		source = *opts.Source
		portableFamily := source.DistributedFamilyID
		if portableFamily == "" {
			portableFamily = source.ID
		}
		if source.ID != opts.SourceContainer && portableFamily != opts.SourceContainer {
			return Prepared{}, fmt.Errorf("portable source ID %q does not match transaction source %q", source.ID, opts.SourceContainer)
		}
	} else {
		var err error
		source, err = s.LoadContainer(opts.SourceContainer)
		if err != nil {
			return Prepared{}, err
		}
	}
	sourceQuarantined := false
	targetParent := opts.ParentBranch
	targetCreated := time.Now().UTC()
	var migrationSource *BranchInfo
	sourcePhysicalBranch := opts.SourcePhysicalBranch
	if sourcePhysicalBranch == "" {
		sourcePhysicalBranch = opts.VisibleBranch
	}
	for _, branch := range source.Branches {
		if opts.Migration && branch.ID == sourcePhysicalBranch {
			copy := branch
			migrationSource = &copy
			targetParent = branch.ParentID
			targetCreated = branch.CreatedAt
			sourceQuarantined = branch.Quarantined
			break
		}
		if branch.ID == opts.ParentBranch {
			sourceQuarantined = branch.Quarantined
			if opts.Migration {
				targetParent = branch.ParentID
			}
			break
		}
	}
	globalFamilyID := source.DistributedFamilyID
	if globalFamilyID == "" {
		globalFamilyID = source.ID
	}
	if existing, err := s.LoadDistributedForkTarget(globalFamilyID, opts.TransactionID); err == nil {
		if existing.Name != opts.TargetName || existing.DistributedForkNodeID != opts.NodeID {
			return Prepared{}, fmt.Errorf("distributed fork transaction %q is bound to another target", opts.TransactionID)
		}
		return Prepared{BundleDir: existing.BundleDir, RootFS: existing.RootFS, Container: existing}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Prepared{}, err
	}
	containers, err := s.containersNamed(opts.TargetName)
	if err != nil {
		return Prepared{}, err
	}
	var existingTarget *ContainerInfo
	for i := range containers {
		if containers[i].Name == opts.TargetName || containers[i].ID == opts.TargetName {
			if existingTarget != nil {
				return Prepared{}, fmt.Errorf("container name %q is ambiguous", opts.TargetName)
			}
			existingTarget = &containers[i]
		}
	}
	if existingTarget != nil {
		// A target family can simultaneously finish an earlier branch import
		// while this one is being reserved. Keep the complete read/validate/write
		// transaction under the same per-container lock as dependency and
		// publication updates; otherwise either writer can silently erase the
		// other's DistributedForkImports entry on a delayed shared filesystem.
		var existing ContainerInfo
		err := s.mutateContainer(existingTarget.ID, func(current *ContainerInfo) (bool, error) {
			// A logical family spans nodes. An import into an existing family is a
			// peer branch even when the target node does not yet have a physical
			// Sentry for that family; the target runtime layer creates one lazily.
			existingFamilyID := current.DistributedFamilyID
			if existingFamilyID == "" {
				existingFamilyID = current.ID
			}
			if existingFamilyID != globalFamilyID {
				return false, fmt.Errorf("container %q belongs to distributed family %q, not %q", opts.TargetName, existingFamilyID, globalFamilyID)
			}
			branchExists := false
			for _, branch := range current.Branches {
				if branch.ID != opts.VisibleBranch {
					continue
				}
				branchExists = true
				if !opts.Migration {
					return false, fmt.Errorf("branch %s@%s already exists", current.Name, opts.VisibleBranch)
				}
				if !opts.AuthoritativeMigration && current.BranchNodeID(branch.ID) != opts.SourceNode {
					return false, fmt.Errorf("migration source branch %s@%s belongs to node %q, not %q", current.Name, branch.ID, current.BranchNodeID(branch.ID), opts.SourceNode)
				}
			}
			if opts.Migration && !opts.AuthoritativeMigration && !branchExists && (migrationSource == nil || source.BranchNodeID(sourcePhysicalBranch) != opts.SourceNode) {
				return false, fmt.Errorf("migration source branch %s@%s is absent from the source-owned family metadata", current.Name, opts.VisibleBranch)
			}
			if current.DistributedForkImports == nil {
				current.DistributedForkImports = make(map[string]DistributedForkImport)
			}
			if _, ok := current.DistributedForkImports[opts.TransactionID]; ok {
				existing = *current
				return false, nil
			}
			for _, pending := range current.DistributedForkImports {
				if pending.BranchID == opts.VisibleBranch {
					return false, fmt.Errorf("branch %s@%s is already being imported", current.Name, opts.VisibleBranch)
				}
			}
			physicalBranch := ""
			if opts.Migration {
				physicalBranch = opts.TargetBranch
			}
			current.DistributedForkImports[opts.TransactionID] = DistributedForkImport{
				BranchID: opts.VisibleBranch, PhysicalBranchID: physicalBranch, ParentID: targetParent, FilesystemBranch: ttcvolume.ScopedBranchID(source.ID, opts.TargetBranch), Quarantined: sourceQuarantined,
				State: "created", CreatedAt: targetCreated, SourceNode: opts.SourceNode, TargetNode: opts.NodeID, Migration: opts.Migration,
				AuthoritativeMigration: opts.AuthoritativeMigration,
			}
			existing = *current
			return true, nil
		})
		if err != nil {
			return Prepared{}, err
		}
		return Prepared{BundleDir: existing.BundleDir, RootFS: existing.RootFS, Container: existing}, nil
	}
	var spec *specs.Spec
	if opts.Spec != nil {
		encoded, err := json.Marshal(opts.Spec)
		if err != nil {
			return Prepared{}, err
		}
		spec = &specs.Spec{}
		if err := json.Unmarshal(encoded, spec); err != nil {
			return Prepared{}, err
		}
	} else {
		var err error
		spec, err = s.LoadSpec(source.ID)
		if err != nil {
			return Prepared{}, err
		}
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return Prepared{}, err
	}
	childRepositoryBranch := ttcvolume.ScopedBranchID(source.ID, opts.TargetBranch)
	for i := range repositories {
		repositories[i].Branch = childRepositoryBranch
		// Private denotes ownership by the container family, not by this target
		// instance. Source-family repositories are promoted outside the source
		// container directory before reservation, so removing either instance
		// cannot invalidate the other one.
	}
	encodedRepositories, err := json.Marshal(repositories)
	if err != nil {
		return Prepared{}, err
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	if len(repositories) == 0 {
		delete(spec.Annotations, specutils.AnnotationTTCVolumeRepositories)
	} else {
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encodedRepositories)
	}
	var id, containerDir string
	if opts.TargetID != "" {
		if err := validateObjectName("distributed target container", opts.TargetID); err != nil {
			return Prepared{}, err
		}
		id = opts.TargetID
		containerDir, err = s.createContainerStagingDir(id)
	} else {
		id, containerDir, err = s.createContainerDir()
	}
	if err != nil {
		return Prepared{}, err
	}
	activeContainerDir := s.activeContainerDir(id)
	if err := os.MkdirAll(activeContainerDir, 0700); err != nil {
		s.discardIncompleteContainer(id)
		return Prepared{}, fmt.Errorf("creating distributed target active directory: %w", err)
	}
	// TTC-generated bind mounts still name the source node here. Their small
	// contents arrive with the authenticated remote session and are rewritten
	// in the transient runtime bundle immediately before runsc create. Trying
	// to read these paths during reservation breaks when nodes use private /tmp
	// mounts, even if their TTC_LOCAL_ROOT strings happen to be identical.
	delete(spec.Annotations, specutils.AnnotationTTCMemoryFile)
	spec.Annotations[specutils.AnnotationTTCDistributedTarget] = "true"
	keep := false
	leaseID := ""
	defer func() {
		if keep {
			return
		}
		if leaseID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = images.DeleteContentLease(cleanupCtx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID})
			cancel()
		}
		s.discardIncompleteContainer(id)
	}()
	if spec.Linux != nil && spec.Linux.Resources != nil && spec.Linux.CgroupsPath != "" {
		spec.Linux.CgroupsPath = filepath.Join("ttc", id)
	}
	leaseID = images.ContainerLeaseID(id)
	encodedSource, err := json.Marshal(source)
	if err != nil {
		return Prepared{}, err
	}
	var target ContainerInfo
	if err := json.Unmarshal(encodedSource, &target); err != nil {
		return Prepared{}, err
	}
	now := time.Now().UTC()
	target.ID = id
	// A remote runtime is a physical implementation detail of the logical
	// distributed family. Give its persistent record an internal unique name;
	// exposing the source family name here makes ordinary name lookup ambiguous
	// after migration. The authority's placement maps it back to the logical family.
	target.Name = id
	target.LeaseID = leaseID
	target.BundleDir = filepath.Join(s.Root, "containers", id, "bundle")
	target.PublishedPorts = nil
	target.AutoRemove = false
	target.State = "created"
	target.ExitStatus = nil
	target.OOMKilled = false
	target.ExitError = ""
	target.CreatedAt = now
	target.StartedAt = nil
	target.ExitedAt = nil
	physicalBranch := opts.VisibleBranch
	if opts.Migration {
		physicalBranch = opts.TargetBranch
	}
	target.Branches = []BranchInfo{{
		ID: physicalBranch, NodeID: opts.NodeID, FilesystemBranch: childRepositoryBranch,
		Quarantined: sourceQuarantined, State: "created", StorageState: "local", SourceNode: opts.SourceNode,
		ParentID: targetParent, CreatedAt: now, DistributedForkTransactionID: opts.TransactionID,
	}}
	target.DistributedForkTransactionID = opts.TransactionID
	target.DistributedForkNodeID = opts.NodeID
	target.DistributedForkHidden = true
	target.DistributedPhysicalRuntime = true
	target.DistributedFamilyID = globalFamilyID
	target.DistributedForkImports = nil
	target.NodeID = opts.NodeID
	for i := range target.Volumes {
		target.Volumes[i].Anonymous = false
	}
	if err := s.writeNewContainer(ctx, id, containerDir, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: source.Digest}, spec, &target); err != nil {
		return Prepared{}, fmt.Errorf("making distributed target: %w", err)
	}
	keep = true
	return Prepared{BundleDir: target.BundleDir, RootFS: target.RootFS, Container: target}, nil
}

func copyRestoreHostPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return copyRestoreHostFile(source, destination, info.Mode().Perm())
	}
	if !info.IsDir() {
		return fmt.Errorf("checkpoint host mount snapshot has unsupported mode %v", info.Mode())
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	var directories []struct {
		path string
		mode os.FileMode
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{target, entryInfo.Mode().Perm()})
			return nil
		case entryInfo.Mode().IsRegular():
			return copyRestoreHostFile(path, target, entryInfo.Mode().Perm())
		case entryInfo.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("checkpoint host mount entry %q has unsupported mode %v", path, entryInfo.Mode())
		}
	})
	for index := len(directories) - 1; index >= 0; index-- {
		if chmodErr := os.Chmod(directories[index].path, directories[index].mode); err == nil && chmodErr != nil {
			err = chmodErr
		}
	}
	if chmodErr := os.Chmod(destination, info.Mode().Perm()); err == nil && chmodErr != nil {
		err = chmodErr
	}
	return err
}

func copyRestoreHostFile(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func (s Store) resolveManagedVolumes(image ocispec.Image, rootfs string, opts PrepareOptions) ([]ManagedVolume, func(), error) {
	volumes := append([]ManagedVolume(nil), opts.ManagedVolumes...)
	// The containers that may attach a volume, listed once a volume needs them.
	var containers []ContainerInfo
	listed := false
	listContainers := func() error {
		if listed {
			return nil
		}
		var err error
		if containers, err = s.listContainers(true); err != nil {
			return err
		}
		listed = true
		return nil
	}
	mounted := make(map[string]bool)
	for _, mount := range opts.HostMounts {
		mounted[filepath.Clean(mount.Destination)] = true
	}
	for _, mount := range opts.FilesystemMounts {
		mounted[filepath.Clean(mount.Destination)] = true
	}
	for _, volume := range volumes {
		destination := filepath.Clean(volume.Destination)
		if mounted[destination] {
			return nil, nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		mounted[destination] = true
	}
	imageDestinations := make([]string, 0, len(image.Config.Volumes))
	for destination := range image.Config.Volumes {
		imageDestinations = append(imageDestinations, destination)
	}
	sort.Strings(imageDestinations)
	for _, destination := range imageDestinations {
		clean := filepath.Clean(destination)
		if !filepath.IsAbs(clean) || clean == "/" {
			return nil, nil, fmt.Errorf("image declares invalid volume destination %q", destination)
		}
		if mounted[clean] {
			continue
		}
		volumes = append(volumes, ManagedVolume{Destination: clean, Anonymous: true})
		mounted[clean] = true
	}
	if len(volumes) == 0 {
		return nil, nil, nil
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return nil, nil, err
	}
	if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
		return nil, nil, fmt.Errorf("volume backing capability changed while preparing managed volumes: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
	}
	ownerManifest, err := ownership.Read(ownership.ManifestPath(rootfs))
	if err != nil {
		return nil, nil, fmt.Errorf("loading image ownership for managed volumes: %w", err)
	}
	type createdSnapshot struct {
		volume string
		branch string
	}
	var snapshots []createdSnapshot
	cleanup := func() {
		for i := len(snapshots) - 1; i >= 0; i-- {
			repository, openErr := manager.Open(snapshots[i].volume)
			if openErr != nil {
				continue
			}
			_ = repository.RemoveBranch(snapshots[i].branch)
			_ = repository.Close()
		}
	}
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	seenIDs := make(map[string]string)
	for i := range volumes {
		if volumes[i].Name == "" {
			id, err := randomObjectID("anonymous volume", 16)
			if err != nil {
				return nil, nil, err
			}
			volumes[i].Name = "anon-" + id
			volumes[i].Anonymous = true
		}
		repository, err := manager.Open(volumes[i].Name)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			origins, allocationErr := allocateRepositoryOrigins(&opts, 1)
			if allocationErr != nil {
				return nil, nil, fmt.Errorf("allocating managed-volume working snapshot: %w", allocationErr)
			}
			if len(origins) == 0 {
				repository, err = manager.Create(volumes[i].Name)
			} else {
				repository, err = manager.CreateWithOrigin(volumes[i].Name, origins[0])
			}
			created = err == nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("resolving managed volume %q: %w", volumes[i].Name, err)
		}
		if created && !volumes[i].NoCopy {
			if err := seedManagedVolume(repository, rootfs, volumes[i].Destination, ownerManifest); err != nil {
				repository.Close()
				_ = manager.Remove(volumes[i].Name)
				return nil, nil, fmt.Errorf("initializing managed volume %q from image path %q: %w", volumes[i].Name, volumes[i].Destination, err)
			}
		}
		info := repository.Info()
		if err := listContainers(); err != nil {
			repository.Close()
			return nil, nil, err
		}
		if err := validateManagedVolumeAttachment(info.ID, volumes[i], containers); err != nil {
			repository.Close()
			return nil, nil, err
		}
		volumes[i].ID = info.ID
		volumes[i].RepositoryPath = repository.HostPath()
		volumes[i].RepositoryBranch = ttcvolume.MainBranch
		volumes[i].CopyMode = string(manager.Capability().Mode)
		if err := repository.Close(); err != nil {
			return nil, nil, err
		}
		if volumes[i].Readonly {
			nonce, err := randomObjectID("managed volume attachment", 16)
			if err != nil {
				return nil, nil, err
			}
			child := "a-" + nonce
			owner, ownerBranch := managedVolumeWritableOwner(info.ID, containers)
			origins, allocationErr := allocateRepositoryOrigins(&opts, 2)
			if allocationErr != nil {
				return nil, nil, fmt.Errorf("allocating readonly volume snapshot origins: %w", allocationErr)
			}
			if owner != nil && (ownerBranch.State == "running" || ownerBranch.State == "paused") {
				if opts.SnapshotManagedVolume == nil {
					return nil, nil, fmt.Errorf("managed volume %q requires a live snapshot from container %q", volumes[i].Name, owner.Name)
				}
				var sourceWorking, childWorking ttcsnapshot.SnapshotID
				if len(origins) != 0 {
					sourceWorking, childWorking = origins[0], origins[1]
				}
				if err := opts.SnapshotManagedVolume(owner.ID, ownerBranch.ID, info.ID, child, sourceWorking, childWorking); err != nil {
					return nil, nil, fmt.Errorf("snapshotting managed volume %q from %s@%s: %w", volumes[i].Name, owner.Name, ownerBranch.ID, err)
				}
			} else {
				repository, err := manager.Open(info.ID)
				if err != nil {
					return nil, nil, err
				}
				var tx *ttcvolume.ForkTransaction
				var forkErr error
				if len(origins) == 0 {
					tx, forkErr = repository.BeginFork(ttcvolume.MainBranch, child)
				} else {
					tx, forkErr = repository.BeginForkWithOrigins(ttcvolume.MainBranch, child, origins[0], origins[1])
				}
				if forkErr == nil {
					forkErr = tx.Commit()
				}
				closeErr := repository.Close()
				if forkErr != nil || closeErr != nil {
					return nil, nil, errors.Join(forkErr, closeErr)
				}
			}
			volumes[i].RepositoryBranch = child
			snapshots = append(snapshots, createdSnapshot{volume: info.ID, branch: child})
		}
		if previous, ok := seenIDs[info.ID]; ok {
			return nil, nil, fmt.Errorf("managed volume %q is mounted more than once (%q and %q); aliases are not yet supported", volumes[i].Name, previous, volumes[i].Destination)
		}
		seenIDs[info.ID] = volumes[i].Destination
	}
	success = true
	return volumes, cleanup, nil
}

func managedVolumeWritableOwner(volumeID string, containers []ContainerInfo) (*ContainerInfo, BranchInfo) {
	for i := range containers {
		writable := false
		for _, volume := range containers[i].Volumes {
			if volume.ID == volumeID && !volume.Readonly {
				writable = true
				break
			}
		}
		if !writable {
			continue
		}
		for _, branch := range containers[i].Branches {
			if branch.ID == "main" {
				return &containers[i], branch
			}
		}
		return &containers[i], BranchInfo{}
	}
	return nil, BranchInfo{}
}

func (s Store) resolveWritableHostMounts(containerDir string, opts *PrepareOptions) error {
	if opts == nil {
		return errors.New("missing host mount options")
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return err
	}
	if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
		return fmt.Errorf("volume backing capability changed while preparing host mounts: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
	}
	hostMountRootCreated := false
	for index := range opts.HostMounts {
		mount := &opts.HostMounts[index]
		if mount.Readonly || mount.Internal {
			continue
		}
		if !mount.ManagedCoW && !mount.Copy {
			return fmt.Errorf("writable host mount %q at %q has no TTC isolation mode", mount.Source, mount.Destination)
		}
		resolved, err := filepath.EvalSymlinks(mount.Source)
		if err != nil {
			return fmt.Errorf("resolving host mount source %q: %w", mount.Source, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("stating host mount source %q: %w", mount.Source, err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("host mount source %q has unsupported type %s", mount.Source, info.Mode().Type())
		}
		if !hostMountRootCreated {
			if err := os.Mkdir(filepath.Join(containerDir, hostMountRepositoryDir), 0700); err != nil && !os.IsExist(err) {
				return fmt.Errorf("creating private host-mount repository root: %w", err)
			}
			hostMountRootCreated = true
		}
		repositoryPath := filepath.Join(containerDir, hostMountRepositoryDir, strconv.Itoa(index))
		origins, err := allocateRepositoryOrigins(opts, 1)
		if err != nil {
			return fmt.Errorf("allocating host-mount working snapshot: %w", err)
		}
		var repository *ttcvolume.Repository
		if len(origins) == 0 {
			repository, err = manager.CreatePrivate(repositoryPath, fmt.Sprintf("host-%d", index))
		} else {
			repository, err = manager.CreatePrivateWithOrigin(repositoryPath, fmt.Sprintf("host-%d", index), origins[0])
		}
		if err != nil {
			return fmt.Errorf("creating persistent upper for host mount %q: %w", mount.Destination, err)
		}
		uid, gid := hostFileOwner(info)
		if info.Mode().IsRegular() {
			err = repository.InitializeRootRegular(uint32(info.Mode().Perm()), uid, gid)
		}
		if err == nil && mount.Copy {
			err = seedCopiedHostMount(repository, resolved, info)
		}
		mount.RepositoryID = repository.Info().ID
		mount.RepositoryPath = repository.HostPath()
		mount.CopyMode = string(manager.Capability().Mode)
		closeErr := repository.Close()
		if err != nil {
			return fmt.Errorf("preparing host mount %q at %q: %w", mount.Source, mount.Destination, err)
		}
		if closeErr != nil {
			return fmt.Errorf("closing host-mount repository %q: %w", mount.Destination, closeErr)
		}
	}
	return nil
}

func hostFileOwner(info os.FileInfo) (uint32, uint32) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, stat.Gid
	}
	return 0, 0
}

func seedCopiedHostMount(repository *ttcvolume.Repository, sourceRoot string, rootInfo os.FileInfo) error {
	if rootInfo.Mode().IsRegular() {
		return copyHostRegularIntoRepository(repository, ttcvolume.RootInode, sourceRoot, rootInfo)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("copy source has unsupported type %s", rootInfo.Mode().Type())
	}
	root, err := repository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		return err
	}
	root.UID, root.GID = hostFileOwner(rootInfo)
	root.Mode = uint32(rootInfo.Mode().Perm())
	root.AtimeNsec = rootInfo.ModTime().UnixNano()
	root.MtimeNsec = rootInfo.ModTime().UnixNano()
	root.CtimeNsec = rootInfo.ModTime().UnixNano()
	root.Xattrs, err = readHostXattrs(sourceRoot)
	if err != nil {
		return err
	}
	if err := repository.UpdateMetadata(ttcvolume.MainBranch, root); err != nil {
		return err
	}

	directories := map[string]uint64{".": ttcvolume.RootInode}
	hardlinks := make(map[hostFileIdentity]uint64)
	err = filepath.Walk(sourceRoot, func(hostPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, hostPath)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		parent := directories[filepath.Dir(relative)]
		if parent == 0 {
			return fmt.Errorf("missing imported parent for %q", relative)
		}
		name := filepath.Base(relative)
		uid, gid := hostFileOwner(info)
		mode := uint32(info.Mode().Perm())
		var ino uint64
		switch {
		case info.IsDir():
			ino, err = repository.CreateDirectory(ttcvolume.MainBranch, parent, name, mode, uid, gid)
			if err == nil {
				directories[relative] = ino
			}
		case info.Mode().IsRegular():
			var identity hostFileIdentity
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				identity = hostFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}
				if existing := hardlinks[identity]; existing != 0 {
					return repository.Link(ttcvolume.MainBranch, parent, name, existing)
				}
			}
			ino, err = repository.CreateRegular(ttcvolume.MainBranch, parent, name, mode, uid, gid)
			if err == nil {
				err = copyHostRegularIntoRepository(repository, ino, hostPath, info)
			}
			if err == nil && identity != (hostFileIdentity{}) {
				hardlinks[identity] = ino
			}
		case info.Mode()&os.ModeSymlink != 0:
			var target string
			target, err = os.Readlink(hostPath)
			if err == nil {
				ino, err = repository.CreateSymlink(ttcvolume.MainBranch, parent, name, target, uid, gid)
			}
		default:
			return fmt.Errorf("copy source path %q has unsupported type %s", hostPath, info.Mode().Type())
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return nil
		}
		inode, err := repository.Inode(ttcvolume.MainBranch, ino)
		if err != nil {
			return err
		}
		inode.AtimeNsec = info.ModTime().UnixNano()
		inode.MtimeNsec = info.ModTime().UnixNano()
		inode.CtimeNsec = info.ModTime().UnixNano()
		inode.Xattrs, err = readHostXattrs(hostPath)
		if err != nil {
			return err
		}
		return repository.UpdateMetadata(ttcvolume.MainBranch, inode)
	})
	if err != nil {
		return err
	}
	return repository.SyncMetadata()
}

func copyHostRegularIntoRepository(repository *ttcvolume.Repository, ino uint64, sourcePath string, info os.FileInfo) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, inode, _, err := repository.OpenDataFile(ttcvolume.MainBranch, ino, true)
	if err != nil {
		return err
	}
	destination, cloneErr := cloneOpenFile(destination, source)
	if destination == nil {
		return cloneErr
	}
	if cloneErr != nil {
		if err := destination.Truncate(0); err != nil {
			destination.Close()
			return err
		}
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			destination.Close()
			return err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			destination.Close()
			return err
		}
		if _, err := io.CopyBuffer(destination, source, make([]byte, 256*1024)); err != nil {
			destination.Close()
			return err
		}
	}
	if err := destination.Close(); err != nil {
		return err
	}
	inode.Size = info.Size()
	inode.UID, inode.GID = hostFileOwner(info)
	inode.Mode = uint32(info.Mode().Perm())
	inode.AtimeNsec = info.ModTime().UnixNano()
	inode.MtimeNsec = info.ModTime().UnixNano()
	inode.CtimeNsec = info.ModTime().UnixNano()
	inode.Xattrs, err = readHostXattrs(sourcePath)
	if err != nil {
		return err
	}
	return repository.UpdateAfterWrite(ttcvolume.MainBranch, inode)
}

func readHostXattrs(name string) (map[string]string, error) {
	return readImageXattrs(name, nil, "")
}

func validateManagedVolumeAttachment(volumeID string, requested ManagedVolume, containers []ContainerInfo) error {
	if requested.Readonly {
		return nil
	}
	for _, container := range containers {
		for _, attached := range container.Volumes {
			if attached.ID == volumeID && !attached.Readonly {
				return fmt.Errorf("managed volume %q already has a writable attachment to container %q; unrelated writable sandbox families are not coherent", requested.Name, container.Name)
			}
		}
	}
	return nil
}

type hostFileIdentity struct {
	dev uint64
	ino uint64
}

// seedManagedVolume implements Docker's copy-on-create volume behavior. The
// repository is known to be new and empty. Data goes directly from the
// immutable image root into persistent opaque volume objects; tmpfs and tar
// staging are deliberately not involved.
func seedManagedVolume(repository *ttcvolume.Repository, rootfs, destination string, manifest *ownership.Manifest) error {
	relativeDestination := strings.TrimPrefix(filepath.Clean(destination), string(filepath.Separator))
	sourceRoot := filepath.Join(rootfs, relativeDestination)
	rootInfo, err := os.Lstat(sourceRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("image path is %s, but managed volumes require a directory", rootInfo.Mode().Type())
	}

	root, err := repository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		return err
	}
	root.Mode = uint32(rootInfo.Mode().Perm())
	if owner, ok := manifest.Lookup(filepath.ToSlash(relativeDestination)); ok {
		root.UID, root.GID = owner.UID, owner.GID
	}
	root.AtimeNsec = rootInfo.ModTime().UnixNano()
	root.MtimeNsec = rootInfo.ModTime().UnixNano()
	root.CtimeNsec = rootInfo.ModTime().UnixNano()
	root.Xattrs, err = readImageXattrs(sourceRoot, manifest, filepath.ToSlash(relativeDestination))
	if err != nil {
		return err
	}
	if err := repository.UpdateMetadata(ttcvolume.MainBranch, root); err != nil {
		return err
	}

	directories := map[string]uint64{".": ttcvolume.RootInode}
	hardlinks := make(map[hostFileIdentity]uint64)
	copyBuffer := make([]byte, 256*1024)
	err = filepath.Walk(sourceRoot, func(hostPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, hostPath)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		parent := directories[filepath.Dir(relative)]
		if parent == 0 {
			return fmt.Errorf("missing imported parent for %q", relative)
		}
		name := filepath.Base(relative)
		imageRelative := filepath.Join(relativeDestination, relative)
		owner, _ := manifest.Lookup(filepath.ToSlash(imageRelative))
		mode := uint32(info.Mode().Perm())

		var ino uint64
		switch {
		case info.IsDir():
			ino, err = repository.CreateDirectory(ttcvolume.MainBranch, parent, name, mode, owner.UID, owner.GID)
			if err == nil {
				directories[relative] = ino
			}
		case info.Mode().IsRegular():
			var identity hostFileIdentity
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				identity = hostFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}
				if existing := hardlinks[identity]; existing != 0 {
					return repository.Link(ttcvolume.MainBranch, parent, name, existing)
				}
			}
			ino, err = repository.CreateRegular(ttcvolume.MainBranch, parent, name, mode, owner.UID, owner.GID)
			if err != nil {
				break
			}
			source, openErr := os.Open(hostPath)
			if openErr != nil {
				return openErr
			}
			destination, _, _, openErr := repository.OpenDataFile(ttcvolume.MainBranch, ino, true)
			if openErr != nil {
				source.Close()
				return openErr
			}
			_, copyErr := io.CopyBuffer(destination, source, copyBuffer)
			sourceCloseErr := source.Close()
			destinationCloseErr := destination.Close()
			if copyErr != nil {
				return copyErr
			}
			if sourceCloseErr != nil {
				return sourceCloseErr
			}
			if destinationCloseErr != nil {
				return destinationCloseErr
			}
			if identity != (hostFileIdentity{}) {
				hardlinks[identity] = ino
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(hostPath)
			if readErr != nil {
				return readErr
			}
			ino, err = repository.CreateSymlink(ttcvolume.MainBranch, parent, name, target, owner.UID, owner.GID)
		default:
			return fmt.Errorf("image path %q has unsupported type %s", imageRelative, info.Mode().Type())
		}
		if err != nil {
			return err
		}
		inode, err := repository.Inode(ttcvolume.MainBranch, ino)
		if err != nil {
			return err
		}
		inode.Size = info.Size()
		inode.AtimeNsec = info.ModTime().UnixNano()
		inode.MtimeNsec = info.ModTime().UnixNano()
		inode.CtimeNsec = info.ModTime().UnixNano()
		inode.Xattrs, err = readImageXattrs(hostPath, manifest, filepath.ToSlash(imageRelative))
		if err != nil {
			return err
		}
		return repository.UpdateAfterWrite(ttcvolume.MainBranch, inode)
	})
	if err != nil {
		return err
	}
	return repository.SyncMetadata()
}

func readImageXattrs(name string, manifest *ownership.Manifest, virtualPath string) (map[string]string, error) {
	size, err := unix.Llistxattr(name, nil)
	if err != nil {
		if !errors.Is(err, unix.ENOTSUP) {
			return nil, fmt.Errorf("listing xattrs for %q: %w", name, err)
		}
		size = 0
	}
	result := make(map[string]string)
	if size != 0 {
		names := make([]byte, size)
		size, err = unix.Llistxattr(name, names)
		if err != nil {
			return nil, fmt.Errorf("listing xattrs for %q: %w", name, err)
		}
		for _, attribute := range strings.Split(string(names[:size]), "\x00") {
			if attribute == "" {
				continue
			}
			valueSize, err := unix.Lgetxattr(name, attribute, nil)
			if err != nil {
				return nil, fmt.Errorf("reading xattr %q for %q: %w", attribute, name, err)
			}
			value := make([]byte, valueSize)
			valueSize, err = unix.Lgetxattr(name, attribute, value)
			if err != nil {
				return nil, fmt.Errorf("reading xattr %q for %q: %w", attribute, name, err)
			}
			result[attribute] = string(value[:valueSize])
		}
	}
	if manifest != nil {
		for _, attribute := range manifest.ListXattrs(virtualPath) {
			value, ok := manifest.LookupXattr(virtualPath, attribute)
			if ok {
				result[attribute] = string(value)
			}
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func randomObjectID(kind string, size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generating %s ID: %w", kind, err)
	}
	return hex.EncodeToString(data), nil
}

func (s Store) createContainerDir() (string, string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		id, err := randomObjectID("container", containerIDBytes)
		if err != nil {
			return "", "", err
		}
		var dir string
		if s.hasUnifiedActiveRoot() {
			dir, err = s.createContainerInPlaceDir(id)
		} else {
			dir, err = s.createContainerStagingDir(id)
		}
		if err == nil {
			return id, dir, nil
		} else if !os.IsExist(err) {
			return "", "", fmt.Errorf("creating state for container %s: %w", id, err)
		}
	}
	return "", "", errors.New("could not allocate a unique container ID")
}

func (s Store) createContainerInPlaceDir(id string) (string, error) {
	root := filepath.Join(s.Root, "containers")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("creating container state root: %w", err)
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, containerCreationMarker), []byte("in progress\n"), 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("marking in-place container construction: %w", err)
	}
	return dir, nil
}

func (s Store) createContainerStagingDir(id string) (string, error) {
	if err := os.MkdirAll(filepath.Join(s.Root, "containers"), 0700); err != nil {
		return "", fmt.Errorf("creating container state root: %w", err)
	}
	root := filepath.Join(s.Root, containerStagingDir)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("creating container staging root: %w", err)
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// writeNewContainer makes a new container's image lease, its OCI config and
// its record durable at once, then publishes the container. The record,
// which names the other two, is given its name only once they are durable.
// Written one after another, each with its own syncs, they were 17 ms of a
// run on a disk whose sync takes 2.5 ms.
func (s Store) writeNewContainer(ctx context.Context, id, containerDir string, lease images.ContentLeaseOptions, spec any, record *ContainerInfo) error {
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		return fmt.Errorf("creating OCI bundle: %w", err)
	}
	var leaseErr, configErr error
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		leaseErr = images.CreateContentLease(ctx, lease)
	}()
	go func() {
		defer group.Done()
		if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
			configErr = fmt.Errorf("writing OCI config: %w", err)
		}
	}()
	staged, err := stageJSON(filepath.Join(containerDir, "container.json"), record)
	group.Wait()
	defer staged.discard()
	if err != nil {
		err = fmt.Errorf("writing container metadata: %w", err)
	}
	if err := errors.Join(leaseErr, configErr, err); err != nil {
		return err
	}
	if err := s.publishContainerRecord(id, containerDir, staged); err != nil {
		return fmt.Errorf("publishing container metadata: %w", err)
	}
	return nil
}

// publishContainerRecord gives a new container's staged record its name and
// publishes the container, as publishContainerDir does. A container made in
// place has both made durable at once.
func (s Store) publishContainerRecord(id, stagingDir string, record *stagedFile) error {
	finalDir := filepath.Join(s.Root, "containers", id)
	if filepath.Clean(stagingDir) != filepath.Clean(finalDir) {
		if err := record.publish(); err != nil {
			return err
		}
		return s.publishContainerDir(id, stagingDir)
	}
	if err := record.rename(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(finalDir, containerCreationMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The container's directory names its record and no longer the marker;
	// its parent names it.
	var parentErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		parentErr = syncDirectory(filepath.Dir(finalDir))
	}()
	err := syncDirectory(finalDir)
	<-done
	return errors.Join(err, parentErr)
}

// publishContainerDir is the only boundary at which a newly constructed
// container becomes visible. Renaming the complete directory is atomic to all
// clients of a conforming shared filesystem; unlike a marker unlink, it does
// not require cross-client cache ordering between independent pathnames.
func (s Store) publishContainerDir(id, stagingDir string) error {
	finalDir := filepath.Join(s.Root, "containers", id)
	if filepath.Clean(stagingDir) == filepath.Clean(finalDir) {
		// container.json was written atomically after every other required file,
		// so it is the visibility boundary for a unified standalone store. The
		// marker is retained only for startup recovery if TTC crashes between
		// those two operations.
		if err := os.Remove(filepath.Join(finalDir, containerCreationMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else {
		if err := s.rename(stagingDir, finalDir); err != nil {
			return err
		}
	}
	directory, err := os.Open(filepath.Dir(finalDir))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
func normalizePortBinding(binding PortBinding) (PortBinding, error) {
	address, err := netip.ParseAddr(binding.HostIP)
	if err != nil || !address.Is4() {
		return PortBinding{}, fmt.Errorf("invalid published-port host address %q", binding.HostIP)
	}
	if binding.HostPort == 0 || binding.ContainerPort == 0 {
		return PortBinding{}, fmt.Errorf("published ports must be between 1 and 65535")
	}
	if binding.Protocol != "tcp" {
		return PortBinding{}, fmt.Errorf("unsupported published-port protocol %q", binding.Protocol)
	}
	binding.HostIP = address.String()
	binding.BranchID = binding.Branch()
	if err := validateObjectName("branch", binding.BranchID); err != nil {
		return PortBinding{}, err
	}
	return binding, nil
}

func portHostEndpoint(binding PortBinding) string {
	return netip.AddrPortFrom(netip.MustParseAddr(binding.HostIP), binding.HostPort).String() + "/" + binding.Protocol
}

func samePortBinding(a, b PortBinding) bool {
	return a.Branch() == b.Branch() && a.HostIP == b.HostIP && a.HostPort == b.HostPort &&
		a.ContainerPort == b.ContainerPort && a.Protocol == b.Protocol
}

// LoadContainer resolves an exact name, full ID, or unambiguous ID prefix.
func (s Store) LoadContainer(identifier string) (ContainerInfo, error) {
	return s.loadContainer(identifier, false)
}

// loadContainer is LoadContainer, which, if hidden is set, also returns the
// hidden target whose ID is identifier.
func (s Store) loadContainer(identifier string, hidden bool) (ContainerInfo, error) {
	if err := validateName(identifier); err != nil {
		return ContainerInfo{}, err
	}
	// An exact ID comes before a name, but an identifier no ID TTC makes can
	// be is a name: its hint comes first, and no record is looked for under
	// it, which would take a round trip of a shared root to find nothing.
	name := !generatedContainerID(identifier)
	byHint := func() (ContainerInfo, bool) {
		if id, ok := containerNameHint(s.Root + "\x00" + identifier); ok {
			if info, err := s.loadContainerKey(id); err == nil && info.Name == identifier && !info.DistributedForkHidden && !info.IsDistributedPhysicalRuntime() {
				return info, true
			}
		}
		return ContainerInfo{}, false
	}
	if name {
		if info, ok := byHint(); ok {
			return info, nil
		}
	}
	if info, err := s.loadContainerKey(identifier); err == nil {
		if hidden || !info.DistributedForkHidden {
			return info, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ContainerInfo{}, err
	}
	if !name {
		if info, ok := byHint(); ok {
			return info, nil
		}
	}
	// Search every container ListContainers lists.
	identities, err := s.publishedContainerIdentities()
	if err != nil {
		return ContainerInfo{}, err
	}
	var names, ids []string
	for id, identity := range identities {
		if identity.name == "" {
			continue
		}
		if identity.name == identifier {
			names = append(names, id)
		}
		if strings.HasPrefix(id, identifier) {
			ids = append(ids, id)
		}
	}
	found := func(id string) (ContainerInfo, error) {
		info, err := s.loadContainerKey(id)
		if errors.Is(err, os.ErrNotExist) {
			return ContainerInfo{}, fmt.Errorf("container %q does not exist: %w", identifier, os.ErrNotExist)
		}
		return info, err
	}
	if len(names) == 1 {
		return found(names[0])
	}
	if len(names) > 1 {
		return ContainerInfo{}, fmt.Errorf("container name %q is ambiguous", identifier)
	}
	if len(ids) == 1 {
		return found(ids[0])
	}
	if len(ids) > 1 {
		return ContainerInfo{}, fmt.Errorf("container ID prefix %q is ambiguous", identifier)
	}
	return ContainerInfo{}, fmt.Errorf("container %q does not exist: %w", identifier, os.ErrNotExist)
}

// loadRuntimeContainer resolves ordinary containers and an exact hidden
// distributed-target ID. Hidden targets remain absent from all name and prefix
// lookups, but runtime preparation must be able to consume their private bundle
// before the target is atomically published.
func (s Store) loadRuntimeContainer(identifier string) (ContainerInfo, error) {
	return s.LoadRuntimeContainer(identifier)
}

// LoadRuntimeContainer resolves ordinary containers and exact persistent
// container IDs. Callers using an internal distributed identity must already
// possess that ID from placement authority; this method deliberately does not
// expose hidden targets through names or ID prefixes. The exact-ID fallback is
// also important on NFS, where a newly committed authority placement can become
// visible before another client's directory cache sees the corresponding
// shared family record.
func (s Store) LoadRuntimeContainer(identifier string) (ContainerInfo, error) {
	info, err := s.loadContainer(identifier, true)
	if err == nil {
		return info, nil
	}
	exact, exactErr := s.loadContainerKey(identifier)
	if exactErr == nil && exact.ID == identifier {
		return exact, nil
	}
	return ContainerInfo{}, err
}

// mutateContainer serializes catalog read-modify-write transactions across
// foreground commands, background publication workers, and clustered daemons
// sharing TTC_ROOT. The lock is deliberately separate from the JSON file so
// atomic replacement of container.json does not replace the lock inode.
//
// An ID TTC makes is the key of its record, which loadContainer looks for
// before anything else: such an identifier's record is read once, under the
// lock, not first to resolve it, a round trip of a shared root.
func (s Store) mutateContainer(identifier string, mutate func(*ContainerInfo) (bool, error)) error {
	if generatedContainerID(identifier) {
		if found, err := s.mutateContainerKey(identifier, mutate); found {
			return err
		}
	}
	resolved, err := s.loadRuntimeContainer(identifier)
	if err != nil {
		return err
	}
	_, err = s.mutateContainerKey(resolved.ID, mutate)
	return err
}

// mutateContainerKey is mutateContainer of the record whose key is id. found
// is whether there is one.
func (s Store) mutateContainerKey(id string, mutate func(*ContainerInfo) (bool, error)) (found bool, err error) {
	directory := filepath.Join(s.Root, "containers", id)
	lock, err := os.OpenFile(filepath.Join(directory, ".metadata.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return true, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	info, err := s.loadContainerKey(id)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), err
	}
	changed, err := mutate(&info)
	if err != nil || !changed {
		return true, err
	}
	return true, writeJSON(filepath.Join(directory, "container.json"), &info)
}

// LoadDistributedForkTarget resolves a hidden or published target by its
// immutable transaction marker. It is intended only for ttcd reconciliation.
func (s Store) LoadDistributedForkTarget(family, transactionID string) (ContainerInfo, error) {
	if err := validateObjectName("distributed fork transaction", transactionID); err != nil {
		return ContainerInfo{}, err
	}
	// The target is a record of its own, named by the transaction and
	// hidden until the transaction publishes it, or one of the family's
	// published records, which imports the branch.
	ids, err := s.FamilyContainerIDs(family)
	if err != nil {
		return ContainerInfo{}, err
	}
	if !slices.Contains(ids, transactionID) {
		ids = append(ids, transactionID)
	}
	var found *ContainerInfo
	for _, id := range ids {
		info, err := s.loadContainerKey(id)
		if errors.Is(err, fs.ErrNotExist) {
			// Removed since it was listed, or not yet published.
			continue
		}
		if err != nil {
			return ContainerInfo{}, err
		}
		matches := info.DistributedForkTransactionID == transactionID
		if _, ok := info.DistributedForkImports[transactionID]; ok {
			matches = true
		}
		for _, branch := range info.Branches {
			if branch.DistributedForkTransactionID == transactionID {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if found != nil {
			return ContainerInfo{}, fmt.Errorf("distributed fork transaction %q has multiple target containers", transactionID)
		}
		found = &info
	}
	if found == nil {
		return ContainerInfo{}, fmt.Errorf("distributed fork transaction %q target: %w", transactionID, fs.ErrNotExist)
	}
	return *found, nil
}

func (s Store) loadContainerKey(key string) (ContainerInfo, error) {
	data, err := readFileWithStaleRetry(filepath.Join(s.Root, "containers", key, "container.json"))
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("reading container %q: %w", key, err)
	}
	var info ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return ContainerInfo{}, fmt.Errorf("decoding container %q: %w", key, err)
	}
	if info.ID != key || info.Name == "" || info.BundleDir == "" {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata", key)
	}
	if err := validateName(info.Name); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata: %w", key, err)
	}
	if info.State == "" {
		return ContainerInfo{}, fmt.Errorf("container %q has no lifecycle state", key)
	}
	if len(info.Branches) == 0 && !info.DistributedCatalogOnly {
		return ContainerInfo{}, fmt.Errorf("container %q has no branch metadata", key)
	}
	if info.DistributedCatalogOnly && info.IsDistributedPhysicalRuntime() {
		return ContainerInfo{}, fmt.Errorf("physical container %q cannot be a catalog-only family", key)
	}
	seenBranches := make(map[string]struct{}, len(info.Branches))
	for i := range info.Branches {
		branch := &info.Branches[i]
		if err := validateObjectName("branch", branch.ID); err != nil {
			return ContainerInfo{}, fmt.Errorf("container %q has invalid branch metadata: %w", key, err)
		}
		if branch.ID == branch.ParentID {
			return ContainerInfo{}, fmt.Errorf("container %q branch %q is its own parent", key, branch.ID)
		}
		if _, ok := seenBranches[branch.ID]; ok {
			return ContainerInfo{}, fmt.Errorf("container %q contains duplicate branch %q", key, branch.ID)
		}
		seenBranches[branch.ID] = struct{}{}
		if branch.FilesystemBranch == "" {
			return ContainerInfo{}, fmt.Errorf("container %q branch %q has no filesystem identity", key, branch.ID)
		}
		if branch.StorageState == "" {
			return ContainerInfo{}, fmt.Errorf("container %q branch %q has no storage state", key, branch.ID)
		}
	}
	seenPortEndpoints := make(map[string]struct{}, len(info.PublishedPorts))
	bindings := info.PublishedPorts[:0]
	for i := range info.PublishedPorts {
		candidate := info.PublishedPorts[i]
		binding, err := normalizePortBinding(candidate)
		if err != nil {
			return ContainerInfo{}, fmt.Errorf("container %q has invalid published-port metadata: %w", key, err)
		}
		if _, ok := seenBranches[binding.BranchID]; !ok {
			// Drop bindings whose branch no longer exists.
			continue
		}
		endpoint := portHostEndpoint(binding)
		if _, ok := seenPortEndpoints[endpoint]; ok {
			return ContainerInfo{}, fmt.Errorf("container %q contains duplicate published host endpoint %s", key, endpoint)
		}
		seenPortEndpoints[endpoint] = struct{}{}
		bindings = append(bindings, binding)
	}
	info.PublishedPorts = bindings
	networkMode, err := normalizeNetworkMode(info.NetworkMode)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata: %w", key, err)
	}
	info.NetworkMode = networkMode
	if info.LiveForkEligible && info.LiveForkWaiver != "" {
		return ContainerInfo{}, fmt.Errorf("container %q is live-fork eligible but records waiver %q", key, info.LiveForkWaiver)
	}
	if info.LiveForkEligible && networkMode == "host" {
		return ContainerInfo{}, fmt.Errorf("container %q is live-fork eligible but uses host networking", key)
	}
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		return ContainerInfo{}, fmt.Errorf("container %q is not live-fork eligible but has no waiver", key)
	}
	if err := checkGPURecord(info.GPUOptions, info.LiveForkEligible); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid GPU options: %w", key, err)
	}
	if len(info.PublishedPorts) != 0 && networkMode != "ttc" {
		return ContainerInfo{}, fmt.Errorf("container %q publishes ports with network mode %q", key, networkMode)
	}
	if info.LogDriver == "" {
		info.LogDriver = DefaultLogDriver
	}
	switch info.LogDriver {
	case "local", "json-file", "none":
	default:
		return ContainerInfo{}, fmt.Errorf("container %q has invalid log driver %q", key, info.LogDriver)
	}
	if info.StopSignal == "" {
		info.StopSignal = DefaultStopSignal
	}
	if info.StopTimeout == nil {
		value := DefaultStopTimeoutSeconds
		info.StopTimeout = &value
	}
	if *info.StopTimeout < -1 {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid stop timeout %d", key, *info.StopTimeout)
	}
	if info.ShmSize == nil {
		value := DefaultShmSizeBytes
		info.ShmSize = &value
	}
	if *info.ShmSize < 0 {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid shared memory size %d", key, *info.ShmSize)
	}
	if err := validateResourceLimits(SpecOptions{PidsLimit: info.PidsLimit, MemoryLimit: info.MemoryLimit, MemorySwap: info.MemorySwap, NanoCPUs: info.NanoCPUs, CPUShares: info.CPUShares, CPUPeriod: info.CPUPeriod, CPUQuota: info.CPUQuota}); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid resource limits: %w", key, err)
	}
	return info, nil
}

// LoadSpec returns the generated OCI runtime spec for a persistent container.
func (s Store) LoadSpec(name string) (*specs.Spec, error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return nil, err
	}
	return s.loadSpecOf(info, name)
}

// LoadSpecOf is LoadSpec of the container whose record, info, the caller has
// loaded already.
func (s Store) LoadSpecOf(info ContainerInfo) (*specs.Spec, error) {
	return s.loadSpecOf(info, info.ID)
}

// loadSpecOf is LoadSpec of the container whose record is info, which name
// named.
func (s Store) loadSpecOf(info ContainerInfo, name string) (*specs.Spec, error) {
	data, err := readFileWithStaleRetry(filepath.Join(info.BundleDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("reading OCI config for container %q: %w", name, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("decoding OCI config for container %q: %w", name, err)
	}
	if s.activeRoot() != s.Root {
		if spec.Root != nil && info.Digest != "" && info.Platform != "" {
			rootfs := filepath.Join(s.imageRootDir(), imageRootKey(info.Digest, info.Platform), "rootfs")
			if err := rewriteImageRoot(&spec, rootfs); err != nil {
				return nil, err
			}
		}
		// TTC-generated bind mounts are small node-private files (hostname,
		// hosts, and resolv.conf). A cold publication stores their contents,
		// and materialization installs them under the active root. Overlay those
		// paths in memory just like the image root and volume repositories; the
		// canonical shared OCI bundle must remain free of node-local paths.
		internalDir := filepath.Join(s.activeContainerDir(info.ID), "internal-mounts")
		if _, err := os.Stat(internalDir); err == nil {
			if spec.Annotations == nil || spec.Annotations[InternalMountsAnnotation] == "" {
				return nil, fmt.Errorf("container %q has node-local internal mounts but no declaration", info.Name)
			}
			var destinations []string
			if err := json.Unmarshal([]byte(spec.Annotations[InternalMountsAnnotation]), &destinations); err != nil {
				return nil, fmt.Errorf("decoding TTC internal mounts: %w", err)
			}
			declared := make(map[string]struct{}, len(destinations))
			for _, destination := range destinations {
				declared[filepath.Clean(destination)] = struct{}{}
			}
			rewritten := 0
			for i := range spec.Mounts {
				mount := &spec.Mounts[i]
				if _, ok := declared[filepath.Clean(mount.Destination)]; !ok {
					continue
				}
				path := filepath.Join(internalDir, fmt.Sprintf("%d-%s", i, filepath.Base(mount.Destination)))
				if _, err := os.Stat(path); err != nil {
					return nil, fmt.Errorf("opening node-local TTC internal mount %q: %w", mount.Destination, err)
				}
				mount.Source = path
				rewritten++
			}
			if rewritten != len(declared) {
				return nil, fmt.Errorf("OCI spec declares %d TTC internal mounts but matched %d", len(declared), rewritten)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("checking node-local TTC internal mounts: %w", err)
		}
	}
	mappings, err := s.activeRepositoryMappings(info.ID)
	if err != nil {
		return nil, err
	}
	if s.activeRoot() == s.Root && mappings == nil {
		return &spec, nil
	}
	repositories, err := specutils.TTCVolumeRepositories(&spec)
	if err != nil {
		return nil, err
	}
	if s.activeRepositories(info.ID, repositories, mappings) {
		encoded, err := json.Marshal(repositories)
		if err != nil {
			return nil, err
		}
		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
	}
	return &spec, nil
}

// activeRepositoryMappings returns the repository map of the container with
// the exact ID id on this node, nil if it has none. Restore aliases are
// one-shot resource keys and are recorded separately from the canonical OCI
// spec: the map lets a checkpoint's restore, on a standalone daemon too,
// advance from a captured repository identity to that of the newly
// installed local repository.
func (s Store) activeRepositoryMappings(id string) (map[string]activeRepositoryMapping, error) {
	data, err := readFileWithStaleRetry(filepath.Join(s.activeContainerDir(id), "repository-map.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mappings := make(map[string]activeRepositoryMapping)
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, fmt.Errorf("decoding active repository map: %w", err)
	}
	return mappings, nil
}

// activeRepositories makes repositories, those a spec of the container with
// the exact ID id names, the repositories this node has: as its repository
// map, mappings, installed them, or, on a cluster node, in its active root.
// It reports whether it changed any.
func (s Store) activeRepositories(id string, repositories []specutils.TTCVolumeRepository, mappings map[string]activeRepositoryMapping) bool {
	changed := false
	for i := range repositories {
		if mapping, ok := mappings[repositories[i].Source]; ok {
			repositories[i].Path = mapping.Path
			repositories[i].RestoreID = mapping.RestoreID
			if mapping.CopyMode != "" {
				repositories[i].CopyMode = mapping.CopyMode
			}
			changed = true
			continue
		}
		if s.activeRoot() == s.Root {
			continue
		}
		// Canonical specs are shared by every node. Their repository paths
		// may name the node that originally created the family and must
		// never become authoritative merely because that path happens to be
		// visible (for example two daemons running on one host, or a local
		// scratch tree accidentally exported over NFS). Resolve every known
		// TTC repository source into this node's active root.
		switch {
		case repositories[i].Source == specutils.TTCRootfsUpperSource:
			repositories[i].Path = filepath.Join(s.activeContainerDir(id), rootfsUpperRepositoryDir)
		case strings.HasPrefix(repositories[i].Source, "/run/ttc/volumes/"):
			repositories[i].Path = filepath.Join(s.activeRoot(), "filesystems", "volumes", filepath.Base(repositories[i].Source))
		case repositories[i].Private:
			repositories[i].Path = filepath.Join(s.activeContainerDir(id), hostMountRepositoryDir, filepath.Base(repositories[i].Path))
		default:
			continue
		}
		changed = true
	}
	return changed
}

// rewriteImageRoot makes every path-bearing immutable-image annotation agree
// with rootfs. Canonical clustered bundles may have been written by another
// node, so copying only Spec.Root.Path leaves the gofer looking for ownership
// metadata or lazy-pack state in the former node's cache. Keep this operation
// centralized so ordinary starts, checkpoint restore, and future cache
// materializers cannot acquire subtly different node-local views.
func rewriteImageRoot(spec *specs.Spec, rootfs string) error {
	if spec == nil || spec.Root == nil {
		return errors.New("OCI spec has no root")
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Root.Path = rootfs
	spec.Annotations["dev.gvisor.spec.rootfs.source"] = rootfs
	spec.Annotations[ownership.Annotation] = ownership.ManifestPath(rootfs)

	// Lazy image backing is a property of this node's cache, not of the
	// canonical image identity. A stale descriptor can make zero-length
	// skeleton files appear complete and later SIGBUS executable mappings.
	delete(spec.Annotations, ttcpack.LazyAnnotation)
	if _, err := ttcpack.ReadLazyDescriptor(rootfs); err == nil {
		spec.Annotations[ttcpack.LazyAnnotation] = ttcpack.LazyDescriptorPath(rootfs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("loading node-local lazy image descriptor: %w", err)
	}
	return nil
}

// WriteActiveRepositoryMap records node-local materializations without
// placing node-local host paths in the shared canonical OCI config.
func (s Store) WriteActiveRepositoryMap(name string, repositories []specutils.TTCVolumeRepository) error {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return err
	}
	mappings := make(map[string]activeRepositoryMapping, len(repositories))
	for _, repository := range repositories {
		mappings[repository.Source] = activeRepositoryMapping{Path: repository.Path, RestoreID: repository.RestoreID, CopyMode: repository.CopyMode}
	}
	return writeJSON(filepath.Join(s.activeContainerDir(info.ID), "repository-map.json"), mappings)
}

// CommitActiveRepositoryRestoreIDs replaces one-shot restore aliases with the
// identities of the repositories that are now installed on this node. Remote
// and cold restores initially key VFS resources by the repository identity in
// the captured Sentry. Once that restore has succeeded, any later capture is
// made from the node-local repositories and therefore names their identities.
// Keeping the old aliases would make a dehydrate/rehydrate cycle fail to
// reconnect its filesystem resources.
func (s Store) CommitActiveRepositoryRestoreIDs(name string) error {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return err
	}
	spec, err := s.LoadSpec(info.ID)
	if err != nil {
		return err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	for i := range repositories {
		info, err := ttcvolume.ReadRepositoryInfo(repositories[i].Path)
		if err != nil {
			return fmt.Errorf("reading restored repository %q: %w", repositories[i].Source, err)
		}
		repositories[i].RestoreID = info.ID
	}
	return s.WriteActiveRepositoryMap(info.ID, repositories)
}

// KeepContainerImageRoot reports whether the image root that the container
// named name runs from is unpacked on this node, and if it is, keeps it for
// the container (keepImageRoot).
func (s Store) KeepContainerImageRoot(name string) (bool, error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(s.imageRootDir(), imageRootKey(info.Digest, info.Platform))
	if s.activeRoot() == s.Root {
		_, err = loadImageRoot(dir)
		return err == nil, nil
	}
	return s.keepImageRoot(dir)
}

// PrepareContainerImageRoot unpacks on this node the image root that the
// container named name runs from, if it is not unpacked yet, without changing
// the container's records; RefreshContainerImageRoot then finds it ready.
func (s Store) PrepareContainerImageRoot(ctx context.Context, name string) error {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return err
	}
	if _, err := loadImageRoot(filepath.Join(s.imageRootDir(), imageRootKey(info.Digest, info.Platform))); err == nil {
		return nil
	}
	pulled, err := s.resolveImage(ctx, info.Digest, info.Platform, "never", nil)
	if err != nil {
		return fmt.Errorf("resolving image for container %q rootfs: %w", info.Name, err)
	}
	if _, _, err := s.prepareImageRoot(ctx, pulled, info.Platform, nil); err != nil {
		return fmt.Errorf("preparing image root for container %q: %w", info.Name, err)
	}
	return nil
}

// RefreshContainerImageRoot moves a stopped container bundle onto the current
// immutable-image root format without changing its writable TTC volume. This
// keeps containers created by older TTC builds startable after image backing
// metadata gains new virtualization requirements.
func (s Store) RefreshContainerImageRoot(ctx context.Context, name string) (ContainerInfo, error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return ContainerInfo{}, err
	}
	want := filepath.Join(s.imageRootDir(), imageRootKey(info.Digest, info.Platform), "rootfs")
	if info.RootFS == want {
		if s.activeRoot() != s.Root {
			if kept, err := s.keepImageRoot(filepath.Dir(want)); err != nil {
				return ContainerInfo{}, err
			} else if kept {
				return info, nil
			}
		} else if _, statErr := os.Stat(want); statErr == nil {
			return info, nil
		}
	}
	if s.activeRoot() == s.Root && strings.HasPrefix(filepath.Base(filepath.Dir(info.RootFS)), imageRootFormatVersion+"-") {
		return info, nil
	}
	pulled, err := s.resolveImage(ctx, info.Digest, info.Platform, "never", nil)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("resolving image for container %q rootfs upgrade: %w", info.Name, err)
	}
	_, rootfs, err := s.prepareImageRoot(ctx, pulled, info.Platform, nil)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("preparing current image root for container %q: %w", info.Name, err)
	}
	if rootfs == info.RootFS {
		return info, nil
	}
	if s.activeRoot() != s.Root {
		// The shared catalog records image identity, not a node's cache path.
		// LoadSpec rewrites the OCI root for each active node.
		info.RootFS = rootfs
		return info, nil
	}
	spec, err := s.LoadSpec(info.ID)
	if err != nil {
		return ContainerInfo{}, err
	}
	if spec.Root == nil {
		return ContainerInfo{}, fmt.Errorf("container %q OCI config has no root", info.Name)
	}
	spec.Root.Path = rootfs
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations[ownership.Annotation] = ownership.ManifestPath(rootfs)
	// Publish the complete OCI config first. If TTC exits before container.json
	// is updated, the next start harmlessly repeats this migration.
	if err := writeJSON(filepath.Join(info.BundleDir, "config.json"), spec); err != nil {
		return ContainerInfo{}, fmt.Errorf("updating OCI config for container %q image root: %w", info.Name, err)
	}
	if err := s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		current.RootFS = rootfs
		info = *current
		return true, nil
	}); err != nil {
		return ContainerInfo{}, fmt.Errorf("updating container %q image root: %w", info.Name, err)
	}
	return info, nil
}

// ListContainers returns all persistent containers ordered by name.
func (s Store) ListContainers() ([]ContainerInfo, error) {
	return s.listContainersWithPolicy(false, false, true)
}

// ListContainersForPlacement returns every published runtime-bearing
// container, including the physical containers that back distributed logical
// branches. Unlike ListContainers, this is an authority view rather than a
// user-facing inventory. Prepared targets remain hidden until their placement
// publication boundary commits.
func (s Store) ListContainersForPlacement() ([]ContainerInfo, error) {
	return s.listContainersWithPolicy(false, true, false)
}

// LoadContainerForPlacement is ListContainersForPlacement for the container
// id: it reports false for one that does not exist, is still being created,
// or is a prepared target not yet published.
func (s Store) LoadContainerForPlacement(id string) (ContainerInfo, bool, error) {
	containerDir := filepath.Join(s.Root, "containers", id)
	info, err := s.loadContainerKey(id)
	if err != nil {
		if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
			return ContainerInfo{}, false, nil
		}
		if _, statErr := os.Lstat(containerDir); errors.Is(statErr, fs.ErrNotExist) {
			return ContainerInfo{}, false, nil
		}
		return ContainerInfo{}, false, err
	}
	if info.DistributedForkHidden {
		return ContainerInfo{}, false, nil
	}
	return info, true, nil
}

// ListContainersForGC returns every durable container, including internal
// removal tombstones, and fails rather than treating an unreadable record as
// unreachable. Destructive graph reconciliation must use this view.
func (s Store) ListContainersForGC() ([]ContainerInfo, error) {
	return s.listContainers(true)
}

func (s Store) listContainers(includeHidden bool) ([]ContainerInfo, error) {
	return s.listContainersWithPolicy(includeHidden, true, false)
}

// listContainersWithPolicy separates availability-oriented discovery from
// conservative internal scans. User-facing listing skips and warns about one
// unreadable record so that an isolated damaged directory cannot hide every
// healthy container. Mutations and garbage collection remain strict: they
// must not infer that unreadable state is unreferenced.
func (s Store) listContainersWithPolicy(includeHidden, includePhysical, tolerateUnreadable bool) ([]ContainerInfo, error) {
	containersDir := filepath.Join(s.Root, "containers")
	entries, err := readDirWithStaleRetry(containersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ContainerInfo{}, nil
		}
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	loaded := s.loadContainerKeys(entries)
	containers := make([]ContainerInfo, 0, len(entries))
	for i, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		containerDir := filepath.Join(containersDir, entry.Name())
		info, err := loaded[i].info, loaded[i].err
		if err != nil {
			if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
				// Another cluster node may still be preparing this directory in
				// the shared data root: it is published when container.json is,
				// atomically, and the publication can become visible to NFS
				// clients one pathname at a time. The marker makes this an
				// unpublished record, not corruption.
				continue
			}
			if _, statErr := os.Lstat(containerDir); errors.Is(statErr, fs.ErrNotExist) {
				// Removed since the directory was read: removal renames the
				// record away before anything else, so nothing it held is
				// reachable from it. A family is removed while other commands
				// run, and a strict listing must not fail for it.
				continue
			}
			// Published since it was read, with its marker gone.
			info, err = s.loadContainerKey(entry.Name())
		}
		if err != nil {
			if tolerateUnreadable {
				key := filepath.Join(containersDir, entry.Name())
				if _, loaded := warnedUnreadableContainers.LoadOrStore(key, struct{}{}); !loaded {
					log.Warningf("Skipping unreadable TTC container directory %q during listing: %v", key, err)
				}
				continue
			}
			return nil, err
		}
		warnedUnreadableContainers.Delete(filepath.Join(containersDir, entry.Name()))
		// A published physical runtime must remain exactly addressable by daemon
		// lifecycle helpers, but authority placement is its only user-visible identity.
		// Never duplicate it in ordinary family inventory.
		physicalDistributedRuntime := info.IsDistributedPhysicalRuntime()
		if includeHidden || (!info.DistributedForkHidden && (includePhysical || !physicalDistributedRuntime)) {
			containers = append(containers, info)
		}
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Name != containers[j].Name {
			return containers[i].Name < containers[j].Name
		}
		return containers[i].ID < containers[j].ID
	})
	return containers, nil
}

// listReadWorkers is how many records a listing reads at once.
const listReadWorkers = 8

// loadedContainer is a record loadContainerKeys read, or why it could not.
type loadedContainer struct {
	info ContainerInfo
	err  error
}

// loadContainerKeys reads the records of the directories among entries, in
// entries' order. Each read waits on the shared root's round trips, which on
// a network mount make a listing's time: it reads several at once.
// readContainerSpecs reads the OCI config of each of infos, listReadWorkers
// at once.
func readContainerSpecs(infos []ContainerInfo) []loadedSpec {
	read := make([]loadedSpec, len(infos))
	work := make(chan int)
	var workers sync.WaitGroup
	for range min(listReadWorkers, len(infos)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range work {
				read[i].data, read[i].err = readFileWithStaleRetry(filepath.Join(infos[i].BundleDir, "config.json"))
			}
		}()
	}
	for i := range infos {
		work <- i
	}
	close(work)
	workers.Wait()
	return read
}

// loadedSpec is what readContainerSpecs read of a container's OCI config.
type loadedSpec struct {
	data []byte
	err  error
}

func (s Store) loadContainerKeys(entries []fs.DirEntry) []loadedContainer {
	loaded := make([]loadedContainer, len(entries))
	work := make(chan int)
	var workers sync.WaitGroup
	for range min(listReadWorkers, len(entries)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range work {
				loaded[i].info, loaded[i].err = s.loadContainerKey(entries[i].Name())
			}
		}()
	}
	for i, entry := range entries {
		if entry.IsDir() {
			work <- i
		}
	}
	close(work)
	workers.Wait()
	return loaded
}

// PublishDistributedForkTarget atomically makes a prepared target visible to
// ordinary TTC listing and lookup.
func (s Store) PublishDistributedForkTarget(family, transactionID string) (ContainerInfo, error) {
	info, err := s.LoadDistributedForkTarget(family, transactionID)
	if err != nil {
		return ContainerInfo{}, err
	}
	var published ContainerInfo
	err = s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if current.DistributedForkTransactionID == transactionID {
			if !current.DistributedForkHidden {
				published = *current
				return false, nil
			}
			current.DistributedForkHidden = false
		} else if pending, ok := current.DistributedForkImports[transactionID]; ok {
			physicalBranch := pending.PhysicalBranchID
			if physicalBranch == "" {
				physicalBranch = pending.BranchID
			}
			if pending.Migration {
				found := false
				for i := range current.Branches {
					branch := &current.Branches[i]
					if branch.ID != physicalBranch {
						continue
					}
					if !pending.AuthoritativeMigration && branch.NodeID != pending.SourceNode {
						return false, fmt.Errorf("migration branch %q owner changed from %q to %q", pending.BranchID, pending.SourceNode, branch.NodeID)
					}
					branch.FilesystemBranch = pending.FilesystemBranch
					branch.Quarantined = pending.Quarantined
					branch.NodeID = pending.TargetNode
					branch.State = pending.State
					branch.StorageState = "local"
					branch.SourceNode = pending.SourceNode
					branch.DependentMemoryBytes = pending.DependentMemoryBytes
					branch.DependentFilesystemBytes = pending.DependentFilesystemBytes
					branch.DistributedForkTransactionID = transactionID
					branch.DistributedMigration = true
					branch.SnapshotID = pending.SnapshotID
					branch.MemoryRoots = append([]string(nil), pending.MemoryRoots...)
					branch.MemoryRootObjects = append([]string(nil), pending.MemoryRootObjects...)
					branch.DistributedGraphReferenceOperation = pending.GraphReferenceOperation
					found = true
					break
				}
				if !found {
					// Physical family records on different nodes need not contain
					// the same branch set. Import the migrating branch alongside the
					// existing local siblings only after placement has cut over.
					current.Branches = append(current.Branches, BranchInfo{
						ID: physicalBranch, ParentID: pending.ParentID, CreatedAt: pending.CreatedAt,
						FilesystemBranch: pending.FilesystemBranch, Quarantined: pending.Quarantined,
						NodeID: pending.TargetNode, State: pending.State, StorageState: "local", SourceNode: pending.SourceNode,
						DependentMemoryBytes: pending.DependentMemoryBytes, DependentFilesystemBytes: pending.DependentFilesystemBytes,
						DistributedForkTransactionID: transactionID, DistributedMigration: true,
						SnapshotID: pending.SnapshotID, MemoryRoots: append([]string(nil), pending.MemoryRoots...),
						MemoryRootObjects:                  append([]string(nil), pending.MemoryRootObjects...),
						DistributedGraphReferenceOperation: pending.GraphReferenceOperation,
					})
				}
			} else {
				for _, branch := range current.Branches {
					if branch.ID == pending.BranchID {
						return false, fmt.Errorf("branch %q appeared while publishing distributed import", pending.BranchID)
					}
				}
				current.Branches = append(current.Branches, BranchInfo{
					ID: pending.BranchID, ParentID: pending.ParentID, FilesystemBranch: pending.FilesystemBranch,
					Quarantined: pending.Quarantined,
					NodeID:      pending.TargetNode,
					State:       pending.State, StorageState: "local", SourceNode: pending.SourceNode, CreatedAt: pending.CreatedAt,
					DependentMemoryBytes: pending.DependentMemoryBytes, DependentFilesystemBytes: pending.DependentFilesystemBytes,
					DistributedForkTransactionID: transactionID,
					SnapshotID:                   pending.SnapshotID, MemoryRoots: append([]string(nil), pending.MemoryRoots...),
					MemoryRootObjects:                  append([]string(nil), pending.MemoryRootObjects...),
					DistributedGraphReferenceOperation: pending.GraphReferenceOperation,
				})
			}
			delete(current.DistributedForkImports, transactionID)
			current.DistributedCatalogOnly = false
			recomputeContainerState(current)
		} else {
			for _, branch := range current.Branches {
				if branch.DistributedForkTransactionID == transactionID {
					published = *current
					return false, nil
				}
			}
			return false, fmt.Errorf("distributed fork transaction %q has no pending target", transactionID)
		}
		published = *current
		return true, nil
	})
	if err != nil {
		return ContainerInfo{}, err
	}
	return published, nil
}

// UpdateDistributedForkDependencies records volatile source bytes while a
// target is hidden or already visible. Values are observations and may move
// downward without changing lifecycle placement.
func (s Store) UpdateDistributedForkDependencies(family, transactionID string, memoryBytes, filesystemBytes uint64) error {
	info, err := s.LoadDistributedForkTarget(family, transactionID)
	if err != nil {
		return err
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if index, ok := distributedForkBranchIndex(*current, transactionID); ok {
			branch := &current.Branches[index]
			if branch.DependentMemoryBytes == memoryBytes && branch.DependentFilesystemBytes == filesystemBytes {
				return false, nil
			}
			branch.DependentMemoryBytes = memoryBytes
			branch.DependentFilesystemBytes = filesystemBytes
			return true, nil
		}
		if pending, ok := current.DistributedForkImports[transactionID]; ok {
			if pending.DependentMemoryBytes == memoryBytes && pending.DependentFilesystemBytes == filesystemBytes {
				return false, nil
			}
			pending.DependentMemoryBytes = memoryBytes
			pending.DependentFilesystemBytes = filesystemBytes
			current.DistributedForkImports[transactionID] = pending
			return true, nil
		}
		return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
	})
}

// UpdateDistributedForkTargetState records hidden-target lifecycle without
// making the target visible.
func (s Store) UpdateDistributedForkTargetState(family, transactionID, state string) error {
	info, err := s.LoadDistributedForkTarget(family, transactionID)
	if err != nil {
		return err
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if index, ok := distributedForkBranchIndex(*current, transactionID); ok {
			current.Branches[index].State = state
			recomputeContainerState(current)
		} else if pending, ok := current.DistributedForkImports[transactionID]; ok {
			pending.State = state
			current.DistributedForkImports[transactionID] = pending
		} else {
			return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
		}
		return true, nil
	})
}

// UpdateDistributedForkMemoryGraph records the immutable graph roots selected
// by a hidden or published distributed target. Target-local logical names are
// derived from its immutable filesystem-branch identity, not its visible name.
func (s Store) UpdateDistributedForkMemoryGraph(family, transactionID string, snapshotID uint64, objects []string, referenceOperation string) error {
	if snapshotID == 0 || len(objects) == 0 || len(objects) > 4096 {
		return errors.New("distributed graph memory identity is incomplete")
	}
	if referenceOperation == "" || filepath.Base(referenceOperation) != referenceOperation || strings.ContainsAny(referenceOperation, `/\\`) {
		return errors.New("distributed graph reference operation is invalid")
	}
	info, err := s.LoadDistributedForkTarget(family, transactionID)
	if err != nil {
		return err
	}
	makeRoots := func(filesystemBranch string) []string {
		roots := make([]string, len(objects))
		for index := range roots {
			roots[index] = fmt.Sprintf("memory/branch/%s/resource-%04x", filesystemBranch, index)
		}
		return roots
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if index, ok := distributedForkBranchIndex(*current, transactionID); ok {
			branch := &current.Branches[index]
			branch.SnapshotID = snapshotID
			branch.MemoryRoots = makeRoots(branch.FilesystemBranch)
			branch.MemoryRootObjects = append([]string(nil), objects...)
			branch.DistributedGraphReferenceOperation = referenceOperation
			return true, nil
		}
		if pending, ok := current.DistributedForkImports[transactionID]; ok {
			pending.SnapshotID = snapshotID
			pending.MemoryRoots = makeRoots(pending.FilesystemBranch)
			pending.MemoryRootObjects = append([]string(nil), objects...)
			pending.GraphReferenceOperation = referenceOperation
			current.DistributedForkImports[transactionID] = pending
			return true, nil
		}
		for index := range current.Branches {
			branch := &current.Branches[index]
			if branch.DistributedForkTransactionID != transactionID {
				continue
			}
			branch.SnapshotID = snapshotID
			branch.MemoryRoots = makeRoots(branch.FilesystemBranch)
			branch.MemoryRootObjects = append([]string(nil), objects...)
			branch.DistributedGraphReferenceOperation = referenceOperation
			return true, nil
		}
		return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
	})
}

// DistributedForkTargetBranch resolves the branch associated with a target
// transaction. peer is true when it was imported into an existing family.
func (s Store) DistributedForkTargetBranch(family, transactionID string) (info ContainerInfo, branch BranchInfo, peer bool, err error) {
	info, err = s.LoadDistributedForkTarget(family, transactionID)
	if err != nil {
		return ContainerInfo{}, BranchInfo{}, false, err
	}
	if index, ok := distributedForkBranchIndex(info, transactionID); ok {
		candidate := info.Branches[index]
		candidate.DistributedForkTransactionID = transactionID
		return info, candidate, info.DistributedForkTransactionID != transactionID || len(info.Branches) > 1, nil
	}
	if pending, ok := info.DistributedForkImports[transactionID]; ok {
		physicalBranch := pending.PhysicalBranchID
		if physicalBranch == "" {
			physicalBranch = pending.BranchID
		}
		branch = BranchInfo{ID: physicalBranch, ParentID: pending.ParentID, CreatedAt: pending.CreatedAt}
		// A migration replaces an existing logical branch. Preserve its display
		// lineage and other stable metadata while the pending import supplies the
		// new runtime/filesystem identities.
		if pending.Migration {
			for _, candidate := range info.Branches {
				if candidate.ID == physicalBranch {
					branch = candidate
					break
				}
			}
		}
		branch.FilesystemBranch = pending.FilesystemBranch
		branch.Quarantined = pending.Quarantined
		branch.State = pending.State
		branch.DistributedForkTransactionID = transactionID
		branch.DistributedMigration = pending.Migration
		return info, branch, true, nil
	}
	for _, candidate := range info.Branches {
		if candidate.DistributedForkTransactionID == transactionID {
			return info, candidate, true, nil
		}
	}
	return ContainerInfo{}, BranchInfo{}, false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
}

// distributedForkBranchIndex resolves the branch originally created by a
// transaction even after its physical Sentry begins hosting imported sibling
// branches. New records always carry the branch-level marker. The singleton
// fallback supports a target while it is still in its initial hidden form.
func distributedForkBranchIndex(info ContainerInfo, transactionID string) (int, bool) {
	for index := range info.Branches {
		if info.Branches[index].DistributedForkTransactionID == transactionID {
			return index, true
		}
	}
	if info.DistributedForkTransactionID == transactionID && len(info.Branches) == 1 {
		return 0, true
	}
	return 0, false
}

// RemoveDistributedForkTarget removes a transaction target and its image
// lease. Borrowed source-family repositories live outside the target directory
// and are deliberately retained.
func (s Store) RemoveDistributedForkTarget(family, transactionID string) error {
	info, branch, peer, err := s.DistributedForkTargetBranch(family, transactionID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if peer {
		pendingImport, pending := info.DistributedForkImports[transactionID]
		if !pending && branch.DistributedMigration {
			return fmt.Errorf("published distributed migration %q cannot be canceled", transactionID)
		}
		if err := s.RemoveBranchFilesystems(info.ID, branch); err != nil {
			return err
		}
		if pending {
			// A migration import borrows the existing logical branch's console
			// and exec artifacts. Cancel only the unpublished filesystem fork.
			if !pendingImport.Migration {
				if err := s.RemoveBranchArtifacts(info.ID, branch); err != nil {
					return err
				}
			}
			return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
				if _, ok := current.DistributedForkImports[transactionID]; !ok {
					return false, nil
				}
				delete(current.DistributedForkImports, transactionID)
				recomputeContainerState(current)
				return true, nil
			})
		}
		if err := s.RemoveBranchArtifacts(info.ID, branch); err != nil {
			return err
		}
		return s.RemoveBranchRecord(info.ID, branch.ID)
	}
	return s.removeContainerInfo(info)
}

func (s Store) removeDistributedMemoryMetadata(info ContainerInfo) error {
	if info.DistributedForkTransactionID == "" {
		return nil
	}
	artifacts := []struct {
		path string
		name string
	}{{filepath.Join(s.Root, "cluster", "memory-metadata", info.DistributedForkTransactionID+".state"), "kernel metadata"}}
	for _, artifact := range artifacts {
		if err := os.Remove(artifact.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing distributed %s: %w", artifact.name, err)
		}
	}
	return nil
}

// UpdateState is retained for checkpoint restore, whose newly-created family
// contains only main. It never rewrites peer branches; callers managing an
// existing family must use the branch-specific state methods instead.
func (s Store) UpdateState(name, state string, exitStatus *int) error {
	now := time.Now().UTC()
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		found := false
		for i := range info.Branches {
			if info.Branches[i].ID == "main" {
				updateBranchLifecycle(&info.Branches[i], state, exitStatus, now)
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Errorf("branch %q is not recorded", "main")
		}
		if state == "running" {
			info.StartedAt = &now
		}
		recomputeContainerState(info)
		if info.State == "exited" {
			info.ExitStatus = exitStatus
			info.ExitedAt = &now
		} else {
			info.ExitStatus = nil
			info.ExitedAt = nil
			info.OOMKilled = false
			info.ExitError = ""
		}
		return true, nil
	})
}

// AddPortBinding atomically records a branch-local published TCP listener.
func (s Store) AddPortBinding(name string, binding PortBinding) error {
	binding, err := normalizePortBinding(binding)
	if err != nil {
		return err
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		if info.NetworkMode != "ttc" {
			return false, fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
		}
		branchFound := false
		for _, branch := range info.Branches {
			if branch.ID != binding.BranchID {
				continue
			}
			if branch.State != "running" && branch.State != "paused" {
				return false, fmt.Errorf("branch %q is %s", binding.BranchID, branch.State)
			}
			branchFound = true
			break
		}
		if !branchFound {
			return false, fmt.Errorf("branch %q is not recorded", binding.BranchID)
		}
		endpoint := portHostEndpoint(binding)
		for _, existing := range info.PublishedPorts {
			if samePortBinding(existing, binding) {
				return false, fmt.Errorf("published port for %s already exists on branch %q", endpoint, binding.BranchID)
			}
			if portHostEndpoint(existing) == endpoint {
				return false, fmt.Errorf("published host endpoint %s is already assigned to branch %q", endpoint, existing.Branch())
			}
		}
		info.PublishedPorts = append(info.PublishedPorts, binding)
		sort.Slice(info.PublishedPorts, func(i, j int) bool {
			a, b := info.PublishedPorts[i], info.PublishedPorts[j]
			if a.Branch() != b.Branch() {
				return a.Branch() < b.Branch()
			}
			if a.HostIP != b.HostIP {
				return a.HostIP < b.HostIP
			}
			if a.HostPort != b.HostPort {
				return a.HostPort < b.HostPort
			}
			return a.ContainerPort < b.ContainerPort
		})
		return true, nil
	})
}

// RemovePortBinding atomically removes one exact branch-local listener.
func (s Store) RemovePortBinding(name string, binding PortBinding) error {
	binding, err := normalizePortBinding(binding)
	if err != nil {
		return err
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i, existing := range info.PublishedPorts {
			if !samePortBinding(existing, binding) {
				continue
			}
			info.PublishedPorts = append(info.PublishedPorts[:i], info.PublishedPorts[i+1:]...)
			return true, nil
		}
		return false, fmt.Errorf("published port is not configured on branch %q", binding.BranchID)
	})
}

// RemovePortBindingsForBranch atomically forgets every listener for branchID.
func (s Store) RemovePortBindingsForBranch(name, branchID string) error {
	if err := validateObjectName("branch", branchID); err != nil {
		return err
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		bindings := info.PublishedPorts[:0]
		for _, binding := range info.PublishedPorts {
			if binding.Branch() != branchID {
				bindings = append(bindings, binding)
			}
		}
		if len(bindings) == len(info.PublishedPorts) {
			return false, nil
		}
		info.PublishedPorts = bindings
		return true, nil
	})
}

// RecordBranch appends one branch identity or refreshes its lifecycle data.
func (s Store) RecordBranch(name string, branch BranchInfo) error {
	return s.RecordBranches(name, []BranchInfo{branch})
}

// RecordBranches atomically appends or refreshes multiple branch identities.
func (s Store) RecordBranches(name string, branches []BranchInfo) error {
	if len(branches) == 0 {
		return errors.New("at least one branch is required")
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		index := make(map[string]int, len(info.Branches))
		for i := range info.Branches {
			index[info.Branches[i].ID] = i
		}
		seen := make(map[string]struct{}, len(branches))
		for _, branch := range branches {
			if err := validateObjectName("branch", branch.ID); err != nil {
				return false, err
			}
			if branch.ID == branch.ParentID {
				return false, fmt.Errorf("branch %q cannot be its own parent", branch.ID)
			}
			if branch.State == "" {
				return false, fmt.Errorf("branch %q has no lifecycle state", branch.ID)
			}
			if branch.FilesystemBranch == "" {
				return false, fmt.Errorf("branch %q has no filesystem identity", branch.ID)
			}
			if branch.StorageState == "" {
				return false, fmt.Errorf("branch %q has no storage state", branch.ID)
			}
			if _, ok := seen[branch.ID]; ok {
				return false, fmt.Errorf("branch %q is repeated", branch.ID)
			}
			seen[branch.ID] = struct{}{}
			if i, ok := index[branch.ID]; ok {
				if info.Branches[i].ParentID != branch.ParentID {
					return false, fmt.Errorf("branch %q parent changed from %q to %q", branch.ID, info.Branches[i].ParentID, branch.ParentID)
				}
				info.Branches[i] = branch
				continue
			}
			index[branch.ID] = len(info.Branches)
			info.Branches = append(info.Branches, branch)
		}
		info.DistributedCatalogOnly = false
		recomputeContainerState(info)
		return true, nil
	})
}

// recomputeContainerState derives the family summary from all surviving
// branches. Branch ancestry is presentation metadata only: main and its
// descendants are lifecycle peers, so no particular branch owns this state.
func recomputeContainerState(info *ContainerInfo) {
	priority := map[string]int{
		"exited":      1,
		"stopped":     1,
		"failed":      2,
		"dead":        2,
		"dehydrated":  3,
		"created":     4,
		"paused":      5,
		"dehydrating": 6,
		"rehydrating": 7,
		"creating":    8,
		"restarting":  9,
		"running":     10,
	}
	state := "exited"
	best := 0
	for _, branch := range info.Branches {
		if rank := priority[branch.State]; rank > best {
			state, best = branch.State, rank
		}
	}
	info.State = state
	if state != "exited" && state != "stopped" {
		info.ExitStatus = nil
		info.ExitedAt = nil
		info.OOMKilled = false
		info.ExitError = ""
	}
}

// updateBranchLifecycle is the single persistence boundary for branch-local
// process state. Exit status is branch state, not family state: siblings may
// terminate independently and a cold branch must remain waitable after its
// Sentry and console sidecar have been reclaimed.
func updateBranchLifecycle(branch *BranchInfo, state string, exitStatus *int, now time.Time) {
	branch.State = state
	if state == "exited" || state == "stopped" {
		if exitStatus != nil {
			status := *exitStatus
			branch.ExitStatus = &status
			finished := now
			branch.ExitedAt = &finished
		}
		return
	}
	branch.ExitStatus = nil
	branch.ExitedAt = nil
}

// UpdateBranchState changes only the last known lifecycle state of a recorded branch.
func (s Store) UpdateBranchState(name, branchID, state string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			if info.Branches[i].ID != branchID {
				continue
			}
			updateBranchLifecycle(&info.Branches[i], state, nil, time.Now().UTC())
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// MarkBranchDehydratedResident records that branchID is dehydrated and that
// its capture exists only in its family's Sentry. See StorageResident.
func (s Store) MarkBranchDehydratedResident(name, branchID string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			updateBranchLifecycle(branch, "dehydrated", nil, time.Now().UTC())
			branch.StorageState = StorageResident
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// MarkBranchDehydrationDurable records that a resident dehydration of
// branchID has been made durable in the data root.
func (s Store) MarkBranchDehydrationDurable(name, branchID string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.StorageState != StorageResident {
				return false, nil
			}
			branch.StorageState = "local"
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// UpdateBranchQuarantine changes the durable external-connectivity flag.
func (s Store) UpdateBranchQuarantine(name, branchID string, quarantined bool) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			if info.Branches[i].ID != branchID {
				continue
			}
			if info.Branches[i].Quarantined == quarantined {
				return false, nil
			}
			info.Branches[i].Quarantined = quarantined
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// UpdateBranchMemoryGraph records or clears the authoritative logical memory
// roots for a branch. Physical provider/cache locations are deliberately not
// part of this record.
func (s Store) UpdateBranchMemoryGraph(name, branchID string, snapshotID uint64, roots, objects []string, referenceOperation string) error {
	return s.updateBranchSnapshotGraph(name, branchID, snapshotID, roots, objects, "", "", referenceOperation, false)
}

// UpdateBranchSnapshotGraph atomically records all immutable process-state
// roots selected by one dehydration capture.
func (s Store) UpdateBranchSnapshotGraph(name, branchID string, snapshotID uint64, roots, objects []string, controlRoot, controlObject, referenceOperation string) error {
	return s.updateBranchSnapshotGraph(name, branchID, snapshotID, roots, objects, controlRoot, controlObject, referenceOperation, true)
}

// ParkBranchMemoryGraph transfers a branch's immutable graph references to
// family-level runtime ownership. The operation is atomic and idempotent. It
// is used before destroying a live branch whose unresolved page loader may be
// transferred by the Sentry to a surviving sibling.
func (s Store) ParkBranchMemoryGraph(name, branchID string) (RuntimeMemoryBacking, bool, error) {
	var parked RuntimeMemoryBacking
	var changed bool
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
				return false, nil
			}
			if len(branch.MemoryRoots) != len(branch.MemoryRootObjects) {
				return false, fmt.Errorf("branch %q memory graph has %d roots and %d objects", branchID, len(branch.MemoryRoots), len(branch.MemoryRootObjects))
			}
			parked = RuntimeMemoryBacking{
				ID:                                 fmt.Sprintf("%s-%016x", branch.FilesystemBranch, branch.SnapshotID),
				SnapshotID:                         branch.SnapshotID,
				MemoryRoots:                        append([]string(nil), branch.MemoryRoots...),
				MemoryRootObjects:                  append([]string(nil), branch.MemoryRootObjects...),
				ControlRoot:                        branch.ControlRoot,
				ControlRootObject:                  branch.ControlRootObject,
				DistributedGraphReferenceOperation: branch.DistributedGraphReferenceOperation,
			}
			for _, existing := range info.RuntimeMemoryBackings {
				if existing.ID != parked.ID {
					continue
				}
				if existing.SnapshotID != parked.SnapshotID ||
					!slices.Equal(existing.MemoryRoots, parked.MemoryRoots) ||
					!slices.Equal(existing.MemoryRootObjects, parked.MemoryRootObjects) ||
					existing.ControlRoot != parked.ControlRoot || existing.ControlRootObject != parked.ControlRootObject ||
					existing.DistributedGraphReferenceOperation != parked.DistributedGraphReferenceOperation {
					return false, fmt.Errorf("runtime memory backing %q already names different graph state", parked.ID)
				}
				break
			}
			found := false
			for _, existing := range info.RuntimeMemoryBackings {
				found = found || existing.ID == parked.ID
			}
			if !found {
				info.RuntimeMemoryBackings = append(info.RuntimeMemoryBackings, parked)
			}
			branch.SnapshotID = 0
			branch.MemoryRoots = nil
			branch.MemoryRootObjects = nil
			branch.ControlRoot = ""
			branch.ControlRootObject = ""
			branch.DistributedGraphReferenceOperation = ""
			changed = true
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return parked, changed, err
}

// RemoveRuntimeMemoryBacking removes id after its loader no longer has
// outstanding pages. Graph and distributed references are released before
// this metadata commit, making retries idempotent after a crash.
func (s Store) RemoveRuntimeMemoryBacking(name, id string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.RuntimeMemoryBackings {
			if info.RuntimeMemoryBackings[i].ID != id {
				continue
			}
			info.RuntimeMemoryBackings = append(info.RuntimeMemoryBackings[:i], info.RuntimeMemoryBackings[i+1:]...)
			return true, nil
		}
		return false, nil
	})
}

func (s Store) updateBranchSnapshotGraph(name, branchID string, snapshotID uint64, roots, objects []string, controlRoot, controlObject, referenceOperation string, replaceControl bool) error {
	if len(roots) != len(objects) {
		return fmt.Errorf("memory graph has %d root names and %d root objects", len(roots), len(objects))
	}
	if replaceControl && (controlRoot == "") != (controlObject == "") {
		return errors.New("control graph root name and object must be set together")
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			branch.SnapshotID = snapshotID
			branch.MemoryRoots = append([]string(nil), roots...)
			branch.MemoryRootObjects = append([]string(nil), objects...)
			if replaceControl {
				branch.ControlRoot = controlRoot
				branch.ControlRootObject = controlObject
			}
			branch.DistributedGraphReferenceOperation = referenceOperation
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// ClearBranchControlGraphIf clears a rehydrated branch's consumed control
// root only if the branch still names the exact capture observed by the
// caller, and keeps its memory roots.
func (s Store) ClearBranchControlGraphIf(name, branchID string, snapshotID uint64, controlRoot, controlObject string) (bool, error) {
	cleared := false
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.SnapshotID != snapshotID || branch.ControlRoot != controlRoot || branch.ControlRootObject != controlObject {
				return false, nil
			}
			branch.ControlRoot = ""
			branch.ControlRootObject = ""
			cleared = true
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return cleared, err
}

// ClearBranchMemoryGraphIf clears a consumed immutable-memory backing only if
// the branch still names the exact capture observed by the caller. Rehydrate,
// dehydrate, fork, migration, and name reuse may all replace branch metadata
// while an asynchronous page loader is finishing; a stale completion must not
// release the replacement capture.
func (s Store) ClearBranchMemoryGraphIf(name, branchID string, snapshotID uint64, roots, objects []string, referenceOperation string) (bool, error) {
	if len(roots) != len(objects) {
		return false, fmt.Errorf("memory graph has %d root names and %d root objects", len(roots), len(objects))
	}
	cleared := false
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.SnapshotID != snapshotID ||
				!slices.Equal(branch.MemoryRoots, roots) ||
				!slices.Equal(branch.MemoryRootObjects, objects) ||
				branch.DistributedGraphReferenceOperation != referenceOperation {
				return false, nil
			}
			branch.SnapshotID = 0
			branch.MemoryRoots = nil
			branch.MemoryRootObjects = nil
			branch.ControlRoot = ""
			branch.ControlRootObject = ""
			branch.DistributedGraphReferenceOperation = ""
			cleared = true
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return cleared, err
}

// ReplaceBranchMemoryGraphIf replaces the memory graph a branch retains, if
// it is still roots under snapshotID, with newRoots under newSnapshotID. It
// reports whether it replaced it.
func (s Store) ReplaceBranchMemoryGraphIf(name, branchID string, snapshotID uint64, roots, objects []string, newSnapshotID uint64, newRoots, newObjects []string) (bool, error) {
	if len(roots) != len(objects) || len(newRoots) != len(newObjects) || len(newRoots) == 0 || newSnapshotID == 0 {
		return false, fmt.Errorf("invalid memory graph replacement of %d roots by %d", len(roots), len(newRoots))
	}
	replaced := false
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.SnapshotID != snapshotID ||
				!slices.Equal(branch.MemoryRoots, roots) ||
				!slices.Equal(branch.MemoryRootObjects, objects) ||
				branch.DistributedGraphReferenceOperation != "" {
				return false, nil
			}
			branch.SnapshotID = newSnapshotID
			branch.MemoryRoots = append([]string(nil), newRoots...)
			branch.MemoryRootObjects = append([]string(nil), newObjects...)
			replaced = true
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return replaced, err
}

// NewBranchPublicationID allocates an immutable publication identity without
// changing branch metadata. Callers use this while constructing a hidden
// capture, then install it with BeginBranchPublicationIf only after the capture
// fence is complete. Keeping preparation invisible prevents a failed capture
// from superseding a valid publication that is still being uploaded.
func (s Store) NewBranchPublicationID() (string, error) {
	nonce, err := randomObjectID("publication", 16)
	if err != nil {
		return "", err
	}
	return "publication-" + nonce, nil
}

// BeginBranchPublicationIf atomically installs publicationID if the branch
// still names expectedPublicationID. It returns the exact storage metadata
// that was replaced. The compare-and-swap fences overlapping exit observers
// and publication workers running in other daemons against the same shared
// container catalog.
func (s Store) BeginBranchPublicationIf(name, branchID, expectedPublicationID, publicationID string) (BranchInfo, error) {
	if publicationID == "" {
		return BranchInfo{}, errors.New("publication identity is empty")
	}
	var previous BranchInfo
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.PublicationID != expectedPublicationID {
				return false, fmt.Errorf("branch %q publication changed from %q to %q", branchID, expectedPublicationID, branch.PublicationID)
			}
			previous = *branch
			branch.PublicationID = publicationID
			branch.StorageState = "publishing"
			branch.StorageError = ""
			branch.StorageRetryAt = time.Time{}
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return previous, err
}

// BeginBranchPublication is retained for callers that do not construct a
// capture in stages. New lifecycle code should use NewBranchPublicationID and
// BeginBranchPublicationIf so a failed preparation never becomes visible.
func (s Store) BeginBranchPublication(name, branchID string) (string, error) {
	publicationID, err := s.NewBranchPublicationID()
	if err != nil {
		return "", err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return "", err
	}
	var expected string
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			expected = branch.PublicationID
			break
		}
	}
	_, err = s.BeginBranchPublicationIf(name, branchID, expected, publicationID)
	return publicationID, err
}

// UpdateBranchStorage publishes an identity-fenced storage transition. A
// worker from an older transaction cannot overwrite a newer capture or
// resurrect a branch whose publication was canceled and removed.
func (s Store) UpdateBranchStorage(name, branchID, publicationID, state, storageError string, retryAt time.Time) error {
	switch state {
	case "local", "publishing", "shared", "materializing", "error":
	default:
		return fmt.Errorf("invalid storage state %q", state)
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if publicationID == "" || publicationID != branch.PublicationID {
				return false, fmt.Errorf("branch %q publication is %q, worker has %q", branchID, branch.PublicationID, publicationID)
			}
			branch.StorageState = state
			branch.StorageError = storageError
			branch.StorageRetryAt = retryAt
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// CompleteBranchPublication atomically commits shared durability and the
// lifecycle transition that was waiting on it.
func (s Store) CompleteBranchPublication(name, branchID, publicationID, manifest, lifecycle string, unplace bool) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if publicationID == "" || branch.PublicationID != publicationID {
				return false, fmt.Errorf("branch %q publication is %q, worker has %q", branchID, branch.PublicationID, publicationID)
			}
			branch.StorageState = "shared"
			branch.StorageError = ""
			branch.StorageManifest = manifest
			branch.StorageRetryAt = time.Time{}
			if lifecycle != "" {
				branch.State = lifecycle
			}
			if unplace {
				branch.NodeID = ""
				branch.Unplaced = true
			}
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// CompleteConsumedBranchPublication rolls derived physical metadata forward
// after the authority has consumed a publication transaction but retained its immutable
// filesystem root. The exact manifest is the fence in this state: the authority no
// longer names a publication ID, and StopBranch has cleared the physical ID so
// a worker that passed an earlier queue check cannot replay its old lifecycle.
func (s Store) CompleteConsumedBranchPublication(name, branchID, manifest, lifecycle string, unplace bool) error {
	if manifest == "" {
		return errors.New("consumed branch publication has no manifest")
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			branch.PublicationID = ""
			branch.StorageState = "shared"
			branch.StorageError = ""
			branch.StorageManifest = manifest
			branch.StorageRetryAt = time.Time{}
			if lifecycle != "" {
				branch.State = lifecycle
			}
			if unplace {
				branch.NodeID = ""
				branch.Unplaced = true
			}
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// AbandonBranchPublication fences an asynchronous worker before a local
// capture is consumed to make the branch active again. The old shared
// manifest remains a valid immutable ancestor, but the active branch is no
// longer represented by the publication being abandoned.
func (s Store) AbandonBranchPublication(name, branchID, publicationID string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.PublicationID != publicationID {
				return false, fmt.Errorf("branch %q publication is %q, cancel has %q", branchID, branch.PublicationID, publicationID)
			}
			branch.PublicationID = ""
			branch.StorageState = "local"
			branch.StorageError = ""
			branch.StorageRetryAt = time.Time{}
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// CompareAndSwapBranchNodeID transfers one process-free branch without
// changing sibling placement.
func (s Store) CompareAndSwapBranchNodeID(name, branchID, expected, replacement string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			if info.Branches[i].ID != branchID {
				continue
			}
			owner := info.Branches[i].NodeID
			if info.Branches[i].Unplaced {
				owner = ""
			}
			// Another writer made the same change: an activation, say,
			// records the owner the authority gave the branch.
			if owner == replacement {
				return false, nil
			}
			if owner != expected {
				return false, fmt.Errorf("branch %s@%s cluster owner changed from %q to %q", info.Name, branchID, expected, owner)
			}
			info.Branches[i].NodeID = replacement
			info.Branches[i].Unplaced = replacement == ""
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// RenameBranch changes a branch's visible name in the durable record.
//
// The name is metadata: the filesystem branch under it, the memory it holds and
// the processes running in it are all identified some other way, and none of
// them moves. What does move is the console state, which is filed under the
// visible name — the log a caller reads, the lock that says a follower is
// running, and the exec consoles namespaced beneath it. Those are renamed
// rather than recreated, so a follower holding one open keeps writing to the
// same file under its new name.
func (s Store) RenameBranch(name, oldID, newID string) error {
	if err := validateObjectName("branch", newID); err != nil {
		return err
	}
	var rollback func() error
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		found := false
		var renamedBranch BranchInfo
		for i := range info.Branches {
			switch info.Branches[i].ID {
			case newID:
				return false, fmt.Errorf("branch %q already exists", newID)
			case oldID:
				found = true
				renamedBranch = info.Branches[i]
			}
		}
		if !found {
			return false, fmt.Errorf("branch %q is not recorded", oldID)
		}
		for i := range info.Branches {
			if info.Branches[i].ID == oldID {
				info.Branches[i].ID = newID
			}
			if info.Branches[i].ParentID == oldID {
				info.Branches[i].ParentID = newID
			}
		}
		for i := range info.PublishedPorts {
			if info.PublishedPorts[i].Branch() == oldID {
				info.PublishedPorts[i].BranchID = newID
			}
		}
		var err error
		rollback, err = s.renameBranchArtifacts(info.ID, renamedBranch, oldID, newID)
		if err != nil {
			return false, err
		}
		recomputeContainerState(info)
		return true, nil
	})
	if err != nil && rollback != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("recording renamed branch: %w; rolling back branch artifacts: %v", err, rollbackErr)
		}
	}
	return err
}

type renamePath struct {
	oldPath string
	newPath string
}

// renameBranchArtifacts moves name-keyed state as one rollback-capable
// transaction. Memory directories use stable filesystem-branch keys and do
// not move when a visible branch is renamed.
func (s Store) renameBranchArtifacts(containerID string, branch BranchInfo, oldID, newID string) (func() error, error) {
	var paths []renamePath
	dir := filepath.Join(s.activeRoot(), "containers", containerID, "console")
	entries, err := readDirWithStaleRetry(dir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if name != oldID && !strings.HasPrefix(name, oldID+".") {
				continue
			}
			renamed := newID + strings.TrimPrefix(name, oldID)
			paths = append(paths, renamePath{filepath.Join(dir, name), filepath.Join(dir, renamed)})
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading console state: %w", err)
	}
	execDir := filepath.Join(dir, "exec", oldID)
	if _, err := os.Stat(execDir); err == nil {
		paths = append(paths, renamePath{execDir, filepath.Join(dir, "exec", newID)})
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading exec console state: %w", err)
	}

	for _, path := range paths {
		if _, err := os.Lstat(path.newPath); err == nil {
			return nil, fmt.Errorf("renaming branch artifact %q: destination %q already exists", path.oldPath, path.newPath)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("checking branch artifact destination %q: %w", path.newPath, err)
		}
	}
	moved := make([]renamePath, 0, len(paths))
	rollback := func() error {
		var result error
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(moved[i].newPath, moved[i].oldPath); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
		return result
	}
	for _, path := range paths {
		if err := os.Rename(path.oldPath, path.newPath); err != nil {
			rollbackErr := rollback()
			if rollbackErr != nil {
				return nil, fmt.Errorf("renaming branch artifact %q: %w; rollback: %v", path.oldPath, err, rollbackErr)
			}
			return nil, fmt.Errorf("renaming branch artifact %q: %w", path.oldPath, err)
		}
		moved = append(moved, path)
	}
	return rollback, nil
}

// RecordBranchStart records a fresh process incarnation for one branch. Family
// state is derived from all branches; starting one branch never deletes or
// rewrites its peers. A publication ID fences one process-free capture, not
// the mutable incarnation created from it. Keep StorageManifest as the
// immutable filesystem ancestor for lazy reads and the next incremental
// publication, but retire the old transaction identity when execution starts.
func (s Store) RecordBranchStart(name, branchID, nodeID string) error {
	return s.RecordBranchActivation(name, branchID, "running", nodeID)
}

// RecordBranchActivation commits a process-bearing incarnation reconstructed
// from a cold branch. state is explicit because a dehydrated branch may have
// been captured paused. This is also the common publication-fence retirement
// boundary for fresh start and rehydration.
func (s Store) RecordBranchActivation(name, branchID, state, nodeID string) error {
	if state != "running" && state != "paused" {
		return fmt.Errorf("invalid active branch state %q", state)
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		now := time.Now().UTC()
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			updateBranchLifecycle(branch, state, nil, now)
			// Activation is the physical lifecycle commit, so it must also repair
			// the derived owner atomically. A cold publication completion may clear
			// NodeID after the authority has already claimed the branch for this node; merely
			// clearing Unplaced here would otherwise leave a running physical branch
			// with no owner and prevent placement from leaving materializing.
			if nodeID != "" {
				branch.NodeID = nodeID
			}
			branch.Unplaced = false
			branch.PublicationID = ""
			branch.StorageState = "local"
			branch.StorageError = ""
			branch.StorageRetryAt = time.Time{}
			info.StartedAt = &now
			info.ExitStatus = nil
			info.ExitedAt = nil
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// StopBranch explicitly discards one branch's execution state while retaining
// its filesystem, logs, identity, and peer branches.
func (s Store) StopBranch(name, branchID string, exitStatus int) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		now := time.Now().UTC()
		found := false
		alreadyExited := false
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			alreadyExited = branch.State == "exited" || branch.State == "stopped"
			if !alreadyExited || branch.ExitStatus == nil {
				updateBranchLifecycle(branch, "exited", &exitStatus, now)
			}
			// Stopping a resident dehydration discards its capture, which
			// was all the branch had that its filesystem did not.
			if branch.StorageState == StorageResident {
				branch.StorageState = "local"
			}
			// Stop consumes this process snapshot. Retire its publication fence in
			// the same metadata transaction so a worker that already passed its
			// queue cancellation check cannot replay the preceding lifecycle after
			// execution state and memory roots have been discarded. The immutable
			// StorageManifest remains the branch's filesystem ancestor.
			branch.PublicationID = ""
			found = true
			break
		}
		if !found {
			return false, fmt.Errorf("branch %q is not recorded", branchID)
		}
		recomputeContainerState(info)
		if info.State == "exited" {
			if !alreadyExited || info.ExitStatus == nil {
				info.ExitStatus = &exitStatus
			}
			info.ExitedAt = &now
		}
		return true, nil
	})
}

// RecordBranchExit records one branch's termination without treating it as a
// family-wide exit. exitStatus is retained on the family only when no other
// surviving branch remains runnable or recoverable.
func (s Store) RecordBranchExit(name, branchID string, exitStatus int) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		now := time.Now().UTC()
		found := false
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			found = true
			if branch.State == "dehydrated" {
				return false, nil
			}
			updateBranchLifecycle(branch, "exited", &exitStatus, now)
			break
		}
		if !found {
			return false, fmt.Errorf("branch %q is not recorded", branchID)
		}
		recomputeContainerState(info)
		if info.State == "exited" {
			info.ExitStatus = &exitStatus
			info.ExitedAt = &now
		}
		return true, nil
	})
}

// KilledExitStatus is the exit status of a branch killed with its runtime:
// SIGKILL's, which Docker reports for a container killed outright or by the
// out-of-memory killer.
const KilledExitStatus = 128 + 9

// RuntimeExitedUnexpectedly is the error recorded for a family whose runtime
// died under main, unless the out-of-memory killer took it, which OOMKilled
// reports instead, as Docker does.
const RuntimeExitedUnexpectedly = "the family's runtime exited unexpectedly"

// RuntimeLoss is what became of the branches of a runtime found gone.
type RuntimeLoss struct {
	// ExitStatuses has the exit status of each branch whose init exited
	// before the runtime did, as its console follower recorded it. Every
	// other branch that needed the runtime was killed with it.
	ExitStatuses map[string]int
	// OOMKilled reports that the out-of-memory killer of the family's cgroup
	// killed the runtime.
	OOMKilled bool
}

// ReconcileRuntimeUnavailable marks only branches that required the missing
// Sentry as exited. Durable dehydrated branches remain recoverable.
func (s Store) ReconcileRuntimeUnavailable(name string, loss RuntimeLoss) error {
	return s.ReconcileNodeRuntimeUnavailable(name, "", loss)
}

// ReconcileNodeRuntimeUnavailable marks branches whose runtime was lost on
// nodeID as exited: with the status their init exited with, if it had, and
// otherwise as killed with the runtime. A family whose main was killed records
// why, in OOMKilled or ExitError. An empty nodeID selects all branches
// for standalone TTC. Elastic families may have live siblings on other nodes,
// which this operation must never rewrite merely because they are absent from
// the local runsc root.
func (s Store) ReconcileNodeRuntimeUnavailable(name, nodeID string, loss RuntimeLoss) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		changed, mainKilled := false, false
		now := time.Now().UTC()
		exit := func(branch *BranchInfo) {
			status, exited := loss.ExitStatuses[branch.ID]
			if !exited {
				status = KilledExitStatus
				mainKilled = mainKilled || branch.ID == "main"
			}
			updateBranchLifecycle(branch, "exited", &status, now)
			changed = true
		}
		for i := range info.Branches {
			branch := &info.Branches[i]
			if nodeID != "" && info.BranchNodeID(branch.ID) != nodeID {
				continue
			}
			switch branch.State {
			case "creating", "running", "paused", "rehydrating":
				exit(branch)
			case "dehydrated":
				// A resident capture was in the runtime that is gone. Like a
				// running branch, it restarts from its filesystem.
				if branch.StorageState == StorageResident {
					exit(branch)
					branch.StorageState = "local"
				}
			case "dehydrating":
				// A successful dehydration deliberately removes the final
				// Sentry before its asynchronous publication commits. The
				// durable local capture and queue intent, rather than a live
				// runtime, are authoritative throughout this state.
			}
		}
		oldState := info.State
		recomputeContainerState(info)
		if oldState != info.State {
			changed = true
		}
		// The family's exit is main's, as a container's is its init's.
		if info.State == "exited" && changed {
			for _, branch := range info.Branches {
				if branch.ID == "main" && branch.ExitStatus != nil {
					status := *branch.ExitStatus
					info.ExitStatus = &status
				}
			}
			info.ExitedAt = &now
			info.OOMKilled = mainKilled && loss.OOMKilled
			if mainKilled && !loss.OOMKilled {
				info.ExitError = RuntimeExitedUnexpectedly
			}
		}
		return changed, nil
	})
}

// RemoveBranchRecord removes a branch from persistent metadata and splices its
// direct children to its parent. No tombstone is retained, so branch IDs are
// reusable and metadata size tracks live branches rather than branch history.
func (s Store) RemoveBranchRecord(name, branchID string) error {
	if err := validateObjectName("branch", branchID); err != nil {
		return err
	}
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		index := -1
		parent := ""
		for i := range info.Branches {
			if info.Branches[i].ID == branchID {
				index = i
				parent = info.Branches[i].ParentID
				break
			}
		}
		if index < 0 {
			return false, fmt.Errorf("branch %q is not recorded", branchID)
		}
		for i := range info.Branches {
			if info.Branches[i].ParentID == branchID {
				info.Branches[i].ParentID = parent
			}
		}
		info.Branches = append(info.Branches[:index], info.Branches[index+1:]...)
		if len(info.Branches) == 0 {
			if info.IsDistributedPhysicalRuntime() {
				return false, errors.New("cannot retain an empty physical container record")
			}
			info.DistributedCatalogOnly = true
		}
		recomputeContainerState(info)
		bindings := info.PublishedPorts[:0]
		for _, binding := range info.PublishedPorts {
			if binding.Branch() != branchID {
				bindings = append(bindings, binding)
			}
		}
		info.PublishedPorts = bindings
		return true, nil
	})
}

// RemoveBranchFilesystems drops one branch pointer from every TTC-managed
// repository and reclaims filesystem state unreachable from surviving branch
// pointers. A named volume's canonical main pointer belongs to the volume,
// whereas private rootfs and copied-host repositories belong to the family.
func (s Store) RemoveBranchFilesystems(name string, branch BranchInfo) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	spec, err := s.LoadSpec(info.ID)
	if err != nil {
		return err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		return nil
	}
	persistentBranch := branch.FilesystemBranch
	if persistentBranch == "" {
		if branch.ID == "main" {
			persistentBranch = ttcvolume.MainBranch
		} else {
			persistentBranch = ttcvolume.ScopedBranchID(info.ID, branch.ID)
		}
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(repositories))
	physicalDistributedRuntime := info.IsDistributedPhysicalRuntime()
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		repositoryBranch := persistentBranch
		if persistentBranch == ttcvolume.MainBranch && config.Branch != "" {
			repositoryBranch = config.Branch
		}
		if repositoryBranch == ttcvolume.MainBranch && !repositoryCanonicalBranchBelongsToFamily(config) {
			// The canonical branch is named-volume state, not family-private
			// state. Descendants and reused visible "main" branches are scoped
			// and therefore take the normal removal path.
			continue
		}
		// The runtime removes a branch and collects what only it held before
		// the daemon cleans up after it, unless the branch was the last. Opening
		// the repository loads its whole graph, 6 s for one of 100,000 files,
		// where its branch pointers tell what is left to do.
		ownedWhole := repositoryCanonicalBranchBelongsToFamily(config) || physicalDistributedRuntime
		if branches, err := manager.PrivateBranches(path); err == nil {
			removed := !slices.Contains(branches, repositoryBranch)
			if removed && len(branches) != 0 {
				// Collection ran with the removal, or a later pass makes it: a
				// daemon's collection waits while a runtime has the repository.
				continue
			}
			if ownedWhole && (removed || len(branches) == 1) {
				// The repository's last branch goes, and the repository with it.
				if err := manager.RemovePrivate(path); err != nil {
					return fmt.Errorf("removing final private repository %q: %w", config.Source, err)
				}
				continue
			}
		}
		repository, err := manager.OpenPrivate(path)
		if err != nil {
			// Runtime eviction and active-tier reclamation may win the race with
			// branch metadata cleanup. An absent node-local repository is already
			// in the desired terminal state; treating it as fatal strands the
			// branch's independent memory graph and transaction record.
			if errors.Is(err, fs.ErrNotExist) {
				if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
					continue
				} else if statErr != nil {
					return fmt.Errorf("stating repository %q: %w", config.Source, statErr)
				}
			}
			return fmt.Errorf("opening repository %q: %w", config.Source, err)
		}
		removeErr := repository.RemoveBranch(repositoryBranch)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		// A transaction-scoped distributed container is a node-local physical
		// materialization. Even a named volume repository belongs to that
		// materialization once its last branch pointer is gone; the logical
		// volume survives in the source/target authority, not in this obsolete
		// node-local repository. Ordinary family removal must still preserve a
		// named volume's canonical main pointer.
		removeRepository := errors.Is(removeErr, ttcvolume.ErrFinalBranch) && ownedWhole
		if removeRepository {
			removeErr = nil
		}
		if removeErr == nil {
			if !removeRepository {
				removeErr = repository.GarbageCollect()
				if errors.Is(removeErr, ttcvolume.ErrEmptyRepository) && ownedWhole {
					// The Sentry may have committed its own branch-pointer
					// deletion before daemon-side artifact cleanup begins. An
					// empty family-private repository is therefore the same
					// final-repository case as ErrFinalBranch, not a failure.
					removeRepository = true
					removeErr = nil
				}
			}
		}
		closeErr := repository.Close()
		if removeErr != nil {
			return fmt.Errorf("removing branch %q from repository %q: %w", repositoryBranch, config.Source, removeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing repository %q: %w", config.Source, closeErr)
		}
		if removeRepository {
			if err := manager.RemovePrivate(path); err != nil {
				return fmt.Errorf("removing final private repository %q: %w", config.Source, err)
			}
		}
	}
	return nil
}

// GarbageCollectRepositories reclaims unreachable metadata and data objects
// from every named and family-private repository in the data root. Repository
// GC shares the mutation flock with Sentry, so this is safe while containers
// are running.
// collectedRepositories holds, for each repository path, the version of its
// graph when GarbageCollectRepositories last collected it: one unchanged
// since has nothing new to collect. Opening each repository every five
// minutes read its whole graph, and kept it in the process's cache of graph
// states, 129 MB for a dehydrated branch of 100,000 files.
var collectedRepositories sync.Map

func (s Store) GarbageCollectRepositories() (int, error) {
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return 0, err
	}
	paths, err := s.repositoryPaths(manager)
	if err != nil {
		return 0, err
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	collected := 0
	for _, path := range ordered {
		graphDir := filepath.Join(path, "graph")
		if version, err := ttcsnapshot.GraphVersionAt(graphDir); err == nil {
			if last, ok := collectedRepositories.Load(path); ok && last.(ttcsnapshot.GraphVersion) == version {
				continue
			}
		}
		repository, err := manager.OpenPrivate(path)
		if err != nil {
			// Cluster metadata describes the family on every node, while private
			// rootfs and copied-host repositories exist only on nodes that have
			// materialized the branch. A non-owner must not report routine GC
			// failures for another node's absent active repository.
			if errors.Is(err, fs.ErrNotExist) {
				if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
					continue
				} else if statErr != nil {
					return collected, fmt.Errorf("stating repository %q: %w", path, statErr)
				}
			}
			return collected, fmt.Errorf("opening repository %q: %w", path, err)
		}
		ran, gcErr := repository.GarbageCollectIdle()
		closeErr := repository.Close()
		if (gcErr != nil || closeErr != nil) && repositoryRemoved(path) {
			continue
		}
		if gcErr != nil {
			return collected, fmt.Errorf("collecting repository %q: %w", path, gcErr)
		}
		if closeErr != nil {
			return collected, fmt.Errorf("closing repository %q: %w", path, closeErr)
		}
		if ran {
			if version, err := ttcsnapshot.GraphVersionAt(graphDir); err == nil {
				collectedRepositories.Store(path, version)
			}
		}
		collected++
	}
	return collected, nil
}

// afterOriginInventoryOpen, if set, is called by FilesystemSnapshotOrigins
// with each repository it has opened, for tests.
var afterOriginInventoryOpen func(path string)

// repositoryRemoved reports whether the repository at path, which an
// inventory opened and failed to close or collect, has been removed since:
// the cleanup of a removed family's state on a node removes its repositories
// without the lock of a command, and so while another command's inventory
// has one open. Such a repository is gone, as a later inventory finds it.
func repositoryRemoved(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// FilesystemSnapshotOrigins returns the origin IDs that still have local
// authoritative graph metadata in any named or container repository. Snapshot
// IDs are globally unique, so the union is sufficient to reconcile the authority's
// coarse provider directory without putting per-file state in the authority.
func (s Store) FilesystemSnapshotOrigins() (map[ttcsnapshot.SnapshotID]struct{}, error) {
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return nil, err
	}
	paths, err := s.originRepositoryPaths(manager)
	if err != nil {
		return nil, err
	}
	origins := make(map[ttcsnapshot.SnapshotID]struct{})
	for path := range paths {
		repository, err := manager.OpenPrivate(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("opening repository %q for origin inventory: %w", path, err)
		}
		if afterOriginInventoryOpen != nil {
			afterOriginInventoryOpen(path)
		}
		for _, origin := range repository.SnapshotOrigins() {
			origins[origin] = struct{}{}
		}
		// A repository removed since it was opened holds nothing now; that
		// its origins count as held this once only puts off abandoning them.
		if err := repository.Close(); err != nil && !repositoryRemoved(path) {
			return nil, fmt.Errorf("closing repository %q after origin inventory: %w", path, err)
		}
	}
	return origins, nil
}

// FrozenFilesystemOrigins returns, in order, the filesystem origins whose
// reference handoffs are frozen in any named or container repository.
func (s Store) FrozenFilesystemOrigins() ([]ttcsnapshot.SnapshotID, error) {
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return nil, err
	}
	paths, err := s.repositoryPaths(manager)
	if err != nil {
		return nil, err
	}
	var origins []ttcsnapshot.SnapshotID
	for path := range paths {
		repository, err := manager.OpenPrivate(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("opening repository %q for frozen origin inventory: %w", path, err)
		}
		origins = append(origins, repository.FrozenOriginHandoffs()...)
		if err := repository.Close(); err != nil && !repositoryRemoved(path) {
			return nil, fmt.Errorf("closing repository %q after frozen origin inventory: %w", path, err)
		}
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	return origins, nil
}

// PrepareFilesystemRetirementOrigins converts all surviving node-local
// filesystem generations into immutable publication roots. Runtime fencing is
// a precondition enforced by the node-retirement state machine; keeping the
// repository traversal here ensures named volumes, rootfs uppers, copied
// mounts, and transaction-private repositories use one inventory.
func (s Store) PrepareFilesystemRetirementOrigins() ([]ttcsnapshot.SnapshotID, error) {
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return nil, err
	}
	paths, err := s.repositoryPaths(manager)
	if err != nil {
		return nil, err
	}
	orderedPaths := make([]string, 0, len(paths))
	for path := range paths {
		orderedPaths = append(orderedPaths, path)
	}
	sort.Strings(orderedPaths)
	seen := make(map[ttcsnapshot.SnapshotID]struct{})
	var origins []ttcsnapshot.SnapshotID
	for _, path := range orderedPaths {
		repository, err := manager.OpenPrivate(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("opening repository %q for retirement: %w", path, err)
		}
		prepared, prepareErr := repository.PrepareRetirementPublication()
		closeErr := repository.Close()
		if prepareErr != nil || closeErr != nil {
			return nil, errors.Join(prepareErr, closeErr)
		}
		for _, origin := range prepared {
			if _, duplicate := seen[origin]; duplicate {
				continue
			}
			seen[origin] = struct{}{}
			origins = append(origins, origin)
		}
	}
	return origins, nil
}

// WithFilesystemSnapshotOrigin opens the unique node-local filesystem
// repository that contains origin and invokes visit while that repository is
// open. Snapshot origins are allocated cluster-wide, so finding the same
// origin in more than one repository indicates corrupt local authority rather
// than an ambiguous lookup.
//
// The callback form deliberately keeps repository discovery in Store. Callers
// should not need to duplicate its inventory of named volumes, rootfs uppers,
// copied mounts, and transaction-private repositories merely to complete an
// origin-level operation such as provider handoff.
func (s Store) WithFilesystemSnapshotOrigin(origin ttcsnapshot.SnapshotID, visit func(*ttcvolume.Repository) error) (bool, error) {
	if origin == 0 {
		return false, errors.New("filesystem snapshot origin is zero")
	}
	if visit == nil {
		return false, errors.New("filesystem snapshot origin visitor is nil")
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot())
	if err != nil {
		return false, err
	}
	paths, err := s.repositoryPaths(manager)
	if err != nil {
		return false, err
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	found := false
	for _, path := range ordered {
		repository, err := manager.OpenPrivate(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return found, fmt.Errorf("opening repository %q for origin lookup: %w", path, err)
		}
		contains := false
		for _, candidate := range repository.SnapshotOrigins() {
			if candidate == origin {
				contains = true
				break
			}
		}
		if !contains {
			if err := repository.Close(); err != nil && !repositoryRemoved(path) {
				return found, fmt.Errorf("closing repository %q after origin lookup: %w", path, err)
			}
			continue
		}
		if found {
			_ = repository.Close()
			return true, fmt.Errorf("filesystem snapshot origin %d appears in multiple repositories", origin)
		}
		found = true
		visitErr := visit(repository)
		closeErr := repository.Close()
		if visitErr != nil || closeErr != nil {
			return true, errors.Join(visitErr, closeErr)
		}
	}
	return found, nil
}

func (s Store) repositoryPaths(manager *ttcvolume.Manager) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	volumes, err := manager.List()
	if err != nil {
		return nil, err
	}
	for _, volume := range volumes {
		paths[filepath.Join(s.activeRoot(), "filesystems", "volumes", volume.ID)] = struct{}{}
	}
	containers, err := s.ContainersWithActiveState()
	if err != nil {
		return nil, err
	}
	if err := s.addContainerRepositoryPaths(paths, containers); err != nil {
		return nil, err
	}
	return paths, nil
}

// canonicalRepositories keeps, for each data root and container ID, the
// repositories the container's canonical spec names, which are fixed when it
// is made: a cluster node took an inventory of its repositories' snapshot
// origins twice after each removal of a branch, and read each of its
// families' records and specs from the shared root for it, 12 ms a family
// on the simulated NFS mount.
var canonicalRepositories sync.Map

// originRepositoryPaths is repositoryPaths for an inventory of the snapshot
// origins repositories hold, which reads the record and spec of a container
// it has listed before no more: where the node-local state of a container
// removed since is still there, it lists that container's repositories too,
// whose origins are then live a while longer, until the state is reclaimed.
func (s Store) originRepositoryPaths(manager *ttcvolume.Manager) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	volumes, err := manager.List()
	if err != nil {
		return nil, err
	}
	for _, volume := range volumes {
		paths[filepath.Join(s.activeRoot(), "filesystems", "volumes", volume.ID)] = struct{}{}
	}
	entries, err := os.ReadDir(filepath.Join(s.activeRoot(), "containers"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing active container state: %w", err)
	}
	prefix := s.Root + "\x00" + s.activeRoot() + "\x00"
	listed := make(map[string]struct{}, len(entries))
	var unknown []fs.DirEntry
	addRepositories := func(id string, canonical []specutils.TTCVolumeRepository) error {
		mappings, err := s.activeRepositoryMappings(id)
		if err != nil {
			return err
		}
		repositories := slices.Clone(canonical)
		s.activeRepositories(id, repositories, mappings)
		for _, repository := range repositories {
			paths[filepath.Clean(repository.Path)] = struct{}{}
		}
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		listed[prefix+entry.Name()] = struct{}{}
		if kept, ok := canonicalRepositories.Load(prefix + entry.Name()); ok {
			if err := addRepositories(entry.Name(), kept.([]specutils.TTCVolumeRepository)); err != nil {
				return nil, err
			}
			continue
		}
		unknown = append(unknown, entry)
	}
	canonicalRepositories.Range(func(key, _ any) bool {
		if name := key.(string); strings.HasPrefix(name, prefix) {
			if _, ok := listed[name]; !ok {
				canonicalRepositories.Delete(name)
			}
		}
		return true
	})
	loaded := s.loadContainerKeys(unknown)
	infos := make([]ContainerInfo, 0, len(unknown))
	for i, entry := range unknown {
		info, err := loaded[i].info, loaded[i].err
		if err != nil {
			containerDir := filepath.Join(s.Root, "containers", entry.Name())
			if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
				continue
			}
			if sharedPathGone(containerDir) {
				continue
			}
			// Published since it was read: see ContainersWithActiveState.
			if info, err = s.loadContainerKey(entry.Name()); err != nil {
				return nil, err
			}
		}
		infos = append(infos, info)
	}
	// Their specs too are read several at once: one after another, after
	// families were made, they were round trips of the shared root in turn.
	configs := readContainerSpecs(infos)
	for i, info := range infos {
		data, err := configs[i].data, configs[i].err
		if err != nil {
			// Removed since it was listed: see addContainerRepositoryPaths.
			if sharedPathGone(filepath.Join(s.Root, "containers", info.ID)) {
				continue
			}
			if _, statErr := os.Lstat(s.activeContainerDir(info.ID)); errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("reading OCI config for container %q: %w", info.ID, err)
		}
		var spec specs.Spec
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, fmt.Errorf("decoding OCI config for container %q: %w", info.ID, err)
		}
		canonical, err := specutils.TTCVolumeRepositories(&spec)
		if err != nil {
			return nil, err
		}
		canonicalRepositories.Store(prefix+info.ID, canonical)
		if err := addRepositories(info.ID, canonical); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// ContainersWithActiveState returns the containers whose state may be in the
// active root, as ListContainersForGC does: all of them, or where the active
// root is a node's own, those with state in it. A container's repositories,
// but for named volumes, its image root, and its runtime on the node are in
// its state there.
func (s Store) ContainersWithActiveState() ([]ContainerInfo, error) {
	if s.activeRoot() == s.Root {
		return s.listContainers(true)
	}
	entries, err := os.ReadDir(filepath.Join(s.activeRoot(), "containers"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing active container state: %w", err)
	}
	loaded := s.loadContainerKeys(entries)
	var containers []ContainerInfo
	for i, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := loaded[i].info, loaded[i].err
		if err != nil {
			containerDir := filepath.Join(s.Root, "containers", entry.Name())
			if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
				continue
			}
			if sharedPathGone(containerDir) {
				continue
			}
			// Published since it was read: a record is published whole,
			// by renaming its directory into place, or by removing its
			// marker once it is complete.
			if info, err = s.loadContainerKey(entry.Name()); err != nil {
				return nil, err
			}
		}
		containers = append(containers, info)
	}
	return containers, nil
}

// addContainerRepositoryPaths adds to paths the repositories of the listed
// containers.
func (s Store) addContainerRepositoryPaths(paths map[string]struct{}, containers []ContainerInfo) error {
	for _, container := range containers {
		spec, err := s.loadSpecOf(container, container.ID)
		if err != nil {
			// Removed since it was listed, as a migrated or forked branch's
			// staging record may be meanwhile, or its node-local state, as a
			// family's is once the node holds none of its branches: removal
			// renames either away before anything else, so nothing it held
			// is reachable from it.
			if sharedPathGone(filepath.Join(s.Root, "containers", container.ID)) {
				continue
			}
			if _, statErr := os.Lstat(s.activeContainerDir(container.ID)); errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return err
		}
		repositories, err := specutils.TTCVolumeRepositories(spec)
		if err != nil {
			return err
		}
		for _, repository := range repositories {
			paths[filepath.Clean(repository.Path)] = struct{}{}
		}
	}
	return nil
}

// repositoryCanonicalBranchBelongsToFamily reports whether the initial main
// branch is private state owned by one container family. Rootfs repositories
// predate TTCVolumeRepository.Private and are identified by their reserved
// source name; copied-host repositories explicitly carry Private. Named
// volumes are the only repositories whose canonical main branch outlives a
// family.
func repositoryCanonicalBranchBelongsToFamily(config specutils.TTCVolumeRepository) bool {
	return config.Private || config.Source == specutils.TTCRootfsUpperSource
}

// RemoveBranchArtifacts deletes persistent state whose lifetime is exactly one
// visible branch: active/dehydrated memory files, init logs, and managed-exec
// logs.
func (s Store) RemoveBranchArtifacts(name string, branch BranchInfo) error {
	if err := validateObjectName("branch", branch.ID); err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	containerDir := s.activeContainerDir(info.ID)
	// Current memory images are keyed by the stable filesystem identity so a
	// visible branch rename doesn't move a potentially large image.
	memoryKey := MemoryBranchKey(branch)
	for _, kind := range []string{"active", "dehydrated"} {
		if err := os.RemoveAll(filepath.Join(containerDir, "memory", kind, memoryKey)); err != nil {
			return err
		}
	}
	consoleDir := filepath.Join(containerDir, "console")
	base := filepath.Join(consoleDir, branch.ID)
	paths := []string{
		base + ".log", base + ".lock", base + ".stdin.lock", base + ".error.log",
		base + ".wait-status", base + ".log.retention.json", base + ".pid",
	}
	maxFiles := info.ConsoleLogMaxFiles
	if maxFiles < 2 {
		maxFiles = 2
	}
	for generation := 1; generation < maxFiles; generation++ {
		paths = append(paths, ttcconsole.GenerationPath(base+".log", generation))
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(consoleDir, "exec", branch.ID)); err != nil {
		return err
	}
	return nil
}

// RemoveContainer removes persistent configuration for name. Shared image
// roots remain until image garbage collection proves that no container uses
// them.
func (s Store) RemoveContainer(name string) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	return s.removeContainerInfo(info)
}

func (s Store) rename(oldPath, newPath string) error {
	if s.renamePath != nil {
		return s.renamePath(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

func (s Store) removeAll(path string) error {
	if s.removeAllPath != nil {
		return s.removeAllPath(path)
	}
	return os.RemoveAll(path)
}

func (s Store) removedContainersRoot() string {
	return filepath.Join(s.Root, removedContainerDir)
}

func (s Store) discardIncompleteContainer(id string) {
	defer s.removeActiveContainer(id)
	staging := filepath.Join(s.Root, containerStagingDir, id)
	if err := s.removeAll(staging); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warningf("Deferring cleanup of unpublished container %q: %v", id, err)
	}
	// A failed construction normally has no entry in containers/. Preserve the
	// legacy recovery path for roots created by an interrupted older daemon and
	// for a failure after the atomic publication rename.
	if _, err := os.Lstat(filepath.Join(s.Root, "containers", id)); errors.Is(err, fs.ErrNotExist) {
		return
	}
	tombstone, err := s.stageContainerRemoval(id)
	if err != nil {
		log.Warningf("Deferring cleanup of incomplete container %q in the active namespace: %v", id, err)
		return
	}
	if err := s.cleanupRemovedContainer(tombstone, nil); err != nil {
		log.Warningf("Deferring physical cleanup of incomplete container %q: %v", id, err)
	}
}

func (s Store) removeActiveContainer(id string) {
	if s.activeRoot() == s.Root {
		return
	}
	if err := s.RemoveActiveContainerState(id); err != nil {
		log.Warningf("Deferring cleanup of node-local state for container %q: %v", id, err)
	}
}

// RemoveActiveContainerState removes the node-local state of the container
// with this ID. The state leaves the active namespace first, in one rename,
// and is deleted after.
func (s Store) RemoveActiveContainerState(id string) error {
	if err := validateObjectName("container", id); err != nil {
		return err
	}
	root := filepath.Join(s.activeRoot(), removedActiveContainerDir)
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("creating the directory of removed node-local state: %w", err)
	}
	tombstone, err := os.MkdirTemp(root, id+".")
	if err != nil {
		return fmt.Errorf("removing node-local state of container %q: %w", id, err)
	}
	if err := s.rename(s.activeContainerDir(id), filepath.Join(tombstone, "state")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(tombstone)
		return fmt.Errorf("removing node-local state of container %q: %w", id, err)
	}
	if backgroundRemovals.enabled.Load() && s.removeAllPath == nil {
		backgroundRemovals.pending.Add(1)
		go func() {
			defer backgroundRemovals.pending.Done()
			backgroundRemovals.mu.Lock()
			defer backgroundRemovals.mu.Unlock()
			if err := os.RemoveAll(tombstone); err != nil {
				log.Warningf("Deleting the removed node-local state of container %q: %v", id, err)
			}
		}()
		return nil
	}
	return s.removeAll(tombstone)
}

// backgroundRemovals, once enabled, has RemoveActiveContainerState delete the
// state it moves away after it returns, one deletion at a time.
var backgroundRemovals struct {
	enabled atomic.Bool
	pending sync.WaitGroup
	mu      sync.Mutex
}

// RemoveInBackground makes RemoveActiveContainerState, in this process, delete
// the node-local state it moves out of the namespace after it returns. A
// family's node-local state holds its branches' repositories, and deleting
// 200,000 files took 2 s of a 3 s removal. State a process had not deleted
// when it stopped is left for SweepRemovedActiveContainerState.
func RemoveInBackground() {
	backgroundRemovals.enabled.Store(true)
}

// WaitForBackgroundRemovals waits for the deletions RemoveInBackground left to
// run in the background.
func WaitForBackgroundRemovals() {
	backgroundRemovals.pending.Wait()
}

// SweepRemovedActiveContainerState deletes the node-local state that
// removals moved away but had not deleted when their daemon stopped. It must
// not run while a removal may.
func (s Store) SweepRemovedActiveContainerState() error {
	if s.activeRoot() == s.Root {
		return nil
	}
	return s.removeAll(filepath.Join(s.activeRoot(), removedActiveContainerDir))
}

// stageContainerRemoval atomically removes one container from the active
// namespace. Physical cleanup happens only after this rename, so a partially
// successful recursive deletion can damage only a tombstone.
func (s Store) stageContainerRemoval(id string) (string, error) {
	active := filepath.Join(s.Root, "containers", id)
	root := s.removedContainersRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("creating removed-container staging directory: %w", err)
	}
	tombstone := filepath.Join(root, id)
	if _, err := os.Lstat(tombstone); err == nil {
		return "", fmt.Errorf("removed-container tombstone already exists for %q", id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("checking removed-container tombstone for %q: %w", id, err)
	}
	if err := s.rename(active, tombstone); err != nil {
		return "", fmt.Errorf("committing removal of container %q: %w", id, err)
	}
	// The rename is already the logical commit point. Directory sync failures
	// affect crash durability, not whether callers may continue using the
	// active namespace, so retain the tombstone and report them in the daemon
	// log instead of turning a committed removal into an ambiguous failure.
	for _, dir := range []string{filepath.Dir(active), root} {
		directory, err := os.Open(dir)
		if err == nil {
			err = directory.Sync()
			err = errors.Join(err, directory.Close())
		}
		if err != nil {
			log.Warningf("Container %q was removed but syncing directory %q failed: %v", id, dir, err)
		}
	}
	return tombstone, nil
}

func loadRemovedContainerInfo(path string) (*ContainerInfo, error) {
	data, err := readFileWithStaleRetry(filepath.Join(path, "container.json"))
	if err != nil {
		return nil, err
	}
	var info ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// cleanupRemovedContainer performs idempotent post-commit cleanup. Metadata is
// deliberately retained until external resources have been released. Once
// recursive removal begins, losing container.json is harmless: all cleanup
// that needed it has already completed.
func (s Store) cleanupRemovedContainer(path string, known *ContainerInfo) error {
	info := known
	if info == nil {
		loaded, err := loadRemovedContainerInfo(path)
		if errors.Is(err, fs.ErrNotExist) {
			// Older TTC versions could leave an empty active directory after first
			// releasing the image lease. Also make the conventional lease cleanup
			// idempotently in case creation was interrupted before publication.
			id := filepath.Base(path)
			if err := images.DeleteContentLease(context.Background(), images.ContentLeaseOptions{Root: s.Root, LeaseID: images.ContainerLeaseID(id)}); err != nil {
				return fmt.Errorf("releasing inferred image content for removed container %q: %w", id, err)
			}
			return s.removeAll(path)
		}
		if err != nil {
			return fmt.Errorf("reading removed container metadata from %q: %w", path, err)
		}
		info = loaded
	}
	if info.LeaseID != "" {
		if err := images.DeleteContentLease(context.Background(), images.ContentLeaseOptions{Root: s.Root, LeaseID: info.LeaseID}); err != nil {
			return fmt.Errorf("releasing image content for removed container %q: %w", info.ID, err)
		}
	}
	if err := s.removeDistributedMemoryMetadata(*info); err != nil {
		return err
	}
	return s.removeAll(path)
}

// cleanupRemovedContainerAfterCommit absorbs only the short close-to-rmdir
// visibility window exhibited by network filesystems. It does not inspect or
// ignore directory entries: persistent busy/nonempty state remains a normal
// deferred tombstone and cannot block removal of other families.
func (s Store) cleanupRemovedContainerAfterCommit(path string, known *ContainerInfo) error {
	deadline := time.Now().Add(time.Second)
	delay := 10 * time.Millisecond
	for {
		err := s.cleanupRemovedContainer(path, known)
		if err == nil || (!errors.Is(err, unix.EBUSY) && !errors.Is(err, unix.ENOTEMPTY)) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
		}
		// Metadata may already have been unlinked by the preceding RemoveAll.
		// Subsequent attempts therefore use the recovery path rather than a
		// stale in-memory record.
		known = nil
	}
}

func (s Store) removeContainerInfo(info ContainerInfo) error {
	tombstone, err := s.stageContainerRemoval(info.ID)
	if err != nil {
		return err
	}
	if err := s.cleanupRemovedContainerAfterCommit(tombstone, &info); err != nil {
		log.Warningf("Container %q was removed; deferred physical cleanup of %q: %v", info.Name, tombstone, err)
	}
	s.removeActiveContainer(info.ID)
	return nil
}

// RecoverContainerRemovals moves incomplete container directories out
// of the active namespace, then retries every tombstone independently. It is
// intended to run at daemon startup and during periodic maintenance while the
// data-root mutation lock is held.
func (s Store) RecoverContainerRemovals() (int, error) {
	containersRoot := filepath.Join(s.Root, "containers")
	entries, err := readDirWithStaleRetry(containersRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("scanning incomplete container removals: %w", err)
	}
	var result error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		containerDir := filepath.Join(containersRoot, entry.Name())
		metadata := filepath.Join(containerDir, "container.json")
		if _, err := os.Stat(metadata); err == nil {
			marker := filepath.Join(containerDir, containerCreationMarker)
			if _, markerErr := os.Stat(marker); markerErr == nil {
				if _, loadErr := s.loadContainerKey(entry.Name()); loadErr == nil {
					if removeErr := os.Remove(marker); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
						result = errors.Join(result, fmt.Errorf("finalizing container creation %q: %w", entry.Name(), removeErr))
					}
				}
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("checking container metadata %q: %w", metadata, err))
			continue
		}
		if _, err := os.Stat(filepath.Join(containerDir, containerCreationMarker)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("checking container creation marker for %q: %w", entry.Name(), err))
			continue
		}
		if _, err := s.stageContainerRemoval(entry.Name()); err != nil {
			result = errors.Join(result, fmt.Errorf("isolating incomplete container directory %q: %w", entry.Name(), err))
		}
	}
	removed, sweepErr := s.SweepRemovedContainers()
	return removed, errors.Join(result, sweepErr)
}

// SweepRemovedContainers retries tombstones independently. One path that an
// NFS server still considers busy is retained without preventing cleanup of
// any other removed container.
func (s Store) SweepRemovedContainers() (int, error) {
	root := s.removedContainersRoot()
	entries, err := readDirWithStaleRetry(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("listing removed containers: %w", err)
	}
	removed := 0
	var result error
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if err := s.cleanupRemovedContainer(path, nil); err != nil {
			result = errors.Join(result, fmt.Errorf("cleaning removed container %q: %w", entry.Name(), err))
			continue
		}
		removed++
	}
	return removed, result
}

// HasImageRoot reports whether TTC has materialized the immutable root for a
// digest and platform. A corrupt cache entry is reported instead of silently
// looking absent.
func (s Store) HasImageRoot(digest, platform string) (bool, error) {
	if digest == "" || platform == "" {
		return false, nil
	}
	dir := filepath.Join(s.imageRootDir(), imageRootKey(digest, platform))
	if _, err := loadImageRoot(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading cached root for %s (%s): %w", digest, platform, err)
	}
	return true, nil
}

// ImageCacheUsage reports complete immutable regular-file content in a
// clustered node-local image skeleton. The boolean is false for an absent
// cache or for standalone fully-extracted roots, which are authoritative image
// storage rather than a disposable cache.
func (s Store) ImageCacheUsage(digest, platform string) (ttcpack.LazyCacheUsage, bool, error) {
	if s.activeRoot() == s.Root || digest == "" || platform == "" {
		return ttcpack.LazyCacheUsage{}, false, nil
	}
	rootfs := filepath.Join(s.imageRootDir(), imageRootKey(digest, platform), "rootfs")
	usage, err := ttcpack.InspectLazyCache(rootfs)
	if errors.Is(err, os.ErrNotExist) {
		return ttcpack.LazyCacheUsage{}, false, nil
	}
	if err != nil {
		return ttcpack.LazyCacheUsage{}, false, fmt.Errorf("inspecting node-local image cache for %s (%s): %w", digest, platform, err)
	}
	return usage, true, nil
}

// DefaultImageCacheTarget returns the default node-local immutable image
// cache budget. It scales with the local filesystem while remaining bounded
// on very large scratch devices. Active gofers may temporarily keep usage
// above this target; correctness takes precedence over cache reclamation.
func DefaultImageCacheTarget(root string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return 0, err
	}
	capacity := stat.Blocks * uint64(stat.Bsize)
	target := capacity / 4
	const (
		minimum = uint64(512 << 20)
		maximum = uint64(64 << 30)
	)
	if target < minimum && capacity >= minimum {
		target = minimum
	}
	if target > maximum {
		target = maximum
	}
	return target, nil
}

// PruneImageCache evicts oldest complete files from inactive clustered image
// skeletons until their aggregate content is at most targetBytes. Metadata,
// directories, symlinks, and hardlinks remain, and an active gofer pins its
// whole image root with a shared lock so cache deletion cannot affect open or
// running files.
func (s Store) PruneImageCache(targetBytes uint64) (ImageCachePruneResult, error) {
	var result ImageCachePruneResult
	if s.activeRoot() == s.Root {
		return result, nil
	}
	entries, err := readDirWithStaleRetry(s.imageRootDir())
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	type cachedRoot struct {
		path  string
		usage ttcpack.LazyCacheUsage
		mtime time.Time
	}
	var roots []cachedRoot
	for _, entry := range entries {
		if !entry.IsDir() || !imageRootDirectoryPattern.MatchString(entry.Name()) || strings.Contains(entry.Name(), ".tmp-") {
			continue
		}
		path := filepath.Join(s.imageRootDir(), entry.Name(), "rootfs")
		usage, err := ttcpack.InspectLazyCache(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("inspecting image cache %q: %w", entry.Name(), err)
		}
		info, err := entry.Info()
		if err != nil {
			return result, err
		}
		result.BeforeBytes += usage.CompleteBytes
		roots = append(roots, cachedRoot{path: path, usage: usage, mtime: info.ModTime()})
	}
	result.AfterBytes = result.BeforeBytes
	if result.AfterBytes <= targetBytes {
		return result, nil
	}
	sort.Slice(roots, func(i, j int) bool {
		if !roots[i].mtime.Equal(roots[j].mtime) {
			return roots[i].mtime.Before(roots[j].mtime)
		}
		return roots[i].path < roots[j].path
	})
	for _, root := range roots {
		if result.AfterBytes <= targetBytes {
			break
		}
		need := result.AfterBytes - targetBytes
		rootTarget := uint64(0)
		if root.usage.CompleteBytes > need {
			rootTarget = root.usage.CompleteBytes - need
		}
		usage, evicted, err := ttcpack.EvictLazyCache(root.path, rootTarget)
		if errors.Is(err, ttcpack.ErrLazyCacheBusy) {
			result.BusyBytes += root.usage.CompleteBytes
			continue
		}
		if err != nil {
			return result, fmt.Errorf("evicting image cache %q: %w", root.path, err)
		}
		result.AfterBytes -= root.usage.CompleteBytes - usage.CompleteBytes
		result.EvictedFiles += evicted.Files
		result.EvictedBytes += evicted.Bytes
	}
	return result, nil
}

var imageRootDirectoryPattern = regexp.MustCompile(`^` + imageRootFormatVersion + `-sha256-[0-9a-f]{64}-.+$`)

// nodeLocalImageCacheGrace closes the short interval between publishing a
// freshly materialized image root and publishing the active container that
// pins it. Image-root construction and pruning share a lock, but the container
// record is deliberately created outside that lock. A young unreferenced cache
// is retained for one maintenance interval and reconsidered later.
const nodeLocalImageCacheGrace = time.Minute

// PruneImageRoots removes current-format materialized image roots that are no
// longer usable by either the image catalog or a persistent container.
func (s Store) PruneImageRoots(ctx context.Context) (int, error) {
	if s.activeRoot() != s.Root {
		// A node of a cluster keeps an image's root while a container with
		// state on the node was made of the image, and for a grace period
		// after; not because the shared store has the image. It lists no
		// image, which took the store's lock and a stat of each image's
		// content, and reads a container's record only the first time.
		containers, err := s.activeContainerImages()
		if err != nil {
			return 0, fmt.Errorf("listing containers before pruning image roots: %w", err)
		}
		return s.pruneImageRoots(nil, containers)
	}
	stored, err := images.List(ctx, images.ListOptions{Root: s.Root})
	if err != nil {
		return 0, fmt.Errorf("listing images before pruning roots: %w", err)
	}
	containers, err := s.ContainersWithActiveState()
	if err != nil {
		return 0, fmt.Errorf("listing containers before pruning image roots: %w", err)
	}
	return s.pruneImageRoots(stored, containers)
}

// containerImages maps a data root, a node's active root and the ID of a
// container with state on the node to the image the container was made of,
// which never changes for an ID: its name, digest and platform.
var containerImages sync.Map

type containerImage struct {
	name, digest, platform string
}

// activeContainerImages returns the containers with state on a cluster's node
// as ContainersWithActiveState does, but with only their IDs, names and
// images: it reads the record of a container only the first time it lists it,
// where pruning the node's image roots after each family's removal read every
// container's, round trips of the shared root in proportion to the families
// on the node.
func (s Store) activeContainerImages() ([]ContainerInfo, error) {
	entries, err := os.ReadDir(filepath.Join(s.activeRoot(), "containers"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing active container state: %w", err)
	}
	prefix := s.Root + "\x00" + s.activeRoot() + "\x00"
	listed := make(map[string]struct{}, len(entries))
	var containers []ContainerInfo
	var unknown []fs.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		listed[prefix+entry.Name()] = struct{}{}
		if kept, ok := containerImages.Load(prefix + entry.Name()); ok {
			image := kept.(containerImage)
			containers = append(containers, ContainerInfo{ID: entry.Name(), Name: image.name, Digest: image.digest, Platform: image.platform})
			continue
		}
		unknown = append(unknown, entry)
	}
	containerImages.Range(func(key, _ any) bool {
		if name := key.(string); strings.HasPrefix(name, prefix) {
			if _, ok := listed[name]; !ok {
				containerImages.Delete(name)
			}
		}
		return true
	})
	loaded := s.loadContainerKeys(unknown)
	for i, entry := range unknown {
		info, err := loaded[i].info, loaded[i].err
		if err != nil {
			// As ContainersWithActiveState passes over it.
			containerDir := filepath.Join(s.Root, "containers", entry.Name())
			if _, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker)); markerErr == nil {
				continue
			}
			if sharedPathGone(containerDir) {
				continue
			}
			if info, err = s.loadContainerKey(entry.Name()); err != nil {
				return nil, err
			}
		}
		containerImages.Store(prefix+entry.Name(), containerImage{name: info.Name, digest: info.Digest, platform: info.Platform})
		containers = append(containers, ContainerInfo{ID: info.ID, Name: info.Name, Digest: info.Digest, Platform: info.Platform})
	}
	return containers, nil
}

func (s Store) pruneImageRoots(stored []images.Info, containers []ContainerInfo) (int, error) {
	keep := make(map[string]struct{}, len(stored)+len(containers))
	if s.activeRoot() == s.Root {
		for _, image := range stored {
			keep[imageRootKey(image.Digest, image.Platform)] = struct{}{}
		}
	}
	imagesDir := s.imageRootDir()
	for _, container := range containers {
		if s.activeRoot() != s.Root {
			if _, err := os.Stat(s.activeContainerDir(container.ID)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return 0, err
			}
		}
		if container.Digest == "" || container.Platform == "" {
			return 0, fmt.Errorf("container %q has no image identity", container.Name)
		}
		keep[imageRootKey(container.Digest, container.Platform)] = struct{}{}
	}

	removed := 0
	err := s.withImageRootLock(func() error {
		entries, err := readDirWithStaleRetry(imagesDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !imageRootDirectoryPattern.MatchString(entry.Name()) {
				continue
			}
			if _, referenced := keep[entry.Name()]; referenced && !strings.Contains(entry.Name(), ".tmp-") {
				continue
			}
			if s.activeRoot() != s.Root {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if time.Since(info.ModTime()) < nodeLocalImageCacheGrace {
					continue
				}
			}
			if err := removeImageRoot(filepath.Join(imagesDir, entry.Name())); err != nil {
				return fmt.Errorf("removing unreferenced image root %q: %w", entry.Name(), err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	return removed, nil
}

// PruneSharedImagePacks removes manifests no longer referenced by an image tag
// or container, then collects packs unreachable from the remaining manifests.
// Publication and collection share an NFS-visible lock, so a completed pack
// cannot be mistaken for garbage in the short interval before its manifest is
// installed. This makes an explicit image removal reclaim storage immediately
// instead of retaining every removed image for a one-hour crash grace.
func (s Store) PruneSharedImagePacks(ctx context.Context) (removedManifests, removedPacks int, retErr error) {
	if s.activeRoot() == s.Root {
		return 0, 0, nil
	}
	err := s.withSharedImagePackLock(func() error {
		var err error
		removedManifests, removedPacks, err = s.pruneSharedImagePacksLocked(ctx)
		return err
	})
	return removedManifests, removedPacks, err
}

func (s Store) pruneSharedImagePacksLocked(ctx context.Context) (int, int, error) {
	stored, err := images.List(ctx, images.ListOptions{Root: s.Root})
	if err != nil {
		return 0, 0, err
	}
	containers, err := s.listContainers(true)
	if err != nil {
		return 0, 0, err
	}
	keepKeys := make(map[string]struct{}, len(stored)+len(containers))
	for _, image := range stored {
		keepKeys[imageRootKey(image.Digest, image.Platform)] = struct{}{}
	}
	for _, container := range containers {
		if container.Digest != "" && container.Platform != "" {
			keepKeys[imageRootKey(container.Digest, container.Platform)] = struct{}{}
		}
	}
	manifestsDir := filepath.Join(s.sharedImagePackRoot(), "manifests")
	entries, err := readDirWithStaleRetry(manifestsDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	removedManifests := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if !imageRootDirectoryPattern.MatchString(key) {
			continue
		}
		if _, ok := keepKeys[key]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(manifestsDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removedManifests, 0, err
		}
		removedManifests++
	}
	remaining, err := readDirWithStaleRetry(manifestsDir)
	if err != nil {
		return removedManifests, 0, err
	}
	keepPacks := make(map[ttcpack.ID]struct{}, len(remaining))
	for _, entry := range remaining {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		manifest, err := loadSharedImagePackManifest(filepath.Join(manifestsDir, entry.Name()))
		if err != nil {
			// Unknown or malformed operator files are reported but never make a
			// valid pack look unreachable.
			return removedManifests, 0, fmt.Errorf("reading retained image pack manifest %q: %w", entry.Name(), err)
		}
		id, _ := ttcpack.ParseID(manifest.Pack)
		keepPacks[id] = struct{}{}
	}
	removedPacks, err := ttcpack.Collect(s.sharedImagePackRoot(), keepPacks, 0)
	return removedManifests, removedPacks, err
}

// RemoveImageRoots removes every platform-specific immutable root for digest.
// Callers must first prove that no persistent container or image alias still
// references the digest.
func (s Store) RemoveImageRoots(digest string) (int, error) {
	if digest == "" || !strings.Contains(digest, ":") {
		return 0, fmt.Errorf("invalid image digest %q", digest)
	}
	imagesDir := s.imageRootDir()
	prefix := safeComponent(imageRootFormatVersion + "-" + digest + "-")
	removed := 0
	err := s.withImageRootLock(func() error {
		entries, err := readDirWithStaleRetry(imagesDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing image roots: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
				continue
			}
			path := filepath.Join(imagesDir, entry.Name())
			if err := removeImageRoot(path); err != nil {
				return fmt.Errorf("removing image root %q: %w", path, err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	return removed, nil
}

// BranchConsolePaths returns persistent console paths for one branch, creating
// their private parent directory if needed.
func (s Store) BranchConsolePaths(container, branch string) (ConsolePaths, error) {
	if err := validateName(container); err != nil {
		return ConsolePaths{}, err
	}
	if err := validateObjectName("branch", branch); err != nil {
		return ConsolePaths{}, err
	}
	// Console sidecars are prepared before a distributed target is published.
	// Exact physical IDs are authority-derived here, so hidden runtime records
	// must be resolvable without exposing them through ordinary user lookup.
	info, err := s.LoadRuntimeContainer(container)
	if err != nil {
		return ConsolePaths{}, err
	}
	return s.BranchConsolePathsIn(info, branch)
}

// BranchConsolePathsIn is BranchConsolePaths of the container whose record,
// info, the caller has: it reads no record.
func (s Store) BranchConsolePathsIn(info ContainerInfo, branch string) (ConsolePaths, error) {
	if err := validateObjectName("branch", branch); err != nil {
		return ConsolePaths{}, err
	}
	paths := s.BranchConsolePathsOf(info.ID, branch)
	if err := os.MkdirAll(paths.FamilyDir, 0700); err != nil {
		return ConsolePaths{}, fmt.Errorf("creating console state for %q: %w", info.ID, err)
	}
	return paths, nil
}

// BranchConsolePathsOf is BranchConsolePaths of the container with the exact
// ID id, for a caller that only reads what is there: it reads no record and
// makes no directory.
func (s Store) BranchConsolePathsOf(id, branch string) ConsolePaths {
	dir := filepath.Join(s.activeRoot(), "containers", id, "console")
	base := filepath.Join(dir, branch)
	return ConsolePaths{
		FamilyDir:      dir,
		Log:            base + ".log",
		Lock:           base + ".lock",
		StdinLock:      base + ".stdin.lock",
		Error:          base + ".error.log",
		WaitStatus:     base + ".wait-status",
		RetentionStats: ttcconsole.RetentionStatsPath(base + ".log"),
		PID:            base + ".pid",
	}
}

// ExecConsolePaths returns persistent console paths for one managed exec
// session, namespaced by branch and exec ID.
func (s Store) ExecConsolePaths(container, branch, execID string) (ConsolePaths, error) {
	if err := validateName(container); err != nil {
		return ConsolePaths{}, err
	}
	if err := validateObjectName("branch", branch); err != nil {
		return ConsolePaths{}, err
	}
	if err := validateObjectName("exec session", execID); err != nil {
		return ConsolePaths{}, err
	}
	info, err := s.LoadRuntimeContainer(container)
	if err != nil {
		return ConsolePaths{}, err
	}
	return s.ExecConsolePathsIn(info, branch, execID)
}

// ExecConsolePathsIn is ExecConsolePaths of the container whose record, info,
// the caller has: it reads no record.
func (s Store) ExecConsolePathsIn(info ContainerInfo, branch, execID string) (ConsolePaths, error) {
	if err := validateObjectName("branch", branch); err != nil {
		return ConsolePaths{}, err
	}
	if err := validateObjectName("exec session", execID); err != nil {
		return ConsolePaths{}, err
	}
	paths := s.ExecConsolePathsOf(info.ID, branch, execID)
	if err := os.MkdirAll(filepath.Dir(paths.Log), 0700); err != nil {
		return ConsolePaths{}, fmt.Errorf("creating exec console state for %q: %w", info.ID, err)
	}
	return paths, nil
}

// ExecConsolePathsOf is ExecConsolePaths of the container with the exact ID
// id, for a caller that only reads what is there: it reads no record and
// makes no directory.
func (s Store) ExecConsolePathsOf(id, branch, execID string) ConsolePaths {
	dir := filepath.Join(s.activeRoot(), "containers", id, "console", "exec", branch)
	base := filepath.Join(dir, execID)
	return ConsolePaths{
		FamilyDir:      filepath.Dir(filepath.Dir(dir)),
		Log:            base + ".log",
		Lock:           base + ".lock",
		StdinLock:      base + ".stdin.lock",
		Error:          base + ".error.log",
		WaitStatus:     base + ".wait-status",
		RetentionStats: ttcconsole.RetentionStatsPath(base + ".log"),
		PID:            base + ".pid",
	}
}

// ListExecConsoleRetentionStats returns non-zero cumulative retention counters by exec ID.
func (s Store) ListExecConsoleRetentionStats(container, branch string) (map[string]ttcconsole.RetentionStats, error) {
	if err := validateName(container); err != nil {
		return nil, err
	}
	if err := validateObjectName("branch", branch); err != nil {
		return nil, err
	}
	info, err := s.LoadContainer(container)
	if err != nil {
		return nil, err
	}
	return s.ListExecConsoleRetentionStatsOf(info.ID, branch)
}

// ListExecConsoleRetentionStatsOf is ListExecConsoleRetentionStats of the
// container with the exact ID id, for a caller that has its record: it reads
// no record.
func (s Store) ListExecConsoleRetentionStatsOf(id, branch string) (map[string]ttcconsole.RetentionStats, error) {
	if err := validateObjectName("branch", branch); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.activeRoot(), "containers", id, "console", "exec", branch)
	entries, err := readDirWithStaleRetry(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing exec console retention for %q: %w", id, err)
	}
	const suffix = ".log.retention.json"
	statsByExec := make(map[string]ttcconsole.RetentionStats)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		execID := strings.TrimSuffix(entry.Name(), suffix)
		if err := validateObjectName("exec session", execID); err != nil {
			continue
		}
		stats, err := ttcconsole.ReadRetentionStats(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading exec console retention for %q: %w", execID, err)
		}
		if stats.EvictedBytes != 0 || stats.EvictedGenerations != 0 {
			statsByExec[execID] = stats
		}
	}
	if len(statsByExec) == 0 {
		return nil, nil
	}
	return statsByExec, nil
}

func normalizePullPolicy(policy string) (string, error) {
	if policy == "" {
		policy = "missing"
	}
	switch policy {
	case "missing", "always", "never":
		return policy, nil
	default:
		return "", fmt.Errorf("unsupported pull policy %q; supported policies are missing, always, and never", policy)
	}
}

// ImageReady reports whether preparing a container from reference under the
// pull policy would find the image, and its root on this node, ready: neither
// pull it nor unpack it.
func (s Store) ImageReady(ctx context.Context, reference, platform, policy string) (bool, error) {
	policy, err := normalizePullPolicy(policy)
	if err != nil || policy == "always" {
		return false, err
	}
	stored, err := images.List(ctx, images.ListOptions{Root: s.Root})
	if err != nil {
		return false, err
	}
	return s.imageReadyAmong(stored, reference, platform)
}

// imageReadyAmong reports whether reference is one of the stored images and
// its root is unpacked on this node.
func (s Store) imageReadyAmong(stored []images.Info, reference, platform string) (bool, error) {
	match, found, err := selectStoredImage(stored, reference, platform)
	if err != nil || !found {
		return false, err
	}
	selectedPlatform := platform
	if selectedPlatform == "" {
		selectedPlatform = match.Platform
	}
	_, err = loadImageRoot(filepath.Join(s.imageRootDir(), imageRootKey(match.Digest, selectedPlatform)))
	return err == nil, nil
}

// PrepareImage pulls reference if the pull policy asks for it and prepares its
// root on this node, as preparing a container from it would.
func (s Store) PrepareImage(ctx context.Context, reference, platform, policy string, progress images.ProgressFunc) error {
	pulled, err := s.resolveImage(ctx, reference, platform, policy, progress)
	if err != nil {
		return err
	}
	_, _, err = s.prepareImageRoot(ctx, pulled, platform, progress)
	return err
}

func (s Store) resolveImage(ctx context.Context, reference, platform, policy string, progress images.ProgressFunc) (images.Info, error) {
	policy, err := normalizePullPolicy(policy)
	if err != nil {
		return images.Info{}, err
	}
	if policy != "always" {
		stored, err := images.List(ctx, images.ListOptions{Root: s.Root})
		if err != nil {
			return images.Info{}, err
		}
		match, found, err := selectStoredImage(stored, reference, platform)
		if err != nil {
			return images.Info{}, err
		}
		if found {
			return match, nil
		}
		if policy == "never" {
			if platform == "" {
				return images.Info{}, fmt.Errorf("image %q is not present locally and pull policy is never", reference)
			}
			return images.Info{}, fmt.Errorf("image %q for platform %s is not present locally and pull policy is never", reference, platform)
		}
	}
	return images.Pull(ctx, images.PullOptions{
		Root:      s.Root,
		Reference: reference,
		Platform:  platform,
		Progress:  progress,
	})
}

func selectStoredImage(stored []images.Info, reference, platform string) (images.Info, bool, error) {
	matches, err := images.Resolve(stored, reference)
	if err != nil {
		if errors.Is(err, images.ErrNotFound) {
			return images.Info{}, false, nil
		}
		return images.Info{}, false, err
	}
	for _, match := range matches {
		if platform == "" || match.Platform == platform {
			return match, true, nil
		}
	}
	return images.Info{}, false, nil
}

func (s Store) prepareImageRoot(ctx context.Context, pulled images.Info, platform string, progress images.ProgressFunc) (info images.RootFSInfo, rootfs string, retErr error) {
	err := s.withImageRootLock(func() error {
		var err error
		info, rootfs, err = s.prepareImageRootLocked(ctx, pulled, platform, progress, nil)
		return err
	})
	return info, rootfs, err
}

func (s Store) prepareImportedImageRoot(ctx context.Context, imported images.ImportedImage, platform string, progress images.ProgressFunc) (info images.RootFSInfo, rootfs string, retErr error) {
	err := s.withImageRootLock(func() error {
		var err error
		info, rootfs, err = s.prepareImageRootLocked(ctx, imported.Info, platform, progress, &imported)
		return err
	})
	return info, rootfs, err
}

func (s Store) prepareImageRootLocked(ctx context.Context, pulled images.Info, platform string, progress images.ProgressFunc, imported *images.ImportedImage) (_ images.RootFSInfo, _ string, retErr error) {
	imagesDir := s.imageRootDir()
	if err := os.MkdirAll(imagesDir, 0700); err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("creating image cache: %w", err)
	}
	selectedPlatform := platform
	if selectedPlatform == "" {
		selectedPlatform = pulled.Platform
	}
	key := imageRootKey(pulled.Digest, selectedPlatform)
	finalDir := filepath.Join(imagesDir, key)
	if err := removeStaleImageRootTemps(imagesDir, key); err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("cleaning stale image roots: %w", err)
	}
	if info, err := loadImageRoot(finalDir); err == nil {
		if s.activeRoot() != s.Root {
			if err := keepImageRootLocked(finalDir); err != nil {
				return images.RootFSInfo{}, "", err
			}
		}
		return info, filepath.Join(finalDir, "rootfs"), nil
	}
	tempDir, err := os.MkdirTemp(imagesDir, key+".tmp-")
	if err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("creating temporary image root: %w", err)
	}
	defer func() {
		if err := removeImageRoot(tempDir); err != nil {
			if retErr == nil {
				retErr = fmt.Errorf("cleaning temporary image root: %w", err)
			} else {
				retErr = fmt.Errorf("%w (also cleaning temporary image root: %v)", retErr, err)
			}
		}
	}()
	rootfs := filepath.Join(tempDir, "rootfs")
	if s.activeRoot() != s.Root {
		if packed, found, err := s.materializeSharedImagePack(key, tempDir); err != nil {
			return images.RootFSInfo{}, "", fmt.Errorf("materializing shared image pack: %w", err)
		} else if found {
			if err := writeJSON(filepath.Join(tempDir, "image.json"), &packed); err != nil {
				return images.RootFSInfo{}, "", fmt.Errorf("writing image-root metadata: %w", err)
			}
			if err := os.Rename(tempDir, finalDir); err != nil {
				if existing, loadErr := loadImageRoot(finalDir); loadErr == nil {
					return existing, filepath.Join(finalDir, "rootfs"), nil
				}
				return images.RootFSInfo{}, "", fmt.Errorf("publishing image root: %w", err)
			}
			return packed, filepath.Join(finalDir, "rootfs"), nil
		}
	}
	rootOptions := images.RootFSOptions{
		StoreRoot: s.Root,
		Reference: pulled.Reference,
		Platform:  platform,
		Root:      rootfs,
		Progress:  progress,
	}
	var info images.RootFSInfo
	if imported == nil {
		info, err = images.MaterializeRootFS(ctx, rootOptions)
	} else {
		info, err = images.MaterializeImportedRootFS(ctx, rootOptions, *imported)
	}
	if err != nil {
		return images.RootFSInfo{}, "", err
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		return images.RootFSInfo{}, "", err
	}
	if s.activeRoot() != s.Root {
		if err := s.publishSharedImagePack(key, tempDir, info); err != nil {
			return images.RootFSInfo{}, "", fmt.Errorf("publishing shared image pack: %w", err)
		}
	}
	if err := writeJSON(filepath.Join(tempDir, "image.json"), &info); err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("writing image-root metadata: %w", err)
	}
	if err := os.Rename(tempDir, finalDir); err != nil {
		if existing, loadErr := loadImageRoot(finalDir); loadErr == nil {
			return existing, filepath.Join(finalDir, "rootfs"), nil
		}
		return images.RootFSInfo{}, "", fmt.Errorf("publishing image root: %w", err)
	}
	return info, filepath.Join(finalDir, "rootfs"), nil
}

const sharedImagePackFormat = 2

type sharedImagePackManifest struct {
	Format int               `json:"format"`
	Pack   string            `json:"pack"`
	Image  images.RootFSInfo `json:"image"`
}

func (s Store) sharedImagePackRoot() string {
	return filepath.Join(s.Root, "image-packs")
}

func (s Store) sharedImagePackManifestPath(key string) string {
	return filepath.Join(s.sharedImagePackRoot(), "manifests", key+".json")
}

func (s Store) publishSharedImagePack(key, imageDir string, info images.RootFSInfo) error {
	return s.withSharedImagePackLock(func() error {
		return s.publishSharedImagePackLocked(key, imageDir, info)
	})
}

func (s Store) publishSharedImagePackLocked(key, imageDir string, info images.RootFSInfo) error {
	root := s.sharedImagePackRoot()
	manifestPath := s.sharedImagePackManifestPath(key)
	if existing, err := loadSharedImagePackManifest(manifestPath); err == nil {
		if existing.Image.ConfigDigest != info.ConfigDigest || existing.Image.Platform != info.Platform {
			return fmt.Errorf("published image pack %q identifies different image contents", key)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	packID := ttcpack.IDForKey("image", key)
	pack, err := ttcpack.WriteDirectory(imageDir, root, packID)
	if err != nil {
		return err
	}
	manifest := sharedImagePackManifest{Format: sharedImagePackFormat, Pack: pack.String(), Image: info}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		return err
	}
	installed, err := installSharedImagePackManifest(manifestPath, manifest)
	if err != nil {
		return err
	}
	if !installed {
		existing, err := loadSharedImagePackManifest(manifestPath)
		if err != nil {
			return fmt.Errorf("reading concurrently published image pack: %w", err)
		}
		if existing.Pack != manifest.Pack || existing.Image.ConfigDigest != info.ConfigDigest || existing.Image.Platform != info.Platform {
			return fmt.Errorf("concurrent image pack publication for %q disagrees with immutable contents", key)
		}
	}
	dir, err := os.Open(filepath.Dir(manifestPath))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s Store) withSharedImagePackLock(fn func() error) error {
	root := s.sharedImagePackRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("creating shared image-pack directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening shared image-pack lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking shared image packs: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn()
}

// installSharedImagePackManifest publishes manifest exactly once. Hard-linking
// a complete temporary file gives POSIX and NFS an atomic create-if-absent;
// unlike rename, it cannot silently replace another node's authoritative
// pointer.
func installSharedImagePackManifest(path string, manifest sharedImagePackManifest) (_ bool, retErr error) {
	data, err := json.MarshalIndent(&manifest, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".image-pack-manifest.tmp-")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return false, err
	}
	if _, err := temporary.Write(data); err != nil {
		return false, err
	}
	if err := temporary.Sync(); err != nil {
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func loadSharedImagePackManifest(path string) (sharedImagePackManifest, error) {
	data, err := readFileWithStaleRetry(path)
	if err != nil {
		return sharedImagePackManifest{}, err
	}
	var manifest sharedImagePackManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return sharedImagePackManifest{}, err
	}
	if manifest.Format != sharedImagePackFormat || manifest.Pack == "" || manifest.Image.ConfigDigest == "" || manifest.Image.Platform == "" {
		return sharedImagePackManifest{}, fmt.Errorf("shared image pack manifest is incomplete or uses unsupported format %d", manifest.Format)
	}
	if _, err := ttcpack.ParseID(manifest.Pack); err != nil {
		return sharedImagePackManifest{}, err
	}
	return manifest, nil
}

func (s Store) materializeSharedImagePack(key, destinationImageDir string) (images.RootFSInfo, bool, error) {
	manifest, err := loadSharedImagePackManifest(s.sharedImagePackManifestPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return images.RootFSInfo{}, false, nil
	}
	if err != nil {
		return images.RootFSInfo{}, false, err
	}
	pack, err := ttcpack.ParseID(manifest.Pack)
	if err != nil {
		return images.RootFSInfo{}, false, err
	}
	if err := ttcpack.MaterializeSkeleton(s.sharedImagePackRoot(), pack, destinationImageDir, []string{ownership.ManifestName}); err != nil {
		return images.RootFSInfo{}, false, err
	}
	rootfs := filepath.Join(destinationImageDir, "rootfs")
	if err := ownership.Validate(ownership.ManifestPath(rootfs)); err != nil {
		return images.RootFSInfo{}, false, fmt.Errorf("materialized ownership manifest is invalid: %w", err)
	}
	if err := ttcpack.WriteLazyDescriptor(rootfs, s.sharedImagePackRoot(), pack, "rootfs"); err != nil {
		return images.RootFSInfo{}, false, fmt.Errorf("writing lazy image descriptor: %w", err)
	}
	return manifest.Image, true, nil
}

func (s Store) withImageRootLock(fn func() error) error {
	imagesDir := s.imageRootDir()
	if err := os.MkdirAll(imagesDir, 0700); err != nil {
		return fmt.Errorf("creating image-root directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(imagesDir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening image-root lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking image roots: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn()
}

// keepImageRoot reports whether the image root dir is unpacked on this node,
// and if it is, keeps it for a container about to run from it: pruning a
// node's image roots spares one no container with state on the node runs from
// only while it is younger than nodeLocalImageCacheGrace, and the container
// has no state on the node until it starts. A start on a node that had
// unpacked the image more than a minute before found its root pruned, and its
// sandbox failed to start.
func (s Store) keepImageRoot(dir string) (bool, error) {
	kept := false
	err := s.withImageRootLock(func() error {
		if _, err := loadImageRoot(dir); err != nil {
			return nil
		}
		kept = true
		return keepImageRootLocked(dir)
	})
	return kept, err
}

// keepImageRootLocked makes the image root dir young again, for keepImageRoot;
// its caller holds withImageRootLock.
func keepImageRootLocked(dir string) error {
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		return fmt.Errorf("keeping image root %q: %w", filepath.Base(dir), err)
	}
	return nil
}

func removeStaleImageRootTemps(imagesDir, key string) error {
	entries, err := readDirWithStaleRetry(imagesDir)
	if err != nil {
		return err
	}
	prefix := key + ".tmp-"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := removeImageRoot(filepath.Join(imagesDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeImageRoot(root string) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode()|0700)
	})
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// EnsureImageRoot materializes the selected immutable image root if it is not
// already cached. It is used by explicit pulls so that a successful `ttc pull`
// leaves the image ready to run.
func (s Store) EnsureImageRoot(ctx context.Context, pulled images.Info, platform string, progress images.ProgressFunc) (images.RootFSInfo, string, error) {
	return s.prepareImageRoot(ctx, pulled, platform, progress)
}

// EnsureImportedImageRoot materializes an archive entry without requiring its
// final tag to be visible in the native image catalog.
func (s Store) EnsureImportedImageRoot(ctx context.Context, imported images.ImportedImage, platform string, progress images.ProgressFunc) (images.RootFSInfo, string, error) {
	return s.prepareImportedImageRoot(ctx, imported, platform, progress)
}

func imageRootKey(digest, platform string) string {
	return safeComponent(imageRootFormatVersion + "-" + digest + "-" + platform)
}

func loadImageRoot(dir string) (images.RootFSInfo, error) {
	data, err := readFileWithStaleRetry(filepath.Join(dir, "image.json"))
	if err != nil {
		return images.RootFSInfo{}, err
	}
	var info images.RootFSInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return images.RootFSInfo{}, err
	}
	rootfs := filepath.Join(dir, "rootfs")
	if stat, err := os.Stat(rootfs); err != nil || !stat.IsDir() {
		return images.RootFSInfo{}, fmt.Errorf("cached rootfs is missing")
	}
	if err := ownership.Validate(ownership.ManifestPath(rootfs)); err != nil {
		return images.RootFSInfo{}, fmt.Errorf("cached rootfs ownership is invalid: %w", err)
	}
	return info, nil
}

func writeJSON(path string, value any) error {
	staged, err := stageJSON(path, value)
	if err != nil {
		return err
	}
	defer staged.discard()
	return staged.publish()
}

// stagedFile is a durable file under a temporary name in the directory of
// path, the name publish gives it.
type stagedFile struct {
	temp, path string
}

// stageJSON writes value as writeJSON does, under a temporary name.
func stageJSON(path string, value any) (*stagedFile, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return nil, err
	}
	staged := &stagedFile{temp: temp.Name(), path: path}
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		staged.discard()
		return nil, err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		staged.discard()
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		staged.discard()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		staged.discard()
		return nil, err
	}
	return staged, nil
}

// rename gives the file its name, not yet durably.
func (f *stagedFile) rename() error {
	if err := os.Rename(f.temp, f.path); err != nil {
		return err
	}
	f.temp = ""
	return nil
}

// publish gives the file its name durably.
func (f *stagedFile) publish() error {
	if err := f.rename(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(f.path))
}

// discard removes the file if it was not given its name.
func (f *stagedFile) discard() {
	if f != nil && f.temp != "" {
		_ = os.Remove(f.temp)
		f.temp = ""
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func validateName(name string) error {
	return validateObjectName("container", name)
}

func validateObjectName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	for i, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (i > 0 && strings.ContainsRune("_.-", r)) {
			continue
		}
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	return nil
}

func safeComponent(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
}
