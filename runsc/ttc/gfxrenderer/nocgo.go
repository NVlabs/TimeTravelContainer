// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !cgo

package gfxrenderer

import "errors"

var errNoCgo = errors.New("the renderer was built without cgo, which it needs to call Vulkan")

// ServeFamily serves the branches of a family. Without cgo, it cannot.
func ServeFamily(fd int) error {
	return errNoCgo
}

// Probe returns the renderer's capability sets. Without cgo, it cannot.
func Probe() ([]Capset, error) {
	return nil, errNoCgo
}
