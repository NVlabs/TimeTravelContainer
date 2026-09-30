// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
)

// A manifest decodes to what was encoded, fields nil and empty alike, and a
// manifest an earlier release wrote, JSON, decodes as it did.
func TestSharedManifestRoundTrip(t *testing.T) {
	random := rand.New(rand.NewSource(10))
	manifests := []SnapshotManifest{{}, {Objects: []PackedObject{}, References: map[string]ObjectID{}, Packs: []PackInfo{}}}
	for round := 0; round < 40; round++ {
		manifest := manifestFixture(random, random.Intn(100))
		for index := range manifest.Objects {
			if random.Intn(8) == 0 {
				manifest.Objects[index].Extents = []PackExtent{}
			}
		}
		if random.Intn(2) == 0 {
			manifest.References = map[string]ObjectID{"root/<&>": {Snapshot: 1, Kind: 1, Ordinal: 3}, "edge:x": {Snapshot: 2, Kind: 3, Ordinal: 4}}
		}
		manifests = append(manifests, manifest)
	}
	for index := range manifests {
		data, err := encodeSharedManifest(&manifests[index])
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeSharedManifest(data)
		if err != nil || !reflect.DeepEqual(got, manifests[index]) {
			t.Fatalf("manifest %d decodes (%v) otherwise than encoded", index, err)
		}
		if index < 2 {
			continue
		}
		encoded := encodeManifest(t, manifests[index])
		legacy, err := decodeSharedManifest(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var want SnapshotManifest
		if err := json.Unmarshal(encoded, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(legacy, want) {
			t.Fatalf("manifest %d decodes from JSON otherwise than encoding/json", index)
		}
	}
}

// A manifest that fails its checksum, or holds other than it declares, is
// refused.
func TestSharedManifestCorruption(t *testing.T) {
	manifest := manifestFixture(rand.New(rand.NewSource(11)), 30)
	data, err := encodeSharedManifest(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	for index := range data {
		corrupt := bytes.Clone(data)
		corrupt[index] ^= 0x04
		if _, err := decodeSharedManifest(corrupt); err == nil {
			t.Fatalf("a manifest with byte %d changed was taken", index)
		}
	}
	for _, length := range []int{0, len(manifestMagic), len(data) / 2, len(data) - 1} {
		if _, err := decodeSharedManifest(data[:length]); err == nil {
			t.Fatalf("a manifest of %d bytes of %d was taken", length, len(data))
		}
	}
}

// FuzzDecodeBinaryManifest checks that a manifest decodes without panic, and
// that what it decodes to encodes to a manifest decoding alike.
func FuzzDecodeBinaryManifest(f *testing.F) {
	manifest := manifestFixture(rand.New(rand.NewSource(12)), 6)
	data, err := encodeSharedManifest(&manifest)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data[len(manifestMagic) : len(data)-4])
	f.Fuzz(func(t *testing.T, body []byte) {
		data := append([]byte(manifestMagic), body...)
		data = binary.BigEndian.AppendUint32(data, crc32.Checksum(data, castagnoli))
		manifest, err := decodeSharedManifest(data)
		if err != nil {
			return
		}
		again, err := encodeSharedManifest(&manifest)
		if err != nil {
			t.Fatalf("a decoded manifest does not encode: %v", err)
		}
		if got, err := decodeSharedManifest(again); err != nil || !reflect.DeepEqual(got, manifest) {
			t.Fatalf("a manifest re-encoded decodes (%v) otherwise", err)
		}
	})
}

// BenchmarkSharedManifest encodes and decodes the manifest of the objects of
// a filesystem of 100,000 files, and reports its size; and decodes the JSON
// an earlier release wrote of it.
func BenchmarkSharedManifest(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "graph")
	createFilesystemGraph(b, dir, 100000, false)
	store, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	manifest := SnapshotManifest{Version: sharedPackVersion, Snapshot: 1, Packs: []PackInfo{{Number: 0, Size: 1 << 20}}}
	for _, object := range store.Inspect().Objects {
		packed := PackedObject{Object: object}
		if object.BulkLength != 0 {
			packed.Extents = []PackExtent{{Pack: 0, Offset: 4096, Length: int64(object.BulkLength)}}
		}
		manifest.Objects = append(manifest.Objects, packed)
	}
	store.Close()
	data, err := encodeSharedManifest(&manifest)
	if err != nil {
		b.Fatal(err)
	}
	legacy := encodeManifest(b, manifest)
	b.Run("encode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := encodeSharedManifest(&manifest); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(len(data)), "bytes")
	})
	for _, format := range []struct {
		name string
		data []byte
	}{{"decode", data}, {"decode-json", legacy}} {
		b.Run(format.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := decodeSharedManifest(format.data); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(format.data)), "bytes")
		})
	}
}
