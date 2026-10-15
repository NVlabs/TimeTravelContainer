// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package boot

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/specutils"
)

func remapFD(oldfd, newfd int) error { return unix.Dup3(oldfd, newfd, unix.O_CLOEXEC) }

func waitForGoferDisconnect(goferFD int) error {
	events := []unix.PollFd{{Fd: int32(goferFD), Events: unix.POLLHUP | unix.POLLRDHUP}}
	_, _, err := specutils.RetryEintr(func() (uintptr, uintptr, error) {
		n, err := unix.Ppoll(events, nil, nil)
		return uintptr(n), 0, err
	})
	return err
}

// isTmpfs reports whether dir is on a tmpfs.
func isTmpfs(dir string) (bool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return false, err
	}
	return fs.Type == unix.TMPFS_MAGIC, nil
}

// openUnnamedFile opens a new file of no name in dir, for reading and
// writing.
func openUnnamedFile(dir string) (int, error) {
	return unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
}
