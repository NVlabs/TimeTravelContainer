// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package specutils

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/processutil"
)

// IsProcessRunning returns true if the process with the given PID is running,
// rather than a zombie. Darwin has no process identities: identity is
// ignored.
func IsProcessRunning(pid int, _ uint64) (bool, error) {
	return processutil.IsRunning(pid), nil
}

// ProcessIdentity returns 0: Darwin has no stable process identities.
func ProcessIdentity(int) (uint64, error) {
	return 0, nil
}

// KillProcess sends SIGKILL to the process with the given PID, if it exists.
// Darwin has no process identities: identity is ignored.
func KillProcess(pid int, _ uint64) error {
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		return fmt.Errorf("cannot kill process %d: %w", pid, err)
	}
	return nil
}

// WaitForNonChildExit waits for the process with the given PID to exit, up to
// `timeout`. `pid` must not be a child of the current process. Darwin has no
// process identities: identity is ignored.
func WaitForNonChildExit(pid int, _ uint64, timeout time.Duration) error {
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

// specCopyFile returns an unlinked file holding data. macOS has neither memfds
// nor file seals, so the copy is not sealed; only the processes that it is
// passed to can reach it.
func specCopyFile(data []byte) (*os.File, error) {
	f, err := os.CreateTemp("", "config.json-")
	if err != nil {
		return nil, fmt.Errorf("cannot create spec copy: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot unlink spec copy: %w", err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot write spec copy: %w", err)
	}
	return f, nil
}
