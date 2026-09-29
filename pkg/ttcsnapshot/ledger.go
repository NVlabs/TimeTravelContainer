// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	referenceLedgerHeader    = "TTCRL2\n"
	maxReferenceLedgerRecord = 8 << 20
	referenceLedgerVersion   = 2
)

type referenceLedgerCheckpoint struct {
	Version  int                  `json:"version"`
	Sequence uint64               `json:"sequence"`
	State    referenceLedgerState `json:"state"`
}

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

type ledgerImportRecord struct {
	Origin     SnapshotID
	Objects    []Object
	References map[string]ObjectID
}

type ledgerImportBeginRecord struct {
	Origin     SnapshotID
	Objects    uint64
	References uint64
}

type ledgerImportObjectsRecord struct {
	Origin  SnapshotID
	Offset  uint64
	Objects []Object
}

type ledgerNamedReference struct {
	Name   string
	Object ObjectID
}

type ledgerImportReferencesRecord struct {
	Origin     SnapshotID
	Offset     uint64
	References []ledgerNamedReference
}

type ledgerImportCommitRecord struct {
	Origin SnapshotID
}

type ledgerReferenceRecord struct {
	Operation string
	Name      string
	Object    ObjectID
	Retain    bool
}

type ledgerHandoffRecord struct {
	Origin    SnapshotID
	From      uint64
	Mutations []SnapshotReferenceMutation
}

type referenceLedgerRecord struct {
	Version          int
	Sequence         uint64
	Import           *ledgerImportRecord
	ImportBegin      *ledgerImportBeginRecord
	ImportObjects    *ledgerImportObjectsRecord
	ImportReferences *ledgerImportReferencesRecord
	ImportCommit     *ledgerImportCommitRecord
	Reference        *ledgerReferenceRecord
	References       []ledgerReferenceRecord
	Handoff          *ledgerHandoffRecord
	Ack              *uint64
	Collect          *int
	Collected        int
	Retire           *SnapshotID
}

type pendingLedgerImport struct {
	begin         ledgerImportBeginRecord
	objects       []Object
	references    map[string]ObjectID
	journalOffset int64
	priorSequence uint64
}

func newReferenceLedgerState() referenceLedgerState {
	return referenceLedgerState{
		Origins: make(map[SnapshotID]struct{}), Objects: make(map[string]Object),
		References: make(map[string]ObjectID), Applied: make(map[string]appliedReference), NextSequence: 1,
		HandoffCursor: make(map[SnapshotID]uint64),
		byOrigin:      make(map[SnapshotID]*ledgerOriginObjects), reclaiming: make(map[string]struct{}),
	}
}

func cloneReferenceLedgerState(state referenceLedgerState) referenceLedgerState {
	result := newReferenceLedgerState()
	result.NextSequence = state.NextSequence
	result.Acknowledged = state.Acknowledged
	for origin, cursor := range state.HandoffCursor {
		result.HandoffCursor[origin] = cursor
	}
	for origin := range state.Origins {
		result.Origins[origin] = struct{}{}
	}
	for key, object := range state.Objects {
		cloned := cloneObject(object)
		result.Objects[key] = cloned
		result.indexObject(key, nil, &cloned)
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

// ledgerUndo holds the prior values of what a mutation of a
// referenceLedgerState has changed, so that a mutation refused part way, or
// one whose journal record is not written, can be undone. A mutation works in
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

// ReferenceLedger is the common exact-counting engine used by volatile
// providers and the elected shared-GC coordinator. Durable wrappers journal
// its bounded mutations; this type contains no filesystem or authority policy.
type ReferenceLedger struct {
	mu       sync.RWMutex
	state    referenceLedgerState
	dir      string
	lock     *os.File
	journal  *os.File
	sequence uint64
	poisoned error
	// checkpointBytes is the size of the checkpoint last written or read.
	checkpointBytes int64
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
		if entry.Name() != "ledger.lock" && entry.Name() != "ledger.log" && entry.Name() != "ledger.checkpoint" &&
			!strings.HasPrefix(entry.Name(), ".ledger-checkpoint-") && !strings.HasPrefix(entry.Name(), ".ledger-log-") {
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
	if err := ledger.loadCheckpoint(); err != nil {
		ledger.Close()
		return nil, err
	}
	if err := ledger.replay(); err != nil {
		ledger.Close()
		return nil, err
	}
	return ledger, nil
}

func (ledger *ReferenceLedger) loadCheckpoint() error {
	data, err := os.ReadFile(filepath.Join(ledger.dir, "ledger.checkpoint"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var checkpoint referenceLedgerCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&checkpoint)
	trailingErr := decoder.Decode(&struct{}{})
	if decodeErr != nil || trailingErr != io.EOF || checkpoint.Version != referenceLedgerVersion {
		return fmt.Errorf("invalid reference ledger checkpoint (decode=%v trailing=%v version=%d sequence=%d)", decodeErr, trailingErr, checkpoint.Version, checkpoint.Sequence)
	}
	if err := validateReferenceLedgerState(checkpoint.State); err != nil {
		return fmt.Errorf("validating reference ledger checkpoint: %w", err)
	}
	ledger.state = cloneReferenceLedgerState(checkpoint.State)
	ledger.sequence = checkpoint.Sequence
	ledger.checkpointBytes = int64(len(data))
	return nil
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
	temporary := &ReferenceLedger{state: cloneReferenceLedgerState(ledger.state), sequence: ledger.sequence}
	offset := int64(len(referenceLedgerHeader))
	var pending *pendingLedgerImport
	truncateTail := func(tail int64) error {
		if pending != nil {
			tail = pending.journalOffset
			temporary.sequence = pending.priorSequence
			pending = nil
		}
		if err := ledger.journal.Truncate(tail); err != nil {
			return err
		}
		offset = tail
		return ledger.journal.Sync()
	}
	for {
		var framing [8]byte
		n, err := ledger.journal.ReadAt(framing[:], offset)
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil || n != len(framing) {
			if err := truncateTail(offset); err != nil {
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
			if err := truncateTail(offset); err != nil {
				return err
			}
			break
		}
		var record referenceLedgerRecord
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&record)
		trailingErr := decoder.Decode(&struct{}{})
		if decodeErr != nil || trailingErr != io.EOF || record.Version != referenceLedgerVersion || record.Sequence == 0 {
			return fmt.Errorf("invalid reference ledger record (decode=%v trailing=%v version=%d sequence=%d)", decodeErr, trailingErr, record.Version, record.Sequence)
		}
		if record.Sequence > temporary.sequence {
			if record.Sequence != temporary.sequence+1 {
				return fmt.Errorf("reference ledger record %d follows %d", record.Sequence, temporary.sequence)
			}
			if err := replayReferenceLedgerRecordWithImport(temporary, record, offset, &pending); err != nil {
				return fmt.Errorf("replaying reference ledger record %d: %w", record.Sequence, err)
			}
			temporary.sequence = record.Sequence
		}
		offset += 8 + int64(length)
	}
	if pending != nil {
		if err := truncateTail(pending.journalOffset); err != nil {
			return err
		}
	}
	ledger.state = temporary.state
	ledger.sequence = temporary.sequence
	_, err = ledger.journal.Seek(offset, io.SeekStart)
	return err
}

func replayReferenceLedgerRecordWithImport(ledger *ReferenceLedger, record referenceLedgerRecord, offset int64, pending **pendingLedgerImport) error {
	if *pending == nil {
		if record.ImportBegin == nil {
			if record.ImportObjects != nil || record.ImportReferences != nil || record.ImportCommit != nil {
				return errors.New("chunked import record has no matching begin")
			}
			return replayReferenceLedgerRecord(ledger, record)
		}
		if referenceLedgerMutationCount(record) != 1 || record.ImportBegin.Origin == 0 {
			return errors.New("invalid chunked import begin")
		}
		*pending = &pendingLedgerImport{
			begin:         *record.ImportBegin,
			objects:       nil,
			references:    make(map[string]ObjectID),
			journalOffset: offset,
			priorSequence: record.Sequence - 1,
		}
		return nil
	}
	p := *pending
	if referenceLedgerMutationCount(record) != 1 {
		return errors.New("chunked import contains an interleaved mutation")
	}
	switch {
	case record.ImportObjects != nil:
		chunk := record.ImportObjects
		if chunk.Origin != p.begin.Origin || chunk.Offset != uint64(len(p.objects)) || len(chunk.Objects) == 0 || uint64(len(p.objects))+uint64(len(chunk.Objects)) > p.begin.Objects {
			return errors.New("chunked import object range is not contiguous")
		}
		p.objects = append(p.objects, chunk.Objects...)
		return nil
	case record.ImportReferences != nil:
		chunk := record.ImportReferences
		if chunk.Origin != p.begin.Origin || chunk.Offset != uint64(len(p.references)) || len(chunk.References) == 0 || uint64(len(p.references))+uint64(len(chunk.References)) > p.begin.References {
			return errors.New("chunked import reference range is not contiguous")
		}
		for _, reference := range chunk.References {
			if _, exists := p.references[reference.Name]; exists {
				return fmt.Errorf("chunked import repeats reference %q", reference.Name)
			}
			p.references[reference.Name] = reference.Object
		}
		return nil
	case record.ImportCommit != nil:
		if record.ImportCommit.Origin != p.begin.Origin || uint64(len(p.objects)) != p.begin.Objects || uint64(len(p.references)) != p.begin.References {
			return errors.New("chunked import commit is incomplete")
		}
		if err := ledger.importLocked(p.begin.Origin, p.objects, p.references); err != nil {
			return err
		}
		*pending = nil
		return nil
	default:
		return errors.New("chunked import contains an interleaved mutation")
	}
}

func referenceLedgerMutationCount(record referenceLedgerRecord) int {
	count := 0
	for _, set := range []bool{
		record.Import != nil, record.ImportBegin != nil, record.ImportObjects != nil,
		record.ImportReferences != nil, record.ImportCommit != nil, record.Reference != nil,
		len(record.References) != 0, record.Handoff != nil, record.Ack != nil, record.Collect != nil, record.Retire != nil,
	} {
		if set {
			count++
		}
	}
	return count
}

func replayReferenceLedgerRecord(ledger *ReferenceLedger, record referenceLedgerRecord) error {
	set := referenceLedgerMutationCount(record)
	if record.Import != nil {
		if err := ledger.ImportOrigin(record.Import.Origin, record.Import.Objects, record.Import.References); err != nil {
			return err
		}
	}
	if record.Reference != nil {
		r := record.Reference
		if err := ledger.ApplyReference(r.Operation, r.Name, r.Object, r.Retain); err != nil {
			return err
		}
	}
	if len(record.References) != 0 {
		mutations := make([]SnapshotReferenceMutation, len(record.References))
		for i, reference := range record.References {
			mutations[i] = SnapshotReferenceMutation{
				Operation: reference.Operation, Name: reference.Name,
				Object: reference.Object, Retain: reference.Retain,
			}
		}
		if err := ledger.ApplyReferences(mutations); err != nil {
			return err
		}
	}
	if record.Handoff != nil {
		h := record.Handoff
		if err := ledger.ApplyHandoff(h.Origin, h.From, h.Mutations); err != nil {
			return err
		}
	}
	if record.Ack != nil {
		if err := ledger.AcknowledgeOutboundThrough(*record.Ack); err != nil {
			return err
		}
	}
	if record.Collect != nil {
		collected, err := ledger.Collect(*record.Collect)
		if err != nil {
			return err
		}
		if collected != record.Collected {
			return fmt.Errorf("collection replay removed %d objects, want %d", collected, record.Collected)
		}
	}
	if record.Retire != nil {
		if err := ledger.RetireOrigin(*record.Retire); err != nil {
			return err
		}
	}
	if set != 1 {
		return errors.New("reference ledger record must contain exactly one mutation")
	}
	return nil
}

// mutateLocked applies change to the ledger's state in place and journals the
// record it returns, a nil one if it changed nothing. A change that fails, or
// whose record is not written, is undone.
func (ledger *ReferenceLedger) mutateLocked(change func(*referenceLedgerState) (*referenceLedgerRecord, error)) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	ledger.state.begin()
	record, err := change(&ledger.state)
	if err == nil && record != nil {
		err = ledger.journalLocked(*record)
	}
	if err != nil {
		ledger.state.rollback()
		return err
	}
	ledger.state.commit()
	return nil
}

// journalLocked durably appends record to the journal, if the ledger has one.
func (ledger *ReferenceLedger) journalLocked(record referenceLedgerRecord) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	if ledger.journal == nil {
		return nil
	}
	if ledger.sequence == math.MaxUint64 {
		return errors.New("reference ledger sequence exhausted")
	}
	record.Sequence = ledger.sequence + 1
	encoded, err := encodeReferenceLedgerRecord(record)
	if err != nil {
		return err
	}
	return ledger.appendLocked(encoded, record.Sequence)
}

// appendLocked durably appends encoded, the record of sequence, to the
// journal.
func (ledger *ReferenceLedger) appendLocked(encoded []byte, sequence uint64) error {
	err := writeFull(ledger.journal, encoded)
	if err == nil {
		err = ledger.journal.Sync()
	}
	if err != nil {
		ledger.poisoned = err
		return err
	}
	ledger.sequence = sequence
	return nil
}

func encodeReferenceLedgerRecord(record referenceLedgerRecord) ([]byte, error) {
	record.Version = referenceLedgerVersion
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encoding reference ledger record: %w", err)
	}
	if len(data) == 0 || len(data) > maxReferenceLedgerRecord {
		return nil, fmt.Errorf("reference ledger record has invalid length %d", len(data))
	}
	encoded := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(encoded[:8], uint64(len(data)))
	copy(encoded[8:], data)
	return encoded, nil
}

// ledgerImportObjectsBatch is the most objects a record of an import holds.
const ledgerImportObjectsBatch = 256

// encodeLedgerImportObjects encodes, as encodeReferenceLedgerRecord does, byte
// for byte, the record of sequence that holds the objects of origin's import
// from offset on: as many of objects as a record fits, one at least, and at
// most ledgerImportObjectsBatch. It returns the record and how many objects
// it holds.
//
// Through json.Marshal, a batch of memory tables too large for a record was
// encoded whole before it was halved, and json kept its encoder, grown to
// 32 MB, in a pool, where the daemon's every later json.Marshal kept it alive.
// Here each object is encoded once, into a buffer of the record's own.
func encodeLedgerImportObjects(sequence uint64, origin SnapshotID, offset uint64, objects []Object) ([]byte, int, error) {
	const tail = `]},"ImportReferences":null,"ImportCommit":null,"Reference":null,"References":null,"Handoff":null,"Ack":null,"Collect":null,"Collected":0,"Retire":null}`
	out := make([]byte, 8, 64<<10)
	out = append(out, `{"Version":`...)
	out = strconv.AppendInt(out, referenceLedgerVersion, 10)
	out = append(out, `,"Sequence":`...)
	out = strconv.AppendUint(out, sequence, 10)
	out = append(out, `,"Import":null,"ImportBegin":null,"ImportObjects":{"Origin":`...)
	out = strconv.AppendUint(out, uint64(origin), 10)
	out = append(out, `,"Offset":`...)
	out = strconv.AppendUint(out, offset, 10)
	out = append(out, `,"Objects":[`...)
	count := 0
	for _, object := range objects[:min(len(objects), ledgerImportObjectsBatch)] {
		mark := len(out)
		if count != 0 {
			out = append(out, ',')
		}
		out = appendObjectJSON(out, object)
		if length := len(out) - 8 + len(tail); length > maxReferenceLedgerRecord {
			if count == 0 {
				return nil, 0, fmt.Errorf("reference ledger record has invalid length %d", length)
			}
			out = out[:mark]
			break
		}
		count++
	}
	if count == 0 {
		return nil, 0, errors.New("reference ledger import record has no objects")
	}
	out = append(out, tail...)
	binary.BigEndian.PutUint64(out[:8], uint64(len(out)-8))
	return out, count, nil
}

// journalChunkedImportLocked makes an arbitrarily large origin import atomic
// without weakening the bounded-WAL invariant. The commit marker is synced
// separately from its payload. Replay rolls an incomplete prefix back to the
// begin offset, so the new graph is never partially visible.
func (ledger *ReferenceLedger) journalChunkedImportLocked(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	named := make([]ledgerNamedReference, 0, len(names))
	for _, name := range names {
		named = append(named, ledgerNamedReference{Name: name, Object: references[name]})
	}
	// Each record is encoded once, with the sequence it takes: they are
	// journaled in the order they are made.
	var encoded [][]byte
	encode := func(record referenceLedgerRecord) ([]byte, error) {
		if uint64(len(encoded)) >= math.MaxUint64-ledger.sequence {
			return nil, errors.New("reference ledger sequence exhausted")
		}
		record.Sequence = ledger.sequence + uint64(len(encoded)) + 1
		return encodeReferenceLedgerRecord(record)
	}
	begin, err := encode(referenceLedgerRecord{ImportBegin: &ledgerImportBeginRecord{
		Origin: origin, Objects: uint64(len(objects)), References: uint64(len(named)),
	}})
	if err != nil {
		return err
	}
	encoded = append(encoded, begin)
	appendBatches := func(total int, makeRecord func(offset, end int) referenceLedgerRecord) error {
		for offset := 0; offset < total; {
			end := offset + 256
			if end > total {
				end = total
			}
			for {
				data, err := encode(makeRecord(offset, end))
				if err == nil {
					encoded = append(encoded, data)
					offset = end
					break
				} else if end-offset == 1 {
					return err
				}
				end = offset + (end-offset)/2
			}
		}
		return nil
	}
	for offset := 0; offset < len(objects); {
		if uint64(len(encoded)) >= math.MaxUint64-ledger.sequence {
			return errors.New("reference ledger sequence exhausted")
		}
		data, count, err := encodeLedgerImportObjects(ledger.sequence+uint64(len(encoded))+1, origin, uint64(offset), objects[offset:])
		if err != nil {
			return err
		}
		encoded = append(encoded, data)
		offset += count
	}
	if err := appendBatches(len(named), func(offset, end int) referenceLedgerRecord {
		return referenceLedgerRecord{ImportReferences: &ledgerImportReferencesRecord{
			Origin: origin, Offset: uint64(offset), References: named[offset:end],
		}}
	}); err != nil {
		return err
	}
	commitSequence := ledger.sequence + uint64(len(encoded)) + 1
	commitData, err := encode(referenceLedgerRecord{ImportCommit: &ledgerImportCommitRecord{Origin: origin}})
	if err != nil {
		return err
	}
	for _, data := range encoded {
		if err := writeFull(ledger.journal, data); err != nil {
			ledger.poisoned = err
			return err
		}
	}
	if err := ledger.journal.Sync(); err != nil {
		ledger.poisoned = err
		return err
	}
	if err := writeFull(ledger.journal, commitData); err != nil {
		ledger.poisoned = err
		return err
	}
	if err := ledger.journal.Sync(); err != nil {
		ledger.poisoned = err
		return err
	}
	ledger.sequence = commitSequence
	return nil
}

func validateReferenceLedgerState(state referenceLedgerState) error {
	if state.Origins == nil || state.Objects == nil || state.References == nil || state.Applied == nil || state.HandoffCursor == nil || state.NextSequence == 0 {
		return errors.New("reference ledger checkpoint has nil or invalid state")
	}
	for origin := range state.Origins {
		if origin == 0 {
			return errors.New("reference ledger owns zero origin")
		}
		if _, ok := state.HandoffCursor[origin]; !ok {
			return fmt.Errorf("owned origin %d has no handoff cursor", origin)
		}
	}
	for key, object := range state.Objects {
		if key != object.ID.key() || !state.owns(object.ID) {
			return fmt.Errorf("reference ledger object %q has invalid owner", key)
		}
		if err := validateObject(object); err != nil {
			return err
		}
	}
	expected := make(map[string]uint64, len(state.Objects))
	for name, id := range state.References {
		if err := validateReferenceName(name); err != nil {
			return err
		}
		if _, ok := state.Objects[id.key()]; !ok || !state.owns(id) {
			return fmt.Errorf("reference %q selects unavailable object %s", name, id)
		}
		expected[id.key()]++
	}
	for _, parent := range state.Objects {
		for _, edge := range parent.Children {
			if state.owns(edge.Child) {
				if _, ok := state.Objects[edge.Child.key()]; !ok {
					return fmt.Errorf("object %s references missing owned child %s", parent.ID, edge.Child)
				}
				expected[edge.Child.key()]++
			}
		}
	}
	for key, object := range state.Objects {
		if object.Refs != expected[key] || (object.Refs == 0) != (object.State == ObjectReclaiming) {
			return fmt.Errorf("object %s reference state is inconsistent", object.ID)
		}
	}
	previous := state.Acknowledged
	for _, outbound := range state.Outbox {
		if outbound.Sequence <= previous || outbound.Sequence >= state.NextSequence {
			return fmt.Errorf("invalid outbound reference sequence %d", outbound.Sequence)
		}
		previous = outbound.Sequence
	}
	for operation, applied := range state.Applied {
		if operation == "" || validateReferenceName(applied.Name) != nil || applied.Object.IsZero() {
			return fmt.Errorf("invalid applied reference operation %q", operation)
		}
	}
	for origin := range state.HandoffCursor {
		if _, ok := state.Origins[origin]; !ok {
			return fmt.Errorf("handoff cursor names unowned origin %d", origin)
		}
	}
	return nil
}

// Checkpoint bounds recovery work by atomically publishing a complete ledger
// state and replacing the semantic WAL with an empty one. If a crash lands
// between the two renames, replay skips WAL records already included in the
// checkpoint, so either durable prefix reconstructs the same state.
func (ledger *ReferenceLedger) Checkpoint() error {
	if ledger == nil {
		return errors.New("reference ledger is nil")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.journal == nil {
		return errors.New("in-memory reference ledger cannot be checkpointed")
	}
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	checkpoint := referenceLedgerCheckpoint{Version: referenceLedgerVersion, Sequence: ledger.sequence, State: ledger.state}
	data, err := encodeReferenceLedgerCheckpoint(&checkpoint)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeReferenceLedgerAtomic(ledger.dir, "ledger.checkpoint", ".ledger-checkpoint-", data); err != nil {
		return err
	}
	ledger.checkpointBytes = int64(len(data))
	if err := writeReferenceLedgerAtomic(ledger.dir, "ledger.log", ".ledger-log-", []byte(referenceLedgerHeader)); err != nil {
		return err
	}
	journal, err := os.OpenFile(filepath.Join(ledger.dir, "ledger.log"), os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		ledger.poisoned = err
		return err
	}
	old := ledger.journal
	ledger.journal = journal
	return old.Close()
}

// encodeReferenceLedgerCheckpoint encodes checkpoint as json.Marshal does,
// byte for byte, its objects, nearly all of it, without reflection, as
// encodeGraphCheckpoint does: encoding/json took 1.6 s for the ledger of a
// cluster holding a branch of 100,000 files, which the import of its
// snapshot checkpoints.
func encodeReferenceLedgerCheckpoint(checkpoint *referenceLedgerCheckpoint) ([]byte, error) {
	// The fields around the objects are small: encoding/json encodes them.
	state := &checkpoint.State
	head, err := json.Marshal(struct {
		Version  int    `json:"version"`
		Sequence uint64 `json:"sequence"`
		State    struct {
			Origins map[SnapshotID]struct{}
		} `json:"state"`
	}{Version: checkpoint.Version, Sequence: checkpoint.Sequence, State: struct {
		Origins map[SnapshotID]struct{}
	}{state.Origins}})
	if err != nil {
		return nil, err
	}
	tail, err := json.Marshal(struct {
		References    map[string]ObjectID
		Applied       map[string]appliedReference
		Outbox        []OutboundReference
		NextSequence  uint64
		Acknowledged  uint64
		HandoffCursor map[SnapshotID]uint64
	}{state.References, state.Applied, state.Outbox, state.NextSequence, state.Acknowledged, state.HandoffCursor})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(head)+len(tail)+objectTableJSONSize(state.Objects))
	// head ends in the two braces that close the state and the checkpoint.
	out = append(out, head[:len(head)-2]...)
	out = append(out, `,"Objects":`...)
	out = appendObjectTableJSON(out, state.Objects)
	out = append(out, ',')
	out = append(out, tail[1:]...)
	return append(out, '}'), nil
}

func writeReferenceLedgerAtomic(dir, destination, prefix string, data []byte) error {
	temporary, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	path := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if err := writeFull(temporary, data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(dir, destination)); err != nil {
		return err
	}
	keep = true
	return syncDirectory(dir)
}

// JournalBytes returns the current semantic WAL size.
func (ledger *ReferenceLedger) JournalBytes() int64 {
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	if ledger.journal == nil {
		return 0
	}
	info, err := ledger.journal.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

// CheckpointDue reports whether the journal has grown to at least minimum
// bytes and to the size of the last checkpoint: a checkpoint costs in
// proportion to the whole ledger, and replaying the journal then costs at
// most as much as reading the checkpoint.
func (ledger *ReferenceLedger) CheckpointDue(minimum int64) bool {
	journal := ledger.JournalBytes()
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	return journal >= max(minimum, ledger.checkpointBytes)
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
	return ledger.importLocked(origin, objects, references)
}

// importLocked imports origin into the ledger's state and journals the
// import, if the ledger has a journal, in one record or, for one too large
// for a record, in a chunked sequence. A refused or unjournaled import is
// undone.
func (ledger *ReferenceLedger) importLocked(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	ledger.state.begin()
	err := ledger.state.importOrigin(origin, objects, references)
	if err == nil && ledger.journal != nil {
		err = ledger.journalImportLocked(origin, objects, references)
	}
	if err != nil {
		ledger.state.rollback()
		return err
	}
	ledger.state.commit()
	return nil
}

// minimalObjectJSON is the length of the shortest object encoding/json
// writes.
var minimalObjectJSON = func() int {
	data, err := json.Marshal(Object{})
	if err != nil {
		panic(err)
	}
	return len(data)
}()

// journalImportLocked journals an import in one record or, if it is too
// large for one, in a chunked sequence. An import of more objects than a
// record holds at their shortest is not encoded whole first, only to be found
// too large: that took 0.5 s for 300,000 objects.
func (ledger *ReferenceLedger) journalImportLocked(origin SnapshotID, objects []Object, references map[string]ObjectID) error {
	if ledger.sequence == math.MaxUint64 {
		return errors.New("reference ledger sequence exhausted")
	}
	if len(objects) <= maxReferenceLedgerRecord/minimalObjectJSON {
		record := referenceLedgerRecord{Import: &ledgerImportRecord{Origin: origin, Objects: objects, References: references}}
		record.Sequence = ledger.sequence + 1
		if encoded, err := encodeReferenceLedgerRecord(record); err == nil {
			return ledger.appendLocked(encoded, record.Sequence)
		}
	}
	return ledger.journalChunkedImportLocked(origin, objects, references)
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
		object := cloneObject(input)
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
// cross-owner edge mutations in one durable transaction. This is semantically
// equivalent to ApplyReference for every element, but clones and fsyncs the
// ledger only once. A failure rejects the entire batch.
func (ledger *ReferenceLedger) ApplyReferences(mutations []SnapshotReferenceMutation) error {
	if ledger == nil {
		return errors.New("invalid reference-ledger mutation batch")
	}
	if len(mutations) == 0 {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.mutateLocked(func(state *referenceLedgerState) (*referenceLedgerRecord, error) {
		records := make([]ledgerReferenceRecord, 0, len(mutations))
		for _, mutation := range mutations {
			if mutation.Operation == "" {
				return nil, errors.New("invalid reference-ledger operation")
			}
			want := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
			if previous, ok := state.Applied[mutation.Operation]; ok {
				if previous != want {
					return nil, fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
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
				return nil, err
			}
			state.putApplied(mutation.Operation, want)
			records = append(records, ledgerReferenceRecord{
				Operation: mutation.Operation, Name: mutation.Name,
				Object: mutation.Object, Retain: mutation.Retain,
			})
		}
		switch len(records) {
		case 0:
			return nil, nil
		case 1:
			return &referenceLedgerRecord{Reference: &records[0]}, nil
		default:
			return &referenceLedgerRecord{References: records}, nil
		}
	})
}

// ApplyHandoff atomically advances one provider-to-owner catch-up cursor.
// Mutations must be contiguous and are individually idempotent by operation
// identity. Retrying an already committed prefix succeeds without another WAL
// write; skipping or reordering any mutation is rejected.
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
		// only when that batch ends no later than the durable cursor.
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
	return ledger.mutateLocked(func(state *referenceLedgerState) (*referenceLedgerRecord, error) {
		want := from + 1
		for _, mutation := range mutations {
			if mutation.Sequence != want || mutation.Object.Snapshot != origin {
				return nil, fmt.Errorf("snapshot origin %d handoff mutation %d, want %d", origin, mutation.Sequence, want)
			}
			applied := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
			if previous, ok := state.Applied[mutation.Operation]; ok {
				if previous != applied {
					return nil, fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
				}
			} else {
				var err error
				if mutation.Retain {
					err = state.retain(mutation.Name, mutation.Object)
				} else {
					err = state.release(mutation.Name, mutation.Object)
				}
				if err != nil {
					return nil, err
				}
				state.putApplied(mutation.Operation, applied)
			}
			want++
		}
		state.putCursor(origin, want-1)
		record := ledgerHandoffRecord{Origin: origin, From: from, Mutations: append([]SnapshotReferenceMutation(nil), mutations...)}
		return &referenceLedgerRecord{Handoff: &record}, nil
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
	return ledger.mutateLocked(func(state *referenceLedgerState) (*referenceLedgerRecord, error) {
		state.Outbox = append([]OutboundReference(nil), state.Outbox[index+1:]...)
		state.Acknowledged = sequence
		return &referenceLedgerRecord{Ack: &sequence}, nil
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
	// A healthy live graph overwhelmingly has nothing to collect: do not
	// append a no-op WAL record on every coordinator maintenance tick.
	if len(queue) == 0 {
		return 0, nil
	}
	collected := 0
	if err := ledger.mutateLocked(func(state *referenceLedgerState) (*referenceLedgerRecord, error) {
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
						return nil, fmt.Errorf("reclaiming %s cannot release child %s", parent.ID, edge.Child)
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
					return nil, err
				}
			}
			state.deleteObject(key)
			collected++
		}
		return &referenceLedgerRecord{Collect: &limit, Collected: collected}, nil
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
// this durable ledger mutation succeeds.
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
	return ledger.mutateLocked(func(state *referenceLedgerState) (*referenceLedgerRecord, error) {
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
		retired := origin
		return &referenceLedgerRecord{Retire: &retired}, nil
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
