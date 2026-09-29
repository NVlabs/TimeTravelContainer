// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
