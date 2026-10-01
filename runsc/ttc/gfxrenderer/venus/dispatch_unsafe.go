// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// handler executes a command, whose arguments args are decoded.
type handler func(s *stream, cmd *vn.Command, args unsafe.Pointer)

// handlers are the handlers of commands, by type. A command without one
// fails the context.
var handlers [vn.CommandTableSize]handler

// handle registers h for the command of type typ.
func handle(typ int32, h handler) {
	handlers[typ] = h
}

// refuse makes the commands fail the context, as virglrenderer does: the
// guest's driver implements them, or must not use them.
func refuse(types ...int32) {
	for _, typ := range types {
		handlers[typ] = nil
	}
}

func init() {
	// Vulkan executes most commands as they are, once their handles are
	// replaced. Those that create or destroy objects need handlers.
	for typ := int32(0); typ < vn.CommandTableSize; typ++ {
		cmd := vn.LookupCommand(typ)
		if cmd == nil || cmd.Level() == vn.LevelNone || cmd.ManagesObjects() {
			continue
		}
		handlers[typ] = passThrough
	}
	registerObjects()
	registerTransport()
	registerInstance()
	registerDevice()
	registerQueue()
	registerMemory()
	registerQuirks()
	registerExternalMemory()
	registerModifiers()
	registerHostCopy()
	registerJournal()
	refuse(
		vn.VK_COMMAND_TYPE_vkEnumerateInstanceLayerProperties_EXT,
		vn.VK_COMMAND_TYPE_vkEnumerateDeviceLayerProperties_EXT,
		vn.VK_COMMAND_TYPE_vkMapMemory_EXT,
		vn.VK_COMMAND_TYPE_vkUnmapMemory_EXT,
		vn.VK_COMMAND_TYPE_vkUnmapMemory2_EXT,
		vn.VK_COMMAND_TYPE_vkFlushMappedMemoryRanges_EXT,
		vn.VK_COMMAND_TYPE_vkInvalidateMappedMemoryRanges_EXT,
		vn.VK_COMMAND_TYPE_vkGetMemoryFdKHR_EXT,
		vn.VK_COMMAND_TYPE_vkGetMemoryFdPropertiesKHR_EXT,
		vn.VK_COMMAND_TYPE_vkUpdateDescriptorSetWithTemplate_EXT,
		vn.VK_COMMAND_TYPE_vkCmdPushDescriptorSetWithTemplate_EXT,
		vn.VK_COMMAND_TYPE_vkCmdPushDescriptorSetWithTemplate2_EXT,
		vn.VK_COMMAND_TYPE_vkBuildAccelerationStructuresKHR_EXT,
		vn.VK_COMMAND_TYPE_vkCopyAccelerationStructureKHR_EXT,
		vn.VK_COMMAND_TYPE_vkCopyAccelerationStructureToMemoryKHR_EXT,
		vn.VK_COMMAND_TYPE_vkCopyMemoryToAccelerationStructureKHR_EXT,
		vn.VK_COMMAND_TYPE_vkWriteAccelerationStructuresPropertiesKHR_EXT,
		vn.VK_COMMAND_TYPE_vkWriteResourceDescriptorsEXT_EXT,
		vn.VK_COMMAND_TYPE_vkWriteSamplerDescriptorsEXT_EXT,
		// Private data slots are the guest driver's: their commands name
		// objects by raw handles.
		vn.VK_COMMAND_TYPE_vkGetPrivateData_EXT,
		vn.VK_COMMAND_TYPE_vkSetPrivateData_EXT,
		// The renderer executes nothing that blocks its streams.
		vn.VK_COMMAND_TYPE_vkQueueWaitIdle_EXT,
		vn.VK_COMMAND_TYPE_vkDeviceWaitIdle_EXT,
		// vkGetDeviceQueue2 sets a queue's timeline.
		vn.VK_COMMAND_TYPE_vkGetDeviceQueue_EXT,
	)
}

// passThrough executes a command as it is, once its handles are replaced.
func passThrough(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	if o, ok := s.callObject(cmd, args); ok && o != nil && o.typ == vn.VK_OBJECT_TYPE_COMMAND_BUFFER {
		s.journalCommandBuffer(o, cmd)
	}
}
