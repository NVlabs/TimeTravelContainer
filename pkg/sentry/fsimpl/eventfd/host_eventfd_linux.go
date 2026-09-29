// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package eventfd

import "golang.org/x/sys/unix"

func createHostEventFD(value uint64, flags int) (int, error) {
	fd, _, errno := unix.Syscall(unix.SYS_EVENTFD2, uintptr(value), uintptr(flags), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}
