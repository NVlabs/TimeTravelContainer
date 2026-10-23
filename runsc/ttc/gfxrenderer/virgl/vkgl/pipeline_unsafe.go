// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// Shader stages, VkShaderStageFlagBits, and pipelines' bind points,
// VkPipelineBindPoint.
const (
	shaderStageVertex      = 0x1
	shaderStageFragment    = 0x10
	shaderStageAllGraphics = 0x1f
	shaderStageCompute     = 0x20
	bindPointGraphics      = 0
	bindPointCompute       = 1
)

// Pipeline is a graphics or a compute pipeline, and its layout, of the
// layouts of the sets of its shaders' resources.
type Pipeline struct {
	pipeline   vn.Handle
	layout     vn.Handle
	setLayouts [numSets]vn.Handle
	// resources are the shaders' resources, of which bindings are.
	resources []StageResource
	// bindPoint is the pipeline's, and stages are those of its push
	// constants.
	bindPoint, stages uint32
}

// VertexAttribute is a vertex shader's input, of binding 0's vertices.
type VertexAttribute struct {
	Location uint32
	Format   uint32
	Offset   uint32
}

// GraphicsDesc describes a graphics pipeline of dynamic rendering to a
// color buffer, of a vertex buffer of vertices of Stride bytes, of
// triangles.
type GraphicsDesc struct {
	VS, FS      []uint32
	Stride      uint32
	Attributes  []VertexAttribute
	ColorFormat uint32
	// Resources are the shaders' resources.
	Resources []StageResource
}

// shaderModule returns a new shader module of SPIR-V's words.
func (d *Device) shaderModule(words []uint32) (vn.Handle, error) {
	a := d.a
	code := array[uint32](a, len(words))
	copy(code, words)
	info := get[vn.VkShaderModuleCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO
	info.CodeSize = uint64(4 * len(words))
	info.PCode = unsafe.Pointer(&code[0])
	h := get[vn.Handle](a)
	cs := get[vn.Args_vkCreateShaderModule](a)
	cs.Device, cs.PCreateInfo, cs.PShaderModule = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateShaderModule_EXT, d.table, unsafe.Pointer(cs))
	return *h, check("vkCreateShaderModule", cs.Ret)
}

func (d *Device) destroyShaderModule(h vn.Handle) {
	ds := get[vn.Args_vkDestroyShaderModule](d.a)
	ds.Device, ds.ShaderModule = d.dev, h
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyShaderModule_EXT, d.table, unsafe.Pointer(ds))
}

// newLayout makes p's layout, of its resources and its push constants, of
// a caller that holds d.mu.
func (d *Device) newLayout(p *Pipeline) error {
	a := d.a
	var err error
	if p.setLayouts, err = d.setLayouts(p.resources); err != nil {
		return err
	}
	sl := array[vn.Handle](a, numSets)
	copy(sl, p.setLayouts[:])
	pcr := get[vn.VkPushConstantRange](a)
	*pcr = vn.VkPushConstantRange{StageFlags: p.stages, Size: tospirv.PushConstantsSize}
	li := get[vn.VkPipelineLayoutCreateInfo](a)
	li.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO
	li.SetLayoutCount, li.PSetLayouts = numSets, unsafe.Pointer(&sl[0])
	li.PushConstantRangeCount, li.PPushConstantRanges = 1, unsafe.Pointer(pcr)
	layout := get[vn.Handle](a)
	cl := get[vn.Args_vkCreatePipelineLayout](a)
	cl.Device, cl.PCreateInfo, cl.PPipelineLayout = d.dev, unsafe.Pointer(li), unsafe.Pointer(layout)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreatePipelineLayout_EXT, d.table, unsafe.Pointer(cl))
	if err := check("vkCreatePipelineLayout", cl.Ret); err != nil {
		d.destroyPipeline(p)
		return err
	}
	p.layout = *layout
	return nil
}

// newGraphicsPipeline returns a graphics pipeline of resources, whose
// layout it makes, of a caller that holds d.mu.
func (d *Device) newGraphicsPipeline(resources []StageResource) (*Pipeline, error) {
	p := &Pipeline{resources: append([]StageResource(nil), resources...), bindPoint: bindPointGraphics,
		stages: shaderStageAllGraphics}
	if err := d.newLayout(p); err != nil {
		return nil, err
	}
	return p, nil
}

// NewGraphicsPipeline returns a pipeline of desc.
func (d *Device) NewGraphicsPipeline(desc GraphicsDesc) (*Pipeline, error) {
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

	binding := get[vn.VkVertexInputBindingDescription](a)
	binding.Stride = desc.Stride
	attrs := array[vn.VkVertexInputAttributeDescription](a, len(desc.Attributes))
	for i, at := range desc.Attributes {
		attrs[i] = vn.VkVertexInputAttributeDescription{Location: at.Location, Format: at.Format, Offset: at.Offset}
	}
	vi := get[vn.VkPipelineVertexInputStateCreateInfo](a)
	vi.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO
	vi.VertexBindingDescriptionCount, vi.PVertexBindingDescriptions = 1, unsafe.Pointer(binding)
	if len(attrs) != 0 {
		vi.VertexAttributeDescriptionCount, vi.PVertexAttributeDescriptions = uint32(len(attrs)), unsafe.Pointer(&attrs[0])
	}
	ia := get[vn.VkPipelineInputAssemblyStateCreateInfo](a)
	ia.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO
	ia.Topology = 3 // VK_PRIMITIVE_TOPOLOGY_TRIANGLE_LIST
	vp := get[vn.VkPipelineViewportStateCreateInfo](a)
	vp.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO
	vp.ViewportCount, vp.ScissorCount = 1, 1
	rs := get[vn.VkPipelineRasterizationStateCreateInfo](a)
	rs.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO
	rs.LineWidth = 1
	ms := get[vn.VkPipelineMultisampleStateCreateInfo](a)
	ms.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO
	ms.RasterizationSamples = 1
	ds := get[vn.VkPipelineDepthStencilStateCreateInfo](a)
	ds.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_DEPTH_STENCIL_STATE_CREATE_INFO
	att := get[vn.VkPipelineColorBlendAttachmentState](a)
	att.ColorWriteMask = 0xf
	cb := get[vn.VkPipelineColorBlendStateCreateInfo](a)
	cb.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO
	cb.AttachmentCount, cb.PAttachments = 1, unsafe.Pointer(att)
	dyn := array[uint32](a, 2)
	dyn[0], dyn[1] = 0, 1 // VK_DYNAMIC_STATE_VIEWPORT, VK_DYNAMIC_STATE_SCISSOR
	dy := get[vn.VkPipelineDynamicStateCreateInfo](a)
	dy.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_DYNAMIC_STATE_CREATE_INFO
	dy.DynamicStateCount, dy.PDynamicStates = 2, unsafe.Pointer(&dyn[0])
	format := get[uint32](a)
	*format = desc.ColorFormat
	ri := get[vn.VkPipelineRenderingCreateInfo](a)
	ri.SType = vn.VK_STRUCTURE_TYPE_PIPELINE_RENDERING_CREATE_INFO
	ri.ColorAttachmentCount, ri.PColorAttachmentFormats = 1, unsafe.Pointer(format)

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

// ComputeDesc describes a compute pipeline: its shader's SPIR-V, and its
// resources.
type ComputeDesc struct {
	CS        []uint32
	Resources []StageResource
}

// NewComputePipeline returns a pipeline of desc.
func (d *Device) NewComputePipeline(desc ComputeDesc) (*Pipeline, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	p := &Pipeline{resources: append([]StageResource(nil), desc.Resources...), bindPoint: bindPointCompute,
		stages: shaderStageCompute}
	if err := d.newLayout(p); err != nil {
		return nil, err
	}
	cs, err := d.shaderModule(desc.CS)
	if err != nil {
		d.destroyPipeline(p)
		return nil, err
	}
	defer d.destroyShaderModule(cs)
	main := cstrings(a, []string{"main"})
	info := get[vn.VkComputePipelineCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO
	info.Stage = vn.VkPipelineShaderStageCreateInfo{SType: vn.VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
		Stage: shaderStageCompute, Module: cs, PName: *(*unsafe.Pointer)(main)}
	info.Layout = p.layout
	info.BasePipelineIndex = -1
	pl := get[vn.Handle](a)
	cc := get[vn.Args_vkCreateComputePipelines](a)
	cc.Device, cc.CreateInfoCount, cc.PCreateInfos, cc.PPipelines = d.dev, 1, unsafe.Pointer(info), unsafe.Pointer(pl)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateComputePipelines_EXT, d.table, unsafe.Pointer(cc))
	if err := check("vkCreateComputePipelines", cc.Ret); err != nil {
		d.destroyPipeline(p)
		return nil, err
	}
	p.pipeline = *pl
	return p, nil
}

// DestroyPipeline destroys p, which the GPU no longer uses.
func (d *Device) DestroyPipeline(p *Pipeline) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	d.destroyPipeline(p)
}

func (d *Device) destroyPipeline(p *Pipeline) {
	a := d.a
	if p.pipeline != 0 {
		dp := get[vn.Args_vkDestroyPipeline](a)
		dp.Device, dp.Pipeline = d.dev, p.pipeline
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyPipeline_EXT, d.table, unsafe.Pointer(dp))
		p.pipeline = 0
	}
	if p.layout != 0 {
		dl := get[vn.Args_vkDestroyPipelineLayout](a)
		dl.Device, dl.PipelineLayout = d.dev, p.layout
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyPipelineLayout_EXT, d.table, unsafe.Pointer(dl))
		p.layout = 0
	}
	d.destroySetLayouts(p.setLayouts)
	p.setLayouts = [numSets]vn.Handle{}
}
