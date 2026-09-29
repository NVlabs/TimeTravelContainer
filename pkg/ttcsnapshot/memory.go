// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"math/bits"
	"slices"
	"sort"
)

// A branch's memory is one tree per MemoryFile, which maps the file's page
// indexes (offset / 4 KiB) to the bytes and identities of its pages. Five
// levels of 256-way MemoryNode objects lead to MemoryTable objects, each of
// which describes 256 consecutive page indexes, 1 MiB of the file: for every
// page mapped there, which bulk memory chunk holds its bytes and where, and
// the identity the guest's memory manager gave it. A table's children are the
// chunks it refers to.
//
// Branches share memory a table at a time: a fork that has not written to a
// megabyte maps the same table as its source. A table that differs in one
// page is a new table, whose other pages still share their bytes.
//
// Graphs written before tables existed map each page index, through a sixth
// level of MemoryNode, to a Page object that records one page. Those trees are
// read, and their pages shared, but never written.
const (
	memoryTreeLevels = 6
	// MemoryChunkBytes is the maximum cold transfer and storage unit. Resident
	// pages retain independent 4-KiB identities and CoW protection.
	MemoryChunkBytes = 1 << 20
	memoryPageBytes  = 1 << 12
	pagesPerChunk    = MemoryChunkBytes / memoryPageBytes
	// PagesPerMemoryTable is the number of page indexes a table describes.
	PagesPerMemoryTable = 256
)

// MemoryPageIdentity preserves the guest memory manager's stable logical page
// and content-version identities across dehydration. Logical identity remains
// unchanged by CoW; Version changes when one branch writes the page.
// References is deliberately part of it: it is branch-scoped restore
// metadata, so pages whose counts differ may not be shared.
type MemoryPageIdentity struct {
	LogicalAllocator [16]byte
	LogicalSequence  uint64
	VersionAllocator [16]byte
	VersionSequence  uint64
	References       uint64
}

func (identity MemoryPageIdentity) complete() bool {
	return identity.LogicalAllocator != ([16]byte{}) && identity.VersionAllocator != ([16]byte{}) && identity.References != 0
}

// MemoryPageLocation is where a page's bytes are: page Page of the bulk memory
// chunk Chunk. The zero location is nowhere.
type MemoryPageLocation struct {
	Chunk ObjectID
	Page  uint8
}

// MemoryPageEntry is one page a memory tree maps.
type MemoryPageEntry struct {
	Index    uint64
	Location MemoryPageLocation
	Identity MemoryPageIdentity
}

// MemoryTableEntry is one page of a table: its slot among the table's page
// indexes, where its bytes are, and its identity.
type MemoryTableEntry struct {
	Slot     uint8
	Location MemoryPageLocation
	Identity MemoryPageIdentity
}

// NewMemoryTable is a table to map in a memory tree: the tree's root name,
// the first page index the table describes, a multiple of
// PagesPerMemoryTable, and its pages in ascending slot order.
type NewMemoryTable struct {
	Root    string
	Index   uint64
	Entries []MemoryTableEntry
}

// A table's data is a format byte, a bitmap of the slots it maps, and then,
// for each of those in order, the index of its chunk among the table's
// children, its page in that chunk, and its identity. The children are the
// distinct chunks in ascending ID order.
const (
	memoryTableFormat     = 1
	memoryTableHeaderSize = 1 + PagesPerMemoryTable/8
	memoryTableEntrySize  = 1 + 1 + 16 + 8 + 16 + 8 + 8
)

func compareObjectIDs(a, b ObjectID) int {
	switch {
	case a.Snapshot != b.Snapshot:
		return cmp.Compare(a.Snapshot, b.Snapshot)
	case a.Kind != b.Kind:
		return cmp.Compare(a.Kind, b.Kind)
	default:
		return cmp.Compare(a.Ordinal, b.Ordinal)
	}
}

// encodeMemoryTable returns the data and children of the table of entries,
// which are in ascending slot order.
func encodeMemoryTable(entries []MemoryTableEntry) ([]byte, []Edge, error) {
	if len(entries) == 0 || len(entries) > PagesPerMemoryTable {
		return nil, nil, fmt.Errorf("a memory table of %d pages", len(entries))
	}
	var chunks []ObjectID
	for index, entry := range entries {
		if index != 0 && entry.Slot <= entries[index-1].Slot {
			return nil, nil, fmt.Errorf("memory table slot %d follows slot %d", entry.Slot, entries[index-1].Slot)
		}
		if entry.Location.Chunk.Kind != ObjectKindMemoryChunk || entry.Location.Chunk.validate() != nil {
			return nil, nil, fmt.Errorf("memory table slot %d is in %s, not a memory chunk", entry.Slot, entry.Location.Chunk.key())
		}
		if !entry.Identity.complete() {
			return nil, nil, fmt.Errorf("memory table slot %d has an incomplete identity", entry.Slot)
		}
		chunks = append(chunks, entry.Location.Chunk)
	}
	slices.SortFunc(chunks, compareObjectIDs)
	chunks = slices.Compact(chunks)
	if len(chunks) > maxObjectEdges {
		return nil, nil, fmt.Errorf("a memory table refers to %d chunks", len(chunks))
	}
	data := make([]byte, memoryTableHeaderSize, memoryTableHeaderSize+len(entries)*memoryTableEntrySize)
	data[0] = memoryTableFormat
	for _, entry := range entries {
		data[1+entry.Slot/8] |= 1 << (entry.Slot % 8)
		edge, _ := slices.BinarySearchFunc(chunks, entry.Location.Chunk, compareObjectIDs)
		data = append(data, byte(edge), entry.Location.Page)
		data = append(data, entry.Identity.LogicalAllocator[:]...)
		data = binary.BigEndian.AppendUint64(data, entry.Identity.LogicalSequence)
		data = append(data, entry.Identity.VersionAllocator[:]...)
		data = binary.BigEndian.AppendUint64(data, entry.Identity.VersionSequence)
		data = binary.BigEndian.AppendUint64(data, entry.Identity.References)
	}
	children := make([]Edge, len(chunks))
	for index, chunk := range chunks {
		children[index] = Edge{Slot: uint16(index), Child: chunk}
	}
	return data, children, nil
}

// decodeMemoryTable returns the entries of table in ascending slot order.
func decodeMemoryTable(table Object) ([]MemoryTableEntry, error) {
	data := table.Data
	if table.ID.Kind != ObjectKindMemoryTable || len(data) < memoryTableHeaderSize || data[0] != memoryTableFormat {
		return nil, fmt.Errorf("object %s is not a memory table", table.ID.key())
	}
	count := 0
	for _, b := range data[1:memoryTableHeaderSize] {
		count += bits.OnesCount8(b)
	}
	if count == 0 || len(data) != memoryTableHeaderSize+count*memoryTableEntrySize {
		return nil, fmt.Errorf("memory table %s has %d bytes for %d pages", table.ID.key(), len(data), count)
	}
	if len(table.Children) == 0 {
		return nil, fmt.Errorf("memory table %s refers to no chunk", table.ID.key())
	}
	for index, edge := range table.Children {
		if int(edge.Slot) != index || edge.Child.Kind != ObjectKindMemoryChunk {
			return nil, fmt.Errorf("memory table %s child %d is %s in slot %d", table.ID.key(), index, edge.Child.key(), edge.Slot)
		}
	}
	entries := make([]MemoryTableEntry, 0, count)
	offset := memoryTableHeaderSize
	for slot := 0; slot < PagesPerMemoryTable; slot++ {
		if data[1+slot/8]&(1<<(slot%8)) == 0 {
			continue
		}
		record := data[offset : offset+memoryTableEntrySize]
		offset += memoryTableEntrySize
		if int(record[0]) >= len(table.Children) {
			return nil, fmt.Errorf("memory table %s slot %d refers to child %d of %d", table.ID.key(), slot, record[0], len(table.Children))
		}
		entry := MemoryTableEntry{
			Slot:     uint8(slot),
			Location: MemoryPageLocation{Chunk: table.Children[record[0]].Child, Page: record[1]},
		}
		copy(entry.Identity.LogicalAllocator[:], record[2:18])
		entry.Identity.LogicalSequence = binary.BigEndian.Uint64(record[18:26])
		copy(entry.Identity.VersionAllocator[:], record[26:42])
		entry.Identity.VersionSequence = binary.BigEndian.Uint64(record[42:50])
		entry.Identity.References = binary.BigEndian.Uint64(record[50:58])
		if !entry.Identity.complete() {
			return nil, fmt.Errorf("memory table %s slot %d has an incomplete identity", table.ID.key(), slot)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// legacyMemoryPageRecordSize is the size of a Page object's record, from
// before tables: its byte offset in its chunk, and its identity.
const legacyMemoryPageRecordSize = 4 + 16 + 8 + 16 + 8 + 8

// decodeLegacyMemoryPage returns where page, a Page object, holds its bytes,
// and its identity.
func decodeLegacyMemoryPage(page Object) (MemoryPageLocation, MemoryPageIdentity, error) {
	if page.ID.Kind != ObjectKindPage || len(page.Children) != 1 || page.Children[0].Slot != 0 || page.Children[0].Child.Kind != ObjectKindMemoryChunk {
		return MemoryPageLocation{}, MemoryPageIdentity{}, fmt.Errorf("object %s is not a chunk-backed memory page", page.ID.key())
	}
	data := page.Data
	if len(data) != legacyMemoryPageRecordSize {
		return MemoryPageLocation{}, MemoryPageIdentity{}, fmt.Errorf("memory page %s record has %d bytes, want %d", page.ID.key(), len(data), legacyMemoryPageRecordSize)
	}
	offset := binary.BigEndian.Uint32(data[0:4])
	if offset%memoryPageBytes != 0 || offset >= MemoryChunkBytes {
		return MemoryPageLocation{}, MemoryPageIdentity{}, fmt.Errorf("memory page %s has chunk offset %d", page.ID.key(), offset)
	}
	var identity MemoryPageIdentity
	copy(identity.LogicalAllocator[:], data[4:20])
	identity.LogicalSequence = binary.BigEndian.Uint64(data[20:28])
	copy(identity.VersionAllocator[:], data[28:44])
	identity.VersionSequence = binary.BigEndian.Uint64(data[44:52])
	identity.References = binary.BigEndian.Uint64(data[52:60])
	return MemoryPageLocation{Chunk: page.Children[0].Child, Page: uint8(offset / memoryPageBytes)}, identity, nil
}

func validateMemoryNode(object Object, level int) error {
	if object.ID.Kind != ObjectKindMemoryNode || len(object.Data) != 1 || int(object.Data[0]) != level {
		return fmt.Errorf("object %s is not a level-%d memory node", object.ID.key(), level)
	}
	return nil
}

// memoryPageEntries enumerates, in ascending index order, the pages mapped by
// the memory tree at root, reading its objects with lookup. Every chunk it
// names is a whole memory chunk.
func memoryPageEntries(root ObjectID, lookup func(ObjectID) (Object, bool)) ([]MemoryPageEntry, error) {
	var entries []MemoryPageEntry
	chunks := make(map[ObjectID]struct{})
	checkChunk := func(chunk ObjectID) error {
		if _, ok := chunks[chunk]; ok {
			return nil
		}
		object, ok := lookup(chunk)
		if !ok || object.ID.Kind != ObjectKindMemoryChunk || object.BulkLength != MemoryChunkBytes {
			return fmt.Errorf("memory chunk %s is missing or incomplete", chunk.key())
		}
		chunks[chunk] = struct{}{}
		return nil
	}
	var visit func(ObjectID, int, uint64) error
	visit = func(id ObjectID, level int, prefix uint64) error {
		object, ok := lookup(id)
		if !ok {
			return fmt.Errorf("memory tree object %s does not exist", id.key())
		}
		if level == 0 && object.ID.Kind == ObjectKindMemoryTable {
			table, err := decodeMemoryTable(object)
			if err != nil {
				return err
			}
			for _, entry := range table {
				if err := checkChunk(entry.Location.Chunk); err != nil {
					return err
				}
				entries = append(entries, MemoryPageEntry{Index: prefix | uint64(entry.Slot), Location: entry.Location, Identity: entry.Identity})
			}
			return nil
		}
		if err := validateMemoryNode(object, level); err != nil {
			return err
		}
		for index, edge := range object.Children {
			if edge.Slot > 255 || (index != 0 && edge.Slot <= object.Children[index-1].Slot) {
				return fmt.Errorf("memory node %s has slot %d out of order", id.key(), edge.Slot)
			}
			child := prefix | uint64(edge.Slot)<<(8*level)
			if level != 0 {
				if err := visit(edge.Child, level-1, child); err != nil {
					return err
				}
				continue
			}
			page, ok := lookup(edge.Child)
			if !ok {
				return fmt.Errorf("memory page %s does not exist", edge.Child.key())
			}
			location, identity, err := decodeLegacyMemoryPage(page)
			if err != nil {
				return err
			}
			if err := checkChunk(location.Chunk); err != nil {
				return err
			}
			entries = append(entries, MemoryPageEntry{Index: child, Location: location, Identity: identity})
		}
		return nil
	}
	if err := visit(root, memoryTreeLevels-1, 0); err != nil {
		return nil, err
	}
	return entries, nil
}

// MemoryPageEntries enumerates, in ascending index order, the pages mapped by
// the memory tree at root. Rehydration necessarily visits each mapped page to
// reconstruct the guest MemoryFile; ordinary lookup and fork never call it.
func (s *Store) MemoryPageEntries(root ObjectID) ([]MemoryPageEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return memoryPageEntries(root, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id.key()]
		return object, ok
	})
}

// memoryPageHolding is a place the graph holds a page, and how many live
// tables and pages map it there.
type memoryPageHolding struct {
	location MemoryPageLocation
	holders  uint32
}

// memoryObjectIndexable reports whether object is a live, materialized table
// or page, whose pages the memory indexes know.
func memoryObjectIndexable(object Object) bool {
	return (object.ID.Kind == ObjectKindMemoryTable || object.ID.Kind == ObjectKindPage) &&
		object.State == ObjectLive && object.Refs != 0 && !object.Deferred
}

// memoryObjectIndexedAlike reports whether the memory indexes know before and
// after, two versions of an object, the same way.
func memoryObjectIndexedAlike(before, after Object) bool {
	if before.ID.Kind != ObjectKindMemoryTable && before.ID.Kind != ObjectKindPage {
		return true
	}
	return memoryObjectIndexable(before) == memoryObjectIndexable(after) &&
		bytes.Equal(before.Data, after.Data) && slices.Equal(before.Children, after.Children)
}

// memoryObjectPages returns the pages of a table or legacy page.
func memoryObjectPages(object Object) ([]MemoryTableEntry, error) {
	if object.ID.Kind == ObjectKindPage {
		location, identity, err := decodeLegacyMemoryPage(object)
		if err != nil {
			return nil, err
		}
		return []MemoryTableEntry{{Location: location, Identity: identity}}, nil
	}
	return decodeMemoryTable(object)
}

func (s *Store) indexMemoryObjectLocked(object Object) {
	if !memoryObjectIndexable(object) {
		return
	}
	pages, err := memoryObjectPages(object)
	if err != nil {
		return
	}
	for _, page := range pages {
		s.holdMemoryPageLocked(page.Identity, page.Location)
	}
	if object.ID.Kind == ObjectKindMemoryTable {
		key := s.memoryTableKey(object.Data, object.Children)
		s.memoryTables[key] = append(s.memoryTables[key], object.ID)
	}
}

func (s *Store) unindexMemoryObjectLocked(object Object) {
	if !memoryObjectIndexable(object) {
		return
	}
	pages, err := memoryObjectPages(object)
	if err != nil {
		return
	}
	for _, page := range pages {
		s.releaseMemoryPageLocked(page.Identity, page.Location)
	}
	if object.ID.Kind == ObjectKindMemoryTable {
		key := s.memoryTableKey(object.Data, object.Children)
		tables := slices.DeleteFunc(s.memoryTables[key], func(id ObjectID) bool { return id == object.ID })
		if len(tables) == 0 {
			delete(s.memoryTables, key)
		} else {
			s.memoryTables[key] = tables
		}
	}
}

func (s *Store) rebuildMemoryIndexesLocked() {
	s.memoryPages = make(map[MemoryPageIdentity]memoryPageHolding)
	s.memoryPagesElsewhere = make(map[MemoryPageIdentity][]memoryPageHolding)
	s.memoryTables = make(map[uint64][]ObjectID)
	for _, object := range s.state.Objects {
		s.indexMemoryObjectLocked(object)
	}
}

func (s *Store) holdMemoryPageLocked(identity MemoryPageIdentity, location MemoryPageLocation) {
	holding, ok := s.memoryPages[identity]
	if !ok {
		s.memoryPages[identity] = memoryPageHolding{location: location, holders: 1}
		return
	}
	if holding.location == location {
		holding.holders++
		s.memoryPages[identity] = holding
		return
	}
	elsewhere := s.memoryPagesElsewhere[identity]
	for index := range elsewhere {
		if elsewhere[index].location == location {
			elsewhere[index].holders++
			return
		}
	}
	s.memoryPagesElsewhere[identity] = append(elsewhere, memoryPageHolding{location: location, holders: 1})
}

func (s *Store) releaseMemoryPageLocked(identity MemoryPageIdentity, location MemoryPageLocation) {
	holding, ok := s.memoryPages[identity]
	if !ok {
		return
	}
	elsewhere := s.memoryPagesElsewhere[identity]
	if holding.location == location {
		if holding.holders > 1 {
			holding.holders--
			s.memoryPages[identity] = holding
			return
		}
		if len(elsewhere) == 0 {
			delete(s.memoryPages, identity)
			return
		}
		s.memoryPages[identity] = elsewhere[0]
		elsewhere = elsewhere[1:]
	} else {
		for index := range elsewhere {
			if elsewhere[index].location != location {
				continue
			}
			if elsewhere[index].holders > 1 {
				elsewhere[index].holders--
			} else {
				elsewhere = slices.Delete(elsewhere, index, index+1)
			}
			break
		}
	}
	if len(elsewhere) == 0 {
		delete(s.memoryPagesElsewhere, identity)
	} else {
		s.memoryPagesElsewhere[identity] = elsewhere
	}
}

// memoryPageLocationLocked returns where the graph holds the page with
// identity in an origin whose state is in allowed, choosing the least chunk
// when there are several.
func (s *Store) memoryPageLocationLocked(identity MemoryPageIdentity, allowed map[SnapshotState]struct{}, origins map[SnapshotID]bool) (MemoryPageLocation, bool) {
	holding, ok := s.memoryPages[identity]
	if !ok {
		return MemoryPageLocation{}, false
	}
	var selected MemoryPageLocation
	found := false
	consider := func(candidate memoryPageHolding) {
		// An origin's state is found by a formatted key: remember it for
		// the many pages a capture looks up in the same few origins.
		origin := candidate.location.Chunk.Snapshot
		permitted, known := origins[origin]
		if !known {
			permitted = s.originAllowedLocked(origin, allowed)
			origins[origin] = permitted
		}
		if !permitted {
			return
		}
		if order := compareObjectIDs(candidate.location.Chunk, selected.Chunk); !found || order < 0 || (order == 0 && candidate.location.Page < selected.Page) {
			selected, found = candidate.location, true
		}
	}
	consider(holding)
	for _, candidate := range s.memoryPagesElsewhere[identity] {
		consider(candidate)
	}
	return selected, found
}

func (s *Store) originAllowedLocked(origin SnapshotID, allowed map[SnapshotState]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok {
		return false
	}
	_, ok = allowed[snapshot.State]
	return ok
}

func (s *Store) memoryTableKey(data []byte, children []Edge) uint64 {
	var hash maphash.Hash
	hash.SetSeed(s.memoryTableSeed)
	_, _ = hash.Write(data)
	var id [17]byte
	for _, edge := range children {
		binary.BigEndian.PutUint64(id[0:8], uint64(edge.Child.Snapshot))
		id[8] = byte(edge.Child.Kind)
		binary.BigEndian.PutUint64(id[9:17], edge.Child.Ordinal)
		_, _ = hash.Write(id[:])
	}
	return hash.Sum64()
}

// existingMemoryTableLocked returns a live table, in an origin whose state is
// in allowed, with exactly data and children.
func (s *Store) existingMemoryTableLocked(data []byte, children []Edge, allowed map[SnapshotState]struct{}) (ObjectID, bool) {
	for _, id := range s.memoryTables[s.memoryTableKey(data, children)] {
		table, ok := s.state.Objects[id.key()]
		if !ok || !memoryObjectIndexable(table) || !s.originAllowedLocked(id.Snapshot, allowed) {
			continue
		}
		if bytes.Equal(table.Data, data) && slices.Equal(table.Children, children) {
			return id, true
		}
	}
	return ObjectID{}, false
}

// reusableStateSet is the set of reusableStates; an empty set accepts every
// origin.
func reusableStateSet(reusableStates []SnapshotState) (map[SnapshotState]struct{}, error) {
	allowed := make(map[SnapshotState]struct{}, len(reusableStates))
	for _, state := range reusableStates {
		if !state.valid() {
			return nil, fmt.Errorf("invalid reusable memory snapshot state %q", state)
		}
		allowed[state] = struct{}{}
	}
	return allowed, nil
}

// HoldMemoryPages returns where the graph holds each page of identities, in
// an origin whose state is one of reusableStates (any live origin if none are
// given), and the zero location for those it does not hold. Every chunk it
// returns is pinned under pinPrefix in the same transaction, so that none is
// collected before a capture's tables refer to it; DeletePins removes the pins
// it returns.
func (s *Store) HoldMemoryPages(pinPrefix string, identities []MemoryPageIdentity, reusableStates ...SnapshotState) ([]MemoryPageLocation, []string, error) {
	if err := validateRootName(pinPrefix); err != nil {
		return nil, nil, err
	}
	allowed, err := reusableStateSet(reusableStates)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	locations := make([]MemoryPageLocation, len(identities))
	chunks := make(map[ObjectID]struct{})
	origins := make(map[SnapshotID]bool)
	for index, identity := range identities {
		if !identity.complete() {
			return nil, nil, fmt.Errorf("memory page %d has an incomplete identity", index)
		}
		location, ok := s.memoryPageLocationLocked(identity, allowed, origins)
		if !ok {
			continue
		}
		locations[index] = location
		chunks[location.Chunk] = struct{}{}
	}
	// A pin is named once per chunk, not per page: a chunk holds up to 256
	// of them.
	pins := make(map[string]ObjectID, len(chunks))
	for chunk := range chunks {
		pins[pinPrefix+"/"+chunk.key()] = chunk
	}
	if err := s.addPinsLocked(pins); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(pins))
	for name := range pins {
		names = append(names, name)
	}
	sort.Strings(names)
	return locations, names, nil
}

// CommitMemoryTables maps each of tables in its tree in the working snapshot
// origin, in one durable transaction. A table with the same pages as a live
// one, in an origin whose state is one of reusableStates (any live origin if
// none are given), shares it rather than being written again. chunks are the
// bulk chunks that tables refer to and the graph does not have yet: reserved
// in origin with ReserveObjects, installed, and made durable. Every chunk
// tables refer to must be one of them or be held, as by HoldMemoryPages.
func (s *Store) CommitMemoryTables(origin SnapshotID, tables []NewMemoryTable, chunks []ObjectID, reusableStates ...SnapshotState) error {
	if len(tables) == 0 {
		return errors.New("cannot commit an empty TTC memory table batch")
	}
	allowed, err := reusableStateSet(reusableStates)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if chunk.Kind != ObjectKindMemoryChunk {
			return fmt.Errorf("object %s is not a memory chunk", chunk.key())
		}
		if err := mutation.putReserved(Object{ID: chunk, State: ObjectLive, BulkLength: MemoryChunkBytes}); err != nil {
			return err
		}
	}
	trees := make(map[string]map[uint64]ObjectID)
	for _, table := range tables {
		if err := validateRootName(table.Root); err != nil {
			return err
		}
		if table.Index%PagesPerMemoryTable != 0 || table.Index >= uint64(1)<<(8*memoryTreeLevels) {
			return fmt.Errorf("memory table at page index %#x", table.Index)
		}
		data, children, err := encodeMemoryTable(table.Entries)
		if err != nil {
			return fmt.Errorf("memory table at page index %#x of %q: %w", table.Index, table.Root, err)
		}
		id, ok := s.existingMemoryTableLocked(data, children, allowed)
		if !ok {
			if id, err = mutation.create(ObjectKindMemoryTable, data, children); err != nil {
				return err
			}
		}
		tree := trees[table.Root]
		if tree == nil {
			tree = make(map[uint64]ObjectID)
			trees[table.Root] = tree
		}
		key := table.Index / PagesPerMemoryTable
		if _, ok := tree[key]; ok {
			return fmt.Errorf("memory table at page index %#x of %q twice", table.Index, table.Root)
		}
		tree[key] = id
	}
	roots := make(map[string]ObjectID, len(trees))
	for name, tree := range trees {
		keys := make([]uint64, 0, len(tree))
		for key := range tree {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		root, err := rewriteMemoryTree(mutation, memoryTreeLevels-1, s.state.Roots[name], keys, tree)
		if err != nil {
			return err
		}
		roots[name] = root
	}
	if err := mutation.setRoots(roots); err != nil {
		return err
	}
	return mutation.commitRoots(roots)
}

// rewriteMemoryTree path-copies the memory node at level, down to the nodes
// above tables, so that it leads to tables, keyed by page index / 256. keys
// are those keys in ascending order.
func rewriteMemoryTree(mutation *graphMutation, level int, old ObjectID, keys []uint64, tables map[uint64]ObjectID) (ObjectID, error) {
	var children []Edge
	if !old.IsZero() {
		object, ok := mutation.object(old)
		if !ok {
			return ObjectID{}, fmt.Errorf("memory tree references missing object %s", old.key())
		}
		if err := validateMemoryNode(object, level); err != nil {
			return ObjectID{}, err
		}
		children = object.Children
	}
	slotOf := func(key uint64) uint16 { return uint16((key >> (8 * (level - 1))) & 0xff) }
	var updates []Edge
	for begin := 0; begin < len(keys); {
		slot := slotOf(keys[begin])
		end := begin + 1
		for end < len(keys) && slotOf(keys[end]) == slot {
			end++
		}
		var child ObjectID
		if level == 1 {
			if end != begin+1 {
				return ObjectID{}, errors.New("duplicate TTC memory table index")
			}
			child = tables[keys[begin]]
		} else {
			oldChild, _ := edgeChild(children, slot)
			var err error
			if child, err = rewriteMemoryTree(mutation, level-1, oldChild, keys[begin:end], tables); err != nil {
				return ObjectID{}, err
			}
		}
		updates = append(updates, Edge{Slot: slot, Child: child})
		begin = end
	}
	return mutation.create(ObjectKindMemoryNode, []byte{byte(level)}, mergeEdges(children, updates))
}

func edgeChild(edges []Edge, slot uint16) (ObjectID, bool) {
	index := sort.Search(len(edges), func(i int) bool { return edges[i].Slot >= slot })
	if index == len(edges) || edges[index].Slot != slot {
		return ObjectID{}, false
	}
	return edges[index].Child, true
}

// mergeEdges returns edges with updates applied, both sorted by slot: an
// update replaces the edge in its slot, or adds one.
func mergeEdges(edges, updates []Edge) []Edge {
	result := make([]Edge, 0, len(edges)+len(updates))
	for len(edges) != 0 || len(updates) != 0 {
		switch {
		case len(updates) == 0 || (len(edges) != 0 && edges[0].Slot < updates[0].Slot):
			result = append(result, edges[0])
			edges = edges[1:]
		case len(edges) == 0 || updates[0].Slot < edges[0].Slot:
			result = append(result, updates[0])
			updates = updates[1:]
		default:
			result = append(result, updates[0])
			edges, updates = edges[1:], updates[1:]
		}
	}
	return result
}
