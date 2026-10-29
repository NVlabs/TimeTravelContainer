// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle materializes image roots and generates OCI bundles for ttc.
package bundle

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	units "github.com/docker/go-units"
	mobysignal "github.com/moby/sys/signal"
	mobyuser "github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

const (
	defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// TmpfsConfigAnnotation records Docker --tmpfs inspect configuration.
	TmpfsConfigAnnotation = "dev.timetravelcontainer.config.tmpfs"
	// StopSignalAnnotation records the effective image or CLI stop signal.
	StopSignalAnnotation = "dev.timetravelcontainer.config.stop-signal"
	// PidsLimitAnnotation enables TTC Sentry-side enforcement of linux.resources.pids.
	PidsLimitAnnotation = "dev.timetravelcontainer.resource.pids-limit"
	// InternalMountsAnnotation identifies TTC-generated implementation mounts.
	InternalMountsAnnotation = specutils.AnnotationTTCInternalMounts
	// ImmutableHostMountsAnnotation records user-attested immutable host sources.
	ImmutableHostMountsAnnotation = "dev.timetravelcontainer.config.immutable-host-mounts"
	// ManagedCoWHostMountsAnnotation records host binds with branch-local
	// persistent ttcvol uppers.
	ManagedCoWHostMountsAnnotation = "dev.timetravelcontainer.config.managed-cow-host-mounts"
	// CopiedHostMountsAnnotation maps detached TTC-volume mount destinations to
	// the host paths from which they were imported.
	CopiedHostMountsAnnotation = "dev.timetravelcontainer.config.copied-host-mounts"
	// DirectHostMountsAnnotation records the destinations of host binds that
	// are writable binds of the host paths themselves.
	DirectHostMountsAnnotation = specutils.AnnotationTTCDirectHostMounts

	// DefaultStopSignal matches Docker's default graceful-termination signal.
	DefaultStopSignal = "SIGTERM"
	// DefaultStopTimeoutSeconds matches Docker's Linux default stop timeout.
	DefaultStopTimeoutSeconds = 10
	// DefaultShmSizeBytes matches Docker's default /dev/shm size.
	DefaultShmSizeBytes int64 = 64 * 1024 * 1024
	// MinimumMemoryLimitBytes matches Docker's Linux minimum hard memory limit.
	MinimumMemoryLimitBytes int64 = 6 * 1024 * 1024
)

// AnnotationValue reads a TimeTravelContainer annotation.
func AnnotationValue(annotations map[string]string, key string) string {
	return annotations[key]
}

var defaultCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// SpecOptions contains user-facing overrides for an image's OCI configuration.
type SpecOptions struct {
	Name         string
	RootFS       string
	Args         []string
	Env          []string
	Labels       map[string]string
	Cwd          string
	User         string
	Hostname     string
	Terminal     bool
	TerminalType string
	ConsoleSize  *specs.Box
	NetworkMode  string
	// WaiveLiveFork explicitly opts the family out of TTC live-fork eligibility.
	// The zero value preserves TTC's default eligibility guarantee.
	WaiveLiveFork    bool
	ReadonlyRootfs   bool
	StopSignal       string
	PidsLimit        *int64
	MemoryLimit      *int64
	MemorySwap       *int64
	NanoCPUs         *int64
	CPUShares        *uint64
	CPUPeriod        *uint64
	CPUQuota         *int64
	CgroupsPath      string
	ShmSize          *int64
	GroupAdd         []string
	CapAdd           []string
	CapDrop          []string
	ExtraHosts       []string
	DNS              []string
	DNSSearch        []string
	DNSOptions       []string
	Entrypoint       []string
	HostMounts       []HostMount
	FilesystemMounts []FilesystemMount
	ManagedVolumes   []ManagedVolume
	// RootfsUpperRepositoryPath identifies the family-owned ttcvol repository
	// used as the root OverlayFS upper. It is runtime-internal and never
	// exposed in the guest namespace.
	RootfsUpperRepositoryPath string
	RootfsUpperCopyMode       string
	Ulimits                   []Ulimit
}

// ManagedVolume is one persistent TTC volume attachment. RepositoryPath is
// runtime-internal and is donated by directory FD; it is not guest-visible.
type ManagedVolume struct {
	Name           string `json:"name"`
	ID             string `json:"id"`
	Destination    string `json:"destination"`
	Readonly       bool   `json:"readonly,omitempty"`
	Anonymous      bool   `json:"anonymous,omitempty"`
	NoCopy         bool   `json:"no_copy,omitempty"`
	RepositoryPath string `json:"-"`
	// RepositoryBranch is the physical branch mounted by this family's
	// initial branch. Read-only attachments use an isolated snapshot rather
	// than sharing a mutable repository pointer with another Sentry.
	RepositoryBranch string `json:"-"`
	CopyMode         string `json:"-"`
}

// HostMount describes a host path exposed through a read-only gofer bind, a
// persistent CoW overlay, a detached copy in a private ttcvol repository, or,
// if Direct, a writable gofer bind of the path itself, as Docker binds one,
// which only a family that waives live forks may have.
type HostMount struct {
	Source          string
	Destination     string
	Readonly        bool
	Internal        bool
	ImmutableSource bool
	ManagedCoW      bool
	Copy            bool
	Direct          bool
	RepositoryID    string
	RepositoryPath  string
	CopyMode        string
}

// FilesystemMount describes a Sentry-implemented tmpfs, procfs, or sysfs mount.
type FilesystemMount struct {
	Type           string   `json:"type"`
	Destination    string   `json:"destination"`
	Readonly       bool     `json:"readonly,omitempty"`
	Options        []string `json:"options,omitempty"`
	HostConfig     bool     `json:"host_config,omitempty"`
	InspectOptions string   `json:"inspect_options,omitempty"`
}

// Ulimit is the Docker inspect representation of one configured resource limit.
type Ulimit struct {
	Name string `json:"Name"`
	Hard int64  `json:"Hard"`
	Soft int64  `json:"Soft"`
}

var ulimitResourceNames = map[string]string{
	"core": "RLIMIT_CORE", "cpu": "RLIMIT_CPU", "data": "RLIMIT_DATA",
	"fsize": "RLIMIT_FSIZE", "locks": "RLIMIT_LOCKS", "memlock": "RLIMIT_MEMLOCK",
	"msgqueue": "RLIMIT_MSGQUEUE", "nice": "RLIMIT_NICE", "nofile": "RLIMIT_NOFILE",
	"nproc": "RLIMIT_NPROC", "rss": "RLIMIT_RSS", "rtprio": "RLIMIT_RTPRIO",
	"rttime": "RLIMIT_RTTIME", "sigpending": "RLIMIT_SIGPENDING", "stack": "RLIMIT_STACK",
}

// ParseUlimits applies Docker parsing and last-value-wins semantics.
func ParseUlimits(values []string) ([]Ulimit, error) {
	byName := make(map[string]Ulimit, len(values))
	for _, value := range values {
		parsed, err := units.ParseUlimit(value)
		if err != nil {
			return nil, err
		}
		byName[parsed.Name] = Ulimit{Name: parsed.Name, Hard: parsed.Hard, Soft: parsed.Soft}
	}
	result := make([]Ulimit, 0, len(byName))
	for _, limit := range byName {
		result = append(result, limit)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// ParseExtraHosts validates and canonicalizes Docker --add-host entries.
func ParseExtraHosts(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		host, address, ok := strings.Cut(value, "=")
		if !ok {
			host, address, ok = strings.Cut(value, ":")
		}
		if !ok || host == "" || strings.ContainsAny(host, " \t\r\n:") {
			return nil, fmt.Errorf("invalid add-host value %q; expected HOST:IP", value)
		}
		address = strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
		if address == "host-gateway" {
			return nil, fmt.Errorf("add-host value %q uses host-gateway, which requires an unsupported bridge gateway", value)
		}
		ip, err := netip.ParseAddr(address)
		if err != nil {
			return nil, fmt.Errorf("invalid IP address in add-host value %q: %w", value, err)
		}
		result = append(result, host+":"+ip.String())
	}
	return result, nil
}

// ParseDNS validates and canonicalizes Docker --dns addresses.
func ParseDNS(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		ip, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("invalid DNS address %q: %w", value, err)
		}
		result = append(result, ip.String())
	}
	return result, nil
}

func validDNSDomain(value string) bool {
	if value == "." {
		return true
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

// ParseDNSSearch validates Docker --dns-search domains.
func ParseDNSSearch(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !validDNSDomain(value) {
			return nil, fmt.Errorf("invalid DNS search domain %q", value)
		}
		result = append(result, value)
	}
	return result, nil
}

// ParseDNSOptions rejects resolver options that would inject extra lines.
func ParseDNSOptions(values []string) ([]string, error) {
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, " \t\r\n") {
			return nil, fmt.Errorf("invalid DNS option %q", value)
		}
	}
	return append([]string(nil), values...), nil
}

// NormalizeCapabilities applies Docker capability spelling and validation.
// Returned names omit the CAP_ prefix, matching Docker HostConfig inspect.
func NormalizeCapabilities(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToUpper(value)
		if normalized == "ALL" {
			result = append(result, normalized)
			continue
		}
		if !strings.HasPrefix(normalized, "CAP_") {
			normalized = "CAP_" + normalized
		}
		if _, ok := linux.CapabilityFromString(normalized); !ok {
			return nil, fmt.Errorf("unknown capability: %q", normalized)
		}
		result = append(result, strings.TrimPrefix(normalized, "CAP_"))
	}
	return result, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func allCapabilityNames() []string {
	capabilities := linux.AllCapabilities()
	result := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		result = append(result, capability.String())
	}
	return result
}

func buildCapabilities(adds, drops []string) ([]string, error) {
	capAdd, err := NormalizeCapabilities(adds)
	if err != nil {
		return nil, err
	}
	capDrop, err := NormalizeCapabilities(drops)
	if err != nil {
		return nil, err
	}
	if containsString(capAdd, "ALL") {
		result := make([]string, 0, len(allCapabilityNames()))
		for _, capability := range allCapabilityNames() {
			if !containsString(capDrop, strings.TrimPrefix(capability, "CAP_")) {
				result = append(result, capability)
			}
		}
		return result, nil
	}
	if containsString(capDrop, "ALL") {
		result := make([]string, 0, len(capAdd))
		for _, capability := range capAdd {
			result = append(result, "CAP_"+capability)
		}
		return result, nil
	}
	result := make([]string, 0, len(defaultCapabilities)+len(capAdd))
	for _, capability := range defaultCapabilities {
		if !containsString(capDrop, strings.TrimPrefix(capability, "CAP_")) {
			result = append(result, capability)
		}
	}
	for _, capability := range capAdd {
		result = append(result, "CAP_"+capability)
	}
	return result, nil
}

func buildProcessCapabilities(uid uint32, adds, drops []string) (*specs.LinuxCapabilities, error) {
	bounding, err := buildCapabilities(adds, drops)
	if err != nil {
		return nil, err
	}
	effective := bounding
	if uid != 0 {
		effective = nil
	}
	return &specs.LinuxCapabilities{
		Bounding:  append([]string(nil), bounding...),
		Effective: append([]string(nil), effective...),
		Permitted: append([]string(nil), effective...),
	}, nil
}

// ParseShmSize parses the Docker --shm-size byte syntax.
func ParseShmSize(value string) (int64, error) {
	bytes, err := units.RAMInBytes(value)
	if err != nil {
		return 0, fmt.Errorf("invalid shared memory size %q: %w", value, err)
	}
	if bytes < 0 {
		return 0, fmt.Errorf("shared memory size cannot be negative: %d", bytes)
	}
	return bytes, nil
}

// ParseMemoryLimit parses Docker's --memory byte syntax.
func ParseMemoryLimit(value string) (int64, error) {
	bytes, err := units.RAMInBytes(value)
	if err != nil {
		return 0, fmt.Errorf("invalid memory limit %q: %w", value, err)
	}
	if bytes < 0 {
		return 0, fmt.Errorf("memory limit cannot be negative: %d", bytes)
	}
	if bytes != 0 && bytes < MinimumMemoryLimitBytes {
		return 0, fmt.Errorf("minimum memory limit allowed is 6 MiB")
	}
	return bytes, nil
}

// ParseMemorySwap parses Docker's --memory-swap: a byte count for memory and
// swap together, or -1 for unlimited swap.
func ParseMemorySwap(value string) (int64, error) {
	if value == "-1" {
		return -1, nil
	}
	bytes, err := units.RAMInBytes(value)
	if err != nil {
		return 0, fmt.Errorf("invalid memory-swap limit %q: %w", value, err)
	}
	if bytes < 0 {
		return 0, fmt.Errorf("memory-swap limit must be -1 or greater: %d", bytes)
	}
	return bytes, nil
}

// EffectiveMemorySwap is the memory-swap limit Docker gives a container with
// a memory limit: the one asked for, or when none was, twice the memory
// limit, which lets it swap out as much as it may hold.
func EffectiveMemorySwap(memory, swap int64) int64 {
	if memory > 0 && swap == 0 {
		return 2 * memory
	}
	return swap
}

func hasHardResourceLimits(opts SpecOptions) bool {
	return opts.MemoryLimit != nil && *opts.MemoryLimit > 0 ||
		opts.NanoCPUs != nil && *opts.NanoCPUs > 0 ||
		opts.CPUShares != nil && *opts.CPUShares > 0 ||
		opts.CPUPeriod != nil && *opts.CPUPeriod > 0 ||
		opts.CPUQuota != nil && *opts.CPUQuota != 0
}

func validateResourceLimits(opts SpecOptions) error {
	if opts.PidsLimit != nil && *opts.PidsLimit < -1 {
		return fmt.Errorf("pids limit must be -1 or greater")
	}
	if opts.MemoryLimit != nil {
		if *opts.MemoryLimit < 0 {
			return fmt.Errorf("memory limit must be zero or greater")
		}
		if *opts.MemoryLimit != 0 && *opts.MemoryLimit < MinimumMemoryLimitBytes {
			return fmt.Errorf("minimum memory limit allowed is 6 MiB")
		}
	}
	if opts.MemorySwap != nil && *opts.MemorySwap != 0 {
		switch {
		case *opts.MemorySwap < -1:
			return fmt.Errorf("memory-swap limit must be -1 or greater")
		case opts.MemoryLimit == nil || *opts.MemoryLimit == 0:
			return fmt.Errorf("a memory-swap limit needs a memory limit")
		case *opts.MemorySwap > 0 && *opts.MemorySwap < *opts.MemoryLimit:
			return fmt.Errorf("the memory-swap limit must be at least the memory limit")
		}
	}
	if opts.NanoCPUs != nil {
		if *opts.NanoCPUs < 0 {
			return fmt.Errorf("NanoCPUs must be zero or greater")
		}
		if *opts.NanoCPUs != 0 && *opts.NanoCPUs < 10_000_000 {
			return fmt.Errorf("CPU limit is below Docker minimum 0.01")
		}
		if opts.CPUPeriod != nil || opts.CPUQuota != nil {
			return fmt.Errorf("NanoCPUs cannot be combined with CPU period or quota")
		}
	}
	if opts.CPUShares != nil && *opts.CPUShares != 0 && (*opts.CPUShares < 2 || *opts.CPUShares > 262144) {
		return fmt.Errorf("CPU shares must be between 2 and 262144")
	}
	if opts.CPUPeriod != nil && *opts.CPUPeriod != 0 && (*opts.CPUPeriod < 1000 || *opts.CPUPeriod > 1000000) {
		return fmt.Errorf("CPU period must be between 1000 and 1000000 microseconds")
	}
	if opts.CPUQuota != nil && *opts.CPUQuota != 0 && *opts.CPUQuota != -1 && *opts.CPUQuota < 1000 {
		return fmt.Errorf("CPU quota must be -1 or at least 1000 microseconds")
	}
	return nil
}

func effectiveShmSize(value *int64) (int64, error) {
	if value == nil {
		return DefaultShmSizeBytes, nil
	}
	if *value < 0 {
		return 0, fmt.Errorf("shared memory size cannot be negative: %d", *value)
	}
	return *value, nil
}

// Default open-file limits: Docker's on a systemd host, where containers
// inherit the daemon's limits, systemd's 1024 soft and 524288 hard (Docker
// 25 dropped LimitNOFILE=infinity from its units). A hard limit of 1024 kept
// programs that raise their soft limit (Go's runtime, databases, stress-ng)
// from going past 1024 files. Fixed rather than inherited, so a container's
// limits are the same on every node its branches run on.
const (
	defaultNofileSoft = 1024
	defaultNofileHard = 524288
)

func buildRlimits(configured []Ulimit) ([]specs.POSIXRlimit, error) {
	byName := map[string]specs.POSIXRlimit{
		"nofile": {Type: "RLIMIT_NOFILE", Hard: defaultNofileHard, Soft: defaultNofileSoft},
	}
	for _, limit := range configured {
		resource, ok := ulimitResourceNames[limit.Name]
		if !ok {
			return nil, fmt.Errorf("invalid ulimit type: %s", limit.Name)
		}
		byName[limit.Name] = specs.POSIXRlimit{Type: resource, Hard: uint64(limit.Hard), Soft: uint64(limit.Soft)}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]specs.POSIXRlimit, 0, len(names))
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result, nil
}

const maxContainerID = 1<<31 - 1

// validateNumericID rejects syntactically numeric IDs that cannot be
// represented by the user package. Without this check, strconv.Atoi overflow
// in mobyuser.GetExecUser causes an all-decimal user or group argument to be
// treated as a name and looked up in the image's passwd or group file.
func validateNumericID(value, kind string) error {
	if value == "" || !isDecimalInteger(value) {
		return nil
	}
	id, err := strconv.ParseInt(value, 10, 32)
	if err != nil || id < 0 || id > maxContainerID {
		return fmt.Errorf("numeric %s %q must be in range 0-%d", kind, value, maxContainerID)
	}
	return nil
}

func isDecimalInteger(value string) bool {
	if value == "" {
		return false
	}
	firstDigit := 0
	if value[0] == '+' || value[0] == '-' {
		firstDigit = 1
	}
	if firstDigit == len(value) {
		return false
	}
	for i := firstDigit; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validateUserSpecNumericIDs(userSpec string) error {
	user, group, _ := strings.Cut(userSpec, ":")
	if err := validateNumericID(user, "user ID"); err != nil {
		return err
	}
	if err := validateNumericID(group, "group ID"); err != nil {
		return err
	}
	return nil
}

// openImageAccounts opens name, an account file of the image, as the
// container reaches it: its symbolic links resolve inside rootfs, as they
// would in the container, and not on the host. It returns nil, as an image
// without the file reads, if no regular file is there.
func openImageAccounts(rootfs, name string) io.Reader {
	relative, err := resolveInRootfs(rootfs, name)
	if err != nil {
		return nil
	}
	// O_NONBLOCK: a FIFO there must not hold the open until a writer comes.
	file, err := os.OpenFile(filepath.Join(rootfs, relative), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil
	}
	return file
}

func closeImageAccounts(readers ...io.Reader) {
	for _, reader := range readers {
		if file, ok := reader.(*os.File); ok {
			file.Close()
		}
	}
}

// BuildSpec combines an OCI image configuration with ttc run overrides.
func BuildSpec(image ocispec.Image, opts SpecOptions) (*specs.Spec, error) {
	if err := validateResourceLimits(opts); err != nil {
		return nil, err
	}
	rootfs, err := filepath.Abs(opts.RootFS)
	if err != nil {
		return nil, fmt.Errorf("resolving rootfs path: %w", err)
	}
	networkMode, err := normalizeNetworkMode(opts.NetworkMode)
	if err != nil {
		return nil, err
	}
	cmd := image.Config.Cmd
	if len(opts.Args) != 0 {
		cmd = opts.Args
	}
	entrypoint := image.Config.Entrypoint
	if opts.Entrypoint != nil {
		entrypoint = opts.Entrypoint
	}
	args := append(append([]string(nil), entrypoint...), cmd...)
	if len(args) == 0 {
		return nil, fmt.Errorf("image and command override specify no command")
	}
	env, err := mergeEnv(image.Config.Env, opts.Env)
	if err != nil {
		return nil, err
	}
	if len(env) == 0 {
		env = []string{"PATH=" + defaultPath}
	}
	if opts.Terminal && !envHasKey(env, "TERM") {
		terminalType := opts.TerminalType
		if terminalType == "" {
			terminalType = "xterm"
		}
		env = append(env, "TERM="+terminalType)
	}
	cwd := image.Config.WorkingDir
	if opts.Cwd != "" {
		cwd = opts.Cwd
	}
	if cwd == "" {
		cwd = "/"
	}
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("working directory %q is not absolute", cwd)
	}
	userSpec := image.Config.User
	if opts.User != "" {
		userSpec = opts.User
	}
	if err := validateUserSpecNumericIDs(userSpec); err != nil {
		return nil, fmt.Errorf("resolving user %q: %w", userSpec, err)
	}
	passwdAccounts, groupAccounts := openImageAccounts(rootfs, "/etc/passwd"), openImageAccounts(rootfs, "/etc/group")
	resolvedUser, err := mobyuser.GetExecUser(userSpec, &mobyuser.ExecUser{}, passwdAccounts, groupAccounts)
	closeImageAccounts(passwdAccounts, groupAccounts)
	if err != nil {
		return nil, fmt.Errorf("resolving user %q: %w", userSpec, err)
	}
	additionalGIDs := make([]uint32, 0, len(resolvedUser.Sgids))
	for _, gid := range resolvedUser.Sgids {
		additionalGIDs = append(additionalGIDs, uint32(gid))
	}
	seenGIDs := make(map[uint32]struct{}, len(additionalGIDs)+len(opts.GroupAdd))
	for _, gid := range additionalGIDs {
		seenGIDs[gid] = struct{}{}
	}
	for _, group := range opts.GroupAdd {
		if err := validateNumericID(group, "additional group ID"); err != nil {
			return nil, fmt.Errorf("resolving additional group %q: %w", group, err)
		}
		accounts := openImageAccounts(rootfs, "/etc/group")
		gids, err := mobyuser.GetAdditionalGroups([]string{group}, accounts)
		closeImageAccounts(accounts)
		if err != nil {
			return nil, fmt.Errorf("resolving additional group %q: %w", group, err)
		}
		for _, gid := range gids {
			value := uint32(gid)
			if _, ok := seenGIDs[value]; !ok {
				additionalGIDs = append(additionalGIDs, value)
				seenGIDs[value] = struct{}{}
			}
		}
	}
	capabilities, err := buildProcessCapabilities(uint32(resolvedUser.Uid), opts.CapAdd, opts.CapDrop)
	if err != nil {
		return nil, err
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = opts.Name
	}
	shmSize, err := effectiveShmSize(opts.ShmSize)
	if err != nil {
		return nil, err
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", fmt.Sprintf("size=%d", shmSize)}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	namespaces := []specs.LinuxNamespace{
		{Type: specs.PIDNamespace},
		{Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace},
		{Type: specs.MountNamespace},
	}
	if networkMode == "none" {
		namespaces = append(namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace})
	}
	annotations := map[string]string{
		"dev.gvisor.spec.rootfs.source":             rootfs,
		"dev.gvisor.spec.rootfs.type":               "bind",
		"dev.gvisor.spec.rootfs.overlay":            "memory",
		"dev.timetravelcontainer.internal-console":  "init",
		"dev.timetravelcontainer.config.network":    networkMode,
		specutils.AnnotationTTCLiveForkEligible:     strconv.FormatBool(!opts.WaiveLiveFork),
		specutils.ContainerdContainerNameAnnotation: opts.Name,
	}
	var resources *specs.LinuxResources
	if opts.PidsLimit != nil && *opts.PidsLimit > 0 {
		resources = &specs.LinuxResources{Pids: &specs.LinuxPids{Limit: *opts.PidsLimit}}
		annotations[PidsLimitAnnotation] = "sentry"
	}
	if opts.MemoryLimit != nil && *opts.MemoryLimit > 0 {
		if resources == nil {
			resources = &specs.LinuxResources{}
		}
		resources.Memory = &specs.LinuxMemory{Limit: cloneInt64Pointer(opts.MemoryLimit)}
		// Unlimited swap is a new cgroup's own default, and needs no
		// memory.swap.max, which a kernel that does not account swap lacks.
		if opts.MemorySwap != nil && *opts.MemorySwap > 0 {
			resources.Memory.Swap = cloneInt64Pointer(opts.MemorySwap)
		}
	}
	cpu := &specs.LinuxCPU{}
	hasCPU := false
	if opts.CPUShares != nil && *opts.CPUShares > 0 {
		value := *opts.CPUShares
		cpu.Shares = &value
		hasCPU = true
	}
	if opts.NanoCPUs != nil && *opts.NanoCPUs > 0 {
		period := uint64(100000)
		quota := (*opts.NanoCPUs/1_000_000_000)*int64(period) + (*opts.NanoCPUs%1_000_000_000)*int64(period)/1_000_000_000
		cpu.Period = &period
		cpu.Quota = &quota
		hasCPU = true
	}
	if opts.CPUPeriod != nil && *opts.CPUPeriod > 0 {
		value := *opts.CPUPeriod
		cpu.Period = &value
		hasCPU = true
	}
	if opts.CPUQuota != nil && *opts.CPUQuota != 0 {
		value := *opts.CPUQuota
		cpu.Quota = &value
		hasCPU = true
	}
	if hasCPU {
		if resources == nil {
			resources = &specs.LinuxResources{}
		}
		resources.CPU = cpu
	}
	ownerManifest := ownership.ManifestPath(rootfs)
	if err := ownership.Validate(ownerManifest); err == nil {
		annotations[ownership.Annotation] = ownerManifest
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading rootfs ownership: %w", err)
	}
	if _, err := ttcpack.ReadLazyDescriptor(rootfs); err == nil {
		annotations[ttcpack.LazyAnnotation] = ttcpack.LazyDescriptorPath(rootfs)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading lazy rootfs image descriptor: %w", err)
	}
	entrypointJSON, err := json.Marshal(entrypoint)
	if err != nil {
		return nil, fmt.Errorf("encoding effective entrypoint: %w", err)
	}
	commandJSON, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("encoding effective command: %w", err)
	}
	labels := make(map[string]string, len(image.Config.Labels)+len(opts.Labels))
	for key, value := range image.Config.Labels {
		labels[key] = value
	}
	for key, value := range opts.Labels {
		labels[key] = value
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return nil, fmt.Errorf("encoding image labels: %w", err)
	}
	annotations["dev.timetravelcontainer.config.entrypoint"] = string(entrypointJSON)
	annotations["dev.timetravelcontainer.config.cmd"] = string(commandJSON)
	annotations["dev.timetravelcontainer.config.user"] = userSpec
	annotations["dev.timetravelcontainer.config.labels"] = string(labelsJSON)
	rlimits, err := buildRlimits(opts.Ulimits)
	if err != nil {
		return nil, err
	}
	stopSignal := opts.StopSignal
	if stopSignal == "" {
		stopSignal = image.Config.StopSignal
	}
	if stopSignal == "" {
		stopSignal = DefaultStopSignal
	}
	parsedStopSignal, err := mobysignal.ParseSignal(stopSignal)
	if err != nil || !mobysignal.ValidSignalForPlatform(parsedStopSignal) {
		return nil, fmt.Errorf("invalid stop signal %q", stopSignal)
	}
	annotations[StopSignalAnnotation] = stopSignal
	seenDestinations := make(map[string]struct{}, len(mounts)+len(opts.HostMounts)+len(opts.FilesystemMounts))
	for _, mount := range mounts {
		seenDestinations[mount.Destination] = struct{}{}
	}
	dockerTmpfs := make(map[string]string)
	for _, mount := range opts.FilesystemMounts {
		destination := filepath.Clean(mount.Destination)
		if !filepath.IsAbs(destination) || destination == "/" {
			return nil, fmt.Errorf("%s mount destination %q must be an absolute non-root path", mount.Type, mount.Destination)
		}
		if _, ok := seenDestinations[destination]; ok {
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		seenDestinations[destination] = struct{}{}
		mountType := mount.Type
		source := mount.Type
		switch mountType {
		case "tmpfs", "proc", "sysfs":
		default:
			return nil, fmt.Errorf("unsupported filesystem mount type %q", mount.Type)
		}
		if mount.HostConfig {
			if mountType != "tmpfs" {
				return nil, fmt.Errorf("only tmpfs mounts can use Docker HostConfig projection")
			}
			dockerTmpfs[destination] = mount.InspectOptions
		}
		options, err := filesystemMountOptions(mount)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, specs.Mount{
			Destination: destination,
			Type:        mountType,
			Source:      source,
			Options:     options,
		})
	}
	if len(dockerTmpfs) != 0 {
		encoded, err := json.Marshal(dockerTmpfs)
		if err != nil {
			return nil, fmt.Errorf("encoding Docker tmpfs configuration: %w", err)
		}
		annotations[TmpfsConfigAnnotation] = string(encoded)
	}
	volumeRepositories := make([]specutils.TTCVolumeRepository, 0, len(opts.ManagedVolumes)+len(opts.HostMounts)+1)
	if opts.RootfsUpperRepositoryPath != "" || opts.RootfsUpperCopyMode != "" {
		if opts.ReadonlyRootfs {
			return nil, fmt.Errorf("read-only rootfs cannot have a writable upper repository")
		}
		if opts.RootfsUpperRepositoryPath == "" || opts.RootfsUpperCopyMode == "" {
			return nil, fmt.Errorf("rootfs upper repository path and copy mode must be specified together")
		}
		volumeRepositories = append(volumeRepositories, specutils.TTCVolumeRepository{
			Source:   specutils.TTCRootfsUpperSource,
			Path:     opts.RootfsUpperRepositoryPath,
			Branch:   "main",
			CopyMode: opts.RootfsUpperCopyMode,
		})
	}
	for _, volume := range opts.ManagedVolumes {
		destination := filepath.Clean(volume.Destination)
		if !filepath.IsAbs(destination) || destination == "/" {
			return nil, fmt.Errorf("managed volume destination %q must be an absolute non-root path", volume.Destination)
		}
		if _, ok := seenDestinations[destination]; ok {
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		if volume.ID == "" || volume.RepositoryPath == "" || volume.CopyMode == "" {
			return nil, fmt.Errorf("managed volume %q is missing resolved repository metadata", volume.Name)
		}
		seenDestinations[destination] = struct{}{}
		source := filepath.Join("/run/ttc/volumes", volume.ID)
		options := []string{"rw"}
		if volume.Readonly {
			options[0] = "ro"
		}
		repositoryBranch := volume.RepositoryBranch
		if repositoryBranch == "" {
			repositoryBranch = ttcvolume.MainBranch
		}
		mounts = append(mounts, specs.Mount{
			Destination: destination,
			Type:        "ttcvol",
			Source:      source,
			Options:     options,
		})
		volumeRepositories = append(volumeRepositories, specutils.TTCVolumeRepository{
			Source: source, Path: volume.RepositoryPath, Branch: repositoryBranch, CopyMode: volume.CopyMode,
		})
	}
	copiedHostMounts := make(map[string]string)
	directHostMounts := make([]string, 0)
	for _, mount := range opts.HostMounts {
		if !mount.Readonly && !mount.ManagedCoW && !mount.Copy && !mount.Direct {
			return nil, fmt.Errorf("host mount %q at %q is writable without TTC isolation", mount.Source, mount.Destination)
		}
		if mount.Direct && (mount.Readonly || mount.ManagedCoW || mount.Copy || mount.Internal) {
			return nil, fmt.Errorf("direct host mount %q at %q cannot be read-only, internal, or have a TTC layer", mount.Source, mount.Destination)
		}
		if mount.Readonly && (mount.ManagedCoW || mount.Copy) {
			return nil, fmt.Errorf("read-only host mount %q at %q cannot have a writable TTC layer", mount.Source, mount.Destination)
		}
		if mount.ManagedCoW && mount.Copy {
			return nil, fmt.Errorf("host mount %q at %q cannot be both CoW and copied", mount.Source, mount.Destination)
		}
		if !filepath.IsAbs(mount.Source) {
			return nil, fmt.Errorf("host mount source %q is not absolute", mount.Source)
		}
		if !mount.Copy {
			if info, err := os.Stat(mount.Source); err != nil {
				return nil, fmt.Errorf("stating host mount source %q: %w", mount.Source, err)
			} else if info.Mode()&os.ModeSocket != 0 || info.Mode()&os.ModeNamedPipe != 0 || info.Mode()&os.ModeDevice != 0 {
				return nil, fmt.Errorf("host mount source %q is not a regular file or directory", mount.Source)
			}
		}
		destination := filepath.Clean(mount.Destination)
		if !filepath.IsAbs(destination) || destination == "/" {
			return nil, fmt.Errorf("host mount destination %q must be an absolute non-root path", mount.Destination)
		}
		if _, ok := seenDestinations[destination]; ok {
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		seenDestinations[destination] = struct{}{}
		if mount.Direct {
			// Writes reach the host path, through the gofer, with no
			// overlay above it (see runsc's gofer mount configurations).
			mounts = append(mounts, specs.Mount{
				Destination: destination,
				Type:        "bind",
				Source:      filepath.Clean(mount.Source),
				Options:     []string{"rbind", "rw"},
			})
			directHostMounts = append(directHostMounts, destination)
			continue
		}
		if !mount.Readonly {
			if mount.RepositoryID == "" || mount.RepositoryPath == "" || mount.CopyMode == "" {
				return nil, fmt.Errorf("writable host mount %q at %q is missing resolved repository metadata", mount.Source, mount.Destination)
			}
			repositorySource := filepath.Join("/run/ttc/host-mounts", mount.RepositoryID)
			repository := specutils.TTCVolumeRepository{
				Source: repositorySource, Path: mount.RepositoryPath, Branch: "main",
				CopyMode: mount.CopyMode, Private: true,
			}
			if mount.Copy {
				mounts = append(mounts, specs.Mount{
					Destination: destination,
					Type:        "ttcvol",
					Source:      repositorySource,
					Options:     []string{"rw"},
				})
				copiedHostMounts[destination] = filepath.Clean(mount.Source)
			} else {
				repository.OverlayDestination = destination
				mounts = append(mounts, specs.Mount{
					Destination: destination,
					Type:        "bind",
					Source:      filepath.Clean(mount.Source),
					Options:     []string{"rbind", "rw"},
				})
			}
			volumeRepositories = append(volumeRepositories, repository)
			continue
		}
		mounts = append(mounts, specs.Mount{
			Destination: destination,
			Type:        "bind",
			Source:      filepath.Clean(mount.Source),
			Options:     []string{"rbind", "ro"},
		})
	}
	if len(volumeRepositories) != 0 {
		encoded, err := json.Marshal(volumeRepositories)
		if err != nil {
			return nil, fmt.Errorf("encoding TTC volume repositories: %w", err)
		}
		annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
	}
	if len(copiedHostMounts) != 0 {
		encoded, err := json.Marshal(copiedHostMounts)
		if err != nil {
			return nil, fmt.Errorf("encoding copied host mounts: %w", err)
		}
		annotations[CopiedHostMountsAnnotation] = string(encoded)
	}
	if len(directHostMounts) != 0 {
		encoded, err := json.Marshal(directHostMounts)
		if err != nil {
			return nil, fmt.Errorf("encoding direct host mount destinations: %w", err)
		}
		annotations[DirectHostMountsAnnotation] = string(encoded)
	}
	internalMounts := make([]string, 0)
	immutableHostMounts := make([]string, 0)
	managedCoWHostMounts := make([]string, 0)
	for _, mount := range opts.HostMounts {
		if mount.ImmutableSource && !mount.Internal {
			immutableHostMounts = append(immutableHostMounts, filepath.Clean(mount.Destination))
		}
		if mount.ManagedCoW && !mount.Internal {
			managedCoWHostMounts = append(managedCoWHostMounts, filepath.Clean(mount.Destination))
		}
		if mount.Internal {
			internalMounts = append(internalMounts, filepath.Clean(mount.Destination))
		}
	}
	if len(immutableHostMounts) != 0 {
		encoded, err := json.Marshal(immutableHostMounts)
		if err != nil {
			return nil, fmt.Errorf("encoding immutable host mount destinations: %w", err)
		}
		annotations[ImmutableHostMountsAnnotation] = string(encoded)
	}
	if len(managedCoWHostMounts) != 0 {
		encoded, err := json.Marshal(managedCoWHostMounts)
		if err != nil {
			return nil, fmt.Errorf("encoding managed CoW host mount destinations: %w", err)
		}
		annotations[ManagedCoWHostMountsAnnotation] = string(encoded)
	}
	if len(internalMounts) != 0 {
		encoded, err := json.Marshal(internalMounts)
		if err != nil {
			return nil, fmt.Errorf("encoding internal mount destinations: %w", err)
		}
		annotations[InternalMountsAnnotation] = string(encoded)
	}
	for destination := range image.Config.Volumes {
		clean := filepath.Clean(destination)
		if !filepath.IsAbs(clean) || clean == "/" {
			return nil, fmt.Errorf("image declares invalid volume destination %q", destination)
		}
		if _, explicitlyMounted := seenDestinations[clean]; !explicitlyMounted {
			return nil, fmt.Errorf("image volume %q was not resolved to a TTC managed volume", destination)
		}
	}
	return &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path:     rootfs,
			Readonly: opts.ReadonlyRootfs,
		},
		Process: &specs.Process{
			Terminal:    opts.Terminal,
			ConsoleSize: opts.ConsoleSize,
			User: specs.User{
				UID:            uint32(resolvedUser.Uid),
				GID:            uint32(resolvedUser.Gid),
				AdditionalGids: additionalGIDs,
			},
			Args:         args,
			Env:          env,
			Cwd:          cwd,
			Capabilities: capabilities,
			Rlimits:      rlimits,
		},
		Hostname:    hostname,
		Mounts:      mounts,
		Linux:       &specs.Linux{Namespaces: namespaces, Resources: resources, CgroupsPath: opts.CgroupsPath},
		Annotations: annotations,
	}, nil
}

func filesystemMountOptions(mount FilesystemMount) ([]string, error) {
	options := append([]string(nil), mount.Options...)
	wantAccess := "rw"
	if mount.Readonly {
		wantAccess = "ro"
	}
	accessIndex := -1
	for i, option := range options {
		if option != "ro" && option != "rw" {
			continue
		}
		if accessIndex >= 0 {
			return nil, fmt.Errorf("%s mount at %q specifies multiple access modes", mount.Type, mount.Destination)
		}
		accessIndex = i
	}
	if accessIndex < 0 {
		return append([]string{wantAccess}, options...), nil
	}
	if options[accessIndex] != wantAccess {
		return nil, fmt.Errorf("%s mount at %q has access option %q inconsistent with readonly=%t", mount.Type, mount.Destination, options[accessIndex], mount.Readonly)
	}
	return options, nil
}

func normalizeNetworkMode(mode string) (string, error) {
	if mode == "" {
		mode = "none"
	}
	switch mode {
	case "none", "host", "ttc":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q; supported modes are none, host, and ttc", mode)
	}
}

func mergeEnv(imageEnv, overrides []string) ([]string, error) {
	result := append([]string(nil), imageEnv...)
	positions := make(map[string]int)
	for i, value := range result {
		name, _, ok := strings.Cut(value, "=")
		if ok {
			positions[name] = i
		}
	}
	for _, value := range overrides {
		name, _, hasValue := strings.Cut(value, "=")
		if name == "" {
			return nil, fmt.Errorf("environment override %q has an empty name", value)
		}
		if !hasValue {
			clear(positions)
			kept := result[:0]
			for _, entry := range result {
				entryName, _, _ := strings.Cut(entry, "=")
				if entryName == name {
					continue
				}
				positions[entryName] = len(kept)
				kept = append(kept, entry)
			}
			result = kept
			continue
		}
		if i, ok := positions[name]; ok {
			result[i] = value
		} else {
			positions[name] = len(result)
			result = append(result, value)
		}
	}
	return result, nil
}

// addDefaultEnv returns env with the assignments, NAME=VALUE, of defaults
// whose names env does not assign.
func addDefaultEnv(env, defaults []string) []string {
	for _, assignment := range defaults {
		name, _, _ := strings.Cut(assignment, "=")
		if !envHasKey(env, name) {
			env = append(env, assignment)
		}
	}
	return env
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, assignment := range env {
		if strings.HasPrefix(assignment, prefix) {
			return true
		}
	}
	return false
}
