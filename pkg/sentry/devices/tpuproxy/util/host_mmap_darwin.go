// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package util

import "golang.org/x/sys/unix"

// MirrorMapping is unavailable because Darwin has no Linux mremap analogue.
func MirrorMapping(_, _, _ uintptr) unix.Errno { return unix.EOPNOTSUPP }
