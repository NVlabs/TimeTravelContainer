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

// Surface is an attachment of a framebuffer: a level of a texture, of its
// layers from First to Last, as a format of the texture's size class.
type Surface struct {
	Texture            *Texture
	Format             Format
	Level, First, Last uint32
	view               vn.Handle
	// aspects are the view's: color, or depth and stencil.
	aspects uint32
}

// CheckSurface returns nil if t's level and layers, as format, a virgl
// format, are a surface's, as the decoder's pipe.c checks virgl's.
func (t *Texture) CheckSurface(format, level, first, last uint32) error {
	f, ok := LookupFormat(format)
	bad := func() error {
		return fmt.Errorf("a surface of format %d, level %d, layers %d..%d of %+v", format, level, first, last, t.Desc)
	}
	if !ok || !formatViewOK(t.Format, f) || level > t.Desc.LastLevel || first > last {
		return bad()
	}
	// Vulkan views no 3D image's slices as layers, but of images that
	// their creation let.
	if t.Desc.Target == Target3D || last >= t.Desc.ArraySize {
		return bad()
	}
	return nil
}

// NewSurface returns a surface of t's level and layers, as format, a virgl
// format, of which the device renders.
func (d *Device) NewSurface(t *Texture, format, level, first, last uint32) (*Surface, error) {
	if err := t.CheckSurface(format, level, first, last); err != nil {
		return nil, err
	}
	f, _ := LookupFormat(format)
	want, feature := uint32(ImageColorAttachment), uint32(featureColorAttachment)
	if t.aspects != aspectColor {
		want, feature = ImageDepthAttachment, featureDepthStencil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	if t.usage&want == 0 || d.formatFeatures(f.VK)&feature == 0 {
		return nil, fmt.Errorf("a surface of %s, which %s renders not, of %+v", f.Name, d.Name, t.Desc)
	}
	s := &Surface{Texture: t, Format: f, Level: level, First: first, Last: last, aspects: t.aspects}
	viewType := uint32(imageViewType2D)
	switch {
	case t.Desc.Target == Target1D || t.Desc.Target == Target1DArray:
		viewType = 0 // VK_IMAGE_VIEW_TYPE_1D
		if last > first {
			viewType = 4 // VK_IMAGE_VIEW_TYPE_1D_ARRAY
		}
	case last > first:
		viewType = 5 // VK_IMAGE_VIEW_TYPE_2D_ARRAY
	}
	usage := get[vn.VkImageViewUsageCreateInfo](a)
	usage.SType = vn.VK_STRUCTURE_TYPE_IMAGE_VIEW_USAGE_CREATE_INFO
	usage.Usage = want
	info := get[vn.VkImageViewCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO
	info.PNext = unsafe.Pointer(usage)
	info.Image = t.img
	info.ViewType = viewType
	info.Format = f.VK
	info.SubresourceRange = vn.VkImageSubresourceRange{
		AspectMask:     s.aspects,
		BaseMipLevel:   level,
		LevelCount:     1,
		BaseArrayLayer: first,
		LayerCount:     last - first + 1,
	}
	h := get[vn.Handle](a)
	cv := get[vn.Args_vkCreateImageView](a)
	cv.Device, cv.PCreateInfo, cv.PView = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImageView_EXT, d.table, unsafe.Pointer(cv))
	if err := check("vkCreateImageView", cv.Ret); err != nil {
		return nil, fmt.Errorf("a surface of level %d, layers %d..%d of %+v: %w", level, first, last, t.Desc, err)
	}
	s.view = *h
	return s, nil
}

// DestroySurface destroys s, which the GPU no longer uses.
func (d *Device) DestroySurface(s *Surface) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	dv := get[vn.Args_vkDestroyImageView](d.a)
	dv.Device, dv.ImageView = d.dev, s.view
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImageView_EXT, d.table, unsafe.Pointer(dv))
}

// extent returns the size of s's level.
func (s *Surface) extent() (width, height uint32) {
	w, h, _ := s.Texture.Desc.levelExtent(s.Level)
	return uint32(w), uint32(h)
}
