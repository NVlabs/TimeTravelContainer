// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A daemon's replay records (see idempotency.go) are kept in one
// write-ahead log a root, the mutation journal.
//
// A record was a file of its own, written as a temporary, synced, renamed,
// and its directory synced: two syncs, 5.4 ms on a disk whose sync takes
// 2.5 ms, and a request makes two records, one before it acts and one after.
// The journal is extended with zeros ahead of the records written into it,
// so that a record overwrites blocks the filesystem has allocated already:
// its data synced, it commits nothing of the filesystem's own, 1 ms on the
// same disk. Records written together share a sync.
//
// The journal is its header, then records, each its payload's length and
// CRC-32C, both little-endian, and its payload, a daemonMutationRecord in
// JSON; then zeros. A record cut short or damaged by a crash ends the log: it
// was never acknowledged, and is zeroed when the journal is opened. Only the
// daemon of the root writes its journal.

const (
	mutationJournalHeader = "TTC-MUTATIONS 1\n"
	// mutationJournalExtent is how much the journal is extended with zeros
	// at a time.
	mutationJournalExtent = 1 << 20
	// mutationJournalMaxRecord bounds a record's payload.
	mutationJournalMaxRecord = 1 << 30
)

var mutationJournalCRC = crc32.MakeTable(crc32.Castagnoli)

// mutationJournal is the journal of one root.
type mutationJournal struct {
	mu sync.Mutex
	// syncDone is signalled when a sync ends.
	syncDone *sync.Cond
	path     string
	file     *os.File
	// end is where the next record goes in the file, and zeroed, where the
	// zeros it was extended with end.
	end, zeroed int64
	// written counts the bytes of records written, in every file the
	// journal has had, and durable, those of them synced.
	written, durable int64
	// syncing is set while a sync is under way.
	syncing bool
	// broken is why the journal can no longer be written: a sync failed, and
	// what it was to make durable may or may not be. The journal is read
	// again from its file when next opened.
	broken error
	// entries locates each request's latest record. Records are read from
	// the file when asked for: the daemon holds only this much a request.
	entries map[string]mutationJournalEntry
}

// mutationJournalEntry locates a request's latest record in the journal.
type mutationJournalEntry struct {
	// offset is where the record's frame starts in the file, and size what
	// it takes.
	offset, size int64
	// written is the journal's written once the record was: the record is
	// durable when durable reaches it.
	written int64
	pending bool
	// created and completed order the records, for listing and compaction.
	created, completed time.Time
}

// mutationJournals holds the journal of each root this process opened.
var mutationJournals struct {
	mu       sync.Mutex
	journals map[string]*mutationJournal
}

func mutationJournalPath(root string) string {
	return filepath.Join(root, "daemon", "requests.log")
}

// openMutationJournal returns the journal of root, opening it, and taking in
// the records of an earlier daemon's request files, if this process has not.
// A journal whose file was removed or replaced meanwhile, or which a failed
// sync left unusable, is read again from its file.
func openMutationJournal(root string) (*mutationJournal, error) {
	if root == "" {
		return nil, errors.New("idempotent daemon mutation requires a data root")
	}
	path := mutationJournalPath(filepath.Clean(root))
	mutationJournals.mu.Lock()
	defer mutationJournals.mu.Unlock()
	if journal := mutationJournals.journals[path]; journal != nil {
		if journal.usable() {
			return journal, nil
		}
		journal.close()
		delete(mutationJournals.journals, path)
	}
	journal, err := loadMutationJournal(path)
	if err != nil {
		return nil, err
	}
	if err := journal.importRequestFiles(filepath.Join(filepath.Dir(path), "requests")); err != nil {
		journal.close()
		return nil, fmt.Errorf("importing daemon mutation records: %w", err)
	}
	if mutationJournals.journals == nil {
		mutationJournals.journals = make(map[string]*mutationJournal)
	}
	mutationJournals.journals[path] = journal
	return journal, nil
}

// usable reports whether j can still be written, and is still the file at
// its path.
func (j *mutationJournal) usable() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.broken == nil && j.isFileAtPath()
}

// close closes j's file; whoever still holds j can no longer use it.
func (j *mutationJournal) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for j.syncing {
		j.syncDone.Wait()
	}
	if j.broken == nil {
		j.broken = errors.New("the daemon mutation journal was closed")
	}
	_ = j.file.Close()
	j.syncDone.Broadcast()
}

// loadMutationJournal opens the journal at path, making it if there is none,
// and reads its records.
func loadMutationJournal(path string) (*mutationJournal, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Temporaries a crash left behind, of a journal made or rewritten.
	temps, err := filepath.Glob(filepath.Join(dir, ".requests.log.tmp-*"))
	if err != nil {
		return nil, err
	}
	for _, temp := range temps {
		if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		file, err = writeMutationJournalFile(path, newMutationJournalData(nil))
	}
	if err != nil {
		return nil, err
	}
	journal := &mutationJournal{path: path, file: file, entries: make(map[string]mutationJournalEntry)}
	journal.syncDone = sync.NewCond(&journal.mu)
	if err := journal.load(); err != nil {
		journal.file.Close()
		return nil, err
	}
	return journal, nil
}

// newMutationJournalData returns a journal holding the records of log, the
// frames of a journal less its header, and its first extent of zeros.
func newMutationJournalData(log []byte) []byte {
	data := make([]byte, len(mutationJournalHeader)+len(log)+mutationJournalExtent)
	copy(data, mutationJournalHeader)
	copy(data[len(mutationJournalHeader):], log)
	return data
}

// writeMutationJournalFile makes data the journal at path, whole or not at
// all, and returns it open.
func writeMutationJournalFile(path string, data []byte) (*os.File, error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".requests.log.tmp-")
	if err != nil {
		return nil, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return nil, err
	}
	if err := os.Rename(tempName, path); err != nil {
		temp.Close()
		return nil, err
	}
	if err := syncJournalDirectory(dir); err != nil {
		// Whichever file a crash leaves at path holds the records.
		temp.Close()
		return nil, err
	}
	return temp, nil
}

func (j *mutationJournal) load() error {
	info, err := j.file.Stat()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.NewSectionReader(j.file, 0, info.Size()))
	if err != nil {
		return err
	}
	if len(data) < len(mutationJournalHeader) || string(data[:len(mutationJournalHeader)]) != mutationJournalHeader {
		return fmt.Errorf("%s is not a daemon mutation journal", j.path)
	}
	offset := int64(len(mutationJournalHeader))
	for {
		record, size, ok := decodeMutationJournalRecord(data[offset:])
		if !ok {
			break
		}
		j.entries[record.RequestID] = newMutationJournalEntry(record, offset, size, 0)
		offset += size
	}
	j.end, j.zeroed = offset, info.Size()
	// What follows the last whole record is zeros, unless a crash cut a
	// record short there: clear it, so that no record written after it can
	// run into its remains.
	if tail := data[offset:]; !allZero(tail) {
		if _, err := j.file.WriteAt(make([]byte, len(tail)), offset); err != nil {
			return err
		}
	}
	// The records read may be in the page cache only, if the daemon that
	// wrote them died before syncing them.
	return syncFileData(j.file)
}

func newMutationJournalEntry(record daemonMutationRecord, offset, size, written int64) mutationJournalEntry {
	return mutationJournalEntry{
		offset: offset, size: size, written: written,
		pending: record.State == "pending",
		created: record.CreatedAt, completed: completedAt(record),
	}
}

func completedAt(record daemonMutationRecord) time.Time {
	if record.CompletedAt.IsZero() {
		return record.CreatedAt
	}
	return record.CompletedAt
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// decodeMutationJournalRecord decodes the record data begins with, and what
// its frame takes. It reports false at the journal's end: zeros, or a
// record cut short or damaged.
func decodeMutationJournalRecord(data []byte) (daemonMutationRecord, int64, bool) {
	if len(data) < 8 {
		return daemonMutationRecord{}, 0, false
	}
	length := binary.LittleEndian.Uint32(data)
	if length == 0 || length > mutationJournalMaxRecord || uint64(len(data)-8) < uint64(length) {
		return daemonMutationRecord{}, 0, false
	}
	payload := data[8 : 8+length]
	if crc32.Checksum(payload, mutationJournalCRC) != binary.LittleEndian.Uint32(data[4:]) {
		return daemonMutationRecord{}, 0, false
	}
	var record daemonMutationRecord
	if err := json.Unmarshal(payload, &record); err != nil || validateDaemonMutationRecord(record) != nil {
		return daemonMutationRecord{}, 0, false
	}
	return record, int64(8 + length), true
}

func encodeMutationJournalRecord(record daemonMutationRecord) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(payload) > mutationJournalMaxRecord {
		return nil, fmt.Errorf("daemon mutation record of %d bytes is too large", len(payload))
	}
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame, uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[4:], crc32.Checksum(payload, mutationJournalCRC))
	copy(frame[8:], payload)
	return frame, nil
}

// lookup returns the latest record of the request with id, once it is
// durable: no answer is given from a record a crash could still take back.
func (j *mutationJournal) lookup(id string) (daemonMutationRecord, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for {
		entry, ok := j.entries[id]
		if !ok {
			return daemonMutationRecord{}, false, nil
		}
		if j.durable < entry.written {
			// The wait may let the record be superseded, or moved.
			if err := j.waitDurableLocked(entry.written); err != nil {
				return daemonMutationRecord{}, false, err
			}
			continue
		}
		record, err := j.readLocked(entry)
		if err != nil {
			return daemonMutationRecord{}, false, err
		}
		return record, true, nil
	}
}

// readLocked reads the record entry locates.
//
// Preconditions: j.mu is locked.
func (j *mutationJournal) readLocked(entry mutationJournalEntry) (daemonMutationRecord, error) {
	if j.broken != nil {
		return daemonMutationRecord{}, fmt.Errorf("daemon mutation journal is unusable: %w", j.broken)
	}
	frame := make([]byte, entry.size)
	if _, err := j.file.ReadAt(frame, entry.offset); err != nil {
		return daemonMutationRecord{}, fmt.Errorf("reading daemon mutation journal: %w", err)
	}
	record, _, ok := decodeMutationJournalRecord(frame)
	if !ok {
		return daemonMutationRecord{}, fmt.Errorf("daemon mutation journal %s has a damaged record at %d", j.path, entry.offset)
	}
	return record, nil
}

// append makes record its request's latest, durably.
func (j *mutationJournal) append(record daemonMutationRecord) error {
	frame, err := encodeMutationJournalRecord(record)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	written, err := j.writeLocked(record, frame)
	if err != nil {
		return err
	}
	return j.waitDurableLocked(written)
}

// writeLocked writes frame, the frame of record, at the journal's end,
// extending the journal first if it would run past the zeros, and returns
// j.written after it. A write that fails leaves the end where it was: what
// it wrote, if anything, is overwritten by the next, or ends the log.
//
// Preconditions: j.mu is locked.
func (j *mutationJournal) writeLocked(record daemonMutationRecord, frame []byte) (int64, error) {
	if j.broken != nil {
		return 0, fmt.Errorf("daemon mutation journal is unusable: %w", j.broken)
	}
	if need := j.end + int64(len(frame)); need > j.zeroed {
		zeroed := j.zeroed + max(mutationJournalExtent, need-j.zeroed)
		if _, err := j.file.WriteAt(make([]byte, zeroed-j.zeroed), j.zeroed); err != nil {
			return 0, err
		}
		// The journal's size changed: this sync commits it.
		if err := j.file.Sync(); err != nil {
			j.broken = err
			return 0, err
		}
		j.zeroed = zeroed
	}
	if _, err := j.file.WriteAt(frame, j.end); err != nil {
		return 0, err
	}
	j.written += int64(len(frame))
	j.entries[record.RequestID] = newMutationJournalEntry(record, j.end, int64(len(frame)), j.written)
	j.end += int64(len(frame))
	return j.written, nil
}

// waitDurableLocked waits until j.durable reaches written, syncing the
// journal unless a sync is under way already, which the caller waits for and
// then looks again.
//
// Preconditions: j.mu is locked.
//
// +checklocks:j.mu
func (j *mutationJournal) waitDurableLocked(written int64) error {
	for j.durable < written {
		if j.broken != nil {
			return fmt.Errorf("syncing daemon mutation journal: %w", j.broken)
		}
		if j.syncing {
			j.syncDone.Wait()
			continue
		}
		j.syncing = true
		upTo := j.written
		file := j.file
		j.mu.Unlock()
		err := syncFileData(file)
		j.mu.Lock()
		j.syncing = false
		if err != nil {
			// The kernel may have persisted any part of what was written.
			j.broken = err
		} else if upTo > j.durable {
			j.durable = upTo
		}
		j.syncDone.Broadcast()
	}
	return nil
}

// list returns the journal's latest records in state, or all of them if
// state is empty, the newest first.
func (j *mutationJournal) list(state string) ([]daemonMutationRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries := make([]mutationJournalEntry, 0, len(j.entries))
	for _, entry := range j.entries {
		if state == "" || entry.pending == (state == "pending") {
			entries = append(entries, entry)
		}
	}
	records := make([]daemonMutationRecord, 0, len(entries))
	for _, entry := range entries {
		record, err := j.readLocked(entry)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, k int) bool {
		if records[i].CreatedAt.Equal(records[k].CreatedAt) {
			return records[i].RequestID < records[k].RequestID
		}
		return records[i].CreatedAt.After(records[k].CreatedAt)
	})
	return records, nil
}

// compact rewrites the journal with every pending record and the newest
// keepCompleted completed ones, when there are more, or when the records
// superseded since the last rewrite take more than the live ones and an
// extent.
func (j *mutationJournal) compact(keepCompleted int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	// A sync under way is of the file a rewrite replaces.
	for j.syncing {
		j.syncDone.Wait()
	}
	if j.broken != nil {
		return fmt.Errorf("daemon mutation journal is unusable: %w", j.broken)
	}
	type kept struct {
		id    string
		entry mutationJournalEntry
	}
	var keep, completed []kept
	var live int64
	for id, entry := range j.entries {
		if entry.pending {
			keep = append(keep, kept{id, entry})
			live += entry.size
		} else {
			completed = append(completed, kept{id, entry})
		}
	}
	sort.Slice(completed, func(i, k int) bool {
		a, b := completed[i].entry.completed, completed[k].entry.completed
		if a.Equal(b) {
			return completed[i].id > completed[k].id
		}
		return a.After(b)
	})
	dropped := len(completed) > keepCompleted
	if dropped {
		completed = completed[:keepCompleted]
	}
	for _, record := range completed {
		keep = append(keep, record)
		live += record.entry.size
	}
	if !dropped && j.end-int64(len(mutationJournalHeader)) <= 2*live+mutationJournalExtent {
		return nil
	}
	// The records keep their order: oldest first.
	sort.Slice(keep, func(i, k int) bool { return keep[i].entry.offset < keep[k].entry.offset })
	old := make([]byte, j.end)
	if _, err := j.file.ReadAt(old, 0); err != nil {
		return fmt.Errorf("reading daemon mutation journal: %w", err)
	}
	log := make([]byte, 0, live)
	entries := make(map[string]mutationJournalEntry, len(keep))
	for _, record := range keep {
		entry := record.entry
		log = append(log, old[entry.offset:entry.offset+entry.size]...)
		entry.offset = int64(len(mutationJournalHeader)+len(log)) - entry.size
		entry.written = j.written
		entries[record.id] = entry
	}
	data := newMutationJournalData(log)
	file, err := writeMutationJournalFile(j.path, data)
	if err != nil {
		// If the new file took the old one's place before the failure, the
		// old one can no longer be written: the journal is read again.
		if !j.isFileAtPath() {
			j.broken = err
		}
		return err
	}
	_ = j.file.Close()
	j.file = file
	j.end, j.zeroed = int64(len(mutationJournalHeader)+len(log)), int64(len(data))
	// Every record written is in the new file, synced.
	j.durable = j.written
	j.entries = entries
	return nil
}

// isFileAtPath reports whether j's file is the one at its path.
//
// Preconditions: j.mu is locked.
func (j *mutationJournal) isFileAtPath() bool {
	current, err := os.Stat(j.path)
	if err != nil {
		return false
	}
	opened, err := j.file.Stat()
	return err == nil && os.SameFile(current, opened)
}

// syncJournalDirectory syncs the directory at path.
func syncJournalDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// importRequestFiles takes in the records an earlier daemon kept as files in
// dir, one a request, then removes them: the journal holds them since.
func (j *mutationJournal) importRequestFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var imported []string
	j.mu.Lock()
	defer j.mu.Unlock()
	written := j.written
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(name, ".") && strings.Contains(name, ".json.tmp-") {
			// A temporary a crash left before its rename.
			imported = append(imported, filepath.Join(dir, name))
			continue
		}
		if filepath.Ext(name) != ".json" {
			continue
		}
		record, err := loadDaemonMutation(filepath.Join(dir, name))
		if err != nil {
			// A damaged record, which no request could have been answered
			// from; it stays where it is.
			continue
		}
		imported = append(imported, filepath.Join(dir, name))
		if existing, ok := j.entries[record.RequestID]; ok && (!existing.pending || record.State == "pending") {
			// An import a crash cut short took it in already.
			continue
		}
		frame, err := encodeMutationJournalRecord(record)
		if err != nil {
			return err
		}
		if written, err = j.writeLocked(record, frame); err != nil {
			return err
		}
	}
	if err := j.waitDurableLocked(written); err != nil {
		return err
	}
	for _, path := range imported {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// The directory goes once empty; a file no record came from stays.
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}
