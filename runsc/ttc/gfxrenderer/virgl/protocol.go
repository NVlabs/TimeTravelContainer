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
	stageGeometry            = 2
	stageTessEval            = 4
	stageCompute             = 5
	shaderOffsetContinuation = 1 << 31
	shaderOffsetMask         = 0x7fffffff
	maxShaderTextBytes       = 16 << 20

	// queryStateSize is the size of struct virgl_host_query_state, which a
	// query's resource holds, and queryStateDone its state once it holds
	// the result.
	queryStateSize = 16
	queryStateDone = 1

	// capsV1Size and capsV2Size are the sizes of virgl_hw.h's struct
	// virgl_caps_v1 and struct virgl_caps_v2, the capability sets.
	capsV1Size = 308
	capsV2Size = 1408

	// Gallium's primitives, and queries, as virgl numbers them: as
	// Gallium did.
	primPoints                 = 0
	primLines                  = 1
	primLineLoop               = 2
	primLineStrip              = 3
	primTriangles              = 4
	primTriangleStrip          = 5
	primTriangleFan            = 6
	primQuads                  = 7
	primQuadStrip              = 8
	primPolygon                = 9
	primLinesAdjacency         = 10
	primLineStripAdjacency     = 11
	primTrianglesAdjacency     = 12
	primTriangleStripAdjacency = 13
	primPatches                = 14
	queryOcclusionCounter      = 0
	queryOcclusionPredicate    = 1
	queryTimestamp             = 2
	queryTimestampDisjoint     = 3
	queryTimeElapsed           = 4
	queryPrimitivesGenerated   = 5
	queryPrimitivesEmitted     = 6
	querySOStatistics          = 7
	querySOOverflowPredicate   = 8
	queryGPUFinished           = 9
	queryOcclusionConservative = 11
	querySOOverflowAny         = 12
	queryTypeI32               = 0
	queryTypeU32               = 1
	queryTypeI64               = 2
	queryTypeU64               = 3
	targetBuffer               = 0
	renderCondByRegionNoWait   = 3
)

// countedQuery reports whether the renderer counts the primitives of a
// query of type qtype itself, where it emulates transform feedback.
func countedQuery(qtype uint32) bool {
	switch qtype {
	case queryPrimitivesGenerated, queryPrimitivesEmitted, querySOStatistics, querySOOverflowPredicate, querySOOverflowAny:
		return true
	}
	return false
}

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
