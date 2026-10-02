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
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
)

// connFD is the descriptor of the connection to the broker or ttcd.
const connFD = 3

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	unconfined := flags.Bool("unconfined", false, "run without the renderer's sandbox, as ttcd does with --unsafe-no-host-isolation")
	flags.Parse(os.Args[2:])
	mode := os.Args[1]
	switch mode {
	case "serve-family", "probe":
		if !*unconfined {
			if err := gfxrenderer.Confine(); err != nil {
				fatal(fmt.Errorf("confining the renderer: %w", err))
			}
		}
	case "launch":
		if *unconfined {
			usage()
		}
	default:
		usage()
	}
	switch mode {
	case "serve-family":
		if err := gfxrenderer.ServeFamily(connFD); err != nil {
			fatal(err)
		}
	case "probe":
		capsets, err := gfxrenderer.Probe()
		if err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(capsets); err != nil {
			fatal(err)
		}
	case "launch":
		// The launcher stays unconfined, so that its renderers can confine
		// themselves: see gfxrenderer.ServeLauncher.
		exe, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		if err := gfxrenderer.ServeLauncher(connFD, exe); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ttc-gfx-renderer serve-family|probe [--unconfined]")
	fmt.Fprintln(os.Stderr, "       ttc-gfx-renderer launch")
	os.Exit(2)
}
