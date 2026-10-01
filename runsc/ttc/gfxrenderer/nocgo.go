// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !cgo

package gfxrenderer

import "errors"

var errNoCgo = errors.New("the renderer was built without cgo, which it needs to load gfxstream")

// Serve renders for the Sentry connected to fd. Without cgo, it cannot.
func Serve(fd int, opts Options) error {
	return errNoCgo
}

// ServeFamily serves the branches of a family. Without cgo, it cannot.
func ServeFamily(fd int, opts Options) error {
	return errNoCgo
}

// Probe returns gfxstream's capability sets. Without cgo, it cannot.
func Probe(opts Options) ([]Capset, error) {
	return nil, errNoCgo
}
