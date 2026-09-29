// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cluster

import (
	"os"

	"golang.org/x/sys/unix"
)

// openShared opens path with direct I/O, or without it where the filesystem
// does not allow it.
func openShared(path string, flags int) (int, error) {
	fd, err := unix.Open(path, flags|unix.O_DIRECT|unix.O_CLOEXEC, 0600)
	if err == unix.EINVAL {
		fd, err = unix.Open(path, flags|unix.O_CLOEXEC, 0600)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return fd, nil
}
