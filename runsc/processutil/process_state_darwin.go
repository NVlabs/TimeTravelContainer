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
