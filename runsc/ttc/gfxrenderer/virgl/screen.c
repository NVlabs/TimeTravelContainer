// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

// The process's Zink screen, on the platform's Vulkan (screen_darwin.c,
// screen_linux.c), and the capability sets that virgl guests read of it.

#include <stdio.h>
#include <string.h>

#include "pipe/p_defines.h"
#include "pipe/p_screen.h"
#include "pipe/p_state.h"
#include "util/format/u_format.h"
#include "virgl.h"
#include "virgl_hw.h"
#include "virgl_int.h"
#include "zink_public.h"

struct pipe_screen *ttc_virgl_screen;

bool ttc_virgl_xfb_emulated;
uint32_t ttc_virgl_xfb_ubo, ttc_virgl_xfb_ssbo;

int ttc_virgl_init(void) {
   if (ttc_virgl_vulkan() != 0)
      return -1;
   ttc_virgl_screen = zink_create_screen(NULL, NULL);
   if (!ttc_virgl_screen)
      return -1;
   // MoltenVK has no transform feedback, which OpenGL ES 3.0 requires, and
   // Android's HWUI with it: the renderer emulates it, in the slots of the
   // vertex shader's state that it keeps for it. It does where Zink has it
   // too, as on Linux, so that guests capture alike everywhere, and forks
   // keep what the decoder counted of a capture (context_unsafe.go).
   const struct pipe_shader_caps *vs = &ttc_virgl_screen->shader_caps[MESA_SHADER_VERTEX];
   ttc_virgl_xfb_emulated = vs->max_const_buffers >= 2 && vs->max_shader_buffers >= 8;
   ttc_virgl_xfb_ubo = vs->max_const_buffers - 1;
   ttc_virgl_xfb_ssbo = vs->max_shader_buffers - PIPE_MAX_SO_BUFFERS;
   return 0;
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
   v1->bset.streamout_pause_resume = c->stream_output_pause_resume || ttc_virgl_xfb_emulated;
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
   v1->max_streamout_buffers =
      ttc_virgl_xfb_emulated ? PIPE_MAX_SO_BUFFERS : c->max_stream_output_buffers;
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
   // Uniform buffers besides the default one, which is buffer 0, less the
   // renderer's.
   v1->max_uniform_blocks = fs->max_const_buffers - 1 - ttc_virgl_xfb_emulated;
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
   // tgsi_to_nir's vertex shaders' inputs are VERT_ATTRIB_GENERIC0..15.
   v2->max_vertex_attribs = MIN2(vs->max_inputs, 16);
   v2->max_shader_patch_varyings = c->max_shader_patch_varyings;
   v2->min_texel_offset = c->min_texel_offset;
   v2->max_texel_offset = c->max_texel_offset;
   v2->min_texture_gather_offset = c->min_texture_gather_offset;
   v2->max_texture_gather_offset = c->max_texture_gather_offset;
   v2->texture_buffer_offset_alignment = c->texture_buffer_offset_alignment;
   v2->uniform_buffer_offset_alignment = c->constant_buffer_offset_alignment;
   v2->shader_buffer_offset_alignment = c->shader_buffer_offset_alignment;

   // Of what Zink has, the renderer offers none of the features of whose
   // shaders tgsi_to_nir translates none, and that no version of OpenGL
   // requires (tgsi_check.c): no shader clock.
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
   // The emulation captures the vertex shader's stream 0 only.
   if (c->max_vertex_streams > 1 && !ttc_virgl_xfb_emulated)
      bits |= VIRGL_CAP_TRANSFORM_FEEDBACK3;
   if (c->fbfetch)
      bits |= VIRGL_CAP_TGSI_FBFETCH;
   v2->capability_bits = bits;

   v2->max_vertex_attrib_stride = c->max_vertex_attrib_stride;
   v2->max_shader_buffer_frag_compute = fs->max_shader_buffers;
   v2->max_shader_buffer_other_stages =
      vs->max_shader_buffers - (ttc_virgl_xfb_emulated ? PIPE_MAX_SO_BUFFERS : 0);
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
   if (ttc_virgl_xfb_emulated)
      v2->max_shader_storage_blocks[0] -= PIPE_MAX_SO_BUFFERS;
   v2->max_combined_atomic_counters = c->max_combined_hw_atomic_counters;
   v2->max_combined_atomic_counter_buffers = c->max_combined_hw_atomic_counter_buffers;
   // virglrenderer's: the guest relies on the host's behaviors of every
   // version up to it, which this host has.
   v2->host_feature_check_version = 23;

   // As above, no vertex shaders' viewport indexes, nor advanced blend
   // equations, which the blend states that the renderer decodes have not,
   // and which guests blend in their shaders with framebuffer fetch.
   uint32_t bits2 = 0;
   if (c->surface_sample_count)
      bits2 |= VIRGL_CAP_V2_IMPLICIT_MSAA;
   if (c->texture_shadow_lod)
      bits2 |= VIRGL_CAP_V2_TEXTURE_SHADOW_LOD;
   if (c->vs_layer_viewport)
      bits2 |= VIRGL_CAP_V2_VS_VERTEX_LAYER;
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
