// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

import (
	"bytes"
	"encoding/binary"
	"math"
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
	queryOcclusionCounter   = 0
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
	b := append([]byte(text), 0)
	for off := 0; off < len(b); off += part {
		end := min(off+part, len(b))
		offlen := uint32(len(b))
		if off > 0 {
			offlen = shaderOffsetContinuation | uint32(off)
		}
		payload := append([]uint32{handle, stage, offlen, 300, 0}, textDwords(b[off:end])...)
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
		t.Skipf("Zink is unavailable: %v", err)
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

// TestTransformFeedback captures a triangle's positions, which Zink cannot
// here, appends a second capture, pauses, and draws what it captured.
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
		t.Skipf("Zink is unavailable: %v", err)
	}
	capsets, err := Capsets()
	if err != nil {
		t.Fatalf("Capsets: %v", err)
	}
	if len(capsets) != 2 || capsets[0].ID != CapsetVirgl || capsets[1].ID != CapsetVirgl2 {
		t.Fatalf("capsets %+v", capsets)
	}
	// struct virgl_caps_v1 begins with max_version, and v2's renderer
	// string names Zink's device.
	v2 := capsets[1].Data
	if binary.LittleEndian.Uint32(v2) != 2 {
		t.Errorf("v2's max_version %d", binary.LittleEndian.Uint32(v2))
	}
	if !strings.Contains(string(v2), "zink") {
		t.Errorf("v2 names no renderer of Zink's")
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
	// Unbinding vertex buffers leaves Zink's slots as they are.
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

func TestInvalidCommands(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		s    func(s *stream)
	}{
		{"truncated", func(s *stream) { *s = append(*s, cmdClear|8<<16, 0) }},
		{"unknown command", func(s *stream) { s.cmd(200, 0) }},
		{"unknown resource", func(s *stream) { s.cmd(cmdCreateObject, objectSurface, 7, 999, formatR8G8B8A8Unorm, 0, 0) }},
		{"blit beyond the target", func(s *stream) {
			s.cmd(cmdBlit, 0, 0xf, 0, 0, f.rt.ID(), 0, formatR8G8B8A8Unorm, 0, 0, 0, width+1, height, 1,
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
	// Zink draws no line loops, which guests' drivers convert, as the
	// capabilities ask. Nor did the errors keep the context from rendering.
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
