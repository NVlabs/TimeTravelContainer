// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"slices"
	"testing"
)

func TestRendererArgsPrecedeTheMode(t *testing.T) {
	// ttcd, as its renderer on macOS.
	opts := BrokerOptions{Renderer: "/usr/local/bin/ttcd", RendererArgs: []string{"_gfx-renderer"}, Unconfined: true}
	if got, want := opts.args("serve-family"), []string{"_gfx-renderer", "serve-family", "--unconfined"}; !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	// The renderer of its own.
	opts = BrokerOptions{Renderer: "/usr/local/bin/ttc-gfx-renderer"}
	if got, want := opts.args("probe"), []string{"probe"}; !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	// args does not change RendererArgs, which every renderer shares.
	shared := make([]string, 1, 4)
	shared[0] = "_gfx-renderer"
	opts = BrokerOptions{RendererArgs: shared}
	a, b := opts.args("probe"), opts.args("launch")
	if a[1] != "probe" || b[1] != "launch" {
		t.Errorf("args share their storage: %q, %q", a, b)
	}
}

func TestMainRefusesUnknownModes(t *testing.T) {
	for _, args := range [][]string{nil, {"render"}, {"launch", "--unconfined"}} {
		if status := Main(args, nil); status != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, status)
		}
	}
}
