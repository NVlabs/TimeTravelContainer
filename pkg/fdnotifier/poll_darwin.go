// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fdnotifier

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/waiter"
)

// NonBlockingPoll queries fd readiness without blocking. Like Linux's poll(2),
// it always reports hangups and errors, and reports EventRdHUp only if mask
// requests it.
func NonBlockingPoll(fd int32, mask waiter.EventMask) waiter.EventMask {
	read, write, err := pollDirections(fd)
	if err != nil {
		return mask
	}
	if (read|write)&unix.POLLNVAL != 0 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			// Darwin devices such as /dev/null don't support poll(2).
			// Linux reports files without poll support as always ready.
			return mask & (waiter.ReadableEvents | waiter.WritableEvents)
		}
		return waiter.EventErr
	}
	var ready waiter.EventMask
	if read&unix.POLLIN != 0 {
		ready |= waiter.ReadableEvents
	}
	if read&unix.POLLPRI != 0 {
		ready |= waiter.EventPri
	}
	if write&unix.POLLOUT != 0 {
		ready |= waiter.WritableEvents
	}
	if (read|write)&unix.POLLERR != 0 {
		ready |= waiter.EventErr
	}
	if readEOF, writeEOF := read&unix.POLLHUP != 0, write&unix.POLLHUP != 0; readEOF || writeEOF {
		ready |= eofEvents(classifyFD(fd), readEOF, writeEOF)
	}
	return ready & (mask | waiter.EventErr | waiter.EventHUp)
}
