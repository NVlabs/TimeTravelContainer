// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// arena is memory for the C structs of Vulkan's calls, which reset frees.
type arena struct {
	pool vn.Pool
	dec  *vn.Decoder
}

func newArena() *arena {
	a := &arena{}
	a.dec = vn.NewDecoder(&a.pool, nil)
	return a
}

// reset frees what the arena allocated.
func (a *arena) reset() {
	a.dec.ResetTempPool()
}

// get returns a new, zeroed T of a.
func get[T any](a *arena) *T {
	p := a.dec.AllocTemp(unsafe.Sizeof(*new(T)))
	if p == nil {
		panic("vkgl: arena exhausted")
	}
	return (*T)(p)
}

// array returns n new, zeroed Ts of a.
func array[T any](a *arena, n int) []T {
	if n == 0 {
		return nil
	}
	p := a.dec.AllocTempArray(unsafe.Sizeof(*new(T)), uint64(n))
	if p == nil {
		panic("vkgl: arena exhausted")
	}
	return unsafe.Slice((*T)(p), n)
}

// cstrings returns a C array of C strings of strs, of a.
func cstrings(a *arena, strs []string) unsafe.Pointer {
	if len(strs) == 0 {
		return nil
	}
	arr := array[unsafe.Pointer](a, len(strs))
	for i, s := range strs {
		b := array[byte](a, len(s)+1)
		copy(b, s)
		arr[i] = unsafe.Pointer(&b[0])
	}
	return unsafe.Pointer(&arr[0])
}

// The Vulkan versions, VK_MAKE_API_VERSION's.
func apiVersion(major, minor uint32) uint32 {
	return major<<22 | minor<<12
}

// Vulkan's enumerations' values that the package uses, of vulkan_core.h.
const (
	physicalDeviceTypeCPU = 4

	queueGraphics = 0x1
	queueCompute  = 0x2

	memoryDeviceLocal  = 0x1
	memoryHostVisible  = 0x2
	memoryHostCoherent = 0x4

	instanceCreateEnumeratePortability = 0x1
)

// Device is a Vulkan device of the renderer's, on which contexts render:
// a GPU's, or, of a host that has none, a software rasterizer's.
type Device struct {
	// mu serializes the device's calls of its arena, and qmu its queue's.
	mu  sync.Mutex
	qmu sync.Mutex
	a   *arena

	lib       *vk.Library
	inst      vn.Handle
	instTable vk.Table
	pdTable   vk.Table
	pd        vn.Handle
	dev       vn.Handle
	table     vk.Table
	queue     vn.Handle
	family    uint32

	props    vn.VkPhysicalDeviceProperties
	memProps vn.VkPhysicalDeviceMemoryProperties
	// The device's features, which it enables, of Vulkan 1.0 and 1.3.
	features10 vn.VkPhysicalDeviceFeatures
	features13 vn.VkPhysicalDeviceVulkan13Features
	// mapMemory is the device's vkMapMemory.
	mapMemory unsafe.Pointer
	// features are the formats' properties.
	features map[uint32]vn.VkFormatProperties
	// cmdPool is the pool of Record's command buffers, and descPool of
	// their descriptor sets.
	cmdPool  vn.Handle
	descPool vn.Handle
	// Name is the physical device's name.
	Name string
}

var (
	libraryOnce sync.Once
	library     *vk.Library
	libraryErr  error
)

// loadLibrary loads Vulkan once: on macOS, MoltenVK, which the renderer
// links, and elsewhere the host's loader.
func loadLibrary() (*vk.Library, error) {
	libraryOnce.Do(func() {
		if linked, ok := vk.Linked(); ok {
			library = linked
			return
		}
		library, libraryErr = vk.Open("libvulkan.so.1", "libvulkan.so")
		if libraryErr != nil {
			libraryErr = fmt.Errorf("loading Vulkan: %w", libraryErr)
		}
	})
	return library, libraryErr
}

// errVulkan is an error of a Vulkan call's result.
type errVulkan struct {
	call string
	ret  int32
}

func (e *errVulkan) Error() string {
	return fmt.Sprintf("%s: VkResult %d", e.call, e.ret)
}

// check returns an error of a call's result, unless it succeeded.
func check(call string, ret int32) error {
	if ret == vn.VK_SUCCESS {
		return nil
	}
	return &errVulkan{call, ret}
}

// NewDevice returns a device of the host's first GPU, or, of a host that
// has none, of its first software rasterizer, of Vulkan 1.3.
func NewDevice() (*Device, error) {
	lib, err := loadLibrary()
	if err != nil {
		return nil, err
	}
	d := &Device{a: newArena(), lib: lib}
	if err := d.init(); err != nil {
		d.Destroy()
		return nil, err
	}
	return d, nil
}

func (d *Device) init() error {
	a := d.a
	defer a.reset()

	app := get[vn.VkApplicationInfo](a)
	app.SType = vn.VK_STRUCTURE_TYPE_APPLICATION_INFO
	app.ApiVersion = apiVersion(1, 3)
	info := get[vn.VkInstanceCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO
	info.PApplicationInfo = unsafe.Pointer(app)
	if runtime.GOOS == "darwin" {
		info.Flags |= instanceCreateEnumeratePortability
		info.EnabledExtensionCount = 1
		info.PpEnabledExtensionNames = cstrings(a, []string{"VK_KHR_portability_enumeration"})
	}
	inst := get[vn.Handle](a)
	ci := get[vn.Args_vkCreateInstance](a)
	ci.PCreateInfo, ci.PInstance = unsafe.Pointer(info), unsafe.Pointer(inst)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateInstance_EXT, d.lib.Global(), unsafe.Pointer(ci))
	if err := check("vkCreateInstance", ci.Ret); err != nil {
		return err
	}
	d.inst = *inst
	d.instTable, d.pdTable = d.lib.InstanceTables(vk.Handle(d.inst))

	pd, err := d.choosePhysicalDevice()
	if err != nil {
		return err
	}
	d.pd = pd
	props := get[vn.VkPhysicalDeviceProperties](a)
	gp := get[vn.Args_vkGetPhysicalDeviceProperties](a)
	gp.PhysicalDevice, gp.PProperties = d.pd, unsafe.Pointer(props)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties_EXT, d.pdTable, unsafe.Pointer(gp))
	d.props = *props
	d.Name = cString(d.props.DeviceName[:])
	if d.props.ApiVersion < apiVersion(1, 3) {
		return fmt.Errorf("%s has Vulkan %d.%d, not 1.3", d.Name, d.props.ApiVersion>>22, d.props.ApiVersion>>12&0x3ff)
	}
	mp := get[vn.Args_vkGetPhysicalDeviceMemoryProperties](a)
	memProps := get[vn.VkPhysicalDeviceMemoryProperties](a)
	mp.PhysicalDevice, mp.PMemoryProperties = d.pd, unsafe.Pointer(memProps)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceMemoryProperties_EXT, d.pdTable, unsafe.Pointer(mp))
	d.memProps = *memProps

	if d.family, err = d.chooseQueueFamily(); err != nil {
		return err
	}
	return d.createDevice()
}

// cString returns the string of chars, a NUL-terminated array.
func cString(chars []int8) string {
	b := make([]byte, 0, len(chars))
	for _, c := range chars {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// choosePhysicalDevice returns the first GPU, or the first software
// rasterizer.
func (d *Device) choosePhysicalDevice() (vn.Handle, error) {
	a := d.a
	count := get[uint32](a)
	ep := get[vn.Args_vkEnumeratePhysicalDevices](a)
	ep.Instance, ep.PPhysicalDeviceCount = d.inst, unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, d.instTable, unsafe.Pointer(ep))
	if err := check("vkEnumeratePhysicalDevices", ep.Ret); err != nil {
		return 0, err
	}
	if *count == 0 {
		return 0, errors.New("Vulkan has no physical device")
	}
	handles := array[vn.Handle](a, int(*count))
	ep.PPhysicalDevices = unsafe.Pointer(&handles[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, d.instTable, unsafe.Pointer(ep))
	if ep.Ret != vn.VK_SUCCESS && ep.Ret != vn.VK_INCOMPLETE {
		return 0, check("vkEnumeratePhysicalDevices", ep.Ret)
	}
	var cpu vn.Handle
	for _, h := range handles[:*count] {
		props := get[vn.VkPhysicalDeviceProperties](a)
		gp := get[vn.Args_vkGetPhysicalDeviceProperties](a)
		gp.PhysicalDevice, gp.PProperties = h, unsafe.Pointer(props)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties_EXT, d.pdTable, unsafe.Pointer(gp))
		if props.DeviceType != physicalDeviceTypeCPU {
			return h, nil
		}
		if cpu == 0 {
			cpu = h
		}
	}
	return cpu, nil
}

// chooseQueueFamily returns the first family of graphics and compute
// queues.
func (d *Device) chooseQueueFamily() (uint32, error) {
	a := d.a
	count := get[uint32](a)
	qf := get[vn.Args_vkGetPhysicalDeviceQueueFamilyProperties](a)
	qf.PhysicalDevice, qf.PQueueFamilyPropertyCount = d.pd, unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties_EXT, d.pdTable, unsafe.Pointer(qf))
	families := array[vn.VkQueueFamilyProperties](a, int(*count))
	if len(families) == 0 {
		return 0, errors.New("no queue family")
	}
	qf.PQueueFamilyProperties = unsafe.Pointer(&families[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties_EXT, d.pdTable, unsafe.Pointer(qf))
	for i, f := range families[:*count] {
		if f.QueueFlags&(queueGraphics|queueCompute) == queueGraphics|queueCompute {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("%s has no queue of graphics and compute", d.Name)
}

// createDevice creates the device, of its physical device's every feature
// of Vulkan 1.0 to 1.3, and gets its queue.
func (d *Device) createDevice() error {
	a := d.a
	f13 := get[vn.VkPhysicalDeviceVulkan13Features](a)
	f13.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES
	f12 := get[vn.VkPhysicalDeviceVulkan12Features](a)
	f12.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES
	f12.PNext = unsafe.Pointer(f13)
	f11 := get[vn.VkPhysicalDeviceVulkan11Features](a)
	f11.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES
	f11.PNext = unsafe.Pointer(f12)
	features := get[vn.VkPhysicalDeviceFeatures2](a)
	features.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2
	features.PNext = unsafe.Pointer(f11)
	gf := get[vn.Args_vkGetPhysicalDeviceFeatures2](a)
	gf.PhysicalDevice, gf.PFeatures = d.pd, unsafe.Pointer(features)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFeatures2_EXT, d.pdTable, unsafe.Pointer(gf))
	if f13.DynamicRendering == 0 {
		return fmt.Errorf("%s has no dynamic rendering", d.Name)
	}
	d.features10 = features.Features
	d.features13 = *f13
	d.features13.PNext = nil

	priority := get[float32](a)
	*priority = 1
	queue := get[vn.VkDeviceQueueCreateInfo](a)
	queue.SType = vn.VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO
	queue.QueueFamilyIndex = d.family
	queue.QueueCount = 1
	queue.PQueuePriorities = unsafe.Pointer(priority)
	info := get[vn.VkDeviceCreateInfo](a)
	info.SType = vn.VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO
	// Every feature that the device has.
	info.PNext = unsafe.Pointer(features)
	info.QueueCreateInfoCount = 1
	info.PQueueCreateInfos = unsafe.Pointer(queue)
	var exts []string
	if runtime.GOOS == "darwin" {
		exts = append(exts, "VK_KHR_portability_subset")
	}
	info.EnabledExtensionCount = uint32(len(exts))
	info.PpEnabledExtensionNames = cstrings(a, exts)
	dev := get[vn.Handle](a)
	cd := get[vn.Args_vkCreateDevice](a)
	cd.PhysicalDevice, cd.PCreateInfo, cd.PDevice = d.pd, unsafe.Pointer(info), unsafe.Pointer(dev)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateDevice_EXT, d.pdTable, unsafe.Pointer(cd))
	if err := check("vkCreateDevice", cd.Ret); err != nil {
		return err
	}
	d.dev = *dev
	var names []unsafe.Pointer
	if len(exts) != 0 {
		names = unsafe.Slice((*unsafe.Pointer)(info.PpEnabledExtensionNames), len(exts))
	}
	d.table = vk.DeviceTable(d.pdTable, vk.Handle(d.dev), apiVersion(1, 3), names)

	q := get[vn.Handle](a)
	gq := get[vn.Args_vkGetDeviceQueue](a)
	gq.Device, gq.QueueFamilyIndex, gq.PQueue = d.dev, d.family, unsafe.Pointer(q)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetDeviceQueue_EXT, d.table, unsafe.Pointer(gq))
	d.queue = *q
	return nil
}

// Destroy destroys the device, whose objects must be destroyed.
func (d *Device) Destroy() {
	a := d.a
	defer a.reset()
	if d.dev != 0 {
		w := get[vn.Args_vkDeviceWaitIdle](a)
		w.Device = d.dev
		vk.Call(vn.VK_COMMAND_TYPE_vkDeviceWaitIdle_EXT, d.table, unsafe.Pointer(w))
		if d.cmdPool != 0 {
			dp := get[vn.Args_vkDestroyCommandPool](a)
			dp.Device, dp.CommandPool = d.dev, d.cmdPool
			vk.Call(vn.VK_COMMAND_TYPE_vkDestroyCommandPool_EXT, d.table, unsafe.Pointer(dp))
		}
		if d.descPool != 0 {
			dp := get[vn.Args_vkDestroyDescriptorPool](a)
			dp.Device, dp.DescriptorPool = d.dev, d.descPool
			vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDescriptorPool_EXT, d.table, unsafe.Pointer(dp))
		}
		dd := get[vn.Args_vkDestroyDevice](a)
		dd.Device = d.dev
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDevice_EXT, d.table, unsafe.Pointer(dd))
		d.table.Free()
		d.dev = 0
	}
	if d.inst != 0 {
		di := get[vn.Args_vkDestroyInstance](a)
		di.Instance = d.inst
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyInstance_EXT, d.instTable, unsafe.Pointer(di))
		d.instTable.Free()
		d.pdTable.Free()
		d.inst = 0
	}
}

// memoryType returns the index of the first memory type of bits that has
// the properties want.
func (d *Device) memoryType(bits, want uint32) (uint32, bool) {
	for i := uint32(0); i < d.memProps.MemoryTypeCount; i++ {
		if bits&(1<<i) != 0 && d.memProps.MemoryTypes[i].PropertyFlags&want == want {
			return i, true
		}
	}
	return 0, false
}
