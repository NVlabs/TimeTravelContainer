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

//go:build linux

package stateipc

import "golang.org/x/sys/unix"

func pollWatchdog(stopFD, socketFD int) (bool, bool, error) {
	events := []unix.PollFd{
		{Fd: int32(stopFD), Events: unix.POLLIN},
		{Fd: int32(socketFD), Events: unix.POLLHUP | unix.POLLRDHUP},
	}
	_, err := unix.Ppoll(events, nil, nil)
	return events[0].Revents != 0, events[1].Revents != 0, err
}

func adviseHugepage(mapping []byte, enabled bool) error {
	advice := unix.MADV_NOHUGEPAGE
	if enabled {
		advice = unix.MADV_HUGEPAGE
	}
	return unix.Madvise(mapping, advice)
}
