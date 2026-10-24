// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the tests' commands carry, as virgl_hw.h and Gallium number them.
const (
	formatR8G8B8A8Unorm     = 67
	formatR32G32B32A32Float = 31
	formatR8Unorm           = 64
	bindRenderTarget        = 1 << 1
	bindSamplerView         = 1 << 3
	bindVertexBuffer        = 1 << 4
	bindQueryBuffer         = 1 << 15
	target2D                = 2
	clearColor0             = 1 << 2
)

// stream encodes virgl's commands, as Mesa's virgl driver does.
type stream []uint32

func (s *stream) cmd(cmd, obj uint32, payload ...uint32) {
	*s = append(*s, cmd|obj<<8|uint32(len(payload))<<16)
	*s = append(*s, payload...)
}

func (s stream) bytes() []byte {
	b := make([]byte, 4*len(s))
	for i, w := range s {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

func f32(f float32) uint32 {
	return math.Float32bits(f)
}

// textDwords returns text, NUL-terminated, in dwords.
func textDwords(text []byte) []uint32 {
	b := append([]byte(nil), text...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	d := make([]uint32, len(b)/4)
	for i := range d {
		d[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return d
}

// shader creates a shader of text, in parts of at most part bytes.
func (s *stream) shader(handle, stage uint32, text string, part int) {
	s.shaderTokens(handle, stage, text, part, 300)
}

// shaderTokens creates a shader of text, of tokens tokens as the guest
// counts them, in parts of at most part bytes.
func (s *stream) shaderTokens(handle, stage uint32, text string, part int, tokens uint32) {
	b := append([]byte(text), 0)
	for off := 0; off < len(b); off += part {
		end := min(off+part, len(b))
		offlen := uint32(len(b))
		if off > 0 {
			offlen = shaderOffsetContinuation | uint32(off)
		}
		payload := append([]uint32{handle, stage, offlen, tokens, 0}, textDwords(b[off:end])...)
		s.cmd(cmdCreateObject, objectShader, payload...)
	}
}

const vertexShader = `VERT
DCL IN[0]
DCL OUT[0], POSITION
  0: MOV OUT[0], IN[0]
  1: END
`

const greenShader = `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], COLOR
IMM[0] FLT32 {    0.0000,     1.0000,     0.0000,     1.0000}
  0: MOV OUT[0], IMM[0]
  1: END
`

const width, height = 64, 64

// fixture is a context with a render target and a vertex buffer of a
// triangle over the lower left of the target's left half.
type fixture struct {
	c      *Context
	rt     *Resource
	rtMem  []byte
	vb     *Resource
	vbMem  []byte
	nextID uint32
	// mems are, of a clone's fixture, the memory of its resources, by ID.
	mems map[uint32][]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if err := Init(); err != nil {
		t.Skipf("the backend is unavailable: %v", err)
	}
	f := &fixture{nextID: 100}
	f.rtMem = make([]byte, width*height*4)
	var err error
	f.rt, err = NewResource(1, ResourceArgs{Target: target2D, Format: formatR8G8B8A8Unorm,
		Bind: bindRenderTarget | bindSamplerView, Width: width, Height: height, Depth: 1, ArraySize: 1}, f.rtMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	t.Cleanup(f.rt.DecRef)
	f.vbMem = make([]byte, 3*16)
	for i, v := range []float32{-1, -1, 0, 1, 0, -1, 0, 1, -1, 1, 0, 1} {
		binary.LittleEndian.PutUint32(f.vbMem[4*i:], math.Float32bits(v))
	}
	f.vb, err = NewResource(2, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindVertexBuffer, Width: uint32(len(f.vbMem)), Height: 1, Depth: 1, ArraySize: 1}, f.vbMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	t.Cleanup(f.vb.DecRef)
	f.c, err = NewContext(1)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	t.Cleanup(f.c.Destroy)
	f.c.Attach(f.rt)
	f.c.Attach(f.vb)
	if err := f.c.Transfer(f.vb, TransferArgs{ToHost: true, W: uint32(len(f.vbMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	return f
}

func (f *fixture) id() uint32 {
	f.nextID++
	return f.nextID
}

// handles are the objects of a draw's state.
type handles struct {
	surf, blend, rast, dsa, vs, fs, ve uint32
}

// setup encodes the state of a draw of the triangle in green.
func (f *fixture) setup(s *stream) handles {
	surf := f.id()
	s.cmd(cmdCreateObject, objectSurface, surf, f.rt.ID(), formatR8G8B8A8Unorm, 0, 0)
	s.cmd(cmdSetFramebufferState, 0, 1, 0, surf)
	// As Mesa's virgl driver sends after each framebuffer.
	s.cmd(cmdSetFramebufferNoAttach, 0, width|height<<16, 0)
	s.cmd(cmdSetViewportState, 0, 0, f32(width/2), f32(height/2), f32(0.5), f32(width/2), f32(height/2), f32(0.5))
	blend := f.id()
	s.cmd(cmdCreateObject, objectBlend, blend, 0, 0, 0xf<<27, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectBlend, blend)
	rast := f.id()
	s.cmd(cmdCreateObject, objectRasterizer, rast, 1<<1|1<<29, f32(1), 0, 0, f32(1), 0, 0, 0)
	s.cmd(cmdBindObject, objectRasterizer, rast)
	dsa := f.id()
	s.cmd(cmdCreateObject, objectDSA, dsa, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectDSA, dsa)
	vs, fs := f.id(), f.id()
	s.shader(vs, stageVertex, vertexShader, 1<<16)
	// In parts, as Mesa sends text longer than a command.
	s.shader(fs, stageFragment, greenShader, 32)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	ve := f.id()
	s.cmd(cmdCreateObject, objectVertexElements, ve, 0, 0, 0, formatR32G32B32A32Float)
	s.cmd(cmdBindObject, objectVertexElements, ve)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, f.vb.ID())
	return handles{surf, blend, rast, dsa, vs, fs, ve}
}

func clearRed(s *stream) {
	s.cmd(cmdClear, 0, clearColor0, f32(1), f32(0), f32(0), f32(1), 0, 0, 0)
}

func drawTriangle(s *stream) {
	s.cmd(cmdDrawVBO, 0, 0, 3, primTriangles, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
}

func (f *fixture) submit(t *testing.T, s stream) {
	t.Helper()
	if err := f.c.Submit(s.bytes()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

// finish waits for what the context executed, and reads the render target.
func (f *fixture) finish(t *testing.T) {
	t.Helper()
	f.c.Flush().Wait()
	if err := f.c.Transfer(f.rt, TransferArgs{W: width, H: height, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
}

func (f *fixture) pixel(x, y int) [4]byte {
	var p [4]byte
	copy(p[:], f.rtMem[(y*width+x)*4:])
	return p
}

var (
	red   = [4]byte{255, 0, 0, 255}
	green = [4]byte{0, 255, 0, 255}
)

func (f *fixture) checkTriangle(t *testing.T) {
	t.Helper()
	// The triangle covers x below 16 on the middle row.
	if p := f.pixel(4, height/2); p != green {
		t.Errorf("inside the triangle: %v, want %v", p, green)
	}
	if p := f.pixel(width-4, height/2); p != red {
		t.Errorf("outside the triangle: %v, want %v", p, red)
	}
}

func TestTriangle(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

// TestHexFloatImmediate draws with an immediate of floats that Mesa's virgl
// driver prints as their bits in hex, which strtof took for hexadecimal
// floats: 0x3f000000, 0.5, was 1056964608.
func TestHexFloatImmediate(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	fs := f.id()
	s.shader(fs, stageFragment, `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], COLOR
IMM[0] FLT32 {0x3f000000, 0x3f800000, 0x00000000, 0x3f800000}
  0: MOV OUT[0], IMM[0]
  1: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	// 0.5 of 255, which GPUs round either way.
	if p := f.pixel(4, height/2); p[0] < 127 || p[0] > 128 || p[1] != 255 || p[2] != 0 || p[3] != 255 {
		t.Errorf("inside the triangle: %v, want {127 or 128, 255, 0, 255}", p)
	}
}

// TestFrontFace draws the triangle in green where a fragment shader's FACE
// input, which TGSI has positive for front faces, is, as Mesa's virgl
// driver reads it, and red where it is not. The rasterizer culls back
// faces, of either winding in turn: the triangle is front facing of the
// winding that draws it. tgsi_to_nir gave Zink, whose face is an integer
// system value, an input FACE of the system value's bits: ~0, a NaN, of
// front faces.
func TestFrontFace(t *testing.T) {
	f := newFixture(t)
	drawn := 0
	for _, ccw := range []uint32{0, 1} {
		var s stream
		f.setup(&s)
		rast := f.id()
		// Clip depth, cull back faces, and center pixels.
		s.cmd(cmdCreateObject, objectRasterizer, rast, 1<<1|2<<8|ccw<<15|1<<29, f32(1), 0, 0, f32(1), 0, 0, 0)
		s.cmd(cmdBindObject, objectRasterizer, rast)
		fs := f.id()
		s.shader(fs, stageFragment, `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL IN[0], FACE, CONSTANT
DCL OUT[0], COLOR
DCL TEMP[0]
IMM[0] FLT32 {0x00000000, 0x3f800000, 0x00000000, 0x00000000}
  0: SGE TEMP[0].x, IN[0].xxxx, IMM[0].xxxx
  1: ADD OUT[0].x, IMM[0].yyyy, -TEMP[0].xxxx
  2: MOV OUT[0].y, TEMP[0].xxxx
  3: MOV OUT[0].zw, IMM[0].xxxy
  4: END
`, 1<<16)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		s.cmd(cmdClear, 0, clearColor0, 0, 0, f32(1), f32(1), 0, 0, 0)
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		switch p := f.pixel(4, height/2); p {
		case [4]byte{0, 0, 255, 255}:
		case green:
			drawn++
		default:
			t.Errorf("front faces counterclockwise %d: %v, want %v", ccw, p, green)
			drawn++
		}
	}
	if drawn != 1 {
		t.Errorf("the triangle drew of %d windings, want 1", drawn)
	}
}

// TestBlitPastTheEdges blits a texture, scaled, to a box past every edge
// of the render target, of a scissor of its left half, as Mesa's state
// tracker clips scaled blits: the scissor clips the blit.
func TestBlitPastTheEdges(t *testing.T) {
	f := newFixture(t)
	const scissorEnable = 1 << 10
	mem := bytes.Repeat(green[:], 4*4)
	src, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatR8G8B8A8Unorm, Bind: bindSamplerView,
		Width: 4, Height: 4, Depth: 1, ArraySize: 1}, mem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer src.DecRef()
	f.c.Attach(src)
	if err := f.c.Transfer(src, TransferArgs{ToHost: true, W: 4, H: 4, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	edge := int32(-16)
	var s stream
	f.setup(&s)
	clearRed(&s)
	s.cmd(cmdBlit, 0, 0xf|scissorEnable, 0, width/2|height<<16, f.rt.ID(), 0, formatR8G8B8A8Unorm,
		uint32(edge), uint32(edge), 0, width+32, height+32, 1, src.ID(), 0, formatR8G8B8A8Unorm, 0, 0, 0, 4, 4, 1)
	f.submit(t, s)
	f.finish(t)
	for _, tc := range []struct {
		x    int
		want [4]byte
	}{{0, green}, {width/2 - 1, green}, {width / 2, red}, {width - 1, red}} {
		for _, y := range []int{0, height - 1} {
			if p := f.pixel(tc.x, y); p != tc.want {
				t.Errorf("at (%d, %d): %v, want %v", tc.x, y, p, tc.want)
			}
		}
	}
}

// TestOneSidedStencilReference replaces the stencil of triangles of either
// winding, of a one-sided stencil test of a back face's reference of 0, as
// Mesa's state tracker clears scissored stencils of quads, and draws where
// it replaced it, of a back face's reference of 2: back faces of one-sided
// tests use the front's reference.
func TestOneSidedStencilReference(t *testing.T) {
	const (
		formatZ24S8      = 19
		bindDepthStencil = 1 << 0
		clearStencil     = 1 << 1
		// Stencil states of virgl's DSA: enabled, of ALWAYS, of REPLACE
		// of each op, and of masks of 0xff; and enabled, of EQUAL, of
		// KEEP, and of no writes.
		stencilReplace = 1 | 7<<1 | 2<<4 | 2<<7 | 2<<10 | 0xff<<13 | 0xff<<21
		stencilEqual   = 1 | 2<<1 | 0xff<<13
	)
	f := newFixture(t)
	zs, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatZ24S8, Bind: bindDepthStencil,
		Width: width, Height: height, Depth: 1, ArraySize: 1}, make([]byte, width*height*4), nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer zs.DecRef()
	f.c.Attach(zs)
	// Quads of two triangles each: the left half's, winding one way, and
	// the right half's, the other, as Mesa's clears' quads do.
	var quadsMem []byte
	for _, v := range []float32{
		-1, -1, 0, 1, 0, -1, 0, 1, 0, 1, 0, 1, -1, -1, 0, 1, 0, 1, 0, 1, -1, 1, 0, 1,
		0, 1, 0, 1, 1, 1, 0, 1, 1, -1, 0, 1, 0, 1, 0, 1, 1, -1, 0, 1, 0, -1, 0, 1,
	} {
		quadsMem = binary.LittleEndian.AppendUint32(quadsMem, math.Float32bits(v))
	}
	quads, err := NewResource(f.id(), ResourceArgs{Target: targetBuffer, Format: formatR8Unorm, Bind: bindVertexBuffer,
		Width: uint32(len(quadsMem)), Height: 1, Depth: 1, ArraySize: 1}, quadsMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer quads.DecRef()
	f.c.Attach(quads)
	if err := f.c.Transfer(quads, TransferArgs{ToHost: true, W: uint32(len(quadsMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	check := func(when string, want [4]byte) {
		t.Helper()
		for _, x := range []int{4, width - 4} {
			if p := f.pixel(x, height/2); p != want {
				t.Errorf("%s: at %d of the middle row: %v, want %v", when, x, p, want)
			}
		}
	}
	drawQuads := func(s *stream) {
		s.cmd(cmdDrawVBO, 0, 0, 12, primTriangles, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
	}
	// Green where the stencil is 1, of none yet.
	var s stream
	h := f.setup(&s)
	zsurf, noColor, replace, equal := f.id(), f.id(), f.id(), f.id()
	s.cmd(cmdCreateObject, objectSurface, zsurf, zs.ID(), formatZ24S8, 0, 0)
	s.cmd(cmdSetFramebufferState, 0, 1, zsurf, h.surf)
	s.cmd(cmdSetFramebufferNoAttach, 0, width|height<<16, 0)
	s.cmd(cmdClear, 0, clearColor0|clearStencil, f32(1), f32(0), f32(0), f32(1), 0, 0, 0)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, quads.ID())
	s.cmd(cmdCreateObject, objectDSA, equal, 0, stencilEqual, 0, 0)
	s.cmd(cmdBindObject, objectDSA, equal)
	s.cmd(cmdSetStencilRef, 0, 1|2<<8)
	drawQuads(&s)
	f.submit(t, s)
	f.finish(t)
	check("of stencils of 0", red)
	// Stencils of 1, of no colors, and green where they are.
	s = nil
	s.cmd(cmdCreateObject, objectBlend, noColor, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectBlend, noColor)
	s.cmd(cmdCreateObject, objectDSA, replace, 0, stencilReplace, 0, 0)
	s.cmd(cmdBindObject, objectDSA, replace)
	s.cmd(cmdSetStencilRef, 0, 1)
	drawQuads(&s)
	s.cmd(cmdBindObject, objectBlend, h.blend)
	s.cmd(cmdBindObject, objectDSA, equal)
	s.cmd(cmdSetStencilRef, 0, 1|2<<8)
	drawQuads(&s)
	f.submit(t, s)
	f.finish(t)
	check("of stencils of 1", green)
}

// TestBorderColors samples textures' borders of colors that Vulkan's
// samplers have not, and MoltenVK has none of, which the translation blends
// of samples of borders of transparent black and of opaque white: of
// colors, of gathers, of swizzled views, of integers, of comparisons of
// depths, of filtering at the edge, and of the texture within it.
func TestBorderColors(t *testing.T) {
	const (
		formatR8G8B8A8Uint = 180
		formatZ32Float     = 18
		bindDepthStencil   = 1 << 0
		clampToBorder      = 3
		mipNone            = 2
		linear             = 1<<9 | 1<<13
		compareLess        = 1<<15 | 1<<16
		swizzleRGBA        = 0 | 1<<3 | 2<<6 | 3<<9
		swizzleBGR1        = 2 | 1<<3 | 0<<6 | 5<<9
	)
	f := newFixture(t)
	texture := func(format, bind uint32, texel []byte) *Resource {
		mem := bytes.Repeat(texel, 4)
		r, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: format, Bind: bind, Width: 2, Height: 2,
			Depth: 1, ArraySize: 1}, mem, nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		t.Cleanup(r.DecRef)
		f.c.Attach(r)
		if err := f.c.Transfer(r, TransferArgs{ToHost: true, W: 2, H: 2, D: 1}); err != nil {
			t.Fatalf("Transfer: %v", err)
		}
		return r
	}
	color := texture(formatR8G8B8A8Unorm, bindSamplerView, []byte{10, 200, 30, 255})
	integer := texture(formatR8G8B8A8Uint, bindSamplerView, []byte{1, 2, 3, 4})
	depth := texture(formatZ32Float, bindSamplerView|bindDepthStencil, binary.LittleEndian.AppendUint32(nil, f32(0.9)))
	const (
		texFS = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
  0: TEX OUT[0], IN[0], SAMP[0], 2D
  1: END
`
		gatherFS = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
IMM[0] UINT32 {1, 0, 0, 0}
  0: TG4 OUT[0], IN[0], IMM[0].xxxx, SAMP[0], 2D
  1: END
`
		uintFS = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, UINT
DCL TEMP[0]
IMM[0] FLT32 {0x3b808081, 0x00000000, 0x00000000, 0x00000000}
  0: TEX TEMP[0], IN[0], SAMP[0], 2D
  1: U2F TEMP[0], TEMP[0]
  2: MUL OUT[0], TEMP[0], IMM[0].xxxx
  3: END
`
		shadowFS = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
  0: TEX OUT[0], IN[0], SAMP[0], SHADOW2D
  1: END
`
	)
	for _, tc := range []struct {
		name            string
		fs              string
		res             *Resource
		format, swizzle uint32
		sampler         uint32
		border          [4]float32
		coord           [3]float32
		want            [4]byte
	}{
		{"a color", texFS, color, formatR8G8B8A8Unorm, swizzleRGBA, 0, [4]float32{0.25, 0.5, 0.75, 1}, [3]float32{-0.5, -0.5, 0}, [4]byte{64, 128, 191, 255}},
		{"the texture", texFS, color, formatR8G8B8A8Unorm, swizzleRGBA, 0, [4]float32{0.25, 0.5, 0.75, 1}, [3]float32{0.25, 0.25, 0}, [4]byte{10, 200, 30, 255}},
		{"half the edge", texFS, color, formatR8G8B8A8Unorm, swizzleRGBA, linear, [4]float32{0.25, 0.5, 0.75, 1}, [3]float32{0, 0.25, 0}, [4]byte{37, 164, 111, 255}},
		{"a gather", gatherFS, color, formatR8G8B8A8Unorm, swizzleRGBA, 0, [4]float32{0.25, 0.5, 0.75, 1}, [3]float32{-0.5, -0.5, 0}, [4]byte{128, 128, 128, 128}},
		{"a swizzle", texFS, color, formatR8G8B8A8Unorm, swizzleBGR1, 0, [4]float32{0.25, 0.5, 0.75, 0}, [3]float32{-0.5, -0.5, 0}, [4]byte{191, 128, 64, 255}},
		{"white", texFS, color, formatR8G8B8A8Unorm, swizzleRGBA, 0, [4]float32{1, 1, 1, 1}, [3]float32{-0.5, -0.5, 0}, [4]byte{255, 255, 255, 255}},
		{"integers", uintFS, integer, formatR8G8B8A8Uint, swizzleRGBA, 0, [4]float32{}, [3]float32{-0.5, -0.5, 0}, [4]byte{7, 8, 9, 10}},
		{"a passing comparison", shadowFS, depth, formatZ32Float, swizzleRGBA, compareLess, [4]float32{0.5}, [3]float32{-0.5, -0.5, 0.25}, [4]byte{255, 255, 255, 255}},
		{"a failing comparison", shadowFS, depth, formatZ32Float, swizzleRGBA, compareLess, [4]float32{0.5}, [3]float32{-0.5, -0.5, 0.75}, [4]byte{0, 0, 0, 255}},
	} {
		border := [4]uint32{f32(tc.border[0]), f32(tc.border[1]), f32(tc.border[2]), f32(tc.border[3])}
		if tc.res == integer {
			border = [4]uint32{7, 8, 9, 10}
		}
		var s stream
		f.setup(&s)
		clearRed(&s)
		vs, fs, view, samp := f.id(), f.id(), f.id(), f.id()
		s.shader(vs, stageVertex, fmt.Sprintf(`VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] FLT32 {0x%08x, 0x%08x, 0x%08x, 0x3f800000}
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IMM[0]
  2: END
`, f32(tc.coord[0]), f32(tc.coord[1]), f32(tc.coord[2])), 1<<16)
		s.shader(fs, stageFragment, tc.fs, 1<<16)
		s.cmd(cmdBindShader, 0, vs, stageVertex)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		s.cmd(cmdCreateObject, objectSamplerView, view, tc.res.ID(), tc.format|target2D<<24, 0, 0, tc.swizzle)
		s.cmd(cmdCreateObject, objectSamplerState, samp, clampToBorder|clampToBorder<<3|clampToBorder<<6|mipNone<<11|tc.sampler,
			0, 0, 0, border[0], border[1], border[2], border[3])
		s.cmd(cmdSetSamplerViews, 0, stageFragment, 0, view)
		s.cmd(cmdBindSamplerStates, 0, stageFragment, 0, samp)
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		p := f.pixel(4, height/2)
		for i := range p {
			if d := int(p[i]) - int(tc.want[i]); d < -2 || d > 2 {
				t.Errorf("%s: %v, want %v", tc.name, p, tc.want)
				break
			}
		}
	}
}

// TestTexture samples a texture, whose sampler TGSI's NIR names
// "sampler", which Metal has for a type: MoltenVK failed to compile the
// shader, and Zink crashed binding no pipeline.
func TestTexture(t *testing.T) {
	f := newFixture(t)
	var s stream
	texel := f.textured(t, &s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	if p := f.pixel(4, height/2); p != texel {
		t.Errorf("inside the triangle: %v, want the texel %v", p, texel)
	}
	if p := f.pixel(width-4, height/2); p != red {
		t.Errorf("outside the triangle: %v, want %v", p, red)
	}
}

// textured encodes a draw of the triangle in the texel of a texture, which
// it returns, on red, and leaves the draw's state with the sampler, the
// sampler view, and the first vertex shader destroyed.
func (f *fixture) textured(t *testing.T, s *stream) [4]byte {
	t.Helper()
	texel := [4]byte{10, 200, 30, 255}
	texMem := make([]byte, 2*2*4)
	for i := 0; i < 4; i++ {
		copy(texMem[4*i:], texel[:])
	}
	tex, err := NewResource(5, ResourceArgs{Target: target2D, Format: formatR8G8B8A8Unorm,
		Bind: bindSamplerView, Width: 2, Height: 2, Depth: 1, ArraySize: 1}, texMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	t.Cleanup(tex.DecRef)
	f.c.Attach(tex)
	if err := f.c.Transfer(tex, TransferArgs{ToHost: true, W: 2, H: 2, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	h := f.setup(s)
	clearRed(s)
	vs, fs := f.id(), f.id()
	s.shader(vs, stageVertex, `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] FLT32 {    0.2500,     0.2500,     0.0000,     1.0000}
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IMM[0]
  2: END
`, 1<<16)
	s.shader(fs, stageFragment, `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
  0: TEX OUT[0], IN[0], SAMP[0], 2D
  1: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	view, samp := f.id(), f.id()
	const swizzleRGBA = 0 | 1<<3 | 2<<6 | 3<<9
	s.cmd(cmdCreateObject, objectSamplerView, view, tex.ID(), formatR8G8B8A8Unorm|target2D<<24, 0, 0, swizzleRGBA)
	// Clamped to the edge, nearest, without mipmaps.
	const clampToEdge, mipNone = 2, 2
	s.cmd(cmdCreateObject, objectSamplerState, samp, clampToEdge|clampToEdge<<3|clampToEdge<<6|mipNone<<11, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdSetSamplerViews, 0, stageFragment, 0, view)
	s.cmd(cmdBindSamplerStates, 0, stageFragment, 0, samp)
	drawTriangle(s)
	// The guest may destroy what is bound.
	s.cmd(cmdDestroyObject, 0, samp)
	s.cmd(cmdDestroyObject, 0, view)
	s.cmd(cmdDestroyObject, 0, h.vs)
	return texel
}

// TestIntegerVertexFormat feeds a vertex shader 8-bit unsigned integers,
// which TGSI's typeless inputs read as floats, and Metal refuses to.
func TestIntegerVertexFormat(t *testing.T) {
	f := newFixture(t)
	const formatR8G8B8A8Uint = 180
	colorMem := []byte{0, 255, 0, 255, 0, 255, 0, 255, 0, 255, 0, 255}
	colors, err := NewResource(6, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindVertexBuffer, Width: uint32(len(colorMem)), Height: 1, Depth: 1, ArraySize: 1}, colorMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer colors.DecRef()
	f.c.Attach(colors)
	if err := f.c.Transfer(colors, TransferArgs{ToHost: true, W: uint32(len(colorMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	f.setup(&s)
	clearRed(&s)
	vs, fs, ve := f.id(), f.id(), f.id()
	s.shader(vs, stageVertex, `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL TEMP[0]
IMM[0] FLT32 {    0.0039,     0.0039,     0.0039,     0.0039}
  0: MOV OUT[0], IN[0]
  1: U2F TEMP[0], IN[1]
  2: MUL OUT[1], TEMP[0], IMM[0]
  3: END
`, 1<<16)
	s.shader(fs, stageFragment, `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
  0: MOV OUT[0], IN[0]
  1: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	s.cmd(cmdCreateObject, objectVertexElements, ve, 0, 0, 0, formatR32G32B32A32Float, 0, 0, 1, formatR8G8B8A8Uint)
	s.cmd(cmdBindObject, objectVertexElements, ve)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, f.vb.ID(), 4, 0, colors.ID())
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	// 255 * 0.0039 rounds to 254 or 255.
	if p := f.pixel(4, height/2); p[0] != 0 || p[1] < 254 || p[2] != 0 {
		t.Errorf("inside the triangle: %v, want %v", p, green)
	}
}

// TestVertexFormats draws the triangle in green of colors of vertex
// formats that Vulkan, or the device, fetches not, which Mesa's virgl
// driver sends of OpenGL's vertex attributes as it offers them all:
// 32-bit normalized and scaled channels, packed channels of 10, 10, 10 and
// 2 bits, scaled, and 8-bit scaled channels.
func TestVertexFormats(t *testing.T) {
	f := newFixture(t)
	const (
		formatR32G32B32A32Unorm = 35
		formatR32G32B32Uscaled  = 38
		formatR32G32B32A32Snorm = 43
		formatR10G10B10A2Uscale = 123
		formatR10G10B10A2Snorm  = 173
		formatR8G8B8A8Uscaled   = 72
	)
	u32s := func(ws ...uint32) []byte {
		var b []byte
		for _, w := range ws {
			b = binary.LittleEndian.AppendUint32(b, w)
		}
		return b
	}
	for _, tc := range []struct {
		name   string
		format uint32
		color  []byte
	}{
		{"R32G32B32A32_UNORM", formatR32G32B32A32Unorm, u32s(0, 0xffffffff, 0, 0xffffffff)},
		{"R32G32B32_USCALED", formatR32G32B32Uscaled, u32s(0, 1, 0)},
		{"R32G32B32A32_SNORM", formatR32G32B32A32Snorm, u32s(0, 0x7fffffff, 0, 0x7fffffff)},
		{"R10G10B10A2_USCALED", formatR10G10B10A2Uscale, u32s(1<<10 | 1<<30)},
		{"R10G10B10A2_SNORM", formatR10G10B10A2Snorm, u32s(511<<10 | 1<<30)},
		{"R8G8B8A8_USCALED", formatR8G8B8A8Uscaled, []byte{0, 1, 0, 1}},
	} {
		colors := f.buffer(t, append(tc.color, make([]byte, 16-len(tc.color))...))
		var s stream
		f.setup(&s)
		clearRed(&s)
		vs, fs, ve := f.id(), f.id(), f.id()
		s.shader(vs, stageVertex, `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[1]
  2: END
`, 1<<16)
		s.shader(fs, stageFragment, `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
  0: MOV OUT[0], IN[0]
  1: END
`, 1<<16)
		s.cmd(cmdBindShader, 0, vs, stageVertex)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		// The color of vertex buffer 1, of a stride of 0: each vertex's.
		s.cmd(cmdCreateObject, objectVertexElements, ve, 0, 0, 0, formatR32G32B32A32Float, 0, 0, 1, tc.format)
		s.cmd(cmdBindObject, objectVertexElements, ve)
		s.cmd(cmdSetVertexBuffers, 0, 16, 0, f.vb.ID(), 0, 0, colors.ID())
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		if p := f.pixel(4, height/2); p != green {
			t.Errorf("%s: inside the triangle: %v, want %v", tc.name, p, green)
		}
	}
}

// TestUnalignedVertices draws the triangle of positions of bytes at an
// offset of no multiple of 4, as OpenGL's vertex attributes may be, of the
// vertex buffer's offset and of the element's: Metal fetches vertex
// buffers of offsets of multiples of 4 alone. And of positions of 16-bit
// channels at an odd offset, which no Vulkan device fetches.
func TestUnalignedVertices(t *testing.T) {
	f := newFixture(t)
	const (
		formatR8G8B8A8Snorm     = 77
		formatR16G16B16A16Snorm = 59
	)
	// The fixture's triangle, of normalized bytes, after a byte.
	posMem := []byte{0xaa, 0x81, 0x81, 0, 127, 0, 0x81, 0, 127, 0x81, 127, 0, 127}
	pos, aligned := f.buffer(t, posMem), f.buffer(t, posMem[1:])
	// And of normalized 16-bit integers, after a byte.
	var pos16Mem []byte
	for _, b := range posMem[1:] {
		pos16Mem = binary.LittleEndian.AppendUint16(pos16Mem, uint16(int16(int8(b))*258))
	}
	pos16 := f.buffer(t, append([]byte{0xaa}, pos16Mem...))
	for _, tc := range []struct {
		name           string
		vb             *Resource
		format, stride uint32
		vbOffset, eOff uint32
	}{
		{"aligned", aligned, formatR8G8B8A8Snorm, 4, 0, 0},
		{"of the buffer's offset", pos, formatR8G8B8A8Snorm, 4, 1, 0},
		{"of the element's", pos, formatR8G8B8A8Snorm, 4, 0, 1},
		{"of 16-bit channels at an odd offset", pos16, formatR16G16B16A16Snorm, 8, 1, 0},
	} {
		var s stream
		f.setup(&s)
		clearRed(&s)
		ve := f.id()
		s.cmd(cmdCreateObject, objectVertexElements, ve, tc.eOff, 0, 0, tc.format)
		s.cmd(cmdBindObject, objectVertexElements, ve)
		s.cmd(cmdSetVertexBuffers, 0, tc.stride, tc.vbOffset, tc.vb.ID())
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		if p := f.pixel(4, height/2); p != green {
			t.Errorf("%s: inside the triangle: %v, want %v", tc.name, p, green)
		}
		if p := f.pixel(width-4, height/2); p != red {
			t.Errorf("%s: outside the triangle: %v, want %v", tc.name, p, red)
		}
	}
}

// TestFanOfOffset draws the triangle of a triangle fan of a vertex buffer
// of an offset, as Mesa's state tracker draws quads of uploads.
func TestFanOfOffset(t *testing.T) {
	f := newFixture(t)
	vbMem := append(make([]byte, 144), f.vbMem...)
	vb := f.buffer(t, vbMem)
	for _, mode := range []uint32{primTriangles, primTriangleFan} {
		var s stream
		f.setup(&s)
		clearRed(&s)
		s.cmd(cmdSetVertexBuffers, 0, 16, 144, vb.ID())
		s.cmd(cmdDrawVBO, 0, 0, 3, mode, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
		f.submit(t, s)
		f.finish(t)
		if p := f.pixel(4, height/2); p != green {
			t.Errorf("mode %d: inside the triangle: %v, want %v", mode, p, green)
		}
	}
}

// TestStencilCellsAndSample replaces the stencil of a framebuffer's depth
// and stencil texture of quads of references of their own, of triangle
// fans, as Mesa's state tracker clears scissored stencils, and samples it,
// of a view of the stencil alone, in one submission, as dEQP's stencil
// texturing does. The quads wind either way: the one-sided test of back
// faces uses the front's reference, the one Mesa's clears set.
func TestStencilCellsAndSample(t *testing.T) {
	f := newFixture(t)
	const (
		formatZ24S8      = 19
		formatR32Uint    = 193
		formatX24S8      = 136
		bindDepthStencil = 1 << 0
	)
	zs, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatZ24S8, Bind: bindDepthStencil | bindSamplerView,
		Width: width, Height: height, Depth: 1, ArraySize: 1}, make([]byte, width*height*4), nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer zs.DecRef()
	f.c.Attach(zs)
	colorMem := make([]byte, width*height*4)
	color, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatR32Uint, Bind: bindRenderTarget,
		Width: width, Height: height, Depth: 1, ArraySize: 1}, colorMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer color.DecRef()
	f.c.Attach(color)
	// Two quads, of the left half and of the right, of fans of 4 vertices
	// each, after 144 bytes; the right one's of the winding of Mesa's.
	var quads []byte
	for _, v := range []float32{-1, -1, 0, 1, 0, -1, 0, 1, 0, 1, 0, 1, -1, 1, 0, 1, 0, 1, 0, 1, 1, 1, 0, 1, 1, -1, 0, 1, 0, -1, 0, 1} {
		quads = binary.LittleEndian.AppendUint32(quads, math.Float32bits(v))
	}
	vb := f.buffer(t, append(make([]byte, 144), quads...))
	ib := f.buffer(t, []byte{0, 1, 2, 0, 2, 3, 4, 5, 6, 4, 6, 7})
	var s stream
	f.setup(&s)
	csurf, zsurf, dsa, blend := f.id(), f.id(), f.id(), f.id()
	s.cmd(cmdCreateObject, objectSurface, csurf, color.ID(), formatR32Uint, 0, 0)
	s.cmd(cmdCreateObject, objectSurface, zsurf, zs.ID(), formatZ24S8, 0, 0)
	s.cmd(cmdSetFramebufferState, 0, 1, zsurf, csurf)
	s.cmd(cmdSetFramebufferNoAttach, 0, width|height<<16, 0)
	s.cmd(cmdCreateObject, objectBlend, blend, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectBlend, blend)
	s.cmd(cmdCreateObject, objectDSA, dsa, 0, 536865071, 0, 0)
	s.cmd(cmdBindObject, objectDSA, dsa)
	// Of a back reference of 0, as Mesa's clears leave it.
	for i, ref := range []uint32{157, 65} {
		s.cmd(cmdSetStencilRef, 0, ref)
		s.cmd(cmdSetVertexBuffers, 0, 16, uint32(144+64*i), vb.ID())
		s.cmd(cmdDrawVBO, 0, 0, 4, primTriangleFan, 0, 1, 0, 0, 0, 0, 0, 3, 0)
	}
	// The stencil, sampled to the color.
	fs, view, samp, blend2, dsa2 := f.id(), f.id(), f.id(), f.id(), f.id()
	s.cmd(cmdDestroyObject, objectSurface, zsurf)
	s.cmd(cmdSetFramebufferState, 0, 1, 0, csurf)
	s.cmd(cmdSetFramebufferNoAttach, 0, width|height<<16, 0)
	s.cmd(cmdCreateObject, objectBlend, blend2, 4, 0, 0xf<<27, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectBlend, blend2)
	s.cmd(cmdCreateObject, objectDSA, dsa2, 0, 0, 0, 0)
	s.cmd(cmdBindObject, objectDSA, dsa2)
	s.shader(fs, stageFragment, `FRAG
DCL IN[0], POSITION, LINEAR
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, UINT
DCL TEMP[0..1]
IMM[0] UINT32 {0, 0, 0, 0}
  0: F2I TEMP[0].xy, IN[0].xyyy
  1: MOV TEMP[0].zw, IMM[0].xxxx
  2: TXF TEMP[1], TEMP[0], SAMP[0], 2D
  3: MOV OUT[0], TEMP[1]
  4: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	const swizzle = 0 | 4<<3 | 4<<6 | 5<<9
	s.cmd(cmdCreateObject, objectSamplerView, view, zs.ID(), formatX24S8|target2D<<24, 0, 0, swizzle)
	s.cmd(cmdCreateObject, objectSamplerState, samp, 0, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdSetSamplerViews, 0, stageFragment, 0, view)
	s.cmd(cmdBindSamplerStates, 0, stageFragment, 0, samp)
	s.cmd(cmdSetVertexBuffers, 0, 16, 144, vb.ID())
	// Of indexes of a byte, as dEQP draws them.
	s.cmd(cmdSetIndexBuffer, 0, ib.ID(), 1, 0)
	s.cmd(cmdDrawVBO, 0, 0, 12, primTriangles, 1, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
	f.submit(t, s)
	f.c.Flush().Wait()
	if err := f.c.Transfer(color, TransferArgs{W: width, H: height, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if v := binary.LittleEndian.Uint32(colorMem[(height/2*width+4)*4:]); v != 157 {
		t.Errorf("the stencil sampled of the left quad: %d, want 157", v)
	}
	if v := binary.LittleEndian.Uint32(colorMem[(height/2*width+width-4)*4:]); v != 65 {
		t.Errorf("the stencil sampled of the right quad: %d, want 65", v)
	}
}

// TestTransformFeedback captures a triangle's positions, which MoltenVK
// cannot, appends a second capture, pauses, and draws what it captured.
func TestTransformFeedback(t *testing.T) {
	f := newFixture(t)
	if !xfbEmulated {
		t.Skip("the renderer emulates no transform feedback here")
	}
	const bindStreamOutput = 1 << 11
	soMem := make([]byte, 2*3*16)
	so, err := NewResource(7, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindStreamOutput | bindVertexBuffer, Width: uint32(len(soMem)), Height: 1, Depth: 1, ArraySize: 1}, soMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer so.DecRef()
	qMem := make([]byte, queryStateSize)
	qres, err := NewResource(8, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindQueryBuffer, Width: queryStateSize, Height: 1, Depth: 1, ArraySize: 1}, qMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer qres.DecRef()
	f.c.Attach(so)
	f.c.Attach(qres)

	var s stream
	f.setup(&s)
	clearRed(&s)
	// A vertex shader that captures OUT[0], its position, to buffer 0, of
	// 4 dwords a vertex.
	vs := f.id()
	text := append([]byte(vertexShader), 0)
	payload := []uint32{vs, stageVertex, uint32(len(text)), 300, 1, 4, 0, 0, 0, 4 << 10, 0}
	s.cmd(cmdCreateObject, objectShader, append(payload, textDwords(text)...)...)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	target, q := f.id(), f.id()
	s.cmd(cmdCreateObject, objectStreamoutTarget, target, so.ID(), 0, uint32(len(soMem)))
	s.cmd(cmdCreateObject, objectQuery, q, queryPrimitivesEmitted, 0, qres.ID())
	s.cmd(cmdSetStreamoutTargets, 0, 0, target)
	s.cmd(cmdBeginQuery, 0, q)
	drawTriangle(&s)
	s.cmd(cmdEndQuery, 0, q)
	s.cmd(cmdGetQueryResult, 0, q, 0)
	// Resumed, the capture appends.
	s.cmd(cmdSetStreamoutTargets, 0, 1, target)
	drawTriangle(&s)
	// Paused, it captures nothing: the buffer has room for two triangles.
	s.cmd(cmdSetStreamoutTargets, 0, 0)
	drawTriangle(&s)
	f.submit(t, s)
	f.c.Flush().Wait()
	if err := f.c.Transfer(so, TransferArgs{W: uint32(len(soMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	for v := 0; v < 6; v++ {
		got := soMem[16*v : 16*v+16]
		want := f.vbMem[16*(v%3) : 16*(v%3)+16]
		if string(got) != string(want) {
			t.Errorf("captured vertex %d: % x, want % x", v, got, want)
		}
	}
	if state, n := binary.LittleEndian.Uint32(qMem), binary.LittleEndian.Uint64(qMem[8:]); state != queryStateDone || n != 1 {
		t.Errorf("primitives emitted: state %d, %d primitives, want state %d, 1", state, n, queryStateDone)
	}

	// The vertices that the target captured, drawn from it.
	s = nil
	clearRed(&s)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, so.ID())
	s.cmd(cmdDrawVBO, 0, 0, 0, primTriangles, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, target)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

// TestTransformFeedbackOverflow captures two triangles to a target with
// room for one: the target takes one, and the queries count the other.
func TestTransformFeedbackOverflow(t *testing.T) {
	f := newFixture(t)
	if !xfbEmulated {
		t.Skip("the renderer emulates no transform feedback here")
	}
	const bindStreamOutput = 1 << 11
	newBuffer := func(id uint32, mem []byte, bind uint32) *Resource {
		r, err := NewResource(id, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
			Bind: bind, Width: uint32(len(mem)), Height: 1, Depth: 1, ArraySize: 1}, mem, nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		t.Cleanup(r.DecRef)
		f.c.Attach(r)
		return r
	}
	vbMem := append(append([]byte(nil), f.vbMem...), f.vbMem...)
	vb := newBuffer(9, vbMem, bindVertexBuffer)
	if err := f.c.Transfer(vb, TransferArgs{ToHost: true, W: uint32(len(vbMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	// The buffer's room past a triangle holds a pattern, which no capture
	// may write over.
	soMem := make([]byte, 3*16+8)
	for i := range soMem {
		soMem[i] = 0x5a
	}
	so := newBuffer(10, soMem, bindStreamOutput)
	if err := f.c.Transfer(so, TransferArgs{ToHost: true, W: uint32(len(soMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	statsMem, overflowMem := make([]byte, queryStateSize), make([]byte, queryStateSize)
	stats := newBuffer(11, statsMem, bindQueryBuffer)
	overflow := newBuffer(12, overflowMem, bindQueryBuffer)

	var s stream
	f.setup(&s)
	vs := f.id()
	text := append([]byte(vertexShader), 0)
	payload := []uint32{vs, stageVertex, uint32(len(text)), 300, 1, 4, 0, 0, 0, 4 << 10, 0}
	s.cmd(cmdCreateObject, objectShader, append(payload, textDwords(text)...)...)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, vb.ID())
	target, qStats, qOverflow := f.id(), f.id(), f.id()
	s.cmd(cmdCreateObject, objectStreamoutTarget, target, so.ID(), 0, uint32(len(soMem)))
	s.cmd(cmdCreateObject, objectQuery, qStats, querySOStatistics, 0, stats.ID())
	s.cmd(cmdCreateObject, objectQuery, qOverflow, querySOOverflowPredicate, 0, overflow.ID())
	s.cmd(cmdSetStreamoutTargets, 0, 0, target)
	s.cmd(cmdBeginQuery, 0, qStats)
	s.cmd(cmdBeginQuery, 0, qOverflow)
	s.cmd(cmdDrawVBO, 0, 0, 6, primTriangles, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
	s.cmd(cmdEndQuery, 0, qStats)
	s.cmd(cmdEndQuery, 0, qOverflow)
	s.cmd(cmdGetQueryResult, 0, qStats, 0)
	s.cmd(cmdGetQueryResult, 0, qOverflow, 0)
	f.submit(t, s)
	f.c.Flush().Wait()
	if err := f.c.Transfer(so, TransferArgs{W: uint32(len(soMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if string(soMem[:48]) != string(f.vbMem) {
		t.Errorf("captured % x, want % x", soMem[:48], f.vbMem)
	}
	if tail := soMem[48:]; string(tail) != "\x5a\x5a\x5a\x5a\x5a\x5a\x5a\x5a" {
		t.Errorf("past the triangle that fits: % x", tail)
	}
	if n := binary.LittleEndian.Uint64(statsMem[8:]); n != 1 {
		t.Errorf("primitives written: %d, want 1", n)
	}
	if n := binary.LittleEndian.Uint64(overflowMem[8:]); n != 1 {
		t.Errorf("overflow: %d, want 1", n)
	}
}

// TestTransformFeedbackLayout gives a stream output an offset past its
// vertex's stride, and a buffer past the targets', which no capture may
// write.
func TestTransformFeedbackLayout(t *testing.T) {
	f := newFixture(t)
	if !xfbEmulated {
		t.Skip("the renderer emulates no transform feedback here")
	}
	const bindStreamOutput = 1 << 11
	soMem := make([]byte, 64)
	for i := range soMem {
		soMem[i] = 0x5a
	}
	so, err := NewResource(13, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindStreamOutput, Width: uint32(len(soMem)), Height: 1, Depth: 1, ArraySize: 1}, soMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer so.DecRef()
	f.c.Attach(so)
	if err := f.c.Transfer(so, TransferArgs{ToHost: true, W: uint32(len(soMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	f.setup(&s)
	vs := f.id()
	text := append([]byte(vertexShader), 0)
	// Two outputs of a stride of 4 dwords: one at dword 3000, one to buffer
	// 5.
	payload := []uint32{vs, stageVertex, uint32(len(text)), 300, 2, 4, 0, 0, 0,
		4<<10 | 3000<<16, 0, 4<<10 | 5<<13, 0}
	s.cmd(cmdCreateObject, objectShader, append(payload, textDwords(text)...)...)
	s.cmd(cmdBindShader, 0, vs, stageVertex)
	target := f.id()
	s.cmd(cmdCreateObject, objectStreamoutTarget, target, so.ID(), 0, 16)
	s.cmd(cmdSetStreamoutTargets, 0, 0, target)
	drawTriangle(&s)
	f.submit(t, s)
	f.c.Flush().Wait()
	if err := f.c.Transfer(so, TransferArgs{W: uint32(len(soMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	for i, b := range soMem {
		if b != 0x5a {
			t.Fatalf("byte %d of the target's buffer is %#x", i, b)
		}
	}
}

func TestCapsets(t *testing.T) {
	if err := Init(); err != nil {
		t.Skipf("the backend is unavailable: %v", err)
	}
	capsets, err := Capsets()
	if err != nil {
		t.Fatalf("Capsets: %v", err)
	}
	if len(capsets) != 2 || capsets[0].ID != CapsetVirgl || capsets[1].ID != CapsetVirgl2 {
		t.Fatalf("capsets %+v", capsets)
	}
	// struct virgl_caps_v1 begins with max_version, and v2's renderer
	// string names the backend's device.
	v2 := capsets[1].Data
	if binary.LittleEndian.Uint32(v2) != 2 {
		t.Errorf("v2's max_version %d", binary.LittleEndian.Uint32(v2))
	}
	if !strings.Contains(string(v2), "vkgl") {
		t.Errorf("v2 names no renderer of vkgl's")
	}
}

func TestQuery(t *testing.T) {
	f := newFixture(t)
	qMem := make([]byte, queryStateSize)
	qres, err := NewResource(3, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindQueryBuffer, Width: queryStateSize, Height: 1, Depth: 1, ArraySize: 1}, qMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer qres.DecRef()
	f.c.Attach(qres)
	var s stream
	f.setup(&s)
	clearRed(&s)
	q := f.id()
	s.cmd(cmdCreateObject, objectQuery, q, queryOcclusionCounter, 0, qres.ID())
	s.cmd(cmdBeginQuery, 0, q)
	drawTriangle(&s)
	s.cmd(cmdEndQuery, 0, q)
	s.cmd(cmdGetQueryResult, 0, q, 0)
	f.submit(t, s)
	// The guest finds the result once the fence of its submission
	// signals.
	f.c.Flush().Wait()
	state := binary.LittleEndian.Uint32(qMem)
	samples := binary.LittleEndian.Uint64(qMem[8:])
	if state != queryStateDone {
		t.Fatalf("query state %d, want %d", state, queryStateDone)
	}
	// Half the left half: 1024 pixels, give or take its edge's.
	if samples < 900 || samples > 1200 {
		t.Errorf("%d samples passed", samples)
	}
}

func TestSubContextsAndZombies(t *testing.T) {
	f := newFixture(t)
	var s stream
	s.cmd(cmdCreateSubCtx, 0, 1)
	s.cmd(cmdSetSubCtx, 0, 1)
	// Sub-context 1 draws, and loses its blend state while it is bound,
	// which must outlive its destruction until another is bound.
	h := f.setup(&s)
	clearRed(&s)
	s.cmd(cmdDestroyObject, 0, h.blend)
	drawTriangle(&s)
	s.cmd(cmdSetSubCtx, 0, 0)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)

	// Sub-context 0 knows none of sub-context 1's objects.
	s = nil
	s.cmd(cmdBindObject, objectBlend, h.blend)
	if err := f.c.Submit(s.bytes()); err == nil {
		t.Errorf("sub-context 0 bound sub-context 1's object")
	}
	s = nil
	s.cmd(cmdDestroySubCtx, 0, 1)
	f.submit(t, s)
}

func TestInlineWriteAndRebind(t *testing.T) {
	f := newFixture(t)
	// A vertex buffer, written by commands, in place of the fixture's.
	vbMem := make([]byte, 48)
	vb, err := NewResource(4, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindVertexBuffer, Width: 48, Height: 1, Depth: 1, ArraySize: 1}, vbMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	f.c.Attach(vb)
	// The context holds what it binds: the creator's reference may go.
	vb.DecRef()
	var s stream
	f.setup(&s)
	clearRed(&s)
	verts := textDwords(f.vbMem)
	s.cmd(cmdResourceInlineWrite, 0, append([]uint32{vb.ID(), 0, 0, 0, 0, 0, 0, 0, 48, 1, 1}, verts...)...)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, vb.ID())
	drawTriangle(&s)
	// Unbinding vertex buffers leaves the backend's slots as they are.
	s.cmd(cmdSetVertexBuffers, 0)
	f.submit(t, s)
	f.c.Detach(vb.ID())
	f.finish(t)
	f.checkTriangle(t)
}

// clone returns a fixture of a clone of f's context, whose resources are
// copies of f's, of memory of their own, as the renderer makes for a fork's
// child.
func (f *fixture) clone(t *testing.T) *fixture {
	t.Helper()
	cf := &fixture{nextID: f.nextID, mems: make(map[uint32][]byte)}
	c, err := f.c.Clone(func(r *Resource) (*Resource, error) {
		mem := append([]byte(nil), r.Backing()...)
		cr, err := r.Clone(mem, nil)
		if err != nil {
			return nil, err
		}
		cf.mems[r.ID()] = mem
		switch r {
		case f.rt:
			cf.rt, cf.rtMem = cr, mem
		case f.vb:
			cf.vb, cf.vbMem = cr, mem
		}
		// The fixture holds a reference of its own.
		cr.IncRef()
		t.Cleanup(cr.DecRef)
		return cr, nil
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	Sync()
	t.Cleanup(c.Destroy)
	cf.c = c
	return cf
}

// A context's clone, a fork's child's, draws with the context's state on
// copies of its resources, and neither sees what the other draws then.
func TestClone(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	clearRed(&s)
	f.submit(t, s)
	cf := f.clone(t)
	// The clone draws on the copy of the target that the parent cleared.
	s = nil
	drawTriangle(&s)
	cf.submit(t, s)
	cf.finish(t)
	cf.checkTriangle(t)
	f.finish(t)
	if p := f.pixel(4, height/2); p != red {
		t.Errorf("the parent's target where its clone drew: %v, want %v", p, red)
	}
	// The parent draws, and clears, as before.
	s = nil
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
	s = nil
	s.cmd(cmdClear, 0, clearColor0, f32(0), f32(0), f32(1), f32(1), 0, 0, 0)
	f.submit(t, s)
	f.finish(t)
	cf.finish(t)
	cf.checkTriangle(t)
}

// A clone samples its copy of a texture, with the sampler and sampler view
// that the guest destroyed while they were bound.
func TestCloneTexture(t *testing.T) {
	f := newFixture(t)
	var s stream
	texel := f.textured(t, &s)
	f.submit(t, s)
	cf := f.clone(t)
	s = nil
	clearRed(&s)
	drawTriangle(&s)
	cf.submit(t, s)
	cf.finish(t)
	if p := cf.pixel(4, height/2); p != texel {
		t.Errorf("inside the triangle: %v, want the texel %v", p, texel)
	}
	if p := cf.pixel(width-4, height/2); p != red {
		t.Errorf("outside the triangle: %v, want %v", p, red)
	}
}

// A query that is active when a context is cloned counts, in the clone,
// what the clone draws from then on, and in the context what it does.
func TestCloneQuery(t *testing.T) {
	f := newFixture(t)
	qMem := make([]byte, queryStateSize)
	qres, err := NewResource(3, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindQueryBuffer, Width: queryStateSize, Height: 1, Depth: 1, ArraySize: 1}, qMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer qres.DecRef()
	f.c.Attach(qres)
	var s stream
	f.setup(&s)
	clearRed(&s)
	q := f.id()
	s.cmd(cmdCreateObject, objectQuery, q, queryOcclusionCounter, 0, qres.ID())
	s.cmd(cmdBeginQuery, 0, q)
	f.submit(t, s)
	cf := f.clone(t)
	end := func(f *fixture, draw bool, mem []byte) uint64 {
		t.Helper()
		var s stream
		if draw {
			drawTriangle(&s)
		}
		s.cmd(cmdEndQuery, 0, q)
		s.cmd(cmdGetQueryResult, 0, q, 0)
		f.submit(t, s)
		f.c.Flush().Wait()
		if state := binary.LittleEndian.Uint32(mem); state != queryStateDone {
			t.Fatalf("query state %d, want %d", state, queryStateDone)
		}
		return binary.LittleEndian.Uint64(mem[8:])
	}
	// Half the left half: 1024 pixels, give or take its edge's.
	if samples := end(cf, true, cf.mems[qres.ID()]); samples < 900 || samples > 1200 {
		t.Errorf("%d samples passed in the clone", samples)
	}
	if samples := end(f, false, qMem); samples != 0 {
		t.Errorf("%d samples passed in the parent, which drew nothing", samples)
	}
}

// A clone has the context's sub-contexts, with the current one current.
func TestCloneSubContexts(t *testing.T) {
	f := newFixture(t)
	var s stream
	s.cmd(cmdCreateSubCtx, 0, 1)
	s.cmd(cmdSetSubCtx, 0, 1)
	h := f.setup(&s)
	s.cmd(cmdDestroyObject, 0, h.blend)
	f.submit(t, s)
	cf := f.clone(t)
	s = nil
	clearRed(&s)
	drawTriangle(&s)
	cf.submit(t, s)
	cf.finish(t)
	cf.checkTriangle(t)
	// Its sub-context 0 has none of sub-context 1's state.
	s = nil
	s.cmd(cmdSetSubCtx, 0, 0)
	s.cmd(cmdBindShader, 0, h.fs, stageFragment)
	if err := cf.c.Submit(s.bytes()); err == nil {
		t.Errorf("the clone's sub-context 0 bound sub-context 1's shader")
	}
}

// sampled reports whether the capabilities offer sampling textures of format.
func sampled(t *testing.T, format uint32) bool {
	t.Helper()
	caps, err := Capsets()
	if err != nil {
		t.Fatalf("Capsets: %v", err)
	}
	// virgl_caps_v1: its version, and then the mask of sampled formats.
	return binary.LittleEndian.Uint32(caps[0].Data[4+4*(format/32):])&(1<<(format%32)) != 0
}

// A copy between a compressed format and a plain one of its blocks' size
// copies blocks as texels, as glCopyImageSubData does, which the
// capabilities offer.
func TestCopyCompressedAndPlain(t *testing.T) {
	const (
		formatDXT1RGB    = 105
		formatETC2RGB8   = 269
		formatR32G32Uint = 194
	)
	f := newFixture(t)
	// A format of 4x4 blocks of 8 bytes that the GPU samples: ETC2's, as
	// Apple's GPUs, or S3TC's, as desktop GPUs and lavapipe.
	var compressed uint32
	for _, format := range []uint32{formatETC2RGB8, formatDXT1RGB} {
		if sampled(t, format) {
			compressed = format
			break
		}
	}
	if compressed == 0 {
		t.Skip("the GPU samples no compressed format of 8-byte blocks")
	}
	// A 16x16 texture of 4x4 blocks of 8 bytes, and textures of their size.
	blocks := make([]byte, 128)
	for i := range blocks {
		blocks[i] = byte(7*i + 1)
	}
	texture := func(id, format, w, h uint32, mem []byte) *Resource {
		r, err := NewResource(id, ResourceArgs{Target: target2D, Format: format, Bind: bindSamplerView,
			Width: w, Height: h, Depth: 1, ArraySize: 1}, mem, nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		t.Cleanup(r.DecRef)
		f.c.Attach(r)
		return r
	}
	etc := texture(5, compressed, 16, 16, blocks)
	plainMem, etc2Mem := make([]byte, 128), make([]byte, 128)
	plain := texture(6, formatR32G32Uint, 4, 4, plainMem)
	etc2 := texture(7, compressed, 16, 16, etc2Mem)
	if err := f.c.Transfer(etc, TransferArgs{ToHost: true, W: 16, H: 16, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	s.cmd(cmdResourceCopyRegion, 0, plain.ID(), 0, 0, 0, 0, etc.ID(), 0, 0, 0, 0, 16, 16, 1)
	s.cmd(cmdResourceCopyRegion, 0, etc2.ID(), 0, 0, 0, 0, plain.ID(), 0, 0, 0, 0, 4, 4, 1)
	f.submit(t, s)
	f.c.Flush().Wait()
	for _, r := range []struct {
		res  *Resource
		w, h uint32
		mem  []byte
	}{{plain, 4, 4, plainMem}, {etc2, 16, 16, etc2Mem}} {
		if err := f.c.Transfer(r.res, TransferArgs{W: r.w, H: r.h, D: 1}); err != nil {
			t.Fatalf("Transfer: %v", err)
		}
		if !bytes.Equal(r.mem, blocks) {
			t.Errorf("resource %d: % x, want % x", r.res.ID(), r.mem, blocks)
		}
	}
	// Of the destination, the copy covers blocks, which must be within it.
	s = nil
	s.cmd(cmdResourceCopyRegion, 0, etc2.ID(), 0, 4, 0, 0, plain.ID(), 0, 0, 0, 0, 4, 4, 1)
	if err := f.c.Submit(s.bytes()); err == nil {
		t.Errorf("a copy past the destination's edge: no error")
	}
}

// A 1D array's layers are its boxes' z, as guests' Mesa addresses them, of
// transfers, copies and clones: a box of rows past the first, which Zink
// would copy outside the image, is refused.
func TestTexture1DArrayLayers(t *testing.T) {
	const target1DArray, layers = 6, 3
	f := newFixture(t)
	array := func(id uint32, mem []byte) *Resource {
		r, err := NewResource(id, ResourceArgs{Target: target1DArray, Format: formatR8G8B8A8Unorm, Bind: bindSamplerView,
			Width: 4, Height: 1, Depth: 1, ArraySize: layers}, mem, nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		t.Cleanup(r.DecRef)
		f.c.Attach(r)
		return r
	}
	texels := make([]byte, 16*layers)
	for i := range texels {
		texels[i] = byte(i + 1)
	}
	mem, copyMem := append([]byte(nil), texels...), make([]byte, 16*layers)
	src, dst := array(5, mem), array(6, copyMem)
	// Layer by layer, from the layer's place in the guest's memory.
	for z := uint32(0); z < layers; z++ {
		if err := f.c.Transfer(src, TransferArgs{ToHost: true, Z: z, W: 4, H: 1, D: 1, Offset: 16 * uint64(z)}); err != nil {
			t.Fatalf("Transfer of layer %d: %v", z, err)
		}
	}
	if err := f.c.Transfer(src, TransferArgs{ToHost: true, Y: 1, W: 4, H: 1, D: 1, Offset: 16}); err == nil {
		t.Errorf("a transfer of a 1D array's second row: no error")
	}
	// Its layers 1 and 2 to the other's 0 and 1.
	var s stream
	s.cmd(cmdResourceCopyRegion, 0, dst.ID(), 0, 0, 0, 0, src.ID(), 0, 0, 0, 1, 4, 1, 2)
	f.submit(t, s)
	clear(mem)
	cf := f.clone(t)
	for _, c := range []struct {
		name string
		f    *fixture
		mems map[uint32][]byte
	}{{"context", f, map[uint32][]byte{src.ID(): mem, dst.ID(): copyMem}}, {"clone", cf, cf.mems}} {
		s = nil
		s.cmd(cmdTransfer3D, 0, src.ID(), 0, 0, 0, 0, 0, 0, 0, 4, 1, layers, 0, transferFromHost)
		s.cmd(cmdTransfer3D, 0, dst.ID(), 0, 0, 0, 0, 0, 0, 0, 4, 1, 2, 0, transferFromHost)
		c.f.submit(t, s)
		if got := c.mems[src.ID()]; !bytes.Equal(got, texels) {
			t.Errorf("%s: the array: % x, want % x", c.name, got, texels)
		}
		if got := c.mems[dst.ID()][:32]; !bytes.Equal(got, texels[16:]) {
			t.Errorf("%s: the copy: % x, want % x", c.name, got, texels[16:])
		}
	}
}

// Transfers and copies that Vulkan does not do are refused: of samples of
// multisampled textures, which guests' Mesa resolves before it transfers
// them, of parts of compressed blocks, between depth and colors, between
// 1D and 2D textures, and between boxes that overlap; and copies of no
// texels do nothing.
func TestTransfersAndCopiesOfVulkan(t *testing.T) {
	const (
		target1D         = 1
		formatR32Float   = 28
		formatZ32Float   = 18
		formatDXT1RGB    = 105
		formatETC2RGB8   = 269
		bindDepthStencil = 1 << 0
	)
	f := newFixture(t)
	texture := func(id uint32, a ResourceArgs) *Resource {
		t.Helper()
		a.Depth, a.ArraySize = max(a.Depth, 1), max(a.ArraySize, 1)
		r, err := NewResource(id, a, make([]byte, 4*a.Width*max(a.Height, 1)), nil)
		if err != nil {
			return nil
		}
		t.Cleanup(r.DecRef)
		f.c.Attach(r)
		return r
	}
	copyRegion := func(dst, src *Resource, x, w, h uint32) error {
		var s stream
		s.cmd(cmdResourceCopyRegion, 0, dst.ID(), 0, 0, 0, 0, src.ID(), 0, x, 0, 0, w, h, 1)
		return f.c.Submit(s.bytes())
	}
	if ms := texture(5, ResourceArgs{Target: target2D, Format: formatR8G8B8A8Unorm, Bind: bindRenderTarget,
		Width: 4, Height: 4, NrSamples: 4}); ms == nil {
		t.Logf("no textures of 4 samples")
	} else {
		for _, toHost := range []bool{true, false} {
			if err := f.c.Transfer(ms, TransferArgs{ToHost: toHost, W: 4, H: 4, D: 1}); err == nil {
				t.Errorf("a transfer of a multisampled texture, to the host %v: no error", toHost)
			}
		}
		var s stream
		s.cmd(cmdResourceInlineWrite, 0, ms.ID(), 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 0)
		if err := f.c.Submit(s.bytes()); err == nil {
			t.Errorf("an inline write of a multisampled texture: no error")
		}
	}
	for _, format := range []uint32{formatETC2RGB8, formatDXT1RGB} {
		if !sampled(t, format) {
			continue
		}
		bc := texture(6, ResourceArgs{Target: target2D, Format: format, Bind: bindSamplerView, Width: 16, Height: 16})
		bc2 := texture(7, ResourceArgs{Target: target2D, Format: format, Bind: bindSamplerView, Width: 16, Height: 16})
		if err := f.c.Transfer(bc, TransferArgs{ToHost: true, X: 2, W: 4, H: 4, D: 1}); err == nil {
			t.Errorf("a transfer of parts of blocks: no error")
		}
		if err := f.c.Transfer(bc, TransferArgs{ToHost: true, X: 12, W: 4, H: 4, D: 1}); err != nil {
			t.Errorf("a transfer of a block: %v", err)
		}
		if err := copyRegion(bc2, bc, 2, 4, 4); err == nil {
			t.Errorf("a copy of parts of blocks: no error")
		}
		break
	}
	depth := texture(8, ResourceArgs{Target: target2D, Format: formatZ32Float, Bind: bindDepthStencil, Width: 4, Height: 4})
	r32 := texture(9, ResourceArgs{Target: target2D, Format: formatR32Float, Bind: bindSamplerView, Width: 4, Height: 4})
	if depth != nil && r32 != nil {
		if err := copyRegion(r32, depth, 0, 4, 4); err == nil {
			t.Errorf("a copy of depth to colors: no error")
		}
	}
	tex1D := texture(10, ResourceArgs{Target: target1D, Format: formatR8G8B8A8Unorm, Bind: bindSamplerView, Width: 4})
	if err := copyRegion(f.rt, tex1D, 0, 4, 1); err == nil {
		t.Errorf("a copy of a 1D texture to a 2D one: no error")
	}
	if err := copyRegion(f.rt, f.rt, 0, 0, 4); err != nil {
		t.Errorf("a copy of no texels: %v", err)
	}
	if err := copyRegion(f.rt, f.rt, 1, 4, 4); err == nil {
		t.Errorf("a copy of a box to a box that it overlaps: no error")
	}
	if err := copyRegion(f.rt, f.rt, 8, 4, 4); err != nil {
		t.Errorf("a copy of a box to one of its texture's level: %v", err)
	}
}

// A copy of an array's layers to a 3D texture's slices copies each layer
// to its slice.
func TestCopyLayersToSlices(t *testing.T) {
	const target3D, target2DArray, layers = 3, 7, 3
	f := newFixture(t)
	texels := make([]byte, 4*4*4*layers)
	for i := range texels {
		texels[i] = byte(5*i + 1)
	}
	slicesMem := make([]byte, len(texels))
	var res [2]*Resource
	for i, a := range []ResourceArgs{
		{Target: target2DArray, Format: formatR8G8B8A8Unorm, Bind: bindSamplerView, Width: 4, Height: 4, Depth: 1, ArraySize: layers},
		{Target: target3D, Format: formatR8G8B8A8Unorm, Bind: bindSamplerView, Width: 4, Height: 4, Depth: layers, ArraySize: 1},
	} {
		mem := texels
		if i == 1 {
			mem = slicesMem
		}
		r, err := NewResource(uint32(5+i), a, mem, nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		defer r.DecRef()
		f.c.Attach(r)
		res[i] = r
	}
	if err := f.c.Transfer(res[0], TransferArgs{ToHost: true, W: 4, H: 4, D: layers}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	s.cmd(cmdResourceCopyRegion, 0, res[1].ID(), 0, 0, 0, 0, res[0].ID(), 0, 0, 0, 0, 4, 4, layers)
	f.submit(t, s)
	if err := f.c.Transfer(res[1], TransferArgs{W: 4, H: 4, D: layers}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if !bytes.Equal(slicesMem, texels) {
		t.Errorf("the slices: % x, want % x", slicesMem, texels)
	}
}

// Clears that Zink would render as attachments that they cannot be are
// refused: of colors of a depth surface, of depth of a color surface, and
// of a compressed texture; and clears of no texels do nothing.
func TestInvalidClears(t *testing.T) {
	const (
		formatZ24S8      = 19
		formatDXT1RGB    = 105
		formatETC2RGB8   = 269
		bindDepthStencil = 1 << 0
		clearDepth       = 1 << 0
		clearStencil     = 1 << 1
	)
	f := newFixture(t)
	depth, err := NewResource(5, ResourceArgs{Target: target2D, Format: formatZ24S8, Bind: bindDepthStencil,
		Width: width, Height: height, Depth: 1, ArraySize: 1}, make([]byte, width*height*4), nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer depth.DecRef()
	f.c.Attach(depth)
	colorSurf, depthSurf := f.id(), f.id()
	var s stream
	s.cmd(cmdCreateObject, objectSurface, colorSurf, f.rt.ID(), formatR8G8B8A8Unorm, 0, 0)
	s.cmd(cmdCreateObject, objectSurface, depthSurf, depth.ID(), formatZ24S8, 0, 0)
	f.submit(t, s)
	clearSurface := func(s *stream, buffers, surf, w, h uint32) {
		s.cmd(cmdClearSurface, 0, buffers<<1, surf, 0, 0, 0, 0, 0, 0, w, h)
	}
	cases := []struct {
		name string
		s    func(*stream)
	}{
		{"a color clear of a depth surface", func(s *stream) { clearSurface(s, clearColor0, depthSurf, width, height) }},
		{"a depth clear of a color surface", func(s *stream) { clearSurface(s, clearDepth, colorSurf, width, height) }},
	}
	for _, format := range []uint32{formatETC2RGB8, formatDXT1RGB} {
		if !sampled(t, format) {
			continue
		}
		r, err := NewResource(6, ResourceArgs{Target: target2D, Format: format, Bind: bindSamplerView,
			Width: 16, Height: 16, Depth: 1, ArraySize: 1}, make([]byte, 128), nil)
		if err != nil {
			t.Fatalf("NewResource: %v", err)
		}
		defer r.DecRef()
		f.c.Attach(r)
		cases = append(cases, struct {
			name string
			s    func(*stream)
		}{"a clear of a compressed texture", func(s *stream) { s.cmd(cmdClearTexture, 0, r.ID(), 0, 0, 0, 0, 16, 16, 1, 0, 0, 0, 0) }})
		break
	}
	for _, tc := range cases {
		s = nil
		tc.s(&s)
		if err := f.c.Submit(s.bytes()); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
	// The surfaces' own clears, and clears of no texels.
	s = nil
	clearSurface(&s, clearColor0, colorSurf, width, height)
	clearSurface(&s, clearDepth|clearStencil, depthSurf, width, height)
	clearSurface(&s, clearColor0, colorSurf, 0, height)
	s.cmd(cmdClearTexture, 0, f.rt.ID(), 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0)
	f.submit(t, s)
}

// A vertex buffer bound at an offset past its end, of which Zink would
// bind the bytes of an underflowed size, is no buffer: the draw reads none
// of it.
func TestVertexBufferPastItsEnd(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	s.cmd(cmdSetVertexBuffers, 0, 16, 1<<20, f.vb.ID())
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	if p := f.pixel(4, height/2); p != red {
		t.Errorf("where the triangle would be: %v, want %v", p, red)
	}
}

// TestIndexedDrawFromStart draws indexes from a start, as Mesa's virgl
// driver sends a draw of them: of the index buffer's offset of the start,
// and the start, which virglrenderer ignores of indexed draws. The
// renderer took both, and drew the indexes past the draw's: of Android's
// quick settings, slivers in place of their tiles.
func TestIndexedDrawFromStart(t *testing.T) {
	f := newFixture(t)
	const bindIndexBuffer = 1 << 5
	// Indexes of 16 bits: a degenerate triangle, the triangle, and another
	// degenerate one.
	var indexMem []byte
	for _, i := range []uint16{0, 0, 0, 0, 1, 2, 0, 0, 0} {
		indexMem = binary.LittleEndian.AppendUint16(indexMem, i)
	}
	ib, err := NewResource(6, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindIndexBuffer, Width: uint32(len(indexMem)), Height: 1, Depth: 1, ArraySize: 1}, indexMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer ib.DecRef()
	f.c.Attach(ib)
	if err := f.c.Transfer(ib, TransferArgs{ToHost: true, W: uint32(len(indexMem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	f.setup(&s)
	clearRed(&s)
	s.cmd(cmdSetIndexBuffer, 0, ib.ID(), 2, 3*2)
	s.cmd(cmdDrawVBO, 0, 3, 3, primTriangles, 1, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

// TestDestroyReleasesBindings checks that a destroyed context holds none
// of the resources that its state bound: of vertex and uniform buffers,
// which it held until the backend freed them, never.
func TestDestroyReleasesBindings(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	s.cmd(cmdSetUniformBuffer, 0, stageFragment, 1, 0, 16, f.vb.ID())
	sampler := f.id()
	s.cmd(cmdCreateObject, objectSamplerState, sampler, 0, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdBindSamplerStates, 0, stageFragment, 0, sampler)
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.c.Flush().Wait()
	vb, ok := f.vb.res.(*vkglResource)
	if !ok {
		t.Skipf("a resource of %T", f.vb.res)
	}
	f.c.Destroy()
	// The guest's resource's reference, alone.
	if n := vb.refs.Load(); n != 1 {
		t.Errorf("the vertex buffer has %d references, want 1", n)
	}
}

// buffer returns a buffer resource of mem, attached to f's context, and
// transferred to the host.
func (f *fixture) buffer(t *testing.T, mem []byte) *Resource {
	t.Helper()
	r, err := NewResource(f.id(), ResourceArgs{Target: targetBuffer, Format: formatR8Unorm, Bind: bindShaderBuffer,
		Width: uint32(len(mem)), Height: 1, Depth: 1, ArraySize: 1}, mem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	t.Cleanup(r.DecRef)
	f.c.Attach(r)
	if err := f.c.Transfer(r, TransferArgs{ToHost: true, W: uint32(len(mem)), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	return r
}

// readBuffer waits for what the context executed, and reads r back to
// its memory.
func (f *fixture) readBuffer(t *testing.T, r *Resource, size int) {
	t.Helper()
	f.c.Flush().Wait()
	if err := f.c.Transfer(r, TransferArgs{W: uint32(size), H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
}

const bindShaderBuffer = 1 << 14

// TestIsNaN tests floats of a buffer for NaNs, as Mesa's virgl driver
// compiles GLSL's isnan, of a comparison of each to itself, which Metal's
// fast math folds to false.
func TestIsNaN(t *testing.T) {
	f := newFixture(t)
	values := []uint32{0x7fc6ba82, f32(1), 0xfff5f113, 0x7f800000, 0xff800000, 0x7f800001, 0}
	var in []byte
	for _, v := range values {
		in = binary.LittleEndian.AppendUint32(in, v)
	}
	out := make([]byte, len(in))
	inBuf, outBuf := f.buffer(t, in), f.buffer(t, out)
	var s stream
	cs := f.id()
	s.shader(cs, stageCompute, `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 1
PROPERTY CS_FIXED_BLOCK_HEIGHT 1
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], BLOCK_ID
DCL BUFFER[0]
DCL BUFFER[1]
DCL TEMP[0..2], LOCAL
IMM[0] UINT32 {2, 1, 0, 0}
  0: SHL TEMP[0].x, SV[0].xxxx, IMM[0].xxxx
  1: LOAD TEMP[1].x, BUFFER[1], TEMP[0].xxxx
  2: FSNE_PRECISE TEMP[2].x, TEMP[1].xxxx, TEMP[1].xxxx
  3: AND TEMP[2].x, TEMP[2].xxxx, IMM[0].yyyy
  4: STORE BUFFER[0].x, TEMP[0].xxxx, TEMP[2].xxxx
  5: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, cs, stageCompute)
	s.cmd(cmdSetShaderBuffers, 0, stageCompute, 0, 0, uint32(len(out)), outBuf.ID(), 0, uint32(len(in)), inBuf.ID())
	s.cmd(cmdLaunchGrid, 0, 1, 1, 1, uint32(len(values)), 1, 1, 0, 0)
	s.cmd(cmdMemoryBarrier, 0, 0xffffffff)
	f.submit(t, s)
	f.readBuffer(t, outBuf, len(out))
	for i, v := range values {
		nan := uint32(0)
		if v&0x7fffffff > 0x7f800000 {
			nan = 1
		}
		if got := binary.LittleEndian.Uint32(out[4*i:]); got != nan {
			t.Errorf("isnan of %#x: %d, want %d", v, got, nan)
		}
	}
}

// TestShaderBuffersOfBindings reads a fragment shader's buffers as Mesa's
// virgl driver numbers them, from the least binding of their blocks, here
// 2, which it binds from slot 0, as virglrenderer reads them.
func TestShaderBuffersOfBindings(t *testing.T) {
	f := newFixture(t)
	color := func(c [4]float32) *Resource {
		var mem []byte
		for _, v := range c {
			mem = binary.LittleEndian.AppendUint32(mem, f32(v))
		}
		return f.buffer(t, mem)
	}
	blue, green := color([4]float32{0, 0, 1, 1}), color([4]float32{0, 1, 0, 1})
	var s stream
	f.setup(&s)
	fs := f.id()
	s.shader(fs, stageFragment, `FRAG
DCL OUT[0], COLOR
DCL BUFFER[2]
DCL BUFFER[3]
DCL TEMP[0]
IMM[0] UINT32 {0, 0, 0, 0}
  0: LOAD TEMP[0], BUFFER[3], IMM[0].xxxx
  1: MOV OUT[0], TEMP[0]
  2: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	s.cmd(cmdSetShaderBuffers, 0, stageFragment, 0, 0, 16, blue.ID(), 0, 16, green.ID())
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

// TestSampleMaskOfSingleSamples draws the triangle of a fragment shader
// that writes its sample mask, of a framebuffer of a sample each pixel,
// whose masks OpenGL ignores, and Vulkan does not.
func TestSampleMaskOfSingleSamples(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		mask uint32
		want [4]byte
	}{{0, green}, {2, green}, {1, green}} {
		var s stream
		f.setup(&s)
		fs := f.id()
		s.shader(fs, stageFragment, fmt.Sprintf(`FRAG
DCL OUT[0], COLOR
DCL OUT[1], SAMPLEMASK
IMM[0] FLT32 {    0.0000,     1.0000,     0.0000,     1.0000}
IMM[1] UINT32 {%d, 0, 0, 0}
  0: MOV OUT[0], IMM[0]
  1: MOV OUT[1].x, IMM[1].xxxx
  2: END
`, tc.mask), 1<<16)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		clearRed(&s)
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		if p := f.pixel(4, height/2); p != tc.want {
			t.Errorf("of a mask of %#x: %v, want %v", tc.mask, p, tc.want)
		}
	}
}

// TestLaunchGrid launches a compute shader's grid, of a block of 64
// invocations, which store their thread's IDs, thrice, to a shader buffer:
// of a grid of the payload's, and of an indirect grid of a buffer's. A
// grid of blocks other than its shader's is refused.
func TestLaunchGrid(t *testing.T) {
	f := newFixture(t)
	mem := make([]byte, 2*64*4)
	buf := f.buffer(t, mem)
	grid := f.buffer(t, []byte{0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0})
	var s stream
	cs := f.id()
	s.shader(cs, stageCompute, `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 64
PROPERTY CS_FIXED_BLOCK_HEIGHT 1
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL BUFFER[0]
DCL TEMP[0], LOCAL
IMM[0] UINT32 {2, 3, 0, 0}
  0: SHL TEMP[0].x, SV[0].xxxx, IMM[0].xxxx
  1: UMUL TEMP[0].y, SV[0].xxxx, IMM[0].yyyy
  2: STORE BUFFER[0].x, TEMP[0].xxxx, TEMP[0].yyyy
  3: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, cs, stageCompute)
	// Of the buffer's first half, and of its second, of a grid of a
	// buffer's, from its fourth byte.
	s.cmd(cmdSetShaderBuffers, 0, stageCompute, 0, 0, 64*4, buf.ID())
	s.cmd(cmdLaunchGrid, 0, 64, 1, 1, 1, 1, 1, 0, 0)
	s.cmd(cmdMemoryBarrier, 0, 0xffffffff)
	s.cmd(cmdSetShaderBuffers, 0, stageCompute, 0, 64*4, 64*4, buf.ID())
	s.cmd(cmdLaunchGrid, 0, 64, 1, 1, 0, 0, 0, grid.ID(), 4)
	s.cmd(cmdMemoryBarrier, 0, 0xffffffff)
	f.submit(t, s)
	f.readBuffer(t, buf, len(mem))
	for i := 0; i < 2*64; i++ {
		if got, want := binary.LittleEndian.Uint32(mem[4*i:]), uint32(i%64*3); got != want {
			t.Fatalf("word %d: %d, want %d", i, got, want)
		}
	}
	var bad stream
	bad.cmd(cmdLaunchGrid, 0, 32, 1, 1, 1, 1, 1, 0, 0)
	if err := f.c.Submit(bad.bytes()); err == nil {
		t.Errorf("a grid of blocks of 32 of a shader of 64 was launched")
	}
}

// TestShaderImages stores a color to each texel of a texture, of a
// compute shader's image, and reads one back of another image.
func TestShaderImages(t *testing.T) {
	f := newFixture(t)
	mem := make([]byte, 8*8*4)
	tex, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatR8G8B8A8Unorm,
		Bind: bindSamplerView, Width: 8, Height: 8, Depth: 1, ArraySize: 1}, mem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer tex.DecRef()
	f.c.Attach(tex)
	var s stream
	cs := f.id()
	s.shader(cs, stageCompute, `COMP
PROPERTY CS_FIXED_BLOCK_WIDTH 8
PROPERTY CS_FIXED_BLOCK_HEIGHT 8
PROPERTY CS_FIXED_BLOCK_DEPTH 1
DCL SV[0], THREAD_ID
DCL IMAGE[0], 2D, PIPE_FORMAT_R8G8B8A8_UNORM, WR
DCL IMAGE[1], 2D, PIPE_FORMAT_R8G8B8A8_UNORM
DCL TEMP[0], LOCAL
IMM[0] FLT32 {    0.0000,     1.0000,     0.0000,     1.0000}
  0: LOAD TEMP[0], IMAGE[1], SV[0].xyyy, 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  1: ADD TEMP[0], TEMP[0], IMM[0]
  2: STORE IMAGE[0], SV[0].xyyy, TEMP[0], 2D, PIPE_FORMAT_R8G8B8A8_UNORM
  3: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, cs, stageCompute)
	// Of the texture as image 0, its first layer and level, writable, and
	// of none as image 1, which reads zeros.
	s.cmd(cmdSetShaderImages, 0, stageCompute, 0, formatR8G8B8A8Unorm, 2, 0, 0, tex.ID(), 0, 0, 0, 0, 0)
	s.cmd(cmdLaunchGrid, 0, 8, 8, 1, 1, 1, 1, 0, 0)
	s.cmd(cmdMemoryBarrier, 0, 0xffffffff)
	f.submit(t, s)
	f.c.Flush().Wait()
	if err := f.c.Transfer(tex, TransferArgs{W: 8, H: 8, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	for i := 0; i < len(mem); i += 4 {
		if p := [4]byte(mem[i : i+4]); p != green {
			t.Fatalf("texel %d: %v, want %v", i/4, p, green)
		}
	}
	// Of a layer past the texture's.
	var bad stream
	bad.cmd(cmdSetShaderImages, 0, stageCompute, 0, formatR8G8B8A8Unorm, 2, 1|1<<16, 0, tex.ID())
	if err := f.c.Submit(bad.bytes()); err == nil {
		t.Errorf("an image of a layer past its texture's was bound")
	}
}

// TestDrawIndirect draws the triangle of a command of a buffer's.
func TestDrawIndirect(t *testing.T) {
	f := newFixture(t)
	// VkDrawIndirectCommand: 3 vertices, of an instance, after 8 bytes.
	commands := f.buffer(t, []byte{0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	var s stream
	f.setup(&s)
	clearRed(&s)
	s.cmd(cmdDrawVBO, 0, 0, 0, primTriangles, 0, 0, 0, 0, 0, 0, 0, 0xffffffff, 0, 0, 0, commands.ID(), 8, 16, 1, 0, 0)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
	// Of a command past its buffer.
	var bad stream
	bad.cmd(cmdDrawVBO, 0, 0, 0, primTriangles, 0, 0, 0, 0, 0, 0, 0, 0xffffffff, 0, 0, 0, commands.ID(), 12, 16, 1, 0, 0)
	if err := f.c.Submit(bad.bytes()); err == nil {
		t.Errorf("a draw of a command past its buffer was accepted")
	}
}

// TestFragmentShaderBuffer counts the triangle's fragments of an atomic of
// a fragment shader's shader buffer, as Mesa's atomic counters of shader
// buffers count.
func TestFragmentShaderBuffer(t *testing.T) {
	f := newFixture(t)
	mem := make([]byte, 16)
	counter := f.buffer(t, mem)
	var s stream
	f.setup(&s)
	fs := f.id()
	s.shader(fs, stageFragment, `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], COLOR
DCL BUFFER[0]
DCL TEMP[0]
IMM[0] UINT32 {0, 1, 0, 0}
IMM[1] FLT32 {    0.0000,     1.0000,     0.0000,     1.0000}
  0: ATOMUADD TEMP[0].x, BUFFER[0], IMM[0].xxxx, IMM[0].yyyy
  1: MOV OUT[0], IMM[1]
  2: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	s.cmd(cmdSetShaderBuffers, 0, stageFragment, 0, 0, 16, counter.ID())
	clearRed(&s)
	drawTriangle(&s)
	s.cmd(cmdMemoryBarrier, 0, 0xffffffff)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
	f.readBuffer(t, counter, len(mem))
	// The triangle's pixels, half of 32 by 64, of the rule of their
	// centers'.
	if got := binary.LittleEndian.Uint32(mem); got < 32*64/2-64 || got > 32*64/2+64 {
		t.Errorf("the triangle's fragments: %d, want about %d", got, 32*64/2)
	}
}

// TestStencilTexturing clears a depth and stencil texture of a framebuffer
// of it alone, and samples its stencil, as GL's stencil texturing does: of
// a view of its format of the stencil alone, X24S8_UINT.
func TestStencilTexturing(t *testing.T) {
	f := newFixture(t)
	const (
		formatZ24S8      = 19
		formatX24S8      = 136
		bindDepthStencil = 1 << 0
		clearDepth       = 1 << 0
		clearStencil     = 1 << 1
	)
	zs, err := NewResource(f.id(), ResourceArgs{Target: target2D, Format: formatZ24S8, Bind: bindDepthStencil | bindSamplerView,
		Width: width, Height: height, Depth: 1, ArraySize: 1}, make([]byte, width*height*4), nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer zs.DecRef()
	f.c.Attach(zs)
	var s stream
	surf := f.id()
	s.cmd(cmdCreateObject, objectSurface, surf, zs.ID(), formatZ24S8, 0, 0)
	s.cmd(cmdSetFramebufferState, 0, 0, surf)
	s.cmd(cmdSetFramebufferNoAttach, 0, width|height<<16, 0)
	depth := math.Float64bits(0.5)
	s.cmd(cmdClear, 0, clearDepth|clearStencil, 0, 0, 0, 0, uint32(depth), uint32(depth>>32), 0x5a)
	f.setup(&s)
	clearRed(&s)
	fs, view, samp := f.id(), f.id(), f.id()
	s.shader(fs, stageFragment, `FRAG
DCL IN[0], POSITION, LINEAR
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, UINT
DCL TEMP[0..1]
IMM[0] UINT32 {0, 1132396544, 1065353216, 0}
  0: F2I TEMP[0].xy, IN[0].xyyy
  1: MOV TEMP[0].zw, IMM[0].xxxx
  2: TXF TEMP[1], TEMP[0], SAMP[0], 2D
  3: U2F TEMP[1].x, TEMP[1].xxxx
  4: DIV OUT[0].x, TEMP[1].xxxx, IMM[0].yyyy
  5: MOV OUT[0].yzw, IMM[0].xxxz
  6: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	const swizzleRGBA = 0 | 1<<3 | 2<<6 | 3<<9
	s.cmd(cmdCreateObject, objectSamplerView, view, zs.ID(), formatX24S8|target2D<<24, 0, 0, swizzleRGBA)
	s.cmd(cmdCreateObject, objectSamplerState, samp, 0, 0, 0, 0, 0, 0, 0, 0)
	s.cmd(cmdSetSamplerViews, 0, stageFragment, 0, view)
	s.cmd(cmdBindSamplerStates, 0, stageFragment, 0, samp)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	if p := f.pixel(4, height/2); p != [4]byte{0x5a, 0, 0, 255} {
		t.Errorf("the stencil sampled: %v, want %v", p, [4]byte{0x5a, 0, 0, 255})
	}
}

// TestUniformBufferRanges binds uniform buffers' ranges that Vulkan binds
// none of as they are: of no bytes, which shaders read zeros of, and past
// their buffers, which the renderer refuses.
func TestUniformBufferRanges(t *testing.T) {
	f := newFixture(t)
	uboMem := make([]byte, 32)
	for i := range uboMem {
		uboMem[i] = 0xff
	}
	ubo, err := NewResource(f.id(), ResourceArgs{Target: targetBuffer, Format: formatR8Unorm, Bind: 1 << 6,
		Width: 32, Height: 1, Depth: 1, ArraySize: 1}, uboMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer ubo.DecRef()
	f.c.Attach(ubo)
	if err := f.c.Transfer(ubo, TransferArgs{ToHost: true, W: 32, H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	f.setup(&s)
	fs := f.id()
	s.shader(fs, stageFragment, `FRAG
DCL OUT[0], COLOR
DCL CONST[1][0]
  0: MOV OUT[0], CONST[1][0]
  1: END
`, 1<<16)
	s.cmd(cmdBindShader, 0, fs, stageFragment)
	s.cmd(cmdSetUniformBuffer, 0, stageFragment, 1, 32, 0, ubo.ID())
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	if p := f.pixel(4, height/2); p != [4]byte{} {
		t.Errorf("of a uniform buffer of no bytes: %v, want zeros", p)
	}
	var bad stream
	bad.cmd(cmdSetUniformBuffer, 0, stageFragment, 1, 0, 64, ubo.ID())
	if err := f.c.Submit(bad.bytes()); err == nil {
		t.Errorf("a uniform buffer of 64 bytes of a buffer of 32 was bound")
	}
}

// Shaders read their constants, which the guest sets, and uniform buffers,
// of resources, as Mesa's state tracker binds them: the vertex shader's
// offsets the triangle, and the fragment shader's color it.
func TestConstantsAndUniformBuffers(t *testing.T) {
	const (
		constVS = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL CONST[0][0]
  0: ADD OUT[0], IN[0], CONST[0][0]
  1: END
`
		constFS = `FRAG
DCL OUT[0], COLOR
DCL CONST[%d][0..1]
  0: MOV OUT[0], CONST[%d][1]
  1: END
`
	)
	floats := func(fs ...float32) []uint32 {
		var d []uint32
		for _, f := range fs {
			d = append(d, f32(f))
		}
		return d
	}
	f := newFixture(t)
	// A uniform buffer of an offset of 0, and green.
	uboMem := make([]byte, 32)
	for i, w := range floats(0, 0, 0, 0, 0, 1, 0, 1) {
		binary.LittleEndian.PutUint32(uboMem[4*i:], w)
	}
	ubo, err := NewResource(5, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm, Bind: 1 << 6,
		Width: 32, Height: 1, Depth: 1, ArraySize: 1}, uboMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer ubo.DecRef()
	f.c.Attach(ubo)
	if err := f.c.Transfer(ubo, TransferArgs{ToHost: true, W: 32, H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	for _, tc := range []struct {
		name string
		set  func(s *stream, fs uint32)
	}{
		{"constants", func(s *stream, fs uint32) {
			s.shader(fs, stageFragment, fmt.Sprintf(constFS, 0, 0), 1<<16)
			s.cmd(cmdSetConstantBuffer, 0, append([]uint32{stageFragment, 0}, floats(0, 0, 0, 0, 0, 1, 0, 1)...)...)
		}},
		{"a uniform buffer", func(s *stream, fs uint32) {
			s.shader(fs, stageFragment, fmt.Sprintf(constFS, 1, 1), 1<<16)
			s.cmd(cmdSetUniformBuffer, 0, stageFragment, 1, 0, 32, ubo.ID())
		}},
		{"a uniform buffer of the constants' slot", func(s *stream, fs uint32) {
			s.shader(fs, stageFragment, fmt.Sprintf(constFS, 0, 0), 1<<16)
			s.cmd(cmdSetUniformBuffer, 0, stageFragment, 0, 0, 32, ubo.ID())
		}},
	} {
		var s stream
		f.setup(&s)
		vs, fs := f.id(), f.id()
		s.shader(vs, stageVertex, constVS, 1<<16)
		s.cmd(cmdSetConstantBuffer, 0, append([]uint32{stageVertex, 0}, floats(0, 0, 0, 0)...)...)
		tc.set(&s, fs)
		s.cmd(cmdBindShader, 0, vs, stageVertex)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		clearRed(&s)
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		if p := f.pixel(4, height/2); p != green {
			t.Errorf("%s: inside the triangle: %v, want %v", tc.name, p, green)
		}
		if p := f.pixel(width-4, height/2); p != red {
			t.Errorf("%s: outside the triangle: %v, want %v", tc.name, p, red)
		}
	}
}

// A triangle of OpenGL's clip space's z between -w and 0, which Vulkan's
// clips, as SurfaceFlinger's orthographic projections place layers at -1,
// is drawn.
func TestGLClipSpaceZ(t *testing.T) {
	f := newFixture(t)
	vbMem := make([]byte, 48)
	for i, v := range []float32{-1, -1, -0.9, 1, 0, -1, -0.9, 1, -1, 1, -0.9, 1} {
		binary.LittleEndian.PutUint32(vbMem[4*i:], math.Float32bits(v))
	}
	vb, err := NewResource(5, ResourceArgs{Target: targetBuffer, Format: formatR8Unorm,
		Bind: bindVertexBuffer, Width: 48, Height: 1, Depth: 1, ArraySize: 1}, vbMem, nil)
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	defer vb.DecRef()
	f.c.Attach(vb)
	if err := f.c.Transfer(vb, TransferArgs{ToHost: true, W: 48, H: 1, D: 1}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	var s stream
	f.setup(&s)
	s.cmd(cmdSetVertexBuffers, 0, 16, 0, vb.ID())
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

func TestInvalidCommands(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		s    func(s *stream)
	}{
		{"truncated", func(s *stream) { *s = append(*s, cmdClear|8<<16, 0) }},
		{"unknown command", func(s *stream) { s.cmd(200, 0) }},
		{"unknown resource", func(s *stream) { s.cmd(cmdCreateObject, objectSurface, 7, 999, formatR8G8B8A8Unorm, 0, 0) }},
		{"blit of layers beyond the target", func(s *stream) {
			s.cmd(cmdBlit, 0, 0xf, 0, 0, f.rt.ID(), 0, formatR8G8B8A8Unorm, 0, 0, 0, width, height, 2,
				f.rt.ID(), 0, formatR8G8B8A8Unorm, 0, 0, 0, width, height, 1)
		}},
		{"transfer beyond the memory", func(s *stream) {
			s.cmd(cmdTransfer3D, 0, f.rt.ID(), 0, 0, 0, 0, 0, 0, 0, width, height, 1, 4, transferFromHost)
		}},
		{"layers beyond the texture", func(s *stream) {
			s.cmd(cmdCreateObject, objectSurface, 7, f.rt.ID(), formatR8G8B8A8Unorm, 0, 1<<16)
		}},
		{"shader continued out of order", func(s *stream) {
			s.cmd(cmdCreateObject, objectShader, 8, stageVertex, shaderOffsetContinuation|4, 300, 0, 0)
		}},
		{"invalid TGSI", func(s *stream) { s.shader(9, stageVertex, "VERT\nNOT TGSI\n", 1<<16) }},
		// What Zink or Metal trapped on.
		{"TGSI of an undeclared register", func(s *stream) {
			s.shader(f.id(), stageVertex, "VERT\nDCL OUT[0], POSITION\n  0: MOV OUT[0], IN[0]\n  1: END\n", 1<<16)
		}},
		{"TGSI of another stage", func(s *stream) { s.shader(f.id(), stageVertex, greenShader, 1<<16) }},
		{"blend factor of no factor", func(s *stream) {
			s.cmd(cmdCreateObject, objectBlend, f.id(), 0, 0, 1|0xf<<27, 0, 0, 0, 0, 0, 0, 0)
		}},
		{"view in a format of another size", func(s *stream) {
			s.cmd(cmdCreateObject, objectSamplerView, f.id(), f.rt.ID(), formatR32G32B32A32Float, 0, 0, 0|1<<3|2<<6|3<<9)
		}},
		{"texture as a vertex buffer", func(s *stream) { s.cmd(cmdSetVertexBuffers, 0, 16, 0, f.rt.ID()) }},
		{"buffer as a color attachment", func(s *stream) {
			surf := f.id()
			s.cmd(cmdCreateObject, objectSurface, surf, f.vb.ID(), formatR8Unorm, 0, 15)
			s.cmd(cmdSetFramebufferState, 0, 1, 0, surf)
		}},
	} {
		var s stream
		tc.s(&s)
		if err := f.c.Submit(s.bytes()); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
	// Draws without shaders are dropped, as virglrenderer drops them.
	var s stream
	drawTriangle(&s)
	f.submit(t, s)
	// The backend draws no line loops, which guests' drivers convert, as
	// the capabilities ask. Nor did the errors keep the context from rendering.
	s = nil
	f.setup(&s)
	clearRed(&s)
	s.cmd(cmdDrawVBO, 0, 0, 3, primLineLoop, 0, 1, 0, 0, 0, 0, 0, 0xffffffff, 0)
	if err := f.c.Submit(s.bytes()); err == nil {
		t.Errorf("line loop: no error")
	}
	s = nil
	drawTriangle(&s)
	f.submit(t, s)
	f.finish(t)
	f.checkTriangle(t)
}

// TestUntranslatableTGSI creates shaders that tgsi_to_nir, and
// tgsi_scan_shader, with which Zink translated TGSI, aborted on, read or
// wrote past their arrays of, or that tgsi_sanity_check allocated without
// end of: the renderer translates them within its limits, which tospirv's
// tests check, or refuses them, and renders on.
func TestUntranslatableTGSI(t *testing.T) {
	f := newFixture(t)
	const stageTessCtrl = 3
	const greenImm = "IMM[0] FLT32 {0x00000000, 0x3f800000, 0x00000000, 0x3f800000}\n"
	for _, tc := range []struct {
		name  string
		stage uint32
		text  string
	}{
		{"an unknown property", stageFragment, "FRAG\nPROPERTY FS_POST_DEPTH_COVERAGE 1\nDCL OUT[0], COLOR\n" +
			greenImm + "  0: MOV OUT[0], IMM[0]\n  1: END\n"},
		{"a geometry shader", stageGeometry, "GEOM\nPROPERTY GS_INPUT_PRIMITIVE TRIANGLES\n" +
			"PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP\nPROPERTY GS_MAX_OUTPUT_VERTICES 3\n" +
			"DCL IN[][0], POSITION\nDCL OUT[0], POSITION\nIMM[0] INT32 {0, 0, 0, 0}\n" +
			"  0: MOV OUT[0], IN[0][0]\n  1: EMIT IMM[0].xxxx\n  2: END\n"},
		{"a tessellation control shader of 2^31 vertices", stageTessCtrl,
			"TESS_CTRL\nPROPERTY TCS_VERTICES_OUT 2147483647\nDCL OUT[][0], GENERIC[0]\n  0: END\n"},
		{"a color beyond the targets", stageFragment, "FRAG\nDCL OUT[0], COLOR[9]\n" + greenImm +
			"  0: MOV OUT[0], IMM[0]\n  1: END\n"},
		{"a uniform buffer beyond tgsi_scan_shader's", stageVertex,
			"VERT\nDCL IN[0]\nDCL OUT[0], POSITION\nDCL CONST[40][0]\n  0: MOV OUT[0], CONST[40][0]\n  1: END\n"},
		{"a sampler beyond tgsi_to_nir's", stageFragment, "FRAG\nDCL IN[0], GENERIC[0], PERSPECTIVE\n" +
			"DCL OUT[0], COLOR\nDCL SAMP[40]\nDCL SVIEW[40], 2D, FLOAT\n  0: TEX OUT[0], IN[0], SAMP[40], 2D\n  1: END\n"},
		{"an input beyond tgsi_scan_shader's", stageFragment, "FRAG\nDCL IN[100], GENERIC[0], PERSPECTIVE\n" +
			"DCL OUT[0], COLOR\n  0: MOV OUT[0], IN[100]\n  1: END\n"},
		{"a vertex input beyond tgsi_to_nir's", stageVertex,
			"VERT\nDCL IN[20]\nDCL OUT[0], POSITION\n  0: MOV OUT[0], IN[20]\n  1: END\n"},
		// TGSI's indexes are of 16 bits, signed: 40000 is -25536.
		{"a register of a negative index", stageFragment, "FRAG\nDCL OUT[0], COLOR\n" +
			"DCL TEMP[0..3], ARRAY(1)\nDCL ADDR[0]\nIMM[0] INT32 {1, 0, 0, 0}\n" +
			"  0: UARL ADDR[0].x, IMM[0].xxxx\n  1: MOV OUT[0], TEMP[ADDR[0].x+40000]\n  2: END\n"},
		{"an unknown system value", stageFragment, "FRAG\nDCL SV[0], SAMPLEPOS\nDCL OUT[0], COLOR\n" +
			"  0: MOV OUT[0], SV[0]\n  1: END\n"},
		{"shared memory", stageCompute, "COMP\nPROPERTY CS_FIXED_BLOCK_WIDTH 1\n" +
			"PROPERTY CS_FIXED_BLOCK_HEIGHT 1\nPROPERTY CS_FIXED_BLOCK_DEPTH 1\nDCL MEMORY[0], SHARED\n" +
			"DCL TEMP[0]\nIMM[0] UINT32 {0, 0, 0, 0}\n  0: LOAD TEMP[0].x, MEMORY[0], IMM[0].xxxx\n  1: END\n"},
		{"ENDIF of no IF", stageFragment, "FRAG\nDCL OUT[0], COLOR\n" + greenImm +
			"  0: MOV OUT[0], IMM[0]\n  1: ENDIF\n  2: END\n"},
		{"TEX of a shadow cube array", stageFragment, "FRAG\nDCL IN[0], GENERIC[0], PERSPECTIVE\n" +
			"DCL OUT[0], COLOR\nDCL SAMP[0]\nDCL SVIEW[0], SHADOWCUBEARRAY, FLOAT\n" +
			"  0: TEX OUT[0], IN[0], SAMP[0], SHADOWCUBEARRAY\n  1: END\n"},
		{"TG4 of a component of a register", stageFragment, "FRAG\nDCL IN[0], GENERIC[0], PERSPECTIVE\n" +
			"DCL OUT[0], COLOR\nDCL SAMP[0]\nDCL SVIEW[0], 2D, FLOAT\nDCL TEMP[0]\n" +
			"  0: MOV TEMP[0], IN[0]\n  1: TG4 OUT[0], IN[0], TEMP[0].xxxx, SAMP[0], 2D\n  2: END\n"},
		{"an indirect temporary of no array", stageFragment, "FRAG\nDCL OUT[0], COLOR\nDCL TEMP[0..3]\n" +
			"DCL ADDR[0]\nIMM[0] INT32 {1, 0, 0, 0}\n  0: UARL ADDR[0].x, IMM[0].xxxx\n" +
			"  1: MOV OUT[0], TEMP[ADDR[0].x]\n  2: END\n"},
		// Which tgsi_sanity_check does not check.
		{"an offset of an immediate beyond the shader's", stageFragment,
			"FRAG\nDCL IN[0], GENERIC[0], PERSPECTIVE\nDCL OUT[0], COLOR\nDCL SAMP[0]\n" +
				"DCL SVIEW[0], 2D, FLOAT\nIMM[0] INT32 {1, 1, 0, 0}\n" +
				"  0: TEX OUT[0], IN[0], SAMP[0], 2D, IMM[3].xyz\n  1: END\n"},
		{"BRK out of loops", stageFragment, "FRAG\nDCL OUT[0], COLOR\n" + greenImm +
			"  0: MOV OUT[0], IMM[0]\n  1: BRK\n  2: END\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s stream
			s.shader(f.id(), tc.stage, tc.text, 1<<16)
			t.Logf("%v", f.c.Submit(s.bytes()))
		})
	}
	// A property that the shader's code has, which tgsi_to_nir took for
	// a meaning that it has not, is the shader's; early fragment tests,
	// which it has not, are the NIR's.
	for _, property := range []string{"FS_BLEND_EQUATION_ADVANCED 65534", "FS_EARLY_DEPTH_STENCIL 1"} {
		var s stream
		f.setup(&s)
		fs := f.id()
		s.shader(fs, stageFragment, "FRAG\nPROPERTY "+property+"\nDCL OUT[0], COLOR\n"+greenImm+
			"  0: MOV OUT[0], IMM[0]\n  1: END\n", 1<<16)
		s.cmd(cmdBindShader, 0, fs, stageFragment)
		clearRed(&s)
		drawTriangle(&s)
		f.submit(t, s)
		f.finish(t)
		f.checkTriangle(t)
	}
}

// TestTGSICorpus creates a shader of each of $VIRGL_TGSI_CORPUS, a
// directory of the TGSI texts, *.tgsi, that a renderer's guests sent, and
// reports those that the renderer refuses, by their errors.
func TestTGSICorpus(t *testing.T) {
	dir := os.Getenv("VIRGL_TGSI_CORPUS")
	if dir == "" {
		t.Skip("VIRGL_TGSI_CORPUS names no corpus")
	}
	f := newFixture(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.tgsi"))
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]uint32{"VERT": stageVertex, "FRAG": stageFragment, "GEOM": stageGeometry,
		"TESS_CTRL": 3, "TESS_EVAL": stageTessEval, "COMP": stageCompute}
	refused := make(map[string][]string)
	n := 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		header, _, _ := strings.Cut(text, "\n")
		var s stream
		// The corpus has not the guests' counts of their tokens, of which
		// each takes a few bytes of text at least.
		s.shaderTokens(f.id(), stages[header], text, 1<<16, uint32(len(text)))
		if err := f.c.Submit(s.bytes()); err != nil {
			msg := err.Error()
			if i := strings.Index(msg, ": shader "); i >= 0 {
				_, msg, _ = strings.Cut(msg[i+len(": shader "):], ": ")
			}
			msg = header + ": " + msg
			refused[msg] = append(refused[msg], filepath.Base(path))
			n++
		}
	}
	for msg, names := range refused {
		t.Errorf("%d refused, %s: %v", len(names), msg, names[:min(len(names), 4)])
	}
	t.Logf("%d shaders, %d refused", len(files), n)
}

func TestDestroyWithFenceInFlight(t *testing.T) {
	f := newFixture(t)
	var s stream
	f.setup(&s)
	clearRed(&s)
	drawTriangle(&s)
	f.submit(t, s)
	fence := f.c.Flush()
	f.c.Destroy()
	fence.Wait()
	if err := f.c.Submit(s.bytes()); err == nil {
		t.Errorf("a destroyed context took commands")
	}
}
