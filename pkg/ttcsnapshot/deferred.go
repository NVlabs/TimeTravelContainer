// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"errors"
	"fmt"
	"math"
	"slices"
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
	inode, err := decodeInodeRecord(data)
	if err != nil {
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
		// The leader commits the complete expansion before waking waiters.
		// Re-read it rather than applying its source a second time. A batch
		// may commit this expansion and then fail on another object's.
		object, ok = s.state.Objects[id.key()]
		if !ok || !object.Deferred {
			return object, ok, ordinalFloor, nil
		}
		if inFlight.err != nil {
			return Object{}, false, ordinalFloor, inFlight.err
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
	// Provider I/O deliberately ran without s.mu. Before allocating local
	// object IDs or appending the expansion, join the same cross-process writer
	// critical section as ordinary repository mutations and replay anything
	// committed while the provider request was in flight.
	acquired, coordinateErr := s.beginImplicitExternalMutationLocked()
	if coordinateErr != nil {
		inFlight.err = fmt.Errorf("coordinating deferred TTC object expansion %s: %w", sourceID, coordinateErr)
		close(inFlight.done)
		return Object{}, false, ordinalFloor, inFlight.err
	}
	defer s.endImplicitExternalMutationLocked(acquired)
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
	slices.Sort(keys)
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

// resolveDeferredBatchLocked expands the deferred objects among ids as
// resolveDeferredLocked expands each, but many to a journal record: making a
// lazily imported tree of 100,000 files independent expands 300,000 objects,
// which took a record, a write and a stat each. An object another goroutine
// is expanding, or whose catalog has no source attached, is left to
// resolveDeferredLocked. s.mu must be held on entry and remains held on
// return; provider I/O runs without it.
//
// A batch does not make others wait for its objects, as resolveDeferredLocked
// does for one: a provider that expands a stub of this graph to answer would
// wait for its own batch. One expanded meanwhile by another is left as it is,
// its source read twice.
// +checklocksignore
func (s *Store) resolveDeferredBatchLocked(ids []ObjectID) error {
	if len(s.deferredResolvers) == 0 {
		// Nothing can be expanded.
		return nil
	}
	const chunk = 256
	for start := 0; start < len(ids); start += chunk {
		if err := s.resolveDeferredChunkLocked(ids[start:min(start+chunk, len(ids))]); err != nil {
			return err
		}
	}
	return nil
}

// +checklocksignore
func (s *Store) resolveDeferredChunkLocked(ids []ObjectID) error {
	type expansion struct {
		key      string
		catalog  string
		sourceID ObjectID
		load     ImmutableObjectLoader
		source   Object
	}
	var work []expansion
	selected := make(map[string]bool)
	for _, id := range ids {
		key := id.key()
		object, ok := s.state.Objects[key]
		if !ok || !object.Deferred || selected[key] {
			continue
		}
		resolver, attached := s.deferredResolvers[object.SourceCatalog]
		if !attached || resolver.load == nil || s.deferredLoads[deferredObjectKey(object.SourceCatalog, object.SourceObject)] != nil {
			continue
		}
		selected[key] = true
		work = append(work, expansion{key: key, catalog: object.SourceCatalog, sourceID: object.SourceObject, load: resolver.load})
	}
	if len(work) == 0 {
		return nil
	}
	s.mu.Unlock()
	var loadErr error
	for index := range work {
		item := &work[index]
		if item.source, loadErr = item.load(item.sourceID); loadErr != nil {
			loadErr = fmt.Errorf("loading deferred TTC object %s: %w", item.sourceID, loadErr)
			break
		}
	}
	s.mu.Lock()
	if loadErr != nil {
		return loadErr
	}
	// As resolveDeferredLockedAfter does, join the writers' critical section
	// and replay what was committed while the provider was read.
	acquired, err := s.beginImplicitExternalMutationLocked()
	if err != nil {
		return fmt.Errorf("coordinating deferred TTC object expansion: %w", err)
	}
	defer s.endImplicitExternalMutationLocked(acquired)
	// The expansions not yet committed: the objects they change, the child
	// stubs they make, and the origins whose ordinals they take.
	touched := make(map[string]Object)
	newMappings := make(map[string]ObjectID)
	snapshots := make(map[SnapshotID]Snapshot)
	current := func(key string) (Object, bool) {
		if object, ok := touched[key]; ok {
			return object, true
		}
		object, ok := s.state.Objects[key]
		return object, ok
	}
	operations := 0
	flush := func() error {
		if len(touched) == 0 {
			return nil
		}
		keys := make([]string, 0, len(touched))
		for key := range touched {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		ops := make([]journalOp, 0, len(keys)+len(snapshots))
		for _, key := range keys {
			object := touched[key]
			ops = append(ops, journalOp{PutObject: &object})
		}
		origins := make([]SnapshotID, 0, len(snapshots))
		for origin := range snapshots {
			origins = append(origins, origin)
		}
		slices.Sort(origins)
		for _, origin := range origins {
			snapshot := snapshots[origin]
			ops = append(ops, journalOp{PutSnapshot: &snapshot})
		}
		if err := s.commitLocked(ops); err != nil {
			return err
		}
		for key, localID := range newMappings {
			s.deferredObjects[key] = localID
		}
		clear(touched)
		clear(newMappings)
		clear(snapshots)
		operations = 0
		return nil
	}
	for _, item := range work {
		// The graph may have advanced while provider I/O was in flight.
		// Another path can only have expanded this exact immutable stub to the
		// same value.
		object, ok := current(item.key)
		if !ok || !object.Deferred {
			continue
		}
		resolver, ok := s.deferredResolvers[item.catalog]
		if !ok || resolver.load == nil || resolver.origin == 0 || object.SourceCatalog != item.catalog || object.SourceObject != item.sourceID {
			return fmt.Errorf("deferred TTC catalog %q changed while loading object %s", item.catalog, item.sourceID)
		}
		source := item.source
		if err := validateObject(source); err != nil {
			return err
		}
		if source.ID != item.sourceID || source.State != ObjectLive || source.Deferred {
			return fmt.Errorf("deferred TTC source returned invalid object %s for %s", source.ID, item.sourceID)
		}
		snapshot, ok := snapshots[resolver.origin]
		if !ok {
			if snapshot, ok = s.state.Snapshots[snapshotKey(resolver.origin)]; !ok || snapshot.State != SnapshotWorking {
				return fmt.Errorf("deferred TTC expansion origin %d is not working", resolver.origin)
			}
		}
		allocated := false
		mapped := make(map[string]ObjectID, len(source.Children))
		edges := make([]Edge, 0, len(source.Children))
		for _, edge := range source.Children {
			foreignKey := deferredObjectKey(object.SourceCatalog, edge.Child)
			localID, found := newMappings[foreignKey]
			if !found {
				localID, found = s.deferredObjects[foreignKey]
			}
			child, live := current(localID.key())
			if !found || !live || child.State != ObjectLive {
				if snapshot.NextOrdinal == math.MaxUint64 {
					return errors.New("deferred TTC object ordinal space exhausted")
				}
				localID = ObjectID{Snapshot: resolver.origin, Ordinal: snapshot.NextOrdinal, Kind: edge.Child.Kind}
				snapshot.NextOrdinal++
				allocated = true
				child = Object{
					ID: localID, State: ObjectLive, SourceCatalog: object.SourceCatalog,
					SourceObject: edge.Child, Deferred: true,
				}
				newMappings[foreignKey] = localID
			}
			if child.Refs == math.MaxUint64 {
				return fmt.Errorf("deferred child %s reference count overflows", localID)
			}
			child.Refs++
			touched[localID.key()] = child
			mapped[edge.Child.key()] = localID
			edges = append(edges, Edge{Slot: edge.Slot, Child: localID})
		}
		if allocated {
			snapshots[resolver.origin] = snapshot
		}
		data, err := translateDeferredFilesystemData(source, mapped)
		if err != nil {
			return err
		}
		object.Data = data
		object.Children = edges
		object.BulkLength = source.BulkLength
		object.Deferred = false
		if err := validateObject(object); err != nil {
			return err
		}
		touched[item.key] = object
		// A record holds a thousand-odd objects, far below its limit.
		if operations += 1 + len(edges); operations >= 1024 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
