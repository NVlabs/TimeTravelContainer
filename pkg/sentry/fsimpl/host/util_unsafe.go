//go:build linux

// Copyright 2020 The gVisor Authors.
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

package host

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func setTimestamps(fd int, atime linux.StatxTimestamp, omitAtime bool, mtime linux.StatxTimestamp, omitMtime bool) error {
	toTimespec := func(ts linux.StatxTimestamp, omit bool) unix.Timespec {
		if omit {
			return unix.Timespec{Nsec: unix.UTIME_OMIT}
		}
		return unix.Timespec{Sec: ts.Sec, Nsec: int64(ts.Nsec)}
	}
	ts := [2]unix.Timespec{toTimespec(atime, omitAtime), toTimespec(mtime, omitMtime)}
	_, _, errno := unix.Syscall6(
		unix.SYS_UTIMENSAT,
		uintptr(fd),
		0, /* path */
		uintptr(unsafe.Pointer(&ts)),
		0, /* flags */
		0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
