// Copyright 2018 Google LLC
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

//go:build darwin

// Package fdnotifier translates native Darwin kqueue events into waiter
// notifications.
package fdnotifier

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

type fdInfo struct {
	queue        *waiter.Queue
	class        fdClass
	readWaiting  bool
	writeWaiting bool
}

type notifier struct {
	kq int

	pauseMu sync.Mutex
	mu      sync.Mutex
	fdMap   map[int32]*fdInfo
}

func newNotifier() (*notifier, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	n := &notifier{kq: kq, fdMap: make(map[int32]*fdInfo)}
	go n.waitAndNotify() // S/R-SAFE: no waiter exists during save/load.
	return n, nil
}

func (n *notifier) setFilter(fd int32, filter int, add bool) error {
	var change unix.Kevent_t
	flags := unix.EV_DELETE
	if add {
		flags = unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR
	}
	unix.SetKevent(&change, int(fd), filter, flags)
	_, err := unix.Kevent(n.kq, []unix.Kevent_t{change}, nil, nil)
	if !add && err == unix.ENOENT {
		return nil
	}
	return err
}

func (n *notifier) waitFD(fd int32, info *fdInfo, mask waiter.EventMask) error {
	wantRead := mask&(waiter.ReadableEvents|waiter.EventPri|waiter.EventRdHUp) != 0
	wantWrite := mask&waiter.WritableEvents != 0
	if mask != 0 && !wantRead && !wantWrite {
		// Like epoll, report hangups and errors to every waiter. kqueue
		// reports them through the filter of the direction that failed, so
		// watch a direction the FD can use.
		if info.class.readable {
			wantRead = true
		} else {
			wantWrite = true
		}
	}
	if wantRead != info.readWaiting {
		if err := n.setFilter(fd, unix.EVFILT_READ, wantRead); err != nil {
			return err
		}
		info.readWaiting = wantRead
	}
	if wantWrite != info.writeWaiting {
		if err := n.setFilter(fd, unix.EVFILT_WRITE, wantWrite); err != nil {
			return err
		}
		info.writeWaiting = wantWrite
	}
	return nil
}

func (n *notifier) addFD(fd int32, queue *waiter.Queue) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.fdMap[fd]; ok {
		panic(fmt.Sprintf("file descriptor %d added twice", fd))
	}
	info := &fdInfo{queue: queue, class: classifyFD(fd)}
	if err := n.waitFD(fd, info, queue.Events()); err != nil {
		return err
	}
	n.fdMap[fd] = info
	return nil
}

func (n *notifier) updateFD(fd int32) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if info, ok := n.fdMap[fd]; ok {
		return n.waitFD(fd, info, info.queue.Events())
	}
	return nil
}

func (n *notifier) removeFD(fd int32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if info, ok := n.fdMap[fd]; ok {
		_ = n.waitFD(fd, info, 0)
		delete(n.fdMap, fd)
	}
}

func (n *notifier) hasFD(fd int32) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.fdMap[fd]
	return ok
}

func (n *notifier) waitAndNotify() error {
	events := make([]unix.Kevent_t, 100)
	for {
		count, err := unix.Kevent(n.kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		notified := false
		n.pauseMu.Lock()
		n.mu.Lock()
		for i := 0; i < count; i++ {
			event := events[i]
			info, ok := n.fdMap[int32(event.Ident)]
			if !ok {
				continue
			}
			var mask waiter.EventMask
			switch event.Filter {
			case unix.EVFILT_READ:
				mask |= waiter.ReadableEvents
				if event.Flags&unix.EV_OOBAND != 0 {
					mask |= waiter.EventPri
				}
			case unix.EVFILT_WRITE:
				mask |= waiter.WritableEvents
			}
			if event.Flags&unix.EV_EOF != 0 {
				if event.Fflags != 0 {
					// A socket error, such as ECONNRESET.
					mask |= waiter.EventErr
				}
				// Whether EOF is a hangup depends on the other direction.
				read, write, err := pollDirections(int32(event.Ident))
				readEOF := event.Filter == unix.EVFILT_READ || (err == nil && read&unix.POLLHUP != 0)
				writeEOF := event.Filter == unix.EVFILT_WRITE || (err == nil && write&unix.POLLHUP != 0)
				mask |= eofEvents(info.class, readEOF, writeEOF)
			}
			if event.Flags&unix.EV_ERROR != 0 && event.Data != 0 {
				mask |= waiter.EventErr
			}
			info.queue.Notify(mask)
			notified = true
		}
		n.mu.Unlock()
		n.pauseMu.Unlock()
		if notified {
			sync.Goyield()
		}
	}
}

func (n *notifier) pause()  { n.pauseMu.Lock() }
func (n *notifier) resume() { n.pauseMu.Unlock() }

var shared struct {
	notifier *notifier
	once     sync.Once
	initErr  error
}

func ensureSharedNotifier() {
	shared.once.Do(func() {
		shared.notifier, shared.initErr = newNotifier()
	})
}

// AddFD adds fd to the shared notifier.
func AddFD(fd int32, queue *waiter.Queue) error {
	ensureSharedNotifier()
	if shared.initErr != nil {
		return shared.initErr
	}
	return shared.notifier.addFD(fd, queue)
}

// UpdateFD updates fd's requested event set.
func UpdateFD(fd int32) error {
	ensureSharedNotifier()
	if shared.initErr != nil {
		return shared.initErr
	}
	return shared.notifier.updateFD(fd)
}

// RemoveFD removes fd from the shared notifier.
func RemoveFD(fd int32) {
	ensureSharedNotifier()
	if shared.initErr == nil {
		shared.notifier.removeFD(fd)
	}
}

// HasFD reports whether fd is registered. It is intended for tests.
func HasFD(fd int32) bool {
	ensureSharedNotifier()
	return shared.initErr == nil && shared.notifier.hasFD(fd)
}

// Pause suspends notifications until Resume.
func Pause() {
	ensureSharedNotifier()
	if shared.initErr == nil {
		shared.notifier.pause()
	}
}

// Resume resumes notifications after Pause.
func Resume() {
	if shared.initErr == nil && shared.notifier != nil {
		shared.notifier.resume()
	}
}
