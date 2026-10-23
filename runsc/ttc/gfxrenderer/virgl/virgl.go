// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package virgl is TTC's renderer for Mesa's virgl OpenGL driver: it
// executes the commands that the driver serializes in a virtio-gpu context
// of virgl's capability sets, as virglrenderer's vrend does, on vkgl, the
// renderer's Gallium on Vulkan, in Go (package vkgl), on the Vulkan of TTC's
// Venus renderer: MoltenVK on macOS, and the host's loader on Linux.
//
// virgl's commands are Gallium's calls, of Gallium's state objects and TGSI
// shaders. The decoder (context_unsafe.go) checks each command and resolves
// its handles; the backend (vkgl_unsafe.go) checks what Vulkan needs of its
// fields, translates its shaders to SPIR-V (package tospirv), and renders
// it. A command that a guest's driver would not send ends its stream, as in
// virglrenderer.
package virgl

// Capset is a capability set of virgl's.
type Capset struct {
	ID         uint32
	MaxVersion uint32
	Data       []byte
}

// ResourceArgs are the parameters of a guest's classic resource, as
// DRM_IOCTL_VIRTGPU_RESOURCE_CREATE has them.
type ResourceArgs struct {
	Target    uint32
	Format    uint32
	Bind      uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	ArraySize uint32
	LastLevel uint32
	NrSamples uint32
	Flags     uint32
}

// TransferArgs are a transfer's, as DRM_IOCTL_VIRTGPU_TRANSFER_TO_HOST
// and DRM_IOCTL_VIRTGPU_TRANSFER_FROM_HOST have them.
type TransferArgs struct {
	ToHost      bool
	X, Y, Z     uint32
	W, H, D     uint32
	Level       uint32
	Offset      uint64
	Stride      uint32
	LayerStride uint32
}
