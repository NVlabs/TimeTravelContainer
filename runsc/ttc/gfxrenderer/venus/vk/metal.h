/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef TTC_VENUS_METAL_H
#define TTC_VENUS_METAL_H

#include <stdint.h>

/* vk_metal_device returns the MTLDevice of a VkDevice, by
 * export_metal_objects, its vkExportMetalObjectsEXT, or NULL. */
void *vk_metal_device(uint64_t device, void *export_metal_objects);

/* vk_metal_buffer returns a shared MTLBuffer of the size bytes at ptr, which
 * it does not copy, or NULL. ptr and size must be multiples of the page
 * size. */
void *vk_metal_buffer(void *mtl_device, void *ptr, uint64_t size);

/* vk_metal_release releases an Objective-C object. */
void vk_metal_release(void *obj);

#endif /* TTC_VENUS_METAL_H */
