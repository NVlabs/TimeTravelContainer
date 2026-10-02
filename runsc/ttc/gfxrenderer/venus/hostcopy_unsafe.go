// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Venus's own host copies and descriptor writes, which carry their memory
// inline, executed by Vulkan's commands that take host pointers.
func registerHostCopy() {
	handle(vn.VK_COMMAND_TYPE_vkCopyMemoryToImageMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCopyMemoryToImageMESA)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		cmd.ReplaceHandles(s.r, args)
		a.Ret = vk.CopyMemoryToImageMESA(dev.table, vk.Handle(a.Device), a.PCopyMemoryToImageInfo)
	})
	handle(vn.VK_COMMAND_TYPE_vkCopyImageToMemoryMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCopyImageToMemoryMESA)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		cmd.ReplaceHandles(s.r, args)
		a.Ret = vk.CopyImageToMemoryMESA(dev.table, vk.Handle(a.Device), a.PCopyImageToMemoryInfo, a.PData)
	})
	handle(vn.VK_COMMAND_TYPE_vkWriteSamplerDescriptorMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkWriteSamplerDescriptorMESA)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		cmd.ReplaceHandles(s.r, args)
		a.Ret = vk.WriteSamplerDescriptorMESA(dev.table, vk.Handle(a.Device), a.PSampler, a.DataSize, a.PData)
	})
	handle(vn.VK_COMMAND_TYPE_vkWriteResourceDescriptorMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkWriteResourceDescriptorMESA)(args)
		dev := s.commandDevice(cmd, args)
		if dev == nil {
			return
		}
		cmd.ReplaceHandles(s.r, args)
		a.Ret = vk.WriteResourceDescriptorMESA(dev.table, vk.Handle(a.Device), a.PResource, a.DataSize, a.PData)
	})
}
