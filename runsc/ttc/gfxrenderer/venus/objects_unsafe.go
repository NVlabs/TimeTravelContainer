// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// simpleObject describes a type of device object that one command creates
// and another destroys, as virglrenderer's simple-object generator does.
type simpleObject struct {
	typ     int32
	create  int32
	destroy int32
	// created returns where the creating command's arguments hold the
	// object's handle.
	created func(args unsafe.Pointer) unsafe.Pointer
	// destroyed returns where the destroying command's arguments hold the
	// object's handle.
	destroyed func(args unsafe.Pointer) unsafe.Pointer
}

// simpleObjects are the device objects of one handle each.
var simpleObjects = []simpleObject{
	{vn.VK_OBJECT_TYPE_BUFFER, vn.VK_COMMAND_TYPE_vkCreateBuffer_EXT, vn.VK_COMMAND_TYPE_vkDestroyBuffer_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateBuffer)(a).PBuffer },
		func(a unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&(*vn.Args_vkDestroyBuffer)(a).Buffer) }},
	{vn.VK_OBJECT_TYPE_BUFFER_VIEW, vn.VK_COMMAND_TYPE_vkCreateBufferView_EXT, vn.VK_COMMAND_TYPE_vkDestroyBufferView_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateBufferView)(a).PView },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyBufferView)(a).BufferView)
		}},
	{vn.VK_OBJECT_TYPE_IMAGE, vn.VK_COMMAND_TYPE_vkCreateImage_EXT, vn.VK_COMMAND_TYPE_vkDestroyImage_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateImage)(a).PImage },
		func(a unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&(*vn.Args_vkDestroyImage)(a).Image) }},
	{vn.VK_OBJECT_TYPE_IMAGE_VIEW, vn.VK_COMMAND_TYPE_vkCreateImageView_EXT, vn.VK_COMMAND_TYPE_vkDestroyImageView_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateImageView)(a).PView },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyImageView)(a).ImageView)
		}},
	{vn.VK_OBJECT_TYPE_SAMPLER, vn.VK_COMMAND_TYPE_vkCreateSampler_EXT, vn.VK_COMMAND_TYPE_vkDestroySampler_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateSampler)(a).PSampler },
		func(a unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&(*vn.Args_vkDestroySampler)(a).Sampler) }},
	{vn.VK_OBJECT_TYPE_SAMPLER_YCBCR_CONVERSION, vn.VK_COMMAND_TYPE_vkCreateSamplerYcbcrConversion_EXT, vn.VK_COMMAND_TYPE_vkDestroySamplerYcbcrConversion_EXT,
		func(a unsafe.Pointer) unsafe.Pointer {
			return (*vn.Args_vkCreateSamplerYcbcrConversion)(a).PYcbcrConversion
		},
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroySamplerYcbcrConversion)(a).YcbcrConversion)
		}},
	{vn.VK_OBJECT_TYPE_DESCRIPTOR_SET_LAYOUT, vn.VK_COMMAND_TYPE_vkCreateDescriptorSetLayout_EXT, vn.VK_COMMAND_TYPE_vkDestroyDescriptorSetLayout_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateDescriptorSetLayout)(a).PSetLayout },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyDescriptorSetLayout)(a).DescriptorSetLayout)
		}},
	{vn.VK_OBJECT_TYPE_DESCRIPTOR_UPDATE_TEMPLATE, vn.VK_COMMAND_TYPE_vkCreateDescriptorUpdateTemplate_EXT, vn.VK_COMMAND_TYPE_vkDestroyDescriptorUpdateTemplate_EXT,
		func(a unsafe.Pointer) unsafe.Pointer {
			return (*vn.Args_vkCreateDescriptorUpdateTemplate)(a).PDescriptorUpdateTemplate
		},
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyDescriptorUpdateTemplate)(a).DescriptorUpdateTemplate)
		}},
	{vn.VK_OBJECT_TYPE_SHADER_MODULE, vn.VK_COMMAND_TYPE_vkCreateShaderModule_EXT, vn.VK_COMMAND_TYPE_vkDestroyShaderModule_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateShaderModule)(a).PShaderModule },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyShaderModule)(a).ShaderModule)
		}},
	{vn.VK_OBJECT_TYPE_PIPELINE_LAYOUT, vn.VK_COMMAND_TYPE_vkCreatePipelineLayout_EXT, vn.VK_COMMAND_TYPE_vkDestroyPipelineLayout_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreatePipelineLayout)(a).PPipelineLayout },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyPipelineLayout)(a).PipelineLayout)
		}},
	{vn.VK_OBJECT_TYPE_PIPELINE_CACHE, vn.VK_COMMAND_TYPE_vkCreatePipelineCache_EXT, vn.VK_COMMAND_TYPE_vkDestroyPipelineCache_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreatePipelineCache)(a).PPipelineCache },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyPipelineCache)(a).PipelineCache)
		}},
	{vn.VK_OBJECT_TYPE_QUERY_POOL, vn.VK_COMMAND_TYPE_vkCreateQueryPool_EXT, vn.VK_COMMAND_TYPE_vkDestroyQueryPool_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateQueryPool)(a).PQueryPool },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyQueryPool)(a).QueryPool)
		}},
	{vn.VK_OBJECT_TYPE_FENCE, vn.VK_COMMAND_TYPE_vkCreateFence_EXT, vn.VK_COMMAND_TYPE_vkDestroyFence_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateFence)(a).PFence },
		func(a unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&(*vn.Args_vkDestroyFence)(a).Fence) }},
	{vn.VK_OBJECT_TYPE_SEMAPHORE, vn.VK_COMMAND_TYPE_vkCreateSemaphore_EXT, vn.VK_COMMAND_TYPE_vkDestroySemaphore_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateSemaphore)(a).PSemaphore },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroySemaphore)(a).Semaphore)
		}},
	{vn.VK_OBJECT_TYPE_EVENT, vn.VK_COMMAND_TYPE_vkCreateEvent_EXT, vn.VK_COMMAND_TYPE_vkDestroyEvent_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateEvent)(a).PEvent },
		func(a unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&(*vn.Args_vkDestroyEvent)(a).Event) }},
	{vn.VK_OBJECT_TYPE_RENDER_PASS, vn.VK_COMMAND_TYPE_vkCreateRenderPass_EXT, vn.VK_COMMAND_TYPE_vkDestroyRenderPass_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateRenderPass)(a).PRenderPass },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyRenderPass)(a).RenderPass)
		}},
	{vn.VK_OBJECT_TYPE_FRAMEBUFFER, vn.VK_COMMAND_TYPE_vkCreateFramebuffer_EXT, vn.VK_COMMAND_TYPE_vkDestroyFramebuffer_EXT,
		func(a unsafe.Pointer) unsafe.Pointer { return (*vn.Args_vkCreateFramebuffer)(a).PFramebuffer },
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyFramebuffer)(a).Framebuffer)
		}},
	{vn.VK_OBJECT_TYPE_ACCELERATION_STRUCTURE_KHR, vn.VK_COMMAND_TYPE_vkCreateAccelerationStructureKHR_EXT, vn.VK_COMMAND_TYPE_vkDestroyAccelerationStructureKHR_EXT,
		func(a unsafe.Pointer) unsafe.Pointer {
			return (*vn.Args_vkCreateAccelerationStructureKHR)(a).PAccelerationStructure
		},
		func(a unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&(*vn.Args_vkDestroyAccelerationStructureKHR)(a).AccelerationStructure)
		}},
}

func registerObjects() {
	for i := range simpleObjects {
		so := &simpleObjects[i]
		handle(so.create, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
			s.createObject(cmd, args, so.typ, so.created(args))
		})
		handle(so.destroy, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
			s.destroyObject(cmd, args, so.typ, so.destroyed(args))
		})
	}
	// Buffers and images for external memory that the renderer emulates.
	handle(vn.VK_COMMAND_TYPE_vkCreateBuffer_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateBuffer)(args)
		s.createExternable(cmd, args, vn.VK_OBJECT_TYPE_BUFFER, a.PBuffer, a.PCreateInfo, vn.VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO)
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateImage_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateImage)(args)
		s.createExternable(cmd, args, vn.VK_OBJECT_TYPE_IMAGE, a.PImage, a.PCreateInfo, vn.VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_IMAGE_CREATE_INFO)
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateRenderPass2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.createObject(cmd, args, vn.VK_OBJECT_TYPE_RENDER_PASS, (*vn.Args_vkCreateRenderPass2)(args).PRenderPass)
	})

	// Pools.
	handle(vn.VK_COMMAND_TYPE_vkCreateCommandPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_COMMAND_POOL, (*vn.Args_vkCreateCommandPool)(args).PCommandPool); o != nil {
			o.children = make(map[uint64]*object)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkDestroyCommandPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.destroyObject(cmd, args, vn.VK_OBJECT_TYPE_COMMAND_POOL, unsafe.Pointer(&(*vn.Args_vkDestroyCommandPool)(args).CommandPool))
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateDescriptorPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL, (*vn.Args_vkCreateDescriptorPool)(args).PDescriptorPool); o != nil {
			o.children = make(map[uint64]*object)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkDestroyDescriptorPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.destroyObject(cmd, args, vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL, unsafe.Pointer(&(*vn.Args_vkDestroyDescriptorPool)(args).DescriptorPool))
	})
	handle(vn.VK_COMMAND_TYPE_vkResetDescriptorPool_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkResetDescriptorPool)(args)
		pool := s.ctx.lookupType(uint64(a.DescriptorPool), vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL)
		if pool == nil {
			s.fail("vkResetDescriptorPool: no pool %d", a.DescriptorPool)
			return
		}
		if s.call(cmd, args) {
			// Resetting a pool frees its sets.
			s.ctx.removeChildren(pool)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkAllocateCommandBuffers_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkAllocateCommandBuffers)(args)
		info := (*vn.VkCommandBufferAllocateInfo)(a.PAllocateInfo)
		level := info.Level
		s.allocateFromPool(cmd, args, vn.VK_OBJECT_TYPE_COMMAND_POOL, uint64(info.CommandPool),
			vn.VK_OBJECT_TYPE_COMMAND_BUFFER, a.PCommandBuffers, uint64(info.CommandBufferCount),
			func(i int, o *object) { o.level = level })
	})
	handle(vn.VK_COMMAND_TYPE_vkFreeCommandBuffers_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkFreeCommandBuffers)(args)
		s.freeToPool(cmd, args, vn.VK_OBJECT_TYPE_COMMAND_BUFFER, a.PCommandBuffers, uint64(a.CommandBufferCount))
	})
	handle(vn.VK_COMMAND_TYPE_vkAllocateDescriptorSets_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkAllocateDescriptorSets)(args)
		info := (*vn.VkDescriptorSetAllocateInfo)(a.PAllocateInfo)
		// The sets' layouts and variable counts, before their handles are
		// replaced.
		sets := newDescriptorSetStates(s.ctx, info)
		s.allocateFromPool(cmd, args, vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL, uint64(info.DescriptorPool),
			vn.VK_OBJECT_TYPE_DESCRIPTOR_SET, a.PDescriptorSets, uint64(info.DescriptorSetCount),
			func(i int, o *object) {
				if i < len(sets) {
					o.set = sets[i]
				}
			})
	})
	handle(vn.VK_COMMAND_TYPE_vkFreeDescriptorSets_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkFreeDescriptorSets)(args)
		s.freeToPool(cmd, args, vn.VK_OBJECT_TYPE_DESCRIPTOR_SET, a.PDescriptorSets, uint64(a.DescriptorSetCount))
	})

	// Pipelines.
	handle(vn.VK_COMMAND_TYPE_vkCreateGraphicsPipelines_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateGraphicsPipelines)(args)
		if dev := s.commandDevice(cmd, args); dev != nil && a.PCreateInfos != nil {
			pipelineQuirks(dev, unsafe.Slice((*vn.VkGraphicsPipelineCreateInfo)(a.PCreateInfos), a.CreateInfoCount))
		}
		s.createPipelines(cmd, args, a.PPipelines, uint64(a.CreateInfoCount))
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateComputePipelines_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateComputePipelines)(args)
		s.createPipelines(cmd, args, a.PPipelines, uint64(a.CreateInfoCount))
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateRayTracingPipelinesKHR_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateRayTracingPipelinesKHR)(args)
		s.createPipelines(cmd, args, a.PPipelines, uint64(a.CreateInfoCount))
	})
	handle(vn.VK_COMMAND_TYPE_vkDestroyPipeline_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.destroyObject(cmd, args, vn.VK_OBJECT_TYPE_PIPELINE, unsafe.Pointer(&(*vn.Args_vkDestroyPipeline)(args).Pipeline))
	})
}

// createExternable creates a buffer or image, which may be for external
// memory that the renderer emulates.
func (s *stream) createExternable(cmd *vn.Command, args unsafe.Pointer, typ int32, out, info unsafe.Pointer, stype int32) {
	dev := s.commandDevice(cmd, args)
	if dev == nil {
		return
	}
	external := stripExternal(dev, info, stype)
	var img *imageInfo
	if typ == vn.VK_OBJECT_TYPE_IMAGE {
		img = newImageInfo((*vn.VkImageCreateInfo)(info))
	}
	if o := s.createObject(cmd, args, typ, out); o != nil {
		o.external = external
		o.image = img
	}
}

// commandDevice returns the device that executes a device command: its first
// argument's.
func (s *stream) commandDevice(cmd *vn.Command, args unsafe.Pointer) *device {
	o := s.ctx.lookup(firstID(args))
	if o == nil || o.dev == nil {
		s.fail("%s: no device %d", cmd.Name, firstID(args))
		return nil
	}
	return o.dev
}

// createObject executes a command that creates an object of type typ,
// whose ID out holds, and returns the object, or nil if the command failed.
func (s *stream) createObject(cmd *vn.Command, args unsafe.Pointer, typ int32, out unsafe.Pointer) *object {
	dev := s.commandDevice(cmd, args)
	if dev == nil {
		return nil
	}
	id := *(*uint64)(out)
	if !s.ctx.idFree(id) {
		s.fail("%s: object ID %d is taken", cmd.Name, id)
		return nil
	}
	s.call(cmd, args)
	h := *(*vn.Handle)(out)
	// The reply holds the guest's ID.
	*(*uint64)(out) = id
	if r, _ := cmd.Result(args); r != vn.VK_SUCCESS {
		return nil
	}
	o := &object{typ: typ, id: id, handle: h, dev: dev}
	s.journalCreation(o)
	if !s.ctx.addObject(o) {
		return nil
	}
	return o
}

// destroyObject executes a command that destroys an object of type typ,
// whose ID handle holds, if it exists.
func (s *stream) destroyObject(cmd *vn.Command, args unsafe.Pointer, typ int32, handle unsafe.Pointer) *object {
	id := *(*uint64)(handle)
	if id == 0 {
		return nil
	}
	o := s.ctx.lookupType(id, typ)
	if o == nil {
		s.fail("%s: no object %d", cmd.Name, id)
		return nil
	}
	// A fork's child may share the object's host handle, which the last
	// context to destroy it destroys.
	if o.releaseHost() && !s.call(cmd, args) {
		return nil
	}
	if o.children != nil {
		// Destroying a pool frees its command buffers or descriptor sets.
		s.ctx.removeChildren(o)
	}
	s.ctx.removeObject(o)
	if o.mem != nil {
		o.mem.release()
	}
	return o
}

// allocateFromPool executes a command that allocates count objects of type
// typ, whose IDs out holds, from pool poolID of type poolType.
func (s *stream) allocateFromPool(cmd *vn.Command, args unsafe.Pointer, poolType int32, poolID uint64, typ int32, out unsafe.Pointer, count uint64, after func(i int, o *object)) {
	dev := s.commandDevice(cmd, args)
	if dev == nil {
		return
	}
	pool := s.ctx.lookupType(poolID, poolType)
	if pool == nil {
		s.fail("%s: no pool %d", cmd.Name, poolID)
		return
	}
	if count != 0 && out == nil {
		s.fail("%s: no objects", cmd.Name)
		return
	}
	slots := unsafe.Slice((*uint64)(out), count)
	ids := make([]uint64, count)
	copy(ids, slots)
	for _, id := range ids {
		if !s.ctx.idFree(id) {
			s.fail("%s: object ID %d is taken", cmd.Name, id)
			return
		}
	}
	s.call(cmd, args)
	if r, _ := cmd.Result(args); r != vn.VK_SUCCESS {
		clear(slots)
		return
	}
	for i, id := range ids {
		o := &object{typ: typ, id: id, handle: vn.Handle(slots[i]), dev: dev, pool: pool}
		slots[i] = id
		o.seq = journalSeq.Add(1)
		if after != nil {
			after(i, o)
		}
		if !s.ctx.addObject(o) {
			return
		}
		s.ctx.mu.Lock()
		pool.children[id] = o
		s.ctx.mu.Unlock()
	}
}

// freeToPool executes a command that frees count objects of type typ,
// whose IDs objs holds.
func (s *stream) freeToPool(cmd *vn.Command, args unsafe.Pointer, typ int32, objs unsafe.Pointer, count uint64) {
	if count != 0 && objs == nil {
		s.fail("%s: no objects", cmd.Name)
		return
	}
	var freed []*object
	for _, id := range unsafe.Slice((*uint64)(objs), count) {
		if id == 0 {
			continue
		}
		o := s.ctx.lookupType(id, typ)
		if o == nil {
			s.fail("%s: no object %d", cmd.Name, id)
			return
		}
		freed = append(freed, o)
	}
	if !s.call(cmd, args) {
		return
	}
	for _, o := range freed {
		s.ctx.removeObject(o)
	}
}

// createPipelines executes a command that creates count pipelines, whose IDs
// out holds. Each may fail on its own.
func (s *stream) createPipelines(cmd *vn.Command, args unsafe.Pointer, out unsafe.Pointer, count uint64) {
	dev := s.commandDevice(cmd, args)
	if dev == nil {
		return
	}
	if count != 0 && out == nil {
		s.fail("%s: no pipelines", cmd.Name)
		return
	}
	slots := unsafe.Slice((*uint64)(out), count)
	ids := make([]uint64, count)
	copy(ids, slots)
	for _, id := range ids {
		if !s.ctx.idFree(id) {
			s.fail("%s: object ID %d is taken", cmd.Name, id)
			return
		}
	}
	s.call(cmd, args)
	if r, _ := cmd.Result(args); r < vn.VK_SUCCESS {
		clear(slots)
		return
	}
	for i, id := range ids {
		h := vn.Handle(slots[i])
		if h == 0 {
			// This pipeline failed.
			slots[i] = 0
			continue
		}
		slots[i] = id
		o := &object{typ: vn.VK_OBJECT_TYPE_PIPELINE, id: id, handle: h, dev: dev}
		s.journalCreation(o)
		if !s.ctx.addObject(o) {
			return
		}
	}
}

// idFree reports whether id may name a new object.
func (c *Context) idFree(id uint64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return id != 0 && c.objects[id] == nil
}

// removeChildren removes the command buffers or descriptor sets of pool.
func (c *Context) removeChildren(pool *object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, o := range pool.children {
		if c.objects[id] == o {
			delete(c.objects, id)
		}
	}
	clear(pool.children)
}

// trackedByDevice reports whether a device tracks its objects of type typ,
// to destroy them with it, as virglrenderer's
// vkr_device_should_track_object does.
func trackedByDevice(typ int32) bool {
	switch typ {
	case vn.VK_OBJECT_TYPE_INSTANCE, vn.VK_OBJECT_TYPE_PHYSICAL_DEVICE, vn.VK_OBJECT_TYPE_DEVICE,
		vn.VK_OBJECT_TYPE_QUEUE, vn.VK_OBJECT_TYPE_COMMAND_BUFFER, vn.VK_OBJECT_TYPE_DESCRIPTOR_SET:
		return false
	}
	return true
}
