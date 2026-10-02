/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "hostmem.h"

#include <stddef.h>

#include "vn_protocol_renderer_util.h"

uint32_t vk_host_import_types(const void *table, uint64_t pd, void *ptr)
{
    const struct vn_physical_device_proc_table *pt = table;
    if (!pt->CreateDevice || !pt->GetDeviceProcAddr)
        return 0;
    float priority = 1.0f;
    VkDeviceQueueCreateInfo queue = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
        .queueFamilyIndex = 0,
        .queueCount = 1,
        .pQueuePriorities = &priority,
    };
    const char *extension = "VK_EXT_external_memory_host";
    VkDeviceCreateInfo info = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
        .queueCreateInfoCount = 1,
        .pQueueCreateInfos = &queue,
        .enabledExtensionCount = 1,
        .ppEnabledExtensionNames = &extension,
    };
    VkDevice device;
    if (pt->CreateDevice((VkPhysicalDevice)(uintptr_t)pd, &info, NULL, &device) != VK_SUCCESS)
        return 0;
    PFN_vkGetMemoryHostPointerPropertiesEXT get =
        (PFN_vkGetMemoryHostPointerPropertiesEXT)pt->GetDeviceProcAddr(device, "vkGetMemoryHostPointerPropertiesEXT");
    PFN_vkDestroyDevice destroy = (PFN_vkDestroyDevice)pt->GetDeviceProcAddr(device, "vkDestroyDevice");
    uint32_t types = 0;
    if (get) {
        VkMemoryHostPointerPropertiesEXT props = {
            .sType = VK_STRUCTURE_TYPE_MEMORY_HOST_POINTER_PROPERTIES_EXT,
        };
        if (get(device, VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT, ptr, &props) == VK_SUCCESS)
            types = props.memoryTypeBits;
    }
    if (destroy)
        destroy(device, NULL);
    return types;
}

uint64_t vk_host_import_alignment(const void *table, uint64_t pd)
{
    const struct vn_physical_device_proc_table *pt = table;
    if (!pt->GetPhysicalDeviceProperties2)
        return 0;
    VkPhysicalDeviceExternalMemoryHostPropertiesEXT host = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_MEMORY_HOST_PROPERTIES_EXT,
    };
    VkPhysicalDeviceProperties2 props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
        .pNext = &host,
    };
    pt->GetPhysicalDeviceProperties2((VkPhysicalDevice)(uintptr_t)pd, &props);
    return host.minImportedHostPointerAlignment;
}
