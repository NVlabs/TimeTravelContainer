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
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// IsRunning reports whether pid still has executable process state.
// kill(pid, 0) alone is insufficient because it succeeds for an unreaped
// zombie.
func IsRunning(pid int) bool {
	if pid <= 0 || unix.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// The process was observable by kill(2), but procfs may be unavailable
		// or restricted. Preserve the conservative result.
		return true
	}
	// comm is parenthesized and may itself contain spaces or ')'. The state is
	// the byte after the final ") ".
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 || end+2 >= len(stat) {
		return true
	}
	switch stat[end+2] {
	case 'Z', 'X', 'x':
		return false
	default:
		return true
	}
}
