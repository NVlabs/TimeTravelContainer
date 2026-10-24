// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"
	"math"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// TestBresenhamLines draws a line of a slope of 1/2, of which OpenGL's
// rule, diamond exit, lights a pixel of each column, where Vulkan's lines,
// rectangles, light 2 of some.
func TestBresenhamLines(t *testing.T) {
	d := newTestDevice(t)
	if !d.bresenhamLines {
		t.Skipf("%s draws no lines of Bresenham's rule", d.Name)
	}
	c, err := d.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Destroy()
	const size = 16
	tex, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: size, Height: size})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DestroyTexture(tex)
	s, err := d.NewSurface(tex, virglR8G8B8A8Unorm, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DestroySurface(s)
	c.SetFramebuffer(Framebuffer{Colors: []*Surface{s}})
	vs := translate(t, passVS, tospirv.Options{})
	fs := translate(t, colorFS, tospirv.Options{ColorBuffers: 1})
	Link(vs, fs)
	p, err := d.NewPipeline(PipelineDesc{
		VS: vs.Words, FS: fs.Words,
		Bindings: []VertexBinding{{Binding: 0, Stride: 32}},
		Inputs: []VertexInput{{Location: 0, Binding: 0, Format: formatR32G32B32A32Sfloat},
			{Location: 1, Binding: 0, Format: formatR32G32B32A32Sfloat, Offset: 16}},
		Topology:   1, // VK_PRIMITIVE_TOPOLOGY_LINE_LIST
		Rasterizer: &Rasterizer{DepthClip: true, LineWidth: 1}, DSA: &DSA{}, Blend: &Blend{RT: [MaxColorBuffers]RTBlend{{ColorMask: 0xf}}},
		ColorFormats: []uint32{formatR8G8B8A8Unorm}, Samples: 1, SampleMask: ^uint32(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DestroyPipeline(p)
	// From pixel (0.5, 0.5) to (16.5, 8.5), of NDC.
	ndc := func(x float32) float32 { return 2*x/size - 1 }
	var verts []byte
	for _, v := range [][8]float32{{ndc(0.5), ndc(0.5), 0, 1, 1, 1, 1, 1}, {ndc(16.5), ndc(8.5), 0, 1, 1, 1, 1, 1}} {
		for _, f := range v {
			verts = binary.LittleEndian.AppendUint32(verts, math.Float32bits(f))
		}
	}
	vb, err := c.Upload(verts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ClearSurface(s, ClearColor0, [4]uint32{}, 0, 0, 0, 0, size, size); err != nil {
		t.Fatal(err)
	}
	if err := c.Render(func(cmds *Commands) error {
		cmds.BindPipeline(p)
		cmds.SetViewport(0, 0, size, size, 0, 1)
		cmds.SetScissor(0, 0, size, size)
		cmds.SetBlendConstants([4]float32{})
		cmds.SetStencilReference(0, 0)
		var push [tospirv.PushConstantsSize]byte
		cmds.PushConstants(p, 0, push[:])
		cmds.BindVertexBuffers(0, []*Buffer{vb}, []uint64{0})
		cmds.DrawVertices(2, 1, 0, 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	px := make([]byte, size*size*4)
	if err := c.ReadTexture(tex, TextureRegion{Box: Box{Width: size, Height: size, Depth: 1}}, px, 0); err != nil {
		t.Fatal(err)
	}
	counts := make([]int, size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if px[4*(y*size+x)] > 128 {
				counts[x]++
			}
		}
	}
	for x, n := range counts {
		if n != 1 {
			t.Fatalf("pixels of each column: %v, want 1 of column %d", counts, x)
		}
	}
}
