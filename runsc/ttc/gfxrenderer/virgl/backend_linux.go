// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package virgl

// initBackend creates the process's backend: on Linux, vkgl, Gallium's
// state and guests' shaders on Vulkan in Go, of no Mesa.
func initBackend() (backend, error) {
	return initVkgl()
}
