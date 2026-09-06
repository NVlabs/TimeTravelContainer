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

package bundle

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareAndSwapNodeID(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir, NodeID: "node-a",
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if err := store.CompareAndSwapNodeID("example", "node-a", "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapNodeID("example", "node-a", "node-c"); err == nil {
		t.Fatal("stale cluster owner changed metadata")
	}
	info, err := store.LoadContainer("example")
	if err != nil || info.NodeID != "node-b" {
		t.Fatalf("container=(%+v, %v)", info, err)
	}
	if err := store.CompareAndSwapBranchNodeID("example", "main", "node-b", "node-c"); err != nil {
		t.Fatal(err)
	}
	info, err = store.LoadContainer("example")
	if err != nil || info.BranchNodeID("main") != "node-c" || info.NodeID != "node-b" {
		t.Fatalf("branch placement=(%+v, %v)", info, err)
	}
}

func TestCompareAndSwapBranchNodeIDClaimsUnplacedBranch(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir, NodeID: "node-a",
		Branches: []BranchInfo{{ID: "cold", Unplaced: true, State: "dehydrated"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if err := store.CompareAndSwapBranchNodeID("example", "cold", "", "node-b"); err != nil {
		t.Fatalf("claiming unplaced branch: %v", err)
	}
	info, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.BranchNodeID("cold"); got != "node-b" || info.Branches[0].Unplaced {
		t.Fatalf("claimed branch owner=%q unplaced=%t", got, info.Branches[0].Unplaced)
	}
	if err := store.CompareAndSwapBranchNodeID("example", "cold", "", "node-c"); err == nil {
		t.Fatal("stale unplaced owner changed claimed branch")
	}
}

func TestReconcileNodeRuntimeUnavailable(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "example")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{
		ID: "example", Name: "example", BundleDir: bundleDir, NodeID: "node-a", State: "running",
		Branches: []BranchInfo{
			{ID: "main", NodeID: "node-a", State: "running"},
			{ID: "publishing", NodeID: "node-a", State: "dehydrating"},
			{ID: "remote", NodeID: "node-b", State: "running"},
		},
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if err := store.ReconcileNodeRuntimeUnavailable("example", "node-a", nil); err != nil {
		t.Fatalf("ReconcileNodeRuntimeUnavailable: %v", err)
	}
	got, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if state := got.Branches[0].State; state != "exited" {
		t.Errorf("local branch state = %q, want exited", state)
	}
	if state := got.Branches[1].State; state != "dehydrating" {
		t.Errorf("process-free publishing branch state = %q, want dehydrating", state)
	}
	if state := got.Branches[2].State; state != "running" {
		t.Errorf("remote branch state = %q, want running", state)
	}
	if got.State != "running" {
		t.Errorf("family state = %q, want running", got.State)
	}
}

func TestRenameBranchPromotesStableMemoryAndConsoleState(t *testing.T) {
	root := t.TempDir()
	containerID := "container-id"
	containerDir := filepath.Join(root, "containers", containerID)
	bundleDir := filepath.Join(root, "bundle")
	for _, dir := range []string{
		filepath.Join(containerDir, "console", "exec"),
		filepath.Join(containerDir, "memory", "dehydrated"),
		bundleDir,
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := BranchInfo{ID: "old", ParentID: "parent", FilesystemBranch: "stable-memory-identity", State: "dehydrated"}
	info := ContainerInfo{
		ID: containerID, Name: "example", BundleDir: bundleDir, State: "dehydrated",
		Branches: []BranchInfo{branch, {ID: "child", ParentID: "old", FilesystemBranch: "child-memory", State: "dehydrated"}},
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "console", "old.log"), []byte("console"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyMemory := filepath.Join(containerDir, "memory", "dehydrated", hex.EncodeToString([]byte("old")))
	if err := os.MkdirAll(legacyMemory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyMemory, "control.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	store := Store{Root: root}
	if err := store.RenameBranch("example", "old", "new"); err != nil {
		t.Fatalf("RenameBranch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "console", "new.log")); err != nil {
		t.Errorf("renamed console: %v", err)
	}
	stableMemory := filepath.Join(containerDir, "memory", "dehydrated", MemoryBranchKey(branch))
	if _, err := os.Stat(filepath.Join(stableMemory, "control.json")); err != nil {
		t.Errorf("stable memory: %v", err)
	}
	loaded, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Branches[0].ID != "new" || loaded.Branches[1].ParentID != "new" {
		t.Fatalf("renamed branches = %+v", loaded.Branches)
	}

	if err := store.RenameBranch("example", "new", "again"); err != nil {
		t.Fatalf("second RenameBranch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stableMemory, "control.json")); err != nil {
		t.Errorf("stable memory moved on second rename: %v", err)
	}
}
