// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64

package cpuid

import "math/bits"

// The leaves, beyond those native_amd64.go names, that describe CPU topology.
const (
	v2ExtendedTopology = 0x1f
	amdExtendedAPICID  = 0x8000001e
)

// Fix returns out, which fs answers CPUID in with, with the leaves that
// describe the CPU topology made to describe v's, for a task that executes
// CPUID on v.CPU: its APIC IDs, and how many threads and cores share a
// package and its caches, as /proc/cpuinfo and /sys show them.
func (v CPUTopologyView) Fix(fs FeatureSet, in In, out Out) Out {
	maxBasic, _, _, _ := fs.query(vendorID)
	maxExtended, _, _, _ := fs.query(extendedFunctionInfo)
	if in.Eax < uint32(extendedStart) && in.Eax > maxBasic || in.Eax >= uint32(extendedStart) && in.Eax > maxExtended {
		return out
	}
	// The single topology's package of NumCPUs cores numbers them by
	// shift bits of their APIC IDs.
	shift := uint32(bits.Len32(max(v.NumCPUs, 1) - 1))
	switch in.Eax {
	case uint32(featureInfo):
		// EBX[31:24] is the initial APIC ID; EBX[23:16] how many APIC IDs a
		// package has.
		out.Ebx = out.Ebx&0x00ffffff | (v.APICID&0xff)<<24
		if !v.Host {
			out.Ebx = out.Ebx&0xff00ffff | min(uint32(1)<<shift, 0xff)<<16
		}
	case uint32(intelDeterministicCacheParams):
		if v.Host || !fs.Intel() || out.Eax&0x1f == 0 {
			break
		}
		// EAX[31:26] is how many core IDs a package has, less one; EAX[25:14]
		// how many APIC IDs share the cache, less one: a core's caches are
		// its own thread's, the last level all of the package's.
		sharing := uint32(0)
		if level := (out.Eax >> 5) & 0x7; level > 2 {
			sharing = min(uint32(1)<<shift, 4096) - 1
		}
		out.Eax = out.Eax&0x3fff | (min(uint32(1)<<shift, 64)-1)<<26 | sharing<<14
	case uint32(intelX2APICInfo), v2ExtendedTopology:
		if v.Host {
			out = native(in)
			out.Edx = v.APICID
			break
		}
		// Level 0 is the thread of each core; level 1, the cores of the
		// package.
		switch in.Ecx {
		case 0:
			out = Out{Eax: 0, Ebx: 1, Ecx: 1 << 8, Edx: v.APICID}
		case 1:
			out = Out{Eax: shift, Ebx: min(v.NumCPUs, 0xffff), Ecx: 2<<8 | 1, Edx: v.APICID}
		default:
			out = Out{Ecx: in.Ecx & 0xff, Edx: v.APICID}
		}
	case amdExtendedAPICID:
		if !v.Host || !fs.AMD() {
			break
		}
		// EAX is the extended APIC ID, EBX[7:0] the core ID and ECX[7:0] the
		// node ID of the CPU, the rest the same on every CPU.
		out = native(in)
		out.Eax = v.APICID
		out.Ebx = out.Ebx&^0xff | v.Core&0xff
		out.Ecx = out.Ecx&^0xff | v.Node&0xff
	case uint32(addressSizes):
		if v.Host || !fs.AMD() {
			break
		}
		// ECX[15:12] is how many bits of the APIC ID number cores, and
		// ECX[7:0] how many cores the package has, less one.
		out.Ecx = out.Ecx&^0xf0ff | min(shift, 0xf)<<12 | (min(v.NumCPUs, 0x100) - 1)
	}
	return out
}
