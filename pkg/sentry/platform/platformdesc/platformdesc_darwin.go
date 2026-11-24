// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platformdesc

func init() {
	// macOS has one platform, HVF (pkg/sentry/platform/vmplatform/hvf), as
	// pkg/sentry/platform/platforms registers there: it opens no device, and
	// needs no capability, but Hypervisor.framework's entitlement, which
	// ttcd's signature carries.
	descs = map[string]Desc{
		"hvf": {},
	}
}
