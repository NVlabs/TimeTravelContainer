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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestClusterAdvertiseResolution(t *testing.T) {
	base, name, err := parseClusterAdvertise("node-a.example:0")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("server name=%q", name)
	}
	resolved, err := resolveClusterAdvertise(base, "[::]:17443")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "http://node-a.example:17443" {
		t.Fatalf("resolved URL=%q", resolved)
	}
	page, err := advertisedPageAddress(base)
	if err != nil {
		t.Fatal(err)
	}
	if page != "node-a.example:0" {
		t.Fatalf("page address=%q", page)
	}
}

func TestTransactionTargetsRemovedPlacement(t *testing.T) {
	fork := cluster.Transaction{Identity: cluster.ForkIdentity{
		SourceContainer: "family", SourceBranch: "main", TargetContainer: "family",
		TargetVisibleBranch: "child",
	}}
	if !transactionTargetsRemovedPlacement(fork, cluster.PlacementRecord{FamilyID: "family", BranchID: "child"}) {
		t.Fatal("remote fork did not match its logical target")
	}
	if transactionTargetsRemovedPlacement(fork, cluster.PlacementRecord{FamilyID: "family", BranchID: "main"}) {
		t.Fatal("remote fork matched its source placement")
	}
	physical := fork
	physical.Identity.TargetContainer = "friendly-name"
	if !transactionTargetsRemovedPlacement(physical, cluster.PlacementRecord{FamilyID: "family", BranchID: "child"}) {
		t.Fatal("remote fork did not match the physical source-family placement")
	}
	migration := fork
	migration.Identity.Migration = true
	if !transactionTargetsRemovedPlacement(migration, cluster.PlacementRecord{FamilyID: "family", BranchID: "main"}) {
		t.Fatal("migration did not match its preserved placement")
	}
}

func TestLogicalFamilyPlacementsOrdersCanonicalRuntimeLastForRemoval(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "migrated", PhysicalContainer: "family", PhysicalBranch: "fork-private", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "remote", PhysicalContainer: "target-private", PhysicalBranch: "remote", OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning},
		{FamilyID: "other", BranchID: "unrelated", PhysicalContainer: "other", PhysicalBranch: "unrelated", OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning},
	} {
		if _, err := placements.Create(record, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	records, err := logicalFamilyPlacements(placements, "family")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("logical placements=%+v, want three family branches", records)
	}
	orderFamilyRemovalPlacements(records, "family")
	if got := records[len(records)-1]; got.BranchID != "main" || got.PhysicalContainer != "family" || got.PhysicalBranch != "main" {
		t.Fatalf("final family removal placement=%+v, want canonical main runtime", got)
	}
}

func TestDaemonCommandOwner(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	if err := os.MkdirAll(containerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(containerDir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "bundle", "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{ID: "web", Name: "web", NodeID: "node-b", BundleDir: filepath.Join(containerDir, "bundle")}); err != nil {
		t.Fatal(err)
	}
	if info, err := (ttcbundle.Store{Root: root}).LoadContainer("web"); err != nil || info.NodeID != "node-b" {
		t.Fatalf("loading routing fixture=(%+v, %v)", info, err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"exec", "-it", "web@main", "bash"},
		{"cp", "web@main:/tmp/result", "/tmp/result"},
		{"fork", "web@main", "child"},
	} {
		owner, _, err := daemonCommandPlacementWithCatalog(root, placements, args)
		if err != nil || owner != "node-b" {
			t.Fatalf("daemonCommandPlacementWithCatalog(%v)=(%q, %v)", args, owner, err)
		}
	}
	if owner, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"images"}); err != nil || owner != "" {
		t.Fatalf("image command owner=(%q, %v)", owner, err)
	}
	if owner, claims, err := daemonCommandPlacementWithCatalog(root, placements, []string{"wait", "--condition=durable", "web@main"}); err != nil || owner != "" || len(claims) != 0 {
		t.Fatalf("durable wait placement=(%q, %+v, %v)", owner, claims, err)
	}
	if owner, claims, err := daemonCommandPlacementWithCatalog(root, placements, []string{"port", "publish", "web@main", "8080:80"}); err != nil || owner != "" || len(claims) != 0 {
		t.Fatalf("port ingress placement=(%q, %+v, %v), want local daemon", owner, claims, err)
	}
}

func TestDaemonCommandPlacementFencesCatalogGeneration(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ttcbundle.ContainerInfo{
		ID: "web", Name: "web", NodeID: "node-b", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-b", State: "running", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	owner, claims, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if owner != "node-b" || len(claims) != 1 || claims[0].Generation != created.Generation || claims[0].OwnerIncarnation != "boot-b" {
		t.Fatalf("placement owner=%q claims=%+v", owner, claims)
	}
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", claims, placements); err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	if _, err := placements.BeginMigration("web", "main", "node-b", "boot-b", created.Generation, "move-one", "node-c", time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", claims, placements); err == nil {
		t.Fatal("stale pre-migration claim remained valid")
	}
	if _, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("new command was routed into an in-progress migration")
	}
}

func TestDaemonCommandPlacementRejectsMissingCurrentPlacement(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ttcbundle.ContainerInfo{
		ID: "web", Name: "web", NodeID: "node-b", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-b", State: "running", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("branch without a Raft placement used persistent metadata as a routing fallback")
	}
}

func TestDaemonCommandPlacementUsesInjectedRaftCatalog(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ttcbundle.ContainerInfo{
		ID: "web", Name: "web", NodeID: "node-b", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-b", State: "running", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	// Current-format clustered roots resolve placements only through the
	// injected Raft catalog.
	raftPlacements, err := cluster.OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raftPlacements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	owner, claims, err := daemonCommandPlacementWithCatalog(root, raftPlacements, []string{"rm", "-f", "web@main"})
	if err != nil {
		t.Fatal(err)
	}
	if owner != "node-b" || len(claims) != 1 || claims[0].BranchID != "main" {
		t.Fatalf("placement owner=%q claims=%+v", owner, claims)
	}
}

func TestDaemonCommandMayAwaitColdUnplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "containers", "family-id", "container.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(filepath.Dir(path), "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(path, ttcbundle.ContainerInfo{
		ID: "family-id", Name: "web", State: "dehydrated", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "cold", State: "dehydrated", StorageState: "publishing", PublicationID: "publication-test"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !daemonCommandMayAwaitColdUnplacement(root, nil, []string{"rehydrate", "web@cold"}) {
		t.Fatal("cold rehydrate was not recognized as an asynchronous publication race")
	}
	if daemonCommandMayAwaitColdUnplacement(root, nil, []string{"exec", "web@cold", "true"}) {
		t.Fatal("non-lifecycle command may wait for cold unplacement")
	}
	info, err := (ttcbundle.Store{Root: root}).LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	info.Branches[0].State = "running"
	if err := writeCurrentBundleFixture(path, info); err != nil {
		t.Fatal(err)
	}
	if daemonCommandMayAwaitColdUnplacement(root, nil, []string{"rehydrate", "web@cold"}) {
		t.Fatal("running branch may wait for conflicting placement")
	}
}

type branchAwarePlacementCatalog struct {
	cluster.PlacementCatalog
	branches map[string]cluster.BranchAuthority
}

func TestRemoveContainerPlacementsResolvesMigratedPhysicalBranch(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := cluster.OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "physical", PhysicalBranch: "migration-1",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{nodeID: "node-a", distributedSource: &distributedSourceRuntime{
		placements: placements, transactions: transactions,
	}}
	if err := backend.removeContainerPlacements(ttcbundle.ContainerInfo{
		ID: "physical", DistributedFamilyID: "family",
		Branches: []ttcbundle.BranchInfo{{ID: "migration-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Lookup("family", "main"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed migrated placement lookup error=%v, want not exist", err)
	}
}

func (c branchAwarePlacementCatalog) Branch(family, branch string) (*cluster.BranchAuthority, error) {
	record, ok := c.branches[family+"\x00"+branch]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

func TestDaemonCommandMayAwaitColdUnplacementUsesRaftAuthority(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "containers", "family-id", "container.json")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(filepath.Dir(path), "bundle")
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deliberately stale derived metadata still describes a running branch.
	if err := writeCurrentBundleFixture(path, ttcbundle.ContainerInfo{
		ID: "family-id", Name: "web", State: "running", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "cold", State: "running"}},
	}); err != nil {
		t.Fatal(err)
	}
	base, err := cluster.OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog := branchAwarePlacementCatalog{
		PlacementCatalog: base,
		branches: map[string]cluster.BranchAuthority{"family-id\x00cold": {
			FamilyID: "family-id", BranchID: "cold", Generation: 3,
			Lifecycle: "dehydrating", PublicationID: "publication-3",
		}},
	}
	if !daemonCommandMayAwaitColdUnplacement(root, catalog, []string{"rehydrate", "web@cold"}) {
		t.Fatal("Raft-authorized process-free publication was hidden by stale derived metadata")
	}
	branch := catalog.branches["family-id\x00cold"]
	branch.Lifecycle = "running"
	catalog.branches["family-id\x00cold"] = branch
	if daemonCommandMayAwaitColdUnplacement(root, catalog, []string{"rehydrate", "web@cold"}) {
		t.Fatal("running Raft branch may wait for conflicting placement")
	}
}

func TestWaitForCommittedColdBranchMetadataOrdersPublisherCompletion(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "family-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const publicationID = "publication-test"
	manifest := coldManifestPath(root, "family-id", "cold", publicationID)
	if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(containerDir, "container.json")
	initial := ttcbundle.ContainerInfo{
		ID: "family-id", Name: "web", State: "dehydrated", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			// Model metadata from before BeginBranchPublication reached the
			// shared catalog. Raft has already committed this publication below.
			ID: "cold", State: "running", StorageState: "local",
			SnapshotID: 17, NodeID: "node-old", CreatedAt: time.Unix(1, 0).UTC(),
		}},
	}
	if err := writeCurrentBundleFixture(metadataPath, initial); err != nil {
		t.Fatal(err)
	}
	relativeManifest, err := filepath.Rel(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	authority := cluster.BranchAuthority{
		FamilyID: "family-id", BranchID: "cold", PublicationID: publicationID, Lifecycle: "dehydrated",
		Roots: cluster.DurableRoots{FilesystemRoot: relativeManifest, MemorySnapshot: 17},
	}
	store := ttcbundle.Store{Root: root}
	completed := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		complete := initial
		complete.State = "dehydrated"
		complete.Branches = append([]ttcbundle.BranchInfo(nil), initial.Branches...)
		branch := &complete.Branches[0]
		branch.State = "dehydrated"
		branch.StorageState = "shared"
		branch.PublicationID = publicationID
		branch.StorageManifest = manifest
		branch.NodeID = ""
		branch.Unplaced = true
		completed <- writeCurrentBundleFixture(metadataPath, complete)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, branch, err := waitForCommittedColdBranchMetadata(ctx, store, "family-id", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if info.ID != "family-id" || branch.StorageState != "shared" || branch.StorageManifest != manifest || !branch.Unplaced {
		t.Fatalf("completed metadata info=%+v branch=%+v", info, branch)
	}
}

func TestClaimUnplacedLifecycleTargetRecordsSelectedNode(t *testing.T) {
	root := t.TempDir()
	local := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ttcbundle.ContainerInfo{
		ID: "web", Name: "web", NodeID: "node-b", BundleDir: bundleDir,
		// Model the short interval after Raft publication unplaced the branch but
		// before the derived shared container record cleared its old owner.
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-old", State: "dehydrated", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{FamilyID: "web", BranchID: "main", State: cluster.PlacementUnplaced}, time.Now()); err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<30), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{clusterDirectory: directory, clusterCapabilities: &node.Capabilities}
	backend := &runtimeBackend{
		dataRoot: root, localRoot: local, nodeID: "node-a",
		distributedSource: &distributedSourceRuntime{directory: directory, placements: placements},
	}
	for range 2 {
		if err := server.claimUnplacedLifecycleTargets(backend, []string{"rehydrate", "web@main"}); err != nil {
			t.Fatal(err)
		}
	}
	record, err := placements.Lookup("web", "main")
	if err != nil {
		t.Fatal(err)
	}
	if record.OwnerNode != "node-a" || record.OwnerIncarnation != node.Incarnation || record.State != cluster.PlacementMaterializing {
		t.Fatalf("claimed placement=%+v", record)
	}
	claimed, err := (ttcbundle.Store{Root: root, ActiveRoot: local}).LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if owner := claimed.BranchNodeID("main"); owner != "node-a" || claimed.Branches[0].Unplaced {
		t.Fatalf("claimed metadata owner=%q unplaced=%t", owner, claimed.Branches[0].Unplaced)
	}
	// Model a publisher that passed its generation check just before the
	// pre-command claim and commits an unplaced durable image while rehydrate
	// is already executing. The post-command claim must repair both catalogs.
	if _, err := placements.Unplace("web", "main", "node-a", node.Incarnation, record.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := (ttcbundle.Store{Root: root, ActiveRoot: local}).CompareAndSwapBranchNodeID("web", "main", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	if err := server.claimUnplacedLifecycleTargets(backend, []string{"start", "web@main"}); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := placements.Lookup("web", "main")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.OwnerNode != "node-a" || reclaimed.State != cluster.PlacementMaterializing {
		t.Fatalf("post-publication claimed placement=%+v", reclaimed)
	}
	owner, claims, err := daemonCommandPlacementWithCatalog(root, placements, []string{"rehydrate", "web@main"})
	if err != nil {
		t.Fatal(err)
	}
	if owner != "node-a" || len(claims) != 1 || !claims[0].AllowMaterializing {
		t.Fatalf("materializing rehydrate route=(%q, %+v)", owner, claims)
	}
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-a", claims, placements); err != nil {
		t.Fatalf("materializing rehydrate claim rejected: %v", err)
	}
	if _, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("materializing branch allowed guest execution")
	}
}

func TestClaimUnplacedDistributedPhysicalBranch(t *testing.T) {
	root := t.TempDir()
	local := t.TempDir()
	writeContainer := func(info ttcbundle.ContainerInfo) {
		t.Helper()
		containerDir := filepath.Join(root, "containers", info.ID)
		info.BundleDir = filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
			t.Fatal(err)
		}
	}
	writeContainer(ttcbundle.ContainerInfo{
		ID: "family", Name: "web",
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-b", State: "running", CreatedAt: time.Unix(1, 0).UTC()}},
	})
	writeContainer(ttcbundle.ContainerInfo{
		ID: "physical-child", Name: "physical-child", DistributedFamilyID: "family", DistributedForkTransactionID: "fork-child",
		Branches: []ttcbundle.BranchInfo{{
			ID: "child", Unplaced: true, State: "dehydrated", StorageState: "shared",
			PublicationID: "publication-child", CreatedAt: time.Unix(2, 0).UTC(),
		}},
	})
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{FamilyID: "family", FamilyName: "web", BranchID: "child", State: cluster.PlacementUnplaced}, time.Now()); err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<30), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{clusterDirectory: directory, clusterCapabilities: &node.Capabilities}
	backend := &runtimeBackend{
		dataRoot: root, localRoot: local, nodeID: "node-a",
		distributedSource: &distributedSourceRuntime{directory: directory, placements: placements},
	}
	resolved, info, err := backend.resolveSelector(selector{container: "web", branch: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != (selector{container: "physical-child", branch: "child"}) || info.ID != "physical-child" {
		t.Fatalf("resolved=%+v info=%q, want physical-child@child", resolved, info.ID)
	}
	if err := server.claimUnplacedLifecycleTargets(backend, []string{"rehydrate", "web@child"}); err != nil {
		t.Fatal(err)
	}
	record, err := placements.Lookup("family", "child")
	if err != nil {
		t.Fatal(err)
	}
	if record.OwnerNode != "node-a" || record.State != cluster.PlacementMaterializing || record.PhysicalContainer != "physical-child" || record.PhysicalBranch != "child" {
		t.Fatalf("claimed placement=%+v", record)
	}
	physical, err := (ttcbundle.Store{Root: root, ActiveRoot: local}).LoadRuntimeContainer("physical-child")
	if err != nil {
		t.Fatal(err)
	}
	if owner := physical.BranchNodeID("child"); owner != "node-a" || physical.Branches[0].Unplaced {
		t.Fatalf("physical metadata owner=%q unplaced=%t", owner, physical.Branches[0].Unplaced)
	}
}

func TestResolveSelectorMapsPrivateBranchInSamePhysicalFamily(t *testing.T) {
	root := t.TempDir()
	local := t.TempDir()
	bundleDir := filepath.Join(root, "containers", "family", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(root, "containers", "family", "container.json"), ttcbundle.ContainerInfo{
		ID: "family", Name: "web", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", NodeID: "node-a", State: "running"},
			{ID: "fork-migration", NodeID: "node-a", State: "running", DistributedMigration: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", FamilyName: "web", BranchID: "test",
		PhysicalContainer: "family", PhysicalBranch: "fork-migration",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{
		dataRoot: root, localRoot: local, nodeID: "node-a",
		distributedSource: &distributedSourceRuntime{placements: placements},
	}
	resolved, info, err := backend.resolveSelector(selector{container: "web", branch: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != (selector{container: "family", branch: "fork-migration"}) || info.ID != "family" {
		t.Fatalf("resolved=%+v info=%q, want family@fork-migration", resolved, info.ID)
	}
}

func TestLogicalPlacementForPhysicalTransaction(t *testing.T) {
	state := &cluster.AuthorityState{
		Transactions: map[string]cluster.Transaction{
			"migration": {Identity: cluster.ForkIdentity{
				TransactionID: "migration", Migration: true,
				SourceContainer: "family", SourceBranch: "main",
				TargetContainer: "family", TargetBranch: "physical-migration",
			}},
			"fork": {Identity: cluster.ForkIdentity{
				TransactionID: "fork", SourceContainer: "family", SourceBranch: "main",
				TargetContainer: "family", TargetBranch: "physical-fork", TargetVisibleBranch: "child",
			}},
		},
		Placements: map[string]cluster.PlacementRecord{
			"family\x00main":  {FamilyID: "family", BranchID: "main", PhysicalContainer: "physical", PhysicalBranch: "migration"},
			"family\x00child": {FamilyID: "family", BranchID: "child", PhysicalContainer: "physical", PhysicalBranch: "child"},
		},
	}
	for _, tc := range []struct {
		name, transaction, family, branch string
		found                             bool
	}{
		{name: "migration", transaction: "migration", family: "family", branch: "main", found: true},
		{name: "fork", transaction: "fork", family: "family", branch: "child", found: true},
		{name: "unknown", transaction: "unknown"},
		{name: "empty", transaction: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			family, branch, found := logicalPlacementForPhysicalTransaction(state, tc.transaction)
			if family != tc.family || branch != tc.branch || found != tc.found {
				t.Fatalf("resolved (%q,%q,%t), want (%q,%q,%t)", family, branch, found, tc.family, tc.branch, tc.found)
			}
		})
	}
	if family, branch, found := logicalPlacementForPhysicalTransaction(nil, "migration"); family != "" || branch != "" || found {
		t.Fatalf("nil authority resolved (%q,%q,%t)", family, branch, found)
	}
	for _, tc := range []struct {
		physical, family, branch string
		found                    bool
	}{
		{physical: "migration", family: "family", branch: "main", found: true},
		{physical: "child", family: "family", branch: "child", found: true},
		{physical: "missing"},
	} {
		family, branch, found := logicalPlacementForPhysicalRuntime(state, "physical", tc.physical)
		if family != tc.family || branch != tc.branch || found != tc.found {
			t.Fatalf("physical %q resolved (%q,%q,%t), want (%q,%q,%t)", tc.physical, family, branch, found, tc.family, tc.branch, tc.found)
		}
	}
	logical, found := logicalFamilyForPhysicalRuntime(state, "physical", []string{"migration", "child"})
	if !found || logical.ID != "family" || len(logical.Branches) != 2 || logical.Branches[0].ID != "main" || logical.Branches[1].ID != "child" {
		t.Fatalf("physical family resolved %+v, found=%t", logical, found)
	}
	if logical, found := logicalFamilyForPhysicalRuntime(state, "physical", []string{"migration", "missing"}); found || logical.ID != "" {
		t.Fatalf("partially mapped physical family resolved %+v, found=%t", logical, found)
	}
}

func TestResolveDistributedPersistentBranchUsesPublicationIdentity(t *testing.T) {
	root := t.TempDir()
	writeContainer := func(info ttcbundle.ContainerInfo) {
		t.Helper()
		containerDir := filepath.Join(root, "containers", info.ID)
		info.BundleDir = filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
			t.Fatal(err)
		}
	}
	writeContainer(ttcbundle.ContainerInfo{
		ID: "family", Name: "web", State: "running",
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running", StorageState: "local"}},
	})
	writeContainer(ttcbundle.ContainerInfo{
		ID: "physical-old", Name: "physical-old", State: "exited", DistributedFamilyID: "family",
		Branches: []ttcbundle.BranchInfo{{ID: "transaction-old", State: "exited", StorageState: "shared", PublicationID: "publication-old", StorageManifest: "/shared/old-manifest"}},
	})
	writeContainer(ttcbundle.ContainerInfo{
		ID: "physical-current", Name: "physical-current", State: "dehydrated", DistributedFamilyID: "family",
		Branches: []ttcbundle.BranchInfo{{ID: "transaction-current", State: "dehydrated", StorageState: "shared", PublicationID: "publication-current", StorageManifest: "/shared/current-manifest"}},
	})
	store := ttcbundle.Store{Root: root}
	logical, err := store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	// Store.LoadContainer normally synthesizes the user-facing family from
	// cluster placements. Reproduce that view explicitly: the logical branch
	// advertises the current publication, but it cannot supply the physical
	// bundle/repository or cold manifest that consumes it.
	logical.Branches = []ttcbundle.BranchInfo{{
		ID:            "main",
		State:         "dehydrated",
		StorageState:  "shared",
		PublicationID: "publication-current",
	}}
	backend := &runtimeBackend{dataRoot: root}
	physical, branch, err := backend.resolveDistributedPersistentBranch(logical, "main", "publication-current")
	if err != nil {
		t.Fatal(err)
	}
	if physical.ID != "physical-current" || branch != "transaction-current" {
		t.Fatalf("resolved %s@%s, want physical-current@transaction-current", physical.ID, branch)
	}
}

func TestLastOnlineClusterNode(t *testing.T) {
	directory, err := cluster.OpenNodeDirectory(filepath.Join(t.TempDir(), "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	register := func(id string) *cluster.NodeRecord {
		record := testElasticCapacityRecord(id, 0, 1<<30)
		registered, err := directory.Register(record, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := directory.Transition(id, registered.Incarnation, cluster.NodeReady, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return ready
	}
	a, b := register("node-a"), register("node-b")
	if last, err := lastOnlineClusterNode(directory, a.NodeID); err != nil || last {
		t.Fatalf("two-node result=(%t,%v), want (false,nil)", last, err)
	}
	if _, err := directory.Transition(b.NodeID, b.Incarnation, cluster.NodeDraining, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if last, err := lastOnlineClusterNode(directory, a.NodeID); err != nil || last {
		t.Fatalf("draining-peer result=(%t,%v), want (false,nil)", last, err)
	}
	if _, err := directory.Transition(b.NodeID, b.Incarnation, cluster.NodeOffline, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if last, err := lastOnlineClusterNode(directory, a.NodeID); err != nil || !last {
		t.Fatalf("offline-peer result=(%t,%v), want (true,nil)", last, err)
	}
}

func TestRecoveredPublisherAdoptsAndUnplacesOldIncarnation(t *testing.T) {
	root := t.TempDir()
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "placements"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-old", State: cluster.PlacementRunning,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	publisher := &branchPublisher{nodeID: "node-a", incarnation: "boot-new"}
	if err := publisher.releasePublishedPlacement(placements, "family", "main"); err != nil {
		t.Fatal(err)
	}
	released, err := placements.Lookup("family", "main")
	if err != nil {
		t.Fatal(err)
	}
	if released.State != cluster.PlacementUnplaced || released.OwnerNode != "" || released.OwnerIncarnation != "" || released.Generation != created.Generation+2 {
		t.Fatalf("released placement = %+v", released)
	}
	if err := publisher.releasePublishedPlacement(placements, "family", "main"); err != nil {
		t.Fatalf("idempotent release failed: %v", err)
	}
}

func TestClusterAdvertiseRejectsMalformedOrUnreachableAddresses(t *testing.T) {
	for _, value := range []string{
		"http://node-a.example:7443", "0.0.0.0:7443", "node-a.example", "node-a.example:7443/path",
	} {
		if _, _, err := parseClusterAdvertise(value); err == nil {
			t.Errorf("parseClusterAdvertise(%q) succeeded", value)
		}
	}
}

func TestResumeFencedElasticRuntimeChecksProcessIdentity(t *testing.T) {
	command := exec.Command("sleep", "60")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	start, err := processStartTime(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	localRoot := t.TempDir()
	path := elasticFencePath(localRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(path, elasticFenceRecord{
		Version: 1, NodeID: "node-a", Incarnation: "old", FencedAt: time.Now(),
		Processes: []elasticFencedProcess{{PID: command.Process.Pid, StartTime: start}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{localRoot: localRoot}
	if err := resumeFencedElasticRuntimes(backend, "node-a", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("execution fence remains after resume: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		status, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(command.Process.Pid), "status"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(status), "State:\tT") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fenced process remained stopped")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFencedRuntimeRequiresEveryBranchInCurrentIncarnation(t *testing.T) {
	family := Family{ID: "family", Branches: []Branch{{ID: "main"}, {ID: "child"}}}
	placements := map[string]cluster.PlacementRecord{
		"main":  {OwnerNode: "node-a", OwnerIncarnation: "new", State: cluster.PlacementRunning},
		"child": {OwnerNode: "node-a", OwnerIncarnation: "new", State: cluster.PlacementPaused},
	}
	lookup := func(_, branch string) (*cluster.PlacementRecord, error) {
		placement, ok := placements[branch]
		if !ok {
			return nil, os.ErrNotExist
		}
		return &placement, nil
	}
	if !fencedRuntimeAuthorized(family, "node-a", "new", lookup) {
		t.Fatal("fully re-authorized runtime was rejected")
	}
	corrupted := placements["child"]
	corrupted.State = cluster.PlacementCorrupted
	corrupted.OwnerNode, corrupted.OwnerIncarnation = "", ""
	placements["child"] = corrupted
	if fencedRuntimeAuthorized(family, "node-a", "new", lookup) {
		t.Fatal("runtime containing corrupted branch was re-authorized")
	}
}

func TestRuntimeFamiliesContainToleratesConcurrentEviction(t *testing.T) {
	families := []Family{{ID: "other", Branches: []Branch{{ID: "main"}}}, {
		ID: "family", Branches: []Branch{{ID: "main"}, {ID: "child"}},
	}}
	for _, test := range []struct {
		family string
		branch string
		want   bool
	}{
		{"family", "", true},
		{"family", "child", true},
		{"family", "gone", false},
		{"gone", "", false},
	} {
		if got := runtimeFamiliesContain(families, test.family, test.branch); got != test.want {
			t.Errorf("runtimeFamiliesContain(%q, %q)=%t, want %t", test.family, test.branch, got, test.want)
		}
	}
}

func TestProvisionalRaftTargetAuthorization(t *testing.T) {
	info := ttcbundle.ContainerInfo{
		ID: "family-id", Name: "family",
		DistributedForkImports: map[string]ttcbundle.DistributedForkImport{
			"tx-fork": {BranchID: "child", TargetNode: "node-b"},
		},
	}
	tx := &cluster.Transaction{
		Identity: cluster.ForkIdentity{
			TransactionID: "tx-fork", TargetNodeID: "node-b",
			TargetContainer: "family", TargetVisibleBranch: "child",
		},
		State: cluster.TransactionTargetRestoring, TargetIncarnation: "inc-b",
	}
	load := func(id string) (*cluster.Transaction, error) {
		if id != "tx-fork" {
			return nil, os.ErrNotExist
		}
		copy := *tx
		return &copy, nil
	}
	if !provisionalRaftTargetAuthorized(info, "child", "node-b", "inc-b", load) {
		t.Fatal("current Raft target transaction did not authorize its provisional runtime")
	}
	visible := info
	visible.DistributedForkImports = nil
	visible.Branches = []ttcbundle.BranchInfo{{
		ID: "child", NodeID: "node-b", State: "running", DistributedForkTransactionID: "tx-fork",
	}}
	if !provisionalRaftTargetAuthorized(visible, "child", "node-b", "inc-b", load) {
		t.Fatal("visible target transaction did not authorize pre-placement runtime")
	}
	tx.State = cluster.TransactionPublished
	if !provisionalRaftTargetAuthorized(info, "child", "node-b", "inc-b", load) {
		t.Fatal("published but unactivated migration target lost its provisional grant")
	}
	tx.State = cluster.TransactionTargetRestoring

	for _, test := range []struct {
		name   string
		mutate func(*cluster.Transaction)
	}{
		{"wrong incarnation", func(tx *cluster.Transaction) { tx.TargetIncarnation = "old" }},
		{"wrong node", func(tx *cluster.Transaction) { tx.Identity.TargetNodeID = "node-c" }},
		{"wrong branch", func(tx *cluster.Transaction) { tx.Identity.TargetVisibleBranch = "other" }},
		{"wrong family", func(tx *cluster.Transaction) { tx.Identity.TargetContainer = "other" }},
		{"canceled", func(tx *cluster.Transaction) { tx.State = cluster.TransactionCanceled }},
		{"activated without placement", func(tx *cluster.Transaction) {
			tx.State = cluster.TransactionPublished
			tx.Activated = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := *tx
			test.mutate(&copy)
			if provisionalRaftTargetAuthorized(info, "child", "node-b", "inc-b", func(string) (*cluster.Transaction, error) { return &copy, nil }) {
				t.Fatal("invalid transaction authorized provisional runtime")
			}
		})
	}
}

func TestLeaseGuardianFencesMatchingRuntimeRoot(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	// Use only shell builtins so the synthetic Sentry never has a transient
	// child that inherits the identifying --root argument between discovery
	// and fencing.
	command := exec.Command("bash", "-c", "exec -a 'ttc-sentry[guardian-test]' bash -c 'while :; do read -t 1 || :; done' -- --root="+runtimeRoot)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Signal(syscall.SIGCONT)
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	localRoot := t.TempDir()
	deadline := time.Now().Add(time.Second)
	for {
		processes, err := sentryProcessesForRuntimeRoot(runtimeRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(processes) == 1 && processes[0].PID == command.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guardian did not discover process %d: %+v", command.Process.Pid, processes)
		}
		time.Sleep(time.Millisecond)
	}
	if err := fenceElasticRuntimeRoot(localRoot, runtimeRoot, "node-a", "incarnation-a"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(elasticFencePath(localRoot))
	if err != nil {
		t.Fatal(err)
	}
	var record elasticFenceRecord
	if err := json.Unmarshal(data, &record); err != nil || len(record.Processes) != 1 || record.Processes[0].PID != command.Process.Pid {
		t.Fatalf("bad guardian fence record: %+v err=%v", record, err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		status, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(command.Process.Pid), "status"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(status), "State:\tT") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guardian target was not stopped:\n%s", status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNodeExecutionGrantsOnlyRequireEnforcementOnRevocation(t *testing.T) {
	state := cluster.NewAuthorityState()
	state.Placements["placed"] = cluster.PlacementRecord{
		FamilyID: "family", BranchID: "main", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}
	previous := nodeExecutionGrants(state, "node-a", "inc-a")
	if len(previous) != 1 {
		t.Fatalf("initial grants=%v, want one", previous)
	}
	state.Transactions["tx"] = cluster.Transaction{
		Identity:          cluster.ForkIdentity{TargetNodeID: "node-a", SourceContainer: "family", TargetContainer: "family-alias", TargetVisibleBranch: "child"},
		TargetIncarnation: "inc-a", State: cluster.TransactionTargetRestoring,
	}
	added := nodeExecutionGrants(state, "node-a", "inc-a")
	if executionGrantRevoked(previous, added) {
		t.Fatal("grant addition was classified as a revocation")
	}
	// Publication replaces the provisional transaction grant with a placement
	// grant. The user-facing target name differs from the physical family ID,
	// but the represented runtime is unchanged and must not be evicted.
	state.Transactions["tx"] = cluster.Transaction{
		Identity:          cluster.ForkIdentity{TargetNodeID: "node-a", SourceContainer: "family", TargetContainer: "family-alias", TargetVisibleBranch: "child"},
		TargetIncarnation: "inc-a", State: cluster.TransactionPublished, Activated: true,
	}
	state.Placements["child"] = cluster.PlacementRecord{
		FamilyID: "family", BranchID: "child", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}
	published := nodeExecutionGrants(state, "node-a", "inc-a")
	if executionGrantRevoked(added, published) {
		t.Fatalf("transaction-to-placement handoff was classified as a revocation: before=%v after=%v", added, published)
	}
	delete(state.Placements, "placed")
	revoked := nodeExecutionGrants(state, "node-a", "inc-a")
	if !executionGrantRevoked(published, revoked) {
		t.Fatal("placement removal was not classified as a revocation")
	}
	delete(state.Placements, "child")
	state.Transactions["tx"] = cluster.Transaction{
		Identity:          cluster.ForkIdentity{TargetNodeID: "node-a", SourceContainer: "family", TargetContainer: "family-alias", TargetVisibleBranch: "child"},
		TargetIncarnation: "inc-a", State: cluster.TransactionTargetRestoring,
	}
	provisional := nodeExecutionGrants(state, "node-a", "inc-a")
	state.Transactions["tx"] = cluster.Transaction{
		Identity:          cluster.ForkIdentity{TargetNodeID: "node-a", SourceContainer: "family", TargetContainer: "family-alias", TargetVisibleBranch: "child"},
		TargetIncarnation: "inc-a", State: cluster.TransactionCanceled,
	}
	if !executionGrantRevoked(provisional, nodeExecutionGrants(state, "node-a", "inc-a")) {
		t.Fatal("provisional target cancellation was not classified as a revocation")
	}
}

func TestExecutionRevocationSkipsPausedMigrationProviderCleanup(t *testing.T) {
	const key = "family\x00branch"
	previous := map[string]struct{}{key: {}}
	current := map[string]struct{}{}
	state := cluster.NewAuthorityState()
	state.Transactions["move"] = cluster.Transaction{
		Identity: cluster.ForkIdentity{
			TransactionID: "move", Migration: true,
			SourceNodeID: "node-a", SourceContainer: "family", SourceBranch: "branch",
		},
		SourceIncarnation:  "inc-a",
		OwnershipCommitted: true,
		State:              cluster.TransactionDraining,
	}
	if executionRevocationsRequireImmediateFence(previous, current, state, "node-a", "inc-a") {
		t.Fatal("paused migration provider cleanup required an immediate node fence")
	}
	transaction := state.Transactions["move"]
	transaction.OwnershipCommitted = false
	state.Transactions["move"] = transaction
	if !executionRevocationsRequireImmediateFence(previous, current, state, "node-a", "inc-a") {
		t.Fatal("pre-cutover revocation did not require an immediate node fence")
	}
	transaction.OwnershipCommitted = true
	transaction.SourceIncarnation = "inc-old"
	state.Transactions["move"] = transaction
	if !executionRevocationsRequireImmediateFence(previous, current, state, "node-a", "inc-a") {
		t.Fatal("another incarnation's migration suppressed an immediate node fence")
	}
}

func TestPlacementHasProcessFreePublication(t *testing.T) {
	placement := cluster.PlacementRecord{
		FamilyID: "family", BranchID: "main", Generation: 7,
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}
	state := cluster.NewAuthorityState()
	state.Branches["family\x00main"] = cluster.BranchAuthority{
		FamilyID: "family", BranchID: "main", Generation: 7,
		Lifecycle: "dehydrating", PublicationID: "publication-1",
	}
	state.Publications["arbitrary-map-key"] = cluster.PublicationRecord{
		FamilyID: "family", BranchID: "main", BranchGeneration: 7,
		PublicationID: "publication-1", Lifecycle: "dehydrated", State: cluster.PublicationPreparing,
	}
	if !placementHasProcessFreePublication(state, placement) {
		t.Fatal("exact preparing dehydration publication did not authorize a process-free placement")
	}
	publication := state.Publications["arbitrary-map-key"]
	publication.State = cluster.PublicationReady
	state.Publications["arbitrary-map-key"] = publication
	if !placementHasProcessFreePublication(state, placement) {
		t.Fatal("capture-ready dehydration publication did not authorize a process-free placement")
	}
	publication.State = cluster.PublicationCommitted
	state.Publications["arbitrary-map-key"] = publication
	if placementHasProcessFreePublication(state, placement) {
		t.Fatal("committed publication authorized a stale live placement")
	}
	publication.State = cluster.PublicationPreparing
	publication.BranchGeneration++
	state.Publications["arbitrary-map-key"] = publication
	if placementHasProcessFreePublication(state, placement) {
		t.Fatal("another generation's publication authorized the placement")
	}
	publication.BranchGeneration = placement.Generation
	state.Publications["arbitrary-map-key"] = publication
	branch := state.Branches["family\x00main"]
	branch.Corrupted = true
	state.Branches["family\x00main"] = branch
	if placementHasProcessFreePublication(state, placement) {
		t.Fatal("corrupted branch publication authorized the placement")
	}
}

func TestExecutionRevocationAcceptsMigrationCleanupWithCompletedRemoval(t *testing.T) {
	previous := map[string]struct{}{
		"family\x00migrated": {},
		"family\x00removed":  {},
	}
	state := cluster.NewAuthorityState()
	state.Transactions["move"] = cluster.Transaction{
		Identity: cluster.ForkIdentity{
			TransactionID: "move", Migration: true,
			SourceNodeID: "node-a", SourceContainer: "family", SourceBranch: "migrated",
		},
		SourceIncarnation: "inc-a", OwnershipCommitted: true,
	}
	if executionRevocationsRequireImmediateFence(previous, map[string]struct{}{}, state, "node-a", "inc-a") {
		t.Fatal("independently safe migration cleanup and completed removal required a node fence")
	}
}

func TestExecutionRevocationSkipsCompactedMigrationHandoff(t *testing.T) {
	const key = "family\x00branch"
	previous := map[string]struct{}{key: {}}
	state := cluster.NewAuthorityState()
	state.Placements["replacement"] = cluster.PlacementRecord{
		FamilyID: "family", BranchID: "branch",
		OwnerNode: "node-b", OwnerIncarnation: "inc-b",
		State: cluster.PlacementRunning,
	}
	if executionRevocationsRequireImmediateFence(previous, map[string]struct{}{}, state, "node-a", "inc-a") {
		t.Fatal("completed ownership handoff required an immediate source-node fence")
	}
	placement := state.Placements["replacement"]
	placement.State = cluster.PlacementCorrupted
	state.Placements["replacement"] = placement
	if !executionRevocationsRequireImmediateFence(previous, map[string]struct{}{}, state, "node-a", "inc-a") {
		t.Fatal("corrupted replacement placement suppressed an immediate source-node fence")
	}
	delete(state.Placements, "replacement")
	if executionRevocationsRequireImmediateFence(previous, map[string]struct{}{}, state, "node-a", "inc-a") {
		t.Fatal("fully removed branch required an immediate source-node fence")
	}
	state.Branches["orphan"] = cluster.BranchAuthority{FamilyID: "family", BranchID: "branch"}
	if !executionRevocationsRequireImmediateFence(previous, map[string]struct{}{}, state, "node-a", "inc-a") {
		t.Fatal("branch without a placement suppressed an immediate source-node fence")
	}
}
