// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"errors"
	"fmt"
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

// release frees the arena's memory, of an arena no longer used.
func (a *arena) release() {
	a.reset()
	a.pool.Release()
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

// portabilitySubsetFeatures is vulkan_beta.h's
// VkPhysicalDevicePortabilitySubsetFeaturesKHR, which venus-protocol has
// not: what a device of VK_KHR_portability_subset has of Vulkan's.
type portabilitySubsetFeatures struct {
	SType                                  uint32
	PNext                                  unsafe.Pointer
	ConstantAlphaColorBlendFactors         uint32
	Events                                 uint32
	ImageViewFormatReinterpretation        uint32
	ImageViewFormatSwizzle                 uint32
	ImageView2DOn3DImage                   uint32
	MultisampleArrayImage                  uint32
	MutableComparisonSamplers              uint32
	PointPolygons                          uint32
	SamplerMipLodBias                      uint32
	SeparateStencilMaskRef                 uint32
	ShaderSampleRateInterpolationFunctions uint32
	TessellationIsolines                   uint32
	TessellationPointMode                  uint32
	TriangleFans                           uint32
	VertexAttributeAccessBeyondStride      uint32
}

// Device is a Vulkan device of the renderer's, on which contexts render:
// a GPU's, or, of a host that has none, a software rasterizer's.
type Device struct {
	// mu serializes the device's calls of its arena, and qmu its queue's.
	mu  sync.Mutex
	qmu sync.Mutex
	a   *arena
	// waitArenas are arenas of fences' waits, of any goroutine, which
	// waitMu guards.
	waitMu     sync.Mutex
	waitArenas []*arena

	lib       *vk.Library
	inst      vn.Handle
	instTable vk.Table
	pdTable   vk.Table
	pd        vn.Handle
	dev       vn.Handle
	table     vk.Table
	queue     vn.Handle
	family    uint32
	// timestampBits are the bits of the queue's timestamps, or 0 if it
	// has none.
	timestampBits uint32

	props    vn.VkPhysicalDeviceProperties
	memProps vn.VkPhysicalDeviceMemoryProperties
	// The device's features, which it enables, of Vulkan 1.0 and 1.3.
	features10 vn.VkPhysicalDeviceFeatures
	features13 vn.VkPhysicalDeviceVulkan13Features
	// The features of extensions that the device enables where it has
	// them, of Gallium's that Vulkan 1.3 lacks: indexes of a byte, flat
	// varyings of primitives' last vertices, of which pipelines may
	// differ in a rendering instance, and instances' divisors.
	indexUint8, provokingLast, divisors bool
	// The features of Vulkan that a device of VK_KHR_portability_subset,
	// as MoltenVK, macOS's Vulkan, may lack, which the package works
	// around, and every other device has: triangle fans, polygons drawn
	// as points, samplers' LOD bias, stencil references of each face,
	// and multisampled arrays. MoltenVK on Macs has the subset's others
	// that the package uses: constant alpha blend factors, updates of
	// comparison samplers' descriptors, and attributes past their
	// bindings' strides.
	triangleFans, pointPolygons, samplerMipLodBias, separateStencilRef, multisampleArrays bool
	// view2DOn3D is whether 3D images' slices are viewed as 2D images'
	// layers, of images created to be.
	view2DOn3D bool
	// alignedVertices is whether the device fetches vertex elements of
	// addresses and strides of multiples of 4 alone, as Metal does.
	alignedVertices bool
	// imageViews2DOf3D is whether shaders store to 2D views of 3D images'
	// slices, of VK_EXT_image_2d_view_of_3d, of images created to be.
	imageViews2DOf3D bool
	// floatImageAtomics is whether shaders exchange texels of images of
	// floats atomically, of VK_EXT_shader_atomic_float.
	floatImageAtomics bool
	// bresenhamLines is whether the device draws lines of Bresenham's
	// rule, OpenGL's diamond exit, of VK_KHR_line_rasterization, rather
	// than as rectangles, as Vulkan's lines are of none.
	bresenhamLines bool
	// workgroupFences is whether shaders' fences of the device's scope are
	// of the workgroup's, of MoltenVK: Apple's GPU compiler crashes of a
	// fence of the device's scope before an atomic's read-modify-write of
	// device memory (macOS 26.7, M5), which MoltenVK's MSL before 3.2
	// fenced of the threadgroup's scope too.
	workgroupFences bool
	// substitutes are the Vulkan formats that the device renders depths
	// to in place of those that it lacks, by the formats lacked.
	substitutes map[uint32]uint32
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
	// The loader lists the devices of VK_KHR_portability_subset, as
	// MoltenVK, only to an instance that enumerates them; MoltenVK that
	// the renderer links has no loader, nor the extension.
	if d.instanceExtension("VK_KHR_portability_enumeration") {
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
			d.timestampBits = f.TimestampValidBits
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("%s has no queue of graphics and compute", d.Name)
}

// extensions returns the names of the physical device's extensions.
// instanceExtension reports whether Vulkan has an instance extension.
func (d *Device) instanceExtension(name string) bool {
	a := newArena()
	defer a.release()
	count := get[uint32](a)
	ee := get[vn.Args_vkEnumerateInstanceExtensionProperties](a)
	ee.PPropertyCount = unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, d.lib.Global(), unsafe.Pointer(ee))
	if ee.Ret != vn.VK_SUCCESS || *count == 0 {
		return false
	}
	props := array[vn.VkExtensionProperties](a, int(*count))
	ee.PProperties = unsafe.Pointer(&props[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceExtensionProperties_EXT, d.lib.Global(), unsafe.Pointer(ee))
	if ee.Ret < 0 {
		return false
	}
	for _, p := range props[:min(int(*count), len(props))] {
		if cString(p.ExtensionName[:]) == name {
			return true
		}
	}
	return false
}

func (d *Device) extensions() map[string]bool {
	a := d.a
	has := make(map[string]bool)
	count := get[uint32](a)
	ee := get[vn.Args_vkEnumerateDeviceExtensionProperties](a)
	ee.PhysicalDevice, ee.PPropertyCount = d.pd, unsafe.Pointer(count)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, d.pdTable, unsafe.Pointer(ee))
	if ee.Ret != vn.VK_SUCCESS || *count == 0 {
		return has
	}
	props := array[vn.VkExtensionProperties](a, int(*count))
	ee.PProperties = unsafe.Pointer(&props[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateDeviceExtensionProperties_EXT, d.pdTable, unsafe.Pointer(ee))
	if ee.Ret < 0 {
		return has
	}
	for _, p := range props[:min(int(*count), len(props))] {
		has[cString(p.ExtensionName[:])] = true
	}
	return has
}

// createDevice creates the device, of its physical device's every feature
// of Vulkan 1.0 to 1.3, and of the extensions of Device's features, and
// gets its queue.
func (d *Device) createDevice() error {
	a := d.a
	has := d.extensions()
	ext := func(names ...string) string {
		for _, n := range names {
			if has[n] {
				return n
			}
		}
		return ""
	}
	f13 := get[vn.VkPhysicalDeviceVulkan13Features](a)
	f13.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES
	// The features of the extensions that the device has, after Vulkan
	// 1.3's.
	tail := &f13.PNext
	link := func(name string, feature unsafe.Pointer, next *unsafe.Pointer) {
		if name != "" {
			*tail, tail = feature, next
		}
	}
	idx8 := get[vn.VkPhysicalDeviceIndexTypeUint8Features](a)
	idx8.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_INDEX_TYPE_UINT8_FEATURES
	idx8Ext := ext("VK_KHR_index_type_uint8", "VK_EXT_index_type_uint8")
	link(idx8Ext, unsafe.Pointer(idx8), &idx8.PNext)
	pv := get[vn.VkPhysicalDeviceProvokingVertexFeaturesEXT](a)
	pv.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROVOKING_VERTEX_FEATURES_EXT
	pvExt := ext("VK_EXT_provoking_vertex")
	link(pvExt, unsafe.Pointer(pv), &pv.PNext)
	div := get[vn.VkPhysicalDeviceVertexAttributeDivisorFeatures](a)
	div.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VERTEX_ATTRIBUTE_DIVISOR_FEATURES
	divExt := ext("VK_KHR_vertex_attribute_divisor", "VK_EXT_vertex_attribute_divisor")
	link(divExt, unsafe.Pointer(div), &div.PNext)
	v3 := get[vn.VkPhysicalDeviceImage2DViewOf3DFeaturesEXT](a)
	v3.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_2D_VIEW_OF_3D_FEATURES_EXT
	v3Ext := ext("VK_EXT_image_2d_view_of_3d")
	link(v3Ext, unsafe.Pointer(v3), &v3.PNext)
	lr := get[vn.VkPhysicalDeviceLineRasterizationFeatures](a)
	lr.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_LINE_RASTERIZATION_FEATURES
	lrExt := ext("VK_KHR_line_rasterization", "VK_EXT_line_rasterization")
	link(lrExt, unsafe.Pointer(lr), &lr.PNext)
	af := get[vn.VkPhysicalDeviceShaderAtomicFloatFeaturesEXT](a)
	af.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_ATOMIC_FLOAT_FEATURES_EXT
	afExt := ext("VK_EXT_shader_atomic_float")
	link(afExt, unsafe.Pointer(af), &af.PNext)
	ps := get[portabilitySubsetFeatures](a)
	ps.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PORTABILITY_SUBSET_FEATURES_KHR
	psExt := ext("VK_KHR_portability_subset")
	link(psExt, unsafe.Pointer(ps), &ps.PNext)
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
	var exts []string
	d.triangleFans, d.pointPolygons, d.samplerMipLodBias, d.separateStencilRef, d.multisampleArrays = true, true, true, true, true
	d.view2DOn3D = true
	if psExt != "" {
		// Which a device that has it requires enabled.
		exts = append(exts, psExt)
		// Views' swizzles are textures' swizzles, which OpenGL ES 3.0
		// has, and those of the formats that the package emulates.
		if ps.ImageViewFormatSwizzle == 0 {
			return fmt.Errorf("%s swizzles no image views", d.Name)
		}
		d.triangleFans = ps.TriangleFans != 0
		d.pointPolygons = ps.PointPolygons != 0
		d.samplerMipLodBias = ps.SamplerMipLodBias != 0
		d.separateStencilRef = ps.SeparateStencilMaskRef != 0
		d.multisampleArrays = ps.MultisampleArrayImage != 0
		d.view2DOn3D = ps.ImageView2DOn3DImage != 0
		d.alignedVertices = true
		d.workgroupFences = true
	}
	d.initSubstitutes()
	if idx8Ext != "" {
		exts = append(exts, idx8Ext)
		d.indexUint8 = idx8.IndexTypeUint8 != 0
	}
	if pvExt != "" {
		exts = append(exts, pvExt)
		// Of no transform feedback, which the device does not enable.
		pv.TransformFeedbackPreservesProvokingVertex = 0
		pvProps := get[vn.VkPhysicalDeviceProvokingVertexPropertiesEXT](a)
		pvProps.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROVOKING_VERTEX_PROPERTIES_EXT
		props := get[vn.VkPhysicalDeviceProperties2](a)
		props.SType, props.PNext = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2, unsafe.Pointer(pvProps)
		gp := get[vn.Args_vkGetPhysicalDeviceProperties2](a)
		gp.PhysicalDevice, gp.PProperties = d.pd, unsafe.Pointer(props)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties2_EXT, d.pdTable, unsafe.Pointer(gp))
		d.provokingLast = pv.ProvokingVertexLast != 0 && pvProps.ProvokingVertexModePerPipeline != 0
	}
	if divExt != "" {
		exts = append(exts, divExt)
		d.divisors = div.VertexAttributeInstanceRateDivisor != 0
	}
	if lrExt != "" {
		exts = append(exts, lrExt)
		d.bresenhamLines = lr.BresenhamLines != 0
	}
	if afExt != "" {
		exts = append(exts, afExt)
		d.floatImageAtomics = af.ShaderImageFloat32Atomics != 0
	}
	if v3Ext != "" {
		exts = append(exts, v3Ext)
		d.imageViews2DOf3D = d.view2DOn3D && v3.Image2DViewOf3D != 0
	}

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

// The Vulkan formats of depth that Apple's GPUs lack, their substitutes,
// and virgl's format of 24-bit depths alone.
const (
	vkX8D24    = 125 // VK_FORMAT_X8_D24_UNORM_PACK32
	vkD32      = 126 // VK_FORMAT_D32_SFLOAT
	vkD24S8    = 129 // VK_FORMAT_D24_UNORM_S8_UINT
	vkD32S8    = 130 // VK_FORMAT_D32_SFLOAT_S8_UINT
	virglZ24X8 = 21  // PIPE_FORMAT_Z24X8_UNORM
)

// initSubstitutes settles the formats of depth that the device renders
// depths to in place of those that it lacks, as Zink does: of 24-bit
// depths, which Apple's GPUs lack, 32-bit floats. Guests' OpenGL has no
// other format of GL_DEPTH24_STENCIL8, which Android's EGL configurations
// are of, and takes 16-bit depths for GL_DEPTH_COMPONENT24. A polygon
// offset's units are then of floats' precision, which is that of 24-bit
// depths at depths of 0.5 and more, and finer nearer.
func (d *Device) initSubstitutes() {
	d.substitutes = make(map[uint32]uint32)
	for _, s := range []struct{ lacked, sub uint32 }{{vkD24S8, vkD32S8}, {vkX8D24, vkD32}} {
		if d.formatFeatures(s.lacked)&featureDepthStencil == 0 && d.formatFeatures(s.sub)&featureDepthStencil != 0 {
			d.substitutes[s.lacked] = s.sub
		}
	}
}

// LookupFormat returns virgl's format f as the device has or emulates it:
// as the package's LookupFormat, of the device's substitutes.
func (d *Device) LookupFormat(f uint32) (Format, bool) {
	ft, ok := LookupFormat(f)
	if sub, ok := d.substitutes[ft.VK]; ok {
		ft.VK = sub
	}
	return ft, ok
}

// Destroy destroys the device, whose objects must be destroyed.
func (d *Device) Destroy() {
	a := d.a
	defer func() {
		a.release()
		for _, w := range d.waitArenas {
			w.release()
		}
		d.waitArenas = nil
	}()
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

// waitArena returns an arena of a fence's wait, of any goroutine, which
// putWaitArena takes back.
func (d *Device) waitArena() *arena {
	d.waitMu.Lock()
	defer d.waitMu.Unlock()
	if n := len(d.waitArenas); n != 0 {
		a := d.waitArenas[n-1]
		d.waitArenas = d.waitArenas[:n-1]
		return a
	}
	return newArena()
}

// putWaitArena takes back an arena of waitArena's.
func (d *Device) putWaitArena(a *arena) {
	a.reset()
	d.waitMu.Lock()
	defer d.waitMu.Unlock()
	d.waitArenas = append(d.waitArenas, a)
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

// UniformBufferAlignment returns the alignment of the offsets of uniform
// buffers' bindings.
func (d *Device) UniformBufferAlignment() uint32 {
	return uint32(d.props.Limits.MinUniformBufferOffsetAlignment)
}

// MaxUniformBufferRange returns the most bytes of a uniform buffer's
// binding.
func (d *Device) MaxUniformBufferRange() uint32 {
	return d.props.Limits.MaxUniformBufferRange
}

// FloatImageAtomics reports whether shaders exchange texels of images of
// floats atomically, as tospirv's Options.FloatImageAtomics.
func (d *Device) FloatImageAtomics() bool {
	return d.floatImageAtomics
}
