// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package boot

import "golang.org/x/sys/unix"

func remapFD(oldfd, newfd int) error {
	if err := unix.Dup2(oldfd, newfd); err != nil {
		return err
	}
	unix.CloseOnExec(newfd)
	return nil
}

func waitForGoferDisconnect(goferFD int) error {
	events := []unix.PollFd{{Fd: int32(goferFD), Events: unix.POLLHUP}}
	for {
		if _, err := unix.Poll(events, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		return nil
	}
}

// isTmpfs reports that dir is on no tmpfs, which the host has none of.
func isTmpfs(dir string) (bool, error) {
	return false, nil
}

// openUnnamedFile fails: the host's files have names.
func openUnnamedFile(dir string) (int, error) {
	return -1, unix.ENOTSUP
}
