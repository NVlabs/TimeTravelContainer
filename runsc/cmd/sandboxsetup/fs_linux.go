// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandboxsetup

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
)

func pivotRoot(root string) error {
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("error changing working directory: %v", err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root failed, make sure that the root mount has a parent: %v", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("error umounting the old root file system: %v", err)
	}
	return nil
}

// AdjustMountOptions adds Linux-filesystem-specific gofer mount options.
func AdjustMountOptions(_ *config.Config, path string, opts []string) ([]string, error) {
	rv := append([]string(nil), opts...)
	var statfs unix.Statfs_t
	if err := unix.Statfs(path, &statfs); err != nil {
		return nil, err
	}
	switch statfs.Type {
	case unix.OVERLAYFS_SUPER_MAGIC:
		rv = append(rv, "overlayfs_stale_read")
	case unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		rv = append(rv, "disable_file_handle_sharing")
	}
	return rv, nil
}
