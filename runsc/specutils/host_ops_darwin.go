// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package specutils

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func waitForNonChildExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return fmt.Errorf("watching process %d: %w", pid, err)
	}
	events := make([]unix.Kevent_t, 1)
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("process %d is still running after %v", pid, timeout)
		}
		ts := unix.NsecToTimespec(remaining.Nanoseconds())
		n, err := unix.Kevent(kq, nil, events, &ts)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for process %d: %w", pid, err)
		}
		if n > 0 {
			return nil
		}
	}
}

func safeMount(src, dst, fstype string, flags uintptr, data, procPath string) error {
	return fmt.Errorf("Linux host mount operations are unavailable on Darwin: %w", unix.EOPNOTSUPP)
}

func hostOOMScoreAdj(int) (int, error) { return 0, nil }
