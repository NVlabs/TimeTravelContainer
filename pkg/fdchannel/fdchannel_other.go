// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build aix || dragonfly || freebsd || netbsd || openbsd || solaris

package fdchannel

import "golang.org/x/sys/unix"

func (*Endpoint) initIov() {}

func sendFDFlags() uintptr { return 0 }

func newConnectedSockets() ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return fds, err
	}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return [2]int{}, err
		}
	}
	return fds, nil
}
