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
	"syscall"
	"time"
	"unicode"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	"gvisor.dev/gvisor/runsc/ttc/images"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

const (
	imageRootFormatVersion   = "v3"
	containerIDBytes         = 32
	rootfsUpperRepositoryDir = "rootfs-upper"
	hostMountRepositoryDir   = "host-mounts"
	// DefaultLogDriver is the persistent logging policy used when none is specified.
	DefaultLogDriver = "local"
)

// Store owns persistent image roots and generated container bundles.
type Store struct {
	Root string
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
	Progress           images.ProgressFunc
	// VolumeCopyMode is the copy policy already verified by ttcd at startup.
	// Empty permits Store-only callers to probe either supported mode.
	VolumeCopyMode ttcvolume.CopyMode
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
}

// DistributedForkOptions materializes a hidden target bundle that refers to
// filesystem branches created atomically by the source Sentry. The source and
// target stores must name the same shared TTC repository.
type DistributedForkOptions struct {
	SourceContainer string
	TargetName      string
	TargetBranch    string
	TransactionID   string
	NodeID          string
}

// BranchInfo is immutable branch lineage plus its last known lifecycle state.
type BranchInfo struct {
	ID               string     `json:"id"`
	ParentID         string     `json:"parent_id,omitempty"`
	State            string     `json:"state"`
	ForkTotalNS      int64      `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS int64      `json:"source_downtime_ns,omitempty"`
	KernelForkNS     int64      `json:"kernel_fork_ns,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
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
	ID                           string            `json:"id"`
	Name                         string            `json:"name"`
	Reference                    string            `json:"reference"`
	Digest                       string            `json:"digest"`
	ImageSize                    int64             `json:"image_size,omitempty"`
	LeaseID                      string            `json:"lease_id,omitempty"`
	Platform                     string            `json:"platform"`
	BundleDir                    string            `json:"bundle_dir"`
	RootFS                       string            `json:"rootfs"`
	Branches                     []BranchInfo      `json:"branches,omitempty"`
	NetworkMode                  string            `json:"network_mode,omitempty"`
	NetworkNAT                   bool              `json:"network_nat,omitempty"`
	NetworkIP                    string            `json:"network_ip,omitempty"`
	NetworkMAC                   string            `json:"network_mac,omitempty"`
	PublishedPorts               []PortBinding     `json:"published_ports,omitempty"`
	LogDriver                    string            `json:"log_driver,omitempty"`
	LogOptions                   map[string]string `json:"log_options,omitempty"`
	LogAttributes                map[string]string `json:"log_attributes,omitempty"`
	ConsoleLogMaxBytes           int64             `json:"console_log_max_bytes,omitempty"`
	ConsoleLogMaxFiles           int               `json:"console_log_max_files,omitempty"`
	Terminal                     bool              `json:"terminal,omitempty"`
	OpenStdin                    bool              `json:"open_stdin,omitempty"`
	AutoRemove                   bool              `json:"auto_remove,omitempty"`
	StopSignal                   string            `json:"stop_signal,omitempty"`
	StopTimeout                  *int              `json:"stop_timeout,omitempty"`
	PidsLimit                    *int64            `json:"pids_limit,omitempty"`
	MemoryLimit                  *int64            `json:"memory_limit,omitempty"`
	NanoCPUs                     *int64            `json:"nano_cpus,omitempty"`
	CPUShares                    *uint64           `json:"cpu_shares,omitempty"`
	CPUPeriod                    *uint64           `json:"cpu_period,omitempty"`
	CPUQuota                     *int64            `json:"cpu_quota,omitempty"`
	ShmSize                      *int64            `json:"shm_size,omitempty"`
	GroupAdd                     []string          `json:"group_add,omitempty"`
	CapAdd                       []string          `json:"cap_add,omitempty"`
	CapDrop                      []string          `json:"cap_drop,omitempty"`
	Ulimits                      []Ulimit          `json:"ulimits,omitempty"`
	ExtraHosts                   []string          `json:"extra_hosts,omitempty"`
	DNS                          []string          `json:"dns,omitempty"`
	DNSSearch                    []string          `json:"dns_search,omitempty"`
	DNSOptions                   []string          `json:"dns_options,omitempty"`
	Volumes                      []ManagedVolume   `json:"volumes,omitempty"`
	RootfsUpperID                string            `json:"rootfs_upper_id,omitempty"`
	RootfsUpperCopyMode          string            `json:"rootfs_upper_copy_mode,omitempty"`
	State                        string            `json:"state"`
	ExitStatus                   *int              `json:"exit_status,omitempty"`
	CreatedAt                    time.Time         `json:"created_at"`
	StartedAt                    *time.Time        `json:"started_at,omitempty"`
	ExitedAt                     *time.Time        `json:"exited_at,omitempty"`
	DistributedForkTransactionID string            `json:"distributed_fork_transaction_id,omitempty"`
	DistributedForkNodeID        string            `json:"distributed_fork_node_id,omitempty"`
	DistributedForkHidden        bool              `json:"distributed_fork_hidden,omitempty"`
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
	keep := false
	leaseID := ""
	defer func() {
		if !keep {
			if leaseID != "" {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = images.DeleteContentLease(cleanupCtx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID})
				cancel()
			}
			os.RemoveAll(containerDir)
		}
	}()
	rootfsUpperID := ""
	if !opts.ReadonlyRootfs {
		manager, err := ttcvolume.OpenManager(s.Root, opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
		if err != nil {
			return Prepared{}, fmt.Errorf("opening rootfs upper store: %w", err)
		}
		if opts.VolumeCopyMode != "" && manager.Capability().Mode != opts.VolumeCopyMode {
			return Prepared{}, fmt.Errorf("volume backing capability changed while preparing rootfs: daemon mode is %s, current mode is %s", opts.VolumeCopyMode, manager.Capability().Mode)
		}
		rootfsUpper, err := manager.CreatePrivate(filepath.Join(containerDir, rootfsUpperRepositoryDir), "rootfs")
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
	if err := s.resolveWritableHostMounts(containerDir, &opts); err != nil {
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
	}
	metadata.Branches = []BranchInfo{{ID: "main", State: "created", CreatedAt: metadata.CreatedAt}}
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
	keep := false
	leaseID := ""
	var importedVolumeIDs []string
	defer func() {
		if keep {
			return
		}
		if manager, err := ttcvolume.OpenManager(s.Root, true); err == nil {
			for _, volumeID := range importedVolumeIDs {
				_ = manager.Remove(volumeID)
			}
		}
		if leaseID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = images.DeleteContentLease(cleanupCtx, images.ContentLeaseOptions{Root: s.Root, LeaseID: leaseID})
			cancel()
		}
		_ = os.RemoveAll(containerDir)
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
	manager, err := ttcvolume.OpenManager(s.Root, opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
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
			path := filepath.Join(containerDir, rootfsUpperRepositoryDir)
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
			path := filepath.Join(containerDir, hostMountRepositoryDir, strconv.Itoa(index))
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
		destination := filepath.Join(containerDir, "checkpoint-mounts", strconv.Itoa(index))
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
	metadata.Branches = []BranchInfo{{ID: "main", State: "created", CreatedAt: now}}
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
	if err := validateObjectName("distributed fork transaction", opts.TransactionID); err != nil {
		return Prepared{}, err
	}
	if opts.NodeID == "" {
		return Prepared{}, errors.New("distributed fork target node ID is empty")
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
	containers, err := s.listContainers(true)
	if err != nil {
		return Prepared{}, err
	}
	for _, existing := range containers {
		if existing.Name == opts.TargetName || existing.ID == opts.TargetName {
			return Prepared{}, fmt.Errorf("container name %q is already in use by %s", opts.TargetName, existing.ID)
		}
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
		// The target borrows a repository owned by the source family. Removing
		// target metadata must never delete that shared repository directory.
		repositories[i].Private = false
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
		_ = os.RemoveAll(containerDir)
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
	target.Branches = []BranchInfo{{ID: "main", State: "created", CreatedAt: now}}
	target.DistributedForkTransactionID = opts.TransactionID
	target.DistributedForkNodeID = opts.NodeID
	target.DistributedForkHidden = true
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
	manager, err := ttcvolume.OpenManager(s.Root, opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
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
	manager, err := ttcvolume.OpenManager(s.Root, opts.VolumeCopyMode == "" || opts.VolumeCopyMode == ttcvolume.CopyModeFull)
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
		if containers[i].DistributedForkTransactionID != transactionID {
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
		info.Branches = []BranchInfo{{ID: "main", State: info.State, CreatedAt: info.CreatedAt}}
	}
	seenBranches := make(map[string]struct{}, len(info.Branches))
	for _, branch := range info.Branches {
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
	}
	if _, ok := seenBranches["main"]; !ok {
		return ContainerInfo{}, fmt.Errorf("container %q branch metadata omits main", key)
	}
	seenPortEndpoints := make(map[string]struct{}, len(info.PublishedPorts))
	for i := range info.PublishedPorts {
		binding, err := normalizePortBinding(info.PublishedPorts[i])
		if err != nil {
			return ContainerInfo{}, fmt.Errorf("container %q has invalid published-port metadata: %w", key, err)
		}
		if _, ok := seenBranches[binding.BranchID]; !ok {
			return ContainerInfo{}, fmt.Errorf("container %q published port targets unknown branch %q", key, binding.BranchID)
		}
		endpoint := portHostEndpoint(binding)
		if _, ok := seenPortEndpoints[endpoint]; ok {
			return ContainerInfo{}, fmt.Errorf("container %q contains duplicate published host endpoint %s", key, endpoint)
		}
		seenPortEndpoints[endpoint] = struct{}{}
		info.PublishedPorts[i] = binding
	}
	networkMode, err := normalizeNetworkMode(info.NetworkMode)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata: %w", key, err)
	}
	info.NetworkMode = networkMode
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
	info, err := s.LoadContainer(name)
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
	return &spec, nil
}

// ListContainers returns all persistent containers ordered by name.
func (s Store) ListContainers() ([]ContainerInfo, error) {
	return s.listContainers(false)
}

func (s Store) listContainers(includeHidden bool) ([]ContainerInfo, error) {
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
		info, err := s.loadContainerKey(entry.Name())
		if err != nil {
			return nil, err
		}
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
	if !info.DistributedForkHidden {
		return info, nil
	}
	info.DistributedForkHidden = false
	if err := writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info); err != nil {
		return ContainerInfo{}, err
	}
	return info, nil
}

// UpdateDistributedForkTargetState records hidden-target lifecycle without
// making the target visible.
func (s Store) UpdateDistributedForkTargetState(transactionID, state string) error {
	info, err := s.LoadDistributedForkTarget(transactionID)
	if err != nil {
		return err
	}
	info.State = state
	for i := range info.Branches {
		if info.Branches[i].ID == "main" {
			info.Branches[i].State = state
		}
	}
	return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
}

// RemoveDistributedForkTarget removes a transaction target and its image
// lease. Borrowed source-family repositories live outside the target directory
// and are deliberately retained.
func (s Store) RemoveDistributedForkTarget(transactionID string) error {
	info, err := s.LoadDistributedForkTarget(transactionID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.LeaseID != "" {
		if err := images.DeleteContentLease(context.Background(), images.ContentLeaseOptions{Root: s.Root, LeaseID: info.LeaseID}); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(s.Root, "containers", info.ID))
}

// UpdateState records the last known base-container lifecycle state.
func (s Store) UpdateState(name, state string, exitStatus *int) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	info.State = state
	info.ExitStatus = exitStatus
	for i := range info.Branches {
		branch := &info.Branches[i]
		if branch.ID == "main" {
			branch.State = state
		}
		if state == "running" && branch.ID != "main" && branch.State != "deleted" {
			branch.State = "deleted"
			branch.DeletedAt = &now
		}
		if state == "exited" && branch.State != "deleted" {
			branch.State = "exited"
		}
	}
	if state == "running" {
		bindings := info.PublishedPorts[:0]
		for _, binding := range info.PublishedPorts {
			if binding.Branch() == "main" {
				bindings = append(bindings, binding)
			}
		}
		info.PublishedPorts = bindings
	}
	switch state {
	case "running":
		info.StartedAt = &now
		info.ExitedAt = nil
	case "exited":
		info.ExitedAt = &now
	}
	path := filepath.Join(s.Root, "containers", info.ID, "container.json")
	if err := writeJSON(path, &info); err != nil {
		return fmt.Errorf("updating container %q: %w", name, err)
	}
	return nil
}

// AddPortBinding atomically records a branch-local published TCP listener.
func (s Store) AddPortBinding(name string, binding PortBinding) error {
	binding, err := normalizePortBinding(binding)
	if err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	if info.NetworkMode != "ttc" {
		return fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
	}
	branchFound := false
	for _, branch := range info.Branches {
		if branch.ID != binding.BranchID {
			continue
		}
		if branch.State != "running" && branch.State != "paused" {
			return fmt.Errorf("branch %q is %s", binding.BranchID, branch.State)
		}
		branchFound = true
		break
	}
	if !branchFound {
		return fmt.Errorf("branch %q is not recorded", binding.BranchID)
	}
	endpoint := portHostEndpoint(binding)
	for _, existing := range info.PublishedPorts {
		if samePortBinding(existing, binding) {
			return fmt.Errorf("published port for %s already exists on branch %q", endpoint, binding.BranchID)
		}
		if portHostEndpoint(existing) == endpoint {
			return fmt.Errorf("published host endpoint %s is already assigned to branch %q", endpoint, existing.Branch())
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
	return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
}

// RemovePortBinding atomically removes one exact branch-local listener.
func (s Store) RemovePortBinding(name string, binding PortBinding) error {
	binding, err := normalizePortBinding(binding)
	if err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	for i, existing := range info.PublishedPorts {
		if !samePortBinding(existing, binding) {
			continue
		}
		info.PublishedPorts = append(info.PublishedPorts[:i], info.PublishedPorts[i+1:]...)
		return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
	}
	return fmt.Errorf("published port is not configured on branch %q", binding.BranchID)
}

// RemovePortBindingsForBranch atomically forgets every listener for branchID.
func (s Store) RemovePortBindingsForBranch(name, branchID string) error {
	if err := validateObjectName("branch", branchID); err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	bindings := info.PublishedPorts[:0]
	for _, binding := range info.PublishedPorts {
		if binding.Branch() != branchID {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) == len(info.PublishedPorts) {
		return nil
	}
	info.PublishedPorts = bindings
	return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
}

// RecordBranch appends one immutable branch identity or refreshes its lifecycle data.
func (s Store) RecordBranch(name string, branch BranchInfo) error {
	return s.RecordBranches(name, []BranchInfo{branch})
}

// RecordBranches atomically appends or refreshes multiple branch identities.
func (s Store) RecordBranches(name string, branches []BranchInfo) error {
	if len(branches) == 0 {
		return errors.New("at least one branch is required")
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	index := make(map[string]int, len(info.Branches))
	for i := range info.Branches {
		index[info.Branches[i].ID] = i
	}
	seen := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		if err := validateObjectName("branch", branch.ID); err != nil {
			return err
		}
		if branch.ID == branch.ParentID {
			return fmt.Errorf("branch %q cannot be its own parent", branch.ID)
		}
		if _, ok := seen[branch.ID]; ok {
			return fmt.Errorf("branch %q is repeated", branch.ID)
		}
		seen[branch.ID] = struct{}{}
		if i, ok := index[branch.ID]; ok {
			if info.Branches[i].ParentID != branch.ParentID {
				return fmt.Errorf("branch %q parent changed from %q to %q", branch.ID, info.Branches[i].ParentID, branch.ParentID)
			}
			info.Branches[i] = branch
			continue
		}
		index[branch.ID] = len(info.Branches)
		info.Branches = append(info.Branches, branch)
	}
	return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
}

// UpdateBranchState changes only the last known lifecycle state of a recorded branch.
func (s Store) UpdateBranchState(name, branchID, state string) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	for i := range info.Branches {
		if info.Branches[i].ID != branchID {
			continue
		}
		if info.Branches[i].State == "deleted" {
			return fmt.Errorf("branch %q is deleted", branchID)
		}
		info.Branches[i].State = state
		if branchID == "main" {
			info.State = state
		}
		return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
	}
	return fmt.Errorf("branch %q is not recorded", branchID)
}

// MarkBranchDeleted retains branch lineage while marking its runtime state gone.
func (s Store) MarkBranchDeleted(name, branchID string) error {
	if branchID == "main" {
		return errors.New("main branch is removed with its container family")
	}
	if err := validateObjectName("branch", branchID); err != nil {
		return err
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range info.Branches {
		if info.Branches[i].ID == branchID {
			info.Branches[i].State = "deleted"
			info.Branches[i].DeletedAt = &now
			bindings := info.PublishedPorts[:0]
			for _, binding := range info.PublishedPorts {
				if binding.Branch() != branchID {
					bindings = append(bindings, binding)
				}
			}
			info.PublishedPorts = bindings
			return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
		}
	}
	info.Branches = append(info.Branches, BranchInfo{ID: branchID, State: "deleted", CreatedAt: now, DeletedAt: &now})
	return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
}

// RemoveContainer removes persistent configuration for name. Shared image
// roots remain until image garbage collection proves that no container uses
// them.
func (s Store) RemoveContainer(name string) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	if info.LeaseID != "" {
		if err := images.DeleteContentLease(context.Background(), images.ContentLeaseOptions{Root: s.Root, LeaseID: info.LeaseID}); err != nil {
			return fmt.Errorf("releasing image content for container %q: %w", name, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(s.Root, "containers", info.ID)); err != nil {
		return fmt.Errorf("removing container %q: %w", name, err)
	}
	return nil
}

// HasImageRoot reports whether TTC has materialized the immutable root for a
// digest and platform. A corrupt cache entry is reported instead of silently
// looking absent.
func (s Store) HasImageRoot(digest, platform string) (bool, error) {
	if digest == "" || platform == "" {
		return false, nil
	}
	dir := filepath.Join(s.Root, "images", imageRootKey(digest, platform))
	if _, err := loadImageRoot(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading cached root for %s (%s): %w", digest, platform, err)
	}
	return true, nil
}

// RemoveImageRoots removes every platform-specific immutable root for digest.
// Callers must first prove that no persistent container or image alias still
// references the digest.
func (s Store) RemoveImageRoots(digest string) (int, error) {
	if digest == "" || !strings.Contains(digest, ":") {
		return 0, fmt.Errorf("invalid image digest %q", digest)
	}
	imagesDir := filepath.Join(s.Root, "images")
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing image roots: %w", err)
	}
	prefixes := []string{
		safeComponent(digest + "-"),
		safeComponent("v2-" + digest + "-"),
		safeComponent(imageRootFormatVersion + "-" + digest + "-"),
	}
	removed := 0
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
			return removed, fmt.Errorf("removing image root %q: %w", path, err)
		}
		removed++
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
	dir := filepath.Join(s.Root, "containers", info.ID, "console")
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
	dir := filepath.Join(s.Root, "containers", info.ID, "console", "exec", branch)
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
	dir := filepath.Join(s.Root, "containers", info.ID, "console", "exec", branch)
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

func (s Store) prepareImageRoot(ctx context.Context, pulled images.Info, platform string, progress images.ProgressFunc) (_ images.RootFSInfo, _ string, retErr error) {
	imagesDir := filepath.Join(s.Root, "images")
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
	info, err := images.MaterializeRootFS(ctx, images.RootFSOptions{
		StoreRoot: s.Root,
		Reference: pulled.Reference,
		Platform:  platform,
		Root:      rootfs,
		Progress:  progress,
	})
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
