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
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// MemoryCatalog is the immutable subset of a snapshot graph required to
// reconstruct graph-native memory. Store implements it for authoritative
// local state; ImmutableCatalog implements it for shared or peer metadata
// without creating a second reference-count authority.
type MemoryCatalog interface {
	MemoryPages(ObjectID) (map[uint64]ObjectID, error)
	MemoryPageRecord(ObjectID) (MemoryChunkID, uint32, MemoryPageIdentity, error)
}

// FilesystemCatalog is the immutable read surface needed by a graph-backed
// filesystem. Store implements it for authoritative local state and
// ImmutableCatalog implements it for peer/shared metadata without importing
// reference ownership into a cache.
type FilesystemCatalog interface {
	FilesystemInode(ObjectID, uint64) (InodeRecord, bool, error)
	FilesystemLookup(ObjectID, uint64, string) (DirectoryEntryRecord, bool, error)
	FilesystemDirectory(ObjectID, uint64) ([]DirectoryEntryRecord, error)
	FilesystemRecords(ObjectID) ([]InodeRecord, []DirectoryEntryRecord, error)
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

// ResolveImmutableCatalog loads immutable origin sections while walking only
// objects reachable from roots. Snapshot packs intentionally contain objects
// allocated by one origin; when a reachable edge crosses origins, load
// supplies that origin directly from its Raft-selected provider or shared
// pack. Each origin and object is visited at most once.
func ResolveImmutableCatalog(roots []ObjectID, load func(SnapshotID) ([]Object, error)) (*ImmutableCatalog, error) {
	if len(roots) == 0 || load == nil {
		return nil, errors.New("immutable TTC catalog resolution requires a root")
	}
	loaded := make(map[SnapshotID]struct{})
	all := make(map[string]Object)
	seen := make(map[ObjectID]struct{})
	pending := append([]ObjectID(nil), roots...)
	for len(pending) != 0 {
		index := len(pending) - 1
		id := pending[index]
		pending = pending[:index]
		if err := id.validate(); err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		object, ok := all[id.key()]
		if !ok {
			origin := id.Snapshot
			if _, ok := loaded[origin]; ok {
				return nil, fmt.Errorf("immutable TTC origin %d omits reachable object %s", origin, id)
			}
			objects, err := load(origin)
			if err != nil {
				return nil, fmt.Errorf("loading immutable TTC origin %d: %w", origin, err)
			}
			loaded[origin] = struct{}{}
			for _, candidate := range objects {
				if candidate.ID.Snapshot != origin {
					return nil, fmt.Errorf("immutable TTC origin %d returned foreign object %s", origin, candidate.ID)
				}
				key := candidate.ID.key()
				if previous, exists := all[key]; exists && !reflect.DeepEqual(previous, candidate) {
					return nil, fmt.Errorf("immutable TTC object %s has conflicting metadata", candidate.ID)
				}
				all[key] = cloneObject(candidate)
			}
			object, ok = all[id.key()]
			if !ok {
				return nil, fmt.Errorf("immutable TTC origin %d omits reachable object %s", origin, id)
			}
		}
		for _, edge := range object.Children {
			pending = append(pending, edge.Child)
		}
	}
	objects := make([]Object, 0, len(all))
	for _, object := range all {
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID.key() < objects[j].ID.key() })
	return NewImmutableCatalog(objects)
}

// Object returns an immutable metadata record.
func (catalog *ImmutableCatalog) Object(id ObjectID) (Object, bool) {
	if catalog == nil {
		return Object{}, false
	}
	object, ok := catalog.objects[id.key()]
	return cloneObject(object), ok
}

// FilesystemInode resolves immutable inode metadata from this catalog.
func (catalog *ImmutableCatalog) FilesystemInode(root ObjectID, number uint64) (InodeRecord, bool, error) {
	return filesystemInodeObjects(root, number, catalog.Object)
}

// FilesystemLookup resolves one immutable directory entry from this catalog.
func (catalog *ImmutableCatalog) FilesystemLookup(root ObjectID, parent uint64, name string) (DirectoryEntryRecord, bool, error) {
	return filesystemLookupObjects(root, parent, name, catalog.Object)
}

// FilesystemDirectory lists one immutable directory from this catalog.
func (catalog *ImmutableCatalog) FilesystemDirectory(root ObjectID, parent uint64) ([]DirectoryEntryRecord, error) {
	return filesystemDirectoryObjects(root, parent, catalog.Object)
}

// FilesystemRecords returns a decoded immutable filesystem. This is reserved
// for explicit portable export and transitional adapters; live lookups should
// use the point and single-directory methods above.
func (catalog *ImmutableCatalog) FilesystemRecords(root ObjectID) ([]InodeRecord, []DirectoryEntryRecord, error) {
	return filesystemRecordsObjects(root, catalog.Object)
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
var _ FilesystemCatalog = (*Store)(nil)
var _ FilesystemCatalog = (*ImmutableCatalog)(nil)
