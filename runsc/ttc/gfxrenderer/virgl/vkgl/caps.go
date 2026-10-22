// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"bytes"
	"encoding/binary"
)

// The capability sets of virgl, of virgl_hw.h's struct virgl_caps_v1 and
// struct virgl_caps_v2, which guests' drivers read of the renderer.

// formatMask is a bit of each virgl format, of struct
// virgl_supported_format_mask.
type formatMask [16]uint32

func (m *formatMask) set(f uint32) {
	if f < 32*uint32(len(m)) {
		m[f/32] |= 1 << (f % 32)
	}
}

func (m *formatMask) has(f uint32) bool {
	return f < 32*uint32(len(m)) && m[f/32]&(1<<(f%32)) != 0
}

// capsV1 is struct virgl_caps_v1.
type capsV1 struct {
	MaxVersion                 uint32
	Sampler                    formatMask
	Render                     formatMask
	DepthStencil               formatMask
	VertexBuffer               formatMask
	BoolSet1                   uint32
	GLSLLevel                  uint32
	MaxTextureArrayLayers      uint32
	MaxStreamoutBuffers        uint32
	MaxDualSourceRenderTargets uint32
	MaxRenderTargets           uint32
	MaxSamples                 uint32
	PrimMask                   uint32
	MaxTBOSize                 uint32
	MaxUniformBlocks           uint32
	MaxViewports               uint32
	MaxTextureGatherComponents uint32
}

// capsV2 is struct virgl_caps_v2.
type capsV2 struct {
	V1                              capsV1
	MinAliasedPointSize             float32
	MaxAliasedPointSize             float32
	MinSmoothPointSize              float32
	MaxSmoothPointSize              float32
	MinAliasedLineWidth             float32
	MaxAliasedLineWidth             float32
	MinSmoothLineWidth              float32
	MaxSmoothLineWidth              float32
	MaxTextureLodBias               float32
	MaxGeomOutputVertices           uint32
	MaxGeomTotalOutputComponents    uint32
	MaxVertexOutputs                uint32
	MaxVertexAttribs                uint32
	MaxShaderPatchVaryings          uint32
	MinTexelOffset                  int32
	MaxTexelOffset                  int32
	MinTextureGatherOffset          int32
	MaxTextureGatherOffset          int32
	TextureBufferOffsetAlignment    uint32
	UniformBufferOffsetAlignment    uint32
	ShaderBufferOffsetAlignment     uint32
	CapabilityBits                  uint32
	SampleLocations                 [8]uint32
	MaxVertexAttribStride           uint32
	MaxShaderBufferFragCompute      uint32
	MaxShaderBufferOtherStages      uint32
	MaxShaderImageFragCompute       uint32
	MaxShaderImageOtherStages       uint32
	MaxImageSamples                 uint32
	MaxComputeWorkGroupInvocations  uint32
	MaxComputeSharedMemorySize      uint32
	MaxComputeGridSize              [3]uint32
	MaxComputeBlockSize             [3]uint32
	MaxTexture2DSize                uint32
	MaxTexture3DSize                uint32
	MaxTextureCubeSize              uint32
	MaxCombinedShaderBuffers        uint32
	MaxAtomicCounters               [6]uint32
	MaxAtomicCounterBuffers         [6]uint32
	MaxCombinedAtomicCounters       uint32
	MaxCombinedAtomicCounterBuffers uint32
	HostFeatureCheckVersion         uint32
	SupportedReadbackFormats        formatMask
	Scanout                         formatMask
	CapabilityBitsV2                uint32
	MaxVideoMemory                  uint32
	Renderer                        [64]byte
	MaxAnisotropy                   float32
	MaxTextureSamplers              uint32
	SupportedMultisampleFormats     formatMask
	MaxConstBufferSize              [6]uint32
	NumVideoCaps                    uint32
	VideoCaps                       [32][4]uint32
	MaxUniformBlockSize             uint32
	MaxTCSOutputs                   uint32
	MaxTESOutputs                   uint32
	MaxShaderStorageBlocks          [6]uint32
}

// The bits of struct virgl_caps_bool_set1, of virgl_hw.h's order.
const (
	capIndepBlendEnable = 1 << iota
	capIndepBlendFunc
	capCubeMapArray
	capShaderStencilExport
	capConditionalRender
	capStartInstance
	capPrimitiveRestart
	capBlendEqSep
	capInstanceID
	capVertexElementInstanceDivisor
	capSeamlessCubeMap
	capOcclusionQuery
	capTimerQuery
	capStreamoutPauseResume
	capTextureMultisample
	capFragmentCoordConventions
	capDepthClipDisable
	capSeamlessCubeMapPerTexture
	capUBO
	capColorClamping
	capPolyStipple
	capMirrorClamp
	capTextureQueryLod
	capHasFP64
	capHasTessellationShaders
	capHasIndirectDraw
	capHasSampleShading
	capHasCull
	capConditionalRenderInverted
	capDerivativeControl
	capPolygonOffsetClamp
	capTransformFeedbackOverflowQuery
)

// The bits of virgl_caps_v2's capability_bits, of virgl_hw.h's VIRGL_CAP_*.
const (
	capTGSIInvariant      = 1 << 0
	capTextureView        = 1 << 1
	capSetMinSamples      = 1 << 2
	capTGSIPrecise        = 1 << 4
	capTXQS               = 1 << 5
	capMemoryBarrier      = 1 << 6
	capComputeShader      = 1 << 7
	capFBNoAttach         = 1 << 8
	capRobustBufferAccess = 1 << 9
	capTextureBarrier     = 1 << 12
	capFBOMixedFormats    = 1 << 18
	capBindCommandArgs    = 1 << 20
	capMultiDrawIndirect  = 1 << 21
	capClipHalfZ          = 1 << 27
	capClearTexture       = 1 << 30
)

// The mesa_prim primitives, by the bits of prim_mask, that Vulkan draws:
// guests' drivers convert the others.
const vkPrimMask = 1<<0 | 1<<1 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<10 | 1<<11 | 1<<12 | 1<<13 | 1<<14

// MaxStreamOutputBuffers is the number of transform feedback's targets,
// which the renderer captures to shader buffers.
const MaxStreamOutputBuffers = 4

// encodeCaps returns c, as virgl's guests read it, of little-endian
// fields.
func encodeCaps(c *capsV2) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, c)
	return b.Bytes()
}
