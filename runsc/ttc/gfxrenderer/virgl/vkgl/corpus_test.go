// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// solidFS is a fragment shader of a color of its own, of no inputs.
const solidFS = `FRAG
DCL OUT[0], COLOR
IMM[0] FLT32 {    1.0000,     0.0000,     0.0000,     1.0000}
  0: MOV OUT[0], IMM[0]
  1: END
`

// TestCorpusPipelines compiles a pipeline of each vertex, fragment and
// compute shader of a corpus of guests' TGSI, the .tgsi files of the
// directory that VKGL_TGSI_CORPUS names, of a vertex or fragment shader's
// with a fragment or vertex shader of the test's, so that the device's
// compiler compiles every one, as draws and grids would. It leaves out
// those that read the framebuffer (FBFETCH), which vkgl's caps do not
// offer guests.
func TestCorpusPipelines(t *testing.T) {
	dir := os.Getenv("VKGL_TGSI_CORPUS")
	if dir == "" {
		t.Skip("VKGL_TGSI_CORPUS names no corpus")
	}
	d := newTestDevice(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.tgsi"))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := tgsi.Parse(passVS)
	if err != nil {
		t.Fatal(err)
	}
	fences := d.WorkgroupFences()
	fopts := tospirv.Options{ColorBuffers: 1, WorkgroupFences: fences}
	compiled, fetches := 0, 0
	for _, path := range files {
		name := filepath.Base(path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := tgsi.Parse(string(b))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		vsShader := pass
		var vs, fs *tospirv.Result
		switch s.Processor {
		case tgsi.ProcessorVertex:
			vsShader = s
			if vs, err = tospirv.Translate(s, tospirv.Options{WorkgroupFences: fences}); err == nil {
				fs = translate(t, solidFS, fopts)
			}
		case tgsi.ProcessorFragment:
			if fs, err = tospirv.Translate(s, fopts); err == nil {
				vs = translate(t, passVS, tospirv.Options{})
			}
		case tgsi.ProcessorCompute:
			if err := compileCompute(d, s); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			compiled++
			continue
		default:
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if slices.ContainsFunc(fs.Resources, func(r tospirv.Resource) bool { return r.Kind == tospirv.InputAttachment }) {
			fetches++
			continue
		}
		if missing := Link(vs, fs); len(missing) != 0 {
			if vs, err = tospirv.Translate(vsShader, tospirv.Options{ExtraOutputs: missing, WorkgroupFences: fences}); err != nil {
				t.Errorf("%s: of the outputs that it lacks: %v", name, err)
				continue
			}
			Link(vs, fs)
		}
		// An attribute of each of the vertex shader's inputs.
		inputs := 0
		for _, decl := range vsShader.Declarations {
			if decl.File == tgsi.FileInput {
				inputs = max(inputs, decl.Last+1)
			}
		}
		var attributes []VertexAttribute
		for i := 0; i < inputs; i++ {
			attributes = append(attributes, VertexAttribute{uint32(i), formatR32G32B32A32Sfloat, uint32(16 * i)})
		}
		var resources []StageResource
		for _, r := range vs.Resources {
			resources = append(resources, StageResource{tgsi.ProcessorVertex, r})
		}
		for _, r := range fs.Resources {
			resources = append(resources, StageResource{tgsi.ProcessorFragment, r})
		}
		p, err := d.NewGraphicsPipeline(GraphicsDesc{
			VS: vs.Words, FS: fs.Words, Stride: uint32(16 * max(inputs, 1)),
			Attributes: attributes, ColorFormat: formatR8G8B8A8Unorm, Resources: resources,
		})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		d.DestroyPipeline(p)
		compiled++
	}
	t.Logf("%d shaders, %d pipelines compiled, %d of FBFETCH left out", len(files), compiled, fetches)
}

// compileCompute compiles a compute pipeline of s.
func compileCompute(d *Device, s *tgsi.Shader) error {
	cs, err := tospirv.Translate(s, tospirv.Options{WorkgroupFences: d.WorkgroupFences()})
	if err != nil {
		return err
	}
	var resources []StageResource
	for _, r := range cs.Resources {
		resources = append(resources, StageResource{tgsi.ProcessorCompute, r})
	}
	p, err := d.NewComputePipeline(ComputeDesc{CS: cs.Words, Resources: resources})
	if err != nil {
		return err
	}
	d.DestroyPipeline(p)
	return nil
}
