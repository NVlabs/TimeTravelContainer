// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"errors"
	"math"
)

// The Gallium state that virgl's commands create, decoded of their
// payloads as virgl_protocol.h lays them out, as the decoder's pipe.c
// decodes them for Zink, and Vulkan's state of it.

// MaxColorBuffers is the number of a framebuffer's color buffers.
const MaxColorBuffers = 8

// RTBlend is a color buffer's blending, of Gallium's enumerations.
type RTBlend struct {
	Enable                        bool
	RGBFunc, RGBSrc, RGBDst       uint32
	AlphaFunc, AlphaSrc, AlphaDst uint32
	ColorMask                     uint32
}

// Blend is a blend state: Gallium's pipe_blend_state.
type Blend struct {
	IndependentBlend bool
	LogicOpEnable    bool
	Dither           bool
	AlphaToCoverage  bool
	AlphaToOne       bool
	LogicOp          uint32
	RT               [MaxColorBuffers]RTBlend
}

// Gallium's blend functions and factors that the decoder checks.
const (
	pipeBlendMax         = 4
	pipeBlendFactorOne   = 0x1
	pipeBlendFactorZero  = 0x11
	pipeBlendFactorInv   = 0x10
	pipeBlendFactorSrc1A = 0xa
)

// blendFactorOK reports whether f is one of Gallium's blend factors.
func blendFactorOK(f uint32) bool {
	base := f &^ pipeBlendFactorInv
	return base >= pipeBlendFactorOne && base <= pipeBlendFactorSrc1A && f != pipeBlendFactorInv|0x6
}

var errState = errors.New("an invalid state")

// DecodeBlend decodes a blend state's payload, of which d[0] is the
// handle.
func DecodeBlend(d []uint32) (*Blend, error) {
	if len(d) < 3+MaxColorBuffers {
		return nil, errState
	}
	s := &Blend{}
	s0 := d[1]
	s.IndependentBlend = s0&1 != 0
	s.LogicOpEnable = s0>>1&1 != 0
	s.Dither = s0>>2&1 != 0
	s.AlphaToCoverage = s0>>3&1 != 0
	s.AlphaToOne = s0>>4&1 != 0
	s.LogicOp = d[2] & 0xf
	for i := range s.RT {
		t := d[3+i]
		rt := &s.RT[i]
		rt.Enable = t&1 != 0
		rt.RGBFunc = t >> 1 & 7
		rt.RGBSrc = t >> 4 & 0x1f
		rt.RGBDst = t >> 9 & 0x1f
		rt.AlphaFunc = t >> 14 & 7
		rt.AlphaSrc = t >> 17 & 0x1f
		rt.AlphaDst = t >> 22 & 0x1f
		rt.ColorMask = t >> 27 & 0xf
		if (i > 0 && !s.IndependentBlend) || !rt.Enable {
			// The factors of the targets that blend only matter.
			rt.RGBFunc, rt.AlphaFunc = 0, 0
			rt.RGBSrc, rt.AlphaSrc = pipeBlendFactorOne, pipeBlendFactorOne
			rt.RGBDst, rt.AlphaDst = pipeBlendFactorZero, pipeBlendFactorZero
			continue
		}
		if rt.RGBFunc > pipeBlendMax || rt.AlphaFunc > pipeBlendMax || !blendFactorOK(rt.RGBSrc) ||
			!blendFactorOK(rt.RGBDst) || !blendFactorOK(rt.AlphaSrc) || !blendFactorOK(rt.AlphaDst) {
			return nil, errState
		}
	}
	return s, nil
}

// vkBlendFactors are Vulkan's blend factors, VkBlendFactor, of Gallium's.
var vkBlendFactors = map[uint32]uint32{
	0x01: 1,  // ONE
	0x02: 2,  // SRC_COLOR
	0x03: 6,  // SRC_ALPHA
	0x04: 8,  // DST_ALPHA
	0x05: 4,  // DST_COLOR
	0x06: 14, // SRC_ALPHA_SATURATE
	0x07: 10, // CONST_COLOR
	0x08: 12, // CONST_ALPHA
	0x09: 15, // SRC1_COLOR
	0x0a: 17, // SRC1_ALPHA
	0x11: 0,  // ZERO
	0x12: 3,  // INV_SRC_COLOR
	0x13: 7,  // INV_SRC_ALPHA
	0x14: 9,  // INV_DST_ALPHA
	0x15: 5,  // INV_DST_COLOR
	0x17: 11, // INV_CONST_COLOR
	0x18: 13, // INV_CONST_ALPHA
	0x19: 16, // INV_SRC1_COLOR
	0x1a: 18, // INV_SRC1_ALPHA
}

// VkBlendFactor returns Vulkan's blend factor of Gallium's.
func VkBlendFactor(f uint32) uint32 {
	return vkBlendFactors[f]
}

// vkLogicOps are Vulkan's logic ops, VkLogicOp, of Gallium's, which are
// in another order.
var vkLogicOps = [16]uint32{
	0,  // CLEAR
	8,  // NOR
	4,  // AND_INVERTED
	12, // COPY_INVERTED
	2,  // AND_REVERSE
	10, // INVERT
	6,  // XOR
	14, // NAND
	1,  // AND
	9,  // EQUIV
	5,  // NOOP
	13, // OR_INVERTED
	3,  // COPY
	11, // OR_REVERSE
	7,  // OR
	15, // SET
}

// VkLogicOp returns Vulkan's logic op of Gallium's.
func VkLogicOp(op uint32) uint32 {
	return vkLogicOps[op&0xf]
}

// Rasterizer is a rasterizer state: Gallium's pipe_rasterizer_state.
type Rasterizer struct {
	FlatShade            bool
	DepthClip            bool
	ClipHalfZ            bool
	RasterizerDiscard    bool
	FlatShadeFirst       bool
	LightTwoSide         bool
	SpriteCoordMode      uint32
	PointQuadRaster      bool
	CullFace             uint32
	FillFront, FillBack  uint32
	Scissor              bool
	FrontCCW             bool
	ClampVertexColor     bool
	ClampFragmentColor   bool
	OffsetLine           bool
	OffsetPoint          bool
	OffsetTri            bool
	PolySmooth           bool
	PolyStipple          bool
	PointSmooth          bool
	PointSizePerVertex   bool
	Multisample          bool
	LineSmooth           bool
	LineStipple          bool
	LineLastPixel        bool
	HalfPixelCenter      bool
	BottomEdgeRule       bool
	ForcePersampleInterp bool
	PointSize            float32
	SpriteCoordEnable    uint32
	LineStipplePattern   uint32
	LineStippleFactor    uint32
	ClipPlaneEnable      uint32
	LineWidth            float32
	OffsetUnits          float32
	OffsetScale          float32
	OffsetClamp          float32
}

// saneFloat returns f, or def if it is not finite.
func saneFloat(bits uint32, def float32) float32 {
	f := math.Float32frombits(bits)
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return def
	}
	return f
}

// DecodeRasterizer decodes a rasterizer state's payload, of which d[0] is
// the handle.
func DecodeRasterizer(d []uint32) (*Rasterizer, error) {
	if len(d) < 9 {
		return nil, errState
	}
	t := d[1]
	bit := func(n uint) bool { return t>>n&1 != 0 }
	s := &Rasterizer{
		FlatShade:            bit(0),
		DepthClip:            bit(1),
		ClipHalfZ:            bit(2),
		RasterizerDiscard:    bit(3),
		FlatShadeFirst:       bit(4),
		LightTwoSide:         bit(5),
		SpriteCoordMode:      t >> 6 & 1,
		PointQuadRaster:      bit(7),
		CullFace:             t >> 8 & 3,
		FillFront:            t >> 10 & 3,
		FillBack:             t >> 12 & 3,
		Scissor:              bit(14),
		FrontCCW:             bit(15),
		ClampVertexColor:     bit(16),
		ClampFragmentColor:   bit(17),
		OffsetLine:           bit(18),
		OffsetPoint:          bit(19),
		OffsetTri:            bit(20),
		PolySmooth:           bit(21),
		PolyStipple:          bit(22),
		PointSmooth:          bit(23),
		PointSizePerVertex:   bit(24),
		Multisample:          bit(25),
		LineSmooth:           bit(26),
		LineStipple:          bit(27),
		LineLastPixel:        bit(28),
		HalfPixelCenter:      bit(29),
		BottomEdgeRule:       bit(30),
		ForcePersampleInterp: bit(31),
		PointSize:            saneFloat(d[2], 1),
		SpriteCoordEnable:    d[3],
		LineStipplePattern:   d[4] & 0xffff,
		LineStippleFactor:    d[4] >> 16 & 0xff,
		ClipPlaneEnable:      d[4] >> 24 & 0xff,
		LineWidth:            saneFloat(d[5], 1),
		OffsetUnits:          saneFloat(d[6], 0),
		OffsetScale:          saneFloat(d[7], 0),
		OffsetClamp:          saneFloat(d[8], 0),
	}
	// Gallium's polygon modes are Vulkan's, but FILL_RECTANGLE, which
	// guests do not send.
	if s.FillFront > 2 || s.FillBack > 2 || s.PointSize < 0 || s.LineWidth < 0 {
		return nil, errState
	}
	return s, nil
}

// StencilFace is a face's stencil test, of Gallium's enumerations.
type StencilFace struct {
	Enable                   bool
	Func                     uint32
	FailOp, ZPassOp, ZFailOp uint32
	ValueMask, WriteMask     uint32
}

// DSA is a depth-stencil-alpha state: Gallium's
// pipe_depth_stencil_alpha_state.
type DSA struct {
	DepthEnable bool
	DepthWrite  bool
	DepthFunc   uint32
	AlphaEnable bool
	AlphaFunc   uint32
	AlphaRef    float32
	Stencil     [2]StencilFace
}

// DecodeDSA decodes a depth-stencil-alpha state's payload, of which d[0]
// is the handle.
func DecodeDSA(d []uint32) (*DSA, error) {
	if len(d) < 5 {
		return nil, errState
	}
	t := d[1]
	s := &DSA{
		DepthEnable: t&1 != 0,
		DepthWrite:  t>>1&1 != 0,
		DepthFunc:   t >> 2 & 7,
		AlphaEnable: t>>8&1 != 0,
		AlphaFunc:   t >> 9 & 7,
		AlphaRef:    math.Float32frombits(d[4]),
	}
	for i := range s.Stencil {
		t := d[2+i]
		s.Stencil[i] = StencilFace{
			Enable:    t&1 != 0,
			Func:      t >> 1 & 7,
			FailOp:    t >> 4 & 7,
			ZPassOp:   t >> 7 & 7,
			ZFailOp:   t >> 10 & 7,
			ValueMask: t >> 13 & 0xff,
			WriteMask: t >> 21 & 0xff,
		}
	}
	return s, nil
}

// vkStencilOps are Vulkan's stencil ops, VkStencilOp, of Gallium's, which
// order INVERT last.
var vkStencilOps = [8]uint32{0, 1, 2, 3, 4, 6, 7, 5}

// VkStencilOp returns Vulkan's stencil op of Gallium's.
func VkStencilOp(op uint32) uint32 {
	return vkStencilOps[op&7]
}

// Sampler state's wrap modes and mip filters, of Gallium's.
const (
	WrapRepeat = iota
	WrapClamp
	WrapClampToEdge
	WrapClampToBorder
	WrapMirrorRepeat
	WrapMirrorClamp
	WrapMirrorClampToEdge
	WrapMirrorClampToBorder

	MipFilterNearest = 0
	MipFilterLinear  = 1
	MipFilterNone    = 2
)

// SamplerState is a sampler state: Gallium's pipe_sampler_state.
type SamplerState struct {
	WrapS, WrapT, WrapR uint32
	MinFilter           uint32
	MipFilter           uint32
	MagFilter           uint32
	CompareMode         bool
	CompareFunc         uint32
	SeamlessCubeMap     bool
	MaxAnisotropy       uint32
	LodBias             float32
	MinLod, MaxLod      float32
	BorderColor         [4]uint32
}

// DecodeSamplerState decodes a sampler state's payload, of which d[0] is
// the handle.
func DecodeSamplerState(d []uint32) (*SamplerState, error) {
	if len(d) < 9 {
		return nil, errState
	}
	t := d[1]
	s := &SamplerState{
		WrapS:           t & 7,
		WrapT:           t >> 3 & 7,
		WrapR:           t >> 6 & 7,
		MinFilter:       t >> 9 & 1,
		MipFilter:       t >> 11 & 3,
		MagFilter:       t >> 13 & 1,
		CompareMode:     t>>15&1 != 0,
		CompareFunc:     t >> 16 & 7,
		SeamlessCubeMap: t>>19&1 != 0,
		MaxAnisotropy:   t >> 20 & 0x1f,
		LodBias:         saneFloat(d[2], 0),
		MinLod:          saneFloat(d[3], 0),
		MaxLod:          saneFloat(d[4], 1000),
	}
	if s.MaxLod < s.MinLod {
		s.MaxLod = s.MinLod
	}
	copy(s.BorderColor[:], d[5:9])
	if s.MipFilter > MipFilterNone {
		return nil, errState
	}
	return s, nil
}

// VkAddressMode returns Vulkan's sampler address mode,
// VkSamplerAddressMode, of a Gallium wrap mode: GL's CLAMP, of the edge's
// and the border's halves, as the edge's, and its mirrored modes but
// MIRROR_CLAMP_TO_EDGE as the mirrored repeat.
func VkAddressMode(wrap uint32) uint32 {
	switch wrap {
	case WrapRepeat:
		return 0
	case WrapClamp, WrapClampToEdge:
		return 2
	case WrapClampToBorder:
		return 3
	case WrapMirrorClampToEdge:
		return 4
	}
	return 1
}

// MaxVertexElements is the number of a vertex elements state's elements,
// and of the vertex buffers that they read.
const MaxVertexElements = 32

// VertexElement is an element of a vertex elements state: a vertex
// shader's input, of a vertex buffer's vertices, or instances of a
// divisor, of a format at an offset of each.
type VertexElement struct {
	Offset          uint32
	InstanceDivisor uint32
	Buffer          uint32
	Format          Format
	// VirglFormat is the element's format, as virgl numbers it.
	VirglFormat uint32
}

// DecodeVertexElements decodes the elements of a vertex elements state's
// payload, 4 dwords each, as the decoder's pipe.c does.
func DecodeVertexElements(elements []uint32) ([]VertexElement, error) {
	if len(elements)%4 != 0 || len(elements)/4 > MaxVertexElements {
		return nil, errState
	}
	ve := make([]VertexElement, len(elements)/4)
	for i := range ve {
		e := elements[4*i:]
		f, ok := LookupFormat(e[3])
		if !ok || e[2] >= MaxVertexElements {
			return nil, errState
		}
		ve[i] = VertexElement{Offset: e[0], InstanceDivisor: e[1], Buffer: e[2], Format: f, VirglFormat: e[3]}
	}
	return ve, nil
}

// Viewport is a Gallium viewport: the scales and translations of its
// transform of normalized device coordinates to window coordinates.
type Viewport struct {
	Scale, Translate [3]float32
}

// DecodeViewports decodes viewports, 6 dwords each, as virgl's
// SET_VIEWPORT_STATE has them.
func DecodeViewports(d []uint32) []Viewport {
	vps := make([]Viewport, len(d)/6)
	for i := range vps {
		v := &vps[i]
		for j := 0; j < 3; j++ {
			v.Scale[j] = saneFloat(d[6*i+j], 0)
			v.Translate[j] = saneFloat(d[6*i+3+j], 0)
		}
	}
	return vps
}

// VkViewport returns the Vulkan viewport of v: x, y, width, height, and
// the depth range, of a negative height for a transform that flips y, and
// of a depth range of GL's clip space's z, of [-1, 1], unless halfZ, of
// which the last stage before rasterization maps z to Vulkan's [0, 1].
func (v Viewport) VkViewport(halfZ bool) (x, y, width, height, minDepth, maxDepth float32) {
	x, width = v.Translate[0]-v.Scale[0], 2*v.Scale[0]
	y, height = v.Translate[1]-v.Scale[1], 2*v.Scale[1]
	if halfZ {
		minDepth, maxDepth = v.Translate[2], v.Translate[2]+v.Scale[2]
	} else {
		minDepth, maxDepth = v.Translate[2]-v.Scale[2], v.Translate[2]+v.Scale[2]
	}
	return x, y, width, height, minDepth, maxDepth
}

// Scissor is a scissor rectangle, of [Min, Max) of each axis.
type Scissor struct {
	MinX, MinY, MaxX, MaxY uint32
}

// DecodeScissors decodes scissors, 2 dwords each, as virgl's
// SET_SCISSOR_STATE has them.
func DecodeScissors(d []uint32) []Scissor {
	ss := make([]Scissor, len(d)/2)
	for i := range ss {
		ss[i] = Scissor{MinX: d[2*i] & 0xffff, MinY: d[2*i] >> 16, MaxX: d[2*i+1] & 0xffff, MaxY: d[2*i+1] >> 16}
	}
	return ss
}
