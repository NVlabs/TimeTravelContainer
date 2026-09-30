// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// checkpointFixture returns a graph state with every kind of field a
// checkpoint holds, some empty and some absent.
func checkpointFixture(random *rand.Rand, objects int) graphState {
	state := graphState{
		Version: formatVersion, Sequence: random.Uint64(), NextSnapshot: 4,
		Snapshots: map[SnapshotID]Snapshot{
			1: {ID: 1, State: SnapshotWorking, NextOrdinal: uint64(objects) + 10},
			2: {ID: 2, State: SnapshotSealedVolatile, NextOrdinal: 3},
		},
		Objects: make(map[ObjectID]Object), Roots: make(map[string]ObjectID), Pins: make(map[string]ObjectID),
		Frozen:   map[SnapshotID]bool{2: true},
		Handoffs: map[SnapshotID]SnapshotLedgerHandoff{2: {Snapshot: 2, NextSequence: 5, Frozen: true, Baseline: map[string]ObjectID{"root/a": {Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 1}}, Mutations: []SnapshotReferenceMutation{{Sequence: 4, Operation: "op", Name: "root/b", Object: ObjectID{Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 2}, Retain: true}}}},
	}
	for ordinal := 1; ordinal <= objects; ordinal++ {
		id := ObjectID{Snapshot: 1, Kind: ObjectKind(1 + random.Intn(8)), Ordinal: uint64(ordinal)}
		object := Object{ID: id, State: ObjectLive, Refs: uint64(random.Intn(3)), BulkLength: uint64(random.Intn(2)) << 40}
		switch random.Intn(4) {
		case 0:
			object.Data = []byte{}
		case 1:
			object.Data = make([]byte, random.Intn(300))
			random.Read(object.Data)
		case 2:
			object.Data = []byte(`{"Number":7,"Type":"regular","Target":"<&>"}`)
		}
		switch random.Intn(3) {
		case 0:
			object.Children = []Edge{}
		case 1:
			for slot := 0; slot < 1+random.Intn(4); slot++ {
				object.Children = append(object.Children, Edge{Slot: uint16(random.Intn(1 << 16)), Child: ObjectID{Snapshot: SnapshotID(random.Uint64()), Kind: ObjectKind(random.Intn(256)), Ordinal: random.Uint64()}})
			}
		}
		if random.Intn(5) == 0 {
			object.SourceCatalog, object.SourceObject, object.Deferred = fmt.Sprintf("catalog-%d", random.Intn(3)), ObjectID{Snapshot: 9, Kind: ObjectKindFile, Ordinal: uint64(ordinal)}, true
		}
		if object.Refs == 0 {
			object.State = ObjectReclaiming
		}
		state.Objects[id] = object
	}
	state.Roots["branch/main"] = ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 1}
	state.Pins["construct/x"] = ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 2}
	return state
}

// journalFixture returns a record with each kind of operation, and fields
// empty and absent.
func journalFixture(random *rand.Rand) journalRecord {
	id := func(ordinal uint64) ObjectID { return ObjectID{Snapshot: 3, Kind: ObjectKindInode, Ordinal: ordinal} }
	snapshot := Snapshot{ID: 3, State: SnapshotShared, NextOrdinal: 9}
	next := SnapshotID(12)
	root, pin := id(1), id(2)
	object := Object{ID: id(3), State: ObjectLive, Refs: 2, Data: []byte("inode"), Children: []Edge{{Slot: 1, Child: id(4)}}, SourceCatalog: "catalog", SourceObject: id(7)}
	empty := Object{ID: id(5), State: ObjectReclaiming, Data: []byte{}, Children: []Edge{}}
	inode := InodeRecord{
		Number: 9, Type: "regular", Mode: 0644, UID: 1, GID: 2, Nlink: 1, Size: -1, File: ObjectID{Snapshot: 3, Kind: ObjectKindFile, Ordinal: 8},
		Target: "t\x00\xff", DeviceMajor: 3, DeviceMinor: 4, AtimeNsec: -5, BtimeNsec: 6, CtimeNsec: random.Int63(), MtimeNsec: -random.Int63(),
		Xattrs: map[string][]byte{"user.a": {1}, "user.empty": {}, "user.nil": nil},
	}
	bare := InodeRecord{Number: 10, Type: "directory", Directory: ObjectID{Snapshot: 3, Kind: ObjectKindDirectoryNode, Ordinal: 9}, Xattrs: map[string][]byte{}}
	return journalRecord{
		Sequence: random.Uint64(),
		Ops: []journalOp{
			{PutSnapshot: &snapshot}, {DeleteSnapshot: 2}, {PutObject: &object}, {PutObject: &empty}, {DeleteObject: id(6)},
			{RootName: "branch/main", PutRoot: &root}, {RootName: "branch/old", DeleteRoot: true},
			{PinName: "construct/x", PutPin: &pin}, {PinName: "construct/y", DeletePin: true},
			{SetNextSnapshot: &next}, {FreezeSnapshot: 3}, {UnfreezeSnapshot: 3},
			{BeginHandoff: &SnapshotLedgerHandoff{Snapshot: 3, NextSequence: 1, Baseline: map[string]ObjectID{"root/a": root}}},
			{BeginHandoff: &SnapshotLedgerHandoff{Snapshot: 4, Frozen: true, Baseline: map[string]ObjectID{}, Mutations: []SnapshotReferenceMutation{}}},
			{AppendHandoff: &SnapshotReferenceMutation{Sequence: 1, Operation: "op", Name: "pin/x", Object: pin, Retain: true}},
			{ApplyFilesystem: &filesystemJournalMutation{
				Origin: 3, RootName: "branch/main",
				Operations: []FilesystemOperation{
					{PutInode: &inode}, {PutInode: &bare}, {DeleteInode: 11},
					{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "name", Inode: 9}},
					{DeleteEntry: &DirectoryEntryRecord{Parent: 1, Name: "gone", Inode: 10}},
				},
				ReleasePins: []string{"construct/x"}, Result: root,
			}},
			{ApplyFilesystem: &filesystemJournalMutation{Origin: 3, RootName: "branch/main", Operations: []FilesystemOperation{{DeleteInode: 9}}, Result: root}},
		},
		References: []journalReference{{Object: root, Refs: 3, State: ObjectLive}, {Object: pin, State: ObjectReclaiming}},
	}
}

// A checkpoint decodes to the state encoded, fields nil and empty alike.
func TestGraphCheckpointRoundTrip(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	states := []graphState{newGraphState()}
	for round := 0; round < 20; round++ {
		states = append(states, checkpointFixture(random, random.Intn(200)))
	}
	for index := range states {
		data, err := encodeGraphCheckpoint(&states[index])
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeGraphCheckpoint(data)
		if err != nil {
			t.Fatalf("state %d: %v", index, err)
		}
		if !reflect.DeepEqual(got, states[index]) {
			t.Fatalf("state %d decodes otherwise than encoded", index)
		}
	}
}

// A checkpoint that fails its checksum, or holds other than it declares, is
// refused.
func TestGraphCheckpointCorruption(t *testing.T) {
	state := checkpointFixture(rand.New(rand.NewSource(2)), 50)
	data, err := encodeGraphCheckpoint(&state)
	if err != nil {
		t.Fatal(err)
	}
	for index := range data {
		corrupt := bytes.Clone(data)
		corrupt[index] ^= 0x10
		if _, err := decodeGraphCheckpoint(corrupt); err == nil {
			t.Fatalf("a checkpoint with byte %d changed was taken", index)
		}
	}
	for _, length := range []int{0, len(checkpointMagic), len(data) / 2, len(data) - 1} {
		if _, err := decodeGraphCheckpoint(data[:length]); err == nil {
			t.Fatalf("a checkpoint of %d bytes of %d was taken", length, len(data))
		}
	}
	// A body whose checksum is right, but which declares fewer bytes than
	// its objects hold.
	body := bytes.Clone(data[:len(data)-4])
	objects, n := binary.Uvarint(body[len(checkpointMagic):])
	dataBytes, m := binary.Uvarint(body[len(checkpointMagic)+n:])
	if objects == 0 || dataBytes < 2 {
		t.Fatalf("fixture declares %d objects of %d bytes", objects, dataBytes)
	}
	prefix := len(checkpointMagic) + n
	resealed := binary.AppendUvarint(bytes.Clone(body[:prefix]), dataBytes/2)
	resealed = append(resealed, body[prefix+m:]...)
	resealed = binary.BigEndian.AppendUint32(resealed, crc32.Checksum(resealed, castagnoli))
	if _, err := decodeGraphCheckpoint(resealed); err == nil {
		t.Fatal("a checkpoint declaring fewer object bytes than it holds was taken")
	}
}

// A journal record decodes to the record encoded.
func TestJournalRecordRoundTrip(t *testing.T) {
	random := rand.New(rand.NewSource(3))
	for round := 0; round < 10; round++ {
		record := journalFixture(random)
		data, err := encodeJournalRecord(&record)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeJournalRecord(data)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, record) {
			t.Fatalf("round %d decodes otherwise than encoded:\n%+v\nwant\n%+v", round, got, record)
		}
		for length := 0; length < len(data); length++ {
			if _, err := decodeJournalRecord(data[:length]); err == nil {
				t.Fatalf("round %d: a record of %d bytes of %d was taken", round, length, len(data))
			}
		}
	}
	if _, err := encodeJournalRecord(&journalRecord{Sequence: 1, Ops: []journalOp{{}}}); err == nil {
		t.Fatal("an empty operation was encoded")
	}
}

// A journal ending within a record, or in a last record that fails its
// checksum, ends before that record; a record before others that fails its
// checksum is corruption.
func TestNextJournalRecord(t *testing.T) {
	first := frameJournalRecord([]byte("first"))
	second := frameJournalRecord([]byte("second record"))
	journal := append(bytes.Clone(first), second...)
	data, length, complete, err := nextJournalRecord(journal)
	if err != nil || !complete || length != len(first) || string(data) != "first" {
		t.Fatalf("first record: %q %d %t %v", data, length, complete, err)
	}
	for end := len(first); end < len(journal); end++ {
		if _, _, complete, err := nextJournalRecord(journal[len(first):end]); err != nil || complete {
			t.Fatalf("a record cut at %d: complete %t, %v", end, complete, err)
		}
	}
	torn := bytes.Clone(journal)
	torn[len(torn)-1] ^= 1
	if _, _, complete, err := nextJournalRecord(torn[len(first):]); err != nil || complete {
		t.Fatalf("a torn last record: complete %t, %v", complete, err)
	}
	corrupt := bytes.Clone(journal)
	corrupt[len(first)-1] ^= 1
	if _, _, _, err := nextJournalRecord(corrupt); err == nil {
		t.Fatal("a corrupt record followed by another was taken")
	}
	if _, _, _, err := nextJournalRecord(make([]byte, 16)); err == nil {
		t.Fatal("a record of length 0 was taken")
	}
}

// A graph of the legacy format opens converted: its checkpoint and the
// complete records of its journal make its state, and it is checkpointed
// anew.
func TestLegacyGraphIsConverted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root := ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 1}
	child := ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 2}
	gone := ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 3}
	checkpoint := legacyCheckpoint{
		Version: legacyFormatVersion, Sequence: 1, NextSnapshot: 2,
		Snapshots: map[string]Snapshot{snapshotKey(1): {ID: 1, State: SnapshotWorking, NextOrdinal: 4}},
		Objects: map[string]Object{
			root.key(): {ID: root, State: ObjectLive, Refs: 1, Data: []byte("root")},
			gone.key(): {ID: gone, State: ObjectLive, Refs: 1, Data: []byte("gone")},
		},
		Roots: map[string]ObjectID{"branch/main": root}, Pins: map[string]ObjectID{"construct/gone": gone},
		Frozen: map[string]bool{}, Handoffs: map[string]SnapshotLedgerHandoff{},
	}
	put := Object{ID: child, State: ObjectLive, Refs: 1, Data: []byte("child")}
	records := []legacyJournalRecord{
		{Version: legacyFormatVersion, Sequence: 2, Ops: []legacyJournalOp{{PutObject: &put}, {PinName: "construct/child", PutPin: &child}}},
		{Version: legacyFormatVersion, Sequence: 3, Ops: []legacyJournalOp{{PinName: "construct/gone", DeletePin: true}, {DeleteObject: gone.key()}}},
		// Being appended when its writer crashed.
		{Version: legacyFormatVersion, Sequence: 4, Ops: []legacyJournalOp{{PinName: "construct/child", DeletePin: true}}},
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	journal := []byte(legacyJournalHeader)
	for index, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		journal = binary.BigEndian.AppendUint32(journal, uint32(len(encoded)))
		if index == len(records)-1 {
			encoded = encoded[:len(encoded)/2]
		}
		journal = append(journal, encoded...)
	}
	for name, content := range map[string][]byte{
		headFileName: []byte(`{"version":5,"generation":7}` + "\n"), "graph.lock": nil,
		graphCheckpointName(7): data, graphJournalName(7): journal,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.generation != 8 || store.state.Sequence != 3 {
		t.Fatalf("converted to generation %d at sequence %d, want 8 at 3", store.generation, store.state.Sequence)
	}
	if _, ok := store.Object(gone); ok {
		t.Fatal("an object the journal deleted survived the conversion")
	}
	if object, ok := store.Object(child); !ok || string(object.Data) != "child" || !store.HasPin("construct/child") {
		t.Fatal("what the journal put is missing after the conversion")
	}
	head, err := os.ReadFile(filepath.Join(dir, headFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != `{"version":6,"generation":8}`+"\n" {
		t.Fatalf("head is %q after the conversion", head)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != headFileName && name != "graph.lock" && name != graphCheckpointName(8) && name != graphJournalName(8) {
			t.Fatalf("the conversion left %q", name)
		}
	}
	// A legacy graph that is invalid is refused, and left as it is.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.Mkdir(broken, 0700); err != nil {
		t.Fatal(err)
	}
	checkpoint.Pins = map[string]ObjectID{}
	if data, err = json.Marshal(checkpoint); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		headFileName: []byte(`{"version":5,"generation":1}`), "graph.lock": nil,
		graphCheckpointName(1): data, graphJournalName(1): []byte(legacyJournalHeader),
	} {
		if err := os.WriteFile(filepath.Join(broken, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(broken); err == nil {
		t.Fatal("an invalid legacy graph was converted")
	}
	if left, err := os.ReadFile(filepath.Join(broken, graphCheckpointName(1))); err != nil || !bytes.Equal(left, data) {
		t.Fatalf("a refused conversion changed the checkpoint: %v", err)
	}
}

// FuzzDecodeGraphCheckpoint checks that a checkpoint decodes without panic,
// and that what it decodes to encodes to a checkpoint decoding alike.
func FuzzDecodeGraphCheckpoint(f *testing.F) {
	state := checkpointFixture(rand.New(rand.NewSource(4)), 6)
	data, err := encodeGraphCheckpoint(&state)
	if err != nil {
		f.Fatal(err)
	}
	// The body: the fuzzer's own seals it.
	f.Add(data[len(checkpointMagic) : len(data)-4])
	f.Fuzz(func(t *testing.T, body []byte) {
		data := append([]byte(checkpointMagic), body...)
		data = binary.BigEndian.AppendUint32(data, crc32.Checksum(data, castagnoli))
		state, err := decodeGraphCheckpoint(data)
		if err != nil {
			return
		}
		again, err := encodeGraphCheckpoint(&state)
		if err != nil {
			t.Fatalf("a decoded checkpoint does not encode: %v", err)
		}
		if got, err := decodeGraphCheckpoint(again); err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("a checkpoint re-encoded decodes (%v) otherwise", err)
		}
	})
}

// FuzzDecodeJournalRecord is FuzzDecodeGraphCheckpoint for a journal record.
func FuzzDecodeJournalRecord(f *testing.F) {
	record := journalFixture(rand.New(rand.NewSource(5)))
	data, err := encodeJournalRecord(&record)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) {
		record, err := decodeJournalRecord(data)
		if err != nil {
			return
		}
		again, err := encodeJournalRecord(&record)
		if err != nil {
			t.Fatalf("a decoded record does not encode: %v", err)
		}
		if got, err := decodeJournalRecord(again); err != nil || !reflect.DeepEqual(got, record) {
			t.Fatalf("a record re-encoded decodes (%v) otherwise", err)
		}
	})
}
