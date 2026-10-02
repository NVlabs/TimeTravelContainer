// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Host-visible memory, which guests map, is shared memory that the renderer
// imports into the host's Vulkan: as an MTLBuffer on macOS (memory.go), and
// by its address, with VK_EXT_external_memory_host, elsewhere. The host's
// driver imports host memory only into some of its memory types: NVIDIA's
// memory of the GPU that the CPU maps through the PCI BAR, for one, is the
// GPU's own. Guests see such types as memory they cannot map.

const (
	extExternalMemoryHost = "VK_EXT_external_memory_host"
	// handleTypeHostAllocation is
	// VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT.
	handleTypeHostAllocation = 0x80
	// hostMemoryPropertyFlags are the memory property flags that the CPU's
	// mapping of memory takes.
	hostMemoryPropertyFlags = vkMemoryPropertyHostVisible | vkMemoryPropertyHostCoherent | vkMemoryPropertyHostCached
)

// importMemoryHostPointerInfo is VkImportMemoryHostPointerInfoEXT.
type importMemoryHostPointerInfo struct {
	sType        int32
	pNext        unsafe.Pointer
	handleType   uint32
	pHostPointer unsafe.Pointer
}

// hostImport is what initHostImport probes of a physical device: the memory
// types that import shared memory, and the alignment that it takes.
type hostImport struct {
	types     uint32
	alignment uint64
}

// hostImportKey names a host's physical device across instances, whose
// handles are each instance's.
type hostImportKey struct {
	vendor, device, driver uint32
	name                   string
}

// hostImports caches the hostImport of each physical device: probing one
// creates a device, which each process of a guest that enumerates the GPU
// would wait for.
var hostImports sync.Map

// initHostImport settles which of pd's memory types the renderer imports
// shared memory into by its address, where Metal does not import it, and
// keeps the others' host visibility from guests. A physical device without
// such types keeps its memory types as they are, and the guest's mappings
// of them fail.
func (s *stream) initHostImport(pd *physicalDevice) {
	if pd.extMetal || !pd.extHostMemory {
		return
	}
	key := hostImportKey{
		vendor: pd.props.VendorID, device: pd.props.DeviceID, driver: pd.props.DriverVersion,
		name: goString(pd.props.DeviceName[:]),
	}
	var probed hostImport
	if cached, ok := hostImports.Load(key); ok {
		probed = cached.(hostImport)
	} else {
		var err error
		if probed, err = probeHostImport(pd); err != nil {
			s.ctx.logf("probing host memory import: %v", err)
			return
		}
		hostImports.Store(key, probed)
	}
	types, alignment := probed.types, probed.alignment
	if types == 0 {
		s.ctx.logf("%s imports no host memory: guests cannot map its memory", goString(pd.props.DeviceName[:]))
		return
	}
	pd.hostImportTypes = types
	pd.hostImportAlignment = alignment
	for i := uint32(0); i < pd.memProps.MemoryTypeCount; i++ {
		if types&(1<<i) == 0 {
			pd.memProps.MemoryTypes[i].PropertyFlags &^= hostMemoryPropertyFlags
		}
	}
}

// probeHostImport probes which of pd's memory types import shared memory, by
// a device of pd's own.
func probeHostImport(pd *physicalDevice) (hostImport, error) {
	t := pd.inst.pdTable
	h := vk.Handle(pd.handle)
	alignment := max(vk.HostImportAlignment(t, h), uint64(unix.Getpagesize()))
	probe, err := newSharedMemory(alignment)
	if err != nil {
		return hostImport{}, err
	}
	defer probe.decRef()
	if uintptr(unsafe.Pointer(&probe.data[0]))%uintptr(alignment) != 0 {
		return hostImport{alignment: alignment}, nil
	}
	return hostImport{types: vk.HostImportTypes(t, h, unsafe.Pointer(&probe.data[0])), alignment: alignment}, nil
}

// importsHostMemory reports whether the renderer imports shared memory into
// pd's host-visible memory types, which guests then map.
func (pd *physicalDevice) importsHostMemory() bool {
	return (pd.extMetal && pd.extMetalObjects) || pd.hostImportTypes != 0
}

// importsHostMemory reports whether the renderer imports shared memory into
// the device's host-visible memory.
func (d *device) importsHostMemory() bool {
	return d.metal != nil || d.pd.hostImportTypes != 0
}

// importHost returns the struct, followed by next, with which allocating
// memory of dev imports shm, and the MTLBuffer that the import wraps it in,
// if any.
func (s *stream) importHost(dev *device, shm *sharedMemory, next unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer, error) {
	if dev.metal != nil {
		imp, err := s.importMetal(dev, shm, next)
		if err != nil {
			return nil, nil, err
		}
		return unsafe.Pointer(imp), imp.handle, nil
	}
	if dev.pd.hostImportTypes == 0 {
		return nil, nil, fmt.Errorf("the device imports no host memory")
	}
	p := unsafe.Pointer(&shm.data[0])
	if uintptr(p)%uintptr(dev.pd.hostImportAlignment) != 0 || uint64(len(shm.data))%dev.pd.hostImportAlignment != 0 {
		return nil, nil, fmt.Errorf("shared memory of %d bytes at %p is not aligned to %d", len(shm.data), p, dev.pd.hostImportAlignment)
	}
	imp := newArgs[importMemoryHostPointerInfo](s)
	imp.sType = vn.VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT
	imp.pNext = next
	imp.handleType = handleTypeHostAllocation
	imp.pHostPointer = p
	return unsafe.Pointer(imp), nil, nil
}
