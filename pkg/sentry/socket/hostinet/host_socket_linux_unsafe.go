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

package hostinet

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func newHostSocket(family, stype, protocol int) (int, error) {
	return unix.Socket(family, stype|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
}

func hostAccept4(fd int, addr *byte, addrlen *uint32, flags int) (int, error) {
	afd, _, errno := unix.Syscall6(unix.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(addr)), uintptr(unsafe.Pointer(addrlen)), uintptr(flags), 0, 0)
	if errno != 0 {
		return 0, translateIOSyscallError(errno)
	}
	return int(afd), nil
}
