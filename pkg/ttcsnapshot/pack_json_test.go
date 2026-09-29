// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// manifestFixture returns a manifest with every kind of field one holds, some
// empty and some absent.
func manifestFixture(random *rand.Rand, objects int) SnapshotManifest {
	state := checkpointFixture(random, objects)
	manifest := SnapshotManifest{Version: sharedPackVersion, Snapshot: 1, Packs: []PackInfo{{Number: 0, Size: 1 << 20}, {Number: 3, Size: 4096}}}
	keys := make([]string, 0, len(state.Objects))
	for key := range state.Objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		packed := PackedObject{Object: state.Objects[key]}
		switch random.Intn(3) {
		case 0:
			packed.Extents = []PackExtent{{Pack: 3, Offset: 4096, Length: int64(random.Intn(1 << 20))}}
		case 1:
			for extent := 0; extent < 1+random.Intn(3); extent++ {
				packed.Extents = append(packed.Extents, PackExtent{Pack: random.Uint32(), Offset: random.Int63(), Length: random.Int63()})
			}
		}
		manifest.Objects = append(manifest.Objects, packed)
	}
	switch random.Intn(3) {
	case 0:
		manifest.References = map[string]ObjectID{}
	case 1:
		manifest.References = map[string]ObjectID{"root/<main>": {Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 1}, "pin": {}}
	}
	return manifest
}

// encodeManifest encodes manifest as encoding/json encoded it for
// writeAtomicJSON.
func encodeManifest(t testing.TB, manifest SnapshotManifest) []byte {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// checkManifestDecodesAlike fails unless decodeSharedManifest decodes data as
// json.Unmarshal does, and reports whether the canonical decoder took it.
func checkManifestDecodesAlike(t *testing.T, data []byte) bool {
	t.Helper()
	var want SnapshotManifest
	wantErr := json.Unmarshal(data, &want)
	canonical, ok := decodeCanonicalManifest(data)
	if ok && (wantErr != nil || !reflect.DeepEqual(canonical, want)) {
		t.Fatalf("the canonical decoder took what json.Unmarshal decodes otherwise (%v):\n%.300s", wantErr, data)
	}
	got, err := decodeSharedManifest(data)
	if (err == nil) != (wantErr == nil) || err == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded (%v) differently from json.Unmarshal (%v):\n%.300s", err, wantErr, data)
	}
	return ok
}

// A manifest as writeAtomicJSON writes it decodes, without encoding/json, to
// what json.Unmarshal decodes; any other form goes to encoding/json.
func TestDecodeSharedManifest(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	manifests := []SnapshotManifest{{}, {Objects: []PackedObject{}}}
	for round := 0; round < 20; round++ {
		manifests = append(manifests, manifestFixture(random, random.Intn(200)))
	}
	for index, manifest := range manifests {
		data := encodeManifest(t, manifest)
		if !checkManifestDecodesAlike(t, data) {
			t.Fatalf("manifest %d: the canonical decoder refused writeAtomicJSON's manifest", index)
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, "", "\t"); err != nil {
			t.Fatal(err)
		}
		if !checkManifestDecodesAlike(t, indented.Bytes()) {
			t.Fatalf("manifest %d: the canonical decoder refused an indented manifest", index)
		}
	}
	fixture := manifestFixture(random, 20)
	fixture.References = map[string]ObjectID{"root": {Snapshot: 1, Kind: ObjectKindMetadata, Ordinal: 1}}
	fixture.Objects[0].Extents = []PackExtent{{Pack: 3, Offset: 4096, Length: 7}}
	text := string(encodeManifest(t, fixture))
	for _, variant := range []string{
		strings.Replace(text, `"objects"`, `"Objects"`, 1),
		strings.Replace(text, `"version"`, `"Version"`, 1),
		strings.Replace(text, `"object":`, `"Object":`, 1),
		strings.Replace(text, `"object":`, `"unknown":1,"object":`, 1),
		strings.Replace(text, `"object":`, `"object":null,"object":`, 1),
		strings.Replace(text, `"extents":[`, `"extents":null,"extents":[`, 1),
		strings.Replace(text, `"extents":[`, `"extents":[],"x":[`, 1),
		strings.Replace(text, `"offset":4096`, `"offset":-4096`, 1),
		strings.Replace(text, `"offset":4096`, `"offset":4096.0`, 1),
		strings.Replace(text, `"offset":4096`, `"offset":9223372036854775808`, 1),
		strings.Replace(text, `"pack":3`, `"pack":4294967296`, 1),
		strings.Replace(text, `"pack":3`, `"pack":3,"pack":3`, 1),
		strings.Replace(text, `"length":7`, `"length":null`, 1),
		strings.Replace(text, `"references":{`, `"references":{"root":{"Snapshot":2},`, 1),
		strings.Replace(text, `"references":{`, `"references":{"ré":{"Snapshot":2},`, 1),
		strings.Replace(text, `"references":{`, `"references":{"x":null,`, 1),
		strings.Replace(text, `"references":{`, `"references":null,"x":{`, 1),
		strings.Replace(text, `"snapshot":1`, `"snapshot":-1`, 1),
		strings.Replace(text, `"packs":[`, `"packs":null,"packs":[`, 1),
		strings.Replace(text, `"State":"live"`, `"State":"livé"`, 1),
		strings.Replace(text, `{"object":`, `null,{"object":`, 1),
		text + " x",
		"null",
		"{}",
		"[]",
		"",
	} {
		if variant == text {
			t.Fatalf("a variant is the manifest itself:\n%.300s", text)
		}
		checkManifestDecodesAlike(t, []byte(variant))
	}
}

// FuzzDecodeSharedManifest checks that whatever the canonical decoder takes,
// json.Unmarshal decodes alike.
func FuzzDecodeSharedManifest(f *testing.F) {
	f.Add(encodeManifest(f, manifestFixture(rand.New(rand.NewSource(2)), 6)))
	f.Add([]byte(`{"objects":[{"object":{"ID":{"Snapshot":1,"Ordinal":2,"Kind":3},"Data":"","Children":[]},"extents":[{"pack":0,"offset":1,"length":2}]}],"references":{"a":{"Kind":1}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkManifestDecodesAlike(t, data)
	})
}

// A manifest encodes as encoding/json encoded it for writeAtomicJSON, byte
// for byte.
func TestEncodeSharedManifest(t *testing.T) {
	random := rand.New(rand.NewSource(9))
	manifests := []SnapshotManifest{{}, {Objects: []PackedObject{}, References: map[string]ObjectID{}, Packs: []PackInfo{}}}
	for round := 0; round < 40; round++ {
		manifest := manifestFixture(random, random.Intn(100))
		for index := range manifest.Objects {
			object := &manifest.Objects[index].Object
			if random.Intn(4) == 0 {
				object.State = ObjectState(oddStrings[random.Intn(len(oddStrings))])
			}
			if random.Intn(4) == 0 {
				object.SourceCatalog = oddStrings[1+random.Intn(len(oddStrings)-1)]
			}
			if random.Intn(8) == 0 {
				manifest.Objects[index].Extents = []PackExtent{}
			}
		}
		if random.Intn(2) == 0 {
			manifest.References = map[string]ObjectID{oddStrings[random.Intn(len(oddStrings))]: {Snapshot: 1, Kind: 1, Ordinal: 2}, "root/<&>": {Snapshot: 1, Kind: 1, Ordinal: 3}}
		}
		manifests = append(manifests, manifest)
	}
	for index := range manifests {
		want := encodeManifest(t, manifests[index])
		got, err := encodeSharedManifest(&manifests[index])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("manifest %d encodes (%v) as\n%.600s\nwant\n%.600s", index, err, got, want)
		}
	}
}
