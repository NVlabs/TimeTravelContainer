// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vkgl renders the Gallium state and the TGSI shaders of virgl's
// guests on Vulkan, in Go: the renderer's virgl backend that is not Mesa's
// Zink (g3doc/proposals/ttc_virgl_go_backend.md). It calls the Vulkan that
// the Venus renderer loads, with the arguments' C structs of package vn,
// in memory that is not Go's.
package vkgl
