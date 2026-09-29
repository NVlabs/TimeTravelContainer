// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build arm64

package cpuid

import "testing"

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
