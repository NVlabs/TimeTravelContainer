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
