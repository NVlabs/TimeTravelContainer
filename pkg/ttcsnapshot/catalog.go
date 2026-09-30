// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
)

// MemoryCatalog is the immutable subset of a snapshot graph required to
// reconstruct graph-native memory. Store implements it for authoritative
// local state; ImmutableCatalog implements it for shared or peer metadata
// without creating a second reference-count authority.
type MemoryCatalog interface {
	MemoryPageEntries(ObjectID) ([]MemoryPageEntry, error)
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
	objects map[ObjectID]Object
}

// ImmutableObjectLoader resolves one immutable metadata record by its stable
// identity. Bulk bytes are deliberately outside this interface.
type ImmutableObjectLoader func(ObjectID) (Object, error)

// LazyImmutableCatalog is a read-only catalog that resolves metadata records
// only when a tree walk reaches them. It is used by distributed filesystems so
// a remote fork need not enumerate an entire inode/directory graph before the
// target can begin executing.
//
// A loader failure poisons the catalog. This is intentional: all records in a
// catalog belong to one fenced logical view, so continuing after one provider
// or structural failure could combine metadata from different views.
type LazyImmutableCatalog struct {
	mu      sync.Mutex
	objects map[ObjectID]Object
	loader  ImmutableObjectLoader
	err     error
}

// NewLazyImmutableCatalog creates an empty demand-loaded immutable catalog.
func NewLazyImmutableCatalog(loader ImmutableObjectLoader) (*LazyImmutableCatalog, error) {
	if loader == nil {
		return nil, errors.New("immutable TTC object loader is nil")
	}
	return &LazyImmutableCatalog{objects: make(map[ObjectID]Object), loader: loader}, nil
}

// Object resolves and caches one immutable metadata record. Concurrent misses
// for the same object are serialized; metadata objects are small and bounded,
// while bulk file and memory bytes remain in the separate data resolver.
func (catalog *LazyImmutableCatalog) Object(id ObjectID) (Object, bool) {
	if catalog == nil {
		return Object{}, false
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.err != nil {
		return Object{}, false
	}
	if object, ok := catalog.objects[id]; ok {
		return cloneObject(object), true
	}
	object, err := catalog.loader(id)
	if err == nil {
		err = validateObject(object)
	}
	if err == nil && object.ID != id {
		err = fmt.Errorf("immutable TTC loader returned %s for %s", object.ID, id)
	}
	if err == nil && object.State != ObjectLive {
		err = fmt.Errorf("immutable TTC loader returned non-live object %s", id)
	}
	if err != nil {
		catalog.err = err
		return Object{}, false
	}
	catalog.objects[id] = cloneObject(object)
	return cloneObject(object), true
}

// Err reports the first provider or structural failure observed by Object.
func (catalog *LazyImmutableCatalog) Err() error {
	if catalog == nil {
		return errors.New("nil immutable TTC catalog")
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return catalog.err
}

// LoadedObjects returns the number of metadata records currently cached. It
// is intended for accounting, diagnostics, and demand-loading tests.
func (catalog *LazyImmutableCatalog) LoadedObjects() int {
	if catalog == nil {
		return 0
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return len(catalog.objects)
}

func (catalog *LazyImmutableCatalog) operationError(err error) error {
	if loadErr := catalog.Err(); loadErr != nil {
		return loadErr
	}
	return err
}

// FilesystemInode resolves one inode without enumerating the filesystem.
func (catalog *LazyImmutableCatalog) FilesystemInode(root ObjectID, number uint64) (InodeRecord, bool, error) {
	record, ok, err := filesystemInodeObjects(root, number, catalog.Object)
	return record, ok, catalog.operationError(err)
}

// FilesystemLookup resolves one directory entry without enumerating siblings.
func (catalog *LazyImmutableCatalog) FilesystemLookup(root ObjectID, parent uint64, name string) (DirectoryEntryRecord, bool, error) {
	record, ok, err := filesystemLookupObjects(root, parent, name, catalog.Object)
	return record, ok, catalog.operationError(err)
}

// FilesystemDirectory loads only the requested directory and its bounded
// metadata paths.
func (catalog *LazyImmutableCatalog) FilesystemDirectory(root ObjectID, parent uint64) ([]DirectoryEntryRecord, error) {
	records, err := filesystemDirectoryObjects(root, parent, catalog.Object)
	return records, catalog.operationError(err)
}

// FilesystemRecords explicitly materializes the complete reachable namespace.
func (catalog *LazyImmutableCatalog) FilesystemRecords(root ObjectID) ([]InodeRecord, []DirectoryEntryRecord, error) {
	inodes, entries, err := filesystemRecordsObjects(root, catalog.Object, nil)
	return inodes, entries, catalog.operationError(err)
}

// MemoryPageEntries resolves a memory tree on demand. Memory restore
// necessarily enumerates every page; filesystem lookup does not use it.
func (catalog *LazyImmutableCatalog) MemoryPageEntries(root ObjectID) ([]MemoryPageEntry, error) {
	objects, err := ReachableObjects(catalog, []ObjectID{root})
	if err != nil {
		return nil, catalog.operationError(err)
	}
	immutable, err := NewImmutableCatalog(objects)
	if err != nil {
		return nil, err
	}
	return immutable.MemoryPageEntries(root)
}

func osErrNotExist(id ObjectID) error {
	return fmt.Errorf("immutable TTC object %s is missing", id)
}

// NewImmutableCatalog constructs a read-only catalog from one or more
// immutable snapshot manifests. References are treated as authority metadata
// and ignored by this cache view.
func NewImmutableCatalog(objects []Object) (*ImmutableCatalog, error) {
	catalog := &ImmutableCatalog{objects: make(map[ObjectID]Object, len(objects))}
	for _, object := range objects {
		if err := validateObject(object); err != nil {
			return nil, err
		}
		if _, ok := catalog.objects[object.ID]; ok {
			return nil, fmt.Errorf("immutable catalog repeats object %s", object.ID)
		}
		catalog.objects[object.ID] = cloneObject(object)
	}
	return catalog, nil
}

// ResolveImmutableCatalog loads immutable origin sections while walking only
// objects reachable from roots. Snapshot packs intentionally contain objects
// allocated by one origin; when a reachable edge crosses origins, load
// supplies that origin directly from its authority-selected provider or shared
// pack. Each origin and object is visited at most once.
func ResolveImmutableCatalog(roots []ObjectID, load func(SnapshotID) ([]Object, error)) (*ImmutableCatalog, error) {
	if len(roots) == 0 || load == nil {
		return nil, errors.New("immutable TTC catalog resolution requires a root")
	}
	loaded := make(map[SnapshotID]struct{})
	all := make(map[ObjectID]Object)
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
		object, ok := all[id]
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
				if previous, exists := all[candidate.ID]; exists && !reflect.DeepEqual(previous, candidate) {
					return nil, fmt.Errorf("immutable TTC object %s has conflicting metadata", candidate.ID)
				}
				all[candidate.ID] = cloneObject(candidate)
			}
			object, ok = all[id]
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
	slices.SortFunc(objects, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	return NewImmutableCatalog(objects)
}

// Object returns an immutable metadata record.
func (catalog *ImmutableCatalog) Object(id ObjectID) (Object, bool) {
	if catalog == nil {
		return Object{}, false
	}
	object, ok := catalog.objects[id]
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
	return filesystemRecordsObjects(root, catalog.Object, nil)
}

// MemoryPageEntries enumerates the pages a memory tree maps.
func (catalog *ImmutableCatalog) MemoryPageEntries(root ObjectID) ([]MemoryPageEntry, error) {
	if catalog == nil {
		return nil, fmt.Errorf("nil immutable TTC catalog")
	}
	return memoryPageEntries(root, func(id ObjectID) (Object, bool) {
		object, ok := catalog.objects[id]
		return object, ok
	})
}

var _ MemoryCatalog = (*Store)(nil)
var _ MemoryCatalog = (*ImmutableCatalog)(nil)
var _ MemoryCatalog = (*LazyImmutableCatalog)(nil)
var _ FilesystemCatalog = (*Store)(nil)
var _ FilesystemCatalog = (*ImmutableCatalog)(nil)
var _ FilesystemCatalog = (*LazyImmutableCatalog)(nil)
var _ ObjectCatalog = (*LazyImmutableCatalog)(nil)
