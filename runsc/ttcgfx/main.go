// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary ttc-gfx-renderer is TTC's GPU renderer. ttcd starts one for each
// family whose virtual GPU is in use:
//
//	ttc-gfx-renderer serve-family [--unconfined]
//	                                 renders for the branches of a family,
//	                                 whose connections the broker connected
//	                                 to fd 3 gives it
//	ttc-gfx-renderer probe [--unconfined]
//	                                 prints the renderer's capability sets
//	ttc-gfx-renderer launch          starts renderers for the ttcd connected
//	                                 to fd 3
//
// A renderer confines itself before it loads Vulkan, unless --unconfined.
package main

import (
	"os"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
	// It runs with the host's glibc, of any version since 2.17.
	_ "gvisor.dev/gvisor/runsc/ttc/glibccompat"
)

func main() {
	os.Exit(gfxrenderer.Main(os.Args[1:]))
}
