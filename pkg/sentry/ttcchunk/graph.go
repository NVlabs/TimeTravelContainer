// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/bits"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

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
	return importCapture(graph, data, origin, manifest, rootPrefix, captureSource{chunk: getChunk}, ttcsnapshot.MemoryReuse{States: reusableStates})
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
}

// importedObject is one capture object: a version of a logical chunk.
type importedObject struct {
	id      ID
	logical pgalloc.LogicalChunkID
	// resource is the index of the resource whose pages the object holds.
	resource int
	// mapped has the slots the capture maps, and slots the pages in them, in
	// slot order.
	mapped [slotBitmapBytes]byte
	slots  []importedSlot
	// missing has the slots whose pages the graph does not hold yet.
	missing [slotBitmapBytes]byte
}

// slot returns the page in a slot the capture maps.
func (object *importedObject) slot(slot uint8) *importedSlot {
	return &object.slots[countBitsBelow(&object.mapped, slot)]
}

// importedSlot is the page in one slot of a capture object: its identity and
// where its bytes are to be.
type importedSlot struct {
	identity ttcsnapshot.MemoryPageIdentity
	location ttcsnapshot.MemoryPageLocation
}

// number returns the slot's number in its object.
func (slot *importedSlot) number() uint8 {
	return uint8(slot.identity.LogicalSequence % PagesPerChunk)
}

// importedMapping is one place a page is mapped: page index of a resource's
// tree.
type importedMapping struct {
	resource int
	index    uint64
	object   *importedObject
	slot     uint8
}

// graphTablesPerTransaction bounds the tables one transaction maps, 128 MiB
// of memory, unless they write less journal than graphTransactionJournalBytes,
// as tables the graph shares do.
const graphTablesPerTransaction = 128

// graphTransactionJournalBytes is about what a transaction of
// graphTablesPerTransaction new tables writes.
const graphTransactionJournalBytes = 2 << 20

// graphTablesPerTransactionLimit bounds the tables one transaction maps
// however little the last wrote. Tables the graph shared may be followed by
// tables it does not, a publication having changed meanwhile the origins a
// capture may share: a batch of all the 1,408 tables left after the first
// of a 1.5 GiB branch's wrote 18 MB, where a transaction may write 8 MiB.
// As many new tables as this write about 4 MiB.
const graphTablesPerTransactionLimit = 2 * graphTablesPerTransaction

// nextGraphTableBatch returns how many tables the next transaction maps, of
// remaining, after one that mapped tables and wrote written bytes: as many as
// would write what graphTablesPerTransaction new tables do, within
// graphTablesPerTransactionLimit.
func nextGraphTableBatch(written, tables, remaining int64) int {
	batch := int64(graphTablesPerTransaction)
	if written > 0 && tables > 0 {
		perTable := max(written/tables, 1)
		batch = max(graphTransactionJournalBytes/perTable, graphTablesPerTransaction)
	}
	return int(max(min(batch, graphTablesPerTransactionLimit, remaining), 1))
}

// heldPinComponent names the pins under which a capture holds the pages it
// shares with other origins until its trees refer to them:
// <rootPrefix>/held/<chunk>.
const heldPinComponent = "held"

// IsHeldPagePin reports whether name is a pin under which a capture held pages
// it shared with another origin. Only the capture needs it, so one that
// outlives its capture's import was left by a process that stopped during it.
func IsHeldPagePin(name string) bool {
	parent, chunk := path.Split(name)
	if !strings.HasSuffix(parent, "/"+heldPinComponent+"/") {
		return false
	}
	_, err := ttcsnapshot.ParseObjectID(chunk)
	return err == nil
}

// ReleaseHeldPagePins deletes pins under which a capture held pages it shared
// with other origins. An origin whose reference ledger is frozen while it is
// handed off refuses the deletion until the handoff ends, which may be after
// the capture: the pins are then deleted in the background once it has, so
// that they don't keep the origin's pages referenced for good. If that fails
// otherwise, IsHeldPagePin identifies the pins when the process next starts.
func ReleaseHeldPagePins(graph *ttcsnapshot.Store, pins []string) error {
	err := graph.DeletePins(pins)
	if !errors.Is(err, ttcsnapshot.ErrSnapshotLedgerFrozen) {
		return err
	}
	go func() {
		for delay := 10 * time.Millisecond; errors.Is(err, ttcsnapshot.ErrSnapshotLedgerFrozen); delay = min(2*delay, 5*time.Second) {
			time.Sleep(delay)
			err = graph.DeletePins(pins)
		}
	}()
	return nil
}

// importCapture installs manifest's pages into graph under rootPrefix, one
// memory tree per resource. Pages the graph already holds, by the identity the
// guest's memory manager gave them, are shared rather than stored again, and
// so is any table they fill exactly; only the others are read from source.
// New pages are packed into as few new chunks as hold them, and mapped once
// all are durable, in bounded transactions.
func importCapture(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, manifest Manifest, rootPrefix string, source captureSource, reuse ttcsnapshot.MemoryReuse) (_ GraphImportResult, retErr error) {
	if graph == nil || data == nil || origin == 0 || rootPrefix == "" || source.chunk == nil {
		return GraphImportResult{}, errors.New("invalid TTC memory graph import")
	}
	if graph.HasRootUnder(rootPrefix) {
		return GraphImportResult{}, fmt.Errorf("memory graph root prefix %q is already in use", rootPrefix)
	}
	objects, resources, mappings, err := planCaptureImport(manifest, rootPrefix)
	if err != nil {
		return GraphImportResult{}, err
	}
	var pins []string
	defer func() {
		if len(pins) != 0 {
			if err := ReleaseHeldPagePins(graph, pins); err != nil && retErr == nil {
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
	pages := 0
	for _, object := range objects {
		pages += len(object.slots)
	}
	identities := make([]ttcsnapshot.MemoryPageIdentity, 0, pages)
	for _, object := range objects {
		for index := range object.slots {
			identities = append(identities, object.slots[index].identity)
		}
	}
	locations, held, err := graph.HoldMemoryPagesReusing(rootPrefix+"/"+heldPinComponent, identities, reuse)
	if err != nil {
		return GraphImportResult{}, err
	}
	pins = held
	for _, object := range objects {
		for index := range object.slots {
			slot := &object.slots[index]
			if location := locations[0]; location.Chunk.IsZero() {
				setBit(&object.missing, slot.number())
			} else {
				slot.location = location
			}
			locations = locations[1:]
		}
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

	// The pages the graph lacks are packed into new chunks, in object and
	// slot order, the order the source sends them in, so that each chunk is
	// written whole, at once. A chunk per object, its pages at their slots,
	// held a fork's scattered writes a few pages to a file, between holes,
	// which a disk writes a tenth as fast. An object's pages share one chunk,
	// as rehydration loads memory a logical chunk at a time: an object that
	// does not fit where the chunk has room left starts the next, and the
	// room is left a hole. So does an object of another resource than the
	// chunk's, as rehydration installs a chunk in one resource's MemoryFile.
	var incomplete []*importedObject
	var starts []int
	position, resource := 0, -1
	for _, object := range objects {
		count := countBits(&object.missing)
		if count == 0 {
			continue
		}
		if used := position % PagesPerChunk; used != 0 && (used+count > PagesPerChunk || object.resource != resource) {
			position += PagesPerChunk - used
		}
		incomplete = append(incomplete, object)
		starts = append(starts, position)
		position += count
		resource = object.resource
	}
	var chunks []ttcsnapshot.ObjectID
	if position != 0 {
		kinds := make([]ttcsnapshot.ObjectKind, (position+PagesPerChunk-1)/PagesPerChunk)
		for index := range kinds {
			kinds[index] = ttcsnapshot.ObjectKindMemoryChunk
		}
		var err error
		if chunks, err = graph.ReserveObjects(origin, kinds); err != nil {
			return GraphImportResult{}, err
		}
		for index, object := range incomplete {
			position := starts[index]
			for index := range object.slots {
				if slot := &object.slots[index]; hasBit(&object.missing, slot.number()) {
					slot.location = ttcsnapshot.MemoryPageLocation{Chunk: chunks[position/PagesPerChunk], Page: uint8(position % PagesPerChunk)}
					position++
				}
			}
		}
	}
	tables, newChunks := planCaptureTables(resources, mappings, chunks)

	// Collection must not see an installed chunk before a transaction naming
	// it commits, or it would remove it as unreferenced.
	if err := data.WithGraphMutation(func() error {
		// Each new chunk is filled as the objects with its pages are read,
		// and installed once full. Its zero pages are holes.
		installed := make([]ttcsnapshot.ObjectID, 0, len(chunks))
		packed := make([]byte, ttcsnapshot.MemoryChunkBytes)
		var extents []ttcsnapshot.Extent
		install := func() error {
			chunk := chunks[len(installed)]
			if err := data.InstallExtentsDeferred(chunk, ttcsnapshot.MemoryChunkBytes, extents); err != nil {
				return fmt.Errorf("installing graph memory chunk %s: %w", chunk, err)
			}
			installed = append(installed, chunk)
			extents = extents[:0]
			return nil
		}
		for _, object := range incomplete {
			// The chunk's pages are read into the source's buffer, and so are
			// copied before the next object is read.
			chunk, err := source.chunk(object.id)
			if err != nil {
				return fmt.Errorf("reading memory chunk %s: %w", object.id, err)
			}
			pages, err := object.missingPages(chunk)
			if err != nil {
				return err
			}
			for _, page := range pages {
				location := object.slot(page.Slot).location
				for len(installed) < len(chunks) && location.Chunk != chunks[len(installed)] {
					if err := install(); err != nil {
						return err
					}
				}
				if len(installed) == len(chunks) {
					return fmt.Errorf("memory chunk %s slot %d is packed out of order", object.id, page.Slot)
				}
				if !page.Zero {
					offset := int(location.Page) * hostarch.PageSize
					copy(packed[offset:offset+hostarch.PageSize], page.Data)
					extents = append(extents, ttcsnapshot.Extent{Offset: int64(offset), Data: packed[offset : offset+hostarch.PageSize]})
				}
			}
		}
		for len(installed) < len(chunks) {
			if err := install(); err != nil {
				return err
			}
		}
		// The chunks must be durable before a transaction naming them is.
		// Their writeback began as each was written, while the rest streamed.
		if err := data.SyncObjects(installed); err != nil {
			return fmt.Errorf("syncing graph memory chunks: %w", err)
		}
		batch := graphTablesPerTransaction
		for begin := 0; begin < len(tables); {
			end := min(begin+batch, len(tables))
			var chunks []ttcsnapshot.ObjectID
			for _, table := range newChunks[begin:end] {
				chunks = append(chunks, table...)
			}
			before := graph.JournalBytes()
			if err := graph.CommitMemoryTablesReusing(origin, tables[begin:end], chunks, reuse); err != nil {
				return fmt.Errorf("committing graph memory tables: %w", err)
			}
			// Tables the graph shares write little: more go in the next
			// transaction. A checkpoint meanwhile leaves the journal
			// shorter.
			batch = nextGraphTableBatch(graph.JournalBytes()-before, int64(end-begin), int64(len(tables)-end))
			begin = end
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

	result := GraphImportResult{Snapshot: origin, Chunks: uint64(len(objects)), Pages: uint64(len(mappings))}
	for _, resource := range resources {
		root, ok := graph.Root(resource.root)
		if !ok {
			return GraphImportResult{}, fmt.Errorf("memory resource root %q was not created", resource.root)
		}
		result.Roots = append(result.Roots, GraphMemoryRoot{ResourceID: resource.id, Name: resource.root, Root: root})
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
// identity, by object in ascending ID order; the capture's resources, each with
// its root under rootPrefix; and the places the pages are mapped, by resource
// and page index.
func planCaptureImport(manifest Manifest, rootPrefix string) ([]*importedObject, []*importedResource, []importedMapping, error) {
	resourceIndexes := make(map[checkpoint.ResourceID]int)
	var resources []*importedResource
	for _, run := range manifest.Runs {
		if _, ok := resourceIndexes[run.ResourceID]; !ok {
			resourceIndexes[run.ResourceID] = len(resources)
			resources = append(resources, &importedResource{id: run.ResourceID})
		}
	}
	slices.SortFunc(resources, func(left, right *importedResource) int {
		return strings.Compare(resourceKey(left.id), resourceKey(right.id))
	})
	for index, resource := range resources {
		resourceIndexes[resource.id] = index
		resource.root = fmt.Sprintf("%s/resource-%04x", rootPrefix, index)
	}

	// Each object's slots are counted first, so that all are allocated at
	// once.
	objectIndexes := make(map[ID]int)
	var objects []*importedObject
	var capacities []int
	runObjects := make([]int, len(manifest.Runs))
	pages := 0
	for position, run := range manifest.Runs {
		if run.References == 0 {
			return nil, nil, nil, fmt.Errorf("memory chunk %s is mapped with zero references", run.Object)
		}
		if run.Pages == 0 || uint16(run.FirstSlot)+run.Pages > PagesPerChunk {
			return nil, nil, nil, fmt.Errorf("memory chunk %s has a run of %d pages from slot %d", run.Object, run.Pages, run.FirstSlot)
		}
		if run.ObjectPage != run.FirstSlot {
			return nil, nil, nil, fmt.Errorf("capture object %s holds slot %d at page %d", run.Object, run.FirstSlot, run.ObjectPage)
		}
		if run.FirstPage.Allocator != run.Logical.Allocator || run.FirstPage.Sequence != run.Logical.Sequence*PagesPerChunk+uint64(run.FirstSlot) {
			return nil, nil, nil, fmt.Errorf("memory chunk %s maps page %+v at slot %d of logical chunk %+v", run.Object, run.FirstPage, run.FirstSlot, run.Logical)
		}
		index, ok := objectIndexes[run.Object]
		if !ok {
			index = len(objects)
			objectIndexes[run.Object] = index
			objects = append(objects, &importedObject{id: run.Object, logical: run.Logical, resource: resourceIndexes[run.ResourceID]})
			capacities = append(capacities, 0)
		} else if objects[index].logical != run.Logical {
			return nil, nil, nil, fmt.Errorf("memory chunk %s has logical coordinates %+v and %+v", run.Object, objects[index].logical, run.Logical)
		} else if objects[index].resource != resourceIndexes[run.ResourceID] {
			return nil, nil, nil, fmt.Errorf("memory chunk %s spans resources", run.Object)
		}
		runObjects[position] = index
		capacities[index] = min(capacities[index]+int(run.Pages), PagesPerChunk)
		pages += int(run.Pages)
	}
	slots := 0
	for _, capacity := range capacities {
		slots += capacity
	}
	slab := make([]importedSlot, slots)
	for index, object := range objects {
		object.slots = slab[:0:capacities[index]]
		slab = slab[capacities[index]:]
	}

	mappings := make([]importedMapping, 0, pages)
	resource, resourceID := -1, checkpoint.ResourceID{}
	for position, run := range manifest.Runs {
		if resource < 0 || run.ResourceID != resourceID {
			resource, resourceID = resourceIndexes[run.ResourceID], run.ResourceID
		}
		object := objects[runObjects[position]]
		for delta := uint16(0); delta < run.Pages; delta++ {
			slotNumber := uint8(uint16(run.FirstSlot) + delta)
			identity := ttcsnapshot.MemoryPageIdentity{
				LogicalAllocator: [16]byte(run.Logical.Allocator),
				LogicalSequence:  run.Logical.Sequence*PagesPerChunk + uint64(slotNumber),
				VersionAllocator: [16]byte(run.FirstVersion.Allocator),
				VersionSequence:  run.FirstVersion.Sequence + uint64(delta),
				References:       run.References,
			}
			rank := countBitsBelow(&object.mapped, slotNumber)
			if !hasBit(&object.mapped, slotNumber) {
				setBit(&object.mapped, slotNumber)
				object.slots = slices.Insert(object.slots, rank, importedSlot{identity: identity})
			} else if object.slots[rank].identity != identity {
				return nil, nil, nil, fmt.Errorf("chunk %s slot %d is mapped with conflicting identities %+v and %+v", run.Object, slotNumber, object.slots[rank].identity, identity)
			}
			mappings = append(mappings, importedMapping{resource: resource, index: run.OldOffset/hostarch.PageSize + uint64(delta), object: object, slot: slotNumber})
		}
	}
	slices.SortFunc(objects, func(left, right *importedObject) int { return bytes.Compare(left.id[:], right.id[:]) })

	// A manifest's runs are in resource and offset order, as compaction
	// leaves them; so are the places they map, and a page index mapped twice
	// is mapped by adjacent ones.
	compare := func(left, right importedMapping) int {
		if c := cmp.Compare(left.resource, right.resource); c != 0 {
			return c
		}
		return cmp.Compare(left.index, right.index)
	}
	if !slices.IsSortedFunc(mappings, compare) {
		slices.SortFunc(mappings, compare)
	}
	distinct := mappings[:0]
	for _, mapping := range mappings {
		if len(distinct) != 0 {
			if previous := distinct[len(distinct)-1]; previous.resource == mapping.resource && previous.index == mapping.index {
				if previous.object != mapping.object || previous.slot != mapping.slot {
					return nil, nil, nil, fmt.Errorf("memory resource maps page index %d twice", mapping.index)
				}
				continue
			}
		}
		distinct = append(distinct, mapping)
	}
	return objects, resources, distinct, nil
}

// planCaptureTables returns the tables that map the capture's pages, once
// every page's location is known, in root and index order, and for each the
// chunks, of the new chunks, that no earlier table refers to. mappings are in
// resource and page index order.
func planCaptureTables(resources []*importedResource, mappings []importedMapping, chunks []ttcsnapshot.ObjectID) ([]ttcsnapshot.NewMemoryTable, [][]ttcsnapshot.ObjectID) {
	fresh := make(map[ttcsnapshot.ObjectID]bool, len(chunks))
	for _, chunk := range chunks {
		fresh[chunk] = true
	}
	var tables []ttcsnapshot.NewMemoryTable
	var newChunks [][]ttcsnapshot.ObjectID
	entries := make([]ttcsnapshot.MemoryTableEntry, len(mappings))
	begin, resource, index := 0, -1, uint64(0)
	for position, mapping := range mappings {
		if tableIndex := mapping.index / ttcsnapshot.PagesPerMemoryTable * ttcsnapshot.PagesPerMemoryTable; mapping.resource != resource || tableIndex != index {
			if resource >= 0 {
				tables = append(tables, ttcsnapshot.NewMemoryTable{Root: resources[resource].root, Index: index, Entries: entries[begin:position:position]})
			}
			begin, resource, index = position, mapping.resource, tableIndex
			newChunks = append(newChunks, nil)
		}
		slot := mapping.object.slot(mapping.slot)
		entries[position] = ttcsnapshot.MemoryTableEntry{Slot: uint8(mapping.index % ttcsnapshot.PagesPerMemoryTable), Location: slot.location, Identity: slot.identity}
		if hasBit(&mapping.object.missing, mapping.slot) && fresh[slot.location.Chunk] {
			newChunks[len(newChunks)-1] = append(newChunks[len(newChunks)-1], slot.location.Chunk)
			delete(fresh, slot.location.Chunk)
		}
	}
	if resource >= 0 {
		tables = append(tables, ttcsnapshot.NewMemoryTable{Root: resources[resource].root, Index: index, Entries: entries[begin:len(mappings):len(mappings)]})
	}
	return tables, newChunks
}

// missingPages checks chunk against the manifest and returns the pages of it
// the graph lacks, in slot order.
func (object *importedObject) missingPages(chunk Chunk) ([]Page, error) {
	if chunk.Logical != object.logical {
		return nil, fmt.Errorf("memory chunk %s has logical coordinate %+v, manifest references %+v", object.id, chunk.Logical, object.logical)
	}
	var pages []Page
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
		planned := object.slot(page.Slot)
		if [16]byte(page.Version.Allocator) != planned.identity.VersionAllocator || page.Version.Sequence != planned.identity.VersionSequence {
			return nil, fmt.Errorf("memory chunk %s slot %d holds version %+v, manifest references another", object.id, page.Slot, page.Version)
		}
		if !page.Zero && len(page.Data) != hostarch.PageSize {
			return nil, fmt.Errorf("memory chunk %s slot %d has %d bytes", object.id, page.Slot, len(page.Data))
		}
		pages = append(pages, page)
	}
	if found != object.missing {
		return nil, fmt.Errorf("memory chunk %s contains %d of the %d pages the graph lacks", object.id, countBits(&found), countBits(&object.missing))
	}
	slices.SortFunc(pages, func(left, right Page) int { return cmp.Compare(left.Slot, right.Slot) })
	return pages, nil
}

func countBits(bitmap *[slotBitmapBytes]byte) int {
	count := 0
	for _, b := range bitmap {
		count += bits.OnesCount8(b)
	}
	return count
}

// countBitsBelow returns the number of bits set in bitmap before bit.
func countBitsBelow(bitmap *[slotBitmapBytes]byte, bit uint8) int {
	count := 0
	word := int(bit) / 64
	for index := 0; index < word; index++ {
		count += bits.OnesCount64(binary.LittleEndian.Uint64(bitmap[index*8:]))
	}
	return count + bits.OnesCount64(binary.LittleEndian.Uint64(bitmap[word*8:])&(1<<(bit%64)-1))
}

func graphChunkResidencyID(chunk ttcsnapshot.ObjectID) ID {
	var id ID
	copy(id[:8], []byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H'})
	binary.BigEndian.PutUint64(id[8:16], uint64(chunk.Snapshot))
	id[16] = byte(chunk.Kind)
	binary.BigEndian.PutUint64(id[17:25], chunk.Ordinal)
	return id
}

// GraphChunkObject returns the graph memory chunk a Sentry names by id, its
// residency identity.
func GraphChunkObject(id ID) (ttcsnapshot.ObjectID, error) {
	return graphChunkObjectID(id)
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
// roots directly and never call it. Its manifest is the one CompactBindings
// makes of the roots' pages, built as they are enumerated.
func BuildGraphManifest(graph ttcsnapshot.MemoryCatalog, roots []GraphMemoryRoot) (GraphManifestResult, error) {
	if graph == nil || len(roots) == 0 {
		return GraphManifestResult{}, errors.New("memory graph roots are unavailable")
	}
	// A root's pages are enumerated in index order: with the roots in
	// resource order, they come in the order CompactBindings sorts them in.
	ordered := slices.Clone(roots)
	slices.SortStableFunc(ordered, func(left, right GraphMemoryRoot) int {
		if c := cmp.Compare(left.ResourceID.ContainerName, right.ResourceID.ContainerName); c != 0 {
			return c
		}
		return cmp.Compare(left.ResourceID.Path, right.ResourceID.Path)
	})
	result := GraphManifestResult{Chunks: make(map[ID]ttcsnapshot.ObjectID)}
	chunkResources := make(map[ttcsnapshot.ObjectID]checkpoint.ResourceID)
	var (
		runs []ManifestRun
		// runChunk is the chunk of the last run.
		runChunk ttcsnapshot.ObjectID
	)
	for index, root := range ordered {
		if index != 0 && root.ResourceID == ordered[index-1].ResourceID {
			return GraphManifestResult{}, fmt.Errorf("memory roots %q and %q map one resource %s", ordered[index-1].Name, root.Name, root.ResourceID)
		}
		// A run of the root's resource extends only a run of the same.
		rootRuns := len(runs)
		if err := graph.VisitMemoryPages(root.Root, func(entry *ttcsnapshot.MemoryPageEntry) error {
			identity := &entry.Identity
			page := pgalloc.LogicalPageID{Allocator: pgalloc.LogicalAllocatorID(identity.LogicalAllocator), Sequence: identity.LogicalSequence}
			version := pgalloc.PageVersionID{Allocator: pgalloc.LogicalAllocatorID(identity.VersionAllocator), Sequence: identity.VersionSequence}
			if identity.References == 0 || version.Allocator == (pgalloc.LogicalAllocatorID{}) {
				return fmt.Errorf("page %d has an incomplete identity", entry.Index)
			}
			oldOffset := entry.Index * hostarch.PageSize
			logical, slot := page.Chunk()
			chunk := entry.Location.Chunk
			if len(runs) > rootRuns && chunk == runChunk {
				run := &runs[len(runs)-1]
				delta := uint64(run.Pages)
				if run.Pages != PagesPerChunk && run.References == identity.References && run.Logical == logical &&
					oldOffset == run.OldOffset+delta*hostarch.PageSize &&
					page.Allocator == run.FirstPage.Allocator && page.Sequence == run.FirstPage.Sequence+delta &&
					version.Allocator == run.FirstVersion.Allocator && version.Sequence == run.FirstVersion.Sequence+delta &&
					uint16(slot) == uint16(run.FirstSlot)+run.Pages && uint16(entry.Location.Page) == uint16(run.ObjectPage)+run.Pages {
					run.Pages++
					return nil
				}
			}
			if previous, ok := chunkResources[chunk]; ok && previous != root.ResourceID {
				return fmt.Errorf("memory chunk %s spans resources %s and %s", chunk, previous, root.ResourceID)
			}
			chunkResources[chunk] = root.ResourceID
			residency := graphChunkResidencyID(chunk)
			result.Chunks[residency] = chunk
			runs = append(runs, ManifestRun{
				ResourceID: root.ResourceID, OldOffset: oldOffset, References: identity.References,
				FirstPage: page, FirstVersion: version, Logical: logical, FirstSlot: slot, Pages: 1,
				Object: residency, ObjectPage: entry.Location.Page,
			})
			runChunk = chunk
			return nil
		}); err != nil {
			return GraphManifestResult{}, fmt.Errorf("enumerating memory root %q: %w", root.Name, err)
		}
	}
	result.Manifest = Manifest{Runs: runs}
	return result, nil
}

// GraphChunkReader exposes graph-native raw 1 MiB chunks in the dense stream
// assigned by InstallManifestSet. It owns data when ownData is true.
type GraphChunkReader struct {
	data    *ttcsnapshot.LocalDataStore
	chunks  []ttcsnapshot.ObjectID
	remote  *ttcsnapshot.ObjectRelayClient
	ownData bool
	fetcher objectFetcher
	mu      sync.RWMutex
	closed  bool
}

// objectFetcher opens immutable objects from data, first fetching those it
// lacks from remote and installing them. Readers of one object share one
// fetch of it, and different objects are fetched at once, so that each
// install's sync overlaps the others' rather than waiting for them: a
// rehydration on another node that fetched its chunks one at a time spent
// 28 s on 1,500 of them.
type objectFetcher struct {
	data   *ttcsnapshot.LocalDataStore
	remote *ttcsnapshot.ObjectRelayClient

	mu       sync.Mutex
	fetching map[ttcsnapshot.ObjectID]chan struct{}
}

func (f *objectFetcher) open(id ttcsnapshot.ObjectID, length int64) (*os.File, error) {
	for {
		file, err := f.data.Open(id, length)
		if err == nil || f.remote == nil || !errors.Is(err, fs.ErrNotExist) {
			return file, err
		}
		f.mu.Lock()
		if done, ok := f.fetching[id]; ok {
			f.mu.Unlock()
			<-done
			continue
		}
		if f.fetching == nil {
			f.fetching = make(map[ttcsnapshot.ObjectID]chan struct{})
		}
		done := make(chan struct{})
		f.fetching[id] = done
		f.mu.Unlock()
		file, err = f.fetch(id, length)
		f.mu.Lock()
		delete(f.fetching, id)
		f.mu.Unlock()
		close(done)
		return file, err
	}
}

// fetch fetches, installs and opens one object.
func (f *objectFetcher) fetch(id ttcsnapshot.ObjectID, length int64) (*os.File, error) {
	data, err := f.remote.Fetch(id, uint64(length))
	if err != nil {
		return nil, err
	}
	// The node's daemon sweeps the store of the objects its graph does not
	// hold, as it does not hold this one. Install and open it under the
	// store's fence, so that a sweep takes neither the temporary file it is
	// written to nor the object before it is open: once one had, loading the
	// page failed, "materializing sparse dehydrated VDSO parameter page: I/O
	// error". A sweep may take it after; a reader that finds it gone fetches
	// it again.
	var file *os.File
	err = f.data.WithGraphMutation(func() error {
		if err := f.data.Install(id, bytes.NewReader(data), length); err != nil {
			return err
		}
		var openErr error
		file, openErr = f.data.Open(id, length)
		return openErr
	})
	return file, err
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
	return &GraphChunkReader{data: data, chunks: chunks, remote: remote, ownData: ownData, fetcher: objectFetcher{data: data, remote: remote}}, nil
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
	return r.fetcher.open(id, int64(pgalloc.SparseChunkBytes))
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
