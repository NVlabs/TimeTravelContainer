// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// resArray is a range of a kind of resource's slots that instructions
// index indirectly, as GLSL's arrays of samplers, blocks and images are:
// one binding of an array of descriptors, an element of which every access
// to the slots, direct or not, takes.
type resArray struct {
	first, n int
}

// dynamicIndexing are the capabilities of arrays of each kind of
// resource, which shaders index of dynamically uniform indexes.
var dynamicIndexing = map[ResourceKind]spirv.Capability{
	UniformBuffer:  spirv.CapabilityUniformBufferArrayDynamicIndexing,
	SampledTexture: spirv.CapabilitySampledImageArrayDynamicIndexing,
	StorageBuffer:  spirv.CapabilityStorageBufferArrayDynamicIndexing,
	StorageImage:   spirv.CapabilityStorageImageArrayDynamicIndexing,
}

// findArrays finds the shader's arrays of resources: the ranges of
// declared slots from each slot that an instruction indexes indirectly.
func (t *translator) findArrays() {
	for i := range t.s.Instructions {
		inst := &t.s.Instructions[i]
		var regs []*tgsi.Register
		for j := range inst.Dst {
			regs = append(regs, &inst.Dst[j].Register)
		}
		for j := range inst.Src {
			regs = append(regs, &inst.Src[j].Register)
		}
		for _, r := range regs {
			switch r.File {
			case tgsi.FileSampler:
				if r.Indirect != nil {
					t.addArray(SampledTexture, r.Index)
				}
			case tgsi.FileConstant:
				if r.Dimension && r.DimIndirect != nil {
					t.addArray(UniformBuffer, r.DimIndex)
				}
			case tgsi.FileBuffer:
				if r.Indirect != nil {
					t.addArray(StorageBuffer, r.Index)
				}
			case tgsi.FileImage:
				if r.Indirect != nil {
					t.addArray(StorageImage, r.Index)
				}
			}
		}
	}
}

// declared reports whether a resource of kind is declared at slot.
func (t *translator) declared(kind ResourceKind, slot int) bool {
	switch kind {
	case UniformBuffer:
		return t.consts[slot] != nil
	case SampledTexture:
		return t.sviews[slot] != nil
	case StorageBuffer:
		return t.bufferDecls[slot]
	case StorageImage:
		return t.imageDecls[slot] != nil
	}
	return false
}

// addArray adds the array of kind of the declared slots from first, which
// an instruction indexes indirectly, merged with the arrays that it
// overlaps.
func (t *translator) addArray(kind ResourceKind, first int) {
	if first < 0 || first >= StageBindings || !t.declared(kind, first) {
		t.fail("an array of resources of kind %d of undeclared slot %d", kind, first)
	}
	n := 1
	for first+n < StageBindings && t.declared(kind, first+n) {
		n++
	}
	for _, a := range t.arrays[kind] {
		if first < a.first+a.n && a.first < first+n {
			last := max(first+n, a.first+a.n)
			a.first = min(first, a.first)
			a.n = last - a.first
			return
		}
	}
	t.arrays[kind] = append(t.arrays[kind], &resArray{first: first, n: n})
}

// arrayOf returns the array of kind that slot is of, or nil.
func (t *translator) arrayOf(kind ResourceKind, slot int) *resArray {
	for _, a := range t.arrays[kind] {
		if slot >= a.first && slot < a.first+a.n {
			return a
		}
	}
	return nil
}

// arrayIndex returns the index of the element of a of slot, plus the index
// of ind if not nil, clamped to a.
func (t *translator) arrayIndex(a *resArray, slot int, ind *tgsi.Indirect) spirv.ID {
	i := t.m.ConstantInt(int32(slot - a.first))
	if ind == nil {
		return i
	}
	i = t.f.Op(spirv.OpIAdd, t.tI32, uint32(t.indirectIndex(ind)), uint32(i))
	return t.glsl(t.tI32, spirv.GLSLstd450SClamp, i, t.m.ConstantInt(0), t.m.ConstantInt(int32(a.n-1)))
}

// descriptorVar returns a new variable of a resource's descriptor, of type
// typ and storage class sc, at its slot's binding of set, or, of a slot of
// an array, of the array's descriptors, at its first slot's binding, and
// the array.
func (t *translator) descriptorVar(kind ResourceKind, slot int, sc spirv.StorageClass, typ spirv.ID, set uint32) (spirv.ID, *resArray) {
	a := t.arrayOf(kind, slot)
	if a != nil {
		typ = t.m.TypeArray(typ, t.m.ConstantUint(uint32(a.n)))
		slot = a.first
		t.m.Capability(dynamicIndexing[kind])
	}
	v := t.m.Variable(t.m.TypePointer(sc, typ), sc)
	t.m.Decorate(v, spirv.DecorationDescriptorSet, set)
	t.m.Decorate(v, spirv.DecorationBinding, Binding(t.s.Processor, slot))
	return v, a
}

// resource records a resource that the shader uses, of an array's slots
// if a is not nil.
func (t *translator) resource(r Resource, a *resArray) {
	r.Count = 1
	if a != nil {
		r.Slot, r.Count = a.first, a.n
	}
	t.res.Resources = append(t.res.Resources, r)
}

// descriptor returns a pointer, of type ptr, to the descriptor of slot,
// plus the index of ind, of v, the variable of a resource's descriptor, or
// of an array's if a is not nil.
func (t *translator) descriptor(v spirv.ID, a *resArray, ptr spirv.ID, slot int, ind *tgsi.Indirect) spirv.ID {
	if a == nil {
		if ind != nil {
			t.fail("an indirect resource of no array")
		}
		return v
	}
	return t.f.Op(spirv.OpAccessChain, ptr, uint32(v), uint32(t.arrayIndex(a, slot, ind)))
}
