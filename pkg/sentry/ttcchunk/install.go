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
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// InstalledChunk maps one newly allocated resident chunk to a virtual stream
// offset in ChunkReader. Reused resident chunks are omitted.
type InstalledChunk struct {
	Object       ID
	ResourceID   checkpoint.ResourceID
	MemoryFile   *pgalloc.MemoryFile
	Target       memmap.FileRange
	StreamOffset uint64
}

// InstallSetResult is the process-local product of installing one manifest
// across its application and private MemoryFiles.
type InstallSetResult struct {
	Relocation   *pgalloc.SparseRelocation
	Chunks       []InstalledChunk
	Objects      []ID
	transactions []*pgalloc.SparseChunkInstallation
	done         bool
}

// ResidentObjects returns manifest objects that were already resident before
// this installation and therefore need no backing read for the restored graph.
func (r *InstallSetResult) ResidentObjects() []ID {
	if r == nil {
		return nil
	}
	return residentObjects(r.Objects, r.Chunks)
}

// Commit retains every installed residency unit.
func (r *InstallSetResult) Commit() error {
	if r == nil || r.done {
		return fmt.Errorf("sparse memory-file installation is unavailable")
	}
	r.done = true
	for _, transaction := range r.transactions {
		if err := transaction.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Abort releases all newly installed residency units. It is idempotent.
func (r *InstallSetResult) Abort() error {
	if r == nil || r.done {
		return nil
	}
	r.done = true
	var result error
	for _, transaction := range r.transactions {
		if err := transaction.Abort(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// InstallResult is the process-local product of installing a manifest.
type InstallResult struct {
	Relocation  *pgalloc.SparseRelocation
	Chunks      []InstalledChunk
	Objects     []ID
	transaction *pgalloc.SparseChunkInstallation
}

// ResidentObjects is the single-MemoryFile form of InstallSetResult.ResidentObjects.
func (r *InstallResult) ResidentObjects() []ID {
	if r == nil {
		return nil
	}
	return residentObjects(r.Objects, r.Chunks)
}

func residentObjects(objects []ID, chunks []InstalledChunk) []ID {
	missing := make(map[ID]struct{}, len(chunks))
	for _, chunk := range chunks {
		missing[chunk.Object] = struct{}{}
	}
	result := make([]ID, 0, len(objects)-len(missing))
	for _, object := range objects {
		if _, ok := missing[object]; !ok {
			result = append(result, object)
		}
	}
	return result
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
	for _, run := range manifest.Runs {
		if run.ResourceID != mf.ResourceID() {
			return InstallResult{}, fmt.Errorf("manifest resource %s does not match destination MemoryFile %s", run.ResourceID, mf.ResourceID())
		}
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
	objectIDs := make([]ID, 0, len(objects))
	firstOldOffset := make(map[ID]uint64, len(objects))
	for object := range objects {
		objectIDs = append(objectIDs, object)
		firstOldOffset[object] = ^uint64(0)
	}
	for _, run := range manifest.Runs {
		if run.OldOffset < firstOldOffset[run.Object] {
			firstOldOffset[run.Object] = run.OldOffset
		}
	}
	sort.Slice(objectIDs, func(i, j int) bool {
		if firstOldOffset[objectIDs[i]] != firstOldOffset[objectIDs[j]] {
			return firstOldOffset[objectIDs[i]] < firstOldOffset[objectIDs[j]]
		}
		return objectIDs[i].String() < objectIDs[j].String()
	})
	installs := make([]pgalloc.SparseChunkInstall, 0, len(objects))
	for _, object := range objectIDs {
		chunk := objects[object]
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
	objectIDs = make([]ID, 0, len(newRanges))
	for object := range newRanges {
		objectIDs = append(objectIDs, ID(object))
	}
	sort.Slice(objectIDs, func(i, j int) bool { return objectIDs[i].String() < objectIDs[j].String() })
	allObjects := make([]ID, 0, len(objects))
	for object := range objects {
		allObjects = append(allObjects, object)
	}
	sort.Slice(allObjects, func(i, j int) bool { return allObjects[i].String() < allObjects[j].String() })
	result := InstallResult{Relocation: relocation, Chunks: make([]InstalledChunk, len(objectIDs)), Objects: allObjects, transaction: transaction}
	for i, object := range objectIDs {
		result.Chunks[i] = InstalledChunk{
			Object: object, ResourceID: mf.ResourceID(), MemoryFile: mf,
			Target: newRanges[pgalloc.SparseChunkObjectID(object)], StreamOffset: uint64(i) * pgalloc.SparseChunkBytes,
		}
	}
	committed = true
	return result, nil
}

// InstallManifestSet installs manifest into the MemoryFile selected by each
// run's ResourceID. One immutable object may belong to only one MemoryFile.
func (r *MemoryRepository) InstallManifestSet(memoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32) (InstallSetResult, error) {
	if len(memoryFiles) == 0 {
		return InstallSetResult{}, errors.New("sparse manifest destination set is empty")
	}
	runsByResource := make(map[checkpoint.ResourceID][]ManifestRun)
	objectResources := make(map[ID]checkpoint.ResourceID)
	for _, run := range manifest.Runs {
		mf, ok := memoryFiles[run.ResourceID]
		if !ok || mf == nil {
			return InstallSetResult{}, fmt.Errorf("manifest has no destination MemoryFile for %s", run.ResourceID)
		}
		if mf.ResourceID() != run.ResourceID {
			return InstallSetResult{}, fmt.Errorf("destination MemoryFile %s does not match manifest resource %s", mf.ResourceID(), run.ResourceID)
		}
		if previous, ok := objectResources[run.Object]; ok && previous != run.ResourceID {
			return InstallSetResult{}, fmt.Errorf("memory object %s spans resources %s and %s", run.Object, previous, run.ResourceID)
		}
		objectResources[run.Object] = run.ResourceID
		runsByResource[run.ResourceID] = append(runsByResource[run.ResourceID], run)
	}
	resources := make([]checkpoint.ResourceID, 0, len(runsByResource))
	for resource := range runsByResource {
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].String() < resources[j].String() })
	result := InstallSetResult{Relocation: pgalloc.NewSparseRelocation()}
	committed := false
	defer func() {
		if !committed {
			_ = result.Abort()
		}
	}()
	for _, resource := range resources {
		submanifest := Manifest{Runs: runsByResource[resource]}
		installed, err := r.InstallManifest(memoryFiles[resource], submanifest, kind, memCgID)
		if err != nil {
			return InstallSetResult{}, err
		}
		result.transactions = append(result.transactions, installed.transaction)
		result.Chunks = append(result.Chunks, installed.Chunks...)
		result.Objects = append(result.Objects, installed.Objects...)
		for _, run := range submanifest.Runs {
			for i := uint16(0); i < run.Pages; i++ {
				old := run.OldOffset + uint64(i)*hostarch.PageSize
				target, ok := installed.Relocation.Page(resource, old)
				if !ok {
					return InstallSetResult{}, fmt.Errorf("submanifest relocation omitted %s:%#x", resource, old)
				}
				if err := result.Relocation.Add(resource, old, target); err != nil {
					return InstallSetResult{}, err
				}
			}
		}
	}
	for i := range result.Chunks {
		result.Chunks[i].StreamOffset = uint64(i) * pgalloc.SparseChunkBytes
	}
	sort.Slice(result.Objects, func(i, j int) bool { return result.Objects[i].String() < result.Objects[j].String() })
	committed = true
	return result, nil
}

// ChunkReader exposes decoded chunks as a dense virtual stream of 1 MiB
// records. Each ReadAt is transactional: it verifies the immutable object,
// builds the complete record in private memory, then copies to the caller.
type ChunkReader struct {
	store       *Store
	objects     []ID
	mu          sync.RWMutex
	closed      bool
	ownStore    bool
	consumption *ConsumptionTracker
}

// NewChunkReader creates a virtual stream in installed-chunk order.
func NewChunkReader(repository *MemoryRepository, installed []InstalledChunk) (*ChunkReader, error) {
	return newChunkReader(repository, installed, false)
}

// NewOwnedChunkReader is NewChunkReader but closes repository when the reader
// closes. It is used for a repository directory donated to an isolated Sentry.
func NewOwnedChunkReader(repository *MemoryRepository, installed []InstalledChunk) (*ChunkReader, error) {
	return newChunkReader(repository, installed, true)
}

// NewOwnedConsumingChunkReader creates a reader that records successfully
// installed objects against branch after ActivateConsumption. Until then,
// completions are buffered so a failed restore leaves its durable image intact.
func NewOwnedConsumingChunkReader(repository *MemoryRepository, installed []InstalledChunk, branch string, manifest ManifestID) (*ChunkReader, error) {
	reader, err := newChunkReader(repository, installed, true)
	if err != nil {
		return nil, err
	}
	tracker, err := newConsumptionTracker(repository, branch, manifest, reader.objects)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	reader.consumption = tracker
	return reader, nil
}

// ActivateConsumption commits buffered completions and marks already-resident
// objects consumed. It must be called only after restored graph references are
// committed.
func (r *ChunkReader) ActivateConsumption(alreadyResident []ID) {
	if r != nil && r.consumption != nil {
		r.consumption.Activate(alreadyResident)
	}
}

// AbortConsumption prevents buffered completions from changing durable roots.
func (r *ChunkReader) AbortConsumption() {
	if r != nil && r.consumption != nil {
		r.consumption.Abort()
	}
}

// RangeLoaded records a pages-file range after atomic MemoryFile installation.
func (r *ChunkReader) RangeLoaded(offset, length uint64) {
	if r != nil && r.consumption != nil {
		r.consumption.Record(offset, length)
	}
}

func newChunkReader(repository *MemoryRepository, installed []InstalledChunk, ownStore bool) (*ChunkReader, error) {
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
	return &ChunkReader{store: repository.chunks, objects: objects, ownStore: ownStore}, nil
}

// Close prevents future reads.
func (r *ChunkReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	var err error
	if r.ownStore {
		if r.consumption != nil {
			err = r.consumption.ReaderClosed()
		} else {
			err = r.store.closeRepository()
		}
	}
	r.mu.Unlock()
	return err
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
	readLock, err := r.store.repository.lockGarbageCollection(unix.LOCK_SH)
	if err != nil {
		return 0, fmt.Errorf("locking TTC memory chunks for reading: %w", err)
	}
	defer unlockGarbageCollection(readLock)
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
