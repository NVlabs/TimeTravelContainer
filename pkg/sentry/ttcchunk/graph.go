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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// GraphMemoryRoot identifies one MemoryFile radix root in a graph-native
// dehydration snapshot. ResourceID is empty for application memory.
type GraphMemoryRoot struct {
	ResourceID checkpoint.ResourceID
	Name       string
	Root       ttcsnapshot.ObjectID
}

// GraphImportResult describes one graph-native capture-stream import. The
// input is consumed chunk-by-chunk; all bulk data and liveness are represented
// by typed graph objects.
type GraphImportResult struct {
	Snapshot ttcsnapshot.SnapshotID
	Roots    []GraphMemoryRoot
	Chunks   uint64
	Pages    uint64
}

// GraphManifestResult adapts graph-native roots to the existing in-Sentry
// sparse installer while that installer is being generalized. Chunks maps the
// opaque process-local residency identity to the authoritative graph object;
// it is not a content checksum or a second persistent manifest.
type GraphManifestResult struct {
	Manifest Manifest
	Chunks   map[ID]ttcsnapshot.ObjectID
}

type graphResourcePages struct {
	id    checkpoint.ResourceID
	pages map[uint64]ttcsnapshot.ObjectID
}

func resourceKey(id checkpoint.ResourceID) string {
	return id.ContainerName + "\x00" + id.Path
}

// importSnapshotGraph installs manifest and its immutable source chunks into
// graph. getChunk lets callers stream a capture directly from the Sentry.
func importSnapshotGraph(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, getChunk func(ID) (Chunk, error)) (_ GraphImportResult, retErr error) {
	return importSnapshotGraphWithReusableStates(graph, data, origin, manifest, rootPrefix, getChunk, nil)
}

func importSnapshotGraphWithReusableStates(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, getChunk func(ID) (Chunk, error), reusableStates []ttcsnapshot.SnapshotState) (_ GraphImportResult, retErr error) {
	if graph == nil || data == nil || origin == 0 || rootPrefix == "" || getChunk == nil {
		return GraphImportResult{}, errors.New("invalid TTC memory graph import")
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

	// Path-copy a bounded group of logical chunks at once. Calling
	// SetMemoryPages once per 1-MiB chunk rewrites the same upper radix path for
	// every chunk and journals all of those immediately-obsolete roots. A
	// 32-chunk group keeps the transaction bounded (at most 8192 distinct page
	// leaves) while amortizing the upper levels across the group.
	const radixChunksPerTransaction = 32
	pendingPages := make(map[string]map[uint64]ttcsnapshot.ObjectID)
	pendingUses := 0
	flushPages := func() error {
		if pendingUses == 0 {
			return nil
		}
		keys := make([]string, 0, len(pendingPages))
		for key := range pendingPages {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			name := fmt.Sprintf("%s/resource-%04x", rootPrefix, resourceIndexes[key])
			if _, exists := graph.Root(name); !exists {
				createdRoots = append(createdRoots, name)
			}
			if _, err := graph.SetMemoryPages(origin, name, pendingPages[key]); err != nil {
				return err
			}
		}
		if len(activePins) != 0 {
			if err := graph.DeletePins(activePins); err != nil {
				return fmt.Errorf("releasing memory construction pins: %w", err)
			}
		}
		activePins = nil
		pendingPages = make(map[string]map[uint64]ttcsnapshot.ObjectID)
		pendingUses = 0
		return nil
	}

	for objectIndex, use := range uses {
		chunk, err := getChunk(use.id)
		if err != nil {
			return GraphImportResult{}, fmt.Errorf("reading memory chunk %s: %w", use.id, err)
		}
		for _, run := range use.runs {
			if run.Logical != chunk.Logical {
				return GraphImportResult{}, fmt.Errorf("memory chunk %s has logical coordinate %+v, manifest references %+v", use.id, chunk.Logical, run.Logical)
			}
		}
		references := make(map[uint8]uint64)
		for _, run := range use.runs {
			for delta := uint16(0); delta < run.Pages; delta++ {
				slot := uint8(uint16(run.FirstSlot) + delta)
				if previous, ok := references[slot]; ok && previous != run.References {
					return GraphImportResult{}, fmt.Errorf("chunk %s slot %d has conflicting reference counts %d and %d", use.id, slot, previous, run.References)
				}
				references[slot] = run.References
			}
		}
		pages := make([]ttcsnapshot.MemoryPageDescriptor, 0, len(chunk.Pages))
		raw := make([]byte, ttcsnapshot.MemoryChunkBytes)
		for _, page := range chunk.Pages {
			pageReferences, ok := references[page.Slot]
			if !ok {
				// A transport chunk may contain pages not selected by this
				// manifest (for example, a filtered resource capture). They are
				// not part of this graph import.
				continue
			}
			if pageReferences == 0 {
				return GraphImportResult{}, fmt.Errorf("memory chunk %s slot %d has zero references", use.id, page.Slot)
			}
			logicalSequence := chunk.Logical.Sequence*PagesPerChunk + uint64(page.Slot)
			pages = append(pages, ttcsnapshot.MemoryPageDescriptor{
				Slot: page.Slot,
				Identity: ttcsnapshot.MemoryPageIdentity{
					LogicalAllocator: [16]byte(chunk.Logical.Allocator),
					LogicalSequence:  logicalSequence,
					VersionAllocator: [16]byte(page.Version.Allocator),
					VersionSequence:  page.Version.Sequence,
					References:       pageReferences,
				},
			})
			if !page.Zero {
				copy(raw[int(page.Slot)*hostarch.PageSize:], page.Data)
			}
		}
		if len(pages) != len(references) {
			return GraphImportResult{}, fmt.Errorf("memory chunk %s contains %d pages, manifest references %d slots", use.id, len(pages), len(references))
		}
		existing, err := graph.ExistingMemoryPagesInStates(pages, reusableStates...)
		if err != nil {
			return GraphImportResult{}, err
		}
		pageIDs := make(map[uint8]ttcsnapshot.PageID, len(pages))
		for slot, page := range existing {
			pageIDs[slot] = page
		}
		missing := make([]ttcsnapshot.MemoryPageDescriptor, 0, len(pages)-len(existing))
		for _, page := range pages {
			if _, ok := existing[page.Slot]; !ok {
				missing = append(missing, page)
			}
		}
		var reservation ttcsnapshot.MemoryChunkReservation
		if len(missing) != 0 {
			pinPrefix := fmt.Sprintf("%s/construction/%08x", rootPrefix, objectIndex)
			reservation, err = graph.ReserveMemoryChunkPages(origin, pinPrefix, missing)
			if err != nil {
				return GraphImportResult{}, err
			}
			chunkID := ttcsnapshot.ObjectID(reservation.Chunk)
			if err := data.Install(chunkID, bytes.NewReader(raw), int64(len(raw))); err != nil {
				return GraphImportResult{}, fmt.Errorf("installing graph memory chunk %s: %w", chunkID, err)
			}
			activePins = append(activePins, reservation.Pins...)
			if err := graph.CommitMemoryChunk(reservation); err != nil {
				return GraphImportResult{}, fmt.Errorf("committing graph memory chunk %s: %w", chunkID, err)
			}
			for slot, page := range reservation.Pages {
				pageIDs[slot] = page
			}
		}
		for _, run := range use.runs {
			key := resourceKey(run.ResourceID)
			if pendingPages[key] == nil {
				pendingPages[key] = make(map[uint64]ttcsnapshot.ObjectID)
			}
			for delta := uint16(0); delta < run.Pages; delta++ {
				slot := uint8(uint16(run.FirstSlot) + delta)
				page, ok := pageIDs[slot]
				if !ok {
					return GraphImportResult{}, fmt.Errorf("manifest references missing slot %d in chunk %s", slot, use.id)
				}
				index := run.OldOffset/hostarch.PageSize + uint64(delta)
				if previous, exists := resources[key].pages[index]; exists && previous != ttcsnapshot.ObjectID(page) {
					return GraphImportResult{}, fmt.Errorf("memory resource maps page index %d twice", index)
				}
				resources[key].pages[index] = ttcsnapshot.ObjectID(page)
				pendingPages[key][index] = ttcsnapshot.ObjectID(page)
			}
		}
		pendingUses++
		if pendingUses == radixChunksPerTransaction {
			if err := flushPages(); err != nil {
				return GraphImportResult{}, err
			}
		}
	}
	if err := flushPages(); err != nil {
		return GraphImportResult{}, err
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

func graphChunkResidencyID(chunk ttcsnapshot.ObjectID) ID {
	var id ID
	copy(id[:8], []byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H'})
	binary.BigEndian.PutUint64(id[8:16], uint64(chunk.Snapshot))
	id[16] = byte(chunk.Kind)
	binary.BigEndian.PutUint64(id[17:25], chunk.Ordinal)
	return id
}

func graphChunkObjectID(id ID) (ttcsnapshot.ObjectID, error) {
	if !bytes.Equal(id[:8], []byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H'}) {
		return ttcsnapshot.ObjectID{}, errors.New("invalid graph memory residency identity")
	}
	object := ttcsnapshot.ObjectID{
		Snapshot: ttcsnapshot.SnapshotID(binary.BigEndian.Uint64(id[8:16])),
		Kind:     ttcsnapshot.ObjectKind(id[16]),
		Ordinal:  binary.BigEndian.Uint64(id[17:25]),
	}
	if object.Kind != ttcsnapshot.ObjectKindMemoryChunk {
		return ttcsnapshot.ObjectID{}, errors.New("residency identity is not a graph memory chunk")
	}
	if _, err := ttcsnapshot.ParseObjectID(object.String()); err != nil {
		return ttcsnapshot.ObjectID{}, err
	}
	if id[25] != 0 || id[26] != 0 || id[27] != 0 || id[28] != 0 || id[29] != 0 || id[30] != 0 || id[31] != 0 {
		return ttcsnapshot.ObjectID{}, errors.New("graph memory residency identity has trailing data")
	}
	return object, nil
}

// GraphChunkObjects derives the authoritative bulk object map from an
// ephemeral graph manifest. The opaque IDs encode coordinates, not hashes.
func GraphChunkObjects(manifest Manifest) (map[ID]ttcsnapshot.ObjectID, error) {
	result := make(map[ID]ttcsnapshot.ObjectID)
	for _, run := range manifest.Runs {
		object, err := graphChunkObjectID(run.Object)
		if err != nil {
			return nil, err
		}
		result[run.Object] = object
	}
	return result, nil
}

// BuildGraphManifest enumerates graph roots for rehydration. Enumeration is
// intentionally an O(mapped pages) rehydrate operation; fork and lookup share
// roots directly and never call it.
func BuildGraphManifest(graph ttcsnapshot.MemoryCatalog, roots []GraphMemoryRoot) (GraphManifestResult, error) {
	if graph == nil || len(roots) == 0 {
		return GraphManifestResult{}, errors.New("memory graph roots are unavailable")
	}
	var bindings []StoredBinding
	result := GraphManifestResult{Chunks: make(map[ID]ttcsnapshot.ObjectID)}
	chunkResources := make(map[ttcsnapshot.ObjectID]checkpoint.ResourceID)
	for _, root := range roots {
		pages, err := graph.MemoryPages(root.Root)
		if err != nil {
			return GraphManifestResult{}, fmt.Errorf("enumerating memory root %q: %w", root.Name, err)
		}
		indexes := make([]uint64, 0, len(pages))
		for index := range pages {
			indexes = append(indexes, index)
		}
		sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
		for _, index := range indexes {
			page := pages[index]
			chunk, offset, identity, err := graph.MemoryPageRecord(page)
			if err != nil {
				return GraphManifestResult{}, err
			}
			if identity.LogicalAllocator == ([16]byte{}) || identity.VersionAllocator == ([16]byte{}) || identity.References == 0 {
				return GraphManifestResult{}, fmt.Errorf("page %s has incomplete restore identity", page)
			}
			chunkObject := ttcsnapshot.ObjectID(chunk)
			if previous, ok := chunkResources[chunkObject]; ok && previous != root.ResourceID {
				return GraphManifestResult{}, fmt.Errorf("memory chunk %s spans resources %s and %s", chunkObject, previous, root.ResourceID)
			}
			chunkResources[chunkObject] = root.ResourceID
			residency := graphChunkResidencyID(chunkObject)
			result.Chunks[residency] = chunkObject
			logicalPage := pgalloc.LogicalPageID{Allocator: pgalloc.LogicalAllocatorID(identity.LogicalAllocator), Sequence: identity.LogicalSequence}
			logicalChunk, slot := logicalPage.Chunk()
			if want := uint8(offset / hostarch.PageSize); slot != want {
				return GraphManifestResult{}, fmt.Errorf("page %s has logical slot %d but bulk offset slot %d", page, slot, want)
			}
			bindings = append(bindings, StoredBinding{
				Binding: pgalloc.CapturedPageBinding{
					ResourceID: root.ResourceID,
					OldOffset:  index * hostarch.PageSize,
					References: identity.References,
					Page:       logicalPage,
					Version:    pgalloc.PageVersionID{Allocator: pgalloc.LogicalAllocatorID(identity.VersionAllocator), Sequence: identity.VersionSequence},
					Chunk:      logicalChunk,
					Slot:       slot,
				},
				Object: residency,
			})
		}
	}
	manifest, err := CompactBindings(bindings)
	if err != nil {
		return GraphManifestResult{}, err
	}
	result.Manifest = manifest
	return result, nil
}

// GraphChunkReader exposes graph-native raw 1 MiB chunks in the dense stream
// assigned by InstallManifestSet. It owns data when ownData is true.
type GraphChunkReader struct {
	data    *ttcsnapshot.LocalDataStore
	chunks  []ttcsnapshot.ObjectID
	remote  *ttcsnapshot.ObjectRelayClient
	ownData bool
	fetchMu sync.Mutex
	mu      sync.RWMutex
	closed  bool
}

// NewGraphChunkReader constructs a demand reader without materializing a
// compatibility chunk repository.
func NewGraphChunkReader(data *ttcsnapshot.LocalDataStore, chunkObjects map[ID]ttcsnapshot.ObjectID, installed []InstalledChunk, ownData bool) (*GraphChunkReader, error) {
	return NewGraphChunkReaderWithRelay(data, chunkObjects, installed, nil, ownData)
}

// NewGraphChunkReaderWithRelay constructs a demand reader that populates
// missing immutable chunks through remote. The relay is adopted and closed
// with the reader.
func NewGraphChunkReaderWithRelay(data *ttcsnapshot.LocalDataStore, chunkObjects map[ID]ttcsnapshot.ObjectID, installed []InstalledChunk, remote *ttcsnapshot.ObjectRelayClient, ownData bool) (*GraphChunkReader, error) {
	fail := func(err error) (*GraphChunkReader, error) {
		if remote != nil {
			err = errors.Join(err, remote.Close())
		}
		if ownData && data != nil {
			err = errors.Join(err, data.Close())
		}
		return nil, err
	}
	if data == nil {
		return fail(errors.New("nil TTC graph memory data store"))
	}
	chunks := make([]ttcsnapshot.ObjectID, len(installed))
	for index, chunk := range installed {
		if chunk.StreamOffset != uint64(index)*pgalloc.SparseChunkBytes || chunk.Target.Length() != pgalloc.SparseChunkBytes {
			return fail(fmt.Errorf("invalid graph installed chunk %d: %+v", index, chunk))
		}
		object, ok := chunkObjects[chunk.Object]
		if !ok || object.Kind != ttcsnapshot.ObjectKindMemoryChunk {
			return fail(fmt.Errorf("graph installed chunk %s has no bulk object", chunk.Object))
		}
		chunks[index] = object
	}
	return &GraphChunkReader{data: data, chunks: chunks, remote: remote, ownData: ownData}, nil
}

// Close prevents future reads and releases a donated data directory.
func (r *GraphChunkReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var remoteErr error
	if r.remote != nil {
		remoteErr = r.remote.Close()
	}
	if r.ownData {
		return errors.Join(remoteErr, r.data.Close())
	}
	return remoteErr
}

func (r *GraphChunkReader) openChunk(id ttcsnapshot.ObjectID) (*os.File, error) {
	const length = int64(pgalloc.SparseChunkBytes)
	file, err := r.data.Open(id, length)
	if err == nil || r.remote == nil || !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	// Multiple asynchronous MemoryFile readers may fault the same chunk. Only
	// one may relay and atomically install it; later readers observe the cache.
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if file, err = r.data.Open(id, length); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	data, err := r.remote.Fetch(id, uint64(length))
	if err != nil {
		return nil, err
	}
	if err := r.data.Install(id, bytes.NewReader(data), length); err != nil {
		return nil, err
	}
	return r.data.Open(id, length)
}

// ReadAt reads each touched chunk completely before exposing any of its bytes
// to the asynchronous installer.
func (r *GraphChunkReader) ReadAt(dst []byte, off int64) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return 0, errors.New("TTC graph memory reader is closed")
	}
	if off < 0 {
		return 0, errors.New("negative TTC graph memory offset")
	}
	total := uint64(len(r.chunks)) * pgalloc.SparseChunkBytes
	if uint64(off) >= total {
		return 0, io.EOF
	}
	written := 0
	for len(dst) != 0 && uint64(off) < total {
		index := int(uint64(off) / pgalloc.SparseChunkBytes)
		within := uint64(off) % pgalloc.SparseChunkBytes
		file, err := r.openChunk(r.chunks[index])
		if err != nil {
			return written, err
		}
		data := make([]byte, pgalloc.SparseChunkBytes)
		_, readErr := io.ReadFull(file, data)
		closeErr := file.Close()
		if readErr != nil {
			return written, readErr
		}
		if closeErr != nil {
			return written, closeErr
		}
		n := copy(dst, data[within:])
		written += n
		dst = dst[n:]
		off += int64(n)
	}
	if len(dst) != 0 {
		return written, io.EOF
	}
	return written, nil
}

// AsyncReader wraps graph chunks with the same atomic 1 MiB demand geometry
// as the existing sparse loader.
func (r *GraphChunkReader) AsyncReader(parallel int) stateio.AsyncReader {
	if parallel <= 0 {
		parallel = 1
	}
	return stateio.NewIOReader(r, pgalloc.SparseChunkBytes, 16, parallel)
}

// RangeLoaded is a no-op: graph roots, rather than per-reader consumption
// files, retain authoritative chunks.
func (*GraphChunkReader) RangeLoaded(uint64, uint64) {}

// ActivateConsumption is a no-op for graph-owned lifetime tracking.
func (*GraphChunkReader) ActivateConsumption([]ID) {}

// AbortConsumption is a no-op for graph-owned lifetime tracking.
func (*GraphChunkReader) AbortConsumption() {}
