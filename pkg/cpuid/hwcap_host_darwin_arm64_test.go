// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64
// +build darwin,arm64

package cpuid

import (
	"fmt"
	"testing"
)

func TestDarwinHWCap(t *testing.T) {
	values := map[string]uint32{
		"hw.optional.floatingpoint":    1,
		"hw.optional.arm.AdvSIMD":      1,
		"hw.optional.arm.FEAT_AES":     1,
		"hw.optional.arm.FEAT_LSE":     1,
		"hw.optional.arm.FEAT_FP16":    1,
		"hw.optional.arm.FEAT_DotProd": 1,
		"hw.optional.arm.FEAT_SHA512":  1,
		"hw.optional.arm.FEAT_SVE":     1,
		"hw.optional.arm.FEAT_PAuth":   1,
		"hw.optional.arm.FEAT_LRCPC":   0,
	}
	lookup := func(name string) (uint32, error) {
		value, ok := values[name]
		if !ok {
			return 0, fmt.Errorf("unknown sysctl %q", name)
		}
		return value, nil
	}

	got := darwinHWCap(lookup).hwCap1
	want := uint64(HWCAP_FP | HWCAP_ASIMD | HWCAP_AES | HWCAP_ATOMICS |
		HWCAP_FPHP | HWCAP_ASIMDHP | HWCAP_ASIMDDP | HWCAP_SHA512)
	if got != want {
		t.Fatalf("darwinHWCap() = %#x, want %#x", got, want)
	}
	if got&HWCAP_SVE != 0 {
		t.Fatalf("darwinHWCap() advertised unsupported SVE state: %#x", got)
	}
}

func TestDarwinHWCapFallbacks(t *testing.T) {
	values := map[string]uint32{
		"hw.optional.neon":            1,
		"hw.optional.armv8_crc32":     1,
		"hw.optional.armv8_1_atomics": 1,
		"hw.optional.neon_fp16":       1,
	}
	lookup := func(name string) (uint32, error) {
		value, ok := values[name]
		if !ok {
			return 0, fmt.Errorf("unknown sysctl %q", name)
		}
		return value, nil
	}

	got := darwinHWCap(lookup).hwCap1
	want := uint64(HWCAP_ASIMD | HWCAP_CRC32 | HWCAP_ATOMICS | HWCAP_FPHP | HWCAP_ASIMDHP)
	if got != want {
		t.Fatalf("darwinHWCap() = %#x, want %#x", got, want)
	}
}
