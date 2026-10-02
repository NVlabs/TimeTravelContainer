// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// What the renderer offers guests beyond what the host's Vulkan has, where
// guests need it and the host can do without it, as MoltenVK, macOS's Vulkan,
// requires:
//
//   - robustness2's nullDescriptor, which Zink, OpenGL on Vulkan, requires.
//     MoltenVK lacks it: the guest's null descriptors reach it as they are,
//     and it binds nil Metal resources for them.
//   - VK_EXT_provoking_vertex, which ANGLE's Vulkan backend requires for
//     OpenGL ES 3, and so Chromium. MoltenVK lacks it, as Metal takes flat
//     values from a primitive's first vertex: pipelines that ask for the last
//     vertex get the first, as they did before ANGLE required the extension.
//   - The host's queue families less the trailing ones that duplicate an
//     earlier one, as MoltenVK's four general families do: guests that find
//     several graphics families may wait for a window surface to pick one,
//     and ANGLE's Vulkan backend, which Android's SurfaceFlinger uses, then
//     uses a device that it has yet to create. The families that remain keep
//     the host's indices.

const extProvokingVertex = "VK_EXT_provoking_vertex"

// initQuirks settles what pd offers beyond the host's Vulkan.
func (s *stream) initQuirks(pd *physicalDevice, hostExtensions map[string]bool) {
	if hostExtensions["VK_EXT_robustness2"] || hostExtensions["VK_KHR_robustness2"] {
		robustness := newArgs[vn.VkPhysicalDeviceRobustness2FeaturesKHR](s)
		robustness.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ROBUSTNESS_2_FEATURES_KHR
		features := newArgs[vn.VkPhysicalDeviceFeatures2](s)
		features.SType = vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2
		features.PNext = unsafe.Pointer(robustness)
		a := newArgs[vn.Args_vkGetPhysicalDeviceFeatures2](s)
		a.PhysicalDevice = pd.handle
		a.PFeatures = unsafe.Pointer(features)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFeatures2_EXT, pd.inst.pdTable, unsafe.Pointer(a))
		pd.hostNullDescriptor = robustness.NullDescriptor != 0
	}
	if spec, ok := extensionSpecVersions()[extProvokingVertex]; ok && !hostExtensions[extProvokingVertex] {
		pd.emulateProvokingVertex = true
		var e vn.VkExtensionProperties
		setString(e.ExtensionName[:], extProvokingVertex)
		e.SpecVersion = spec
		pd.extensions = append(pd.extensions, e)
	}
	pd.initExternalMemory(hostExtensions)
	pd.initModifiers(hostExtensions)
	// Trailing families that duplicate an earlier one.
	pd.familyCount = uint32(len(pd.queueFamilies))
	for pd.familyCount > 1 {
		last := pd.queueFamilies[pd.familyCount-1]
		dup := false
		for _, f := range pd.queueFamilies[:pd.familyCount-1] {
			if f == last {
				dup = true
				break
			}
		}
		if !dup {
			break
		}
		pd.familyCount--
	}
}

func registerQuirks() {
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFeatures2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		pd := s.physicalDevice(cmd, args)
		if pd == nil || !s.call(cmd, args) {
			return
		}
		features := (*vn.VkPhysicalDeviceFeatures2)((*vn.Args_vkGetPhysicalDeviceFeatures2)(args).PFeatures)
		if p := findStruct(features.PNext, vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ROBUSTNESS_2_FEATURES_KHR); p != nil {
			(*vn.VkPhysicalDeviceRobustness2FeaturesKHR)(p).NullDescriptor = 1
		}
		if p := findStruct(features.PNext, vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROVOKING_VERTEX_FEATURES_EXT); p != nil && pd.emulateProvokingVertex {
			f := (*vn.VkPhysicalDeviceProvokingVertexFeaturesEXT)(p)
			f.ProvokingVertexLast = 1
			f.TransformFeedbackPreservesProvokingVertex = 0
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceProperties2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		pd := s.physicalDevice(cmd, args)
		if pd == nil || !s.call(cmd, args) {
			return
		}
		props := (*vn.VkPhysicalDeviceProperties2)((*vn.Args_vkGetPhysicalDeviceProperties2)(args).PProperties)
		props.Properties.ApiVersion = pd.props.ApiVersion
		if p := findStruct(props.PNext, vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROVOKING_VERTEX_PROPERTIES_EXT); p != nil && pd.emulateProvokingVertex {
			pp := (*vn.VkPhysicalDeviceProvokingVertexPropertiesEXT)(p)
			pp.ProvokingVertexModePerPipeline = 1
			pp.TransformFeedbackPreservesTriangleFanProvokingVertex = 0
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceQueueFamilyProperties)(args)
		s.queueFamilies(cmd, args, (*uint32)(a.PQueueFamilyPropertyCount), a.PQueueFamilyProperties != nil)
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceQueueFamilyProperties2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceQueueFamilyProperties2)(args)
		s.queueFamilies(cmd, args, (*uint32)(a.PQueueFamilyPropertyCount), a.PQueueFamilyProperties != nil)
	})
}

// queueFamilies executes a query of a physical device's queue families,
// which offers the guest only the families that do not duplicate earlier
// ones.
func (s *stream) queueFamilies(cmd *vn.Command, args unsafe.Pointer, count *uint32, query bool) {
	pd := s.physicalDevice(cmd, args)
	if pd == nil {
		return
	}
	if query {
		*count = min(*count, pd.familyCount)
	}
	if s.call(cmd, args) {
		*count = min(*count, pd.familyCount)
	}
}

// deviceQuirks keeps from the host's device what the renderer offers
// beyond the host's Vulkan: info is the guest's VkDeviceCreateInfo, and
// names the device's extensions.
func deviceQuirks(pd *physicalDevice, info *vn.VkDeviceCreateInfo, names []unsafe.Pointer) []unsafe.Pointer {
	if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ROBUSTNESS_2_FEATURES_KHR); p != nil && !pd.hostNullDescriptor {
		(*vn.VkPhysicalDeviceRobustness2FeaturesKHR)(p).NullDescriptor = 0
	}
	emulated := make(map[string]bool)
	if pd.emulateProvokingVertex {
		removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROVOKING_VERTEX_FEATURES_EXT)
		emulated[extProvokingVertex] = true
	}
	if pd.emulateDmaBuf {
		for _, name := range emulatedExtensions {
			emulated[name] = true
		}
	}
	if pd.emulateModifiers {
		emulated[extImageDrmFormatModifier] = true
		emulated[extQueueFamilyForeign] = true
	}
	kept := names[:0]
	for _, name := range names {
		if !emulated[goCString(name)] {
			kept = append(kept, name)
		}
	}
	return kept
}

// pipelineQuirks keeps from the host's driver what the renderer offers
// beyond it in graphics pipelines' create infos.
func pipelineQuirks(dev *device, infos []vn.VkGraphicsPipelineCreateInfo) {
	if !dev.pd.emulateProvokingVertex {
		return
	}
	for i := range infos {
		if infos[i].PRasterizationState != nil {
			removeStructs(infos[i].PRasterizationState, vn.VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_PROVOKING_VERTEX_STATE_CREATE_INFO_EXT)
		}
	}
}

// removeStructs removes the structs of type stype from the pNext chain of
// the struct at p.
func removeStructs(p unsafe.Pointer, stype int32) {
	for {
		next := *(*unsafe.Pointer)(unsafe.Add(p, 8))
		if next == nil {
			return
		}
		if *(*int32)(next) == stype {
			setNext(p, *(*unsafe.Pointer)(unsafe.Add(next, 8)))
			continue
		}
		p = next
	}
}
