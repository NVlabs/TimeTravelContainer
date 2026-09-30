// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"strings"
	"testing"
)

func TestValidateGPUOptions(t *testing.T) {
	for _, test := range []struct {
		name     string
		opts     GPUOptions
		eligible bool
		goos     string
		wantErr  string
	}{
		{name: "no GPU", eligible: true, goos: "darwin"},
		{name: "no GPU, waived", eligible: false, goos: "darwin"},
		{name: "gfx needs a waiver", opts: GPUOptions{GFX: true}, eligible: true, goos: "darwin", wantErr: "--gfx-fork=device-lost or --gfx-fork=snapshot, or --waive-live-fork"},
		{name: "gfx waived", opts: GPUOptions{GFX: true}, eligible: false, goos: "darwin"},
		{name: "gfx device-lost", opts: GPUOptions{GFX: true, GFXFork: GPUForkDeviceLost}, eligible: true, goos: "darwin"},
		{name: "gfx device-lost and waived", opts: GPUOptions{GFX: true, GFXFork: GPUForkDeviceLost}, eligible: false, goos: "linux", wantErr: "drop --gfx-fork"},
		{name: "gfx snapshot", opts: GPUOptions{GFX: true, GFXFork: GPUForkSnapshot}, eligible: true, goos: "darwin"},
		{name: "gfx-fork without gfx", opts: GPUOptions{GFXFork: GPUForkDeviceLost}, eligible: true, goos: "linux", wantErr: "requires --gfx"},
		{name: "unknown gfx-fork", opts: GPUOptions{GFX: true, GFXFork: "reset"}, eligible: true, goos: "linux", wantErr: `--gfx-fork="reset"`},
		{name: "nvproxy on macOS", opts: GPUOptions{NVProxy: true}, eligible: false, goos: "darwin", wantErr: "macOS hosts do not have"},
		{name: "nvproxy on Linux", opts: GPUOptions{NVProxy: true, NVProxyFork: GPUForkDeviceLost}, eligible: true, goos: "linux", wantErr: "not supported yet"},
		{name: "nvproxy-fork without nvproxy", opts: GPUOptions{NVProxyFork: GPUForkSnapshot}, eligible: true, goos: "linux", wantErr: "requires --nvproxy"},
		{name: "nvproxy caps without nvproxy", opts: GPUOptions{NVProxyCaps: "compute"}, eligible: true, goos: "linux", wantErr: "require --nvproxy"},
	} {
		err := ValidateGPUOptions(test.opts, test.eligible, test.goos)
		switch {
		case test.wantErr == "" && err != nil:
			t.Errorf("%s: %v", test.name, err)
		case test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)):
			t.Errorf("%s: %v, want an error containing %q", test.name, err, test.wantErr)
		}
	}
}
