// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hostbin carries the helpers that ttcd runs on Linux, and that link
// the C library, as ttcd does not: the GPU renderer and ttc-cuda-checkpoint,
// gzip-compressed. A build for macOS, where ttcd is its renderer, for a
// platform without cgo, as cross-builds are, or with
// //runsc/ttc/hostbin:carry_helpers=false carries neither.
package hostbin

import _ "embed"

//go:embed ttc-gfx-renderer.gz
var gfxRenderer []byte

//go:embed ttc-cuda-checkpoint.gz
var cudaCheckpoint []byte

// GFXRenderer returns the GPU renderer, gzip-compressed, or nil if this
// build carries none.
func GFXRenderer() []byte { return carried(gfxRenderer) }

// CUDACheckpoint returns ttc-cuda-checkpoint, gzip-compressed, or nil if this
// build carries none.
func CUDACheckpoint() []byte { return carried(cudaCheckpoint) }

func carried(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
