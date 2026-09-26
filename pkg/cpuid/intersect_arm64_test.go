// Copyright 2026 The gVisor Authors.
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
