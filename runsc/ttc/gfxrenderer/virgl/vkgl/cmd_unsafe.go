// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Pipeline stages and accesses, of VkPipelineStageFlagBits and
// VkAccessFlagBits.
const (
	StageTopOfPipe             = 0x1
	StageVertexInput           = 0x4
	StageVertexShader          = 0x8
	StageFragmentShader        = 0x80
	StageColorAttachmentOutput = 0x400
	StageComputeShader         = 0x800
	StageTransfer              = 0x1000
	StageBottomOfPipe          = 0x2000
	StageHost                  = 0x4000
	StageAllCommands           = 0x10000

	AccessShaderRead           = 0x20
	AccessColorAttachmentRead  = 0x80
	AccessColorAttachmentWrite = 0x100
	AccessTransferRead         = 0x800
	AccessTransferWrite        = 0x1000
	AccessHostRead             = 0x2000
	AccessMemoryRead           = 0x8000
	AccessMemoryWrite          = 0x10000
)

// Commands records commands of a command buffer of the device's, in a call
// of Device.Record.
type Commands struct {
	d  *Device
	a  *arena
	cb vn.Handle
}

// Record records f's commands, submits them, and waits for the GPU to
// execute them.
func (d *Device) Record(f func(c *Commands) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	if d.cmdPool == 0 {
		info := get[vn.VkCommandPoolCreateInfo](a)
		info.SType = vn.VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO
		info.QueueFamilyIndex = d.family
		pool := get[vn.Handle](a)
		cp := get[vn.Args_vkCreateCommandPool](a)
		cp.Device, cp.PCreateInfo, cp.PCommandPool = d.dev, unsafe.Pointer(info), unsafe.Pointer(pool)
		vk.Call(vn.VK_COMMAND_TYPE_vkCreateCommandPool_EXT, d.table, unsafe.Pointer(cp))
		if err := check("vkCreateCommandPool", cp.Ret); err != nil {
			return err
		}
		d.cmdPool = *pool
	}
	ai := get[vn.VkCommandBufferAllocateInfo](a)
	ai.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO
	ai.CommandPool = d.cmdPool
	ai.CommandBufferCount = 1
	cbs := get[vn.Handle](a)
	ac := get[vn.Args_vkAllocateCommandBuffers](a)
	ac.Device, ac.PAllocateInfo, ac.PCommandBuffers = d.dev, unsafe.Pointer(ai), unsafe.Pointer(cbs)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateCommandBuffers_EXT, d.table, unsafe.Pointer(ac))
	if err := check("vkAllocateCommandBuffers", ac.Ret); err != nil {
		return err
	}
	defer func() {
		fc := get[vn.Args_vkFreeCommandBuffers](a)
		fc.Device, fc.CommandPool, fc.CommandBufferCount, fc.PCommandBuffers = d.dev, d.cmdPool, 1, unsafe.Pointer(cbs)
		vk.Call(vn.VK_COMMAND_TYPE_vkFreeCommandBuffers_EXT, d.table, unsafe.Pointer(fc))
	}()
	c := &Commands{d: d, a: a, cb: *cbs}
	bi := get[vn.VkCommandBufferBeginInfo](a)
	bi.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO
	bi.Flags = 0x1 // VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT
	bc := get[vn.Args_vkBeginCommandBuffer](a)
	bc.CommandBuffer, bc.PBeginInfo = c.cb, unsafe.Pointer(bi)
	vk.Call(vn.VK_COMMAND_TYPE_vkBeginCommandBuffer_EXT, d.table, unsafe.Pointer(bc))
	if err := check("vkBeginCommandBuffer", bc.Ret); err != nil {
		return err
	}
	if err := f(c); err != nil {
		return err
	}
	ec := get[vn.Args_vkEndCommandBuffer](a)
	ec.CommandBuffer = c.cb
	vk.Call(vn.VK_COMMAND_TYPE_vkEndCommandBuffer_EXT, d.table, unsafe.Pointer(ec))
	if err := check("vkEndCommandBuffer", ec.Ret); err != nil {
		return err
	}
	return d.submitAndWait(c.cb)
}

// submitAndWait submits a command buffer and waits for the GPU to execute
// it.
func (d *Device) submitAndWait(cb vn.Handle) error {
	a := d.a
	fi := get[vn.VkFenceCreateInfo](a)
	fi.SType = vn.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO
	fence := get[vn.Handle](a)
	cf := get[vn.Args_vkCreateFence](a)
	cf.Device, cf.PCreateInfo, cf.PFence = d.dev, unsafe.Pointer(fi), unsafe.Pointer(fence)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, d.table, unsafe.Pointer(cf))
	if err := check("vkCreateFence", cf.Ret); err != nil {
		return err
	}
	defer func() {
		df := get[vn.Args_vkDestroyFence](a)
		df.Device, df.Fence = d.dev, *fence
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyFence_EXT, d.table, unsafe.Pointer(df))
	}()
	cbs := get[vn.Handle](a)
	*cbs = cb
	si := get[vn.VkSubmitInfo](a)
	si.SType = vn.VK_STRUCTURE_TYPE_SUBMIT_INFO
	si.CommandBufferCount = 1
	si.PCommandBuffers = unsafe.Pointer(cbs)
	qs := get[vn.Args_vkQueueSubmit](a)
	qs.Queue, qs.SubmitCount, qs.PSubmits, qs.Fence = d.queue, 1, unsafe.Pointer(si), *fence
	d.qmu.Lock()
	vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, d.table, unsafe.Pointer(qs))
	d.qmu.Unlock()
	if err := check("vkQueueSubmit", qs.Ret); err != nil {
		return err
	}
	wf := get[vn.Args_vkWaitForFences](a)
	wf.Device, wf.FenceCount, wf.PFences, wf.WaitAll, wf.Timeout = d.dev, 1, unsafe.Pointer(fence), 1, ^uint64(0)
	vk.Call(vn.VK_COMMAND_TYPE_vkWaitForFences_EXT, d.table, unsafe.Pointer(wf))
	if d.descPool != 0 {
		// The command buffer's descriptor sets are no longer used.
		rp := get[vn.Args_vkResetDescriptorPool](a)
		rp.Device, rp.DescriptorPool = d.dev, d.descPool
		vk.Call(vn.VK_COMMAND_TYPE_vkResetDescriptorPool_EXT, d.table, unsafe.Pointer(rp))
	}
	return check("vkWaitForFences", wf.Ret)
}

// call calls a command of the command buffer, of args, whose first field
// is the command buffer.
func (c *Commands) call(typ int32, args unsafe.Pointer) {
	*(*vn.Handle)(args) = c.cb
	vk.Call(typ, c.d.table, args)
}

// ImageLayout transitions img to layout, after the commands before of
// stages and accesses src, before those after of dst.
func (c *Commands) ImageLayout(img *Image, layout uint32, srcStages, srcAccess, dstStages, dstAccess uint32) {
	b := get[vn.VkImageMemoryBarrier](c.a)
	b.SType = vn.VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER
	b.SrcAccessMask, b.DstAccessMask = srcAccess, dstAccess
	b.OldLayout, b.NewLayout = img.layout, layout
	b.SrcQueueFamilyIndex, b.DstQueueFamilyIndex = ^uint32(0), ^uint32(0)
	b.Image = img.img
	b.SubresourceRange = vn.VkImageSubresourceRange{AspectMask: aspectColor, LevelCount: 1, LayerCount: 1}
	pb := get[vn.Args_vkCmdPipelineBarrier](c.a)
	pb.SrcStageMask, pb.DstStageMask = srcStages, dstStages
	pb.ImageMemoryBarrierCount, pb.PImageMemoryBarriers = 1, unsafe.Pointer(b)
	c.call(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, unsafe.Pointer(pb))
	img.layout = layout
}

// BeginRendering begins rendering to img, a color attachment, cleared to
// clear.
func (c *Commands) BeginRendering(img *Image, clear [4]float32) {
	att := get[vn.VkRenderingAttachmentInfo](c.a)
	att.SType = vn.VK_STRUCTURE_TYPE_RENDERING_ATTACHMENT_INFO
	att.ImageView = img.view
	att.ImageLayout = img.layout
	att.LoadOp = 1  // VK_ATTACHMENT_LOAD_OP_CLEAR
	att.StoreOp = 0 // VK_ATTACHMENT_STORE_OP_STORE
	*(*[4]float32)(unsafe.Pointer(&att.ClearValue)) = clear
	info := get[vn.VkRenderingInfo](c.a)
	info.SType = vn.VK_STRUCTURE_TYPE_RENDERING_INFO
	info.RenderArea = vn.VkRect2D{Extent: vn.VkExtent2D{Width: img.width, Height: img.height}}
	info.LayerCount = 1
	info.ColorAttachmentCount = 1
	info.PColorAttachments = unsafe.Pointer(att)
	br := get[vn.Args_vkCmdBeginRendering](c.a)
	br.PRenderingInfo = unsafe.Pointer(info)
	c.call(vn.VK_COMMAND_TYPE_vkCmdBeginRendering_EXT, unsafe.Pointer(br))
}

// EndRendering ends rendering.
func (c *Commands) EndRendering() {
	er := get[vn.Args_vkCmdEndRendering](c.a)
	c.call(vn.VK_COMMAND_TYPE_vkCmdEndRendering_EXT, unsafe.Pointer(er))
}

// Viewport sets the viewport and the scissor, of the area of width and
// height from x and y, of a viewport of negative height if height is.
func (c *Commands) Viewport(x, y, width, height float32, w, h uint32) {
	vp := get[vn.VkViewport](c.a)
	*vp = vn.VkViewport{X: x, Y: y, Width: width, Height: height, MinDepth: 0, MaxDepth: 1}
	sv := get[vn.Args_vkCmdSetViewport](c.a)
	sv.ViewportCount, sv.PViewports = 1, unsafe.Pointer(vp)
	c.call(vn.VK_COMMAND_TYPE_vkCmdSetViewport_EXT, unsafe.Pointer(sv))
	sc := get[vn.VkRect2D](c.a)
	sc.Extent = vn.VkExtent2D{Width: w, Height: h}
	ss := get[vn.Args_vkCmdSetScissor](c.a)
	ss.ScissorCount, ss.PScissors = 1, unsafe.Pointer(sc)
	c.call(vn.VK_COMMAND_TYPE_vkCmdSetScissor_EXT, unsafe.Pointer(ss))
}

// BindPipeline binds a graphics pipeline.
func (c *Commands) BindPipeline(p *Pipeline) {
	bp := get[vn.Args_vkCmdBindPipeline](c.a)
	bp.Pipeline = p.pipeline
	c.call(vn.VK_COMMAND_TYPE_vkCmdBindPipeline_EXT, unsafe.Pointer(bp))
}

// BindVertexBuffer binds b at binding 0, of offset.
func (c *Commands) BindVertexBuffer(b *Buffer, offset uint64) {
	bufs := get[vn.Handle](c.a)
	*bufs = b.buf
	offs := get[uint64](c.a)
	*offs = offset
	bv := get[vn.Args_vkCmdBindVertexBuffers](c.a)
	bv.BindingCount, bv.PBuffers, bv.POffsets = 1, unsafe.Pointer(bufs), unsafe.Pointer(offs)
	c.call(vn.VK_COMMAND_TYPE_vkCmdBindVertexBuffers_EXT, unsafe.Pointer(bv))
}

// PushConstants sets a pipeline's push constants at offset to data.
func (c *Commands) PushConstants(p *Pipeline, offset uint32, data []byte) {
	values := array[byte](c.a, len(data))
	copy(values, data)
	pc := get[vn.Args_vkCmdPushConstants](c.a)
	pc.Layout, pc.StageFlags, pc.Offset, pc.Size, pc.PValues = p.layout, shaderStageAllGraphics, offset, uint32(len(data)), unsafe.Pointer(&values[0])
	c.call(vn.VK_COMMAND_TYPE_vkCmdPushConstants_EXT, unsafe.Pointer(pc))
}

// Draw draws vertices of instances.
func (c *Commands) Draw(vertices, instances uint32) {
	dr := get[vn.Args_vkCmdDraw](c.a)
	dr.VertexCount, dr.InstanceCount = vertices, instances
	c.call(vn.VK_COMMAND_TYPE_vkCmdDraw_EXT, unsafe.Pointer(dr))
}

// CopyImageToBuffer copies img, in the transfer source layout, to b, as
// rows of its texels, tightly.
func (c *Commands) CopyImageToBuffer(img *Image, b *Buffer) {
	r := get[vn.VkBufferImageCopy](c.a)
	r.ImageSubresource = vn.VkImageSubresourceLayers{AspectMask: aspectColor, LayerCount: 1}
	r.ImageExtent = vn.VkExtent3D{Width: img.width, Height: img.height, Depth: 1}
	ci := get[vn.Args_vkCmdCopyImageToBuffer](c.a)
	ci.SrcImage, ci.SrcImageLayout, ci.DstBuffer = img.img, img.layout, b.buf
	ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(r)
	c.call(vn.VK_COMMAND_TYPE_vkCmdCopyImageToBuffer_EXT, unsafe.Pointer(ci))
}
