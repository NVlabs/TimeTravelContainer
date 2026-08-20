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

package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	heartbeat, err := directory.Heartbeat("node-a", "boot-a", 7<<30, 512<<20, now.Add(2*time.Second), 30*time.Second)
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
	if _, err := directory.Heartbeat("node-a", "boot-b", 0, 0, now.Add(3*time.Second), 30*time.Second); err == nil {
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
	offline, err := directory.Transition("node-a", "boot-a", NodeOffline, now.Add(5*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if offline.LeaseActive(now.Add(5 * time.Second)) {
		t.Fatal("offline node has an active lease")
	}
	if _, err := directory.Register(testNodeRecord("node-a", "boot-b"), now.Add(6*time.Second), 30*time.Second); err != nil {
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
	if _, err := directory.Heartbeat("node-a", "boot-node-a", 0, 0, now.Add(time.Second), time.Second); err == nil {
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
