// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

const virglZ24S8 = 19

// TestPortabilitySubset renders on the test's device as on a device of
// VK_KHR_portability_subset that lacks what Apple's GPUs under MoltenVK
// lack, of the package's workarounds: triangle fans, which the caps leave
// guests to draw as triangles, 24-bit depths, of 32-bit floats, and
// multisampled arrays, which the device refuses.
func TestPortabilitySubset(t *testing.T) {
	d := newTestDevice(t)
	for _, f := range []uint32{vkD32, vkD32S8} {
		if d.lockedFormatFeatures(f)&featureDepthStencil == 0 {
			t.Skipf("%s renders no depths of format %d", d.Name, f)
		}
	}
	d.triangleFans, d.pointPolygons, d.samplerMipLodBias, d.separateStencilRef, d.multisampleArrays = false, false, false, false, false
	d.substitutes = map[uint32]uint32{vkD24S8: vkD32S8, vkX8D24: vkD32}
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()

	// Triangle fans, which the caps offer not.
	var caps capsV2
	if err := binary.Read(bytes.NewReader(d.Caps(2, 1, 4)), binary.LittleEndian, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.V1.PrimMask&(1<<primTriangleFan) != 0 {
		t.Errorf("caps of triangle fans: %#x", caps.V1.PrimMask)
	}
	if _, ok := d.VkTopology(primTriangleFan); ok {
		t.Errorf("a topology of triangle fans")
	}
	if _, ok := d.VkTopology(4); !ok {
		t.Errorf("no topology of triangles")
	}
	for _, f := range []uint32{virglZ24S8, virglZ24X8} {
		if !caps.V1.DepthStencil.has(f) {
			t.Errorf("no caps of depths of format %d", f)
		}
	}

	// Z24X8, of 32-bit floats, whose transfers and clears convert the
	// guest's 24-bit depths.
	z24, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglZ24X8, Width: 4, Height: 1})
	if err != nil {
		t.Fatalf("NewTexture of Z24X8: %v", err)
	}
	defer d.DestroyTexture(z24)
	if z24.Format.VK != vkD32 || !z24.floatDepth24 {
		t.Fatalf("Z24X8 of Vulkan's format %d", z24.Format.VK)
	}
	row := TextureRegion{Box: Box{Width: 4, Height: 1, Depth: 1}}
	depths := func(zs ...uint32) []byte {
		var b []byte
		for _, z := range zs {
			b = binary.LittleEndian.AppendUint32(b, z)
		}
		return b
	}
	read := func() []byte {
		t.Helper()
		got := make([]byte, 16)
		if err := c.ReadTexture(z24, row, got, 0); err != nil {
			t.Fatalf("ReadTexture: %v", err)
		}
		return got
	}
	if err := c.WriteTexture(z24, row, depths(0, 1, 0x7fffff, 0xffffff), 0); err != nil {
		t.Fatalf("WriteTexture: %v", err)
	}
	if got, want := read(), depths(0, 1, 0x7fffff, 0xffffff); !bytes.Equal(got, want) {
		t.Errorf("depths read back: %x, want %x", got, want)
	}
	if err := c.ClearTexture(z24, 0, Box{X: 1, Width: 2, Height: 1, Depth: 1}, depths(0x123456)); err != nil {
		t.Fatalf("ClearTexture: %v", err)
	}
	if got, want := read(), depths(0, 0x123456, 0x123456, 0xffffff); !bytes.Equal(got, want) {
		t.Errorf("depths after a clear: %x, want %x", got, want)
	}

	// Z24S8, of 32-bit floats and stencil, which renders and samples.
	zs, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglZ24S8, Width: 8, Height: 8})
	if err != nil {
		t.Fatalf("NewTexture of Z24S8: %v", err)
	}
	defer d.DestroyTexture(zs)
	if zs.Format.VK != vkD32S8 {
		t.Fatalf("Z24S8 of Vulkan's format %d", zs.Format.VK)
	}
	s, err := d.NewSurface(zs, virglZ24S8, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewSurface of Z24S8: %v", err)
	}
	defer d.DestroySurface(s)
	if err := c.ClearSurface(s, ClearDepth|ClearStencil, [4]uint32{}, 0.5, 3, 0, 0, 8, 8); err != nil {
		t.Fatalf("ClearSurface of Z24S8: %v", err)
	}
	v, err := d.NewSamplerView(zs, ViewDesc{Format: virglZ24S8, Target: Target2D, Swizzle: [4]uint32{SwizzleX, SwizzleY, SwizzleZ, SwizzleW}})
	if err != nil {
		t.Fatalf("NewSamplerView of Z24S8: %v", err)
	}
	d.DestroySamplerView(v)
	c.Flush().Wait()

	// Multisampled arrays, which the device has none of.
	if ms, err := d.NewTexture(ResourceDesc{Target: Target2DArray, Format: virglR8G8B8A8Unorm, Width: 4, Height: 4, ArraySize: 2, Samples: 4}); err == nil {
		d.DestroyTexture(ms)
		t.Errorf("a multisampled array: no error")
	}
}

// TestWidenIndexes widens indexes of a byte, of a device that draws none,
// to 16 bits, and the restart index among them to Vulkan's, if restart is
// enabled.
func TestWidenIndexes(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	b, err := d.NewBuffer(8, BufferTransferSrc|BufferTransferDst|BufferIndex, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(b)
	if err := c.WriteBuffer(b, 0, []byte{9, 0, 1, 0xff, 2, 0, 0, 0}); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	for _, tc := range []struct {
		restart bool
		want    []byte
	}{
		{true, []byte{0, 0, 1, 0, 0xff, 0xff, 2, 0}},
		{false, []byte{0, 0, 1, 0, 0xff, 0, 2, 0}},
	} {
		w, err := c.WidenIndexes(b, 1, 4, tc.restart, 0xff)
		if err != nil {
			t.Fatalf("WidenIndexes: %v", err)
		}
		if got := w.Bytes()[:8]; !bytes.Equal(got, tc.want) {
			t.Errorf("indexes widened of restart %v: %v, want %v", tc.restart, got, tc.want)
		}
	}
	c.Flush().Wait()
}

// TestVertexLimits refuses pipelines of vertex input past the device's
// limits, which guests' drivers keep to, as the caps give them, but guests
// need not.
func TestVertexLimits(t *testing.T) {
	d := newTestDevice(t)
	l := &d.props.Limits
	t.Logf("%d bindings of strides of %d, %d attributes of offsets of %d", l.MaxVertexInputBindings,
		l.MaxVertexInputBindingStride, l.MaxVertexInputAttributes, l.MaxVertexInputAttributeOffset)
	for _, tc := range []struct {
		name   string
		limit  uint32
		desc   PipelineDesc
		wantIn string
	}{
		{"a binding", l.MaxVertexInputBindings, PipelineDesc{Bindings: []VertexBinding{{Binding: l.MaxVertexInputBindings}}}, "a vertex binding"},
		{"a stride", l.MaxVertexInputBindingStride, PipelineDesc{Bindings: []VertexBinding{{Stride: l.MaxVertexInputBindingStride + 4}}}, "a vertex binding"},
		{"a location", l.MaxVertexInputAttributes, PipelineDesc{Inputs: []VertexInput{{Location: l.MaxVertexInputAttributes}}}, "a vertex input"},
		{"an offset", l.MaxVertexInputAttributeOffset, PipelineDesc{Inputs: []VertexInput{{Offset: l.MaxVertexInputAttributeOffset + 4}}}, "a vertex input"},
	} {
		if tc.limit > ^uint32(0)-4 {
			// Of no value past the limit.
			continue
		}
		tc.desc.Rasterizer, tc.desc.DSA, tc.desc.Blend = &Rasterizer{}, &DSA{}, &Blend{}
		p, err := d.NewPipeline(tc.desc)
		if err == nil {
			d.DestroyPipeline(p)
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s past the device's limit: %v, want an error of %q", tc.name, err, tc.wantIn)
		}
	}
}

// TestRealignVertices copies vertex elements of addresses and strides of no
// multiples of 4 to a buffer of multiples of 4, of the vertices that the
// source has alone, whatever the range of the draw's.
func TestRealignVertices(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	src, err := d.NewBuffer(64, BufferTransferSrc|BufferTransferDst|BufferVertex, false)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(src)
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i + 1)
	}
	if err := c.WriteBuffer(src, 0, data); err != nil {
		t.Fatalf("WriteBuffer: %v", err)
	}
	for _, tc := range []struct {
		name                       string
		offset                     uint64
		stride, size, first, count uint32
		wantStride                 uint32
		want                       []byte
	}{
		// Elements of 3 bytes at 13, 19 and 25, of vertices 2 to 4.
		{"a range", 1, 6, 3, 2, 3, 4, append(make([]byte, 8), 14, 15, 16, 0, 20, 21, 22, 0, 26, 27, 28, 0)},
		// Of vertices 9 and 10 alone, of which the source has 3 bytes of
		// the last's 4, at 61.
		{"past the source", 1, 6, 4, 9, 100, 4, append(make([]byte, 36), 56, 57, 58, 59, 62, 63, 64, 0)},
		// One element of all vertices.
		{"a stride of 0", 5, 0, 4, 7, 9, 0, []byte{6, 7, 8, 9}},
		// Of vertices past 32 bits' indexes, which the source has none of.
		{"past 32 bits", 1, 6, 4, ^uint32(0) - 1, MaxRealigned, 4, make([]byte, 4)},
	} {
		dst, stride, err := c.RealignVertices(src, tc.offset, tc.stride, tc.size, tc.first, tc.count)
		if err != nil {
			t.Fatalf("%s: RealignVertices: %v", tc.name, err)
		}
		if stride != tc.wantStride || dst.Size() != uint64(len(tc.want)) {
			t.Errorf("%s: a buffer of %d bytes of a stride of %d, want %d of %d", tc.name, dst.Size(), stride,
				len(tc.want), tc.wantStride)
			continue
		}
		got := make([]byte, len(tc.want))
		if err := c.ReadBuffer(dst, 0, got); err != nil {
			t.Fatalf("%s: ReadBuffer: %v", tc.name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	c.Flush().Wait()
}

// TestVertexAlignment checks the alignments of vertex elements that Vulkan
// requires, of their channels' sizes or of packed formats' wholes, of
// which devices of no alignment of their own realign elements, and Metal's
// of 4 bytes at least.
func TestVertexAlignment(t *testing.T) {
	for _, tc := range []struct {
		vf, want uint32
	}{
		{66, 1},  // R8G8B8_UNORM
		{67, 1},  // R8G8B8A8_UNORM
		{48, 2},  // R16_UNORM
		{57, 2},  // R16G16_SNORM
		{94, 2},  // R16G16B16A16_FLOAT
		{28, 4},  // R32_FLOAT
		{31, 4},  // R32G32B32A32_FLOAT
		{33, 4},  // R32G32_UNORM, of no Vulkan format
		{7, 2},   // B5G6R5_UNORM, packed
		{8, 4},   // R10G10B10A2_UNORM, packed
		{131, 4}, // B10G10R10A2_UNORM, packed
	} {
		if got := VertexAlignment(tc.vf); got != tc.want {
			t.Errorf("VertexAlignment(%d) = %d, want %d", tc.vf, got, tc.want)
		}
	}
	d := newTestDevice(t)
	for _, tc := range []struct {
		address        uint64
		stride, align  uint32
		metal, aligned bool
	}{
		{1, 3, 1, false, true},
		{2, 6, 2, false, true},
		{1, 4, 2, false, false},
		{2, 3, 2, false, false},
		{4, 8, 4, false, true},
		{2, 4, 4, false, false},
		{2, 6, 2, true, false},
		{4, 12, 1, true, true},
	} {
		d.alignedVertices = tc.metal
		if got := d.VertexAligned(tc.address, tc.stride, tc.align); got != tc.aligned {
			t.Errorf("VertexAligned(%d, %d, %d) of Metal %v = %v, want %v", tc.address, tc.stride, tc.align, tc.metal, got, tc.aligned)
		}
	}
}
