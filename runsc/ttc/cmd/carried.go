// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/gvisorbinaries"
	"gvisor.dev/gvisor/runsc/specutils"
)

// carriedHelpers are the executables that ttcd carries, gzip-compressed: on
// Linux, where ttcd links no C library, the GPU renderer and
// ttc-cuda-checkpoint, which do. A build without them carries none, and ttcd
// looks for them beside itself. On macOS ttcd is its renderer (see
// GFXRendererArg).
var carriedHelpers struct {
	gfxRenderer    []byte
	cudaCheckpoint []byte

	rendererOnce sync.Once
	renderer     string
	rendererErr  error
}

// SetCarriedHelpers registers the helpers that ttcd carries, gzip-compressed,
// either of which may be empty. It must be called before ttcd serves.
func SetCarriedHelpers(gfxRenderer, cudaCheckpoint []byte) {
	carriedHelpers.gfxRenderer = gfxRenderer
	carriedHelpers.cudaCheckpoint = cudaCheckpoint
}

// carriedRenderer returns the path from which the GPU renderer that ttcd
// carries runs, or "" if it carries none. ttcd decompresses it once, into a
// memory file that it keeps open, or into dir where a memory file cannot run.
func carriedRenderer(dir string) (string, error) {
	h := &carriedHelpers
	if len(h.gfxRenderer) == 0 {
		return "", nil
	}
	h.rendererOnce.Do(func() {
		var content []byte
		if content, h.rendererErr = gunzip(h.gfxRenderer); h.rendererErr == nil {
			h.renderer, h.rendererErr = runnableExecutable(gfxRendererName, content, dir)
		}
	})
	return h.renderer, h.rendererErr
}

// carriedCUDACheckpoint writes the ttc-cuda-checkpoint that ttcd carries in
// dir, unless it is there already, and returns its path, or "" if ttcd
// carries none. Containers get it by a mount, which needs a file.
func carriedCUDACheckpoint(dir string) (string, error) {
	if len(carriedHelpers.cudaCheckpoint) == 0 || dir == "" {
		return "", nil
	}
	content, err := gunzip(carriedHelpers.cudaCheckpoint)
	if err != nil {
		return "", err
	}
	return writeContainerFile(dir, cudaCheckpointName, content, 0555)
}

// carriedSidecars are the C sidecars of gvisorbinaries that ttcd carries,
// gzip-compressed, by name, and the paths from which they run, once made.
var carriedSidecars struct {
	mu    sync.Mutex
	gz    map[string][]byte
	paths map[string]string
}

// SetCarriedSidecars registers the C sidecars that ttcd carries,
// gzip-compressed, by name. It must be called before ttcd runs anything.
func SetCarriedSidecars(gz map[string][]byte) {
	carriedSidecars.mu.Lock()
	defer carriedSidecars.mu.Unlock()
	carriedSidecars.gz = gz
}

// CarriedSidecar implements gvisorbinaries.Provider for ttcd, which carries
// its sidecars rather than installing them beside itself: ttcd is the Sentry,
// and each C sidecar that it carries runs from a sealed memory file that the
// process asking for it makes, once, and keeps open.
func CarriedSidecar(name string) (string, bool) {
	if name == gvisorbinaries.GvisorSentry.Name {
		return specutils.ExePath, true
	}
	carriedSidecars.mu.Lock()
	defer carriedSidecars.mu.Unlock()
	if path, ok := carriedSidecars.paths[name]; ok {
		return path, true
	}
	gz := carriedSidecars.gz[name]
	if len(gz) == 0 {
		return "", false
	}
	content, err := gunzip(gz)
	if err != nil {
		log.Warningf("Unpacking the carried sidecar %q: %v", name, err)
		return "", false
	}
	path, err := runnableExecutable(name, content, "")
	if err != nil {
		log.Warningf("Making the carried sidecar %q runnable: %v", name, err)
		return "", false
	}
	if carriedSidecars.paths == nil {
		carriedSidecars.paths = make(map[string]string)
	}
	carriedSidecars.paths[name] = path
	return path, true
}

// gunzip decompresses gzip-compressed b.
func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
