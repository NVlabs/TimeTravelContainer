// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
)

// appendJSONString appends s as json.Marshal writes it.
func appendJSONString(out []byte, s string) []byte {
	return appendJSONStringEscaping(out, s, true)
}

// appendJSONStringEscaping appends s as a json.Encoder writes it that
// escapes HTML, or does not.
func appendJSONStringEscaping(out []byte, s string, escapeHTML bool) []byte {
	for i := 0; i < len(s); i++ {
		// Beyond these, encoding/json escapes a character or replaces it.
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || escapeHTML && (c == '<' || c == '>' || c == '&') {
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(escapeHTML)
			_ = encoder.Encode(s)
			return append(out, bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})...)
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

// jsonDecoder reads the canonical form of the JSON of a shared manifest or a
// filesystem record: each field once and known, each string ASCII without
// escapes, each number an integer.
type jsonDecoder struct {
	data []byte
	pos  int
}

func (d *jsonDecoder) space() {
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
func (d *jsonDecoder) consume(c byte) bool {
	d.space()
	if d.pos < len(d.data) && d.data[d.pos] == c {
		d.pos++
		return true
	}
	return false
}

// literal consumes word after any space.
func (d *jsonDecoder) literal(word string) bool {
	d.space()
	if len(d.data)-d.pos >= len(word) && string(d.data[d.pos:d.pos+len(word)]) == word {
		d.pos += len(word)
		return true
	}
	return false
}

// plainString consumes a string of printable ASCII without escapes, and
// returns its contents.
func (d *jsonDecoder) plainString() ([]byte, bool) {
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
func (d *jsonDecoder) integer(bits uint) (uint64, bool) {
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
func (d *jsonDecoder) members(member func(name []byte) bool) bool {
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

// array consumes an array, or null, calling element with the decoder at each
// element, which element consumes. It reports whether the array was null.
func (d *jsonDecoder) array(element func() bool) (null, ok bool) {
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

// skip consumes any value, checking its syntax.
func (d *jsonDecoder) skip(depth int) bool {
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

// object consumes one object's record.
func (d *jsonDecoder) object(object *Object) bool {
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
			name, ok := d.plainString()
			if ok {
				object.State, ok = parseObjectState(string(name))
			}
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

func (d *jsonDecoder) objectID(id *ObjectID) bool {
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
func (d *jsonDecoder) bytes(value *[]byte) bool {
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
func (d *jsonDecoder) edges(value *[]Edge) bool {
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
