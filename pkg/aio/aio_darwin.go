// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package aio

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func executeRequest(r Request) Completion {
	var sysno uintptr
	switch r.Op {
	case OpRead:
		sysno = unix.SYS_PREAD
	case OpWrite:
		sysno = unix.SYS_PWRITE
	case OpReadv, OpWritev:
		return executeVectoredRequest(r)
	default:
		return Completion{ID: r.ID, Result: -int64(unix.EINVAL)}
	}
	n, _, errno := unix.Syscall6(sysno, uintptr(r.FD), uintptr(r.Buf), uintptr(r.Len), uintptr(r.Off), 0, 0)
	result := Completion{ID: r.ID, Result: int64(n)}
	if errno != 0 {
		result.Result = -int64(errno)
	}
	return result
}

// Darwin provides preadv(2)/pwritev(2) through libc but does not expose
// stable raw syscall numbers for them. GoQueue workers therefore preserve the
// positioned-vector semantics by issuing one positioned operation per iovec.
// Returning a positive partial result lets the queue's caller continue the
// operation exactly as it would after a short vectored syscall.
func executeVectoredRequest(r Request) Completion {
	iovecs := unsafe.Slice((*unix.Iovec)(r.Buf), r.Len)
	offset := r.Off
	var total int64
	for _, iovec := range iovecs {
		length := int(iovec.Len)
		if uint64(length) != iovec.Len {
			return Completion{ID: r.ID, Result: -int64(unix.EINVAL)}
		}
		if length == 0 {
			continue
		}
		buffer := unsafe.Slice(iovec.Base, length)
		var (
			n   int
			err error
		)
		if r.Op == OpReadv {
			n, err = unix.Pread(int(r.FD), buffer, offset)
		} else {
			n, err = unix.Pwrite(int(r.FD), buffer, offset)
		}
		// Failed calls return n == -1, which must not count as progress.
		if n > 0 {
			total += int64(n)
			offset += int64(n)
		}
		if err != nil {
			if total != 0 {
				return Completion{ID: r.ID, Result: total}
			}
			if errno, ok := err.(unix.Errno); ok {
				return Completion{ID: r.ID, Result: -int64(errno)}
			}
			return Completion{ID: r.ID, Result: -int64(unix.EIO)}
		}
		if n != length {
			return Completion{ID: r.ID, Result: total}
		}
	}
	return Completion{ID: r.ID, Result: total}
}
