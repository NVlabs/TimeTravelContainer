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

package bundle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/ttcvolume"
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
)

// promotedRepositoryDir is retained by the legacy compatibility helper and
// its cleanup tests. Production tiered-storage paths never invoke promotion.
const promotedRepositoryDir = "repositories"

// removedContainerDir is outside the active container namespace so that a
// failed physical deletion cannot make container discovery fail. It remains
// under Store.Root to keep the rename that commits a deletion on one
// filesystem, including when Store.Root is an NFS mount.
const removedContainerDir = "removed-containers"

const containerCreationMarker = ".creating"

// Store owns persistent image roots and generated container bundles.
type Store struct {
	Root string
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

func (s Store) activeContainerDir(id string) string {
	return filepath.Join(s.activeRoot(), "containers", id)
}

func (s Store) imageRootDir() string {
	return filepath.Join(s.activeRoot(), "images")
}

var warnedUnreadableContainers sync.Map

// RemoveObsoleteApplicationMemoryFiles removes pathname-backed live memory
// allocators created by TTC versions predating sparse dehydration. Current
// runtimes use anonymous memfds; durable page contents live exclusively in the
// family memory-chunk repository. Unlinking is safe even if an older Sentry
// still has the file open, since its descriptor remains valid until it exits.
func (s Store) RemoveObsoleteApplicationMemoryFiles() (int, error) {
	containersRoot := filepath.Join(s.activeRoot(), "containers")
	entries, err := os.ReadDir(containersRoot)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(containersRoot, entry.Name(), "memory", "application.mem")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("checking obsolete application memory for %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return removed, fmt.Errorf("obsolete application memory path for %s is not a regular file", entry.Name())
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("removing obsolete application memory for %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
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

// PrepareDistributedMigrationRestoreBundle creates a cold target bundle whose
// bootstrap SandboxFamily already uses the migrated branch's visible identity.
// Distributed restore later replaces that placeholder Kernel in place; it
// must not silently fall back to the ordinary "main" bootstrap identity.
func (s Store) PrepareDistributedMigrationRestoreBundle(name, branchID, filesystemBranch string) (string, func(), error) {
	if branchID == "" || filesystemBranch == "" {
		return "", nil, errors.New("distributed migration restore branch or filesystem identity is empty")
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
		repositories[i].Branch = filesystemBranch
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
	PublishedPorts     []PortBinding
	WaiveLiveFork      bool
	LiveForkWaiver     string
	Progress           images.ProgressFunc
	// VolumeCopyMode is the copy policy already verified by ttcd at startup.
	// Empty permits Store-only callers to probe either supported mode.
	VolumeCopyMode ttcvolume.CopyMode
	NodeID         string
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
}

// DistributedForkOptions materializes a hidden target bundle that refers to
// filesystem branches created atomically by the source Sentry. The source and
// target stores must name the same shared TTC repository.
type DistributedForkOptions struct {
	SourceContainer string
	TargetName      string
	TargetBranch    string
	VisibleBranch   string
	TransactionID   string
	NodeID          string
	SourceNode      string
	Migration       bool
}

// BranchInfo is branch display lineage plus its last known lifecycle state.
type BranchInfo struct {
	ID string `json:"id"`
	// NodeID overrides the legacy family-wide owner for elastic placements.
	// Empty inherits ContainerInfo.NodeID.
	NodeID string `json:"node_id,omitempty"`
	// Unplaced distinguishes a durable cold branch from legacy empty NodeID,
	// which inherits the family owner.
	Unplaced bool `json:"unplaced,omitempty"`
	// ParentID is display provenance, not an ownership edge. Removing a branch
	// reparents its direct children to this branch's parent.
	ParentID                     string `json:"parent_id,omitempty"`
	FilesystemBranch             string `json:"filesystem_branch,omitempty"`
	DistributedForkTransactionID string `json:"distributed_fork_transaction_id,omitempty"`
	DistributedSparseRepository  string `json:"distributed_sparse_repository,omitempty"`
	DistributedSparseBranch      string `json:"distributed_sparse_branch,omitempty"`
	DistributedMigration         bool   `json:"distributed_migration,omitempty"`
	State                        string `json:"state"`
	// StorageState describes where the authoritative captured generation is.
	// It is independent of the process lifecycle in State.
	StorageState             string    `json:"storage_state,omitempty"`
	StorageGeneration        uint64    `json:"storage_generation,omitempty"`
	StorageError             string    `json:"storage_error,omitempty"`
	StorageManifest          string    `json:"storage_manifest,omitempty"`
	StorageRetryAt           time.Time `json:"storage_retry_at,omitempty"`
	SourceNode               string    `json:"source_node,omitempty"`
	DependentMemoryBytes     uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64    `json:"dependent_filesystem_bytes,omitempty"`
	ForkTotalNS              int64     `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS         int64     `json:"source_downtime_ns,omitempty"`
	KernelForkNS             int64     `json:"kernel_fork_ns,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
}

// BranchNodeID returns the effective cluster owner of branchID.
func (c ContainerInfo) BranchNodeID(branchID string) string {
	for _, branch := range c.Branches {
		if branch.ID == branchID {
			if branch.Unplaced {
				return ""
			}
			if branch.NodeID != "" {
				return branch.NodeID
			}
			return c.NodeID
		}
	}
	return ""
}

// MemoryBranchKey returns the stable directory key used for a branch's
// dehydration metadata and legacy active page sources. It deliberately uses
// the filesystem branch identity rather than the user-visible branch name so
// renaming a branch never changes where its durable memory is stored.
func MemoryBranchKey(branch BranchInfo) string {
	identity := branch.FilesystemBranch
	if identity == "" {
		identity = branch.ID
	}
	return hex.EncodeToString([]byte(identity))
}

// DistributedForkImport is a reserved but not yet visible branch imported
// into an already-running target family.
type DistributedForkImport struct {
	BranchID                 string    `json:"branch_id"`
	FilesystemBranch         string    `json:"filesystem_branch"`
	State                    string    `json:"state"`
	CreatedAt                time.Time `json:"created_at"`
	SparseRepository         string    `json:"sparse_repository,omitempty"`
	SparseBranch             string    `json:"sparse_branch,omitempty"`
	SourceNode               string    `json:"source_node,omitempty"`
	TargetNode               string    `json:"target_node,omitempty"`
	Migration                bool      `json:"migration,omitempty"`
	DependentMemoryBytes     uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64    `json:"dependent_filesystem_bytes,omitempty"`
}

// PortBinding describes a TCP listener managed by TTC for one live branch.
// An empty BranchID is the legacy representation of the main branch.
type PortBinding struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	BranchID      string `json:"branch_id,omitempty"`
}

// Branch returns the branch targeted by p, normalizing legacy main bindings.
func (p PortBinding) Branch() string {
	if p.BranchID == "" {
		return "main"
	}
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
	NextStorageGeneration        uint64                           `json:"next_storage_generation,omitempty"`
	NetworkMode                  string                           `json:"network_mode,omitempty"`
	LiveForkEligible             bool                             `json:"live_fork_eligible"`
	LiveForkWaiver               string                           `json:"live_fork_waiver,omitempty"`
	NetworkNAT                   bool                             `json:"network_nat,omitempty"`
	NetworkIP                    string                           `json:"network_ip,omitempty"`
	NetworkMAC                   string                           `json:"network_mac,omitempty"`
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
	CreatedAt                    time.Time                        `json:"created_at"`
	StartedAt                    *time.Time                       `json:"started_at,omitempty"`
	ExitedAt                     *time.Time                       `json:"exited_at,omitempty"`
	DistributedForkTransactionID string                           `json:"distributed_fork_transaction_id,omitempty"`
	DistributedForkNodeID        string                           `json:"distributed_fork_node_id,omitempty"`
	DistributedForkHidden        bool                             `json:"distributed_fork_hidden,omitempty"`
	DistributedSparseRepository  string                           `json:"distributed_sparse_repository,omitempty"`
	DistributedSparseBranch      string                           `json:"distributed_sparse_branch,omitempty"`
	DistributedFamilyID          string                           `json:"distributed_family_id,omitempty"`
	DistributedForkImports       map[string]DistributedForkImport `json:"distributed_fork_imports,omitempty"`
	NodeID                       string                           `json:"node_id,omitempty"`
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
	hostResolv, err := os.ReadFile("/etc/resolv.conf")
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
	etc := filepath.Join(rootfs, "etc")
	if info, err := os.Lstat(etc); os.IsNotExist(err) {
		if err := os.Mkdir(etc, 0755); err != nil {
			return fmt.Errorf("creating rootfs /etc mountpoint directory: %w", err)
		}
		manifest.Entries["etc"] = ownership.Owner{}
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
			manifest.Entries[filepath.ToSlash(filepath.Join("etc", base))] = ownership.Owner{}
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
		manifest.Entries[filepath.ToSlash(filepath.Join("etc", base))] = ownership.Owner{}
	}
	if err := ownership.Write(manifestName, manifest); err != nil {
		return fmt.Errorf("updating rootfs ownership for network mountpoints: %w", err)
	}
	return nil
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
		contents, err := os.ReadFile(mount.Source)
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

// ConsolePaths are persistent host-side files for one branch-local console.
// None of these files are donated into the sandbox.
type ConsolePaths struct {
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
	containers, err := s.listContainers(true)
	if err != nil {
		return Prepared{}, err
	}
	for _, existing := range containers {
		if existing.Name == opts.Name || existing.ID == opts.Name {
			return Prepared{}, fmt.Errorf("container name %q is already in use by %s", opts.Name, existing.ID)
		}
	}
	pulled, err := s.resolveImage(ctx, opts.Reference, opts.Platform, opts.PullPolicy, opts.Progress)
	if err != nil {
		return Prepared{}, err
	}
	imageInfo, rootfs, err := s.prepareImageRoot(ctx, pulled, opts.Platform, opts.Progress)
	if err != nil {
		return Prepared{}, err
	}
	opts.ManagedVolumes, err = s.resolveManagedVolumes(imageInfo.Image, rootfs, containers, opts)
	if err != nil {
		return Prepared{}, err
	}
	id, containerDir, err := s.createContainerDir(containers)
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
		manager, err := ttcvolume.OpenManager(s.activeRoot(), opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
		if err != nil {
			return Prepared{}, fmt.Errorf("opening rootfs upper store: %w", err)
		}
		if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
			return Prepared{}, fmt.Errorf("volume backing capability changed while preparing rootfs: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
		}
		rootfsUpper, err := manager.CreatePrivate(filepath.Join(activeContainerDir, rootfsUpperRepositoryDir), "rootfs")
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
	internalMounts, err := materializeNetworkFiles(containerDir, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	opts.HostMounts = append(opts.HostMounts, internalMounts...)
	opts.SpecOptions.RootFS = rootfs
	if hasHardResourceLimits(opts.SpecOptions) {
		opts.SpecOptions.CgroupsPath = filepath.Join("ttc", id)
	}
	spec, err := BuildSpec(imageInfo.Image, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	logAttributes, err := selectLogAttributes(opts.LogOptions, spec)
	if err != nil {
		return Prepared{}, err
	}
	leaseID = images.ContainerLeaseID(id)
	if err := images.CreateContentLease(ctx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: pulled.Digest}); err != nil {
		return Prepared{}, err
	}
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		return Prepared{}, fmt.Errorf("creating OCI bundle: %w", err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		return Prepared{}, fmt.Errorf("writing OCI config: %w", err)
	}
	shmSize, err := effectiveShmSize(opts.ShmSize)
	if err != nil {
		return Prepared{}, err
	}
	metadata := ContainerInfo{
		ID:                  id,
		Name:                opts.Name,
		Reference:           imageInfo.Reference,
		Digest:              pulled.Digest,
		ImageSize:           pulled.Size,
		LeaseID:             leaseID,
		Platform:            imageInfo.Platform,
		BundleDir:           bundleDir,
		RootFS:              rootfs,
		NetworkMode:         networkMode,
		LiveForkEligible:    liveForkEligible,
		LiveForkWaiver:      opts.LiveForkWaiver,
		NetworkNAT:          opts.NetworkNAT,
		NetworkIP:           opts.NetworkIP,
		NetworkMAC:          opts.NetworkMAC,
		PublishedPorts:      append([]PortBinding(nil), opts.PublishedPorts...),
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
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &metadata); err != nil {
		return Prepared{}, fmt.Errorf("writing container metadata: %w", err)
	}
	keep = true
	return Prepared{BundleDir: bundleDir, RootFS: rootfs, Image: imageInfo, Container: metadata}, nil
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
	containers, err := s.listContainers(true)
	if err != nil {
		return Prepared{}, err
	}
	for _, existing := range containers {
		if existing.Name == opts.Name || existing.ID == opts.Name {
			return Prepared{}, fmt.Errorf("container name %q is already in use by %s", opts.Name, existing.ID)
		}
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
	id, containerDir, err := s.createContainerDir(containers)
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
		if manager, err := ttcvolume.OpenManager(s.activeRoot(), true); err == nil {
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
	manager, err := ttcvolume.OpenManager(s.activeRoot(), opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
	if err != nil {
		return Prepared{}, err
	}
	if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
		return Prepared{}, fmt.Errorf("volume backing capability changed during restore: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
	}
	copyMode := manager.Capability().Mode
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
			imported, snapshot, err := manager.ImportPrivateSnapshot(path, "rootfs", snapshotPath)
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
			imported, snapshot, err := manager.ImportPrivateSnapshot(path, fmt.Sprintf("host-%d", index), snapshotPath)
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
		imported, snapshot, err := manager.ImportSnapshot(name, snapshotPath)
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
	delete(spec.Annotations, specutils.AnnotationTTCMemoryFile)
	spec.Annotations["dev.gvisor.spec.rootfs.source"] = rootfs
	if len(repositories) == 0 {
		delete(spec.Annotations, specutils.AnnotationTTCVolumeRepositories)
	} else {
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encodedRepositories)
	}
	if spec.Root == nil {
		return Prepared{}, errors.New("checkpoint OCI spec has no root")
	}
	spec.Root.Path = rootfs
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
	if err := images.CreateContentLease(ctx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: pulled.Digest}); err != nil {
		return Prepared{}, err
	}
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		return Prepared{}, err
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &spec); err != nil {
		return Prepared{}, err
	}

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
	metadata.BundleDir = bundleDir
	metadata.RootFS = rootfs
	metadata.PublishedPorts = nil
	metadata.RootfsUpperID = rootfsUpperID
	metadata.RootfsUpperCopyMode = string(copyMode)
	metadata.State = "created"
	metadata.ExitStatus = nil
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
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &metadata); err != nil {
		return Prepared{}, err
	}
	keep = true
	return Prepared{BundleDir: bundleDir, RootFS: rootfs, Image: imageInfo, Container: metadata}, nil
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
	if existing, err := s.LoadDistributedForkTarget(opts.TransactionID); err == nil {
		if existing.Name != opts.TargetName || existing.DistributedForkNodeID != opts.NodeID {
			return Prepared{}, fmt.Errorf("distributed fork transaction %q is bound to another target", opts.TransactionID)
		}
		return Prepared{BundleDir: existing.BundleDir, RootFS: existing.RootFS, Container: existing}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Prepared{}, err
	}
	source, err := s.LoadContainer(opts.SourceContainer)
	if err != nil {
		return Prepared{}, err
	}
	globalFamilyID := source.DistributedFamilyID
	if globalFamilyID == "" {
		globalFamilyID = source.ID
	}
	containers, err := s.listContainers(true)
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
			if !opts.Migration && current.NodeID != "" && current.NodeID != opts.NodeID {
				return false, fmt.Errorf("container %q belongs to cluster node %q", opts.TargetName, current.NodeID)
			}
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
				if current.BranchNodeID(branch.ID) != opts.SourceNode {
					return false, fmt.Errorf("migration source branch %s@%s belongs to node %q, not %q", current.Name, branch.ID, current.BranchNodeID(branch.ID), opts.SourceNode)
				}
			}
			if opts.Migration && !branchExists {
				return false, fmt.Errorf("migration source branch %s@%s does not exist", current.Name, opts.VisibleBranch)
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
			current.DistributedForkImports[opts.TransactionID] = DistributedForkImport{
				BranchID: opts.VisibleBranch, FilesystemBranch: ttcvolume.ScopedBranchID(source.ID, opts.TargetBranch),
				State: "created", CreatedAt: time.Now().UTC(), SourceNode: opts.SourceNode, TargetNode: opts.NodeID, Migration: opts.Migration,
			}
			existing = *current
			return true, nil
		})
		if err != nil {
			return Prepared{}, err
		}
		return Prepared{BundleDir: existing.BundleDir, RootFS: existing.RootFS, Container: existing}, nil
	}
	if opts.Migration {
		return Prepared{}, fmt.Errorf("migration target family %q is unavailable", opts.TargetName)
	}
	if opts.VisibleBranch != "main" {
		return Prepared{}, fmt.Errorf("new distributed target family %q must start with @main", opts.TargetName)
	}
	spec, err := s.LoadSpec(source.ID)
	if err != nil {
		return Prepared{}, err
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
	id, containerDir, err := s.createContainerDir(containers)
	if err != nil {
		return Prepared{}, err
	}
	activeContainerDir := s.activeContainerDir(id)
	if err := os.MkdirAll(activeContainerDir, 0700); err != nil {
		s.discardIncompleteContainer(id)
		return Prepared{}, fmt.Errorf("creating distributed target active directory: %w", err)
	}
	if err := rehomeInternalMounts(spec, activeContainerDir); err != nil {
		s.discardIncompleteContainer(id)
		return Prepared{}, err
	}
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
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		return Prepared{}, err
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		return Prepared{}, err
	}
	leaseID = images.ContainerLeaseID(id)
	if err := images.CreateContentLease(ctx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID, Digest: source.Digest}); err != nil {
		return Prepared{}, err
	}
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
	target.Name = opts.TargetName
	target.LeaseID = leaseID
	target.BundleDir = bundleDir
	target.PublishedPorts = nil
	target.AutoRemove = false
	target.State = "created"
	target.ExitStatus = nil
	target.CreatedAt = now
	target.StartedAt = nil
	target.ExitedAt = nil
	target.Branches = []BranchInfo{{ID: "main", NodeID: opts.NodeID, FilesystemBranch: childRepositoryBranch, State: "created", StorageState: "local", SourceNode: opts.SourceNode, CreatedAt: now}}
	target.DistributedForkTransactionID = opts.TransactionID
	target.DistributedForkNodeID = opts.NodeID
	target.DistributedForkHidden = true
	// Sparse roots describe this target runtime's own demand-page source. They
	// must never be inherited when the source is itself a distributed child;
	// Prepare will install a new root if this transaction uses sparse handoff.
	target.DistributedSparseRepository = ""
	target.DistributedSparseBranch = ""
	target.DistributedFamilyID = globalFamilyID
	target.DistributedForkImports = nil
	target.NodeID = opts.NodeID
	for i := range target.Volumes {
		target.Volumes[i].Anonymous = false
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &target); err != nil {
		return Prepared{}, err
	}
	keep = true
	return Prepared{BundleDir: bundleDir, RootFS: target.RootFS, Container: target}, nil
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

func (s Store) resolveManagedVolumes(image ocispec.Image, rootfs string, containers []ContainerInfo, opts PrepareOptions) ([]ManagedVolume, error) {
	volumes := append([]ManagedVolume(nil), opts.ManagedVolumes...)
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
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
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
			return nil, fmt.Errorf("image declares invalid volume destination %q", destination)
		}
		if mounted[clean] {
			continue
		}
		volumes = append(volumes, ManagedVolume{Destination: clean, Anonymous: true})
		mounted[clean] = true
	}
	if len(volumes) == 0 {
		return nil, nil
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot(), opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
	if err != nil {
		return nil, err
	}
	if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
		return nil, fmt.Errorf("volume backing capability changed while preparing managed volumes: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
	}
	ownerManifest, err := ownership.Read(ownership.ManifestPath(rootfs))
	if err != nil {
		return nil, fmt.Errorf("loading image ownership for managed volumes: %w", err)
	}
	seenIDs := make(map[string]string)
	for i := range volumes {
		if volumes[i].Name == "" {
			id, err := randomObjectID("anonymous volume", 16)
			if err != nil {
				return nil, err
			}
			volumes[i].Name = "anon-" + id
			volumes[i].Anonymous = true
		}
		repository, err := manager.Open(volumes[i].Name)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			repository, err = manager.Create(volumes[i].Name)
			created = err == nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolving managed volume %q: %w", volumes[i].Name, err)
		}
		if created && !volumes[i].NoCopy {
			if err := seedManagedVolume(repository, rootfs, volumes[i].Destination, ownerManifest); err != nil {
				repository.Close()
				_ = manager.Remove(volumes[i].Name)
				return nil, fmt.Errorf("initializing managed volume %q from image path %q: %w", volumes[i].Name, volumes[i].Destination, err)
			}
		}
		info := repository.Info()
		if err := validateManagedVolumeAttachment(info.ID, volumes[i], containers); err != nil {
			repository.Close()
			return nil, err
		}
		volumes[i].ID = info.ID
		volumes[i].RepositoryPath = repository.HostPath()
		volumes[i].CopyMode = string(manager.Capability().Mode)
		repository.Close()
		if previous, ok := seenIDs[info.ID]; ok {
			return nil, fmt.Errorf("managed volume %q is mounted more than once (%q and %q); aliases are not yet supported", volumes[i].Name, previous, volumes[i].Destination)
		}
		seenIDs[info.ID] = volumes[i].Destination
	}
	return volumes, nil
}

func (s Store) resolveWritableHostMounts(containerDir string, opts *PrepareOptions) error {
	if opts == nil {
		return errors.New("missing host mount options")
	}
	manager, err := ttcvolume.OpenManager(s.activeRoot(), opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
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
		repository, err := manager.CreatePrivate(repositoryPath, fmt.Sprintf("host-%d", index))
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
	cloneErr := unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
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

func (s Store) createContainerDir(existing []ContainerInfo) (string, string, error) {
	unavailable := make(map[string]bool, len(existing))
	for _, info := range existing {
		unavailable[info.Name] = true
	}
	containersDir := filepath.Join(s.Root, "containers")
	if err := os.MkdirAll(containersDir, 0700); err != nil {
		return "", "", fmt.Errorf("creating container state root: %w", err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		id, err := randomObjectID("container", containerIDBytes)
		if err != nil {
			return "", "", err
		}
		if unavailable[id] {
			continue
		}
		dir := filepath.Join(containersDir, id)
		if err := os.Mkdir(dir, 0700); err == nil {
			// Retain the marker through metadata publication. On NFS, another
			// client may observe its unlink before preceding config.json and
			// container.json renames. Startup/maintenance removes it only after
			// loading the complete record through the normal path.
			marker := filepath.Join(dir, containerCreationMarker)
			if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0600); err != nil {
				_ = os.Remove(dir)
				return "", "", fmt.Errorf("marking container %s creation: %w", id, err)
			}
			return id, dir, nil
		} else if !os.IsExist(err) {
			return "", "", fmt.Errorf("creating state for container %s: %w", id, err)
		}
	}
	return "", "", errors.New("could not allocate a unique container ID")
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
// Legacy name-keyed records are returned with ID set to their directory key.
func (s Store) LoadContainer(identifier string) (ContainerInfo, error) {
	if err := validateName(identifier); err != nil {
		return ContainerInfo{}, err
	}
	if info, err := s.loadContainerKey(identifier); err == nil {
		if !info.DistributedForkHidden {
			return info, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ContainerInfo{}, err
	}
	containers, err := s.ListContainers()
	if err != nil {
		return ContainerInfo{}, err
	}
	var names []ContainerInfo
	var ids []ContainerInfo
	for _, info := range containers {
		if info.Name == identifier {
			names = append(names, info)
		}
		if strings.HasPrefix(info.ID, identifier) {
			ids = append(ids, info)
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if len(names) > 1 {
		return ContainerInfo{}, fmt.Errorf("container name %q is ambiguous", identifier)
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	if len(ids) > 1 {
		return ContainerInfo{}, fmt.Errorf("container ID prefix %q is ambiguous", identifier)
	}
	return ContainerInfo{}, fmt.Errorf("container %q does not exist", identifier)
}

// loadRuntimeContainer resolves ordinary containers and an exact hidden
// distributed-target ID. Hidden targets remain absent from all name and prefix
// lookups, but runtime preparation must be able to consume their private bundle
// before the target is atomically published.
func (s Store) loadRuntimeContainer(identifier string) (ContainerInfo, error) {
	info, err := s.LoadContainer(identifier)
	if err == nil {
		return info, nil
	}
	hidden, hiddenErr := s.loadContainerKey(identifier)
	if hiddenErr == nil && hidden.DistributedForkHidden {
		return hidden, nil
	}
	return ContainerInfo{}, err
}

// mutateContainer serializes catalog read-modify-write transactions across
// foreground commands, background publication workers, and clustered daemons
// sharing TTC_ROOT. The lock is deliberately separate from the JSON file so
// atomic replacement of container.json does not replace the lock inode.
func (s Store) mutateContainer(identifier string, mutate func(*ContainerInfo) (bool, error)) error {
	resolved, err := s.loadRuntimeContainer(identifier)
	if err != nil {
		return err
	}
	directory := filepath.Join(s.Root, "containers", resolved.ID)
	lock, err := os.OpenFile(filepath.Join(directory, ".metadata.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	info, err := s.loadContainerKey(resolved.ID)
	if err != nil {
		return err
	}
	changed, err := mutate(&info)
	if err != nil || !changed {
		return err
	}
	return writeJSON(filepath.Join(directory, "container.json"), &info)
}

// LoadDistributedForkTarget resolves a hidden or published target by its
// immutable transaction marker. It is intended only for ttcd reconciliation.
func (s Store) LoadDistributedForkTarget(transactionID string) (ContainerInfo, error) {
	if err := validateObjectName("distributed fork transaction", transactionID); err != nil {
		return ContainerInfo{}, err
	}
	containers, err := s.listContainers(true)
	if err != nil {
		return ContainerInfo{}, err
	}
	var found *ContainerInfo
	for i := range containers {
		matches := containers[i].DistributedForkTransactionID == transactionID
		if _, ok := containers[i].DistributedForkImports[transactionID]; ok {
			matches = true
		}
		for _, branch := range containers[i].Branches {
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
		copy := containers[i]
		found = &copy
	}
	if found == nil {
		return ContainerInfo{}, fmt.Errorf("distributed fork transaction %q target: %w", transactionID, fs.ErrNotExist)
	}
	return *found, nil
}

func (s Store) loadContainerKey(key string) (ContainerInfo, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, "containers", key, "container.json"))
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("reading container %q: %w", key, err)
	}
	var info ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return ContainerInfo{}, fmt.Errorf("decoding container %q: %w", key, err)
	}
	if info.ID == "" {
		info.ID = key
	}
	if info.ID != key || info.Name == "" || info.BundleDir == "" {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata", key)
	}
	if err := validateName(info.Name); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata: %w", key, err)
	}
	if info.State == "" {
		info.State = "created"
	}
	if len(info.Branches) == 0 {
		info.Branches = []BranchInfo{{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: info.State, CreatedAt: info.CreatedAt}}
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
			if branch.ID == "main" {
				branch.FilesystemBranch = ttcvolume.MainBranch
			} else {
				branch.FilesystemBranch = ttcvolume.ScopedBranchID(info.ID, branch.ID)
			}
		}
		if branch.StorageState == "" {
			if branch.State == "dehydrated" {
				branch.StorageState = "shared"
			} else {
				branch.StorageState = "local"
			}
		}
	}
	seenPortEndpoints := make(map[string]struct{}, len(info.PublishedPorts))
	bindings := info.PublishedPorts[:0]
	for i := range info.PublishedPorts {
		binding, err := normalizePortBinding(info.PublishedPorts[i])
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
	// Metadata predating the immutable eligibility property omitted both fields.
	// Existing contained-network families were created under TTC's forkable
	// configuration, while legacy host-network families necessarily remain
	// ineligible.
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		if networkMode == "host" {
			info.LiveForkWaiver = "legacy host network configuration"
		} else {
			info.LiveForkEligible = true
		}
	}
	if info.LiveForkEligible && info.LiveForkWaiver != "" {
		return ContainerInfo{}, fmt.Errorf("container %q is live-fork eligible but records waiver %q", key, info.LiveForkWaiver)
	}
	if info.LiveForkEligible && networkMode == "host" {
		return ContainerInfo{}, fmt.Errorf("container %q is live-fork eligible but uses host networking", key)
	}
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		return ContainerInfo{}, fmt.Errorf("container %q is not live-fork eligible but has no waiver", key)
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
	if err := validateResourceLimits(SpecOptions{PidsLimit: info.PidsLimit, MemoryLimit: info.MemoryLimit, NanoCPUs: info.NanoCPUs, CPUShares: info.CPUShares, CPUPeriod: info.CPUPeriod, CPUQuota: info.CPUQuota}); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid resource limits: %w", key, err)
	}
	if _, err := os.Stat(filepath.Join(info.BundleDir, "config.json")); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q bundle is unavailable: %w", key, err)
	}
	return info, nil
}

// LoadSpec returns the generated OCI runtime spec for a persistent container.
func (s Store) LoadSpec(name string) (*specs.Spec, error) {
	info, err := s.loadRuntimeContainer(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(info.BundleDir, "config.json"))
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
			spec.Root.Path = rootfs
			if spec.Annotations == nil {
				spec.Annotations = make(map[string]string)
			}
			spec.Annotations[ownership.Annotation] = ownership.ManifestPath(rootfs)
		}
		repositories, err := specutils.TTCVolumeRepositories(&spec)
		if err != nil {
			return nil, err
		}
		changed := false
		activeMappings := make(map[string]activeRepositoryMapping)
		if mappingData, err := os.ReadFile(filepath.Join(s.activeContainerDir(info.ID), "repository-map.json")); err == nil {
			if err := json.Unmarshal(mappingData, &activeMappings); err != nil {
				return nil, fmt.Errorf("decoding active repository map: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for i := range repositories {
			if mapping, ok := activeMappings[repositories[i].Source]; ok {
				repositories[i].Path = mapping.Path
				repositories[i].RestoreID = mapping.RestoreID
				if mapping.CopyMode != "" {
					repositories[i].CopyMode = mapping.CopyMode
				}
				changed = true
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
				repositories[i].Path = filepath.Join(s.activeContainerDir(info.ID), rootfsUpperRepositoryDir)
			case strings.HasPrefix(repositories[i].Source, "/run/ttc/volumes/"):
				repositories[i].Path = filepath.Join(s.activeRoot(), "volumes-v2", filepath.Base(repositories[i].Source))
			case repositories[i].Private:
				repositories[i].Path = filepath.Join(s.activeContainerDir(info.ID), hostMountRepositoryDir, filepath.Base(repositories[i].Path))
			default:
				continue
			}
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(repositories)
			if err != nil {
				return nil, err
			}
			if spec.Annotations == nil {
				spec.Annotations = make(map[string]string)
			}
			spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
		}
	}
	return &spec, nil
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
		if _, statErr := os.Stat(want); statErr == nil {
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
	return s.listContainersWithPolicy(false, true)
}

func (s Store) listContainers(includeHidden bool) ([]ContainerInfo, error) {
	return s.listContainersWithPolicy(includeHidden, false)
}

// listContainersWithPolicy separates availability-oriented discovery from
// conservative internal scans. User-facing listing skips and warns about one
// unreadable record so that an isolated damaged directory cannot hide every
// healthy container. Mutations and garbage collection remain strict: they
// must not infer that unreadable state is unreferenced.
func (s Store) listContainersWithPolicy(includeHidden, tolerateUnreadable bool) ([]ContainerInfo, error) {
	containersDir := filepath.Join(s.Root, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ContainerInfo{}, nil
		}
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	containers := make([]ContainerInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		containerDir := filepath.Join(containersDir, entry.Name())
		_, markerErr := os.Stat(filepath.Join(containerDir, containerCreationMarker))
		creationMarked := markerErr == nil
		if _, metadataErr := os.Stat(filepath.Join(containerDir, "container.json")); errors.Is(metadataErr, fs.ErrNotExist) {
			if creationMarked {
				// Another cluster node may still be preparing this directory in the
				// shared data root. It becomes visible only when container.json is
				// atomically published.
				continue
			}
		}
		info, err := s.loadContainerKey(entry.Name())
		if err != nil {
			if creationMarked {
				// Publication can become visible to NFS clients one pathname at a
				// time. The marker makes this an unpublished record, not corruption.
				continue
			}
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
		if includeHidden || !info.DistributedForkHidden {
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

// PublishDistributedForkTarget atomically makes a prepared target visible to
// ordinary TTC listing and lookup.
func (s Store) PublishDistributedForkTarget(transactionID string) (ContainerInfo, error) {
	info, err := s.LoadDistributedForkTarget(transactionID)
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
			if pending.Migration {
				found := false
				for i := range current.Branches {
					branch := &current.Branches[i]
					if branch.ID != pending.BranchID {
						continue
					}
					owner := branch.NodeID
					if owner == "" {
						owner = current.NodeID
					}
					if owner != pending.SourceNode {
						return false, fmt.Errorf("migration branch %q owner changed from %q to %q", pending.BranchID, pending.SourceNode, owner)
					}
					branch.FilesystemBranch = pending.FilesystemBranch
					branch.NodeID = pending.TargetNode
					branch.State = pending.State
					branch.StorageState = "local"
					branch.SourceNode = pending.SourceNode
					branch.DependentMemoryBytes = pending.DependentMemoryBytes
					branch.DependentFilesystemBytes = pending.DependentFilesystemBytes
					branch.DistributedForkTransactionID = transactionID
					branch.DistributedSparseRepository = pending.SparseRepository
					branch.DistributedSparseBranch = pending.SparseBranch
					branch.DistributedMigration = true
					found = true
					break
				}
				if !found {
					return false, fmt.Errorf("migration branch %q disappeared before publication", pending.BranchID)
				}
			} else {
				for _, branch := range current.Branches {
					if branch.ID == pending.BranchID {
						return false, fmt.Errorf("branch %q appeared while publishing distributed import", pending.BranchID)
					}
				}
				current.Branches = append(current.Branches, BranchInfo{
					ID: pending.BranchID, FilesystemBranch: pending.FilesystemBranch,
					NodeID: current.NodeID,
					State:  pending.State, StorageState: "local", SourceNode: pending.SourceNode, CreatedAt: pending.CreatedAt,
					DependentMemoryBytes: pending.DependentMemoryBytes, DependentFilesystemBytes: pending.DependentFilesystemBytes,
					DistributedForkTransactionID: transactionID,
					DistributedSparseRepository:  pending.SparseRepository,
					DistributedSparseBranch:      pending.SparseBranch,
				})
			}
			delete(current.DistributedForkImports, transactionID)
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
func (s Store) UpdateDistributedForkDependencies(transactionID string, memoryBytes, filesystemBytes uint64) error {
	info, err := s.LoadDistributedForkTarget(transactionID)
	if err != nil {
		return err
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if current.DistributedForkTransactionID == transactionID {
			for i := range current.Branches {
				if current.Branches[i].ID == "main" {
					current.Branches[i].DependentMemoryBytes = memoryBytes
					current.Branches[i].DependentFilesystemBytes = filesystemBytes
					return true, nil
				}
			}
		} else if pending, ok := current.DistributedForkImports[transactionID]; ok {
			pending.DependentMemoryBytes = memoryBytes
			pending.DependentFilesystemBytes = filesystemBytes
			current.DistributedForkImports[transactionID] = pending
			return true, nil
		} else {
			for i := range current.Branches {
				if current.Branches[i].DistributedForkTransactionID == transactionID {
					current.Branches[i].DependentMemoryBytes = memoryBytes
					current.Branches[i].DependentFilesystemBytes = filesystemBytes
					return true, nil
				}
			}
		}
		return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
	})
}

// UpdateDistributedForkTargetState records hidden-target lifecycle without
// making the target visible.
func (s Store) UpdateDistributedForkTargetState(transactionID, state string) error {
	info, err := s.LoadDistributedForkTarget(transactionID)
	if err != nil {
		return err
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if current.DistributedForkTransactionID == transactionID {
			current.State = state
			for i := range current.Branches {
				if current.Branches[i].ID == "main" {
					current.Branches[i].State = state
				}
			}
		} else if pending, ok := current.DistributedForkImports[transactionID]; ok {
			pending.State = state
			current.DistributedForkImports[transactionID] = pending
		} else {
			found := false
			for i := range current.Branches {
				if current.Branches[i].DistributedForkTransactionID == transactionID {
					current.Branches[i].State = state
					found = true
				}
			}
			if !found {
				return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
			}
			recomputeContainerState(current)
		}
		return true, nil
	})
}

// UpdateDistributedForkSparseRoot records the shared sparse-memory root that
// keeps a distributed target restorable until its demand loader has installed
// every chunk. Passing empty values clears an already-released root.
func (s Store) UpdateDistributedForkSparseRoot(transactionID, repository, branch string) error {
	if (repository == "") != (branch == "") {
		return errors.New("distributed sparse repository and branch must be specified together")
	}
	info, err := s.LoadDistributedForkTarget(transactionID)
	if err != nil {
		return err
	}
	return s.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		if current.DistributedForkTransactionID == transactionID {
			current.DistributedSparseRepository = repository
			current.DistributedSparseBranch = branch
		} else if pending, ok := current.DistributedForkImports[transactionID]; ok {
			pending.SparseRepository = repository
			pending.SparseBranch = branch
			current.DistributedForkImports[transactionID] = pending
		} else {
			found := false
			for i := range current.Branches {
				if current.Branches[i].DistributedForkTransactionID == transactionID {
					current.Branches[i].DistributedSparseRepository = repository
					current.Branches[i].DistributedSparseBranch = branch
					found = true
				}
			}
			if !found {
				return false, fmt.Errorf("distributed fork transaction %q target branch is unavailable", transactionID)
			}
		}
		return true, nil
	})
}

// DistributedForkTargetBranch resolves the branch associated with a target
// transaction. peer is true when it was imported into an existing family.
func (s Store) DistributedForkTargetBranch(transactionID string) (info ContainerInfo, branch BranchInfo, peer bool, err error) {
	info, err = s.LoadDistributedForkTarget(transactionID)
	if err != nil {
		return ContainerInfo{}, BranchInfo{}, false, err
	}
	if info.DistributedForkTransactionID == transactionID {
		for _, candidate := range info.Branches {
			if candidate.ID == "main" {
				candidate.DistributedForkTransactionID = transactionID
				candidate.DistributedSparseRepository = info.DistributedSparseRepository
				candidate.DistributedSparseBranch = info.DistributedSparseBranch
				return info, candidate, false, nil
			}
		}
		return ContainerInfo{}, BranchInfo{}, false, errors.New("distributed target family has no main branch")
	}
	if pending, ok := info.DistributedForkImports[transactionID]; ok {
		branch = BranchInfo{ID: pending.BranchID, CreatedAt: pending.CreatedAt}
		// A migration replaces an existing logical branch. Preserve its display
		// lineage and other stable metadata while the pending import supplies the
		// new runtime/filesystem identities.
		if pending.Migration {
			for _, candidate := range info.Branches {
				if candidate.ID == pending.BranchID {
					branch = candidate
					break
				}
			}
		}
		branch.FilesystemBranch = pending.FilesystemBranch
		branch.State = pending.State
		branch.DistributedForkTransactionID = transactionID
		branch.DistributedSparseRepository = pending.SparseRepository
		branch.DistributedSparseBranch = pending.SparseBranch
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

// RemoveDistributedForkTarget removes a transaction target and its image
// lease. Borrowed source-family repositories live outside the target directory
// and are deliberately retained.
func (s Store) RemoveDistributedForkTarget(transactionID string) error {
	info, branch, peer, err := s.DistributedForkTargetBranch(transactionID)
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
				if err := s.RemoveBranchArtifacts(info.ID, branch.ID); err != nil {
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
		if err := s.RemoveBranchArtifacts(info.ID, branch.ID); err != nil {
			return err
		}
		return s.RemoveBranchRecord(info.ID, branch.ID)
	}
	return s.removeContainerInfo(info)
}

func (s Store) removeDistributedMemorySnapshot(info ContainerInfo) error {
	if info.DistributedForkTransactionID == "" {
		return nil
	}
	artifacts := []struct {
		path string
		name string
	}{
		{filepath.Join(s.Root, "cluster", "memory-snapshots", info.DistributedForkTransactionID+".mem"), "memory snapshot"},
		{filepath.Join(s.Root, "cluster", "memory-metadata", info.DistributedForkTransactionID+".state"), "kernel metadata"},
	}
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
				info.Branches[i].State = state
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
			if branch.NodeID == "" {
				if i, ok := index[branch.ID]; ok && info.Branches[i].NodeID != "" {
					branch.NodeID = info.Branches[i].NodeID
				} else if branch.ParentID != "" && info.BranchNodeID(branch.ParentID) != "" {
					branch.NodeID = info.BranchNodeID(branch.ParentID)
				} else {
					branch.NodeID = info.NodeID
				}
			}
			if err := validateObjectName("branch", branch.ID); err != nil {
				return false, err
			}
			if branch.ID == branch.ParentID {
				return false, fmt.Errorf("branch %q cannot be its own parent", branch.ID)
			}
			if _, ok := seen[branch.ID]; ok {
				return false, fmt.Errorf("branch %q is repeated", branch.ID)
			}
			seen[branch.ID] = struct{}{}
			if i, ok := index[branch.ID]; ok {
				if info.Branches[i].ParentID != branch.ParentID {
					return false, fmt.Errorf("branch %q parent changed from %q to %q", branch.ID, info.Branches[i].ParentID, branch.ParentID)
				}
				if branch.StorageState == "" {
					branch.Unplaced = info.Branches[i].Unplaced
					branch.StorageState = info.Branches[i].StorageState
					branch.StorageGeneration = info.Branches[i].StorageGeneration
					branch.StorageError = info.Branches[i].StorageError
					branch.StorageManifest = info.Branches[i].StorageManifest
					branch.StorageRetryAt = info.Branches[i].StorageRetryAt
					branch.SourceNode = info.Branches[i].SourceNode
					branch.DependentMemoryBytes = info.Branches[i].DependentMemoryBytes
					branch.DependentFilesystemBytes = info.Branches[i].DependentFilesystemBytes
				}
				info.Branches[i] = branch
				continue
			}
			index[branch.ID] = len(info.Branches)
			info.Branches = append(info.Branches, branch)
		}
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
	}
}

// UpdateBranchState changes only the last known lifecycle state of a recorded branch.
func (s Store) UpdateBranchState(name, branchID, state string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			if info.Branches[i].ID != branchID {
				continue
			}
			info.Branches[i].State = state
			recomputeContainerState(info)
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// BeginBranchPublication allocates a family-monotonic generation and marks a
// branch as publishing. Generations are never reused, including after a branch
// name is removed and later recreated.
func (s Store) BeginBranchPublication(name, branchID string) (uint64, error) {
	var generation uint64
	err := s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		info.NextStorageGeneration++
		if info.NextStorageGeneration == 0 {
			return false, errors.New("storage generation space exhausted")
		}
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			branch.StorageGeneration = info.NextStorageGeneration
			branch.StorageState = "publishing"
			branch.StorageError = ""
			branch.StorageRetryAt = time.Time{}
			generation = branch.StorageGeneration
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
	return generation, err
}

// UpdateBranchStorage publishes a generation-fenced storage transition. A
// worker from an older generation cannot overwrite a newer capture or
// resurrect a branch whose generation was canceled and removed.
func (s Store) UpdateBranchStorage(name, branchID string, generation uint64, state, storageError string, retryAt time.Time) error {
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
			if generation != branch.StorageGeneration {
				return false, fmt.Errorf("branch %q storage generation is %d, worker has %d", branchID, branch.StorageGeneration, generation)
			}
			branch.StorageGeneration = generation
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
func (s Store) CompleteBranchPublication(name, branchID string, generation uint64, manifest, lifecycle string, unplace bool) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.StorageGeneration != generation {
				return false, fmt.Errorf("branch %q storage generation is %d, worker has %d", branchID, branch.StorageGeneration, generation)
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

// AbandonBranchPublication fences an asynchronous worker before a local
// capture is consumed to make the branch active again. The old shared
// manifest remains a valid immutable ancestor, but the active branch is no
// longer represented by the publication being abandoned.
func (s Store) AbandonBranchPublication(name, branchID string, generation uint64) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			if branch.StorageGeneration != generation {
				return false, fmt.Errorf("branch %q storage generation is %d, cancel has %d", branchID, branch.StorageGeneration, generation)
			}
			info.NextStorageGeneration++
			if info.NextStorageGeneration == 0 {
				return false, errors.New("storage generation space exhausted")
			}
			branch.StorageGeneration = info.NextStorageGeneration
			branch.StorageState = "local"
			branch.StorageError = ""
			branch.StorageRetryAt = time.Time{}
			return true, nil
		}
		return false, fmt.Errorf("branch %q is not recorded", branchID)
	})
}

// CompareAndSwapNodeID transfers whole-family cluster ownership while the
// family is process-free. The per-container advisory lock serializes TTC
// daemons sharing an NFS data root; higher-level placement generations fence
// stale callers.
func (s Store) CompareAndSwapNodeID(name, expected, replacement string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		if info.NodeID != expected {
			return false, fmt.Errorf("container %q cluster owner changed from %q to %q", info.Name, expected, info.NodeID)
		}
		info.NodeID = replacement
		return true, nil
	})
}

// CompareAndSwapBranchNodeID transfers one process-free branch without
// changing sibling placement. Empty legacy branch owners inherit NodeID.
func (s Store) CompareAndSwapBranchNodeID(name, branchID, expected, replacement string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		for i := range info.Branches {
			if info.Branches[i].ID != branchID {
				continue
			}
			owner := ""
			if !info.Branches[i].Unplaced {
				owner = info.Branches[i].NodeID
			}
			if owner == "" && !info.Branches[i].Unplaced {
				owner = info.NodeID
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
// transaction. Memory directories from older TTC versions are also promoted
// to the stable filesystem-branch key here; current directories never move on
// subsequent renames.
func (s Store) renameBranchArtifacts(containerID string, branch BranchInfo, oldID, newID string) (func() error, error) {
	var paths []renamePath
	dir := filepath.Join(s.activeRoot(), "containers", containerID, "console")
	entries, err := os.ReadDir(dir)
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

	legacyKey := hex.EncodeToString([]byte(oldID))
	stableKey := MemoryBranchKey(branch)
	if legacyKey != stableKey {
		for _, state := range []string{"dehydrated", "active"} {
			oldPath := filepath.Join(s.activeRoot(), "containers", containerID, "memory", state, legacyKey)
			if _, err := os.Lstat(oldPath); err == nil {
				paths = append(paths, renamePath{oldPath, filepath.Join(filepath.Dir(oldPath), stableKey)})
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("checking legacy %s memory state: %w", state, err)
			}
		}
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
// rewrites its peers.
func (s Store) RecordBranchStart(name, branchID string) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		now := time.Now().UTC()
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			branch.State = "running"
			branch.Unplaced = false
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
		found := false
		alreadyExited := false
		for i := range info.Branches {
			branch := &info.Branches[i]
			if branch.ID != branchID {
				continue
			}
			alreadyExited = branch.State == "exited" || branch.State == "stopped"
			branch.State = "exited"
			found = true
			break
		}
		if !found {
			return false, fmt.Errorf("branch %q is not recorded", branchID)
		}
		recomputeContainerState(info)
		if info.State == "exited" {
			now := time.Now().UTC()
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
			branch.State = "exited"
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

// ReconcileRuntimeUnavailable marks only branches that required the missing
// Sentry as exited. Durable dehydrated branches remain recoverable.
func (s Store) ReconcileRuntimeUnavailable(name string, exitStatus *int) error {
	return s.ReconcileNodeRuntimeUnavailable(name, "", exitStatus)
}

// ReconcileNodeRuntimeUnavailable marks branches whose runtime was lost on
// nodeID as exited. An empty nodeID retains the legacy whole-family behavior.
// Elastic families may have live siblings on other nodes, which this operation
// must never rewrite merely because they are absent from the local runsc root.
func (s Store) ReconcileNodeRuntimeUnavailable(name, nodeID string, exitStatus *int) error {
	return s.mutateContainer(name, func(info *ContainerInfo) (bool, error) {
		changed := false
		for i := range info.Branches {
			branch := &info.Branches[i]
			if nodeID != "" && info.BranchNodeID(branch.ID) != nodeID {
				continue
			}
			switch branch.State {
			case "creating", "running", "paused", "dehydrating", "rehydrating":
				branch.State = "exited"
				changed = true
			}
		}
		oldState := info.State
		recomputeContainerState(info)
		if oldState != info.State {
			changed = true
		}
		if info.State == "exited" && exitStatus != nil {
			now := time.Now().UTC()
			info.ExitStatus = exitStatus
			info.ExitedAt = &now
			changed = true
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
	manager, err := ttcvolume.OpenManager(s.activeRoot(), true)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(repositories))
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		if persistentBranch == ttcvolume.MainBranch && !repositoryCanonicalBranchBelongsToFamily(config) {
			// The canonical branch is named-volume state, not family-private
			// state. Descendants and reused visible "main" branches are scoped
			// and therefore take the normal removal path.
			continue
		}
		repository, err := manager.OpenPrivate(path)
		if err != nil {
			return fmt.Errorf("opening repository %q: %w", config.Source, err)
		}
		removeErr := repository.RemoveBranch(persistentBranch)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		if removeErr == nil {
			removeErr = repository.GarbageCollect()
		}
		closeErr := repository.Close()
		if removeErr != nil {
			return fmt.Errorf("removing branch %q from repository %q: %w", persistentBranch, config.Source, removeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing repository %q: %w", config.Source, closeErr)
		}
	}
	return nil
}

// GarbageCollectRepositories reclaims unreachable metadata and data objects
// from every named and family-private repository in the data root. Repository
// GC shares the mutation flock with Sentry, so this is safe while containers
// are running.
func (s Store) GarbageCollectRepositories() (int, error) {
	manager, err := ttcvolume.OpenManager(s.activeRoot(), true)
	if err != nil {
		return 0, err
	}
	paths := make(map[string]struct{})
	volumes, err := manager.List()
	if err != nil {
		return 0, err
	}
	for _, volume := range volumes {
		paths[filepath.Join(s.activeRoot(), "volumes-v2", volume.ID)] = struct{}{}
	}
	containers, err := s.listContainers(true)
	if err != nil {
		return 0, err
	}
	for _, container := range containers {
		spec, err := s.LoadSpec(container.ID)
		if err != nil {
			return 0, err
		}
		repositories, err := specutils.TTCVolumeRepositories(spec)
		if err != nil {
			return 0, err
		}
		for _, repository := range repositories {
			paths[filepath.Clean(repository.Path)] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	collected := 0
	for _, path := range ordered {
		repository, err := manager.OpenPrivate(path)
		if err != nil {
			return collected, fmt.Errorf("opening repository %q: %w", path, err)
		}
		gcErr := repository.GarbageCollect()
		closeErr := repository.Close()
		if gcErr != nil {
			return collected, fmt.Errorf("collecting repository %q: %w", path, gcErr)
		}
		if closeErr != nil {
			return collected, fmt.Errorf("closing repository %q: %w", path, closeErr)
		}
		collected++
	}
	return collected, nil
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
func (s Store) RemoveBranchArtifacts(name, branchID string) error {
	if err := validateObjectName("branch", branchID); err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	containerDir := s.activeContainerDir(info.ID)
	encodedBranch := hex.EncodeToString([]byte(branchID))
	for _, kind := range []string{"active", "dehydrated"} {
		if err := os.RemoveAll(filepath.Join(containerDir, "memory", kind, encodedBranch)); err != nil {
			return err
		}
	}
	consoleDir := filepath.Join(containerDir, "console")
	base := filepath.Join(consoleDir, branchID)
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
	if err := os.RemoveAll(filepath.Join(consoleDir, "exec", branchID)); err != nil {
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
	if err := s.removeAll(s.activeContainerDir(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warningf("Deferring cleanup of node-local state for container %q: %v", id, err)
	}
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
	data, err := os.ReadFile(filepath.Join(path, "container.json"))
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
	if err := s.removeDistributedMemorySnapshot(*info); err != nil {
		return err
	}
	return s.removeAll(path)
}

func (s Store) removeContainerInfo(info ContainerInfo) error {
	tombstone, err := s.stageContainerRemoval(info.ID)
	if err != nil {
		return err
	}
	if err := s.cleanupRemovedContainer(tombstone, &info); err != nil {
		log.Warningf("Container %q was removed; deferred physical cleanup of %q: %v", info.Name, tombstone, err)
	}
	s.removeActiveContainer(info.ID)
	// Also sweep the repository namespace used by experimental pre-tiered
	// roots. Fresh format-v1 roots never populate it, but keeping deletion
	// tolerant avoids leaking data created by tests or interrupted development
	// builds that already carry the current root marker.
	if err := s.gcPromotedRepositories(); err != nil {
		log.Warningf("Container %q was removed; deferred legacy repository cleanup: %v", info.Name, err)
	}
	return nil
}

// RecoverContainerRemovals moves incomplete legacy container directories out
// of the active namespace, then retries every tombstone independently. It is
// intended to run at daemon startup and during periodic maintenance while the
// data-root mutation lock is held.
func (s Store) RecoverContainerRemovals() (int, error) {
	containersRoot := filepath.Join(s.Root, "containers")
	entries, err := os.ReadDir(containersRoot)
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
	entries, err := os.ReadDir(root)
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

// PromoteContainerRepositories moves container-private repositories into the
// shared cluster repository namespace. Open repository directory descriptors
// remain valid across the rename, while future starts and distributed targets
// use the new stable paths recorded in the source OCI spec.
//
// Promotion is idempotent and is required before reserving a distributed fork
// target. It ensures that deleting the source container metadata cannot delete
// filesystem state still referenced by a remote branch.
func (s Store) PromoteContainerRepositories(name string) error {
	return s.withPromotedRepositoryLock(func() error {
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
		containerDir := filepath.Join(s.Root, "containers", info.ID)
		familyDir := filepath.Join(s.promotedRepositoryRoot(), info.ID)
		type movedRepository struct {
			oldPath string
			newPath string
		}
		var moved []movedRepository
		changed := false
		rollback := func() {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(moved[i].newPath, moved[i].oldPath)
			}
		}
		for i := range repositories {
			repository := &repositories[i]
			inside, err := pathWithin(containerDir, repository.Path)
			if err != nil {
				rollback()
				return fmt.Errorf("checking repository %q: %w", repository.Source, err)
			}
			if !inside {
				continue
			}
			if err := os.MkdirAll(familyDir, 0700); err != nil {
				rollback()
				return fmt.Errorf("creating promoted repository namespace: %w", err)
			}
			digest := sha256.Sum256([]byte(repository.Source))
			base := filepath.Base(repository.Path)
			destination := filepath.Join(familyDir, base+"-"+hex.EncodeToString(digest[:8]))
			oldExists, err := directoryExistsWithoutSymlink(repository.Path)
			if err != nil {
				rollback()
				return fmt.Errorf("checking repository %q source: %w", repository.Source, err)
			}
			newExists, err := directoryExistsWithoutSymlink(destination)
			if err != nil {
				rollback()
				return fmt.Errorf("checking repository %q destination: %w", repository.Source, err)
			}
			switch {
			case oldExists && !newExists:
				if err := os.Rename(repository.Path, destination); err != nil {
					rollback()
					return fmt.Errorf("promoting repository %q: %w", repository.Source, err)
				}
				moved = append(moved, movedRepository{oldPath: repository.Path, newPath: destination})
			case oldExists && newExists:
				rollback()
				return fmt.Errorf("promoting repository %q: both source and destination exist", repository.Source)
			case !oldExists && !newExists:
				rollback()
				return fmt.Errorf("promoting repository %q: source is missing", repository.Source)
			}
			repository.Path = destination
			changed = true
		}
		if !changed {
			return nil
		}
		encoded, err := json.Marshal(repositories)
		if err != nil {
			rollback()
			return err
		}
		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		spec.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
		if err := writeJSON(filepath.Join(info.BundleDir, "config.json"), spec); err != nil {
			rollback()
			return fmt.Errorf("recording promoted repository paths: %w", err)
		}
		return nil
	})
}

func (s Store) promotedRepositoryRoot() string {
	return filepath.Join(s.Root, "cluster", promotedRepositoryDir)
}

func (s Store) withPromotedRepositoryLock(fn func() error) error {
	root := s.promotedRepositoryRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn()
}

func pathWithin(parent, child string) (bool, error) {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	return relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func directoryExistsWithoutSymlink(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%q is not a directory", path)
	}
	return true, nil
}

// gcPromotedRepositories removes stable family repositories after no
// remaining container spec references them. Scanning specs is deliberately
// conservative: any unreadable metadata aborts collection rather than risking
// live branch data.
func (s Store) gcPromotedRepositories() error {
	return s.withPromotedRepositoryLock(func() error {
		root := s.promotedRepositoryRoot()
		containers, err := s.listContainers(true)
		if err != nil {
			return err
		}
		referenced := make(map[string]struct{})
		for _, container := range containers {
			spec, err := s.LoadSpec(container.ID)
			if err != nil {
				return err
			}
			repositories, err := specutils.TTCVolumeRepositories(spec)
			if err != nil {
				return err
			}
			for _, repository := range repositories {
				inside, err := pathWithin(root, repository.Path)
				if err != nil {
					return err
				}
				if inside {
					referenced[filepath.Clean(repository.Path)] = struct{}{}
				}
			}
		}
		families, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, family := range families {
			if family.Name() == ".lock" || !family.IsDir() {
				continue
			}
			familyPath := filepath.Join(root, family.Name())
			repositories, err := os.ReadDir(familyPath)
			if err != nil {
				return err
			}
			for _, repository := range repositories {
				path := filepath.Join(familyPath, repository.Name())
				if _, ok := referenced[path]; ok {
					continue
				}
				if repository.Type()&os.ModeSymlink != 0 || !repository.IsDir() {
					return fmt.Errorf("unexpected entry in promoted repository namespace: %s", path)
				}
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			remaining, err := os.ReadDir(familyPath)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if len(remaining) == 0 {
				if err := os.Remove(familyPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
		return nil
	})
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

var imageRootDirectoryPattern = regexp.MustCompile(`^(?:v[0-9]+-)?sha256-[0-9a-f]{64}-.+$`)

// PruneImageRoots removes materialized image roots that are no longer usable
// by either the current image catalog or a persistent container. In
// particular, this collects roots left behind when imageRootFormatVersion is
// advanced. Container references are authoritative so that an older stopped
// container remains startable until RefreshContainerImageRoot migrates it.
func (s Store) PruneImageRoots(ctx context.Context) (int, error) {
	stored, err := images.List(ctx, images.ListOptions{Root: s.Root})
	if err != nil {
		return 0, fmt.Errorf("listing images before pruning roots: %w", err)
	}
	containers, err := s.listContainers(true)
	if err != nil {
		return 0, fmt.Errorf("listing containers before pruning image roots: %w", err)
	}
	return s.pruneImageRoots(stored, containers)
}

func (s Store) pruneImageRoots(stored []images.Info, containers []ContainerInfo) (int, error) {
	keep := make(map[string]struct{}, len(stored)+len(containers))
	for _, image := range stored {
		keep[imageRootKey(image.Digest, image.Platform)] = struct{}{}
	}
	imagesDir := s.imageRootDir()
	for _, container := range containers {
		if container.Digest != "" && container.Platform != "" {
			keep[imageRootKey(container.Digest, container.Platform)] = struct{}{}
			continue
		}
		// Compatibility for tests and old records that predate image identity.
		rootfs := filepath.Clean(container.RootFS)
		if filepath.Base(rootfs) != "rootfs" {
			continue
		}
		relative, err := filepath.Rel(imagesDir, filepath.Dir(rootfs))
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !strings.ContainsRune(relative, filepath.Separator) {
			keep[relative] = struct{}{}
		}
	}

	removed := 0
	err := s.withImageRootLock(func() error {
		entries, err := os.ReadDir(imagesDir)
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

// RemoveImageRoots removes every platform-specific immutable root for digest.
// Callers must first prove that no persistent container or image alias still
// references the digest.
func (s Store) RemoveImageRoots(digest string) (int, error) {
	if digest == "" || !strings.Contains(digest, ":") {
		return 0, fmt.Errorf("invalid image digest %q", digest)
	}
	imagesDir := s.imageRootDir()
	prefixes := []string{
		safeComponent(digest + "-"),
		safeComponent("v2-" + digest + "-"),
		safeComponent("v3-" + digest + "-"),
		safeComponent(imageRootFormatVersion + "-" + digest + "-"),
	}
	removed := 0
	err := s.withImageRootLock(func() error {
		entries, err := os.ReadDir(imagesDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing image roots: %w", err)
		}
		for _, entry := range entries {
			matches := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(entry.Name(), prefix) {
					matches = true
					break
				}
			}
			if !entry.IsDir() || !matches {
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
	info, err := s.LoadContainer(container)
	if err != nil {
		return ConsolePaths{}, err
	}
	dir := filepath.Join(s.activeRoot(), "containers", info.ID, "console")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ConsolePaths{}, fmt.Errorf("creating console state for %q: %w", container, err)
	}
	base := filepath.Join(dir, branch)
	return ConsolePaths{
		Log:            base + ".log",
		Lock:           base + ".lock",
		StdinLock:      base + ".stdin.lock",
		Error:          base + ".error.log",
		WaitStatus:     base + ".wait-status",
		RetentionStats: ttcconsole.RetentionStatsPath(base + ".log"),
		PID:            base + ".pid",
	}, nil
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
	info, err := s.LoadContainer(container)
	if err != nil {
		return ConsolePaths{}, err
	}
	dir := filepath.Join(s.activeRoot(), "containers", info.ID, "console", "exec", branch)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ConsolePaths{}, fmt.Errorf("creating exec console state for %q: %w", container, err)
	}
	base := filepath.Join(dir, execID)
	return ConsolePaths{
		Log:            base + ".log",
		Lock:           base + ".lock",
		StdinLock:      base + ".stdin.lock",
		Error:          base + ".error.log",
		WaitStatus:     base + ".wait-status",
		RetentionStats: ttcconsole.RetentionStatsPath(base + ".log"),
		PID:            base + ".pid",
	}, nil
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
	dir := filepath.Join(s.activeRoot(), "containers", info.ID, "console", "exec", branch)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing exec console retention for %q: %w", container, err)
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

func removeStaleImageRootTemps(imagesDir, key string) error {
	entries, err := os.ReadDir(imagesDir)
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
	data, err := os.ReadFile(filepath.Join(dir, "image.json"))
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
	if _, err := ownership.Read(ownership.ManifestPath(rootfs)); err != nil {
		return images.RootFSInfo{}, fmt.Errorf("cached rootfs ownership is invalid: %w", err)
	}
	return info, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
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
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
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
