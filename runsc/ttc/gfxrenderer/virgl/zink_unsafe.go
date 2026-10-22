// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

/*
#include <stdlib.h>
#include "virgl.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// zink is the backend of Zink, Mesa's Gallium driver on Vulkan, which
// pipe.c and screen.c call.
type zink struct{}

func ptr(d []uint32) *C.uint32_t {
	if len(d) == 0 {
		return nil
	}
	return (*C.uint32_t)(unsafe.Pointer(&d[0]))
}

func errnoErr(errno C.int) error {
	if errno != 0 {
		return fmt.Errorf("errno %d", int(errno))
	}
	return nil
}

// initZink creates the process's Zink screen, on the Vulkan that the
// renderer links.
func initZink() (backend, error) {
	if C.ttc_virgl_init() != 0 {
		return nil, errors.New("Zink found no usable Vulkan device")
	}
	return zink{}, nil
}

func (zink) fillCaps(set uint32, caps []byte) {
	C.ttc_virgl_fill_caps(C.uint32_t(set), unsafe.Pointer(&caps[0]), C.uint32_t(len(caps)))
}

func (zink) xfb() (bool, uint32) {
	var ssbo C.uint32_t
	emulated := bool(C.ttc_virgl_xfb(&ssbo))
	return emulated, uint32(ssbo)
}

func (zink) newResource(a ResourceArgs) (backendResource, error) {
	p := C.ttc_virgl_resource_create(C.uint32_t(a.Target), C.uint32_t(a.Format), C.uint32_t(a.Bind),
		C.uint32_t(a.Width), C.uint32_t(a.Height), C.uint32_t(a.Depth), C.uint32_t(a.ArraySize),
		C.uint32_t(a.LastLevel), C.uint32_t(a.NrSamples), C.uint32_t(a.Flags))
	if p == nil {
		return nil, errors.New("Zink refused the resource")
	}
	return zinkResource{p}, nil
}

func (zink) newContext() (backendContext, error) {
	pipe := C.ttc_virgl_context_create()
	if pipe == nil {
		return nil, errNoContext
	}
	return zinkContext{pipe}, nil
}

func (zink) formatIntClass(format uint32) uint32 {
	return uint32(C.ttc_virgl_format_int_class(C.uint32_t(format)))
}

// zinkTokens are a shader's TGSI tokens, of Mesa's tgsi_text_translate.
type zinkTokens struct {
	p unsafe.Pointer
}

func (t zinkTokens) dup() shaderCode {
	return zinkTokens{C.ttc_virgl_dup_tokens(t.p)}
}

func (t zinkTokens) free() {
	C.ttc_virgl_free_tokens(t.p)
}

func (zink) parseShader(text string, numTokens uint32) (shaderCode, error) {
	ctext := C.CString(text)
	defer C.free(unsafe.Pointer(ctext))
	var errBuf [256]C.char
	tokens := C.ttc_virgl_translate_tgsi(ctext, C.uint32_t(numTokens), &errBuf[0], C.uint32_t(len(errBuf)))
	if tokens == nil {
		return nil, errors.New(C.GoString(&errBuf[0]))
	}
	return zinkTokens{tokens}, nil
}

// zinkResource is a Gallium resource of Zink's.
type zinkResource struct {
	p *C.struct_pipe_resource
}

func (r zinkResource) unref() {
	C.ttc_virgl_resource_unref(r.p)
}

func (r zinkResource) target() uint32 {
	return uint32(C.ttc_virgl_resource_target(r.p))
}

func (r zinkResource) width(level uint32) uint32 {
	return uint32(C.ttc_virgl_resource_width(r.p, C.uint32_t(level)))
}

func (r zinkResource) height(level uint32) uint32 {
	return uint32(C.ttc_virgl_resource_height(r.p, C.uint32_t(level)))
}

func (r zinkResource) checkSurface(format, val0, val1 uint32) error {
	return errnoErr(C.ttc_virgl_check_surface(r.p, C.uint32_t(format), C.uint32_t(val0), C.uint32_t(val1)))
}

// pipeOf returns r's Gallium resource, or nil.
func pipeOf(r backendResource) *C.struct_pipe_resource {
	if r == nil {
		return nil
	}
	return r.(zinkResource).p
}

// pipes returns rs's Gallium resources, as an array of one more, nil.
func pipes(rs []backendResource) **C.struct_pipe_resource {
	ps := make([]*C.struct_pipe_resource, len(rs)+1)
	for i, r := range rs {
		ps[i] = pipeOf(r)
	}
	return &ps[0]
}

// objPtr returns a Gallium object of Zink's, or nil.
func objPtr(o any) unsafe.Pointer {
	if o == nil {
		return nil
	}
	return o.(unsafe.Pointer)
}

// orNil returns p, or an untyped nil if p is nil, as objects that Zink
// refused are none.
func orNil(p unsafe.Pointer) any {
	if p == nil {
		return nil
	}
	return p
}

// zinkFence is a Gallium fence of Zink's.
type zinkFence struct {
	h *C.struct_pipe_fence_handle
}

func (f zinkFence) wait() {
	C.ttc_virgl_fence_wait(f.h)
}

// zinkContext is a Gallium context of Zink's.
type zinkContext struct {
	pipe *C.struct_pipe_context
}

func (c zinkContext) transfer(r backendResource, a TransferArgs, data []byte) error {
	errno := C.ttc_virgl_transfer(c.pipe, pipeOf(r), C.bool(a.ToHost), C.uint32_t(a.Level),
		C.int32_t(a.X), C.int32_t(a.Y), C.int32_t(a.Z), C.int32_t(a.W), C.int32_t(a.H), C.int32_t(a.D),
		(*C.uint8_t)(unsafe.Pointer(&data[0])), C.uint64_t(len(data)), C.uint64_t(a.Offset),
		C.uint32_t(a.Stride), C.uint32_t(a.LayerStride))
	return errnoErr(errno)
}

func (c zinkContext) copyResource(dst, src backendResource) {
	C.ttc_virgl_resource_copy(c.pipe, pipeOf(dst), pipeOf(src))
}

func (c zinkContext) unbind() {
	C.ttc_virgl_context_unbind(c.pipe)
}

func (c zinkContext) destroy() {
	C.ttc_virgl_context_destroy(c.pipe)
}

func (c zinkContext) flush() backendFence {
	if h := C.ttc_virgl_flush(c.pipe); h != nil {
		return zinkFence{h}
	}
	return nil
}

func (c zinkContext) flushAsync() {
	C.ttc_virgl_flush_async(c.pipe)
}

func (c zinkContext) createBlend(d []uint32) any {
	return orNil(C.ttc_virgl_create_blend(c.pipe, ptr(d)))
}

func (c zinkContext) createRasterizer(d []uint32) any {
	return orNil(C.ttc_virgl_create_rasterizer(c.pipe, ptr(d)))
}

func (c zinkContext) createDSA(d []uint32) any {
	return orNil(C.ttc_virgl_create_dsa(c.pipe, ptr(d)))
}

func (c zinkContext) createSamplerState(d []uint32) any {
	return orNil(C.ttc_virgl_create_sampler_state(c.pipe, ptr(d)))
}

func (c zinkContext) createVertexElements(elements []uint32, strides []uint32) any {
	return orNil(C.ttc_virgl_create_vertex_elements(c.pipe, ptr(elements), C.uint32_t(len(elements)/4), ptr(strides)))
}

func (c zinkContext) createShader(stage uint32, code shaderCode, so []uint32, sharedMem, sintMask, uintMask uint32, xfb bool) (any, error) {
	var errBuf [256]C.char
	p := C.ttc_virgl_create_shader(c.pipe, C.uint32_t(stage), code.(zinkTokens).p, ptr(so), C.uint32_t(len(so)),
		C.uint32_t(sharedMem), C.uint32_t(sintMask), C.uint32_t(uintMask), C.bool(xfb), &errBuf[0], C.uint32_t(len(errBuf)))
	if p == nil {
		return nil, errors.New(C.GoString(&errBuf[0]))
	}
	return p, nil
}

func (c zinkContext) createSamplerView(r backendResource, format, target, val0, val1, swizzle uint32) any {
	v := C.ttc_virgl_create_sampler_view(c.pipe, pipeOf(r), C.uint32_t(format), C.uint32_t(target), C.uint32_t(val0),
		C.uint32_t(val1), C.uint32_t(swizzle))
	return orNil(unsafe.Pointer(v))
}

func (c zinkContext) createQuery(typ, index uint32) any {
	return orNil(unsafe.Pointer(C.ttc_virgl_create_query(c.pipe, C.uint32_t(typ), C.uint32_t(index))))
}

func (c zinkContext) deleteObject(typ, stage uint32, obj any) {
	C.ttc_virgl_delete_object(c.pipe, C.uint32_t(typ), C.uint32_t(stage), objPtr(obj))
}

func (c zinkContext) bindObject(typ uint32, obj any) {
	C.ttc_virgl_bind_object(c.pipe, C.uint32_t(typ), objPtr(obj))
}

func (c zinkContext) bindShader(stage uint32, shader any) {
	C.ttc_virgl_bind_shader(c.pipe, C.uint32_t(stage), objPtr(shader))
}

func (c zinkContext) setXfb(params []uint32, targets []backendResource) {
	C.ttc_virgl_set_xfb(c.pipe, ptr(params), pipes(targets))
}

func (c zinkContext) bufferWrite(r backendResource, offset uint32, data []byte) {
	C.ttc_virgl_buffer_write(c.pipe, pipeOf(r), C.uint32_t(offset), unsafe.Pointer(&data[0]), C.uint32_t(len(data)))
}

func cSurface(s surface) C.struct_ttc_virgl_surface {
	return C.struct_ttc_virgl_surface{res: pipeOf(s.res), format: C.uint32_t(s.format), val0: C.uint32_t(s.val0),
		val1: C.uint32_t(s.val1), nr_samples: C.uint32_t(s.nrSamples)}
}

func (c zinkContext) setFramebuffer(cbufs []surface, zsbuf surface) error {
	cs := make([]C.struct_ttc_virgl_surface, len(cbufs)+1)
	for i, s := range cbufs {
		cs[i] = cSurface(s)
	}
	zs := cSurface(zsbuf)
	return errnoErr(C.ttc_virgl_set_framebuffer(c.pipe, C.uint32_t(len(cbufs)), &cs[0], &zs))
}

func (c zinkContext) setFramebufferNoAttach(width, height, layers, samples uint32) error {
	return errnoErr(C.ttc_virgl_set_framebuffer_no_attach(c.pipe, C.uint32_t(width), C.uint32_t(height),
		C.uint32_t(layers), C.uint32_t(samples)))
}

func (c zinkContext) setViewports(start uint32, d []uint32) {
	C.ttc_virgl_set_viewports(c.pipe, C.uint32_t(start), C.uint32_t(len(d)/6), ptr(d))
}

func (c zinkContext) setScissors(start uint32, d []uint32) {
	C.ttc_virgl_set_scissors(c.pipe, C.uint32_t(start), C.uint32_t(len(d)/2), ptr(d))
}

func (c zinkContext) setVertexBuffers(res []backendResource, offsets []uint32) {
	os := make([]uint32, len(offsets)+1)
	copy(os, offsets)
	C.ttc_virgl_set_vertex_buffers(c.pipe, C.uint32_t(len(res)), pipes(res), ptr(os))
}

func (c zinkContext) setConstants(stage uint32, d []uint32) {
	C.ttc_virgl_set_constants(c.pipe, C.uint32_t(stage), ptr(d), C.uint32_t(len(d)))
}

func (c zinkContext) setUniformBuffer(stage, index uint32, r backendResource, offset, size uint32) error {
	return errnoErr(C.ttc_virgl_set_uniform_buffer(c.pipe, C.uint32_t(stage), C.uint32_t(index), pipeOf(r),
		C.uint32_t(offset), C.uint32_t(size)))
}

func (c zinkContext) setSamplerViews(stage, start uint32, views []any) {
	vs := make([]*C.struct_pipe_sampler_view, len(views)+1)
	for i, v := range views {
		vs[i] = (*C.struct_pipe_sampler_view)(objPtr(v))
	}
	C.ttc_virgl_set_sampler_views(c.pipe, C.uint32_t(stage), C.uint32_t(start), C.uint32_t(len(views)), &vs[0])
}

func (c zinkContext) bindSamplers(stage, start uint32, samplers []any) {
	ss := make([]unsafe.Pointer, len(samplers)+1)
	for i, s := range samplers {
		ss[i] = objPtr(s)
	}
	C.ttc_virgl_bind_samplers(c.pipe, C.uint32_t(stage), C.uint32_t(start), C.uint32_t(len(samplers)), &ss[0])
}

func (c zinkContext) setStencilRef(v uint32) {
	C.ttc_virgl_set_stencil_ref(c.pipe, C.uint32_t(v))
}

func (c zinkContext) setBlendColor(d []uint32) {
	C.ttc_virgl_set_blend_color(c.pipe, ptr(d))
}

func (c zinkContext) setPolygonStipple(d []uint32) {
	C.ttc_virgl_set_polygon_stipple(c.pipe, ptr(d))
}

func (c zinkContext) setClipState(d []uint32) {
	C.ttc_virgl_set_clip_state(c.pipe, ptr(d))
}

func (c zinkContext) setSampleMask(mask uint32) {
	C.ttc_virgl_set_sample_mask(c.pipe, C.uint32_t(mask))
}

func (c zinkContext) setMinSamples(n uint32) {
	C.ttc_virgl_set_min_samples(c.pipe, C.uint32_t(n))
}

func (c zinkContext) setTessState(d []uint32) {
	C.ttc_virgl_set_tess_state(c.pipe, ptr(d))
}

func (c zinkContext) setShaderBuffers(stage, start uint32, res []backendResource, offsets, sizes []uint32) error {
	return errnoErr(C.ttc_virgl_set_shader_buffers(c.pipe, C.uint32_t(stage), C.uint32_t(start), C.uint32_t(len(res)),
		pipes(res), ptr(offsets), ptr(sizes)))
}

func (c zinkContext) setAtomicBuffers(start uint32, res []backendResource, offsets, sizes []uint32) error {
	return errnoErr(C.ttc_virgl_set_atomic_buffers(c.pipe, C.uint32_t(start), C.uint32_t(len(res)), pipes(res),
		ptr(offsets), ptr(sizes)))
}

func (c zinkContext) setShaderImages(stage, start uint32, res []backendResource, d []uint32) error {
	return errnoErr(C.ttc_virgl_set_shader_images(c.pipe, C.uint32_t(stage), C.uint32_t(start), C.uint32_t(len(res)),
		pipes(res), ptr(d)))
}

func (c zinkContext) renderCondition(q any, condition bool, mode uint32) {
	C.ttc_virgl_render_condition(c.pipe, (*C.struct_pipe_query)(objPtr(q)), C.bool(condition), C.uint32_t(mode))
}

func (c zinkContext) clear(d []uint32) {
	C.ttc_virgl_clear(c.pipe, ptr(d))
}

func (c zinkContext) clearTexture(r backendResource, d []uint32) error {
	return errnoErr(C.ttc_virgl_clear_texture(c.pipe, pipeOf(r), ptr(d)))
}

func (c zinkContext) clearSurface(s surface, d []uint32) error {
	cs := cSurface(s)
	return errnoErr(C.ttc_virgl_clear_surface(c.pipe, &cs, ptr(d)))
}

func (c zinkContext) draw(d []uint32, a drawArgs) error {
	draw := C.struct_ttc_virgl_draw{
		index:          pipeOf(a.index),
		index_size:     C.uint32_t(a.indexSize),
		index_offset:   C.uint32_t(a.indexOffset),
		indirect:       pipeOf(a.indirect),
		indirect_count: pipeOf(a.indirectCount),
	}
	return errnoErr(C.ttc_virgl_draw(c.pipe, ptr(d), C.uint32_t(len(d)), &draw))
}

func (c zinkContext) blit(dst, src backendResource, d []uint32) error {
	return errnoErr(C.ttc_virgl_blit(c.pipe, pipeOf(dst), pipeOf(src), ptr(d)))
}

func (c zinkContext) copyRegion(dst, src backendResource, d []uint32) error {
	return errnoErr(C.ttc_virgl_copy_region(c.pipe, pipeOf(dst), pipeOf(src), ptr(d)))
}

func (c zinkContext) inlineWrite(r backendResource, d []uint32, data []byte) error {
	return errnoErr(C.ttc_virgl_inline_write(c.pipe, pipeOf(r), ptr(d), (*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint32_t(len(data))))
}

func (c zinkContext) launchGrid(d []uint32, indirect backendResource) error {
	return errnoErr(C.ttc_virgl_launch_grid(c.pipe, ptr(d), pipeOf(indirect)))
}

func (c zinkContext) memoryBarrier(flags uint32) {
	C.ttc_virgl_memory_barrier(c.pipe, C.uint32_t(flags))
}

func (c zinkContext) textureBarrier(flags uint32) {
	C.ttc_virgl_texture_barrier(c.pipe, C.uint32_t(flags))
}

func (c zinkContext) beginQuery(q any) bool {
	return bool(C.ttc_virgl_begin_query(c.pipe, (*C.struct_pipe_query)(objPtr(q))))
}

func (c zinkContext) endQuery(q any) bool {
	return bool(C.ttc_virgl_end_query(c.pipe, (*C.struct_pipe_query)(objPtr(q))))
}

func (c zinkContext) queryResult(q any, typ uint32, wait bool, state []byte) bool {
	return bool(C.ttc_virgl_query_result(c.pipe, (*C.struct_pipe_query)(objPtr(q)), C.uint32_t(typ), C.bool(wait),
		unsafe.Pointer(&state[0])))
}

func (c zinkContext) queryResultQBO(q any, qbo backendResource, wait bool, resultType, offset uint32, index int32) error {
	return errnoErr(C.ttc_virgl_query_result_qbo(c.pipe, (*C.struct_pipe_query)(objPtr(q)), pipeOf(qbo), C.bool(wait),
		C.uint32_t(resultType), C.uint32_t(offset), C.int32_t(index)))
}
