// Copyright 2020 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hostfd

import (
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/safemem"
)

// MaxReadWriteIov is the maximum permitted size of a struct iovec array in a
// readv, writev, preadv, or pwritev host syscall.
const MaxReadWriteIov = 1024 // UIO_MAXIOV

// MaxSendRecvMsgIov is the maximum permitted size of a struct iovec array in a
// sendmsg or recvmsg host syscall.
const MaxSendRecvMsgIov = 1024 // UIO_MAXIOV

// Preadv2 reads up to dsts.NumBytes() bytes from host file descriptor fd into
// dsts. offset and flags are interpreted as for preadv2(2).
func Preadv2(fd int32, dsts safemem.BlockSeq, offset int64, flags uint32) (uint64, error) {
	var (
		n uintptr
		e unix.Errno
	)
	if flags == 0 && dsts.NumBlocks() == 1 {
		dst := dsts.Head()
		if offset == -1 {
			n, _, e = unix.Syscall(unix.SYS_READ, uintptr(fd), dst.Addr(), uintptr(dst.Len()))
		} else {
			n, _, e = unix.Syscall6(unix.SYS_PREAD64, uintptr(fd), dst.Addr(), uintptr(dst.Len()), uintptr(offset), 0, 0)
		}
	} else {
		n, e = iovecsReadWrite(unix.SYS_PREADV2, fd, safemem.IovecsFromBlockSeq(dsts), offset, flags)
		if e != 0 && n > 0 {
			// Some bytes were read before the error. Report the partial
			// read, as Linux does; the error will resurface on the next
			// call.
			e = 0
		}
	}
	if e != 0 {
		return 0, e
	}
	if n == 0 {
		return 0, io.EOF
	}
	return uint64(n), nil
}

// Pwritev2 writes up to srcs.NumBytes() from srcs into host file descriptor
// fd. offset and flags are interpreted as for pwritev2(2).
func Pwritev2(fd int32, srcs safemem.BlockSeq, offset int64, flags uint32) (uint64, error) {
	var (
		n uintptr
		e unix.Errno
	)
	if flags == 0 && srcs.NumBlocks() == 1 {
		src := srcs.Head()
		if offset == -1 {
			n, _, e = unix.Syscall(unix.SYS_WRITE, uintptr(fd), src.Addr(), uintptr(src.Len()))
		} else {
			n, _, e = unix.Syscall6(unix.SYS_PWRITE64, uintptr(fd), src.Addr(), uintptr(src.Len()), uintptr(offset), 0, 0)
		}
	} else {
		n, e = iovecsReadWrite(unix.SYS_PWRITEV2, fd, safemem.IovecsFromBlockSeq(srcs), offset, flags)
		if e != 0 && n > 0 {
			// Some bytes were written before the error. Report the
			// partial write, as Linux does; the error will resurface on
			// the next call.
			e = 0
		}
	}
	if e != 0 {
		return 0, e
	}
	return uint64(n), nil
}
