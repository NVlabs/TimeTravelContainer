// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlacementLifecycleAndRemovalAreGenerationFenced(t *testing.T) {
	root := t.TempDir()
	store, err := OpenPlacementStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(800, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := store.UpdateStableState("family", "main", "node-a", "boot-a", created.Generation, PlacementPaused, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if paused.Generation != created.Generation+1 || paused.State != PlacementPaused {
		t.Fatalf("paused placement=%+v", paused)
	}
	if err := store.Remove("family", "main", created.Generation); err == nil {
		t.Fatal("stale generation removed placement")
	}
	if err := store.Remove("family", "main", paused.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup("family", "main"); !os.IsNotExist(err) {
		t.Fatalf("removed placement lookup error=%v, want not exist", err)
	}
	unknown := strings.Repeat("a", 64) + ".lock"
	if err := os.WriteFile(filepath.Join(root, unknown), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		branch := fmt.Sprintf("branch-%d", i)
		record, err := store.Create(PlacementRecord{
			FamilyID: "family", BranchID: branch, PhysicalContainer: "family", PhysicalBranch: branch,
			OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Remove("family", branch, record.Generation); err != nil {
			t.Fatal(err)
		}
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("records after removal=%+v err=%v", records, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	foundUnknown := false
	for _, entry := range entries {
		if entry.Name() == unknown {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatalf("unknown placement artifact %q was modified", unknown)
	}
}

func TestMaterializingPlacementBecomesStable(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(850, 0).UTC()
	unplaced, err := store.Create(PlacementRecord{FamilyID: "family", BranchID: "main", State: PlacementUnplaced}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimUnplaced("family", "main", "node-a", "boot-a", "family", "main", unplaced.Generation, PlacementMaterializing, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.UpdateStableState("family", "main", "node-a", "boot-a", claimed.Generation, PlacementRunning, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if running.State != PlacementRunning || running.Generation != claimed.Generation+1 {
		t.Fatalf("stable placement=%+v after materializing=%+v", running, claimed)
	}
}

func TestPlacementQuarantineIsMigrationStableAndGenerationFenced(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(875, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "runtime-a", PhysicalBranch: "fork-a",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := store.SetQuarantine("family", "child", "node-a", "boot-a", created.Generation, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !quarantined.Quarantined || quarantined.Generation != created.Generation {
		t.Fatalf("quarantined placement=%+v", quarantined)
	}
	if _, err := store.SetQuarantine("family", "child", "node-b", "boot-b", created.Generation, false, now.Add(2*time.Second)); err == nil {
		t.Fatal("stale owner changed quarantine policy")
	}
	migrating, err := store.BeginMigration("family", "child", "node-a", "boot-a", created.Generation, "move-1", "node-b", now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.CommitMigration("family", "child", "move-1", "node-b", "boot-b", "runtime-b", "fork-b", now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !migrating.Quarantined || !moved.Quarantined {
		t.Fatalf("migration lost quarantine policy: migrating=%+v moved=%+v", migrating, moved)
	}
}

func TestPlacementRenameReleasesOldName(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(890, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "runtime-a", PhysicalBranch: "child",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.Rename("family", "child", "renamed", "node-a", "boot-a", "renamed", created.Generation, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if renamed.BranchID != "renamed" || renamed.PhysicalBranch != "renamed" || renamed.Generation != created.Generation+1 {
		t.Fatalf("renamed placement=%+v", renamed)
	}
	if _, err := store.Lookup("family", "child"); !os.IsNotExist(err) {
		t.Fatalf("old placement lookup error=%v, want not exist", err)
	}
	if _, err := store.Create(PlacementRecord{FamilyID: "family", BranchID: "child", State: PlacementUnplaced}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reusing old branch name: %v", err)
	}
}

func TestPlacementCatalogLocksReadsAgainstSnapshotWriter(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, time.Unix(900, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- store.withCatalogLock(true, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	lookupDone := make(chan error, 1)
	go func() {
		_, err := store.Lookup("family", "main")
		lookupDone <- err
	}()
	select {
	case err := <-lookupDone:
		t.Fatalf("lookup escaped an exclusive catalog snapshot lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-lookupDone; err != nil {
		t.Fatal(err)
	}
}

func TestPlacementMigrationFencesConcurrentCutovers(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "physical-a", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		op  string
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, operation := range []string{"move-one", "move-two"} {
		go func(operation string) {
			start.Wait()
			_, err := store.BeginMigration("family", "main", "node-a", "boot-a", created.Generation, operation, "node-b", now.Add(time.Second))
			results <- result{operation, err}
		}(operation)
	}
	start.Done()
	winner := ""
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("two migration operations acquired the same generation")
			}
			winner = result.op
		}
	}
	if winner == "" {
		t.Fatal("no migration operation acquired the placement")
	}
	committed, err := store.CommitMigration("family", "main", winner, "node-b", "boot-b", "physical-b", "main", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if committed.OwnerNode != "node-b" || committed.Generation != 2 || committed.State != PlacementRunning {
		t.Fatalf("committed placement=%+v", committed)
	}
	if _, err := store.BeginMigration("family", "main", "node-a", "boot-a", 1, "stale", "node-c", now.Add(3*time.Second)); err == nil {
		t.Fatal("stale owner acquired migrated placement")
	}
}

func TestPlacementAdoptsRestartedOwnerIncarnation(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-old", State: PlacementPaused,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := store.AdoptOwnerIncarnation("family", "main", "node-a", "boot-old", "boot-new", created.Generation, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if adopted.OwnerIncarnation != "boot-new" || adopted.Generation != created.Generation+1 || adopted.State != PlacementPaused {
		t.Fatalf("adopted=%+v", adopted)
	}
	if _, err := store.AdoptOwnerIncarnation("family", "main", "node-a", "boot-old", "boot-stale", created.Generation, now.Add(2*time.Second)); err == nil {
		t.Fatal("stale incarnation adopted placement")
	}
}

func TestPlacementRecoversInterruptedSourceMigration(t *testing.T) {
	store, err := OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(3000, 0).UTC()
	created, err := store.Create(PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-old", State: PlacementRunning,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	migrating, err := store.BeginMigration("family", "main", "node-a", "boot-old", created.Generation, "move-one", "node-b", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverSourceMigration("family", "main", "move-one", "node-a", "boot-old", "boot-new", migrating.Generation, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != PlacementRunning || recovered.OwnerIncarnation != "boot-new" || recovered.Generation != created.Generation+1 || recovered.LastOperationID != "move-one" {
		t.Fatalf("recovered=%+v", recovered)
	}
	// Retrying the exact recovery after an uncertain response is idempotent.
	retried, err := store.RecoverSourceMigration("family", "main", "move-one", "node-a", "boot-old", "boot-new", migrating.Generation, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried.Revision != recovered.Revision || retried.Generation != recovered.Generation {
		t.Fatalf("retry changed placement: got %+v, want %+v", retried, recovered)
	}
	if _, err := store.RecoverSourceMigration("family", "main", "move-two", "node-a", "boot-old", "boot-new", migrating.Generation, now.Add(4*time.Second)); err == nil {
		t.Fatal("unrelated operation recovered placement")
	}
}
