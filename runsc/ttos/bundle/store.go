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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ttosconsole "gvisor.dev/gvisor/runsc/ttos/console"
	"gvisor.dev/gvisor/runsc/ttos/images"
	"gvisor.dev/gvisor/runsc/ttos/ownership"
)

const (
	imageRootFormatVersion = "v3"
	containerIDBytes       = 32
	volumeIDBytes          = 32
	// DefaultLogDriver is the persistent logging policy used when none is specified.
	DefaultLogDriver = "local"
)

// Store owns persistent image roots and generated container bundles.
type Store struct {
	Root      string
	Address   string
	Namespace string
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
}

// Prepared is a generated OCI bundle ready for runsc.
type Prepared struct {
	BundleDir string
	RootFS    string
	Image     images.RootFSInfo
	Container ContainerInfo
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

// ContainerInfo is the persistent configuration needed to start a container.
type ContainerInfo struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Reference          string            `json:"reference"`
	Digest             string            `json:"digest"`
	Platform           string            `json:"platform"`
	BundleDir          string            `json:"bundle_dir"`
	RootFS             string            `json:"rootfs"`
	Volumes            []ManagedVolume   `json:"volumes,omitempty"`
	Branches           []BranchInfo      `json:"branches,omitempty"`
	NetworkMode        string            `json:"network_mode,omitempty"`
	LogDriver          string            `json:"log_driver,omitempty"`
	LogOptions         map[string]string `json:"log_options,omitempty"`
	LogAttributes      map[string]string `json:"log_attributes,omitempty"`
	ConsoleLogMaxBytes int64             `json:"console_log_max_bytes,omitempty"`
	ConsoleLogMaxFiles int               `json:"console_log_max_files,omitempty"`
	Terminal           bool              `json:"terminal,omitempty"`
	OpenStdin          bool              `json:"open_stdin,omitempty"`
	AutoRemove         bool              `json:"auto_remove,omitempty"`
	StopSignal         string            `json:"stop_signal,omitempty"`
	StopTimeout        *int              `json:"stop_timeout,omitempty"`
	PidsLimit          *int64            `json:"pids_limit,omitempty"`
	ShmSize            *int64            `json:"shm_size,omitempty"`
	GroupAdd           []string          `json:"group_add,omitempty"`
	CapAdd             []string          `json:"cap_add,omitempty"`
	CapDrop            []string          `json:"cap_drop,omitempty"`
	Ulimits            []Ulimit          `json:"ulimits,omitempty"`
	ExtraHosts         []string          `json:"extra_hosts,omitempty"`
	DNS                []string          `json:"dns,omitempty"`
	DNSSearch          []string          `json:"dns_search,omitempty"`
	DNSOptions         []string          `json:"dns_options,omitempty"`
	State              string            `json:"state"`
	ExitStatus         *int              `json:"exit_status,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	StartedAt          *time.Time        `json:"started_at,omitempty"`
	ExitedAt           *time.Time        `json:"exited_at,omitempty"`
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
	hostDNS, hostSearch, hostOptions := hostResolverConfig(hostData)
	if len(dns) == 0 {
		dns = hostDNS
	}
	if len(dns) == 0 {
		dns = []string{"127.0.0.1"}
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
		{name: "resolv.conf", destination: "/etc/resolv.conf", contents: buildResolvConf(hostResolv, opts.DNS, opts.DNSSearch, opts.DNSOptions)},
	}
	mounts := make([]HostMount, 0, len(files))
	for _, file := range files {
		name := filepath.Join(networkDir, file.name)
		if err := os.WriteFile(name, []byte(file.contents), 0444); err != nil {
			return nil, fmt.Errorf("writing container network file %q: %w", file.destination, err)
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

// VolumeInfo is persistent metadata for a family-local managed volume. Volume
// contents live in Sentry tmpfs and are not checkpointed to this store.
type VolumeInfo struct {
	Name      string    `json:"name"`
	Driver    string    `json:"driver"`
	CreatedAt time.Time `json:"created_at"`
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
		if value := spec.Annotations["dev.timetravelos.config.labels"]; value != "" {
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
	if opts.PidsLimit != nil && *opts.PidsLimit < -1 {
		return Prepared{}, fmt.Errorf("pids limit must be -1 or greater")
	}
	networkMode, err := normalizeNetworkMode(opts.NetworkMode)
	if err != nil {
		return Prepared{}, err
	}
	opts.NetworkMode = networkMode
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
	containers, err := s.ListContainers()
	if err != nil {
		return Prepared{}, err
	}
	for _, existing := range containers {
		if existing.Name == opts.Name || existing.ID == opts.Name {
			return Prepared{}, fmt.Errorf("container name %q is already in use by %s", opts.Name, existing.ID)
		}
	}
	pulled, err := s.resolveImage(ctx, opts.Reference, opts.Platform, opts.PullPolicy)
	if err != nil {
		return Prepared{}, err
	}
	imageInfo, rootfs, err := s.prepareImageRoot(ctx, pulled, opts.Platform)
	if err != nil {
		return Prepared{}, err
	}
	opts.ManagedVolumes, err = s.resolveManagedVolumes(imageInfo.Image, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	id, containerDir, err := s.createContainerDir(containers)
	if err != nil {
		return Prepared{}, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(containerDir)
		}
	}()
	internalMounts, err := materializeNetworkFiles(containerDir, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	opts.HostMounts = append(opts.HostMounts, internalMounts...)
	opts.SpecOptions.RootFS = rootfs
	spec, err := BuildSpec(imageInfo.Image, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	logAttributes, err := selectLogAttributes(opts.LogOptions, spec)
	if err != nil {
		return Prepared{}, err
	}
	for _, volume := range opts.ManagedVolumes {
		if _, err := s.EnsureVolume(volume.Name); err != nil {
			return Prepared{}, err
		}
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
		ID:                 id,
		Name:               opts.Name,
		Reference:          imageInfo.Reference,
		Digest:             pulled.Digest,
		Platform:           imageInfo.Platform,
		BundleDir:          bundleDir,
		RootFS:             rootfs,
		Volumes:            append([]ManagedVolume(nil), opts.ManagedVolumes...),
		NetworkMode:        networkMode,
		LogDriver:          logDriver,
		LogOptions:         cloneStringMap(opts.LogOptions),
		LogAttributes:      logAttributes,
		ConsoleLogMaxBytes: opts.ConsoleLogMaxBytes,
		ConsoleLogMaxFiles: opts.ConsoleLogMaxFiles,
		Terminal:           opts.Terminal,
		OpenStdin:          opts.OpenStdin,
		AutoRemove:         opts.AutoRemove,
		StopSignal:         spec.Annotations[StopSignalAnnotation],
		StopTimeout:        cloneIntPointer(opts.StopTimeout),
		PidsLimit:          cloneInt64Pointer(opts.PidsLimit),
		ShmSize:            cloneInt64Pointer(&shmSize),
		GroupAdd:           append([]string(nil), opts.GroupAdd...),
		CapAdd:             append([]string(nil), opts.CapAdd...),
		CapDrop:            append([]string(nil), opts.CapDrop...),
		Ulimits:            append([]Ulimit(nil), opts.Ulimits...),
		ExtraHosts:         append([]string(nil), opts.ExtraHosts...),
		DNS:                append([]string(nil), opts.DNS...),
		DNSSearch:          append([]string(nil), opts.DNSSearch...),
		DNSOptions:         append([]string(nil), opts.DNSOptions...),
		State:              "created",
		CreatedAt:          time.Now().UTC(),
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

func randomObjectID(kind string, size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generating %s ID: %w", kind, err)
	}
	return hex.EncodeToString(data), nil
}

func (s Store) resolveManagedVolumes(image ocispec.Image, opts SpecOptions) ([]ManagedVolume, error) {
	volumes := append([]ManagedVolume(nil), opts.ManagedVolumes...)
	occupied := map[string]bool{
		"/proc": true, "/dev": true, "/dev/pts": true, "/dev/shm": true, "/sys": true,
	}
	for _, mount := range opts.HostMounts {
		occupied[filepath.Clean(mount.Destination)] = true
	}
	for _, mount := range opts.ManagedVolumes {
		occupied[filepath.Clean(mount.Destination)] = true
	}
	for _, mount := range opts.FilesystemMounts {
		occupied[filepath.Clean(mount.Destination)] = true
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
		if occupied[clean] {
			continue
		}
		occupied[clean] = true
		volumes = append(volumes, ManagedVolume{Destination: clean, Anonymous: true})
	}

	known := make(map[string]bool)
	if existing, err := s.ListVolumes(); err != nil {
		return nil, err
	} else {
		for _, volume := range existing {
			known[volume.Name] = true
		}
	}
	for i := range volumes {
		if volumes[i].Name != "" {
			continue
		}
		volumes[i].Anonymous = true
		for attempt := 0; attempt < 100; attempt++ {
			name, err := randomObjectID("volume", volumeIDBytes)
			if err != nil {
				return nil, err
			}
			if !known[name] {
				volumes[i].Name = name
				known[name] = true
				break
			}
		}
		if volumes[i].Name == "" {
			return nil, errors.New("could not allocate a unique volume ID")
		}
	}
	return volumes, nil
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

// LoadContainer resolves an exact name, full ID, or unambiguous ID prefix.
// Legacy name-keyed records are returned with ID set to their directory key.
func (s Store) LoadContainer(identifier string) (ContainerInfo, error) {
	if err := validateName(identifier); err != nil {
		return ContainerInfo{}, err
	}
	if info, err := s.loadContainerKey(identifier); err == nil {
		return info, nil
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
	networkMode, err := normalizeNetworkMode(info.NetworkMode)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata: %w", key, err)
	}
	info.NetworkMode = networkMode
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
	if info.PidsLimit != nil && *info.PidsLimit < -1 {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid pids limit %d", key, *info.PidsLimit)
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
		containers = append(containers, info)
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Name != containers[j].Name {
			return containers[i].Name < containers[j].Name
		}
		return containers[i].ID < containers[j].ID
	})
	return containers, nil
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

// RecordBranch appends one immutable branch identity or refreshes its lifecycle data.
func (s Store) RecordBranch(name string, branch BranchInfo) error {
	if err := validateObjectName("branch", branch.ID); err != nil {
		return err
	}
	if branch.ID == branch.ParentID {
		return fmt.Errorf("branch %q cannot be its own parent", branch.ID)
	}
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	for i := range info.Branches {
		if info.Branches[i].ID != branch.ID {
			continue
		}
		if info.Branches[i].ParentID != branch.ParentID {
			return fmt.Errorf("branch %q parent changed from %q to %q", branch.ID, info.Branches[i].ParentID, branch.ParentID)
		}
		info.Branches[i] = branch
		return writeJSON(filepath.Join(s.Root, "containers", info.ID, "container.json"), &info)
	}
	info.Branches = append(info.Branches, branch)
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
	if err := os.RemoveAll(filepath.Join(s.Root, "containers", info.ID)); err != nil {
		return fmt.Errorf("removing container %q: %w", name, err)
	}
	return nil
}

// HasImageRoot reports whether TTOS has materialized the immutable root for a
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
		if err := os.RemoveAll(path); err != nil {
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
		RetentionStats: ttosconsole.RetentionStatsPath(base + ".log"),
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
		RetentionStats: ttosconsole.RetentionStatsPath(base + ".log"),
		PID:            base + ".pid",
	}, nil
}

// ListExecConsoleRetentionStats returns non-zero cumulative retention counters by exec ID.
func (s Store) ListExecConsoleRetentionStats(container, branch string) (map[string]ttosconsole.RetentionStats, error) {
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
	statsByExec := make(map[string]ttosconsole.RetentionStats)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		execID := strings.TrimSuffix(entry.Name(), suffix)
		if err := validateObjectName("exec session", execID); err != nil {
			continue
		}
		stats, err := ttosconsole.ReadRetentionStats(filepath.Join(dir, entry.Name()))
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

// CreateVolume creates metadata for a family-local managed tmpfs volume.
func (s Store) CreateVolume(name string) (VolumeInfo, error) {
	if err := validateVolumeName(name); err != nil {
		return VolumeInfo{}, err
	}
	volumesDir := filepath.Join(s.Root, "volumes")
	if err := os.MkdirAll(volumesDir, 0700); err != nil {
		return VolumeInfo{}, fmt.Errorf("creating volume state root: %w", err)
	}
	volumeDir := filepath.Join(volumesDir, name)
	if err := os.Mkdir(volumeDir, 0700); err != nil {
		return VolumeInfo{}, fmt.Errorf("creating volume %q: %w", name, err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(volumeDir)
		}
	}()
	info := VolumeInfo{Name: name, Driver: "local-tmpfs", CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(volumeDir, "volume.json"), &info); err != nil {
		return VolumeInfo{}, fmt.Errorf("writing volume %q metadata: %w", name, err)
	}
	keep = true
	return info, nil
}

// EnsureVolume returns an existing volume or creates it idempotently.
func (s Store) EnsureVolume(name string) (VolumeInfo, error) {
	info, err := s.LoadVolume(name)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return VolumeInfo{}, err
	}
	info, err = s.CreateVolume(name)
	if err == nil {
		return info, nil
	}
	if errors.Is(err, os.ErrExist) {
		return s.LoadVolume(name)
	}
	return VolumeInfo{}, err
}

// LoadVolume returns persistent managed-volume metadata.
func (s Store) LoadVolume(name string) (VolumeInfo, error) {
	if err := validateVolumeName(name); err != nil {
		return VolumeInfo{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.Root, "volumes", name, "volume.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return VolumeInfo{}, fmt.Errorf("volume %q does not exist: %w", name, err)
		}
		return VolumeInfo{}, fmt.Errorf("reading volume %q: %w", name, err)
	}
	var info VolumeInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return VolumeInfo{}, fmt.Errorf("decoding volume %q: %w", name, err)
	}
	if info.Name != name || info.Driver != "local-tmpfs" {
		return VolumeInfo{}, fmt.Errorf("volume %q has invalid metadata", name)
	}
	return info, nil
}

// ListVolumes returns managed volumes ordered by name.
func (s Store) ListVolumes() ([]VolumeInfo, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "volumes"))
	if err != nil {
		if os.IsNotExist(err) {
			return []VolumeInfo{}, nil
		}
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	volumes := make([]VolumeInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := s.LoadVolume(entry.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, info)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, nil
}

// RemoveVolume removes an unused managed volume's metadata.
func (s Store) RemoveVolume(name string) error {
	if _, err := s.LoadVolume(name); err != nil {
		return err
	}
	containers, err := s.ListContainers()
	if err != nil {
		return err
	}
	for _, container := range containers {
		for _, volume := range container.Volumes {
			if volume.Name == name {
				return fmt.Errorf("volume %q is used by container %q", name, container.Name)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(s.Root, "volumes", name)); err != nil {
		return fmt.Errorf("removing volume %q: %w", name, err)
	}
	return nil
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

func (s Store) resolveImage(ctx context.Context, reference, platform, policy string) (images.Info, error) {
	policy, err := normalizePullPolicy(policy)
	if err != nil {
		return images.Info{}, err
	}
	if policy != "always" {
		stored, err := images.List(ctx, images.ListOptions{Address: s.Address, Namespace: s.Namespace})
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
		Address:   s.Address,
		Namespace: s.Namespace,
		Reference: reference,
		Platform:  platform,
		Unpack:    false,
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

func (s Store) prepareImageRoot(ctx context.Context, pulled images.Info, platform string) (images.RootFSInfo, string, error) {
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
	if info, err := loadImageRoot(finalDir); err == nil {
		return info, filepath.Join(finalDir, "rootfs"), nil
	}
	tempDir, err := os.MkdirTemp(imagesDir, key+".tmp-")
	if err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("creating temporary image root: %w", err)
	}
	defer os.RemoveAll(tempDir)
	rootfs := filepath.Join(tempDir, "rootfs")
	info, err := images.MaterializeRootFS(ctx, images.RootFSOptions{
		Address:   s.Address,
		Namespace: s.Namespace,
		Reference: pulled.Reference,
		Platform:  platform,
		Root:      rootfs,
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

func validateVolumeName(name string) error {
	return validateObjectName("volume", name)
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
