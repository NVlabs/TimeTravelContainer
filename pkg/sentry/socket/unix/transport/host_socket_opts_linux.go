// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package transport

import "golang.org/x/sys/unix"

func hostSocketDomain(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN)
}

func hostSocketPasscred(fd int) (bool, error) {
	value, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED)
	return value != 0, err
}
