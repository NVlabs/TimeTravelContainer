// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package rdmaproxy

import "golang.org/x/sys/unix"

func mirrorMapping(_, _, _ uintptr) unix.Errno { return unix.EOPNOTSUPP }

func advisePopulateWrite(_, _ uintptr) unix.Errno { return unix.EOPNOTSUPP }
