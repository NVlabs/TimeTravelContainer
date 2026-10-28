// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package nvproxy

import "golang.org/x/sys/unix"

func mirrorMapping(_, _, _ uintptr) unix.Errno { return unix.EOPNOTSUPP }

func advisePopulate(_, _ uintptr, _ bool) unix.Errno { return unix.EOPNOTSUPP }
