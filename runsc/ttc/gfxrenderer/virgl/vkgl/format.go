// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"encoding/binary"
	"math"
)

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

// depth24Mask is the bits of a 24-bit depth.
const depth24Mask = 1<<24 - 1

// depth24ToFloat converts 24-bit depths, of the low bits of each 32 bits of
// b, as the guest's memory has Z24X8's, to 32-bit floats, in place.
func depth24ToFloat(b []byte) {
	for i := 0; i+4 <= len(b); i += 4 {
		z := binary.LittleEndian.Uint32(b[i:]) & depth24Mask
		binary.LittleEndian.PutUint32(b[i:], math.Float32bits(float32(z)/depth24Mask))
	}
}

// floatToDepth24 converts 32-bit floats of depths to 24-bit depths, of the
// low bits of each 32 bits of b, in place.
func floatToDepth24(b []byte) {
	for i := 0; i+4 <= len(b); i += 4 {
		f := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))
		if !(f > 0) {
			f = 0
		}
		binary.LittleEndian.PutUint32(b[i:], uint32(math.Round(min(f, 1)*depth24Mask)))
	}
}
