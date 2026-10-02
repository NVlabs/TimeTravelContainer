// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"
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

// gunzip decompresses gzip-compressed b.
func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
