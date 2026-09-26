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
	flags := unix.MSG_DONTWAIT | unix.MSG_TRUNC
	if peek {
		flags |= unix.MSG_PEEK
	}
	length, iovecs, intermediate, buildErr := buildIovec(bufs, maxlen, true)
	if buildErr != nil && len(iovecs) == 0 {
		return 0, 0, 0, false, buildErr
	}
	n, oobn, recvFlags, _, recvErr := unix.RecvmsgBuffers(fd, buffersForIovecs(iovecs), control, flags)
	if recvErr != nil {
		return 0, 0, 0, false, recvErr
	}
	if intermediate != nil {
		copyToMulti(bufs, intermediate)
	}
	controlTrunc = recvFlags&unix.MSG_CTRUNC != 0
	if int64(n) > length {
		return length, int64(n), uint64(oobn), controlTrunc, nil
	}
	return int64(n), int64(n), uint64(oobn), controlTrunc, nil
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
	n, sendErr := unix.SendmsgBuffers(fd, buffersForIovecs(iovecs), nil, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
	if sendErr != nil {
		return 0, length, sendErr
	}
	return int64(n), length, buildErr
}

// buffersForIovecs converts the already validated and bounded iovecs back to
// byte slices for x/sys/unix's Darwin recvmsg and sendmsg wrappers. Unlike the
// Linux RawSyscall API, Darwin's RawSyscall takes a libSystem function pointer,
// not a numeric SYS_* value. The generated wrappers use the correct libc call
// boundary, which also makes an EL1-resident Sentry redpill before host work.
func buffersForIovecs(iovecs []unix.Iovec) [][]byte {
	bufs := make([][]byte, len(iovecs))
	for i := range iovecs {
		bufs[i] = unsafe.Slice(iovecs[i].Base, int(iovecs[i].Len))
	}
	return bufs
}
