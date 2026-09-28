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
