// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cluster

import (
	"os"

	"golang.org/x/sys/unix"
)

// openShared opens path uncached, with F_NOCACHE, Darwin's direct I/O, or
// cached where the filesystem does not allow it.
func openShared(path string, flags int) (int, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC, 0600)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_NOCACHE, 1)
	return fd, nil
}
