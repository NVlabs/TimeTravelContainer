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
	"encoding/json"
	"math/rand"
	"testing"
)

// randomJournalOp returns an op with one selector set, as a graph writes
// them, or, now and then, several.
func randomJournalOp(random *rand.Rand) journalOp {
	var op journalOp
	id := ObjectID{Snapshot: SnapshotID(random.Uint64()), Kind: ObjectKind(random.Intn(256)), Ordinal: random.Uint64()}
	odd := func() string { return oddStrings[random.Intn(len(oddStrings))] }
	for count := 1 + random.Intn(2); count > 0; count-- {
		switch random.Intn(13) {
		case 0:
			op.PutSnapshot = &Snapshot{ID: SnapshotID(random.Uint64()), State: SnapshotState(odd()), NextOrdinal: random.Uint64()}
		case 1:
			op.DeleteSnapshot = SnapshotID(random.Uint64())
		case 2:
			objects := checkpointFixture(random, 3).Objects
			for _, object := range objects {
				op.PutObject = &object
				break
			}
		case 3:
			op.DeleteObject = odd()
		case 4:
			op.RootName, op.PutRoot = odd(), &id
		case 5:
			op.RootName, op.DeleteRoot = odd(), true
		case 6:
			op.PinName, op.PutPin = odd(), &id
		case 7:
			op.PinName, op.DeletePin = odd(), true
		case 8:
			next := SnapshotID(random.Uint64())
			op.SetNextSnapshot = &next
		case 9:
			op.FreezeSnapshot, op.UnfreezeSnapshot = SnapshotID(random.Uint64()), SnapshotID(random.Uint64())
		case 10:
			op.BeginHandoff = &SnapshotLedgerHandoff{Snapshot: 2, NextSequence: 5, Baseline: map[string]ObjectID{odd(): id}, Mutations: []SnapshotReferenceMutation{{Sequence: 4, Operation: odd(), Name: "root/b", Object: id, Retain: true}}}
		case 11:
			op.AppendHandoff = &SnapshotReferenceMutation{Sequence: random.Uint64(), Operation: odd(), Name: odd(), Object: id}
		case 12:
			mutation := &filesystemJournalMutation{Origin: SnapshotID(random.Uint64()), RootName: odd(), Result: id}
			for index := 0; index < random.Intn(4); index++ {
				var operation FilesystemOperation
				switch random.Intn(4) {
				case 0:
					inode := randomInodeRecord(random)
					inode.Target = odd()
					operation.PutInode = &inode
				case 1:
					operation.DeleteInode = random.Uint64()
				case 2:
					operation.PutEntry = &DirectoryEntryRecord{Parent: random.Uint64(), Name: odd(), Inode: random.Uint64()}
				case 3:
					operation.DeleteEntry = &DirectoryEntryRecord{Parent: random.Uint64(), Name: odd()}
				}
				mutation.Operations = append(mutation.Operations, operation)
			}
			switch random.Intn(3) {
			case 0:
				mutation.ReleasePins = []string{}
			case 1:
				mutation.ReleasePins = []string{odd(), "construct/x"}
			}
			op.ApplyFilesystem = mutation
		}
	}
	return op
}

// A journal record encodes as json.Marshal writes it, byte for byte.
func TestEncodeJournalRecord(t *testing.T) {
	random := rand.New(rand.NewSource(6))
	records := []journalRecord{{}, {Ops: []journalOp{}, References: []journalReference{}}, {Ops: []journalOp{{}}}}
	for round := 0; round < 500; round++ {
		record := journalRecord{Version: formatVersion, Sequence: random.Uint64()}
		for index := 0; index < random.Intn(5); index++ {
			record.Ops = append(record.Ops, randomJournalOp(random))
		}
		for index := 0; index < random.Intn(3); index++ {
			record.References = append(record.References, journalReference{Object: ObjectID{Snapshot: 1, Kind: 2, Ordinal: random.Uint64()}, Refs: random.Uint64(), State: ObjectState(oddStrings[random.Intn(len(oddStrings))])})
		}
		records = append(records, record)
	}
	for index := range records {
		want, err := json.Marshal(records[index])
		if err != nil {
			t.Fatal(err)
		}
		got, err := encodeJournalRecord(&records[index])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("record %d encodes (%v) as\n%.600s\nwant\n%.600s", index, err, got, want)
		}
	}
}
