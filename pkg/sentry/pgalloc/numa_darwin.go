// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package pgalloc

import "gvisor.dev/gvisor/pkg/errors/linuxerr"

// bindChunkMapping does nothing: the host has no memory nodes to bind to.
func bindChunkMapping(addr, length uintptr, node int) {}

// PageNode fails: the host has no memory nodes.
func PageNode(addr uintptr) (int, error) {
	return 0, linuxerr.ENOSYS
}
