// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tospirv translates guests' TGSI shaders to SPIR-V for Vulkan, as
// Mesa's tgsi_to_nir and Zink's nir_to_spirv do, but directly, for the
// renderer's own virgl backend.
//
// TGSI's registers are typeless vectors of four 32-bit channels, whose bits
// each instruction takes as its operands' type: every register is a uvec4
// of a Private variable, which an instruction loads, swizzles, bitcasts to
// its type, and modifies, and to which it writes its result's bits, of the
// channels of its write mask. A prologue copies the shader's inputs and
// built-ins to registers, and an epilogue copies its output registers to
// its outputs and built-ins, after the shader's body, a function of its
// own, returns: as TGSI's RET returns from the body, every path passes the
// epilogue.
package tospirv

import (
	"fmt"
	"math"
	"sort"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// BaseType is a channel's type, as an instruction takes it.
type BaseType uint8

// The base types.
const (
	Float BaseType = iota
	Int
	Uint
)

// Options are the state that a shader's translation depends on, of the
// pipeline that it is in.
type Options struct {
	// InputTypes are a vertex shader's inputs' types, as the vertex
	// elements bound give them: Vulkan, and Metal, read integer formats as
	// integers.
	InputTypes [MaxVertexInputs]BaseType
	// VertexFetches are the conversions of a vertex shader's inputs of
	// formats that the device fetches not, which it fetches as integers.
	VertexFetches [MaxVertexInputs]VertexFetch
	// ColorTypes are a fragment shader's color outputs' types, as the
	// framebuffer's color buffers' formats give them: Vulkan writes
	// integer formats of integers.
	ColorTypes [MaxColorBuffers]BaseType
	// ColorBuffers is the number of color buffers that a fragment shader
	// that writes COLOR[0] to all, as FS_COLOR0_WRITES_ALL_CBUFS says,
	// writes.
	ColorBuffers int
	// DualSourceBlend is whether a fragment shader's COLOR[1] is the
	// second source of COLOR[0]'s blending, as the blend state's factors
	// say.
	DualSourceBlend bool
	// FlatShade is whether a fragment shader's inputs that interpolate as
	// colors, TGSI_INTERPOLATE_COLOR, are flat, as the rasterizer state
	// says.
	FlatShade bool
	// TwoSided is whether a fragment shader's color inputs are of the
	// previous stage's back colors, BCOLOR, for back faces, as the
	// rasterizer state's light_twoside says.
	TwoSided bool
	// AlphaTest is whether a fragment shader discards fragments whose
	// COLOR[0]'s alpha fails the depth-stencil-alpha state's alpha test:
	// its function, Gallium's PIPE_FUNC_*, of the reference.
	AlphaTest bool
	AlphaFunc uint32
	AlphaRef  float32
	// SpriteCoords are the TEXCOORDs, by their indexes' bits, that are a
	// fragment shader's point sprite's coordinates, rather than the
	// previous stage's, as the rasterizer state's sprite_coord_enable says
	// of points, of the lower left origin if SpriteCoordsLowerLeft.
	SpriteCoords          uint32
	SpriteCoordsLowerLeft bool
	// UserClipPlanes are the enabled user clip planes, which clip the last
	// stage before rasterization's CLIPVERTEX, or POSITION, unless it
	// writes clip distances; of one that does, ClipDisable are those that
	// the rasterizer state does not enable, by their indexes' bits.
	UserClipPlanes [][4]float32
	ClipDisable    uint8
	// GLClipZ is whether the last stage before rasterization's positions
	// are of OpenGL's clip space, whose z is of [-w, w], which it makes
	// Vulkan's, of [0, w], as the rasterizer state's clip_halfz says they
	// are not.
	GLClipZ bool
	// PointSize, if not 0, is the size of the points that the last stage
	// before rasterization writes, rather than its PSIZE, as the
	// rasterizer state's point size is GL's unless shaders' sizes are
	// enabled: Vulkan has no points of no size.
	PointSize float32
	// ExtraOutputs are varyings, of their semantics, that the next stage
	// reads and the shader may not write, which it declares: Vulkan
	// requires that a stage's inputs be the previous stage's outputs.
	ExtraOutputs []Varying
	// StreamOutput, if not nil, is a vertex shader's stream output, which
	// it captures itself.
	StreamOutput *StreamOutput
	// SharedMemory is a compute shader's shared memory, in bytes.
	SharedMemory int
	// WorkgroupFences is whether MEMBAR's fences of the device's scope
	// are of the workgroup's: Apple's GPU compiler crashes of a fence of
	// the device's scope before an atomic's read-modify-write.
	WorkgroupFences bool
	// FloatImageAtomics is whether the device exchanges texels of float
	// images atomically, as OES_shader_image_atomic does: without it, the
	// translation declares those images as unsigned integers', as their
	// Resources say.
	FloatImageAtomics bool
	// Multisampled is whether a fragment shader's color buffers, which
	// FBFETCH reads, are multisampled: it reads its sample's.
	Multisampled bool
}

// MaxVertexInputs is the number of a vertex shader's inputs.
const MaxVertexInputs = 32

// MaxColorBuffers is the number of a framebuffer's color buffers.
const MaxColorBuffers = 8

// The descriptor sets that resources bind in, and the bindings of a
// stage's resources in them: a set of each kind of resource, of bindings
// of each stage's slots, StageBindings apart.
const (
	SetUniformBuffers = 0
	SetSamplers       = 1
	StageBindings     = 32
)

// SetInputAttachments is the descriptor set of a fragment shader's input
// attachments, the color buffers that FBFETCH reads, of the bindings of
// their indexes.
const SetInputAttachments = 4

// Binding returns the binding of a stage's resource of slot.
func Binding(stage tgsi.Processor, slot int) uint32 {
	return uint32(int(stage)*StageBindings + slot)
}

// Varying is an input or output of a shader that the pipeline links to
// another shader's: its semantic, and the word of its Location decoration,
// which linking may change.
type Varying struct {
	Semantic      tgsi.Semantic
	SemanticIndex int
	// LocationWord is the index of the Location decoration's literal in
	// Result.Words.
	LocationWord int
	Flat         bool
}

// Resource is a resource that a shader uses: its kind and slot, or the
// first of an array's Count slots, and, of a sampler view, its target and
// channels' type.
type Resource struct {
	Kind   ResourceKind
	Slot   int
	Count  int
	Target tgsi.TextureTarget
	Type   BaseType
	// Size is a constant buffer's size in bytes, of the constants that the
	// shader declares.
	Size int
	// Format is a storage image's format, as Mesa names pipe_formats, or
	// PIPE_FORMAT_NONE of a write-only image of none.
	Format string
}

// ResourceKind is a kind of resource.
type ResourceKind uint8

// The kinds of resources.
const (
	UniformBuffer ResourceKind = iota
	SampledTexture
	StorageBuffer
	StorageImage
	// InputAttachment is a color buffer that a fragment shader reads, of
	// its slot's index, and its Type.
	InputAttachment
)

// Result is a translated shader.
type Result struct {
	Words     []uint32
	Inputs    []Varying
	Outputs   []Varying
	Resources []Resource
	// Kills is whether a fragment shader discards fragments.
	Kills bool
	// WritesDepth is whether a fragment shader writes the fragment's depth,
	// and WritesStencil its stencil reference.
	WritesDepth   bool
	WritesStencil bool
}

// Error is an error of a shader that the translator does not take.
type Error struct {
	Instruction int
	Msg         string
}

func (e *Error) Error() string {
	if e.Instruction < 0 {
		return "TGSI: " + e.Msg
	}
	return fmt.Sprintf("TGSI instruction %d: %s", e.Instruction, e.Msg)
}

// Translate translates s to SPIR-V.
func Translate(s *tgsi.Shader, opts Options) (res *Result, err error) {
	t := newTranslator(s, opts)
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			res, err = nil, e
		}
	}()
	t.translate()
	return t.finish(), nil
}

// fail aborts the translation.
func (t *translator) fail(format string, args ...any) {
	panic(&Error{Instruction: t.inst, Msg: fmt.Sprintf(format, args...)})
}

// register is a register of a file: its variable, a Private uvec4, or, of
// an array, a Private array of them, of which it is the element index.
type register struct {
	v     spirv.ID
	array *regArray
	index int
}

// regArray is an array of registers that a declaration's ArrayID makes,
// which instructions may index indirectly.
type regArray struct {
	v           spirv.ID
	first, last int
}

type translator struct {
	s    *tgsi.Shader
	opts Options
	m    *spirv.Module
	f    *spirv.Function
	inst int

	// Types.
	tVoid, tBool, tF32, tI32, tU32 spirv.ID
	tVec4F, tVec4I, tVec4U, tVec4B spirv.ID
	tVec2F, tVec3F                 spirv.ID
	tPtrPrivU4                     spirv.ID
	zero4U                         spirv.ID

	// registers are the registers declared, of each file but constants'.
	registers map[tgsi.File]map[int]bool
	temps     map[int]register
	addrs     map[int]spirv.ID
	inputs    map[int]*io
	outputs   map[int]*io
	imms      []spirv.ID
	consts    map[int]*constBuf
	sviews    map[int]*tgsi.Declaration
	samplers  map[int]*sampler
	sysvals   map[int]*io
	buffers   map[int]*memoryBuffer
	shared    *memoryBuffer
	images    map[int]*storageImage
	// attachments are the input attachments of color buffers, by their
	// indexes.
	attachments map[int]spirv.ID
	// imageDecls are the IMAGE declarations, by their slots, and
	// bufferDecls the slots that BUFFER declarations declare.
	imageDecls  map[int]*tgsi.Declaration
	bufferDecls map[int]bool
	// arrays are the arrays of resources, of each kind's.
	arrays map[ResourceKind][]*resArray
	// ubos are the uniform blocks made, by their slots, or their arrays'
	// first.
	ubos map[int]*constBuf

	interfaces []spirv.ID
	builtins   map[builtinKey]*iface
	// extraOutputs are the outputs of Options.ExtraOutputs that the
	// shader declares.
	extraOutputs []*io
	// invariant are the outputs decorated invariant.
	invariant map[spirv.ID]bool
	// bitcasts are the bitcasts emitted, of their values.
	bitcasts  map[spirv.ID]bitcastOf
	pushBlock spirv.ID
	locations []pendingLocation
	res       Result
	body      *spirv.Function
	// flow is the stack of the body's open IFs and loops.
	flow []construct
	// terminated is whether the current block ended, as after BRK, before
	// the next instruction begins one.
	terminated bool
	kills      bool
}

// constBuf is a constant buffer: its uniform block's variable, of an array
// of size uvec4s, or an array's blocks' if array is not nil.
type constBuf struct {
	v     spirv.ID
	size  int
	array *resArray
	tPtr  spirv.ID
}

// sampler is a sampled texture: its variable, of a combined image sampler,
// or of an array's, and its types.
type sampler struct {
	v       spirv.ID
	tImage  spirv.ID
	tSample spirv.ID
	target  tgsi.TextureTarget
	typ     BaseType
	array   *resArray
	// tPtr is the type of a pointer to a descriptor.
	tPtr spirv.ID
}

func newTranslator(s *tgsi.Shader, opts Options) *translator {
	t := &translator{
		s:         s,
		opts:      opts,
		m:         spirv.NewModule(),
		inst:      -1,
		registers: make(map[tgsi.File]map[int]bool),
		temps:     make(map[int]register),
		addrs:     make(map[int]spirv.ID),
		inputs:    make(map[int]*io),
		outputs:   make(map[int]*io),
		consts:    make(map[int]*constBuf),
		sviews:    make(map[int]*tgsi.Declaration),
		samplers:  make(map[int]*sampler),
		sysvals:   make(map[int]*io),
		buffers:   make(map[int]*memoryBuffer),
		builtins:  make(map[builtinKey]*iface),
		bitcasts:  make(map[spirv.ID]bitcastOf),
		invariant: make(map[spirv.ID]bool),
		images:    make(map[int]*storageImage),

		attachments: make(map[int]spirv.ID),
		imageDecls:  make(map[int]*tgsi.Declaration),
		bufferDecls: make(map[int]bool),
		arrays:      make(map[ResourceKind][]*resArray),
		ubos:        make(map[int]*constBuf),
	}
	m := t.m
	t.tVoid = m.TypeVoid()
	t.tBool = m.TypeBool()
	t.tF32 = m.TypeFloat(32)
	t.tI32 = m.TypeInt(32, true)
	t.tU32 = m.TypeInt(32, false)
	t.tVec4F = m.TypeVector(t.tF32, 4)
	t.tVec4I = m.TypeVector(t.tI32, 4)
	t.tVec4U = m.TypeVector(t.tU32, 4)
	t.tVec4B = m.TypeVector(t.tBool, 4)
	t.tVec2F = m.TypeVector(t.tF32, 2)
	t.tVec3F = m.TypeVector(t.tF32, 3)
	t.tPtrPrivU4 = m.TypePointer(spirv.StorageClassPrivate, t.tVec4U)
	t.zero4U = m.ConstantNull(t.tVec4U)
	return t
}

// vecType returns the vec4 type of base type b.
func (t *translator) vecType(b BaseType) spirv.ID {
	switch b {
	case Int:
		return t.tVec4I
	case Uint:
		return t.tVec4U
	}
	return t.tVec4F
}

// scalarType returns the scalar type of base type b.
func (t *translator) scalarType(b BaseType) spirv.ID {
	switch b {
	case Int:
		return t.tI32
	case Uint:
		return t.tU32
	}
	return t.tF32
}

// privateReg returns a new Private uvec4 register, of zero.
func (t *translator) privateReg() spirv.ID {
	return t.m.Variable(t.tPtrPrivU4, spirv.StorageClassPrivate, t.zero4U)
}

func (t *translator) executionModel() spirv.ExecutionModel {
	switch t.s.Processor {
	case tgsi.ProcessorVertex:
		return spirv.ExecutionModelVertex
	case tgsi.ProcessorTessCtrl:
		t.m.Capability(spirv.CapabilityTessellation)
		return spirv.ExecutionModelTessellationControl
	case tgsi.ProcessorTessEval:
		t.m.Capability(spirv.CapabilityTessellation)
		return spirv.ExecutionModelTessellationEvaluation
	case tgsi.ProcessorGeometry:
		t.m.Capability(spirv.CapabilityGeometry)
		return spirv.ExecutionModelGeometry
	case tgsi.ProcessorFragment:
		return spirv.ExecutionModelFragment
	case tgsi.ProcessorCompute:
		return spirv.ExecutionModelGLCompute
	}
	t.fail("shaders of processor %v", t.s.Processor)
	return 0
}

func (t *translator) translate() {
	model := t.executionModel()
	for i := range t.s.Declarations {
		t.declare(&t.s.Declarations[i])
	}
	for i := range t.s.Immediates {
		t.immediate(&t.s.Immediates[i])
	}
	t.declareInterface()
	t.findArrays()

	// The body.
	tFn := t.m.TypeFunction(t.tVoid)
	t.body = t.m.Function(t.tVoid, tFn)
	t.f = t.body
	t.f.Label()
	for i := range t.s.Instructions {
		t.inst = i
		if t.instruction(&t.s.Instructions[i]) {
			break
		}
	}
	t.inst = -1
	if len(t.flow) != 0 {
		t.fail("%d IFs or loops are not closed", len(t.flow))
	}
	if !t.terminated {
		t.f.Do(spirv.OpReturn)
	}
	t.f.End()

	// The entry point: the prologue, the body, and the epilogue.
	main := t.m.Function(t.tVoid, tFn)
	t.f = main
	t.f.Label()
	t.prologue()
	t.f.Op(spirv.OpFunctionCall, t.tVoid, uint32(t.body.ID()))
	if t.s.Processor != tgsi.ProcessorGeometry {
		// A geometry shader's EMITs copy its outputs.
		t.epilogue()
	}
	t.f.Do(spirv.OpReturn)
	t.f.End()

	t.m.EntryPoint(model, main.ID(), "main", t.interfaces...)
	t.stageModes(main.ID())
}

func (t *translator) finish() *Result {
	words := t.m.Assemble()
	t.resolveLocations(words)
	t.res.Words = words
	t.res.Kills = t.kills
	sort.Slice(t.res.Resources, func(i, j int) bool {
		a, b := t.res.Resources[i], t.res.Resources[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Slot < b.Slot
	})
	return &t.res
}

// property returns the value of the shader's property, and whether it has
// it.
func (t *translator) property(name tgsi.PropertyName) (uint32, bool) {
	for _, p := range t.s.Properties {
		if p.Name == name {
			return p.Value, true
		}
	}
	return 0, false
}

// declare makes the registers of a declaration. A register is declared
// once, so that the registers made are as many as the indexes at most.
func (t *translator) declare(d *tgsi.Declaration) {
	if d.File != tgsi.FileConstant {
		declared := t.registers[d.File]
		if declared == nil {
			declared = make(map[int]bool)
			t.registers[d.File] = declared
		}
		for i := d.First; i <= d.Last; i++ {
			if declared[i] {
				t.fail("%v[%d] is declared twice", d.File, i)
			}
			declared[i] = true
		}
	}
	switch d.File {
	case tgsi.FileTemporary:
		t.declareTemps(d)
	case tgsi.FileAddress:
		for i := d.First; i <= d.Last; i++ {
			t.addrs[i] = t.m.Variable(t.m.TypePointer(spirv.StorageClassPrivate, t.tVec4I),
				spirv.StorageClassPrivate, t.m.ConstantNull(t.tVec4I))
		}
	case tgsi.FileInput, tgsi.FileOutput:
		regs := t.inputs
		if d.File == tgsi.FileOutput {
			regs = t.outputs
		}
		for i := d.First; i <= d.Last; i++ {
			if regs[i] != nil {
				t.fail("%v[%d] is declared twice", d.File, i)
			}
			// Its variables are made once all are declared.
			regs[i] = &io{decl: d, idx: i - d.First}
		}
	case tgsi.FileSystemValue:
		for i := d.First; i <= d.Last; i++ {
			t.sysvals[i] = &io{decl: d, idx: i - d.First, reg: t.privateReg()}
		}
	case tgsi.FileConstant:
		t.declareConstants(d)
	case tgsi.FileSamplerView:
		for i := d.First; i <= d.Last; i++ {
			t.sviews[i] = d
		}
	case tgsi.FileImage:
		for i := d.First; i <= d.Last; i++ {
			t.imageDecls[i] = d
		}
	case tgsi.FileBuffer:
		for i := d.First; i <= d.Last; i++ {
			t.bufferDecls[i] = true
		}
	case tgsi.FileSampler, tgsi.FileMemory:
		// Made as instructions use them: samplers combined with the
		// sampler view of their index, once their views are declared.
	default:
		t.fail("declarations of %v", d.File)
	}
}

func (t *translator) declareTemps(d *tgsi.Declaration) {
	if d.ArrayID == 0 {
		for i := d.First; i <= d.Last; i++ {
			t.temps[i] = register{v: t.privateReg()}
		}
		return
	}
	n := d.Last - d.First + 1
	tArr := t.m.TypeArray(t.tVec4U, t.m.ConstantUint(uint32(n)))
	ptr := t.m.TypePointer(spirv.StorageClassPrivate, tArr)
	arr := &regArray{v: t.m.Variable(ptr, spirv.StorageClassPrivate, t.m.ConstantNull(tArr)), first: d.First, last: d.Last}
	for i := d.First; i <= d.Last; i++ {
		t.temps[i] = register{array: arr, index: i - d.First}
	}
}

// declareConstants declares a constant buffer: a uniform block of an array
// of uvec4s, of the constants declared. TGSI's constant buffers are of
// vec4s, as std140 lays out an array of them.
func (t *translator) declareConstants(d *tgsi.Declaration) {
	buf := 0
	if d.Dimension {
		buf = d.Index2D
	}
	if buf < 0 || buf >= StageBindings {
		t.fail("constant buffer %d", buf)
	}
	c := t.consts[buf]
	if c == nil {
		c = &constBuf{}
		t.consts[buf] = c
	}
	if d.Last+1 > c.size {
		c.size = d.Last + 1
	}
}

// constBlock returns the uniform block of constant buffer buf, or of its
// array, making it the first time, once all are declared.
func (t *translator) constBlock(buf int) *constBuf {
	d := t.consts[buf]
	if d == nil {
		t.fail("constant buffer %d is not declared", buf)
	}
	key, size := buf, d.size
	a := t.arrayOf(UniformBuffer, buf)
	if a != nil {
		// Of the largest of the array's blocks.
		key = a.first
		for i := a.first; i < a.first+a.n; i++ {
			size = max(size, t.consts[i].size)
		}
	}
	if c := t.ubos[key]; c != nil {
		return c
	}
	tArr := t.m.TypeArrayNew(t.tVec4U, t.m.ConstantUint(uint32(size)))
	t.m.Decorate(tArr, spirv.DecorationArrayStride, 16)
	tBlock := t.m.TypeStruct(tArr)
	t.m.Decorate(tBlock, spirv.DecorationBlock)
	t.m.MemberDecorate(tBlock, 0, spirv.DecorationOffset, 0)
	c := &constBuf{size: size, tPtr: t.m.TypePointer(spirv.StorageClassUniform, tBlock)}
	c.v, c.array = t.descriptorVar(UniformBuffer, buf, spirv.StorageClassUniform, tBlock, SetUniformBuffers)
	t.resource(Resource{Kind: UniformBuffer, Slot: buf, Size: 16 * size}, c.array)
	t.ubos[key] = c
	return c
}

func (t *translator) immediate(imm *tgsi.Immediate) {
	switch imm.Type {
	case tgsi.ImmFloat32, tgsi.ImmUint32, tgsi.ImmInt32:
	default:
		t.fail("immediates of %v", imm.Type)
	}
	var parts [4]spirv.ID
	for i := range parts {
		v := uint32(0)
		if i < len(imm.Values) {
			v = imm.Values[i]
		}
		parts[i] = t.m.ConstantUint(v)
	}
	t.imms = append(t.imms, t.m.ConstantComposite(t.tVec4U, parts[:]...))
}

// load emits a load of ptr, of type typ.
func (t *translator) load(typ, ptr spirv.ID) spirv.ID {
	return t.f.Op(spirv.OpLoad, typ, uint32(ptr))
}

// store emits a store of v to ptr.
func (t *translator) store(ptr, v spirv.ID) {
	t.f.Do(spirv.OpStore, uint32(ptr), uint32(v))
}

// bitcast converts v, a vec4 or a scalar, to typ, unless it is of it, or
// is a bitcast of a value of typ, which it returns.
func (t *translator) bitcast(typ, from, v spirv.ID) spirv.ID {
	if typ == from {
		return v
	}
	if b, ok := t.bitcasts[v]; ok && b.typ == typ {
		return b.v
	}
	r := t.f.Op(spirv.OpBitcast, typ, uint32(v))
	t.bitcasts[r] = bitcastOf{v: v, typ: from}
	return r
}

// bitcastOf is the value of a bitcast, and its type.
type bitcastOf struct {
	v, typ spirv.ID
}

// float32Const returns a float constant.
func (t *translator) float32Const(f float32) spirv.ID {
	return t.m.ConstantFloat(f)
}

// splat returns a vec4 of typ of scalar s's four copies.
func (t *translator) splat(vecType, s spirv.ID) spirv.ID {
	return t.f.Op(spirv.OpCompositeConstruct, vecType, uint32(s), uint32(s), uint32(s), uint32(s))
}

// constVec4F returns a constant vec4 of floats.
func (t *translator) constVec4F(x, y, z, w float32) spirv.ID {
	return t.m.ConstantComposite(t.tVec4F, t.float32Const(x), t.float32Const(y), t.float32Const(z), t.float32Const(w))
}

func float32bits(f float32) uint32 { return math.Float32bits(f) }

// VertexFetch is the conversion of a vertex shader's input of a format that
// the device fetches not, which it fetches as integers of the vertex
// buffer's bits: the prologue converts them to the format's floats, of
// Channels channels, and the others to 0, but w to 1.
type VertexFetch struct {
	Kind     FetchKind
	Signed   bool
	Channels uint8
	// Normalized is whether a packed format's channels are normalized,
	// and BGR whether its first is blue.
	Normalized, BGR bool
}

// FetchKind is a kind of a vertex input's conversion.
type FetchKind uint8

// The kinds of conversions.
const (
	// FetchNative is a fetch of the input's format itself.
	FetchNative FetchKind = iota
	// FetchScaled converts integers to floats of their values, as
	// Gallium's SCALED formats have them.
	FetchScaled
	// FetchNormalized32 converts integers of 32 bits to floats of [0, 1],
	// or of [-1, 1] if Signed, as Gallium's 32-bit UNORM and SNORM formats
	// have them.
	FetchNormalized32
	// FetchPacked1010102 unpacks an integer of 32 bits to channels of 10,
	// 10, 10 and 2 bits, which it scales or normalizes.
	FetchPacked1010102
)
