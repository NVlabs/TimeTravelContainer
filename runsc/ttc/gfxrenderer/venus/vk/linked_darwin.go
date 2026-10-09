// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

/*
#cgo CFLAGS: -I${SRCDIR}/include -DVK_NO_PROTOTYPES -DVK_USE_PLATFORM_METAL_EXT
#include <vulkan/vulkan.h>

// MoltenVK, which the renderer links on macOS, answers for Vulkan's commands
// itself, as the loader does.
extern VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL vkGetInstanceProcAddr(VkInstance instance, const char *pName);

static PFN_vkGetInstanceProcAddr vk_linked_gipa(void) {
	return vkGetInstanceProcAddr;
}
*/
import "C"

// Linked returns the Vulkan that the renderer links, MoltenVK, so that ttcd
// loads no library but Apple's.
func Linked() (*Library, bool) {
	return newLibrary(nil, C.vk_linked_gipa()), true
}
