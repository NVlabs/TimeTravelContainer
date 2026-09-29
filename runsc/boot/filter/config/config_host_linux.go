// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package config

import "golang.org/x/sys/unix"

func hostHotSyscalls() []uintptr { return []uintptr{unix.SYS_FUTEX} }
