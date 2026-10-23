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
	"math/bits"
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
// its capability sets offer guests none of. TODO: what OpenGL ES 3.2 needs:
// geometry and tessellation shaders, and texture buffers
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
// captures its stream output; of a compute shader, its shared memory, in
// bytes.
type vkglShader struct {
	s          *vkgl.Shader
	stage      uint32
	sint, uint uint32
	so         *tospirv.StreamOutput
	sharedMem  uint32
	// samplers are the sampler slots that the shader declares, of bits,
	// and writesSampleMask whether a fragment shader writes SAMPLEMASK.
	samplers         uint32
	writesSampleMask bool
}

// vkglView is a sampler view, and the resource that it holds a reference
// to.
type vkglView struct {
	vkglRef
	res  *vkglResource
	view *vkgl.SamplerView
}

// vkglSampler is a sampler state, and its Vulkan sampler, of the border
// color of Vulkan's nearest the state's, and others of other border
// colors, VkBorderColor, as views' formats and translations' blending of
// borders need them.
type vkglSampler struct {
	vkglRef
	st       *vkgl.SamplerState
	sampler  *vkgl.Sampler
	variants map[uint32]*vkgl.Sampler
}

// variant returns the state's sampler of a border color, VkBorderColor,
// creating it the first time.
func (s *vkglSampler) variant(d *vkgl.Device, border uint32) (*vkgl.Sampler, error) {
	if v := s.variants[border]; v != nil {
		return v, nil
	}
	v, err := d.NewSamplerOfBorder(s.st, border)
	if err != nil {
		return nil, err
	}
	if s.variants == nil {
		s.variants = make(map[uint32]*vkgl.Sampler)
	}
	s.variants[border] = v
	return v, nil
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

// vkglImage is a shader image's binding: a level, and layers, of a
// texture, as a format, of which it makes views of the targets that
// shaders declare.
type vkglImage struct {
	vkglRef
	d                  *vkgl.Device
	res                *vkglResource
	format             uint32
	level, first, last uint32
	views              map[[2]uint32]*vkgl.ImageView
}

// free destroys the binding's views, of no references.
func (img *vkglImage) free() {
	for _, v := range img.views {
		img.d.DestroyImageView(v)
	}
	img.views = nil
	img.res.decRef()
}

// view returns the binding's view of target, a view type's, as format.
func (img *vkglImage) view(target, format uint32) (*vkgl.ImageView, error) {
	key := [2]uint32{target, format}
	if v := img.views[key]; v != nil {
		return v, nil
	}
	desc := vkgl.ViewDesc{Format: format, Target: target, FirstLevel: img.level, LastLevel: img.level,
		FirstLayer: img.first, LastLayer: img.last}
	switch target {
	case vkgl.Target1D, vkgl.Target2D:
		// Of one layer, the first, of a binding of one or of all.
		desc.LastLayer = desc.FirstLayer
	case vkgl.TargetCube:
		desc.LastLayer = desc.FirstLayer + 5
	case vkgl.Target3D:
		// Of the level's slices, all.
		desc.FirstLayer, desc.LastLayer = 0, 0
	}
	v, err := img.d.NewImageView(img.res.tex, desc)
	if err != nil {
		return nil, err
	}
	if img.views == nil {
		img.views = make(map[[2]uint32]*vkgl.ImageView)
	}
	img.views[key] = v
	return v, nil
}

// vkglDummyImage is an image of a texel of zeros, which shaders access of
// slots of no image.
type vkglDummyImage struct {
	tex  *vkgl.Texture
	view *vkgl.ImageView
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
	ssbos     [maxShaderStages][maxShaderBuffers]vkglRange
	images    [maxShaderStages][maxShaderImages]*vkglImage
	// dummies are the images of slots of no image, of their targets and
	// formats, and loggedZeros whether the context logged one in place of
	// a view that the device refused.
	dummies     map[[2]uint32]vkglDummyImage
	loggedZeros bool
	vbs         []vkglRange
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
		c.setShaderBuffers(uint32(stage), 0, make([]backendResource, maxShaderBuffers), make([]uint32, maxShaderBuffers),
			make([]uint32, maxShaderBuffers))
		c.setShaderImages(uint32(stage), 0, make([]backendResource, maxShaderImages), make([]uint32, 5*maxShaderImages))
	}
	c.setVertexBuffers(nil, nil)
}

func (c *vkglContext) destroy() {
	c.ctx.Destroy()
	for _, p := range c.pipelines {
		c.b.d.DestroyPipeline(p)
	}
	c.pipelines = nil
	for _, img := range c.dummies {
		c.b.d.DestroyImageView(img.view)
		c.b.d.DestroyTexture(img.tex)
	}
	c.dummies = nil
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
		log.Printf("virgl: a sampler state of %#x: %v", d, err)
		return nil
	}
	s, err := c.b.d.NewSamplerOf(st)
	if err != nil {
		log.Printf("virgl: a sampler state of %+v: %v", *st, err)
		return nil
	}
	o := &vkglSampler{st: st, sampler: s}
	o.init(func() {
		c.b.d.DestroySampler(s)
		for _, v := range o.variants {
			c.b.d.DestroySampler(v)
		}
	})
	return o
}

func (c *vkglContext) createVertexElements(elements []uint32, strides []uint32) any {
	ve, err := vkgl.DecodeVertexElements(elements)
	if err != nil {
		log.Printf("virgl: vertex elements of %v: %v", elements, err)
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
	case stageCompute:
		p = tgsi.ProcessorCompute
	default:
		return nil, fmt.Errorf("a shader of stage %d: %w", stage, errVkgl)
	}
	vs := code.(vkglCode).s
	if vs.TGSI.Processor != p {
		return nil, fmt.Errorf("TGSI of %v, of a shader of stage %d", vs.TGSI.Processor, stage)
	}
	s := &vkglShader{s: vs, stage: stage, sint: sintMask, uint: uintMask, sharedMem: sharedMem}
	for _, d := range vs.TGSI.Declarations {
		for i := d.First; d.File == tgsi.FileSampler && i <= d.Last && i < tospirv.StageBindings; i++ {
			s.samplers |= 1 << i
		}
		if d.File == tgsi.FileOutput && d.HasSemantic && d.Semantic == tgsi.SemanticSampleMask {
			s.writesSampleMask = true
		}
	}
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
		log.Printf("virgl: a sampler view: %v", err)
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
	// Of a range of the buffer, of an offset of Vulkan's alignment, as the
	// caps have guests align them.
	if r != nil {
		align := uint64(c.b.d.UniformBufferAlignment())
		if n := r.buf.Size(); uint64(offset)%align != 0 || uint64(offset) > n || uint64(size) > n-uint64(offset) {
			return fmt.Errorf("a uniform buffer of %d bytes at %d of a buffer of %d", size, offset, n)
		}
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

func (c *vkglContext) setShaderBuffers(stage, start uint32, res []backendResource, offsets, sizes []uint32) error {
	if stage >= maxShaderStages || start > maxShaderBuffers || uint32(len(res)) > maxShaderBuffers-start {
		return fmt.Errorf("shader buffers %d+%d of stage %d", start, len(res), stage)
	}
	// Of buffers' ranges, which Vulkan binds of offsets of its alignment,
	// as the caps have guests align them, and within the buffers. A range
	// of no bytes, as Mesa's state tracker binds of an offset at a
	// buffer's end, is a slot of none.
	align := uint64(c.b.d.StorageBufferAlignment())
	for i, br := range res {
		r := resOfVkgl(br)
		if r == nil {
			continue
		}
		if r.buf == nil {
			return errors.New("a shader buffer of a texture")
		}
		if size := r.buf.Size(); uint64(offsets[i])%align != 0 || uint64(offsets[i]) > size || uint64(sizes[i]) > size-uint64(offsets[i]) {
			return fmt.Errorf("a shader buffer of %d bytes at %d of a buffer of %d", sizes[i], offsets[i], size)
		}
	}
	for i, br := range res {
		slot := &c.ssbos[stage][start+uint32(i)]
		if slot.res != nil {
			slot.res.decRef()
		}
		*slot = vkglRange{}
		if r := resOfVkgl(br); r != nil && sizes[i] != 0 {
			r.incRef()
			*slot = vkglRange{res: r, offset: offsets[i], size: sizes[i]}
		}
	}
	return nil
}

func (c *vkglContext) setAtomicBuffers(start uint32, res []backendResource, offsets, sizes []uint32) error {
	// Of no atomic counters of the hardware's, as the caps say: guests'
	// counters are of shader buffers, and their atomic buffers unbound.
	for _, r := range res {
		if r != nil {
			return fmt.Errorf("atomic buffers: %w", errVkgl)
		}
	}
	return nil
}

func (c *vkglContext) setShaderImages(stage, start uint32, res []backendResource, d []uint32) error {
	if stage >= maxShaderStages || start > maxShaderImages || uint32(len(res)) > maxShaderImages-start ||
		len(d) < 5*len(res) {
		return fmt.Errorf("shader images %d+%d of stage %d", start, len(res), stage)
	}
	images := make([]*vkglImage, len(res))
	for i, br := range res {
		r := resOfVkgl(br)
		if r == nil {
			continue
		}
		if r.tex == nil {
			return fmt.Errorf("an image of a buffer: %w", errVkgl)
		}
		// Of each: its format, its access, the first and the last of its
		// layers, of 16 bits each, and its level, of the low 8 bits of
		// the last dword, whose others are Mesa's flags of views that
		// the targets that shaders declare say.
		format, layers, level := d[5*i], d[5*i+2], d[5*i+3]&0xff
		img := &vkglImage{d: c.b.d, res: r, format: format, level: level, first: layers & 0xffff, last: layers >> 16}
		desc := r.tex.Desc
		// Of a 3D texture, its slices are its layers.
		layerCount := max(desc.ArraySize, 1)
		if desc.Target == vkgl.Target3D {
			layerCount = max(desc.Depth>>level, 1)
		}
		if _, ok := vkgl.LookupFormat(format); !ok || vkgl.IsViewFormat(format) || level > desc.LastLevel ||
			img.first > img.last || img.last >= layerCount {
			return fmt.Errorf("an image of format %d, of level %d, of layers %d to %d, of %+v", format, level, img.first,
				img.last, desc)
		}
		images[i] = img
	}
	for i, img := range images {
		slot := &c.images[stage][start+uint32(i)]
		if *slot != nil {
			(*slot).decRef()
		}
		*slot = img
		if img != nil {
			img.res.incRef()
			img.init(img.free)
		}
	}
	return nil
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

// The decoder's launch_grid's payload: virgl's.
const (
	gridBlock          = 0
	gridSize           = 3
	gridIndirectOffset = 7
)

func (c *vkglContext) launchGrid(d []uint32, indirect backendResource) error {
	cs := c.shaders[stageCompute]
	if cs == nil {
		return nil
	}
	// Of a shader of a fixed block, as OpenGL's are, which Gallium's
	// block of the launch is of.
	opts := tospirv.Options{SharedMemory: int(cs.sharedMem), WorkgroupFences: c.b.d.WorkgroupFences(),
		FloatImageAtomics: c.b.d.FloatImageAtomics()}
	c.borders(stageCompute, cs, &opts)
	res, err := cs.s.Translate(opts)
	if err != nil {
		return fmt.Errorf("the compute shader: %w", err)
	}
	if block := fixedBlock(cs.s.TGSI); block != [3]uint32(d[gridBlock:gridBlock+3]) {
		return fmt.Errorf("a grid of blocks of %v of a shader of %v: %w", d[gridBlock:gridBlock+3], block, errVkgl)
	}
	var commands *vkglResource
	if indirect != nil {
		commands = resOfVkgl(indirect)
		if commands.buf == nil {
			return errors.New("a grid of a texture's commands")
		}
		// VkDispatchIndirectCommand, Gallium's grid's.
		if err := c.b.d.CheckIndirect(commands.buf, uint64(d[gridIndirectOffset]), 1, 0, 12); err != nil {
			return err
		}
		if err := c.use(&commands.vkglRef); err != nil {
			return err
		}
	} else if err := c.b.d.CheckDispatch(d[gridSize], d[gridSize+1], d[gridSize+2]); err != nil {
		return err
	}
	var resources []vkgl.StageResource
	for _, r := range res.Resources {
		resources = append(resources, vkgl.StageResource{Stage: tgsi.ProcessorCompute, Resource: r})
	}
	key := computeKey(res.Words)
	p := c.pipelines[key]
	if p == nil {
		if p, err = c.b.d.NewComputePipeline(vkgl.ComputeDesc{CS: res.Words, Resources: resources}); err != nil {
			return err
		}
		c.pipelines[key] = p
	}
	var b vkglBindings
	if err := c.stageBindings(tgsi.ProcessorCompute, stageCompute, res, &b); err != nil {
		return err
	}
	return c.ctx.Compute(func(cmds *vkgl.Commands) error {
		cmds.BindPipeline(p)
		if err := c.ctx.BindResources(cmds, p, b.buffers, b.textures, b.images); err != nil {
			return err
		}
		if commands != nil {
			cmds.DispatchIndirect(commands.buf, uint64(d[gridIndirectOffset]))
		} else {
			cmds.Dispatch(d[gridSize], d[gridSize+1], d[gridSize+2])
		}
		return nil
	})
}

// fixedBlock returns a compute shader's block, as its properties fix it:
// of 1 of each dimension that they do not.
func fixedBlock(s *tgsi.Shader) [3]uint32 {
	block := [3]uint32{1, 1, 1}
	for _, p := range s.Properties {
		switch p.Name {
		case tgsi.PropertyCSFixedBlockWidth:
			block[0] = max(p.Value, 1)
		case tgsi.PropertyCSFixedBlockHeight:
			block[1] = max(p.Value, 1)
		case tgsi.PropertyCSFixedBlockDepth:
			block[2] = max(p.Value, 1)
		}
	}
	return block
}

// computeKey returns a key of a compute pipeline of a shader's SPIR-V, of
// which pipelines are the same, apart from graphics pipelines' keys.
func computeKey(words []uint32) string {
	var b strings.Builder
	b.WriteString("compute|")
	for _, w := range words {
		fmt.Fprintf(&b, "%x,", w)
	}
	return b.String()
}

func (c *vkglContext) memoryBarrier(flags uint32) {
	if err := c.ctx.Barrier(); err != nil {
		log.Printf("virgl: a memory barrier: %v", err)
	}
}

func (c *vkglContext) textureBarrier(flags uint32) {
	if err := c.ctx.Barrier(); err != nil {
		log.Printf("virgl: a texture barrier: %v", err)
	}
}

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
	drawMinIndex
	drawMaxIndex
	drawCountFromSO
	drawVerticesPerPatch
	drawID
	drawIndirectHandle
	drawIndirectOffset
	drawIndirectStride
	drawIndirectDrawCount
)

func (c *vkglContext) draw(d []uint32, a drawArgs) error {
	topology, ok := c.b.d.VkTopology(d[drawMode])
	if !ok {
		return fmt.Errorf("a draw of primitives %d", d[drawMode])
	}
	vs, fs, ve := c.shaders[stageVertex], c.shaders[stageFragment], c.velems
	if vs == nil || fs == nil || ve == nil || c.blend == nil || c.rast == nil || c.dsa == nil {
		// Like virglrenderer, the decoder drops such draws.
		return nil
	}
	// Of an indirect draw, its commands, of Gallium's, which are Vulkan's,
	// of a buffer: of a count of the payload's, and of none of a buffer's,
	// which the caps offer guests not.
	var commands *vkglResource
	var commandCount uint32
	if a.indirect != nil {
		if a.indirectCount != nil {
			return fmt.Errorf("an indirect draw of a count of a buffer: %w", errVkgl)
		}
		if commands = resOfVkgl(a.indirect); commands.buf == nil {
			return errors.New("an indirect draw of a texture's commands")
		}
		if commandCount = d[drawIndirectDrawCount]; commandCount == 0 {
			return nil
		}
		size := uint32(16) // VkDrawIndirectCommand
		if d[drawIndexed] != 0 {
			size = 20 // VkDrawIndexedIndirectCommand
		}
		if err := c.b.d.CheckIndirect(commands.buf, uint64(d[drawIndirectOffset]), commandCount, d[drawIndirectStride], size); err != nil {
			return err
		}
	}
	// The indexes first: widening them reads them back, which submits the
	// context's batch, of which the draw's uploads and resources must not
	// be.
	var indexes *vkgl.Buffer
	indexSize := a.indexSize
	var firstIndex uint32
	var indexOffset uint64
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
		// the renderer, ignores of indexed draws. Of an indirect draw, of
		// the indexes from the offset, which its commands' first indexes
		// count from.
		size := index.buf.Size()
		start := uint64(a.indexOffset)
		count := uint64(d[drawCount])
		if commands != nil && start <= size {
			count = (size - start) / uint64(indexSize)
		}
		if n := count * uint64(indexSize); start%uint64(indexSize) != 0 || start > size || n > size-start {
			return fmt.Errorf("%d indexes of %d bytes from %d of a buffer of %d bytes", count, indexSize, start, size)
		}
		firstIndex = a.indexOffset / indexSize
		if commands != nil {
			firstIndex, indexOffset = 0, start
		}
		if err := c.use(&index.vkglRef); err != nil {
			return err
		}
		indexes = index.buf
		if widen {
			var err error
			if indexes, err = c.ctx.WidenIndexes(index.buf, start, uint32(count), d[drawPrimitiveRestart] != 0, d[drawRestartIndex]); err != nil {
				return err
			}
			indexSize, firstIndex, indexOffset = 2, 0, 0
		}
	}
	if commands != nil {
		if err := c.use(&commands.vkglRef); err != nil {
			return err
		}
	}
	colors, depthFormat, hasDepth, hasStencil, colorTypes := c.formats()

	// The vertex elements' formats as the device fetches them, and the
	// shader's conversions of those that it fetches not.
	var vsOpts, fsOpts tospirv.Options
	fetchFormats := make([]uint32, len(ve.elements))
	for i, e := range ve.elements {
		switch {
		case vs.sint&(1<<i) != 0:
			vsOpts.InputTypes[i] = tospirv.Int
		case vs.uint&(1<<i) != 0:
			vsOpts.InputTypes[i] = tospirv.Uint
		}
		vk, fetch, ok := c.b.d.VertexFetch(e.VirglFormat)
		if !ok {
			return fmt.Errorf("vertex elements of format %d: %w", e.VirglFormat, errVkgl)
		}
		fetchFormats[i], vsOpts.VertexFetches[i] = vk, fetch
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
	vsOpts.WorkgroupFences, vsOpts.FloatImageAtomics = c.b.d.WorkgroupFences(), c.b.d.FloatImageAtomics()
	fsOpts.WorkgroupFences, fsOpts.FloatImageAtomics = vsOpts.WorkgroupFences, vsOpts.FloatImageAtomics
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
	fsOpts.SingleSampled = fs.writesSampleMask && c.fbSamples() == 1
	fsOpts.SampleShading = c.minSamples > 1 && c.fbSamples() > 1
	c.borders(stageVertex, vs, &vsOpts)
	c.borders(stageFragment, fs, &fsOpts)
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

	// A binding of each element, of its vertex buffer, or of a realigned
	// copy of the vertices that the draw fetches of it, of an address or
	// a stride that the device fetches not.
	var bindings []vkgl.VertexBinding
	var inputs []vkgl.VertexInput
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
		stride := uint32(0)
		if int(e.Buffer) < len(ve.strides) {
			stride = ve.strides[e.Buffer]
		}
		buf, offset, elementOffset := vb.res.buf, uint64(vb.offset), e.Offset
		if address := offset + uint64(e.Offset); !c.b.d.VertexAligned(address, stride) {
			first, count := vertexRange(d, a, e.InstanceDivisor, buf.Size(), address, stride)
			var err error
			if buf, stride, err = c.ctx.RealignVertices(buf, address, stride, vkgl.VertexElementSize(e.VirglFormat), first, count); err != nil {
				return err
			}
			offset, elementOffset = 0, 0
		}
		vbufs[i], voffs[i] = buf, offset
		bindings = append(bindings, vkgl.VertexBinding{Binding: uint32(i), Stride: stride, Instance: e.InstanceDivisor != 0,
			Divisor: e.InstanceDivisor})
		inputs = append(inputs, vkgl.VertexInput{Location: uint32(i), Binding: uint32(i), Format: fetchFormats[i], Offset: elementOffset})
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

	var b vkglBindings
	if err := c.stageBindings(tgsi.ProcessorVertex, stageVertex, vsRes, &b); err != nil {
		return err
	}
	if err := c.stageBindings(tgsi.ProcessorFragment, stageFragment, fsRes, &b); err != nil {
		return err
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
		// A one-sided stencil test uses the front's reference for both
		// faces, as Gallium's drivers do: Mesa's state tracker leaves the
		// back's unset for the quads of scissored clears.
		back := c.stencilRef[1]
		if !c.dsa.Stencil[1].Enable {
			back = c.stencilRef[0]
		}
		cmds.SetStencilReference(c.stencilRef[0], back)
		if err := c.ctx.BindResources(cmds, p, b.buffers, b.textures, b.images); err != nil {
			return err
		}
		var push [tospirv.PushConstantsSize]byte
		binary.LittleEndian.PutUint32(push[tospirv.PushFramebufferHeight:], math.Float32bits(float32(height)))
		binary.LittleEndian.PutUint32(push[tospirv.PushBaseInstance:], d[drawStartInstance])
		cmds.PushConstants(p, 0, push[:])
		cmds.BindVertexBuffers(0, vbufs, voffs)
		if indexes != nil {
			cmds.BindIndexBuffer(indexes, indexOffset, indexSize)
		}
		switch {
		case commands != nil:
			cmds.DrawIndirect(commands.buf, uint64(d[drawIndirectOffset]), commandCount, d[drawIndirectStride], indexes != nil)
		case indexes != nil:
			cmds.DrawIndexed(d[drawCount], max(d[drawInstances], 1), firstIndex, int32(d[drawIndexBias]), d[drawStartInstance])
		default:
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

// vkglBindings are what the resources of a draw's or a dispatch's shaders
// bind.
type vkglBindings struct {
	buffers  []vkgl.BufferBinding
	textures []vkgl.TextureBinding
	images   []vkgl.ImageBinding
}

// stageBindings appends to b the bindings of the resources, res's, that a
// stage's shader uses: of the bound state, and of the backend's dummies
// where it binds none.
func (c *vkglContext) stageBindings(stage tgsi.Processor, virgl uint32, res *tospirv.Result, b *vkglBindings) error {
	// The sampler slots whose borders the translation blends.
	var bordered uint32
	for _, r := range res.Resources {
		if r.Kind == tospirv.BorderTexture {
			bordered |= 1 << r.Slot
		}
	}
	for _, r := range res.Resources {
		for slot := r.Slot; slot < r.Slot+max(r.Count, 1); slot++ {
			switch r.Kind {
			case tospirv.UniformBuffer:
				bb, err := c.uniformBinding(stage, virgl, slot)
				if err != nil {
					return err
				}
				b.buffers = append(b.buffers, bb)
			case tospirv.SampledTexture:
				if r.Target == tgsi.TextureBuffer {
					return fmt.Errorf("a texture buffer: %w", errVkgl)
				}
				tb := vkgl.TextureBinding{Stage: stage, Slot: slot, View: c.b.dummyView, Sampler: c.b.dummySampler}
				if v := c.views[virgl][slot]; v != nil {
					if err := c.use(&v.vkglRef); err != nil {
						return err
					}
					tb.View = v.view
				}
				if s := c.samplers[virgl][slot]; s != nil {
					if err := c.use(&s.vkglRef); err != nil {
						return err
					}
					tb.Sampler = s.sampler
					if v := c.views[virgl][slot]; v != nil {
						// Of the border color of the view's format's
						// type, or of a transparent black border, of a
						// translation that blends the border.
						border, _ := v.view.Border(s.st)
						if bordered&(1<<slot) != 0 {
							border &= 1
						}
						var err error
						if tb.Sampler, err = s.variant(c.b.d, border); err != nil {
							return err
						}
					}
				}
				b.textures = append(b.textures, tb)
			case tospirv.BorderTexture:
				// The view, of the sampler of an opaque white border,
				// of a slot whose border the translation blends, which
				// has both (borders).
				v, s := c.views[virgl][slot], c.samplers[virgl][slot]
				if v == nil || s == nil {
					return fmt.Errorf("a border of sampler slot %d of no view or sampler", slot)
				}
				border, _ := v.view.Border(s.st)
				sampler, err := s.variant(c.b.d, border&1+vkgl.BorderOpaqueWhite)
				if err != nil {
					return err
				}
				b.textures = append(b.textures, vkgl.TextureBinding{Stage: stage, Slot: slot, View: v.view, Sampler: sampler,
					Border: true})
			case tospirv.StorageBuffer:
				bb, err := c.storageBinding(stage, virgl, slot)
				if err != nil {
					return err
				}
				b.buffers = append(b.buffers, bb)
			case tospirv.StorageImage:
				ib, err := c.imageBinding(stage, virgl, r, slot)
				if err != nil {
					return err
				}
				b.images = append(b.images, ib)
			default:
				return fmt.Errorf("a resource of kind %d: %w", r.Kind, errVkgl)
			}
		}
	}
	return nil
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
		// Of a range of no bytes, as Mesa's state tracker binds of an
		// offset at a buffer's end, none; of a range past the device's
		// most, which Mesa binds of a buffer bound whole, of shaders
		// that read less, the most.
		if rg := c.ubos[virgl][slot]; rg.res != nil && rg.size != 0 {
			if err := c.use(&rg.res.vkglRef); err != nil {
				return bb, err
			}
			bb.Buffer, bb.Offset = rg.res.buf, uint64(rg.offset)
			bb.Range = uint64(min(rg.size, c.b.d.MaxUniformBufferRange()))
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
// whole, as the decoder's draws give the shader its offsets; of the
// guest's, the range bound, of the device's most bytes; or zeros of none.
func (c *vkglContext) storageBinding(stage tgsi.Processor, virgl uint32, slot int) (vkgl.BufferBinding, error) {
	bb := vkgl.BufferBinding{Stage: stage, Kind: tospirv.StorageBuffer, Slot: slot}
	if i := slot - vkglXfbSSBO; virgl == stageVertex && i >= 0 && i < maxStreamoutTargets {
		if i < len(c.xfbTargets) {
			if r := resOfVkgl(c.xfbTargets[i]); r != nil && r.buf != nil {
				if err := c.use(&r.vkglRef); err != nil {
					return bb, err
				}
				bb.Buffer, bb.Range = r.buf, r.buf.Size()
				return bb, nil
			}
		}
	} else if slot < maxShaderBuffers {
		if rg := c.ssbos[virgl][slot]; rg.res != nil {
			if err := c.use(&rg.res.vkglRef); err != nil {
				return bb, err
			}
			bb.Buffer, bb.Offset, bb.Range = rg.res.buf, uint64(rg.offset), uint64(min(rg.size, c.b.d.MaxStorageBufferRange()))
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

// imageTargets are the targets of the views of the images that shaders
// declare, of their TGSI's.
var imageTargets = map[tgsi.TextureTarget]uint32{
	tgsi.Texture1D:        vkgl.Target1D,
	tgsi.Texture2D:        vkgl.Target2D,
	tgsi.Texture3D:        vkgl.Target3D,
	tgsi.TextureCube:      vkgl.TargetCube,
	tgsi.Texture1DArray:   vkgl.Target1DArray,
	tgsi.Texture2DArray:   vkgl.Target2DArray,
	tgsi.TextureCubeArray: vkgl.TargetCubeArray,
}

// imageBinding returns the binding of a stage's image slot, of r, the
// image that its shader declares: a view of the bound image of r's target,
// or, of none, the context's image of zeros of it, as of an image that
// Vulkan views not of it, as of a 3D texture's slice of a 2D image: GL's
// loads of an incompatible image are of zeros, and its stores dropped.
func (c *vkglContext) imageBinding(stage tgsi.Processor, virgl uint32, r tospirv.Resource, slot int) (vkgl.ImageBinding, error) {
	ib := vkgl.ImageBinding{Stage: stage, Slot: slot}
	target, ok := imageTargets[r.Target]
	if !ok {
		return ib, fmt.Errorf("an image of %v: %w", r.Target, errVkgl)
	}
	if slot < maxShaderImages {
		if img := c.images[virgl][slot]; img != nil {
			// The view is of the format that the shader declares, as
			// OpenGL's image units' formats are, or of the binding's if
			// it declares none.
			format := img.format
			if declared, ok := vkgl.FormatByName(r.Format); ok {
				format = declared
			}
			v, err := img.view(target, format)
			if err == nil {
				if err := c.use(&img.vkglRef); err != nil {
					return ib, err
				}
				ib.View = v
				return ib, nil
			}
			if !c.loggedZeros {
				log.Printf("virgl: images of zeros in place of %v, and others: %v", r.Target, err)
				c.loggedZeros = true
			}
		}
	}
	v, err := c.dummyImage(target, r)
	if err != nil {
		return ib, err
	}
	ib.View = v
	return ib, nil
}

// dummyImage returns the context's image of zeros of target, of the
// format of r, an image that a shader declares, or of its channels' type of
// a declaration of none.
func (c *vkglContext) dummyImage(target uint32, r tospirv.Resource) (*vkgl.ImageView, error) {
	format, ok := vkgl.FormatByName(r.Format)
	if !ok {
		name := map[tospirv.BaseType]string{tospirv.Float: "R32G32B32A32_FLOAT", tospirv.Int: "R32G32B32A32_SINT",
			tospirv.Uint: "R32G32B32A32_UINT"}[r.Type]
		format, _ = vkgl.FormatByName(name)
	}
	key := [2]uint32{target, format}
	if img, ok := c.dummies[key]; ok {
		return img.view, nil
	}
	d := c.b.d
	desc := vkgl.ResourceDesc{Target: target, Format: format, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if target == vkgl.TargetCube || target == vkgl.TargetCubeArray {
		desc.ArraySize = 6
	}
	tex, err := d.NewTexture(desc)
	if err != nil {
		return nil, err
	}
	view, err := d.NewImageView(tex, vkgl.ViewDesc{Format: format, Target: target, LastLayer: desc.ArraySize - 1})
	if err != nil {
		d.DestroyTexture(tex)
		return nil, err
	}
	// Of zeros, which Vulkan's new images are not of.
	zeros := make([]byte, tex.Format.BlockBytes*desc.ArraySize)
	region := vkgl.TextureRegion{Box: vkgl.Box{Width: 1, Height: 1, Depth: int64(desc.ArraySize)}}
	if err := c.ctx.WriteTexture(tex, region, zeros, 0); err != nil {
		d.DestroyImageView(view)
		d.DestroyTexture(tex)
		return nil, err
	}
	if c.dummies == nil {
		c.dummies = make(map[[2]uint32]vkglDummyImage)
	}
	c.dummies[key] = vkglDummyImage{tex: tex, view: view}
	return view, nil
}

// borders sets opts' borders of the sampler slots that shader sh of a
// stage declares, of the views and samplers bound, whose border colors
// Vulkan's are not, which the translation blends.
func (c *vkglContext) borders(stage uint32, sh *vkglShader, opts *tospirv.Options) {
	for slots := sh.samplers; slots != 0; slots &= slots - 1 {
		slot := bits.TrailingZeros32(slots)
		v, s := c.views[stage][slot], c.samplers[stage][slot]
		if v == nil || s == nil {
			continue
		}
		if _, b := v.view.Border(s.st); b != nil {
			opts.BorderSlots |= 1 << slot
			opts.Borders[slot] = *b
		}
	}
}

// vertexRange returns the vertices that the draw d of a fetches of an
// element's vertex buffer of size bytes, from address, of stride bytes
// each, of the element's instance divisor: the draw's vertices, its
// instances, or its indexes' bounds; or, of an indirect draw or of indexes
// of no bounds, all that the buffer has; of vkgl.MaxRealigned at most.
func vertexRange(d []uint32, a drawArgs, divisor uint32, size, address uint64, stride uint32) (first, count uint32) {
	defer func() { count = min(count, vkgl.MaxRealigned) }()
	all := func() (uint32, uint32) {
		if stride == 0 || address >= size {
			return 0, 1
		}
		return 0, uint32(min((size-address)/uint64(stride)+1, vkgl.MaxRealigned))
	}
	switch {
	case a.indirect != nil:
		return all()
	case divisor != 0:
		return d[drawStartInstance], (max(d[drawInstances], 1) + divisor - 1) / divisor
	case d[drawIndexed] == 0:
		return d[drawStart], d[drawCount]
	}
	lo, hi, bias := d[drawMinIndex], d[drawMaxIndex], int64(int32(d[drawIndexBias]))
	if hi == ^uint32(0) || lo > hi || int64(lo)+bias < 0 || int64(hi)+bias > int64(^uint32(0)) {
		return all()
	}
	return uint32(int64(lo) + bias), hi - lo + 1
}
