// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package gfxrenderer is TTC's GPU renderer: a host process that renders,
// with gfxstream, what the contexts of a sandbox's virtual GPU (see
// pkg/sentry/devices/virtgpu) submit, and speaks pkg/gfxproto to the Sentry.
//
// gfxstream's renderer is global to its process, so each branch that uses a
// GPU has a renderer process of its own.
package gfxrenderer

import (
	"os"
	"path/filepath"
	"runtime"
)

// Options configures a renderer.
type Options struct {
	// Library is the path of gfxstream's host library. If it is empty, the
	// renderer looks beside its executable, then where the dynamic linker
	// looks.
	Library string

	// Features are gfxstream's renderer features, such as
	// "VulkanBatchedDescriptorSetUpdate:disabled".
	Features string

	// Snapshots is whether the renderer takes snapshots and restores them.
	Snapshots bool

	// ScratchDir is, in a renderer that takes snapshots, a directory of its
	// own, where gfxstream writes and reads them.
	ScratchDir string
}

// snapshotFeatures are the gfxstream features of a renderer that takes
// snapshots and restores them:
//   - VulkanSnapshots records how to rebuild each Vulkan object, from the
//     renderer's start;
//   - batched descriptor set updates, which the capability sets offer the
//     guest, are off: their replay fails to restore descriptor sets.
const snapshotFeatures = "VulkanSnapshots:enabled,VulkanBatchedDescriptorSetUpdate:disabled"

// flags returns the renderer's flags for the options.
func (o *Options) flags() []string {
	var flags []string
	if o.Library != "" {
		flags = append(flags, "--library", o.Library)
	}
	if o.Features != "" {
		flags = append(flags, "--features", o.Features)
	}
	if o.Snapshots {
		flags = append(flags, "--snapshots")
	}
	return flags
}

// libraryName is the file name of gfxstream's host library.
func libraryName() string {
	if runtime.GOOS == "darwin" {
		return "libgfxstream_backend.dylib"
	}
	return "libgfxstream_backend.so"
}

// library returns the path of gfxstream's host library.
func (o *Options) library() string {
	if o.Library != "" {
		return o.Library
	}
	if exe, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(exe), libraryName())
		if _, err := os.Stat(beside); err == nil {
			return beside
		}
	}
	return libraryName()
}

// setVulkanLoader tells gfxstream where the Vulkan loader is, which it looks
// for only beside the program on macOS.
func setVulkanLoader() {
	if runtime.GOOS != "darwin" || os.Getenv("ANDROID_EMU_VK_LOADER_PATH") != "" {
		return
	}
	for _, path := range []string{"/opt/homebrew/lib/libvulkan.dylib", "/usr/local/lib/libvulkan.dylib"} {
		if _, err := os.Stat(path); err == nil {
			os.Setenv("ANDROID_EMU_VK_LOADER_PATH", path)
			return
		}
	}
}

// Capset is a capability set of the renderer.
type Capset struct {
	ID         uint32 `json:"id"`
	MaxVersion uint32 `json:"max_version"`
	Data       []byte `json:"data"`
}
