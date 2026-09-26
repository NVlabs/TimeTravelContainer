// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcsnapshot

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func unsyncedNameCount(store *LocalDataStore) int {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	return len(store.unsyncedNames)
}

// A deferred install leaves its name for the next fence; a durable one does
// not; and a sync forgets what it made durable.
func TestDeferredInstallsLeaveTheirNamesForTheNextFence(t *testing.T) {
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
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := graph.ReserveObjects(origin, []ObjectKind{ObjectKindFile, ObjectKindFile, ObjectKindFile})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("payload")
	if err := data.Install(ids[0], bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if got := unsyncedNameCount(data); got != 0 {
		t.Fatalf("a durable install left %d names for a fence", got)
	}
	if err := data.InstallDeferred(ids[1], bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if err := data.CloneDeferred(ids[1], ids[2], true); err != nil {
		t.Fatal(err)
	}
	if got := unsyncedNameCount(data); got != 2 {
		t.Fatalf("two deferred installs left %d names, want 2", got)
	}
	if err := data.SyncNewNames(); err != nil {
		t.Fatal(err)
	}
	if got := unsyncedNameCount(data); got != 0 {
		t.Fatalf("a sync left %d names", got)
	}
}

// Syncing objects makes their names durable too, so the next fence does not
// sync them again; objects it did not cover stay for that fence.
func TestSyncObjectsForgetsTheNamesItMadeDurable(t *testing.T) {
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
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]ObjectKind, 40)
	for i := range kinds {
		kinds[i] = ObjectKindFile
	}
	ids, err := graph.ReserveObjects(origin, kinds)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		payload := bytes.Repeat([]byte{byte(i)}, i+1)
		if err := data.InstallDeferred(id, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
	}
	if err := data.SyncObjects(ids[:39]); err != nil {
		t.Fatal(err)
	}
	if got := unsyncedNameCount(data); got != 1 {
		t.Fatalf("syncing 39 of 40 deferred installs left %d names, want 1", got)
	}
	for i, id := range ids {
		file, err := data.Open(id, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, i+1)) {
			t.Fatalf("object %d reads %q (%v)", i, got, err)
		}
	}
}

// An object installed from extents reads back as them with zeros between,
// which are holes.
func TestInstallExtentsLeavesHoles(t *testing.T) {
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
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := graph.ReserveObjects(origin, []ObjectKind{ObjectKindMemoryChunk, ObjectKindMemoryChunk})
	if err != nil {
		t.Fatal(err)
	}
	page := func(b byte) []byte { return bytes.Repeat([]byte{b}, 4096) }
	extents := []Extent{{Offset: 0, Data: page(1)}, {Offset: 4096, Data: page(2)}, {Offset: 40960, Data: page(3)}}
	if err := data.InstallExtentsDeferred(ids[0], MemoryChunkBytes, extents); err != nil {
		t.Fatal(err)
	}
	file, err := data.Open(ids[0], MemoryChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	file.Close()
	want := make([]byte, MemoryChunkBytes)
	for _, extent := range extents {
		copy(want[extent.Offset:], extent.Data)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the object does not read back as its extents")
	}
	if allocated := stat.Blocks * 512; allocated > 64<<10 {
		t.Fatalf("a 1 MiB object of three pages allocates %d bytes", allocated)
	}
	if err := data.InstallExtentsDeferred(ids[1], MemoryChunkBytes, []Extent{{Offset: 8192, Data: page(1)}, {Offset: 0, Data: page(2)}}); err == nil {
		t.Fatal("installed extents out of order")
	}
}

// An object removed before the next fence has no name to make durable. Both
// removal paths — an explicit Remove and the collector, which also removes the
// emptied shard directories — must forget it, or the fence fails on a
// directory that is correctly gone.
func TestRemovedObjectsAreForgottenBeforeTheFence(t *testing.T) {
	for _, collect := range []bool{false, true} {
		dir := t.TempDir()
		graph, err := Open(filepath.Join(dir, "graph"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := OpenLocalDataStore(filepath.Join(dir, "objects"))
		if err != nil {
			t.Fatal(err)
		}
		origin, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		id, err := graph.ReserveObject(origin, ObjectKindFile)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("short-lived")
		if err := data.InstallDeferred(id, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if collect {
			// Never committed to the graph, so the collector sees it as
			// unreferenced and removes it and its now-empty directories.
			if _, _, err := data.ReclaimUnreferenced(graph); err != nil {
				t.Fatal(err)
			}
		} else if err := data.Remove(id); err != nil {
			t.Fatal(err)
		}
		if err := data.SyncNewNames(); err != nil {
			t.Fatalf("collect=%v: syncing after removal: %v", collect, err)
		}
		if got := unsyncedNameCount(data); got != 0 {
			t.Fatalf("collect=%v: %d names survived their object", collect, got)
		}
		data.Close()
		graph.Close()
	}
}

// A linked object is a second name for the source's bytes: a write through it
// is a write through the source, it outlives the source's removal, and its
// name waits for the next fence like any deferred install. In both store
// modes, and into an origin whose directory does not exist yet.
func TestLinkedObjectSharesBytesAndOutlivesItsSource(t *testing.T) {
	for _, donated := range []bool{false, true} {
		dir := t.TempDir()
		graph, err := Open(filepath.Join(dir, "graph"))
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, "objects")
		data, err := OpenLocalDataStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if donated {
			data.Close()
			directory, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if data, err = OpenLocalDataStoreFD(directory); err != nil {
				t.Fatal(err)
			}
		}
		sealed, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		working, err := graph.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		source, err := graph.ReserveObject(sealed, ObjectKindFile)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := graph.ReserveObject(working, ObjectKindFile)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("payload")
		if err := data.Install(source, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if err := data.LinkDeferred(source, destination); err != nil {
			t.Fatalf("donated=%v: %v", donated, err)
		}
		if got := unsyncedNameCount(data); got != 1 {
			t.Fatalf("donated=%v: a link left %d names for the fence, want 1", donated, got)
		}
		read := func(id ObjectID) string {
			t.Helper()
			file, err := data.Open(id, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			return string(got)
		}
		file, err := data.OpenFile(destination, os.O_RDWR, -1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte("P"), 0); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if got := read(source); got != "Payload" {
			t.Fatalf("donated=%v: the source reads %q after a write through the link", donated, got)
		}
		if err := data.Remove(source); err != nil {
			t.Fatal(err)
		}
		if got := read(destination); got != "Payload" {
			t.Fatalf("donated=%v: the link reads %q after the source's removal", donated, got)
		}
		if err := data.SyncNewNames(); err != nil {
			t.Fatal(err)
		}
		if got := unsyncedNameCount(data); got != 0 {
			t.Fatalf("donated=%v: a sync left %d names", donated, got)
		}
		data.Close()
		graph.Close()
	}
}
