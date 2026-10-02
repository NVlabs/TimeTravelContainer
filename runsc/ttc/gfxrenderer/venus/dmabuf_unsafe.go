// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// External memory, which guests share as dma-bufs, where the host's Vulkan
// has none, as MoltenVK has none.
//
// Mesa's Venus driver offers applications external memory only where the
// renderer offers VK_EXT_external_memory_dma_buf, and Zink, OpenGL on Vulkan,
// requires it of a render node's device. The renderer offers it: external
// memory is host-visible memory, which is shared memory already (see
// memory.go), and which a blob resource exports to the guest, where it is a
// dma-buf. Memory that imports a resource imports its shared memory.
//
// The host's driver sees none of it: the renderer keeps the external memory
// structs of guests' create infos from it, and has the buffers and images
// that guests create as external bind host-visible memory only.

// Values of Vulkan's that the renderer uses beyond vn's.
const (
	handleTypeOpaqueFD      = 0x1
	handleTypeDmaBuf        = 0x200
	emulatedHandleTypes     = handleTypeOpaqueFD | handleTypeDmaBuf
	memoryFeatureExportable = 0x2
	memoryFeatureImportable = 0x4
)

// emulatedExtensions are the device extensions of external memory that the
// renderer offers where the host's Vulkan lacks them.
var emulatedExtensions = []string{"VK_KHR_external_memory_fd", "VK_EXT_external_memory_dma_buf"}

// initExternalMemory settles whether pd offers external memory beyond the
// host's Vulkan.
func (pd *physicalDevice) initExternalMemory(hostExtensions map[string]bool) {
	for i := uint32(0); i < pd.memProps.MemoryTypeCount; i++ {
		if pd.memProps.MemoryTypes[i].PropertyFlags&vkMemoryPropertyHostVisible != 0 {
			pd.hostVisibleTypes |= 1 << i
		}
	}
	// Shared memory backs host-visible memory only where the renderer
	// imports it (hostimport.go). Guests' dma-bufs are blob resources, not
	// the host's, though the host's Vulkan has its own.
	if !pd.importsHostMemory() || pd.hostVisibleTypes == 0 {
		return
	}
	pd.emulateDmaBuf = true
	specs := extensionSpecVersions()
	for _, name := range emulatedExtensions {
		spec, ok := specs[name]
		if !ok || hostExtensions[name] {
			continue
		}
		var e vn.VkExtensionProperties
		setString(e.ExtensionName[:], name)
		e.SpecVersion = spec
		pd.extensions = append(pd.extensions, e)
	}
}

// externalMemoryProperties are the properties of the external memory that
// the renderer emulates.
func externalMemoryProperties(handleType uint32) vn.VkExternalMemoryProperties {
	return vn.VkExternalMemoryProperties{
		ExternalMemoryFeatures:        memoryFeatureExportable | memoryFeatureImportable,
		ExportFromImportedHandleTypes: handleType,
		CompatibleHandleTypes:         handleType,
	}
}

func registerExternalMemory() {
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceExternalBufferProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceExternalBufferProperties)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		info := (*vn.VkPhysicalDeviceExternalBufferInfo)(a.PExternalBufferInfo)
		if pd.emulateDmaBuf && info.HandleType&emulatedHandleTypes != 0 {
			props := (*vn.VkExternalBufferProperties)(a.PExternalBufferProperties)
			props.ExternalMemoryProperties = externalMemoryProperties(info.HandleType)
			return
		}
		s.call(cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceImageFormatProperties2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceImageFormatProperties2)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		info := (*vn.VkPhysicalDeviceImageFormatInfo2)(a.PImageFormatInfo)
		modifier := false
		if pd.emulateModifiers && info.Tiling == imageTilingDrmFormatModifier {
			p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_DRM_FORMAT_MODIFIER_INFO_EXT)
			if p == nil || (*vn.VkPhysicalDeviceImageDrmFormatModifierInfoEXT)(p).DrmFormatModifier != drmFormatModLinear {
				a.Ret = vn.VK_ERROR_FORMAT_NOT_SUPPORTED
				return
			}
			removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_DRM_FORMAT_MODIFIER_INFO_EXT)
			info.Tiling = imageTilingLinear
			modifier = true
		}
		var handleType uint32
		if pd.emulateDmaBuf {
			if p := findStruct(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_IMAGE_FORMAT_INFO); p != nil {
				ext := (*vn.VkPhysicalDeviceExternalImageFormatInfo)(p)
				if ext.HandleType&emulatedHandleTypes != 0 {
					handleType = ext.HandleType
					removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_IMAGE_FORMAT_INFO)
				}
			}
		}
		// The linear image of a guest's dma-buf may be a shadow (see
		// shadow_unsafe.go), which can have what both it and its linear
		// image can.
		var linear *vn.VkImageFormatProperties
		if (modifier || handleType != 0) && s.shadows(pd, info.Format, info.Type, info.Tiling, info.Usage) {
			if linear = s.linearImageFormatProperties(pd, info.Format, info.Type, info.Flags); linear == nil {
				a.Ret = vn.VK_ERROR_FORMAT_NOT_SUPPORTED
				return
			}
			info.Tiling = imageTilingOptimal
			info.Usage |= imageUsageTransfers
		}
		if !s.call(cmd, args) || a.Ret != vn.VK_SUCCESS {
			return
		}
		if linear != nil {
			limitImageFormatProperties(&(*vn.VkImageFormatProperties2)(a.PImageFormatProperties).ImageFormatProperties, linear)
		}
		if handleType == 0 {
			return
		}
		if p := findStruct(a.PImageFormatProperties, vn.VK_STRUCTURE_TYPE_EXTERNAL_IMAGE_FORMAT_PROPERTIES); p != nil {
			(*vn.VkExternalImageFormatProperties)(p).ExternalMemoryProperties = externalMemoryProperties(handleType)
		}
	})

	// Memory requirements of external buffers and images, which bind
	// host-visible memory only.
	handle(vn.VK_COMMAND_TYPE_vkGetBufferMemoryRequirements_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetBufferMemoryRequirements)(args)
		o := s.ctx.lookup(uint64(a.Buffer))
		if s.call(cmd, args) && o != nil && o.external {
			restrictToHostVisible(o.dev, &(*vn.VkMemoryRequirements)(a.PMemoryRequirements).MemoryTypeBits)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetImageMemoryRequirements)(args)
		o := s.ctx.lookup(uint64(a.Image))
		// A shadow's memory is its linear image's.
		linear := s.linearImage(uint64(a.Image))
		called := s.callFixed(cmd, args, func() {
			if linear != 0 {
				a.Image = linear
			}
		})
		if called && o != nil && o.external {
			restrictToHostVisible(o.dev, &(*vn.VkMemoryRequirements)(a.PMemoryRequirements).MemoryTypeBits)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetBufferMemoryRequirements2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetBufferMemoryRequirements2)(args)
		// The info's buffer follows its sType and pNext.
		o := s.ctx.lookup(*(*uint64)(unsafe.Add(a.PInfo, 16)))
		if s.call(cmd, args) && o != nil && o.external {
			restrictToHostVisible(o.dev, &(*vn.VkMemoryRequirements2)(a.PMemoryRequirements).MemoryRequirements.MemoryTypeBits)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetImageMemoryRequirements2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetImageMemoryRequirements2)(args)
		info := (*vn.VkImageMemoryRequirementsInfo2)(a.PInfo)
		o := s.ctx.lookup(uint64(info.Image))
		linear := s.linearImage(uint64(info.Image))
		called := s.callFixed(cmd, args, func() {
			if linear != 0 {
				info.Image = linear
			}
		})
		if called && o != nil && o.external {
			restrictToHostVisible(o.dev, &(*vn.VkMemoryRequirements2)(a.PMemoryRequirements).MemoryRequirements.MemoryTypeBits)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetDeviceBufferMemoryRequirements_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetDeviceBufferMemoryRequirements)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		// VkDeviceBufferMemoryRequirements's pCreateInfo.
		external := stripExternal(dev, *(*unsafe.Pointer)(unsafe.Add(a.PInfo, 16)), vn.VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO)
		if s.call(cmd, args) && external {
			restrictToHostVisible(dev, &(*vn.VkMemoryRequirements2)(a.PMemoryRequirements).MemoryRequirements.MemoryTypeBits)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetDeviceImageMemoryRequirements_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetDeviceImageMemoryRequirements)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		createInfo := *(*unsafe.Pointer)(unsafe.Add(a.PInfo, 16))
		external := stripExternal(dev, createInfo, vn.VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_IMAGE_CREATE_INFO)
		if createInfo != nil {
			ci := (*vn.VkImageCreateInfo)(createInfo)
			modifier := ci.Tiling == imageTilingDrmFormatModifier
			if _, ok := linearizeImageInfo(dev.pd, ci); !ok {
				s.fail("%s: an image of no modifier that the renderer has", cmd.Name)
				return
			}
			// A shadow's memory is its linear image's.
			if (modifier || external) && s.shadows(dev.pd, ci.Format, ci.ImageType, ci.Tiling, ci.Usage) {
				ci.Usage = imageUsageTransfers
			}
		}
		if s.call(cmd, args) && external {
			restrictToHostVisible(dev, &(*vn.VkMemoryRequirements2)(a.PMemoryRequirements).MemoryRequirements.MemoryTypeBits)
		}
	})
}

// stripExternal removes, from the create info at info, a struct of type
// stype that makes the object external with handle types that the renderer
// emulates. It reports whether it did.
func stripExternal(dev *device, info unsafe.Pointer, stype int32) bool {
	if !dev.pd.emulateDmaBuf || info == nil {
		return false
	}
	p := findStruct(info, stype)
	// VkExternalMemoryBufferCreateInfo and VkExternalMemoryImageCreateInfo
	// alike.
	if p == nil || (*vn.VkExternalMemoryBufferCreateInfo)(p).HandleTypes&emulatedHandleTypes == 0 {
		return false
	}
	removeStructs(info, stype)
	return true
}

// restrictToHostVisible restricts memory type bits to host-visible memory.
func restrictToHostVisible(dev *device, bits *uint32) {
	if dev != nil && dev.pd.emulateDmaBuf {
		*bits &= dev.pd.hostVisibleTypes
	}
}

// stripExportMemory removes from a VkMemoryAllocateInfo the export of
// memory as handle types that the renderer emulates, and reports whether it
// did.
func stripExportMemory(dev *device, info *vn.VkMemoryAllocateInfo) bool {
	if !dev.pd.emulateDmaBuf {
		return false
	}
	p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO)
	if p == nil || (*vn.VkExportMemoryAllocateInfo)(p).HandleTypes&emulatedHandleTypes == 0 {
		return false
	}
	removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO)
	return true
}
