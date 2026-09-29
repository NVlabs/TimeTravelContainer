// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package lisafs

import "golang.org/x/sys/unix"

func waitForHangup(fd int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	// Darwin has no POLLRDHUP. Waiting for readability with poll spins at full
	// CPU whenever unread RPC bytes sit on the socket. An EV_CLEAR read filter
	// instead wakes once per change and flags the peer's close with EV_EOF.
	changes := []unix.Kevent_t{{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, changes, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		changes = nil
		if n == 1 && events[0].Flags&(unix.EV_EOF|unix.EV_ERROR) != 0 {
			return nil
		}
	}
}
