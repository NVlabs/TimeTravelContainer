// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !cgo

// Package venus is TTC's renderer for Mesa's Venus Vulkan driver. It needs
// cgo, to call Vulkan.
package venus

import "errors"

var errNoCgo = errors.New("the renderer was built without cgo, which it needs to call Vulkan")

// CapsetVenus is the ID of Venus's capability set.
const CapsetVenus = 4

// Serve renders for the Sentry connected to fd. Without cgo, it cannot.
func Serve(fd int) error {
	return errNoCgo
}

// Probe returns Venus's capability set. Without cgo, it cannot.
func Probe() ([]byte, error) {
	return nil, errNoCgo
}
