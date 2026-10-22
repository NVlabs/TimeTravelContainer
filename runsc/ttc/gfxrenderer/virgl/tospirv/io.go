// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"sort"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// The push constants: values that the renderer gives each draw's shaders
// in their push constant block, at these offsets.
const (
	// PushFramebufferHeight is the framebuffer's height, a float, by which
	// a fragment shader whose coordinates' origin is TGSI's lower left
	// flips Vulkan's.
	PushFramebufferHeight = 0
	// PushBaseInstance is the draw's first instance, an int, from which
	// Vulkan's instance indexes count, and GL's instance IDs do not.
	PushBaseInstance = 4
	// PushConstantsSize is the size of the block.
	PushConstantsSize = 16
)

// MaxPatchVertices is the size of tessellation shaders' arrays of their
// patches' vertices' inputs: GL's least gl_MaxPatchVertices, and Vulkan's
// least maxTessellationPatchSize.
const MaxPatchVertices = 32

// pushConstant returns the uint of the push constant block at offset.
func (t *translator) pushConstant(offset uint32) spirv.ID {
	if t.pushBlock == 0 {
		tArr := t.m.TypeArrayNew(t.tU32, t.m.ConstantUint(PushConstantsSize/4))
		t.m.Decorate(tArr, spirv.DecorationArrayStride, 4)
		tBlock := t.m.TypeStruct(tArr)
		t.m.Decorate(tBlock, spirv.DecorationBlock)
		t.m.MemberDecorate(tBlock, 0, spirv.DecorationOffset, 0)
		t.pushBlock = t.m.Variable(t.m.TypePointer(spirv.StorageClassPushConstant, tBlock), spirv.StorageClassPushConstant)
	}
	ptr := t.f.Op(spirv.OpAccessChain, t.m.TypePointer(spirv.StorageClassPushConstant, t.tU32),
		uint32(t.pushBlock), uint32(t.m.ConstantUint(0)), uint32(t.m.ConstantUint(offset/4)))
	return t.load(t.tU32, ptr)
}

// iface is a variable of the shader's interface, a varying's or a
// built-in's, which registers' channels are of: a vec4's channels, an
// array's elements, or a scalar, of type scalar; of each vertex of a
// primitive or patch, as an array of their values, if perVertex.
type iface struct {
	v      spirv.ID
	sc     spirv.StorageClass
	scalar spirv.ID
	// vec is the type of a vec4's, whose channels are a register's.
	vec       spirv.ID
	perVertex bool
	// vertices is the number of a perVertex iface's vertices.
	vertices int
}

// channel is where a register's channel is of the interface: an element of
// f, a vec4's channel or an array's element, or f itself if elem is -1.
type channel struct {
	f    *iface
	elem int
}

// io is an input, output or system value register.
type io struct {
	decl *tgsi.Declaration
	// idx is the register's offset in its declaration's range, which its
	// semantic index adds.
	idx int
	// reg is the register's Private uvec4, which a prologue or an epilogue
	// copies to or from its channels, or 0 if instructions access its
	// channels in the interface: a per-vertex input's, or a tessellation
	// control shader's output's.
	reg spirv.ID
	// chans are where the register's channels are of the interface, of
	// those that it has; none of a register that the prologue computes.
	chans [4]channel
	// colors are a fragment shader's color outputs that the register
	// writes, of the types of their color buffers.
	colors []*iface
	// input is a fragment shader's varying's Input, which interpolation
	// instructions interpolate, and back a color's of back faces.
	input *iface
	back  *iface
}

// semantic returns the register's semantic and index, or false if it has
// none.
func (o *io) semantic() (tgsi.Semantic, int, bool) {
	if !o.decl.HasSemantic {
		return 0, 0, false
	}
	return o.decl.Semantic, o.decl.SemanticIndex + o.idx, true
}

// vecChannels maps a register's channels to f's vec4's.
func vecChannels(f *iface) [4]channel {
	return [4]channel{{f, 0}, {f, 1}, {f, 2}, {f, 3}}
}

// wholeVec returns the iface whose vec4 a register's channels are, in
// order, or nil.
func (o *io) wholeVec() *iface {
	f := o.chans[0].f
	if f == nil || f.vec == 0 {
		return nil
	}
	for c := range o.chans {
		if o.chans[c].f != f || o.chans[c].elem != c {
			return nil
		}
	}
	return f
}

// newIface returns a new variable of the interface, of type typ, of a
// primitive's vertices' if vertices is not 0.
func (t *translator) newIface(sc spirv.StorageClass, typ, scalar, vec spirv.ID, vertices int) *iface {
	f := &iface{sc: sc, scalar: scalar, vec: vec, perVertex: vertices > 0, vertices: vertices}
	if f.perVertex {
		typ = t.m.TypeArray(typ, t.m.ConstantUint(uint32(vertices)))
	}
	f.v = t.m.Variable(t.m.TypePointer(sc, typ), sc)
	t.interfaces = append(t.interfaces, f.v)
	return f
}

type builtinKey struct {
	sc spirv.StorageClass
	b  spirv.BuiltIn
}

// builtin returns the interface's built-in b, of type typ, of a primitive's
// vertices' if vertices is not 0, making it the first time.
func (t *translator) builtin(sc spirv.StorageClass, b spirv.BuiltIn, typ, scalar, vec spirv.ID, vertices int) *iface {
	k := builtinKey{sc, b}
	if f := t.builtins[k]; f != nil {
		return f
	}
	f := t.newIface(sc, typ, scalar, vec, vertices)
	t.m.Decorate(f.v, spirv.DecorationBuiltIn, uint32(b))
	if t.s.Processor == tgsi.ProcessorFragment && sc == spirv.StorageClassInput && (scalar == t.tI32 || scalar == t.tU32) {
		// A fragment shader's integer inputs are flat.
		t.m.Decorate(f.v, spirv.DecorationFlat)
	}
	t.builtins[k] = f
	return f
}

// builtinValue returns the value of the input built-in b, of type typ, a
// scalar's or a vector's of scalar.
func (t *translator) builtinValue(b spirv.BuiltIn, typ, scalar spirv.ID) spirv.ID {
	return t.load(typ, t.builtin(spirv.StorageClassInput, b, typ, scalar, 0, 0).v)
}

// location decorates v at a location of its own, of the shader's inputs or
// outputs, which linking may change, and records it as the varying of
// semantic sem and index semIndex.
func (t *translator) location(v spirv.ID, varyings *[]Varying, sem tgsi.Semantic, semIndex int, flat bool) {
	loc := uint32(len(*varyings))
	t.m.Decorate(v, spirv.DecorationLocation, loc)
	*varyings = append(*varyings, Varying{Semantic: sem, SemanticIndex: semIndex, LocationWord: -1, Flat: flat})
	t.locations = append(t.locations, pendingLocation{v: v, varyings: varyings, i: len(*varyings) - 1})
}

// varying returns a new varying of the interface, of a register's
// semantic, a vec4 of floats, of a primitive's vertices' if vertices is not
// 0, at a location of its own.
func (t *translator) varying(sc spirv.StorageClass, o *io, vertices int, flat bool) *iface {
	sem, index, ok := o.semantic()
	if !ok {
		t.fail("a varying of no semantic")
	}
	return t.varyingOf(sc, sem, index, vertices, flat)
}

// varyingOf returns a new varying of semantic sem and index.
func (t *translator) varyingOf(sc spirv.StorageClass, sem tgsi.Semantic, index, vertices int, flat bool) *iface {
	f := t.newIface(sc, t.tVec4F, t.tF32, t.tVec4F, vertices)
	varyings := &t.res.Inputs
	if sc == spirv.StorageClassOutput {
		varyings = &t.res.Outputs
	}
	t.location(f.v, varyings, sem, index, flat)
	if sem == tgsi.SemanticPatch {
		t.m.Decorate(f.v, spirv.DecorationPatch)
	}
	return f
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// isPatch reports whether a tessellation shader's input or output of
// semantic sem is of the patch rather than of each vertex.
func isPatch(sem tgsi.Semantic) bool {
	switch sem {
	case tgsi.SemanticPatch, tgsi.SemanticTessOuter, tgsi.SemanticTessInner, tgsi.SemanticPrimID:
		return true
	}
	return false
}

// inputVertices returns the number of vertices of a geometry shader's
// input primitive, or of a tessellation shader's input patch, or 0.
func (t *translator) inputVertices() int {
	switch t.s.Processor {
	case tgsi.ProcessorGeometry:
		prim, _ := t.property(tgsi.PropertyGSInputPrimitive)
		switch prim {
		case primPoints:
			return 1
		case primLines, primLineStrip, primLineLoop:
			return 2
		case primLinesAdjacency, primLineStripAdjacency:
			return 4
		case primTrianglesAdjacency, primTriangleStripAdjacency:
			return 6
		}
		return 3
	case tgsi.ProcessorTessCtrl, tgsi.ProcessorTessEval:
		return MaxPatchVertices
	}
	return 0
}

// outputVertices returns the number of a tessellation control shader's
// output patch's vertices.
func (t *translator) outputVertices() int {
	n, _ := t.property(tgsi.PropertyTCSVerticesOut)
	if n < 1 || n > MaxPatchVertices {
		t.fail("a patch of %d vertices", n)
	}
	return int(n)
}

// clipDistances returns how many of the CLIPDIST registers' channels are
// clip distances, and how many cull distances, which follow.
func (t *translator) clipDistances() (clip, cull int) {
	nclip, hasClip := t.property(tgsi.PropertyNumClipDistEnabled)
	ncull, hasCull := t.property(tgsi.PropertyNumCullDistEnabled)
	if !hasClip && !hasCull {
		// Every channel declared is a clip distance.
		for _, d := range t.s.Declarations {
			if d.HasSemantic && d.Semantic == tgsi.SemanticClipDist && d.File != tgsi.FileSystemValue {
				for i := d.First; i <= d.Last; i++ {
					if n := 4 * (d.SemanticIndex + i - d.First + 1); n > clip {
						clip = n
					}
				}
			}
		}
		return clip, 0
	}
	if nclip+ncull > 8 {
		t.fail("%d clip and %d cull distances", nclip, ncull)
	}
	return int(nclip), int(ncull)
}

// clipChannels maps CLIPDIST[index]'s channels to the ClipDistance and
// CullDistance built-ins of storage class sc, of vertices' if vertices is
// not 0.
func (t *translator) clipChannels(sc spirv.StorageClass, index, vertices int) [4]channel {
	nclip, ncull := t.clipDistances()
	var chans [4]channel
	for c := 0; c < 4; c++ {
		d := 4*index + c
		var b spirv.BuiltIn
		var n int
		switch {
		case d < nclip:
			b, n = spirv.BuiltInClipDistance, nclip
			t.m.Capability(spirv.CapabilityClipDistance)
		case d < nclip+ncull:
			b, n, d = spirv.BuiltInCullDistance, ncull, d-nclip
			t.m.Capability(spirv.CapabilityCullDistance)
		default:
			continue
		}
		typ := t.m.TypeArray(t.tF32, t.m.ConstantUint(uint32(n)))
		chans[c] = channel{t.builtin(sc, b, typ, t.tF32, 0, vertices), d}
	}
	return chans
}

// scalarChannel maps a register's channel c to a scalar built-in.
func (t *translator) scalarChannel(sc spirv.StorageClass, b spirv.BuiltIn, scalar spirv.ID, c, vertices int) [4]channel {
	var chans [4]channel
	chans[c] = channel{t.builtin(sc, b, scalar, scalar, 0, vertices), -1}
	return chans
}

// declareInterface makes the variables of the shader's inputs and outputs,
// once all are declared.
func (t *translator) declareInterface() {
	for _, i := range sortedKeys(t.inputs) {
		t.declareInput(t.inputs[i], i)
	}
	for _, i := range sortedKeys(t.outputs) {
		t.declareOutput(t.outputs[i])
	}
	t.declareExtraOutputs()
}

// declareExtraOutputs declares the outputs of Options.ExtraOutputs that
// the shader does not write, of zeros but of a tessellation control
// shader's, whose values are undefined, as GL's of outputs that the
// previous stage does not write are.
func (t *translator) declareExtraOutputs() {
	if len(t.opts.ExtraOutputs) == 0 || t.s.Processor == tgsi.ProcessorFragment || t.s.Processor == tgsi.ProcessorCompute {
		return
	}
	written := make(map[Varying]bool)
	for _, v := range t.res.Outputs {
		written[Varying{Semantic: v.Semantic, SemanticIndex: v.SemanticIndex}] = true
	}
	for _, v := range t.opts.ExtraOutputs {
		k := Varying{Semantic: v.Semantic, SemanticIndex: v.SemanticIndex}
		if written[k] {
			continue
		}
		written[k] = true
		vertices := 0
		if t.s.Processor == tgsi.ProcessorTessCtrl && !isPatch(v.Semantic) {
			vertices = t.outputVertices()
		}
		f := t.varyingOf(spirv.StorageClassOutput, v.Semantic, v.SemanticIndex, vertices, false)
		if t.s.Processor != tgsi.ProcessorTessCtrl {
			// An output register of its own, of zeros, which no
			// instruction writes.
			d := &tgsi.Declaration{File: tgsi.FileOutput, HasSemantic: true, Semantic: v.Semantic,
				SemanticIndex: v.SemanticIndex}
			t.extraOutputs = append(t.extraOutputs, &io{decl: d, reg: t.privateReg(), chans: vecChannels(f)})
		}
	}
}

func (t *translator) declareInput(o *io, i int) {
	d := o.decl
	switch t.s.Processor {
	case tgsi.ProcessorVertex:
		if i < 0 || i >= MaxVertexInputs {
			t.fail("vertex input %d", i)
		}
		b := t.opts.InputTypes[i]
		f := t.newIface(spirv.StorageClassInput, t.vecType(b), t.scalarType(b), t.vecType(b), 0)
		t.m.Decorate(f.v, spirv.DecorationLocation, uint32(i))
		o.reg, o.chans = t.privateReg(), vecChannels(f)
	case tgsi.ProcessorFragment:
		o.reg = t.privateReg()
		sem, index, _ := o.semantic()
		switch {
		case !d.HasSemantic:
			t.fail("a fragment shader's input of no semantic")
		case sem == tgsi.SemanticPosition, sem == tgsi.SemanticFace, sem == tgsi.SemanticPCoord:
			// Computed by the prologue.
		case sem == tgsi.SemanticTexCoord && index < 32 && t.opts.SpriteCoords&(1<<index) != 0:
			// A point sprite's coordinates, which the prologue computes.
		case sem == tgsi.SemanticPrimID:
			t.m.Capability(spirv.CapabilityGeometry)
			o.chans = t.scalarChannel(spirv.StorageClassInput, spirv.BuiltInPrimitiveId, t.tI32, 0, 0)
		case sem == tgsi.SemanticLayer:
			t.m.Capability(spirv.CapabilityGeometry)
			o.chans = t.scalarChannel(spirv.StorageClassInput, spirv.BuiltInLayer, t.tI32, 0, 0)
		case sem == tgsi.SemanticViewportIndex:
			t.m.Capability(spirv.CapabilityMultiViewport)
			o.chans = t.scalarChannel(spirv.StorageClassInput, spirv.BuiltInViewportIndex, t.tI32, 0, 0)
		case sem == tgsi.SemanticClipDist:
			o.chans = t.clipChannels(spirv.StorageClassInput, index, 0)
		default:
			f := t.fragmentVarying(d, sem, index)
			o.chans, o.input = vecChannels(f), f
			if sem == tgsi.SemanticColor && t.opts.TwoSided {
				// The back face's color, which the prologue takes of back
				// faces.
				o.back = t.fragmentVarying(d, tgsi.SemanticBColor, index)
			}
		}
	case tgsi.ProcessorGeometry, tgsi.ProcessorTessCtrl, tgsi.ProcessorTessEval:
		sem, index, ok := o.semantic()
		if !ok {
			t.fail("an input of %v of no semantic", t.s.Processor)
		}
		vertices := t.inputVertices()
		if t.s.Processor != tgsi.ProcessorGeometry && isPatch(sem) {
			// A tessellation evaluation shader's patch's: copied.
			o.reg, vertices = t.privateReg(), 0
		}
		sc := spirv.StorageClassInput
		switch sem {
		case tgsi.SemanticPosition:
			o.chans = vecChannels(t.builtin(sc, spirv.BuiltInPosition, t.tVec4F, t.tF32, t.tVec4F, vertices))
		case tgsi.SemanticPSize:
			t.pointSizeCapability()
			o.chans = t.scalarChannel(sc, spirv.BuiltInPointSize, t.tF32, 0, vertices)
		case tgsi.SemanticClipDist:
			o.chans = t.clipChannels(sc, index, vertices)
		case tgsi.SemanticTessOuter, tgsi.SemanticTessInner:
			o.chans = t.tessLevelChannels(sc, sem)
		case tgsi.SemanticPrimID:
			t.fail("PRIMID as an input")
		default:
			o.chans = vecChannels(t.varying(sc, o, vertices, false))
		}
	default:
		t.fail("inputs of %v", t.s.Processor)
	}
}

// fragmentVarying returns a fragment shader's new input varying of
// semantic sem and index, interpolated as d says.
func (t *translator) fragmentVarying(d *tgsi.Declaration, sem tgsi.Semantic, index int) *iface {
	flat := false
	if d.Interpolate {
		switch d.Interp {
		case tgsi.InterpolateConstant:
			flat = true
		case tgsi.InterpolateColor:
			flat = t.opts.FlatShade
		}
	}
	f := t.varyingOf(spirv.StorageClassInput, sem, index, 0, flat)
	if flat {
		t.m.Decorate(f.v, spirv.DecorationFlat)
	} else if d.Interpolate && d.Interp == tgsi.InterpolateLinear {
		t.m.Decorate(f.v, spirv.DecorationNoPerspective)
	}
	switch d.Location {
	case tgsi.LocationCentroid:
		t.m.Decorate(f.v, spirv.DecorationCentroid)
	case tgsi.LocationSample:
		t.m.Capability(spirv.CapabilitySampleRateShading)
		t.m.Decorate(f.v, spirv.DecorationSample)
	}
	return f
}

// tessLevelChannels maps a TESSOUTER or TESSINNER register's channels to
// the patch's tessellation levels.
func (t *translator) tessLevelChannels(sc spirv.StorageClass, sem tgsi.Semantic) [4]channel {
	b, n := spirv.BuiltInTessLevelOuter, 4
	if sem == tgsi.SemanticTessInner {
		b, n = spirv.BuiltInTessLevelInner, 2
	}
	f := t.builtins[builtinKey{sc, b}]
	if f == nil {
		typ := t.m.TypeArray(t.tF32, t.m.ConstantUint(uint32(n)))
		f = t.builtin(sc, b, typ, t.tF32, 0, 0)
		t.m.Decorate(f.v, spirv.DecorationPatch)
	}
	var chans [4]channel
	for c := 0; c < n; c++ {
		chans[c] = channel{f, c}
	}
	return chans
}

// pointSizeCapability declares the capability of a point size of a
// tessellation or geometry shader.
func (t *translator) pointSizeCapability() {
	switch t.s.Processor {
	case tgsi.ProcessorTessCtrl, tgsi.ProcessorTessEval:
		t.m.Capability(spirv.CapabilityTessellationPointSize)
	case tgsi.ProcessorGeometry:
		t.m.Capability(spirv.CapabilityGeometryPointSize)
	}
}

func (t *translator) declareOutput(o *io) {
	d := o.decl
	sc := spirv.StorageClassOutput
	if t.s.Processor == tgsi.ProcessorFragment {
		t.declareFragmentOutput(o)
		return
	}
	if d.Invariant {
		defer func() {
			for _, ch := range o.chans {
				if ch.f != nil && !t.invariant[ch.f.v] {
					t.invariant[ch.f.v] = true
					t.m.Decorate(ch.f.v, spirv.DecorationInvariant)
				}
			}
		}()
	}
	sem, index, ok := o.semantic()
	if !ok {
		t.fail("an output of %v of no semantic", t.s.Processor)
	}
	vertices := 0
	if t.s.Processor == tgsi.ProcessorTessCtrl {
		// The control shader's invocations share their outputs, which
		// they access in the interface.
		if !isPatch(sem) {
			vertices = t.outputVertices()
		}
	} else {
		o.reg = t.privateReg()
	}
	switch sem {
	case tgsi.SemanticPosition:
		o.chans = vecChannels(t.builtin(sc, spirv.BuiltInPosition, t.tVec4F, t.tF32, t.tVec4F, vertices))
	case tgsi.SemanticPSize:
		t.pointSizeCapability()
		o.chans = t.scalarChannel(sc, spirv.BuiltInPointSize, t.tF32, 0, vertices)
	case tgsi.SemanticClipDist:
		o.chans = t.clipChannels(sc, index, vertices)
	case tgsi.SemanticLayer, tgsi.SemanticViewportIndex:
		b, cap := spirv.BuiltInLayer, spirv.CapabilityGeometry
		if sem == tgsi.SemanticViewportIndex {
			b, cap = spirv.BuiltInViewportIndex, spirv.CapabilityMultiViewport
		}
		if t.s.Processor != tgsi.ProcessorGeometry {
			t.m.Extension("SPV_EXT_shader_viewport_index_layer")
			cap = spirv.CapabilityShaderViewportIndexLayerEXT
		}
		t.m.Capability(cap)
		o.chans = t.scalarChannel(sc, b, t.tI32, 0, vertices)
	case tgsi.SemanticTessOuter, tgsi.SemanticTessInner:
		o.chans = t.tessLevelChannels(sc, sem)
	case tgsi.SemanticEdgeFlag:
		// Of fixed function's features that Vulkan has none of, which
		// guests' drivers lower.
	case tgsi.SemanticClipVertex:
		// Which user clip planes clip, of the epilogue's.
	default:
		f := t.varying(sc, o, vertices, false)
		if t.s.Processor == tgsi.ProcessorGeometry {
			if stream := d.Streams[0]; stream != 0 {
				t.m.Capability(spirv.CapabilityGeometryStreams)
				t.m.Decorate(f.v, spirv.DecorationStream, uint32(stream))
			}
		}
		o.chans = vecChannels(f)
	}
}

func (t *translator) declareFragmentOutput(o *io) {
	o.reg = t.privateReg()
	sem, index, ok := o.semantic()
	if !ok {
		t.fail("a fragment output of no semantic")
	}
	sc := spirv.StorageClassOutput
	switch sem {
	case tgsi.SemanticColor:
		first, n := index, 1
		if v, _ := t.property(tgsi.PropertyFSColor0WritesAllCbufs); v != 0 && index == 0 && t.opts.ColorBuffers > 1 {
			n = t.opts.ColorBuffers
		}
		for k := first; k < first+n; k++ {
			if k < 0 || k >= MaxColorBuffers {
				t.fail("COLOR[%d]", k)
			}
			loc, blendIndex := k, 0
			if t.opts.DualSourceBlend && k == 1 {
				loc, blendIndex = 0, 1
			}
			b := t.opts.ColorTypes[loc]
			f := t.newIface(sc, t.vecType(b), t.scalarType(b), t.vecType(b), 0)
			t.m.Decorate(f.v, spirv.DecorationLocation, uint32(loc))
			if blendIndex != 0 {
				t.m.Decorate(f.v, spirv.DecorationIndex, uint32(blendIndex))
			}
			o.colors = append(o.colors, f)
		}
	case tgsi.SemanticPosition:
		t.res.WritesDepth = true
		o.chans = t.scalarChannel(sc, spirv.BuiltInFragDepth, t.tF32, tgsi.Z, 0)
	case tgsi.SemanticStencil:
		t.m.Extension("SPV_EXT_shader_stencil_export")
		t.m.Capability(spirv.CapabilityStencilExportEXT)
		t.res.WritesStencil = true
		o.chans = t.scalarChannel(sc, spirv.BuiltInFragStencilRefEXT, t.tI32, tgsi.Y, 0)
	case tgsi.SemanticSampleMask:
		typ := t.m.TypeArray(t.tI32, t.m.ConstantUint(1))
		o.chans[0] = channel{t.builtin(sc, spirv.BuiltInSampleMask, typ, t.tI32, 0, 0), 0}
	default:
		t.fail("fragment output %v", sem)
	}
}

// chanPtr returns a pointer to the scalar of a register's channel, of
// vertex, of a per-vertex iface's.
func (t *translator) chanPtr(ch channel, vertex spirv.ID) spirv.ID {
	var idx []uint32
	if ch.f.perVertex {
		idx = append(idx, uint32(vertex))
	}
	if ch.elem >= 0 {
		idx = append(idx, uint32(t.m.ConstantUint(uint32(ch.elem))))
	}
	if len(idx) == 0 {
		return ch.f.v
	}
	ptr := t.m.TypePointer(ch.f.sc, ch.f.scalar)
	return t.f.Op(spirv.OpAccessChain, ptr, append([]uint32{uint32(ch.f.v)}, idx...)...)
}

// vecPtr returns a pointer to f's vec4, of vertex, of a per-vertex f's.
func (t *translator) vecPtr(f *iface, vertex spirv.ID) spirv.ID {
	if !f.perVertex {
		return f.v
	}
	return t.f.Op(spirv.OpAccessChain, t.m.TypePointer(f.sc, f.vec), uint32(f.v), uint32(vertex))
}

// loadChannels returns a register's channels of the interface, of vertex,
// as a uvec4 of their bits, of 0 of the channels that it has none of.
func (t *translator) loadChannels(o *io, vertex spirv.ID) spirv.ID {
	if f := o.wholeVec(); f != nil {
		return t.bitcast(t.tVec4U, f.vec, t.load(f.vec, t.vecPtr(f, vertex)))
	}
	var parts [4]uint32
	for c, ch := range o.chans {
		if ch.f == nil {
			parts[c] = uint32(t.m.ConstantUint(0))
			continue
		}
		v := t.load(ch.f.scalar, t.chanPtr(ch, vertex))
		parts[c] = uint32(t.bitcast(t.tU32, ch.f.scalar, v))
	}
	return t.f.Op(spirv.OpCompositeConstruct, t.tVec4U, parts[:]...)
}

// storeChannels stores the channels of mask of bits, a uvec4, to a
// register's channels of the interface, of vertex.
func (t *translator) storeChannels(o *io, vertex, bits spirv.ID, mask uint8) {
	if f := o.wholeVec(); f != nil {
		ptr := t.vecPtr(f, vertex)
		t.storeMasked(ptr, f.vec, t.bitcast(f.vec, t.tVec4U, bits), mask)
		return
	}
	for c, ch := range o.chans {
		if ch.f == nil || mask&(1<<c) == 0 {
			continue
		}
		v := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(bits), uint32(c))
		t.store(t.chanPtr(ch, vertex), t.bitcast(ch.f.scalar, t.tU32, v))
	}
}

// vertexIndex returns the vertex that a per-vertex register's second
// dimension indexes, clamped to the vertices.
func (t *translator) vertexIndex(r *tgsi.Register, o *io) spirv.ID {
	n := t.ioVertices(o)
	if !r.Dimension {
		t.fail("%v[%d] of no vertex", r.File, r.Index)
	}
	if r.DimIndirect == nil {
		if r.DimIndex < 0 || r.DimIndex >= n {
			t.fail("%v[%d][%d] past the %d vertices", r.File, r.DimIndex, r.Index, n)
		}
		return t.m.ConstantInt(int32(r.DimIndex))
	}
	i := t.f.Op(spirv.OpIAdd, t.tI32, uint32(t.indirectIndex(r.DimIndirect)), uint32(t.m.ConstantInt(int32(r.DimIndex))))
	return t.glsl(t.tI32, spirv.GLSLstd450SClamp, i, t.m.ConstantInt(0), t.m.ConstantInt(int32(n-1)))
}

// ioVertices returns the number of vertices of a register's per-vertex
// channels.
func (t *translator) ioVertices(o *io) int {
	for _, ch := range o.chans {
		if ch.f != nil && ch.f.perVertex {
			return ch.f.vertices
		}
	}
	t.fail("a register of no vertices")
	return 0
}

// prologue copies the shader's inputs and system values to their
// registers.
func (t *translator) prologue() {
	for _, i := range sortedKeys(t.inputs) {
		o := t.inputs[i]
		if o.reg == 0 {
			continue
		}
		if t.s.Processor == tgsi.ProcessorFragment {
			if v := t.fragmentInput(o); v != 0 {
				t.store(o.reg, v)
				continue
			}
		}
		t.store(o.reg, t.loadChannels(o, 0))
	}
	for _, i := range sortedKeys(t.sysvals) {
		sv := t.sysvals[i]
		t.store(sv.reg, t.systemValue(sv))
	}
}

// fragmentInput returns the bits of a fragment shader's input that the
// prologue computes, or 0 if the input is of the interface.
func (t *translator) fragmentInput(o *io) spirv.ID {
	sem, _, _ := o.semantic()
	switch sem {
	case tgsi.SemanticPosition:
		return t.fragCoord()
	case tgsi.SemanticFace:
		front := t.builtinValue(spirv.BuiltInFrontFacing, t.tBool, t.tBool)
		f := t.f.Op(spirv.OpSelect, t.tF32, uint32(front), uint32(t.float32Const(1)), uint32(t.float32Const(-1)))
		one, zero := t.float32Const(1), t.float32Const(0)
		vec := t.f.Op(spirv.OpCompositeConstruct, t.tVec4F, uint32(f), uint32(zero), uint32(zero), uint32(one))
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	case tgsi.SemanticPCoord:
		pc := t.builtinValue(spirv.BuiltInPointCoord, t.tVec2F, t.tF32)
		vec := t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(pc), uint32(t.constVec4F(0, 0, 0, 1)), 0, 1, 4, 5)
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	case tgsi.SemanticTexCoord:
		if _, index, _ := o.semantic(); index >= 32 || t.opts.SpriteCoords&(1<<index) == 0 {
			break
		}
		pc := t.builtinValue(spirv.BuiltInPointCoord, t.tVec2F, t.tF32)
		if t.opts.SpriteCoordsLowerLeft {
			// Of Vulkan's upper left origin, flipped.
			y := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(pc), 1)
			y = t.f.Op(spirv.OpFSub, t.tF32, uint32(t.float32Const(1)), uint32(y))
			pc = t.f.Op(spirv.OpCompositeInsert, t.tVec2F, uint32(y), uint32(pc), 1)
		}
		vec := t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(pc), uint32(t.constVec4F(0, 0, 0, 1)), 0, 1, 4, 5)
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	case tgsi.SemanticColor:
		if o.back == nil {
			break
		}
		front := t.builtinValue(spirv.BuiltInFrontFacing, t.tBool, t.tBool)
		fv := t.load(t.tVec4F, o.input.v)
		bv := t.load(t.tVec4F, o.back.v)
		vec := t.f.Op(spirv.OpSelect, t.tVec4F, uint32(t.splatBool(front)), uint32(fv), uint32(bv))
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	}
	return 0
}

// splatBool returns a bvec4 of b.
func (t *translator) splatBool(b spirv.ID) spirv.ID {
	return t.f.Op(spirv.OpCompositeConstruct, t.tVec4B, uint32(b), uint32(b), uint32(b), uint32(b))
}

// fragCoord returns the fragment's coordinates, as TGSI's properties have
// them: of the lower left origin, flipped by the framebuffer's height, and
// of integer pixel centers.
func (t *translator) fragCoord() spirv.ID {
	fc := t.builtinValue(spirv.BuiltInFragCoord, t.tVec4F, t.tF32)
	x := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(fc), 0)
	y := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(fc), 1)
	if v, _ := t.property(tgsi.PropertyFSCoordPixelCenter); v == 1 {
		half := uint32(t.float32Const(0.5))
		x = t.f.Op(spirv.OpFSub, t.tF32, uint32(x), half)
		y = t.f.Op(spirv.OpFSub, t.tF32, uint32(y), half)
	}
	if v, _ := t.property(tgsi.PropertyFSCoordOrigin); v == 1 {
		h := t.bitcast(t.tF32, t.tU32, t.pushConstant(PushFramebufferHeight))
		y = t.f.Op(spirv.OpFSub, t.tF32, uint32(h), uint32(y))
	}
	z := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(fc), 2)
	w := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(fc), 3)
	vec := t.f.Op(spirv.OpCompositeConstruct, t.tVec4F, uint32(x), uint32(y), uint32(z), uint32(w))
	return t.bitcast(t.tVec4U, t.tVec4F, vec)
}

// systemValue returns a system value's value, as a register's bits.
func (t *translator) systemValue(sv *io) spirv.ID {
	sem, _, ok := sv.semantic()
	if !ok {
		t.fail("a system value of no semantic")
	}
	zero := t.m.ConstantUint(0)
	// uintVec returns a uvec4 of x, of 0 of the other channels.
	uintVec := func(x spirv.ID) spirv.ID {
		return t.f.Op(spirv.OpCompositeConstruct, t.tVec4U, uint32(x), uint32(zero), uint32(zero), uint32(zero))
	}
	intBuiltin := func(b spirv.BuiltIn) spirv.ID {
		return uintVec(t.bitcast(t.tU32, t.tI32, t.builtinValue(b, t.tI32, t.tI32)))
	}
	if t.s.Processor == tgsi.ProcessorCompute {
		if v := t.computeSystemValue(sem); v != 0 {
			return v
		}
	}
	switch sem {
	case tgsi.SemanticVertexID:
		return intBuiltin(spirv.BuiltInVertexIndex)
	case tgsi.SemanticInstanceID:
		// GL's instance IDs count from the draw's first instance, which
		// Vulkan's instance indexes include.
		i := t.builtinValue(spirv.BuiltInInstanceIndex, t.tI32, t.tI32)
		base := t.bitcast(t.tI32, t.tU32, t.pushConstant(PushBaseInstance))
		return uintVec(t.bitcast(t.tU32, t.tI32, t.f.Op(spirv.OpISub, t.tI32, uint32(i), uint32(base))))
	case tgsi.SemanticBaseInstance:
		return uintVec(t.pushConstant(PushBaseInstance))
	case tgsi.SemanticFace:
		front := t.builtinValue(spirv.BuiltInFrontFacing, t.tBool, t.tBool)
		// As a system value, FACE is an integer of ~0 for front faces.
		return uintVec(t.f.Op(spirv.OpSelect, t.tU32, uint32(front), uint32(t.m.ConstantUint(^uint32(0))), uint32(zero)))
	case tgsi.SemanticPosition:
		return t.fragCoord()
	case tgsi.SemanticSampleID:
		t.m.Capability(spirv.CapabilitySampleRateShading)
		return intBuiltin(spirv.BuiltInSampleId)
	case tgsi.SemanticSamplePos:
		t.m.Capability(spirv.CapabilitySampleRateShading)
		pos := t.builtinValue(spirv.BuiltInSamplePosition, t.tVec2F, t.tF32)
		vec := t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(pos), uint32(t.constVec4F(0, 0, 0, 0)), 0, 1, 2, 3)
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	case tgsi.SemanticSampleMask:
		typ := t.m.TypeArray(t.tI32, t.m.ConstantUint(1))
		f := t.builtin(spirv.StorageClassInput, spirv.BuiltInSampleMask, typ, t.tI32, 0, 0)
		return uintVec(t.bitcast(t.tU32, t.tI32, t.load(t.tI32, t.chanPtr(channel{f, 0}, 0))))
	case tgsi.SemanticHelperInvocation:
		h := t.builtinValue(spirv.BuiltInHelperInvocation, t.tBool, t.tBool)
		return uintVec(t.f.Op(spirv.OpSelect, t.tU32, uint32(h), uint32(t.m.ConstantUint(^uint32(0))), uint32(zero)))
	case tgsi.SemanticPrimID:
		switch t.s.Processor {
		case tgsi.ProcessorGeometry:
			t.m.Capability(spirv.CapabilityGeometry)
		case tgsi.ProcessorFragment:
			t.m.Capability(spirv.CapabilityGeometry)
		}
		return intBuiltin(spirv.BuiltInPrimitiveId)
	case tgsi.SemanticInvocationID:
		return intBuiltin(spirv.BuiltInInvocationId)
	case tgsi.SemanticVerticesIn:
		return intBuiltin(spirv.BuiltInPatchVertices)
	case tgsi.SemanticTessCoord:
		tc := t.builtinValue(spirv.BuiltInTessCoord, t.tVec3F, t.tF32)
		vec := t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(tc), uint32(t.constVec4F(0, 0, 0, 0)), 0, 1, 2, 3)
		return t.bitcast(t.tVec4U, t.tVec4F, vec)
	case tgsi.SemanticTessOuter, tgsi.SemanticTessInner:
		o := &io{decl: sv.decl, chans: t.tessLevelChannels(spirv.StorageClassInput, sem)}
		return t.loadChannels(o, 0)
	}
	t.fail("system value %v of %v", sem, t.s.Processor)
	return 0
}

// epilogue copies the shader's output registers to its outputs, as a
// geometry shader's EMIT does.
func (t *translator) epilogue() {
	if t.s.Processor == tgsi.ProcessorFragment && t.opts.AlphaTest {
		t.alphaTest()
	}
	for _, i := range sortedKeys(t.outputs) {
		o := t.outputs[i]
		if o.reg == 0 {
			continue
		}
		bits := t.load(t.tVec4U, o.reg)
		for _, f := range o.colors {
			t.store(f.v, t.bitcast(f.vec, t.tVec4U, bits))
		}
		t.storeChannels(o, 0, bits, tgsi.WriteMaskXYZW)
	}
	for _, o := range t.extraOutputs {
		t.storeChannels(o, 0, t.zero4U, tgsi.WriteMaskXYZW)
	}
	if t.s.Processor != tgsi.ProcessorFragment && t.s.Processor != tgsi.ProcessorTessCtrl {
		t.clipPlanes()
	}
	if so := t.opts.StreamOutput; so != nil && len(so.Outputs) > 0 {
		t.captureStreamOutput(so)
	}
	if t.opts.PointSize > 0 {
		switch t.s.Processor {
		case tgsi.ProcessorVertex, tgsi.ProcessorTessEval, tgsi.ProcessorGeometry:
			t.pointSizeCapability()
			f := t.builtin(spirv.StorageClassOutput, spirv.BuiltInPointSize, t.tF32, t.tF32, 0, 0)
			t.store(f.v, t.float32Const(t.opts.PointSize))
		}
	}
	if t.opts.GLClipZ {
		t.vulkanClipZ()
	}
}

// vulkanClipZ makes the position, of OpenGL's clip space, Vulkan's: its z,
// of [-w, w], (z + w) / 2, of [0, w], once the clip planes and the stream
// output used OpenGL's.
func (t *translator) vulkanClipZ() {
	switch t.s.Processor {
	case tgsi.ProcessorVertex, tgsi.ProcessorTessEval, tgsi.ProcessorGeometry:
	default:
		return
	}
	f := t.builtins[builtinKey{spirv.StorageClassOutput, spirv.BuiltInPosition}]
	if f == nil {
		return
	}
	pos := t.load(t.tVec4F, f.v)
	z := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(pos), 2)
	w := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(pos), 3)
	zw := t.f.Op(spirv.OpFAdd, t.tF32, uint32(z), uint32(w))
	half := t.f.Op(spirv.OpFMul, t.tF32, uint32(zw), uint32(t.float32Const(0.5)))
	t.store(f.v, t.f.Op(spirv.OpCompositeInsert, t.tVec4F, uint32(half), uint32(pos), 2))
}

// The mesa_prim primitives of the properties of geometry and tessellation
// shaders.
const (
	primPoints = iota
	primLines
	primLineLoop
	primLineStrip
	primTriangles
	primTriangleStrip
	primTriangleFan
	primQuads
	primQuadStrip
	primPolygon
	primLinesAdjacency
	primLineStripAdjacency
	primTrianglesAdjacency
	primTriangleStripAdjacency
)

// The pipe_tess_spacing spacings of TES_SPACING.
const (
	spacingFractionalOdd = iota
	spacingFractionalEven
	spacingEqual
)

// stageModes declares the execution modes of the shader's stage.
func (t *translator) stageModes(fn spirv.ID) {
	switch t.s.Processor {
	case tgsi.ProcessorFragment:
		t.fragmentModes(fn)
	case tgsi.ProcessorCompute:
		x, y, z := t.blockSize()
		t.m.ExecutionMode(fn, spirv.ExecutionModeLocalSize, x, y, z)
	case tgsi.ProcessorGeometry:
		t.geometryModes(fn)
	case tgsi.ProcessorTessCtrl:
		t.m.ExecutionMode(fn, spirv.ExecutionModeOutputVertices, uint32(t.outputVertices()))
	case tgsi.ProcessorTessEval:
		t.tessEvalModes(fn)
	}
}

func (t *translator) fragmentModes(fn spirv.ID) {
	// Vulkan's fragment coordinates are of the upper left origin; TGSI's
	// say theirs, and the renderer flips the framebuffer for a lower left
	// one.
	t.m.ExecutionMode(fn, spirv.ExecutionModeOriginUpperLeft)
	if v, ok := t.property(tgsi.PropertyFSEarlyDepthStencil); ok && v != 0 {
		t.m.ExecutionMode(fn, spirv.ExecutionModeEarlyFragmentTests)
	}
	if t.res.WritesDepth {
		t.m.ExecutionMode(fn, spirv.ExecutionModeDepthReplacing)
	}
}

func (t *translator) geometryModes(fn spirv.ID) {
	in := map[uint32]spirv.ExecutionMode{
		primPoints:                 spirv.ExecutionModeInputPoints,
		primLines:                  spirv.ExecutionModeInputLines,
		primLineStrip:              spirv.ExecutionModeInputLines,
		primLineLoop:               spirv.ExecutionModeInputLines,
		primTriangles:              spirv.ExecutionModeTriangles,
		primTriangleStrip:          spirv.ExecutionModeTriangles,
		primTriangleFan:            spirv.ExecutionModeTriangles,
		primLinesAdjacency:         spirv.ExecutionModeInputLinesAdjacency,
		primLineStripAdjacency:     spirv.ExecutionModeInputLinesAdjacency,
		primTrianglesAdjacency:     spirv.ExecutionModeInputTrianglesAdjacency,
		primTriangleStripAdjacency: spirv.ExecutionModeInputTrianglesAdjacency,
	}
	out := map[uint32]spirv.ExecutionMode{
		primPoints:        spirv.ExecutionModeOutputPoints,
		primLineStrip:     spirv.ExecutionModeOutputLineStrip,
		primTriangleStrip: spirv.ExecutionModeOutputTriangleStrip,
	}
	prim, _ := t.property(tgsi.PropertyGSInputPrimitive)
	mode, ok := in[prim]
	if !ok {
		t.fail("a geometry shader of input primitive %d", prim)
	}
	t.m.ExecutionMode(fn, mode)
	prim, _ = t.property(tgsi.PropertyGSOutputPrimitive)
	if mode, ok = out[prim]; !ok {
		t.fail("a geometry shader of output primitive %d", prim)
	}
	t.m.ExecutionMode(fn, mode)
	n, _ := t.property(tgsi.PropertyGSMaxOutputVertices)
	t.m.ExecutionMode(fn, spirv.ExecutionModeOutputVertices, n)
	invocations, _ := t.property(tgsi.PropertyGSInvocations)
	if invocations < 1 {
		invocations = 1
	}
	t.m.ExecutionMode(fn, spirv.ExecutionModeInvocations, invocations)
}

func (t *translator) tessEvalModes(fn spirv.ID) {
	prim, _ := t.property(tgsi.PropertyTESPrimMode)
	switch prim {
	case primTriangles:
		t.m.ExecutionMode(fn, spirv.ExecutionModeTriangles)
	case primQuads:
		t.m.ExecutionMode(fn, spirv.ExecutionModeQuads)
	case primLines:
		t.m.ExecutionMode(fn, spirv.ExecutionModeIsolines)
	default:
		t.fail("a tessellation of primitive %d", prim)
	}
	spacing, _ := t.property(tgsi.PropertyTESSpacing)
	switch spacing {
	case spacingFractionalOdd:
		t.m.ExecutionMode(fn, spirv.ExecutionModeSpacingFractionalOdd)
	case spacingFractionalEven:
		t.m.ExecutionMode(fn, spirv.ExecutionModeSpacingFractionalEven)
	default:
		t.m.ExecutionMode(fn, spirv.ExecutionModeSpacingEqual)
	}
	if cw, _ := t.property(tgsi.PropertyTESVertexOrderCW); cw != 0 {
		t.m.ExecutionMode(fn, spirv.ExecutionModeVertexOrderCw)
	} else {
		t.m.ExecutionMode(fn, spirv.ExecutionModeVertexOrderCcw)
	}
	if points, _ := t.property(tgsi.PropertyTESPointMode); points != 0 {
		t.m.ExecutionMode(fn, spirv.ExecutionModePointMode)
	}
}

// The comparison functions of Gallium's alpha test, PIPE_FUNC_*.
const (
	FuncNever = iota
	FuncLess
	FuncEqual
	FuncLessEqual
	FuncGreater
	FuncNotEqual
	FuncGreaterEqual
	FuncAlways
)

// alphaTest discards the fragment if its color's alpha fails the alpha
// test, as GL's fixed function does after the fragment shader.
func (t *translator) alphaTest() {
	var color *io
	for _, o := range t.outputs {
		if sem, index, ok := o.semantic(); ok && sem == tgsi.SemanticColor && index == 0 {
			color = o
		}
	}
	if color == nil || t.opts.AlphaFunc == FuncAlways {
		return
	}
	t.kills = true
	bits := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.load(t.tVec4U, color.reg)), 3)
	alpha := t.bitcast(t.tF32, t.tU32, bits)
	ref := uint32(t.float32Const(t.opts.AlphaRef))
	var pass spirv.ID
	switch t.opts.AlphaFunc {
	case FuncNever:
		pass = t.m.ConstantBool(false)
	case FuncLess:
		pass = t.f.Op(spirv.OpFOrdLessThan, t.tBool, uint32(alpha), ref)
	case FuncEqual:
		pass = t.f.Op(spirv.OpFOrdEqual, t.tBool, uint32(alpha), ref)
	case FuncLessEqual:
		pass = t.f.Op(spirv.OpFOrdLessThanEqual, t.tBool, uint32(alpha), ref)
	case FuncGreater:
		pass = t.f.Op(spirv.OpFOrdGreaterThan, t.tBool, uint32(alpha), ref)
	case FuncNotEqual:
		pass = t.f.Op(spirv.OpFUnordNotEqual, t.tBool, uint32(alpha), ref)
	case FuncGreaterEqual:
		pass = t.f.Op(spirv.OpFOrdGreaterThanEqual, t.tBool, uint32(alpha), ref)
	default:
		t.fail("alpha function %d", t.opts.AlphaFunc)
	}
	kill, merge := t.m.NewID(), t.m.NewID()
	t.f.Do(spirv.OpSelectionMerge, uint32(merge), uint32(spirv.SelectionControlNone))
	t.f.Do(spirv.OpBranchConditional, uint32(pass), uint32(merge), uint32(kill))
	t.f.Begin(kill)
	t.f.Do(spirv.OpKill)
	t.f.Begin(merge)
}

// clipPlanes has the last stage before rasterization clip as GL's enabled
// clip planes say: of a shader that writes clip distances, it disables
// those of ClipDisable, and of one that writes none, it writes the
// distances of its CLIPVERTEX, or its POSITION, to UserClipPlanes.
func (t *translator) clipPlanes() {
	var position, clipVertex *io
	writes := false
	for _, o := range t.outputs {
		switch sem, _, _ := o.semantic(); sem {
		case tgsi.SemanticClipDist:
			writes = true
		case tgsi.SemanticPosition:
			position = o
		case tgsi.SemanticClipVertex:
			clipVertex = o
		}
	}
	if writes {
		nclip, _ := t.clipDistances()
		for i := 0; i < nclip && i < 8; i++ {
			if t.opts.ClipDisable&(1<<i) == 0 {
				continue
			}
			// A distance of 0 clips nothing.
			f := t.builtins[builtinKey{spirv.StorageClassOutput, spirv.BuiltInClipDistance}]
			t.store(t.chanPtr(channel{f, i}, 0), t.float32Const(0))
		}
		return
	}
	n := len(t.opts.UserClipPlanes)
	if n == 0 {
		return
	}
	if n > 8 {
		t.fail("%d clip planes", n)
	}
	from := clipVertex
	if from == nil {
		from = position
	}
	t.m.Capability(spirv.CapabilityClipDistance)
	typ := t.m.TypeArray(t.tF32, t.m.ConstantUint(uint32(n)))
	f := t.builtin(spirv.StorageClassOutput, spirv.BuiltInClipDistance, typ, t.tF32, 0, 0)
	for i, p := range t.opts.UserClipPlanes {
		d := t.float32Const(1)
		if from != nil {
			v := t.bitcast(t.tVec4F, t.tVec4U, t.load(t.tVec4U, from.reg))
			d = t.f.Op(spirv.OpDot, t.tF32, uint32(v), uint32(t.constVec4F(p[0], p[1], p[2], p[3])))
		}
		t.store(t.chanPtr(channel{f, i}, 0), d)
	}
}
