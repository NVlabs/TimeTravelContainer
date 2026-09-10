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

func encodeMemoryPageOffset(offset uint32) []byte {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], offset)
	return data[:]
}

// MemoryPageOffset returns the byte offset of page in its immutable 1-MiB
// chunk. The page object contains no bulk bytes of its own.
func (s *Store) MemoryPageOffset(page ObjectID) (MemoryChunkID, uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[page.key()]
	if !ok || object.ID.Kind != ObjectKindPage || len(object.Data) != 4 || len(object.Children) != 1 || object.Children[0].Slot != 0 || object.Children[0].Child.Kind != ObjectKindMemoryChunk {
		return MemoryChunkID{}, 0, fmt.Errorf("object %s is not a chunk-backed memory page", page.key())
	}
	offset := binary.BigEndian.Uint32(object.Data)
	chunk := s.state.Objects[object.Children[0].Child.key()]
	if uint64(offset)+memoryPageBytes > chunk.BulkLength || offset%memoryPageBytes != 0 {
		return MemoryChunkID{}, 0, fmt.Errorf("page %s has invalid chunk offset %d", page.key(), offset)
	}
	return MemoryChunkID(object.Children[0].Child), offset, nil
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
		page, err := mutation.create(ObjectKindPage, encodeMemoryPageOffset(uint32(slot)*memoryPageBytes), []Edge{{Slot: 0, Child: chunk}})
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
