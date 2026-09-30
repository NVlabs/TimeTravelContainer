// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary ttc-gfx-renderer is TTC's GPU renderer. ttcd starts one for each
// branch whose virtual GPU is in use:
//
//	ttc-gfx-renderer serve [flags]  renders for the Sentry connected to fd 3
//	ttc-gfx-renderer probe [flags]  prints the renderer's capability sets
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
)

// connFD is the descriptor of the connection to the Sentry.
const connFD = 3

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var opts gfxrenderer.Options
	flags.StringVar(&opts.Library, "library", "", "path of gfxstream's host library")
	flags.StringVar(&opts.Features, "features", "", "gfxstream renderer features")
	flags.Parse(os.Args[2:])
	switch os.Args[1] {
	case "serve":
		if err := gfxrenderer.Serve(connFD, opts); err != nil {
			fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
			os.Exit(1)
		}
	case "probe":
		capsets, err := gfxrenderer.Probe(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(capsets); err != nil {
			fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ttc-gfx-renderer serve|probe [--library PATH] [--features FEATURES]")
	os.Exit(2)
}
