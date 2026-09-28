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
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// checkpointFixture returns a graph state with every kind of field a
// checkpoint holds, some empty and some absent.
func checkpointFixture(random *rand.Rand, objects int) graphState {
	state := graphState{
		Version: formatVersion, Sequence: random.Uint64(), NextSnapshot: 4,
		Snapshots: map[string]Snapshot{
			snapshotKey(1): {ID: 1, State: SnapshotWorking, NextOrdinal: uint64(objects) + 10},
			snapshotKey(2): {ID: 2, State: SnapshotSealedVolatile, NextOrdinal: 3},
		},
		Objects: make(map[string]Object), Roots: make(map[string]ObjectID), Pins: make(map[string]ObjectID),
		Frozen:   map[string]bool{snapshotKey(2): true},
		Handoffs: map[string]SnapshotLedgerHandoff{snapshotKey(2): {Snapshot: 2, NextSequence: 5, Frozen: true, Baseline: map[string]ObjectID{"root/a": {Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 1}}, Mutations: []SnapshotReferenceMutation{{Sequence: 4, Operation: "op", Name: "root/b", Object: ObjectID{Snapshot: 2, Kind: ObjectKindMetadata, Ordinal: 2}, Retain: true}}}},
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
		state.Objects[id.key()] = object
	}
	state.Roots["branch/main"] = ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 1}
	state.Pins["construct/x"] = ObjectID{Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 2}
	return state
}

// checkDecodesAlike fails unless decodeGraphCheckpoint decodes data as
// json.Unmarshal does, and reports whether the canonical decoder took it.
func checkDecodesAlike(t *testing.T, data []byte) bool {
	t.Helper()
	var want graphState
	wantErr := json.Unmarshal(data, &want)
	canonical, ok := decodeCanonicalCheckpoint(data)
	if ok && (wantErr != nil || !reflect.DeepEqual(canonical, want)) {
		t.Fatalf("the canonical decoder took what json.Unmarshal decodes otherwise (%v):\n%.300s", wantErr, data)
	}
	got, err := decodeGraphCheckpoint(data)
	if (err == nil) != (wantErr == nil) || err == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded (%v) differently from json.Unmarshal (%v):\n%.300s", err, wantErr, data)
	}
	return ok
}

// A checkpoint as json.Marshal writes it decodes, without encoding/json, to
// what json.Unmarshal decodes; any other form goes to encoding/json.
func TestDecodeGraphCheckpoint(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for round := 0; round < 20; round++ {
		data, err := json.Marshal(checkpointFixture(random, 1+random.Intn(200)))
		if err != nil {
			t.Fatal(err)
		}
		if !checkDecodesAlike(t, data) {
			t.Fatalf("round %d: the canonical decoder refused json.Marshal's checkpoint", round)
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, "", "\t"); err != nil {
			t.Fatal(err)
		}
		if !checkDecodesAlike(t, indented.Bytes()) {
			t.Fatalf("round %d: the canonical decoder refused an indented checkpoint", round)
		}
	}
	data, err := json.Marshal(checkpointFixture(random, 20))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	id := `{"Snapshot":1,"Kind":`
	for _, variant := range []string{
		strings.Replace(text, `"Objects"`, `"objects"`, 1),
		strings.Replace(text, `"Refs"`, `"refs"`, 1),
		strings.Replace(text, `"State":"live"`, `"State":"live"`, 1),
		strings.Replace(text, `"Refs":`, `"Unknown":1,"Refs":`, 1),
		strings.Replace(text, `"Refs":`, `"Refs":1,"Refs":`, 1),
		strings.Replace(text, `"Refs":1`, `"Refs":1.0`, 1),
		strings.Replace(text, `"Refs":1`, `"Refs":"1"`, 1),
		strings.Replace(text, `"Refs":1`, `"Refs":null`, 1),
		strings.Replace(text, `"Data":`, `"Data":"!",`+`"x":`, 1),
		strings.Replace(text, `"Deferred":true`, `"Deferred":null`, 1),
		strings.Replace(text, id, `{"Snapshot":1,"Kind":300,"x":`, 1),
		strings.Replace(text, `"Version":`, `"Version":-`, 1),
		strings.Replace(text, `"Roots":{`, `"Roots":{"ré":`+id+`1,"Ordinal":1},`, 1),
		text + " x",
		"null",
		"{}",
		"[]",
		"",
	} {
		checkDecodesAlike(t, []byte(variant))
	}
}

// A checkpoint encodes as json.Marshal writes it, byte for byte.
func TestEncodeGraphCheckpoint(t *testing.T) {
	random := rand.New(rand.NewSource(3))
	states := []graphState{{}, {Objects: map[string]Object{}}}
	for round := 0; round < 20; round++ {
		states = append(states, checkpointFixture(random, random.Intn(200)))
	}
	odd := checkpointFixture(random, 5)
	odd.Objects["key <&> \"quoted\" \u2028 \xff"] = Object{ID: ObjectID{Snapshot: 1, Kind: 1, Ordinal: 1}, State: "odd\tstate", SourceCatalog: "<catalog>"}
	states = append(states, odd)
	for index := range states {
		want, err := json.Marshal(states[index])
		if err != nil {
			t.Fatal(err)
		}
		got, err := encodeGraphCheckpoint(&states[index])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("state %d encodes (%v) as\n%.400s\nwant\n%.400s", index, err, got, want)
		}
	}
}

// FuzzDecodeGraphCheckpoint checks that whatever the canonical decoder takes,
// json.Unmarshal decodes alike.
func FuzzDecodeGraphCheckpoint(f *testing.F) {
	data, err := json.Marshal(checkpointFixture(rand.New(rand.NewSource(2)), 6))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"Objects":{"k":{"ID":{"Snapshot":1,"Ordinal":2,"Kind":3},"Data":"","Children":[]}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkDecodesAlike(t, data)
	})
}
