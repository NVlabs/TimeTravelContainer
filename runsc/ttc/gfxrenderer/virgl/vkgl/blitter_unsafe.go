// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// The blitter draws the blits that Vulkan's blits do not do, as Gallium's
// util_blitter does: a rectangle of the destination's box, of each of its
// layers, of a fragment shader that fetches the source's texels, or
// samples them of a linear filter, of a view of the source's format of the
// blit, of the channels of its mask, and of its scissor.

// vkR32G32B32A32Sfloat is VK_FORMAT_R32G32B32A32_SFLOAT, of the blitter's
// vertices.
const vkR32G32B32A32Sfloat = 109

// blitVS is the blitter's vertex shader: of a position, and of texel and
// normalized coordinates of the source.
const blitVS = `VERT
DCL IN[0]
DCL IN[1]
DCL IN[2]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL OUT[2], GENERIC[1]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[1]
  2: MOV OUT[2], IN[2]
  3: END
`

// blitFS returns a fragment shader of the blitter's, of a view of target,
// a TGSI target's name, of texels of typ, FLOAT, SINT or UINT: it samples
// the view's normalized coordinates, of a linear filter, or fetches the
// texels of its texel coordinates, and writes their depth, or their
// channels of swizzle, of TGSI's xyzw.
func blitFS(target, typ string, linear, depth bool, swizzle string) string {
	var b strings.Builder
	b.WriteString("FRAG\n")
	if linear {
		b.WriteString("DCL IN[0], GENERIC[1], LINEAR\n")
	} else {
		b.WriteString("DCL IN[0], GENERIC[0], LINEAR\n")
	}
	if depth {
		b.WriteString("DCL OUT[0], POSITION\n")
	} else {
		b.WriteString("DCL OUT[0], COLOR\n")
	}
	fmt.Fprintf(&b, "DCL SAMP[0]\nDCL SVIEW[0], %s, %s\nDCL TEMP[0..1]\n", target, typ)
	n := 0
	if linear {
		fmt.Fprintf(&b, "  %d: TEX TEMP[1], IN[0], SAMP[0], %s\n", n, target)
		n++
	} else {
		// Of texel coordinates of the texels' centers, which F2I floors,
		// and of the view's level 0, the blit's.
		fmt.Fprintf(&b, "  %d: F2I TEMP[0], IN[0]\n  %d: TXF TEMP[1], TEMP[0], SAMP[0], %s\n", n, n+1, target)
		n += 2
	}
	if depth {
		fmt.Fprintf(&b, "  %d: MOV OUT[0].z, TEMP[1].xxxx\n", n)
	} else {
		fmt.Fprintf(&b, "  %d: MOV OUT[0], TEMP[1].%s\n", n, swizzle)
	}
	fmt.Fprintf(&b, "  %d: END\n", n+1)
	return b.String()
}

// blitTarget returns the target of the blitter's views of a texture of
// target t, and its TGSI name: arrays' and cubes' as arrays.
func blitTarget(t uint32) (uint32, string) {
	switch t {
	case Target1D, Target1DArray:
		return Target1DArray, "1D_ARRAY"
	case TargetRect:
		return TargetRect, "RECT"
	case Target3D:
		return Target3D, "3D"
	}
	return Target2DArray, "2D_ARRAY"
}

// blitOutput returns the swizzle of the blitter's write of a color of the
// channels of a format's, of the Vulkan format's channels, and the mask of
// the Vulkan format's channels of mask, of the format's.
func blitOutput(f Format, mask uint32) (string, uint32) {
	out := []byte("xyzw")
	var vkMask uint32
	for k := range 4 {
		out[k] = 'x'
		for j := range f.Swizzle {
			if f.Swizzle[j] == "XYZW"[k] {
				out[k] = "xyzw"[j]
				if mask&(1<<j) != 0 {
					vkMask |= 1 << k
				}
				break
			}
		}
	}
	return string(out), vkMask
}

// shaderBlit draws b, of the blitter, of scissor, or none, in order of the
// context's commands: it returns ErrNotNative of blits that it does not
// draw either, of multisampled sources, of stencil, of 3D destinations, or
// of blending.
func (c *Context) shaderBlit(b Blit, scissor *Scissor) error {
	d := c.d
	df, _ := LookupFormat(b.DstFormat)
	sf, _ := LookupFormat(b.SrcFormat)
	depth := b.Mask&MaskZ != 0
	if max(b.Src.Desc.Samples, 1) > 1 || b.Mask&MaskS != 0 || b.Dst.Desc.Target == Target3D {
		return ErrNotNative
	}
	ds, ss := b.DstBox, b.SrcBox
	src3D := b.Src.Desc.Target == Target3D
	if ds.Depth <= 0 || ss.Depth <= 0 || !src3D && ds.Depth != ss.Depth {
		return ErrNotNative
	}
	typ := "FLOAT"
	integer, signed := sf.IsInteger()
	switch {
	case integer && signed:
		typ = "SINT"
	case integer:
		typ = "UINT"
	}
	dint, dsigned := df.IsInteger()
	if dint != integer || dint && dsigned != signed {
		return ErrNotNative
	}
	scaled := ds.Width != ss.Width || ds.Height != ss.Height || ds.Depth != ss.Depth
	linear := b.Linear && scaled && !integer && !depth && d.lockedFormatFeatures(sf.VK)&featureFilterLinear != 0
	viewTarget, tgsiTarget := blitTarget(b.Src.Desc.Target)
	swizzle, vkMask := blitOutput(df, b.Mask)

	// The source's view, of the blit's level, and of every layer, of the
	// format of the blit.
	last := b.Src.Desc.ArraySize - 1
	if src3D {
		last = 0
	}
	view, err := d.NewSamplerView(b.Src, ViewDesc{Format: b.SrcFormat, Target: viewTarget, LastLayer: last,
		FirstLevel: b.SrcLevel, LastLevel: b.SrcLevel, Swizzle: [4]uint32{SwizzleX, SwizzleY, SwizzleZ, SwizzleW}})
	if err != nil {
		return ErrNotNative
	}
	if err := c.Keep(view, func() func() { return func() { d.DestroySamplerView(view) } }); err != nil {
		d.DestroySamplerView(view)
		return err
	}
	sampler, err := c.blitSampler(linear)
	if err != nil {
		return err
	}
	p, err := c.blitPipeline(tgsiTarget, typ, linear, depth, swizzle, vkMask, b.Dst, df)
	if err != nil {
		return err
	}

	saved := c.fb
	defer c.SetFramebuffer(saved)
	w, h, _ := b.Dst.Desc.levelExtent(b.DstLevel)
	sw, sh, sd := b.Src.Desc.levelExtent(b.SrcLevel)
	for i := int64(0); i < ds.Depth; i++ {
		s, err := d.NewSurface(b.Dst, b.DstFormat, b.DstLevel, uint32(ds.Z+i), uint32(ds.Z+i))
		if err != nil {
			return ErrNotNative
		}
		if err := c.Keep(s, func() func() { return func() { d.DestroySurface(s) } }); err != nil {
			d.DestroySurface(s)
			return err
		}
		if depth {
			c.SetFramebuffer(Framebuffer{Depth: s})
		} else {
			c.SetFramebuffer(Framebuffer{Colors: []*Surface{s}})
		}
		// The vertices' coordinates of the source: of texels, and
		// normalized, of the view's target's order, of the layer, or of
		// the slice's center.
		layer := float64(ss.Z + i)
		if src3D {
			layer = float64(ss.Z) + (float64(i)+0.5)*float64(ss.Depth)/float64(ds.Depth)
		}
		var vertices []byte
		for _, corner := range [4][2]int64{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
			x := float64(ds.X + corner[0]*ds.Width)
			y := float64(ds.Y + corner[1]*ds.Height)
			u := float64(ss.X + corner[0]*ss.Width)
			v := float64(ss.Y + corner[1]*ss.Height)
			texel := [4]float64{u, v, layer, 0}
			norm := [4]float64{u / float64(sw), v / float64(sh), layer, 0}
			switch viewTarget {
			case Target1DArray:
				texel, norm = [4]float64{u, layer, 0, 0}, [4]float64{u / float64(sw), layer, 0, 0}
			case TargetRect:
				norm = texel
			case Target3D:
				norm[2] = layer / float64(sd)
			}
			for _, f := range append([]float64{2*x/float64(w) - 1, 2*y/float64(h) - 1, 0, 1}, append(texel[:], norm[:]...)...) {
				vertices = binary.LittleEndian.AppendUint32(vertices, math.Float32bits(float32(f)))
			}
		}
		vb, err := c.Upload(vertices)
		if err != nil {
			return err
		}
		if err := c.Render(func(cmds *Commands) error {
			cmds.BindPipeline(p)
			cmds.SetViewport(0, 0, float32(w), float32(h), 0, 1)
			if scissor != nil {
				x0, y0 := min(int64(scissor.MinX), w), min(int64(scissor.MinY), h)
				x1, y1 := min(max(int64(scissor.MaxX), x0), w), min(max(int64(scissor.MaxY), y0), h)
				cmds.SetScissor(int32(x0), int32(y0), uint32(x1-x0), uint32(y1-y0))
			} else {
				cmds.SetScissor(0, 0, uint32(w), uint32(h))
			}
			cmds.SetBlendConstants([4]float32{})
			cmds.SetStencilReference(0, 0)
			if err := c.BindResources(cmds, p, nil, []TextureBinding{{Stage: tgsi.ProcessorFragment, View: view, Sampler: sampler}}); err != nil {
				return err
			}
			var push [tospirv.PushConstantsSize]byte
			cmds.PushConstants(p, 0, push[:])
			cmds.BindVertexBuffers(0, []*Buffer{vb}, []uint64{0})
			cmds.DrawVertices(4, 1, 0, 0)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// blitSampler returns the context's sampler of the blitter's, of a linear
// filter or of the nearest texels', of coordinates clamped to the edges.
func (c *Context) blitSampler(linear bool) (*Sampler, error) {
	i := 0
	if linear {
		i = 1
	}
	if c.blitSamplers[i] == nil {
		s, err := c.d.NewSampler(SamplerDesc{MagFilter: uint32(i), MinFilter: uint32(i), AddressMode: 2}) // VK_SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE
		if err != nil {
			return nil, err
		}
		c.blitSamplers[i] = s
	}
	return c.blitSamplers[i], nil
}

// blitPipeline returns the context's pipeline of the blitter's, of its
// fragment shader's parameters, of a destination of dst's, as format f,
// writing vkMask's channels.
func (c *Context) blitPipeline(target, typ string, linear, depth bool, swizzle string, vkMask uint32, dst *Texture, f Format) (*Pipeline, error) {
	key := fmt.Sprintf("%s/%s/%v/%v/%s/%x/%d/%d", target, typ, linear, depth, swizzle, vkMask, f.VK, max(dst.Desc.Samples, 1))
	if p := c.blitPipelines[key]; p != nil {
		return p, nil
	}
	vs, err := translateTGSI(blitVS, tospirv.Options{})
	if err != nil {
		return nil, err
	}
	fopts := tospirv.Options{ColorBuffers: 1}
	switch typ {
	case "SINT":
		fopts.ColorTypes[0] = tospirv.Int
	case "UINT":
		fopts.ColorTypes[0] = tospirv.Uint
	}
	if depth {
		fopts.ColorBuffers = 0
	}
	fs, err := translateTGSI(blitFS(target, typ, linear, depth, swizzle), fopts)
	if err != nil {
		return nil, err
	}
	Link(vs, fs)
	var resources []StageResource
	for _, r := range fs.Resources {
		resources = append(resources, StageResource{Stage: tgsi.ProcessorFragment, Resource: r})
	}
	desc := PipelineDesc{
		VS: vs.Words, FS: fs.Words, Resources: resources,
		Bindings: []VertexBinding{{Binding: 0, Stride: 48}},
		Inputs: []VertexInput{{Location: 0, Format: vkR32G32B32A32Sfloat},
			{Location: 1, Format: vkR32G32B32A32Sfloat, Offset: 16},
			{Location: 2, Format: vkR32G32B32A32Sfloat, Offset: 32}},
		Topology:   4, // VK_PRIMITIVE_TOPOLOGY_TRIANGLE_STRIP
		Rasterizer: &Rasterizer{DepthClip: true, Scissor: true},
		DSA:        &DSA{},
		Blend:      &Blend{},
		Samples:    max(dst.Desc.Samples, 1), SampleMask: ^uint32(0),
	}
	if depth {
		desc.DSA = &DSA{DepthEnable: true, DepthWrite: true, DepthFunc: 7} // PIPE_FUNC_ALWAYS
		desc.DepthFormat, desc.HasDepth, desc.HasStencil = f.VK, true, dst.aspects&aspectStencil != 0
	} else {
		desc.Blend.RT[0].ColorMask = vkMask
		desc.ColorFormats = []uint32{f.VK}
	}
	p, err := c.d.NewPipeline(desc)
	if err != nil {
		return nil, err
	}
	if c.blitPipelines == nil {
		c.blitPipelines = make(map[string]*Pipeline)
	}
	c.blitPipelines[key] = p
	return p, nil
}

// translateTGSI returns text's translation of opts.
func translateTGSI(text string, opts tospirv.Options) (*tospirv.Result, error) {
	s, err := tgsi.Parse(text)
	if err != nil {
		return nil, err
	}
	return tospirv.Translate(s, opts)
}

// destroyBlitter destroys the context's blitter's pipelines and samplers,
// which the GPU no longer uses.
func (c *Context) destroyBlitter() {
	for _, p := range c.blitPipelines {
		c.d.DestroyPipeline(p)
	}
	c.blitPipelines = nil
	for i, s := range c.blitSamplers {
		if s != nil {
			c.d.DestroySampler(s)
			c.blitSamplers[i] = nil
		}
	}
}
