// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// varyingKey is a varying's semantic, by which stages link.
type varyingKey struct {
	sem   tgsi.Semantic
	index int
}

// Link links the outputs of a stage, out, to the inputs of the next, in,
// by their semantics: it gives each varying a location, the same of an
// output and an input of a semantic, in their words. It returns the inputs
// that out does not write, which Vulkan requires it to, of which out must
// be translated again, of them as its Options.ExtraOutputs.
func Link(out, in *tospirv.Result) (missing []tospirv.Varying) {
	locs := make(map[varyingKey]uint32)
	next := uint32(0)
	location := func(v tospirv.Varying) uint32 {
		k := varyingKey{v.Semantic, v.SemanticIndex}
		l, ok := locs[k]
		if !ok {
			l = next
			locs[k] = l
			next++
		}
		return l
	}
	for _, v := range out.Outputs {
		out.Words[v.LocationWord] = location(v)
	}
	for _, v := range in.Inputs {
		if _, ok := locs[varyingKey{v.Semantic, v.SemanticIndex}]; !ok {
			missing = append(missing, tospirv.Varying{Semantic: v.Semantic, SemanticIndex: v.SemanticIndex})
		}
		in.Words[v.LocationWord] = location(v)
	}
	return missing
}
