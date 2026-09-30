// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"reflect"
	"testing"
)

func TestArchive(t *testing.T) {
	entries := []archiveEntry{
		{name: "gfxstream_renderer.bin", data: []byte("renderer")},
		{name: "empty", data: []byte{}},
		{name: archiveResources, data: encodeResources([]archivedResource{{id: 7, blob: true}, {id: 2}})},
	}
	got, err := decodeArchive(encodeArchive(entries))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Errorf("decoded %v, want %v", got, entries)
	}
	resources, err := decodeResources(got[2].data)
	if err != nil {
		t.Fatal(err)
	}
	// Encoding sorts them by ID.
	if want := []archivedResource{{id: 2}, {id: 7, blob: true}}; !reflect.DeepEqual(resources, want) {
		t.Errorf("resources %v, want %v", resources, want)
	}
}

func TestArchiveRejects(t *testing.T) {
	valid := encodeArchive([]archiveEntry{{name: "a", data: []byte("x")}})
	for _, test := range []struct {
		name    string
		archive []byte
	}{
		{"empty", nil},
		{"another format", []byte("NOTGFXS1\x00\x00\x00\x00")},
		{"truncated", valid[:len(valid)-1]},
		{"trailing bytes", append(append([]byte{}, valid...), 0)},
		{"a path", encodeArchive([]archiveEntry{{name: "../a"}})},
		{"a parent", encodeArchive([]archiveEntry{{name: ".."}})},
		{"a duplicate", encodeArchive([]archiveEntry{{name: "a"}, {name: "a"}})},
	} {
		if _, err := decodeArchive(test.archive); err == nil {
			t.Errorf("%s: decoded", test.name)
		}
	}
	if _, err := decodeResources([]byte{1, 2, 3}); err == nil {
		t.Error("decoded a malformed resource list")
	}
}
