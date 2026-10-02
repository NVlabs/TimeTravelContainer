/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef TTC_VENUS_HOSTMEM_H
#define TTC_VENUS_HOSTMEM_H

#include <stdint.h>

/* vk_host_import_types returns the memory types of the physical device pd
 * that import the host memory at ptr with VK_EXT_external_memory_host, as a
 * device of its own reports them, which it creates on queue family 0 and
 * destroys; or 0, if none does or the device cannot be created. pt is the
 * physical device's struct vn_physical_device_proc_table. */
uint32_t vk_host_import_types(const void *pt, uint64_t pd, void *ptr);

/* vk_host_import_alignment returns the alignment of the host memory that the
 * physical device pd imports, its minImportedHostPointerAlignment, or 0. */
uint64_t vk_host_import_alignment(const void *pt, uint64_t pd);

#endif /* TTC_VENUS_HOSTMEM_H */
