// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package testutil

import (
	// Platforms are pluggable.
	_ "gvisor.dev/gvisor/pkg/sentry/platform/kvm"
	_ "gvisor.dev/gvisor/pkg/sentry/platform/ptrace"
)

// defaultPlatform is the platform that Boot uses by default.
const defaultPlatform = "ptrace"
