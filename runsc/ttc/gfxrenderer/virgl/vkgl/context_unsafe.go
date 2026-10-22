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

// Context is a Gallium context of the device's, a virgl sub-context's: its
// state, and the commands of its batch, a command buffer that it records
// until a flush submits it. One goroutine at a time uses a context, as the
// decoder serializes its commands.
//
// The context keeps images in the general layout, which every use of
// theirs takes, and orders its commands with barriers of all memory.
type Context struct {
	d *Device
	// a is the context's arena, of the C structs of its calls.
	a    *arena
	pool vn.Handle
	// batch is the batch that the context records, or nil, and inflight
	// the batches that it submitted that the GPU may not have finished.
	batch    *batch
	inflight []*batch
}

// batch is a command buffer of the context's, and what its commands use,
// which the GPU is done with once its fence signals.
type batch struct {
	cb    vn.Handle
	fence vn.Handle
	// staging are buffers of transfers, which the batch frees.
	staging []*Buffer
	// descPool is the pool of the batch's descriptor sets, or 0.
	descPool vn.Handle
	// rendering is whether the batch's commands are of a rendering
	// instance, which commands that render not end.
	rendering bool
}

// NewContext returns a new context.
func (d *Device) NewContext() (*Context, error) {
	c := &Context{d: d, a: newArena()}
	a := c.a
	defer a.reset()
	info := get[vn.VkCommandPoolCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO
	info.Flags = 0x2 // VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT
	info.QueueFamilyIndex = d.family
	pool := get[vn.Handle](a)
	cp := get[vn.Args_vkCreateCommandPool](a)
	cp.Device, cp.PCreateInfo, cp.PCommandPool = d.dev, unsafe.Pointer(info), unsafe.Pointer(pool)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateCommandPool_EXT, d.table, unsafe.Pointer(cp))
	if err := check("vkCreateCommandPool", cp.Ret); err != nil {
		return nil, err
	}
	c.pool = *pool
	return c, nil
}

// Destroy waits for the context's batches and destroys the context.
func (c *Context) Destroy() {
	if c.batch != nil {
		c.Flush().Wait()
	}
	for len(c.inflight) != 0 {
		c.waitOldest()
	}
	a := c.a
	defer a.reset()
	dp := get[vn.Args_vkDestroyCommandPool](a)
	dp.Device, dp.CommandPool = c.d.dev, c.pool
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyCommandPool_EXT, c.d.table, unsafe.Pointer(dp))
}

// commands returns the commands of the context's batch, beginning one if
// it has none.
func (c *Context) commands() (*Commands, error) {
	if c.batch == nil {
		if err := c.begin(); err != nil {
			return nil, err
		}
	}
	return &Commands{d: c.d, a: c.a, cb: c.batch.cb}, nil
}

func (c *Context) begin() error {
	d, a := c.d, c.a
	c.retire()
	ai := get[vn.VkCommandBufferAllocateInfo](a)
	ai.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO
	ai.CommandPool = c.pool
	ai.CommandBufferCount = 1
	cb := get[vn.Handle](a)
	ac := get[vn.Args_vkAllocateCommandBuffers](a)
	ac.Device, ac.PAllocateInfo, ac.PCommandBuffers = d.dev, unsafe.Pointer(ai), unsafe.Pointer(cb)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateCommandBuffers_EXT, d.table, unsafe.Pointer(ac))
	if err := check("vkAllocateCommandBuffers", ac.Ret); err != nil {
		return err
	}
	b := &batch{cb: *cb}
	bi := get[vn.VkCommandBufferBeginInfo](a)
	bi.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO
	bi.Flags = 0x1 // VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT
	bc := get[vn.Args_vkBeginCommandBuffer](a)
	bc.CommandBuffer, bc.PBeginInfo = b.cb, unsafe.Pointer(bi)
	vk.Call(vn.VK_COMMAND_TYPE_vkBeginCommandBuffer_EXT, d.table, unsafe.Pointer(bc))
	if err := check("vkBeginCommandBuffer", bc.Ret); err != nil {
		c.freeBatch(b)
		return err
	}
	c.batch = b
	return nil
}

// Fence is a batch that a flush submitted, which Wait waits for.
type Fence struct {
	c *Context
	b *batch
}

// Flush submits the context's batch, and returns its fence, or nil if the
// context has no commands.
func (c *Context) Flush() *Fence {
	b := c.batch
	if b == nil {
		return nil
	}
	c.batch = nil
	d, a := c.d, c.a
	defer a.reset()
	cmds := &Commands{d: d, a: a, cb: b.cb}
	if b.rendering {
		cmds.EndRendering()
		b.rendering = false
	}
	ec := get[vn.Args_vkEndCommandBuffer](a)
	ec.CommandBuffer = b.cb
	vk.Call(vn.VK_COMMAND_TYPE_vkEndCommandBuffer_EXT, d.table, unsafe.Pointer(ec))
	fi := get[vn.VkFenceCreateInfo](a)
	fi.SType = vn.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO
	fence := get[vn.Handle](a)
	cf := get[vn.Args_vkCreateFence](a)
	cf.Device, cf.PCreateInfo, cf.PFence = d.dev, unsafe.Pointer(fi), unsafe.Pointer(fence)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, d.table, unsafe.Pointer(cf))
	if ec.Ret != vn.VK_SUCCESS || cf.Ret != vn.VK_SUCCESS {
		// The device is lost, or out of memory: the batch's commands are
		// dropped, as a lost device's are.
		c.freeBatch(b)
		return nil
	}
	b.fence = *fence
	cbs := get[vn.Handle](a)
	*cbs = b.cb
	si := get[vn.VkSubmitInfo](a)
	si.SType = vn.VK_STRUCTURE_TYPE_SUBMIT_INFO
	si.CommandBufferCount, si.PCommandBuffers = 1, unsafe.Pointer(cbs)
	d.qmu.Lock()
	qs := get[vn.Args_vkQueueSubmit](a)
	qs.Queue, qs.SubmitCount, qs.PSubmits, qs.Fence = d.queue, 1, unsafe.Pointer(si), b.fence
	vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, d.table, unsafe.Pointer(qs))
	d.qmu.Unlock()
	if qs.Ret != vn.VK_SUCCESS {
		c.freeBatch(b)
		return nil
	}
	c.inflight = append(c.inflight, b)
	return &Fence{c: c, b: b}
}

// Wait waits until the GPU finished the fence's batch.
func (f *Fence) Wait() {
	if f == nil || f.b.fence == 0 {
		return
	}
	c := f.c
	for len(c.inflight) != 0 {
		done := c.inflight[0] == f.b
		c.waitOldest()
		if done {
			return
		}
	}
}

// waitOldest waits for the oldest batch in flight, and frees it.
func (c *Context) waitOldest() {
	b := c.inflight[0]
	c.inflight = c.inflight[1:]
	a := c.a
	defer a.reset()
	fence := get[vn.Handle](a)
	*fence = b.fence
	wf := get[vn.Args_vkWaitForFences](a)
	wf.Device, wf.FenceCount, wf.PFences, wf.WaitAll, wf.Timeout = c.d.dev, 1, unsafe.Pointer(fence), 1, ^uint64(0)
	vk.Call(vn.VK_COMMAND_TYPE_vkWaitForFences_EXT, c.d.table, unsafe.Pointer(wf))
	c.freeBatch(b)
}

// retire frees the batches that the GPU finished.
func (c *Context) retire() {
	a := c.a
	for len(c.inflight) != 0 {
		b := c.inflight[0]
		gs := get[vn.Args_vkGetFenceStatus](a)
		gs.Device, gs.Fence = c.d.dev, b.fence
		vk.Call(vn.VK_COMMAND_TYPE_vkGetFenceStatus_EXT, c.d.table, unsafe.Pointer(gs))
		if gs.Ret != vn.VK_SUCCESS {
			return
		}
		c.inflight = c.inflight[1:]
		c.freeBatch(b)
	}
}

// freeBatch frees a batch that the GPU is done with, or that was never
// submitted.
func (c *Context) freeBatch(b *batch) {
	d, a := c.d, c.a
	for _, s := range b.staging {
		d.DestroyBuffer(s)
	}
	b.staging = nil
	if b.descPool != 0 {
		dp := get[vn.Args_vkDestroyDescriptorPool](a)
		dp.Device, dp.DescriptorPool = d.dev, b.descPool
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDescriptorPool_EXT, d.table, unsafe.Pointer(dp))
	}
	if b.fence != 0 {
		df := get[vn.Args_vkDestroyFence](a)
		df.Device, df.Fence = d.dev, b.fence
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyFence_EXT, d.table, unsafe.Pointer(df))
	}
	cbs := get[vn.Handle](a)
	*cbs = b.cb
	fc := get[vn.Args_vkFreeCommandBuffers](a)
	fc.Device, fc.CommandPool, fc.CommandBufferCount, fc.PCommandBuffers = d.dev, c.pool, 1, unsafe.Pointer(cbs)
	vk.Call(vn.VK_COMMAND_TYPE_vkFreeCommandBuffers_EXT, d.table, unsafe.Pointer(fc))
}

// outsideRendering returns the batch's commands, of no rendering
// instance, as transfers and barriers must be, ending the batch's.
func (c *Context) outsideRendering() (*Commands, error) {
	cmds, err := c.commands()
	if err != nil {
		return nil, err
	}
	if c.batch.rendering {
		cmds.EndRendering()
		c.batch.rendering = false
	}
	return cmds, nil
}

// barrier orders the commands after of all memory after those before.
func (c *Commands) barrier() {
	mb := get[vn.VkMemoryBarrier](c.a)
	mb.SType = vn.VK_STRUCTURE_TYPE_MEMORY_BARRIER
	mb.SrcAccessMask = AccessMemoryWrite
	mb.DstAccessMask = AccessMemoryRead | AccessMemoryWrite
	pb := get[vn.Args_vkCmdPipelineBarrier](c.a)
	pb.SrcStageMask, pb.DstStageMask = StageAllCommands, StageAllCommands
	pb.MemoryBarrierCount, pb.PMemoryBarriers = 1, unsafe.Pointer(mb)
	c.call(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, unsafe.Pointer(pb))
}

// staging returns a new buffer, visible to the host, of size bytes, which
// the batch frees once the GPU is done with it.
func (c *Context) staging(size uint64) (*Buffer, error) {
	b, err := c.d.NewBuffer(size, BufferTransferSrc|BufferTransferDst, true)
	if err != nil {
		return nil, err
	}
	c.batch.staging = append(c.batch.staging, b)
	return b, nil
}

// WriteBuffer writes data to b at offset, in order of the context's
// commands.
func (c *Context) WriteBuffer(b *Buffer, offset uint64, data []byte) error {
	if offset > b.size || uint64(len(data)) > b.size-offset {
		return fmt.Errorf("writing %d bytes at %d of a buffer of %d", len(data), offset, b.size)
	}
	if len(data) == 0 {
		return nil
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	s, err := c.staging(uint64(len(data)))
	if err != nil {
		return err
	}
	copy(s.Bytes(), data)
	cmds.barrier()
	r := get[vn.VkBufferCopy](c.a)
	*r = vn.VkBufferCopy{DstOffset: offset, Size: uint64(len(data))}
	cb := get[vn.Args_vkCmdCopyBuffer](c.a)
	cb.SrcBuffer, cb.DstBuffer, cb.RegionCount, cb.PRegions = s.buf, b.buf, 1, unsafe.Pointer(r)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBuffer_EXT, unsafe.Pointer(cb))
	cmds.barrier()
	return nil
}

// ReadBuffer reads b at offset into out, once the GPU executed the
// context's commands before.
func (c *Context) ReadBuffer(b *Buffer, offset uint64, out []byte) error {
	if offset > b.size || uint64(len(out)) > b.size-offset {
		return fmt.Errorf("reading %d bytes at %d of a buffer of %d", len(out), offset, b.size)
	}
	if len(out) == 0 {
		return nil
	}
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	// The read's staging buffer, which outlives the batch, which the wait
	// frees.
	s, err := c.d.NewBuffer(uint64(len(out)), BufferTransferSrc|BufferTransferDst, true)
	if err != nil {
		c.a.reset()
		return err
	}
	defer c.d.DestroyBuffer(s)
	cmds.barrier()
	r := get[vn.VkBufferCopy](c.a)
	*r = vn.VkBufferCopy{SrcOffset: offset, Size: uint64(len(out))}
	cb := get[vn.Args_vkCmdCopyBuffer](c.a)
	cb.SrcBuffer, cb.DstBuffer, cb.RegionCount, cb.PRegions = b.buf, s.buf, 1, unsafe.Pointer(r)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBuffer_EXT, unsafe.Pointer(cb))
	cmds.hostBarrier()
	c.a.reset()
	f := c.Flush()
	if f == nil {
		return fmt.Errorf("reading a buffer: the device is lost")
	}
	f.Wait()
	copy(out, s.Bytes())
	return nil
}

// hostBarrier makes the commands' writes before visible to the host's
// reads, once their batch's fence signals.
func (c *Commands) hostBarrier() {
	mb := get[vn.VkMemoryBarrier](c.a)
	mb.SType = vn.VK_STRUCTURE_TYPE_MEMORY_BARRIER
	mb.SrcAccessMask = AccessMemoryWrite
	mb.DstAccessMask = AccessHostRead
	pb := get[vn.Args_vkCmdPipelineBarrier](c.a)
	pb.SrcStageMask, pb.DstStageMask = StageAllCommands, StageHost
	pb.MemoryBarrierCount, pb.PMemoryBarriers = 1, unsafe.Pointer(mb)
	c.call(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, unsafe.Pointer(pb))
}

// TextureRegion is a box of a texture's level, as virgl's transfers give
// them, of the guest's memory of rows of stride bytes and layers or slices
// of layerStride, of the level's size if 0.
type TextureRegion struct {
	Level       uint32
	Box         Box
	Stride      uint32
	LayerStride uint32
}

// copyRegion returns the Vulkan copy of a box of t's level, of a buffer of
// rows of its blocks, tightly, from offset.
func (t *Texture) copyRegion(r TextureRegion, offset uint64) vn.VkBufferImageCopy {
	b := r.Box
	c := vn.VkBufferImageCopy{
		BufferOffset:     offset,
		ImageSubresource: vn.VkImageSubresourceLayers{AspectMask: t.aspects, MipLevel: r.Level, LayerCount: 1},
		ImageOffset:      vn.VkOffset3D{X: int32(b.X), Y: int32(b.Y)},
		ImageExtent:      vn.VkExtent3D{Width: uint32(b.Width), Height: uint32(b.Height), Depth: 1},
	}
	switch t.Desc.Target {
	case Target1DArray, Target2DArray, TargetCube, TargetCubeArray:
		c.ImageSubresource.BaseArrayLayer, c.ImageSubresource.LayerCount = uint32(b.Z), uint32(b.Depth)
	case Target3D:
		c.ImageOffset.Z, c.ImageExtent.Depth = int32(b.Z), uint32(b.Depth)
	}
	return c
}

// memLayout returns the texture region's layout of the guest's memory: its
// rows' and layers' strides, of the level's size if the region says none,
// the bytes of a row of the box, its rows, and the bytes that it spans.
func (t *Texture) memLayout(r TextureRegion) (stride, layerStride, row, rows, span uint64) {
	f := t.Format
	w, h, _ := t.Desc.levelExtent(r.Level)
	blocksX := func(n int64) uint64 { return uint64((n + int64(f.BlockWidth) - 1) / int64(f.BlockWidth)) }
	blocksY := func(n int64) uint64 { return uint64((n + int64(f.BlockHeight) - 1) / int64(f.BlockHeight)) }
	stride, layerStride = uint64(r.Stride), uint64(r.LayerStride)
	if stride == 0 {
		stride = blocksX(w) * uint64(f.BlockBytes)
	}
	if layerStride == 0 {
		layerStride = stride * blocksY(h)
	}
	row = blocksX(r.Box.Width) * uint64(f.BlockBytes)
	rows = blocksY(r.Box.Height)
	span = uint64(r.Box.Depth-1)*layerStride + (rows-1)*stride + row
	return stride, layerStride, row, rows, span
}

// checkRegion returns an error unless r is a box of a level of t, in data
// from offset, as the decoder's pipe.c checks transfers.
func (t *Texture) checkRegion(r TextureRegion, data []byte, offset uint64) error {
	b := r.Box
	if b.Width <= 0 || b.Height <= 0 || b.Depth <= 0 || !t.Desc.boxOK(r.Level, b) {
		return fmt.Errorf("a box %+v of level %d of %+v", b, r.Level, t.Desc)
	}
	if t.Format.IsDepth() && t.Format.IsStencil() {
		return fmt.Errorf("transfers of %s", t.Format.Name)
	}
	stride, _, row, _, span := t.memLayout(r)
	if row > stride || offset > uint64(len(data)) || span > uint64(len(data))-offset {
		return fmt.Errorf("a box %+v of level %d of %+v, of %d bytes at %d of %d", b, r.Level, t.Desc, span, offset, len(data))
	}
	return nil
}

// packRegion copies a texture region of data from offset to dst, tightly,
// or the other way if unpack.
func (t *Texture) packRegion(r TextureRegion, data []byte, offset uint64, dst []byte, unpack bool) {
	stride, layerStride, row, rows, _ := t.memLayout(r)
	layers := uint64(r.Box.Depth)
	tight := uint64(0)
	for l := uint64(0); l < layers; l++ {
		for y := uint64(0); y < rows; y++ {
			o := offset + l*layerStride + y*stride
			if unpack {
				copy(data[o:o+row], dst[tight:tight+row])
			} else {
				copy(dst[tight:tight+row], data[o:o+row])
			}
			tight += row
		}
	}
}

// tightSize returns the bytes of a region's rows, tightly.
func (t *Texture) tightSize(r TextureRegion) uint64 {
	_, _, row, rows, _ := t.memLayout(r)
	return row * rows * uint64(r.Box.Depth)
}

// general transitions t to the general layout, in which the context keeps
// textures, if it is not.
func (c *Context) general(cmds *Commands, t *Texture) {
	if t.layout == ImageLayoutGeneral {
		return
	}
	b := get[vn.VkImageMemoryBarrier](c.a)
	b.SType = vn.VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER
	b.SrcAccessMask, b.DstAccessMask = AccessMemoryWrite, AccessMemoryRead|AccessMemoryWrite
	b.OldLayout, b.NewLayout = t.layout, ImageLayoutGeneral
	b.SrcQueueFamilyIndex, b.DstQueueFamilyIndex = ^uint32(0), ^uint32(0)
	b.Image = t.img
	b.SubresourceRange = vn.VkImageSubresourceRange{AspectMask: t.aspects, LevelCount: ^uint32(0), LayerCount: ^uint32(0)}
	pb := get[vn.Args_vkCmdPipelineBarrier](c.a)
	pb.SrcStageMask, pb.DstStageMask = StageAllCommands, StageAllCommands
	pb.ImageMemoryBarrierCount, pb.PImageMemoryBarriers = 1, unsafe.Pointer(b)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, unsafe.Pointer(pb))
	t.layout = ImageLayoutGeneral
}

// WriteTexture writes a region of t of the guest's memory, data from
// offset, in order of the context's commands.
func (c *Context) WriteTexture(t *Texture, r TextureRegion, data []byte, offset uint64) error {
	if err := t.checkRegion(r, data, offset); err != nil {
		return err
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	s, err := c.staging(t.tightSize(r))
	if err != nil {
		return err
	}
	t.packRegion(r, data, offset, s.Bytes(), false)
	c.general(cmds, t)
	cmds.barrier()
	region := get[vn.VkBufferImageCopy](c.a)
	*region = t.copyRegion(r, 0)
	ci := get[vn.Args_vkCmdCopyBufferToImage](c.a)
	ci.SrcBuffer, ci.DstImage, ci.DstImageLayout = s.buf, t.img, t.layout
	ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(region)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBufferToImage_EXT, unsafe.Pointer(ci))
	cmds.barrier()
	return nil
}

// ReadTexture reads a region of t into the guest's memory, data from
// offset, once the GPU executed the context's commands before.
func (c *Context) ReadTexture(t *Texture, r TextureRegion, data []byte, offset uint64) error {
	if err := t.checkRegion(r, data, offset); err != nil {
		return err
	}
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	s, err := c.d.NewBuffer(t.tightSize(r), BufferTransferSrc|BufferTransferDst, true)
	if err != nil {
		c.a.reset()
		return err
	}
	defer c.d.DestroyBuffer(s)
	c.general(cmds, t)
	cmds.barrier()
	region := get[vn.VkBufferImageCopy](c.a)
	*region = t.copyRegion(r, 0)
	ci := get[vn.Args_vkCmdCopyImageToBuffer](c.a)
	ci.SrcImage, ci.SrcImageLayout, ci.DstBuffer = t.img, t.layout, s.buf
	ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(region)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyImageToBuffer_EXT, unsafe.Pointer(ci))
	cmds.hostBarrier()
	c.a.reset()
	f := c.Flush()
	if f == nil {
		return fmt.Errorf("reading a texture: the device is lost")
	}
	f.Wait()
	t.packRegion(r, data, offset, s.Bytes(), true)
	return nil
}
