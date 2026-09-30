// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
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

// TakePageLoads hands the caller the loaders of the pages still to be loaded
// in the residency units the installation reused, each with an owner
// reference of the caller's: the restored owners read those pages too. Commit
// and Abort release those not taken.
func (r *InstallSetResult) TakePageLoads() []*pgalloc.AsyncPagesFileLoad {
	if r == nil {
		return nil
	}
	var loads []*pgalloc.AsyncPagesFileLoad
	for _, transaction := range r.transactions {
		loads = append(loads, transaction.TakePageLoads()...)
	}
	return loads
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

// TakePageLoads is the single-MemoryFile form of InstallSetResult.TakePageLoads.
func (r *InstallResult) TakePageLoads() []*pgalloc.AsyncPagesFileLoad {
	return r.transaction.TakePageLoads()
}

// Abort rolls back an incomplete restore.
func (r *InstallResult) Abort() error { return r.transaction.Abort() }

// InstallManifest installs an already-loaded immutable graph manifest.
func InstallManifest(mf *pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32) (InstallResult, error) {
	return installManifest(mf, manifest, kind, memCgID, pgalloc.NewSparseRelocation())
}

// compareIDs orders IDs as their String forms are ordered.
func compareIDs(a, b ID) int { return bytes.Compare(a[:], b[:]) }

// installManifest is InstallManifest, recording the relocation of the
// manifest's pages in relocation.
func installManifest(mf *pgalloc.MemoryFile, manifest Manifest, kind usage.MemoryKind, memCgID uint32, relocation *pgalloc.SparseRelocation) (InstallResult, error) {
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
	// An object may hold pages of several logical chunks, packed. Objects
	// are installed in the order of their first pages.
	type objectInstall struct {
		firstOldOffset uint64
		install        pgalloc.SparseChunkInstall
	}
	objects := make(map[ID]int)
	var ordered []objectInstall
	for _, run := range manifest.Runs {
		at, ok := objects[run.Object]
		if !ok {
			at = len(ordered)
			objects[run.Object] = at
			ordered = append(ordered, objectInstall{firstOldOffset: run.OldOffset, install: pgalloc.SparseChunkInstall{
				Object: pgalloc.SparseChunkObjectID(run.Object), Kind: kind, MemCgID: memCgID,
			}})
		}
		object := &ordered[at]
		object.firstOldOffset = min(object.firstOldOffset, run.OldOffset)
		object.install.Runs = append(object.install.Runs, pgalloc.SparseChunkRun{
			Position: run.ObjectPage, Pages: run.Pages, Page: run.FirstPage, Version: run.FirstVersion,
		})
	}
	slices.SortFunc(ordered, func(a, b objectInstall) int {
		if c := cmp.Compare(a.firstOldOffset, b.firstOldOffset); c != 0 {
			return c
		}
		return compareIDs(ID(a.install.Object), ID(b.install.Object))
	})
	installs := make([]pgalloc.SparseChunkInstall, len(ordered))
	for i := range ordered {
		installs[i] = ordered[i].install
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
	for _, run := range manifest.Runs {
		resident, ok := mf.SparseChunkRange(pgalloc.SparseChunkObjectID(run.Object))
		if !ok {
			return InstallResult{}, fmt.Errorf("installed memory object %s has no resident range", run.Object)
		}
		old := memmap.FileRange{Start: run.OldOffset, End: run.OldOffset + uint64(run.Pages)*hostarch.PageSize}
		if err := relocation.AddRange(run.ResourceID, old, resident.Start+uint64(run.ObjectPage)*hostarch.PageSize); err != nil {
			return InstallResult{}, err
		}
	}
	objectIDs := make([]ID, 0, len(newRanges))
	for object := range newRanges {
		objectIDs = append(objectIDs, ID(object))
	}
	slices.SortFunc(objectIDs, compareIDs)
	allObjects := make([]ID, 0, len(objects))
	for object := range objects {
		allObjects = append(allObjects, object)
	}
	slices.SortFunc(allObjects, compareIDs)
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
		installed, err := installManifest(memoryFiles[resource], submanifest, kind, memCgID, result.Relocation)
		if err != nil {
			return InstallSetResult{}, err
		}
		result.transactions = append(result.transactions, installed.transaction)
		result.Chunks = append(result.Chunks, installed.Chunks...)
		result.Objects = append(result.Objects, installed.Objects...)
	}
	for i := range result.Chunks {
		result.Chunks[i].StreamOffset = uint64(i) * pgalloc.SparseChunkBytes
	}
	slices.SortFunc(result.Objects, compareIDs)
	committed = true
	return result, nil
}
