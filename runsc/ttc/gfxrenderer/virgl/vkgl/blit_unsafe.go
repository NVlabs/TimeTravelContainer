// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"errors"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Gallium's masks of a blit's channels, PIPE_MASK_*.
const (
	MaskRGBA = 0xf
	MaskZ    = 0x10
	MaskS    = 0x20
)

// The features of formats that blits use, of VkFormatFeatureFlagBits.
const (
	featureBlitSrc      = 0x400
	featureBlitDst      = 0x800
	featureFilterLinear = 0x1000
)

// Blit is Gallium's blit, of pipe_blit_info: of Src's box of level
// SrcLevel, as SrcFormat, a virgl format, to Dst's of DstLevel, as
// DstFormat, of the channels of Mask, of a linear filter if Linear, and of
// the pixels of Scissor, if any. A box's negative width or height flips
// it.
type Blit struct {
	Dst, Src             *Texture
	DstLevel, SrcLevel   uint32
	DstFormat, SrcFormat uint32
	DstBox, SrcBox       Box
	Mask                 uint32
	Linear               bool
	Scissor              *Scissor
}

// ErrNotNative is the error of a blit that Vulkan's blits, resolves and
// copies do not do, nor the blitter: of multisampled sources of formats
// or channels that are not theirs, of stencil of formats of their own, or
// of 3D destinations' slices.
var ErrNotNative = errors.New("a blit that the device does not do")

// check returns an error unless b is a blit, as the decoder's pipe.c
// checks virgl's: of a source's box of the source, and a destination's of
// its layers, but of rows and columns past its edges, as Gallium's may be,
// which Mesa's state tracker keeps of scaled blits that it clips, of their
// scissors.
func (b *Blit) check() error {
	bad := func() error { return fmt.Errorf("a blit %+v", *b) }
	df, dok := LookupFormat(b.DstFormat)
	sf, sok := LookupFormat(b.SrcFormat)
	if !dok || !sok || !formatViewOK(b.Dst.Format, df) || !formatViewOK(b.Src.Format, sf) ||
		!b.Dst.Desc.layersOK(b.DstLevel, b.DstBox) || !b.Src.Desc.boxOK(b.SrcLevel, b.SrcBox) {
		return bad()
	}
	zs, color := b.Mask&(MaskZ|MaskS) != 0, b.Mask&MaskRGBA != 0
	isZS := func(f Format) bool { return f.IsDepth() || f.IsStencil() }
	if b.Mask == 0 || b.Mask&^(MaskRGBA|MaskZ|MaskS) != 0 || zs == color || zs && (!isZS(df) || !isZS(sf)) ||
		color && (isZS(df) || isZS(sf)) {
		return bad()
	}
	return nil
}

// aspects returns the aspects of the blit's mask, and whether it is of all
// of a color's channels, or of aspects that both textures have.
func (b *Blit) aspects() (uint32, bool) {
	if b.Mask&MaskRGBA != 0 {
		return aspectColor, b.Mask&MaskRGBA == MaskRGBA
	}
	var aspects uint32
	if b.Mask&MaskZ != 0 {
		aspects |= aspectDepth
	}
	if b.Mask&MaskS != 0 {
		aspects |= aspectStencil
	}
	return aspects, b.Dst.aspects&aspects == aspects && b.Src.aspects&aspects == aspects
}

// span returns the start and end of a span of n from x, which a negative
// n flips.
func span(x, n int64) (int32, int32) {
	return int32(x), int32(x + n)
}

// Blit blits b, in order of the context's commands, of Vulkan's blits,
// resolves or copies, or else of the blitter's draws: it returns
// ErrNotNative of blits that neither does.
func (c *Context) Blit(b Blit) error {
	if err := b.check(); err != nil {
		return err
	}
	ds, ss := b.DstBox, b.SrcBox
	if ds.Width == 0 || ds.Height == 0 || ds.Depth == 0 || ss.Width == 0 || ss.Height == 0 || ss.Depth == 0 {
		return nil
	}
	if err := c.nativeBlit(b); err != ErrNotNative {
		return err
	}
	return c.shaderBlit(b, b.Scissor)
}

// nativeBlit blits b, a checked blit of a box of texels, of Vulkan's blits,
// resolves or copies, or returns ErrNotNative of blits that they do not do.
func (c *Context) nativeBlit(b Blit) error {
	// Of a destination's box within it, of no scissor.
	if b.Scissor != nil || !b.Dst.Desc.boxOK(b.DstLevel, b.DstBox) {
		return ErrNotNative
	}
	d := c.d
	df, _ := d.LookupFormat(b.DstFormat)
	sf, _ := d.LookupFormat(b.SrcFormat)
	aspects, ok := b.aspects()
	// Of the textures' formats, but of channels that the destination
	// ignores, and of the channels that the formats have.
	if !ok || df.VK != b.Dst.Format.VK || sf.VK != b.Src.Format.VK || sf.Swizzle != "XYZW" && sf.Swizzle != df.Swizzle {
		return ErrNotNative
	}
	// Of depth and stencil, and of samples, of one format.
	srcSamples, dstSamples := max(b.Src.Desc.Samples, 1), max(b.Dst.Desc.Samples, 1)
	if (aspects != aspectColor || srcSamples > 1 || dstSamples > 1) && df.VK != sf.VK {
		return ErrNotNative
	}
	ds, ss := b.DstBox, b.SrcBox
	dst3D, src3D := b.Dst.Desc.Target == Target3D, b.Src.Desc.Target == Target3D
	if dst3D != src3D || !dst3D && ds.Depth != ss.Depth || ds.Depth < 0 || ss.Depth < 0 {
		// Of layers that Vulkan scales, or flips, not.
		return ErrNotNative
	}
	dsub, _ := b.Dst.subresource(b.DstLevel, 0, 0, ds.Z, ds.Depth)
	ssub, _ := b.Src.subresource(b.SrcLevel, 0, 0, ss.Z, ss.Depth)
	dsub.AspectMask, ssub.AspectMask = aspects, aspects
	scaled := ds.Width != ss.Width || ds.Height != ss.Height || ds.Depth != ss.Depth
	flipped := ds.Width < 0 || ds.Height < 0 || ss.Width < 0 || ss.Height < 0
	switch {
	case srcSamples > 1 || dstSamples > 1:
		// Resolves, and copies of samples, of boxes of the same size.
		if scaled || flipped || dstSamples > 1 && dstSamples != srcSamples || dstSamples == 1 && aspects != aspectColor {
			return ErrNotNative
		}
	default:
		dfeat, sfeat := d.lockedFormatFeatures(df.VK), d.lockedFormatFeatures(sf.VK)
		if dfeat&featureBlitDst == 0 || sfeat&featureBlitSrc == 0 {
			return ErrNotNative
		}
		dint, dsigned := df.IsInteger()
		sint, ssigned := sf.IsInteger()
		if dint != sint || dsigned != ssigned || (dint || aspects != aspectColor) && b.Linear && scaled ||
			b.Linear && scaled && sfeat&featureFilterLinear == 0 {
			return ErrNotNative
		}
	}

	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	a := c.a
	c.general(cmds, b.Src)
	c.general(cmds, b.Dst)
	cmds.barrier()
	switch {
	case srcSamples > 1 && dstSamples == 1:
		r := get[vn.VkImageResolve](a)
		r.SrcSubresource, r.DstSubresource = ssub, dsub
		r.SrcOffset = vn.VkOffset3D{X: int32(ss.X), Y: int32(ss.Y)}
		r.DstOffset = vn.VkOffset3D{X: int32(ds.X), Y: int32(ds.Y)}
		r.Extent = vn.VkExtent3D{Width: uint32(ss.Width), Height: uint32(ss.Height), Depth: 1}
		ri := get[vn.Args_vkCmdResolveImage](a)
		ri.SrcImage, ri.SrcImageLayout, ri.DstImage, ri.DstImageLayout = b.Src.img, ImageLayoutGeneral, b.Dst.img, ImageLayoutGeneral
		ri.RegionCount, ri.PRegions = 1, unsafe.Pointer(r)
		cmds.call(vn.VK_COMMAND_TYPE_vkCmdResolveImage_EXT, unsafe.Pointer(ri))
	case srcSamples > 1:
		r := get[vn.VkImageCopy](a)
		r.SrcSubresource, r.DstSubresource = ssub, dsub
		r.SrcOffset = vn.VkOffset3D{X: int32(ss.X), Y: int32(ss.Y)}
		r.DstOffset = vn.VkOffset3D{X: int32(ds.X), Y: int32(ds.Y)}
		r.Extent = vn.VkExtent3D{Width: uint32(ss.Width), Height: uint32(ss.Height), Depth: 1}
		ci := get[vn.Args_vkCmdCopyImage](a)
		ci.SrcImage, ci.SrcImageLayout, ci.DstImage, ci.DstImageLayout = b.Src.img, ImageLayoutGeneral, b.Dst.img, ImageLayoutGeneral
		ci.RegionCount, ci.PRegions = 1, unsafe.Pointer(r)
		cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyImage_EXT, unsafe.Pointer(ci))
	default:
		r := get[vn.VkImageBlit](a)
		r.SrcSubresource, r.DstSubresource = ssub, dsub
		var sz0, sz1, dz0, dz1 int32 = 0, 1, 0, 1
		if src3D {
			sz0, sz1 = span(ss.Z, ss.Depth)
			dz0, dz1 = span(ds.Z, ds.Depth)
		}
		sx0, sx1 := span(ss.X, ss.Width)
		sy0, sy1 := span(ss.Y, ss.Height)
		dx0, dx1 := span(ds.X, ds.Width)
		dy0, dy1 := span(ds.Y, ds.Height)
		r.SrcOffsets = [2]vn.VkOffset3D{{X: sx0, Y: sy0, Z: sz0}, {X: sx1, Y: sy1, Z: sz1}}
		r.DstOffsets = [2]vn.VkOffset3D{{X: dx0, Y: dy0, Z: dz0}, {X: dx1, Y: dy1, Z: dz1}}
		bi := get[vn.Args_vkCmdBlitImage](a)
		bi.SrcImage, bi.SrcImageLayout, bi.DstImage, bi.DstImageLayout = b.Src.img, ImageLayoutGeneral, b.Dst.img, ImageLayoutGeneral
		bi.RegionCount, bi.PRegions = 1, unsafe.Pointer(r)
		if b.Linear && scaled {
			bi.Filter = 1 // VK_FILTER_LINEAR
		}
		cmds.call(vn.VK_COMMAND_TYPE_vkCmdBlitImage_EXT, unsafe.Pointer(bi))
	}
	cmds.barrier()
	return nil
}
