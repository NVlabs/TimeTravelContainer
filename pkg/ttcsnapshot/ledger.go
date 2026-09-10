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
	"bytes"
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
	referenceLedgerHeader    = "TTCRL1\n"
	maxReferenceLedgerRecord = 8 << 20
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
	Origins      map[SnapshotID]struct{}
	Objects      map[string]Object
	References   map[string]ObjectID
	Applied      map[string]appliedReference
	Outbox       []OutboundReference
	NextSequence uint64
	Acknowledged uint64
}

type ledgerImportRecord struct {
	Origin     SnapshotID
	Objects    []Object
	References map[string]ObjectID
}

type ledgerReferenceRecord struct {
	Operation string
	Name      string
	Object    ObjectID
	Retain    bool
}

type referenceLedgerRecord struct {
	Version   int
	Sequence  uint64
	Import    *ledgerImportRecord
	Reference *ledgerReferenceRecord
	Ack       *uint64
	Collect   *int
	Collected int
}

func newReferenceLedgerState() referenceLedgerState {
	return referenceLedgerState{
		Origins: make(map[SnapshotID]struct{}), Objects: make(map[string]Object),
		References: make(map[string]ObjectID), Applied: make(map[string]appliedReference), NextSequence: 1,
	}
}

func cloneReferenceLedgerState(state referenceLedgerState) referenceLedgerState {
	result := newReferenceLedgerState()
	result.NextSequence = state.NextSequence
	result.Acknowledged = state.Acknowledged
	for origin := range state.Origins {
		result.Origins[origin] = struct{}{}
	}
	for key, object := range state.Objects {
		result.Objects[key] = cloneObject(object)
	}
	for name, object := range state.References {
		result.References[name] = object
	}
	for operation, applied := range state.Applied {
		result.Applied[operation] = applied
	}
	result.Outbox = append([]OutboundReference(nil), state.Outbox...)
	return result
}

// ReferenceLedger is the common exact-counting engine used by volatile
// providers and the elected shared-GC coordinator. Durable wrappers journal
// its bounded mutations; this type contains no filesystem or Raft policy.
type ReferenceLedger struct {
	mu       sync.RWMutex
	state    referenceLedgerState
	dir      string
	lock     *os.File
	journal  *os.File
	sequence uint64
	poisoned error
}

// NewReferenceLedger returns an empty owner ledger.
func NewReferenceLedger() *ReferenceLedger {
	return &ReferenceLedger{state: newReferenceLedgerState()}
}

// OpenReferenceLedger opens a durable owner ledger. Complete mutations are
// length-framed and fsynced; an incomplete final frame is discarded on reopen.
// The directory lock ensures exactly one provider/coordinator process applies
// counts at a time.
func OpenReferenceLedger(dir string) (*ReferenceLedger, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("reference ledger directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() != "ledger.lock" && entry.Name() != "ledger.log" {
			return nil, fmt.Errorf("reference ledger contains unknown entry %q", entry.Name())
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, "ledger.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*ReferenceLedger, error) { lock.Close(); return nil, err }
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("locking reference ledger: %w", err))
	}
	journal, err := os.OpenFile(filepath.Join(dir, "ledger.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	ledger := &ReferenceLedger{state: newReferenceLedgerState(), dir: dir, lock: lock, journal: journal}
	if err := ledger.replay(); err != nil {
		ledger.Close()
		return nil, err
	}
	return ledger, nil
}

func (ledger *ReferenceLedger) replay() error {
	info, err := ledger.journal.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := ledger.journal.WriteString(referenceLedgerHeader); err != nil {
			return err
		}
		if err := ledger.journal.Sync(); err != nil {
			return err
		}
		return syncDirectory(ledger.dir)
	}
	header := make([]byte, len(referenceLedgerHeader))
	if _, err := ledger.journal.ReadAt(header, 0); err != nil || string(header) != referenceLedgerHeader {
		return fmt.Errorf("invalid reference ledger header: %w", err)
	}
	temporary := NewReferenceLedger()
	offset := int64(len(referenceLedgerHeader))
	expected := uint64(1)
	for {
		var framing [8]byte
		n, err := ledger.journal.ReadAt(framing[:], offset)
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil || n != len(framing) {
			if err := ledger.journal.Truncate(offset); err != nil {
				return err
			}
			break
		}
		length := binary.BigEndian.Uint64(framing[:])
		if length == 0 || length > maxReferenceLedgerRecord {
			return fmt.Errorf("invalid reference ledger record length %d", length)
		}
		data := make([]byte, length)
		n, err = ledger.journal.ReadAt(data, offset+8)
		if err != nil || uint64(n) != length {
			if err := ledger.journal.Truncate(offset); err != nil {
				return err
			}
			break
		}
		var record referenceLedgerRecord
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&record)
		trailingErr := decoder.Decode(&struct{}{})
		if decodeErr != nil || trailingErr != io.EOF || record.Version != 1 || record.Sequence != expected {
			return fmt.Errorf("invalid reference ledger record %d (decode=%v trailing=%v version=%d sequence=%d)", expected, decodeErr, trailingErr, record.Version, record.Sequence)
		}
		if err := replayReferenceLedgerRecord(temporary, record); err != nil {
			return fmt.Errorf("replaying reference ledger record %d: %w", expected, err)
		}
		offset += 8 + int64(length)
		expected++
	}
	ledger.state = cloneReferenceLedgerState(temporary.state)
	ledger.sequence = expected - 1
	_, err = ledger.journal.Seek(offset, io.SeekStart)
	return err
}

func replayReferenceLedgerRecord(ledger *ReferenceLedger, record referenceLedgerRecord) error {
	set := 0
	if record.Import != nil {
		set++
		if err := ledger.ImportOrigin(record.Import.Origin, record.Import.Objects, record.Import.References); err != nil {
			return err
		}
	}
	if record.Reference != nil {
		set++
		r := record.Reference
		if err := ledger.ApplyReference(r.Operation, r.Name, r.Object, r.Retain); err != nil {
			return err
		}
	}
	if record.Ack != nil {
		set++
		if err := ledger.AcknowledgeOutbound(*record.Ack); err != nil {
			return err
		}
	}
	if record.Collect != nil {
		set++
		collected, err := ledger.Collect(*record.Collect)
		if err != nil {
			return err
		}
		if collected != record.Collected {
			return fmt.Errorf("collection replay removed %d objects, want %d", collected, record.Collected)
		}
	}
	if set != 1 {
		return errors.New("reference ledger record must contain exactly one mutation")
	}
	return nil
}

func (ledger *ReferenceLedger) commitLocked(record referenceLedgerRecord, next referenceLedgerState) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	if ledger.journal == nil {
		ledger.state = next
		return nil
	}
	if ledger.sequence == math.MaxUint64 {
		return errors.New("reference ledger sequence exhausted")
	}
	record.Version = 1
	record.Sequence = ledger.sequence + 1
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding reference ledger record: %w", err)
	}
	if len(data) == 0 || len(data) > maxReferenceLedgerRecord {
		return fmt.Errorf("reference ledger record has invalid length %d", len(data))
	}
	var framing [8]byte
	binary.BigEndian.PutUint64(framing[:], uint64(len(data)))
	encoded := append(framing[:], data...)
	var written int
	if written, err = ledger.journal.Write(encoded); err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = ledger.journal.Sync()
	}
	if err != nil {
		ledger.poisoned = err
		return err
	}
	ledger.sequence = record.Sequence
	ledger.state = next
	return nil
}

// Close releases a durable ledger. It is harmless for an in-memory ledger.
func (ledger *ReferenceLedger) Close() error {
	if ledger == nil {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var result error
	if ledger.journal != nil {
		result = ledger.journal.Close()
		ledger.journal = nil
	}
	if ledger.lock != nil {
		result = errors.Join(result, syscall.Flock(int(ledger.lock.Fd()), syscall.LOCK_UN), ledger.lock.Close())
		ledger.lock = nil
	}
	return result
}

func edgeReferenceName(parent ObjectID, slot uint16) string {
	return fmt.Sprintf("edge/%s/%04x", parent.key(), slot)
}

func edgeOperationName(parent ObjectID, slot uint16, retain bool) string {
	action := "release"
	if retain {
		action = "retain"
	}
	return fmt.Sprintf("%s:%s:%04x", action, parent.key(), slot)
}

func (state *referenceLedgerState) owns(id ObjectID) bool {
	_, ok := state.Origins[id.Snapshot]
	return ok
}

func (state *referenceLedgerState) retain(name string, id ObjectID) error {
	if err := validateRootName(name); err != nil {
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
	state.Objects[id.key()] = object
	state.References[name] = id
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
	state.Objects[id.key()] = object
	delete(state.References, name)
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
		return fmt.Errorf("snapshot origin %d is already owned", origin)
	}
	next := cloneReferenceLedgerState(ledger.state)
	next.Origins[origin] = struct{}{}
	for _, input := range objects {
		object := cloneObject(input)
		if object.ID.Snapshot != origin {
			return fmt.Errorf("origin %d import contains foreign object %s", origin, object.ID)
		}
		object.State, object.Refs = ObjectLive, 0
		if err := validateObject(object); err != nil {
			return err
		}
		if _, exists := next.Objects[object.ID.key()]; exists {
			return fmt.Errorf("origin import repeats object %s", object.ID)
		}
		next.Objects[object.ID.key()] = object
	}
	for _, input := range objects {
		parent := next.Objects[input.ID.key()]
		for _, edge := range parent.Children {
			if next.owns(edge.Child) {
				child, ok := next.Objects[edge.Child.key()]
				if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
					return fmt.Errorf("object %s references unavailable owned child %s", parent.ID, edge.Child)
				}
				child.Refs++
				next.Objects[edge.Child.key()] = child
			} else if err := next.enqueue(parent.ID, edge, true); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := next.retain(name, references[name]); err != nil {
			return fmt.Errorf("importing reference %q: %w", name, err)
		}
	}
	for key, object := range next.Objects {
		if object.ID.Snapshot == origin && object.Refs == 0 {
			object.State = ObjectReclaiming
			next.Objects[key] = object
		}
	}
	return ledger.commitLocked(referenceLedgerRecord{Import: &ledgerImportRecord{Origin: origin, Objects: objects, References: references}}, next)
}

// ApplyReference applies one idempotent local root or delivered cross-owner
// edge. Reusing an operation identity with different input is rejected.
func (ledger *ReferenceLedger) ApplyReference(operation, name string, object ObjectID, retain bool) error {
	if ledger == nil || operation == "" {
		return errors.New("invalid reference-ledger operation")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	want := appliedReference{Name: name, Object: object, Retain: retain}
	if previous, ok := ledger.state.Applied[operation]; ok {
		if previous != want {
			return fmt.Errorf("reference operation %q was reused with different input", operation)
		}
		return nil
	}
	next := cloneReferenceLedgerState(ledger.state)
	var err error
	if retain {
		err = next.retain(name, object)
	} else {
		err = next.release(name, object)
	}
	if err != nil {
		return err
	}
	next.Applied[operation] = want
	return ledger.commitLocked(referenceLedgerRecord{Reference: &ledgerReferenceRecord{
		Operation: operation, Name: name, Object: object, Retain: retain,
	}}, next)
}

// PendingOutbound returns the ordered, unacknowledged cross-owner mutations.
func (ledger *ReferenceLedger) PendingOutbound() []OutboundReference {
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	return append([]OutboundReference(nil), ledger.state.Outbox...)
}

// AcknowledgeOutbound removes only the oldest mutation. Serial acknowledgement
// ensures a release can never overtake its edge's retain after retries.
func (ledger *ReferenceLedger) AcknowledgeOutbound(sequence uint64) error {
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
	if ledger.state.Outbox[0].Sequence != sequence {
		return fmt.Errorf("outbox acknowledgement %d does not select oldest sequence %d", sequence, ledger.state.Outbox[0].Sequence)
	}
	next := cloneReferenceLedgerState(ledger.state)
	next.Outbox = append([]OutboundReference(nil), next.Outbox[1:]...)
	next.Acknowledged = sequence
	return ledger.commitLocked(referenceLedgerRecord{Ack: &sequence}, next)
}

// Collect reclaims zero-count immutable objects and cascades internal edges.
// Cross-owner deactivations enter the same ordered outbox as activations.
func (ledger *ReferenceLedger) Collect(limit int) (int, error) {
	if ledger == nil || limit <= 0 {
		return 0, nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	next := cloneReferenceLedgerState(ledger.state)
	queue := make([]string, 0)
	for key, object := range next.Objects {
		if object.State == ObjectReclaiming {
			queue = append(queue, key)
		}
	}
	sort.Strings(queue)
	queued := make(map[string]struct{}, len(queue))
	for _, key := range queue {
		queued[key] = struct{}{}
	}
	collected := 0
	for len(queue) != 0 && collected < limit {
		key := queue[0]
		queue = queue[1:]
		parent, ok := next.Objects[key]
		if !ok || parent.State != ObjectReclaiming || parent.Refs != 0 {
			continue
		}
		for _, edge := range parent.Children {
			if next.owns(edge.Child) {
				child, ok := next.Objects[edge.Child.key()]
				if !ok || child.State != ObjectLive || child.Refs == 0 {
					return 0, fmt.Errorf("reclaiming %s cannot release child %s", parent.ID, edge.Child)
				}
				child.Refs--
				if child.Refs == 0 {
					child.State = ObjectReclaiming
					if _, exists := queued[child.ID.key()]; !exists {
						queue = append(queue, child.ID.key())
						queued[child.ID.key()] = struct{}{}
					}
				}
				next.Objects[child.ID.key()] = child
			} else if err := next.enqueue(parent.ID, edge, false); err != nil {
				return 0, err
			}
		}
		delete(next.Objects, key)
		collected++
	}
	if err := ledger.commitLocked(referenceLedgerRecord{Collect: &limit, Collected: collected}, next); err != nil {
		return 0, err
	}
	return collected, nil
}

// ReferenceLedgerInspection is a stable diagnostic snapshot.
type ReferenceLedgerInspection struct {
	Origins           []SnapshotID
	Objects           map[ObjectID]uint64
	References        map[string]ObjectID
	PendingOutbound   []OutboundReference
	AppliedOperations int
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
	return result
}
