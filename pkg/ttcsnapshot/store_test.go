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
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestSnapshotIDsNeverReuse(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("snapshot IDs=(%d,%d), want (1,2)", first, second)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	third, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if third != 3 {
		t.Fatalf("snapshot ID after reopen=%d, want 3", third)
	}
}

func TestOpenRejectsUnmarkedLegacyGraphWithoutModification(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "graph.journal")
	contents := []byte("legacy graph data")
	if err := os.WriteFile(legacy, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted an unmarked legacy graph")
	}
	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatalf("legacy graph changed from %q to %q", contents, got)
	}
	if _, err := os.Stat(filepath.Join(dir, headFileName)); !os.IsNotExist(err) {
		t.Fatalf("failed open created graph head: %v", err)
	}
}

func TestCollectEmptySnapshotsNeverReusesIdentity(t *testing.T) {
	store := openTestStore(t)
	first, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	retained, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if collected, err := store.CollectEmptySnapshots([]SnapshotID{retained}); err != nil || collected != 1 {
		t.Fatalf("CollectEmptySnapshots collected=%d err=%v, want 1", collected, err)
	}
	if _, ok := store.Snapshot(first); ok {
		t.Fatalf("empty snapshot %d survived collection", first)
	}
	if _, ok := store.Snapshot(retained); !ok {
		t.Fatalf("retained snapshot %d was collected", retained)
	}
	next, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if next != retained+1 {
		t.Fatalf("snapshot identity after collection=%d, want %d", next, retained+1)
	}
}

func TestExternallyCoordinatedStoresReload(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	one, err := first.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Reload(); err != nil {
		t.Fatal(err)
	}
	two, err := second.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if two != one+1 {
		t.Fatalf("second snapshot = %d, want %d", two, one+1)
	}
	if err := first.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := first.Snapshot(two); !ok {
		t.Fatalf("first store did not observe snapshot %d after reload", two)
	}
}

func TestExternallyCoordinatedCheckpointSwitchesGeneration(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	origin, err := writer.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CheckpointExternallyCoordinated(); err != nil {
		t.Fatal(err)
	}
	if writer.generation != 2 {
		t.Fatalf("checkpoint generation=%d, want 2", writer.generation)
	}
	if _, err := os.Stat(filepath.Join(dir, graphJournalName(1))); !os.IsNotExist(err) {
		t.Fatalf("retired generation journal survived: %v", err)
	}
	if err := reader.Reload(); err != nil {
		t.Fatal(err)
	}
	if reader.generation != 2 {
		t.Fatalf("reader generation=%d, want 2", reader.generation)
	}
	if _, ok := reader.Snapshot(origin); !ok {
		t.Fatalf("reader lost snapshot %d across generation switch", origin)
	}
	if err := reader.ReloadForWrite(); err != nil {
		t.Fatal(err)
	}
	next, err := reader.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if next != origin+1 {
		t.Fatalf("snapshot after generation switch=%d, want %d", next, origin+1)
	}
	if err := writer.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := writer.Snapshot(next); !ok {
		t.Fatalf("writer did not observe post-checkpoint snapshot %d", next)
	}
}

func TestExternalReaderSurvivesConcurrentAppendAndCompaction(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	const snapshots = 200
	done := make(chan struct{})
	errorsCh := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := reader.Reload(); err != nil {
				select {
				case errorsCh <- err:
				default:
				}
				return
			}
			if err := reader.Verify(); err != nil {
				select {
				case errorsCh <- err:
				default:
				}
				return
			}
		}
	}()
	for index := 0; index < snapshots; index++ {
		if err := writer.ReloadForWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.AllocateWorking(); err != nil {
			t.Fatal(err)
		}
		if index%7 == 6 {
			if err := writer.CheckpointExternallyCoordinated(); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(done)
	wait.Wait()
	select {
	case err := <-errorsCh:
		t.Fatal(err)
	default:
	}
	if err := reader.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := reader.Stats().Snapshots; got != snapshots {
		t.Fatalf("reader snapshots=%d, want %d", got, snapshots)
	}
}

func TestRootSharingAndZeroCountCascade(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.CreateObject(origin, "build/page", ObjectKindPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.CreateObject(origin, "build/node", ObjectKindMemoryNode, []byte{1}, []Edge{{Slot: 7, Child: page}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/page"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot("family@main/memory", node); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot("family@child/memory", node); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/node"); err != nil {
		t.Fatal(err)
	}
	gotNode, _ := store.Object(node)
	gotPage, _ := store.Object(page)
	if gotNode.Refs != 2 || gotPage.Refs != 1 {
		t.Fatalf("shared refs: node=%d page=%d, want 2/1", gotNode.Refs, gotPage.Refs)
	}
	if err := store.DeleteRoot("family@main/memory"); err != nil {
		t.Fatal(err)
	}
	gotNode, _ = store.Object(node)
	if gotNode.Refs != 1 {
		t.Fatalf("node refs after first delete=%d, want 1", gotNode.Refs)
	}
	if err := store.DeleteRoot("family@child/memory"); err != nil {
		t.Fatal(err)
	}
	gotNode, _ = store.Object(node)
	if gotNode.State != ObjectReclaiming || gotNode.Refs != 0 {
		t.Fatalf("node after final delete=%+v", gotNode)
	}
	if err := store.SetPin("resurrect", node); err == nil {
		t.Fatal("retained a zero-count reclaiming object")
	}
	if count, err := store.Collect(8); err != nil || count != 2 {
		t.Fatalf("Collect()=(%d,%v), want (2,nil)", count, err)
	}
	if stats := store.Stats(); stats.Objects != 0 || stats.Roots != 0 {
		t.Fatalf("stats after collection=%+v", stats)
	}
}

func TestCheckpointAndTruncatedTailRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.CreateObject(origin, "build/object", ObjectKindMetadata, []byte("stable"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot("root", object); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/object"); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	journal := store.journal.Name()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], 100)
	if _, err := file.Write(append(length[:], []byte("partial")...)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, ok := store.Root("root"); !ok || got != object {
		t.Fatalf("root after recovery=(%+v,%t), want %+v", got, ok, object)
	}
	if info, err := os.Stat(journal); err != nil || info.Size() != int64(len(journalHeader)) {
		t.Fatalf("journal after recovery: info=%v err=%v", info, err)
	}
}

func TestSealIsConstantGraphMutation(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := store.CreateObject(origin, fmt.Sprintf("build/page-%d", i), ObjectKindPage, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	before := store.Stats().Sequence
	if err := store.Seal(origin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	if after := store.Stats().Sequence; after != before+1 {
		t.Fatalf("seal journal records=%d, want 1", after-before)
	}
	if _, err := store.CreateObject(origin, "sealed", ObjectKindPage, nil, nil); err == nil {
		t.Fatal("created object in sealed origin")
	}
}

func TestSealAndBatchForkIsOneTransaction(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.CreateObject(origin, "build/page", ObjectKindPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.SetMemoryPage(origin, "family/main/memory", 7, page)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/page"); err != nil {
		t.Fatal(err)
	}
	copies := make(map[string]string)
	for i := 0; i < 20; i++ {
		copies[fmt.Sprintf("family/child-%d/memory", i)] = "family/main/memory"
	}
	before := store.Stats().Sequence
	working, err := store.SealAndFork(origin, SnapshotSealedLocal, copies, 21)
	if err != nil {
		t.Fatal(err)
	}
	if len(working) != 21 || working[0] != origin+1 || working[20] != origin+21 {
		t.Fatalf("fresh working origins=%v", working)
	}
	if got := store.Stats().Sequence - before; got != 1 {
		t.Fatalf("batch seal/fork used %d journal records, want 1", got)
	}
	object, _ := store.Object(root)
	if object.Refs != 21 {
		t.Fatalf("shared root references=%d, want 21", object.Refs)
	}
}

func TestMemoryRadixPathCopyAndSharing(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateObject(origin, "build/first", ObjectKindPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root1, err := store.SetMemoryPage(origin, "branch/main/memory", 0x123456789ab, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/first"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.MemoryPage(root1, 0x123456789ab); err != nil || !ok || got != first {
		t.Fatalf("first lookup=(%+v,%t,%v), want %+v", got, ok, err, first)
	}
	if _, ok, err := store.MemoryPage(root1, 0x123456789ac); err != nil || ok {
		t.Fatalf("absent lookup=(%t,%v), want (false,nil)", ok, err)
	}

	second, err := store.CreateObject(origin, "build/second", ObjectKindPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := store.Stats()
	root2, err := store.SetMemoryPage(origin, "branch/main/memory", 0x123456789ac, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/second"); err != nil {
		t.Fatal(err)
	}
	if root2 == root1 {
		t.Fatal("path-copy retained old root identity")
	}
	after := store.Stats()
	if got := after.Objects - before.Objects; got != memoryTreeLevels {
		t.Fatalf("path-copy created %d nodes, want %d", got, memoryTreeLevels)
	}
	for index, want := range map[uint64]ObjectID{0x123456789ab: first, 0x123456789ac: second} {
		if got, ok, err := store.MemoryPage(root2, index); err != nil || !ok || got != want {
			t.Fatalf("lookup %#x=(%+v,%t,%v), want %+v", index, got, ok, err, want)
		}
	}
	// A fork shares one root reference and does not traverse descendants.
	beforeSequence := store.Stats().Sequence
	if err := store.SetRoot("branch/child/memory", root2); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().Sequence - beforeSequence; got != 1 {
		t.Fatalf("fork root mutation used %d journal records, want 1", got)
	}
}

func TestMemoryChunkCreatesOneSharedRadixPath(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := store.ReserveObject(origin, ObjectKindMemoryChunk)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitReservedObject(chunk, "build/chunk", MemoryChunkBytes, nil, nil); err != nil {
		t.Fatal(err)
	}
	slots := make([]uint8, pagesPerChunk)
	for index := range slots {
		slots[index] = uint8(index)
	}
	before := store.Stats()
	root, pages, err := store.SetMemoryChunk(origin, "branch/main/memory", 0x1234500, chunk, slots)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != pagesPerChunk {
		t.Fatalf("page IDs=%d, want %d", len(pages), pagesPerChunk)
	}
	after := store.Stats()
	if got := after.Objects - before.Objects; got != pagesPerChunk+memoryTreeLevels {
		t.Fatalf("chunk install created %d objects, want %d pages plus %d shared radix nodes", got, pagesPerChunk, memoryTreeLevels)
	}
	for _, slot := range []uint8{0, 1, 127, 255} {
		page, ok, err := store.MemoryPage(root, 0x1234500+uint64(slot))
		if err != nil || !ok {
			t.Fatalf("resolving slot %d: page=%s ok=%t err=%v", slot, page, ok, err)
		}
		gotChunk, offset, err := store.MemoryPageOffset(page)
		if err != nil || ObjectID(gotChunk) != chunk || offset != uint32(slot)*memoryPageBytes {
			t.Fatalf("slot %d backing=(%s,%d,%v), want (%s,%d,nil)", slot, ObjectID(gotChunk), offset, err, chunk, uint32(slot)*memoryPageBytes)
		}
	}
	chunkObject, _ := store.Object(chunk)
	if chunkObject.Refs != pagesPerChunk+1 {
		t.Fatalf("chunk refs=%d, want %d page edges plus construction pin", chunkObject.Refs, pagesPerChunk)
	}
	if err := store.DeletePin("build/chunk"); err != nil {
		t.Fatal(err)
	}
	chunkObject, _ = store.Object(chunk)
	if chunkObject.Refs != pagesPerChunk {
		t.Fatalf("chunk refs after unpin=%d, want %d", chunkObject.Refs, pagesPerChunk)
	}
}

func TestReservedBatchReferencesAndPinRelease(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := store.ReserveObjects(origin, []ObjectKind{ObjectKindMemoryChunk, ObjectKindPage})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitReservedObjects([]ReservedObject{
		{ID: ids[0], Pin: "build/chunk", BulkLength: MemoryChunkBytes},
		{ID: ids[1], Pin: "build/page", Data: encodeMemoryPageRecord(0, MemoryPageIdentity{}), Children: []Edge{{Slot: 0, Child: ids[0]}}},
	}); err != nil {
		t.Fatal(err)
	}
	chunk, _ := store.Object(ids[0])
	if chunk.Refs != 2 {
		t.Fatalf("chunk refs=%d, want construction pin plus page edge", chunk.Refs)
	}
	if err := store.DeletePins([]string{"build/chunk", "build/page"}); err != nil {
		t.Fatal(err)
	}
	if collected, err := store.Collect(16); err != nil || collected != 2 {
		t.Fatalf("Collect()=(%d,%v), want (2,nil)", collected, err)
	}
}

func TestMemoryChunkReservationPublishesBulkBeforeGraph(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	data, err := OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	identity := MemoryPageIdentity{LogicalAllocator: [16]byte{1}, LogicalSequence: 99, VersionAllocator: [16]byte{2}, VersionSequence: 103}
	reservation, err := store.ReserveMemoryChunkPages(origin, "build/memory-0", []MemoryPageDescriptor{{Slot: 0, Identity: identity}, {Slot: 7}, {Slot: 255}})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, MemoryChunkBytes)
	if err := data.Install(ObjectID(reservation.Chunk), bytes.NewReader(payload), MemoryChunkBytes); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Object(ObjectID(reservation.Chunk)); ok {
		t.Fatal("bulk object became visible before graph commit")
	}
	if err := store.CommitMemoryChunk(reservation); err != nil {
		t.Fatal(err)
	}
	pages := make(map[uint64]ObjectID, len(reservation.Pages))
	for slot, page := range reservation.Pages {
		pages[0x1000+uint64(slot)] = ObjectID(page)
	}
	root, err := store.SetMemoryPages(origin, "branch/main/memory", pages)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins(reservation.Pins); err != nil {
		t.Fatal(err)
	}
	for slot, want := range reservation.Pages {
		got, ok, err := store.MemoryPage(root, 0x1000+uint64(slot))
		if err != nil || !ok || got != ObjectID(want) {
			t.Fatalf("slot %d lookup=(%s,%t,%v), want %s", slot, got, ok, err, ObjectID(want))
		}
	}
	_, _, gotIdentity, err := store.MemoryPageRecord(ObjectID(reservation.Pages[0]))
	if err != nil || gotIdentity != identity {
		t.Fatalf("MemoryPageRecord identity=(%+v,%v), want %+v", gotIdentity, err, identity)
	}
	file, err := data.Open(ObjectID(reservation.Chunk), MemoryChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(loaded, payload) {
		t.Fatalf("cold chunk load bytes=%d err=%v", len(loaded), err)
	}
}

func TestCloneRootsIsAtomicAndReferenceCounted(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.CreateObject(origin, "construction/root", ObjectKindMetadata, []byte("root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot("memory/source/0", object); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("construction/root"); err != nil {
		t.Fatal(err)
	}
	if err := store.CloneRoots(map[string]string{
		"memory/child-a/0": "memory/source/0",
		"memory/child-b/0": "memory/source/0",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Object(object)
	if !ok || got.Refs != 3 {
		t.Fatalf("shared object=(%+v,%t), want three root references", got, ok)
	}
	if err := store.CloneRoots(map[string]string{
		"memory/child-c/0": "memory/source/0",
		"memory/child-a/0": "memory/source/0",
	}); err == nil {
		t.Fatal("CloneRoots replaced an existing destination")
	}
	if _, ok := store.Root("memory/child-c/0"); ok {
		t.Fatal("failed CloneRoots left a partial destination")
	}
	got, _ = store.Object(object)
	if got.Refs != 3 {
		t.Fatalf("failed CloneRoots changed refs to %d", got.Refs)
	}
	if err := store.ReleaseRoots([]string{"memory/child-a/0", "memory/missing/0"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseRoots([]string{"memory/child-a/0"}); err != nil {
		t.Fatalf("idempotent ReleaseRoots: %v", err)
	}
	got, _ = store.Object(object)
	if got.Refs != 2 {
		t.Fatalf("ReleaseRoots left refs=%d, want 2", got.Refs)
	}
}

func TestPersistentBTreeSplitsAndHardlinks(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]ObjectID)
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("entry-%04d", i)
		pin := "build/" + name
		inode, err := store.CreateObject(origin, pin, ObjectKindInode, []byte(name), nil)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = inode
		if _, err := store.BTreeSet(origin, "branch/main/directory", ObjectKindDirectoryNode, []byte(name), inode); err != nil {
			t.Fatal(err)
		}
		if err := store.DeletePin(pin); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Collect(4096); err != nil {
			t.Fatal(err)
		}
	}
	root, ok := store.Root("branch/main/directory")
	if !ok {
		t.Fatal("directory root is absent")
	}
	for name, want := range values {
		got, ok, err := store.BTreeLookup(root, ObjectKindDirectoryNode, []byte(name))
		if err != nil || !ok || got != want {
			t.Fatalf("lookup %q=(%+v,%t,%v), want %+v", name, got, ok, err, want)
		}
	}
	linked := values["entry-0007"]
	if _, err := store.BTreeSet(origin, "branch/main/directory", ObjectKindDirectoryNode, []byte("second-link"), linked); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Collect(4096); err != nil {
		t.Fatal(err)
	}
	root, _ = store.Root("branch/main/directory")
	if got, ok, err := store.BTreeLookup(root, ObjectKindDirectoryNode, []byte("second-link")); err != nil || !ok || got != linked {
		t.Fatalf("hardlink lookup=(%+v,%t,%v), want %+v", got, ok, err, linked)
	}
	if err := store.DeleteRoot("branch/main/directory"); err != nil {
		t.Fatal(err)
	}
	for store.Stats().Objects != 0 {
		count, err := store.Collect(4096)
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("GC stalled with stats %+v", store.Stats())
		}
	}
}

func TestFilesystemAtomicMetadataAndDeletion(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.CreateObject(origin, "build/file", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootInode := InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}
	regular := InodeRecord{Number: 2, Type: "regular", Mode: 0640, UID: 1000, GID: 1000, Nlink: 2, Size: 7, File: file, Xattrs: map[string][]byte{"user.test": []byte("value")}}
	root, err := store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{PutInode: &rootInode},
		{PutInode: &regular},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "first", Inode: 2}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "second", Inode: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/file"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.FilesystemInode(root, 2); err != nil || !ok || got.File != file || string(got.Xattrs["user.test"]) != "value" {
		t.Fatalf("inode=(%+v,%t,%v)", got, ok, err)
	}
	entries, err := store.FilesystemDirectory(root, 1)
	if err != nil || len(entries) != 2 || entries[0].Name != "first" || entries[1].Name != "second" {
		t.Fatalf("directory=(%+v,%v)", entries, err)
	}
	root, err = store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{DeleteEntry: &DirectoryEntryRecord{Parent: 1, Name: "first"}},
		{DeleteEntry: &DirectoryEntryRecord{Parent: 1, Name: "second"}},
		{DeleteInode: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.FilesystemLookup(root, 1, "first"); err != nil || ok {
		t.Fatalf("deleted entry lookup=(%t,%v), want (false,nil)", ok, err)
	}
	if _, ok, err := store.FilesystemInode(root, 2); err != nil || ok {
		t.Fatalf("deleted inode lookup=(%t,%v), want (false,nil)", ok, err)
	}
	if _, err := store.Collect(10000); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDataStoreReserveInstallCloneAndReclaim(t *testing.T) {
	dir := t.TempDir()
	graph, err := Open(filepath.Join(dir, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := OpenLocalDataStore(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	source, err := graph.ReserveObject(origin, ObjectKindFile)
	if err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("snapshot-data"), 4096)
	if err := data.Install(source, bytes.NewReader(contents), int64(len(contents))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(source, "build/source", uint64(len(contents)), nil, nil); err != nil {
		t.Fatal(err)
	}
	clone, err := graph.ReserveObject(origin, ObjectKindFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.Clone(source, clone); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(clone, "build/clone", uint64(len(contents)), nil, nil); err != nil {
		t.Fatal(err)
	}
	clonePath, err := data.objectPath(clone)
	if err != nil {
		t.Fatal(err)
	}
	cloneFile, err := os.OpenFile(clonePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cloneFile.WriteAt([]byte("private"), 0); err != nil {
		t.Fatal(err)
	}
	cloneFile.Close()
	sourceFile, err := data.Open(source, int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("snapshot-data"))
	if _, err := sourceFile.Read(got); err != nil {
		t.Fatal(err)
	}
	sourceFile.Close()
	if !bytes.Equal(got, []byte("snapshot-data")) {
		t.Fatalf("clone write changed source to %q", got)
	}
	if err := graph.DeletePin("build/source"); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/clone"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Collect(16); err != nil {
		t.Fatal(err)
	}
	removed, quarantined, err := data.ReclaimUnreferenced(graph)
	if err != nil || removed != 2 || quarantined != 0 {
		t.Fatalf("reclaim=(%d,%d,%v), want (2,0,nil)", removed, quarantined, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects", "files", snapshotKey(origin))); !os.IsNotExist(err) {
		t.Fatalf("empty snapshot data directory survived reclaim: %v", err)
	}
}
