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

package ttcsnapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Six radix levels cover 48 page-index bits, or 60 virtual-address bits with
// 4-KiB base pages. The height and maximum work per mutation are fixed.
const (
	memoryTreeLevels = 6
	// MemoryChunkBytes is the maximum cold transfer and storage unit. Resident
	// pages retain independent 4-KiB identities and CoW protection.
	MemoryChunkBytes = 1 << 20
	memoryPageBytes  = 1 << 12
	pagesPerChunk    = MemoryChunkBytes / memoryPageBytes
)

// MemoryChunkReservation allocates stable IDs before cold bytes are installed.
// Pins are removed only after every branch memory root referencing Pages is
// committed.
type MemoryChunkReservation struct {
	Chunk      MemoryChunkID
	Pages      map[uint8]PageID
	Identities map[uint8]MemoryPageIdentity
	Pins       []string
}

// MemoryPageIdentity preserves the guest memory manager's stable logical page
// and content-version identities across dehydration. Logical identity remains
// unchanged by CoW; Version changes when one branch writes the page.
type MemoryPageIdentity struct {
	LogicalAllocator [16]byte
	LogicalSequence  uint64
	VersionAllocator [16]byte
	VersionSequence  uint64
	References       uint64
}

// MemoryPageDescriptor describes one independently-CoW base page in a bulk
// memory chunk.
type MemoryPageDescriptor struct {
	Slot     uint8
	Identity MemoryPageIdentity
}

// ExistingMemoryPages resolves already-live immutable pages by the stable
// guest logical/version identity captured by the memory manager. References
// is deliberately part of the key: it is branch-scoped restore metadata, so
// a page record with a different value may share bytes but cannot share the
// same PageID. Missing identities are omitted from the result.
func (s *Store) ExistingMemoryPages(pages []MemoryPageDescriptor) (map[uint8]PageID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[uint8]PageID, len(pages))
	seen := make(map[uint8]struct{}, len(pages))
	for _, descriptor := range pages {
		if _, ok := seen[descriptor.Slot]; ok {
			return nil, fmt.Errorf("memory page lookup repeats slot %d", descriptor.Slot)
		}
		seen[descriptor.Slot] = struct{}{}
		identity := descriptor.Identity
		if identity.LogicalAllocator == ([16]byte{}) || identity.VersionAllocator == ([16]byte{}) || identity.References == 0 {
			return nil, fmt.Errorf("memory page lookup slot %d has incomplete identity", descriptor.Slot)
		}
		var selected ObjectID
		for id := range s.memoryPages[identity] {
			if selected.IsZero() || id.key() < selected.key() {
				selected = id
			}
		}
		if !selected.IsZero() {
			result[descriptor.Slot] = PageID(selected)
		}
	}
	return result, nil
}

func memorySlot(pageIndex uint64, level int) uint16 {
	return uint16((pageIndex >> (8 * level)) & 0xff)
}

func edgeChild(edges []Edge, slot uint16) (ObjectID, bool) {
	index := sort.Search(len(edges), func(i int) bool { return edges[i].Slot >= slot })
	if index == len(edges) || edges[index].Slot != slot {
		return ObjectID{}, false
	}
	return edges[index].Child, true
}

func replaceEdge(edges []Edge, slot uint16, child ObjectID) []Edge {
	result := append([]Edge(nil), edges...)
	index := sort.Search(len(result), func(i int) bool { return result[i].Slot >= slot })
	if index < len(result) && result[index].Slot == slot {
		result[index].Child = child
		return result
	}
	result = append(result, Edge{})
	copy(result[index+1:], result[index:])
	result[index] = Edge{Slot: slot, Child: child}
	return result
}

func validateMemoryNode(object Object, level int) error {
	if object.ID.Kind != ObjectKindMemoryNode || len(object.Data) != 1 || int(object.Data[0]) != level {
		return fmt.Errorf("object %s is not a level-%d memory node", object.ID.key(), level)
	}
	return nil
}

const memoryPageRecordBytes = 4 + 16 + 8 + 16 + 8 + 8

func encodeMemoryPageRecord(offset uint32, identity MemoryPageIdentity) []byte {
	data := make([]byte, memoryPageRecordBytes)
	binary.BigEndian.PutUint32(data[0:4], offset)
	copy(data[4:20], identity.LogicalAllocator[:])
	binary.BigEndian.PutUint64(data[20:28], identity.LogicalSequence)
	copy(data[28:44], identity.VersionAllocator[:])
	binary.BigEndian.PutUint64(data[44:52], identity.VersionSequence)
	binary.BigEndian.PutUint64(data[52:60], identity.References)
	return data[:]
}

func decodeMemoryPageRecord(data []byte) (uint32, MemoryPageIdentity, error) {
	if len(data) != memoryPageRecordBytes {
		return 0, MemoryPageIdentity{}, fmt.Errorf("memory page record has %d bytes, want %d", len(data), memoryPageRecordBytes)
	}
	var identity MemoryPageIdentity
	copy(identity.LogicalAllocator[:], data[4:20])
	identity.LogicalSequence = binary.BigEndian.Uint64(data[20:28])
	copy(identity.VersionAllocator[:], data[28:44])
	identity.VersionSequence = binary.BigEndian.Uint64(data[44:52])
	identity.References = binary.BigEndian.Uint64(data[52:60])
	return binary.BigEndian.Uint32(data[0:4]), identity, nil
}

// MemoryPageRecord returns the chunk coordinate and stable logical identity
// of a graph-native page.
func (s *Store) MemoryPageRecord(page ObjectID) (MemoryChunkID, uint32, MemoryPageIdentity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[page.key()]
	if !ok || object.ID.Kind != ObjectKindPage || len(object.Children) != 1 || object.Children[0].Slot != 0 || object.Children[0].Child.Kind != ObjectKindMemoryChunk {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("object %s is not a chunk-backed memory page", page.key())
	}
	offset, identity, err := decodeMemoryPageRecord(object.Data)
	if err != nil {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("page %s: %w", page.key(), err)
	}
	chunk := s.state.Objects[object.Children[0].Child.key()]
	if uint64(offset)+memoryPageBytes > chunk.BulkLength || offset%memoryPageBytes != 0 {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("page %s has invalid chunk offset %d", page.key(), offset)
	}
	return MemoryChunkID(object.Children[0].Child), offset, identity, nil
}

// MemoryPageOffset returns the byte offset of page in its immutable 1-MiB
// chunk. The page object contains no bulk bytes of its own.
func (s *Store) MemoryPageOffset(page ObjectID) (MemoryChunkID, uint32, error) {
	chunk, offset, _, err := s.MemoryPageRecord(page)
	return chunk, offset, err
}

// ReserveMemoryChunk allocates one bulk chunk ID and one PageID per selected
// slot in a single journal transaction. Slots are canonicalized so retries
// and callers have deterministic page ordering.
func (s *Store) ReserveMemoryChunk(origin SnapshotID, pinPrefix string, slots []uint8) (MemoryChunkReservation, error) {
	descriptors := make([]MemoryPageDescriptor, len(slots))
	for index, slot := range slots {
		descriptors[index].Slot = slot
	}
	return s.ReserveMemoryChunkPages(origin, pinPrefix, descriptors)
}

// ReserveMemoryChunkPages is ReserveMemoryChunk with stable guest page
// identities. The descriptors are persisted only by CommitMemoryChunk, after
// the corresponding bulk object has been installed.
func (s *Store) ReserveMemoryChunkPages(origin SnapshotID, pinPrefix string, pages []MemoryPageDescriptor) (MemoryChunkReservation, error) {
	if err := validateRootName(pinPrefix); err != nil {
		return MemoryChunkReservation{}, err
	}
	if len(pages) == 0 || len(pages) > pagesPerChunk {
		return MemoryChunkReservation{}, errors.New("invalid TTC memory chunk slots")
	}
	ordered := append([]MemoryPageDescriptor(nil), pages...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Slot < ordered[j].Slot })
	for index := 1; index < len(ordered); index++ {
		if ordered[index].Slot == ordered[index-1].Slot {
			return MemoryChunkReservation{}, fmt.Errorf("memory chunk repeats page slot %d", ordered[index].Slot)
		}
	}
	pins := make([]string, 0, len(ordered)+1)
	pins = append(pins, pinPrefix+"/chunk")
	for _, page := range ordered {
		pins = append(pins, fmt.Sprintf("%s/page-%02x", pinPrefix, page.Slot))
	}
	for _, pin := range pins {
		if err := validateRootName(pin); err != nil {
			return MemoryChunkReservation{}, err
		}
	}
	kinds := make([]ObjectKind, len(ordered)+1)
	kinds[0] = ObjectKindMemoryChunk
	for index := range ordered {
		kinds[index+1] = ObjectKindPage
	}
	ids, err := s.ReserveObjects(origin, kinds)
	if err != nil {
		return MemoryChunkReservation{}, err
	}
	reservation := MemoryChunkReservation{
		Chunk:      MemoryChunkID(ids[0]),
		Pages:      make(map[uint8]PageID, len(ordered)),
		Identities: make(map[uint8]MemoryPageIdentity, len(ordered)),
		Pins:       pins,
	}
	for index, page := range ordered {
		reservation.Pages[page.Slot] = PageID(ids[index+1])
		reservation.Identities[page.Slot] = page.Identity
	}
	return reservation, nil
}

// CommitMemoryChunk publishes an already-installed fixed-size chunk and all
// of its page-to-offset records atomically under temporary construction pins.
func (s *Store) CommitMemoryChunk(reservation MemoryChunkReservation) error {
	chunk := ObjectID(reservation.Chunk)
	if chunk.Kind != ObjectKindMemoryChunk || len(reservation.Pages) == 0 || len(reservation.Pins) != len(reservation.Pages)+1 {
		return errors.New("invalid TTC memory chunk reservation")
	}
	slots := make([]int, 0, len(reservation.Pages))
	for slot := range reservation.Pages {
		slots = append(slots, int(slot))
	}
	sort.Ints(slots)
	objects := make([]ReservedObject, 0, len(slots)+1)
	objects = append(objects, ReservedObject{ID: chunk, Pin: reservation.Pins[0], BulkLength: MemoryChunkBytes})
	for index, slot := range slots {
		page := ObjectID(reservation.Pages[uint8(slot)])
		if page.Kind != ObjectKindPage {
			return fmt.Errorf("reserved memory slot %d has non-page object %s", slot, page.key())
		}
		objects = append(objects, ReservedObject{
			ID: page, Pin: reservation.Pins[index+1],
			Data:     encodeMemoryPageRecord(uint32(slot)*memoryPageBytes, reservation.Identities[uint8(slot)]),
			Children: []Edge{{Slot: 0, Child: chunk}},
		})
	}
	return s.CommitReservedObjects(objects)
}

// MemoryPage resolves pageIndex through root without following snapshot
// ancestry. The returned false means that the page is absent.
func (s *Store) MemoryPage(root ObjectID, pageIndex uint64) (ObjectID, bool, error) {
	if pageIndex >= uint64(1)<<(8*memoryTreeLevels) {
		return ObjectID{}, false, fmt.Errorf("memory page index %#x exceeds radix coverage", pageIndex)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	current := root
	for level := memoryTreeLevels - 1; level >= 0; level-- {
		node, ok := s.state.Objects[current.key()]
		if !ok {
			return ObjectID{}, false, fmt.Errorf("memory node %s does not exist", current.key())
		}
		if err := validateMemoryNode(node, level); err != nil {
			return ObjectID{}, false, err
		}
		child, ok := edgeChild(node.Children, memorySlot(pageIndex, level))
		if !ok {
			return ObjectID{}, false, nil
		}
		if level == 0 {
			if child.Kind != ObjectKindPage {
				return ObjectID{}, false, fmt.Errorf("memory leaf references non-page object %s", child.key())
			}
			return child, true, nil
		}
		current = child
	}
	panic("unreachable")
}

// MemoryPages enumerates the page-index-to-object mapping represented by a
// radix root. Rehydration necessarily visits each mapped page to reconstruct
// the guest MemoryFile; ordinary lookup and fork never call this method.
func (s *Store) MemoryPages(root ObjectID) (map[uint64]ObjectID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[uint64]ObjectID)
	var visit func(ObjectID, int, uint64) error
	visit = func(id ObjectID, level int, prefix uint64) error {
		object, ok := s.state.Objects[id.key()]
		if !ok {
			return fmt.Errorf("memory node %s does not exist", id.key())
		}
		if err := validateMemoryNode(object, level); err != nil {
			return err
		}
		for _, edge := range object.Children {
			if edge.Slot > 255 {
				return fmt.Errorf("memory node %s has invalid slot %d", id.key(), edge.Slot)
			}
			index := prefix | uint64(edge.Slot)<<(8*level)
			if level == 0 {
				if edge.Child.Kind != ObjectKindPage {
					return fmt.Errorf("memory leaf references non-page object %s", edge.Child.key())
				}
				if _, exists := result[index]; exists {
					return fmt.Errorf("memory radix repeats page index %#x", index)
				}
				result[index] = edge.Child
				continue
			}
			if err := visit(edge.Child, level-1, index); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root, memoryTreeLevels-1, 0); err != nil {
		return nil, err
	}
	return result, nil
}

// SetMemoryPage path-copies a fixed-height radix path and atomically replaces
// rootName. It creates exactly memoryTreeLevels metadata nodes regardless of
// memory size, snapshot ancestry, or the number of mapped pages.
func (s *Store) SetMemoryPage(origin SnapshotID, rootName string, pageIndex uint64, page ObjectID) (ObjectID, error) {
	return s.SetMemoryPages(origin, rootName, map[uint64]ObjectID{pageIndex: page})
}

// SetMemoryPages atomically path-copies the union of all affected radix
// paths. Its work is proportional to the changed prefixes, not graph size.
// In particular, 256 aligned pages require one leaf and five ancestor nodes.
func (s *Store) SetMemoryPages(origin SnapshotID, rootName string, pages map[uint64]ObjectID) (ObjectID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if len(pages) == 0 {
		return ObjectID{}, errors.New("empty TTC memory-page transaction")
	}
	indices := make([]uint64, 0, len(pages))
	for pageIndex, page := range pages {
		if pageIndex >= uint64(1)<<(8*memoryTreeLevels) {
			return ObjectID{}, fmt.Errorf("memory page index %#x exceeds radix coverage", pageIndex)
		}
		if page.Kind != ObjectKindPage {
			return ObjectID{}, fmt.Errorf("memory leaf %s is not a page", page.key())
		}
		indices = append(indices, pageIndex)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })

	s.mu.Lock()
	defer s.mu.Unlock()
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return ObjectID{}, err
	}
	for _, page := range pages {
		pageObject, ok := mutation.object(page)
		if !ok || pageObject.State != ObjectLive {
			return ObjectID{}, fmt.Errorf("page object %s is not live", page.key())
		}
	}
	oldRoot := s.state.Roots[rootName]
	newRoot, err := rewriteMemoryRadix(mutation, memoryTreeLevels-1, oldRoot, indices, pages)
	if err != nil {
		return ObjectID{}, err
	}
	if err := mutation.setRoot(rootName, newRoot); err != nil {
		return ObjectID{}, err
	}
	if err := mutation.commit(rootName, newRoot); err != nil {
		return ObjectID{}, err
	}
	return newRoot, nil
}

func rewriteMemoryRadix(mutation *graphMutation, level int, oldRoot ObjectID, indices []uint64, pages map[uint64]ObjectID) (ObjectID, error) {
	var children []Edge
	if !oldRoot.IsZero() {
		object, ok := mutation.object(oldRoot)
		if !ok {
			return ObjectID{}, fmt.Errorf("memory radix references missing object %s", oldRoot.key())
		}
		if err := validateMemoryNode(object, level); err != nil {
			return ObjectID{}, err
		}
		children = object.Children
	}
	for begin := 0; begin < len(indices); {
		slot := memorySlot(indices[begin], level)
		end := begin + 1
		for end < len(indices) && memorySlot(indices[end], level) == slot {
			end++
		}
		var child ObjectID
		if level == 0 {
			if end != begin+1 {
				return ObjectID{}, errors.New("duplicate TTC memory page index")
			}
			child = pages[indices[begin]]
		} else {
			oldChild, _ := edgeChild(children, slot)
			var err error
			child, err = rewriteMemoryRadix(mutation, level-1, oldChild, indices[begin:end], pages)
			if err != nil {
				return ObjectID{}, err
			}
		}
		children = replaceEdge(children, slot, child)
		begin = end
	}
	return mutation.create(ObjectKindMemoryNode, []byte{byte(level)}, children)
}

// SetMemoryChunk creates independently reference-counted 4-KiB PageIDs over
// one immutable 1-MiB bulk record and installs the selected slots atomically.
// basePage must be aligned to the chunk boundary.
func (s *Store) SetMemoryChunk(origin SnapshotID, rootName string, basePage uint64, chunk ObjectID, slots []uint8) (ObjectID, []PageID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, nil, err
	}
	if basePage%pagesPerChunk != 0 || len(slots) == 0 || len(slots) > pagesPerChunk {
		return ObjectID{}, nil, errors.New("invalid TTC memory chunk page range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return ObjectID{}, nil, err
	}
	chunkObject, ok := mutation.object(chunk)
	if !ok || chunkObject.State != ObjectLive || chunk.Kind != ObjectKindMemoryChunk || chunkObject.BulkLength != MemoryChunkBytes {
		return ObjectID{}, nil, fmt.Errorf("memory chunk %s is not a complete live 1-MiB object", chunk.key())
	}
	seen := make(map[uint8]struct{}, len(slots))
	pages := make(map[uint64]ObjectID, len(slots))
	pageIDs := make([]PageID, 0, len(slots))
	for _, slot := range slots {
		if _, exists := seen[slot]; exists {
			return ObjectID{}, nil, fmt.Errorf("memory chunk repeats page slot %d", slot)
		}
		seen[slot] = struct{}{}
		page, err := mutation.create(ObjectKindPage, encodeMemoryPageRecord(uint32(slot)*memoryPageBytes, MemoryPageIdentity{}), []Edge{{Slot: 0, Child: chunk}})
		if err != nil {
			return ObjectID{}, nil, err
		}
		pages[basePage+uint64(slot)] = page
		pageIDs = append(pageIDs, PageID(page))
	}
	indices := make([]uint64, 0, len(pages))
	for index := range pages {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	newRoot, err := rewriteMemoryRadix(mutation, memoryTreeLevels-1, s.state.Roots[rootName], indices, pages)
	if err != nil {
		return ObjectID{}, nil, err
	}
	if err := mutation.setRoot(rootName, newRoot); err != nil {
		return ObjectID{}, nil, err
	}
	if err := mutation.commit(rootName, newRoot); err != nil {
		return ObjectID{}, nil, err
	}
	return newRoot, pageIDs, nil
}
