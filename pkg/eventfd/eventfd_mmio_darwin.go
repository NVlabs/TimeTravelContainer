// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package eventfd

import (
	"fmt"
	"unsafe"
)

func mmioCopyOut(unsafe.Pointer, []byte) (int, error) {
	return 0, fmt.Errorf("eventfd MMIO is unavailable on Darwin")
}
