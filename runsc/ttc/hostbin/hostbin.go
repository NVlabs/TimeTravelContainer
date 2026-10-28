// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hostbin carries the helpers that ttcd runs on Linux, and that link
// the C library, as ttcd does not: the GPU renderer and ttc-cuda-checkpoint;
// and the C sidecars that the Sentry and the runtime run; all
// gzip-compressed. A build for macOS, where ttcd is its renderer, for a
// platform without cgo, as cross-builds are, or with
// //runsc/ttc/hostbin:carry_helpers=false carries none of them.
package hostbin

import _ "embed"

//go:embed ttc-gfx-renderer.gz
var gfxRenderer []byte

//go:embed ttc-cuda-checkpoint.gz
var cudaCheckpoint []byte

//go:embed gvisor-sentry-prewarmer.gz
var prewarmer []byte

//go:embed runsc-fd-parking.gz
var fdParking []byte

//go:embed gvisor-proc-umounter.gz
var procUmounter []byte

// Sidecars returns the C sidecars of //runsc/gvisorbinaries that this build
// carries, gzip-compressed, by name.
func Sidecars() map[string][]byte {
	sidecars := make(map[string][]byte)
	for name, gz := range map[string][]byte{
		"gvisor-sentry-prewarmer": prewarmer,
		"runsc-fd-parking":        fdParking,
		"gvisor-proc-umounter":    procUmounter,
	} {
		if b := carried(gz); b != nil {
			sidecars[name] = b
		}
	}
	return sidecars
}

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
