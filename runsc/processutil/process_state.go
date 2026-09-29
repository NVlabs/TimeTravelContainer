// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package processutil contains lightweight host-process inspection helpers.
package processutil

import (
	"golang.org/x/sys/unix"
)

// IsRunning reports whether pid still has executable process state.
// kill(pid, 0) alone is insufficient because it succeeds for an unreaped
// zombie.
func IsRunning(pid int) bool {
	if pid <= 0 || unix.Kill(pid, 0) != nil {
		return false
	}
	if zombie, known := processZombieState(pid); known {
		return !zombie
	}
	// The process was observable by kill(2), but the host-specific process
	// state interface may be unavailable or restricted. Preserve the
	// conservative result.
	return true
}
