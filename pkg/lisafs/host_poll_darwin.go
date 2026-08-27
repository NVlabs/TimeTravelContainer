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

package lisafs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func waitForHangup(fd int) error {
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
	for {
		events[0].Revents = 0
		n, err := unix.Poll(events, -1)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("poll returned %d events, want 1", n)
		}
		if events[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return nil
		}
		if events[0].Revents&unix.POLLIN != 0 {
			var byte [1]byte
			n, _, recvErr := unix.Recvfrom(fd, byte[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
			if recvErr == nil && n == 0 {
				return nil
			}
			if recvErr != nil && recvErr != unix.EAGAIN && recvErr != unix.EWOULDBLOCK {
				return recvErr
			}
		}
	}
}
