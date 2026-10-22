// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

const virglB8G8R8A8Unorm = 1

// TestContextBlits blits of Vulkan's blits, scaled, flipped, and resolved
// of samples, and refuses blits that they do not do.
func TestContextBlits(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	texture := func(desc ResourceDesc) *Texture {
		t.Helper()
		tex, err := d.NewTexture(desc)
		if err != nil {
			t.Fatalf("NewTexture(%+v): %v", desc, err)
		}
		t.Cleanup(func() { d.DestroyTexture(tex) })
		return tex
	}
	region := func(level uint32, w, h int64) TextureRegion {
		return TextureRegion{Level: level, Box: Box{Width: w, Height: h, Depth: 1}}
	}
	rgba := func(w, h, levels uint32) *Texture {
		return texture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: w, Height: h, LastLevel: levels - 1})
	}
	blit := func(dst, src *Texture, dstBox, srcBox Box, linear bool) error {
		return c.Blit(Blit{Dst: dst, Src: src, DstLevel: 0, SrcLevel: 0, DstFormat: virglR8G8B8A8Unorm,
			SrcFormat: virglR8G8B8A8Unorm, DstBox: dstBox, SrcBox: srcBox, Mask: MaskRGBA, Linear: linear})
	}

	// A level of a color to the next, halved, linearly, as mipmaps are
	// made.
	mip := rgba(8, 8, 2)
	gray := bytes.Repeat([]byte{100, 150, 200, 255}, 64)
	if err := c.WriteTexture(mip, region(0, 8, 8), gray, 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	if err := c.Blit(Blit{Dst: mip, Src: mip, DstLevel: 1, SrcLevel: 0, DstFormat: virglR8G8B8A8Unorm,
		SrcFormat: virglR8G8B8A8Unorm, DstBox: Box{Width: 4, Height: 4, Depth: 1}, SrcBox: Box{Width: 8, Height: 8, Depth: 1},
		Mask: MaskRGBA, Linear: true}); err != nil {
		t.Fatalf("Blit of a level: %v", err)
	}
	got := make([]byte, 64)
	if err := c.ReadTexture(mip, region(1, 4, 4), got, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	if !bytes.Equal(got, gray[:64]) {
		t.Errorf("level 1: % x, want % x", got, gray[:64])
	}

	// Flipped, of texels each their own.
	src, dst := rgba(4, 4, 1), rgba(4, 4, 1)
	texels := make([]byte, 64)
	for i := range texels {
		texels[i] = byte(i)
	}
	if err := c.WriteTexture(src, region(0, 4, 4), texels, 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	if err := blit(dst, src, Box{Width: 4, Height: 4, Depth: 1}, Box{Y: 4, Width: 4, Height: -4, Depth: 1}, false); err != nil {
		t.Fatalf("Blit flipped: %v", err)
	}
	if err := c.ReadTexture(dst, region(0, 4, 4), got, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	for y := 0; y < 4; y++ {
		if row, want := got[16*y:16*y+16], texels[16*(3-y):16*(3-y)+16]; !bytes.Equal(row, want) {
			t.Errorf("flipped row %d: % x, want % x", y, row, want)
		}
	}

	// Resolved, of a framebuffer of 4 samples cleared to red.
	if max4 := d.props.Limits.FramebufferColorSampleCounts&4 != 0; !max4 {
		t.Logf("no framebuffers of 4 samples")
	} else {
		ms := texture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, Samples: 4})
		s, err := d.NewSurface(ms, virglR8G8B8A8Unorm, 0, 0, 0)
		if err != nil {
			t.Fatalf("NewSurface: %v", err)
		}
		defer d.DestroySurface(s)
		c.SetFramebuffer(Framebuffer{Colors: []*Surface{s}})
		one := math.Float32bits(1)
		if err := c.Clear(ClearColor0, [4]uint32{one, 0, 0, one}, 0, 0); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		c.SetFramebuffer(Framebuffer{})
		box := Box{Width: 4, Height: 4, Depth: 1}
		if err := blit(dst, ms, box, box, false); err != nil {
			t.Fatalf("Blit of samples: %v", err)
		}
		if err := c.ReadTexture(dst, region(0, 4, 4), got, 0); err != nil {
			t.Fatalf("ReadTexture: %v", err)
		}
		if want := bytes.Repeat([]byte{255, 0, 0, 255}, 16); !bytes.Equal(got, want) {
			t.Errorf("resolved: % x, want % x", got, want)
		}
	}

	whole := Box{Width: 4, Height: 4, Depth: 1}
	// Of the blitter: of views of formats of their own, of some channels,
	// and of a scissor.
	fill := func(tex *Texture, texel []byte) {
		t.Helper()
		if err := c.WriteTexture(tex, region(0, 4, 4), bytes.Repeat(texel, 16), 0); err != nil {
			t.Fatalf("WriteTexture: %v", err)
		}
	}
	fill(src, []byte{1, 2, 3, 4})
	for _, tc := range []struct {
		name string
		b    Blit
		want []byte
	}{
		{"as BGRA", Blit{Dst: dst, Src: src, DstFormat: virglB8G8R8A8Unorm, SrcFormat: virglR8G8B8A8Unorm,
			DstBox: whole, SrcBox: whole, Mask: MaskRGBA}, bytes.Repeat([]byte{3, 2, 1, 4}, 16)},
		{"of red, green and blue", Blit{Dst: dst, Src: src, DstFormat: virglR8G8B8A8Unorm, SrcFormat: virglR8G8B8A8Unorm,
			DstBox: whole, SrcBox: whole, Mask: 0x7}, bytes.Repeat([]byte{1, 2, 3, 9}, 16)},
		{"of a scissor", Blit{Dst: dst, Src: src, DstFormat: virglR8G8B8A8Unorm, SrcFormat: virglR8G8B8A8Unorm,
			DstBox: whole, SrcBox: whole, Mask: MaskRGBA, Scissor: &Scissor{MinX: 1, MinY: 1, MaxX: 3, MaxY: 3}}, nil},
	} {
		fill(dst, []byte{9, 9, 9, 9})
		if err := c.Blit(tc.b); err != nil {
			t.Fatalf("a blit %s: %v", tc.name, err)
		}
		if err := c.ReadTexture(dst, region(0, 4, 4), got, 0); err != nil {
			t.Fatalf("ReadTexture: %v", err)
		}
		want := tc.want
		if want == nil {
			want = bytes.Repeat([]byte{9, 9, 9, 9}, 16)
			for y := 1; y < 3; y++ {
				for x := 1; x < 3; x++ {
					copy(want[4*(4*y+x):], []byte{1, 2, 3, 4})
				}
			}
		}
		if !bytes.Equal(got, want) {
			t.Errorf("a blit %s: % x, want % x", tc.name, got, want)
		}
	}

	// Of depth, of a scissor, which Vulkan's blits have not.
	const virglZ32Float = 18
	zsrc := texture(ResourceDesc{Target: Target2D, Format: virglZ32Float, Width: 4, Height: 4})
	zdst := texture(ResourceDesc{Target: Target2D, Format: virglZ32Float, Width: 4, Height: 4})
	for _, z := range []struct {
		tex   *Texture
		depth float64
	}{{zsrc, 0.25}, {zdst, 1}} {
		s, err := d.NewSurface(z.tex, virglZ32Float, 0, 0, 0)
		if err != nil {
			t.Fatalf("NewSurface: %v", err)
		}
		defer d.DestroySurface(s)
		if err := c.ClearSurface(s, ClearDepth, [4]uint32{}, z.depth, 0, 0, 0, 4, 4); err != nil {
			t.Fatalf("ClearSurface: %v", err)
		}
	}
	if err := c.Blit(Blit{Dst: zdst, Src: zsrc, DstFormat: virglZ32Float, SrcFormat: virglZ32Float, DstBox: whole,
		SrcBox: whole, Mask: MaskZ, Scissor: &Scissor{MinX: 1, MinY: 1, MaxX: 3, MaxY: 3}}); err != nil {
		t.Fatalf("a blit of depth: %v", err)
	}
	if err := c.ReadTexture(zdst, region(0, 4, 4), got, 0); err != nil {
		t.Fatalf("ReadTexture: %v", err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			want := float32(1)
			if x >= 1 && x < 3 && y >= 1 && y < 3 {
				want = 0.25
			}
			if z := math.Float32frombits(binary.LittleEndian.Uint32(got[4*(4*y+x):])); z != want {
				t.Errorf("depth %d,%d: %v, want %v", x, y, z, want)
			}
		}
	}

	// Of invalid blits.
	for _, tc := range []struct {
		name string
		b    Blit
	}{
		{"past the source", Blit{Dst: dst, Src: src, DstFormat: virglR8G8B8A8Unorm, SrcFormat: virglR8G8B8A8Unorm,
			DstBox: whole, SrcBox: Box{X: 1, Width: 4, Height: 4, Depth: 1}, Mask: MaskRGBA}},
		{"of depth of colors", Blit{Dst: dst, Src: src, DstFormat: virglR8G8B8A8Unorm, SrcFormat: virglR8G8B8A8Unorm,
			DstBox: whole, SrcBox: whole, Mask: MaskZ}},
	} {
		if err := c.Blit(tc.b); err == nil || errors.Is(err, ErrNotNative) {
			t.Errorf("a blit %s: %v", tc.name, err)
		}
	}
}

// TestBlitterShaders translates the blitter's shaders, of each target,
// type, filter and output.
func TestBlitterShaders(t *testing.T) {
	if _, err := translateTGSI(blitVS, tospirv.Options{}); err != nil {
		t.Fatalf("the vertex shader: %v", err)
	}
	for _, target := range []string{"1D_ARRAY", "2D_ARRAY", "RECT", "3D"} {
		for _, typ := range []string{"FLOAT", "SINT", "UINT"} {
			for _, linear := range []bool{false, true} {
				for _, depth := range []bool{false, true} {
					if (linear || depth) && typ != "FLOAT" {
						continue
					}
					opts := tospirv.Options{ColorBuffers: 1}
					text := blitFS(target, typ, linear, depth, "wxyz")
					if _, err := translateTGSI(text, opts); err != nil {
						t.Errorf("%s %s linear %v depth %v: %v\n%s", target, typ, linear, depth, err, text)
					}
				}
			}
		}
	}
}

// TestBlitOutput writes the blitter's colors of emulated formats as their
// Vulkan formats' channels, of the channels of a mask: of the channels
// that it writes, the swizzle's.
func TestBlitOutput(t *testing.T) {
	for _, tc := range []struct {
		format, mask uint32
		swizzle      string
		vkMask       uint32
	}{
		{virglR8G8B8A8Unorm, MaskRGBA, "xyzw", 0xf},
		{virglR8G8B8A8Unorm, 0x7, "xyz", 0x7},
		{virglA8Unorm, MaskRGBA, "w", 0x1},
		{virglA8Unorm, 0x7, "", 0},
		{12, MaskRGBA, "xw", 0x3}, // L8A8_UNORM, of luminance as red
	} {
		f, _ := LookupFormat(tc.format)
		swizzle, vkMask := blitOutput(f, tc.mask)
		written := ""
		for k := range 4 {
			if vkMask&(1<<k) != 0 {
				written += swizzle[k : k+1]
			}
		}
		if written != tc.swizzle || vkMask != tc.vkMask {
			t.Errorf("%s of mask %#x: %q (%q) %#x, want %q %#x", f.Name, tc.mask, written, swizzle, vkMask, tc.swizzle, tc.vkMask)
		}
	}
}
