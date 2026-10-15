// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package pgalloc

import (
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// bindChunkMapping does nothing: the host has no memory nodes to bind to.
func bindChunkMapping(addr, length uintptr, node int) {}

// bindFileRange does nothing, as bindChunkMapping does.
func bindFileRange(fd uintptr, fr memmap.FileRange, node int) {}

// PageNode fails: the host has no memory nodes.
func PageNode(addr uintptr) (int, error) {
	return 0, linuxerr.ENOSYS
}
