// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import "golang.org/x/sys/unix"

// pathOnlyFlag opens a descriptor that only names its file, which is enough to
// exec it.
const pathOnlyFlag = unix.O_PATH
