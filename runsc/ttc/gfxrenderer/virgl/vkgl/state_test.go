// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestDecodeBlend(t *testing.T) {
	d := make([]uint32, 3+MaxColorBuffers)
	d[1] = 1 | 1<<1 // independent blending, logic op
	d[2] = 6        // XOR
	// Target 0 blends SRC_ALPHA, INV_SRC_ALPHA, of RGB, and ONE, ZERO of
	// alpha, writing RGB; target 1 does not blend.
	d[3] = 1 | 0<<1 | 0x03<<4 | 0x13<<9 | 0<<14 | 0x01<<17 | 0x11<<22 | 7<<27
	d[4] = 0xf << 27
	s, err := DecodeBlend(d)
	if err != nil {
		t.Fatal(err)
	}
	rt := s.RT[0]
	if !s.IndependentBlend || !s.LogicOpEnable || VkLogicOp(s.LogicOp) != 6 || !rt.Enable || rt.ColorMask != 7 ||
		VkBlendFactor(rt.RGBSrc) != 6 || VkBlendFactor(rt.RGBDst) != 7 ||
		VkBlendFactor(rt.AlphaSrc) != 1 || VkBlendFactor(rt.AlphaDst) != 0 {
		t.Errorf("blend %+v", s)
	}
	if s.RT[1].Enable || s.RT[1].ColorMask != 0xf || s.RT[1].RGBDst != pipeBlendFactorZero {
		t.Errorf("target 1 %+v", s.RT[1])
	}
	// INV_SRC_ALPHA_SATURATE is none of Gallium's factors.
	d[3] = 1 | 0x16<<4
	if _, err := DecodeBlend(d); err == nil {
		t.Errorf("a blend factor of 0x16 was accepted")
	}
	if _, err := DecodeBlend(d[:4]); err == nil {
		t.Errorf("a short blend state was accepted")
	}
}

func TestDecodeRasterizerAndDSA(t *testing.T) {
	r, err := DecodeRasterizer([]uint32{0, 1 | 1<<15 | 2<<8 | 1<<10, math.Float32bits(4), 1 << 3, 3 << 24,
		math.Float32bits(float32(math.NaN())), 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !r.FlatShade || !r.FrontCCW || r.CullFace != 2 || r.FillFront != 1 || r.PointSize != 4 ||
		r.SpriteCoordEnable != 8 || r.ClipPlaneEnable != 3 || r.LineWidth != 1 {
		t.Errorf("rasterizer %+v", r)
	}
	if _, err := DecodeRasterizer([]uint32{0, 3 << 10, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Errorf("a fill mode of 3 was accepted")
	}
	s, err := DecodeDSA([]uint32{0, 1 | 1<<1 | 3<<2 | 1<<8 | 4<<9, 1 | 2<<1 | 5<<4 | 0xff<<13 | 0x0f<<21, 0,
		math.Float32bits(0.5)})
	if err != nil {
		t.Fatal(err)
	}
	f := s.Stencil[0]
	if !s.DepthEnable || !s.DepthWrite || s.DepthFunc != 3 || !s.AlphaEnable || s.AlphaFunc != 4 || s.AlphaRef != 0.5 ||
		!f.Enable || f.Func != 2 || VkStencilOp(f.FailOp) != 6 || f.ValueMask != 0xff || f.WriteMask != 0x0f {
		t.Errorf("DSA %+v", s)
	}
}

func TestDecodeSamplerState(t *testing.T) {
	s, err := DecodeSamplerState([]uint32{0, WrapClamp | WrapMirrorClampToEdge<<3 | WrapClampToBorder<<6 | 1<<9 | 1<<11,
		0, math.Float32bits(2), math.Float32bits(1), 1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if VkAddressMode(s.WrapS) != 2 || VkAddressMode(s.WrapT) != 4 || VkAddressMode(s.WrapR) != 3 ||
		s.MinFilter != 1 || s.MipFilter != MipFilterLinear || s.MinLod != 2 || s.MaxLod != 2 ||
		s.BorderColor != [4]uint32{1, 2, 3, 4} {
		t.Errorf("sampler %+v", s)
	}
	if _, err := DecodeSamplerState([]uint32{0, 3 << 11, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Errorf("a mip filter of 3 was accepted")
	}
}

func TestFormats(t *testing.T) {
	for _, c := range []struct {
		virgl          uint32
		vk             uint32
		swizzle        string
		depth, stencil bool
		integer        bool
		bytes, block   uint32
	}{
		{1, 44, "XYZW", false, false, false, 4, 1},    // B8G8R8A8_UNORM
		{2, 44, "XYZ1", false, false, false, 4, 1},    // B8G8R8X8_UNORM, of B8G8R8A8's
		{10, 9, "000X", false, false, false, 1, 1},    // A8_UNORM, of R8's
		{19, 129, "XYZW", true, true, false, 4, 1},    // Z24_UNORM_S8_UINT
		{177, 13, "XYZW", false, false, true, 1, 1},   // R8_UINT
		{105, 131, "XYZW", false, false, false, 8, 4}, // DXT1_RGB, BC1
	} {
		f, ok := LookupFormat(c.virgl)
		if !ok {
			t.Errorf("no format %d", c.virgl)
			continue
		}
		integer, _ := f.IsInteger()
		if f.VK != c.vk || f.Swizzle != c.swizzle || f.IsDepth() != c.depth || f.IsStencil() != c.stencil ||
			integer != c.integer || f.BlockBytes != c.bytes || f.BlockWidth != c.block {
			t.Errorf("format %d: %+v", c.virgl, f)
		}
	}
	if _, ok := LookupFormat(0); ok {
		t.Errorf("format 0, which is none, is")
	}
}

func TestResourceChecks(t *testing.T) {
	l := Limits{Max2DSize: 16384, MaxCubeLevels: 15, Max3DLevels: 12, MaxArrayLayers: 2048}
	for _, c := range []struct {
		d  ResourceDesc
		ok bool
	}{
		{ResourceDesc{Target: TargetBuffer, Width: 0}, true},
		{ResourceDesc{Target: TargetBuffer, Width: 64, LastLevel: 1}, false},
		{ResourceDesc{Target: Target2D, Width: 64, Height: 32, LastLevel: 6}, true},
		{ResourceDesc{Target: Target2D, Width: 64, Height: 32, LastLevel: 7}, false},
		{ResourceDesc{Target: Target2D, Width: 64, Height: 64, Samples: 4}, true},
		{ResourceDesc{Target: Target2D, Width: 64, Height: 64, Samples: 3}, false},
		{ResourceDesc{Target: Target3D, Width: 64, Height: 64, Depth: 4, Samples: 4}, false},
		{ResourceDesc{Target: TargetCube, Width: 64, Height: 64, ArraySize: 6}, true},
		{ResourceDesc{Target: TargetCube, Width: 64, Height: 32, ArraySize: 6}, false},
		{ResourceDesc{Target: TargetCubeArray, Width: 8, Height: 8, ArraySize: 12}, true},
		{ResourceDesc{Target: TargetCubeArray, Width: 8, Height: 8, ArraySize: 8}, false},
		{ResourceDesc{Target: Target1D, Width: 64, Height: 2}, false},
		{ResourceDesc{Target: Target2D, Width: 64, Height: 64, ArraySize: 2}, false},
		{ResourceDesc{Target: TargetRect, Width: 64, Height: 64, LastLevel: 1}, false},
		{ResourceDesc{Target: Target2D, Width: 1 << 15, Height: 1}, false},
	} {
		if err := c.d.normalize().check(l); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.d, err)
		}
	}
	d := ResourceDesc{Target: Target1DArray, Width: 64, ArraySize: 4, LastLevel: 2}.normalize()
	if w, h, depth := d.levelExtent(2); w != 16 || h != 1 || depth != 4 {
		t.Errorf("a 1D array's level 2: %d %d %d", w, h, depth)
	}
	// Whose layers are boxes' z, as guests address them.
	if !d.boxOK(1, Box{X: 0, Y: 0, Z: 1, Width: 32, Height: 1, Depth: 3}) || d.boxOK(1, Box{Y: 1, Width: 32, Height: 1, Depth: 1}) ||
		d.boxOK(1, Box{Width: 33, Height: 1, Depth: 1}) ||
		d.boxOK(3, Box{Width: 1, Height: 1, Depth: 1}) || !d.boxOK(0, Box{X: 64, Width: -64, Height: 1, Depth: 1}) {
		t.Errorf("boxes of %+v", d)
	}
}

// TestCapsLayout checks the capability sets' sizes and fields' offsets of
// virgl_hw.h's, which a C program of Mesa 26.2.4's measured.
func TestCapsLayout(t *testing.T) {
	var c capsV2
	if n := binary.Size(capsV1{}); n != 308 {
		t.Errorf("struct virgl_caps_v1 of %d bytes, want 308", n)
	}
	if n := binary.Size(c); n != 1408 {
		t.Errorf("struct virgl_caps_v2 of %d bytes, want 1408", n)
	}
	// Of fields' offsets.
	b := encodeCaps(&capsV2{V1: capsV1{BoolSet1: 0x11, GLSLLevel: 0x22}, Scanout: formatMask{0x33},
		Renderer: [64]byte{0x44}, VideoCaps: [32][4]uint32{{0x55}}, MaxShaderStorageBlocks: [6]uint32{0x66}})
	for off, want := range map[int]byte{260: 0x11, 264: 0x22, 624: 0x33, 696: 0x44, 860: 0x55, 1384: 0x66} {
		if b[off] != want {
			t.Errorf("byte %d is %#x, want %#x", off, b[off], want)
		}
	}
}

// TestVkColor gives clears' colors of emulated formats as their Vulkan
// formats' channels.
func TestVkColor(t *testing.T) {
	color := [4]uint32{1, 2, 3, 4}
	for _, tc := range []struct {
		format uint32
		want   [4]uint32
	}{
		{67, [4]uint32{1, 2, 3, 4}}, // R8G8B8A8_UNORM
		{10, [4]uint32{4, 0, 0, 0}}, // A8_UNORM, as R8's red
		{9, [4]uint32{1, 0, 0, 0}},  // L8_UNORM, as R8's red
		{12, [4]uint32{1, 4, 0, 0}}, // L8A8_UNORM, as R8G8's
		{2, [4]uint32{1, 2, 3, 0}},  // B8G8R8X8_UNORM, of no alpha
	} {
		f, ok := LookupFormat(tc.format)
		if !ok {
			t.Fatalf("no format %d", tc.format)
		}
		if got := f.vkColor(color); got != tc.want {
			t.Errorf("%s: %v, want %v", f.Name, got, tc.want)
		}
	}
}
