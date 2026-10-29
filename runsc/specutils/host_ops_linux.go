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

package specutils

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/processutil"
)

// PIDFS_MAGIC is the filesystem magic of pidfs, whose inode numbers identify
// processes.
const PIDFS_MAGIC = 0x50494446

// IsProcessRunning returns true if the process with the given PID and
// ProcessIdentity is running, rather than a zombie. Nonpositive PIDs and
// processes that no longer exist return false.
func IsProcessRunning(pid int, identity uint64) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	pidfd, err := OpenProcess(pid, identity)
	if err == unix.ESRCH {
		return false, nil
	}
	if err == unix.ENOSYS {
		// No pidfds (Linux < 5.3): the PID alone, and /proc for zombies.
		return processutil.IsRunning(pid), nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot open pidfd for process %d: %w", pid, err)
	}
	defer unix.Close(pidfd)

	pfds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(pfds, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("polling pidfd for process %d: %w", pid, err)
		}
		// A pidfd becomes readable (POLLIN) when the process exits (including when it is a zombie).
		return n == 0, nil
	}
}

// ProcessIdentity returns an identifier of the process with the given PID
// that is stable and unique. Returns 0 on hosts without `pidfs`, or without
// pidfds.
func ProcessIdentity(pid int) (uint64, error) {
	pidfd, err := OpenProcess(pid, 0)
	if err == unix.ENOSYS {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot open pidfd for process %d: %w", pid, err)
	}
	defer unix.Close(pidfd)
	return pidfdIdentity(pidfd)
}

func pidfdIdentity(pidfd int) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(pidfd, &fs); err != nil {
		return 0, fmt.Errorf("cannot statfs pidfd: %w", err)
	}
	if fs.Type != PIDFS_MAGIC {
		return 0, nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(pidfd, &st); err != nil {
		return 0, fmt.Errorf("cannot stat pidfd: %w", err)
	}
	return st.Ino, nil
}

// OpenProcess returns a pidfd for the process with the given PID and
// identity, or `unix.ESRCH` if there is no such process, or `unix.ENOSYS` if
// the host has no pidfds (Linux < 5.3).
// If `identity` is 0, is matches any process with the given PID.
func OpenProcess(pid int, identity uint64) (int, error) {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err == unix.EINVAL {
		return -1, unix.ESRCH // pid is a thread, not a process.
	}
	if err != nil || identity == 0 {
		return pidfd, err
	}
	got, err := pidfdIdentity(pidfd)
	if err == nil && got != identity {
		err = unix.ESRCH
	}
	if err != nil {
		unix.Close(pidfd)
		return -1, err
	}
	return pidfd, nil
}

// KillProcess sends SIGKILL to the process with the given PID and
// ProcessIdentity, if it exists.
// If `identity` is zero, the process with PID `pid` is killed regardless
// of its identity, as it is on hosts without pidfds.
func KillProcess(pid int, identity uint64) error {
	pidfd, err := OpenProcess(pid, identity)
	if err == unix.ESRCH {
		return nil
	}
	if err == unix.ENOSYS {
		if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
			return fmt.Errorf("cannot kill process %d: %w", pid, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot open pidfd for process %d: %w", pid, err)
	}
	defer unix.Close(pidfd)
	if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && err != unix.ESRCH {
		return fmt.Errorf("cannot kill process %d: %w", pid, err)
	}
	return nil
}

// WaitForNonChildExit waits for the process with the given PID and
// `identity` to exit, up to `timeout`.
// If `identity` is zero, match processes on `pid` alone.
// `pid` must not be a child of the current process.
// Returns immediately if the process does not exist.
func WaitForNonChildExit(pid int, identity uint64, timeout time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	pidfd, err := OpenProcess(pid, identity)
	if err == unix.ESRCH {
		return nil // Already gone.
	}
	if err == unix.ENOSYS {
		// No pidfds (Linux < 5.3). Fall back to polling.
		b := backoff.WithMaxRetries(backoff.NewConstantBackOff(10*time.Millisecond), uint64(timeout/(10*time.Millisecond)))
		return backoff.Retry(func() error {
			if err := unix.Kill(pid, 0); err == nil {
				return fmt.Errorf("process %d is still running", pid)
			}
			return nil
		}, b)
	}
	if err != nil {
		return fmt.Errorf("cannot open pidfd for process %d: %w", pid, err)
	}
	defer unix.Close(pidfd)
	deadline := time.Now().Add(timeout)
	for {
		timeoutMS := int(time.Until(deadline).Milliseconds())
		if timeoutMS <= 0 {
			return fmt.Errorf("process %d is still running after %v", pid, timeout)
		}
		pfds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, timeoutMS)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("polling pidfd for process %d: %v", pid, err)
		}
		if n > 0 {
			return nil // The pidfd becomes "readable" when the process exits.
		}
	}
}

func safeMount(src, dst, fstype string, flags uintptr, data, procPath string) error {
	fd, err := unix.Open(dst, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to safely mount: Open(%s, _, _): %w", dst, err)
	}
	defer unix.Close(fd)
	if procPath == "" {
		procPath = "/proc"
	}
	safePath := filepath.Join(procPath, "self/fd", strconv.Itoa(fd))
	target, err := os.Readlink(safePath)
	if err != nil {
		return fmt.Errorf("failed to safely mount: Readlink(%s): %w", safePath, err)
	}
	if dst != target {
		return &ErrSymlinkMount{fmt.Errorf("failed to safely mount: expected to open %s, but found %s", dst, target)}
	}
	if err := unix.Mount(src, safePath, fstype, flags, data); err != nil {
		return fmt.Errorf("mount(%q, %q, %q, %#x, %q) failed: %w", src, safePath, fstype, flags, data, err)
	}
	return nil
}

func hostOOMScoreAdj(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// specCopyFile returns a sealed memfd holding data.
func specCopyFile(data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("config.json", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("cannot create memfd: %w", err)
	}
	f := os.NewFile(uintptr(fd), "config.json")
	if _, err := f.WriteAt(data, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot write spec copy: %w", err)
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot seal spec copy: %w", err)
	}
	return f, nil
}
