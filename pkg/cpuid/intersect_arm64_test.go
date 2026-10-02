// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build arm64

package cpuid

import (
	"slices"
	"strings"
	"testing"
)

func TestIntersectMasksHWCap(t *testing.T) {
	fs := FeatureSet{hwCap: hwCap{hwCap1: HWCAP_FP | HWCAP_ASIMD | HWCAP_AES, hwCap2: 1}}
	got, err := fs.Intersect(map[Feature]struct{}{ARM64FeatureFP: {}, ARM64FeatureAES: {}, ARM64FeatureSHA1: {}})
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(HWCAP_FP | HWCAP_AES); got.hwCap.hwCap1 != want {
		t.Errorf("hwCap1 = %#x, want %#x", got.hwCap.hwCap1, want)
	}
	if got.hwCap.hwCap2 != fs.hwCap.hwCap2 {
		t.Errorf("hwCap2 = %#x, want unchanged %#x", got.hwCap.hwCap2, fs.hwCap.hwCap2)
	}
	if !got.HasFeature(ARM64FeatureAES) || got.HasFeature(ARM64FeatureASIMD) || got.HasFeature(ARM64FeatureSHA1) {
		t.Errorf("intersection features = %q", got.FlagString())
	}
	if _, err := fs.Intersect(map[Feature]struct{}{64: {}}); err == nil {
		t.Error("Intersect accepted a feature without a HWCAP bit")
	}
}

func TestCPUInfoListsOnlyGuestFeatures(t *testing.T) {
	// The host has SVE, whose state gVisor does not keep: AT_HWCAP does not
	// give it to guests (AllowedHWCap1), and /proc/cpuinfo must not list it,
	// or a compiler tuning for the CPU (gcc's -mcpu=native) uses it.
	fs := FeatureSet{hwCap: hwCap{hwCap1: HWCAP_FP | HWCAP_ASIMD | HWCAP_SVE}}
	var b strings.Builder
	fs.WriteCPUInfoTo(0, 1, &b)
	var features []string
	for _, line := range strings.Split(b.String(), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "Features" {
			features = strings.Fields(value)
		}
	}
	if !slices.Contains(features, "fp") || !slices.Contains(features, "asimd") || slices.Contains(features, "sve") {
		t.Errorf("Features = %q, want fp and asimd, and not sve", features)
	}
}
