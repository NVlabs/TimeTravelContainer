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
