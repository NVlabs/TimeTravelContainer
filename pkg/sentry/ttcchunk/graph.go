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
	"math/bits"
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

// graphImportCheckpointBytes bounds crash-recovery replay while importing a
// large memory image. Checkpointing after every small graph transaction would
// repeatedly rewrite the growing radix forest; 64 MiB, or the checkpoint's own
// size once it is larger, keeps that cost amortized while preventing one
// interrupted capture from leaving a WAL much longer than its checkpoint.
const graphImportCheckpointBytes = 64 << 20

func checkpointGraphImportIfNeeded(graph *ttcsnapshot.Store, threshold int64) error {
	if graph == nil || threshold <= 0 || !graph.CheckpointDue(threshold) {
		return nil
	}
	if err := graph.Checkpoint(); err != nil {
		return fmt.Errorf("checkpointing in-progress memory graph import: %w", err)
	}
	return nil
}

func resourceKey(id checkpoint.ResourceID) string {
	return id.ContainerName + "\x00" + id.Path
}

// importSnapshotGraph installs manifest and its immutable source chunks into
// graph. getChunk returns the pages of one capture object.
func importSnapshotGraph(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, getChunk func(ID) (Chunk, error)) (GraphImportResult, error) {
	return importSnapshotGraphWithReusableStates(graph, data, origin, manifest, rootPrefix, getChunk, nil)
}

func importSnapshotGraphWithReusableStates(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, getChunk func(ID) (Chunk, error), reusableStates []ttcsnapshot.SnapshotState) (GraphImportResult, error) {
	return importCapture(graph, data, origin, manifest, rootPrefix, captureSource{chunk: getChunk}, reusableStates)
}

// captureSource yields the pages of a capture's objects to an import.
type captureSource struct {
	// need, when set, is told which slots of every object the graph lacks,
	// objects in ascending ID order, before any object is read, so that
	// only those are sent.
	need func(objects []ID, slots [][slotBitmapBytes]byte) error
	// chunk returns the pages of object id. It is called in ascending ID
	// order, only for objects some of whose slots the graph lacks. The
	// chunk may hold other pages too.
	chunk func(id ID) (Chunk, error)
	// finish, when set, is called once every object needed has been read,
	// to read what else the source carries and check that nothing follows.
	finish func() error
}

// importedResource is one MemoryFile of a capture and the memory tree its
// pages are mapped in.
type importedResource struct {
	id   checkpoint.ResourceID
	root string
	// pages maps each page index to the object slot mapped there.
	pages map[uint64]importedSlotKey
}

type importedSlotKey struct {
	object ID
	slot   uint8
}

// importedObject is one capture object: a version of a logical chunk.
type importedObject struct {
	id      ID
	logical pgalloc.LogicalChunkID
	slots   map[uint8]*importedSlot
	// missing has the slots whose pages the graph does not hold yet, and
	// chunk is the new chunk that holds them.
	missing [slotBitmapBytes]byte
	chunk   ttcsnapshot.ObjectID
}

// importedSlot is the page in one slot of a capture object: its identity,
// where its bytes are to be, and the places it is mapped.
type importedSlot struct {
	identity ttcsnapshot.MemoryPageIdentity
	location ttcsnapshot.MemoryPageLocation
	links    []importedLink
}

// importedLink is one place a page is mapped: page index of a resource's tree.
type importedLink struct {
	resource *importedResource
	index    uint64
}

// graphTablesPerTransaction bounds the tables one transaction maps: 128 MiB
// of memory.
const graphTablesPerTransaction = 128

// importCapture installs manifest's pages into graph under rootPrefix, one
// memory tree per resource. Pages the graph already holds, by the identity the
// guest's memory manager gave them, are shared rather than stored again, and
// so is any table they fill exactly; only the others are read from source.
// New pages are written a chunk per capture object, and mapped once all are
// durable, in bounded transactions.
func importCapture(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, source captureSource, reusableStates []ttcsnapshot.SnapshotState) (_ GraphImportResult, retErr error) {
	if graph == nil || data == nil || origin == 0 || rootPrefix == "" || source.chunk == nil {
		return GraphImportResult{}, errors.New("invalid TTC memory graph import")
	}
	if graph.HasRootUnder(rootPrefix) {
		return GraphImportResult{}, fmt.Errorf("memory graph root prefix %q is already in use", rootPrefix)
	}
	objects, resources, err := planCaptureImport(manifest, rootPrefix)
	if err != nil {
		return GraphImportResult{}, err
	}
	var pins []string
	defer func() {
		if len(pins) != 0 {
			if err := graph.DeletePins(pins); err != nil && retErr == nil {
				retErr = fmt.Errorf("releasing the memory pages a capture shares: %w", err)
			}
		}
		if retErr == nil {
			return
		}
		for _, resource := range resources {
			if _, exists := graph.Root(resource.root); exists {
				_ = graph.DeleteRoot(resource.root)
			}
		}
		for {
			collected, err := graph.Collect(4096)
			if err != nil || collected == 0 {
				break
			}
		}
		_, _, _ = data.ReclaimUnreferenced(graph)
	}()

	// Find the pages the graph holds already, and hold them until the new
	// trees refer to them.
	var identities []ttcsnapshot.MemoryPageIdentity
	var planned []*importedSlot
	var plannedObjects []*importedObject
	var plannedSlots []uint8
	for _, object := range objects {
		for slot := 0; slot < PagesPerChunk; slot++ {
			if page, ok := object.slots[uint8(slot)]; ok {
				identities = append(identities, page.identity)
				planned = append(planned, page)
				plannedObjects = append(plannedObjects, object)
				plannedSlots = append(plannedSlots, uint8(slot))
			}
		}
	}
	locations, held, err := graph.HoldMemoryPages(rootPrefix+"/held", identities, reusableStates...)
	if err != nil {
		return GraphImportResult{}, err
	}
	pins = held
	for index, location := range locations {
		if location.Chunk.IsZero() {
			setBit(&plannedObjects[index].missing, plannedSlots[index])
			continue
		}
		planned[index].location = location
	}
	if source.need != nil {
		ids := make([]ID, len(objects))
		slots := make([][slotBitmapBytes]byte, len(objects))
		for index, object := range objects {
			ids[index] = object.id
			slots[index] = object.missing
		}
		if err := source.need(ids, slots); err != nil {
			return GraphImportResult{}, err
		}
	}

	// Each object with pages the graph lacks gets a new chunk, which holds
	// them at their slots.
	var incomplete []*importedObject
	for _, object := range objects {
		if object.missing != ([slotBitmapBytes]byte{}) {
			incomplete = append(incomplete, object)
		}
	}
	if len(incomplete) != 0 {
		kinds := make([]ttcsnapshot.ObjectKind, len(incomplete))
		for index := range kinds {
			kinds[index] = ttcsnapshot.ObjectKindMemoryChunk
		}
		chunks, err := graph.ReserveObjects(origin, kinds)
		if err != nil {
			return GraphImportResult{}, err
		}
		for index, object := range incomplete {
			object.chunk = chunks[index]
			for slot, page := range object.slots {
				if hasBit(&object.missing, slot) {
					page.location = ttcsnapshot.MemoryPageLocation{Chunk: object.chunk, Page: slot}
				}
			}
		}
	}
	tables, newChunks := planCaptureTables(resources, objects, incomplete)

	// Collection must not see an installed chunk before a transaction naming
	// it commits, or it would remove it as unreferenced.
	if err := data.WithGraphMutation(func() error {
		installed := make([]ttcsnapshot.ObjectID, 0, len(incomplete))
		for _, object := range incomplete {
			chunk, err := source.chunk(object.id)
			if err != nil {
				return fmt.Errorf("reading memory chunk %s: %w", object.id, err)
			}
			extents, err := object.extents(chunk)
			if err != nil {
				return err
			}
			// A new chunk holds only the pages the graph does not have yet;
			// the rest of it, like its zero pages, is a hole. A branch that
			// rewrote a few pages of each chunk otherwise stored its whole
			// image again.
			if err := data.InstallExtentsDeferred(object.chunk, ttcsnapshot.MemoryChunkBytes, extents); err != nil {
				return fmt.Errorf("installing graph memory chunk %s: %w", object.chunk, err)
			}
			installed = append(installed, object.chunk)
		}
		// The chunks must be durable before a transaction naming them is.
		// Their writeback began as each was written, while the rest streamed.
		if err := data.SyncObjects(installed); err != nil {
			return fmt.Errorf("syncing graph memory chunks: %w", err)
		}
		for begin := 0; begin < len(tables); begin += graphTablesPerTransaction {
			end := min(begin+graphTablesPerTransaction, len(tables))
			var chunks []ttcsnapshot.ObjectID
			for _, table := range newChunks[begin:end] {
				chunks = append(chunks, table...)
			}
			if err := graph.CommitMemoryTables(origin, tables[begin:end], chunks, reusableStates...); err != nil {
				return fmt.Errorf("committing graph memory tables: %w", err)
			}
			if err := checkpointGraphImportIfNeeded(graph, graphImportCheckpointBytes); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return GraphImportResult{}, err
	}
	if source.finish != nil {
		if err := source.finish(); err != nil {
			return GraphImportResult{}, err
		}
	}

	result := GraphImportResult{Snapshot: origin, Chunks: uint64(len(objects))}
	for _, resource := range resources {
		root, ok := graph.Root(resource.root)
		if !ok {
			return GraphImportResult{}, fmt.Errorf("memory resource root %q was not created", resource.root)
		}
		result.Roots = append(result.Roots, GraphMemoryRoot{ResourceID: resource.id, Name: resource.root, Root: root})
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

// planCaptureImport derives from manifest every page of the capture, with its
// identity and the places it is mapped, by object in ascending ID order, and
// the capture's resources, each with its root under rootPrefix.
func planCaptureImport(manifest Manifest, rootPrefix string) ([]*importedObject, []*importedResource, error) {
	resourcesByKey := make(map[string]*importedResource)
	for _, run := range manifest.Runs {
		key := resourceKey(run.ResourceID)
		if resourcesByKey[key] == nil {
			resourcesByKey[key] = &importedResource{id: run.ResourceID, pages: make(map[uint64]importedSlotKey)}
		}
	}
	keys := make([]string, 0, len(resourcesByKey))
	for key := range resourcesByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resources := make([]*importedResource, len(keys))
	for index, key := range keys {
		resources[index] = resourcesByKey[key]
		resources[index].root = fmt.Sprintf("%s/resource-%04x", rootPrefix, index)
	}

	objectsByID := make(map[ID]*importedObject)
	for _, run := range manifest.Runs {
		if run.References == 0 {
			return nil, nil, fmt.Errorf("memory chunk %s is mapped with zero references", run.Object)
		}
		if run.Pages == 0 || uint16(run.FirstSlot)+run.Pages > PagesPerChunk {
			return nil, nil, fmt.Errorf("memory chunk %s has a run of %d pages from slot %d", run.Object, run.Pages, run.FirstSlot)
		}
		if run.FirstPage.Allocator != run.Logical.Allocator || run.FirstPage.Sequence != run.Logical.Sequence*PagesPerChunk+uint64(run.FirstSlot) {
			return nil, nil, fmt.Errorf("memory chunk %s maps page %+v at slot %d of logical chunk %+v", run.Object, run.FirstPage, run.FirstSlot, run.Logical)
		}
		object := objectsByID[run.Object]
		if object == nil {
			object = &importedObject{id: run.Object, logical: run.Logical, slots: make(map[uint8]*importedSlot)}
			objectsByID[run.Object] = object
		} else if object.logical != run.Logical {
			return nil, nil, fmt.Errorf("memory chunk %s has logical coordinates %+v and %+v", run.Object, object.logical, run.Logical)
		}
		resource := resourcesByKey[resourceKey(run.ResourceID)]
		for delta := uint16(0); delta < run.Pages; delta++ {
			slotNumber := uint8(uint16(run.FirstSlot) + delta)
			identity := ttcsnapshot.MemoryPageIdentity{
				LogicalAllocator: [16]byte(run.Logical.Allocator),
				LogicalSequence:  run.Logical.Sequence*PagesPerChunk + uint64(slotNumber),
				VersionAllocator: [16]byte(run.FirstVersion.Allocator),
				VersionSequence:  run.FirstVersion.Sequence + uint64(delta),
				References:       run.References,
			}
			slot := object.slots[slotNumber]
			if slot == nil {
				slot = &importedSlot{identity: identity}
				object.slots[slotNumber] = slot
			} else if slot.identity != identity {
				return nil, nil, fmt.Errorf("chunk %s slot %d is mapped with conflicting identities %+v and %+v", run.Object, slotNumber, slot.identity, identity)
			}
			index := run.OldOffset/hostarch.PageSize + uint64(delta)
			key := importedSlotKey{object: run.Object, slot: slotNumber}
			if previous, mapped := resource.pages[index]; mapped {
				if previous != key {
					return nil, nil, fmt.Errorf("memory resource maps page index %d twice", index)
				}
				continue
			}
			resource.pages[index] = key
			slot.links = append(slot.links, importedLink{resource: resource, index: index})
		}
	}
	objects := make([]*importedObject, 0, len(objectsByID))
	for _, object := range objectsByID {
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool { return bytes.Compare(objects[i].id[:], objects[j].id[:]) < 0 })
	return objects, resources, nil
}

// planCaptureTables returns the tables that map the capture's pages, once
// every page's location is known, in root and index order, and for each the
// new chunks, of incomplete objects, that no earlier table refers to.
func planCaptureTables(resources []*importedResource, objects, incomplete []*importedObject) ([]ttcsnapshot.NewMemoryTable, [][]ttcsnapshot.ObjectID) {
	type tableKey struct {
		resource int
		index    uint64
	}
	resourceIndex := make(map[*importedResource]int, len(resources))
	for index, resource := range resources {
		resourceIndex[resource] = index
	}
	entries := make(map[tableKey][]ttcsnapshot.MemoryTableEntry)
	for _, object := range objects {
		for _, slot := range object.slots {
			for _, link := range slot.links {
				key := tableKey{resource: resourceIndex[link.resource], index: link.index / ttcsnapshot.PagesPerMemoryTable * ttcsnapshot.PagesPerMemoryTable}
				entries[key] = append(entries[key], ttcsnapshot.MemoryTableEntry{
					Slot: uint8(link.index % ttcsnapshot.PagesPerMemoryTable), Location: slot.location, Identity: slot.identity,
				})
			}
		}
	}
	keys := make([]tableKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].resource != keys[j].resource {
			return keys[i].resource < keys[j].resource
		}
		return keys[i].index < keys[j].index
	})
	fresh := make(map[ttcsnapshot.ObjectID]bool, len(incomplete))
	for _, object := range incomplete {
		fresh[object.chunk] = true
	}
	tables := make([]ttcsnapshot.NewMemoryTable, len(keys))
	newChunks := make([][]ttcsnapshot.ObjectID, len(keys))
	for index, key := range keys {
		table := entries[key]
		sort.Slice(table, func(i, j int) bool { return table[i].Slot < table[j].Slot })
		tables[index] = ttcsnapshot.NewMemoryTable{Root: resources[key.resource].root, Index: key.index, Entries: table}
		for _, entry := range table {
			if fresh[entry.Location.Chunk] {
				newChunks[index] = append(newChunks[index], entry.Location.Chunk)
				delete(fresh, entry.Location.Chunk)
			}
		}
	}
	return tables, newChunks
}

// extents checks chunk against the manifest and returns the extents of the
// object's new chunk: the pages the graph lacks that are not zero, at their
// slots.
func (object *importedObject) extents(chunk Chunk) ([]ttcsnapshot.Extent, error) {
	if chunk.Logical != object.logical {
		return nil, fmt.Errorf("memory chunk %s has logical coordinate %+v, manifest references %+v", object.id, chunk.Logical, object.logical)
	}
	var extents []ttcsnapshot.Extent
	var found [slotBitmapBytes]byte
	for _, page := range chunk.Pages {
		// The graph holds the others already, or the manifest does not map
		// them; a source that sends every page sends those too.
		if !hasBit(&object.missing, page.Slot) {
			continue
		}
		if hasBit(&found, page.Slot) {
			return nil, fmt.Errorf("memory chunk %s repeats slot %d", object.id, page.Slot)
		}
		setBit(&found, page.Slot)
		planned := object.slots[page.Slot]
		if [16]byte(page.Version.Allocator) != planned.identity.VersionAllocator || page.Version.Sequence != planned.identity.VersionSequence {
			return nil, fmt.Errorf("memory chunk %s slot %d holds version %+v, manifest references another", object.id, page.Slot, page.Version)
		}
		if !page.Zero {
			if len(page.Data) != hostarch.PageSize {
				return nil, fmt.Errorf("memory chunk %s slot %d has %d bytes", object.id, page.Slot, len(page.Data))
			}
			extents = append(extents, ttcsnapshot.Extent{Offset: int64(page.Slot) * hostarch.PageSize, Data: page.Data})
		}
	}
	if found != object.missing {
		return nil, fmt.Errorf("memory chunk %s contains %d of the %d pages the graph lacks", object.id, countBits(&found), countBits(&object.missing))
	}
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })
	return extents, nil
}

func countBits(bitmap *[slotBitmapBytes]byte) int {
	count := 0
	for _, b := range bitmap {
		count += bits.OnesCount8(b)
	}
	return count
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
		entries, err := graph.MemoryPageEntries(root.Root)
		if err != nil {
			return GraphManifestResult{}, fmt.Errorf("enumerating memory root %q: %w", root.Name, err)
		}
		for _, entry := range entries {
			identity := entry.Identity
			chunkObject := entry.Location.Chunk
			if previous, ok := chunkResources[chunkObject]; ok && previous != root.ResourceID {
				return GraphManifestResult{}, fmt.Errorf("memory chunk %s spans resources %s and %s", chunkObject, previous, root.ResourceID)
			}
			chunkResources[chunkObject] = root.ResourceID
			residency := graphChunkResidencyID(chunkObject)
			result.Chunks[residency] = chunkObject
			logicalPage := pgalloc.LogicalPageID{Allocator: pgalloc.LogicalAllocatorID(identity.LogicalAllocator), Sequence: identity.LogicalSequence}
			logicalChunk, slot := logicalPage.Chunk()
			if slot != entry.Location.Page {
				return GraphManifestResult{}, fmt.Errorf("page %d of memory root %q has logical slot %d but is page %d of its chunk", entry.Index, root.Name, slot, entry.Location.Page)
			}
			bindings = append(bindings, StoredBinding{
				Binding: pgalloc.CapturedPageBinding{
					ResourceID: root.ResourceID,
					OldOffset:  entry.Index * hostarch.PageSize,
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
