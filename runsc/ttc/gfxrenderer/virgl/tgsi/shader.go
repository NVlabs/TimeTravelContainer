// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

// Shader is a TGSI shader: its processor, and its tokens in their order,
// which its properties, declarations, immediates and instructions hold.
type Shader struct {
	Processor    Processor
	Properties   []Property
	Declarations []Declaration
	Immediates   []Immediate
	Instructions []Instruction

	// Order is the order of the shader's properties, declarations and
	// immediates, which interleave before its instructions, as their kinds
	// and indexes.
	Order []TokenRef
}

// TokenKind is the kind of a shader's token.
type TokenKind uint8

// The kinds of tokens that Order orders.
const (
	TokenProperty TokenKind = iota
	TokenDeclaration
	TokenImmediate
)

// TokenRef is a token of a shader: the Index-th of its Kind.
type TokenRef struct {
	Kind  TokenKind
	Index int
}

// Property is a shader's property: its name, and its value.
type Property struct {
	Name  PropertyName
	Value uint32
}

// Declaration declares a range of a file's registers.
type Declaration struct {
	File File
	// Dimension is whether the declaration has a second dimension, whose
	// index is Index2D, as a constant buffer's declaration has its buffer.
	Dimension bool
	Index2D   int
	// First and Last are the registers declared.
	First, Last int
	// UsageMask is the channels used, a write mask.
	UsageMask uint8
	// ArrayID is the array that the registers are, or 0.
	ArrayID int
	Local   bool

	// HasSemantic is whether the registers have a semantic: Semantic, of
	// index SemanticIndex, of vertex streams Streams, a geometry shader's
	// output's for each channel.
	HasSemantic   bool
	Semantic      Semantic
	SemanticIndex int
	Streams       [4]uint8

	// Of an IMAGE declaration: its kind, its format, as Mesa names
	// pipe_formats, and whether it is written and raw.
	ImageTarget   TextureTarget
	ImageFormat   string
	ImageWritable bool
	ImageRaw      bool

	// Of a BUFFER declaration: whether it holds atomic counters.
	Atomic bool

	// Of a MEMORY declaration: its type.
	MemoryType MemoryType

	// Of a SVIEW declaration: its kind, and the type of each channel.
	ViewTarget  TextureTarget
	ReturnTypes [4]ReturnType

	// Of a fragment shader's input: its interpolation, and where.
	Interpolate bool
	Interp      Interpolation
	Location    InterpolationLocation

	Invariant bool
}

// Immediate is an immediate: its type, and its values, of which a 64-bit
// type's take two.
type Immediate struct {
	Type   ImmediateType
	Values []uint32
}

// Instruction is an instruction.
type Instruction struct {
	Opcode    Opcode
	Saturate  bool
	Precise   bool
	Dst       []DstRegister
	Src       []SrcRegister
	Texture   TextureTarget
	TexOffset []TexOffset
	// Of an instruction that accesses memory: its qualifiers, the texture
	// kind and the format of the image that it accesses.
	Memory        uint32
	MemoryTexture TextureTarget
	MemoryFormat  string
	// Label is the instruction that a branch goes to, if HasLabel.
	HasLabel bool
	Label    int
}

// Indirect is a register's index of another register's channel, the
// address register's mostly, plus the index that it has.
type Indirect struct {
	File    File
	Index   int
	Swizzle uint8
	// ArrayID is the array that the register indexes, or 0.
	ArrayID int
}

// Register is a register of an instruction.
type Register struct {
	File  File
	Index int
	// Indirect, if not nil, adds a register's channel to Index.
	Indirect *Indirect
	// Dimension is whether the register has a second dimension, before its
	// index: a constant's buffer, or a geometry shader input's vertex.
	Dimension   bool
	DimIndex    int
	DimIndirect *Indirect
}

// DstRegister is a destination register: a register, and the channels
// that the instruction writes.
type DstRegister struct {
	Register
	WriteMask uint8
}

// SrcRegister is a source register: a register, its channels in the order
// that the instruction reads them, and their modifiers.
type SrcRegister struct {
	Register
	Swizzle  [4]uint8
	Negate   bool
	Absolute bool
}

// TexOffset is a texture instruction's offset: a register's three channels.
type TexOffset struct {
	File    File
	Index   int
	Swizzle [3]uint8
}

// identitySwizzle reads every channel of its own.
var identitySwizzle = [4]uint8{X, Y, Z, W}
