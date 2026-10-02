// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"testing"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

func TestNVProxyNeedsSystrap(t *testing.T) {
	for _, tc := range []struct {
		platform, fork string
		refused        bool
	}{
		{"kvm", ttcbundle.GPUForkSnapshot, true},
		{"kvm", ttcbundle.GPUForkDeviceLost, true},
		{"kvm", "", true},
		{"systrap", ttcbundle.GPUForkSnapshot, false},
		{"systrap", ttcbundle.GPUForkDeviceLost, false},
	} {
		s := &nvproxyService{platform: tc.platform}
		// Families that are not refused go on to find the host's driver, which
		// the test's host may lack.
		_, _, err := s.edits(ttcbundle.GPUOptions{NVProxy: true, NVProxyFork: tc.fork})
		if refused := errors.Is(err, errNVProxyOnKVM); refused != tc.refused {
			t.Errorf("--nvproxy-fork=%s on %s: refused %t (%v), want %t", tc.fork, tc.platform, refused, err, tc.refused)
		}
	}
}
