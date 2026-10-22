// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// MaxStreamOutputBuffers is the number of a stream output's targets.
const MaxStreamOutputBuffers = 4

// StreamOutput is a vertex shader's stream output, which it captures to
// its targets, shader buffers, itself, as the renderer emulates transform
// feedback: at its end, the shader stores the outputs to the targets at its
// vertex's index among the draw's, which it computes of the capture's
// parameters, in uniform buffer UBO: the draw's first vertex, the vertices
// of an instance that it captures, and the vertices that fit the targets;
// then the targets' offsets in bytes.
type StreamOutput struct {
	Outputs []StreamOutputItem
	// Strides are the targets' vertices' strides, in dwords.
	Strides [MaxStreamOutputBuffers]uint32
	// UBO is the slot of the parameters' uniform buffer, and SSBO the
	// first of the targets' shader buffers' slots.
	UBO, SSBO int
}

// StreamOutputItem is an output that a stream output captures: channels
// of an output register, to a dword offset of a target's vertices.
type StreamOutputItem struct {
	Register       int
	StartComponent uint32
	NumComponents  uint32
	Buffer         uint32
	DstOffset      uint32
}

// The parameters of a capture, uints of the parameters' uniform buffer.
const (
	xfbFirst = iota
	xfbPerInstance
	xfbMax
	xfbOffsets = 4
)

// captureStreamOutput stores a vertex shader's outputs to its stream
// output's targets, at the end of its epilogue.
func (t *translator) captureStreamOutput(so *StreamOutput) {
	if t.s.Processor != tgsi.ProcessorVertex {
		t.fail("stream output of %v", t.s.Processor)
	}
	if t.consts[so.UBO] != nil {
		t.fail("constant buffer %d of the stream output's parameters is declared", so.UBO)
	}
	tArr := t.m.TypeArrayNew(t.tVec4U, t.m.ConstantUint(2))
	t.m.Decorate(tArr, spirv.DecorationArrayStride, 16)
	tBlock := t.m.TypeStruct(tArr)
	t.m.Decorate(tBlock, spirv.DecorationBlock)
	t.m.MemberDecorate(tBlock, 0, spirv.DecorationOffset, 0)
	ubo, _ := t.descriptorVar(UniformBuffer, so.UBO, spirv.StorageClassUniform, tBlock, SetUniformBuffers)
	t.resource(Resource{Kind: UniformBuffer, Slot: so.UBO, Size: 32}, nil)
	param := func(i uint32) spirv.ID {
		ptr := t.f.Op(spirv.OpAccessChain, t.m.TypePointer(spirv.StorageClassUniform, t.tU32), uint32(ubo),
			uint32(t.m.ConstantUint(0)), uint32(t.m.ConstantUint(i/4)), uint32(t.m.ConstantUint(i%4)))
		return t.load(t.tU32, ptr)
	}

	vertexIndex := t.bitcast(t.tU32, t.tI32, t.builtinValue(spirv.BuiltInVertexIndex, t.tI32, t.tI32))
	instanceIndex := t.bitcast(t.tU32, t.tI32, t.builtinValue(spirv.BuiltInInstanceIndex, t.tI32, t.tI32))
	instance := t.f.Op(spirv.OpISub, t.tU32, uint32(instanceIndex), uint32(t.pushConstant(PushBaseInstance)))
	vertex := t.f.Op(spirv.OpISub, t.tU32, uint32(vertexIndex), uint32(param(xfbFirst)))
	perInstance := param(xfbPerInstance)
	index := t.f.Op(spirv.OpIAdd, t.tU32,
		uint32(t.f.Op(spirv.OpIMul, t.tU32, uint32(instance), uint32(perInstance))), uint32(vertex))
	cond := t.f.Op(spirv.OpLogicalAnd, t.tBool,
		uint32(t.f.Op(spirv.OpULessThan, t.tBool, uint32(vertex), uint32(perInstance))),
		uint32(t.f.Op(spirv.OpULessThan, t.tBool, uint32(index), uint32(param(xfbMax)))))
	then, merge := t.m.NewID(), t.m.NewID()
	t.f.Do(spirv.OpSelectionMerge, uint32(merge), uint32(spirv.SelectionControlNone))
	t.f.Do(spirv.OpBranchConditional, uint32(cond), uint32(then), uint32(merge))
	t.f.Begin(then)
	for _, o := range so.Outputs {
		// An output must fit its vertex's stride, as OpenGL lays them out:
		// the draw's checks of the targets' room count on it.
		if o.Buffer >= MaxStreamOutputBuffers || o.NumComponents == 0 ||
			o.StartComponent+o.NumComponents > 4 || o.DstOffset+o.NumComponents > so.Strides[o.Buffer] {
			continue
		}
		out := t.outputs[o.Register]
		if out == nil || out.reg == 0 {
			continue
		}
		bits := t.load(t.tVec4U, out.reg)
		stride := t.m.ConstantUint(so.Strides[o.Buffer])
		// The dword of the output: the target's offset's, plus the
		// vertex's, plus the output's.
		word := t.f.Op(spirv.OpShiftRightLogical, t.tU32, uint32(param(xfbOffsets+o.Buffer)), uint32(t.m.ConstantUint(2)))
		word = t.f.Op(spirv.OpIAdd, t.tU32, uint32(word), uint32(t.f.Op(spirv.OpIMul, t.tU32, uint32(index), uint32(stride))))
		b := t.storageBuffer(so.SSBO + int(o.Buffer))
		ref := memoryRef{b, b.v}
		for c := uint32(0); c < o.NumComponents; c++ {
			w := t.f.Op(spirv.OpIAdd, t.tU32, uint32(word), uint32(t.m.ConstantUint(o.DstOffset+c)))
			v := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(bits), o.StartComponent+c)
			t.store(t.element(ref, w), v)
		}
	}
	t.f.Do(spirv.OpBranch, uint32(merge))
	t.f.Begin(merge)
}
