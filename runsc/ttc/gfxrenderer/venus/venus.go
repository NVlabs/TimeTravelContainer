// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

// Package venus is TTC's renderer for Mesa's Venus Vulkan driver: it
// executes, with the host's Vulkan, the commands that the driver serializes
// in a virtio-gpu context of the Venus capability set, as virglrenderer's
// vkr does, and speaks pkg/gfxproto to the Sentry's virtual GPU.
//
// Package vn decodes the commands into the C structs that Vulkan takes, and
// package vk calls Vulkan with them. Each context keeps the objects that its
// guest creates, by the IDs that the guest gives them.
package venus

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
)

// CapsetVenus is the ID of Venus's capability set.
const CapsetVenus = 4

// maxAPIVersion is the newest Vulkan version that the renderer offers, as
// virglrenderer offers: VK_API_VERSION_1_4.
const maxAPIVersion = 1<<22 | 4<<12

// apiVersion returns VK_MAKE_API_VERSION(0, major, minor, 0).
func apiVersion(major, minor uint32) uint32 {
	return major<<22 | minor<<12
}

// capMinor caps the minor version of version at cap's, keeping version's
// patch, as vkr_api_version_cap_minor does.
func capMinor(version, cap uint32) uint32 {
	minor := func(v uint32) uint32 { return v >> 12 & 0x3ff }
	if minor(version) > minor(cap) {
		return cap&^0xfff | version&0xfff
	}
	return version
}

// unsupportedExtensions are the protocol's extensions that the renderer does
// not offer, because the guest's driver implements them, as virglrenderer
// decides.
var unsupportedExtensions = map[string]bool{
	"VK_EXT_tooling_info":             true,
	"VK_KHR_map_memory2":              true,
	"VK_KHR_deferred_host_operations": true,
}

// extensionSpecVersions are the spec versions of the extensions that the
// renderer offers.
var extensionSpecVersions = sync.OnceValue(func() map[string]uint32 {
	m := make(map[string]uint32)
	for _, e := range vk.Extensions() {
		if !unsupportedExtensions[e.Name] {
			m[e.Name] = e.SpecVersion
		}
	}
	return m
})

// Capset returns Venus's capability set, struct
// virgl_renderer_capset_venus.
func Capset() []byte {
	specs := extensionSpecVersions()
	var mask [32]uint32
	for _, e := range vk.Extensions() {
		mask[e.Number/32] |= 1 << (e.Number % 32)
	}
	// Bit 0 says that the mask is valid.
	mask[0] |= 1
	fields := []uint32{
		vk.WireFormatVersion(),
		vk.VkXMLVersion(),
		specs["VK_EXT_command_serialization"],
		specs["VK_MESA_venus_protocol"],
		1, // supports_blob_id_0
	}
	fields = append(fields, mask[:]...)
	fields = append(fields,
		1, // allow_vk_wait_syncs
		1, // supports_multiple_timelines
		0, // use_guest_vram
	)
	buf := make([]byte, 0, 4*len(fields))
	for _, f := range fields {
		buf = binary.LittleEndian.AppendUint32(buf, f)
	}
	return buf
}

// libraryPaths are where the renderer looks for Vulkan: the loader, then
// MoltenVK itself on macOS.
func libraryPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"libvulkan.1.dylib",
			"/opt/homebrew/lib/libvulkan.1.dylib",
			"/usr/local/lib/libvulkan.1.dylib",
			"libMoltenVK.dylib",
			"/opt/homebrew/lib/libMoltenVK.dylib",
		}
	}
	return []string{"libvulkan.so.1", "libvulkan.so"}
}

var (
	libraryOnce sync.Once
	library     *vk.Library
	libraryErr  error
)

// loadLibrary loads Vulkan, once for the process.
func loadLibrary() (*vk.Library, error) {
	libraryOnce.Do(func() {
		library, libraryErr = vk.Open(libraryPaths()...)
		if libraryErr != nil {
			libraryErr = fmt.Errorf("loading Vulkan: %w", libraryErr)
		}
	})
	return library, libraryErr
}
