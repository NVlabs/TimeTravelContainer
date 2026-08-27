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

// NonBlockingPoll queries fd readiness without blocking.
func NonBlockingPoll(fd int32, mask waiter.EventMask) waiter.EventMask {
	var requested int16
	if mask&waiter.ReadableEvents != 0 {
		requested |= unix.POLLIN
	}
	if mask&waiter.WritableEvents != 0 {
		requested |= unix.POLLOUT
	}
	if mask&waiter.EventPri != 0 {
		requested |= unix.POLLPRI
	}
	events := []unix.PollFd{{Fd: fd, Events: requested}}
	for {
		count, err := unix.Poll(events, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return mask
		}
		if count == 0 {
			return 0
		}
		var ready waiter.EventMask
		revents := events[0].Revents
		if revents&unix.POLLIN != 0 {
			ready |= waiter.ReadableEvents
		}
		if revents&unix.POLLOUT != 0 {
			ready |= waiter.WritableEvents
		}
		if revents&unix.POLLPRI != 0 {
			ready |= waiter.EventPri
		}
		if revents&unix.POLLHUP != 0 {
			ready |= waiter.EventHUp | waiter.EventRdHUp
		}
		if revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			ready |= waiter.EventErr
		}
		return ready & (mask | waiter.EventErr | waiter.EventHUp | waiter.EventRdHUp)
	}
}
