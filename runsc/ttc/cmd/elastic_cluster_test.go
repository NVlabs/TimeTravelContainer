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
	"encoding/json"
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
	encoded, err := json.Marshal(ttcbundle.ContainerInfo{ID: "web", Name: "web", NodeID: "node-b", BundleDir: filepath.Join(containerDir, "bundle")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := (ttcbundle.Store{Root: root}).LoadContainer("web"); err != nil || info.NodeID != "node-b" {
		t.Fatalf("loading routing fixture=(%+v, %v)", info, err)
	}
	for _, args := range [][]string{
		{"exec", "-it", "web@main", "bash"},
		{"cp", "web@main:/tmp/result", "/tmp/result"},
		{"fork", "web@main", "child"},
	} {
		owner, err := daemonCommandOwner(root, args)
		if err != nil || owner != "node-b" {
			t.Fatalf("daemonCommandOwner(%v)=(%q, %v)", args, owner, err)
		}
	}
	if owner, err := daemonCommandOwner(root, []string{"images"}); err != nil || owner != "" {
		t.Fatalf("image command owner=(%q, %v)", owner, err)
	}
	if owner, claims, err := daemonCommandPlacement(root, []string{"wait", "--condition=durable", "web@main"}); err != nil || owner != "" || len(claims) != 0 {
		t.Fatalf("durable wait placement=(%q, %+v, %v)", owner, claims, err)
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
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
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
	owner, claims, err := daemonCommandPlacement(root, []string{"exec", "web@main", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if owner != "node-b" || len(claims) != 1 || claims[0].Generation != created.Generation || claims[0].OwnerIncarnation != "boot-b" {
		t.Fatalf("placement owner=%q claims=%+v", owner, claims)
	}
	if err := validateDaemonPlacementClaims(root, "node-b", claims); err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	if _, err := placements.BeginMigration("web", "main", "node-b", "boot-b", created.Generation, "move-one", "node-c", time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := validateDaemonPlacementClaims(root, "node-b", claims); err == nil {
		t.Fatal("stale pre-migration claim remained valid")
	}
	if _, _, err := daemonCommandPlacement(root, []string{"exec", "web@main", "true"}); err == nil {
		t.Fatal("new command was routed into an in-progress migration")
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
		Branches: []ttcbundle.BranchInfo{{ID: "main", Unplaced: true, State: "dehydrated", CreatedAt: time.Unix(1, 0).UTC()}},
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
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
	backend := &runtimeBackend{dataRoot: root, localRoot: local, nodeID: "node-a"}
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

	for _, test := range []struct {
		name   string
		mutate func(*cluster.Transaction)
	}{
		{"wrong incarnation", func(tx *cluster.Transaction) { tx.TargetIncarnation = "old" }},
		{"wrong node", func(tx *cluster.Transaction) { tx.Identity.TargetNodeID = "node-c" }},
		{"wrong branch", func(tx *cluster.Transaction) { tx.Identity.TargetVisibleBranch = "other" }},
		{"wrong family", func(tx *cluster.Transaction) { tx.Identity.TargetContainer = "other" }},
		{"canceled", func(tx *cluster.Transaction) { tx.State = cluster.TransactionCanceled }},
		{"published without placement", func(tx *cluster.Transaction) { tx.State = cluster.TransactionPublished }},
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
	command := exec.Command("bash", "-c", "exec -a 'ttc-sentry[guardian-test]' bash -c 'while :; do sleep 1; done' -- --root="+runtimeRoot)
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
