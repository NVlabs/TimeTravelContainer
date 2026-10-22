// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"bytes"
	"testing"
)

// The virgl formats that the copies' tests use.
const (
	virglR8G8B8A8Unorm = 67
	virglR8Unorm       = 64
	virglDXT1RGB       = 105
	virglR32G32Uint    = 194
)

// TestContextCopies copies textures' boxes, of arrays' layers, of 3D
// textures' slices as layers, and of compressed formats' blocks as texels,
// and buffers' bytes, and refuses the copies that Vulkan has not.
func TestContextCopies(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	whole := func(tex *Texture) TextureRegion {
		w, h, depth := tex.Desc.levelExtent(0)
		return TextureRegion{Box: Box{Width: w, Height: h, Depth: depth}}
	}
	// fill returns a texture of desc, of level 0 of bytes of seed, and the
	// bytes, tightly, or nil if the device has no textures of desc.
	fill := func(desc ResourceDesc, seed int) (*Texture, []byte) {
		t.Helper()
		tex, err := d.NewTexture(desc)
		if err != nil {
			return nil, nil
		}
		t.Cleanup(func() { d.DestroyTexture(tex) })
		_, _, _, _, span := tex.memLayout(whole(tex))
		data := make([]byte, span)
		for i := range data {
			data[i] = byte(i*seed + 1)
		}
		if err := c.WriteTexture(tex, whole(tex), data, 0); err != nil {
			t.Fatalf("WriteTexture(%+v): %v", desc, err)
		}
		return tex, data
	}
	read := func(tex *Texture) []byte {
		t.Helper()
		_, _, _, _, span := tex.memLayout(whole(tex))
		out := make([]byte, span)
		if err := c.ReadTexture(tex, whole(tex), out, 0); err != nil {
			t.Fatalf("ReadTexture(%+v): %v", tex.Desc, err)
		}
		return out
	}
	// Of textures of R8G8B8A8, which the boxes' texels index, of layers or
	// slices z.
	for _, tc := range []struct {
		name     string
		dst, src ResourceDesc
		x, y, z  int64
		box      Box
	}{
		{"2D arrays' layers",
			ResourceDesc{Target: Target2DArray, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, ArraySize: 3},
			ResourceDesc{Target: Target2DArray, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, ArraySize: 3},
			1, 1, 0, Box{Z: 1, Width: 3, Height: 3, Depth: 2}},
		{"1D arrays' layers",
			ResourceDesc{Target: Target1DArray, Format: virglR8G8B8A8Unorm, Width: 8, ArraySize: 4},
			ResourceDesc{Target: Target1DArray, Format: virglR8G8B8A8Unorm, Width: 8, ArraySize: 4},
			0, 0, 0, Box{X: 2, Z: 1, Width: 4, Height: 1, Depth: 3}},
		{"3D slices to a 2D array's layers",
			ResourceDesc{Target: Target2DArray, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, ArraySize: 4},
			ResourceDesc{Target: Target3D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, Depth: 4},
			0, 0, 2, Box{Z: 1, Width: 4, Height: 4, Depth: 2}},
		{"a cube's faces to 3D slices",
			ResourceDesc{Target: Target3D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, Depth: 4},
			ResourceDesc{Target: TargetCube, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, ArraySize: 6},
			0, 0, 1, Box{Z: 3, Width: 4, Height: 4, Depth: 3}},
	} {
		dst, dstData := fill(tc.dst, 13)
		src, srcData := fill(tc.src, 7)
		if dst == nil || src == nil {
			t.Fatalf("%s: textures of %+v and %+v", tc.name, tc.dst, tc.src)
		}
		if err := c.CopyTexture(dst, 0, tc.x, tc.y, tc.z, src, 0, tc.box); err != nil {
			t.Fatalf("%s: CopyTexture: %v", tc.name, err)
		}
		want := append([]byte(nil), dstData...)
		texel := func(desc ResourceDesc, x, y, z int64) int64 {
			w, h, _ := desc.levelExtent(0)
			return ((z*h+y)*w + x) * 4
		}
		b := tc.box
		for z := int64(0); z < b.Depth; z++ {
			for y := int64(0); y < b.Height; y++ {
				for x := int64(0); x < b.Width; x++ {
					o := texel(dst.Desc, tc.x+x, tc.y+y, tc.z+z)
					copy(want[o:o+4], srcData[texel(src.Desc, b.X+x, b.Y+y, b.Z+z):])
				}
			}
		}
		if got := read(dst); !bytes.Equal(got, want) {
			t.Errorf("%s: % x, want % x", tc.name, got, want)
		}
	}

	// Of a compressed format's 4x4 blocks of 8 bytes, as texels of
	// R32G32_UINT, and back.
	bc, blocks := fill(ResourceDesc{Target: Target2D, Format: virglDXT1RGB, Width: 16, Height: 16}, 3)
	if bc == nil {
		t.Logf("no textures of DXT1")
	} else {
		plain, _ := fill(ResourceDesc{Target: Target2D, Format: virglR32G32Uint, Width: 4, Height: 4}, 5)
		bc2, _ := fill(ResourceDesc{Target: Target2D, Format: virglDXT1RGB, Width: 16, Height: 16}, 11)
		if err := c.CopyTexture(plain, 0, 0, 0, 0, bc, 0, Box{Width: 16, Height: 16, Depth: 1}); err != nil {
			t.Fatalf("CopyTexture of blocks to texels: %v", err)
		}
		if err := c.CopyTexture(bc2, 0, 0, 0, 0, plain, 0, Box{Width: 4, Height: 4, Depth: 1}); err != nil {
			t.Fatalf("CopyTexture of texels to blocks: %v", err)
		}
		for _, tex := range []*Texture{plain, bc2} {
			if got := read(tex); !bytes.Equal(got, blocks) {
				t.Errorf("copies of %s: % x, want % x", tex.Format.Name, got, blocks)
			}
		}
		// Of whole blocks.
		if _, err := CheckCopy(bc2, 0, 0, 0, 0, bc, 0, Box{X: 2, Width: 4, Height: 4, Depth: 1}); err == nil {
			t.Errorf("a copy of a box of parts of blocks: no error")
		}
		if _, err := CheckCopy(bc2, 0, 4, 0, 0, plain, 0, Box{Width: 4, Height: 4, Depth: 1}); err == nil {
			t.Errorf("a copy of blocks past the destination's edge: no error")
		}
	}

	rgba, _ := fill(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4}, 1)
	r8, _ := fill(ResourceDesc{Target: Target2D, Format: virglR8Unorm, Width: 4, Height: 4}, 1)
	array1D, _ := fill(ResourceDesc{Target: Target1DArray, Format: virglR8G8B8A8Unorm, Width: 4, ArraySize: 4}, 1)
	for _, tc := range []struct {
		name     string
		dst, src *Texture
		box      Box
	}{
		{"past the source", rgba, rgba, Box{X: 2, Width: 4, Height: 4, Depth: 1}},
		{"of texels of another size", r8, rgba, Box{Width: 4, Height: 4, Depth: 1}},
		{"of a 1D image to a 2D one", rgba, array1D, Box{Width: 4, Height: 1, Depth: 1}},
		{"of a 1D array's rows", array1D, array1D, Box{Y: 1, Width: 4, Height: 1, Depth: 1}},
	} {
		if err := c.CopyTexture(tc.dst, 0, 0, 0, 0, tc.src, 0, tc.box); err == nil {
			t.Errorf("a copy %s: no error", tc.name)
		}
	}

	// Of buffers' bytes.
	var bufs [2]*Buffer
	for i := range bufs {
		if bufs[i], err = d.NewBuffer(64, BufferTransferSrc|BufferTransferDst, false); err != nil {
			t.Fatalf("NewBuffer: %v", err)
		}
		defer d.DestroyBuffer(bufs[i])
	}
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i + 1)
	}
	if err := c.WriteBuffer(bufs[0], 0, data); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := c.WriteBuffer(bufs[1], 0, make([]byte, 64)); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	if err := c.CopyBuffer(bufs[1], 8, bufs[0], 4, 16); err != nil {
		t.Fatalf("CopyBuffer: %v", err)
	}
	if err := c.CopyBuffer(bufs[1], 60, bufs[0], 0, 8); err == nil {
		t.Errorf("a copy past a buffer's end: no error")
	}
	got := make([]byte, 64)
	if err := c.ReadBuffer(bufs[1], 0, got); err != nil {
		t.Fatalf("ReadBuffer: %v", err)
	}
	want := make([]byte, 64)
	copy(want[8:24], data[4:20])
	if !bytes.Equal(got, want) {
		t.Errorf("the copied buffer: % x, want % x", got, want)
	}
}
