// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

/*
#include <stdint.h>

// vkMapMemory, which Venus's protocol, and so package vk's tables, lack:
// guests map memory of blobs.
typedef int32_t (*vkgl_map_memory)(uint64_t device, uint64_t memory, uint64_t offset,
                                   uint64_t size, uint32_t flags, void **data);

static int32_t vkgl_map(void *fn, uint64_t device, uint64_t memory, void **data) {
	return ((vkgl_map_memory)fn)(device, memory, 0, ~(uint64_t)0, 0, data);
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Buffer and image usages, of VkBufferUsageFlagBits and
// VkImageUsageFlagBits.
const (
	BufferTransferSrc = 0x1
	BufferTransferDst = 0x2
	BufferUniform     = 0x10
	BufferStorage     = 0x20
	BufferIndex       = 0x40
	BufferVertex      = 0x80
	BufferIndirect    = 0x100

	ImageTransferSrc     = 0x1
	ImageTransferDst     = 0x2
	ImageSampled         = 0x4
	ImageStorage         = 0x8
	ImageColorAttachment = 0x10
	ImageDepthAttachment = 0x20
)

// allocation is device memory of a buffer's or an image's, mapped if it is
// visible to the host.
type allocation struct {
	mem  vn.Handle
	size uint64
	// data is the memory, mapped, or nil.
	data unsafe.Pointer
}

// allocate allocates memory of reqs, of the properties want, preferring
// those of prefer too, and maps it if it is visible to the host.
func (d *Device) allocate(reqs *vn.VkMemoryRequirements, want, prefer uint32) (allocation, error) {
	a := d.a
	typ, ok := d.memoryType(reqs.MemoryTypeBits, want|prefer)
	if !ok {
		if typ, ok = d.memoryType(reqs.MemoryTypeBits, want); !ok {
			return allocation{}, fmt.Errorf("no memory type of properties %#x of types %#x", want, reqs.MemoryTypeBits)
		}
	}
	info := get[vn.VkMemoryAllocateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO
	info.AllocationSize = reqs.Size
	info.MemoryTypeIndex = typ
	mem := get[vn.Handle](a)
	am := get[vn.Args_vkAllocateMemory](a)
	am.Device, am.PAllocateInfo, am.PMemory = d.dev, unsafe.Pointer(info), unsafe.Pointer(mem)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateMemory_EXT, d.table, unsafe.Pointer(am))
	if err := check("vkAllocateMemory", am.Ret); err != nil {
		return allocation{}, err
	}
	m := allocation{mem: *mem, size: reqs.Size}
	if d.memProps.MemoryTypes[typ].PropertyFlags&memoryHostVisible != 0 {
		if d.mapMemory == nil {
			d.mapMemory = vk.DeviceProc(d.pdTable, vk.Handle(d.dev), "vkMapMemory")
		}
		if ret := int32(C.vkgl_map(d.mapMemory, C.uint64_t(d.dev), C.uint64_t(m.mem), &m.data)); ret != vn.VK_SUCCESS {
			d.free(m)
			return allocation{}, check("vkMapMemory", ret)
		}
	}
	return m, nil
}

// free frees memory, unmapping it.
func (d *Device) free(m allocation) {
	if m.mem == 0 {
		return
	}
	a := d.a
	fm := get[vn.Args_vkFreeMemory](a)
	fm.Device, fm.Memory = d.dev, m.mem
	vk.Call(vn.VK_COMMAND_TYPE_vkFreeMemory_EXT, d.table, unsafe.Pointer(fm))
}

// Buffer is a buffer of the device, and its memory.
type Buffer struct {
	buf  vn.Handle
	mem  allocation
	size uint64
}

// NewBuffer returns a buffer of size bytes, of usage, in memory of the
// device's, or, if hostVisible, visible to the host and mapped.
func (d *Device) NewBuffer(size uint64, usage uint32, hostVisible bool) (*Buffer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	info := get[vn.VkBufferCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO
	info.Size = size
	info.Usage = usage
	buf := get[vn.Handle](a)
	cb := get[vn.Args_vkCreateBuffer](a)
	cb.Device, cb.PCreateInfo, cb.PBuffer = d.dev, unsafe.Pointer(info), unsafe.Pointer(buf)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateBuffer_EXT, d.table, unsafe.Pointer(cb))
	if err := check("vkCreateBuffer", cb.Ret); err != nil {
		return nil, err
	}
	b := &Buffer{buf: *buf, size: size}
	reqs := get[vn.VkMemoryRequirements](a)
	mr := get[vn.Args_vkGetBufferMemoryRequirements](a)
	mr.Device, mr.Buffer, mr.PMemoryRequirements = d.dev, b.buf, unsafe.Pointer(reqs)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetBufferMemoryRequirements_EXT, d.table, unsafe.Pointer(mr))
	want, prefer := uint32(memoryDeviceLocal), uint32(0)
	if hostVisible {
		want, prefer = memoryHostVisible|memoryHostCoherent, 0
	}
	var err error
	if b.mem, err = d.allocate(reqs, want, prefer); err != nil {
		d.destroyBuffer(b)
		return nil, err
	}
	bm := get[vn.Args_vkBindBufferMemory](a)
	bm.Device, bm.Buffer, bm.Memory = d.dev, b.buf, b.mem.mem
	vk.Call(vn.VK_COMMAND_TYPE_vkBindBufferMemory_EXT, d.table, unsafe.Pointer(bm))
	if err := check("vkBindBufferMemory", bm.Ret); err != nil {
		d.destroyBuffer(b)
		return nil, err
	}
	return b, nil
}

// Bytes returns a mapped buffer's memory.
func (b *Buffer) Bytes() []byte {
	if b.mem.data == nil {
		return nil
	}
	return unsafe.Slice((*byte)(b.mem.data), b.size)
}

// DestroyBuffer destroys b, which the GPU no longer uses.
func (d *Device) DestroyBuffer(b *Buffer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	d.destroyBuffer(b)
}

func (d *Device) destroyBuffer(b *Buffer) {
	a := d.a
	if b.buf != 0 {
		db := get[vn.Args_vkDestroyBuffer](a)
		db.Device, db.Buffer = d.dev, b.buf
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyBuffer_EXT, d.table, unsafe.Pointer(db))
		b.buf = 0
	}
	d.free(b.mem)
	b.mem = allocation{}
}

// Image is a 2D image of the device, of one level and layer, and its
// memory and view.
type Image struct {
	img    vn.Handle
	view   vn.Handle
	mem    allocation
	format uint32
	width  uint32
	height uint32
	// layout is the image's layout, after the commands recorded.
	layout uint32
}

// NewImage returns an image of format, width by height, of usage, in memory
// of the device's, of a view of its color.
func (d *Device) NewImage(format, width, height, usage uint32) (*Image, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	info := get[vn.VkImageCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO
	info.ImageType = imageType2D
	info.Format = format
	info.Extent = vn.VkExtent3D{Width: width, Height: height, Depth: 1}
	info.MipLevels = 1
	info.ArrayLayers = 1
	info.Samples = 1
	info.Tiling = imageTilingOptimal
	info.Usage = usage
	h := get[vn.Handle](a)
	ci := get[vn.Args_vkCreateImage](a)
	ci.Device, ci.PCreateInfo, ci.PImage = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImage_EXT, d.table, unsafe.Pointer(ci))
	if err := check("vkCreateImage", ci.Ret); err != nil {
		return nil, err
	}
	img := &Image{img: *h, format: format, width: width, height: height, layout: ImageLayoutUndefined}
	reqs := get[vn.VkMemoryRequirements](a)
	mr := get[vn.Args_vkGetImageMemoryRequirements](a)
	mr.Device, mr.Image, mr.PMemoryRequirements = d.dev, img.img, unsafe.Pointer(reqs)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements_EXT, d.table, unsafe.Pointer(mr))
	var err error
	if img.mem, err = d.allocate(reqs, memoryDeviceLocal, 0); err != nil {
		d.destroyImage(img)
		return nil, err
	}
	bm := get[vn.Args_vkBindImageMemory](a)
	bm.Device, bm.Image, bm.Memory = d.dev, img.img, img.mem.mem
	vk.Call(vn.VK_COMMAND_TYPE_vkBindImageMemory_EXT, d.table, unsafe.Pointer(bm))
	if err := check("vkBindImageMemory", bm.Ret); err != nil {
		d.destroyImage(img)
		return nil, err
	}
	vi := get[vn.VkImageViewCreateInfo](a)
	vi.SType = vn.VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO
	vi.Image = img.img
	vi.ViewType = imageViewType2D
	vi.Format = format
	vi.SubresourceRange = vn.VkImageSubresourceRange{AspectMask: aspectColor, LevelCount: 1, LayerCount: 1}
	view := get[vn.Handle](a)
	cv := get[vn.Args_vkCreateImageView](a)
	cv.Device, cv.PCreateInfo, cv.PView = d.dev, unsafe.Pointer(vi), unsafe.Pointer(view)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImageView_EXT, d.table, unsafe.Pointer(cv))
	if err := check("vkCreateImageView", cv.Ret); err != nil {
		d.destroyImage(img)
		return nil, err
	}
	img.view = *view
	return img, nil
}

// DestroyImage destroys img, which the GPU no longer uses.
func (d *Device) DestroyImage(img *Image) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	d.destroyImage(img)
}

func (d *Device) destroyImage(img *Image) {
	a := d.a
	if img.view != 0 {
		dv := get[vn.Args_vkDestroyImageView](a)
		dv.Device, dv.ImageView = d.dev, img.view
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImageView_EXT, d.table, unsafe.Pointer(dv))
		img.view = 0
	}
	if img.img != 0 {
		di := get[vn.Args_vkDestroyImage](a)
		di.Device, di.Image = d.dev, img.img
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImage_EXT, d.table, unsafe.Pointer(di))
		img.img = 0
	}
	d.free(img.mem)
	img.mem = allocation{}
}

// Image types, tilings, view types and aspects, of vulkan_core.h.
const (
	imageType2D        = 1
	imageTilingOptimal = 0
	imageViewType2D    = 1

	aspectColor = 0x1
)

// Image layouts, of VkImageLayout.
const (
	ImageLayoutUndefined              = 0
	ImageLayoutGeneral                = 1
	ImageLayoutColorAttachmentOptimal = 2
	ImageLayoutShaderReadOnlyOptimal  = 5
	ImageLayoutTransferSrcOptimal     = 6
	ImageLayoutTransferDstOptimal     = 7
)
