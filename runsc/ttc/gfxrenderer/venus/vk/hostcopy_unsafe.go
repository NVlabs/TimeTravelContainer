// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

/*
#include <stdlib.h>
#include "vn_protocol_renderer_util.h"

// Venus's own host copies and descriptor writes carry their memory inline,
// for Vulkan's commands that take host pointers, which virglrenderer's
// vkr_host_copy.c and vkr_descriptor_heap.c call as these do.

static int32_t vk_copy_memory_to_image_mesa(const struct vn_device_proc_table *t,
		uint64_t device, const VkCopyMemoryToImageInfoMESA *info) {
	if (!t->CopyMemoryToImage)
		return VK_ERROR_FEATURE_NOT_PRESENT;
	VkMemoryToImageCopy *regions = calloc(info->regionCount ? info->regionCount : 1, sizeof(*regions));
	if (!regions)
		return VK_ERROR_OUT_OF_HOST_MEMORY;
	for (uint32_t i = 0; i < info->regionCount; i++) {
		const VkMemoryToImageCopyMESA *r = &info->pRegions[i];
		regions[i] = (VkMemoryToImageCopy){
			.sType = VK_STRUCTURE_TYPE_MEMORY_TO_IMAGE_COPY,
			.pHostPointer = r->pData,
			.memoryRowLength = r->memoryRowLength,
			.memoryImageHeight = r->memoryImageHeight,
			.imageSubresource = r->imageSubresource,
			.imageOffset = r->imageOffset,
			.imageExtent = r->imageExtent,
		};
	}
	const VkCopyMemoryToImageInfo local = {
		.sType = VK_STRUCTURE_TYPE_COPY_MEMORY_TO_IMAGE_INFO,
		.flags = info->flags,
		.dstImage = info->dstImage,
		.dstImageLayout = info->dstImageLayout,
		.regionCount = info->regionCount,
		.pRegions = regions,
	};
	VkResult result = t->CopyMemoryToImage((VkDevice)(uintptr_t)device, &local);
	free(regions);
	return result;
}

static int32_t vk_copy_image_to_memory_mesa(const struct vn_device_proc_table *t,
		uint64_t device, const VkCopyImageToMemoryInfoMESA *info, void *data) {
	if (!t->CopyImageToMemory)
		return VK_ERROR_FEATURE_NOT_PRESENT;
	const VkImageToMemoryCopy region = {
		.sType = VK_STRUCTURE_TYPE_IMAGE_TO_MEMORY_COPY,
		.pHostPointer = data,
		.memoryRowLength = info->memoryRowLength,
		.memoryImageHeight = info->memoryImageHeight,
		.imageSubresource = info->imageSubresource,
		.imageOffset = info->imageOffset,
		.imageExtent = info->imageExtent,
	};
	const VkCopyImageToMemoryInfo local = {
		.sType = VK_STRUCTURE_TYPE_COPY_IMAGE_TO_MEMORY_INFO,
		.flags = info->flags,
		.srcImage = info->srcImage,
		.srcImageLayout = info->srcImageLayout,
		.regionCount = 1,
		.pRegions = &region,
	};
	return t->CopyImageToMemory((VkDevice)(uintptr_t)device, &local);
}

static int32_t vk_write_sampler_descriptor_mesa(const struct vn_device_proc_table *t,
		uint64_t device, const VkSamplerCreateInfo *sampler, uint64_t size, void *data) {
	if (!t->WriteSamplerDescriptorsEXT)
		return VK_ERROR_FEATURE_NOT_PRESENT;
	const VkHostAddressRangeEXT range = { .address = data, .size = size };
	return t->WriteSamplerDescriptorsEXT((VkDevice)(uintptr_t)device, 1, sampler, &range);
}

static int32_t vk_write_resource_descriptor_mesa(const struct vn_device_proc_table *t,
		uint64_t device, const VkResourceDescriptorInfoEXT *resource, uint64_t size, void *data) {
	if (!t->WriteResourceDescriptorsEXT)
		return VK_ERROR_FEATURE_NOT_PRESENT;
	const VkHostAddressRangeEXT range = { .address = data, .size = size };
	return t->WriteResourceDescriptorsEXT((VkDevice)(uintptr_t)device, 1, resource, &range);
}
*/
import "C"

import "unsafe"

// CopyMemoryToImageMESA executes vkCopyMemoryToImageMESA's info, a
// VkCopyMemoryToImageInfoMESA whose handles are the host's, through a
// device's table.
func CopyMemoryToImageMESA(t Table, device Handle, info unsafe.Pointer) int32 {
	return int32(C.vk_copy_memory_to_image_mesa((*C.struct_vn_device_proc_table)(t.p), C.uint64_t(device),
		(*C.VkCopyMemoryToImageInfoMESA)(info)))
}

// CopyImageToMemoryMESA executes vkCopyImageToMemoryMESA's info, a
// VkCopyImageToMemoryInfoMESA whose handles are the host's, into data.
func CopyImageToMemoryMESA(t Table, device Handle, info, data unsafe.Pointer) int32 {
	return int32(C.vk_copy_image_to_memory_mesa((*C.struct_vn_device_proc_table)(t.p), C.uint64_t(device),
		(*C.VkCopyImageToMemoryInfoMESA)(info), data))
}

// WriteSamplerDescriptorMESA executes vkWriteSamplerDescriptorMESA.
func WriteSamplerDescriptorMESA(t Table, device Handle, sampler unsafe.Pointer, size uint64, data unsafe.Pointer) int32 {
	return int32(C.vk_write_sampler_descriptor_mesa((*C.struct_vn_device_proc_table)(t.p), C.uint64_t(device),
		(*C.VkSamplerCreateInfo)(sampler), C.uint64_t(size), data))
}

// WriteResourceDescriptorMESA executes vkWriteResourceDescriptorMESA.
func WriteResourceDescriptorMESA(t Table, device Handle, resource unsafe.Pointer, size uint64, data unsafe.Pointer) int32 {
	return int32(C.vk_write_resource_descriptor_mesa((*C.struct_vn_device_proc_table)(t.p), C.uint64_t(device),
		(*C.VkResourceDescriptorInfoEXT)(resource), C.uint64_t(size), data))
}
