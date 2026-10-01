// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// importMemoryMetalHandleInfo is VkImportMemoryMetalHandleInfoEXT.
type importMemoryMetalHandleInfo struct {
	sType      int32
	pNext      unsafe.Pointer
	handleType uint32
	handle     unsafe.Pointer
}

// memory is a VkDeviceMemory.
type memory struct {
	dev           *device
	size          uint64
	typeIndex     uint32
	propertyFlags uint32
	// shm is the memory of host-visible memory, which the guest maps: shared
	// memory that Vulkan imports as an MTLBuffer.
	shm *sharedMemory
	// metalBuffer is the MTLBuffer of shm.
	metalBuffer unsafe.Pointer
	// exported is whether a resource has the memory.
	exported bool
}

// release releases what the memory holds, once Vulkan freed it.
func (m *memory) release() {
	if m.metalBuffer != nil {
		vk.MetalRelease(m.metalBuffer)
		m.metalBuffer = nil
	}
	if m.shm != nil {
		m.shm.decRef()
		m.shm = nil
	}
}

func registerMemory() {
	handle(vn.VK_COMMAND_TYPE_vkAllocateMemory_EXT, allocateMemory)
	handle(vn.VK_COMMAND_TYPE_vkFreeMemory_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.destroyObject(cmd, args, vn.VK_OBJECT_TYPE_DEVICE_MEMORY, unsafe.Pointer(&(*vn.Args_vkFreeMemory)(args).Memory))
	})
	handle(vn.VK_COMMAND_TYPE_vkGetMemoryResourcePropertiesMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetMemoryResourcePropertiesMESA)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		r := s.ctx.resource(a.ResourceId)
		if r == nil {
			s.fail("vkGetMemoryResourcePropertiesMESA: no resource %d", a.ResourceId)
			return
		}
		if !dev.pd.emulateDmaBuf {
			a.Ret = vn.VK_ERROR_INVALID_EXTERNAL_HANDLE
			return
		}
		// A resource's shared memory imports as host-visible memory.
		props := (*vn.VkMemoryResourcePropertiesMESA)(a.PMemoryResourceProperties)
		props.MemoryTypeBits = dev.pd.hostVisibleTypes
		if p := findStruct(props.PNext, vn.VK_STRUCTURE_TYPE_MEMORY_RESOURCE_ALLOCATION_SIZE_PROPERTIES_MESA); p != nil {
			(*vn.VkMemoryResourceAllocationSizePropertiesMESA)(p).AllocationSize = r.size
		}
		a.Ret = vn.VK_SUCCESS
	})
}

// allocateMemory executes vkAllocateMemory. Host-visible memory is shared
// memory that the renderer wraps as an MTLBuffer and Vulkan imports, so that
// the guest maps it too, as virglrenderer does on macOS. Memory that imports
// a resource imports the resource's shared memory the same way.
func allocateMemory(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkAllocateMemory)(args)
	dev := s.commandDevice(cmd, args)
	if dev == nil {
		return
	}
	info := (*vn.VkMemoryAllocateInfo)(a.PAllocateInfo)
	stripExportMemory(dev, info)
	props := &dev.pd.memProps
	if info.MemoryTypeIndex >= props.MemoryTypeCount {
		a.Ret = vn.VK_ERROR_UNKNOWN
		return
	}
	mem := &memory{
		dev:           dev,
		typeIndex:     info.MemoryTypeIndex,
		propertyFlags: props.MemoryTypes[info.MemoryTypeIndex].PropertyFlags,
	}

	var shm *sharedMemory
	if prev := findPrevStruct(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_IMPORT_MEMORY_RESOURCE_INFO_MESA); prev != nil {
		resInfo := (*vn.VkImportMemoryResourceInfoMESA)(*(*unsafe.Pointer)(unsafe.Add(prev, 8)))
		r := s.ctx.resource(resInfo.ResourceId)
		if r == nil {
			s.fail("vkAllocateMemory: no resource %d", resInfo.ResourceId)
			return
		}
		if info.AllocationSize > uint64(len(r.mem.data)) {
			a.Ret = vn.VK_ERROR_INVALID_EXTERNAL_HANDLE
			return
		}
		r.mem.incRef()
		shm = r.mem
		// Replace the resource's import with the Metal import.
		imp, err := s.importMetal(dev, shm, resInfo.PNext)
		if err != nil {
			shm.decRef()
			a.Ret = vn.VK_ERROR_INVALID_EXTERNAL_HANDLE
			return
		}
		setNext(prev, unsafe.Pointer(imp))
		mem.metalBuffer = imp.handle
		info.AllocationSize = uint64(len(shm.data))
	} else if mem.propertyFlags&vkMemoryPropertyHostVisible != 0 && dev.metal != nil {
		var err error
		shm, err = newSharedMemory(info.AllocationSize)
		if err != nil {
			s.ctx.logf("vkAllocateMemory: %v", err)
			a.Ret = vn.VK_ERROR_OUT_OF_HOST_MEMORY
			return
		}
		imp, err := s.importMetal(dev, shm, info.PNext)
		if err != nil {
			s.ctx.logf("vkAllocateMemory: %v", err)
			shm.decRef()
			a.Ret = vn.VK_ERROR_OUT_OF_HOST_MEMORY
			return
		}
		info.PNext = unsafe.Pointer(imp)
		mem.metalBuffer = imp.handle
		info.AllocationSize = uint64(len(shm.data))
	}
	mem.shm = shm
	mem.size = info.AllocationSize

	o := s.createObject(cmd, args, vn.VK_OBJECT_TYPE_DEVICE_MEMORY, a.PMemory)
	if o == nil {
		mem.release()
		return
	}
	o.mem = mem
}

// importMetal returns a VkImportMemoryMetalHandleInfoEXT, followed by next,
// that imports shm as an MTLBuffer of dev.
func (s *stream) importMetal(dev *device, shm *sharedMemory, next unsafe.Pointer) (*importMemoryMetalHandleInfo, error) {
	if dev.metal == nil {
		return nil, fmt.Errorf("the device has no MTLDevice")
	}
	buf := vk.MetalBuffer(dev.metal, unsafe.Pointer(&shm.data[0]), uint64(len(shm.data)))
	if buf == nil {
		return nil, fmt.Errorf("wrapping %d bytes as an MTLBuffer failed", len(shm.data))
	}
	imp := newArgs[importMemoryMetalHandleInfo](s)
	imp.sType = vn.VK_STRUCTURE_TYPE_IMPORT_MEMORY_METAL_HANDLE_INFO_EXT
	imp.pNext = next
	imp.handleType = vkExternalMemoryHandleTypeMTLBuffer
	imp.handle = buf
	return imp, nil
}

// Map infos of resources, VIRGL_RENDERER_MAP_CACHE_*.
const (
	mapCacheCached = 1
	mapCacheWC     = 3
)

// exportMemory returns the shared memory of memory id of the context for a
// blob resource of size bytes, and its map info.
func (c *Context) exportMemory(id uint64, size uint64, mappable bool) (*sharedMemory, uint32, error) {
	o := c.lookupType(id, vn.VK_OBJECT_TYPE_DEVICE_MEMORY)
	if o == nil {
		return nil, 0, fmt.Errorf("no memory %d", id)
	}
	mem := o.mem
	if mem.exported {
		return nil, 0, fmt.Errorf("memory %d is exported", id)
	}
	if mem.shm == nil {
		return nil, 0, fmt.Errorf("memory %d is not host-visible", id)
	}
	if size > uint64(len(mem.shm.data)) {
		return nil, 0, fmt.Errorf("a blob of %d bytes exceeds memory %d", size, id)
	}
	mapInfo := uint32(0)
	if mappable {
		coherent := mem.propertyFlags&vkMemoryPropertyHostCoherent != 0
		cached := mem.propertyFlags&vkMemoryPropertyHostCached != 0
		mapInfo = mapCacheWC
		if coherent && cached {
			mapInfo = mapCacheCached
		}
	}
	mem.exported = true
	mem.shm.incRef()
	return mem.shm, mapInfo, nil
}
