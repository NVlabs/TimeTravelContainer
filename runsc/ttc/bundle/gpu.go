// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"errors"
	"fmt"
)

// GPU fork semantics, as --gfx-fork and --nvproxy-fork spell them. A GPU
// device without them refuses forks while it is in use, so its family must
// waive live forks.
const (
	// GPUForkDeviceLost forks, and the child's GPU state is lost, as it is
	// when a GPU is reset.
	GPUForkDeviceLost = "device-lost"
	// GPUForkSnapshot forks the GPU's state.
	GPUForkSnapshot = "snapshot"
)

// GPUOptions are the GPU devices of a family.
type GPUOptions struct {
	// GFX is whether the family has the virtual GPU (--gfx), and GFXFork is
	// what a fork does with it (--gfx-fork).
	GFX     bool   `json:"gfx,omitempty"`
	GFXFork string `json:"gfx_fork,omitempty"`

	// NVProxy is whether the family has the host's NVIDIA GPUs (--nvproxy),
	// NVProxyCaps the driver capabilities it may use, and NVProxyFork what a
	// fork does with them (--nvproxy-fork).
	NVProxy     bool   `json:"nvproxy,omitempty"`
	NVProxyCaps string `json:"nvproxy_caps,omitempty"`
	NVProxyFork string `json:"nvproxy_fork,omitempty"`
}

// errGFXNeedsForkSemantics and errNVProxyNeedsForkSemantics explain the rule
// that a family's GPU devices and its live-fork eligibility must agree.
var (
	errGFXNeedsForkSemantics     = errors.New("the virtual GPU refuses live forks unless --gfx-fork says what a fork does with it; pass --gfx-fork=device-lost or --gfx-fork=snapshot, or --waive-live-fork")
	errNVProxyNeedsForkSemantics = errors.New("NVIDIA GPUs refuse live forks unless --nvproxy-fork says what a fork does with them; pass --nvproxy-fork=device-lost, or --waive-live-fork")
)

// ValidateGPUOptions returns an error if a new family's GPU options are
// invalid, unsupported on a host of the given OS, or disagree with its
// live-fork eligibility.
func ValidateGPUOptions(opts GPUOptions, liveForkEligible bool, goos string) error {
	if err := checkGPURecord(opts, liveForkEligible); err != nil {
		return err
	}
	if opts.NVProxy {
		if goos == "darwin" {
			return errors.New("--nvproxy uses the host's NVIDIA GPUs, which macOS hosts do not have")
		}
		return errors.New("--nvproxy is not supported yet")
	}
	return nil
}

// checkGPURecord returns an error if a family's GPU options are invalid, or
// disagree with its live-fork eligibility.
func checkGPURecord(opts GPUOptions, liveForkEligible bool) error {
	if err := validateGPUFork("--gfx-fork", opts.GFXFork); err != nil {
		return err
	}
	if err := validateGPUFork("--nvproxy-fork", opts.NVProxyFork); err != nil {
		return err
	}
	if opts.GFXFork != "" && !opts.GFX {
		return errors.New("--gfx-fork requires --gfx")
	}
	if opts.NVProxyFork != "" && !opts.NVProxy {
		return errors.New("--nvproxy-fork requires --nvproxy")
	}
	if opts.NVProxyCaps != "" && !opts.NVProxy {
		return errors.New("NVIDIA driver capabilities require --nvproxy")
	}
	forkSemantics := opts.GFXFork != "" || opts.NVProxyFork != ""
	switch {
	case liveForkEligible && opts.GFX && opts.GFXFork == "":
		return errGFXNeedsForkSemantics
	case liveForkEligible && opts.NVProxy && opts.NVProxyFork == "":
		return errNVProxyNeedsForkSemantics
	case !liveForkEligible && forkSemantics:
		return errors.New("GPU fork semantics make a GPU fork-eligible, which a family that waives live forks does not need; drop --gfx-fork and --nvproxy-fork, or --waive-live-fork")
	}
	return nil
}

func validateGPUFork(flag, value string) error {
	switch value {
	case "", GPUForkDeviceLost, GPUForkSnapshot:
		return nil
	default:
		return fmt.Errorf("%s=%q: want %s or %s", flag, value, GPUForkDeviceLost, GPUForkSnapshot)
	}
}
