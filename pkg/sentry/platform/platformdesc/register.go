// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platformdesc

// Register describes a platform that a program registers itself, outside
// pkg/sentry/platform/platforms, as the Sentry's tests do theirs
// (pkg/sentry/platform/platformtest). It is called from init functions.
func Register(name string, d Desc) {
	descs[name] = d
}
