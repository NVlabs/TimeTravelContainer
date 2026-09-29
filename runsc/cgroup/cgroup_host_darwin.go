// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cgroup

import "golang.org/x/sys/unix"

// Darwin has no host cgroup filesystem. Trusted-mode lifecycle validation
// rejects cgroup configuration before any host cgroup operation is attempted.
func isCgroup2FS(_ *unix.Statfs_t) bool {
	return false
}
