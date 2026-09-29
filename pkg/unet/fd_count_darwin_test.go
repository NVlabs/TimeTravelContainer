// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package unet

import (
	"testing"

	"golang.org/x/sys/unix"
)

func openFDCount(t *testing.T) int {
	t.Helper()
	count := 0
	for fd := 0; fd < unix.Getdtablesize(); fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
}
