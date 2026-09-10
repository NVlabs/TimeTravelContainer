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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedPackPublicationAndRandomAccess(t *testing.T) {
	directory := t.TempDir()
	graph, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	local, err := OpenLocalDataStore(filepath.Join(directory, "local"))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := OpenSharedPackStore(filepath.Join(directory, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("range-readable-object"), 131073)
	id, err := graph.ReserveObject(origin, ObjectKindFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "construction/file", uint64(len(data)), []byte("inode metadata"), nil); err != nil {
		t.Fatal(err)
	}
	metadata, err := graph.CreateObject(origin, "construction/metadata", ObjectKindMetadata, []byte("metadata only"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	// Publication is idempotent after the manifest authority point.
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	manifest, err := shared.Manifest(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Packs) != 1 || len(manifest.Objects) != 2 {
		t.Fatalf("manifest packs=%d objects=%d, want 1 and 2", len(manifest.Packs), len(manifest.Objects))
	}
	var extents int
	for _, object := range manifest.Objects {
		if object.Object.ID == id {
			extents = len(object.Extents)
			for _, extent := range object.Extents {
				if extent.Offset%sharedPackAlignment != 0 || extent.Length > sharedPackExtent {
					t.Fatalf("invalid extent: %+v", extent)
				}
			}
		}
	}
	if extents != 3 {
		t.Fatalf("bulk object has %d extents, want 3", extents)
	}
	reader, length, err := shared.Fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || length != uint64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("fetched length=%d bytes=%d err=%v", length, len(got), err)
	}
	reader, length, err = shared.Fetch(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(reader)
	reader.Close()
	if err != nil || length != 0 || len(got) != 0 {
		t.Fatalf("metadata fetch length=%d bytes=%d err=%v", length, len(got), err)
	}
}

func TestSharedPackManifestLastAndTruncation(t *testing.T) {
	directory := t.TempDir()
	sharedRoot := filepath.Join(directory, "shared")
	shared, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Manifest(1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished manifest error=%v, want not-exist", err)
	}

	graph, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	local, _ := OpenLocalDataStore(filepath.Join(directory, "local"))
	origin, _ := graph.AllocateWorking()
	id, _ := graph.ReserveObject(origin, ObjectKindPage)
	data := bytes.Repeat([]byte{0x5a}, 4096)
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "page", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	manifest, err := shared.Manifest(origin)
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(shared.snapshotDirectory(origin), packName(manifest.Packs[0].Number))
	if err := os.Truncate(packPath, manifest.Packs[0].Size-sharedPackAlignment); err != nil {
		t.Fatal(err)
	}
	uncached, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uncached.Manifest(origin); err == nil {
		t.Fatal("truncated authoritative pack was accepted")
	}
}
