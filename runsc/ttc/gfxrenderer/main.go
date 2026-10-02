// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// connFD is the descriptor of a renderer's connection to the broker or ttcd.
const connFD = 3

// Main runs a renderer with args, its mode and flags, and returns its exit
// status:
//
//	serve-family [--unconfined]  renders for the branches of a family, whose
//	                             connections the broker connected to fd 3
//	                             gives it
//	probe [--unconfined]         prints the renderer's capability sets
//	launch                       starts renderers for the ttcd connected to
//	                             fd 3
//
// A renderer confines itself before it loads Vulkan, unless --unconfined.
// rendererArgs precede the mode in the arguments of the renderers that a
// launcher starts, of its own executable: ttcd's _gfx-renderer, where ttcd
// is the renderer.
func Main(args []string, rendererArgs []string) int {
	if len(args) < 1 {
		return usage(os.Stderr)
	}
	mode := args[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	unconfined := flags.Bool("unconfined", false, "run without the renderer's sandbox, as ttcd does with --unsafe-no-host-isolation")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	switch mode {
	case "serve-family", "probe":
		if !*unconfined {
			if err := Confine(); err != nil {
				return fatal(fmt.Errorf("confining the renderer: %w", err))
			}
		}
	case "launch":
		if *unconfined {
			return usage(os.Stderr)
		}
	default:
		return usage(os.Stderr)
	}
	switch mode {
	case "serve-family":
		if err := ServeFamily(connFD); err != nil {
			return fatal(err)
		}
	case "probe":
		capsets, err := Probe()
		if err != nil {
			return fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(capsets); err != nil {
			return fatal(err)
		}
	case "launch":
		// The launcher stays unconfined, so that its renderers can confine
		// themselves: see ServeLauncher.
		exe, err := os.Executable()
		if err != nil {
			return fatal(err)
		}
		if err := ServeLauncher(connFD, exe, rendererArgs); err != nil {
			return fatal(err)
		}
	}
	return 0
}

func fatal(err error) int {
	fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
	return 1
}

func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: ttc-gfx-renderer serve-family|probe [--unconfined]")
	fmt.Fprintln(w, "       ttc-gfx-renderer launch")
	return 2
}
