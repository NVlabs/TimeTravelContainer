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

// NewSamplerOf returns a sampler of a Gallium sampler state's, of the
// border color of Vulkan's nearest its own.
func (d *Device) NewSamplerOf(st *SamplerState) (*Sampler, error) {
	return d.NewSamplerOfBorder(st, vkBorderColor(st.BorderColor))
}

// NewSamplerOfBorder returns a sampler of a Gallium sampler state's, of a
// border color of Vulkan's, VkBorderColor.
func (d *Device) NewSamplerOfBorder(st *SamplerState, border uint32) (*Sampler, error) {
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
	// Of none where the device's samplers have none, as a device of the
	// portability subset's may not: OpenGL ES has no samplers' bias, but
	// shaders' (TXB).
	if d.samplerMipLodBias {
		info.MipLodBias = max(min(st.LodBias, limits.MaxSamplerLodBias), -limits.MaxSamplerLodBias)
	}
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
	info.BorderColor = border
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

// Vulkan's border colors, VkBorderColor, of floats; of integers, each
// one more.
const (
	BorderTransparentBlack = 0
	BorderOpaqueBlack      = 2
	BorderOpaqueWhite      = 4
)

// UsesBorder reports whether samplers of st sample borders.
func (st *SamplerState) UsesBorder() bool {
	return st.WrapS == WrapClampToBorder || st.WrapT == WrapClampToBorder || st.WrapR == WrapClampToBorder
}

// Border returns the border color, VkBorderColor, of a sampler of st that
// samples v, and, of a border color of the guest's that Vulkan's are not,
// as v swizzles it, Vulkan's nearest, and the border that a shader's
// translation blends (tospirv.Border), of v's samples of a transparent
// black border, of the type of the color returned (its lowest bit), and of
// an opaque white one, of its slot's BorderTexture.
func (v *SamplerView) Border(st *SamplerState) (uint32, *tospirv.Border) {
	f := v.Format
	integer, _ := f.IsInteger()
	base, one := uint32(BorderTransparentBlack), uint32(0x3f800000)
	if integer || f.kind == kindStencil {
		base, one = BorderTransparentBlack+1, 1
	}
	if !st.UsesBorder() {
		return base, nil
	}
	c := st.BorderColor
	if st.CompareMode && f.IsDepth() {
		// Of the border's depth alone, which shadow samplers compare.
		switch c[0] {
		case 0:
			return base, nil
		case one:
			return base + BorderOpaqueWhite, nil
		}
		return base, &tospirv.Border{Color: c, Compare: st.CompareFunc, ClampRef: unormDepth(f.VK)}
	}
	// Vulkan swizzles the border colors of views that swizzle their
	// channels, but of 0 and 1, which it may not, as it may not swizzle
	// opaque black.
	m := vkComponents(v.Desc.Swizzle, f)
	identity := m == vn.VkComponentMapping{R: 3, G: 4, B: 5, A: 6}
	constants := false
	for _, x := range []uint32{m.R, m.G, m.B, m.A} {
		constants = constants || x == 1 || x == 2
	}
	switch {
	case c == [4]uint32{} && !constants:
		return base, nil
	case c == [4]uint32{one, one, one, one} && !constants:
		return base + BorderOpaqueWhite, nil
	case c == [4]uint32{0, 0, 0, one} && identity:
		return base + BorderOpaqueBlack, nil
	}
	nearest := base
	switch {
	case c == [4]uint32{one, one, one, one}:
		nearest += BorderOpaqueWhite
	case c[3] == one:
		nearest += BorderOpaqueBlack
	}
	b := &tospirv.Border{}
	for i, sw := range v.Desc.Swizzle {
		switch sw {
		case Swizzle0:
		case Swizzle1:
			b.Color[i] = one
		default:
			b.Color[i] = c[sw&3]
		}
	}
	return nearest, b
}

// unormDepth reports whether a Vulkan format's depths are fixed-point,
// whose comparisons' references Vulkan clamps to [0, 1].
func unormDepth(vk uint32) bool {
	switch vk {
	case 124, 125, 128, 129: // D16_UNORM, X8_D24_UNORM_PACK32, D16_UNORM_S8_UINT, D24_UNORM_S8_UINT
		return true
	}
	return false
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
