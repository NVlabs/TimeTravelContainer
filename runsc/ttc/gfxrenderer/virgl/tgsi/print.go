// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Print returns the shader's text, as Mesa's tgsi_dump_str prints it with
// TGSI_DUMP_FLOAT_AS_HEX, as virgl's guests send it.
func (s *Shader) Print() string {
	var b strings.Builder
	b.WriteString(s.Processor.String())
	b.WriteByte('\n')
	for _, ref := range s.Order {
		switch ref.Kind {
		case TokenProperty:
			s.printProperty(&b, &s.Properties[ref.Index])
		case TokenDeclaration:
			s.printDeclaration(&b, &s.Declarations[ref.Index])
		case TokenImmediate:
			printImmediate(&b, ref.Index, &s.Immediates[ref.Index])
		}
	}
	indent := 0
	for i := range s.Instructions {
		indent = printInstruction(&b, i, indent, &s.Instructions[i])
	}
	return b.String()
}

func (s *Shader) printProperty(b *strings.Builder, p *Property) {
	b.WriteString("PROPERTY ")
	b.WriteString(p.Name.String())
	b.WriteByte(' ')
	if names := propertyValueNames(p.Name); names != nil {
		b.WriteString(enumName(names, int(p.Value)))
	} else {
		b.WriteString(strconv.FormatInt(int64(int32(p.Value)), 10))
	}
	b.WriteByte('\n')
}

func printWriteMask(b *strings.Builder, mask uint8) {
	if mask == WriteMaskXYZW {
		return
	}
	b.WriteByte('.')
	for i := 0; i < 4; i++ {
		if mask&(1<<i) != 0 {
			b.WriteString(channelNames[i])
		}
	}
}

func (s *Shader) printDeclaration(b *strings.Builder, d *Declaration) {
	b.WriteString("DCL ")
	b.WriteString(d.File.String())
	patch := d.HasSemantic && (d.Semantic == SemanticPatch || d.Semantic == SemanticTessInner ||
		d.Semantic == SemanticTessOuter || d.Semantic == SemanticPrimID)
	// Geometry shaders' inputs, and tessellation shaders' but patches',
	// are two dimensional, as are tessellation control shaders' outputs.
	if d.File == FileInput && (s.Processor == ProcessorGeometry ||
		!patch && (s.Processor == ProcessorTessCtrl || s.Processor == ProcessorTessEval)) {
		b.WriteString("[]")
	}
	if d.File == FileOutput && !patch && s.Processor == ProcessorTessCtrl {
		b.WriteString("[]")
	}
	if d.Dimension {
		fmt.Fprintf(b, "[%d]", d.Index2D)
	}
	fmt.Fprintf(b, "[%d", d.First)
	if d.First != d.Last {
		fmt.Fprintf(b, "..%d", d.Last)
	}
	b.WriteByte(']')
	printWriteMask(b, d.UsageMask)
	if d.ArrayID != 0 {
		fmt.Fprintf(b, ", ARRAY(%d)", d.ArrayID)
	}
	if d.Local {
		b.WriteString(", LOCAL")
	}
	if d.HasSemantic {
		b.WriteString(", ")
		b.WriteString(d.Semantic.String())
		if d.SemanticIndex != 0 || d.Semantic == SemanticTexCoord || d.Semantic == SemanticGeneric {
			fmt.Fprintf(b, "[%d]", d.SemanticIndex)
		}
		if d.Streams != [4]uint8{} {
			fmt.Fprintf(b, ", STREAM(%d, %d, %d, %d)", d.Streams[0], d.Streams[1], d.Streams[2], d.Streams[3])
		}
	}
	switch d.File {
	case FileImage:
		b.WriteString(", ")
		b.WriteString(d.ImageTarget.String())
		b.WriteString(", ")
		if d.ImageFormat == "" {
			b.WriteString("PIPE_FORMAT_NONE")
		} else {
			b.WriteString(d.ImageFormat)
		}
		if d.ImageWritable {
			b.WriteString(", WR")
		}
		if d.ImageRaw {
			b.WriteString(", RAW")
		}
	case FileBuffer:
		if d.Atomic {
			b.WriteString(", ATOMIC")
		}
	case FileMemory:
		b.WriteString(", ")
		b.WriteString(enumName(memoryTypeNames, int(d.MemoryType)))
	case FileSamplerView:
		b.WriteString(", ")
		b.WriteString(d.ViewTarget.String())
		b.WriteString(", ")
		r := d.ReturnTypes
		if r[0] == r[1] && r[0] == r[2] && r[0] == r[3] {
			b.WriteString(r[0].String())
		} else {
			fmt.Fprintf(b, "%s, %s, %s, %s", r[0], r[1], r[2], r[3])
		}
	}
	if d.Interpolate {
		if s.Processor == ProcessorFragment && d.File == FileInput {
			b.WriteString(", ")
			b.WriteString(d.Interp.String())
		}
		if d.Location != LocationCenter {
			b.WriteString(", ")
			b.WriteString(d.Location.String())
		}
	}
	if d.Invariant {
		b.WriteString(", INVARIANT")
	}
	b.WriteByte('\n')
}

func printImmediate(b *strings.Builder, index int, imm *Immediate) {
	fmt.Fprintf(b, "IMM[%d] %s {", index, imm.Type)
	for i := 0; i < len(imm.Values); i++ {
		v := imm.Values[i]
		switch imm.Type {
		case ImmFloat64:
			d := math.Float64frombits(uint64(v) | uint64(imm.Values[i+1])<<32)
			fmt.Fprintf(b, "%10.8f", d)
			i++
		case ImmInt64:
			fmt.Fprintf(b, "%d", int64(uint64(v)|uint64(imm.Values[i+1])<<32))
			i++
		case ImmUint64:
			fmt.Fprintf(b, "%d", uint64(v)|uint64(imm.Values[i+1])<<32)
			i++
		case ImmFloat32:
			fmt.Fprintf(b, "0x%08x", v)
		case ImmUint32:
			fmt.Fprintf(b, "%d", v)
		case ImmInt32:
			fmt.Fprintf(b, "%d", int32(v))
		}
		if i < len(imm.Values)-1 {
			b.WriteString(", ")
		}
	}
	b.WriteString("}\n")
}

func printIndirect(b *strings.Builder, ind *Indirect, index int) {
	b.WriteByte('[')
	b.WriteString(ind.File.String())
	fmt.Fprintf(b, "[%d].%s", ind.Index, channelNames[ind.Swizzle&3])
	if index != 0 {
		if index > 0 {
			b.WriteByte('+')
		}
		b.WriteString(strconv.Itoa(index))
	}
	b.WriteByte(']')
	if ind.ArrayID != 0 {
		fmt.Fprintf(b, "(%d)", ind.ArrayID)
	}
}

func printRegister(b *strings.Builder, r *Register) {
	b.WriteString(r.File.String())
	if r.Dimension {
		if r.DimIndirect != nil {
			printIndirect(b, r.DimIndirect, r.DimIndex)
		} else {
			fmt.Fprintf(b, "[%d]", r.DimIndex)
		}
	}
	if r.Indirect != nil {
		printIndirect(b, r.Indirect, r.Index)
	} else {
		fmt.Fprintf(b, "[%d]", r.Index)
	}
}

// printInstruction prints the n-th instruction, indented by indent, and
// returns the next one's indentation.
func printInstruction(b *strings.Builder, n, indent int, inst *Instruction) int {
	info, _ := inst.Opcode.Info()
	fmt.Fprintf(b, "%3d: ", n)
	if info.PreDedent {
		indent--
	}
	for i := 0; i < indent; i++ {
		b.WriteString("  ")
	}
	if info.PostIndent {
		indent++
	}
	b.WriteString(info.Name)
	if inst.Saturate {
		b.WriteString("_SAT")
	}
	if inst.Precise {
		b.WriteString("_PRECISE")
	}
	first := true
	sep := func() {
		if !first {
			b.WriteByte(',')
		}
		b.WriteByte(' ')
		first = false
	}
	for i := range inst.Dst {
		sep()
		printRegister(b, &inst.Dst[i].Register)
		printWriteMask(b, inst.Dst[i].WriteMask)
	}
	for i := range inst.Src {
		src := &inst.Src[i]
		sep()
		if src.Negate {
			b.WriteByte('-')
		}
		if src.Absolute {
			b.WriteByte('|')
		}
		printRegister(b, &src.Register)
		if src.Swizzle != identitySwizzle {
			b.WriteByte('.')
			for _, c := range src.Swizzle {
				b.WriteString(channelNames[c&3])
			}
		}
		if src.Absolute {
			b.WriteByte('|')
		}
	}
	isSample := inst.Opcode >= OpSAMPLE && inst.Opcode <= OpGATHER4
	if info.IsTex && !isSample {
		b.WriteString(", ")
		b.WriteString(inst.Texture.String())
	}
	for _, off := range inst.TexOffset {
		fmt.Fprintf(b, ", %s[%d].%s%s%s", off.File, off.Index,
			channelNames[off.Swizzle[0]&3], channelNames[off.Swizzle[1]&3], channelNames[off.Swizzle[2]&3])
	}
	for q := 0; q < len(memoryQualifierNames); q++ {
		if inst.Memory&(1<<q) != 0 {
			b.WriteString(", ")
			b.WriteString(memoryQualifierNames[q])
		}
	}
	if inst.MemoryTexture != TextureBuffer {
		b.WriteString(", ")
		b.WriteString(inst.MemoryTexture.String())
	}
	if inst.MemoryFormat != "" {
		b.WriteString(", ")
		b.WriteString(inst.MemoryFormat)
	}
	if inst.HasLabel {
		switch inst.Opcode {
		case OpIF, OpUIF, OpELSE, OpBGNLOOP, OpENDLOOP, OpCAL, OpBGNSUB:
			fmt.Fprintf(b, " :%d", inst.Label)
		}
	}
	b.WriteByte('\n')
	return indent
}
