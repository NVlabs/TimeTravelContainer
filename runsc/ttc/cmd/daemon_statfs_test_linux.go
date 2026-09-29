// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import "golang.org/x/sys/unix"

const testLinuxHost = true

func setTestNFSStatfs(statfs *unix.Statfs_t) {
	statfs.Type = unix.NFS_SUPER_MAGIC
}

func setTestLocalStatfs(statfs *unix.Statfs_t) {
	statfs.Type = unix.TMPFS_MAGIC
}

func setTestLustreStatfs(statfs *unix.Statfs_t) {
	statfs.Type = 0x0bd00bd0
}
