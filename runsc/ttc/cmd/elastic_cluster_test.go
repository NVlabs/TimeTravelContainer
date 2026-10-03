// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type transientHeartbeatCatalog struct {
	cluster.NodeCatalog
	attempts int
	success  chan struct{}
}

func (c *transientHeartbeatCatalog) Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*cluster.NodeRecord, error) {
	c.attempts++
	if c.attempts == 1 {
		return nil, errors.New("transient heartbeat failure")
	}
	select {
	case c.success <- struct{}{}:
	default:
	}
	return &cluster.NodeRecord{NodeID: nodeID, Incarnation: incarnation}, nil
}

func TestMaintainElasticNodeRetriesBeforeFencing(t *testing.T) {
	catalog := &transientHeartbeatCatalog{success: make(chan struct{}, 1)}
	done := make(chan struct{})
	defer close(done)
	fatal := make(chan error, 1)
	fenced := make(chan struct{}, 1)
	go maintainElasticNode(catalog, cluster.NodeRecord{NodeID: "node-a", Incarnation: "inc-a"}, "", 900*time.Millisecond, nil, done, nil, func() error {
		fenced <- struct{}{}
		return nil
	}, fatal)
	select {
	case <-catalog.success:
	case err := <-fatal:
		t.Fatalf("node fenced instead of retrying: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for heartbeat retry")
	}
	select {
	case <-fenced:
		t.Fatal("transient heartbeat failure fenced the node")
	default:
	}
}

func TestElasticHeartbeatAttemptBudget(t *testing.T) {
	for _, test := range []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{name: "expired", remaining: 0, want: 0},
		{name: "negative", remaining: -time.Second, want: 0},
		{name: "short", remaining: 3 * time.Millisecond, want: 2 * time.Millisecond},
		{name: "throttled control plane", remaining: 6 * time.Second, want: 4 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := elasticHeartbeatAttemptBudget(test.remaining); got != test.want {
				t.Fatalf("elasticHeartbeatAttemptBudget(%v)=%v, want %v", test.remaining, got, test.want)
			}
		})
	}
}

func TestSerializedAuthorityStateRefreshesAfterLock(t *testing.T) {
	stale := cluster.NewAuthorityState()
	current := stale
	fresh := cluster.NewAuthorityState()
	fresh.Placements["family@main"] = cluster.PlacementRecord{
		FamilyID:  "family",
		BranchID:  "main",
		OwnerNode: "node-a",
		Revision:  1,
	}
	locked := false
	state, unlock, err := serializedAuthorityState(func() func() {
		locked = true
		// Model a command committing placement before releasing the lifecycle
		// lock while a heartbeat is waiting to perform orphan reclamation.
		current = fresh
		return func() { locked = false }
	}, func() (*cluster.AuthorityState, error) {
		if !locked {
			t.Fatal("authority was read without the lifecycle lock")
		}
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Placements["family@main"]; !ok {
		t.Fatal("serialized authority read returned the pre-lock snapshot")
	}
	if !locked {
		t.Fatal("lifecycle lock was released before local reconciliation")
	}
	unlock()
	if locked {
		t.Fatal("unlock did not release the lifecycle lock")
	}
}

func TestDistributedStatsPartitionsEveryPhysicalRuntime(t *testing.T) {
	placements := []cluster.PlacementRecord{
		{OwnerNode: "node-b", PhysicalContainer: "physical-c", BranchID: "z", State: cluster.PlacementPaused},
		{OwnerNode: "node-a", PhysicalContainer: "physical-a", BranchID: "main", State: cluster.PlacementRunning},
		{OwnerNode: "node-a", PhysicalContainer: "physical-a", BranchID: "child", State: cluster.PlacementRunning},
		{OwnerNode: "node-a", PhysicalContainer: "physical-b", BranchID: "other", State: cluster.PlacementRunning},
		{OwnerNode: "node-a", PhysicalContainer: "physical-stopped", BranchID: "cold", State: cluster.PlacementUnplaced},
		{OwnerNode: "", PhysicalContainer: "physical-none", BranchID: "none", State: cluster.PlacementRunning},
	}
	got := distributedStatsPartitions(placements)
	want := []distributedStatsPartition{
		{node: "node-a", physicalContainer: "physical-a", logicalBranch: "child"},
		{node: "node-a", physicalContainer: "physical-b", logicalBranch: "other"},
		{node: "node-b", physicalContainer: "physical-c", logicalBranch: "z"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("distributedStatsPartitions() = %#v, want %#v", got, want)
	}
}

func TestDistributedSourceRuntimeWaitsForScheduledCleanup(t *testing.T) {
	runtime := &distributedSourceRuntime{}
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	runtime.scheduleHandoff(func() {
		started <- struct{}{}
		<-release
	})
	runtime.scheduleSourceMonitor(func() {
		started <- struct{}{}
		<-release
	})
	<-started
	<-started

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := runtime.waitForBackgroundTasks(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait returned %v while cleanup was active, want deadline", err)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.waitForBackgroundTasks(ctx); err != nil {
		t.Fatalf("wait after cleanup: %v", err)
	}
}

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

func TestDistributedSourceTerminalCommandPreservesActivatedMigrationFilesystem(t *testing.T) {
	for _, test := range []struct {
		name string
		tx   *cluster.Transaction
		want string
	}{
		{name: "missing", want: "distributed-cancel"},
		{name: "ordinary-fork", tx: &cluster.Transaction{}, want: "distributed-cancel"},
		{name: "migration-before-cutover", tx: &cluster.Transaction{Identity: cluster.ForkIdentity{Migration: true}}, want: "distributed-cancel"},
		{name: "activated-migration", tx: &cluster.Transaction{Identity: cluster.ForkIdentity{Migration: true}, Activated: true}, want: "distributed-complete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := distributedSourceTerminalCommand(test.tx); got != test.want {
				t.Fatalf("distributedSourceTerminalCommand(%+v)=%q, want %q", test.tx, got, test.want)
			}
		})
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
	if owner, claims, err := daemonCommandPlacementWithCatalog(root, nil, []string{"_remove-canceled-fork-placement", "web@child", "fork-one"}); err != nil || owner != "" || len(claims) != 0 {
		t.Fatalf("canceled target cleanup placement=(%q, %+v, %v), want coordinator-selected node", owner, claims, err)
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
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", []string{"exec", "web@main", "true"}, claims, placements); err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	if _, err := placements.BeginMigration("web", "main", "node-b", "boot-b", created.Generation, "move-one", "node-c", time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", []string{"exec", "web@main", "true"}, claims, placements); err == nil {
		t.Fatal("stale pre-migration claim remained valid")
	}
	if _, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("new command was routed into an in-progress migration")
	}
}

// A dehydration's publication may commit after a command that asks for the
// branch back was routed to its owner. The unplaced branch is then the
// command's to claim; any other command's claim is stale.
func TestPlacementClaimOfABranchUnplacedSinceRoutingAdmitsAClaimingCommand(t *testing.T) {
	root := t.TempDir()
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementPaused,
	}, time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, claims, err := daemonCommandPlacementWithCatalog(root, placements, []string{"rehydrate", "web@main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Unplace("web", "main", "node-b", "boot-b", created.Generation, time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rehydrate", "web@main"}, {"start", "web@main"}, {"restart", "web@main"}} {
		if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", args, claims, placements); err != nil {
			t.Errorf("%v of a branch unplaced since routing: %v", args, err)
		}
	}
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-b", []string{"exec", "web@main", "true"}, claims, placements); err == nil {
		t.Error("exec's claim of a branch unplaced since routing was accepted")
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
		t.Fatal("branch without a authority placement used persistent metadata as a routing fallback")
	}
}

func TestDaemonCommandPlacementUsesInjectedAuthorityCatalog(t *testing.T) {
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
	// injected authority catalog.
	authorityPlacements, err := cluster.OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorityPlacements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	owner, claims, err := daemonCommandPlacementWithCatalog(root, authorityPlacements, []string{"rm", "-f", "web@main"})
	if err != nil {
		t.Fatal(err)
	}
	if owner != "node-b" || len(claims) != 1 || claims[0].BranchID != "main" {
		t.Fatalf("placement owner=%q claims=%+v", owner, claims)
	}
}

func TestLoadDistributedFamilyIdentityUsesAuthorityDuringNFSVisibilityLag(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family-id", FamilyName: "web", BranchID: "main",
		PhysicalContainer: "family-id", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementPaused,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The store deliberately contains no container directory, modeling an NFS
	// client whose negative-entry cache has not observed the owner's metadata.
	info, err := loadDistributedFamilyIdentity(ttcbundle.Store{Root: t.TempDir()}, placements, "web")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "family-id" || info.DistributedFamilyID != "family-id" || info.Name != "web" {
		t.Fatalf("identity=%+v", info)
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

func TestRemoveContainerPlacementsDoesNotMatchStaleLogicalNameInPhysicalRuntime(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := cluster.OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", BranchID: "child", PhysicalContainer: "physical", PhysicalBranch: "migration-current",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{nodeID: "node-a", distributedSource: &distributedSourceRuntime{
		placements: placements, transactions: transactions,
	}}
	if err := backend.removeContainerPlacements(ttcbundle.ContainerInfo{
		ID: "physical", DistributedFamilyID: "family", DistributedForkTransactionID: "fork-original",
		Branches: []ttcbundle.BranchInfo{{ID: "child"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Lookup("family", "child"); err != nil {
		t.Fatalf("stale physical branch removed current placement: %v", err)
	}
}

func TestRemoveLogicalBranchPlacementRemovesPublishedUnplacedBranch(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := cluster.OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", BranchID: "cold", State: cluster.PlacementUnplaced,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{nodeID: "node-a", distributedSource: &distributedSourceRuntime{
		placements: placements, transactions: transactions,
	}}
	if err := backend.removeLogicalBranchPlacement("family", "cold"); err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Lookup("family", "cold"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed published placement lookup error=%v, want not exist", err)
	}
	// Retries after a lost response must remain harmless.
	if err := backend.removeLogicalBranchPlacement("family", "cold"); err != nil {
		t.Fatalf("idempotent removal: %v", err)
	}
}

func TestDistributedFamilyHasPeerPlacementUsesLogicalFamily(t *testing.T) {
	standalone := &runtimeBackend{}
	if peers, err := standalone.distributedFamilyHasPeerPlacement("family", "main"); err != nil || peers {
		t.Fatalf("standalone peer lookup = (%t, %v), want (false, nil)", peers, err)
	}

	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, record := range []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "child", PhysicalContainer: "physical-child", PhysicalBranch: "child", OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning},
		{FamilyID: "other", BranchID: "main", PhysicalContainer: "other", PhysicalBranch: "main", OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning},
	} {
		if _, err := placements.Create(record, now); err != nil {
			t.Fatal(err)
		}
	}
	backend := &runtimeBackend{nodeID: "node-a", distributedSource: &distributedSourceRuntime{placements: placements}}
	if peers, err := backend.distributedFamilyHasPeerPlacement("family", "main"); err != nil || !peers {
		t.Fatalf("cluster peer lookup = (%t, %v), want (true, nil)", peers, err)
	}
	child, err := placements.Lookup("family", "child")
	if err != nil {
		t.Fatal(err)
	}
	if err := placements.Remove("family", "child", child.Generation); err != nil {
		t.Fatal(err)
	}
	if peers, err := backend.distributedFamilyHasPeerPlacement("family", "main"); err != nil || peers {
		t.Fatalf("final logical branch lookup = (%t, %v), want (false, nil)", peers, err)
	}
}

func (c branchAwarePlacementCatalog) Branch(family, branch string) (*cluster.BranchAuthority, error) {
	record, ok := c.branches[family+"\x00"+branch]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &record, nil
}

func TestProcessFreeStartRelocationEligibility(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "family")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "family", Name: "web", BundleDir: bundleDir, State: "created",
		Branches: []ttcbundle.BranchInfo{{ID: "main", FilesystemBranch: "main", State: "created"}},
	}); err != nil {
		t.Fatal(err)
	}
	base, err := cluster.OpenPlacementStore(filepath.Join(root, "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Create(cluster.PlacementRecord{
		FamilyID: "family", FamilyName: "web", BranchID: "main", OwnerNode: "node-a", OwnerIncarnation: "inc-a",
		PhysicalContainer: "family", PhysicalBranch: "main", State: cluster.PlacementMaterializing,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	catalog := branchAwarePlacementCatalog{PlacementCatalog: base, branches: map[string]cluster.BranchAuthority{
		"family\x00main": {FamilyID: "family", BranchID: "main", Lifecycle: "created"},
	}}
	operands, ok, err := processFreeStartRelocationOperands(root, catalog, []string{"start", "-a", "web@main"})
	if err != nil || !ok || !slices.Equal(operands, []string{"web@main"}) {
		t.Fatalf("created relocation = (%v, %t, %v)", operands, ok, err)
	}
	owner, claims, err := daemonCommandPlacementWithCatalog(root, catalog, []string{"cp", "/etc/hostname", "web@main:/prepared"})
	if err != nil || owner != "node-a" || len(claims) != 1 || !claims[0].AllowMaterializing {
		t.Fatalf("created cp route = (%q, %+v, %v)", owner, claims, err)
	}
	branch := catalog.branches["family\x00main"]
	branch.Lifecycle = "running"
	catalog.branches["family\x00main"] = branch
	if _, ok, err := processFreeStartRelocationOperands(root, catalog, []string{"start", "web@main"}); err != nil || ok {
		t.Fatalf("running relocation eligibility = (%t, %v), want false", ok, err)
	}
	// A branch whose rehydration failed on the node that claimed it can be
	// removed there, and nothing else can be done to it but retrying. The
	// claim made its lifecycle the placement's.
	branch.Lifecycle = "materializing"
	catalog.branches["family\x00main"] = branch
	owner, claims, err = daemonCommandPlacementWithCatalog(root, catalog, []string{"rm", "-f", "web@main"})
	if err != nil || owner != "node-a" || len(claims) != 1 || !claims[0].AllowMaterializing {
		t.Fatalf("rm of a branch whose rehydration failed routes to (%q, %+v, %v)", owner, claims, err)
	}
	for _, args := range [][]string{{"exec", "web@main", "true"}, {"cp", "/etc/hostname", "web@main:/prepared"}} {
		if _, _, err := daemonCommandPlacementWithCatalog(root, catalog, args); err == nil {
			t.Fatalf("%v of a materializing dehydrated branch was routed", args)
		}
	}
}

func TestDaemonCommandMayAwaitColdUnplacementUsesAuthority(t *testing.T) {
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
		t.Fatal("authority-authorized process-free publication was hidden by stale derived metadata")
	}
	branch := catalog.branches["family-id\x00cold"]
	branch.Lifecycle = "running"
	catalog.branches["family-id\x00cold"] = branch
	if daemonCommandMayAwaitColdUnplacement(root, catalog, []string{"rehydrate", "web@cold"}) {
		t.Fatal("running authority branch may wait for conflicting placement")
	}
}

func TestAwaitDaemonColdUnplacementWaitsForSameDestinationOwner(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "family-id")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Derived metadata may still name the old owner after stop has captured the
	// process-free state. The authority lifecycle state is authoritative for deciding
	// whether start may wait for the asynchronous placement release.
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "family-id", Name: "web", State: "exited", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "cold", NodeID: "node-a", State: "exited"}},
	}); err != nil {
		t.Fatal(err)
	}
	base, err := cluster.OpenPlacementStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, err := base.Create(cluster.PlacementRecord{
		FamilyID: "family-id", BranchID: "cold", PhysicalContainer: "family-id", PhysicalBranch: "cold",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	catalog := branchAwarePlacementCatalog{
		PlacementCatalog: base,
		branches: map[string]cluster.BranchAuthority{"family-id\x00cold": {
			FamilyID: "family-id", BranchID: "cold", Generation: record.Generation,
			Lifecycle: "exited", Roots: cluster.DurableRoots{FilesystemRoot: "cold/fs.manifest"},
		}},
	}
	if !daemonCommandMayAwaitColdUnplacement(root, catalog, []string{"start", "web@cold"}) {
		t.Fatal("exited branch was not recognized as awaiting placement release")
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, err := base.Unplace("family-id", "cold", "node-a", "inc-a", record.Generation, time.Now())
		released <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	owner, claims, err := awaitDaemonColdUnplacement(ctx, root, catalog, []string{"start", "web@cold"})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if owner != "" || len(claims) != 0 {
		t.Fatalf("awaitDaemonColdUnplacement owner=%q claims=%+v, want unplaced", owner, claims)
	}
}

func TestWaitForCommittedColdBranchMetadataRepairsPublisherCrash(t *testing.T) {
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
	encodedManifest, err := json.Marshal(coldBranchManifest{
		Version: 5, Container: "family-id", Branch: "cold", PublicationID: publicationID, Kind: "dehydrate",
		MemorySnapshot: 17,
		ControlRoot:    ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(encodedManifest, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(containerDir, "container.json")
	initial := ttcbundle.ContainerInfo{
		ID: "family-id", Name: "web", State: "dehydrated", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			// Model a publisher crash after the authority committed the new roots but before
			// it replaced the physical branch's preceding shared manifest.
			ID: "cold", State: "running", StorageState: "local",
			PublicationID:   publicationID,
			StorageManifest: coldManifestPath(root, "family-id", "cold", "publication-previous"),
			SnapshotID:      17, NodeID: "node-old", CreatedAt: time.Unix(1, 0).UTC(),
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, branch, err := waitForCommittedColdBranchMetadata(ctx, store, "family-id", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "family-id" || branch.StorageState != "shared" || branch.StorageManifest != manifest || !branch.Unplaced {
		t.Fatalf("completed metadata info=%+v branch=%+v", info, branch)
	}
}

func TestWaitForCommittedColdBranchMetadataWaitsForManifestVisibility(t *testing.T) {
	root := t.TempDir()
	const publicationID = "publication-delayed"
	containerDir := filepath.Join(root, "containers", "physical")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := coldManifestPath(root, "physical", "cold", publicationID)
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "physical", Name: "physical", DistributedFamilyID: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			ID: "cold", State: "dehydrated", Unplaced: true, StorageState: "shared",
			PublicationID: publicationID, StorageManifest: manifestPath, SnapshotID: 19,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	relativeManifest, err := filepath.Rel(root, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	authority := cluster.BranchAuthority{
		FamilyID: "family", BranchID: "cold", PublicationID: publicationID, Lifecycle: "dehydrated",
		Roots: cluster.DurableRoots{FilesystemRoot: relativeManifest, MemorySnapshot: 19},
	}
	written := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
			written <- err
			return
		}
		encoded, err := json.Marshal(coldBranchManifest{
			Version: 5, Container: "physical", Branch: "cold", PublicationID: publicationID,
			Kind: "dehydrate", MemorySnapshot: 19,
			ControlRoot: ttcsnapshot.ObjectID{Snapshot: 19, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
		})
		if err == nil {
			// Production publication installs manifests with a durable rename.
			// Preserve that atomic-visibility contract in this timing test; a
			// direct WriteFile can expose a transient empty JSON file and tests a
			// corruption path rather than delayed NFS visibility.
			err = writeDurableJSON(manifestPath, json.RawMessage(encoded))
		}
		written <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, branch, err := waitForCommittedColdBranchMetadata(ctx, ttcbundle.Store{Root: root}, "family", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if branch.PublicationID != publicationID || branch.SnapshotID != 19 {
		t.Fatalf("resolved branch=%+v", branch)
	}
}

func TestWaitForCommittedColdBranchMetadataIgnoresLocalCacheState(t *testing.T) {
	root := t.TempDir()
	const publicationID = "publication-cold"
	containerDir := filepath.Join(root, "containers", "physical")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := coldManifestPath(root, "physical", "cold", publicationID)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := coldBranchManifest{
		Version: 5, Container: "physical", Branch: "cold", PublicationID: publicationID, Kind: "dehydrate",
		MemorySnapshot: 17,
		ControlRoot:    ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "physical", Name: "physical", DistributedFamilyID: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			ID: "cold", State: "dehydrated", Unplaced: true,
			StorageState: "materializing", PublicationID: publicationID, StorageManifest: manifestPath,
			SnapshotID: 17,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	relativeManifest, err := filepath.Rel(root, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	authority := cluster.BranchAuthority{
		FamilyID: "family", BranchID: "cold", PublicationID: publicationID, Lifecycle: "dehydrated",
		Roots: cluster.DurableRoots{FilesystemRoot: relativeManifest, MemorySnapshot: 17},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, branch, err := waitForCommittedColdBranchMetadata(ctx, ttcbundle.Store{Root: root}, "family", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if branch.PublicationID != publicationID || branch.StorageManifest != manifestPath {
		t.Fatalf("resolved branch=%+v", branch)
	}
}

func TestWaitForCommittedColdBranchMetadataUsesFilesystemRootAfterMemoryDiscard(t *testing.T) {
	root := t.TempDir()
	const (
		publicationID  = "publication-cold"
		physicalID     = "physical-cold"
		physicalBranch = "transaction-cold"
	)
	containerDir := filepath.Join(root, "containers", physicalID)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := coldManifestPath(root, physicalID, physicalBranch, publicationID)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	encodedManifest, err := json.Marshal(coldBranchManifest{
		Version: 5, Container: physicalID, Branch: physicalBranch, PublicationID: publicationID, Kind: "dehydrate",
		MemorySnapshot: 17,
		ControlRoot:    ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(encodedManifest, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: physicalID, Name: physicalID, DistributedFamilyID: "family-id", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			ID: physicalBranch, State: "exited", Unplaced: true, StorageState: "shared",
			PublicationID: publicationID, StorageManifest: manifestPath,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	relativeManifest, err := filepath.Rel(root, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	authority := cluster.BranchAuthority{
		FamilyID: "family-id", BranchID: "cold", Lifecycle: "exited",
		Roots: cluster.DurableRoots{FilesystemRoot: relativeManifest},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, branch, err := waitForCommittedColdBranchMetadata(ctx, ttcbundle.Store{Root: root}, "family-id", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != physicalID || branch.ID != physicalBranch || branch.PublicationID != "" {
		t.Fatalf("resolved info=%q branch=%+v", info.ID, branch)
	}
}

func TestWaitForCommittedColdBranchMetadataRepairsLifecycleAfterMemoryDiscard(t *testing.T) {
	root := t.TempDir()
	const (
		publicationID  = "publication-cold"
		physicalID     = "physical-cold"
		physicalBranch = "transaction-cold"
	)
	containerDir := filepath.Join(root, "containers", physicalID)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := coldManifestPath(root, physicalID, physicalBranch, publicationID)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	encodedManifest, err := json.Marshal(coldBranchManifest{
		Version: 5, Container: physicalID, Branch: physicalBranch,
		PublicationID: publicationID, Kind: "dehydrate", MemorySnapshot: 17,
		ControlRoot: ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(encodedManifest, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	status := 137
	exitedAt := time.Unix(17, 0).UTC()
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: physicalID, Name: physicalID, DistributedFamilyID: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{
			ID: physicalBranch, State: "dehydrated", Unplaced: true,
			ExitStatus: &status, ExitedAt: &exitedAt, StorageState: "shared",
			PublicationID: publicationID, StorageManifest: manifestPath,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	relativeManifest, err := filepath.Rel(root, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	authority := cluster.BranchAuthority{
		FamilyID: "family", BranchID: "cold", Lifecycle: "exited",
		Roots: cluster.DurableRoots{FilesystemRoot: relativeManifest},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, branch, err := waitForCommittedColdBranchMetadata(ctx, ttcbundle.Store{Root: root}, "family", "cold", authority)
	if err != nil {
		t.Fatal(err)
	}
	if branch.State != "exited" || branch.PublicationID != "" || branch.ExitStatus == nil || *branch.ExitStatus != status || branch.ExitedAt == nil {
		t.Fatalf("repaired branch=%+v, want exited lifecycle with retained status", branch)
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
		// Model the short interval after the authority publication unplaced the branch but
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
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
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
	if err := validateDaemonPlacementClaimsWithCatalog(root, "node-a", []string{"rehydrate", "web@main"}, claims, placements); err != nil {
		t.Fatalf("materializing rehydrate claim rejected: %v", err)
	}
	if _, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("materializing branch allowed guest execution")
	}
	// Its memory past its target, the node admits no cold branch, but a
	// branch it ran stays its own: the claim repeated once the command ran
	// succeeds.
	if _, err := directory.Heartbeat(node.NodeID, node.Incarnation, 1<<61, time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := server.claimUnplacedLifecycleTargets(backend, []string{"start", "web@main"}); err == nil || !strings.Contains(err.Error(), "state=pressured") {
		t.Fatalf("a pressured node admitted a cold branch: %v", err)
	}
	if err := server.reclaimCompletedLifecycleTargets(backend, []string{"start", "web@main"}); err != nil {
		t.Fatalf("re-claiming the branch a pressured node ran: %v", err)
	}
}

// Placement reconciliation leaves alone a family that a long operation works
// on without the lifecycle lock: its runtime and records are changing.
func TestPlacementReconciliationLeavesABusyFamily(t *testing.T) {
	root := t.TempDir()
	runsc := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	// A running placement with neither a runtime nor a record on its node is
	// what a removal leaves while it works.
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: node.Incarnation, State: cluster.PlacementRunning,
	}, now); err != nil {
		t.Fatal(err)
	}
	var operations familyOperations
	op := &familyOperation{id: "web", name: "web", longOperation: longOperation{state: "removing", shown: true}}
	if err := operations.begin(op); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{
		root: filepath.Join(root, "runsc-root"), runsc: runsc, dataRoot: root, localRoot: t.TempDir(), nodeID: "node-a",
		operations: &operations, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard,
		distributedSource: &distributedSourceRuntime{directory: directory, placements: placements},
	}
	if err := backend.reconcilePlacementCatalog(); err != nil {
		t.Fatalf("reconcilePlacementCatalog: %v", err)
	}
	if _, err := placements.Lookup("web", "main"); err != nil {
		t.Fatalf("the placement of a family a long operation works on was removed: %v", err)
	}
	operations.end(op)
	// Retiring the removed placement's transactions needs the authority,
	// which this catalog has not; the removal itself comes first.
	if err := backend.reconcilePlacementCatalog(); err != nil && !strings.Contains(err.Error(), "retiring transactions for removed placement web@main") {
		t.Fatalf("reconcilePlacementCatalog: %v", err)
	}
	if _, err := placements.Lookup("web", "main"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale placement of an idle family remains: %v", err)
	}
}

// A reconciliation of the families a command names repairs only theirs: the
// stale placement of another family stays until a reconciliation of every
// family.
func TestPlacementReconciliationOfOneFamily(t *testing.T) {
	root := t.TempDir()
	runsc := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Running placements with neither a runtime nor a record on their node
	// are stale.
	for _, family := range []string{"web", "db"} {
		if _, err := placements.Create(cluster.PlacementRecord{
			FamilyID: family, BranchID: "main", PhysicalContainer: family, PhysicalBranch: "main",
			OwnerNode: "node-a", OwnerIncarnation: node.Incarnation, State: cluster.PlacementRunning,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	backend := &runtimeBackend{
		root: filepath.Join(root, "runsc-root"), runsc: runsc, dataRoot: root, localRoot: t.TempDir(), nodeID: "node-a",
		operations: &familyOperations{}, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard,
		distributedSource: &distributedSourceRuntime{directory: directory, placements: placements},
	}
	reconcile := func(families ...string) {
		t.Helper()
		// Retiring a removed placement's transactions needs the authority,
		// which this catalog has not; the removal itself comes first.
		if err := backend.reconcilePlacementCatalogOf(families...); err != nil && !strings.Contains(err.Error(), "retiring transactions for removed placement") {
			t.Fatalf("reconcilePlacementCatalogOf(%q): %v", families, err)
		}
	}
	reconcile("web")
	if _, err := placements.Lookup("web", "main"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale placement of the family reconciled remains: %v", err)
	}
	if _, err := placements.Lookup("db", "main"); err != nil {
		t.Fatalf("the placement of a family not reconciled was removed: %v", err)
	}
	reconcile()
	if _, err := placements.Lookup("db", "main"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale placement of another family remains after reconciling every family: %v", err)
	}
}

// The cleanup of runtime branches a node does not own leaves alone one that a
// command there is making: a fork's new branch runs before its record names
// it, as a checkpoint's hidden branch does all its life.
func TestNonOwnedBranchCleanupLeavesABranchBeingMade(t *testing.T) {
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
		ID: "web", Name: "web", NodeID: "node-a", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-a", State: "running", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	// The runtime runs main and a new b1, and logs what it is asked to do.
	calls := filepath.Join(t.TempDir(), "calls")
	runsc := filepath.Join(t.TempDir(), "runsc")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
		"[ \"$3\" = list ] && { echo '[{\"id\":\"web\",\"status\":\"running\"}]'; exit 0; }\n" +
		"[ \"$5\" = branches ] && { echo '[{\"ID\":\"main\",\"State\":\"running\"},{\"ID\":\"b1\",\"State\":\"running\"}]'; exit 0; }\n" +
		"[ \"$5\" = destroy ] && exit 0\nexit 2\n"
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	destroyed := func() bool {
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(data), "timetravel web destroy b1")
	}
	var operations familyOperations
	made := operations.beginMaking("web", "b1")
	backend := &runtimeBackend{
		root: filepath.Join(root, "runsc-root"), runsc: runsc, dataRoot: root, localRoot: t.TempDir(), nodeID: "node-a",
		operations: &operations, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard,
	}
	if err := backend.cleanupNonOwnedRuntimeBranches(); err != nil {
		t.Fatalf("cleanupNonOwnedRuntimeBranches: %v", err)
	}
	if destroyed() {
		t.Fatal("the cleanup destroyed a branch a fork was making")
	}
	made()
	made()
	if operations.beingMade("web", "b1") {
		t.Fatal("b1 is still being made after its fork finished")
	}
	if err := backend.cleanupNonOwnedRuntimeBranches(); err != nil {
		t.Fatalf("cleanupNonOwnedRuntimeBranches: %v", err)
	}
	if !destroyed() {
		t.Fatal("the cleanup left a branch no record gives this node and no command makes")
	}
}

// A rehydration makes its branch routable even while a long operation, a
// remote fork's handoff for one, works on its family: the reconciliation
// after the command passes over such a family.
func TestRehydrationActivatesItsPlacementInABusyFamily(t *testing.T) {
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
		ID: "web", Name: "web", NodeID: "node-a", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", NodeID: "node-a", State: "running", CreatedAt: time.Unix(1, 0).UTC()},
			{ID: "b1", ParentID: "main", NodeID: "node-a", State: "running", CreatedAt: time.Unix(2, 0).UTC()},
		},
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), info); err != nil {
		t.Fatal(err)
	}
	// The runtime runs b1, and not main.
	runsc := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\nfor a; do [ \"$a\" = branches ] && { echo '[{\"ID\":\"b1\",\"State\":\"running\"}]'; exit 0; }; done\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "b1"} {
		if _, err := placements.Create(cluster.PlacementRecord{
			FamilyID: "web", BranchID: branch, PhysicalContainer: "web", PhysicalBranch: branch,
			OwnerNode: "node-a", OwnerIncarnation: node.Incarnation, State: cluster.PlacementMaterializing,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	var operations familyOperations
	op := &familyOperation{id: "web", name: "web", longOperation: longOperation{branch: "main", state: "forking"}}
	if err := operations.begin(op); err != nil {
		t.Fatal(err)
	}
	defer operations.end(op)
	server := &daemonServer{clusterDirectory: directory, clusterCapabilities: &node.Capabilities}
	backend := &runtimeBackend{
		root: filepath.Join(root, "runsc-root"), runsc: runsc, dataRoot: root, localRoot: local, nodeID: "node-a",
		operations: &operations, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard,
		distributedSource: &distributedSourceRuntime{directory: directory, placements: placements},
	}
	for _, target := range []string{"web@b1", "web@main"} {
		if err := server.activateMaterializedLifecycleTargets(backend, []string{"rehydrate", target}); err != nil {
			t.Fatal(err)
		}
	}
	if record, err := placements.Lookup("web", "b1"); err != nil || record.State != cluster.PlacementRunning {
		t.Fatalf("rehydrated placement=%+v, %v, want running", record, err)
	}
	if owner, _, err := daemonCommandPlacementWithCatalog(root, placements, []string{"exec", "web@b1", "true"}); err != nil || owner != "node-a" {
		t.Fatalf("exec on the rehydrated branch routes to (%q, %v)", owner, err)
	}
	// A branch whose runtime does not run it is not activated.
	if record, err := placements.Lookup("web", "main"); err != nil || record.State != cluster.PlacementMaterializing {
		t.Fatalf("placement of a branch with no runtime=%+v, %v, want materializing", record, err)
	}
}

func TestRegisterCreatedContainerPlacementRetainsLocalOwner(t *testing.T) {
	root := t.TempDir()
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "placements"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := cluster.OpenNodeDirectory(filepath.Join(root, "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if node, err = directory.Transition(node.NodeID, node.Incarnation, cluster.NodeReady, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{nodeID: "node-a", distributedSource: &distributedSourceRuntime{
		directory:  directory,
		placements: placements,
		// Presence identifies the production authority-backed cluster path. The
		// placement store remains a deterministic catalog test double.
		authority: &cluster.AuthorityCatalog{},
	}}
	created := time.Unix(123, 0).UTC()
	info := ttcbundle.ContainerInfo{
		ID: "family", Name: "web", Reference: "busybox:1.36", Digest: "sha256:image", ImageSize: 42,
		LiveForkEligible: true, CreatedAt: created,
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "created", CreatedAt: created}},
	}
	if err := backend.registerCreatedContainerPlacement(info); err != nil {
		t.Fatal(err)
	}
	record, err := placements.Lookup("family", "main")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != cluster.PlacementMaterializing || record.OwnerNode != "node-a" || record.OwnerIncarnation != node.Incarnation || record.PhysicalContainer != "family" || record.PhysicalBranch != "main" || record.FamilyName != "web" || record.ImageReference != "busybox:1.36" {
		t.Fatalf("created placement=%+v", record)
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
	node, err := directory.Register(testElasticCapacityRecord("node-a", 0, 1<<60), now, time.Minute)
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
			}, TargetPhysicalContainer: "physical", TargetPhysicalBranch: "migration"},
			"fork": {Identity: cluster.ForkIdentity{
				TransactionID: "fork", SourceContainer: "family", SourceBranch: "main",
				TargetContainer: "family", TargetBranch: "physical-fork", TargetVisibleBranch: "child",
			}, TargetPhysicalContainer: "physical", TargetPhysicalBranch: "child"},
			"placement-fallback": {Identity: cluster.ForkIdentity{
				TransactionID: "placement-fallback", SourceContainer: "fallback-family", SourceBranch: "main",
				TargetContainer: "fallback-family", TargetBranch: "physical-fallback", TargetVisibleBranch: "base",
			}},
		},
		Placements: map[string]cluster.PlacementRecord{
			"family\x00main":          {FamilyID: "family", BranchID: "main", PhysicalContainer: "physical", PhysicalBranch: "migration"},
			"family\x00child":         {FamilyID: "family", BranchID: "child", PhysicalContainer: "physical", PhysicalBranch: "child"},
			"fallback-family\x00base": {FamilyID: "fallback-family", BranchID: "base", PhysicalContainer: "fallback-physical", PhysicalBranch: "base"},
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
	for _, tc := range []struct {
		name, transaction, physicalContainer, physicalBranch, family, branch string
		found                                                                bool
	}{
		{name: "fork descendant", transaction: "fork", physicalContainer: "physical", physicalBranch: "grandchild", family: "family", branch: "grandchild", found: true},
		{name: "migration descendant", transaction: "migration", physicalContainer: "physical", physicalBranch: "debug", family: "family", branch: "debug", found: true},
		{name: "placement fallback", transaction: "placement-fallback", physicalContainer: "fallback-physical", physicalBranch: "sibling", family: "fallback-family", branch: "sibling", found: true},
		{name: "original branch", transaction: "fork", physicalContainer: "physical", physicalBranch: "child"},
		{name: "wrong runtime", transaction: "fork", physicalContainer: "other", physicalBranch: "grandchild"},
		{name: "unknown transaction", transaction: "missing", physicalContainer: "physical", physicalBranch: "grandchild"},
	} {
		t.Run("descendant/"+tc.name, func(t *testing.T) {
			family, branch, found := logicalPlacementForUnmappedTransactionDescendant(state, tc.transaction, tc.physicalContainer, tc.physicalBranch)
			if family != tc.family || branch != tc.branch || found != tc.found {
				t.Fatalf("resolved (%q,%q,%t), want (%q,%q,%t)", family, branch, found, tc.family, tc.branch, tc.found)
			}
		})
	}
}

func TestAuthorityProtectsLocalContainer(t *testing.T) {
	state := &cluster.AuthorityState{
		Placements: map[string]cluster.PlacementRecord{
			"placed\x00main": {
				FamilyID: "placed", PhysicalContainer: "physical-placed", OwnerNode: "node-a",
			},
		},
		Transactions: map[string]cluster.Transaction{
			"cold-source": {
				Identity: cluster.ForkIdentity{
					SourceNodeID: "node-a", SourceContainer: "logical", TargetNodeID: "node-b", TargetContainer: "logical",
				},
				SourcePhysicalContainer: "physical-cold", TargetPhysicalContainer: "physical-target", SourceNeeded: true,
			},
			"released-source": {
				Identity:                cluster.ForkIdentity{SourceNodeID: "node-a", SourceContainer: "released"},
				SourcePhysicalContainer: "physical-released", SourceNeeded: false,
			},
			"identity-fallback": {
				Identity: cluster.ForkIdentity{
					SourceNodeID: "node-a", SourceContainer: "fallback-source", TargetNodeID: "node-a", TargetContainer: "fallback-target",
				},
				SourceNeeded: true,
			},
		},
	}
	for _, test := range []struct {
		name, node, container string
		want                  bool
	}{
		{name: "placement", node: "node-a", container: "physical-placed", want: true},
		{name: "cold source", node: "node-a", container: "physical-cold", want: true},
		{name: "target", node: "node-b", container: "physical-target", want: true},
		{name: "released source", node: "node-a", container: "physical-released", want: false},
		{name: "source fallback", node: "node-a", container: "fallback-source", want: true},
		{name: "target fallback", node: "node-a", container: "fallback-target", want: true},
		{name: "wrong node", node: "node-c", container: "physical-cold", want: false},
		{name: "unknown", node: "node-a", container: "unknown", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := authorityProtectsLocalContainer(state, test.node, test.container); got != test.want {
				t.Fatalf("authorityProtectsLocalContainer(%q, %q)=%t, want %t", test.node, test.container, got, test.want)
			}
		})
	}
	if authorityProtectsLocalContainer(nil, "node-a", "physical-cold") {
		t.Fatal("nil authority protected local state")
	}
}

func TestForkPlacementDestinationPreflight(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "logical", BranchID: "remote", PhysicalContainer: "physical-remote", PhysicalBranch: "remote",
		OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{distributedSource: &distributedSourceRuntime{placements: placements}}
	info := ttcbundle.ContainerInfo{ID: "physical", DistributedFamilyID: "logical"}
	if err := backend.ensureForkPlacementDestinationsAvailable(info, []string{"new-a", "new-b"}); err != nil {
		t.Fatalf("available names rejected: %v", err)
	}
	if err := backend.ensureForkPlacementDestinationsAvailable(info, []string{"new-a", "remote"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("remote placement conflict error=%v, want already exists", err)
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
		state, err := processState(command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if state != 'T' {
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

func TestProvisionalTargetAuthorization(t *testing.T) {
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
	if !provisionalTargetAuthorized(info, "child", "node-b", "inc-b", load) {
		t.Fatal("current target transaction did not authorize its provisional runtime")
	}
	visible := info
	visible.DistributedForkImports = nil
	visible.Branches = []ttcbundle.BranchInfo{{
		ID: "child", NodeID: "node-b", State: "running", DistributedForkTransactionID: "tx-fork",
	}}
	if !provisionalTargetAuthorized(visible, "child", "node-b", "inc-b", load) {
		t.Fatal("visible target transaction did not authorize pre-placement runtime")
	}
	tx.State = cluster.TransactionPublished
	if !provisionalTargetAuthorized(info, "child", "node-b", "inc-b", load) {
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
			if provisionalTargetAuthorized(info, "child", "node-b", "inc-b", func(string) (*cluster.Transaction, error) { return &copy, nil }) {
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
		state, err := processState(command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if state == 'T' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guardian target was not stopped: state %c", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTerminateRetiredElasticRuntimeRoot(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	otherRoot := filepath.Join(t.TempDir(), "runtime")
	start := func(name, root string) *exec.Cmd {
		command := exec.Command("bash", "-c", "exec -a '"+name+"' bash -c 'while :; do read -t 1 || :; done' -- --root="+root)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	sentry := start("ttc-sentry[retired-test]", runtimeRoot)
	gofer := start("runsc-gofer", runtimeRoot)
	unrelated := start("runsc-gofer", otherRoot)
	defer func() {
		for _, command := range []*exec.Cmd{sentry, gofer, unrelated} {
			_ = command.Process.Signal(syscall.SIGCONT)
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		processes, err := elasticProcessesForRuntimeRoot(runtimeRoot, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(processes) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retired runtime helpers were not discoverable: %+v", processes)
		}
		time.Sleep(time.Millisecond)
	}
	if err := terminateRetiredElasticRuntimeRoot(runtimeRoot); err != nil {
		t.Fatal(err)
	}
	for _, command := range []*exec.Cmd{sentry, gofer} {
		state, err := processState(command.Process.Pid)
		if err == nil && state != 'Z' {
			t.Fatalf("retired runtime PID %d remains in state %c", command.Process.Pid, state)
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated runtime was terminated: %v", err)
	}
}

func TestTransactionPlacementOwnedByIgnoresHistoricalTargetNode(t *testing.T) {
	tx := cluster.Transaction{Identity: cluster.ForkIdentity{
		TargetNodeID:        "node-a",
		SourceContainer:     "family",
		SourceBranch:        "main",
		TargetContainer:     "family",
		TargetBranch:        "physical-child",
		TargetVisibleBranch: "child",
	}}
	state := cluster.NewAuthorityState()
	state.Placements["family@child"] = cluster.PlacementRecord{
		FamilyID: "family", BranchID: "child", OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning,
	}
	if transactionPlacementOwnedBy(tx, state, "node-a", "inc-a") {
		t.Fatal("historical transaction target was treated as current placement owner")
	}
	if !transactionPlacementOwnedBy(tx, state, "node-b", "inc-b") {
		t.Fatal("current placement owner was not recognized")
	}
	state.Placements["family@child"] = cluster.PlacementRecord{
		FamilyID: "family", BranchID: "child", State: cluster.PlacementUnplaced,
	}
	if transactionPlacementOwnedBy(tx, state, "node-b", "inc-b") {
		t.Fatal("unplaced branch was treated as a live graph-handoff target")
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

func TestLifecycleCommandOperandsSkipsOptions(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"restart", "--time", "3", "web@main", "web@child"}, []string{"web@main", "web@child"}},
		{[]string{"restart", "-t=0", "web@main"}, []string{"web@main"}},
		{[]string{"start", "-ai", "web@main"}, []string{"web@main"}},
		{[]string{"rehydrate", "web@main"}, []string{"web@main"}},
	}
	for _, test := range tests {
		got, err := lifecycleCommandOperands(test.args)
		if err != nil {
			t.Fatalf("lifecycleCommandOperands(%v): %v", test.args, err)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("lifecycleCommandOperands(%v) = %v, want %v", test.args, got, test.want)
		}
	}
}

// A family's removal merges the removals of branches on one node that share
// their flags into one command.
// In a cluster as on a standalone daemon, a removal of several targets
// reports each that does not exist, and removes the others still: here, none
// exists, and the removal fails having removed nothing.
func TestDistributedFamilyMutationReportsMissingTargets(t *testing.T) {
	root := t.TempDir()
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "placements"))
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := cluster.OpenStore(filepath.Join(root, "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{
		dataRoot: root, clusterPlacements: placements, clusterStore: transactions,
		clusterCapabilities: &cluster.Capabilities{NodeID: "node-a"},
	}
	handled, status, stdout, stderr, err := server.executeDistributedFamilyMutation(daemonRequest{Args: []string{"rm", "-f", "gone", "lost@main"}})
	if err != nil || !handled || status != 1 {
		t.Fatalf("handled=%t status=%d err=%v, want a handled failure", handled, status, err)
	}
	if len(stdout) != 0 {
		t.Errorf("stdout=%q, want nothing removed", stdout)
	}
	for _, name := range []string{"gone", "lost"} {
		if want := fmt.Sprintf("container %q does not exist", name); strings.Count(string(stderr), want) != 1 {
			t.Errorf("stderr=%q, want one %s", stderr, want)
		}
	}
}

func TestBranchRemovalParts(t *testing.T) {
	for _, test := range []struct {
		args           []string
		flags, targets []string
		ok             bool
	}{
		{[]string{"rm", "-f", "fam@b1"}, []string{"rm", "-f"}, []string{"fam@b1"}, true},
		{[]string{"rm", "fam@b1", "fam@b2"}, []string{"rm"}, []string{"fam@b1", "fam@b2"}, true},
		{[]string{"rm", "-f", "fam"}, nil, nil, false},
		{[]string{"rm", "-f"}, nil, nil, false},
		{[]string{"_remove-family-record", "id"}, nil, nil, false},
		{[]string{"stop", "fam@b1"}, nil, nil, false},
	} {
		flags, targets, ok := branchRemovalParts(test.args)
		if ok != test.ok || !slices.Equal(flags, test.flags) || !slices.Equal(targets, test.targets) {
			t.Errorf("branchRemovalParts(%q) = %q, %q, %t; want %q, %q, %t", test.args, flags, targets, ok, test.flags, test.targets, test.ok)
		}
	}
}

// Enforcing placements keeps the runtime of a branch its node is
// materializing, as a rehydration does, and of one it runs; not of one
// placed elsewhere, or with another incarnation, or unplaced.
func TestPlacementKeepsRuntime(t *testing.T) {
	for _, test := range []struct {
		placement cluster.PlacementRecord
		want      bool
	}{
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning}, true},
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementPaused}, true},
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementMigrating}, true},
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementMaterializing}, true},
		{cluster.PlacementRecord{OwnerNode: "node-b", OwnerIncarnation: "inc-b", State: cluster.PlacementRunning}, false},
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-old", State: cluster.PlacementRunning}, false},
		{cluster.PlacementRecord{State: cluster.PlacementUnplaced}, false},
		{cluster.PlacementRecord{OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementCorrupted}, false},
	} {
		placement := test.placement
		if got := placementKeepsRuntime(&placement, "node-a", "inc-a"); got != test.want {
			t.Errorf("placementKeepsRuntime(%+v) = %t, want %t", placement, got, test.want)
		}
	}
}

func TestPlacementAwaitsActivation(t *testing.T) {
	placement := cluster.PlacementRecord{FamilyID: "family", BranchID: "branch", OwnerNode: "n3", PhysicalContainer: "fork-1", PhysicalBranch: "fork-1"}
	tx := cluster.Transaction{
		Identity:                cluster.ForkIdentity{TransactionID: "fork-1", TargetNodeID: "n3", Migration: true},
		State:                   cluster.TransactionPublished,
		TargetPhysicalContainer: "fork-1",
		TargetPhysicalBranch:    "fork-1",
	}
	state := &cluster.AuthorityState{Transactions: map[string]cluster.Transaction{"fork-1": tx}}
	if !placementAwaitsActivation(state, placement, "n3") {
		t.Error("the target's placement before activation was not left to the migration")
	}
	if placementAwaitsActivation(state, placement, "n2") {
		t.Error("another node's reconciliation left the placement alone")
	}
	for name, change := range map[string]func(*cluster.Transaction){
		"activated": func(tx *cluster.Transaction) { tx.Activated = true },
		"canceled":  func(tx *cluster.Transaction) { tx.State = cluster.TransactionCanceled },
		"a fork":    func(tx *cluster.Transaction) { tx.Identity.Migration = false },
		"elsewhere": func(tx *cluster.Transaction) { tx.TargetPhysicalBranch = "fork-2" },
	} {
		changed := tx
		change(&changed)
		state.Transactions["fork-1"] = changed
		if placementAwaitsActivation(state, placement, "n3") {
			t.Errorf("%s: the placement was left to the migration", name)
		}
	}
	if placementAwaitsActivation(nil, placement, "n3") {
		t.Error("a node without the authority's state left the placement alone")
	}
}

// sentryRuns takes a process whose command line names the container's Sentry
// of the runtime root for one, and no other.
func TestSentryRunsChecksTheCommandLine(t *testing.T) {
	sentry := exec.Command("/bin/sh", "-c", "sleep 30", "--root=/runtime", "container-a")
	sentry.Args[0] = "ttc-sentry[a]"
	if err := sentry.Start(); err != nil {
		t.Skipf("starting a stand-in Sentry: %v", err)
	}
	defer func() {
		_ = sentry.Process.Kill()
		_ = sentry.Wait()
	}()
	pid := sentry.Process.Pid
	// The shell's command line is in place once it runs its script.
	deadline := time.Now().Add(5 * time.Second)
	for !sentryRuns(pid, "container-a", "/runtime") {
		if time.Now().After(deadline) {
			t.Fatal("the stand-in Sentry was not found")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sentryRuns(pid, "container-b", "/runtime") {
		t.Fatal("the Sentry of container-a was taken for container-b's")
	}
	if sentryRuns(pid, "container-a", "/other-runtime") {
		t.Fatal("the Sentry of one runtime root was taken for another's")
	}
	if sentryRuns(os.Getpid(), "container-a", "/runtime") {
		t.Fatal("a process that is no Sentry was taken for one")
	}
}
