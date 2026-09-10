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
	"bytes"
	"errors"
	"fmt"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// GraphMemoryRoot identifies one MemoryFile radix root in a graph-native
// dehydration snapshot. ResourceID is empty for application memory.
type GraphMemoryRoot struct {
	ResourceID checkpoint.ResourceID
	Name       string
	Root       ttcsnapshot.ObjectID
}

// GraphImportResult describes one graph-native import of a legacy capture
// stream. The compatibility input is consumed chunk-by-chunk; all new bulk
// data and liveness are represented by typed graph objects.
type GraphImportResult struct {
	Snapshot ttcsnapshot.SnapshotID
	Roots    []GraphMemoryRoot
	Chunks   uint64
	Pages    uint64
}

type graphResourcePages struct {
	id    checkpoint.ResourceID
	pages map[uint64]ttcsnapshot.ObjectID
}

func resourceKey(id checkpoint.ResourceID) string {
	return id.ContainerName + "\x00" + id.Path
}

// ImportSnapshotGraph converts manifest into fixed-size graph memory chunks.
// It never installs a partially-populated branch root: on failure every root
// created under rootPrefix is released and unreferenced bulk objects are
// reclaimed. origin remains allocated (snapshot IDs are never reused).
func (r *MemoryRepository) ImportSnapshotGraph(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifestID ManifestID, rootPrefix string) (_ GraphImportResult, retErr error) {
	if r == nil || graph == nil || data == nil || origin == 0 || rootPrefix == "" {
		return GraphImportResult{}, errors.New("invalid TTC memory graph import")
	}
	manifest, err := r.LoadManifest(manifestID)
	if err != nil {
		return GraphImportResult{}, err
	}
	inspection := graph.Inspect()
	for name := range inspection.Roots {
		if name == rootPrefix || len(name) > len(rootPrefix) && name[:len(rootPrefix)+1] == rootPrefix+"/" {
			return GraphImportResult{}, fmt.Errorf("memory graph root prefix %q is already in use", rootPrefix)
		}
	}

	resources := make(map[string]*graphResourcePages)
	type objectUse struct {
		id   ID
		runs []ManifestRun
	}
	usesByID := make(map[ID][]ManifestRun)
	for _, run := range manifest.Runs {
		usesByID[run.Object] = append(usesByID[run.Object], run)
		key := resourceKey(run.ResourceID)
		if resources[key] == nil {
			resources[key] = &graphResourcePages{id: run.ResourceID, pages: make(map[uint64]ttcsnapshot.ObjectID)}
		}
	}
	resourceKeys := sortedResourceKeys(resources)
	resourceIndexes := make(map[string]int, len(resourceKeys))
	for index, key := range resourceKeys {
		resourceIndexes[key] = index
	}
	uses := make([]objectUse, 0, len(usesByID))
	for id, runs := range usesByID {
		uses = append(uses, objectUse{id: id, runs: runs})
	}
	sort.Slice(uses, func(i, j int) bool { return bytes.Compare(uses[i].id[:], uses[j].id[:]) < 0 })

	createdRoots := make([]string, 0, len(resources))
	var activePins []string
	defer func() {
		if retErr == nil {
			return
		}
		if len(activePins) != 0 {
			_ = graph.DeletePins(activePins)
		}
		for _, name := range createdRoots {
			_ = graph.DeleteRoot(name)
		}
		for {
			collected, err := graph.Collect(4096)
			if err != nil || collected == 0 {
				break
			}
		}
		_, _, _ = data.ReclaimUnreferenced(graph)
	}()

	for objectIndex, use := range uses {
		chunk, err := r.chunks.Get(use.id)
		if err != nil {
			return GraphImportResult{}, fmt.Errorf("reading memory chunk %s: %w", use.id, err)
		}
		pages := make([]ttcsnapshot.MemoryPageDescriptor, 0, len(chunk.Pages))
		raw := make([]byte, ttcsnapshot.MemoryChunkBytes)
		for _, page := range chunk.Pages {
			logicalSequence := chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)
			pages = append(pages, ttcsnapshot.MemoryPageDescriptor{
				Slot: page.Slot,
				Identity: ttcsnapshot.MemoryPageIdentity{
					LogicalAllocator: [16]byte(chunk.Logical.Allocator),
					LogicalSequence:  logicalSequence,
					VersionAllocator: [16]byte(page.Version.Allocator),
					VersionSequence:  page.Version.Sequence,
				},
			})
			if !page.Zero {
				copy(raw[int(page.Slot)*hostarch.PageSize:], page.Data)
			}
		}
		pinPrefix := fmt.Sprintf("%s/construction/%08x", rootPrefix, objectIndex)
		reservation, err := graph.ReserveMemoryChunkPages(origin, pinPrefix, pages)
		if err != nil {
			return GraphImportResult{}, err
		}
		chunkID := ttcsnapshot.ObjectID(reservation.Chunk)
		if err := data.Install(chunkID, bytes.NewReader(raw), int64(len(raw))); err != nil {
			return GraphImportResult{}, fmt.Errorf("installing graph memory chunk %s: %w", chunkID, err)
		}
		activePins = reservation.Pins
		if err := graph.CommitMemoryChunk(reservation); err != nil {
			return GraphImportResult{}, fmt.Errorf("committing graph memory chunk %s: %w", chunkID, err)
		}
		rootPages := make(map[string]map[uint64]ttcsnapshot.ObjectID)
		for _, run := range use.runs {
			key := resourceKey(run.ResourceID)
			if rootPages[key] == nil {
				rootPages[key] = make(map[uint64]ttcsnapshot.ObjectID)
			}
			for delta := uint16(0); delta < run.Pages; delta++ {
				slot := uint8(uint16(run.FirstSlot) + delta)
				page, ok := reservation.Pages[slot]
				if !ok {
					return GraphImportResult{}, fmt.Errorf("manifest references missing slot %d in chunk %s", slot, use.id)
				}
				index := run.OldOffset/hostarch.PageSize + uint64(delta)
				if previous, exists := resources[key].pages[index]; exists && previous != ttcsnapshot.ObjectID(page) {
					return GraphImportResult{}, fmt.Errorf("memory resource maps page index %d twice", index)
				}
				resources[key].pages[index] = ttcsnapshot.ObjectID(page)
				rootPages[key][index] = ttcsnapshot.ObjectID(page)
			}
		}
		keys := make([]string, 0, len(rootPages))
		for key := range rootPages {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			resource := resources[key]
			if resource == nil {
				return GraphImportResult{}, errors.New("memory graph resource disappeared")
			}
			if resource.pages == nil {
				resource.pages = make(map[uint64]ttcsnapshot.ObjectID)
			}
			name := fmt.Sprintf("%s/resource-%04x", rootPrefix, resourceIndexes[key])
			if _, exists := graph.Root(name); !exists {
				createdRoots = append(createdRoots, name)
			}
			if _, err := graph.SetMemoryPages(origin, name, rootPages[key]); err != nil {
				return GraphImportResult{}, err
			}
		}
		if err := graph.DeletePins(reservation.Pins); err != nil {
			return GraphImportResult{}, fmt.Errorf("releasing memory construction pins: %w", err)
		}
		activePins = nil
	}

	result := GraphImportResult{Snapshot: origin, Chunks: uint64(len(uses))}
	for index, key := range resourceKeys {
		name := fmt.Sprintf("%s/resource-%04x", rootPrefix, index)
		root, ok := graph.Root(name)
		if !ok {
			return GraphImportResult{}, fmt.Errorf("memory resource root %q was not created", name)
		}
		resource := resources[key]
		result.Roots = append(result.Roots, GraphMemoryRoot{ResourceID: resource.id, Name: name, Root: root})
		result.Pages += uint64(len(resource.pages))
	}
	for {
		collected, err := graph.Collect(4096)
		if err != nil {
			return GraphImportResult{}, err
		}
		if collected == 0 {
			break
		}
	}
	if _, _, err := data.ReclaimUnreferenced(graph); err != nil {
		return GraphImportResult{}, err
	}
	return result, nil
}

func sortedResourceKeys(resources map[string]*graphResourcePages) []string {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
