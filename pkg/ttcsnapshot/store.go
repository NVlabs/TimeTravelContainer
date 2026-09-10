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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

const (
	journalHeader        = "TTCSGJ1\n"
	maxJournalRecordSize = 8 << 20
)

type graphState struct {
	Version      int
	Sequence     uint64
	NextSnapshot SnapshotID
	Snapshots    map[string]Snapshot
	Objects      map[string]Object
	Roots        map[string]ObjectID
	Pins         map[string]ObjectID
}

func newGraphState() graphState {
	return graphState{
		Version:      formatVersion,
		NextSnapshot: 1,
		Snapshots:    make(map[string]Snapshot),
		Objects:      make(map[string]Object),
		Roots:        make(map[string]ObjectID),
		Pins:         make(map[string]ObjectID),
	}
}

type journalOp struct {
	PutSnapshot     *Snapshot
	PutObject       *Object
	DeleteObject    string
	RootName        string
	PutRoot         *ObjectID
	DeleteRoot      bool
	PinName         string
	PutPin          *ObjectID
	DeletePin       bool
	SetNextSnapshot *SnapshotID
}

type journalRecord struct {
	Version  int
	Sequence uint64
	Ops      []journalOp
}

// Store owns one authoritative logical snapshot graph.
type Store struct {
	mu       sync.RWMutex
	dir      string
	lock     *os.File
	journal  *os.File
	state    graphState
	poisoned error
}

// Open opens or creates a snapshot graph store at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("TTC snapshot graph directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "graph.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("locking TTC snapshot graph: %w", err)
	}
	store := &Store{dir: dir, lock: lock, state: newGraphState()}
	if err := store.loadCheckpoint(); err != nil {
		store.Close()
		return nil, err
	}
	journal, err := os.OpenFile(filepath.Join(dir, "graph.journal"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		store.Close()
		return nil, err
	}
	store.journal = journal
	if err := store.replayJournal(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the store lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.journal != nil {
		err = s.journal.Close()
		s.journal = nil
	}
	if s.lock != nil {
		if unlockErr := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); err == nil {
			err = unlockErr
		}
		if closeErr := s.lock.Close(); err == nil {
			err = closeErr
		}
		s.lock = nil
	}
	return err
}

func (s *Store) loadCheckpoint() error {
	data, err := os.ReadFile(filepath.Join(s.dir, "graph.checkpoint"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state graphState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decoding TTC snapshot graph checkpoint: %w", err)
	}
	if err := validateGraphState(&state); err != nil {
		return fmt.Errorf("invalid TTC snapshot graph checkpoint: %w", err)
	}
	s.state = state
	return nil
}

func validateGraphState(state *graphState) error {
	if state == nil || state.Version != formatVersion || state.NextSnapshot == 0 {
		return errors.New("invalid graph state header")
	}
	if state.Snapshots == nil || state.Objects == nil || state.Roots == nil || state.Pins == nil {
		return errors.New("graph state has nil maps")
	}
	for key, snapshot := range state.Snapshots {
		if key != snapshotKey(snapshot.ID) || snapshot.ID == 0 || !snapshot.State.valid() || snapshot.NextOrdinal == 0 {
			return fmt.Errorf("invalid snapshot %q", key)
		}
	}
	for key, object := range state.Objects {
		if key != object.ID.key() {
			return fmt.Errorf("object key %q does not match %s", key, object.ID.key())
		}
		if _, err := parseObjectKey(key); err != nil {
			return err
		}
		if err := validateObject(object); err != nil {
			return err
		}
		snapshot, ok := state.Snapshots[snapshotKey(object.ID.Snapshot)]
		if !ok || object.ID.Ordinal >= snapshot.NextOrdinal {
			return fmt.Errorf("object %s has invalid snapshot origin", key)
		}
	}
	expected := make(map[string]uint64, len(state.Objects))
	for name, id := range state.Roots {
		if err := validateRootName(name); err != nil {
			return err
		}
		if _, ok := state.Objects[id.key()]; !ok {
			return fmt.Errorf("root %q references missing object %s", name, id.key())
		}
		expected[id.key()]++
	}
	for name, id := range state.Pins {
		if err := validateRootName(name); err != nil {
			return fmt.Errorf("invalid pin: %w", err)
		}
		if _, ok := state.Objects[id.key()]; !ok {
			return fmt.Errorf("pin %q references missing object %s", name, id.key())
		}
		expected[id.key()]++
	}
	for _, object := range state.Objects {
		for _, edge := range object.Children {
			if _, ok := state.Objects[edge.Child.key()]; !ok {
				return fmt.Errorf("object %s references missing child %s", object.ID.key(), edge.Child.key())
			}
			expected[edge.Child.key()]++
		}
	}
	for key, object := range state.Objects {
		if object.Refs != expected[key] {
			return fmt.Errorf("object %s has %d references, graph has %d", key, object.Refs, expected[key])
		}
		if (object.Refs == 0) != (object.State == ObjectReclaiming) {
			return fmt.Errorf("object %s state %q disagrees with reference count %d", key, object.State, object.Refs)
		}
	}
	return nil
}

func snapshotKey(id SnapshotID) string {
	return fmt.Sprintf("%016x", uint64(id))
}

func (s *Store) replayJournal() error {
	info, err := s.journal.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := s.journal.WriteAt([]byte(journalHeader), 0); err != nil {
			return err
		}
		if err := s.journal.Sync(); err != nil {
			return err
		}
	}
	header := make([]byte, len(journalHeader))
	if _, err := s.journal.ReadAt(header, 0); err != nil {
		return err
	}
	if string(header) != journalHeader {
		return errors.New("invalid TTC snapshot graph journal header")
	}
	offset := int64(len(journalHeader))
	lengthBuffer := make([]byte, 4)
	for {
		recordStart := offset
		n, err := s.journal.ReadAt(lengthBuffer, offset)
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n != len(lengthBuffer) {
			return s.truncateJournal(recordStart)
		}
		offset += int64(len(lengthBuffer))
		length := binary.BigEndian.Uint32(lengthBuffer)
		if length == 0 || length > maxJournalRecordSize {
			return fmt.Errorf("invalid TTC snapshot graph journal record length %d at %d", length, recordStart)
		}
		data := make([]byte, length)
		n, err = s.journal.ReadAt(data, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n != len(data) {
			return s.truncateJournal(recordStart)
		}
		offset += int64(length)
		var record journalRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decoding TTC snapshot graph journal record at %d: %w", recordStart, err)
		}
		if record.Version != formatVersion {
			return fmt.Errorf("journal record version %d, want %d", record.Version, formatVersion)
		}
		if record.Sequence <= s.state.Sequence {
			continue
		}
		if record.Sequence != s.state.Sequence+1 {
			return fmt.Errorf("journal sequence %d follows %d", record.Sequence, s.state.Sequence)
		}
		if err := applyJournalRecord(&s.state, record); err != nil {
			return fmt.Errorf("applying journal sequence %d: %w", record.Sequence, err)
		}
	}
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}

func (s *Store) truncateJournal(offset int64) error {
	if err := s.journal.Truncate(offset); err != nil {
		return err
	}
	if err := s.journal.Sync(); err != nil {
		return err
	}
	_, err := s.journal.Seek(0, io.SeekEnd)
	return err
}

func applyJournalRecord(state *graphState, record journalRecord) error {
	next := cloneGraphState(*state)
	for _, op := range record.Ops {
		switch {
		case op.PutSnapshot != nil:
			next.Snapshots[snapshotKey(op.PutSnapshot.ID)] = *op.PutSnapshot
		case op.PutObject != nil:
			if err := validateObject(*op.PutObject); err != nil {
				return err
			}
			next.Objects[op.PutObject.ID.key()] = cloneObject(*op.PutObject)
		case op.DeleteObject != "":
			if _, err := parseObjectKey(op.DeleteObject); err != nil {
				return err
			}
			delete(next.Objects, op.DeleteObject)
		case op.PutRoot != nil:
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
			next.Roots[op.RootName] = *op.PutRoot
		case op.DeleteRoot:
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
			delete(next.Roots, op.RootName)
		case op.PutPin != nil:
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
			next.Pins[op.PinName] = *op.PutPin
		case op.DeletePin:
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
			delete(next.Pins, op.PinName)
		case op.SetNextSnapshot != nil:
			next.NextSnapshot = *op.SetNextSnapshot
		default:
			return errors.New("empty TTC snapshot graph journal operation")
		}
	}
	next.Sequence = record.Sequence
	if err := validateGraphState(&next); err != nil {
		return err
	}
	*state = next
	return nil
}

func cloneGraphState(state graphState) graphState {
	next := graphState{
		Version: state.Version, Sequence: state.Sequence, NextSnapshot: state.NextSnapshot,
		Snapshots: make(map[string]Snapshot, len(state.Snapshots)),
		Objects:   make(map[string]Object, len(state.Objects)),
		Roots:     make(map[string]ObjectID, len(state.Roots)),
		Pins:      make(map[string]ObjectID, len(state.Pins)),
	}
	for key, value := range state.Snapshots {
		next.Snapshots[key] = value
	}
	for key, value := range state.Objects {
		next.Objects[key] = cloneObject(value)
	}
	for key, value := range state.Roots {
		next.Roots[key] = value
	}
	for key, value := range state.Pins {
		next.Pins[key] = value
	}
	return next
}

func cloneObject(object Object) Object {
	object.Data = append([]byte(nil), object.Data...)
	object.Children = append([]Edge(nil), object.Children...)
	return object
}

func (s *Store) commitLocked(ops []journalOp) error {
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	record := journalRecord{Version: formatVersion, Sequence: s.state.Sequence + 1, Ops: ops}
	next := cloneGraphState(s.state)
	if err := applyJournalRecord(&next, record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxJournalRecordSize {
		return fmt.Errorf("TTC snapshot graph transaction is %d bytes, limit is %d", len(data), maxJournalRecordSize)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	start, err := s.journal.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		if err := s.journal.Truncate(start); err != nil {
			s.poisoned = fmt.Errorf("%v; journal rollback failed: %w", cause, err)
			return s.poisoned
		}
		_, _ = s.journal.Seek(start, io.SeekStart)
		return cause
	}
	if err := writeFull(s.journal, length[:]); err != nil {
		return rollback(err)
	}
	if err := writeFull(s.journal, data); err != nil {
		return rollback(err)
	}
	if err := s.journal.Sync(); err != nil {
		// The kernel may have persisted any prefix, all, or none of the record.
		// Do not issue another transaction until reopen resolves that outcome.
		s.poisoned = err
		return err
	}
	s.state = next
	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// AllocateWorking allocates a never-reused working snapshot origin.
func (s *Store) AllocateWorking() (SnapshotID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.state.NextSnapshot
	if id == 0 || id == SnapshotID(math.MaxUint64) {
		return 0, errors.New("TTC snapshot ID space exhausted")
	}
	next := id + 1
	snapshot := Snapshot{ID: id, State: SnapshotWorking, NextOrdinal: 1}
	if err := s.commitLocked([]journalOp{{PutSnapshot: &snapshot}, {SetNextSnapshot: &next}}); err != nil {
		return 0, err
	}
	return id, nil
}

// Seal freezes a working origin in O(1).
func (s *Store) Seal(id SnapshotID, state SnapshotState) error {
	if state != SnapshotSealedLocal && state != SnapshotSealedVolatile {
		return fmt.Errorf("invalid seal target %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(id)]
	if !ok {
		return fmt.Errorf("snapshot %d does not exist", id)
	}
	if snapshot.State != SnapshotWorking {
		return fmt.Errorf("snapshot %d is %s, want working", id, snapshot.State)
	}
	snapshot.State = state
	return s.commitLocked([]journalOp{{PutSnapshot: &snapshot}})
}

// SealAndFork atomically seals source, copies named logical roots, and
// allocates fresh working origins for every resumed source/destination branch.
// copies maps destination root names to existing source root names.
func (s *Store) SealAndFork(source SnapshotID, state SnapshotState, copies map[string]string, workingOrigins int) ([]SnapshotID, error) {
	if state != SnapshotSealedLocal && state != SnapshotSealedVolatile {
		return nil, fmt.Errorf("invalid seal target %q", state)
	}
	if len(copies) == 0 || workingOrigins <= 0 {
		return nil, errors.New("TTC snapshot fork requires roots and fresh working origins")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(source)]
	if !ok || snapshot.State != SnapshotWorking {
		return nil, fmt.Errorf("snapshot %d is not working", source)
	}
	destinations := make([]string, 0, len(copies))
	for destination, sourceName := range copies {
		if err := validateRootName(destination); err != nil {
			return nil, err
		}
		if err := validateRootName(sourceName); err != nil {
			return nil, err
		}
		if _, exists := s.state.Roots[destination]; exists {
			return nil, fmt.Errorf("destination root %q already exists", destination)
		}
		if _, exists := s.state.Roots[sourceName]; !exists {
			return nil, fmt.Errorf("source root %q does not exist", sourceName)
		}
		destinations = append(destinations, destination)
	}
	if uint64(workingOrigins) > math.MaxUint64-uint64(s.state.NextSnapshot) {
		return nil, errors.New("TTC snapshot ID space exhausted")
	}
	sort.Strings(destinations)
	touched := make(map[string]Object)
	ops := make([]journalOp, 0, len(copies)*2+workingOrigins+2)
	for _, destination := range destinations {
		id := s.state.Roots[copies[destination]]
		object, ok := touched[id.key()]
		if !ok {
			object = s.state.Objects[id.key()]
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return nil, fmt.Errorf("fork root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id.key()] = object
		ops = append(ops, journalOp{RootName: destination, PutRoot: &id})
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	objectOps := make([]journalOp, 0, len(keys))
	for _, key := range keys {
		object := touched[key]
		objectOps = append(objectOps, journalOp{PutObject: &object})
	}
	ops = append(objectOps, ops...)
	snapshot.State = state
	ops = append(ops, journalOp{PutSnapshot: &snapshot})
	allocated := make([]SnapshotID, workingOrigins)
	next := s.state.NextSnapshot
	for index := range allocated {
		allocated[index] = next
		working := Snapshot{ID: next, State: SnapshotWorking, NextOrdinal: 1}
		ops = append(ops, journalOp{PutSnapshot: &working})
		next++
	}
	ops = append(ops, journalOp{SetNextSnapshot: &next})
	if err := s.commitLocked(ops); err != nil {
		return nil, err
	}
	return allocated, nil
}

// CreateObject creates an object and a named construction pin. Every child
// edge is activated in the same bounded transaction, so recovery can derive
// every reference from durable graph structure.
func (s *Store) CreateObject(origin SnapshotID, pin string, kind ObjectKind, data []byte, children []Edge) (ObjectID, error) {
	if !kind.valid() {
		return ObjectID{}, fmt.Errorf("invalid TTC object kind %d", kind)
	}
	if err := validateRootName(pin); err != nil {
		return ObjectID{}, fmt.Errorf("invalid construction pin: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Pins[pin]; ok {
		return ObjectID{}, fmt.Errorf("construction pin %q already exists", pin)
	}
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok {
		return ObjectID{}, fmt.Errorf("snapshot %d does not exist", origin)
	}
	if snapshot.State != SnapshotWorking {
		return ObjectID{}, fmt.Errorf("snapshot %d is %s, want working", origin, snapshot.State)
	}
	if snapshot.NextOrdinal == 0 || snapshot.NextOrdinal == math.MaxUint64 {
		return ObjectID{}, fmt.Errorf("snapshot %d object ordinal space exhausted", origin)
	}
	id := ObjectID{Snapshot: origin, Ordinal: snapshot.NextOrdinal, Kind: kind}
	snapshot.NextOrdinal++
	object := Object{ID: id, State: ObjectLive, Refs: 1, Data: append([]byte(nil), data...), Children: append([]Edge(nil), children...)}
	if err := validateObject(object); err != nil {
		return ObjectID{}, err
	}
	ops := make([]journalOp, 0, len(children)+3)
	for _, edge := range object.Children {
		child, ok := s.state.Objects[edge.Child.key()]
		if !ok {
			return ObjectID{}, fmt.Errorf("child object %s does not exist", edge.Child.key())
		}
		if child.State != ObjectLive || child.Refs == math.MaxUint64 {
			return ObjectID{}, fmt.Errorf("child object %s cannot be retained", edge.Child.key())
		}
		child.Refs++
		ops = append(ops, journalOp{PutObject: &child})
	}
	ops = append(ops, journalOp{PutObject: &object}, journalOp{PutSnapshot: &snapshot}, journalOp{PinName: pin, PutPin: &id})
	if err := s.commitLocked(ops); err != nil {
		return ObjectID{}, err
	}
	return id, nil
}

// ReserveObject durably allocates an object ID before bulk bytes are written.
// A crash may leave an unused ordinal, but IDs are never reused and no graph
// reference exists until CommitReservedObject succeeds.
func (s *Store) ReserveObject(origin SnapshotID, kind ObjectKind) (ObjectID, error) {
	if !kind.valid() {
		return ObjectID{}, fmt.Errorf("invalid TTC object kind %d", kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
	if !ok || snapshot.State != SnapshotWorking {
		return ObjectID{}, fmt.Errorf("snapshot %d is not working", origin)
	}
	if snapshot.NextOrdinal == 0 || snapshot.NextOrdinal == math.MaxUint64 {
		return ObjectID{}, fmt.Errorf("snapshot %d object ordinal space exhausted", origin)
	}
	id := ObjectID{Snapshot: origin, Ordinal: snapshot.NextOrdinal, Kind: kind}
	snapshot.NextOrdinal++
	if err := s.commitLocked([]journalOp{{PutSnapshot: &snapshot}}); err != nil {
		return ObjectID{}, err
	}
	return id, nil
}

// CommitReservedObject makes a reserved ID reachable through pin. Bulk data
// must already have been atomically installed before this call.
func (s *Store) CommitReservedObject(id ObjectID, pin string, bulkLength uint64, data []byte, children []Edge) error {
	if err := id.validate(); err != nil {
		return err
	}
	if err := validateRootName(pin); err != nil {
		return fmt.Errorf("invalid construction pin: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(id.Snapshot)]
	if !ok || snapshot.State != SnapshotWorking || id.Ordinal >= snapshot.NextOrdinal {
		return fmt.Errorf("object ID %s is not reserved in a working snapshot", id.key())
	}
	if _, ok := s.state.Objects[id.key()]; ok {
		return fmt.Errorf("object %s already exists", id.key())
	}
	if _, ok := s.state.Pins[pin]; ok {
		return fmt.Errorf("construction pin %q already exists", pin)
	}
	object := Object{ID: id, State: ObjectLive, Refs: 1, BulkLength: bulkLength, Data: append([]byte(nil), data...), Children: append([]Edge(nil), children...)}
	if err := validateObject(object); err != nil {
		return err
	}
	ops := make([]journalOp, 0, len(children)+2)
	for _, edge := range object.Children {
		child, ok := s.state.Objects[edge.Child.key()]
		if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
			return fmt.Errorf("child object %s cannot be retained", edge.Child.key())
		}
		child.Refs++
		ops = append(ops, journalOp{PutObject: &child})
	}
	ops = append(ops, journalOp{PutObject: &object}, journalOp{PinName: pin, PutPin: &id})
	return s.commitLocked(ops)
}

// Object returns a copy of object metadata.
func (s *Store) Object(id ObjectID) (Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[id.key()]
	return cloneObject(object), ok
}

// SnapshotObjects returns a stable copy of every graph object whose immutable
// identity was allocated by snapshot. It does not walk roots or ancestry.
func (s *Store) SnapshotObjects(snapshot SnapshotID) ([]Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.state.Snapshots[snapshotKey(snapshot)]; !ok {
		return nil, os.ErrNotExist
	}
	objects := make([]Object, 0)
	for _, object := range s.state.Objects {
		if object.ID.Snapshot == snapshot {
			objects = append(objects, cloneObject(object))
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].ID.key() < objects[j].ID.key()
	})
	return objects, nil
}

// Root returns a named root.
func (s *Store) Root(name string) (ObjectID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.state.Roots[name]
	return id, ok
}

// SetPin atomically replaces a named transaction/open/cache pin.
func (s *Store) SetPin(name string, id ObjectID) error {
	if err := validateRootName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.state.Pins[name]; ok && previous == id {
		return nil
	}
	object, ok := s.state.Objects[id.key()]
	if !ok {
		return fmt.Errorf("object %s does not exist", id.key())
	}
	if object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("object %s cannot be retained", id.key())
	}
	object.Refs++
	ops := []journalOp{{PutObject: &object}, {PinName: name, PutPin: &id}}
	if previous, ok := s.state.Pins[name]; ok {
		old, err := releaseObject(s.state.Objects[previous.key()])
		if err != nil {
			return err
		}
		ops = append(ops, journalOp{PutObject: &old})
	}
	return s.commitLocked(ops)
}

func releaseObject(object Object) (Object, error) {
	if object.State != ObjectLive || object.Refs == 0 {
		return Object{}, fmt.Errorf("object %s cannot be released", object.ID.key())
	}
	object.Refs--
	if object.Refs == 0 {
		object.State = ObjectReclaiming
	}
	return object, nil
}

// DeletePin removes a named transaction/open/cache pin.
func (s *Store) DeletePin(name string) error {
	if err := validateRootName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.state.Pins[name]
	if !ok {
		return os.ErrNotExist
	}
	object, ok := s.state.Objects[id.key()]
	if !ok {
		return fmt.Errorf("object %s does not exist", id.key())
	}
	object, err := releaseObject(object)
	if err != nil {
		return err
	}
	return s.commitLocked([]journalOp{{PinName: name, DeletePin: true}, {PutObject: &object}})
}

// SetRoot atomically replaces a named root and its reference.
func (s *Store) SetRoot(name string, id ObjectID) error {
	if err := validateRootName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.state.Roots[name]; ok && previous == id {
		return nil
	}
	object, ok := s.state.Objects[id.key()]
	if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("root object %s cannot be retained", id.key())
	}
	object.Refs++
	ops := []journalOp{{PutObject: &object}, {RootName: name, PutRoot: &id}}
	if previous, ok := s.state.Roots[name]; ok {
		old := s.state.Objects[previous.key()]
		var err error
		old, err = releaseObject(old)
		if err != nil {
			return err
		}
		ops = append(ops, journalOp{PutObject: &old})
	}
	return s.commitLocked(ops)
}

// DeleteRoot removes a named root and releases its object.
func (s *Store) DeleteRoot(name string) error {
	if err := validateRootName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.state.Roots[name]
	if !ok {
		return os.ErrNotExist
	}
	object, err := releaseObject(s.state.Objects[id.key()])
	if err != nil {
		return err
	}
	return s.commitLocked([]journalOp{{RootName: name, DeleteRoot: true}, {PutObject: &object}})
}

// Collect reclaims at most limit zero-count objects and cascades their child
// edge releases. It returns the number of physical object records removed.
func (s *Store) Collect(limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	collected := 0
	for collected < limit {
		keys := make([]string, 0)
		for key, object := range s.state.Objects {
			if object.State == ObjectReclaiming {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			break
		}
		sort.Strings(keys)
		object := s.state.Objects[keys[0]]
		ops := make([]journalOp, 0, len(object.Children)+1)
		for _, edge := range object.Children {
			child, ok := s.state.Objects[edge.Child.key()]
			if !ok {
				return collected, fmt.Errorf("reclaiming object %s references missing child %s", object.ID.key(), edge.Child.key())
			}
			var err error
			child, err = releaseObject(child)
			if err != nil {
				return collected, err
			}
			ops = append(ops, journalOp{PutObject: &child})
		}
		ops = append(ops, journalOp{DeleteObject: object.ID.key()})
		if err := s.commitLocked(ops); err != nil {
			return collected, err
		}
		collected++
	}
	return collected, nil
}

// Checkpoint atomically writes a compact graph checkpoint and resets the WAL.
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.dir, ".graph-checkpoint-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(s.dir, "graph.checkpoint")); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	if err := directory.Close(); err != nil {
		return err
	}
	if err := s.journal.Truncate(0); err != nil {
		return err
	}
	if _, err := s.journal.WriteAt([]byte(journalHeader), 0); err != nil {
		return err
	}
	if err := s.journal.Sync(); err != nil {
		return err
	}
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}

// Stats summarizes authoritative graph state.
type Stats struct {
	Snapshots  int
	Objects    int
	Roots      int
	Pins       int
	Reclaiming int
	Sequence   uint64
}

// Inspection is a physical-path-free diagnostic view of the logical graph.
type Inspection struct {
	Sequence  uint64
	Snapshots []Snapshot
	Roots     map[string]ObjectID
	Pins      map[string]ObjectID
	Objects   []Object
}

// Inspect returns a consistent logical graph view. Callers must not infer
// physical placement or authority from object ordering.
func (s *Store) Inspect() Inspection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := Inspection{
		Sequence: s.state.Sequence,
		Roots:    make(map[string]ObjectID, len(s.state.Roots)),
		Pins:     make(map[string]ObjectID, len(s.state.Pins)),
	}
	for _, snapshot := range s.state.Snapshots {
		result.Snapshots = append(result.Snapshots, snapshot)
	}
	sort.Slice(result.Snapshots, func(i, j int) bool { return result.Snapshots[i].ID < result.Snapshots[j].ID })
	for name, id := range s.state.Roots {
		result.Roots[name] = id
	}
	for name, id := range s.state.Pins {
		result.Pins[name] = id
	}
	for _, object := range s.state.Objects {
		result.Objects = append(result.Objects, cloneObject(object))
	}
	sort.Slice(result.Objects, func(i, j int) bool { return result.Objects[i].ID.key() < result.Objects[j].ID.key() })
	return result
}

// Verify checks all graph, reference, and state invariants.
func (s *Store) Verify() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := cloneGraphState(s.state)
	return validateGraphState(&state)
}

// Stats returns a consistent graph summary.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats := Stats{Snapshots: len(s.state.Snapshots), Objects: len(s.state.Objects), Roots: len(s.state.Roots), Pins: len(s.state.Pins), Sequence: s.state.Sequence}
	for _, object := range s.state.Objects {
		if object.State == ObjectReclaiming {
			stats.Reclaiming++
		}
	}
	return stats
}
