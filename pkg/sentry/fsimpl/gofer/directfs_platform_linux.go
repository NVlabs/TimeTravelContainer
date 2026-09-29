// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package gofer

import "golang.org/x/sys/unix"

func directfsSupported() bool { return true }

func directfsFallocate(fd int, mode uint32, offset, length int64) error {
	return unix.Fallocate(fd, mode, offset, length)
}

func goferDataSync(fd int) error { return unix.Fdatasync(fd) }

func dupToCloseOnExec(oldfd, newfd int) error {
	return unix.Dup3(oldfd, newfd, unix.O_CLOEXEC)
}
