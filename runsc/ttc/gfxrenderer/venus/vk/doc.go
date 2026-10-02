// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vk calls Vulkan for the Venus renderer: it loads the Vulkan loader
// (or a driver), and calls commands, with arguments that package vn decoded,
// through procedure tables, as virglrenderer does. It needs cgo.
//
// Arguments are the C structs that vn decodes into, in memory that is not
// Go's, so that they pass to C as they are.
package vk
