// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64
// +build amd64

package cpuid

import "testing"

// topologyTestFeatureSet is an Intel CPU of maximum basic leaf maxBasic with
// three cache levels, whose host values describe a package of 64 threads.
func topologyTestFeatureSet(vendor string, maxBasic uint32) FeatureSet {
	s := make(Static)
	var id [12]byte
	copy(id[:], vendor)
	bx, cx, dx := regsFromVendorID(id)
	s[In{Eax: uint32(vendorID)}] = Out{Eax: maxBasic, Ebx: bx, Ecx: cx, Edx: dx}
	s[In{Eax: uint32(extendedFunctionInfo)}] = Out{Eax: uint32(addressSizes)}
	// Host: APIC ID 0x2a, 64 IDs a package, CLFLUSH 8.
	s[In{Eax: uint32(featureInfo)}] = Out{Ebx: 0x2a<<24 | 64<<16 | 8<<8}
	for i, level := range []uint32{1, 2, 3} {
		s[In{Eax: uint32(intelDeterministicCacheParams), Ecx: uint32(i)}] = Out{Eax: 31<<26 | 63<<14 | level<<5 | 1}
	}
	s[In{Eax: uint32(addressSizes)}] = Out{Ecx: 0x6<<12 | 63}
	return FeatureSet{Function: s}
}

func TestSingleTopologyLeaves(t *testing.T) {
	fs := topologyTestFeatureSet("GenuineIntel", 0x1f)
	v := CPUTopologyView{CPU: 5, NumCPUs: 12, APICID: 5}
	fix := func(eax, ecx uint32) Out {
		in := In{Eax: eax, Ecx: ecx}
		return v.Fix(fs, in, fs.Query(in))
	}
	if got, want := fix(1, 0).Ebx, uint32(5<<24|16<<16|8<<8); got != want {
		t.Errorf("leaf 1 EBX = %#x, want %#x: APIC ID 5 of 16", got, want)
	}
	for i, wantSharing := range []uint32{0, 0, 15} {
		eax := fix(4, uint32(i)).Eax
		if cores, sharing := eax>>26, (eax>>14)&0xfff; cores != 15 || sharing != wantSharing {
			t.Errorf("leaf 4 subleaf %d: %d core IDs and %d sharing, less one; want 15 and %d", i, cores, sharing, wantSharing)
		}
		if eax&0x3fff != fs.Query(In{Eax: 4, Ecx: uint32(i)}).Eax&0x3fff {
			t.Errorf("leaf 4 subleaf %d: the cache's description changed", i)
		}
	}
	for _, leaf := range []uint32{0xb, 0x1f} {
		for subleaf, want := range []Out{
			{Eax: 0, Ebx: 1, Ecx: 1 << 8, Edx: 5},
			{Eax: 4, Ebx: 12, Ecx: 2<<8 | 1, Edx: 5},
			{Eax: 0, Ebx: 0, Ecx: 2, Edx: 5},
		} {
			if got := fix(leaf, uint32(subleaf)); got != want {
				t.Errorf("leaf %#x subleaf %d = %+v, want %+v", leaf, subleaf, got, want)
			}
		}
	}
}

func TestSingleTopologyLeavesBeyondMaximum(t *testing.T) {
	fs := topologyTestFeatureSet("GenuineIntel", 0xa)
	v := CPUTopologyView{CPU: 1, NumCPUs: 4, APICID: 1}
	in := In{Eax: 0xb, Ecx: 1}
	if got := v.Fix(fs, in, fs.Query(in)); got != (Out{}) {
		t.Errorf("leaf 0xb beyond the maximum basic leaf = %+v, want none", got)
	}
}

func TestSingleTopologyLeavesAMD(t *testing.T) {
	fs := topologyTestFeatureSet("AuthenticAMD", 0x10)
	v := CPUTopologyView{CPU: 3, NumCPUs: 24, APICID: 3}
	in := In{Eax: uint32(addressSizes)}
	if got, want := v.Fix(fs, in, fs.Query(in)).Ecx, uint32(5<<12|23); got != want {
		t.Errorf("leaf 0x80000008 ECX = %#x, want %#x", got, want)
	}
	// Leaf 4 is Intel's.
	in = In{Eax: 4, Ecx: 2}
	if got, want := v.Fix(fs, in, fs.Query(in)), fs.Query(in); got != want {
		t.Errorf("AMD leaf 4 = %+v, want %+v", got, want)
	}
}

func TestHostTopologyLeaves(t *testing.T) {
	fs := topologyTestFeatureSet("GenuineIntel", 0x1f)
	v := CPUTopologyView{Host: true, CPU: 70, NumCPUs: 96, APICID: 0x47}
	in := In{Eax: 1}
	if got, want := v.Fix(fs, in, fs.Query(in)).Ebx, uint32(0x47<<24|64<<16|8<<8); got != want {
		t.Errorf("leaf 1 EBX = %#x, want %#x: the host's, with APIC ID 0x47", got, want)
	}
	in = In{Eax: 4, Ecx: 2}
	if got, want := v.Fix(fs, in, fs.Query(in)), fs.Query(in); got != want {
		t.Errorf("leaf 4 = %+v, want the host's %+v", got, want)
	}
	in = In{Eax: 0xb}
	if got := v.Fix(fs, in, fs.Query(in)).Edx; got != 0x47 {
		t.Errorf("leaf 0xb EDX = %#x, want x2APIC ID 0x47", got)
	}
}
