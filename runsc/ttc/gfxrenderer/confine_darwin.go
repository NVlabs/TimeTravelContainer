// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"os"
	"path/filepath"

	"gvisor.dev/gvisor/runsc/ttc/seatbelt"
)

// Confine confines the renderer, and whatever it starts, with the macOS
// sandbox to seatbelt.RendererProfile. The renderer calls it before it loads
// Vulkan or reads anything from its Sentry.
//
// It fails in a process that is confined already, such as a child of a
// confined ttcd: macOS lets no process change its profile, and ttcd's bars
// the GPU. Such a daemon starts renderers through a launcher that it started
// before it confined itself (see ServeLauncher).
func Confine() error {
	// The renderer reads its executable, by the path it ran and the path
	// that resolves.
	var readable []string
	if exe, err := os.Executable(); err == nil {
		if exe, err := filepath.Abs(exe); err == nil {
			readable = append(readable, exe)
			if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
				readable = append(readable, resolved)
			}
		}
	}
	profile, err := seatbelt.RendererProfile(readable)
	if err != nil {
		return err
	}
	return seatbelt.Apply(profile)
}
