// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package memutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// CreateMemFD emulates Linux memfd_create with an unlinked temporary file.
// The returned descriptor is the caller's responsibility.
func CreateMemFD(name string, flags int) (int, error) {
	file, err := os.CreateTemp("", "gvisor_"+name+"_")
	if err != nil {
		return -1, fmt.Errorf("creating anonymous backing file: %w", err)
	}
	path := file.Name()
	// Linux MFD_CLOEXEC is bit 0. Darwin cannot emulate seals, but it can
	// preserve descriptor inheritance semantics; F_DUPFD_CLOEXEC sets the flag
	// atomically so a concurrent fork cannot inherit the descriptor.
	dupCmd := unix.F_DUPFD
	if flags&1 != 0 {
		dupCmd = unix.F_DUPFD_CLOEXEC
	}
	fd, dupErr := unix.FcntlInt(file.Fd(), dupCmd, 0)
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if dupErr != nil {
		return -1, fmt.Errorf("duplicating anonymous backing file: %w", dupErr)
	}
	if closeErr != nil {
		unix.Close(fd)
		return -1, closeErr
	}
	if removeErr != nil {
		unix.Close(fd)
		return -1, removeErr
	}
	return fd, nil
}
