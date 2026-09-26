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

package flipcall

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func sealPacketWindowFile(fd int) error {
	// Prevent either party from shrinking the backing file or adding seals
	// that would make the shared packet windows unusable.
	if _, _, errno := unix.RawSyscall(unix.SYS_FCNTL, uintptr(fd), linux.F_ADD_SEALS, linux.F_SEAL_SHRINK|linux.F_SEAL_SEAL); errno != 0 {
		return fmt.Errorf("failed to apply memfd seals: %v", errno)
	}
	return nil
}
