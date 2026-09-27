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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func ledgerObject(snapshot SnapshotID, ordinal uint64, kind ObjectKind, children ...Edge) Object {
	return Object{ID: ObjectID{Snapshot: snapshot, Ordinal: ordinal, Kind: kind}, State: ObjectLive, Children: children}
}

func TestReferenceLedgerInternalCascade(t *testing.T) {
	ledger := NewReferenceLedger()
	leaf := ledgerObject(1, 1, ObjectKindMemoryChunk)
	if err := ledger.ImportOrigin(1, []Object{leaf}, map[string]ObjectID{"branch/a": leaf.ID}); err != nil {
		t.Fatal(err)
	}
	parent := ledgerObject(2, 1, ObjectKindMemoryNode, Edge{Slot: 7, Child: leaf.ID})
	if err := ledger.ImportOrigin(2, []Object{parent}, map[string]ObjectID{"branch/b": parent.ID}); err != nil {
		t.Fatal(err)
	}
	inspection := ledger.Inspect()
	if got := inspection.Objects[leaf.ID]; got != 2 {
		t.Fatalf("leaf references=%d, want root+parent=2", got)
	}
	if err := ledger.ApplyReference("drop-a", "branch/a", leaf.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("drop-b", "branch/b", parent.ID, false); err != nil {
		t.Fatal(err)
	}
	if collected, err := ledger.Collect(10); err != nil || collected != 2 {
		t.Fatalf("Collect=(%d,%v), want (2,nil)", collected, err)
	}
	if got := len(ledger.Inspect().Objects); got != 0 {
		t.Fatalf("objects after cascade=%d, want 0", got)
	}
}

func TestReferenceLedgerNoOpCollectionDoesNotGrowJournal(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	object := ledgerObject(1, 1, ObjectKindMemoryChunk)
	if err := ledger.ImportOrigin(1, []Object{object}, map[string]ObjectID{"branch/a": object.ID}); err != nil {
		t.Fatal(err)
	}
	before := ledger.JournalBytes()
	for i := 0; i < 8; i++ {
		if collected, err := ledger.Collect(64 << 10); err != nil || collected != 0 {
			t.Fatalf("Collect()=(%d, %v), want (0, nil)", collected, err)
		}
	}
	if after := ledger.JournalBytes(); after != before {
		t.Fatalf("no-op collection grew journal from %d to %d bytes", before, after)
	}
}

func TestReferenceLedgerConvertsIncomingBridgeInEitherImportOrder(t *testing.T) {
	child := ledgerObject(21, 1, ObjectKindFile)
	parent := ledgerObject(22, 1, ObjectKindInode, Edge{Slot: 9, Child: child.ID})
	edgeName := edgeReferenceName(parent.ID, 9)
	for _, childFirst := range []bool{false, true} {
		ledger := NewReferenceLedger()
		importChild := func() error {
			return ledger.ImportOrigin(21, []Object{child}, map[string]ObjectID{"child-root": child.ID, edgeName: child.ID})
		}
		importParent := func() error {
			return ledger.ImportOrigin(22, []Object{parent}, map[string]ObjectID{"parent-root": parent.ID})
		}
		if childFirst {
			if err := importChild(); err != nil {
				t.Fatal(err)
			}
			if err := importParent(); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := importParent(); err != nil {
				t.Fatal(err)
			}
			if err := importChild(); err != nil {
				t.Fatal(err)
			}
		}
		inspection := ledger.Inspect()
		if got := inspection.Objects[child.ID]; got != 2 {
			t.Fatalf("childFirst=%t child references=%d, want root+internal edge=2", childFirst, got)
		}
		if _, bridged := inspection.References[edgeName]; bridged {
			t.Fatalf("childFirst=%t retained obsolete external edge bridge", childFirst)
		}
		objects, references, ok := ledger.OriginManifest(21)
		if !ok || len(objects) != 1 || objects[0].Refs != 2 {
			t.Fatalf("childFirst=%t origin manifest objects=%+v ok=%t", childFirst, objects, ok)
		}
		if references[edgeName] != child.ID || references["child-root"] != child.ID {
			t.Fatalf("childFirst=%t origin manifest references=%+v", childFirst, references)
		}
	}
}

func TestReferenceLedgerCrossOwnerOrderingAndDeduplication(t *testing.T) {
	childOwner := NewReferenceLedger()
	child := ledgerObject(1, 1, ObjectKindFile)
	if err := childOwner.ImportOrigin(1, []Object{child}, map[string]ObjectID{"source": child.ID}); err != nil {
		t.Fatal(err)
	}
	parentOwner := NewReferenceLedger()
	parent := ledgerObject(2, 1, ObjectKindInode, Edge{Slot: 3, Child: child.ID})
	if err := parentOwner.ImportOrigin(2, []Object{parent}, map[string]ObjectID{"target": parent.ID}); err != nil {
		t.Fatal(err)
	}
	outbox := parentOwner.PendingOutbound()
	if len(outbox) != 1 || !outbox[0].Retain || outbox[0].Child != child.ID {
		t.Fatalf("initial outbox=%+v", outbox)
	}
	activate := outbox[0]
	if err := childOwner.ApplyReference("release-before-retain", activate.Name, activate.Child, false); err == nil {
		t.Fatal("cross-owner release overtook its retain")
	}
	if err := childOwner.ApplyReference(activate.Operation, activate.Name, activate.Child, activate.Retain); err != nil {
		t.Fatal(err)
	}
	// A lost acknowledgement repeats input without double-counting.
	if err := childOwner.ApplyReference(activate.Operation, activate.Name, activate.Child, activate.Retain); err != nil {
		t.Fatal(err)
	}
	if got := childOwner.Inspect().Objects[child.ID]; got != 2 {
		t.Fatalf("child references after duplicate activation=%d, want 2", got)
	}
	if err := parentOwner.AcknowledgeOutbound(activate.Sequence + 1); err == nil {
		t.Fatal("out-of-order acknowledgement succeeded")
	}
	if err := parentOwner.AcknowledgeOutbound(activate.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := parentOwner.AcknowledgeOutbound(activate.Sequence); err != nil {
		t.Fatalf("repeated acknowledgement: %v", err)
	}
	if err := parentOwner.ApplyReference("drop-target", "target", parent.ID, false); err != nil {
		t.Fatal(err)
	}
	if collected, err := parentOwner.Collect(1); err != nil || collected != 1 {
		t.Fatalf("parent Collect=(%d,%v)", collected, err)
	}
	outbox = parentOwner.PendingOutbound()
	if len(outbox) != 1 || outbox[0].Retain || outbox[0].Child != child.ID {
		t.Fatalf("release outbox=%+v", outbox)
	}
	release := outbox[0]
	if release.Sequence <= activate.Sequence {
		t.Fatalf("release sequence=%d, activation=%d", release.Sequence, activate.Sequence)
	}
	if err := childOwner.ApplyReference(release.Operation, release.Name, release.Child, release.Retain); err != nil {
		t.Fatal(err)
	}
	if err := childOwner.ApplyReference("drop-source", "source", child.ID, false); err != nil {
		t.Fatal(err)
	}
	if collected, err := childOwner.Collect(1); err != nil || collected != 1 {
		t.Fatalf("child Collect=(%d,%v)", collected, err)
	}
}

func TestReferenceLedgerBatchedDeliveryAndReplay(t *testing.T) {
	childOwner := NewReferenceLedger()
	childA := ledgerObject(1, 1, ObjectKindFile)
	childB := ledgerObject(1, 2, ObjectKindFile)
	if err := childOwner.ImportOrigin(1, []Object{childA, childB}, map[string]ObjectID{
		"source/a": childA.ID, "source/b": childB.ID,
	}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "ledger")
	parentOwner, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	parent := ledgerObject(2, 1, ObjectKindInode,
		Edge{Slot: 1, Child: childA.ID}, Edge{Slot: 2, Child: childB.ID})
	if err := parentOwner.ImportOrigin(2, []Object{parent}, map[string]ObjectID{"target": parent.ID}); err != nil {
		t.Fatal(err)
	}
	if got := parentOwner.PendingOutboundBatch(1); len(got) != 1 {
		t.Fatalf("bounded pending outbound has %d entries, want 1", len(got))
	}
	pending := parentOwner.PendingOutboundBatch(8)
	if len(pending) != 2 {
		t.Fatalf("pending outbound has %d entries, want 2", len(pending))
	}
	mutations := make([]SnapshotReferenceMutation, len(pending))
	for i, outbound := range pending {
		mutations[i] = SnapshotReferenceMutation{
			Operation: outbound.Operation, Name: outbound.Name,
			Object: outbound.Child, Retain: outbound.Retain,
		}
	}
	if err := childOwner.ApplyReferences(mutations); err != nil {
		t.Fatal(err)
	}
	if err := parentOwner.AcknowledgeOutboundThrough(pending[1].Sequence); err != nil {
		t.Fatal(err)
	}
	if got := parentOwner.PendingOutbound(); len(got) != 0 {
		t.Fatalf("pending outbound after batch acknowledgement=%+v", got)
	}
	if err := parentOwner.Close(); err != nil {
		t.Fatal(err)
	}
	parentOwner, err = OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer parentOwner.Close()
	inspection := parentOwner.Inspect()
	if len(inspection.PendingOutbound) != 0 {
		t.Fatalf("replayed pending outbound=%+v", inspection.PendingOutbound)
	}
	if inspection.Objects[parent.ID] != 1 {
		t.Fatalf("replayed parent references=%d, want 1", inspection.Objects[parent.ID])
	}
	if err := parentOwner.AcknowledgeOutboundThrough(pending[1].Sequence); err != nil {
		t.Fatalf("repeated batch acknowledgement: %v", err)
	}
}

func TestReferenceLedgerImportRequiresCompleteBaseline(t *testing.T) {
	ledger := NewReferenceLedger()
	object := ledgerObject(1, 1, ObjectKindMetadata)
	if err := ledger.ImportOrigin(1, []Object{object}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("late", "late-root", object.ID, true); err == nil {
		t.Fatal("late reference resurrected an unreferenced imported object")
	}
	if collected, err := ledger.Collect(1); err != nil || collected != 1 {
		t.Fatalf("Collect=(%d,%v)", collected, err)
	}
}

func TestReferenceLedgerDurableReplayAndPartialTail(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	object := ledgerObject(9, 1, ObjectKindMemoryChunk)
	if err := ledger.ImportOrigin(9, []Object{object}, map[string]ObjectID{"branch": object.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReferenceLedger(directory); err == nil {
		t.Fatal("second writer opened a locked reference ledger")
	}
	if err := ledger.ApplyReference("release-branch", "branch", object.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(filepath.Join(directory, "ledger.log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	journal.Close()

	reopened, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	inspection := reopened.Inspect()
	if got := inspection.Objects[object.ID]; got != 0 {
		t.Fatalf("replayed references=%d, want 0", got)
	}
	if collected, err := reopened.Collect(1); err != nil || collected != 1 {
		t.Fatalf("replayed Collect=(%d,%v)", collected, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if got := len(final.Inspect().Objects); got != 0 {
		t.Fatalf("objects after collection replay=%d, want 0", got)
	}
}

func largeLedgerImport(t *testing.T, origin SnapshotID) ([]Object, map[string]ObjectID) {
	t.Helper()
	objects := make([]Object, 4)
	references := make(map[string]ObjectID, len(objects))
	for index := range objects {
		objects[index] = ledgerObject(origin, uint64(index+1), ObjectKindMetadata)
		objects[index].Data = make([]byte, 2<<20)
		objects[index].Data[0] = byte(index + 1)
		references["root/large/"+string(rune('a'+index))] = objects[index].ID
	}
	record, err := json.Marshal(referenceLedgerRecord{Version: referenceLedgerVersion, Sequence: 1, Import: &ledgerImportRecord{
		Origin: origin, Objects: objects, References: references,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(record) <= maxReferenceLedgerRecord {
		t.Fatalf("fixture import is %d bytes, want more than %d", len(record), maxReferenceLedgerRecord)
	}
	return objects, references
}

func referenceLedgerFrameOffsets(t *testing.T, data []byte) []int {
	t.Helper()
	offsets := []int{len(referenceLedgerHeader)}
	for offset := len(referenceLedgerHeader); offset < len(data); {
		if len(data)-offset < 8 {
			t.Fatalf("partial frame at %d", offset)
		}
		length := int(binary.BigEndian.Uint64(data[offset : offset+8]))
		offset += 8 + length
		if offset > len(data) {
			t.Fatalf("frame ends at %d past %d", offset, len(data))
		}
		offsets = append(offsets, offset)
	}
	return offsets
}

func TestReferenceLedgerLargeImportIsBoundedAtomicAndDurable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	objects, references := largeLedgerImport(t, 71)
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportOrigin(71, objects, references); err != nil {
		t.Fatal(err)
	}
	if got := len(ledger.Inspect().Objects); got != len(objects) {
		t.Fatalf("objects=%d, want %d", got, len(objects))
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "ledger.log"))
	if err != nil {
		t.Fatal(err)
	}
	offsets := referenceLedgerFrameOffsets(t, data)
	if len(offsets) < 4 {
		t.Fatalf("large import used %d frames, want begin, payload, and commit", len(offsets)-1)
	}
	for index := 1; index < len(offsets); index++ {
		if size := offsets[index] - offsets[index-1] - 8; size <= 0 || size > maxReferenceLedgerRecord {
			t.Fatalf("frame %d size=%d", index, size)
		}
	}
	reopened, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := len(reopened.Inspect().Objects); got != len(objects) {
		t.Fatalf("replayed objects=%d, want %d", got, len(objects))
	}
	if err := reopened.ImportOrigin(71, objects, references); err != nil {
		t.Fatalf("idempotent import retry: %v", err)
	}
}

func TestReferenceLedgerIncompleteLargeImportRollsBack(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	objects, references := largeLedgerImport(t, 72)
	ledger, err := OpenReferenceLedger(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportOrigin(72, objects, references); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, "ledger.log"))
	if err != nil {
		t.Fatal(err)
	}
	offsets := referenceLedgerFrameOffsets(t, data)
	for name, length := range map[string]int{
		"before-commit":   offsets[len(offsets)-2],
		"partial-payload": offsets[1] + (offsets[2]-offsets[1])/2,
	} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "ledger")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "ledger.log"), data[:length], 0600); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenReferenceLedger(directory)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.Inspect(); len(got.Origins) != 0 || len(got.Objects) != 0 {
				t.Fatalf("partial import became visible: %+v", got)
			}
			if got := reopened.JournalBytes(); got != int64(len(referenceLedgerHeader)) {
				t.Fatalf("rolled-back journal is %d bytes", got)
			}
			if err := reopened.ImportOrigin(72, objects, references); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReferenceLedgerHandoffCursorIsAtomicAndDurable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	first := ledgerObject(12, 1, ObjectKindMemoryChunk)
	second := ledgerObject(12, 2, ObjectKindMemoryChunk)
	baseline := map[string]ObjectID{"root/branch": first.ID, "pin/next": second.ID}
	if err := ledger.ImportOrigin(12, []Object{first, second}, baseline); err != nil {
		t.Fatal(err)
	}
	// The manifest-backed origin import is idempotent across a lost response.
	if err := ledger.ImportOrigin(12, []Object{first, second}, baseline); err != nil {
		t.Fatalf("retrying origin import: %v", err)
	}
	mutations := []SnapshotReferenceMutation{
		{Sequence: 1, Operation: "handoff-12-1", Name: "root/branch", Object: first.ID, Retain: false},
		{Sequence: 2, Operation: "handoff-12-2", Name: "root/branch", Object: second.ID, Retain: true},
		{Sequence: 3, Operation: "handoff-12-3", Name: "pin/next", Object: second.ID, Retain: false},
	}
	if err := ledger.ApplyHandoff(12, 0, mutations); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyHandoff(12, 0, mutations); err != nil {
		t.Fatalf("retrying handoff batch: %v", err)
	}
	bad := append([]SnapshotReferenceMutation(nil), mutations[:2]...)
	bad[1].Object = first.ID
	if err := ledger.ApplyHandoff(12, 0, bad); err == nil {
		t.Fatal("accepted a conflicting committed handoff prefix")
	}
	if err := ledger.ApplyHandoff(12, 4, nil); err == nil {
		t.Fatal("accepted a skipped handoff cursor")
	}
	if got := ledger.Inspect().HandoffCursors[12]; got != 3 {
		t.Fatalf("handoff cursor=%d, want 3", got)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	inspection := reopened.Inspect()
	if inspection.HandoffCursors[12] != 3 || inspection.References["root/branch"] != second.ID {
		t.Fatalf("replayed handoff=%+v", inspection)
	}
}

func TestReferenceLedgerCheckpointAndOldWALCrashPrefix(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	object := ledgerObject(12, 1, ObjectKindFile)
	if err := ledger.ImportOrigin(12, []Object{object}, map[string]ObjectID{"branch": object.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("pin", "runtime", object.ID, true); err != nil {
		t.Fatal(err)
	}
	oldWAL, err := os.ReadFile(filepath.Join(directory, "ledger.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := ledger.JournalBytes(); got != int64(len(referenceLedgerHeader)) {
		t.Fatalf("compacted journal is %d bytes, want %d", got, len(referenceLedgerHeader))
	}
	if err := ledger.ApplyReference("drop-runtime", "runtime", object.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	// Emulate a crash after checkpoint publication but before the old WAL was
	// replaced. Reopen must skip the prefix incorporated by the checkpoint.
	if err := os.WriteFile(filepath.Join(directory, "ledger.log"), oldWAL, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	inspection := reopened.Inspect()
	if got := inspection.Objects[object.ID]; got != 2 {
		t.Fatalf("references after old-WAL replay=%d, want 2", got)
	}
	if err := reopened.ApplyReference("drop-runtime", "runtime", object.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	final, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if got := final.Inspect().Objects[object.ID]; got != 1 {
		t.Fatalf("references after post-checkpoint replay=%d, want 1", got)
	}
}

func TestSnapshotLedgerCapturesForeignParentEdges(t *testing.T) {
	graph, err := Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	first, _ := graph.AllocateWorking()
	child, err := graph.CreateObject(first, "child-pin", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(first, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	second, _ := graph.AllocateWorking()
	parent, err := graph.CreateObject(second, "parent-pin", ObjectKindInode, nil, []Edge{{Slot: 4, Child: child}})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(second, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	objects, references, err := graph.SnapshotLedger(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || references["pin/child-pin"] != child || references[edgeReferenceName(parent, 4)] != child {
		t.Fatalf("objects=%+v references=%+v", objects, references)
	}
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(first, objects, references); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Inspect().Objects[child]; got != 2 {
		t.Fatalf("imported child references=%d, want pin+foreign parent=2", got)
	}
}

func TestOriginReferencesAfterHandoff(t *testing.T) {
	graph, err := Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	first, _ := graph.AllocateWorking()
	child, err := graph.CreateObject(first, "child-pin", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/live", child); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(first, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	// A later capture of the live branch, not handed off yet.
	second, _ := graph.AllocateWorking()
	if _, err := graph.CreateObject(second, "capture-pin", ObjectKindInode, nil, []Edge{{Slot: 1, Child: child}}); err != nil {
		t.Fatal(err)
	}
	// An origin handed off with the first, whose edge the shared ledger counts.
	third, _ := graph.AllocateWorking()
	if _, err := graph.CreateObject(third, "shared-pin", ObjectKindInode, nil, []Edge{{Slot: 2, Child: child}}); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []SnapshotID{second, third} {
		if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
			t.Fatal(err)
		}
	}
	for _, origin := range []SnapshotID{first, third} {
		if err := graph.MarkShared(origin); err != nil {
			t.Fatal(err)
		}
	}
	references, edges := graph.OriginReferences(first)
	if len(references) != 2 || references["root/branch/live"] != child || references["pin/child-pin"] != child || !edges {
		t.Fatalf("OriginReferences = %v, %t; want the root, the pin and an edge", references, edges)
	}
	if err := graph.DeleteRoots([]string{"branch/live"}); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("child-pin"); err != nil {
		t.Fatal(err)
	}
	if references, edges := graph.OriginReferences(first); len(references) != 0 || !edges {
		t.Fatalf("OriginReferences = %v, %t; want only the capture's edge", references, edges)
	}
	// The capture's object, no longer referenced, is not live.
	if err := graph.DeletePin("capture-pin"); err != nil {
		t.Fatal(err)
	}
	if references, edges := graph.OriginReferences(first); len(references) != 0 || edges {
		t.Fatalf("OriginReferences = %v, %t; want nothing", references, edges)
	}
}

func TestOriginReferencesSeeDeferredSources(t *testing.T) {
	graph, err := Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	first, _ := graph.AllocateWorking()
	source, err := graph.CreateObject(first, "source-pin", ObjectKindMetadata, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(first, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := graph.MarkShared(first); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("source-pin"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := graph.AllocateWorking()
	if _, err := graph.ImportDeferredFilesystemRoot(catalog, "filesystem/imported", "repository:root:2", source); err != nil {
		t.Fatal(err)
	}
	if references, edges := graph.OriginReferences(first); len(references) != 0 || !edges {
		t.Fatalf("OriginReferences = %v, %t; want the deferred root's source", references, edges)
	}
}

func TestSnapshotLedgerFinalBarrierIsDurableAndOriginScoped(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "graph")
	graph, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	first, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	child, err := graph.CreateObject(first, "build/child", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/first", child); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/child"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(first, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	objects, references, err := graph.FreezeSnapshotLedger(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || references["root/branch/first"] != child || !graph.SnapshotLedgerFrozen(first) {
		t.Fatalf("frozen baseline objects=%+v references=%+v", objects, references)
	}
	if frozen := graph.FrozenSnapshotLedgers(); len(frozen) != 1 || frozen[0] != first {
		t.Fatalf("frozen ledgers = %v, want [%d]", frozen, first)
	}
	if err := graph.DeleteRoot("branch/first"); err == nil {
		t.Fatal("deleted a root through a frozen handoff barrier")
	}
	second, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CreateObject(second, "build/foreign-parent", ObjectKindInode, nil, []Edge{{Slot: 1, Child: child}}); err == nil {
		t.Fatal("created an incoming edge through a frozen handoff barrier")
	}
	independent, err := graph.CreateObject(second, "build/independent", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatalf("unrelated origin mutation was blocked: %v", err)
	}
	if err := graph.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.SnapshotLedgerFrozen(first) {
		t.Fatal("final barrier was lost across reopen")
	}
	// A restarted provider lists the barriers that it must roll forward.
	if frozen := reopened.FrozenSnapshotLedgers(); len(frozen) != 1 || frozen[0] != first {
		t.Fatalf("frozen ledgers after reopen = %v, want [%d]", frozen, first)
	}
	if err := reopened.MarkShared(first); err != nil {
		t.Fatalf("authority state transition was blocked by reference barrier: %v", err)
	}
	if err := reopened.UnfreezeSnapshotLedger(first); err != nil {
		t.Fatal(err)
	}
	if frozen := reopened.FrozenSnapshotLedgers(); len(frozen) != 0 {
		t.Fatalf("frozen ledgers after cancellation = %v", frozen)
	}
	if err := reopened.DeleteRoot("branch/first"); err != nil {
		t.Fatalf("reference mutation remained blocked after cancellation: %v", err)
	}
	if _, ok := reopened.Object(independent); !ok {
		t.Fatal("unrelated origin object disappeared")
	}
}

func TestSnapshotLedgerHandoffCatchupIsDurableAndExact(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "graph")
	graph, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := graph.CreateObject(origin, "build/first", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graph.CreateObject(origin, "build/second", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/source", first); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/first"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	objects, baseline, cursor, err := graph.BeginSnapshotLedgerHandoff(origin)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || baseline["root/branch/source"] != first || baseline["pin/build/second"] != second {
		t.Fatalf("cursor=%d baseline=%+v", cursor, baseline)
	}
	if err := graph.SetRoot("branch/source", second); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/second"); err != nil {
		t.Fatal(err)
	}
	other, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := graph.CreateObject(other, "build/parent", ObjectKindInode, nil, []Edge{{Slot: 7, Child: second}})
	if err != nil {
		t.Fatal(err)
	}

	changes, cursor, frozen, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if frozen || cursor != 4 || len(changes) != 4 {
		t.Fatalf("changes=%+v cursor=%d frozen=%t", changes, cursor, frozen)
	}
	for index, mutation := range changes {
		if mutation.Sequence != uint64(index+1) || mutation.Operation == "" {
			t.Fatalf("mutation %d=%+v", index, mutation)
		}
	}
	if changes[0].Name != "root/branch/source" || changes[0].Object != first || changes[0].Retain ||
		changes[1].Name != "root/branch/source" || changes[1].Object != second || !changes[1].Retain {
		t.Fatalf("root replacement ordering=%+v", changes[:2])
	}
	if got, err := graph.Collect(100); err != nil || got != 0 {
		t.Fatalf("collection crossed an active handoff: got=%d err=%v", got, err)
	}
	if err := graph.Close(); err != nil {
		t.Fatal(err)
	}

	graph, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	repeatedObjects, repeatedBaseline, repeatedCursor, err := graph.BeginSnapshotLedgerHandoff(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeatedObjects) != len(objects) || repeatedCursor != cursor || !sameObjectReferences(repeatedBaseline, baseline) {
		t.Fatalf("reopened handoff changed: objects=%d/%d cursor=%d/%d baseline=%+v/%+v", len(repeatedObjects), len(objects), repeatedCursor, cursor, repeatedBaseline, baseline)
	}
	frozenObjects, frozenBaseline, err := graph.FreezeSnapshotLedger(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozenObjects) != len(objects) || !sameObjectReferences(frozenBaseline, baseline) {
		t.Fatalf("freeze changed baseline: objects=%d/%d baseline=%+v/%+v", len(frozenObjects), len(objects), frozenBaseline, baseline)
	}
	// References moved since the baseline: the root from first to second, and
	// the pin of second away. Objects returned with the baseline must count
	// the baseline's references, not the current ones, or a receiver that
	// applies the recorded mutations counts them twice.
	for name, returned := range map[string][]Object{"repeated": repeatedObjects, "frozen": frozenObjects} {
		for index, object := range returned {
			want := objects[index]
			if object.ID != want.ID || object.Refs != want.Refs || object.State != want.State {
				t.Fatalf("%s handoff object %s refs=%d state=%s, baseline object refs=%d state=%s", name, object.ID, object.Refs, object.State, want.Refs, want.State)
			}
		}
	}
	changes, finalCursor, frozen, err := graph.SnapshotLedgerHandoffChanges(origin, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !frozen || finalCursor != cursor || len(changes) != 2 {
		t.Fatalf("final suffix=%+v cursor=%d frozen=%t", changes, finalCursor, frozen)
	}
	if err := graph.DeleteRoot("branch/source"); err == nil {
		t.Fatal("mutated references after final handoff freeze")
	}

	// The receiver of a handoff retried after the provider restarted imports
	// what the retry returned.
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, repeatedObjects, repeatedBaseline); err != nil {
		t.Fatal(err)
	}
	allChanges, _, _, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range allChanges {
		if err := ledger.ApplyReference(mutation.Operation, mutation.Name, mutation.Object, mutation.Retain); err != nil {
			t.Fatal(err)
		}
	}
	inspection := ledger.Inspect()
	currentObjects, currentReferences, err := graph.SnapshotLedger(origin)
	if err != nil {
		t.Fatal(err)
	}
	if !sameObjectReferences(inspection.References, currentReferences) {
		t.Fatalf("receiver references=%+v provider=%+v", inspection.References, currentReferences)
	}
	for _, object := range currentObjects {
		if got := inspection.Objects[object.ID]; got != object.Refs {
			t.Fatalf("receiver object %s refs=%d, provider=%d", object.ID, got, object.Refs)
		}
	}
	if currentReferences[edgeReferenceName(parent, 7)] != second {
		t.Fatalf("foreign edge missing from final references: %+v", currentReferences)
	}
}

func TestSnapshotLedgerHandoffTracksPreparedFilesystemTransactions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "graph")
	graph, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	first, err := graph.CreateObject(origin, "build/first", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graph.CreateObject(origin, "build/second", ObjectKindFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	filesystemOrigin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	rootName := "branch/main/filesystem"
	if _, err := graph.ApplyFilesystem(filesystemOrigin, rootName, []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}},
		{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, File: first}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "value", Inode: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := graph.BeginSnapshotLedgerHandoff(origin); err != nil {
		t.Fatal(err)
	}

	// ApplyFilesystem uses commitLockedPrepared. Replacing the root twice
	// must advance the handoff cursor in the writer as well as in the WAL.
	for index := 0; index < 2; index++ {
		file := second
		if index == 1 {
			file = first
		}
		if _, err := graph.ApplyFilesystem(filesystemOrigin, rootName, []FilesystemOperation{
			{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, File: file, MtimeNsec: int64(index + 1)}},
		}); err != nil {
			t.Fatalf("filesystem mutation %d: %v", index, err)
		}
	}
	changes, cursor, _, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != uint64(len(changes)) || cursor == 0 {
		t.Fatalf("handoff cursor=%d changes=%d", cursor, len(changes))
	}
	for index, change := range changes {
		if change.Sequence != uint64(index+1) {
			t.Fatalf("change %d sequence=%d", index, change.Sequence)
		}
	}
	if err := graph.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("reopening graph after prepared handoff mutations: %v", err)
	}
	defer reopened.Close()
	replayed, replayedCursor, _, err := reopened.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if replayedCursor != cursor || len(replayed) != len(changes) {
		t.Fatalf("replayed cursor=%d changes=%d, want %d/%d", replayedCursor, len(replayed), cursor, len(changes))
	}
}

func TestReferenceLedgerRetiresOnlyEmptyDrainedOrigin(t *testing.T) {
	origin := SnapshotID(41)
	object := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindFile, Ordinal: 1}}
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, []Object{object}, map[string]ObjectID{"root/branch/demo": object.ID}); err != nil {
		t.Fatal(err)
	}
	if got := ledger.EmptyOrigins(); len(got) != 0 {
		t.Fatalf("live origin reported empty: %v", got)
	}
	if err := ledger.RetireOrigin(origin); err == nil {
		t.Fatal("retired referenced origin")
	}
	if err := ledger.ApplyReference("release-demo", "root/branch/demo", object.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.Collect(16); err != nil || got != 1 {
		t.Fatalf("Collect()=(%d, %v), want (1, nil)", got, err)
	}
	if got := ledger.EmptyOrigins(); len(got) != 1 || got[0] != origin {
		t.Fatalf("empty origins=%v, want [%d]", got, origin)
	}
	if err := ledger.RetireOrigin(origin); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RetireOrigin(origin); err != nil {
		t.Fatalf("idempotent retirement: %v", err)
	}
	if got := ledger.Inspect(); len(got.Origins) != 0 || len(got.Objects) != 0 || got.AppliedOperations != 0 {
		t.Fatalf("retired ledger residue: %+v", got)
	}
}

func TestReferenceLedgerOriginManifestExcludesPendingReclamation(t *testing.T) {
	origin := SnapshotID(42)
	child := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindFile, Ordinal: 1}}
	parent := Object{
		ID:       ObjectID{Snapshot: origin, Kind: ObjectKindInode, Ordinal: 2},
		Children: []Edge{{Slot: 0, Child: child.ID}},
	}
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, []Object{child, parent}, map[string]ObjectID{"root/branch/demo": parent.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("release-demo", "root/branch/demo", parent.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.Collect(1); err != nil || got != 1 {
		t.Fatalf("Collect()=(%d, %v), want (1, nil)", got, err)
	}
	objects, references, ok := ledger.OriginManifest(origin)
	if !ok || len(objects) != 0 || len(references) != 0 {
		t.Fatalf("origin manifest exposes reclaiming descendants: objects=%+v references=%+v ok=%t", objects, references, ok)
	}
	if _, _, ok, ready := ledger.OriginCompactionManifest(origin); !ok || ready {
		t.Fatalf("partially collected origin compaction readiness=(ok=%t, ready=%t), want (true, false)", ok, ready)
	}
	inspection := ledger.Inspect()
	if got := inspection.Objects[child.ID]; got != 0 {
		t.Fatalf("pending child refcount=%d, want 0", got)
	}
}

func TestReferenceLedgerCompactionWaitsForAllDeadParentEdges(t *testing.T) {
	origin := SnapshotID(43)
	child := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindFile, Ordinal: 1}}
	parentA := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindInode, Ordinal: 2}, Children: []Edge{{Slot: 0, Child: child.ID}}}
	parentB := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindInode, Ordinal: 3}, Children: []Edge{{Slot: 0, Child: child.ID}}}
	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, []Object{child, parentA, parentB}, map[string]ObjectID{"root/a": parentA.ID, "root/b": parentB.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("release-a", "root/a", parentA.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("release-b", "root/b", parentB.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.Collect(1); err != nil || got != 1 {
		t.Fatalf("Collect(1)=(%d, %v), want (1, nil)", got, err)
	}
	objects, _, ok, ready := ledger.OriginCompactionManifest(origin)
	if !ok || ready || len(objects) != 1 || objects[0].ID != child.ID {
		t.Fatalf("partial compaction view=(objects=%+v, ok=%t, ready=%t), want the live child and not ready", objects, ok, ready)
	}
	if got, err := ledger.Collect(8); err != nil || got != 2 {
		t.Fatalf("final Collect=(%d, %v), want (2, nil)", got, err)
	}
	objects, references, ok, ready := ledger.OriginCompactionManifest(origin)
	if !ok || !ready || len(objects) != 0 || len(references) != 0 {
		t.Fatalf("final compaction view=(objects=%+v references=%+v ok=%t ready=%t)", objects, references, ok, ready)
	}
}

func TestReferenceLedgerRetirementRecovers(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	origin := SnapshotID(52)
	object := Object{ID: ObjectID{Snapshot: origin, Kind: ObjectKindMemoryChunk, Ordinal: 1}, BulkLength: 4096}
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportOrigin(origin, []Object{object}, map[string]ObjectID{}); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.Collect(16); err != nil || got != 1 {
		t.Fatalf("Collect()=(%d, %v), want (1, nil)", got, err)
	}
	if err := ledger.RetireOrigin(origin); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err = OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if got := ledger.Inspect(); len(got.Origins) != 0 || len(got.Objects) != 0 {
		t.Fatalf("retired origin reappeared after replay: %+v", got)
	}
}

// A mutation works on the ledger's state in place: one refused part way, or
// one whose journal record is not written, must leave the state as it was.
func TestReferenceLedgerRefusedMutationChangesNothing(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	a := ledgerObject(1, 1, ObjectKindMemoryChunk)
	b := ledgerObject(1, 2, ObjectKindMemoryChunk)
	if err := ledger.ImportOrigin(1, []Object{a, b}, map[string]ObjectID{"root/a": a.ID, "root/b": b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyHandoff(1, 0, []SnapshotReferenceMutation{
		{Sequence: 1, Operation: "handoff-1-1", Name: "root/c", Object: a.ID, Retain: true},
	}); err != nil {
		t.Fatal(err)
	}
	before := ledger.Inspect()
	unchanged := func(what string) {
		t.Helper()
		if after := ledger.Inspect(); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s changed the ledger:\nbefore %+v\nafter  %+v", what, before, after)
		}
		checkLedgerIndexes(t, ledger)
	}

	if err := ledger.ApplyReferences([]SnapshotReferenceMutation{
		{Operation: "retain-x", Name: "root/x", Object: a.ID, Retain: true},
		{Operation: "release-b", Name: "root/b", Object: b.ID},
		{Operation: "release-missing", Name: "root/missing", Object: a.ID},
	}); err == nil {
		t.Fatal("a batch releasing a missing reference was applied")
	}
	unchanged("a batch refused part way")

	if err := ledger.ApplyHandoff(1, 1, []SnapshotReferenceMutation{
		{Sequence: 2, Operation: "handoff-1-2", Name: "root/c", Object: a.ID},
		{Sequence: 3, Operation: "handoff-1-3", Name: "root/missing", Object: b.ID},
	}); err == nil {
		t.Fatal("a handoff releasing a missing reference was applied")
	}
	unchanged("a handoff refused part way")

	// The import adds its objects, counts its internal edge, and queues a
	// retain of the unowned child, before its baseline is refused.
	foreign := ledgerObject(9, 1, ObjectKindMemoryChunk)
	child := ledgerObject(2, 2, ObjectKindMemoryChunk)
	parent := ledgerObject(2, 1, ObjectKindMemoryNode, Edge{Slot: 1, Child: child.ID}, Edge{Slot: 2, Child: foreign.ID}, Edge{Slot: 3, Child: a.ID})
	if err := ledger.ImportOrigin(2, []Object{parent, child}, map[string]ObjectID{"root/parent": parent.ID, "root/elsewhere": b.ID, "root/unknown": ledgerObject(2, 9, ObjectKindMemoryChunk).ID}); err == nil {
		t.Fatal("an import whose baseline names an unknown object was applied")
	}
	unchanged("an import refused part way")

	// A mutation whose record cannot be written is undone, and the ledger
	// refuses further mutations until it is reopened.
	if err := ledger.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyReference("release-a", "root/a", a.ID, false); err == nil {
		t.Fatal("a mutation whose record was not written was applied")
	}
	unchanged("a mutation whose record was not written")
	if err := ledger.ApplyReference("retain-y", "root/y", b.ID, true); err == nil {
		t.Fatal("a poisoned ledger applied a mutation")
	}
	unchanged("a poisoned ledger's mutation")
	ledger.journal = nil
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if replayed := reopened.Inspect(); !reflect.DeepEqual(replayed, before) {
		t.Fatalf("replayed ledger differs:\nwant %+v\ngot  %+v", before, replayed)
	}
}

// checkLedgerIndexes fails unless the ledger's indexes of its objects are
// what the objects make them.
func checkLedgerIndexes(t *testing.T, ledger *ReferenceLedger) {
	t.Helper()
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	byOrigin := make(map[SnapshotID]map[string]struct{})
	reclaiming := make(map[string]struct{})
	for key, object := range ledger.state.Objects {
		if byOrigin[object.ID.Snapshot] == nil {
			byOrigin[object.ID.Snapshot] = make(map[string]struct{})
		}
		byOrigin[object.ID.Snapshot][key] = struct{}{}
		if object.State == ObjectReclaiming {
			reclaiming[key] = struct{}{}
		}
	}
	for origin, keys := range byOrigin {
		if !reflect.DeepEqual(ledger.state.originKeys(origin), keys) {
			t.Fatalf("origin %d indexes objects %v, has %v", origin, ledger.state.originKeys(origin), keys)
		}
	}
	for origin, entry := range ledger.state.byOrigin {
		if _, ok := ledger.state.Origins[origin]; !ok {
			t.Fatalf("origin %d is indexed but not owned", origin)
		}
		if len(entry.keys) != len(byOrigin[origin]) {
			t.Fatalf("origin %d indexes %d objects, has %d", origin, len(entry.keys), len(byOrigin[origin]))
		}
	}
	if !reflect.DeepEqual(ledger.state.reclaiming, reclaiming) {
		t.Fatalf("reclaiming index %v, reclaiming objects %v", ledger.state.reclaiming, reclaiming)
	}
}

// The ledger indexes its objects by origin, and the ones being reclaimed. The
// indexes must follow every mutation, refused ones included, and an origin's
// change count must advance whenever its live objects change.
func TestReferenceLedgerIndexesFollowMutations(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenReferenceLedger(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ledger.Close() }()
	random := rand.New(rand.NewSource(1))
	type liveView struct {
		changes uint64
		live    map[ObjectID]struct{}
	}
	seen := make(map[SnapshotID]liveView)
	check := func(step string) {
		t.Helper()
		checkLedgerIndexes(t, ledger)
		for origin, count := range ledger.OriginChanges() {
			live, changes, ok, _ := ledger.OriginLiveObjects(origin)
			if !ok || changes != count {
				t.Fatalf("%s: origin %d reads count %d (ok %t), listed with %d", step, origin, changes, ok, count)
			}
			if prior, ok := seen[origin]; ok && prior.changes == count && !reflect.DeepEqual(prior.live, live) {
				t.Fatalf("%s: origin %d's live objects changed from %v to %v without its count advancing", step, origin, prior.live, live)
			}
			seen[origin] = liveView{changes: count, live: live}
		}
	}
	references := make(map[string]ObjectID)
	live := func() []ObjectID {
		var result []ObjectID
		ledger.mu.RLock()
		for _, object := range ledger.state.Objects {
			if object.State == ObjectLive {
				result = append(result, object.ID)
			}
		}
		ledger.mu.RUnlock()
		slices.SortFunc(result, compareObjectIDs)
		return result
	}
	next := SnapshotID(1)
	operation, retired := 0, 0
	for step := 0; step < 600; step++ {
		operation++
		var what string
		switch choice := random.Intn(10); {
		case choice == 0 || next == 1:
			// An origin of a node and its chunks, some of the node's edges to
			// live objects of earlier origins.
			what = "import"
			origin := next
			next++
			chunks := 1 + random.Intn(6)
			var objects []Object
			var edges []Edge
			for ordinal := 2; ordinal < 2+chunks; ordinal++ {
				chunk := ledgerObject(origin, uint64(ordinal), ObjectKindMemoryChunk)
				objects = append(objects, chunk)
				edges = append(edges, Edge{Slot: uint16(ordinal), Child: chunk.ID})
			}
			if earlier := live(); len(earlier) != 0 {
				for i := random.Intn(3); i > 0; i-- {
					edges = append(edges, Edge{Slot: uint16(100 + i), Child: earlier[random.Intn(len(earlier))]})
				}
			}
			node := ledgerObject(origin, 1, ObjectKindMemoryNode, edges...)
			objects = append(objects, node)
			name := fmt.Sprintf("root/%d", origin)
			baseline := map[string]ObjectID{name: node.ID}
			if random.Intn(5) == 0 {
				// Refused part way: the baseline names an object of no origin.
				baseline["root/unknown"] = ledgerObject(origin, 99, ObjectKindMemoryChunk).ID
				if err := ledger.ImportOrigin(origin, objects, baseline); err == nil {
					t.Fatal("an import naming an unknown object was applied")
				}
				what = "refused import"
				break
			}
			if err := ledger.ImportOrigin(origin, objects, baseline); err != nil {
				t.Fatalf("importing origin %d: %v", origin, err)
			}
			references[name] = node.ID
		case choice <= 3:
			what = "retain"
			if objects := live(); len(objects) != 0 {
				name := fmt.Sprintf("root/extra-%d", operation)
				id := objects[random.Intn(len(objects))]
				if err := ledger.ApplyReference(fmt.Sprintf("retain-%d", operation), name, id, true); err != nil {
					t.Fatalf("retaining %s: %v", id, err)
				}
				references[name] = id
			}
		case choice <= 6:
			what = "release"
			names := make([]string, 0, len(references))
			for name := range references {
				names = append(names, name)
			}
			if len(names) != 0 {
				slices.Sort(names)
				name := names[random.Intn(len(names))]
				if err := ledger.ApplyReference(fmt.Sprintf("release-%d", operation), name, references[name], false); err != nil {
					t.Fatalf("releasing %q: %v", name, err)
				}
				delete(references, name)
			}
		case choice == 7:
			what = "collect"
			if _, err := ledger.Collect(1 + random.Intn(16)); err != nil {
				t.Fatal(err)
			}
		case choice == 8:
			what = "refused batch"
			if objects := live(); len(objects) != 0 {
				if err := ledger.ApplyReferences([]SnapshotReferenceMutation{
					{Operation: fmt.Sprintf("retain-%d", operation), Name: fmt.Sprintf("root/extra-%d", operation), Object: objects[0], Retain: true},
					{Operation: fmt.Sprintf("release-%d", operation), Name: "root/missing", Object: objects[0]},
				}); err == nil {
					t.Fatal("a batch releasing a missing reference was applied")
				}
			}
		default:
			what = "retire"
			for _, origin := range ledger.EmptyOrigins() {
				if err := ledger.RetireOrigin(origin); err != nil {
					t.Fatalf("retiring origin %d: %v", origin, err)
				}
				delete(seen, origin)
				retired++
			}
		}
		check(fmt.Sprintf("step %d (%s)", step, what))
	}
	if retired == 0 {
		t.Fatal("no origin emptied to be retired")
	}
	for _, reopen := range []string{"replayed", "checkpointed"} {
		if reopen == "checkpointed" {
			if err := ledger.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		before := ledger.Inspect()
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		if ledger, err = OpenReferenceLedger(directory); err != nil {
			t.Fatal(err)
		}
		if after := ledger.Inspect(); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s ledger differs:\nwant %+v\ngot  %+v", reopen, before, after)
		}
		checkLedgerIndexes(t, ledger)
	}
}

// A mutation during a handoff is previewed in place and undone, and the
// handoff's catch-up read from what it touched: the catch-up must record
// exactly the references each mutation changes of the snapshot, whatever the
// mutation, and a frozen snapshot's refusal must leave the graph as it was.
func TestSnapshotLedgerHandoffCatchupFollowsRandomMutations(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "graph")
	graph, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	var files []ObjectID
	for index := 0; index < 12; index++ {
		file, err := graph.CreateObject(origin, fmt.Sprintf("build/%d", index), ObjectKindFile, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if err := graph.SetRoot("branch/r0", files[0]); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	other, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	filesystem, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	const tree = "branch/main/filesystem"
	if _, err := graph.ApplyFilesystem(filesystem, tree, []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}},
		{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, File: files[1]}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "value", Inode: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	_, baseline, cursor, err := graph.BeginSnapshotLedgerHandoff(origin)
	if err != nil {
		t.Fatal(err)
	}
	references := cloneObjectReferences(baseline)
	current := func() map[string]ObjectID {
		graph.mu.RLock()
		defer graph.mu.RUnlock()
		return snapshotReferences(graph.state, origin)
	}
	random := rand.New(rand.NewSource(1))
	var parents []string
	for step := 0; step < 400; step++ {
		file := files[random.Intn(len(files))]
		name := fmt.Sprintf("r%d", random.Intn(4))
		var err error
		switch random.Intn(8) {
		case 0:
			err = graph.SetRoot("branch/"+name, file)
		case 1:
			if _, ok := graph.Root("branch/" + name); ok {
				err = graph.DeleteRoot("branch/" + name)
			}
		case 2:
			err = graph.SetPin("hold/"+name, file)
		case 3:
			if graph.HasPin("hold/" + name) {
				err = graph.DeletePin("hold/" + name)
			}
		case 4:
			pin := fmt.Sprintf("parent/%d", step)
			edges := []Edge{{Slot: 1, Child: file}, {Slot: 2, Child: files[random.Intn(len(files))]}}
			if _, err = graph.CreateObject(other, pin, ObjectKindInode, nil, edges); err == nil {
				parents = append(parents, pin)
			}
		case 5:
			if len(parents) != 0 {
				index := random.Intn(len(parents))
				err = graph.DeletePin(parents[index])
				parents = append(parents[:index], parents[index+1:]...)
			}
		case 6:
			_, err = graph.ApplyFilesystem(filesystem, tree, []FilesystemOperation{
				{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, File: file, MtimeNsec: int64(step)}},
			})
		case 7:
			_, err = graph.Collect(16)
		}
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		changes, latest, _, err := graph.SnapshotLedgerHandoffChanges(origin, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range changes {
			if change.Retain {
				references[change.Name] = change.Object
				continue
			}
			if references[change.Name] != change.Object {
				t.Fatalf("step %d releases %q of %s, which selects %s", step, change.Name, change.Object, references[change.Name])
			}
			delete(references, change.Name)
		}
		cursor = latest
		if want := current(); !reflect.DeepEqual(references, want) {
			t.Fatalf("step %d: the baseline and catch-up give\n%v\nthe graph holds\n%v", step, references, want)
		}
		if err := graph.Verify(); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}

	if _, _, err := graph.FreezeSnapshotLedger(origin); err != nil {
		t.Fatal(err)
	}
	before := graph.Inspect()
	if err := graph.SetRoot("branch/frozen", files[2]); !errors.Is(err, ErrSnapshotLedgerFrozen) {
		t.Fatalf("a new root of a frozen snapshot = %v, want it refused", err)
	}
	if _, err := graph.ApplyFilesystem(filesystem, tree, []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644, Nlink: 1, File: files[3], MtimeNsec: -1}},
	}); !errors.Is(err, ErrSnapshotLedgerFrozen) {
		t.Fatalf("a filesystem change of a frozen snapshot's references = %v, want it refused", err)
	}
	if after := graph.Inspect(); !reflect.DeepEqual(after, before) {
		t.Fatal("refused mutations of a frozen snapshot changed the graph")
	}
	if _, err := graph.CreateObject(other, "unrelated", ObjectKindFile, nil, nil); err != nil {
		t.Fatalf("a mutation of another snapshot while one is frozen: %v", err)
	}
	final := graph.Inspect()
	changes, _, _, err := graph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, _, _, err := reopened.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, changes) || !reflect.DeepEqual(reopened.Inspect(), final) {
		t.Fatal("the replayed graph differs from the one written")
	}
}
