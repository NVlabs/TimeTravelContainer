// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

const (
	virglA8Unorm          = 10
	virglR32G32B32A32Uint = 196
)

// TestContextClears clears surfaces' rects, of formats that Vulkan has and
// that it emulates, and textures' boxes, of copies of rows of a texel.
func TestContextClears(t *testing.T) {
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
	surface := func(tex *Texture, format uint32) *Surface {
		t.Helper()
		s, err := d.NewSurface(tex, format, 0, 0, 0)
		if err != nil {
			t.Fatalf("NewSurface: %v", err)
		}
		t.Cleanup(func() { d.DestroySurface(s) })
		return s
	}
	read := func(tex *Texture, w, h int64, texel int) []byte {
		t.Helper()
		out := make([]byte, int(w*h)*texel)
		if err := c.ReadTexture(tex, TextureRegion{Box: Box{Width: w, Height: h, Depth: 1}}, out, 0); err != nil {
			t.Fatalf("ReadTexture: %v", err)
		}
		return out
	}
	f32 := math.Float32bits

	// A rect of a color surface, after the whole of it.
	rgba := texture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 8, Height: 8})
	s := surface(rgba, virglR8G8B8A8Unorm)
	if err := c.ClearSurface(s, ClearColor0, [4]uint32{f32(1), 0, 0, f32(1)}, 0, 0, 0, 0, 8, 8); err != nil {
		t.Fatalf("ClearSurface: %v", err)
	}
	if err := c.ClearSurface(s, ClearColor0, [4]uint32{0, f32(1), 0, f32(1)}, 0, 0, 2, 2, 4, 4); err != nil {
		t.Fatalf("ClearSurface of a rect: %v", err)
	}
	px := read(rgba, 8, 8, 4)
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			want := [4]byte{255, 0, 0, 255}
			if x >= 2 && x < 6 && y >= 2 && y < 6 {
				want = [4]byte{0, 255, 0, 255}
			}
			if got := [4]byte(px[4*(8*y+x):]); got != want {
				t.Fatalf("texel %d,%d: %v, want %v", x, y, got, want)
			}
		}
	}
	if err := c.ClearSurface(s, ClearColor0, [4]uint32{}, 0, 0, 6, 0, 4, 4); err == nil {
		t.Errorf("a clear of a rect past a surface: no error")
	}

	// Of A8, which Vulkan's R8 emulates: its alpha is R8's red.
	a8 := texture(ResourceDesc{Target: Target2D, Format: virglA8Unorm, Width: 4, Height: 4})
	if err := c.ClearSurface(surface(a8, virglA8Unorm), ClearColor0, [4]uint32{f32(1), f32(1), f32(1), f32(0.5)}, 0, 0, 0, 0, 4, 4); err != nil {
		t.Fatalf("ClearSurface of A8: %v", err)
	}
	for i, b := range read(a8, 4, 4, 1) {
		if b != 127 && b != 128 {
			t.Fatalf("A8 texel %d: %d, want 128", i, b)
		}
	}

	// A box of a texture, of a texel of its format, and the whole of a
	// texture of rows of more than a MiB.
	small := texture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4})
	if err := c.WriteTexture(small, TextureRegion{Box: Box{Width: 4, Height: 4, Depth: 1}}, make([]byte, 64), 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	if err := c.ClearTexture(small, 0, Box{X: 1, Y: 1, Width: 2, Height: 2, Depth: 1}, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("ClearTexture: %v", err)
	}
	px = read(small, 4, 4, 4)
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			want := [4]byte{}
			if x >= 1 && x < 3 && y >= 1 && y < 3 {
				want = [4]byte{1, 2, 3, 4}
			}
			if got := [4]byte(px[4*(4*y+x):]); got != want {
				t.Fatalf("cleared texel %d,%d: %v, want %v", x, y, got, want)
			}
		}
	}
	big := texture(ResourceDesc{Target: Target2D, Format: virglR32G32B32A32Uint, Width: 512, Height: 512})
	texel := binary.LittleEndian.AppendUint32(nil, 7)
	texel = binary.LittleEndian.AppendUint32(texel, 8)
	texel = binary.LittleEndian.AppendUint32(texel, 9)
	texel = binary.LittleEndian.AppendUint32(texel, 10)
	if err := c.ClearTexture(big, 0, Box{Width: 512, Height: 512, Depth: 1}, texel); err != nil {
		t.Fatalf("ClearTexture of a big texture: %v", err)
	}
	if px := read(big, 512, 512, 16); !bytes.Equal(px, bytes.Repeat(texel, 512*512)) {
		t.Errorf("a big texture's texels are not the clear's")
	}
	if err := c.ClearTexture(small, 0, Box{X: 3, Width: 2, Height: 1, Depth: 1}, texel); err == nil {
		t.Errorf("a clear of a box past a texture: no error")
	}

	// No surfaces of what Vulkan renders not.
	if bc, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglDXT1RGB, Width: 16, Height: 16}); err == nil {
		defer d.DestroyTexture(bc)
		if s, err := d.NewSurface(bc, virglDXT1RGB, 0, 0, 0); err == nil {
			d.DestroySurface(s)
			t.Errorf("a surface of a compressed texture: no error")
		}
	}
}
