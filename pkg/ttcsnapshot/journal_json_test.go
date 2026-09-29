// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// plainStrings are strings encoding/json writes as they are.
var plainStrings = []string{"", "plain", "filesystem/main", "construct/x-1"}

// randomJournalOp returns an op with one selector set, as a graph writes
// them, or, now and then, several, whose names are some of names.
func randomJournalOp(random *rand.Rand, names []string) journalOp {
	var op journalOp
	id := ObjectID{Snapshot: SnapshotID(random.Uint64()), Kind: ObjectKind(random.Intn(256)), Ordinal: random.Uint64()}
	odd := func() string { return names[random.Intn(len(names))] }
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
			record.Ops = append(record.Ops, randomJournalOp(random, oddStrings))
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

// checkJournalDecodesAlike fails unless decodeJournalRecord decodes data as
// json.Unmarshal does, and reports whether the canonical decoder took it.
func checkJournalDecodesAlike(t *testing.T, data []byte) bool {
	t.Helper()
	var want journalRecord
	wantErr := json.Unmarshal(data, &want)
	canonical, ok := decodeCanonicalJournalRecord(data)
	if ok && (wantErr != nil || !reflect.DeepEqual(canonical, want)) {
		t.Fatalf("the canonical decoder took what json.Unmarshal decodes otherwise (%v):\n%.600s", wantErr, data)
	}
	got, err := decodeJournalRecord(data)
	if (err == nil) != (wantErr == nil) || err == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded (%v) differently from json.Unmarshal (%v):\n%.600s", err, wantErr, data)
	}
	return ok
}

func randomJournalRecord(random *rand.Rand, names []string) journalRecord {
	record := journalRecord{Version: formatVersion, Sequence: random.Uint64()}
	for index := 0; index < random.Intn(5); index++ {
		record.Ops = append(record.Ops, randomJournalOp(random, names))
	}
	for index := 0; index < random.Intn(3); index++ {
		record.References = append(record.References, journalReference{Object: ObjectID{Snapshot: 1, Kind: 2, Ordinal: random.Uint64()}, Refs: random.Uint64(), State: ObjectLive})
	}
	return record
}

// A record as encodeJournalRecord writes it decodes, without encoding/json,
// to what json.Unmarshal decodes; any other form goes to encoding/json.
func TestDecodeJournalRecord(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	records := []journalRecord{{}, {Ops: []journalOp{}, References: []journalReference{}}, {Ops: []journalOp{{}}}}
	for round := 0; round < 500; round++ {
		records = append(records, randomJournalRecord(random, plainStrings))
	}
	for index := range records {
		data, err := encodeJournalRecord(&records[index])
		if err != nil {
			t.Fatal(err)
		}
		if !checkJournalDecodesAlike(t, data) {
			t.Fatalf("record %d: the canonical decoder refused encodeJournalRecord's record %s", index, data)
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, "", "\t"); err != nil {
			t.Fatal(err)
		}
		if !checkJournalDecodesAlike(t, indented.Bytes()) {
			t.Fatalf("record %d: the canonical decoder refused an indented record", index)
		}
	}
	for round := 0; round < 200; round++ {
		record := randomJournalRecord(random, oddStrings)
		data, err := encodeJournalRecord(&record)
		if err != nil {
			t.Fatal(err)
		}
		checkJournalDecodesAlike(t, data)
	}
	full := journalRecord{Version: formatVersion, Sequence: 3}
	for len(full.Ops) < 13 {
		full.Ops = append(full.Ops, randomJournalOp(random, plainStrings))
	}
	data, err := encodeJournalRecord(&full)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, variant := range []string{
		strings.Replace(text, `"Ops"`, `"ops"`, 1),
		strings.Replace(text, `"Version":`, `"Version":-`, 1),
		strings.Replace(text, `"Version":`, `"Unknown":1,"Version":`, 1),
		strings.Replace(text, `"Sequence":3`, `"Sequence":3.0`, 1),
		strings.Replace(text, `"DeleteRoot":false`, `"DeleteRoot":null`, 1),
		strings.Replace(text, `"DeleteObject":""`, `"DeleteObject":null`, 1),
		strings.Replace(text, `"PutObject":`, `"PutObject":null,"PutObject":`, 1),
		strings.Replace(text, `"PutSnapshot":null`, `"PutSnapshot":{"ID":1,"ID":2}`, 1),
		strings.Replace(text, `"PutSnapshot":null`, `"PutSnapshot":{}`, 1),
		strings.Replace(text, `"SetNextSnapshot":null`, `"SetNextSnapshot":7`, 1),
		strings.Replace(text, `"SetNextSnapshot":null`, `"SetNextSnapshot":"7"`, 1),
		strings.Replace(text, `"BeginHandoff":null`, `"BeginHandoff":{"Snapshot":1,"Baseline":{"x":{"Kind":1}}}`, 1),
		strings.Replace(text, `"AppendHandoff":null`, `"AppendHandoff":{"Name":"caf\u00e9"}`, 1),
		strings.Replace(text, `"ApplyFilesystem":null`, `"ApplyFilesystem":{"Operations":[{"PutInode":{"Number":1,"TargetBytes":"AAE="}}]}`, 1),
		strings.Replace(text, `"ApplyFilesystem":null`, `"ApplyFilesystem":{"Operations":[],"ReleasePins":[]}`, 1),
		strings.Replace(text, `"ApplyFilesystem":null`, `"ApplyFilesystem":{"Operations":[null]}`, 1),
		strings.Replace(text, `"References":`, `"References":[{"Object":{"Kind":1},"Refs":1,"State":"live"}],"x":`, 1),
		text + " x",
		"null",
		"{}",
		"[]",
		"",
	} {
		if variant == text {
			t.Fatalf("a variant is the record itself:\n%.300s", text)
		}
		checkJournalDecodesAlike(t, []byte(variant))
	}
}

// FuzzDecodeJournalRecord checks that whatever the canonical decoder takes,
// json.Unmarshal decodes alike.
func FuzzDecodeJournalRecord(f *testing.F) {
	random := rand.New(rand.NewSource(8))
	for index := 0; index < 4; index++ {
		record := randomJournalRecord(random, plainStrings)
		data, err := encodeJournalRecord(&record)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkJournalDecodesAlike(t, data)
	})
}
