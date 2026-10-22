// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package eventfd

import (
	"fmt"
	"io"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// Create returns a local event notification pipe. The Darwin adapter keeps
// the read and write descriptors together; protocols that transfer Eventfd.FD
// alone must use a Darwin-specific two-FD transport instead.
//
// The value lives in a counter rather than in the pipe, which holds one byte
// while the counter is non-zero. Like an eventfd, the read descriptor is thus
// readable exactly while the value is non-zero, writes accumulate, and Read
// returns and resets their sum; the pipe can never fill.
func Create() (Eventfd, error) {
	var fds [2]int
	// Darwin has no pipe2: hold off concurrent forks until the descriptors
	// are close-on-exec, as os.Pipe does, so that no child inherits them.
	syscall.ForkLock.RLock()
	err := unix.Pipe(fds[:])
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return Eventfd{}, fmt.Errorf("creating event pipe: %w", err)
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return Eventfd{}, err
	}
	return Eventfd{fd: fds[0], writeFD: fds[1], counter: new(atomicbitops.Uint64)}, nil
}

// Read blocks until the value is non-zero, and returns and resets it.
func (ev Eventfd) Read() (uint64, error) {
	if ev.counter == nil {
		return ev.readValue()
	}
	var b [1]byte
	for {
		n, err := unix.Read(ev.fd, b[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		// The byte was written when the counter became non-zero, and only
		// this Swap makes it zero again.
		return ev.counter.Swap(0), nil
	}
}

// readValue blocks until a complete value written by Write is available in
// a wrapped FD, which has no counter.
func (ev Eventfd) readValue() (uint64, error) {
	var tmp [sizeofUint64]byte
	read := 0
	for read != len(tmp) {
		n, err := unix.Read(ev.fd, tmp[read:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		read += n
	}
	return hostarch.ByteOrder.Uint64(tmp[:]), nil
}
