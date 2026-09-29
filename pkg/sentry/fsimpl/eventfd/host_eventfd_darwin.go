// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package eventfd

import "golang.org/x/sys/unix"

func createHostEventFD(_ uint64, _ int) (int, error) {
	// A Darwin pipe requires distinct read/write descriptors and cannot be
	// donated through the Linux single-FD eventfd contract. Internal sentry
	// eventfds remain fully supported.
	return -1, unix.EOPNOTSUPP
}
