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

//go:build linux

package fsutil

import "golang.org/x/sys/unix"

func mmapFileAtExact(fd int, offset, length uint64, prot int) (uintptr, unix.Errno) {
	addr, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		uintptr(offset),
		uintptr(length),
		uintptr(prot),
		unix.MAP_SHARED|unix.MAP_FIXED_NOREPLACE,
		uintptr(fd),
		uintptr(offset))
	if errno == 0 && uint64(addr) != offset {
		// The host kernel predates MAP_FIXED_NOREPLACE and the requested
		// address conflicts with an existing mapping.
		errno = unix.EEXIST
		unix.RawSyscall(unix.SYS_MUNMAP, addr, uintptr(length), 0)
	}
	return addr, errno
}
