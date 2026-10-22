// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Sampler is a sampler of the device's.
type Sampler struct {
	sampler vn.Handle
}

// SamplerDesc describes a sampler, of Vulkan's filters, VkFilter, and
// addressing, VkSamplerAddressMode.
type SamplerDesc struct {
	MagFilter, MinFilter uint32
	AddressMode          uint32
}

// NewSampler returns a sampler of desc.
func (d *Device) NewSampler(desc SamplerDesc) (*Sampler, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	info := get[vn.VkSamplerCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_SAMPLER_CREATE_INFO
	info.MagFilter, info.MinFilter = desc.MagFilter, desc.MinFilter
	info.AddressModeU, info.AddressModeV, info.AddressModeW = desc.AddressMode, desc.AddressMode, desc.AddressMode
	info.MaxLod = 1000
	h := get[vn.Handle](a)
	cs := get[vn.Args_vkCreateSampler](a)
	cs.Device, cs.PCreateInfo, cs.PSampler = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateSampler_EXT, d.table, unsafe.Pointer(cs))
	if err := check("vkCreateSampler", cs.Ret); err != nil {
		return nil, err
	}
	return &Sampler{sampler: *h}, nil
}

// DestroySampler destroys s, which the GPU no longer uses.
func (d *Device) DestroySampler(s *Sampler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	ds := get[vn.Args_vkDestroySampler](d.a)
	ds.Device, ds.Sampler = d.dev, s.sampler
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroySampler_EXT, d.table, unsafe.Pointer(ds))
}

// CopyBufferToImage copies b, rows of texels, tightly, to img, in the
// transfer destination layout.
func (c *Commands) CopyBufferToImage(b *Buffer, img *Image) {
	r := get[vn.VkBufferImageCopy](c.a)
	r.ImageSubresource = vn.VkImageSubresourceLayers{AspectMask: aspectColor, LayerCount: 1}
	r.ImageExtent = vn.VkExtent3D{Width: img.width, Height: img.height, Depth: 1}
	ci := get[vn.Args_vkCmdCopyBufferToImage](c.a)
	ci.SrcBuffer, ci.DstImage, ci.DstImageLayout = b.buf, img.img, img.layout
	ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(r)
	c.call(vn.VK_COMMAND_TYPE_vkCmdCopyBufferToImage_EXT, unsafe.Pointer(ci))
}
