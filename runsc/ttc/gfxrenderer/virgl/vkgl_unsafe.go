// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/vkgl"
)

// The backend of package vkgl: Gallium's state and TGSI's shaders on
// Vulkan, in Go (g3doc/proposals/ttc_virgl_go_backend.md), which the
// renderer renders with. It refuses what it does not render yet, as
// errVkgl, of which its capability sets offer guests none.
//
// The GPU may use an object after the decoder lets it go: each object has
// a count of references, of which the decoder holds one, and each batch
// of a context's that uses it another, which the batch releases once the
// GPU is done with it.

// The slots of the vertex shader's state that the backend keeps for
// transform feedback's emulation, of tospirv's StreamOutput.
const (
	vkglXfbUBO  = 15
	vkglXfbSSBO = 12
)

// errVkgl is the error of what the vkgl backend does not render yet, which
// its capability sets offer guests none of. TODO: what OpenGL ES 3.1 needs
// (g3doc/proposals/ttc_virgl_go_backend.md, "OpenGL ES 3.1").
var errVkgl = errors.New("the vkgl backend renders none of it yet")

type vkglBackend struct {
	d *vkgl.Device
	// dummyView and dummySampler are what shaders sample of slots that
	// have none bound.
	dummy        *vkglResource
	dummyView    *vkgl.SamplerView
	dummySampler *vkgl.Sampler
}

func initVkgl() (backend, error) {
	d, err := vkgl.NewDevice()
	if err != nil {
		return nil, fmt.Errorf("vkgl: %w", err)
	}
	b := &vkglBackend{d: d}
	r, err := b.newResource(ResourceArgs{Target: targetTexture2D, Format: formatB8G8R8A8Unorm, Bind: bindSamplerViewArg,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1})
	if err != nil {
		return nil, err
	}
	b.dummy = r.(*vkglResource)
	if b.dummyView, err = d.NewSamplerView(b.dummy.tex, vkgl.DecodeViewDesc(formatB8G8R8A8Unorm, targetTexture2D, 0, 0,
		0|1<<3|2<<6|3<<9)); err != nil {
		return nil, err
	}
	if b.dummySampler, err = d.NewSamplerOf(&vkgl.SamplerState{MaxLod: 1000}); err != nil {
		return nil, err
	}
	return b, nil
}

// The virgl numbers that the backend's own objects use.
const (
	targetTexture2D     = 2
	formatB8G8R8A8Unorm = 1
	bindSamplerViewArg  = 1 << 3
)

func (b *vkglBackend) fillCaps(set uint32, caps []byte) {
	copy(caps, b.d.Caps(set, 1, 4))
}

func (b *vkglBackend) xfb() (bool, uint32) {
	return true, vkglXfbSSBO
}

func (b *vkglBackend) formatIntClass(format uint32) uint32 {
	f, ok := vkgl.LookupFormat(format)
	if !ok {
		return 0
	}
	switch integer, signed := f.IsInteger(); {
	case integer && signed:
		return 1
	case integer:
		return 2
	}
	return 0
}

// vkglRef is a count of an object's references.
type vkglRef struct {
	refs    atomic.Int32
	destroy func()
}

func (r *vkglRef) init(destroy func()) {
	r.refs.Store(1)
	r.destroy = destroy
}

func (r *vkglRef) incRef() {
	r.refs.Add(1)
}

func (r *vkglRef) decRef() {
	if r.refs.Add(-1) == 0 {
		r.destroy()
	}
}

// vkglResource is a resource of the backend's: a buffer, or a texture and
// the surfaces of it that framebuffers had.
type vkglResource struct {
	vkglRef
	b        *vkglBackend
	desc     vkgl.ResourceDesc
	buf      *vkgl.Buffer
	tex      *vkgl.Texture
	surfaces map[[3]uint32]*vkgl.Surface
}

func (b *vkglBackend) newResource(a ResourceArgs) (backendResource, error) {
	desc := vkgl.ResourceDesc{Target: a.Target, Format: a.Format, Bind: a.Bind, Width: a.Width, Height: a.Height,
		Depth: a.Depth, ArraySize: a.ArraySize, LastLevel: a.LastLevel, Samples: a.NrSamples}
	r := &vkglResource{b: b, desc: desc}
	var err error
	if a.Target == vkgl.TargetBuffer {
		r.buf, err = b.d.NewBuffer(uint64(max(a.Width, 1)), vkgl.BufferTransferSrc|vkgl.BufferTransferDst|
			vkgl.BufferUniform|vkgl.BufferStorage|vkgl.BufferIndex|vkgl.BufferVertex|vkgl.BufferIndirect, false)
	} else {
		r.tex, err = b.d.NewTexture(desc)
	}
	if err != nil {
		return nil, err
	}
	r.init(r.free)
	return r, nil
}

// free destroys the resource, of no references.
func (r *vkglResource) free() {
	d := r.b.d
	for _, s := range r.surfaces {
		d.DestroySurface(s)
	}
	if r.tex != nil {
		d.DestroyTexture(r.tex)
	}
	if r.buf != nil {
		d.DestroyBuffer(r.buf)
	}
}

func (r *vkglResource) unref() {
	r.decRef()
}

func (r *vkglResource) target() uint32 {
	return r.desc.Target
}

func (r *vkglResource) width(level uint32) uint32 {
	if r.buf != nil {
		return r.desc.Width
	}
	return max(r.desc.Width>>level, 1)
}

func (r *vkglResource) height(level uint32) uint32 {
	if r.buf != nil {
		return r.desc.Height
	}
	return max(r.desc.Height>>level, 1)
}

func (r *vkglResource) checkSurface(format, val0, val1 uint32) error {
	if r.tex == nil {
		return fmt.Errorf("a surface of a buffer: %w", errVkgl)
	}
	return r.tex.CheckSurface(format, val0, val1&0xffff, val1>>16)
}

// surface returns the vkgl surface of s, a surface of r.
func (r *vkglResource) surface(s surface) (*vkgl.Surface, error) {
	key := [3]uint32{s.format, s.val0, s.val1}
	if vs := r.surfaces[key]; vs != nil {
		return vs, nil
	}
	vs, err := r.b.d.NewSurface(r.tex, s.format, s.val0, s.val1&0xffff, s.val1>>16)
	if err != nil {
		return nil, err
	}
	if r.surfaces == nil {
		r.surfaces = make(map[[3]uint32]*vkgl.Surface)
	}
	r.surfaces[key] = vs
	return vs, nil
}

// vkglCode is a shader's parsed TGSI, which vkgl translates for each
// pipeline's state.
type vkglCode struct {
	s *vkgl.Shader
}

func (c vkglCode) dup() shaderCode {
	return c
}

func (c vkglCode) free() {}

func (b *vkglBackend) parseShader(text string, numTokens uint32) (shaderCode, error) {
	s, err := vkgl.NewShader(text)
	if err != nil {
		return nil, err
	}
	// Of shaders that tospirv translates, before a draw, as the guest's
	// commands refer to them: once, of no options, of which draws find the
	// translation.
	if _, err := s.Translate(tospirv.Options{}); err != nil {
		return nil, err
	}
	return vkglCode{s}, nil
}

// vkglShader is a shader of a stage, and of a vertex shader its variant's:
// the inputs that are signed and unsigned integers, and whether it
// captures its stream output.
type vkglShader struct {
	s          *vkgl.Shader
	stage      uint32
	sint, uint uint32
	so         *tospirv.StreamOutput
}

// vkglView is a sampler view, and the resource that it holds a reference
// to.
type vkglView struct {
	vkglRef
	res  *vkglResource
	view *vkgl.SamplerView
}

// vkglSampler is a sampler state, and its Vulkan sampler.
type vkglSampler struct {
	vkglRef
	sampler *vkgl.Sampler
}

// vkglVertexElements are vertex elements, of the vertex buffers' strides.
type vkglVertexElements struct {
	elements []vkgl.VertexElement
	strides  []uint32
}

// vkglFence is a vkgl context's fence.
type vkglFence struct {
	f *vkgl.Fence
}

func (f vkglFence) wait() {
	f.f.Wait()
}

// vkglRange is a buffer's range bound to a slot.
type vkglRange struct {
	res          *vkglResource
	offset, size uint32
}

// vkglContext is a context of the backend's: a vkgl context, and the
// Gallium state bound to it.
type vkglContext struct {
	b   *vkglBackend
	ctx *vkgl.Context

	blend     *vkgl.Blend
	rast      *vkgl.Rasterizer
	dsa       *vkgl.DSA
	velems    *vkglVertexElements
	shaders   [maxShaderStages]*vkglShader
	views     [maxShaderStages][maxSamplerViews]*vkglView
	samplers  [maxShaderStages][maxSamplers]*vkglSampler
	constants [maxShaderStages][]uint32
	ubos      [maxShaderStages][maxConstantBuffers]vkglRange
	vbs       []vkglRange
	// fb is the framebuffer, of fbRes's surfaces.
	fb         vkgl.Framebuffer
	fbRes      []*vkglResource
	viewport   vkgl.Viewport
	scissor    vkgl.Scissor
	stencilRef [2]uint32
	blendColor [4]float32
	sampleMask uint32
	minSamples uint32
	clip       [8][4]float32
	xfbParams  []uint32
	xfbTargets []backendResource
	pipelines  map[string]*vkgl.Pipeline
}

func (b *vkglBackend) newContext() (backendContext, error) {
	ctx, err := b.d.NewContext()
	if err != nil {
		return nil, err
	}
	return &vkglContext{b: b, ctx: ctx, sampleMask: ^uint32(0), pipelines: make(map[string]*vkgl.Pipeline)}, nil
}

// use has the context's batch hold r until the GPU is done with it.
func (c *vkglContext) use(r *vkglRef) error {
	return c.ctx.Keep(r, func() func() {
		r.incRef()
		return r.decRef
	})
}

func resOfVkgl(r backendResource) *vkglResource {
	if r == nil {
		return nil
	}
	return r.(*vkglResource)
}

func (c *vkglContext) transfer(br backendResource, a TransferArgs, data []byte) error {
	r := resOfVkgl(br)
	if err := c.use(&r.vkglRef); err != nil {
		return err
	}
	if r.buf != nil {
		if a.W == 0 || a.Offset > uint64(len(data)) || uint64(a.W) > uint64(len(data))-a.Offset {
			return fmt.Errorf("a transfer of %d bytes at %d of %d", a.W, a.Offset, len(data))
		}
		part := data[a.Offset : a.Offset+uint64(a.W)]
		if a.ToHost {
			return c.ctx.WriteBuffer(r.buf, uint64(a.X), part)
		}
		return c.ctx.ReadBuffer(r.buf, uint64(a.X), part)
	}
	region := vkgl.TextureRegion{Level: a.Level, Stride: a.Stride, LayerStride: a.LayerStride,
		Box: vkgl.Box{X: int64(a.X), Y: int64(a.Y), Z: int64(a.Z), Width: int64(a.W), Height: int64(a.H), Depth: int64(a.D)}}
	if a.ToHost {
		return c.ctx.WriteTexture(r.tex, region, data, a.Offset)
	}
	return c.ctx.ReadTexture(r.tex, region, data, a.Offset)
}

func (c *vkglContext) copyResource(dst, src backendResource) {
	dr, sr := resOfVkgl(dst), resOfVkgl(src)
	err := c.use(&dr.vkglRef)
	if err == nil {
		err = c.use(&sr.vkglRef)
	}
	if err == nil {
		if dr.buf != nil {
			err = c.ctx.CopyBuffer(dr.buf, 0, sr.buf, 0, min(dr.buf.Size(), sr.buf.Size()))
		} else {
			err = c.ctx.CopyTextureAll(dr.tex, sr.tex)
		}
	}
	if err != nil {
		log.Printf("virgl: copying a resource of %+v: %v", sr.desc, err)
	}
}

func (c *vkglContext) unbind() {
	c.setFramebuffer(nil, surface{})
	for stage := range c.views {
		c.setSamplerViews(uint32(stage), 0, make([]any, maxSamplerViews))
		c.bindSamplers(uint32(stage), 0, make([]any, maxSamplers))
		for i := range c.ubos[stage] {
			c.setUniformBuffer(uint32(stage), uint32(i), nil, 0, 0)
		}
	}
	c.setVertexBuffers(nil, nil)
}

func (c *vkglContext) destroy() {
	c.ctx.Destroy()
	for _, p := range c.pipelines {
		c.b.d.DestroyPipeline(p)
	}
	c.pipelines = nil
}

func (c *vkglContext) flush() backendFence {
	if f := c.ctx.Flush(); f != nil {
		return vkglFence{f}
	}
	return nil
}

func (c *vkglContext) flushAsync() {
	c.ctx.Flush()
}

func (c *vkglContext) createBlend(d []uint32) any {
	if b, err := vkgl.DecodeBlend(d); err == nil {
		return b
	}
	return nil
}

func (c *vkglContext) createRasterizer(d []uint32) any {
	if r, err := vkgl.DecodeRasterizer(d); err == nil {
		return r
	}
	return nil
}

func (c *vkglContext) createDSA(d []uint32) any {
	if z, err := vkgl.DecodeDSA(d); err == nil {
		return z
	}
	return nil
}

func (c *vkglContext) createSamplerState(d []uint32) any {
	st, err := vkgl.DecodeSamplerState(d)
	if err != nil {
		return nil
	}
	s, err := c.b.d.NewSamplerOf(st)
	if err != nil {
		return nil
	}
	o := &vkglSampler{sampler: s}
	o.init(func() { c.b.d.DestroySampler(s) })
	return o
}

func (c *vkglContext) createVertexElements(elements []uint32, strides []uint32) any {
	ve, err := vkgl.DecodeVertexElements(elements)
	if err != nil {
		return nil
	}
	return &vkglVertexElements{elements: ve, strides: append([]uint32(nil), strides...)}
}

func (c *vkglContext) createShader(stage uint32, code shaderCode, so []uint32, sharedMem, sintMask, uintMask uint32, xfb bool) (any, error) {
	var p tgsi.Processor
	switch stage {
	case stageVertex:
		p = tgsi.ProcessorVertex
	case stageFragment:
		p = tgsi.ProcessorFragment
	default:
		return nil, fmt.Errorf("a shader of stage %d: %w", stage, errVkgl)
	}
	vs := code.(vkglCode).s
	if vs.TGSI.Processor != p {
		return nil, fmt.Errorf("TGSI of %v, of a shader of stage %d", vs.TGSI.Processor, stage)
	}
	s := &vkglShader{s: vs, stage: stage, sint: sintMask, uint: uintMask}
	if xfb {
		var err error
		if s.so, err = decodeStreamOutput(so); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// decodeStreamOutput returns the stream output of so, a shader's payload
// from its count, as tospirv captures it.
func decodeStreamOutput(so []uint32) (*tospirv.StreamOutput, error) {
	if len(so) == 0 || so[0] == 0 {
		return nil, nil
	}
	n := so[0]
	if n > 64 || uint32(len(so)) < 5+2*n {
		return nil, fmt.Errorf("%d stream outputs in %d dwords", n, len(so))
	}
	out := &tospirv.StreamOutput{UBO: vkglXfbUBO, SSBO: vkglXfbSSBO}
	copy(out.Strides[:], so[1:5])
	for i := uint32(0); i < n; i++ {
		t := so[5+2*i]
		out.Outputs = append(out.Outputs, tospirv.StreamOutputItem{
			Register:       int(t & 0xff),
			StartComponent: t >> 8 & 3,
			NumComponents:  t >> 10 & 7,
			Buffer:         t >> 13 & 7,
			DstOffset:      t >> 16,
		})
	}
	return out, nil
}

func (c *vkglContext) createSamplerView(br backendResource, format, target, val0, val1, swizzle uint32) any {
	r := resOfVkgl(br)
	if r.tex == nil {
		// Texture buffers: not yet.
		return nil
	}
	view, err := c.b.d.NewSamplerView(r.tex, vkgl.DecodeViewDesc(format, target, val0, val1, swizzle))
	if err != nil {
		return nil
	}
	r.incRef()
	o := &vkglView{res: r, view: view}
	o.init(func() {
		c.b.d.DestroySamplerView(view)
		r.decRef()
	})
	return o
}

// vkglQuery is a query of virgl's type typ, and its vkgl query, or nil of
// a timestamp disjoint query, which the backend answers of no GPU's.
type vkglQuery struct {
	typ uint32
	q   *vkgl.Query
}

func (c *vkglContext) createQuery(typ, index uint32) any {
	kind := -1
	switch typ {
	case queryOcclusionCounter, queryOcclusionPredicate, queryOcclusionConservative:
		kind = vkgl.QueryOcclusion
	case queryTimestamp:
		kind = vkgl.QueryTimestamp
	case queryTimeElapsed:
		kind = vkgl.QueryTimeElapsed
	case queryGPUFinished:
		kind = vkgl.QueryFinished
	case queryTimestampDisjoint:
	default:
		return nil
	}
	// A vertex stream's index: Gallium's are 0 to 3.
	if index > 3 {
		return nil
	}
	vq := &vkglQuery{typ: typ}
	if kind >= 0 {
		q, err := c.b.d.NewQuery(kind)
		if err != nil {
			return nil
		}
		vq.q = q
	}
	return vq
}

func (c *vkglContext) deleteObject(typ, stage uint32, obj any) {
	switch o := obj.(type) {
	case *vkglView:
		o.decRef()
	case *vkglSampler:
		o.decRef()
	case *vkglQuery:
		if o.q != nil {
			c.ctx.DestroyQuery(o.q)
		}
	}
}

func (c *vkglContext) bindObject(typ uint32, obj any) {
	switch typ {
	case objectBlend:
		c.blend, _ = obj.(*vkgl.Blend)
	case objectRasterizer:
		c.rast, _ = obj.(*vkgl.Rasterizer)
	case objectDSA:
		c.dsa, _ = obj.(*vkgl.DSA)
	case objectVertexElements:
		c.velems, _ = obj.(*vkglVertexElements)
	}
}

func (c *vkglContext) bindShader(stage uint32, shader any) {
	if stage < maxShaderStages {
		c.shaders[stage], _ = shader.(*vkglShader)
	}
}

func (c *vkglContext) setXfb(params []uint32, targets []backendResource) {
	c.xfbParams = append(c.xfbParams[:0], params...)
	c.xfbTargets = append(c.xfbTargets[:0], targets...)
}

func (c *vkglContext) bufferWrite(br backendResource, offset uint32, data []byte) {
	r := resOfVkgl(br)
	if r.buf == nil || c.use(&r.vkglRef) != nil {
		return
	}
	c.ctx.WriteBuffer(r.buf, uint64(offset), data)
}

func (c *vkglContext) setFramebuffer(cbufs []surface, zsbuf surface) error {
	fb := vkgl.Framebuffer{Colors: make([]*vkgl.Surface, len(cbufs))}
	var held []*vkglResource
	hold := func(s surface) (*vkgl.Surface, error) {
		r := resOfVkgl(s.res)
		if r == nil {
			return nil, nil
		}
		vs, err := r.surface(s)
		if err != nil {
			return nil, err
		}
		r.incRef()
		held = append(held, r)
		return vs, nil
	}
	release := func(rs []*vkglResource) {
		for _, r := range rs {
			r.decRef()
		}
	}
	for i, s := range cbufs {
		vs, err := hold(s)
		if err != nil {
			release(held)
			return err
		}
		fb.Colors[i] = vs
	}
	vs, err := hold(zsbuf)
	if err != nil {
		release(held)
		return err
	}
	fb.Depth = vs
	// The batch keeps the framebuffer's resources until the GPU is done.
	for _, r := range held {
		if err := c.use(&r.vkglRef); err != nil {
			release(held)
			return err
		}
	}
	c.ctx.SetFramebuffer(fb)
	release(c.fbRes)
	c.fb, c.fbRes = fb, held
	return nil
}

func (c *vkglContext) setFramebufferNoAttach(width, height, layers, samples uint32) error {
	if len(c.fbRes) != 0 {
		// Mesa's virgl driver sends this after each framebuffer, which
		// has attachments: only a framebuffer of none is of it.
		return nil
	}
	c.fb = vkgl.Framebuffer{Width: width, Height: height, Layers: layers, Samples: samples}
	c.ctx.SetFramebuffer(c.fb)
	return nil
}

func (c *vkglContext) setViewports(start uint32, d []uint32) {
	if vps := vkgl.DecodeViewports(d); start == 0 && len(vps) != 0 {
		c.viewport = vps[0]
	}
}

func (c *vkglContext) setScissors(start uint32, d []uint32) {
	if scs := vkgl.DecodeScissors(d); start == 0 && len(scs) != 0 {
		c.scissor = scs[0]
	}
}

func (c *vkglContext) setVertexBuffers(res []backendResource, offsets []uint32) {
	for _, r := range c.vbs {
		if r.res != nil {
			r.res.decRef()
		}
	}
	c.vbs = c.vbs[:0]
	for i, br := range res {
		r := resOfVkgl(br)
		if r != nil {
			r.incRef()
		}
		c.vbs = append(c.vbs, vkglRange{res: r, offset: offsets[i]})
	}
}

func (c *vkglContext) setConstants(stage uint32, d []uint32) {
	if stage < maxShaderStages {
		c.constants[stage] = append(c.constants[stage][:0], d...)
	}
}

func (c *vkglContext) setUniformBuffer(stage, index uint32, br backendResource, offset, size uint32) error {
	if stage >= maxShaderStages || index >= maxConstantBuffers {
		return fmt.Errorf("uniform buffer %d of stage %d", index, stage)
	}
	r := resOfVkgl(br)
	if r != nil && r.buf == nil {
		return errors.New("a uniform buffer of a texture")
	}
	if old := c.ubos[stage][index].res; old != nil {
		old.decRef()
	}
	if r != nil {
		r.incRef()
	}
	c.ubos[stage][index] = vkglRange{res: r, offset: offset, size: size}
	if index == 0 {
		c.constants[stage] = nil
	}
	return nil
}

func (c *vkglContext) setSamplerViews(stage, start uint32, views []any) {
	for i, v := range views {
		slot := &c.views[stage][start+uint32(i)]
		if *slot != nil {
			(*slot).decRef()
		}
		*slot, _ = v.(*vkglView)
		if *slot != nil {
			(*slot).incRef()
		}
	}
}

func (c *vkglContext) bindSamplers(stage, start uint32, samplers []any) {
	for i, s := range samplers {
		slot := &c.samplers[stage][start+uint32(i)]
		if *slot != nil {
			(*slot).decRef()
		}
		*slot, _ = s.(*vkglSampler)
		if *slot != nil {
			(*slot).incRef()
		}
	}
}

func (c *vkglContext) setStencilRef(v uint32) {
	c.stencilRef = [2]uint32{v & 0xff, v >> 8 & 0xff}
}

func (c *vkglContext) setBlendColor(d []uint32) {
	for i := range c.blendColor {
		c.blendColor[i] = math.Float32frombits(d[i])
	}
}

func (c *vkglContext) setPolygonStipple(d []uint32) {}

func (c *vkglContext) setClipState(d []uint32) {
	for i := range c.clip {
		for j := range c.clip[i] {
			c.clip[i][j] = math.Float32frombits(d[4*i+j])
		}
	}
}

func (c *vkglContext) setSampleMask(mask uint32) {
	c.sampleMask = mask
}

func (c *vkglContext) setMinSamples(n uint32) {
	c.minSamples = n
}

func (c *vkglContext) setTessState(d []uint32) {}

// unbinding returns errVkgl unless res are of no resources, of which
// guests' Mesa unbinds slots of shader buffers and images that the
// backend renders none of yet: of them, nothing.
func unbinding(what string, res []backendResource) error {
	for _, r := range res {
		if r != nil {
			return fmt.Errorf("%s: %w", what, errVkgl)
		}
	}
	return nil
}

func (c *vkglContext) setShaderBuffers(stage, start uint32, res []backendResource, offsets, sizes []uint32) error {
	return unbinding("shader buffers", res)
}

func (c *vkglContext) setAtomicBuffers(start uint32, res []backendResource, offsets, sizes []uint32) error {
	return unbinding("atomic buffers", res)
}

func (c *vkglContext) setShaderImages(stage, start uint32, res []backendResource, d []uint32) error {
	return unbinding("shader images", res)
}

func (c *vkglContext) renderCondition(q any, condition bool, mode uint32) {}

func (c *vkglContext) clear(d []uint32) {
	depth := math.Float64frombits(uint64(d[5]) | uint64(d[6])<<32)
	c.ctx.Clear(d[0], [4]uint32{d[1], d[2], d[3], d[4]}, depth, d[7])
}

func (c *vkglContext) clearTexture(br backendResource, d []uint32) error {
	r := resOfVkgl(br)
	if r.tex == nil {
		return errors.New("a clear of a buffer's texels")
	}
	// The payload: the resource, its level, a box, whose z and depth are
	// of 16 bits, as Gallium's boxes', and a texel of its format.
	box := vkgl.Box{X: int64(int32(d[2])), Y: int64(int32(d[3])), Z: int64(int16(d[4])),
		Width: int64(int32(d[5])), Height: int64(int32(d[6])), Depth: int64(int16(d[7]))}
	var texel [16]byte
	for i := range 4 {
		binary.LittleEndian.PutUint32(texel[4*i:], d[8+i])
	}
	if err := c.use(&r.vkglRef); err != nil {
		return err
	}
	return c.ctx.ClearTexture(r.tex, d[1], box, texel[:])
}

func (c *vkglContext) clearSurface(s surface, d []uint32) error {
	r := resOfVkgl(s.res)
	if r == nil || r.tex == nil {
		return errors.New("a clear of a buffer's surface")
	}
	vs, err := r.surface(s)
	if err != nil {
		return err
	}
	if err := c.use(&r.vkglRef); err != nil {
		return err
	}
	// The payload: its buffers, and whether of the render condition, the
	// surface, a color, or a depth and a stencil, and a rect.
	buffers := d[0] >> 1 & 7
	if buffers&vkgl.ClearColor0 != 0 {
		buffers = vkgl.ClearColor0
	}
	// Of a color of a color surface, and of depth and stencil of a depth
	// and stencil one.
	if zs := vs.Format.IsDepth() || vs.Format.IsStencil(); buffers != 0 && zs == (buffers == vkgl.ClearColor0) {
		return fmt.Errorf("a clear of buffers %#x of a surface of %s", buffers, vs.Format.Name)
	}
	var color [4]uint32
	copy(color[:], d[2:6])
	depth := math.Float64frombits(uint64(d[2]) | uint64(d[3])<<32)
	return c.ctx.ClearSurface(vs, buffers, color, depth, d[4], d[6], d[7], d[8], d[9])
}

func (c *vkglContext) blit(dst, src backendResource, d []uint32) error {
	dr, sr := resOfVkgl(dst), resOfVkgl(src)
	if dr.tex == nil || sr.tex == nil {
		return errors.New("a blit of a buffer")
	}
	// The payload: its mask, filter, and flags, its scissor, and of the
	// destination and the source, the resource, its level, a format, and
	// a box, whose z and depth are of 16 bits, as Gallium's boxes'.
	box := func(d []uint32) vkgl.Box {
		return vkgl.Box{X: int64(int32(d[0])), Y: int64(int32(d[1])), Z: int64(int16(d[2])),
			Width: int64(int32(d[3])), Height: int64(int32(d[4])), Depth: int64(int16(d[5]))}
	}
	t := d[0]
	if filter := t >> 8 & 3; filter > 1 {
		return fmt.Errorf("a blit of filter %d", filter)
	}
	b := vkgl.Blit{Dst: dr.tex, Src: sr.tex, DstLevel: d[4], SrcLevel: d[13], DstFormat: d[5], SrcFormat: d[14],
		DstBox: box(d[6:12]), SrcBox: box(d[15:21]), Mask: t & 0xff, Linear: t>>8&3 == 1}
	if t>>10&1 != 0 {
		b.Scissor = &vkgl.Scissor{MinX: d[1] & 0xffff, MinY: d[1] >> 16, MaxX: d[2] & 0xffff, MaxY: d[2] >> 16}
	}
	if t>>12&1 != 0 {
		return fmt.Errorf("a blit of blending: %w", errVkgl)
	}
	if err := c.use(&dr.vkglRef); err != nil {
		return err
	}
	if err := c.use(&sr.vkglRef); err != nil {
		return err
	}
	if err := c.ctx.Blit(b); err != nil {
		if errors.Is(err, vkgl.ErrNotNative) {
			return fmt.Errorf("%w: %w", err, errVkgl)
		}
		return err
	}
	return nil
}

func (c *vkglContext) copyRegion(dst, src backendResource, d []uint32) error {
	dr, sr := resOfVkgl(dst), resOfVkgl(src)
	// The payload: the destination, its level, x, y and z, the source,
	// its level, and its box, whose z and depth are of 16 bits, as
	// Gallium's boxes'.
	box := vkgl.Box{X: int64(int32(d[7])), Y: int64(int32(d[8])), Z: int64(int16(d[9])),
		Width: int64(int32(d[10])), Height: int64(int32(d[11])), Depth: int64(int16(d[12]))}
	x, y, z := int64(int32(d[2])), int64(int32(d[3])), int64(int16(d[4]))
	if (dr.buf == nil) != (sr.buf == nil) {
		return errors.New("a copy between a buffer and a texture")
	}
	if dr.buf != nil {
		// Of bytes, of a buffer's only level and row.
		if d[1] != 0 || d[6] != 0 || x < 0 || y != 0 || z != 0 || box.X < 0 || box.Width < 0 || box.Y != 0 || box.Z != 0 ||
			box.Height > 1 || box.Depth > 1 {
			return fmt.Errorf("a copy of %+v of a buffer to %d", box, x)
		}
		if err := c.use(&dr.vkglRef); err != nil {
			return err
		}
		if err := c.use(&sr.vkglRef); err != nil {
			return err
		}
		return c.ctx.CopyBuffer(dr.buf, uint64(x), sr.buf, uint64(box.X), uint64(box.Width))
	}
	if _, err := vkgl.CheckCopy(dr.tex, d[1], x, y, z, sr.tex, d[6], box); err != nil {
		return err
	}
	if err := c.use(&dr.vkglRef); err != nil {
		return err
	}
	if err := c.use(&sr.vkglRef); err != nil {
		return err
	}
	return c.ctx.CopyTexture(dr.tex, d[1], x, y, z, sr.tex, d[6], box)
}

func (c *vkglContext) inlineWrite(br backendResource, d []uint32, data []byte) error {
	r := resOfVkgl(br)
	if r.buf == nil {
		// The payload's box, whose z and depth are of 16 bits, as
		// Gallium's boxes', of rows and layers that the guest packs, and
		// says so, or of the box's size.
		box := vkgl.Box{X: int64(int32(d[5])), Y: int64(int32(d[6])), Z: int64(int16(d[7])),
			Width: int64(int32(d[8])), Height: int64(int32(d[9])), Depth: int64(int16(d[10]))}
		if box.Width <= 0 || box.Height <= 0 || box.Depth <= 0 {
			return fmt.Errorf("an inline write of %+v", box)
		}
		f := r.tex.Format
		bw, bh := int64(f.BlockWidth), int64(f.BlockHeight)
		stride, layerStride := d[3], d[4]
		if stride == 0 {
			stride = uint32((box.Width+bw-1)/bw) * f.BlockBytes
		}
		if layerStride == 0 {
			layerStride = stride * uint32((box.Height+bh-1)/bh)
		}
		if err := c.use(&r.vkglRef); err != nil {
			return err
		}
		return c.ctx.WriteTexture(r.tex, vkgl.TextureRegion{Level: d[1], Box: box, Stride: stride, LayerStride: layerStride}, data, 0)
	}
	// The payload's box: d[1] level, d[2] usage, d[3] stride, d[4] layer
	// stride, then x, y, z, w, h, d.
	x, w := uint64(d[5]), uint64(d[8])
	if w > uint64(len(data)) || x > r.buf.Size() || w > r.buf.Size()-x {
		return fmt.Errorf("an inline write of %d bytes at %d", w, x)
	}
	if err := c.use(&r.vkglRef); err != nil {
		return err
	}
	return c.ctx.WriteBuffer(r.buf, x, data[:w])
}

func (c *vkglContext) launchGrid(d []uint32, indirect backendResource) error {
	return errVkgl
}

func (c *vkglContext) memoryBarrier(flags uint32) {}

func (c *vkglContext) textureBarrier(flags uint32) {}

func (c *vkglContext) beginQuery(q any) bool {
	vq, ok := q.(*vkglQuery)
	return ok && (vq.q == nil || c.ctx.BeginQuery(vq.q) == nil)
}

func (c *vkglContext) endQuery(q any) bool {
	vq, ok := q.(*vkglQuery)
	return ok && (vq.q == nil || c.ctx.EndQuery(vq.q) == nil)
}

// result returns q's result, as Gallium's of its type, and whether it has
// it, of the context's commands, which it waits for if wait.
func (q *vkglQuery) result(c *vkglContext, wait bool) (uint64, bool) {
	if q.q == nil {
		// Of the frequency of timestamps, which are nanoseconds, of no
		// disjoint ones.
		return 1e9, true
	}
	value, ok := c.ctx.QueryResult(q.q, wait)
	switch q.typ {
	case queryOcclusionPredicate, queryOcclusionConservative:
		if value != 0 {
			value = 1
		}
	}
	return value, ok
}

func (c *vkglContext) queryResult(q any, typ uint32, wait bool, state []byte) bool {
	vq, ok := q.(*vkglQuery)
	if !ok {
		return false
	}
	value, ok := vq.result(c, wait)
	if !ok {
		return false
	}
	// virglrenderer's sizes: timer queries' results are 64-bit, others'
	// 32.
	size := uint32(4)
	if vq.typ == queryTimestamp || vq.typ == queryTimeElapsed {
		size = 8
	}
	binary.LittleEndian.PutUint32(state[0:], queryStateDone)
	binary.LittleEndian.PutUint32(state[4:], size)
	binary.LittleEndian.PutUint64(state[8:], value)
	return true
}

func (c *vkglContext) queryResultQBO(q any, qbo backendResource, wait bool, resultType, offset uint32, index int32) error {
	vq, ok := q.(*vkglQuery)
	r := resOfVkgl(qbo)
	if !ok || r == nil || r.buf == nil || resultType > queryTypeU64 {
		return fmt.Errorf("a query's result of type %d to a buffer", resultType)
	}
	size := uint64(4)
	if resultType >= queryTypeI64 {
		size = 8
	}
	if uint64(offset) > r.buf.Size() || size > r.buf.Size()-uint64(offset) {
		return fmt.Errorf("a query's result at %d of a buffer of %d bytes", offset, r.buf.Size())
	}
	value, ready := vq.result(c, wait)
	switch {
	case index < 0:
		// Whether the result is ready.
		value = 0
		if ready {
			value = 1
		}
	case !ready:
		// Gallium's buffers keep what they had of results that are not.
		return nil
	}
	// Of 32 bits, clamped, as Gallium's.
	switch resultType {
	case queryTypeI32:
		value = min(value, math.MaxInt32)
	case queryTypeU32:
		value = min(value, math.MaxUint32)
	case queryTypeI64:
		value = min(value, math.MaxInt64)
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], value)
	if err := c.use(&r.vkglRef); err != nil {
		return err
	}
	return c.ctx.WriteBuffer(r.buf, uint64(offset), b[:size])
}

// formats returns the Vulkan formats of the framebuffer's color buffers,
// of 0 where it has none, and of its depth and stencil buffer, and the
// tospirv types of its colors.
func (c *vkglContext) formats() (colors []uint32, depth uint32, hasDepth, hasStencil bool, types [tospirv.MaxColorBuffers]tospirv.BaseType) {
	colors = make([]uint32, len(c.fb.Colors))
	for i, s := range c.fb.Colors {
		if s == nil {
			continue
		}
		colors[i] = s.Format.VK
		switch integer, signed := s.Format.IsInteger(); {
		case integer && signed:
			types[i] = tospirv.Int
		case integer:
			types[i] = tospirv.Uint
		}
	}
	if s := c.fb.Depth; s != nil {
		depth, hasDepth, hasStencil = s.Format.VK, s.Format.IsDepth(), s.Format.IsStencil()
	}
	return colors, depth, hasDepth, hasStencil, types
}

// usesSrc1 reports whether a blend state's color buffer 0 blends of a
// second source, of dual-source blending.
func usesSrc1(b *vkgl.Blend) bool {
	rt := b.RT[0]
	if !rt.Enable {
		return false
	}
	for _, f := range []uint32{rt.RGBSrc, rt.RGBDst, rt.AlphaSrc, rt.AlphaDst} {
		// PIPE_BLENDFACTOR_SRC1_COLOR, SRC1_ALPHA, and their inverses.
		if f&^0x10 == 0x9 || f&^0x10 == 0xa {
			return true
		}
	}
	return false
}

// The decoder's draw's payload: virgl's draw_vbo.
const (
	drawStart = iota
	drawCount
	drawMode
	drawIndexed
	drawInstances
	drawIndexBias
	drawStartInstance
	drawPrimitiveRestart
	drawRestartIndex
)

func (c *vkglContext) draw(d []uint32, a drawArgs) error {
	if a.indirect != nil {
		return fmt.Errorf("an indirect draw: %w", errVkgl)
	}
	topology, ok := c.b.d.VkTopology(d[drawMode])
	if !ok {
		return fmt.Errorf("a draw of primitives %d", d[drawMode])
	}
	vs, fs, ve := c.shaders[stageVertex], c.shaders[stageFragment], c.velems
	if vs == nil || fs == nil || ve == nil || c.blend == nil || c.rast == nil || c.dsa == nil {
		// Like virglrenderer, the decoder drops such draws.
		return nil
	}
	// The indexes first: widening them reads them back, which submits the
	// context's batch, of which the draw's uploads and resources must not
	// be.
	var indexes *vkgl.Buffer
	indexSize := a.indexSize
	var firstIndex uint32
	if d[drawIndexed] != 0 {
		index := resOfVkgl(a.index)
		if index == nil || index.buf == nil {
			return errors.New("an indexed draw of no index buffer")
		}
		widen := indexSize == 1 && !c.b.d.IndexSizeOK(1)
		if !widen && !c.b.d.IndexSizeOK(indexSize) {
			return fmt.Errorf("indexes of %d bytes: %w", indexSize, errVkgl)
		}
		// Of indexes of the buffer, which it checks, and draws them:
		// from the offset's index, which Vulkan binds them at a multiple
		// of their size of. The offset is that of the draw's first index:
		// Mesa's virgl driver sets the index buffer's offset of the
		// draw's start, which it sends too, and which virglrenderer, as
		// the renderer, ignores of indexed draws.
		size := index.buf.Size()
		start := uint64(a.indexOffset)
		if n := uint64(d[drawCount]) * uint64(indexSize); start%uint64(indexSize) != 0 || start > size || n > size-start {
			return fmt.Errorf("%d indexes of %d bytes from %d of a buffer of %d bytes", d[drawCount], indexSize, start, size)
		}
		firstIndex = a.indexOffset / indexSize
		if err := c.use(&index.vkglRef); err != nil {
			return err
		}
		indexes = index.buf
		if widen {
			var err error
			if indexes, err = c.ctx.WidenIndexes(index.buf, start, d[drawCount], d[drawPrimitiveRestart] != 0, d[drawRestartIndex]); err != nil {
				return err
			}
			indexSize, firstIndex = 2, 0
		}
	}
	colors, depthFormat, hasDepth, hasStencil, colorTypes := c.formats()

	var vsOpts, fsOpts tospirv.Options
	for i := range ve.elements {
		switch {
		case vs.sint&(1<<i) != 0:
			vsOpts.InputTypes[i] = tospirv.Int
		case vs.uint&(1<<i) != 0:
			vsOpts.InputTypes[i] = tospirv.Uint
		}
	}
	if !c.rast.PointSizePerVertex {
		vsOpts.PointSize = c.rast.PointSize
	}
	for i := range c.clip {
		if c.rast.ClipPlaneEnable&(1<<i) != 0 {
			vsOpts.UserClipPlanes = append(vsOpts.UserClipPlanes, c.clip[i])
		}
	}
	vsOpts.ClipDisable = uint8(^c.rast.ClipPlaneEnable)
	vsOpts.StreamOutput = vs.so
	vsOpts.GLClipZ = !c.rast.ClipHalfZ
	fsOpts.ColorBuffers = len(colors)
	fsOpts.ColorTypes = colorTypes
	fsOpts.FlatShade, fsOpts.TwoSided = c.rast.FlatShade, c.rast.LightTwoSide
	if c.dsa.AlphaEnable {
		fsOpts.AlphaTest, fsOpts.AlphaFunc, fsOpts.AlphaRef = true, c.dsa.AlphaFunc, c.dsa.AlphaRef
	}
	if d[drawMode] == 0 && c.rast.SpriteCoordEnable != 0 {
		fsOpts.SpriteCoords, fsOpts.SpriteCoordsLowerLeft = c.rast.SpriteCoordEnable, c.rast.SpriteCoordMode == 1
	}
	fsOpts.DualSourceBlend = usesSrc1(c.blend)
	vsRes, err := vs.s.Translate(vsOpts)
	if err != nil {
		return fmt.Errorf("the vertex shader: %w", err)
	}
	fsRes, err := fs.s.Translate(fsOpts)
	if err != nil {
		return fmt.Errorf("the fragment shader: %w", err)
	}
	if missing := vkgl.Link(vsRes, fsRes); len(missing) != 0 {
		vsOpts.ExtraOutputs = missing
		if vsRes, err = vs.s.Translate(vsOpts); err != nil {
			return fmt.Errorf("the vertex shader: %w", err)
		}
		vkgl.Link(vsRes, fsRes)
	}

	var bindings []vkgl.VertexBinding
	var inputs []vkgl.VertexInput
	for i, e := range ve.elements {
		stride := uint32(0)
		if int(e.Buffer) < len(ve.strides) {
			stride = ve.strides[e.Buffer]
		}
		bindings = append(bindings, vkgl.VertexBinding{Binding: uint32(i), Stride: stride, Instance: e.InstanceDivisor != 0,
			Divisor: e.InstanceDivisor})
		inputs = append(inputs, vkgl.VertexInput{Location: uint32(i), Binding: uint32(i), Format: e.Format.VK, Offset: e.Offset})
	}
	var resources []vkgl.StageResource
	for _, sr := range []struct {
		stage tgsi.Processor
		res   *tospirv.Result
	}{{tgsi.ProcessorVertex, vsRes}, {tgsi.ProcessorFragment, fsRes}} {
		for _, r := range sr.res.Resources {
			resources = append(resources, vkgl.StageResource{Stage: sr.stage, Resource: r})
		}
	}
	desc := vkgl.PipelineDesc{
		VS: vsRes.Words, FS: fsRes.Words, Resources: resources, Bindings: bindings, Inputs: inputs,
		Topology: topology, PrimitiveRestart: d[drawPrimitiveRestart] != 0,
		Rasterizer: c.rast, DSA: c.dsa, Blend: c.blend,
		ColorFormats: colors, DepthFormat: depthFormat, HasDepth: hasDepth, HasStencil: hasStencil,
		Samples: c.fbSamples(), SampleMask: c.sampleMask, MinSamples: c.minSamples,
	}
	key := pipelineKey(&desc)
	p := c.pipelines[key]
	if p == nil {
		if p, err = c.b.d.NewPipeline(desc); err != nil {
			return err
		}
		c.pipelines[key] = p
	}

	buffers, textures, err := c.bindings(vsRes, fsRes, d)
	if err != nil {
		return err
	}
	vbufs := make([]*vkgl.Buffer, len(ve.elements))
	voffs := make([]uint64, len(ve.elements))
	for i, e := range ve.elements {
		if int(e.Buffer) >= len(c.vbs) || c.vbs[e.Buffer].res == nil || c.vbs[e.Buffer].res.buf == nil {
			return nil
		}
		vb := c.vbs[e.Buffer]
		if err := c.use(&vb.res.vkglRef); err != nil {
			return err
		}
		vbufs[i], voffs[i] = vb.res.buf, uint64(vb.offset)
	}
	width, height, _ := c.area()
	if width == 0 || height == 0 {
		// Of a framebuffer of no area: nothing.
		return nil
	}
	return c.ctx.Render(func(cmds *vkgl.Commands) error {
		cmds.BindPipeline(p)
		x, y, w, h, minZ, maxZ := c.viewport.VkViewport(c.rast.ClipHalfZ)
		cmds.SetViewport(x, y, w, h, minZ, maxZ)
		if c.rast.Scissor {
			s := c.scissor
			cmds.SetScissor(int32(s.MinX), int32(s.MinY), uint32(max(int32(s.MaxX)-int32(s.MinX), 0)),
				uint32(max(int32(s.MaxY)-int32(s.MinY), 0)))
		} else {
			cmds.SetScissor(0, 0, width, height)
		}
		cmds.SetBlendConstants(c.blendColor)
		cmds.SetStencilReference(c.stencilRef[0], c.stencilRef[1])
		if err := c.ctx.BindResources(cmds, p, buffers, textures); err != nil {
			return err
		}
		var push [tospirv.PushConstantsSize]byte
		binary.LittleEndian.PutUint32(push[tospirv.PushFramebufferHeight:], math.Float32bits(float32(height)))
		binary.LittleEndian.PutUint32(push[tospirv.PushBaseInstance:], d[drawStartInstance])
		cmds.PushConstants(p, 0, push[:])
		cmds.BindVertexBuffers(0, vbufs, voffs)
		if indexes != nil {
			cmds.BindIndexBuffer(indexes, 0, indexSize)
			cmds.DrawIndexed(d[drawCount], max(d[drawInstances], 1), firstIndex, int32(d[drawIndexBias]), d[drawStartInstance])
		} else {
			cmds.DrawVertices(d[drawCount], max(d[drawInstances], 1), d[drawStart], d[drawStartInstance])
		}
		return nil
	})
}

// area returns the framebuffer's area and layers.
func (c *vkglContext) area() (width, height, layers uint32) {
	width, height, layers = c.fb.Width, c.fb.Height, max(c.fb.Layers, 1)
	some := false
	for _, s := range append(append([]*vkgl.Surface(nil), c.fb.Colors...), c.fb.Depth) {
		if s == nil {
			continue
		}
		w, h := max(s.Texture.Desc.Width>>s.Level, 1), max(s.Texture.Desc.Height>>s.Level, 1)
		if !some {
			width, height, some = w, h, true
			continue
		}
		width, height = min(width, w), min(height, h)
	}
	return width, height, layers
}

// fbSamples returns the framebuffer's samples.
func (c *vkglContext) fbSamples() uint32 {
	for _, s := range append(append([]*vkgl.Surface(nil), c.fb.Colors...), c.fb.Depth) {
		if s != nil {
			return max(s.Texture.Desc.Samples, 1)
		}
	}
	return max(c.fb.Samples, 1)
}

// pipelineKey returns a key of desc, of which pipelines are the same.
func pipelineKey(desc *vkgl.PipelineDesc) string {
	var b strings.Builder
	words := func(ws []uint32) {
		for _, w := range ws {
			fmt.Fprintf(&b, "%x,", w)
		}
		b.WriteByte('|')
	}
	words(desc.VS)
	words(desc.FS)
	fmt.Fprintf(&b, "%v|%v|%d|%v|%+v|%+v|%+v|%v|%d|%v|%v|%d|%d|%d",
		desc.Bindings, desc.Inputs, desc.Topology, desc.PrimitiveRestart, *desc.Rasterizer, *desc.DSA, *desc.Blend,
		desc.ColorFormats, desc.DepthFormat, desc.HasDepth, desc.HasStencil, desc.Samples, desc.SampleMask, desc.MinSamples)
	return b.String()
}

// bindings returns the buffers and textures that shaders of vsRes and
// fsRes read, of the draw d: of the bound state, and of the backend's
// dummies where it binds none.
func (c *vkglContext) bindings(vsRes, fsRes *tospirv.Result, d []uint32) ([]vkgl.BufferBinding, []vkgl.TextureBinding, error) {
	var buffers []vkgl.BufferBinding
	var textures []vkgl.TextureBinding
	for _, sr := range []struct {
		stage    tgsi.Processor
		virgl    uint32
		resource *tospirv.Result
	}{{tgsi.ProcessorVertex, stageVertex, vsRes}, {tgsi.ProcessorFragment, stageFragment, fsRes}} {
		for _, r := range sr.resource.Resources {
			for slot := r.Slot; slot < r.Slot+max(r.Count, 1); slot++ {
				switch r.Kind {
				case tospirv.UniformBuffer:
					bb, err := c.uniformBinding(sr.stage, sr.virgl, slot)
					if err != nil {
						return nil, nil, err
					}
					buffers = append(buffers, bb)
				case tospirv.SampledTexture:
					if r.Target == tgsi.TextureBuffer {
						return nil, nil, fmt.Errorf("a texture buffer: %w", errVkgl)
					}
					tb := vkgl.TextureBinding{Stage: sr.stage, Slot: slot, View: c.b.dummyView, Sampler: c.b.dummySampler}
					if v := c.views[sr.virgl][slot]; v != nil {
						if err := c.use(&v.vkglRef); err != nil {
							return nil, nil, err
						}
						tb.View = v.view
					}
					if s := c.samplers[sr.virgl][slot]; s != nil {
						if err := c.use(&s.vkglRef); err != nil {
							return nil, nil, err
						}
						tb.Sampler = s.sampler
					}
					textures = append(textures, tb)
				case tospirv.StorageBuffer:
					bb, err := c.storageBinding(sr.stage, sr.virgl, slot)
					if err != nil {
						return nil, nil, err
					}
					buffers = append(buffers, bb)
				default:
					return nil, nil, fmt.Errorf("a resource of kind %d: %w", r.Kind, errVkgl)
				}
			}
		}
	}
	return buffers, textures, nil
}

// uniformBinding returns the binding of a stage's uniform buffer slot: the
// stage's constants, of slot 0 where the guest set them, or the range
// bound.
func (c *vkglContext) uniformBinding(stage tgsi.Processor, virgl uint32, slot int) (vkgl.BufferBinding, error) {
	bb := vkgl.BufferBinding{Stage: stage, Kind: tospirv.UniformBuffer, Slot: slot}
	if slot == 0 && c.constants[virgl] != nil {
		data := make([]byte, 4*max(len(c.constants[virgl]), 4))
		for i, w := range c.constants[virgl] {
			binary.LittleEndian.PutUint32(data[4*i:], w)
		}
		buf, err := c.ctx.Upload(data)
		if err != nil {
			return bb, err
		}
		bb.Buffer, bb.Range = buf, uint64(len(data))
		return bb, nil
	}
	if virgl == stageVertex && slot == vkglXfbUBO && c.xfbParams != nil {
		data := make([]byte, 4*len(c.xfbParams))
		for i, w := range c.xfbParams {
			binary.LittleEndian.PutUint32(data[4*i:], w)
		}
		buf, err := c.ctx.Upload(data)
		if err != nil {
			return bb, err
		}
		bb.Buffer, bb.Range = buf, uint64(len(data))
		return bb, nil
	}
	if slot < maxConstantBuffers {
		if rg := c.ubos[virgl][slot]; rg.res != nil {
			if err := c.use(&rg.res.vkglRef); err != nil {
				return bb, err
			}
			bb.Buffer, bb.Offset, bb.Range = rg.res.buf, uint64(rg.offset), uint64(rg.size)
			return bb, nil
		}
	}
	// A slot of none: zeros.
	buf, err := c.ctx.Upload(make([]byte, 16))
	if err != nil {
		return bb, err
	}
	bb.Buffer, bb.Range = buf, 16
	return bb, nil
}

// storageBinding returns the binding of a stage's shader buffer slot: of
// the vertex shader's capture of its stream output, the buffer of a target,
// whole, as the decoder's draws give the shader its offsets, or zeros of
// none.
func (c *vkglContext) storageBinding(stage tgsi.Processor, virgl uint32, slot int) (vkgl.BufferBinding, error) {
	bb := vkgl.BufferBinding{Stage: stage, Kind: tospirv.StorageBuffer, Slot: slot}
	i := slot - vkglXfbSSBO
	if virgl != stageVertex || i < 0 || i >= maxStreamoutTargets {
		return bb, fmt.Errorf("shader buffer %d of stage %d: %w", slot, virgl, errVkgl)
	}
	if i < len(c.xfbTargets) {
		if r := resOfVkgl(c.xfbTargets[i]); r != nil && r.buf != nil {
			if err := c.use(&r.vkglRef); err != nil {
				return bb, err
			}
			bb.Buffer, bb.Range = r.buf, r.buf.Size()
			return bb, nil
		}
	}
	buf, err := c.ctx.Upload(make([]byte, 16))
	if err != nil {
		return bb, err
	}
	bb.Buffer, bb.Range = buf, 16
	return bb, nil
}
