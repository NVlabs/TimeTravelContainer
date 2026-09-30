// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// A graph of legacyFormatVersion kept its checkpoint and journal as JSON.
// Opened, it is converted: its generation is loaded and checkpointed anew.
const (
	legacyFormatVersion = 5
	legacyJournalHeader = "TTCSGJ5\n"
)

// legacyCheckpoint is a checkpoint of legacyFormatVersion, its objects named
// by their keys.
type legacyCheckpoint struct {
	Version      int
	Sequence     uint64
	NextSnapshot SnapshotID
	Snapshots    map[string]Snapshot
	Objects      map[string]Object
	Roots        map[string]ObjectID
	Pins         map[string]ObjectID
	Frozen       map[string]bool
	Handoffs     map[string]SnapshotLedgerHandoff
}

type legacyJournalRecord struct {
	Version    int
	Sequence   uint64
	Ops        []legacyJournalOp
	References []journalReference
}

// legacyJournalOp is a journalOp of legacyFormatVersion, which named an
// object it deleted by its key.
type legacyJournalOp struct {
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

// state returns the graph state checkpoint holds, each object under the ID
// its key names, which must be its own.
func (checkpoint *legacyCheckpoint) state() (graphState, error) {
	if checkpoint.Version != legacyFormatVersion {
		return graphState{}, fmt.Errorf("checkpoint of format %d", checkpoint.Version)
	}
	state := graphState{
		Version: formatVersion, Sequence: checkpoint.Sequence, NextSnapshot: checkpoint.NextSnapshot,
		Snapshots: checkpoint.Snapshots, Roots: checkpoint.Roots, Pins: checkpoint.Pins,
		Frozen: checkpoint.Frozen, Handoffs: checkpoint.Handoffs,
	}
	if checkpoint.Objects != nil {
		state.Objects = make(map[ObjectID]Object, len(checkpoint.Objects))
		for key, object := range checkpoint.Objects {
			id, err := parseObjectKey(key)
			if err != nil {
				return graphState{}, err
			}
			if id != object.ID {
				return graphState{}, fmt.Errorf("object key %q does not match %s", key, object.ID)
			}
			state.Objects[id] = object
		}
	}
	return state, validateGraphStateShape(&state)
}

func (legacy *legacyJournalRecord) record() (journalRecord, error) {
	if legacy.Version != legacyFormatVersion {
		return journalRecord{}, fmt.Errorf("journal record of format %d", legacy.Version)
	}
	record := journalRecord{Sequence: legacy.Sequence, References: legacy.References}
	if legacy.Ops != nil {
		record.Ops = make([]journalOp, 0, len(legacy.Ops))
	}
	for _, op := range legacy.Ops {
		converted := journalOp{
			PutSnapshot: op.PutSnapshot, DeleteSnapshot: op.DeleteSnapshot, PutObject: op.PutObject,
			RootName: op.RootName, PutRoot: op.PutRoot, DeleteRoot: op.DeleteRoot,
			PinName: op.PinName, PutPin: op.PutPin, DeletePin: op.DeletePin,
			SetNextSnapshot: op.SetNextSnapshot, FreezeSnapshot: op.FreezeSnapshot, UnfreezeSnapshot: op.UnfreezeSnapshot,
			BeginHandoff: op.BeginHandoff, AppendHandoff: op.AppendHandoff, ApplyFilesystem: op.ApplyFilesystem,
		}
		if op.DeleteObject != "" {
			id, err := parseObjectKey(op.DeleteObject)
			if err != nil {
				return journalRecord{}, err
			}
			converted.DeleteObject = id
		}
		record.Ops = append(record.Ops, converted)
	}
	return record, nil
}

// loadLegacyGeneration returns the state of the generation of
// legacyFormatVersion head names: its checkpoint, and the records its journal
// holds whole.
func (s *Store) loadLegacyGeneration(head graphHead) (graphState, error) {
	data, err := s.readFile(graphCheckpointName(head.Generation))
	if err != nil {
		return graphState{}, err
	}
	var checkpoint legacyCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return graphState{}, fmt.Errorf("decoding checkpoint: %w", err)
	}
	state, err := checkpoint.state()
	if err != nil {
		return graphState{}, fmt.Errorf("invalid checkpoint: %w", err)
	}
	journal, err := s.readFile(graphJournalName(head.Generation))
	if err != nil {
		return graphState{}, err
	}
	if !bytes.HasPrefix(journal, []byte(legacyJournalHeader)) {
		return graphState{}, errors.New("invalid journal header")
	}
	// A record ended by the journal's end was being appended by a writer that
	// crashed.
	for offset := len(legacyJournalHeader); offset+4 <= len(journal); {
		length := binary.BigEndian.Uint32(journal[offset:])
		if length == 0 || length > maxJournalRecordSize {
			return graphState{}, fmt.Errorf("invalid journal record length %d at %d", length, offset)
		}
		end := offset + 4 + int(length)
		if end > len(journal) {
			break
		}
		var legacy legacyJournalRecord
		if err := json.Unmarshal(journal[offset+4:end], &legacy); err != nil {
			return graphState{}, fmt.Errorf("decoding journal record at %d: %w", offset, err)
		}
		record, err := legacy.record()
		if err == nil {
			err = validateJournalRecord(record)
		}
		if err != nil {
			return graphState{}, fmt.Errorf("journal record at %d: %w", offset, err)
		}
		if record.Sequence > state.Sequence {
			if record.Sequence != state.Sequence+1 {
				return graphState{}, fmt.Errorf("journal sequence %d follows %d", record.Sequence, state.Sequence)
			}
			if err := applyJournalRecord(&state, record); err != nil {
				return graphState{}, fmt.Errorf("applying journal sequence %d: %w", record.Sequence, err)
			}
		}
		offset = end
	}
	if err := validateGraphState(&state); err != nil {
		return graphState{}, err
	}
	return state, nil
}

// convertLegacyGeneration converts the generation of legacyFormatVersion head
// names: it checkpoints its state in a generation of formatVersion. Old
// processes that have the graph open cannot read it after.
func (s *Store) convertLegacyGeneration(head graphHead) error {
	state, err := s.loadLegacyGeneration(head)
	if err != nil {
		return fmt.Errorf("converting TTC snapshot graph of format %d: %w", legacyFormatVersion, err)
	}
	s.state, s.generation = state, head.Generation
	return s.compactGenerationLocked()
}
