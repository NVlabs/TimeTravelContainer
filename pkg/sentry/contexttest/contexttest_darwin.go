// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package contexttest

import (
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/platformtest"
)

// newPlatform returns the platform of test contexts. A test binary without the
// hypervisor entitlement can't create a Hypervisor.framework VM.
func newPlatform() (platform.Platform, error) {
	return platformtest.New(), nil
}
