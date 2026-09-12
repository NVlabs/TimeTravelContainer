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
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// InstalledChunk maps one newly allocated resident chunk to a virtual stream
// offset in a graph chunk reader. Reused resident chunks are omitted.
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

// InstallManifest installs an already-loaded immutable graph manifest.
func InstallManifest(mf *pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32) (InstallResult, error) {
	if mf == nil {
		return InstallResult{}, fmt.Errorf("installing manifest into nil MemoryFile")
	}
	for _, run := range manifest.Runs {
		if run.ResourceID != mf.ResourceID() {
			return InstallResult{}, fmt.Errorf("manifest resource %s does not match destination MemoryFile %s", run.ResourceID, mf.ResourceID())
		}
	}
	// The manifest contains all identity metadata
	// required by InstallSparseChunks. Do not open object payloads here: for a
	// cold remote restore that would eagerly fetch every 1 MiB chunk before the
	// first guest instruction. Page contents are loaded by ChunkReader when the
	// corresponding resident range first faults in.
	objects := make(map[ID]pgalloc.LogicalChunkID)
	for _, run := range manifest.Runs {
		if logical, ok := objects[run.Object]; ok {
			if logical != run.Logical {
				return InstallResult{}, fmt.Errorf("manifest object %s has conflicting logical coordinates %+v and %+v", run.Object, logical, run.Logical)
			}
		} else {
			objects[run.Object] = run.Logical
		}
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
		install := pgalloc.SparseChunkInstall{
			Object: pgalloc.SparseChunkObjectID(object), Logical: objects[object],
			Kind: kind, MemCgID: memCgID,
		}
		installedPages := make(map[uint8]pgalloc.SparseChunkPage)
		for _, run := range manifest.Runs {
			if run.Object != object {
				continue
			}
			for i := uint16(0); i < run.Pages; i++ {
				slot := run.FirstSlot + uint8(i)
				version := pgalloc.PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + uint64(i)}
				page := pgalloc.SparseChunkPage{
					Slot:    slot,
					Page:    pgalloc.LogicalPageID{Allocator: run.FirstPage.Allocator, Sequence: run.FirstPage.Sequence + uint64(i)},
					Version: version, References: run.References,
				}
				if previous, ok := installedPages[slot]; ok {
					if previous != page {
						return InstallResult{}, fmt.Errorf("manifest object %s slot %d has conflicting page identities", object, slot)
					}
					continue
				}
				installedPages[slot] = page
				install.Pages = append(install.Pages, page)
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

// InstallManifestSet installs an already-loaded manifest into its selected
// MemoryFiles without requiring a pathname-backed repository.
func InstallManifestSet(memoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32) (InstallSetResult, error) {
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
		installed, err := InstallManifest(memoryFiles[resource], submanifest, kind, memCgID)
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
