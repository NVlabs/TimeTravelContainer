// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cmd

import (
	"testing"

	"golang.org/x/sys/unix"
)

const testLinuxHost = false

func setTestNFSStatfs(statfs *unix.Statfs_t) {
	setTestFilesystemName(statfs, "nfs")
	statfs.Flags &^= unix.MNT_LOCAL
}

func setTestLocalStatfs(statfs *unix.Statfs_t) {
	setTestFilesystemName(statfs, "apfs")
	statfs.Flags |= unix.MNT_LOCAL
}

// setTestLustreStatfs is as macOS would describe a Lustre filesystem, which
// it has no client for, if it had: a filesystem not marked local.
func setTestLustreStatfs(statfs *unix.Statfs_t) {
	setTestFilesystemName(statfs, "lustre")
	statfs.Flags &^= unix.MNT_LOCAL
}

func setTestFilesystemName(statfs *unix.Statfs_t, name string) {
	for i := range statfs.Fstypename {
		statfs.Fstypename[i] = 0
	}
	for i := 0; i < len(name) && i < len(statfs.Fstypename); i++ {
		statfs.Fstypename[i] = name[i]
	}
}

func TestCurrentNodeMemoryBytesDarwin(t *testing.T) {
	resident, err := currentNodeMemoryBytes("")
	if err != nil {
		t.Fatalf("currentNodeMemoryBytes: %v", err)
	}
	if resident == 0 {
		t.Fatal("currentNodeMemoryBytes returned zero")
	}
}
