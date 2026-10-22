// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Sync files of semaphores and fences, which Mesa's Venus driver before 24.2,
// as Android images such as redroid's ship it, requires of the renderer to
// offer Android's window system (VK_ANDROID_native_buffer): the renderer's
// semaphores must import sync files and its fences export them. The driver
// enables VK_KHR_external_semaphore_fd and VK_KHR_external_fence_fd, and
// keeps the sync files themselves, but has the renderer
//
//   - give a semaphore the payload of a signaled sync file, once the driver
//     waited for the sync file that it imported (vkImportSemaphoreResourceMESA
//     of resource 0);
//   - wait on a semaphore, as exporting its sync file would, once the
//     driver made the sync file (vkWaitSemaphoreResourceMESA);
//   - reset a fence, as exporting its sync file would, once the driver made
//     the sync file (vkResetFenceResourceMESA).
//
// Later drivers ask for none of this. The renderer has no sync files, nor
// does MoltenVK, macOS's Vulkan: it offers the extensions where the host's
// Vulkan lacks them, signals and waits on semaphores by empty submissions,
// and replaces a fence whose signal is pending, which it cannot reset, by
// another.

const (
	extExternalSemaphoreFD = "VK_KHR_external_semaphore_fd"
	extExternalFenceFD     = "VK_KHR_external_fence_fd"

	// semaphoreHandleSyncFD is VK_EXTERNAL_SEMAPHORE_HANDLE_TYPE_SYNC_FD_BIT,
	// and fenceHandleSyncFD VK_EXTERNAL_FENCE_HANDLE_TYPE_SYNC_FD_BIT.
	semaphoreHandleSyncFD = 0x10
	fenceHandleSyncFD     = 0x8
	// syncFDFeatures are VK_EXTERNAL_SEMAPHORE_FEATURE_EXPORTABLE_BIT and
	// VK_EXTERNAL_SEMAPHORE_FEATURE_IMPORTABLE_BIT, which fences' features
	// equal.
	syncFDFeatures = 0x1 | 0x2
	// pipelineStageAllCommands is VK_PIPELINE_STAGE_ALL_COMMANDS_BIT.
	pipelineStageAllCommands = 0x10000
)

// initSyncFDs settles whether pd emulates the sync files of semaphores and
// fences.
func (pd *physicalDevice) initSyncFDs(hostExtensions map[string]bool) {
	specs := extensionSpecVersions()
	for _, ext := range []struct {
		name    string
		emulate *bool
	}{
		{extExternalSemaphoreFD, &pd.emulateSemaphoreFD},
		{extExternalFenceFD, &pd.emulateFenceFD},
	} {
		spec, ok := specs[ext.name]
		if !ok || hostExtensions[ext.name] {
			continue
		}
		*ext.emulate = true
		var e vn.VkExtensionProperties
		setString(e.ExtensionName[:], ext.name)
		e.SpecVersion = spec
		pd.extensions = append(pd.extensions, e)
	}
}

// stripSyncFDExport keeps sync files out of the export info of type stype,
// if any, of a semaphore's or fence's create info: the renderer exports
// none of the host's.
func stripSyncFDExport(info unsafe.Pointer, stype int32, handleType uint32) {
	p := findStruct(info, stype)
	if p == nil {
		return
	}
	// VkExportFenceCreateInfo is laid out as VkExportSemaphoreCreateInfo.
	e := (*vn.VkExportSemaphoreCreateInfo)(p)
	e.HandleTypes &^= handleType
	if e.HandleTypes == 0 {
		removeStructs(info, stype)
	}
}

func registerSyncFDs() {
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceExternalSemaphoreProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceExternalSemaphoreProperties)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		info := (*vn.VkPhysicalDeviceExternalSemaphoreInfo)(a.PExternalSemaphoreInfo)
		if !pd.emulateSemaphoreFD || info.HandleType != semaphoreHandleSyncFD {
			s.call(cmd, args)
			return
		}
		props := (*vn.VkExternalSemaphoreProperties)(a.PExternalSemaphoreProperties)
		props.ExportFromImportedHandleTypes, props.CompatibleHandleTypes, props.ExternalSemaphoreFeatures = 0, 0, 0
		// Sync files are the payloads of binary semaphores only.
		if p := findStruct(info.PNext, vn.VK_STRUCTURE_TYPE_SEMAPHORE_TYPE_CREATE_INFO); p == nil || (*vn.VkSemaphoreTypeCreateInfo)(p).SemaphoreType != semaphoreTypeTimeline {
			props.ExportFromImportedHandleTypes = semaphoreHandleSyncFD
			props.CompatibleHandleTypes = semaphoreHandleSyncFD
			props.ExternalSemaphoreFeatures = syncFDFeatures
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkGetPhysicalDeviceExternalFenceProperties_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkGetPhysicalDeviceExternalFenceProperties)(args)
		pd := s.physicalDevice(cmd, args)
		if pd == nil {
			return
		}
		info := (*vn.VkPhysicalDeviceExternalFenceInfo)(a.PExternalFenceInfo)
		if !pd.emulateFenceFD || info.HandleType != fenceHandleSyncFD {
			s.call(cmd, args)
			return
		}
		props := (*vn.VkExternalFenceProperties)(a.PExternalFenceProperties)
		props.ExportFromImportedHandleTypes = fenceHandleSyncFD
		props.CompatibleHandleTypes = fenceHandleSyncFD
		props.ExternalFenceFeatures = syncFDFeatures
	})
	handle(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkCreateFence)(args)
		stripSyncFDExport(a.PCreateInfo, vn.VK_STRUCTURE_TYPE_EXPORT_FENCE_CREATE_INFO, fenceHandleSyncFD)
		s.createObject(cmd, args, vn.VK_OBJECT_TYPE_FENCE, a.PFence)
	})
	handle(vn.VK_COMMAND_TYPE_vkImportSemaphoreResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		info := (*vn.VkImportSemaphoreResourceInfoMESA)((*vn.Args_vkImportSemaphoreResourceMESA)(args).PImportSemaphoreResourceInfo)
		if info.ResourceId != 0 {
			s.fail("vkImportSemaphoreResourceMESA: the renderer has no sync_fd resources")
			return
		}
		// Resource 0 is the sync file -1, which is signaled.
		if o := s.syncObject(cmd, uint64(info.Semaphore), vn.VK_OBJECT_TYPE_SEMAPHORE); o != nil {
			s.submitSemaphore(o, true)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkWaitSemaphoreResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if o := s.syncObject(cmd, uint64((*vn.Args_vkWaitSemaphoreResourceMESA)(args).Semaphore), vn.VK_OBJECT_TYPE_SEMAPHORE); o != nil {
			s.submitSemaphore(o, false)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkResetFenceResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if o := s.syncObject(cmd, uint64((*vn.Args_vkResetFenceResourceMESA)(args).Fence), vn.VK_OBJECT_TYPE_FENCE); o != nil {
			s.resetFence(o)
		}
	})
}

// syncObject returns semaphore or fence id, of type typ, or fails the
// context.
func (s *stream) syncObject(cmd *vn.Command, id uint64, typ int32) *object {
	o := s.ctx.lookupType(id, typ)
	if o == nil || o.dev == nil || len(o.dev.queues) == 0 {
		s.fail("%s: no object %d", cmd.Name, id)
		return nil
	}
	return o
}

// submitSemaphore signals the binary semaphore o, or waits on it, by an
// empty submission to its device's first queue. The guest's driver orders
// the commands that use the semaphore after this one, on the same ring.
func (s *stream) submitSemaphore(o *object, signal bool) {
	dev := o.dev
	q := dev.queues[0]
	sems := alloc[vn.Handle](s, 1)
	*sems = o.handle
	info := newArgs[vn.VkSubmitInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_SUBMIT_INFO
	if signal {
		info.SignalSemaphoreCount, info.PSignalSemaphores = 1, unsafe.Pointer(sems)
	} else {
		stages := alloc[uint32](s, 1)
		*stages = pipelineStageAllCommands
		info.WaitSemaphoreCount, info.PWaitSemaphores, info.PWaitDstStageMask = 1, unsafe.Pointer(sems), unsafe.Pointer(stages)
	}
	a := newArgs[vn.Args_vkQueueSubmit](s)
	a.Queue, a.SubmitCount, a.PSubmits = q.handle, 1, unsafe.Pointer(info)
	q.host.mu.Lock()
	vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, dev.table, unsafe.Pointer(a))
	q.host.mu.Unlock()
	if a.Ret != vn.VK_SUCCESS && a.Ret != vn.VK_ERROR_DEVICE_LOST {
		s.ctx.logf("submitting semaphore %d (signal %t): %d", o.id, signal, a.Ret)
	}
}

// resetFence resets the fence o, as exporting its sync file would. Vulkan
// cannot reset a fence whose signal is pending, as the guest's may be: such
// a fence is replaced by another, unsignaled, and retired until its device
// is destroyed, or reused once it signals.
func (s *stream) resetFence(o *object) {
	dev := o.dev
	get := newArgs[vn.Args_vkGetFenceStatus](s)
	get.Device, get.Fence = dev.handle, o.handle
	vk.Call(vn.VK_COMMAND_TYPE_vkGetFenceStatus_EXT, dev.table, unsafe.Pointer(get))
	switch get.Ret {
	case vn.VK_SUCCESS:
		s.resetFences(dev, o.handle)
	case vn.VK_NOT_READY:
		fence := s.unsignaledFence(dev)
		if fence == 0 {
			return
		}
		dev.retiredMu.Lock()
		dev.retired = append(dev.retired, o.handle)
		dev.retiredMu.Unlock()
		s.ctx.mu.Lock()
		o.handle = fence
		s.ctx.mu.Unlock()
	}
}

// resetFences resets the host's fence f of dev.
func (s *stream) resetFences(dev *device, f vn.Handle) {
	fences := alloc[vn.Handle](s, 1)
	*fences = f
	a := newArgs[vn.Args_vkResetFences](s)
	a.Device, a.FenceCount, a.PFences = dev.handle, 1, unsafe.Pointer(fences)
	vk.Call(vn.VK_COMMAND_TYPE_vkResetFences_EXT, dev.table, unsafe.Pointer(a))
}

// unsignaledFence returns an unsignaled fence of dev's: a retired fence that
// signaled, reset, or a new one. It returns 0 if it can create none.
func (s *stream) unsignaledFence(dev *device) vn.Handle {
	dev.retiredMu.Lock()
	retired := dev.retired
	dev.retiredMu.Unlock()
	for _, f := range retired {
		get := newArgs[vn.Args_vkGetFenceStatus](s)
		get.Device, get.Fence = dev.handle, f
		vk.Call(vn.VK_COMMAND_TYPE_vkGetFenceStatus_EXT, dev.table, unsafe.Pointer(get))
		if get.Ret != vn.VK_SUCCESS {
			continue
		}
		dev.retiredMu.Lock()
		for i, r := range dev.retired {
			if r == f {
				dev.retired = append(dev.retired[:i], dev.retired[i+1:]...)
				break
			}
		}
		dev.retiredMu.Unlock()
		s.resetFences(dev, f)
		return f
	}
	info := newArgs[vn.VkFenceCreateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO
	out := alloc[vn.Handle](s, 1)
	a := newArgs[vn.Args_vkCreateFence](s)
	a.Device, a.PCreateInfo, a.PFence = dev.handle, unsafe.Pointer(info), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, dev.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		s.ctx.logf("creating a fence to replace one that was reset: %d", a.Ret)
		return 0
	}
	return *out
}

// destroyRetiredFences destroys dev's retired fences, once nothing uses
// them.
func (s *stream) destroyRetiredFences(dev *device) {
	dev.retiredMu.Lock()
	retired := dev.retired
	dev.retired = nil
	dev.retiredMu.Unlock()
	for _, f := range retired {
		a := newArgs[vn.Args_vkDestroyFence](s)
		a.Device, a.Fence = dev.handle, f
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyFence_EXT, dev.table, unsafe.Pointer(a))
	}
}
