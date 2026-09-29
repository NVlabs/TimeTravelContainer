// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
