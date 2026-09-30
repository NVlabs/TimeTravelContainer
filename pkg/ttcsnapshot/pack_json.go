// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
)

// encodeSharedManifest encodes manifest as a json.Encoder that does not
// escape HTML encodes it, byte for byte, its objects, nearly all of it,
// without reflection: encoding/json took 0.5 s for the manifest of a branch
// of 100,000 files.
func encodeSharedManifest(manifest *SnapshotManifest) ([]byte, error) {
	// The packs are few: encoding/json encodes them.
	packs, err := json.Marshal(manifest.Packs)
	if err != nil {
		return nil, err
	}
	size := 64 + len(packs) + 48*len(manifest.References)
	for index := range manifest.Objects {
		object := &manifest.Objects[index].Object
		size += 230 + len(object.State) + base64.StdEncoding.EncodedLen(len(object.Data)) + 64*len(object.Children) + len(object.SourceCatalog) + 64*len(manifest.Objects[index].Extents)
	}
	out := make([]byte, 0, size)
	out = append(out, `{"version":`...)
	out = strconv.AppendInt(out, int64(manifest.Version), 10)
	out = append(out, `,"snapshot":`...)
	out = strconv.AppendUint(out, uint64(manifest.Snapshot), 10)
	out = append(out, `,"packs":`...)
	out = append(out, packs...)
	out = append(out, `,"objects":`...)
	if manifest.Objects == nil {
		out = append(out, "null"...)
	} else {
		out = append(out, '[')
		for index := range manifest.Objects {
			packed := &manifest.Objects[index]
			if index != 0 {
				out = append(out, ',')
			}
			out = append(out, `{"object":`...)
			out = appendObjectJSONEscaping(out, packed.Object, false)
			if len(packed.Extents) != 0 {
				out = append(out, `,"extents":[`...)
				for extent, value := range packed.Extents {
					if extent != 0 {
						out = append(out, ',')
					}
					out = append(out, `{"pack":`...)
					out = strconv.AppendUint(out, uint64(value.Pack), 10)
					out = append(out, `,"offset":`...)
					out = strconv.AppendInt(out, value.Offset, 10)
					out = append(out, `,"length":`...)
					out = strconv.AppendInt(out, value.Length, 10)
					out = append(out, '}')
				}
				out = append(out, ']')
			}
			out = append(out, '}')
		}
		out = append(out, ']')
	}
	if len(manifest.References) != 0 {
		names := make([]string, 0, len(manifest.References))
		for name := range manifest.References {
			names = append(names, name)
		}
		slices.Sort(names)
		out = append(out, `,"references":{`...)
		for index, name := range names {
			if index != 0 {
				out = append(out, ',')
			}
			out = appendJSONStringEscaping(out, name, false)
			out = append(out, ':')
			out = appendObjectIDJSON(out, manifest.References[name])
		}
		out = append(out, '}')
	}
	return append(out, '}', '\n'), nil
}

// decodeSharedManifest decodes a shared snapshot manifest as json.Unmarshal
// does. A manifest as writeAtomicJSON writes one has its objects, nearly all
// of it, decoded as a graph checkpoint's are, without reflection: the
// manifest of a branch of 100,000 files is 110 MB, which encoding/json took
// 1.4 s to decode, and each node that reads the snapshot decodes it. Anything
// else, however valid, is left to encoding/json whole, so that the two can
// never differ.
func decodeSharedManifest(data []byte) (SnapshotManifest, error) {
	if manifest, ok := decodeCanonicalManifest(data); ok {
		return manifest, nil
	}
	var manifest SnapshotManifest
	err := json.Unmarshal(data, &manifest)
	return manifest, err
}

// decodeCanonicalManifest decodes a manifest of the canonical form. It
// reports false for anything else.
func decodeCanonicalManifest(data []byte) (SnapshotManifest, bool) {
	d := jsonDecoder{data: data}
	var manifest SnapshotManifest
	var seen [5]bool
	ok := d.members(func(name []byte) bool {
		// The fields other than the objects and references are small:
		// encoding/json decodes them.
		var target any
		index := 0
		switch string(name) {
		case "version":
			target = &manifest.Version
		case "snapshot":
			index, target = 1, &manifest.Snapshot
		case "packs":
			index, target = 2, &manifest.Packs
		case "objects":
			if seen[3] {
				return false
			}
			seen[3] = true
			return d.packedObjects(&manifest.Objects)
		case "references":
			if seen[4] {
				return false
			}
			seen[4] = true
			return d.references(&manifest.References)
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
	return manifest, ok && d.pos == len(d.data)
}

// packedObjects consumes an array of packed objects, or null, into a new
// slice.
func (d *jsonDecoder) packedObjects(value *[]PackedObject) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	if !d.consume('[') {
		return false
	}
	objects := []PackedObject{}
	if !d.consume(']') {
		for {
			var packed PackedObject
			var seen [2]bool
			if !d.members(func(name []byte) bool {
				switch string(name) {
				case "object":
					if seen[0] {
						return false
					}
					seen[0] = true
					return d.object(&packed.Object)
				case "extents":
					if seen[1] {
						return false
					}
					seen[1] = true
					return d.extents(&packed.Extents)
				default:
					return false
				}
			}) {
				return false
			}
			objects = append(objects, packed)
			if d.consume(',') {
				continue
			}
			if !d.consume(']') {
				return false
			}
			break
		}
	}
	*value = objects
	return true
}

// extents consumes an array of pack extents, or null, into a new slice.
func (d *jsonDecoder) extents(value *[]PackExtent) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	if !d.consume('[') {
		return false
	}
	extents := []PackExtent{}
	if !d.consume(']') {
		for {
			var extent PackExtent
			var seen [3]bool
			if !d.members(func(name []byte) bool {
				var index int
				var bits uint = 63
				switch string(name) {
				case "pack":
					bits = 32
				case "offset":
					index = 1
				case "length":
					index = 2
				default:
					return false
				}
				if seen[index] {
					return false
				}
				seen[index] = true
				number, ok := d.integer(bits)
				switch index {
				case 0:
					extent.Pack = uint32(number)
				case 1:
					extent.Offset = int64(number)
				case 2:
					extent.Length = int64(number)
				}
				return ok
			}) {
				return false
			}
			extents = append(extents, extent)
			if d.consume(',') {
				continue
			}
			if !d.consume(']') {
				return false
			}
			break
		}
	}
	*value = extents
	return true
}

// references consumes an object of named object IDs, or null, into a new
// map.
func (d *jsonDecoder) references(value *map[string]ObjectID) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	references := make(map[string]ObjectID)
	if !d.members(func(name []byte) bool {
		// encoding/json keeps the last of a repeated name: leave it that.
		if _, repeated := references[string(name)]; repeated {
			return false
		}
		var id ObjectID
		if !d.objectID(&id) {
			return false
		}
		references[string(name)] = id
		return true
	}) {
		return false
	}
	*value = references
	return true
}
