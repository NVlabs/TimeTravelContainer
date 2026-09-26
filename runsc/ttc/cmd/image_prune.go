// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sync"

	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// pruneRetainedImage drops from the image a rehydrated branch retains for its
// next dehydration the chunks its family uses no more. The Sentry frees a
// rehydrated chunk once every page of it has been rewritten or freed by every
// branch, and no dehydration will share any page of it; a chunk with a page
// still in use stays whole. The image moves to a new origin, whose tables are
// the old ones wherever nothing was dropped. Only a standalone daemon's
// images, which no other node holds, are pruned.
//
// Freeing chunks also frees the memory they held, and lets a rehydration's
// loader, which loads only what is touched, finish once what is left is
// loaded: it runs whether or not the branch's memory is all loaded.
func (b *runtimeBackend) pruneRetainedImage(containerID string, branch ttcbundle.BranchInfo) error {
	if b.nodeID != "" || b.snapshotGraph == nil || branch.DistributedGraphReferenceOperation != "" || branch.SnapshotID == 0 ||
		len(branch.MemoryRoots) == 0 || len(branch.MemoryRoots) != len(branch.MemoryRootObjects) {
		return nil
	}
	output, err := b.command("timetravel", containerID, "reclaim-image-chunks", branch.ID)
	if err != nil {
		return err
	}
	var reclaimed []string
	if err := json.Unmarshal(output, &reclaimed); err != nil {
		return fmt.Errorf("decoding the chunks the Sentry freed: %w", err)
	}
	stale := make(map[ttcsnapshot.ObjectID]bool, len(reclaimed))
	for _, encoded := range reclaimed {
		var id ttcchunk.ID
		decoded, err := hex.DecodeString(encoded)
		if err != nil || len(decoded) != len(id) {
			return fmt.Errorf("the Sentry freed chunk %q", encoded)
		}
		copy(id[:], decoded)
		// Chunks the Sentry received otherwise are none of the image's.
		if chunk, err := ttcchunk.GraphChunkObject(id); err == nil {
			stale[chunk] = true
		}
	}
	// The Sentry reports every chunk it freed until one is installed again:
	// walk the image only when it has freed more since the last walk.
	key := fmt.Sprintf("%s@%s/%d", containerID, branch.ID, branch.SnapshotID)
	if len(stale) == 0 || !prunedImages.walk(key, len(stale)) {
		return nil
	}
	kept := make([][]ttcsnapshot.MemoryPageEntry, len(branch.MemoryRoots))
	dropped := false
	for i := range branch.MemoryRoots {
		root, err := ttcsnapshot.ParseObjectID(branch.MemoryRootObjects[i])
		if err != nil {
			return err
		}
		entries, err := b.snapshotGraph.MemoryPageEntries(root)
		if err != nil {
			return err
		}
		keep := entries[:0]
		for _, entry := range entries {
			if !stale[entry.Location.Chunk] {
				keep = append(keep, entry)
			}
		}
		dropped = dropped || len(keep) != len(entries)
		kept[i] = keep
	}
	if !dropped {
		return nil
	}
	origin, sealState, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return err
	}
	// Each root keeps its name within the origin's; a root left with
	// nothing goes.
	prefix := fmt.Sprintf("memory/%016x", uint64(origin))
	var names []string
	var tables []ttcsnapshot.NewMemoryTable
	for i, entries := range kept {
		if len(entries) == 0 {
			continue
		}
		name := prefix + "/" + path.Base(branch.MemoryRoots[i])
		names = append(names, name)
		for begin := 0; begin < len(entries); {
			index := entries[begin].Index / ttcsnapshot.PagesPerMemoryTable * ttcsnapshot.PagesPerMemoryTable
			table := ttcsnapshot.NewMemoryTable{Root: name, Index: index}
			for ; begin < len(entries) && entries[begin].Index/ttcsnapshot.PagesPerMemoryTable*ttcsnapshot.PagesPerMemoryTable == index; begin++ {
				entry := entries[begin]
				table.Entries = append(table.Entries, ttcsnapshot.MemoryTableEntry{
					Slot: uint8(entry.Index % ttcsnapshot.PagesPerMemoryTable), Location: entry.Location, Identity: entry.Identity,
				})
			}
			tables = append(tables, table)
		}
	}
	store := b.containerStore()
	if len(names) == 0 {
		// Nothing the image holds is of use. Its control state goes once the
		// branch has loaded what it touched.
		if branch.ControlRoot != "" {
			prunedImages.forget(key)
			return nil
		}
		cleared, err := store.ClearBranchMemoryGraphIf(containerID, branch.ID, branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects, "")
		if err != nil || !cleared {
			return err
		}
		return b.releaseGraphMemoryRoots(branch.MemoryRoots)
	}
	states := []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotWorking, ttcsnapshot.SnapshotSealedLocal}
	for begin := 0; begin < len(tables); begin += graphTablesPerPruneTransaction {
		end := min(begin+graphTablesPerPruneTransaction, len(tables))
		if err := b.snapshotGraph.CommitMemoryTables(origin, tables[begin:end], nil, states...); err != nil {
			_ = b.releaseGraphMemoryRoots(names)
			return fmt.Errorf("writing the pruned image: %w", err)
		}
	}
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		_ = b.releaseGraphMemoryRoots(names)
		return fmt.Errorf("sealing the pruned image: %w", err)
	}
	objects := make([]string, len(names))
	for i, name := range names {
		root, ok := b.snapshotGraph.Root(name)
		if !ok {
			_ = b.releaseGraphMemoryRoots(names)
			return fmt.Errorf("pruned image root %q was not created", name)
		}
		objects[i] = root.String()
	}
	replaced, err := store.ReplaceBranchMemoryGraphIf(containerID, branch.ID, branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects, uint64(origin), names, objects)
	if err != nil || !replaced {
		// A dehydration superseded the image meanwhile.
		return b.releaseGraphMemoryRoots(names)
	}
	prunedImages.forget(key)
	return b.releaseGraphMemoryRoots(branch.MemoryRoots)
}

// prunedImages has, by branch image, how many freed chunks the Sentry had
// reported when the image was last walked. It forgets them all when it holds
// maxPrunedImages, as it holds those of removed branches too: a forgotten
// image is walked once more.
var prunedImages prunedImageCounts

const maxPrunedImages = 1 << 14

type prunedImageCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

// walk reports whether the image key is to be walked, with this many freed
// chunks reported, and records that it is.
func (p *prunedImageCounts) walk(key string, freed int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.counts[key]; ok && previous == freed {
		return false
	}
	if p.counts == nil || len(p.counts) >= maxPrunedImages {
		p.counts = make(map[string]int)
	}
	p.counts[key] = freed
	return true
}

func (p *prunedImageCounts) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.counts, key)
}

// graphTablesPerPruneTransaction bounds the tables one transaction of a prune
// maps, as graphTablesPerTransaction bounds an import's: 128 MiB of memory.
const graphTablesPerPruneTransaction = 128
