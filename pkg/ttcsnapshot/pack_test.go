// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	// A caller's manifest is its own: the reads below go by the store's.
	for index := range manifest.Objects {
		manifest.Objects[index].Object.BulkLength++
		for extent := range manifest.Objects[index].Extents {
			manifest.Objects[index].Extents[extent].Offset++
		}
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

// A handoff retried after a failure resumes where the failure left it,
// frozen: the pack's manifest, which the retry does not replace, holds the
// baseline that the handoff's recorded mutations follow, and the receiver,
// importing both, counts exactly the provider's references. Canceling the
// handoff instead loses its mutations, as the retry freezes a new baseline
// that the manifest does not hold; so publishers never cancel a handoff once
// its pack exists.
func TestSharedPackHandoffRetryResumesTheFrozenHandoff(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
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
			if err := graph.SetRoot("branch/main", object); err != nil {
				t.Fatal(err)
			}
			if err := graph.DeletePin("publish/root"); err != nil {
				t.Fatal(err)
			}
			if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
				t.Fatal(err)
			}
			// The first attempt writes the pack, records a mutation, freezes,
			// and fails.
			if err := shared.PublishHandoff(context.Background(), graph, origin, local); err != nil {
				t.Fatal(err)
			}
			if err := graph.SetRoot("branch/fork", object); err != nil {
				t.Fatal(err)
			}
			if _, _, err := graph.FreezeSnapshotLedger(origin); err != nil {
				t.Fatal(err)
			}
			if cancel {
				if err := graph.UnfreezeSnapshotLedger(origin); err != nil {
					t.Fatal(err)
				}
				if err := graph.DeleteRoot("branch/main"); err != nil {
					t.Fatal(err)
				}
			}
			// The retry, and its receiver.
			if err := shared.PublishHandoff(context.Background(), graph, origin, local); err != nil {
				t.Fatal(err)
			}
			if _, _, err := graph.FreezeSnapshotLedger(origin); err != nil {
				t.Fatal(err)
			}
			changes, _, frozen, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
			if err != nil || !frozen {
				t.Fatalf("retried handoff changes: frozen=%t err=%v", frozen, err)
			}
			manifest, err := shared.Manifest(origin)
			if err != nil {
				t.Fatal(err)
			}
			objects := make([]Object, len(manifest.Objects))
			for index, packed := range manifest.Objects {
				objects[index] = packed.Object
			}
			receiver := NewReferenceLedger()
			if err := receiver.ImportOrigin(origin, objects, manifest.References); err != nil {
				t.Fatal(err)
			}
			if err := receiver.ApplyHandoff(origin, 0, changes); err != nil {
				t.Fatal(err)
			}
			_, provider, err := graph.SnapshotLedger(origin)
			if err != nil {
				t.Fatal(err)
			}
			received := receiver.Inspect().References
			if exact := sameObjectReferences(received, provider); exact == cancel {
				t.Fatalf("receiver references=%+v, provider=%+v: exact=%t after cancel=%t", received, provider, exact, cancel)
			}
		})
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
	// The ledger keeps no object's data: compaction takes it from the
	// manifest.
	meta, err := graph.CreateObject(origin, "meta", ObjectKindMetadata, []byte("an inode record"), []Edge{{Slot: 0, Child: live}})
	if err != nil {
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
	otherNode, err := OpenSharedPackStore(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cached, err := otherNode.Manifest(origin); err != nil || len(cached.Objects) != 3 {
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
	if !ok || len(liveObjects) != 2 || !slices.ContainsFunc(liveObjects, func(object Object) bool { return object.ID == live }) ||
		!slices.ContainsFunc(liveObjects, func(object Object) bool { return object.ID == meta && len(object.Data) == 0 }) {
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
	if stats.BeforeObjects != 3 || stats.AfterObjects != 2 || stats.AfterBytes >= stats.BeforeBytes {
		t.Fatalf("compaction stats=%+v", stats)
	}
	compacted, err := shared.Manifest(origin)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range compacted.Objects {
		if object.Object.ID == meta && string(object.Object.Data) != "an inode record" {
			t.Fatalf("compaction wrote %s with data %q", meta, object.Object.Data)
		}
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
	if reopened, err := otherNode.Manifest(origin); err != nil || len(reopened.Objects) != 2 {
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

// A store keeps the manifests it has read decoded only within its bound, the
// least recently used forgotten first.
// A view opens a snapshot's manifest once, for all the objects it reads of
// the snapshot.
func TestSharedPackViewOpensEachManifestOnce(t *testing.T) {
	shared, err := OpenSharedPackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph := openTestStore(t)
	data, err := OpenLocalDataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := graph.CreateObject(origin, "root/first", ObjectKindMetadata, []byte("first"), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graph.CreateObject(origin, "root/second", ObjectKindMetadata, []byte("second"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if err := shared.Publish(context.Background(), graph, origin, data); err != nil {
		t.Fatal(err)
	}
	view := shared.View()
	if object, err := view.Object(first); err != nil || string(object.Data) != "first" {
		t.Fatalf("view object: %+v, %v", object, err)
	}
	if err := os.Remove(filepath.Join(shared.snapshotDirectory(origin), sharedManifestName)); err != nil {
		t.Fatal(err)
	}
	if object, err := view.Object(second); err != nil || string(object.Data) != "second" {
		t.Fatalf("view object after its manifest was read: %+v, %v", object, err)
	}
	if _, err := shared.Object(second); err == nil {
		t.Fatal("the store read an object of a removed manifest")
	}
}

func TestSharedPackManifestCacheIsBounded(t *testing.T) {
	shared, err := OpenSharedPackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph := openTestStore(t)
	data, err := OpenLocalDataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var origins []SnapshotID
	var largest int64
	for index := 0; index < 3; index++ {
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
		if err := shared.Publish(context.Background(), graph, origin, data); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(shared.snapshotDirectory(origin), sharedManifestName))
		if err != nil {
			t.Fatal(err)
		}
		largest = max(largest, info.Size())
		origins = append(origins, origin)
	}
	// The publisher keeps the manifest it wrote, the one a reader decodes.
	reader, err := OpenSharedPackStore(shared.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range origins {
		shared.mu.Lock()
		published, ok := shared.manifests[origin]
		shared.mu.Unlock()
		if !ok {
			t.Fatalf("the publisher of %d did not keep its manifest", origin)
		}
		decoded, err := reader.Manifest(origin)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cloneManifest(published.manifest), decoded) {
			t.Fatalf("published manifest of %d\n%+v\ndecodes as\n%+v", origin, published.manifest, decoded)
		}
	}
	shared.mu.Lock()
	for _, origin := range origins {
		shared.forgetManifestLocked(origin)
	}
	shared.mu.Unlock()
	shared.manifestLimit = 2 * largest
	cached := func(want ...SnapshotID) {
		t.Helper()
		shared.mu.Lock()
		defer shared.mu.Unlock()
		if len(shared.manifests) != len(want) {
			t.Fatalf("cached %d manifests, want %v", len(shared.manifests), want)
		}
		for _, origin := range want {
			if _, ok := shared.manifests[origin]; !ok {
				t.Fatalf("manifest of %d is not cached, want %v", origin, want)
			}
		}
	}
	read := func(origin SnapshotID) {
		t.Helper()
		if _, err := shared.manifest(origin); err != nil {
			t.Fatal(err)
		}
	}
	a, b, c := origins[0], origins[1], origins[2]
	read(a)
	read(b)
	cached(a, b)
	read(c)
	cached(b, c)
	read(b)
	read(a)
	cached(a, b)
}

// The cache forgets a manifest none has read for a while, and one whose
// origin was retired, whose manifest is gone.
func TestSharedPackManifestCacheExpires(t *testing.T) {
	shared, err := OpenSharedPackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph := openTestStore(t)
	data, err := OpenLocalDataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var origins []SnapshotID
	for index := 0; index < 3; index++ {
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
		if err := shared.Publish(context.Background(), graph, origin, data); err != nil {
			t.Fatal(err)
		}
		origins = append(origins, origin)
	}
	idle, used, retired := origins[0], origins[1], origins[2]
	cached := func(origin SnapshotID) bool {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		_, ok := shared.manifests[origin]
		return ok
	}
	shared.mu.Lock()
	entry := shared.manifests[idle]
	entry.usedAt = time.Now().Add(-10 * time.Minute)
	shared.manifests[idle] = entry
	shared.mu.Unlock()
	if _, err := shared.manifest(used); err != nil {
		t.Fatal(err)
	}
	shared.ExpireManifests(2 * time.Minute)
	if cached(idle) || !cached(used) {
		t.Fatalf("after expiry, the idle manifest is cached %t and the one read %t, want false and true", cached(idle), cached(used))
	}
	if err := os.Remove(filepath.Join(shared.snapshotDirectory(retired), sharedManifestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.manifest(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a removed manifest: %v", err)
	}
	if cached(retired) {
		t.Fatal("the manifest of a retired origin is still cached")
	}
}

// validateManifest takes a consistent manifest in any order, and sorts it;
// it refuses each inconsistency.
func TestValidateManifest(t *testing.T) {
	store, err := OpenSharedPackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const snapshot = SnapshotID(3)
	directory := store.snapshotDirectory(snapshot)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	pack, err := os.Create(filepath.Join(directory, packName(0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := writePackHeader(pack, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := pack.Truncate(2 * sharedPackAlignment); err != nil {
		t.Fatal(err)
	}
	pack.Close()
	root := ObjectID{Snapshot: snapshot, Kind: ObjectKindMetadata, Ordinal: 1}
	inode := ObjectID{Snapshot: snapshot, Kind: ObjectKindInode, Ordinal: 2}
	file := ObjectID{Snapshot: snapshot, Kind: ObjectKindFile, Ordinal: 3}
	foreign := ObjectID{Snapshot: snapshot + 1, Kind: ObjectKindFile, Ordinal: 3}
	consistent := func() SnapshotManifest {
		return SnapshotManifest{
			Version: sharedPackVersion, Snapshot: snapshot,
			Packs: []PackInfo{{Number: 0, Size: 2 * sharedPackAlignment}},
			Objects: []PackedObject{
				{Object: Object{ID: file, State: ObjectLive, Refs: 1, BulkLength: 10}, Extents: []PackExtent{{Pack: 0, Offset: sharedPackAlignment, Length: 10}}},
				{Object: Object{ID: inode, State: ObjectLive, Refs: 1, Children: []Edge{{Slot: 0, Child: file}, {Slot: 1, Child: foreign}}}},
				{Object: Object{ID: root, State: ObjectLive, Refs: 2, Children: []Edge{{Slot: 0, Child: inode}}}},
			},
			References: map[string]ObjectID{"filesystem/main": root, "pin/main": root},
		}
	}
	byID := func(a, b PackedObject) int { return compareObjectIDs(a.Object.ID, b.Object.ID) }
	manifest := consistent()
	slices.SortFunc(manifest.Objects, func(a, b PackedObject) int { return byID(b, a) })
	if err := store.validateManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(manifest.Objects, byID) {
		t.Fatalf("the manifest's objects are not sorted: %+v", manifest.Objects)
	}
	for name, corrupt := range map[string]func(*SnapshotManifest){
		"repeated object": func(m *SnapshotManifest) { m.Objects = append(m.Objects, m.Objects[0]) },
		"foreign object": func(m *SnapshotManifest) {
			m.Objects[0].Object.ID = foreign
			m.Objects[1].Object.Children = m.Objects[1].Object.Children[1:]
		},
		"absent child":        func(m *SnapshotManifest) { m.Objects = m.Objects[1:] },
		"too few references":  func(m *SnapshotManifest) { m.Objects[2].Object.Refs = 3 },
		"too many references": func(m *SnapshotManifest) { m.Objects[2].Object.Refs = 1 },
		"absent reference": func(m *SnapshotManifest) {
			m.References["pin/main"] = ObjectID{Snapshot: snapshot, Kind: ObjectKindMetadata, Ordinal: 9}
		},
		"foreign reference":      func(m *SnapshotManifest) { m.References["pin/main"] = foreign },
		"extent beyond its pack": func(m *SnapshotManifest) { m.Objects[0].Extents[0].Offset = 2 * sharedPackAlignment },
		"short extents":          func(m *SnapshotManifest) { m.Objects[0].Object.BulkLength = 11 },
	} {
		manifest := consistent()
		corrupt(&manifest)
		if err := store.validateManifest(&manifest); err == nil {
			t.Errorf("%s: the manifest was taken", name)
		}
	}
}
