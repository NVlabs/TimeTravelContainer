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
	"math"
	"math/bits"
	"os"
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

// memoryTableEncoder encodes tables in room it reuses: the data and children
// of a table it encodes are the encoder's until it encodes the next.
type memoryTableEncoder struct {
	data     []byte
	children []Edge
	chunks   []ObjectID
}

// encode returns the data and children of the table of entries, which are in
// ascending slot order.
func (e *memoryTableEncoder) encode(entries []MemoryTableEntry) ([]byte, []Edge, error) {
	if len(entries) == 0 || len(entries) > PagesPerMemoryTable {
		return nil, nil, fmt.Errorf("a memory table of %d pages", len(entries))
	}
	chunks := e.chunks[:0]
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
		// Most pages are in the chunk of the page before.
		if len(chunks) == 0 || chunks[len(chunks)-1] != entry.Location.Chunk {
			chunks = append(chunks, entry.Location.Chunk)
		}
	}
	slices.SortFunc(chunks, compareObjectIDs)
	chunks = slices.Compact(chunks)
	e.chunks = chunks
	if len(chunks) > maxObjectEdges {
		return nil, nil, fmt.Errorf("a memory table refers to %d chunks", len(chunks))
	}
	data := slices.Grow(e.data[:0], memoryTableHeaderSize+len(entries)*memoryTableEntrySize)[:memoryTableHeaderSize]
	clear(data)
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
	children := slices.Grow(e.children[:0], len(chunks))
	for index, chunk := range chunks {
		children = append(children, Edge{Slot: uint16(index), Child: chunk})
	}
	e.data, e.children = data, children
	return data, children, nil
}

// decodeMemoryTable returns the entries of table in ascending slot order.
func decodeMemoryTable(table Object) ([]MemoryTableEntry, error) {
	return appendMemoryTable(nil, table)
}

// appendMemoryTable appends to entries those of table, in ascending slot
// order.
func appendMemoryTable(entries []MemoryTableEntry, table Object) ([]MemoryTableEntry, error) {
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
	entries = slices.Grow(entries, count)
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
	if err := visitMemoryPages(root, lookup, func(entry *MemoryPageEntry) error {
		entries = append(entries, *entry)
		return nil
	}); err != nil {
		return nil, err
	}
	return entries, nil
}

// visitMemoryPages calls visit with each page the memory tree at root maps,
// in ascending index order, reading its objects with lookup, until visit
// fails. Every chunk it names is a whole memory chunk. The entry is visit's
// only for the call.
func visitMemoryPages(root ObjectID, lookup func(ObjectID) (Object, bool), visit func(*MemoryPageEntry) error) error {
	chunks := make(map[ObjectID]struct{})
	// Most pages are in the chunk of the page before.
	var checked ObjectID
	checkChunk := func(chunk ObjectID) error {
		if chunk == checked && chunk != (ObjectID{}) {
			return nil
		}
		if _, ok := chunks[chunk]; !ok {
			object, ok := lookup(chunk)
			if !ok || object.ID.Kind != ObjectKindMemoryChunk || object.BulkLength != MemoryChunkBytes {
				return fmt.Errorf("memory chunk %s is missing or incomplete", chunk.key())
			}
			chunks[chunk] = struct{}{}
		}
		checked = chunk
		return nil
	}
	var (
		table []MemoryTableEntry
		entry MemoryPageEntry
	)
	var visitNode func(ObjectID, int, uint64) error
	visitNode = func(id ObjectID, level int, prefix uint64) error {
		object, ok := lookup(id)
		if !ok {
			return fmt.Errorf("memory tree object %s does not exist", id.key())
		}
		if level == 0 && object.ID.Kind == ObjectKindMemoryTable {
			var err error
			if table, err = appendMemoryTable(table[:0], object); err != nil {
				return err
			}
			for i := range table {
				page := &table[i]
				if err := checkChunk(page.Location.Chunk); err != nil {
					return err
				}
				entry = MemoryPageEntry{Index: prefix | uint64(page.Slot), Location: page.Location, Identity: page.Identity}
				if err := visit(&entry); err != nil {
					return err
				}
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
				if err := visitNode(edge.Child, level-1, child); err != nil {
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
			entry = MemoryPageEntry{Index: child, Location: location, Identity: identity}
			if err := visit(&entry); err != nil {
				return err
			}
		}
		return nil
	}
	return visitNode(root, memoryTreeLevels-1, 0)
}

// MemoryPageEntries enumerates, in ascending index order, the pages mapped by
// the memory tree at root. Rehydration necessarily visits each mapped page to
// reconstruct the guest MemoryFile; ordinary lookup and fork never call it.
func (s *Store) MemoryPageEntries(root ObjectID) ([]MemoryPageEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return memoryPageEntries(root, s.lookupObjectLocked)
}

// VisitMemoryPages calls visit with each page the memory tree at root maps,
// in ascending index order, until visit fails. The entry is visit's only for
// the call, and visit must not call s.
func (s *Store) VisitMemoryPages(root ObjectID, visit func(*MemoryPageEntry) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return visitMemoryPages(root, s.lookupObjectLocked, visit)
}

// lookupObjectLocked returns the object id names.
//
// Preconditions: s.mu is locked.
func (s *Store) lookupObjectLocked(id ObjectID) (Object, bool) {
	object, ok := s.state.Objects[id]
	return object, ok
}

// memoryPageHolding is a place the graph holds a page, and how many live
// tables and pages map it there.
type memoryPageHolding struct {
	location MemoryPageLocation
	holders  uint32
}

// memoryPageIndex is where the graph holds each page, by the stable
// logical/version identity the guest's memory manager gave it: see
// Store.memoryPages. It keeps the pages of each 256 logical pages of an
// allocator together, a block. Tables and captures list pages a block at a
// time, which then share one lookup of their block, and a search of it;
// a map of every page sent each lookup to its own place among tens of
// megabytes, which the processor's caches missed.
type memoryPageIndex struct {
	blocks map[memoryPageBlockKey]*memoryPageBlock
	// released is how many blocks have left blocks since it was made: see
	// compact.
	released int
}

// memoryPageBlockKey names a block: its allocator and the sequence of its
// first page, divided by memoryPageBlockPages.
type memoryPageBlockKey struct {
	allocator [16]byte
	block     uint64
}

// memoryPageBlockPages is the number of logical pages of a block.
const memoryPageBlockPages = 256

// memoryPageBlock is the places the graph holds pages of a block, by their
// slots, the pages' sequences in the block, then the rest of the pages'
// identities, then the places. Most slots have one.
type memoryPageBlock struct {
	records []memoryPageRecord
}

// memoryPageRecord is a place the graph holds a page of a block.
type memoryPageRecord struct {
	slot             uint8
	versionAllocator [16]byte
	versionSequence  uint64
	references       uint64
	holding          memoryPageHolding
}

func newMemoryPageIndex() *memoryPageIndex {
	return &memoryPageIndex{blocks: make(map[memoryPageBlockKey]*memoryPageBlock)}
}

// memoryPageRecordOf returns the block of the page with identity, and its
// record at location, which holds nothing.
func memoryPageRecordOf(identity *MemoryPageIdentity, location MemoryPageLocation) (memoryPageBlockKey, memoryPageRecord) {
	return memoryPageBlockKey{allocator: identity.LogicalAllocator, block: identity.LogicalSequence / memoryPageBlockPages},
		memoryPageRecord{
			slot: uint8(identity.LogicalSequence % memoryPageBlockPages), versionAllocator: identity.VersionAllocator,
			versionSequence: identity.VersionSequence, references: identity.References, holding: memoryPageHolding{location: location},
		}
}

// comparePage orders records by their pages.
func (r *memoryPageRecord) comparePage(other *memoryPageRecord) int {
	if c := cmp.Compare(r.slot, other.slot); c != 0 {
		return c
	}
	if c := bytes.Compare(r.versionAllocator[:], other.versionAllocator[:]); c != 0 {
		return c
	}
	if c := cmp.Compare(r.versionSequence, other.versionSequence); c != 0 {
		return c
	}
	return cmp.Compare(r.references, other.references)
}

// compare orders records as a block holds them: a page's by place, the
// place with no chunk first.
func (r *memoryPageRecord) compare(other *memoryPageRecord) int {
	if c := r.comparePage(other); c != 0 {
		return c
	}
	if c := compareObjectIDs(r.holding.location.Chunk, other.holding.location.Chunk); c != 0 {
		return c
	}
	return cmp.Compare(r.holding.location.Page, other.holding.location.Page)
}

// samePage reports whether records are of one page.
func (r *memoryPageRecord) samePage(other *memoryPageRecord) bool {
	return r.slot == other.slot && r.versionSequence == other.versionSequence && r.references == other.references &&
		r.versionAllocator == other.versionAllocator
}

// slot returns the records of slot, from begin to end.
func (block *memoryPageBlock) slot(slot uint8) (begin, end int) {
	records := block.records
	// Most blocks hold a record of each slot, at its index.
	if at := int(slot); at < len(records) && records[at].slot == slot && (at == 0 || records[at-1].slot < slot) {
		begin = at
	} else {
		begin = sort.Search(len(records), func(i int) bool { return records[i].slot >= slot })
	}
	end = begin
	for end < len(records) && records[end].slot == slot {
		end++
	}
	return begin, end
}

// search returns where record goes in block, and whether block has it
// there.
func (block *memoryPageBlock) search(record *memoryPageRecord) (int, bool) {
	records := block.records
	// Tables list most pages in slot order: they go last.
	if n := len(records); n == 0 || records[n-1].compare(record) < 0 {
		return n, false
	}
	begin, end := block.slot(record.slot)
	for at := begin; at < end; at++ {
		if c := records[at].compare(record); c >= 0 {
			return at, c == 0
		}
	}
	return end, false
}

// hold records that one more live table or page maps the page with identity
// at location.
func (index *memoryPageIndex) hold(identity *MemoryPageIdentity, location MemoryPageLocation) {
	key, record := memoryPageRecordOf(identity, location)
	block := index.blocks[key]
	if block == nil {
		block = &memoryPageBlock{}
		index.blocks[key] = block
	}
	at, found := block.search(&record)
	if found {
		block.records[at].holding.holders++
		return
	}
	record.holding.holders = 1
	block.records = slices.Insert(block.records, at, record)
}

// release records that one fewer live table or page maps the page with
// identity at location.
func (index *memoryPageIndex) release(identity *MemoryPageIdentity, location MemoryPageLocation) {
	key, record := memoryPageRecordOf(identity, location)
	block := index.blocks[key]
	if block == nil {
		return
	}
	at, found := block.search(&record)
	if !found {
		return
	}
	if block.records[at].holding.holders > 1 {
		block.records[at].holding.holders--
		return
	}
	block.records = slices.Delete(block.records, at, at+1)
	if len(block.records) == 0 {
		delete(index.blocks, key)
		index.released++
		index.compact()
	}
}

// holdings returns the records of the places the graph holds the page with
// identity, in order.
func (index *memoryPageIndex) holdings(identity *MemoryPageIdentity) []memoryPageRecord {
	key, probe := memoryPageRecordOf(identity, MemoryPageLocation{})
	block := index.blocks[key]
	if block == nil {
		return nil
	}
	begin, end := block.slot(probe.slot)
	for begin < end && !block.records[begin].samePage(&probe) {
		begin++
	}
	last := begin
	for last < end && block.records[last].samePage(&probe) {
		last++
	}
	return block.records[begin:last]
}

// memoryIndexCompaction is how many blocks must leave the page index, as
// many as it still holds at least, before compact makes it anew.
const memoryIndexCompaction = 1 << 4

// compact copies the blocks into a map of their present size once those that
// left it outnumber those left: a map keeps the room of its largest size,
// however few blocks it holds since. Each block that left pays for the copy
// of one that stays.
func (index *memoryPageIndex) compact() {
	if index.released < memoryIndexCompaction || index.released < len(index.blocks) {
		return
	}
	blocks := make(map[memoryPageBlockKey]*memoryPageBlock, len(index.blocks))
	for key, block := range index.blocks {
		blocks[key] = block
	}
	index.blocks, index.released = blocks, 0
}

// appendMemoryObjectPages appends to pages those of a table or legacy page.
func appendMemoryObjectPages(pages []MemoryTableEntry, object Object) ([]MemoryTableEntry, error) {
	if object.ID.Kind == ObjectKindPage {
		location, identity, err := decodeLegacyMemoryPage(object)
		if err != nil {
			return nil, err
		}
		return append(pages, MemoryTableEntry{Location: location, Identity: identity}), nil
	}
	return appendMemoryTable(pages, object)
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

// memoryObjectPagesLocked returns the pages of a table or legacy page,
// decoded where the next call decodes them.
//
// Preconditions: s.mu is locked.
func (s *Store) memoryObjectPagesLocked(object Object) ([]MemoryTableEntry, error) {
	pages, err := appendMemoryObjectPages(s.memoryPageScratch[:0], object)
	if err == nil {
		s.memoryPageScratch = pages
	}
	return pages, err
}

func (s *Store) indexMemoryObjectLocked(object Object) {
	if !memoryObjectIndexable(object) {
		return
	}
	pages, err := s.memoryObjectPagesLocked(object)
	if err != nil {
		return
	}
	for index := range pages {
		s.memoryPages.hold(&pages[index].Identity, pages[index].Location)
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
	pages, err := s.memoryObjectPagesLocked(object)
	if err != nil {
		return
	}
	for index := range pages {
		s.memoryPages.release(&pages[index].Identity, pages[index].Location)
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
	s.memoryPages = newMemoryPageIndex()
	s.memoryTables = make(map[uint64][]ObjectID)
	for _, object := range s.state.Objects {
		s.indexMemoryObjectLocked(object)
	}
}

// memoryPageLocationLocked returns where the graph holds the page with
// identity in an origin reuse allows, choosing the least chunk when there are
// several.
func (s *Store) memoryPageLocationLocked(identity MemoryPageIdentity, reuse reuseFilter, origins map[SnapshotID]bool) (MemoryPageLocation, bool) {
	for _, record := range s.memoryPages.holdings(&identity) {
		// An origin's state is found by a formatted key: remember it for
		// the many pages a capture looks up in the same few origins.
		origin := record.holding.location.Chunk.Snapshot
		permitted, known := origins[origin]
		if !known {
			permitted = s.originAllowedLocked(origin, reuse)
			origins[origin] = permitted
		}
		if permitted {
			return record.holding.location, true
		}
	}
	return MemoryPageLocation{}, false
}

func (s *Store) originAllowedLocked(origin SnapshotID, reuse reuseFilter) bool {
	if _, ok := reuse.origins[origin]; ok {
		return true
	}
	if len(reuse.states) == 0 {
		return true
	}
	snapshot, ok := s.state.Snapshots[origin]
	if !ok {
		return false
	}
	_, ok = reuse.states[snapshot.State]
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

// existingMemoryTableLocked returns a live table, in an origin reuse allows,
// with exactly data and children.
func (s *Store) existingMemoryTableLocked(data []byte, children []Edge, reuse reuseFilter) (ObjectID, bool) {
	for _, id := range s.memoryTables[s.memoryTableKey(data, children)] {
		table, ok := s.state.Objects[id]
		if !ok || !memoryObjectIndexable(table) || !s.originAllowedLocked(id.Snapshot, reuse) {
			continue
		}
		if bytes.Equal(table.Data, data) && slices.Equal(table.Children, children) {
			return id, true
		}
	}
	return ObjectID{}, false
}

// ErrObjectReclaiming is returned by AdoptSharedMemory for an object the graph
// let go of and has not collected yet: a record is not taken back once its
// count reached zero, so Collect it first.
var ErrObjectReclaiming = errors.New("TTC graph object is being reclaimed")

// MemoryReuse selects the origins whose pages and tables a capture may share:
// those whose state is one of States, any live origin if States is empty, and
// those of Origins whatever their state. A shared origin's objects are counted
// by its shared ledger, which knows nothing of the references a graph's own
// objects hold of them: a caller names one in Origins only while it holds a
// claim there on what the capture may share, as on what AdoptSharedMemory
// adopted of it.
type MemoryReuse struct {
	States  []SnapshotState
	Origins []SnapshotID
}

// reuseFilter is a MemoryReuse as sets: an empty states accepts every origin.
type reuseFilter struct {
	states  map[SnapshotState]struct{}
	origins map[SnapshotID]struct{}
}

func (reuse MemoryReuse) filter() (reuseFilter, error) {
	filter := reuseFilter{states: make(map[SnapshotState]struct{}, len(reuse.States))}
	for _, state := range reuse.States {
		if !state.valid() {
			return reuseFilter{}, fmt.Errorf("invalid reusable memory snapshot state %q", state)
		}
		filter.states[state] = struct{}{}
	}
	if len(reuse.Origins) != 0 {
		filter.origins = make(map[SnapshotID]struct{}, len(reuse.Origins))
		for _, origin := range reuse.Origins {
			filter.origins[origin] = struct{}{}
		}
	}
	return filter, nil
}

// HoldMemoryPages is HoldMemoryPagesReusing for the origins whose state is
// one of reusableStates, any live origin if none are given.
func (s *Store) HoldMemoryPages(pinPrefix string, identities []MemoryPageIdentity, reusableStates ...SnapshotState) ([]MemoryPageLocation, []string, error) {
	return s.HoldMemoryPagesReusing(pinPrefix, identities, MemoryReuse{States: reusableStates})
}

// HoldMemoryPagesReusing returns where the graph holds each page of
// identities, in an origin reuse selects, and the zero location for those it
// does not hold. Every chunk it returns is pinned under pinPrefix in the same
// transaction, so that none is collected before a capture's tables refer to
// it; DeletePins removes the pins it returns.
func (s *Store) HoldMemoryPagesReusing(pinPrefix string, identities []MemoryPageIdentity, reuse MemoryReuse) ([]MemoryPageLocation, []string, error) {
	if err := validateRootName(pinPrefix); err != nil {
		return nil, nil, err
	}
	allowed, err := reuse.filter()
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	locations := make([]MemoryPageLocation, len(identities))
	chunks := make(map[ObjectID]struct{})
	origins := make(map[SnapshotID]bool)
	// Most pages are in the chunk of the page before.
	var chunk ObjectID
	for index, identity := range identities {
		if !identity.complete() {
			return nil, nil, fmt.Errorf("memory page %d has an incomplete identity", index)
		}
		location, ok := s.memoryPageLocationLocked(identity, allowed, origins)
		if !ok {
			continue
		}
		locations[index] = location
		if location.Chunk != chunk {
			chunk = location.Chunk
			chunks[chunk] = struct{}{}
		}
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

// CommitMemoryTables is CommitMemoryTablesReusing for the origins whose state
// is one of reusableStates, any live origin if none are given.
func (s *Store) CommitMemoryTables(origin SnapshotID, tables []NewMemoryTable, chunks []ObjectID, reusableStates ...SnapshotState) error {
	return s.CommitMemoryTablesReusing(origin, tables, chunks, MemoryReuse{States: reusableStates})
}

// CommitMemoryTablesReusing maps each of tables in its tree in the working
// snapshot origin, in one durable transaction. A table with the same pages as
// a live one, in an origin reuse selects, shares it rather than being written
// again. chunks are the bulk chunks that tables refer to and the graph does not
// have yet: reserved in origin with ReserveObjects, installed, and made
// durable. Every chunk tables refer to must be one of them or be held, as by
// HoldMemoryPagesReusing.
func (s *Store) CommitMemoryTablesReusing(origin SnapshotID, tables []NewMemoryTable, chunks []ObjectID, reuse MemoryReuse) error {
	if len(tables) == 0 {
		return errors.New("cannot commit an empty TTC memory table batch")
	}
	allowed, err := reuse.filter()
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
	// Most tables are shared, and their encodings only looked up.
	var encoder memoryTableEncoder
	for _, table := range tables {
		if err := validateRootName(table.Root); err != nil {
			return err
		}
		if table.Index%PagesPerMemoryTable != 0 || table.Index >= uint64(1)<<(8*memoryTreeLevels) {
			return fmt.Errorf("memory table at page index %#x", table.Index)
		}
		data, children, err := encoder.encode(table.Entries)
		if err != nil {
			return fmt.Errorf("memory table at page index %#x of %q: %w", table.Index, table.Root, err)
		}
		id, ok := s.existingMemoryTableLocked(data, children, allowed)
		if !ok {
			if id, err = mutation.create(ObjectKindMemoryTable, bytes.Clone(data), slices.Clone(children)); err != nil {
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

// AdoptSharedMemory adds to the graph tables, memory tables of shared origins,
// and those of chunks that they refer to, as far as the graph lacks them, and
// pins each of tables under pinPrefix, in one transaction. The graph records
// their origins as shared, whose objects the shared ledger counts: it counts
// only the references it holds itself. A capture may then share the tables,
// and the pages in them, with MemoryReuse naming their origins, as it shares
// the node's own; the caller holds a claim at the shared ledger on what it
// adopts while the graph refers to it. Every chunk a table refers to must be
// one of chunks or be in the graph already. It returns the pins, which
// DeletePins removes.
func (s *Store) AdoptSharedMemory(pinPrefix string, tables, chunks []Object) ([]string, error) {
	if err := validateRootName(pinPrefix); err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, nil
	}
	offered := make(map[ObjectID]Object, len(chunks))
	for _, chunk := range chunks {
		if chunk.ID.Kind != ObjectKindMemoryChunk || chunk.BulkLength == 0 || chunk.BulkLength > MemoryChunkBytes || len(chunk.Data) != 0 || len(chunk.Children) != 0 || chunk.Deferred {
			return nil, fmt.Errorf("object %s is not a memory chunk", chunk.ID.key())
		}
		offered[chunk.ID] = chunk
	}
	sorted := slices.Clone(tables)
	slices.SortFunc(sorted, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	for index, table := range sorted {
		if index != 0 && table.ID == sorted[index-1].ID {
			return nil, fmt.Errorf("memory table %s is adopted twice", table.ID.key())
		}
		if _, err := decodeMemoryTable(table); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// added are the objects the graph lacks, with the references the
	// adoption gives them; touched, those it has, with theirs.
	added := make(map[ObjectID]Object)
	touched := make(map[ObjectID]Object)
	reference := func(id ObjectID) error {
		if object, ok := added[id]; ok {
			object.Refs++
			added[id] = object
			return nil
		}
		object, ok := touched[id]
		if !ok {
			if object, ok = s.state.Objects[id]; !ok {
				return os.ErrNotExist
			}
		}
		if object.State != ObjectLive {
			return fmt.Errorf("object %s: %w", id.key(), ErrObjectReclaiming)
		}
		if object.Refs == math.MaxUint64 {
			return fmt.Errorf("object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id] = object
		return nil
	}
	pins := make(map[string]ObjectID, len(sorted))
	names := make([]string, 0, len(sorted))
	for _, table := range sorted {
		name := pinPrefix + "/" + table.ID.key()
		if _, exists := s.state.Pins[name]; exists {
			return nil, fmt.Errorf("pin %q already exists", name)
		}
		pins[name] = table.ID
		names = append(names, name)
		if existing, ok := s.state.Objects[table.ID]; ok {
			if !bytes.Equal(existing.Data, table.Data) || !slices.Equal(existing.Children, table.Children) {
				return nil, fmt.Errorf("memory table %s differs from the graph's", table.ID.key())
			}
			if err := reference(table.ID); err != nil {
				return nil, err
			}
			continue
		}
		added[table.ID] = Object{ID: table.ID, State: ObjectLive, Refs: 1, Data: slices.Clone(table.Data), Children: slices.Clone(table.Children)}
		for _, edge := range table.Children {
			err := reference(edge.Child)
			if errors.Is(err, os.ErrNotExist) {
				chunk, ok := offered[edge.Child]
				if !ok {
					return nil, fmt.Errorf("memory table %s refers to chunk %s, which is not adopted", table.ID.key(), edge.Child.key())
				}
				added[edge.Child] = Object{ID: chunk.ID, State: ObjectLive, Refs: 1, BulkLength: chunk.BulkLength}
				err = nil
			}
			if err != nil {
				return nil, err
			}
		}
	}
	// The origins of what is added are shared: recorded so, or recorded
	// anew, each up to its greatest ordinal.
	next := make(map[SnapshotID]uint64)
	for id := range added {
		next[id.Snapshot] = max(next[id.Snapshot], id.Ordinal+1)
	}
	var ops []journalOp
	origins := make([]SnapshotID, 0, len(next))
	for origin := range next {
		origins = append(origins, origin)
	}
	slices.Sort(origins)
	nextSnapshot := s.state.NextSnapshot
	for _, origin := range origins {
		snapshot, ok := s.state.Snapshots[origin]
		if ok && snapshot.State != SnapshotShared {
			return nil, fmt.Errorf("snapshot %d is %s, not shared", origin, snapshot.State)
		}
		if ok && snapshot.NextOrdinal >= next[origin] {
			continue
		}
		snapshot = Snapshot{ID: origin, State: SnapshotShared, NextOrdinal: max(snapshot.NextOrdinal, next[origin])}
		ops = append(ops, journalOp{PutSnapshot: &snapshot})
		if origin >= nextSnapshot {
			nextSnapshot = origin + 1
		}
	}
	if nextSnapshot != s.state.NextSnapshot {
		ops = append(ops, journalOp{SetNextSnapshot: &nextSnapshot})
	}
	for _, objects := range []map[ObjectID]Object{added, touched} {
		ids := make([]ObjectID, 0, len(objects))
		for id := range objects {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, compareObjectIDs)
		for _, id := range ids {
			object := objects[id]
			ops = append(ops, journalOp{PutObject: &object})
		}
	}
	for _, name := range names {
		id := pins[name]
		ops = append(ops, journalOp{PinName: name, PutPin: &id})
	}
	if err := s.commitLocked(ops); err != nil {
		return nil, err
	}
	return names, nil
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
