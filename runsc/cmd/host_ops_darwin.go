// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cmd

import "golang.org/x/sys/unix"

// pathOnlyFlag opens a descriptor to exec its file: macOS has no O_PATH, so it
// reads.
const pathOnlyFlag = unix.O_RDONLY
