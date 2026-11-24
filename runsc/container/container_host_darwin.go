// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package container

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func containerHostSocketPair() ([2]int, error) {
	// Darwin has no SOCK_CLOEXEC. Hold ForkLock until both ends are
	// close-on-exec so a concurrent fork cannot inherit them.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

func setGoferParentDeathSignal(*exec.Cmd) {}

// openBeneath opens name in the directory dirFD through no symbolic link:
// macOS has no openat2(2), but name is one component.
func openBeneath(dirFD int, name string, flags int, mode uint32) (int, error) {
	return unix.Openat(dirFD, name, flags|unix.O_NOFOLLOW_ANY, mode)
}

// setCloneIntoCgroup does nothing: macOS has no cgroups to start a process in.
func setCloneIntoCgroup(*unix.SysProcAttr, *os.File) {}

func goferHostIsolationUnsupported() error {
	return fmt.Errorf("Linux gofer credential isolation is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

func setGoferCommandCredential(*exec.Cmd, uint32, uint32) error {
	return goferHostIsolationUnsupported()
}
func disableGoferSetgroupsForIDMapping(*exec.Cmd) error {
	return goferHostIsolationUnsupported()
}
func enableGoferSetgroupsForHostUDS(*exec.Cmd) error {
	return goferHostIsolationUnsupported()
}

func hostSetOOMScoreAdj(int, int) error { return nil }
