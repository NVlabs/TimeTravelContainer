// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package transport

import "golang.org/x/sys/unix"

func hostSocketDomain(fd int) (int, error) {
	address, err := unix.Getsockname(fd)
	if err != nil {
		return 0, err
	}
	if _, ok := address.(*unix.SockaddrUnix); !ok {
		return 0, unix.EAFNOSUPPORT
	}
	return unix.AF_UNIX, nil
}

func hostSocketPasscred(_ int) (bool, error) {
	// Darwin has LOCAL_PEERCRED for querying a connected peer, but no Linux
	// SO_PASSCRED/SCM_CREDENTIALS per-message facility.
	return false, nil
}
