// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tmpfs

import "golang.org/x/sys/unix"

func syncFileData(fd int) error { return unix.Fdatasync(fd) }
