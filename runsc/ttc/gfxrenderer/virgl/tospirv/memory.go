// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tospirv

import (
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/spirv"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
)

// SetStorageBuffers and SetImages are the descriptor sets of storage
// buffers and of storage images, of bindings of each stage's slots, as
// Binding numbers them.
const (
	SetStorageBuffers = 2
	SetImages         = 3
)

// MaxSharedMemory is the most shared memory, in bytes, of a compute shader:
// Vulkan's least maxComputeSharedMemorySize.
const MaxSharedMemory = 16384

// memoryBuffer is a storage buffer, or shared memory: a variable of a
// runtime array, or an array, of uints, as TGSI's memory instructions
// address bytes; or a variable of an array of storage buffers.
type memoryBuffer struct {
	v      spirv.ID
	ptr    spirv.ID // a pointer to an element
	shared bool
	// count is shared memory's number of uints.
	count int
	array *resArray
	// tBlock is the type of a pointer to a storage buffer's block.
	tBlock spirv.ID
}

// memoryRef is a buffer that an instruction accesses, and a pointer to its
// block, of a storage buffer's.
type memoryRef struct {
	*memoryBuffer
	block spirv.ID
}

// storageBuffer returns storage buffer slot's block, or its array's.
func (t *translator) storageBuffer(slot int) *memoryBuffer {
	if slot < 0 || slot >= StageBindings {
		t.fail("buffer %d", slot)
	}
	key := slot
	if a := t.arrayOf(StorageBuffer, slot); a != nil {
		key = a.first
	}
	if b := t.buffers[key]; b != nil {
		return b
	}
	tArr := t.m.TypeRuntimeArray(t.tU32)
	t.m.Decorate(tArr, spirv.DecorationArrayStride, 4)
	tBlock := t.m.TypeStruct(tArr)
	t.m.Decorate(tBlock, spirv.DecorationBlock)
	t.m.MemberDecorate(tBlock, 0, spirv.DecorationOffset, 0)
	b := &memoryBuffer{
		ptr:    t.m.TypePointer(spirv.StorageClassStorageBuffer, t.tU32),
		tBlock: t.m.TypePointer(spirv.StorageClassStorageBuffer, tBlock),
	}
	b.v, b.array = t.descriptorVar(StorageBuffer, slot, spirv.StorageClassStorageBuffer, tBlock, SetStorageBuffers)
	t.buffers[key] = b
	t.resource(Resource{Kind: StorageBuffer, Slot: slot}, b.array)
	return b
}

// sharedMemory returns a compute shader's shared memory.
func (t *translator) sharedMemory() *memoryBuffer {
	if t.shared != nil {
		return t.shared
	}
	if t.s.Processor != tgsi.ProcessorCompute {
		t.fail("shared memory of %v", t.s.Processor)
	}
	n := t.opts.SharedMemory / 4
	if n <= 0 || n > MaxSharedMemory/4 {
		n = MaxSharedMemory / 4
	}
	tArr := t.m.TypeArray(t.tU32, t.m.ConstantUint(uint32(n)))
	t.shared = &memoryBuffer{
		v:      t.m.Variable(t.m.TypePointer(spirv.StorageClassWorkgroup, tArr), spirv.StorageClassWorkgroup),
		ptr:    t.m.TypePointer(spirv.StorageClassWorkgroup, t.tU32),
		shared: true,
		count:  n,
	}
	return t.shared
}

// memory returns the buffer of a memory instruction's register.
func (t *translator) memory(r *tgsi.Register) memoryRef {
	switch r.File {
	case tgsi.FileBuffer:
		b := t.storageBuffer(r.Index)
		return memoryRef{b, t.descriptor(b.v, b.array, b.tBlock, r.Index, r.Indirect)}
	case tgsi.FileMemory:
		return memoryRef{t.sharedMemory(), 0}
	}
	t.fail("memory of %v", r.File)
	return memoryRef{}
}

// element returns a pointer to uint i of b, an index of uints, clamped to
// shared memory, whose accesses Vulkan does not bound.
func (t *translator) element(b memoryRef, i spirv.ID) spirv.ID {
	if b.shared {
		i = t.glsl(t.tU32, spirv.GLSLstd450UMin, i, t.m.ConstantUint(uint32(b.count-1)))
		return t.f.Op(spirv.OpAccessChain, b.ptr, uint32(b.v), uint32(i))
	}
	return t.f.Op(spirv.OpAccessChain, b.ptr, uint32(b.block), uint32(t.m.ConstantUint(0)), uint32(i))
}

// wordIndex returns the index of the uint at the byte offset of source n's
// x, plus c uints.
func (t *translator) wordIndex(inst *tgsi.Instruction, n int, c uint32) spirv.ID {
	off := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.src(inst, n, Uint)), 0)
	i := t.f.Op(spirv.OpShiftRightLogical, t.tU32, uint32(off), uint32(t.m.ConstantUint(2)))
	if c != 0 {
		i = t.f.Op(spirv.OpIAdd, t.tU32, uint32(i), uint32(t.m.ConstantUint(c)))
	}
	return i
}

// TGSI's MEMBAR's flags, TGSI_MEMBAR_*: of the memory that it orders, and
// of the workgroup's scope.
const (
	membarShaderBuffer = 1 << iota
	membarAtomicBuffer
	membarShaderImage
	membarShared
	membarThreadGroup
)

// The atomics, by their opcodes: SPIR-V's, and their operands' type.
var atomics = map[tgsi.Opcode]struct {
	op spirv.Op
	b  BaseType
}{
	tgsi.OpATOMUADD: {spirv.OpAtomicIAdd, Uint},
	tgsi.OpATOMXCHG: {spirv.OpAtomicExchange, Uint},
	tgsi.OpATOMAND:  {spirv.OpAtomicAnd, Uint},
	tgsi.OpATOMOR:   {spirv.OpAtomicOr, Uint},
	tgsi.OpATOMXOR:  {spirv.OpAtomicXor, Uint},
	tgsi.OpATOMUMIN: {spirv.OpAtomicUMin, Uint},
	tgsi.OpATOMUMAX: {spirv.OpAtomicUMax, Uint},
	tgsi.OpATOMIMIN: {spirv.OpAtomicSMin, Int},
	tgsi.OpATOMIMAX: {spirv.OpAtomicSMax, Int},
}

// memoryInstruction translates a memory instruction, and returns whether
// it is one.
func (t *translator) memoryInstruction(inst *tgsi.Instruction) bool {
	op := inst.Opcode
	switch op {
	case tgsi.OpLOAD:
		if inst.Src[0].File == tgsi.FileImage {
			t.imageLoad(inst)
			break
		}
		b := t.memory(&inst.Src[0].Register)
		mask := inst.Dst[0].WriteMask
		var chans [4]uint32
		for c := uint32(0); c < 4; c++ {
			if mask&(1<<c) == 0 {
				chans[c] = uint32(t.m.ConstantUint(0))
				continue
			}
			chans[c] = uint32(t.load(t.tU32, t.element(b, t.wordIndex(inst, 1, c))))
		}
		t.dst(inst, 0, Uint, t.f.Op(spirv.OpCompositeConstruct, t.tVec4U, chans[:]...))
	case tgsi.OpSTORE:
		if inst.Dst[0].File == tgsi.FileImage {
			t.imageStore(inst)
			break
		}
		b := t.memory(&inst.Dst[0].Register)
		v := t.src(inst, 1, Uint)
		for c := uint32(0); c < 4; c++ {
			if inst.Dst[0].WriteMask&(1<<c) == 0 {
				continue
			}
			x := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(v), c)
			t.store(t.element(b, t.wordIndex(inst, 0, c)), x)
		}
	case tgsi.OpRESQ:
		if inst.Src[0].File == tgsi.FileImage {
			t.imageSize(inst)
			break
		}
		if inst.Src[0].File != tgsi.FileBuffer {
			t.fail("RESQ of %v", inst.Src[0].File)
		}
		b := t.memory(&inst.Src[0].Register)
		n := t.f.Op(spirv.OpArrayLength, t.tU32, uint32(b.block), 0)
		size := t.f.Op(spirv.OpIMul, t.tU32, uint32(n), uint32(t.m.ConstantUint(4)))
		t.dst(inst, 0, Uint, t.splat(t.tVec4U, size))
	case tgsi.OpATOMCAS:
		if inst.Src[0].File == tgsi.FileImage {
			t.imageAtomic(inst, spirv.OpAtomicCompareExchange, Uint)
			break
		}
		b := t.memory(&inst.Src[0].Register)
		ptr := t.element(b, t.wordIndex(inst, 1, 0))
		cmp := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.src(inst, 2, Uint)), 0)
		val := t.f.Op(spirv.OpCompositeExtract, t.tU32, uint32(t.src(inst, 3, Uint)), 0)
		scope, sem := t.atomicScope(b)
		old := t.f.Op(spirv.OpAtomicCompareExchange, t.tU32, uint32(ptr), uint32(scope), uint32(sem), uint32(sem),
			uint32(val), uint32(cmp))
		t.dst(inst, 0, Uint, t.splat(t.tVec4U, old))
	case tgsi.OpBARRIER:
		wg := t.m.ConstantUint(uint32(spirv.ScopeWorkgroup))
		switch t.s.Processor {
		case tgsi.ProcessorCompute:
			// GLSL's barrier() orders shared memory alone, but drivers
			// order buffers and images too, which shaders count on:
			// dEQP's image_atomic_op needs a workgroup's texture writes
			// ordered before its atomics of them, which Metal orders of
			// a fence of textures alone.
			sem := t.m.ConstantUint(uint32(spirv.MemorySemanticsAcquireRelease | spirv.MemorySemanticsWorkgroupMemory |
				spirv.MemorySemanticsUniformMemory | spirv.MemorySemanticsImageMemory))
			t.f.Do(spirv.OpControlBarrier, uint32(wg), uint32(wg), uint32(sem))
		case tgsi.ProcessorTessCtrl:
			// Which orders the invocations' outputs' accesses too.
			inv := t.m.ConstantUint(uint32(spirv.ScopeInvocation))
			t.f.Do(spirv.OpControlBarrier, uint32(wg), uint32(inv), uint32(t.m.ConstantUint(uint32(spirv.MemorySemanticsRelaxed))))
		default:
			t.fail("BARRIER of %v", t.s.Processor)
		}
	case tgsi.OpMEMBAR:
		// Of the memory of its flags, of the workgroup's scope if they
		// say or the options do, or of the device's.
		flags := t.immediateChannel(&inst.Src[0])
		sem := spirv.MemorySemanticsAcquireRelease
		if flags&(membarShaderBuffer|membarAtomicBuffer) != 0 {
			sem |= spirv.MemorySemanticsUniformMemory
		}
		if flags&membarShaderImage != 0 {
			sem |= spirv.MemorySemanticsImageMemory
		}
		if flags&membarShared != 0 {
			sem |= spirv.MemorySemanticsWorkgroupMemory
		}
		if sem == spirv.MemorySemanticsAcquireRelease {
			break
		}
		scope := spirv.ScopeDevice
		if flags&membarThreadGroup != 0 || t.opts.WorkgroupFences {
			scope = spirv.ScopeWorkgroup
		}
		t.f.Do(spirv.OpMemoryBarrier, uint32(t.m.ConstantUint(uint32(scope))), uint32(t.m.ConstantUint(uint32(sem))))
	default:
		a, ok := atomics[op]
		if !ok {
			return false
		}
		if inst.Src[0].File == tgsi.FileImage {
			t.imageAtomic(inst, a.op, a.b)
			break
		}
		b := t.memory(&inst.Src[0].Register)
		ptr := t.element(b, t.wordIndex(inst, 1, 0))
		val := t.f.Op(spirv.OpCompositeExtract, t.scalarType(a.b), uint32(t.src(inst, 2, a.b)), 0)
		if a.b == Int {
			val = t.bitcast(t.tU32, t.tI32, val)
		}
		scope, sem := t.atomicScope(b)
		old := t.f.Op(a.op, t.tU32, uint32(ptr), uint32(scope), uint32(sem), uint32(val))
		t.dst(inst, 0, Uint, t.splat(t.tVec4U, old))
	}
	return true
}

// atomicScope returns the scope and memory semantics of an atomic of b.
func (t *translator) atomicScope(b memoryRef) (spirv.ID, spirv.ID) {
	if b.shared {
		return t.m.ConstantUint(uint32(spirv.ScopeWorkgroup)), t.m.ConstantUint(uint32(spirv.MemorySemanticsRelaxed))
	}
	return t.m.ConstantUint(uint32(spirv.ScopeDevice)), t.m.ConstantUint(uint32(spirv.MemorySemanticsRelaxed))
}

// computeSystemValue returns a compute shader's system value, as a
// register's bits, or 0 if it is not one.
func (t *translator) computeSystemValue(sem tgsi.Semantic) spirv.ID {
	tVec3U := t.m.TypeVector(t.tU32, 3)
	vec3 := func(b spirv.BuiltIn) spirv.ID {
		v := t.builtinValue(b, tVec3U, t.tU32)
		return t.f.Op(spirv.OpVectorShuffle, t.tVec4U, uint32(v), uint32(t.zero4U), 0, 1, 2, 3)
	}
	switch sem {
	case tgsi.SemanticThreadID:
		return vec3(spirv.BuiltInLocalInvocationId)
	case tgsi.SemanticBlockID:
		return vec3(spirv.BuiltInWorkgroupId)
	case tgsi.SemanticGridSize:
		return vec3(spirv.BuiltInNumWorkgroups)
	case tgsi.SemanticBlockSize:
		x, y, z := t.blockSize()
		return t.m.ConstantComposite(t.tVec4U, t.m.ConstantUint(x), t.m.ConstantUint(y), t.m.ConstantUint(z), t.m.ConstantUint(0))
	}
	return 0
}

// blockSize returns a compute shader's fixed block size.
func (t *translator) blockSize() (x, y, z uint32) {
	x, y, z = 1, 1, 1
	if v, ok := t.property(tgsi.PropertyCSFixedBlockWidth); ok && v > 0 {
		x = v
	}
	if v, ok := t.property(tgsi.PropertyCSFixedBlockHeight); ok && v > 0 {
		y = v
	}
	if v, ok := t.property(tgsi.PropertyCSFixedBlockDepth); ok && v > 0 {
		z = v
	}
	return
}
