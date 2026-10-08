// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"context"
	"slices"
	"testing"
)

func TestRenderersRunAsName(t *testing.T) {
	// ttcd, as its renderer on macOS.
	opts := BrokerOptions{Renderer: "/usr/local/bin/ttcd", Unconfined: true}
	cmd := opts.command(context.Background(), "serve-family")
	if got, want := cmd.Args, []string{"ttc-gfx-renderer", "serve-family", "--unconfined"}; !slices.Equal(got, want) || cmd.Path != opts.Renderer {
		t.Errorf("command = %s %q, want %s %q", cmd.Path, got, opts.Renderer, want)
	}
	// The renderer of its own.
	opts = BrokerOptions{Renderer: "/usr/local/bin/ttc-gfx-renderer"}
	cmd = opts.command(context.Background(), "probe")
	if got, want := cmd.Args, []string{"ttc-gfx-renderer", "probe"}; !slices.Equal(got, want) || cmd.Path != opts.Renderer {
		t.Errorf("command = %s %q, want %s %q", cmd.Path, got, opts.Renderer, want)
	}
}

func TestMainRefusesUnknownModes(t *testing.T) {
	for _, args := range [][]string{nil, {"render"}, {"launch", "--unconfined"}} {
		if status := Main(args); status != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, status)
		}
	}
}
