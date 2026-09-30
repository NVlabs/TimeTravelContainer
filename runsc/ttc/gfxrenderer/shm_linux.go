// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

import (
	"golang.org/x/sys/unix"
)

// createSharedMemory returns anonymous shared memory of size bytes.
func createSharedMemory(size uint64) (int, error) {
	fd, err := unix.MemfdCreate("ttc-gfx", unix.MFD_CLOEXEC)
	if err != nil {
		return -1, err
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
