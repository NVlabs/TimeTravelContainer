// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"
	"math"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// formatByName returns virgl's format of name, as Mesa names it, but
// PIPE_FORMAT_.
func formatByName(t *testing.T, name string) uint32 {
	t.Helper()
	vf, ok := FormatByName(name)
	if !ok {
		t.Fatalf("no format %s", name)
	}
	return vf
}

// newComputePipeline returns a compute pipeline of a TGSI compute shader,
// of its shared memory.
func newComputePipeline(t *testing.T, d *Device, text string, shared int) *Pipeline {
	t.Helper()
	cs := translate(t, text, tospirv.Options{SharedMemory: shared})
	var resources []StageResource
	for _, r := range cs.Resources {
		resources = append(resources, StageResource{tgsi.ProcessorCompute, r})
	}
	p, err := d.NewComputePipeline(ComputeDesc{CS: cs.Words, Resources: resources})
	if err != nil {
		t.Fatalf("NewComputePipeline: %v", err)
	}
	t.Cleanup(func() { d.DestroyPipeline(p) })
	return p
}

// TestStorageFormats checks that the device stores texels of OpenGL ES
// 3.1's formats of images, and logs those of 32-bit integers' atomics.
func TestStorageFormats(t *testing.T) {
	d := newTestDevice(t)
	for _, name := range []string{"R32G32B32A32_FLOAT", "R16G16B16A16_FLOAT", "R32_FLOAT", "R8G8B8A8_UNORM",
		"R8G8B8A8_SNORM", "R32G32B32A32_SINT", "R16G16B16A16_SINT", "R8G8B8A8_SINT", "R32_SINT",
		"R32G32B32A32_UINT", "R16G16B16A16_UINT", "R8G8B8A8_UINT", "R32_UINT"} {
		f, _ := d.LookupFormat(formatByName(t, name))
		features := d.lockedFormatFeatures(f.VK)
		if features&featureStorageImage == 0 {
			t.Errorf("%s: no storage of images", name)
		}
		// VK_FORMAT_FEATURE_STORAGE_IMAGE_ATOMIC_BIT.
		if f.Name == "R32_UINT" || f.Name == "R32_SINT" {
			t.Logf("%s: atomics of images: %v", name, features&0x4 != 0)
		}
	}
}

// TestComputeBuffers dispatches workgroups of a compute shader that reads
// a shader buffer to shared memory, reverses each workgroup's words through
// it, writes them to another buffer, and counts its invocations of an
// atomic of a third.
func TestComputeBuffers(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	p := newComputePipeline(t, d, `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 64
PROPERTY CS_FIXED_BLOCK_HEIGHT 1
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL SV[1], BLOCK_ID
DCL BUFFER[0]
DCL BUFFER[1]
DCL BUFFER[2]
DCL MEMORY[0], SHARED
DCL TEMP[0..2], LOCAL
IMM[0] UINT32 {64, 2, 0, 1}
IMM[1] UINT32 {252, 0, 0, 0}
  0: UMAD TEMP[0].x, SV[1].xxxx, IMM[0].xxxx, SV[0].xxxx
  1: SHL TEMP[0].y, TEMP[0].xxxx, IMM[0].yyyy
  2: LOAD TEMP[1].x, BUFFER[0], TEMP[0].yyyy
  3: SHL TEMP[0].z, SV[0].xxxx, IMM[0].yyyy
  4: STORE MEMORY[0].x, TEMP[0].zzzz, TEMP[1].xxxx
  5: BARRIER
  6: XOR TEMP[0].w, TEMP[0].zzzz, IMM[1].xxxx
  7: LOAD TEMP[2].x, MEMORY[0], TEMP[0].wwww
  8: STORE BUFFER[1].x, TEMP[0].yyyy, TEMP[2].xxxx
  9: ATOMUADD TEMP[2].y, BUFFER[2], IMM[0].zzzz, IMM[0].wwww
 10: END
`, 64*4)
	const n = 256
	in := make([]byte, 4*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(in[4*i:], uint32(1000+i))
	}
	var bufs [3]*Buffer
	for i, size := range []uint64{4 * n, 4 * n, 16} {
		if bufs[i], err = d.NewBuffer(size, BufferStorage|BufferTransferSrc|BufferTransferDst, false); err != nil {
			t.Fatalf("NewBuffer: %v", err)
		}
		defer d.DestroyBuffer(bufs[i])
	}
	if err := c.WriteBuffer(bufs[0], 0, in); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := c.WriteBuffer(bufs[2], 0, make([]byte, 16)); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := d.CheckDispatch(n/64, 1, 1); err != nil {
		t.Fatalf("CheckDispatch: %v", err)
	}
	if err := c.Compute(func(cmds *Commands) error {
		cmds.BindPipeline(p)
		var buffers []BufferBinding
		for i, b := range bufs {
			buffers = append(buffers, BufferBinding{tgsi.ProcessorCompute, tospirv.StorageBuffer, i, b, 0, b.size})
		}
		if err := c.BindResources(cmds, p, buffers, nil, nil); err != nil {
			return err
		}
		cmds.Dispatch(n/64, 1, 1)
		return nil
	}); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	out := make([]byte, 4*n)
	if err := c.ReadBuffer(bufs[1], 0, out); err != nil {
		t.Fatalf("ReadBuffer: %v", err)
	}
	for i := 0; i < n; i++ {
		want := uint32(1000 + i/64*64 + 63 - i%64)
		if got := binary.LittleEndian.Uint32(out[4*i:]); got != want {
			t.Fatalf("word %d: %d, want %d", i, got, want)
		}
	}
	count := make([]byte, 4)
	if err := c.ReadBuffer(bufs[2], 0, count); err != nil {
		t.Fatalf("ReadBuffer: %v", err)
	}
	if got := binary.LittleEndian.Uint32(count); got != n {
		t.Errorf("the atomic counted %d invocations, want %d", got, n)
	}
	// Of the same results, of a dispatch of its workgroups' counts of a
	// buffer.
	indirect, err := d.NewBuffer(16, BufferIndirect|BufferTransferDst, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(indirect)
	cmd := make([]byte, 16)
	binary.LittleEndian.PutUint32(cmd[4:], n/64)
	binary.LittleEndian.PutUint32(cmd[8:], 1)
	binary.LittleEndian.PutUint32(cmd[12:], 1)
	if err := c.WriteBuffer(indirect, 0, cmd); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := d.CheckIndirect(indirect, 4, 1, 0, 12); err != nil {
		t.Fatalf("CheckIndirect: %v", err)
	}
	if err := c.Compute(func(cmds *Commands) error {
		cmds.BindPipeline(p)
		var buffers []BufferBinding
		for i, b := range bufs {
			buffers = append(buffers, BufferBinding{tgsi.ProcessorCompute, tospirv.StorageBuffer, i, b, 0, b.size})
		}
		if err := c.BindResources(cmds, p, buffers, nil, nil); err != nil {
			return err
		}
		cmds.DispatchIndirect(indirect, 4)
		return nil
	}); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if err := c.ReadBuffer(bufs[2], 0, count); err != nil {
		t.Fatalf("ReadBuffer: %v", err)
	}
	if got := binary.LittleEndian.Uint32(count); got != 2*n {
		t.Errorf("the atomic counted %d invocations of both dispatches, want %d", got, 2*n)
	}
	if err := d.CheckIndirect(indirect, 8, 1, 0, 12); err == nil {
		t.Errorf("a command past its buffer was accepted")
	}
	if err := d.CheckDispatch(1<<31, 1, 1); err == nil {
		t.Errorf("a dispatch of 2^31 workgroups was accepted")
	}
}

// TestComputeImages stores a color to each texel of a 2D image, and counts
// the invocations of an atomic of an image of 32-bit integers, where the
// device has them.
func TestComputeImages(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	r32ui := formatByName(t, "R32_UINT")
	f, _ := d.LookupFormat(r32ui)
	atomics := d.lockedFormatFeatures(f.VK)&0x4 != 0
	text := `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 8
PROPERTY CS_FIXED_BLOCK_HEIGHT 8
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL SV[1], BLOCK_ID
DCL IMAGE[0], 2D, PIPE_FORMAT_R8G8B8A8_UNORM, WR
DCL IMAGE[1], 2D_ARRAY, PIPE_FORMAT_R32_UINT, WR
DCL TEMP[0..2], LOCAL
IMM[0] UINT32 {8, 1, 0, 0}
IMM[1] FLT32 {    1.0000,     0.0000,     0.5000,     1.0000}
  0: UMAD TEMP[0].xy, SV[1].xyyy, IMM[0].xxxx, SV[0].xyyy
  1: LOAD TEMP[1], IMAGE[0], TEMP[0].xyyy, 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  2: ADD TEMP[1], TEMP[1], IMM[1]
  3: STORE IMAGE[0], TEMP[0].xyyy, TEMP[1], 2D, PIPE_FORMAT_R8G8B8A8_UNORM
`
	if atomics {
		text += `  4: ATOMUADD TEMP[2].x, IMAGE[1], IMM[0].zzzz, IMM[0].yyyy, 2D_ARRAY, PIPE_FORMAT_R32_UINT
`
	} else {
		t.Logf("%s has no atomics of images of 32-bit integers", d.Name)
		text += `  4: STORE IMAGE[1], IMM[0].zzzz, IMM[0].yyyy, 2D_ARRAY, PIPE_FORMAT_R32_UINT
`
	}
	text += "  5: END\n"
	p := newComputePipeline(t, d, text, 0)
	color, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 16, Height: 16})
	if err != nil {
		t.Fatalf("NewTexture: %v", err)
	}
	defer d.DestroyTexture(color)
	counter, err := d.NewTexture(ResourceDesc{Target: Target2DArray, Format: r32ui, Width: 4, Height: 4, ArraySize: 2})
	if err != nil {
		t.Fatalf("NewTexture: %v", err)
	}
	defer d.DestroyTexture(counter)
	region := TextureRegion{Box: Box{Width: 16, Height: 16, Depth: 1}}
	texels := make([]byte, 16*16*4)
	for i := 0; i < len(texels); i += 4 {
		texels[i+1] = 64
	}
	if err := c.WriteTexture(color, region, texels, 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	counterRegion := TextureRegion{Box: Box{Width: 4, Height: 4, Depth: 2}}
	if err := c.WriteTexture(counter, counterRegion, make([]byte, 4*4*4*2), 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	cv, err := d.NewImageView(color, ViewDesc{Format: virglR8G8B8A8Unorm, Target: Target2D})
	if err != nil {
		t.Fatalf("NewImageView: %v", err)
	}
	defer d.DestroyImageView(cv)
	// Of the counter's second layer, of an array of it alone.
	nv, err := d.NewImageView(counter, ViewDesc{Format: r32ui, Target: Target2DArray, FirstLayer: 1, LastLayer: 1})
	if err != nil {
		t.Fatalf("NewImageView: %v", err)
	}
	defer d.DestroyImageView(nv)
	if _, err := d.NewImageView(color, ViewDesc{Format: virglR8G8B8A8Unorm, Target: Target2D, LastLevel: 1}); err == nil {
		t.Errorf("an image view of 2 levels was created")
	}
	if err := c.Compute(func(cmds *Commands) error {
		cmds.BindPipeline(p)
		images := []ImageBinding{{tgsi.ProcessorCompute, 0, cv}, {tgsi.ProcessorCompute, 1, nv}}
		if err := c.BindResources(cmds, p, nil, nil, images); err != nil {
			return err
		}
		cmds.Dispatch(2, 2, 1)
		return nil
	}); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	out := make([]byte, len(texels))
	if err := c.ReadTexture(color, region, out, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	for i := 0; i < len(out); i += 4 {
		// 64/255 + 0, clamped, and 0.5 of 255, which GPUs round either
		// way.
		if out[i] != 255 || out[i+1] != 64 || out[i+2] < 127 || out[i+2] > 128 || out[i+3] != 255 {
			t.Fatalf("texel %d: %v, want {255, 64, 127 or 128, 255}", i/4, out[i:i+4])
		}
	}
	counts := make([]byte, 4*4*4*2)
	if err := c.ReadTexture(counter, counterRegion, counts, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	want := uint32(1)
	if atomics {
		want = 16 * 16
	}
	if got := binary.LittleEndian.Uint32(counts[4*4*4:]); got != want {
		t.Errorf("the second layer's first texel: %d, want %d", got, want)
	}
	if got := binary.LittleEndian.Uint32(counts); got != 0 {
		t.Errorf("the first layer's first texel: %d, want 0", got)
	}
}

// TestDrawIndirect draws the context's triangle of commands of a buffer:
// of vertices, and of indexes.
func TestDrawIndirect(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	tex, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 16, Height: 16})
	if err != nil {
		t.Fatalf("NewTexture: %v", err)
	}
	defer d.DestroyTexture(tex)
	s, err := d.NewSurface(tex, virglR8G8B8A8Unorm, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
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
		Topology:   3, // VK_PRIMITIVE_TOPOLOGY_TRIANGLE_LIST
		Rasterizer: &Rasterizer{DepthClip: true}, DSA: &DSA{}, Blend: &Blend{RT: [MaxColorBuffers]RTBlend{{ColorMask: 0xf}}},
		ColorFormats: []uint32{formatR8G8B8A8Unorm}, Samples: 1, SampleMask: ^uint32(0),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	defer d.DestroyPipeline(p)
	// The triangle over the lower left half, red, and over the upper right
	// half, green.
	var vertices []byte
	for _, v := range [][8]float32{{-1, -1, 0, 1, 1, 0, 0, 1}, {1, -1, 0, 1, 1, 0, 0, 1}, {-1, 1, 0, 1, 1, 0, 0, 1},
		{1, 1, 0, 1, 0, 1, 0, 1}, {-1, 1, 0, 1, 0, 1, 0, 1}, {1, -1, 0, 1, 0, 1, 0, 1}} {
		for _, f := range v {
			vertices = binary.LittleEndian.AppendUint32(vertices, math.Float32bits(f))
		}
	}
	vb, err := d.NewBuffer(uint64(len(vertices)), BufferVertex|BufferTransferDst, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(vb)
	// Indexes of the second triangle, from the fourth of the buffer.
	ib, err := d.NewBuffer(16, BufferIndex|BufferTransferDst, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(ib)
	// VkDrawIndirectCommand, of the first triangle, and, after a gap,
	// VkDrawIndexedIndirectCommand, of the second.
	commands, err := d.NewBuffer(64, BufferIndirect|BufferTransferDst, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(commands)
	var cmds []byte
	for _, w := range []uint32{3, 1, 0, 0, 0, 0, 0, 0, 3, 1, 3, 0, 0} {
		cmds = binary.LittleEndian.AppendUint32(cmds, w)
	}
	for _, w := range []struct {
		b    *Buffer
		data []byte
	}{{vb, vertices}, {ib, []byte{0, 0, 0, 0, 0, 0, 3, 0, 4, 0, 5, 0}}, {commands, cmds}} {
		if err := c.WriteBuffer(w.b, 0, w.data); err != nil {
			t.Fatalf("WriteBuffer: %v", err)
		}
	}
	if err := d.CheckIndirect(commands, 0, 1, 0, 16); err != nil {
		t.Fatalf("CheckIndirect: %v", err)
	}
	if err := d.CheckIndirect(commands, 32, 1, 0, 20); err != nil {
		t.Fatalf("CheckIndirect: %v", err)
	}
	if err := c.Clear(ClearColor0, [4]uint32{}, 0, 0); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := c.Render(func(cmds *Commands) error {
		cmds.BindPipeline(p)
		cmds.SetViewport(0, 0, 16, 16, 0, 1)
		cmds.SetScissor(0, 0, 16, 16)
		cmds.SetBlendConstants([4]float32{})
		cmds.SetStencilReference(0, 0)
		var push [tospirv.PushConstantsSize]byte
		cmds.PushConstants(p, 0, push[:])
		cmds.BindVertexBuffers(0, []*Buffer{vb}, []uint64{0})
		cmds.DrawIndirect(commands, 0, 1, 0, false)
		cmds.BindIndexBuffer(ib, 0, 2)
		cmds.DrawIndirect(commands, 32, 1, 0, true)
		return nil
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := make([]byte, 16*16*4)
	if err := c.ReadTexture(tex, TextureRegion{Box: Box{Width: 16, Height: 16, Depth: 1}}, out, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	if got := pixel(out, 16, 2, 2); got != [4]byte{255, 0, 0, 255} {
		t.Errorf("the first triangle: %v", got)
	}
	if got := pixel(out, 16, 13, 13); got != [4]byte{0, 255, 0, 255} {
		t.Errorf("the second triangle: %v", got)
	}
	if err := d.CheckIndirect(commands, 2, 1, 0, 16); err == nil {
		t.Errorf("a command at an offset of no multiple of 4 was accepted")
	}
	if err := d.CheckIndirect(commands, 48, 1, 0, 20); err == nil {
		t.Errorf("a command past its buffer was accepted")
	}
}

// TestStencilTexturing samples the stencil of a depth and stencil texture,
// as GL's stencil texturing does, of a view of its format of stencil
// alone, and its depth, of the texture's.
func TestStencilTexturing(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	const z24s8, x24s8 = 19, 136
	tex, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: z24s8, Width: 4, Height: 4})
	if err != nil {
		t.Fatalf("NewTexture: %v", err)
	}
	defer d.DestroyTexture(tex)
	if _, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: x24s8, Width: 4, Height: 4}); err == nil {
		t.Errorf("a texture of a format of views alone was created")
	}
	s, err := d.NewSurface(tex, z24s8, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	defer d.DestroySurface(s)
	if err := c.ClearSurface(s, ClearDepth|ClearStencil, [4]uint32{}, 0.25, 0x5a, 0, 0, 4, 4); err != nil {
		t.Fatalf("ClearSurface: %v", err)
	}
	c.Flush().Wait()
	sampler, err := d.NewSamplerOf(&SamplerState{MaxLod: 1000})
	if err != nil {
		t.Fatalf("NewSamplerOf: %v", err)
	}
	defer d.DestroySampler(sampler)
	full := [][8]float32{{-1, -1, 0, 1}, {3, -1, 0, 1}, {-1, 3, 0, 1}}
	for _, tc := range []struct {
		format uint32
		typ    string
		want   uint32
	}{
		{x24s8, "UINT", 0x5a},
		{z24s8, "FLOAT", math.Float32bits(0.25)},
	} {
		view, err := d.NewSamplerView(tex, ViewDesc{Format: tc.format, Target: Target2D, Swizzle: [4]uint32{0, 1, 2, 3}})
		if err != nil {
			t.Fatalf("NewSamplerView(%d): %v", tc.format, err)
		}
		px := render(t, d, passVS, `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, `+tc.typ+`
DCL TEMP[0]
IMM[0] INT32 {1, 1, 0, 0}
  0: TXF TEMP[0], IMM[0], SAMP[0], 2D
  1: MOV OUT[0], TEMP[0]
  2: END
`, full, 1, targetRGBA32U, nil, TextureBinding{Stage: tgsi.ProcessorFragment, View: view, Sampler: sampler})
		d.DestroySamplerView(view)
		got := binary.LittleEndian.Uint32(px)
		if tc.format == z24s8 {
			// Of 24 bits of depth, of floats' if the device has them in
			// its place.
			if g := math.Float32frombits(got); math.Abs(float64(g)-0.25) > 1e-6 {
				t.Errorf("the depth: %v, want 0.25", g)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("the stencil: %#x, want %#x", got, tc.want)
		}
	}
}

// TestMultisampleFetch fetches a sample of a multisampled texture.
func TestMultisampleFetch(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	tex, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, Samples: 4})
	if err != nil {
		t.Skipf("no textures of 4 samples: %v", err)
	}
	defer d.DestroyTexture(tex)
	s, err := d.NewSurface(tex, virglR8G8B8A8Unorm, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	defer d.DestroySurface(s)
	if err := c.ClearSurface(s, ClearColor0, [4]uint32{0, math.Float32bits(1), 0, math.Float32bits(1)}, 0, 0, 0, 0, 4, 4); err != nil {
		t.Fatalf("ClearSurface: %v", err)
	}
	c.Flush().Wait()
	view, err := d.NewSamplerView(tex, ViewDesc{Format: virglR8G8B8A8Unorm, Target: Target2D, Swizzle: [4]uint32{0, 1, 2, 3}})
	if err != nil {
		t.Fatalf("NewSamplerView: %v", err)
	}
	defer d.DestroySamplerView(view)
	sampler, err := d.NewSamplerOf(&SamplerState{MaxLod: 1000})
	if err != nil {
		t.Fatalf("NewSamplerOf: %v", err)
	}
	defer d.DestroySampler(sampler)
	full := [][8]float32{{-1, -1, 0, 1}, {3, -1, 0, 1}, {-1, 3, 0, 1}}
	px := render(t, d, passVS, `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D_MSAA, FLOAT
DCL TEMP[0..1]
IMM[0] INT32 {1, 2, 0, 3}
IMM[1] INT32 {4, 0, 0, 0}
IMM[2] FLT32 {    1.0000,     0.0000,     0.0000,     0.0000}
  0: TXF TEMP[0], IMM[0], SAMP[0], 2D_MSAA
  1: TXQS TEMP[1].x, SAMP[0], 2D_MSAA
  2: USEQ TEMP[1].x, TEMP[1].xxxx, IMM[1].xxxx
  3: AND TEMP[0].x, TEMP[1].xxxx, IMM[2].xxxx
  4: MOV OUT[0], TEMP[0]
  5: END
`, full, 1, targetRGBA8, nil, TextureBinding{Stage: tgsi.ProcessorFragment, View: view, Sampler: sampler})
	// The sample's green, and red of the texture's 4 samples.
	if got := pixel(px, 1, 0, 0); got != [4]byte{255, 255, 0, 255} {
		t.Errorf("the sample: %v, want {255, 255, 0, 255}", got)
	}
}
