// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// CopyBuffer copies size bytes of src from srcOffset to dst at dstOffset,
// in order of the context's commands.
func (c *Context) CopyBuffer(dst *Buffer, dstOffset uint64, src *Buffer, srcOffset, size uint64) error {
	if srcOffset > src.size || size > src.size-srcOffset || dstOffset > dst.size || size > dst.size-dstOffset {
		return fmt.Errorf("a copy of %d bytes from %d of %d to %d of %d", size, srcOffset, src.size, dstOffset, dst.size)
	}
	if size == 0 {
		return nil
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	region := get[vn.VkBufferCopy](c.a)
	*region = vn.VkBufferCopy{SrcOffset: srcOffset, DstOffset: dstOffset, Size: size}
	cmds.barrier()
	cb := get[vn.Args_vkCmdCopyBuffer](c.a)
	cb.SrcBuffer, cb.DstBuffer, cb.RegionCount, cb.PRegions = src.buf, dst.buf, 1, unsafe.Pointer(region)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBuffer_EXT, unsafe.Pointer(cb))
	cmds.barrier()
	return nil
}

// subresource returns the Vulkan subresource and offset of t's box of a
// level at x, y and z, of depth layers or slices, as Gallium's boxes
// address them: an array's or a cube's layers are its z.
func (t *Texture) subresource(level uint32, x, y, z, depth int64) (vn.VkImageSubresourceLayers, vn.VkOffset3D) {
	s := vn.VkImageSubresourceLayers{AspectMask: t.aspects, MipLevel: level, LayerCount: 1}
	o := vn.VkOffset3D{X: int32(x), Y: int32(y)}
	switch t.Desc.Target {
	case Target1DArray, Target2DArray, TargetCube, TargetCubeArray:
		s.BaseArrayLayer, s.LayerCount = uint32(z), uint32(depth)
	case Target3D:
		o.Z = int32(z)
	}
	return s, o
}

// vkImageType returns the type of the Vulkan images of a target's
// textures.
func vkImageType(target uint32) uint32 {
	switch target {
	case Target1D, Target1DArray:
		return imageType1D
	case Target3D:
		return imageType3D
	}
	return imageType2D
}

// CheckCopy returns nil if a copy of src's box of level srcLevel, of its
// texels, to dst's level dstLevel at x, y and z is valid, as the decoder's
// pipe.c checks virgl's: of textures of formats of blocks of the same
// size, of one plane, of the same samples. A copy between a compressed
// format and a plain one copies a block of one as a texel of the other: of
// the destination, it covers the blocks that the box touches. It returns
// the destination's box.
func CheckCopy(dst *Texture, dstLevel uint32, x, y, z int64, src *Texture, srcLevel uint32, box Box) (Box, error) {
	sf, df := src.Format, dst.Format
	to := Box{X: x, Y: y, Z: z, Width: box.Width, Height: box.Height, Depth: box.Depth}
	bad := func() (Box, error) {
		return to, fmt.Errorf("a copy of %+v of level %d of %+v to %+v of level %d of %+v", box, srcLevel, src.Desc,
			to, dstLevel, dst.Desc)
	}
	if box.Width < 0 || box.Height < 0 || box.Depth < 0 || !src.Desc.boxOK(srcLevel, box) {
		return bad()
	}
	if sf.BlockWidth != df.BlockWidth || sf.BlockHeight != df.BlockHeight {
		if sf.Compressed && df.Compressed {
			return bad()
		}
		to.Width = (box.Width + int64(sf.BlockWidth) - 1) / int64(sf.BlockWidth) * int64(df.BlockWidth)
		to.Height = (box.Height + int64(sf.BlockHeight) - 1) / int64(sf.BlockHeight) * int64(df.BlockHeight)
	}
	if !dst.Desc.boxOK(dstLevel, to) || sf.BlockBytes != df.BlockBytes || dst.Desc.Samples != src.Desc.Samples {
		return bad()
	}
	// What Vulkan copies: depth and stencil of the same format, images of
	// one type, or 2D ones and 3D ones, and boxes of whole blocks, but at
	// a level's edges.
	if src.aspects != dst.aspects || src.aspects != aspectColor && sf.VK != df.VK {
		return bad()
	}
	st, dt := vkImageType(src.Desc.Target), vkImageType(dst.Desc.Target)
	if st != dt && (st == imageType1D || dt == imageType1D) {
		return bad()
	}
	if !src.blocksOK(srcLevel, box) || !dst.blocksOK(dstLevel, to) {
		return bad()
	}
	// Of a level to itself: Vulkan copies no regions that overlap, but of
	// a box to itself, which copies nothing.
	overlap := func(a, n, b, m int64) bool { return a < b+m && b < a+n }
	if src == dst && srcLevel == dstLevel && (x != box.X || y != box.Y || z != box.Z) &&
		overlap(x, to.Width, box.X, box.Width) && overlap(y, to.Height, box.Y, box.Height) && overlap(z, to.Depth, box.Z, box.Depth) {
		return bad()
	}
	return to, nil
}

// CopyTexture copies src's box of level srcLevel, of its texels, to dst's
// level dstLevel at x, y and z, in order of the context's commands, as
// CheckCopy checks it.
func (c *Context) CopyTexture(dst *Texture, dstLevel uint32, x, y, z int64, src *Texture, srcLevel uint32, box Box) error {
	if _, err := CheckCopy(dst, dstLevel, x, y, z, src, srcLevel, box); err != nil {
		return err
	}
	if box.Width == 0 || box.Height == 0 || box.Depth == 0 || dst == src && dstLevel == srcLevel && x == box.X && y == box.Y && z == box.Z {
		return nil
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	region := get[vn.VkImageCopy](c.a)
	region.SrcSubresource, region.SrcOffset = src.subresource(srcLevel, box.X, box.Y, box.Z, box.Depth)
	region.DstSubresource, region.DstOffset = dst.subresource(dstLevel, x, y, z, box.Depth)
	// Of the source's texels: of 3D images, slices, which Vulkan copies as
	// the other's layers.
	region.Extent = vn.VkExtent3D{Width: uint32(box.Width), Height: uint32(box.Height), Depth: 1}
	if src.Desc.Target == Target3D || dst.Desc.Target == Target3D {
		region.Extent.Depth = uint32(box.Depth)
	}
	c.general(cmds, src)
	c.general(cmds, dst)
	cmds.barrier()
	ci := get[vn.Args_vkCmdCopyImage](c.a)
	ci.SrcImage, ci.SrcImageLayout, ci.DstImage, ci.DstImageLayout = src.img, ImageLayoutGeneral, dst.img, ImageLayoutGeneral
	ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(region)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyImage_EXT, unsafe.Pointer(ci))
	cmds.barrier()
	return nil
}

// CopyTextureAll copies every level and layer of src to dst, a texture of
// src's parameters, in order of the context's commands.
func (c *Context) CopyTextureAll(dst, src *Texture) error {
	if dst.Desc != src.Desc {
		return fmt.Errorf("a copy of %+v to %+v", src.Desc, dst.Desc)
	}
	for level := uint32(0); level <= src.Desc.LastLevel; level++ {
		w, h, depth := src.Desc.levelExtent(level)
		if err := c.CopyTexture(dst, level, 0, 0, 0, src, level, Box{Width: w, Height: h, Depth: depth}); err != nil {
			return err
		}
	}
	return nil
}
