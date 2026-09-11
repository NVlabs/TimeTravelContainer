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
	"os"
	"path/filepath"
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
	if err := reopened.MarkShared(first); err != nil {
		t.Fatalf("authority state transition was blocked by reference barrier: %v", err)
	}
	if err := reopened.UnfreezeSnapshotLedger(first); err != nil {
		t.Fatal(err)
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

	ledger := NewReferenceLedger()
	if err := ledger.ImportOrigin(origin, objects, baseline); err != nil {
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
