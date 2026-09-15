// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0

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

const daemonCPUProfileVersion = 1

// daemonCPUProfile is the exact guest-visible instruction contract. Mode is
// descriptive; Features is authoritative and is never recomputed after a
// root or cluster has been initialized.
type daemonCPUProfile struct {
	Version  int      `json:"version"`
	Mode     string   `json:"mode"`
	Features []string `json:"features"`
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
		// Portable is deliberately conservative about large, optional XSAVE
		// components. The exact resulting list is persisted at cluster init;
		// later nodes must contain it rather than silently narrowing it.
		for _, feature := range host {
			if strings.HasPrefix(feature, "avx512") || strings.HasPrefix(feature, "amx_") {
				continue
			}
			features = append(features, feature)
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
	return daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: mode, Features: features}, nil
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
	if explicitlySet && (requested.Mode != stored.Mode || !slices.Equal(requested.Features, stored.Features)) {
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
