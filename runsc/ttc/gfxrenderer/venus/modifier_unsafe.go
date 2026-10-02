// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// DRM format modifiers and foreign queue families, which Mesa's Venus driver
// requires of the renderer to offer Android's buffers to applications
// (VK_ANDROID_external_memory_android_hardware_buffer and
// VK_ANDROID_native_buffer), for the guests' dma-bufs that the renderer
// emulates (see dmabuf_unsafe.go): the host's Vulkan may lack them, as
// MoltenVK does, and its own modifiers, as NVIDIA's on Linux, describe its
// own memory, not the renderer's shared memory that guests' dma-bufs are.
//
//   - VK_EXT_image_drm_format_modifier, with one modifier, linear: an image
//     of a modifier is a linear image of the host's, or a shadow where its
//     usage needs what the host's linear images lack (see
//     shadow_unsafe.go). Its layout is the host's, which Android's buffers
//     have (see crossdomain_unsafe.go).
//   - VK_EXT_queue_family_foreign: the host's driver sees no transfer to or
//     from a foreign queue family, which its own transfers, of external
//     memory that is host memory, would not do anything for either.

const (
	extImageDrmFormatModifier = "VK_EXT_image_drm_format_modifier"
	extQueueFamilyForeign     = "VK_EXT_queue_family_foreign"

	// imageTilingDrmFormatModifier is VK_IMAGE_TILING_DRM_FORMAT_MODIFIER_EXT.
	imageTilingDrmFormatModifier = 1000158000
	// drmFormatModLinear is DRM_FORMAT_MOD_LINEAR.
	drmFormatModLinear = 0
	// queueFamilyForeign is VK_QUEUE_FAMILY_FOREIGN_EXT, and
	// queueFamilyIgnored VK_QUEUE_FAMILY_IGNORED.
	queueFamilyForeign = 0xfffffffd
	queueFamilyIgnored = 0xffffffff
	// aspectMemoryPlane0 is VK_IMAGE_ASPECT_MEMORY_PLANE_0_BIT_EXT, which
	// the next three planes' bits follow.
	aspectMemoryPlane0 = 0x80
	aspectMemoryPlanes = 0x80 | 0x100 | 0x200 | 0x400
)

// initModifiers settles whether pd emulates DRM format modifiers and
// foreign queue families.
func (pd *physicalDevice) initModifiers(hostExtensions map[string]bool) {
	if !pd.emulateDmaBuf {
		return
	}
	pd.emulateModifiers = true
	specs := extensionSpecVersions()
	for _, name := range []string{extImageDrmFormatModifier, extQueueFamilyForeign} {
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

// formatPlaneCount returns the number of planes of images of a format.
func formatPlaneCount(format uint32) uint32 {
	switch format {
	case 1000156002, 1000156004, 1000156006, 1000156012, 1000156014, 1000156016,
		1000156022, 1000156024, 1000156026, 1000156029, 1000156031, 1000156033:
		return 3
	case 1000156003, 1000156005, 1000156013, 1000156015, 1000156023, 1000156025,
		1000156030, 1000156032, 1000330000, 1000330001, 1000330002, 1000330003:
		return 2
	}
	return 1
}

// linearizeImageInfo makes the create info of an image of a DRM format
// modifier, which pd emulates, that of a linear image, and returns the
// explicit layout that the guest gave it, if any. It reports false if the
// guest asked for no modifier that the renderer has.
func linearizeImageInfo(pd *physicalDevice, info *vn.VkImageCreateInfo) ([]vn.VkSubresourceLayout, bool) {
	if !pd.emulateModifiers || info.Tiling != imageTilingDrmFormatModifier {
		return nil, true
	}
	var layouts []vn.VkSubresourceLayout
	linear := false
	if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_EXPLICIT_CREATE_INFO_EXT); p != nil {
		e := (*vn.VkImageDrmFormatModifierExplicitCreateInfoEXT)(p)
		linear = e.DrmFormatModifier == drmFormatModLinear
		if e.PPlaneLayouts != nil {
			layouts = append(layouts, unsafe.Slice((*vn.VkSubresourceLayout)(e.PPlaneLayouts), e.DrmFormatModifierPlaneCount)...)
		}
	}
	if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_LIST_CREATE_INFO_EXT); p != nil {
		l := (*vn.VkImageDrmFormatModifierListCreateInfoEXT)(p)
		if l.PDrmFormatModifiers != nil {
			for _, m := range unsafe.Slice((*uint64)(l.PDrmFormatModifiers), l.DrmFormatModifierCount) {
				linear = linear || m == drmFormatModLinear
			}
		}
	}
	removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_EXPLICIT_CREATE_INFO_EXT)
	removeStructs(unsafe.Pointer(info), vn.VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_LIST_CREATE_INFO_EXT)
	info.Tiling = imageTilingLinear
	return layouts, linear
}

// checkLinearLayout logs where the host lays out the linear image img of
// format otherwise than layouts, which the guest gave it: what the guest
// and the host read of its memory then differs.
func (s *stream) checkLinearLayout(dev *device, img vn.Handle, format uint32, layouts []vn.VkSubresourceLayout) {
	planes := formatPlaneCount(format)
	for i := range layouts {
		if uint32(i) >= planes {
			break
		}
		sub := newArgs[vn.VkImageSubresource](s)
		sub.AspectMask = aspectColor
		if planes > 1 {
			sub.AspectMask = planeAspects[i]
		}
		layout := newArgs[vn.VkSubresourceLayout](s)
		a := newArgs[vn.Args_vkGetImageSubresourceLayout](s)
		a.Device, a.Image, a.PSubresource, a.PLayout = dev.handle, img, unsafe.Pointer(sub), unsafe.Pointer(layout)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetImageSubresourceLayout_EXT, dev.table, unsafe.Pointer(a))
		if layout.Offset != layouts[i].Offset || layout.RowPitch != layouts[i].RowPitch {
			s.ctx.logf("plane %d of an image of format %d lies at %d, of rows of %d bytes, not at %d, of rows of %d bytes, as the guest's buffer does",
				i, format, layout.Offset, layout.RowPitch, layouts[i].Offset, layouts[i].RowPitch)
		}
	}
}

// subresourceAspect returns the aspect of the host's image that aspect, of
// an image of format, names: a memory plane of a linear image is its color
// or one of its planes.
func subresourceAspect(format, aspect uint32) uint32 {
	if aspect&aspectMemoryPlanes == 0 {
		return aspect
	}
	plane := 0
	for aspect&(aspectMemoryPlane0<<plane) == 0 {
		plane++
	}
	if formatPlaneCount(format) == 1 || plane >= len(planeAspects) {
		return aspectColor
	}
	return planeAspects[plane]
}

// foreignBarriers makes the barriers that transfer images and buffers to or
// from a foreign queue family transfer nothing.
func foreignBarriers(images unsafe.Pointer, imageCount uint32, buffers unsafe.Pointer, bufferCount uint32, v2 bool) {
	fix := func(src, dst *uint32) {
		if *src == queueFamilyForeign || *dst == queueFamilyForeign {
			*src, *dst = queueFamilyIgnored, queueFamilyIgnored
		}
	}
	switch {
	case images == nil:
	case v2:
		bs := unsafe.Slice((*vn.VkImageMemoryBarrier2)(images), imageCount)
		for i := range bs {
			fix(&bs[i].SrcQueueFamilyIndex, &bs[i].DstQueueFamilyIndex)
		}
	default:
		bs := unsafe.Slice((*vn.VkImageMemoryBarrier)(images), imageCount)
		for i := range bs {
			fix(&bs[i].SrcQueueFamilyIndex, &bs[i].DstQueueFamilyIndex)
		}
	}
	switch {
	case buffers == nil:
	case v2:
		bs := unsafe.Slice((*vn.VkBufferMemoryBarrier2)(buffers), bufferCount)
		for i := range bs {
			fix(&bs[i].SrcQueueFamilyIndex, &bs[i].DstQueueFamilyIndex)
		}
	default:
		bs := unsafe.Slice((*vn.VkBufferMemoryBarrier)(buffers), bufferCount)
		for i := range bs {
			fix(&bs[i].SrcQueueFamilyIndex, &bs[i].DstQueueFamilyIndex)
		}
	}
}

// foreignDependencies makes the barriers of count dependency infos at deps
// transfer nothing to or from a foreign queue family.
func foreignDependencies(deps unsafe.Pointer, count uint32) {
	if deps == nil {
		return
	}
	ds := unsafe.Slice((*vn.VkDependencyInfo)(deps), count)
	for i := range ds {
		foreignBarriers(ds[i].PImageMemoryBarriers, ds[i].ImageMemoryBarrierCount, ds[i].PBufferMemoryBarriers, ds[i].BufferMemoryBarrierCount, true)
	}
}

func registerModifiers() {
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceFormatProperties2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceFormatProperties2)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		// The lists of modifiers are the renderer's, which the host's driver
		// fills in if it has modifiers of its own: their capacities are the
		// guest's.
		props := (*vn.VkFormatProperties2)(a.PFormatProperties)
		var list, list2 unsafe.Pointer
		var capacity, capacity2 uint32
		if pd.emulateModifiers {
			if list = findStruct(props.PNext, vn.VK_STRUCTURE_TYPE_DRM_FORMAT_MODIFIER_PROPERTIES_LIST_EXT); list != nil {
				capacity = (*vn.VkDrmFormatModifierPropertiesListEXT)(list).DrmFormatModifierCount
			}
			if list2 = findStruct(props.PNext, vn.VK_STRUCTURE_TYPE_DRM_FORMAT_MODIFIER_PROPERTIES_LIST_2_EXT); list2 != nil {
				capacity2 = (*vn.VkDrmFormatModifierPropertiesList2EXT)(list2).DrmFormatModifierCount
			}
		}
		if !s.call(cmd, args) || !pd.emulateModifiers {
			return
		}
		features := modifierFeatures(a.Format, &props.FormatProperties)
		count := uint32(0)
		if features != 0 {
			count = 1
		}
		if list != nil {
			l := (*vn.VkDrmFormatModifierPropertiesListEXT)(list)
			if l.PDrmFormatModifierProperties != nil {
				l.DrmFormatModifierCount = min(capacity, count)
				if l.DrmFormatModifierCount != 0 {
					*(*vn.VkDrmFormatModifierPropertiesEXT)(l.PDrmFormatModifierProperties) = vn.VkDrmFormatModifierPropertiesEXT{
						DrmFormatModifier:               drmFormatModLinear,
						DrmFormatModifierPlaneCount:     formatPlaneCount(a.Format),
						DrmFormatModifierTilingFeatures: features,
					}
				}
			} else {
				l.DrmFormatModifierCount = count
			}
		}
		if list2 != nil {
			l := (*vn.VkDrmFormatModifierPropertiesList2EXT)(list2)
			if l.PDrmFormatModifierProperties != nil {
				l.DrmFormatModifierCount = min(capacity2, count)
				if l.DrmFormatModifierCount != 0 {
					*(*vn.VkDrmFormatModifierProperties2EXT)(l.PDrmFormatModifierProperties) = vn.VkDrmFormatModifierProperties2EXT{
						DrmFormatModifier:               drmFormatModLinear,
						DrmFormatModifierPlaneCount:     formatPlaneCount(a.Format),
						DrmFormatModifierTilingFeatures: uint64(features),
					}
				}
			} else {
				l.DrmFormatModifierCount = count
			}
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceImageFormatProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceImageFormatProperties)(args)
		if a.Tiling == imageTilingDrmFormatModifier {
			// A modifier is the second version's to give.
			a.Ret = vn.VK_ERROR_FORMAT_NOT_SUPPORTED
			return
		}
		s.call(cmd, args)
	})
	handle(vn.VK_COMMAND_TYPE_vkGetImageDrmFormatModifierPropertiesEXT_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetImageDrmFormatModifierPropertiesEXT)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		if !dev.pd.emulateModifiers {
			s.call(cmd, args)
			return
		}
		(*vn.VkImageDrmFormatModifierPropertiesEXT)(a.PProperties).DrmFormatModifier = drmFormatModLinear
		a.Ret = vn.VK_SUCCESS
	})
	// The layouts of a shadow's subresources are its linear image's.
	handle(vn.VK_COMMAND_TYPE_vkGetImageSubresourceLayout_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetImageSubresourceLayout)(args)
		if o := s.ctx.lookup(uint64(a.Image)); o != nil && o.image != nil {
			sub := (*vn.VkImageSubresource)(a.PSubresource)
			sub.AspectMask = subresourceAspect(o.image.format, sub.AspectMask)
		}
		linear := s.linearImage(uint64(a.Image))
		s.callFixed(cmd, args, func() {
			if linear != 0 {
				a.Image = linear
			}
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkGetImageSubresourceLayout2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetImageSubresourceLayout2)(args)
		if o := s.ctx.lookup(uint64(a.Image)); o != nil && o.image != nil {
			sub := &(*vn.VkImageSubresource2)(a.PSubresource).ImageSubresource
			sub.AspectMask = subresourceAspect(o.image.format, sub.AspectMask)
		}
		linear := s.linearImage(uint64(a.Image))
		s.callFixed(cmd, args, func() {
			if linear != 0 {
				a.Image = linear
			}
		})
	})

	// Barriers, which the command buffers that record them journal, with
	// what shadows need of them.
	handle(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdPipelineBarrier)(args)
		s.barrierCommand(cmd, args, func(cb *object) ([]shadowCopy, []shadowCopy) {
			release, acquire := s.shadowTransfers(cb, a.PImageMemoryBarriers, a.ImageMemoryBarrierCount, false, true)
			foreignBarriers(a.PImageMemoryBarriers, a.ImageMemoryBarrierCount, a.PBufferMemoryBarriers, a.BufferMemoryBarrierCount, false)
			return release, acquire
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdWaitEvents_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdWaitEvents)(args)
		s.barrierCommand(cmd, args, func(cb *object) ([]shadowCopy, []shadowCopy) {
			release, acquire := s.shadowTransfers(cb, a.PImageMemoryBarriers, a.ImageMemoryBarrierCount, false, true)
			foreignBarriers(a.PImageMemoryBarriers, a.ImageMemoryBarrierCount, a.PBufferMemoryBarriers, a.BufferMemoryBarrierCount, false)
			return release, acquire
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdPipelineBarrier2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		deps := (*vn.Args_vkCmdPipelineBarrier2)(args).PDependencyInfo
		s.barrierCommand(cmd, args, func(cb *object) ([]shadowCopy, []shadowCopy) {
			release, acquire := s.shadowDependencies(cb, deps, 1, true)
			foreignDependencies(deps, 1)
			return release, acquire
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdWaitEvents2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCmdWaitEvents2)(args)
		s.barrierCommand(cmd, args, func(cb *object) ([]shadowCopy, []shadowCopy) {
			release, acquire := s.shadowDependencies(cb, a.PDependencyInfos, a.EventCount, true)
			foreignDependencies(a.PDependencyInfos, a.EventCount)
			return release, acquire
		})
	})
	handle(vn.VK_COMMAND_TYPE_vkCmdSetEvent2_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		// The barriers execute where vkCmdWaitEvents2 waits for the event.
		deps := (*vn.Args_vkCmdSetEvent2)(args).PDependencyInfo
		s.shadowDependencies(s.commandBuffer(firstID(args)), deps, 1, false)
		foreignDependencies(deps, 1)
		passThrough(s, cmd, args)
	})
}
