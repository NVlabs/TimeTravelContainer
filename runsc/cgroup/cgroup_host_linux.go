// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cgroup

import "golang.org/x/sys/unix"

func isCgroup2FS(stat *unix.Statfs_t) bool {
	return stat.Type == unix.CGROUP2_SUPER_MAGIC
}
