// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package sandboxsetup

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/timing"
)

// ResolvedThreadCaps are the capabilities to apply to every thread: Darwin
// has no Linux capabilities, so there are none.
type ResolvedThreadCaps struct{}

// ResolveThreadCaps resolves no capabilities.
func ResolveThreadCaps(*specs.LinuxCapabilities, *timing.Timer) (*ResolvedThreadCaps, error) {
	return &ResolvedThreadCaps{}, nil
}

// Apply applies no capabilities.
func (*ResolvedThreadCaps) Apply(*timing.Timer) error { return nil }

func ApplyCapsAllThreads(*specs.LinuxCapabilities, *timing.Timer) error { return nil }
