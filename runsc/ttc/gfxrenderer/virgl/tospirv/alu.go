// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// Componentwise instructions of SPIR-V's opcodes, by their operands' type.
var (
	floatBinary = map[tgsi.Opcode]spirv.Op{
		tgsi.OpADD: spirv.OpFAdd,
		tgsi.OpMUL: spirv.OpFMul,
		tgsi.OpDIV: spirv.OpFDiv,
	}
	floatUnaryGLSL = map[tgsi.Opcode]spirv.GLSLstd450{
		tgsi.OpFLR:   spirv.GLSLstd450Floor,
		tgsi.OpROUND: spirv.GLSLstd450RoundEven,
		tgsi.OpTRUNC: spirv.GLSLstd450Trunc,
		tgsi.OpCEIL:  spirv.GLSLstd450Ceil,
		tgsi.OpFRC:   spirv.GLSLstd450Fract,
		tgsi.OpSSG:   spirv.GLSLstd450FSign,
	}
	floatBinaryGLSL = map[tgsi.Opcode]spirv.GLSLstd450{
		tgsi.OpMIN: spirv.GLSLstd450NMin,
		tgsi.OpMAX: spirv.GLSLstd450NMax,
	}
	intBinary = map[tgsi.Opcode]struct {
		op spirv.Op
		b  BaseType
	}{
		tgsi.OpUADD: {spirv.OpIAdd, Uint},
		tgsi.OpUMUL: {spirv.OpIMul, Uint},
		tgsi.OpIDIV: {spirv.OpSDiv, Int},
		tgsi.OpUDIV: {spirv.OpUDiv, Uint},
		tgsi.OpMOD:  {spirv.OpSRem, Int},
		tgsi.OpUMOD: {spirv.OpUMod, Uint},
		tgsi.OpAND:  {spirv.OpBitwiseAnd, Uint},
		tgsi.OpOR:   {spirv.OpBitwiseOr, Uint},
		tgsi.OpXOR:  {spirv.OpBitwiseXor, Uint},
	}
	intBinaryGLSL = map[tgsi.Opcode]struct {
		op spirv.GLSLstd450
		b  BaseType
	}{
		tgsi.OpIMIN: {spirv.GLSLstd450SMin, Int},
		tgsi.OpIMAX: {spirv.GLSLstd450SMax, Int},
		tgsi.OpUMIN: {spirv.GLSLstd450UMin, Uint},
		tgsi.OpUMAX: {spirv.GLSLstd450UMax, Uint},
	}
	// Comparisons: their opcode, their operands' type, and whether they
	// give floats, 1.0 or 0.0, rather than masks, ~0 or 0.
	compares = map[tgsi.Opcode]struct {
		op     spirv.Op
		b      BaseType
		floats bool
	}{
		tgsi.OpSEQ:  {spirv.OpFOrdEqual, Float, true},
		tgsi.OpSNE:  {spirv.OpFUnordNotEqual, Float, true},
		tgsi.OpSLT:  {spirv.OpFOrdLessThan, Float, true},
		tgsi.OpSGE:  {spirv.OpFOrdGreaterThanEqual, Float, true},
		tgsi.OpSLE:  {spirv.OpFOrdLessThanEqual, Float, true},
		tgsi.OpSGT:  {spirv.OpFOrdGreaterThan, Float, true},
		tgsi.OpFSEQ: {spirv.OpFOrdEqual, Float, false},
		tgsi.OpFSNE: {spirv.OpFUnordNotEqual, Float, false},
		tgsi.OpFSLT: {spirv.OpFOrdLessThan, Float, false},
		tgsi.OpFSGE: {spirv.OpFOrdGreaterThanEqual, Float, false},
		tgsi.OpUSEQ: {spirv.OpIEqual, Uint, false},
		tgsi.OpUSNE: {spirv.OpINotEqual, Uint, false},
		tgsi.OpUSLT: {spirv.OpULessThan, Uint, false},
		tgsi.OpUSGE: {spirv.OpUGreaterThanEqual, Uint, false},
		tgsi.OpISLT: {spirv.OpSLessThan, Int, false},
		tgsi.OpISGE: {spirv.OpSGreaterThanEqual, Int, false},
	}
	// Scalar functions of their sources' first channel, replicated.
	scalarGLSL = map[tgsi.Opcode]spirv.GLSLstd450{
		tgsi.OpEX2:  spirv.GLSLstd450Exp2,
		tgsi.OpLG2:  spirv.GLSLstd450Log2,
		tgsi.OpSIN:  spirv.GLSLstd450Sin,
		tgsi.OpCOS:  spirv.GLSLstd450Cos,
		tgsi.OpSQRT: spirv.GLSLstd450Sqrt,
		tgsi.OpRSQ:  spirv.GLSLstd450InverseSqrt,
	}
)

// instruction translates an instruction, and returns whether it is END.
func (t *translator) instruction(inst *tgsi.Instruction) bool {
	info, ok := inst.Opcode.Info()
	if !ok {
		t.fail("opcode %d", inst.Opcode)
	}
	if len(inst.Dst) != info.NumDst || len(inst.Src) != info.NumSrc {
		t.fail("%s of %d destinations and %d sources", info.Name, len(inst.Dst), len(inst.Src))
	}
	if t.terminated && !t.flowContinues(inst.Opcode) {
		// Unreachable code after BRK, CONT, KILL or RET, before its
		// block's end: of a block of its own.
		t.f.Label()
		t.terminated = false
	}
	op := inst.Opcode
	if spv, ok := floatBinary[op]; ok {
		t.dst(inst, 0, Float, t.f.Op(spv, t.tVec4F, uint32(t.src(inst, 0, Float)), uint32(t.src(inst, 1, Float))))
		return false
	}
	if g, ok := floatUnaryGLSL[op]; ok {
		t.dst(inst, 0, Float, t.glsl(t.tVec4F, g, t.src(inst, 0, Float)))
		return false
	}
	if g, ok := floatBinaryGLSL[op]; ok {
		t.dst(inst, 0, Float, t.glsl(t.tVec4F, g, t.src(inst, 0, Float), t.src(inst, 1, Float)))
		return false
	}
	if ib, ok := intBinary[op]; ok {
		typ := t.vecType(ib.b)
		t.dst(inst, 0, ib.b, t.f.Op(ib.op, typ, uint32(t.src(inst, 0, ib.b)), uint32(t.src(inst, 1, ib.b))))
		return false
	}
	if ig, ok := intBinaryGLSL[op]; ok {
		t.dst(inst, 0, ig.b, t.glsl(t.vecType(ig.b), ig.op, t.src(inst, 0, ig.b), t.src(inst, 1, ig.b)))
		return false
	}
	if c, ok := compares[op]; ok {
		t.compare(inst, c.op, c.b, c.floats)
		return false
	}
	if g, ok := scalarGLSL[op]; ok {
		x := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 0, Float)), 0)
		t.dst(inst, 0, Float, t.splat(t.tVec4F, t.glsl(t.tF32, g, x)))
		return false
	}
	switch op {
	case tgsi.OpMOV:
		t.dst(inst, 0, Uint, t.src(inst, 0, Uint))
	case tgsi.OpMAD:
		mul := t.f.Op(spirv.OpFMul, t.tVec4F, uint32(t.src(inst, 0, Float)), uint32(t.src(inst, 1, Float)))
		t.dst(inst, 0, Float, t.f.Op(spirv.OpFAdd, t.tVec4F, uint32(mul), uint32(t.src(inst, 2, Float))))
	case tgsi.OpFMA:
		t.dst(inst, 0, Float, t.glsl(t.tVec4F, spirv.GLSLstd450Fma, t.src(inst, 0, Float), t.src(inst, 1, Float), t.src(inst, 2, Float)))
	case tgsi.OpUMAD:
		mul := t.f.Op(spirv.OpIMul, t.tVec4U, uint32(t.src(inst, 0, Uint)), uint32(t.src(inst, 1, Uint)))
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpIAdd, t.tVec4U, uint32(mul), uint32(t.src(inst, 2, Uint))))
	case tgsi.OpLRP:
		// src0 * src1 + (1 - src0) * src2: mix(src2, src1, src0).
		t.dst(inst, 0, Float, t.glsl(t.tVec4F, spirv.GLSLstd450FMix, t.src(inst, 2, Float), t.src(inst, 1, Float), t.src(inst, 0, Float)))
	case tgsi.OpRCP:
		x := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 0, Float)), 0)
		t.dst(inst, 0, Float, t.splat(t.tVec4F, t.f.Op(spirv.OpFDiv, t.tF32, uint32(t.float32Const(1)), uint32(x))))
	case tgsi.OpPOW:
		x := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 0, Float)), 0)
		y := t.f.Op(spirv.OpCompositeExtract, t.tF32, uint32(t.src(inst, 1, Float)), 0)
		t.dst(inst, 0, Float, t.splat(t.tVec4F, t.glsl(t.tF32, spirv.GLSLstd450Pow, x, y)))
	case tgsi.OpDP2, tgsi.OpDP3, tgsi.OpDP4:
		t.dot(inst)
	case tgsi.OpLDEXP:
		t.dst(inst, 0, Float, t.glsl(t.tVec4F, spirv.GLSLstd450Ldexp, t.src(inst, 0, Float), t.src(inst, 1, Int)))
	case tgsi.OpCMP:
		// src0 < 0 ? src1 : src2, of floats, whose bits it selects.
		lt := t.f.Op(spirv.OpFOrdLessThan, t.tVec4B, uint32(t.src(inst, 0, Float)), uint32(t.constVec4F(0, 0, 0, 0)))
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpSelect, t.tVec4U, uint32(lt), uint32(t.src(inst, 1, Uint)), uint32(t.src(inst, 2, Uint))))
	case tgsi.OpUCMP:
		// src0 != 0 ? src1 : src2, of their bits.
		ne := t.f.Op(spirv.OpINotEqual, t.tVec4B, uint32(t.src(inst, 0, Uint)), uint32(t.zero4U))
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpSelect, t.tVec4U, uint32(ne), uint32(t.src(inst, 1, Uint)), uint32(t.src(inst, 2, Uint))))
	case tgsi.OpDDX, tgsi.OpDDX_FINE:
		t.derivative(inst, spirv.OpDPdx, spirv.OpDPdxFine, op == tgsi.OpDDX_FINE)
	case tgsi.OpDDY, tgsi.OpDDY_FINE:
		t.derivative(inst, spirv.OpDPdy, spirv.OpDPdyFine, op == tgsi.OpDDY_FINE)
	case tgsi.OpI2F:
		t.dst(inst, 0, Float, t.f.Op(spirv.OpConvertSToF, t.tVec4F, uint32(t.src(inst, 0, Int))))
	case tgsi.OpU2F:
		t.dst(inst, 0, Float, t.f.Op(spirv.OpConvertUToF, t.tVec4F, uint32(t.src(inst, 0, Uint))))
	case tgsi.OpF2I:
		t.dst(inst, 0, Int, t.f.Op(spirv.OpConvertFToS, t.tVec4I, uint32(t.src(inst, 0, Float))))
	case tgsi.OpF2U:
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpConvertFToU, t.tVec4U, uint32(t.src(inst, 0, Float))))
	case tgsi.OpINEG:
		t.dst(inst, 0, Int, t.f.Op(spirv.OpSNegate, t.tVec4I, uint32(t.src(inst, 0, Int))))
	case tgsi.OpIABS:
		t.dst(inst, 0, Int, t.glsl(t.tVec4I, spirv.GLSLstd450SAbs, t.src(inst, 0, Int)))
	case tgsi.OpISSG:
		t.dst(inst, 0, Int, t.glsl(t.tVec4I, spirv.GLSLstd450SSign, t.src(inst, 0, Int)))
	case tgsi.OpNOT:
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpNot, t.tVec4U, uint32(t.src(inst, 0, Uint))))
	case tgsi.OpSHL, tgsi.OpISHR, tgsi.OpUSHR:
		t.shift(inst)
	case tgsi.OpIMUL_HI, tgsi.OpUMUL_HI:
		t.mulHigh(inst)
	case tgsi.OpARL:
		t.dst(inst, 0, Int, t.f.Op(spirv.OpConvertFToS, t.tVec4I, uint32(t.glsl(t.tVec4F, spirv.GLSLstd450Floor, t.src(inst, 0, Float)))))
	case tgsi.OpUARL:
		t.dst(inst, 0, Int, t.src(inst, 0, Int))
	case tgsi.OpPK2H:
		x := t.src(inst, 0, Float)
		xy := t.f.Op(spirv.OpVectorShuffle, t.tVec2F, uint32(x), uint32(x), 0, 1)
		t.dst(inst, 0, Uint, t.splat(t.tVec4U, t.glsl(t.tU32, spirv.GLSLstd450PackHalf2x16, xy)))
	case tgsi.OpUP2H:
		x := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.src(inst, 0, Uint)), 0)
		xy := t.glsl(t.tVec2F, spirv.GLSLstd450UnpackHalf2x16, x)
		t.dst(inst, 0, Float, t.f.Op(spirv.OpVectorShuffle, t.tVec4F, uint32(xy), uint32(xy), 0, 1, 0, 1))
	case tgsi.OpBREV:
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpBitReverse, t.tVec4U, uint32(t.src(inst, 0, Uint))))
	case tgsi.OpPOPC:
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpBitCount, t.tVec4U, uint32(t.src(inst, 0, Uint))))
	case tgsi.OpLSB:
		t.dst(inst, 0, Int, t.glsl(t.tVec4I, spirv.GLSLstd450FindILsb, t.src(inst, 0, Int)))
	case tgsi.OpIMSB:
		t.dst(inst, 0, Int, t.glsl(t.tVec4I, spirv.GLSLstd450FindSMsb, t.src(inst, 0, Int)))
	case tgsi.OpUMSB:
		t.dst(inst, 0, Int, t.glsl(t.tVec4I, spirv.GLSLstd450FindUMsb, t.src(inst, 0, Uint)))
	case tgsi.OpIBFE, tgsi.OpUBFE:
		t.bitfieldExtract(inst)
	case tgsi.OpBFI:
		t.bitfieldInsert(inst)
	case tgsi.OpNOP:
	case tgsi.OpEND:
		return true
	default:
		if t.flowInstruction(inst) || t.textureInstruction(inst) || t.memoryInstruction(inst) ||
			t.stageInstruction(inst) {
			return false
		}
		t.fail("opcode %s", info.Name)
	}
	return false
}

// compare emits a componentwise comparison, whose result is floats of 1.0
// or 0.0, or masks of ~0 or 0.
func (t *translator) compare(inst *tgsi.Instruction, op spirv.Op, b BaseType, floats bool) {
	cond := t.f.Op(op, t.tVec4B, uint32(t.src(inst, 0, b)), uint32(t.src(inst, 1, b)))
	if floats {
		t.dst(inst, 0, Float, t.f.Op(spirv.OpSelect, t.tVec4F, uint32(cond), uint32(t.constVec4F(1, 1, 1, 1)), uint32(t.constVec4F(0, 0, 0, 0))))
		return
	}
	ones := t.m.ConstantUint(^uint32(0))
	all := t.m.ConstantComposite(t.tVec4U, ones, ones, ones, ones)
	t.dst(inst, 0, Uint, t.f.Op(spirv.OpSelect, t.tVec4U, uint32(cond), uint32(all), uint32(t.zero4U)))
}

// dot emits DP2, DP3 or DP4: the dot product of the sources' first
// channels, replicated.
func (t *translator) dot(inst *tgsi.Instruction) {
	a, b := t.src(inst, 0, Float), t.src(inst, 1, Float)
	var n int
	switch inst.Opcode {
	case tgsi.OpDP2:
		n = 2
	case tgsi.OpDP3:
		n = 3
	default:
		n = 4
	}
	var d spirv.ID
	if n == 4 {
		d = t.f.Op(spirv.OpDot, t.tF32, uint32(a), uint32(b))
	} else {
		typ := t.m.TypeVector(t.tF32, uint32(n))
		sel := []uint32{0, 1, 2}[:n]
		as := t.f.Op(spirv.OpVectorShuffle, typ, append([]uint32{uint32(a), uint32(a)}, sel...)...)
		bs := t.f.Op(spirv.OpVectorShuffle, typ, append([]uint32{uint32(b), uint32(b)}, sel...)...)
		d = t.f.Op(spirv.OpDot, t.tF32, uint32(as), uint32(bs))
	}
	t.dst(inst, 0, Float, t.splat(t.tVec4F, d))
}

// derivative emits a derivative of a fragment shader, coarse or fine.
func (t *translator) derivative(inst *tgsi.Instruction, coarse, fine spirv.Op, isFine bool) {
	if t.s.Processor != tgsi.ProcessorFragment {
		t.fail("derivatives of %v", t.s.Processor)
	}
	op := coarse
	if isFine {
		t.m.Capability(spirv.CapabilityDerivativeControl)
		op = fine
	}
	t.dst(inst, 0, Float, t.f.Op(op, t.tVec4F, uint32(t.src(inst, 0, Float))))
}

// shift emits SHL, ISHR or USHR, of shifts of the low five bits of their
// counts, as TGSI's are.
func (t *translator) shift(inst *tgsi.Instruction) {
	b := Uint
	op := spirv.OpShiftLeftLogical
	switch inst.Opcode {
	case tgsi.OpISHR:
		b, op = Int, spirv.OpShiftRightArithmetic
	case tgsi.OpUSHR:
		op = spirv.OpShiftRightLogical
	}
	n31 := t.m.ConstantUint(31)
	mask := t.m.ConstantComposite(t.tVec4U, n31, n31, n31, n31)
	count := t.f.Op(spirv.OpBitwiseAnd, t.tVec4U, uint32(t.src(inst, 1, Uint)), uint32(mask))
	t.dst(inst, 0, b, t.f.Op(op, t.vecType(b), uint32(t.src(inst, 0, b)), uint32(count)))
}

// mulHigh emits IMUL_HI or UMUL_HI: the high 32 bits of 64-bit products.
func (t *translator) mulHigh(inst *tgsi.Instruction) {
	b, op := Uint, spirv.OpUMulExtended
	if inst.Opcode == tgsi.OpIMUL_HI {
		b, op = Int, spirv.OpSMulExtended
	}
	typ := t.vecType(b)
	pair := t.m.TypeStruct(typ, typ)
	r := t.f.Op(op, pair, uint32(t.src(inst, 0, b)), uint32(t.src(inst, 1, b)))
	t.dst(inst, 0, b, t.f.Op(spirv.OpCompositeExtract, typ, uint32(r), 1))
}

// bitfieldExtract emits IBFE or UBFE, of the channels' offsets and counts,
// which SPIR-V takes as scalars: per channel.
func (t *translator) bitfieldExtract(inst *tgsi.Instruction) {
	b, op, scalar := Uint, spirv.OpBitFieldUExtract, t.tU32
	if inst.Opcode == tgsi.OpIBFE {
		b, op, scalar = Int, spirv.OpBitFieldSExtract, t.tI32
	}
	v, off, n := t.src(inst, 0, b), t.src(inst, 1, Uint), t.src(inst, 2, Uint)
	var chans [4]uint32
	for c := uint32(0); c < 4; c++ {
		vc := t.f.Op(spirv.OpCompositeExtract, scalar, uint32(v), c)
		oc := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(off), c)
		nc := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(n), c)
		chans[c] = uint32(t.f.Op(op, scalar, uint32(vc), uint32(oc), uint32(nc)))
	}
	t.dst(inst, 0, b, t.f.Op(spirv.OpCompositeConstruct, t.vecType(b), chans[:]...))
}

// bitfieldInsert emits BFI.
func (t *translator) bitfieldInsert(inst *tgsi.Instruction) {
	base, ins, off, n := t.src(inst, 0, Uint), t.src(inst, 1, Uint), t.src(inst, 2, Uint), t.src(inst, 3, Uint)
	var chans [4]uint32
	for c := uint32(0); c < 4; c++ {
		bc := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(base), c)
		ic := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(ins), c)
		oc := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(off), c)
		nc := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(n), c)
		chans[c] = uint32(t.f.Op(spirv.OpBitFieldInsert, t.tU32, uint32(bc), uint32(ic), uint32(oc), uint32(nc)))
	}
	t.dst(inst, 0, Uint, t.f.Op(spirv.OpCompositeConstruct, t.tVec4U, chans[:]...))
}
