// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package unet

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func hostSocket(packet bool) (int, error) {
	// Darwin has no SOCK_CLOEXEC. Hold ForkLock until the socket is
	// close-on-exec so a concurrent fork cannot inherit it.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Socket(unix.AF_UNIX, socketType(packet), 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	return fd, err
}

func hostSocketPair(packet bool) ([2]int, error) {
	// Darwin has no SOCK_CLOEXEC. Hold ForkLock until both ends are
	// close-on-exec so a concurrent fork cannot inherit them.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, socketType(packet), 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	return fds, err
}

// closeOnExecAdopted makes a socket that NewSocket adopts close-on-exec, as
// Darwin creates none so.
func closeOnExecAdopted(fd int) {
	unix.CloseOnExec(fd)
}
