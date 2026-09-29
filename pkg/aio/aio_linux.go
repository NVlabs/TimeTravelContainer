// Copyright 2024 The gVisor Authors.
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

package aio

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func executeRequest(r Request) Completion {
	var sysno uintptr
	switch r.Op {
	case OpRead:
		sysno = unix.SYS_PREAD64
	case OpWrite:
		sysno = unix.SYS_PWRITE64
	case OpReadv:
		sysno = unix.SYS_PREADV2
	case OpWritev:
		sysno = unix.SYS_PWRITEV2
	default:
		panic(fmt.Sprintf("unknown op %v", r.Op))
	}
	n, _, errno := unix.Syscall6(sysno, uintptr(r.FD), uintptr(r.Buf), uintptr(r.Len), uintptr(r.Off), 0, 0)
	result := Completion{ID: r.ID, Result: int64(n)}
	if errno != 0 {
		result.Result = -int64(errno)
	}
	return result
}
