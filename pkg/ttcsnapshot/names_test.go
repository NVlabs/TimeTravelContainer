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
	"path/filepath"
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
