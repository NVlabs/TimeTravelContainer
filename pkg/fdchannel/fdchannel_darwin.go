// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fdchannel

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func (ep *Endpoint) initIov() {
	ep.msghdr.Iov = (*unix.Iovec)(unsafe.Pointer(&ep.iov))
	ep.msghdr.Iovlen = 1
	ep.iov.Base = &ep.iovByte
	ep.iov.SetLen(1)
}

func sendFDFlags() uintptr { return unix.MSG_DONTWAIT }

func newConnectedSockets() ([2]int, error) {
	// Darwin does not implement AF_UNIX SOCK_SEQPACKET. SCM_RIGHTS on
	// SOCK_STREAM requires the one-byte iovec configured by initIov. Darwin
	// also has no SOCK_CLOEXEC, so hold ForkLock until both ends are
	// close-on-exec.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
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
