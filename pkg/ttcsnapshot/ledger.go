// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// OutboundReference is one ordered cross-owner edge mutation. Name is stable
// for the immutable parent edge. Delivery is strictly in Sequence order and
// the receiver deduplicates Operation.
type OutboundReference struct {
	Sequence  uint64
	Operation string
	Name      string
	Parent    ObjectID
	Child     ObjectID
	Retain    bool
}

type appliedReference struct {
	Name   string
	Object ObjectID
	Retain bool
}

type referenceLedgerState struct {
	Origins       map[SnapshotID]struct{}
	Objects       map[string]Object
	References    map[string]ObjectID
	Applied       map[string]appliedReference
	Outbox        []OutboundReference
	NextSequence  uint64
	Acknowledged  uint64
	HandoffCursor map[SnapshotID]uint64

	// undo records what the mutation in progress has changed.
	undo *ledgerUndo
	// byOrigin and reclaiming index Objects, so that work on one origin, and
	// collection, need not go through every object.
	byOrigin   map[SnapshotID]*ledgerOriginObjects
	reclaiming map[string]struct{}
}

// ledgerOriginObjects indexes the objects of one origin.
type ledgerOriginObjects struct {
	keys map[string]struct{}
	// changes advances whenever one of the objects stops being live, or one is
	// added other than live: while it stands still, the origin's live objects
	// are the same.
	changes uint64
}

func newReferenceLedgerState() referenceLedgerState {
	return referenceLedgerState{
		Origins: make(map[SnapshotID]struct{}), Objects: make(map[string]Object),
		References: make(map[string]ObjectID), Applied: make(map[string]appliedReference), NextSequence: 1,
		HandoffCursor: make(map[SnapshotID]uint64),
		byOrigin:      make(map[SnapshotID]*ledgerOriginObjects), reclaiming: make(map[string]struct{}),
	}
}

// ledgerUndo holds the prior values of what a mutation of a
// referenceLedgerState has changed, so that a mutation refused part way can
// be undone. A mutation works in
// place, in time proportional to what it changes: copying the whole state
// first, as mutations did, took a quarter of a second and 87 MB for each
// reference retained on a ledger of 300,000 objects.
type ledgerUndo struct {
	// A nil prior value is an absent one.
	origins      map[SnapshotID]bool
	objects      map[string]*Object
	references   map[string]*ObjectID
	applied      map[string]*appliedReference
	cursors      map[SnapshotID]*uint64
	outbox       []OutboundReference
	nextSequence uint64
	acknowledged uint64
}

// begin starts recording what a mutation changes.
func (state *referenceLedgerState) begin() {
	state.undo = &ledgerUndo{outbox: state.Outbox, nextSequence: state.NextSequence, acknowledged: state.Acknowledged}
}

// commit keeps what the mutation changed.
func (state *referenceLedgerState) commit() {
	state.undo = nil
}

// rollback restores what the mutation changed.
func (state *referenceLedgerState) rollback() {
	undo := state.undo
	state.undo = nil
	for origin, present := range undo.origins {
		if present {
			state.Origins[origin] = struct{}{}
		} else {
			delete(state.Origins, origin)
		}
	}
	for key, prior := range undo.objects {
		var current *Object
		if object, ok := state.Objects[key]; ok {
			current = &object
		}
		if prior != nil {
			state.Objects[key] = *prior
		} else {
			delete(state.Objects, key)
		}
		state.indexObject(key, current, prior)
	}
	for origin, present := range undo.origins {
		if !present {
			// It had no objects, as it has none again.
			delete(state.byOrigin, origin)
		}
	}
	for name, prior := range undo.references {
		if prior != nil {
			state.References[name] = *prior
		} else {
			delete(state.References, name)
		}
	}
	for operation, prior := range undo.applied {
		if prior != nil {
			state.Applied[operation] = *prior
		} else {
			delete(state.Applied, operation)
		}
	}
	for origin, prior := range undo.cursors {
		if prior != nil {
			state.HandoffCursor[origin] = *prior
		} else {
			delete(state.HandoffCursor, origin)
		}
	}
	state.Outbox, state.NextSequence, state.Acknowledged = undo.outbox, undo.nextSequence, undo.acknowledged
}

// The setters below change the state's maps, recording each entry's first
// prior value in a mutation. The outbox and the sequence numbers are recorded
// whole when the mutation begins.

func (state *referenceLedgerState) setOrigin(origin SnapshotID, present bool) {
	if undo := state.undo; undo != nil {
		if _, recorded := undo.origins[origin]; !recorded {
			if undo.origins == nil {
				undo.origins = make(map[SnapshotID]bool)
			}
			_, present := state.Origins[origin]
			undo.origins[origin] = present
		}
	}
	if present {
		state.Origins[origin] = struct{}{}
	} else {
		delete(state.Origins, origin)
		// A retired origin has no objects left.
		delete(state.byOrigin, origin)
	}
}

func (state *referenceLedgerState) recordObject(key string, prior *Object) {
	if undo := state.undo; undo != nil {
		if _, recorded := undo.objects[key]; !recorded {
			if undo.objects == nil {
				undo.objects = make(map[string]*Object)
			}
			if prior != nil {
				saved := *prior
				undo.objects[key] = &saved
			} else {
				undo.objects[key] = nil
			}
		}
	}
}

func (state *referenceLedgerState) putObject(key string, object Object) {
	var prior *Object
	if existing, ok := state.Objects[key]; ok {
		prior = &existing
	}
	state.recordObject(key, prior)
	state.Objects[key] = object
	state.indexObject(key, prior, &object)
}

func (state *referenceLedgerState) deleteObject(key string) {
	existing, ok := state.Objects[key]
	if !ok {
		return
	}
	state.recordObject(key, &existing)
	delete(state.Objects, key)
	state.indexObject(key, &existing, nil)
}

// indexObject keeps the indexes in step with the object at key changing from
// prior to next, nil for absent.
func (state *referenceLedgerState) indexObject(key string, prior, next *Object) {
	if prior != nil && next != nil && prior.State == next.State {
		// Only its count changed.
		return
	}
	var origin SnapshotID
	switch {
	case next != nil:
		origin = next.ID.Snapshot
	case prior != nil:
		origin = prior.ID.Snapshot
	default:
		return
	}
	entry := state.byOrigin[origin]
	if entry == nil {
		entry = &ledgerOriginObjects{keys: make(map[string]struct{})}
		state.byOrigin[origin] = entry
	}
	switch {
	case prior == nil:
		entry.keys[key] = struct{}{}
	case next == nil:
		delete(entry.keys, key)
	}
	if prior != nil && prior.State == ObjectReclaiming {
		delete(state.reclaiming, key)
	}
	if next != nil && next.State == ObjectReclaiming {
		state.reclaiming[key] = struct{}{}
	}
	if prior != nil || next.State != ObjectLive {
		entry.changes++
	}
}

// originKeys returns the keys of origin's objects.
func (state *referenceLedgerState) originKeys(origin SnapshotID) map[string]struct{} {
	if entry := state.byOrigin[origin]; entry != nil {
		return entry.keys
	}
	return nil
}

func (state *referenceLedgerState) recordReference(name string) {
	if undo := state.undo; undo != nil {
		if _, recorded := undo.references[name]; !recorded {
			if undo.references == nil {
				undo.references = make(map[string]*ObjectID)
			}
			if prior, ok := state.References[name]; ok {
				undo.references[name] = &prior
			} else {
				undo.references[name] = nil
			}
		}
	}
}

func (state *referenceLedgerState) putReference(name string, id ObjectID) {
	state.recordReference(name)
	state.References[name] = id
}

func (state *referenceLedgerState) deleteReference(name string) {
	state.recordReference(name)
	delete(state.References, name)
}

func (state *referenceLedgerState) recordApplied(operation string) {
	if undo := state.undo; undo != nil {
		if _, recorded := undo.applied[operation]; !recorded {
			if undo.applied == nil {
				undo.applied = make(map[string]*appliedReference)
			}
			if prior, ok := state.Applied[operation]; ok {
				undo.applied[operation] = &prior
			} else {
				undo.applied[operation] = nil
			}
		}
	}
}

func (state *referenceLedgerState) putApplied(operation string, applied appliedReference) {
	state.recordApplied(operation)
	state.Applied[operation] = applied
}

func (state *referenceLedgerState) deleteApplied(operation string) {
	state.recordApplied(operation)
	delete(state.Applied, operation)
}

func (state *referenceLedgerState) recordCursor(origin SnapshotID) {
	if undo := state.undo; undo != nil {
		if _, recorded := undo.cursors[origin]; !recorded {
			if undo.cursors == nil {
				undo.cursors = make(map[SnapshotID]*uint64)
			}
			if prior, ok := state.HandoffCursor[origin]; ok {
				undo.cursors[origin] = &prior
			} else {
				undo.cursors[origin] = nil
			}
		}
	}
}

func (state *referenceLedgerState) putCursor(origin SnapshotID, cursor uint64) {
	state.recordCursor(origin)
	state.HandoffCursor[origin] = cursor
}

func (state *referenceLedgerState) deleteCursor(origin SnapshotID) {
	state.recordCursor(origin)
	delete(state.HandoffCursor, origin)
}

// ReferenceLedger is the exact-counting engine of the elected shared-GC
// coordinator. It is kept in memory: a coordinator builds it anew from the
// shared manifests and the nodes' claims whenever it takes the lease. This
// type contains no filesystem or authority policy.
type ReferenceLedger struct {
	mu    sync.RWMutex
	state referenceLedgerState
}

// NewReferenceLedger returns an empty owner ledger.
func NewReferenceLedger() *ReferenceLedger {
	return &ReferenceLedger{state: newReferenceLedgerState()}
}

// mutateLocked applies change to the ledger's state in place. A change that
// fails is undone.
func (ledger *ReferenceLedger) mutateLocked(change func(*referenceLedgerState) error) error {
	ledger.state.begin()
	if err := change(&ledger.state); err != nil {
		ledger.state.rollback()
		return err
	}
	ledger.state.commit()
	return nil
}

// Close releases the ledger.
func (ledger *ReferenceLedger) Close() error {
	return nil
}

func edgeReferenceName(parent ObjectID, slot uint16) string {
	const digits = "0123456789abcdef"
	name := make([]byte, 0, len("edge/")+36+5)
	name = append(name, "edge/"...)
	name = append(name, parent.key()...)
	return string(append(name, '/', digits[slot>>12], digits[slot>>8&15], digits[slot>>4&15], digits[slot&15]))
}

// parseEdgeReferenceName returns the parent and slot of the edge that name,
// as edgeReferenceName writes it, names.
func parseEdgeReferenceName(name string) (ObjectID, uint16, bool) {
	rest, ok := strings.CutPrefix(name, "edge/")
	if !ok {
		return ObjectID{}, 0, false
	}
	key, slotText, ok := strings.Cut(rest, "/")
	if !ok {
		return ObjectID{}, 0, false
	}
	parent, err := parseObjectKey(key)
	if err != nil {
		return ObjectID{}, 0, false
	}
	slot, err := strconv.ParseUint(slotText, 16, 16)
	if err != nil || edgeReferenceName(parent, uint16(slot)) != name {
		return ObjectID{}, 0, false
	}
	return parent, uint16(slot), true
}

func edgeOperationName(parent ObjectID, slot uint16, retain bool) string {
	action := "release"
	if retain {
		action = "retain"
	}
	return fmt.Sprintf("%s:%s:%04x", action, parent.key(), slot)
}

func validateReferenceName(name string) error {
	if name == "" || len(name) > 1024 || strings.TrimSpace(name) != name || strings.ContainsRune(name, 0) {
		return errors.New("invalid TTC reference name")
	}
	return nil
}

func (state *referenceLedgerState) owns(id ObjectID) bool {
	_, ok := state.Origins[id.Snapshot]
	return ok
}

func (state *referenceLedgerState) retain(name string, id ObjectID) error {
	if err := validateReferenceName(name); err != nil {
		return err
	}
	if previous, ok := state.References[name]; ok {
		if previous == id {
			return nil
		}
		return fmt.Errorf("reference %q already selects %s", name, previous)
	}
	object, ok := state.Objects[id.key()]
	if !ok || !state.owns(id) || object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("owned object %s cannot be retained", id)
	}
	object.Refs++
	state.putObject(id.key(), object)
	state.putReference(name, id)
	return nil
}

func (state *referenceLedgerState) release(name string, expected ObjectID) error {
	id, ok := state.References[name]
	if !ok {
		return fmt.Errorf("reference %q does not exist", name)
	}
	if id != expected {
		return fmt.Errorf("reference %q selects %s, not %s", name, id, expected)
	}
	object, ok := state.Objects[id.key()]
	if !ok || object.State != ObjectLive || object.Refs == 0 {
		return fmt.Errorf("owned object %s cannot be released", id)
	}
	object.Refs--
	if object.Refs == 0 {
		object.State = ObjectReclaiming
	}
	state.putObject(id.key(), object)
	state.deleteReference(name)
	return nil
}

func (state *referenceLedgerState) enqueue(parent ObjectID, edge Edge, retain bool) error {
	if state.NextSequence == 0 || state.NextSequence == math.MaxUint64 {
		return errors.New("reference outbox sequence exhausted")
	}
	state.Outbox = append(state.Outbox, OutboundReference{
		Sequence: state.NextSequence, Operation: edgeOperationName(parent, edge.Slot, retain),
		Name: edgeReferenceName(parent, edge.Slot), Parent: parent, Child: edge.Child, Retain: retain,
	})
	state.NextSequence++
	return nil
}

// ImportOrigin atomically accepts authority for one immutable origin. Initial
// references are the complete provider handoff baseline: branch roots,
// transaction roots, open/runtime pins, and incoming cross-owner edge names.
// Work is proportional to objects created by this origin, which publication
// already serializes; ordinary fork remains a root-only operation.
func (ledger *ReferenceLedger) ImportOrigin(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	if ledger == nil || origin == 0 {
		return errors.New("invalid reference-ledger origin import")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, exists := ledger.state.Origins[origin]; exists {
		return nil
	}
	return ledger.importLocked(origin, objectsWithoutData(objects), references)
}

// objectsWithoutData returns objects as the ledger keeps them: without their
// data, which only the origin's manifest in the shared root keeps. The ledger
// counts references; the data, memory tables and inode records, was most of
// what it held, journaled, and checkpointed of every shared origin.
func objectsWithoutData(objects []Object) []Object {
	stripped := make([]Object, len(objects))
	for index, object := range objects {
		stripped[index] = objectWithoutData(object)
	}
	return stripped
}

// objectWithoutData is object without its data; see objectsWithoutData.
func objectWithoutData(object Object) Object {
	return Object{
		ID: object.ID, State: object.State, Refs: object.Refs, BulkLength: object.BulkLength,
		Children: slices.Clone(object.Children), SourceCatalog: object.SourceCatalog,
		SourceObject: object.SourceObject, Deferred: object.Deferred,
	}
}

// importLocked imports origin into the ledger's state. A refused import is
// undone.
func (ledger *ReferenceLedger) importLocked(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	return ledger.mutateLocked(func(state *referenceLedgerState) error {
		return state.importOrigin(origin, objects, references)
	})
}

func (state *referenceLedgerState) importOrigin(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	if origin == 0 {
		return errors.New("invalid reference-ledger origin import")
	}
	if _, exists := state.Origins[origin]; exists {
		return nil
	}
	state.setOrigin(origin, true)
	state.putCursor(origin, 0)
	// Each object's key, formed once.
	keys := make([]string, len(objects))
	for index, input := range objects {
		object := input
		if object.ID.Snapshot != origin {
			return fmt.Errorf("origin %d import contains foreign object %s", origin, object.ID)
		}
		object.State, object.Refs = ObjectLive, 0
		if err := validateObject(object); err != nil {
			return err
		}
		keys[index] = object.ID.key()
		if _, exists := state.Objects[keys[index]]; exists {
			return fmt.Errorf("origin import repeats object %s", object.ID)
		}
		state.putObject(keys[index], object)
	}
	// An edge of the import may replace a named reference bridging it, which
	// only the parents named by one can have: naming each edge to look for
	// one named 300,000 edges for a branch of 100,000 files.
	bridgedParents := make(map[ObjectID]struct{})
	for name := range state.References {
		if parent, _, ok := parseEdgeReferenceName(name); ok {
			bridgedParents[parent] = struct{}{}
		}
	}
	for index := range objects {
		parent := state.Objects[keys[index]]
		_, parentBridged := bridgedParents[parent.ID]
		for _, edge := range parent.Children {
			if !state.owns(edge.Child) {
				if err := state.enqueue(parent.ID, edge, true); err != nil {
					return err
				}
				continue
			}
			child, ok := state.Objects[edge.Child.key()]
			if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
				return fmt.Errorf("object %s references unavailable owned child %s", parent.ID, edge.Child)
			}
			if parentBridged {
				bridge := edgeReferenceName(parent.ID, edge.Slot)
				if bridged, ok := state.References[bridge]; ok {
					if bridged != edge.Child {
						return fmt.Errorf("incoming edge bridge %q selects %s, want %s", bridge, bridged, edge.Child)
					}
					// Replace the external named reference with the now-internal
					// structural edge. The child's count is unchanged.
					state.deleteReference(bridge)
					continue
				}
			}
			child.Refs++
			state.putObject(edge.Child.key(), child)
		}
	}
	for _, parent := range state.Objects {
		if parent.ID.Snapshot == origin || !state.owns(parent.ID) {
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == origin {
				child, ok := state.Objects[edge.Child.key()]
				if !ok || child.Refs == math.MaxUint64 {
					return fmt.Errorf("owned parent %s references unavailable imported child %s", parent.ID, edge.Child)
				}
				child.State = ObjectLive
				child.Refs++
				state.putObject(edge.Child.key(), child)
			}
		}
	}
	// internalIncoming returns the imported object an owned parent's edge
	// that name names selects.
	internalIncoming := func(name string) (ObjectID, bool) {
		id, slot, ok := parseEdgeReferenceName(name)
		if !ok || !state.owns(id) {
			return ObjectID{}, false
		}
		parent, ok := state.Objects[id.key()]
		if !ok {
			return ObjectID{}, false
		}
		for _, edge := range parent.Children {
			if edge.Slot == slot && edge.Child.Snapshot == origin {
				return edge.Child, true
			}
		}
		return ObjectID{}, false
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		effectiveName := name
		if strings.HasPrefix(name, "root/bridge/edge/") {
			effectiveName = strings.TrimPrefix(name, "root/bridge/")
		}
		if internal, ok := internalIncoming(effectiveName); ok {
			if references[name] != internal {
				return fmt.Errorf("incoming edge baseline %q selects %s, want %s", effectiveName, references[name], internal)
			}
			// The already-owned parent edge supplies this count directly.
			continue
		}
		if err := state.retain(effectiveName, references[name]); err != nil {
			return fmt.Errorf("importing reference %q: %w", effectiveName, err)
		}
	}
	for _, key := range keys {
		if object := state.Objects[key]; object.Refs == 0 {
			object.State = ObjectReclaiming
			state.putObject(key, object)
		}
	}
	return nil
}

// ApplyReference applies one idempotent local root or delivered cross-owner
// edge. Reusing an operation identity with different input is rejected.
func (ledger *ReferenceLedger) ApplyReference(operation, name string, object ObjectID, retain bool) error {
	return ledger.ApplyReferences([]SnapshotReferenceMutation{{
		Operation: operation, Name: name, Object: object, Retain: retain,
	}})
}

// ApplyReferences applies an ordered batch of independent idempotent root or
// cross-owner edge mutations as one. This is semantically equivalent to
// ApplyReference for every element. A failure rejects the entire batch.
func (ledger *ReferenceLedger) ApplyReferences(mutations []SnapshotReferenceMutation) error {
	if ledger == nil {
		return errors.New("invalid reference-ledger mutation batch")
	}
	if len(mutations) == 0 {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.mutateLocked(func(state *referenceLedgerState) error {
		for _, mutation := range mutations {
			if mutation.Operation == "" {
				return errors.New("invalid reference-ledger operation")
			}
			want := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
			if previous, ok := state.Applied[mutation.Operation]; ok {
				if previous != want {
					return fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
				}
				continue
			}
			var err error
			if mutation.Retain {
				err = state.retain(mutation.Name, mutation.Object)
			} else {
				err = state.release(mutation.Name, mutation.Object)
			}
			if err != nil {
				return err
			}
			state.putApplied(mutation.Operation, want)
		}
		return nil
	})
}

// ApplyHandoff atomically advances one provider-to-owner catch-up cursor.
// Mutations must be contiguous and are individually idempotent by operation
// identity. Retrying an already committed prefix succeeds and changes
// nothing; skipping or reordering any mutation is rejected.
func (ledger *ReferenceLedger) ApplyHandoff(origin SnapshotID, from uint64, mutations []SnapshotReferenceMutation) error {
	if ledger == nil || origin == 0 {
		return errors.New("invalid reference-ledger handoff")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, ok := ledger.state.Origins[origin]; !ok {
		return fmt.Errorf("snapshot origin %d is not owned", origin)
	}
	current := ledger.state.HandoffCursor[origin]
	if from < current {
		// The caller may retry a previously committed whole batch. It is safe
		// only when that batch ends no later than the cursor.
		if uint64(len(mutations)) > math.MaxUint64-from || from+uint64(len(mutations)) > current {
			return fmt.Errorf("snapshot origin %d handoff overlaps cursor %d", origin, current)
		}
		want := from + 1
		for _, mutation := range mutations {
			if mutation.Sequence != want || mutation.Object.Snapshot != origin {
				return fmt.Errorf("snapshot origin %d retried handoff mutation %d, want %d", origin, mutation.Sequence, want)
			}
			applied := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
			if previous, ok := ledger.state.Applied[mutation.Operation]; !ok || previous != applied {
				return fmt.Errorf("snapshot origin %d handoff retry does not match mutation %d", origin, mutation.Sequence)
			}
			want++
		}
		return nil
	}
	if from != current {
		return fmt.Errorf("snapshot origin %d handoff starts at %d, want %d", origin, from, current)
	}
	if len(mutations) == 0 {
		return nil
	}
	return ledger.mutateLocked(func(state *referenceLedgerState) error {
		want := from + 1
		for _, mutation := range mutations {
			if mutation.Sequence != want || mutation.Object.Snapshot != origin {
				return fmt.Errorf("snapshot origin %d handoff mutation %d, want %d", origin, mutation.Sequence, want)
			}
			applied := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
			if previous, ok := state.Applied[mutation.Operation]; ok {
				if previous != applied {
					return fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
				}
			} else {
				var err error
				if mutation.Retain {
					err = state.retain(mutation.Name, mutation.Object)
				} else {
					err = state.release(mutation.Name, mutation.Object)
				}
				if err != nil {
					return err
				}
				state.putApplied(mutation.Operation, applied)
			}
			want++
		}
		state.putCursor(origin, want-1)
		return nil
	})
}

// PendingOutbound returns the ordered, unacknowledged cross-owner mutations.
func (ledger *ReferenceLedger) PendingOutbound() []OutboundReference {
	return ledger.PendingOutboundBatch(0)
}

// PendingOutboundBatch returns at most limit ordered, unacknowledged
// cross-owner mutations. A non-positive limit returns the complete outbox.
func (ledger *ReferenceLedger) PendingOutboundBatch(limit int) []OutboundReference {
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	if limit <= 0 || limit > len(ledger.state.Outbox) {
		limit = len(ledger.state.Outbox)
	}
	return append([]OutboundReference(nil), ledger.state.Outbox[:limit]...)
}

// OwnsEdge reports whether parent and child are both owned by this ledger and
// the immutable parent contains the exact slot. It lets an outbox dispatcher
// fold a queued cross-owner retain into an edge that became local while the
// destination origin was being imported.
func (ledger *ReferenceLedger) OwnsEdge(parent ObjectID, slot uint16, child ObjectID) bool {
	if ledger == nil {
		return false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	if !ledger.state.owns(parent) || !ledger.state.owns(child) {
		return false
	}
	object, ok := ledger.state.Objects[parent.key()]
	if !ok {
		return false
	}
	for _, edge := range object.Children {
		if edge.Slot == slot {
			return edge.Child == child
		}
	}
	return false
}

// AcknowledgeOutbound removes only the oldest mutation. Serial acknowledgement
// ensures a release can never overtake its edge's retain after retries.
func (ledger *ReferenceLedger) AcknowledgeOutbound(sequence uint64) error {
	ledger.mu.RLock()
	if sequence <= ledger.state.Acknowledged {
		ledger.mu.RUnlock()
		return nil
	}
	if len(ledger.state.Outbox) == 0 || ledger.state.Outbox[0].Sequence != sequence {
		var oldest uint64
		if len(ledger.state.Outbox) != 0 {
			oldest = ledger.state.Outbox[0].Sequence
		}
		ledger.mu.RUnlock()
		return fmt.Errorf("outbox acknowledgement %d does not select oldest sequence %d", sequence, oldest)
	}
	ledger.mu.RUnlock()
	return ledger.AcknowledgeOutboundThrough(sequence)
}

// AcknowledgeOutboundThrough atomically removes the contiguous delivered
// prefix ending at sequence. Repeating a committed acknowledgement succeeds;
// skipping beyond the current outbox is rejected.
func (ledger *ReferenceLedger) AcknowledgeOutboundThrough(sequence uint64) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.state.Outbox) == 0 {
		if sequence <= ledger.state.Acknowledged {
			return nil
		}
		return fmt.Errorf("outbox acknowledgement %d has no pending mutation", sequence)
	}
	if sequence <= ledger.state.Acknowledged {
		return nil
	}
	index := sort.Search(len(ledger.state.Outbox), func(i int) bool {
		return ledger.state.Outbox[i].Sequence >= sequence
	})
	if index == len(ledger.state.Outbox) || ledger.state.Outbox[index].Sequence != sequence {
		return fmt.Errorf("outbox acknowledgement %d is not pending", sequence)
	}
	return ledger.mutateLocked(func(state *referenceLedgerState) error {
		state.Outbox = append([]OutboundReference(nil), state.Outbox[index+1:]...)
		state.Acknowledged = sequence
		return nil
	})
}

// Collect reclaims zero-count immutable objects and cascades internal edges.
// Cross-owner deactivations enter the same ordered outbox as activations.
func (ledger *ReferenceLedger) Collect(limit int) (int, error) {
	if ledger == nil || limit <= 0 {
		return 0, nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	queue := make([]string, 0, len(ledger.state.reclaiming))
	for key := range ledger.state.reclaiming {
		queue = append(queue, key)
	}
	// A healthy live graph overwhelmingly has nothing to collect.
	if len(queue) == 0 {
		return 0, nil
	}
	collected := 0
	if err := ledger.mutateLocked(func(state *referenceLedgerState) error {
		sort.Strings(queue)
		queued := make(map[string]struct{}, len(queue))
		for _, key := range queue {
			queued[key] = struct{}{}
		}
		for len(queue) != 0 && collected < limit {
			key := queue[0]
			queue = queue[1:]
			parent, ok := state.Objects[key]
			if !ok || parent.State != ObjectReclaiming || parent.Refs != 0 {
				continue
			}
			for _, edge := range parent.Children {
				if state.owns(edge.Child) {
					child, ok := state.Objects[edge.Child.key()]
					if !ok || child.State != ObjectLive || child.Refs == 0 {
						return fmt.Errorf("reclaiming %s cannot release child %s", parent.ID, edge.Child)
					}
					child.Refs--
					if child.Refs == 0 {
						child.State = ObjectReclaiming
						if _, exists := queued[child.ID.key()]; !exists {
							queue = append(queue, child.ID.key())
							queued[child.ID.key()] = struct{}{}
						}
					}
					state.putObject(child.ID.key(), child)
				} else if err := state.enqueue(parent.ID, edge, false); err != nil {
					return err
				}
			}
			state.deleteObject(key)
			collected++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return collected, nil
}

// EmptyOrigins returns origins whose immutable object graph has been fully
// collected and whose cross-owner edge queue is drained. The shared-GC
// coordinator must commit permanent retirement in the authority before calling
// RetireOrigin; this method merely reports candidates.
func (ledger *ReferenceLedger) EmptyOrigins() []SnapshotID {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	blocked := make(map[SnapshotID]struct{})
	for origin, entry := range ledger.state.byOrigin {
		if len(entry.keys) != 0 {
			blocked[origin] = struct{}{}
		}
	}
	for _, reference := range ledger.state.References {
		blocked[reference.Snapshot] = struct{}{}
	}
	for _, outbound := range ledger.state.Outbox {
		blocked[outbound.Parent.Snapshot] = struct{}{}
		blocked[outbound.Child.Snapshot] = struct{}{}
	}
	result := make([]SnapshotID, 0, len(ledger.state.Origins))
	for origin := range ledger.state.Origins {
		if _, exists := blocked[origin]; !exists {
			result = append(result, origin)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// RetireOrigin forgets an empty origin after the authority has permanently fenced its
// snapshot ID. It is idempotent so a successor coordinator can finish a crash
// that occurred after the authority command. Physical packs are removed only after
// this ledger mutation succeeds.
func (ledger *ReferenceLedger) RetireOrigin(origin SnapshotID) error {
	if ledger == nil || origin == 0 {
		return errors.New("invalid reference-ledger origin retirement")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, exists := ledger.state.Origins[origin]; !exists {
		return nil
	}
	for key := range ledger.state.originKeys(origin) {
		return fmt.Errorf("snapshot origin %d still contains object %s", origin, ledger.state.Objects[key].ID)
	}
	for name, object := range ledger.state.References {
		if object.Snapshot == origin {
			return fmt.Errorf("snapshot origin %d still has reference %q", origin, name)
		}
	}
	for _, outbound := range ledger.state.Outbox {
		if outbound.Parent.Snapshot == origin || outbound.Child.Snapshot == origin {
			return fmt.Errorf("snapshot origin %d still has pending outbound reference %d", origin, outbound.Sequence)
		}
	}
	return ledger.mutateLocked(func(state *referenceLedgerState) error {
		state.setOrigin(origin, false)
		state.deleteCursor(origin)
		// Once the authority fences the immutable ID, retries are rejected before
		// reaching this ledger. Its old operation receipts no longer serve
		// deduplication and would otherwise grow without bound over
		// long-lived clusters.
		for operation, applied := range state.Applied {
			if applied.Object.Snapshot == origin {
				state.deleteApplied(operation)
			}
		}
		return nil
	})
}

// ReferenceLedgerInspection is a stable diagnostic snapshot.
type ReferenceLedgerInspection struct {
	Origins           []SnapshotID
	Objects           map[ObjectID]uint64
	References        map[string]ObjectID
	PendingOutbound   []OutboundReference
	AppliedOperations int
	HandoffCursors    map[SnapshotID]uint64
}

// OriginManifest returns the current immutable objects and complete incoming
// reference baseline for one owned origin. Cross-origin parent edges are
// represented as stable named references so the result can independently
// reconstruct the origin's exact counts after pack compaction or coordinator
// rebuild.
func (ledger *ReferenceLedger) OriginManifest(origin SnapshotID) ([]Object, map[string]ObjectID, bool) {
	if ledger == nil || origin == 0 {
		return nil, nil, false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	objects, references, ok, _ := ledger.originManifestLocked(origin)
	return objects, references, ok
}

// OriginCompactionManifest returns an exact physical-compaction view. ready is
// false while bounded collection has left any reclaiming object in the origin:
// live descendants can still carry references from those logically dead
// parents, so omitting the parents before their edge releases have cascaded
// would produce an inconsistent compacted manifest.
func (ledger *ReferenceLedger) OriginCompactionManifest(origin SnapshotID) (objects []Object, references map[string]ObjectID, ok, ready bool) {
	if ledger == nil || origin == 0 {
		return nil, nil, false, false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	return ledger.originManifestLocked(origin)
}

// OriginChanges returns, for each origin that has or had objects and is not
// retired, a count that advances whenever one of them stops being live. An
// origin whose count is the one OriginLiveObjects returned with its live
// objects still has those live objects.
func (ledger *ReferenceLedger) OriginChanges() map[SnapshotID]uint64 {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	result := make(map[SnapshotID]uint64, len(ledger.state.byOrigin))
	for origin, entry := range ledger.state.byOrigin {
		result[origin] = entry.changes
	}
	return result
}

// OriginLiveObjects returns the IDs of origin's live objects, and its
// OriginChanges count with them. ready is as for OriginCompactionManifest,
// whose view this is without the objects and the references: enough to decide
// whether the origin needs compacting.
func (ledger *ReferenceLedger) OriginLiveObjects(origin SnapshotID) (live map[ObjectID]struct{}, changes uint64, ok, ready bool) {
	if ledger == nil || origin == 0 {
		return nil, 0, false, false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	if _, ok := ledger.state.Origins[origin]; !ok {
		return nil, 0, false, false
	}
	ready = true
	keys := ledger.state.originKeys(origin)
	live = make(map[ObjectID]struct{}, len(keys))
	for key := range keys {
		switch object := ledger.state.Objects[key]; object.State {
		case ObjectReclaiming:
			ready = false
		case ObjectLive:
			live[object.ID] = struct{}{}
		}
	}
	if entry := ledger.state.byOrigin[origin]; entry != nil {
		changes = entry.changes
	}
	return live, changes, true, ready
}

// Origins returns the owned immutable origin IDs without copying the much
// larger object and reference tables. Callers that only schedule per-origin
// maintenance must not pay Inspect's object-proportional diagnostic cost.
func (ledger *ReferenceLedger) Origins() []SnapshotID {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	result := make([]SnapshotID, 0, len(ledger.state.Origins))
	for origin := range ledger.state.Origins {
		result = append(result, origin)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// References returns the ledger's named roots without copying its potentially
// much larger object table. Authority reconciliation is proportional to the
// number of branch roots, not to resident memory or filesystem object count.
func (ledger *ReferenceLedger) References() map[string]ObjectID {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	references := make(map[string]ObjectID, len(ledger.state.References))
	for name, object := range ledger.state.References {
		references[name] = object
	}
	return references
}

func (ledger *ReferenceLedger) originManifestLocked(origin SnapshotID) (objects []Object, references map[string]ObjectID, ok, ready bool) {
	if _, ok := ledger.state.Origins[origin]; !ok {
		return nil, nil, false, false
	}
	ready = true
	keys := ledger.state.originKeys(origin)
	objects = make([]Object, 0, len(keys))
	for key := range keys {
		// A bounded collection may leave descendants in ObjectReclaiming until
		// its next pass. They are already logically dead and must not be offered
		// to shared-pack compaction as part of the live origin view.
		object := ledger.state.Objects[key]
		if object.State == ObjectReclaiming {
			ready = false
			continue
		}
		if object.State == ObjectLive {
			objects = append(objects, cloneObject(object))
		}
	}
	// In key order, which is ID order.
	slices.SortFunc(objects, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	references = make(map[string]ObjectID)
	for name, object := range ledger.state.References {
		if object.Snapshot == origin {
			references[name] = object
		}
	}
	for _, parent := range ledger.state.Objects {
		if parent.ID.Snapshot == origin {
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == origin {
				references[edgeReferenceName(parent.ID, edge.Slot)] = edge.Child
			}
		}
	}
	return objects, references, true, ready
}

// Inspect reports exact owner-local counts without exposing mutable maps.
func (ledger *ReferenceLedger) Inspect() ReferenceLedgerInspection {
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	result := ReferenceLedgerInspection{
		Objects:           make(map[ObjectID]uint64, len(ledger.state.Objects)),
		References:        make(map[string]ObjectID, len(ledger.state.References)),
		PendingOutbound:   append([]OutboundReference(nil), ledger.state.Outbox...),
		AppliedOperations: len(ledger.state.Applied),
		HandoffCursors:    make(map[SnapshotID]uint64, len(ledger.state.HandoffCursor)),
	}
	for origin := range ledger.state.Origins {
		result.Origins = append(result.Origins, origin)
	}
	sort.Slice(result.Origins, func(i, j int) bool { return result.Origins[i] < result.Origins[j] })
	for _, object := range ledger.state.Objects {
		result.Objects[object.ID] = object.Refs
	}
	for name, object := range ledger.state.References {
		result.References[name] = object
	}
	for origin, cursor := range ledger.state.HandoffCursor {
		result.HandoffCursors[origin] = cursor
	}
	return result
}
