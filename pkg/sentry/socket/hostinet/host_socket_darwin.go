// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostinet

import "golang.org/x/sys/unix"

// hostinet passes Linux sockaddr layouts directly to host syscalls. Darwin
// uses different sockaddr layouts and family numbers, and the macOS product
// deliberately supports netstack rather than hostinet.
func newHostSocket(family, stype, protocol int) (int, error) {
	return -1, unix.EOPNOTSUPP
}

func hostAccept4(fd int, addr *byte, addrlen *uint32, flags int) (int, error) {
	return -1, unix.EOPNOTSUPP
}
