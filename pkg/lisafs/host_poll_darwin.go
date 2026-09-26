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
