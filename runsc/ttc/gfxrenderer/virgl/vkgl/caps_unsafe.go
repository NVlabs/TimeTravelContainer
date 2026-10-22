// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import "encoding/binary"

// Format features of buffers, of VkFormatFeatureFlagBits.
const featureVertexBuffer = 0x40

// Caps returns virgl's capability set set, 1 or 2, of what the device
// renders, as the decoder's screen.c fills Zink's. ubos and ssbos are the
// uniform and shader buffer slots of each stage that the renderer keeps
// for itself, of transform feedback's emulation.
func (d *Device) Caps(set uint32, ubos, ssbos uint32) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.a.reset()
	l := &d.props.Limits
	f := &d.features10
	var c capsV2
	v1 := &c.V1
	v1.MaxVersion = 2
	if set == 1 {
		v1.MaxVersion = 1
	}
	for vf, ft := range formats {
		props := d.formatProperties(ft.VK)
		opt := props.OptimalTilingFeatures
		if opt&featureSampledImage != 0 {
			v1.Sampler.set(vf)
			c.SupportedReadbackFormats.set(vf)
		}
		switch {
		case ft.IsDepth() || ft.IsStencil():
			if opt&featureDepthStencil != 0 {
				v1.DepthStencil.set(vf)
			}
		case opt&featureColorAttachment != 0 && !ft.Compressed:
			v1.Render.set(vf)
			if l.FramebufferColorSampleCounts&4 != 0 {
				c.SupportedMultisampleFormats.set(vf)
			}
		}
		if props.BufferFeatures&featureVertexBuffer != 0 && ft.Swizzle == "XYZW" {
			v1.VertexBuffer.set(vf)
		}
	}
	for _, s := range []uint32{1, 2, 67, 134, 7, 8} {
		// B8G8R8A8, B8G8R8X8, R8G8B8A8, R8G8B8X8, B5G6R5, R10G10B10A2.
		if v1.Render.has(s) {
			c.Scanout.set(s)
		}
	}
	bools := uint32(capIndepBlendFunc | capStartInstance | capPrimitiveRestart | capBlendEqSep | capInstanceID |
		capSeamlessCubeMap | capStreamoutPauseResume | capTextureMultisample | capFragmentCoordConventions |
		capUBO | capTextureQueryLod | capHasIndirectDraw | capDerivativeControl | capOcclusionQuery)
	flag := func(feature uint32, bit uint32) {
		if feature != 0 {
			bools |= bit
		}
	}
	flag(d.timestampBits, capTimerQuery)
	if d.divisors {
		bools |= capVertexElementInstanceDivisor
	}
	flag(f.IndependentBlend, capIndepBlendEnable)
	flag(f.ImageCubeArray, capCubeMapArray)
	flag(f.DepthClamp, capDepthClipDisable)
	flag(f.TessellationShader, capHasTessellationShaders)
	flag(f.SampleRateShading, capHasSampleShading)
	flag(f.ShaderCullDistance, capHasCull)
	flag(f.DepthBiasClamp, capPolygonOffsetClamp)
	v1.BoolSet1 = bools
	v1.GLSLLevel = 450
	v1.MaxTextureArrayLayers = l.MaxImageArrayLayers
	v1.MaxStreamoutBuffers = MaxStreamOutputBuffers
	if f.DualSrcBlend != 0 {
		v1.MaxDualSourceRenderTargets = l.MaxFragmentDualSrcAttachments
	}
	v1.MaxRenderTargets = min(l.MaxColorAttachments, MaxColorBuffers)
	v1.MaxSamples = 1
	for s := uint32(16); s > 1; s /= 2 {
		if l.FramebufferColorSampleCounts&s != 0 {
			v1.MaxSamples = s
			break
		}
	}
	v1.PrimMask = vkPrimMask
	if f.TessellationShader == 0 {
		v1.PrimMask &^= 1 << 14 // MESA_PRIM_PATCHES
	}
	if f.GeometryShader == 0 {
		v1.PrimMask &^= 1<<10 | 1<<11 | 1<<12 | 1<<13
	}
	v1.MaxTBOSize = l.MaxTexelBufferElements
	// Uniform buffers besides the default one, buffer 0, less the
	// renderer's.
	v1.MaxUniformBlocks = min(l.MaxPerStageDescriptorUniformBuffers, 16) - 1 - ubos
	v1.MaxViewports = 1
	if f.MultiViewport != 0 {
		v1.MaxViewports = min(l.MaxViewports, 16)
	}
	v1.MaxTextureGatherComponents = 4
	// What the backend renders none of yet, which guests are offered not:
	// indirect draws, tessellation, and texture buffers.
	v1.BoolSet1 &^= capHasIndirectDraw | capHasTessellationShaders
	v1.PrimMask &^= 1 << 14 // MESA_PRIM_PATCHES
	v1.MaxTBOSize = 0
	if set == 1 {
		return encodeCaps(&c)[:binary.Size(capsV1{})]
	}

	c.MinAliasedPointSize, c.MaxAliasedPointSize = l.PointSizeRange[0], l.PointSizeRange[1]
	c.MinSmoothPointSize, c.MaxSmoothPointSize = l.PointSizeRange[0], l.PointSizeRange[1]
	c.MinAliasedLineWidth, c.MaxAliasedLineWidth = l.LineWidthRange[0], l.LineWidthRange[1]
	c.MinSmoothLineWidth, c.MaxSmoothLineWidth = l.LineWidthRange[0], l.LineWidthRange[1]
	c.MaxTextureLodBias = l.MaxSamplerLodBias
	if f.GeometryShader != 0 {
		c.MaxGeomOutputVertices = l.MaxGeometryOutputVertices
		c.MaxGeomTotalOutputComponents = l.MaxGeometryTotalOutputComponents
	}
	c.MaxVertexOutputs = min(l.MaxVertexOutputComponents/4, 32)
	c.MaxVertexAttribs = min(l.MaxVertexInputAttributes, 32)
	if f.TessellationShader != 0 {
		c.MaxShaderPatchVaryings = min(l.MaxTessellationControlPerPatchOutputComponents/4, 30)
	}
	c.MinTexelOffset, c.MaxTexelOffset = l.MinTexelOffset, int32(l.MaxTexelOffset)
	c.MinTextureGatherOffset, c.MaxTextureGatherOffset = l.MinTexelGatherOffset, int32(l.MaxTexelGatherOffset)
	c.TextureBufferOffsetAlignment = uint32(l.MinTexelBufferOffsetAlignment)
	c.UniformBufferOffsetAlignment = uint32(l.MinUniformBufferOffsetAlignment)
	c.ShaderBufferOffsetAlignment = uint32(l.MinStorageBufferOffsetAlignment)
	c.CapabilityBits = capTGSIInvariant | capSetMinSamples | capTGSIPrecise | capMemoryBarrier | capClearTexture |
		capBindCommandArgs | capTXQS | capComputeShader | capFBNoAttach | capTextureView | capRobustBufferAccess |
		capTextureBarrier | capFBOMixedFormats | capClipHalfZ
	if f.MultiDrawIndirect != 0 {
		c.CapabilityBits |= capMultiDrawIndirect
	}
	c.MaxVertexAttribStride = min(l.MaxVertexInputBindingStride, 2048)
	stageBuffers := min(l.MaxPerStageDescriptorStorageBuffers, 16)
	c.MaxShaderBufferFragCompute = stageBuffers
	if f.VertexPipelineStoresAndAtomics != 0 {
		c.MaxShaderBufferOtherStages = stageBuffers - ssbos
	}
	stageImages := min(l.MaxPerStageDescriptorStorageImages, 8)
	if f.FragmentStoresAndAtomics != 0 {
		c.MaxShaderImageFragCompute = stageImages
	}
	if f.VertexPipelineStoresAndAtomics != 0 {
		c.MaxShaderImageOtherStages = stageImages
	}
	c.MaxComputeWorkGroupInvocations = l.MaxComputeWorkGroupInvocations
	c.MaxComputeSharedMemorySize = l.MaxComputeSharedMemorySize
	c.MaxComputeGridSize = l.MaxComputeWorkGroupCount
	c.MaxComputeBlockSize = l.MaxComputeWorkGroupSize
	c.MaxTexture2DSize = l.MaxImageDimension2D
	c.MaxTexture3DSize = l.MaxImageDimension3D
	c.MaxTextureCubeSize = l.MaxImageDimensionCube
	c.MaxCombinedShaderBuffers = min(l.MaxDescriptorSetStorageBuffers, 6*stageBuffers)
	for i := range c.MaxConstBufferSize {
		c.MaxConstBufferSize[i] = min(l.MaxUniformBufferRange, 64<<10)
		c.MaxShaderStorageBlocks[i] = stageBuffers
	}
	c.MaxShaderStorageBlocks[0] -= ssbos
	// virglrenderer's: the guest relies on the host's behaviors of every
	// version up to it, which this host has.
	c.HostFeatureCheckVersion = 23
	copy(c.Renderer[:len(c.Renderer)-1], "vkgl ("+d.Name+")")
	c.MaxAnisotropy = l.MaxSamplerAnisotropy
	c.MaxTextureSamplers = min(l.MaxPerStageDescriptorSampledImages, l.MaxPerStageDescriptorSamplers, 32)
	c.MaxUniformBlockSize = min(l.MaxUniformBufferRange, 64<<10)
	if f.TessellationShader != 0 {
		c.MaxTCSOutputs = min(l.MaxTessellationControlPerVertexOutputComponents/4, 32)
		c.MaxTESOutputs = min(l.MaxTessellationEvaluationOutputComponents/4, 32)
	}
	// Nor geometry and tessellation shaders, compute, shader buffers and
	// images, and multiple indirect draws.
	c.CapabilityBits &^= capComputeShader | capMultiDrawIndirect
	c.MaxGeomOutputVertices, c.MaxGeomTotalOutputComponents, c.MaxShaderPatchVaryings = 0, 0, 0
	c.MaxTCSOutputs, c.MaxTESOutputs = 0, 0
	c.MaxShaderBufferFragCompute, c.MaxShaderBufferOtherStages = 0, 0
	c.MaxShaderImageFragCompute, c.MaxShaderImageOtherStages = 0, 0
	c.MaxCombinedShaderBuffers, c.MaxShaderStorageBlocks = 0, [6]uint32{}
	c.MaxComputeWorkGroupInvocations, c.MaxComputeSharedMemorySize = 0, 0
	c.MaxComputeGridSize, c.MaxComputeBlockSize = [3]uint32{}, [3]uint32{}
	return encodeCaps(&c)
}
