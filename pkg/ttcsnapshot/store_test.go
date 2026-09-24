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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

func withExternalTransaction(t *testing.T, store *Store, fn func()) {
	t.Helper()
	if err := store.BeginExternalTransaction(); err != nil {
		t.Fatal(err)
	}
	fn()
	if err := store.EndExternalTransaction(); err != nil {
		t.Fatal(err)
	}
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

func TestAdoptWorkingPreservesExternalIdentityAndLocalMonotonicity(t *testing.T) {
	store := openTestStore(t)
	if err := store.AdoptWorking(42); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptWorking(42); err != nil {
		t.Fatalf("idempotent adoption: %v", err)
	}
	if snapshot, ok := store.Snapshot(42); !ok || snapshot.State != SnapshotWorking {
		t.Fatalf("adopted snapshot=(%+v,%t), want working", snapshot, ok)
	}
	next, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if next != 43 {
		t.Fatalf("next local snapshot=%d, want 43", next)
	}
	if err := store.Seal(42, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptWorking(42); err == nil {
		t.Fatal("immutable snapshot regressed to working")
	}
	if err := store.AdoptWorking(0); err == nil {
		t.Fatal("zero external snapshot was accepted")
	}
}

func TestMarkSharedRequiresImmutableSnapshot(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkShared(origin); err == nil {
		t.Fatal("working snapshot became shared without being sealed")
	}
	if err := store.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkShared(origin); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkShared(origin); err != nil {
		t.Fatalf("idempotent shared transition failed: %v", err)
	}
	if snapshot, ok := store.Snapshot(origin); !ok || snapshot.State != SnapshotShared {
		t.Fatalf("shared snapshot=(%+v,%t)", snapshot, ok)
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
	var one SnapshotID
	withExternalTransaction(t, first, func() {
		one, err = first.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
	})
	if err := second.Reload(); err != nil {
		t.Fatal(err)
	}
	var two SnapshotID
	withExternalTransaction(t, second, func() {
		two, err = second.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
	})
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

func TestExternallyCoordinatedMutationRequiresWriterTransaction(t *testing.T) {
	store, err := OpenExternallyCoordinated(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AllocateWorking(); err == nil {
		t.Fatal("unfenced externally-coordinated mutation succeeded")
	}
	withExternalTransaction(t, store, func() {
		if _, err := store.AllocateWorking(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExternallyCoordinatedReloadReplaysJournalSuffix(t *testing.T) {
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
	journal := reader.journal

	withExternalTransaction(t, writer, func() {
		origin, err := writer.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := writer.CreateObject(origin, "construction/leaf", ObjectKindFile, []byte("leaf"), nil)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := writer.CreateObject(origin, "construction/parent", ObjectKindMetadata, []byte("parent"), []Edge{{Slot: 0, Child: leaf}})
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.SetRoot("branch/main", parent); err != nil {
			t.Fatal(err)
		}
		if err := writer.DeletePin("construction/leaf"); err != nil {
			t.Fatal(err)
		}
		if err := writer.DeletePin("construction/parent"); err != nil {
			t.Fatal(err)
		}
		if err := writer.DeleteRoot("branch/main"); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Collect(16); err != nil {
			t.Fatal(err)
		}
	})

	if err := reader.Reload(); err != nil {
		t.Fatal(err)
	}
	if reader.journal != journal {
		t.Fatal("same-generation reload reopened the journal instead of replaying its suffix")
	}
	if err := reader.Verify(); err != nil {
		t.Fatalf("incrementally replayed graph is invalid: %v", err)
	}
	if got, want := reader.Inspect(), writer.Inspect(); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental replay differs from writer:\nreader=%+v\nwriter=%+v", got, want)
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
	var origin SnapshotID
	withExternalTransaction(t, writer, func() {
		origin, err = writer.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
	})
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
	var next SnapshotID
	withExternalTransaction(t, reader, func() {
		next, err = reader.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
	})
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
		withExternalTransaction(t, writer, func() {
			if _, err := writer.AllocateWorking(); err != nil {
				t.Fatal(err)
			}
			if index%7 == 6 {
				if err := writer.CheckpointExternallyCoordinated(); err != nil {
					t.Fatal(err)
				}
			}
		})
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
	// The last reference's release collects what it held at once, down
	// through the page, rather than leaving both for Collect.
	if _, ok := store.Object(node); ok {
		t.Fatal("the node outlived its last reference")
	}
	if _, ok := store.Object(page); ok {
		t.Fatal("the page outlived the node that held it")
	}
	if err := store.SetPin("resurrect", node); err == nil {
		t.Fatal("retained a released object")
	}
	if count, err := store.Collect(8); err != nil || count != 0 {
		t.Fatalf("Collect()=(%d,%v), want nothing left", count, err)
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

func TestCheckpointHeadPublicationBoundary(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if store.generation != 2 {
		t.Fatalf("baseline generation=%d, want 2", store.generation)
	}
	checkpoint, err := json.Marshal(store.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	baseline := readGraphTestFiles(t, dir)

	nextCheckpoint := append(checkpoint, '\n')
	nextJournal := []byte(journalHeader)
	nextHead, err := json.Marshal(graphHead{Version: formatVersion, Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	nextHead = append(nextHead, '\n')

	tests := []struct {
		name       string
		files      map[string][]byte
		remove     []string
		wantError  bool
		wantGen    uint64
		wantNextID SnapshotID
	}{
		{
			name:       "checkpoint prepared",
			files:      map[string][]byte{graphCheckpointName(3): nextCheckpoint},
			wantGen:    2,
			wantNextID: first + 1,
		},
		{
			name: "checkpoint and journal prepared",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				graphJournalName(3):    nextJournal,
			},
			wantGen:    2,
			wantNextID: first + 1,
		},
		{
			name: "head temp prepared",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				graphJournalName(3):    nextJournal,
				".graph-head-deadbeef": nextHead,
			},
			wantGen:    2,
			wantNextID: first + 1,
		},
		{
			name: "head published",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				graphJournalName(3):    nextJournal,
				headFileName:           nextHead,
			},
			wantGen:    3,
			wantNextID: first + 1,
		},
		{
			name: "old generation retired",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				graphJournalName(3):    nextJournal,
				headFileName:           nextHead,
			},
			remove:     []string{graphCheckpointName(2), graphJournalName(2)},
			wantGen:    3,
			wantNextID: first + 1,
		},
		{
			name: "published head missing checkpoint",
			files: map[string][]byte{
				graphJournalName(3): nextJournal,
				headFileName:        nextHead,
			},
			wantError: true,
		},
		{
			name: "published head missing journal",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				headFileName:           nextHead,
			},
			wantError: true,
		},
		{
			name: "published head truncated checkpoint",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint[:len(nextCheckpoint)/2],
				graphJournalName(3):    nextJournal,
				headFileName:           nextHead,
			},
			wantError: true,
		},
		{
			name: "truncated published head",
			files: map[string][]byte{
				graphCheckpointName(3): nextCheckpoint,
				graphJournalName(3):    nextJournal,
				headFileName:           nextHead[:len(nextHead)/2],
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseDir := t.TempDir()
			writeGraphTestFiles(t, caseDir, baseline)
			writeGraphTestFiles(t, caseDir, test.files)
			for _, name := range test.remove {
				if err := os.Remove(filepath.Join(caseDir, name)); err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := Open(caseDir)
			if test.wantError {
				if err == nil {
					recovered.Close()
					t.Fatal("Open accepted an incomplete published generation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if recovered.generation != test.wantGen {
				t.Fatalf("generation=%d, want %d", recovered.generation, test.wantGen)
			}
			if _, ok := recovered.Snapshot(first); !ok {
				t.Fatalf("snapshot %d disappeared", first)
			}
			next, err := recovered.AllocateWorking()
			if err != nil {
				t.Fatal(err)
			}
			if next != test.wantNextID {
				t.Fatalf("next snapshot=%d, want %d", next, test.wantNextID)
			}
		})
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

func TestSealAndForkWithExternallyAllocatedOrigins(t *testing.T) {
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
	before := store.Stats().Sequence
	working := []SnapshotID{1001, 1002}
	if err := store.SealAndForkWithOrigins(origin, SnapshotSealedVolatile, map[string]string{
		"family/child/memory": "family/main/memory",
	}, working); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().Sequence - before; got != 1 {
		t.Fatalf("external seal/fork used %d journal records, want 1", got)
	}
	for _, id := range working {
		snapshot, ok := store.Snapshot(id)
		if !ok || snapshot.State != SnapshotWorking {
			t.Fatalf("external working snapshot %d=(%+v,%t)", id, snapshot, ok)
		}
	}
	object, _ := store.Object(root)
	if object.Refs != 2 {
		t.Fatalf("shared root references=%d, want 2", object.Refs)
	}
	if next, err := store.AllocateWorking(); err != nil || next != 1003 {
		t.Fatalf("next local allocation=(%d,%v), want (1003,nil)", next, err)
	}
	if err := store.SealAndForkWithOrigins(working[0], SnapshotSealedVolatile, map[string]string{
		"family/duplicate/memory": "family/main/memory",
	}, []SnapshotID{working[1]}); err == nil {
		t.Fatal("accepted an already-present external working origin")
	}
}

func TestRetainRootsIsAtomicAndIdempotent(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateObject(origin, "build/first-root", ObjectKindMetadata, []byte("first"), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateObject(origin, "build/second-root", ObjectKindMetadata, []byte("second"), nil)
	if err != nil {
		t.Fatal(err)
	}
	before := store.Stats().Sequence
	bindings := map[string]ObjectID{
		"remote/operation-a/root-0": first,
		"remote/operation-a/root-1": first,
		"remote/operation-a/root-2": second,
	}
	if err := store.RetainRoots(bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins([]string{"build/first-root", "build/second-root"}); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().Sequence - before; got != 2 {
		t.Fatalf("RetainRoots plus construction-pin handoff used %d journal records, want 2", got)
	}
	firstObject, _ := store.Object(first)
	secondObject, _ := store.Object(second)
	if firstObject.Refs != 2 || secondObject.Refs != 1 {
		t.Fatalf("retained refs=(%d,%d), want (2,1)", firstObject.Refs, secondObject.Refs)
	}

	before = store.Stats().Sequence
	if err := store.RetainRoots(bindings); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if got := store.Stats().Sequence; got != before {
		t.Fatalf("idempotent replay advanced sequence from %d to %d", before, got)
	}
	if err := store.RetainRoots(map[string]ObjectID{
		"remote/operation-a/root-0": second,
		"remote/operation-b/root-0": second,
	}); err == nil {
		t.Fatal("mismatched replay replaced an authoritative root")
	}
	if _, ok := store.Root("remote/operation-b/root-0"); ok {
		t.Fatal("failed batch left a partial root")
	}
	secondObject, _ = store.Object(second)
	if secondObject.Refs != 1 {
		t.Fatalf("failed batch changed second refs to %d", secondObject.Refs)
	}

	wrongRelease := map[string]ObjectID{
		"remote/operation-a/root-0": second,
		"remote/operation-a/root-1": first,
	}
	if err := store.ReleaseRetainedRoots(wrongRelease); err == nil {
		t.Fatal("mismatched release removed an authoritative root")
	}
	if got, ok := store.Root("remote/operation-a/root-1"); !ok || got != first {
		t.Fatalf("failed release changed sibling root=(%s,%t)", got, ok)
	}
	if err := store.ReleaseRetainedRoots(bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseRetainedRoots(bindings); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	if collected, err := store.Collect(8); err != nil || collected != 2 {
		t.Fatalf("Collect()=(%d,%v), want (2,nil)", collected, err)
	}
}

func TestRetainRootsJournalPrefixRecovery(t *testing.T) {
	base := t.TempDir()
	store, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.CreateObject(origin, "build/crash-root", ObjectKindMetadata, []byte("stable"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot("branch/source", object); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/crash-root"); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	journalName := filepath.Base(store.journal.Name())
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	baseline := readGraphTestFiles(t, base)

	store, err = Open(base)
	if err != nil {
		t.Fatal(err)
	}
	binding := map[string]ObjectID{"remote/tx/root-0": object}
	if err := store.RetainRoots(binding); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(base, journalName))
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) <= len(journalHeader) {
		t.Fatal("retain transaction did not append a journal record")
	}
	record := journal[len(journalHeader):]

	for prefix := 0; prefix <= len(record); prefix++ {
		dir := filepath.Join(t.TempDir(), fmt.Sprintf("prefix-%04d", prefix))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		writeGraphTestFiles(t, dir, baseline)
		file, err := os.OpenFile(filepath.Join(dir, journalName), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(record[:prefix]); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		recovered, err := Open(dir)
		if err != nil {
			t.Fatalf("prefix %d/%d: %v", prefix, len(record), err)
		}
		root, retained := recovered.Root("remote/tx/root-0")
		gotObject, exists := recovered.Object(object)
		if !exists {
			recovered.Close()
			t.Fatalf("prefix %d removed baseline object", prefix)
		}
		complete := prefix == len(record)
		if retained != complete || retained && root != object {
			recovered.Close()
			t.Fatalf("prefix %d retained=(%s,%t), complete=%t", prefix, root, retained, complete)
		}
		wantRefs := uint64(1)
		if complete {
			wantRefs = 2
		}
		if gotObject.Refs != wantRefs {
			recovered.Close()
			t.Fatalf("prefix %d refs=%d, want %d", prefix, gotObject.Refs, wantRefs)
		}
		if err := recovered.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func readGraphTestFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = data
	}
	return files
}

func writeGraphTestFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
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

func TestReachableBulkObjectsUsesImmutableIdentity(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	commitBulk := func(pin string, kind ObjectKind, length uint64) ObjectID {
		t.Helper()
		id, err := store.ReserveObject(origin, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CommitReservedObject(id, pin, length, nil, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	shared := commitBulk("build/shared", ObjectKindFile, 4096)
	private := commitBulk("build/private", ObjectKindMemoryChunk, MemoryChunkBytes)
	root, err := store.CreateObject(origin, "build/root", ObjectKindMetadata, nil, []Edge{{Slot: 0, Child: shared}, {Slot: 1, Child: private}})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := ReachableBulkObjects(store, []ObjectID{root, root})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[shared] != 4096 || objects[private] != MemoryChunkBytes {
		t.Fatalf("reachable bulk objects = %+v", objects)
	}
	missing := ObjectID{Snapshot: origin, Kind: ObjectKindMetadata, Ordinal: 9999}
	if _, err := ReachableBulkObjects(store, []ObjectID{missing}); err == nil {
		t.Fatal("missing reachable object was silently ignored")
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
	for _, id := range ids {
		if _, ok := store.Object(id); ok {
			t.Fatalf("object %s outlived its pins", id)
		}
	}
	if collected, err := store.Collect(16); err != nil || collected != 0 {
		t.Fatalf("Collect()=(%d,%v), want nothing left", collected, err)
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
	defer data.Close()
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	identity := MemoryPageIdentity{LogicalAllocator: [16]byte{1}, LogicalSequence: 99, VersionAllocator: [16]byte{2}, VersionSequence: 103, References: 2}
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
	all, err := store.MemoryPages(root)
	if err != nil || len(all) != len(reservation.Pages) {
		t.Fatalf("MemoryPages=(%d,%v), want %d entries", len(all), err, len(reservation.Pages))
	}
	for slot, want := range reservation.Pages {
		if got := all[0x1000+uint64(slot)]; got != ObjectID(want) {
			t.Fatalf("MemoryPages slot %d=%s, want %s", slot, got, ObjectID(want))
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
	objectsDir, err := os.Open(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	donated, err := OpenLocalDataStoreFD(objectsDir)
	objectsDir.Close()
	if err != nil {
		t.Fatal(err)
	}
	file, err = donated.Open(ObjectID(reservation.Chunk), MemoryChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := file.Stat(); err != nil || info.Size() != MemoryChunkBytes {
		t.Fatalf("donated object info=(%v,%v)", info, err)
	}
	file.Close()
	if err := donated.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExistingMemoryPagesRebuildAndReclaim(t *testing.T) {
	directory := t.TempDir()
	graphDirectory := filepath.Join(directory, "graph")
	store, err := Open(graphDirectory)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	identity := MemoryPageIdentity{LogicalAllocator: [16]byte{1}, LogicalSequence: 7, VersionAllocator: [16]byte{2}, VersionSequence: 11, References: 3}
	descriptor := MemoryPageDescriptor{Slot: 5, Identity: identity}
	reservation, err := store.ReserveMemoryChunkPages(origin, "reuse/build", []MemoryPageDescriptor{descriptor})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitMemoryChunk(reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMemoryPages(origin, "reuse/root", map[uint64]ObjectID{99: ObjectID(reservation.Pages[5])}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins(reservation.Pins); err != nil {
		t.Fatal(err)
	}
	got, err := store.ExistingMemoryPages([]MemoryPageDescriptor{descriptor})
	if err != nil || got[5] != reservation.Pages[5] {
		t.Fatalf("ExistingMemoryPages()=(%v,%v), want slot 5=%s", got, err, ObjectID(reservation.Pages[5]))
	}
	differentReferences := descriptor
	differentReferences.Identity.References++
	if got, err := store.ExistingMemoryPages([]MemoryPageDescriptor{differentReferences}); err != nil || len(got) != 0 {
		t.Fatalf("ExistingMemoryPages(different references)=(%v,%v), want empty", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(graphDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err = store.ExistingMemoryPages([]MemoryPageDescriptor{descriptor})
	if err != nil || got[5] != reservation.Pages[5] {
		t.Fatalf("ExistingMemoryPages() after reopen=(%v,%v), want slot 5=%s", got, err, ObjectID(reservation.Pages[5]))
	}
	if err := store.DeleteRoot("reuse/root"); err != nil {
		t.Fatal(err)
	}
	for store.Stats().Objects != 0 {
		collected, err := store.Collect(4096)
		if err != nil {
			t.Fatal(err)
		}
		if collected == 0 {
			t.Fatalf("GC stalled with stats %+v", store.Stats())
		}
	}
	if got, err := store.ExistingMemoryPages([]MemoryPageDescriptor{descriptor}); err != nil || len(got) != 0 {
		t.Fatalf("ExistingMemoryPages() after reclaim=(%v,%v), want empty", got, err)
	}
}

func TestExistingMemoryPagesFiltersSnapshotAuthorityState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := MemoryPageDescriptor{Slot: 7, Identity: MemoryPageIdentity{
		LogicalAllocator: [16]byte{1}, LogicalSequence: 9,
		VersionAllocator: [16]byte{2}, VersionSequence: 11, References: 1,
	}}
	reservation, err := store.ReserveMemoryChunkPages(origin, "authority/build", []MemoryPageDescriptor{descriptor})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitMemoryChunk(reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMemoryPages(origin, "authority/root", map[uint64]ObjectID{0: ObjectID(reservation.Pages[7])}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins(reservation.Pins); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ExistingMemoryPagesInStates([]MemoryPageDescriptor{descriptor}, SnapshotSealedVolatile); err != nil || got[7] != reservation.Pages[7] {
		t.Fatalf("volatile lookup=(%v,%v), want slot 7=%s", got, err, ObjectID(reservation.Pages[7]))
	}
	if err := store.MarkShared(origin); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ExistingMemoryPagesInStates([]MemoryPageDescriptor{descriptor}, SnapshotWorking, SnapshotSealedVolatile); err != nil || len(got) != 0 {
		t.Fatalf("volatile-only lookup of shared page=(%v,%v), want empty", got, err)
	}
	if got, err := store.ExistingMemoryPagesInStates([]MemoryPageDescriptor{descriptor}, SnapshotShared); err != nil || got[7] != reservation.Pages[7] {
		t.Fatalf("shared lookup=(%v,%v), want slot 7=%s", got, err, ObjectID(reservation.Pages[7]))
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
	if got, err := store.FilesystemBulkBytes(root); err != nil || got != 7 {
		t.Fatalf("FilesystemBulkBytes=%d, %v; want 7, nil", got, err)
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
	if got, err := store.FilesystemBulkBytes(root); err != nil || got != 0 {
		t.Fatalf("FilesystemBulkBytes after delete=%d, %v; want 0, nil", got, err)
	}
	if _, err := store.Collect(10000); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemBulkBytesTracksInodesNotDirectoryEntries(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	file1, err := store.CreateObject(origin, "build/file-1", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file2, err := store.CreateObject(origin, "build/file-2", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootInode := InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}
	first := InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 2, Size: 100, File: file1}
	root, err := store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{PutInode: &rootInode}, {PutInode: &first},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "one", Inode: 2}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "hardlink", Inode: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.FilesystemBulkBytes(root); err != nil || got != 100 {
		t.Fatalf("hard-linked aggregate=%d, %v; want 100, nil", got, err)
	}
	first.Size = 40
	second := InodeRecord{Number: 3, Type: "regular", Mode: 0644, Nlink: 1, Size: 25, File: file2}
	root, err = store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{PutInode: &first}, {PutInode: &second},
		{DeleteEntry: &DirectoryEntryRecord{Parent: 1, Name: "one"}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "two", Inode: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.FilesystemBulkBytes(root); err != nil || got != 65 {
		t.Fatalf("updated aggregate=%d, %v; want 65, nil", got, err)
	}
	forked := "branch/peer/filesystem"
	if err := store.CloneRoots(map[string]string{forked: "branch/main/filesystem"}); err != nil {
		t.Fatal(err)
	}
	peerRoot, ok := store.Root(forked)
	if !ok {
		t.Fatal("forked filesystem root is missing")
	}
	if got, err := store.FilesystemBulkBytes(peerRoot); err != nil || got != 65 {
		t.Fatalf("forked aggregate=%d, %v; want 65, nil", got, err)
	}
}

func TestFilesystemLargeBatchCopiesEachTreePathOnce(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	const files = 512
	rootInode := InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}
	operations := []FilesystemOperation{{PutInode: &rootInode}}
	for index := 0; index < files; index++ {
		inode := InodeRecord{Number: uint64(index + 2), Type: "regular", Mode: 0644, Nlink: 1}
		entry := DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%04d", index), Inode: inode.Number}
		operations = append(operations, FilesystemOperation{PutInode: &inode}, FilesystemOperation{PutEntry: &entry})
	}
	root, err := store.ApplyFilesystem(origin, "branch/main/filesystem", operations)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := store.FilesystemDirectory(root, 1); err != nil || len(entries) != files {
		t.Fatalf("directory entries=%d err=%v, want %d", len(entries), err, files)
	}
	// One inode value and one directory-entry value per file dominate this
	// graph. A generous linear bound catches accidental per-operation path-copy
	// chains without depending on exact B-tree packing.
	if objects := store.Stats().Objects; objects > 3*files {
		t.Fatalf("batched filesystem created %d live objects for %d files", objects, files)
	}
	if err := store.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	replayed, ok := store.Root("branch/main/filesystem")
	if !ok || replayed != root {
		t.Fatalf("replayed root=(%s,%t), want %s", replayed, ok, root)
	}
	if entry, ok, err := store.FilesystemLookup(replayed, 1, "file-0511"); err != nil || !ok || entry.Inode != 513 {
		t.Fatalf("replayed lookup=(%+v,%t,%v)", entry, ok, err)
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
	if err := data.Clone(source, clone, true); err != nil {
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
	if _, err := os.Stat(filepath.Join(dir, "objects", "files")); !os.IsNotExist(err) {
		t.Fatalf("empty object-kind directory survived reclaim: %v", err)
	}
}

func TestLocalDataStoreReclaimWaitsForGraphCommit(t *testing.T) {
	directory := t.TempDir()
	graph, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	collector, err := OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	id, err := graph.ReserveObject(origin, ObjectKindMemoryChunk)
	if err != nil {
		t.Fatal(err)
	}

	installed := make(chan struct{})
	commit := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- data.WithGraphMutation(func() error {
			if err := data.Install(id, strings.NewReader("data"), 4); err != nil {
				return err
			}
			close(installed)
			<-commit
			return graph.CommitReservedObject(id, "build/object", 4, nil, nil)
		})
	}()
	<-installed

	reclaimDone := make(chan error, 1)
	go func() {
		_, _, err := collector.ReclaimUnreferenced(graph)
		reclaimDone <- err
	}()
	select {
	case err := <-reclaimDone:
		t.Fatalf("reclamation crossed an uncommitted graph mutation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(commit)
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-reclaimDone; err != nil {
		t.Fatal(err)
	}
	file, err := data.Open(id, 4)
	if err != nil {
		t.Fatalf("committed bulk object was reclaimed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDataStoreConcurrentReclaimIsIdempotent(t *testing.T) {
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
	const objects = 128
	for i := 0; i < objects; i++ {
		id, err := graph.ReserveObject(origin, ObjectKindMemoryChunk)
		if err != nil {
			t.Fatal(err)
		}
		if err := data.Install(id, bytes.NewReader([]byte{byte(i)}), 1); err != nil {
			t.Fatal(err)
		}
		pin := fmt.Sprintf("concurrent/%d", i)
		if err := graph.CommitReservedObject(id, pin, 1, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := graph.DeletePin(pin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := graph.Collect(objects); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsByWorker := make(chan error, 8)
	removedByWorker := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			removed, quarantined, err := data.ReclaimUnreferenced(graph)
			if err == nil && quarantined != 0 {
				err = fmt.Errorf("quarantined %d known objects", quarantined)
			}
			removedByWorker <- removed
			errorsByWorker <- err
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	close(removedByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	removed := 0
	for count := range removedByWorker {
		removed += count
	}
	if removed != objects {
		t.Fatalf("concurrent reclaim removed %d objects, want %d", removed, objects)
	}
}

// Deleting a root collects what it alone held in the same transaction, as far
// as a collection transaction goes, and leaves the rest to Collect.
func TestDeletingARootCollectsWhatItHeldWithinABound(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	const children = maxCollectObjectsPerTransaction + 10
	edges := make([]Edge, 0, children)
	for i := 0; i < children; i++ {
		id, err := store.CreateObject(origin, fmt.Sprintf("build/child-%d", i), ObjectKindMetadata, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		edges = append(edges, Edge{Slot: uint16(i), Child: id})
	}
	node, err := store.CreateObject(origin, "build/node", ObjectKindMetadata, nil, edges)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < children; i++ {
		if err := store.DeletePin(fmt.Sprintf("build/child-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetRoot("root", node); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePin("build/node"); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Objects != children+1 || stats.Reclaiming != 0 {
		t.Fatalf("stats while rooted=%+v", stats)
	}
	if err := store.DeleteRoot("root"); err != nil {
		t.Fatal(err)
	}
	// The node first, then its children breadth first, to the bound.
	left := children + 1 - maxCollectObjectsPerTransaction
	if stats := store.Stats(); stats.Objects != left || stats.Reclaiming != left {
		t.Fatalf("stats after the delete=%+v, want %d objects left reclaiming", stats, left)
	}
	if got, err := store.Collect(4096); err != nil || got != left {
		t.Fatalf("Collect(4096)=(%d,%v), want (%d,nil)", got, err, left)
	}
	if stats := store.Stats(); stats.Objects != 0 {
		t.Fatalf("stats after collection=%+v", stats)
	}
}

// A checkpoint is due once the journal has grown past both the minimum and
// the checkpoint it would replace, as reopening the graph also knows.
func TestCheckpointDueWaitsForTheJournalToOutgrowTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	create := func() {
		if _, err := store.CreateObject(origin, fmt.Sprintf("due/%d", next), ObjectKindMetadata, nil, nil); err != nil {
			t.Fatal(err)
		}
		next++
	}
	for i := 0; i < 64; i++ {
		create()
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	checkpoint := store.checkpointBytes
	for store.JournalBytes() < checkpoint {
		if store.CheckpointDue(1) {
			t.Fatalf("checkpoint due with a %d-byte journal and a %d-byte checkpoint", store.JournalBytes(), checkpoint)
		}
		create()
	}
	if !store.CheckpointDue(1) {
		t.Fatalf("checkpoint not due with a %d-byte journal and a %d-byte checkpoint", store.JournalBytes(), checkpoint)
	}
	if store.CheckpointDue(store.JournalBytes() + 1) {
		t.Fatal("checkpoint due below its minimum")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.checkpointBytes != checkpoint || !reopened.CheckpointDue(1) {
		t.Fatalf("reopened graph has a %d-byte checkpoint, due %t; want %d, due", reopened.checkpointBytes, reopened.CheckpointDue(1), checkpoint)
	}
}

// A checkpoint is also due, before the journal outgrows it, once the graph
// has shrunk to under half of what the checkpoint holds, as after a large
// removal; reopening the graph knows what its checkpoint holds.
func TestCheckpointDueOnceTheGraphHasShrunk(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	for i := 0; i < 64; i++ {
		pin := fmt.Sprintf("shrink/%d", i)
		if _, err := store.CreateObject(origin, pin, ObjectKindMetadata, bytes.Repeat([]byte{1}, 256), nil); err != nil {
			t.Fatal(err)
		}
		pins = append(pins, pin)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePins(pins[:31]); err != nil {
		t.Fatal(err)
	}
	for {
		collected, err := store.Collect(4096)
		if err != nil {
			t.Fatal(err)
		}
		if collected == 0 {
			break
		}
	}
	if store.CheckpointDue(1) {
		t.Fatal("checkpoint due with more than half the graph left")
	}
	if err := store.DeletePins(pins[31:33]); err != nil {
		t.Fatal(err)
	}
	for {
		collected, err := store.Collect(4096)
		if err != nil {
			t.Fatal(err)
		}
		if collected == 0 {
			break
		}
	}
	if store.JournalBytes() >= store.checkpointBytes {
		t.Fatalf("the journal of %d bytes outgrew the %d-byte checkpoint", store.JournalBytes(), store.checkpointBytes)
	}
	if !store.CheckpointDue(1) {
		t.Fatal("checkpoint not due with half the graph removed")
	}
	if store.CheckpointDue(store.JournalBytes() + 1) {
		t.Fatal("checkpoint due below its minimum")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.CheckpointDue(1) {
		t.Fatal("the reopened graph does not know it has shrunk")
	}
	if err := reopened.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if reopened.CheckpointDue(1) {
		t.Fatal("checkpoint still due after one was taken")
	}
}

// Collect collects up to its limit in one call, in transactions bounded by
// the objects they delete and the reference counts they update.
func TestCollectCommitsBoundedTransactions(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	// Three nodes of four children each. Releasing retained roots leaves the
	// nodes for Collect, where deleting a root or pin would collect them.
	retained := make(map[string]ObjectID)
	for i := 0; i < 3; i++ {
		edges := make([]Edge, 0, 4)
		for j := 0; j < 4; j++ {
			pin := fmt.Sprintf("batch/%d/%d", i, j)
			id, err := store.CreateObject(origin, pin, ObjectKindMetadata, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			edges = append(edges, Edge{Slot: uint16(j), Child: id})
		}
		pin := fmt.Sprintf("batch/%d", i)
		id, err := store.CreateObject(origin, pin, ObjectKindMetadata, nil, edges)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 4; j++ {
			if err := store.DeletePin(fmt.Sprintf("batch/%d/%d", i, j)); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.SetRoot(pin, id); err != nil {
			t.Fatal(err)
		}
		if err := store.DeletePin(pin); err != nil {
			t.Fatal(err)
		}
		retained[pin] = id
	}
	if err := store.ReleaseRetainedRoots(retained); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Objects != 15 || stats.Reclaiming != 3 {
		t.Fatalf("stats after the release=%+v, want 15 objects, 3 reclaiming", stats)
	}
	// A limit is honoured across transactions.
	sequence := store.Stats().Sequence
	if got, err := store.collect(2, 4096, 4); err != nil || got != 2 {
		t.Fatalf("collect(2)=(%d,%v), want (2,nil)", got, err)
	}
	if stats := store.Stats(); stats.Sequence != sequence+2 {
		t.Fatalf("collect(2) committed %d transactions, want 2", stats.Sequence-sequence)
	}
	// The call carries on from where the last one stopped: the last node,
	// whose four counts fill a transaction, then all twelve children, which
	// update none.
	sequence = store.Stats().Sequence
	if got, err := store.collect(4096, 4096, 4); err != nil || got != 13 {
		t.Fatalf("collect(4096)=(%d,%v), want (13,nil)", got, err)
	}
	if stats := store.Stats(); stats.Objects != 0 || stats.Sequence != sequence+2 {
		t.Fatalf("stats after collection=%+v, want no objects after 2 transactions from %d", stats, sequence)
	}
}

// Collect carries on from where its last call stopped, and still finds what
// was released since before it reports that nothing is left.
func TestCollectFindsWhatWasReleasedSinceItsLastCall(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	retain := func(first, count int) map[string]ObjectID {
		retained := make(map[string]ObjectID, count)
		for i := first; i < first+count; i++ {
			pin := fmt.Sprintf("later/%d", i)
			id, err := store.CreateObject(origin, pin, ObjectKindMetadata, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetRoot(pin, id); err != nil {
				t.Fatal(err)
			}
			if err := store.DeletePin(pin); err != nil {
				t.Fatal(err)
			}
			retained[pin] = id
		}
		return retained
	}
	early, late := retain(0, 5), retain(5, 1)
	if err := store.ReleaseRetainedRoots(early); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Collect(2); err != nil || got != 2 {
		t.Fatalf("Collect(2)=(%d,%v), want (2,nil)", got, err)
	}
	if err := store.ReleaseRetainedRoots(late); err != nil {
		t.Fatal(err)
	}
	total := 0
	for {
		got, err := store.Collect(4096)
		if err != nil {
			t.Fatal(err)
		}
		if got == 0 {
			break
		}
		total += got
	}
	if stats := store.Stats(); total != 4 || stats.Objects != 0 {
		t.Fatalf("Collect until zero collected %d, left %+v; want 4, nothing left", total, stats)
	}
}

// Collect is not capped at one transaction's worth per call.
func TestCollectTakesMoreThanOneTransactionPerCall(t *testing.T) {
	store := openTestStore(t)
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	const objects = 10
	retained := make(map[string]ObjectID, objects)
	for i := 0; i < objects; i++ {
		pin := fmt.Sprintf("batch/%d", i)
		id, err := store.CreateObject(origin, pin, ObjectKindMetadata, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetRoot(pin, id); err != nil {
			t.Fatal(err)
		}
		if err := store.DeletePin(pin); err != nil {
			t.Fatal(err)
		}
		retained[pin] = id
	}
	if err := store.ReleaseRetainedRoots(retained); err != nil {
		t.Fatal(err)
	}
	sequence := store.Stats().Sequence
	if got, err := store.collect(4096, 4, 4096); err != nil || got != objects {
		t.Fatalf("collect(4096)=(%d,%v), want (%d,nil)", got, err, objects)
	}
	if stats := store.Stats(); stats.Objects != 0 || stats.Sequence != sequence+3 {
		t.Fatalf("stats after collection=%+v, want no objects after 3 transactions from %d", stats, sequence)
	}
}

// Install leaves a hole for each zero page of an object, which still reads
// back whole.
func TestLocalDataStoreInstallLeavesZeroPagesAsHoles(t *testing.T) {
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
	ids, err := graph.ReserveObjects(origin, []ObjectKind{ObjectKindMemoryChunk, ObjectKindMemoryChunk})
	if err != nil {
		t.Fatal(err)
	}
	// Two pages with data among 64 zero ones, and a partial zero page at the
	// end.
	contents := make([]byte, 64*sparsePageSize+100)
	copy(contents[5*sparsePageSize:], bytes.Repeat([]byte("x"), sparsePageSize))
	copy(contents[40*sparsePageSize+17:], "middle")
	if err := data.Install(ids[0], bytes.NewReader(contents), int64(len(contents))); err != nil {
		t.Fatal(err)
	}
	file, err := data.Open(ids[0], int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatal("installed object does not read back as written")
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if allocated := info.Sys().(*syscall.Stat_t).Blocks * 512; allocated >= int64(len(contents))/2 {
		t.Fatalf("object of %d bytes has %d allocated, want its zero pages left as holes", len(contents), allocated)
	}
	if err := data.Install(ids[1], bytes.NewReader(contents[:10]), int64(len(contents))); err == nil {
		t.Fatal("Install of a short source succeeded")
	}
}

func TestLocalDataStoreInstallCrashBoundaries(t *testing.T) {
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
	ids, err := graph.ReserveObjects(origin, []ObjectKind{ObjectKindFile, ObjectKindFile})
	if err != nil {
		t.Fatal(err)
	}
	committed, abandoned := ids[0], ids[1]
	contents := bytes.Repeat([]byte("complete immutable bytes"), 1024)
	if err := data.Install(committed, bytes.NewReader(contents), int64(len(contents))); err != nil {
		t.Fatal(err)
	}
	if err := graph.CommitReservedObject(committed, "build/committed", uint64(len(contents)), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := data.Install(abandoned, bytes.NewReader(contents), int64(len(contents))); err != nil {
		t.Fatal(err)
	}

	// A crash before link(2) can leave only a hidden temporary. It is never
	// authoritative, even if its byte length happens to be complete.
	abandonedPath, err := data.objectPath(abandoned)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(filepath.Dir(abandonedPath), ".object-crash-boundary")
	if err := os.WriteFile(temporary, contents[:17], 0600); err != nil {
		t.Fatal(err)
	}
	committedPath, err := data.objectPath(committed)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Dir(committedPath), filepath.Dir(abandonedPath)} {
		if err := os.WriteFile(filepath.Join(directory, ".foreign"), []byte("unknown"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := data.VerifyReferenced(graph); err != nil {
		t.Fatalf("complete committed object failed validation: %v", err)
	}
	removed, quarantined, err := data.ReclaimUnreferenced(graph)
	if err != nil || removed != 2 || quarantined != 2 {
		t.Fatalf("reclaim crash residue=(%d,%d,%v), want (2,2,nil)", removed, quarantined, err)
	}
	if _, err := os.Stat(abandonedPath); !os.IsNotExist(err) {
		t.Fatalf("uncommitted published bytes survived: %v", err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("unpublished temporary survived: %v", err)
	}
	quarantine, err := os.ReadDir(filepath.Join(dir, "objects", "quarantine"))
	if err != nil || len(quarantine) != 2 {
		t.Fatalf("quarantine entries=%d err=%v, want 2", len(quarantine), err)
	}
	if err := os.Truncate(committedPath, int64(len(contents)-1)); err != nil {
		t.Fatal(err)
	}
	if err := data.VerifyReferenced(graph); err == nil {
		t.Fatal("truncated referenced object passed validation")
	}
}

func TestLocalDataStoreReplaceDeferredIsAtomic(t *testing.T) {
	dir := t.TempDir()
	data, err := OpenLocalDataStore(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	id := ObjectID{Snapshot: 1, Kind: ObjectKindFile, Ordinal: 1}
	old := []byte("previous-complete-object")
	if err := data.InstallDeferred(id, bytes.NewReader(old), int64(len(old))); err != nil {
		t.Fatal(err)
	}
	// An interrupted replacement cannot expose its prefix as a complete cache
	// object or destroy the previous complete object.
	if err := data.ReplaceDeferred(id, bytes.NewReader([]byte("short")), 100); err == nil {
		t.Fatal("short replacement unexpectedly succeeded")
	}
	file, err := data.Open(id, int64(len(old)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("object changed after interrupted replacement: %q, %v", got, err)
	}

	replacement := bytes.Repeat([]byte("complete"), 4096)
	if err := data.ReplaceDeferred(id, bytes.NewReader(replacement), int64(len(replacement))); err != nil {
		t.Fatal(err)
	}
	file, err = data.Open(id, int64(len(replacement)))
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement bytes=%d err=%v", len(got), err)
	}
}

func assertGraphReferenceModel(t *testing.T, store *Store) {
	t.Helper()
	inspection := store.Inspect()
	want := make(map[string]uint64, len(inspection.Objects))
	objects := make(map[string]Object, len(inspection.Objects))
	for _, object := range inspection.Objects {
		objects[object.ID.key()] = object
		want[object.ID.key()] = 0
	}
	retain := func(id ObjectID, source string) {
		if _, ok := objects[id.key()]; !ok {
			t.Fatalf("%s references missing object %s", source, id.key())
		}
		want[id.key()]++
	}
	for name, id := range inspection.Roots {
		retain(id, "root "+name)
	}
	for name, id := range inspection.Pins {
		retain(id, "pin "+name)
	}
	for _, object := range inspection.Objects {
		for _, edge := range object.Children {
			retain(edge.Child, "object "+object.ID.key())
		}
	}
	for key, object := range objects {
		if object.Refs != want[key] {
			t.Fatalf("object %s refs=%d, independently computed %d", key, object.Refs, want[key])
		}
		wantState := ObjectLive
		if want[key] == 0 {
			wantState = ObjectReclaiming
		}
		if object.State != wantState {
			t.Fatalf("object %s state=%q, want %q for %d refs", key, object.State, wantState, want[key])
		}
	}
	if err := store.Verify(); err != nil {
		t.Fatalf("graph verification: %v", err)
	}
}

func TestRandomGraphReferenceAndReplayModel(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}

	var random uint64 = 0x7c4a7f19d2e85b61
	nextRandom := func(limit int) int {
		random ^= random << 13
		random ^= random >> 7
		random ^= random << 17
		return int(random % uint64(limit))
	}
	names := func(values map[string]ObjectID) []string {
		result := make([]string, 0, len(values))
		for name := range values {
			result = append(result, name)
		}
		sort.Strings(result)
		return result
	}
	liveObjects := func() []ObjectID {
		var result []ObjectID
		for _, object := range store.Inspect().Objects {
			if object.State == ObjectLive {
				result = append(result, object.ID)
			}
		}
		return result
	}

	for step := 0; step < 1000; step++ {
		inspection := store.Inspect()
		live := liveObjects()
		switch nextRandom(8) {
		case 0, 1:
			children := make([]Edge, 0, 3)
			if len(live) != 0 {
				for slot, count := 0, nextRandom(4); slot < count; slot++ {
					children = append(children, Edge{Slot: uint16(slot), Child: live[nextRandom(len(live))]})
				}
			}
			if _, err := store.CreateObject(origin, fmt.Sprintf("construction/%04d", step), ObjectKindMetadata, []byte{byte(step)}, children); err != nil {
				t.Fatalf("step %d create: %v", step, err)
			}
		case 2:
			if len(live) != 0 {
				if err := store.SetRoot(fmt.Sprintf("branch/%02d", nextRandom(32)), live[nextRandom(len(live))]); err != nil {
					t.Fatalf("step %d set root: %v", step, err)
				}
			}
		case 3:
			rootNames := names(inspection.Roots)
			if len(rootNames) != 0 {
				if err := store.DeleteRoot(rootNames[nextRandom(len(rootNames))]); err != nil {
					t.Fatalf("step %d delete root: %v", step, err)
				}
			}
		case 4:
			if len(live) != 0 {
				if err := store.SetPin(fmt.Sprintf("open/%02d", nextRandom(32)), live[nextRandom(len(live))]); err != nil {
					t.Fatalf("step %d set pin: %v", step, err)
				}
			}
		case 5:
			pinNames := names(inspection.Pins)
			if len(pinNames) != 0 {
				if err := store.DeletePin(pinNames[nextRandom(len(pinNames))]); err != nil {
					t.Fatalf("step %d delete pin: %v", step, err)
				}
			}
		case 6:
			if _, err := store.Collect(1 + nextRandom(8)); err != nil {
				t.Fatalf("step %d collect: %v", step, err)
			}
		case 7:
			rootNames := names(inspection.Roots)
			if len(rootNames) != 0 {
				source := rootNames[nextRandom(len(rootNames))]
				working, forkErr := store.SealAndFork(origin, SnapshotSealedLocal, map[string]string{
					fmt.Sprintf("fork/%04d", step): source,
				}, 1)
				if forkErr != nil {
					t.Fatalf("step %d fork: %v", step, forkErr)
				}
				origin = working[0]
			}
		}
		assertGraphReferenceModel(t, store)
		if step%113 == 112 {
			if err := store.Checkpoint(); err != nil {
				t.Fatalf("step %d checkpoint: %v", step, err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("step %d close: %v", step, err)
			}
			store, err = Open(dir)
			if err != nil {
				t.Fatalf("step %d reopen: %v", step, err)
			}
			assertGraphReferenceModel(t, store)
		}
	}

	inspection := store.Inspect()
	for _, name := range names(inspection.Roots) {
		if err := store.DeleteRoot(name); err != nil {
			t.Fatal(err)
		}
	}
	inspection = store.Inspect()
	for _, name := range names(inspection.Pins) {
		if err := store.DeletePin(name); err != nil {
			t.Fatal(err)
		}
	}
	for store.Stats().Objects != 0 {
		collected, err := store.Collect(64)
		if err != nil {
			t.Fatal(err)
		}
		if collected == 0 {
			t.Fatal("unreachable objects remained but collection made no progress")
		}
	}
	assertGraphReferenceModel(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
