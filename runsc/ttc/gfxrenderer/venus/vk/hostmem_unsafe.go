// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

// #include "hostmem.h"
import "C"

import "unsafe"

// HostImportTypes returns the memory types of the physical device pd that
// import the host memory at p, which is not Go's, with
// VK_EXT_external_memory_host, by a device of pd's own that it creates and
// destroys; or 0. physicalDevice is pd's table.
func HostImportTypes(physicalDevice Table, pd Handle, p unsafe.Pointer) uint32 {
	return uint32(C.vk_host_import_types(physicalDevice.p, C.uint64_t(pd), p))
}

// HostImportAlignment returns the alignment of the host memory that the
// physical device pd imports, or 0. physicalDevice is pd's table.
func HostImportAlignment(physicalDevice Table, pd Handle) uint64 {
	return uint64(C.vk_host_import_alignment(physicalDevice.p, C.uint64_t(pd)))
}
