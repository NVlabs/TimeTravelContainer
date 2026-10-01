// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

// #include "metal.h"
import "C"

import "unsafe"

// MetalDevice returns the MTLDevice of a device, which must have enabled
// VK_EXT_metal_objects, or nil. physicalDevice is its physical device's
// table.
func MetalDevice(physicalDevice Table, device Handle) unsafe.Pointer {
	export := DeviceProc(physicalDevice, device, "vkExportMetalObjectsEXT")
	if export == nil {
		return nil
	}
	return C.vk_metal_device(C.uint64_t(device), export)
}

// MetalBuffer returns a shared MTLBuffer of size bytes of memory at p, which
// is not Go's, or nil. p and size must be multiples of the page size.
func MetalBuffer(device unsafe.Pointer, p unsafe.Pointer, size uint64) unsafe.Pointer {
	return C.vk_metal_buffer(device, p, C.uint64_t(size))
}

// MetalRelease releases an Objective-C object, such as a MetalBuffer.
func MetalRelease(obj unsafe.Pointer) {
	C.vk_metal_release(obj)
}
