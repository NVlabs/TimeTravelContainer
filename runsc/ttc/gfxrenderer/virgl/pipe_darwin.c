// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// virgl's commands as Gallium's calls. The Go decoder checked each command's
// length and resolved its handles; these unpack its fields, as
// virglrenderer's vrend_decode.c does, into Gallium's state. d is a
// command's payload: virgl_protocol.h's offsets less one, its header's.

#include <errno.h>
#include <stdio.h>
#include <string.h>

#include "compiler/glsl_types.h"
#include "compiler/nir/nir.h"
#include "nir/tgsi_to_nir.h"
#include "pipe/p_context.h"
#include "pipe/p_defines.h"
#include "pipe/p_screen.h"
#include "pipe/p_shader_tokens.h"
#include "pipe/p_state.h"
#include "tgsi/tgsi_text.h"
#include "util/format/u_format.h"
#include "util/u_inlines.h"
#include "util/u_math.h"
#include "util/u_memory.h"
#include "util/u_surface.h"
#include "virgl.h"
#include "virgl_hw.h"
#include "virgl_int.h"
#include "virgl_protocol.h"

// Gallium's binding flags of virgl's, which are Gallium's of 2015. A guest's
// sharing and display flags concern its memory of a resource, which the
// renderer has, and not the host's resource.
static unsigned pipe_bind(uint32_t bind) {
   unsigned b = 0;
   if (bind & VIRGL_BIND_DEPTH_STENCIL)
      b |= PIPE_BIND_DEPTH_STENCIL;
   if (bind & VIRGL_BIND_RENDER_TARGET)
      b |= PIPE_BIND_RENDER_TARGET;
   if (bind & VIRGL_BIND_SAMPLER_VIEW)
      b |= PIPE_BIND_SAMPLER_VIEW;
   if (bind & VIRGL_BIND_VERTEX_BUFFER)
      b |= PIPE_BIND_VERTEX_BUFFER;
   if (bind & VIRGL_BIND_INDEX_BUFFER)
      b |= PIPE_BIND_INDEX_BUFFER;
   if (bind & VIRGL_BIND_CONSTANT_BUFFER)
      b |= PIPE_BIND_CONSTANT_BUFFER;
   if (bind & VIRGL_BIND_COMMAND_ARGS)
      b |= PIPE_BIND_COMMAND_ARGS_BUFFER;
   if (bind & VIRGL_BIND_STREAM_OUTPUT)
      b |= PIPE_BIND_STREAM_OUTPUT;
   if (bind & VIRGL_BIND_SHADER_BUFFER)
      b |= PIPE_BIND_SHADER_BUFFER;
   if (bind & VIRGL_BIND_QUERY_BUFFER)
      b |= PIPE_BIND_QUERY_BUFFER;
   return b;
}

// level_extent returns the extent of level of res in a box's terms: a 1D
// array's layers are its y, and a 2D array's or cube's its z.
static void level_extent(const struct pipe_resource *res, unsigned level, int64_t *w, int64_t *h,
                         int64_t *d) {
   *w = u_minify(res->width0, level);
   *h = 1;
   *d = 1;
   switch (res->target) {
   case PIPE_BUFFER:
      *w = res->width0;
      break;
   case PIPE_TEXTURE_1D:
      break;
   case PIPE_TEXTURE_1D_ARRAY:
      *h = res->array_size;
      break;
   case PIPE_TEXTURE_3D:
      *h = u_minify(res->height0, level);
      *d = u_minify(res->depth0, level);
      break;
   case PIPE_TEXTURE_2D_ARRAY:
   case PIPE_TEXTURE_CUBE:
   case PIPE_TEXTURE_CUBE_ARRAY:
      *h = u_minify(res->height0, level);
      *d = res->array_size;
      break;
   default:
      *h = u_minify(res->height0, level);
      break;
   }
}

static bool span_ok(int64_t start, int64_t size, int64_t extent) {
   int64_t end = start + size;
   if (end < start) {
      int64_t t = start;
      start = end;
      end = t;
   }
   return start >= 0 && end <= extent;
}

// box_ok reports whether box is within level of res. A blit's boxes may
// have negative extents, which flip them.
static bool box_ok(const struct pipe_resource *res, unsigned level, const struct pipe_box *box) {
   if (level > res->last_level)
      return false;
   int64_t w, h, d;
   level_extent(res, level, &w, &h, &d);
   return span_ok(box->x, box->width, w) && span_ok(box->y, box->height, h) &&
          span_ok(box->z, box->depth, d);
}

// resource_ok reports whether a virgl resource is valid, as virglrenderer's
// check_resource_valid does, and Gallium's drivers assume.
static bool resource_ok(const struct pipe_resource *t) {
   const struct pipe_screen *s = ttc_virgl_screen;
   if (t->width0 == 0 || t->nr_samples > 32 || t->last_level > 15)
      return false;
   if (t->nr_samples > 1 && t->target != PIPE_TEXTURE_2D && t->target != PIPE_TEXTURE_2D_ARRAY)
      return false;
   if (t->nr_samples > 1 && t->last_level > 0)
      return false;
   unsigned max2d = s->caps.max_texture_2d_size;
   switch (t->target) {
   case PIPE_BUFFER:
      return t->height0 == 1 && t->depth0 == 1 && t->array_size == 1 && t->last_level == 0;
   case PIPE_TEXTURE_1D:
   case PIPE_TEXTURE_1D_ARRAY:
      if (t->height0 != 1 || t->depth0 != 1 || t->width0 > max2d)
         return false;
      break;
   case PIPE_TEXTURE_RECT:
      if (t->last_level > 0)
         return false;
      FALLTHROUGH;
   case PIPE_TEXTURE_2D:
   case PIPE_TEXTURE_2D_ARRAY:
      if (t->depth0 != 1 || t->width0 > max2d || t->height0 > max2d)
         return false;
      break;
   case PIPE_TEXTURE_CUBE:
   case PIPE_TEXTURE_CUBE_ARRAY:
      if (t->depth0 != 1 || t->width0 != t->height0 || t->array_size % 6 != 0 ||
          t->width0 > (1u << (s->caps.max_texture_cube_levels - 1)))
         return false;
      if (t->target == PIPE_TEXTURE_CUBE && t->array_size != 6)
         return false;
      break;
   case PIPE_TEXTURE_3D:
      if (t->array_size != 1 || t->width0 > (1u << (s->caps.max_texture_3d_levels - 1)) ||
          t->height0 > (1u << (s->caps.max_texture_3d_levels - 1)) ||
          t->depth0 > (1u << (s->caps.max_texture_3d_levels - 1)))
         return false;
      break;
   default:
      return false;
   }
   bool array = t->target == PIPE_TEXTURE_1D_ARRAY || t->target == PIPE_TEXTURE_2D_ARRAY ||
                t->target == PIPE_TEXTURE_CUBE_ARRAY;
   if (!array && t->target != PIPE_TEXTURE_CUBE && t->array_size != 1)
      return false;
   if (array && t->array_size > s->caps.max_texture_array_layers)
      return false;
   unsigned size = MAX3(t->width0, t->target == PIPE_TEXTURE_1D || t->target == PIPE_TEXTURE_1D_ARRAY ? 1 : t->height0,
                        t->target == PIPE_TEXTURE_3D ? t->depth0 : 1);
   return t->target == PIPE_BUFFER || t->last_level <= util_logbase2(size);
}

struct pipe_resource *ttc_virgl_resource_create(uint32_t target, uint32_t format, uint32_t bind,
                                                uint32_t width, uint32_t height, uint32_t depth,
                                                uint32_t array_size, uint32_t last_level,
                                                uint32_t nr_samples, uint32_t flags) {
   (void)flags;
   if (target >= PIPE_MAX_TEXTURE_TYPES)
      return NULL;
   struct pipe_resource t;
   memset(&t, 0, sizeof(t));
   t.target = target;
   t.format = target == PIPE_BUFFER ? PIPE_FORMAT_R8_UNORM : ttc_virgl_pipe_format(format);
   if (t.format == PIPE_FORMAT_NONE)
      return NULL;
   t.bind = pipe_bind(bind);
   // Like virglrenderer, take a buffer of no bytes, and no height, depth or
   // layers for one.
   t.width0 = target == PIPE_BUFFER ? MAX2(width, 1) : width;
   t.height0 = MAX2(height, 1);
   t.depth0 = MAX2(depth, 1);
   t.array_size = MAX2(array_size, 1);
   t.last_level = last_level;
   t.nr_samples = nr_samples > 1 ? nr_samples : 0;
   t.nr_storage_samples = t.nr_samples;
   t.usage = PIPE_USAGE_DEFAULT;
   if (!resource_ok(&t))
      return NULL;
   return ttc_virgl_screen->resource_create(ttc_virgl_screen, &t);
}

void ttc_virgl_resource_unref(struct pipe_resource *res) {
   pipe_resource_reference(&res, NULL);
}

uint32_t ttc_virgl_resource_target(struct pipe_resource *res) {
   return res->target;
}

uint32_t ttc_virgl_resource_width(struct pipe_resource *res, uint32_t level) {
   return u_minify(res->width0, level);
}

uint32_t ttc_virgl_resource_height(struct pipe_resource *res, uint32_t level) {
   return u_minify(res->height0, level);
}

// transfer copies box between res and data, whose rows are stride bytes
// apart and whose layers layer_stride.
static int transfer(struct pipe_context *pipe, struct pipe_resource *res, bool to_host,
                    uint32_t level, const struct pipe_box *box, uint8_t *data, uint64_t size,
                    uint64_t offset, uint32_t stride, uint32_t layer_stride) {
   if (box->width <= 0 || box->height <= 0 || box->depth <= 0 || !box_ok(res, level, box))
      return EINVAL;
   if (res->target == PIPE_BUFFER) {
      if (offset > size || box->width > size - offset)
         return EINVAL;
      if (to_host) {
         pipe->buffer_subdata(pipe, res, PIPE_MAP_WRITE, box->x, box->width, data + offset);
         return 0;
      }
      struct pipe_transfer *t = NULL;
      void *map = pipe->buffer_map(pipe, res, 0, PIPE_MAP_READ, box, &t);
      if (!map)
         return EIO;
      memcpy(data + offset, map, box->width);
      pipe->buffer_unmap(pipe, t);
      return 0;
   }
   enum pipe_format format = res->format;
   int64_t lw, lh, ld;
   level_extent(res, level, &lw, &lh, &ld);
   // Like virglrenderer, rows and layers of the level's size unless the
   // guest says otherwise.
   if (!stride)
      stride = util_format_get_stride(format, lw);
   if (!layer_stride)
      layer_stride = util_format_get_2d_size(format, stride, lh);
   uint64_t rows = util_format_get_nblocksy(format, box->height);
   uint64_t row = util_format_get_stride(format, box->width);
   uint64_t need = (uint64_t)(box->depth - 1) * layer_stride + (rows - 1) * stride + row;
   if (row > stride || offset > size || need > size - offset)
      return EINVAL;
   if (to_host) {
      pipe->texture_subdata(pipe, res, level, PIPE_MAP_WRITE, box, data + offset, stride,
                            layer_stride);
      return 0;
   }
   struct pipe_transfer *t = NULL;
   uint8_t *map = pipe->texture_map(pipe, res, level, PIPE_MAP_READ, box, &t);
   if (!map)
      return EIO;
   util_copy_box(data + offset, format, stride, layer_stride, 0, 0, 0, box->width, box->height,
                 box->depth, map, t->stride, t->layer_stride, 0, 0, 0);
   pipe->texture_unmap(pipe, t);
   return 0;
}

int ttc_virgl_transfer(struct pipe_context *pipe, struct pipe_resource *res, bool to_host,
                       uint32_t level, int32_t x, int32_t y, int32_t z, int32_t w, int32_t h,
                       int32_t d, uint8_t *data, uint64_t size, uint64_t offset, uint32_t stride,
                       uint32_t layer_stride) {
   struct pipe_box box = {.x = x, .y = y, .z = z, .width = w, .height = h, .depth = d};
   return transfer(pipe, res, to_host, level, &box, data, size, offset, stride, layer_stride);
}

struct pipe_context *ttc_virgl_context_create(void) {
   return ttc_virgl_screen->context_create(ttc_virgl_screen, NULL, 0);
}

// ttc_virgl_context_unbind unbinds what cso_unbind_context unbinds.
void ttc_virgl_context_unbind(struct pipe_context *pipe) {
   struct pipe_screen *scr = pipe->screen;
   static struct pipe_sampler_view *views[PIPE_MAX_SHADER_SAMPLER_VIEWS];
   static struct pipe_shader_buffer ssbos[PIPE_MAX_SHADER_BUFFERS];
   static void *samplers[PIPE_MAX_SAMPLERS];
   pipe->bind_blend_state(pipe, NULL);
   pipe->bind_rasterizer_state(pipe, NULL);
   pipe->bind_depth_stencil_alpha_state(pipe, NULL);
   for (unsigned i = 0; i < 6; i++) {
      mesa_shader_stage sh = ttc_virgl_stage(i);
      const struct pipe_shader_caps *caps = &scr->shader_caps[sh];
      if (caps->max_instructions == 0)
         continue;
      if (caps->max_texture_samplers)
         pipe->bind_sampler_states(pipe, sh, 0, MIN2(caps->max_texture_samplers, PIPE_MAX_SAMPLERS),
                                   samplers);
      if (caps->max_sampler_views)
         pipe->set_sampler_views(pipe, sh, 0, 0,
                                 MIN2(caps->max_sampler_views, PIPE_MAX_SHADER_SAMPLER_VIEWS), views);
      if (caps->max_shader_buffers)
         pipe->set_shader_buffers(pipe, sh, 0, MIN2(caps->max_shader_buffers, PIPE_MAX_SHADER_BUFFERS),
                                  ssbos, 0);
      if (caps->max_shader_images)
         pipe->set_shader_images(pipe, sh, 0, 0, MIN2(caps->max_shader_images, PIPE_MAX_SHADER_IMAGES),
                                 NULL);
      for (unsigned cb = 0; cb < MIN2(caps->max_const_buffers, PIPE_MAX_CONSTANT_BUFFERS); cb++)
         pipe->set_constant_buffer(pipe, sh, cb, NULL);
      ttc_virgl_bind_shader(pipe, i, NULL);
   }
   pipe->bind_vertex_elements_state(pipe, NULL);
   pipe->set_vertex_buffers(pipe, 0, NULL);
   if (scr->caps.max_stream_output_buffers)
      pipe->set_stream_output_targets(pipe, 0, NULL, NULL, 0);
   pipe->render_condition(pipe, NULL, false, 0);
   struct pipe_framebuffer_state fb;
   memset(&fb, 0, sizeof(fb));
   pipe->set_framebuffer_state(pipe, &fb);
}

void ttc_virgl_context_destroy(struct pipe_context *pipe) {
   pipe->destroy(pipe);
}

void ttc_virgl_flush_async(struct pipe_context *pipe) {
   pipe->flush(pipe, NULL, 0);
}

struct pipe_fence_handle *ttc_virgl_flush(struct pipe_context *pipe) {
   struct pipe_fence_handle *fence = NULL;
   pipe->flush(pipe, &fence, 0);
   return fence;
}

void ttc_virgl_fence_wait(struct pipe_fence_handle *fence) {
   if (!fence)
      return;
   ttc_virgl_screen->fence_finish(ttc_virgl_screen, NULL, fence, OS_TIMEOUT_INFINITE);
   ttc_virgl_screen->fence_reference(ttc_virgl_screen, &fence, NULL);
}

void *ttc_virgl_create_blend(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_blend_state s;
   memset(&s, 0, sizeof(s));
   uint32_t s0 = d[1];
   s.independent_blend_enable = s0 & 1;
   s.logicop_enable = (s0 >> 1) & 1;
   s.dither = (s0 >> 2) & 1;
   s.alpha_to_coverage = (s0 >> 3) & 1;
   s.alpha_to_one = (s0 >> 4) & 1;
   s.logicop_func = d[2] & 0xf;
   s.max_rt = s.independent_blend_enable ? PIPE_MAX_COLOR_BUFS - 1 : 0;
   for (unsigned i = 0; i < PIPE_MAX_COLOR_BUFS; i++) {
      uint32_t t = d[3 + i];
      s.rt[i].blend_enable = t & 1;
      s.rt[i].rgb_func = (t >> 1) & 7;
      s.rt[i].rgb_src_factor = (t >> 4) & 0x1f;
      s.rt[i].rgb_dst_factor = (t >> 9) & 0x1f;
      s.rt[i].alpha_func = (t >> 14) & 7;
      s.rt[i].alpha_src_factor = (t >> 17) & 0x1f;
      s.rt[i].alpha_dst_factor = (t >> 22) & 0x1f;
      s.rt[i].colormask = (t >> 27) & 0xf;
   }
   return pipe->create_blend_state(pipe, &s);
}

void *ttc_virgl_create_rasterizer(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_rasterizer_state s;
   memset(&s, 0, sizeof(s));
   uint32_t t = d[1];
#define bit(name, n) s.name = (t >> (n)) & 1
   bit(flatshade, 0);
   s.depth_clip_near = s.depth_clip_far = (t >> 1) & 1;
   s.depth_clamp = !s.depth_clip_near;
   bit(clip_halfz, 2);
   bit(rasterizer_discard, 3);
   bit(flatshade_first, 4);
   bit(light_twoside, 5);
   bit(sprite_coord_mode, 6);
   bit(point_quad_rasterization, 7);
   s.cull_face = (t >> 8) & 3;
   s.fill_front = (t >> 10) & 3;
   s.fill_back = (t >> 12) & 3;
   bit(scissor, 14);
   bit(front_ccw, 15);
   bit(clamp_vertex_color, 16);
   bit(clamp_fragment_color, 17);
   bit(offset_line, 18);
   bit(offset_point, 19);
   bit(offset_tri, 20);
   bit(poly_smooth, 21);
   bit(poly_stipple_enable, 22);
   bit(point_smooth, 23);
   bit(point_size_per_vertex, 24);
   bit(multisample, 25);
   bit(line_smooth, 26);
   bit(line_stipple_enable, 27);
   bit(line_last_pixel, 28);
   bit(half_pixel_center, 29);
   bit(bottom_edge_rule, 30);
   bit(force_persample_interp, 31);
#undef bit
   s.point_size = uif(d[2]);
   s.sprite_coord_enable = d[3];
   s.line_stipple_pattern = d[4] & 0xffff;
   s.line_stipple_factor = (d[4] >> 16) & 0xff;
   s.clip_plane_enable = (d[4] >> 24) & 0xff;
   s.line_width = uif(d[5]);
   s.offset_units = uif(d[6]);
   s.offset_scale = uif(d[7]);
   s.offset_clamp = uif(d[8]);
   return pipe->create_rasterizer_state(pipe, &s);
}

void *ttc_virgl_create_dsa(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_depth_stencil_alpha_state s;
   memset(&s, 0, sizeof(s));
   uint32_t t = d[1];
   s.depth_enabled = t & 1;
   s.depth_writemask = (t >> 1) & 1;
   s.depth_func = (t >> 2) & 7;
   s.alpha_enabled = (t >> 8) & 1;
   s.alpha_func = (t >> 9) & 7;
   for (unsigned i = 0; i < 2; i++) {
      t = d[2 + i];
      s.stencil[i].enabled = t & 1;
      s.stencil[i].func = (t >> 1) & 7;
      s.stencil[i].fail_op = (t >> 4) & 7;
      s.stencil[i].zpass_op = (t >> 7) & 7;
      s.stencil[i].zfail_op = (t >> 10) & 7;
      s.stencil[i].valuemask = (t >> 13) & 0xff;
      s.stencil[i].writemask = (t >> 21) & 0xff;
   }
   s.alpha_ref_value = uif(d[4]);
   return pipe->create_depth_stencil_alpha_state(pipe, &s);
}

void *ttc_virgl_create_sampler_state(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_sampler_state s;
   memset(&s, 0, sizeof(s));
   uint32_t t = d[1];
   s.wrap_s = t & 7;
   s.wrap_t = (t >> 3) & 7;
   s.wrap_r = (t >> 6) & 7;
   s.min_img_filter = (t >> 9) & 1;
   s.min_mip_filter = (t >> 11) & 3;
   s.mag_img_filter = (t >> 13) & 1;
   s.compare_mode = (t >> 15) & 1;
   s.compare_func = (t >> 16) & 7;
   s.seamless_cube_map = (t >> 19) & 1;
   s.max_anisotropy = (t >> 20) & 0x1f;
   s.lod_bias = uif(d[2]);
   s.min_lod = uif(d[3]);
   s.max_lod = uif(d[4]);
   for (unsigned i = 0; i < 4; i++)
      s.border_color.ui[i] = d[5 + i];
   if (s.min_mip_filter > PIPE_TEX_MIPFILTER_NONE)
      return NULL;
   return pipe->create_sampler_state(pipe, &s);
}

void *ttc_virgl_create_vertex_elements(struct pipe_context *pipe, const uint32_t *elements,
                                       uint32_t count, const uint32_t *strides) {
   struct pipe_vertex_element ve[PIPE_MAX_ATTRIBS];
   if (count > PIPE_MAX_ATTRIBS)
      return NULL;
   memset(ve, 0, sizeof(ve));
   for (uint32_t i = 0; i < count; i++) {
      const uint32_t *e = elements + 4 * i;
      if (e[2] >= PIPE_MAX_ATTRIBS)
         return NULL;
      ve[i].src_offset = e[0];
      ve[i].instance_divisor = e[1];
      ve[i].vertex_buffer_index = e[2];
      enum pipe_format format = ttc_virgl_pipe_format(e[3]);
      if (format == PIPE_FORMAT_NONE)
         return NULL;
      ve[i].src_format = format;
      ve[i].src_stride = strides[e[2]];
   }
   return pipe->create_vertex_elements_state(pipe, count, ve);
}

void *ttc_virgl_translate_tgsi(const char *text, uint32_t num_tokens, char *err,
                               uint32_t errlen) {
   if (num_tokens > 0x100000) {
      snprintf(err, errlen, "%u tokens", num_tokens);
      return NULL;
   }
   // virglrenderer's margin: the guest counts the tokens of its own TGSI.
   uint32_t ntokens = num_tokens + 10;
   struct tgsi_token *tokens = CALLOC(ntokens, sizeof(struct tgsi_token));
   if (!tokens) {
      snprintf(err, errlen, "no memory for %u tokens", ntokens);
      return NULL;
   }
   if (!tgsi_text_translate(text, tokens, ntokens)) {
      snprintf(err, errlen, "invalid TGSI");
      FREE(tokens);
      return NULL;
   }
   return tokens;
}

void ttc_virgl_free_tokens(void *tokens) {
   FREE(tokens);
}

uint32_t ttc_virgl_format_int_class(uint32_t format) {
   enum pipe_format f = ttc_virgl_pipe_format(format);
   if (util_format_is_pure_sint(f))
      return 1;
   if (util_format_is_pure_uint(f))
      return 2;
   return 0;
}

// retype_inputs gives a vertex shader's inputs of sint_mask and uint_mask
// integer types, which tgsi_to_nir declares as floats, and their derefs.
static void retype_inputs(nir_shader *nir, uint32_t sint_mask, uint32_t uint_mask) {
   nir_foreach_shader_in_variable(var, nir) {
      if (var->data.location < VERT_ATTRIB_GENERIC0 || glsl_type_is_array(var->type))
         continue;
      unsigned i = var->data.location - VERT_ATTRIB_GENERIC0;
      if (i >= 32)
         continue;
      enum glsl_base_type base;
      if (sint_mask & (1u << i))
         base = GLSL_TYPE_INT;
      else if (uint_mask & (1u << i))
         base = GLSL_TYPE_UINT;
      else
         continue;
      var->type = glsl_vector_type(base, glsl_get_vector_elements(var->type));
   }
   nir_foreach_function_impl(impl, nir) {
      nir_foreach_block(block, impl) {
         nir_foreach_instr(instr, block) {
            if (instr->type != nir_instr_type_deref)
               continue;
            nir_deref_instr *deref = nir_instr_as_deref(instr);
            if (deref->deref_type == nir_deref_type_var && deref->var->data.mode == nir_var_shader_in)
               deref->type = deref->var->type;
         }
      }
   }
}

void *ttc_virgl_create_shader(struct pipe_context *pipe, uint32_t stage, const void *tokens,
                              const uint32_t *so, uint32_t so_dwords, uint32_t shared_mem,
                              uint32_t sint_mask, uint32_t uint_mask, char *err,
                              uint32_t errlen) {
   mesa_shader_stage s = ttc_virgl_stage(stage);
   if (s == MESA_SHADER_NONE) {
      snprintf(err, errlen, "stage %u", stage);
      return NULL;
   }
   void *shader = NULL;
   if (s == MESA_SHADER_COMPUTE) {
      struct pipe_compute_state cs = {
         .ir_type = PIPE_SHADER_IR_TGSI,
         .prog = tokens,
         .static_shared_mem = shared_mem,
      };
      shader = pipe->create_compute_state(pipe, &cs);
      if (!shader)
         snprintf(err, errlen, "the driver refused the shader");
      return shader;
   }
   struct pipe_shader_state ss;
   memset(&ss, 0, sizeof(ss));
   ss.type = PIPE_SHADER_IR_TGSI;
   ss.tokens = tokens;
   if (so_dwords > 0 && so[0] > 0) {
      uint32_t n = so[0];
      if (n > PIPE_MAX_SO_OUTPUTS || so_dwords < 5 + 2 * n) {
         snprintf(err, errlen, "%u stream outputs in %u dwords", n, so_dwords);
         return NULL;
      }
      ss.stream_output.num_outputs = n;
      for (unsigned i = 0; i < 4; i++)
         ss.stream_output.stride[i] = so[1 + i];
      for (uint32_t i = 0; i < n; i++) {
         uint32_t t = so[5 + 2 * i];
         struct pipe_stream_output *o = &ss.stream_output.output[i];
         o->register_index = t & 0xff;
         o->start_component = (t >> 8) & 3;
         o->num_components = (t >> 10) & 7;
         o->output_buffer = (t >> 13) & 7;
         o->dst_offset = (t >> 16) & 0xffff;
         o->stream = so[6 + 2 * i] & 3;
      }
   }
   if (s == MESA_SHADER_VERTEX && (sint_mask | uint_mask)) {
      nir_shader *nir = tgsi_to_nir(tokens, pipe->screen, false);
      if (!nir) {
         snprintf(err, errlen, "tgsi_to_nir failed");
         return NULL;
      }
      retype_inputs(nir, sint_mask, uint_mask);
      ss.type = PIPE_SHADER_IR_NIR;
      ss.ir.nir = nir;
   }
   switch (s) {
   case MESA_SHADER_VERTEX:
      shader = pipe->create_vs_state(pipe, &ss);
      break;
   case MESA_SHADER_FRAGMENT:
      shader = pipe->create_fs_state(pipe, &ss);
      break;
   case MESA_SHADER_GEOMETRY:
      shader = pipe->create_gs_state(pipe, &ss);
      break;
   case MESA_SHADER_TESS_CTRL:
      shader = pipe->create_tcs_state(pipe, &ss);
      break;
   case MESA_SHADER_TESS_EVAL:
      shader = pipe->create_tes_state(pipe, &ss);
      break;
   default:
      break;
   }
   if (!shader)
      snprintf(err, errlen, "the driver refused the shader");
   return shader;
}

struct pipe_sampler_view *ttc_virgl_create_sampler_view(struct pipe_context *pipe,
                                                       struct pipe_resource *res,
                                                       uint32_t format, uint32_t target,
                                                       uint32_t val0, uint32_t val1,
                                                       uint32_t swizzle) {
   struct pipe_sampler_view t;
   memset(&t, 0, sizeof(t));
   t.format = ttc_virgl_pipe_format(format);
   if (t.format == PIPE_FORMAT_NONE)
      return NULL;
   // A guest without texture views, or a view of a buffer, names no target.
   t.target = target ? target : res->target;
   t.swizzle_r = swizzle & 7;
   t.swizzle_g = (swizzle >> 3) & 7;
   t.swizzle_b = (swizzle >> 6) & 7;
   t.swizzle_a = (swizzle >> 9) & 7;
   if (res->target == PIPE_BUFFER) {
      t.target = PIPE_BUFFER;
      unsigned size = util_format_get_blocksize(t.format);
      if (val1 < val0 || size == 0)
         return NULL;
      t.u.buf.offset = val0 * size;
      t.u.buf.size = (val1 - val0 + 1) * size;
   } else {
      t.u.tex.first_layer = val0 & 0xffff;
      t.u.tex.last_layer = (val0 >> 16) & 0xffff;
      t.u.tex.first_level = val1 & 0xff;
      t.u.tex.last_level = (val1 >> 8) & 0xff;
   }
   return pipe->create_sampler_view(pipe, res, &t);
}

// query_type returns Gallium's query of virgl's, which numbers them as
// Gallium did.
static int query_type(uint32_t type) {
   static const int types[] = {
      PIPE_QUERY_OCCLUSION_COUNTER,       PIPE_QUERY_OCCLUSION_PREDICATE,
      PIPE_QUERY_TIMESTAMP,               PIPE_QUERY_TIMESTAMP_DISJOINT,
      PIPE_QUERY_TIME_ELAPSED,            PIPE_QUERY_PRIMITIVES_GENERATED,
      PIPE_QUERY_PRIMITIVES_EMITTED,      PIPE_QUERY_SO_STATISTICS,
      PIPE_QUERY_SO_OVERFLOW_PREDICATE,   PIPE_QUERY_GPU_FINISHED,
      PIPE_QUERY_PIPELINE_STATISTICS_SINGLE, PIPE_QUERY_OCCLUSION_PREDICATE_CONSERVATIVE,
      PIPE_QUERY_SO_OVERFLOW_ANY_PREDICATE,
   };
   return type < sizeof(types) / sizeof(types[0]) ? types[type] : -1;
}

struct pipe_query *ttc_virgl_create_query(struct pipe_context *pipe, uint32_t type,
                                          uint32_t index) {
   int t = query_type(type);
   if (t < 0)
      return NULL;
   return pipe->create_query(pipe, t, index);
}

struct pipe_stream_output_target *ttc_virgl_create_so_target(struct pipe_context *pipe,
                                                             struct pipe_resource *res,
                                                             uint32_t offset, uint32_t size) {
   return pipe->create_stream_output_target(pipe, res, offset, size);
}

void ttc_virgl_delete_object(struct pipe_context *pipe, uint32_t type, uint32_t stage, void *obj) {
   switch (type) {
   case VIRGL_OBJECT_BLEND:
      pipe->delete_blend_state(pipe, obj);
      break;
   case VIRGL_OBJECT_RASTERIZER:
      pipe->delete_rasterizer_state(pipe, obj);
      break;
   case VIRGL_OBJECT_DSA:
      pipe->delete_depth_stencil_alpha_state(pipe, obj);
      break;
   case VIRGL_OBJECT_VERTEX_ELEMENTS:
      pipe->delete_vertex_elements_state(pipe, obj);
      break;
   case VIRGL_OBJECT_SAMPLER_STATE:
      pipe->delete_sampler_state(pipe, obj);
      break;
   case VIRGL_OBJECT_SAMPLER_VIEW:
      pipe_sampler_view_release(obj);
      break;
   case VIRGL_OBJECT_QUERY:
      pipe->destroy_query(pipe, obj);
      break;
   case VIRGL_OBJECT_STREAMOUT_TARGET: {
      struct pipe_stream_output_target *t = obj;
      pipe_so_target_reference(&t, NULL);
      break;
   }
   case VIRGL_OBJECT_SHADER:
      switch (ttc_virgl_stage(stage)) {
      case MESA_SHADER_VERTEX:
         pipe->delete_vs_state(pipe, obj);
         break;
      case MESA_SHADER_FRAGMENT:
         pipe->delete_fs_state(pipe, obj);
         break;
      case MESA_SHADER_GEOMETRY:
         pipe->delete_gs_state(pipe, obj);
         break;
      case MESA_SHADER_TESS_CTRL:
         pipe->delete_tcs_state(pipe, obj);
         break;
      case MESA_SHADER_TESS_EVAL:
         pipe->delete_tes_state(pipe, obj);
         break;
      case MESA_SHADER_COMPUTE:
         pipe->delete_compute_state(pipe, obj);
         break;
      default:
         break;
      }
      break;
   default:
      break;
   }
}

void ttc_virgl_bind_object(struct pipe_context *pipe, uint32_t type, void *obj) {
   switch (type) {
   case VIRGL_OBJECT_BLEND:
      pipe->bind_blend_state(pipe, obj);
      break;
   case VIRGL_OBJECT_RASTERIZER:
      pipe->bind_rasterizer_state(pipe, obj);
      break;
   case VIRGL_OBJECT_DSA:
      pipe->bind_depth_stencil_alpha_state(pipe, obj);
      break;
   case VIRGL_OBJECT_VERTEX_ELEMENTS:
      pipe->bind_vertex_elements_state(pipe, obj);
      break;
   default:
      break;
   }
}

void ttc_virgl_bind_shader(struct pipe_context *pipe, uint32_t stage, void *shader) {
   switch (ttc_virgl_stage(stage)) {
   case MESA_SHADER_VERTEX:
      pipe->bind_vs_state(pipe, shader);
      break;
   case MESA_SHADER_FRAGMENT:
      pipe->bind_fs_state(pipe, shader);
      break;
   case MESA_SHADER_GEOMETRY:
      pipe->bind_gs_state(pipe, shader);
      break;
   case MESA_SHADER_TESS_CTRL:
      pipe->bind_tcs_state(pipe, shader);
      break;
   case MESA_SHADER_TESS_EVAL:
      pipe->bind_tes_state(pipe, shader);
      break;
   case MESA_SHADER_COMPUTE:
      pipe->bind_compute_state(pipe, shader);
      break;
   default:
      break;
   }
}

int ttc_virgl_check_surface(struct pipe_resource *res, uint32_t format, uint32_t val0,
                            uint32_t val1) {
   enum pipe_format f = ttc_virgl_pipe_format(format);
   if (f == PIPE_FORMAT_NONE)
      return EINVAL;
   if (res->target == PIPE_BUFFER) {
      // virglrenderer's buffers' surfaces: first and last elements.
      return val1 < val0 ? EINVAL : 0;
   }
   unsigned first = val1 & 0xffff, last = (val1 >> 16) & 0xffff;
   int64_t w, h, d;
   level_extent(res, val0, &w, &h, &d);
   int64_t layers = res->target == PIPE_TEXTURE_1D_ARRAY ? h : d;
   if (val0 > res->last_level || last < first || last >= layers)
      return EINVAL;
   return 0;
}

// surface fills a Gallium surface of a virgl surface, whose val1 is its
// layers, and val0 its level, of a texture.
static void surface(struct pipe_surface *dst, const struct ttc_virgl_surface *s) {
   memset(dst, 0, sizeof(*dst));
   if (!s->res)
      return;
   dst->texture = s->res;
   dst->format = ttc_virgl_pipe_format(s->format);
   dst->nr_samples = s->nr_samples;
   if (s->res->target != PIPE_BUFFER) {
      dst->level = s->val0;
      dst->first_layer = s->val1 & 0xffff;
      dst->last_layer = (s->val1 >> 16) & 0xffff;
   }
}

void ttc_virgl_set_framebuffer(struct pipe_context *pipe, uint32_t nr_cbufs,
                               const struct ttc_virgl_surface *cbufs,
                               const struct ttc_virgl_surface *zsbuf) {
   struct pipe_framebuffer_state fb;
   memset(&fb, 0, sizeof(fb));
   uint32_t width = UINT32_MAX, height = UINT32_MAX;
   fb.nr_cbufs = nr_cbufs;
   for (uint32_t i = 0; i < nr_cbufs && i < PIPE_MAX_COLOR_BUFS; i++) {
      surface(&fb.cbufs[i], &cbufs[i]);
      if (cbufs[i].res) {
         width = MIN2(width, u_minify(cbufs[i].res->width0, fb.cbufs[i].level));
         height = MIN2(height, u_minify(cbufs[i].res->height0, fb.cbufs[i].level));
      }
   }
   surface(&fb.zsbuf, zsbuf);
   if (zsbuf->res) {
      width = MIN2(width, u_minify(zsbuf->res->width0, fb.zsbuf.level));
      height = MIN2(height, u_minify(zsbuf->res->height0, fb.zsbuf.level));
   }
   fb.width = width == UINT32_MAX ? 0 : width;
   fb.height = height == UINT32_MAX ? 0 : height;
   pipe->set_framebuffer_state(pipe, &fb);
}

void ttc_virgl_set_framebuffer_no_attach(struct pipe_context *pipe, uint32_t width,
                                         uint32_t height, uint32_t layers, uint32_t samples) {
   struct pipe_framebuffer_state fb;
   memset(&fb, 0, sizeof(fb));
   fb.width = width;
   fb.height = height;
   fb.layers = layers;
   fb.samples = samples;
   pipe->set_framebuffer_state(pipe, &fb);
}

void ttc_virgl_set_viewports(struct pipe_context *pipe, uint32_t start, uint32_t count,
                             const uint32_t *d) {
   struct pipe_viewport_state vp[PIPE_MAX_VIEWPORTS];
   for (uint32_t v = 0; v < count; v++) {
      for (unsigned i = 0; i < 3; i++) {
         vp[v].scale[i] = uif(d[6 * v + i]);
         vp[v].translate[i] = uif(d[6 * v + 3 + i]);
      }
      vp[v].swizzle_x = PIPE_VIEWPORT_SWIZZLE_POSITIVE_X;
      vp[v].swizzle_y = PIPE_VIEWPORT_SWIZZLE_POSITIVE_Y;
      vp[v].swizzle_z = PIPE_VIEWPORT_SWIZZLE_POSITIVE_Z;
      vp[v].swizzle_w = PIPE_VIEWPORT_SWIZZLE_POSITIVE_W;
   }
   pipe->set_viewport_states(pipe, start, count, vp);
}

void ttc_virgl_set_scissors(struct pipe_context *pipe, uint32_t start, uint32_t count,
                            const uint32_t *d) {
   struct pipe_scissor_state ss[PIPE_MAX_VIEWPORTS];
   for (uint32_t i = 0; i < count; i++) {
      ss[i].minx = d[2 * i] & 0xffff;
      ss[i].miny = (d[2 * i] >> 16) & 0xffff;
      ss[i].maxx = d[2 * i + 1] & 0xffff;
      ss[i].maxy = (d[2 * i + 1] >> 16) & 0xffff;
   }
   pipe->set_scissor_states(pipe, start, count, ss);
}

void ttc_virgl_set_vertex_buffers(struct pipe_context *pipe, uint32_t count,
                                  struct pipe_resource **res, const uint32_t *offsets) {
   struct pipe_vertex_buffer vb[PIPE_MAX_ATTRIBS];
   memset(vb, 0, sizeof(vb));
   for (uint32_t i = 0; i < count && i < PIPE_MAX_ATTRIBS; i++) {
      vb[i].buffer.resource = res[i];
      vb[i].buffer_offset = offsets[i];
   }
   pipe->set_vertex_buffers(pipe, count, vb);
}

void ttc_virgl_set_constants(struct pipe_context *pipe, uint32_t stage, const uint32_t *d,
                             uint32_t n) {
   mesa_shader_stage s = ttc_virgl_stage(stage);
   if (n == 0) {
      pipe->set_constant_buffer(pipe, s, 0, NULL);
      return;
   }
   struct pipe_constant_buffer cb = {.user_buffer = d, .buffer_size = n * 4};
   pipe->set_constant_buffer(pipe, s, 0, &cb);
}

void ttc_virgl_set_uniform_buffer(struct pipe_context *pipe, uint32_t stage, uint32_t index,
                                  struct pipe_resource *res, uint32_t offset, uint32_t size) {
   mesa_shader_stage s = ttc_virgl_stage(stage);
   if (!res) {
      pipe->set_constant_buffer(pipe, s, index, NULL);
      return;
   }
   struct pipe_constant_buffer cb = {.buffer = res, .buffer_offset = offset, .buffer_size = size};
   pipe->set_constant_buffer(pipe, s, index, &cb);
}

void ttc_virgl_set_sampler_views(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                 uint32_t count, struct pipe_sampler_view **views) {
   pipe->set_sampler_views(pipe, ttc_virgl_stage(stage), start, count, 0, views);
}

void ttc_virgl_bind_samplers(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                             uint32_t count, void **samplers) {
   pipe->bind_sampler_states(pipe, ttc_virgl_stage(stage), start, count, samplers);
}

void ttc_virgl_set_stencil_ref(struct pipe_context *pipe, uint32_t v) {
   struct pipe_stencil_ref ref = {.ref_value = {v & 0xff, (v >> 8) & 0xff}};
   pipe->set_stencil_ref(pipe, ref);
}

void ttc_virgl_set_blend_color(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_blend_color c;
   for (unsigned i = 0; i < 4; i++)
      c.color[i] = uif(d[i]);
   pipe->set_blend_color(pipe, &c);
}

void ttc_virgl_set_polygon_stipple(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_poly_stipple ps;
   for (unsigned i = 0; i < 32; i++)
      ps.stipple[i] = d[i];
   pipe->set_polygon_stipple(pipe, &ps);
}

void ttc_virgl_set_clip_state(struct pipe_context *pipe, const uint32_t *d) {
   struct pipe_clip_state clip;
   for (unsigned i = 0; i < 8; i++) {
      for (unsigned j = 0; j < 4; j++)
         clip.ucp[i][j] = uif(d[4 * i + j]);
   }
   pipe->set_clip_state(pipe, &clip);
}

void ttc_virgl_set_sample_mask(struct pipe_context *pipe, uint32_t mask) {
   pipe->set_sample_mask(pipe, mask);
}

void ttc_virgl_set_min_samples(struct pipe_context *pipe, uint32_t n) {
   if (pipe->set_min_samples)
      pipe->set_min_samples(pipe, n);
}

void ttc_virgl_set_tess_state(struct pipe_context *pipe, const uint32_t *d) {
   float outer[4], inner[2];
   for (unsigned i = 0; i < 4; i++)
      outer[i] = uif(d[i]);
   for (unsigned i = 0; i < 2; i++)
      inner[i] = uif(d[4 + i]);
   if (pipe->set_tess_state)
      pipe->set_tess_state(pipe, outer, inner);
}

void ttc_virgl_set_shader_buffers(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                  uint32_t count, struct pipe_resource **res,
                                  const uint32_t *offsets, const uint32_t *sizes) {
   struct pipe_shader_buffer sb[PIPE_MAX_SHADER_BUFFERS];
   memset(sb, 0, sizeof(sb));
   for (uint32_t i = 0; i < count && i < PIPE_MAX_SHADER_BUFFERS; i++) {
      sb[i].buffer = res[i];
      sb[i].buffer_offset = offsets[i];
      sb[i].buffer_size = sizes[i];
   }
   pipe->set_shader_buffers(pipe, ttc_virgl_stage(stage), start, count, sb, ~0u);
}

void ttc_virgl_set_atomic_buffers(struct pipe_context *pipe, uint32_t start, uint32_t count,
                                  struct pipe_resource **res, const uint32_t *offsets,
                                  const uint32_t *sizes) {
   struct pipe_shader_buffer sb[PIPE_MAX_HW_ATOMIC_BUFFERS];
   memset(sb, 0, sizeof(sb));
   for (uint32_t i = 0; i < count && i < PIPE_MAX_HW_ATOMIC_BUFFERS; i++) {
      sb[i].buffer = res[i];
      sb[i].buffer_offset = offsets[i];
      sb[i].buffer_size = sizes[i];
   }
   if (pipe->set_hw_atomic_buffers)
      pipe->set_hw_atomic_buffers(pipe, start, count, sb);
}

void ttc_virgl_set_shader_images(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                 uint32_t count, struct pipe_resource **res,
                                 const uint32_t *d) {
   struct pipe_image_view iv[PIPE_MAX_SHADER_IMAGES];
   memset(iv, 0, sizeof(iv));
   for (uint32_t i = 0; i < count && i < PIPE_MAX_SHADER_IMAGES; i++) {
      const uint32_t *e = d + 5 * i;
      if (!res[i])
         continue;
      iv[i].resource = res[i];
      iv[i].format = ttc_virgl_pipe_format(e[0]);
      iv[i].access = e[1] & 0xffff;
      iv[i].shader_access = iv[i].access;
      if (res[i]->target == PIPE_BUFFER) {
         iv[i].u.buf.offset = e[2];
         iv[i].u.buf.size = e[3];
      } else {
         iv[i].u.tex.first_layer = e[2] & 0xffff;
         iv[i].u.tex.last_layer = (e[2] >> 16) & 0xffff;
         iv[i].u.tex.level = e[3];
      }
   }
   pipe->set_shader_images(pipe, ttc_virgl_stage(stage), start, count, 0, iv);
}

void ttc_virgl_set_so_targets(struct pipe_context *pipe, uint32_t count,
                              struct pipe_stream_output_target **targets, uint32_t append) {
   unsigned offsets[PIPE_MAX_SO_BUFFERS];
   for (uint32_t i = 0; i < count && i < PIPE_MAX_SO_BUFFERS; i++)
      offsets[i] = (append >> i) & 1 ? (unsigned)-1 : 0;
   pipe->set_stream_output_targets(pipe, MIN2(count, PIPE_MAX_SO_BUFFERS), targets, offsets,
                                   MESA_PRIM_UNKNOWN);
}

void ttc_virgl_render_condition(struct pipe_context *pipe, struct pipe_query *q,
                                bool condition, uint32_t mode) {
   pipe->render_condition(pipe, q, condition, mode);
}

void ttc_virgl_clear(struct pipe_context *pipe, const uint32_t *d) {
   union pipe_color_union color;
   for (unsigned i = 0; i < 4; i++)
      color.ui[i] = d[1 + i];
   double depth;
   memcpy(&depth, &d[5], sizeof(depth));
   // virgl's clears clear the whole of each buffer.
   pipe->clear(pipe, d[0], ~0u, 0xff, NULL, &color, depth, d[7]);
}

int ttc_virgl_clear_texture(struct pipe_context *pipe, struct pipe_resource *res,
                            const uint32_t *d) {
   struct pipe_box box = {
      .x = d[2], .y = d[3], .z = d[4], .width = d[5], .height = d[6], .depth = d[7],
   };
   if (box.width < 0 || box.height < 0 || box.depth < 0 || !box_ok(res, d[1], &box))
      return EINVAL;
   pipe->clear_texture(pipe, res, d[1], &box, &d[8]);
   return 0;
}

int ttc_virgl_clear_surface(struct pipe_context *pipe, const struct ttc_virgl_surface *s,
                            const uint32_t *d) {
   struct pipe_surface surf;
   surface(&surf, s);
   if (s->res->target == PIPE_BUFFER)
      return EINVAL;
   int64_t w = u_minify(s->res->width0, surf.level), h = u_minify(s->res->height0, surf.level);
   if (!span_ok(d[6], d[8], w) || !span_ok(d[7], d[9], h))
      return EINVAL;
   bool rc = d[0] & 1;
   unsigned buffers = (d[0] >> 1) & 7;
   if (buffers & PIPE_CLEAR_COLOR0) {
      union pipe_color_union color;
      for (unsigned i = 0; i < 4; i++)
         color.ui[i] = d[2 + i];
      pipe->clear_render_target(pipe, &surf, &color, d[6], d[7], d[8], d[9], rc);
      return 0;
   }
   if (!(buffers & PIPE_CLEAR_DEPTHSTENCIL))
      return 0;
   double depth;
   memcpy(&depth, &d[2], sizeof(depth));
   pipe->clear_depth_stencil(pipe, &surf, buffers & PIPE_CLEAR_DEPTHSTENCIL, depth, d[4], d[6],
                             d[7], d[8], d[9], rc);
   return 0;
}

// buffer_range_ok reports whether size bytes from offset are within the
// buffer res.
static bool buffer_range_ok(const struct pipe_resource *res, uint64_t offset, uint64_t size) {
   return res->target == PIPE_BUFFER && offset <= res->width0 && size <= res->width0 - offset;
}

int ttc_virgl_draw(struct pipe_context *pipe, const uint32_t *d, uint32_t n,
                   const struct ttc_virgl_draw *draw) {
   if (d[2] >= MESA_PRIM_COUNT)
      return EINVAL;
   struct pipe_draw_info info;
   memset(&info, 0, sizeof(info));
   struct pipe_draw_start_count_bias sc = {.start = d[0], .count = d[1], .index_bias = d[5]};
   info.mode = d[2];
   if (d[3] && draw->index && draw->index_size) {
      info.index_size = draw->index_size;
      info.index.resource = draw->index;
      sc.start += draw->index_offset / draw->index_size;
      info.min_index = d[9];
      info.max_index = d[10];
      info.index_bounds_valid = d[10] >= d[9] && d[10] != ~0u;
      info.primitive_restart = d[7] != 0;
      info.restart_index = d[8];
   }
   info.instance_count = d[4];
   info.start_instance = d[6];
   unsigned drawid = 0;
   if (n >= 14) {
      if (pipe->set_patch_vertices && d[12])
         pipe->set_patch_vertices(pipe, d[12]);
      drawid = d[13];
   }
   struct pipe_draw_indirect_info indirect;
   memset(&indirect, 0, sizeof(indirect));
   const struct pipe_draw_indirect_info *ind = NULL;
   if (n >= 20 && draw->indirect) {
      // A draw's arguments are at most 5 dwords; its count, 1.
      uint64_t stride = d[16] ? d[16] : 20;
      if (!buffer_range_ok(draw->indirect, d[15], stride * MAX2(d[17], 1)) ||
          (draw->indirect_count && !buffer_range_ok(draw->indirect_count, d[18], 4)))
         return EINVAL;
      indirect.buffer = draw->indirect;
      indirect.offset = d[15];
      indirect.stride = d[16];
      indirect.draw_count = d[17];
      indirect.indirect_draw_count_offset = d[18];
      indirect.indirect_draw_count = draw->indirect_count;
      ind = &indirect;
   } else if (draw->count_from_so) {
      indirect.count_from_stream_output = draw->count_from_so;
      ind = &indirect;
   }
   pipe->draw_vbo(pipe, &info, drawid, ind, &sc, 1);
   return 0;
}

int ttc_virgl_blit(struct pipe_context *pipe, struct pipe_resource *dst,
                   struct pipe_resource *src, const uint32_t *d) {
   struct pipe_blit_info info;
   memset(&info, 0, sizeof(info));
   uint32_t t = d[0];
   info.mask = t & 0xff;
   info.filter = (t >> 8) & 3;
   info.scissor_enable = (t >> 10) & 1;
   info.render_condition_enable = (t >> 11) & 1;
   info.alpha_blend = (t >> 12) & 1;
   info.scissor.minx = d[1] & 0xffff;
   info.scissor.miny = (d[1] >> 16) & 0xffff;
   info.scissor.maxx = d[2] & 0xffff;
   info.scissor.maxy = (d[2] >> 16) & 0xffff;
   info.dst.resource = dst;
   info.dst.level = d[4];
   info.dst.format = ttc_virgl_pipe_format(d[5]);
   info.dst.box = (struct pipe_box){.x = d[6], .y = d[7], .z = d[8], .width = d[9],
                                    .height = d[10], .depth = d[11]};
   info.src.resource = src;
   info.src.level = d[13];
   info.src.format = ttc_virgl_pipe_format(d[14]);
   info.src.box = (struct pipe_box){.x = d[15], .y = d[16], .z = d[17], .width = d[18],
                                    .height = d[19], .depth = d[20]};
   if (info.dst.format == PIPE_FORMAT_NONE || info.src.format == PIPE_FORMAT_NONE ||
       !box_ok(dst, info.dst.level, &info.dst.box) || !box_ok(src, info.src.level, &info.src.box))
      return EINVAL;
   pipe->blit(pipe, &info);
   return 0;
}

int ttc_virgl_copy_region(struct pipe_context *pipe, struct pipe_resource *dst,
                          struct pipe_resource *src, const uint32_t *d) {
   struct pipe_box box = {.x = d[7], .y = d[8], .z = d[9], .width = d[10], .height = d[11],
                          .depth = d[12]};
   struct pipe_box to = {.x = d[2], .y = d[3], .z = d[4], .width = box.width, .height = box.height,
                         .depth = box.depth};
   if (box.width < 0 || box.height < 0 || box.depth < 0 || !box_ok(src, d[6], &box) ||
       !box_ok(dst, d[1], &to) || (dst->target == PIPE_BUFFER) != (src->target == PIPE_BUFFER) ||
       util_format_get_blocksize(dst->format) != util_format_get_blocksize(src->format))
      return EINVAL;
   pipe->resource_copy_region(pipe, dst, d[1], d[2], d[3], d[4], src, d[6], &box);
   return 0;
}

int ttc_virgl_inline_write(struct pipe_context *pipe, struct pipe_resource *res,
                           const uint32_t *d, const uint8_t *data, uint32_t size) {
   struct pipe_box box = {.x = d[5], .y = d[6], .z = d[7], .width = d[8], .height = d[9],
                          .depth = d[10]};
   uint32_t stride = d[3], layer_stride = d[4];
   // The guest packs an inline write's rows, and says so.
   if (!stride)
      stride = util_format_get_stride(res->format, box.width);
   if (!layer_stride)
      layer_stride = util_format_get_2d_size(res->format, stride, box.height);
   return transfer(pipe, res, true, d[1], &box, (uint8_t *)data, size, 0, stride, layer_stride);
}

int ttc_virgl_launch_grid(struct pipe_context *pipe, const uint32_t *d,
                          struct pipe_resource *indirect) {
   if (indirect && !buffer_range_ok(indirect, d[7], 12))
      return EINVAL;
   struct pipe_grid_info info;
   memset(&info, 0, sizeof(info));
   info.work_dim = 3;
   for (unsigned i = 0; i < 3; i++) {
      info.block[i] = d[i];
      info.grid[i] = d[3 + i];
      info.last_block[i] = 0;
   }
   info.indirect = indirect;
   info.indirect_offset = d[7];
   pipe->launch_grid(pipe, &info);
   return 0;
}

void ttc_virgl_memory_barrier(struct pipe_context *pipe, uint32_t flags) {
   // Guests send their own Mesa's PIPE_BARRIER_* values, which changed over
   // the years: any barrier is a full one.
   if (flags)
      pipe->memory_barrier(pipe, PIPE_BARRIER_ALL);
}

void ttc_virgl_texture_barrier(struct pipe_context *pipe, uint32_t flags) {
   if (pipe->texture_barrier)
      pipe->texture_barrier(pipe, flags);
}

bool ttc_virgl_begin_query(struct pipe_context *pipe, struct pipe_query *q) {
   return pipe->begin_query(pipe, q);
}

bool ttc_virgl_end_query(struct pipe_context *pipe, struct pipe_query *q) {
   return pipe->end_query(pipe, q);
}

bool ttc_virgl_query_result(struct pipe_context *pipe, struct pipe_query *q, uint32_t type,
                            bool wait, void *out) {
   union pipe_query_result result;
   memset(&result, 0, sizeof(result));
   if (!pipe->get_query_result(pipe, q, wait, &result))
      return false;
   struct virgl_host_query_state state = {.query_state = VIRGL_QUERY_STATE_DONE};
   int t = query_type(type);
   switch (t) {
   case PIPE_QUERY_OCCLUSION_PREDICATE:
   case PIPE_QUERY_OCCLUSION_PREDICATE_CONSERVATIVE:
   case PIPE_QUERY_SO_OVERFLOW_PREDICATE:
   case PIPE_QUERY_SO_OVERFLOW_ANY_PREDICATE:
   case PIPE_QUERY_GPU_FINISHED:
      state.result = result.b;
      break;
   case PIPE_QUERY_SO_STATISTICS:
      state.result = result.so_statistics.num_primitives_written;
      break;
   case PIPE_QUERY_TIMESTAMP_DISJOINT:
      state.result = result.timestamp_disjoint.frequency;
      break;
   default:
      state.result = result.u64;
      break;
   }
   // virglrenderer's sizes: timer queries' results are 64-bit, others'
   // 32.
   state.result_size =
      t == PIPE_QUERY_TIMESTAMP || t == PIPE_QUERY_TIME_ELAPSED ? 8 : 4;
   memcpy(out, &state, sizeof(state));
   return true;
}

int ttc_virgl_query_result_qbo(struct pipe_context *pipe, struct pipe_query *q,
                               struct pipe_resource *qbo, bool wait, uint32_t result_type,
                               uint32_t offset, int32_t index) {
   unsigned size = result_type >= PIPE_QUERY_TYPE_I64 ? 8 : 4;
   if (result_type > PIPE_QUERY_TYPE_U64 || !buffer_range_ok(qbo, offset, size))
      return EINVAL;
   pipe->get_query_result_resource(pipe, q, wait ? PIPE_QUERY_WAIT : 0, result_type, index, qbo,
                                   offset);
   return 0;
}
