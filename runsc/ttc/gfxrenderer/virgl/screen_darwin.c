// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The process's Zink screen, on the MoltenVK that the renderer links, and
// the capability sets that virgl guests read of it.

#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "pipe/p_defines.h"
#include "pipe/p_screen.h"
#include "util/format/u_format.h"
#include "virgl.h"
#include "virgl_hw.h"
#include "virgl_int.h"
#include "zink_public.h"

struct pipe_screen *ttc_virgl_screen;

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

int ttc_virgl_init(void) {
   zink_vk_get_instance_proc_addr = (void *)zink_gipa;
   zink_vk_get_device_proc_addr = (void *)zink_gdpa;
   ttc_virgl_screen = zink_create_screen(NULL, NULL);
   return ttc_virgl_screen ? 0 : -1;
}

static void set_format(struct virgl_supported_format_mask *mask, uint32_t format) {
   mask->bitmask[format / 32] |= 1u << (format % 32);
}

static bool supported(enum pipe_format format, enum pipe_texture_target target, unsigned samples,
                      unsigned bind) {
   struct pipe_screen *s = ttc_virgl_screen;
   return s->is_format_supported(s, format, target, samples, samples, bind);
}

void ttc_virgl_fill_caps(uint32_t set, void *out, uint32_t size) {
   struct pipe_screen *s = ttc_virgl_screen;
   const struct pipe_caps *c = &s->caps;
   const struct pipe_shader_caps *fs = &s->shader_caps[MESA_SHADER_FRAGMENT];
   const struct pipe_shader_caps *vs = &s->shader_caps[MESA_SHADER_VERTEX];
   union virgl_caps caps;
   memset(&caps, 0, sizeof(caps));
   struct virgl_caps_v1 *v1 = &caps.v1;
   struct virgl_caps_v2 *v2 = &caps.v2;

   v1->max_version = set == 1 ? 1 : 2;
   for (uint32_t vf = 1; vf < VIRGL_FORMAT_MAX; vf++) {
      enum pipe_format pf = ttc_virgl_pipe_format(vf);
      if (pf == PIPE_FORMAT_NONE)
         continue;
      if (supported(pf, PIPE_TEXTURE_2D, 0, PIPE_BIND_SAMPLER_VIEW)) {
         set_format(&v1->sampler, vf);
         set_format(&v2->supported_readback_formats, vf);
      }
      if (supported(pf, PIPE_TEXTURE_2D, 0, PIPE_BIND_RENDER_TARGET)) {
         set_format(&v1->render, vf);
         if (supported(pf, PIPE_TEXTURE_2D, 4, PIPE_BIND_RENDER_TARGET))
            set_format(&v2->supported_multisample_formats, vf);
      }
      if (supported(pf, PIPE_TEXTURE_2D, 0, PIPE_BIND_DEPTH_STENCIL))
         set_format(&v1->depthstencil, vf);
      if (supported(pf, PIPE_BUFFER, 0, PIPE_BIND_VERTEX_BUFFER))
         set_format(&v1->vertexbuffer, vf);
   }
   static const uint32_t scanout[] = {
      VIRGL_FORMAT_B8G8R8A8_UNORM, VIRGL_FORMAT_B8G8R8X8_UNORM,
      VIRGL_FORMAT_R8G8B8A8_UNORM, VIRGL_FORMAT_R8G8B8X8_UNORM,
      VIRGL_FORMAT_B5G6R5_UNORM,   VIRGL_FORMAT_R10G10B10A2_UNORM,
   };
   for (unsigned i = 0; i < sizeof(scanout) / sizeof(scanout[0]); i++) {
      if (v1->render.bitmask[scanout[i] / 32] & (1u << (scanout[i] % 32)))
         set_format(&v2->scanout, scanout[i]);
   }

   v1->bset.indep_blend_enable = c->indep_blend_enable;
   v1->bset.indep_blend_func = c->indep_blend_func;
   v1->bset.cube_map_array = c->cube_map_array;
   v1->bset.shader_stencil_export = c->shader_stencil_export;
   v1->bset.conditional_render = c->conditional_render;
   v1->bset.start_instance = c->start_instance;
   v1->bset.primitive_restart = c->primitive_restart;
   v1->bset.blend_eq_sep = 1;
   v1->bset.instanceid = 1;
   v1->bset.vertex_element_instance_divisor = 1;
   v1->bset.seamless_cube_map = c->seamless_cube_map;
   v1->bset.occlusion_query = c->occlusion_query;
   v1->bset.timer_query = c->query_timestamp && c->query_time_elapsed;
   v1->bset.streamout_pause_resume = c->stream_output_pause_resume;
   v1->bset.texture_multisample = c->texture_multisample;
   v1->bset.fragment_coord_conventions = c->fs_coord_origin_lower_left;
   v1->bset.depth_clip_disable = c->depth_clip_disable;
   v1->bset.seamless_cube_map_per_texture = c->seamless_cube_map_per_texture;
   v1->bset.ubo = 1;
   v1->bset.color_clamping = c->vertex_color_clamped || c->fragment_color_clamped;
   v1->bset.poly_stipple = 1;
   v1->bset.mirror_clamp = c->texture_mirror_clamp;
   v1->bset.texture_query_lod = c->texture_query_lod;
   v1->bset.has_fp64 = c->doubles;
   v1->bset.has_tessellation_shaders = s->shader_caps[MESA_SHADER_TESS_CTRL].max_instructions > 0;
   v1->bset.has_indirect_draw = c->draw_indirect;
   v1->bset.has_sample_shading = c->sample_shading;
   v1->bset.has_cull = c->cull_distance;
   v1->bset.conditional_render_inverted = c->conditional_render_inverted;
   v1->bset.derivative_control = c->fs_fine_derivative;
   v1->bset.polygon_offset_clamp = c->polygon_offset_clamp;
   v1->bset.transform_feedback_overflow_query = c->query_so_overflow;

   v1->glsl_level = c->glsl_feature_level;
   v1->max_texture_array_layers = c->max_texture_array_layers;
   v1->max_streamout_buffers = c->max_stream_output_buffers;
   v1->max_dual_source_render_targets = c->max_dual_source_render_targets;
   v1->max_render_targets = c->max_render_targets;
   v1->max_samples = 1;
   for (unsigned samples = 16; samples > 1; samples /= 2) {
      if (supported(PIPE_FORMAT_R8G8B8A8_UNORM, PIPE_TEXTURE_2D, samples,
                    PIPE_BIND_RENDER_TARGET)) {
         v1->max_samples = samples;
         break;
      }
   }
   v1->prim_mask = c->supported_prim_modes;
   v1->max_tbo_size = c->max_texel_buffer_elements;
   v1->max_uniform_blocks = fs->max_const_buffers;
   v1->max_viewports = c->max_viewports;
   v1->max_texture_gather_components = c->max_texture_gather_components;
   if (set == 1) {
      memcpy(out, &caps, size < sizeof(caps.v1) ? size : sizeof(caps.v1));
      return;
   }

   v2->min_aliased_point_size = c->min_point_size;
   v2->max_aliased_point_size = c->max_point_size;
   v2->min_smooth_point_size = c->min_point_size_aa;
   v2->max_smooth_point_size = c->max_point_size_aa;
   v2->min_aliased_line_width = c->min_line_width;
   v2->max_aliased_line_width = c->max_line_width;
   v2->min_smooth_line_width = c->min_line_width_aa;
   v2->max_smooth_line_width = c->max_line_width_aa;
   v2->max_texture_lod_bias = c->max_texture_lod_bias;
   v2->max_geom_output_vertices = c->max_geometry_output_vertices;
   v2->max_geom_total_output_components = c->max_geometry_total_output_components;
   v2->max_vertex_outputs = vs->max_outputs;
   v2->max_vertex_attribs = vs->max_inputs;
   v2->max_shader_patch_varyings = c->max_shader_patch_varyings;
   v2->min_texel_offset = c->min_texel_offset;
   v2->max_texel_offset = c->max_texel_offset;
   v2->min_texture_gather_offset = c->min_texture_gather_offset;
   v2->max_texture_gather_offset = c->max_texture_gather_offset;
   v2->texture_buffer_offset_alignment = c->texture_buffer_offset_alignment;
   v2->uniform_buffer_offset_alignment = c->constant_buffer_offset_alignment;
   v2->shader_buffer_offset_alignment = c->shader_buffer_offset_alignment;

   uint32_t bits = VIRGL_CAP_TGSI_INVARIANT | VIRGL_CAP_SET_MIN_SAMPLES | VIRGL_CAP_TGSI_PRECISE |
                   VIRGL_CAP_MEMORY_BARRIER | VIRGL_CAP_CLEAR_TEXTURE | VIRGL_CAP_BIND_COMMAND_ARGS;
   if (c->texture_query_samples)
      bits |= VIRGL_CAP_TXQS;
   if (c->compute)
      bits |= VIRGL_CAP_COMPUTE_SHADER;
   if (c->framebuffer_no_attachment)
      bits |= VIRGL_CAP_FB_NO_ATTACH;
   if (c->sampler_view_target)
      bits |= VIRGL_CAP_TEXTURE_VIEW;
   if (c->copy_between_compressed_and_plain_formats)
      bits |= VIRGL_CAP_COPY_IMAGE;
   if (c->robust_buffer_access_behavior)
      bits |= VIRGL_CAP_ROBUST_BUFFER_ACCESS;
   if (c->texture_barrier)
      bits |= VIRGL_CAP_TEXTURE_BARRIER;
   if (c->shader_array_components)
      bits |= VIRGL_CAP_TGSI_COMPONENTS;
   if (c->dest_surface_srgb_control)
      bits |= VIRGL_CAP_SRGB_WRITE_CONTROL;
   if (c->mixed_colorbuffer_formats)
      bits |= VIRGL_CAP_FBO_MIXED_COLOR_FORMATS;
   if (c->clip_halfz)
      bits |= VIRGL_CAP_CLIP_HALFZ;
   if (c->query_buffer_object)
      bits |= VIRGL_CAP_QBO;
   if (c->multi_draw_indirect)
      bits |= VIRGL_CAP_MULTI_DRAW_INDIRECT;
   if (c->multi_draw_indirect_params)
      bits |= VIRGL_CAP_INDIRECT_PARAMS;
   if (c->max_vertex_streams > 1)
      bits |= VIRGL_CAP_TRANSFORM_FEEDBACK3;
   if (c->fbfetch)
      bits |= VIRGL_CAP_TGSI_FBFETCH;
   if (c->shader_clock)
      bits |= VIRGL_CAP_SHADER_CLOCK;
   v2->capability_bits = bits;

   v2->max_vertex_attrib_stride = c->max_vertex_attrib_stride;
   v2->max_shader_buffer_frag_compute = fs->max_shader_buffers;
   v2->max_shader_buffer_other_stages = vs->max_shader_buffers;
   v2->max_shader_image_frag_compute = fs->max_shader_images;
   v2->max_shader_image_other_stages = vs->max_shader_images;
   // Gallium has no cap of multisampled images: offer none.
   v2->max_image_samples = 0;
   if (c->compute) {
      const struct pipe_compute_caps *cc = &s->compute_caps;
      v2->max_compute_work_group_invocations = cc->max_threads_per_block;
      v2->max_compute_shared_memory_size = cc->max_local_size;
      for (unsigned i = 0; i < 3; i++) {
         v2->max_compute_grid_size[i] = cc->max_grid_size[i];
         v2->max_compute_block_size[i] = cc->max_block_size[i];
      }
   }
   v2->max_texture_2d_size = c->max_texture_2d_size;
   v2->max_texture_3d_size = 1u << (c->max_texture_3d_levels - 1);
   v2->max_texture_cube_size = 1u << (c->max_texture_cube_levels - 1);
   v2->max_combined_shader_buffers = c->max_combined_shader_buffers;
   for (uint32_t i = 0; i < 6; i++) {
      const struct pipe_shader_caps *sc = &s->shader_caps[ttc_virgl_stage(i)];
      v2->max_atomic_counters[i] = sc->max_hw_atomic_counters;
      v2->max_atomic_counter_buffers[i] = sc->max_hw_atomic_counter_buffers;
      v2->max_const_buffer_size[i] = sc->max_const_buffer0_size;
      v2->max_shader_storage_blocks[i] = sc->max_shader_buffers;
   }
   v2->max_combined_atomic_counters = c->max_combined_hw_atomic_counters;
   v2->max_combined_atomic_counter_buffers = c->max_combined_hw_atomic_counter_buffers;
   // virglrenderer's: the guest relies on the host's behaviors of every
   // version up to it, which this host has.
   v2->host_feature_check_version = 23;

   uint32_t bits2 = 0;
   if (c->blend_equation_advanced)
      bits2 |= VIRGL_CAP_V2_BLEND_EQUATION;
   if (c->surface_sample_count)
      bits2 |= VIRGL_CAP_V2_IMPLICIT_MSAA;
   if (c->texture_shadow_lod)
      bits2 |= VIRGL_CAP_V2_TEXTURE_SHADOW_LOD;
   if (c->vs_layer_viewport)
      bits2 |= VIRGL_CAP_V2_VS_VERTEX_LAYER | VIRGL_CAP_V2_VS_VIEWPORT_INDEX;
   if (c->query_pipeline_statistics)
      bits2 |= VIRGL_CAP_V2_PIPELINE_STATISTICS_QUERY;
   if (c->draw_parameters)
      bits2 |= VIRGL_CAP_V2_DRAW_PARAMETERS;
   if (c->shader_group_vote)
      bits2 |= VIRGL_CAP_V2_GROUP_VOTE;
   if (c->texture_mirror_clamp_to_edge)
      bits2 |= VIRGL_CAP_V2_MIRROR_CLAMP_TO_EDGE;
   if (c->texture_mirror_clamp)
      bits2 |= VIRGL_CAP_V2_MIRROR_CLAMP;
   v2->capability_bits_v2 = bits2;

   snprintf(v2->renderer, sizeof(v2->renderer), "%s", s->get_name(s));
   v2->max_anisotropy = c->max_texture_anisotropy;
   v2->max_texture_samplers = fs->max_texture_samplers;
   v2->max_uniform_block_size = c->max_constant_buffer_size;
   v2->max_tcs_outputs = s->shader_caps[MESA_SHADER_TESS_CTRL].max_outputs;
   v2->max_tes_outputs = s->shader_caps[MESA_SHADER_TESS_EVAL].max_outputs;
   memcpy(out, &caps, size < sizeof(caps.v2) ? size : sizeof(caps.v2));
}
