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
	"os"
	"path/filepath"
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "graph.journal")
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
}
