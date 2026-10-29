// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package config

import "fmt"

const (
	hostDefaultPlatform      = "hvf"
	hostDefaultIgnoreCgroups = true
	hostDefaultDirectFS      = false
	hostDefaultTTCNetworkNAT = true
)

var hostDefaultUnsafeNoHostIsolation = false

const hostDefaultNetwork = NetworkTimeTravel

func validateHostConfig(c *Config) error {
	if !c.UnsafeNoHostIsolation {
		return fmt.Errorf("native macOS execution requires --unsafe-no-host-isolation")
	}
	// macOS has no seccomp equivalent. UnsafeNoHostIsolation is an explicit
	// acknowledgement that the Sentry and gofer run without a host syscall
	// filter, so carry that decision into the boot process.
	c.DisableSeccomp = true
	if c.Platform != "hvf" {
		return fmt.Errorf("native macOS execution requires --platform=hvf, got %q", c.Platform)
	}
	if c.DirectFS {
		return fmt.Errorf("native macOS does not support --directfs")
	}
	if c.Network != NetworkTimeTravel && c.Network != NetworkNone {
		return fmt.Errorf("native macOS supports only --network=ttc or --network=none, got %q", c.Network)
	}
	if c.NVProxy || c.NVProxyDocker || c.TPUProxy || c.RDMAProxy {
		return fmt.Errorf("native macOS does not support host device proxies")
	}
	if c.IOUring {
		return fmt.Errorf("native macOS does not support host io_uring")
	}
	if c.EnableCoreTags {
		return fmt.Errorf("native macOS does not support Linux core tags")
	}
	if c.Rootless {
		return fmt.Errorf("--rootless is a Linux host-isolation mode; use --unsafe-no-host-isolation on macOS")
	}
	if c.SystemdCgroup || c.InSandboxCgroup == InSandboxCgroupV2 {
		return fmt.Errorf("native macOS does not support host cgroup configuration")
	}
	return nil
}
