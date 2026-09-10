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
