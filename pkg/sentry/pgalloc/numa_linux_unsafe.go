// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package pgalloc

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/log"
)

// maxNUMANodes is how many host memory nodes a node mask passed to the host
// has room for: Linux's largest MAX_NUMNODES, 1024.
const maxNUMANodes = 1024

// bindChunkMapping binds the pages of the shared memory file mapped at addr
// to the host memory node node, preferring it: the host falls back to other
// nodes when it is full, as it does for a thread's own local node.
func bindChunkMapping(addr, length uintptr, node int) {
	var mask [maxNUMANodes / 64]uint64
	mask[node/64] |= 1 << (node % 64)
	// Linux reads maxnode-1 bits of the mask; see mm/mempolicy.c:get_nodes().
	if _, _, errno := unix.Syscall6(unix.SYS_MBIND, addr, length, uintptr(linux.MPOL_PREFERRED), uintptr(unsafe.Pointer(&mask[0])), maxNUMANodes+1, 0); errno != 0 {
		log.Warningf("Binding MemoryFile chunk %#x to memory node %d failed: %v", addr, node, errno)
	}
}

// PageNode returns the host memory node of the page mapped at addr in the
// Sentry, committing it if it is not.
func PageNode(addr uintptr) (int, error) {
	var node int32
	if _, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&node)), 0, 0, addr, linux.MPOL_F_NODE|linux.MPOL_F_ADDR, 0); errno != 0 {
		return 0, errno
	}
	return int(node), nil
}
