// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Shadow images, for the images of guests' dma-bufs whose usage the host's
// driver gives no linear image.
//
// The images that guests create of their dma-bufs, Android's buffers among
// them, are linear images of the host's in the renderer's shared memory
// (see modifier_unsafe.go), which every process that shares the buffer maps.
// Some drivers cannot render to linear images: NVIDIA's lists color
// attachments among the linear tiling features of no format, and faults
// (Xid 69) when it renders to one. An image that the guest creates for such
// a use is a shadow: an optimal image of the host's, in device-local memory
// of the renderer's, which the guest's commands use, and a linear image in
// the guest's memory, which only the renderer's copies use:
//
//   - The guest sees one image. Its memory requirements and its
//     subresources' layouts are the linear image's, and its memory binds the
//     linear image. The linear modifier's format features include what
//     shadows give.
//   - The renderer copies the linear image to the shadow where the guest
//     takes the image from a foreign or external queue family, as it does
//     an image that other processes or the CPU wrote, and the shadow to the
//     linear image where the guest gives it to one, and at the end of each
//     command buffer that wrote it: render passes and dynamic rendering that
//     store to it, and transfers to it. Shaders' writes to storage images it
//     does not see.
//   - A fork's child creates its own shadow, a copy of its parent's.
//
// Images whose usage linear images have, such as the buffers that
// SurfaceFlinger only samples, and every image on hosts whose linear images
// have what optimal ones have, as MoltenVK's on Apple silicon, are not
// shadows, and nothing tracks their use.

// shadowImage is what the renderer keeps of an image that is a shadow: the
// image's host handle is the shadow's.
type shadowImage struct {
	// linear is the linear image that the guest's memory binds.
	linear vn.Handle
	// memory is the shadow's memory, the renderer's.
	memory vn.Handle
}

// liveShadows counts the shadows of the renderer's contexts. Without any,
// what shadows need of commands is skipped.
var liveShadows atomic.Int32

// Format features, VkFormatFeatureFlagBits.
const (
	featureSampledImage        = 0x1
	featureStorageImage        = 0x2
	featureStorageImageAtomic  = 0x4
	featureColorAttachment     = 0x80
	featureColorBlend          = 0x100
	featureSampledFilterLinear = 0x1000
	featureTransfers           = 0x4000 | 0x8000
)

// Image usages, VkImageUsageFlagBits, beyond layout_unsafe.go's.
const (
	imageUsageStorage         = 0x8
	imageUsageInputAttachment = 0x80
)

// shadowUsages are the usages that shadows give images, with the format
// feature that each needs and the features that come with it.
var shadowUsages = []struct {
	usage, needs, brings uint32
}{
	{imageUsageColorAttachment, featureColorAttachment, featureColorBlend},
	{imageUsageInputAttachment, featureColorAttachment, 0},
	{imageUsageStorage, featureStorageImage, featureStorageImageAtomic},
	{imageUsageSampled, featureSampledImage, featureSampledFilterLinear},
}

// shadowFeatures returns the format features of images of a format, whose
// linear and optimal tiling features are linear and optimal, that are linear
// images or shadows.
func shadowFeatures(linear, optimal uint32) uint32 {
	if linear&featureTransfers != featureTransfers || optimal&featureTransfers != featureTransfers {
		return linear
	}
	f := linear
	for _, u := range shadowUsages {
		if linear&u.needs == 0 && optimal&u.needs != 0 {
			f |= optimal & (u.needs | u.brings)
		}
	}
	return f
}

// shadowNeeded reports whether an image of usage, of a format whose linear
// and optimal tiling features are linear and optimal, needs a shadow.
func shadowNeeded(linear, optimal, usage uint32) bool {
	if linear&featureTransfers != featureTransfers || optimal&featureTransfers != featureTransfers {
		return false
	}
	for _, u := range shadowUsages {
		if usage&u.usage != 0 && linear&u.needs == 0 && optimal&u.needs != 0 {
			return true
		}
	}
	return false
}

// shadowableFormat reports whether images of a format may be shadows: those
// of one color plane, which copies copy whole.
func shadowableFormat(format uint32) bool {
	return formatPlaneCount(format) == 1 && formatAspects(format) == aspectColor
}

// formatCache caches the format properties of a physical device.
type formatCache struct {
	mu    sync.Mutex
	props map[uint32]vn.VkFormatProperties
}

// formatProperties returns the host's properties of format on pd.
func (s *stream) formatProperties(pd *physicalDevice, format uint32) vn.VkFormatProperties {
	fc := pd.formats
	fc.mu.Lock()
	props, ok := fc.props[format]
	fc.mu.Unlock()
	if ok {
		return props
	}
	out := newArgs[vn.VkFormatProperties](s)
	a := newArgs[vn.Args_vkGetPhysicalDeviceFormatProperties](s)
	a.PhysicalDevice, a.Format, a.PFormatProperties = pd.handle, format, unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFormatProperties_EXT, pd.inst.pdTable, unsafe.Pointer(a))
	props = *out
	fc.mu.Lock()
	fc.props[format] = props
	fc.mu.Unlock()
	return props
}

// modifierFeatures returns the format features of the linear modifier of a
// format whose properties are props: those of linear images, and what
// shadows give them.
func modifierFeatures(format uint32, props *vn.VkFormatProperties) uint32 {
	if !shadowableFormat(format) {
		return props.LinearTilingFeatures
	}
	return shadowFeatures(props.LinearTilingFeatures, props.OptimalTilingFeatures)
}

// shadows reports whether an image of a guest's dma-buf, which the host
// creates linear, of format, type, tiling and usage, is a shadow.
func (s *stream) shadows(pd *physicalDevice, format, imageType, tiling, usage uint32) bool {
	if tiling != imageTilingLinear || imageType != imageType2D || !shadowableFormat(format) {
		return false
	}
	props := s.formatProperties(pd, format)
	return shadowNeeded(props.LinearTilingFeatures, props.OptimalTilingFeatures, usage)
}

// imageType2D is VK_IMAGE_TYPE_2D.
const imageType2D = 1

// shadowCreateInfo makes info, the create info of a linear image that is a
// shadow, the shadow's, and returns the linear image's.
func (s *stream) shadowCreateInfo(info *vn.VkImageCreateInfo) *vn.VkImageCreateInfo {
	linear := newArgs[vn.VkImageCreateInfo](s)
	*linear = *info
	linear.Usage = imageUsageTransfers
	info.Tiling = imageTilingOptimal
	info.Usage |= imageUsageTransfers
	return linear
}

// linearImageFormatProperties returns the host's image format properties of
// the linear image of a shadow of format, type and flags, or nil if it has
// none.
func (s *stream) linearImageFormatProperties(pd *physicalDevice, format, imageType, flags uint32) *vn.VkImageFormatProperties {
	info := newArgs[vn.VkPhysicalDeviceImageFormatInfo2](s)
	info.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_FORMAT_INFO_2
	info.Format, info.Type, info.Tiling, info.Usage, info.Flags = format, imageType, imageTilingLinear, imageUsageTransfers, flags
	props := newArgs[vn.VkImageFormatProperties2](s)
	props.SType = vn.VK_STRUCTURE_TYPE_IMAGE_FORMAT_PROPERTIES_2
	a := newArgs[vn.Args_vkGetPhysicalDeviceImageFormatProperties2](s)
	a.PhysicalDevice, a.PImageFormatInfo, a.PImageFormatProperties = pd.handle, unsafe.Pointer(info), unsafe.Pointer(props)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceImageFormatProperties2_EXT, pd.inst.pdTable, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		return nil
	}
	return &props.ImageFormatProperties
}

// limitImageFormatProperties limits props, a shadow's, to what its linear
// image, of linear's, can have too.
func limitImageFormatProperties(props, linear *vn.VkImageFormatProperties) {
	props.MaxExtent.Width = min(props.MaxExtent.Width, linear.MaxExtent.Width)
	props.MaxExtent.Height = min(props.MaxExtent.Height, linear.MaxExtent.Height)
	props.MaxExtent.Depth = min(props.MaxExtent.Depth, linear.MaxExtent.Depth)
	props.MaxMipLevels = min(props.MaxMipLevels, linear.MaxMipLevels)
	props.MaxArrayLayers = min(props.MaxArrayLayers, linear.MaxArrayLayers)
	props.SampleCounts &= linear.SampleCounts
	props.MaxResourceSize = min(props.MaxResourceSize, linear.MaxResourceSize)
}

// vkMemoryPropertyDeviceLocal is VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT.
const vkMemoryPropertyDeviceLocal = 0x1

// createShadow gives image o, the shadow that the guest's command created,
// its memory, and creates its linear image, of linear. It reports whether
// it did.
func (s *stream) createShadow(o *object, linear *vn.VkImageCreateInfo) bool {
	dev := o.dev
	t := dev.table
	out := alloc[vn.Handle](s, 1)
	ci := newArgs[vn.Args_vkCreateImage](s)
	ci.Device, ci.PCreateInfo, ci.PImage = dev.handle, unsafe.Pointer(linear), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImage_EXT, t, unsafe.Pointer(ci))
	if ci.Ret != vn.VK_SUCCESS {
		s.ctx.logf("creating the linear image of a shadow of format %d: %d", linear.Format, ci.Ret)
		return false
	}
	sh := &shadowImage{linear: *out}

	reqs := newArgs[vn.VkMemoryRequirements](s)
	mr := newArgs[vn.Args_vkGetImageMemoryRequirements](s)
	mr.Device, mr.Image, mr.PMemoryRequirements = dev.handle, o.handle, unsafe.Pointer(reqs)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements_EXT, t, unsafe.Pointer(mr))
	typeIndex := -1
	for i := uint32(0); i < dev.pd.memProps.MemoryTypeCount; i++ {
		if reqs.MemoryTypeBits&(1<<i) == 0 {
			continue
		}
		if typeIndex < 0 {
			typeIndex = int(i)
		}
		if dev.pd.memProps.MemoryTypes[i].PropertyFlags&vkMemoryPropertyDeviceLocal != 0 {
			typeIndex = int(i)
			break
		}
	}
	dedicated := newArgs[vn.VkMemoryDedicatedAllocateInfo](s)
	dedicated.SType = vn.VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO
	dedicated.Image = o.handle
	info := newArgs[vn.VkMemoryAllocateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO
	info.PNext = unsafe.Pointer(dedicated)
	info.AllocationSize = reqs.Size
	info.MemoryTypeIndex = uint32(typeIndex)
	mem := alloc[vn.Handle](s, 1)
	am := newArgs[vn.Args_vkAllocateMemory](s)
	am.Device, am.PAllocateInfo, am.PMemory = dev.handle, unsafe.Pointer(info), unsafe.Pointer(mem)
	if typeIndex >= 0 {
		vk.Call(vn.VK_COMMAND_TYPE_vkAllocateMemory_EXT, t, unsafe.Pointer(am))
	}
	if typeIndex < 0 || am.Ret != vn.VK_SUCCESS {
		s.ctx.logf("allocating %d bytes for a shadow of format %d: %d", reqs.Size, linear.Format, am.Ret)
		s.destroyShadow(dev, sh)
		return false
	}
	sh.memory = *mem
	bind := newArgs[vn.Args_vkBindImageMemory](s)
	bind.Device, bind.Image, bind.Memory = dev.handle, o.handle, sh.memory
	vk.Call(vn.VK_COMMAND_TYPE_vkBindImageMemory_EXT, t, unsafe.Pointer(bind))
	if bind.Ret != vn.VK_SUCCESS {
		s.ctx.logf("binding a shadow of format %d: %d", linear.Format, bind.Ret)
		s.destroyShadow(dev, sh)
		return false
	}
	o.shadow = sh
	liveShadows.Add(1)
	return true
}

// destroyShadow destroys what the renderer created for a shadow, whose own
// image is destroyed.
func (s *stream) destroyShadow(dev *device, sh *shadowImage) {
	if sh.linear != 0 {
		a := newArgs[vn.Args_vkDestroyImage](s)
		a.Device, a.Image = dev.handle, sh.linear
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImage_EXT, dev.table, unsafe.Pointer(a))
	}
	if sh.memory != 0 {
		a := newArgs[vn.Args_vkFreeMemory](s)
		a.Device, a.Memory = dev.handle, sh.memory
		vk.Call(vn.VK_COMMAND_TYPE_vkFreeMemory_EXT, dev.table, unsafe.Pointer(a))
	}
}

// releaseShadow destroys what the renderer created for image o, if it is a
// shadow, once its own image is destroyed.
func (s *stream) releaseShadow(o *object) {
	if o.shadow == nil {
		return
	}
	s.destroyShadow(o.dev, o.shadow)
	o.shadow = nil
	liveShadows.Add(-1)
}

// linearImage returns the linear image of image id, if it is a shadow, or 0.
// The guest's memory binds it, and the guest's queries of its memory and
// layout are its.
func (s *stream) linearImage(id uint64) vn.Handle {
	if o := s.ctx.lookupType(id, vn.VK_OBJECT_TYPE_IMAGE); o != nil && o.shadow != nil {
		return o.shadow.linear
	}
	return 0
}

// memoryHandle returns the host handle of the buffer or image that o's
// memory binds: a shadow's linear image's.
func (o *object) memoryHandle() vn.Handle {
	if o.shadow != nil {
		return o.shadow.linear
	}
	return o.handle
}

// Attachment store ops, VkAttachmentStoreOp.
const attachmentStoreOpStore = 0

// attachmentOp is what a render pass does with an attachment at its end.
type attachmentOp struct {
	finalLayout uint32
	// stores is whether the render pass stores what it rendered.
	stores bool
}

// fbShadow is an attachment of a framebuffer that views a shadow.
type fbShadow struct {
	index uint32
	image *object
}

// shadowWrite is a shadow that a command buffer wrote, and the layout that
// its commands leave it in.
type shadowWrite struct {
	image  *object
	layout uint32
}

// wrote records that command buffer cb's commands leave shadow img in
// layout, written.
func (cb *object) wrote(img *object, layout uint32) {
	for i := range cb.shadowWrites {
		if cb.shadowWrites[i].image == img {
			cb.shadowWrites[i].layout = layout
			return
		}
	}
	cb.shadowWrites = append(cb.shadowWrites, shadowWrite{img, layout})
}

// written returns the index of shadow img in command buffer cb's writes, or
// -1.
func (cb *object) written(img *object) int {
	for i := range cb.shadowWrites {
		if cb.shadowWrites[i].image == img {
			return i
		}
	}
	return -1
}

// shadowView returns the shadow that image view id views, or nil.
func (s *stream) shadowView(id uint64) *object {
	if v := s.ctx.lookupType(id, vn.VK_OBJECT_TYPE_IMAGE_VIEW); v != nil {
		return v.viewOf
	}
	return nil
}

// commandBuffer returns command buffer id, while shadows exist, or nil.
func (s *stream) commandBuffer(id uint64) *object {
	if liveShadows.Load() == 0 {
		return nil
	}
	return s.ctx.lookupType(id, vn.VK_OBJECT_TYPE_COMMAND_BUFFER)
}

// writeImage records that command buffer cbID writes image id, in layout,
// if it is a shadow.
func (s *stream) writeImage(cbID, id uint64, layout uint32) {
	cb := s.commandBuffer(cbID)
	if cb == nil {
		return
	}
	if img := s.ctx.lookupType(id, vn.VK_OBJECT_TYPE_IMAGE); img != nil && img.shadow != nil {
		cb.wrote(img, layout)
	}
}

// shadowCopy is a copy between a shadow and its linear image, around which
// the shadow is in layout.
type shadowCopy struct {
	image  *object
	layout uint32
}

// shadowTransfers prepares the count image barriers at images, of
// VkImageMemoryBarrier2 if v2 or else VkImageMemoryBarrier, of command
// buffer cb, for their shadows, before their handles are replaced: a shadow
// is the renderer's, so that its barriers transfer it to or from no queue
// family. It returns the copies that the barriers need: of the shadows that
// cb wrote to their linear images before the barriers give them to a
// foreign or external queue family, and of the linear images to the
// shadows after the barriers take them from one. If execute is false, as
// for vkCmdSetEvent2, the barriers' transfers are only prepared.
func (s *stream) shadowTransfers(cb *object, images unsafe.Pointer, count uint32, v2, execute bool) (release, acquire []shadowCopy) {
	if cb == nil || images == nil {
		return nil, nil
	}
	for i := uint32(0); i < count; i++ {
		var src, dst, oldLayout, newLayout *uint32
		var id uint64
		if v2 {
			b := (*vn.VkImageMemoryBarrier2)(elemAt(images, i, unsafe.Sizeof(vn.VkImageMemoryBarrier2{})))
			src, dst, oldLayout, newLayout, id = &b.SrcQueueFamilyIndex, &b.DstQueueFamilyIndex, &b.OldLayout, &b.NewLayout, uint64(b.Image)
		} else {
			b := (*vn.VkImageMemoryBarrier)(elemAt(images, i, unsafe.Sizeof(vn.VkImageMemoryBarrier{})))
			src, dst, oldLayout, newLayout, id = &b.SrcQueueFamilyIndex, &b.DstQueueFamilyIndex, &b.OldLayout, &b.NewLayout, uint64(b.Image)
		}
		img := s.ctx.lookupType(id, vn.VK_OBJECT_TYPE_IMAGE)
		if img == nil || img.shadow == nil {
			continue
		}
		fromOutside, toOutside := outsideFamily(*src), outsideFamily(*dst)
		if fromOutside || toOutside {
			*src, *dst = queueFamilyIgnored, queueFamilyIgnored
		}
		if !execute {
			continue
		}
		w := cb.written(img)
		switch {
		case toOutside && !fromOutside:
			if w >= 0 {
				if *oldLayout != imageLayoutUndefined {
					release = append(release, shadowCopy{img, *oldLayout})
				}
				cb.shadowWrites = append(cb.shadowWrites[:w], cb.shadowWrites[w+1:]...)
			}
		case fromOutside && !toOutside:
			if *newLayout != imageLayoutUndefined && *newLayout != imageLayoutPreinitialized {
				acquire = append(acquire, shadowCopy{img, *newLayout})
			}
			if w >= 0 {
				cb.shadowWrites[w].layout = *newLayout
			}
		case w >= 0:
			cb.shadowWrites[w].layout = *newLayout
		}
	}
	return release, acquire
}

// outsideFamily reports whether a queue family index is a foreign or an
// external queue family, whose owner is outside the guest's device.
func outsideFamily(index uint32) bool {
	return index == queueFamilyForeign || index == queueFamilyExternal
}

// Values of Vulkan's that copies between shadows and their linear images
// use.
const (
	// queueFamilyExternal is VK_QUEUE_FAMILY_EXTERNAL.
	queueFamilyExternal = 0xfffffffe

	imageLayoutUndefined          = 0
	imageLayoutPreinitialized     = 8
	imageLayoutTransferSrcOptimal = 6
	imageLayoutTransferDstOptimal = 7

	stageTransfer    = 0x1000
	stageHost        = 0x4000
	stageAllCommands = 0x10000

	accessTransferRead  = 0x800
	accessTransferWrite = 0x1000
	accessHostRead      = 0x2000
	accessMemoryRead    = 0x8000
	accessMemoryWrite   = 0x10000

	// remaining is VK_REMAINING_MIP_LEVELS and VK_REMAINING_ARRAY_LAYERS.
	remaining = ^uint32(0)
)

// liveCopies returns the copies of shadows that exist still: a command
// buffer whose shadow was destroyed is invalid, but may be ended.
func liveCopies(copies []shadowCopy) []shadowCopy {
	live := copies[:0]
	for _, c := range copies {
		if c.image.shadow != nil {
			live = append(live, c)
		}
	}
	return live
}

// imageBarrier returns a barrier of all of image's subresources.
func imageBarrier(image vn.Handle, oldLayout, newLayout, srcAccess, dstAccess uint32) vn.VkImageMemoryBarrier {
	return vn.VkImageMemoryBarrier{
		SType:               vn.VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
		SrcAccessMask:       srcAccess,
		DstAccessMask:       dstAccess,
		OldLayout:           oldLayout,
		NewLayout:           newLayout,
		SrcQueueFamilyIndex: queueFamilyIgnored,
		DstQueueFamilyIndex: queueFamilyIgnored,
		Image:               image,
		SubresourceRange:    vn.VkImageSubresourceRange{AspectMask: aspectColor, LevelCount: remaining, LayerCount: remaining},
	}
}

// pipelineBarrier records image barriers in command buffer cb.
func (s *stream) pipelineBarrier(cb *object, srcStage, dstStage uint32, barriers []vn.VkImageMemoryBarrier) {
	a := newArgs[vn.Args_vkCmdPipelineBarrier](s)
	a.CommandBuffer, a.SrcStageMask, a.DstStageMask = cb.handle, srcStage, dstStage
	a.ImageMemoryBarrierCount, a.PImageMemoryBarriers = uint32(len(barriers)), unsafe.Pointer(&barriers[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, cb.dev.table, unsafe.Pointer(a))
}

// copyWhole records a copy of every level and layer of an image of info in
// command buffer cb, from src to dst, in their layouts.
func (s *stream) copyWhole(cb *object, info *imageInfo, src vn.Handle, srcLayout uint32, dst vn.Handle, dstLayout uint32) {
	n := int(info.mipLevels)
	regions := unsafe.Slice(alloc[vn.VkImageCopy](s, n), n)
	for level := range regions {
		sub := vn.VkImageSubresourceLayers{AspectMask: aspectColor, MipLevel: uint32(level), LayerCount: info.arrayLayers}
		regions[level] = vn.VkImageCopy{
			SrcSubresource: sub,
			DstSubresource: sub,
			Extent: vn.VkExtent3D{
				Width:  max(1, info.extent.Width>>level),
				Height: max(1, info.extent.Height>>level),
				Depth:  max(1, info.extent.Depth>>level),
			},
		}
	}
	a := newArgs[vn.Args_vkCmdCopyImage](s)
	a.CommandBuffer = cb.handle
	a.SrcImage, a.SrcImageLayout = src, srcLayout
	a.DstImage, a.DstImageLayout = dst, dstLayout
	a.RegionCount, a.PRegions = uint32(n), unsafe.Pointer(&regions[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkCmdCopyImage_EXT, cb.dev.table, unsafe.Pointer(a))
}

// copyToLinear records in command buffer cb copies of shadows to their
// linear images, after what cb recorded before, and before what it records
// after; the shadows are left in their layouts. The linear images stay in
// the general layout, which linear images in host memory need no
// transition into.
func (s *stream) copyToLinear(cb *object, copies []shadowCopy) {
	copies = liveCopies(copies)
	if len(copies) == 0 {
		return
	}
	n := len(copies)
	before := unsafe.Slice(alloc[vn.VkImageMemoryBarrier](s, 2*n), 2*n)
	after := unsafe.Slice(alloc[vn.VkImageMemoryBarrier](s, 2*n), 2*n)
	for i, c := range copies {
		shadow, linear := c.image.handle, c.image.shadow.linear
		before[2*i] = imageBarrier(shadow, c.layout, imageLayoutTransferSrcOptimal, accessMemoryWrite, accessTransferRead)
		// The copy overwrites the whole linear image.
		before[2*i+1] = imageBarrier(linear, imageLayoutUndefined, imageLayoutGeneral, accessMemoryWrite, accessTransferWrite)
		after[2*i] = imageBarrier(shadow, imageLayoutTransferSrcOptimal, c.layout, 0, accessMemoryRead|accessMemoryWrite)
		after[2*i+1] = imageBarrier(linear, imageLayoutGeneral, imageLayoutGeneral, accessTransferWrite, accessMemoryRead|accessHostRead)
	}
	s.pipelineBarrier(cb, stageAllCommands, stageTransfer, before)
	for _, c := range copies {
		s.copyWhole(cb, c.image.image, c.image.handle, imageLayoutTransferSrcOptimal, c.image.shadow.linear, imageLayoutGeneral)
	}
	s.pipelineBarrier(cb, stageTransfer, stageAllCommands|stageHost, after)
}

// copyFromLinear records in command buffer cb copies of linear images to
// their shadows, after what cb recorded before, and before what it records
// after; the shadows are left in their layouts.
func (s *stream) copyFromLinear(cb *object, copies []shadowCopy) {
	copies = liveCopies(copies)
	if len(copies) == 0 {
		return
	}
	n := len(copies)
	before := unsafe.Slice(alloc[vn.VkImageMemoryBarrier](s, 2*n), 2*n)
	after := unsafe.Slice(alloc[vn.VkImageMemoryBarrier](s, n), n)
	for i, c := range copies {
		shadow, linear := c.image.handle, c.image.shadow.linear
		// The copy overwrites the whole shadow.
		before[2*i] = imageBarrier(shadow, imageLayoutUndefined, imageLayoutTransferDstOptimal, accessMemoryWrite, accessTransferWrite)
		before[2*i+1] = imageBarrier(linear, imageLayoutGeneral, imageLayoutGeneral, accessMemoryWrite, accessTransferRead)
		after[i] = imageBarrier(shadow, imageLayoutTransferDstOptimal, c.layout, accessTransferWrite, accessMemoryRead|accessMemoryWrite)
	}
	s.pipelineBarrier(cb, stageAllCommands, stageTransfer, before)
	for _, c := range copies {
		s.copyWhole(cb, c.image.image, c.image.shadow.linear, imageLayoutGeneral, c.image.handle, imageLayoutTransferDstOptimal)
	}
	s.pipelineBarrier(cb, stageTransfer, stageAllCommands, after)
}

// shadowDependencies prepares the image barriers of count dependency infos
// at deps for their shadows, as shadowTransfers does.
func (s *stream) shadowDependencies(cb *object, deps unsafe.Pointer, count uint32, execute bool) (release, acquire []shadowCopy) {
	if cb == nil || deps == nil {
		return nil, nil
	}
	for _, d := range unsafe.Slice((*vn.VkDependencyInfo)(deps), count) {
		r, a := s.shadowTransfers(cb, d.PImageMemoryBarriers, d.ImageMemoryBarrierCount, true, execute)
		release, acquire = append(release, r...), append(acquire, a...)
	}
	return release, acquire
}

// barrierCommand executes a command of a command buffer that records
// barriers, with the copies that they need for shadows, which prepare
// returns once it prepared the barriers.
func (s *stream) barrierCommand(cmd *vn.Command, args unsafe.Pointer, prepare func(cb *object) (release, acquire []shadowCopy)) {
	cb := s.commandBuffer(firstID(args))
	release, acquire := prepare(cb)
	s.copyToLinear(cb, release)
	passThrough(s, cmd, args)
	s.copyFromLinear(cb, acquire)
}

// renderPassAttachments returns what render pass info does with its
// attachments, of VkAttachmentDescription2 if v2.
func renderPassAttachments(attachments unsafe.Pointer, count uint32, v2 bool) []attachmentOp {
	if attachments == nil || count == 0 {
		return nil
	}
	ops := make([]attachmentOp, count)
	for i := range ops {
		var store, stencilStore, final uint32
		if v2 {
			d := (*vn.VkAttachmentDescription2)(elemAt(attachments, uint32(i), unsafe.Sizeof(vn.VkAttachmentDescription2{})))
			store, stencilStore, final = d.StoreOp, d.StencilStoreOp, d.FinalLayout
		} else {
			d := (*vn.VkAttachmentDescription)(elemAt(attachments, uint32(i), unsafe.Sizeof(vn.VkAttachmentDescription{})))
			store, stencilStore, final = d.StoreOp, d.StencilStoreOp, d.FinalLayout
		}
		ops[i] = attachmentOp{finalLayout: final, stores: store == attachmentStoreOpStore || stencilStore == attachmentStoreOpStore}
	}
	return ops
}

// beginRenderPass records the shadows that the render pass that begin
// begins, in command buffer cbID, writes.
func (s *stream) beginRenderPass(cbID uint64, begin unsafe.Pointer) {
	cb := s.commandBuffer(cbID)
	if cb == nil || begin == nil {
		return
	}
	info := (*vn.VkRenderPassBeginInfo)(begin)
	rp := s.ctx.lookupType(uint64(info.RenderPass), vn.VK_OBJECT_TYPE_RENDER_PASS)
	if rp == nil {
		return
	}
	write := func(index uint32, img *object) {
		if img != nil && index < uint32(len(rp.attachmentOps)) && rp.attachmentOps[index].stores {
			cb.wrote(img, rp.attachmentOps[index].finalLayout)
		}
	}
	if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_RENDER_PASS_ATTACHMENT_BEGIN_INFO); p != nil {
		// An imageless framebuffer's attachments.
		views := (*vn.VkRenderPassAttachmentBeginInfo)(p)
		for i := uint32(0); i < views.AttachmentCount; i++ {
			write(i, s.shadowView(*(*uint64)(elemAt(views.PAttachments, i, 8))))
		}
		return
	}
	if fb := s.ctx.lookupType(uint64(info.Framebuffer), vn.VK_OBJECT_TYPE_FRAMEBUFFER); fb != nil {
		for _, a := range fb.fbShadows {
			write(a.index, a.image)
		}
	}
}

// beginRendering records the shadows that the dynamic rendering of info, in
// command buffer cbID, writes.
func (s *stream) beginRendering(cbID uint64, rendering unsafe.Pointer) {
	cb := s.commandBuffer(cbID)
	if cb == nil || rendering == nil {
		return
	}
	info := (*vn.VkRenderingInfo)(rendering)
	for i := uint32(0); i < info.ColorAttachmentCount; i++ {
		a := (*vn.VkRenderingAttachmentInfo)(elemAt(info.PColorAttachments, i, unsafe.Sizeof(vn.VkRenderingAttachmentInfo{})))
		if img := s.shadowView(uint64(a.ImageView)); img != nil && a.StoreOp == attachmentStoreOpStore {
			cb.wrote(img, a.ImageLayout)
		}
		if a.ResolveMode == 0 {
			continue
		}
		if img := s.shadowView(uint64(a.ResolveImageView)); img != nil {
			cb.wrote(img, a.ResolveImageLayout)
		}
	}
}

// endCommandBuffer copies the shadows that primary command buffer cbID
// wrote to their linear images, at its end. A secondary command buffer's
// writes are its primary's, which executes it.
func (s *stream) endCommandBuffer(cbID uint64) {
	cb := s.commandBuffer(cbID)
	if cb == nil || cb.level != commandBufferLevelPrimary || len(cb.shadowWrites) == 0 {
		return
	}
	copies := make([]shadowCopy, len(cb.shadowWrites))
	for i, w := range cb.shadowWrites {
		copies[i] = shadowCopy{w.image, w.layout}
	}
	cb.shadowWrites = nil
	s.copyToLinear(cb, copies)
}

// commandBufferLevelPrimary is VK_COMMAND_BUFFER_LEVEL_PRIMARY.
const commandBufferLevelPrimary = 0

func registerShadows() {
	handle(vn.VK_COMMAND_TYPE_vkDestroyImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if o := s.destroyObject(cmd, args, vn.VK_OBJECT_TYPE_IMAGE, unsafe.Pointer(&(*vn.Args_vkDestroyImage)(args).Image)); o != nil {
			s.releaseShadow(o)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateImageView_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateImageView)(args)
		var img *object
		if liveShadows.Load() != 0 && a.PCreateInfo != nil {
			img = s.ctx.lookupType(uint64((*vn.VkImageViewCreateInfo)(a.PCreateInfo).Image), vn.VK_OBJECT_TYPE_IMAGE)
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_IMAGE_VIEW, a.PView); o != nil && img != nil && img.shadow != nil {
			o.viewOf = img
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateFramebuffer_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateFramebuffer)(args)
		var shadows []fbShadow
		if liveShadows.Load() != 0 && a.PCreateInfo != nil {
			info := (*vn.VkFramebufferCreateInfo)(a.PCreateInfo)
			for i := uint32(0); i < info.AttachmentCount && info.PAttachments != nil; i++ {
				if img := s.shadowView(*(*uint64)(elemAt(info.PAttachments, i, 8))); img != nil {
					shadows = append(shadows, fbShadow{index: i, image: img})
				}
			}
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_FRAMEBUFFER, a.PFramebuffer); o != nil {
			o.fbShadows = shadows
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateRenderPass_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateRenderPass)(args)
		var ops []attachmentOp
		if a.PCreateInfo != nil {
			info := (*vn.VkRenderPassCreateInfo)(a.PCreateInfo)
			ops = renderPassAttachments(info.PAttachments, info.AttachmentCount, false)
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_RENDER_PASS, a.PRenderPass); o != nil {
			o.attachmentOps = ops
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateRenderPass2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateRenderPass2)(args)
		var ops []attachmentOp
		if a.PCreateInfo != nil {
			info := (*vn.VkRenderPassCreateInfo2)(a.PCreateInfo)
			ops = renderPassAttachments(info.PAttachments, info.AttachmentCount, true)
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_RENDER_PASS, a.PRenderPass); o != nil {
			o.attachmentOps = ops
		}
	})

	// What command buffers write to shadows.
	resetWrites := func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if cb := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_COMMAND_BUFFER); cb != nil {
			cb.shadowWrites = nil
		}
		passThrough(s, cmd, args)
	}
	handle(vn.VK_COMMAND_TYPE_vkBeginCommandBuffer_EXT, resetWrites)
	handle(vn.VK_COMMAND_TYPE_vkResetCommandBuffer_EXT, resetWrites)
	handle(vn.VK_COMMAND_TYPE_vkEndCommandBuffer_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.endCommandBuffer(firstID(args))
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdExecuteCommands_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdExecuteCommands)(args)
		if cb := s.commandBuffer(firstID(args)); cb != nil && a.PCommandBuffers != nil {
			for i := uint32(0); i < a.CommandBufferCount; i++ {
				sec := s.ctx.lookupType(*(*uint64)(elemAt(a.PCommandBuffers, i, 8)), vn.VK_OBJECT_TYPE_COMMAND_BUFFER)
				if sec == nil {
					continue
				}
				for _, w := range sec.shadowWrites {
					cb.wrote(w.image, w.layout)
				}
			}
		}
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdBeginRenderPass_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.beginRenderPass(firstID(args), (*vn.Args_vkCmdBeginRenderPass)(args).PRenderPassBegin)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdBeginRenderPass2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.beginRenderPass(firstID(args), (*vn.Args_vkCmdBeginRenderPass2)(args).PRenderPassBegin)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdBeginRendering_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.beginRendering(firstID(args), (*vn.Args_vkCmdBeginRendering)(args).PRenderingInfo)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdCopyImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdCopyImage)(args)
		s.writeImage(firstID(args), uint64(a.DstImage), a.DstImageLayout)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdCopyImage2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if p := (*vn.Args_vkCmdCopyImage2)(args).PCopyImageInfo; p != nil {
			info := (*vn.VkCopyImageInfo2)(p)
			s.writeImage(firstID(args), uint64(info.DstImage), info.DstImageLayout)
		}
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdCopyBufferToImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdCopyBufferToImage)(args)
		s.writeImage(firstID(args), uint64(a.DstImage), a.DstImageLayout)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdCopyBufferToImage2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if p := (*vn.Args_vkCmdCopyBufferToImage2)(args).PCopyBufferToImageInfo; p != nil {
			info := (*vn.VkCopyBufferToImageInfo2)(p)
			s.writeImage(firstID(args), uint64(info.DstImage), info.DstImageLayout)
		}
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdBlitImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdBlitImage)(args)
		s.writeImage(firstID(args), uint64(a.DstImage), a.DstImageLayout)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdBlitImage2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if p := (*vn.Args_vkCmdBlitImage2)(args).PBlitImageInfo; p != nil {
			info := (*vn.VkBlitImageInfo2)(p)
			s.writeImage(firstID(args), uint64(info.DstImage), info.DstImageLayout)
		}
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdResolveImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdResolveImage)(args)
		s.writeImage(firstID(args), uint64(a.DstImage), a.DstImageLayout)
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdResolveImage2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if p := (*vn.Args_vkCmdResolveImage2)(args).PResolveImageInfo; p != nil {
			info := (*vn.VkResolveImageInfo2)(p)
			s.writeImage(firstID(args), uint64(info.DstImage), info.DstImageLayout)
		}
		passThrough(s, cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdClearColorImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdClearColorImage)(args)
		s.writeImage(firstID(args), uint64(a.Image), a.ImageLayout)
		passThrough(s, cmd, args)
	})
}
