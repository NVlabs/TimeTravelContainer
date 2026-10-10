// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virgl

// virgl's capability sets.
const (
	// CapsetVirgl is VIRTGPU_DRM_CAPSET_VIRGL.
	CapsetVirgl = 1
	// CapsetVirgl2 is VIRTGPU_DRM_CAPSET_VIRGL2.
	CapsetVirgl2 = 2
)

// virgl's commands, as virgl_protocol.h numbers them. A command is a header,
// VIRGL_CMD0's (cmd | object << 8 | length << 16), and its payload of length
// dwords.
const (
	cmdNop                    = 0
	cmdCreateObject           = 1
	cmdBindObject             = 2
	cmdDestroyObject          = 3
	cmdSetViewportState       = 4
	cmdSetFramebufferState    = 5
	cmdSetVertexBuffers       = 6
	cmdClear                  = 7
	cmdDrawVBO                = 8
	cmdResourceInlineWrite    = 9
	cmdSetSamplerViews        = 10
	cmdSetIndexBuffer         = 11
	cmdSetConstantBuffer      = 12
	cmdSetStencilRef          = 13
	cmdSetBlendColor          = 14
	cmdSetScissorState        = 15
	cmdBlit                   = 16
	cmdResourceCopyRegion     = 17
	cmdBindSamplerStates      = 18
	cmdBeginQuery             = 19
	cmdEndQuery               = 20
	cmdGetQueryResult         = 21
	cmdSetPolygonStipple      = 22
	cmdSetClipState           = 23
	cmdSetSampleMask          = 24
	cmdSetStreamoutTargets    = 25
	cmdSetRenderCondition     = 26
	cmdSetUniformBuffer       = 27
	cmdSetSubCtx              = 28
	cmdCreateSubCtx           = 29
	cmdDestroySubCtx          = 30
	cmdBindShader             = 31
	cmdSetTessState           = 32
	cmdSetMinSamples          = 33
	cmdSetShaderBuffers       = 34
	cmdSetShaderImages        = 35
	cmdMemoryBarrier          = 36
	cmdLaunchGrid             = 37
	cmdSetFramebufferNoAttach = 38
	cmdTextureBarrier         = 39
	cmdSetAtomicBuffers       = 40
	cmdSetDebugFlags          = 41
	cmdGetQueryResultQBO      = 42
	cmdTransfer3D             = 43
	cmdEndTransfers           = 44
	cmdCopyTransfer3D         = 45
	cmdSetTweaks              = 46
	cmdClearTexture           = 47
	cmdPipeResourceCreate     = 48
	cmdPipeResourceSetType    = 49
	cmdGetMemoryInfo          = 50
	cmdSendStringMarker       = 51
	cmdLinkShader             = 52
	cmdClearSurface           = 62
)

// virgl's objects.
const (
	objectBlend           = 1
	objectRasterizer      = 2
	objectDSA             = 3
	objectShader          = 4
	objectVertexElements  = 5
	objectSamplerView     = 6
	objectSamplerState    = 7
	objectSurface         = 8
	objectQuery           = 9
	objectStreamoutTarget = 10
	objectMSAASurface     = 11
)

// What commands carry.
const (
	transferToHost   = 1
	transferFromHost = 2

	// A shader's text, which comes in parts if it is longer than a command,
	// and its stage, of the protocol's stages: vertex, fragment, geometry,
	// tessellation control and evaluation, and compute.
	stageVertex              = 0
	stageFragment            = 1
	stageCompute             = 5
	shaderOffsetContinuation = 1 << 31
	shaderOffsetMask         = 0x7fffffff
	maxShaderTextBytes       = 16 << 20

	// queryStateSize is the size of struct virgl_host_query_state, which a
	// query's resource holds.
	queryStateSize = 16
)

// The most of what commands carry that the renderer takes: Gallium's
// limits, and virglrenderer's.
const (
	maxColorBufs        = 8
	maxShaderStages     = 6
	maxViewports        = 16
	maxVertexBuffers    = 32
	maxVertexElements   = 32
	maxSamplerViews     = 128
	maxSamplers         = 32
	maxConstantBuffers  = 32
	maxShaderBuffers    = 32
	maxShaderImages     = 64
	maxAtomicBuffers    = 32
	maxStreamoutTargets = 4
	maxStreamOutputs    = 128
	// maxConstantDwords is the most constants that a command sets, 64 KiB:
	// what OpenGL's drivers offer, Zink's among them.
	maxConstantDwords = 16 << 10
)
