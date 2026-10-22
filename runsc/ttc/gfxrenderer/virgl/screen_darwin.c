// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The renderer's Vulkan for Zink on macOS: the MoltenVK that the renderer
// links, with what Zink needs of Vulkan that MoltenVK lacks.

#include <pthread.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "virgl_int.h"
#include "zink_public.h"

// Zink requires robustness2's nullDescriptor, which MoltenVK lacks; MoltenVK
// binds nil Metal resources for null descriptors, as TTC's Venus renderer
// relies on for guests' Zink (venus/quirks_unsafe.go). Zink's Vulkan
// therefore reports the feature, and keeps it from vkCreateDevice.
static PFN_vkGetPhysicalDeviceFeatures2 real_features2;
static PFN_vkCreateDevice real_create_device;

static VkBaseOutStructure *find_struct(const void *chain, VkStructureType type) {
   for (VkBaseOutStructure *s = (VkBaseOutStructure *)chain; s; s = s->pNext) {
      if (s->sType == type)
         return s;
   }
   return NULL;
}

static void VKAPI_CALL quirk_features2(VkPhysicalDevice pd, VkPhysicalDeviceFeatures2 *features) {
   real_features2(pd, features);
   VkPhysicalDeviceRobustness2FeaturesKHR *rb2 = (void *)find_struct(
      features->pNext, VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ROBUSTNESS_2_FEATURES_KHR);
   if (rb2)
      rb2->nullDescriptor = VK_TRUE;
}

static VkResult VKAPI_CALL quirk_create_device(VkPhysicalDevice pd, const VkDeviceCreateInfo *info,
                                               const VkAllocationCallbacks *alloc,
                                               VkDevice *device) {
   VkPhysicalDeviceRobustness2FeaturesKHR *rb2 = (void *)find_struct(
      info->pNext, VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ROBUSTNESS_2_FEATURES_KHR);
   VkBool32 saved = VK_FALSE;
   if (rb2) {
      saved = rb2->nullDescriptor;
      rb2->nullDescriptor = VK_FALSE;
   }
   VkResult result = real_create_device(pd, info, alloc, device);
   if (rb2)
      rb2->nullDescriptor = saved;
   return result;
}

// Zink names SPIR-V's variables by NIR's, which are TGSI's for virgl's
// shaders: "sampler", for one. MoltenVK's SPIRV-Cross names Metal's
// variables by them, and Metal fails to compile a texture named sampler,
// which is a type of Metal's. Zink's Vulkan drops the names, which are for
// debuggers only, so that SPIRV-Cross makes up its own.
static PFN_vkCreateShaderModule real_create_shader_module;

enum { SPIRV_MAGIC = 0x07230203, SPIRV_HEADER_WORDS = 5, OP_NAME = 5, OP_MEMBER_NAME = 6 };

static VkResult VKAPI_CALL quirk_create_shader_module(VkDevice device,
                                                      const VkShaderModuleCreateInfo *info,
                                                      const VkAllocationCallbacks *alloc,
                                                      VkShaderModule *module) {
   const uint32_t *code = info->pCode;
   size_t n = info->codeSize / 4;
   if (n < SPIRV_HEADER_WORDS || code[0] != SPIRV_MAGIC)
      return real_create_shader_module(device, info, alloc, module);
   uint32_t *out = malloc(n * 4);
   if (!out)
      return VK_ERROR_OUT_OF_HOST_MEMORY;
   memcpy(out, code, SPIRV_HEADER_WORDS * 4);
   size_t m = SPIRV_HEADER_WORDS;
   for (size_t i = SPIRV_HEADER_WORDS; i < n;) {
      uint32_t words = code[i] >> 16, op = code[i] & 0xffff;
      if (words == 0 || words > n - i) {
         // Not SPIR-V that Zink wrote: let Vulkan judge it.
         free(out);
         return real_create_shader_module(device, info, alloc, module);
      }
      if (op != OP_NAME && op != OP_MEMBER_NAME) {
         memcpy(out + m, code + i, words * 4);
         m += words;
      }
      i += words;
   }
   VkShaderModuleCreateInfo stripped = *info;
   stripped.pCode = out;
   stripped.codeSize = m * 4;
   VkResult result = real_create_shader_module(device, &stripped, alloc, module);
   free(out);
   return result;
}

// A pipeline that MoltenVK fails to create, as of a shader that Metal fails
// to compile, Zink binds as VK_NULL_HANDLE, which MoltenVK dereferences.
// Zink's Vulkan binds no such pipeline, and drops the draws and dispatches
// of a command buffer whose bind point has none, as virglrenderer drops
// draws that it cannot draw. Such command buffers are few: the draws of
// others check a count, and take no lock.
struct dropping {
   VkCommandBuffer cb;
   // Whether the graphics and the compute bind points have no pipeline.
   bool none[2];
};

static pthread_mutex_t dropping_mu = PTHREAD_MUTEX_INITIALIZER;
static struct dropping *droppings;
static unsigned num_droppings, cap_droppings;
static atomic_uint any_droppings;

static struct dropping *find_dropping_locked(VkCommandBuffer cb) {
   for (unsigned i = 0; i < num_droppings; i++) {
      if (droppings[i].cb == cb)
         return &droppings[i];
   }
   return NULL;
}

static void remove_dropping_locked(struct dropping *d) {
   *d = droppings[--num_droppings];
   atomic_store(&any_droppings, num_droppings);
}

static void forget_droppings(const VkCommandBuffer *cbs, uint32_t n) {
   if (!atomic_load_explicit(&any_droppings, memory_order_relaxed))
      return;
   pthread_mutex_lock(&dropping_mu);
   for (uint32_t i = 0; i < n; i++) {
      struct dropping *d = find_dropping_locked(cbs[i]);
      if (d)
         remove_dropping_locked(d);
   }
   pthread_mutex_unlock(&dropping_mu);
}

// dropped reports whether cb has no pipeline at bind point bp.
static bool dropped(VkCommandBuffer cb, unsigned bp) {
   if (!atomic_load_explicit(&any_droppings, memory_order_relaxed))
      return false;
   pthread_mutex_lock(&dropping_mu);
   struct dropping *d = find_dropping_locked(cb);
   bool none = d && d->none[bp];
   pthread_mutex_unlock(&dropping_mu);
   return none;
}

static PFN_vkCmdBindPipeline real_bind_pipeline;
static PFN_vkBeginCommandBuffer real_begin_command_buffer;
static PFN_vkFreeCommandBuffers real_free_command_buffers;

static void VKAPI_CALL quirk_bind_pipeline(VkCommandBuffer cb, VkPipelineBindPoint bp,
                                           VkPipeline pipeline) {
   if (bp > VK_PIPELINE_BIND_POINT_COMPUTE) {
      real_bind_pipeline(cb, bp, pipeline);
      return;
   }
   if (pipeline != VK_NULL_HANDLE && !atomic_load_explicit(&any_droppings, memory_order_relaxed)) {
      real_bind_pipeline(cb, bp, pipeline);
      return;
   }
   pthread_mutex_lock(&dropping_mu);
   struct dropping *d = find_dropping_locked(cb);
   if (pipeline == VK_NULL_HANDLE) {
      if (!d) {
         if (num_droppings == cap_droppings) {
            unsigned cap = cap_droppings ? 2 * cap_droppings : 8;
            struct dropping *grown = realloc(droppings, cap * sizeof(*grown));
            if (!grown) {
               // Rather crash here than in Metal.
               pthread_mutex_unlock(&dropping_mu);
               abort();
            }
            droppings = grown;
            cap_droppings = cap;
         }
         d = &droppings[num_droppings++];
         *d = (struct dropping){.cb = cb};
         atomic_store(&any_droppings, num_droppings);
      }
      d->none[bp] = true;
   } else if (d) {
      d->none[bp] = false;
      if (!d->none[0] && !d->none[1])
         remove_dropping_locked(d);
   }
   pthread_mutex_unlock(&dropping_mu);
   if (pipeline != VK_NULL_HANDLE)
      real_bind_pipeline(cb, bp, pipeline);
}

static VkResult VKAPI_CALL quirk_begin_command_buffer(VkCommandBuffer cb,
                                                      const VkCommandBufferBeginInfo *info) {
   forget_droppings(&cb, 1);
   return real_begin_command_buffer(cb, info);
}

static void VKAPI_CALL quirk_free_command_buffers(VkDevice device, VkCommandPool pool, uint32_t n,
                                                  const VkCommandBuffer *cbs) {
   forget_droppings(cbs, n);
   real_free_command_buffers(device, pool, n, cbs);
}

// DROPPABLE defines a wrapper of command name, of bind point bp, with
// parameters params, which it passes as args.
#define DROPPABLE(name, bp, params, args)                                                      \
   static PFN_vk##name real_##name;                                                            \
   static void VKAPI_CALL quirk_##name params {                                                \
      if (!dropped(cb, bp))                                                                    \
         real_##name args;                                                                     \
   }

DROPPABLE(CmdDraw, 0,
          (VkCommandBuffer cb, uint32_t vertices, uint32_t instances, uint32_t first_vertex,
           uint32_t first_instance),
          (cb, vertices, instances, first_vertex, first_instance))
DROPPABLE(CmdDrawIndexed, 0,
          (VkCommandBuffer cb, uint32_t indices, uint32_t instances, uint32_t first_index,
           int32_t vertex_offset, uint32_t first_instance),
          (cb, indices, instances, first_index, vertex_offset, first_instance))
DROPPABLE(CmdDrawIndirect, 0,
          (VkCommandBuffer cb, VkBuffer buffer, VkDeviceSize offset, uint32_t count,
           uint32_t stride),
          (cb, buffer, offset, count, stride))
DROPPABLE(CmdDrawIndexedIndirect, 0,
          (VkCommandBuffer cb, VkBuffer buffer, VkDeviceSize offset, uint32_t count,
           uint32_t stride),
          (cb, buffer, offset, count, stride))
DROPPABLE(CmdDrawIndirectCount, 0,
          (VkCommandBuffer cb, VkBuffer buffer, VkDeviceSize offset, VkBuffer count_buffer,
           VkDeviceSize count_offset, uint32_t max_count, uint32_t stride),
          (cb, buffer, offset, count_buffer, count_offset, max_count, stride))
DROPPABLE(CmdDrawIndexedIndirectCount, 0,
          (VkCommandBuffer cb, VkBuffer buffer, VkDeviceSize offset, VkBuffer count_buffer,
           VkDeviceSize count_offset, uint32_t max_count, uint32_t stride),
          (cb, buffer, offset, count_buffer, count_offset, max_count, stride))
DROPPABLE(CmdDrawMultiEXT, 0,
          (VkCommandBuffer cb, uint32_t n, const VkMultiDrawInfoEXT *draws, uint32_t instances,
           uint32_t first_instance, uint32_t stride),
          (cb, n, draws, instances, first_instance, stride))
DROPPABLE(CmdDrawMultiIndexedEXT, 0,
          (VkCommandBuffer cb, uint32_t n, const VkMultiDrawIndexedInfoEXT *draws,
           uint32_t instances, uint32_t first_instance, uint32_t stride,
           const int32_t *vertex_offset),
          (cb, n, draws, instances, first_instance, stride, vertex_offset))
DROPPABLE(CmdDispatch, 1, (VkCommandBuffer cb, uint32_t x, uint32_t y, uint32_t z), (cb, x, y, z))
DROPPABLE(CmdDispatchIndirect, 1, (VkCommandBuffer cb, VkBuffer buffer, VkDeviceSize offset),
          (cb, buffer, offset))
DROPPABLE(CmdDispatchBase, 1,
          (VkCommandBuffer cb, uint32_t bx, uint32_t by, uint32_t bz, uint32_t x, uint32_t y,
           uint32_t z),
          (cb, bx, by, bz, x, y, z))

static PFN_vkVoidFunction quirk(const char *name, PFN_vkVoidFunction fn) {
   if (!fn)
      return NULL;
   if (!strcmp(name, "vkCreateShaderModule")) {
      real_create_shader_module = (PFN_vkCreateShaderModule)fn;
      return (PFN_vkVoidFunction)quirk_create_shader_module;
   }
#define WRAP(fn_name, real, wrapper)                                                           \
   if (!strcmp(name, fn_name)) {                                                               \
      real = (void *)fn;                                                                       \
      return (PFN_vkVoidFunction)wrapper;                                                      \
   }
   WRAP("vkCmdBindPipeline", real_bind_pipeline, quirk_bind_pipeline)
   WRAP("vkBeginCommandBuffer", real_begin_command_buffer, quirk_begin_command_buffer)
   WRAP("vkFreeCommandBuffers", real_free_command_buffers, quirk_free_command_buffers)
#define WRAP_DROPPABLE(cmd) WRAP("vk" #cmd, real_##cmd, quirk_##cmd)
   WRAP_DROPPABLE(CmdDraw)
   WRAP_DROPPABLE(CmdDrawIndexed)
   WRAP_DROPPABLE(CmdDrawIndirect)
   WRAP_DROPPABLE(CmdDrawIndexedIndirect)
   WRAP_DROPPABLE(CmdDrawIndirectCount)
   WRAP_DROPPABLE(CmdDrawIndexedIndirectCount)
   WRAP_DROPPABLE(CmdDrawMultiEXT)
   WRAP_DROPPABLE(CmdDrawMultiIndexedEXT)
   WRAP_DROPPABLE(CmdDispatch)
   WRAP_DROPPABLE(CmdDispatchIndirect)
   WRAP_DROPPABLE(CmdDispatchBase)
   WRAP("vkCmdDrawIndirectCountKHR", real_CmdDrawIndirectCount, quirk_CmdDrawIndirectCount)
   WRAP("vkCmdDrawIndexedIndirectCountKHR", real_CmdDrawIndexedIndirectCount,
        quirk_CmdDrawIndexedIndirectCount)
   WRAP("vkCmdDispatchBaseKHR", real_CmdDispatchBase, quirk_CmdDispatchBase)
#undef WRAP_DROPPABLE
#undef WRAP
   return fn;
}

// MoltenVK warns of each of Zink's pipelines, which disable primitive
// restart, which Metal cannot: Zink's instance logs MoltenVK's errors only,
// as compiling Metal's shaders fails with.
static PFN_vkCreateInstance real_create_instance;

static VkResult VKAPI_CALL quirk_create_instance(const VkInstanceCreateInfo *info,
                                                 const VkAllocationCallbacks *alloc,
                                                 VkInstance *instance) {
   static const int32_t log_errors = 1; // MVK_CONFIG_LOG_LEVEL_ERROR
   const VkLayerSettingEXT setting = {
      .pLayerName = "MoltenVK",
      .pSettingName = "MVK_CONFIG_LOG_LEVEL",
      .type = VK_LAYER_SETTING_TYPE_INT32_EXT,
      .valueCount = 1,
      .pValues = &log_errors,
   };
   const VkLayerSettingsCreateInfoEXT settings = {
      .sType = VK_STRUCTURE_TYPE_LAYER_SETTINGS_CREATE_INFO_EXT,
      .pNext = info->pNext,
      .settingCount = 1,
      .pSettings = &setting,
   };
   VkInstanceCreateInfo quiet = *info;
   quiet.pNext = &settings;
   return real_create_instance(&quiet, alloc, instance);
}

static PFN_vkVoidFunction VKAPI_CALL zink_gdpa(VkDevice device, const char *name) {
   return quirk(name, vkGetDeviceProcAddr(device, name));
}

static PFN_vkVoidFunction VKAPI_CALL zink_gipa(VkInstance instance, const char *name) {
   PFN_vkVoidFunction fn = vkGetInstanceProcAddr(instance, name);
   if (!fn)
      return NULL;
   if (!strcmp(name, "vkGetInstanceProcAddr"))
      return (PFN_vkVoidFunction)zink_gipa;
   if (!strcmp(name, "vkGetDeviceProcAddr"))
      return (PFN_vkVoidFunction)zink_gdpa;
   if (!strcmp(name, "vkCreateInstance")) {
      real_create_instance = (PFN_vkCreateInstance)fn;
      return (PFN_vkVoidFunction)quirk_create_instance;
   }
   if (!strcmp(name, "vkGetPhysicalDeviceFeatures2") ||
       !strcmp(name, "vkGetPhysicalDeviceFeatures2KHR")) {
      real_features2 = (PFN_vkGetPhysicalDeviceFeatures2)fn;
      return (PFN_vkVoidFunction)quirk_features2;
   }
   if (!strcmp(name, "vkCreateDevice")) {
      real_create_device = (PFN_vkCreateDevice)fn;
      return (PFN_vkVoidFunction)quirk_create_device;
   }
   return quirk(name, fn);
}

int ttc_virgl_vulkan(void) {
   zink_vk_get_instance_proc_addr = (void *)zink_gipa;
   zink_vk_get_device_proc_addr = (void *)zink_gdpa;
   return 0;
}
