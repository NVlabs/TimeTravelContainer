// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The renderer's virgl host on Gallium: what the Go decoder (context_unsafe.go)
// calls, after it validated a command's length and resolved its handles,
// to have Zink, Mesa's Gallium driver on Vulkan, execute the command.

#ifndef TTC_VIRGL_H
#define TTC_VIRGL_H

#include <stdbool.h>
#include <stdint.h>

struct pipe_context;
struct pipe_fence_handle;
struct pipe_query;
struct pipe_resource;
struct pipe_sampler_view;

// The process's screen, on the Vulkan that the renderer links.
int ttc_virgl_init(void);
void ttc_virgl_fill_caps(uint32_t set, void *caps, uint32_t size);

// Resources.
struct pipe_resource *ttc_virgl_resource_create(uint32_t target, uint32_t format, uint32_t bind,
                                                uint32_t width, uint32_t height, uint32_t depth,
                                                uint32_t array_size, uint32_t last_level,
                                                uint32_t nr_samples, uint32_t flags);
void ttc_virgl_resource_unref(struct pipe_resource *res);
uint32_t ttc_virgl_resource_target(struct pipe_resource *res);
uint32_t ttc_virgl_resource_width(struct pipe_resource *res, uint32_t level);
uint32_t ttc_virgl_resource_height(struct pipe_resource *res, uint32_t level);
// ttc_virgl_resource_copy copies every level and layer of src to dst, a
// resource of src's parameters, on the GPU.
void ttc_virgl_resource_copy(struct pipe_context *pipe, struct pipe_resource *dst,
                             struct pipe_resource *src);

// Transfers between a resource and the guest's memory of it, data, of size
// bytes, from which the transfer reads (to_host) or into which it writes.
// They return 0, or an errno.
int ttc_virgl_transfer(struct pipe_context *pipe, struct pipe_resource *res, bool to_host,
                       uint32_t level, int32_t x, int32_t y, int32_t z, int32_t w, int32_t h,
                       int32_t d, uint8_t *data, uint64_t size, uint64_t offset, uint32_t stride,
                       uint32_t layer_stride);

// Contexts.
struct pipe_context *ttc_virgl_context_create(void);
// ttc_virgl_context_unbind unbinds the context's state, as Gallium's
// frontends do before they destroy a context and the state's objects.
void ttc_virgl_context_unbind(struct pipe_context *pipe);
void ttc_virgl_context_destroy(struct pipe_context *pipe);
// ttc_virgl_flush returns a fence of what pipe executed, which
// ttc_virgl_fence_wait waits for and releases.
struct pipe_fence_handle *ttc_virgl_flush(struct pipe_context *pipe);
void ttc_virgl_fence_wait(struct pipe_fence_handle *fence);
// ttc_virgl_flush_async has the GPU execute what pipe executed.
void ttc_virgl_flush_async(struct pipe_context *pipe);

// Objects. d is the command's payload, after its header, of n dwords.
void *ttc_virgl_create_blend(struct pipe_context *pipe, const uint32_t *d);
void *ttc_virgl_create_rasterizer(struct pipe_context *pipe, const uint32_t *d);
void *ttc_virgl_create_dsa(struct pipe_context *pipe, const uint32_t *d);
void *ttc_virgl_create_sampler_state(struct pipe_context *pipe, const uint32_t *d);
// elements are the guest's: 4 dwords each. strides are the bound vertex
// buffers' strides.
void *ttc_virgl_create_vertex_elements(struct pipe_context *pipe, const uint32_t *elements,
                                       uint32_t count, const uint32_t *strides);
// ttc_virgl_translate_tgsi returns the tokens of text, a guest's TGSI,
// NUL-terminated, which the guest counted num_tokens of, or NULL, and why
// in err. ttc_virgl_free_tokens frees them.
void *ttc_virgl_translate_tgsi(const char *text, uint32_t num_tokens, char *err,
                               uint32_t errlen);
void ttc_virgl_free_tokens(void *tokens);
// ttc_virgl_dup_tokens returns a copy of tokens, which
// ttc_virgl_free_tokens frees.
void *ttc_virgl_dup_tokens(const void *tokens);
// ttc_virgl_create_shader returns a shader of tokens, or NULL, and why in
// err. so is the shader's stream output, the payload's dwords from its
// count. A vertex shader's inputs of sint_mask are signed integers, and of
// uint_mask unsigned, which TGSI's typeless inputs leave unsaid: Metal
// requires the vertex formats' types. A vertex shader with xfb captures
// its stream output as ttc_virgl_set_xfb says.
void *ttc_virgl_create_shader(struct pipe_context *pipe, uint32_t stage, const void *tokens,
                              const uint32_t *so, uint32_t so_dwords, uint32_t shared_mem,
                              uint32_t sint_mask, uint32_t uint_mask, bool xfb, char *err,
                              uint32_t errlen);
// ttc_virgl_xfb reports whether the renderer emulates transform feedback,
// and the shader buffer slots of its targets.
bool ttc_virgl_xfb(uint32_t *ssbo);
// ttc_virgl_set_xfb sets the parameters of the next draw's capture: the
// draw's first vertex, the vertices of an instance that it captures, the
// vertices that fit the targets, and the targets' offsets in bytes. targets
// are the targets' buffers, or NULL.
void ttc_virgl_set_xfb(struct pipe_context *pipe, const uint32_t *params,
                       struct pipe_resource **targets);
// ttc_virgl_buffer_write writes size bytes of data at offset of the buffer
// res, which the caller checked.
void ttc_virgl_buffer_write(struct pipe_context *pipe, struct pipe_resource *res,
                            uint32_t offset, const void *data, uint32_t size);
// ttc_virgl_format_int_class returns 1 if virgl's format is of signed
// integers, 2 if of unsigned integers, and 0 otherwise.
uint32_t ttc_virgl_format_int_class(uint32_t format);
struct pipe_sampler_view *ttc_virgl_create_sampler_view(struct pipe_context *pipe,
                                                       struct pipe_resource *res,
                                                       uint32_t format, uint32_t target,
                                                       uint32_t val0, uint32_t val1,
                                                       uint32_t swizzle);
struct pipe_query *ttc_virgl_create_query(struct pipe_context *pipe, uint32_t type,
                                          uint32_t index);
void ttc_virgl_delete_object(struct pipe_context *pipe, uint32_t type, uint32_t stage, void *obj);
void ttc_virgl_bind_object(struct pipe_context *pipe, uint32_t type, void *obj);
void ttc_virgl_bind_shader(struct pipe_context *pipe, uint32_t stage, void *shader);

// State.
//
// ttc_virgl_check_surface returns 0 if a surface of res of format, whose
// val0 and val1 are a virgl surface's, is valid, or an errno.
int ttc_virgl_check_surface(struct pipe_resource *res, uint32_t format, uint32_t val0,
                            uint32_t val1);
struct ttc_virgl_surface {
   struct pipe_resource *res;
   uint32_t format;
   uint32_t val0, val1;
   uint32_t nr_samples;
};
int ttc_virgl_set_framebuffer(struct pipe_context *pipe, uint32_t nr_cbufs,
                              const struct ttc_virgl_surface *cbufs,
                              const struct ttc_virgl_surface *zsbuf);
int ttc_virgl_set_framebuffer_no_attach(struct pipe_context *pipe, uint32_t width,
                                        uint32_t height, uint32_t layers, uint32_t samples);
void ttc_virgl_set_viewports(struct pipe_context *pipe, uint32_t start, uint32_t count,
                             const uint32_t *d);
void ttc_virgl_set_scissors(struct pipe_context *pipe, uint32_t start, uint32_t count,
                            const uint32_t *d);
void ttc_virgl_set_vertex_buffers(struct pipe_context *pipe, uint32_t count,
                                  struct pipe_resource **res, const uint32_t *offsets);
void ttc_virgl_set_constants(struct pipe_context *pipe, uint32_t stage, const uint32_t *d,
                             uint32_t n);
int ttc_virgl_set_uniform_buffer(struct pipe_context *pipe, uint32_t stage, uint32_t index,
                                 struct pipe_resource *res, uint32_t offset, uint32_t size);
void ttc_virgl_set_sampler_views(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                 uint32_t count, struct pipe_sampler_view **views);
void ttc_virgl_bind_samplers(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                             uint32_t count, void **samplers);
void ttc_virgl_set_stencil_ref(struct pipe_context *pipe, uint32_t v);
void ttc_virgl_set_blend_color(struct pipe_context *pipe, const uint32_t *d);
void ttc_virgl_set_polygon_stipple(struct pipe_context *pipe, const uint32_t *d);
void ttc_virgl_set_clip_state(struct pipe_context *pipe, const uint32_t *d);
void ttc_virgl_set_sample_mask(struct pipe_context *pipe, uint32_t mask);
void ttc_virgl_set_min_samples(struct pipe_context *pipe, uint32_t n);
void ttc_virgl_set_tess_state(struct pipe_context *pipe, const uint32_t *d);
// The setters that return int return 0, or an errno, of an invalid
// binding, with the context's state unchanged.
int ttc_virgl_set_shader_buffers(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                 uint32_t count, struct pipe_resource **res,
                                 const uint32_t *offsets, const uint32_t *sizes);
int ttc_virgl_set_atomic_buffers(struct pipe_context *pipe, uint32_t start, uint32_t count,
                                 struct pipe_resource **res, const uint32_t *offsets,
                                 const uint32_t *sizes);
int ttc_virgl_set_shader_images(struct pipe_context *pipe, uint32_t stage, uint32_t start,
                                uint32_t count, struct pipe_resource **res,
                                const uint32_t *d);
void ttc_virgl_render_condition(struct pipe_context *pipe, struct pipe_query *q,
                                bool condition, uint32_t mode);

// Operations. Those that return int return 0, or an errno if the command
// is invalid, as a box beyond its resource.
void ttc_virgl_clear(struct pipe_context *pipe, const uint32_t *d);
int ttc_virgl_clear_texture(struct pipe_context *pipe, struct pipe_resource *res,
                            const uint32_t *d);
int ttc_virgl_clear_surface(struct pipe_context *pipe, const struct ttc_virgl_surface *s,
                            const uint32_t *d);
struct ttc_virgl_draw {
   struct pipe_resource *index;
   uint32_t index_size, index_offset;
   struct pipe_resource *indirect;
   struct pipe_resource *indirect_count;
};
int ttc_virgl_draw(struct pipe_context *pipe, const uint32_t *d, uint32_t n,
                   const struct ttc_virgl_draw *draw);
int ttc_virgl_blit(struct pipe_context *pipe, struct pipe_resource *dst,
                   struct pipe_resource *src, const uint32_t *d);
int ttc_virgl_copy_region(struct pipe_context *pipe, struct pipe_resource *dst,
                          struct pipe_resource *src, const uint32_t *d);
int ttc_virgl_inline_write(struct pipe_context *pipe, struct pipe_resource *res,
                           const uint32_t *d, const uint8_t *data, uint32_t size);
int ttc_virgl_launch_grid(struct pipe_context *pipe, const uint32_t *d,
                          struct pipe_resource *indirect);
void ttc_virgl_memory_barrier(struct pipe_context *pipe, uint32_t flags);
void ttc_virgl_texture_barrier(struct pipe_context *pipe, uint32_t flags);
bool ttc_virgl_begin_query(struct pipe_context *pipe, struct pipe_query *q);
bool ttc_virgl_end_query(struct pipe_context *pipe, struct pipe_query *q);
// ttc_virgl_query_result writes, if q's result is ready, or once it is if
// wait, a struct virgl_host_query_state to state, and returns whether it
// did.
bool ttc_virgl_query_result(struct pipe_context *pipe, struct pipe_query *q, uint32_t type,
                            bool wait, void *state);
int ttc_virgl_query_result_qbo(struct pipe_context *pipe, struct pipe_query *q,
                               struct pipe_resource *qbo, bool wait, uint32_t result_type,
                               uint32_t offset, int32_t index);

#endif
