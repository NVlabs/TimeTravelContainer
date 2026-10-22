// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

// The renderer's Vulkan for Zink on Linux: the host's loader, which the Venus
// renderer loads too (venus.go), and of the host's physical devices those
// that the Venus renderer gives guests (venus/instance_unsafe.go): its GPUs,
// or, on a host that has none, its CPU devices, such as lavapipe, which Zink
// renders on only where asked to.

#include <dlfcn.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "virgl_int.h"
#include "zink_public.h"

static PFN_vkGetInstanceProcAddr real_gipa;
static PFN_vkCreateInstance real_create_instance;
static PFN_vkEnumeratePhysicalDevices real_enumerate_physical_devices;

// hardware_devices returns the physical devices of instance that the
// renderer renders on, *n of them, which the caller frees, and in *cpu
// whether they are CPU devices; or NULL, with the error in *result.
static VkPhysicalDevice *hardware_devices(VkInstance instance, uint32_t *n, bool *cpu,
                                          VkResult *result) {
   PFN_vkEnumeratePhysicalDevices enumerate = real_enumerate_physical_devices;
   if (!enumerate)
      enumerate = (PFN_vkEnumeratePhysicalDevices)real_gipa(instance, "vkEnumeratePhysicalDevices");
   PFN_vkGetPhysicalDeviceProperties properties = (PFN_vkGetPhysicalDeviceProperties)real_gipa(
      instance, "vkGetPhysicalDeviceProperties");
   if (!enumerate || !properties) {
      *result = VK_ERROR_INITIALIZATION_FAILED;
      return NULL;
   }
   uint32_t count = 0;
   *result = enumerate(instance, &count, NULL);
   if (*result != VK_SUCCESS)
      return NULL;
   VkPhysicalDevice *all = calloc(count + 1, sizeof(*all));
   VkPhysicalDevice *kept = calloc(count + 1, sizeof(*kept));
   if (!all || !kept) {
      free(all);
      free(kept);
      *result = VK_ERROR_OUT_OF_HOST_MEMORY;
      return NULL;
   }
   *result = enumerate(instance, &count, all);
   if (*result != VK_SUCCESS && *result != VK_INCOMPLETE) {
      free(all);
      free(kept);
      return NULL;
   }
   *result = VK_SUCCESS;
   uint32_t gpus = 0;
   for (uint32_t i = 0; i < count; i++) {
      VkPhysicalDeviceProperties props;
      properties(all[i], &props);
      if (props.deviceType != VK_PHYSICAL_DEVICE_TYPE_CPU)
         kept[gpus++] = all[i];
   }
   *cpu = gpus == 0;
   if (*cpu) {
      free(kept);
      *n = count;
      return all;
   }
   free(all);
   *n = gpus;
   return kept;
}

// Zink takes the first physical device, unless it renders on a CPU device,
// which it finds itself: Zink's Vulkan enumerates only the devices that the
// renderer renders on.
static VkResult VKAPI_CALL quirk_enumerate_physical_devices(VkInstance instance, uint32_t *count,
                                                            VkPhysicalDevice *pdevs) {
   uint32_t n;
   bool cpu;
   VkResult result;
   VkPhysicalDevice *devices = hardware_devices(instance, &n, &cpu, &result);
   if (!devices)
      return result;
   if (!pdevs) {
      *count = n;
      free(devices);
      return VK_SUCCESS;
   }
   uint32_t m = n < *count ? n : *count;
   memcpy(pdevs, devices, m * sizeof(*pdevs));
   *count = m;
   free(devices);
   return m < n ? VK_INCOMPLETE : VK_SUCCESS;
}

// Zink chooses its physical device once it has created its instance: Zink's
// Vulkan has it render on a CPU device by then if the host has no GPU.
static VkResult VKAPI_CALL quirk_create_instance(const VkInstanceCreateInfo *info,
                                                 const VkAllocationCallbacks *alloc,
                                                 VkInstance *instance) {
   VkResult result = real_create_instance(info, alloc, instance);
   if (result != VK_SUCCESS)
      return result;
   uint32_t n;
   bool cpu;
   VkResult enumerated;
   VkPhysicalDevice *devices = hardware_devices(*instance, &n, &cpu, &enumerated);
   if (devices) {
      zink_vk_cpu_device = cpu && n > 0;
      free(devices);
   }
   return result;
}

static PFN_vkVoidFunction VKAPI_CALL zink_gipa(VkInstance instance, const char *name) {
   PFN_vkVoidFunction fn = real_gipa(instance, name);
   if (!fn)
      return NULL;
   if (!strcmp(name, "vkGetInstanceProcAddr"))
      return (PFN_vkVoidFunction)zink_gipa;
   if (!strcmp(name, "vkCreateInstance")) {
      real_create_instance = (PFN_vkCreateInstance)fn;
      return (PFN_vkVoidFunction)quirk_create_instance;
   }
   if (!strcmp(name, "vkEnumeratePhysicalDevices")) {
      real_enumerate_physical_devices = (PFN_vkEnumeratePhysicalDevices)fn;
      return (PFN_vkVoidFunction)quirk_enumerate_physical_devices;
   }
   return fn;
}

int ttc_virgl_vulkan(void) {
   // As the Venus renderer's libraryPaths.
   static const char *const loaders[] = {"libvulkan.so.1", "libvulkan.so"};
   void *loader = NULL;
   for (unsigned i = 0; i < sizeof(loaders) / sizeof(loaders[0]) && !loader; i++)
      loader = dlopen(loaders[i], RTLD_NOW | RTLD_LOCAL);
   if (!loader)
      return -1;
   real_gipa = (PFN_vkGetInstanceProcAddr)dlsym(loader, "vkGetInstanceProcAddr");
   void *gdpa = dlsym(loader, "vkGetDeviceProcAddr");
   if (!real_gipa || !gdpa) {
      dlclose(loader);
      return -1;
   }
   zink_vk_get_instance_proc_addr = (void *)zink_gipa;
   zink_vk_get_device_proc_addr = gdpa;
   return 0;
}
