// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbin

import (
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// oldestGlibc is the oldest glibc that the helpers run with: 2.17, that of
// glibc's arm64 port.
var oldestGlibc = [2]int{2, 17}

type helper struct {
	name string
	gz   []byte
	// args run the helper to an exit of one of codes, from its start
	// through the C library's.
	args  []string
	codes []int
}

func helpers() []helper {
	return []helper{
		// Its usage, from Go's runtime, which starts threads.
		{name: "ttc-gfx-renderer", gz: GFXRenderer(), codes: []int{2}},
		// Its loading of the CUDA driver, there or not.
		{name: "ttc-cuda-checkpoint", gz: CUDACheckpoint(), args: []string{"--get-state", "--pid", "1"}, codes: []int{0, 1}},
	}
}

func unpack(t *testing.T, h helper) []byte {
	t.Helper()
	if h.gz == nil {
		t.Skipf("this build carries no %s", h.name)
	}
	r, err := gzip.NewReader(bytes.NewReader(h.gz))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// glibcVersion parses a glibc symbol version, as GLIBC_2.2.5.
func glibcVersion(v string) ([2]int, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "GLIBC_"), ".")
	if len(parts) < 2 {
		return [2]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return [2]int{major, minor}, err1 == nil && err2 == nil
}

func TestHelpersNeedNoNewerGlibc(t *testing.T) {
	for _, h := range helpers() {
		t.Run(h.name, func(t *testing.T) {
			f, err := elf.NewFile(bytes.NewReader(unpack(t, h)))
			if err != nil {
				t.Fatal(err)
			}
			symbols, err := f.ImportedSymbols()
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range symbols {
				if !strings.HasPrefix(s.Version, "GLIBC_") {
					continue
				}
				v, ok := glibcVersion(s.Version)
				if !ok || v[0] > oldestGlibc[0] || v[0] == oldestGlibc[0] && v[1] > oldestGlibc[1] {
					t.Errorf("%s needs %s@%s from %s, newer than glibc %d.%d", h.name, s.Name, s.Version, s.Library, oldestGlibc[0], oldestGlibc[1])
				}
			}
		})
	}
}

func TestHelpersRun(t *testing.T) {
	for _, h := range helpers() {
		t.Run(h.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), h.name)
			if err := os.WriteFile(path, unpack(t, h), 0755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := exec.CommandContext(ctx, path, h.args...).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s %q ran for a minute: %s", h.name, h.args, out)
			}
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			for _, c := range h.codes {
				if code == c {
					return
				}
			}
			t.Errorf("%s %q exited with %d (%v), want one of %v: %s", h.name, h.args, code, err, h.codes, out)
		})
	}
}

// TestSidecarsAreStatic checks that each carried sidecar is a static
// executable, which runs wherever ttcd does.
func TestSidecarsAreStatic(t *testing.T) {
	sidecars := Sidecars()
	if len(sidecars) == 0 {
		t.Skip("this build carries no sidecars")
	}
	for name, gz := range sidecars {
		content := unpack(t, helper{name: name, gz: gz})
		f, err := elf.NewFile(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("%s has an interpreter", name)
			}
		}
	}
}
