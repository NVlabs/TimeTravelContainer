// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package fdchannel

import (
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
	// SOCK_STREAM requires the one-byte iovec configured by initIov.
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
