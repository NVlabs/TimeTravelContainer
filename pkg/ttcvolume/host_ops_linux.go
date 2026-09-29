// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ttcvolume

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func cloneFile(source, destination *os.File, _ int, _ string) (*os.File, error) {
	if err := unix.IoctlFileClone(int(destination.Fd()), int(source.Fd())); err != nil {
		return destination, err
	}
	return destination, nil
}

func syncFileData(file *os.File) error {
	return unix.Fdatasync(int(file.Fd()))
}

func allocateFile(file *os.File, offset, length int64) error {
	return unix.Fallocate(int(file.Fd()), 0, offset, length)
}

// directoryFDPath returns a path that names the open directory dir.
func directoryFDPath(dir *os.File) (string, error) {
	return fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), nil
}
