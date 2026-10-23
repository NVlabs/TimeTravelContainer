// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"math"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// texInfo is what a texture target is of Vulkan's images: its dimension,
// its coordinates' channels, but the array's layer, whether it is arrayed,
// shadow or multisampled.
type texInfo struct {
	dim     spirv.Dim
	coords  int
	arrayed bool
	shadow  bool
	ms      bool
	rect    bool
}

func textureInfo(target tgsi.TextureTarget) (texInfo, bool) {
	switch target {
	case tgsi.Texture1D:
		return texInfo{dim: spirv.Dim1D, coords: 1}, true
	case tgsi.Texture2D:
		return texInfo{dim: spirv.Dim2D, coords: 2}, true
	case tgsi.Texture3D:
		return texInfo{dim: spirv.Dim3D, coords: 3}, true
	case tgsi.TextureCube:
		return texInfo{dim: spirv.DimCube, coords: 3}, true
	case tgsi.TextureRect:
		return texInfo{dim: spirv.Dim2D, coords: 2, rect: true}, true
	case tgsi.TextureShadow1D:
		return texInfo{dim: spirv.Dim1D, coords: 1, shadow: true}, true
	case tgsi.TextureShadow2D:
		return texInfo{dim: spirv.Dim2D, coords: 2, shadow: true}, true
	case tgsi.TextureShadowRect:
		return texInfo{dim: spirv.Dim2D, coords: 2, shadow: true, rect: true}, true
	case tgsi.Texture1DArray:
		return texInfo{dim: spirv.Dim1D, coords: 1, arrayed: true}, true
	case tgsi.Texture2DArray:
		return texInfo{dim: spirv.Dim2D, coords: 2, arrayed: true}, true
	case tgsi.TextureShadow1DArray:
		return texInfo{dim: spirv.Dim1D, coords: 1, arrayed: true, shadow: true}, true
	case tgsi.TextureShadow2DArray:
		return texInfo{dim: spirv.Dim2D, coords: 2, arrayed: true, shadow: true}, true
	case tgsi.TextureShadowCube:
		return texInfo{dim: spirv.DimCube, coords: 3, shadow: true}, true
	case tgsi.Texture2DMSAA:
		return texInfo{dim: spirv.Dim2D, coords: 2, ms: true}, true
	case tgsi.Texture2DArrayMSAA:
		return texInfo{dim: spirv.Dim2D, coords: 2, arrayed: true, ms: true}, true
	case tgsi.TextureCubeArray:
		return texInfo{dim: spirv.DimCube, coords: 3, arrayed: true}, true
	case tgsi.TextureShadowCubeArray:
		return texInfo{dim: spirv.DimCube, coords: 3, arrayed: true, shadow: true}, true
	case tgsi.TextureBuffer:
		return texInfo{dim: spirv.DimBuffer, coords: 1}, true
	}
	return texInfo{}, false
}

// components is the number of the coordinates' channels, the layer's
// included.
func (ti texInfo) components() int {
	if ti.arrayed {
		return ti.coords + 1
	}
	return ti.coords
}

// sampledImage returns the combined image sampler of a texture
// instruction's sampler slot, of its target, or of its array.
func (t *translator) sampledImage(slot int, target tgsi.TextureTarget) *sampler {
	if slot < 0 || slot >= StageBindings {
		t.fail("sampler %d", slot)
	}
	ti, ok := textureInfo(target)
	if !ok {
		t.fail("texture target %v", target)
	}
	key := slot
	if a := t.arrayOf(SampledTexture, slot); a != nil {
		key = a.first
	}
	if s := t.samplers[key]; s != nil {
		if s.target != target {
			t.fail("sampler %d of targets %v and %v", slot, s.target, target)
		}
		return s
	}
	typ := Float
	if d := t.sviews[slot]; d != nil {
		switch d.ReturnTypes[0] {
		case tgsi.ReturnSint:
			typ = Int
		case tgsi.ReturnUint:
			typ = Uint
		}
	}
	switch {
	case ti.dim == spirv.Dim1D:
		t.m.Capability(spirv.CapabilitySampled1D)
	case ti.dim == spirv.DimBuffer:
		t.m.Capability(spirv.CapabilitySampledBuffer)
	case ti.dim == spirv.DimCube && ti.arrayed:
		t.m.Capability(spirv.CapabilitySampledCubeArray)
	}
	depth, arrayed, ms := uint32(0), uint32(0), uint32(0)
	if ti.shadow {
		depth = 1
	}
	if ti.arrayed {
		arrayed = 1
	}
	if ti.ms {
		ms = 1
	}
	s := &sampler{target: target, typ: typ}
	s.tImage = t.m.TypeImage(t.scalarType(typ), ti.dim, depth, arrayed, ms, 1, spirv.ImageFormatUnknown)
	var varType spirv.ID
	if ti.dim == spirv.DimBuffer {
		// A uniform texel buffer, which no sampler samples.
		varType = s.tImage
	} else {
		s.tSample = t.m.TypeSampledImage(s.tImage)
		varType = s.tSample
	}
	s.tPtr = t.m.TypePointer(spirv.StorageClassUniformConstant, varType)
	s.v, s.array = t.descriptorVar(SampledTexture, slot, spirv.StorageClassUniformConstant, varType, SetSamplers)
	t.samplers[key] = s
	t.resource(Resource{Kind: SampledTexture, Slot: slot, Target: target, Type: typ}, s.array)
	if t.opts.BorderSlots&(1<<slot) != 0 && s.array == nil && s.tSample != 0 && !ti.ms {
		// The slot's view, of a sampler of an opaque white border.
		s.border = t.m.Variable(s.tPtr, spirv.StorageClassUniformConstant)
		t.m.Decorate(s.border, spirv.DecorationDescriptorSet, SetSamplers)
		t.m.Decorate(s.border, spirv.DecorationBinding, ResourceBinding(t.s.Processor, BorderTexture, slot))
		s.b = t.opts.Borders[slot]
		t.resource(Resource{Kind: BorderTexture, Slot: slot, Target: target, Type: typ}, nil)
	}
	return s
}

// coordVector returns the first n channels of v, a vec4 of type vecTyp,
// as a scalar or vector of the scalar type.
func (t *translator) coordVector(v spirv.ID, scalar spirv.ID, n int) spirv.ID {
	if n == 1 {
		return t.f.Op(spirv.OpCompositeExtract, scalar, uint32(v), 0)
	}
	if n == 4 {
		return v
	}
	typ := t.m.TypeVector(scalar, uint32(n))
	ops := []uint32{uint32(v), uint32(v)}
	for i := 0; i < n; i++ {
		ops = append(ops, uint32(i))
	}
	return t.f.Op(spirv.OpVectorShuffle, typ, ops...)
}

// textureInstruction translates a texture instruction, and returns whether
// it is one.
func (t *translator) textureInstruction(inst *tgsi.Instruction) bool {
	switch inst.Opcode {
	case tgsi.OpTEX, tgsi.OpTXP, tgsi.OpTXB, tgsi.OpTXL, tgsi.OpTXD, tgsi.OpTEX2, tgsi.OpTXB2, tgsi.OpTXL2,
		tgsi.OpTXF, tgsi.OpLODQ:
		t.sample(inst)
		return true
	case tgsi.OpTXQ:
		t.query(inst)
		return true
	case tgsi.OpTXQS:
		t.querySamples(inst)
		return true
	case tgsi.OpTG4:
		t.gather(inst)
		return true
	}
	return false
}

// samplerOf returns the sampled texture of a texture instruction's SAMP
// source, its last, of the instruction's target, and a pointer to its
// descriptor.
func (t *translator) samplerOf(inst *tgsi.Instruction) (*sampler, texInfo, spirv.ID) {
	for i := len(inst.Src) - 1; i >= 0; i-- {
		r := &inst.Src[i]
		if r.File != tgsi.FileSampler {
			continue
		}
		ti, ok := textureInfo(inst.Texture)
		if !ok {
			t.fail("texture target %v", inst.Texture)
		}
		s := t.sampledImage(r.Index, inst.Texture)
		return s, ti, t.descriptor(s.v, s.array, s.tPtr, r.Index, r.Indirect)
	}
	t.fail("%v of no sampler", inst.Opcode)
	return nil, texInfo{}, 0
}

// image returns the image of s's descriptor at ptr, of its combined image
// sampler's.
func (t *translator) image(s *sampler, ptr spirv.ID) spirv.ID {
	if s.tSample == 0 {
		return t.load(s.tImage, ptr)
	}
	return t.f.Op(spirv.OpImage, s.tImage, uint32(t.load(s.tSample, ptr)))
}

func (t *translator) sample(inst *tgsi.Instruction) {
	op := inst.Opcode
	s, ti, ptr := t.samplerOf(inst)
	n := ti.components()

	if op == tgsi.OpTXF {
		t.fetchTexel(inst, s, ti, ptr)
		return
	}
	if op == tgsi.OpTXP && ti.arrayed {
		t.fail("TXP of %v", inst.Texture)
	}
	if ti.dim == spirv.DimBuffer || ti.ms {
		t.fail("%v of %v", op, inst.Texture)
	}
	src0 := t.src(inst, 0, Float)
	coord := t.coordVector(src0, t.tF32, n)
	img := t.load(s.tSample, ptr)

	if op == tgsi.OpTXP {
		// Projective: coordinates, and the reference, divided by w.
		w := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), 3)
		src0 = t.f.Op(spirv.OpFDiv, t.tVec4F, uint32(src0), uint32(t.splat(t.tVec4F, w)))
		coord = t.coordVector(src0, t.tF32, n)
	}
	if ti.rect {
		coord = t.normalizeRect(img, s, coord)
	}

	var ref spirv.ID
	if ti.shadow {
		switch n {
		case 4:
			ref = t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 1, Float)), 0)
		case 3:
			ref = t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), 3)
		default:
			ref = t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), 2)
		}
	}

	// Image operands: the bias or LOD, the gradients, the offset.
	var mask spirv.ImageOperands
	var operands []uint32
	explicit := false
	switch op {
	case tgsi.OpTXB:
		mask |= spirv.ImageOperandsBias
		operands = append(operands, uint32(t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), 3)))
	case tgsi.OpTXB2:
		mask |= spirv.ImageOperandsBias
		operands = append(operands, uint32(t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 1, Float)), 0)))
	case tgsi.OpTXL:
		mask |= spirv.ImageOperandsLod
		operands = append(operands, uint32(t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), 3)))
		explicit = true
	case tgsi.OpTXL2:
		mask |= spirv.ImageOperandsLod
		operands = append(operands, uint32(t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 1, Float)), 0)))
		explicit = true
	case tgsi.OpTXD:
		mask |= spirv.ImageOperandsGrad
		operands = append(operands,
			uint32(t.coordVector(t.src(inst, 1, Float), t.tF32, ti.coords)),
			uint32(t.coordVector(t.src(inst, 2, Float), t.tF32, ti.coords)))
		explicit = true
	}
	if t.s.Processor != tgsi.ProcessorFragment && !explicit && op != tgsi.OpLODQ {
		// Of shaders other than fragment shaders, implicit LODs are of
		// LOD 0.
		if mask&spirv.ImageOperandsBias != 0 {
			mask &^= spirv.ImageOperandsBias
			operands = operands[1:]
		}
		mask |= spirv.ImageOperandsLod
		operands = append([]uint32{uint32(t.float32Const(0))}, operands...)
		explicit = true
	}
	if off, ok := t.texOffset(inst, ti); ok {
		mask |= spirv.ImageOperandsConstOffset
		operands = append(operands, uint32(off))
	}

	if op == tgsi.OpLODQ {
		t.m.Capability(spirv.CapabilityImageQuery)
		lod := t.f.Op(spirv.OpImageQueryLod, t.tVec2F, uint32(img), uint32(coord))
		t.dst(inst, 0, Float, t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(lod), uint32(t.constVec4F(0, 0, 0, 0)), 0, 1, 2, 3))
		return
	}

	typ, spvOp := t.vecType(s.typ), spirv.OpImageSampleImplicitLod
	if explicit {
		spvOp = spirv.OpImageSampleExplicitLod
	}
	if ti.shadow {
		typ, spvOp = t.tF32, spirv.OpImageSampleDrefImplicitLod
		if explicit {
			spvOp = spirv.OpImageSampleDrefExplicitLod
		}
	}
	sampleOf := func(img spirv.ID) spirv.ID {
		args := []uint32{uint32(img), uint32(coord)}
		if ti.shadow {
			args = append(args, uint32(ref))
		}
		if mask != 0 {
			args = append(args, uint32(mask))
			args = append(args, operands...)
		}
		return t.f.Op(spvOp, typ, args...)
	}
	result := sampleOf(img)
	if s.border != 0 {
		result = t.blendBorder(s, ti, result, sampleOf(t.load(s.tSample, s.border)), ref, false, 0)
	}
	if ti.shadow {
		// GL's legacy depth texture mode's LUMINANCE: (r, r, r, 1).
		one := t.float32Const(1)
		result = t.f.Op(spirv.OpCompositeConstruct, t.tVec4F, uint32(result), uint32(result), uint32(result), uint32(one))
		t.dst(inst, 0, Float, result)
	} else {
		t.dst(inst, 0, s.typ, result)
	}
}

// blendBorder blends s's border color into tb, a sample of s of a sampler
// of a transparent black border, by its difference from ow, a sample of
// one of an opaque white border: of each channel, the border's weight. Of
// a shadow sampler, of a reference ref, the samples' comparisons of their
// borders' depths, 0 and 1, differ only where the comparison of s's
// border's depth can, which replaces them. Of a gather, the samples are
// of each texel's channel comp, or of their comparisons.
func (t *translator) blendBorder(s *sampler, ti texInfo, tb, ow, ref spirv.ID, gather bool, comp uint32) spirv.ID {
	b := s.b
	var typ, w spirv.ID
	sub, mul, add := spirv.OpFSub, spirv.OpFMul, spirv.OpFAdd
	switch {
	case ti.shadow:
		if b.ClampRef {
			ref = t.glsl(t.tF32, spirv.GLSLstd450FClamp, ref, t.float32Const(0), t.float32Const(1))
		}
		depth := math.Float32frombits(b.Color[0])
		if b.ClampRef {
			depth = min(max(depth, 0), 1)
		}
		black, white := t.compareRef(b.Compare, ref, 0), t.compareRef(b.Compare, ref, 1)
		typ, w = t.tF32, t.f.Op(spirv.OpFMul, t.tF32,
			uint32(t.f.Op(spirv.OpFSub, t.tF32, uint32(t.compareRef(b.Compare, ref, depth)), uint32(black))),
			uint32(t.f.Op(spirv.OpFSub, t.tF32, uint32(white), uint32(black))))
		if gather {
			typ, w = t.tVec4F, t.splat(t.tVec4F, w)
		}
	default:
		typ = t.vecType(s.typ)
		var parts [4]spirv.ID
		for i := range parts {
			c := b.Color[i]
			if gather {
				c = b.Color[comp&3]
			}
			parts[i] = t.m.Constant(t.scalarType(s.typ), c)
		}
		w = t.m.ConstantComposite(typ, parts[:]...)
		if s.typ != Float {
			sub, mul, add = spirv.OpISub, spirv.OpIMul, spirv.OpIAdd
		}
	}
	diff := t.f.Op(sub, typ, uint32(ow), uint32(tb))
	return t.f.Op(add, typ, uint32(tb), uint32(t.f.Op(mul, typ, uint32(diff), uint32(w))))
}

// compareRef returns 1.0 where a shadow sampler's comparison, PIPE_FUNC_*,
// of ref to depth passes, and 0.0 where it fails.
func (t *translator) compareRef(fn uint32, ref spirv.ID, depth float32) spirv.ID {
	var op spirv.Op
	switch fn & 7 {
	case 0:
		return t.float32Const(0)
	case 1:
		op = spirv.OpFOrdLessThan
	case 2:
		op = spirv.OpFOrdEqual
	case 3:
		op = spirv.OpFOrdLessThanEqual
	case 4:
		op = spirv.OpFOrdGreaterThan
	case 5:
		op = spirv.OpFUnordNotEqual
	case 6:
		op = spirv.OpFOrdGreaterThanEqual
	default:
		return t.float32Const(1)
	}
	pass := t.f.Op(op, t.tBool, uint32(ref), uint32(t.float32Const(depth)))
	return t.f.Op(spirv.OpSelect, t.tF32, uint32(pass), uint32(t.float32Const(1)), uint32(t.float32Const(0)))
}

// normalizeRect divides a rectangle texture's coordinates by its size, as
// Vulkan's coordinates are normalized.
func (t *translator) normalizeRect(img spirv.ID, s *sampler, coord spirv.ID) spirv.ID {
	t.m.Capability(spirv.CapabilityImageQuery)
	tVec2I := t.m.TypeVector(t.tI32, 2)
	image := t.f.Op(spirv.OpImage, s.tImage, uint32(img))
	size := t.f.Op(spirv.OpImageQuerySizeLod, tVec2I, uint32(image), uint32(t.m.ConstantInt(0)))
	sizeF := t.f.Op(spirv.OpConvertSToF, t.tVec2F, uint32(size))
	return t.f.Op(spirv.OpFDiv, t.tVec2F, uint32(coord), uint32(sizeF))
}

// texOffset returns a texture instruction's offset, of an immediate, as a
// constant of the coordinates' channels.
func (t *translator) texOffset(inst *tgsi.Instruction, ti texInfo) (spirv.ID, bool) {
	if len(inst.TexOffset) == 0 {
		return 0, false
	}
	off, ok := t.constOffset(inst.TexOffset[0], ti.coords)
	if !ok {
		t.fail("texture offsets of %v", inst.TexOffset[0].File)
	}
	return off, true
}

// constOffset returns an offset of an immediate, of n channels, as a
// constant, or false if it is not of an immediate.
func (t *translator) constOffset(off tgsi.TexOffset, n int) (spirv.ID, bool) {
	if off.File != tgsi.FileImmediate {
		return 0, false
	}
	if off.Index < 0 || off.Index >= len(t.s.Immediates) {
		t.fail("IMM[%d] is not declared", off.Index)
	}
	vals := t.s.Immediates[off.Index].Values
	var parts []spirv.ID
	for i := 0; i < n; i++ {
		c := off.Swizzle[i] & 3
		v := uint32(0)
		if int(c) < len(vals) {
			v = vals[c]
		}
		parts = append(parts, t.m.ConstantInt(int32(v)))
	}
	if len(parts) == 1 {
		return parts[0], true
	}
	return t.m.ConstantComposite(t.m.TypeVector(t.tI32, uint32(len(parts))), parts...), true
}

// gather emits TG4: the four texels that a bilinear filter would take,
// of a channel of each, or their comparisons with a reference.
func (t *translator) gather(inst *tgsi.Instruction) {
	s, ti, ptr := t.samplerOf(inst)
	if ti.dim == spirv.DimBuffer || ti.ms || ti.dim == spirv.Dim1D || ti.dim == spirv.Dim3D {
		t.fail("TG4 of %v", inst.Texture)
	}
	n := ti.components()
	src0 := t.src(inst, 0, Float)
	coord := t.coordVector(src0, t.tF32, n)
	img := t.load(s.tSample, ptr)
	if ti.rect {
		coord = t.normalizeRect(img, s, coord)
	}
	args := []uint32{uint32(img), uint32(coord)}
	op := spirv.OpImageGather
	var ref spirv.ID
	var comp uint32
	switch {
	case ti.shadow && n == 4:
		// A cube array's reference is source 1's x.
		op = spirv.OpImageDrefGather
		ref = t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 1, Float)), 0)
		args = append(args, uint32(ref))
	case ti.shadow:
		op = spirv.OpImageDrefGather
		c := uint32(n)
		if n < 2 {
			c = 2
		}
		ref = t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(src0), c)
		args = append(args, uint32(ref))
	default:
		// The channel, of source 1's immediate.
		comp = t.immediateChannel(&inst.Src[1])
		if comp > 3 {
			t.fail("TG4 of channel %d", comp)
		}
		args = append(args, uint32(t.m.ConstantUint(comp)))
	}
	switch len(inst.TexOffset) {
	case 0:
	case 1:
		if off, ok := t.constOffset(inst.TexOffset[0], ti.coords); ok {
			args = append(args, uint32(spirv.ImageOperandsConstOffset), uint32(off))
			break
		}
		// An offset of a register's, which gathers alone take.
		t.m.Capability(spirv.CapabilityImageGatherExtended)
		o := inst.TexOffset[0]
		r := tgsi.Register{File: o.File, Index: o.Index}
		bits := t.fetch(r)
		sw := []uint32{uint32(bits), uint32(bits)}
		for i := 0; i < ti.coords; i++ {
			sw = append(sw, uint32(o.Swizzle[i]&3))
		}
		vec := t.f.Op(spirv.OpVectorShuffle, t.m.TypeVector(t.tU32, uint32(ti.coords)), sw...)
		off := t.f.Op(spirv.OpBitcast, t.m.TypeVector(t.tI32, uint32(ti.coords)), uint32(vec))
		args = append(args, uint32(spirv.ImageOperandsOffset), uint32(off))
	case 4:
		// Four offsets, one of each texel.
		t.m.Capability(spirv.CapabilityImageGatherExtended)
		tVec2I := t.m.TypeVector(t.tI32, 2)
		var offs []spirv.ID
		for _, o := range inst.TexOffset {
			off, ok := t.constOffset(o, 2)
			if !ok {
				t.fail("TG4's offsets of %v", o.File)
			}
			offs = append(offs, off)
		}
		arr := t.m.ConstantComposite(t.m.TypeArray(tVec2I, t.m.ConstantUint(4)), offs...)
		args = append(args, uint32(spirv.ImageOperandsConstOffsets), uint32(arr))
	default:
		t.fail("TG4 of %d offsets", len(inst.TexOffset))
	}
	result := t.f.Op(op, t.vecType(s.typ), args...)
	if s.border != 0 {
		args[0] = uint32(t.load(s.tSample, s.border))
		result = t.blendBorder(s, ti, result, t.f.Op(op, t.vecType(s.typ), args...), ref, true, comp)
	}
	t.dst(inst, 0, s.typ, result)
}

// querySamples emits TXQS: the number of a multisampled texture's samples.
func (t *translator) querySamples(inst *tgsi.Instruction) {
	s, ti, ptr := t.samplerOf(inst)
	if !ti.ms {
		t.fail("TXQS of %v", inst.Texture)
	}
	t.m.Capability(spirv.CapabilityImageQuery)
	n := t.f.Op(spirv.OpImageQuerySamples, t.tI32, uint32(t.image(s, ptr)))
	t.dst(inst, 0, Int, t.splat(t.tVec4I, n))
}

// fetchTexel emits TXF: a texel of integer coordinates, of the LOD in w,
// or a sample's of the index in w.
func (t *translator) fetchTexel(inst *tgsi.Instruction, s *sampler, ti texInfo, ptr spirv.ID) {
	src0 := t.src(inst, 0, Int)
	n := ti.components()
	coord := t.coordVector(src0, t.tI32, n)
	img := t.image(s, ptr)
	w := t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(src0), 3)
	args := []uint32{uint32(img), uint32(coord)}
	switch {
	case ti.ms:
		args = append(args, uint32(spirv.ImageOperandsSample), uint32(w))
	case ti.dim != spirv.DimBuffer:
		mask := spirv.ImageOperandsLod
		ops := []uint32{uint32(w)}
		if off, ok := t.texOffset(inst, ti); ok {
			mask |= spirv.ImageOperandsConstOffset
			ops = append(ops, uint32(off))
		}
		args = append(args, uint32(mask))
		args = append(args, ops...)
	}
	t.dst(inst, 0, s.typ, t.f.Op(spirv.OpImageFetch, t.vecType(s.typ), args...))
}

// query emits TXQ: the size of the LOD in source 0's x, and the number of
// levels in w.
func (t *translator) query(inst *tgsi.Instruction) {
	s, ti, ptr := t.samplerOf(inst)
	t.m.Capability(spirv.CapabilityImageQuery)
	img := t.image(s, ptr)
	n := ti.components()
	if ti.dim == spirv.DimCube {
		// A cube's size is of its faces', and an array's layers'.
		n = 2
		if ti.arrayed {
			n = 3
		}
	}
	sizeType := t.tI32
	if n > 1 {
		sizeType = t.m.TypeVector(t.tI32, uint32(n))
	}
	var size, levels spirv.ID
	if ti.dim == spirv.DimBuffer || ti.ms {
		size = t.f.Op(spirv.OpImageQuerySize, sizeType, uint32(img))
		levels = t.m.ConstantInt(1)
	} else {
		lod := t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(t.src(inst, 0, Int)), 0)
		size = t.f.Op(spirv.OpImageQuerySizeLod, sizeType, uint32(img), uint32(lod))
		levels = t.f.Op(spirv.OpImageQueryLevels, t.tI32, uint32(img))
	}
	var parts [4]uint32
	zero := uint32(t.m.ConstantInt(0))
	for i := 0; i < 3; i++ {
		switch {
		case i >= n:
			parts[i] = zero
		case n == 1:
			parts[i] = uint32(size)
		default:
			parts[i] = uint32(t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(size), uint32(i)))
		}
	}
	parts[3] = uint32(levels)
	t.dst(inst, 0, Int, t.f.Op(spirv.OpCompositeConstruct, t.tVec4I, parts[:]...))
}
