// SPDX-FileCopyrightText: Copyright 2008 VMware, Inc.
// SPDX-License-Identifier: MIT

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

// Type is the type that an instruction takes its operand's bits as, of
// which its source modifiers are: a float's absolute value and negation,
// or an integer's negation.
type Type uint8

// The types, as Mesa's tgsi_opcode_type has them.
const (
	TypeUntyped Type = iota
	TypeFloat
	TypeUnsigned
	TypeSigned
	TypeDouble
	TypeUnsigned64
	TypeSigned64
)

// IsFloat reports whether operands of type t, of whose untyped ones Mesa's
// tgsi_to_nir takes floats, are floats.
func (t Type) IsFloat() bool {
	return t == TypeUntyped || t == TypeFloat || t == TypeDouble
}

// Type returns the type of op's destination, as Mesa's
// tgsi_opcode_infer_type infers it.
func (op Opcode) Type() Type {
	switch op {
	case OpMOV, OpUCMP:
		return TypeUntyped
	case OpNOT, OpSHL, OpAND, OpOR, OpXOR, OpTXQ, OpTXQS, OpF2U, OpUDIV, OpUMAD, OpUMAX, OpUMIN,
		OpUMOD, OpUMUL, OpUSEQ, OpUSGE, OpUSHR, OpUSLT, OpUSNE, OpSVIEWINFO, OpUMUL_HI, OpUBFE,
		OpBFI, OpBREV, OpIMG2HND, OpSAMP2HND:
		return TypeUnsigned
	case OpARL, OpARR, OpMOD, OpF2I, OpFSEQ, OpFSGE, OpFSLT, OpFSNE, OpIDIV, OpIMAX, OpIMIN,
		OpINEG, OpISGE, OpISHR, OpISLT, OpUADD, OpUARL, OpIABS, OpISSG, OpIMUL_HI, OpIBFE, OpIMSB,
		OpDSEQ, OpDSGE, OpDSLT, OpDSNE, OpU64SEQ, OpU64SNE, OpU64SLT, OpU64SGE, OpI64SLT, OpI64SGE,
		OpLSB, OpPOPC, OpUMSB:
		return TypeSigned
	case OpDADD, OpDABS, OpDFMA, OpDNEG, OpDMUL, OpDMAX, OpDDIV, OpDMIN, OpDRCP, OpDSQRT, OpDMAD,
		OpDLDEXP, OpDFRAC, OpDRSQ, OpDTRUNC, OpDCEIL, OpDFLR, OpDROUND, OpDSSG, OpF2D, OpI2D, OpU2D,
		OpU642D, OpI642D:
		return TypeDouble
	case OpU64MAX, OpU64MIN, OpU64ADD, OpU64MUL, OpU64DIV, OpU64MOD, OpU64SHL, OpU64SHR, OpF2U64,
		OpD2U64:
		return TypeUnsigned64
	case OpI64MAX, OpI64MIN, OpI64ABS, OpI64SSG, OpI64NEG, OpI64SHR, OpI64DIV, OpI64MOD, OpF2I64,
		OpU2I64, OpI2I64, OpD2I64:
		return TypeSigned64
	}
	return TypeFloat
}

// SrcType returns the type of op's source i, as Mesa's
// tgsi_opcode_infer_src_type infers it.
func (op Opcode) SrcType(i int) Type {
	switch {
	case i == 1 && (op == OpDLDEXP || op == OpLDEXP):
		return TypeSigned
	case i == 1 && op == OpLOAD, i == 0 && op == OpSTORE:
		return TypeUnsigned
	case i == 1 && (op >= OpATOMUADD && op <= OpATOMIMAX || op == OpATOMINC_WRAP || op == OpATOMDEC_WRAP):
		return TypeUnsigned
	}
	switch op {
	case OpUIF, OpTXF, OpU2F, OpU2D, OpUADD, OpSWITCH, OpCASE, OpSAMPLE_I, OpSAMPLE_I_MS,
		OpUMUL_HI, OpUP2H, OpU2I64, OpMEMBAR, OpUMSB:
		return TypeUnsigned
	case OpIMUL_HI, OpI2F, OpI2D, OpI2I64:
		return TypeSigned
	case OpARL, OpARR, OpF2D, OpF2I, OpF2U, OpFSEQ, OpFSGE, OpFSLT, OpFSNE, OpUCMP, OpF2U64,
		OpF2I64:
		return TypeFloat
	case OpD2F, OpD2U, OpD2I, OpDSEQ, OpDSGE, OpDSLT, OpDSNE, OpD2U64, OpD2I64:
		return TypeDouble
	case OpU64SEQ, OpU64SNE, OpU64SLT, OpU64SGE, OpU642F, OpU642D:
		return TypeUnsigned64
	case OpI64SLT, OpI64SGE, OpI642F, OpI642D:
		return TypeSigned64
	}
	return op.Type()
}
