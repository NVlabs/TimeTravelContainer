// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package glibccompat

// The package's C files, start_dl.c and pthread.c, are empty but on Linux.

import "C"
