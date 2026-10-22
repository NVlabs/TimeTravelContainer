// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"encoding/binary"
	"math"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// contextTriangle returns a function that draws, in c, of a framebuffer of
// 16 by 16, a triangle over its lower left half, of 120 to 136 samples.
func contextTriangle(t *testing.T, d *Device, c *Context) func() {
	t.Helper()
	tex, err := d.NewTexture(ResourceDesc{Target: Target2D, Format: virglR8G8B8A8Unorm, Width: 16, Height: 16})
	if err != nil {
		t.Fatalf("NewTexture: %v", err)
	}
	t.Cleanup(func() { d.DestroyTexture(tex) })
	s, err := d.NewSurface(tex, virglR8G8B8A8Unorm, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	t.Cleanup(func() { d.DestroySurface(s) })
	c.SetFramebuffer(Framebuffer{Colors: []*Surface{s}})
	vs := translate(t, passVS, tospirv.Options{})
	fs := translate(t, colorFS, tospirv.Options{ColorBuffers: 1})
	Link(vs, fs)
	p, err := d.NewPipeline(PipelineDesc{
		VS: vs.Words, FS: fs.Words,
		Bindings: []VertexBinding{{Binding: 0, Stride: 32}},
		Inputs: []VertexInput{{Location: 0, Binding: 0, Format: formatR32G32B32A32Sfloat},
			{Location: 1, Binding: 0, Format: formatR32G32B32A32Sfloat, Offset: 16}},
		Topology:   3, // VK_PRIMITIVE_TOPOLOGY_TRIANGLE_LIST
		Rasterizer: &Rasterizer{DepthClip: true}, DSA: &DSA{}, Blend: &Blend{},
		ColorFormats: []uint32{formatR8G8B8A8Unorm}, Samples: 1, SampleMask: ^uint32(0),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	t.Cleanup(func() { d.DestroyPipeline(p) })
	var vertices []byte
	for _, v := range [][8]float32{{-1, -1, 0, 1, 1, 0, 0, 1}, {1, -1, 0, 1, 1, 0, 0, 1}, {-1, 1, 0, 1, 1, 0, 0, 1}} {
		for _, f := range v {
			vertices = binary.LittleEndian.AppendUint32(vertices, math.Float32bits(f))
		}
	}
	return func() {
		t.Helper()
		vb, err := c.Upload(vertices)
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if err := c.Render(func(cmds *Commands) error {
			cmds.BindPipeline(p)
			cmds.SetViewport(0, 0, 16, 16, 0, 1)
			cmds.SetScissor(0, 0, 16, 16)
			cmds.SetBlendConstants([4]float32{})
			cmds.SetStencilReference(0, 0)
			var push [tospirv.PushConstantsSize]byte
			cmds.PushConstants(p, 0, push[:])
			cmds.BindVertexBuffers(0, []*Buffer{vb}, []uint64{0})
			cmds.DrawVertices(3, 1, 0, 0)
			return nil
		}); err != nil {
			t.Fatalf("Render: %v", err)
		}
	}
}

// TestContextQueries counts samples in occlusion queries, of draws across
// flushes and of queries active at once, as Vulkan's are not, and times
// draws.
func TestContextQueries(t *testing.T) {
	d := newTestDevice(t)
	c, err := d.NewContext()
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	defer c.Destroy()
	draw := contextTriangle(t, d, c)
	newQuery := func(kind int) *Query {
		t.Helper()
		q, err := d.NewQuery(kind)
		if err != nil {
			t.Fatalf("NewQuery(%d): %v", kind, err)
		}
		return q
	}
	begin := func(q *Query) {
		t.Helper()
		if err := c.BeginQuery(q); err != nil {
			t.Fatalf("BeginQuery: %v", err)
		}
	}
	end := func(q *Query) {
		t.Helper()
		if err := c.EndQuery(q); err != nil {
			t.Fatalf("EndQuery: %v", err)
		}
	}
	result := func(q *Query) uint64 {
		t.Helper()
		v, ok := c.QueryResult(q, true)
		if !ok {
			t.Fatalf("no result of a query of kind %d", q.Kind)
		}
		return v
	}

	one := newQuery(QueryOcclusion)
	begin(one)
	draw()
	if _, ok := c.QueryResult(one, false); ok {
		t.Errorf("the result of a query that counts")
	}
	end(one)
	if _, ok := c.QueryResult(one, false); ok {
		t.Errorf("the result of a query of commands that the context did not submit")
	}
	n := result(one)
	if n < 120 || n > 136 {
		t.Fatalf("a triangle's samples: %d", n)
	}

	// Of draws across flushes, of queries active at once.
	a, b := newQuery(QueryOcclusion), newQuery(QueryOcclusion)
	begin(a)
	draw()
	c.Flush()
	begin(b)
	draw()
	end(a)
	draw()
	c.Flush()
	end(b)
	if got := result(a); got != 2*n {
		t.Errorf("a query of two draws across a flush: %d, want %d", got, 2*n)
	}
	if got := result(b); got != 2*n {
		t.Errorf("a query of two draws, one of another query's: %d, want %d", got, 2*n)
	}
	// Begun again, a query counts from then on.
	begin(a)
	end(a)
	if got := result(a); got != 0 {
		t.Errorf("a query of no draws: %d", got)
	}
	// A query let go while it counts.
	gone := newQuery(QueryOcclusion)
	begin(gone)
	draw()
	c.DestroyQuery(gone)
	c.Flush().Wait()

	if _, err := d.NewQuery(QueryTimestamp); err != nil {
		t.Logf("no timestamps: %v", err)
		return
	}
	ts, elapsed, finished := newQuery(QueryTimestamp), newQuery(QueryTimeElapsed), newQuery(QueryFinished)
	begin(elapsed)
	draw()
	end(elapsed)
	end(ts)
	end(finished)
	if got := result(ts); got == 0 {
		t.Errorf("a timestamp of 0")
	}
	if got := result(elapsed); got > 10e9 {
		t.Errorf("a draw of %d ns", got)
	}
	if got := result(finished); got != 1 {
		t.Errorf("finished: %d", got)
	}
}
