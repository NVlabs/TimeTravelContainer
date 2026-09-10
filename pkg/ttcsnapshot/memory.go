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
	"fmt"
	"math"
	"sort"
)

// Six radix levels cover 48 page-index bits, or 60 virtual-address bits with
// 4-KiB base pages. The height and maximum work per mutation are fixed.
const memoryTreeLevels = 6

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
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if pageIndex >= uint64(1)<<(8*memoryTreeLevels) {
		return ObjectID{}, fmt.Errorf("memory page index %#x exceeds radix coverage", pageIndex)
	}
	if page.Kind != ObjectKindPage {
		return ObjectID{}, fmt.Errorf("memory leaf %s is not a page", page.key())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok || snapshot.State != SnapshotWorking {
		return ObjectID{}, fmt.Errorf("snapshot %d is not working", origin)
	}
	pageObject, ok := s.state.Objects[page.key()]
	if !ok || pageObject.State != ObjectLive {
		return ObjectID{}, fmt.Errorf("page object %s is not live", page.key())
	}
	if snapshot.NextOrdinal > math.MaxUint64-memoryTreeLevels {
		return ObjectID{}, fmt.Errorf("snapshot %d object ordinal space exhausted", origin)
	}

	// Capture the old nodes on the affected path. Missing suffixes are empty.
	var oldPath [memoryTreeLevels]*Object
	if current, exists := s.state.Roots[rootName]; exists {
		for level := memoryTreeLevels - 1; level >= 0; level-- {
			object, ok := s.state.Objects[current.key()]
			if !ok {
				return ObjectID{}, fmt.Errorf("memory root references missing object %s", current.key())
			}
			if err := validateMemoryNode(object, level); err != nil {
				return ObjectID{}, err
			}
			copy := cloneObject(object)
			oldPath[level] = &copy
			child, ok := edgeChild(object.Children, memorySlot(pageIndex, level))
			if !ok || level == 0 {
				break
			}
			current = child
		}
	}

	next := cloneGraphState(s.state)
	touched := make(map[string]struct{})
	retain := func(id ObjectID) error {
		object, ok := next.Objects[id.key()]
		if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("child object %s cannot be retained", id.key())
		}
		object.Refs++
		next.Objects[id.key()] = object
		touched[id.key()] = struct{}{}
		return nil
	}

	child := page
	for level := 0; level < memoryTreeLevels; level++ {
		var children []Edge
		if oldPath[level] != nil {
			children = oldPath[level].Children
		}
		children = replaceEdge(children, memorySlot(pageIndex, level), child)
		id := ObjectID{Snapshot: origin, Ordinal: snapshot.NextOrdinal, Kind: ObjectKindMemoryNode}
		snapshot.NextOrdinal++
		node := Object{ID: id, State: ObjectLive, Data: []byte{byte(level)}, Children: children}
		if err := validateObject(node); err != nil {
			return ObjectID{}, err
		}
		for _, edge := range children {
			if err := retain(edge.Child); err != nil {
				return ObjectID{}, err
			}
		}
		next.Objects[id.key()] = node
		touched[id.key()] = struct{}{}
		child = id
	}
	newRoot := child
	if err := retain(newRoot); err != nil {
		return ObjectID{}, err
	}
	if oldRoot, exists := next.Roots[rootName]; exists {
		object, err := releaseObject(next.Objects[oldRoot.key()])
		if err != nil {
			return ObjectID{}, err
		}
		next.Objects[oldRoot.key()] = object
		touched[oldRoot.key()] = struct{}{}
	}
	next.Roots[rootName] = newRoot
	next.Snapshots[snapshotKey(origin)] = snapshot
	if err := validateGraphState(&next); err != nil {
		return ObjectID{}, err
	}

	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ops := make([]journalOp, 0, len(keys)+2)
	for _, key := range keys {
		object := next.Objects[key]
		ops = append(ops, journalOp{PutObject: &object})
	}
	ops = append(ops, journalOp{PutSnapshot: &snapshot}, journalOp{RootName: rootName, PutRoot: &newRoot})
	if err := s.commitLocked(ops); err != nil {
		return ObjectID{}, err
	}
	return newRoot, nil
}
