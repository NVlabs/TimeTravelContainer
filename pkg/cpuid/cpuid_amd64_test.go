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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

func TestUnsetXsaveSubfeatureDoesNotEnableXsave(t *testing.T) {
	features := make(Static)
	X86FeatureXSAVEOPT.Unset(features)
	got := features.ToFeatureSet()
	if got.HasFeature(X86FeatureXSAVE) {
		t.Fatal("unsetting XSAVEOPT unexpectedly enabled XSAVE")
	}
	if got.HasFeature(X86FeatureXSAVEOPT) {
		t.Fatal("XSAVEOPT remains enabled")
	}
}

func TestIntersectWithoutXsaveClearsEnumerationLeaf(t *testing.T) {
	allowed := map[Feature]struct{}{
		X86FeatureFPU: {}, X86FeatureFXSR: {}, X86FeatureSSE: {}, X86FeatureSSE2: {},
	}
	got, err := HostFeatureSet().Intersect(allowed)
	if err != nil {
		t.Fatal(err)
	}
	for subleaf := uint32(0); subleaf < xSaveInfoNumLeaves; subleaf++ {
		if out := got.Query(In{Eax: uint32(xSaveInfo), Ecx: subleaf}); out != (Out{}) {
			t.Fatalf("CPUID.0xd.%d=%+v, want zero without XSAVE", subleaf, out)
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

func TestX86Family(t *testing.T) {
	tests := []struct {
		name string
		sig  uint32
		want uint32
	}{
		{name: "Family6", sig: 0x6 << 8, want: 6},
		{name: "Family6IgnoresExtendedFamily", sig: 0x0a<<20 | 0x6<<8, want: 6},
		{name: "Family0Fh", sig: 0xf << 8, want: 15},
		{name: "Family19h", sig: 0x0a<<20 | 0xf<<8, want: 25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := x86Family(tc.sig); got != tc.want {
				t.Errorf("x86Family(%#x) = %d, want %d", tc.sig, got, tc.want)
			}
		})
	}
}

func TestWriteCPUInfoFamilyEPYC(t *testing.T) {
	// Overlay an AMD EPYC 7R13-like signature (family 19h) on the host
	// feature set so WriteCPUInfoTo has the rest of the leaves it needs.
	s := HostFeatureSet().ToStatic()
	in := In{Eax: uint32(featureInfo)}
	out := s.Query(in)
	const familyMask = 0x0ff00f00 // extended family 27:20 and family 11:8
	out.Eax = (out.Eax &^ familyMask) | 0x0a<<20 | 0xf<<8
	s.Set(in, out)

	var buf strings.Builder
	s.ToFeatureSet().WriteCPUInfoTo(0, 1, &buf)
	got := buf.String()
	const want = "cpu family\t: 25\n"
	if !strings.Contains(got, want) {
		t.Errorf("WriteCPUInfoTo() missing %q, got:\n%s", want, got)
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

func TestUnsetFSGSBASEHWCap(t *testing.T) {
	fs := makeFeatureSet(X86FeatureFSGSBase, X86FeatureFPU)
	fs.hwCap.hwCap2 = HWCAP2_FSGSBASE
	masked := fs.UnsetFSGSBASE()
	if masked.HasFeature(X86FeatureFSGSBase) {
		t.Fatalf("UnsetFSGSBASE left CPUID feature enabled: %q", masked.FlagString())
	}
	if masked.hwCap.hwCap2&HWCAP2_FSGSBASE != 0 {
		t.Fatalf("UnsetFSGSBASE left AT_HWCAP2 bit enabled: %#x", masked.hwCap.hwCap2)
	}
	if !masked.HasFeature(X86FeatureFPU) {
		t.Fatalf("UnsetFSGSBASE removed unrelated features: %q", masked.FlagString())
	}
}

func TestCPUIDFeaturesRecordFeatureRegisters(t *testing.T) {
	leaves := map[In]Out{
		// GenuineIntel, with leaves up to 0xd.
		{Eax: 0}: {Eax: 0xd, Ebx: 0x756e6547, Ecx: 0x6c65746e, Edx: 0x49656e69},
		// The signature and the APIC ID are not features, nor is the
		// hypervisor bit.
		{Eax: 1}: {Eax: 0x806f8, Ebx: 0x12345678, Ecx: 1<<31 | 1<<28, Edx: 1 << 25},
		// One further subleaf; MD_CLEAR is a speculation control.
		{Eax: 7}:           {Eax: 1, Ebx: 1 << 5, Edx: 1<<10 | 1<<23},
		{Eax: 7, Ecx: 1}:   {Eax: 1 << 4},
		{Eax: 7, Ecx: 2}:   {Edx: 1 << 8},
		{Eax: 0xd, Ecx: 1}: {Eax: 0xf},
		// Above the highest basic leaf.
		{Eax: 0x14}:       {Ebx: 1 << 4},
		{Eax: 0x80000000}: {Eax: 0x80000008},
		{Eax: 0x80000001}: {Ecx: 1 << 5, Edx: 1 << 29},
		// IBPB is a speculation control.
		{Eax: 0x80000008}: {Ebx: 1<<0 | 1<<12},
		// Above the highest extended leaf.
		{Eax: 0x80000021}: {Eax: 1},
	}
	got := cpuidFeatures(func(in In) Out { return leaves[in] })
	want := "vendor=GenuineIntel;1.0.ecx=10000000;1.0.edx=2000000;7.0.eax=1;7.0.ebx=20;7.0.edx=800000;7.1.eax=10;d.1.eax=f;80000001.0.ecx=20;80000001.0.edx=20000000;80000008.0.ebx=1"
	if got != want {
		t.Errorf("cpuidFeatures=%q, want %q", got, want)
	}
}

func TestHostCPUIDFeatures(t *testing.T) {
	got := HostCPUIDFeatures()
	if !strings.HasPrefix(got, "vendor=") || !strings.Contains(got, ";1.0.edx=") {
		t.Fatalf("HostCPUIDFeatures=%q, want the vendor and leaf 1", got)
	}
	if again := HostCPUIDFeatures(); again != got {
		t.Fatalf("HostCPUIDFeatures changed from %q to %q", got, again)
	}
}

func TestUnsetFSGSBASE(t *testing.T) {
	hostFeatures := HostFeatureSet()
	guestFeatures := hostFeatures.UnsetFSGSBASE()

	if guestFeatures.HasFeature(X86FeatureFSGSBase) {
		t.Errorf("UnsetFSGSBASE failed: X86FeatureFSGSBase should be disabled")
	}

	out := guestFeatures.Query(In{Eax: 7, Ecx: 0})
	if out.Ebx&1 != 0 {
		t.Errorf("UnsetFSGSBASE failed: CPUID.(EAX=7, ECX=0).EBX bit 0 (FSGSBASE) should be 0, got %x", out.Ebx)
	}
}
