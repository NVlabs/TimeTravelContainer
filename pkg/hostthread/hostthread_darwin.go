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

//go:build darwin

package hostthread

import (
	_ "unsafe" // Required for go:linkname.

	"golang.org/x/sys/unix"
)

// SetSyscallsEnd sets whether the calling goroutine's host system calls all
// end, rather than block for as long as, say, a peer stays silent: those on
// non-blocking descriptors do, as do file I/O and requests to a gofer. A
// thread that keeps an HVF vCPU parked, which only the thread can retire,
// retires it before any other goroutine's system call, in case the call
// never returns; it keeps it across these.
//
// It returns the previous setting, so that code can declare only its own
// calls:
//
//	defer hostthread.SetSyscallsEnd(hostthread.SetSyscallsEnd(true))
//
// It requires the Go runtime patched by tools/go_darwin_vm_hooks.patch.
//
//go:linkname SetSyscallsEnd runtime.ttcSetSyscallsEnd
func SetSyscallsEnd(end bool) bool

// SyscallsEnd records that all of the calling goroutine's host system calls
// end; see SetSyscallsEnd.
func SyscallsEnd() {
	SetSyscallsEnd(true)
}

// Flock is flock(2) for a goroutine whose other system calls end: a lock that
// another process holds may take as long as that process likes, so a
// contended acquisition blocks as a system call that may not end.
func Flock(fd int, how int) error {
	if how&(unix.LOCK_NB|unix.LOCK_UN) == 0 {
		err := unix.Flock(fd, how|unix.LOCK_NB)
		if err != unix.EWOULDBLOCK {
			return err
		}
		defer SetSyscallsEnd(SetSyscallsEnd(false))
	}
	return unix.Flock(fd, how)
}
