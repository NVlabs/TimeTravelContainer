/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "metal.h"

#ifdef __APPLE__

#include <objc/message.h>
#include <objc/runtime.h>

#include "vn_protocol_renderer_defines.h"

void *vk_metal_device(uint64_t device, void *export_metal_objects)
{
    PFN_vkExportMetalObjectsEXT export_objects = (PFN_vkExportMetalObjectsEXT)export_metal_objects;
    if (!export_objects)
        return NULL;
    VkExportMetalDeviceInfoEXT device_info = {
        .sType = VK_STRUCTURE_TYPE_EXPORT_METAL_DEVICE_INFO_EXT,
    };
    VkExportMetalObjectsInfoEXT export_info = {
        .sType = VK_STRUCTURE_TYPE_EXPORT_METAL_OBJECTS_INFO_EXT,
        .pNext = &device_info,
    };
    export_objects((VkDevice)(uintptr_t)device, &export_info);
    return (void *)device_info.mtlDevice;
}

void *vk_metal_buffer(void *mtl_device, void *ptr, uint64_t size)
{
    if (!mtl_device)
        return NULL;
    /* -[MTLDevice newBufferWithBytesNoCopy:length:options:deallocator:],
     * with MTLResourceStorageModeShared, which is 0, and no deallocator: the
     * memory is the renderer's to unmap once the buffer is released. */
    typedef void *(*new_buffer_fn)(void *, SEL, void *, unsigned long, unsigned long, void *);
    SEL sel = sel_registerName("newBufferWithBytesNoCopy:length:options:deallocator:");
    return ((new_buffer_fn)objc_msgSend)(mtl_device, sel, ptr, (unsigned long)size, 0, NULL);
}

void vk_metal_release(void *obj)
{
    if (obj)
        ((void (*)(void *, SEL))objc_msgSend)(obj, sel_registerName("release"));
}

#else /* !__APPLE__ */

void *vk_metal_device(uint64_t device, void *export_metal_objects)
{
    (void)device;
    (void)export_metal_objects;
    return 0;
}

void *vk_metal_buffer(void *mtl_device, void *ptr, uint64_t size)
{
    (void)mtl_device;
    (void)ptr;
    (void)size;
    return 0;
}

void vk_metal_release(void *obj)
{
    (void)obj;
}

#endif /* __APPLE__ */
