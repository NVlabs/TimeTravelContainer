// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package testutil

import (
	"gvisor.dev/gvisor/pkg/sentry/platform/platformtest"
)

// defaultPlatform is the platform that Boot uses by default. A test binary
// without the hypervisor entitlement can't create a Hypervisor.framework VM,
// so the default can't run application code.
const defaultPlatform = platformtest.Name
