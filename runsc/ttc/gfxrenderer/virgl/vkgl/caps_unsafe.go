// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

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
	v1.MaxSamples = 1
	for s := uint32(16); s > 1; s /= 2 {
		if l.FramebufferColorSampleCounts&s != 0 {
			v1.MaxSamples = s
			break
		}
	}
	// Formats are of multisampled textures, as guests' drivers take them,
	// of every count of samples up to the most, of which textures are of
	// the next count that the format has (roundSamples): of the most,
	// then, of their renderings' and their samples', as shaders fetch them.
	multisampled := func(counts uint32) bool {
		return v1.MaxSamples > 1 && counts&v1.MaxSamples != 0
	}
	for vf := range formats {
		ft, _ := d.LookupFormat(vf)
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
				if multisampled(d.sampleCounts(ft)) {
					c.SupportedMultisampleFormats.set(vf)
				}
			}
		case opt&featureColorAttachment != 0 && !ft.Compressed:
			v1.Render.set(vf)
			if multisampled(d.sampleCounts(ft)) {
				c.SupportedMultisampleFormats.set(vf)
			}
		}
		if props.BufferFeatures&featureVertexBuffer != 0 && ft.Swizzle == "XYZW" {
			v1.VertexBuffer.set(vf)
		}
	}
	// Of framebuffers of no attachments, PIPE_FORMAT_NONE's.
	if multisampled(l.FramebufferNoAttachmentsSampleCounts) {
		c.SupportedMultisampleFormats.set(0)
	}
	for vf := range viewFormats {
		// Of views alone, which shaders sample.
		ft, _ := d.LookupFormat(vf)
		if d.formatProperties(ft.VK).OptimalTilingFeatures&featureSampledImage != 0 {
			v1.Sampler.set(vf)
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
	if f.VertexPipelineStoresAndAtomics != 0 {
		// Of which vertex shaders capture their stream outputs.
		v1.MaxStreamoutBuffers = MaxStreamOutputBuffers
	}
	if f.DualSrcBlend != 0 {
		v1.MaxDualSourceRenderTargets = l.MaxFragmentDualSrcAttachments
	}
	v1.MaxRenderTargets = min(l.MaxColorAttachments, MaxColorBuffers)
	v1.PrimMask = vkPrimMask
	if !d.triangleFans {
		v1.PrimMask &^= 1 << primTriangleFan
	}
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
	// tessellation, and texture buffers. TODO: of OpenGL ES 3.2.
	v1.BoolSet1 &^= capHasTessellationShaders
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
	// Shader buffers and images, of the stages that store to memory, the
	// vertex shader's less the renderer's, and of compute, of no
	// geometry and tessellation shaders. Guests have atomic counters of
	// shader buffers, of none of the hardware's.
	stageBuffers := min(l.MaxPerStageDescriptorStorageBuffers, 16)
	stageImages := min(l.MaxPerStageDescriptorStorageImages, 8)
	var vertexBuffers, fragmentBuffers, vertexImages, fragmentImages uint32
	if f.VertexPipelineStoresAndAtomics != 0 {
		vertexBuffers, vertexImages = max(stageBuffers, ssbos)-ssbos, stageImages
	}
	if f.FragmentStoresAndAtomics != 0 {
		fragmentBuffers, fragmentImages = stageBuffers, stageImages
	}
	c.MaxShaderStorageBlocks = [6]uint32{stageVertex: vertexBuffers, stageFragment: fragmentBuffers, stageCompute: stageBuffers}
	c.MaxShaderBufferFragCompute, c.MaxShaderBufferOtherStages = fragmentBuffers, vertexBuffers
	c.MaxShaderImageFragCompute, c.MaxShaderImageOtherStages = fragmentImages, vertexImages
	c.MaxCombinedShaderBuffers = min(l.MaxDescriptorSetStorageBuffers, vertexBuffers+fragmentBuffers+stageBuffers)
	// Of compute's shared memory, the most that the renderer's shaders
	// have.
	c.MaxComputeWorkGroupInvocations = l.MaxComputeWorkGroupInvocations
	c.MaxComputeSharedMemorySize = min(l.MaxComputeSharedMemorySize, tospirv.MaxSharedMemory)
	c.MaxComputeGridSize = l.MaxComputeWorkGroupCount
	c.MaxComputeBlockSize = l.MaxComputeWorkGroupSize
	c.MaxTexture2DSize = l.MaxImageDimension2D
	c.MaxTexture3DSize = l.MaxImageDimension3D
	c.MaxTextureCubeSize = l.MaxImageDimensionCube
	for i := range c.MaxConstBufferSize {
		c.MaxConstBufferSize[i] = min(l.MaxUniformBufferRange, 64<<10)
	}
	if l.StandardSampleLocations != 0 {
		c.SampleLocations = standardSampleLocations()
	}
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
	// Nor geometry and tessellation shaders, as above.
	c.MaxGeomOutputVertices, c.MaxGeomTotalOutputComponents, c.MaxShaderPatchVaryings = 0, 0, 0
	c.MaxTCSOutputs, c.MaxTESOutputs = 0, 0
	return encodeCaps(&c)
}

// virgl's shader stages, of the caps' arrays of each.
const (
	stageVertex   = 0
	stageFragment = 1
	stageCompute  = 5
)

// standardSampleLocations returns Vulkan's standard locations of samples,
// in sixteenths of a pixel, of 2, 4, 8 and 16 samples, as virgl's caps
// have them: of each sample a byte of x and y, from words 0, 1, 2 and 4.
func standardSampleLocations() [8]uint32 {
	var words [8]uint32
	for k, locations := range [][][2]uint32{
		{{12, 12}, {4, 4}},
		{{6, 2}, {14, 6}, {2, 10}, {10, 14}},
		{{9, 5}, {7, 11}, {13, 9}, {5, 3}, {3, 13}, {1, 7}, {11, 15}, {15, 1}},
		{{9, 9}, {7, 5}, {5, 10}, {12, 7}, {3, 6}, {10, 13}, {13, 11}, {11, 3}, {6, 14}, {8, 1}, {4, 2}, {2, 12},
			{0, 8}, {15, 4}, {14, 15}, {1, 0}},
	} {
		first := []int{0, 1, 2, 4}[k]
		for i, p := range locations {
			words[first+i/4] |= (p[0]<<4 | p[1]) << (8 * (i % 4))
		}
	}
	return words
}

// sampleCounts returns the counts of samples, of VkSampleCountFlagBits, of
// which the device renders textures of ft, and shaders fetch them.
func (d *Device) sampleCounts(ft Format) uint32 {
	l := &d.props.Limits
	if ft.IsDepth() || ft.IsStencil() {
		counts := ^uint32(0)
		if ft.IsDepth() {
			counts &= l.FramebufferDepthSampleCounts & l.SampledImageDepthSampleCounts
		}
		if ft.IsStencil() {
			counts &= l.FramebufferStencilSampleCounts & l.SampledImageStencilSampleCounts
		}
		return counts
	}
	sampled := l.SampledImageColorSampleCounts
	if integer, _ := ft.IsInteger(); integer {
		sampled = l.SampledImageIntegerSampleCounts
	}
	return l.FramebufferColorSampleCounts & sampled
}

// NoAttachmentSamples returns the samples of a framebuffer of no
// attachments that a guest asked n of: the next count that the device
// has, as of textures (roundSamples), or 1 of none.
func (d *Device) NoAttachmentSamples(n uint32) uint32 {
	return max(roundSamples(d.props.Limits.FramebufferNoAttachmentsSampleCounts, n), 1)
}
