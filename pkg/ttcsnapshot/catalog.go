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

import "fmt"

// MemoryCatalog is the immutable subset of a snapshot graph required to
// reconstruct graph-native memory. Store implements it for authoritative
// local state; ImmutableCatalog implements it for shared or peer metadata
// without creating a second reference-count authority.
type MemoryCatalog interface {
	MemoryPages(ObjectID) (map[uint64]ObjectID, error)
	MemoryPageRecord(ObjectID) (MemoryChunkID, uint32, MemoryPageIdentity, error)
}

// ImmutableCatalog is a structurally validated read-only object catalog. It
// intentionally contains neither roots nor reference counts: branch roots
// arrive in the fenced control record, and reference ownership remains at the
// origin provider (or shared ledger).
type ImmutableCatalog struct {
	objects map[string]Object
}

// NewImmutableCatalog constructs a read-only catalog from one or more
// immutable snapshot manifests. References are treated as authority metadata
// and ignored by this cache view.
func NewImmutableCatalog(objects []Object) (*ImmutableCatalog, error) {
	catalog := &ImmutableCatalog{objects: make(map[string]Object, len(objects))}
	for _, object := range objects {
		if err := validateObject(object); err != nil {
			return nil, err
		}
		key := object.ID.key()
		if _, ok := catalog.objects[key]; ok {
			return nil, fmt.Errorf("immutable catalog repeats object %s", key)
		}
		catalog.objects[key] = cloneObject(object)
	}
	return catalog, nil
}

// Object returns an immutable metadata record.
func (catalog *ImmutableCatalog) Object(id ObjectID) (Object, bool) {
	if catalog == nil {
		return Object{}, false
	}
	object, ok := catalog.objects[id.key()]
	return cloneObject(object), ok
}

// MemoryPages enumerates one fixed-height memory radix tree.
func (catalog *ImmutableCatalog) MemoryPages(root ObjectID) (map[uint64]ObjectID, error) {
	if catalog == nil {
		return nil, fmt.Errorf("nil immutable TTC catalog")
	}
	result := make(map[uint64]ObjectID)
	var visit func(ObjectID, int, uint64) error
	visit = func(id ObjectID, level int, prefix uint64) error {
		object, ok := catalog.objects[id.key()]
		if !ok {
			return fmt.Errorf("memory node %s does not exist in immutable catalog", id.key())
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

// MemoryPageRecord resolves one page descriptor to its immutable chunk.
func (catalog *ImmutableCatalog) MemoryPageRecord(page ObjectID) (MemoryChunkID, uint32, MemoryPageIdentity, error) {
	if catalog == nil {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("nil immutable TTC catalog")
	}
	object, ok := catalog.objects[page.key()]
	if !ok || object.ID.Kind != ObjectKindPage || len(object.Children) != 1 || object.Children[0].Slot != 0 || object.Children[0].Child.Kind != ObjectKindMemoryChunk {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("object %s is not a chunk-backed memory page", page.key())
	}
	offset, identity, err := decodeMemoryPageRecord(object.Data)
	if err != nil {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("page %s: %w", page.key(), err)
	}
	chunk, ok := catalog.objects[object.Children[0].Child.key()]
	if !ok || chunk.ID.Kind != ObjectKindMemoryChunk {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("page %s references missing memory chunk %s", page.key(), object.Children[0].Child.key())
	}
	if uint64(offset)+memoryPageBytes > chunk.BulkLength || offset%memoryPageBytes != 0 {
		return MemoryChunkID{}, 0, MemoryPageIdentity{}, fmt.Errorf("page %s has invalid chunk offset %d", page.key(), offset)
	}
	return MemoryChunkID(object.Children[0].Child), offset, identity, nil
}

var _ MemoryCatalog = (*Store)(nil)
var _ MemoryCatalog = (*ImmutableCatalog)(nil)
