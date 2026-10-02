// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"sort"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// What a fork's child needs to rebuild its parent's objects (see
// clone_unsafe.go), which contexts keep as their guests use the objects:
//
//   - Immutable objects, such as shader modules and pipelines, the child
//     shares: nothing is kept but a count of the contexts that share them.
//   - Mutable objects, such as memory, buffers, and images, the child
//     creates anew: their objects keep the commands that created them, as
//     they were encoded, which the child executes again, with the same IDs.
//   - What binds memory to buffers and images, and what descriptor sets
//     hold, objects keep as the guest's IDs.
//   - Command buffers keep what was recorded in them since they began,
//     which the child records again.

// journalSeq orders what contexts do that a fork's child does again: the
// creation of objects, the binding of memory to buffers and images, and the
// export of memory as resources. The child does it in the same order, in
// which what each step needs exists.
var journalSeq atomic.Uint64

// journalState is what an object keeps for a fork's child.
type journalState struct {
	// seq is the object's creation's in journalSeq.
	seq uint64
	// create is the command that created a mutable object.
	create []byte
	// boundMemory and boundOffset are the memory that a buffer or an image
	// is bound to, if bound is set, and bindSeq is the binding's in
	// journalSeq.
	bound       bool
	boundMemory uint64
	boundOffset uint64
	bindSeq     uint64
	// image is what the child needs to copy an image's contents.
	image *imageInfo
	// layoutBindings are a descriptor set layout's bindings' descriptor
	// counts, by binding.
	layoutBindings map[uint32]uint32
	// set is what a descriptor set holds.
	set *descriptorSetState
	// level is a command buffer's VkCommandBufferLevel.
	level uint32
	// timeline is whether a semaphore is a timeline semaphore.
	timeline bool
	// recording is what was recorded in a command buffer since it began,
	// if it began.
	recording []byte
}

// shareable reports whether a fork's child shares the objects of type typ
// with its parent: they are immutable once created.
func shareable(typ int32) bool {
	switch typ {
	case vn.VK_OBJECT_TYPE_SHADER_MODULE, vn.VK_OBJECT_TYPE_PIPELINE_LAYOUT,
		vn.VK_OBJECT_TYPE_DESCRIPTOR_SET_LAYOUT, vn.VK_OBJECT_TYPE_SAMPLER,
		vn.VK_OBJECT_TYPE_SAMPLER_YCBCR_CONVERSION, vn.VK_OBJECT_TYPE_RENDER_PASS,
		vn.VK_OBJECT_TYPE_PIPELINE, vn.VK_OBJECT_TYPE_DESCRIPTOR_UPDATE_TEMPLATE,
		vn.VK_OBJECT_TYPE_PIPELINE_CACHE:
		return true
	}
	return false
}

// journalCreation keeps what a fork's child needs of an object that the
// command being executed created.
func (s *stream) journalCreation(o *object) {
	o.seq = journalSeq.Add(1)
	if shareable(o.typ) {
		o.shared = new(atomic.Int32)
		o.shared.Store(1)
		return
	}
	o.create = s.commandBytes()
}

// journalCommandBuffer keeps a command that was recorded in command buffer
// o.
func (s *stream) journalCommandBuffer(o *object, cmd *vn.Command) {
	switch cmd.Type {
	case vn.VK_COMMAND_TYPE_vkBeginCommandBuffer_EXT:
		o.recording = s.commandBytes()
	case vn.VK_COMMAND_TYPE_vkResetCommandBuffer_EXT:
		o.recording = nil
	default:
		if o.recording != nil {
			o.recording = append(o.recording, s.dec.Bytes(s.cmdStart, s.cmdEnd)...)
		}
	}
}

// imageInfo is what a fork's child needs to copy an image's contents.
type imageInfo struct {
	imageType   uint32
	format      uint32
	extent      vn.VkExtent3D
	mipLevels   uint32
	arrayLayers uint32
	samples     uint32
	tiling      uint32
	flags       uint32
}

// semaphoreTypeTimeline is VK_SEMAPHORE_TYPE_TIMELINE.
const semaphoreTypeTimeline = 1

const (
	imageTilingOptimal  = 0
	imageTilingLinear   = 1
	imageUsageTransfers = 0x1 | 0x2 // VK_IMAGE_USAGE_TRANSFER_SRC_BIT | DST
	imageCreateSparse   = 0x1 | 0x2 | 0x4
)

// newImageInfo returns what a fork's child needs of an image created with
// info, whose usage it makes include transfers, to copy the image.
func newImageInfo(info *vn.VkImageCreateInfo) *imageInfo {
	if info.Tiling == imageTilingOptimal {
		info.Usage |= imageUsageTransfers
	}
	return &imageInfo{
		imageType:   info.ImageType,
		format:      info.Format,
		extent:      info.Extent,
		mipLevels:   info.MipLevels,
		arrayLayers: info.ArrayLayers,
		samples:     info.Samples,
		tiling:      info.Tiling,
		flags:       info.Flags,
	}
}

// descriptorSetState is what a descriptor set holds.
type descriptorSetState struct {
	// layout is the set's layout's object.
	layout *object
	// variableCount is the set's variable descriptor count, if
	// hasVariableCount.
	hasVariableCount bool
	variableCount    uint32
	descriptors      map[descriptorKey]descriptor
	// inline are the set's inline uniform blocks, by binding.
	inline map[uint32][]byte
}

type descriptorKey struct {
	binding, element uint32
}

// descriptor is a descriptor of a set, by the guest's object IDs.
type descriptor struct {
	typ         uint32
	sampler     uint64
	imageView   uint64
	imageLayout uint32
	buffer      uint64
	offset      uint64
	rng         uint64
	texelView   uint64
	accel       uint64
}

// Descriptor types, VkDescriptorType.
const (
	descriptorSampler              = 0
	descriptorCombinedImageSampler = 1
	descriptorSampledImage         = 2
	descriptorStorageImage         = 3
	descriptorUniformTexelBuffer   = 4
	descriptorStorageTexelBuffer   = 5
	descriptorUniformBuffer        = 6
	descriptorStorageBuffer        = 7
	descriptorUniformBufferDynamic = 8
	descriptorStorageBufferDynamic = 9
	descriptorInputAttachment      = 10
	descriptorInlineUniformBlock   = 1000138000
	descriptorAccelerationKHR      = 1000150000
)

// newDescriptorSetStates returns the states of the sets that info allocates,
// whose handles are the guest's IDs.
func newDescriptorSetStates(c *Context, info *vn.VkDescriptorSetAllocateInfo) []*descriptorSetState {
	layouts := unsafe.Slice((*uint64)(info.PSetLayouts), info.DescriptorSetCount)
	var counts []uint32
	if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_DESCRIPTOR_SET_VARIABLE_DESCRIPTOR_COUNT_ALLOCATE_INFO); p != nil {
		v := (*vn.VkDescriptorSetVariableDescriptorCountAllocateInfo)(p)
		counts = unsafe.Slice((*uint32)(v.PDescriptorCounts), v.DescriptorSetCount)
	}
	sets := make([]*descriptorSetState, len(layouts))
	for i, id := range layouts {
		st := &descriptorSetState{
			layout:      c.lookupType(id, vn.VK_OBJECT_TYPE_DESCRIPTOR_SET_LAYOUT),
			descriptors: make(map[descriptorKey]descriptor),
		}
		if i < len(counts) {
			st.hasVariableCount = true
			st.variableCount = counts[i]
		}
		sets[i] = st
	}
	return sets
}

// next returns the descriptor after key, which may be the first of the next
// binding, as writes and copies continue past a binding's end.
func (st *descriptorSetState) next(key descriptorKey) descriptorKey {
	key.element++
	if st.layout == nil || st.layout.layoutBindings == nil {
		return key
	}
	if count, ok := st.layout.layoutBindings[key.binding]; ok && key.element >= count {
		// The next binding that the layout has.
		best := ^uint32(0)
		for b := range st.layout.layoutBindings {
			if b > key.binding && b < best {
				best = b
			}
		}
		if best != ^uint32(0) {
			return descriptorKey{binding: best}
		}
	}
	return key
}

// write keeps a VkWriteDescriptorSet of the guest's, before its handles are
// replaced.
func (st *descriptorSetState) write(w *vn.VkWriteDescriptorSet) {
	if w.DescriptorType == descriptorInlineUniformBlock {
		p := findStruct(w.PNext, vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET_INLINE_UNIFORM_BLOCK)
		if p == nil {
			return
		}
		block := (*vn.VkWriteDescriptorSetInlineUniformBlock)(p)
		data := unsafe.Slice((*byte)(block.PData), block.DataSize)
		if st.inline == nil {
			st.inline = make(map[uint32][]byte)
		}
		cur := st.inline[w.DstBinding]
		end := int(w.DstArrayElement) + len(data)
		if len(cur) < end {
			cur = append(cur, make([]byte, end-len(cur))...)
		}
		copy(cur[w.DstArrayElement:], data)
		st.inline[w.DstBinding] = cur
		return
	}
	key := descriptorKey{binding: w.DstBinding, element: w.DstArrayElement}
	var accels []uint64
	if w.DescriptorType == descriptorAccelerationKHR {
		if p := findStruct(w.PNext, vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET_ACCELERATION_STRUCTURE_KHR); p != nil {
			a := (*vn.VkWriteDescriptorSetAccelerationStructureKHR)(p)
			accels = unsafe.Slice((*uint64)(a.PAccelerationStructures), a.AccelerationStructureCount)
		}
	}
	for i := uint32(0); i < w.DescriptorCount; i++ {
		d := descriptor{typ: w.DescriptorType}
		switch w.DescriptorType {
		case descriptorSampler, descriptorCombinedImageSampler, descriptorSampledImage,
			descriptorStorageImage, descriptorInputAttachment:
			if w.PImageInfo != nil {
				info := (*vn.VkDescriptorImageInfo)(elemAt(w.PImageInfo, i, unsafe.Sizeof(vn.VkDescriptorImageInfo{})))
				d.sampler, d.imageView, d.imageLayout = uint64(info.Sampler), uint64(info.ImageView), info.ImageLayout
			}
		case descriptorUniformTexelBuffer, descriptorStorageTexelBuffer:
			if w.PTexelBufferView != nil {
				d.texelView = *(*uint64)(elemAt(w.PTexelBufferView, i, 8))
			}
		case descriptorUniformBuffer, descriptorStorageBuffer, descriptorUniformBufferDynamic,
			descriptorStorageBufferDynamic:
			if w.PBufferInfo != nil {
				info := (*vn.VkDescriptorBufferInfo)(elemAt(w.PBufferInfo, i, unsafe.Sizeof(vn.VkDescriptorBufferInfo{})))
				d.buffer, d.offset, d.rng = uint64(info.Buffer), info.Offset, info.Range
			}
		case descriptorAccelerationKHR:
			if int(i) < len(accels) {
				d.accel = accels[i]
			}
		default:
			// A type that the renderer does not keep, such as a mutable
			// descriptor: the child's descriptor stays unwritten.
			continue
		}
		st.descriptors[key] = d
		key = st.next(key)
	}
}

// copyFrom keeps a VkCopyDescriptorSet into st from src.
func (st *descriptorSetState) copyFrom(src *descriptorSetState, c *vn.VkCopyDescriptorSet) {
	if src == nil {
		return
	}
	if data, ok := src.inline[c.SrcBinding]; ok {
		// An inline uniform block's offset and count are bytes.
		end := min(int(c.SrcArrayElement+c.DescriptorCount), len(data))
		if int(c.SrcArrayElement) < end {
			w := vn.VkWriteDescriptorSet{DstBinding: c.DstBinding, DstArrayElement: c.DstArrayElement}
			chunk := data[c.SrcArrayElement:end]
			if st.inline == nil {
				st.inline = make(map[uint32][]byte)
			}
			cur := st.inline[w.DstBinding]
			need := int(w.DstArrayElement) + len(chunk)
			if len(cur) < need {
				cur = append(cur, make([]byte, need-len(cur))...)
			}
			copy(cur[w.DstArrayElement:], chunk)
			st.inline[w.DstBinding] = cur
		}
		return
	}
	from := descriptorKey{binding: c.SrcBinding, element: c.SrcArrayElement}
	to := descriptorKey{binding: c.DstBinding, element: c.DstArrayElement}
	for i := uint32(0); i < c.DescriptorCount; i++ {
		if d, ok := src.descriptors[from]; ok {
			st.descriptors[to] = d
		} else {
			delete(st.descriptors, to)
		}
		from = src.next(from)
		to = st.next(to)
	}
}

// sortedKeys returns the set's descriptors' keys, in order.
func (st *descriptorSetState) sortedKeys() []descriptorKey {
	keys := make([]descriptorKey, 0, len(st.descriptors))
	for k := range st.descriptors {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].binding != keys[j].binding {
			return keys[i].binding < keys[j].binding
		}
		return keys[i].element < keys[j].element
	})
	return keys
}

// elemAt returns element i of the array at p of elements of size bytes.
func elemAt(p unsafe.Pointer, i uint32, size uintptr) unsafe.Pointer {
	return unsafe.Add(p, uintptr(i)*size)
}

// binding is a binding of memory to a buffer or an image, by the guest's
// IDs.
type binding struct {
	object, memory, offset uint64
}

func registerJournal() {
	handle(vn.VK_COMMAND_TYPE_vkCreateDescriptorSetLayout_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateDescriptorSetLayout)(args)
		info := (*vn.VkDescriptorSetLayoutCreateInfo)(a.PCreateInfo)
		bindings := make(map[uint32]uint32, info.BindingCount)
		for _, b := range unsafe.Slice((*vn.VkDescriptorSetLayoutBinding)(info.PBindings), info.BindingCount) {
			bindings[b.Binding] = b.DescriptorCount
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_DESCRIPTOR_SET_LAYOUT, a.PSetLayout); o != nil {
			o.layoutBindings = bindings
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateSemaphore_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateSemaphore)(args)
		info := (*vn.VkSemaphoreCreateInfo)(a.PCreateInfo)
		timeline := false
		if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_SEMAPHORE_TYPE_CREATE_INFO); p != nil {
			timeline = (*vn.VkSemaphoreTypeCreateInfo)(p).SemaphoreType == semaphoreTypeTimeline
		}
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_SEMAPHORE, a.PSemaphore); o != nil {
			o.timeline = timeline
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkBindBufferMemory_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkBindBufferMemory)(args)
		s.bind(cmd, args, []binding{{uint64(a.Buffer), uint64(a.Memory), a.MemoryOffset}}, nil)
	})
	// The guest's memory binds a shadow's linear image (see
	// shadow_unsafe.go).
	handle(vn.VK_COMMAND_TYPE_vkBindImageMemory_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkBindImageMemory)(args)
		linear := s.linearImage(uint64(a.Image))
		s.bind(cmd, args, []binding{{uint64(a.Image), uint64(a.Memory), a.MemoryOffset}}, func() {
			if linear != 0 {
				a.Image = linear
			}
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkBindBufferMemory2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkBindBufferMemory2)(args)
		var bs []binding
		for _, info := range unsafe.Slice((*vn.VkBindBufferMemoryInfo)(a.PBindInfos), a.BindInfoCount) {
			bs = append(bs, binding{uint64(info.Buffer), uint64(info.Memory), info.MemoryOffset})
		}
		s.bind(cmd, args, bs, nil)
	})
	handle(vn.VK_COMMAND_TYPE_vkBindImageMemory2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkBindImageMemory2)(args)
		infos := unsafe.Slice((*vn.VkBindImageMemoryInfo)(a.PBindInfos), a.BindInfoCount)
		var bs []binding
		var linear []vn.Handle
		for i, info := range infos {
			if l := s.linearImage(uint64(info.Image)); l != 0 {
				if linear == nil {
					linear = make([]vn.Handle, len(infos))
				}
				linear[i] = l
			}
			if findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_BIND_IMAGE_PLANE_MEMORY_INFO) != nil {
				// A plane of a disjoint image, which a child does not
				// rebind.
				continue
			}
			bs = append(bs, binding{uint64(info.Image), uint64(info.Memory), info.MemoryOffset})
		}
		s.bind(cmd, args, bs, func() {
			for i, l := range linear {
				if l != 0 {
					infos[i].Image = l
				}
			}
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkUpdateDescriptorSets_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkUpdateDescriptorSets)(args)
		// Keep the writes and copies by the guest's IDs, before their
		// handles are replaced.
		writes := unsafe.Slice((*vn.VkWriteDescriptorSet)(a.PDescriptorWrites), a.DescriptorWriteCount)
		copies := unsafe.Slice((*vn.VkCopyDescriptorSet)(a.PDescriptorCopies), a.DescriptorCopyCount)
		c := s.ctx
		for i := range writes {
			if o := c.lookupType(uint64(writes[i].DstSet), vn.VK_OBJECT_TYPE_DESCRIPTOR_SET); o != nil && o.set != nil {
				o.set.write(&writes[i])
			}
		}
		for i := range copies {
			dst := c.lookupType(uint64(copies[i].DstSet), vn.VK_OBJECT_TYPE_DESCRIPTOR_SET)
			src := c.lookupType(uint64(copies[i].SrcSet), vn.VK_OBJECT_TYPE_DESCRIPTOR_SET)
			if dst != nil && dst.set != nil && src != nil {
				dst.set.copyFrom(src.set, &copies[i])
			}
		}
		s.call(cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkResetCommandPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkResetCommandPool)(args)
		pool := s.ctx.lookupType(uint64(a.CommandPool), vn.VK_OBJECT_TYPE_COMMAND_POOL)
		if pool == nil {
			s.fail("vkResetCommandPool: no pool %d", a.CommandPool)
			return
		}
		if !s.call(cmd, args) {
			return
		}
		// The pool's command buffers are reset.
		s.ctx.mu.RLock()
		for _, o := range pool.children {
			o.recording = nil
			o.shadowWrites = nil
		}
		s.ctx.mu.RUnlock()
	})
}

// bind executes a command that binds memory to buffers or images, which it
// keeps, once fix, if not nil, fixed its arguments, whose handles are
// replaced.
func (s *stream) bind(cmd *vn.Command, args unsafe.Pointer, bs []binding, fix func()) {
	if !s.callFixed(cmd, args, fix) {
		return
	}
	if r, ok := cmd.Result(args); ok && r != vn.VK_SUCCESS {
		return
	}
	seq := journalSeq.Add(1)
	for _, b := range bs {
		if o := s.ctx.lookup(b.object); o != nil {
			o.bound, o.boundMemory, o.boundOffset, o.bindSeq = true, b.memory, b.offset, seq
		}
	}
}
