// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/maphash"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostthread"
)

const (
	journalHeader        = "TTCSGJ6\n"
	maxJournalRecordSize = 8 << 20
	// maxCollectObjectsPerTransaction bounds the fan-out of reference updates
	// emitted by one GC transaction. An object may have maxObjectEdges
	// children, so an arbitrarily large caller limit can exceed the journal
	// record ceiling even though every individual object is valid. Callers
	// already repeat Collect until it returns zero.
	maxCollectObjectsPerTransaction = 128
	// maxCollectBatchObjects and maxCollectReferencesPerTransaction bound one
	// Collect transaction: its object deletions and the reference updates of
	// the objects they release, maxObjectEdges more at most, keep the record
	// within about 6 MiB even with the widest IDs, below maxJournalRecordSize.
	maxCollectBatchObjects             = 4096
	maxCollectReferencesPerTransaction = 32768
	headFileName                       = "graph.head"
)

// ErrSnapshotLedgerFrozen reports a reference or object mutation that raced
// the final provider-to-owner authority handoff. Callers performing transient
// observations may wait for the short handoff barrier and retry; ordinary
// mutations must propagate the error.
var ErrSnapshotLedgerFrozen = errors.New("snapshot ledger is frozen for authority handoff")

type graphHead struct {
	Version    int    `json:"version"`
	Generation uint64 `json:"generation"`
}

type graphState struct {
	Version      int
	Sequence     uint64
	NextSnapshot SnapshotID
	Snapshots    map[SnapshotID]Snapshot
	Objects      map[ObjectID]Object
	Roots        map[string]ObjectID
	Pins         map[string]ObjectID
	Frozen       map[SnapshotID]bool
	Handoffs     map[SnapshotID]SnapshotLedgerHandoff

	// undo records what a transaction applied for a preview has changed.
	undo *graphUndo
}

// graphUndo holds the prior values of what a transaction applied in place
// has changed in a graphState: from them the transaction is undone, and the
// references it changes of a snapshot in handoff are read from the names it
// touched alone. A preview that copied the whole graph instead made each
// mutation during a handoff cost time in proportion to the graph, 51 ms in
// one of 10,000 objects.
type graphUndo struct {
	// A nil prior value is an absent one.
	snapshots    map[SnapshotID]*Snapshot
	objects      map[ObjectID]*Object
	roots        map[string]*ObjectID
	pins         map[string]*ObjectID
	frozen       map[SnapshotID]*bool
	handoffs     map[SnapshotID]*SnapshotLedgerHandoff
	nextSnapshot SnapshotID
	sequence     uint64
}

// beginUndo starts recording what the transaction to be applied changes.
func (state *graphState) beginUndo() {
	state.undo = &graphUndo{nextSnapshot: state.NextSnapshot, sequence: state.Sequence}
}

// rollbackUndo restores what the recorded transaction changed.
func (state *graphState) rollbackUndo() {
	undo := state.undo
	state.undo = nil
	restoreEntries(state.Snapshots, undo.snapshots)
	restoreEntries(state.Objects, undo.objects)
	restoreEntries(state.Roots, undo.roots)
	restoreEntries(state.Pins, undo.pins)
	restoreEntries(state.Frozen, undo.frozen)
	restoreEntries(state.Handoffs, undo.handoffs)
	state.NextSnapshot, state.Sequence = undo.nextSnapshot, undo.sequence
}

func restoreEntries[K comparable, V any](entries map[K]V, priors map[K]*V) {
	for key, prior := range priors {
		if prior != nil {
			entries[key] = *prior
		} else {
			delete(entries, key)
		}
	}
}

// recordEntry records, while a transaction is recorded, the prior value of
// entries[key], unless it has recorded one already.
func recordEntry[K comparable, V any](priors *map[K]*V, entries map[K]V, key K) {
	if _, recorded := (*priors)[key]; recorded {
		return
	}
	if *priors == nil {
		*priors = make(map[K]*V)
	}
	if prior, ok := entries[key]; ok {
		(*priors)[key] = &prior
	} else {
		(*priors)[key] = nil
	}
}

// The setters below change the state's maps, recording what they change
// while a transaction is recorded.

func (state *graphState) putSnapshot(snapshot Snapshot) {
	if state.undo != nil {
		recordEntry(&state.undo.snapshots, state.Snapshots, snapshot.ID)
	}
	state.Snapshots[snapshot.ID] = snapshot
}

func (state *graphState) deleteSnapshot(id SnapshotID) {
	if state.undo != nil {
		recordEntry(&state.undo.snapshots, state.Snapshots, id)
	}
	delete(state.Snapshots, id)
}

func (state *graphState) putObject(object Object) {
	if state.undo != nil {
		recordEntry(&state.undo.objects, state.Objects, object.ID)
	}
	state.Objects[object.ID] = object
}

func (state *graphState) deleteObject(id ObjectID) {
	if state.undo != nil {
		recordEntry(&state.undo.objects, state.Objects, id)
	}
	delete(state.Objects, id)
}

func (state *graphState) putRoot(name string, id ObjectID) {
	if state.undo != nil {
		recordEntry(&state.undo.roots, state.Roots, name)
	}
	state.Roots[name] = id
}

func (state *graphState) deleteRoot(name string) {
	if state.undo != nil {
		recordEntry(&state.undo.roots, state.Roots, name)
	}
	delete(state.Roots, name)
}

func (state *graphState) putPin(name string, id ObjectID) {
	if state.undo != nil {
		recordEntry(&state.undo.pins, state.Pins, name)
	}
	state.Pins[name] = id
}

func (state *graphState) deletePin(name string) {
	if state.undo != nil {
		recordEntry(&state.undo.pins, state.Pins, name)
	}
	delete(state.Pins, name)
}

func (state *graphState) putFrozen(id SnapshotID) {
	if state.undo != nil {
		recordEntry(&state.undo.frozen, state.Frozen, id)
	}
	state.Frozen[id] = true
}

func (state *graphState) deleteFrozen(id SnapshotID) {
	if state.undo != nil {
		recordEntry(&state.undo.frozen, state.Frozen, id)
	}
	delete(state.Frozen, id)
}

func (state *graphState) putHandoff(handoff SnapshotLedgerHandoff) {
	if state.undo != nil {
		recordEntry(&state.undo.handoffs, state.Handoffs, handoff.Snapshot)
	}
	state.Handoffs[handoff.Snapshot] = handoff
}

func (state *graphState) deleteHandoff(id SnapshotID) {
	if state.undo != nil {
		recordEntry(&state.undo.handoffs, state.Handoffs, id)
	}
	delete(state.Handoffs, id)
}

func newGraphState() graphState {
	return graphState{
		Version:      formatVersion,
		NextSnapshot: 1,
		Snapshots:    make(map[SnapshotID]Snapshot),
		Objects:      make(map[ObjectID]Object),
		Roots:        make(map[string]ObjectID),
		Pins:         make(map[string]ObjectID),
		Frozen:       make(map[SnapshotID]bool),
		Handoffs:     make(map[SnapshotID]SnapshotLedgerHandoff),
	}
}

// SnapshotReferenceMutation is one ordered reference change after a handoff
// baseline. Release precedes retain when one stable name is rebound.
type SnapshotReferenceMutation struct {
	Sequence  uint64
	Operation string
	Name      string
	Object    ObjectID
	Retain    bool
}

// SnapshotLedgerHandoff is the durable provider-side publication cursor. The
// baseline is captured before bulk publication; mutations catch the receiver
// up, and Frozen marks the short final authority-switch barrier.
type SnapshotLedgerHandoff struct {
	Snapshot     SnapshotID
	NextSequence uint64
	Baseline     map[string]ObjectID
	Mutations    []SnapshotReferenceMutation
	Frozen       bool
}

type journalOp struct {
	PutSnapshot      *Snapshot
	DeleteSnapshot   SnapshotID
	PutObject        *Object
	DeleteObject     ObjectID
	RootName         string
	PutRoot          *ObjectID
	DeleteRoot       bool
	PinName          string
	PutPin           *ObjectID
	DeletePin        bool
	SetNextSnapshot  *SnapshotID
	FreezeSnapshot   SnapshotID
	UnfreezeSnapshot SnapshotID
	BeginHandoff     *SnapshotLedgerHandoff
	AppendHandoff    *SnapshotReferenceMutation
	ApplyFilesystem  *filesystemJournalMutation
}

// object returns the object op puts or deletes, if any.
func (op *journalOp) object() (ObjectID, bool) {
	if op.PutObject != nil {
		return op.PutObject.ID, true
	}
	if !op.DeleteObject.IsZero() {
		return op.DeleteObject, true
	}
	return ObjectID{}, false
}

type journalRecord struct {
	Sequence   uint64
	Ops        []journalOp
	References []journalReference
}

// journalReference records the final materialized reference count for an
// immutable object changed by a transaction. Graph mutations keep these hot
// counters out of PutObject operations so path-copying does not rewrite every
// unchanged child. Recording the bounded changed set here lets another Store
// replay an appended journal suffix without rescanning the entire graph.
type journalReference struct {
	Object ObjectID
	Refs   uint64
	State  ObjectState
}

// Store owns one authoritative logical snapshot graph.
type Store struct {
	// unsynced is set while a store being made is not yet durable: until
	// Synced, it syncs none of what it writes. See
	// CreateExternallyCoordinated.
	unsynced atomic.Bool

	mu         sync.RWMutex
	dir        string
	dirFD      *os.File
	lock       *os.File
	journal    *os.File
	generation uint64
	state      graphState
	poisoned   error
	journalEnd int64
	// journalWeight is what replaying the journal costs, as loading so many
	// bytes of checkpoint costs: its bytes, and more for what its records
	// cost to replay beyond them (see journalRecordWeight).
	journalWeight int64
	// checkpointBytes is the size of the generation's checkpoint, and
	// checkpointObjects the number of objects it holds.
	checkpointBytes   int64
	checkpointObjects int
	headDev           uint64
	headIno           uint64
	headSize          int64
	headMtime         int64
	// externallyCoordinated is used by graph stores embedded in a TTC
	// filesystem repository. In that mode the repository's cross-process
	// flock serializes writers, so this Store must not retain graph.lock for
	// its whole lifetime. Callers must invoke Reload while holding that flock
	// before every mutation that may race another process.
	externallyCoordinated bool
	// holding is set while the store holds the graph state it took from
	// graphStates for heldDirectory, which GraphHeld reports.
	holding       bool
	heldDirectory graphFileIdentity
	// externalTransaction is true while the enclosing repository holds this
	// graph's cross-process writer fence. Repository transactions retain the
	// fence from reload through their final graph mutation; otherwise a lazy
	// deferred expansion from another Sentry can allocate from the same stale
	// snapshot ordinal or interleave journal framing bytes.
	externalTransaction bool
	// implicitExternalTransaction is true while a nominal read operation is
	// committing a lazy deferred-object expansion under graph.lock. Keep this
	// distinct from externalTransaction: the latter is an explicit repository
	// transaction that may span multiple Store calls.
	implicitExternalTransaction bool
	deferredResolvers           map[string]deferredResolver
	deferredObjects             map[deferredKey]ObjectID
	// memoryPages is a derived index of where the graph holds each page, by
	// the stable logical/version identity the guest's memory manager gave it,
	// so that a capture maps pages the graph holds rather than storing them
	// again. It counts, for each place, the live tables and pages that map the
	// page there. memoryTables finds live tables by their content, so that a
	// capture mapping the same pages as one shares it. They are rebuilt from
	// authoritative graph metadata on open and are never persisted.
	memoryPages     *memoryPageIndex
	memoryTables    map[uint64][]ObjectID
	memoryTableSeed maphash.Seed
	// memoryPageScratch is where the pages of a table are decoded to index
	// them.
	memoryPageScratch []MemoryTableEntry
	// deferredLoads serializes provider I/O for one foreign immutable object.
	// Provider reads must never run while mu is held: a multi-hop filesystem
	// graph may legitimately re-enter this Store while resolving an ancestor.
	deferredLoads map[deferredKey]*deferredLoad
	// collectQueue holds the zero-count objects Collect has found but not
	// reached yet, in the order it will reach them, and collectQueued every
	// object it has queued since it last scanned the graph. Callers repeat
	// Collect until it returns zero, and the queue lets them scan the graph
	// once rather than on every call. An entry may be stale: Collect skips an
	// object that is gone, and scans again before it reports nothing left.
	collectQueue  []ObjectID
	collectQueued map[ObjectID]struct{}
	// collectedBulk holds, once RecordCollectedBulk asked for it, the objects
	// with standalone bulk data collection has deleted since
	// TakeCollectedBulk last returned them.
	recordCollected bool
	collectedBulk   []ObjectID
}

type deferredResolver struct {
	origin SnapshotID
	load   ImmutableObjectLoader
}

type deferredLoad struct {
	done   chan struct{}
	source Object
	err    error
}

// deferredKey names an object of a foreign catalog.
type deferredKey struct {
	catalog string
	id      ObjectID
}

func deferredObjectKey(catalog string, id ObjectID) deferredKey {
	return deferredKey{catalog: catalog, id: id}
}

var graphTemporarySequence atomic.Uint64

// Open opens or creates a snapshot graph store at dir.
func Open(dir string) (*Store, error) {
	return open(dir, false, false)
}

// OpenExternallyCoordinated opens a graph whose writers are serialized by an
// enclosing cross-process transaction lock. Unlike Open, it does not retain
// graph.lock. This is intentionally a narrow integration API for repository
// implementations; ordinary graph owners should use Open.
func OpenExternallyCoordinated(dir string) (*Store, error) {
	return open(dir, true, false)
}

// CreateExternallyCoordinated makes a graph in dir, a directory just made,
// and opens it as OpenExternallyCoordinated does, but syncs nothing it
// writes until Synced. Its caller makes it durable before anything names it,
// with the rest of what it is part of: syncs that come together, the
// filesystem commits together, and one after another, each on its own.
func CreateExternallyCoordinated(dir string) (*Store, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("creating TTC snapshot graph: %q is not empty", dir)
	}
	return open(dir, true, true)
}

// Synced has the store sync what it writes from now on: its caller has made
// it durable.
func (s *Store) Synced() {
	s.unsynced.Store(false)
}

func decodeHead(data []byte) (graphHead, error) {
	var head graphHead
	if err := json.Unmarshal(data, &head); err != nil {
		return graphHead{}, fmt.Errorf("decoding TTC snapshot graph head: %w", err)
	}
	if head.Version != formatVersion && head.Version != legacyFormatVersion || head.Generation == 0 {
		return graphHead{}, fmt.Errorf("unsupported TTC snapshot graph head version %d generation %d", head.Version, head.Generation)
	}
	return head, nil
}

func validGenerationFile(name, suffix string) bool {
	if !strings.HasPrefix(name, "graph-") || !strings.HasSuffix(name, suffix) {
		return false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(name, "graph-"), suffix)
	if len(middle) != 16 {
		return false
	}
	_, err := strconv.ParseUint(middle, 16, 64)
	return err == nil
}

func preflightGraphDirectory(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	hasHead, hasLock := false, false
	for _, entry := range entries {
		switch entry.Name() {
		case headFileName:
			hasHead = true
		case "graph.lock":
			hasLock = true
		}
	}
	if !hasHead {
		for _, entry := range entries {
			if entry.Name() != "graph.lock" {
				return false, fmt.Errorf("unmarked or legacy TTC snapshot graph contains %q", entry.Name())
			}
		}
		return false, nil
	}
	headInfo, err := os.Lstat(filepath.Join(dir, headFileName))
	if err != nil {
		// Listed, and gone since: the graph was moved away with its
		// repository, as the removal of a family's state on a node does while
		// another command takes inventory of the node's repositories. It is
		// not-exist, which that inventory takes for a repository gone.
		return false, err
	}
	if !headInfo.Mode().IsRegular() {
		return false, errors.New("TTC snapshot graph head is not a regular file")
	}
	data, err := os.ReadFile(filepath.Join(dir, headFileName))
	if err != nil {
		return false, err
	}
	if _, err := decodeHead(data); err != nil {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		known := name == headFileName || name == "graph.lock" ||
			validGenerationFile(name, ".checkpoint") || validGenerationFile(name, ".journal") ||
			strings.HasPrefix(name, ".graph-head-") || strings.HasPrefix(name, ".graph-checkpoint-") || strings.HasPrefix(name, ".graph-journal-")
		if !known {
			return false, fmt.Errorf("TTC snapshot graph contains unknown entry %q", name)
		}
	}
	lockInfo, err := os.Lstat(filepath.Join(dir, "graph.lock"))
	if err != nil && hasLock {
		// Listed, and gone since: as the head above.
		return false, err
	}
	if err != nil || !lockInfo.Mode().IsRegular() {
		return false, errors.New("marked TTC snapshot graph has no regular graph.lock")
	}
	return true, nil
}

func open(dir string, externallyCoordinated, unsynced bool) (*Store, error) {
	if dir == "" {
		return nil, errors.New("TTC snapshot graph directory is empty")
	}
	marked, err := preflightGraphDirectory(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	lockFlags := os.O_RDWR
	if !marked {
		lockFlags |= os.O_CREATE
	}
	dirFD, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	lockFD, err := unix.Openat(int(dirFD.Fd()), "graph.lock", lockFlags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		dirFD.Close()
		return nil, err
	}
	lock := os.NewFile(uintptr(lockFD), "ttc-snapshot-graph-lock")
	if !externallyCoordinated {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			lock.Close()
			dirFD.Close()
			return nil, fmt.Errorf("locking TTC snapshot graph: %w", err)
		}
	}
	store := &Store{
		dir: dir, dirFD: dirFD, lock: lock, state: newGraphState(), externallyCoordinated: externallyCoordinated,
		deferredResolvers: make(map[string]deferredResolver), deferredObjects: make(map[deferredKey]ObjectID),
		deferredLoads: make(map[deferredKey]*deferredLoad), memoryTableSeed: maphash.MakeSeed(),
	}
	store.unsynced.Store(unsynced)
	if externallyCoordinated {
		if err := hostthread.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			store.Close()
			return nil, err
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}
	if err := store.loadCurrentGeneration(!externallyCoordinated); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// Reload discards process-local graph state and replays the durable
// checkpoint and journal. Externally-coordinated stores take a shared graph
// lock while selecting and loading the published generation, so readers do
// not need the enclosing repository's writer lock. Reload is harmless for
// exclusive stores and is useful in tests and recovery tooling.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	if s.externalTransaction {
		// The transaction holds graph.lock, and began with a reload: until it
		// ends, only this store changes the graph. A repository reloaded for
		// each operation within one, two stats a file a guest creates.
		return nil
	}
	if s.externallyCoordinated {
		// Whether anything changed is seen without the lock: a writer that
		// appends as the journal is looked at comes after the look, which a
		// look just before it would not have seen either. Only a change is
		// read under the lock. A guest's every path lookup reloads, and the
		// lock took two of its four system calls.
		if s.journal != nil {
			unchanged, err := s.headUnchanged()
			if err != nil {
				return err
			}
			size, err := s.journalSize()
			if err != nil {
				return err
			}
			if unchanged && size == s.journalEnd {
				return nil
			}
		}
		if err := hostthread.Flock(int(s.lock.Fd()), syscall.LOCK_SH); err != nil {
			return err
		}
		defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	}
	if s.journal != nil {
		unchanged, err := s.headUnchanged()
		if err != nil {
			return err
		}
		size, err := s.journalSize()
		if err != nil {
			return err
		}
		if unchanged && size >= s.journalEnd {
			if size == s.journalEnd {
				return nil
			}
			return s.replayJournalSuffix(false)
		}
	}
	head, err := s.readHead()
	if err != nil {
		return err
	}
	return s.loadGeneration(head, false)
}

// ReloadForWrite reloads and repairs an incomplete journal tail while the
// caller holds the enclosing repository's cross-process writer lock.
func (s *Store) ReloadForWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.externallyCoordinated {
		return errors.New("ReloadForWrite is only valid for an externally-coordinated TTC graph")
	}
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	if err := hostthread.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	if s.journal != nil {
		unchanged, err := s.headUnchanged()
		if err != nil {
			return err
		}
		size, err := s.journalSize()
		if err != nil {
			return err
		}
		if unchanged && size >= s.journalEnd {
			if size == s.journalEnd {
				return nil
			}
			return s.replayJournalSuffix(true)
		}
	}
	head, err := s.readHead()
	if err != nil {
		return err
	}
	return s.loadGeneration(head, true)
}

// BeginExternalTransaction acquires an externally-coordinated graph's writer
// fence and reloads the latest committed state. The caller must pair a
// successful call with EndExternalTransaction. This is used by an enclosing
// repository to keep graph preparation and journal append in one
// cross-process critical section.
func (s *Store) BeginExternalTransaction() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.externallyCoordinated {
		return errors.New("external transaction requires an externally-coordinated TTC graph")
	}
	if s.externalTransaction {
		return errors.New("TTC snapshot graph external transaction is already active")
	}
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	if err := hostthread.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if err := s.reloadForExternalWriteLocked(); err != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		return err
	}
	s.externalTransaction = true
	return nil
}

// EndExternalTransaction releases the writer fence acquired by
// BeginExternalTransaction.
func (s *Store) EndExternalTransaction() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.externalTransaction {
		return errors.New("TTC snapshot graph external transaction is not active")
	}
	s.externalTransaction = false
	return syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
}

// reloadForExternalWriteLocked reloads the published generation while the
// caller holds both s.mu and graph.lock exclusively.
func (s *Store) reloadForExternalWriteLocked() error {
	if s.journal != nil {
		unchanged, err := s.headUnchanged()
		if err != nil {
			return err
		}
		size, err := s.journalSize()
		if err != nil {
			return err
		}
		if unchanged && size >= s.journalEnd {
			if size == s.journalEnd {
				return nil
			}
			return s.replayJournalSuffix(true)
		}
	}
	head, err := s.readHead()
	if err != nil {
		return err
	}
	return s.loadGeneration(head, true)
}

// beginImplicitExternalMutationLocked fences a graph mutation reached from a
// nominal read path, such as lazy expansion of a foreign immutable object.
// s.mu must be held. It returns true when the caller must release the fence.
func (s *Store) beginImplicitExternalMutationLocked() (bool, error) {
	if !s.externallyCoordinated || s.externalTransaction {
		return false, nil
	}
	if err := hostthread.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	if err := s.reloadForExternalWriteLocked(); err != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		return false, err
	}
	s.implicitExternalTransaction = true
	return true, nil
}

func (s *Store) endImplicitExternalMutationLocked(acquired bool) {
	if acquired {
		s.implicitExternalTransaction = false
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	}
}

// journalSize returns the size of the journal file. Every reload checks it,
// and os.File.Stat allocates what it returns.
func (s *Store) journalSize() (int64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(s.journal.Fd()), &stat); err != nil {
		return 0, err
	}
	return stat.Size, nil
}

func (s *Store) headUnchanged() (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.dirFD.Fd()), headFileName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	mtime := stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
	return uint64(stat.Dev) == s.headDev && stat.Ino == s.headIno && stat.Size == s.headSize && mtime == s.headMtime, nil
}

func (s *Store) rememberHead() error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.dirFD.Fd()), headFileName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	s.headDev = uint64(stat.Dev)
	s.headIno = stat.Ino
	s.headSize = stat.Size
	s.headMtime = stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
	return nil
}

// Close releases the store lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheStateLocked()
	if s.holding {
		graphStates.release(s.heldDirectory)
		s.holding = false
	}
	var err error
	if s.journal != nil {
		err = s.journal.Close()
		s.journal = nil
	}
	if s.lock != nil {
		if !s.externallyCoordinated || s.externalTransaction {
			if unlockErr := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); err == nil {
				err = unlockErr
			}
			s.externalTransaction = false
		}
		if closeErr := s.lock.Close(); err == nil {
			err = closeErr
		}
		s.lock = nil
	}
	if s.dirFD != nil {
		if closeErr := s.dirFD.Close(); err == nil {
			err = closeErr
		}
		s.dirFD = nil
	}
	return err
}

func graphCheckpointName(generation uint64) string {
	return fmt.Sprintf("graph-%016x.checkpoint", generation)
}

func graphJournalName(generation uint64) string {
	return fmt.Sprintf("graph-%016x.journal", generation)
}

func (s *Store) openFile(name string, flags int, mode uint32) (*os.File, error) {
	if s.dirFD == nil {
		return nil, errors.New("TTC snapshot graph is closed")
	}
	fd, err := unix.Openat(int(s.dirFD.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	// The name is diagnostic only; all I/O remains anchored to dirFD. Keeping
	// the path-shaped name also preserves useful crash/test diagnostics.
	return os.NewFile(uintptr(fd), filepath.Join(s.dir, name)), nil
}

func (s *Store) readFile(name string) ([]byte, error) {
	file, err := s.openFile(name, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readWholeFile(file)
}

// readWholeFile reads file to its end into a buffer of the file's size, as
// os.ReadFile does: io.ReadAll grows one from 512 bytes, copying a graph
// checkpoint of 100 MB over and over, which took 0.3 s of a daemon's first
// open of a repository of 100,000 files.
func readWholeFile(file *os.File) ([]byte, error) {
	size := 512
	if info, err := file.Stat(); err == nil && info.Size() < math.MaxInt32 {
		// One byte more, to read the end of the file.
		size = max(size, int(info.Size())+1)
	}
	data := make([]byte, 0, size)
	for {
		n, err := file.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)]
		}
	}
}

func (s *Store) regularFile(name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.dirFD.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%q is not a regular file", name)
	}
	return nil
}

func (s *Store) readDirNames() ([]string, error) {
	dir, err := s.openFile(".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.Readdirnames(-1)
}

func (s *Store) writeFileAtomically(destination, prefix string, data []byte) error {
	var temporary *os.File
	var temporaryName string
	for attempts := 0; attempts < 100; attempts++ {
		temporaryName = fmt.Sprintf("%s%016x", prefix, graphTemporarySequence.Add(1))
		var err error
		temporary, err = s.openFile(temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if temporary == nil {
		return errors.New("allocating temporary TTC snapshot graph file")
	}
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = unix.Unlinkat(int(s.dirFD.Fd()), temporaryName, 0)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if !s.unsynced.Load() {
		if err := temporary.Sync(); err != nil {
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(s.dirFD.Fd()), temporaryName, int(s.dirFD.Fd()), destination); err != nil {
		return err
	}
	keep = true
	return nil
}

func (s *Store) syncDirectory() error {
	if s.dirFD == nil {
		return errors.New("TTC snapshot graph is closed")
	}
	if s.unsynced.Load() {
		return nil
	}
	return s.dirFD.Sync()
}

func (s *Store) writeHead(head graphHead) error {
	data, err := json.Marshal(head)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := s.writeFileAtomically(headFileName, ".graph-head-", data); err != nil {
		return err
	}
	return s.syncDirectory()
}

func (s *Store) initializeGeneration() error {
	state := newGraphState()
	checkpoint, err := encodeGraphCheckpoint(&state)
	if err != nil {
		return err
	}
	if err := s.writeFileAtomically(graphCheckpointName(1), ".graph-checkpoint-", checkpoint); err != nil {
		return err
	}
	if err := s.writeFileAtomically(graphJournalName(1), ".graph-journal-", []byte(journalHeader)); err != nil {
		return err
	}
	if err := s.syncDirectory(); err != nil {
		return err
	}
	return s.writeHead(graphHead{Version: formatVersion, Generation: 1})
}

func (s *Store) readHead() (graphHead, error) {
	data, err := s.readFile(headFileName)
	if err != nil {
		return graphHead{}, err
	}
	return decodeHead(data)
}

func (s *Store) loadCurrentGeneration(repairTail bool) error {
	head, err := s.readHead()
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := s.readDirNames()
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if entry != "graph.lock" {
				return fmt.Errorf("unmarked or legacy TTC snapshot graph contains %q", entry)
			}
		}
		if err := s.initializeGeneration(); err != nil {
			return err
		}
		head = graphHead{Version: formatVersion, Generation: 1}
	} else if err != nil {
		return err
	}
	if head.Version == legacyFormatVersion {
		if err := s.convertLegacyGeneration(head); err != nil {
			return err
		}
		if head, err = s.readHead(); err != nil {
			return err
		}
	}
	return s.loadGeneration(head, repairTail)
}

// graphStates keeps, for the graphs this process has closed, the state each
// had, with the generation and journal position it reflects. A filesystem
// repository is opened afresh for each operation on it, and each open decoded
// and validated its whole graph again, 1.8 s for a graph of 200,000 objects,
// five times in one rehydration. An open that finds the generation's
// checkpoint unchanged starts from the cached state, and replays only the
// journal after it.
var graphStates = graphStateCache{entries: make(map[graphFileIdentity]*cachedGraphState), held: make(map[graphFileIdentity]int)}

// graphStateCacheObjects bounds the objects of the cached states.
const graphStateCacheObjects = 1 << 19

// graphFileIdentity identifies a file of a graph, or its directory.
type graphFileIdentity struct {
	dev, ino    uint64
	size, mtime int64
}

func fileIdentityAt(dirFD int, name string) (graphFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return graphFileIdentity{}, err
	}
	return graphFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino, size: stat.Size, mtime: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec}, nil
}

// directoryIdentity identifies the store's directory; its size and time,
// which its files' creation changes, are left out.
func (s *Store) directoryIdentity() (graphFileIdentity, error) {
	identity, err := fileIdentityAt(int(s.dirFD.Fd()), ".")
	identity.size, identity.mtime = 0, 0
	return identity, err
}

type cachedGraphState struct {
	generation uint64
	checkpoint graphFileIdentity
	// journal is the journal's device and inode.
	journal           graphFileIdentity
	checkpointBytes   int64
	checkpointObjects int
	journalEnd        int64
	journalWeight     int64
	// state is a closed store's, which the store that takes it changes.
	state graphState
	// indexes are the store's indexes of state, which its taker takes too:
	// rebuilt instead, they took 0.4 s for a graph of 300,000 objects.
	indexes graphIndexes
	used    uint64
	// cachedAt is when a store closed and cached it.
	cachedAt time.Time
}

// graphIndexes are a store's indexes derived from its graph state, and the
// seed of the hashes memoryTables is keyed by.
type graphIndexes struct {
	deferredObjects map[deferredKey]ObjectID
	memoryPages     *memoryPageIndex
	memoryTables    map[uint64][]ObjectID
	memoryTableSeed maphash.Seed
}

type graphStateCache struct {
	mu sync.Mutex
	// entries holds a state per graph directory.
	entries map[graphFileIdentity]*cachedGraphState
	// held counts, for each graph directory, the open stores that took its
	// state from the cache.
	held    map[graphFileIdentity]int
	objects int
	uses    uint64
}

// take returns the state cached of directory, if it is of generation and
// checkpoint, and forgets it: its taker changes it in place, and caches it
// again when it closes. Takers copied it, which took 0.5-0.8 s for a graph of
// 300,000 objects at each opening of a repository; one that opens it while
// another holds its state loads it afresh instead.
func (c *graphStateCache) take(directory graphFileIdentity, generation uint64, checkpoint graphFileIdentity) (*cachedGraphState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[directory]
	if !ok || entry.generation != generation || entry.checkpoint != checkpoint {
		return nil, false
	}
	delete(c.entries, directory)
	c.objects -= len(entry.state.Objects)
	c.held[directory]++
	return entry, true
}

// release records that a store that took directory's state holds it no more.
func (c *graphStateCache) release(directory graphFileIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[directory]--; c.held[directory] <= 0 {
		delete(c.held, directory)
	}
}

// GraphHeld reports whether a store of this process that took the graph at
// dir from the cache has it open: another opening would load it afresh,
// 1.7 s for a graph of 300,000 objects.
func GraphHeld(dir string) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		return false, err
	}
	graphStates.mu.Lock()
	defer graphStates.mu.Unlock()
	return graphStates.held[graphFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}] > 0, nil
}

func (c *graphStateCache) put(directory graphFileIdentity, entry *cachedGraphState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.entries[directory]; ok {
		c.objects -= len(previous.state.Objects)
		delete(c.entries, directory)
	}
	if len(entry.state.Objects) > graphStateCacheObjects {
		return
	}
	c.uses++
	entry.used = c.uses
	entry.cachedAt = time.Now()
	c.entries[directory] = entry
	c.objects += len(entry.state.Objects)
	for c.objects > graphStateCacheObjects {
		var oldest graphFileIdentity
		var oldestEntry *cachedGraphState
		for key, candidate := range c.entries {
			if oldestEntry == nil || candidate.used < oldestEntry.used {
				oldest, oldestEntry = key, candidate
			}
		}
		c.objects -= len(oldestEntry.state.Objects)
		delete(c.entries, oldest)
	}
}

// GraphVersion identifies a graph's committed state by its files: the
// generation its head names, and the identities of that generation's
// checkpoint and journal, the journal's size among them. A commit appends to
// the journal, and a checkpoint starts a generation: an unchanged version is
// an unchanged graph.
type GraphVersion struct {
	generation uint64
	checkpoint graphFileIdentity
	journal    graphFileIdentity
}

// GraphVersionAt returns the version of the graph at dir, without opening it.
func GraphVersionAt(dir string) (GraphVersion, error) {
	data, err := os.ReadFile(filepath.Join(dir, headFileName))
	if err != nil {
		return GraphVersion{}, err
	}
	head, err := decodeHead(data)
	if err != nil {
		return GraphVersion{}, err
	}
	identity := func(name string) (graphFileIdentity, error) {
		var stat unix.Stat_t
		if err := unix.Stat(filepath.Join(dir, name), &stat); err != nil {
			return graphFileIdentity{}, err
		}
		return graphFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino, size: stat.Size, mtime: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec}, nil
	}
	version := GraphVersion{generation: head.Generation}
	if version.checkpoint, err = identity(graphCheckpointName(head.Generation)); err != nil {
		return GraphVersion{}, err
	}
	if version.journal, err = identity(graphJournalName(head.Generation)); err != nil {
		return GraphVersion{}, err
	}
	return version, nil
}

// ExpireGraphStates forgets the states cached of graphs no store has opened
// for idle, and returns how many objects they held. A filesystem
// repository's graph is opened several times in one operation, and a
// dehydrated branch's then not for hours: its state, 129 MB for 100,000
// files, stayed until newer ones outgrew the cache.
func ExpireGraphStates(idle time.Duration) int {
	graphStates.mu.Lock()
	defer graphStates.mu.Unlock()
	now := time.Now()
	expired := 0
	for key, entry := range graphStates.entries {
		if now.Sub(entry.cachedAt) > idle {
			expired += len(entry.state.Objects)
			graphStates.objects -= len(entry.state.Objects)
			delete(graphStates.entries, key)
		}
	}
	return expired
}

// cacheStateLocked records the store's state in graphStates. Close calls it,
// and a closed store changes its state no more: its maps are cached as they
// are, and whoever takes them copies them. Copying them here as well took
// 0.3 s for a graph of 300,000 objects.
func (s *Store) cacheStateLocked() {
	if s.poisoned != nil || s.journal == nil || s.dirFD == nil {
		return
	}
	directory, err := s.directoryIdentity()
	if err != nil {
		return
	}
	checkpoint, err := fileIdentityAt(int(s.dirFD.Fd()), graphCheckpointName(s.generation))
	if err != nil {
		return
	}
	journal, err := fileIdentityAt(int(s.dirFD.Fd()), graphJournalName(s.generation))
	if err != nil {
		return
	}
	journal.size, journal.mtime = 0, 0
	graphStates.put(directory, &cachedGraphState{
		generation: s.generation, checkpoint: checkpoint, journal: journal, checkpointBytes: s.checkpointBytes,
		checkpointObjects: s.checkpointObjects, journalEnd: s.journalEnd, journalWeight: s.journalWeight, state: s.state,
		indexes: graphIndexes{
			deferredObjects: s.deferredObjects, memoryPages: s.memoryPages,
			memoryTables: s.memoryTables, memoryTableSeed: s.memoryTableSeed,
		},
	})
}

// loadCachedGeneration loads the generation head names from the state cached
// of it, if its checkpoint is the one cached, and replays the journal after the
// cached position.
func (s *Store) loadCachedGeneration(head graphHead, repairTail bool) (bool, error) {
	directory, err := s.directoryIdentity()
	if err != nil {
		return false, nil
	}
	checkpoint, err := fileIdentityAt(int(s.dirFD.Fd()), graphCheckpointName(head.Generation))
	if err != nil {
		return false, nil
	}
	cached, ok := graphStates.take(directory, head.Generation, checkpoint)
	if !ok {
		return false, nil
	}
	journal, err := s.openFile(graphJournalName(head.Generation), unix.O_RDWR, 0600)
	if err != nil {
		graphStates.release(directory)
		graphStates.put(directory, cached)
		return false, nil
	}
	header := make([]byte, len(journalHeader))
	var stat unix.Stat_t
	statErr := unix.Fstat(int(journal.Fd()), &stat)
	if _, readErr := journal.ReadAt(header, 0); readErr != nil || statErr != nil || string(header) != journalHeader ||
		uint64(stat.Dev) != cached.journal.dev || stat.Ino != cached.journal.ino || stat.Size < cached.journalEnd {
		journal.Close()
		graphStates.release(directory)
		graphStates.put(directory, cached)
		return false, nil
	}
	previous := s.journal
	previousState := s.state
	previousGeneration := s.generation
	previousEnd, previousWeight := s.journalEnd, s.journalWeight
	previousPoisoned := s.poisoned
	// The state taken is this store's to change; if loading fails, what it
	// may have changed of it is dropped with it.
	s.state = cached.state
	s.journal = journal
	s.generation = head.Generation
	s.journalEnd, s.journalWeight = cached.journalEnd, cached.journalWeight
	if cached.indexes.deferredObjects != nil && cached.indexes.memoryPages != nil {
		s.deferredObjects = cached.indexes.deferredObjects
		s.memoryPages = cached.indexes.memoryPages
		s.memoryTables = cached.indexes.memoryTables
		s.memoryTableSeed = cached.indexes.memoryTableSeed
	} else {
		s.rebuildDeferredObjectsLocked()
		s.rebuildMemoryIndexesLocked()
	}
	restore := func() {
		s.journal = previous
		s.state = previousState
		s.generation = previousGeneration
		s.journalEnd, s.journalWeight = previousEnd, previousWeight
		s.poisoned = previousPoisoned
		s.rebuildDeferredObjectsLocked()
		s.rebuildMemoryIndexesLocked()
		journal.Close()
		graphStates.release(directory)
	}
	if err := s.replayJournalSuffix(repairTail); err != nil {
		// The cached state and the journal disagree, or the journal is
		// damaged: load from the checkpoint, which reports it.
		restore()
		return false, nil
	}
	if err := s.rememberHead(); err != nil {
		restore()
		return false, err
	}
	if previous != nil {
		_ = previous.Close()
	}
	s.checkpointBytes = cached.checkpointBytes
	s.checkpointObjects = cached.checkpointObjects
	// A store holds its graph once, however often it takes its state.
	if s.holding {
		graphStates.release(directory)
	}
	s.holding, s.heldDirectory = true, directory
	return true, nil
}

func (s *Store) loadGeneration(head graphHead, repairTail bool) error {
	if head.Version != formatVersion {
		// An open converts the graph.
		return fmt.Errorf("TTC snapshot graph generation %d is of format %d", head.Generation, head.Version)
	}
	checkpointName := graphCheckpointName(head.Generation)
	journalName := graphJournalName(head.Generation)
	for _, name := range []string{checkpointName, journalName} {
		if err := s.regularFile(name); err != nil {
			return fmt.Errorf("validating selected TTC snapshot graph file %q: %w", name, err)
		}
	}
	if loaded, err := s.loadCachedGeneration(head, repairTail); err != nil || loaded {
		return err
	}
	data, err := s.readFile(checkpointName)
	if err != nil {
		return fmt.Errorf("reading TTC snapshot graph generation %d checkpoint: %w", head.Generation, err)
	}
	state, err := decodeGraphCheckpoint(data)
	if err != nil {
		return fmt.Errorf("decoding TTC snapshot graph checkpoint: %w", err)
	}
	// replayJournal validates the graph it makes of the checkpoint, whole:
	// validating the checkpoint's as well took 0.5 s for 300,000 objects.
	if err := validateGraphStateShape(&state); err != nil {
		return fmt.Errorf("invalid TTC snapshot graph checkpoint: %w", err)
	}
	journal, err := s.openFile(journalName, unix.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening TTC snapshot graph generation %d journal: %w", head.Generation, err)
	}
	previous := s.journal
	previousState := s.state
	previousGeneration := s.generation
	previousEnd, previousWeight := s.journalEnd, s.journalWeight
	previousHeadDev, previousHeadIno := s.headDev, s.headIno
	previousHeadSize, previousHeadMtime := s.headSize, s.headMtime
	checkpointObjects := len(state.Objects)
	s.state = state
	s.journal = journal
	s.generation = head.Generation
	s.journalEnd = 0
	if err := s.replayJournal(repairTail); err != nil {
		s.journal = previous
		s.state = previousState
		s.generation = previousGeneration
		s.journalEnd, s.journalWeight = previousEnd, previousWeight
		s.headDev, s.headIno = previousHeadDev, previousHeadIno
		s.headSize, s.headMtime = previousHeadSize, previousHeadMtime
		journal.Close()
		return err
	}
	if err := s.rememberHead(); err != nil {
		s.journal = previous
		s.state = previousState
		s.generation = previousGeneration
		s.journalEnd, s.journalWeight = previousEnd, previousWeight
		s.headDev, s.headIno = previousHeadDev, previousHeadIno
		s.headSize, s.headMtime = previousHeadSize, previousHeadMtime
		journal.Close()
		return err
	}
	if previous != nil {
		_ = previous.Close()
	}
	s.checkpointBytes = int64(len(data))
	s.checkpointObjects = checkpointObjects
	s.rebuildDeferredObjectsLocked()
	s.rebuildMemoryIndexesLocked()
	return nil
}

func (s *Store) rebuildDeferredObjectsLocked() {
	s.deferredObjects = make(map[deferredKey]ObjectID)
	for _, object := range s.state.Objects {
		if object.SourceCatalog != "" && !object.SourceObject.IsZero() {
			s.deferredObjects[deferredObjectKey(object.SourceCatalog, object.SourceObject)] = object.ID
		}
	}
}

// validateGraphStateShape checks what applying a journal record to state
// relies on: its header and its maps.
func validateGraphStateShape(state *graphState) error {
	if state == nil || state.Version != formatVersion || state.NextSnapshot == 0 {
		return errors.New("invalid graph state header")
	}
	if state.Snapshots == nil || state.Objects == nil || state.Roots == nil || state.Pins == nil || state.Frozen == nil || state.Handoffs == nil {
		return errors.New("graph state has nil maps")
	}
	return nil
}

func validateGraphState(state *graphState) error {
	if err := validateGraphStateShape(state); err != nil {
		return err
	}
	nextOrdinals := make(map[SnapshotID]uint64, len(state.Snapshots))
	for id, snapshot := range state.Snapshots {
		if id != snapshot.ID || snapshot.ID == 0 || !snapshot.State.valid() || snapshot.NextOrdinal == 0 {
			return fmt.Errorf("invalid snapshot %d", id)
		}
		nextOrdinals[snapshot.ID] = snapshot.NextOrdinal
	}
	// References are counted by sorting: the objects, and what the roots,
	// pins and edges reference, are each sorted, and walked together once.
	// Counting them in a table took 0.2 s of the 0.3 s it took to validate a
	// graph of 300,000 objects, most of it waiting for memory.
	counts := make([]referenceCount, 0, len(state.Objects))
	edges := 0
	for id, object := range state.Objects {
		if id != object.ID {
			return fmt.Errorf("object key %s does not match %s", id, object.ID)
		}
		if err := validateObject(object); err != nil {
			return err
		}
		next, ok := nextOrdinals[object.ID.Snapshot]
		if !ok || object.ID.Ordinal >= next {
			return fmt.Errorf("object %s has invalid snapshot origin", id)
		}
		counts = append(counts, referenceCount{id: id, refs: object.Refs, reclaiming: object.State == ObjectReclaiming})
		edges += len(object.Children)
	}
	for id, frozen := range state.Frozen {
		if !frozen {
			return fmt.Errorf("snapshot reference barrier %d is false", id)
		}
		snapshot, ok := state.Snapshots[id]
		if !ok || snapshot.State == SnapshotWorking {
			return fmt.Errorf("snapshot reference barrier %d does not select an immutable origin", id)
		}
	}
	for id, handoff := range state.Handoffs {
		if id != handoff.Snapshot {
			return fmt.Errorf("snapshot handoff key %d does not match %d", id, handoff.Snapshot)
		}
		if err := validateSnapshotLedgerHandoff(handoff); err != nil {
			return err
		}
		if handoff.Frozen != state.Frozen[id] {
			return fmt.Errorf("snapshot %d handoff and final barrier disagree", handoff.Snapshot)
		}
		snapshot, ok := state.Snapshots[id]
		if !ok {
			return fmt.Errorf("snapshot %d handoff has no origin", handoff.Snapshot)
		}
		if snapshot.State == SnapshotWorking {
			return fmt.Errorf("snapshot %d handoff selects a working origin", handoff.Snapshot)
		}
	}
	for id := range state.Frozen {
		if _, ok := state.Handoffs[id]; !ok {
			return fmt.Errorf("snapshot reference barrier %d has no handoff", id)
		}
	}
	references := make([]ObjectID, 0, len(state.Roots)+len(state.Pins)+edges)
	for name, id := range state.Roots {
		if err := validateRootName(name); err != nil {
			return err
		}
		references = append(references, id)
	}
	for name, id := range state.Pins {
		if err := validateRootName(name); err != nil {
			return fmt.Errorf("invalid pin: %w", err)
		}
		references = append(references, id)
	}
	for _, object := range state.Objects {
		for _, edge := range object.Children {
			references = append(references, edge.Child)
		}
	}
	// The two sorts are apart, and took most of the time: they run at once.
	var sorted sync.WaitGroup
	sorted.Add(1)
	go func() {
		defer sorted.Done()
		slices.SortFunc(references, compareObjectIDs)
	}()
	slices.SortFunc(counts, func(a, b referenceCount) int { return compareObjectIDs(a.id, b.id) })
	sorted.Wait()
	next := 0
	for _, count := range counts {
		if next < len(references) && compareObjectIDs(references[next], count.id) < 0 {
			return missingReference(state, references[next])
		}
		first := next
		for next < len(references) && references[next] == count.id {
			next++
		}
		if referenced := uint64(next - first); count.refs != referenced {
			return fmt.Errorf("object %s has %d references, graph has %d", count.id, count.refs, referenced)
		}
		if (count.refs == 0) != count.reclaiming {
			return fmt.Errorf("object %s state disagrees with reference count %d", count.id, count.refs)
		}
	}
	if next < len(references) {
		return missingReference(state, references[next])
	}
	return nil
}

// referenceCount is what validateGraphState checks of an object's references.
type referenceCount struct {
	id         ObjectID
	refs       uint64
	reclaiming bool
}

// missingReference describes the reference of state to missing, an object it
// lacks.
func missingReference(state *graphState, missing ObjectID) error {
	for name, id := range state.Roots {
		if id == missing {
			return fmt.Errorf("root %q references missing object %s", name, missing)
		}
	}
	for name, id := range state.Pins {
		if id == missing {
			return fmt.Errorf("pin %q references missing object %s", name, missing)
		}
	}
	for _, object := range state.Objects {
		for _, edge := range object.Children {
			if edge.Child == missing {
				return fmt.Errorf("object %s references missing child %s", object.ID, missing)
			}
		}
	}
	return fmt.Errorf("graph state references missing object %s", missing)
}

// snapshotKey returns id in 16 hexadecimal digits, as %016x formats it.
func snapshotKey(id SnapshotID) string {
	var key [16]byte
	appendHex(key[:0], uint64(id), 16)
	return string(key[:])
}

// appendObjectKey appends id's key, as key returns it.
func appendObjectKey(out []byte, id ObjectID) []byte {
	out = appendHex(out, uint64(id.Snapshot), 16)
	out = append(out, '-')
	out = appendHex(out, uint64(id.Kind), 2)
	out = append(out, '-')
	return appendHex(out, id.Ordinal, 16)
}

// appendHex appends the digits lowest of value in hexadecimal, zero-padded.
func appendHex(out []byte, value uint64, digits int) []byte {
	const hex = "0123456789abcdef"
	for shift := 4 * (digits - 1); shift >= 0; shift -= 4 {
		out = append(out, hex[value>>shift&15])
	}
	return out
}

func (s *Store) replayJournal(repairTail bool) error {
	size, err := s.journalSize()
	if err != nil {
		return err
	}
	if size == 0 {
		if _, err := s.journal.WriteAt([]byte(journalHeader), 0); err != nil {
			return err
		}
		if !s.unsynced.Load() {
			if err := s.journal.Sync(); err != nil {
				return err
			}
		}
	}
	journal, err := s.readJournalAt(0)
	if err != nil {
		return err
	}
	if len(journal) < len(journalHeader) || string(journal[:len(journalHeader)]) != journalHeader {
		return errors.New("invalid TTC snapshot graph journal header")
	}
	offset := len(journalHeader)
	s.journalEnd, s.journalWeight = int64(offset), int64(offset)
	for offset < len(journal) {
		data, length, complete, err := nextJournalRecord(journal[offset:])
		if err != nil {
			return fmt.Errorf("reading TTC snapshot graph journal record at %d: %w", offset, err)
		}
		if !complete {
			if repairTail {
				if err := s.truncateJournal(int64(offset)); err != nil {
					return err
				}
			}
			break
		}
		record, err := decodeJournalRecord(data)
		if err != nil {
			return fmt.Errorf("decoding TTC snapshot graph journal record at %d: %w", offset, err)
		}
		if err := validateJournalRecord(record); err != nil {
			return fmt.Errorf("validating TTC snapshot graph journal record at %d: %w", offset, err)
		}
		if record.Sequence > s.state.Sequence {
			if record.Sequence != s.state.Sequence+1 {
				return fmt.Errorf("journal sequence %d follows %d", record.Sequence, s.state.Sequence)
			}
			if err := applyJournalRecord(&s.state, record); err != nil {
				return fmt.Errorf("applying journal sequence %d: %w", record.Sequence, err)
			}
			s.journalWeight += journalRecordWeight(&record, length)
		}
		offset += length
		s.journalEnd = int64(offset)
	}
	if err := validateGraphState(&s.state); err != nil {
		return fmt.Errorf("validating TTC snapshot graph after journal replay: %w", err)
	}
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}

// readJournalAt reads the journal from offset to its end.
func (s *Store) readJournalAt(offset int64) ([]byte, error) {
	size, err := s.journalSize()
	if err != nil {
		return nil, err
	}
	if size < offset {
		return nil, fmt.Errorf("TTC snapshot graph journal is %d bytes, fewer than the %d replayed", size, offset)
	}
	data := make([]byte, size-offset)
	n, err := s.journal.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:n], nil
}

// nextJournalRecord returns the data of the record framed at the start of
// frames, and the length of the frame and data. It reports the record
// incomplete if frames end within it, or it is the last and fails its
// checksum: a writer that crashed appending a record can leave either.
func nextJournalRecord(frames []byte) ([]byte, int, bool, error) {
	if len(frames) < journalFrameBytes {
		return nil, 0, false, nil
	}
	size := binary.BigEndian.Uint32(frames[0:4])
	if size == 0 || size > maxJournalRecordSize {
		return nil, 0, false, fmt.Errorf("invalid TTC snapshot graph journal record length %d", size)
	}
	end := journalFrameBytes + int(size)
	if end > len(frames) {
		return nil, 0, false, nil
	}
	data := frames[journalFrameBytes:end]
	if crc32.Checksum(data, castagnoli) != binary.BigEndian.Uint32(frames[4:8]) {
		if end == len(frames) {
			return nil, 0, false, nil
		}
		return nil, 0, false, errors.New("TTC snapshot graph journal record fails its checksum")
	}
	return data, end, true, nil
}

// replayJournalSuffix consumes transactions appended by another
// externally-coordinated Store. The enclosing graph lock prevents a writer
// from appending while the suffix is selected and decoded. Every record
// carries the bounded set of materialized reference-count changes that is not
// otherwise represented by immutable PutObject operations, so replay work is
// proportional to the appended transactions rather than total graph size.
func (s *Store) replayJournalSuffix(repairTail bool) error {
	base := s.journalEnd
	journal, err := s.readJournalAt(base)
	if err != nil {
		return err
	}
	offset := 0
	var records []journalRecord
	var weight int64
	for offset < len(journal) {
		recordStart := base + int64(offset)
		data, length, complete, err := nextJournalRecord(journal[offset:])
		if err != nil {
			return fmt.Errorf("reading TTC snapshot graph journal record at %d: %w", recordStart, err)
		}
		if !complete {
			if repairTail {
				if err := s.truncateJournal(recordStart); err != nil {
					return err
				}
			}
			break
		}
		record, err := decodeJournalRecord(data)
		if err != nil {
			return fmt.Errorf("decoding TTC snapshot graph journal record at %d: %w", recordStart, err)
		}
		if err := validateJournalRecord(record); err != nil {
			return fmt.Errorf("validating TTC snapshot graph journal record at %d: %w", recordStart, err)
		}
		want := s.state.Sequence + uint64(len(records)) + 1
		if record.Sequence != want {
			return fmt.Errorf("journal sequence %d follows %d", record.Sequence, want-1)
		}
		records = append(records, record)
		weight += journalRecordWeight(&record, length)
		offset += length
	}
	for _, record := range records {
		if err := s.applyIncrementalJournalRecord(record); err != nil {
			s.poisoned = fmt.Errorf("applying appended TTC graph journal sequence %d: %w", record.Sequence, err)
			return s.poisoned
		}
	}
	s.journalEnd = base + int64(offset)
	s.journalWeight += weight
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}

func (s *Store) applyIncrementalJournalRecord(record journalRecord) error {
	oldObjects := make(map[ObjectID]Object, len(record.Ops)+len(record.References))
	for _, op := range record.Ops {
		id, ok := op.object()
		if !ok {
			continue
		}
		if object, ok := s.state.Objects[id]; ok {
			oldObjects[id] = object
		}
	}
	for _, reference := range record.References {
		if object, ok := s.state.Objects[reference.Object]; ok {
			oldObjects[reference.Object] = object
		}
	}
	if err := applyJournalRecord(&s.state, record); err != nil {
		return err
	}
	for _, object := range oldObjects {
		if object.SourceCatalog != "" && !object.SourceObject.IsZero() {
			delete(s.deferredObjects, deferredObjectKey(object.SourceCatalog, object.SourceObject))
		}
		s.unindexMemoryObjectLocked(object)
	}
	for id := range oldObjects {
		object, ok := s.state.Objects[id]
		if ok && object.SourceCatalog != "" && !object.SourceObject.IsZero() {
			s.deferredObjects[deferredObjectKey(object.SourceCatalog, object.SourceObject)] = object.ID
		}
		if ok {
			s.indexMemoryObjectLocked(object)
		}
	}
	for _, op := range record.Ops {
		if op.PutObject == nil {
			continue
		}
		if _, existed := oldObjects[op.PutObject.ID]; existed {
			continue
		}
		object, ok := s.state.Objects[op.PutObject.ID]
		if ok && object.SourceCatalog != "" && !object.SourceObject.IsZero() {
			s.deferredObjects[deferredObjectKey(object.SourceCatalog, object.SourceObject)] = object.ID
		}
		if ok {
			s.indexMemoryObjectLocked(object)
		}
	}
	return nil
}

func (s *Store) truncateJournal(offset int64) error {
	if err := s.journal.Truncate(offset); err != nil {
		return err
	}
	if s.unsynced.Load() {
		s.journalEnd = offset
		_, err := s.journal.Seek(0, io.SeekEnd)
		return err
	}
	if err := s.journal.Sync(); err != nil {
		return err
	}
	s.journalEnd = offset
	_, err := s.journal.Seek(0, io.SeekEnd)
	return err
}

func applyJournalRecord(state *graphState, record journalRecord) error {
	for _, op := range record.Ops {
		switch {
		case op.PutSnapshot != nil:
			state.putSnapshot(*op.PutSnapshot)
		case op.DeleteSnapshot != 0:
			state.deleteSnapshot(op.DeleteSnapshot)
		case op.PutObject != nil:
			if err := validateObject(*op.PutObject); err != nil {
				return err
			}
			state.putObject(cloneObject(*op.PutObject))
		case !op.DeleteObject.IsZero():
			if err := op.DeleteObject.validate(); err != nil {
				return err
			}
			state.deleteObject(op.DeleteObject)
		case op.PutRoot != nil:
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
			state.putRoot(op.RootName, *op.PutRoot)
		case op.DeleteRoot:
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
			state.deleteRoot(op.RootName)
		case op.PutPin != nil:
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
			state.putPin(op.PinName, *op.PutPin)
		case op.DeletePin:
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
			state.deletePin(op.PinName)
		case op.SetNextSnapshot != nil:
			state.NextSnapshot = *op.SetNextSnapshot
		case op.FreezeSnapshot != 0:
			handoff, ok := state.Handoffs[op.FreezeSnapshot]
			if !ok {
				return fmt.Errorf("snapshot %d has no active reference handoff", op.FreezeSnapshot)
			}
			handoff.Frozen = true
			state.putHandoff(handoff)
			state.putFrozen(op.FreezeSnapshot)
		case op.UnfreezeSnapshot != 0:
			state.deleteFrozen(op.UnfreezeSnapshot)
			state.deleteHandoff(op.UnfreezeSnapshot)
		case op.BeginHandoff != nil:
			handoff := cloneSnapshotLedgerHandoff(*op.BeginHandoff)
			if _, ok := state.Handoffs[handoff.Snapshot]; ok {
				return fmt.Errorf("snapshot %d already has an active reference handoff", handoff.Snapshot)
			}
			state.putHandoff(handoff)
			if handoff.Frozen {
				state.putFrozen(handoff.Snapshot)
			}
		case op.AppendHandoff != nil:
			mutation := *op.AppendHandoff
			handoff, ok := state.Handoffs[mutation.Object.Snapshot]
			if !ok {
				return fmt.Errorf("snapshot %d has no active reference handoff", mutation.Object.Snapshot)
			}
			if mutation.Sequence != handoff.NextSequence {
				return fmt.Errorf("snapshot %d handoff mutation %d follows %d", handoff.Snapshot, mutation.Sequence, handoff.NextSequence-1)
			}
			handoff.Mutations = append(handoff.Mutations, mutation)
			handoff.NextSequence++
			state.putHandoff(handoff)
		case op.ApplyFilesystem != nil:
			mutation, root, err := buildFilesystemMutation(state, op.ApplyFilesystem.Origin, op.ApplyFilesystem.RootName, op.ApplyFilesystem.Operations, op.ApplyFilesystem.ReleasePins)
			if err != nil {
				return err
			}
			if root != op.ApplyFilesystem.Result {
				return fmt.Errorf("filesystem journal replay produced root %s, want %s", root, op.ApplyFilesystem.Result)
			}
			if _, err := installGraphMutation(state, mutation, op.ApplyFilesystem.RootName, root, op.ApplyFilesystem.ReleasePins); err != nil {
				return err
			}
		default:
			return errors.New("empty TTC snapshot graph journal operation")
		}
	}
	for _, update := range record.References {
		object, ok := state.Objects[update.Object]
		if !ok {
			return fmt.Errorf("reference update selects missing object %s", update.Object)
		}
		object.Refs = update.Refs
		object.State = update.State
		state.putObject(object)
	}
	state.Sequence = record.Sequence
	return nil
}

func validateJournalRecord(record journalRecord) error {
	if record.Sequence == 0 {
		return errors.New("invalid TTC snapshot graph journal record header")
	}
	for _, op := range record.Ops {
		selectors := 0
		if op.PutSnapshot != nil {
			selectors++
			if op.PutSnapshot.ID == 0 || !op.PutSnapshot.State.valid() || op.PutSnapshot.NextOrdinal == 0 {
				return fmt.Errorf("invalid snapshot %d", op.PutSnapshot.ID)
			}
		}
		if op.DeleteSnapshot != 0 {
			selectors++
		}
		if op.PutObject != nil {
			selectors++
			if err := validateObject(*op.PutObject); err != nil {
				return err
			}
		}
		if !op.DeleteObject.IsZero() {
			selectors++
			if err := op.DeleteObject.validate(); err != nil {
				return err
			}
		}
		if op.PutRoot != nil {
			selectors++
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
		}
		if op.DeleteRoot {
			selectors++
			if err := validateRootName(op.RootName); err != nil {
				return err
			}
		}
		if op.PutPin != nil {
			selectors++
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
		}
		if op.DeletePin {
			selectors++
			if err := validateRootName(op.PinName); err != nil {
				return err
			}
		}
		if op.SetNextSnapshot != nil {
			selectors++
			if *op.SetNextSnapshot == 0 {
				return errors.New("invalid next snapshot ID")
			}
		}
		if op.FreezeSnapshot != 0 {
			selectors++
		}
		if op.UnfreezeSnapshot != 0 {
			selectors++
		}
		if op.BeginHandoff != nil {
			selectors++
			if err := validateSnapshotLedgerHandoff(*op.BeginHandoff); err != nil {
				return err
			}
		}
		if op.AppendHandoff != nil {
			selectors++
			if err := validateSnapshotReferenceMutation(*op.AppendHandoff); err != nil {
				return err
			}
		}
		if op.ApplyFilesystem != nil {
			selectors++
			if err := validateFilesystemJournalMutation(*op.ApplyFilesystem); err != nil {
				return err
			}
		}
		if selectors != 1 {
			return errors.New("invalid TTC snapshot graph journal operation selector")
		}
	}
	seenReferences := make(map[ObjectID]struct{}, len(record.References))
	for _, update := range record.References {
		if err := update.Object.validate(); err != nil {
			return fmt.Errorf("invalid materialized reference update: %w", err)
		}
		if _, duplicate := seenReferences[update.Object]; duplicate {
			return fmt.Errorf("duplicate materialized reference update for %s", update.Object)
		}
		seenReferences[update.Object] = struct{}{}
		if (update.Refs == 0) != (update.State == ObjectReclaiming) || (update.Refs != 0 && update.State != ObjectLive) {
			return fmt.Errorf("materialized reference update for %s has %d references in state %q", update.Object, update.Refs, update.State)
		}
	}
	return nil
}

func cloneGraphState(state graphState) graphState {
	next := graphState{
		Version: state.Version, Sequence: state.Sequence, NextSnapshot: state.NextSnapshot,
		Snapshots: make(map[SnapshotID]Snapshot, len(state.Snapshots)),
		Objects:   make(map[ObjectID]Object, len(state.Objects)),
		Roots:     make(map[string]ObjectID, len(state.Roots)),
		Pins:      make(map[string]ObjectID, len(state.Pins)),
		Frozen:    make(map[SnapshotID]bool, len(state.Frozen)),
		Handoffs:  make(map[SnapshotID]SnapshotLedgerHandoff, len(state.Handoffs)),
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
	for key, value := range state.Frozen {
		next.Frozen[key] = value
	}
	for key, value := range state.Handoffs {
		next.Handoffs[key] = cloneSnapshotLedgerHandoff(value)
	}
	return next
}

func cloneSnapshotLedgerHandoff(handoff SnapshotLedgerHandoff) SnapshotLedgerHandoff {
	handoff.Baseline = cloneObjectReferences(handoff.Baseline)
	handoff.Mutations = append([]SnapshotReferenceMutation(nil), handoff.Mutations...)
	return handoff
}

func cloneObjectReferences(references map[string]ObjectID) map[string]ObjectID {
	cloned := make(map[string]ObjectID, len(references))
	for name, object := range references {
		cloned[name] = object
	}
	return cloned
}

func validateSnapshotReferenceMutation(mutation SnapshotReferenceMutation) error {
	if mutation.Sequence == 0 || mutation.Operation == "" {
		return errors.New("invalid snapshot handoff mutation identity")
	}
	if err := validateReferenceName(mutation.Name); err != nil {
		return err
	}
	return mutation.Object.validate()
}

func validateSnapshotLedgerHandoff(handoff SnapshotLedgerHandoff) error {
	if handoff.Snapshot == 0 || handoff.NextSequence == 0 || handoff.Baseline == nil {
		return errors.New("invalid snapshot ledger handoff header")
	}
	for name, object := range handoff.Baseline {
		if err := validateReferenceName(name); err != nil {
			return err
		}
		if object.Snapshot != handoff.Snapshot {
			return fmt.Errorf("snapshot %d handoff baseline %q selects foreign object %s", handoff.Snapshot, name, object)
		}
	}
	want := uint64(1)
	for _, mutation := range handoff.Mutations {
		if err := validateSnapshotReferenceMutation(mutation); err != nil {
			return err
		}
		if mutation.Object.Snapshot != handoff.Snapshot || mutation.Sequence != want {
			return fmt.Errorf("snapshot %d has invalid handoff mutation sequence %d", handoff.Snapshot, mutation.Sequence)
		}
		want++
	}
	if handoff.NextSequence != want {
		return fmt.Errorf("snapshot %d handoff next sequence is %d, want %d", handoff.Snapshot, handoff.NextSequence, want)
	}
	return nil
}

// snapshotReferences returns the references of snapshot that a handoff of it
// names to the shared ledger: roots, pins, and edges from other origins'
// objects, but for those of shared origins, which the ledger counts from the
// objects' own published edges. Such an edge is the graph's alone: the
// published object names the source of what the graph mirrors, as
// PublishHandoffAliasing publishes it, and shared objects otherwise refer to
// shared ones only.
func snapshotReferences(state graphState, snapshot SnapshotID) map[string]ObjectID {
	references, _ := snapshotReferencesAndLocalEdges(&state, snapshot)
	return references
}

// snapshotReferencesAndLocalEdges returns what snapshotReferences does, and
// counts, by child, the edges into snapshot from objects of shared origins,
// which it leaves to the shared ledger.
func snapshotReferencesAndLocalEdges(state *graphState, snapshot SnapshotID) (map[string]ObjectID, map[ObjectID]uint64) {
	references := make(map[string]ObjectID)
	for name, object := range state.Roots {
		if object.Snapshot == snapshot {
			references["root/"+name] = object
		}
	}
	for name, object := range state.Pins {
		if object.Snapshot == snapshot {
			references["pin/"+name] = object
		}
	}
	var local map[ObjectID]uint64
	shared := make(sharedOrigins)
	for _, parent := range state.Objects {
		if !shared.contains(state, parent.ID.Snapshot) {
			addEdgeReferences(references, parent, snapshot)
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot && parent.ID.Snapshot != snapshot {
				if local == nil {
					local = make(map[ObjectID]uint64)
				}
				local[edge.Child]++
			}
		}
	}
	return references, local
}

// sharedOrigins memoizes which origins a graph state records as shared.
type sharedOrigins map[SnapshotID]bool

func (s sharedOrigins) contains(state *graphState, origin SnapshotID) bool {
	shared, known := s[origin]
	if !known {
		record, ok := state.Snapshots[origin]
		shared = ok && record.State == SnapshotShared
		s[origin] = shared
	}
	return shared
}

// addEdgeReferences adds to references parent's edges into snapshot, if
// parent is of another snapshot, named as by snapshotReferences.
func addEdgeReferences(references map[string]ObjectID, parent Object, snapshot SnapshotID) {
	if parent.ID.Snapshot == snapshot {
		return
	}
	for _, edge := range parent.Children {
		if edge.Child.Snapshot == snapshot {
			references[edgeReferenceName(parent.ID, edge.Slot)] = edge.Child
		}
	}
}

// snapshotReferenceChanges returns the references of snapshot, named as by
// snapshotReferences, that state held before and holds after the recorded
// transaction, of the roots, pins and objects the transaction touched: the
// others hold the same references before and after.
func (undo *graphUndo) snapshotReferenceChanges(state *graphState, snapshot SnapshotID) (before, after map[string]ObjectID) {
	before, after = make(map[string]ObjectID), make(map[string]ObjectID)
	for name, prior := range undo.roots {
		if prior != nil && prior.Snapshot == snapshot {
			before["root/"+name] = *prior
		}
		if current, ok := state.Roots[name]; ok && current.Snapshot == snapshot {
			after["root/"+name] = current
		}
	}
	for name, prior := range undo.pins {
		if prior != nil && prior.Snapshot == snapshot {
			before["pin/"+name] = *prior
		}
		if current, ok := state.Pins[name]; ok && current.Snapshot == snapshot {
			after["pin/"+name] = current
		}
	}
	shared := make(sharedOrigins)
	for key, prior := range undo.objects {
		if prior != nil && !shared.contains(state, prior.ID.Snapshot) {
			addEdgeReferences(before, *prior, snapshot)
		}
		if current, ok := state.Objects[key]; ok && !shared.contains(state, current.ID.Snapshot) {
			addEdgeReferences(after, current, snapshot)
		}
	}
	return before, after
}

func sameObjectReferences(left, right map[string]ObjectID) bool {
	if len(left) != len(right) {
		return false
	}
	for name, object := range left {
		if right[name] != object {
			return false
		}
	}
	return true
}

// validateFrozenSnapshotMutation refuses the transaction state records, of
// ops, if it changes a snapshot of frozen, the snapshots frozen before it.
func validateFrozenSnapshotMutation(state *graphState, frozen []SnapshotID, ops []journalOp) error {
	for _, snapshot := range frozen {
		if before, after := state.undo.snapshotReferenceChanges(state, snapshot); !sameObjectReferences(before, after) {
			return fmt.Errorf("snapshot %d references are frozen for authority handoff: %w", snapshot, ErrSnapshotLedgerFrozen)
		}
		for _, op := range ops {
			if op.PutObject != nil && op.PutObject.ID.Snapshot == snapshot {
				return fmt.Errorf("snapshot %d objects are frozen for authority handoff: %w", snapshot, ErrSnapshotLedgerFrozen)
			}
			if !op.DeleteObject.IsZero() && op.DeleteObject.Snapshot == snapshot {
				return fmt.Errorf("snapshot %d objects are frozen for authority handoff: %w", snapshot, ErrSnapshotLedgerFrozen)
			}
		}
	}
	return nil
}

// sortedIDs returns the keys of objects in ID order.
func sortedIDs[V any](objects map[ObjectID]V) []ObjectID {
	ids := make([]ObjectID, 0, len(objects))
	for id := range objects {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, compareObjectIDs)
	return ids
}

func cloneObject(object Object) Object {
	object.Data = append([]byte(nil), object.Data...)
	object.Children = append([]Edge(nil), object.Children...)
	return object
}

func (s *Store) commitLocked(ops []journalOp) error {
	return s.commitLockedPrepared(ops, nil, nil)
}

// commitLockedWithMaterializedReferences commits authoritative graph changes
// and then installs reference-count-only object updates in memory. Those
// updates are omitted from the WAL because replay derives them exactly from
// immutable edges, roots, and pins.
func (s *Store) commitLockedWithMaterializedReferences(ops []journalOp, references map[ObjectID]Object) error {
	return s.commitLockedPrepared(ops, references, nil)
}

// commitLockedPrepared writes ops and installs a transaction that was already
// built against the current state. prepared avoids rebuilding high-level
// filesystem operations after their compact WAL record has been appended.
func (s *Store) commitLockedPrepared(ops []journalOp, references map[ObjectID]Object, prepared func(*graphState) error) error {
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	// An externally-coordinated Store may coexist with manager and Sentry
	// handles for the same graph. Every mutation must retain graph.lock from
	// the reload that selected its input state through the journal append. A
	// reload followed by an unlocked mutation is not sufficient: another
	// handle can begin writing a record in between, causing this Store to
	// observe a partial tail or append from a stale position.
	if s.externallyCoordinated && !s.externalTransaction && !s.implicitExternalTransaction {
		return errors.New("externally-coordinated TTC graph mutation requires an active writer transaction")
	}
	referenceIDs := make([]ObjectID, 0, len(references))
	for id := range references {
		referenceIDs = append(referenceIDs, id)
	}
	slices.SortFunc(referenceIDs, compareObjectIDs)
	referenceUpdates := make([]journalReference, 0, len(referenceIDs))
	for _, id := range referenceIDs {
		object := references[id]
		referenceUpdates = append(referenceUpdates, journalReference{Object: object.ID, Refs: object.Refs, State: object.State})
	}
	// Preserve the old derived-index entries until the durable transaction has
	// been applied successfully. Reference-only updates can move a Page object
	// between live and reclaiming without a PutObject operation.
	affectedObjects := make(map[ObjectID]Object, len(ops)+len(referenceIDs))
	affectedIDs := make(map[ObjectID]struct{}, len(ops)+len(referenceIDs))
	for index := range ops {
		id, ok := ops[index].object()
		if !ok {
			continue
		}
		affectedIDs[id] = struct{}{}
		if object, ok := s.state.Objects[id]; ok {
			affectedObjects[id] = object
		}
	}
	for _, id := range referenceIDs {
		affectedIDs[id] = struct{}{}
		if object, ok := s.state.Objects[id]; ok {
			affectedObjects[id] = object
		}
	}
	record := journalRecord{Sequence: s.state.Sequence + 1, Ops: ops, References: referenceUpdates}
	if err := validateJournalRecord(record); err != nil {
		return err
	}
	// The overwhelmingly common filesystem transaction has no authority
	// handoff in progress. All of its semantic validation has already happened
	// while constructing ops, and validateJournalRecord makes applying it
	// infallible. Avoid cloning the entire graph for every inode mutation: that
	// made a package installation O(number of mutations * graph size).
	previewRequired := len(s.state.Handoffs) != 0 || len(s.state.Frozen) != 0
	if !previewRequired {
		for _, op := range ops {
			if op.FreezeSnapshot != 0 || op.UnfreezeSnapshot != 0 || op.BeginHandoff != nil || op.AppendHandoff != nil {
				previewRequired = true
				break
			}
		}
	}
	if previewRequired {
		// Apply the transaction in place to learn what it changes of the
		// snapshots in handoff, then undo it: it is applied for good below,
		// once its record, with the handoffs' catch-up, is written.
		handoffs := make([]SnapshotLedgerHandoff, 0, len(s.state.Handoffs))
		for _, handoff := range s.state.Handoffs {
			handoffs = append(handoffs, handoff)
		}
		slices.SortFunc(handoffs, func(a, b SnapshotLedgerHandoff) int { return cmp.Compare(a.Snapshot, b.Snapshot) })
		frozen := make([]SnapshotID, 0, len(s.state.Frozen))
		for id := range s.state.Frozen {
			frozen = append(frozen, id)
		}
		slices.Sort(frozen)
		s.state.beginUndo()
		var previewErr error
		if prepared != nil {
			previewErr = prepared(&s.state)
		} else {
			previewErr = applyJournalRecord(&s.state, record)
		}
		var mutations []journalOp
		if previewErr == nil {
			mutations = snapshotHandoffMutationOps(&s.state, handoffs)
			previewErr = validateFrozenSnapshotMutation(&s.state, frozen, ops)
		}
		s.state.rollbackUndo()
		if previewErr != nil {
			return previewErr
		}
		if len(mutations) != 0 {
			record.Ops = append(record.Ops, mutations...)
			// A prepared transaction applies only the caller's original
			// high-level mutation. The handoff mutations above are derived
			// after that prepared state was built, so using the prepared
			// installer would leave Handoffs.NextSequence unchanged in this
			// Store. A subsequent transaction would then emit the same
			// sequence numbers again, producing a journal that no fresh
			// reader can replay. Apply the complete bounded journal record
			// whenever handoff catch-up was appended. This remains O(number
			// of operations in this transaction); it does not clone or walk
			// the graph.
			prepared = nil
			if err := validateJournalRecord(record); err != nil {
				return err
			}
		}
	}
	data, err := encodeJournalRecord(&record)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxJournalRecordSize {
		return fmt.Errorf("TTC snapshot graph transaction is %d bytes, limit is %d", len(data), maxJournalRecordSize)
	}
	start := s.journalEnd
	size, err := s.journalSize()
	if err != nil {
		return err
	}
	if size != start {
		return fmt.Errorf("TTC snapshot graph journal size %d disagrees with committed end %d", size, start)
	}
	rollback := func(cause error) error {
		if err := s.journal.Truncate(start); err != nil {
			s.poisoned = fmt.Errorf("%v; journal rollback failed: %w", cause, err)
			return s.poisoned
		}
		return cause
	}
	frame := frameJournalRecord(data)
	if err := writeFullAt(s.journal, frame, start); err != nil {
		return rollback(err)
	}
	// A Store embedded in a filesystem repository follows ordinary Linux
	// filesystem durability semantics: mutations become visible immediately,
	// but need not survive a host crash until the guest calls fsync/sync or TTC
	// reaches a lifecycle durability fence. The repository calls Sync below at
	// those boundaries. Syncing every namespace mutation makes workloads such
	// as package installation serialize on a host fsync for every inode update.
	//
	// Standalone graph users retain the original commit-is-durable contract.
	if !s.externallyCoordinated {
		if err := s.journal.Sync(); err != nil {
			// The kernel may have persisted any prefix, all, or none of the record.
			// Do not issue another transaction until reopen resolves that outcome.
			s.poisoned = err
			return err
		}
	}
	s.journalEnd = start + int64(len(frame))
	s.journalWeight += journalRecordWeight(&record, len(frame))
	var applyErr error
	if prepared != nil {
		applyErr = prepared(&s.state)
		if applyErr == nil {
			s.state.Sequence = record.Sequence
		}
	} else {
		applyErr = applyJournalRecord(&s.state, record)
	}
	if applyErr != nil {
		s.poisoned = fmt.Errorf("durable TTC graph transaction could not be applied in memory: %w", applyErr)
		return s.poisoned
	}
	for id, object := range references {
		if id != object.ID {
			s.poisoned = fmt.Errorf("materialized TTC graph reference key %s does not match %s", id, object.ID)
			return s.poisoned
		}
		current, ok := s.state.Objects[id]
		if !ok || current.ID != object.ID || current.BulkLength != object.BulkLength || !bytes.Equal(current.Data, object.Data) || !slices.Equal(current.Children, object.Children) {
			s.poisoned = fmt.Errorf("materialized TTC graph reference update changes immutable object %s", id)
			return s.poisoned
		}
		// The object keeps its data and children, which are the same: a
		// memory table's, compared again below, is 15 KB.
		object.Data, object.Children = current.Data, current.Children
		s.state.Objects[id] = object
	}
	for id, before := range affectedObjects {
		// Most objects a transaction touches only gain or lose a reference,
		// which leaves a page indexed under the same identity.
		if after, ok := s.state.Objects[id]; ok && memoryObjectIndexedAlike(before, after) {
			delete(affectedIDs, id)
			continue
		}
		s.unindexMemoryObjectLocked(before)
	}
	for id := range affectedIDs {
		if object, ok := s.state.Objects[id]; ok {
			s.indexMemoryObjectLocked(object)
		}
	}
	return nil
}

func writeFullAt(file *os.File, data []byte, offset int64) error {
	for len(data) != 0 {
		n, err := file.WriteAt(data, offset)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
		offset += int64(n)
	}
	return nil
}

// Sync makes every journal transaction completed before this call durable.
// Externally-coordinated filesystem stores call this from guest fsync/sync and
// TTC lifecycle fences. Exclusive stores already synchronize every commit,
// but accepting Sync keeps the durability boundary explicit for their users.
func (s *Store) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return fmt.Errorf("TTC snapshot graph requires reopen after an uncertain journal commit: %w", s.poisoned)
	}
	if s.journal == nil {
		return errors.New("TTC snapshot graph is closed")
	}
	if s.unsynced.Load() {
		return nil
	}
	if err := s.journal.Sync(); err != nil {
		s.poisoned = err
		return err
	}
	return nil
}

// snapshotHandoffMutationOps returns the catch-up, for each of handoffs, the
// handoffs before the transaction state records, of the references the
// transaction changes of its snapshot: in name order, a release of each
// reference replaced or dropped, and a retain of each one made.
func snapshotHandoffMutationOps(state *graphState, handoffs []SnapshotLedgerHandoff) []journalOp {
	var ops []journalOp
	for _, handoff := range handoffs {
		if handoff.Frozen {
			continue
		}
		before, after := state.undo.snapshotReferenceChanges(state, handoff.Snapshot)
		names := make(map[string]struct{}, len(before)+len(after))
		for name := range before {
			names[name] = struct{}{}
		}
		for name := range after {
			names[name] = struct{}{}
		}
		ordered := make([]string, 0, len(names))
		for name := range names {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		sequence := handoff.NextSequence
		appendMutation := func(name string, object ObjectID, retain bool) {
			mutation := &SnapshotReferenceMutation{
				Sequence: sequence, Operation: fmt.Sprintf("handoff-%016x-%016x", uint64(handoff.Snapshot), sequence),
				Name: name, Object: object, Retain: retain,
			}
			ops = append(ops, journalOp{AppendHandoff: mutation})
			sequence++
		}
		for _, name := range ordered {
			old, hadOld := before[name]
			updated, hasUpdated := after[name]
			if hadOld && hasUpdated && old == updated {
				continue
			}
			if hadOld {
				appendMutation(name, old, false)
			}
			if hasUpdated {
				appendMutation(name, updated, true)
			}
		}
	}
	return ops
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

// AdoptWorking installs an exact externally allocated snapshot origin. It is
// used by clustered TTC after the authority commits the globally monotonic ID. Replays
// are idempotent only while the local origin remains working; immutable state
// can never be regressed to working.
func (s *Store) AdoptWorking(id SnapshotID) error {
	if id == 0 || id == SnapshotID(math.MaxUint64) {
		return fmt.Errorf("invalid externally allocated snapshot ID %d", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.state.Snapshots[id]; ok {
		if existing.State == SnapshotWorking {
			return nil
		}
		return fmt.Errorf("snapshot %d is already %s", id, existing.State)
	}
	snapshot := Snapshot{ID: id, State: SnapshotWorking, NextOrdinal: 1}
	ops := []journalOp{{PutSnapshot: &snapshot}}
	if id >= s.state.NextSnapshot {
		next := id + 1
		ops = append(ops, journalOp{SetNextSnapshot: &next})
	}
	return s.commitLocked(ops)
}

// Seal freezes a working origin in O(1).
func (s *Store) Seal(id SnapshotID, state SnapshotState) error {
	if state != SnapshotSealedLocal && state != SnapshotSealedVolatile {
		return fmt.Errorf("invalid seal target %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[id]
	if !ok {
		return fmt.Errorf("snapshot %d does not exist", id)
	}
	if snapshot.State != SnapshotWorking {
		return fmt.Errorf("snapshot %d is %s, want working", id, snapshot.State)
	}
	snapshot.State = state
	return s.commitLocked([]journalOp{{PutSnapshot: &snapshot}})
}

// MarkShared records that an immutable origin has completed the authoritative
// provider-to-shared handoff. Replays are idempotent; a working origin can
// never bypass sealing.
func (s *Store) MarkShared(id SnapshotID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[id]
	if !ok {
		return fmt.Errorf("snapshot %d does not exist", id)
	}
	if snapshot.State == SnapshotShared {
		return nil
	}
	if snapshot.State != SnapshotSealedLocal && snapshot.State != SnapshotSealedVolatile {
		return fmt.Errorf("snapshot %d is %s, want sealed", id, snapshot.State)
	}
	snapshot.State = SnapshotShared
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
	if uint64(workingOrigins) > math.MaxUint64-uint64(s.state.NextSnapshot) {
		return nil, errors.New("TTC snapshot ID space exhausted")
	}
	allocated := make([]SnapshotID, workingOrigins)
	for index := range allocated {
		allocated[index] = s.state.NextSnapshot + SnapshotID(index)
	}
	if err := s.sealAndForkLocked(source, state, copies, allocated); err != nil {
		return nil, err
	}
	return allocated, nil
}

// SealAndForkWithOrigins is the clustered counterpart to SealAndFork. The
// caller must first commit every supplied working origin in the cluster authority's
// state machine. Installing the exact IDs and sealing the source occur in one
// local graph transaction, so no branch can resume with an untracked origin.
func (s *Store) SealAndForkWithOrigins(source SnapshotID, state SnapshotState, copies map[string]string, workingOrigins []SnapshotID) error {
	if state != SnapshotSealedLocal && state != SnapshotSealedVolatile {
		return fmt.Errorf("invalid seal target %q", state)
	}
	if len(copies) == 0 || len(workingOrigins) == 0 {
		return errors.New("TTC snapshot fork requires roots and fresh working origins")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealAndForkLocked(source, state, copies, workingOrigins)
}

func (s *Store) sealAndForkLocked(source SnapshotID, state SnapshotState, copies map[string]string, workingOrigins []SnapshotID) error {
	snapshot, ok := s.state.Snapshots[source]
	if !ok || snapshot.State != SnapshotWorking {
		return fmt.Errorf("snapshot %d is not working", source)
	}
	seenOrigins := make(map[SnapshotID]struct{}, len(workingOrigins))
	next := s.state.NextSnapshot
	for _, id := range workingOrigins {
		if id == 0 || id == SnapshotID(math.MaxUint64) {
			return fmt.Errorf("invalid fresh working snapshot ID %d", id)
		}
		if _, duplicate := seenOrigins[id]; duplicate {
			return fmt.Errorf("fresh working snapshot ID %d is repeated", id)
		}
		seenOrigins[id] = struct{}{}
		if existing, exists := s.state.Snapshots[id]; exists {
			return fmt.Errorf("fresh working snapshot ID %d is already %s", id, existing.State)
		}
		if id >= next {
			next = id + 1
		}
	}
	destinations := make([]string, 0, len(copies))
	for destination, sourceName := range copies {
		if err := validateRootName(destination); err != nil {
			return err
		}
		if err := validateRootName(sourceName); err != nil {
			return err
		}
		if _, exists := s.state.Roots[sourceName]; !exists {
			return fmt.Errorf("source root %q does not exist", sourceName)
		}
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(copies)*2+len(workingOrigins)+2)
	for _, destination := range destinations {
		id := s.state.Roots[copies[destination]]
		object, ok := touched[id]
		if !ok {
			object = s.state.Objects[id]
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("fork root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id] = object
		if previous, exists := s.state.Roots[destination]; exists {
			old, ok := touched[previous]
			if !ok {
				old = s.state.Objects[previous]
			}
			var err error
			old, err = releaseObject(old)
			if err != nil {
				return fmt.Errorf("replacing destination root %q: %w", destination, err)
			}
			touched[previous] = old
		}
		ops = append(ops, journalOp{RootName: destination, PutRoot: &id})
	}
	ids := sortedIDs(touched)
	objectOps := make([]journalOp, 0, len(ids))
	for _, id := range ids {
		object := touched[id]
		objectOps = append(objectOps, journalOp{PutObject: &object})
	}
	ops = append(objectOps, ops...)
	snapshot.State = state
	ops = append(ops, journalOp{PutSnapshot: &snapshot})
	for _, id := range workingOrigins {
		working := Snapshot{ID: id, State: SnapshotWorking, NextOrdinal: 1}
		ops = append(ops, journalOp{PutSnapshot: &working})
	}
	if next != s.state.NextSnapshot {
		ops = append(ops, journalOp{SetNextSnapshot: &next})
	}
	if err := s.commitLocked(ops); err != nil {
		return err
	}
	return nil
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
	snapshot, ok := s.state.Snapshots[origin]
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
	touched := make(map[ObjectID]Object, len(children))
	for _, edge := range object.Children {
		child, ok := touched[edge.Child]
		if !ok {
			child, ok = s.state.Objects[edge.Child]
		}
		if !ok {
			return ObjectID{}, fmt.Errorf("child object %s does not exist", edge.Child.key())
		}
		if child.State != ObjectLive || child.Refs == math.MaxUint64 {
			return ObjectID{}, fmt.Errorf("child object %s cannot be retained", edge.Child.key())
		}
		child.Refs++
		touched[edge.Child] = child
	}
	ids := sortedIDs(touched)
	ops := make([]journalOp, 0, len(ids)+3)
	for _, id := range ids {
		child := touched[id]
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
	ids, err := s.ReserveObjects(origin, []ObjectKind{kind})
	if err != nil {
		return ObjectID{}, err
	}
	return ids[0], nil
}

// ReserveObjects durably allocates a consecutive group of object IDs in one
// journal transaction. This keeps image import and dehydration metadata work
// proportional to batches rather than the number of pages or files.
func (s *Store) ReserveObjects(origin SnapshotID, kinds []ObjectKind) ([]ObjectID, error) {
	if len(kinds) == 0 {
		return nil, errors.New("cannot reserve an empty TTC object batch")
	}
	for _, kind := range kinds {
		if !kind.valid() {
			return nil, fmt.Errorf("invalid TTC object kind %d", kind)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[origin]
	if !ok || snapshot.State != SnapshotWorking {
		return nil, fmt.Errorf("snapshot %d is not working", origin)
	}
	if snapshot.NextOrdinal == 0 || uint64(len(kinds)) > math.MaxUint64-snapshot.NextOrdinal {
		return nil, fmt.Errorf("snapshot %d object ordinal space exhausted", origin)
	}
	ids := make([]ObjectID, len(kinds))
	for index, kind := range kinds {
		ids[index] = ObjectID{Snapshot: origin, Ordinal: snapshot.NextOrdinal + uint64(index), Kind: kind}
	}
	snapshot.NextOrdinal += uint64(len(kinds))
	if err := s.commitLocked([]journalOp{{PutSnapshot: &snapshot}}); err != nil {
		return nil, err
	}
	return ids, nil
}

// ReservedObject describes one already-installed immutable bulk object.
type ReservedObject struct {
	ID         ObjectID
	Pin        string
	BulkLength uint64
	Data       []byte
	Children   []Edge
}

// CommitReservedObject makes a reserved ID reachable through pin. Bulk data
// must already have been atomically installed before this call.
func (s *Store) CommitReservedObject(id ObjectID, pin string, bulkLength uint64, data []byte, children []Edge) error {
	return s.CommitReservedObjects([]ReservedObject{{ID: id, Pin: pin, BulkLength: bulkLength, Data: data, Children: children}})
}

// CommitReservedObjects publishes a batch of installed objects and their
// construction pins in one journal transaction.
func (s *Store) CommitReservedObjects(reserved []ReservedObject) error {
	if len(reserved) == 0 {
		return errors.New("cannot commit an empty TTC object batch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	touched := make(map[ObjectID]Object, len(reserved)*2)
	getObject := func(id ObjectID) (Object, bool) {
		if object, ok := touched[id]; ok {
			return object, true
		}
		object, ok := s.state.Objects[id]
		return object, ok
	}
	seenIDs := make(map[ObjectID]struct{}, len(reserved))
	seenPins := make(map[string]struct{}, len(reserved))
	for _, item := range reserved {
		if err := item.ID.validate(); err != nil {
			return err
		}
		if err := validateRootName(item.Pin); err != nil {
			return fmt.Errorf("invalid construction pin: %w", err)
		}
		snapshot, ok := s.state.Snapshots[item.ID.Snapshot]
		if !ok || snapshot.State != SnapshotWorking || item.ID.Ordinal >= snapshot.NextOrdinal {
			return fmt.Errorf("object ID %s is not reserved in a working snapshot", item.ID.key())
		}
		if _, duplicate := seenIDs[item.ID]; duplicate {
			return fmt.Errorf("object batch repeats %s", item.ID)
		}
		seenIDs[item.ID] = struct{}{}
		if _, exists := getObject(item.ID); exists {
			return fmt.Errorf("object %s already exists", item.ID.key())
		}
		if _, duplicate := seenPins[item.Pin]; duplicate {
			return fmt.Errorf("object batch repeats construction pin %q", item.Pin)
		}
		seenPins[item.Pin] = struct{}{}
		if _, exists := s.state.Pins[item.Pin]; exists {
			return fmt.Errorf("construction pin %q already exists", item.Pin)
		}
		object := Object{ID: item.ID, State: ObjectLive, Refs: 1, BulkLength: item.BulkLength, Data: append([]byte(nil), item.Data...), Children: append([]Edge(nil), item.Children...)}
		if err := validateObject(object); err != nil {
			return err
		}
		touched[item.ID] = object
		for _, edge := range object.Children {
			child, ok := getObject(edge.Child)
			if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
				return fmt.Errorf("child object %s cannot be retained", edge.Child.key())
			}
			child.Refs++
			touched[edge.Child] = child
		}
	}
	ids := sortedIDs(touched)
	ops := make([]journalOp, 0, len(ids)+len(reserved))
	for _, id := range ids {
		object := touched[id]
		ops = append(ops, journalOp{PutObject: &object})
	}
	for _, item := range reserved {
		id := item.ID
		ops = append(ops, journalOp{PinName: item.Pin, PutPin: &id})
	}
	return s.commitLocked(ops)
}

// ResolveObject returns materialized object metadata. Deferred metadata is
// fetched from its currently attached authoritative catalog first.
func (s *Store) ResolveObject(id ObjectID) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok, err := s.resolveDeferredLocked(id)
	if err != nil {
		return Object{}, err
	}
	if !ok {
		return Object{}, os.ErrNotExist
	}
	return cloneObject(object), nil
}

// ResolveObjects makes the deferred objects among ids local, as ResolveObject
// makes each, many to a journal record.
func (s *Store) ResolveObjects(ids []ObjectID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveDeferredBatchLocked(ids)
}

// Object returns a copy of locally available object metadata. It does not
// contact a deferred provider; callers that require complete metadata use
// ResolveObject.
func (s *Store) Object(id ObjectID) (Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[id]
	return cloneObject(object), ok
}

// UpdateWorkingBulkLength records the current length of an exclusively
// mutable bulk object. Bytes may change in-place only while their origin is
// working; sealing freezes both content and length.
func (s *Store) UpdateWorkingBulkLength(id ObjectID, length uint64) error {
	if id.Kind != ObjectKindFile && id.Kind != ObjectKindPage && id.Kind != ObjectKindMemoryChunk {
		return fmt.Errorf("object %s is not bulk data", id.key())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.state.Objects[id]
	if !ok || object.State != ObjectLive {
		return fmt.Errorf("working bulk object %s does not exist", id.key())
	}
	snapshot, ok := s.state.Snapshots[id.Snapshot]
	if !ok || snapshot.State != SnapshotWorking {
		return fmt.Errorf("bulk object %s origin is not working", id.key())
	}
	if object.BulkLength == length {
		return nil
	}
	object.BulkLength = length
	return s.commitLocked([]journalOp{{PutObject: &object}})
}

// DeletePinIfPresent releases a construction pin if it still exists.
func (s *Store) DeletePinIfPresent(name string) error {
	if err := validateRootName(name); err != nil {
		return err
	}
	s.mu.RLock()
	_, ok := s.state.Pins[name]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return s.DeletePin(name)
}

// Snapshot returns a copy of snapshot authority metadata.
func (s *Store) Snapshot(id SnapshotID) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.state.Snapshots[id]
	return snapshot, ok
}

// SnapshotObjects returns a stable copy of every graph object whose immutable
// identity was allocated by snapshot. It does not walk roots or ancestry.
func (s *Store) SnapshotObjects(snapshot SnapshotID) ([]Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.state.Snapshots[snapshot]; !ok {
		return nil, os.ErrNotExist
	}
	objects := make([]Object, 0)
	for _, object := range s.state.Objects {
		if object.ID.Snapshot == snapshot {
			objects = append(objects, cloneObject(object))
		}
	}
	slices.SortFunc(objects, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	return objects, nil
}

// SnapshotLedger returns an atomic provider-to-owner handoff baseline for one
// immutable origin. References includes named roots, pins, and every incoming
// parent edge owned by another origin. Same-origin edges remain represented by
// the returned immutable objects themselves. The sum of these references and
// same-origin edges exactly reproduces each object's current reference count.
func (s *Store) SnapshotLedger(snapshot SnapshotID) ([]Object, map[string]ObjectID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshotLedger(s.state, snapshot)
}

// snapshotLedgerOrigin returns the record of snapshot, whose ledger is to be
// read: it must be sealed.
func snapshotLedgerOrigin(state graphState, snapshot SnapshotID) (Snapshot, error) {
	record, ok := state.Snapshots[snapshot]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	if record.State == SnapshotWorking {
		return Snapshot{}, fmt.Errorf("snapshot %d is still working", snapshot)
	}
	return record, nil
}

// checkSnapshotLedger checks snapshot's objects' references as snapshotLedger
// does, and returns its references, without listing its objects.
func checkSnapshotLedger(state graphState, snapshot SnapshotID) (map[string]ObjectID, error) {
	if _, err := snapshotLedgerOrigin(state, snapshot); err != nil {
		return nil, err
	}
	references, local := snapshotReferencesAndLocalEdges(&state, snapshot)
	counts := make(map[ObjectID]uint64)
	for _, parent := range state.Objects {
		if parent.ID.Snapshot != snapshot {
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				counts[edge.Child]++
			}
		}
	}
	for _, object := range references {
		counts[object]++
	}
	for _, object := range state.Objects {
		if object.ID.Snapshot == snapshot && counts[object.ID]+local[object.ID] != object.Refs {
			return nil, fmt.Errorf("snapshot %d object %s baseline has %d references, graph records %d", snapshot, object.ID, counts[object.ID]+local[object.ID], object.Refs)
		}
	}
	return references, nil
}

func snapshotLedger(state graphState, snapshot SnapshotID) ([]Object, map[string]ObjectID, error) {
	record, err := snapshotLedgerOrigin(state, snapshot)
	if err != nil {
		return nil, nil, err
	}
	// The snapshot has at most as many objects as it has given ordinals.
	objects := make([]Object, 0, min(uint64(len(state.Objects)), record.NextOrdinal))
	for _, object := range state.Objects {
		if object.ID.Snapshot == snapshot {
			objects = append(objects, cloneObject(object))
		}
	}
	slices.SortFunc(objects, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	references, local := snapshotReferencesAndLocalEdges(&state, snapshot)
	// By ID, not by key: keys were a million strings for a snapshot of
	// 300,000 objects.
	counts := make(map[ObjectID]uint64, len(objects))
	for _, parent := range objects {
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				counts[edge.Child]++
			}
		}
	}
	for _, object := range references {
		counts[object]++
	}
	for _, object := range objects {
		if counts[object.ID]+local[object.ID] != object.Refs {
			return nil, nil, fmt.Errorf("snapshot %d object %s baseline has %d references, graph records %d", snapshot, object.ID, counts[object.ID]+local[object.ID], object.Refs)
		}
	}
	if len(local) != 0 {
		// Published, the objects count what the shared ledger does.
		objects = baselineObjects(objects, snapshot, references)
	}
	return objects, references, nil
}

// OriginReferences returns what this graph still references of snapshot, an
// origin it has handed off, that the shared ledger does not count: its roots
// and pins in snapshot, named as SnapshotLedger names them, and whether a live
// object of an origin that is not shared has an edge into snapshot or reads
// its content from there as a deferred object. The shared ledger counts an
// edge from a shared origin's object itself.
func (s *Store) OriginReferences(snapshot SnapshotID) (map[string]ObjectID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	references := make(map[string]ObjectID)
	for name, object := range s.state.Roots {
		if object.Snapshot == snapshot {
			references["root/"+name] = object
		}
	}
	for name, object := range s.state.Pins {
		if object.Snapshot == snapshot {
			references["pin/"+name] = object
		}
	}
	shared := make(map[SnapshotID]bool)
	for _, parent := range s.state.Objects {
		if parent.ID.Snapshot == snapshot || parent.State != ObjectLive {
			continue
		}
		parentShared, known := shared[parent.ID.Snapshot]
		if !known {
			// An object of an origin this graph has no record of is not the
			// graph's to count either.
			record, ok := s.state.Snapshots[parent.ID.Snapshot]
			parentShared = !ok || record.State == SnapshotShared
			shared[parent.ID.Snapshot] = parentShared
		}
		if parentShared {
			continue
		}
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				return references, true
			}
		}
		// A deferred object reads its content from its source until it is
		// materialized.
		if parent.Deferred && parent.SourceObject.Snapshot == snapshot {
			return references, true
		}
	}
	return references, false
}

// BeginSnapshotLedgerHandoff durably captures the bulk-transfer baseline for
// one immutable origin. Reference mutations after the baseline are recorded in
// order until FreezeSnapshotLedger installs the final short barrier. Repeating
// the call after a crash returns the original baseline and current cursor.
func (s *Store) BeginSnapshotLedgerHandoff(snapshot SnapshotID) ([]Object, map[string]ObjectID, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, references, err := snapshotLedger(s.state, snapshot)
	if err != nil {
		return nil, nil, 0, err
	}
	if handoff, ok := s.state.Handoffs[snapshot]; ok {
		baseline := cloneObjectReferences(handoff.Baseline)
		return baselineObjects(objects, snapshot, baseline), baseline, handoff.NextSequence - 1, nil
	}
	handoff := SnapshotLedgerHandoff{
		Snapshot:     snapshot,
		NextSequence: 1,
		Baseline:     cloneObjectReferences(references),
	}
	if err := s.commitLocked([]journalOp{{BeginHandoff: &handoff}}); err != nil {
		return nil, nil, 0, err
	}
	return objects, references, 0, nil
}

// SnapshotLedgerHandoffChanges returns the ordered mutation suffix after
// cursor. The returned cursor is stable once frozen and can be acknowledged by
// an idempotent receiver.
func (s *Store) SnapshotLedgerHandoffChanges(snapshot SnapshotID, cursor uint64) ([]SnapshotReferenceMutation, uint64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	handoff, ok := s.state.Handoffs[snapshot]
	if !ok {
		return nil, 0, false, os.ErrNotExist
	}
	latest := handoff.NextSequence - 1
	if cursor > latest {
		return nil, latest, handoff.Frozen, fmt.Errorf("snapshot %d handoff cursor %d is beyond latest mutation %d", snapshot, cursor, latest)
	}
	changes := make([]SnapshotReferenceMutation, 0, latest-cursor)
	for _, mutation := range handoff.Mutations {
		if mutation.Sequence > cursor {
			changes = append(changes, mutation)
		}
	}
	return changes, latest, handoff.Frozen, nil
}

// FreezeSnapshotLedgerHandoff freezes snapshot's reference handoff as
// FreezeSnapshotLedger does, for a caller that wants the freeze alone: it
// checks the snapshot's references, but lists none of its objects, whose
// copies, sorted, took 0.4 s for a snapshot of 300,000.
func (s *Store) FreezeSnapshotLedgerHandoff(snapshot SnapshotID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	references, err := checkSnapshotLedger(s.state, snapshot)
	if err != nil {
		return err
	}
	handoff, exists := s.state.Handoffs[snapshot]
	if !exists {
		handoff = SnapshotLedgerHandoff{
			Snapshot:     snapshot,
			NextSequence: 1,
			Baseline:     cloneObjectReferences(references),
			Frozen:       true,
		}
		return s.commitLocked([]journalOp{{BeginHandoff: &handoff}})
	}
	if !handoff.Frozen {
		return s.commitLocked([]journalOp{{FreezeSnapshot: snapshot}})
	}
	return nil
}

// FreezeSnapshotLedger durably installs the final provider-to-owner handoff
// barrier and returns its original exact reference baseline. While the barrier
// exists, no root, pin, foreign parent edge, or object owned by snapshot may
// change; unrelated origins remain writable. The receiver applies the ordered
// suffix from SnapshotLedgerHandoffChanges after importing this baseline.
// Repeating the call after a crash returns the same baseline.
func (s *Store) FreezeSnapshotLedger(snapshot SnapshotID) ([]Object, map[string]ObjectID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, references, err := snapshotLedger(s.state, snapshot)
	if err != nil {
		return nil, nil, err
	}
	handoff, exists := s.state.Handoffs[snapshot]
	if !exists {
		handoff = SnapshotLedgerHandoff{
			Snapshot:     snapshot,
			NextSequence: 1,
			Baseline:     cloneObjectReferences(references),
			Frozen:       true,
		}
		if err := s.commitLocked([]journalOp{{BeginHandoff: &handoff}}); err != nil {
			return nil, nil, err
		}
		return objects, references, nil
	}
	baseline := cloneObjectReferences(handoff.Baseline)
	if !handoff.Frozen {
		if err := s.commitLocked([]journalOp{{FreezeSnapshot: snapshot}}); err != nil {
			return nil, nil, err
		}
	}
	return baselineObjects(objects, snapshot, baseline), baseline, nil
}

// baselineObjects returns objects, the current objects of snapshot, as they
// were at a handoff's baseline, whose references were baseline. A receiver
// imports them with baseline and then applies the mutations recorded since,
// so they must not count those mutations already. Same-origin edges are
// immutable, and nothing of a snapshot being handed off is collected: an
// object's count at the baseline is its same-origin edges and its references
// in baseline.
func baselineObjects(objects []Object, snapshot SnapshotID, baseline map[string]ObjectID) []Object {
	counts := make(map[ObjectID]uint64, len(objects))
	for _, parent := range objects {
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				counts[edge.Child]++
			}
		}
	}
	for _, object := range baseline {
		counts[object]++
	}
	for index := range objects {
		objects[index].Refs = counts[objects[index].ID]
		objects[index].State = ObjectLive
		if objects[index].Refs == 0 {
			objects[index].State = ObjectReclaiming
		}
	}
	return objects
}

// UnfreezeSnapshotLedger cancels a failed authority handoff. It must not be
// used after another owner has accepted the baseline; successful handoff
// instead replaces local reference authority with the routed owner ledger.
func (s *Store) UnfreezeSnapshotLedger(snapshot SnapshotID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Handoffs[snapshot]; !ok {
		return nil
	}
	return s.commitLocked([]journalOp{{UnfreezeSnapshot: snapshot}})
}

// SnapshotLedgerFrozen reports whether an origin is held at its durable final
// handoff barrier.
func (s *Store) SnapshotLedgerFrozen(snapshot SnapshotID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Frozen[snapshot]
}

// FrozenSnapshotLedger returns a snapshot whose reference ledger is frozen for
// an authority handoff, if the graph has one.
func (s *Store) FrozenSnapshotLedger() (SnapshotID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, frozen := range s.state.Frozen {
		if frozen {
			return id, true
		}
	}
	return 0, false
}

// FrozenSnapshotLedgers returns, in order, every snapshot whose reference
// ledger is frozen for an authority handoff.
func (s *Store) FrozenSnapshotLedgers() []SnapshotID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var snapshots []SnapshotID
	for id, frozen := range s.state.Frozen {
		if frozen {
			snapshots = append(snapshots, id)
		}
	}
	slices.Sort(snapshots)
	return snapshots
}

// Root returns a named root.
func (s *Store) Root(name string) (ObjectID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.state.Roots[name]
	return id, ok
}

// HasRootUnder reports whether a root is named prefix or lies under it.
func (s *Store) HasRootUnder(prefix string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name := range s.state.Roots {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}

// HasPin reports whether name currently retains an object.
func (s *Store) HasPin(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.state.Pins[name]
	return ok
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
	object, ok := s.state.Objects[id]
	if !ok {
		return fmt.Errorf("object %s does not exist", id.key())
	}
	if object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("object %s cannot be retained", id.key())
	}
	object.Refs++
	ops := []journalOp{{PutObject: &object}, {PinName: name, PutPin: &id}}
	if previous, ok := s.state.Pins[name]; ok {
		old, err := releaseObject(s.state.Objects[previous])
		if err != nil {
			return err
		}
		ops = append(ops, journalOp{PutObject: &old})
	}
	return s.commitLocked(ops)
}

// AddPins atomically installs a set of new pins. It is used by observations
// that need a coherent view of several roots: either every root is retained,
// or none are. Existing pin names are rejected.
func (s *Store) AddPins(pins map[string]ObjectID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addPinsLocked(pins)
}

func (s *Store) addPinsLocked(pins map[string]ObjectID) error {
	if len(pins) == 0 {
		return nil
	}
	names := make([]string, 0, len(pins))
	for name := range pins {
		if err := validateRootName(name); err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	touched := make(map[ObjectID]Object, len(pins))
	for _, name := range names {
		if _, exists := s.state.Pins[name]; exists {
			return fmt.Errorf("pin %q already exists", name)
		}
		id := pins[name]
		object, ok := touched[id]
		if !ok {
			object, ok = s.state.Objects[id]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id] = object
	}
	ops := make([]journalOp, 0, len(touched)+len(names))
	ids := sortedIDs(touched)
	for _, id := range ids {
		object := touched[id]
		ops = append(ops, journalOp{PutObject: &object})
	}
	for _, name := range names {
		id := pins[name]
		ops = append(ops, journalOp{PinName: name, PutPin: &id})
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
	return s.DeletePins([]string{name})
}

// DeletePins removes construction/transaction pins in one journal record.
func (s *Store) DeletePins(names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if err := validateRootName(name); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(names)*2)
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("pin batch repeats %q", name)
		}
		seen[name] = struct{}{}
		id, ok := s.state.Pins[name]
		if !ok {
			return os.ErrNotExist
		}
		object, ok := touched[id]
		if !ok {
			object, ok = s.state.Objects[id]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		object, err := releaseObject(object)
		if err != nil {
			return err
		}
		touched[id] = object
		ops = append(ops, journalOp{PinName: name, DeletePin: true})
	}
	return s.commitReleaseLocked(ops, touched)
}

// commitReleaseLocked commits ops, which drop the references recorded in
// touched, together with the collection of what only those references held.
//
// A filesystem mutation collapses the path it replaced at once. A released
// root or pin used to leave everything below it to the next Collect, and until
// then every object it had shared still counted the reference: a branch whose
// fork was removed saw its files as shared until a garbage collection ran.
// The cascade starts from the objects released here and never scans the
// graph. It is bounded like Collect, which finishes whatever it leaves, and
// while an authority handoff is in progress it leaves everything to Collect.
func (s *Store) commitReleaseLocked(ops []journalOp, touched map[ObjectID]Object) error {
	var queue []ObjectID
	for id, object := range touched {
		if object.State == ObjectReclaiming {
			queue = append(queue, id)
		}
	}
	slices.SortFunc(queue, compareObjectIDs)
	collected, err := s.collectReleasedLocked(touched, queue, maxCollectObjectsPerTransaction)
	if err != nil {
		return err
	}
	for _, id := range collected {
		ops = append(ops, journalOp{DeleteObject: id})
	}
	if err := s.commitLockedWithMaterializedReferences(ops, touched); err != nil {
		return err
	}
	if s.recordCollected {
		for _, id := range collected {
			if _, err := objectKindName(id.Kind); err == nil {
				s.collectedBulk = append(s.collectedBulk, id)
			}
		}
	}
	return nil
}

// collectReleasedLocked collects the objects in queue that a release left
// without references, and the objects their collection leaves without
// references in turn, breadth first and at most limit in all. It records the
// resulting reference changes in touched, removes the collected objects from
// it, and returns them in the order collected. While an authority handoff is
// in progress it collects nothing.
func (s *Store) collectReleasedLocked(touched map[ObjectID]Object, queue []ObjectID, limit int) ([]ObjectID, error) {
	if len(s.state.Handoffs) != 0 || len(s.state.Frozen) != 0 {
		return nil, nil
	}
	objectFor := func(id ObjectID) (Object, bool) {
		if object, ok := touched[id]; ok {
			return object, true
		}
		object, ok := s.state.Objects[id]
		return object, ok
	}
	queued := make(map[ObjectID]struct{}, len(queue))
	for _, id := range queue {
		queued[id] = struct{}{}
	}
	var collected []ObjectID
	for len(queue) != 0 && len(collected) < limit {
		id := queue[0]
		queue = queue[1:]
		object, ok := objectFor(id)
		if !ok || object.State != ObjectReclaiming || object.Refs != 0 {
			continue
		}
		for _, edge := range object.Children {
			child, ok := objectFor(edge.Child)
			if !ok {
				return nil, fmt.Errorf("reclaiming object %s references missing child %s", id, edge.Child)
			}
			child, err := releaseObject(child)
			if err != nil {
				return nil, err
			}
			touched[child.ID] = child
			if child.State == ObjectReclaiming {
				if _, ok := queued[child.ID]; !ok {
					queue = append(queue, child.ID)
					queued[child.ID] = struct{}{}
				}
			}
		}
		collected = append(collected, id)
		delete(touched, id)
	}
	return collected, nil
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
	object, ok := s.state.Objects[id]
	if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("root object %s cannot be retained", id.key())
	}
	object.Refs++
	ops := []journalOp{{PutObject: &object}, {RootName: name, PutRoot: &id}}
	if previous, ok := s.state.Roots[name]; ok {
		old := s.state.Objects[previous]
		var err error
		old, err = releaseObject(old)
		if err != nil {
			return err
		}
		ops = append(ops, journalOp{PutObject: &old})
	}
	return s.commitLocked(ops)
}

// RetainRoots atomically installs externally named root references. Replaying
// the same name-to-object bindings is a no-op; reusing an existing name for a
// different object is rejected. This is the bounded transaction used by a
// distributed origin owner to activate a remote branch or operation root
// without traversing the immutable graph below it.
func (s *Store) RetainRoots(roots map[string]ObjectID) error {
	if len(roots) == 0 {
		return nil
	}
	names := make([]string, 0, len(roots))
	for name, id := range roots {
		if err := validateRootName(name); err != nil {
			return err
		}
		if err := id.validate(); err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)

	s.mu.Lock()
	defer s.mu.Unlock()
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(roots)*2)
	for _, name := range names {
		id := roots[name]
		if previous, exists := s.state.Roots[name]; exists {
			if previous != id {
				return fmt.Errorf("root %q already retains %s, cannot retain %s", name, previous.key(), id.key())
			}
			continue
		}
		object, ok := touched[id]
		if !ok {
			object, ok = s.state.Objects[id]
		}
		if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id] = object
		ops = append(ops, journalOp{RootName: name, PutRoot: &id})
	}
	if len(ops) == 0 {
		return nil
	}
	ids := sortedIDs(touched)
	objectOps := make([]journalOp, 0, len(ids))
	for _, id := range ids {
		object := touched[id]
		objectOps = append(objectOps, journalOp{PutObject: &object})
	}
	return s.commitLocked(append(objectOps, ops...))
}

// CloneRoots atomically gives each destination a reference to the object
// named by its source root. Destinations must not already exist. Work is
// proportional only to the number of logical roots, never to the number of
// pages or filesystem objects reachable through them.
func (s *Store) CloneRoots(copies map[string]string) error {
	if len(copies) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	destinations := make([]string, 0, len(copies))
	for destination, source := range copies {
		if err := validateRootName(destination); err != nil {
			return err
		}
		if err := validateRootName(source); err != nil {
			return err
		}
		if destination == source {
			return fmt.Errorf("cannot clone root %q onto itself", source)
		}
		if _, exists := s.state.Roots[destination]; exists {
			return fmt.Errorf("destination root %q already exists", destination)
		}
		if _, exists := s.state.Roots[source]; !exists {
			return fmt.Errorf("source root %q does not exist", source)
		}
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(copies)*2)
	for _, destination := range destinations {
		id := s.state.Roots[copies[destination]]
		object, ok := touched[id]
		if !ok {
			object = s.state.Objects[id]
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id] = object
		ops = append(ops, journalOp{RootName: destination, PutRoot: &id})
	}
	ids := sortedIDs(touched)
	objectOps := make([]journalOp, 0, len(ids))
	for _, id := range ids {
		object := touched[id]
		objectOps = append(objectOps, journalOp{PutObject: &object})
	}
	return s.commitLocked(append(objectOps, ops...))
}

// DeleteRoot removes a named root and releases its object.
func (s *Store) DeleteRoot(name string) error {
	return s.DeleteRoots([]string{name})
}

// DeleteRoots removes named roots and releases their objects in one journal
// transaction. Missing roots are rejected so callers cannot silently lose a
// required lifetime reference.
func (s *Store) DeleteRoots(names []string) error {
	return s.deleteRoots(names, false)
}

// ReleaseRoots is the idempotent lifecycle form of DeleteRoots. Missing names
// are treated as an already-committed release, while all existing roots and
// reference decrements are still recorded in one journal transaction.
func (s *Store) ReleaseRoots(names []string) error {
	return s.deleteRoots(names, true)
}

// ReleaseRetainedRoots atomically releases an externally named root batch if
// every existing name still selects the expected object. Missing bindings are
// treated as completed retries; a mismatched binding rejects the whole batch.
func (s *Store) ReleaseRetainedRoots(roots map[string]ObjectID) error {
	if len(roots) == 0 {
		return nil
	}
	names := make([]string, 0, len(roots))
	for name, id := range roots {
		if err := validateRootName(name); err != nil {
			return err
		}
		if err := id.validate(); err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if current, ok := s.state.Roots[name]; ok && current != roots[name] {
			return fmt.Errorf("root %q retains %s, not expected %s", name, current.key(), roots[name].key())
		}
	}
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(roots)*2)
	for _, name := range names {
		id, ok := s.state.Roots[name]
		if !ok {
			continue
		}
		object, ok := touched[id]
		if !ok {
			object, ok = s.state.Objects[id]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		var err error
		object, err = releaseObject(object)
		if err != nil {
			return err
		}
		touched[id] = object
		ops = append(ops, journalOp{RootName: name, DeleteRoot: true})
	}
	if len(ops) == 0 {
		return nil
	}
	ids := sortedIDs(touched)
	for _, id := range ids {
		object := touched[id]
		ops = append(ops, journalOp{PutObject: &object})
	}
	return s.commitLocked(ops)
}

func (s *Store) deleteRoots(names []string, allowMissing bool) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if err := validateRootName(name); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	touched := make(map[ObjectID]Object)
	ops := make([]journalOp, 0, len(names)*2)
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("root batch repeats %q", name)
		}
		seen[name] = struct{}{}
		id, ok := s.state.Roots[name]
		if !ok {
			if allowMissing {
				continue
			}
			return os.ErrNotExist
		}
		object, ok := touched[id]
		if !ok {
			object = s.state.Objects[id]
		}
		object, err := releaseObject(object)
		if err != nil {
			return err
		}
		touched[id] = object
		ops = append(ops, journalOp{RootName: name, DeleteRoot: true})
	}
	if len(ops) == 0 {
		return nil
	}
	return s.commitReleaseLocked(ops, touched)
}

// Collect reclaims at most limit zero-count objects and cascades their child
// edge releases. It returns the number of physical object records removed.
//
// Finding the zero-count objects scans the whole graph, so Collect scans once
// and commits what it finds in bounded transactions, rather than scanning, and
// syncing the journal, for every 128 objects: releasing a dehydrated 1.5 GiB
// branch, some 400,000 objects, took 75 seconds that way.
func (s *Store) Collect(limit int) (int, error) {
	return s.collect(limit, maxCollectBatchObjects, maxCollectReferencesPerTransaction)
}

// collect is Collect with each transaction bounded to delete at most
// batchObjects objects and to update about batchReferences reference counts.
func (s *Store) collect(limit, batchObjects, batchReferences int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Handoffs) != 0 {
		// A handoff may have begun since the queue was filled. Scanning
		// again leaves the snapshot it transfers alone.
		s.collectQueue = nil
	}
	scanned := false
	collected := 0
	for collected < limit {
		if len(s.collectQueue) == 0 {
			// Only a scan can say that nothing is left, but a call that
			// collected something can leave it to the next.
			if scanned || collected != 0 {
				break
			}
			s.scanReclaimingLocked()
			scanned = true
			if len(s.collectQueue) == 0 {
				break
			}
		}
		n, err := s.collectBatchLocked(min(limit-collected, batchObjects), batchReferences)
		collected += n
		if err != nil {
			s.collectQueue = nil
			return collected, err
		}
	}
	if len(s.collectQueue) == 0 {
		s.collectQueue, s.collectQueued = nil, nil
	}
	return collected, nil
}

// scanReclaimingLocked queues every zero-count object, in ID order, except
// those of a snapshot being handed off.
func (s *Store) scanReclaimingLocked() {
	queue := make([]ObjectID, 0)
	for id, object := range s.state.Objects {
		if object.State != ObjectReclaiming {
			continue
		}
		if len(s.state.Handoffs) != 0 {
			if _, transferring := s.state.Handoffs[object.ID.Snapshot]; transferring {
				continue
			}
		}
		queue = append(queue, id)
	}
	slices.SortFunc(queue, compareObjectIDs)
	s.collectQueued = make(map[ObjectID]struct{}, len(queue))
	for _, id := range queue {
		s.collectQueued[id] = struct{}{}
	}
	s.collectQueue = queue
}

// collectBatchLocked collects zero-count objects from the front of the
// collection queue, breadth first, and commits them in one transaction: at
// most limit objects, and none more once the transaction updates
// referenceLimit counts, which an object's children can overrun by
// maxObjectEdges. The objects it leaves without references join the queue.
// It returns how many it collected.
func (s *Store) collectBatchLocked(limit, referenceLimit int) (int, error) {
	touched := make(map[ObjectID]Object)
	deleted := make(map[ObjectID]struct{})
	objectFor := func(id ObjectID) (Object, bool) {
		if object, ok := touched[id]; ok {
			return object, true
		}
		object, ok := s.state.Objects[id]
		return object, ok
	}
	collected := 0
	var bulk []ObjectID
	for len(s.collectQueue) != 0 && collected < limit && len(touched) < referenceLimit {
		id := s.collectQueue[0]
		s.collectQueue = s.collectQueue[1:]
		object, ok := objectFor(id)
		if !ok || object.State != ObjectReclaiming {
			continue
		}
		for _, edge := range object.Children {
			child, ok := objectFor(edge.Child)
			if !ok {
				return 0, fmt.Errorf("reclaiming object %s references missing child %s", object.ID, edge.Child)
			}
			var err error
			child, err = releaseObject(child)
			if err != nil {
				return 0, err
			}
			touched[child.ID] = child
			if child.State == ObjectReclaiming {
				if _, exists := s.collectQueued[child.ID]; !exists {
					s.collectQueue = append(s.collectQueue, child.ID)
					s.collectQueued[child.ID] = struct{}{}
				}
			}
		}
		delete(touched, id)
		deleted[id] = struct{}{}
		if _, err := objectKindName(object.ID.Kind); err == nil {
			bulk = append(bulk, object.ID)
		}
		collected++
	}
	if collected == 0 {
		return 0, nil
	}
	references := make(map[ObjectID]Object, len(touched))
	for id, object := range touched {
		if _, removed := deleted[id]; !removed {
			references[id] = object
		}
	}
	deletedIDs := sortedIDs(deleted)
	ops := make([]journalOp, 0, len(deletedIDs))
	for _, id := range deletedIDs {
		ops = append(ops, journalOp{DeleteObject: id})
	}
	if err := s.commitLockedWithMaterializedReferences(ops, references); err != nil {
		return 0, err
	}
	if s.recordCollected {
		s.collectedBulk = append(s.collectedBulk, bulk...)
	}
	return collected, nil
}

// RecordCollectedBulk makes the Store record, for TakeCollectedBulk, the
// objects with standalone bulk data its collection deletes. Its user must
// take them after each collection.
func (s *Store) RecordCollectedBulk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCollected = true
}

// TakeCollectedBulk returns the objects with standalone bulk data that this
// Store's collection has deleted since the last call, as RecordCollectedBulk
// asked. Once the collection is durable, their data may go, found by name
// rather than by reading every object's directory.
func (s *Store) TakeCollectedBulk() []ObjectID {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.collectedBulk
	s.collectedBulk = nil
	return ids
}

// CollectEmptySnapshots removes origins that own no graph objects and are not
// named in retain. NextSnapshot is monotonic and is never moved backward, so
// deleting an empty origin cannot cause identity reuse. The caller decides
// which authority records must remain; repositories retain every active
// branch's working origin.
func (s *Store) CollectEmptySnapshots(retain []SnapshotID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	retained := make(map[SnapshotID]struct{}, len(retain))
	for _, id := range retain {
		if id == 0 {
			return 0, errors.New("cannot retain zero snapshot ID")
		}
		retained[id] = struct{}{}
	}
	occupied := make(map[SnapshotID]struct{})
	for _, object := range s.state.Objects {
		occupied[object.ID.Snapshot] = struct{}{}
	}
	ids := make([]SnapshotID, 0)
	for _, snapshot := range s.state.Snapshots {
		if _, keep := retained[snapshot.ID]; keep {
			continue
		}
		if _, transferring := s.state.Handoffs[snapshot.ID]; transferring {
			continue
		}
		if _, used := occupied[snapshot.ID]; used {
			continue
		}
		ids = append(ids, snapshot.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return 0, nil
	}
	ops := make([]journalOp, 0, len(ids))
	for _, id := range ids {
		ops = append(ops, journalOp{DeleteSnapshot: id})
	}
	if err := s.commitLocked(ops); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// Checkpoint atomically writes a compact graph checkpoint and resets the WAL.
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.externallyCoordinated {
		return errors.New("externally-coordinated TTC graph checkpoint requires repository-level compaction")
	}
	return s.compactGenerationLocked()
}

// CheckpointExternallyCoordinated creates a new immutable checkpoint/journal
// generation while the caller holds the enclosing repository writer lock.
// graph.head is the sole publication point. Readers lock graph.lock only while
// selecting and loading a generation, so old generation names can be removed
// immediately after head publication without invalidating already-open files.
func (s *Store) CheckpointExternallyCoordinated() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.externallyCoordinated {
		return errors.New("graph is not externally coordinated")
	}
	acquired := false
	if !s.externalTransaction {
		if err := hostthread.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		acquired = true
	}
	if acquired {
		defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	}
	head, err := s.readHead()
	if err != nil {
		return err
	}
	size, err := s.journalSize()
	if err != nil {
		return err
	}
	if head.Generation != s.generation || size != s.journalEnd {
		if err := s.loadGeneration(head, true); err != nil {
			return err
		}
	}
	return s.compactGenerationLocked()
}

func (s *Store) compactGenerationLocked() error {
	if s.generation == math.MaxUint64 {
		return errors.New("TTC snapshot graph generation space exhausted")
	}
	nextGeneration := s.generation + 1
	data, err := encodeGraphCheckpoint(&s.state)
	if err != nil {
		return err
	}
	if err := s.writeFileAtomically(graphCheckpointName(nextGeneration), ".graph-checkpoint-", data); err != nil {
		return err
	}
	if err := s.writeFileAtomically(graphJournalName(nextGeneration), ".graph-journal-", []byte(journalHeader)); err != nil {
		return err
	}
	if err := s.syncDirectory(); err != nil {
		return err
	}
	journal, err := s.openFile(graphJournalName(nextGeneration), unix.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if _, err := journal.Seek(0, io.SeekEnd); err != nil {
		journal.Close()
		return err
	}
	if err := s.writeHead(graphHead{Version: formatVersion, Generation: nextGeneration}); err != nil {
		journal.Close()
		return err
	}
	if err := s.rememberHead(); err != nil {
		journal.Close()
		s.poisoned = fmt.Errorf("published TTC graph generation %d but cannot identify its head: %w", nextGeneration, err)
		return s.poisoned
	}
	previous := s.journal
	s.journal = journal
	s.generation = nextGeneration
	s.journalEnd, s.journalWeight = int64(len(journalHeader)), int64(len(journalHeader))
	s.checkpointBytes = int64(len(data))
	s.checkpointObjects = len(s.state.Objects)
	if previous != nil {
		_ = previous.Close()
	}
	entries, err := s.readDirNames()
	if err != nil {
		return err
	}
	currentCheckpoint := graphCheckpointName(nextGeneration)
	currentJournal := graphJournalName(nextGeneration)
	for _, name := range entries {
		if name == currentCheckpoint || name == currentJournal || name == headFileName || name == "graph.lock" {
			continue
		}
		if strings.HasPrefix(name, "graph-") && (strings.HasSuffix(name, ".checkpoint") || strings.HasSuffix(name, ".journal")) {
			if err := unix.Unlinkat(int(s.dirFD.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if strings.HasPrefix(name, ".graph-head-") || strings.HasPrefix(name, ".graph-checkpoint-") || strings.HasPrefix(name, ".graph-journal-") {
			if err := unix.Unlinkat(int(s.dirFD.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return s.syncDirectory()
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

// ObjectCatalog is the read-only graph surface required for reachability
// accounting. Both an authoritative Store and an ImmutableCatalog implement
// it; accounting therefore does not import reference authority into a cache.
type ObjectCatalog interface {
	Object(ObjectID) (Object, bool)
}

// ReachableObjects returns a stable copy of the immutable metadata graph
// reachable from roots. It is intended for explicit metadata transfer and
// validation; it never reads bulk file or memory bytes.
func ReachableObjects(catalog ObjectCatalog, roots []ObjectID) ([]Object, error) {
	if catalog == nil || len(roots) == 0 {
		return nil, errors.New("TTC snapshot catalog and root are required")
	}
	objects := make(map[ObjectID]Object)
	stack := append([]ObjectID(nil), roots...)
	for len(stack) != 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, ok := objects[id]; ok {
			continue
		}
		object, ok := catalog.Object(id)
		if !ok || object.State != ObjectLive {
			return nil, fmt.Errorf("reachable TTC object %s is missing or not live", id)
		}
		objects[id] = cloneObject(object)
		for _, edge := range object.Children {
			stack = append(stack, edge.Child)
		}
	}
	result := make([]Object, 0, len(objects))
	for _, object := range objects {
		result = append(result, object)
	}
	slices.SortFunc(result, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
	return result, nil
}

// ReachableBulkObjects returns every bulk object reachable from roots, keyed
// by immutable object identity. Metadata nodes are traversed but contribute no
// bytes. Missing or reclaiming objects fail closed instead of under-reporting
// resource use.
func ReachableBulkObjects(catalog ObjectCatalog, roots []ObjectID) (map[ObjectID]uint64, error) {
	if catalog == nil {
		return nil, errors.New("TTC snapshot catalog is unavailable")
	}
	result := make(map[ObjectID]uint64)
	seen := make(map[ObjectID]struct{})
	stack := append([]ObjectID(nil), roots...)
	for len(stack) != 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		object, ok := catalog.Object(id)
		if !ok || object.State != ObjectLive {
			return nil, fmt.Errorf("reachable TTC object %s is missing or not live", id)
		}
		if object.BulkLength != 0 {
			result[id] = object.BulkLength
		}
		for _, edge := range object.Children {
			stack = append(stack, edge.Child)
		}
	}
	return result, nil
}

// InspectWithoutObjects is Inspect without the objects: the graph's
// snapshots, roots and pins. Copying every object took 28 ms and 21 MB in a
// graph of 10,000 objects, where callers that list those need none.
func (s *Store) InspectWithoutObjects() Inspection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inspectWithoutObjectsLocked()
}

func (s *Store) inspectWithoutObjectsLocked() Inspection {
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
	return result
}

// Inspect returns a consistent logical graph view. Callers must not infer
// physical placement or authority from object ordering.
func (s *Store) Inspect() Inspection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := s.inspectWithoutObjectsLocked()
	for _, object := range s.state.Objects {
		result.Objects = append(result.Objects, cloneObject(object))
	}
	slices.SortFunc(result.Objects, func(a, b Object) int { return compareObjectIDs(a.ID, b.ID) })
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

// JournalBytes returns the selected WAL generation's durable byte length.
// Callers use this only to schedule compaction; correctness never depends on
// the value being current after this call returns.
func (s *Store) JournalBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.journalEnd
}

// filesystemOperationWeight is what replaying a filesystem operation costs,
// as loading so many bytes of checkpoint costs. A journal names such an
// operation in some 50 bytes, but replaying it rebuilds the paths it changes
// of the filesystem's trees, and counts anew the references of what those
// paths reference. For a filesystem of 100,000 files, created 128 files a
// transaction, as a repository commits them, an operation took 10 us; 1,024
// files a transaction, 5.6 us; and a checkpoint loads in 27 ns a byte. A
// quarter of a kilobyte keeps what replaying a journal costs within about
// one and a half times what loading its checkpoint does. Counted by what it
// costs, the journal was folded so often that creating files took half as
// long again; weighed half a kilobyte, creating them took 27-29 us each,
// against 23-25.
const filesystemOperationWeight = 1 << 8

// journalRecordWeight returns the weight of record, which a journal holds in
// frameBytes: what replaying it costs, as loading so many bytes of checkpoint
// costs.
func journalRecordWeight(record *journalRecord, frameBytes int) int64 {
	weight := int64(frameBytes)
	for index := range record.Ops {
		if mutation := record.Ops[index].ApplyFilesystem; mutation != nil {
			weight += filesystemOperationWeight * int64(len(mutation.Operations))
		}
	}
	return weight
}

// CheckpointDue reports whether the journal has grown enough to fold into a
// new checkpoint: its weight past minimum, and past the size of the
// checkpoint it would replace. A checkpoint rewrites the whole graph, so the
// second bound spreads that cost over as much journal as it writes; without
// it, importing a large memory image rewrote a checkpoint of hundreds of
// megabytes for every minimum's worth of journal. Replaying such a journal
// costs about what loading its checkpoint does.
func (s *Store) CheckpointDue(minimum int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.journalWeight >= max(minimum, s.checkpointBytes) {
		return true
	}
	// A checkpoint costs in proportion to what it holds. Once the graph has
	// shrunk to under half of what the current one holds, as when a large
	// family is removed, a new one costs less and gives back the space of
	// everything removed, however short the journal of the removal: that of
	// a family of memory tables can stay under any minimum, and so kept its
	// checkpoint whole. Each such checkpoint halves the graph at least, so
	// they cannot come often.
	return len(s.state.Objects)*2 < s.checkpointObjects
}
