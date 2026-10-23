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

// Format features, of VkFormatFeatureFlagBits, and image creation flags,
// of VkImageCreateFlagBits.
const (
	featureSampledImage     = 0x1
	featureStorageImage     = 0x2
	featureColorAttachment  = 0x80
	featureDepthStencil     = 0x200
	featureTransferSrc      = 0x4000
	featureTransferDst      = 0x8000
	createMutableFormat     = 0x8
	createCubeCompatible    = 0x10
	create2DArrayCompatible = 0x20
	create2DViewCompatible  = 0x20000
	createExtendedUsage     = 0x100
	imageUsageDepthStencil  = 0x20
	imageType1D             = 0
	imageType3D             = 2
	aspectDepth             = 0x2
	aspectStencil           = 0x4
)

// formatFeatures returns the features of a format's optimally tiled
// images, which the device keeps.
func (d *Device) formatFeatures(format uint32) uint32 {
	return d.formatProperties(format).OptimalTilingFeatures
}

// lockedFormatFeatures is formatFeatures, of a caller that does not hold
// d.mu.
func (d *Device) lockedFormatFeatures(format uint32) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	return d.formatFeatures(format)
}

// formatProperties returns a format's properties, which the device keeps,
// of a caller that holds d.mu.
func (d *Device) formatProperties(format uint32) vn.VkFormatProperties {
	if f, ok := d.features[format]; ok {
		return f
	}
	a := d.a
	props := get[vn.VkFormatProperties](a)
	fp := get[vn.Args_vkGetPhysicalDeviceFormatProperties](a)
	fp.PhysicalDevice, fp.Format, fp.PFormatProperties = d.pd, format, unsafe.Pointer(props)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFormatProperties_EXT, d.pdTable, unsafe.Pointer(fp))
	if d.features == nil {
		d.features = make(map[uint32]vn.VkFormatProperties)
	}
	d.features[format] = *props
	return *props
}

// Texture is a texture resource: a Vulkan image of a ResourceDesc's, of
// every usage that its format has, viewable as the formats of its size
// class.
type Texture struct {
	Desc   ResourceDesc
	Format Format
	img    vn.Handle
	mem    allocation
	// aspects are the image's aspects, of VkImageAspectFlagBits.
	aspects uint32
	// layout is the layout of the image's subresources, all of which the
	// renderer keeps in one.
	layout uint32
	// usage is the image's, of VkImageUsageFlagBits.
	usage uint32
	// floatDepth24 is whether the image's depths are 32-bit floats, the
	// substitute of the format's 24-bit depths, of which the guest's
	// memory has its texels.
	floatDepth24 bool
	// slices2D is whether surfaces view a 3D image's slices as layers, of
	// which the image is compatible, and slices2DImages whether images
	// view them as 2D images.
	slices2D, slices2DImages bool
}

// NewTexture returns a texture of desc, a texture's, of a format that
// Vulkan has or emulates, in the general layout, in which contexts keep
// textures: none changes a layout in a rendering instance.
func (d *Device) NewTexture(desc ResourceDesc) (*Texture, error) {
	t, err := d.newTexture(desc)
	if err != nil {
		return nil, err
	}
	if err := d.Record(func(c *Commands) error {
		c.general(t)
		return nil
	}); err != nil {
		d.DestroyTexture(t)
		return nil, err
	}
	return t, nil
}

func (d *Device) newTexture(desc ResourceDesc) (*Texture, error) {
	desc = desc.normalize()
	if desc.Target == TargetBuffer {
		return nil, fmt.Errorf("a texture of a buffer's %+v", desc)
	}
	if err := desc.check(d.limits()); err != nil {
		return nil, err
	}
	f, ok := d.LookupFormat(desc.Format)
	if !ok || IsViewFormat(desc.Format) {
		return nil, fmt.Errorf("a texture of format %d, which Vulkan lacks", desc.Format)
	}
	if desc.Samples > 1 && desc.Target == Target2DArray && !d.multisampleArrays {
		return nil, fmt.Errorf("a multisampled array %+v, which %s has none of", desc, d.Name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.a
	defer a.reset()
	features := d.formatFeatures(f.VK)
	var usage uint32
	for _, u := range []struct{ feature, usage uint32 }{
		{featureSampledImage, ImageSampled},
		{featureStorageImage, ImageStorage},
		{featureColorAttachment, ImageColorAttachment},
		{featureDepthStencil, imageUsageDepthStencil},
		{featureTransferSrc, ImageTransferSrc},
		{featureTransferDst, ImageTransferDst},
	} {
		if features&u.feature != 0 {
			usage |= u.usage
		}
	}
	if usage&(ImageTransferSrc|ImageTransferDst) != ImageTransferSrc|ImageTransferDst {
		return nil, fmt.Errorf("%s's images cannot be copied", f.Name)
	}
	if desc.Samples > 1 {
		// Of the usages of multisampled images.
		usage &^= ImageStorage
	}
	info := get[vn.VkImageCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO
	info.Flags = createMutableFormat | createExtendedUsage
	info.ImageType = imageType2D
	info.Extent = vn.VkExtent3D{Width: desc.Width, Height: desc.Height, Depth: 1}
	info.ArrayLayers = desc.ArraySize
	switch desc.Target {
	case Target1D, Target1DArray:
		info.ImageType = imageType1D
	case Target3D:
		info.ImageType = imageType3D
		info.Extent.Depth = desc.Depth
		if d.view2DOn3D {
			// Of surfaces of its slices, as OpenGL ES 3.0 renders to.
			info.Flags |= create2DArrayCompatible
		}
		if d.imageViews2DOf3D && usage&ImageStorage != 0 {
			// Of images of its slices, as OpenGL ES 3.1 binds them.
			info.Flags |= create2DViewCompatible
		}
	case TargetCube, TargetCubeArray:
		info.Flags |= createCubeCompatible
	case Target2DArray:
		if desc.ArraySize >= 6 && desc.Width == desc.Height && desc.Samples <= 1 {
			// Of views of cubes, as GL's texture views may have.
			info.Flags |= createCubeCompatible
		}
	}
	info.Format = f.VK
	info.MipLevels = desc.LastLevel + 1
	info.Samples = max(desc.Samples, 1)
	info.Tiling = imageTilingOptimal
	info.Usage = usage
	h := get[vn.Handle](a)
	ci := get[vn.Args_vkCreateImage](a)
	ci.Device, ci.PCreateInfo, ci.PImage = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImage_EXT, d.table, unsafe.Pointer(ci))
	if err := check("vkCreateImage", ci.Ret); err != nil {
		return nil, fmt.Errorf("a texture of %+v, %s: %w", desc, f.Name, err)
	}
	t := &Texture{Desc: desc, Format: f, img: *h, layout: ImageLayoutUndefined, aspects: aspectColor, usage: usage,
		floatDepth24: desc.Format == virglZ24X8 && f.VK == vkD32, slices2D: info.Flags&create2DArrayCompatible != 0,
		slices2DImages: info.Flags&create2DViewCompatible != 0}
	switch {
	case f.IsDepth() && f.IsStencil():
		t.aspects = aspectDepth | aspectStencil
	case f.IsDepth():
		t.aspects = aspectDepth
	case f.IsStencil():
		t.aspects = aspectStencil
	}
	reqs := get[vn.VkMemoryRequirements](a)
	mr := get[vn.Args_vkGetImageMemoryRequirements](a)
	mr.Device, mr.Image, mr.PMemoryRequirements = d.dev, t.img, unsafe.Pointer(reqs)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements_EXT, d.table, unsafe.Pointer(mr))
	var err error
	if t.mem, err = d.allocate(reqs, memoryDeviceLocal, 0); err != nil {
		d.destroyTexture(t)
		return nil, err
	}
	bm := get[vn.Args_vkBindImageMemory](a)
	bm.Device, bm.Image, bm.Memory = d.dev, t.img, t.mem.mem
	vk.Call(vn.VK_COMMAND_TYPE_vkBindImageMemory_EXT, d.table, unsafe.Pointer(bm))
	if err := check("vkBindImageMemory", bm.Ret); err != nil {
		d.destroyTexture(t)
		return nil, err
	}
	return t, nil
}

// DestroyTexture destroys t, which the GPU no longer uses.
func (d *Device) DestroyTexture(t *Texture) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	d.destroyTexture(t)
}

func (d *Device) destroyTexture(t *Texture) {
	if t.img != 0 {
		di := get[vn.Args_vkDestroyImage](d.a)
		di.Device, di.Image = d.dev, t.img
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImage_EXT, d.table, unsafe.Pointer(di))
		t.img = 0
	}
	d.free(t.mem)
	t.mem = allocation{}
}

// limits returns the device's limits of resources.
func (d *Device) limits() Limits {
	l := &d.props.Limits
	levels := func(size uint32) uint32 {
		n := uint32(0)
		for size > 0 {
			n++
			size >>= 1
		}
		return n
	}
	return Limits{
		Max2DSize:      l.MaxImageDimension2D,
		MaxCubeLevels:  levels(l.MaxImageDimensionCube),
		Max3DLevels:    levels(l.MaxImageDimension3D),
		MaxArrayLayers: l.MaxImageArrayLayers,
	}
}
