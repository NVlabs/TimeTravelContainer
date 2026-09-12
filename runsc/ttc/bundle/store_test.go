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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gvisor.dev/gvisor/pkg/ttcvolume"
)

// writeCurrentContainerFixture fills fields that every current-format writer
// persists. Tests that intentionally exercise malformed metadata use writeJSON
// directly instead.
func writeCurrentContainerFixture(path string, input *ContainerInfo) error {
	info := *input
	info.Branches = append([]BranchInfo(nil), input.Branches...)
	if info.ID == "" {
		info.ID = filepath.Base(filepath.Dir(path))
	}
	if info.State == "" {
		info.State = "created"
	}
	if len(info.Branches) == 0 {
		info.Branches = []BranchInfo{{ID: "main", CreatedAt: info.CreatedAt}}
	}
	for i := range info.Branches {
		branch := &info.Branches[i]
		if branch.FilesystemBranch == "" {
			if branch.ID == "main" {
				branch.FilesystemBranch = ttcvolume.MainBranch
			} else {
				branch.FilesystemBranch = ttcvolume.ScopedBranchID(info.ID, branch.ID)
			}
		}
		if branch.State == "" {
			branch.State = info.State
		}
		if branch.StorageState == "" {
			branch.StorageState = "local"
		}
	}
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		if info.NetworkMode == "host" {
			info.LiveForkWaiver = "test fixture explicitly waives live fork"
		} else {
			info.LiveForkEligible = true
		}
	}
	return writeJSON(path, &info)
}

func TestMarkedRootRejectsLegacyContainerMetadata(t *testing.T) {
	root := t.TempDir()
	marker, err := json.Marshal(map[string]any{"version": 2, "schema": "ttc-root", "role": "standalone-authority"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ttc-root-format"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	info := ContainerInfo{ID: "container-id", Name: "example", BundleDir: bundleDir, State: "created"}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContainer(info.ID); err == nil {
		t.Fatal("marked root synthesized a missing legacy branch")
	}
	info.Branches = []BranchInfo{{ID: "main", State: "created", StorageState: "local"}}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContainer(info.ID); err == nil {
		t.Fatal("marked root synthesized a missing filesystem identity")
	}
	info.Branches[0].FilesystemBranch = "main"
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContainer(info.ID); err == nil {
		t.Fatal("marked root synthesized omitted live-fork eligibility")
	}
}

func TestCompareAndSwapBranchNodeIDDoesNotInheritFamilyOwner(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir, NodeID: "node-a",
		Branches: []BranchInfo{{ID: "main", NodeID: "node-b"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if err := store.CompareAndSwapBranchNodeID("example", "main", "node-b", "node-c"); err != nil {
		t.Fatal(err)
	}
	info, err := store.LoadContainer("example")
	if err != nil || info.BranchNodeID("main") != "node-c" || info.NodeID != "node-a" {
		t.Fatalf("branch placement=(%+v, %v)", info, err)
	}
	if err := store.RecordBranch("example", BranchInfo{ID: "child", ParentID: "main", FilesystemBranch: ttcvolume.ScopedBranchID("container-id", "child"), State: "running", StorageState: "local"}); err != nil {
		t.Fatal(err)
	}
	info, err = store.LoadContainer("example")
	if err != nil || info.BranchNodeID("child") != "" {
		t.Fatalf("ownerless branch inherited family placement: (%+v, %v)", info, err)
	}
}

func TestClearBranchMemoryGraphIfIsGenerationFenced(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wantRoots := []string{"memory/7/resource-0000"}
	wantObjects := []string{"0000000000000007-03-0000000000000001"}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir,
		Branches: []BranchInfo{{
			ID: "main", SnapshotID: 7, MemoryRoots: wantRoots,
			MemoryRootObjects: wantObjects, DistributedGraphReferenceOperation: "retain-7",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	cleared, err := store.ClearBranchMemoryGraphIf("example", "main", 6, wantRoots, wantObjects, "retain-7")
	if err != nil || cleared {
		t.Fatalf("stale snapshot clear=(%t, %v), want false, nil", cleared, err)
	}
	cleared, err = store.ClearBranchMemoryGraphIf("example", "main", 7, wantRoots, wantObjects, "retain-7")
	if err != nil || !cleared {
		t.Fatalf("matching snapshot clear=(%t, %v), want true, nil", cleared, err)
	}
	info, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	branch := info.Branches[0]
	if branch.SnapshotID != 0 || len(branch.MemoryRoots) != 0 || len(branch.MemoryRootObjects) != 0 || branch.DistributedGraphReferenceOperation != "" {
		t.Fatalf("cleared branch retained memory graph: %+v", branch)
	}
}

func TestUpdateBranchQuarantinePersistsIndependentlyOfLifecycle(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir,
		Branches: []BranchInfo{{ID: "main", State: "dehydrated"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if err := store.UpdateBranchQuarantine("example", "main", true); err != nil {
		t.Fatal(err)
	}
	info, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Branches[0].Quarantined || info.Branches[0].State != "dehydrated" {
		t.Fatalf("updated branch=%+v", info.Branches[0])
	}
	if err := store.UpdateBranchQuarantine("example", "main", false); err != nil {
		t.Fatal(err)
	}
	info, err = store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if info.Branches[0].Quarantined {
		t.Fatalf("quarantine flag remained set: %+v", info.Branches[0])
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
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
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
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
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

func TestRenameBranchKeepsStableMemoryAndRenamesConsoleState(t *testing.T) {
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
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "console", "old.log"), []byte("console"), 0600); err != nil {
		t.Fatal(err)
	}
	stableMemory := filepath.Join(containerDir, "memory", "dehydrated", MemoryBranchKey(branch))
	if err := os.MkdirAll(stableMemory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stableMemory, "control.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	store := Store{Root: root}
	if err := store.RenameBranch("example", "old", "new"); err != nil {
		t.Fatalf("RenameBranch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "console", "new.log")); err != nil {
		t.Errorf("renamed console: %v", err)
	}
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

func TestRemoveBranchArtifactsRemovesStableMemory(t *testing.T) {
	root := t.TempDir()
	containerID := "container-id"
	containerDir := filepath.Join(root, "containers", containerID)
	bundleDir := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := BranchInfo{ID: "reused", FilesystemBranch: "stable-filesystem-branch", State: "dehydrated"}
	info := ContainerInfo{
		ID: containerID, Name: "example", BundleDir: bundleDir, State: "dehydrated",
		Branches: []BranchInfo{branch},
	}
	if err := os.MkdirAll(containerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	stableKey := MemoryBranchKey(branch)
	legacyKey := hex.EncodeToString([]byte(branch.ID))
	for _, kind := range []string{"active", "dehydrated"} {
		path := filepath.Join(containerDir, "memory", kind, stableKey)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "control.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		// An unknown old-layout entry is not interpreted as branch state in the
		// current format and must not widen branch removal's ownership scope.
		if err := os.MkdirAll(filepath.Join(containerDir, "memory", kind, legacyKey), 0700); err != nil {
			t.Fatal(err)
		}
	}

	store := Store{Root: root}
	if err := store.RemoveBranchArtifacts("example", branch); err != nil {
		t.Fatalf("RemoveBranchArtifacts: %v", err)
	}
	for _, kind := range []string{"active", "dehydrated"} {
		if _, err := os.Lstat(filepath.Join(containerDir, "memory", kind, stableKey)); !os.IsNotExist(err) {
			t.Errorf("%s/%s remains after removal: %v", kind, stableKey, err)
		}
		if _, err := os.Lstat(filepath.Join(containerDir, "memory", kind, legacyKey)); err != nil {
			t.Errorf("unknown %s/%s was unexpectedly removed: %v", kind, legacyKey, err)
		}
	}
}
