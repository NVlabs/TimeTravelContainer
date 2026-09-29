// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package hostthread

import "golang.org/x/sys/unix"

// SetSyscallsEnd sets whether the calling goroutine's host system calls all
// end, and returns the previous setting. Only the Darwin runtime uses it.
func SetSyscallsEnd(bool) bool { return false }

// SyscallsEnd records that all of the calling goroutine's host system calls
// end. Only the Darwin runtime uses it.
func SyscallsEnd() {}

// Flock is flock(2).
func Flock(fd int, how int) error {
	return unix.Flock(fd, how)
}
