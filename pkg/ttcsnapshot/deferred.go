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
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// AttachDeferredResolver attaches the transient data-plane resolver for one
// foreign catalog. The catalog identity and local/source object mapping are
// durable, but connections are deliberately reattached after restore.
func (s *Store) AttachDeferredResolver(catalog string, origin SnapshotID, loader ImmutableObjectLoader) error {
	if catalog == "" || len(catalog) > 256 || loader == nil || origin == 0 {
		return errors.New("invalid deferred TTC catalog resolver")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok || snapshot.State != SnapshotWorking {
		return fmt.Errorf("deferred TTC catalog expansion origin %d is not working", origin)
	}
	if s.deferredResolvers == nil {
		s.deferredResolvers = make(map[string]deferredResolver)
	}
	s.deferredResolvers[catalog] = deferredResolver{origin: origin, load: loader}
	return nil
}

// DetachDeferredResolver removes a transient resolver without changing the
// durable dependency. A later access fails closed until a source is attached.
func (s *Store) DetachDeferredResolver(catalog string) {
	s.mu.Lock()
	delete(s.deferredResolvers, catalog)
	s.mu.Unlock()
}

// ImportDeferredFilesystemRoot installs a single local graph stub for a
// foreign immutable filesystem root. No descendant is enumerated. Each
// expansion creates only the direct child stubs of the node being traversed.
func (s *Store) ImportDeferredFilesystemRoot(origin SnapshotID, rootName, catalog string, sourceRoot ObjectID) (ObjectID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if catalog == "" || len(catalog) > 256 || sourceRoot.IsZero() || sourceRoot.Kind != ObjectKindMetadata {
		return ObjectID{}, errors.New("invalid deferred TTC filesystem root")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok || snapshot.State != SnapshotWorking || snapshot.NextOrdinal == math.MaxUint64 {
		return ObjectID{}, fmt.Errorf("snapshot %d cannot accept a deferred filesystem root", origin)
	}
	var object Object
	key := deferredObjectKey(catalog, sourceRoot)
	if existing, found := s.deferredObjects[key]; found {
		object, found = s.state.Objects[existing.key()]
		if !found || object.State != ObjectLive || object.Refs == math.MaxUint64 {
			object = Object{}
		}
	}
	var ops []journalOp
	if object.ID.IsZero() {
		object = Object{
			ID:    ObjectID{Snapshot: origin, Ordinal: snapshot.NextOrdinal, Kind: sourceRoot.Kind},
			State: ObjectLive, SourceCatalog: catalog, SourceObject: sourceRoot, Deferred: true,
		}
		snapshot.NextOrdinal++
		ops = append(ops, journalOp{PutSnapshot: &snapshot})
	}
	object.Refs++
	if previous, found := s.state.Roots[rootName]; found {
		old, ok := s.state.Objects[previous.key()]
		if !ok {
			return ObjectID{}, fmt.Errorf("root %q references missing object %s", rootName, previous)
		}
		var err error
		old, err = releaseObject(old)
		if err != nil {
			return ObjectID{}, err
		}
		ops = append(ops, journalOp{PutObject: &old})
	}
	root := object.ID
	ops = append(ops, journalOp{PutObject: &object}, journalOp{RootName: rootName, PutRoot: &root})
	if err := s.commitLocked(ops); err != nil {
		return ObjectID{}, err
	}
	if s.deferredObjects == nil {
		s.deferredObjects = make(map[string]ObjectID)
	}
	s.deferredObjects[key] = root
	return root, nil
}

// DeferredSource reports the foreign identity retained by a local graph
// object. The identity remains available after metadata expansion so bulk
// file bytes can be fetched without changing the local inode object.
func (s *Store) DeferredSource(id ObjectID) (string, ObjectID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[id.key()]
	if !ok || object.SourceCatalog == "" || object.SourceObject.IsZero() {
		return "", ObjectID{}, false
	}
	return object.SourceCatalog, object.SourceObject, true
}

func translateDeferredFilesystemData(source Object, children map[string]ObjectID) ([]byte, error) {
	data := append([]byte(nil), source.Data...)
	if source.ID.Kind != ObjectKindInode {
		return data, nil
	}
	var inode InodeRecord
	if err := json.Unmarshal(data, &inode); err != nil {
		return nil, err
	}
	translate := func(id ObjectID) (ObjectID, error) {
		if id.IsZero() {
			return ObjectID{}, nil
		}
		mapped, ok := children[id.key()]
		if !ok {
			return ObjectID{}, fmt.Errorf("foreign inode %s embeds uncounted child %s", source.ID, id)
		}
		return mapped, nil
	}
	var err error
	if inode.File, err = translate(inode.File); err != nil {
		return nil, err
	}
	if inode.Directory, err = translate(inode.Directory); err != nil {
		return nil, err
	}
	return encodeRecord(inode)
}

// resolveDeferredLocked expands one node and its bounded direct-child set.
// s.mu must be held on entry and remains held on return. Provider I/O is
// serialized per immutable source object, but is performed without s.mu: a
// multi-hop graph can re-enter this Store while resolving an ancestor. The
// object and expansion origin are revalidated after the lock is reacquired,
// so observers still see either the stub or the complete committed expansion.
// +checklocksignore
func (s *Store) resolveDeferredLocked(id ObjectID) (Object, bool, error) {
	object, ok, _, err := s.resolveDeferredLockedAfter(id, 0)
	return object, ok, err
}

// resolveDeferredLockedAfter reserves every expansion object after
// ordinalFloor. Filesystem mutations may already have allocated unpublished
// path-copy objects in the same working origin; honoring their next ordinal
// prevents a demand-loaded child from colliding with that private batch.
// +checklocksignore
func (s *Store) resolveDeferredLockedAfter(id ObjectID, ordinalFloor uint64) (Object, bool, uint64, error) {
	object, ok := s.state.Objects[id.key()]
	if !ok || !object.Deferred {
		return object, ok, ordinalFloor, nil
	}
	resolver, ok := s.deferredResolvers[object.SourceCatalog]
	if !ok || resolver.load == nil {
		return Object{}, false, ordinalFloor, fmt.Errorf("deferred TTC catalog %q has no attached authoritative source", object.SourceCatalog)
	}
	catalog := object.SourceCatalog
	sourceID := object.SourceObject
	loadKey := deferredObjectKey(catalog, sourceID)
	if inFlight := s.deferredLoads[loadKey]; inFlight != nil {
		s.mu.Unlock()
		<-inFlight.done
		s.mu.Lock()
		if inFlight.err != nil {
			return Object{}, false, ordinalFloor, inFlight.err
		}
		// The leader commits the complete expansion before waking waiters.
		// Re-read it rather than applying its source a second time.
		object, ok = s.state.Objects[id.key()]
		if !ok || !object.Deferred {
			return object, ok, ordinalFloor, nil
		}
		return Object{}, false, ordinalFloor, fmt.Errorf("deferred TTC object %s remained unresolved after provider load", id)
	}
	if s.deferredLoads == nil {
		s.deferredLoads = make(map[string]*deferredLoad)
	}
	inFlight := &deferredLoad{done: make(chan struct{})}
	s.deferredLoads[loadKey] = inFlight
	s.mu.Unlock()
	source, loadErr := resolver.load(sourceID)
	s.mu.Lock()
	if current := s.deferredLoads[loadKey]; current == inFlight {
		delete(s.deferredLoads, loadKey)
	}
	if loadErr != nil {
		inFlight.err = fmt.Errorf("loading deferred TTC object %s: %w", sourceID, loadErr)
		close(inFlight.done)
		return Object{}, false, ordinalFloor, inFlight.err
	}
	// The graph may have advanced while provider I/O was in flight. Another
	// path can only have expanded this exact immutable stub to the same value.
	object, ok = s.state.Objects[id.key()]
	if !ok || !object.Deferred {
		inFlight.source = source
		close(inFlight.done)
		return object, ok, ordinalFloor, nil
	}
	resolver, ok = s.deferredResolvers[catalog]
	if !ok || resolver.load == nil || resolver.origin == 0 || object.SourceCatalog != catalog || object.SourceObject != sourceID {
		inFlight.err = fmt.Errorf("deferred TTC catalog %q changed while loading object %s", catalog, sourceID)
		close(inFlight.done)
		return Object{}, false, ordinalFloor, inFlight.err
	}
	inFlight.source = source
	// Wake waiters only after this goroutine has atomically committed the
	// expansion below. Every error path after this point must publish it.
	finishLoad := func(err error) {
		inFlight.err = err
		close(inFlight.done)
	}
	finished := false
	defer func() {
		if !finished {
			finishLoad(errors.New("deferred TTC expansion aborted"))
		}
	}()
	fail := func(err error) (Object, bool, uint64, error) {
		finished = true
		finishLoad(err)
		return Object{}, false, ordinalFloor, err
	}
	if err := validateObject(source); err != nil {
		return fail(err)
	}
	if source.ID != sourceID || source.State != ObjectLive || source.Deferred {
		return fail(fmt.Errorf("deferred TTC source returned invalid object %s for %s", source.ID, sourceID))
	}
	snapshot, ok := s.state.Snapshots[snapshotKey(resolver.origin)]
	if !ok || snapshot.State != SnapshotWorking {
		return fail(fmt.Errorf("deferred TTC expansion origin %d is not working", resolver.origin))
	}
	if snapshot.NextOrdinal < ordinalFloor {
		snapshot.NextOrdinal = ordinalFloor
	}
	touched := make(map[string]Object)
	mapped := make(map[string]ObjectID, len(source.Children))
	edges := make([]Edge, 0, len(source.Children))
	newMappings := make(map[string]ObjectID)
	for _, edge := range source.Children {
		foreignKey := deferredObjectKey(object.SourceCatalog, edge.Child)
		localID, found := s.deferredObjects[foreignKey]
		child, live := s.state.Objects[localID.key()]
		if !found || !live || child.State != ObjectLive {
			if snapshot.NextOrdinal == math.MaxUint64 {
				return fail(errors.New("deferred TTC object ordinal space exhausted"))
			}
			localID = ObjectID{Snapshot: resolver.origin, Ordinal: snapshot.NextOrdinal, Kind: edge.Child.Kind}
			snapshot.NextOrdinal++
			child = Object{
				ID: localID, State: ObjectLive, SourceCatalog: object.SourceCatalog,
				SourceObject: edge.Child, Deferred: true,
			}
			newMappings[foreignKey] = localID
		}
		if previous, exists := touched[localID.key()]; exists {
			child = previous
		}
		if child.Refs == math.MaxUint64 {
			return fail(fmt.Errorf("deferred child %s reference count overflows", localID))
		}
		child.Refs++
		touched[localID.key()] = child
		mapped[edge.Child.key()] = localID
		edges = append(edges, Edge{Slot: edge.Slot, Child: localID})
	}
	data, err := translateDeferredFilesystemData(source, mapped)
	if err != nil {
		return fail(err)
	}
	object.Data = data
	object.Children = edges
	object.BulkLength = source.BulkLength
	object.Deferred = false
	if err := validateObject(object); err != nil {
		return fail(err)
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sortStrings(keys)
	ops := make([]journalOp, 0, len(keys)+2)
	for _, key := range keys {
		child := touched[key]
		ops = append(ops, journalOp{PutObject: &child})
	}
	ops = append(ops, journalOp{PutObject: &object})
	if len(newMappings) != 0 {
		ops = append(ops, journalOp{PutSnapshot: &snapshot})
	}
	if err := s.commitLocked(ops); err != nil {
		return fail(err)
	}
	for key, localID := range newMappings {
		s.deferredObjects[key] = localID
	}
	finished = true
	finishLoad(nil)
	return cloneObject(object), true, snapshot.NextOrdinal, nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
