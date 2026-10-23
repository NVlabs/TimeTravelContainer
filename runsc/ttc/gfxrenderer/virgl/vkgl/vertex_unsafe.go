// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"fmt"
	"strings"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// VertexFetch returns the Vulkan format that the device fetches vertex
// elements of virgl's format vf as, and the conversion of what it fetches
// that vertex shaders make: of the format's own Vulkan format, of no
// conversion; of scaled formats, of the integers of their layout; of
// packed formats of 10, 10, 10 and 2 bits, of a 32-bit integer; or false
// if the device fetches none of them.
func (d *Device) VertexFetch(vf uint32) (uint32, tospirv.VertexFetch, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	fetches := func(vk uint32) bool {
		return vk != 0 && d.formatProperties(vk).BufferFeatures&featureVertexBuffer != 0
	}
	if fetch, ok := fetch32(vf); ok {
		vk := vkR32Uints[fetch.Channels]
		if fetch.Signed {
			vk = vkR32Sints[fetch.Channels]
		}
		return vk, fetch, fetches(vk)
	}
	f, ok := LookupFormat(vf)
	if !ok || f.Swizzle != "XYZW" {
		return 0, tospirv.VertexFetch{}, false
	}
	if fetches(f.VK) {
		return f.VK, tospirv.VertexFetch{}, true
	}
	layout, kind, _ := strings.Cut(f.Name, "_")
	scaled := kind == "USCALED" || kind == "SSCALED"
	signed := kind == "SSCALED" || kind == "SNORM"
	// Vulkan's UINT and SINT formats follow their layouts' USCALED and
	// SSCALED by two.
	if scaled && fetches(f.VK+2) {
		return f.VK + 2, tospirv.VertexFetch{Kind: tospirv.FetchScaled, Signed: signed,
			Channels: uint8(len(channelsRE.FindAllString(layout, -1)))}, true
	}
	if (layout == "R10G10B10A2" || layout == "B10G10R10A2") && (scaled || kind == "UNORM" || kind == "SNORM") &&
		fetches(vkR32Uint) {
		return vkR32Uint, tospirv.VertexFetch{Kind: tospirv.FetchPacked1010102, Signed: signed, Channels: 4,
			Normalized: !scaled, BGR: layout == "B10G10R10A2"}, true
	}
	return 0, tospirv.VertexFetch{}, false
}

// VertexAligned reports whether the device fetches vertex elements of an
// address, of a buffer's offset and an element's, and a stride: of
// multiples of 4, of Metal's, and of any of others.
func (d *Device) VertexAligned(address uint64, stride uint32) bool {
	return !d.alignedVertices || address%4 == 0 && stride%4 == 0
}

// RealignVertices copies the elements, of size bytes, of count vertices from
// first, which src has at offset plus a multiple of stride, to a new vertex
// buffer of addresses and a stride of multiples of 4, and returns it and its
// stride: size rounded up to a multiple of 4, or 0 of a stride of 0. Of
// vertices that src has not, the new buffer has none either, which fetches
// read as zeros, as they would of src. The batch frees the buffer, which it
// fills in order of the context's commands.
func (c *Context) RealignVertices(src *Buffer, offset uint64, stride, size, first, count uint32) (*Buffer, uint32, error) {
	newStride := (size + 3) &^ 3
	if stride == 0 {
		newStride, first, count = 0, 0, 1
	}
	if count > MaxRealigned {
		return nil, 0, fmt.Errorf("a realignment of %d vertices", count)
	}
	end := uint64(first) + uint64(count)
	switch {
	case offset >= src.size:
		end = 0
	case stride != 0:
		end = min(end, (src.size-offset+uint64(stride)-1)/uint64(stride))
	}
	total := uint64(newStride)
	if stride == 0 {
		total = uint64(size+3) &^ 3
	}
	if end > uint64(first) {
		total = max(total, end*uint64(newStride))
	}
	if total > maxRealignedSize {
		return nil, 0, fmt.Errorf("a realignment of vertices to %d bytes", total)
	}
	dst, err := c.d.NewBuffer(total, BufferVertex|BufferTransferSrc|BufferTransferDst, false)
	if err != nil {
		return nil, 0, err
	}
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		c.d.DestroyBuffer(dst)
		return nil, 0, err
	}
	c.batch.staging = append(c.batch.staging, dst)
	var regions []vn.VkBufferCopy
	for k := uint64(first); k < end; k++ {
		at := offset + k*uint64(stride)
		regions = append(regions, vn.VkBufferCopy{SrcOffset: at, DstOffset: k * uint64(newStride), Size: min(uint64(size), src.size-at)})
	}
	cmds.barrier()
	fb := get[vn.Args_vkCmdFillBuffer](c.a)
	fb.DstBuffer, fb.Size = dst.buf, ^uint64(0)
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdFillBuffer_EXT, unsafe.Pointer(fb))
	cmds.barrier()
	for len(regions) != 0 {
		n := min(len(regions), 1<<14)
		rs := array[vn.VkBufferCopy](c.a, n)
		copy(rs, regions[:n])
		cb := get[vn.Args_vkCmdCopyBuffer](c.a)
		cb.SrcBuffer, cb.DstBuffer, cb.RegionCount, cb.PRegions = src.buf, dst.buf, uint32(n), unsafe.Pointer(&rs[0])
		cmds.call(vn.VK_COMMAND_TYPE_vkCmdCopyBuffer_EXT, unsafe.Pointer(cb))
		c.a.reset()
		regions = regions[n:]
	}
	cmds.barrier()
	return dst, newStride, nil
}
