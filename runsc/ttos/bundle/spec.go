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

// Package bundle materializes image roots and generates OCI bundles for ttos.
package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	units "github.com/docker/go-units"
	mobysignal "github.com/moby/sys/signal"
	mobyuser "github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/ttos/ownership"
)

const (
	defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// TmpfsConfigAnnotation records Docker --tmpfs inspect configuration.
	TmpfsConfigAnnotation = "dev.timetravelos.config.tmpfs"
	// StopSignalAnnotation records the effective image or CLI stop signal.
	StopSignalAnnotation = "dev.timetravelos.config.stop-signal"
	// PidsLimitAnnotation enables TTOS Sentry-side enforcement of linux.resources.pids.
	PidsLimitAnnotation = "dev.timetravelos.resource.pids-limit"

	// DefaultStopSignal matches Docker's default graceful-termination signal.
	DefaultStopSignal = "SIGTERM"
	// DefaultStopTimeoutSeconds matches Docker's Linux default stop timeout.
	DefaultStopTimeoutSeconds = 10
)

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
	Name             string
	RootFS           string
	Args             []string
	Env              []string
	Labels           map[string]string
	Cwd              string
	User             string
	Hostname         string
	Terminal         bool
	NetworkMode      string
	ReadonlyRootfs   bool
	StopSignal       string
	PidsLimit        *int64
	Entrypoint       []string
	HostMounts       []HostMount
	ManagedVolumes   []ManagedVolume
	FilesystemMounts []FilesystemMount
	Ulimits          []Ulimit
}

// HostMount describes a host path exposed through a read-only gofer bind.
// Writable host paths require a managed CoW volume and are intentionally not
// represented by this type yet.
type HostMount struct {
	Source      string
	Destination string
	Readonly    bool
}

// ManagedVolume describes a family-local Sentry tmpfs. Repeated attachments of
// the same name resolve to one filesystem inside each branch.
type ManagedVolume struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Readonly    bool   `json:"readonly,omitempty"`
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

func buildRlimits(configured []Ulimit) ([]specs.POSIXRlimit, error) {
	byName := map[string]specs.POSIXRlimit{
		"nofile": {Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
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

// BuildSpec combines an OCI image configuration with ttos run overrides.
func BuildSpec(image ocispec.Image, opts SpecOptions) (*specs.Spec, error) {
	if opts.PidsLimit != nil && *opts.PidsLimit < -1 {
		return nil, fmt.Errorf("pids limit must be -1 or greater")
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
	resolvedUser, err := mobyuser.GetExecUserPath(userSpec, &mobyuser.ExecUser{}, filepath.Join(rootfs, "etc/passwd"), filepath.Join(rootfs, "etc/group"))
	if err != nil {
		return nil, fmt.Errorf("resolving user %q: %w", userSpec, err)
	}
	additionalGIDs := make([]uint32, 0, len(resolvedUser.Sgids))
	for _, gid := range resolvedUser.Sgids {
		additionalGIDs = append(additionalGIDs, uint32(gid))
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = opts.Name
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
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
		"dev.gvisor.spec.rootfs.source":     rootfs,
		"dev.gvisor.spec.rootfs.type":       "bind",
		"dev.gvisor.spec.rootfs.overlay":    "memory",
		"dev.timetravelos.internal-console": "init",
		"dev.timetravelos.config.network":   networkMode,
	}
	var resources *specs.LinuxResources
	if opts.PidsLimit != nil && *opts.PidsLimit > 0 {
		resources = &specs.LinuxResources{Pids: &specs.LinuxPids{Limit: *opts.PidsLimit}}
		annotations[PidsLimitAnnotation] = "sentry"
	}
	ownerManifest := ownership.ManifestPath(rootfs)
	if _, err := ownership.Read(ownerManifest); err == nil {
		annotations[ownership.Annotation] = ownerManifest
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading rootfs ownership: %w", err)
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
	annotations["dev.timetravelos.config.entrypoint"] = string(entrypointJSON)
	annotations["dev.timetravelos.config.cmd"] = string(commandJSON)
	annotations["dev.timetravelos.config.user"] = userSpec
	annotations["dev.timetravelos.config.labels"] = string(labelsJSON)
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
	seenDestinations := make(map[string]struct{}, len(mounts)+len(opts.HostMounts)+len(opts.ManagedVolumes)+len(opts.FilesystemMounts))
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
	for _, mount := range opts.HostMounts {
		if !mount.Readonly {
			return nil, fmt.Errorf("host mount %q at %q is writable; use a managed CoW volume", mount.Source, mount.Destination)
		}
		if !filepath.IsAbs(mount.Source) {
			return nil, fmt.Errorf("host mount source %q is not absolute", mount.Source)
		}
		if info, err := os.Stat(mount.Source); err != nil {
			return nil, fmt.Errorf("stating host mount source %q: %w", mount.Source, err)
		} else if info.Mode()&os.ModeSocket != 0 || info.Mode()&os.ModeNamedPipe != 0 || info.Mode()&os.ModeDevice != 0 {
			return nil, fmt.Errorf("host mount source %q is not a regular file or directory", mount.Source)
		}
		destination := filepath.Clean(mount.Destination)
		if !filepath.IsAbs(destination) || destination == "/" {
			return nil, fmt.Errorf("host mount destination %q must be an absolute non-root path", mount.Destination)
		}
		if _, ok := seenDestinations[destination]; ok {
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		seenDestinations[destination] = struct{}{}
		mounts = append(mounts, specs.Mount{
			Destination: destination,
			Type:        "bind",
			Source:      filepath.Clean(mount.Source),
			Options:     []string{"rbind", "ro"},
		})
	}
	volumeSources := make(map[string]string)
	for _, volume := range opts.ManagedVolumes {
		if err := validateVolumeName(volume.Name); err != nil {
			return nil, err
		}
		destination := filepath.Clean(volume.Destination)
		if !filepath.IsAbs(destination) || destination == "/" {
			return nil, fmt.Errorf("managed volume destination %q must be an absolute non-root path", volume.Destination)
		}
		if _, ok := seenDestinations[destination]; ok {
			return nil, fmt.Errorf("duplicate mount destination %q", destination)
		}
		seenDestinations[destination] = struct{}{}
		source, ok := volumeSources[volume.Name]
		if !ok {
			source = "ttos-volume-" + volume.Name
			volumeSources[volume.Name] = source
			hint := fmt.Sprintf("dev.gvisor.spec.mount.ttos%d.", len(volumeSources)-1)
			annotations[hint+"source"] = source
			annotations[hint+"share"] = "container"
			annotations[hint+"type"] = "tmpfs"
			annotations[hint+"options"] = "rw,nosuid,nodev,mode=0755"
		}
		mountOptions := []string{"rw", "nosuid", "nodev", "mode=0755"}
		if volume.Readonly {
			mountOptions[0] = "ro"
		}
		mounts = append(mounts, specs.Mount{Destination: destination, Type: "tmpfs", Source: source, Options: mountOptions})
	}

	return &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path:     rootfs,
			Readonly: opts.ReadonlyRootfs,
		},
		Process: &specs.Process{
			Terminal: opts.Terminal,
			User: specs.User{
				UID:            uint32(resolvedUser.Uid),
				GID:            uint32(resolvedUser.Gid),
				AdditionalGids: additionalGIDs,
			},
			Args: args,
			Env:  env,
			Cwd:  cwd,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  append([]string(nil), defaultCapabilities...),
				Effective: append([]string(nil), defaultCapabilities...),
				Permitted: append([]string(nil), defaultCapabilities...),
			},
			Rlimits: rlimits,
		},
		Hostname:    hostname,
		Mounts:      mounts,
		Linux:       &specs.Linux{Namespaces: namespaces, Resources: resources},
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
	case "none", "host":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q; supported modes are none and host", mode)
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
