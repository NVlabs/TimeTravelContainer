// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// Descriptor types, VkDescriptorType.
const (
	descriptorCombinedImageSampler = 1
	descriptorStorageImage         = 3
	descriptorUniformTexelBuffer   = 4
	descriptorStorageTexelBuffer   = 5
	descriptorUniformBuffer        = 6
	descriptorStorageBuffer        = 7
)

// numSets is the number of descriptor sets of the translator's resources,
// one of each kind, as tospirv's Set* number them.
const numSets = 4

// setOf returns the descriptor set of a resource's kind.
func setOf(kind tospirv.ResourceKind) int {
	switch kind {
	case tospirv.UniformBuffer:
		return tospirv.SetUniformBuffers
	case tospirv.SampledTexture:
		return tospirv.SetSamplers
	case tospirv.StorageBuffer:
		return tospirv.SetStorageBuffers
	}
	return tospirv.SetImages
}

// descriptorType returns the descriptor type of a resource.
func descriptorType(r tospirv.Resource) uint32 {
	switch r.Kind {
	case tospirv.UniformBuffer:
		return descriptorUniformBuffer
	case tospirv.SampledTexture:
		if r.Target == tgsi.TextureBuffer {
			return descriptorUniformTexelBuffer
		}
		return descriptorCombinedImageSampler
	case tospirv.StorageBuffer:
		return descriptorStorageBuffer
	}
	if r.Target == tgsi.TextureBuffer {
		return descriptorStorageTexelBuffer
	}
	return descriptorStorageImage
}

// shaderStageBit returns the VkShaderStageFlagBits of a TGSI processor.
func shaderStageBit(p tgsi.Processor) uint32 {
	switch p {
	case tgsi.ProcessorVertex:
		return 0x1
	case tgsi.ProcessorTessCtrl:
		return 0x2
	case tgsi.ProcessorTessEval:
		return 0x4
	case tgsi.ProcessorGeometry:
		return 0x8
	case tgsi.ProcessorFragment:
		return 0x10
	}
	return 0x20
}

// StageResource is a resource of a stage's shader.
type StageResource struct {
	Stage tgsi.Processor
	tospirv.Resource
}

// setLayouts creates the descriptor set layouts of resources, a layout of
// each kind's set, of a binding of each stage's resource.
func (d *Device) setLayouts(resources []StageResource) ([numSets]vn.Handle, error) {
	a := d.a
	var layouts [numSets]vn.Handle
	var bindings [numSets][]vn.VkDescriptorSetLayoutBinding
	for _, r := range resources {
		// The renderer reads no color buffers as the rendering writes
		// them, as FBFETCH's input attachments are.
		if r.Kind == tospirv.InputAttachment {
			return layouts, fmt.Errorf("input attachment %d", r.Slot)
		}
		s := setOf(r.Kind)
		bindings[s] = append(bindings[s], vn.VkDescriptorSetLayoutBinding{
			Binding:         tospirv.Binding(r.Stage, r.Slot),
			DescriptorType:  descriptorType(r.Resource),
			DescriptorCount: uint32(max(r.Count, 1)),
			StageFlags:      shaderStageBit(r.Stage),
		})
	}
	for s := range layouts {
		info := get[vn.VkDescriptorSetLayoutCreateInfo](a)
		info.SType = vn.VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO
		if n := len(bindings[s]); n != 0 {
			bs := array[vn.VkDescriptorSetLayoutBinding](a, n)
			copy(bs, bindings[s])
			info.BindingCount, info.PBindings = uint32(n), unsafe.Pointer(&bs[0])
		}
		h := get[vn.Handle](a)
		cl := get[vn.Args_vkCreateDescriptorSetLayout](a)
		cl.Device, cl.PCreateInfo, cl.PSetLayout = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
		vk.Call(vn.VK_COMMAND_TYPE_vkCreateDescriptorSetLayout_EXT, d.table, unsafe.Pointer(cl))
		if err := check("vkCreateDescriptorSetLayout", cl.Ret); err != nil {
			d.destroySetLayouts(layouts)
			return layouts, err
		}
		layouts[s] = *h
	}
	return layouts, nil
}

func (d *Device) destroySetLayouts(layouts [numSets]vn.Handle) {
	for _, h := range layouts {
		if h == 0 {
			continue
		}
		dl := get[vn.Args_vkDestroyDescriptorSetLayout](d.a)
		dl.Device, dl.DescriptorSetLayout = d.dev, h
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDescriptorSetLayout_EXT, d.table, unsafe.Pointer(dl))
	}
}

// descriptorPool returns the pool of the descriptor sets of Record's
// command buffer, which submitAndWait resets.
func (d *Device) descriptorPool() (vn.Handle, error) {
	if d.descPool != 0 {
		return d.descPool, nil
	}
	h, err := d.newDescriptorPool(d.a)
	if err != nil {
		return 0, err
	}
	d.descPool = h
	return h, nil
}

// newDescriptorPool returns a new descriptor pool, of a's structs.
func (d *Device) newDescriptorPool(a *arena) (vn.Handle, error) {
	types := []uint32{descriptorCombinedImageSampler, descriptorStorageImage, descriptorUniformTexelBuffer,
		descriptorStorageTexelBuffer, descriptorUniformBuffer, descriptorStorageBuffer}
	sizes := array[vn.VkDescriptorPoolSize](a, len(types))
	for i, t := range types {
		sizes[i] = vn.VkDescriptorPoolSize{Type: t, DescriptorCount: 4096}
	}
	info := get[vn.VkDescriptorPoolCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO
	info.MaxSets = 1024
	info.PoolSizeCount, info.PPoolSizes = uint32(len(sizes)), unsafe.Pointer(&sizes[0])
	h := get[vn.Handle](a)
	cp := get[vn.Args_vkCreateDescriptorPool](a)
	cp.Device, cp.PCreateInfo, cp.PDescriptorPool = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateDescriptorPool_EXT, d.table, unsafe.Pointer(cp))
	if err := check("vkCreateDescriptorPool", cp.Ret); err != nil {
		return 0, err
	}
	return *h, nil
}

// BufferBinding is a buffer bound to a stage's slot of a kind: a uniform
// or a storage buffer's range.
type BufferBinding struct {
	Stage  tgsi.Processor
	Kind   tospirv.ResourceKind
	Slot   int
	Buffer *Buffer
	Offset uint64
	Range  uint64
}

// TextureBinding is a sampler view, in the general layout, or an image, in
// the shader read layout, and a sampler, bound to a stage's sampler slot.
type TextureBinding struct {
	Stage   tgsi.Processor
	Slot    int
	View    *SamplerView
	Image   *Image
	Sampler *Sampler
}

// ImageBinding is a view of a texture, in the general layout, bound to a
// stage's image slot.
type ImageBinding struct {
	Stage tgsi.Processor
	Slot  int
	View  *ImageView
}

// binding returns the binding of a stage's resource of kind of slot in p's
// layout, and its element, of an array's slots, or false if p has none.
func (p *Pipeline) binding(stage tgsi.Processor, kind tospirv.ResourceKind, slot int) (binding, element uint32, ok bool) {
	for _, r := range p.resources {
		if r.Stage == stage && r.Kind == kind && slot >= r.Slot && slot < r.Slot+max(r.Count, 1) {
			return tospirv.Binding(stage, r.Slot), uint32(slot - r.Slot), true
		}
	}
	return 0, 0, false
}

// Bind binds buffers, textures and images to a pipeline's resources, in
// descriptor sets of their kinds' that it allocates of the device's pool,
// which a Device.Record's submission frees.
func (c *Commands) Bind(p *Pipeline, buffers []BufferBinding, textures []TextureBinding, images []ImageBinding) error {
	return c.bind(p, buffers, textures, images, func(bool) (vn.Handle, error) { return c.d.descriptorPool() })
}

// BindResources binds buffers, textures and images to a pipeline's
// resources, as Bind does, in descriptor sets of the batch's pools, which
// the batch frees.
func (c *Context) BindResources(cmds *Commands, p *Pipeline, buffers []BufferBinding, textures []TextureBinding,
	images []ImageBinding) error {
	b := c.batch
	return cmds.bind(p, buffers, textures, images, func(full bool) (vn.Handle, error) {
		if len(b.descPools) == 0 || full {
			h, err := c.d.newDescriptorPool(c.a)
			if err != nil {
				return 0, err
			}
			b.descPools = append(b.descPools, h)
		}
		return b.descPools[len(b.descPools)-1], nil
	})
}

// bind binds buffers, textures and images to a pipeline's resources, in
// descriptor sets of pool's, which returns another pool if full, as the
// last's are out.
func (c *Commands) bind(p *Pipeline, buffers []BufferBinding, textures []TextureBinding, images []ImageBinding,
	pool func(full bool) (vn.Handle, error)) error {
	d, a := c.d, c.a
	var sets [numSets]vn.Handle
	for s := range sets {
		for full := false; ; full = true {
			h, err := pool(full)
			if err != nil {
				return err
			}
			layout := get[vn.Handle](a)
			*layout = p.setLayouts[s]
			ai := get[vn.VkDescriptorSetAllocateInfo](a)
			ai.SType = vn.VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO
			ai.DescriptorPool, ai.DescriptorSetCount, ai.PSetLayouts = h, 1, unsafe.Pointer(layout)
			set := get[vn.Handle](a)
			as := get[vn.Args_vkAllocateDescriptorSets](a)
			as.Device, as.PAllocateInfo, as.PDescriptorSets = d.dev, unsafe.Pointer(ai), unsafe.Pointer(set)
			vk.Call(vn.VK_COMMAND_TYPE_vkAllocateDescriptorSets_EXT, d.table, unsafe.Pointer(as))
			// VK_ERROR_OUT_OF_POOL_MEMORY and VK_ERROR_FRAGMENTED_POOL.
			if !full && (as.Ret == -1000069000 || as.Ret == -12) {
				continue
			}
			if err := check("vkAllocateDescriptorSets", as.Ret); err != nil {
				return err
			}
			sets[s] = *set
			break
		}
	}
	if n := len(buffers) + len(textures) + len(images); n != 0 {
		writes := array[vn.VkWriteDescriptorSet](a, n)
		// write fills the next write, of a stage's resource of kind of
		// slot, of a type of descriptor.
		next := 0
		write := func(stage tgsi.Processor, kind tospirv.ResourceKind, slot int, typ uint32) (*vn.VkWriteDescriptorSet, error) {
			binding, element, ok := p.binding(stage, kind, slot)
			if !ok {
				return nil, fmt.Errorf("a binding of slot %d of resources of kind %d of %v, which the pipeline has none of",
					slot, kind, stage)
			}
			w := &writes[next]
			next++
			w.SType = vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET
			w.DstSet = sets[setOf(kind)]
			w.DstBinding, w.DstArrayElement = binding, element
			w.DescriptorCount = 1
			w.DescriptorType = typ
			return w, nil
		}
		for _, tex := range textures {
			info := get[vn.VkDescriptorImageInfo](a)
			if tex.View != nil {
				*info = vn.VkDescriptorImageInfo{Sampler: tex.Sampler.sampler, ImageView: tex.View.view, ImageLayout: ImageLayoutGeneral}
			} else {
				*info = vn.VkDescriptorImageInfo{Sampler: tex.Sampler.sampler, ImageView: tex.Image.view, ImageLayout: tex.Image.layout}
			}
			w, err := write(tex.Stage, tospirv.SampledTexture, tex.Slot, descriptorCombinedImageSampler)
			if err != nil {
				return err
			}
			w.PImageInfo = unsafe.Pointer(info)
		}
		for _, b := range buffers {
			info := get[vn.VkDescriptorBufferInfo](a)
			*info = vn.VkDescriptorBufferInfo{Buffer: b.Buffer.buf, Offset: b.Offset, Range: b.Range}
			var typ uint32
			switch b.Kind {
			case tospirv.UniformBuffer:
				typ = descriptorUniformBuffer
			case tospirv.StorageBuffer:
				typ = descriptorStorageBuffer
			default:
				return fmt.Errorf("a buffer of resources of kind %d", b.Kind)
			}
			w, err := write(b.Stage, b.Kind, b.Slot, typ)
			if err != nil {
				return err
			}
			w.PBufferInfo = unsafe.Pointer(info)
		}
		for _, img := range images {
			info := get[vn.VkDescriptorImageInfo](a)
			*info = vn.VkDescriptorImageInfo{ImageView: img.View.view, ImageLayout: ImageLayoutGeneral}
			w, err := write(img.Stage, tospirv.StorageImage, img.Slot, descriptorStorageImage)
			if err != nil {
				return err
			}
			w.PImageInfo = unsafe.Pointer(info)
		}
		ud := get[vn.Args_vkUpdateDescriptorSets](a)
		ud.Device, ud.DescriptorWriteCount, ud.PDescriptorWrites = d.dev, uint32(n), unsafe.Pointer(&writes[0])
		vk.Call(vn.VK_COMMAND_TYPE_vkUpdateDescriptorSets_EXT, d.table, unsafe.Pointer(ud))
	}
	hs := array[vn.Handle](a, numSets)
	copy(hs, sets[:])
	bd := get[vn.Args_vkCmdBindDescriptorSets](a)
	bd.PipelineBindPoint, bd.Layout, bd.DescriptorSetCount, bd.PDescriptorSets = p.bindPoint, p.layout, numSets, unsafe.Pointer(&hs[0])
	c.call(vn.VK_COMMAND_TYPE_vkCmdBindDescriptorSets_EXT, unsafe.Pointer(bd))
	return nil
}
