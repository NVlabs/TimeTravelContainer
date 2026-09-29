// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package eventfd

import (
	"unsafe"

	"gvisor.dev/gvisor/pkg/safecopy"
)

func mmioCopyOut(dst unsafe.Pointer, src []byte) (int, error) {
	return safecopy.CopyOut(dst, src)
}
