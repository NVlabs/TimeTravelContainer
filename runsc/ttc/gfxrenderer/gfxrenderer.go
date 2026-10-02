// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package gfxrenderer is TTC's GPU renderer: a host process that renders,
// with TTC's Venus renderer (see venus), what the contexts of the virtual
// GPUs (see pkg/sentry/devices/virtgpu) of a family's branches submit, and
// speaks pkg/gfxproto to their Sentry.
//
// One renderer serves a family, a connection for each branch, so that a
// fork's child shares what its parent compiled. The broker, which ttcd runs
// for each Sentry, starts it, through a launcher where ttcd confines itself,
// and the renderer confines itself.
package gfxrenderer

// Capset is a capability set of the renderer.
type Capset struct {
	ID         uint32 `json:"id"`
	MaxVersion uint32 `json:"max_version"`
	Data       []byte `json:"data"`
}
