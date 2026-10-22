// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"fmt"
	"math"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Framebuffer is what draws render to: color surfaces, of which any may be
// nil, and a depth and stencil surface, or nil; or, of none, an area of
// Width by Height, of Layers and Samples.
type Framebuffer struct {
	Colors                         []*Surface
	Depth                          *Surface
	Width, Height, Layers, Samples uint32
}

// attachments returns the framebuffer's surfaces, of its depth's last.
func (fb *Framebuffer) attachments() []*Surface {
	return append(append([]*Surface(nil), fb.Colors...), fb.Depth)
}

// area returns the framebuffer's area and layers: its surfaces' least, or
// its own of none.
func (fb *Framebuffer) area() (width, height, layers uint32) {
	width, height, layers = fb.Width, fb.Height, max(fb.Layers, 1)
	some := false
	for _, s := range fb.attachments() {
		if s == nil {
			continue
		}
		w, h := s.extent()
		n := s.Last - s.First + 1
		if !some {
			width, height, layers, some = w, h, n, true
			continue
		}
		width, height, layers = min(width, w), min(height, h), min(layers, n)
	}
	return width, height, layers
}

// samples returns the framebuffer's surfaces' samples, or its own of none.
func (fb *Framebuffer) samples() uint32 {
	for _, s := range fb.attachments() {
		if s != nil {
			return max(s.Texture.Desc.Samples, 1)
		}
	}
	return max(fb.Samples, 1)
}

// SetFramebuffer has the context's draws render to fb.
func (c *Context) SetFramebuffer(fb Framebuffer) {
	c.endRendering()
	c.fb = fb
}

// endRendering ends the batch's rendering instance, if it has one.
func (c *Context) endRendering() {
	if c.batch == nil || !c.batch.rendering {
		return
	}
	defer c.a.reset()
	cmds := &Commands{d: c.d, a: c.a, cb: c.batch.cb}
	cmds.EndRendering()
	c.batch.rendering = false
}

// rendering returns the batch's commands, of a rendering instance of the
// context's framebuffer, beginning one if the batch has none.
func (c *Context) rendering() (*Commands, error) {
	cmds, err := c.commands()
	if err != nil {
		return nil, err
	}
	if c.batch.rendering {
		return cmds, nil
	}
	a := c.a
	width, height, layers := c.fb.area()
	if width == 0 || height == 0 {
		// Of which Vulkan renders nothing.
		return nil, fmt.Errorf("a rendering of a framebuffer of %dx%d", width, height)
	}
	for _, s := range c.fb.attachments() {
		if s != nil {
			c.general(cmds, s.Texture)
		}
	}
	// Of what earlier commands, transfers and other renderings, wrote.
	cmds.barrier()
	info := get[vn.VkRenderingInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_RENDERING_INFO
	info.RenderArea = vn.VkRect2D{Extent: vn.VkExtent2D{Width: width, Height: height}}
	info.LayerCount = layers
	if n := len(c.fb.Colors); n != 0 {
		atts := array[vn.VkRenderingAttachmentInfo](a, n)
		for i, s := range c.fb.Colors {
			atts[i].SType = vn.VK_STRUCTURE_TYPE_RENDERING_ATTACHMENT_INFO
			if s != nil {
				atts[i].ImageView = s.view
				atts[i].ImageLayout = ImageLayoutGeneral
			}
		}
		info.ColorAttachmentCount, info.PColorAttachments = uint32(n), unsafe.Pointer(&atts[0])
	}
	if s := c.fb.Depth; s != nil {
		att := get[vn.VkRenderingAttachmentInfo](a)
		att.SType = vn.VK_STRUCTURE_TYPE_RENDERING_ATTACHMENT_INFO
		att.ImageView = s.view
		att.ImageLayout = ImageLayoutGeneral
		if s.aspects&aspectDepth != 0 {
			info.PDepthAttachment = unsafe.Pointer(att)
		}
		if s.aspects&aspectStencil != 0 {
			info.PStencilAttachment = unsafe.Pointer(att)
		}
	}
	br := get[vn.Args_vkCmdBeginRendering](a)
	br.PRenderingInfo = unsafe.Pointer(info)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdBeginRendering_EXT, unsafe.Pointer(br))
	c.batch.rendering = true
	return cmds, nil
}

// Gallium's buffers of a clear, PIPE_CLEAR_*.
const (
	ClearDepth   = 1 << 0
	ClearStencil = 1 << 1
	// ClearColor0 is color buffer 0's, which later ones' follow.
	ClearColor0 = 1 << 2
)

// Clear clears the framebuffer's buffers of buffers, a mask of Clear*:
// colors to color, the bits of their channels, as their formats have them,
// the depth to depth, and the stencil to stencil.
func (c *Context) Clear(buffers uint32, color [4]uint32, depth float64, stencil uint32) error {
	width, height, _ := c.fb.area()
	return c.clear(buffers, color, depth, stencil, 0, 0, width, height)
}

// ClearSurface clears the rect of s of x, y, width and height, of its
// layers, as Gallium's clear_render_target and clear_depth_stencil do: of
// buffers, a mask of Clear*, a color surface's color to color, and a depth
// and stencil surface's depth to depth and stencil to stencil.
func (c *Context) ClearSurface(s *Surface, buffers uint32, color [4]uint32, depth float64, stencil, x, y, width, height uint32) error {
	w, h := s.extent()
	if x > w || width > w-x || y > h || height > h-y {
		return fmt.Errorf("a clear of %dx%d at %d,%d of a surface of %dx%d", width, height, x, y, w, h)
	}
	fb := c.fb
	defer c.SetFramebuffer(fb)
	if s.aspects == aspectColor {
		c.SetFramebuffer(Framebuffer{Colors: []*Surface{s}})
		buffers &= ClearColor0
	} else {
		c.SetFramebuffer(Framebuffer{Depth: s})
		buffers &= ClearDepth | ClearStencil
	}
	return c.clear(buffers, color, depth, stencil, x, y, width, height)
}

// clear clears the framebuffer's buffers of buffers, as Clear, of the rect
// of x, y, width and height, of its area.
func (c *Context) clear(buffers uint32, color [4]uint32, depth float64, stencil, x, y, width, height uint32) error {
	if width == 0 || height == 0 {
		return nil
	}
	defer c.a.reset()
	cmds, err := c.rendering()
	if err != nil {
		return err
	}
	a := c.a
	var atts []vn.VkClearAttachment
	for i, s := range c.fb.Colors {
		if s == nil || buffers&(ClearColor0<<i) == 0 {
			continue
		}
		att := vn.VkClearAttachment{AspectMask: aspectColor, ColorAttachment: uint32(i)}
		*(*[4]uint32)(unsafe.Pointer(&att.ClearValue)) = s.Format.vkColor(color)
		atts = append(atts, att)
	}
	if s := c.fb.Depth; s != nil {
		var aspects uint32
		if buffers&ClearDepth != 0 {
			aspects |= s.aspects & aspectDepth
		}
		if buffers&ClearStencil != 0 {
			aspects |= s.aspects & aspectStencil
		}
		if aspects != 0 {
			att := vn.VkClearAttachment{AspectMask: aspects}
			value := (*[2]uint32)(unsafe.Pointer(&att.ClearValue))
			value[0], value[1] = math.Float32bits(float32(depth)), stencil
			atts = append(atts, att)
		}
	}
	_, _, layers := c.fb.area()
	if len(atts) == 0 {
		return nil
	}
	ca := array[vn.VkClearAttachment](a, len(atts))
	copy(ca, atts)
	rect := get[vn.VkClearRect](a)
	*rect = vn.VkClearRect{Rect: vn.VkRect2D{Offset: vn.VkOffset2D{X: int32(x), Y: int32(y)},
		Extent: vn.VkExtent2D{Width: width, Height: height}}, LayerCount: layers}
	cl := get[vn.Args_vkCmdClearAttachments](a)
	cl.AttachmentCount, cl.PAttachments = uint32(len(ca)), unsafe.Pointer(&ca[0])
	cl.RectCount, cl.PRects = 1, unsafe.Pointer(rect)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdClearAttachments_EXT, unsafe.Pointer(cl))
	return nil
}

// ClearTexture sets the texels of t's box of level to texel, of t's
// format's blocks, of which it has at least one, in order of the context's
// commands, as Gallium's clear_texture does, of textures that the device
// renders to, as the decoder's pipe.c checks them.
func (c *Context) ClearTexture(t *Texture, level uint32, box Box, texel []byte) error {
	f := t.Format
	r := TextureRegion{Level: level, Box: box}
	if box.Width < 0 || box.Height < 0 || box.Depth < 0 || uint32(len(texel)) < f.BlockBytes ||
		t.usage&(ImageColorAttachment|imageUsageDepthStencil) == 0 {
		return fmt.Errorf("a clear of %+v of level %d of %+v", box, level, t.Desc)
	}
	if box.Width == 0 || box.Height == 0 || box.Depth == 0 {
		return t.checkBox(r)
	}
	if err := t.checkBox(r); err != nil {
		return err
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	// Rows of the texel, of at most a MiB, but a row, which each copy of
	// rows of the box reads.
	_, _, row, rows, _ := t.memLayout(r)
	chunk := max(1, min(rows, (1<<20)/row))
	s, err := c.staging(chunk * row)
	if err != nil {
		return err
	}
	data := s.Bytes()
	for i := 0; i < len(data); i += int(f.BlockBytes) {
		copy(data[i:], texel[:f.BlockBytes])
	}
	bh := int64(f.BlockHeight)
	var regions []vn.VkBufferImageCopy
	for z := int64(0); z < box.Depth; z++ {
		for y := uint64(0); y < rows; y += chunk {
			part := Box{X: box.X, Y: box.Y + int64(y)*bh, Z: box.Z + z, Width: box.Width, Depth: 1}
			part.Height = min(int64(chunk)*bh, box.Height-int64(y)*bh)
			regions = append(regions, t.copyRegion(TextureRegion{Level: level, Box: part}, 0))
		}
	}
	rs := array[vn.VkBufferImageCopy](c.a, len(regions))
	copy(rs, regions)
	c.general(cmds, t)
	cmds.barrier()
	ci := get[vn.Args_vkCmdCopyBufferToImage](c.a)
	ci.SrcBuffer, ci.DstImage, ci.DstImageLayout = s.buf, t.img, t.layout
	ci.RegionCount, ci.PRegions = uint32(len(rs)), unsafe.Pointer(&rs[0])
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBufferToImage_EXT, unsafe.Pointer(ci))
	cmds.barrier()
	return nil
}

// Render records f's commands in a rendering instance of the context's
// framebuffer.
func (c *Context) Render(f func(cmds *Commands) error) error {
	defer c.a.reset()
	cmds, err := c.rendering()
	if err != nil {
		return err
	}
	return f(cmds)
}

// Upload returns a buffer, visible to the host, of data, which shaders
// read as a uniform or a shader buffer, and draws as vertices or indexes,
// and which the batch frees once the GPU is done with it.
func (c *Context) Upload(data []byte) (*Buffer, error) {
	if _, err := c.commands(); err != nil {
		return nil, err
	}
	b, err := c.d.NewBuffer(uint64(max(len(data), 4)), BufferUniform|BufferStorage|BufferVertex|BufferIndex, true)
	if err != nil {
		return nil, err
	}
	copy(b.Bytes(), data)
	c.batch.staging = append(c.batch.staging, b)
	return b, nil
}
