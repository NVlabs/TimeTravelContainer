// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// Shaders of the kinds that guests' drivers send.
var shaders = map[string]string{
	"vs-transform": `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION, INVARIANT
DCL OUT[1], GENERIC[9]
DCL CONST[0][0..3]
DCL TEMP[0..1], LOCAL
IMM[0] FLT32 {0x3f000000, 0x00000000, 0xbf800000, 0x3f800000}
  0: MUL TEMP[0], CONST[0][0], IN[0].xxxx
  1: MAD TEMP[0], CONST[0][1], IN[0].yyyy, TEMP[0]
  2: MAD TEMP[0], CONST[0][2], IN[0].zzzz, TEMP[0]
  3: MAD OUT[0], CONST[0][3], IN[0].wwww, TEMP[0]
  4: MOV_SAT OUT[1], -|IN[1].yxzw|
  5: END
`,
	"fs-texture-kill": `FRAG
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
	"fs-loop": `FRAG
PROPERTY FS_COORD_ORIGIN LOWER_LEFT
PROPERTY FS_COORD_PIXEL_CENTER INTEGER
DCL IN[0], POSITION, LINEAR
DCL IN[1], FACE, CONSTANT
DCL OUT[0], COLOR
DCL OUT[1], POSITION
DCL TEMP[0..3], ARRAY(1), LOCAL
DCL TEMP[4..5], LOCAL
DCL ADDR[0]
IMM[0] UINT32 {0, 1, 4, 0}
IMM[1] FLT32 {0x3f800000, 0x00000000, 0x3f000000, 0x40000000}
  0: MOV TEMP[4].x, IMM[0].xxxx
  1: BGNLOOP :9
  2:   USGE TEMP[5].x, TEMP[4].xxxx, IMM[0].zzzz
  3:   UIF TEMP[5].xxxx :5
  4:     BRK
  5:   ENDIF
  6:   UARL ADDR[0].x, TEMP[4].xxxx
  7:   MOV TEMP[ADDR[0].x](1), IN[0]
  8:   UADD TEMP[4].x, TEMP[4].xxxx, IMM[0].yyyy
  9: ENDLOOP :1
 10: ADD TEMP[4], TEMP[0], TEMP[3]
 11: DP3 TEMP[5].x, TEMP[4].xyzz, IMM[1].xyzz
 12: RSQ TEMP[5].y, TEMP[5].xxxx
 13: CMP TEMP[4], IN[1].xxxx, IMM[1].yyyy, IMM[1].xxxx
 14: LRP TEMP[4], TEMP[5].yyyy, TEMP[4], IMM[1].zzzz
 15: KILL_IF -TEMP[5].xxxx
 16: MOV OUT[0], TEMP[4]
 17: MOV OUT[1].z, IMM[1].zzzz
 18: END
`,
	"vs-int-inputs": `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL SV[0], VERTEXID
DCL SV[1], INSTANCEID
DCL SAMP[0]
DCL SVIEW[0], 2D, UINT
DCL TEMP[0..1], LOCAL
IMM[0] INT32 {1, 3, 0, 0}
  0: I2F TEMP[0], IN[1]
  1: UADD TEMP[1].x, SV[0].xxxx, SV[1].xxxx
  2: IBFE TEMP[1].y, IN[1].xxxx, IMM[0].xxxx, IMM[0].yyyy
  3: TXF TEMP[1], IMM[0].zzzz, SAMP[0], 2D
  4: TXQ TEMP[1], IMM[0].zzzz, SAMP[0], 2D
  5: MOV OUT[0], IN[0]
  6: ADD OUT[1], TEMP[0], TEMP[1]
  7: END
`,
	"cs-buffers": `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 64
PROPERTY CS_FIXED_BLOCK_HEIGHT 1
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL SV[1], BLOCK_ID
DCL BUFFER[0]
DCL BUFFER[1]
DCL MEMORY[1], SHARED
DCL TEMP[0..2], LOCAL
IMM[0] UINT32 {64, 2, 0, 1}
  0: UMAD TEMP[0].x, SV[1].xxxx, IMM[0].xxxx, SV[0].xxxx
  1: SHL TEMP[0].x, TEMP[0].xxxx, IMM[0].yyyy
  2: LOAD TEMP[1].xy, BUFFER[0], TEMP[0].xxxx
  3: STORE MEMORY[1].x, TEMP[0].xxxx, TEMP[1].xxxx
  4: BARRIER
  5: ATOMUADD TEMP[2].x, BUFFER[1], IMM[0].zzzz, IMM[0].wwww
  6: ATOMCAS TEMP[2].x, BUFFER[1], IMM[0].zzzz, IMM[0].wwww, TEMP[2].xxxx
  7: STORE BUFFER[1].xy, TEMP[0].xxxx, TEMP[1]
  8: RESQ TEMP[2].x, BUFFER[0]
  9: MEMBAR IMM[0].wwww
 10: END
`,
	"fs-shadow-rect": `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SAMP[1]
DCL SVIEW[0], SHADOW2D, FLOAT
DCL SVIEW[1], RECT, FLOAT
DCL TEMP[0..1], LOCAL
  0: TEX TEMP[0], IN[0], SAMP[0], SHADOW2D
  1: TXP TEMP[1], IN[0], SAMP[1], RECT
  2: DDX TEMP[1].x, TEMP[1].xxxx
  3: MUL OUT[0], TEMP[0], TEMP[1]
  4: END
`,
	"gs-layered": `GEOM
PROPERTY GS_INPUT_PRIMITIVE TRIANGLES
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
PROPERTY GS_INVOCATIONS 2
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[0]
DCL IN[][2], CLIPDIST[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL OUT[2], LAYER
DCL OUT[3], CLIPDIST[0]
DCL SV[0], PRIM_ID
DCL SV[1], INVOCATIONID
DCL TEMP[0..1], LOCAL
DCL ADDR[0]
IMM[0] UINT32 {0, 1, 2, 3}
  0: MOV TEMP[0].x, IMM[0].xxxx
  1: BGNLOOP :12
  2:   USGE TEMP[1].x, TEMP[0].xxxx, IMM[0].wwww
  3:   UIF TEMP[1].xxxx :5
  4:     BRK
  5:   ENDIF
  6:   UARL ADDR[0].x, TEMP[0].xxxx
  7:   MOV OUT[0], IN[ADDR[0].x][0]
  8:   MOV OUT[1], IN[ADDR[0].x][1]
  9:   UADD OUT[2].x, SV[0].xxxx, SV[1].xxxx
 10:   MOV OUT[3], IN[2][2]
 11:   EMIT IMM[0].xxxx
 12:   UADD TEMP[0].x, TEMP[0].xxxx, IMM[0].yyyy
 13: ENDLOOP :1
 14: ENDPRIM IMM[0].xxxx
 15: END
`,
	"tcs-patch": `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 3
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[0]
DCL OUT[][0], POSITION
DCL OUT[][1], GENERIC[0]
DCL OUT[2], PATCH[0]
DCL OUT[3], TESSOUTER
DCL OUT[4], TESSINNER
DCL SV[0], INVOCATIONID
DCL SV[1], VERTICESIN
DCL SV[2], PRIM_ID
DCL TEMP[0..1], LOCAL
DCL ADDR[0]
IMM[0] FLT32 {0x40000000, 0x3f800000, 0x00000000, 0x00000000}
  0: UARL ADDR[0].x, SV[0].xxxx
  1: MOV OUT[ADDR[0].x][0], IN[ADDR[0].x][0]
  2: MOV OUT[ADDR[0].x][1].xy, IN[ADDR[0].x][1]
  3: BARRIER
  4: MOV TEMP[0], OUT[0][1]
  5: MOV OUT[3], IMM[0].xxxx
  6: MOV OUT[4].xy, IMM[0].yyyy
  7: UADD TEMP[1].x, SV[1].xxxx, SV[2].xxxx
  8: MOV OUT[2], TEMP[0]
  9: MOV OUT[2].w, OUT[3].xxxx
 10: END
`,
	"tes-triangles": `TESS_EVAL
PROPERTY TES_PRIM_MODE 4
PROPERTY TES_SPACING 1
PROPERTY TES_VERTEX_ORDER_CW 1
PROPERTY TES_POINT_MODE 0
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[0]
DCL IN[2], PATCH[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL OUT[2], CLIPDIST[0]
DCL SV[0], TESSCOORD
DCL SV[1], TESSOUTER
DCL SV[2], PRIM_ID
DCL TEMP[0], LOCAL
  0: MUL TEMP[0], IN[0][0], SV[0].xxxx
  1: MAD TEMP[0], IN[1][0], SV[0].yyyy, TEMP[0]
  2: MAD OUT[0], IN[2][0], SV[0].zzzz, TEMP[0]
  3: ADD OUT[1], IN[0][1], IN[2]
  4: MOV OUT[2], SV[1]
  5: MOV OUT[1].w, SV[2].xxxx
  6: END
`,
	"fs-interp-outputs": `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL IN[1], GENERIC[1], CONSTANT
DCL IN[2], COLOR, COLOR
DCL IN[3], PRIM_ID, CONSTANT
DCL IN[4], LAYER, CONSTANT
DCL OUT[0], COLOR
DCL OUT[1], COLOR[1]
DCL OUT[2], POSITION
DCL OUT[3], STENCIL
DCL OUT[4], SAMPLEMASK
DCL SV[0], SAMPLEID
DCL SV[1], SAMPLEPOS
DCL SV[2], SAMPLEMASK
DCL SV[3], HELPER_INVOCATION
DCL TEMP[0..2], LOCAL
IMM[0] FLT32 {0x3f000000, 0xbf000000, 0x00000000, 0x3f800000}
  0: INTERP_CENTROID TEMP[0], IN[0]
  1: INTERP_SAMPLE TEMP[1], IN[0], SV[0].xxxx
  2: INTERP_OFFSET TEMP[2], -IN[0].yxzw, IMM[0].xyyy
  3: ADD TEMP[0], TEMP[0], TEMP[1]
  4: ADD TEMP[0], TEMP[0], TEMP[2]
  5: MAD OUT[0], TEMP[0], IN[2], IN[4].xxxx
  6: MOV OUT[1], IN[1]
  7: MOV OUT[2].z, SV[1].xxxx
  8: MOV OUT[3].y, IN[3].xxxx
  9: MOV OUT[4].x, SV[2].xxxx
 10: UIF SV[3].xxxx :12
 11:   DEMOTE
 12: ENDIF
 13: END
`,
	"vs-clip-cull": `VERT
PROPERTY NUM_CLIPDIST_ENABLED 6
PROPERTY NUM_CULLDIST_ENABLED 2
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], CLIPDIST[0]
DCL OUT[2], CLIPDIST[1]
DCL OUT[3], LAYER
DCL OUT[4], PSIZE
DCL OUT[5], GENERIC[0]
DCL SV[0], INSTANCEID
DCL SV[1], VERTEXID
DCL TEMP[0], LOCAL
IMM[0] FLT32 {0x3f800000, 0x40000000, 0x00000000, 0x00000000}
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[0]
  2: MOV OUT[2], -IN[0]
  3: MOV OUT[3].x, SV[0].xxxx
  4: MOV OUT[4].x, IMM[0].yyyy
  5: MOV_SAT TEMP[0], IN[0]
  6: UADD TEMP[0].x, SV[1].xxxx, -SV[0].xxxx
  7: LDEXP OUT[5], TEMP[0], IMM[0].zzzz
  8: END
`,
	"cs-images": `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 8
PROPERTY CS_FIXED_BLOCK_HEIGHT 8
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL SV[1], BLOCK_ID
DCL IMAGE[0], 2D, PIPE_FORMAT_R8G8B8A8_UNORM, WR
DCL IMAGE[1], 2D_ARRAY, PIPE_FORMAT_R32_UINT, WR
DCL IMAGE[2], BUFFER, PIPE_FORMAT_R32G32B32A32_FLOAT
DCL IMAGE[3], 2D, PIPE_FORMAT_NONE, WR
DCL IMAGE[4], CUBEARRAY, PIPE_FORMAT_R16G16_FLOAT, WR
DCL TEMP[0..2], LOCAL
IMM[0] UINT32 {8, 1, 0, 0}
  0: UMAD TEMP[0].xy, SV[1].xyyy, IMM[0].xxxx, SV[0].xyyy
  1: LOAD TEMP[1], IMAGE[0], TEMP[0].xyyy, 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  2: STORE IMAGE[0], TEMP[0].xyyy, TEMP[1], 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  3: ATOMUADD TEMP[2].x, IMAGE[1], TEMP[0].xyzz, IMM[0].yyyy, 2D_ARRAY, PIPE_FORMAT_R32_UINT
  4: ATOMCAS TEMP[2].x, IMAGE[1], TEMP[0].xyzz, IMM[0].zzzz, IMM[0].yyyy, 2D_ARRAY, PIPE_FORMAT_R32_UINT
  5: LOAD TEMP[1], IMAGE[2], TEMP[0].xxxx, BUFFER, PIPE_FORMAT_R32G32B32A32_FLOAT
  6: RESQ TEMP[2], IMAGE[0]
  7: STORE IMAGE[3], TEMP[0].xyyy, TEMP[1], 2D, PIPE_FORMAT_NONE
  8: STORE IMAGE[4], TEMP[0].xyzz, TEMP[1], CUBEARRAY, PIPE_FORMAT_R16G16_FLOAT
  9: RESQ TEMP[2], IMAGE[4]
 10: END
`,
	"fs-gather": `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SAMP[1]
DCL SAMP[2]
DCL SVIEW[0], 2D, FLOAT
DCL SVIEW[1], SHADOWCUBEARRAY, FLOAT
DCL SVIEW[2], 2D_MSAA, UINT
DCL TEMP[0..3], LOCAL
IMM[0] UINT32 {1, 0, 0, 0}
IMM[1] INT32 {-1, 1, 0, 0}
IMM[2] INT32 {-1, -1, 0, 0}
IMM[3] INT32 {1, -1, 0, 0}
  0: TG4 TEMP[0], IN[0], IMM[0].xxxx, SAMP[0], 2D
  1: TG4 TEMP[1], IN[0], IMM[0].yyyy, SAMP[0], 2D, IMM[1].xyz
  2: TG4 TEMP[2], IN[0], IN[0].wwww, SAMP[1], SHADOWCUBEARRAY
  3: TG4 TEMP[3], IN[0], IMM[0].xxxx, SAMP[0], 2D, TEMP[0].xyz
  4: TG4 TEMP[3], IN[0], IMM[0].xxxx, SAMP[0], 2D, IMM[1].xyz, IMM[2].xyz, IMM[3].xyz, IMM[1].xyz
  5: TXQS TEMP[1].x, SAMP[2], 2D_MSAA
  6: ADD OUT[0], TEMP[0], TEMP[1]
  7: END
`,
	"fs-resource-arrays": `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SAMP[1]
DCL SAMP[2]
DCL SAMP[3]
DCL SVIEW[0], 2D, FLOAT
DCL SVIEW[1], 2D, FLOAT
DCL SVIEW[2], 2D, FLOAT
DCL SVIEW[3], 2D, FLOAT
DCL CONST[0][0..1]
DCL CONST[1][0..3]
DCL CONST[2][0..3]
DCL BUFFER[0]
DCL BUFFER[1]
DCL IMAGE[0], 2D, PIPE_FORMAT_R32_UINT, WR
DCL IMAGE[1], 2D, PIPE_FORMAT_R32_UINT, WR
DCL TEMP[0..2], LOCAL
DCL ADDR[0..2]
IMM[0] UINT32 {0, 4, 0, 0}
  0: F2U TEMP[0].x, IN[0].zzzz
  1: UARL ADDR[2].x, TEMP[0].xxxx
  2: TEX TEMP[1], IN[0], SAMP[ADDR[2].x+1], 2D
  3: TEX TEMP[2], IN[0], SAMP[1], 2D
  4: UARL ADDR[1].x, TEMP[0].xxxx
  5: MOV TEMP[2], CONST[ADDR[1].x+1][2]
  6: LOAD TEMP[0].x, BUFFER[ADDR[2].x], IMM[0].yyyy
  7: ATOMUADD TEMP[0].y, IMAGE[ADDR[2].x], IMM[0].xxxx, IMM[0].yyyy, 2D, PIPE_FORMAT_R32_UINT
  8: ADD OUT[0], TEMP[1], TEMP[2]
  9: MOV OUT[0].x, CONST[0].yyyy
 10: END
`,
}

func translate(t *testing.T, name, text string, opts Options) *Result {
	t.Helper()
	s, err := tgsi.Parse(text)
	if err != nil {
		t.Fatalf("%s: Parse: %v", name, err)
	}
	res, err := Translate(s, opts)
	if err != nil {
		t.Fatalf("%s: Translate: %v", name, err)
	}
	if dir := os.Getenv("TOSPIRV_DUMP"); dir != "" {
		b := make([]byte, 4*len(res.Words))
		for i, w := range res.Words {
			binary.LittleEndian.PutUint32(b[4*i:], w)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".spv"), b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func TestTranslate(t *testing.T) {
	for name, text := range shaders {
		t.Run(name, func(t *testing.T) {
			opts := Options{ColorBuffers: 2, FlatShade: true, PointSize: 4}
			opts.InputTypes[1] = Int
			opts.ColorTypes[1] = Uint
			res := translate(t, name, text, opts)
			if res.Words[0] != spirv.Magic {
				t.Fatalf("magic %#x", res.Words[0])
			}
			for _, v := range append(append([]Varying(nil), res.Inputs...), res.Outputs...) {
				if v.LocationWord <= 0 || v.LocationWord >= len(res.Words) {
					t.Errorf("varying %v[%d]'s location word %d", v.Semantic, v.SemanticIndex, v.LocationWord)
				}
			}
		})
	}
}

func TestTranslateErrors(t *testing.T) {
	for name, text := range map[string]string{
		"undeclared": "FRAG\nDCL OUT[0], COLOR\n  0: MOV OUT[0], TEMP[0]\n  1: END\n",
		"unclosed":   "FRAG\nDCL TEMP[0]\n  0: UIF TEMP[0].xxxx :1\n  1: END\n",
		"brk":        "FRAG\n  0: BRK\n  1: END\n",
		"kill-vs":    "VERT\n  0: KILL\n  1: END\n",
		"const-past": "VERT\nDCL OUT[0], POSITION\nDCL CONST[0][0]\n  0: MOV OUT[0], CONST[0][5]\n  1: END\n",
		"fbfetch-vs": "VERT\nDCL OUT[0], POSITION\nDCL TEMP[0]\n  0: FBFETCH TEMP[0], OUT[0]\n  1: END\n",
	} {
		s, err := tgsi.Parse(text)
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if _, err := Translate(s, Options{}); err == nil {
			t.Errorf("%s: Translate succeeded", name)
		}
	}
}

// TestCorpus translates each shader of $TOSPIRV_CORPUS, a directory of the
// TGSI texts, *.tgsi, that a renderer's guests sent, and reports the
// shaders that do not print as they parse or do not translate, by their
// errors, writing the others' SPIR-V to $TOSPIRV_DUMP.
func TestCorpus(t *testing.T) {
	dir := os.Getenv("TOSPIRV_CORPUS")
	if dir == "" {
		t.Skip("TOSPIRV_CORPUS names no corpus")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.tgsi"))
	if err != nil {
		t.Fatal(err)
	}
	failed := make(map[string][]string)
	reprinted := 0
	for _, path := range files {
		name := filepath.Base(path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := tgsi.Parse(string(b))
		if err != nil {
			failed["parse: "+err.Error()] = append(failed["parse: "+err.Error()], name)
			continue
		}
		if s.Print() != string(b) {
			reprinted++
		}
		opts := Options{ColorBuffers: 1}
		res, err := Translate(s, opts)
		if err != nil {
			msg := err.Error()
			if e, ok := err.(*Error); ok {
				msg = s.Processor.String() + ": " + e.Msg
			}
			failed[msg] = append(failed[msg], name)
			continue
		}
		if out := os.Getenv("TOSPIRV_DUMP"); out != "" {
			b := make([]byte, 4*len(res.Words))
			for i, w := range res.Words {
				binary.LittleEndian.PutUint32(b[4*i:], w)
			}
			if err := os.WriteFile(filepath.Join(out, name+".spv"), b, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	msgs := make([]string, 0, len(failed))
	n := 0
	for msg, names := range failed {
		msgs = append(msgs, msg)
		n += len(names)
	}
	sort.Slice(msgs, func(i, j int) bool { return len(failed[msgs[i]]) > len(failed[msgs[j]]) })
	for _, msg := range msgs {
		t.Errorf("%d shaders: %s (%s)", len(failed[msg]), msg, failed[msg][0])
	}
	t.Logf("%d shaders: %d translated, %d failed; %d print otherwise", len(files), len(files)-n, n, reprinted)
}

func TestStreamOutput(t *testing.T) {
	opts := Options{StreamOutput: &StreamOutput{
		Outputs: []StreamOutputItem{
			{Register: 0, NumComponents: 4},
			{Register: 1, StartComponent: 1, NumComponents: 2, Buffer: 1, DstOffset: 1},
			// Past its stride: not captured.
			{Register: 1, NumComponents: 4, Buffer: 1, DstOffset: 2},
		},
		Strides: [MaxStreamOutputBuffers]uint32{4, 3},
		UBO:     14,
		SSBO:    8,
	}}
	res := translate(t, "vs-xfb", shaders["vs-transform"], opts)
	var ubo, ssbos int
	for _, r := range res.Resources {
		switch {
		case r.Kind == UniformBuffer && r.Slot == 14:
			ubo++
		case r.Kind == StorageBuffer && (r.Slot == 8 || r.Slot == 9):
			ssbos++
		}
	}
	if ubo != 1 || ssbos != 2 {
		t.Errorf("resources %+v", res.Resources)
	}
}

// TestFixedFunction translates a fragment shader of GL's fixed-function
// state that guests leave to the host: two-sided colors, flat shading, and
// the alpha test.
func TestFixedFunction(t *testing.T) {
	opts := Options{TwoSided: true, FlatShade: true, AlphaTest: true, AlphaFunc: FuncGreater, AlphaRef: 0.5}
	res := translate(t, "fs-fixed-function", shaders["fs-texture-kill"], opts)
	var bcolor bool
	for _, v := range res.Inputs {
		if v.Semantic == tgsi.SemanticBColor && v.SemanticIndex == 0 && v.Flat {
			bcolor = true
		}
	}
	if !bcolor || !res.Kills {
		t.Errorf("inputs %+v, kills %v", res.Inputs, res.Kills)
	}
}

// TestClipPlanesAndSprites translates shaders of GL's user clip planes and
// point sprites.
func TestClipPlanesAndSprites(t *testing.T) {
	ucp := `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], CLIPVERTEX
DCL OUT[2], TEXCOORD[3]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], -IN[0]
  2: MOV OUT[2], IN[0]
  3: END
`
	translate(t, "vs-user-clip-planes", ucp, Options{UserClipPlanes: [][4]float32{{1, 0, 0, 0}, {0, 1, 0, 0.5}}})
	translate(t, "vs-clip-disable", shaders["vs-clip-cull"], Options{ClipDisable: 0x21})
	// Of OpenGL's clip space's z, which Vulkan's is not, once the clip
	// planes used it.
	translate(t, "vs-gl-clip-z", ucp, Options{UserClipPlanes: [][4]float32{{1, 0, 0, 0}}, GLClipZ: true})
	sprite := `FRAG
DCL IN[0], TEXCOORD[3], PERSPECTIVE
DCL IN[1], TEXCOORD[4], PERSPECTIVE
DCL OUT[0], COLOR
  0: ADD OUT[0], IN[0], IN[1]
  1: END
`
	res := translate(t, "fs-sprite", sprite, Options{SpriteCoords: 1 << 3, SpriteCoordsLowerLeft: true})
	if len(res.Inputs) != 1 || res.Inputs[0].SemanticIndex != 4 {
		t.Errorf("inputs %+v", res.Inputs)
	}
}

// TestFramebufferFetch translates FBFETCH, as Mesa's GLSL linker blends
// advanced equations with it, of single-sampled and multisampled color
// buffers, of which one's integers.
func TestFramebufferFetch(t *testing.T) {
	text := `FRAG
DCL OUT[0], COLOR
DCL OUT[1], COLOR[1]
DCL TEMP[0..1]
  0: FBFETCH TEMP[0], OUT[0]
  1: FBFETCH TEMP[1], OUT[1].yxzw
  2: MOV OUT[0], TEMP[0]
  3: MOV OUT[1], TEMP[1]
  4: END
`
	for _, ms := range []bool{false, true} {
		opts := Options{Multisampled: ms}
		opts.ColorTypes[1] = Uint
		name := "fs-fbfetch"
		if ms {
			name += "-ms"
		}
		res := translate(t, name, text, opts)
		var got []Resource
		for _, r := range res.Resources {
			if r.Kind == InputAttachment {
				got = append(got, r)
			}
		}
		want := []Resource{{Kind: InputAttachment, Slot: 0, Count: 1, Type: Float},
			{Kind: InputAttachment, Slot: 1, Count: 1, Type: Uint}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: input attachments %+v, want %+v", name, got, want)
		}
	}
}

// TestExtraOutputs translates a vertex shader of outputs that the next
// stage reads, of which it writes one.
func TestExtraOutputs(t *testing.T) {
	extra := []Varying{{Semantic: tgsi.SemanticGeneric, SemanticIndex: 3}, {Semantic: tgsi.SemanticGeneric, SemanticIndex: 9},
		{Semantic: tgsi.SemanticColor}}
	res := translate(t, "vs-extra-outputs", shaders["vs-transform"], Options{ExtraOutputs: extra})
	got := make(map[Varying]int)
	for _, v := range res.Outputs {
		got[Varying{Semantic: v.Semantic, SemanticIndex: v.SemanticIndex}]++
	}
	for _, v := range extra {
		if got[v] != 1 {
			t.Errorf("output %v[%d] %d times: %+v", v.Semantic, v.SemanticIndex, got[v], res.Outputs)
		}
	}
	translate(t, "gs-extra-outputs", shaders["gs-layered"], Options{ExtraOutputs: extra})
	translate(t, "tcs-extra-outputs", shaders["tcs-patch"], Options{ExtraOutputs: extra})
}

// TestSampleShading translates a fragment shader of sample shading, of
// which it interpolates its inputs at the samples' locations, as OpenGL
// does, but its flat ones, which it interpolates not.
func TestSampleShading(t *testing.T) {
	text := `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL IN[1], GENERIC[1], LINEAR, CENTROID
DCL IN[2], GENERIC[2], CONSTANT
DCL OUT[0], COLOR
DCL TEMP[0]
  0: ADD TEMP[0], IN[0], IN[1]
  1: ADD OUT[0], TEMP[0], IN[2]
  2: END
`
	for _, shading := range []bool{false, true} {
		res := translate(t, fmt.Sprintf("fs-sample-shading-%v", shading), text, Options{SampleShading: shading})
		samples := 0
		for i := 5; i < len(res.Words); {
			n, op := res.Words[i]>>16, spirv.Op(res.Words[i]&0xffff)
			if n == 0 {
				t.Fatalf("an instruction of no words at %d", i)
			}
			if op == spirv.OpDecorate && spirv.Decoration(res.Words[i+2]) == spirv.DecorationSample {
				samples++
			}
			i += int(n)
		}
		if want := map[bool]int{false: 0, true: 2}[shading]; samples != want {
			t.Errorf("of sample shading %v: %d inputs of samples' locations, want %d", shading, samples, want)
		}
	}
}
