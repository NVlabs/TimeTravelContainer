// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

func TestIsHeldPagePin(t *testing.T) {
	object := ttcsnapshot.ObjectID{Snapshot: 0x130, Kind: ttcsnapshot.ObjectKindMemoryChunk, Ordinal: 1}
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "remote/305/fork-1/" + heldPinComponent + "/" + object.String(), want: true},
		{name: "import/7/" + heldPinComponent + "/" + object.String(), want: true},
		{name: "remote/305/fork-1/resource-0000", want: false},
		{name: "remote/305/fork-1/" + heldPinComponent + "/resource-0000", want: false},
		{name: heldPinComponent + "/" + object.String(), want: false},
		{name: "remote/305/fork-1/other/" + object.String(), want: false},
	} {
		if got := IsHeldPagePin(test.name); got != test.want {
			t.Errorf("IsHeldPagePin(%q) = %t, want %t", test.name, got, test.want)
		}
	}
}

// A capture's handoff can freeze an origin whose pages another capture holds,
// until the handoff ends. The hold must go once it has.
func TestHeldPagePinsOutliveTheirOriginsHandoff(t *testing.T) {
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := graph.CreateObject(origin, "build/object", ttcsnapshot.ObjectKindMetadata, []byte("object"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/root", object); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("build/object"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	held := "remote/2/fork-1/" + heldPinComponent + "/" + object.String()
	if err := graph.SetPin(held, object); err != nil {
		t.Fatal(err)
	}
	if _, _, err := graph.FreezeSnapshotLedger(origin); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseHeldPagePins(graph, []string{held}); err != nil {
		t.Fatalf("releasing a hold on an origin frozen for its handoff: %v", err)
	}
	if !graph.HasPin(held) {
		t.Fatal("a hold on an origin frozen for its handoff went while it was frozen")
	}
	if err := graph.UnfreezeSnapshotLedger(origin); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); graph.HasPin(held); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a hold on an origin outlived its handoff by 10s")
		}
	}
}

// A transaction maps more tables after one whose tables the graph shared,
// but never more than new tables could fill a transaction with.
func TestNextGraphTableBatch(t *testing.T) {
	for _, test := range []struct {
		written, tables, remaining int64
		want                       int
	}{
		// A checkpoint between left the journal shorter.
		{-1 << 20, 128, 1408, graphTablesPerTransaction},
		{0, 128, 1408, graphTablesPerTransaction},
		// New tables.
		{graphTransactionJournalBytes, 128, 1408, graphTablesPerTransaction},
		{128 * 8 << 10, 128, 1408, 256},
		// Shared tables, which once made a batch of all 1,408 left.
		{128, 128, 1408, graphTablesPerTransactionLimit},
		{128 << 10, 128, 1408, graphTablesPerTransactionLimit},
		{128, 128, 10, 10},
	} {
		if got := nextGraphTableBatch(test.written, test.tables, test.remaining); got != test.want {
			t.Errorf("nextGraphTableBatch(%d, %d, %d) = %d, want %d", test.written, test.tables, test.remaining, got, test.want)
		}
	}
	// New tables write about graphTransactionJournalBytes for each
	// graphTablesPerTransaction; a transaction may write 8 MiB.
	if worst := graphTablesPerTransactionLimit * graphTransactionJournalBytes / graphTablesPerTransaction; worst > (8<<20)/2 {
		t.Fatalf("a batch of new tables may write %d bytes", worst)
	}
}
