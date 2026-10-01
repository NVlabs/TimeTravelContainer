// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// layoutDevice is a Vulkan device of the renderer's own, which it asks how
// the host's Vulkan lays out linear images. Android's buffers are laid out
// so (see crossdomain_unsafe.go), so that the images that guests create of
// them, linear images of the host's (see modifier_unsafe.go), share their
// memory.
type layoutDevice struct {
	mu sync.Mutex
	s  *stream

	inst      vn.Handle
	instTable vk.Table
	pdTable   vk.Table
	pd        vn.Handle
	dev       vn.Handle
	table     vk.Table
}

var layouts struct {
	once sync.Once
	dev  *layoutDevice
	err  error
}

// getLayoutDevice returns the renderer's layout device, which it creates
// once.
func getLayoutDevice() (*layoutDevice, error) {
	layouts.once.Do(func() {
		layouts.dev, layouts.err = newLayoutDevice()
	})
	return layouts.dev, layouts.err
}

func newLayoutDevice() (*layoutDevice, error) {
	lib, err := loadLibrary()
	if err != nil {
		return nil, err
	}
	s := &stream{}
	s.dec = vn.NewDecoder(&s.pool, nil)
	defer s.dec.ResetTempPool()
	d := &layoutDevice{s: s}

	app := newArgs[vn.VkApplicationInfo](s)
	app.SType = vn.VK_STRUCTURE_TYPE_APPLICATION_INFO
	app.ApiVersion = apiVersion(1, 1)
	info := newArgs[vn.VkInstanceCreateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO
	info.PApplicationInfo = unsafe.Pointer(app)
	if runtime.GOOS == "darwin" && s.hasInstanceExtensionIn(lib, "VK_KHR_portability_enumeration") {
		info.Flags |= vkInstanceCreateEnumeratePortability
		info.EnabledExtensionCount = 1
		info.PpEnabledExtensionNames = s.cstrings([]string{"VK_KHR_portability_enumeration"})
	}
	out := alloc[vn.Handle](s, 1)
	ci := newArgs[vn.Args_vkCreateInstance](s)
	ci.PCreateInfo, ci.PInstance = unsafe.Pointer(info), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateInstance_EXT, lib.Global(), unsafe.Pointer(ci))
	if ci.Ret != vn.VK_SUCCESS {
		return nil, fmt.Errorf("creating a layout instance: %d", ci.Ret)
	}
	d.inst = *out
	d.instTable, d.pdTable = lib.InstanceTables(vk.Handle(d.inst))

	count := alloc[uint32](s, 1)
	*count = 1
	pds := alloc[vn.Handle](s, 1)
	ep := newArgs[vn.Args_vkEnumeratePhysicalDevices](s)
	ep.Instance, ep.PPhysicalDeviceCount, ep.PPhysicalDevices = d.inst, unsafe.Pointer(count), unsafe.Pointer(pds)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumeratePhysicalDevices_EXT, d.instTable, unsafe.Pointer(ep))
	if (ep.Ret != vn.VK_SUCCESS && ep.Ret != vn.VK_INCOMPLETE) || *count == 0 {
		d.destroy()
		return nil, fmt.Errorf("no physical device for layouts: %d", ep.Ret)
	}
	d.pd = *pds

	priority := alloc[float32](s, 1)
	*priority = 1
	queue := newArgs[vn.VkDeviceQueueCreateInfo](s)
	queue.SType = vn.VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO
	queue.QueueCount = 1
	queue.PQueuePriorities = unsafe.Pointer(priority)
	devInfo := newArgs[vn.VkDeviceCreateInfo](s)
	devInfo.SType = vn.VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO
	devInfo.QueueCreateInfoCount = 1
	devInfo.PQueueCreateInfos = unsafe.Pointer(queue)
	var exts []unsafe.Pointer
	if d.hasDeviceExtension("VK_KHR_portability_subset") {
		names := s.cstrings([]string{"VK_KHR_portability_subset"})
		exts = append(exts, *(*unsafe.Pointer)(names))
		devInfo.EnabledExtensionCount = 1
		devInfo.PpEnabledExtensionNames = names
	}
	dev := alloc[vn.Handle](s, 1)
	cd := newArgs[vn.Args_vkCreateDevice](s)
	cd.PhysicalDevice, cd.PCreateInfo, cd.PDevice = d.pd, unsafe.Pointer(devInfo), unsafe.Pointer(dev)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateDevice_EXT, d.pdTable, unsafe.Pointer(cd))
	if cd.Ret != vn.VK_SUCCESS {
		d.destroy()
		return nil, fmt.Errorf("creating a layout device: %d", cd.Ret)
	}
	d.dev = *dev
	d.table = vk.DeviceTable(d.pdTable, vk.Handle(d.dev), apiVersion(1, 1), exts)
	return d, nil
}

// hasInstanceExtensionIn reports whether lib's Vulkan has the instance
// extension name.
func (s *stream) hasInstanceExtensionIn(lib *vk.Library, name string) bool {
	count := alloc[uint32](s, 1)
	a := newArgs[vn.Args_vkEnumerateInstanceExtensionProperties](s)
	a.PPropertyCount = unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, lib.Global(), unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS || *count == 0 {
		return false
	}
	props := unsafe.Slice(alloc[vn.VkExtensionProperties](s, int(*count)), *count)
	a.PProperties = unsafe.Pointer(&props[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, lib.Global(), unsafe.Pointer(a))
	for i := range props[:*count] {
		if goString(props[i].ExtensionName[:]) == name {
			return true
		}
	}
	return false
}

// hasDeviceExtension reports whether the layout device's physical device
// has the extension name.
func (d *layoutDevice) hasDeviceExtension(name string) bool {
	s := d.s
	count := alloc[uint32](s, 1)
	a := newArgs[vn.Args_vkEnumerateDeviceExtensionProperties](s)
	a.PhysicalDevice, a.PPropertyCount = d.pd, unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, d.pdTable, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS || *count == 0 {
		return false
	}
	props := unsafe.Slice(alloc[vn.VkExtensionProperties](s, int(*count)), *count)
	a.PProperties = unsafe.Pointer(&props[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, d.pdTable, unsafe.Pointer(a))
	for i := range props[:*count] {
		if goString(props[i].ExtensionName[:]) == name {
			return true
		}
	}
	return false
}

func (d *layoutDevice) destroy() {
	s := d.s
	if d.dev != 0 {
		a := newArgs[vn.Args_vkDestroyDevice](s)
		a.Device = d.dev
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyDevice_EXT, d.table, unsafe.Pointer(a))
	}
	if d.inst != 0 {
		a := newArgs[vn.Args_vkDestroyInstance](s)
		a.Instance = d.inst
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyInstance_EXT, d.instTable, unsafe.Pointer(a))
	}
}

// planeLayout is where a plane of an image lies in its memory.
type planeLayout struct {
	offset, rowPitch uint64
}

// imageLayout is how an image lies in its memory.
type imageLayout struct {
	size   uint64
	planes []planeLayout
}

// Image usages, VkImageUsageFlagBits.
const (
	imageUsageSampled         = 0x4
	imageUsageColorAttachment = 0x10
)

// Image aspects of planes, VkImageAspectFlagBits.
var planeAspects = [3]uint32{0x10, 0x20, 0x40}

// linearLayout returns how the host's Vulkan lays out a linear 2D image of
// format, width by height, with usage, of planes planes.
func (d *layoutDevice) linearLayout(format, width, height, usage uint32, planes int) (imageLayout, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.s
	defer s.dec.ResetTempPool()

	info := newArgs[vn.VkImageCreateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO
	info.ImageType = 1 // VK_IMAGE_TYPE_2D
	info.Format = format
	info.Extent = vn.VkExtent3D{Width: width, Height: height, Depth: 1}
	info.MipLevels = 1
	info.ArrayLayers = 1
	info.Samples = 1
	info.Tiling = imageTilingLinear
	info.Usage = usage
	img := alloc[vn.Handle](s, 1)
	ci := newArgs[vn.Args_vkCreateImage](s)
	ci.Device, ci.PCreateInfo, ci.PImage = d.dev, unsafe.Pointer(info), unsafe.Pointer(img)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateImage_EXT, d.table, unsafe.Pointer(ci))
	if ci.Ret != vn.VK_SUCCESS {
		return imageLayout{}, fmt.Errorf("creating a linear image of format %d, %dx%d: %d", format, width, height, ci.Ret)
	}
	defer func() {
		a := newArgs[vn.Args_vkDestroyImage](s)
		a.Device, a.Image = d.dev, *img
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyImage_EXT, d.table, unsafe.Pointer(a))
	}()
	reqs := newArgs[vn.VkMemoryRequirements](s)
	mr := newArgs[vn.Args_vkGetImageMemoryRequirements](s)
	mr.Device, mr.Image, mr.PMemoryRequirements = d.dev, *img, unsafe.Pointer(reqs)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements_EXT, d.table, unsafe.Pointer(mr))
	l := imageLayout{size: reqs.Size}
	for p := 0; p < planes; p++ {
		sub := newArgs[vn.VkImageSubresource](s)
		sub.AspectMask = aspectColor
		if planes > 1 {
			sub.AspectMask = planeAspects[p]
		}
		layout := newArgs[vn.VkSubresourceLayout](s)
		sl := newArgs[vn.Args_vkGetImageSubresourceLayout](s)
		sl.Device, sl.Image, sl.PSubresource, sl.PLayout = d.dev, *img, unsafe.Pointer(sub), unsafe.Pointer(layout)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetImageSubresourceLayout_EXT, d.table, unsafe.Pointer(sl))
		l.planes = append(l.planes, planeLayout{offset: layout.Offset, rowPitch: layout.RowPitch})
	}
	return l, nil
}
