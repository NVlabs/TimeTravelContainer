// SPDX-FileCopyrightText: Copyright 2007-2008 VMware, Inc.
// SPDX-FileCopyrightText: Copyright 2008 VMware, Inc.
// SPDX-FileCopyrightText: Copyright 2009-2010 VMware, Inc.
// SPDX-FileCopyrightText: Copyright 2012 VMware, Inc.
// SPDX-License-Identifier: MIT

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tgsi parses TGSI, Gallium's shader language, of the text that
// Mesa's virgl driver sends a virgl context's renderer: the text that Mesa's
// tgsi_dump_str prints, floats as their bits in hex, which Mesa's
// tgsi_text_translate parses. Print prints a shader as tgsi_dump_str does.
//
// The enumerations' values and names are Mesa 26.2.4's
// (p_shader_tokens.h, tgsi_strings.c).
package tgsi

import "strconv"

// Processor is the stage of the pipeline that a shader is of, as Mesa's
// mesa_shader_stage numbers them.
type Processor uint8

// The processors.
const (
	ProcessorVertex Processor = iota
	ProcessorTessCtrl
	ProcessorTessEval
	ProcessorGeometry
	ProcessorFragment
	ProcessorCompute
)

var processorNames = []string{"VERT", "TESS_CTRL", "TESS_EVAL", "GEOM", "FRAG", "COMP"}

func (p Processor) String() string { return enumName(processorNames, int(p)) }

// File is a register file.
type File uint8

// The register files.
const (
	FileNull File = iota
	FileConstant
	FileInput
	FileOutput
	FileTemporary
	FileSampler
	FileAddress
	FileImmediate
	FileSystemValue
	FileImage
	FileSamplerView
	FileBuffer
	FileMemory
	FileConstBuf
	FileHWAtomic
)

var fileNames = []string{"NULL", "CONST", "IN", "OUT", "TEMP", "SAMP", "ADDR", "IMM", "SV",
	"IMAGE", "SVIEW", "BUFFER", "MEMORY", "CONSTBUF", "HWATOMIC"}

func (f File) String() string { return enumName(fileNames, int(f)) }

// Semantic is the meaning of a declared input, output or system value.
type Semantic uint8

// The semantics that renderers treat apart.
const (
	SemanticPosition Semantic = iota
	SemanticColor
	SemanticBColor
	SemanticFog
	SemanticPSize
	SemanticGeneric
	SemanticNormal
	SemanticFace
	SemanticEdgeFlag
	SemanticPrimID
	SemanticInstanceID
	SemanticVertexID
	SemanticStencil
	SemanticClipDist
	SemanticClipVertex
	SemanticGridSize
	SemanticBlockID
	SemanticBlockSize
	SemanticThreadID
	SemanticTexCoord
	SemanticPCoord
	SemanticViewportIndex
	SemanticLayer
	SemanticSampleID
	SemanticSamplePos
	SemanticSampleMask
	SemanticInvocationID
	SemanticVertexIDNoBase
	SemanticBaseVertex
	SemanticPatch
	SemanticTessCoord
	SemanticTessOuter
	SemanticTessInner
	SemanticVerticesIn
	SemanticHelperInvocation
	SemanticBaseInstance
	SemanticDrawID
	SemanticWorkDim
	SemanticSubgroupSize
	SemanticSubgroupInvocation
	SemanticSubgroupEqMask
	SemanticSubgroupGeMask
	SemanticSubgroupGtMask
	SemanticSubgroupLeMask
	SemanticSubgroupLtMask
	SemanticCSUserDataAMD
	SemanticViewportMask
)

var semanticNames = []string{"POSITION", "COLOR", "BCOLOR", "FOG", "PSIZE", "GENERIC", "NORMAL",
	"FACE", "EDGEFLAG", "PRIM_ID", "INSTANCEID", "VERTEXID", "STENCIL", "CLIPDIST", "CLIPVERTEX",
	"GRID_SIZE", "BLOCK_ID", "BLOCK_SIZE", "THREAD_ID", "TEXCOORD", "PCOORD", "VIEWPORT_INDEX",
	"LAYER", "SAMPLEID", "SAMPLEPOS", "SAMPLEMASK", "INVOCATIONID", "VERTEXID_NOBASE",
	"BASEVERTEX", "PATCH", "TESSCOORD", "TESSOUTER", "TESSINNER", "VERTICESIN",
	"HELPER_INVOCATION", "BASEINSTANCE", "DRAWID", "WORK_DIM", "SUBGROUP_SIZE",
	"SUBGROUP_INVOCATION", "SUBGROUP_EQ_MASK", "SUBGROUP_GE_MASK", "SUBGROUP_GT_MASK",
	"SUBGROUP_LE_MASK", "SUBGROUP_LT_MASK", "CS_USER_DATA_AMD", "VIEWPORT_MASK"}

func (s Semantic) String() string { return enumName(semanticNames, int(s)) }

// TextureTarget is a texture's, a sampler view's or an image's kind.
type TextureTarget uint8

// The texture targets.
const (
	TextureBuffer TextureTarget = iota
	Texture1D
	Texture2D
	Texture3D
	TextureCube
	TextureRect
	TextureShadow1D
	TextureShadow2D
	TextureShadowRect
	Texture1DArray
	Texture2DArray
	TextureShadow1DArray
	TextureShadow2DArray
	TextureShadowCube
	Texture2DMSAA
	Texture2DArrayMSAA
	TextureCubeArray
	TextureShadowCubeArray
	TextureUnknown
)

var textureNames = []string{"BUFFER", "1D", "2D", "3D", "CUBE", "RECT", "SHADOW1D", "SHADOW2D",
	"SHADOWRECT", "1D_ARRAY", "2D_ARRAY", "SHADOW1D_ARRAY", "SHADOW2D_ARRAY", "SHADOWCUBE",
	"2D_MSAA", "2D_ARRAY_MSAA", "CUBEARRAY", "SHADOWCUBEARRAY", "UNKNOWN"}

func (t TextureTarget) String() string { return enumName(textureNames, int(t)) }

// ReturnType is the type of a sampler view's channels.
type ReturnType uint8

// The return types.
const (
	ReturnUnorm ReturnType = iota
	ReturnSnorm
	ReturnSint
	ReturnUint
	ReturnFloat
)

var returnTypeNames = []string{"UNORM", "SNORM", "SINT", "UINT", "FLOAT"}

func (r ReturnType) String() string { return enumName(returnTypeNames, int(r)) }

// Interpolation is how a fragment shader's input interpolates.
type Interpolation uint8

// The interpolations.
const (
	InterpolateConstant Interpolation = iota
	InterpolateLinear
	InterpolatePerspective
	InterpolateColor
)

var interpolationNames = []string{"CONSTANT", "LINEAR", "PERSPECTIVE", "COLOR"}

func (i Interpolation) String() string { return enumName(interpolationNames, int(i)) }

// InterpolationLocation is where in a pixel an input interpolates.
type InterpolationLocation uint8

// The interpolation locations.
const (
	LocationCenter InterpolationLocation = iota
	LocationCentroid
	LocationSample
)

var locationNames = []string{"CENTER", "CENTROID", "SAMPLE"}

func (l InterpolationLocation) String() string { return enumName(locationNames, int(l)) }

// PropertyName is a shader property's.
type PropertyName uint8

// The properties.
const (
	PropertyGSInputPrimitive PropertyName = iota
	PropertyGSOutputPrimitive
	PropertyGSMaxOutputVertices
	PropertyFSCoordOrigin
	PropertyFSCoordPixelCenter
	PropertyFSColor0WritesAllCbufs
	PropertyFSDepthLayout
	PropertyVSProhibitUCPs
	PropertyGSInvocations
	PropertyVSWindowSpacePosition
	PropertyTCSVerticesOut
	PropertyTESPrimMode
	PropertyTESSpacing
	PropertyTESVertexOrderCW
	PropertyTESPointMode
	PropertyNumClipDistEnabled
	PropertyNumCullDistEnabled
	PropertyFSEarlyDepthStencil
	PropertyFSPostDepthCoverage
	PropertyNextShader
	PropertyCSFixedBlockWidth
	PropertyCSFixedBlockHeight
	PropertyCSFixedBlockDepth
	PropertyLegacyMathRules
	PropertyVSBlitSGPRsAMD
	PropertyCSUserDataComponentsAMD
	PropertyLayerViewportRelative
	PropertyFSBlendEquationAdvanced
	PropertySeparableProgram
)

var propertyNames = []string{"GS_INPUT_PRIMITIVE", "GS_OUTPUT_PRIMITIVE", "GS_MAX_OUTPUT_VERTICES",
	"FS_COORD_ORIGIN", "FS_COORD_PIXEL_CENTER", "FS_COLOR0_WRITES_ALL_CBUFS", "FS_DEPTH_LAYOUT",
	"VS_PROHIBIT_UCPS", "GS_INVOCATIONS", "VS_WINDOW_SPACE_POSITION", "TCS_VERTICES_OUT",
	"TES_PRIM_MODE", "TES_SPACING", "TES_VERTEX_ORDER_CW", "TES_POINT_MODE",
	"NUM_CLIPDIST_ENABLED", "NUM_CULLDIST_ENABLED", "FS_EARLY_DEPTH_STENCIL",
	"FS_POST_DEPTH_COVERAGE", "NEXT_SHADER", "CS_FIXED_BLOCK_WIDTH", "CS_FIXED_BLOCK_HEIGHT",
	"CS_FIXED_BLOCK_DEPTH", "LEGACY_MATH_RULES", "VS_BLIT_SGPRS_AMD",
	"CS_USER_DATA_COMPONENTS_AMD", "LAYER_VIEWPORT_RELATIVE", "FS_BLEND_EQUATION_ADVANCED",
	"SEPARABLE_PROGRAM"}

func (p PropertyName) String() string { return enumName(propertyNames, int(p)) }

// The values of the properties that name them: primitives, as Mesa's
// mesa_prim numbers them, the fragment coordinates' origin and pixel
// center, and the next shader's processor.
var (
	primitiveNames = []string{"POINTS", "LINES", "LINE_LOOP", "LINE_STRIP", "TRIANGLES",
		"TRIANGLE_STRIP", "TRIANGLE_FAN", "QUADS", "QUAD_STRIP", "POLYGON", "LINES_ADJACENCY",
		"LINE_STRIP_ADJACENCY", "TRIANGLES_ADJACENCY", "TRIANGLE_STRIP_ADJACENCY", "PATCHES"}
	coordOriginNames      = []string{"UPPER_LEFT", "LOWER_LEFT"}
	coordPixelCenterNames = []string{"HALF_INTEGER", "INTEGER"}
)

// propertyValueNames returns the names of property's values, or nil if its
// values are numbers.
func propertyValueNames(name PropertyName) []string {
	switch name {
	case PropertyGSInputPrimitive, PropertyGSOutputPrimitive:
		return primitiveNames
	case PropertyFSCoordOrigin:
		return coordOriginNames
	case PropertyFSCoordPixelCenter:
		return coordPixelCenterNames
	case PropertyNextShader:
		return processorNames
	}
	return nil
}

// ImmediateType is an immediate's type.
type ImmediateType uint8

// The immediate types.
const (
	ImmFloat32 ImmediateType = iota
	ImmUint32
	ImmInt32
	ImmFloat64
	ImmUint64
	ImmInt64
)

var immediateTypeNames = []string{"FLT32", "UINT32", "INT32", "FLT64", "UINT64", "INT64"}

func (t ImmediateType) String() string { return enumName(immediateTypeNames, int(t)) }

// MemoryType is the memory that a MEMORY declaration's registers are of.
type MemoryType uint8

// The memory types.
const (
	MemoryGlobal MemoryType = iota
	MemoryShared
	MemoryPrivate
	MemoryInput
)

var memoryTypeNames = []string{"GLOBAL", "SHARED", "PRIVATE", "INPUT"}

// The memory qualifiers of an instruction that accesses memory, as bits.
const (
	MemoryCoherent          = 1 << 0
	MemoryRestrict          = 1 << 1
	MemoryVolatile          = 1 << 2
	MemoryStreamCachePolicy = 1 << 3
)

var memoryQualifierNames = []string{"COHERENT", "RESTRICT", "VOLATILE", "STREAM_CACHE_POLICY"}

// The channels of a register, as a write mask's bits and as swizzles'
// values.
const (
	X = 0
	Y = 1
	Z = 2
	W = 3

	WriteMaskXYZW = 0xf
)

var channelNames = []string{"x", "y", "z", "w"}

// enumName returns names[e], or e's number if names has none, as Mesa's
// dump_enum prints them.
func enumName(names []string, e int) string {
	if e >= 0 && e < len(names) {
		return names[e]
	}
	return strconv.Itoa(e)
}

// enumValue returns the value of the enumeration whose name is s.
func enumValue(names []string, s string) (int, bool) {
	for i, n := range names {
		if n == s {
			return i, true
		}
	}
	return 0, false
}
