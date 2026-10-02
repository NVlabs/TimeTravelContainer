// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func gzipped(t *testing.T, content []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMemoryExecutableRuns(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skipf("no /bin/true to run: %v", err)
	}
	path, err := memoryExecutable("true", content)
	if err != nil {
		t.Skipf("memory files do not run here: %v", err)
	}
	fd, err := strconv.Atoi(strings.TrimPrefix(path, "/proc/self/fd/"))
	if err != nil || fd < carriedExecutableFD {
		t.Fatalf("memoryExecutable = %q, want /proc/self/fd/N with N >= %d", path, carriedExecutableFD)
	}
	defer unix.Close(fd)
	seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE; seals&want != want {
		t.Errorf("seals = %#x, want %#x", seals, want)
	}
	// A child, whose descriptors os/exec moves, runs it.
	cmd := exec.Command(path)
	cmd.ExtraFiles = []*os.File{os.Stdin, os.Stdout, os.Stderr}
	if err := cmd.Run(); err != nil {
		t.Fatalf("running %s: %v", path, err)
	}
}

func TestCarriedCUDACheckpointIsWrittenOnce(t *testing.T) {
	defer SetCarriedHelpers(carriedHelpers.gfxRenderer, carriedHelpers.cudaCheckpoint)
	content := []byte("\x7fELF a cuda-checkpoint")
	SetCarriedHelpers(nil, gzipped(t, content))
	dir := t.TempDir()
	path, err := carriedCUDACheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, cudaCheckpointName) {
		t.Fatalf("carriedCUDACheckpoint = %q, want it in %s", path, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("written %q (%v), want %q", got, err, content)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("mode %v (%v), want 0555", info.Mode(), err)
	}
	// The second time finds it, and writes nothing.
	before := info.ModTime()
	if again, err := carriedCUDACheckpoint(dir); err != nil || again != path {
		t.Fatalf("again: %q, %v", again, err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(before) {
		t.Fatalf("rewritten: %v, %v", info.ModTime(), err)
	}
	// A ttcd that carries none says so, for the file beside it.
	SetCarriedHelpers(nil, nil)
	if none, err := carriedCUDACheckpoint(dir); none != "" || err != nil {
		t.Fatalf("carrying none: %q, %v", none, err)
	}
}

func TestRendererOptionsRunTheCarriedRenderer(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skipf("no /bin/true: %v", err)
	}
	saved := carriedHelpers.gfxRenderer
	defer func() {
		carriedHelpers.gfxRenderer = saved
		carriedHelpers.rendererOnce = sync.Once{}
		carriedHelpers.renderer, carriedHelpers.rendererErr = "", nil
	}()
	t.Setenv("TTC_GFX_RENDERER", "")
	carriedHelpers.gfxRenderer = gzipped(t, content)
	carriedHelpers.rendererOnce = sync.Once{}
	opts, err := rendererOptions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// It runs as ttc-gfx-renderer, from a link to its memory file, or from a
	// file where memory files do not run.
	if filepath.Base(opts.Renderer) != gfxRendererName {
		t.Fatalf("Renderer = %q, want the carried renderer, as %s", opts.Renderer, gfxRendererName)
	}
	if len(opts.RendererArgs) != 0 {
		t.Fatalf("RendererArgs = %q, want none on Linux", opts.RendererArgs)
	}
	if err := exec.Command(opts.Renderer).Run(); err != nil {
		t.Fatalf("running %s: %v", opts.Renderer, err)
	}
}
