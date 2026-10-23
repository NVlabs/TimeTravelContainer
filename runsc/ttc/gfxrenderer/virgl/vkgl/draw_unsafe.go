// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// VertexBinding is a binding of a pipeline's vertex buffers: its vertices'
// stride, of instances if Instance, of Divisor instances each if more
// than 1, as devices of instances' divisors have.
type VertexBinding struct {
	Binding, Stride uint32
	Instance        bool
	Divisor         uint32
}

// VertexInput is a vertex shader's input: its location, of a binding's
// vertices, of a Vulkan format at an offset of each.
type VertexInput struct {
	Location, Binding, Format, Offset uint32
}

// PipelineDesc describes a graphics pipeline of dynamic rendering, of the
// state that Gallium's objects hold, which the pipeline makes Vulkan's.
type PipelineDesc struct {
	// VS and FS are the shaders' SPIR-V, and Resources their resources.
	VS, FS    []uint32
	Resources []StageResource
	Bindings  []VertexBinding
	Inputs    []VertexInput
	// Topology is the primitives' VkPrimitiveTopology.
	Topology         uint32
	PrimitiveRestart bool
	Rasterizer       *Rasterizer
	DSA              *DSA
	Blend            *Blend
	// ColorFormats are the framebuffer's color buffers' Vulkan formats,
	// of 0 where it has none, and DepthFormat its depth and stencil
	// buffer's, of which HasDepth and HasStencil say.
	ColorFormats         []uint32
	DepthFormat          uint32
	HasDepth, HasStencil bool
	// Samples are the framebuffer's, and SampleMask its samples that
	// draws write; MinSamples are those that fragment shaders run of.
	Samples, SampleMask, MinSamples uint32
}

// Gallium's primitives, of mesa_prim, which the guests' drivers draw of
// Vulkan's (see vkPrimMask).
var vkTopologies = map[uint32]uint32{
	0:  0,  // POINTS: POINT_LIST
	1:  1,  // LINES: LINE_LIST
	3:  2,  // LINE_STRIP
	4:  3,  // TRIANGLES: TRIANGLE_LIST
	5:  4,  // TRIANGLE_STRIP
	6:  5,  // TRIANGLE_FAN
	10: 6,  // LINES_ADJACENCY
	11: 7,  // LINE_STRIP_ADJACENCY
	12: 8,  // TRIANGLES_ADJACENCY
	13: 9,  // TRIANGLE_STRIP_ADJACENCY
	14: 10, // PATCHES: PATCH_LIST
}

// primTriangleFan is MESA_PRIM_TRIANGLE_FAN.
const primTriangleFan = 6

// VkTopology returns the VkPrimitiveTopology of Gallium's primitive mode,
// and whether the device draws it: of triangle fans, which a device of the
// portability subset may lack, of which the caps then have guests' drivers
// draw triangles.
func (d *Device) VkTopology(mode uint32) (uint32, bool) {
	if mode == primTriangleFan && !d.triangleFans {
		return 0, false
	}
	t, ok := vkTopologies[mode]
	return t, ok
}

// Gallium's polygon modes, PIPE_POLYGON_MODE_*, of which Vulkan's,
// VkPolygonMode, are the first three in its order.
const (
	pipePolygonFill = iota
	pipePolygonLine
	pipePolygonPoint
)

// NewPipeline returns a pipeline of desc.
func (d *Device) NewPipeline(desc PipelineDesc) (*Pipeline, error) {
	if desc.Rasterizer == nil || desc.DSA == nil || desc.Blend == nil {
		return nil, fmt.Errorf("a pipeline of no rasterizer, depth-stencil-alpha or blend state")
	}
	// Of vertex input of the device's limits, which guests' drivers keep
	// to, as the caps give them, but guests need not.
	l := &d.props.Limits
	for _, b := range desc.Bindings {
		if b.Binding >= l.MaxVertexInputBindings || b.Stride > l.MaxVertexInputBindingStride {
			return nil, fmt.Errorf("a vertex binding %+v of %s", b, d.Name)
		}
	}
	for _, in := range desc.Inputs {
		if in.Location >= l.MaxVertexInputAttributes || in.Offset > l.MaxVertexInputAttributeOffset {
			return nil, fmt.Errorf("a vertex input %+v of %s", in, d.Name)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	p, err := d.newGraphicsPipeline(desc.Resources)
	if err != nil {
		return nil, err
	}

	vs, err := d.shaderModule(desc.VS)
	if err != nil {
		d.destroyPipeline(p)
		return nil, err
	}
	defer d.destroyShaderModule(vs)
	fs, err := d.shaderModule(desc.FS)
	if err != nil {
		d.destroyPipeline(p)
		return nil, err
	}
	defer d.destroyShaderModule(fs)
	main := cstrings(a, []string{"main"})
	stages := array[vn.VkPipelineShaderStageCreateInfo](a, 2)
	for i, s := range []struct {
		stage  uint32
		module vn.Handle
	}{{shaderStageVertex, vs}, {shaderStageFragment, fs}} {
		stages[i].SType = vn.VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO
		stages[i].Stage = s.stage
		stages[i].Module = s.module
		stages[i].PName = *(*unsafe.Pointer)(main)
	}

	vi := get[vn.VkPipelineVertexInputStateCreateInfo](a)
	vi.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO
	if n := len(desc.Bindings); n != 0 {
		bindings := array[vn.VkVertexInputBindingDescription](a, n)
		var divisors []vn.VkVertexInputBindingDivisorDescription
		for i, b := range desc.Bindings {
			bindings[i] = vn.VkVertexInputBindingDescription{Binding: b.Binding, Stride: b.Stride}
			if b.Instance {
				bindings[i].InputRate = 1 // VK_VERTEX_INPUT_RATE_INSTANCE
				if b.Divisor > 1 {
					divisors = append(divisors, vn.VkVertexInputBindingDivisorDescription{Binding: b.Binding, Divisor: b.Divisor})
				}
			}
		}
		vi.VertexBindingDescriptionCount, vi.PVertexBindingDescriptions = uint32(n), unsafe.Pointer(&bindings[0])
		if len(divisors) != 0 {
			if !d.divisors {
				d.destroyPipeline(p)
				return nil, fmt.Errorf("instances' divisors of %+v, which %s has not", divisors, d.Name)
			}
			ds := array[vn.VkVertexInputBindingDivisorDescription](a, len(divisors))
			copy(ds, divisors)
			dsi := get[vn.VkPipelineVertexInputDivisorStateCreateInfo](a)
			dsi.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_DIVISOR_STATE_CREATE_INFO
			dsi.VertexBindingDivisorCount, dsi.PVertexBindingDivisors = uint32(len(ds)), unsafe.Pointer(&ds[0])
			vi.PNext = unsafe.Pointer(dsi)
		}
	}
	if n := len(desc.Inputs); n != 0 {
		attrs := array[vn.VkVertexInputAttributeDescription](a, n)
		for i, in := range desc.Inputs {
			attrs[i] = vn.VkVertexInputAttributeDescription{Location: in.Location, Binding: in.Binding, Format: in.Format, Offset: in.Offset}
		}
		vi.VertexAttributeDescriptionCount, vi.PVertexAttributeDescriptions = uint32(n), unsafe.Pointer(&attrs[0])
	}
	ia := get[vn.VkPipelineInputAssemblyStateCreateInfo](a)
	ia.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO
	ia.Topology = desc.Topology
	if desc.PrimitiveRestart {
		ia.PrimitiveRestartEnable = 1
	}
	vp := get[vn.VkPipelineViewportStateCreateInfo](a)
	vp.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO
	vp.ViewportCount, vp.ScissorCount = 1, 1

	r := desc.Rasterizer
	rs := get[vn.VkPipelineRasterizationStateCreateInfo](a)
	rs.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO
	// Vulkan has one polygon mode of both faces: that of the faces drawn.
	fill := r.FillFront
	if r.CullFace == 1 { // PIPE_FACE_FRONT
		fill = r.FillBack
	}
	if fill > 2 {
		fill = pipePolygonFill
	}
	if fill == pipePolygonPoint && !d.pointPolygons {
		// Of the device's that draw no polygons as points, the
		// nearest: their edges, through their vertices.
		fill = pipePolygonLine
	}
	rs.PolygonMode = fill
	rs.CullMode = r.CullFace & 3
	rs.FrontFace = 1 // VK_FRONT_FACE_CLOCKWISE
	if r.FrontCCW {
		rs.FrontFace = 0
	}
	if !r.DepthClip {
		rs.DepthClampEnable = 1
	}
	if r.RasterizerDiscard {
		rs.RasterizerDiscardEnable = 1
	}
	if r.OffsetTri || r.OffsetLine || r.OffsetPoint {
		rs.DepthBiasEnable = 1
		rs.DepthBiasConstantFactor, rs.DepthBiasSlopeFactor, rs.DepthBiasClamp = r.OffsetUnits, r.OffsetScale, r.OffsetClamp
	}
	rs.LineWidth = 1
	if r.LineWidth > 0 && d.features10.WideLines != 0 {
		rs.LineWidth = r.LineWidth
	}
	if !r.FlatShadeFirst && d.provokingLast {
		// Of flat varyings of primitives' last vertices, as OpenGL's are
		// by default, and Vulkan's not. A device of no
		// VK_EXT_provoking_vertex, as MoltenVK, gives the first's, as
		// Metal does.
		pv := get[vn.VkPipelineRasterizationProvokingVertexStateCreateInfoEXT](a)
		pv.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_PROVOKING_VERTEX_STATE_CREATE_INFO_EXT
		pv.ProvokingVertexMode = 1 // VK_PROVOKING_VERTEX_MODE_LAST_VERTEX_EXT
		rs.PNext = unsafe.Pointer(pv)
	}

	ms := get[vn.VkPipelineMultisampleStateCreateInfo](a)
	ms.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO
	ms.RasterizationSamples = max(desc.Samples, 1)
	mask := get[uint32](a)
	*mask = desc.SampleMask
	ms.PSampleMask = unsafe.Pointer(mask)
	if desc.MinSamples > 1 && desc.Samples > 1 {
		ms.SampleShadingEnable = 1
		ms.MinSampleShading = min(float32(desc.MinSamples)/float32(desc.Samples), 1)
	}
	if desc.Blend.AlphaToCoverage {
		ms.AlphaToCoverageEnable = 1
	}
	if desc.Blend.AlphaToOne && d.features10.AlphaToOne != 0 {
		ms.AlphaToOneEnable = 1
	}

	z := desc.DSA
	ds := get[vn.VkPipelineDepthStencilStateCreateInfo](a)
	ds.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_DEPTH_STENCIL_STATE_CREATE_INFO
	if z.DepthEnable && desc.HasDepth {
		ds.DepthTestEnable = 1
		ds.DepthCompareOp = z.DepthFunc & 7
		if z.DepthWrite {
			ds.DepthWriteEnable = 1
		}
	}
	if z.Stencil[0].Enable && desc.HasStencil {
		ds.StencilTestEnable = 1
		back := z.Stencil[1]
		if !back.Enable {
			back = z.Stencil[0]
		}
		ds.Front, ds.Back = vkStencilOpState(z.Stencil[0]), vkStencilOpState(back)
	}

	b := desc.Blend
	cb := get[vn.VkPipelineColorBlendStateCreateInfo](a)
	cb.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO
	if b.LogicOpEnable && d.features10.LogicOp != 0 {
		cb.LogicOpEnable, cb.LogicOp = 1, VkLogicOp(b.LogicOp)
	}
	if n := len(desc.ColorFormats); n != 0 {
		atts := array[vn.VkPipelineColorBlendAttachmentState](a, n)
		for i := range atts {
			rt := b.RT[0]
			if b.IndependentBlend {
				rt = b.RT[i]
			}
			atts[i].ColorWriteMask = rt.ColorMask & 0xf
			if rt.Enable && !b.LogicOpEnable {
				atts[i].BlendEnable = 1
				atts[i].SrcColorBlendFactor, atts[i].DstColorBlendFactor = VkBlendFactor(rt.RGBSrc), VkBlendFactor(rt.RGBDst)
				atts[i].SrcAlphaBlendFactor, atts[i].DstAlphaBlendFactor = VkBlendFactor(rt.AlphaSrc), VkBlendFactor(rt.AlphaDst)
				atts[i].ColorBlendOp, atts[i].AlphaBlendOp = rt.RGBFunc, rt.AlphaFunc
			}
		}
		cb.AttachmentCount, cb.PAttachments = uint32(n), unsafe.Pointer(&atts[0])
	}

	dyn := array[uint32](a, 4)
	// VK_DYNAMIC_STATE_VIEWPORT, SCISSOR, BLEND_CONSTANTS, STENCIL_REFERENCE.
	dyn[0], dyn[1], dyn[2], dyn[3] = 0, 1, 4, 8
	dy := get[vn.VkPipelineDynamicStateCreateInfo](a)
	dy.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_DYNAMIC_STATE_CREATE_INFO
	dy.DynamicStateCount, dy.PDynamicStates = uint32(len(dyn)), unsafe.Pointer(&dyn[0])

	ri := get[vn.VkPipelineRenderingCreateInfo](a)
	ri.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_RENDERING_CREATE_INFO
	if n := len(desc.ColorFormats); n != 0 {
		formats := array[uint32](a, n)
		copy(formats, desc.ColorFormats)
		ri.ColorAttachmentCount, ri.PColorAttachmentFormats = uint32(n), unsafe.Pointer(&formats[0])
	}
	if desc.HasDepth {
		ri.DepthAttachmentFormat = desc.DepthFormat
	}
	if desc.HasStencil {
		ri.StencilAttachmentFormat = desc.DepthFormat
	}

	info := get[vn.VkGraphicsPipelineCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_GRAPHICS_PIPELINE_CREATE_INFO
	info.PNext = unsafe.Pointer(ri)
	info.StageCount, info.PStages = 2, unsafe.Pointer(&stages[0])
	info.PVertexInputState = unsafe.Pointer(vi)
	info.PInputAssemblyState = unsafe.Pointer(ia)
	info.PViewportState = unsafe.Pointer(vp)
	info.PRasterizationState = unsafe.Pointer(rs)
	info.PMultisampleState = unsafe.Pointer(ms)
	info.PDepthStencilState = unsafe.Pointer(ds)
	info.PColorBlendState = unsafe.Pointer(cb)
	info.PDynamicState = unsafe.Pointer(dy)
	info.Layout = p.layout
	info.BasePipelineIndex = -1
	pl := get[vn.Handle](a)
	cg := get[vn.Args_vkCreateGraphicsPipelines](a)
	cg.Device, cg.CreateInfoCount, cg.PCreateInfos, cg.PPipelines = d.dev, 1, unsafe.Pointer(info), unsafe.Pointer(pl)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateGraphicsPipelines_EXT, d.table, unsafe.Pointer(cg))
	if err := check("vkCreateGraphicsPipelines", cg.Ret); err != nil {
		d.destroyPipeline(p)
		return nil, err
	}
	p.pipeline = *pl
	return p, nil
}

// vkStencilOpState returns a face's stencil test, of Gallium's
// enumerations, as Vulkan's; its reference is dynamic.
func vkStencilOpState(s StencilFace) vn.VkStencilOpState {
	return vn.VkStencilOpState{
		FailOp:      VkStencilOp(s.FailOp),
		PassOp:      VkStencilOp(s.ZPassOp),
		DepthFailOp: VkStencilOp(s.ZFailOp),
		CompareOp:   s.Func & 7,
		CompareMask: s.ValueMask,
		WriteMask:   s.WriteMask,
	}
}

// BindVertexBuffers binds bufs, of offsets, from binding first.
func (c *Commands) BindVertexBuffers(first uint32, bufs []*Buffer, offsets []uint64) {
	if len(bufs) == 0 {
		return
	}
	hs := array[vn.Handle](c.a, len(bufs))
	offs := array[uint64](c.a, len(bufs))
	for i, b := range bufs {
		hs[i], offs[i] = b.buf, offsets[i]
	}
	bv := get[vn.Args_vkCmdBindVertexBuffers](c.a)
	bv.FirstBinding, bv.BindingCount, bv.PBuffers, bv.POffsets = first, uint32(len(bufs)), unsafe.Pointer(&hs[0]), unsafe.Pointer(&offs[0])
	c.call(vn.VK_COMMAND_TYPE_vkCmdBindVertexBuffers_EXT, unsafe.Pointer(bv))
}

// StorageBufferAlignment returns the alignment of the offsets of storage
// buffers' bindings.
func (d *Device) StorageBufferAlignment() uint32 {
	return uint32(d.props.Limits.MinStorageBufferOffsetAlignment)
}

// MaxStorageBufferRange returns the most bytes of a storage buffer's
// binding.
func (d *Device) MaxStorageBufferRange() uint32 {
	return d.props.Limits.MaxStorageBufferRange
}

// IndexSizeOK reports whether the device draws indexes of size bytes.
func (d *Device) IndexSizeOK(size uint32) bool {
	return size == 2 || size == 4 || size == 1 && d.indexUint8
}

// BindIndexBuffer binds b, of indexes of size bytes, of which the device
// draws, from offset.
func (c *Commands) BindIndexBuffer(b *Buffer, offset uint64, size uint32) {
	bi := get[vn.Args_vkCmdBindIndexBuffer](c.a)
	bi.Buffer, bi.Offset = b.buf, offset
	switch size {
	case 1:
		bi.IndexType = 1000265000 // VK_INDEX_TYPE_UINT8_KHR
	case 4:
		bi.IndexType = 1 // VK_INDEX_TYPE_UINT32
	}
	c.call(vn.VK_COMMAND_TYPE_vkCmdBindIndexBuffer_EXT, unsafe.Pointer(bi))
}

// DrawVertices draws vertices from first, of instances from firstInstance.
func (c *Commands) DrawVertices(vertices, instances, first, firstInstance uint32) {
	dr := get[vn.Args_vkCmdDraw](c.a)
	dr.VertexCount, dr.InstanceCount, dr.FirstVertex, dr.FirstInstance = vertices, instances, first, firstInstance
	c.call(vn.VK_COMMAND_TYPE_vkCmdDraw_EXT, unsafe.Pointer(dr))
}

// DrawIndexed draws the vertices of indexes from first, plus bias, of
// instances from firstInstance.
func (c *Commands) DrawIndexed(indexes, instances, first uint32, bias int32, firstInstance uint32) {
	dr := get[vn.Args_vkCmdDrawIndexed](c.a)
	dr.IndexCount, dr.InstanceCount, dr.FirstIndex, dr.VertexOffset, dr.FirstInstance = indexes, instances, first, bias, firstInstance
	c.call(vn.VK_COMMAND_TYPE_vkCmdDrawIndexed_EXT, unsafe.Pointer(dr))
}

// DrawIndirect draws count draws of the commands of b from offset, of
// stride bytes each: VkDrawIndirectCommands, or VkDrawIndexedIndirectCommands
// of the bound indexes if indexed. Gallium's commands of indirect draws are
// Vulkan's.
func (c *Commands) DrawIndirect(b *Buffer, offset uint64, count, stride uint32, indexed bool) {
	if indexed {
		dr := get[vn.Args_vkCmdDrawIndexedIndirect](c.a)
		dr.Buffer, dr.Offset, dr.DrawCount, dr.Stride = b.buf, offset, count, stride
		c.call(vn.VK_COMMAND_TYPE_vkCmdDrawIndexedIndirect_EXT, unsafe.Pointer(dr))
		return
	}
	dr := get[vn.Args_vkCmdDrawIndirect](c.a)
	dr.Buffer, dr.Offset, dr.DrawCount, dr.Stride = b.buf, offset, count, stride
	c.call(vn.VK_COMMAND_TYPE_vkCmdDrawIndirect_EXT, unsafe.Pointer(dr))
}

// CheckIndirect returns an error unless count commands of size bytes, of
// stride bytes each, from offset, are of b, as Vulkan reads them: from an
// offset of a multiple of 4, of a stride of one, at least size, if more
// than one, of a device of multiple draws.
func (d *Device) CheckIndirect(b *Buffer, offset uint64, count, stride, size uint32) error {
	bad := func() error {
		return fmt.Errorf("%d indirect commands of %d bytes, of a stride of %d, at %d of a buffer of %d bytes",
			count, size, stride, offset, b.size)
	}
	if offset%4 != 0 || count == 0 {
		return bad()
	}
	span := uint64(size)
	if count > 1 {
		if d.features10.MultiDrawIndirect == 0 || stride%4 != 0 || stride < size {
			return bad()
		}
		span += uint64(count-1) * uint64(stride)
	}
	if offset > b.size || span > b.size-offset {
		return bad()
	}
	return nil
}

// SetViewport sets the viewport, of Vulkan's parameters.
func (c *Commands) SetViewport(x, y, width, height, minDepth, maxDepth float32) {
	vp := get[vn.VkViewport](c.a)
	*vp = vn.VkViewport{X: x, Y: y, Width: width, Height: height, MinDepth: minDepth, MaxDepth: maxDepth}
	sv := get[vn.Args_vkCmdSetViewport](c.a)
	sv.ViewportCount, sv.PViewports = 1, unsafe.Pointer(vp)
	c.call(vn.VK_COMMAND_TYPE_vkCmdSetViewport_EXT, unsafe.Pointer(sv))
}

// SetScissor sets the scissor, of its rectangle.
func (c *Commands) SetScissor(x, y int32, width, height uint32) {
	sc := get[vn.VkRect2D](c.a)
	*sc = vn.VkRect2D{Offset: vn.VkOffset2D{X: x, Y: y}, Extent: vn.VkExtent2D{Width: width, Height: height}}
	ss := get[vn.Args_vkCmdSetScissor](c.a)
	ss.ScissorCount, ss.PScissors = 1, unsafe.Pointer(sc)
	c.call(vn.VK_COMMAND_TYPE_vkCmdSetScissor_EXT, unsafe.Pointer(ss))
}

// SetBlendConstants sets the blend constants.
func (c *Commands) SetBlendConstants(rgba [4]float32) {
	bc := get[vn.Args_vkCmdSetBlendConstants](c.a)
	bc.BlendConstants = rgba
	c.call(vn.VK_COMMAND_TYPE_vkCmdSetBlendConstants_EXT, unsafe.Pointer(bc))
}

// SetStencilReference sets the stencil reference of front and back faces:
// of a device of the portability subset that has one of both, the front's.
func (c *Commands) SetStencilReference(front, back uint32) {
	if !c.d.separateStencilRef {
		back = front
	}
	for _, f := range []struct{ mask, ref uint32 }{{1, front}, {2, back}} {
		sr := get[vn.Args_vkCmdSetStencilReference](c.a)
		sr.FaceMask, sr.Reference = f.mask, f.ref
		c.call(vn.VK_COMMAND_TYPE_vkCmdSetStencilReference_EXT, unsafe.Pointer(sr))
	}
}

// WorkgroupFences reports whether shaders' fences of the device's scope
// are to be of the workgroup's, as tospirv's Options.WorkgroupFences.
func (d *Device) WorkgroupFences() bool {
	return d.workgroupFences
}
