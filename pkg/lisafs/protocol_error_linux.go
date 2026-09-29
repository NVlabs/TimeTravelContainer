// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package lisafs

import "golang.org/x/sys/unix"

func protocolError(errno uint32) error {
	return unix.Errno(errno)
}
