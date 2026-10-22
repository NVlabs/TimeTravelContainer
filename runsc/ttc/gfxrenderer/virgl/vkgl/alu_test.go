// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"testing"
)

// aluCase is an instruction, of TEMP[0], TEMP[1] and TEMP[2], of srcs'
// bits, whose result, TEMP[3]'s bits, of 0 before, is want, or within
// tol of want's floats if tol is not 0.
type aluCase struct {
	inst string
	srcs [3][4]uint32
	want [4]uint32
	tol  float64
}

func fv(x, y, z, w float32) [4]uint32 {
	return [4]uint32{math.Float32bits(x), math.Float32bits(y), math.Float32bits(z), math.Float32bits(w)}
}

func iv(x, y, z, w int32) [4]uint32 {
	return [4]uint32{uint32(x), uint32(y), uint32(z), uint32(w)}
}

func uv(x, y, z, w uint32) [4]uint32 {
	return [4]uint32{x, y, z, w}
}

func f1(x float32) [4]uint32 {
	return fv(x, x, x, x)
}

const allOnes = 0xffffffff

// negZero is -0, which Go's constants, of exact values, lack.
var negZero = float32(math.Copysign(0, -1))

// half returns the float16 of f, rounded to the nearest even.
func half(f float32) uint32 {
	b := math.Float32bits(f)
	sign := (b >> 16) & 0x8000
	exp := int((b>>23)&0xff) - 127 + 15
	mant := b & 0x7fffff
	switch {
	case exp >= 31:
		return sign | 0x7c00
	case exp <= 0:
		return sign
	}
	h := uint32(exp)<<10 | mant>>13
	rem := mant & 0x1fff
	if rem > 0x1000 || rem == 0x1000 && h&1 != 0 {
		h++
	}
	return sign | h
}

// aluCases are instructions and their results, as TGSI defines them.
func aluCases() []aluCase {
	a := fv(1.5, -2.25, 0.5, 0)
	b := fv(2, 4, -8, 3)
	c := fv(0.25, 0.5, 0.75, 1)
	ia := iv(7, -9, 1<<30, -1)
	ib := iv(2, 3, -4, 33)
	ic := iv(5, 0, 31, 8)
	return []aluCase{
		{"ADD TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(3.5, 1.75, -7.5, 3), 0},
		{"MUL TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(3, -9, -4, 0), 0},
		{"MAD TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{a, b, c}, fv(3.25, -8.5, -3.25, 1), 0},
		{"LRP TEMP[3], TEMP[2], TEMP[0], TEMP[1]", [3][4]uint32{a, b, c}, fv(0.25*1.5+0.75*2, 0.5*-2.25+0.5*4, 0.75*0.5+0.25*-8, 0), 1e-6},
		{"DP2 TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, f1(3 - 9), 0},
		{"DP3 TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, f1(3 - 9 - 4), 0},
		{"DP4 TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, f1(3 - 9 - 4), 0},
		{"MIN TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(1.5, -2.25, -8, 0), 0},
		{"MAX TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(2, 4, 0.5, 3), 0},
		{"SLT TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(1, 1, 0, 1), 0},
		{"SGE TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, fv(0, 0, 1, 0), 0},
		{"SEQ TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{a}, fv(1, 0, 1, 1), 0},
		{"SNE TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{a}, fv(0, 1, 0, 0), 0},
		{"FSLT TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, uv(allOnes, allOnes, 0, allOnes), 0},
		{"FSGE TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, b}, uv(0, 0, allOnes, 0), 0},
		{"FSEQ TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{a}, uv(allOnes, 0, allOnes, allOnes), 0},
		{"FSNE TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{a}, uv(0, allOnes, 0, 0), 0},
		{"CMP TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{a, b, c}, fv(0.25, 4, 0.75, 1), 0},
		{"CMP TEMP[3], -|TEMP[0]|, -TEMP[1], TEMP[2]", [3][4]uint32{a, b, c}, fv(-2, -4, 8, 1), 0},
		{"UCMP TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{uv(1, 0, allOnes, 0), b, c}, fv(2, 0.5, -8, 1), 0},
		{"FRC TEMP[3], TEMP[0]", [3][4]uint32{a}, fv(0.5, 0.75, 0.5, 0), 0},
		{"FLR TEMP[3], TEMP[0]", [3][4]uint32{a}, fv(1, -3, 0, 0), 0},
		{"CEIL TEMP[3], TEMP[0]", [3][4]uint32{a}, fv(2, -2, 1, 0), 0},
		{"TRUNC TEMP[3], TEMP[0]", [3][4]uint32{a}, fv(1, -2, 0, 0), 0},
		{"ROUND TEMP[3], TEMP[0]", [3][4]uint32{fv(1.5, 2.5, -0.5, 0.6)}, fv(2, 2, negZero, 1), 0},
		{"SSG TEMP[3], TEMP[0]", [3][4]uint32{a}, fv(1, -1, 1, 0), 0},
		{"DIV TEMP[3], TEMP[1], TEMP[0]", [3][4]uint32{a, b}, fv(2/1.5, 4/-2.25, -16, float32(math.Inf(1))), 1e-6},
		{"RCP TEMP[3], TEMP[1].yyyy", [3][4]uint32{a, b}, f1(0.25), 1e-6},
		{"RSQ TEMP[3], TEMP[1].yyyy", [3][4]uint32{a, b}, f1(0.5), 1e-5},
		{"SQRT TEMP[3], TEMP[1].yyyy", [3][4]uint32{a, b}, f1(2), 1e-5},
		{"EX2 TEMP[3], TEMP[0].xxxx", [3][4]uint32{a}, f1(float32(math.Exp2(1.5))), 1e-5},
		{"LG2 TEMP[3], TEMP[1].yyyy", [3][4]uint32{a, b}, f1(2), 1e-5},
		{"POW TEMP[3], TEMP[1].yyyy, TEMP[0].xxxx", [3][4]uint32{a, b}, f1(8), 1e-4},
		{"SIN TEMP[3], TEMP[2].yyyy", [3][4]uint32{a, b, c}, f1(float32(math.Sin(0.5))), 1e-3},
		{"COS TEMP[3], TEMP[2].yyyy", [3][4]uint32{a, b, c}, f1(float32(math.Cos(0.5))), 1e-3},
		{"LDEXP TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{a, iv(2, -1, 3, 0)}, fv(6, -1.125, 4, 0), 0},
		{"MOV_SAT TEMP[3], TEMP[1]", [3][4]uint32{a, fv(2, 0.5, -1, 0)}, fv(1, 0.5, 0, 0), 0},
		{"ADD_SAT TEMP[3], TEMP[0], TEMP[2]", [3][4]uint32{a, b, c}, fv(1, 0, 1, 1), 0},
		{"MOV TEMP[3].xz, -TEMP[0].wzyx", [3][4]uint32{a}, [4]uint32{math.Float32bits(negZero), 0, math.Float32bits(2.25), 0}, 0},
		{"MOV TEMP[3], |TEMP[0]|", [3][4]uint32{a}, fv(1.5, 2.25, 0.5, 0), 0},
		{"I2F TEMP[3], TEMP[0]", [3][4]uint32{ia}, fv(7, -9, 1<<30, -1), 0},
		{"U2F TEMP[3], TEMP[0]", [3][4]uint32{ia}, fv(7, float32(uint32(0xfffffff7)), 1<<30, float32(uint32(allOnes))), 1e-7},
		{"F2I TEMP[3], TEMP[0]", [3][4]uint32{fv(1.9, -2.9, 1e3, -0.5)}, iv(1, -2, 1000, 0), 0},
		{"F2U TEMP[3], TEMP[0]", [3][4]uint32{fv(1.9, 2.9, 1e3, 0.5)}, uv(1, 2, 1000, 0), 0},
		{"UADD TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(9, -6, 1<<30-4, 32), 0},
		{"UADD TEMP[3], TEMP[0], -TEMP[1]", [3][4]uint32{ia, ib}, iv(5, -12, 1<<30+4, -34), 0},
		{"UMUL TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(14, -27, 0, -33), 0},
		{"UMAD TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{ia, ib, ic}, iv(19, -27, 31, -25), 0},
		{"IMUL_HI TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(0, -1, -1, -1), 0},
		{"UMUL_HI TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(0, 2, 1<<30-1, 32), 0},
		{"IDIV TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(3, -3, -(1 << 28), 0), 0},
		{"UDIV TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(3, 0xfffffff7/3, 0, allOnes/33), 0},
		{"MOD TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(1, 0, 0, -1), 0},
		{"UMOD TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(1, 0xfffffff7%3, 1<<30, allOnes%33), 0},
		{"AND TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(2, 3&-9, 1<<30, 33), 0},
		{"OR TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(7, -9|3, -4, -1), 0},
		{"XOR TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(5, -9^3, (1<<30)^-4, -1^33), 0},
		{"NOT TEMP[3], TEMP[0]", [3][4]uint32{ia}, iv(^7, ^-9, ^(1 << 30), 0), 0},
		{"SHL TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(28, -72, 0, -2), 0},
		{"ISHR TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(1, -2, 1<<2, -1), 0},
		{"USHR TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(1, 0xfffffff7>>3, 1<<2, allOnes>>1), 0},
		{"IMIN TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(2, -9, -4, -1), 0},
		{"IMAX TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(7, 3, 1<<30, 33), 0},
		{"UMIN TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(2, 3, 1<<30, 33), 0},
		{"UMAX TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, iv(7, -9, -4, -1), 0},
		{"ISLT TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(0, allOnes, 0, allOnes), 0},
		{"ISGE TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(allOnes, 0, allOnes, 0), 0},
		{"USLT TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(0, 0, allOnes, 0), 0},
		{"USGE TEMP[3], TEMP[0], TEMP[1]", [3][4]uint32{ia, ib}, uv(allOnes, allOnes, 0, allOnes), 0},
		{"USEQ TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{ia}, uv(allOnes, 0, allOnes, allOnes), 0},
		{"USNE TEMP[3], TEMP[0], TEMP[0].xxzw", [3][4]uint32{ia}, uv(0, allOnes, 0, 0), 0},
		{"INEG TEMP[3], TEMP[0]", [3][4]uint32{ia}, iv(-7, 9, -(1 << 30), 1), 0},
		{"IABS TEMP[3], TEMP[0]", [3][4]uint32{ia}, iv(7, 9, 1<<30, 1), 0},
		{"ISSG TEMP[3], TEMP[0]", [3][4]uint32{iv(7, -9, 0, -1)}, iv(1, -1, 0, -1), 0},
		{"IBFE TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{iv(0x7f0, -1, 0x12345678, 0x0f0), iv(4, 3, 16, 4), iv(7, 4, 8, 4)}, iv(-1, -1, 0x34, -1), 0},
		{"UBFE TEMP[3], TEMP[0], TEMP[1], TEMP[2]", [3][4]uint32{iv(0x7f0, -1, 0x12345678, 0x0f0), iv(4, 3, 16, 4), iv(7, 4, 8, 4)}, iv(0x7f, 0xf, 0x34, 0xf), 0},
		{"BFI TEMP[3], TEMP[0], TEMP[1], TEMP[2], TEMP[2].wwww", [3][4]uint32{uv(0, allOnes, 0x12345678, 0), uv(0xff, 0, 0xabcd, 1), uv(4, 8, 16, 4)}, uv(0xf0, 0xfffff0ff, 0x123d5678, 0x10), 0},
		{"BREV TEMP[3], TEMP[0]", [3][4]uint32{uv(1, 0x80000000, 0x12345678, 0)}, uv(0x80000000, 1, bits.Reverse32(0x12345678), 0), 0},
		{"POPC TEMP[3], TEMP[0]", [3][4]uint32{uv(0, allOnes, 0x12345678, 1)}, uv(0, 32, 13, 1), 0},
		{"LSB TEMP[3], TEMP[0]", [3][4]uint32{uv(0, 8, 0x12345678, allOnes)}, iv(-1, 3, 3, 0), 0},
		{"IMSB TEMP[3], TEMP[0]", [3][4]uint32{iv(0, 8, -1, -9)}, iv(-1, 3, -1, 3), 0},
		{"UMSB TEMP[3], TEMP[0]", [3][4]uint32{uv(0, 8, allOnes, 0x12345678)}, iv(-1, 3, 31, 28), 0},
		{"PK2H TEMP[3], TEMP[0]", [3][4]uint32{fv(1, -2, 7, 7)}, [4]uint32{half(1) | half(-2)<<16, half(1) | half(-2)<<16, half(1) | half(-2)<<16, half(1) | half(-2)<<16}, 0},
		{"UP2H TEMP[3], TEMP[0]", [3][4]uint32{uv(half(1)|half(-2)<<16, 0, 0, 0)}, fv(1, -2, 1, -2), 0},
	}
}

// aluFS is a fragment shader that executes inst, of its sources of its
// constants, to its color, of 32-bit unsigned integers.
const aluFS = `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0..3], LOCAL
  0: MOV TEMP[0], CONST[0]
  1: MOV TEMP[1], CONST[1]
  2: MOV TEMP[2], CONST[2]
  3: %s
  4: MOV OUT[0], TEMP[3]
  5: END
`

// TestALU checks each instruction's result, of operands that the shader
// reads of its constants, for drivers to compute rather than to fold.
func TestALU(t *testing.T) {
	d := newTestDevice(t)
	full := [][8]float32{{-1, -1, 0, 1}, {3, -1, 0, 1}, {-1, 3, 0, 1}}
	for _, c := range aluCases() {
		var constants []uint32
		for _, s := range c.srcs {
			constants = append(constants, s[:]...)
		}
		px := render(t, d, passVS, fmt.Sprintf(aluFS, c.inst), full, 1, targetRGBA32U, constants)
		var got [4]uint32
		for i := range got {
			got[i] = binary.LittleEndian.Uint32(px[4*i:])
		}
		if c.tol == 0 {
			if got != c.want {
				t.Errorf("%s: got %#x, want %#x", c.inst, got, c.want)
			}
			continue
		}
		for i := range got {
			g, w := float64(math.Float32frombits(got[i])), float64(math.Float32frombits(c.want[i]))
			if math.IsInf(w, 0) {
				if g != w {
					t.Errorf("%s: channel %d is %v, want %v", c.inst, i, g, w)
				}
				continue
			}
			if math.Abs(g-w) > c.tol*math.Max(1, math.Abs(w)) {
				t.Errorf("%s: channel %d is %v, want %v", c.inst, i, g, w)
			}
		}
	}
}

// programCase is a fragment shader's program, of the constants of srcs,
// whose color is want, of 32-bit unsigned integers.
type programCase struct {
	name string
	text string
	srcs [3][4]uint32
	want [4]uint32
}

// TestPrograms checks programs of control flow, of arrays indexed
// indirectly, and of kills.
func TestPrograms(t *testing.T) {
	d := newTestDevice(t)
	full := [][8]float32{{-1, -1, 0, 1}, {3, -1, 0, 1}, {-1, 3, 0, 1}}
	for _, c := range []programCase{{
		// The sum of 0..n-1, of a loop that breaks, and its count.
		name: "loop",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0..3], LOCAL
IMM[0] UINT32 {0, 1, 0, 0}
  0: MOV TEMP[0], IMM[0].xxxx
  1: MOV TEMP[1], IMM[0].xxxx
  2: BGNLOOP :8
  3:   USGE TEMP[2].x, TEMP[0].xxxx, CONST[0].xxxx
  4:   UIF TEMP[2].xxxx :6
  5:     BRK
  6:   ENDIF
  7:   UADD TEMP[1].x, TEMP[1].xxxx, TEMP[0].xxxx
  8:   UADD TEMP[0].x, TEMP[0].xxxx, IMM[0].yyyy
  9: ENDLOOP :2
 10: MOV TEMP[1].y, TEMP[0].xxxx
 11: MOV TEMP[1].zw, IMM[0].yyyy
 12: MOV OUT[0], TEMP[1]
 13: END
`,
		srcs: [3][4]uint32{uv(10, 0, 0, 0)},
		want: uv(45, 10, 1, 1),
	}, {
		// A loop that continues past the odd numbers, of a nested IF's
		// ELSE.
		name: "continue-else",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0..3], LOCAL
IMM[0] UINT32 {0, 1, 2, 100}
  0: MOV TEMP[0], IMM[0].xxxx
  1: MOV TEMP[1], IMM[0].xxxx
  2: BGNLOOP :14
  3:   UADD TEMP[0].x, TEMP[0].xxxx, IMM[0].yyyy
  4:   USLT TEMP[2].x, CONST[0].xxxx, TEMP[0].xxxx
  5:   UIF TEMP[2].xxxx :7
  6:     BRK
  7:   ENDIF
  8:   AND TEMP[2].x, TEMP[0].xxxx, IMM[0].yyyy
  9:   UIF TEMP[2].xxxx :12
 10:     UADD TEMP[1].y, TEMP[1].yyyy, IMM[0].yyyy
 11:     CONT
 12:   ELSE :14
 13:     UADD TEMP[1].x, TEMP[1].xxxx, TEMP[0].xxxx
 14:   ENDIF
 15: ENDLOOP :2
 16: MOV TEMP[1].zw, IMM[0].wwww
 17: MOV OUT[0], TEMP[1]
 18: END
`,
		srcs: [3][4]uint32{uv(10, 0, 0, 0)},
		want: uv(2+4+6+8+10, 5, 100, 100),
	}, {
		// An array of temporaries, written and read of indexes of an
		// address register, clamped.
		name: "temp-array",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0..3], ARRAY(1), LOCAL
DCL TEMP[4..5], LOCAL
DCL ADDR[0]
IMM[0] UINT32 {0, 1, 4, 7}
  0: MOV TEMP[4].x, IMM[0].xxxx
  1: BGNLOOP :7
  2:   USGE TEMP[5].x, TEMP[4].xxxx, IMM[0].zzzz
  3:   UIF TEMP[5].xxxx :5
  4:     BRK
  5:   ENDIF
  6:   UARL ADDR[0].x, TEMP[4].xxxx
  7:   UMUL TEMP[ADDR[0].x](1), TEMP[4].xxxx, IMM[0].wwww
  8:   UADD TEMP[4].x, TEMP[4].xxxx, IMM[0].yyyy
  9: ENDLOOP :1
 10: UARL ADDR[0].x, CONST[0].xxxx
 11: MOV TEMP[5].x, TEMP[ADDR[0].x](1).xxxx
 12: MOV TEMP[5].y, TEMP[ADDR[0].x+1](1).xxxx
 13: MOV TEMP[5].z, TEMP[2].xxxx
 14: UARL ADDR[0].x, CONST[0].yyyy
 15: MOV TEMP[5].w, TEMP[ADDR[0].x](1).xxxx
 16: MOV OUT[0], TEMP[5]
 17: END
`,
		srcs: [3][4]uint32{uv(1, 100, 0, 0)},
		want: uv(7, 14, 14, 21),
	}, {
		// A constant buffer's vec4s, of an index of the address register,
		// clamped to the buffer.
		name: "const-indirect",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0], LOCAL
DCL ADDR[0]
  0: UARL ADDR[0].x, CONST[0].wwww
  1: MOV TEMP[0], CONST[ADDR[0].x]
  2: MOV TEMP[0].w, CONST[ADDR[0].x+1].xxxx
  3: MOV OUT[0], TEMP[0]
  4: END
`,
		srcs: [3][4]uint32{uv(1, 2, 3, 1), uv(4, 5, 6, 7), uv(8, 9, 10, 11)},
		want: uv(4, 5, 6, 8),
	}, {
		// KILL_IF of a negative channel discards: the color stays clear.
		name: "kill-if",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL TEMP[0], LOCAL
  0: KILL_IF CONST[0]
  1: MOV OUT[0], CONST[1]
  2: END
`,
		srcs: [3][4]uint32{fv(1, 2, -3, 4), uv(5, 6, 7, 8)},
		want: uv(0, 0, 0, 0),
	}, {
		// RET in an IF returns from the body to the epilogue, which
		// writes the color.
		name: "ret",
		text: `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
  0: MOV OUT[0], CONST[0]
  1: UIF CONST[1].xxxx :3
  2:   RET
  3: ENDIF
  4: MOV OUT[0], CONST[2]
  5: END
`,
		srcs: [3][4]uint32{uv(1, 2, 3, 4), uv(1, 0, 0, 0), uv(5, 6, 7, 8)},
		want: uv(1, 2, 3, 4),
	}} {
		var constants []uint32
		for _, s := range c.srcs {
			constants = append(constants, s[:]...)
		}
		px := render(t, d, passVS, c.text, full, 1, targetRGBA32U, constants)
		var got [4]uint32
		for i := range got {
			got[i] = binary.LittleEndian.Uint32(px[4*i:])
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// newTexture returns a texture of RGBA32F texels, w by h, in the shader
// read layout.
func newTexture(t *testing.T, d *Device, w, h uint32, texels [][4]float32) *Image {
	t.Helper()
	img, err := d.NewImage(formatR32G32B32A32Sfloat, w, h, ImageSampled|ImageTransferDst)
	if err != nil {
		t.Fatalf("NewImage: %v", err)
	}
	t.Cleanup(func() { d.DestroyImage(img) })
	staging, err := d.NewBuffer(uint64(16*w*h), BufferTransferSrc, true)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer d.DestroyBuffer(staging)
	b := staging.Bytes()
	for i, tx := range texels {
		for j, f := range tx {
			binary.LittleEndian.PutUint32(b[16*i+4*j:], math.Float32bits(f))
		}
	}
	if err := d.Record(func(c *Commands) error {
		c.ImageLayout(img, ImageLayoutTransferDstOptimal, StageTopOfPipe, 0, StageTransfer, AccessTransferWrite)
		c.CopyBufferToImage(staging, img)
		c.ImageLayout(img, ImageLayoutShaderReadOnlyOptimal, StageTransfer, AccessTransferWrite,
			StageFragmentShader, AccessShaderRead)
		return nil
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return img
}

// TestTextures checks texture instructions of a 2 by 2 texture of distinct
// texels, sampled of the nearest texels: the coordinates and the results
// that the instructions take and give.
func TestTextures(t *testing.T) {
	d := newTestDevice(t)
	img := newTexture(t, d, 2, 2, [][4]float32{{1, 0, 0, 1}, {0, 1, 0, 1}, {0, 0, 1, 1}, {1, 1, 1, 0.5}})
	nearest, err := d.NewSampler(SamplerDesc{AddressMode: 2}) // VK_SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	defer d.DestroySampler(nearest)
	tex := TextureBinding{Stage: 4, Slot: 0, Image: img, Sampler: nearest}
	full := [][8]float32{{-1, -1, 0, 1}, {3, -1, 0, 1}, {-1, 3, 0, 1}}
	const decls = `FRAG
DCL OUT[0], COLOR
DCL CONST[0..2]
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL TEMP[0..1], LOCAL
`
	for _, c := range []programCase{{
		name: "tex",
		text: decls + "  0: TEX TEMP[0], CONST[0], SAMP[0], 2D\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{fv(0.75, 0.25, 0, 0)},
		want: fv(0, 1, 0, 1),
	}, {
		name: "txl",
		text: decls + "  0: TXL TEMP[0], CONST[0], SAMP[0], 2D\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{fv(0.75, 0.75, 0, 0)},
		want: fv(1, 1, 1, 0.5),
	}, {
		name: "txp",
		text: decls + "  0: TXP TEMP[0], CONST[0], SAMP[0], 2D\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{fv(0.5, 1.5, 0, 2)},
		want: fv(0, 0, 1, 1),
	}, {
		name: "tex-offset",
		text: "FRAG\nDCL OUT[0], COLOR\nDCL CONST[0..2]\nDCL SAMP[0]\nDCL SVIEW[0], 2D, FLOAT\nDCL TEMP[0..1], LOCAL\nIMM[0] INT32 {1, 0, 0, 0}\n" +
			"  0: TEX TEMP[0], CONST[0], SAMP[0], 2D, IMM[0].xyz\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{fv(0.25, 0.25, 0, 0)},
		want: fv(0, 1, 0, 1),
	}, {
		name: "txf",
		text: decls + "  0: TXF TEMP[0], CONST[0], SAMP[0], 2D\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{iv(0, 1, 0, 0)},
		want: fv(0, 0, 1, 1),
	}, {
		name: "txq",
		text: decls + "  0: TXQ TEMP[0], CONST[0].xxxx, SAMP[0], 2D\n  1: MOV OUT[0], TEMP[0]\n  2: END\n",
		srcs: [3][4]uint32{iv(0, 0, 0, 0)},
		want: iv(2, 2, 0, 1),
	}, {
		// The red of the four texels about the center, in Vulkan's order:
		// (0,1), (1,1), (1,0), (0,0).
		name: "tg4",
		text: "FRAG\nDCL OUT[0], COLOR\nDCL CONST[0..2]\nDCL SAMP[0]\nDCL SVIEW[0], 2D, FLOAT\nDCL TEMP[0..1], LOCAL\nIMM[0] UINT32 {0, 3, 0, 0}\n" +
			"  0: TG4 TEMP[0], CONST[0], IMM[0].xxxx, SAMP[0], 2D\n  1: TG4 TEMP[1], CONST[0], IMM[0].yyyy, SAMP[0], 2D\n" +
			"  2: MOV TEMP[0].w, TEMP[1].xxxx\n  3: MOV OUT[0], TEMP[0]\n  4: END\n",
		srcs: [3][4]uint32{fv(0.5, 0.5, 0, 0)},
		want: fv(0, 1, 0, 1),
	}} {
		var constants []uint32
		for _, s := range c.srcs {
			constants = append(constants, s[:]...)
		}
		px := render(t, d, passVS, c.text, full, 1, targetRGBA32U, constants, tex)
		var got [4]uint32
		for i := range got {
			got[i] = binary.LittleEndian.Uint32(px[4*i:])
		}
		if got != c.want {
			t.Errorf("%s: got %#x, want %#x", c.name, got, c.want)
		}
	}
}
