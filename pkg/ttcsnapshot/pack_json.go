// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"encoding/json"
)

// decodeJSONManifest decodes a shared snapshot manifest of an earlier
// release, JSON, as json.Unmarshal does. A manifest as it wrote one has its
// objects, nearly all of it, decoded without reflection: the manifest of a
// branch of 100,000 files was 110 MB, which encoding/json took 1.4 s to
// decode. Anything else, however valid, is left to encoding/json whole, so
// that the two can never differ.
func decodeJSONManifest(data []byte) (SnapshotManifest, error) {
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
