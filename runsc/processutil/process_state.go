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
