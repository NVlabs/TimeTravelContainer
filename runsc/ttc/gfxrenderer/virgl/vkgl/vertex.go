// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"regexp"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// Vertex formats of what Mesa's virgl driver sends: every plain format of
// Gallium's, as it offers them all, whatever the caps say, of which Vulkan
// lacks some, and devices fetch fewer. Vertex shaders convert what the
// device fetches of the others, as integers (tospirv.VertexFetch).

// The Vulkan formats of integers of 32 bits, by their channels, which
// formats of 32-bit channels that Vulkan has not are fetched as.
var (
	vkR32Uints = [5]uint32{0, 98, 101, 104, 107} // VK_FORMAT_R32*_UINT
	vkR32Sints = [5]uint32{0, 99, 102, 105, 108} // VK_FORMAT_R32*_SINT
)

// vkR32Uint is VK_FORMAT_R32_UINT, which packed formats are fetched as.
const vkR32Uint = 98

// fetch32 returns the fetch of virgl's formats of 32-bit normalized and
// scaled channels, which Vulkan has none of, and false of others: virgl's
// R32*_UNORM, USCALED, SNORM and SSCALED, of 1 to 4 channels each, are
// formats 32 to 47.
func fetch32(vf uint32) (tospirv.VertexFetch, bool) {
	if vf < 32 || vf > 47 {
		return tospirv.VertexFetch{}, false
	}
	group := (vf - 32) / 4 // UNORM, USCALED, SNORM, SSCALED.
	fetch := tospirv.VertexFetch{Kind: tospirv.FetchScaled, Signed: group >= 2, Channels: uint8((vf-32)%4 + 1)}
	if group%2 == 0 {
		fetch.Kind = tospirv.FetchNormalized32
	}
	return fetch, true
}

// IsVertexFormat reports whether virgl's format vf is of vertex elements
// that VertexFetch may fetch.
func IsVertexFormat(vf uint32) bool {
	_, ok := fetch32(vf)
	_, known := LookupFormat(vf)
	return ok || known
}

// channelsRE matches the channels of a format's name, as Mesa's.
var channelsRE = regexp.MustCompile(`[RGBAX][0-9]+`)

// MaxRealigned is the most vertices that RealignVertices copies, and
// maxRealignedSize the most bytes of the buffer it copies them to.
const (
	MaxRealigned     = 1 << 20
	maxRealignedSize = 1 << 28
)

// VertexElementSize returns the bytes of a vertex element of virgl's
// format vf.
func VertexElementSize(vf uint32) uint32 {
	if fetch, ok := fetch32(vf); ok {
		return 4 * uint32(fetch.Channels)
	}
	f, _ := LookupFormat(vf)
	return f.BlockBytes
}
