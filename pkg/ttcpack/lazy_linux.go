// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ttcpack

import "golang.org/x/sys/unix"

// startWriteback starts writing the data of the file fd back, so that the
// sync of a fetch's first file commits every file's: see populate.
func startWriteback(fd int) {
	_ = unix.SyncFileRange(fd, 0, 0, unix.SYNC_FILE_RANGE_WRITE)
}
