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

package transport

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func fdReadVec(fd int, bufs [][]byte, control []byte, peek bool, maxlen int64) (readLen int64, msgLen int64, controlLen uint64, controlTrunc bool, err error) {
	flags := uintptr(unix.MSG_DONTWAIT | unix.MSG_TRUNC)
	if peek {
		flags |= unix.MSG_PEEK
	}
	length, iovecs, intermediate, buildErr := buildIovec(bufs, maxlen, true)
	if buildErr != nil && len(iovecs) == 0 {
		return 0, 0, 0, false, buildErr
	}
	var msg unix.Msghdr
	if len(control) != 0 {
		msg.Control = &control[0]
		msg.SetControllen(len(control))
	}
	if len(iovecs) != 0 {
		msg.Iov = &iovecs[0]
		msg.SetIovlen(len(iovecs))
	}
	rawN, _, errno := unix.RawSyscall(unix.SYS_RECVMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), flags)
	if errno != 0 {
		return 0, 0, 0, false, errno
	}
	n := int64(rawN)
	if intermediate != nil {
		copyToMulti(bufs, intermediate)
	}
	controlTrunc = msg.Flags&unix.MSG_CTRUNC != 0
	if n > length {
		return length, n, uint64(msg.Controllen), controlTrunc, nil
	}
	return n, n, uint64(msg.Controllen), controlTrunc, nil
}

func fdWriteVec(fd int, bufs [][]byte, maxlen int64, truncate bool, creds bool) (int64, int64, error) {
	length, iovecs, intermediate, buildErr := buildIovec(bufs, maxlen, truncate)
	if buildErr != nil && len(iovecs) == 0 {
		return 0, length, buildErr
	}
	if creds {
		return 0, length, unix.EOPNOTSUPP
	}
	if intermediate != nil {
		copyFromMulti(intermediate, bufs)
	}
	var msg unix.Msghdr
	if len(iovecs) != 0 {
		msg.Iov = &iovecs[0]
		msg.SetIovlen(len(iovecs))
	}
	n, _, errno := unix.RawSyscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
	if errno != 0 {
		return 0, length, errno
	}
	return int64(n), length, buildErr
}
