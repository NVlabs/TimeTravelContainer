// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// A shared snapshot's manifest is binary, as a graph's checkpoint is: its
// header, its packs, its objects each with its extents, and its references,
// then a CRC-32C of all of it. A manifest that begins with '{' is JSON, as an
// earlier release wrote it. As JSON, the manifest of a branch of 100,000
// files was 110 MB, which each node that read the snapshot decoded.
const manifestMagic = "TTCSMF2\n"

// encodeSharedManifest encodes manifest as a shared store holds it.
func encodeSharedManifest(manifest *SnapshotManifest) ([]byte, error) {
	size, dataBytes, edges := len(manifestMagic)+64+16*len(manifest.Packs)+48*len(manifest.References), 0, 0
	for index := range manifest.Objects {
		object := &manifest.Objects[index].Object
		dataBytes += len(object.Data)
		edges += len(object.Children)
		size += 40 + len(object.Data) + 16*len(object.Children) + len(object.SourceCatalog) + 16*len(manifest.Objects[index].Extents)
	}
	e := graphEncoder{out: make([]byte, 0, size)}
	e.out = append(e.out, manifestMagic...)
	e.uvarint(uint64(manifest.Version))
	e.uvarint(uint64(manifest.Snapshot))
	if manifest.Packs == nil {
		e.uvarint(0)
	} else {
		e.uvarint(uint64(len(manifest.Packs)) + 1)
		for _, pack := range manifest.Packs {
			e.uvarint(uint64(pack.Number))
			e.varint(pack.Size)
		}
	}
	// What the objects hold in all, for the decoder to allocate at once.
	e.uvarint(uint64(dataBytes))
	e.uvarint(uint64(edges))
	if manifest.Objects == nil {
		e.uvarint(0)
	} else {
		e.uvarint(uint64(len(manifest.Objects)) + 1)
		for index := range manifest.Objects {
			packed := &manifest.Objects[index]
			if err := e.object(&packed.Object); err != nil {
				return nil, err
			}
			if packed.Extents == nil {
				e.uvarint(0)
				continue
			}
			e.uvarint(uint64(len(packed.Extents)) + 1)
			for _, extent := range packed.Extents {
				e.uvarint(uint64(extent.Pack))
				e.varint(extent.Offset)
				e.varint(extent.Length)
			}
		}
	}
	e.references(manifest.References)
	return binary.BigEndian.AppendUint32(e.out, crc32.Checksum(e.out, castagnoli)), nil
}

// decodeBinaryManifest decodes a manifest as encodeSharedManifest encodes it.
func decodeBinaryManifest(data []byte) (SnapshotManifest, error) {
	if len(data) < len(manifestMagic)+4 {
		return SnapshotManifest{}, errors.New("truncated shared snapshot manifest")
	}
	body := data[:len(data)-4]
	if binary.BigEndian.Uint32(data[len(body):]) != crc32.Checksum(body, castagnoli) {
		return SnapshotManifest{}, errors.New("shared snapshot manifest fails its checksum")
	}
	d := graphDecoder{data: body, pos: len(manifestMagic)}
	var manifest SnapshotManifest
	manifest.Version = int(d.integer(31))
	manifest.Snapshot = SnapshotID(d.uvarint())
	if count, ok := d.nullableCount(); ok {
		manifest.Packs = make([]PackInfo, 0, count)
		for index := 0; index < count && d.err == nil; index++ {
			manifest.Packs = append(manifest.Packs, PackInfo{Number: uint32(d.integer(32)), Size: d.varint()})
		}
	}
	// Each byte and edge of the objects takes at least one byte.
	dataBytes := d.count()
	edges := d.count()
	if count, ok := d.nullableCount(); ok && d.err == nil {
		d.dataArena = make([]byte, dataBytes)
		d.edgeArena = make([]Edge, edges)
		manifest.Objects = make([]PackedObject, count)
		for index := 0; index < count && d.err == nil; index++ {
			packed := &manifest.Objects[index]
			packed.Object = d.object()
			if extents, ok := d.nullableCount(); ok {
				packed.Extents = make([]PackExtent, extents)
				for extent := range packed.Extents {
					packed.Extents[extent] = PackExtent{Pack: uint32(d.integer(32)), Offset: d.varint(), Length: d.varint()}
				}
			}
		}
	}
	manifest.References = d.references()
	if d.err == nil && (d.pos != len(body) || len(d.dataArena) != 0 || len(d.edgeArena) != 0) {
		d.fail("the manifest holds other than it declares")
	}
	if d.err != nil {
		return SnapshotManifest{}, fmt.Errorf("decoding shared snapshot manifest: %w", d.err)
	}
	return manifest, nil
}

// decodeSharedManifest decodes a shared snapshot manifest, binary or, as an
// earlier release wrote it, JSON.
func decodeSharedManifest(data []byte) (SnapshotManifest, error) {
	if bytes.HasPrefix(data, []byte(manifestMagic)) {
		return decodeBinaryManifest(data)
	}
	return decodeJSONManifest(data)
}
