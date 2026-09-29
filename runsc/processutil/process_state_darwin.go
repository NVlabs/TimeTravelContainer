// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package processutil

import "golang.org/x/sys/unix"

const darwinSZomb int8 = 5 // SZOMB from Darwin's <sys/proc.h>.

func processZombieState(pid int) (zombie, known bool) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || proc == nil || proc.Proc.P_pid != int32(pid) {
		return false, false
	}
	return proc.Proc.P_stat == darwinSZomb, true
}
