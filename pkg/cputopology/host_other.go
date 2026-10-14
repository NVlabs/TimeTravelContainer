// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package cputopology

import "errors"

// HostTopology reads the host's topology: on Linux only.
func HostTopology() (Topology, error) {
	return Topology{}, errors.New("the host CPU topology is read on Linux only")
}

// AllowedNodeCount returns 1: the host topology is read on Linux only.
func AllowedNodeCount() int {
	return 1
}
