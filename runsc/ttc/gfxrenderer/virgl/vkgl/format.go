// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

// Format is a virgl format, as Vulkan has or emulates it: its Vulkan
// format, and the swizzle that a view applies to give the virgl format's
// channels of the Vulkan format's, of 'X', 'Y', 'Z', 'W', '0' and '1'; and
// its blocks of texels, of BlockBytes bytes each.
type Format struct {
	Name        string
	VK          uint32
	Swizzle     string
	BlockBytes  uint32
	BlockWidth  uint32
	BlockHeight uint32
	kind        formatKind
	SRGB        bool
	Compressed  bool
}

// formatKind is what a format's channels are.
type formatKind uint8

const (
	kindFloat formatKind = iota
	kindSint
	kindUint
	kindDepth
	kindStencil
	kindDepthStencil
)

// LookupFormat returns virgl's format f, as Vulkan has or emulates it, or
// false if it has neither.
func LookupFormat(f uint32) (Format, bool) {
	ft, ok := formats[f]
	return ft, ok
}

// IsDepth reports whether the format has depth, and IsStencil whether it
// has stencil.
func (f Format) IsDepth() bool {
	return f.kind == kindDepth || f.kind == kindDepthStencil
}

// IsStencil reports whether the format has stencil.
func (f Format) IsStencil() bool {
	return f.kind == kindStencil || f.kind == kindDepthStencil
}

// IsInteger reports whether the format's channels are integers, and of
// which sign.
func (f Format) IsInteger() (integer, signed bool) {
	return f.kind == kindSint || f.kind == kindUint, f.kind == kindSint
}

// vkColor returns a color of the format's channels, of a clear or a write,
// as the Vulkan format's: each of the channel that the swizzle reads it
// as, or 0.
func (f Format) vkColor(color [4]uint32) [4]uint32 {
	if f.Swizzle == "XYZW" {
		return color
	}
	var c [4]uint32
	for k := range c {
		for j := range f.Swizzle {
			if f.Swizzle[j] == "XYZW"[k] {
				c[k] = color[j]
				break
			}
		}
	}
	return c
}
