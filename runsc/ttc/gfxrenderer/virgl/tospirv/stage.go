// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// stageInstruction translates an instruction of a stage's own, a geometry
// shader's or a fragment shader's, and returns whether it is one.
func (t *translator) stageInstruction(inst *tgsi.Instruction) bool {
	switch inst.Opcode {
	case tgsi.OpEMIT, tgsi.OpENDPRIM:
		if t.s.Processor != tgsi.ProcessorGeometry {
			t.fail("%v of %v", inst.Opcode, t.s.Processor)
		}
		stream := t.immediateChannel(&inst.Src[0])
		if inst.Opcode == tgsi.OpEMIT {
			// The vertex is of the outputs' values.
			t.epilogue()
			if stream == 0 {
				t.f.Do(spirv.OpEmitVertex)
			} else {
				t.m.Capability(spirv.CapabilityGeometryStreams)
				t.f.Do(spirv.OpEmitStreamVertex, uint32(t.m.ConstantUint(stream)))
			}
		} else {
			if stream == 0 {
				t.f.Do(spirv.OpEndPrimitive)
			} else {
				t.m.Capability(spirv.CapabilityGeometryStreams)
				t.f.Do(spirv.OpEndStreamPrimitive, uint32(t.m.ConstantUint(stream)))
			}
		}
	case tgsi.OpDEMOTE:
		t.fragmentOnly(inst)
		t.demoteExtension()
		t.kills = true
		t.f.Do(spirv.OpDemoteToHelperInvocation)
	case tgsi.OpREAD_HELPER:
		t.fragmentOnly(inst)
		t.demoteExtension()
		h := t.f.Op(spirv.OpIsHelperInvocationEXT, t.tBool)
		mask := t.f.Op(spirv.OpSelect, t.tU32, uint32(h), uint32(t.m.ConstantUint(^uint32(0))), uint32(t.m.ConstantUint(0)))
		t.dst(inst, 0, Uint, t.splat(t.tVec4U, mask))
	case tgsi.OpINTERP_CENTROID, tgsi.OpINTERP_SAMPLE, tgsi.OpINTERP_OFFSET:
		t.interpolate(inst)
	case tgsi.OpFBFETCH:
		t.fragmentOnly(inst)
		t.fbfetch(inst)
	default:
		return false
	}
	return true
}

// fbfetch translates FBFETCH, which reads the framebuffer's color of its
// source, a color output: from the color buffer's input attachment, which
// the rendering reads locally.
func (t *translator) fbfetch(inst *tgsi.Instruction) {
	s := &inst.Src[0]
	if s.File != tgsi.FileOutput || s.Indirect != nil {
		t.fail("FBFETCH of %v", s.File)
	}
	sem, k, _ := t.ioReg(&s.Register).semantic()
	if sem != tgsi.SemanticColor || k < 0 || k >= MaxColorBuffers {
		t.fail("FBFETCH of %v[%d]", sem, k)
	}
	b := t.opts.ColorTypes[k]
	ms := uint32(0)
	if t.opts.Multisampled {
		ms = 1
	}
	tImage := t.m.TypeImage(t.scalarType(b), spirv.DimSubpassData, 0, 0, ms, 2, spirv.ImageFormatUnknown)
	v, ok := t.attachments[k]
	if !ok {
		t.m.Capability(spirv.CapabilityInputAttachment)
		v = t.m.Variable(t.m.TypePointer(spirv.StorageClassUniformConstant, tImage), spirv.StorageClassUniformConstant)
		t.m.Decorate(v, spirv.DecorationDescriptorSet, SetInputAttachments)
		t.m.Decorate(v, spirv.DecorationBinding, uint32(k))
		t.m.Decorate(v, spirv.DecorationInputAttachmentIndex, uint32(k))
		t.attachments[k] = v
		t.resource(Resource{Kind: InputAttachment, Slot: k, Type: b}, nil)
	}
	// A subpass's input is of the fragment's own pixel: its coordinates
	// are offsets, of 0.
	tVec2I := t.m.TypeVector(t.tI32, 2)
	ops := []uint32{uint32(t.load(tImage, v)), uint32(t.m.ConstantNull(tVec2I))}
	if ms != 0 {
		t.m.Capability(spirv.CapabilitySampleRateShading)
		sample := t.builtinValue(spirv.BuiltInSampleId, t.tI32, t.tI32)
		ops = append(ops, uint32(spirv.ImageOperandsSample), uint32(sample))
	}
	texel := t.f.Op(spirv.OpImageRead, t.vecType(b), ops...)
	bits := t.bitcast(t.tVec4U, t.vecType(b), texel)
	t.dst(inst, 0, Uint, t.modify(inst, 0, bits, Uint))
}

// fragmentOnly fails unless the shader is a fragment shader.
func (t *translator) fragmentOnly(inst *tgsi.Instruction) {
	if t.s.Processor != tgsi.ProcessorFragment {
		t.fail("%v of %v", inst.Opcode, t.s.Processor)
	}
}

func (t *translator) demoteExtension() {
	t.m.Extension("SPV_EXT_demote_to_helper_invocation")
	t.m.Capability(spirv.CapabilityDemoteToHelperInvocation)
}

// immediateChannel returns the value of an immediate source's x, as its
// swizzle has it.
func (t *translator) immediateChannel(s *tgsi.SrcRegister) uint32 {
	if s.File != tgsi.FileImmediate || s.Indirect != nil || s.Index < 0 || s.Index >= len(t.s.Immediates) {
		t.fail("a source of %v, not an immediate", s.File)
	}
	vals := t.s.Immediates[s.Index].Values
	c := int(s.Swizzle[0] & 3)
	if c >= len(vals) {
		return 0
	}
	return vals[c]
}

// interpolate emits INTERP_CENTROID, INTERP_SAMPLE or INTERP_OFFSET: a
// varying's value at the pixel's centroid, at a sample of it, or at an
// offset from its center.
func (t *translator) interpolate(inst *tgsi.Instruction) {
	t.fragmentOnly(inst)
	s := &inst.Src[0]
	if s.File != tgsi.FileInput || s.Indirect != nil {
		t.fail("%v of %v", inst.Opcode, s.File)
	}
	o := t.ioReg(&s.Register)
	if o.input == nil {
		t.fail("%v of IN[%d], no varying", inst.Opcode, s.Index)
	}
	t.m.Capability(spirv.CapabilityInterpolationFunction)
	var v spirv.ID
	switch inst.Opcode {
	case tgsi.OpINTERP_CENTROID:
		v = t.glsl(t.tVec4F, spirv.GLSLstd450InterpolateAtCentroid, o.input.v)
	case tgsi.OpINTERP_SAMPLE:
		sample := t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(t.src(inst, 1, Int)), 0)
		v = t.glsl(t.tVec4F, spirv.GLSLstd450InterpolateAtSample, o.input.v, sample)
	default:
		off := t.src(inst, 1, Float)
		xy := t.f.Op(spirv.OpVectorShuffle, t.tVec2F, uint32(off), uint32(off), 0, 1)
		v = t.glsl(t.tVec4F, spirv.GLSLstd450InterpolateAtOffset, o.input.v, xy)
	}
	t.dst(inst, 0, Float, t.modify(inst, 0, t.bitcast(t.tVec4U, t.tVec4F, v), Float))
}
