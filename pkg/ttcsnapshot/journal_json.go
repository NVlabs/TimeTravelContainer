// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"encoding/json"
	"strconv"
)

// encodeJournalRecord encodes record as json.Marshal does, byte for byte,
// without reflection for what nearly every record holds: objects, snapshots
// and filesystem mutations. A graph journals a record for each mutation, and
// each object a deferred tree expands.
func encodeJournalRecord(record *journalRecord) ([]byte, error) {
	// An op's fields take 300 bytes, and its object's data a third more in
	// base64.
	size := 64 + 96*len(record.References)
	for index := range record.Ops {
		size += 320
		if object := record.Ops[index].PutObject; object != nil {
			size += 200 + len(object.Data)*4/3 + 64*len(object.Children)
		}
		if mutation := record.Ops[index].ApplyFilesystem; mutation != nil {
			size += 128 + 384*len(mutation.Operations)
		}
	}
	out := make([]byte, 0, size)
	out = append(out, `{"Version":`...)
	out = strconv.AppendInt(out, int64(record.Version), 10)
	out = append(out, `,"Sequence":`...)
	out = strconv.AppendUint(out, record.Sequence, 10)
	out = append(out, `,"Ops":`...)
	if record.Ops == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index := range record.Ops {
			if index != 0 {
				out = append(out, ',')
			}
			var err error
			if out, err = appendJournalOpJSON(out, &record.Ops[index]); err != nil {
				return nil, err
			}
		}
		out = append(out, ']')
	}
	out = append(out, `,"References":`...)
	if record.References == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index, reference := range record.References {
			if index != 0 {
				out = append(out, ',')
			}
			out = append(out, `{"Object":`...)
			out = appendObjectIDJSON(out, reference.Object)
			out = append(out, `,"Refs":`...)
			out = strconv.AppendUint(out, reference.Refs, 10)
			out = append(out, `,"State":`...)
			out = appendJSONString(out, string(reference.State))
			out = append(out, '}')
		}
		out = append(out, ']')
	}
	return append(out, '}'), nil
}

func appendJournalOpJSON(out []byte, op *journalOp) ([]byte, error) {
	out = append(out, `{"PutSnapshot":`...)
	if op.PutSnapshot == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, `{"ID":`...)
		out = strconv.AppendUint(out, uint64(op.PutSnapshot.ID), 10)
		out = append(out, `,"State":`...)
		out = appendJSONString(out, string(op.PutSnapshot.State))
		out = append(out, `,"NextOrdinal":`...)
		out = strconv.AppendUint(out, op.PutSnapshot.NextOrdinal, 10)
		out = append(out, '}')
	}
	out = append(out, `,"DeleteSnapshot":`...)
	out = strconv.AppendUint(out, uint64(op.DeleteSnapshot), 10)
	out = append(out, `,"PutObject":`...)
	if op.PutObject == nil {
		out = append(out, "null"...)
	} else {
		out = appendObjectJSON(out, *op.PutObject)
	}
	out = append(out, `,"DeleteObject":`...)
	out = appendJSONString(out, op.DeleteObject)
	out = append(out, `,"RootName":`...)
	out = appendJSONString(out, op.RootName)
	out = append(out, `,"PutRoot":`...)
	out = appendObjectIDPointerJSON(out, op.PutRoot)
	out = append(out, `,"DeleteRoot":`...)
	out = strconv.AppendBool(out, op.DeleteRoot)
	out = append(out, `,"PinName":`...)
	out = appendJSONString(out, op.PinName)
	out = append(out, `,"PutPin":`...)
	out = appendObjectIDPointerJSON(out, op.PutPin)
	out = append(out, `,"DeletePin":`...)
	out = strconv.AppendBool(out, op.DeletePin)
	out = append(out, `,"SetNextSnapshot":`...)
	if op.SetNextSnapshot == nil {
		out = append(out, "null"...)
	} else {
		out = strconv.AppendUint(out, uint64(*op.SetNextSnapshot), 10)
	}
	out = append(out, `,"FreezeSnapshot":`...)
	out = strconv.AppendUint(out, uint64(op.FreezeSnapshot), 10)
	out = append(out, `,"UnfreezeSnapshot":`...)
	out = strconv.AppendUint(out, uint64(op.UnfreezeSnapshot), 10)
	// Handoffs are rare: encoding/json encodes them.
	var err error
	out = append(out, `,"BeginHandoff":`...)
	if op.BeginHandoff == nil {
		out = append(out, "null"...)
	} else if out, err = appendMarshaledJSON(out, op.BeginHandoff); err != nil {
		return nil, err
	}
	out = append(out, `,"AppendHandoff":`...)
	if op.AppendHandoff == nil {
		out = append(out, "null"...)
	} else if out, err = appendMarshaledJSON(out, op.AppendHandoff); err != nil {
		return nil, err
	}
	out = append(out, `,"ApplyFilesystem":`...)
	if op.ApplyFilesystem == nil {
		out = append(out, "null"...)
	} else if out, err = appendFilesystemMutationJSON(out, op.ApplyFilesystem); err != nil {
		return nil, err
	}
	return append(out, '}'), nil
}

func appendMarshaledJSON(out []byte, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	return append(out, data...), err
}

func appendObjectIDPointerJSON(out []byte, id *ObjectID) []byte {
	if id == nil {
		return append(out, "null"...)
	}
	return appendObjectIDJSON(out, *id)
}

func appendFilesystemMutationJSON(out []byte, mutation *filesystemJournalMutation) ([]byte, error) {
	out = append(out, `{"Origin":`...)
	out = strconv.AppendUint(out, uint64(mutation.Origin), 10)
	out = append(out, `,"RootName":`...)
	out = appendJSONString(out, mutation.RootName)
	out = append(out, `,"Operations":`...)
	if mutation.Operations == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index := range mutation.Operations {
			operation := &mutation.Operations[index]
			if index != 0 {
				out = append(out, ',')
			}
			var err error
			out = append(out, `{"PutInode":`...)
			if operation.PutInode == nil {
				out = append(out, "null"...)
			} else if out, err = appendInodeRecordJSON(out, operation.PutInode); err != nil {
				return nil, err
			}
			out = append(out, `,"DeleteInode":`...)
			out = strconv.AppendUint(out, operation.DeleteInode, 10)
			out = append(out, `,"PutEntry":`...)
			if operation.PutEntry == nil {
				out = append(out, "null"...)
			} else if out, err = appendDirectoryEntryRecordJSON(out, operation.PutEntry); err != nil {
				return nil, err
			}
			out = append(out, `,"DeleteEntry":`...)
			if operation.DeleteEntry == nil {
				out = append(out, "null"...)
			} else if out, err = appendDirectoryEntryRecordJSON(out, operation.DeleteEntry); err != nil {
				return nil, err
			}
			out = append(out, '}')
		}
		out = append(out, ']')
	}
	out = append(out, `,"ReleasePins":`...)
	if mutation.ReleasePins == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index, pin := range mutation.ReleasePins {
			if index != 0 {
				out = append(out, ',')
			}
			out = appendJSONString(out, pin)
		}
		out = append(out, ']')
	}
	out = append(out, `,"Result":`...)
	out = appendObjectIDJSON(out, mutation.Result)
	return append(out, '}'), nil
}

// decodeJournalRecord decodes a journal record as json.Unmarshal does. A
// record as encodeJournalRecord writes one is decoded without reflection, but
// for its handoffs; anything else, however valid, is left to encoding/json
// whole, so that the two can never differ. A store opening a graph replays
// every record since its checkpoint: a guest's 100,000 new files are 100,000
// records.
func decodeJournalRecord(data []byte) (journalRecord, error) {
	if record, ok := decodeCanonicalJournalRecord(data); ok {
		return record, nil
	}
	var record journalRecord
	err := json.Unmarshal(data, &record)
	return record, err
}

// decodeCanonicalJournalRecord decodes a record of the canonical form. It
// reports false for anything else.
func decodeCanonicalJournalRecord(data []byte) (journalRecord, bool) {
	d := checkpointDecoder{data: data}
	var record journalRecord
	var seen [4]bool
	ok := d.members(func(name []byte) bool {
		var index int
		var ok bool
		switch string(name) {
		case "Version":
			var version uint64
			version, ok = d.integer(31)
			record.Version = int(version)
		case "Sequence":
			index = 1
			record.Sequence, ok = d.integer(64)
		case "Ops":
			index = 2
			ok = d.journalOps(&record.Ops)
		case "References":
			index = 3
			ok = d.journalReferences(&record.References)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
	d.space()
	return record, ok && d.pos == len(d.data)
}

// array consumes an array, or null, calling element with the decoder at each
// element, which element consumes. It reports whether the array was null.
func (d *checkpointDecoder) array(element func() bool) (null, ok bool) {
	if d.literal("null") {
		return true, true
	}
	if !d.consume('[') {
		return false, false
	}
	if d.consume(']') {
		return false, true
	}
	for {
		if !element() {
			return false, false
		}
		if d.consume(',') {
			continue
		}
		return false, d.consume(']')
	}
}

func (d *checkpointDecoder) journalOps(value *[]journalOp) bool {
	ops := []journalOp{}
	null, ok := d.array(func() bool {
		ops = append(ops, journalOp{})
		return d.journalOp(&ops[len(ops)-1])
	})
	if null {
		ops = nil
	}
	*value = ops
	return ok
}

func (d *checkpointDecoder) journalReferences(value *[]journalReference) bool {
	references := []journalReference{}
	null, ok := d.array(func() bool {
		var reference journalReference
		var seen [3]bool
		if !d.members(func(name []byte) bool {
			var index int
			var ok bool
			switch string(name) {
			case "Object":
				ok = d.objectID(&reference.Object)
			case "Refs":
				index = 1
				reference.Refs, ok = d.integer(64)
			case "State":
				index = 2
				var state []byte
				state, ok = d.plainString()
				reference.State = ObjectState(state)
			default:
				return false
			}
			if seen[index] {
				return false
			}
			seen[index] = true
			return ok
		}) {
			return false
		}
		references = append(references, reference)
		return true
	})
	if null {
		references = nil
	}
	*value = references
	return ok
}

// boolean consumes true or false.
func (d *checkpointDecoder) boolean(value *bool) bool {
	if d.literal("true") {
		*value = true
		return true
	}
	*value = false
	return d.literal("false")
}

// objectIDPointer consumes an object ID, or null, into a new ID.
func (d *checkpointDecoder) objectIDPointer(value **ObjectID) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	*value = new(ObjectID)
	return d.objectID(*value)
}

// unmarshaled consumes a value, or null, that encoding/json decodes into
// target, a pointer to a pointer it sets.
func (d *checkpointDecoder) unmarshaled(target any) bool {
	start := d.pos
	if !d.skip(0) {
		return false
	}
	return json.Unmarshal(d.data[start:d.pos], target) == nil
}

func (d *checkpointDecoder) journalOp(op *journalOp) bool {
	var seen [16]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		var value uint64
		switch string(name) {
		case "PutSnapshot":
			if d.literal("null") {
				op.PutSnapshot, ok = nil, true
				break
			}
			snapshot := new(Snapshot)
			var fields [3]bool
			ok = d.members(func(name []byte) bool {
				var field int
				var ok bool
				switch string(name) {
				case "ID":
					value, ok = d.integer(64)
					snapshot.ID = SnapshotID(value)
				case "State":
					field = 1
					var state []byte
					state, ok = d.plainString()
					snapshot.State = SnapshotState(state)
				case "NextOrdinal":
					field = 2
					snapshot.NextOrdinal, ok = d.integer(64)
				default:
					return false
				}
				if fields[field] {
					return false
				}
				fields[field] = true
				return ok
			})
			op.PutSnapshot = snapshot
		case "DeleteSnapshot":
			index = 1
			value, ok = d.integer(64)
			op.DeleteSnapshot = SnapshotID(value)
		case "PutObject":
			index = 2
			if d.literal("null") {
				op.PutObject, ok = nil, true
				break
			}
			op.PutObject = new(Object)
			ok = d.object(op.PutObject)
		case "DeleteObject":
			index = 3
			var key []byte
			key, ok = d.plainString()
			op.DeleteObject = string(key)
		case "RootName":
			index = 4
			var root []byte
			root, ok = d.plainString()
			op.RootName = string(root)
		case "PutRoot":
			index = 5
			ok = d.objectIDPointer(&op.PutRoot)
		case "DeleteRoot":
			index = 6
			ok = d.boolean(&op.DeleteRoot)
		case "PinName":
			index = 7
			var pin []byte
			pin, ok = d.plainString()
			op.PinName = string(pin)
		case "PutPin":
			index = 8
			ok = d.objectIDPointer(&op.PutPin)
		case "DeletePin":
			index = 9
			ok = d.boolean(&op.DeletePin)
		case "SetNextSnapshot":
			index = 10
			if d.literal("null") {
				op.SetNextSnapshot, ok = nil, true
				break
			}
			value, ok = d.integer(64)
			next := SnapshotID(value)
			op.SetNextSnapshot = &next
		case "FreezeSnapshot":
			index = 11
			value, ok = d.integer(64)
			op.FreezeSnapshot = SnapshotID(value)
		case "UnfreezeSnapshot":
			index = 12
			value, ok = d.integer(64)
			op.UnfreezeSnapshot = SnapshotID(value)
		case "BeginHandoff":
			// Handoffs are rare: encoding/json decodes them.
			index = 13
			ok = d.unmarshaled(&op.BeginHandoff)
		case "AppendHandoff":
			index = 14
			ok = d.unmarshaled(&op.AppendHandoff)
		case "ApplyFilesystem":
			index = 15
			if d.literal("null") {
				op.ApplyFilesystem, ok = nil, true
				break
			}
			op.ApplyFilesystem = new(filesystemJournalMutation)
			ok = d.filesystemMutation(op.ApplyFilesystem)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
}

func (d *checkpointDecoder) filesystemMutation(mutation *filesystemJournalMutation) bool {
	var seen [5]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		switch string(name) {
		case "Origin":
			var origin uint64
			origin, ok = d.integer(64)
			mutation.Origin = SnapshotID(origin)
		case "RootName":
			index = 1
			var root []byte
			root, ok = d.plainString()
			mutation.RootName = string(root)
		case "Operations":
			index = 2
			operations := []FilesystemOperation{}
			var null bool
			null, ok = d.array(func() bool {
				operations = append(operations, FilesystemOperation{})
				return d.filesystemOperation(&operations[len(operations)-1])
			})
			if null {
				operations = nil
			}
			mutation.Operations = operations
		case "ReleasePins":
			index = 3
			pins := []string{}
			var null bool
			null, ok = d.array(func() bool {
				pin, ok := d.plainString()
				pins = append(pins, string(pin))
				return ok
			})
			if null {
				pins = nil
			}
			mutation.ReleasePins = pins
		case "Result":
			index = 4
			ok = d.objectID(&mutation.Result)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
}

func (d *checkpointDecoder) filesystemOperation(operation *FilesystemOperation) bool {
	var seen [4]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		switch string(name) {
		case "PutInode":
			if d.literal("null") {
				operation.PutInode, ok = nil, true
				break
			}
			operation.PutInode = new(InodeRecord)
			ok = d.inodeRecord(operation.PutInode)
		case "DeleteInode":
			index = 1
			operation.DeleteInode, ok = d.integer(64)
		case "PutEntry":
			index = 2
			if d.literal("null") {
				operation.PutEntry, ok = nil, true
				break
			}
			operation.PutEntry = new(DirectoryEntryRecord)
			ok = d.directoryEntryRecord(operation.PutEntry)
		case "DeleteEntry":
			index = 3
			if d.literal("null") {
				operation.DeleteEntry, ok = nil, true
				break
			}
			operation.DeleteEntry = new(DirectoryEntryRecord)
			ok = d.directoryEntryRecord(operation.DeleteEntry)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
}
