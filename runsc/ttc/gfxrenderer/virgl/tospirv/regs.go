// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// pendingLocation is a Location decoration whose word result finds, of a
// varying.
type pendingLocation struct {
	v        spirv.ID
	varyings *[]Varying
	i        int
}

// resolveLocations finds the words of the varyings' Location decorations
// in the module's words.
func (t *translator) resolveLocations(words []uint32) {
	at := make(map[spirv.ID]int)
	for i := 5; i < len(words); {
		n := int(words[i] >> 16)
		if n == 0 {
			break
		}
		if spirv.Op(words[i]&0xffff) == spirv.OpDecorate && n >= 4 && spirv.Decoration(words[i+2]) == spirv.DecorationLocation {
			at[spirv.ID(words[i+1])] = i + 3
		}
		i += n
	}
	for _, p := range t.locations {
		(*p.varyings)[p.i].LocationWord = at[p.v]
	}
}

// indirectIndex returns the index that an indirect register adds: a
// channel of an address register, or of another register, as an int.
func (t *translator) indirectIndex(ind *tgsi.Indirect) spirv.ID {
	var vec spirv.ID
	switch ind.File {
	case tgsi.FileAddress:
		a, ok := t.addrs[ind.Index]
		if !ok {
			t.fail("ADDR[%d] is not declared", ind.Index)
		}
		vec = t.load(t.tVec4I, a)
	case tgsi.FileTemporary, tgsi.FileInput, tgsi.FileConstant, tgsi.FileImmediate, tgsi.FileSystemValue:
		bits := t.fetch(tgsi.Register{File: ind.File, Index: ind.Index})
		vec = t.bitcast(t.tVec4I, t.tVec4U, bits)
	default:
		t.fail("indexes of %v", ind.File)
	}
	return t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(vec), uint32(ind.Swizzle&3))
}

// arrayElement returns a pointer to the element of an array of registers
// that a register, which may index it indirectly, is: clamped to the
// array, as robust drivers read past arrays.
func (t *translator) arrayElement(arr *regArray, r *tgsi.Register) spirv.ID {
	var idx spirv.ID
	if r.Indirect != nil {
		i := t.f.Op(spirv.OpIAdd, t.tI32, uint32(t.indirectIndex(r.Indirect)),
			uint32(t.m.ConstantInt(int32(r.Index-arr.first))))
		max := t.m.ConstantInt(int32(arr.last - arr.first))
		idx = t.f.Op(spirv.OpExtInst, t.tI32, uint32(t.m.GLSL()), uint32(spirv.GLSLstd450SClamp),
			uint32(i), uint32(t.m.ConstantInt(0)), uint32(max))
	} else {
		idx = t.m.ConstantInt(int32(r.Index - arr.first))
	}
	return t.f.Op(spirv.OpAccessChain, t.tPtrPrivU4, uint32(arr.v), uint32(idx))
}

// ioReg returns an IN, OUT or SV register.
func (t *translator) ioReg(r *tgsi.Register) *io {
	regs := t.inputs
	switch r.File {
	case tgsi.FileOutput:
		regs = t.outputs
	case tgsi.FileSystemValue:
		regs = t.sysvals
	}
	o, ok := regs[r.Index]
	if !ok {
		t.fail("%v[%d] is not declared", r.File, r.Index)
	}
	return o
}

// ioVertex returns the vertex of a register whose channels are of each
// vertex of the interface, or 0.
func (t *translator) ioVertex(r *tgsi.Register, o *io) spirv.ID {
	for _, ch := range o.chans {
		if ch.f != nil && ch.f.perVertex {
			return t.vertexIndex(r, o)
		}
	}
	return 0
}

// regPointer returns a pointer to a TEMP, IN, OUT, or SV register's
// variable.
func (t *translator) regPointer(r *tgsi.Register) spirv.ID {
	switch r.File {
	case tgsi.FileTemporary:
		if r.Indirect != nil {
			reg, ok := t.temps[r.Index]
			if !ok || reg.array == nil {
				// The index of an array whose first register is
				// indexed past.
				for _, cand := range t.temps {
					if cand.array != nil && r.Index >= cand.array.first && r.Index <= cand.array.last {
						return t.arrayElement(cand.array, r)
					}
				}
				t.fail("TEMP[%d] is indexed but not of an array", r.Index)
			}
			return t.arrayElement(reg.array, r)
		}
		reg, ok := t.temps[r.Index]
		if !ok {
			t.fail("TEMP[%d] is not declared", r.Index)
		}
		if reg.array != nil {
			return t.arrayElement(reg.array, r)
		}
		return reg.v
	case tgsi.FileInput, tgsi.FileOutput, tgsi.FileSystemValue:
		if r.Indirect != nil {
			t.fail("indirect %v registers", r.File)
		}
		o := t.ioReg(r)
		if o.reg == 0 {
			t.fail("%v[%d] of the interface", r.File, r.Index)
		}
		return o.reg
	}
	t.fail("registers of %v", r.File)
	return 0
}

// fetch returns a register's bits, a uvec4.
func (t *translator) fetch(r tgsi.Register) spirv.ID {
	switch r.File {
	case tgsi.FileImmediate:
		if r.Indirect != nil {
			t.fail("indirect immediates")
		}
		if r.Index < 0 || r.Index >= len(t.imms) {
			t.fail("IMM[%d] is not declared", r.Index)
		}
		return t.imms[r.Index]
	case tgsi.FileConstant:
		buf := 0
		var bufInd *tgsi.Indirect
		if r.Dimension {
			buf, bufInd = r.DimIndex, r.DimIndirect
		}
		c := t.constBlock(buf)
		block := t.descriptor(c.v, c.array, c.tPtr, buf, bufInd)
		var idx spirv.ID
		if r.Indirect != nil {
			i := t.f.Op(spirv.OpIAdd, t.tI32, uint32(t.indirectIndex(r.Indirect)), uint32(t.m.ConstantInt(int32(r.Index))))
			idx = t.f.Op(spirv.OpExtInst, t.tI32, uint32(t.m.GLSL()), uint32(spirv.GLSLstd450SClamp),
				uint32(i), uint32(t.m.ConstantInt(0)), uint32(t.m.ConstantInt(int32(c.size-1))))
		} else {
			if r.Index < 0 || r.Index >= c.size {
				t.fail("CONST[%d][%d] is past its declaration", buf, r.Index)
			}
			idx = t.m.ConstantInt(int32(r.Index))
		}
		ptr := t.f.Op(spirv.OpAccessChain, t.m.TypePointer(spirv.StorageClassUniform, t.tVec4U),
			uint32(block), uint32(t.m.ConstantUint(0)), uint32(idx))
		return t.load(t.tVec4U, ptr)
	case tgsi.FileInput, tgsi.FileOutput:
		if o := t.ioReg(&r); o.reg == 0 {
			if r.Indirect != nil {
				t.fail("indirect %v registers", r.File)
			}
			return t.loadChannels(o, t.ioVertex(&r, o))
		}
	}
	return t.load(t.tVec4U, t.regPointer(&r))
}

// src returns an instruction's source operand n, of type b: its register's
// channels, swizzled, of their absolute values and negated as it says, of
// the type of the operand that the instruction takes, as Mesa's
// tgsi_to_nir modifies them: absolute values of floats, and negations of
// floats or of integers.
func (t *translator) src(inst *tgsi.Instruction, n int, b BaseType) spirv.ID {
	if n >= len(inst.Src) {
		t.fail("source %d of %d", n, len(inst.Src))
	}
	return t.modify(inst, n, t.fetch(inst.Src[n].Register), b)
}

// modify returns bits, of source operand n's register, swizzled and
// modified as the operand says, as a vec4 of type b.
func (t *translator) modify(inst *tgsi.Instruction, n int, bits spirv.ID, b BaseType) spirv.ID {
	s := &inst.Src[n]
	if s.Swizzle != [4]uint8{0, 1, 2, 3} {
		bits = t.f.Op(spirv.OpVectorShuffle, t.tVec4U, uint32(bits), uint32(bits),
			uint32(s.Swizzle[0]&3), uint32(s.Swizzle[1]&3), uint32(s.Swizzle[2]&3), uint32(s.Swizzle[3]&3))
	}
	if s.Absolute {
		v := t.glsl(t.tVec4F, spirv.GLSLstd450FAbs, t.bitcast(t.tVec4F, t.tVec4U, bits))
		bits = t.bitcast(t.tVec4U, t.tVec4F, v)
	}
	if s.Negate {
		if inst.Opcode.SrcType(n).IsFloat() {
			v := t.f.Op(spirv.OpFNegate, t.tVec4F, uint32(t.bitcast(t.tVec4F, t.tVec4U, bits)))
			bits = t.bitcast(t.tVec4U, t.tVec4F, v)
		} else {
			bits = t.f.Op(spirv.OpSNegate, t.tVec4U, uint32(bits))
		}
	}
	return t.bitcast(t.vecType(b), t.tVec4U, bits)
}

// glsl emits a GLSL.std.450 instruction.
func (t *translator) glsl(typ spirv.ID, op spirv.GLSLstd450, args ...spirv.ID) spirv.ID {
	ops := []uint32{uint32(t.m.GLSL()), uint32(op)}
	for _, a := range args {
		ops = append(ops, uint32(a))
	}
	return t.f.Op(spirv.OpExtInst, typ, ops...)
}

// dst writes v, a vec4 of type b, to an instruction's destination n, of
// the channels of its write mask, saturated, as floats, if the instruction
// says.
func (t *translator) dst(inst *tgsi.Instruction, n int, b BaseType, v spirv.ID) {
	if n >= len(inst.Dst) {
		t.fail("destination %d of %d", n, len(inst.Dst))
	}
	d := &inst.Dst[n]
	bits := t.bitcast(t.tVec4U, t.vecType(b), v)
	if inst.Saturate {
		f := t.bitcast(t.tVec4F, t.tVec4U, bits)
		f = t.glsl(t.tVec4F, spirv.GLSLstd450FClamp, f, t.constVec4F(0, 0, 0, 0), t.constVec4F(1, 1, 1, 1))
		bits = t.bitcast(t.tVec4U, t.tVec4F, f)
	}
	t.storeReg(&d.Register, bits, d.WriteMask)
}

// storeReg writes the channels of mask of bits, a uvec4, to register r.
func (t *translator) storeReg(r *tgsi.Register, bits spirv.ID, mask uint8) {
	if r.File == tgsi.FileAddress {
		a, ok := t.addrs[r.Index]
		if !ok {
			t.fail("ADDR[%d] is not declared", r.Index)
		}
		ints := t.bitcast(t.tVec4I, t.tVec4U, bits)
		t.storeMasked(a, t.tVec4I, ints, mask)
		return
	}
	if r.File == tgsi.FileNull {
		return
	}
	if r.File == tgsi.FileOutput {
		if o := t.ioReg(r); o.reg == 0 {
			if r.Indirect != nil {
				t.fail("indirect %v registers", r.File)
			}
			t.storeChannels(o, t.ioVertex(r, o), bits, mask)
			return
		}
	}
	t.storeMasked(t.regPointer(r), t.tVec4U, bits, mask)
}

// storeMasked stores the channels of mask of v to ptr, a vec4 of typ.
func (t *translator) storeMasked(ptr, typ, v spirv.ID, mask uint8) {
	mask &= 0xf
	if mask == 0xf {
		t.store(ptr, v)
		return
	}
	if mask == 0 {
		return
	}
	old := t.load(typ, ptr)
	var sel [4]uint32
	for i := 0; i < 4; i++ {
		if mask&(1<<i) != 0 {
			sel[i] = uint32(4 + i)
		} else {
			sel[i] = uint32(i)
		}
	}
	merged := t.f.Op(spirv.OpVectorShuffle, typ, uint32(old), uint32(v), sel[0], sel[1], sel[2], sel[3])
	t.store(ptr, merged)
}
