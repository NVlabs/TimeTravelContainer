// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// Shaders as Mesa's tgsi_dump_str prints them with TGSI_DUMP_FLOAT_AS_HEX,
// which Print must print back alike.
var printed = map[string]string{
	"vertex": `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[9]
DCL CONST[0][0..3]
DCL TEMP[0..1], LOCAL
IMM[0] UINT32 {0, 1065353216, 0, 0}
IMM[1] FLT32 {0x3f000000, 0x00000000, 0xbf800000, 0x3f800000}
  0: MUL TEMP[0], CONST[0][0], IN[0].xxxx
  1: MAD TEMP[0], CONST[0][1], IN[0].yyyy, TEMP[0]
  2: MAD TEMP[0], CONST[0][2], IN[0].zzzz, TEMP[0]
  3: MAD OUT[0], CONST[0][3], IN[0].wwww, TEMP[0]
  4: MOV_SAT OUT[1], -|IN[1].yxzw|
  5: END
`,
	"fragment": `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL IN[0], GENERIC[9], PERSPECTIVE
DCL IN[1], COLOR, COLOR, CENTROID
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL CONST[1][0..1]
DCL TEMP[0..2], LOCAL
DCL ADDR[0]
IMM[0] FLT32 {0x00000000, 0x3f800000, 0x00000000, 0x00000000}
IMM[1] INT32 {-1, 0, 2, 3}
  0: TEX TEMP[0], IN[0].xyyy, SAMP[0], 2D
  1: FSLT TEMP[1].x, TEMP[0].wwww, IMM[0].xxxx
  2: UIF TEMP[1].xxxx :4
  3:   KILL
  4: ENDIF
  5: UARL ADDR[0].x, IMM[1].zzzz
  6: MOV TEMP[2], CONST[1][ADDR[0].x-1]
  7: TXL TEMP[0], IN[0], SAMP[0], 2D, IMM[1].xyz
  8: MUL OUT[0], TEMP[0], IN[1]
  9: END
`,
	"geometry": `GEOM
PROPERTY GS_INPUT_PRIMITIVE TRIANGLES
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0], STREAM(1, 1, 1, 1)
DCL TEMP[0], ARRAY(1), LOCAL
DCL ADDR[0]
IMM[0] INT32 {0, 1, 2, 0}
  0: BGNLOOP :6
  1:   UARL ADDR[0].x, IMM[0].xxxx
  2:   MOV OUT[0], IN[ADDR[0].x][0]
  3:   MOV OUT[1], IN[2][1]
  4:   EMIT IMM[0].xxxx
  5:   BRK
  6: ENDLOOP :0
  7: END
`,
	"compute": `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 64
PROPERTY CS_FIXED_BLOCK_HEIGHT 1
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL BUFFER[0]
DCL BUFFER[1], ATOMIC
DCL IMAGE[0], 2D, PIPE_FORMAT_R8G8B8A8_UNORM, WR
DCL MEMORY[0], SHARED
DCL TEMP[0..1], LOCAL
IMM[0] UINT32 {4, 0, 0, 0}
IMM[1] FLT64 {1.00000000, 0.50000000}
  0: UMUL TEMP[0].x, SV[0].xxxx, IMM[0].xxxx
  1: LOAD TEMP[1], BUFFER[0], TEMP[0].xxxx
  2: STORE IMAGE[0], SV[0].xyyy, TEMP[1], COHERENT, 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  3: ATOMUADD TEMP[1].x, BUFFER[1], IMM[0].yyyy, IMM[0].xxxx
  4: DADD TEMP[0].xy, IMM[1], IMM[1]
  5: BARRIER
  6: END
`,
	"tessellation": `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 3
DCL IN[][0], POSITION
DCL OUT[][0], POSITION
DCL OUT[1], TESSOUTER
DCL OUT[2], PATCH
DCL SV[0], INVOCATIONID
  0: MOV OUT[SV[0].x][0], IN[SV[0].x][0]
  1: END
`,
	"sample": `FRAG
DCL IN[0], GENERIC[0], LINEAR, SAMPLE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D_ARRAY, UINT, UINT, SINT, FLOAT
DCL TEMP[0]
IMM[0] INT32 {1, -1, 0, 0}
  0: SAMPLE TEMP[0], IN[0], SVIEW[0], SAMP[0], IMM[0].xyx
  1: TXF TEMP[0], IN[0], SAMP[0], 2D_ARRAY
  2: MOV OUT[0], TEMP[0]
  3: END
`,
}

func TestPrintRoundTrip(t *testing.T) {
	for name, text := range printed {
		t.Run(name, func(t *testing.T) {
			s, err := Parse(text)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := s.Print(); got != text {
				t.Errorf("Print:\n%s\nwant:\n%s", got, text)
			}
		})
	}
}

func TestParseFields(t *testing.T) {
	s, err := Parse(printed["fragment"])
	if err != nil {
		t.Fatal(err)
	}
	if s.Processor != ProcessorFragment || len(s.Declarations) != 8 || len(s.Immediates) != 2 || len(s.Instructions) != 10 {
		t.Fatalf("shader %v: %d declarations, %d immediates, %d instructions", s.Processor,
			len(s.Declarations), len(s.Immediates), len(s.Instructions))
	}
	in1 := s.Declarations[1]
	if !in1.HasSemantic || in1.Semantic != SemanticColor || !in1.Interpolate || in1.Interp != InterpolateColor || in1.Location != LocationCentroid {
		t.Errorf("IN[1]: %+v", in1)
	}
	if c := s.Declarations[5]; !c.Dimension || c.Index2D != 1 || c.First != 0 || c.Last != 1 {
		t.Errorf("CONST[1][0..1]: %+v", c)
	}
	if v := s.Immediates[0].Values[1]; math.Float32frombits(v) != 1 {
		t.Errorf("IMM[0].y = %#x, want 1.0's bits", v)
	}
	if v := int32(s.Immediates[1].Values[0]); v != -1 {
		t.Errorf("IMM[1].x = %d, want -1", v)
	}
	uif := s.Instructions[2]
	if uif.Opcode != OpUIF || !uif.HasLabel || uif.Label != 4 {
		t.Errorf("UIF: %+v", uif)
	}
	mov := s.Instructions[6].Src[0]
	if mov.Indirect == nil || mov.Indirect.File != FileAddress || mov.Index != -1 || !mov.Dimension || mov.DimIndex != 1 {
		t.Errorf("CONST[1][ADDR[0].x-1]: %+v %+v", mov, mov.Indirect)
	}
	txl := s.Instructions[7]
	if txl.Texture != Texture2D || len(txl.TexOffset) != 1 || txl.TexOffset[0].File != FileImmediate {
		t.Errorf("TXL: %+v", txl)
	}
	vs, err := Parse(printed["vertex"])
	if err != nil {
		t.Fatal(err)
	}
	sat := vs.Instructions[4]
	if !sat.Saturate || !sat.Src[0].Negate || !sat.Src[0].Absolute || sat.Src[0].Swizzle != [4]uint8{Y, X, Z, W} {
		t.Errorf("MOV_SAT: %+v", sat)
	}
}

// Mesa's text, which tgsi_text_translate also takes: decimal floats, short
// swizzles, and no instruction numbers.
func TestParseLenient(t *testing.T) {
	s, err := Parse(`FRAG
DCL OUT[0], COLOR
IMM[0] FLT32 {    0.0000,     1.0000,     0.5000,     1.0000}
MOV OUT[0], IMM[0].x
mov out[0].xy, imm[0].zw
END
`)
	if err != nil {
		t.Fatal(err)
	}
	if v := math.Float32frombits(s.Immediates[0].Values[2]); v != 0.5 {
		t.Errorf("IMM[0].z = %v", v)
	}
	if sw := s.Instructions[0].Src[0].Swizzle; sw != [4]uint8{X, X, X, X} {
		t.Errorf(".x = %v, want xxxx", sw)
	}
	if mask := s.Instructions[1].Dst[0].WriteMask; mask != 3 {
		t.Errorf(".xy = %#x", mask)
	}
	if sw := s.Instructions[1].Src[0].Swizzle; sw != [4]uint8{Z, W, W, W} {
		t.Errorf(".zw = %v, want zwww", sw)
	}
}

func TestParseErrors(t *testing.T) {
	for _, text := range []string{
		"",
		"PIXEL\nEND\n",
		"FRAG\nFOO OUT[0]\n",
		"FRAG\nMOV OUT[0]\n",
		"FRAG\nMOV OUT[0], TEMP[\n",
		"FRAG\nMOV OUT[0], TEMP[0].q\n",
		"FRAG\nDCL TEMP[3..1]\n",
		"FRAG\nDCL TEMP[0], NOWHERE\n",
		"FRAG\nIMM[1] FLT32 {0, 0, 0, 0}\n",
		"FRAG\nIMM[0] FLT32 {0x1, 0, 0}\n",
		"FRAG\nIMM[0] UINT32 {4294967296, 0, 0, 0}\n",
		"FRAG\nTEX TEMP[0], IN[0], SAMP[0]\n",
		"FRAG\nMOV TEMP[2000000], IN[0]\n",
		"FRAG\nUIF TEMP[0].xxxx :\n",
	} {
		_, err := Parse(text)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("Parse(%q) = %v, want a SyntaxError", text, err)
		}
	}
}

// FuzzParse checks that Parse takes any text without panicking, and that a
// shader it parses prints as text that parses to the same shader.
func FuzzParse(f *testing.F) {
	for _, text := range printed {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		s, err := Parse(text)
		if err != nil {
			return
		}
		again, err := Parse(s.Print())
		if err != nil {
			t.Fatalf("Parse(Print()) of %q: %v\n%s", text, err, s.Print())
		}
		if a, b := s.Print(), again.Print(); a != b {
			t.Fatalf("Print differs after a round trip:\n%s\n%s", a, b)
		}
	})
}

func TestNoTrailingText(t *testing.T) {
	if _, err := Parse("FRAG\nEND\n" + strings.Repeat(" ", 3)); err != nil {
		t.Errorf("trailing spaces: %v", err)
	}
	if _, err := Parse("FRAG\nEND\n\x00garbage"); err != nil {
		t.Errorf("a NUL ends the text: %v", err)
	}
}
