// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hosttid

import "golang.org/x/sys/unix"

func nativeTIDForTest() (uint64, bool) {
	return uint64(unix.Gettid()), true
}
