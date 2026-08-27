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

package stateipc

import "golang.org/x/sys/unix"

func pollWatchdog(stopFD, socketFD int) (bool, bool, error) {
	events := []unix.PollFd{
		{Fd: int32(stopFD), Events: unix.POLLIN},
		// Darwin may report a peer shutdown as readable EOF without POLLHUP.
		{Fd: int32(socketFD), Events: unix.POLLIN | unix.POLLHUP},
	}
	_, err := unix.Poll(events, -1)
	if err != nil {
		return false, false, err
	}
	stopped := events[0].Revents != 0
	socketEvents := events[1].Revents
	disconnected := socketEvents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0
	if !disconnected && socketEvents&unix.POLLIN != 0 {
		var byte [1]byte
		n, _, recvErr := unix.Recvfrom(socketFD, byte[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case recvErr == nil:
			disconnected = n == 0
		case recvErr == unix.EAGAIN || recvErr == unix.EWOULDBLOCK:
		default:
			return stopped, false, recvErr
		}
	}
	return stopped, disconnected, nil
}

// Darwin does not expose Linux transparent-hugepage advice. Shared mappings
// retain the platform's normal VM policy.
func adviseHugepage(_ []byte, _ bool) error {
	return nil
}
