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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	for _, test := range []struct {
		name   string
		offset int64
		length int
	}{
		{name: "unaligned-4k", offset: 37, length: 4096},
		{name: "cross-extent", offset: sharedPackExtent - 127, length: 8192},
		{name: "third-extent", offset: 2*sharedPackExtent + 91, length: 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer := make([]byte, test.length)
			n, err := shared.ReadObjectRange(context.Background(), id, buffer, test.offset)
			if err != nil || n != len(buffer) {
				t.Fatalf("ReadObjectRange(%d, %d) = %d, %v", test.offset, test.length, n, err)
			}
			if !bytes.Equal(buffer, data[test.offset:test.offset+int64(test.length)]) {
				t.Fatal("range contents differ")
			}
		})
	}
	object, err := shared.Object(id)
	if err != nil || object.ID != id || object.BulkLength != uint64(len(data)) {
		t.Fatalf("Object(%s) = %+v, %v", id, object, err)
	}
	objects, err := shared.Objects([]ObjectID{id, metadata, id})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[id].BulkLength != uint64(len(data)) || objects[metadata].BulkLength != 0 {
		t.Fatalf("Objects() = %+v, want both published objects", objects)
	}
	missing := ObjectID{Snapshot: origin, Kind: ObjectKindFile, Ordinal: 1 << 62}
	if _, err := shared.Objects([]ObjectID{missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Objects(missing) error = %v, want os.ErrNotExist", err)
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

func TestSharedPackHandoffUsesDurableBaseline(t *testing.T) {
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
	object, err := graph.CreateObject(origin, "publish/root", ObjectKindMetadata, []byte("metadata"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := shared.PublishHandoff(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("publish/root"); err != nil {
		t.Fatal(err)
	}
	manifest, err := shared.Manifest(origin)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.References["pin/publish/root"] != object {
		t.Fatalf("manifest did not retain original handoff baseline: %+v", manifest.References)
	}
	changes, cursor, frozen, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 1 || frozen || len(changes) != 1 || changes[0].Retain || changes[0].Object != object {
		t.Fatalf("handoff changes=%+v cursor=%d frozen=%t", changes, cursor, frozen)
	}
	if collected, err := graph.Collect(1); err != nil || collected != 0 {
		t.Fatalf("active pack handoff allowed collection: (%d,%v)", collected, err)
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

func TestSharedPackRetireWaitsForReaders(t *testing.T) {
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
	sharedRoot := filepath.Join(directory, "shared")
	shared, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := graph.AllocateWorking()
	id, _ := graph.ReserveObject(origin, ObjectKindMemoryChunk)
	data := bytes.Repeat([]byte{0xa5}, 1<<20)
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "chunk", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	reader, _, err := shared.Fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := shared.RetirementReady(origin); err != nil || ready {
		t.Fatalf("RetirementReady with reader = (ready=%t, err=%v), want not ready", ready, err)
	}
	if busy, err := shared.Retire(origin); err != nil || !busy {
		t.Fatalf("Retire with reader = (busy=%t, err=%v), want busy", busy, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if ready, err := shared.RetirementReady(origin); err != nil || !ready {
		t.Fatalf("RetirementReady after close = (ready=%t, err=%v), want ready", ready, err)
	}
	if busy, err := shared.Retire(origin); err != nil || busy {
		t.Fatalf("Retire after close = (busy=%t, err=%v)", busy, err)
	}
	if _, err := shared.Manifest(origin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired manifest error=%v, want not-exist", err)
	}
	crashResidue := filepath.Join(sharedRoot, "snapshots", ".retired-"+snapshotKey(origin)+"-crash")
	if err := os.MkdirAll(crashResidue, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crashResidue, "pack.data"), []byte("residue"), 0600); err != nil {
		t.Fatal(err)
	}
	if busy, err := shared.CompleteRetirement(origin); err != nil || busy {
		t.Fatalf("CompleteRetirement = (busy=%t, err=%v)", busy, err)
	}
	if _, err := os.Stat(crashResidue); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement residue stat error=%v, want not-exist", err)
	}
	// A publisher interrupted immediately after creating its namespace has no
	// manifest for Retire to inspect. Permanent retirement still authorizes
	// removal of the entire incomplete namespace.
	incomplete := origin + 1
	if err := os.MkdirAll(shared.snapshotDirectory(incomplete), 0700); err != nil {
		t.Fatal(err)
	}
	if busy, err := shared.CompleteRetirement(incomplete); err != nil || busy {
		t.Fatalf("CompleteRetirement(incomplete) = (busy=%t, err=%v)", busy, err)
	}
	if _, err := os.Stat(shared.snapshotDirectory(incomplete)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete snapshot namespace stat error=%v, want not-exist", err)
	}
	err = filepath.WalkDir(filepath.Join(sharedRoot, "snapshots"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && len(entry.Name()) > 4 && entry.Name()[:4] == ".nfs" {
			t.Errorf("unexpected NFS tombstone %q", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCanceledSharedPackPublicationLeavesRecoverableNamespace(t *testing.T) {
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
	id, err := graph.ReserveObject(origin, ObjectKindMemoryChunk)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xa5}, 1<<20)
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "chunk", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shared.Publish(ctx, graph, origin, local); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish(canceled) error=%v, want context cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(shared.snapshotDirectory(origin), sharedManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled publication manifest stat error=%v, want not-exist", err)
	}
	if err := shared.Publish(context.Background(), graph, origin, local); err != nil {
		t.Fatalf("retrying canceled publication: %v", err)
	}
	if _, err := shared.Manifest(origin); err != nil {
		t.Fatalf("opening retried publication: %v", err)
	}
}

type blockingObjectSource struct {
	ObjectSource
	entered chan struct{}
	release chan struct{}
}

func (source *blockingObjectSource) Fetch(ctx context.Context, id ObjectID) (io.ReadCloser, uint64, error) {
	select {
	case source.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case <-source.release:
	}
	return source.ObjectSource.Fetch(ctx, id)
}

func TestConcurrentCanceledSharedPackPublicationCannotRemoveCommit(t *testing.T) {
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
	id, err := graph.ReserveObject(origin, ObjectKindMemoryChunk)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0x5c}, 1<<20)
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "chunk", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}

	blocked := &blockingObjectSource{ObjectSource: local, entered: make(chan struct{}, 1), release: make(chan struct{})}
	published := make(chan error, 1)
	go func() {
		published <- shared.Publish(context.Background(), graph, origin, blocked)
	}()
	<-blocked.entered
	competing := &blockingObjectSource{ObjectSource: local, entered: make(chan struct{}, 1), release: make(chan struct{})}
	canceled, cancel := context.WithCancel(context.Background())
	competed := make(chan error, 1)
	go func() {
		competed <- shared.Publish(canceled, graph, origin, competing)
	}()
	// Without publication serialization, the competitor reaches Fetch and a
	// subsequent cancellation removes the directory out from under the first
	// publisher. With the lease, it remains outside the data path until the
	// cancellation interrupts its lock wait.
	select {
	case <-competing.entered:
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-competed; !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent canceled Publish() error=%v, want context cancellation", err)
	}
	close(blocked.release)
	if err := <-published; err != nil {
		t.Fatalf("winning Publish(): %v", err)
	}
	reader, length, err := shared.Fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || length != uint64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("published object length=%d bytes=%d err=%v", length, len(got), errors.Join(readErr, closeErr))
	}
}

func TestSharedPackDiscardHandoffWaitsForReaders(t *testing.T) {
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
	sharedRoot := filepath.Join(directory, "shared")
	shared, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := graph.AllocateWorking()
	id, _ := graph.ReserveObject(origin, ObjectKindMemoryChunk)
	data := bytes.Repeat([]byte{0x3c}, 1<<20)
	if err := local.Install(id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(id, "chunk", uint64(len(data)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := shared.PublishHandoff(context.Background(), graph, origin, local); err != nil {
		t.Fatal(err)
	}
	reader, _, err := shared.Fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := shared.DiscardHandoff(origin); err != nil || !busy {
		t.Fatalf("DiscardHandoff with reader = (busy=%t, err=%v), want busy", busy, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if busy, err := shared.DiscardHandoff(origin); err != nil || busy {
		t.Fatalf("DiscardHandoff after close = (busy=%t, err=%v)", busy, err)
	}
	if _, err := shared.Manifest(origin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded manifest error=%v, want not-exist", err)
	}
	if busy, err := shared.DiscardHandoff(origin); err != nil || busy {
		t.Fatalf("idempotent DiscardHandoff = (busy=%t, err=%v)", busy, err)
	}
}

func TestSharedPackCompactionUsesLedgerViewAndWaitsForReaders(t *testing.T) {
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
	sharedRoot := filepath.Join(directory, "shared")
	shared, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	install := func(pin string, value byte) ObjectID {
		id, err := graph.ReserveObject(origin, ObjectKindFile)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{value}, 1<<20)
		if err := local.Install(id, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if err := graph.CommitReservedObject(id, pin, uint64(len(payload)), nil, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dead := install("dead", 0xd0)
	live := install("live", 0x1a)
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
	otherNode, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cached, err := otherNode.Manifest(origin); err != nil || len(cached.Objects) != 2 {
		t.Fatalf("other-node initial manifest=%+v err=%v", cached, err)
	}
	oldPack := filepath.Join(shared.snapshotDirectory(origin), packName(manifest.Packs[0].Number))
	objects := make([]Object, len(manifest.Objects))
	for i := range manifest.Objects {
		objects[i] = manifest.Objects[i].Object
	}
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, objects, manifest.References); err != nil {
		t.Fatal(err)
	}
	var deadReference string
	for name, object := range manifest.References {
		if object == dead {
			deadReference = name
			break
		}
	}
	if deadReference == "" {
		t.Fatal("dead object has no publication reference")
	}
	if err := ledger.ApplyReference("drop-dead", deadReference, dead, false); err != nil {
		t.Fatal(err)
	}
	if collected, err := ledger.Collect(16); err != nil || collected != 1 {
		t.Fatalf("Collect()=(%d,%v), want (1,nil)", collected, err)
	}
	liveObjects, liveReferences, ok := ledger.OriginManifest(origin)
	if !ok || len(liveObjects) != 1 || liveObjects[0].ID != live {
		t.Fatalf("live origin view=%+v ok=%t", liveObjects, ok)
	}

	reader, _, err := shared.Fetch(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	if _, busy, err := shared.Compact(origin, liveObjects, liveReferences); err != nil || !busy {
		t.Fatalf("Compact with reader=(busy=%t,err=%v), want busy", busy, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	stats, busy, err := shared.Compact(origin, liveObjects, liveReferences)
	if err != nil || busy {
		t.Fatalf("Compact=(%+v,busy=%t,err=%v)", stats, busy, err)
	}
	if stats.BeforeObjects != 2 || stats.AfterObjects != 1 || stats.AfterBytes >= stats.BeforeBytes {
		t.Fatalf("compaction stats=%+v", stats)
	}
	if _, err := os.Stat(oldPack); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old pack survived compaction: %v", err)
	}
	reader, length, err := shared.Fetch(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || length != 1<<20 || len(got) != 1<<20 || got[0] != 0x1a {
		t.Fatalf("compacted live read length=%d bytes=%d err=%v", length, len(got), errors.Join(readErr, closeErr))
	}
	if _, _, err := shared.Fetch(context.Background(), dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collected object fetch error=%v, want not-exist", err)
	}
	if reopened, err := otherNode.Manifest(origin); err != nil || len(reopened.Objects) != 1 || reopened.Objects[0].Object.ID != live {
		t.Fatalf("reopened compacted manifest=%+v err=%v", reopened, err)
	}
	residue := filepath.Join(shared.snapshotDirectory(origin), packName(0x99))
	if err := os.WriteFile(residue, []byte("post-manifest crash residue"), 0600); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(shared.snapshotDirectory(origin), "operator-note")
	if err := os.WriteFile(unknown, []byte("leave me"), 0600); err != nil {
		t.Fatal(err)
	}
	if removed, busy, err := shared.Reconcile(origin); err != nil || busy || removed != 1 {
		t.Fatalf("Reconcile()=(removed=%d,busy=%t,err=%v)", removed, busy, err)
	}
	if _, err := os.Stat(residue); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete finalized pack survived reconciliation: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown file was removed: %v", err)
	}
}

func TestSharedPackManifestsEnumeratesOnlyCommittedOrigins(t *testing.T) {
	sharedRoot := t.TempDir()
	shared, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	graph := openTestStore(t)
	data, err := OpenLocalDataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var origins []SnapshotID
	for index := 0; index < 2; index++ {
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		object, err := graph.CreateObject(origin, fmt.Sprintf("root/%d", origin), ObjectKindMetadata, []byte{byte(origin)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.SetRoot(fmt.Sprintf("snapshot/%d", origin), object); err != nil {
			t.Fatal(err)
		}
		if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
			t.Fatal(err)
		}
		origins = append(origins, origin)
	}
	for index := len(origins) - 1; index >= 0; index-- {
		if err := shared.Publish(context.Background(), graph, origins[index], data); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(sharedRoot, "snapshots", "operator-notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sharedRoot, "snapshots", snapshotKey(origins[1]+1)), 0700); err != nil {
		t.Fatal(err)
	}
	manifests, err := shared.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 || manifests[0].Snapshot != origins[0] || manifests[1].Snapshot != origins[1] {
		t.Fatalf("Manifests()=%+v, want snapshots %d and %d", manifests, origins[0], origins[1])
	}
}
