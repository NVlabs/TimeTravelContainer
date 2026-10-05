// Copyright 2018 The gVisor Authors.
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

package lisafs

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/fdnotifier"
	"gvisor.dev/gvisor/pkg/waiter"
)

// hangupEvents are the events of a peer hanging up.
const hangupEvents = waiter.EventHUp | waiter.EventRdHUp | waiter.EventErr

// waitForHangup waits until fd's peer hangs up, in the epoll of fdnotifier,
// in which every other descriptor the process watches waits too. Waiting in
// a poll of its own, a client, of each mount of each branch, held a thread
// for as long as its mount lived.
func waitForHangup(fd int) error {
	var queue waiter.Queue
	entry, ch := waiter.NewChannelEntry(hangupEvents)
	queue.EventRegister(&entry)
	defer queue.EventUnregister(&entry)
	if err := fdnotifier.AddFD(int32(fd), &queue); err != nil {
		return pollForHangup(fd)
	}
	defer fdnotifier.RemoveFD(int32(fd))
	for fdnotifier.NonBlockingPoll(int32(fd), hangupEvents) == 0 {
		<-ch
	}
	return nil
}

// pollForHangup is waitForHangup in a poll of its own.
func pollForHangup(fd int) error {
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLHUP | unix.POLLRDHUP}}
	for {
		n, err := unix.Ppoll(events, nil, nil)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("poll returned %d events, want 1", n)
		}
		return nil
	}
}
