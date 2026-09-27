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

	"golang.org/x/sys/unix"
)

const (
	journalHeader        = "TTCSGJ5\n"
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
	Snapshots    map[string]Snapshot
	Objects      map[string]Object
	Roots        map[string]ObjectID
	Pins         map[string]ObjectID
	Frozen       map[string]bool
	Handoffs     map[string]SnapshotLedgerHandoff

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
	snapshots    map[string]*Snapshot
	objects      map[string]*Object
	roots        map[string]*ObjectID
	pins         map[string]*ObjectID
	frozen       map[string]*bool
	handoffs     map[string]*SnapshotLedgerHandoff
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

func restoreEntries[V any](entries map[string]V, priors map[string]*V) {
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
func recordEntry[V any](priors *map[string]*V, entries map[string]V, key string) {
	if _, recorded := (*priors)[key]; recorded {
		return
	}
	if *priors == nil {
		*priors = make(map[string]*V)
	}
	if prior, ok := entries[key]; ok {
		(*priors)[key] = &prior
	} else {
		(*priors)[key] = nil
	}
}

// The setters below change the state's maps, recording what they change
// while a transaction is recorded.

func (state *graphState) putSnapshot(key string, snapshot Snapshot) {
	if state.undo != nil {
		recordEntry(&state.undo.snapshots, state.Snapshots, key)
	}
	state.Snapshots[key] = snapshot
}

func (state *graphState) deleteSnapshot(key string) {
	if state.undo != nil {
		recordEntry(&state.undo.snapshots, state.Snapshots, key)
	}
	delete(state.Snapshots, key)
}

func (state *graphState) putObject(key string, object Object) {
	if state.undo != nil {
		recordEntry(&state.undo.objects, state.Objects, key)
	}
	state.Objects[key] = object
}

func (state *graphState) deleteObject(key string) {
	if state.undo != nil {
		recordEntry(&state.undo.objects, state.Objects, key)
	}
	delete(state.Objects, key)
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

func (state *graphState) putFrozen(key string) {
	if state.undo != nil {
		recordEntry(&state.undo.frozen, state.Frozen, key)
	}
	state.Frozen[key] = true
}

func (state *graphState) deleteFrozen(key string) {
	if state.undo != nil {
		recordEntry(&state.undo.frozen, state.Frozen, key)
	}
	delete(state.Frozen, key)
}

func (state *graphState) putHandoff(key string, handoff SnapshotLedgerHandoff) {
	if state.undo != nil {
		recordEntry(&state.undo.handoffs, state.Handoffs, key)
	}
	state.Handoffs[key] = handoff
}

func (state *graphState) deleteHandoff(key string) {
	if state.undo != nil {
		recordEntry(&state.undo.handoffs, state.Handoffs, key)
	}
	delete(state.Handoffs, key)
}

func newGraphState() graphState {
	return graphState{
		Version:      formatVersion,
		NextSnapshot: 1,
		Snapshots:    make(map[string]Snapshot),
		Objects:      make(map[string]Object),
		Roots:        make(map[string]ObjectID),
		Pins:         make(map[string]ObjectID),
		Frozen:       make(map[string]bool),
		Handoffs:     make(map[string]SnapshotLedgerHandoff),
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
	DeleteObject     string
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

type journalRecord struct {
	Version    int
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
	mu         sync.RWMutex
	dir        string
	dirFD      *os.File
	lock       *os.File
	journal    *os.File
	generation uint64
	state      graphState
	poisoned   error
	journalEnd int64
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
	deferredObjects             map[string]ObjectID
	// memoryPages is a derived index of where the graph holds each page, by
	// the stable logical/version identity the guest's memory manager gave it,
	// so that a capture maps pages the graph holds rather than storing them
	// again. It counts, for each place, the live tables and pages that map the
	// page there. Most pages are held in one place; memoryPagesElsewhere has
	// the others. memoryTables finds live tables by their content, so that a
	// capture mapping the same pages as one shares it. They are rebuilt from
	// authoritative graph metadata on open and are never persisted.
	memoryPages          map[MemoryPageIdentity]memoryPageHolding
	memoryPagesElsewhere map[MemoryPageIdentity][]memoryPageHolding
	memoryTables         map[uint64][]ObjectID
	memoryTableSeed      maphash.Seed
	// deferredLoads serializes provider I/O for one foreign immutable object.
	// Provider reads must never run while mu is held: a multi-hop filesystem
	// graph may legitimately re-enter this Store while resolving an ancestor.
	deferredLoads map[string]*deferredLoad
	// collectQueue holds the zero-count objects Collect has found but not
	// reached yet, in the order it will reach them, and collectQueued every
	// object it has queued since it last scanned the graph. Callers repeat
	// Collect until it returns zero, and the queue lets them scan the graph
	// once rather than on every call. An entry may be stale: Collect skips an
	// object that is gone, and scans again before it reports nothing left.
	collectQueue  []string
	collectQueued map[string]struct{}
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

func deferredObjectKey(catalog string, id ObjectID) string {
	return catalog + "\x00" + id.key()
}

var graphTemporarySequence atomic.Uint64

// Open opens or creates a snapshot graph store at dir.
func Open(dir string) (*Store, error) {
	return open(dir, false)
}

// OpenExternallyCoordinated opens a graph whose writers are serialized by an
// enclosing cross-process transaction lock. Unlike Open, it does not retain
// graph.lock. This is intentionally a narrow integration API for repository
// implementations; ordinary graph owners should use Open.
func OpenExternallyCoordinated(dir string) (*Store, error) {
	return open(dir, true)
}

func decodeHead(data []byte) (graphHead, error) {
	var head graphHead
	if err := json.Unmarshal(data, &head); err != nil {
		return graphHead{}, fmt.Errorf("decoding TTC snapshot graph head: %w", err)
	}
	if head.Version != formatVersion || head.Generation == 0 {
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
	hasHead := false
	for _, entry := range entries {
		if entry.Name() == headFileName {
			hasHead = true
			break
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
	if err != nil || !headInfo.Mode().IsRegular() {
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
	if err != nil || !lockInfo.Mode().IsRegular() {
		return false, errors.New("marked TTC snapshot graph has no regular graph.lock")
	}
	return true, nil
}

func open(dir string, externallyCoordinated bool) (*Store, error) {
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
		deferredResolvers: make(map[string]deferredResolver), deferredObjects: make(map[string]ObjectID),
		deferredLoads: make(map[string]*deferredLoad), memoryTableSeed: maphash.MakeSeed(),
	}
	if externallyCoordinated {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
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
	if s.externallyCoordinated && !s.externalTransaction {
		if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_SH); err != nil {
			return err
		}
		defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	}
	if s.journal != nil {
		unchanged, err := s.headUnchanged()
		if err != nil {
			return err
		}
		info, err := s.journal.Stat()
		if err != nil {
			return err
		}
		if unchanged && info.Size() >= s.journalEnd {
			if info.Size() == s.journalEnd {
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
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	if s.journal != nil {
		unchanged, err := s.headUnchanged()
		if err != nil {
			return err
		}
		info, err := s.journal.Stat()
		if err != nil {
			return err
		}
		if unchanged && info.Size() >= s.journalEnd {
			if info.Size() == s.journalEnd {
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
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
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
		info, err := s.journal.Stat()
		if err != nil {
			return err
		}
		if unchanged && info.Size() >= s.journalEnd {
			if info.Size() == s.journalEnd {
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
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
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
	return io.ReadAll(file)
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
	if err := temporary.Sync(); err != nil {
		return err
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
	checkpoint, err := json.Marshal(state)
	if err != nil {
		return err
	}
	checkpoint = append(checkpoint, '\n')
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
	return s.loadGeneration(head, repairTail)
}

func (s *Store) loadGeneration(head graphHead, repairTail bool) error {
	checkpointName := graphCheckpointName(head.Generation)
	journalName := graphJournalName(head.Generation)
	for _, name := range []string{checkpointName, journalName} {
		if err := s.regularFile(name); err != nil {
			return fmt.Errorf("validating selected TTC snapshot graph file %q: %w", name, err)
		}
	}
	data, err := s.readFile(checkpointName)
	if err != nil {
		return fmt.Errorf("reading TTC snapshot graph generation %d checkpoint: %w", head.Generation, err)
	}
	var state graphState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decoding TTC snapshot graph checkpoint: %w", err)
	}
	if err := validateGraphState(&state); err != nil {
		return fmt.Errorf("invalid TTC snapshot graph checkpoint: %w", err)
	}
	journal, err := s.openFile(journalName, unix.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening TTC snapshot graph generation %d journal: %w", head.Generation, err)
	}
	previous := s.journal
	previousState := s.state
	previousGeneration := s.generation
	previousEnd := s.journalEnd
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
		s.journalEnd = previousEnd
		s.headDev, s.headIno = previousHeadDev, previousHeadIno
		s.headSize, s.headMtime = previousHeadSize, previousHeadMtime
		journal.Close()
		return err
	}
	if err := s.rememberHead(); err != nil {
		s.journal = previous
		s.state = previousState
		s.generation = previousGeneration
		s.journalEnd = previousEnd
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
	s.deferredObjects = make(map[string]ObjectID)
	for _, object := range s.state.Objects {
		if object.SourceCatalog != "" && !object.SourceObject.IsZero() {
			s.deferredObjects[deferredObjectKey(object.SourceCatalog, object.SourceObject)] = object.ID
		}
	}
}

func validateGraphState(state *graphState) error {
	if state == nil || state.Version != formatVersion || state.NextSnapshot == 0 {
		return errors.New("invalid graph state header")
	}
	if state.Snapshots == nil || state.Objects == nil || state.Roots == nil || state.Pins == nil || state.Frozen == nil || state.Handoffs == nil {
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
	for key, frozen := range state.Frozen {
		if !frozen {
			return fmt.Errorf("snapshot reference barrier %q is false", key)
		}
		id, err := strconv.ParseUint(key, 16, 64)
		if err != nil || snapshotKey(SnapshotID(id)) != key {
			return fmt.Errorf("invalid snapshot reference barrier %q", key)
		}
		snapshot, ok := state.Snapshots[key]
		if !ok || snapshot.State == SnapshotWorking {
			return fmt.Errorf("snapshot reference barrier %q does not select an immutable origin", key)
		}
	}
	for key, handoff := range state.Handoffs {
		if key != snapshotKey(handoff.Snapshot) {
			return fmt.Errorf("snapshot handoff key %q does not match %d", key, handoff.Snapshot)
		}
		if err := validateSnapshotLedgerHandoff(handoff); err != nil {
			return err
		}
		if handoff.Frozen != state.Frozen[key] {
			return fmt.Errorf("snapshot %d handoff and final barrier disagree", handoff.Snapshot)
		}
		snapshot, ok := state.Snapshots[key]
		if !ok {
			return fmt.Errorf("snapshot %d handoff has no origin", handoff.Snapshot)
		}
		if snapshot.State == SnapshotWorking {
			return fmt.Errorf("snapshot %d handoff selects a working origin", handoff.Snapshot)
		}
	}
	for key := range state.Frozen {
		if _, ok := state.Handoffs[key]; !ok {
			return fmt.Errorf("snapshot reference barrier %q has no handoff", key)
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

func (s *Store) replayJournal(repairTail bool) error {
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
	s.journalEnd = offset
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
			if !repairTail {
				_, seekErr := s.journal.Seek(0, io.SeekEnd)
				return seekErr
			}
			if err := s.truncateJournal(recordStart); err != nil {
				return err
			}
			break
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
			if !repairTail {
				_, seekErr := s.journal.Seek(0, io.SeekEnd)
				return seekErr
			}
			if err := s.truncateJournal(recordStart); err != nil {
				return err
			}
			break
		}
		offset += int64(length)
		var record journalRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decoding TTC snapshot graph journal record at %d: %w", recordStart, err)
		}
		if record.Version != formatVersion {
			return fmt.Errorf("journal record version %d, want %d", record.Version, formatVersion)
		}
		if err := validateJournalRecord(record); err != nil {
			return fmt.Errorf("validating TTC snapshot graph journal record at %d: %w", recordStart, err)
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
		s.journalEnd = offset
	}
	if err := validateGraphState(&s.state); err != nil {
		return fmt.Errorf("validating TTC snapshot graph after journal replay: %w", err)
	}
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}

// replayJournalSuffix consumes transactions appended by another
// externally-coordinated Store. The enclosing graph lock prevents a writer
// from appending while the suffix is selected and decoded. Every v5 record
// carries the bounded set of materialized reference-count changes that is not
// otherwise represented by immutable PutObject operations, so replay work is
// proportional to the appended transactions rather than total graph size.
func (s *Store) replayJournalSuffix(repairTail bool) error {
	start := s.journalEnd
	offset := start
	lengthBuffer := make([]byte, 4)
	var records []journalRecord
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
			if repairTail {
				if err := s.truncateJournal(recordStart); err != nil {
					return err
				}
			}
			offset = recordStart
			break
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
			if repairTail {
				if err := s.truncateJournal(recordStart); err != nil {
					return err
				}
			}
			offset = recordStart
			break
		}
		offset += int64(length)
		var record journalRecord
		if err := json.Unmarshal(data, &record); err != nil {
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
	}
	for _, record := range records {
		if err := s.applyIncrementalJournalRecord(record); err != nil {
			s.poisoned = fmt.Errorf("applying appended TTC graph journal sequence %d: %w", record.Sequence, err)
			return s.poisoned
		}
	}
	s.journalEnd = offset
	_, err := s.journal.Seek(0, io.SeekEnd)
	return err
}

func (s *Store) applyIncrementalJournalRecord(record journalRecord) error {
	oldObjects := make(map[string]Object, len(record.Ops)+len(record.References))
	for _, op := range record.Ops {
		if op.PutObject != nil || op.DeleteObject != "" {
			key := op.DeleteObject
			if op.PutObject != nil {
				key = op.PutObject.ID.key()
			}
			if object, ok := s.state.Objects[key]; ok {
				oldObjects[key] = object
			}
		}
	}
	for _, reference := range record.References {
		key := reference.Object.key()
		if object, ok := s.state.Objects[key]; ok {
			oldObjects[key] = object
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
	for key := range oldObjects {
		object, ok := s.state.Objects[key]
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
		key := op.PutObject.ID.key()
		if _, existed := oldObjects[key]; existed {
			continue
		}
		object, ok := s.state.Objects[key]
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
			state.putSnapshot(snapshotKey(op.PutSnapshot.ID), *op.PutSnapshot)
		case op.DeleteSnapshot != 0:
			state.deleteSnapshot(snapshotKey(op.DeleteSnapshot))
		case op.PutObject != nil:
			if err := validateObject(*op.PutObject); err != nil {
				return err
			}
			state.putObject(op.PutObject.ID.key(), cloneObject(*op.PutObject))
		case op.DeleteObject != "":
			if _, err := parseObjectKey(op.DeleteObject); err != nil {
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
			key := snapshotKey(op.FreezeSnapshot)
			handoff, ok := state.Handoffs[key]
			if !ok {
				return fmt.Errorf("snapshot %d has no active reference handoff", op.FreezeSnapshot)
			}
			handoff.Frozen = true
			state.putHandoff(key, handoff)
			state.putFrozen(key)
		case op.UnfreezeSnapshot != 0:
			state.deleteFrozen(snapshotKey(op.UnfreezeSnapshot))
			state.deleteHandoff(snapshotKey(op.UnfreezeSnapshot))
		case op.BeginHandoff != nil:
			handoff := cloneSnapshotLedgerHandoff(*op.BeginHandoff)
			key := snapshotKey(handoff.Snapshot)
			if _, ok := state.Handoffs[key]; ok {
				return fmt.Errorf("snapshot %d already has an active reference handoff", handoff.Snapshot)
			}
			state.putHandoff(key, handoff)
			if handoff.Frozen {
				state.putFrozen(key)
			}
		case op.AppendHandoff != nil:
			mutation := *op.AppendHandoff
			key := snapshotKey(mutation.Object.Snapshot)
			handoff, ok := state.Handoffs[key]
			if !ok {
				return fmt.Errorf("snapshot %d has no active reference handoff", mutation.Object.Snapshot)
			}
			if mutation.Sequence != handoff.NextSequence {
				return fmt.Errorf("snapshot %d handoff mutation %d follows %d", handoff.Snapshot, mutation.Sequence, handoff.NextSequence-1)
			}
			handoff.Mutations = append(handoff.Mutations, mutation)
			handoff.NextSequence++
			state.putHandoff(key, handoff)
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
		key := update.Object.key()
		object, ok := state.Objects[key]
		if !ok {
			return fmt.Errorf("reference update selects missing object %s", key)
		}
		object.Refs = update.Refs
		object.State = update.State
		state.putObject(key, object)
	}
	state.Sequence = record.Sequence
	return nil
}

func validateJournalRecord(record journalRecord) error {
	if record.Version != formatVersion || record.Sequence == 0 {
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
		if op.DeleteObject != "" {
			selectors++
			if _, err := parseObjectKey(op.DeleteObject); err != nil {
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
	seenReferences := make(map[string]struct{}, len(record.References))
	for _, update := range record.References {
		if err := update.Object.validate(); err != nil {
			return fmt.Errorf("invalid materialized reference update: %w", err)
		}
		key := update.Object.key()
		if _, duplicate := seenReferences[key]; duplicate {
			return fmt.Errorf("duplicate materialized reference update for %s", key)
		}
		seenReferences[key] = struct{}{}
		if (update.Refs == 0) != (update.State == ObjectReclaiming) || (update.Refs != 0 && update.State != ObjectLive) {
			return fmt.Errorf("materialized reference update for %s has %d references in state %q", key, update.Refs, update.State)
		}
	}
	return nil
}

func cloneGraphState(state graphState) graphState {
	next := graphState{
		Version: state.Version, Sequence: state.Sequence, NextSnapshot: state.NextSnapshot,
		Snapshots: make(map[string]Snapshot, len(state.Snapshots)),
		Objects:   make(map[string]Object, len(state.Objects)),
		Roots:     make(map[string]ObjectID, len(state.Roots)),
		Pins:      make(map[string]ObjectID, len(state.Pins)),
		Frozen:    make(map[string]bool, len(state.Frozen)),
		Handoffs:  make(map[string]SnapshotLedgerHandoff, len(state.Handoffs)),
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

func snapshotReferences(state graphState, snapshot SnapshotID) map[string]ObjectID {
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
	for _, parent := range state.Objects {
		addEdgeReferences(references, parent, snapshot)
	}
	return references
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
	for key, prior := range undo.objects {
		if prior != nil {
			addEdgeReferences(before, *prior, snapshot)
		}
		if current, ok := state.Objects[key]; ok {
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
			if op.DeleteObject != "" {
				id, err := parseObjectKey(op.DeleteObject)
				if err != nil {
					return err
				}
				if id.Snapshot == snapshot {
					return fmt.Errorf("snapshot %d objects are frozen for authority handoff: %w", snapshot, ErrSnapshotLedgerFrozen)
				}
			}
		}
	}
	return nil
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
func (s *Store) commitLockedWithMaterializedReferences(ops []journalOp, references map[string]Object) error {
	return s.commitLockedPrepared(ops, references, nil)
}

// commitLockedPrepared writes ops and installs a transaction that was already
// built against the current state. prepared avoids rebuilding high-level
// filesystem operations after their compact WAL record has been appended.
func (s *Store) commitLockedPrepared(ops []journalOp, references map[string]Object, prepared func(*graphState) error) error {
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
	referenceKeys := make([]string, 0, len(references))
	for key := range references {
		referenceKeys = append(referenceKeys, key)
	}
	sort.Strings(referenceKeys)
	referenceUpdates := make([]journalReference, 0, len(referenceKeys))
	for _, key := range referenceKeys {
		object := references[key]
		referenceUpdates = append(referenceUpdates, journalReference{Object: object.ID, Refs: object.Refs, State: object.State})
	}
	// Preserve the old derived-index entries until the durable transaction has
	// been applied successfully. Reference-only updates can move a Page object
	// between live and reclaiming without a PutObject operation.
	affectedObjects := make(map[string]Object, len(ops)+len(referenceKeys))
	affectedKeys := make(map[string]struct{}, len(ops)+len(referenceKeys))
	for _, op := range ops {
		key := op.DeleteObject
		if op.PutObject != nil {
			key = op.PutObject.ID.key()
		}
		if key == "" {
			continue
		}
		affectedKeys[key] = struct{}{}
		if object, ok := s.state.Objects[key]; ok {
			affectedObjects[key] = object
		}
	}
	for _, key := range referenceKeys {
		affectedKeys[key] = struct{}{}
		if object, ok := s.state.Objects[key]; ok {
			affectedObjects[key] = object
		}
	}
	record := journalRecord{Version: formatVersion, Sequence: s.state.Sequence + 1, Ops: ops, References: referenceUpdates}
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
		handoffKeys := make([]string, 0, len(s.state.Handoffs))
		for key := range s.state.Handoffs {
			handoffKeys = append(handoffKeys, key)
		}
		sort.Strings(handoffKeys)
		handoffs := make([]SnapshotLedgerHandoff, 0, len(handoffKeys))
		for _, key := range handoffKeys {
			handoffs = append(handoffs, s.state.Handoffs[key])
		}
		frozen := make([]SnapshotID, 0, len(s.state.Frozen))
		for key := range s.state.Frozen {
			parsed, err := strconv.ParseUint(key, 16, 64)
			if err != nil {
				return err
			}
			frozen = append(frozen, SnapshotID(parsed))
		}
		sort.Slice(frozen, func(i, j int) bool { return frozen[i] < frozen[j] })
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
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxJournalRecordSize {
		return fmt.Errorf("TTC snapshot graph transaction is %d bytes, limit is %d", len(data), maxJournalRecordSize)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	start := s.journalEnd
	info, err := s.journal.Stat()
	if err != nil {
		return err
	}
	if info.Size() != start {
		return fmt.Errorf("TTC snapshot graph journal size %d disagrees with committed end %d", info.Size(), start)
	}
	rollback := func(cause error) error {
		if err := s.journal.Truncate(start); err != nil {
			s.poisoned = fmt.Errorf("%v; journal rollback failed: %w", cause, err)
			return s.poisoned
		}
		return cause
	}
	frame := make([]byte, len(length)+len(data))
	copy(frame, length[:])
	copy(frame[len(length):], data)
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
	s.journalEnd = start + int64(len(length)) + int64(len(data))
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
	for key, object := range references {
		if key != object.ID.key() {
			s.poisoned = fmt.Errorf("materialized TTC graph reference key %q does not match %s", key, object.ID.key())
			return s.poisoned
		}
		current, ok := s.state.Objects[key]
		if !ok || current.ID != object.ID || current.BulkLength != object.BulkLength || !bytes.Equal(current.Data, object.Data) || !slices.Equal(current.Children, object.Children) {
			s.poisoned = fmt.Errorf("materialized TTC graph reference update changes immutable object %s", key)
			return s.poisoned
		}
		s.state.Objects[key] = cloneObject(object)
	}
	for key, before := range affectedObjects {
		// Most objects a transaction touches only gain or lose a reference,
		// which leaves a page indexed under the same identity.
		if after, ok := s.state.Objects[key]; ok && memoryObjectIndexedAlike(before, after) {
			delete(affectedKeys, key)
			continue
		}
		s.unindexMemoryObjectLocked(before)
	}
	for key := range affectedKeys {
		if object, ok := s.state.Objects[key]; ok {
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
// used by clustered TTC after Raft commits the globally monotonic ID. Replays
// are idempotent only while the local origin remains working; immutable state
// can never be regressed to working.
func (s *Store) AdoptWorking(id SnapshotID) error {
	if id == 0 || id == SnapshotID(math.MaxUint64) {
		return fmt.Errorf("invalid externally allocated snapshot ID %d", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.state.Snapshots[snapshotKey(id)]; ok {
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

// MarkShared records that an immutable origin has completed the authoritative
// provider-to-shared handoff. Replays are idempotent; a working origin can
// never bypass sealing.
func (s *Store) MarkShared(id SnapshotID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.state.Snapshots[snapshotKey(id)]
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
// caller must first commit every supplied working origin in the cluster Raft
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
	snapshot, ok := s.state.Snapshots[snapshotKey(source)]
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
		if existing, exists := s.state.Snapshots[snapshotKey(id)]; exists {
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
	touched := make(map[string]Object)
	ops := make([]journalOp, 0, len(copies)*2+len(workingOrigins)+2)
	for _, destination := range destinations {
		id := s.state.Roots[copies[destination]]
		object, ok := touched[id.key()]
		if !ok {
			object = s.state.Objects[id.key()]
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("fork root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id.key()] = object
		if previous, exists := s.state.Roots[destination]; exists {
			old, ok := touched[previous.key()]
			if !ok {
				old = s.state.Objects[previous.key()]
			}
			var err error
			old, err = releaseObject(old)
			if err != nil {
				return fmt.Errorf("replacing destination root %q: %w", destination, err)
			}
			touched[previous.key()] = old
		}
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
	touched := make(map[string]Object, len(children))
	for _, edge := range object.Children {
		child, ok := touched[edge.Child.key()]
		if !ok {
			child, ok = s.state.Objects[edge.Child.key()]
		}
		if !ok {
			return ObjectID{}, fmt.Errorf("child object %s does not exist", edge.Child.key())
		}
		if child.State != ObjectLive || child.Refs == math.MaxUint64 {
			return ObjectID{}, fmt.Errorf("child object %s cannot be retained", edge.Child.key())
		}
		child.Refs++
		touched[edge.Child.key()] = child
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ops := make([]journalOp, 0, len(keys)+3)
	for _, key := range keys {
		child := touched[key]
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
	snapshot, ok := s.state.Snapshots[snapshotKey(origin)]
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
	touched := make(map[string]Object, len(reserved)*2)
	getObject := func(id ObjectID) (Object, bool) {
		if object, ok := touched[id.key()]; ok {
			return object, true
		}
		object, ok := s.state.Objects[id.key()]
		return object, ok
	}
	seenIDs := make(map[string]struct{}, len(reserved))
	seenPins := make(map[string]struct{}, len(reserved))
	for _, item := range reserved {
		if err := item.ID.validate(); err != nil {
			return err
		}
		if err := validateRootName(item.Pin); err != nil {
			return fmt.Errorf("invalid construction pin: %w", err)
		}
		snapshot, ok := s.state.Snapshots[snapshotKey(item.ID.Snapshot)]
		if !ok || snapshot.State != SnapshotWorking || item.ID.Ordinal >= snapshot.NextOrdinal {
			return fmt.Errorf("object ID %s is not reserved in a working snapshot", item.ID.key())
		}
		if _, duplicate := seenIDs[item.ID.key()]; duplicate {
			return fmt.Errorf("object batch repeats %s", item.ID.key())
		}
		seenIDs[item.ID.key()] = struct{}{}
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
		touched[item.ID.key()] = object
		for _, edge := range object.Children {
			child, ok := getObject(edge.Child)
			if !ok || child.State != ObjectLive || child.Refs == math.MaxUint64 {
				return fmt.Errorf("child object %s cannot be retained", edge.Child.key())
			}
			child.Refs++
			touched[edge.Child.key()] = child
		}
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ops := make([]journalOp, 0, len(keys)+len(reserved))
	for _, key := range keys {
		object := touched[key]
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

// Object returns a copy of locally available object metadata. It does not
// contact a deferred provider; callers that require complete metadata use
// ResolveObject.
func (s *Store) Object(id ObjectID) (Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.state.Objects[id.key()]
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
	object, ok := s.state.Objects[id.key()]
	if !ok || object.State != ObjectLive {
		return fmt.Errorf("working bulk object %s does not exist", id.key())
	}
	snapshot, ok := s.state.Snapshots[snapshotKey(id.Snapshot)]
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
	snapshot, ok := s.state.Snapshots[snapshotKey(id)]
	return snapshot, ok
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

func snapshotLedger(state graphState, snapshot SnapshotID) ([]Object, map[string]ObjectID, error) {
	record, ok := state.Snapshots[snapshotKey(snapshot)]
	if !ok {
		return nil, nil, os.ErrNotExist
	}
	if record.State == SnapshotWorking {
		return nil, nil, fmt.Errorf("snapshot %d is still working", snapshot)
	}
	objects := make([]Object, 0)
	for _, object := range state.Objects {
		if object.ID.Snapshot == snapshot {
			objects = append(objects, cloneObject(object))
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID.key() < objects[j].ID.key() })
	references := snapshotReferences(state, snapshot)
	counts := make(map[string]uint64, len(objects))
	for _, parent := range objects {
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				counts[edge.Child.key()]++
			}
		}
	}
	for _, object := range references {
		counts[object.key()]++
	}
	for _, object := range objects {
		if counts[object.ID.key()] != object.Refs {
			return nil, nil, fmt.Errorf("snapshot %d object %s baseline has %d references, graph records %d", snapshot, object.ID, counts[object.ID.key()], object.Refs)
		}
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
			record, ok := s.state.Snapshots[snapshotKey(parent.ID.Snapshot)]
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
	key := snapshotKey(snapshot)
	if handoff, ok := s.state.Handoffs[key]; ok {
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
	handoff, ok := s.state.Handoffs[snapshotKey(snapshot)]
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
	key := snapshotKey(snapshot)
	handoff, exists := s.state.Handoffs[key]
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
	counts := make(map[string]uint64, len(objects))
	for _, parent := range objects {
		for _, edge := range parent.Children {
			if edge.Child.Snapshot == snapshot {
				counts[edge.Child.key()]++
			}
		}
	}
	for _, object := range baseline {
		counts[object.key()]++
	}
	for index := range objects {
		objects[index].Refs = counts[objects[index].ID.key()]
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
	key := snapshotKey(snapshot)
	if _, ok := s.state.Handoffs[key]; !ok {
		return nil
	}
	return s.commitLocked([]journalOp{{UnfreezeSnapshot: snapshot}})
}

// SnapshotLedgerFrozen reports whether an origin is held at its durable final
// handoff barrier.
func (s *Store) SnapshotLedgerFrozen(snapshot SnapshotID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Frozen[snapshotKey(snapshot)]
}

// FrozenSnapshotLedger returns a snapshot whose reference ledger is frozen for
// an authority handoff, if the graph has one.
func (s *Store) FrozenSnapshotLedger() (SnapshotID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, frozen := range s.state.Frozen {
		if !frozen {
			continue
		}
		parsed, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			continue
		}
		return SnapshotID(parsed), true
	}
	return 0, false
}

// FrozenSnapshotLedgers returns, in order, every snapshot whose reference
// ledger is frozen for an authority handoff.
func (s *Store) FrozenSnapshotLedgers() []SnapshotID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var snapshots []SnapshotID
	for key, frozen := range s.state.Frozen {
		if !frozen {
			continue
		}
		parsed, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			continue
		}
		snapshots = append(snapshots, SnapshotID(parsed))
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i] < snapshots[j] })
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
	touched := make(map[string]Object, len(pins))
	for _, name := range names {
		if _, exists := s.state.Pins[name]; exists {
			return fmt.Errorf("pin %q already exists", name)
		}
		id := pins[name]
		object, ok := touched[id.key()]
		if !ok {
			object, ok = s.state.Objects[id.key()]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id.key()] = object
	}
	ops := make([]journalOp, 0, len(touched)+len(names))
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object := touched[key]
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
	touched := make(map[string]Object)
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
		object, ok := touched[id.key()]
		if !ok {
			object, ok = s.state.Objects[id.key()]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		object, err := releaseObject(object)
		if err != nil {
			return err
		}
		touched[id.key()] = object
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
func (s *Store) commitReleaseLocked(ops []journalOp, touched map[string]Object) error {
	var queue []string
	for key, object := range touched {
		if object.State == ObjectReclaiming {
			queue = append(queue, key)
		}
	}
	sort.Strings(queue)
	collected, err := s.collectReleasedLocked(touched, queue, maxCollectObjectsPerTransaction)
	if err != nil {
		return err
	}
	for _, key := range collected {
		ops = append(ops, journalOp{DeleteObject: key})
	}
	return s.commitLockedWithMaterializedReferences(ops, touched)
}

// collectReleasedLocked collects the objects in queue that a release left
// without references, and the objects their collection leaves without
// references in turn, breadth first and at most limit in all. It records the
// resulting reference changes in touched, removes the collected objects from
// it, and returns their keys in the order collected. While an authority
// handoff is in progress it collects nothing.
func (s *Store) collectReleasedLocked(touched map[string]Object, queue []string, limit int) ([]string, error) {
	if len(s.state.Handoffs) != 0 || len(s.state.Frozen) != 0 {
		return nil, nil
	}
	objectFor := func(key string) (Object, bool) {
		if object, ok := touched[key]; ok {
			return object, true
		}
		object, ok := s.state.Objects[key]
		return object, ok
	}
	queued := make(map[string]struct{}, len(queue))
	for _, key := range queue {
		queued[key] = struct{}{}
	}
	var collected []string
	for len(queue) != 0 && len(collected) < limit {
		key := queue[0]
		queue = queue[1:]
		object, ok := objectFor(key)
		if !ok || object.State != ObjectReclaiming || object.Refs != 0 {
			continue
		}
		for _, edge := range object.Children {
			child, ok := objectFor(edge.Child.key())
			if !ok {
				return nil, fmt.Errorf("reclaiming object %s references missing child %s", key, edge.Child.key())
			}
			child, err := releaseObject(child)
			if err != nil {
				return nil, err
			}
			touched[child.ID.key()] = child
			if child.State == ObjectReclaiming {
				if _, ok := queued[child.ID.key()]; !ok {
					queue = append(queue, child.ID.key())
					queued[child.ID.key()] = struct{}{}
				}
			}
		}
		collected = append(collected, key)
		delete(touched, key)
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
	touched := make(map[string]Object)
	ops := make([]journalOp, 0, len(roots)*2)
	for _, name := range names {
		id := roots[name]
		if previous, exists := s.state.Roots[name]; exists {
			if previous != id {
				return fmt.Errorf("root %q already retains %s, cannot retain %s", name, previous.key(), id.key())
			}
			continue
		}
		object, ok := touched[id.key()]
		if !ok {
			object, ok = s.state.Objects[id.key()]
		}
		if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("root object %s cannot be retained", id.key())
		}
		object.Refs++
		touched[id.key()] = object
		ops = append(ops, journalOp{RootName: name, PutRoot: &id})
	}
	if len(ops) == 0 {
		return nil
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
	touched := make(map[string]Object)
	ops := make([]journalOp, 0, len(copies)*2)
	for _, destination := range destinations {
		id := s.state.Roots[copies[destination]]
		object, ok := touched[id.key()]
		if !ok {
			object = s.state.Objects[id.key()]
		}
		if object.State != ObjectLive || object.Refs == math.MaxUint64 {
			return fmt.Errorf("root object %s cannot be retained", id.key())
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
	touched := make(map[string]Object)
	ops := make([]journalOp, 0, len(roots)*2)
	for _, name := range names {
		id, ok := s.state.Roots[name]
		if !ok {
			continue
		}
		object, ok := touched[id.key()]
		if !ok {
			object, ok = s.state.Objects[id.key()]
		}
		if !ok {
			return fmt.Errorf("object %s does not exist", id.key())
		}
		var err error
		object, err = releaseObject(object)
		if err != nil {
			return err
		}
		touched[id.key()] = object
		ops = append(ops, journalOp{RootName: name, DeleteRoot: true})
	}
	if len(ops) == 0 {
		return nil
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object := touched[key]
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
	touched := make(map[string]Object)
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
		object, ok := touched[id.key()]
		if !ok {
			object = s.state.Objects[id.key()]
		}
		object, err := releaseObject(object)
		if err != nil {
			return err
		}
		touched[id.key()] = object
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

// scanReclaimingLocked queues every zero-count object, in key order, except
// those of a snapshot being handed off.
func (s *Store) scanReclaimingLocked() {
	queue := make([]string, 0)
	for key, object := range s.state.Objects {
		if object.State != ObjectReclaiming {
			continue
		}
		if len(s.state.Handoffs) != 0 {
			if _, transferring := s.state.Handoffs[snapshotKey(object.ID.Snapshot)]; transferring {
				continue
			}
		}
		queue = append(queue, key)
	}
	sort.Strings(queue)
	s.collectQueued = make(map[string]struct{}, len(queue))
	for _, key := range queue {
		s.collectQueued[key] = struct{}{}
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
	touched := make(map[string]Object)
	deleted := make(map[string]struct{})
	objectFor := func(key string) (Object, bool) {
		if object, ok := touched[key]; ok {
			return object, true
		}
		object, ok := s.state.Objects[key]
		return object, ok
	}
	collected := 0
	for len(s.collectQueue) != 0 && collected < limit && len(touched) < referenceLimit {
		key := s.collectQueue[0]
		s.collectQueue = s.collectQueue[1:]
		object, ok := objectFor(key)
		if !ok || object.State != ObjectReclaiming {
			continue
		}
		for _, edge := range object.Children {
			child, ok := objectFor(edge.Child.key())
			if !ok {
				return 0, fmt.Errorf("reclaiming object %s references missing child %s", object.ID.key(), edge.Child.key())
			}
			var err error
			child, err = releaseObject(child)
			if err != nil {
				return 0, err
			}
			touched[child.ID.key()] = child
			if child.State == ObjectReclaiming {
				if _, exists := s.collectQueued[child.ID.key()]; !exists {
					s.collectQueue = append(s.collectQueue, child.ID.key())
					s.collectQueued[child.ID.key()] = struct{}{}
				}
			}
		}
		delete(touched, key)
		deleted[key] = struct{}{}
		collected++
	}
	if collected == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(touched))
	for key := range touched {
		if _, removed := deleted[key]; !removed {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	deletedKeys := make([]string, 0, len(deleted))
	for key := range deleted {
		deletedKeys = append(deletedKeys, key)
	}
	sort.Strings(deletedKeys)
	ops := make([]journalOp, 0, len(deletedKeys))
	references := make(map[string]Object, len(keys))
	for _, key := range keys {
		references[key] = touched[key]
	}
	for _, key := range deletedKeys {
		ops = append(ops, journalOp{DeleteObject: key})
	}
	if err := s.commitLockedWithMaterializedReferences(ops, references); err != nil {
		return 0, err
	}
	return collected, nil
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
		if _, transferring := s.state.Handoffs[snapshotKey(snapshot.ID)]; transferring {
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
		if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
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
	info, err := s.journal.Stat()
	if err != nil {
		return err
	}
	if head.Generation != s.generation || info.Size() != s.journalEnd {
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
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err := s.writeFileAtomically(graphCheckpointName(nextGeneration), ".graph-checkpoint-", append(data, '\n')); err != nil {
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
	s.journalEnd = int64(len(journalHeader))
	s.checkpointBytes = int64(len(data) + 1)
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
	objects := make(map[string]Object)
	stack := append([]ObjectID(nil), roots...)
	for len(stack) != 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, ok := objects[id.key()]; ok {
			continue
		}
		object, ok := catalog.Object(id)
		if !ok || object.State != ObjectLive {
			return nil, fmt.Errorf("reachable TTC object %s is missing or not live", id)
		}
		objects[id.key()] = cloneObject(object)
		for _, edge := range object.Children {
			stack = append(stack, edge.Child)
		}
	}
	result := make([]Object, 0, len(objects))
	for _, object := range objects {
		result = append(result, object)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID.key() < result[j].ID.key() })
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

// JournalBytes returns the selected WAL generation's durable byte length.
// Callers use this only to schedule compaction; correctness never depends on
// the value being current after this call returns.
func (s *Store) JournalBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.journalEnd
}

// CheckpointDue reports whether the journal has grown enough to fold into a
// new checkpoint: past minimum, and past the size of the checkpoint it would
// replace. A checkpoint rewrites the whole graph, so the second bound spreads
// that cost over as many journal bytes as it writes; without it, importing a
// large memory image rewrote a checkpoint of hundreds of megabytes for every
// minimum's worth of journal. Replaying such a journal costs about what
// loading its checkpoint does.
func (s *Store) CheckpointDue(minimum int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.journalEnd >= max(minimum, s.checkpointBytes) {
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
