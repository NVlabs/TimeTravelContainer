// Copyright 2019 The gVisor Authors.
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

//go:build amd64
// +build amd64

package cpuid

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// makeFatureSet creates a new FeatureSet.
func makeFeatureSet(features ...Feature) FeatureSet {
	s := make(Static)
	for _, f := range features {
		s.Add(f)
	}
	return FeatureSet{
		Function: s,
	}
}

var (
	justFPU       = makeFeatureSet(X86FeatureFPU)
	justFPUandPAE = makeFeatureSet(X86FeatureFPU, X86FeaturePAE)
)

func TestSubtract(t *testing.T) {
	if left := justFPU.Subtract(justFPUandPAE); len(left) > 0 {
		t.Errorf("Got %q is not subset of %q, want left (%v) to be non-empty", justFPU.FlagString(), justFPUandPAE.FlagString(), left)
	}
	if left := justFPUandPAE.Subtract(justFPU); len(left) == 0 {
		t.Errorf("Got %q is a subset of %q, want left (%v) to be empty", justFPU.FlagString(), justFPUandPAE.FlagString(), left)
	}
}

// TODO(b/73346484): Run this test on a very old platform, and make sure more
// bits are enabled than just FPU and PAE. This test currently may not detect
// if HostFeatureSet gives back junk bits.
func TestHostFeatureSet(t *testing.T) {
	hostFeatures := HostFeatureSet()
	if justFPUandPAE.Subtract(hostFeatures) != nil {
		t.Errorf("Got invalid feature set %v from HostFeatureSet()", hostFeatures)
	}
}

func TestFixedExtendedState(t *testing.T) {
	hostFeatures := HostFeatureSet()
	fixedFeatures := hostFeatures.Fixed()
	for i, allowed := range allowedBasicFunctions {
		if !allowed {
			continue
		}
		in := In{Eax: uint32(i) + uint32(extendedStart)}
		h := hostFeatures.Query(in)
		f := fixedFeatures.Query(in)
		if h != f {
			t.Errorf("native: %x fixed: %x", h, f)
		}
	}

}

func featureSetWithIdentity(signature uint32, vendor, brand string) FeatureSet {
	static := make(Static)
	var vendorBytes [12]byte
	copy(vendorBytes[:], vendor)
	bx, cx, dx := regsFromVendorID(vendorBytes)
	static[In{Eax: uint32(vendorID)}] = Out{Eax: uint32(xSaveInfo), Ebx: bx, Ecx: cx, Edx: dx}
	static[In{Eax: uint32(featureInfo)}] = Out{Eax: signature, Ebx: 8 << 8}
	static[In{Eax: uint32(extendedFunctionInfo)}] = Out{Eax: uint32(processorBrandString4)}

	var brandBytes [48]byte
	copy(brandBytes[:], brand)
	for i, leaf := range [...]cpuidFunction{
		processorBrandString2,
		processorBrandString3,
		processorBrandString4,
	} {
		offset := i * 16
		static[In{Eax: uint32(leaf)}] = Out{
			Eax: binary.LittleEndian.Uint32(brandBytes[offset:]),
			Ebx: binary.LittleEndian.Uint32(brandBytes[offset+4:]),
			Ecx: binary.LittleEndian.Uint32(brandBytes[offset+8:]),
			Edx: binary.LittleEndian.Uint32(brandBytes[offset+12:]),
		}
	}
	return FeatureSet{Function: static}
}

func TestWriteCPUInfoIdentity(t *testing.T) {
	tests := []struct {
		name      string
		signature uint32
		vendor    string
		brand     string
		want      []string
	}{
		{
			name:      "Intel",
			signature: 0x000806f8,
			vendor:    "GenuineIntel",
			brand:     "Intel(R) Xeon(R) Platinum 8488C",
			want: []string{
				"vendor_id\t: GenuineIntel\n",
				"cpu family\t: 6\n",
				"model\t\t: 143\n",
				"model name\t: Intel(R) Xeon(R) Platinum 8488C\n",
				"stepping\t: 8\n",
			},
		},
		{
			name:      "AMD extended family",
			signature: 0x00a10f12,
			vendor:    "AuthenticAMD",
			brand:     "AMD EPYC Test Processor",
			want: []string{
				"vendor_id\t: AuthenticAMD\n",
				"cpu family\t: 25\n",
				"model\t\t: 17\n",
				"model name\t: AMD EPYC Test Processor\n",
				"stepping\t: 2\n",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			features := featureSetWithIdentity(test.signature, test.vendor, test.brand)
			var output bytes.Buffer
			features.WriteCPUInfoTo(0, 1, &output)
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Errorf("WriteCPUInfoTo output missing %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func TestHasFeature(t *testing.T) {
	if !justFPU.HasFeature(X86FeatureFPU) {
		t.Errorf("HasFeature failed, %q should contain %v", justFPU.FlagString(), X86FeatureFPU)
	}
	if justFPU.HasFeature(X86FeatureAVX) {
		t.Errorf("HasFeature failed, %q should not contain %v", justFPU.FlagString(), X86FeatureAVX)
	}
}

func TestAdd(t *testing.T) {
	// Test a basic insertion into the FeatureSet.
	testFeatures := makeFeatureSet(X86FeatureCLFSH)
	if !testFeatures.HasFeature(X86FeatureCLFSH) {
		t.Errorf("Add failed, got %v want set with %v", testFeatures, X86FeatureCLFSH)
	}

	// Test that duplicates are ignored.
	testFeatures.Function.(Static).Add(X86FeatureCLFSH)
	if !testFeatures.HasFeature(X86FeatureCLFSH) {
		t.Errorf("Duplicate add removed entry, got %v want set with %v", testFeatures, X86FeatureCLFSH)
	}
}

func TestRemove(t *testing.T) {
	// Try removing the last feature.
	testFeatures := makeFeatureSet(X86FeatureFPU, X86FeaturePAE)
	testFeatures.Function.(Static).Remove(X86FeaturePAE)
	if !testFeatures.HasFeature(X86FeatureFPU) || testFeatures.HasFeature(X86FeaturePAE) {
		t.Errorf("Remove failed, got %q want %q", testFeatures.FlagString(), justFPU.FlagString())
	}

	// Try removing a feature not in the set.
	testFeatures.Function.(Static).Remove(X86FeatureRDRAND)
	if !testFeatures.HasFeature(X86FeatureFPU) {
		t.Errorf("Remove failed, got %q want %q", testFeatures.FlagString(), justFPU.FlagString())
	}
}

func TestWithoutFSGSBASE(t *testing.T) {
	fs := makeFeatureSet(X86FeatureFSGSBase, X86FeatureFPU)
	fs.hwCap.hwCap2 = HWCAP2_FSGSBASE
	masked := fs.WithoutFSGSBASE()
	if masked.HasFeature(X86FeatureFSGSBase) {
		t.Fatalf("WithoutFSGSBASE left CPUID feature enabled: %q", masked.FlagString())
	}
	if masked.hwCap.hwCap2&HWCAP2_FSGSBASE != 0 {
		t.Fatalf("WithoutFSGSBASE left AT_HWCAP2 bit enabled: %#x", masked.hwCap.hwCap2)
	}
	if !masked.HasFeature(X86FeatureFPU) {
		t.Fatalf("WithoutFSGSBASE removed unrelated features: %q", masked.FlagString())
	}
}
