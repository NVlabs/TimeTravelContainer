// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package contexttest

import (
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/ptrace"
)

// newPlatform returns the platform of test contexts.
func newPlatform() (platform.Platform, error) {
	return ptrace.New()
}
