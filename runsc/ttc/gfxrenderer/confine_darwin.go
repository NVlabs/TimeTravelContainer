// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"os"
	"path/filepath"
	"strings"

	"gvisor.dev/gvisor/runsc/ttc/seatbelt"
)

// Confine confines the renderer, and whatever it starts, with the macOS
// sandbox to seatbelt.RendererProfile. The renderer calls it before it loads
// gfxstream or reads anything from its Sentry.
//
// It fails in a process that is confined already, such as a child of a
// confined ttcd: macOS lets no process change its profile, and ttcd's bars
// the GPU. Such a daemon starts renderers through a launcher that it started
// before it confined itself (see ServeLauncher).
func Confine(opts Options) error {
	setVulkanLoader()
	var readable []string
	add := func(path string) {
		if !strings.Contains(path, "/") {
			// A bare library name is the dynamic linker's to find, in
			// directories that the profile admits or not.
			return
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return
		}
		readable = append(readable, path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
			readable = append(readable, resolved)
		}
	}
	if exe, err := os.Executable(); err == nil {
		add(exe)
	}
	add(opts.library())
	add(os.Getenv("ANDROID_EMU_VK_LOADER_PATH"))
	profile, err := seatbelt.RendererProfile(readable, opts.ScratchDir)
	if err != nil {
		return err
	}
	return seatbelt.Apply(profile)
}
