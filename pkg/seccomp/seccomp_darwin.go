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

package seccomp

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/bpf"
)

func SetFilter([]bpf.Instruction) error {
	return fmt.Errorf("seccomp filters are unavailable on Darwin")
}

//go:nosplit
func SetFilterInChild([]bpf.Instruction) unix.Errno { return unix.ENOSYS }

func isKillProcessAvailable() (bool, error) { return false, nil }

//go:nosplit
func seccomp(uint32, uint32, unsafe.Pointer) (uintptr, unix.Errno) {
	return 0, unix.ENOSYS
}
