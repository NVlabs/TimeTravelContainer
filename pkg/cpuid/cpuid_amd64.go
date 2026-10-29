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
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// FeatureSet defines features in terms of CPUID leaves and bits.
// The kernel also exposes the presence of features to userspace through
// a set of flags(HWCAP/HWCAP2) bits, exposed in the auxiliary vector, which
// are necessary to read for some features (e.g. FSGSBASE).
//
// Common references:
//
// Intel:
//   - Intel SDM Volume 2, Chapter 3.2 "CPUID" (more up-to-date)
//   - Intel Application Note 485 (more detailed)
//
// AMD:
//   - AMD64 APM Volume 3, Appendix 3 "Obtaining Processor Information ..."
//
// +stateify savable
type FeatureSet struct {
	// Function is the underlying CPUID Function.
	//
	// This is exported to allow direct calls of the underlying CPUID
	// function, where required.
	Function `state:".(Static)"`
	// hwCap stores HWCAP1/2 exposed from the elf auxiliary vector.
	hwCap hwCap
}

// saveFunction saves the function as a static query.
func (fs *FeatureSet) saveFunction() Static {
	if s, ok := fs.Function.(Static); ok {
		return s
	}
	return fs.ToStatic()
}

// loadFunction saves the function as a static query.
func (fs *FeatureSet) loadFunction(_ context.Context, s Static) {
	fs.Function = s
}

// Helper to convert 3 regs into 12-byte vendor ID.
//
//go:nosplit
func vendorIDFromRegs(bx, cx, dx uint32) (r [12]byte) {
	for i := uint(0); i < 4; i++ {
		b := byte(bx >> (i * 8))
		r[i] = b
	}

	for i := uint(0); i < 4; i++ {
		b := byte(dx >> (i * 8))
		r[4+i] = b
	}

	for i := uint(0); i < 4; i++ {
		b := byte(cx >> (i * 8))
		r[8+i] = b
	}

	return r
}

// Helper to merge a 12-byte vendor ID back to registers.
//
// Used by static_amd64.go.
func regsFromVendorID(r [12]byte) (bx, cx, dx uint32) {
	bx |= uint32(r[0])
	bx |= uint32(r[1]) << 8
	bx |= uint32(r[2]) << 16
	bx |= uint32(r[3]) << 24
	dx |= uint32(r[4])
	dx |= uint32(r[5]) << 8
	dx |= uint32(r[6]) << 16
	dx |= uint32(r[7]) << 24
	cx |= uint32(r[8])
	cx |= uint32(r[9]) << 8
	cx |= uint32(r[10]) << 16
	cx |= uint32(r[11]) << 24
	return
}

// VendorID is the 12-char string returned in ebx:edx:ecx for eax=0.
//
//go:nosplit
func (fs FeatureSet) VendorID() [12]byte {
	_, bx, cx, dx := fs.query(vendorID)
	return vendorIDFromRegs(bx, cx, dx)
}

// Helper to deconstruct signature dword.
//
//go:nosplit
func signatureSplit(v uint32) (ef, em, pt, f, m, sid uint8) {
	sid = uint8(v & 0xf)
	m = uint8(v>>4) & 0xf
	f = uint8(v>>8) & 0xf
	pt = uint8(v>>12) & 0x3
	em = uint8(v>>16) & 0xf
	ef = uint8(v >> 20)
	return
}

// ExtendedFamily is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) ExtendedFamily() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	ef, _, _, _, _, _ := signatureSplit(ax)
	return ef
}

// ExtendedModel is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) ExtendedModel() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	_, em, _, _, _, _ := signatureSplit(ax)
	return em
}

// ProcessorType is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) ProcessorType() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	_, _, pt, _, _, _ := signatureSplit(ax)
	return pt
}

// Family is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) Family() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	_, _, _, f, _, _ := signatureSplit(ax)
	return f
}

// Model is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) Model() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	_, _, _, _, m, _ := signatureSplit(ax)
	return m
}

// SteppingID is part of the processor signature.
//
//go:nosplit
func (fs FeatureSet) SteppingID() uint8 {
	ax, _, _, _ := fs.query(featureInfo)
	_, _, _, _, _, sid := signatureSplit(ax)
	return sid
}

// ProcessorBrandString returns the processor brand string exposed through
// extended CPUID leaves 0x80000002 through 0x80000004. It returns an empty
// string when the leaves are unavailable or contain no brand string.
func (fs FeatureSet) ProcessorBrandString() string {
	maxExtended, _, _, _ := fs.query(extendedFunctionInfo)
	if maxExtended < uint32(processorBrandString4) {
		return ""
	}

	var brand [48]byte
	for i, leaf := range [...]cpuidFunction{
		processorBrandString2,
		processorBrandString3,
		processorBrandString4,
	} {
		ax, bx, cx, dx := fs.query(leaf)
		offset := i * 16
		binary.LittleEndian.PutUint32(brand[offset:], ax)
		binary.LittleEndian.PutUint32(brand[offset+4:], bx)
		binary.LittleEndian.PutUint32(brand[offset+8:], cx)
		binary.LittleEndian.PutUint32(brand[offset+12:], dx)
	}
	return strings.TrimSpace(strings.TrimRight(string(brand[:]), "\x00"))
}

// displayFamilyModel returns the family and model numbers as Linux displays
// them in /proc/cpuinfo. Extended family applies only to base family 0xf;
// extended model applies only to base families 0x6 and 0xf.
func displayFamilyModel(signature uint32) (uint16, uint16) {
	ef, em, _, f, m, _ := signatureSplit(signature)
	family := uint16(f)
	if f == 0xf {
		family += uint16(ef)
	}
	model := uint16(m)
	if f == 0x6 || f == 0xf {
		model |= uint16(em) << 4
	}
	return family, model
}

// VirtualAddressBits returns the number of bits available for virtual
// addresses.
//
//go:nosplit
func (fs FeatureSet) VirtualAddressBits() uint32 {
	ax, _, _, _ := fs.query(addressSizes)
	return (ax >> 8) & 0xff
}

// PhysicalAddressBits returns the number of bits available for physical
// addresses.
//
//go:nosplit
func (fs FeatureSet) PhysicalAddressBits() uint32 {
	ax, _, _, _ := fs.query(addressSizes)
	physBits := ax & 0xff
	if !fs.AMD() {
		return physBits
	}

	maxExtended, _, _, _ := fs.query(extendedFunctionInfo)
	if maxExtended < uint32(amdMemoryEncryptionInfo) {
		return physBits
	}

	memEncAX, memEncBX, _, _ := fs.query(amdMemoryEncryptionInfo)
	if memEncAX&amdMemoryEncryptionFeatureMask == 0 {
		return physBits
	}
	// AMD memory encryption reduces usable physical address width by the
	// CPUID-reported amount. Match Linux's
	// arch/x86/kernel/cpu/amd.c:early_detect_mem_encrypt().
	return physBits - ((memEncBX >> amdPhysAddrReductionShift) & amdPhysAddrReductionMask)
}

// CacheType describes the type of a cache, as returned in eax[4:0] for eax=4.
type CacheType uint8

const (
	// cacheNull indicates that there are no more entries.
	cacheNull CacheType = iota

	// CacheData is a data cache.
	CacheData

	// CacheInstruction is an instruction cache.
	CacheInstruction

	// CacheUnified is a unified instruction and data cache.
	CacheUnified
)

// Cache describes the parameters of a single cache on the system.
//
// This is returned by the Caches method on FeatureSet.
type Cache struct {
	// Level is the hierarchical level of this cache (L1, L2, etc).
	Level uint32

	// Type is the type of cache.
	Type CacheType

	// FullyAssociative indicates that entries may be placed in any block.
	FullyAssociative bool

	// Partitions is the number of physical partitions in the cache.
	Partitions uint32

	// Ways is the number of ways of associativity in the cache.
	Ways uint32

	// Sets is the number of sets in the cache.
	Sets uint32

	// InvalidateHierarchical indicates that WBINVD/INVD from threads
	// sharing this cache acts upon lower level caches for threads sharing
	// this cache.
	InvalidateHierarchical bool

	// Inclusive indicates that this cache is inclusive of lower cache
	// levels.
	Inclusive bool

	// DirectMapped indicates that this cache is directly mapped from
	// address, rather than using a hash function.
	DirectMapped bool
}

// Caches describes the caches on the CPU.
//
// Only supported on Intel; requires allocation.
func (fs FeatureSet) Caches() (caches []Cache) {
	if !fs.Intel() {
		return
	}
	// Check against the cache line, which should be consistent.
	cacheLine := fs.CacheLine()
	for i := uint32(0); ; i++ {
		out := fs.Query(In{
			Eax: uint32(intelDeterministicCacheParams),
			Ecx: i,
		})
		t := CacheType(out.Eax & 0xf)
		if t == cacheNull {
			break
		}
		lineSize := (out.Ebx & 0xfff) + 1
		if lineSize != cacheLine {
			panic(fmt.Sprintf("Mismatched cache line size: %d vs %d", lineSize, cacheLine))
		}
		caches = append(caches, Cache{
			Type:                   t,
			Level:                  (out.Eax >> 5) & 0x7,
			FullyAssociative:       ((out.Eax >> 9) & 1) == 1,
			Partitions:             ((out.Ebx >> 12) & 0x3ff) + 1,
			Ways:                   ((out.Ebx >> 22) & 0x3ff) + 1,
			Sets:                   out.Ecx + 1,
			InvalidateHierarchical: (out.Edx & 1) == 0,
			Inclusive:              ((out.Edx >> 1) & 1) == 1,
			DirectMapped:           ((out.Edx >> 2) & 1) == 0,
		})
	}
	return
}

// CacheLine is the size of a cache line in bytes.
//
// All caches use the same line size. This is not enforced in the CPUID
// encoding, but is true on all known x86 processors.
//
//go:nosplit
func (fs FeatureSet) CacheLine() uint32 {
	_, bx, _, _ := fs.query(featureInfo)
	return 8 * (bx >> 8) & 0xff
}

// HasFeature tests whether or not a feature is in the given feature set.
//
// This function is safe to call from a nosplit context, as long as the
// FeatureSet does not have any masked features.
//
//go:nosplit
func (fs FeatureSet) HasFeature(feature Feature) bool {
	return feature.check(fs)
}

// x86Family returns the CPU family printed in /proc/cpuinfo from a CPUID
// leaf 1 EAX signature.
//
// This matches Linux arch/x86/lib/cpu.c:x86_family(). When the 4-bit
// family ID is 0xf, the 8-bit extended family is added.
func x86Family(sig uint32) uint32 {
	family := (sig >> 8) & 0xf
	if family == 0xf {
		family += (sig >> 20) & 0xff
	}
	return family
}

// WriteCPUInfoTo is to generate a section of one cpu in /proc/cpuinfo. This is
// a minimal /proc/cpuinfo, it is missing some fields like "microcode" that are
// not always printed in Linux. Several fields are simply made up.
func (fs FeatureSet) WriteCPUInfoTo(cpu, numCPU uint, w io.Writer) {
	// Pretend all CPUs are in the same socket, and each CPU is a distinct
	// core (rather than a hyperthread).
	fs.WriteCPUInfoPlacedTo(cpu, CPUPlace{Core: uint32(cpu), Siblings: uint32(numCPU), Cores: uint32(numCPU), APICID: uint32(cpu)}, w)
}

// WriteCPUInfoPlacedTo is WriteCPUInfoTo for a CPU at place.
func (fs FeatureSet) WriteCPUInfoPlacedTo(cpu uint, place CPUPlace, w io.Writer) {
	// Avoid many redundant calls here, since this can occasionally appear
	// in the hot path. Read all basic information up front, see above.
	ax, _, _, _ := fs.query(featureInfo)
	_, _, _, _, _, stepping := signatureSplit(ax)
	family, model := displayFamilyModel(ax)
	vendor := fs.VendorID()
	modelName := fs.ProcessorBrandString()
	if modelName == "" {
		modelName = string(vendor[:])
	}
	fmt.Fprintf(w, "processor\t: %d\n", cpu)
	fmt.Fprintf(w, "vendor_id\t: %s\n", string(vendor[:]))
	fmt.Fprintf(w, "cpu family\t: %d\n", family)
	fmt.Fprintf(w, "model\t\t: %d\n", model)
	fmt.Fprintf(w, "model name\t: %s\n", modelName)
	fmt.Fprintf(w, "stepping\t: %d\n", stepping)
	fmt.Fprintf(w, "cpu MHz\t\t: %.3f\n", cpuFreqMHz)
	// Pretend the CPU has 8192 KB of cache. Note that real /proc/cpuinfo exposes total L3 cache
	// size on Intel and per-core L2 cache size on AMD (as of Linux 6.1.0), so the value of this
	// field is not really important in practice. Any value that is chosen here will be wrong
	// by an order of magnitude on a significant chunk of x86 machines.
	// 8192 KB is selected because it is a reasonable size that will be effectively usable on
	// lightly loaded machines - most machines have 1-4MB of L3 cache per core.
	fmt.Fprintf(w, "cache size\t: 8192 KB\n")
	fmt.Fprintf(w, "physical id\t: %d\n", place.Package)
	fmt.Fprintf(w, "siblings\t: %d\n", place.Siblings)
	fmt.Fprintf(w, "core id\t\t: %d\n", place.Core)
	fmt.Fprintf(w, "cpu cores\t: %d\n", place.Cores)
	fmt.Fprintf(w, "apicid\t\t: %d\n", place.APICID)
	fmt.Fprintf(w, "initial apicid\t: %d\n", place.APICID)
	fmt.Fprintf(w, "fpu\t\t: yes\n")
	fmt.Fprintf(w, "fpu_exception\t: yes\n")
	fmt.Fprintf(w, "cpuid level\t: %d\n", uint32(xSaveInfo)) // Same as ax in vendorID.
	fmt.Fprintf(w, "wp\t\t: yes\n")
	fmt.Fprintf(w, "flags\t\t: %s\n", fs.FlagString())
	fmt.Fprintf(w, "bogomips\t: %.02f\n", cpuFreqMHz) // It's bogus anyway.
	fmt.Fprintf(w, "clflush size\t: %d\n", fs.CacheLine())
	fmt.Fprintf(w, "cache_alignment\t: %d\n", fs.CacheLine())
	fmt.Fprintf(w, "address sizes\t: %d bits physical, %d bits virtual\n", 46, 48)
	fmt.Fprintf(w, "power management:\n") // This is always here, but can be blank.
	fmt.Fprintf(w, "\n")                  // The /proc/cpuinfo file ends with an extra newline.
}

var (
	authenticAMD = [12]byte{'A', 'u', 't', 'h', 'e', 'n', 't', 'i', 'c', 'A', 'M', 'D'}
	genuineIntel = [12]byte{'G', 'e', 'n', 'u', 'i', 'n', 'e', 'I', 'n', 't', 'e', 'l'}
)

// AMD returns true if fs describes an AMD CPU.
//
//go:nosplit
func (fs FeatureSet) AMD() bool {
	return fs.VendorID() == authenticAMD
}

// Intel returns true if fs describes an Intel CPU.
//
//go:nosplit
func (fs FeatureSet) Intel() bool {
	return fs.VendorID() == genuineIntel
}

// AMX state component numbers, which are both the XCR0 bits that enable
// them and the leaf 0xD subleaves that describe them.
const (
	amxTileCfgComponent  = 17
	amxTileDataComponent = 18
)

// Leaf 0 of xsaveinfo function returns the size for currently
// enabled xsave features in ebx, the maximum size if all valid
// features are saved with xsave in ecx, and valid XCR0 bits in
// edx:eax.
//
// If xSaveInfo isn't supported, cpuid will not fault but will
// return bogus values.
var (
	xsaveSize       = native(In{Eax: uint32(xSaveInfo)}).Ebx
	maxXsaveSize    = native(In{Eax: uint32(xSaveInfo)}).Ecx
	amxTileCfgSize  = native(In{Eax: uint32(xSaveInfo), Ecx: amxTileCfgComponent}).Eax
	amxTileDataSize = native(In{Eax: uint32(xSaveInfo), Ecx: amxTileDataComponent}).Eax
)

// HostXsaveABI identifies the userspace XSAVE ABI of the physical CPU. XSAVE
// and XGETBV execute natively, so feature-bit masking alone cannot make their
// buffer sizes or layouts portable. Applications may cache these values at
// startup; a migration must preserve them for the lifetime of the process.
func HostXsaveABI() string {
	if !HostFeatureSet().UseXsave() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xcr0=%x", xgetbv(0))
	for leaf := uint32(0); leaf < 64; leaf++ {
		out := native(In{Eax: uint32(xSaveInfo), Ecx: leaf})
		if out != (Out{}) {
			fmt.Fprintf(&b, ";%x=%x,%x,%x,%x", leaf, out.Eax, out.Ebx, out.Ecx, out.Edx)
		}
	}
	return b.String()
}

// HostCPUIDFeatures returns, as a canonical string, the registers of the
// host's own CPUID that enumerate what an application may execute. An
// application of a systrap sandbox on a host without CPUID faulting reads
// them itself instead of its sandbox's FeatureSet, including leaves no
// FeatureSet models, such as leaf 7's later subleaves; hosts whose strings
// are equal show it the same CPU.
func HostCPUIDFeatures() string {
	return cpuidFeatures(native)
}

// cpuidPlatformBits are the bits of the registers cpuidFeatures records that
// report the hypervisor or speculation controls rather than instructions.
// Hypervisors and microcode set them, so hosts with the same CPU may differ
// in them.
var cpuidPlatformBits = map[In]Out{
	// The hypervisor bit.
	{Eax: uint32(featureInfo)}: {Ecx: 1 << 31},
	// SRBDS_CTRL, MD_CLEAR, RTM_ALWAYS_ABORT, TSX_FORCE_ABORT, and SPEC_CTRL
	// through SPEC_CTRL_SSBD.
	{Eax: uint32(extendedFeatureInfo)}: {Edx: 1<<9 | 1<<10 | 1<<11 | 1<<13 | 0x3f<<26},
	// PSFD, IPRED_CTRL, RRSBA_CTRL, DDPD_U, BHI_CTRL, MCDT_NO,
	// UC_LOCK_DISABLE and MONITOR_MITG_NO.
	{Eax: uint32(extendedFeatureInfo), Ecx: 2}: {Edx: 0xff},
	// AMD's speculation controls from IBPB on, PPIN and CPPC.
	{Eax: uint32(addressSizes)}: {Ebx: 0xfffff000},
	// AMD's SBPB, IBPB_BRTYPE and SRSO bits.
	{Eax: uint32(extendedStart) + 0x21}: {Eax: 0x1f << 27},
}

// cpuidFeatures records the registers HostCPUIDFeatures does, read with
// query, leaving out zeros.
func cpuidFeatures(query func(In) Out) string {
	var b strings.Builder
	vendor := query(In{Eax: uint32(vendorID)})
	id := vendorIDFromRegs(vendor.Ebx, vendor.Ecx, vendor.Edx)
	fmt.Fprintf(&b, "vendor=%s", id[:])
	record := func(in In, registers ...int) Out {
		out := query(in)
		ignore := cpuidPlatformBits[in]
		values := [4]uint32{out.Eax &^ ignore.Eax, out.Ebx &^ ignore.Ebx, out.Ecx &^ ignore.Ecx, out.Edx &^ ignore.Edx}
		for _, register := range registers {
			if values[register] != 0 {
				fmt.Fprintf(&b, ";%x.%x.%s=%x", in.Eax, in.Ecx, [4]string{"eax", "ebx", "ecx", "edx"}[register], values[register])
			}
		}
		return out
	}
	const eax, ebx, ecx, edx = 0, 1, 2, 3
	// Leaves 7 and 0x24 count their further subleaves in subleaf 0's EAX.
	subleaves := func(leaf uint32) {
		last := record(In{Eax: leaf}, eax, ebx, ecx, edx).Eax
		for subleaf := uint32(1); subleaf <= last && subleaf < 32; subleaf++ {
			record(In{Eax: leaf, Ecx: subleaf}, eax, ebx, ecx, edx)
		}
	}
	// A leaf above the highest one returns another leaf's values on Intel.
	maxBasic := vendor.Eax
	if maxBasic >= uint32(featureInfo) {
		record(In{Eax: uint32(featureInfo)}, ecx, edx)
	}
	if maxBasic >= uint32(extendedFeatureInfo) {
		subleaves(uint32(extendedFeatureInfo))
	}
	if maxBasic >= uint32(xSaveInfo) {
		record(In{Eax: uint32(xSaveInfo), Ecx: 1}, eax)
	}
	// PTWRITE is in leaf 0x14, Key Locker in leaf 0x19, and AVX10 in leaf
	// 0x24.
	if maxBasic >= 0x14 {
		record(In{Eax: 0x14}, ebx)
	}
	if maxBasic >= 0x19 {
		record(In{Eax: 0x19}, ebx)
	}
	if maxBasic >= 0x24 {
		subleaves(0x24)
	}
	maxExtended := query(In{Eax: uint32(extendedFunctionInfo)}).Eax
	if maxExtended >= uint32(extendedFeatures) {
		record(In{Eax: uint32(extendedFeatures)}, ecx, edx)
	}
	if maxExtended >= uint32(addressSizes) {
		record(In{Eax: uint32(addressSizes)}, ebx)
	}
	if maxExtended >= uint32(extendedStart)+0x21 {
		record(In{Eax: uint32(extendedStart) + 0x21}, eax)
	}
	return b.String()
}

const (
	// XCR0AmxCfgMask is the bits that enable xsave to operate on
	// AMX TILECFG.
	//
	// See Intel® 64 and IA-32 Architectures Software Developer’s Manual Vol.1
	// section 13.3 for details.
	XCR0AmxCfgMask = uint64(1 << amxTileCfgComponent)

	// XCR0AmxDataMask is the bits that enable xsave to operate on
	// AMX TILEDATA.
	XCR0AmxDataMask = uint64(1 << amxTileDataComponent)
)

// ExtendedStateSize returns the number of bytes needed to save the "extended
// state" for the enabled features and the boundary it must be aligned to.
// Extended state includes floating point registers, and other cpu state that's
// not associated with the normal task context.
//
// Note: the return value matches the size of signal FP state frames.
// Look at check_xstate_in_sigframe() in the kernel sources for more details.
//
//go:nosplit
func (fs FeatureSet) ExtendedStateSize() (size, align uint) {
	if fs.UseXsave() {
		return uint(xsaveSize), 64
	}

	// If we don't support xsave, we fall back to fxsave, which requires
	// 512 bytes aligned to 16 bytes.
	return 512, 16
}

// AMXExtendedStateSize returns the number of bytes within the "extended state"
// area that is used for AMX.
func (fs FeatureSet) AMXExtendedStateSize() uint {
	total := uint(0)
	if fs.UseXsave() {
		xcr0 := xgetbv(0)
		// TILECFG is not part of AMX extended state, only TILEDATA.
		if (xcr0 & XCR0AmxDataMask) != 0 {
			total += uint(amxTileDataSize)
		}
	}
	return total
}

// ValidXCR0Mask returns the valid bits in control register XCR0.
//
// Always exclude AMX bits, because we do not support it.
// TODO(gvisor.dev/issues/9896): Implement AMX Support.
//
//go:nosplit
func (fs FeatureSet) ValidXCR0Mask() uint64 {
	if !fs.HasFeature(X86FeatureXSAVE) {
		return 0
	}
	ax, _, _, dx := fs.query(xSaveInfo)
	return (uint64(dx)<<32 | uint64(ax)) &^ (XCR0AmxCfgMask | XCR0AmxDataMask)
}

// UseXsave returns the choice of fp state saving instruction.
//
//go:nosplit
func (fs FeatureSet) UseXsave() bool {
	return fs.HasFeature(X86FeatureXSAVE) && fs.HasFeature(X86FeatureOSXSAVE)
}

// UseXsaveopt returns true if 'fs' supports the "xsaveopt" instruction.
//
//go:nosplit
func (fs FeatureSet) UseXsaveopt() bool {
	return fs.UseXsave() && fs.HasFeature(X86FeatureXSAVEOPT)
}

// UseXsavec returns true if 'fs' supports the "xsavec" instruction.
//
//go:nosplit
func (fs FeatureSet) UseXsavec() bool {
	return fs.UseXsaveopt() && fs.HasFeature(X86FeatureXSAVEC)
}

// UseFSGSBASE returns true if 'fs' supports the (RD|WR)(FS|GS)BASE instructions.
func (fs FeatureSet) UseFSGSBASE() bool {
	HWCAP2_FSGSBASE := uint64(1) << 1
	return fs.HasFeature(X86FeatureFSGSBase) && ((fs.hwCap.hwCap2 & HWCAP2_FSGSBASE) != 0)
}

// WithoutAVX512 returns fs without advertising AVX-512 instructions. This is
// useful for execution backends that intentionally preserve only the portable
// x87/SSE/AVX subset of userspace XSAVE state.
func (fs FeatureSet) WithoutAVX512() FeatureSet {
	static := fs.ToStatic()
	for _, feature := range []Feature{
		X86FeatureAVX512F,
		X86FeatureAVX512DQ,
		X86FeatureAVX512IFMA,
		X86FeatureAVX512PF,
		X86FeatureAVX512ER,
		X86FeatureAVX512CD,
		X86FeatureAVX512BW,
		X86FeatureAVX512VL,
		X86FeatureAVX512VBMI,
		X86FeatureAVX512_VBMI2,
		X86FeatureAVX512_VNNI,
		X86FeatureAVX512_BITALG,
		X86FeatureAVX512_VPOPCNTDQ,
		X86FeatureAVX512_4VNNIW,
		X86FeatureAVX512_4FMAPS,
		X86FeatureAVX512_VP2INTERSECT,
		X86FeatureAVX512_FP16,
	} {
		static.Remove(feature)
	}
	masked := static.ToFeatureSet()
	masked.hwCap = fs.hwCap
	return masked
}

// archCheckHostCompatible checks for compatibility.
func (fs FeatureSet) archCheckHostCompatible(hfs FeatureSet) error {
	// The size of a cache line must match, as it is critical to correctly
	// utilizing CLFLUSH. Other cache properties are allowed to change, as
	// they are not important to correctness.
	fsCache := fs.CacheLine()
	hostCache := hfs.CacheLine()
	if fsCache != hostCache {
		return &ErrIncompatible{
			reason: fmt.Sprintf("CPU cache line size %d incompatible with host cache line size %d", fsCache, hostCache),
		}
	}

	return nil
}

// AllowedHWCap1 returns the HWCAP1 bits that the guest is allowed to depend
// on.
func (fs FeatureSet) AllowedHWCap1() uint64 {
	// HWCAPS are not supported on amd64.
	return 0
}

// AllowedHWCap2 returns the HWCAP2 bits that the guest is allowed to depend
// on.
func (fs FeatureSet) AllowedHWCap2() uint64 {
	// HWCAPS are not supported on amd64.
	return 0
}

// UnsetFSGSBASE returns fs without advertising userspace FSGSBASE
// instructions through CPUID or AT_HWCAP2. This is used by systrap while its
// syscall-patching fast path temporarily owns GS; conforming applications then
// request GS changes through arch_prctl, where systrap can transition safely.
// If userspace used WRGSBASE without the Sentry's knowledge, it would corrupt
// the GS register used by systrap for syscall patching.
func (fs FeatureSet) UnsetFSGSBASE() FeatureSet {
	s := fs.ToStatic()
	X86FeatureFSGSBase.Unset(s)
	sfs := s.ToFeatureSet()
	sfs.hwCap = fs.hwCap
	sfs.hwCap.hwCap2 &^= HWCAP2_FSGSBASE
	return sfs
}
