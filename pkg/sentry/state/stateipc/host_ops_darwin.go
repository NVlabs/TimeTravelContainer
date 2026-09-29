// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package stateipc

import "golang.org/x/sys/unix"

// pollWatchdog blocks until stopFD is readable or the socket's peer
// disconnects. Darwin has no POLLRDHUP, and waiting for readability with poll
// returned at once whenever unread bytes sat on the socket, so the caller's
// loop spun at full CPU. An EV_CLEAR read filter wakes once per change and
// flags the peer's close with EV_EOF.
func pollWatchdog(stopFD, socketFD int) (bool, bool, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return false, false, err
	}
	defer unix.Close(kq)
	changes := []unix.Kevent_t{
		{Ident: uint64(stopFD), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD},
		{Ident: uint64(socketFD), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	events := make([]unix.Kevent_t, len(changes))
	for {
		n, err := unix.Kevent(kq, changes, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, false, err
		}
		changes = nil
		stopped, disconnected := false, false
		for _, event := range events[:n] {
			switch int(event.Ident) {
			case stopFD:
				stopped = true
			case socketFD:
				disconnected = event.Flags&(unix.EV_EOF|unix.EV_ERROR) != 0
			}
		}
		if stopped || disconnected {
			return stopped, disconnected, nil
		}
	}
}

// Darwin does not expose Linux transparent-hugepage advice. Shared mappings
// retain the platform's normal VM policy.
func adviseHugepage(_ []byte, _ bool) error {
	return nil
}
