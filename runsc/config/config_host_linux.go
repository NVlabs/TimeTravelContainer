// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package config

const (
	hostDefaultPlatform      = "systrap"
	hostDefaultIgnoreCgroups = false
	hostDefaultDirectFS      = true
	hostDefaultTTCNetworkNAT = false
)

var hostDefaultUnsafeNoHostIsolation = false

const hostDefaultNetwork = NetworkSandbox

func validateHostConfig(*Config) error { return nil }
