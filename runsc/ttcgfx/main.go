// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary ttc-gfx-renderer is TTC's GPU renderer. ttcd starts one for each
// branch whose virtual GPU is in use:
//
//	ttc-gfx-renderer serve [flags]   renders for the Sentry connected to fd 3
//	ttc-gfx-renderer probe [flags]   prints the renderer's capability sets
//	ttc-gfx-renderer launch [flags]  starts renderers, with flags, for the
//	                                 ttcd connected to fd 3
//
// A renderer confines itself before it loads gfxstream, unless --unconfined.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	flags.BoolVar(&opts.Snapshots, "snapshots", false, "take snapshots and restore them")
	unconfined := flags.Bool("unconfined", false, "run without the renderer's sandbox, as ttcd does with --unsafe-no-host-isolation")
	flags.Parse(os.Args[2:])
	mode := os.Args[1]
	switch mode {
	case "serve", "probe":
		if mode == "serve" && opts.Snapshots {
			// gfxstream takes and restores snapshots in a directory, which
			// is the renderer's own.
			dir, err := os.MkdirTemp("", "ttc-gfx-snapshots.")
			if err != nil {
				fatal(err)
			}
			defer os.RemoveAll(dir)
			// The sandbox's rules name the directory's own path, not one
			// through a symbolic link, such as macOS's /var.
			if dir, err = filepath.EvalSymlinks(dir); err != nil {
				fatal(err)
			}
			opts.ScratchDir = dir
		}
		if !*unconfined {
			if err := gfxrenderer.Confine(opts); err != nil {
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
	case "serve":
		if err := gfxrenderer.Serve(connFD, opts); err != nil {
			if opts.ScratchDir != "" {
				os.RemoveAll(opts.ScratchDir)
			}
			fatal(err)
		}
	case "probe":
		capsets, err := gfxrenderer.Probe(opts)
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
		if err := gfxrenderer.ServeLauncher(connFD, exe, opts); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "ttc-gfx-renderer: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ttc-gfx-renderer serve|probe [--library PATH] [--features FEATURES] [--snapshots] [--unconfined]")
	fmt.Fprintln(os.Stderr, "       ttc-gfx-renderer launch [--library PATH] [--features FEATURES]")
	os.Exit(2)
}
