// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64
// +build darwin,arm64

package cpuid

import "syscall"

// darwinHWCap translates Apple's public ARM feature sysctls to the Linux
// HWCAP namespace used by the Sentry ABI. Only features whose guest register
// state is already preserved by the platform are advertised.
func darwinHWCap(lookup func(string) (uint32, error)) hwCap {
	has := func(names ...string) bool {
		for _, name := range names {
			if value, err := lookup(name); err == nil && value != 0 {
				return true
			}
		}
		return false
	}

	var bits uint64
	set := func(bit int, names ...string) {
		if has(names...) {
			bits |= uint64(bit)
		}
	}

	set(HWCAP_FP, "hw.optional.floatingpoint")
	set(HWCAP_ASIMD, "hw.optional.arm.AdvSIMD", "hw.optional.neon")
	set(HWCAP_AES, "hw.optional.arm.FEAT_AES")
	set(HWCAP_PMULL, "hw.optional.arm.FEAT_PMULL")
	set(HWCAP_SHA1, "hw.optional.arm.FEAT_SHA1")
	set(HWCAP_SHA2, "hw.optional.arm.FEAT_SHA256")
	set(HWCAP_CRC32, "hw.optional.arm.FEAT_CRC32", "hw.optional.armv8_crc32")
	set(HWCAP_ATOMICS, "hw.optional.arm.FEAT_LSE", "hw.optional.armv8_1_atomics")
	// Apple's FEAT_FP16 key denotes scalar and Advanced SIMD half-precision
	// arithmetic. The older keys are retained as fallbacks for macOS 15.
	set(HWCAP_FPHP, "hw.optional.arm.FEAT_FP16", "hw.optional.neon_fp16")
	set(HWCAP_ASIMDHP, "hw.optional.arm.FEAT_FP16", "hw.optional.arm.AdvSIMD_HPFPCvt", "hw.optional.neon_fp16")
	set(HWCAP_ASIMDRDM, "hw.optional.arm.FEAT_RDM")
	set(HWCAP_JSCVT, "hw.optional.arm.FEAT_JSCVT")
	set(HWCAP_FCMA, "hw.optional.arm.FEAT_FCMA")
	set(HWCAP_LRCPC, "hw.optional.arm.FEAT_LRCPC")
	set(HWCAP_DCPOP, "hw.optional.arm.FEAT_DPB")
	set(HWCAP_SHA3, "hw.optional.arm.FEAT_SHA3")
	set(HWCAP_ASIMDDP, "hw.optional.arm.FEAT_DotProd")
	set(HWCAP_SHA512, "hw.optional.arm.FEAT_SHA512")
	set(HWCAP_ASIMDFHM, "hw.optional.arm.FEAT_FHM")
	return hwCap{hwCap1: bits}
}

func initHWCap() {
	hostFeatureSet.hwCap = darwinHWCap(syscall.SysctlUint32)
}
