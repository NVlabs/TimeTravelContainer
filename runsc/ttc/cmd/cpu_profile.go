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

func validateDaemonCPUProfile(profile daemonCPUProfile, runtimePlatform string) error {
	if profile.Version != daemonCPUProfileVersion || profile.Mode == "" || len(profile.Features) == 0 || !slices.IsSorted(profile.Features) {
		return errors.New("invalid TTC CPU profile")
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

func validateClusterCPUEnforcement(runtimePlatform string, cpuidFaulting bool) error {
	if runtimePlatform == "systrap" && !cpuidFaulting {
		return errors.New("systrap cluster mode requires host CPUID faulting (ARCH_SET_CPUID); this host cannot enforce a migration-safe guest CPU profile")
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
	if explicitlySet && (requested.Mode != stored.Mode || !slices.Equal(requested.Features, stored.Features) || requested.XsaveABI != stored.XsaveABI) {
		return daemonCPUProfile{}, fmt.Errorf("requested CPU profile differs from immutable profile %q stored in TTC_ROOT", stored.Mode)
	}
	return stored, nil
}

func (p daemonCPUProfile) annotation() string {
	return strings.Join(p.Features, ",")
}

func consumeDaemonCPUOption(args []string, opts *daemonClusterOptions) (int, error) {
	if len(args) == 0 {
		return 0, nil
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
	return 0, nil
}
