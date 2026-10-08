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

// Name is the renderer's name, which its processes take as their argv[0],
// whatever their executable: the renderer's own, or ttcd, which is the
// renderer on macOS when it runs as Name, as it is runsc's gofer when it runs
// as runsc-gofer.
const Name = "ttc-gfx-renderer"

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
// A renderer confines itself before it loads Vulkan, unless --unconfined. A
// launcher starts renderers of its own executable.
func Main(args []string) int {
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
		if err := ServeLauncher(connFD, exe); err != nil {
			return fatal(err)
		}
	}
	return 0
}

func fatal(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", Name, err)
	return 1
}

func usage(w io.Writer) int {
	fmt.Fprintf(w, "usage: %s serve-family|probe [--unconfined]\n", Name)
	fmt.Fprintf(w, "       %s launch\n", Name)
	return 2
}
