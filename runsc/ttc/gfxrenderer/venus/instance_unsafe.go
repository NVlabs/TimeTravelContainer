// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"runtime"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Vulkan's values that the renderer uses beyond those of vn.
const (
	vkInstanceCreateEnumeratePortability = 0x1
	vkExportMetalObjectTypeMetalDevice   = 0x1
	vkExternalMemoryHandleTypeMTLBuffer  = 0x10000
	vkMemoryPropertyHostVisible          = 0x2
	vkMemoryPropertyHostCoherent         = 0x4
	vkMemoryPropertyHostCached           = 0x8
)

// exportMetalObjectCreateInfo is VkExportMetalObjectCreateInfoEXT.
type exportMetalObjectCreateInfo struct {
	sType            int32
	pNext            unsafe.Pointer
	exportObjectType uint32
}

// instance is a context's VkInstance.
type instance struct {
	obj     *object
	handle  vn.Handle
	table   vk.Table
	pdTable vk.Table
	// apiVersion is the version that the instance was created for.
	apiVersion uint32
	// refs counts the contexts whose instances share the VkInstance: a
	// fork's child shares its parent's.
	refs *atomic.Int32
	// handles are the host's physical devices, and physicalDevices the
	// guest's objects of them, once it enumerated them.
	enumerated      bool
	handles         []vn.Handle
	physicalDevices []*physicalDevice
}

// physicalDevice is a physical device of an instance.
type physicalDevice struct {
	inst   *instance
	obj    *object
	handle vn.Handle
	// props are the device's properties, with the version that the
	// renderer offers.
	props      vn.VkPhysicalDeviceProperties
	apiVersion uint32
	// extensions are the device's extensions that the renderer offers.
	extensions    []vn.VkExtensionProperties
	memProps      vn.VkPhysicalDeviceMemoryProperties
	queueFamilies []vn.VkQueueFamilyProperties
	// What the renderer needs of the host's extensions.
	extMetal, extMetalObjects, extPortabilitySubset bool
	devices                                         map[*device]struct{}

	// What the renderer offers beyond the host's Vulkan: see quirks.go and
	// dmabuf.go.
	hostNullDescriptor     bool
	emulateProvokingVertex bool
	emulateDmaBuf          bool
	// hostVisibleTypes are the host-visible memory types.
	hostVisibleTypes uint32
	// familyCount is the number of queue families that guests see.
	familyCount uint32
}

// newArgs allocates a call's arguments in the stream's pool.
func newArgs[T any](s *stream) *T {
	return (*T)(s.dec.AllocTemp(unsafe.Sizeof(*new(T))))
}

// alloc allocates n values of type T in the stream's pool.
func alloc[T any](s *stream, n int) *T {
	return (*T)(s.dec.AllocTempArray(unsafe.Sizeof(*new(T)), uint64(n)))
}

// cstrings returns an array of C strings of strs in the stream's pool.
func (s *stream) cstrings(strs []string) unsafe.Pointer {
	if len(strs) == 0 {
		return nil
	}
	arr := unsafe.Slice(alloc[unsafe.Pointer](s, len(strs)), len(strs))
	for i, str := range strs {
		p := unsafe.Slice(alloc[byte](s, len(str)+1), len(str)+1)
		copy(p, str)
		arr[i] = unsafe.Pointer(&p[0])
	}
	return unsafe.Pointer(&arr[0])
}

// goString returns the C string in a fixed array of chars.
func goString(chars []int8) string {
	b := unsafe.Slice((*byte)(unsafe.Pointer(&chars[0])), len(chars))
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// setString sets a fixed array of chars to str.
func setString(chars []int8, str string) {
	b := unsafe.Slice((*byte)(unsafe.Pointer(&chars[0])), len(chars))
	clear(b)
	copy(b[:len(b)-1], str)
}

// findStruct returns the struct of type stype of the pNext chain from p, or
// nil.
func findStruct(p unsafe.Pointer, stype int32) unsafe.Pointer {
	for ; p != nil; p = *(*unsafe.Pointer)(unsafe.Add(p, 8)) {
		if *(*int32)(p) == stype {
			return p
		}
	}
	return nil
}

// findPrevStruct returns the struct of the chain from p whose next one has
// type stype, or nil.
func findPrevStruct(p unsafe.Pointer, stype int32) unsafe.Pointer {
	for ; p != nil; p = *(*unsafe.Pointer)(unsafe.Add(p, 8)) {
		next := *(*unsafe.Pointer)(unsafe.Add(p, 8))
		if next != nil && *(*int32)(next) == stype {
			return p
		}
	}
	return nil
}

// setNext sets the pNext of a struct.
func setNext(p, next unsafe.Pointer) {
	*(*unsafe.Pointer)(unsafe.Add(p, 8)) = next
}

func registerInstance() {
	handle(vn.VK_COMMAND_TYPE_vkEnumerateInstanceVersion_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkEnumerateInstanceVersion)(args)
		version, ret := s.instanceVersion()
		a.Ret = ret
		if ret == vn.VK_SUCCESS {
			version = capMinor(version, maxAPIVersion)
		}
		*(*uint32)(a.PApiVersion) = version
	})
	handle(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkEnumerateInstanceExtensionProperties)(args)
		// The instance extensions are Venus's own.
		names := []string{"VK_EXT_command_serialization", "VK_MESA_venus_protocol"}
		count := (*uint32)(a.PPropertyCount)
		if a.PProperties == nil {
			*count = uint32(len(names))
			a.Ret = vn.VK_SUCCESS
			return
		}
		n := min(int(*count), len(names))
		props := unsafe.Slice((*vn.VkExtensionProperties)(a.PProperties), n)
		for i := range props {
			setString(props[i].ExtensionName[:], names[i])
			props[i].SpecVersion = extensionSpecVersions()[names[i]]
		}
		*count = uint32(n)
		a.Ret = vn.VK_SUCCESS
		if n < len(names) {
			a.Ret = vn.VK_INCOMPLETE
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateInstance_EXT, createInstance)
	handle(vn.VK_COMMAND_TYPE_vkDestroyInstance_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		o := s.ctx.lookupType(uint64((*vn.Args_vkDestroyInstance)(args).Instance), vn.VK_OBJECT_TYPE_INSTANCE)
		if o == nil || o.inst != s.ctx.instance {
			s.fail("vkDestroyInstance: not the context's instance")
			return
		}
		s.destroyInstance(o.inst)
	})
	handle(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, enumeratePhysicalDevices)
	handle(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDeviceGroups_EXT, enumeratePhysicalDeviceGroups)
	handle(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkEnumerateDeviceExtensionProperties)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		if a.PLayerName != nil {
			s.fail("vkEnumerateDeviceExtensionProperties: layers")
			return
		}
		count := (*uint32)(a.PPropertyCount)
		if a.PProperties == nil {
			*count = uint32(len(pd.extensions))
			a.Ret = vn.VK_SUCCESS
			return
		}
		n := min(int(*count), len(pd.extensions))
		copy(unsafe.Slice((*vn.VkExtensionProperties)(a.PProperties), n), pd.extensions)
		*count = uint32(n)
		a.Ret = vn.VK_SUCCESS
		if n < len(pd.extensions) {
			a.Ret = vn.VK_INCOMPLETE
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if pd := s.physicalDevice(cmd, args); pd != nil {
			*(*vn.VkPhysicalDeviceProperties)((*vn.Args_vkGetPhysicalDeviceProperties)(args).PProperties) = pd.props
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceMemoryProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if pd := s.physicalDevice(cmd, args); pd != nil {
			*(*vn.VkPhysicalDeviceMemoryProperties)((*vn.Args_vkGetPhysicalDeviceMemoryProperties)(args).PMemoryProperties) = pd.memProps
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceMemoryProperties2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		props := (*vn.VkPhysicalDeviceMemoryProperties2)((*vn.Args_vkGetPhysicalDeviceMemoryProperties2)(args).PMemoryProperties)
		if props.PNext == nil {
			// The properties are invariant.
			props.MemoryProperties = pd.memProps
			return
		}
		s.call(cmd, args)
	})
}

// physicalDevice returns the physical device that a command's first argument
// is.
func (s *stream) physicalDevice(cmd *vn.Command, args unsafe.Pointer) *physicalDevice {
	o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_PHYSICAL_DEVICE)
	if o == nil {
		s.fail("%s: no physical device %d", cmd.Name, firstID(args))
		return nil
	}
	return o.pd
}

// instanceVersion returns the host's instance version.
func (s *stream) instanceVersion() (uint32, int32) {
	a := newArgs[vn.Args_vkEnumerateInstanceVersion](s)
	version := alloc[uint32](s, 1)
	a.PApiVersion = unsafe.Pointer(version)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceVersion_EXT, s.ctx.lib().Global(), unsafe.Pointer(a))
	return *version, a.Ret
}

// hasInstanceExtension reports whether the host has an instance extension.
func (s *stream) hasInstanceExtension(name string) bool {
	a := newArgs[vn.Args_vkEnumerateInstanceExtensionProperties](s)
	count := alloc[uint32](s, 1)
	a.PPropertyCount = unsafe.Pointer(count)
	global := s.ctx.lib().Global()
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, global, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS || *count == 0 {
		return false
	}
	props := alloc[vn.VkExtensionProperties](s, int(*count))
	a.PProperties = unsafe.Pointer(props)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, global, unsafe.Pointer(a))
	if a.Ret < vn.VK_SUCCESS {
		return false
	}
	for _, p := range unsafe.Slice(props, *count) {
		if goString(p.ExtensionName[:]) == name {
			return true
		}
	}
	return false
}

// createInstance executes vkCreateInstance, as virglrenderer's does: the
// guest enables nothing of the host's, and gets Vulkan 1.1 at least.
func createInstance(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	c := s.ctx
	if c.instance != nil {
		s.fail("vkCreateInstance: the context has an instance")
		return
	}
	a := (*vn.Args_vkCreateInstance)(args)
	info := (*vn.VkInstanceCreateInfo)(a.PCreateInfo)
	if info.EnabledLayerCount != 0 {
		a.Ret = vn.VK_ERROR_LAYER_NOT_PRESENT
		return
	}
	if info.EnabledExtensionCount != 0 {
		a.Ret = vn.VK_ERROR_EXTENSION_NOT_PRESENT
		return
	}
	version, ret := s.instanceVersion()
	if ret != vn.VK_SUCCESS {
		a.Ret = ret
		return
	}
	if version < apiVersion(1, 1) {
		a.Ret = vn.VK_ERROR_INITIALIZATION_FAILED
		return
	}
	var exts []string
	if runtime.GOOS == "darwin" {
		if s.hasInstanceExtension("VK_KHR_portability_enumeration") {
			info.Flags |= vkInstanceCreateEnumeratePortability
			exts = append(exts, "VK_KHR_portability_enumeration")
		}
		// The renderer exports devices' MTLDevices, to wrap host-visible
		// memory in MTLBuffers, which VK_EXT_metal_objects requires
		// instances to declare.
		export := newArgs[exportMetalObjectCreateInfo](s)
		export.sType = vn.VK_STRUCTURE_TYPE_EXPORT_METAL_OBJECT_CREATE_INFO_EXT
		export.pNext = info.PNext
		export.exportObjectType = vkExportMetalObjectTypeMetalDevice
		info.PNext = unsafe.Pointer(export)
	}
	info.EnabledExtensionCount = uint32(len(exts))
	info.PpEnabledExtensionNames = s.cstrings(exts)

	app := newArgs[vn.VkApplicationInfo](s)
	app.SType = vn.VK_STRUCTURE_TYPE_APPLICATION_INFO
	if info.PApplicationInfo != nil {
		*app = *(*vn.VkApplicationInfo)(info.PApplicationInfo)
	}
	app.ApiVersion = max(app.ApiVersion, apiVersion(1, 1))
	info.PApplicationInfo = unsafe.Pointer(app)

	id := *(*uint64)(a.PInstance)
	if !c.idFree(id) {
		s.fail("vkCreateInstance: object ID %d is taken", id)
		return
	}
	s.call(cmd, args)
	h := *(*vn.Handle)(a.PInstance)
	*(*uint64)(a.PInstance) = id
	if a.Ret != vn.VK_SUCCESS {
		return
	}
	inst := &instance{handle: h, apiVersion: app.ApiVersion, refs: new(atomic.Int32)}
	inst.refs.Store(1)
	inst.table, inst.pdTable = c.lib().InstanceTables(vk.Handle(h))
	inst.obj = &object{typ: vn.VK_OBJECT_TYPE_INSTANCE, id: id, handle: h, inst: inst}
	if !c.addObject(inst.obj) {
		s.destroyHostInstance(inst)
		return
	}
	c.mu.Lock()
	c.instance = inst
	c.mu.Unlock()
	if app.PApplicationName != nil {
		c.logf("instance of %s", goCString(app.PApplicationName))
	}
}

// goCString returns the C string at p.
func goCString(p unsafe.Pointer) string {
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

// destroyHostInstance destroys an instance's VkInstance and its tables,
// once no context shares them.
func (s *stream) destroyHostInstance(inst *instance) {
	if inst.refs.Add(-1) != 0 {
		return
	}
	a := newArgs[vn.Args_vkDestroyInstance](s)
	a.Instance = inst.handle
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyInstance_EXT, inst.table, unsafe.Pointer(a))
	inst.table.Free()
	inst.pdTable.Free()
}

// destroyInstance destroys the context's instance, with its devices.
func (s *stream) destroyInstance(inst *instance) {
	c := s.ctx
	for _, pd := range inst.physicalDevices {
		if pd == nil {
			continue
		}
		for dev := range pd.devices {
			s.destroyDevice(dev)
		}
		c.removeObject(pd.obj)
	}
	s.destroyHostInstance(inst)
	c.removeObject(inst.obj)
	c.mu.Lock()
	if c.instance == inst {
		c.instance = nil
	}
	c.mu.Unlock()
}

// enumerate enumerates the host's physical devices, once.
func (s *stream) enumerate(inst *instance) int32 {
	if inst.enumerated {
		return vn.VK_SUCCESS
	}
	a := newArgs[vn.Args_vkEnumeratePhysicalDevices](s)
	count := alloc[uint32](s, 1)
	a.Instance = inst.handle
	a.PPhysicalDeviceCount = unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, inst.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		return a.Ret
	}
	handles := alloc[vn.Handle](s, int(*count))
	a.PPhysicalDevices = unsafe.Pointer(handles)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, inst.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		return a.Ret
	}
	inst.handles = append([]vn.Handle(nil), unsafe.Slice(handles, *count)...)
	inst.physicalDevices = make([]*physicalDevice, len(inst.handles))
	inst.enumerated = true
	return vn.VK_SUCCESS
}

func (s *stream) commandInstance(cmd *vn.Command, args unsafe.Pointer) *instance {
	o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_INSTANCE)
	if o == nil || o.inst != s.ctx.instance {
		s.fail("%s: not the context's instance", cmd.Name)
		return nil
	}
	return o.inst
}

// enumeratePhysicalDevices executes vkEnumeratePhysicalDevices: the guest
// names the host's physical devices with IDs of its own.
func enumeratePhysicalDevices(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkEnumeratePhysicalDevices)(args)
	inst := s.commandInstance(cmd, args)
	if inst == nil {
		return
	}
	if a.Ret = s.enumerate(inst); a.Ret != vn.VK_SUCCESS {
		return
	}
	count := uint32(len(inst.handles))
	countp := (*uint32)(a.PPhysicalDeviceCount)
	if a.PPhysicalDevices == nil {
		*countp = count
		return
	}
	if count > *countp {
		count = *countp
		a.Ret = vn.VK_INCOMPLETE
	} else {
		*countp = count
	}
	ids := unsafe.Slice((*uint64)(a.PPhysicalDevices), count)
	for i, id := range ids {
		if pd := inst.physicalDevices[i]; pd != nil {
			if pd.obj.id != id {
				s.fail("vkEnumeratePhysicalDevices: physical device %d is %d, not %d", i, pd.obj.id, id)
				return
			}
			continue
		}
		if !s.ctx.idFree(id) {
			s.fail("vkEnumeratePhysicalDevices: object ID %d is taken", id)
			return
		}
		pd := s.newPhysicalDevice(inst, inst.handles[i])
		pd.obj = &object{typ: vn.VK_OBJECT_TYPE_PHYSICAL_DEVICE, id: id, handle: pd.handle, pd: pd}
		if !s.ctx.addObject(pd.obj) {
			return
		}
		inst.physicalDevices[i] = pd
	}
}

// newPhysicalDevice returns a physical device of the host's, whose
// properties it caches.
func (s *stream) newPhysicalDevice(inst *instance, h vn.Handle) *physicalDevice {
	pd := &physicalDevice{inst: inst, handle: h, devices: make(map[*device]struct{})}
	t := inst.pdTable

	props := newArgs[vn.Args_vkGetPhysicalDeviceProperties](s)
	props.PhysicalDevice = h
	props.PProperties = unsafe.Pointer(alloc[vn.VkPhysicalDeviceProperties](s, 1))
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties_EXT, t, unsafe.Pointer(props))
	pd.props = *(*vn.VkPhysicalDeviceProperties)(props.PProperties)
	pd.props.ApiVersion = capMinor(pd.props.ApiVersion, maxAPIVersion)
	pd.apiVersion = min(pd.props.ApiVersion, inst.apiVersion)

	hostExtensions := make(map[string]bool)
	exts := newArgs[vn.Args_vkEnumerateDeviceExtensionProperties](s)
	count := alloc[uint32](s, 1)
	exts.PhysicalDevice = h
	exts.PPropertyCount = unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, t, unsafe.Pointer(exts))
	if exts.Ret == vn.VK_SUCCESS && *count != 0 {
		list := alloc[vn.VkExtensionProperties](s, int(*count))
		exts.PProperties = unsafe.Pointer(list)
		vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, t, unsafe.Pointer(exts))
		specs := extensionSpecVersions()
		for _, e := range unsafe.Slice(list, *count) {
			name := goString(e.ExtensionName[:])
			hostExtensions[name] = true
			switch name {
			case "VK_EXT_external_memory_metal":
				pd.extMetal = true
			case "VK_EXT_metal_objects":
				pd.extMetalObjects = true
			case "VK_KHR_portability_subset":
				pd.extPortabilitySubset = true
			}
			if spec, ok := specs[name]; ok {
				e.SpecVersion = min(e.SpecVersion, spec)
				pd.extensions = append(pd.extensions, e)
			}
		}
	}

	mem := newArgs[vn.Args_vkGetPhysicalDeviceMemoryProperties](s)
	mem.PhysicalDevice = h
	mem.PMemoryProperties = unsafe.Pointer(alloc[vn.VkPhysicalDeviceMemoryProperties](s, 1))
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceMemoryProperties_EXT, t, unsafe.Pointer(mem))
	pd.memProps = *(*vn.VkPhysicalDeviceMemoryProperties)(mem.PMemoryProperties)

	qf := newArgs[vn.Args_vkGetPhysicalDeviceQueueFamilyProperties](s)
	qcount := alloc[uint32](s, 1)
	qf.PhysicalDevice = h
	qf.PQueueFamilyPropertyCount = unsafe.Pointer(qcount)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties_EXT, t, unsafe.Pointer(qf))
	if *qcount != 0 {
		list := alloc[vn.VkQueueFamilyProperties](s, int(*qcount))
		qf.PQueueFamilyProperties = unsafe.Pointer(list)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties_EXT, t, unsafe.Pointer(qf))
		pd.queueFamilies = append([]vn.VkQueueFamilyProperties(nil), unsafe.Slice(list, *qcount)...)
	}
	s.initQuirks(pd, hostExtensions)
	return pd
}

// enumeratePhysicalDeviceGroups executes vkEnumeratePhysicalDeviceGroups,
// whose groups name physical devices by the guest's IDs, as virglrenderer's
// does.
func enumeratePhysicalDeviceGroups(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkEnumeratePhysicalDeviceGroups)(args)
	inst := s.commandInstance(cmd, args)
	if inst == nil {
		return
	}
	if a.Ret = s.enumerate(inst); a.Ret != vn.VK_SUCCESS {
		return
	}
	orig := a.PPhysicalDeviceGroupProperties
	count := *(*uint32)(a.PPhysicalDeviceGroupCount)
	if orig != nil {
		groups := alloc[vn.VkPhysicalDeviceGroupProperties](s, int(count))
		for i := range unsafe.Slice(groups, count) {
			unsafe.Slice(groups, count)[i].SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_GROUP_PROPERTIES
		}
		a.PPhysicalDeviceGroupProperties = unsafe.Pointer(groups)
	}
	s.call(cmd, args)
	if a.Ret != vn.VK_SUCCESS || orig == nil {
		a.PPhysicalDeviceGroupProperties = orig
		return
	}
	count = *(*uint32)(a.PPhysicalDeviceGroupCount)
	host := unsafe.Slice((*vn.VkPhysicalDeviceGroupProperties)(a.PPhysicalDeviceGroupProperties), count)
	out := unsafe.Slice((*vn.VkPhysicalDeviceGroupProperties)(orig), count)
	a.PPhysicalDeviceGroupProperties = orig
	for i := range host {
		out[i].PhysicalDeviceCount = host[i].PhysicalDeviceCount
		out[i].SubsetAllocation = host[i].SubsetAllocation
		for j := uint32(0); j < host[i].PhysicalDeviceCount && j < uint32(len(host[i].PhysicalDevices)); j++ {
			var pd *physicalDevice
			for k, h := range inst.handles {
				if h == host[i].PhysicalDevices[j] {
					pd = inst.physicalDevices[k]
				}
			}
			if pd == nil {
				// The guest must enumerate physical devices first.
				a.Ret = vn.VK_ERROR_INITIALIZATION_FAILED
				return
			}
			out[i].PhysicalDevices[j] = vn.Handle(pd.obj.id)
		}
	}
}
