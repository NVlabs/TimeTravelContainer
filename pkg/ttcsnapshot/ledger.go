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
	ledger.state = cloneReferenceLedgerState(temporary.state)
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
		next, err := importOriginState(ledger.state, p.begin.Origin, p.objects, p.references)
		if err != nil {
			return err
		}
		ledger.state = next
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
	record.Sequence = ledger.sequence + 1
	encoded, err := encodeReferenceLedgerRecord(record)
	if err != nil {
		return err
	}
	err = writeFull(ledger.journal, encoded)
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

// commitChunkedImportLocked makes an arbitrarily large origin import atomic
// without weakening the bounded-WAL invariant. The commit marker is synced
// separately from its payload. Replay rolls an incomplete prefix back to the
// begin offset, so the new graph is never partially visible.
func (ledger *ReferenceLedger) commitChunkedImportLocked(origin SnapshotID, objects []Object, references map[string]ObjectID, next referenceLedgerState) error {
	if ledger.poisoned != nil {
		return fmt.Errorf("reference ledger requires reopen: %w", ledger.poisoned)
	}
	if ledger.journal == nil {
		ledger.state = next
		return nil
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
	records := []referenceLedgerRecord{{ImportBegin: &ledgerImportBeginRecord{
		Origin: origin, Objects: uint64(len(objects)), References: uint64(len(named)),
	}}}
	appendBatches := func(total int, makeRecord func(offset, end int) referenceLedgerRecord) error {
		for offset := 0; offset < total; {
			end := offset + 256
			if end > total {
				end = total
			}
			for {
				record := makeRecord(offset, end)
				// MaxUint64 has the widest JSON representation of Sequence, so
				// a record that fits this preview also fits its real sequence.
				record.Sequence = math.MaxUint64
				if _, err := encodeReferenceLedgerRecord(record); err == nil {
					records = append(records, record)
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
	if err := appendBatches(len(objects), func(offset, end int) referenceLedgerRecord {
		return referenceLedgerRecord{ImportObjects: &ledgerImportObjectsRecord{
			Origin: origin, Offset: uint64(offset), Objects: objects[offset:end],
		}}
	}); err != nil {
		return err
	}
	if err := appendBatches(len(named), func(offset, end int) referenceLedgerRecord {
		return referenceLedgerRecord{ImportReferences: &ledgerImportReferencesRecord{
			Origin: origin, Offset: uint64(offset), References: named[offset:end],
		}}
	}); err != nil {
		return err
	}
	commit := referenceLedgerRecord{ImportCommit: &ledgerImportCommitRecord{Origin: origin}}
	if uint64(len(records))+1 > math.MaxUint64-ledger.sequence {
		return errors.New("reference ledger sequence exhausted")
	}
	encoded := make([][]byte, 0, len(records)+1)
	for index := range records {
		records[index].Sequence = ledger.sequence + uint64(index) + 1
		data, err := encodeReferenceLedgerRecord(records[index])
		if err != nil {
			return err
		}
		encoded = append(encoded, data)
	}
	commit.Sequence = ledger.sequence + uint64(len(records)) + 1
	commitData, err := encodeReferenceLedgerRecord(commit)
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
	ledger.sequence = commit.Sequence
	ledger.state = next
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
	checkpoint := referenceLedgerCheckpoint{Version: referenceLedgerVersion, Sequence: ledger.sequence, State: cloneReferenceLedgerState(ledger.state)}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeReferenceLedgerAtomic(ledger.dir, "ledger.checkpoint", ".ledger-checkpoint-", data); err != nil {
		return err
	}
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
		return nil
	}
	next, err := importOriginState(ledger.state, origin, objects, references)
	if err != nil {
		return err
	}
	record := referenceLedgerRecord{Import: &ledgerImportRecord{Origin: origin, Objects: objects, References: references}}
	if ledger.journal == nil {
		return ledger.commitLocked(record, next)
	}
	record.Sequence = ledger.sequence + 1
	if _, err := encodeReferenceLedgerRecord(record); err == nil {
		return ledger.commitLocked(record, next)
	}
	return ledger.commitChunkedImportLocked(origin, objects, references, next)
}

func importOriginState(state referenceLedgerState, origin SnapshotID, objects []Object, references map[string]ObjectID) (referenceLedgerState, error) {
	if origin == 0 {
		return referenceLedgerState{}, errors.New("invalid reference-ledger origin import")
	}
	if _, exists := state.Origins[origin]; exists {
		return cloneReferenceLedgerState(state), nil
	}
	next := cloneReferenceLedgerState(state)
	next.Origins[origin] = struct{}{}
	next.HandoffCursor[origin] = 0
	for _, input := range objects {
		object := cloneObject(input)
		if object.ID.Snapshot != origin {
			return referenceLedgerState{}, fmt.Errorf("origin %d import contains foreign object %s", origin, object.ID)
		}
		object.State, object.Refs = ObjectLive, 0
		if err := validateObject(object); err != nil {
			return referenceLedgerState{}, err
		}
		if _, exists := next.Objects[object.ID.key()]; exists {
			return referenceLedgerState{}, fmt.Errorf("origin import repeats object %s", object.ID)
		}
		next.Objects[object.ID.key()] = object
	}
	for _, input := range objects {
		parent := next.Objects[input.ID.key()]
		for _, edge := range parent.Children {
			if next.owns(edge.Child) {
				child, ok := next.Objects[edge.Child.key()]
				if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
					return referenceLedgerState{}, fmt.Errorf("object %s references unavailable owned child %s", parent.ID, edge.Child)
				}
				bridge := edgeReferenceName(parent.ID, edge.Slot)
				if bridged, ok := next.References[bridge]; ok {
					if bridged != edge.Child {
						return referenceLedgerState{}, fmt.Errorf("incoming edge bridge %q selects %s, want %s", bridge, bridged, edge.Child)
					}
					// Replace the external named reference with the now-internal
					// structural edge. The child's count is unchanged.
					delete(next.References, bridge)
				} else {
					child.Refs++
					next.Objects[edge.Child.key()] = child
				}
			} else if err := next.enqueue(parent.ID, edge, true); err != nil {
				return referenceLedgerState{}, err
			}
		}
	}
	internalIncoming := make(map[string]ObjectID)
	for _, parent := range next.Objects {
		if !next.owns(parent.ID) {
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == origin {
				name := edgeReferenceName(parent.ID, edge.Slot)
				internalIncoming[name] = edge.Child
				if parent.ID.Snapshot != origin {
					child, ok := next.Objects[edge.Child.key()]
					if !ok || child.Refs == math.MaxUint64 {
						return referenceLedgerState{}, fmt.Errorf("owned parent %s references unavailable imported child %s", parent.ID, edge.Child)
					}
					child.State = ObjectLive
					child.Refs++
					next.Objects[edge.Child.key()] = child
				}
			}
		}
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
		if internal, ok := internalIncoming[effectiveName]; ok {
			if references[name] != internal {
				return referenceLedgerState{}, fmt.Errorf("incoming edge baseline %q selects %s, want %s", effectiveName, references[name], internal)
			}
			// The already-owned parent edge supplies this count directly.
			continue
		}
		if err := next.retain(effectiveName, references[name]); err != nil {
			return referenceLedgerState{}, fmt.Errorf("importing reference %q: %w", effectiveName, err)
		}
	}
	for key, object := range next.Objects {
		if object.ID.Snapshot == origin && object.Refs == 0 {
			object.State = ObjectReclaiming
			next.Objects[key] = object
		}
	}
	return next, nil
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
	next := cloneReferenceLedgerState(ledger.state)
	records := make([]ledgerReferenceRecord, 0, len(mutations))
	for _, mutation := range mutations {
		if mutation.Operation == "" {
			return errors.New("invalid reference-ledger operation")
		}
		want := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
		if previous, ok := next.Applied[mutation.Operation]; ok {
			if previous != want {
				return fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
			}
			continue
		}
		var err error
		if mutation.Retain {
			err = next.retain(mutation.Name, mutation.Object)
		} else {
			err = next.release(mutation.Name, mutation.Object)
		}
		if err != nil {
			return err
		}
		next.Applied[mutation.Operation] = want
		records = append(records, ledgerReferenceRecord{
			Operation: mutation.Operation, Name: mutation.Name,
			Object: mutation.Object, Retain: mutation.Retain,
		})
	}
	if len(records) == 0 {
		return nil
	}
	if len(records) == 1 {
		return ledger.commitLocked(referenceLedgerRecord{Reference: &records[0]}, next)
	}
	return ledger.commitLocked(referenceLedgerRecord{References: records}, next)
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
	next := cloneReferenceLedgerState(ledger.state)
	want := from + 1
	for _, mutation := range mutations {
		if mutation.Sequence != want || mutation.Object.Snapshot != origin {
			return fmt.Errorf("snapshot origin %d handoff mutation %d, want %d", origin, mutation.Sequence, want)
		}
		applied := appliedReference{Name: mutation.Name, Object: mutation.Object, Retain: mutation.Retain}
		if previous, ok := next.Applied[mutation.Operation]; ok {
			if previous != applied {
				return fmt.Errorf("reference operation %q was reused with different input", mutation.Operation)
			}
		} else {
			var err error
			if mutation.Retain {
				err = next.retain(mutation.Name, mutation.Object)
			} else {
				err = next.release(mutation.Name, mutation.Object)
			}
			if err != nil {
				return err
			}
			next.Applied[mutation.Operation] = applied
		}
		want++
	}
	next.HandoffCursor[origin] = want - 1
	record := ledgerHandoffRecord{Origin: origin, From: from, Mutations: append([]SnapshotReferenceMutation(nil), mutations...)}
	return ledger.commitLocked(referenceLedgerRecord{Handoff: &record}, next)
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
	next := cloneReferenceLedgerState(ledger.state)
	next.Outbox = append([]OutboundReference(nil), next.Outbox[index+1:]...)
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
	// A healthy live graph overwhelmingly has nothing to collect. Avoid cloning
	// the complete object table and appending a no-op WAL record on every
	// coordinator maintenance tick. For large memory origins that work is
	// proportional to resident memory and can otherwise keep a coordinator CPU
	// saturated even though the graph has not changed.
	hasReclaiming := false
	for _, object := range ledger.state.Objects {
		if object.State == ObjectReclaiming {
			hasReclaiming = true
			break
		}
	}
	if !hasReclaiming {
		return 0, nil
	}
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

// EmptyOrigins returns origins whose immutable object graph has been fully
// collected and whose cross-owner edge queue is drained. The shared-GC
// coordinator must commit permanent retirement in Raft before calling
// RetireOrigin; this method merely reports candidates.
func (ledger *ReferenceLedger) EmptyOrigins() []SnapshotID {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	blocked := make(map[SnapshotID]struct{})
	for _, object := range ledger.state.Objects {
		blocked[object.ID.Snapshot] = struct{}{}
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

// RetireOrigin forgets an empty origin after Raft has permanently fenced its
// snapshot ID. It is idempotent so a successor coordinator can finish a crash
// that occurred after the Raft commit. Physical packs are removed only after
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
	for _, object := range ledger.state.Objects {
		if object.ID.Snapshot == origin {
			return fmt.Errorf("snapshot origin %d still contains object %s", origin, object.ID)
		}
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
	next := cloneReferenceLedgerState(ledger.state)
	delete(next.Origins, origin)
	delete(next.HandoffCursor, origin)
	// Once Raft fences the immutable ID, retries are rejected before reaching
	// this ledger. Its old operation receipts no longer serve deduplication and
	// would otherwise grow without bound over long-lived clusters.
	for operation, applied := range next.Applied {
		if applied.Object.Snapshot == origin {
			delete(next.Applied, operation)
		}
	}
	retired := origin
	return ledger.commitLocked(referenceLedgerRecord{Retire: &retired}, next)
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
	objects = make([]Object, 0)
	for _, object := range ledger.state.Objects {
		// A bounded collection may leave descendants in ObjectReclaiming until
		// its next pass. They are already logically dead and must not be offered
		// to shared-pack compaction as part of the live origin view.
		if object.ID.Snapshot != origin {
			continue
		}
		if object.State == ObjectReclaiming {
			ready = false
			continue
		}
		if object.State == ObjectLive {
			objects = append(objects, cloneObject(object))
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID.key() < objects[j].ID.key() })
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
