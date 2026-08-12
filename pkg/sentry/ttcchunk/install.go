// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcchunk

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// InstalledChunk maps one newly allocated resident chunk to a virtual stream
// offset in ChunkReader. Reused resident chunks are omitted.
type InstalledChunk struct {
	Object       ID
	Target       memmap.FileRange
	StreamOffset uint64
}

// InstallResult is the process-local product of installing a manifest.
type InstallResult struct {
	Relocation  *pgalloc.SparseRelocation
	Chunks      []InstalledChunk
	transaction *pgalloc.SparseChunkInstallation
}

// Commit transfers installed page references to a successfully restored graph.
func (r *InstallResult) Commit() error { return r.transaction.Commit() }

// Abort rolls back an incomplete restore.
func (r *InstallResult) Abort() error { return r.transaction.Abort() }

// InstallManifest validates manifest objects, installs or reuses their 1 MiB
// resident ranges, and builds the old-offset relocation table used by state
// restore. Newly allocated ranges remain zero until Chunks are attached to an
// asynchronous ChunkReader.
func (r *MemoryRepository) InstallManifest(mf *pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32) (InstallResult, error) {
	if mf == nil {
		return InstallResult{}, fmt.Errorf("installing manifest into nil MemoryFile")
	}
	objects := make(map[ID]Chunk)
	for _, run := range manifest.Runs {
		if _, ok := objects[run.Object]; ok {
			continue
		}
		chunk, err := r.chunks.Get(run.Object)
		if err != nil {
			return InstallResult{}, fmt.Errorf("loading memory object %s: %w", run.Object, err)
		}
		objects[run.Object] = chunk
	}
	installs := make([]pgalloc.SparseChunkInstall, 0, len(objects))
	for object, chunk := range objects {
		pagesBySlot := make(map[uint8]Page, len(chunk.Pages))
		for _, page := range chunk.Pages {
			pagesBySlot[page.Slot] = page
		}
		install := pgalloc.SparseChunkInstall{
			Object: pgalloc.SparseChunkObjectID(object), Logical: chunk.Logical,
			Kind: kind, MemCgID: memCgID,
		}
		for _, run := range manifest.Runs {
			if run.Object != object {
				continue
			}
			for i := uint16(0); i < run.Pages; i++ {
				slot := run.FirstSlot + uint8(i)
				page, ok := pagesBySlot[slot]
				version := pgalloc.PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + uint64(i)}
				if !ok || page.Version != version {
					return InstallResult{}, fmt.Errorf("manifest object %s slot %d version mismatch", object, slot)
				}
				install.Pages = append(install.Pages, pgalloc.SparseChunkPage{
					Slot:    slot,
					Page:    pgalloc.LogicalPageID{Allocator: run.FirstPage.Allocator, Sequence: run.FirstPage.Sequence + uint64(i)},
					Version: version, References: run.References,
				})
			}
		}
		installs = append(installs, install)
	}
	transaction, err := mf.InstallSparseChunks(installs)
	if err != nil {
		return InstallResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Abort()
		}
	}()
	newRanges := transaction.NewRanges()
	relocation := pgalloc.NewSparseRelocation()
	for _, run := range manifest.Runs {
		resident, ok := mf.SparseChunkRange(pgalloc.SparseChunkObjectID(run.Object))
		if !ok {
			return InstallResult{}, fmt.Errorf("installed memory object %s has no resident range", run.Object)
		}
		for i := uint16(0); i < run.Pages; i++ {
			if err := relocation.Add(run.ResourceID, run.OldOffset+uint64(i)*hostarch.PageSize, resident.Start+uint64(run.FirstSlot+uint8(i))*hostarch.PageSize); err != nil {
				return InstallResult{}, err
			}
		}
	}
	objectIDs := make([]ID, 0, len(newRanges))
	for object := range newRanges {
		objectIDs = append(objectIDs, ID(object))
	}
	sort.Slice(objectIDs, func(i, j int) bool { return objectIDs[i].String() < objectIDs[j].String() })
	result := InstallResult{Relocation: relocation, Chunks: make([]InstalledChunk, len(objectIDs)), transaction: transaction}
	for i, object := range objectIDs {
		result.Chunks[i] = InstalledChunk{Object: object, Target: newRanges[pgalloc.SparseChunkObjectID(object)], StreamOffset: uint64(i) * pgalloc.SparseChunkBytes}
	}
	committed = true
	return result, nil
}

// ChunkReader exposes decoded chunks as a dense virtual stream of 1 MiB
// records. Each ReadAt is transactional: it verifies the immutable object,
// builds the complete record in private memory, then copies to the caller.
type ChunkReader struct {
	store   *Store
	objects []ID
	mu      sync.RWMutex
	closed  bool
}

// NewChunkReader creates a virtual stream in installed-chunk order.
func NewChunkReader(repository *MemoryRepository, installed []InstalledChunk) (*ChunkReader, error) {
	if repository == nil {
		return nil, fmt.Errorf("nil TTC memory repository")
	}
	objects := make([]ID, len(installed))
	for i, chunk := range installed {
		if chunk.StreamOffset != uint64(i)*pgalloc.SparseChunkBytes || chunk.Target.Length() != pgalloc.SparseChunkBytes {
			return nil, fmt.Errorf("invalid installed chunk %d: %+v", i, chunk)
		}
		objects[i] = chunk.Object
	}
	return &ChunkReader{store: repository.chunks, objects: objects}, nil
}

// Close prevents future reads.
func (r *ChunkReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *ChunkReader) decoded(index int) ([]byte, error) {
	chunk, err := r.store.Get(r.objects[index])
	if err != nil {
		return nil, err
	}
	data := make([]byte, pgalloc.SparseChunkBytes)
	for _, page := range chunk.Pages {
		if !page.Zero {
			copy(data[uint64(page.Slot)*hostarch.PageSize:], page.Data)
		}
	}
	return data, nil
}

// ReadAt implements io.ReaderAt.
func (r *ChunkReader) ReadAt(dst []byte, off int64) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return 0, fmt.Errorf("TTC memory chunk reader is closed")
	}
	if off < 0 {
		return 0, fmt.Errorf("negative TTC memory chunk offset")
	}
	total := uint64(len(r.objects)) * pgalloc.SparseChunkBytes
	if uint64(off) >= total {
		return 0, io.EOF
	}
	written := 0
	for len(dst) != 0 && uint64(off) < total {
		index := int(uint64(off) / pgalloc.SparseChunkBytes)
		within := uint64(off) % pgalloc.SparseChunkBytes
		decoded, err := r.decoded(index)
		if err != nil {
			return written, err
		}
		n := copy(dst, decoded[within:])
		written += n
		dst = dst[n:]
		off += int64(n)
	}
	if len(dst) != 0 {
		return written, io.EOF
	}
	return written, nil
}

// AsyncReader wraps r with bounded parallel synchronous host reads. One
// demand fault reads exactly one 1 MiB unloaded segment.
func (r *ChunkReader) AsyncReader(parallel int) stateio.AsyncReader {
	if parallel <= 0 {
		parallel = 1
	}
	return stateio.NewIOReader(r, pgalloc.SparseChunkBytes, 16, parallel)
}
