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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func TestRetryStalePathRead(t *testing.T) {
	attempts := 0
	value, err := retryStalePathRead(func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", &os.PathError{Op: "read", Path: "container.json", Err: syscall.ESTALE}
		}
		return "current", nil
	})
	if err != nil || value != "current" || attempts != 3 {
		t.Fatalf("retryStalePathRead()=(%q, %v) after %d attempts, want current after 3", value, err, attempts)
	}

	attempts = 0
	_, err = retryStalePathRead(func() (string, error) {
		attempts++
		return "", os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || attempts != 1 {
		t.Fatalf("non-ESTALE retry returned %v after %d attempts, want permission after 1", err, attempts)
	}
}

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
	wantControlRoot := "control/capture/0000000000000007"
	wantControlObject := "0000000000000007-07-0000000000000002"
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir,
		Branches: []BranchInfo{{
			ID: "main", SnapshotID: 7, MemoryRoots: wantRoots,
			MemoryRootObjects: wantObjects, DistributedGraphReferenceOperation: "retain-7",
			ControlRoot: wantControlRoot, ControlRootObject: wantControlObject,
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
	if branch.SnapshotID != 0 || len(branch.MemoryRoots) != 0 || len(branch.MemoryRootObjects) != 0 || branch.ControlRoot != "" || branch.ControlRootObject != "" || branch.DistributedGraphReferenceOperation != "" {
		t.Fatalf("cleared branch retained memory graph: %+v", branch)
	}
}

// A pruned image replaces the one it was pruned from, unless a dehydration
// superseded that meanwhile.
func TestReplaceBranchMemoryGraphIfIsFenced(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "example")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	roots, objects := []string{"memory/7/resource-0000"}, []string{"7:1:memory-tree"}
	info := ContainerInfo{
		ID: "example", Name: "example", BundleDir: bundleDir, State: "running",
		Branches: []BranchInfo{{ID: "main", State: "running", SnapshotID: 7, MemoryRoots: roots, MemoryRootObjects: objects}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	newRoots, newObjects := []string{"memory/9/resource-0000"}, []string{"9:1:memory-tree"}
	if replaced, err := store.ReplaceBranchMemoryGraphIf("example", "main", 8, roots, objects, 9, newRoots, newObjects); err != nil || replaced {
		t.Fatalf("replacing another capture's image = %v, %v", replaced, err)
	}
	if replaced, err := store.ReplaceBranchMemoryGraphIf("example", "main", 7, roots, objects, 9, newRoots, newObjects); err != nil || !replaced {
		t.Fatalf("replacing the image = %v, %v", replaced, err)
	}
	got, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	branch := got.Branches[0]
	if branch.SnapshotID != 9 || !slices.Equal(branch.MemoryRoots, newRoots) || !slices.Equal(branch.MemoryRootObjects, newObjects) {
		t.Errorf("memory = %d %v %v, want 9 %v %v", branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects, newRoots, newObjects)
	}
}

// A rehydrated branch gives up the control state it was restored from, and
// keeps its memory roots for its next dehydration.
func TestClearBranchControlGraphIfKeepsMemory(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "example")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	roots, objects := []string{"memory/7/0"}, []string{"7:1:memory-tree"}
	info := ContainerInfo{
		ID: "example", Name: "example", BundleDir: bundleDir, State: "running",
		Branches: []BranchInfo{{ID: "main", State: "running", SnapshotID: 7, MemoryRoots: roots, MemoryRootObjects: objects, ControlRoot: "control/7", ControlRootObject: "7:2:control"}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	if cleared, err := store.ClearBranchControlGraphIf("example", "main", 6, "control/7", "7:2:control"); err != nil || cleared {
		t.Fatalf("clearing the control root of another capture = %v, %v", cleared, err)
	}
	if cleared, err := store.ClearBranchControlGraphIf("example", "main", 7, "control/7", "7:2:control"); err != nil || !cleared {
		t.Fatalf("clearing the control root = %v, %v", cleared, err)
	}
	got, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	branch := got.Branches[0]
	if branch.ControlRoot != "" || branch.ControlRootObject != "" {
		t.Errorf("control root = %q/%q, want none", branch.ControlRoot, branch.ControlRootObject)
	}
	if branch.SnapshotID != 7 || !slices.Equal(branch.MemoryRoots, roots) || !slices.Equal(branch.MemoryRootObjects, objects) {
		t.Errorf("memory = %d %v %v, want 7 %v %v", branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects, roots, objects)
	}
}

func TestParkBranchMemoryGraphTransfersAuthorityToFamily(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "container-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := BranchInfo{
		ID: "child", FilesystemBranch: "b-child", SnapshotID: 9,
		MemoryRoots:       []string{"memory/9/resource-0000"},
		MemoryRootObjects: []string{"0000000000000009-03-0000000000000001"},
		ControlRoot:       "control/capture/9", ControlRootObject: "0000000000000009-07-0000000000000002",
		DistributedGraphReferenceOperation: "retain-9",
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: bundleDir, Branches: []BranchInfo{branch},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	backing, changed, err := store.ParkBranchMemoryGraph("example", "child")
	if err != nil || !changed {
		t.Fatalf("ParkBranchMemoryGraph=(%+v, %t, %v)", backing, changed, err)
	}
	info, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Branches[0]; got.SnapshotID != 0 || len(got.MemoryRoots) != 0 || got.DistributedGraphReferenceOperation != "" {
		t.Fatalf("parked branch retained graph authority: %+v", got)
	}
	if len(info.RuntimeMemoryBackings) != 1 || info.RuntimeMemoryBackings[0].ID != backing.ID || info.RuntimeMemoryBackings[0].DistributedGraphReferenceOperation != "retain-9" {
		t.Fatalf("runtime backings=%+v, want parked graph", info.RuntimeMemoryBackings)
	}
	if _, changed, err := store.ParkBranchMemoryGraph("example", "child"); err != nil || changed {
		t.Fatalf("idempotent ParkBranchMemoryGraph changed=%t err=%v", changed, err)
	}
	if err := store.RemoveRuntimeMemoryBacking("example", backing.ID); err != nil {
		t.Fatal(err)
	}
	info, err = store.LoadContainer("example")
	if err != nil || len(info.RuntimeMemoryBackings) != 0 {
		t.Fatalf("runtime backings after remove=(%+v, %v)", info.RuntimeMemoryBackings, err)
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
	// A claim that finds its change already made, as an activation makes
	// it, succeeds.
	if err := store.CompareAndSwapBranchNodeID("example", "cold", "", "node-b"); err != nil {
		t.Fatalf("claiming a branch already recorded as the claimant's: %v", err)
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
	if err := store.ReconcileNodeRuntimeUnavailable("example", "node-a", RuntimeLoss{}); err != nil {
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

// A branch that loses its runtime was killed with it, unless its init had
// exited first; a family whose main was killed says why, as Docker does.
func TestRuntimeLossRecordsHowEachBranchEnded(t *testing.T) {
	for _, test := range []struct {
		name      string
		loss      RuntimeLoss
		wantMain  int
		wantChild int
		wantOOM   bool
		wantError string
	}{
		{name: "killed", loss: RuntimeLoss{}, wantMain: KilledExitStatus, wantChild: KilledExitStatus, wantError: RuntimeExitedUnexpectedly},
		{name: "out of memory", loss: RuntimeLoss{OOMKilled: true}, wantMain: KilledExitStatus, wantChild: KilledExitStatus, wantOOM: true},
		{name: "main exited first", loss: RuntimeLoss{ExitStatuses: map[string]int{"main": 3}, OOMKilled: true}, wantMain: 3, wantChild: KilledExitStatus},
		{name: "all exited", loss: RuntimeLoss{ExitStatuses: map[string]int{"main": 0, "child": 1}}, wantMain: 0, wantChild: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
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
				ID: "example", Name: "example", BundleDir: bundleDir, State: "running",
				Branches: []BranchInfo{
					{ID: "main", State: "running"},
					{ID: "child", ParentID: "main", State: "paused"},
				},
			}
			if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
				t.Fatal(err)
			}
			store := Store{Root: root}
			if err := store.ReconcileRuntimeUnavailable("example", test.loss); err != nil {
				t.Fatalf("ReconcileRuntimeUnavailable: %v", err)
			}
			got, err := store.LoadContainer("example")
			if err != nil {
				t.Fatal(err)
			}
			for index, want := range []int{test.wantMain, test.wantChild} {
				branch := got.Branches[index]
				if branch.State != "exited" || branch.ExitStatus == nil || *branch.ExitStatus != want {
					t.Errorf("%s = %s with status %v, want exited with %d", branch.ID, branch.State, branch.ExitStatus, want)
				}
			}
			if got.State != "exited" || got.ExitStatus == nil || *got.ExitStatus != test.wantMain {
				t.Errorf("family = %s with status %v, want exited with %d", got.State, got.ExitStatus, test.wantMain)
			}
			if got.OOMKilled != test.wantOOM || got.ExitError != test.wantError {
				t.Errorf("family OOMKilled = %v, ExitError = %q; want %v, %q", got.OOMKilled, got.ExitError, test.wantOOM, test.wantError)
			}
			// Starting it again forgets how it last ended.
			if err := store.UpdateBranchState("example", "main", "running"); err != nil {
				t.Fatal(err)
			}
			if got, err = store.LoadContainer("example"); err != nil {
				t.Fatal(err)
			}
			if got.ExitStatus != nil || got.OOMKilled || got.ExitError != "" {
				t.Errorf("restarted family keeps its last end: status %v, OOMKilled %v, ExitError %q", got.ExitStatus, got.OOMKilled, got.ExitError)
			}
		})
	}
}

// A resident dehydration's capture lives only in its family's runtime: made
// durable it is an ordinary dehydrated branch, stopped it gives the capture up,
// and a runtime that disappears takes it the way it takes a running branch.
func TestResidentDehydrationLifecycle(t *testing.T) {
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
		ID: "example", Name: "example", BundleDir: bundleDir, State: "running",
		Branches: []BranchInfo{
			{ID: "main", State: "running", StorageState: "local"},
			{ID: "resident", State: "running", StorageState: "local"},
			{ID: "stopped", State: "running", StorageState: "local"},
			{ID: "durable", State: "running", StorageState: "local"},
			{ID: "cold", State: "dehydrated", StorageState: "local"},
		},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	for _, id := range []string{"resident", "stopped", "durable"} {
		if err := store.MarkBranchDehydratedResident("example", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkBranchDehydrationDurable("example", "durable"); err != nil {
		t.Fatal(err)
	}
	if err := store.StopBranch("example", "stopped", 137); err != nil {
		t.Fatal(err)
	}
	branches := func() map[string]BranchInfo {
		got, err := store.LoadContainer("example")
		if err != nil {
			t.Fatal(err)
		}
		byID := make(map[string]BranchInfo)
		for _, branch := range got.Branches {
			byID[branch.ID] = branch
		}
		return byID
	}
	before := branches()
	if b := before["resident"]; !ResidentDehydration(b) {
		t.Fatalf("marked branch = %s/%s, want dehydrated/resident", b.State, b.StorageState)
	}
	if b := before["durable"]; ResidentDehydration(b) || b.State != "dehydrated" || b.StorageState != "local" {
		t.Fatalf("durable branch = %s/%s, want dehydrated/local", b.State, b.StorageState)
	}
	if b := before["stopped"]; b.State != "exited" || b.StorageState != "local" {
		t.Fatalf("stopped resident branch = %s/%s, want exited/local", b.State, b.StorageState)
	}
	if err := store.ReconcileNodeRuntimeUnavailable("example", "", RuntimeLoss{}); err != nil {
		t.Fatal(err)
	}
	after := branches()
	for id, want := range map[string]string{"main": "exited", "resident": "exited", "durable": "dehydrated", "cold": "dehydrated"} {
		if b := after[id]; b.State != want || b.StorageState == StorageResident {
			t.Errorf("%s after runtime loss = %s/%s, want %s", id, b.State, b.StorageState, want)
		}
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

func TestBeginBranchPublicationIfDoesNotSupersedeChangedIdentity(t *testing.T) {
	root := t.TempDir()
	containerID := "container-id"
	containerDir := filepath.Join(root, "containers", containerID)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{ID: containerID, Name: "example", BundleDir: bundleDir, State: "exited", Branches: []BranchInfo{{
		ID: "main", FilesystemBranch: "main", State: "exited", PublicationID: "publication-old",
		StorageState: "publishing", StorageManifest: "old-manifest",
	}}}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	previous, err := store.BeginBranchPublicationIf("example", "main", "publication-old", "publication-new")
	if err != nil {
		t.Fatal(err)
	}
	if previous.PublicationID != "publication-old" || previous.StorageManifest != "old-manifest" {
		t.Fatalf("replaced metadata = %+v", previous)
	}
	if _, err := store.BeginBranchPublicationIf("example", "main", "publication-old", "publication-stale"); err == nil {
		t.Fatal("stale publication compare-and-swap succeeded")
	}
	loaded, err := store.LoadContainer("example")
	if err != nil {
		t.Fatal(err)
	}
	branch := loaded.Branches[0]
	if branch.PublicationID != "publication-new" || branch.StorageState != "publishing" || branch.StorageManifest != "old-manifest" {
		t.Fatalf("branch after stale begin = %+v", branch)
	}
}

// A name that resolved once resolves again without a full listing, and still
// follows the records when a family is renamed, removed, or its name reused.
// A container's name never changes, but it can be removed, and its name taken
// by another.
func TestLoadContainerByNameFollowsRemovalAndReuse(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	write := func(id, name string) {
		t.Helper()
		containerDir := filepath.Join(root, "containers", id)
		bundleDir := filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(bundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{ID: id, Name: name, BundleDir: bundleDir}); err != nil {
			t.Fatal(err)
		}
	}
	want := func(name, id string) {
		t.Helper()
		info, err := store.LoadContainer(name)
		if id == "" {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("LoadContainer(%q) = (%q, %v), want it not to exist", name, info.ID, err)
			}
			return
		}
		if err != nil || info.ID != id {
			t.Fatalf("LoadContainer(%q) = (%q, %v), want %q", name, info.ID, err, id)
		}
	}
	write("id-one", "web")
	write("id-two", "db")
	want("web", "id-one")
	want("db", "id-two")
	want("id-o", "id-one")
	if err := os.RemoveAll(filepath.Join(root, "containers", "id-two")); err != nil {
		t.Fatal(err)
	}
	want("db", "")
	want("id-t", "")
	write("id-three", "db")
	want("db", "id-three")
	want("id-t", "id-three")
	if _, err := store.LoadContainer("id-"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("LoadContainer(%q) = %v, want an ambiguous ID prefix", "id-", err)
	}
}

// A container removed after it was listed holds no repository an inventory
// must keep; one still present whose spec cannot be read fails the inventory.
func TestRepositoryInventoryPassesOverAContainerRemovedSinceListing(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	paths := make(map[string]struct{})
	removed := ContainerInfo{ID: "removed-since-listing", Name: "gone"}
	if err := store.addContainerRepositoryPaths(paths, []ContainerInfo{removed}); err != nil {
		t.Fatalf("inventory of a container removed since listing: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("inventory of a removed container = %v, want nothing", paths)
	}
	present := ContainerInfo{ID: "present", Name: "present"}
	if err := os.MkdirAll(filepath.Join(root, "containers", present.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.addContainerRepositoryPaths(paths, []ContainerInfo{present}); err == nil {
		t.Fatal("inventory of a present container whose spec cannot be read succeeded")
	}
}
