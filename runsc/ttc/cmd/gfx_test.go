// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

func TestGFXGuestEdits(t *testing.T) {
	driverDir := t.TempDir()
	driver := filepath.Join(driverDir, gfxGuestDriverName)
	if err := os.WriteFile(driver, []byte("driver"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_GFX_GUEST_DRIVER", driverDir)
	zink := gfxGuestZinkByDefault
	t.Cleanup(func() { gfxGuestZinkByDefault = zink })
	gfxGuestZinkByDefault = true
	g := newGFXService(t.TempDir(), filepath.Join(t.TempDir(), "gfx"), gfxHost{}, nil)

	if mounts, env, err := g.guestEdits(ttcbundle.GPUOptions{}); mounts != nil || env != nil || err != nil {
		t.Errorf("guestEdits without --gfx = %v, %v, %v; want nothing", mounts, env, err)
	}
	for i := 0; i < 2; i++ {
		mounts, env, err := g.guestEdits(ttcbundle.GPUOptions{GFX: true})
		if err != nil {
			t.Fatalf("guestEdits: %v", err)
		}
		if !slices.Equal(env, gfxGuestEnv) {
			t.Errorf("env = %q, want %q", env, gfxGuestEnv)
		}
		sources := make(map[string]string)
		for _, m := range mounts {
			sources[m.Destination] = m.Source
		}
		if len(sources) != 3 || sources[gfxGuestDriverPath] != driver {
			t.Fatalf("mounts = %+v, want the driver, its manifest, and the OpenGL configuration", mounts)
		}
		var manifest struct {
			ICD struct {
				LibraryPath string `json:"library_path"`
			}
		}
		content, err := os.ReadFile(sources[gfxGuestManifestPath])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(content, &manifest); err != nil || manifest.ICD.LibraryPath != gfxGuestDriverPath {
			t.Errorf("manifest %s: %v, library %q", content, err, manifest.ICD.LibraryPath)
		}
		content, err = os.ReadFile(sources[gfxGuestDriconfPath])
		if err != nil || !bytes.Equal(content, []byte(gfxGuestDriconf)) {
			t.Errorf("OpenGL configuration %q, %v", content, err)
		}
		for _, source := range []string{sources[gfxGuestManifestPath], sources[gfxGuestDriconfPath]} {
			if info, err := os.Stat(source); err != nil || info.Mode().Perm() != 0444 {
				t.Errorf("stat %s: %v, %v; want mode 0444", source, info, err)
			}
		}
	}
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".json.") || strings.Contains(e.Name(), ".conf.") {
			t.Errorf("left a temporary file %s", e.Name())
		}
	}
}

func TestGFXGuestEditsWithoutZink(t *testing.T) {
	driverDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(driverDir, gfxGuestDriverName), []byte("driver"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_GFX_GUEST_DRIVER", driverDir)
	zink := gfxGuestZinkByDefault
	t.Cleanup(func() { gfxGuestZinkByDefault = zink })
	gfxGuestZinkByDefault = false
	g := newGFXService(t.TempDir(), filepath.Join(t.TempDir(), "gfx"), gfxHost{}, nil)
	mounts, _, err := g.guestEdits(ttcbundle.GPUOptions{GFX: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts {
		if m.Destination == gfxGuestDriconfPath {
			t.Errorf("mounts = %+v, want no OpenGL configuration", mounts)
		}
	}
	if len(mounts) != 2 {
		t.Errorf("mounts = %+v, want the driver and its manifest", mounts)
	}
}

func TestGFXGuestEditsWithoutDriver(t *testing.T) {
	t.Setenv("TTC_GFX_GUEST_DRIVER", t.TempDir())
	var stderr bytes.Buffer
	g := newGFXService(t.TempDir(), filepath.Join(t.TempDir(), "gfx"), gfxHost{}, &stderr)
	for i := 0; i < 2; i++ {
		if mounts, env, err := g.guestEdits(ttcbundle.GPUOptions{GFX: true}); mounts != nil || env != nil || err != nil {
			t.Errorf("guestEdits without a driver = %v, %v, %v; want nothing", mounts, env, err)
		}
	}
	if n := strings.Count(stderr.String(), "no guest Vulkan driver"); n != 1 {
		t.Errorf("warned %d times, want once: %q", n, stderr.String())
	}
}
