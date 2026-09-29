// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bundle

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneOpenFile(destination, source *os.File) (*os.File, error) {
	return destination, unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
}
