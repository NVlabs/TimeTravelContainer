// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// Vulkan's formats that the tests use.
const (
	formatR8G8B8A8Unorm      = 37
	formatR32G32B32A32Uint   = 107
	formatR32G32B32A32Sfloat = 109
)

func newTestDevice(t *testing.T) *Device {
	t.Helper()
	d, err := NewDevice()
	if err != nil {
		t.Skipf("no Vulkan device: %v", err)
	}
	t.Cleanup(d.Destroy)
	t.Logf("device: %s", d.Name)
	return d
}

func translate(t *testing.T, text string, opts tospirv.Options) *tospirv.Result {
	t.Helper()
	s, err := tgsi.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := tospirv.Translate(s, opts)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return res
}

// target is a render target's format, and its texels' size and type.
type target struct {
	format uint32
	texel  uint32
	typ    tospirv.BaseType
}

var (
	targetRGBA8   = target{formatR8G8B8A8Unorm, 4, tospirv.Float}
	targetRGBA32U = target{formatR32G32B32A32Uint, 16, tospirv.Uint}
)

// render draws vertices, of a position and a color each, with the shaders
// vs and fs, to an image of size by size of tgt, cleared to 0, of the
// fragment shader's constant buffer 0 of constants, and its textures, and
// returns its texels.
func render(t *testing.T, d *Device, vs, fs string, vertices [][8]float32, size uint32, tgt target, constants []uint32,
	textures ...TextureBinding) []byte {
	t.Helper()
	v := translate(t, vs, tospirv.Options{})
	fopts := tospirv.Options{ColorBuffers: 1}
	fopts.ColorTypes[0] = tgt.typ
	f := translate(t, fs, fopts)
	Link(v, f)
	var resources []StageResource
	for _, r := range v.Resources {
		resources = append(resources, StageResource{tgsi.ProcessorVertex, r})
	}
	for _, r := range f.Resources {
		resources = append(resources, StageResource{tgsi.ProcessorFragment, r})
	}
	p, err := d.NewGraphicsPipeline(GraphicsDesc{
		VS: v.Words, FS: f.Words, Stride: 32,
		Attributes:  []VertexAttribute{{0, formatR32G32B32A32Sfloat, 0}, {1, formatR32G32B32A32Sfloat, 16}},
		ColorFormat: tgt.format,
		Resources:   resources,
	})
	if err != nil {
		t.Fatalf("NewGraphicsPipeline: %v", err)
	}
	defer d.DestroyPipeline(p)
	vb, err := d.NewBuffer(uint64(32*len(vertices)), BufferVertex, true)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(vb)
	b := vb.Bytes()
	for i, vert := range vertices {
		for j, f := range vert {
			binary.LittleEndian.PutUint32(b[32*i+4*j:], math.Float32bits(f))
		}
	}
	img, err := d.NewImage(tgt.format, size, size, ImageColorAttachment|ImageTransferSrc)
	if err != nil {
		t.Fatalf("NewImage: %v", err)
	}
	defer d.DestroyImage(img)
	out, err := d.NewBuffer(uint64(tgt.texel*size*size), BufferTransferDst, true)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(out)
	var ubo *Buffer
	if len(constants) != 0 {
		if ubo, err = d.NewBuffer(uint64(4*len(constants)), BufferUniform, true); err != nil {
			t.Fatalf("NewBuffer: %v", err)
		}
		defer d.DestroyBuffer(ubo)
		b := ubo.Bytes()
		for i, c := range constants {
			binary.LittleEndian.PutUint32(b[4*i:], c)
		}
	}
	if err := d.Record(func(c *Commands) error {
		c.ImageLayout(img, ImageLayoutColorAttachmentOptimal, StageTopOfPipe, 0,
			StageColorAttachmentOutput, AccessColorAttachmentWrite)
		c.BeginRendering(img, [4]float32{0, 0, 0, 0})
		c.BindPipeline(p)
		c.Viewport(0, 0, float32(size), float32(size), size, size)
		c.BindVertexBuffer(vb, 0)
		var buffers []BufferBinding
		if ubo != nil {
			buffers = append(buffers, BufferBinding{tgsi.ProcessorFragment, tospirv.UniformBuffer, 0, ubo, 0, ubo.size})
		}
		if err := c.Bind(p, buffers, textures); err != nil {
			return err
		}
		c.PushConstants(p, 0, make([]byte, tospirv.PushConstantsSize))
		c.Draw(uint32(len(vertices)), 1)
		c.EndRendering()
		c.ImageLayout(img, ImageLayoutTransferSrcOptimal, StageColorAttachmentOutput, AccessColorAttachmentWrite,
			StageTransfer, AccessTransferRead)
		c.CopyImageToBuffer(img, out)
		return nil
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return append([]byte(nil), out.Bytes()...)
}

func pixel(pixels []byte, size, x, y uint32) [4]byte {
	o := 4 * (y*size + x)
	return [4]byte{pixels[o], pixels[o+1], pixels[o+2], pixels[o+3]}
}

const passVS = `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[1]
  2: END
`

const colorFS = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
  0: MOV OUT[0], IN[0]
  1: END
`

// TestTriangle renders a triangle of the top left half of the image, red,
// of TGSI's shaders, translated.
func TestTriangle(t *testing.T) {
	d := newTestDevice(t)
	red := [4]float32{1, 0, 0, 1}
	vert := func(x, y float32) [8]float32 {
		return [8]float32{x, y, 0, 1, red[0], red[1], red[2], red[3]}
	}
	const size = 32
	px := render(t, d, passVS, colorFS, [][8]float32{vert(-1, -1), vert(1, -1), vert(-1, 1)}, size, targetRGBA8, nil)
	if got := pixel(px, size, 2, 2); got != [4]byte{255, 0, 0, 255} {
		t.Errorf("top left: %v", got)
	}
	if got := pixel(px, size, size-3, size-3); got != [4]byte{0, 0, 0, 0} {
		t.Errorf("bottom right: %v", got)
	}
}

// TestNewTexture creates textures of each target, of formats of colors and
// of depth, and of formats that Vulkan emulates.
func TestNewTexture(t *testing.T) {
	d := newTestDevice(t)
	for _, desc := range []ResourceDesc{
		{Target: Target2D, Format: 67, Width: 64, Height: 32, LastLevel: 6}, // R8G8B8A8_UNORM
		{Target: Target1DArray, Format: 1, Width: 64, ArraySize: 3},
		{Target: Target3D, Format: 31, Width: 8, Height: 8, Depth: 8, LastLevel: 3}, // R32G32B32A32_FLOAT
		{Target: TargetCube, Format: 67, Width: 16, Height: 16, ArraySize: 6, LastLevel: 4},
		{Target: TargetCubeArray, Format: 67, Width: 16, Height: 16, ArraySize: 12},
		{Target: Target2DArray, Format: 67, Width: 16, Height: 16, ArraySize: 2, Samples: 4},
		{Target: Target2D, Format: 19, Width: 64, Height: 64}, // Z24_UNORM_S8_UINT
		{Target: Target2D, Format: 10, Width: 64, Height: 64}, // A8_UNORM, as R8
		{Target: TargetRect, Format: 2, Width: 100, Height: 30},
	} {
		tex, err := d.NewTexture(desc)
		if err != nil {
			t.Errorf("NewTexture(%+v): %v", desc, err)
			continue
		}
		d.DestroyTexture(tex)
	}
	if _, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: 67, Width: 64, Height: 64, LastLevel: 9}); err == nil {
		t.Errorf("a texture of more levels than its size has was created")
	}
}

// TestContextBuffers writes a buffer and reads it back through a context's
// batches, in order of its commands.
func TestContextBuffers(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	b, err := d.NewBuffer(1024, BufferTransferSrc|BufferTransferDst|BufferVertex, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(b)
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := c.WriteBuffer(b, 24, data); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := c.WriteBuffer(b, 24, data[:4]); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	out := make([]byte, 1000)
	if err := c.ReadBuffer(b, 24, out); err != nil {
		t.Fatalf("ReadBuffer: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Errorf("read %v..., want %v...", out[:8], data[:8])
	}
	if err := c.WriteBuffer(b, 1000, data[:25]); err == nil {
		t.Errorf("a write past the buffer was accepted")
	}
	c.Flush().Wait()
}

// TestContextTextures writes regions of textures of each kind of layout,
// of the guest's strides, and reads them back.
func TestContextTextures(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	for _, tc := range []struct {
		desc ResourceDesc
		r    TextureRegion
	}{
		{ResourceDesc{Target: Target2D, Format: 67, Width: 16, Height: 8, LastLevel: 1},
			TextureRegion{Level: 0, Box: Box{X: 3, Y: 2, Width: 5, Height: 4, Depth: 1}, Stride: 24}},
		{ResourceDesc{Target: Target2D, Format: 67, Width: 16, Height: 8, LastLevel: 1},
			TextureRegion{Level: 1, Box: Box{X: 0, Y: 0, Width: 8, Height: 4, Depth: 1}}},
		{ResourceDesc{Target: Target2DArray, Format: 31, Width: 4, Height: 4, ArraySize: 3},
			TextureRegion{Box: Box{X: 1, Y: 1, Z: 1, Width: 2, Height: 2, Depth: 2}, Stride: 40, LayerStride: 100}},
		// Whose layers are the box's z, as guests address them.
		{ResourceDesc{Target: Target1DArray, Format: 67, Width: 8, ArraySize: 4},
			TextureRegion{Box: Box{X: 2, Z: 1, Width: 4, Height: 1, Depth: 3}}},
		{ResourceDesc{Target: Target3D, Format: 64, Width: 4, Height: 4, Depth: 4}, // R8_UNORM
			TextureRegion{Box: Box{Y: 1, Z: 2, Width: 4, Height: 3, Depth: 2}}},
		{ResourceDesc{Target: TargetCube, Format: 67, Width: 4, Height: 4, ArraySize: 6},
			TextureRegion{Box: Box{Z: 3, Width: 4, Height: 4, Depth: 3}}},
	} {
		tex, err := d.NewTexture(tc.desc)
		if err != nil {
			t.Fatalf("NewTexture(%+v): %v", tc.desc, err)
		}
		stride, layerStride, _, _, span := tex.memLayout(tc.r)
		data := make([]byte, 7+span)
		for i := range data {
			data[i] = byte(i*13 + 1)
		}
		if err := c.WriteTexture(tex, tc.r, data, 7); err != nil {
			t.Fatalf("WriteTexture(%+v): %v", tc.desc, err)
		}
		out := make([]byte, len(data))
		if err := c.ReadTexture(tex, tc.r, out, 7); err != nil {
			t.Fatalf("ReadTexture(%+v): %v", tc.desc, err)
		}
		// The rows that the region spans read back as written; the gaps
		// between them are untouched.
		_, _, row, rows, _ := tex.memLayout(tc.r)
		for l := uint64(0); l < uint64(tc.r.Box.Depth); l++ {
			for y := uint64(0); y < rows; y++ {
				o := 7 + l*layerStride + y*stride
				if !bytes.Equal(out[o:o+row], data[o:o+row]) {
					t.Errorf("%+v: layer %d row %d: %v, want %v", tc.desc, l, y, out[o:o+row], data[o:o+row])
				}
			}
		}
		d.DestroyTexture(tex)
	}
	// Of no samples of multisampled textures, which Vulkan copies none of.
	ms, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: 67, Width: 4, Height: 4, Samples: 4})
	if err != nil {
		t.Logf("no textures of 4 samples: %v", err)
		return
	}
	defer d.DestroyTexture(ms)
	if err := c.WriteTexture(ms, TextureRegion{Box: Box{Width: 4, Height: 4, Depth: 1}}, make([]byte, 64), 0); err == nil {
		t.Errorf("a transfer of a multisampled texture: no error")
	}
}

// TestCaps fills the device's capability sets.
func TestCaps(t *testing.T) {
	d := newTestDevice(t)
	v1 := d.Caps(1, 1, 4)
	v2 := d.Caps(2, 1, 4)
	if len(v1) != 308 || len(v2) != 1408 {
		t.Fatalf("caps of %d and %d bytes", len(v1), len(v2))
	}
	var c capsV2
	if err := binary.Read(bytes.NewReader(v2), binary.LittleEndian, &c); err != nil {
		t.Fatal(err)
	}
	// R8G8B8A8_UNORM samples and renders, and Z24_UNORM_S8_UINT is a
	// depth-stencil format.
	if !c.V1.Sampler.has(67) || !c.V1.Render.has(67) || !c.V1.DepthStencil.has(19) || !c.V1.VertexBuffer.has(31) ||
		c.V1.GLSLLevel != 450 || c.V1.MaxRenderTargets == 0 || c.MaxTexture2DSize == 0 || c.HostFeatureCheckVersion != 23 {
		t.Errorf("caps %+v", c.V1)
	}
	t.Logf("renderer %q, %d render targets, %d samples, prim mask %#x", string(bytes.TrimRight(c.Renderer[:], "\x00")),
		c.V1.MaxRenderTargets, c.V1.MaxSamples, c.V1.PrimMask)
}
