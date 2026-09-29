// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package stateipc

import "golang.org/x/sys/unix"

func pollWatchdog(stopFD, socketFD int) (bool, bool, error) {
	events := []unix.PollFd{
		{Fd: int32(stopFD), Events: unix.POLLIN},
		{Fd: int32(socketFD), Events: unix.POLLHUP | unix.POLLRDHUP},
	}
	_, err := unix.Ppoll(events, nil, nil)
	return events[0].Revents != 0, events[1].Revents != 0, err
}

func adviseHugepage(mapping []byte, enabled bool) error {
	advice := unix.MADV_NOHUGEPAGE
	if enabled {
		advice = unix.MADV_HUGEPAGE
	}
	return unix.Madvise(mapping, advice)
}
