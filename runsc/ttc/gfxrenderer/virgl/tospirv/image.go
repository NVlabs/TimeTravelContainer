// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// storageImage is a storage image of an IMAGE declaration's: its variable,
// of its image type, or of an array's, and its channels' type.
type storageImage struct {
	v      spirv.ID
	tImage spirv.ID
	typ    BaseType
	info   texInfo
	format spirv.ImageFormat
	array  *resArray
	// tPtr is the type of a pointer to a descriptor.
	tPtr spirv.ID
}

// imageFormat is a format of images that shaders access, as SPIR-V has it.
type imageFormat struct {
	f spirv.ImageFormat
	b BaseType
	// extended is whether the format needs the capability of extended
	// formats.
	extended bool
}

// imageFormats are the pipe_formats of images, as Mesa names them, that
// SPIR-V has.
var imageFormats = map[string]imageFormat{
	"PIPE_FORMAT_R32G32B32A32_FLOAT": {spirv.ImageFormatRgba32f, Float, false},
	"PIPE_FORMAT_R16G16B16A16_FLOAT": {spirv.ImageFormatRgba16f, Float, false},
	"PIPE_FORMAT_R32_FLOAT":          {spirv.ImageFormatR32f, Float, false},
	"PIPE_FORMAT_R8G8B8A8_UNORM":     {spirv.ImageFormatRgba8, Float, false},
	"PIPE_FORMAT_R8G8B8A8_SNORM":     {spirv.ImageFormatRgba8Snorm, Float, false},
	"PIPE_FORMAT_R32G32_FLOAT":       {spirv.ImageFormatRg32f, Float, true},
	"PIPE_FORMAT_R16G16_FLOAT":       {spirv.ImageFormatRg16f, Float, true},
	"PIPE_FORMAT_R11G11B10_FLOAT":    {spirv.ImageFormatR11fG11fB10f, Float, true},
	"PIPE_FORMAT_R16_FLOAT":          {spirv.ImageFormatR16f, Float, true},
	"PIPE_FORMAT_R16G16B16A16_UNORM": {spirv.ImageFormatRgba16, Float, true},
	"PIPE_FORMAT_R10G10B10A2_UNORM":  {spirv.ImageFormatRgb10A2, Float, true},
	"PIPE_FORMAT_R16G16_UNORM":       {spirv.ImageFormatRg16, Float, true},
	"PIPE_FORMAT_R8G8_UNORM":         {spirv.ImageFormatRg8, Float, true},
	"PIPE_FORMAT_R16_UNORM":          {spirv.ImageFormatR16, Float, true},
	"PIPE_FORMAT_R8_UNORM":           {spirv.ImageFormatR8, Float, true},
	"PIPE_FORMAT_R16G16B16A16_SNORM": {spirv.ImageFormatRgba16Snorm, Float, true},
	"PIPE_FORMAT_R16G16_SNORM":       {spirv.ImageFormatRg16Snorm, Float, true},
	"PIPE_FORMAT_R8G8_SNORM":         {spirv.ImageFormatRg8Snorm, Float, true},
	"PIPE_FORMAT_R16_SNORM":          {spirv.ImageFormatR16Snorm, Float, true},
	"PIPE_FORMAT_R8_SNORM":           {spirv.ImageFormatR8Snorm, Float, true},
	"PIPE_FORMAT_R32G32B32A32_SINT":  {spirv.ImageFormatRgba32i, Int, false},
	"PIPE_FORMAT_R16G16B16A16_SINT":  {spirv.ImageFormatRgba16i, Int, false},
	"PIPE_FORMAT_R8G8B8A8_SINT":      {spirv.ImageFormatRgba8i, Int, false},
	"PIPE_FORMAT_R32_SINT":           {spirv.ImageFormatR32i, Int, false},
	"PIPE_FORMAT_R32G32_SINT":        {spirv.ImageFormatRg32i, Int, true},
	"PIPE_FORMAT_R16G16_SINT":        {spirv.ImageFormatRg16i, Int, true},
	"PIPE_FORMAT_R8G8_SINT":          {spirv.ImageFormatRg8i, Int, true},
	"PIPE_FORMAT_R16_SINT":           {spirv.ImageFormatR16i, Int, true},
	"PIPE_FORMAT_R8_SINT":            {spirv.ImageFormatR8i, Int, true},
	"PIPE_FORMAT_R32G32B32A32_UINT":  {spirv.ImageFormatRgba32ui, Uint, false},
	"PIPE_FORMAT_R16G16B16A16_UINT":  {spirv.ImageFormatRgba16ui, Uint, false},
	"PIPE_FORMAT_R8G8B8A8_UINT":      {spirv.ImageFormatRgba8ui, Uint, false},
	"PIPE_FORMAT_R32_UINT":           {spirv.ImageFormatR32ui, Uint, false},
	"PIPE_FORMAT_R10G10B10A2_UINT":   {spirv.ImageFormatRgb10a2ui, Uint, true},
	"PIPE_FORMAT_R32G32_UINT":        {spirv.ImageFormatRg32ui, Uint, true},
	"PIPE_FORMAT_R16G16_UINT":        {spirv.ImageFormatRg16ui, Uint, true},
	"PIPE_FORMAT_R8G8_UINT":          {spirv.ImageFormatRg8ui, Uint, true},
	"PIPE_FORMAT_R16_UINT":           {spirv.ImageFormatR16ui, Uint, true},
	"PIPE_FORMAT_R8_UINT":            {spirv.ImageFormatR8ui, Uint, true},
}

// storageImageOf returns the storage image of a memory instruction's
// IMAGE register, as its declaration has it, and a pointer to its
// descriptor.
func (t *translator) storageImageOf(r *tgsi.Register) (*storageImage, spirv.ID) {
	img := t.storageImage(r.Index)
	return img, t.descriptor(img.v, img.array, img.tPtr, r.Index, r.Indirect)
}

// storageImage returns the storage image of slot, or of its array, making
// it the first time.
func (t *translator) storageImage(slot int) *storageImage {
	key := slot
	if a := t.arrayOf(StorageImage, slot); a != nil {
		key = a.first
	}
	if img := t.images[key]; img != nil {
		return img
	}
	d := t.imageDecls[slot]
	if d == nil {
		t.fail("IMAGE[%d] is not declared", slot)
	}
	if slot < 0 || slot >= StageBindings {
		t.fail("image %d", slot)
	}
	ti, ok := textureInfo(d.ImageTarget)
	if !ok || ti.shadow || ti.rect {
		t.fail("images of %v", d.ImageTarget)
	}
	img := &storageImage{info: ti, typ: Float, format: spirv.ImageFormatUnknown}
	format := d.ImageFormat
	if format == "PIPE_FORMAT_R32_FLOAT" && !t.opts.FloatImageAtomics && t.exchangesImage(slot) {
		// OES_shader_image_atomic exchanges floats, which a device of no
		// atomics of float images exchanges as their bits: the image is
		// one of unsigned integers, as its Resource says, as its view is.
		format = "PIPE_FORMAT_R32_UINT"
	}
	if f, ok := imageFormats[format]; ok {
		img.typ, img.format = f.b, f.f
		if f.extended {
			t.m.Capability(spirv.CapabilityStorageImageExtendedFormats)
		}
	} else if format != "PIPE_FORMAT_NONE" && format != "" {
		t.fail("images of %s", format)
	}
	switch {
	case ti.dim == spirv.Dim1D:
		t.m.Capability(spirv.CapabilityImage1D)
	case ti.dim == spirv.DimBuffer:
		t.m.Capability(spirv.CapabilityImageBuffer)
	case ti.dim == spirv.DimCube && ti.arrayed:
		t.m.Capability(spirv.CapabilityImageCubeArray)
	}
	arrayed, ms := uint32(0), uint32(0)
	if ti.arrayed {
		arrayed = 1
	}
	if ti.ms {
		ms = 1
		t.m.Capability(spirv.CapabilityStorageImageMultisample)
	}
	img.tImage = t.m.TypeImage(t.scalarType(img.typ), ti.dim, 0, arrayed, ms, 2, img.format)
	img.tPtr = t.m.TypePointer(spirv.StorageClassUniformConstant, img.tImage)
	img.v, img.array = t.descriptorVar(StorageImage, slot, spirv.StorageClassUniformConstant, img.tImage, SetImages)
	if !d.ImageWritable {
		t.m.Decorate(img.v, spirv.DecorationNonWritable)
	}
	t.images[key] = img
	t.resource(Resource{Kind: StorageImage, Slot: slot, Target: d.ImageTarget, Type: img.typ,
		Format: format}, img.array)
	return img
}

// imageCoord returns an image instruction's integer coordinates, of
// source n: a cube's face, and a cube array's layer's face, are of the
// third, and a multisampled image's sample of the fourth.
func (t *translator) imageCoord(inst *tgsi.Instruction, n int, img *storageImage) (coord, sample spirv.ID) {
	src := t.src(inst, n, Int)
	k := img.info.components()
	if img.info.dim == spirv.DimCube {
		k = 3
	}
	coord = t.coordVector(src, t.tI32, k)
	if img.info.ms {
		sample = t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(src), 3)
	}
	return coord, sample
}

// imageOperands returns the image operands of a multisampled image's
// sample, or none.
func imageOperands(sample spirv.ID) []uint32 {
	if sample == 0 {
		return nil
	}
	return []uint32{uint32(spirv.ImageOperandsSample), uint32(sample)}
}

// imageLoad emits LOAD of an image: a texel, of the image's format.
func (t *translator) imageLoad(inst *tgsi.Instruction) {
	img, ptr := t.storageImageOf(&inst.Src[0].Register)
	if img.format == spirv.ImageFormatUnknown {
		t.m.Capability(spirv.CapabilityStorageImageReadWithoutFormat)
	}
	coord, sample := t.imageCoord(inst, 1, img)
	args := append([]uint32{uint32(t.load(img.tImage, ptr)), uint32(coord)}, imageOperands(sample)...)
	t.dst(inst, 0, img.typ, t.f.Op(spirv.OpImageRead, t.vecType(img.typ), args...))
}

// imageStore emits STORE to an image: a texel, of the image's format.
func (t *translator) imageStore(inst *tgsi.Instruction) {
	img, ptr := t.storageImageOf(&inst.Dst[0].Register)
	if img.format == spirv.ImageFormatUnknown {
		t.m.Capability(spirv.CapabilityStorageImageWriteWithoutFormat)
	}
	coord, sample := t.imageCoord(inst, 0, img)
	texel := t.src(inst, 1, img.typ)
	args := append([]uint32{uint32(t.load(img.tImage, ptr)), uint32(coord), uint32(texel)}, imageOperands(sample)...)
	t.f.Do(spirv.OpImageWrite, args...)
}

// imageSize emits RESQ of an image: its size, and a multisampled image's
// number of samples in w.
func (t *translator) imageSize(inst *tgsi.Instruction) {
	img, ptr := t.storageImageOf(&inst.Src[0].Register)
	t.m.Capability(spirv.CapabilityImageQuery)
	n := img.info.components()
	if img.info.dim == spirv.DimCube {
		// A cube's size is of its faces', and an array's layers'.
		n = 2
		if img.info.arrayed {
			n = 3
		}
	}
	sizeType := t.tI32
	if n > 1 {
		sizeType = t.m.TypeVector(t.tI32, uint32(n))
	}
	image := t.load(img.tImage, ptr)
	size := t.f.Op(spirv.OpImageQuerySize, sizeType, uint32(image))
	var parts [4]uint32
	zero := uint32(t.m.ConstantInt(0))
	for i := 0; i < 3; i++ {
		switch {
		case i >= n:
			parts[i] = zero
		case n == 1:
			parts[i] = uint32(size)
		default:
			parts[i] = uint32(t.f.Op(spirv.OpCompositeExtract, t.tI32, uint32(size), uint32(i)))
		}
	}
	parts[3] = zero
	if img.info.ms {
		parts[3] = uint32(t.f.Op(spirv.OpImageQuerySamples, t.tI32, uint32(image)))
	}
	t.dst(inst, 0, Int, t.f.Op(spirv.OpCompositeConstruct, t.tVec4I, parts[:]...))
}

// imageAtomic emits an atomic of an image's texel, of a format of 32-bit
// integers: ATOMCAS's of its sources 2 and 3, or the others' of source 2.
func (t *translator) imageAtomic(inst *tgsi.Instruction, op spirv.Op, b BaseType) {
	img, imgPtr := t.storageImageOf(&inst.Src[0].Register)
	switch {
	case img.format == spirv.ImageFormatR32ui || img.format == spirv.ImageFormatR32i:
	case img.format == spirv.ImageFormatR32f && op == spirv.OpAtomicExchange && t.opts.FloatImageAtomics:
		// OES_shader_image_atomic's exchange of floats.
	default:
		t.fail("atomics of images of format %d", img.format)
	}
	scalar := t.scalarType(img.typ)
	coord, sample := t.imageCoord(inst, 1, img)
	if sample == 0 {
		sample = t.m.ConstantUint(0)
	}
	ptr := t.f.Op(spirv.OpImageTexelPointer, t.m.TypePointer(spirv.StorageClassImage, scalar),
		uint32(imgPtr), uint32(coord), uint32(sample))
	scope := t.m.ConstantUint(uint32(spirv.ScopeDevice))
	sem := t.m.ConstantUint(uint32(spirv.MemorySemanticsRelaxed))
	arg := func(n int) spirv.ID {
		v := t.f.Op(spirv.OpCompositeExtract, t.scalarType(b), uint32(t.src(inst, n, b)), 0)
		return t.bitcast(scalar, t.scalarType(b), v)
	}
	var old spirv.ID
	if op == spirv.OpAtomicCompareExchange {
		cmp, val := arg(2), arg(3)
		old = t.f.Op(op, scalar, uint32(ptr), uint32(scope), uint32(sem), uint32(sem), uint32(val), uint32(cmp))
	} else {
		old = t.f.Op(op, scalar, uint32(ptr), uint32(scope), uint32(sem), uint32(arg(2)))
	}
	t.dst(inst, 0, img.typ, t.splat(t.vecType(img.typ), old))
}

// exchangesImage reports whether the shader exchanges texels of image
// slot, or of an image that it indexes indirectly, atomically.
func (t *translator) exchangesImage(slot int) bool {
	for i := range t.s.Instructions {
		inst := &t.s.Instructions[i]
		if inst.Opcode == tgsi.OpATOMXCHG && len(inst.Src) != 0 && inst.Src[0].File == tgsi.FileImage &&
			(inst.Src[0].Index == slot || inst.Src[0].Indirect != nil) {
			return true
		}
	}
	return false
}
