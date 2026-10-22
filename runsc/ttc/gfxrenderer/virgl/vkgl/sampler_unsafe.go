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

// NewSamplerOf returns a sampler of a Gallium sampler state's.
func (d *Device) NewSamplerOf(st *SamplerState) (*Sampler, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	limits := &d.props.Limits
	info := get[vn.VkSamplerCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_SAMPLER_CREATE_INFO
	// Gallium's filters, PIPE_TEX_FILTER_*, are Vulkan's, VkFilter.
	info.MagFilter, info.MinFilter = st.MagFilter, st.MinFilter
	info.AddressModeU, info.AddressModeV, info.AddressModeW = VkAddressMode(st.WrapS), VkAddressMode(st.WrapT), VkAddressMode(st.WrapR)
	info.MipLodBias = max(min(st.LodBias, limits.MaxSamplerLodBias), -limits.MaxSamplerLodBias)
	info.MinLod, info.MaxLod = st.MinLod, st.MaxLod
	switch st.MipFilter {
	case MipFilterLinear:
		info.MipmapMode = 1 // VK_SAMPLER_MIPMAP_MODE_LINEAR
	case MipFilterNone:
		// Of the base level only, as Zink samples it.
		info.MinLod, info.MaxLod = 0, 0.25
	}
	if st.MaxAnisotropy > 1 && d.features10.SamplerAnisotropy != 0 {
		info.AnisotropyEnable = 1
		info.MaxAnisotropy = min(float32(st.MaxAnisotropy), limits.MaxSamplerAnisotropy)
	}
	if st.CompareMode {
		// Gallium's functions, PIPE_FUNC_*, are Vulkan's, VkCompareOp.
		info.CompareEnable, info.CompareOp = 1, st.CompareFunc&7
	}
	info.BorderColor = vkBorderColor(st.BorderColor)
	h := get[vn.Handle](a)
	cs := get[vn.Args_vkCreateSampler](a)
	cs.Device, cs.PCreateInfo, cs.PSampler = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateSampler_EXT, d.table, unsafe.Pointer(cs))
	if err := check("vkCreateSampler", cs.Ret); err != nil {
		return nil, err
	}
	return &Sampler{sampler: *h}, nil
}

// vkBorderColor returns Vulkan's border color, VkBorderColor, that is
// nearest a Gallium border color's bits, as floats: transparent black,
// opaque black, or opaque white.
func vkBorderColor(c [4]uint32) uint32 {
	const one = 0x3f800000
	switch {
	case c[0] == one && c[1] == one && c[2] == one && c[3] == one:
		return 4 // VK_BORDER_COLOR_FLOAT_OPAQUE_WHITE
	case c[3] == one:
		return 2 // VK_BORDER_COLOR_FLOAT_OPAQUE_BLACK
	}
	return 0 // VK_BORDER_COLOR_FLOAT_TRANSPARENT_BLACK
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
