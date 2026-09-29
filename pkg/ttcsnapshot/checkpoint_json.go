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
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
)

// decodeGraphCheckpoint decodes a graph checkpoint as json.Unmarshal does
// into a graphState. A checkpoint as json.Marshal writes one has its objects,
// nearly all of it, decoded here without reflection, four times as fast:
// encoding/json took 1.6 s for 300,000 objects, and a daemon's first load of
// a repository of 100,000 files spent 4 s in it. Anything else, however
// valid, is left to encoding/json whole, so that the two can never differ.
func decodeGraphCheckpoint(data []byte) (graphState, error) {
	if state, ok := decodeCanonicalCheckpoint(data); ok {
		return state, nil
	}
	var state graphState
	err := json.Unmarshal(data, &state)
	return state, err
}

// encodeGraphCheckpoint encodes state as json.Marshal does, byte for byte,
// the objects without reflection, twice as fast: encoding/json took 1 s for
// 300,000 objects.
func encodeGraphCheckpoint(state *graphState) ([]byte, error) {
	// The fields around the objects are small: encoding/json encodes them.
	head, err := json.Marshal(struct {
		Version      int
		Sequence     uint64
		NextSnapshot SnapshotID
		Snapshots    map[string]Snapshot
	}{state.Version, state.Sequence, state.NextSnapshot, state.Snapshots})
	if err != nil {
		return nil, err
	}
	tail, err := json.Marshal(struct {
		Roots    map[string]ObjectID
		Pins     map[string]ObjectID
		Frozen   map[string]bool
		Handoffs map[string]SnapshotLedgerHandoff
	}{state.Roots, state.Pins, state.Frozen, state.Handoffs})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(head)+len(tail)+objectTableJSONSize(state.Objects))
	out = append(out, head[:len(head)-1]...)
	out = append(out, `,"Objects":`...)
	out = appendObjectTableJSON(out, state.Objects)
	out = append(out, ',')
	out = append(out, tail[1:]...)
	return out, nil
}

// objectTableJSONSize estimates, generously, the length of objects as
// appendObjectTableJSON writes it. A buffer sized at 256 bytes an object grew
// as a checkpoint filled it, and copied a checkpoint of 100 MB over and over.
func objectTableJSONSize(objects map[string]Object) int {
	size := 2
	for key, object := range objects {
		size += len(key) + 4 + 190 + len(object.State) + base64.StdEncoding.EncodedLen(len(object.Data)) + 64*len(object.Children) + len(object.SourceCatalog)
	}
	return size
}

// appendObjectTableJSON appends objects as json.Marshal writes it.
func appendObjectTableJSON(out []byte, objects map[string]Object) []byte {
	if objects == nil {
		return append(out, "null"...)
	}
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out = append(out, '{')
	for index, key := range keys {
		if index != 0 {
			out = append(out, ',')
		}
		out = appendJSONString(out, key)
		out = append(out, ':')
		out = appendObjectJSON(out, objects[key])
	}
	return append(out, '}')
}

// appendJSONString appends s as json.Marshal writes it.
func appendJSONString(out []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		// Beyond these, json.Marshal escapes a character or replaces it.
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			encoded, _ := json.Marshal(s)
			return append(out, encoded...)
		}
	}
	out = append(out, '"')
	out = append(out, s...)
	return append(out, '"')
}

// appendJSONBytes appends value as json.Marshal writes a byte slice.
func appendJSONBytes(out []byte, value []byte) []byte {
	if value == nil {
		return append(out, "null"...)
	}
	out = append(out, '"')
	out = base64.StdEncoding.AppendEncode(out, value)
	return append(out, '"')
}

func appendObjectIDJSON(out []byte, id ObjectID) []byte {
	out = append(out, `{"Snapshot":`...)
	out = strconv.AppendUint(out, uint64(id.Snapshot), 10)
	out = append(out, `,"Ordinal":`...)
	out = strconv.AppendUint(out, id.Ordinal, 10)
	out = append(out, `,"Kind":`...)
	out = strconv.AppendUint(out, uint64(id.Kind), 10)
	return append(out, '}')
}

// appendObjectJSON appends object as json.Marshal writes it.
func appendObjectJSON(out []byte, object Object) []byte {
	out = append(out, `{"ID":`...)
	out = appendObjectIDJSON(out, object.ID)
	out = append(out, `,"State":`...)
	out = appendJSONString(out, string(object.State))
	out = append(out, `,"Refs":`...)
	out = strconv.AppendUint(out, object.Refs, 10)
	out = append(out, `,"BulkLength":`...)
	out = strconv.AppendUint(out, object.BulkLength, 10)
	out = append(out, `,"Data":`...)
	out = appendJSONBytes(out, object.Data)
	out = append(out, `,"Children":`...)
	if object.Children == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index, edge := range object.Children {
			if index != 0 {
				out = append(out, ',')
			}
			out = append(out, `{"Slot":`...)
			out = strconv.AppendUint(out, uint64(edge.Slot), 10)
			out = append(out, `,"Child":`...)
			out = appendObjectIDJSON(out, edge.Child)
			out = append(out, '}')
		}
		out = append(out, ']')
	}
	if object.SourceCatalog != "" {
		out = append(out, `,"SourceCatalog":`...)
		out = appendJSONString(out, object.SourceCatalog)
	}
	out = append(out, `,"SourceObject":`...)
	out = appendObjectIDJSON(out, object.SourceObject)
	if object.Deferred {
		out = append(out, `,"Deferred":true`...)
	}
	return append(out, '}')
}

// checkpointDecoder reads the canonical form of a checkpoint: each field once
// and known, each string ASCII without escapes, each number an integer.
type checkpointDecoder struct {
	data []byte
	pos  int
}

// decodeCanonicalCheckpoint decodes a checkpoint of the canonical form. It
// reports false for anything else.
func decodeCanonicalCheckpoint(data []byte) (graphState, bool) {
	d := checkpointDecoder{data: data}
	var state graphState
	var seen [9]bool
	ok := d.members(func(name []byte) bool {
		// The fields other than the objects are small: encoding/json decodes
		// them.
		var target any
		index := 0
		switch string(name) {
		case "Version":
			target = &state.Version
		case "Sequence":
			index, target = 1, &state.Sequence
		case "NextSnapshot":
			index, target = 2, &state.NextSnapshot
		case "Snapshots":
			index, target = 3, &state.Snapshots
		case "Objects":
			if seen[4] {
				return false
			}
			seen[4] = true
			return d.objects(&state.Objects)
		case "Roots":
			index, target = 5, &state.Roots
		case "Pins":
			index, target = 6, &state.Pins
		case "Frozen":
			index, target = 7, &state.Frozen
		case "Handoffs":
			index, target = 8, &state.Handoffs
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		start := d.pos
		if !d.skip(0) {
			return false
		}
		return json.Unmarshal(d.data[start:d.pos], target) == nil
	})
	d.space()
	return state, ok && d.pos == len(d.data)
}

func (d *checkpointDecoder) space() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

// consume consumes c after any space.
func (d *checkpointDecoder) consume(c byte) bool {
	d.space()
	if d.pos < len(d.data) && d.data[d.pos] == c {
		d.pos++
		return true
	}
	return false
}

// literal consumes word after any space.
func (d *checkpointDecoder) literal(word string) bool {
	d.space()
	if len(d.data)-d.pos >= len(word) && string(d.data[d.pos:d.pos+len(word)]) == word {
		d.pos += len(word)
		return true
	}
	return false
}

// plainString consumes a string of printable ASCII without escapes, and
// returns its contents.
func (d *checkpointDecoder) plainString() ([]byte, bool) {
	d.space()
	if d.pos >= len(d.data) || d.data[d.pos] != '"' {
		return nil, false
	}
	for i := d.pos + 1; i < len(d.data); i++ {
		switch c := d.data[i]; {
		case c == '"':
			value := d.data[d.pos+1 : i]
			d.pos = i + 1
			return value, true
		case c == '\\' || c < 0x20 || c >= 0x7f:
			return nil, false
		}
	}
	return nil, false
}

// integer consumes an unsigned integer of at most bits bits, written without
// a sign, fraction, exponent or leading zero.
func (d *checkpointDecoder) integer(bits uint) (uint64, bool) {
	d.space()
	start := d.pos
	limit := uint64(1)<<bits - 1
	if bits == 64 {
		limit = ^uint64(0)
	}
	var value uint64
	for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
		digit := uint64(d.data[d.pos] - '0')
		if value > (limit-digit)/10 {
			return 0, false
		}
		value = value*10 + digit
		d.pos++
	}
	digits := d.pos - start
	if digits == 0 || digits > 1 && d.data[start] == '0' {
		return 0, false
	}
	if d.pos < len(d.data) {
		switch d.data[d.pos] {
		case '.', 'e', 'E':
			return 0, false
		}
	}
	return value, true
}

// members consumes an object, calling member with each member's name with
// the decoder at its value, which member consumes.
func (d *checkpointDecoder) members(member func(name []byte) bool) bool {
	if !d.consume('{') {
		return false
	}
	if d.consume('}') {
		return true
	}
	for {
		name, ok := d.plainString()
		if !ok || !d.consume(':') || !member(name) {
			return false
		}
		if d.consume(',') {
			continue
		}
		return d.consume('}')
	}
}

// skip consumes any value, checking its syntax.
func (d *checkpointDecoder) skip(depth int) bool {
	if depth > 10000 {
		return false
	}
	d.space()
	if d.pos >= len(d.data) {
		return false
	}
	switch d.data[d.pos] {
	case '{':
		return d.members(func([]byte) bool { return d.skip(depth + 1) })
	case '[':
		d.pos++
		if d.consume(']') {
			return true
		}
		for {
			if !d.skip(depth + 1) {
				return false
			}
			if d.consume(',') {
				continue
			}
			return d.consume(']')
		}
	case '"':
		_, ok := d.plainString()
		return ok
	case 't':
		return d.literal("true")
	case 'f':
		return d.literal("false")
	case 'n':
		return d.literal("null")
	default:
		_, ok := d.integer(64)
		return ok
	}
}

// objects consumes the checkpoint's object table into *table.
func (d *checkpointDecoder) objects(table *map[string]Object) bool {
	if d.literal("null") {
		*table = nil
		return true
	}
	if *table == nil {
		*table = make(map[string]Object)
	}
	return d.members(func(key []byte) bool {
		var object Object
		if !d.object(&object) {
			return false
		}
		(*table)[string(key)] = object
		return true
	})
}

// object consumes one object's record.
func (d *checkpointDecoder) object(object *Object) bool {
	var seen [9]bool
	once := func(index int) bool {
		if seen[index] {
			return false
		}
		seen[index] = true
		return true
	}
	return d.members(func(name []byte) bool {
		switch string(name) {
		case "ID":
			return once(0) && d.objectID(&object.ID)
		case "State":
			state, ok := d.plainString()
			object.State = ObjectState(state)
			return once(1) && ok
		case "Refs":
			refs, ok := d.integer(64)
			object.Refs = refs
			return once(2) && ok
		case "BulkLength":
			length, ok := d.integer(64)
			object.BulkLength = length
			return once(3) && ok
		case "Data":
			return once(4) && d.bytes(&object.Data)
		case "Children":
			return once(5) && d.edges(&object.Children)
		case "SourceCatalog":
			catalog, ok := d.plainString()
			object.SourceCatalog = string(catalog)
			return once(6) && ok
		case "SourceObject":
			return once(7) && d.objectID(&object.SourceObject)
		case "Deferred":
			if !once(8) {
				return false
			}
			if d.literal("true") {
				object.Deferred = true
				return true
			}
			return d.literal("false")
		default:
			return false
		}
	})
}

func (d *checkpointDecoder) objectID(id *ObjectID) bool {
	var seen [3]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		switch string(name) {
		case "Snapshot":
			var snapshot uint64
			snapshot, ok = d.integer(64)
			id.Snapshot = SnapshotID(snapshot)
		case "Ordinal":
			index = 1
			id.Ordinal, ok = d.integer(64)
		case "Kind":
			index = 2
			var kind uint64
			kind, ok = d.integer(8)
			id.Kind = ObjectKind(kind)
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

// bytes consumes a base64 string, or null, as json.Unmarshal does into a
// byte slice: "" is an empty slice, null a nil one.
func (d *checkpointDecoder) bytes(value *[]byte) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	encoded, ok := d.plainString()
	if !ok {
		return false
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, err := base64.StdEncoding.Decode(decoded, encoded)
	*value = decoded[:n]
	return err == nil
}

// edges consumes an array of edges, or null, into a new slice.
func (d *checkpointDecoder) edges(value *[]Edge) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	if !d.consume('[') {
		return false
	}
	edges := []Edge{}
	if !d.consume(']') {
		for {
			var edge Edge
			var seen [2]bool
			if !d.members(func(name []byte) bool {
				switch string(name) {
				case "Slot":
					slot, ok := d.integer(16)
					edge.Slot = uint16(slot)
					if seen[0] {
						return false
					}
					seen[0] = true
					return ok
				case "Child":
					if seen[1] {
						return false
					}
					seen[1] = true
					return d.objectID(&edge.Child)
				default:
					return false
				}
			}) {
				return false
			}
			edges = append(edges, edge)
			if d.consume(',') {
				continue
			}
			if !d.consume(']') {
				return false
			}
			break
		}
	}
	*value = edges
	return true
}
