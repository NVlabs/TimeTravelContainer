// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package specutils contains utility functions for working with OCI runtime
// specs.
package specutils

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/mohae/deepcopy"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-spec/specs-go/features"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// LINT.IfChange(features_annotations)
const (
	// annotationFlagPrefix allows annotations to change the value of flags.
	//
	// Usage:
	//	"dev.gvisor.flag.<flag-name>": "<value>"
	//	"dev.gvisor.flag.debug": "true"
	//
	// Note: Only some flags can be overridden, unless `--allow-flag-override` is set. See
	// `config.overrideAllowlist` for the list of allowed flags.
	annotationFlagPrefix = "dev.gvisor.flag."

	// annotationContainerNameRemap allows the container name to be changed. This is useful during
	// restore when container name has changed after it has been saved.
	//
	// Usage:
	//	Remapping container name from "cont-123" (current) to "cont" (saved):
	//	"dev.gvisor.container-name-remap.<any-unique-id>": "<from>=<to>"
	//	"dev.gvisor.container-name-remap.1": "cont-123=cont"
	annotationContainerNameRemap = "dev.gvisor.container-name-remap."

	// AnnotationRootfsUpperTar specifies the rootfs upper layer tar path.
	// In multi-container pods, append the container name to select a specific
	// container:
	//
	// Usage:
	//	"dev.gvisor.tar.rootfs.upper": "<path>"
	//	"dev.gvisor.tar.rootfs.upper.<container-name>": "<path>"
	AnnotationRootfsUpperTar = "dev.gvisor.tar.rootfs.upper"

	// AnnotationMountTarOnExit maps shared mount sources to pending tar outputs.
	AnnotationMountTarOnExit = "dev.timetravelcontainer.tar.mounts.on-exit"

	// AnnotationTTCVolumeRepositories maps ttcvol mount sources to host
	// repository directories donated to the Sentry.
	AnnotationTTCVolumeRepositories = "dev.timetravelcontainer.volume-repositories"

	// AnnotationTTCInternalMounts lists bind-mount destinations generated and
	// owned by TTC. Portable TTC checkpoints snapshot and rehome these paths;
	// the annotation must never be applied to user-provided host mounts.
	AnnotationTTCInternalMounts = "dev.timetravelcontainer.config.internal-mounts"

	// AnnotationTTCDirectHostMounts lists, as JSON, the destinations of bind
	// mounts that are writable binds of their host paths, whose writes reach
	// the host: they have no overlay, whatever --overlay2 says.
	AnnotationTTCDirectHostMounts = "dev.timetravelcontainer.config.direct-host-mounts"

	// AnnotationTTCMemoryFile identifies obsolete pathname-backed application
	// memory created by older TTC versions. It is retained only so upgraded
	// bundles can discard it safely; runtimes must never open the path.
	AnnotationTTCMemoryFile = "dev.timetravelcontainer.memory-file"

	// AnnotationTTCDistributedTarget asks the Sentry to reserve an anonymous
	// MemoryFile for a subsequently restored distributed-fork Kernel.
	AnnotationTTCDistributedTarget = "dev.timetravelcontainer.distributed-target"

	// AnnotationTTCInitialBranchID, AnnotationTTCInitialBranchParentID, and AnnotationTTCInitialFilesystemBranch
	// select the visible branch identity and persistent filesystem pointer used
	// when TTC starts a previously exited branch as a new sandbox loader.
	AnnotationTTCInitialBranchID         = "dev.timetravelcontainer.initial-branch-id"
	AnnotationTTCInitialBranchParentID   = "dev.timetravelcontainer.initial-branch-parent-id"
	AnnotationTTCInitialFilesystemBranch = "dev.timetravelcontainer.initial-filesystem-branch"
	// AnnotationTTCLiveForkEligible records the immutable family policy chosen
	// by TTC when the container is created.
	AnnotationTTCLiveForkEligible = "dev.timetravelcontainer.live-fork-eligible"
	// AnnotationTTCInitialStartPaused starts a fresh branch in a task-set-wide
	// stop while ttcd restores independently dehydrated peers into its Sentry.
	AnnotationTTCInitialStartPaused = "dev.timetravelcontainer.initial-start-paused"

	// TTCRootfsUpperSource is the reserved, guest-invisible source key for a
	// container family's persistent writable rootfs upper repository.
	TTCRootfsUpperSource = "/run/ttc/rootfs-upper"

	// RootfsMountTarSource is the reserved source key used to export the root
	// filesystem through AnnotationMountTarOnExit.
	RootfsMountTarSource = "dev.timetravelcontainer.rootfs"

	// annotationSeccomp indicates what seccomp rules was set to a given container.
	//
	// Usage:
	//	"dev.gvisor.internal.seccomp.<container-name>": "<value>"
	//	"dev.gvisor.internal.seccomp.cont": "RuntimeDefault"
	annotationSeccomp               = "dev.gvisor.internal.seccomp."
	annotationSeccompRuntimeDefault = "RuntimeDefault"

	// AnnotationTPU is the annotation used to enable TPU proxy on a pod.
	AnnotationTPU = "dev.gvisor.internal.tpuproxy"

	// AnnotationCPUFeatures is the annotation used to control cpu features
	// that exposed to user apps.
	AnnotationCPUFeatures = "dev.gvisor.internal.cpufeatures"

	// AnnotationCPUTopology is the annotation that sets the CPU topology user
	// apps see, as cputopology.Topology.Marshal formats it.
	AnnotationCPUTopology = "dev.gvisor.internal.cputopology"
)

// LINT.ThenChange(:Features)

// MemoryFileTmpfsDir is where, in a sandbox's root, the tmpfs that holds its
// application memory files is mounted, if config.Config.MemoryFileHugeTmpfs
// asks for one.
const MemoryFileTmpfsDir = "/memory-file"

// ExePath must point to runsc binary, which is normally the same binary. It's
// changed in tests that aren't linked in the same binary.
var ExePath = hostExecutablePath()

// Version is the supported spec version.
var Version = specs.Version

var (
	globalFeatures *features.Features
	featuresOnce   sync.Once
)

// LogSpecDebug writes the spec in a human-friendly format to the debug log.
func LogSpecDebug(orig *specs.Spec, logSeccomp bool) {
	if log.IsLogging(log.Debug) {
		LogSpecCustomLogger(orig, logSeccomp, log.Debugf)
	}
}

// LogSpecCustomLogger writes the spec in a human-friendly format to the provided logger.
func LogSpecCustomLogger(orig *specs.Spec, logSeccomp bool, logf func(format string, args ...any)) {
	// Strip down parts of the spec that are not interesting.
	spec := deepcopy.Copy(orig).(*specs.Spec)
	if spec.Linux != nil {
		if !logSeccomp {
			spec.Linux.Seccomp = nil
		}
		spec.Linux.MaskedPaths = nil
		spec.Linux.ReadonlyPaths = nil
		if spec.Linux.Resources != nil {
			spec.Linux.Resources.Devices = nil
		}
	}
	if spec.Process != nil {
		spec.Process.Env = nil
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		logf("Failed to marshal spec: %v", err)
		return
	}
	logf("Spec:\n%s", out)
}

// ValidateSpec validates that the spec is compatible with runsc.
//
// noRootContainer requires the spec to have no Process and no Root.
func ValidateSpec(spec *specs.Spec, conf *config.Config, noRootContainer bool) error {
	// Mandatory fields.
	if noRootContainer {
		if spec.Process != nil {
			return fmt.Errorf("Spec.Process must not be defined with --no-root-container: %+v", spec.Process)
		}
		if spec.Root != nil {
			return fmt.Errorf("Spec.Root must not be defined with --no-root-container: %+v", spec.Root)
		}
	} else {
		if spec.Process == nil {
			return fmt.Errorf("Spec.Process must be defined: %+v", spec)
		}
		if len(spec.Process.Args) == 0 {
			return fmt.Errorf("Spec.Process.Arg must be defined: %+v", spec.Process)
		}
		if spec.Root == nil {
			return fmt.Errorf("Spec.Root must be defined: %+v", spec)
		}
		if len(spec.Root.Path) == 0 {
			return fmt.Errorf("Spec.Root.Path must be defined: %+v", spec.Root)
		}
	}

	// Unsupported fields.
	if spec.Solaris != nil {
		return fmt.Errorf("Spec.Solaris is not supported: %+v", spec)
	}
	if spec.Windows != nil {
		return fmt.Errorf("Spec.Windows is not supported: %+v", spec)
	}
	if spec.Process != nil {
		if len(spec.Process.SelinuxLabel) != 0 {
			return fmt.Errorf("SELinux is not supported: %s", spec.Process.SelinuxLabel)
		}

		// Docker uses AppArmor by default, so just log that it's being ignored.
		if spec.Process.ApparmorProfile != "" {
			log.Warningf("AppArmor profile %q is being ignored", spec.Process.ApparmorProfile)
		}
	}

	if spec.Linux != nil && spec.Linux.RootfsPropagation != "" {
		if err := validateRootfsPropagation(spec.Linux.RootfsPropagation); err != nil {
			return err
		}
	}
	for _, m := range spec.Mounts {
		if err := validateMount(&m); err != nil {
			return err
		}
	}

	// Validate rootfs upper tar annotation.
	if path := RootfsTarUpperPath(spec); path != "" {
		if !conf.AllowRootfsTarAnnotation {
			return fmt.Errorf("rootfs tar annotation is disabled, use --allow-rootfs-tar-annotation to enable it")
		}
		if spec.Root == nil {
			return fmt.Errorf("rootfs tar upper path is set but Spec.Root is not defined")
		}
		if spec.Root.Readonly {
			return fmt.Errorf("rootfs tar upper path is set but rootfs is readonly")
		}
	}

	// CRI specifies whether a container should start a new sandbox, or run
	// another container in an existing sandbox.
	switch SpecContainerType(spec) {
	case ContainerTypeContainer:
		// When starting a container in an existing sandbox, the
		// sandbox ID must be set.
		if _, ok := SandboxID(spec); !ok {
			return fmt.Errorf("spec has container-type of container, but no sandbox ID set")
		}
	case ContainerTypeUnknown:
		return fmt.Errorf("unknown container-type")
	default:
	}

	return nil
}

// absPath turns the given path into an absolute path (if it is not already
// absolute) by prepending the base path.
func absPath(base, rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(base, rel)
}

// OpenSpec opens an OCI runtime spec from the given bundle directory.
func OpenSpec(bundleDir string) (*os.File, error) {
	// The spec file must be named "config.json" inside the bundle directory.
	return os.Open(filepath.Join(bundleDir, "config.json"))
}

// SpecOpts says how to read a spec.
type SpecOpts struct {
	Conf *config.Config

	// NoRootContainer requires the spec to have no Process and no Root.
	NoRootContainer bool
}

// ReadSpec reads an OCI runtime spec from the given bundle directory.
// ReadSpec also normalizes all potential relative paths into absolute
// path, e.g. spec.Root.Path, mount.Source.
func ReadSpec(bundleDir string, opts SpecOpts) (*specs.Spec, error) {
	specFile, err := OpenSpec(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("error opening spec file %q: %v", filepath.Join(bundleDir, "config.json"), err)
	}
	defer specFile.Close()
	return ReadSpecFromFile(bundleDir, specFile, opts)
}

// ReadSpecFromFile reads an OCI runtime spec from the given file. It also fixes
// up the spec so that the rest of the code doesn't need to worry about it.
//  1. Normalizes all relative paths into absolute by prepending the bundle
//     dir to them.
//  2. Looks for flag overrides and applies them if any.
//  3. Removes seccomp rules if `RuntimeDefault` was used.
func ReadSpecFromFile(bundleDir string, specFile *os.File, opts SpecOpts) (*specs.Spec, error) {
	if _, err := specFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("error seeking to beginning of file %q: %v", specFile.Name(), err)
	}
	specBytes, err := io.ReadAll(specFile)
	if err != nil {
		return nil, fmt.Errorf("error reading spec from file %q: %v", specFile.Name(), err)
	}
	var spec specs.Spec
	decoder := json.NewDecoder(bytes.NewReader(specBytes))
	decoder.DisallowUnknownFields()
	if errStrictDecode := decoder.Decode(&spec); errStrictDecode != nil {
		if errLooseDecode := json.Unmarshal(specBytes, &spec); errLooseDecode != nil {
			return nil, fmt.Errorf("error unmarshaling spec from file %q: %v\n %s", specFile.Name(), errLooseDecode, string(specBytes))
		} else {
			log.Warningf("OCI spec file %q contains fields unknown to `runsc`: %v. Ignoring these fields and continuing anyway.", specFile.Name(), errStrictDecode)
		}
	}
	if err := ValidateSpec(&spec, opts.Conf, opts.NoRootContainer); err != nil {
		return nil, err
	}
	if err := fixSpec(&spec, bundleDir, opts.Conf); err != nil {
		return nil, err
	}
	return &spec, nil
}

func fixSpec(spec *specs.Spec, bundleDir string, conf *config.Config) error {
	// Turn any relative paths in the spec to absolute by prepending the bundleDir.
	if spec.Root != nil {
		spec.Root.Path = absPath(bundleDir, spec.Root.Path)
	}
	for i := range spec.Mounts {
		m := &spec.Mounts[i]
		if m.Source != "" {
			m.Source = absPath(bundleDir, m.Source)
		}
	}
	// Mount hints identify mounts by source. Keep hint sources in sync with the
	// OCI mount sources normalized above; otherwise a bundle-relative source no
	// longer matches after fixSpec and repeated shared mounts become independent.
	const mountHintPrefix = "dev.gvisor.spec.mount."
	for annotation, source := range spec.Annotations {
		if strings.HasPrefix(annotation, mountHintPrefix) && strings.HasSuffix(annotation, ".source") && source != "" {
			spec.Annotations[annotation] = absPath(bundleDir, source)
		}
	}
	if encoded := spec.Annotations[AnnotationTTCVolumeRepositories]; encoded != "" {
		var repositories []TTCVolumeRepository
		if err := json.Unmarshal([]byte(encoded), &repositories); err != nil {
			return fmt.Errorf("normalizing %s: %w", AnnotationTTCVolumeRepositories, err)
		}
		for i := range repositories {
			repositories[i].Source = absPath(bundleDir, repositories[i].Source)
			repositories[i].Path = absPath(bundleDir, repositories[i].Path)
		}
		normalized, err := json.Marshal(repositories)
		if err != nil {
			return fmt.Errorf("normalizing %s: %w", AnnotationTTCVolumeRepositories, err)
		}
		spec.Annotations[AnnotationTTCVolumeRepositories] = string(normalized)
	}
	delete(spec.Annotations, AnnotationTTCMemoryFile)
	if encoded := spec.Annotations[AnnotationMountTarOnExit]; encoded != "" {
		var targets map[string]MountTarOnExitTarget
		if err := json.Unmarshal([]byte(encoded), &targets); err != nil {
			return fmt.Errorf("normalizing %s: %w", AnnotationMountTarOnExit, err)
		}
		normalized := make(map[string]MountTarOnExitTarget, len(targets))
		for source, target := range targets {
			if source != RootfsMountTarSource {
				source = absPath(bundleDir, source)
			}
			if _, ok := normalized[source]; ok {
				return fmt.Errorf("%s contains duplicate normalized source %q", AnnotationMountTarOnExit, source)
			}
			normalized[source] = target
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("normalizing %s: %w", AnnotationMountTarOnExit, err)
		}
		spec.Annotations[AnnotationMountTarOnExit] = string(encoded)
	}

	if containerName := containerNameNoRemap(spec); len(containerName) > 0 {
		// If we know the container name, then check to see if seccomp
		// instructions were given to the container.
		for annotation, val := range spec.Annotations {
			if annotation == annotationSeccomp+containerName && val == annotationSeccompRuntimeDefault {
				// Container seccomp rules are redundant when using gVisor, so remove
				// them when seccomp is set to RuntimeDefault.
				if spec.Linux != nil && spec.Linux.Seccomp != nil {
					log.Debugf("Seccomp is being ignored because annotation %q is set to default.", annotationSeccomp)
					spec.Linux.Seccomp = nil
				}
			}
		}
	}
	return nil
}

// TTCDistributedTarget reports whether spec is an unpublished distributed
// fork target whose reserved anonymous MemoryFile must remain empty until the
// remote Kernel metadata is installed.
func TTCDistributedTarget(spec *specs.Spec) bool {
	return spec != nil && spec.Annotations != nil && spec.Annotations[AnnotationTTCDistributedTarget] == "true"
}

// FixConfig fixes config options that are set via annotations in the spec.
func FixConfig(conf *config.Config, spec *specs.Spec) error {
	// Look for config bundle annotations and verify that they exist.
	const configBundlePrefix = "dev.gvisor.bundle."
	var bundles []config.BundleName
	for annotation, val := range spec.Annotations {
		if !strings.HasPrefix(annotation, configBundlePrefix) {
			continue
		}
		if val != "true" {
			return fmt.Errorf("invalid value %q for annotation %q (must be set to 'true' or removed entirely)", val, annotation)
		}
		bundleName := config.BundleName(annotation[len(configBundlePrefix):])
		if _, exists := config.Bundles[bundleName]; !exists {
			log.Warningf("Bundle name %q (from annotation %q=%q) does not exist; this bundle may have been deprecated. Skipping.", bundleName, annotation, val)
			continue
		}
		bundles = append(bundles, bundleName)
	}

	// Apply config bundles, if any.
	if len(bundles) > 0 {
		log.Infof("Applying config bundles: %v", bundles)
		if err := conf.ApplyBundles(flag.CommandLine, bundles...); err != nil {
			return err
		}
	}

	for annotation, val := range spec.Annotations {
		if strings.HasPrefix(annotation, annotationFlagPrefix) {
			// Override flags using annotation to allow customization per sandbox
			// instance.
			name := annotation[len(annotationFlagPrefix):]
			log.Infof("Overriding flag from flag annotation: --%s=%q", name, val)
			if err := conf.Override(flag.CommandLine, name, val /* force= */, false); err != nil {
				return err
			}
		}
	}
	// Override does not validate the config. Validate once after all
	// annotations have been applied: annotations are iterated in random map
	// order, so interdependent flags (e.g. qdisc=tbf and qdisc-tbf-rate) may
	// be applied in an order that is only valid once complete.
	return conf.Validate()
}

// ReadMounts reads mount list from a file.
func ReadMounts(f *os.File) ([]specs.Mount, error) {
	bytes, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("error reading mounts: %v", err)
	}
	var mounts []specs.Mount
	if err := json.Unmarshal(bytes, &mounts); err != nil {
		return nil, fmt.Errorf("error unmarshaling mounts: %v\nJSON bytes:\n%s", err, string(bytes))
	}
	return mounts, nil
}

// ChangeMountType changes m.Type to the specified type. It may do necessary
// amends to m.Options. It returns true if m was modified.
func ChangeMountType(m *specs.Mount, newType string) bool {
	updated := false
	if m.Type != newType {
		m.Type = newType
		updated = true
	}

	// OCI spec allows bind mounts to be specified in options only. So if new type
	// is not bind, remove bind/rbind from options.
	//
	// "For bind mounts (when options include either bind or rbind), the type is
	// a dummy, often "none" (not listed in /proc/filesystems)."
	if newType != "bind" {
		newOpts := make([]string, 0, len(m.Options))
		for _, opt := range m.Options {
			if opt != "rbind" && opt != "bind" {
				newOpts = append(newOpts, opt)
			} else {
				updated = true
			}
		}
		m.Options = newOpts
	}
	return updated
}

// Capabilities takes in spec and returns a TaskCapabilities corresponding to
// the spec.
func Capabilities(enableRaw bool, specCaps *specs.LinuxCapabilities) (*auth.TaskCapabilities, error) {
	// Strip CAP_NET_RAW from all capability sets if necessary.
	skipSet := map[linux.Capability]struct{}{}
	if !enableRaw {
		skipSet[linux.CAP_NET_RAW] = struct{}{}
	}

	var caps auth.TaskCapabilities
	if specCaps != nil {
		var err error
		if caps.BoundingCaps, err = capsFromNames(specCaps.Bounding, skipSet); err != nil {
			return nil, err
		}
		if caps.EffectiveCaps, err = capsFromNames(specCaps.Effective, skipSet); err != nil {
			return nil, err
		}
		if caps.InheritableCaps, err = capsFromNames(specCaps.Inheritable, skipSet); err != nil {
			return nil, err
		}
		if caps.PermittedCaps, err = capsFromNames(specCaps.Permitted, skipSet); err != nil {
			return nil, err
		}
		if caps.AmbientCaps, err = capsFromNames(specCaps.Ambient, skipSet); err != nil {
			return nil, err
		}
	}
	return &caps, nil
}

// AllCapabilities returns a LinuxCapabilities struct with all capabilities.
func AllCapabilities() *specs.LinuxCapabilities {
	var names []string
	for n := range capFromName {
		names = append(names, n)
	}
	return &specs.LinuxCapabilities{
		Bounding:    names,
		Effective:   names,
		Inheritable: names,
		Permitted:   names,
		Ambient:     names,
	}
}

// AllCapabilitiesSet returns a CapabilitySet containing all capabilities.
func AllCapabilitiesSet() auth.CapabilitySet {
	var rv auth.CapabilitySet
	for _, cap := range capFromName {
		rv.Add(cap)
	}
	return rv
}

// MergeCapabilities merges the capabilities from first and second.
func MergeCapabilities(first, second *specs.LinuxCapabilities) *specs.LinuxCapabilities {
	return &specs.LinuxCapabilities{
		Bounding:    mergeUnique(first.Bounding, second.Bounding),
		Effective:   mergeUnique(first.Effective, second.Effective),
		Inheritable: mergeUnique(first.Inheritable, second.Inheritable),
		Permitted:   mergeUnique(first.Permitted, second.Permitted),
		Ambient:     mergeUnique(first.Ambient, second.Ambient),
	}
}

// DropCapability removes the specified capability from all capability sets.
func DropCapability(caps *specs.LinuxCapabilities, drop string) {
	caps.Bounding = remove(caps.Bounding, drop)
	caps.Effective = remove(caps.Effective, drop)
	caps.Inheritable = remove(caps.Inheritable, drop)
	caps.Permitted = remove(caps.Permitted, drop)
	caps.Ambient = remove(caps.Ambient, drop)
}

func mergeUnique(strSlices ...[]string) []string {
	common := make(map[string]struct{})
	for _, strSlice := range strSlices {
		for _, s := range strSlice {
			common[s] = struct{}{}
		}
	}

	res := make([]string, 0, len(common))
	for s := range common {
		res = append(res, s)
	}
	return res
}

func remove(ss []string, rem string) []string {
	var out []string
	for _, s := range ss {
		if s == rem {
			continue
		}
		out = append(out, s)
	}
	return out
}

var capFromName = map[string]linux.Capability{
	"CAP_CHOWN":              linux.CAP_CHOWN,
	"CAP_DAC_OVERRIDE":       linux.CAP_DAC_OVERRIDE,
	"CAP_DAC_READ_SEARCH":    linux.CAP_DAC_READ_SEARCH,
	"CAP_FOWNER":             linux.CAP_FOWNER,
	"CAP_FSETID":             linux.CAP_FSETID,
	"CAP_KILL":               linux.CAP_KILL,
	"CAP_SETGID":             linux.CAP_SETGID,
	"CAP_SETUID":             linux.CAP_SETUID,
	"CAP_SETPCAP":            linux.CAP_SETPCAP,
	"CAP_LINUX_IMMUTABLE":    linux.CAP_LINUX_IMMUTABLE,
	"CAP_NET_BIND_SERVICE":   linux.CAP_NET_BIND_SERVICE,
	"CAP_NET_BROADCAST":      linux.CAP_NET_BROADCAST,
	"CAP_NET_ADMIN":          linux.CAP_NET_ADMIN,
	"CAP_NET_RAW":            linux.CAP_NET_RAW,
	"CAP_IPC_LOCK":           linux.CAP_IPC_LOCK,
	"CAP_IPC_OWNER":          linux.CAP_IPC_OWNER,
	"CAP_SYS_MODULE":         linux.CAP_SYS_MODULE,
	"CAP_SYS_RAWIO":          linux.CAP_SYS_RAWIO,
	"CAP_SYS_CHROOT":         linux.CAP_SYS_CHROOT,
	"CAP_SYS_PTRACE":         linux.CAP_SYS_PTRACE,
	"CAP_SYS_PACCT":          linux.CAP_SYS_PACCT,
	"CAP_SYS_ADMIN":          linux.CAP_SYS_ADMIN,
	"CAP_SYS_BOOT":           linux.CAP_SYS_BOOT,
	"CAP_SYS_NICE":           linux.CAP_SYS_NICE,
	"CAP_SYS_RESOURCE":       linux.CAP_SYS_RESOURCE,
	"CAP_SYS_TIME":           linux.CAP_SYS_TIME,
	"CAP_SYS_TTY_CONFIG":     linux.CAP_SYS_TTY_CONFIG,
	"CAP_MKNOD":              linux.CAP_MKNOD,
	"CAP_LEASE":              linux.CAP_LEASE,
	"CAP_AUDIT_WRITE":        linux.CAP_AUDIT_WRITE,
	"CAP_AUDIT_CONTROL":      linux.CAP_AUDIT_CONTROL,
	"CAP_SETFCAP":            linux.CAP_SETFCAP,
	"CAP_MAC_OVERRIDE":       linux.CAP_MAC_OVERRIDE,
	"CAP_MAC_ADMIN":          linux.CAP_MAC_ADMIN,
	"CAP_SYSLOG":             linux.CAP_SYSLOG,
	"CAP_WAKE_ALARM":         linux.CAP_WAKE_ALARM,
	"CAP_BLOCK_SUSPEND":      linux.CAP_BLOCK_SUSPEND,
	"CAP_AUDIT_READ":         linux.CAP_AUDIT_READ,
	"CAP_PERFMON":            linux.CAP_PERFMON,
	"CAP_BPF":                linux.CAP_BPF,
	"CAP_CHECKPOINT_RESTORE": linux.CAP_CHECKPOINT_RESTORE,
}

func capsFromNames(names []string, skipSet map[linux.Capability]struct{}) (auth.CapabilitySet, error) {
	var caps []linux.Capability
	for _, n := range names {
		c, ok := capFromName[n]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q", n)
		}
		// Should we skip this capability?
		if _, ok := skipSet[c]; ok {
			continue
		}
		caps = append(caps, c)
	}
	return auth.CapabilitySetOfMany(caps), nil
}

// IsGoferMount returns true if the given mount can be mounted as an external
// gofer.
func IsGoferMount(m specs.Mount) bool {
	MaybeConvertToBindMount(&m)
	return m.Type == "bind" && m.Source != ""
}

// IsErofsMount returns true if the given mount can be mounted as EROFS.
func IsErofsMount(m specs.Mount) bool {
	return m.Type == "erofs"
}

// HasMountConfig returns true if the given mount has an associated GoferMountConf.
func HasMountConfig(m specs.Mount) bool {
	return IsGoferMount(m) || IsErofsMount(m)
}

// IsIDMappedMount returns true if the given mount has a UID and GID mapping.
func IsIDMappedMount(m specs.Mount) bool {
	return len(m.UIDMappings) > 0 && len(m.GIDMappings) > 0
}

// MaybeConvertToBindMount converts mount type to "bind" in case any of the
// mount options are either "bind" or "rbind" as required by the OCI spec.
//
// "For bind mounts (when options include either bind or rbind), the type is a
// dummy, often "none" (not listed in /proc/filesystems)."
func MaybeConvertToBindMount(m *specs.Mount) {
	if m.Type == "bind" {
		return
	}
	for _, opt := range m.Options {
		if opt == "bind" || opt == "rbind" {
			m.Type = "bind"
			return
		}
	}
}

// WaitForReady waits for a process to become ready. The process is ready when
// the 'ready' function returns true. It continues to wait if 'ready' returns
// false. It returns error on timeout, if the process stops or if 'ready' fails.
func WaitForReady(pid int, timeout time.Duration, ready func() (bool, error)) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 1 * time.Millisecond
	b.MaxInterval = 1 * time.Second
	b.MaxElapsedTime = timeout

	op := func() error {
		if ok, err := ready(); err != nil {
			return backoff.Permanent(err)
		} else if ok {
			return nil
		}

		// Check if the process is still running.
		// If the process is alive, child is 0 because of the NOHANG option.
		// If the process has terminated, child equals the process id.
		var ws unix.WaitStatus
		var ru unix.Rusage
		child, err := unix.Wait4(pid, &ws, unix.WNOHANG, &ru)
		if err != nil {
			return backoff.Permanent(fmt.Errorf("error waiting for process: %v", err))
		} else if child == pid {
			return backoff.Permanent(fmt.Errorf("process %d has terminated", pid))
		}
		return fmt.Errorf("process %d not running yet", pid)
	}
	return backoff.Retry(op, b)
}

// WaitForNonChildExit waits for the given process to exit, up to `timeout`.
// As the name implies, `pid` must not be a child of the current process.
// Returns immediately if the process does not exist.
func WaitForNonChildExit(pid int, timeout time.Duration) error {
	return waitForNonChildExit(pid, timeout)
}

// OpenDebugLogFile opens a log file using 'logPattern' as location. If 'logPattern'
// ends with '/', it's used as a directory with default file name. See OpenLogFile
// for more details about variable substitutions.
func OpenDebugLogFile(logPattern string, opts log.FileOpts) (*os.File, error) {
	if strings.HasSuffix(logPattern, "/") {
		// Default format: <debug-log>/runsc.log.<yyyymmdd-hhmmss.uuuuuu>.<command>.txt
		logPattern += "runsc.log.%TIMESTAMP%.%COMMAND%.txt"
	}
	return log.OpenFile(logPattern, os.O_WRONLY|os.O_CREATE|os.O_APPEND, opts)
}

// LogFileOpts implements log.FileOpts for runsc log files.
// It builds the log file path based using the following variables:
//   - %TIMESTAMP%: is replaced with Timestamp using the following format:
//     <yyyymmdd-hhmmss.uuuuuu>
//   - %COMMAND%: is replaced with Command
//   - %TEST%: is replaced with Test
//   - %ID%: is replaced with SandboxID
//   - %CID%: is replaced with CID (container ID)
type LogFileOpts struct {
	SandboxID string
	CID       string
	Command   string
	Test      string
	Timestamp time.Time
}

// Build implements log.FileOpts.Build.
func (l *LogFileOpts) Build(logPattern string) string {
	// Replace variables in the log pattern.
	logPattern = strings.ReplaceAll(logPattern, "%TIMESTAMP%", l.Timestamp.Format("20060102-150405.000000"))
	logPattern = strings.ReplaceAll(logPattern, "%COMMAND%", l.Command)
	logPattern = strings.ReplaceAll(logPattern, "%TEST%", l.Test)
	logPattern = strings.ReplaceAll(logPattern, "%ID%", l.SandboxID)
	logPattern = strings.ReplaceAll(logPattern, "%CID%", l.CID)
	return logPattern
}

// TestName returns the test name if `TESTONLY-test-name-env` flag is set.
func TestName(conf *config.Config, spec *specs.Spec) string {
	if len(conf.TestOnlyTestNameEnv) == 0 {
		return ""
	}
	t, _ := EnvVar(spec.Process.Env, conf.TestOnlyTestNameEnv)
	return t
}

// IsDebugCommand returns true if the command should be debugged or not, based
// on the current configuration.
func IsDebugCommand(conf *config.Config, command string) bool {
	if len(conf.DebugCommand) == 0 {
		// Debug everything by default.
		return true
	}
	filter := conf.DebugCommand
	rv := true
	if filter[0] == '!' {
		// Negate the match, e.g. !boot should log all, but "boot".
		filter = filter[1:]
		rv = false
	}
	for _, cmd := range strings.Split(filter, ",") {
		if cmd == command {
			return rv
		}
	}
	return !rv
}

// TPUProxyEnabled checks if tpuproxy is enabled in the config or annotations.
func TPUProxyEnabled(spec *specs.Spec, conf *config.Config) bool {
	return conf.TPUProxy || AnnotationToBool(spec, AnnotationTPU) || TPUFunctionalityRequested(spec, conf)
}

// VFIOFunctionalityRequested returns true if the container should have access
// to VFIO functionality.
func VFIOFunctionalityRequested(dev *specs.LinuxDevice) bool {
	return strings.HasPrefix(dev.Path, "/dev/vfio") && dev.Path != "/dev/vfio/vfio"
}

// AcceleratorFunctionalityRequested returns true if the container should have
// access to compute accelerators. Compute accelerators are different from GPUs
// by using a different major number and different device char files.
func AcceleratorFunctionalityRequested(dev *specs.LinuxDevice) bool {
	return strings.HasPrefix(dev.Path, "/dev/accel")
}

// TPUFunctionalityRequested returns true if the container should have access
// to TPU functionality.
func TPUFunctionalityRequested(spec *specs.Spec, conf *config.Config) bool {
	if spec.Linux != nil {
		for _, dev := range spec.Linux.Devices {
			if AcceleratorFunctionalityRequested(&dev) || VFIOFunctionalityRequested(&dev) {
				return true
			}
		}
	}
	return false
}

// SafeSetupAndMount creates the mount point and calls Mount with the given
// flags. procPath is the path to procfs. If it is "", procfs is assumed to be
// mounted at /proc.
func SafeSetupAndMount(src, dst, typ string, flags uint32, procPath string) error {
	// Create the mount point inside. The type must be the same as the source
	// (file or directory).
	var isDir bool
	if typ == "proc" {
		// Special case, as there is no source directory for proc mounts.
		isDir = true
	} else if fi, err := os.Stat(src); err != nil {
		return fmt.Errorf("stat(%q) failed: %v", src, err)
	} else {
		isDir = fi.IsDir()
	}

	if isDir {
		// Create the destination directory.
		if err := os.MkdirAll(dst, 0777); err != nil {
			return fmt.Errorf("mkdir(%q) failed: %v", dst, err)
		}
	} else {
		// Create the parent destination directory.
		parent := path.Dir(dst)
		if err := os.MkdirAll(parent, 0777); err != nil {
			return fmt.Errorf("mkdir(%q) failed: %v", parent, err)
		}
		// Create the destination file if it does not exist.
		f, err := os.OpenFile(dst, unix.O_CREAT, 0777)
		if err != nil {
			return fmt.Errorf("open(%q) failed: %v", dst, err)
		}
		f.Close()
	}

	// Do the mount.
	if err := SafeMount(src, dst, typ, uintptr(flags), "", procPath); err != nil {
		return fmt.Errorf("mount(%q, %q, %d) failed: %v", src, dst, flags, err)
	}
	return nil
}

// ErrSymlinkMount is returned by SafeMount when the mount destination is found
// to be a symlink.
type ErrSymlinkMount struct {
	error
}

// SafeMount is like unix.Mount, but will fail if dst is a symlink. procPath is
// the path to procfs. If it is "", procfs is assumed to be mounted at /proc.
//
// SafeMount can fail when dst contains a symlink. However, it is called in the
// normal case with a destination consisting of a known root (/proc/root) and
// symlink-free path (from resolveSymlink).
func SafeMount(src, dst, fstype string, flags uintptr, data, procPath string) error {
	return safeMount(src, dst, fstype, flags, data, procPath)
}

// RetryEintr retries the function until an error different than EINTR is
// returned.
func RetryEintr(f func() (uintptr, uintptr, error)) (uintptr, uintptr, error) {
	for {
		r1, r2, err := f()
		if err != unix.EINTR {
			return r1, r2, err
		}
	}
}

// GetOOMScoreAdj reads the given process' oom_score_adj
func GetOOMScoreAdj(pid int) (int, error) {
	return hostOOMScoreAdj(pid)
}

// EnvVar looks for a variable value in the env slice assuming the following
// format: "NAME=VALUE". If a variable is defined multiple times, the last
// value is used.
func EnvVar(env []string, name string) (string, bool) {
	var err error
	env, err = ResolveEnvs(env)
	if err != nil {
		return "", false
	}
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

// ResolveEnvs transforms lists of environment variables into a single list of
// environment variables. If a variable is defined multiple times, the last
// value is used.
func ResolveEnvs(envs ...[]string) ([]string, error) {
	// First create a map of variable names to values. This removes any
	// duplicates.
	envMap := make(map[string]string)
	for _, env := range envs {
		for _, str := range env {
			parts := strings.SplitN(str, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid variable: %s", str)
			}
			envMap[parts[0]] = parts[1]
		}
	}
	// Reassemble envMap into a list of environment variables of the form
	// NAME=VALUE.
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env, nil
}

// FaqErrorMsg returns an error message pointing to the FAQ.
func FaqErrorMsg(anchor, msg string) string {
	return fmt.Sprintf("%s; see https://gvisor.dev/faq#%s for more details", msg, anchor)
}

// ContainerName looks for an annotation in the spec with the container name. Returns empty string
// if no annotation is found.
func ContainerName(spec *specs.Spec) string {
	name := containerNameNoRemap(spec)
	if len(name) > 0 {
		// Check if annotations requested the container name to change.
		for key, mapping := range spec.Annotations {
			if strings.HasPrefix(key, annotationContainerNameRemap) {
				// Mapping should be in the form: "current_name=new_name".
				parts := strings.SplitN(mapping, "=", 2)
				if len(parts) != 2 {
					log.Warningf("Invalid annotation %q", key)
					continue
				}
				if parts[0] == name {
					name = parts[1]
					break
				}
			}
		}
	}
	return name
}

func containerNameNoRemap(spec *specs.Spec) string {
	return spec.Annotations[ContainerdContainerNameAnnotation]
}

// RootfsTarUpperPath returns the path to the rootfs upper tar file, or empty
// string if not set. In multi-container mode, only container-specific
// annotations are used.
func RootfsTarUpperPath(spec *specs.Spec) string {
	if spec == nil || spec.Annotations == nil {
		return ""
	}
	if name := ContainerName(spec); name != "" {
		return spec.Annotations[AnnotationRootfsUpperTar+"."+name]
	}
	// Multi-container sandbox must use container-specific annotations.
	if cType := SpecContainerType(spec); cType == ContainerTypeContainer || cType == ContainerTypeSandbox {
		return ""
	}
	return spec.Annotations[AnnotationRootfsUpperTar]
}

// MountTarOnExitTarget names a pending archive and its completion marker.
type MountTarOnExitTarget struct {
	Path      string `json:"path"`
	ReadyPath string `json:"ready_path"`
}

// TTCVolumeRepository describes one repository directory donated for a ttcvol
// mount. Source matches the OCI mount source and Path is never exposed inside
// the sandbox.
type TTCVolumeRepository struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	CopyMode string `json:"copy_mode"`
	// Private marks a container-family-owned repository. Unlike a named or
	// anonymous managed volume, it is restored beneath the new container's
	// private state directory and is removed with that container.
	Private bool `json:"private,omitempty"`
	// OverlayDestination selects this repository as the persistent upper for
	// the gofer bind mounted at the given guest destination. Empty means that
	// the repository is mounted directly as a ttcvol filesystem.
	OverlayDestination string `json:"overlay_destination,omitempty"`
	// RestoreID is the repository identity serialized in a TTC checkpoint.
	// Empty during ordinary creation and live fork.
	RestoreID string `json:"restore_id,omitempty"`
}

// TTCDirectHostMounts returns the destinations of the spec's bind mounts that
// AnnotationTTCDirectHostMounts lists.
func TTCDirectHostMounts(spec *specs.Spec) (map[string]bool, error) {
	encoded := spec.Annotations[AnnotationTTCDirectHostMounts]
	if encoded == "" {
		return nil, nil
	}
	var destinations []string
	if err := json.Unmarshal([]byte(encoded), &destinations); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", AnnotationTTCDirectHostMounts, err)
	}
	direct := make(map[string]bool, len(destinations))
	for _, destination := range destinations {
		direct[filepath.Clean(destination)] = true
	}
	return direct, nil
}

// TTCVolumeRepositories parses and validates persistent-volume donations in
// deterministic source order.
func TTCVolumeRepositories(spec *specs.Spec) ([]TTCVolumeRepository, error) {
	if spec == nil || spec.Annotations == nil || spec.Annotations[AnnotationTTCVolumeRepositories] == "" {
		return nil, nil
	}
	var repositories []TTCVolumeRepository
	if err := json.Unmarshal([]byte(spec.Annotations[AnnotationTTCVolumeRepositories]), &repositories); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", AnnotationTTCVolumeRepositories, err)
	}
	seen := make(map[string]bool, len(repositories))
	seenOverlayDestinations := make(map[string]bool, len(repositories))
	for _, repository := range repositories {
		if repository.Source == "" || repository.Path == "" || repository.Branch == "" {
			return nil, fmt.Errorf("%s contains an empty source, path, or branch", AnnotationTTCVolumeRepositories)
		}
		if !filepath.IsAbs(repository.Path) {
			return nil, fmt.Errorf("%s repository path %q must be absolute", AnnotationTTCVolumeRepositories, repository.Path)
		}
		if seen[repository.Source] {
			return nil, fmt.Errorf("%s contains duplicate source %q", AnnotationTTCVolumeRepositories, repository.Source)
		}
		seen[repository.Source] = true
		switch repository.CopyMode {
		case "reflink", "full-copy":
		default:
			return nil, fmt.Errorf("%s source %q has invalid copy mode %q", AnnotationTTCVolumeRepositories, repository.Source, repository.CopyMode)
		}
		if repository.OverlayDestination != "" {
			destination := filepath.Clean(repository.OverlayDestination)
			if !filepath.IsAbs(destination) || destination == "/" || destination != repository.OverlayDestination {
				return nil, fmt.Errorf("%s source %q has invalid overlay destination %q", AnnotationTTCVolumeRepositories, repository.Source, repository.OverlayDestination)
			}
			if !repository.Private {
				return nil, fmt.Errorf("%s source %q has a non-private overlay repository", AnnotationTTCVolumeRepositories, repository.Source)
			}
			if seenOverlayDestinations[destination] {
				return nil, fmt.Errorf("%s contains duplicate overlay destination %q", AnnotationTTCVolumeRepositories, destination)
			}
			seenOverlayDestinations[destination] = true
		}
		if repository.RestoreID != "" {
			if len(repository.RestoreID) != 32 {
				return nil, fmt.Errorf("%s source %q has invalid restore ID %q", AnnotationTTCVolumeRepositories, repository.Source, repository.RestoreID)
			}
			if _, err := hex.DecodeString(repository.RestoreID); err != nil {
				return nil, fmt.Errorf("%s source %q has invalid restore ID %q", AnnotationTTCVolumeRepositories, repository.Source, repository.RestoreID)
			}
		}
	}
	sort.Slice(repositories, func(i, j int) bool {
		return repositories[i].Source < repositories[j].Source
	})
	return repositories, nil
}

// MountTarOnExitPath associates one shared mount with a guest destination and host outputs.
type MountTarOnExitPath struct {
	Source      string
	Destination string
	Path        string
	ReadyPath   string
}

// MountTarOnExitPaths returns final shared-mount exports in deterministic source order.
func MountTarOnExitPaths(spec *specs.Spec) ([]MountTarOnExitPath, error) {
	if spec == nil || spec.Annotations == nil {
		return nil, nil
	}
	encoded := spec.Annotations[AnnotationMountTarOnExit]
	if encoded == "" {
		return nil, nil
	}
	var targets map[string]MountTarOnExitTarget
	if err := json.Unmarshal([]byte(encoded), &targets); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", AnnotationMountTarOnExit, err)
	}
	type mountDestination struct {
		path     string
		readonly bool
	}
	destinations := make(map[string]mountDestination)
	if spec.Root != nil && !spec.Root.Readonly {
		destinations[RootfsMountTarSource] = mountDestination{path: "/"}
	}
	for _, mount := range spec.Mounts {
		readonly := false
		for _, option := range mount.Options {
			if option == "ro" {
				readonly = true
				break
			}
		}
		current, exists := destinations[mount.Source]
		if !exists || current.readonly && !readonly {
			destinations[mount.Source] = mountDestination{path: mount.Destination, readonly: readonly}
		}
	}
	sources := make([]string, 0, len(targets))
	for source, target := range targets {
		if source == "" || target.Path == "" || target.ReadyPath == "" {
			return nil, fmt.Errorf("%s contains an empty source or path", AnnotationMountTarOnExit)
		}
		if !filepath.IsAbs(target.Path) || !filepath.IsAbs(target.ReadyPath) {
			return nil, fmt.Errorf("%s output paths for source %q must be absolute", AnnotationMountTarOnExit, source)
		}
		if destinations[source].path == "" {
			return nil, fmt.Errorf("%s source %q is not mounted", AnnotationMountTarOnExit, source)
		}
		sources = append(sources, source)
	}
	sort.Strings(sources)
	result := make([]MountTarOnExitPath, 0, len(sources))
	for _, source := range sources {
		target := targets[source]
		result = append(result, MountTarOnExitPath{Source: source, Destination: destinations[source].path, Path: target.Path, ReadyPath: target.ReadyPath})
	}
	return result, nil
}

// AnnotationToBool parses the annotation value as a bool. On failure, it logs a warning and
// returns false.
func AnnotationToBool(spec *specs.Spec, annotation string) bool {
	val, ok := spec.Annotations[annotation]
	if !ok {
		return false
	}
	ret, err := strconv.ParseBool(val)
	if err != nil {
		log.Warningf("Failed to parse annotation %q=%q as a bool: %v", annotation, val, err)
		return false
	}
	return ret
}

func boolPtr(b bool) *bool {
	return &b
}

// Features returns the features supported by the runtime.
func Features() *features.Features {
	featuresOnce.Do(func() {
		feat := features.Features{
			OCIVersionMin: "1.0.0",
			OCIVersionMax: specs.Version,
			Hooks: []string{
				"prestart",
				"createRuntime",
				"createContainer",
				"startContainer",
				"poststart",
				"poststop",
			},
			MountOptions: KnownMountOptions(),
			Linux: &features.Linux{
				Namespaces:   KnownNamespaces(),
				Capabilities: AllCapabilities().Bounding,
				Cgroup: &features.Cgroup{
					V1:          boolPtr(true),
					V2:          boolPtr(false),
					Systemd:     boolPtr(false),
					SystemdUser: boolPtr(false),
					Rdma:        boolPtr(false),
				},
				Seccomp: &features.Seccomp{
					Enabled:        boolPtr(true),
					Actions:        knownSeccompActions(),
					Operators:      knownSeccompOperators(),
					Archs:          knownSeccompArchs(),
					KnownFlags:     knownSeccompFlags(),
					SupportedFlags: supportedSeccompFlags(),
				},
				Apparmor: &features.Apparmor{
					Enabled: boolPtr(false),
				},
				Selinux: &features.Selinux{
					Enabled: boolPtr(false),
				},
				IntelRdt: &features.IntelRdt{
					Enabled: boolPtr(false),
				},
				MountExtensions: &features.MountExtensions{
					IDMap: &features.IDMap{
						Enabled: boolPtr(true),
					},
				},
			},
		}

		tmpFs := flag.NewFlagSet("tmp", flag.ContinueOnError)
		config.RegisterFlags(tmpFs)
		annotations := make(map[string]string)
		tmpFs.VisitAll(func(f *flag.Flag) {
			key := "dev.gvisor.flag." + f.Name
			annotations[key] = f.DefValue
		})

		// LINT.IfChange
		annotations["dev.gvisor.container-name-remap."] = ""
		annotations[AnnotationRootfsUpperTar] = ""
		annotations["dev.gvisor.internal.seccomp."] = ""
		annotations["dev.gvisor.internal.seccomp.cont"] = "RuntimeDefault"
		annotations[AnnotationTPU] = ""
		annotations[AnnotationCPUFeatures] = ""
		annotations[AnnotationCPUTopology] = ""
		// LINT.ThenChange(:features_annotations)
		feat.Annotations = annotations

		var unsafeAnnotations []string
		tmpFs.VisitAll(func(f *flag.Flag) {
			if !config.IsFlagSafeToOverride(f.Name) {
				unsafeAnnotations = append(unsafeAnnotations, "dev.gvisor.flag."+f.Name)
			}
		})

		// LINT.IfChange
		unsafeAnnotations = append(unsafeAnnotations,
			"dev.gvisor.container-name-remap.",
			AnnotationRootfsUpperTar,
			"dev.gvisor.internal.seccomp.",
			AnnotationTPU,
			AnnotationCPUFeatures,
			AnnotationCPUTopology,
		)
		// LINT.ThenChange(:features_annotations)

		sort.Strings(unsafeAnnotations)
		feat.PotentiallyUnsafeConfigAnnotations = unsafeAnnotations

		globalFeatures = &feat
	})

	return globalFeatures
}
