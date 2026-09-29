// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package sentrycmd

import (
	"os"

	"golang.org/x/sys/unix"
)

// armParentDeathSignal emulates Linux PR_SET_PDEATHSIG with an EVFILT_PROC
// watcher. The watcher is process-local and is therefore re-armed after exec.
func armParentDeathSignal() error {
	parentPID := os.Getppid()
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	change := unix.Kevent_t{
		Ident:  uint64(parentPID),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		unix.Close(kq)
		return err
	}
	if parentPID <= 1 || os.Getppid() != parentPID {
		unix.Close(kq)
		return unix.ESRCH
	}
	go func() {
		defer unix.Close(kq)
		events := make([]unix.Kevent_t, 1)
		for {
			if n, err := unix.Kevent(kq, nil, events, nil); err == unix.EINTR {
				continue
			} else if err == nil && n != 0 {
				_ = unix.Kill(os.Getpid(), unix.SIGKILL)
			}
			return
		}
	}()
	return nil
}
