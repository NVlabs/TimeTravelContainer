// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo && !darwin

package vk

// Linked returns the Vulkan that the renderer links, of which there is none
// but on macOS: elsewhere it loads the host's loader (see Open).
func Linked() (*Library, bool) {
	return nil, false
}
