// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// createPlacements has holder create count placements of family.
func createPlacements(t *testing.T, holder *AuthorityNode, family string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		branch := fmt.Sprintf("b%d", i)
		result, err := holder.Propose(context.Background(), "create-"+family+"-"+branch, AuthorityPlacementCreate, AuthorityPlacementPayload{Record: PlacementRecord{
			FamilyID: family, BranchID: branch, PhysicalContainer: family, PhysicalBranch: branch,
			OwnerNode: "node-a", OwnerIncarnation: "incarnation-a", State: PlacementRunning,
		}})
		if err == nil {
			err = result.Err()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// lastSegment returns the path of the last segment in dir.
func lastSegment(t *testing.T, dir string) string {
	t.Helper()
	segments, err := authoritySegments(dir)
	if err != nil || len(segments) == 0 {
		t.Fatalf("segments %v, %v", segments, err)
	}
	return segments[len(segments)-1].path
}

func TestAuthorityLogReplaysOnReopen(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	createPlacements(t, holder.node, "family", 5)
	want, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	holding := holder.node.current.Load()
	if err := holder.node.Hold(AuthorityOptions{NodeID: "node-a", Dir: holder.shared.EpochDir(1), Epoch: 1, Held: holding.held}); err != nil {
		t.Fatal(err)
	}
	got, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision || len(got.Placements) != 5 {
		t.Fatalf("reopened revision %d with %d placements, want %d with 5", got.Revision, len(got.Placements), want.Revision)
	}
	createPlacements(t, holder.node, "other", 1)
}

// A holder that stopped in the middle of a write leaves a torn record, which
// ends the log: the next holder of the epoch drops it and appends after the
// last whole record.
func TestAuthorityLogDropsATornTail(t *testing.T) {
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	createPlacements(t, holder.node, "family", 3)
	want, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	held := holder.node.current.Load().held
	if err := holder.node.Close(); err != nil {
		t.Fatal(err)
	}
	dir := holder.shared.EpochDir(1)
	segment := lastSegment(t, dir)
	info, err := os.Stat(segment)
	if err != nil {
		t.Fatal(err)
	}
	// Half of the next record's header, and garbage.
	file, err := os.OpenFile(segment, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0, 0, 0, 9, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := holder.node.Hold(AuthorityOptions{NodeID: "node-a", Dir: dir, Epoch: 1, Held: held}); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(segment); err != nil || after.Size() != info.Size() {
		t.Fatalf("the torn record was not dropped: size %v, %v, want %d", after.Size(), err, info.Size())
	}
	got, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision {
		t.Fatalf("revision %d after the torn tail, want %d", got.Revision, want.Revision)
	}
	createPlacements(t, holder.node, "other", 2)
	if err := holder.node.Close(); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := loadAuthorityState(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Placements) != 5 {
		t.Fatalf("%d placements after appending past the torn tail, want 5", len(state.Placements))
	}
}

// Damage anywhere but at the end of the log is not a torn write: reading it
// fails.
func TestAuthorityLogRefusesDamageBeforeItsEnd(t *testing.T) {
	records, bytes := authoritySegmentRecords, authoritySegmentBytes
	authoritySegmentRecords = 4
	defer func() { authoritySegmentRecords, authoritySegmentBytes = records, bytes }()
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	held := holder.node.current.Load().held
	dir := holder.shared.EpochDir(1)
	// Keep the segments compaction would remove, to damage the first.
	if err := holder.node.Close(); err != nil {
		t.Fatal(err)
	}
	log, _, err := openAuthorityLog(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	command := []byte(`{}`)
	for i := 0; i < 6; i++ {
		if _, err := log.append([][]byte{command}); err != nil {
			t.Fatal(err)
		}
		if log.full() {
			if err := log.startSegment(); err != nil {
				t.Fatal(err)
			}
		}
	}
	log.close()
	segments, err := authoritySegments(dir)
	if err != nil || len(segments) < 2 {
		t.Fatalf("segments %v, %v, want two or more", segments, err)
	}
	data, err := os.ReadFile(segments[0].path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(segments[0].path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := holder.node.Hold(AuthorityOptions{NodeID: "node-a", Dir: dir, Epoch: 1, Held: held}); err == nil {
		t.Fatal("held an authority whose log is damaged before its end")
	}
}

// Past a segment's bound, the holder writes a snapshot and removes the
// segments it holds.
func TestAuthorityLogCompacts(t *testing.T) {
	records := authoritySegmentRecords
	authoritySegmentRecords = 4
	defer func() { authoritySegmentRecords = records }()
	holder := newTestHolder(t)
	registerReadyNode(t, holder.node, "node-a", "incarnation-a")
	createPlacements(t, holder.node, "family", 10)
	want, err := holder.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	dir := holder.shared.EpochDir(1)
	index, _, err := readAuthoritySnapshot(dir)
	if err != nil || index == 0 {
		t.Fatalf("snapshot at %d, %v, want a compacted one", index, err)
	}
	segments, err := authoritySegments(dir)
	if err != nil || len(segments) != 1 || segments[0].first != index+1 {
		t.Fatalf("segments %+v, %v, want one, after the snapshot's %d", segments, err, index)
	}
	if err := holder.node.Close(); err != nil {
		t.Fatal(err)
	}
	state, last, _, err := loadAuthorityState(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != want.Revision || len(state.Placements) != 10 || last < index {
		t.Fatalf("replayed revision %d, %d placements, last %d; want %d, 10, at least %d", state.Revision, len(state.Placements), last, want.Revision, index)
	}
}

// A successor makes its epoch's state from the previous epoch's snapshot and
// log, and changes nothing of them.
func TestPrepareEpochCarriesTheState(t *testing.T) {
	records := authoritySegmentRecords
	authoritySegmentRecords = 4
	defer func() { authoritySegmentRecords = records }()
	shared := NewSharedCluster(t.TempDir())
	first := holdTestEpoch(t, shared, "node-a", 1, 0)
	registerReadyNode(t, first.node, "node-a", "incarnation-a")
	// Some of the state is in a snapshot, and some only in the log.
	createPlacements(t, first.node, "family", 5)
	want, err := first.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(shared.EpochDir(1))
	if err != nil {
		t.Fatal(err)
	}
	second := holdTestEpoch(t, shared, "node-b", 2, 1)
	got, err := second.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision || len(got.Placements) != 5 {
		t.Fatalf("epoch 2 has revision %d with %d placements, want %d with 5", got.Revision, len(got.Placements), want.Revision)
	}
	if record, ok := got.Nodes["node-a"]; !ok || record.State != NodeReady {
		t.Fatalf("epoch 2 has node-a as %+v (%t), want it ready", record, ok)
	}
	after, err := os.ReadDir(shared.EpochDir(1))
	if err != nil || len(after) != len(before) {
		t.Fatalf("epoch 1's state changed from %d to %d entries, %v", len(before), len(after), err)
	}
	if err := shared.PrepareEpoch(1, 2); err == nil {
		t.Fatal("made the state of a made epoch again")
	}
}

// A stalled holder may write its log after its successor read it, if only
// what it never acknowledges; its successor's state ends where the log it
// read did, and a torn record does not reach it.
func TestPrepareEpochReadsAWrittenLog(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	first := holdTestEpoch(t, shared, "node-a", 1, 0)
	registerReadyNode(t, first.node, "node-a", "incarnation-a")
	createPlacements(t, first.node, "family", 2)
	want, err := first.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	segment := lastSegment(t, shared.EpochDir(1))
	file, err := os.OpenFile(segment, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(appendAuthorityRecord(nil, 999, []byte(`{"torn":`))[:20]); err != nil {
		t.Fatal(err)
	}
	file.Close()
	second := holdTestEpoch(t, shared, "node-b", 2, 1)
	got, err := second.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision {
		t.Fatalf("epoch 2 has revision %d, want %d", got.Revision, want.Revision)
	}
}

// A claimant that ended before it made its epoch's state acknowledged
// nothing in it: the epoch taken from it is made from the epoch it would
// have made its own from.
func TestStateSourceSkipsAnEpochNeverMade(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	if source, err := shared.StateSource(0); err != nil || source != 0 {
		t.Fatalf("the source of a new cluster is %d, %v, want 0", source, err)
	}
	first := holdTestEpoch(t, shared, "node-a", 1, 0)
	registerReadyNode(t, first.node, "node-a", "incarnation-a")
	createPlacements(t, first.node, "family", 3)
	want, err := first.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	// Node b claims epoch 2 from epoch 1, and ends before it makes its state.
	if won, err := shared.ClaimEpoch(AuthorityRecord{Epoch: 2, Previous: 1, NodeID: "node-b", Incarnation: "i-1"}); err != nil || !won {
		t.Fatalf("claiming epoch 2: %t, %v", won, err)
	}
	source, err := shared.StateSource(2)
	if err != nil || source != 1 {
		t.Fatalf("the source of an epoch taken from epoch 2 is %d, %v, want 1", source, err)
	}
	third := holdTestEpoch(t, shared, "node-c", 3, source)
	got, err := third.node.AppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision || len(got.Placements) != 3 {
		t.Fatalf("epoch 3 has revision %d with %d placements, want %d with 3", got.Revision, len(got.Placements), want.Revision)
	}
	// The first claimant of a cluster that ended before it made the state
	// leaves a new cluster's.
	other := NewSharedCluster(t.TempDir())
	if won, err := other.ClaimEpoch(AuthorityRecord{Epoch: 1, NodeID: "node-a", Incarnation: "i-1"}); err != nil || !won {
		t.Fatalf("claiming epoch 1: %t, %v", won, err)
	}
	if source, err := other.StateSource(1); err != nil || source != 0 {
		t.Fatalf("the source of an epoch taken from an unmade first epoch is %d, %v, want 0", source, err)
	}
}

// An epoch an earlier TTC kept in a Raft log is refused, with a message that
// says what to do.
func TestAuthorityStateInARaftLogIsRefused(t *testing.T) {
	shared := NewSharedCluster(t.TempDir())
	if err := os.MkdirAll(filepath.Join(shared.EpochDir(1), "raft"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := shared.PrepareEpoch(1, 2); !errors.Is(err, errOldAuthorityFormat) {
		t.Fatalf("preparing from a Raft epoch = %v, want errOldAuthorityFormat", err)
	}
}
