// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNodeIncomingReservationsFenceCapacityAndExpire(t *testing.T) {
	directory, err := OpenNodeDirectory(filepath.Join(t.TempDir(), "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(5000, 0).UTC()
	if _, err := directory.Register(testNodeRecord("node-a", "boot-a"), now, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Transition("node-a", "boot-a", NodeReady, now.Add(time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Heartbeat("node-a", "boot-a", 6<<30, now.Add(2*time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		id  string
		err error
	}
	results := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, operation := range []string{"move-one", "move-two"} {
		go func(operation string) {
			start.Wait()
			_, err := directory.ReserveIncoming("node-a", "boot-a", operation, "node-b", 2<<30, now.Add(3*time.Second), 5*time.Second)
			results <- outcome{operation, err}
		}(operation)
	}
	start.Done()
	winner := ""
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("two reservations overbooked the target")
			}
			winner = result.id
		}
	}
	if winner == "" {
		t.Fatal("no reservation acquired available target capacity")
	}
	// An exact retry is idempotent and does not double the total.
	retried, err := directory.ReserveIncoming("node-a", "boot-a", winner, "node-b", 2<<30, now.Add(4*time.Second), 5*time.Second)
	if err != nil || retried.IncomingReservedBytes != 2<<30 || len(retried.IncomingReservations) != 1 {
		t.Fatalf("reservation retry=(%+v, %v)", retried, err)
	}
	if _, err := directory.CompleteIncoming("node-a", "boot-a", winner, now.Add(4500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := directory.Heartbeat("node-a", "boot-a", 6<<30, now.Add(5*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.IncomingReservedBytes != 0 || len(heartbeat.IncomingReservations) != 0 {
		t.Fatalf("completed reservation survived fresh heartbeat: %+v", heartbeat)
	}
	if _, err := directory.ReserveIncoming("node-a", "boot-a", "move-expire", "node-b", 1<<30, now.Add(6*time.Second), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// Heartbeat also prunes an abandoned reservation after its TTL.
	heartbeat, err = directory.Heartbeat("node-a", "boot-a", 6<<30, now.Add(9*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.IncomingReservedBytes != 0 || len(heartbeat.IncomingReservations) != 0 {
		t.Fatalf("expired reservation survived: %+v", heartbeat)
	}
}

func testNodeRecord(node, incarnation string) NodeRecord {
	return NodeRecord{
		NodeID: node, Incarnation: incarnation,
		ControlURL: "https://" + node + ".example:7443", ControlServerName: node + ".example",
		Capabilities: testCapabilities(node), MemoryTargetBytes: 8 << 30,
		Labels: map[string]string{"pool": "test"},
	}
}

func TestNodeDirectoryLifecycleAndFencing(t *testing.T) {
	directory, err := OpenNodeDirectory(filepath.Join(t.TempDir(), "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	registered, err := directory.Register(testNodeRecord("node-a", "boot-a"), now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if registered.State != NodeJoining || !registered.LeaseActive(now) {
		t.Fatalf("registered node=%+v", registered)
	}
	ready, err := directory.Transition("node-a", "boot-a", NodeReady, now.Add(time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.AcceptsPlacement(now.Add(2 * time.Second)) {
		t.Fatalf("ready node rejects placement: %+v", ready)
	}
	if _, err := directory.Heartbeat("node-a", "boot-a", 7<<30, now.Add(1250*time.Millisecond), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	reserved, err := directory.ReserveIncoming("node-a", "boot-a", "move-one", "node-b", 512<<20, now.Add(1500*time.Millisecond), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.IncomingReservedBytes != 512<<20 {
		t.Fatalf("reservation=%+v", reserved)
	}
	heartbeat, err := directory.Heartbeat("node-a", "boot-a", 7<<30, now.Add(2*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.MemoryCurrentBytes != 7<<30 || heartbeat.IncomingReservedBytes != 512<<20 {
		t.Fatalf("heartbeat load=%+v", heartbeat)
	}
	pressured, err := directory.Transition("node-a", "boot-a", NodePressured, now.Add(2500*time.Millisecond), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if pressured.AcceptsPlacement(now.Add(3 * time.Second)) {
		t.Fatal("pressured node accepts new placement")
	}
	if _, err := directory.Transition("node-a", "boot-a", NodeReady, now.Add(3*time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Register(testNodeRecord("node-a", "boot-b"), now.Add(3*time.Second), 30*time.Second); err == nil {
		t.Fatal("live node incarnation was replaced")
	}
	if _, err := directory.Heartbeat("node-a", "boot-b", 0, now.Add(3*time.Second), 30*time.Second); err == nil {
		t.Fatal("stale incarnation renewed node lease")
	}
	if _, err := directory.Transition("node-a", "boot-a", NodeJoining, now.Add(3*time.Second), 30*time.Second); err == nil {
		t.Fatal("illegal ready-to-joining transition succeeded")
	}
	draining, err := directory.Transition("node-a", "boot-a", NodeDraining, now.Add(4*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if draining.AcceptsPlacement(now.Add(5 * time.Second)) {
		t.Fatal("draining node accepts placement")
	}
	readyAgain, err := directory.Transition("node-a", "boot-a", NodeReady, now.Add(5*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !readyAgain.AcceptsPlacement(now.Add(5 * time.Second)) {
		t.Fatal("node does not accept placement after canceling drain")
	}
	if _, err := directory.Transition("node-a", "boot-a", NodeDraining, now.Add(6*time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	offline, err := directory.Transition("node-a", "boot-a", NodeOffline, now.Add(7*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if offline.LeaseActive(now.Add(7 * time.Second)) {
		t.Fatal("offline node has an active lease")
	}
	if _, err := directory.Register(testNodeRecord("node-a", "boot-b"), now.Add(8*time.Second), 30*time.Second); err != nil {
		t.Fatalf("replacing offline incarnation: %v", err)
	}
}

func TestNodeDirectoryExpiryAndStableList(t *testing.T) {
	directory, err := OpenNodeDirectory(filepath.Join(t.TempDir(), "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0).UTC()
	for _, node := range []string{"node-b", "node-a"} {
		if _, err := directory.Register(testNodeRecord(node, "boot-"+node), now, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	records, err := directory.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].NodeID != "node-a" || records[1].NodeID != "node-b" {
		t.Fatalf("records=%+v", records)
	}
	if records[0].LeaseActive(now.Add(time.Second)) {
		t.Fatal("lease remained active at its expiry boundary")
	}
	if _, err := directory.Heartbeat("node-a", "boot-node-a", 0, now.Add(time.Second), time.Second); err == nil {
		t.Fatal("expired incarnation was resurrected")
	}
	if _, err := directory.Register(testNodeRecord("node-a", "replacement"), now.Add(time.Second), time.Second); err != nil {
		t.Fatalf("replacing expired node: %v", err)
	}
}

func TestNodeDirectoryCanRecoverDirectlyIntoDraining(t *testing.T) {
	directory, err := OpenNodeDirectory(filepath.Join(t.TempDir(), "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1500, 0).UTC()
	if _, err := directory.Register(testNodeRecord("node-a", "recovery"), now, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	draining, err := directory.Transition("node-a", "recovery", NodeDraining, now.Add(time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if draining.State != NodeDraining || draining.AcceptsPlacement(now.Add(2*time.Second)) {
		t.Fatalf("recovered record=%+v", draining)
	}
}

func TestNodeDirectoryRejectsCorruptSharedRecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nodes")
	directory, err := OpenNodeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node-a.json"), []byte(`{"node_id":"node-a","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Lookup("node-a"); err == nil {
		t.Fatal("corrupt node record was accepted")
	}
}

// Only a record of a live incarnation of a compute node may grant execution.
func TestOnlyALiveComputeIncarnationMayExecute(t *testing.T) {
	control := map[string]string{NodeRoleLabel: RoleControl}
	for _, test := range []struct {
		record NodeRecord
		want   bool
	}{
		{NodeRecord{State: NodeReady}, true},
		{NodeRecord{State: NodeDraining}, true},
		{NodeRecord{State: NodeOffline}, false},
		{NodeRecord{State: NodeReady, Labels: control}, false},
	} {
		if got := test.record.mayExecute(); got != test.want {
			t.Errorf("%+v may execute: %t, want %t", test.record, got, test.want)
		}
	}
}
