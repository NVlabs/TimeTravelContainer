// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The authority's state lives in the shared root, in the directory of its
// epoch: a snapshot of the state as of one command, and the log of the
// commands after it, in segments. The holder appends each batch of commands
// and syncs it, and acknowledges it only if it still holds the authority
// (AuthorityHolder.Held), so every command it acknowledged is in the log
// before a successor reads the log to make its own epoch's snapshot. A holder
// that stopped in the middle of a write leaves a torn record at the end of its
// last segment, where reading stops.
//
// A snapshot is authoritySnapshotMagic, the epoch, the index of the last
// command it holds, the length and CRC-32C of the encoded state, and the
// state. A segment, log-<index of its first record>, begins with
// authorityLogMagic, the epoch and the index of its first record; each record
// is the length of its command, the CRC-32C of its index and command, its
// index, and the command.

const (
	authoritySnapshotMagic = "TTCASNP1"
	authorityLogMagic      = "TTCALOG1"
	authoritySnapshotName  = "snapshot"
	authoritySegmentPrefix = "log-"
	// authoritySegmentHeader is the length of a segment's header.
	authoritySegmentHeader = len(authorityLogMagic) + 16
	// authorityRecordHeader is the length of a record's header.
	authorityRecordHeader = 16
	// maxAuthorityCommand bounds a command, and so a record.
	maxAuthorityCommand = 16 << 20
	// maxAuthorityStateBytes bounds the encoded state, as a snapshot holds it
	// and a read sends it.
	maxAuthorityStateBytes = 512 << 20
)

// A segment past either of these gives way to a snapshot and a new segment.
var (
	authoritySegmentRecords       = 8192
	authoritySegmentBytes   int64 = 32 << 20
)

var authorityCRC = crc32.MakeTable(crc32.Castagnoli)

// errOldAuthorityFormat is the error of an epoch whose state an earlier TTC
// kept in a Raft log.
var errOldAuthorityFormat = errors.New("the cluster's authority was kept in a Raft log by an earlier TTC; start a new cluster")

// authoritySegment is one segment of an epoch's log.
type authoritySegment struct {
	path  string
	first uint64
}

// authoritySegments returns the segments in dir, in order.
func authoritySegments(dir string) ([]authoritySegment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var segments []authoritySegment
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, authoritySegmentPrefix) {
			continue
		}
		first, err := strconv.ParseUint(strings.TrimPrefix(name, authoritySegmentPrefix), 10, 64)
		if err != nil {
			continue
		}
		segments = append(segments, authoritySegment{path: filepath.Join(dir, name), first: first})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].first < segments[j].first })
	return segments, nil
}

func authoritySegmentPath(dir string, first uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%s%020d", authoritySegmentPrefix, first))
}

// encodeAuthoritySnapshot encodes state, as of the command at index, for
// epoch.
func encodeAuthoritySnapshot(epoch, index uint64, state []byte) []byte {
	out := make([]byte, 0, len(authoritySnapshotMagic)+28+len(state))
	out = append(out, authoritySnapshotMagic...)
	out = binary.BigEndian.AppendUint64(out, epoch)
	out = binary.BigEndian.AppendUint64(out, index)
	out = binary.BigEndian.AppendUint64(out, uint64(len(state)))
	out = binary.BigEndian.AppendUint32(out, crc32.Checksum(state, authorityCRC))
	return append(out, state...)
}

// readAuthoritySnapshot returns the snapshot in dir: the index of the last
// command it holds, and the state.
func readAuthoritySnapshot(dir string) (uint64, *AuthorityState, error) {
	data, err := os.ReadFile(filepath.Join(dir, authoritySnapshotName))
	if errors.Is(err, fs.ErrNotExist) {
		if _, raftErr := os.Stat(filepath.Join(dir, "raft")); raftErr == nil {
			return 0, nil, errOldAuthorityFormat
		}
	}
	if err != nil {
		return 0, nil, fmt.Errorf("reading the authority's snapshot: %w", err)
	}
	header := len(authoritySnapshotMagic) + 28
	if len(data) < header || string(data[:len(authoritySnapshotMagic)]) != authoritySnapshotMagic {
		return 0, nil, errors.New("the authority's snapshot is not one")
	}
	fields := data[len(authoritySnapshotMagic):]
	index := binary.BigEndian.Uint64(fields[8:])
	length := binary.BigEndian.Uint64(fields[16:])
	sum := binary.BigEndian.Uint32(fields[24:])
	state := data[header:]
	if length > maxAuthorityStateBytes || uint64(len(state)) != length || crc32.Checksum(state, authorityCRC) != sum {
		return 0, nil, errors.New("the authority's snapshot is damaged")
	}
	restored, err := RestoreAuthorityState(state)
	if err != nil {
		return 0, nil, fmt.Errorf("decoding the authority's snapshot: %w", err)
	}
	return index, restored, nil
}

// writeAuthoritySnapshot replaces the snapshot in dir with state, as of the
// command at index, for epoch.
func writeAuthoritySnapshot(dir string, epoch, index uint64, state []byte) error {
	temporary := filepath.Join(dir, authoritySnapshotName+".tmp")
	if err := writeFileSynced(temporary, encodeAuthoritySnapshot(epoch, index, state)); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(dir, authoritySnapshotName)); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeFileSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// appendAuthorityRecord appends the record of command, at index, to out.
func appendAuthorityRecord(out []byte, index uint64, command []byte) []byte {
	var indexBytes [8]byte
	binary.BigEndian.PutUint64(indexBytes[:], index)
	sum := crc32.Update(crc32.Checksum(indexBytes[:], authorityCRC), authorityCRC, command)
	out = binary.BigEndian.AppendUint32(out, uint32(len(command)))
	out = binary.BigEndian.AppendUint32(out, sum)
	out = append(out, indexBytes[:]...)
	return append(out, command...)
}

// authorityTail is where the last segment of a log ends: its path, the
// length of its valid prefix, and its records.
type authorityTail struct {
	path    string
	valid   int64
	records int
	next    uint64
}

// replayAuthorityLog applies to state the commands in dir's segments after
// index, in order, and returns the last segment's tail. A torn or damaged
// record ends the last segment; in an earlier one, it is an error.
func replayAuthorityLog(dir string, epoch, index uint64, apply func(uint64, []byte) error) (authorityTail, error) {
	segments, err := authoritySegments(dir)
	if err != nil {
		return authorityTail{}, err
	}
	tail := authorityTail{next: index + 1}
	for i, segment := range segments {
		last := i == len(segments)-1
		if !last && segments[i+1].first <= index+1 {
			// Every record of this segment is in the snapshot.
			continue
		}
		data, err := os.ReadFile(segment.path)
		if err != nil {
			return authorityTail{}, err
		}
		if len(data) < authoritySegmentHeader || string(data[:len(authorityLogMagic)]) != authorityLogMagic ||
			binary.BigEndian.Uint64(data[len(authorityLogMagic):]) != epoch ||
			binary.BigEndian.Uint64(data[len(authorityLogMagic)+8:]) != segment.first {
			if last && len(data) < authoritySegmentHeader {
				// A segment whose header its writer did not finish.
				tail = authorityTail{path: segment.path, next: tail.next}
				break
			}
			return authorityTail{}, fmt.Errorf("authority log segment %s is not one of epoch %d", filepath.Base(segment.path), epoch)
		}
		if segment.first > tail.next {
			return authorityTail{}, fmt.Errorf("the authority log lacks commands %d to %d", tail.next, segment.first-1)
		}
		offset := authoritySegmentHeader
		records := 0
		torn := false
		for offset < len(data) {
			if len(data)-offset < authorityRecordHeader {
				torn = true
				break
			}
			length := int(binary.BigEndian.Uint32(data[offset:]))
			sum := binary.BigEndian.Uint32(data[offset+4:])
			recordIndex := binary.BigEndian.Uint64(data[offset+8:])
			end := offset + authorityRecordHeader + length
			if length > maxAuthorityCommand || end > len(data) ||
				crc32.Checksum(data[offset+8:end], authorityCRC) != sum {
				torn = true
				break
			}
			command := data[offset+authorityRecordHeader : end]
			switch {
			case recordIndex < tail.next:
				// In the snapshot already.
			case recordIndex == tail.next:
				if err := apply(recordIndex, command); err != nil {
					return authorityTail{}, fmt.Errorf("applying authority command %d: %w", recordIndex, err)
				}
				tail.next++
			default:
				return authorityTail{}, fmt.Errorf("the authority log lacks commands %d to %d", tail.next, recordIndex-1)
			}
			records++
			offset = end
		}
		if torn && !last {
			return authorityTail{}, fmt.Errorf("authority log segment %s is damaged at %d", filepath.Base(segment.path), offset)
		}
		tail.path, tail.valid, tail.records = segment.path, int64(offset), records
	}
	return tail, nil
}

// loadAuthorityState returns the state in dir, the snapshot's and the log's
// after it, the index of its last command, and the log's tail.
func loadAuthorityState(dir string, epoch uint64) (*AuthorityState, uint64, authorityTail, error) {
	index, state, err := readAuthoritySnapshot(dir)
	if err != nil {
		return nil, 0, authorityTail{}, err
	}
	tail, err := replayAuthorityLog(dir, epoch, index, func(_ uint64, data []byte) error {
		command, err := decodeAuthorityCommand(data)
		if err != nil {
			return err
		}
		state.Apply(command)
		return nil
	})
	if err != nil {
		return nil, 0, authorityTail{}, err
	}
	return state, tail.next - 1, tail, nil
}

// authorityLog appends to the log of an epoch whose authority this node
// holds.
type authorityLog struct {
	dir   string
	epoch uint64
	file  *os.File
	// next is the index of the next command, offset where its record goes,
	// and records and bytes what the current segment holds.
	next    uint64
	offset  int64
	records int
	bytes   int64
}

// openAuthorityLog opens dir's log for appending after its last valid
// record, as the holder of epoch, and returns it with the state it holds.
func openAuthorityLog(dir string, epoch uint64) (*authorityLog, *AuthorityState, error) {
	state, last, tail, err := loadAuthorityState(dir, epoch)
	if err != nil {
		return nil, nil, err
	}
	log := &authorityLog{dir: dir, epoch: epoch, next: last + 1}
	if tail.path == "" {
		if err := log.startSegment(); err != nil {
			return nil, nil, err
		}
		return log, state, nil
	}
	if tail.valid < int64(authoritySegmentHeader) {
		// A segment whose header its writer did not finish holds nothing.
		if err := os.Remove(tail.path); err != nil {
			return nil, nil, err
		}
		if err := log.startSegment(); err != nil {
			return nil, nil, err
		}
		return log, state, nil
	}
	file, err := os.OpenFile(tail.path, os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err == nil && info.Size() != tail.valid {
		// Drop the torn record a holder left.
		err = file.Truncate(tail.valid)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("opening the authority log: %w", err)
	}
	log.file, log.offset, log.records, log.bytes = file, tail.valid, tail.records, tail.valid
	return log, state, nil
}

func authoritySegmentHeaderBytes(epoch, first uint64) []byte {
	out := make([]byte, 0, authoritySegmentHeader)
	out = append(out, authorityLogMagic...)
	out = binary.BigEndian.AppendUint64(out, epoch)
	return binary.BigEndian.AppendUint64(out, first)
}

// startSegment begins a segment whose first record is the next command's.
func (l *authorityLog) startSegment() error {
	path := authoritySegmentPath(l.dir, l.next)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(authoritySegmentHeaderBytes(l.epoch, l.next)); err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = syncDir(l.dir)
	}
	if err != nil {
		file.Close()
		return fmt.Errorf("starting an authority log segment: %w", err)
	}
	if l.file != nil {
		_ = l.file.Close()
	}
	l.file, l.offset, l.records, l.bytes = file, int64(authoritySegmentHeader), 0, int64(authoritySegmentHeader)
	return nil
}

// append appends commands, the next ones, and syncs them. It returns the
// index of the first.
func (l *authorityLog) append(commands [][]byte) (uint64, error) {
	first := l.next
	size := 0
	for _, command := range commands {
		if len(command) > maxAuthorityCommand {
			return 0, fmt.Errorf("authority command of %d bytes exceeds %d", len(command), maxAuthorityCommand)
		}
		size += authorityRecordHeader + len(command)
	}
	records := make([]byte, 0, size)
	for i, command := range commands {
		records = appendAuthorityRecord(records, first+uint64(i), command)
	}
	if _, err := l.file.WriteAt(records, l.offset); err != nil {
		return 0, err
	}
	if err := l.file.Sync(); err != nil {
		return 0, err
	}
	l.offset += int64(len(records))
	l.bytes += int64(len(records))
	l.records += len(commands)
	l.next += uint64(len(commands))
	return first, nil
}

// full reports whether the current segment should give way to a snapshot.
func (l *authorityLog) full() bool {
	return l.records >= authoritySegmentRecords || l.bytes >= authoritySegmentBytes
}

// compactAuthorityLog writes state, as of the command at index, as the
// snapshot in dir, and removes the segments the snapshot holds: those that
// begin at or before index, which the holder began a segment after.
func compactAuthorityLog(dir string, epoch, index uint64, state []byte) error {
	if err := writeAuthoritySnapshot(dir, epoch, index, state); err != nil {
		return fmt.Errorf("writing the authority's snapshot: %w", err)
	}
	segments, err := authoritySegments(dir)
	if err != nil {
		return err
	}
	var result error
	for _, segment := range segments {
		if segment.first <= index {
			if err := os.Remove(segment.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func (l *authorityLog) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// prepareAuthorityEpoch makes dir the state of epoch: from the state in
// source, a previous epoch's directory, or, without one, a new cluster's.
func prepareAuthorityEpoch(dir string, epoch uint64, source string, sourceEpoch uint64) error {
	state := NewAuthorityState()
	var index uint64
	if source != "" {
		var err error
		if state, index, _, err = loadAuthorityState(source, sourceEpoch); err != nil {
			return fmt.Errorf("reading the authority of epoch %d: %w", sourceEpoch, err)
		}
	}
	encoded, err := state.SnapshotBytes()
	if err != nil {
		return err
	}
	return writeAuthoritySnapshot(dir, epoch, index, encoded)
}
