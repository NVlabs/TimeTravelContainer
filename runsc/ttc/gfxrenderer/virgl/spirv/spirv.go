// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package spirv builds SPIR-V modules, of the shaders that the renderer
// translates guests' shaders to: their types and constants, which it makes
// once each, their decorations, their global variables, and their
// functions' instructions, in the sections that SPIR-V lays them out in.
package spirv

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// Magic is SPIR-V's magic number, a module's first word.
const Magic = 0x07230203

// Version is the SPIR-V version of the modules built: 1.3, which every
// Vulkan 1.1 driver takes, of which the StorageBuffer storage class is core.
const Version = 0x00010300

// ID is a result's ID.
type ID uint32

// Module builds a SPIR-V module.
type Module struct {
	bound        ID
	capabilities []Capability
	hasCap       map[Capability]bool
	extensions   []string
	glsl         ID

	entryPoints    [][]uint32
	executionModes [][]uint32
	names          [][]uint32
	annotations    [][]uint32
	globals        [][]uint32
	functions      [][]uint32

	// cache are the types and constants made, by their instructions' words
	// but their result IDs.
	cache map[string]ID
}

// NewModule returns an empty module, of the Shader capability and the
// logical addressing and GLSL450 memory models.
func NewModule() *Module {
	m := &Module{bound: 1, hasCap: make(map[Capability]bool), cache: make(map[string]ID)}
	m.Capability(CapabilityShader)
	return m
}

// NewID returns a new result ID.
func (m *Module) NewID() ID {
	id := m.bound
	m.bound++
	return id
}

// Capability declares a capability, once.
func (m *Module) Capability(c Capability) {
	if !m.hasCap[c] {
		m.hasCap[c] = true
		m.capabilities = append(m.capabilities, c)
	}
}

// Extension declares an extension, once.
func (m *Module) Extension(name string) {
	for _, e := range m.extensions {
		if e == name {
			return
		}
	}
	m.extensions = append(m.extensions, name)
}

// GLSL returns the ID of GLSL.std.450's import.
func (m *Module) GLSL() ID {
	if m.glsl == 0 {
		m.glsl = m.NewID()
	}
	return m.glsl
}

// inst returns an instruction's words.
func inst(op Op, operands ...uint32) []uint32 {
	words := make([]uint32, 0, 1+len(operands))
	words = append(words, uint32(len(operands)+1)<<16|uint32(op))
	return append(words, operands...)
}

// String returns s's words: its UTF-8 bytes, NUL-terminated, padded to a
// word.
func String(s string) []uint32 {
	b := append([]byte(s), 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	words := make([]uint32, len(b)/4)
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return words
}

// EntryPoint declares fn an entry point of model, named name, whose
// interface is the input and output variables that it uses.
func (m *Module) EntryPoint(model ExecutionModel, fn ID, name string, interfaces ...ID) {
	ops := append([]uint32{uint32(model), uint32(fn)}, String(name)...)
	for _, id := range interfaces {
		ops = append(ops, uint32(id))
	}
	m.entryPoints = append(m.entryPoints, inst(OpEntryPoint, ops...))
}

// ExecutionMode declares an execution mode of entry point fn.
func (m *Module) ExecutionMode(fn ID, mode ExecutionMode, literals ...uint32) {
	m.executionModes = append(m.executionModes,
		inst(OpExecutionMode, append([]uint32{uint32(fn), uint32(mode)}, literals...)...))
}

// Name names id, for debuggers.
func (m *Module) Name(id ID, name string) {
	m.names = append(m.names, inst(OpName, append([]uint32{uint32(id)}, String(name)...)...))
}

// Decorate decorates target.
func (m *Module) Decorate(target ID, d Decoration, literals ...uint32) {
	m.annotations = append(m.annotations,
		inst(OpDecorate, append([]uint32{uint32(target), uint32(d)}, literals...)...))
}

// MemberDecorate decorates a member of a struct type.
func (m *Module) MemberDecorate(structType ID, member uint32, d Decoration, literals ...uint32) {
	m.annotations = append(m.annotations,
		inst(OpMemberDecorate, append([]uint32{uint32(structType), member, uint32(d)}, literals...)...))
}

// global returns the ID of a type or constant of op and operands, which it
// makes once, if cached, or anew.
func (m *Module) global(op Op, cached bool, resultType ID, operands ...uint32) ID {
	var key string
	if cached {
		var b strings.Builder
		b.WriteString(strconv.Itoa(int(op)))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(int(resultType)))
		for _, o := range operands {
			b.WriteByte(',')
			b.WriteString(strconv.FormatUint(uint64(o), 10))
		}
		key = b.String()
		if id, ok := m.cache[key]; ok {
			return id
		}
	}
	id := m.NewID()
	var ops []uint32
	if resultType != 0 {
		ops = append(ops, uint32(resultType))
	}
	ops = append(ops, uint32(id))
	ops = append(ops, operands...)
	m.globals = append(m.globals, inst(op, ops...))
	if cached {
		m.cache[key] = id
	}
	return id
}

// The types, which the module makes once each, but structs.

func (m *Module) TypeVoid() ID { return m.global(OpTypeVoid, true, 0) }
func (m *Module) TypeBool() ID { return m.global(OpTypeBool, true, 0) }

// TypeInt returns an integer type of width bits, signed or not.
func (m *Module) TypeInt(width uint32, signed bool) ID {
	s := uint32(0)
	if signed {
		s = 1
	}
	return m.global(OpTypeInt, true, 0, width, s)
}

// TypeFloat returns a float type of width bits.
func (m *Module) TypeFloat(width uint32) ID { return m.global(OpTypeFloat, true, 0, width) }

// TypeVector returns a vector of count components of type component.
func (m *Module) TypeVector(component ID, count uint32) ID {
	return m.global(OpTypeVector, true, 0, uint32(component), count)
}

// TypeArray returns an array of elem, of length, a constant's ID. Arrays
// that a decoration, such as ArrayStride, tells apart are TypeArrayNew's.
func (m *Module) TypeArray(elem, length ID) ID {
	return m.global(OpTypeArray, true, 0, uint32(elem), uint32(length))
}

// TypeArrayNew returns an array type of its own, which its decorations
// tell apart from the module's others.
func (m *Module) TypeArrayNew(elem, length ID) ID {
	return m.global(OpTypeArray, false, 0, uint32(elem), uint32(length))
}

// TypeRuntimeArray returns an array of elem of no length, of its own.
func (m *Module) TypeRuntimeArray(elem ID) ID {
	return m.global(OpTypeRuntimeArray, false, 0, uint32(elem))
}

// TypeStruct returns a struct type of its own, of members.
func (m *Module) TypeStruct(members ...ID) ID {
	ops := make([]uint32, len(members))
	for i, t := range members {
		ops[i] = uint32(t)
	}
	return m.global(OpTypeStruct, false, 0, ops...)
}

// TypePointer returns a pointer to t, of storage class sc.
func (m *Module) TypePointer(sc StorageClass, t ID) ID {
	return m.global(OpTypePointer, true, 0, uint32(sc), uint32(t))
}

// TypeFunction returns a function type.
func (m *Module) TypeFunction(ret ID, params ...ID) ID {
	ops := []uint32{uint32(ret)}
	for _, p := range params {
		ops = append(ops, uint32(p))
	}
	return m.global(OpTypeFunction, true, 0, ops...)
}

// TypeImage returns an image type: of sampledType's texels, of dim, a depth
// image or not (0, 1, or 2 for unknown), arrayed, multisampled, sampled (1)
// or a storage image (2), and of format.
func (m *Module) TypeImage(sampledType ID, dim Dim, depth, arrayed, ms, sampled uint32, format ImageFormat) ID {
	return m.global(OpTypeImage, true, 0, uint32(sampledType), uint32(dim), depth, arrayed, ms, sampled, uint32(format))
}

// TypeSampledImage returns the type of image's images combined with
// samplers.
func (m *Module) TypeSampledImage(image ID) ID {
	return m.global(OpTypeSampledImage, true, 0, uint32(image))
}

// TypeSampler returns the sampler type.
func (m *Module) TypeSampler() ID { return m.global(OpTypeSampler, true, 0) }

// The constants, which the module makes once each.

// Constant returns a scalar constant of type t, of its words.
func (m *Module) Constant(t ID, words ...uint32) ID { return m.global(OpConstant, true, t, words...) }

// ConstantUint returns a 32-bit unsigned integer constant.
func (m *Module) ConstantUint(v uint32) ID { return m.Constant(m.TypeInt(32, false), v) }

// ConstantInt returns a 32-bit signed integer constant.
func (m *Module) ConstantInt(v int32) ID { return m.Constant(m.TypeInt(32, true), uint32(v)) }

// ConstantFloat returns a 32-bit float constant of f.
func (m *Module) ConstantFloat(f float32) ID {
	return m.Constant(m.TypeFloat(32), math.Float32bits(f))
}

// ConstantBool returns a boolean constant.
func (m *Module) ConstantBool(b bool) ID {
	if b {
		return m.global(OpConstantTrue, true, m.TypeBool())
	}
	return m.global(OpConstantFalse, true, m.TypeBool())
}

// ConstantComposite returns a composite constant of t, of parts.
func (m *Module) ConstantComposite(t ID, parts ...ID) ID {
	ops := make([]uint32, len(parts))
	for i, p := range parts {
		ops[i] = uint32(p)
	}
	return m.global(OpConstantComposite, true, t, ops...)
}

// ConstantNull returns t's zero.
func (m *Module) ConstantNull(t ID) ID { return m.global(OpConstantNull, true, t) }

// Variable returns a global variable of type ptrType, a pointer of storage
// class sc, of the initializer init, if any.
func (m *Module) Variable(ptrType ID, sc StorageClass, init ...ID) ID {
	ops := []uint32{uint32(sc)}
	for _, i := range init {
		ops = append(ops, uint32(i))
	}
	return m.global(OpVariable, false, ptrType, ops...)
}

// Function builds a function's instructions.
type Function struct {
	m    *Module
	id   ID
	head []uint32
	// vars are the function's variables, which SPIR-V has at its first
	// block's start.
	vars   []uint32
	blocks []uint32
	// entry is whether the first block's label was emitted, after which
	// the variables go.
	entry int
}

// Function begins a function of type fnType, which returns ret.
func (m *Module) Function(ret, fnType ID) *Function {
	f := &Function{m: m, id: m.NewID()}
	f.head = inst(OpFunction, uint32(ret), uint32(f.id), uint32(FunctionControlNone), uint32(fnType))
	return f
}

// ID returns the function's ID.
func (f *Function) ID() ID { return f.id }

// Label begins a block, of a new label, and returns it.
func (f *Function) Label() ID {
	id := f.m.NewID()
	f.Begin(id)
	return id
}

// Begin begins a block of label id, which an earlier branch named.
func (f *Function) Begin(id ID) {
	f.blocks = append(f.blocks, inst(OpLabel, uint32(id))...)
	if f.entry == 0 {
		f.entry = len(f.blocks)
	}
}

// Variable returns a variable of the function, of type ptrType, a
// Function pointer.
func (f *Function) Variable(ptrType ID) ID {
	id := f.m.NewID()
	f.vars = append(f.vars, inst(OpVariable, uint32(ptrType), uint32(id), uint32(StorageClassFunction))...)
	return id
}

// Op emits an instruction that has a result, of type resultType, and
// returns the result's ID.
func (f *Function) Op(op Op, resultType ID, operands ...uint32) ID {
	id := f.m.NewID()
	f.blocks = append(f.blocks, inst(op, append([]uint32{uint32(resultType), uint32(id)}, operands...)...)...)
	return id
}

// Do emits an instruction that has no result.
func (f *Function) Do(op Op, operands ...uint32) {
	f.blocks = append(f.blocks, inst(op, operands...)...)
}

// End ends the function, whose last block ended, and adds it to the module.
func (f *Function) End() {
	words := append([]uint32(nil), f.head...)
	words = append(words, f.blocks[:f.entry]...)
	words = append(words, f.vars...)
	words = append(words, f.blocks[f.entry:]...)
	words = append(words, inst(OpFunctionEnd)...)
	f.m.functions = append(f.m.functions, words)
}

// IDs returns ids as operands.
func IDs(ids ...ID) []uint32 {
	ops := make([]uint32, len(ids))
	for i, id := range ids {
		ops[i] = uint32(id)
	}
	return ops
}

// Assemble returns the module's words.
func (m *Module) Assemble() []uint32 {
	words := []uint32{Magic, Version, 0, uint32(m.bound), 0}
	for _, c := range m.capabilities {
		words = append(words, inst(OpCapability, uint32(c))...)
	}
	for _, e := range m.extensions {
		words = append(words, inst(OpExtension, String(e)...)...)
	}
	if m.glsl != 0 {
		words = append(words, inst(OpExtInstImport, append([]uint32{uint32(m.glsl)}, String("GLSL.std.450")...)...)...)
	}
	words = append(words, inst(OpMemoryModel, uint32(AddressingModelLogical), uint32(MemoryModelGLSL450))...)
	for _, section := range [][][]uint32{m.entryPoints, m.executionModes, m.names, m.annotations, m.globals, m.functions} {
		for _, w := range section {
			words = append(words, w...)
		}
	}
	return words
}

// Bytes returns the module's words as little-endian bytes, as Vulkan takes
// them.
func (m *Module) Bytes() []byte {
	words := m.Assemble()
	b := make([]byte, 4*len(words))
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}
