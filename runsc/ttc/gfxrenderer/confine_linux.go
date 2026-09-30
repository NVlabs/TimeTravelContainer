// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

// Confine does nothing yet: Linux's renderers run as ttcd's children, with
// ttcd's privileges. Confining them, as runsc confines its sandboxes, is for
// the renderer's Linux support (plan item B1) to do.
func Confine(Options) error {
	return nil
}
