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

// Gallium's swizzles, of pipe_swizzle.
const (
	SwizzleX = iota
	SwizzleY
	SwizzleZ
	SwizzleW
	Swizzle0
	Swizzle1
)

// viewClasses are the classes of targets that views of a target's
// resources may be of, as GL's texture views may.
var viewClasses = [numTargets]int{
	TargetBuffer: 1, Target1D: 2, Target1DArray: 2, Target2D: 3, Target2DArray: 3, TargetCube: 3,
	TargetCubeArray: 3, TargetRect: 4, Target3D: 5,
}

// ViewDesc is a sampler view's: a view of a texture's levels and layers,
// or of a buffer's elements, of a format and a target, and of a swizzle of
// the format's channels, as virgl's sampler views encode them.
type ViewDesc struct {
	Format     uint32
	Target     uint32
	FirstLayer uint32
	LastLayer  uint32
	FirstLevel uint32
	LastLevel  uint32
	Swizzle    [4]uint32
}

// DecodeViewDesc decodes a sampler view's target, val0, val1 and swizzle,
// as virgl's command has them, of a texture's.
func DecodeViewDesc(format, target, val0, val1, swizzle uint32) ViewDesc {
	return ViewDesc{
		Format:     format,
		Target:     target,
		FirstLayer: val0 & 0xffff,
		LastLayer:  val0 >> 16 & 0xffff,
		FirstLevel: val1 & 0xff,
		LastLevel:  val1 >> 8 & 0xff,
		Swizzle:    [4]uint32{swizzle & 7, swizzle >> 3 & 7, swizzle >> 6 & 7, swizzle >> 9 & 7},
	}
}

// formatViewOK reports whether a texture of format f may be viewed as
// format v: of the same size class, and both colors or both depths, as
// GL's texture views and Vulkan require.
func formatViewOK(f, v Format) bool {
	return f.BlockBytes == v.BlockBytes && f.BlockWidth == v.BlockWidth && f.BlockHeight == v.BlockHeight &&
		(f.IsDepth() || f.IsStencil()) == (v.IsDepth() || v.IsStencil())
}

// check returns an error unless v is a view of t, as the decoder's pipe.c
// checks virgl's.
func (v *ViewDesc) check(t *Texture) error {
	bad := func() error { return fmt.Errorf("a view %+v of %+v", *v, t.Desc) }
	f, ok := LookupFormat(v.Format)
	if !ok || !formatViewOK(t.Format, f) {
		return bad()
	}
	if v.Target == 0 {
		// A guest without texture views names no target.
		v.Target = t.Desc.Target
	}
	for _, s := range v.Swizzle {
		if s > Swizzle1 {
			return bad()
		}
	}
	if v.Target >= numTargets || viewClasses[v.Target] != viewClasses[t.Desc.Target] {
		return bad()
	}
	if v.FirstLevel > v.LastLevel || v.LastLevel > t.Desc.LastLevel || v.FirstLayer > v.LastLayer {
		return bad()
	}
	layers := t.Desc.ArraySize
	if t.Desc.Target == Target3D {
		layers = 1
	}
	switch {
	case v.LastLayer >= layers:
		return bad()
	case v.Target == TargetCube && v.FirstLayer+6 > layers:
		return bad()
	case v.Target == TargetCubeArray && (v.LastLayer-v.FirstLayer+1)%6 != 0:
		return bad()
	}
	return nil
}

// vkComponents returns the view's components, VkComponentSwizzle, of the
// guest's swizzle of the view format's channels, which f's emulation
// swizzles of the Vulkan format's.
func vkComponents(swizzle [4]uint32, f Format) vn.VkComponentMapping {
	var c [4]uint32
	for i, s := range swizzle {
		switch s {
		case Swizzle0:
			c[i] = 1 // VK_COMPONENT_SWIZZLE_ZERO
		case Swizzle1:
			c[i] = 2 // VK_COMPONENT_SWIZZLE_ONE
		default:
			switch e := f.Swizzle[s]; e {
			case '0':
				c[i] = 1
			case '1':
				c[i] = 2
			default:
				// VK_COMPONENT_SWIZZLE_R and on.
				c[i] = 3 + uint32(e-'X')
				if e == 'W' {
					c[i] = 6
				}
			}
		}
	}
	return vn.VkComponentMapping{R: c[0], G: c[1], B: c[2], A: c[3]}
}

// vkViewType returns the view type, VkImageViewType, of a target.
func vkViewType(target uint32) uint32 {
	switch target {
	case Target1D:
		return 0
	case Target3D:
		return 2
	case TargetCube:
		return 3
	case Target1DArray:
		return 4
	case Target2DArray:
		return 5
	case TargetCubeArray:
		return 6
	}
	return 1
}

// SamplerView is a view of a texture that shaders sample.
type SamplerView struct {
	Desc   ViewDesc
	Format Format
	tex    *Texture
	view   vn.Handle
}

// NewSamplerView returns a view of t of desc.
func (d *Device) NewSamplerView(t *Texture, desc ViewDesc) (*SamplerView, error) {
	if err := desc.check(t); err != nil {
		return nil, err
	}
	f, _ := d.LookupFormat(desc.Format)
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	// Of formats that the device samples, of images of their usage.
	if t.usage&ImageSampled == 0 || d.formatFeatures(f.VK)&featureSampledImage == 0 {
		return nil, fmt.Errorf("a view of %s, which %s samples not, of %+v", f.Name, d.Name, t.Desc)
	}
	aspects := t.aspects
	switch {
	case f.IsDepth():
		// Of the depth that shaders sample, rather than the stencil.
		aspects = aspectDepth
	case f.IsStencil():
		aspects = aspectStencil
	}
	h, err := d.newView(t, desc, f, aspects, ImageSampled, vkComponents(desc.Swizzle, f))
	if err != nil {
		return nil, err
	}
	return &SamplerView{Desc: desc, Format: f, tex: t, view: h}, nil
}

// newView returns a view of t of desc, of format f, of its aspects, for
// usage, of components, of a caller that holds d.mu.
func (d *Device) newView(t *Texture, desc ViewDesc, f Format, aspects, usage uint32, components vn.VkComponentMapping) (vn.Handle, error) {
	a := d.a
	u := get[vn.VkImageViewUsageCreateInfo](a)
	u.SType = vn.VK_STRUCTURE_TYPE_IMAGE_VIEW_USAGE_CREATE_INFO
	u.Usage = usage
	info := get[vn.VkImageViewCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO
	info.PNext = unsafe.Pointer(u)
	info.Image = t.img
	info.ViewType = vkViewType(desc.Target)
	info.Format = f.VK
	info.Components = components
	info.SubresourceRange = vn.VkImageSubresourceRange{
		AspectMask:     aspects,
		BaseMipLevel:   desc.FirstLevel,
		LevelCount:     desc.LastLevel - desc.FirstLevel + 1,
		BaseArrayLayer: desc.FirstLayer,
		LayerCount:     desc.LastLayer - desc.FirstLayer + 1,
	}
	if t.Desc.Target == Target3D && desc.Target == Target3D {
		info.SubresourceRange.BaseArrayLayer, info.SubresourceRange.LayerCount = 0, 1
	}
	if desc.Target == TargetCube {
		info.SubresourceRange.LayerCount = 6
	}
	h := get[vn.Handle](a)
	cv := get[vn.Args_vkCreateImageView](a)
	cv.Device, cv.PCreateInfo, cv.PView = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImageView_EXT, d.table, unsafe.Pointer(cv))
	if err := check("vkCreateImageView", cv.Ret); err != nil {
		return 0, fmt.Errorf("a view %+v of %+v: %w", desc, t.Desc, err)
	}
	return *h, nil
}

// DestroySamplerView destroys v, which the GPU no longer uses.
func (d *Device) DestroySamplerView(v *SamplerView) {
	d.destroyView(v.view)
}

func (d *Device) destroyView(h vn.Handle) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	dv := get[vn.Args_vkDestroyImageView](d.a)
	dv.Device, dv.ImageView = d.dev, h
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImageView_EXT, d.table, unsafe.Pointer(dv))
}

// ImageView is a view of a texture's level that shaders load and store,
// as a shader image.
type ImageView struct {
	Desc   ViewDesc
	Format Format
	tex    *Texture
	view   vn.Handle
}

// NewImageView returns a view of t of desc, of one level, which shaders
// load and store texels of, of the format's channels, which it swizzles
// none of: of a format that the device stores, of Vulkan's own, of an image
// of storage.
func (d *Device) NewImageView(t *Texture, desc ViewDesc) (*ImageView, error) {
	desc.Swizzle = [4]uint32{SwizzleX, SwizzleY, SwizzleZ, SwizzleW}
	if t.Desc.Target == Target3D && desc.Target == Target2D {
		// A slice of a 3D texture, as a 2D image, of a texture whose
		// creation let.
		_, _, depth := t.Desc.levelExtent(desc.FirstLevel)
		f, ok := LookupFormat(desc.Format)
		if !t.slices2DImages || !ok || !formatViewOK(t.Format, f) || desc.FirstLevel > t.Desc.LastLevel ||
			desc.FirstLayer != desc.LastLayer || int64(desc.FirstLayer) >= depth {
			return nil, fmt.Errorf("an image view %+v of %+v", desc, t.Desc)
		}
	} else if err := desc.check(t); err != nil {
		return nil, err
	}
	f, _ := d.LookupFormat(desc.Format)
	if desc.FirstLevel != desc.LastLevel || f.Swizzle != "XYZW" || f.IsDepth() || f.IsStencil() {
		return nil, fmt.Errorf("an image view %+v of %s", desc, f.Name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	if t.usage&ImageStorage == 0 || d.formatFeatures(f.VK)&featureStorageImage == 0 {
		return nil, fmt.Errorf("an image view of %s, which %s stores not, of %+v", f.Name, d.Name, t.Desc)
	}
	h, err := d.newView(t, desc, f, aspectColor, ImageStorage, vn.VkComponentMapping{})
	if err != nil {
		return nil, err
	}
	return &ImageView{Desc: desc, Format: f, tex: t, view: h}, nil
}

// DestroyImageView destroys v, which the GPU no longer uses.
func (d *Device) DestroyImageView(v *ImageView) {
	d.destroyView(v.view)
}
