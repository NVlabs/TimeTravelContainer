// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package unet

import "golang.org/x/sys/unix"

func hostSocket(packet bool) (int, error) {
	return unix.Socket(unix.AF_UNIX, socketType(packet)|unix.SOCK_CLOEXEC, 0)
}

func hostSocketPair(packet bool) ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, socketType(packet)|unix.SOCK_CLOEXEC, 0)
}

// closeOnExecAdopted does nothing: a Linux socket that must not survive an
// exec is made close-on-exec as it is created, and the seccomp filters of the
// processes that adopt sockets, a gofer's among them, refuse fcntl(F_SETFD).
func closeOnExecAdopted(int) {}
