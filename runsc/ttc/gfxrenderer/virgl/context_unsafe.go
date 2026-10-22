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
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"unsafe"
)

// Context is a virgl context: what one guest process does with the GPU by
// virgl. Its sub-contexts, one for each of the process's OpenGL contexts,
// are Gallium contexts of Zink's.
type Context struct {
	id uint32

	mu sync.Mutex
	// resources are the resources that the context may use.
	resources map[uint32]*Resource
	subs      map[uint32]*subContext
	sub       *subContext
	// pending are queries whose results the guest asked for before they
	// were ready, which fences write once they are.
	pending []*object
	// inflight counts the fences that Flush returned and that Wait has yet
	// to wait for: the Gallium contexts, whose fences they are, outlive
	// them.
	inflight  int
	destroyed bool
	// loggedCapture is whether the context logged a draw whose transform
	// feedback the renderer cannot capture.
	loggedCapture bool
}

// subContext is a sub-context: a Gallium context, the guest's objects of
// it, and what of its state the decoder keeps.
//
// Gallium's drivers keep no references to much of their state: to the
// buffers bound to slots, which Zink holds until a call replaces them in
// their slots, and to the state objects bound, which they free when they
// are deleted. A sub-context keeps the resources that Zink holds, and an
// object that the guest destroys while bound lives on until it is unbound.
type subContext struct {
	pipe    *C.struct_pipe_context
	objects map[uint32]*object
	// zombies are objects that the guest destroyed while they were bound.
	zombies map[*object]struct{}

	// The objects bound: blend, rasterizer and depth-stencil-alpha states,
	// which the guest unbinds by binding the sub-context's defaults, and
	// shaders, samplers, the vertex elements whose Gallium state is bound,
	// and the query of the render condition.
	cso         [objectDSA + 1]*object
	defaults    [objectDSA + 1]*object
	shaders     [maxShaderStages]*object
	samplers    [maxShaderStages][maxSamplers]*object
	velemsBound *object
	velemsCSO   unsafe.Pointer
	cond        *object
	// vsBound is the vertex shader whose variant, vsCSO, is bound: draws
	// bind the variant of the guest's vertex shader, shaders[stageVertex],
	// for the vertex elements' types.
	vsBound *object
	vsCSO   unsafe.Pointer

	// fbAttached is whether the framebuffer has attachments, whose size
	// SET_FRAMEBUFFER_STATE_NO_ATTACH, which guests send after each
	// framebuffer, leaves as it is.
	fbAttached bool

	// The stream output targets bound, whose transform feedback the
	// renderer captures itself (see prepareCapture), and the active queries
	// that it counts itself, of primitives and of transform feedback.
	soTargets      [maxStreamoutTargets]*object
	soCount        int
	countedQueries map[*object]struct{}

	// The guest's vertex elements and vertex buffers, which draws bind:
	// Gallium's vertex elements, not its vertex buffers, carry the
	// buffers' strides.
	velems   *object
	vbs      [maxVertexBuffers]vertexBuffer
	numVBs   int
	vbsDirty bool

	// held are the resources that Zink holds, by slot.
	held map[slot]*Resource

	ib       *Resource
	ibSize   uint32
	ibOffset uint32

	// What the decoder keeps of the rest of the state, which a fork's
	// clone sets again (see clone_unsafe.go): the sampler views and the
	// framebuffer's surfaces bound, and of what binds no object, what the
	// commands that set it last carried.
	views [maxShaderStages][maxSamplerViews]*object
	// fb are the framebuffer's depth-stencil surface, and then its color
	// buffers, fbCbufs of them.
	fb         [1 + maxColorBufs]*object
	fbCbufs    uint32
	fbNoAttach []uint32
	viewports  [maxViewports][]uint32
	scissors   [maxViewports][]uint32
	constants  [maxShaderStages][]uint32
	// ranges are, of the slots that hold resources, the ranges of the
	// buffers, and the images' views of their resources: what the commands
	// that bound them carried but the resources.
	ranges map[slot][]uint32
	// scalars are what the commands that set the state of no object or
	// resource carried, by command.
	scalars map[uint32][]uint32
	// condition is the render condition's condition and mode.
	condition [2]uint32
}

type vertexBuffer struct {
	res    *Resource
	stride uint32
	offset uint32
}

// slot is a slot of a Gallium context's state that holds a resource.
type slot struct {
	kind, stage, index uint32
}

const (
	slotVertexBuffer = iota
	slotConstantBuffer
	slotShaderBuffer
	slotShaderImage
	slotAtomicBuffer
)

// object is a virgl object of a sub-context.
type object struct {
	typ   uint32
	stage uint32
	ptr   unsafe.Pointer
	// binds counts the slots of the sub-context's state that hold the
	// object, and deleted is whether the guest destroyed it.
	binds   int
	deleted bool
	// res is the resource that a surface, sampler view, query or stream
	// output target is of.
	res  *Resource
	surf C.struct_ttc_virgl_surface
	// args are what the command that created a state object, a sampler
	// view or a query carried, of which a clone creates it again.
	args []uint32
	// elements and csos are vertex elements': the guest's elements, and a
	// Gallium state of them for each set of strides. sintMask and uintMask
	// are the elements of signed and unsigned integers.
	elements []uint32
	csos     map[string]unsafe.Pointer
	sintMask uint32
	uintMask uint32
	// A shader whose text comes in parts.
	text      []byte
	filled    uint32
	numTokens uint32
	so        []uint32
	shared    uint32
	// A shader's TGSI, which a clone compiles again, and a vertex shader's
	// variants (see bindVertexShader).
	tokens   unsafe.Pointer
	variants map[variantKey]unsafe.Pointer
	// A stream output target's range of its buffer, and the bytes and the
	// vertices captured to it.
	soOffset uint32
	soSize   uint32
	soFilled uint32
	soVerts  uint32
	// A query that the renderer counts itself, and its counts.
	counted   bool
	generated uint64
	emitted   uint64
	needed    uint64
	// A query's type, the sub-context that created it, and whether it is
	// active: begun and not ended.
	qtype  uint32
	sub    *subContext
	active bool
}

var (
	errNoContext = errors.New("Zink refused a context")
	errDestroyed = errors.New("the context is destroyed")
)

// NewContext returns a context of id.
func NewContext(id uint32) (*Context, error) {
	if err := Init(); err != nil {
		return nil, err
	}
	c := &Context{
		id:        id,
		resources: make(map[uint32]*Resource),
		subs:      make(map[uint32]*subContext),
	}
	sub, err := c.newSubContext()
	if err != nil {
		return nil, err
	}
	c.subs[0] = sub
	c.sub = sub
	return c, nil
}

func (c *Context) newSubContext() (*subContext, error) {
	pipe := C.ttc_virgl_context_create()
	if pipe == nil {
		return nil, errNoContext
	}
	sub := &subContext{
		pipe:    pipe,
		objects: make(map[uint32]*object),
		zombies: make(map[*object]struct{}),
		held:    make(map[slot]*Resource),

		countedQueries: make(map[*object]struct{}),
		ranges:         make(map[slot][]uint32),
		scalars:        make(map[uint32][]uint32),
	}
	// Zink draws with the blend, rasterizer and depth-stencil-alpha states
	// bound, which a guest that binds none would leave unbound: OpenGL's
	// defaults, as near as matters.
	var blend [11]uint32
	for i := 3; i < len(blend); i++ {
		blend[i] = 0xf << 27 // colormask
	}
	rast := [9]uint32{
		1: 1<<1 | 1<<15 | 1<<29, // depth_clip, front_ccw, half_pixel_center
		2: 0x3f800000,           // point_size 1.0
		5: 0x3f800000,           // line_width 1.0
	}
	var dsa [5]uint32
	sub.defaults[objectBlend] = &object{typ: objectBlend, ptr: C.ttc_virgl_create_blend(pipe, ptr(blend[:]))}
	sub.defaults[objectRasterizer] = &object{typ: objectRasterizer, ptr: C.ttc_virgl_create_rasterizer(pipe, ptr(rast[:]))}
	sub.defaults[objectDSA] = &object{typ: objectDSA, ptr: C.ttc_virgl_create_dsa(pipe, ptr(dsa[:]))}
	for _, typ := range []uint32{objectBlend, objectRasterizer, objectDSA} {
		o := sub.defaults[typ]
		if o.ptr == nil {
			c.destroySubLocked(sub)
			return nil, fmt.Errorf("Zink refused a default state of type %d", typ)
		}
		C.ttc_virgl_bind_object(pipe, C.uint32_t(typ), o.ptr)
		c.rebind(sub, &sub.cso[typ], o)
	}
	return sub, nil
}

// Attach lets the context use r.
func (c *Context) Attach(r *Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed || c.resources[r.id] != nil {
		return
	}
	r.incRef()
	c.resources[r.id] = r
}

// Detach ends the context's use of resource id.
func (c *Context) Detach(id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.resources[id]; r != nil {
		delete(c.resources, id)
		r.DecRef()
	}
}

// Transfer executes a transfer between r and the guest's memory of it, in
// order with what the context executed.
func (c *Context) Transfer(r *Resource, a TransferArgs) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return errDestroyed
	}
	return transfer(c.sub.pipe, r, a)
}

// Fence is a fence of what a context executed before Flush returned it.
type Fence struct {
	c       *Context
	handles []*C.struct_pipe_fence_handle
}

// Flush has the GPU execute what the context executed, and returns a fence
// of it.
func (c *Context) Flush() *Fence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return &Fence{}
	}
	f := &Fence{c: c}
	for _, sub := range c.subs {
		if h := C.ttc_virgl_flush(sub.pipe); h != nil {
			f.handles = append(f.handles, h)
		}
	}
	c.inflight++
	return f
}

// Wait waits until what f fences is done, and then writes the results of
// the context's queries that the guest asked for and that are then ready,
// so that a guest that waits for the fence finds them.
func (f *Fence) Wait() {
	for _, h := range f.handles {
		C.ttc_virgl_fence_wait(h)
	}
	f.handles = nil
	c := f.c
	if c == nil {
		return
	}
	f.c = nil
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	if !c.destroyed {
		c.checkQueriesLocked()
	} else if c.inflight == 0 {
		c.destroySubsLocked()
	}
}

// Destroy destroys the context and its objects, once its fences are done.
func (c *Context) Destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return
	}
	c.destroyed = true
	c.pending = nil
	if c.inflight == 0 {
		c.destroySubsLocked()
	}
}

func (c *Context) destroySubsLocked() {
	for _, sub := range c.subs {
		c.destroySubLocked(sub)
	}
	c.subs = nil
	c.sub = nil
	for _, r := range c.resources {
		r.DecRef()
	}
	c.resources = nil
}

// destroySubLocked unbinds sub's state, as Gallium's frontends do before
// they destroy a context, frees its objects, and destroys it.
func (c *Context) destroySubLocked(sub *subContext) {
	C.ttc_virgl_context_unbind(sub.pipe)
	for _, o := range sub.objects {
		c.freeObjectLocked(sub, o)
	}
	for o := range sub.zombies {
		c.freeObjectLocked(sub, o)
	}
	for _, o := range sub.defaults {
		if o != nil {
			c.freeObjectLocked(sub, o)
		}
	}
	C.ttc_virgl_context_destroy(sub.pipe)
	sub.pipe = nil
	sub.objects = nil
	sub.zombies = nil
	for _, r := range sub.held {
		r.DecRef()
	}
	sub.held = nil
	for i := range sub.vbs {
		release(sub.vbs[i].res)
	}
	release(sub.ib)
}

// rebind records that a slot of sub's state, *s, holds o, after the call
// that bound it, and frees what the slot held if the guest destroyed it
// and no other slot holds it.
func (c *Context) rebind(sub *subContext, s **object, o *object) {
	old := *s
	*s = o
	if o != nil {
		o.binds++
	}
	if old != nil {
		old.binds--
		if old.binds == 0 && old.deleted {
			c.freeObjectLocked(sub, old)
		}
	}
}

// unhold undoes holds of slots of kind and stage from start, whose
// previous holdings were old, where the call that would have replaced them
// failed, and left Zink's state as it was.
func (sub *subContext) unhold(kind, stage, start uint32, old []*Resource) {
	for i, r := range old {
		release(sub.hold(slot{kind, stage, start + uint32(i)}, r), r)
	}
}

// hold records that Zink holds r in slot s, and returns what s held, which
// the caller releases once the call that replaced it returned.
func (sub *subContext) hold(s slot, r *Resource) *Resource {
	old := sub.held[s]
	if r != nil {
		r.incRef()
		sub.held[s] = r
	} else {
		delete(sub.held, s)
	}
	return old
}

// setRange records what the command that bound r to slot s carried but r,
// d, or that s holds none if r is nil.
func (sub *subContext) setRange(s slot, r *Resource, d ...uint32) {
	if r == nil {
		delete(sub.ranges, s)
		return
	}
	sub.ranges[s] = append(sub.ranges[s][:0], d...)
}

func release(rs ...*Resource) {
	for _, r := range rs {
		if r != nil {
			r.DecRef()
		}
	}
}

// destroyObjectLocked destroys the guest's object h, which lives on while
// it is bound.
func (c *Context) destroyObjectLocked(sub *subContext, h uint32) {
	o := sub.objects[h]
	if o == nil {
		return
	}
	delete(sub.objects, h)
	o.deleted = true
	if sub.velems == o {
		sub.velems = nil
	}
	if o.binds > 0 {
		sub.zombies[o] = struct{}{}
		return
	}
	c.freeObjectLocked(sub, o)
}

// freeObjectLocked deletes o's Gallium state, which sub's state no longer
// holds.
func (c *Context) freeObjectLocked(sub *subContext, o *object) {
	delete(sub.zombies, o)
	delete(sub.countedQueries, o)
	if o.variants != nil {
		// o.ptr is one of them.
		for _, v := range o.variants {
			C.ttc_virgl_delete_object(sub.pipe, objectShader, stageVertex, v)
		}
		o.variants = nil
		o.ptr = nil
	}
	if o.tokens != nil {
		C.ttc_virgl_free_tokens(o.tokens)
		o.tokens = nil
	}
	if o.ptr != nil {
		C.ttc_virgl_delete_object(sub.pipe, C.uint32_t(o.typ), C.uint32_t(o.stage), o.ptr)
		o.ptr = nil
	}
	for _, cso := range o.csos {
		C.ttc_virgl_delete_object(sub.pipe, objectVertexElements, 0, cso)
	}
	o.csos = nil
	if o.res != nil {
		o.res.DecRef()
		o.res = nil
	}
	for i, p := range c.pending {
		if p == o {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			break
		}
	}
}

// Submit executes a command stream that the guest submitted.
func (c *Context) Submit(buf []byte) error {
	words := make([]uint32, len(buf)/4)
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(buf[4*i:])
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return errDestroyed
	}
	for off := 0; off < len(words); {
		hdr := words[off]
		length := int(hdr >> 16)
		cmd := hdr & 0xff
		obj := (hdr >> 8) & 0xff
		if off+1+length > len(words) {
			return fmt.Errorf("command %d at dword %d: %d dwords, past the stream's end", cmd, off, length)
		}
		if err := c.execute(cmd, obj, words[off+1:off+1+length]); err != nil {
			// The rest of the stream may depend on the command; virglrenderer
			// stops too.
			return fmt.Errorf("command %d (object %d) at dword %d: %w", cmd, obj, off, err)
		}
		off += 1 + length
	}
	return nil
}

func ptr(d []uint32) *C.uint32_t {
	if len(d) == 0 {
		return nil
	}
	return (*C.uint32_t)(unsafe.Pointer(&d[0]))
}

func errLen(n int) error {
	return fmt.Errorf("%d dwords", n)
}

func errnoErr(errno C.int) error {
	if errno != 0 {
		return fmt.Errorf("errno %d", int(errno))
	}
	return nil
}

func (c *Context) resource(h uint32) (*Resource, error) {
	if h == 0 {
		return nil, nil
	}
	r := c.resources[h]
	if r == nil {
		return nil, fmt.Errorf("resource %d is not the context's", h)
	}
	return r, nil
}

// buffer returns resource h, which is a buffer, or nil if h is 0: Zink
// binds a texture of no VkBuffer as no buffer.
func (c *Context) buffer(h uint32) (*Resource, error) {
	r, err := c.resource(h)
	if err == nil && r != nil && C.ttc_virgl_resource_target(r.pipe) != targetBuffer {
		err = fmt.Errorf("resource %d is no buffer", h)
	}
	return r, err
}

// mustResource returns resource h, which is not 0.
func (c *Context) mustResource(h uint32) (*Resource, error) {
	r, err := c.resource(h)
	if err == nil && r == nil {
		err = errors.New("resource 0")
	}
	return r, err
}

func pipeOf(r *Resource) *C.struct_pipe_resource {
	if r == nil {
		return nil
	}
	return r.pipe
}

func pipes(rs []*Resource) **C.struct_pipe_resource {
	ps := make([]*C.struct_pipe_resource, len(rs)+1)
	for i, r := range rs {
		ps[i] = pipeOf(r)
	}
	return &ps[0]
}

// lookup returns the sub-context's object h of type typ, or nil if h is 0.
func (c *Context) lookup(h, typ uint32) (*object, error) {
	if h == 0 {
		return nil, nil
	}
	o := c.sub.objects[h]
	if o == nil || o.typ != typ || o.ptr == nil && !o.renderers() {
		return nil, fmt.Errorf("no object %d of type %d", h, typ)
	}
	return o, nil
}

// renderers reports whether o is an object of the renderer's, with no
// Gallium state: vertex elements, which draws make Gallium's of, stream
// output targets, and queries that the renderer counts.
func (o *object) renderers() bool {
	return o.typ == objectVertexElements || o.typ == objectStreamoutTarget || o.counted
}

// mustLookup returns the sub-context's object h of type typ, which is not
// 0.
func (c *Context) mustLookup(h, typ uint32) (*object, error) {
	o, err := c.lookup(h, typ)
	if err == nil && o == nil {
		err = fmt.Errorf("object 0 of type %d", typ)
	}
	return o, err
}

func objPtr(o *object) unsafe.Pointer {
	if o == nil {
		return nil
	}
	return o.ptr
}

func (c *Context) surface(h uint32) (*object, error) {
	o := c.sub.objects[h]
	if o == nil || o.typ != objectSurface && o.typ != objectMSAASurface {
		return nil, fmt.Errorf("no surface %d", h)
	}
	return o, nil
}

// surfaceOrNil is surface, or nil if h is 0.
func (c *Context) surfaceOrNil(h uint32) (*object, error) {
	if h == 0 {
		return nil, nil
	}
	return c.surface(h)
}

func (c *Context) execute(cmd, obj uint32, d []uint32) error {
	sub := c.sub
	pipe := sub.pipe
	n := len(d)
	switch cmd {
	case cmdNop, cmdEndTransfers, cmdSetTweaks, cmdSetDebugFlags, cmdSendStringMarker, cmdLinkShader:
		return nil

	case cmdCreateObject:
		return c.createObject(obj, d)

	case cmdBindObject:
		if n != 1 {
			return errLen(n)
		}
		switch obj {
		case objectBlend, objectRasterizer, objectDSA:
			o, err := c.lookup(d[0], obj)
			if err != nil {
				return err
			}
			if o == nil {
				o = sub.defaults[obj]
			}
			C.ttc_virgl_bind_object(pipe, C.uint32_t(obj), o.ptr)
			c.rebind(sub, &sub.cso[obj], o)
		case objectVertexElements:
			o, err := c.lookup(d[0], obj)
			if err != nil {
				return err
			}
			sub.velems = o
		default:
			return fmt.Errorf("binding object type %d", obj)
		}
		return nil

	case cmdDestroyObject:
		if n != 1 {
			return errLen(n)
		}
		c.destroyObjectLocked(sub, d[0])
		return nil

	case cmdSetViewportState:
		if n < 1 || (n-1)%6 != 0 {
			return errLen(n)
		}
		count := uint32((n - 1) / 6)
		if count > maxViewports || d[0] > maxViewports-count {
			return fmt.Errorf("viewports %d+%d", d[0], count)
		}
		C.ttc_virgl_set_viewports(pipe, C.uint32_t(d[0]), C.uint32_t(count), ptr(d[1:]))
		for i := uint32(0); i < count; i++ {
			v := &sub.viewports[d[0]+i]
			*v = append((*v)[:0], d[1+6*i:7+6*i]...)
		}
		return nil

	case cmdSetScissorState:
		if n < 1 || (n-1)%2 != 0 {
			return errLen(n)
		}
		count := uint32((n - 1) / 2)
		if count > maxViewports || d[0] > maxViewports-count {
			return fmt.Errorf("scissors %d+%d", d[0], count)
		}
		C.ttc_virgl_set_scissors(pipe, C.uint32_t(d[0]), C.uint32_t(count), ptr(d[1:]))
		for i := uint32(0); i < count; i++ {
			v := &sub.scissors[d[0]+i]
			*v = append((*v)[:0], d[1+2*i:3+2*i]...)
		}
		return nil

	case cmdSetFramebufferState:
		if n < 2 || d[0] > maxColorBufs || n != 2+int(d[0]) {
			return errLen(n)
		}
		var cbufs [maxColorBufs]C.struct_ttc_virgl_surface
		var surfs [1 + maxColorBufs]*object
		for i := 0; i < int(d[0]); i++ {
			o, err := c.surfaceOrNil(d[2+i])
			if err != nil {
				return err
			}
			if o != nil {
				cbufs[i] = o.surf
				surfs[1+i] = o
			}
		}
		zs, err := c.surfaceOrNil(d[1])
		if err != nil {
			return err
		}
		surfs[0] = zs
		var zsbuf C.struct_ttc_virgl_surface
		attached := zs != nil
		if zs != nil {
			zsbuf = zs.surf
		}
		for _, cb := range cbufs {
			attached = attached || cb.res != nil
		}
		// Zink keeps references to the framebuffer's resources.
		if err := errnoErr(C.ttc_virgl_set_framebuffer(pipe, C.uint32_t(d[0]), &cbufs[0], &zsbuf)); err != nil {
			return err
		}
		sub.fbAttached = attached
		for i, o := range surfs {
			c.rebind(sub, &sub.fb[i], o)
		}
		sub.fbCbufs = d[0]
		sub.fbNoAttach = nil
		return nil

	case cmdSetFramebufferNoAttach:
		if n != 2 {
			return errLen(n)
		}
		// Like virglrenderer's, this sets the size of a framebuffer without
		// attachments, which OpenGL's default framebuffer parameters are.
		if sub.fbAttached {
			return nil
		}
		if err := errnoErr(C.ttc_virgl_set_framebuffer_no_attach(pipe, C.uint32_t(d[0]&0xffff), C.uint32_t(d[0]>>16),
			C.uint32_t(d[1]&0xffff), C.uint32_t((d[1]>>16)&0xff))); err != nil {
			return err
		}
		sub.fbNoAttach = []uint32{d[0], d[1]}
		return nil

	case cmdSetVertexBuffers:
		if n%3 != 0 || n/3 > maxVertexBuffers {
			return errLen(n)
		}
		count := n / 3
		var vbs [maxVertexBuffers]vertexBuffer
		for i := 0; i < count; i++ {
			r, err := c.buffer(d[3*i+2])
			if err != nil {
				return err
			}
			vbs[i] = vertexBuffer{res: r, stride: d[3*i], offset: d[3*i+1]}
		}
		for i := range vbs {
			if vbs[i].res != nil {
				vbs[i].res.incRef()
			}
		}
		old := sub.vbs
		sub.vbs = vbs
		sub.numVBs = count
		sub.vbsDirty = true
		for i := range old {
			release(old[i].res)
		}
		return nil

	case cmdSetIndexBuffer:
		if n != 1 && n != 3 {
			return errLen(n)
		}
		r, err := c.buffer(d[0])
		if err != nil {
			return err
		}
		if r != nil {
			r.incRef()
		}
		release(sub.ib)
		sub.ib, sub.ibSize, sub.ibOffset = r, 0, 0
		if n == 3 {
			sub.ibSize, sub.ibOffset = d[1], d[2]
		}
		return nil

	case cmdSetConstantBuffer:
		if n < 2 || d[0] >= maxShaderStages || n-2 > maxConstantDwords {
			return errLen(n)
		}
		// The constants take the slot of uniform buffer 0.
		old := sub.hold(slot{slotConstantBuffer, d[0], 0}, nil)
		C.ttc_virgl_set_constants(pipe, C.uint32_t(d[0]), ptr(d[2:]), C.uint32_t(n-2))
		release(old)
		delete(sub.ranges, slot{slotConstantBuffer, d[0], 0})
		sub.constants[d[0]] = append(sub.constants[d[0]][:0], d[2:]...)
		return nil

	case cmdSetUniformBuffer:
		if n != 5 || d[0] >= maxShaderStages || d[1] >= maxConstantBuffers {
			return errLen(n)
		}
		r, err := c.resource(d[4])
		if err != nil {
			return err
		}
		old := sub.hold(slot{slotConstantBuffer, d[0], d[1]}, r)
		if err := errnoErr(C.ttc_virgl_set_uniform_buffer(pipe, C.uint32_t(d[0]), C.uint32_t(d[1]), pipeOf(r),
			C.uint32_t(d[2]), C.uint32_t(d[3]))); err != nil {
			sub.unhold(slotConstantBuffer, d[0], d[1], []*Resource{old})
			return err
		}
		release(old)
		sub.setRange(slot{slotConstantBuffer, d[0], d[1]}, r, d[2], d[3])
		if d[1] == 0 {
			sub.constants[d[0]] = nil
		}
		return nil

	case cmdSetSamplerViews:
		if n < 2 || d[0] >= maxShaderStages {
			return errLen(n)
		}
		count := uint32(n - 2)
		if count > maxSamplerViews || d[1] > maxSamplerViews-count {
			return fmt.Errorf("sampler views %d+%d", d[1], count)
		}
		// Zink keeps references to the views that it binds.
		views := make([]*C.struct_pipe_sampler_view, count+1)
		objs := make([]*object, count)
		for i := uint32(0); i < count; i++ {
			o, err := c.lookup(d[2+i], objectSamplerView)
			if err != nil {
				return err
			}
			objs[i] = o
			views[i] = (*C.struct_pipe_sampler_view)(objPtr(o))
		}
		C.ttc_virgl_set_sampler_views(pipe, C.uint32_t(d[0]), C.uint32_t(d[1]), C.uint32_t(count), &views[0])
		for i, o := range objs {
			c.rebind(sub, &sub.views[d[0]][d[1]+uint32(i)], o)
		}
		return nil

	case cmdBindSamplerStates:
		if n < 2 || d[0] >= maxShaderStages {
			return errLen(n)
		}
		count := uint32(n - 2)
		if count > maxSamplers || d[1] > maxSamplers-count {
			return fmt.Errorf("samplers %d+%d", d[1], count)
		}
		objs := make([]*object, count)
		states := make([]unsafe.Pointer, count+1)
		for i := uint32(0); i < count; i++ {
			o, err := c.lookup(d[2+i], objectSamplerState)
			if err != nil {
				return err
			}
			objs[i] = o
			states[i] = objPtr(o)
		}
		C.ttc_virgl_bind_samplers(pipe, C.uint32_t(d[0]), C.uint32_t(d[1]), C.uint32_t(count), &states[0])
		for i, o := range objs {
			c.rebind(sub, &sub.samplers[d[0]][d[1]+uint32(i)], o)
		}
		return nil

	case cmdSetStencilRef:
		if n != 1 {
			return errLen(n)
		}
		C.ttc_virgl_set_stencil_ref(pipe, C.uint32_t(d[0]))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetBlendColor:
		if n != 4 {
			return errLen(n)
		}
		C.ttc_virgl_set_blend_color(pipe, ptr(d))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetPolygonStipple:
		if n != 32 {
			return errLen(n)
		}
		C.ttc_virgl_set_polygon_stipple(pipe, ptr(d))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetClipState:
		if n != 32 {
			return errLen(n)
		}
		C.ttc_virgl_set_clip_state(pipe, ptr(d))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetSampleMask:
		if n != 1 {
			return errLen(n)
		}
		C.ttc_virgl_set_sample_mask(pipe, C.uint32_t(d[0]))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetMinSamples:
		if n != 1 {
			return errLen(n)
		}
		C.ttc_virgl_set_min_samples(pipe, C.uint32_t(d[0]))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetTessState:
		if n != 6 {
			return errLen(n)
		}
		C.ttc_virgl_set_tess_state(pipe, ptr(d))
		sub.scalars[cmd] = append(sub.scalars[cmd][:0], d...)
		return nil

	case cmdSetShaderBuffers:
		if n < 2 || d[0] >= maxShaderStages || (n-2)%3 != 0 {
			return errLen(n)
		}
		count := uint32((n - 2) / 3)
		if count == 0 {
			return nil
		}
		if d[1] > maxShaderBuffers || count > maxShaderBuffers-d[1] {
			return fmt.Errorf("shader buffers %d+%d", d[1], count)
		}
		res, offsets, sizes, err := c.buffers(d[2:], count)
		if err != nil {
			return err
		}
		old := c.holdRange(slotShaderBuffer, d[0], d[1], res)
		if err := errnoErr(C.ttc_virgl_set_shader_buffers(pipe, C.uint32_t(d[0]), C.uint32_t(d[1]), C.uint32_t(count),
			pipes(res), &offsets[0], &sizes[0])); err != nil {
			sub.unhold(slotShaderBuffer, d[0], d[1], old)
			return err
		}
		release(old...)
		for i, r := range res {
			sub.setRange(slot{slotShaderBuffer, d[0], d[1] + uint32(i)}, r, uint32(offsets[i]), uint32(sizes[i]))
		}
		return nil

	case cmdSetAtomicBuffers:
		if n < 1 || (n-1)%3 != 0 {
			return errLen(n)
		}
		count := uint32((n - 1) / 3)
		if count == 0 {
			return nil
		}
		if d[0] > maxAtomicBuffers || count > maxAtomicBuffers-d[0] {
			return fmt.Errorf("atomic buffers %d+%d", d[0], count)
		}
		res, offsets, sizes, err := c.buffers(d[1:], count)
		if err != nil {
			return err
		}
		old := c.holdRange(slotAtomicBuffer, 0, d[0], res)
		if err := errnoErr(C.ttc_virgl_set_atomic_buffers(pipe, C.uint32_t(d[0]), C.uint32_t(count), pipes(res), &offsets[0], &sizes[0])); err != nil {
			sub.unhold(slotAtomicBuffer, 0, d[0], old)
			return err
		}
		release(old...)
		for i, r := range res {
			sub.setRange(slot{slotAtomicBuffer, 0, d[0] + uint32(i)}, r, uint32(offsets[i]), uint32(sizes[i]))
		}
		return nil

	case cmdSetShaderImages:
		if n < 2 || d[0] >= maxShaderStages || (n-2)%5 != 0 {
			return errLen(n)
		}
		count := uint32((n - 2) / 5)
		if count == 0 {
			return nil
		}
		if d[1] > maxShaderImages || count > maxShaderImages-d[1] {
			return fmt.Errorf("shader images %d+%d", d[1], count)
		}
		res := make([]*Resource, count)
		for i := uint32(0); i < count; i++ {
			r, err := c.resource(d[2+5*i+4])
			if err != nil {
				return err
			}
			res[i] = r
		}
		old := c.holdRange(slotShaderImage, d[0], d[1], res)
		if err := errnoErr(C.ttc_virgl_set_shader_images(pipe, C.uint32_t(d[0]), C.uint32_t(d[1]), C.uint32_t(count), pipes(res), ptr(d[2:]))); err != nil {
			sub.unhold(slotShaderImage, d[0], d[1], old)
			return err
		}
		release(old...)
		for i, r := range res {
			sub.setRange(slot{slotShaderImage, d[0], d[1] + uint32(i)}, r, d[2+5*i:6+5*i]...)
		}
		return nil

	case cmdSetStreamoutTargets:
		if n < 1 || n-1 > maxStreamoutTargets {
			return errLen(n)
		}
		var targets [maxStreamoutTargets]*object
		for i := 0; i < n-1; i++ {
			o, err := c.lookup(d[1+i], objectStreamoutTarget)
			if err != nil {
				return err
			}
			targets[i] = o
		}
		// A target that the guest binds afresh, rather than to append to,
		// is empty, as Gallium's offset of 0 makes it.
		for i, o := range targets {
			if o != nil && d[0]&(1<<i) == 0 {
				o.soFilled, o.soVerts = 0, 0
			}
			c.rebind(sub, &sub.soTargets[i], o)
		}
		sub.soCount = n - 1
		return nil

	case cmdSetRenderCondition:
		if n != 3 {
			return errLen(n)
		}
		o, err := c.lookup(d[0], objectQuery)
		if err != nil {
			return err
		}
		if o != nil && o.counted || d[2] > renderCondByRegionNoWait {
			// OpenGL conditions rendering on occlusion queries.
			return fmt.Errorf("a render condition of mode %d", d[2])
		}
		C.ttc_virgl_render_condition(pipe, (*C.struct_pipe_query)(objPtr(o)), C.bool(d[1]&1 != 0), C.uint32_t(d[2]))
		c.rebind(sub, &sub.cond, o)
		sub.condition = [2]uint32{d[1], d[2]}
		return nil

	case cmdClear:
		if n != 8 {
			return errLen(n)
		}
		C.ttc_virgl_clear(pipe, ptr(d))
		return nil

	case cmdClearTexture:
		if n != 12 {
			return errLen(n)
		}
		r, err := c.mustResource(d[0])
		if err != nil {
			return err
		}
		return errnoErr(C.ttc_virgl_clear_texture(pipe, r.pipe, ptr(d)))

	case cmdClearSurface:
		if n != 10 {
			return errLen(n)
		}
		o, err := c.surface(d[1])
		if err != nil {
			return err
		}
		return errnoErr(C.ttc_virgl_clear_surface(pipe, &o.surf, ptr(d)))

	case cmdDrawVBO:
		return c.draw(d)

	case cmdBlit:
		if n != 21 {
			return errLen(n)
		}
		dst, err := c.mustResource(d[3])
		if err != nil {
			return err
		}
		src, err := c.mustResource(d[12])
		if err != nil {
			return err
		}
		return errnoErr(C.ttc_virgl_blit(pipe, dst.pipe, src.pipe, ptr(d)))

	case cmdResourceCopyRegion:
		if n != 13 {
			return errLen(n)
		}
		dst, err := c.mustResource(d[0])
		if err != nil {
			return err
		}
		src, err := c.mustResource(d[5])
		if err != nil {
			return err
		}
		return errnoErr(C.ttc_virgl_copy_region(pipe, dst.pipe, src.pipe, ptr(d)))

	case cmdResourceInlineWrite:
		if n < 12 {
			return errLen(n)
		}
		r, err := c.mustResource(d[0])
		if err != nil {
			return err
		}
		data := d[11:]
		return errnoErr(C.ttc_virgl_inline_write(pipe, r.pipe, ptr(d), (*C.uint8_t)(unsafe.Pointer(&data[0])), C.uint32_t(4*len(data))))

	case cmdTransfer3D:
		if n < 13 {
			return errLen(n)
		}
		r, err := c.mustResource(d[0])
		if err != nil {
			return err
		}
		if d[12] != transferToHost && d[12] != transferFromHost {
			return fmt.Errorf("transfer direction %d", d[12])
		}
		return transfer(pipe, r, TransferArgs{
			ToHost: d[12] == transferToHost,
			Level:  d[1], Stride: d[3], LayerStride: d[4],
			X: d[5], Y: d[6], Z: d[7], W: d[8], H: d[9], D: d[10],
			Offset: uint64(d[11]),
		})

	case cmdBeginQuery, cmdEndQuery:
		if n != 1 {
			return errLen(n)
		}
		o, err := c.mustLookup(d[0], objectQuery)
		if err != nil {
			return err
		}
		switch {
		case o.counted && cmd == cmdBeginQuery:
			o.generated, o.emitted, o.needed = 0, 0, 0
			sub.countedQueries[o] = struct{}{}
		case o.counted:
			delete(sub.countedQueries, o)
		case cmd == cmdBeginQuery:
			C.ttc_virgl_begin_query(pipe, (*C.struct_pipe_query)(o.ptr))
			o.active = true
		default:
			C.ttc_virgl_end_query(pipe, (*C.struct_pipe_query)(o.ptr))
			o.active = false
		}
		return nil

	case cmdGetQueryResult:
		if n != 2 {
			return errLen(n)
		}
		o, err := c.mustLookup(d[0], objectQuery)
		if err != nil {
			return err
		}
		if o.counted {
			c.countedResultLocked(o)
			return nil
		}
		// Like virglrenderer's, a result that is not ready goes to the
		// guest's memory once a fence finds that it is, whether the guest
		// asked to wait or not: the guest waits for the fence.
		if !c.queryResultLocked(o) {
			for _, p := range c.pending {
				if p == o {
					return nil
				}
			}
			c.pending = append(c.pending, o)
		}
		return nil

	case cmdGetQueryResultQBO:
		if n != 6 {
			return errLen(n)
		}
		o, err := c.mustLookup(d[0], objectQuery)
		if err != nil {
			return err
		}
		qbo, err := c.mustResource(d[1])
		if err != nil {
			return err
		}
		if o.counted {
			return c.countedResultQBO(o, qbo, d[3], d[4], int32(d[5]))
		}
		return errnoErr(C.ttc_virgl_query_result_qbo(pipe, (*C.struct_pipe_query)(o.ptr), qbo.pipe, C.bool(d[2] != 0),
			C.uint32_t(d[3]), C.uint32_t(d[4]), C.int32_t(d[5])))

	case cmdSetSubCtx, cmdCreateSubCtx, cmdDestroySubCtx:
		if n != 1 {
			return errLen(n)
		}
		return c.subContextCmd(cmd, d[0])

	case cmdBindShader:
		if n != 2 || d[1] >= maxShaderStages {
			return errLen(n)
		}
		o, err := c.lookup(d[0], objectShader)
		if err != nil {
			return err
		}
		if o != nil && o.stage != d[1] {
			return fmt.Errorf("binding a shader of stage %d to stage %d", o.stage, d[1])
		}
		if d[1] != stageVertex {
			C.ttc_virgl_bind_shader(pipe, C.uint32_t(d[1]), objPtr(o))
		}
		c.rebind(sub, &sub.shaders[d[1]], o)
		return nil

	case cmdMemoryBarrier:
		if n != 1 {
			return errLen(n)
		}
		C.ttc_virgl_memory_barrier(pipe, C.uint32_t(d[0]))
		return nil

	case cmdTextureBarrier:
		if n != 1 {
			return errLen(n)
		}
		C.ttc_virgl_texture_barrier(pipe, C.uint32_t(d[0]))
		return nil

	case cmdLaunchGrid:
		if n != 8 {
			return errLen(n)
		}
		r, err := c.resource(d[6])
		if err != nil {
			return err
		}
		if sub.shaders[stageCompute] == nil {
			// Like virglrenderer, drop work without a shader.
			return nil
		}
		return errnoErr(C.ttc_virgl_launch_grid(pipe, ptr(d), pipeOf(r)))
	}
	return fmt.Errorf("unsupported command %d", cmd)
}

// holdRange holds res in the slots of kind and stage from start, and
// returns what they held.
func (c *Context) holdRange(kind, stage, start uint32, res []*Resource) []*Resource {
	old := make([]*Resource, len(res))
	for i, r := range res {
		old[i] = c.sub.hold(slot{kind, stage, start + uint32(i)}, r)
	}
	return old
}

func (c *Context) buffers(d []uint32, count uint32) ([]*Resource, []C.uint32_t, []C.uint32_t, error) {
	res := make([]*Resource, count)
	offsets := make([]C.uint32_t, count)
	sizes := make([]C.uint32_t, count)
	for i := uint32(0); i < count; i++ {
		r, err := c.resource(d[3*i+2])
		if err != nil {
			return nil, nil, nil, err
		}
		res[i] = r
		offsets[i] = C.uint32_t(d[3*i])
		sizes[i] = C.uint32_t(d[3*i+1])
	}
	return res, offsets, sizes, nil
}

func (c *Context) subContextCmd(cmd, id uint32) error {
	switch cmd {
	case cmdCreateSubCtx:
		if c.subs[id] != nil {
			return nil
		}
		sub, err := c.newSubContext()
		if err != nil {
			return err
		}
		c.subs[id] = sub
	case cmdSetSubCtx:
		// Like virglrenderer, stay in the sub-context if there is no other.
		if sub := c.subs[id]; sub != nil {
			c.sub = sub
		}
	case cmdDestroySubCtx:
		sub := c.subs[id]
		if sub == nil || id == 0 {
			return nil
		}
		if c.sub == sub {
			c.sub = c.subs[0]
		}
		c.destroySubLocked(sub)
		delete(c.subs, id)
	}
	return nil
}

func (c *Context) draw(d []uint32) error {
	n := len(d)
	if n != 12 && n != 14 && n != 20 {
		return errLen(n)
	}
	sub := c.sub
	if sub.velems == nil || sub.shaders[0] == nil || sub.shaders[1] == nil {
		// Like virglrenderer, drop draws without vertex elements or vertex
		// and fragment shaders, which Zink cannot draw without.
		return nil
	}
	for i := 0; i < len(sub.velems.elements)/4; i++ {
		if vb := sub.velems.elements[4*i+2]; vb >= uint32(sub.numVBs) || sub.vbs[vb].res == nil {
			// Nor without the vertex buffers that the elements read, which
			// Gallium's frontends always bind.
			return nil
		}
	}
	if d[11] != 0 {
		// A draw of the vertices that a stream output target captured, as
		// glDrawTransformFeedback draws.
		o, err := c.mustLookup(d[11], objectStreamoutTarget)
		if err != nil {
			return err
		}
		d = append([]uint32(nil), d...)
		d[0], d[1], d[11] = 0, o.soVerts, 0
	}
	var draw C.struct_ttc_virgl_draw
	if d[3] != 0 {
		ib := sub.ib
		if ib == nil {
			return errors.New("an indexed draw without an index buffer")
		}
		switch sub.ibSize {
		case 1, 2, 4:
		default:
			return fmt.Errorf("index size %d", sub.ibSize)
		}
		draw.index = ib.pipe
		draw.index_size = C.uint32_t(sub.ibSize)
		draw.index_offset = C.uint32_t(sub.ibOffset)
	}
	if n == 20 && d[14] != 0 {
		r, err := c.mustResource(d[14])
		if err != nil {
			return err
		}
		count, err := c.resource(d[19])
		if err != nil {
			return err
		}
		draw.indirect = r.pipe
		draw.indirect_count = pipeOf(count)
	}
	indirect := draw.indirect != nil
	cp := c.prepareCapture(d[2], d[0], d[1], d[4], d[3] != 0, indirect)
	if err := c.bindVertexState(); err != nil {
		return err
	}
	if err := c.bindVertexShader(cp != nil); err != nil {
		return err
	}
	if cp != nil {
		var res [maxStreamoutTargets]*C.struct_pipe_resource
		old := make([]*Resource, maxStreamoutTargets)
		for b := range res {
			var r *Resource
			if cp.strides[b] != 0 {
				r = sub.soTargets[b].res
			}
			res[b] = pipeOf(r)
			old[b] = sub.hold(slot{slotShaderBuffer, stageVertex, xfbSSBO + uint32(b)}, r)
		}
		C.ttc_virgl_set_xfb(sub.pipe, ptr(cp.params[:]), &res[0])
		release(old...)
	}
	if err := errnoErr(C.ttc_virgl_draw(sub.pipe, ptr(d), C.uint32_t(n), &draw)); err != nil {
		return err
	}
	c.countDraw(d[2], d[1], d[4], indirect, d, cp)
	return nil
}

// capture is what a draw's transform feedback captures.
type capture struct {
	// params are the capture's parameters, ttc_virgl_set_xfb's.
	params [8]uint32
	// strides are those, in bytes, of the targets that the vertex shader
	// captures to, and 0 for others.
	strides [maxStreamoutTargets]uint32
	// The vertices captured, and those that would have been had the
	// targets room, which whole primitives, of vertices each, make.
	written, wanted, vertices uint32
}

// prepareCapture returns the capture of a draw of mode, from the vertex
// first, of count vertices and of instances, or nil if transform feedback
// captures none. The renderer captures as OpenGL ES 3.0 allows: lists of
// points, lines and triangles, and no indexed draws.
func (c *Context) prepareCapture(mode, first, count, instances uint32, indexed, indirect bool) *capture {
	sub := c.sub
	vs := sub.shaders[stageVertex]
	if sub.soCount == 0 || !xfbEmulated {
		return nil
	}
	if len(vs.so) == 0 || vs.so[0] == 0 {
		// The guest gives the stream output to the last stage that
		// processes vertices: the renderer captures a vertex shader's only.
		for _, stage := range []uint32{stageGeometry, stageTessEval} {
			if s := sub.shaders[stage]; s != nil && len(s.so) > 0 && s.so[0] > 0 && !c.loggedCapture {
				c.loggedCapture = true
				log.Printf("virgl: context %d: transform feedback of shader stage %d captures nothing", c.id, stage)
			}
		}
		return nil
	}
	vertices := listVertices(mode)
	if vertices == 0 || indexed || indirect {
		if !c.loggedCapture {
			c.loggedCapture = true
			log.Printf("virgl: context %d: transform feedback of a draw of mode %d (indexed %v, indirect %v) captures nothing",
				c.id, mode, indexed, indirect)
		}
		return nil
	}
	cp := &capture{vertices: vertices}
	nso := int(vs.so[0])
	for i := 0; i < nso && 5+2*i < len(vs.so); i++ {
		if buf := vs.so[5+2*i] >> 13 & 7; buf < maxStreamoutTargets {
			cp.strides[buf] = vs.so[1+buf] * 4
		}
	}
	perInstance := count - count%vertices
	wanted := uint64(perInstance) * uint64(instances)
	fit := wanted
	for b, stride := range cp.strides {
		if stride == 0 {
			continue
		}
		t := sub.soTargets[b]
		if t == nil || b >= sub.soCount {
			fit = 0
			continue
		}
		fit = min(fit, uint64((t.soSize-t.soFilled)/stride))
		cp.params[4+b] = t.soOffset + t.soFilled
	}
	fit -= fit % uint64(vertices)
	cp.written, cp.wanted = uint32(fit), uint32(min(wanted, 1<<32-1))
	cp.params[0], cp.params[1], cp.params[2] = first, perInstance, cp.written
	return cp
}

// listVertices returns the vertices of each primitive of a list of
// points, lines or triangles, and 0 for other modes.
func listVertices(mode uint32) uint32 {
	switch mode {
	case primPoints:
		return 1
	case primLines:
		return 2
	case primTriangles:
		return 3
	}
	return 0
}

// primitives returns the primitives of count vertices of mode, which
// patches of patchVertices make of mode primPatches.
func primitives(mode, count, patchVertices uint32) uint64 {
	n := uint64(count)
	sub := func(k uint64) uint64 {
		if n < k {
			return 0
		}
		return n - k
	}
	switch mode {
	case primPoints:
		return n
	case primLines:
		return n / 2
	case primLineLoop:
		if n < 2 {
			return 0
		}
		return n
	case primLineStrip:
		return sub(1)
	case primTriangles:
		return n / 3
	case primTriangleStrip, primTriangleFan:
		return sub(2)
	case primQuads:
		return n / 4
	case primQuadStrip:
		return sub(2) / 2
	case primPolygon:
		if n < 3 {
			return 0
		}
		return 1
	case primLinesAdjacency:
		return n / 4
	case primLineStripAdjacency:
		return sub(3)
	case primTrianglesAdjacency:
		return n / 6
	case primTriangleStripAdjacency:
		return sub(4) / 2
	case primPatches:
		if patchVertices == 0 {
			return 0
		}
		return n / uint64(patchVertices)
	}
	return 0
}

// countDraw counts a draw, d, of mode, of count vertices and of instances,
// whose capture is cp, for the active queries that the renderer counts,
// and the targets that it captured to.
func (c *Context) countDraw(mode, count, instances uint32, indirect bool, d []uint32, cp *capture) {
	sub := c.sub
	if cp != nil {
		for b, stride := range cp.strides {
			if stride != 0 && b < sub.soCount && sub.soTargets[b] != nil {
				t := sub.soTargets[b]
				t.soFilled += cp.written * stride
				t.soVerts += cp.written
			}
		}
	}
	if len(sub.countedQueries) == 0 || indirect {
		return
	}
	var patchVertices uint32
	if len(d) >= 14 {
		patchVertices = d[12]
	}
	generated := primitives(mode, count, patchVertices) * uint64(instances)
	for q := range sub.countedQueries {
		switch q.qtype {
		case queryPrimitivesGenerated:
			q.generated += generated
		default:
			if cp != nil {
				q.emitted += uint64(cp.written / cp.vertices)
				q.needed += uint64(cp.wanted / cp.vertices)
			}
		}
	}
}

// countedResultLocked writes the result of a query that the renderer
// counts to the guest's memory of its resource.
func (c *Context) countedResultLocked(o *object) {
	var state [queryStateSize]byte
	binary.LittleEndian.PutUint32(state[0:], queryStateDone)
	binary.LittleEndian.PutUint32(state[4:], 4)
	binary.LittleEndian.PutUint64(state[8:], o.countedResult(0))
	copy(o.res.backing, state[:])
}

// countedResult returns a counted query's result, or, for SO_STATISTICS,
// its value index: the primitives written, or those that would have been.
func (o *object) countedResult(index int32) uint64 {
	switch o.qtype {
	case queryPrimitivesGenerated:
		return o.generated
	case queryPrimitivesEmitted:
		return o.emitted
	case querySOStatistics:
		if index == 1 {
			return o.needed
		}
		return o.emitted
	default: // the overflow predicates
		if o.needed > o.emitted {
			return 1
		}
		return 0
	}
}

// countedResultQBO writes a counted query's result to the buffer qbo at
// offset, as Gallium's get_query_result_resource does: of resultType, a
// PIPE_QUERY_TYPE_*, and the value index, or its availability if index is
// -1.
func (c *Context) countedResultQBO(o *object, qbo *Resource, resultType, offset uint32, index int32) error {
	value := uint64(1)
	if index >= 0 {
		value = o.countedResult(index)
	}
	var b [8]byte
	size := uint32(4)
	switch resultType {
	case queryTypeI32:
		binary.LittleEndian.PutUint32(b[:], uint32(min(value, 1<<31-1)))
	case queryTypeU32:
		binary.LittleEndian.PutUint32(b[:], uint32(min(value, 1<<32-1)))
	case queryTypeI64, queryTypeU64:
		binary.LittleEndian.PutUint64(b[:], value)
		size = 8
	default:
		return fmt.Errorf("query result type %d", resultType)
	}
	width := uint32(C.ttc_virgl_resource_width(qbo.pipe, 0))
	if C.ttc_virgl_resource_target(qbo.pipe) != targetBuffer || offset > width || size > width-offset {
		return fmt.Errorf("a query result at %d of resource %d", offset, qbo.id)
	}
	C.ttc_virgl_buffer_write(c.sub.pipe, qbo.pipe, C.uint32_t(offset), unsafe.Pointer(&b[0]), C.uint32_t(size))
	return nil
}

// bindVertexState binds a Gallium state of the guest's vertex elements, of
// the bound vertex buffers' strides, and then, as Gallium requires, the
// vertex buffers.
func (c *Context) bindVertexState() error {
	sub := c.sub
	o := sub.velems
	var key strings.Builder
	var strides [maxVertexBuffers]C.uint32_t
	for i := 0; i < len(o.elements)/4; i++ {
		vb := o.elements[4*i+2]
		if vb >= maxVertexBuffers {
			return fmt.Errorf("vertex buffer %d", vb)
		}
		strides[vb] = C.uint32_t(sub.vbs[vb].stride)
		fmt.Fprintf(&key, "%d:%d,", vb, sub.vbs[vb].stride)
	}
	cso := o.csos[key.String()]
	if cso == nil {
		cso = C.ttc_virgl_create_vertex_elements(sub.pipe, ptr(o.elements), C.uint32_t(len(o.elements)/4), &strides[0])
		if cso == nil {
			return errors.New("Zink refused vertex elements")
		}
		o.csos[key.String()] = cso
	}
	if cso != sub.velemsCSO {
		C.ttc_virgl_bind_object(sub.pipe, objectVertexElements, cso)
		sub.velemsCSO = cso
		c.rebind(sub, &sub.velemsBound, o)
		sub.vbsDirty = true
	}
	if !sub.vbsDirty {
		return nil
	}
	// Zink replaces the buffers of the slots below the count, and holds
	// the others' until a later call replaces them.
	var res [maxVertexBuffers]*C.struct_pipe_resource
	var offsets [maxVertexBuffers]C.uint32_t
	old := make([]*Resource, sub.numVBs)
	for i := 0; i < sub.numVBs; i++ {
		vb := sub.vbs[i]
		res[i] = pipeOf(vb.res)
		offsets[i] = C.uint32_t(vb.offset)
		old[i] = sub.hold(slot{slotVertexBuffer, 0, uint32(i)}, vb.res)
	}
	C.ttc_virgl_set_vertex_buffers(sub.pipe, C.uint32_t(sub.numVBs), &res[0], &offsets[0])
	release(old...)
	sub.vbsDirty = false
	return nil
}

func (c *Context) createObject(typ uint32, d []uint32) error {
	sub := c.sub
	pipe := sub.pipe
	n := len(d)
	if n < 1 || d[0] == 0 {
		return errLen(n)
	}
	h := d[0]
	if typ == objectShader {
		return c.createShader(d)
	}
	o := &object{typ: typ}
	switch typ {
	case objectBlend:
		if n != 11 {
			return errLen(n)
		}
		o.ptr = C.ttc_virgl_create_blend(pipe, ptr(d))
	case objectRasterizer:
		if n != 9 {
			return errLen(n)
		}
		o.ptr = C.ttc_virgl_create_rasterizer(pipe, ptr(d))
	case objectDSA:
		if n != 5 {
			return errLen(n)
		}
		o.ptr = C.ttc_virgl_create_dsa(pipe, ptr(d))
	case objectSamplerState:
		if n != 9 {
			return errLen(n)
		}
		o.ptr = C.ttc_virgl_create_sampler_state(pipe, ptr(d))
	case objectVertexElements:
		if (n-1)%4 != 0 || (n-1)/4 > maxVertexElements {
			return errLen(n)
		}
		o.elements = append([]uint32(nil), d[1:]...)
		o.csos = make(map[string]unsafe.Pointer)
		for i := 0; i < len(o.elements)/4; i++ {
			switch C.ttc_virgl_format_int_class(C.uint32_t(o.elements[4*i+3])) {
			case 1:
				o.sintMask |= 1 << i
			case 2:
				o.uintMask |= 1 << i
			}
		}
	case objectSurface, objectMSAASurface:
		if typ == objectSurface && n != 5 || typ == objectMSAASurface && n != 6 {
			return errLen(n)
		}
		r, err := c.mustResource(d[1])
		if err != nil {
			return err
		}
		if C.ttc_virgl_check_surface(r.pipe, C.uint32_t(d[2]), C.uint32_t(d[3]), C.uint32_t(d[4])) != 0 {
			return fmt.Errorf("a surface of resource %d: format %d, %#x, %#x", d[1], d[2], d[3], d[4])
		}
		r.incRef()
		o.res = r
		o.surf = C.struct_ttc_virgl_surface{res: r.pipe, format: C.uint32_t(d[2]), val0: C.uint32_t(d[3]), val1: C.uint32_t(d[4])}
		if typ == objectMSAASurface {
			if d[5] > 32 || d[5]&(d[5]-1) != 0 {
				r.DecRef()
				return fmt.Errorf("a surface of %d samples", d[5])
			}
			o.surf.nr_samples = C.uint32_t(d[5])
		}
	case objectSamplerView:
		if n != 6 {
			return errLen(n)
		}
		r, err := c.mustResource(d[1])
		if err != nil {
			return err
		}
		o.ptr = unsafe.Pointer(C.ttc_virgl_create_sampler_view(pipe, r.pipe, C.uint32_t(d[2]&0xffffff),
			C.uint32_t(d[2]>>24), C.uint32_t(d[3]), C.uint32_t(d[4]), C.uint32_t(d[5])))
		if o.ptr != nil {
			r.incRef()
			o.res = r
		}
	case objectQuery:
		if n != 4 {
			return errLen(n)
		}
		r, err := c.mustResource(d[3])
		if err != nil {
			return err
		}
		if len(r.backing) < queryStateSize {
			return fmt.Errorf("a query of resource %d, of %d bytes", d[3], len(r.backing))
		}
		o.qtype = d[1] & 0xffff
		if xfbEmulated && countedQuery(o.qtype) {
			// Queries that Zink, which has no transform feedback here,
			// cannot answer: the renderer counts their primitives itself.
			o.counted = true
		} else {
			o.ptr = unsafe.Pointer(C.ttc_virgl_create_query(pipe, C.uint32_t(o.qtype), C.uint32_t(d[1]>>16)))
		}
		if o.ptr != nil || o.counted {
			r.incRef()
			o.res = r
			o.sub = sub
		}
	case objectStreamoutTarget:
		if n != 4 {
			return errLen(n)
		}
		r, err := c.mustResource(d[1])
		if err != nil {
			return err
		}
		if !xfbEmulated {
			return errors.New("no transform feedback")
		}
		width := uint32(C.ttc_virgl_resource_width(r.pipe, 0))
		if C.ttc_virgl_resource_target(r.pipe) != targetBuffer || d[2] > width || d[3] > width-d[2] || d[2]%4 != 0 {
			return fmt.Errorf("a stream output target at %d, of %d bytes, of resource %d", d[2], d[3], d[1])
		}
		r.incRef()
		o.res = r
		o.soOffset, o.soSize = d[2], d[3]
	default:
		return fmt.Errorf("creating object type %d", typ)
	}
	if o.ptr == nil && typ != objectSurface && typ != objectMSAASurface && !o.renderers() {
		return fmt.Errorf("Zink refused object %d of type %d", h, typ)
	}
	switch typ {
	case objectBlend, objectRasterizer, objectDSA, objectSamplerState, objectSamplerView, objectQuery:
		o.args = append([]uint32(nil), d...)
	}
	c.destroyObjectLocked(sub, h)
	sub.objects[h] = o
	return nil
}

// createShader creates a shader of its text, which comes in parts when it
// is longer than a command.
func (c *Context) createShader(d []uint32) error {
	n := len(d)
	if n < 5 {
		return errLen(n)
	}
	h, stage, offlen, tokens := d[0], d[1], d[2], d[3]
	if stage >= maxShaderStages {
		return fmt.Errorf("shader stage %d", stage)
	}
	start := 5
	var so []uint32
	var shared uint32
	if stage == stageCompute {
		shared = d[4]
	} else if nso := d[4]; nso > 0 {
		if nso > maxStreamOutputs || n < 5+4+2*int(nso) {
			return fmt.Errorf("%d stream outputs in %d dwords", nso, n)
		}
		start += 4 + 2*int(nso)
		so = append([]uint32(nil), d[4:start]...)
	}
	part := make([]byte, 4*(n-start))
	for i, w := range d[start:] {
		binary.LittleEndian.PutUint32(part[4*i:], w)
	}
	sub := c.sub
	var o *object
	if offlen&shaderOffsetContinuation != 0 {
		o = sub.objects[h]
		off := offlen & shaderOffsetMask
		if o == nil || o.typ != objectShader || o.ptr != nil || o.text == nil || off != o.filled {
			return fmt.Errorf("shader %d continues at %d", h, off)
		}
	} else {
		total := offlen & shaderOffsetMask
		if total == 0 || total > maxShaderTextBytes {
			return fmt.Errorf("shader of %d bytes", total)
		}
		o = &object{typ: objectShader, stage: stage, text: make([]byte, total), numTokens: tokens, so: so, shared: shared}
		c.destroyObjectLocked(sub, h)
		sub.objects[h] = o
	}
	o.filled += uint32(copy(o.text[o.filled:], part))
	if o.filled < uint32(len(o.text)) {
		return nil
	}
	text := string(o.text)
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	o.text = nil
	ctext := C.CString(text)
	defer C.free(unsafe.Pointer(ctext))
	var errBuf [256]C.char
	tgsi := C.ttc_virgl_translate_tgsi(ctext, C.uint32_t(o.numTokens), &errBuf[0], C.uint32_t(len(errBuf)))
	if tgsi != nil {
		o.ptr = C.ttc_virgl_create_shader(sub.pipe, C.uint32_t(o.stage), tgsi, ptr(o.so), C.uint32_t(len(o.so)),
			C.uint32_t(o.shared), 0, 0, false, &errBuf[0], C.uint32_t(len(errBuf)))
	}
	if o.ptr == nil {
		C.ttc_virgl_free_tokens(tgsi)
		delete(sub.objects, h)
		return fmt.Errorf("shader %d: %s", h, C.GoString(&errBuf[0]))
	}
	o.tokens = tgsi
	if o.stage == stageVertex {
		// Draws bind the vertex shader's variant of the vertex elements'
		// types.
		o.variants = map[variantKey]unsafe.Pointer{{}: o.ptr}
	}
	return nil
}

// variantKey is a vertex shader's variant's: the inputs that are signed
// and unsigned integers, and whether it captures its stream output.
type variantKey struct {
	sint, uint uint32
	xfb        bool
}

// bindVertexShader binds the variant of the guest's vertex shader whose
// inputs have the types of the vertex elements that feed them, and that
// captures its stream output if xfb: TGSI's inputs have no types, and
// Metal requires that shaders read integer vertex formats as integers.
func (c *Context) bindVertexShader(xfb bool) error {
	sub := c.sub
	vs, ve := sub.shaders[stageVertex], sub.velems
	key := variantKey{ve.sintMask, ve.uintMask, xfb}
	v := vs.variants[key]
	if v == nil {
		var errBuf [256]C.char
		v = C.ttc_virgl_create_shader(sub.pipe, stageVertex, vs.tokens, ptr(vs.so), C.uint32_t(len(vs.so)), 0,
			C.uint32_t(ve.sintMask), C.uint32_t(ve.uintMask), C.bool(xfb), &errBuf[0], C.uint32_t(len(errBuf)))
		if v == nil {
			return fmt.Errorf("a vertex shader of integer inputs %#x and %#x: %s", ve.sintMask, ve.uintMask, C.GoString(&errBuf[0]))
		}
		vs.variants[key] = v
	}
	if v != sub.vsCSO {
		C.ttc_virgl_bind_shader(sub.pipe, stageVertex, v)
		sub.vsCSO = v
		c.rebind(sub, &sub.vsBound, vs)
	}
	return nil
}

// queryResultLocked writes o's result to the guest's memory of its
// resource if it is ready, and reports whether it did.
func (c *Context) queryResultLocked(o *object) bool {
	var state [queryStateSize]byte
	if !C.ttc_virgl_query_result(o.sub.pipe, (*C.struct_pipe_query)(o.ptr), C.uint32_t(o.qtype), false, unsafe.Pointer(&state[0])) {
		return false
	}
	copy(o.res.backing, state[:])
	return true
}

// checkQueriesLocked writes the results of pending queries that are ready.
func (c *Context) checkQueriesLocked() {
	pending := c.pending[:0]
	for _, o := range c.pending {
		if !c.queryResultLocked(o) {
			pending = append(pending, o)
		}
	}
	c.pending = pending
}
