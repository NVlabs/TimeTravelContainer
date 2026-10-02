// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// device is a context's VkDevice. A fork's child shares its parent's
// VkDevice, and with it the objects that the two share.
type device struct {
	obj    *object
	pd     *physicalDevice
	handle vn.Handle
	table  vk.Table
	// metal is the device's MTLDevice, which host-visible memory's
	// MTLBuffers belong to.
	metal  unsafe.Pointer
	queues []*queue
	// objects are the device's objects, which it destroys with it. The
	// context's mu guards it.
	objects map[uint64]*object
	// host is what the contexts that share the VkDevice share.
	host *hostDevice
}

// hostDevice is what the contexts that share a VkDevice share.
type hostDevice struct {
	// refs counts the devices of contexts that share the VkDevice.
	refs atomic.Int32
	// queues are the VkDevice's queues, in the order of the devices'.
	queues []*hostQueue
}

// hostQueue is a VkQueue, which contexts that share its device share.
type hostQueue struct {
	// mu serializes the use of the queue, which Vulkan requires.
	mu sync.Mutex
}

// lockQueues locks every queue of the VkDevice, which vkDeviceWaitIdle
// requires.
func (h *hostDevice) lockQueues() {
	for _, q := range h.queues {
		q.mu.Lock()
	}
}

func (h *hostDevice) unlockQueues() {
	for _, q := range h.queues {
		q.mu.Unlock()
	}
}

func registerDevice() {
	handle(vn.VK_COMMAND_TYPE_vkCreateDevice_EXT, createDevice)
	handle(vn.VK_COMMAND_TYPE_vkDestroyDevice_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_DEVICE)
		if o == nil {
			s.fail("vkDestroyDevice: no device %d", firstID(args))
			return
		}
		s.destroyDevice(o.dev)
	})
	handle(vn.VK_COMMAND_TYPE_vkGetDeviceQueue2_EXT, getDeviceQueue2)
}

// createDevice executes vkCreateDevice. The device enables the host's
// extensions that the renderer needs, besides the guest's.
func createDevice(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkCreateDevice)(args)
	pd := s.physicalDevice(cmd, args)
	if pd == nil {
		return
	}
	info := (*vn.VkDeviceCreateInfo)(a.PCreateInfo)

	// At most two queue create infos share a family, one protected, and
	// their queues must not outnumber the family's.
	queueInfos := unsafe.Slice((*vn.VkDeviceQueueCreateInfo)(info.PQueueCreateInfos), info.QueueCreateInfoCount)
	counts := make([]uint32, len(pd.queueFamilies))
	for _, qi := range queueInfos {
		if int(qi.QueueFamilyIndex) >= len(counts) ||
			pd.queueFamilies[qi.QueueFamilyIndex].QueueCount-counts[qi.QueueFamilyIndex] < qi.QueueCount {
			a.Ret = vn.VK_ERROR_UNKNOWN
			return
		}
		counts[qi.QueueFamilyIndex] += qi.QueueCount
	}

	var exts []unsafe.Pointer
	for _, name := range unsafe.Slice((*unsafe.Pointer)(info.PpEnabledExtensionNames), info.EnabledExtensionCount) {
		exts = append(exts, name)
	}
	exts = deviceQuirks(pd, info, exts)
	var ours []string
	if pd.extMetal {
		ours = append(ours, "VK_EXT_external_memory_metal")
	}
	if pd.extMetalObjects {
		ours = append(ours, "VK_EXT_metal_objects")
	}
	if pd.extPortabilitySubset {
		ours = append(ours, "VK_KHR_portability_subset")
	}
	if len(ours) != 0 {
		exts = append(exts, unsafe.Slice((*unsafe.Pointer)(s.cstrings(ours)), len(ours))...)
	}
	info.EnabledExtensionCount = uint32(len(exts))
	info.PpEnabledExtensionNames = nil
	if len(exts) != 0 {
		arr := unsafe.Slice(alloc[unsafe.Pointer](s, len(exts)), len(exts))
		copy(arr, exts)
		info.PpEnabledExtensionNames = unsafe.Pointer(&arr[0])
	}

	id := *(*uint64)(a.PDevice)
	if !s.ctx.idFree(id) {
		s.fail("vkCreateDevice: object ID %d is taken", id)
		return
	}
	s.call(cmd, args)
	h := *(*vn.Handle)(a.PDevice)
	*(*uint64)(a.PDevice) = id
	if a.Ret != vn.VK_SUCCESS {
		return
	}
	dev := &device{pd: pd, handle: h, objects: make(map[uint64]*object), host: &hostDevice{}}
	dev.host.refs.Store(1)
	dev.table = vk.DeviceTable(pd.inst.pdTable, vk.Handle(h), pd.apiVersion, exts)
	if pd.extMetal && pd.extMetalObjects {
		dev.metal = vk.MetalDevice(pd.inst.pdTable, vk.Handle(h))
	}
	for _, qi := range queueInfos {
		for i := uint32(0); i < qi.QueueCount; i++ {
			hq := &hostQueue{}
			dev.host.queues = append(dev.host.queues, hq)
			dev.queues = append(dev.queues, s.newQueue(dev, hq, qi.Flags, qi.QueueFamilyIndex, i))
		}
	}
	dev.obj = &object{typ: vn.VK_OBJECT_TYPE_DEVICE, id: id, handle: h, dev: dev}
	if !s.ctx.addObject(dev.obj) {
		s.destroyHostDevice(dev)
		return
	}
	s.ctx.mu.Lock()
	pd.devices[dev] = struct{}{}
	s.ctx.mu.Unlock()
}

// getDeviceQueue2 executes vkGetDeviceQueue2, which names a queue that the
// device created, and binds it to a ring's timeline.
func getDeviceQueue2(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkGetDeviceQueue2)(args)
	o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_DEVICE)
	if o == nil {
		s.fail("vkGetDeviceQueue2: no device %d", firstID(args))
		return
	}
	info := (*vn.VkDeviceQueueInfo2)(a.PQueueInfo)
	var q *queue
	for _, cand := range o.dev.queues {
		if cand.flags == info.Flags && cand.family == info.QueueFamilyIndex && cand.index == info.QueueIndex {
			q = cand
		}
	}
	if q == nil {
		s.fail("vkGetDeviceQueue2: no queue %d of family %d", info.QueueIndex, info.QueueFamilyIndex)
		return
	}
	// The guest gets each queue once, which fixes its ID.
	if q.obj != nil {
		s.fail("vkGetDeviceQueue2: the queue has an ID")
		return
	}
	timeline := (*vn.VkDeviceQueueTimelineInfoMESA)(findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_DEVICE_QUEUE_TIMELINE_INFO_MESA))
	if timeline == nil {
		s.fail("vkGetDeviceQueue2: no VkDeviceQueueTimelineInfoMESA")
		return
	}
	c := s.ctx
	ring := timeline.RingIdx
	c.mu.Lock()
	if ring == 0 || ring >= uint32(len(c.syncQueues)) || c.syncQueues[ring] != nil {
		c.mu.Unlock()
		s.fail("vkGetDeviceQueue2: invalid ring %d", ring)
		return
	}
	c.syncQueues[ring] = q
	q.ringIdx = ring
	c.mu.Unlock()
	id := *(*uint64)(a.PQueue)
	q.obj = &object{typ: vn.VK_OBJECT_TYPE_QUEUE, id: id, handle: q.handle, dev: o.dev, queue: q}
	c.addObject(q.obj)
}

// destroyDevice destroys a device, with its objects and queues.
func (s *stream) destroyDevice(dev *device) {
	c := s.ctx
	// Nothing may use what the device destroys.
	wait := newArgs[vn.Args_vkDeviceWaitIdle](s)
	wait.Device = dev.handle
	dev.host.lockQueues()
	vk.Call(vn.VK_COMMAND_TYPE_vkDeviceWaitIdle_EXT, dev.table, unsafe.Pointer(wait))
	dev.host.unlockQueues()

	c.mu.Lock()
	objs := make([]*object, 0, len(dev.objects))
	for _, o := range dev.objects {
		objs = append(objs, o)
	}
	c.mu.Unlock()
	// Destroy objects before what they use: pools free their command buffers
	// and sets; pipelines, framebuffers, and views go before what they
	// refer to; memory goes last.
	sort.Slice(objs, func(i, j int) bool {
		return destroyOrder(objs[i].typ) < destroyOrder(objs[j].typ)
	})
	for _, o := range objs {
		if o.releaseHost() {
			s.destroyHostObject(dev, o)
		}
		if o.children != nil {
			c.removeChildren(o)
		}
		c.removeObject(o)
		if o.mem != nil {
			o.mem.release()
		}
	}
	for _, q := range dev.queues {
		q.stop()
		c.mu.Lock()
		if q.ringIdx != 0 && c.syncQueues[q.ringIdx] == q {
			c.syncQueues[q.ringIdx] = nil
		}
		c.mu.Unlock()
		if q.obj != nil {
			c.removeObject(q.obj)
		}
	}
	s.destroyHostDevice(dev)
	c.mu.Lock()
	delete(dev.pd.devices, dev)
	c.mu.Unlock()
	c.removeObject(dev.obj)
}

// destroyHostDevice releases a context's device: the device's fences, and
// the VkDevice and its table once no context shares them.
func (s *stream) destroyHostDevice(dev *device) {
	for _, q := range dev.queues {
		q.releaseFences(s)
	}
	if dev.host.refs.Add(-1) != 0 {
		return
	}
	a := newArgs[vn.Args_vkDestroyDevice](s)
	a.Device = dev.handle
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDevice_EXT, dev.table, unsafe.Pointer(a))
	dev.table.Free()
}

// destroyOrder orders the destruction of a device's objects by type.
func destroyOrder(typ int32) int {
	switch typ {
	case vn.VK_OBJECT_TYPE_COMMAND_POOL, vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL:
		return 0
	case vn.VK_OBJECT_TYPE_PIPELINE, vn.VK_OBJECT_TYPE_FRAMEBUFFER:
		return 1
	case vn.VK_OBJECT_TYPE_IMAGE_VIEW, vn.VK_OBJECT_TYPE_BUFFER_VIEW, vn.VK_OBJECT_TYPE_ACCELERATION_STRUCTURE_KHR:
		return 2
	case vn.VK_OBJECT_TYPE_IMAGE, vn.VK_OBJECT_TYPE_BUFFER:
		return 3
	case vn.VK_OBJECT_TYPE_DEVICE_MEMORY:
		return 5
	default:
		return 4
	}
}

// destroyCommands are the commands that destroy a device's objects, by
// type. Their arguments are (device, object, allocator).
var destroyCommands = map[int32]int32{
	vn.VK_OBJECT_TYPE_COMMAND_POOL:    vn.VK_COMMAND_TYPE_vkDestroyCommandPool_EXT,
	vn.VK_OBJECT_TYPE_DESCRIPTOR_POOL: vn.VK_COMMAND_TYPE_vkDestroyDescriptorPool_EXT,
	vn.VK_OBJECT_TYPE_PIPELINE:        vn.VK_COMMAND_TYPE_vkDestroyPipeline_EXT,
	vn.VK_OBJECT_TYPE_DEVICE_MEMORY:   vn.VK_COMMAND_TYPE_vkFreeMemory_EXT,
	vn.VK_OBJECT_TYPE_COMMAND_BUFFER:  -1,
	vn.VK_OBJECT_TYPE_DESCRIPTOR_SET:  -1,
	vn.VK_OBJECT_TYPE_RENDER_PASS:     vn.VK_COMMAND_TYPE_vkDestroyRenderPass_EXT,
}

func init() {
	for _, so := range simpleObjects {
		if _, ok := destroyCommands[so.typ]; !ok {
			destroyCommands[so.typ] = so.destroy
		}
	}
}

// destroyHostObject destroys a device's object with Vulkan.
func (s *stream) destroyHostObject(dev *device, o *object) {
	typ, ok := destroyCommands[o.typ]
	if !ok || typ < 0 {
		return
	}
	// (device, object, allocator), as vn.Args_vkDestroyBuffer.
	a := newArgs[vn.Args_vkDestroyBuffer](s)
	a.Device = dev.handle
	a.Buffer = o.handle
	vk.Call(typ, dev.table, unsafe.Pointer(a))
}
