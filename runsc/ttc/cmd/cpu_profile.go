// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/cputopology"
)

const daemonCPUProfileVersion = 3

// portableAMD64Features is an x86-64-v2-class guest instruction contract,
// expressed using gVisor's Linux-compatible
// feature names. It is deliberately independent of the bootstrap host's vendor
// and model so an AMD-initialized cluster can admit an Intel node (and vice
// versa). Newer vector instructions are available via immutable host or
// explicit profiles.
var portableAMD64Features = []string{
	"apic", "cmov", "cx16", "cx8", "fpu", "fxsr", "lahf_lm", "lm",
	"mmx", "nx", "pae", "pat", "pge", "pni", "popcnt", "pse",
	"pse36", "sep", "sse", "sse2", "sse4_1", "sse4_2", "ssse3", "syscall",
	"tsc",
}

// daemonCPUProfile is the exact guest-visible instruction contract. Mode is
// descriptive; Features is authoritative and is never recomputed after a
// root or cluster has been initialized.
type daemonCPUProfile struct {
	Version  int      `json:"version"`
	Mode     string   `json:"mode"`
	Features []string `json:"features"`
	XsaveABI string   `json:"xsave_abi,omitempty"`
	// NoCPUIDFaulting is set in a cluster created with
	// --experimental-no-cpuid-faulting, whose systrap nodes need no CPUID
	// faulting. An application on a node without it reads the host's own
	// CPUID instead of the profile, so every node must have exactly the
	// CPUID registers CPUID records (cpuid.HostCPUIDFeatures). And as such an
	// application may see FSGSBASE and write GS itself, every node runs
	// systrap without syscall patching, whose fast path owns GS.
	NoCPUIDFaulting bool   `json:"experimental_no_cpuid_faulting,omitempty"`
	CPUID           string `json:"cpuid,omitempty"`
	// Topology is the CPU topology a cluster's guests see,
	// cputopology.Single or cputopology.Host, which the cluster's first
	// daemon records with its CPU profile and which is chosen independently
	// of it. A cluster recorded without one shows the single topology.
	Topology string `json:"cpu_topology,omitempty"`
	// TopologyFingerprint is, under the host topology, the fingerprint of the
	// layout every node of the cluster must have (see
	// cputopology.Topology.Fingerprint), so that a guest moved between nodes
	// finds its CPU and node numbers meaning what they meant.
	TopologyFingerprint string `json:"cpu_topology_fingerprint,omitempty"`
}

// cpuTopologyMode returns the CPU topology profile records.
func (p daemonCPUProfile) cpuTopologyMode() string {
	if p.Topology == "" {
		return cputopology.Single
	}
	return p.Topology
}

// cpuProfileWithoutFaultingWarning returns what a daemon says at start when
// its applications read the host's own CPUID rather than its CPU profile: on
// a systrap host without CPUID faulting, a profile other than the host's
// shows only in /proc/cpuinfo and the auxiliary vector, while a library that
// executes CPUID, as MKL and glibc's own dispatch do, finds the host's
// features and uses them.
func cpuProfileWithoutFaultingWarning(profile daemonCPUProfile, runtimePlatform string, cpuidFaulting bool) string {
	if runtimePlatform != "systrap" || cpuidFaulting || profile.Mode == "host" || profile.NoCPUIDFaulting {
		return ""
	}
	return fmt.Sprintf("this host has no CPUID faulting, so applications read its own CPUID, not the %s CPU profile, which only /proc/cpuinfo shows; libraries that execute CPUID use features outside the profile. use --cpu-profile host to make the profile the host's", profile.Mode)
}

// daemonCPUTopologyWarning is what a daemon says at start when its CPUs span
// more than one memory node and --cpu-topology did not choose a topology.
const daemonCPUTopologyWarning = "this daemon has access to multiple numa nodes, but --cpu-topology is not set explicitly. by default, numa and guest affinity are ignored. use --cpu-topology host or bind the daemon to a specific numa node with numactl or taskset."

// resolveDaemonCPUTopology returns the topology a daemon's guests see under
// mode. Under the host topology, it is the host's, which must have
// fingerprint want, if want is not empty: a cluster's nodes all have one.
func resolveDaemonCPUTopology(mode, want string) (cputopology.Topology, error) {
	switch mode {
	case "", cputopology.Single:
		return cputopology.Topology{Mode: cputopology.Single}, nil
	case cputopology.Host:
	default:
		return cputopology.Topology{}, fmt.Errorf("unknown CPU topology %q; it is single or host", mode)
	}
	host, err := cputopology.HostTopology()
	if err != nil {
		return cputopology.Topology{}, fmt.Errorf("reading the host CPU topology: %w", err)
	}
	if want != "" && host.Fingerprint() != want {
		return cputopology.Topology{}, errors.New("this host's CPU topology differs from the cluster's: under the host topology every node has the same CPUs, cores, packages and memory nodes, by the same numbers")
	}
	return host, nil
}

func hostCPUFeatureNames(runtimePlatform string) []string {
	cpuid.Initialize()
	host := cpuid.HostFeatureSet()
	features := make([]string, 0, len(cpuid.AllFeatures()))
	for _, feature := range cpuid.AllFeatures() {
		if !host.HasFeature(feature) {
			continue
		}
		name := feature.String()
		if name == "" || strings.HasPrefix(name, "[") {
			continue
		}
		// Systrap's syscall-patching fast path temporarily owns GS. It can
		// transparently retire patched sites on arch_prctl, but direct
		// FSGSBASE instructions cannot be intercepted.
		if runtimePlatform == "systrap" && name == "fsgsbase" {
			continue
		}
		// gVisor does not currently preserve AMX tile state. Never advertise
		// instructions whose architectural state cannot survive a TTC fork or
		// migration, including in the host profile.
		if strings.HasPrefix(name, "amx_") {
			continue
		}
		features = append(features, name)
	}
	slices.Sort(features)
	return slices.Compact(features)
}

func resolveDaemonCPUProfile(mode, explicit, runtimePlatform string) (daemonCPUProfile, error) {
	if mode == "" {
		mode = "portable"
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	host := hostCPUFeatureNames(runtimePlatform)
	hostSet := make(map[string]struct{}, len(host))
	for _, feature := range host {
		hostSet[feature] = struct{}{}
	}
	var features []string
	switch mode {
	case "portable":
		if strings.TrimSpace(explicit) != "" {
			return daemonCPUProfile{}, errors.New("--cpu-features requires --cpu-profile=explicit")
		}
		if runtime.GOARCH == "amd64" {
			features = append(features, portableAMD64Features...)
		} else {
			// There is not yet a TTC cross-vendor baseline for other
			// architectures. Persisting the exact host set still fails closed at
			// admission rather than silently changing guest-visible features.
			features = append(features, host...)
		}
		for _, feature := range features {
			if _, ok := hostSet[feature]; !ok {
				return daemonCPUProfile{}, fmt.Errorf("host does not support portable CPU feature %q", feature)
			}
		}
	case "host":
		if strings.TrimSpace(explicit) != "" {
			return daemonCPUProfile{}, errors.New("--cpu-features requires --cpu-profile=explicit")
		}
		features = append(features, host...)
	case "explicit":
		if strings.TrimSpace(explicit) == "" {
			return daemonCPUProfile{}, errors.New("--cpu-profile=explicit requires --cpu-features")
		}
		for _, name := range strings.Split(explicit, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return daemonCPUProfile{}, errors.New("--cpu-features contains an empty feature")
			}
			feature, ok := cpuid.FeatureFromString(name)
			if !ok {
				return daemonCPUProfile{}, fmt.Errorf("unknown CPU feature %q", name)
			}
			if _, ok := hostSet[feature.String()]; !ok {
				return daemonCPUProfile{}, fmt.Errorf("host does not support requested CPU feature %q", name)
			}
			features = append(features, feature.String())
		}
		slices.Sort(features)
		features = slices.Compact(features)
	default:
		return daemonCPUProfile{}, fmt.Errorf("unsupported CPU profile %q; supported profiles are portable, host, and explicit", mode)
	}
	if len(features) == 0 {
		return daemonCPUProfile{}, fmt.Errorf("CPU profile %q contains no features on %s", mode, runtime.GOARCH)
	}
	if err := validateXsaveCPUProfileContract(features); err != nil {
		return daemonCPUProfile{}, err
	}
	profile := daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: mode, Features: features}
	if slices.Contains(features, "xsave") {
		profile.XsaveABI = cpuid.HostXsaveABI()
	}
	return profile, nil
}

func validateXsaveCPUProfileContract(features []string) error {
	if runtime.GOARCH != "amd64" {
		return nil
	}
	// The internal systrap save format is independent of the guest instruction
	// contract. Portable guests use the stable FXSAVE ABI. Exposing XSAVE requires
	// matching the physical ABI on every node, since native XGETBV/XSAVE cannot
	// be virtualized by the CPUID feature mask.
	xsave := slices.Contains(features, "xsave")
	if xsave != slices.Contains(features, "osxsave") {
		return errors.New("CPU profiles must enable xsave and osxsave together")
	}
	for _, feature := range features {
		if !xsave && (strings.HasPrefix(feature, "avx") || strings.HasPrefix(feature, "xsave") || feature == "fma" || feature == "f16c") {
			return fmt.Errorf("CPU feature %q requires xsave and osxsave", feature)
		}
	}
	return nil
}

// waiveCPUIDFaulting makes profile that of a cluster whose systrap nodes need
// no CPUID faulting, recording this host's CPUID as the one every node must
// have.
func waiveCPUIDFaulting(profile daemonCPUProfile, runtimePlatform string) (daemonCPUProfile, error) {
	if runtime.GOARCH != "amd64" {
		return daemonCPUProfile{}, fmt.Errorf("--experimental-no-cpuid-faulting applies to x86-64 hosts, not %s", runtime.GOARCH)
	}
	if runtimePlatform != "systrap" {
		return daemonCPUProfile{}, fmt.Errorf("--experimental-no-cpuid-faulting applies to systrap nodes; %s needs no CPUID faulting", runtimePlatform)
	}
	if profile.Mode != "host" {
		return daemonCPUProfile{}, errors.New("--experimental-no-cpuid-faulting requires --cpu-profile host")
	}
	profile.NoCPUIDFaulting = true
	profile.CPUID = cpuid.HostCPUIDFeatures()
	return profile, nil
}

// cpuidDifferences names the registers whose values differ between want and
// got, two cpuid.HostCPUIDFeatures strings.
func cpuidDifferences(want, got string) []string {
	parse := func(features string) map[string]string {
		values := make(map[string]string)
		for _, field := range strings.Split(features, ";") {
			if name, value, ok := strings.Cut(field, "="); ok {
				values[name] = value
			}
		}
		return values
	}
	wantValues, gotValues := parse(want), parse(got)
	var names []string
	for name := range wantValues {
		names = append(names, name)
	}
	for name := range gotValues {
		if _, ok := wantValues[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var differences []string
	for _, name := range names {
		wantValue, gotValue := wantValues[name], gotValues[name]
		if wantValue == gotValue {
			continue
		}
		if wantValue == "" {
			wantValue = "0"
		}
		if gotValue == "" {
			gotValue = "0"
		}
		differences = append(differences, fmt.Sprintf("%s is %s in the cluster, %s here", name, wantValue, gotValue))
	}
	return differences
}

func validateDaemonCPUProfile(profile daemonCPUProfile, runtimePlatform string) error {
	if profile.Version != daemonCPUProfileVersion || profile.Mode == "" || len(profile.Features) == 0 || !slices.IsSorted(profile.Features) {
		return errors.New("invalid TTC CPU profile")
	}
	if profile.NoCPUIDFaulting != (profile.CPUID != "") || (profile.NoCPUIDFaulting && profile.Mode != "host") {
		return errors.New("invalid TTC CPU profile")
	}
	if profile.NoCPUIDFaulting {
		if runtimePlatform != "systrap" {
			return fmt.Errorf("the cluster's nodes run without CPUID faulting, on systrap only; this node's platform is %s", runtimePlatform)
		}
		if host := cpuid.HostCPUIDFeatures(); host != profile.CPUID {
			return fmt.Errorf("the cluster's nodes run without CPUID faulting, so they must have identical CPUs, and this host's CPUID differs: %s", strings.Join(cpuidDifferences(profile.CPUID, host), "; "))
		}
	}
	host := make(map[string]struct{})
	for _, feature := range hostCPUFeatureNames(runtimePlatform) {
		host[feature] = struct{}{}
	}
	var missing []string
	for _, feature := range profile.Features {
		if _, ok := host[feature]; !ok {
			missing = append(missing, feature)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("host CPU cannot execute immutable TTC profile %q; missing features: %s", profile.Mode, strings.Join(missing, ","))
	}
	if err := validateXsaveCPUProfileContract(profile.Features); err != nil {
		return err
	}
	if slices.Contains(profile.Features, "xsave") && (profile.XsaveABI == "" || profile.XsaveABI != cpuid.HostXsaveABI()) {
		return errors.New("host XSAVE ABI differs from the immutable CPU profile; use a fresh portable-profile root for heterogeneous CPUs")
	}
	return nil
}

// validateNoCPUIDFaultingOption refuses --experimental-no-cpuid-faulting
// outside a cluster's compute nodes.
func validateNoCPUIDFaultingOption(opts daemonClusterOptions) error {
	if opts.NoCPUIDFaulting && (!opts.Configured || opts.ControlOnly || opts.ControlHolder) {
		return errors.New("--experimental-no-cpuid-faulting applies to a cluster's compute nodes")
	}
	return nil
}

// validateCPUTopologyOption refuses --cpu-topology for a daemon that runs no
// guest: a cluster's control-only daemon.
func validateCPUTopologyOption(opts daemonClusterOptions) error {
	if opts.CPUTopologySet && (opts.ControlOnly || opts.ControlHolder) {
		return errors.New("--cpu-topology applies to daemons that run containers, not to a cluster's control-only daemon")
	}
	return nil
}

// validateClusterCPUEnforcement refuses a systrap node whose host lacks CPUID
// faulting, unless its cluster waives it.
func validateClusterCPUEnforcement(runtimePlatform string, cpuidFaulting, waived bool) error {
	if runtimePlatform == "systrap" && !cpuidFaulting && !waived {
		return errors.New("systrap cluster mode requires host CPUID faulting (ARCH_SET_CPUID); this host cannot enforce a migration-safe guest CPU profile. A cluster of identical CPUs can do without it if it is created with --cpu-profile host --experimental-no-cpuid-faulting")
	}
	return nil
}

func applyStandaloneCPUProfile(root, mode, features, runtimePlatform string, explicitlySet bool) (daemonCPUProfile, error) {
	path := filepath.Join(root, ".ttc-cpu-profile.json")
	requested, err := resolveDaemonCPUProfile(mode, features, runtimePlatform)
	if err != nil {
		return daemonCPUProfile{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeDurableJSON(path, requested); err != nil {
			return daemonCPUProfile{}, fmt.Errorf("recording immutable CPU profile: %w", err)
		}
		return requested, nil
	}
	if err != nil {
		return daemonCPUProfile{}, err
	}
	var stored daemonCPUProfile
	if err := json.Unmarshal(data, &stored); err != nil {
		return daemonCPUProfile{}, fmt.Errorf("decoding immutable CPU profile: %w", err)
	}
	if err := validateDaemonCPUProfile(stored, runtimePlatform); err != nil {
		return daemonCPUProfile{}, err
	}
	if explicitlySet && !requestsCPUProfile(requested, stored) {
		return daemonCPUProfile{}, fmt.Errorf("requested CPU profile differs from immutable profile %q stored in TTC_ROOT", stored.Mode)
	}
	return stored, nil
}

// requestsCPUProfile reports whether requested, a --cpu-profile as this build
// resolves it, asks for stored, the immutable profile of a root. A named
// profile (host, portable) asks for whatever it was when the root was made: a
// later build may know more of the host's features, or a different portable
// baseline, and the stored profile stays the root's, which
// validateDaemonCPUProfile checks the host can still run. An explicit profile
// asks for its features.
func requestsCPUProfile(requested, stored daemonCPUProfile) bool {
	if requested.Mode != stored.Mode {
		return false
	}
	if requested.Mode != "explicit" {
		return true
	}
	want, have := slices.Clone(requested.Features), slices.Clone(stored.Features)
	slices.Sort(want)
	slices.Sort(have)
	return slices.Equal(want, have)
}

func (p daemonCPUProfile) annotation() string {
	return strings.Join(p.Features, ",")
}

// cpuTopologyAnnotation returns the annotation that shows guests t: none for
// the single topology, which is a sandbox's own.
func cpuTopologyAnnotation(t cputopology.Topology) string {
	if t.Mode != cputopology.Host {
		return ""
	}
	return t.Marshal()
}

func consumeDaemonCPUOption(args []string, opts *daemonClusterOptions) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if args[0] == "--experimental-no-cpuid-faulting" {
		opts.NoCPUIDFaulting = true
		return 1, nil
	}
	set := func(name string, dst *string) (int, error) {
		if args[0] == name {
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			*dst = args[1]
			return 2, nil
		}
		if strings.HasPrefix(args[0], name+"=") {
			*dst = strings.TrimPrefix(args[0], name+"=")
			if strings.TrimSpace(*dst) == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			return 1, nil
		}
		return 0, nil
	}
	if consumed, err := set("--cpu-profile", &opts.CPUProfileMode); consumed != 0 || err != nil {
		opts.CPUProfileSet = consumed != 0
		return consumed, err
	}
	if consumed, err := set("--cpu-features", &opts.CPUFeatures); consumed != 0 || err != nil {
		opts.CPUProfileSet = consumed != 0
		return consumed, err
	}
	if consumed, err := set("--cpu-topology", &opts.CPUTopology); consumed != 0 || err != nil {
		if err != nil {
			return consumed, err
		}
		opts.CPUTopology = strings.ToLower(strings.TrimSpace(opts.CPUTopology))
		if opts.CPUTopology != cputopology.Single && opts.CPUTopology != cputopology.Host {
			return 0, fmt.Errorf("unknown CPU topology %q; --cpu-topology is single or host", opts.CPUTopology)
		}
		opts.CPUTopologySet = true
		return consumed, nil
	}
	return 0, nil
}
