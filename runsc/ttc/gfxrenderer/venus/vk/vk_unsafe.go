// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

/*
#cgo CFLAGS: -I${SRCDIR}/include -I${SRCDIR} -DVK_NO_PROTOTYPES -DVK_USE_PLATFORM_METAL_EXT
#cgo linux LDFLAGS: -ldl
#cgo darwin LDFLAGS: -lobjc
#include <dlfcn.h>
#include <stdlib.h>
#include "zcall.h"
#include "metal.h"

static void *vk_open(const char *path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static PFN_vkGetInstanceProcAddr vk_gipa(void *lib) {
	return (PFN_vkGetInstanceProcAddr)dlsym(lib, "vkGetInstanceProcAddr");
}

// vk_pin_drivers creates an instance that the process keeps, of which the
// loader keeps its drivers loaded (see pinDrivers).
static VkResult vk_pin_drivers(const struct vn_global_proc_table *t) {
	VkApplicationInfo app = {
		.sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
		.apiVersion = VK_API_VERSION_1_0,
	};
	VkInstanceCreateInfo info = {
		.sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
		.pApplicationInfo = &app,
	};
	VkInstance instance;
	return t->CreateInstance(&info, NULL, &instance);
}

static void vk_init_global(PFN_vkGetInstanceProcAddr gipa, struct vn_global_proc_table *t) {
	vn_util_init_global_proc_table(gipa, t);
}

static void vk_init_instance(uint64_t instance, PFN_vkGetInstanceProcAddr gipa,
		struct vn_instance_proc_table *it, struct vn_physical_device_proc_table *pt) {
	vn_util_init_instance_proc_table((VkInstance)(uintptr_t)instance, gipa, it);
	vn_util_init_physical_device_proc_table((VkInstance)(uintptr_t)instance, gipa, pt);
}

static void vk_init_device(uint64_t device, const struct vn_physical_device_proc_table *pt,
		uint32_t api_version, const char *const *names, uint32_t count,
		struct vn_device_proc_table *dt) {
	struct vn_info_extension_table ext;
	memset(&ext, 0, sizeof(ext));
	for (uint32_t i = 0; i < count; i++) {
		int32_t index = vn_info_extension_index(names[i]);
		if (index >= 0)
			ext.enabled[index] = true;
	}
	vn_util_init_device_proc_table((VkDevice)(uintptr_t)device, pt->GetDeviceProcAddr,
		api_version, &ext, dt);
}

static PFN_vkVoidFunction vk_device_proc(uint64_t device,
		const struct vn_physical_device_proc_table *pt, const char *name) {
	return pt->GetDeviceProcAddr((VkDevice)(uintptr_t)device, name);
}

static uint32_t vk_extension_count(void) {
	return _vn_info_extension_count;
}

static const struct vn_info_extension *vk_extension(uint32_t i) {
	return &_vn_info_extensions[i];
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Handle is a Vulkan handle, dispatchable or not.
type Handle uint64

// Library is a Vulkan loader, or driver, that the renderer loaded or links.
type Library struct {
	// handle is the loaded library's, or nil for the linked one.
	handle unsafe.Pointer
	gipa   C.PFN_vkGetInstanceProcAddr
	global *C.struct_vn_global_proc_table
}

func newLibrary(handle unsafe.Pointer, gipa C.PFN_vkGetInstanceProcAddr) *Library {
	l := &Library{
		handle: handle,
		gipa:   gipa,
		global: (*C.struct_vn_global_proc_table)(C.calloc(1, C.sizeof_struct_vn_global_proc_table)),
	}
	C.vk_init_global(gipa, l.global)
	return l
}

// pinDrivers creates, of the first library opened, an instance that the
// process keeps. The loader loads its drivers for each instance, and unloads
// them of the last, and glibc reclaims no static TLS of a library that it
// unloads, of which NVIDIA's driver, libnvidia-tls, takes some each time
// that it loads: of the 16th instance created while none lived, the driver
// loads no more ("cannot allocate memory in static TLS block"), and Vulkan
// has no NVIDIA GPU, as of guests' instances, which each serves one of, and
// vkgl's tests' devices.
var pinDrivers sync.Once

// Open loads the first of paths that loads, a loader, whose drivers stay
// loaded for the process (pinDrivers).
func Open(paths ...string) (*Library, error) {
	var errs []error
	for _, path := range paths {
		cpath := C.CString(path)
		h := C.vk_open(cpath)
		C.free(unsafe.Pointer(cpath))
		if h == nil {
			errs = append(errs, fmt.Errorf("%s: %s", path, C.GoString(C.dlerror())))
			continue
		}
		gipa := C.vk_gipa(h)
		if gipa == nil {
			C.dlclose(h)
			errs = append(errs, fmt.Errorf("%s has no vkGetInstanceProcAddr", path))
			continue
		}
		l := newLibrary(h, gipa)
		pinDrivers.Do(func() {
			// Of a loader of no drivers, nothing: no instance loads any.
			C.vk_pin_drivers(l.global)
		})
		return l, nil
	}
	return nil, errors.Join(errs...)
}

// Close unloads the library, whose tables must be no longer used.
func (l *Library) Close() {
	C.free(unsafe.Pointer(l.global))
	if l.handle != nil {
		C.dlclose(l.handle)
	}
}

// Table is a procedure table, of the commands of one level: global, or of an
// instance, its physical devices, or a device.
type Table struct {
	p unsafe.Pointer
}

// Global returns the library's table of global commands.
func (l *Library) Global() Table {
	return Table{unsafe.Pointer(l.global)}
}

// InstanceTables returns the tables of an instance's commands, and of its
// physical devices'.
func (l *Library) InstanceTables(instance Handle) (Table, Table) {
	it := C.calloc(1, C.sizeof_struct_vn_instance_proc_table)
	pt := C.calloc(1, C.sizeof_struct_vn_physical_device_proc_table)
	C.vk_init_instance(C.uint64_t(instance), l.gipa, (*C.struct_vn_instance_proc_table)(it),
		(*C.struct_vn_physical_device_proc_table)(pt))
	return Table{it}, Table{pt}
}

// DeviceTable returns the table of a device's commands. physicalDevice is
// its physical device's table; apiVersion and extensions, which may be in
// memory that is not Go's, are the device's, which decide which commands it
// has.
func DeviceTable(physicalDevice Table, device Handle, apiVersion uint32, extensions []unsafe.Pointer) Table {
	dt := C.calloc(1, C.sizeof_struct_vn_device_proc_table)
	var names unsafe.Pointer
	if len(extensions) != 0 {
		names = C.calloc(C.size_t(len(extensions)), C.size_t(unsafe.Sizeof(uintptr(0))))
		copy(unsafe.Slice((*unsafe.Pointer)(names), len(extensions)), extensions)
		defer C.free(names)
	}
	C.vk_init_device(C.uint64_t(device), (*C.struct_vn_physical_device_proc_table)(physicalDevice.p),
		C.uint32_t(apiVersion), (**C.char)(names), C.uint32_t(len(extensions)),
		(*C.struct_vn_device_proc_table)(dt))
	return Table{dt}
}

// DeviceProc returns a device's command name, or nil if it has none.
func DeviceProc(physicalDevice Table, device Handle, name string) unsafe.Pointer {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return unsafe.Pointer(C.vk_device_proc(C.uint64_t(device),
		(*C.struct_vn_physical_device_proc_table)(physicalDevice.p), cname))
}

// Free frees the table.
func (t Table) Free() {
	C.free(t.p)
}

// Valid reports whether the table exists.
func (t Table) Valid() bool {
	return t.p != nil
}

// Call calls the command of type typ, a VkCommandTypeEXT, with args, the
// command's arguments (vn.Args_*), through t, the table of the command's
// level. args must be memory that is not Go's, or hold no pointer to Go's.
// It reports false, calling nothing, if the table lacks the command.
func Call(typ int32, t Table, args unsafe.Pointer) bool {
	return bool(C.vn_call(C.int32_t(typ), t.p, args))
}

// Extension is an extension of the protocol.
type Extension struct {
	Name        string
	Number      uint32
	SpecVersion uint32
}

// Extensions returns the extensions of the protocol, sorted by name.
func Extensions() []Extension {
	n := int(C.vk_extension_count())
	exts := make([]Extension, 0, n)
	for i := 0; i < n; i++ {
		e := C.vk_extension(C.uint32_t(i))
		exts = append(exts, Extension{
			Name:        C.GoString(e.name),
			Number:      uint32(e.number),
			SpecVersion: uint32(e.spec_version),
		})
	}
	return exts
}

// WireFormatVersion returns the version of the protocol's wire format.
func WireFormatVersion() uint32 {
	return uint32(C.vn_info_wire_format_version())
}

// VkXMLVersion returns the version of vk.xml of the protocol.
func VkXMLVersion() uint32 {
	return uint32(C.vn_info_vk_xml_version())
}
