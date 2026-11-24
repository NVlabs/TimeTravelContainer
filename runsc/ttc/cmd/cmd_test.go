// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
)

func TestRemoteExecSessionRoundTrip(t *testing.T) {
	runningExit := uint32(17)
	exitedStatus := uint32(7 << 8)
	created := time.Unix(123, 456).UTC()
	source := []boot.ExecSessionInfo{{
		ID: "build", ContainerID: "source-runtime", PID: 42, ConsoleID: "console-build",
		Terminal: true, State: boot.ExecSessionRunning, CreatedAt: created, ExitStatus: &runningExit,
	}, {
		ID: "finished", ContainerID: "source-runtime", PID: 43, ConsoleID: "console-finished",
		State: boot.ExecSessionExited, CreatedAt: created.Add(time.Second), ExitStatus: &exitedStatus,
	}}
	transport := remoteExecSessions(source)
	if len(transport) != 2 || transport[0].ID != "build" || transport[1].ID != "finished" {
		t.Fatalf("transport sessions=%+v", transport)
	}
	restored := bootExecSessions(transport, "target-runtime")
	if len(restored) != 2 {
		t.Fatalf("restored sessions=%+v", restored)
	}
	got := restored[0]
	if got.ID != "build" || got.ContainerID != "target-runtime" || got.PID != 42 || got.ConsoleID != "console-build" || !got.Terminal || got.State != boot.ExecSessionRunning || !got.CreatedAt.Equal(created) || got.ExitStatus == nil || *got.ExitStatus != runningExit {
		t.Fatalf("restored session=%+v", got)
	}
	finished := restored[1]
	if finished.ID != "finished" || finished.ContainerID != "target-runtime" || finished.State != boot.ExecSessionExited || finished.ExitStatus == nil || *finished.ExitStatus != exitedStatus {
		t.Fatalf("restored exited session=%+v", finished)
	}
	*source[0].ExitStatus = 99
	if *transport[0].ExitStatus != 17 || *restored[0].ExitStatus != 17 {
		t.Fatal("exec-session exit status aliases source storage")
	}
}

func TestBranchRemovalRequiresForce(t *testing.T) {
	if !branchRemovalRequiresForce("running", false) || !branchRemovalRequiresForce("paused", false) {
		t.Fatal("live healthy branch did not require forced removal")
	}
	if branchRemovalRequiresForce("exited", false) {
		t.Fatal("exited branch required forced removal")
	}
	if branchRemovalRequiresForce("running", true) {
		t.Fatal("fenced corrupted branch retained stale force requirement")
	}
}

func TestDistributedConsoleLogRelay(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.log")
	newer := filepath.Join(dir, "newer.log")
	if err := os.WriteFile(older, []byte("older\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("newer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := newDistributedPageRelay(listener, nil, "", "")
	defer relay.Close()
	var token [16]byte
	copy(token[:], "console-log-test")
	openExtent := func(path string) remoteConsoleLogExtent {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		return remoteConsoleLogExtent{file: file, size: info.Size()}
	}
	if err := relay.ConfigureConsoleLogs(token, map[string][]remoteConsoleLogExtent{"build": {openExtent(older), openExtent(newer)}}); err != nil {
		t.Fatal(err)
	}
	// A running session may append after capture. The relay must expose exactly
	// the captured prefix, not the file's later length.
	file, err := os.OpenFile(newer, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("late\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "target", "build.log")
	if err := fetchRemoteConsoleLog(t.Context(), listener.Addr().String(), token, "build", uint64(len("older\nnewer\n")), destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if want := "older\nnewer\n"; string(got) != want {
		t.Fatalf("retained output=%q, want %q", got, want)
	}
	// Installation is idempotent after a lost Prepare response.
	if err := fetchRemoteConsoleLog(t.Context(), listener.Addr().String(), token, "build", uint64(len(got)), destination); err != nil {
		t.Fatalf("idempotent fetch: %v", err)
	}
	got, err = os.ReadFile(destination)
	if err != nil || string(got) != "older\nnewer\n" {
		t.Fatalf("idempotent retained output=%q, err=%v", got, err)
	}
	// Returning to a node can find an older disjoint segment of the same
	// session. It precedes the current source segment and must not be replaced.
	disjoint := filepath.Join(dir, "target", "disjoint.log")
	if err := os.WriteFile(disjoint, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fetchRemoteConsoleLog(t.Context(), listener.Addr().String(), token, "build", uint64(len("older\nnewer\n")), disjoint); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(disjoint); err != nil || string(got) != "previous\nolder\nnewer\n" {
		t.Fatalf("merged retained output=%q, err=%v", got, err)
	}
	// An existing prefix is already included by the source and must not be
	// duplicated.
	prefix := filepath.Join(dir, "target", "prefix.log")
	if err := os.WriteFile(prefix, []byte("older\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fetchRemoteConsoleLog(t.Context(), listener.Addr().String(), token, "build", uint64(len("older\nnewer\n")), prefix); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(prefix); err != nil || string(got) != "older\nnewer\n" {
		t.Fatalf("prefix retained output=%q, err=%v", got, err)
	}
}

func TestDistributedArtifactRelay(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := newDistributedPageRelay(listener, nil, "", "")
	defer relay.Close()
	var token [16]byte
	copy(token[:], "artifact-test")
	payload := bytes.Repeat([]byte("kernel-state\n"), 1<<12)
	if err := relay.ConfigureArtifacts(token, map[string][]byte{"metadata": payload}); err != nil {
		t.Fatal(err)
	}
	file, err := fetchRemoteArtifact(t.Context(), listener.Addr().String(), token, "metadata", uint64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("artifact bytes=%d, want %d, err=%v", len(got), len(payload), err)
	}
	if _, err := fetchRemoteArtifact(t.Context(), listener.Addr().String(), token, "metadata", uint64(len(payload))+1); err == nil {
		t.Fatal("accepted mismatched artifact size")
	}
	wrong := token
	wrong[0]++
	if _, err := fetchRemoteArtifact(t.Context(), listener.Addr().String(), wrong, "metadata", uint64(len(payload))); err == nil {
		t.Fatal("accepted wrong artifact token")
	}
}

func TestApplyColdFilesystemRestoreIDs(t *testing.T) {
	roots := []cluster.FilesystemRootDescriptor{
		{Source: "/run/ttc/rootfs-upper", RepositoryID: "cache-rootfs", RestoreID: "cache-rootfs"},
		{Source: "/run/ttc/volume-data", RepositoryID: "cache-data", RestoreID: "cache-data"},
	}
	cold := []coldFilesystemSnapshot{
		{Root: cluster.FilesystemRootDescriptor{Source: "/run/ttc/volume-data", RestoreID: "captured-data"}},
		{Root: cluster.FilesystemRootDescriptor{Source: "/run/ttc/rootfs-upper", RestoreID: "captured-rootfs"}},
	}
	if err := applyColdFilesystemRestoreIDs(roots, cold); err != nil {
		t.Fatal(err)
	}
	if got, want := roots[0].RestoreID, "captured-rootfs"; got != want {
		t.Fatalf("rootfs restore ID=%q, want %q", got, want)
	}
	if got, want := roots[1].RestoreID, "captured-data"; got != want {
		t.Fatalf("data restore ID=%q, want %q", got, want)
	}
	if err := applyColdFilesystemRestoreIDs(roots[:1], cold); err == nil || !strings.Contains(err.Error(), "unmatched") {
		t.Fatalf("unmatched cold filesystem error=%v", err)
	}
	duplicate := append(append([]coldFilesystemSnapshot(nil), cold...), cold[0])
	if err := applyColdFilesystemRestoreIDs(roots, duplicate); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Fatalf("duplicate cold filesystem error=%v", err)
	}
}

func TestApplyDehydratedFilesystemRestoreIDsAllowsPendingPublication(t *testing.T) {
	roots := []cluster.FilesystemRootDescriptor{{
		Source: "/run/ttc/rootfs-upper", RepositoryID: "local-rootfs", RestoreID: "local-rootfs",
	}}
	if err := applyDehydratedFilesystemRestoreIDs(roots, "", 8); err != nil {
		t.Fatal(err)
	}
	if got, want := roots[0].RestoreID, "local-rootfs"; got != want {
		t.Fatalf("pending-publication restore ID=%q, want %q", got, want)
	}
	// A branch dehydrated again names its earlier publication's manifest,
	// of another capture, until its own publication commits.
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	data, err := json.Marshal(coldBranchManifest{
		Version: 5, Container: "web", Branch: "main", PublicationID: "publication-earlier", Kind: "dehydrate",
		MemorySnapshot: 5, ControlRoot: ttcsnapshot.ObjectID{Snapshot: 5, Ordinal: 2, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "/run/ttc/rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef", RestoreID: "earlier-rootfs",
			BranchID: "main", Root: ttcsnapshot.ObjectID{Snapshot: 5, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String(), NextInode: 2,
		}, Origins: []uint64{5}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := applyDehydratedFilesystemRestoreIDs(roots, manifestPath, 8); err != nil {
		t.Fatal(err)
	}
	if got, want := roots[0].RestoreID, "local-rootfs"; got != want {
		t.Fatalf("restore ID beside an earlier publication's manifest=%q, want %q", got, want)
	}
	// The manifest of the capture itself names its identities.
	if err := applyDehydratedFilesystemRestoreIDs(roots, manifestPath, 5); err != nil {
		t.Fatal(err)
	}
	if got, want := roots[0].RestoreID, "earlier-rootfs"; got != want {
		t.Fatalf("restore ID from the capture's own manifest=%q, want %q", got, want)
	}
}

// A restore of a dehydration capture, a dehydrated fork's child copying its
// source's, maps this node's repositories to the identities the capture saved
// its filesystems under, which the capture's publication recorded. The
// node's repository map names one identity per repository, of the latest
// restore here, which other restores rewrite: a child restored after its
// source was materialized here found another restore's, and its filesystem
// "was not provided". A capture made since its publication was made here, of
// the repositories as the map has them.
func TestDehydrationRestoreIDsAreTheCapturePublications(t *testing.T) {
	dataRoot := t.TempDir()
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("family")
	if err != nil {
		t.Fatal(err)
	}
	current, path := repository.Info().ID, repository.Path()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(dataRoot, "containers", "family")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	const otherRestore = "fedcba9876543210fedcba9876543210"
	repositories := fmt.Sprintf(`[{"source":"/upper","path":%q,"branch":"main","copy_mode":"full-copy","private":true,"restore_id":%q}]`, path, otherRestore)
	config, err := json.Marshal(map[string]any{"annotations": map[string]string{specutils.AnnotationTTCVolumeRepositories: repositories}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "family", Name: "family", BundleDir: bundleDir,
		Branches: []ttcbundle.BranchInfo{{ID: "source", State: "dehydrated", StorageState: "shared"}},
	}); err != nil {
		t.Fatal(err)
	}
	const captured = "0123456789abcdef0123456789abcdef"
	root := ttcsnapshot.ObjectID{Snapshot: 3, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	manifest := coldBranchManifest{
		Version: 5, Container: "family", Branch: "source", PublicationID: "publication-source", Kind: "dehydrate",
		MemorySnapshot: 8, ControlRoot: ttcsnapshot.ObjectID{Snapshot: 8, Ordinal: 2, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "/upper", RepositoryID: captured, RestoreID: captured,
			BranchID: "main", Root: root.String(), NextInode: 2,
		}, Origins: []uint64{3}}},
	}
	manifestPath := filepath.Join(dataRoot, "cold", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{dataRoot: dataRoot}
	info, err := b.containerStore().LoadContainer("family")
	if err != nil {
		t.Fatal(err)
	}
	source := ttcbundle.BranchInfo{ID: "source", StorageManifest: manifestPath, SnapshotID: 8}
	ids, err := b.dehydrationRestoreIDs(info, source)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids[current]; got != captured {
		t.Fatalf("a published capture restores repository %s as %q, want its publication's %q", current, got, captured)
	}
	source.SnapshotID = 9
	if ids, err = b.dehydrationRestoreIDs(info, source); err != nil {
		t.Fatal(err)
	}
	if got := ids[current]; got != otherRestore {
		t.Fatalf("a capture made since its publication restores repository %s as %q, want the map's %q", current, got, otherRestore)
	}
}

func writeCurrentBundleFixture(path string, input ttcbundle.ContainerInfo) error {
	info := input
	info.Branches = append([]ttcbundle.BranchInfo(nil), input.Branches...)
	if info.ID == "" {
		info.ID = filepath.Base(filepath.Dir(path))
	}
	if info.State == "" {
		info.State = "created"
	}
	if len(info.Branches) == 0 {
		info.Branches = []ttcbundle.BranchInfo{{ID: "main", CreatedAt: info.CreatedAt}}
	}
	for i := range info.Branches {
		branch := &info.Branches[i]
		if branch.FilesystemBranch == "" {
			branch.FilesystemBranch = branch.ID
		}
		if branch.State == "" {
			branch.State = info.State
		}
		if branch.StorageState == "" {
			branch.StorageState = "local"
		}
	}
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		info.LiveForkEligible = true
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		return err
	}
	// Production metadata writers replace container.json atomically. Tests that
	// exercise publication/read ordering must preserve that contract; truncating
	// the file in place creates a partial-JSON race that cannot occur in TTC.
	temp, err := os.CreateTemp(filepath.Dir(path), ".container.json.fixture-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func TestLogicalFamilyID(t *testing.T) {
	for _, tc := range []struct {
		name string
		info ttcbundle.ContainerInfo
		want string
	}{
		{name: "local physical identity", info: ttcbundle.ContainerInfo{ID: "physical"}, want: "physical"},
		{name: "migrated logical identity", info: ttcbundle.ContainerInfo{ID: "physical", DistributedFamilyID: "family"}, want: "family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := logicalFamilyID(tc.info); got != tc.want {
				t.Fatalf("logicalFamilyID(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

func TestPortIngressPersistencePolicy(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		backend *runtimeBackend
		want    bool
	}{
		{name: "no manager", backend: &runtimeBackend{}},
		{name: "standalone manager", backend: &runtimeBackend{portIngress: newPortIngressManager(&daemonServer{})}},
		{name: "cluster manager", backend: &runtimeBackend{portIngress: newPortIngressManager(&daemonServer{clusterPlacements: placements})}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.backend.usesEphemeralPortIngress(); got != tc.want {
				t.Fatalf("usesEphemeralPortIngress()=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestDistributedPortIngressUsesPlacementCatalogWithoutLocalBundle(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", FamilyName: "web", BranchID: "main",
		PhysicalContainer: "physical", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := uint16(probe.Addr().(*net.TCPAddr).Port)
	probe.Close()
	server := &daemonServer{clusterPlacements: placements}
	manager := newPortIngressManager(server)
	server.portIngress = manager
	defer manager.Close()
	binding := ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 8080, Protocol: "tcp"}
	if err := manager.Publish(selector{container: "web", branch: "main"}, binding); err != nil {
		t.Fatal(err)
	}
	listed, err := manager.List("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].HostPort != hostPort {
		t.Fatalf("listed bindings=%+v", listed)
	}
	if err := manager.Unpublish(selector{container: "web", branch: "main"}, binding); err != nil {
		t.Fatal(err)
	}
}

func TestDistributedPortIngressRejectsKnownNonTTCNetwork(t *testing.T) {
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
		ID: "family", Name: "web", BundleDir: bundleDir, NetworkMode: "none",
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}},
	}); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", FamilyName: "web", BranchID: "main",
		PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{dataRoot: root, clusterPlacements: placements}
	manager := newPortIngressManager(server)
	server.portIngress = manager
	defer manager.Close()
	err = manager.Publish(selector{container: "web", branch: "main"}, ttcbundle.PortBinding{
		HostIP: "127.0.0.1", HostPort: 1, ContainerPort: 8080, Protocol: "tcp",
	})
	if err == nil || !strings.Contains(err.Error(), `network mode "none", not ttc`) {
		t.Fatalf("Publish() error=%v, want network-mode rejection", err)
	}
}

func TestDistributedPortIngressCanPrecedeInitialPlacement(t *testing.T) {
	placements, err := cluster.OpenPlacementStore(filepath.Join(t.TempDir(), "placements"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := uint16(probe.Addr().(*net.TCPAddr).Port)
	probe.Close()
	server := &daemonServer{clusterPlacements: placements}
	manager := newPortIngressManager(server)
	server.portIngress = manager
	defer manager.Close()
	info := ttcbundle.ContainerInfo{
		ID: "family", Name: "web", NetworkMode: "ttc",
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "creating"}},
	}
	binding := ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 8080, Protocol: "tcp"}
	if err := manager.PublishPrepared(info, "main", binding); err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "family", FamilyName: "web", BranchID: "main",
		PhysicalContainer: "family", PhysicalBranch: "main",
		OwnerNode: "node-a", OwnerIncarnation: "boot-a", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	listed, err := manager.List("family")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].HostPort != hostPort {
		t.Fatalf("listed pre-placement bindings=%+v", listed)
	}
	if err := manager.Unpublish(selector{container: "family", branch: "main"}, binding); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRunIngressKeepsListenerAtEntryDaemon(t *testing.T) {
	args := []string{"run", "-dit", "--node", "node-b", "--name", "web", "--network=ttc", "-p", "127.0.0.1:18080:8080", "--publish=18081:8081", "image", "serve", "-p", "guest-argument"}
	clean, prepared, err := prepareRunIngress(args)
	if err != nil {
		t.Fatal(err)
	}
	if prepared == nil || prepared.target != (selector{container: "web", branch: "main"}) {
		t.Fatalf("prepared=%+v", prepared)
	}
	if len(prepared.bindings) != 2 || prepared.bindings[0].HostPort != 18080 || prepared.bindings[1].HostPort != 18081 {
		t.Fatalf("bindings=%+v", prepared.bindings)
	}
	want := []string{"run", "-dit", "--node", "node-b", "--name", "web", "--network=ttc", "image", "serve", "-p", "guest-argument"}
	if !slices.Equal(clean, want) {
		t.Fatalf("clean=%q, want %q", clean, want)
	}
}

func TestPrepareRunIngressMakesGeneratedNameExplicit(t *testing.T) {
	clean, prepared, err := prepareRunIngress([]string{"run", "-d", "--network=ttc", "-p=18080:80", "image"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared == nil || !strings.HasPrefix(prepared.target.container, "ttc-") {
		t.Fatalf("prepared=%+v", prepared)
	}
	if len(clean) < 4 || clean[1] != "--name" || clean[2] != prepared.target.container {
		t.Fatalf("clean=%q does not expose generated name %q", clean, prepared.target.container)
	}
}

func TestPrepareRunIngressRejectsNonTTCNetwork(t *testing.T) {
	if _, _, err := prepareRunIngress([]string{"run", "-d", "-p", "18080:80", "image"}); err == nil || !strings.Contains(err.Error(), "--network=ttc") {
		t.Fatalf("prepareRunIngress() error=%v, want network rejection", err)
	}
}

func TestDaemonStatusWriterObservesSplitFrames(t *testing.T) {
	var framed bytes.Buffer
	var mu sync.Mutex
	if err := writeDaemonFrame(&framed, &mu, daemonFrameStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonStatus(&framed, &mu, 7); err != nil {
		t.Fatal(err)
	}
	var forwarded bytes.Buffer
	observer := &daemonStatusWriter{writer: &forwarded}
	data := framed.Bytes()
	for len(data) != 0 {
		n := min(3, len(data))
		if _, err := observer.Write(data[:n]); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	if !bytes.Equal(forwarded.Bytes(), framed.Bytes()) {
		t.Fatal("observer changed the daemon frame stream")
	}
	if !observer.statusSeen || observer.status != 7 {
		t.Fatalf("statusSeen=%t status=%d", observer.statusSeen, observer.status)
	}
}

func TestDistributedPageRelayAcceptsPastIdleFilesystemStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := newDistributedPageRelay(listener, nil, "source", "session")
	defer relay.Close()
	idle, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	active, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if _, err := active.Write([]byte("TTCF")); err != nil {
		t.Fatal(err)
	}
	if err := active.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := active.Read(one[:]); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("second stream was not classified while first stayed idle: %v", err)
	}
}

func TestDockerCompatibleHelpIsLocalAndSuccessful(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ttcd.sock")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"flag command", []string{"run", "--help"}, "Usage of run:"},
		{"container namespace", []string{"container", "--help"}, "Usage: ttc container COMMAND"},
		{"container subcommand", []string{"container", "run", "--help"}, "Usage of run:"},
		{"pause", []string{"pause", "--help"}, "Usage: ttc pause"},
		{"unpause", []string{"unpause", "--help"}, "Usage: ttc unpause"},
		{"top", []string{"top", "--help"}, "Usage: ttc top"},
		{"cp", []string{"cp", "--help"}, "Usage: ttc cp"},
		{"events", []string{"events", "--help"}, "Usage: ttc events"},
		{"image inspect", []string{"image", "inspect", "--help"}, "Usage: ttc image inspect"},
		{"image load", []string{"image", "load", "--help"}, "Usage: ttc image load"},
		{"image save", []string{"image", "save", "--help"}, "Usage: ttc image save"},
		{"commit", []string{"commit", "--help"}, "Usage: ttc commit"},
		{"exec wait", []string{"exec", "wait", "--help"}, "Usage: ttc exec wait"},
		{"daemon", []string{"daemon", "--help"}, "Usage: ttc daemon"},
		{"volume", []string{"volume", "--help"}, "Usage: ttc volume"},
		{"volume create", []string{"volume", "create", "--help"}, "Usage: ttc volume create"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"--daemon-socket", socket}, test.args...)
			if status := Run(args, strings.NewReader(""), &stdout, &stderr); status != 0 {
				t.Fatalf("status=%d, stdout=%q, stderr=%q", status, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout=%q, want %q", stdout.String(), test.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if _, err := os.Stat(socket); !os.IsNotExist(err) {
				t.Fatalf("help created daemon socket: %v", err)
			}
		})
	}
}

func TestPortLockHolderUsesRecordedPID(t *testing.T) {
	const (
		container = "port-owner-test"
		branch    = "child"
		endpoint  = "127.0.0.1:32123:8080"
	)
	lockPath := filepath.Join(t.TempDir(), "port.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestPortLockHolderHelper$", "--", "port-forward", "--branch="+branch, container, endpoint)
	command.Env = append(os.Environ(), "TTC_PORT_LOCK_HELPER="+lockPath)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil {
		t.Fatalf("waiting for helper: %v", err)
	}
	if string(ready) != "ready\n" {
		t.Fatalf("helper readiness = %q", ready)
	}

	pid, err := portLockHolder(lockPath, container, branch, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if pid != command.Process.Pid {
		t.Fatalf("portLockHolder returned PID %d, want %d", pid, command.Process.Pid)
	}
}

func TestPortLockHolderHelper(t *testing.T) {
	lockPath := os.Getenv("TTC_PORT_LOCK_HELPER")
	if lockPath == "" {
		return
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := recordPortSidecarPID(lockFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("ready\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestQuarantineCommandsAddressBranchState(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"quarantine", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("quarantine status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.quarantineTarget, (selector{container: "web", branch: "child"}); got != want || !b.quarantineEnabled {
		t.Fatalf("quarantine call=(%v, %t), want (%v, true)", got, b.quarantineEnabled, want)
	}
	if got, want := stdout.String(), "web@child\n"; got != want {
		t.Fatalf("quarantine output=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"unquarantine", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("unquarantine status=%d, stderr=%q", status, stderr.String())
	}
	if b.quarantineTarget != (selector{container: "web", branch: "child"}) || b.quarantineEnabled {
		t.Fatalf("unquarantine call=(%v, %t)", b.quarantineTarget, b.quarantineEnabled)
	}
}

func TestMigrateCommand(t *testing.T) {
	backend := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"migrate", "--node", "node-b", "web@main"}, &stdout, &stderr, backend); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, stderr.String())
	}
	if backend.migrateTarget.String() != "web@main" || backend.migrateNode != "node-b" || stdout.String() != "web@main\n" {
		t.Fatalf("migration=(%s, %q) stdout=%q", backend.migrateTarget, backend.migrateNode, stdout.String())
	}
}

func TestTTYStatRoutesNamedSessionAndJSON(t *testing.T) {
	backend := &fakeBackend{ttyObservation: kernel.InternalConsoleObservation{
		Version: 1, InputPendingBytes: 7, Canonical: true, ForegroundPGID: 42,
	}}
	var stdout bytes.Buffer
	if err := runTTY([]string{"stat", "demo@main", "--session", "build1", "--json"}, &stdout, backend); err != nil {
		t.Fatal(err)
	}
	if backend.ttyTarget != (selector{container: "demo", branch: "main"}) || backend.ttySession != "build1" {
		t.Fatalf("tty route target=%v session=%q", backend.ttyTarget, backend.ttySession)
	}
	var got kernel.InternalConsoleObservation
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.InputPendingBytes != 7 || !got.Canonical || got.ForegroundPGID != 42 {
		t.Fatalf("tty observation=%+v", got)
	}
}

func TestTTYStatHumanOutputIncludesCausalFields(t *testing.T) {
	backend := &fakeBackend{ttyObservation: kernel.InternalConsoleObservation{
		Version: 1, SessionEpoch: 9, InputConsumedBytes: 17,
		TermiosGeneration: 2, ForegroundPGIDGeneration: 3,
		InputFlushGeneration: 4, ReplicaOpenCount: 5,
	}}
	var stdout bytes.Buffer
	if err := runTTY([]string{"stat", "demo@main", "--session", "build1"}, &stdout, backend); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"session_epoch=9", "input_consumed=17", "termios_generation=2",
		"foreground_generation=3", "input_flush_generation=4", "replica_open_count=5",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("human observation %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestStatsJSONIncludesFamilyTotals(t *testing.T) {
	backend := &fakeBackend{resourceStatsResult: FamilyResourceStats{
		Container: "demo", ResidentMemoryUniqueBytes: 4096, FilesystemUniqueBytes: 8192,
		ImageCacheBytes: 16384, ImageCacheTotalBytes: 32768,
		SnapshotGraph: &SnapshotGraphResourceStats{Sequence: 7, Objects: 11, FamilyReachableObjects: 5, PendingReferenceOperations: 1},
		Branches: []BranchResourceStats{{
			Target: "demo@main", State: "running", NodeID: "node-a", Quarantined: true,
			SnapshotID: 3, MemoryRoots: []string{"memory/root"}, MemoryRootObjects: []string{"object-3"}, FilesystemRoot: "filesystem-root",
			ResidentMemory: ResourceBytes{PrivateBytes: 4096, TotalBytes: 4096},
		}},
	}}
	var stdout bytes.Buffer
	if err := runStats([]string{"--json", "--no-stream", "demo@main"}, &stdout, io.Discard, backend); err != nil {
		t.Fatal(err)
	}
	if backend.resourceStatsTarget != (selector{container: "demo", branch: "main"}) {
		t.Fatalf("stats target=%v", backend.resourceStatsTarget)
	}
	var got FamilyResourceStats
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResidentMemoryUniqueBytes != 4096 || got.FilesystemUniqueBytes != 8192 || got.ImageCacheBytes != 16384 || got.ImageCacheTotalBytes != 32768 || len(got.Branches) != 1 ||
		got.SnapshotGraph == nil || got.SnapshotGraph.Sequence != 7 || got.SnapshotGraph.FamilyReachableObjects != 5 || got.Branches[0].SnapshotID != 3 || got.Branches[0].NodeID != "node-a" || !got.Branches[0].Quarantined || got.Branches[0].FilesystemRoot != "filesystem-root" {
		t.Fatalf("stats=%+v", got)
	}
}

func TestStatsHumanOutputUsesPrintablePadding(t *testing.T) {
	stats := FamilyResourceStats{Container: "demo", Branches: []BranchResourceStats{{
		Target: "demo@main", State: "running", StorageState: "local",
	}}}
	var stdout bytes.Buffer
	if err := writeResourceStats(&stdout, stats, false); err != nil {
		t.Fatal(err)
	}
	for _, b := range stdout.Bytes() {
		if b < 0x20 && b != '\n' && b != '\t' {
			t.Fatalf("human stats contains control byte %#x: %q", b, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "demo@main") {
		t.Fatalf("human stats=%q", stdout.String())
	}
}

func TestStatsHumanOutputShowsHugePages(t *testing.T) {
	stats := FamilyResourceStats{Container: "demo", ApplicationMappedBytes: 400_000_000, ApplicationHugePageBytes: 300_000_000}
	var stdout bytes.Buffer
	if err := writeResourceStats(&stdout, stats, false); err != nil {
		t.Fatal(err)
	}
	if want := "application memory in huge pages: 300MB of 400MB (75%)\n"; !strings.HasSuffix(stdout.String(), want) {
		t.Fatalf("human stats=%q, want it to end %q", stdout.String(), want)
	}
	stdout.Reset()
	stats.ApplicationMappedBytes, stats.ApplicationHugePageBytes = 0, 0
	if err := writeResourceStats(&stdout, stats, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "huge pages") {
		t.Fatalf("human stats of a family running nowhere here=%q", stdout.String())
	}
}

func TestSmapsRollupOfThisProcess(t *testing.T) {
	rss, huge, err := smapsRollup(os.Getpid())
	if err != nil {
		t.Skipf("no smaps_rollup: %v", err)
	}
	if rss == 0 || huge > rss {
		t.Fatalf("rss=%d huge=%d", rss, huge)
	}
	if children := processChildren(); !slices.Contains(children[os.Getppid()], os.Getpid()) {
		t.Fatalf("this process is not among its parent's children: %v", children[os.Getppid()])
	}
}

func TestParseSelector(t *testing.T) {
	tests := []struct {
		input string
		want  selector
		err   bool
	}{
		{input: "web", err: true},
		{input: "web@test-a", want: selector{container: "web", branch: "test-a"}},
		{input: "", err: true},
		{input: "@child", err: true},
		{input: "web@", err: true},
		{input: "web@child@again", err: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseSelector(test.input)
			if (err != nil) != test.err {
				t.Fatalf("parseSelector(%q) error: %v, want error=%t", test.input, err, test.err)
			}
			if got != test.want {
				t.Errorf("parseSelector(%q) = %+v, want %+v", test.input, got, test.want)
			}
		})
	}
}

func TestParseSelectorOrMain(t *testing.T) {
	tests := []struct {
		input string
		want  selector
		err   bool
	}{
		{input: "web", want: selector{container: "web", branch: defaultBranchID}},
		{input: strings.Repeat("a", 64), want: selector{container: strings.Repeat("a", 64), branch: defaultBranchID}},
		{input: "web@child", want: selector{container: "web", branch: "child"}},
		{input: "", err: true},
		{input: "web@", err: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseSelectorOrMain(test.input)
			if (err != nil) != test.err {
				t.Fatalf("parseSelectorOrMain(%q) error: %v, want error=%t", test.input, err, test.err)
			}
			if got != test.want {
				t.Errorf("parseSelectorOrMain(%q) = %+v, want %+v", test.input, got, test.want)
			}
		})
	}
}

func TestPortCommandsAreBranchAware(t *testing.T) {
	b := &fakeBackend{portList: []ttcbundle.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", BranchID: "main"},
		{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 80, Protocol: "tcp", BranchID: "child"},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"port", "publish", "web@child", "127.0.0.1:8082:8080"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port publish status=%d, stderr=%q", status, stderr.String())
	}
	if b.publishedTarget != (selector{container: "web", branch: "child"}) || b.publishedBinding != (ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: 8082, ContainerPort: 8080, Protocol: "tcp", BranchID: "child"}) {
		t.Errorf("publish=(%+v, %+v)", b.publishedTarget, b.publishedBinding)
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "ls", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port ls status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), "web@main") || strings.Contains(stdout.String(), "web@child") {
		t.Errorf("port ls output=%q", stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "ls", "--json", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port ls child status=%d, stderr=%q", status, stderr.String())
	}
	if strings.Contains(stdout.String(), `"branch_id": "main"`) || !strings.Contains(stdout.String(), `"branch_id": "child"`) {
		t.Errorf("filtered port JSON=%q", stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "unpublish", "web@child", "127.0.0.1:8081:80/tcp"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port unpublish status=%d, stderr=%q", status, stderr.String())
	}
	if b.unpublishedTarget != (selector{container: "web", branch: "child"}) || b.unpublishedBinding.Branch() != "child" || b.unpublishedBinding.HostPort != 8081 {
		t.Errorf("unpublish=(%+v, %+v)", b.unpublishedTarget, b.unpublishedBinding)
	}
}

func TestParsePublishedPortsAssignsInitialBranch(t *testing.T) {
	bindings, err := parsePublishedPorts([]string{"127.0.0.1:8080:80"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].BranchID != "main" {
		t.Fatalf("bindings = %+v, want one main-branch binding", bindings)
	}
}

func TestWriteTree(t *testing.T) {
	created := time.Date(2026, 7, 17, 8, 9, 10, 123, time.UTC)
	families := []Family{{
		ID:    "web",
		State: "running",
		Branches: []Branch{
			{ID: "retry", ParentID: "test-a", State: "running", ForkTotalNS: 2_000_000, SourceDowntimeNS: 1_000_000},
			{ID: "test-b", ParentID: "main", State: "paused"},
			{ID: "main", State: "running", CreatedAt: created, InitPID: 1, InitState: "running", MemorySharing: "cow"},
			{ID: "test-a", ParentID: "main", State: "paused"},
		},
	}}
	var out bytes.Buffer
	writeTree(&out, families)
	want := "web (running)\n" +
		"└── main running pid1=running(1) memory=cow created=2026-07-17T08:09:10.000000123Z\n" +
		"    ├── test-a paused\n" +
		"    │   └── retry running fork=2ms downtime=1ms\n" +
		"    └── test-b paused\n"
	if got := out.String(); got != want {
		t.Errorf("writeTree output:\n%s\nwant:\n%s", got, want)
	}
}

func TestEnrichPersistentBranchDiagnosticsAcrossNodes(t *testing.T) {
	family := Family{Branches: []Branch{
		{ID: "a1", NodeID: "node-a", State: "running"},
		{ID: "a2", NodeID: "node-a", State: "paused"},
		{ID: "b1", NodeID: "node-b", State: "running"},
		{ID: "cold", State: "dehydrated"},
		{ID: "done", State: "exited"},
		{ID: "observed", NodeID: "node-b", State: "running", InitPID: 7, InitState: "exited", MemorySharing: "cow"},
	}}
	enrichPersistentBranchDiagnostics(&family)
	byID := make(map[string]Branch)
	for _, branch := range family.Branches {
		byID[branch.ID] = branch
	}
	for _, id := range []string{"a1", "a2"} {
		if got := byID[id]; got.InitPID != 1 || got.InitState != got.State || got.MemorySharing != "cow" {
			t.Errorf("%s diagnostics=%+v", id, got)
		}
	}
	if got := byID["b1"]; got.InitPID != 1 || got.InitState != "running" || got.MemorySharing != "cow" {
		// observed is another live Kernel on node-b even though its runtime
		// supplied diagnostics must remain untouched below.
		t.Errorf("b1 diagnostics=%+v", got)
	}
	if got := byID["cold"]; got.InitState != "unknown" || got.MemorySharing != "unknown" {
		t.Errorf("cold diagnostics=%+v", got)
	}
	if got := byID["done"]; got.InitState != "exited" || got.MemorySharing != "released" {
		t.Errorf("done diagnostics=%+v", got)
	}
	if got := byID["observed"]; got.InitPID != 7 || got.InitState != "exited" || got.MemorySharing != "cow" {
		t.Errorf("runtime diagnostics were overwritten: %+v", got)
	}
}

func TestListJSONIncludesBranchParentage(t *testing.T) {
	b := &fakeBackend{families: []Family{{
		ID: "web", State: "running",
		Branches: []Branch{
			{ID: "main", State: "running"},
			{ID: "child", ParentID: "main", State: "paused"},
		},
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ls", "--json"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ls --json status=%d, stderr=%q", status, stderr.String())
	}
	var families []struct {
		Branches []map[string]any `json:"branches"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &families); err != nil {
		t.Fatalf("decoding ls JSON: %v", err)
	}
	if got := families[0].Branches[0]["parent"]; got != nil {
		t.Errorf("root parent=%v, want omitted", got)
	}
	child := families[0].Branches[1]
	if child["parent"] != "main" {
		t.Errorf("child parentage=%v, want parent main", child)
	}
	if _, ok := child["parent_id"]; ok {
		t.Errorf("child parentage=%v, parent_id must not be emitted", child)
	}
}

func TestStoppedStateIsExposedAsExited(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		ID: "legacy", Name: "legacy", State: "stopped",
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "stopped"}},
	}
	inspected := buildInspectInfo(container, nil, nil, "")
	if got := inspected.State.Status; got != "exited" {
		t.Fatalf("inspect state=%q, want exited", got)
	}
	if got := inspected.TTC.Container.State; got != "exited" {
		t.Fatalf("persistent container state=%q, want exited", got)
	}
	if got := inspected.TTC.Container.Branches[0].State; got != "exited" {
		t.Fatalf("persistent branch state=%q, want exited", got)
	}
}

func TestPsFiltersAndUsesQualifiedSelectors(t *testing.T) {
	b := &fakeBackend{families: []Family{
		{ID: "web", State: "running", Branches: []Branch{
			{ID: "main", State: "running"},
			{ID: "child", ParentID: "main", State: "paused"},
			{ID: "old", ParentID: "main", State: "exited"},
		}},
		{ID: "prepared", State: "created", Branches: []Branch{{ID: "main", State: "created"}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ps", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\n"; got != want {
		t.Errorf("ps --quiet=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"ps", "--all", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --all status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\nweb@old\nprepared@main\n"; got != want {
		t.Errorf("ps --all --quiet=%q, want %q", got, want)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"ps", "-aq"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps -aq status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\nweb@old\nprepared@main\n"; got != want {
		t.Errorf("ps -aq=%q, want %q", got, want)
	}
}

func TestPsSizeUsesBranchAccounting(t *testing.T) {
	b := &fakeBackend{
		families: []Family{
			{Name: "web", ImageSize: 1000000, Branches: []Branch{
				{ID: "main", State: "running"},
				{ID: "child", State: "paused"},
				{ID: "old", State: "exited"},
			}},
			{Name: "prepared", ImageSize: 2000000, Branches: []Branch{{ID: "main", State: "created"}}},
		},
		rootfsSizes: map[selector]uint64{
			{container: "web", branch: "main"}:  4096,
			{container: "web", branch: "child"}: 8192,
		},
	}
	var stdout, stderr bytes.Buffer
	args := []string{"ps", "--all", "--size", "--format", "{{.Names}}|{{.Size}}|{{.SizeRW}}|{{.SizeRootFS}}"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --size status=%d, stderr=%q", status, stderr.String())
	}
	want := "web@main|4.1kB (virtual 1MB)|4096|1004096\n" +
		"web@child|8.19kB (virtual 1.01MB)|8192|1008192\n" +
		"web@old|- (virtual 1MB)|0|1000000\n" +
		"prepared@main|0B (virtual 2MB)|0|2000000\n"
	if got := stdout.String(); got != want {
		t.Errorf("ps --size=%q, want %q", got, want)
	}
	wantCalls := []selector{{container: "web", branch: "main"}, {container: "web", branch: "child"}}
	if !reflect.DeepEqual(b.sizeCalls, wantCalls) {
		t.Errorf("size calls=%+v, want %+v", b.sizeCalls, wantCalls)
	}

	b.sizeCalls = nil
	stdout.Reset()
	if status := runCommand([]string{"ps", "--size", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --size --quiet status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.sizeCalls) != 0 {
		t.Errorf("quiet ps performed invisible size calls: %+v", b.sizeCalls)
	}
}

func TestPsDockerFiltersLatestAndFormatting(t *testing.T) {
	base := time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC)
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b := &fakeBackend{families: []Family{
		{
			ID: "web", Image: "docker.io/library/alpine:3.20", Digest: digest,
			CreatedAt: base,
			Branches: []Branch{
				{ID: "main", State: "running", CreatedAt: base},
				{ID: "child", ParentID: "main", State: "paused", CreatedAt: base.Add(2 * time.Hour)},
				{ID: "old", ParentID: "main", State: "exited", CreatedAt: base.Add(time.Hour)},
			},
		},
		{
			ID: "newest", Image: "docker.io/library/busybox:1.36",
			CreatedAt: base.Add(3 * time.Hour),
			Branches:  []Branch{{ID: "main", State: "created", CreatedAt: base.Add(3 * time.Hour)}},
		},
	}}
	var stdout, stderr bytes.Buffer
	args := []string{
		"ps", "--all",
		"--filter", "status=paused",
		"--filter", "ancestor=alpine:3.20",
		"--filter", "name=web@child",
		"--format", "{{.ID}}|{{.Image}}|{{.State}}|{{.ImageID}}",
	}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("filtered ps status=%d, stderr=%q", status, stderr.String())
	}
	want := "web@child|alpine:3.20|paused|0123456789ab\n"
	if got := stdout.String(); got != want {
		t.Errorf("filtered ps=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"ps", "--latest", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --latest status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "newest@main\n"; got != want {
		t.Errorf("ps --latest=%q, want %q", got, want)
	}

	stdout.Reset()
	if status := runCommand([]string{"ps", "--last", "2", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --last status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "newest@main\nweb@child\n"; got != want {
		t.Errorf("ps --last 2=%q, want %q", got, want)
	}

	stdout.Reset()
	if status := runCommand([]string{"ps", "--all", "--filter", "branch=child", "--format", "table {{.Names}}\t{{.Status}}"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted ps status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "NAMES\tSTATUS\nweb@child\tPaused\n"; got != want {
		t.Errorf("formatted ps=%q, want %q", got, want)
	}
}

func TestPsJSONAndNoTruncImageID(t *testing.T) {
	containerID := strings.Repeat("d", 64)
	created := time.Date(2026, time.July, 17, 1, 2, 3, 0, time.UTC)
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := &fakeBackend{families: []Family{{
		ID: containerID, Name: "demo", Image: "docker.io/example/demo:v1", Digest: digest, CreatedAt: created,
		Branches: []Branch{{ID: "main", State: "running", CreatedAt: created}},
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ps", "--format", "json", "--no-trunc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps json status=%d, stderr=%q", status, stderr.String())
	}
	var row map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &row); err != nil {
		t.Fatalf("decoding ps JSON %q: %v", stdout.String(), err)
	}
	if row["id"] != containerID+"@main" || row["names"] != "demo@main" || row["container"] != "demo" || row["image"] != "example/demo:v1" || row["image_id"] != digest {
		t.Errorf("ps JSON row=%+v", row)
	}
	if row["created_at"] != created.Format(time.RFC3339Nano) {
		t.Errorf("created_at=%v, want %s", row["created_at"], created.Format(time.RFC3339Nano))
	}
}

func TestPsRejectsInvalidFiltersAndSelection(t *testing.T) {
	tests := [][]string{
		{"ps", "--filter", "missing-equals"},
		{"ps", "--filter", "unknown=value"},
		{"ps", "--filter", "name=["},
		{"ps", "--last", "-2"},
		{"ps", "--latest", "--last", "2"},
	}
	for _, args := range tests {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("%v status=%d, want 1", args, status)
		}
		if b.families == nil && stdout.Len() != 0 {
			t.Errorf("%v unexpectedly wrote %q", args, stdout.String())
		}
	}
}

func TestTopTableAndJSON(t *testing.T) {
	b := &fakeBackend{processList: []ProcessInfo{{
		UID: 1000, PID: 7, PPID: 1, PGID: 7, Threads: []int32{7, 8},
		C: 3, TTY: "?", STime: "12:34", Time: "9ms", Cmd: "worker",
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"top", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top status=%d, stderr=%q", status, stderr.String())
	}
	if b.processTarget != (selector{container: "web", branch: "child"}) {
		t.Errorf("top target=%+v", b.processTarget)
	}
	for _, text := range []string{"UID", "PID", "CMD", "1000", "worker"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("top output %q does not contain %q", stdout.String(), text)
		}
	}
	stdout.Reset()
	if status := runCommand([]string{"top", "--json", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top --json status=%d, stderr=%q", status, stderr.String())
	}
	var got []ProcessInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !reflect.DeepEqual(got, b.processList) {
		t.Errorf("top JSON=%+v err=%v, want %+v", got, err, b.processList)
	}

	stdout.Reset()
	if status := runCommand([]string{"top", "web@child", "-eo", "pid,ppid,nlwp,pcpu,comm"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top -eo status=%d, stderr=%q", status, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !reflect.DeepEqual(strings.Fields(lines[0]), []string{"PID", "PPID", "NLWP", "%CPU", "COMMAND"}) || !reflect.DeepEqual(strings.Fields(lines[1]), []string{"7", "1", "2", "3", "worker"}) {
		t.Errorf("top -eo output=%q", stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"top", "web@child", "-o", "user=OWNER", "-opid,args"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top repeated -o status=%d, stderr=%q", status, stderr.String())
	}
	lines = strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !reflect.DeepEqual(strings.Fields(lines[0]), []string{"OWNER", "PID", "COMMAND"}) || !reflect.DeepEqual(strings.Fields(lines[1]), []string{"1000", "7", "worker"}) {
		t.Errorf("top repeated -o output=%q", stdout.String())
	}
}

func TestTopRejectsUnsupportedProcessOptions(t *testing.T) {
	for _, args := range [][]string{
		{"top", "web", "aux"},
		{"top", "web", "-o", "pid,rss"},
		{"top", "web", "-o"},
		{"top", "--json", "web", "-ef"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("%q status=%d, want 1", args, status)
		}
		if b.processTarget != (selector{}) {
			t.Errorf("%q queried backend before rejecting options", args)
		}
	}
}

func TestCommandsUseQualifiedBranchesAndBareForkDestination(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "web@test-a\n" {
		t.Errorf("fork output=%q, want %q", got, "web@test-a\n")
	}
	if b.forkSource != (selector{container: "web", branch: "main"}) || b.forkDestination != "test-a" || !b.forkPaused {
		t.Errorf("fork call = source %+v destination %q paused=%t", b.forkSource, b.forkDestination, b.forkPaused)
	}

	stdout.Reset()
	if status := runCommand([]string{"pause", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pause status=%d, stderr=%q", status, stderr.String())
	}
	if b.paused != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("pause target=%+v", b.paused)
	}

	stdout.Reset()
	if status := runCommand([]string{"resume", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("resume status=%d, stderr=%q", status, stderr.String())
	}
	if b.resumed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("resume target=%+v", b.resumed)
	}

	stdout.Reset()
	b.resumed = selector{}
	if status := runCommand([]string{"unpause", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("unpause status=%d, stderr=%q", status, stderr.String())
	}
	if b.resumed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("unpause target=%+v", b.resumed)
	}

	stdout.Reset()
	if status := runCommand([]string{"rm", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch-targeted rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
	if b.destroyForce {
		t.Error("branch-targeted rm without -f forced removal")
	}
}

func TestForkRejectsRemovedCheckFlag(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--check", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("fork accepted the removed --check flag")
	}
	if b.forkDestination != "" {
		t.Fatalf("removed --check flag attempted a fork to %q", b.forkDestination)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -check") {
		t.Fatalf("stderr=%q, want unknown --check flag", stderr.String())
	}
}

func TestForkGeneratesDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.forkDestination, "fork-") {
		t.Errorf("generated destination %q does not start with fork-", b.forkDestination)
	}
	if got := stdout.String(); got != "web@"+b.forkDestination+"\n" {
		t.Errorf("fork output=%q, want %q", got, "web@"+b.forkDestination+"\n")
	}
}

func TestForkCreatesSiblingBatch(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "a", "b", "c"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.forkDestinations, []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork destinations=%q, want %q", got, want)
	}
	if !b.forkPaused {
		t.Fatal("batch fork did not preserve --paused")
	}
	if got, want := stdout.String(), "web@a\nweb@b\nweb@c\n"; got != want {
		t.Errorf("fork output=%q, want %q", got, want)
	}
}

type fakeDistributedBackend struct {
	*fakeBackend
	source           selector
	targetNode       string
	targetContainer  string
	targetContainers []string
	targetPaused     bool
}

type fakeElasticNodeBackend struct {
	*fakeBackend
	stoppedNode  string
	stoppedMode  string
	stoppedLocal bool
	removedNode  string
}

func (*fakeElasticNodeBackend) clusterNodes() ([]cluster.NodeRecord, error) { return nil, nil }
func (b *fakeElasticNodeBackend) removeNode(node string) error {
	b.removedNode = node
	return nil
}
func (b *fakeElasticNodeBackend) stopNode(node, mode string, local bool) ([]selector, error) {
	b.stoppedNode, b.stoppedMode, b.stoppedLocal = node, mode, local
	return nil, nil
}
func (*fakeElasticNodeBackend) rebalanceNode(string, string, bool) (cluster.FixedTerminalPlan, error) {
	return cluster.FixedTerminalPlan{}, nil
}
func (*fakeElasticNodeBackend) plannerSnapshot(string, bool) (cluster.PlannerSnapshot, error) {
	return cluster.PlannerSnapshot{}, nil
}

func TestNodeStopAcceptsPolicyAfterNode(t *testing.T) {
	b := &fakeElasticNodeBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"node", "stop", "node-b", "--migrate-all"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("node stop status=%d stderr=%q", status, stderr.String())
	}
	if b.stoppedNode != "node-b" || b.stoppedMode != "migrate" || b.stoppedLocal {
		t.Fatalf("node stop call=(%q, %q, %t)", b.stoppedNode, b.stoppedMode, b.stoppedLocal)
	}
}

func TestNodeRemoveForgetsOfflineIdentity(t *testing.T) {
	b := &fakeElasticNodeBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"node", "rm", "node-b"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("node rm status=%d stderr=%q", status, stderr.String())
	}
	if b.removedNode != "node-b" || stdout.String() != "node-b\n" {
		t.Fatalf("node rm target=%q stdout=%q", b.removedNode, stdout.String())
	}
}

func (b *fakeDistributedBackend) forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error) {
	b.source = source
	b.targetNode = targetNode
	b.targetContainer = targetContainer
	b.targetPaused = paused
	return parseSelector(targetContainer)
}

func (b *fakeDistributedBackend) forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error) {
	b.source = source
	b.targetNode = targetNode
	b.targetContainers = append([]string(nil), targetContainers...)
	b.targetPaused = paused
	results := make([]selector, len(targetContainers))
	for i, target := range targetContainers {
		parsed, err := parseSelector(target)
		if err != nil {
			return nil, err
		}
		results[i] = parsed
	}
	if len(targetContainers) == 1 {
		b.targetContainer = targetContainers[0]
	}
	return results, nil
}

func TestForkTargetsRemoteNode(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "--node", "node-b", "web@child", "remote"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if b.source != (selector{container: "web", branch: "child"}) || b.targetNode != "node-b" || b.targetContainer != "web@remote" || !b.targetPaused {
		t.Fatalf("distributed fork call source=%+v node=%q container=%q paused=%t", b.source, b.targetNode, b.targetContainer, b.targetPaused)
	}
	if got, want := stdout.String(), "web@remote\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkTargetsRemoteSiblingBatch(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@main", "remote-a", "remote-b"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.targetContainers, []string{"web@remote-a", "web@remote-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("distributed targets=%q, want %q", got, want)
	}
	if got, want := stdout.String(), "web@remote-a\nweb@remote-b\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkGeneratesQualifiedRemoteDestination(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.targetContainer, "web@fork-") {
		t.Fatalf("generated remote container=%q", b.targetContainer)
	}
	if got, want := stdout.String(), b.targetContainer+"\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkRejectsQualifiedRemoteDestination(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@main", "remote@second"}, &stdout, &stderr, b); status != 0 {
		if !strings.Contains(stderr.String(), "must be a branch name") {
			t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
		}
		return
	}
	t.Fatal("qualified remote destination was accepted")
}

func TestForkRejectsQualifiedLocalDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"fork", "web@main", "other@child"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "must be a branch name, not CONTAINER@BRANCH") {
		t.Errorf("stderr=%q", stderr.String())
	}
}

func TestRemoveAcceptsFamiliesAndQualifiedBranches(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"rm", "web"}, &stdout, &stderr, b)
	if status != 0 || b.removed != "web" {
		t.Fatalf("family rm status=%d removed=%q stderr=%q", status, b.removed, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"rm", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch-targeted rm main status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "main"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"rm", "-f", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("forced branch-targeted rm main status=%d, stderr=%q", status, stderr.String())
	}
	if !b.destroyForce {
		t.Error("rm -f did not force branch removal")
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"rm", "-fv", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("combined family rm flags status=%d, stderr=%q", status, stderr.String())
	}
	if !b.removeForce || !b.removeVolumes {
		t.Errorf("rm -fv parsed force=%t volumes=%t", b.removeForce, b.removeVolumes)
	}
}

// missingTargetsBackend is a fakeBackend without the containers in missing.
type volumesBackend struct {
	*fakeBackend
	volumes []ttcvolume.VolumeInfo
}

func (b *volumesBackend) createVolume(name string) (ttcvolume.VolumeInfo, error) {
	return ttcvolume.VolumeInfo{}, errors.New("not supported")
}

func (b *volumesBackend) listVolumes() ([]ttcvolume.VolumeInfo, error) {
	return b.volumes, nil
}

func (b *volumesBackend) removeVolume(string, bool) error { return errors.New("not supported") }

func (b *volumesBackend) volumeDriver() (string, error) { return "ttcvol", nil }

func TestVolumeListJSON(t *testing.T) {
	b := &volumesBackend{fakeBackend: &fakeBackend{}, volumes: []ttcvolume.VolumeInfo{{Name: "web"}, {Name: "db"}}}
	var out bytes.Buffer
	if err := runVolume([]string{"ls", "--json"}, &out, b); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), `[{"name":"db","driver":"ttcvol"},{"name":"web","driver":"ttcvol"}]`+"\n"; got != want {
		t.Errorf("volume ls --json = %q, want %q", got, want)
	}
	out.Reset()
	if err := runVolume([]string{"ls"}, &out, b); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "DRIVER  VOLUME NAME\nttcvol  db\nttcvol  web\n"; got != want {
		t.Errorf("volume ls = %q, want %q", got, want)
	}
	if err := runVolume([]string{"ls", "extra"}, &out, b); err == nil {
		t.Error("volume ls accepted an argument")
	}
}

type missingTargetsBackend struct {
	*fakeBackend
	missing  map[string]bool
	removed  []string
	signaled []string
}

func (b *missingTargetsBackend) remove(name string, force, volumes bool) error {
	if b.missing[name] {
		return fmt.Errorf("container %q does not exist: %w", name, os.ErrNotExist)
	}
	b.removed = append(b.removed, name)
	return nil
}

func (b *missingTargetsBackend) destroy(target selector, force bool) error {
	if b.missing[target.container] {
		return fmt.Errorf("container %q does not exist: %w", target.container, os.ErrNotExist)
	}
	b.removed = append(b.removed, target.String())
	return nil
}

func (b *missingTargetsBackend) signal(target selector, signal int, all bool) error {
	if b.missing[target.container] {
		return fmt.Errorf("container %q does not exist: %w", target.container, os.ErrNotExist)
	}
	b.signaled = append(b.signaled, target.String())
	return nil
}

// As docker's do, a command of several targets tries every one: a target that
// does not exist neither keeps the others from being acted on nor goes
// unreported, and the command fails.
func TestCommandsOfSeveralTargetsTryEveryTarget(t *testing.T) {
	for _, test := range []struct {
		args []string
		done func(*missingTargetsBackend) []string
		want []string
	}{
		{
			args: []string{"rm", "-f", "gone", "b1", "lost", "b2"},
			done: func(b *missingTargetsBackend) []string { return b.removed },
			want: []string{"b1", "b2"},
		},
		{
			args: []string{"rm", "gone@main", "b1@main", "lost@child", "b2@child"},
			done: func(b *missingTargetsBackend) []string { return b.removed },
			want: []string{"b1@main", "b2@child"},
		},
		{
			args: []string{"kill", "gone@main", "b1@main", "lost@main", "b2@main"},
			done: func(b *missingTargetsBackend) []string { return b.signaled },
			want: []string{"b1@main", "b2@main"},
		},
	} {
		b := &missingTargetsBackend{fakeBackend: &fakeBackend{}, missing: map[string]bool{"gone": true, "lost": true}}
		var stdout, stderr bytes.Buffer
		if status := runCommand(test.args, &stdout, &stderr, b); status != 1 {
			t.Errorf("%q: status=%d, want 1", test.args, status)
		}
		if got := test.done(b); !slices.Equal(got, test.want) {
			t.Errorf("%q: acted on %q, want %q", test.args, got, test.want)
		}
		if got, want := stdout.String(), strings.Join(test.want, "\n")+"\n"; got != want {
			t.Errorf("%q: stdout=%q, want %q", test.args, got, want)
		}
		for _, name := range []string{"gone", "lost"} {
			if want := fmt.Sprintf("container %q does not exist", name); strings.Count(stderr.String(), want) != 1 {
				t.Errorf("%q: stderr=%q, want one %s", test.args, stderr.String(), want)
			}
		}
	}
}

func TestPullNormalizesDockerReference(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{
		Reference: "docker.io/library/ubuntu:latest",
		Digest:    "sha256:1234",
		Platform:  "linux/amd64",
		PullStats: &ttcimages.PullStats{DownloadedBytes: 1024, ReusedBytes: 2048},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"pull", "ubuntu"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/ubuntu:latest" {
		t.Errorf("pull reference=%q", b.pullReference)
	}
	want := "Pulled docker.io/library/ubuntu:latest\nDigest: sha256:1234\nPlatform: linux/amd64\nContent: 1024 bytes downloaded, 2048 bytes reused\n"
	if got := stdout.String(); got != want {
		t.Errorf("pull output=%q, want %q", got, want)
	}
}

func TestImagePullAlias(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{Digest: "sha256:alias"}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"image", "pull", "-q", "alpine:3.20"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/alpine:3.20" || stdout.String() != "sha256:alias\n" {
		t.Fatalf("image pull reference=%q output=%q", b.pullReference, stdout.String())
	}
}

func TestPullOptionsAndQuietOutput(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{Digest: "sha256:5678"}}
	var stdout, stderr bytes.Buffer
	args := []string{"pull", "--platform", "linux/arm64", "-q", "example/app:v1"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/example/app:v1" || b.pullPlatform != "linux/arm64" {
		t.Errorf("pull call: reference=%q platform=%q", b.pullReference, b.pullPlatform)
	}
	if got := stdout.String(); got != "sha256:5678\n" {
		t.Errorf("quiet pull output=%q", got)
	}
}

func TestImageLoadUsesDockerCLIAndRestoresEveryTag(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "images.tar")
	if err := os.WriteFile(archive, []byte("archive bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"load", "-i", archive, "--platform", "linux/amd64"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("load status=%d, stderr=%q", status, stderr.String())
	}
	if b.loadedBytes != len("archive bytes") || b.loadedPlatform != "linux/amd64" {
		t.Fatalf("load bytes=%d platform=%q", b.loadedBytes, b.loadedPlatform)
	}
	want := "Loaded image: one:latest\nLoaded image: two:test\n"
	if got := stdout.String(); got != want {
		t.Fatalf("load output=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"image", "load", "-q", "-i", archive}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image load status=%d, stderr=%q", status, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("quiet image load output=%q", stdout.String())
	}
	if status := runCommand([]string{"image", "import", archive}, &stdout, &stderr, b); status == 0 {
		t.Fatal("legacy image import unexpectedly succeeded")
	}
}

func TestImageSaveUsesDockerCLIAndStreamsArchive(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"save", "--platform", "linux/amd64", "one:latest", "two:test"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("save status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "image archive" {
		t.Fatalf("save archive=%q", got)
	}
	if want := []string{"one:latest", "two:test"}; !reflect.DeepEqual(b.savedIdentifiers, want) || b.savedPlatform != "linux/amd64" {
		t.Fatalf("save identifiers=%q platform=%q", b.savedIdentifiers, b.savedPlatform)
	}
	output := filepath.Join(t.TempDir(), "images.tar")
	if status := runCommand([]string{"image", "save", "-o", output, "one:latest"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image save status=%d, stderr=%q", status, stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "image archive" {
		t.Fatalf("saved file=%q", data)
	}
}

func TestImagesDockerStyleTableAndQuietAliases(t *testing.T) {
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b := &fakeBackend{imageList: []ImageInfo{
		{
			Reference:    "docker.io/library/busybox:latest",
			Digest:       digest,
			Size:         2_150_000,
			Platform:     "linux/amd64",
			Materialized: true,
			CreatedAt:    time.Now().Add(-2 * time.Hour),
		},
		{
			Reference: "docker.io/library/busybox:stable",
			Digest:    digest,
			Size:      2_150_000,
			Platform:  "linux/amd64",
			CreatedAt: time.Now().Add(-2 * time.Hour),
		},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"images"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("images status=%d, stderr=%q", status, stderr.String())
	}
	for _, want := range []string{"REPOSITORY", "TAG", "IMAGE ID", "PLATFORM", "ROOTFS", "busybox", "latest", "stable", "0123456789ab", "linux/amd64", "2.15MB", "2 hours ago"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("images output %q does not contain %q", stdout.String(), want)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"image", "ls", "-q"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image ls -q status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "0123456789ab\n"; got != want {
		t.Errorf("image ls -q=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"images", "-q", "--no-trunc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("images --no-trunc status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), digest+"\n"; got != want {
		t.Errorf("images --no-trunc=%q, want %q", got, want)
	}
}

func TestImageInspectAggregatesAliasesAndTTCState(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	created := time.Date(2026, time.July, 1, 2, 3, 4, 0, time.UTC)
	infos := []ImageInfo{
		{
			Reference:  "docker.io/library/busybox:latest",
			Digest:     digest,
			MediaType:  "application/vnd.oci.image.index.v1+json",
			Size:       1234,
			Platform:   "linux/arm64/v8",
			Containers: []string{"zeta", "alpha"},
			CreatedAt:  created,
			Labels:     map[string]string{"base.label": "kept"},
		},
		{
			Reference:    "docker.io/library/busybox:stable",
			Digest:       digest,
			Platform:     "linux/arm64/v8",
			Materialized: true,
			LocalCache:   &ImageCacheInfo{CompleteFiles: 3, TotalFiles: 5, CompleteBytes: 1024, TotalBytes: 2048},
		},
	}
	got, err := inspectImageFromInventory(infos, "busybox")
	if err != nil {
		t.Fatalf("inspectImageFromInventory: %v", err)
	}
	if got.ID != digest || got.OS != "linux" || got.Architecture != "arm64" || got.Variant != "v8" || !got.Materialized {
		t.Errorf("inspect info=%+v", got)
	}
	if got.LocalCache == nil || got.LocalCache.CompleteBytes != 1024 || got.LocalCache.TotalBytes != 2048 {
		t.Errorf("LocalCache=%+v", got.LocalCache)
	}
	if want := []string{"busybox:latest", "busybox:stable"}; !reflect.DeepEqual(got.RepoTags, want) {
		t.Errorf("RepoTags=%q, want %q", got.RepoTags, want)
	}
	if want := []string{"busybox@" + digest}; !reflect.DeepEqual(got.RepoDigests, want) {
		t.Errorf("RepoDigests=%q, want %q", got.RepoDigests, want)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got.Containers, want) {
		t.Errorf("Containers=%q, want %q", got.Containers, want)
	}
	if got.Config.Labels["base.label"] != "kept" {
		t.Errorf("Config.Labels=%v", got.Config.Labels)
	}
}

func TestCommitCommandUsesDockerOptions(t *testing.T) {
	digest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	b := &fakeBackend{commitInfo: ttcimages.Info{Digest: digest}}
	var stdout, stderr bytes.Buffer
	args := []string{
		"commit", "--author", "TTC Harness <ttc@example.com>", "--message", "prepared session",
		"--change", "LABEL timetravel.usage=agent-session", "--change", `CMD ["agent","run"]`,
		"--pause=false", "demo@child", "example/session:v1",
	}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("commit status=%d, stderr=%q", status, stderr.String())
	}
	wantChanges := []string{"LABEL timetravel.usage=agent-session", `CMD ["agent","run"]`}
	if b.commitTarget != (selector{container: "demo", branch: "child"}) || b.commitReference != "example/session:v1" ||
		b.commitOptions.Author != "TTC Harness <ttc@example.com>" || b.commitOptions.Message != "prepared session" ||
		b.commitOptions.Pause || !reflect.DeepEqual(b.commitOptions.Changes, wantChanges) {
		t.Fatalf("commit target=%+v reference=%q options=%+v", b.commitTarget, b.commitReference, b.commitOptions)
	}
	if stdout.String() != digest+"\n" {
		t.Fatalf("commit output=%q", stdout.String())
	}
}

func TestImageInspectAndRemoveCommandRouting(t *testing.T) {
	digest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b := &fakeBackend{
		imageInspectInfo: ImageInspectInfo{ID: digest, RepoTags: []string{"alpine:latest"}},
		imageRemoveResult: ImageRemoveResult{
			Untagged: []string{"alpine:latest"},
			Deleted:  digest,
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"image", "inspect", "alpine"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image inspect status=%d, stderr=%q", status, stderr.String())
	}
	if b.imageInspectID != "alpine" || !strings.Contains(stdout.String(), `"Id": "`+digest+`"`) {
		t.Errorf("image inspect id=%q output=%q", b.imageInspectID, stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"image", "inspect", "alpine", "--format", "{{.Id}}"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted image inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != digest+"\n" {
		t.Errorf("formatted image inspect output=%q", got)
	}
	stdout.Reset()
	if status := runCommand([]string{"rmi", "--force", "alpine"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rmi status=%d, stderr=%q", status, stderr.String())
	}
	if b.imageRemoveID != "alpine" || !b.imageRemoveForce {
		t.Errorf("remove call id=%q force=%t", b.imageRemoveID, b.imageRemoveForce)
	}
	want := "Untagged: alpine:latest\nDeleted: " + digest + "\n"
	if got := stdout.String(); got != want {
		t.Errorf("rmi output=%q, want %q", got, want)
	}
}

func TestValidateImageRemovalRequiresForceWhenInUse(t *testing.T) {
	digest := "sha256:abcd"
	users := []string{"alpha", "zeta"}
	if err := validateImageRemoval(digest, users, false); err == nil || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Fatalf("validateImageRemoval without force=%v, want named conflict", err)
	}
	if err := validateImageRemoval(digest, users, true); err != nil {
		t.Errorf("validateImageRemoval with force: %v", err)
	}
	if err := validateImageRemoval(digest, nil, false); err != nil {
		t.Errorf("validateImageRemoval unused: %v", err)
	}
}

func TestRunParsesDockerStyleOptions(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	b := &fakeBackend{containerID: containerID}
	var stdout, stderr bytes.Buffer
	args := []string{"run", "--name", "demo", "-dit", "--rm", "--read-only", "--stop-signal", "SIGUSR2", "--stop-timeout", "7", "--pids-limit", "23", "-m", "128m", "--cpus", "1.5", "--cpu-shares", "512", "--shm-size", "32m", "--group-add", "wheel", "--group-add", "4242", "--cap-add", "sys_admin", "--cap-drop", "cap_net_raw", "--ulimit", "nofile=64:128", "--ulimit", "core=0", "--pull", "never", "--network", "host", "--waive-live-fork", "-e", "A=1", "--label", "suite=fast", "--workdir", "/work", "--user", "123:456", "-h", "demo-host", "--entrypoint", "/entry", "--log-driver", "json-file", "--log-opt", "max-size=1.5m", "--log-opt", "max-file=4", "--log-opt", "env=A", "--log-opt", "labels=suite", "-v", "/host/a:/host-data:ro", "--mount", "type=bind,src=/host/b,dst=/config,readonly,immutable-source", "--tmpfs", "/scratch:mode=0700,size=1m", "--mount", "type=tmpfs,target=/worktmp,tmpfs-size=2097152,tmpfs-mode=0750", "--mount", "type=proc,target=/alt-proc,readonly", "--mount", "type=sysfs,target=/alt-sys", "busybox:1.36", "echo", "ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("run status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("run output=%q, want %q", got, want)
	}
	if b.runRequest.name != "demo" || b.runRequest.reference != "busybox:1.36" || !b.runRequest.detach || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || !b.runRequest.readonlyRootfs || b.runRequest.cwd != "/work" || b.runRequest.user != "123:456" || b.runRequest.hostname != "demo-host" || b.runRequest.networkMode != "host" || b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" || b.runRequest.pullPolicy != "never" || b.runRequest.stopSignal != "SIGUSR2" || b.runRequest.stopTimeout != 7 {
		t.Errorf("run request=%+v", b.runRequest)
	}
	if b.runRequest.pidsLimit == nil || *b.runRequest.pidsLimit != 23 {
		t.Errorf("pids limit=%v, want 23", b.runRequest.pidsLimit)
	}
	if b.runRequest.memoryLimit == nil || *b.runRequest.memoryLimit != 128*1024*1024 {
		t.Errorf("memory limit=%v, want 128 MiB", b.runRequest.memoryLimit)
	}
	if b.runRequest.nanoCPUs == nil || *b.runRequest.nanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs=%v, want 1500000000", b.runRequest.nanoCPUs)
	}
	if b.runRequest.cpuShares == nil || *b.runRequest.cpuShares != 512 {
		t.Errorf("CPU shares=%v, want 512", b.runRequest.cpuShares)
	}
	if got, want := b.runRequest.shmSize, int64(32*1024*1024); got != want {
		t.Errorf("shared memory size=%d, want %d", got, want)
	}
	if got, want := b.runRequest.groupAdd, []string{"wheel", "4242"}; !reflect.DeepEqual(got, want) {
		t.Errorf("group add=%q, want %q", got, want)
	}
	if got, want := b.runRequest.capAdd, []string{"SYS_ADMIN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cap add=%q, want %q", got, want)
	}
	if got, want := b.runRequest.capDrop, []string{"NET_RAW"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cap drop=%q, want %q", got, want)
	}
	wantUlimits := []ttcbundle.Ulimit{{Name: "core", Soft: 0, Hard: 0}, {Name: "nofile", Soft: 64, Hard: 128}}
	if !reflect.DeepEqual(b.runRequest.ulimits, wantUlimits) {
		t.Errorf("ulimits=%+v, want %+v", b.runRequest.ulimits, wantUlimits)
	}
	if got, want := strings.Join(b.runRequest.args, " "), "echo ok"; got != want {
		t.Errorf("run args=%q, want %q", got, want)
	}
	if got, want := strings.Join(b.runRequest.entrypoint, " "), "/entry"; got != want {
		t.Errorf("entrypoint=%q, want %q", got, want)
	}
	wantMounts := []ttcbundle.HostMount{
		{Source: "/host/a", Destination: "/host-data", Readonly: true},
		{Source: "/host/b", Destination: "/config", Readonly: true, ImmutableSource: true},
	}
	if !reflect.DeepEqual(b.runRequest.hostMounts, wantMounts) {
		t.Errorf("host mounts=%+v, want %+v", b.runRequest.hostMounts, wantMounts)
	}
	wantFilesystems := []ttcbundle.FilesystemMount{
		{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
		{Type: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
		{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
	}
	if !reflect.DeepEqual(b.runRequest.filesystemMounts, wantFilesystems) {
		t.Errorf("filesystem mounts=%+v, want %+v", b.runRequest.filesystemMounts, wantFilesystems)
	}
	if b.runRequest.logDriver != "json-file" {
		t.Errorf("log driver=%q, want json-file", b.runRequest.logDriver)
	}
	if got, want := b.runRequest.logOptions, map[string]string{"env": "A", "labels": "suite"}; !reflect.DeepEqual(got, want) {
		t.Errorf("log options=%q, want %q", got, want)
	}
	if b.runRequest.consoleLogMaxBytes != 1572864 || b.runRequest.consoleLogMaxFiles != 4 {
		t.Errorf("log retention=(%d, %d), want (1572864, 4)", b.runRequest.consoleLogMaxBytes, b.runRequest.consoleLogMaxFiles)
	}
}

func TestRunAndCreateParseTimeTravelNetworking(t *testing.T) {
	wantPorts := []ttcbundle.PortBinding{
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", BranchID: "main"},
		{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp", BranchID: "main"},
	}
	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			backend := &fakeBackend{containerID: "demo"}
			var stdout, stderr bytes.Buffer
			args := []string{command, "--network=ttc", "--nat", "--ip", "192.0.2.44", "--mac-address", "02:aa:bb:cc:dd:ee", "--interface-name", "eth1", "-p", "8080:80", "--publish", "127.0.0.1:8443:443/tcp", "busybox"}
			if status := runCommand(args, &stdout, &stderr, backend); status != 0 {
				t.Fatalf("status=%d, stderr=%q", status, stderr.String())
			}
			if backend.runRequest.networkMode != "ttc" || !backend.runRequest.networkNAT {
				t.Errorf("network request=%+v", backend.runRequest)
			}
			if backend.runRequest.networkIP != "192.0.2.44" || backend.runRequest.networkMAC != "02:aa:bb:cc:dd:ee" {
				t.Errorf("virtual NIC=(%q, %q), want custom IP and MAC", backend.runRequest.networkIP, backend.runRequest.networkMAC)
			}
			if got := backend.runRequest.networkInterface; got != "eth1" {
				t.Errorf("virtual NIC name=%q, want eth1", got)
			}
			if !reflect.DeepEqual(backend.runRequest.publishedPorts, wantPorts) {
				t.Errorf("published ports=%+v, want %+v", backend.runRequest.publishedPorts, wantPorts)
			}
		})
	}
}

func TestImageProgressIsHumanReadable(t *testing.T) {
	var output bytes.Buffer
	progress := newImageProgress(&output)
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressResolving})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Current: 500, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloadDone, Current: 1000, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressExtracting, Total: 1000, Index: 1, Count: 2})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressExtracted, Current: 1000, Total: 1000, Index: 1, Count: 2})
	for _, want := range []string{"Resolving image metadata", "Pulling fs layer", "Downloading 500B / 1kB (50%)", "Download complete", "Extracting layer 1/2", "Pull complete"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("progress output %q does not contain %q", output.String(), want)
		}
	}
}

func TestImageProgressRepaintsOneTerminalLinePerLayer(t *testing.T) {
	var output bytes.Buffer
	progress := newImageProgressWithTerminal(&output, true, 80)
	first := ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 10 << 20}
	first.Descriptor.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second := ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 20 << 20}
	second.Descriptor.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressResolving})
	progress(first)
	progress(second)
	first.Current = 5 << 20
	progress(first)
	first.Phase = ttcimages.ProgressDownloadDone
	first.Current = first.Total
	progress(first)
	second.Phase = ttcimages.ProgressAlreadyExists
	second.Current = second.Total
	progress(second)
	first.Phase = ttcimages.ProgressExtracting
	first.Current = 4 << 20
	first.Index = 1
	first.Count = 2
	progress(first)
	first.Phase = ttcimages.ProgressExtracted
	first.Current = first.Total
	progress(first)

	got := output.String()
	for _, want := range []string{
		"Resolving image metadata\n",
		"\x1b[1A",
		"\x1b[2A",
		"\r\x1b[2Kaaaaaaaaaaaa: Downloading",
		"bbbbbbbbbbbb: Already exists",
		"Extracting 1/2",
		"aaaaaaaaaaaa: Pull complete",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("interactive progress output %q does not contain %q", got, want)
		}
	}
}

func TestTimeTravelNetworkingRejectsInvalidCombinations(t *testing.T) {
	tests := [][]string{
		{"run", "--nat", "busybox"},
		{"run", "--ip", "192.0.2.44", "busybox"},
		{"create", "--network=ttc", "--ip", "192.0.2.2", "busybox"},
		{"create", "--network=ttc", "--mac-address", "01:00:5e:00:00:01", "busybox"},
		{"run", "--interface-name", "eth1", "busybox"},
		{"create", "--network=ttc", "--interface-name", "lo", "busybox"},
		{"create", "--network=ttc", "--interface-name", "eth/1", "busybox"},
		{"create", "--network=ttc", "--interface-name", "0123456789abcdef", "busybox"},
		{"run", "-p", "8080:80", "busybox"},
		{"create", "--network=ttc", "-p", "8080:80/udp", "busybox"},
		{"create", "--network=ttc", "-p", "not-a-port", "busybox"},
		{"create", "--network=ttc", "-p", "127.0.0.1:8080:80", "-p", "127.0.0.1:8080:81", "busybox"},
	}
	for _, args := range tests {
		backend := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, backend); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if backend.runRequest.reference != "" {
			t.Errorf("runCommand(%q) reached backend", args)
		}
	}
}

func TestParseTmpfsMountDockerOptions(t *testing.T) {
	const value = "/data:ro,suid,dev,noexec,sync,dirsync,noatime,nodiratime,size=64m,mode=1770,uid=123,gid=456,nr_inodes=400k,nr_blocks=1024"
	got, err := parseTmpfsMount(value)
	if err != nil {
		t.Fatalf("parseTmpfsMount: %v", err)
	}
	want := ttcbundle.FilesystemMount{
		Type: "tmpfs", Destination: "/data", Readonly: true,
		Options:    []string{"ro", "suid", "dev", "noexec", "sync", "dirsync", "noatime", "nodiratime", "size=64m", "mode=1770", "uid=123", "gid=456", "nr_inodes=400k", "nr_blocks=1024"},
		HostConfig: true, InspectOptions: strings.TrimPrefix(value, "/data:"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mount=%+v, want %+v", got, want)
	}
	lastWins, err := parseTmpfsMount("/data:ro,rw")
	if err != nil {
		t.Fatalf("last access mode: %v", err)
	}
	if lastWins.Readonly || !reflect.DeepEqual(lastWins.Options, []string{"rw"}) {
		t.Errorf("last access mode did not win: %+v", lastWins)
	}
}

func TestRunAndCreateLoadEnvironmentAndLabels(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "container.env")
	const absent = "TTC_TEST_ABSENT_ENV_7281"
	if _, ok := os.LookupEnv(absent); ok {
		t.Fatalf("test environment unexpectedly contains %s", absent)
	}
	if err := os.WriteFile(envFile, []byte("# from a file\nFROM_FILE=file\nHOST_IMPORT\n"+absent+"\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	labelFile := filepath.Join(dir, "container.labels")
	if err := os.WriteFile(labelFile, []byte("# metadata\nsuite=file\nfile-only=value\nempty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_IMPORT", "host-value")
	t.Setenv("DIRECT_IMPORT", "direct-host")

	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := []string{
				command,
				"--env-file", envFile,
				"--env", "FROM_FILE=direct",
				"-e", "DIRECT=value",
				"-e", "DIRECT_IMPORT",
				"-e", absent,
				"--label-file", labelFile,
				"--label", "suite=direct",
				"-l", "direct-only=value",
				"--name", "demo",
				"--pids-limit", "17",
				"--memory", "64m",
				"--cpu-period", "200000",
				"--cpu-quota", "-1",
				"--ulimit", "stack=8388608",
				"--group-add", "4242",
				"--cap-add", "chown",
				"--cap-drop", "all",
				"--add-host", "db:192.0.2.10",
				"--add-host", "v6=[2001:0db8::10]",
				"--dns", "203.0.113.53",
				"--dns", "2001:0db8::53",
				"--dns-search", "example.test",
				"--dns-option", "ndots:2",
				"--dns-opt", "rotate",
				"busybox:1.36",
			}
			if status := runCommand(args, &stdout, &stderr, b); status != 0 {
				t.Fatalf("%s status=%d, stderr=%q", command, status, stderr.String())
			}
			wantEnv := []string{
				"FROM_FILE=file",
				"HOST_IMPORT=host-value",
				absent,
				"FROM_FILE=direct",
				"DIRECT=value",
				"DIRECT_IMPORT=direct-host",
				absent,
			}
			if !reflect.DeepEqual(b.runRequest.env, wantEnv) {
				t.Errorf("environment=%q, want %q", b.runRequest.env, wantEnv)
			}
			wantLabels := map[string]string{
				"suite":       "direct",
				"file-only":   "value",
				"empty":       "",
				"direct-only": "value",
			}
			if !reflect.DeepEqual(b.runRequest.labels, wantLabels) {
				t.Errorf("labels=%q, want %q", b.runRequest.labels, wantLabels)
			}
			if b.runRequest.pidsLimit == nil || *b.runRequest.pidsLimit != 17 {
				t.Errorf("pids limit=%v, want 17", b.runRequest.pidsLimit)
			}
			if b.runRequest.memoryLimit == nil || *b.runRequest.memoryLimit != 64*1024*1024 {
				t.Errorf("memory limit=%v, want 64 MiB", b.runRequest.memoryLimit)
			}
			if b.runRequest.cpuPeriod == nil || *b.runRequest.cpuPeriod != 200000 || b.runRequest.cpuQuota == nil || *b.runRequest.cpuQuota != -1 {
				t.Errorf("CPU period/quota=(%v, %v), want (200000, -1)", b.runRequest.cpuPeriod, b.runRequest.cpuQuota)
			}
			if got, want := b.runRequest.groupAdd, []string{"4242"}; !reflect.DeepEqual(got, want) {
				t.Errorf("group add=%q, want %q", got, want)
			}
			if got, want := b.runRequest.capAdd, []string{"CHOWN"}; !reflect.DeepEqual(got, want) {
				t.Errorf("cap add=%q, want %q", got, want)
			}
			if got, want := b.runRequest.capDrop, []string{"ALL"}; !reflect.DeepEqual(got, want) {
				t.Errorf("cap drop=%q, want %q", got, want)
			}
			if got, want := b.runRequest.extraHosts, []string{"db:192.0.2.10", "v6:2001:db8::10"}; !reflect.DeepEqual(got, want) {
				t.Errorf("extra hosts=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dns, []string{"203.0.113.53", "2001:db8::53"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dnsSearch, []string{"example.test"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS search=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dnsOptions, []string{"ndots:2", "rotate"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS options=%q, want %q", got, want)
			}
			if got, want := b.runRequest.shmSize, ttcbundle.DefaultShmSizeBytes; got != want {
				t.Errorf("default shared memory size=%d, want %d", got, want)
			}
			if got, want := b.runRequest.ulimits, []ttcbundle.Ulimit{{Name: "stack", Soft: 8388608, Hard: 8388608}}; !reflect.DeepEqual(got, want) {
				t.Errorf("ulimits=%+v, want %+v", got, want)
			}
			wantOutput := ""
			if command == "create" {
				wantOutput = "demo\n"
			}
			if got := stdout.String(); got != wantOutput {
				t.Errorf("output=%q, want %q", got, wantOutput)
			}
			if got := b.created; got != (command == "create") {
				t.Errorf("created=%t for %s", got, command)
			}
		})
	}
}

func TestRunAndCreateRejectInvalidEnvironmentAndLabelFiles(t *testing.T) {
	dir := t.TempDir()
	invalidEnv := filepath.Join(dir, "invalid.env")
	if err := os.WriteFile(invalidEnv, []byte(" =value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invalidLabels := filepath.Join(dir, "invalid.labels")
	if err := os.WriteFile(invalidLabels, []byte("=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run", "--env-file", filepath.Join(dir, "missing"), "busybox"},
		{"run", "--env-file", invalidEnv, "busybox"},
		{"create", "--label-file", invalidLabels, "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) reached backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectInvalidPullPolicy(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--pull", "sometimes", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s invalid pull policy status=%d, want 1", command, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("%s invalid pull policy invoked backend with %+v", command, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectInvalidCapability(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, flagName := range []string{"--cap-add", "--cap-drop"} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{command, flagName, "not-a-capability", "busybox"}, &stdout, &stderr, b); status != 1 {
				t.Errorf("%s invalid %s status=%d, want 1", command, flagName, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("%s invalid %s invoked backend", command, flagName)
			}
		}
	}
}

func TestRunAndCreateRejectInvalidShmSize(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, value := range []string{"invalid", "-1"} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{command, "--shm-size", value, "busybox"}, &stdout, &stderr, b); status != 1 {
				t.Errorf("%s invalid shm size %q status=%d, want 1", command, value, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("%s invalid shm size invoked backend", command)
			}
		}
	}
}

func TestRunAndCreateRejectInvalidUlimit(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--ulimit", "nofile=2:1", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s invalid ulimit status=%d, want 1", command, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("%s invalid ulimit invoked backend", command)
		}
	}
}

func TestRunAndCreateRejectInvalidStopPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--stop-signal", "SIGINVALID", "busybox"},
		{"create", "--stop-timeout", "-2", "busybox"},
		{"run", "--pids-limit", "-2", "busybox"},
		{"run", "--memory", "-1", "busybox"},
		{"create", "--memory", "1m", "busybox"},
		{"create", "--cpus", "0.001", "busybox"},
		{"run", "--cpu-shares", "1", "busybox"},
		{"run", "--cpus", "1", "--cpu-quota", "50000", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectInvalidNetworkConfiguration(t *testing.T) {
	invalid := [][]string{
		{"--add-host", "missing-address"},
		{"--add-host", "host:host-gateway"},
		{"--dns", "not-an-ip"},
		{"--dns-search", "bad..domain"},
		{"--dns-option", "ndots:2 rotate"},
	}
	for _, command := range []string{"run", "create"} {
		for _, option := range invalid {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := append([]string{command}, option...)
			args = append(args, "busybox")
			if status := runCommand(args, &stdout, &stderr, b); status != 1 {
				t.Errorf("runCommand(%q) status=%d, want 1", args, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
			}
		}
	}
}

func TestRunAndCreateRejectUnsupportedNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--network", "bridge", "busybox"},
		{"create", "--net", "sandbox", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunAndCreateRequireExplicitHostNetworkForkWaiver(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--network=host", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s host network without waiver status=%d, want 1", command, status)
		}
		if !strings.Contains(stderr.String(), "--waive-live-fork") {
			t.Errorf("%s host network error=%q, want waiver guidance", command, stderr.String())
		}
		stderr.Reset()
		if status := runCommand([]string{command, "--network=host", "--waive-live-fork", "busybox"}, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%s host network with waiver status=%d: %s", command, status, stderr.String())
		}
		if b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" {
			t.Errorf("%s request eligibility=(%t, %q), want explicit waiver", command, b.runRequest.liveForkEligible, b.runRequest.liveForkWaiver)
		}
	}
}

func TestRunRejectsInvalidLogOptions(t *testing.T) {
	for _, option := range []string{"max-size=0", "max-size=nope", "max-file=0", "labels=one,,two", "env-regex=[", "unknown=value", "missing"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{"run", "--log-opt", option, "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("--log-opt %q status=%d, want 1", option, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("--log-opt %q invoked backend with %+v", option, b.runRequest)
		}
	}
}

func TestRunAndCreateValidateLogDrivers(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, args := range [][]string{
			{command, "--log-driver", "journald", "busybox"},
			{command, "--log-driver", "none", "--log-opt", "max-file=2", "busybox"},
		} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand(args, &stdout, &stderr, b); status != 1 {
				t.Errorf("runCommand(%q) status=%d, want 1", args, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
			}
		}
		b := &fakeBackend{containerID: strings.Repeat("b", 64)}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--log-driver", "none", "busybox"}, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%s --log-driver none status=%d, stderr=%q", command, status, stderr.String())
		}
		if b.runRequest.logDriver != "none" {
			t.Errorf("%s log driver=%q, want none", command, b.runRequest.logDriver)
		}
	}
}

func TestParseImmutableHostMountAssertion(t *testing.T) {
	mounts, err := parseHostMounts([]string{"/host/a:/data:ro,immutable-source"}, []string{"type=bind,source=/host/b,target=/config,readonly,immutable-source=true"})
	if err != nil {
		t.Fatalf("parseHostMounts: %v", err)
	}
	if len(mounts) != 2 || !mounts[0].ImmutableSource || !mounts[1].ImmutableSource {
		t.Errorf("immutable host mounts=%+v", mounts)
	}
}

// A host bind with no mode, or rw, binds the host path itself, writable, as
// Docker does; ro, copy and lazy_copy are explicit.
func TestWritableHostMountsDefaultToDirect(t *testing.T) {
	for _, input := range []struct {
		volumes []string
		mounts  []string
	}{
		{volumes: []string{"/host:/data"}},
		{volumes: []string{"/host:/data:rw"}},
		{mounts: []string{"type=bind,source=/host,target=/data"}},
	} {
		mounts, err := parseHostMounts(input.volumes, input.mounts)
		if err != nil {
			t.Fatal(err)
		}
		if len(mounts) != 1 || !mounts[0].Direct || mounts[0].ManagedCoW || mounts[0].Readonly || mounts[0].Copy {
			t.Errorf("writable host mounts of %+v=%+v", input, mounts)
		}
	}
	for _, value := range []string{"/host:/data:ro,rw", "/host:/data:immutable-source", "/host:/data:rw,immutable-source"} {
		if mounts, err := parseHostMounts([]string{value}, nil); err == nil {
			t.Errorf("parseHostMounts(%q)=%+v, want an error", value, mounts)
		}
	}
}

// Only a family that waives live forks binds a host path writable.
func TestRunAndCreateRequireForkWaiverForWritableHostBind(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, bind := range [][]string{{"-v", "/host:/data"}, {"-v", "/host:/data:rw"}, {"--mount", "type=bind,source=/host,target=/data"}} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := append(append([]string{command}, bind...), "busybox")
			if status := runCommand(args, &stdout, &stderr, b); status != 1 {
				t.Errorf("%q without waiver status=%d, want 1", args, status)
			}
			if !strings.Contains(stderr.String(), "--waive-live-fork") || !strings.Contains(stderr.String(), ":lazy_copy") {
				t.Errorf("%q error=%q, want waiver and lazy_copy guidance", args, stderr.String())
			}
			stderr.Reset()
			args = append(append([]string{command, "--waive-live-fork"}, bind...), "busybox")
			if status := runCommand(args, &stdout, &stderr, b); status != 0 {
				t.Fatalf("%q status=%d: %s", args, status, stderr.String())
			}
			if mounts := b.runRequest.hostMounts; len(mounts) != 1 || !mounts[0].Direct {
				t.Errorf("%q host mounts=%+v, want one direct bind", args, mounts)
			}
			if b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver != "explicit waiver for a writable host bind" {
				t.Errorf("%q eligibility=(%t, %q), want the writable bind's waiver", args, b.runRequest.liveForkEligible, b.runRequest.liveForkWaiver)
			}
		}
		// The explicit modes keep live forks.
		for _, mode := range []string{"ro", "copy", "lazy_copy"} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{command, "-v", "/host:/data:" + mode, "busybox"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("%s :%s status=%d: %s", command, mode, status, stderr.String())
			}
			if !b.runRequest.liveForkEligible {
				t.Errorf("%s :%s waived live forks", command, mode)
			}
		}
	}
}

func TestRunAndCreateRejectImagePlatformOverride(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--platform", "linux/arm64", "busybox"},
		{"create", "--platform=linux/arm64", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
		if !strings.Contains(stderr.String(), "flag provided but not defined: -platform") {
			t.Errorf("runCommand(%q) stderr=%q, want unsupported platform flag", args, stderr.String())
		}
	}
}

func TestRunRejectsInvalidFilesystemMounts(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--tmpfs", "relative", "busybox"},
		{"run", "--tmpfs", "/scratch:mode=0899", "busybox"},
		{"run", "--tmpfs", "/scratch:size=zero", "busybox"},
		{"run", "--tmpfs", "/scratch:unknown", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,source=host", "busybox"},
		{"run", "--mount", "type=proc,target=relative", "busybox"},
		{"run", "--mount", "type=proc,target=/alt-proc,size=1m", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,tmpfs-size=2m", "busybox"},
		{"run", "--tmpfs", "/scratch:uid=-1", "busybox"},
		{"run", "--tmpfs", "/scratch:nr_blocks=10%", "busybox"},
		{"run", "--mount", "type=sysfs,target=/alt-sys,readonly=maybe", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,target=/again", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunForwardsContainerExitStatus(t *testing.T) {
	b := &fakeBackend{runStatus: 42}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"run", "busybox", "false"}, &stdout, &stderr, b); status != 42 {
		t.Fatalf("run status=%d, want 42; stderr=%q", status, stderr.String())
	}
}

func TestCreateAndStart(t *testing.T) {
	containerID := strings.Repeat("b", 64)
	b := &fakeBackend{containerID: containerID}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"create", "--name", "demo", "-it", "--rm", "--stop-signal", "SIGQUIT", "--stop-timeout", "4", "-h", "create-host", "--net", "host", "--waive-live-fork", "busybox", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("create status=%d, stderr=%q", status, stderr.String())
	}
	if !b.created || b.runRequest.name != "demo" || b.runRequest.pullPolicy != "missing" || b.runRequest.hostname != "create-host" || b.runRequest.networkMode != "host" || b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || b.runRequest.stopSignal != "SIGQUIT" || b.runRequest.stopTimeout != 4 || strings.Join(b.runRequest.args, " ") != "sleep 30" {
		t.Errorf("create request=%+v created=%t", b.runRequest, b.created)
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("create output=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"start", "demo@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("start status=%d, stderr=%q", status, stderr.String())
	}
	if b.started != (selector{container: "demo", branch: "main"}) || !b.startDetach || b.startInteractive {
		t.Errorf("start target=%v detach=%t interactive=%t", b.started, b.startDetach, b.startInteractive)
	}
	if stdout.String() != "demo@main\n" {
		t.Errorf("start output=%q", stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"start", containerID}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("start by container ID status=%d, stderr=%q", status, stderr.String())
	}
	if b.started != (selector{container: containerID, branch: defaultBranchID}) {
		t.Errorf("start by container ID target=%v", b.started)
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("start by container ID output=%q, want %q", got, want)
	}
}

func TestStartMultipleAndContainerAliases(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"container", "start", "one@main", "two@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("container start status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "one@main\ntwo@child\n"; got != want {
		t.Errorf("multi-start output=%q, want %q", got, want)
	}
	if got, want := b.startCalls, []startCall{{target: selector{container: "one", branch: "main"}, detach: true}, {target: selector{container: "two", branch: "child"}, detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("start calls=%+v, want %+v", got, want)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"start", "--attach", "one@main", "two@main"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("multi-start accepted --attach")
	}
	if len(b.startCalls) != 0 {
		t.Errorf("rejected attached multi-start made calls %+v", b.startCalls)
	}

	b = &fakeBackend{inspectInfo: InspectInfo{ID: "demo", Name: "/demo"}}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"container", "inspect", "-f", "{{.Id}}|{{.ID}}|{{json .}}", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("container inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); !strings.HasPrefix(got, "demo|demo|") || !strings.Contains(got, `"Id":"demo"`) || strings.Contains(got, `"ID":`) {
		t.Errorf("Docker and legacy ID template output=%q", got)
	}

	if status := runCommand([]string{"container", "unknown"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("container namespace accepted an unknown command")
	}
}

func TestAttach(t *testing.T) {
	b := &fakeBackend{waitStatus: uint32(7 << 8)}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"attach", "-i", "--exec", "exec-one", "--detach-keys", "ctrl-a,z", "demo@child"}, &stdout, &stderr, b)
	if status != 7 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if b.attached.target != (selector{container: "demo", branch: "child"}) || b.attached.execID != "exec-one" || !b.attached.stdin || !b.attached.stdout || !b.attached.stderr {
		t.Errorf("attach request=%+v", b.attached)
	}
	if got, want := b.attached.detachKeys, []byte{1, 'z'}; !bytes.Equal(got, want) {
		t.Errorf("attach detach keys=%v, want %v", got, want)
	}
}

func TestAttachStreamSelection(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stdin  bool
		stdout bool
		stderr bool
	}{
		{name: "output", args: []string{"--no-stdin"}, stdout: true, stderr: true},
		{name: "stdin-only", args: []string{"-i", "--no-stdout", "--no-stderr"}, stdin: true},
		{name: "stdout-only", args: []string{"--no-stderr"}, stdout: true},
		{name: "stderr-only", args: []string{"--no-stdout"}, stderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := append([]string{"attach"}, test.args...)
			args = append(args, "demo@main")
			if status := runCommand(args, &stdout, &stderr, b); status != 0 {
				t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
			}
			if b.attached.stdin != test.stdin || b.attached.stdout != test.stdout || b.attached.stderr != test.stderr {
				t.Errorf("attach streams=(stdin=%t stdout=%t stderr=%t), want (%t %t %t)",
					b.attached.stdin, b.attached.stdout, b.attached.stderr, test.stdin, test.stdout, test.stderr)
			}
		})
	}
	for _, args := range [][]string{
		{"attach", "--no-stdout", "--no-stderr", "demo@main"},
		{"attach", "-i", "--no-stdin", "demo@main"},
		{"attach", "-i", "--takeover", "demo@main"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.attached.target.container != "" {
			t.Errorf("rejected attach reached backend with %+v", b.attached)
		}
	}
}

func TestAttachDetachIsSuccess(t *testing.T) {
	b := &fakeBackend{err: errDetached}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"attach", "-i", "demo@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.attached.detachKeys, defaultDetachKeys; !bytes.Equal(got, want) {
		t.Errorf("default detach keys=%v, want %v", got, want)
	}
}

func TestParseDetachKeys(t *testing.T) {
	for _, test := range []struct {
		value string
		want  []byte
	}{
		{value: "ctrl-p,ctrl-q", want: []byte{0x10, 0x11}},
		{value: "CTRL-@,ctrl-[,ctrl-\\,ctrl-_,x", want: []byte{0, 27, 28, 31, 'x'}},
	} {
		got, err := parseDetachKeys(test.value)
		if err != nil {
			t.Errorf("parseDetachKeys(%q): %v", test.value, err)
		} else if !bytes.Equal(got, test.want) {
			t.Errorf("parseDetachKeys(%q)=%v, want %v", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "ctrl-", "ctrl-1", "too-long"} {
		if _, err := parseDetachKeys(value); err == nil {
			t.Errorf("parseDetachKeys(%q) succeeded", value)
		}
	}
}

func TestDetachKeyFilterAcrossReads(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte("hello\x10"))
	if got, want := string(output), "hello"; got != want || detached {
		t.Fatalf("first push=(%q, %t), want (%q, false)", got, detached, want)
	}
	output, detached = filter.push([]byte("\x11ignored"))
	if len(output) != 0 || !detached {
		t.Fatalf("second push=(%q, %t), want (empty, true)", output, detached)
	}
	if pending := filter.flush(); len(pending) != 0 {
		t.Errorf("flush after detach=%q, want empty", pending)
	}
}

func TestDetachKeyFilterForwardsNearMatchAndFlushesPrefix(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte{'a', 0x10, 'x', 0x10})
	if got, want := output, []byte{'a', 0x10, 'x'}; !bytes.Equal(got, want) || detached {
		t.Fatalf("push=(%v, %t), want (%v, false)", got, detached, want)
	}
	if got, want := filter.flush(), []byte{0x10}; !bytes.Equal(got, want) {
		t.Errorf("flush=%v, want %v", got, want)
	}
}

func TestLogs(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	since := "2026-07-17T01:02:03.123456789Z"
	until := "2026-07-17T02:03:04Z"
	if status := runCommand([]string{"logs", "--follow", "--exec", "exec-one", "--tail", "7", "--since", since, "--until", until, "--timestamps", "--details", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs status=%d, stderr=%q", status, stderr.String())
	}
	options := b.logsOptions
	if b.logsTarget != (selector{container: "demo", branch: "child"}) || options.ExecID != "exec-one" || !options.Follow || options.Tail != 7 || !options.Timestamps || !options.Details {
		t.Errorf("logs target=%+v options=%+v", b.logsTarget, options)
	}
	if options.Since == nil || options.Since.Format(time.RFC3339Nano) != since || options.Until == nil || options.Until.Format(time.RFC3339Nano) != until {
		t.Errorf("logs time options=%+v", options)
	}
	if status := runCommand([]string{"logs", "--tail", "invalid", "demo@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs with Docker-compatible invalid tail status=%d, stderr=%q", status, stderr.String())
	}
	if b.logsOptions.Tail != -1 {
		t.Errorf("invalid --tail selected %d, want all (-1)", b.logsOptions.Tail)
	}
	if status := runCommand([]string{"logs", "--since", "not-a-time", "demo@main"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("logs accepted an invalid --since value")
	}
}

// A family made and not started, being dehydrated or dehydrated has no
// process whose console a follower could follow: its logs are what it wrote,
// and following them ends there.
func TestLogsOfABranchWithoutAProcess(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\necho \"unexpected command: $*\" >&2\nexit 2\n"), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	for name, state := range map[string]string{"made": "created", "going": "dehydrating", "cold": "dehydrated"} {
		containerDir := filepath.Join(dataRoot, "containers", name)
		bundleDir := filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(bundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
			Name: name, BundleDir: bundleDir, State: state, CreatedAt: time.Now(),
			Branches: []ttcbundle.BranchInfo{{ID: "main", State: state}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"going", "cold"} {
		consoleDir := filepath.Join(dataRoot, "containers", name, "console")
		if err := os.MkdirAll(consoleDir, 0700); err != nil {
			t.Fatal(err)
		}
		log, err := os.Create(filepath.Join(consoleDir, "main.log"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ttcconsole.WriteFrame(log, ttcconsole.StreamStdout, []byte(name+"\n")); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, stdout: &stdout, stderr: &stderr}
	if err := b.logs(selector{container: "made", branch: "main"}, logsOptions{Tail: -1}); err != nil || stdout.Len() != 0 {
		t.Errorf("logs of a family made and not started: %v, %q; want none", err, stdout.String())
	}
	for _, name := range []string{"going", "cold"} {
		stdout.Reset()
		if err := b.logs(selector{container: name, branch: "main"}, logsOptions{Tail: -1, Follow: true}); err != nil || stdout.String() != name+"\n" {
			t.Errorf("logs of family %s, followed: %v, %q; want %q", name, err, stdout.String(), name+"\n")
		}
	}
}

func TestParseLogTime(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	tests := map[string]time.Time{
		"2026-07-17T01:02:03.123456789Z": time.Date(2026, 7, 17, 1, 2, 3, 123456789, time.UTC),
		"60.25":                          time.Unix(60, 250000000).UTC(),
		"-1.5":                           time.Unix(-1, -500000000).UTC(),
		"90m":                            now.Add(-90 * time.Minute),
	}
	for input, want := range tests {
		got, err := parseLogTime(input, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseLogTime(%q)=%v, %v; want %v", input, got, err, want)
		}
	}
}

func TestConsoleLogOutputFiltersAndPrefixesLines(t *testing.T) {
	since := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Second)
	output := newConsoleLogOutput(logsOptions{Since: &since, Until: &until, Timestamps: true, Details: true}, map[string]string{"zeta": "two", "alpha": "one"})
	var stdout, stderr bytes.Buffer
	frames := []ttcconsole.Frame{
		{Stream: ttcconsole.StreamStdout, Timestamp: since.Add(-time.Second), Data: []byte("old\n")},
		{Stream: ttcconsole.StreamStdout, Timestamp: since, Data: []byte("one\ntwo")},
		{Stream: ttcconsole.StreamStdout, Timestamp: since.Add(time.Second), Data: []byte("-continued\nthree\n")},
		{Stream: ttcconsole.StreamStderr, Timestamp: until, Data: []byte("problem\n")},
		{Stream: ttcconsole.StreamStderr, Timestamp: until.Add(time.Nanosecond), Data: []byte("new\n")},
	}
	for _, frame := range frames {
		if err := output.write(frame, &stdout, &stderr); err != nil {
			t.Fatalf("write(%+v): %v", frame, err)
		}
	}
	wantStdout := "2026-07-17T01:00:00Z alpha=one,zeta=two one\n2026-07-17T01:00:00Z alpha=one,zeta=two two-continued\n2026-07-17T01:00:01Z alpha=one,zeta=two three\n"
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout=%q, want %q", got, wantStdout)
	}
	if got, want := stderr.String(), "2026-07-17T01:00:02Z alpha=one,zeta=two problem\n"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogTailPreservesGlobalLineOrderAndStreams(t *testing.T) {
	tail := &consoleLogTail{limit: 3}
	stdout := tail.writer(ttcconsole.StreamStdout)
	stderr := tail.writer(ttcconsole.StreamStderr)
	stdout.Write([]byte("drop\nkeep-out"))
	stderr.Write([]byte("keep-err-1\nkeep-err-2\n"))
	stdout.Write([]byte("-continued"))

	var gotStdout, gotStderr bytes.Buffer
	if err := tail.flush(&gotStdout, &gotStderr); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, want := gotStdout.String(), "keep-out-continued"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
	if got, want := gotStderr.String(), "keep-err-1\nkeep-err-2\n"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerTailsAcrossRetainedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttcconsole.NewRotatingWriter(path, 34)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"drop\n", "keep-one\n", "keep-two\n"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	tail := &consoleLogTail{limit: 2}
	if _, err := follower.drain(tail.writer(ttcconsole.StreamStdout), tail.writer(ttcconsole.StreamStderr)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	var stdout bytes.Buffer
	if err := tail.flush(&stdout, nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, want := stdout.String(), "keep-one\nkeep-two\n"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerHandlesPartialAppend(t *testing.T) {
	var encoded bytes.Buffer
	for _, frame := range []struct {
		stream byte
		data   string
	}{
		{stream: ttcconsole.StreamStdout, data: "hello"},
		{stream: ttcconsole.StreamStderr, data: "problem"},
		{stream: ttcconsole.StreamStdout},
		{stream: ttcconsole.StreamStderr},
	} {
		if err := ttcconsole.WriteFrame(&encoded, frame.stream, []byte(frame.data)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "main.log")
	data := encoded.Bytes()
	if err := os.WriteFile(path, data[:5], 0600); err != nil {
		t.Fatalf("writing partial log: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining partial header: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("partial frame produced stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	appendFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("opening append log: %v", err)
	}
	if _, err := appendFile.Write(data[5:]); err != nil {
		appendFile.Close()
		t.Fatalf("appending log: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatalf("closing append log: %v", err)
	}
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining completed frames: progress=%t err=%v", progress, err)
	}
	if got := stdout.String(); got != "hello" {
		t.Errorf("stdout=%q, want hello", got)
	}
	if got := stderr.String(); got != "problem" {
		t.Errorf("stderr=%q, want problem", got)
	}
	if !follower.stdoutEOF || !follower.stderrEOF || len(follower.pending) != 0 {
		t.Errorf("follower state: stdoutEOF=%t stderrEOF=%t pending=%d", follower.stdoutEOF, follower.stderrEOF, len(follower.pending))
	}
}

func TestConsoleLogFollowerFollowsLiveRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttcconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte("third")); err != nil {
		t.Fatalf("WriteFrame(third): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if got, want := stdout.String(), "firstsecondthird"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr=%q, want empty", stderr.String())
	}
}

func TestConsoleLogFollowerReportsRetentionOverrun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttcconsole.NewRotatingWriterWithFiles(path, 10, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte("first")); err != nil {
		t.Fatalf("WriteFrame(first): %v", err)
	}
	follower, err := openConsoleLogFollower(path, 2)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	for _, value := range []string{"second", "third"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout bytes.Buffer
	_, err = follower.drain(&stdout, nil)
	if err == nil || !strings.Contains(err.Error(), "fell behind retention") {
		t.Fatalf("drain error=%v, want retention overrun", err)
	}
	if got, want := stdout.String(), "first"; got != want {
		t.Errorf("stdout before overrun=%q, want %q", got, want)
	}
}
func TestConsoleLogFollowerReplaysRetainedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttcconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second", "third"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStderr, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout=%q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "firstsecondthird"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerReplaysMultipleGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttcconsole.NewRotatingWriterWithFiles(path, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	for _, value := range []string{"drop", "two", "three", "four"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path, 3)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout bytes.Buffer
	if progress, err := follower.drain(&stdout, nil); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if got, want := stdout.String(), "twothreefour"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
}
func TestConsoleDrainHealth(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "console.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("opening lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("locking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || !active {
		t.Fatalf("active probe=(%t, %v), want (true, nil)", active, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlocking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || active {
		t.Fatalf("inactive probe=(%t, %v), want (false, nil)", active, err)
	}
	errorPath := filepath.Join(dir, "console.error.log")
	if err := os.WriteFile(errorPath, []byte("low-level failure\n"), 0600); err != nil {
		t.Fatalf("writing diagnostic: %v", err)
	}
	if err := consoleDrainStoppedError(errorPath); err == nil || !strings.Contains(err.Error(), "low-level failure") {
		t.Fatalf("consoleDrainStoppedError=%v", err)
	}
}

func TestExecParsesBranchAndDockerStyleOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	args := []string{"exec", "-it", "-e", "A=1", "--workdir", "/work", "--user", "123:456", "--additional-gids", "12", "--additional-gids", "34", "--cap", "CAP_CHOWN", "--cap", "CAP_SYS_PTRACE", "demo@child", "sh", "-c", "echo ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if request.target != (selector{container: "demo", branch: "child"}) || !request.interactive || !request.terminal || request.cwd != "/work" || request.user != "123:456" {
		t.Errorf("exec request=%+v", request)
	}
	if got := strings.Join(request.env, " "); got != "A=1" {
		t.Errorf("exec env=%q", got)
	}
	if got, want := request.additionalGIDs, []string{"12", "34"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exec additional GIDs=%v, want %v", got, want)
	}
	if got, want := request.capabilities, []string{"CAP_CHOWN", "CAP_SYS_PTRACE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exec capabilities=%v, want %v", got, want)
	}
	if got := strings.Join(request.args, " "); got != "sh -c echo ok" {
		t.Errorf("exec args=%q", got)
	}
	if !strings.HasPrefix(request.execID, "exec-") {
		t.Errorf("attached exec has no stable session ID: %q", request.execID)
	}
	if request.retain {
		t.Error("ordinary foreground exec unexpectedly retained its session")
	}
}

func TestExecSessionBeforeCombinedShortOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "--session", "build1", "-dit", "demo@main", "sh"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec status=%d, stderr=%q", status, stderr.String())
	}
	if b.execRequest.execID != "build1" || !b.execRequest.detach || !b.execRequest.interactive || !b.execRequest.terminal || !b.execRequest.retain {
		t.Fatalf("exec request=%+v", b.execRequest)
	}
}

func TestExecProcessFileControlsCompleteProcess(t *testing.T) {
	dir := t.TempDir()
	processPath := filepath.Join(dir, "process.json")
	data, err := json.Marshal(specs.Process{
		Terminal: true,
		Args:     []string{"sh", "-c", "echo process"},
		Cwd:      "/work",
		User:     specs.User{UID: 123, GID: 456, AdditionalGids: []uint32{12, 34}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(processPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "-di", "--process", processPath, "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec --process status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if request.processPath != processPath || !request.detach || !request.interactive || !request.terminal || len(request.args) != 0 {
		t.Fatalf("exec --process request=%+v", request)
	}
	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"exec", "--process", processPath, "--env", "A=1", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("exec --process accepted a CLI process override")
	}
	if b.execRequest.processPath != "" {
		t.Fatalf("rejected --process invocation reached backend: %+v", b.execRequest)
	}
}

func TestExecDetachReturnsQualifiedSessionID(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "-dit", "demo@child", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("detached exec status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if !request.detach || !request.interactive || !request.terminal || !request.retain || !strings.HasPrefix(request.execID, "exec-") {
		t.Fatalf("detached exec request=%+v", request)
	}
	want := "demo@child:" + request.execID + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("detached exec output=%q, want %q", got, want)
	}
}

func TestExecNamedDurableSession(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "--session", "build1", "-d", "demo@child", "make"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("named detached exec status=%d, stderr=%q", status, stderr.String())
	}
	if got := b.execRequest.execID; got != "build1" {
		t.Fatalf("exec session=%q, want build1", got)
	}
	if !b.execRequest.retain {
		t.Fatal("named exec did not retain its session")
	}
	if got, want := stdout.String(), "demo@child:build1\n"; got != want {
		t.Fatalf("detached exec output=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"exec", "--session", "../escape", "-d", "demo@child", "true"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("exec accepted an unsafe session name")
	}
}

func TestExecSessionControls(t *testing.T) {
	created := time.Date(2026, time.July, 17, 2, 3, 4, 0, time.UTC)
	b := &fakeBackend{execSessionList: []runscExecSession{{
		ID: "exec-abc", PID: 7, State: "running", Terminal: true, CreatedAt: created,
	}}, waitStatus: uint32(7 << 8)}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "ls", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec ls status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "exec-abc") || !strings.Contains(got, "running") {
		t.Fatalf("exec ls output=%q", got)
	}
	stdout.Reset()
	if status := runCommand([]string{"exec", "wait", "demo@child:exec-abc"}, &stdout, &stderr, b); status != 0 || stdout.String() != "7\n" {
		t.Fatalf("exec wait status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"exec", "kill", "--signal", "TERM", "demo@child:exec-abc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec kill status=%d, stderr=%q", status, stderr.String())
	}
	if b.execControlTarget != (selector{container: "demo", branch: "child"}) || b.execControlID != "exec-abc" || b.execControlSignal != 15 {
		t.Fatalf("exec control target=%+v id=%q signal=%d", b.execControlTarget, b.execControlID, b.execControlSignal)
	}
	if stdout.String() != "demo@child:exec-abc\n" {
		t.Fatalf("exec kill output=%q", stdout.String())
	}
}

func TestDiffHumanAndJSON(t *testing.T) {
	b := &fakeBackend{diffChanges: []BranchPathChange{
		{Path: "/work/new", Kind: "add", Type: "regular", Mount: "/"},
		{Path: "/work/old", Kind: "delete", Type: "regular", Mount: "/"},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"diff", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("diff status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "A /work/new\nD /work/old\n"; got != want {
		t.Fatalf("diff output=%q, want %q", got, want)
	}
	if b.processTarget != (selector{container: "demo", branch: "child"}) {
		t.Fatalf("diff target=%+v", b.processTarget)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"diff", "--json", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("diff --json status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"path": "/work/new"`) || !strings.Contains(stdout.String(), `"kind": "add"`) {
		t.Fatalf("diff JSON=%q", stdout.String())
	}
}

func TestSyntheticRootfsMountChange(t *testing.T) {
	spec := &specs.Spec{Mounts: []specs.Mount{
		{Destination: "/proc"},
		{Destination: "/data"},
		{Destination: "/etc/hostname"},
	}}
	tests := []struct {
		name            string
		repositoryMount string
		repositoryPath  string
		want            bool
	}{
		{name: "root entry", repositoryMount: "/", repositoryPath: "/", want: true},
		{name: "runtime proc mount", repositoryMount: "/", repositoryPath: "/proc", want: true},
		{name: "user data mount", repositoryMount: "/", repositoryPath: "/data/", want: true},
		{name: "ordinary rootfs path", repositoryMount: "/", repositoryPath: "/root", want: false},
		{name: "mounted repository root", repositoryMount: "/data", repositoryPath: "/", want: false},
		{name: "mounted repository child", repositoryMount: "/data", repositoryPath: "/file", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := syntheticRootfsMountChange(spec, test.repositoryMount, test.repositoryPath); got != test.want {
				t.Fatalf("syntheticRootfsMountChange(%q, %q) = %t, want %t", test.repositoryMount, test.repositoryPath, got, test.want)
			}
		})
	}
}

func TestParseExecSelectorRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "demo", "demo:", "demo:../bad", "demo@child:-bad"} {
		if _, err := parseExecSelector(value); err == nil {
			t.Errorf("parseExecSelector(%q) succeeded", value)
		}
	}
}

func TestExecForwardsProcessExitStatus(t *testing.T) {
	b := &fakeBackend{execStatus: 23}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "demo@main", "false"}, &stdout, &stderr, b); status != 23 {
		t.Fatalf("exec status=%d, want 23; stderr=%q", status, stderr.String())
	}
}

func TestInspect(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{
		ID: "demo", Name: "/demo", Image: "sha256:abc",
		State:  InspectState{Status: "running", Running: true},
		Config: InspectConfig{Image: "docker.io/library/busybox:latest"},
		TTC: TTCInspectInfo{Branches: []InspectBranch{{
			Metadata: Branch{ID: "main", State: "running"},
		}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"inspect", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("inspect status=%d, stderr=%q", status, stderr.String())
	}
	var infos []InspectInfo
	if err := json.Unmarshal(stdout.Bytes(), &infos); err != nil {
		t.Fatalf("decoding inspect output %q: %v", stdout.String(), err)
	}
	if len(infos) != 1 || infos[0].ID != "demo" || infos[0].State.Status != "running" || infos[0].Config.Image != "docker.io/library/busybox:latest" {
		t.Errorf("inspect output=%+v", infos)
	}

	stdout.Reset()
	if status := runCommand([]string{"inspect", "--format", `{{.Name}}|{{.State.Status}}|{{len .TTC.Branches}}`, "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "/demo|running|1\n"; got != want {
		t.Errorf("formatted inspect=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"inspect", "--fork-check", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted the removed --fork-check flag")
	}

	if status := runCommand([]string{"inspect", "--type", "image", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted unsupported object type")
	}
	if status := runCommand([]string{"inspect", "demo@main"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted branch selector")
	}
	if status := runCommand([]string{"inspect", "--format", "{{", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted invalid template")
	}
}

func TestPublishedPortStateUsesActiveRoot(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	backend := &runtimeBackend{dataRoot: shared, localRoot: local}
	binding := ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"}
	lockPath, logPath := portSidecarPaths(backend.activeDataRoot(), "container", binding)
	for _, path := range []string{lockPath, logPath} {
		if !strings.HasPrefix(path, local+string(os.PathSeparator)) {
			t.Fatalf("sidecar path %q is not below active root %q", path, local)
		}
		if strings.HasPrefix(path, shared+string(os.PathSeparator)) {
			t.Fatalf("sidecar path %q leaked into shared root %q", path, shared)
		}
	}
}

func TestBuildInspectInfo(t *testing.T) {
	containerID := strings.Repeat("c", 64)
	created := time.Date(2026, 7, 17, 1, 2, 3, 4, time.UTC)
	started := created.Add(time.Second)
	exit := 23
	stopTimeout := 4
	pidsLimit := int64(23)
	memoryLimit := int64(256 * 1024 * 1024)
	nanoCPUs := int64(1_500_000_000)
	cpuShares := uint64(512)
	cpuPeriod := uint64(100000)
	cpuQuota := int64(150000)
	shmSize := int64(32 * 1024 * 1024)
	groupAdd := []string{"wheel", "4242"}
	capAdd := []string{"SYS_ADMIN"}
	capDrop := []string{"NET_RAW"}
	ulimits := []ttcbundle.Ulimit{{Name: "core", Soft: 0, Hard: 0}, {Name: "nofile", Soft: 64, Hard: 128}}
	extraHosts := []string{"db:192.0.2.10", "v6:2001:db8::10"}
	dns := []string{"203.0.113.53"}
	dnsSearch := []string{"example.test"}
	dnsOptions := []string{"ndots:2", "rotate"}
	container := ttcbundle.ContainerInfo{
		ID: containerID, Name: "demo", Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abc", Platform: "linux/amd64",
		RootFS: "/roots/alpine", NetworkMode: "host", LogDriver: "json-file", ConsoleLogMaxBytes: 65536, ConsoleLogMaxFiles: 3, AutoRemove: true, StopSignal: "SIGUSR1", StopTimeout: &stopTimeout, PidsLimit: &pidsLimit, MemoryLimit: &memoryLimit, NanoCPUs: &nanoCPUs, CPUShares: &cpuShares, CPUPeriod: &cpuPeriod, CPUQuota: &cpuQuota, ShmSize: &shmSize, GroupAdd: groupAdd, CapAdd: capAdd, CapDrop: capDrop, Ulimits: ulimits, ExtraHosts: extraHosts, DNS: dns, DNSSearch: dnsSearch, DNSOptions: dnsOptions, State: "running", ExitStatus: &exit, CreatedAt: created, StartedAt: &started,
	}
	spec := &specs.Spec{
		Root: &specs.Root{Readonly: true},
		Process: &specs.Process{
			Args: []string{"/bin/sh", "-c", "sleep 10"}, Env: []string{"A=1"}, Cwd: "/work",
			User: specs.User{UID: 123, GID: 456},
		},
		Mounts: []specs.Mount{
			{Type: "proc", Source: "proc", Destination: "/proc"},
			{Type: "bind", Source: "/host/data", Destination: "/data", Options: []string{"rbind", "ro"}},
			{Type: "bind", Source: "/state/network/hosts", Destination: "/etc/hosts", Options: []string{"rbind", "ro"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
			{Type: "proc", Source: "proc", Destination: "/alt-proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Source: "sysfs", Destination: "/alt-sys", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
		Hostname: "custom-host",
		Annotations: map[string]string{
			"dev.timetravelcontainer.config.entrypoint": `["/bin/sh"]`,
			"dev.timetravelcontainer.config.cmd":        `["-c","sleep 10"]`,
			"dev.timetravelcontainer.config.user":       "app",
			"dev.timetravelcontainer.config.labels":     `{"suite":"ttc"}`,
			"dev.timetravelcontainer.config.tmpfs":      `{"/scratch":"mode=0700,size=1m"}`,
			ttcbundle.InternalMountsAnnotation:          `["/etc/hosts"]`,
		},
	}
	family := &Family{ID: containerID, Name: "demo", State: "running", SandboxPID: 4321, Branches: []Branch{
		{ID: "child", ParentID: "main", State: "running"},
		{ID: "main", State: "paused"},
	}}
	info := buildInspectInfo(container, spec, family, "")
	if info.ID != containerID || info.Name != "/demo" || info.Path != "/bin/sh" || !reflect.DeepEqual(info.Args, []string{"-c", "sleep 10"}) {
		t.Errorf("identity/process view=%+v", info)
	}
	if info.State.Status != "paused" || !info.State.Running || !info.State.Paused || info.State.Pid != 4321 || info.State.ExitCode != 23 {
		t.Errorf("state=%+v", info.State)
	}
	if info.Config.Hostname != "custom-host" || info.Config.User != "app" || info.Config.WorkingDir != "/work" || info.Config.Image != container.Reference {
		t.Errorf("config=%+v", info.Config)
	}
	if !reflect.DeepEqual(info.Config.Entrypoint, []string{"/bin/sh"}) || !reflect.DeepEqual(info.Config.Cmd, []string{"-c", "sleep 10"}) || info.Config.Labels["suite"] != "ttc" {
		t.Errorf("Docker config reconstruction=%+v", info.Config)
	}
	if len(info.Mounts) != 4 || info.Mounts[0].Type != "bind" || info.Mounts[0].RW || info.Mounts[1].Type != "tmpfs" || !info.Mounts[1].RW || info.Mounts[2].Type != "proc" || info.Mounts[2].RW || info.Mounts[3].Type != "sysfs" || info.Mounts[3].RW {
		t.Errorf("mounts=%+v", info.Mounts)
	}
	if got := info.TTC.ExternalViews; len(got) != 1 || got[0].Source != "/host/data" || got[0].Destination != "/data" || !got[0].Readonly || got[0].Semantics != "shared-external-view" {
		t.Errorf("external views=%+v", got)
	}

	if got := info.HostConfig.Binds; !reflect.DeepEqual(got, []string{"/host/data:/data:ro"}) {
		t.Errorf("binds=%q", got)
	}
	if got := info.HostConfig.Tmpfs["/scratch"]; got != "mode=0700,size=1m" {
		t.Errorf("tmpfs inspect option=%q", got)
	}
	if !info.HostConfig.ReadonlyRootfs {
		t.Error("inspect omitted read-only rootfs policy")
	}
	if info.Config.StopSignal != "SIGUSR1" || info.HostConfig.StopTimeout == nil || *info.HostConfig.StopTimeout != 4 {
		t.Errorf("inspect stop policy=(%q, %v), want (SIGUSR1, 4)", info.Config.StopSignal, info.HostConfig.StopTimeout)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != pidsLimit {
		t.Errorf("inspect pids limit=%v, want %d", info.HostConfig.PidsLimit, pidsLimit)
	}
	if info.HostConfig.Memory != memoryLimit || info.HostConfig.NanoCPUs != nanoCPUs || info.HostConfig.CPUShares != cpuShares || info.HostConfig.CPUPeriod != cpuPeriod || info.HostConfig.CPUQuota != cpuQuota {
		t.Errorf("inspect resources=%+v", info.HostConfig)
	}
	if !reflect.DeepEqual(info.HostConfig.GroupAdd, groupAdd) || !reflect.DeepEqual(info.HostConfig.CapAdd, capAdd) || !reflect.DeepEqual(info.HostConfig.CapDrop, capDrop) {
		t.Errorf("inspect groups/capabilities=(%q, %q, %q)", info.HostConfig.GroupAdd, info.HostConfig.CapAdd, info.HostConfig.CapDrop)
	}
	if info.HostConfig.ShmSize != shmSize {
		t.Errorf("inspect shared memory size=%d, want %d", info.HostConfig.ShmSize, shmSize)
	}
	if !reflect.DeepEqual(info.HostConfig.Ulimits, ulimits) {
		t.Errorf("inspect ulimits=%+v, want %+v", info.HostConfig.Ulimits, ulimits)
	}
	if !info.HostConfig.AutoRemove {
		t.Error("inspect omitted auto-remove policy")
	}
	if info.HostConfig.LogConfig.Type != "json-file" || info.HostConfig.LogConfig.Config["max-size"] != "65536" || info.HostConfig.LogConfig.Config["max-file"] != "3" {
		t.Errorf("log config=%+v", info.HostConfig.LogConfig)
	}
	if info.HostConfig.NetworkMode != "host" {
		t.Errorf("network mode=%q, want host", info.HostConfig.NetworkMode)
	}
	if !reflect.DeepEqual(info.HostConfig.ExtraHosts, extraHosts) || !reflect.DeepEqual(info.HostConfig.DNS, dns) || !reflect.DeepEqual(info.HostConfig.DNSSearch, dnsSearch) || !reflect.DeepEqual(info.HostConfig.DNSOptions, dnsOptions) {
		t.Errorf("inspect network configuration=(%q, %q, %q, %q)", info.HostConfig.ExtraHosts, info.HostConfig.DNS, info.HostConfig.DNSSearch, info.HostConfig.DNSOptions)
	}
	var networkTemplate bytes.Buffer
	if err := writeInspectFormat(&networkTemplate, []InspectInfo{info}, `{{index .HostConfig.Dns 0}}|{{index .HostConfig.DnsSearch 0}}|{{index .HostConfig.DnsOptions 0}}|{{.HostConfig.ShmSize}}`); err != nil {
		t.Fatalf("Docker DNS inspect template: %v", err)
	}
	if got, want := networkTemplate.String(), "203.0.113.53|example.test|ndots:2|33554432\n"; got != want {
		t.Errorf("Docker DNS inspect template=%q, want %q", got, want)
	}
	var logTemplate bytes.Buffer
	if err := writeInspectFormat(&logTemplate, []InspectInfo{info}, `{{.HostConfig.LogConfig.Type}}|{{index .HostConfig.LogConfig.Config "max-size"}}|{{index .HostConfig.LogConfig.Config "max-file"}}`); err != nil {
		t.Fatalf("Docker log-config inspect template: %v", err)
	}
	if got, want := logTemplate.String(), "json-file|65536|3\n"; got != want {
		t.Errorf("Docker log-config inspect template=%q, want %q", got, want)
	}

	if len(info.TTC.Branches) != 2 || info.TTC.Branches[0].Metadata.ID != "child" {
		t.Errorf("TTC branches=%+v", info.TTC.Branches)
	}
}

func TestInternalAutoRemoveNowForcesRemoval(t *testing.T) {
	b := &fakeBackend{
		waitStatus: uint32(7 << 8),
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
			Metadata: Branch{ID: "main", State: "exited"},
		}}}},
	}
	if err := runAutoRemoveNow([]string{"demo@main"}, b); err != nil {
		t.Fatalf("runAutoRemoveNow: %v", err)
	}
	if b.removed != "demo" || !b.removeForce || !b.removeVolumes {
		t.Errorf("remove=(%q, force=%t, volumes=%t)", b.removed, b.removeForce, b.removeVolumes)
	}
	if err := runAutoRemoveNow([]string{"demo"}, b); err == nil {
		t.Fatal("internal auto-remove accepted an unqualified family")
	}
}

func TestInternalAutoRemovePreservesForkedPeers(t *testing.T) {
	b := &fakeBackend{
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: "main", State: "exited"}},
			{Metadata: Branch{ID: "child", State: "running"}},
		}}},
	}
	if err := runAutoRemoveNow([]string{"demo@main"}, b); err != nil {
		t.Fatalf("runAutoRemoveNow: %v", err)
	}
	if b.destroyed != (selector{container: "demo", branch: "main"}) || !b.destroyForce {
		t.Errorf("destroyed=%+v force=%t", b.destroyed, b.destroyForce)
	}
	if b.removed != "" {
		t.Errorf("removed family %q despite live peer", b.removed)
	}
}

func TestRemove(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rm", "--force", "--volumes", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.removed != "demo" || !b.removeForce || !b.removeVolumes || stdout.String() != "demo\n" {
		t.Errorf("removed=%q force=%t volumes=%t output=%q", b.removed, b.removeForce, b.removeVolumes, stdout.String())
	}
}

func TestListJSON(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ls", "--json"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("status=%d, stderr=%q", status, stderr.String())
	}
	want := `"id": "web"`
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("JSON output %q does not contain %q", stdout.String(), want)
	}
}

func TestKillParsesSignalAndSelectsBranch(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"kill", "--signal", "SIGTERM", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("kill status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "child"}) || b.signals[0].signal != 15 || b.signals[0].all {
		t.Errorf("signal calls=%+v", b.signals)
	}
	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"kill", "web@child", "SIGUSR1"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("positional kill status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "child"}) || b.signals[0].signal != 10 {
		t.Errorf("positional signal calls=%+v", b.signals)
	}
}

func TestStopGraceful(t *testing.T) {
	timeout := 3
	b := &fakeBackend{inspectInfo: InspectInfo{
		Config:     InspectConfig{StopSignal: "SIGUSR1"},
		HostConfig: InspectHostConfig{StopTimeout: &timeout},
		TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: "running"}}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].signal != 10 {
		t.Errorf("signal calls=%+v, want configured SIGUSR1 only", b.signals)
	}
	if b.waited != (selector{container: "web", branch: "child"}) {
		t.Errorf("wait target=%+v", b.waited)
	}
	want := selector{container: "web", branch: "child"}
	if b.preparedStop != want || b.discarded != want {
		t.Errorf("prepare=%+v discard=%+v, want %+v", b.preparedStop, b.discarded, want)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	timeout := 0
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGUSR2"},
			HostConfig: InspectHostConfig{StopTimeout: &timeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 12 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want configured SIGUSR2 then SIGKILL", b.signals)
	}
}

func TestStopAcceptsGracefulExitRacingWithKill(t *testing.T) {
	timeout := 0
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		killErr:     errors.New("control socket closed"),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGTERM"},
			HostConfig: InspectHostConfig{StopTimeout: &timeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop race status=%d, stderr=%q", status, stderr.String())
	}
}

func TestStopTimeoutOverride(t *testing.T) {
	configuredTimeout := 30
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGTERM"},
			HostConfig: InspectHostConfig{StopTimeout: &configuredTimeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "--time", "0", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop override status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 15 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want SIGTERM then immediate SIGKILL", b.signals)
	}
}

func TestWaitPrintsDockerExitCodesInArgumentOrder(t *testing.T) {
	main := selector{container: "web", branch: "main"}
	child := selector{container: "web", branch: "child"}
	b := &fakeBackend{waitStatuses: map[selector]uint32{
		main:  uint32(7 << 8),
		child: uint32(syscall.SIGTERM),
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"container", "wait", "web@main", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("wait status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "7\n143\n"; got != want {
		t.Errorf("wait output=%q, want %q", got, want)
	}
	b.mu.Lock()
	calls := append([]selector(nil), b.waitCalls...)
	b.mu.Unlock()
	if len(calls) != 2 || !containsSelector(calls, main) || !containsSelector(calls, child) {
		t.Errorf("wait calls=%+v", calls)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"wait", "--condition", "removed", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("wait accepted unsupported removed condition")
	}
}

func TestWaitDurableAcceptsAuthoritativeStandaloneLocalState(t *testing.T) {
	b := &authoritativeWaitBackend{
		fakeBackend: &fakeBackend{
			localDurable: true,
			inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
				Metadata: Branch{ID: "child", State: "dehydrated", StorageState: "local"},
			}}}},
		},
		// runtimeBackend has this method even in standalone mode. Its false
		// result means that the authority is inapplicable, not that local durability can
		// never converge.
		durable: false,
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"wait", "--condition", "durable", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("wait durable status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@child\n"; got != want {
		t.Fatalf("wait durable output=%q, want %q", got, want)
	}
}

// A live branch has no durable state, so waiting for it fails at once rather
// than never returning.
func TestWaitDurableRefusesALiveBranch(t *testing.T) {
	for _, state := range []string{"running", "paused"} {
		b := &fakeBackend{
			localDurable: true,
			inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
				Metadata: Branch{ID: "child", State: state, StorageState: "local"},
			}}}},
		}
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- runCommand([]string{"wait", "--condition", "durable", "web@child"}, &stdout, &stderr, b)
		}()
		select {
		case status := <-done:
			if status == 0 || !strings.Contains(stderr.String(), "only a stopped or dehydrated branch") {
				t.Fatalf("%s: wait durable status=%d stderr=%q, want a refusal", state, status, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: wait durable on a live branch did not return", state)
		}
	}
}

// A family target pauses its running branches and resumes its paused ones,
// as the help for pause and resume says.
func TestPauseAndResumeAFamily(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "held", State: "paused"}},
		{Metadata: Branch{ID: "done", State: "exited"}},
		{Metadata: Branch{ID: "work", State: "running"}},
	}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"pause", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pause web status=%d stderr=%q", status, stderr.String())
	}
	if want := []selector{{container: "web", branch: "main"}, {container: "web", branch: "work"}}; !reflect.DeepEqual(b.pausedAll, want) {
		t.Fatalf("paused %+v, want the running branches %+v", b.pausedAll, want)
	}
	if status := runCommand([]string{"resume", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("resume web status=%d stderr=%q", status, stderr.String())
	}
	if want := []selector{{container: "web", branch: "held"}}; !reflect.DeepEqual(b.resumedAll, want) {
		t.Fatalf("resumed %+v, want the paused branch %+v", b.resumedAll, want)
	}
}

type authoritativeWaitBackend struct {
	*fakeBackend
	durable bool
}

func (b *authoritativeWaitBackend) authoritativeBranchDurable(selector) (bool, error) {
	return b.durable, nil
}

func TestWaitDurableUsesClusterAuthorityInsteadOfStaleLocalMetadata(t *testing.T) {
	b := &authoritativeWaitBackend{
		fakeBackend: &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
			Metadata: Branch{ID: "child", State: "dehydrating", StorageState: "local"},
		}}}}},
		durable: true,
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"wait", "--condition", "durable", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("wait durable status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@child\n"; got != want {
		t.Fatalf("wait durable output=%q, want %q", got, want)
	}
}

// A fork of a dehydrated branch away from the node that holds its capture
// waits for the branch's publication to write its image to the shared store,
// as a remote fork's target reads it from there, and says it waited.
func TestForkOfAPublishingBranchWaitsForItsImage(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "containers", "web", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{ID: "main", State: "dehydrated", StorageState: "publishing", PublicationID: "publication-2", CreatedAt: time.Now()}
	info := ttcbundle.ContainerInfo{ID: "web", Name: "web", BundleDir: bundleDir, Branches: []ttcbundle.BranchInfo{branch}}
	if err := writeCurrentBundleFixture(filepath.Join(root, "containers", "web", "container.json"), info); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{dataRoot: root, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard,
		distributedSource: &distributedSourceRuntime{authority: &cluster.AuthorityCatalog{}}}
	target := selector{container: "web", branch: "main"}
	store := ttcbundle.Store{Root: root}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = store.CompleteBranchPublication("web", "main", "publication-2", filepath.Join(root, "manifest.json"), "", false)
	}()
	started := time.Now()
	waited, err := b.awaitSharedDehydratedSource(info, target, target, branch)
	if err != nil || !waited {
		t.Fatalf("awaitSharedDehydratedSource = %t, %v; want it to wait for the image", waited, err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("awaitSharedDehydratedSource returned after %v, before the image was shared", elapsed)
	}
	// Shared, the image is forked at once.
	shared, err := store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	record, err := persistentBranchInfo(shared, "main")
	if err != nil {
		t.Fatal(err)
	}
	if waited, err := b.awaitSharedDehydratedSource(shared, target, target, record); err != nil || waited {
		t.Fatalf("awaitSharedDehydratedSource of a shared image = %t, %v; want no wait", waited, err)
	}
}

// movedStopBackend's branch was rehydrated on another node while a stop
// waited for its dehydration.
type movedStopBackend struct {
	*fakeBackend
	moved []selector
}

func (b *movedStopBackend) authoritativeBranchDurable(target selector) (bool, error) {
	return false, fmt.Errorf("%s is running: only a stopped or dehydrated branch has durable state to wait for", target)
}

func (b *movedStopBackend) branchMovedAway(target selector) bool {
	b.moved = append(b.moved, target)
	return true
}

// A stop of a dehydrating branch that another node rehydrates meanwhile has
// done nothing, and asks whether the branch has moved, for its daemon to send
// the stop where it went.
func TestStopOfABranchRehydratedElsewhereAsksWhereItWent(t *testing.T) {
	b := &movedStopBackend{fakeBackend: &fakeBackend{}}
	target := branchOperationTarget{selector: selector{container: "web", branch: "child"}, state: "dehydrating"}
	if err := stopBranchLifecycle(target, 15, 10, b, true); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Fatalf("stopBranchLifecycle = %v, want the wait's failure", err)
	}
	if len(b.moved) != 1 || b.moved[0] != target.selector {
		t.Fatalf("asked whether %v moved, want %v", b.moved, target.selector)
	}
}

// A stop of a dehydrating branch waits for the branch's publication for so
// long, then gives up rather than hold the command for as long as the
// publication retries.
func TestWaitForDurableBranchUntilGivesUpAtItsDeadline(t *testing.T) {
	b := &authoritativeWaitBackend{fakeBackend: &fakeBackend{}}
	target := selector{container: "web", branch: "child"}
	started := time.Now()
	result := waitForDurableBranchUntil(target, b, started.Add(200*time.Millisecond))
	if result.err == nil || !strings.Contains(result.err.Error(), "still under way") {
		t.Fatalf("waitForDurableBranchUntil = %v, want the publication still under way", result.err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("waitForDurableBranchUntil gave up after %v, want its deadline", elapsed)
	}
	b.durable = true
	if result := waitForDurableBranchUntil(target, b, time.Now().Add(time.Minute)); result.err != nil {
		t.Fatalf("waitForDurableBranchUntil of a durable branch = %v", result.err)
	}
}

func containsSelector(values []selector, target selector) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStopPausedAndDehydratedDiscardsWithoutResume(t *testing.T) {
	for _, state := range []string{"paused", "dehydrated"} {
		t.Run(state, func(t *testing.T) {
			b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: state}}}}}}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("stop status=%d stderr=%q", status, stderr.String())
			}
			if len(b.signals) != 0 || b.resumed.container != "" || b.preparedStop.container != "" {
				t.Fatalf("stop %s signaled/resumed/prepared: signals=%+v resumed=%+v prepared=%+v", state, b.signals, b.resumed, b.preparedStop)
			}
			if b.discarded != (selector{container: "web", branch: "child"}) {
				t.Fatalf("discarded=%+v", b.discarded)
			}
			wantPrepared := 0
			if state == "paused" {
				wantPrepared = 1
			}
			if got := len(b.preparedPausedStops); got != wantPrepared {
				t.Fatalf("paused filesystem preparations=%+v, want %d", b.preparedPausedStops, wantPrepared)
			}
		})
	}
}

func TestStopDehydratingWaitsForSealedFilesystemPublication(t *testing.T) {
	fake := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "child", State: "dehydrating", StorageState: "publishing"}},
	}}}}
	b := &authoritativeWaitBackend{
		fakeBackend: fake,
		durable:     true,
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d stderr=%q", status, stderr.String())
	}
	if b.discarded != (selector{container: "web", branch: "child"}) {
		t.Fatalf("discarded=%+v", b.discarded)
	}
}

func TestStopFamilyAppliesBranchStateSemantics(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "paused", State: "paused"}},
		{Metadata: Branch{ID: "cold", State: "dehydrated"}},
		{Metadata: Branch{ID: "done", State: "exited"}},
	}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop family status=%d stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web\n"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "main"}) {
		t.Fatalf("family stop signals=%+v", b.signals)
	}
	wantDiscards := []selector{{container: "web", branch: "cold"}, {container: "web", branch: "done"}, {container: "web", branch: "main"}, {container: "web", branch: "paused"}}
	if !reflect.DeepEqual(b.discards, wantDiscards) {
		t.Fatalf("family stop discards=%+v, want %+v", b.discards, wantDiscards)
	}
	if b.resumed.container != "" {
		t.Fatalf("family stop resumed %+v", b.resumed)
	}
}

func TestDehydrateAcceptsBranchOrFamily(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "paused", State: "paused"}},
		{Metadata: Branch{ID: "cold", State: "dehydrated"}},
	}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"dehydrate", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("dehydrate family status=%d stderr=%q", status, stderr.String())
	}
	want := []selector{{container: "web", branch: "main"}, {container: "web", branch: "paused"}}
	if !reflect.DeepEqual(b.dehydratedTargets, want) {
		t.Fatalf("dehydrated=%+v, want %+v", b.dehydratedTargets, want)
	}
	b.dehydratedTargets = nil
	if status := runCommand([]string{"dehydrate", "web@paused"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("dehydrate branch status=%d stderr=%q", status, stderr.String())
	}
	if !reflect.DeepEqual(b.dehydratedTargets, []selector{{container: "web", branch: "paused"}}) {
		t.Fatalf("branch dehydration=%+v", b.dehydratedTargets)
	}
}

func TestRestartRunningExitedPausedAndDehydratedBranches(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		wantSignal  bool
		wantDiscard bool
	}{
		{name: "running", state: "running", wantSignal: true, wantDiscard: true},
		{name: "exited", state: "exited"},
		{name: "paused", state: "paused", wantDiscard: true},
		{name: "dehydrated", state: "dehydrated", wantDiscard: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeout := 1
			b := &fakeBackend{inspectInfo: InspectInfo{
				Config:     InspectConfig{StopSignal: "SIGQUIT"},
				HostConfig: InspectHostConfig{StopTimeout: &timeout},
				TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: test.state}}}},
			}}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"restart", "web@child"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("restart status=%d, stderr=%q", status, stderr.String())
			}
			if got, want := stdout.String(), "web@child\n"; got != want {
				t.Errorf("restart output=%q, want %q", got, want)
			}
			wantTarget := selector{container: "web", branch: "child"}
			if got, want := b.startCalls, []startCall{{target: wantTarget, detach: true}}; !reflect.DeepEqual(got, want) {
				t.Errorf("start calls=%+v, want %+v", got, want)
			}
			if got := len(b.signals) != 0; got != test.wantSignal {
				t.Errorf("signals=%+v, wantSignal=%t", b.signals, test.wantSignal)
			}
			if test.wantSignal && (len(b.signals) != 1 || b.signals[0].target != wantTarget || b.signals[0].signal != 3) {
				t.Errorf("restart signals=%+v, want configured child SIGQUIT", b.signals)
			}
			if test.wantDiscard != (b.discarded == wantTarget) {
				t.Errorf("discarded=%+v, wantDiscard=%t", b.discarded, test.wantDiscard)
			}
			if b.resumed.container != "" {
				t.Errorf("restart resumed branch: %+v", b.resumed)
			}
		})
	}

	peerInfo := InspectInfo{
		TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: "main", State: "exited"}},
			{Metadata: Branch{ID: "child", State: "running"}},
		}},
	}
	b := &fakeBackend{inspectInfo: peerInfo}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"restart", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restart with active peer status=%d stderr=%q", status, stderr.String())
	}
	if got, want := b.startCalls, []startCall{{target: selector{container: "web", branch: "main"}, detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("restart starts=%+v, want %+v", got, want)
	}

	b = &fakeBackend{inspectInfo: InspectInfo{
		TTC: TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: defaultBranchID, State: "exited"}}}},
	}}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restart", "--force", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restart main container status=%d stderr=%q", status, stderr.String())
	}
	if got, want := b.startCalls, []startCall{{target: selector{container: "web", branch: defaultBranchID}, detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("main-container restart starts=%+v, want %+v", got, want)
	}
	if got, want := stdout.String(), "web\n"; got != want {
		t.Errorf("main-container restart output=%q, want %q", got, want)
	}
}

// explicitStopBackend records the order of a paused branch's stop: the fence
// on its console follower's exit report, the filesystem's preparation, the
// discard that ends the init, and how the fence was finished.
type explicitStopBackend struct {
	*fakeBackend
	events     []string
	prepareErr error
}

func (b *explicitStopBackend) beginExplicitInitStop(target selector) (func(bool), error) {
	b.events = append(b.events, "fence "+target.String())
	return func(commit bool) {
		b.events = append(b.events, fmt.Sprintf("finish %t", commit))
	}, nil
}

func (b *explicitStopBackend) preparePausedStop(target selector) error {
	b.events = append(b.events, "prepare")
	if b.prepareErr != nil {
		return b.prepareErr
	}
	return b.fakeBackend.preparePausedStop(target)
}

func (b *explicitStopBackend) discard(target selector) error {
	b.events = append(b.events, "discard")
	return b.fakeBackend.discard(target)
}

func (b *explicitStopBackend) start(target selector, detach, interactive bool) (int, error) {
	b.events = append(b.events, "start")
	return b.fakeBackend.start(target, detach, interactive)
}

// A restart's fresh start of a paused branch must not be discarded by the
// late report of the old init's end, which the stop owns: it fences the
// report before it discards the init.
func TestStopPausedFencesConsoleExitReport(t *testing.T) {
	paused := InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: "paused"}}}}}
	b := &explicitStopBackend{fakeBackend: &fakeBackend{inspectInfo: paused}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"restart", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restart status=%d stderr=%q", status, stderr.String())
	}
	if want := []string{"fence web@child", "prepare", "discard", "finish true", "start"}; !reflect.DeepEqual(b.events, want) {
		t.Errorf("restart of a paused branch=%q, want %q", b.events, want)
	}

	// A stop that fails before it ends the init gives the fence up.
	b = &explicitStopBackend{fakeBackend: &fakeBackend{inspectInfo: paused}, prepareErr: errors.New("sealing failed")}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("stop succeeded although preparing the paused branch failed")
	}
	if want := []string{"fence web@child", "prepare", "finish false"}; !reflect.DeepEqual(b.events, want) {
		t.Errorf("failed stop of a paused branch=%q, want %q", b.events, want)
	}
}

func TestFreshStartPeers(t *testing.T) {
	info := ttcbundle.ContainerInfo{Branches: []ttcbundle.BranchInfo{
		{ID: "target", State: "exited"},
		{ID: "z-running", State: "running"},
		{ID: "a-paused", State: "paused"},
		{ID: "cold", State: "dehydrated"},
		{ID: "old", State: "stopped"},
		// A corrupted branch, which only rm is left to, refused every
		// start of its runtime's other branches.
		{ID: "lost", State: "corrupted"},
	}}
	got, err := freshStartPeers(info, selector{container: "demo", branch: "target"}, "")
	if err != nil {
		t.Fatalf("freshStartPeers: %v", err)
	}
	want := []selector{
		{container: "demo", branch: "a-paused"},
		{container: "demo", branch: "z-running"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("freshStartPeers=%+v, want %+v", got, want)
	}

	// A branch still dehydrating once the start holds the lifecycle lock is
	// one whose publication is under way, which needs no runtime.
	info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "publishing", State: "dehydrating"})
	got, err = freshStartPeers(info, selector{container: "demo", branch: "target"}, "")
	if err != nil {
		t.Fatalf("freshStartPeers beside a publication: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("freshStartPeers beside a publication=%+v, want %+v", got, want)
	}
	info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "busy", State: "rehydrating"})
	if _, err := freshStartPeers(info, selector{container: "demo", branch: "target"}, ""); err == nil || !strings.Contains(err.Error(), "busy is rehydrating") {
		t.Fatalf("freshStartPeers transient error=%v", err)
	}

	info.Branches = []ttcbundle.BranchInfo{
		{ID: "target", NodeID: "node-a", State: "exited"},
		{ID: "local", NodeID: "node-a", State: "running"},
		{ID: "remote", NodeID: "node-b", State: "running"},
		{ID: "unplaced", State: "dehydrating"},
	}
	got, err = freshStartPeers(info, selector{container: "demo", branch: "target"}, "node-a")
	if err != nil {
		t.Fatalf("freshStartPeers distributed: %v", err)
	}
	want = []selector{{container: "demo", branch: "local"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("distributed freshStartPeers=%+v, want %+v", got, want)
	}
}

func TestRehydrateAcceptsPublishingCapture(t *testing.T) {
	target := selector{container: "web", branch: "main"}
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
		Metadata: Branch{ID: target.branch, State: "dehydrating"},
	}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rehydrate", target.String()}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rehydrate publishing capture status=%d stderr=%q", status, stderr.String())
	}
	if got := b.rehydratedTargets; !reflect.DeepEqual(got, []selector{target}) {
		t.Fatalf("rehydrated targets=%+v, want %+v", got, []selector{target})
	}
}

func TestAcquireConsoleStdinIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.stdin.lock")
	target := selector{container: "demo", branch: "main"}
	first, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("first acquireConsoleStdin: %v", err)
	}
	if _, err := acquireConsoleStdin(path, target); err == nil {
		first.Close()
		t.Fatal("second acquireConsoleStdin succeeded")
	} else if !strings.Contains(err.Error(), "stdin for demo@main is already attached") {
		first.Close()
		t.Fatalf("second acquireConsoleStdin error=%q", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first owner: %v", err)
	}
	second, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("reacquireConsoleStdin: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second owner: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stating stdin lock: %v", err)
	} else if info.Mode().Perm() != 0600 {
		t.Errorf("stdin lock mode=%o, want 600", info.Mode().Perm())
	}
}

func TestParseSignal(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{value: "9", want: 9},
		{value: "KILL", want: 9},
		{value: "sigusr1", want: 10},
	} {
		got, err := parseSignal(test.value)
		if err != nil || got != test.want {
			t.Errorf("parseSignal(%q)=(%d, %v), want (%d, nil)", test.value, got, err, test.want)
		}
	}
	if _, err := parseSignal("NOPE"); err == nil {
		t.Error("parseSignal(NOPE) succeeded")
	}
}

func TestReadConsoleWaitStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.wait-status")
	if status, found, err := readConsoleWaitStatus(path); err != nil || found || status != 0 {
		t.Fatalf("missing status=(%d, %t, %v), want (0, false, nil)", status, found, err)
	}
	if err := os.WriteFile(path, []byte("1792\n"), 0600); err != nil {
		t.Fatalf("writing status: %v", err)
	}
	if status, found, err := readConsoleWaitStatus(path); err != nil || !found || status != 1792 {
		t.Fatalf("status=(%d, %t, %v), want (1792, true, nil)", status, found, err)
	}
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatalf("writing invalid status: %v", err)
	}
	if _, _, err := readConsoleWaitStatus(path); err == nil {
		t.Fatal("invalid status was accepted")
	}
}

// observedInitExitFixture is a standalone family, demo, whose running branch's
// init exited with status 7, as its console follower recorded, while nothing
// has yet finalized the exit.
func observedInitExitFixture(t *testing.T) (*runtimeBackend, ttcbundle.Store) {
	t.Helper()
	dataRoot := t.TempDir()
	runsc := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(dataRoot, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, State: "running",
		Branches: []ttcbundle.BranchInfo{{ID: defaultBranchID, State: "running", StorageState: "local"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := ttcbundle.Store{Root: dataRoot}
	paths, err := store.BranchConsolePaths("demo", defaultBranchID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.WaitStatus), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.WaitStatus, []byte(strconv.FormatUint(uint64(7<<8), 10)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &runtimeBackend{root: filepath.Join(dataRoot, "runsc-root"), runsc: runsc, dataRoot: dataRoot, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}, store
}

func TestFinalizeObservedInitExit(t *testing.T) {
	b, store := observedInitExitFixture(t)
	changed, err := b.finalizeObservedInitExit(selector{container: "demo", branch: defaultBranchID})
	if err != nil {
		t.Fatalf("finalizeObservedInitExit: %v", err)
	}
	if !changed {
		t.Fatal("completed init exit was not finalized")
	}
	info, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := persistentBranchInfo(info, defaultBranchID)
	if err != nil {
		t.Fatal(err)
	}
	if branch.State != "exited" || info.ExitStatus == nil || *info.ExitStatus != 7 {
		t.Fatalf("finalized metadata=%+v branch=%+v", info, branch)
	}
}

// An exit in a family a long operation works on is left to the operation;
// reconciliation finalizes it once the operation is over.
func TestReconcileLeavesTheExitsOfABusyFamily(t *testing.T) {
	b, store := observedInitExitFixture(t)
	var operations familyOperations
	op := &familyOperation{id: "demo", name: "demo", longOperation: longOperation{branch: defaultBranchID, state: "stopping"}}
	if err := operations.begin(op); err != nil {
		t.Fatal(err)
	}
	b.operations = &operations
	state := func() string {
		t.Helper()
		info, err := store.LoadContainer("demo")
		if err != nil {
			t.Fatal(err)
		}
		branch, err := persistentBranchInfo(info, defaultBranchID)
		if err != nil {
			t.Fatal(err)
		}
		return branch.State
	}
	if err := b.reconcileObservedInitExits(); err != nil {
		t.Fatalf("reconcileObservedInitExits: %v", err)
	}
	if got := state(); got != "running" {
		t.Fatalf("reconciliation finalized an exit in a busy family: branch state=%q", got)
	}
	operations.end(op)
	if err := b.reconcileObservedInitExits(); err != nil {
		t.Fatalf("reconcileObservedInitExits: %v", err)
	}
	if got := state(); got != "exited" {
		t.Fatalf("branch state=%q once the family was idle, want exited", got)
	}
}

func TestFinalizeObservedInitExitSkipsCommittedColdPhysicalRecord(t *testing.T) {
	dataRoot := t.TempDir()
	containerDir := filepath.Join(dataRoot, "containers", "physical")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "physical", Name: "logical", BundleDir: bundleDir, State: "dehydrated",
		Branches: []ttcbundle.BranchInfo{{
			ID: "private", State: "dehydrated", StorageState: "shared",
			PublicationID: "publication-1", StorageManifest: "cold/manifest.json",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{dataRoot: dataRoot, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	changed, err := b.finalizeObservedInitExit(selector{container: "physical", branch: "private"})
	if err != nil {
		t.Fatalf("finalizeObservedInitExit: %v", err)
	}
	if changed {
		t.Fatal("committed cold branch was treated as a newly exited runtime")
	}
}

func TestStopContainerConsoleDrainsRemovesInactiveFollower(t *testing.T) {
	dataRoot := t.TempDir()
	base := filepath.Join(dataRoot, "containers", "demo", "console", "exec", "main", "exec-test")
	if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		base + ".lock":        "",
		base + ".pid":         "2147483647\n",
		base + ".wait-status": "0\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := runtimeBackend{dataRoot: dataRoot}
	if err := b.stopContainerConsoleDrains("demo"); err != nil {
		t.Fatalf("stopContainerConsoleDrains: %v", err)
	}
	for _, path := range []string{base + ".pid", base + ".wait-status"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%q still exists or could not be inspected: %v", path, err)
		}
	}
}

func TestVerifyCgroupV2Resources(t *testing.T) {
	const pid = 4242
	root := t.TempDir()
	dir := filepath.Join(root, "user.slice", "ttc", "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	memory := int64(256 * 1024 * 1024)
	quota := int64(150000)
	period := uint64(100000)
	shares := uint64(512)
	resources := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: &memory},
		CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period, Shares: &shares},
	}
	write("cgroup.procs", "7\n4242\n")
	write("memory.max", "268435456\n")
	write("cpu.max", "150000 100000\n")
	write("cpu.weight", "20\n")
	membership := []byte("0::/user.slice/ttc/demo\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err != nil {
		t.Fatalf("verifyCgroupV2Resources: %v", err)
	}

	write("memory.max", "max\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("wrong memory.max error=%v", err)
	}
	write("memory.max", "268435456\n")
	if err := verifyCgroupV2Resources(pid, []byte("1:cpu:/legacy\n"), root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "unified cgroup v2") {
		t.Fatalf("missing cgroup v2 error=%v", err)
	}
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/other", resources); err == nil || !strings.Contains(err.Error(), "want suffix") {
		t.Fatalf("wrong cgroup path error=%v", err)
	}
	write("cgroup.procs", "7\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "is not a member") {
		t.Fatalf("missing PID error=%v", err)
	}
}

func TestPreflightCgroupV2Delegation(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "delegated")
	leaf := filepath.Join(parent, "daemon")
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "cgroup.controllers"):       "cpu memory pids\n",
		filepath.Join(parent, "cgroup.controllers"):     "cpu memory\n",
		filepath.Join(parent, "cgroup.subtree_control"): "+cpu +memory\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(proc, []byte("0::/delegated/daemon\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightCgroupV2Delegation(proc, root, []string{"cpu", "memory"}); err != nil {
		t.Fatalf("preflightCgroupV2Delegation: %v", err)
	}
	// A cgroup whose controls the user cannot write is not delegated to it;
	// root writes them whatever their mode.
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(parent, "cgroup.subtree_control"), 0400); err != nil {
			t.Fatal(err)
		}
		if err := preflightCgroupV2Delegation(proc, root, []string{"memory"}); err == nil || !strings.Contains(err.Error(), "Delegate=yes") {
			t.Fatalf("non-delegated preflight error=%v", err)
		}
		if err := os.Chmod(filepath.Join(parent, "cgroup.subtree_control"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := preflightCgroupV2Delegation(proc, root, []string{"io"}); err == nil || !strings.Contains(err.Error(), `controller "io"`) {
		t.Fatalf("missing-controller preflight error=%v", err)
	}
}

func TestPreflightHostResourceLimitsRefusesHardLimitsOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only: Linux preflight depends on the host's cgroups")
	}
	limit := int64(1 << 30)
	spec := specs.Spec{Linux: &specs.Linux{
		CgroupsPath: "ttc/test",
		Resources:   &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}},
	}}
	data, err := json.Marshal(&spec)
	if err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightHostResourceLimits(bundle); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("preflight of a hard memory limit on macOS: %v, want a refusal that names macOS", err)
	}
	spec.Linux.Resources = &specs.LinuxResources{}
	if data, err = json.Marshal(&spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightHostResourceLimits(bundle); err != nil {
		t.Fatalf("preflight without hard limits on macOS: %v", err)
	}
}

func TestClientDoesNotAcceptStorageOrDirectOptions(t *testing.T) {
	for _, option := range []string{"--root=/runtime", "--runsc=/ttcd", "--data-root=/data", "--direct"} {
		if _, _, _, err := parseGlobalOptions([]string{option, "ps"}, io.Discard); err == nil {
			t.Fatalf("legacy client option %q was accepted", option)
		}
	}
}

func TestUnsafeNoHostIsolationRequiresExplicitOptIn(t *testing.T) {
	opts, remaining, done, err := parseGlobalOptions([]string{"run", "busybox"}, io.Discard)
	if err != nil || done || opts.unsafeNoHostIsolation || !reflect.DeepEqual(remaining, []string{"run", "busybox"}) {
		t.Fatalf("default options=(%+v, %q, %t, %v), want hardened launch", opts, remaining, done, err)
	}
	opts, remaining, done, err = parseGlobalOptions([]string{"--unsafe-no-host-isolation", "run", "busybox"}, io.Discard)
	if err != nil || done || !opts.unsafeNoHostIsolation || !reflect.DeepEqual(remaining, []string{"run", "busybox"}) {
		t.Fatalf("unsafe options=(%+v, %q, %t, %v), want explicit opt-in", opts, remaining, done, err)
	}

	// The dentry cache is sized from this process's descriptor limit, so what
	// it should be is asked for rather than written down.
	dcache := []string(nil)
	if size, known := dentryCacheSize(); known {
		dcache = []string{"--dcache=" + strconv.Itoa(size)}
	}
	// On systrap, private pages go in address order, and the Sentry keeps
	// the family within its memory limit.
	if defaultRuntimePlatform == "systrap" {
		dcache = append(dcache, "--memory-homes=true", "--sentry-oom-kill=true")
	}
	wantHardened := append([]string{"--root", "/runtime", "--platform=" + defaultRuntimePlatform, "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=none", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory", "--memory-file-huge-tmpfs=true"}, dcache...)
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgs(); !reflect.DeepEqual(got, wantHardened) {
		t.Errorf("hardened runsc args=%q, want %q", got, wantHardened)
	}
	wantHost := append([]string{"--root", "/runtime", "--platform=" + defaultRuntimePlatform, "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=host", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory", "--memory-file-huge-tmpfs=true"}, dcache...)
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetwork("host"); !reflect.DeepEqual(got, wantHost) {
		t.Errorf("host-network runsc args=%q, want %q", got, wantHost)
	}
	wantTTCNAT := append(append([]string{"--root", "/runtime", "--platform=" + defaultRuntimePlatform, "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=ttc", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory", "--memory-file-huge-tmpfs=true"}, dcache...), "--ttc-network-nat=true")
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetworkNAT("ttc", true); !reflect.DeepEqual(got, wantTTCNAT) {
		t.Errorf("TTC NAT runsc args=%q, want %q", got, wantTTCNAT)
	}
	wantCustomNIC := append(append([]string(nil), wantTTCNAT...), "--ttc-network-ip=192.0.2.44", "--ttc-network-mac=02:aa:bb:cc:dd:ee", "--ttc-network-interface=eth1")
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetworkConfig("ttc", true, "192.0.2.44", "02:aa:bb:cc:dd:ee", "eth1"); !reflect.DeepEqual(got, wantCustomNIC) {
		t.Errorf("TTC custom NIC runsc args=%q, want %q", got, wantCustomNIC)
	}
	wantUnsafe := append([]string{
		"--root", "/runtime", "--platform=" + defaultRuntimePlatform, "--panic-log=/runtime/sentry-panic.log", "--rootless=false", "--unsafe-no-host-isolation=true",
		"--network=none", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory", "--memory-file-huge-tmpfs=true",
	}, dcache...)
	if got := (&runtimeBackend{root: "/runtime", unsafeNoHostIsolation: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantUnsafe) {
		t.Errorf("unsafe runsc args=%q, want %q", got, wantUnsafe)
	}
	wantUnsafeControl := []string{"--root", "/runtime", "--rootless=false", "--unsafe-no-host-isolation=true"}
	if got := (&runtimeBackend{root: "/runtime", unsafeNoHostIsolation: true}).runtimeControlArgs(); !reflect.DeepEqual(got, wantUnsafeControl) {
		t.Errorf("unsafe runsc control args=%q, want %q", got, wantUnsafeControl)
	}
	wantOldKernel := append(append([]string(nil), wantHardened...), "--gso=false")
	if got := (&runtimeBackend{root: "/runtime", disableHostGSO: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantOldKernel) {
		t.Errorf("old-kernel runsc args=%q, want %q", got, wantOldKernel)
	}
	var wantKVM []string
	for _, arg := range wantHardened {
		if arg != "--memory-homes=true" && arg != "--sentry-oom-kill=true" {
			wantKVM = append(wantKVM, arg)
		}
	}
	wantKVM[2] = "--platform=kvm"
	if got := (&runtimeBackend{root: "/runtime", runtimePlatform: "kvm"}).configuredRunscArgs(); !reflect.DeepEqual(got, wantKVM) {
		t.Errorf("KVM runsc args=%q, want %q", got, wantKVM)
	}
	// A cluster whose nodes need no CPUID faulting runs systrap without
	// syscall patching; KVM has none.
	wantUnpatched := append(append([]string(nil), wantKVM...), "--memory-homes=true", "--sentry-oom-kill=true", "--systrap-disable-syscall-patching=true")
	wantUnpatched[2] = "--platform=systrap"
	if got := (&runtimeBackend{root: "/runtime", runtimePlatform: "systrap", disableSyscallPatching: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantUnpatched) {
		t.Errorf("unpatched systrap runsc args=%q, want %q", got, wantUnpatched)
	}
	if got := (&runtimeBackend{root: "/runtime", runtimePlatform: "kvm", disableSyscallPatching: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantKVM) {
		t.Errorf("KVM runsc args without syscall patching=%q, want %q", got, wantKVM)
	}
}

// The cache a sandbox is allowed fits in the descriptors it will be given.
//
// This is the whole of the fix: a login node hands its processes 8191
// descriptors and no more, the gofer spends close to two of them on every
// dentry the sentry holds, and the per-mount default of a thousand each meant a
// seven-mount sandbox was permitted a cache it could not pay for. It reached
// EMFILE with its caches still filling, and a gofer at EMFILE never recovers —
// nothing evicts on a descriptor shortage.
func TestTheDentryCacheFitsInTheDescriptorsTheGoferWillHave(t *testing.T) {
	size, known := dentryCacheSize()
	if !known {
		t.Skip("this process cannot read its own descriptor limit")
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	// What a full cache costs the gofer, against what the gofer has. The
	// margin is the point: the other half is for the files the workload is
	// holding open, which are not cache and cannot be evicted.
	cost := size * goferFDsPerDentry
	if uint64(cost) >= limit.Cur && size < maxDentryCache {
		t.Fatalf("a full cache of %d dentries costs %d descriptors of %d", size, cost, limit.Cur)
	}
	if size < minDentryCache {
		t.Fatalf("cache of %d dentries is too small to be worth having", size)
	}
	if size > maxDentryCache {
		t.Fatalf("cache of %d dentries is past the ceiling of %d", size, maxDentryCache)
	}
}

// The sandbox is told the size, so that one cache serves every mount.
//
// Without the flag the sentry gives each mount its own thousand, which is what
// made the budget grow with the number of things mounted rather than staying
// inside what the gofer can hold.
func TestTheSandboxIsGivenOneCacheForAllOfItsMounts(t *testing.T) {
	size, known := dentryCacheSize()
	if !known {
		t.Skip("this process cannot read its own descriptor limit")
	}
	args := (&runtimeBackend{root: "/runtime"}).configuredRunscArgs()
	want := "--dcache=" + strconv.Itoa(size)
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("runsc args %q do not carry %q", args, want)
}

func TestDehydrationNetworkArgs(t *testing.T) {
	none, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{NetworkMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !none.IsRestore || len(none.LoopbackLinks) != 1 || none.TimeTravel != nil {
		t.Fatalf("none network args=%+v", none)
	}
	ttc, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{
		NetworkMode: "ttc", NetworkNAT: true, NetworkIP: "192.0.2.44", NetworkMAC: "02:aa:bb:cc:dd:ee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ttc.TimeTravel == nil || !ttc.TimeTravel.NAT || ttc.TimeTravel.GuestIP.String() != "192.0.2.44" {
		t.Fatalf("ttc network args=%+v", ttc)
	}
	if got := ttc.TimeTravel.GuestMAC.String(); got != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("restored MAC=%q", got)
	}
	// Records from before the interface's name was recorded keep ttc0.
	if got := ttc.TimeTravel.InterfaceName; got != "ttc0" {
		t.Fatalf("restored interface of a legacy record=%q, want ttc0", got)
	}
	named, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{NetworkMode: "ttc", NetworkInterface: "eth1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := named.TimeTravel.InterfaceName; got != "eth1" {
		t.Fatalf("restored interface=%q, want eth1", got)
	}
	if _, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{NetworkMode: "host"}); err == nil {
		t.Fatal("host network unexpectedly accepted for process-free restore")
	}
}

func TestRemoveConsumedRehydrationPageSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		branch    string
		transient bool
	}{
		{name: "durable dehydration image", branch: "main"},
		{name: "transient peer bootstrap", branch: "fresh", transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataRoot := t.TempDir()
			containerID := "container-id"
			memoryRoot := filepath.Join(dataRoot, "containers", containerID, "memory")
			encodedBranch := hex.EncodeToString([]byte(test.branch))
			sourceDir := filepath.Join(memoryRoot, "dehydrated", encodedBranch)
			if test.transient {
				sourceDir = filepath.Join(memoryRoot, ".fresh-start-test")
			}
			if err := os.MkdirAll(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			snapshotPath := filepath.Join(sourceDir, "memory-0000.snapshot")
			if err := os.WriteFile(snapshotPath, []byte("lazy page data"), 0600); err != nil {
				t.Fatal(err)
			}
			openSnapshot, err := os.Open(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			defer openSnapshot.Close()

			var stderr bytes.Buffer
			b := &runtimeBackend{dataRoot: dataRoot, stderr: &stderr}
			branch := ttcbundle.BranchInfo{ID: test.branch, FilesystemBranch: test.branch}
			argument := sourceDir
			if !test.transient {
				argument = ""
			}
			b.retainRehydrationPageSource(containerID, branch, argument)
			if stderr.Len() != 0 {
				t.Fatalf("removing page source warned: %s", stderr.String())
			}
			if _, err := os.Lstat(sourceDir); !os.IsNotExist(err) {
				t.Fatalf("source directory still exists: %v", err)
			}
			got, err := io.ReadAll(openSnapshot)
			if err != nil || string(got) != "lazy page data" {
				t.Fatalf("open snapshot after unlink=(%q, %v)", got, err)
			}

			// Cleanup is idempotent after the source has been removed.
			b.retainRehydrationPageSource(containerID, branch, argument)
			if stderr.Len() != 0 {
				t.Fatalf("repeated cleanup warned: %s", stderr.String())
			}
		})
	}
}

func TestRunConfiguredCommandDoesNotInheritCallerPipes(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
echo discarded-output
echo launch-diagnostic >&2
sleep 60 &
echo $! >"$0.pid"
exit 0
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stdout: &stdout, stderr: &stderr}
	started := time.Now()
	status, err := b.runConfiguredCommand("start", "demo")
	if pidBytes, readErr := os.ReadFile(runsc + ".pid"); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes))); parseErr == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	// The descendant sleeps for a minute, so 20 seconds is a generous
	// allowance for a heavily loaded host while still proving that launch did
	// not wait for it.
	if elapsed := time.Since(started); elapsed >= 20*time.Second {
		t.Fatalf("launch waited %v for a descendant holding stdio", elapsed)
	}
	if err != nil || status != 0 {
		t.Fatalf("launch returned status=%d err=%v", status, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("runsc launch stdout leaked to container output: %q", stdout.String())
	}
	if got, want := stderr.String(), "launch-diagnostic\n"; got != want {
		t.Errorf("runsc launch diagnostic=%q, want %q", got, want)
	}
}

func TestRuntimeDiagnostics(t *testing.T) {
	dataRoot := t.TempDir()
	b := &runtimeBackend{dataRoot: dataRoot}
	if !runtimeDiagnosticsPerContainer {
		configured, err := b.withRuntimeDiagnostics([]string{"--root", "/runtime"}, "container-id")
		if err != nil || !reflect.DeepEqual(configured, []string{"--root", "/runtime"}) {
			t.Fatalf("diagnostic arguments=%q, %v, want the runtime root's log alone", configured, err)
		}
		return
	}
	configured, err := b.withRuntimeDiagnostics([]string{"--root", "/runtime"}, "container-id")
	if err != nil {
		t.Fatal(err)
	}
	panicLog := filepath.Join(dataRoot, "containers", "container-id", "runtime", "panic.log")
	want := []string{"--root", "/runtime", "--panic-log=" + panicLog}
	if !reflect.DeepEqual(configured, want) {
		t.Fatalf("diagnostic arguments=%q, want %q", configured, want)
	}
	started := time.Now()
	contents := "unrelated runtime text\n" +
		"ttc ring0 bridge error: status=0xfae94003 phase=0x100\n" +
		"ttc hvf application error: owner continuation publication raced host return\n"
	if err := os.WriteFile(panicLog, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := b.runtimeDiagnosticSince("container-id", started), "ttc hvf application error: owner continuation publication raced host return"; got != want {
		t.Fatalf("runtime diagnostic=%q, want %q", got, want)
	}
	if got := b.runtimeDiagnosticSince("missing", started); got != "" {
		t.Fatalf("missing runtime diagnostic=%q, want empty", got)
	}
}

func TestRuntimeBackendUsesRunscCLI(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	if [ -e "$0.deleted" ]; then
		echo '[]'
		exit 0
	fi
	if [ -e "$0.stopped" ]; then
		echo '[{"id":"web","pid":0,"status":"stopped"}]'
	else
		echo '[{"id":"web","pid":4321,"status":"running"}]'
	fi
	exit 0
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	if [ -e "$0.stopped" ]; then
		exit 128
	fi
	if [ -e "$0.child" ]; then
		echo '[{"ID":"main","ParentID":"","FilesystemBranch":"main","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}},{"ID":"child","ParentID":"main","FilesystemBranch":"child","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}]'
	else
		echo '[{"ID":"main","ParentID":"","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}}]'
	fi
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--source" ] && [ "$5" = "main" ] && [ "$6" = "--paused" ] && [ "$7" = "web" ] && [ "$8" = "fork" ] && [ "$9" = "child" ]; then
	touch "$0.child"
	echo '{"ID":"child","ParentID":"main","FilesystemBranch":"child","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "exec-list" ] && [ "$6" = "child" ]; then
	echo '[]'
	exit 0
fi

if [ "$3" = "timetravel" ] && [ "$4" = "--console-socket" ] && [ "$6" = "--console-lock" ] && [ "$8" = "--console-ready-fd" ] && [ "$9" = "3" ] && [ "${10}" = "web" ] && [ "${11}" = "console-follow-family" ]; then
	exec env ` + fakeFollowerSocketEnv + `="$5" ` + fakeFollowerMarkerEnv + `="$0.retention" "` + testBinary(t) + `"
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "ps" ] && [ "$6" = "main" ]; then
	echo '[{"uid":0,"pid":1,"ppid":0,"pgid":1,"threads":[1],"c":0,"tty":"?","stime":"12:00","time":"1ms","cmd":"init"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "ps" ] && [ "$6" = "child" ]; then
	echo '[{"uid":1000,"pid":7,"ppid":1,"pgid":7,"threads":[7,8],"c":3,"tty":"?","stime":"12:34","time":"9ms","cmd":"worker"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "size" ] && [ "$6" = "child" ]; then
	echo '{"writable_bytes":32768}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--path" ] && { [ "$5" = "/cache" ] || [ "$5" = "/cache-ro" ]; } && [ "$6" = "web" ] && [ "$7" = "filesystem-size" ] && [ "$8" = "child" ]; then
	echo '{"writable_bytes":12288}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--path" ] && [ "$5" = "/cache" ] && [ "$6" = "--output" ] && [ "$8" = "web" ] && [ "$9" = "filesystem-tar" ] && [ "${10}" = "child" ]; then
	printf fake-tar >"$7"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--signal" ] && [ "$5" = "15" ] && [ "$6" = "web" ] && [ "$7" = "signal" ] && [ "$8" = "child" ]; then
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "wait" ] && [ "$6" = "child" ]; then
	echo '{"wait_status":15}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "wait" ] && [ "$6" = "main" ]; then
	exit 128
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		Name: "web", BundleDir: bundleDir, ImageSize: 2217006,
		ConsoleLogMaxBytes: 4096, ConsoleLogMaxFiles: 3, State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}},
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	consoleDir := filepath.Join(containerDir, "console")
	if err := os.MkdirAll(consoleDir, 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	retentionData := []byte(`{"evicted_bytes":1234,"evicted_generations":2,"updated_at":"2026-07-17T00:00:00Z"}`)
	if err := os.WriteFile(ttcconsole.RetentionStatsPath(filepath.Join(consoleDir, "main.log")), retentionData, 0600); err != nil {
		t.Fatalf("writing retention stats: %v", err)
	}
	staleRotatedLog := filepath.Join(consoleDir, "child.log"+ttcconsole.RotatedSuffix)
	if err := os.WriteFile(staleRotatedLog, []byte("stale"), 0600); err != nil {
		t.Fatalf("writing stale rotated log: %v", err)
	}
	staleOlderLog := ttcconsole.GenerationPath(filepath.Join(consoleDir, "child.log"), 2)
	if err := os.WriteFile(staleOlderLog, []byte("older"), 0600); err != nil {
		t.Fatalf("writing older stale log: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "web" || families[0].ImageSize != 2217006 || len(families[0].Branches) != 1 || families[0].Branches[0].ID != "main" {
		t.Fatalf("listFamilies returned %+v", families)
	}
	if got, err := b.rootfsSize(selector{container: "web", branch: "child"}); err != nil || got != 32768 {
		t.Fatalf("rootfsSize(child)=(%d, %v), want (32768, nil)", got, err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"root":{"readonly":true}}`), 0600); err != nil {
		t.Fatalf("writing read-only config: %v", err)
	}
	if got, err := b.rootfsSize(selector{container: "web", branch: "main"}); err != nil || got != 0 {
		t.Fatalf("rootfsSize(read-only main)=(%d, %v), want (0, nil)", got, err)
	}
	inspected, err := b.inspect("web")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.State.Status != "running" || inspected.State.Pid != 4321 || len(inspected.TTC.Branches) != 1 || len(inspected.TTC.Branches[0].Processes) != 1 || inspected.TTC.Branches[0].Processes[0].PID != 1 || inspected.TTC.Branches[0].OutputRetention == nil || inspected.TTC.Branches[0].OutputRetention.EvictedBytes != 1234 {
		t.Errorf("runtime-enriched inspect=%+v", inspected)
	}
	child, err := b.fork(selector{container: "web", branch: "main"}, "child", true)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != "main" || child.State != "paused" || child.ForkTotalNS != 3_000_000 || child.KernelForkNS != 2_000_000 {
		t.Errorf("fork returned %+v", child)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "console", "child.log")); err != nil {
		t.Errorf("fork console log: %v", err)
	}
	if _, err := os.Stat(staleRotatedLog); !os.IsNotExist(err) {
		t.Errorf("stale rotated log still exists: %v", err)
	}
	if _, err := os.Stat(staleOlderLog); !os.IsNotExist(err) {
		t.Errorf("older stale log still exists: %v", err)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, false); err == nil || !strings.Contains(err.Error(), "use -f to force removal") {
		t.Fatalf("unforced paused-branch removal error=%v, want force diagnostic", err)
	}
	marker := runsc + ".retention"
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console sidecar did not receive persisted retention flags")
		}
		time.Sleep(time.Millisecond)
	}

	processes, err := b.processes(selector{container: "web", branch: "child"})
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if len(processes) != 1 || processes[0].PID != 7 || processes[0].UID != 1000 || processes[0].Cmd != "worker" {
		t.Errorf("processes returned %+v", processes)
	}
	if err := b.signal(selector{container: "web", branch: "child"}, 15, false); err != nil {
		t.Fatalf("signal: %v", err)
	}
	status, err := b.wait(selector{container: "web", branch: "child"})
	if err != nil || status != 15 {
		t.Errorf("wait returned (%d, %v), want (15, nil)", status, err)
	}
	store := ttcbundle.Store{Root: dataRoot}
	mainConsole, err := store.BranchConsolePaths("web", "main")
	if err != nil {
		t.Fatalf("BranchConsolePaths(main): %v", err)
	}
	if err := os.WriteFile(mainConsole.WaitStatus, []byte("9\n"), 0600); err != nil {
		t.Fatalf("writing persistent main wait status: %v", err)
	}
	status, err = b.wait(selector{container: "web", branch: "main"})
	if err != nil || status != 9 {
		t.Errorf("wait fallback returned (%d, %v), want (9, nil)", status, err)
	}
	persisted, err := store.LoadContainer("web")
	if err != nil {
		t.Fatalf("LoadContainer after main wait: %v", err)
	}
	if persisted.State != "exited" || persisted.ExitStatus == nil || *persisted.ExitStatus != 137 || persisted.ExitedAt == nil {
		t.Errorf("main wait did not reconcile persistent state: %+v", persisted)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err == nil {
		t.Error("destroy swallowed a live-sandbox control error")
	}
	if err := os.WriteFile(runsc+".stopped", nil, 0600); err != nil {
		t.Fatalf("marking fake sandbox stopped: %v", err)
	}
	families, err = b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies after runtime stopped: %v", err)
	}
	mainState := ""
	if len(families) == 1 {
		for _, branch := range families[0].Branches {
			if branch.ID == "main" {
				mainState = branch.State
			}
		}
	}
	if len(families) != 1 || families[0].State != "exited" || mainState != "exited" {
		t.Fatalf("stopped runtime leaked through TTC state: %+v", families)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err != nil {
		t.Errorf("destroy after sandbox exit: %v", err)
	}
	if err := b.destroy(selector{container: "missing", branch: "child"}, true); err == nil {
		t.Error("destroy accepted a nonexistent persistent container")
	}
}

func TestRuntimeBackendResolvesStableContainerID(t *testing.T) {
	dir := t.TempDir()
	id := "1234567890ab" + strings.Repeat("c", 52)
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
id="` + id + `"
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
shift 2
case "$1 $2 $3 $4" in
"list --format=json --sandbox ")
	printf "[{\"id\":\"%s\",\"pid\":2468,\"status\":\"running\"}]\n" "$id"
	;;
"timetravel $id branches ")
	echo "[{\"ID\":\"main\",\"ParentID\":\"\",\"State\":\"running\",\"ForkMetrics\":{\"SourceDowntime\":0,\"Kernel\":{\"Total\":0},\"Total\":0}}]"
	;;
"timetravel $id ps main")
	echo "[{\"uid\":0,\"pid\":1,\"ppid\":0,\"pgid\":1,\"threads\":[1],\"c\":0,\"tty\":\"?\",\"stime\":\"12:00\",\"time\":\"1ms\",\"cmd\":\"init\"}]"
	;;
"timetravel $id pause main"|"timetravel $id resume main")
	;;
"delete --force $id ")
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", id)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: id, Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != id || families[0].Name != "web" || families[0].SandboxPID != 2468 {
		t.Fatalf("stable family=%+v", families)
	}
	for _, identifier := range []string{"web", id[:16]} {
		info, err := b.inspect(identifier)
		if err != nil {
			t.Fatalf("inspect(%q): %v", identifier, err)
		}
		if info.ID != id || info.Name != "/web" || len(info.TTC.Branches) != 1 || len(info.TTC.Branches[0].Processes) != 1 {
			t.Errorf("inspect(%q)=%+v", identifier, info)
		}
	}
	target := selector{container: "web", branch: "main"}
	if err := b.pause(target); err != nil {
		t.Fatalf("pause by name: %v", err)
	}
	if err := b.resume(selector{container: id[:16], branch: "main"}); err != nil {
		t.Fatalf("resume by ID prefix: %v", err)
	}
	if err := b.remove("web", true, false); err != nil {
		t.Fatalf("remove by name: %v", err)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Errorf("removed ID-keyed state still exists: %v", err)
	}
}

func TestRuntimeBackendExecUsesBranchFlag(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" = "--root" ] && [ "$2" = "/runtime" ] &&
   [ "$3" = "exec" ] && [ "$4" = "--ttc-allow-stopped-sandbox" ] &&
   [ "$5" = "--branch" ] && [ "$6" = "child" ] &&
   [ "$7" = "--exec-id" ] && [ "$8" = "exec-test" ] &&
   [ "$9" = "--internal-console" ] && [ "${10}" = "ttc-exec-test" ] &&
   [ "${11}" = "--env" ] && [ "${12}" = "A=1" ] &&
   [ "${13}" = "--cwd" ] && [ "${14}" = "/work" ] &&
   [ "${15}" = "--user" ] && [ "${16}" = "123:456" ] &&
   [ "${17}" = "--additional-gids" ] && [ "${18}" = "12" ] &&
   [ "${19}" = "--additional-gids" ] && [ "${20}" = "34" ] &&
   [ "${21}" = "--cap" ] && [ "${22}" = "CAP_CHOWN" ] &&
   [ "${23}" = "--cap" ] && [ "${24}" = "CAP_SYS_PTRACE" ] &&
   [ "${25}" = "web" ] && [ "${26}" = "echo" ] && [ "${27}" = "ok" ]; then
	echo branch-output
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stdout: &stdout, stderr: &stderr}
	status, err := b.exec(execRequest{
		target:         selector{container: "web", branch: "child"},
		execID:         "exec-test",
		args:           []string{"echo", "ok"},
		env:            []string{"A=1"},
		cwd:            "/work",
		user:           "123:456",
		additionalGIDs: []string{"12", "34"},
		capabilities:   []string{"CAP_CHOWN", "CAP_SYS_PTRACE"},
	})
	if err != nil || status != 0 {
		t.Fatalf("exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
	if stdout.String() != "branch-output\n" {
		t.Errorf("stdout=%q", stdout.String())
	}
}

func TestRuntimeBackendExecForwardsProcessFile(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" = "--root" ] && [ "$2" = "/runtime" ] &&
   [ "$3" = "exec" ] && [ "$4" = "--ttc-allow-stopped-sandbox" ] &&
   [ "$5" = "--branch" ] && [ "$6" = "child" ] &&
   [ "$7" = "--exec-id" ] && [ "$8" = "exec-test" ] &&
   [ "$9" = "--internal-console" ] && [ "${10}" = "ttc-exec-test" ] &&
   [ "${11}" = "--process" ] && [ "${12}" = "/process.json" ] &&
   [ "${13}" = "web" ] && [ -z "${14}" ]; then
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stderr: &stderr}
	status, err := b.exec(execRequest{
		target:      selector{container: "web", branch: "child"},
		execID:      "exec-test",
		processPath: "/process.json",
	})
	if err != nil || status != 0 {
		t.Fatalf("exec --process returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestRuntimeBackendTerminalExecUsesClientTERM(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
case " $* " in
  *" --env TERM=tmux-256color "*) ;;
  *) echo "client TERM missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" --internal-console-terminal "*) ;;
  *) echo "terminal exec flags missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" web true ") exit 0 ;;
  *) echo "terminal exec command missing: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, clientTERM: "tmux-256color", stderr: &stderr}
	status, err := b.exec(execRequest{
		target:   selector{container: "web", branch: "main"},
		args:     []string{"true"},
		terminal: true,
	})
	if err != nil || status != 0 {
		t.Fatalf("terminal exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestRuntimeBackendTerminalExecPreservesExplicitTERM(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
case " $* " in
  *" --env TERM=screen-256color "*) ;;
  *) echo "explicit TERM missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" --env TERM=xterm-256color "*) echo "client TERM overrode explicit TERM: $*" >&2; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, clientTERM: "xterm-256color", stderr: &stderr}
	status, err := b.exec(execRequest{
		target:   selector{container: "web", branch: "main"},
		args:     []string{"true"},
		env:      []string{"TERM=screen-256color"},
		terminal: true,
	})
	if err != nil || status != 0 {
		t.Fatalf("terminal exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestPurgeRuntimeDoesNotRequireBranchControlServer(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
case "$3" in
list)
	echo '[{"id":"private-source","pid":2468,"status":"running"}]'
	;;
timetravel)
	echo 'branch control server is unavailable' >&2
	exit 128
	;;
delete)
	if [ "$4" != "--force" ] || [ "$5" != "private-source" ]; then
		echo "unexpected delete: $*" >&2
		exit 2
	fi
	touch "$0.deleted"
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc}
	if err := b.purgeRuntime("private-source"); err != nil {
		t.Fatalf("purgeRuntime: %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Fatalf("runtime was not force-deleted: %v", err)
	}
}

// A sandbox whose container record is gone, such as with the TTC_ROOT that
// held it, lists by its ID; rm must remove it.
func TestRemoveSandboxWithoutRecord(t *testing.T) {
	dir := t.TempDir()
	exitedID := "2fee32cac1f1" + strings.Repeat("a", 52)
	runningID := "2fee7a11ce00" + strings.Repeat("b", 52)
	// A reaped child's PID names no process.
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "deleted")
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
shift 2
case "$1 $2 $3" in
"list --format=json --sandbox")
	cat <<'EOF'
[{"id":"` + exitedID + `","pid":` + strconv.Itoa(child.Process.Pid) + `,"status":"stopped"},{"id":"` + runningID + `","pid":` + strconv.Itoa(os.Getpid()) + `,"status":"running"}]
EOF
	;;
"delete --force "*)
	echo "$3" >>"` + deleted + `"
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dataRoot, "containers"), 0700); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}

	if err := b.remove("2fee", false, false); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("removing by a prefix of both sandboxes: err=%v, want an ambiguous prefix", err)
	}
	if err := b.remove(runningID, false, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("removing a running sandbox without --force: err=%v, want a request for --force", err)
	}
	if err := b.remove(exitedID[:12], false, false); err != nil {
		t.Errorf("removing an exited sandbox by its short ID: %v", err)
	}
	if err := b.destroy(selector{container: runningID, branch: defaultBranchID}, true); err != nil {
		t.Errorf("force-removing a running sandbox's main branch: %v", err)
	}
	if err := b.remove("missing", false, false); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removing a missing container: err=%v, want %v", err, os.ErrNotExist)
	}
	got, err := os.ReadFile(deleted)
	if err != nil {
		t.Fatal(err)
	}
	if want := exitedID + "\n" + runningID + "\n"; string(got) != want {
		t.Errorf("deleted sandboxes %q, want %q", got, want)
	}
}

func TestRemoveStoppedRuntimeBeforeStart(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	case "$(cat "$0.mode")" in
	stopped|loader-exited|unknown) printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID" ;;
	running|dehydrated) printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID" ;;
	stale) echo '[{"id":"web","pid":2147483647,"status":"running"}]' ;;
	transition)
		count=$(cat "$0.transition")
		if [ "$count" -lt 2 ]; then
			echo $((count + 1)) >"$0.transition"
			echo running >"$0.last"
			printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID"
		else
			echo exited >"$0.last"
			printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID"
		fi
		;;
	absent) echo '[]' ;;
	esac
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	case "$(cat "$0.mode")" in
	stopped) echo '[{"ID":"main","State":"exited"}]' ;;
	running) echo '[{"ID":"main","State":"running"}]' ;;
	dehydrated) echo '[{"ID":"main","State":"dehydrated"}]' ;;
	loader-exited) echo '[{"ID":"main","State":"exited"},{"ID":"peer","State":"running"}]' ;;
	transition) printf '[{"ID":"main","State":"%s"}]\n' "$(cat "$0.last")" ;;
	*) echo "control server unavailable" >&2; exit 1 ;;
	esac
	exit 0
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc}
	if err := os.WriteFile(runsc+".mode", []byte("stopped\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", false); err != nil {
		t.Fatalf("removeStoppedRuntime(stopped): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stopped runtime was not deleted: %v", err)
	}
	if err := os.Remove(runsc + ".deleted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("running\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", false); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("removeStoppedRuntime(running)=%v, want already-running error", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); !os.IsNotExist(err) {
		t.Errorf("running runtime delete marker err=%v", err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", false); err != nil {
		t.Fatalf("removeStoppedRuntime(stale): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stale runtime was not deleted: %v", err)
	}
	if err := os.Remove(runsc + ".deleted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("transition\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".transition", []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", true); err != nil {
		t.Fatalf("removeStoppedRuntime(transition): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("transitioned runtime was not deleted: %v", err)
	}
	if err := os.Remove(runsc + ".deleted"); err != nil {
		t.Fatal(err)
	}

	// The runtime's state is its loader branch's. A runtime whose loader
	// exited runs its other branches still, and one whose loader is
	// dehydrated, whose capture the start has made durable, runs none.
	for _, test := range []struct {
		mode    string
		deleted bool
		refusal string
	}{
		{mode: "loader-exited", refusal: `still runs branch "peer"`},
		{mode: "dehydrated", deleted: true},
		// A live Sentry that cannot list its branches may run any.
		{mode: "unknown", refusal: "already exited"},
	} {
		if err := os.WriteFile(runsc+".mode", []byte(test.mode+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := b.removeStoppedRuntime("web", false)
		if test.refusal == "" && err != nil {
			t.Errorf("removeStoppedRuntime(%s): %v", test.mode, err)
		}
		if test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
			t.Errorf("removeStoppedRuntime(%s)=%v, want %q", test.mode, err, test.refusal)
		}
		_, statErr := os.Stat(runsc + ".deleted")
		if deleted := statErr == nil; deleted != test.deleted {
			t.Errorf("removeStoppedRuntime(%s) deleted the runtime: %t, want %t", test.mode, deleted, test.deleted)
		}
		_ = os.Remove(runsc + ".deleted")
	}

	// A remote fork importing a branch into the runtime keeps it: the
	// removal waits for the import, then finds its branch running, which a
	// start goes beside.
	b.operations = &familyOperations{}
	if err := os.WriteFile(runsc+".mode", []byte("stopped\n"), 0600); err != nil {
		t.Fatal(err)
	}
	made := b.operations.beginMaking("web", "peer")
	removed := make(chan error, 1)
	go func() { removed <- b.removeStoppedRuntime("web", false) }()
	select {
	case err := <-removed:
		t.Fatalf("removeStoppedRuntime went on during an import: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(runsc+".mode", []byte("loader-exited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	made()
	var running *runtimeStillRunsError
	if err := <-removed; !errors.As(err, &running) || running.branch != "peer" {
		t.Fatalf("removeStoppedRuntime after the import = %v, want the runtime still running the import's branch", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err == nil {
		t.Fatal("the runtime was deleted under an import")
	}
}

// A runtime its record says runs nothing is kept while it runs a branch the
// record does not name, as one a remote fork imports into it.
func TestProcessFreeRuntimeKeepsAnUnrecordedBranch(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$3" = "list" ]; then
	printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	case "$(cat "$0.mode")" in
	import) echo '[{"ID":"main","State":"exited"},{"ID":"import","State":"paused"}]' ;;
	recorded) echo '[{"ID":"main","State":"exited"}]' ;;
	esac
	exit 0
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, operations: &familyOperations{}}
	info := ttcbundle.ContainerInfo{ID: "web", Name: "web", Branches: []ttcbundle.BranchInfo{{ID: "main", State: "exited"}}}
	for _, test := range []struct {
		mode    string
		deleted bool
	}{
		{mode: "import"},
		{mode: "recorded", deleted: true},
	} {
		if err := os.WriteFile(runsc+".mode", []byte(test.mode+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := b.deleteRuntimeIfProcessFree(info); err != nil {
			t.Fatalf("deleteRuntimeIfProcessFree(%s): %v", test.mode, err)
		}
		_, statErr := os.Stat(runsc + ".deleted")
		if deleted := statErr == nil; deleted != test.deleted {
			t.Errorf("deleteRuntimeIfProcessFree(%s) deleted the runtime: %t, want %t", test.mode, deleted, test.deleted)
		}
		_ = os.Remove(runsc + ".deleted")
	}
	// Nor is it released while a command makes a branch in it.
	made := b.operations.beginMaking("web", "import")
	defer made()
	if err := b.deleteRuntimeIfProcessFree(info); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err == nil {
		t.Error("the runtime was released while a command made a branch in it")
	}
}

// A removal that destroyed its branch in the Sentry and failed after leaves
// the branch recorded running; its retry must not ask the Sentry to prepare a
// branch it no longer has.
func TestRemovalRetryOfABranchItsSentryNoLongerHas(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo '[{"ID":"main","ParentID":"","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}}]'
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}, {ID: "child", ParentID: "main", State: "running"}},
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err != nil {
		t.Fatalf("retrying the removal of a branch the Sentry no longer has: %v", err)
	}
	info, err := ttcbundle.Store{Root: dataRoot}.LoadContainer("web")
	if err != nil {
		t.Fatalf("LoadContainer: %v", err)
	}
	if len(info.Branches) != 1 || info.Branches[0].ID != "main" {
		t.Errorf("branches after the removal: %+v, want main alone", info.Branches)
	}
}

// A live runtime that cannot list its branches may still run the one being
// removed: its removal fails, keeping its record, rather than take it for
// gone. A stopped runtime runs none, and its branch is removed.
func TestRemovalOfABranchOfARuntimeThatCannotListItsBranches(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	printf '[{"id":"web","pid":%s,"status":"%s"}]\n' "$PPID" "$(cat "$0.status")"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo "control server did not answer" >&2
	exit 1
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}, {ID: "child", ParentID: "main", State: "running"}},
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	branches := func() []string {
		t.Helper()
		info, err := ttcbundle.Store{Root: dataRoot}.LoadContainer("web")
		if err != nil {
			t.Fatalf("LoadContainer: %v", err)
		}
		var ids []string
		for _, branch := range info.Branches {
			ids = append(ids, branch.ID)
		}
		return ids
	}
	if err := os.WriteFile(runsc+".status", []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err == nil {
		t.Error("removed a branch of a live runtime that could not list its branches")
	}
	if got := branches(); !slices.Equal(got, []string{"main", "child"}) {
		t.Fatalf("branches after the failed removal: %v, want main and child", got)
	}
	if err := os.WriteFile(runsc+".status", []byte("stopped"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err != nil {
		t.Errorf("removing a branch of a stopped runtime: %v", err)
	}
	if got := branches(); !slices.Equal(got, []string{"main"}) {
		t.Errorf("branches after the removal: %v, want main", got)
	}
}

// interruptedDehydrationFixture is a standalone family, web, whose branch main
// is recorded as dehydrating while its Sentry holds a capture that was never
// made durable: what ttcd leaves if it dies in the middle of a dehydration.
// The fake runsc records a rehydration by creating runsc+".rehydrated" and a
// runtime deletion by creating runsc+".deleted". images are the capture's
// incomplete image directories.
func interruptedDehydrationFixture(t *testing.T) (runsc, dataRoot string, images []string) {
	t.Helper()
	dir := t.TempDir()
	runsc = filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
case "$3" in
list)
	printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID"
	;;
timetravel)
	if [ "$4" != "web" ]; then
		echo "unexpected family: $*" >&2
		exit 2
	fi
	case "$5" in
	branches)
		state=dehydrated
		[ -f "$0.rehydrated" ] && state=running
		printf '[{"ID":"main","State":"%s"}]\n' "$state"
		;;
	rehydrate)
		[ "$6" = "main" ] || exit 2
		touch "$0.rehydrated"
		;;
	*)
		echo "unexpected timetravel command: $*" >&2
		exit 2
		;;
	esac
	;;
delete)
	touch "$0.deleted"
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot = filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{ID: "main", FilesystemBranch: "main", State: "dehydrating"}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "web", Name: "web", BundleDir: bundleDir, State: "dehydrating", Branches: []ttcbundle.BranchInfo{branch},
	}); err != nil {
		t.Fatal(err)
	}
	memoryRoot := filepath.Join(containerDir, "memory", "dehydrated")
	key := ttcbundle.MemoryBranchKey(branch)
	for _, name := range []string{key, "." + key + "-interrupted"} {
		path := filepath.Join(memoryRoot, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		images = append(images, path)
	}
	return runsc, dataRoot, images
}

func TestReconcileInterruptedStandaloneDehydrationRollsBackLiveCapture(t *testing.T) {
	runsc, dataRoot, images := interruptedDehydrationFixture(t)
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	complete, err := b.reconcileInterruptedStandaloneLifecycles()
	if err != nil {
		t.Fatalf("reconcileInterruptedStandaloneLifecycles: %v", err)
	}
	if !complete {
		t.Fatal("completed rollback was reported as still transitioning")
	}
	if _, err := os.Stat(runsc + ".rehydrated"); err != nil {
		t.Fatalf("Sentry capture was not rehydrated: %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); !os.IsNotExist(err) {
		t.Fatalf("live rolled-back runtime was deleted: %v", err)
	}
	stored, err := (ttcbundle.Store{Root: dataRoot}).LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Branches[0].State; got != "running" {
		t.Fatalf("persistent branch state=%q, want running", got)
	}
	for _, path := range images {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("interrupted dehydration path %q remains: %v", path, err)
		}
	}
}

// A dehydration a long operation is making durable looks, to reconciliation,
// like one a crash interrupted. It is the operation's to finish, not
// reconciliation's to roll back.
func TestReconcileLeavesALongOperationsDehydrationAlone(t *testing.T) {
	runsc, dataRoot, images := interruptedDehydrationFixture(t)
	var operations familyOperations
	if err := operations.begin(&familyOperation{id: "web", name: "web", longOperation: longOperation{branch: "main", state: "dehydrating", shown: true}}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, operations: &operations}
	complete, err := b.reconcileInterruptedStandaloneLifecycles()
	if err != nil {
		t.Fatalf("reconcileInterruptedStandaloneLifecycles: %v", err)
	}
	if complete {
		t.Error("reconciliation was reported complete while a long operation works on the family")
	}
	for _, mark := range []string{".rehydrated", ".deleted"} {
		if _, err := os.Stat(runsc + mark); !os.IsNotExist(err) {
			t.Errorf("reconciliation changed the runtime a long operation works on (%s): %v", mark, err)
		}
	}
	stored, err := (ttcbundle.Store{Root: dataRoot}).LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Branches[0].State; got != "dehydrating" {
		t.Errorf("persistent branch state=%q, want dehydrating", got)
	}
	for _, path := range images {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the capture's image %q was removed: %v", path, err)
		}
	}
}

func TestReconcileInterruptedStandaloneDehydrationCommitsCompleteCapture(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
case "$3" in
list)
	printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID"
	;;
timetravel)
	[ "$4" = "web" ] && [ "$5" = "branches" ] || exit 2
	printf '[{"ID":"main","State":"dehydrated"}]\n'
	;;
delete)
	touch "$0.deleted"
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{
		ID: "main", FilesystemBranch: "main", State: "dehydrating",
		SnapshotID: 7, MemoryRoots: []string{"memory/7/root"}, MemoryRootObjects: []string{"object-7"},
		ControlRoot: "control/7", ControlRootObject: "control-object-7",
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "web", Name: "web", BundleDir: bundleDir, State: "dehydrating", Branches: []ttcbundle.BranchInfo{branch},
	}); err != nil {
		t.Fatal(err)
	}
	captureDir := memoryBranchDir(dataRoot, "web", "dehydrated", branch)
	if err := os.MkdirAll(captureDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"control.json", "metadata.state"} {
		if err := os.WriteFile(filepath.Join(captureDir, name), []byte("complete\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	complete, err := b.reconcileInterruptedStandaloneLifecycles()
	if err != nil {
		t.Fatalf("reconcileInterruptedStandaloneLifecycles: %v", err)
	}
	if !complete {
		t.Fatal("completed capture was reported as still transitioning")
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Fatalf("process-free runtime shell was not deleted: %v", err)
	}
	stored, err := (ttcbundle.Store{Root: dataRoot}).LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Branches[0].State; got != "dehydrated" {
		t.Fatalf("persistent branch state=%q, want dehydrated", got)
	}
	for _, name := range []string{"control.json", "metadata.state"} {
		if _, err := os.Stat(filepath.Join(captureDir, name)); err != nil {
			t.Fatalf("complete dehydration image %q was removed: %v", name, err)
		}
	}
}

func TestSetCommandStdinDoesNotHoldWaitOpen(t *testing.T) {
	input, inputWriter := io.Pipe()
	command := exec.Command("/bin/sh", "-c", "exit 0")
	cleanup, err := setCommandStdin(command, input)
	if err != nil {
		t.Fatalf("setCommandStdin: %v", err)
	}
	defer cleanup()
	defer inputWriter.Close()
	defer input.Close()
	if _, ok := command.Stdin.(*os.File); !ok {
		t.Fatalf("command stdin type=%T, want *os.File", command.Stdin)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("starting command: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("waiting for command: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command Wait blocked on still-open daemon stdin")
	}
}

func TestListFamiliesMergesPersistentStateAndReconcilesStaleRunning(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte(`#!/bin/sh
if [ "$3" = "list" ]; then
	echo '[]'
	exit 0
fi
exit 2
`), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}

	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "demo", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	createdAt := time.Date(2026, time.July, 17, 1, 2, 3, 0, time.UTC)
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", "demo", "container.json"), ttcbundle.ContainerInfo{
		Name:      "demo",
		Reference: "docker.io/library/alpine:3.20",
		Digest:    "sha256:abcd",
		BundleDir: bundleDir,
		State:     "created",
		CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "demo" || families[0].State != "created" || families[0].Branches[0].State != "created" {
		t.Fatalf("listFamilies returned %+v", families)
	}

	if families[0].Image != "docker.io/library/alpine:3.20" || families[0].Digest != "sha256:abcd" || !families[0].CreatedAt.Equal(createdAt) || !families[0].Branches[0].CreatedAt.Equal(createdAt) {
		t.Errorf("persistent family metadata=%+v", families[0])
	}
	store := ttcbundle.Store{Root: dataRoot}
	if err := store.UpdateState("demo", "running", nil); err != nil {
		t.Fatalf("marking container running: %v", err)
	}
	consolePaths, err := store.BranchConsolePaths("demo", defaultBranchID)
	if err != nil {
		t.Fatalf("BranchConsolePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(consolePaths.WaitStatus), 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	if err := os.WriteFile(consolePaths.WaitStatus, []byte("11\n"), 0600); err != nil {
		t.Fatalf("writing crash wait status: %v", err)
	}
	families, err = b.listFamilies()
	if err != nil {
		t.Fatalf("reconciling listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].State != "exited" || families[0].Branches[0].State != "exited" {
		t.Fatalf("reconciled listFamilies returned %+v", families)
	}
	info, err := store.LoadContainer("demo")
	if families[0].Image == "" || !families[0].Branches[0].CreatedAt.Equal(createdAt) {
		t.Errorf("reconciliation lost persistent metadata: %+v", families[0])
	}
	if err != nil {
		t.Fatalf("loading reconciled metadata: %v", err)
	}
	if info.State != "exited" || info.ExitedAt == nil || info.ExitStatus == nil || *info.ExitStatus != 139 {
		t.Errorf("reconciled metadata = %+v, want exited with status 139 and timestamp", info)
	}
	inspected, err := b.inspect("demo")
	if err != nil {
		t.Fatalf("inspect after reconciliation: %v", err)
	}
	if inspected.State.Status != "exited" || inspected.State.ExitCode != 139 {
		t.Errorf("inspect after reconciliation = %+v, want exited with status 139", inspected.State)
	}
}

// A listing that runs without the lifecycle lock repairs nothing: the runtime
// it cannot find may be one that a long mutation is rebuilding.
func TestListFamiliesWithoutTheLockRepairsNothing(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte(`#!/bin/sh
if [ "$3" = "list" ]; then
	echo '[]'
	exit 0
fi
exit 2
`), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "demo", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", "demo", "container.json"), ttcbundle.ContainerInfo{
		Name: "demo", Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abcd", BundleDir: bundleDir,
		State: "running", CreatedAt: time.Date(2026, time.July, 17, 1, 2, 3, 0, time.UTC),
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	store := ttcbundle.Store{Root: dataRoot}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, inventoryReadOnly: true}
	if _, err := b.listFamilies(); err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if info, err := store.LoadContainer("demo"); err != nil || info.State != "running" {
		t.Fatalf("a listing without the lock changed the record: state %q, err %v", info.State, err)
	}
	b.inventoryReadOnly = false
	if _, err := b.listFamilies(); err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if info, err := store.LoadContainer("demo"); err != nil || info.State != "exited" {
		t.Fatalf("a listing with the lock did not reconcile the lost runtime: state %q, err %v", info.State, err)
	}
}

func TestListFamiliesIncludesRemotePlacementWithoutReconcilingIt(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "remote", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", "remote", "container.json"), ttcbundle.ContainerInfo{
		ID: "remote", Name: "remote", NodeID: "node-b", BundleDir: bundleDir,
		State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: defaultBranchID, NodeID: "node-b", State: "running", CreatedAt: time.Now()}},
	}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, nodeID: "node-a"}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].ID != "remote" || families[0].NodeID != "node-b" || families[0].State != "running" {
		t.Fatalf("remote families=%+v", families)
	}
	stored, err := (ttcbundle.Store{Root: dataRoot}).LoadContainer("remote")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "running" {
		t.Fatalf("remote metadata was reconciled locally: %+v", stored)
	}
}

// A family listed from its placements alone is named after one that names
// it, not after one a runtime of it, apart from it, named, whichever comes
// first.
func TestListFamiliesNamesAFamilyAfterAPlacementThatNamesIt(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	placements, err := cluster.OpenPlacementStore(filepath.Join(dataRoot, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, record := range []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "a", FamilyName: "fork-0123", PhysicalContainer: "fork-0123", PhysicalBranch: "a"},
		{FamilyID: "family", BranchID: "b", FamilyName: "fc", PhysicalContainer: "family", PhysicalBranch: "b"},
	} {
		record.OwnerNode, record.OwnerIncarnation, record.State = "node-b", "boot-b", cluster.PlacementRunning
		if _, err := placements.Create(record, now); err != nil {
			t.Fatal(err)
		}
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, nodeID: "node-a", distributedSource: &distributedSourceRuntime{placements: placements}}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].Name != "fc" || len(families[0].Branches) != 2 {
		t.Fatalf("families=%+v, want fc with both branches", families)
	}
}

func TestListFamiliesHidesCompletedPhysicalMigrationRuntime(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	const physicalID = "fork-0123456789abcdef"
	script := `#!/bin/sh
if [ "$3" = "list" ]; then
	echo '[{"id":"` + physicalID + `","pid":2468,"status":"paused"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "` + physicalID + `" ] && [ "$5" = "branches" ]; then
	echo '[{"ID":"main","State":"paused"}]'
	exit 0
fi
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", physicalID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", physicalID, "container.json"), ttcbundle.ContainerInfo{
		ID: physicalID, Name: physicalID, BundleDir: bundleDir, State: "paused", CreatedAt: time.Now(),
		DistributedFamilyID: "logical-family", DistributedForkTransactionID: "completed-migration",
		Branches: []ttcbundle.BranchInfo{{ID: defaultBranchID, State: "paused", CreatedAt: time.Now()}},
	}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, nodeID: "node-a"}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 0 {
		t.Fatalf("completed physical migration runtime leaked into inventory: %+v", families)
	}
}

func TestVisiblePhysicalRuntimeBranchesHidesSparseProviders(t *testing.T) {
	branches := []Branch{
		{ID: "main", State: "paused"},
		{ID: "migration-current", State: "running"},
		{ID: "migration-provisional", State: "paused"},
	}
	placements := []cluster.PlacementRecord{{
		FamilyID: "logical-family", BranchID: "main",
		PhysicalContainer: "physical-family", PhysicalBranch: "migration-current",
		OwnerNode: "node-a", OwnerIncarnation: "inc-a", State: cluster.PlacementRunning,
	}}
	visible := visiblePhysicalRuntimeBranches(branches, placements, "physical-family")
	if len(visible) != 1 || visible[0].ID != "main" || visible[0].State != "running" {
		t.Fatalf("visible physical branches = %+v, want only logical main", visible)
	}
}

func TestBranchesInPlacementSnapshotDropsRenamedMetadata(t *testing.T) {
	branches := []Branch{
		{ID: "main", State: "running"},
		{ID: "old-name", State: "running"},
		{ID: "new-name", State: "running"},
	}
	placements := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main"},
		{FamilyID: "family", BranchID: "new-name"},
	}
	got := branchesInPlacementSnapshot(branches, placements)
	if len(got) != 2 || got[0].ID != "main" || got[1].ID != "new-name" {
		t.Fatalf("filtered branches=%+v, want main and new-name", got)
	}
}

func TestProjectContainerAuthorityReplacesStaleSummary(t *testing.T) {
	container := ttcbundle.ContainerInfo{NodeID: "old-node", State: "paused"}
	family := Family{NodeID: "new-node", State: "running"}
	projectContainerAuthority(&container, &family)
	if container.NodeID != "new-node" || container.State != "running" {
		t.Fatalf("projected container = %+v, want authoritative node and state", container)
	}
	projectContainerAuthority(&container, nil)
	if container.NodeID != "new-node" || container.State != "running" {
		t.Fatalf("nil family changed container = %+v", container)
	}
}

func TestLogicalPersistentBranchInfosHidesMigrationNames(t *testing.T) {
	created := time.Unix(123, 456).UTC()
	infos := []ttcbundle.BranchInfo{
		{ID: "main", State: "running"},
		{
			ID: "fork-current", ParentID: "main", State: "running", DistributedMigration: true,
			DistributedForkTransactionID: "fork-current", DistributedGraphReferenceOperation: "graph-current",
		},
		{ID: "fork-retired", ParentID: "main", State: "paused", DistributedMigration: true},
	}
	placements := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "node-a", State: cluster.PlacementRunning},
		{
			FamilyID: "family", BranchID: "test", PhysicalContainer: "family", PhysicalBranch: "fork-current",
			OwnerNode: "node-a", State: cluster.PlacementRunning, ParentBranch: "main", BranchCreatedAt: created, Quarantined: true,
		},
	}
	visible := logicalPersistentBranchInfos(infos, placements, nil, "family")
	if len(visible) != 2 || visible[0].ID != "main" || visible[1].ID != "test" {
		t.Fatalf("logical persistent branches = %+v, want main and test", visible)
	}
	if visible[1].DistributedMigration || visible[1].DistributedForkTransactionID != "" || visible[1].DistributedGraphReferenceOperation != "" {
		t.Fatalf("logical branch leaked private migration metadata: %+v", visible[1])
	}
	if visible[1].NodeID != "node-a" || !visible[1].CreatedAt.Equal(created) || !visible[1].Quarantined {
		t.Fatalf("logical branch lost placement metadata: %+v", visible[1])
	}
}

func TestLogicalPersistentBranchInfosUsesAuthorityNames(t *testing.T) {
	infos := []ttcbundle.BranchInfo{
		{ID: "main", State: "running"},
		{ID: "old-name", ParentID: "main", State: "running"},
	}
	placements := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "node-a", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "new-name", PhysicalContainer: "remote-physical", PhysicalBranch: "private", OwnerNode: "node-b", ParentBranch: "main", State: cluster.PlacementRunning},
	}
	got := logicalPersistentBranchInfos(infos, placements, nil, "family")
	if len(got) != 2 || got[0].ID != "main" || got[1].ID != "new-name" {
		t.Fatalf("logical infos=%+v, want main and new-name", got)
	}
	if got[1].NodeID != "node-b" || got[1].ParentID != "main" || got[1].State != "running" {
		t.Fatalf("synthesized renamed branch=%+v", got[1])
	}
}

// A running or paused branch whose node is offline is neither: it is
// unavailable until the node returns, or its removal gives the branch up.
func TestLogicalPersistentBranchInfosShowsOfflineOwnersUnavailable(t *testing.T) {
	infos := []ttcbundle.BranchInfo{
		{ID: "main", State: "running"},
		{ID: "held", ParentID: "main", State: "paused"},
		{ID: "gone", ParentID: "main", State: "exited"},
	}
	placements := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "node-b", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "held", PhysicalContainer: "family", PhysicalBranch: "held", OwnerNode: "node-b", ParentBranch: "main", State: cluster.PlacementPaused},
		{FamilyID: "family", BranchID: "gone", PhysicalContainer: "family", PhysicalBranch: "gone", OwnerNode: "node-b", ParentBranch: "main", State: cluster.PlacementCorrupted},
	}
	got := logicalPersistentBranchInfos(infos, placements, map[string]bool{"node-b": true}, "family")
	want := map[string]string{"main": "unavailable", "held": "unavailable", "gone": string(cluster.PlacementCorrupted)}
	for _, info := range got {
		if info.State != want[info.ID] {
			t.Errorf("branch %s is %q, want %q", info.ID, info.State, want[info.ID])
		}
	}
	for _, info := range logicalPersistentBranchInfos(infos, placements, nil, "family") {
		if info.ID == "main" && info.State != string(cluster.PlacementRunning) {
			t.Errorf("with its node online, main is %q, want running", info.State)
		}
	}
}

func TestMergeAuthoritativeBranchSynthesizesColdMigratedBranch(t *testing.T) {
	created := time.Unix(123, 456).UTC()
	family := Family{ID: "family", Branches: []Branch{{ID: "main", State: "running"}}}
	authority := cluster.BranchAuthority{
		FamilyID: "family", BranchID: "worker", Generation: 4,
		PublicationID: "publication-worker", Lifecycle: "dehydrated",
		Roots: cluster.DurableRoots{FilesystemRoot: "snapshot-packs/manifests/worker.json"},
	}
	placement := cluster.PlacementRecord{
		FamilyID: "family", BranchID: "worker", ParentBranch: "main",
		BranchCreatedAt: created, State: cluster.PlacementUnplaced, Quarantined: true,
	}
	got := mergeAuthoritativeBranch(family, authority, placement, true, "/shared")
	if len(got.Branches) != 2 || got.Branches[0].ID != "main" || got.Branches[1].ID != "worker" {
		t.Fatalf("merged family branches=%+v", got.Branches)
	}
	worker := got.Branches[1]
	if worker.State != "dehydrated" || worker.NodeID != "" || worker.ParentID != "main" || !worker.Quarantined ||
		worker.PublicationID != "publication-worker" || worker.StorageState != "shared" ||
		worker.StorageManifest != "/shared/snapshot-packs/manifests/worker.json" || !worker.CreatedAt.Equal(created) {
		t.Fatalf("synthesized worker=%+v", worker)
	}

	// Reapplying an authority snapshot updates the existing logical record and
	// must not manufacture a duplicate branch.
	authority.Lifecycle = "running"
	placement.State, placement.OwnerNode = cluster.PlacementRunning, "node-b"
	got = mergeAuthoritativeBranch(got, authority, placement, true, "/shared")
	if len(got.Branches) != 2 || got.Branches[1].NodeID != "node-b" || got.Branches[1].State != "running" {
		t.Fatalf("updated family branches=%+v", got.Branches)
	}
}

func TestAllBranchesColdOnNodeIgnoresRemoteRunningPeers(t *testing.T) {
	info := ttcbundle.ContainerInfo{
		NodeID: "node-a",
		Branches: []ttcbundle.BranchInfo{
			{ID: "local", NodeID: "node-a", State: "dehydrated"},
			{ID: "remote", NodeID: "node-b", State: "running"},
		},
	}
	allCold, count := allBranchesColdOnNode(info, "node-a")
	if !allCold || count != 1 {
		t.Fatalf("allBranchesColdOnNode(node-a)=(%t,%d), want (true,1)", allCold, count)
	}
	allCold, count = allBranchesColdOnNode(info, "")
	if allCold || count != 2 {
		t.Fatalf("allBranchesColdOnNode(legacy)=(%t,%d), want (false,2)", allCold, count)
	}
}

// A standalone dehydration stays resident only while a sibling on the node is
// live; a clustered one never does, because other nodes find dehydrated
// branches only through what was published.
func TestDehydrationStaysResidentOnlyBesideALiveSibling(t *testing.T) {
	family := func(states ...string) ttcbundle.ContainerInfo {
		info := ttcbundle.ContainerInfo{ID: "family"}
		for i, state := range states {
			info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "b" + strconv.Itoa(i), State: state})
		}
		return info
	}
	standalone := &runtimeBackend{dataRoot: t.TempDir()}
	for _, tc := range []struct {
		states []string
		want   bool
	}{
		{[]string{"dehydrating", "running"}, true},
		{[]string{"dehydrating", "paused", "dehydrated"}, true},
		{[]string{"dehydrating", "restarting"}, true},
		{[]string{"dehydrating", "exited", "dehydrated", "created"}, false},
		{[]string{"dehydrating", "dehydrating"}, false},
		{[]string{"dehydrating"}, false},
	} {
		if got := standalone.dehydrationMayStayResident(family(tc.states...), "b0"); got != tc.want {
			t.Errorf("siblings %v: resident=%t, want %t", tc.states[1:], got, tc.want)
		}
	}
	clustered := &runtimeBackend{dataRoot: t.TempDir(), publicationQueue: &ttcstorage.Queue{}}
	if clustered.dehydrationMayStayResident(family("dehydrating", "running"), "b0") {
		t.Error("a clustered dehydration stayed resident")
	}
	noDataRoot := &runtimeBackend{}
	if noDataRoot.dehydrationMayStayResident(family("dehydrating", "running"), "b0") {
		t.Error("a dehydration without a data root stayed resident")
	}
	elsewhere := ttcbundle.ContainerInfo{ID: "family", Branches: []ttcbundle.BranchInfo{
		{ID: "b0", NodeID: "node-a", State: "dehydrating"},
		{ID: "b1", NodeID: "node-b", State: "running"},
	}}
	onNode := &runtimeBackend{dataRoot: t.TempDir(), nodeID: "node-a"}
	if onNode.dehydrationMayStayResident(elsewhere, "b0") {
		t.Error("a live sibling on another node kept a dehydration resident")
	}
}

// A resident capture is in the runtime, so a family holding one needs it.
func TestResidentDehydrationRequiresItsRuntime(t *testing.T) {
	info := ttcbundle.ContainerInfo{Branches: []ttcbundle.BranchInfo{
		{ID: "main", State: "exited"},
		{ID: "fork", State: "dehydrated", StorageState: ttcbundle.StorageResident},
	}}
	if !persistentFamilyRequiredRuntime(info, "") {
		t.Error("a family with a resident dehydration did not require its runtime")
	}
	info.Branches[1].StorageState = "local"
	if persistentFamilyRequiredRuntime(info, "") {
		t.Error("a family with only durable and exited branches required its runtime")
	}
}

func TestListFamiliesReconcilesDeadListedSandbox(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	if [ "$(cat "$0.mode")" = "live" ]; then
		pid=$PPID
	else
		pid=2147483647
	fi
	printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$pid"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo 'connecting to control server: connection refused' >&2
	exit 128
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("live\n"), 0600); err != nil {
		t.Fatal(err)
	}

	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "web", Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: createdAt,
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", State: "exited", CreatedAt: createdAt},
			{ID: "test", ParentID: "main", State: "running", CreatedAt: createdAt},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store := ttcbundle.Store{Root: dataRoot}
	consolePaths, err := store.BranchConsolePaths("web", defaultBranchID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(consolePaths.WaitStatus), 0700); err != nil {
		t.Fatal(err)
	}
	// This belongs to main's earlier exit and must not be attributed to test.
	if err := os.WriteFile(consolePaths.WaitStatus, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	if _, err := b.listFamilies(); err != nil {
		t.Fatalf("listFamilies with live Sentry: %v", err)
	}
	info, err := store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "running" || info.Branches[1].State != "running" {
		t.Fatalf("transient RPC failure reconciled a live Sentry: %+v", info)
	}

	if err := os.WriteFile(runsc+".mode", []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies with dead Sentry: %v", err)
	}
	if len(families) != 1 || families[0].State != "exited" || families[0].SandboxPID != 0 || families[0].Error == "" {
		t.Fatalf("dead listed sandbox was not reconciled: %+v", families)
	}
	info, err = store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "exited" || info.ExitStatus != nil {
		t.Fatalf("reconciled metadata=%+v, want exited with unknown status", info)
	}
	for _, branch := range info.Branches {
		if branch.State != "exited" {
			t.Errorf("branch %q state=%q, want exited", branch.ID, branch.State)
		}
	}
	if err := b.removeStoppedRuntime("web", false); err != nil {
		t.Fatalf("removeStoppedRuntime(stale): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stale runtime was not deleted before restart: %v", err)
	}
}

type signalCall struct {
	target selector
	signal int
	all    bool
}

type startCall struct {
	target      selector
	detach      bool
	interactive bool
}

type fakeBackend struct {
	loadedBytes         int
	localDurable        bool
	loadedPlatform      string
	savedIdentifiers    []string
	savedPlatform       string
	mu                  sync.Mutex
	families            []Family
	processTarget       selector
	processList         []ProcessInfo
	ttyTarget           selector
	ttySession          string
	ttyObservation      kernel.InternalConsoleObservation
	rootfsSizes         map[selector]uint64
	diffChanges         []BranchPathChange
	resourceStatsTarget selector
	resourceStatsResult FamilyResourceStats
	sizeCalls           []selector
	sizeErr             error
	err                 error
	killErr             error
	forkSource          selector
	forkDestination     string
	forkDestinations    []string
	forkPaused          bool
	migrateTarget       selector
	migrateNode         string
	publishedTarget     selector
	publishedBinding    ttcbundle.PortBinding
	portListContainer   string
	portList            []ttcbundle.PortBinding
	unpublishedTarget   selector
	unpublishedBinding  ttcbundle.PortBinding
	paused              selector
	resumed             selector
	quarantineTarget    selector
	quarantineEnabled   bool
	preparedStop        selector
	preparedStops       []selector
	preparedPausedStops []selector
	discarded           selector
	discards            []selector
	dehydratedTargets   []selector
	rehydratedTargets   []selector
	destroyed           selector
	destroyForce        bool
	signals             []signalCall
	waited              selector
	waitCalls           []selector
	waitStatus          uint32
	waitStatuses        map[selector]uint32
	waitRelease         chan struct{}
	pullReference       string
	pullPlatform        string
	pullInfo            ttcimages.Info
	imageList           []ImageInfo
	imageInspectID      string
	imageInspectInfo    ImageInspectInfo
	imageRemoveID       string
	imageRemoveForce    bool
	imageRemoveResult   ImageRemoveResult
	commitTarget        selector
	commitReference     string
	commitOptions       imageCommitOptions
	commitInfo          ttcimages.Info
	runRequest          runRequest
	containerID         string
	runStatus           int
	created             bool
	started             selector
	startCalls          []startCall
	startDetach         bool
	startInteractive    bool
	attached            attachRequest
	logsTarget          selector
	logsOptions         logsOptions
	inspectInfo         InspectInfo
	pausedAll           []selector
	resumedAll          []selector
	removed             string
	removeForce         bool
	removeVolumes       bool
	execRequest         execRequest
	execStatus          int
	execSessionList     []runscExecSession
	execControlTarget   selector
	execControlID       string
	execControlSignal   int
	copyStdin           io.Reader
	copyExportTarget    selector
	copyExportPath      string
	copyExportFollow    bool
	copyExportData      []byte
	copyImportTarget    selector
	copyImportPath      string
	copyImportArchive   bool
	copyImportRaw       bool
	copyImportData      []byte
	checkpointRequest   checkpointRequest
	restoreSource       string
	restoreName         string
	renamed             []string
}

func (b *fakeBackend) localStorageIsDurable() bool { return b.localDurable }

func (b *fakeBackend) migrate(target selector, node string) error {
	b.migrateTarget = target
	b.migrateNode = node
	return b.err
}

func (b *fakeBackend) resourceStats(target selector) (FamilyResourceStats, error) {
	b.resourceStatsTarget = target
	return b.resourceStatsResult, b.err
}

func (b *fakeBackend) resultID(request runRequest) string {
	if b.containerID != "" {
		return b.containerID
	}
	return request.name
}

func (b *fakeBackend) create(request runRequest) (string, error) {
	b.runRequest = request
	b.created = true
	return b.resultID(request), b.err
}

func (b *fakeBackend) run(request runRequest) (containerRunResult, error) {
	b.runRequest = request
	return containerRunResult{ID: b.resultID(request), Status: b.runStatus}, b.err
}

func (b *fakeBackend) start(target selector, detach, interactive bool) (int, error) {
	b.startCalls = append(b.startCalls, startCall{target: target, detach: detach, interactive: interactive})
	b.started = target
	b.startDetach = detach
	b.startInteractive = interactive
	return b.runStatus, b.err
}

func (b *fakeBackend) attach(request attachRequest) (uint32, error) {
	b.attached = request
	b.attached.detachKeys = append([]byte(nil), request.detachKeys...)
	return b.waitStatus, b.err
}

func (b *fakeBackend) logs(target selector, options logsOptions) error {
	b.logsTarget = target
	b.logsOptions = options
	return b.err
}

func (b *fakeBackend) exec(request execRequest) (int, error) {
	b.execRequest = request
	return b.execStatus, b.err
}

func (b *fakeBackend) execSessions(target selector) ([]runscExecSession, error) {
	b.execControlTarget = target
	return append([]runscExecSession(nil), b.execSessionList...), b.err
}

func (b *fakeBackend) waitExecSession(target selector, execID string) (uint32, error) {
	b.execControlTarget = target
	b.execControlID = execID
	return b.waitStatus, b.err
}

func (b *fakeBackend) signalExecSession(target selector, execID string, signal int) error {
	b.execControlTarget = target
	b.execControlID = execID
	b.execControlSignal = signal
	return b.err
}

func (b *fakeBackend) inspect(name string) (InspectInfo, error) {
	return b.inspectInfo, b.err
}

func (b *fakeBackend) remove(name string, force, volumes bool) error {
	b.removed = name
	b.removeForce = force
	b.removeVolumes = volumes
	return b.err
}

func (b *fakeBackend) loadImages(archive io.Reader, platform string) ([]ttcimages.Info, error) {
	if archive == nil {
		return nil, errors.New("no image archive on standard input")
	}
	data, err := io.ReadAll(archive)
	if err != nil {
		return nil, err
	}
	b.loadedBytes = len(data)
	b.loadedPlatform = platform
	return []ttcimages.Info{
		{Reference: "docker.io/library/one:latest", Digest: "sha256:one", Platform: platform},
		{Reference: "docker.io/library/two:test", Digest: "sha256:two", Platform: platform},
	}, nil
}

func (b *fakeBackend) saveImages(archive io.Writer, identifiers []string, platform string) error {
	b.savedIdentifiers = append([]string(nil), identifiers...)
	b.savedPlatform = platform
	_, err := archive.Write([]byte("image archive"))
	return err
}

func (b *fakeBackend) pull(reference, platform string, progress bool) (ttcimages.Info, error) {
	b.pullReference = reference
	b.pullPlatform = platform
	return b.pullInfo, b.err
}

func (b *fakeBackend) listImages() ([]ImageInfo, error) {
	return b.imageList, b.err
}

func (b *fakeBackend) inspectImage(identifier string) (ImageInspectInfo, error) {
	b.imageInspectID = identifier
	return b.imageInspectInfo, b.err
}

func (b *fakeBackend) removeImage(identifier string, force bool) (ImageRemoveResult, error) {
	b.imageRemoveID = identifier
	b.imageRemoveForce = force
	return b.imageRemoveResult, b.err
}

func (b *fakeBackend) commit(target selector, reference string, options imageCommitOptions) (ttcimages.Info, error) {
	b.commitTarget = target
	b.commitReference = reference
	b.commitOptions = options
	b.commitOptions.Changes = append([]string(nil), options.Changes...)
	return b.commitInfo, b.err
}

func (b *fakeBackend) listFamilies() ([]Family, error) {
	return b.families, b.err
}

func (b *fakeBackend) processes(target selector) ([]ProcessInfo, error) {
	b.processTarget = target
	return b.processList, b.err
}

func (b *fakeBackend) consoleObservation(target selector, session string) (kernel.InternalConsoleObservation, error) {
	b.ttyTarget = target
	b.ttySession = session
	return b.ttyObservation, b.err
}

func (b *fakeBackend) watchConsole(target selector, session string, output io.Writer) error {
	b.ttyTarget = target
	b.ttySession = session
	if b.err != nil {
		return b.err
	}
	return json.NewEncoder(output).Encode(&b.ttyObservation)
}

func (b *fakeBackend) rootfsSize(target selector) (uint64, error) {
	b.sizeCalls = append(b.sizeCalls, target)
	if b.sizeErr != nil {
		return 0, b.sizeErr
	}
	return b.rootfsSizes[target], nil
}

func (b *fakeBackend) diff(target selector) ([]BranchPathChange, error) {
	b.processTarget = target
	return append([]BranchPathChange(nil), b.diffChanges...), b.err
}

func (b *fakeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	branches, err := b.forkMany(source, []string{destination}, paused, false)
	if err != nil {
		return Branch{}, err
	}
	return branches[0], nil
}

func (b *fakeBackend) forkMany(source selector, destinations []string, paused, quarantine bool) ([]Branch, error) {
	b.forkSource = source
	b.forkDestinations = append([]string(nil), destinations...)
	b.forkDestination = ""
	if len(destinations) != 0 {
		b.forkDestination = destinations[len(destinations)-1]
	}
	b.forkPaused = paused
	if b.err != nil {
		return nil, b.err
	}
	branches := make([]Branch, len(destinations))
	for i, destination := range destinations {
		branches[i] = Branch{ID: destination, ParentID: source.branch, State: "paused"}
	}
	return branches, nil
}

func (b *fakeBackend) publishPort(target selector, binding ttcbundle.PortBinding) error {
	b.publishedTarget = target
	b.publishedBinding = binding
	return b.err
}

func (b *fakeBackend) listPorts(container string) ([]ttcbundle.PortBinding, error) {
	b.portListContainer = container
	return append([]ttcbundle.PortBinding(nil), b.portList...), b.err
}

func (b *fakeBackend) unpublishPort(target selector, binding ttcbundle.PortBinding) error {
	b.unpublishedTarget = target
	b.unpublishedBinding = binding
	return b.err
}

func (b *fakeBackend) pause(target selector) error {
	b.paused = target
	b.pausedAll = append(b.pausedAll, target)
	return b.err
}

func (b *fakeBackend) resume(target selector) error {
	b.resumed = target
	b.resumedAll = append(b.resumedAll, target)
	return b.err
}

func (b *fakeBackend) setQuarantine(target selector, enabled bool) error {
	b.quarantineTarget = target
	b.quarantineEnabled = enabled
	return b.err
}

func (b *fakeBackend) renameBranch(target selector, name string) error {
	b.renamed = append(b.renamed, target.String()+"->"+name)
	return b.err
}

func (b *fakeBackend) prepareStop(target selector) error {
	b.preparedStop = target
	b.preparedStops = append(b.preparedStops, target)
	return b.err
}

func (b *fakeBackend) preparePausedStop(target selector) error {
	b.preparedPausedStops = append(b.preparedPausedStops, target)
	return b.err
}

func (b *fakeBackend) discard(target selector) error {
	b.discarded = target
	b.discards = append(b.discards, target)
	return b.err
}

func TestRuntimeFamilyExitedLoaderDoesNotDeleteLivePeers(t *testing.T) {
	family := Family{
		ID: "physical", State: "exited",
		Branches: []Branch{{ID: "loader", State: "exited"}, {ID: "peer", State: "running"}},
	}
	if present, only := runtimeFamilyBranchDisposition(family, "loader"); !present || only {
		t.Fatalf("loader disposition=(%t,%t), want present without whole-sandbox deletion", present, only)
	}
	if present, only := runtimeFamilyBranchDisposition(Family{Branches: []Branch{{ID: "last", State: "exited"}}}, "last"); !present || !only {
		t.Fatalf("final branch disposition=(%t,%t), want final sandbox deletion", present, only)
	}
}

func (b *fakeBackend) dehydrate(target selector) error {
	b.dehydratedTargets = append(b.dehydratedTargets, target)
	return b.err
}

func (b *fakeBackend) rehydrate(target selector) error {
	b.rehydratedTargets = append(b.rehydratedTargets, target)
	return b.err
}

func (b *fakeBackend) destroy(target selector, force bool) error {
	b.destroyed = target
	b.destroyForce = force
	return b.err
}

func (b *fakeBackend) signal(target selector, signal int, all bool) error {
	b.signals = append(b.signals, signalCall{target: target, signal: signal, all: all})
	if signal == 9 {
		if b.waitRelease != nil {
			close(b.waitRelease)
		}
		if b.killErr != nil {
			return b.killErr
		}
	}
	return b.err
}

func (b *fakeBackend) wait(target selector) (uint32, error) {
	b.mu.Lock()
	b.waited = target
	b.waitCalls = append(b.waitCalls, target)
	status := b.waitStatus
	if value, ok := b.waitStatuses[target]; ok {
		status = value
	}
	release := b.waitRelease
	err := b.err
	b.mu.Unlock()
	if release != nil {
		<-release
	}
	return status, err
}

func (b *fakeBackend) copyInput() io.Reader {
	return b.copyStdin
}

func (b *fakeBackend) exportContainerPath(target selector, source string, followLink bool, output io.Writer) error {
	b.copyExportTarget = target
	b.copyExportPath = source
	b.copyExportFollow = followLink
	if _, err := output.Write(b.copyExportData); err != nil {
		return err
	}
	return b.err
}

func (b *fakeBackend) importContainerPath(target selector, destination string, preserveOwner, rawArchive bool, input io.Reader) error {
	b.copyImportTarget = target
	b.copyImportPath = destination
	b.copyImportArchive = preserveOwner
	b.copyImportRaw = rawArchive
	data, err := io.ReadAll(input)
	b.copyImportData = data
	if err != nil {
		return err
	}
	return b.err
}

func (b *fakeBackend) checkpoint(request checkpointRequest, output io.Writer) error {
	b.checkpointRequest = request
	if request.destination == "-" {
		_, _ = io.WriteString(output, "checkpoint-bytes")
	}
	return b.err
}

func (b *fakeBackend) restoreCheckpoint(source, name string) (string, error) {
	b.restoreSource = source
	b.restoreName = name
	return b.containerID, b.err
}

func TestCheckpointAndRestoreCommands(t *testing.T) {
	b := &fakeBackend{containerID: "restored-id"}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"checkpoint", "--compression=none", "web@child", "/tmp/web.ttc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("checkpoint status=%d, stderr=%q", status, stderr.String())
	}
	if b.checkpointRequest.target != (selector{container: "web", branch: "child"}) ||
		b.checkpointRequest.destination != "/tmp/web.ttc" ||
		b.checkpointRequest.compression != "none" {
		t.Fatalf("checkpoint request=%+v", b.checkpointRequest)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restore", "--name", "recovered", "/tmp/web.ttc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restore status=%d, stderr=%q", status, stderr.String())
	}
	if b.restoreSource != "/tmp/web.ttc" || b.restoreName != "recovered" {
		t.Fatalf("restore=(%q, %q)", b.restoreSource, b.restoreName)
	}
	if got := strings.TrimSpace(stdout.String()); got != "restored-id" {
		t.Fatalf("restore stdout=%q", got)
	}
}

func TestPrepareCheckpointClientIO(t *testing.T) {
	t.Run("checkpoint download", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "snapshot.ttc")
		args, input, output, finish, err := prepareCheckpointClientIO(
			[]string{"checkpoint", "--compression=none", "web@main", destination}, strings.NewReader("stdin"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if args[len(args)-1] != "-" || input == nil || finish == nil {
			t.Fatalf("prepared checkpoint args=%q input=%v finish-nil=%t", args, input, finish == nil)
		}
		if _, err := io.WriteString(output, "checkpoint-bytes"); err != nil {
			t.Fatal(err)
		}
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(destination); err != nil || string(data) != "checkpoint-bytes" {
			t.Fatalf("checkpoint output=(%q, %v)", data, err)
		}
	})

	t.Run("restore upload", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "snapshot.ttc")
		if err := os.WriteFile(source, []byte("checkpoint-bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		args, input, _, finish, err := prepareCheckpointClientIO(
			[]string{"restore", "--node", "node-b", "--name", "web", source}, strings.NewReader(""), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if args[len(args)-1] != "-" || finish == nil {
			t.Fatalf("prepared restore args=%q finish-nil=%t", args, finish == nil)
		}
		data, err := io.ReadAll(input)
		if err != nil || string(data) != "checkpoint-bytes" {
			t.Fatalf("restore input=(%q, %v)", data, err)
		}
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCopyCheckpointHostDirectory(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data"), []byte("value"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/data", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot")
	if err := copyCheckpointHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("copied data=%q", data)
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "nested/data" {
		t.Fatalf("copied symlink=%q", target)
	}
}

func TestCopyCheckpointHostFileCreatesParent(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("value"), 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "host-mounts", "5")
	if err := copyCheckpointHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("copied data=%q", data)
	}
}

func TestManagedVolumeMountParsing(t *testing.T) {
	request, err := parseMountRequests(
		[]string{"/anonymous", "named:/cache:ro,nocopy"},
		[]string{"type=volume,source=long,target=/long,readonly"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.managed) != 3 {
		t.Fatalf("managed volumes=%+v, want three", request.managed)
	}
	if got := request.managed[0]; !got.Anonymous || got.Destination != "/anonymous" {
		t.Errorf("anonymous short volume=%+v", got)
	}
	if got := request.managed[1]; got.Name != "named" || got.Destination != "/cache" || !got.Readonly || !got.NoCopy {
		t.Errorf("named short volume=%+v", got)
	}
	if got := request.managed[2]; got.Name != "long" || got.Destination != "/long" || !got.Readonly {
		t.Errorf("long volume=%+v", got)
	}
}

func TestMergeBranchHistoryUsesPersistedReparenting(t *testing.T) {
	now := time.Now().UTC()
	merged := mergeBranchHistory(
		[]Branch{{ID: "child", ParentID: "", Quarantined: true, State: "running", CreatedAt: now, SnapshotID: 41, MemoryRoots: []string{"memory/41/root"}, MemoryRootObjects: []string{"0000000000000029-03-0000000000000001"}}},
		[]Branch{{ID: "child", ParentID: "removed-parent", State: "paused", CreatedAt: now}},
	)
	if len(merged) != 1 || merged[0].ParentID != "" || !merged[0].Quarantined || merged[0].State != "paused" ||
		merged[0].SnapshotID != 41 || !slices.Equal(merged[0].MemoryRoots, []string{"memory/41/root"}) || !slices.Equal(merged[0].MemoryRootObjects, []string{"0000000000000029-03-0000000000000001"}) {
		t.Fatalf("merged branch=%+v, want persisted parent and live state", merged)
	}
}

func TestClearConsoleIncarnationStatusPreservesLogs(t *testing.T) {
	dir := t.TempDir()
	paths := ttcbundle.ConsolePaths{
		Log: filepath.Join(dir, "main.log"), RetentionStats: filepath.Join(dir, "main.retention.json"),
		PID: filepath.Join(dir, "main.pid"), WaitStatus: filepath.Join(dir, "main.wait-status"),
	}
	for path, contents := range map[string]string{
		paths.Log: "old output\n", paths.RetentionStats: `{"evicted_bytes":1}`,
		paths.PID: "123\n", paths.WaitStatus: "15\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := clearConsoleIncarnationStatus(paths); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PID, paths.WaitStatus} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("terminal marker %q remains: %v", path, err)
		}
	}
	for _, path := range []string{paths.Log, paths.RetentionStats} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("durable log state %q was removed: %v", path, err)
		}
	}
}

func TestLocalGraphCaptureRejectsStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	control := `{"snapshot_id":8,"memory_roots":[{"name":"memory/8/root","root":"0000000000000008-03-0000000000000001"}]}`
	if err := os.WriteFile(filepath.Join(dir, "control.json"), []byte(control), 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{SnapshotID: 8, MemoryRoots: []string{"memory/8/root"}, MemoryRootObjects: []string{"0000000000000008-03-0000000000000001"}}
	if !localGraphCaptureMatches(dir, branch) {
		t.Fatal("matching local graph capture was rejected")
	}
	branch.SnapshotID = 9
	if localGraphCaptureMatches(dir, branch) {
		t.Fatal("stale local graph capture was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "control.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if localGraphCaptureMatches(dir, branch) {
		t.Fatal("malformed local graph capture was accepted")
	}
}

// A rehydration that cancels a dehydration's publication restores the
// branch from its capture here. The manifest the branch still names is an
// older publication's, its ancestor: the capture is current, and nothing is
// installed or refused from the manifest.
func TestLocalColdBranchKeepsItsCaptureOverAnAncestorManifest(t *testing.T) {
	dataRoot := t.TempDir()
	b := &runtimeBackend{dataRoot: dataRoot}
	info := ttcbundle.ContainerInfo{ID: "physical"}
	root := ttcsnapshot.ObjectID{Snapshot: 3, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	ancestor := coldBranchManifest{
		Version: 5, Container: "physical", Branch: "main", PublicationID: "publication-ancestor", Kind: "dehydrate",
		MemorySnapshot: 3, ControlRoot: ttcsnapshot.ObjectID{Snapshot: 3, Ordinal: 2, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
		InternalMounts: []cluster.InternalMountDescriptor{{Destination: "/etc/hostname", Contents: []byte("older\n"), Mode: 0644}},
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "0123456789abcdef0123456789abcdef",
			BranchID:  "main", Root: root.String(), NextInode: 2,
		}, Origins: []uint64{3}}},
	}
	manifestPath := filepath.Join(dataRoot, "cold", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{
		ID: "main", FilesystemBranch: "main", State: "dehydrating", StorageState: "local", StorageManifest: manifestPath,
		SnapshotID: 8, MemoryRoots: []string{"memory/8/root"}, MemoryRootObjects: []string{"0000000000000008-03-0000000000000001"},
	}
	capture := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
	if err := os.MkdirAll(capture, 0700); err != nil {
		t.Fatal(err)
	}
	control := `{"snapshot_id":8,"memory_roots":[{"name":"memory/8/root","root":"0000000000000008-03-0000000000000001"}]}`
	if err := os.WriteFile(filepath.Join(capture, "control.json"), []byte(control), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.ensureLocalColdBranch(info, branch); err != nil {
		t.Fatalf("restoring a canceled publication's capture: %v", err)
	}
	// Nor while the capture's own publication is under way, which the
	// record names, and the manifest not yet.
	branch.PublicationID, branch.StorageState = "publication-new", "publishing"
	if err := b.ensureLocalColdBranch(info, branch); err != nil {
		t.Fatalf("forking a branch whose publication is under way: %v", err)
	}
	// A shared branch's manifest is its state, and must be its publication's.
	branch.State, branch.StorageState = "dehydrated", "shared"
	if err := b.ensureLocalColdBranch(info, branch); err == nil || !strings.Contains(err.Error(), "publication-ancestor") {
		t.Fatalf("a shared branch whose manifest is another publication's = %v, want refused", err)
	}
}

func TestValidateGraphRootObjectsAllowsInheritedOrigin(t *testing.T) {
	if err := validateGraphRootObjects(9, []string{"memory/branch/main/resource-0000"}, []string{"0000000000000008-03-0000000000000001"}); err != nil {
		t.Fatalf("inherited immutable origin was rejected: %v", err)
	}
	for _, test := range []struct {
		name    string
		objects []string
	}{
		{name: "missing", objects: nil},
		{name: "wrong kind", objects: []string{"0000000000000008-02-0000000000000001"}},
		{name: "malformed", objects: []string{"not-an-object"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGraphRootObjects(9, []string{"memory/branch/main/resource-0000"}, test.objects); err == nil {
				t.Fatal("invalid immutable root-object metadata was accepted")
			}
		})
	}
}

func TestSharedSnapshotNeverUsesReclaimedLocalObjectsAsAuthority(t *testing.T) {
	for _, state := range []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotSealedLocal, ttcsnapshot.SnapshotSealedVolatile} {
		if !localSnapshotObjectsAuthoritative(state) {
			t.Errorf("%s should use local authoritative objects", state)
		}
	}
	for _, state := range []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotWorking, ttcsnapshot.SnapshotShared} {
		if localSnapshotObjectsAuthoritative(state) {
			t.Errorf("%s must resolve through its provider/shared authority", state)
		}
	}
}

func TestGraphReferenceRootsPreserveLogicalSlots(t *testing.T) {
	bindings := []cluster.SnapshotRootBinding{
		{Slot: 7, Object: "000000000000002a-03-0000000000000001"},
		{Slot: 11, Object: "000000000000002a-03-0000000000000001"},
	}
	roots, err := graphReferenceRoots(42, "tx-reference", bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots["remote/42/tx-reference/00000007"] != roots["remote/42/tx-reference/0000000b"] {
		t.Fatalf("reference roots=%+v", roots)
	}
	if _, err := graphReferenceRoots(42, "tx-reference", append(bindings, bindings[0])); err == nil {
		t.Fatal("duplicate logical slot was accepted")
	}
	if _, err := graphReferenceRoots(42, "tx-reference", []cluster.SnapshotRootBinding{{Slot: 1, Object: "0000000000000029-03-0000000000000001"}}); err == nil {
		t.Fatal("root from another origin was accepted")
	}
}

func TestGroupGraphRootBindingsByInheritedOrigin(t *testing.T) {
	groups, err := groupGraphRootBindings([]string{
		"000000000000002a-03-0000000000000001",
		"0000000000000011-03-0000000000000009",
		"000000000000002a-03-0000000000000007",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("origin groups=%+v, want two", groups)
	}
	if got := groups[42]; len(got) != 2 || got[0].Slot != 0 || got[1].Slot != 2 {
		t.Fatalf("origin 42 bindings=%+v", got)
	}
	if got := groups[17]; len(got) != 1 || got[0].Slot != 1 {
		t.Fatalf("origin 17 bindings=%+v", got)
	}
	if _, err := groupGraphRootBindings([]string{"000000000000002a-01-0000000000000001"}); err == nil {
		t.Fatal("non-memory root was accepted")
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"completion", shell}, &stdout, &stderr, &fakeBackend{}); status != 0 {
				t.Fatalf("status=%d stderr=%q", status, stderr.String())
			}
			if !strings.Contains(stdout.String(), "command ttc") || !strings.Contains(stdout.String(), "_complete") {
				t.Errorf("completion script=%q does not invoke dynamic completion", stdout.String())
			}
		})
	}
}

func TestDynamicCompletionUsesTTCObjects(t *testing.T) {
	b := &fakeBackend{
		families: []Family{{Name: "demo", Branches: []Branch{
			{ID: "main", State: "running"}, {ID: "child", State: "paused"},
		}}},
		imageList:       []ImageInfo{{Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abc"}},
		execSessionList: []runscExecSession{{ID: "exec-123", State: "running"}},
	}
	platformCompletions := runtimePlatformCompletions()
	tests := []struct {
		args    []string
		want    []string
		notWant string
	}{
		{args: []string{"_complete", "pa"}, want: []string{"pause"}},
		{args: []string{"_complete", "daemon", "start", ""}, want: []string{"--platform", "--unsafe-no-host-isolation"}},
		{args: []string{"_complete", "daemon", "start", "--platform", ""}, want: platformCompletions},
		{args: []string{"_complete", "unp"}, want: []string{"unpause"}},
		{args: []string{"_complete", "unpause", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "pause", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "cp", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "rm", ""}, want: []string{"-f", "--force", "demo", "demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "image", "inspect", "docker"}, want: []string{"docker.io/library/alpine:3.20"}},
		{args: []string{"_complete", "exec", "wait", "demo@main", "exec-"}, want: []string{"exec-123"}},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if status := runCommand(test.args, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%v status=%d stderr=%q", test.args, status, stderr.String())
		}
		lines := strings.Fields(stdout.String())
		for _, want := range test.want {
			if !slices.Contains(lines, want) {
				t.Errorf("%v output=%q, want %q", test.args, stdout.String(), want)
			}
		}
		if test.notWant != "" && slices.Contains(lines, test.notWant) {
			t.Errorf("%v output=%q unexpectedly contains %q", test.args, stdout.String(), test.notWant)
		}
	}
	if b.execControlTarget != (selector{container: "demo", branch: "main"}) {
		t.Errorf("exec completion target=%+v", b.execControlTarget)
	}
}

func TestParseManagedCoWHostMount(t *testing.T) {
	mounts, err := parseHostMounts(
		[]string{"/host/a:/data:cow,immutable-source", "/host/c:/lazy:lazy_copy", "/host/d:/lazy2:rw,lazy-copy"},
		[]string{"type=bind,source=/host/b,target=/config,managed-cow=true,immutable-source", "type=bind,source=/host/e,target=/lazy3,lazy_copy"},
	)
	if err != nil {
		t.Fatalf("parseHostMounts: %v", err)
	}
	if len(mounts) != 5 {
		t.Fatalf("managed CoW host mounts=%+v", mounts)
	}
	for _, mount := range mounts {
		if !mount.ManagedCoW || mount.Readonly || mount.Direct || mount.Copy {
			t.Errorf("managed CoW host mount=%+v", mount)
		}
	}
	if !mounts[0].ImmutableSource || !mounts[3].ImmutableSource {
		t.Errorf("managed CoW host mounts=%+v, want immutable sources", mounts)
	}
	for _, value := range []string{"/host:/data:ro,cow,immutable-source", "/host:/data:cow,copy", "/host:/data:ro,lazy_copy"} {
		if _, err := parseHostMounts([]string{value}, nil); err == nil {
			t.Errorf("parseHostMounts accepted invalid managed CoW mount %q", value)
		}
	}
}

func TestParseCopiedHostMount(t *testing.T) {
	mounts, err := parseHostMounts(
		[]string{"/host/a:/data:copy"},
		[]string{"type=bind,source=/host/b,target=/config,copy=true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 || !mounts[0].Copy || !mounts[1].Copy || mounts[0].ManagedCoW || mounts[1].ManagedCoW {
		t.Errorf("copied host mounts=%+v", mounts)
	}
}

func TestInspectManagedCoWHostMount(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{},
		Mounts:  []specs.Mount{{Type: "bind", Source: "/host/lower", Destination: "/data", Options: []string{"rbind", "rw"}}},
		Annotations: map[string]string{
			ttcbundle.ImmutableHostMountsAnnotation:  `["/data"]`,
			ttcbundle.ManagedCoWHostMountsAnnotation: `["/data"]`,
		},
	}
	info := buildInspectInfo(ttcbundle.ContainerInfo{Name: "demo"}, spec, nil, "")
	if len(info.Mounts) != 1 || !info.Mounts[0].RW || info.Mounts[0].Mode != "rw,lazy_copy" {
		t.Errorf("managed CoW inspect mounts=%+v", info.Mounts)
	}
	if got, want := info.HostConfig.Binds, []string{"/host/lower:/data:rw,lazy_copy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("managed CoW binds=%q, want %q", got, want)
	}
	if got := info.TTC.ExternalViews; len(got) != 1 || !got[0].ManagedCoW || !got[0].ImmutableSourceAsserted || got[0].Readonly || got[0].Semantics != "branch-local-persistent-cow-over-user-asserted-immutable-host-lower" {
		t.Errorf("managed CoW external view=%+v", got)
	}
}

func TestInspectDirectHostMount(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{},
		Mounts:  []specs.Mount{{Type: "bind", Source: "/host/data", Destination: "/data", Options: []string{"rbind", "rw"}}},
		Annotations: map[string]string{
			ttcbundle.DirectHostMountsAnnotation: `["/data"]`,
		},
	}
	info := buildInspectInfo(ttcbundle.ContainerInfo{Name: "demo"}, spec, nil, "")
	if len(info.Mounts) != 1 || !info.Mounts[0].RW || info.Mounts[0].Mode != "rw" {
		t.Errorf("direct inspect mounts=%+v", info.Mounts)
	}
	if got, want := info.HostConfig.Binds, []string{"/host/data:/data:rw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("direct binds=%q, want %q", got, want)
	}
	if got := info.TTC.ExternalViews; len(got) != 1 || got[0].ManagedCoW || got[0].Readonly || got[0].Semantics != "shared-external-writable-view" {
		t.Errorf("direct external view=%+v", got)
	}
}

func TestInspectCopiedHostMount(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{},
		Mounts: []specs.Mount{{
			Type: "ttcvol", Source: "/run/ttc/host-mounts/id", Destination: "/data", Options: []string{"rw"},
		}},
		Annotations: map[string]string{
			ttcbundle.CopiedHostMountsAnnotation: `{"/data":"/host/source"}`,
		},
	}
	info := buildInspectInfo(ttcbundle.ContainerInfo{Name: "demo"}, spec, nil, "")
	if len(info.Mounts) != 1 || info.Mounts[0].Type != "bind" || info.Mounts[0].Source != "/host/source" || info.Mounts[0].Mode != "rw,copy" {
		t.Errorf("copied inspect mounts=%+v", info.Mounts)
	}
	if got, want := info.HostConfig.Binds, []string{"/host/source:/data:rw,copy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copied binds=%q, want %q", got, want)
	}
	if len(info.TTC.ExternalViews) != 0 {
		t.Errorf("copied mount reported as external view: %+v", info.TTC.ExternalViews)
	}
}

func TestColdBranchManifestRequiresGraphFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	legacy := coldBranchManifest{Version: 1, Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "stop"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readColdBranchManifest(path); err == nil || !strings.Contains(err.Error(), "snapshot-graph format 5") {
		t.Fatalf("legacy cold manifest error=%v", err)
	}
	root := ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	current := coldBranchManifest{
		Version: 5, Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "stop",
		InternalMounts: []cluster.InternalMountDescriptor{{Destination: "/etc/hostname", Contents: []byte("worker\n"), Mode: 0644}},
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "0123456789abcdef0123456789abcdef",
			BranchID:  "main", Root: root.String(), NextInode: 2,
		}, Origins: []uint64{17}}},
	}
	data, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readColdBranchManifest(path); err != nil || len(got.Filesystems) != 1 || len(got.InternalMounts) != 1 || string(got.InternalMounts[0].Contents) != "worker\n" {
		t.Fatalf("current cold manifest=%+v, %v", got, err)
	}
	current.InternalMounts = append(current.InternalMounts, current.InternalMounts[0])
	data, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readColdBranchManifest(path); err == nil || !strings.Contains(err.Error(), "repeats internal mount") {
		t.Fatalf("duplicate internal mount error=%v", err)
	}
}

func TestColdManifestIdentityRetainsActivatedFilesystemAncestor(t *testing.T) {
	manifest := coldBranchManifest{
		Container: "physical", Branch: "child", PublicationID: "publication-cold",
	}
	info := ttcbundle.ContainerInfo{ID: "physical"}
	for _, state := range []string{"running", "paused", "exited"} {
		branch := ttcbundle.BranchInfo{
			ID: "child", State: state, StorageState: "local",
			StorageManifest: "/shared/cold.json",
		}
		if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
			t.Errorf("activated %s ancestor rejected: %v", state, err)
		}
	}
	if err := validateColdManifestIdentity(info, ttcbundle.BranchInfo{
		ID: "child", State: "exited", StorageState: "shared", StorageManifest: "/shared/cold.json",
	}, manifest); err != nil {
		t.Fatalf("consumed stopped filesystem ancestor rejected: %v", err)
	}
	// A publication that failed after its commit left the branch in error
	// before a stop retired it: the manifest is still its ancestor.
	if err := validateColdManifestIdentity(info, ttcbundle.BranchInfo{
		ID: "child", State: "exited", StorageState: "error", StorageManifest: "/shared/cold.json",
	}, manifest); err != nil {
		t.Fatalf("stopped filesystem ancestor of a failed completion rejected: %v", err)
	}
	branch := ttcbundle.BranchInfo{
		ID: "child", State: "dehydrated", StorageState: "shared",
		PublicationID: "publication-cold",
	}
	if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
		t.Fatalf("matching cold publication rejected: %v", err)
	}
	branch.PublicationID = "publication-other"
	if err := validateColdManifestIdentity(info, branch, manifest); err == nil {
		t.Fatal("mismatched live publication was accepted")
	}
	branch.PublicationID = ""
	if err := validateColdManifestIdentity(info, branch, manifest); err == nil {
		t.Fatal("dehydrated branch without its publication fence was accepted")
	}
	branch.State, branch.StorageState = "running", "local"
	manifest.Container = "other"
	if err := validateColdManifestIdentity(info, branch, manifest); err == nil {
		t.Fatal("foreign physical manifest was accepted as an ancestor")
	}
}

// A branch forked in a remote fork's target container names the container's
// transaction, which the authority retires once the fork is terminal and its
// target holds no placement. Completing the branch's dehydration made the
// fork independent, and failed on the retired transaction, "file does not
// exist", on every retry.
func TestAdvanceDehydratedForkPassesOverARetiredFork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	transactions, err := cluster.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceDehydratedFork(transactions, "fork-0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("completing the dehydration of a branch whose fork is retired: %v", err)
	}
}

// The runtime has no branch that is not live, and of one exited or
// dehydrated it answered that the branch "does not exist", or could not load
// the family's runtime at all. A pause, a resume and a fork are refused by the
// state the branch's record gives, as an exec is.
func TestLifecycleStateRefusal(t *testing.T) {
	info := ttcbundle.ContainerInfo{ID: "physical", Branches: []ttcbundle.BranchInfo{
		{ID: "live", State: "running"}, {ID: "held", State: "paused"},
		{ID: "gone", State: "exited"}, {ID: "cold", State: "dehydrated"},
	}}
	named := selector{container: "web", branch: "b"}
	for _, test := range []struct {
		branch, verb string
		states       []string
		refused      bool
	}{
		{"live", "pause", []string{"running", "paused"}, false},
		{"held", "resume", []string{"running", "paused"}, false},
		{"gone", "pause", []string{"running", "paused"}, true},
		{"cold", "resume", []string{"running", "paused"}, true},
		{"cold", "fork", []string{"running", "paused", "dehydrated", "dehydrating"}, false},
		{"gone", "fork", []string{"running", "paused", "dehydrated", "dehydrating"}, true},
	} {
		err := lifecycleStateRefusal(info, selector{container: "physical", branch: test.branch}, named, test.verb, test.states...)
		if refused := err != nil; refused != test.refused {
			t.Errorf("%s of a branch %s: %v, want refused %t", test.verb, test.branch, err, test.refused)
		} else if refused && !strings.Contains(err.Error(), "cannot "+test.verb+" web@b: branch is ") {
			t.Errorf("%s of a branch %s refused as %q", test.verb, test.branch, err)
		}
	}
}

// A canceled publication's cleanup canceled every handoff it began. One that
// had reached the shared coordinator, which then held its baseline and its
// mutations as numbered, began again from one, which the coordinator refused
// for good, "handoff retry does not match mutation 1": the origin stayed
// frozen, and every fork sharing it waited. A handoff whose bridge the
// publication recorded, as it does before it contacts the coordinator, is
// adopted as the origin's own and rolled forward; one another operation
// recorded is that one's; one with no bridge is canceled.
func TestCanceledPublicationKeepsAHandoffItMaySend(t *testing.T) {
	root := t.TempDir()
	const ours, theirs = "publication-ours", "publication-theirs"
	for origin, intent := range map[ttcsnapshot.SnapshotID]sharedPublicationBridgeIntent{
		2: {Kind: sharedPublicationBridgePublication, Family: "family", Branch: "main", PublicationID: ours},
		3: {Kind: sharedPublicationBridgePublication, Family: "family", Branch: "other", PublicationID: theirs},
		4: {Kind: sharedPublicationBridgeOrigin},
	} {
		intent.Version, intent.Snapshot, intent.CreatedAt = sharedPublicationBridgeIntentVersion, uint64(origin), time.Now().UTC()
		if err := persistSharedPublicationBridgeIntent(root, intent); err != nil {
			t.Fatal(err)
		}
	}
	for origin, want := range map[ttcsnapshot.SnapshotID][2]bool{1: {true, false}, 2: {false, true}, 3: {false, false}, 4: {false, false}} {
		cancel, adopt, err := canceledOriginHandoffBridge(root, ours, origin)
		if err != nil || cancel != want[0] || adopt != want[1] {
			t.Errorf("origin %d: cancel %t, adopt %t, %v; want %t, %t", origin, cancel, adopt, err, want[0], want[1])
		}
	}
	// The canceled publication is no longer live in the authority.
	if err := adoptOriginHandoff(root, 2, &cluster.AuthorityState{}); err != nil {
		t.Fatal(err)
	}
	if bridge, found, err := loadSharedPublicationBridgeIntent(root, 2); err != nil || !found || bridge.Kind != sharedPublicationBridgeOrigin {
		t.Fatalf("adopted handoff's bridge: %+v, %t, %v; want the origin's own", bridge, found, err)
	}
}

// A fork's memory roots retain what the source's do, which a snapshot frozen
// for a handoff refuses: the fork waits the handoff out before it forks the
// runtime, rather than fork, fail and destroy its children.
func TestGraphMemoryRootsFrozen(t *testing.T) {
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{snapshotGraph: graph}
	object := ttcsnapshot.ObjectID{Snapshot: origin, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}.String()
	if err := b.graphMemoryRootsFrozen([]string{object}); err != nil {
		t.Fatalf("roots of a snapshot not frozen: %v", err)
	}
	if err := graph.Seal(origin, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if err := graph.FreezeSnapshotLedgerHandoff(origin); err != nil {
		t.Fatal(err)
	}
	if err := b.graphMemoryRootsFrozen([]string{object}); !frozenForAuthorityHandoff(err) {
		t.Fatalf("roots of a frozen snapshot: %v, want a handoff's refusal", err)
	}
}

func TestColdExecConsolePackRoundTrip(t *testing.T) {
	shared, active := t.TempDir(), t.TempDir()
	info := ttcbundle.ContainerInfo{ID: "physical", ConsoleLogMaxFiles: 3}
	directory := coldExecConsoleDir(active, info.ID, "main")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "build.log")
	if err := os.WriteFile(ttcconsole.GenerationPath(logPath, 1), []byte("older\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("newer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := coldManifestPath(shared, info.ID, "main", "publication-console")
	logs, size, err := captureColdExecConsolePack(active, info, "main", manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ExecID != "build" || logs[0].Offset != 0 || logs[0].Length != uint64(len("older\nnewer\n")) || size != logs[0].Length {
		t.Fatalf("console descriptors=%+v size=%d", logs, size)
	}
	manifest := coldBranchManifest{
		Version: 5, Container: info.ID, Branch: "main", PublicationID: "publication-console", Kind: "stop",
		ExecConsoleLogs: logs, ConsolePackSize: size,
	}
	if err := writeDurableJSON(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := readColdBranchManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	// A previous residence may leave a prefix in this node's disposable cache.
	if err := os.WriteFile(logPath, []byte("older\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installColdExecConsolePack(active, info, "main", loaded, manifestPath); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(logPath); err != nil || string(got) != "older\nnewer\n" {
		t.Fatalf("restored console output=%q, err=%v", got, err)
	}
	if err := installColdExecConsolePack(active, info, "main", loaded, manifestPath); err != nil {
		t.Fatalf("idempotent restore: %v", err)
	}
	if got, err := os.ReadFile(logPath); err != nil || string(got) != "older\nnewer\n" {
		t.Fatalf("idempotent restored console output=%q, err=%v", got, err)
	}
}

func TestColdDehydrationManifestRequiresSameOriginControlRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	manifest := coldBranchManifest{
		Version: 5, Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "dehydrate",
		MemorySnapshot: 17,
		ControlRoot:    ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 9, Kind: ttcsnapshot.ObjectKindMetadata}.String(),
	}
	write := func() {
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := readColdBranchManifest(path); err != nil {
		t.Fatalf("graph-native dehydration manifest rejected: %v", err)
	}
	manifest.ControlRoot = ttcsnapshot.ObjectID{Snapshot: 18, Ordinal: 9, Kind: ttcsnapshot.ObjectKindMetadata}.String()
	write()
	if _, err := readColdBranchManifest(path); err == nil || !strings.Contains(err.Error(), "graph-native control state") {
		t.Fatalf("foreign control origin error=%v", err)
	}
}

func TestColdStorageReclaimsRetiredGraphHandoffManifest(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	root := filepath.Join(shared, "cluster", "memory-manifests")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(root, "fork-retired.manifest")
	live := filepath.Join(root, "fork-live.manifest")
	independent := filepath.Join(root, "fork-independent.manifest")
	unbound := filepath.Join(root, "fork-unbound.manifest")
	unknown := filepath.Join(root, "operator-note.manifest")
	for _, path := range []string{retired, live, independent, unbound, unknown} {
		if err := os.WriteFile(path, []byte("descriptor"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := transactionListCatalog{records: []cluster.Transaction{
		{Identity: cluster.ForkIdentity{TransactionID: "fork-live"}, State: cluster.TransactionPublished},
		{
			Identity:     cluster.ForkIdentity{TransactionID: "fork-independent"},
			State:        cluster.TransactionIndependent,
			GraphHandoff: &cluster.GraphHandoffDescriptor{GraphManifest: filepath.Join("cluster", "memory-manifests", "fork-independent.manifest")},
		},
		{Identity: cluster.ForkIdentity{TransactionID: "fork-unbound"}, State: cluster.TransactionIndependent},
	}}
	if err := collectColdStorage(shared, local, nil, catalog, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired graph handoff manifest remains: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown file was not left untouched: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("nonterminal graph handoff manifest was reclaimed: %v", err)
	}
	if _, err := os.Stat(independent); err != nil {
		t.Fatalf("bound independent graph handoff manifest was reclaimed: %v", err)
	}
	if _, err := os.Stat(unbound); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unbound terminal graph handoff manifest remains: %v", err)
	}
}

func TestFinalColdStorageCollectionHasNoPublicationGraceResidue(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	orphan := filepath.Join(shared, "cold", "manifests", "family", "branch", "0000000000000001.json")
	if err := os.MkdirAll(filepath.Dir(orphan), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := collectColdStorage(shared, local, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("ordinary grace did not retain a recent publication: %v", err)
	}
	if err := collectColdStorageOlderThan(shared, local, nil, nil, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final collection retained unreachable publication: %v", err)
	}
}

func TestColdStorageRetainsDistributedPhysicalBranchManifest(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	const physicalID = "fork-physical-child"
	manifestPath := coldManifestPath(shared, physicalID, "child", "publication-child")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	root := ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	manifest := coldBranchManifest{
		Version: 5, Container: physicalID, Branch: "child", PublicationID: "publication-child", Kind: "stop",
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "0123456789abcdef0123456789abcdef", BranchID: "child",
			Root: root.String(), NextInode: 2,
		}, Origins: []uint64{17}}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(manifestPath, old, old); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(shared, "containers", physicalID)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: physicalID, Name: physicalID, BundleDir: bundleDir,
		DistributedFamilyID: "logical-family", DistributedForkTransactionID: "fork-child",
		Branches: []ttcbundle.BranchInfo{{
			ID: "child", State: "dehydrated", StorageState: "shared", PublicationID: "publication-child", StorageManifest: manifestPath,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := collectColdStorageOlderThan(shared, local, nil, nil, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("distributed physical branch manifest was reclaimed: %v", err)
	}
}

func TestColdStorageRetainsAuthorityBranchBackingAfterPhysicalCleanup(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	manifestPath := coldManifestPath(shared, "fork-old-physical", "fork-old-physical", "publication-old")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	root := ttcsnapshot.ObjectID{Snapshot: 29, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	manifest := coldBranchManifest{
		Version: 5, Container: "fork-old-physical", Branch: "fork-old-physical", PublicationID: "publication-old", Kind: "stop",
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "0123456789abcdef0123456789abcdef", BranchID: "fork-old-physical",
			Root: root.String(), NextInode: 2,
		}, Origins: []uint64{29}}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(manifestPath, old, old); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(shared, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	authority := &cluster.AuthorityState{Branches: map[string]cluster.BranchAuthority{
		"logical-family\x00main": {
			FamilyID: "logical-family", BranchID: "main", Lifecycle: "running",
			Roots: cluster.DurableRoots{FilesystemRoot: filepath.ToSlash(relative)},
		},
	}}
	// The old physical container record has already been removed. The logical
	// branch the authority records is nevertheless still allowed to fault from this immutable
	// manifest, so terminal collection must retain it.
	if err := collectColdStorageOlderThan(shared, local, nil, nil, authority, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("authority-selected branch backing was reclaimed: %v", err)
	}
}

func TestEmitCapacityRequestCoalescesUntilAcknowledged(t *testing.T) {
	dataRoot := t.TempDir()
	backend := &runtimeBackend{
		dataRoot: dataRoot,
		distributedSource: &distributedSourceRuntime{capabilities: cluster.Capabilities{
			Architecture: "amd64",
			RepositoryID: "shared-repository",
		}},
	}
	source := cluster.NodeRecord{
		NodeID:             "node-a",
		MemoryTargetBytes:  8 << 30,
		MemoryCurrentBytes: 9 << 30,
	}
	path, err := backend.emitCapacityRequest(source, "memory-pressure")
	if err != nil {
		t.Fatal(err)
	}
	read := func() elasticCapacityRequest {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request elasticCapacityRequest
		if err := json.Unmarshal(contents, &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	first := read()
	if first.Version != 1 || first.Operation != "acquire" || first.Nodes != 1 || first.MemoryBytesEach != source.MemoryTargetBytes || first.Architecture != "amd64" || first.RepositoryID != "shared-repository" || first.SourceNode != source.NodeID || first.Reason != "memory-pressure" || first.RequestID == "" || !first.Deadline.After(first.CreatedAt) {
		t.Fatalf("capacity request=%+v", first)
	}
	coalescedPath, err := backend.emitCapacityRequest(source, "node-drain")
	if err != nil {
		t.Fatal(err)
	}
	if coalescedPath != path {
		t.Fatalf("coalesced path=%q, want %q", coalescedPath, path)
	}
	if got := read(); got.RequestID != first.RequestID || got.Reason != first.Reason {
		t.Fatalf("coalesced request=%+v, want original ID and reason", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newPath, err := backend.emitCapacityRequest(source, "node-drain")
	if err != nil {
		t.Fatal(err)
	}
	if newPath != path {
		t.Fatalf("replacement path=%q, want %q", newPath, path)
	}
	if got := read(); got.RequestID == first.RequestID || got.Reason != "node-drain" {
		t.Fatalf("replacement request=%+v, original=%+v", got, first)
	}
}

func TestElasticDrainIntentSurvivesRetryAndIsConsumedAfterCompletion(t *testing.T) {
	dataRoot := t.TempDir()
	first, err := beginElasticDrainIntent(dataRoot, "node-a", "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != elasticDrainActive || first.Attempts != 1 || first.OperationID == "" {
		t.Fatalf("first intent=%+v", first)
	}
	retry, err := beginElasticDrainIntent(dataRoot, "node-a", "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if retry.OperationID != first.OperationID || retry.Attempts != 2 {
		t.Fatalf("retry intent=%+v, first=%+v", retry, first)
	}
	retargeted, err := beginElasticDrainIntent(dataRoot, "node-a", "node-c")
	if err != nil || retargeted.OperationID != first.OperationID || retargeted.TargetNode != "node-c" || retargeted.Attempts != 3 {
		t.Fatalf("retargeted intent=(%+v, %v)", retargeted, err)
	}
	attemptErr := errors.New("target temporarily unavailable")
	if err := finishElasticDrainAttempt(dataRoot, "node-a", first.OperationID, attemptErr); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.State != elasticDrainActive || loaded.LastError != attemptErr.Error() || loaded.CompletedAt != nil {
		t.Fatalf("failed intent=%+v", loaded)
	}
	if err := finishElasticDrainAttempt(dataRoot, "node-a", first.OperationID, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.State != elasticDrainComplete || loaded.LastError != "" || loaded.CompletedAt == nil {
		t.Fatalf("completed intent=%+v", loaded)
	}
	if err := removeCompletedElasticDrainIntent(dataRoot, "node-a"); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil || loaded != nil {
		t.Fatalf("consumed intent=(%+v, %v), want nil", loaded, err)
	}
}

func TestElasticCommandsTakeMutationLock(t *testing.T) {
	for _, args := range [][]string{{"migrate", "--node", "node-b", "demo@main"}, {"node", "stop", "--migrate-all", "node-a"}, {"node", "rebalance", "node-a"}} {
		if !daemonCommandNeedsMutationLock(args) {
			t.Errorf("%v does not take the mutation lock", args)
		}
	}
	if daemonCommandNeedsMutationLock([]string{"node", "ls", "--json"}) {
		t.Error("node ls unnecessarily takes the mutation lock")
	}
}

func TestPersistentFamilyRequiredRuntime(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		NodeID: "node-a",
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", NodeID: "node-a", State: "created"},
			{ID: "cold", NodeID: "node-a", State: "dehydrated"},
			{ID: "remote", NodeID: "node-b", State: "running"},
		},
	}
	if persistentFamilyRequiredRuntime(container, "node-a") {
		t.Fatal("process-free local branches unexpectedly require a runtime")
	}
	if !persistentFamilyRequiredRuntime(container, "node-b") {
		t.Fatal("running remote branch did not require its node runtime")
	}
	container.Branches[0].State = "paused"
	if !persistentFamilyRequiredRuntime(container, "node-a") {
		t.Fatal("paused local branch did not require its runtime")
	}
}

func TestPendingDistributedTargetProtectsHiddenRuntime(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-a", State: "running"}},
		DistributedForkImports: map[string]ttcbundle.DistributedForkImport{
			"tx-migrate": {
				BranchID: "main", TargetNode: "node-b", SourceNode: "node-a",
				Migration: true, State: "paused",
			},
			"tx-fork": {
				BranchID: "child", TargetNode: "node-b", SourceNode: "node-a",
				Migration: false, State: "paused",
			},
		},
	}
	if !pendingDistributedTarget(container, "main", "node-b") {
		t.Fatal("hidden migration target was not protected on its target node")
	}
	if pendingDistributedTarget(container, "main", "node-a") {
		t.Fatal("hidden migration target protected the source node")
	}
	if !pendingDistributedTarget(container, "child", "node-b") {
		t.Fatal("ordinary remote fork target was not protected before publication")
	}
	container.DistributedForkImports = nil
	container.Branches = append(container.Branches, ttcbundle.BranchInfo{
		ID: "child", NodeID: "node-b", State: "running", DistributedForkTransactionID: "tx-fork",
	})
	if !pendingDistributedTarget(container, "child", "node-b") {
		t.Fatal("visible target was not protected before authority placement publication")
	}
}

func TestLocalFamilyStateNeeded(t *testing.T) {
	info := ttcbundle.ContainerInfo{NodeID: "node-a", Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-a", State: "running"}}}
	if localFamilyStateNeeded(info, "node-b") {
		t.Fatal("remote-only family retained node-local state")
	}
	info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "child", NodeID: "node-b", State: "running"})
	if !localFamilyStateNeeded(info, "node-b") {
		t.Fatal("locally owned branch did not retain node-local state")
	}
	info.Branches = info.Branches[:1]
	info.DistributedForkImports = map[string]ttcbundle.DistributedForkImport{
		"tx": {BranchID: "child", TargetNode: "node-b"},
	}
	if !localFamilyStateNeeded(info, "node-b") {
		t.Fatal("provisional import did not retain node-local state")
	}
}

func TestPendingPublicationProtectsPhysicalContainer(t *testing.T) {
	intents := []ttcstorage.Intent{
		{Container: "physical-a", AuthorityContainer: "family", Branch: "transaction-a"},
		{Container: "physical-b", AuthorityContainer: "family", Branch: "transaction-b"},
	}
	for _, test := range []struct {
		container string
		want      bool
	}{{"physical-a", true}, {"physical-b", true}, {"family", false}, {"", false}} {
		if got := pendingPublicationProtectsContainer(intents, test.container); got != test.want {
			t.Errorf("pendingPublicationProtectsContainer(%q)=%t, want %t", test.container, got, test.want)
		}
	}
}

func TestElasticNodeMutationRoutesToSourceBeforeLock(t *testing.T) {
	for _, args := range [][]string{
		{"node", "stop", "--migrate-all", "node-a"},
		{"node", "stop", "--_local", "--dehydrate-all", "node-a"},
		{"node", "rebalance", "node-a"},
	} {
		owner, _, err := daemonCommandPlacementWithCatalog(t.TempDir(), nil, args)
		if err != nil {
			t.Fatalf("daemonCommandPlacementWithCatalog(%v): %v", args, err)
		}
		if owner != "node-a" {
			t.Errorf("daemonCommandPlacementWithCatalog(%v)=%q, want node-a", args, owner)
		}
	}
}

func TestSelectAdmissionNodeUsesCompatibleLeastLoadedCapacity(t *testing.T) {
	capabilities := func(node string) cluster.Capabilities {
		policy := ttcremote.DefaultPolicy()
		policy.PageSize = 4096
		return cluster.Capabilities{
			NodeID: node, ProtocolMin: 1, ProtocolMax: 1, BuildID: "test", Architecture: "amd64",
			CPUProfile: "portable", CPUFeatures: []string{"sse", "sse2"},
			BasePageSize: 4096, MaxTransferSize: 2 << 20, RepositoryID: "shared",
			Features:     []string{cluster.FeatureRemotePull, cluster.FeatureNetworkRecreate, cluster.FeatureFilesystemBranch, cluster.FeatureSharedMemorySnapshot},
			PagingPolicy: policy,
		}
	}
	now := time.Unix(7000, 0).UTC()
	record := func(node string, current, target, reserved uint64) cluster.NodeRecord {
		return cluster.NodeRecord{
			NodeID: node, Incarnation: "boot-" + node, State: cluster.NodeReady,
			Capabilities: capabilities(node), MemoryCurrentBytes: current, MemoryTargetBytes: target,
			IncomingReservedBytes: reserved, LeaseExpiresAt: now.Add(time.Minute),
		}
	}
	local := capabilities("node-a")
	incompatible := record("node-c", 1<<30, 8<<30, 0)
	incompatible.Capabilities.Architecture = "arm64"
	selected, err := selectAdmissionNode([]cluster.NodeRecord{
		record("node-a", 6<<30, 8<<30, 0),
		record("node-b", 2<<30, 8<<30, 1<<30),
		incompatible,
	}, local, now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.NodeID != "node-b" {
		t.Fatalf("selected %q, want least-loaded compatible node-b", selected.NodeID)
	}
	full := record("node-b", 7<<30, 8<<30, 1<<30)
	if selected, err := selectAdmissionNode([]cluster.NodeRecord{full}, local, now); err == nil {
		t.Fatalf("selected full node %+v", selected)
	}
	selected, err = selectAdmissionNodeExcluding([]cluster.NodeRecord{
		record("node-a", 1<<30, 8<<30, 0),
		record("node-b", 2<<30, 8<<30, 0),
	}, local, now, map[string]struct{}{"node-a": {}})
	if err != nil {
		t.Fatal(err)
	}
	if selected.NodeID != "node-b" {
		t.Fatalf("selected excluded node %q, want node-b", selected.NodeID)
	}
	// A start or a rehydration goes to a node of its family, however loaded,
	// while one accepts it.
	family := map[string]struct{}{"node-b": {}}
	loaded := []cluster.NodeRecord{record("node-a", 1<<30, 8<<30, 0), record("node-b", 6<<30, 8<<30, 0)}
	if selected, err := selectAdmissionNodePreferring(loaded, local, now, nil, family); err != nil || selected.NodeID != "node-b" {
		t.Fatalf("selected (%q, %v), want the family's node-b", selected.NodeID, err)
	}
	if selected, err := selectAdmissionNodePreferring(loaded, local, now, nil, nil); err != nil || selected.NodeID != "node-a" {
		t.Fatalf("selected (%q, %v) for no family, want the least loaded node-a", selected.NodeID, err)
	}
	atTarget := []cluster.NodeRecord{record("node-a", 1<<30, 8<<30, 0), record("node-b", 8<<30, 8<<30, 0)}
	if selected, err := selectAdmissionNodePreferring(atTarget, local, now, nil, family); err != nil || selected.NodeID != "node-a" {
		t.Fatalf("selected (%q, %v) with the family's node at its target, want node-a", selected.NodeID, err)
	}
	if selected, err := selectAdmissionNodePreferring(loaded, local, now, family, family); err != nil || selected.NodeID != "node-a" {
		t.Fatalf("selected (%q, %v) with the family's node excluded, want node-a", selected.NodeID, err)
	}
	// A node judges by its own record whether it still accepts what an entry
	// daemon chose it for, as selection judged it.
	if err := nodeAdmissionRefusal(record("node-a", 1<<30, 8<<30, 0), now); err != nil {
		t.Fatalf("a ready node below its target refused: %v", err)
	}
	pressured := record("node-a", 1<<30, 8<<30, 0)
	pressured.State = cluster.NodePressured
	if err := nodeAdmissionRefusal(pressured, now); err == nil || !strings.Contains(err.Error(), "state=pressured") {
		t.Fatalf("a pressured node: %v", err)
	}
	if err := nodeAdmissionRefusal(record("node-a", 7<<30, 8<<30, 1<<30), now); err == nil || !strings.Contains(err.Error(), "memory target") {
		t.Fatalf("a node at its target with what it reserved: %v", err)
	}
	if err := nodeAdmissionRefusal(record("node-a", 1<<30, 8<<30, 0), now.Add(2*time.Minute)); err == nil {
		t.Fatal("a node whose lease expired accepted")
	}
}

func TestParseDistributedFamilyMutation(t *testing.T) {
	for _, test := range []struct {
		args    []string
		prefix  []string
		targets []string
		handle  bool
	}{
		{args: []string{"rm", "-fv", "demo"}, prefix: []string{"rm", "-fv"}, targets: []string{"demo"}, handle: true},
		{args: []string{"stop", "--time", "2", "demo", "peer@main"}, prefix: []string{"stop", "--time", "2"}, targets: []string{"demo", "peer@main"}, handle: true},
		{args: []string{"dehydrate", "demo"}, prefix: []string{"dehydrate"}, targets: []string{"demo"}, handle: true},
		{args: []string{"stop", "demo@main"}, prefix: []string{"stop"}, targets: []string{"demo@main"}, handle: true},
		{args: []string{"pause", "demo"}, handle: false},
		{args: []string{"rm", "--unknown", "demo"}, handle: false},
	} {
		got, ok := parseDistributedFamilyMutation(test.args)
		if ok != test.handle {
			t.Errorf("parseDistributedFamilyMutation(%v) handled=%v, want %v", test.args, ok, test.handle)
			continue
		}
		if ok && (!slices.Equal(got.prefix, test.prefix) || !slices.Equal(got.targets, test.targets)) {
			t.Errorf("parseDistributedFamilyMutation(%v)=%+v, want prefix=%v targets=%v", test.args, got, test.prefix, test.targets)
		}
		if ok && got.removeVolumes != slices.Contains(test.args, "-fv") {
			t.Errorf("parseDistributedFamilyMutation(%v) removeVolumes=%t", test.args, got.removeVolumes)
		}
	}
	mutation, ok := parseDistributedFamilyMutation([]string{"rm", "-fv", "demo"})
	if !ok || !slices.Equal(mutation.branchPrefix(), []string{"rm", "-f"}) {
		t.Fatalf("distributed branch removal prefix=%v, want [rm -f]", mutation.branchPrefix())
	}
	first := distributedChildRequestID("parent", 2, []string{"stop", "demo@main"})
	if err := validateDaemonRequestID(first); err != nil {
		t.Fatalf("child request ID %q is not a valid daemon mutation ID: %v", first, err)
	}
	if second := distributedChildRequestID("parent", 2, []string{"stop", "demo@main"}); first != second {
		t.Fatalf("child request ID is not deterministic: %q != %q", first, second)
	}
	if other := distributedChildRequestID("parent", 3, []string{"stop", "demo@main"}); first == other {
		t.Fatalf("different child operations collided at %q", first)
	}
}

func TestDistributedMutationSourceTransactions(t *testing.T) {
	transaction := func(id, sourceNode, sourceBranch, targetBranch string, state cluster.TransactionState, needed, migration bool) cluster.Transaction {
		return cluster.Transaction{
			Identity: cluster.ForkIdentity{
				TransactionID: id, SourceNodeID: sourceNode, SourceContainer: "family-id", SourceBranch: sourceBranch,
				TargetContainer: "family-id", TargetBranch: id, TargetVisibleBranch: targetBranch, Migration: migration,
			},
			State: state, SourceNeeded: needed, SourceIncarnation: sourceNode + "-incarnation",
		}
	}
	transactions := []cluster.Transaction{
		transaction("incoming", "node-a", "main", "child", cluster.TransactionPublished, true, false),
		transaction("outgoing", "node-b", "child", "grandchild", cluster.TransactionDraining, true, false),
		transaction("active-migration", "node-e", "child", "child", cluster.TransactionPublished, true, true),
		transaction("independent-fork", "node-c", "main", "old", cluster.TransactionIndependent, false, false),
		transaction("independent-migration", "node-d", "main", "moved", cluster.TransactionIndependent, false, true),
	}
	info := ttcbundle.ContainerInfo{ID: "family-id"}
	all := distributedMutationSourceTransactions(transactions, info, "", nil)
	if len(all) != 4 {
		t.Fatalf("family settlement nodes=%v, want node-a, node-b, node-d, and node-e", all)
	}
	if _, ok := all["node-a"]["incoming"]; !ok {
		t.Fatalf("family settlement omitted incoming transaction: %v", all)
	}
	if _, ok := all["node-b"]["outgoing"]; !ok {
		t.Fatalf("family settlement omitted outgoing transaction: %v", all)
	}
	if _, ok := all["node-d"]["independent-migration"]; !ok {
		t.Fatalf("family settlement omitted independent migration cleanup: %v", all)
	}
	retired := distributedMutationSourceTransactions(transactions, info, "", map[string]string{
		"node-d": "node-d-incarnation",
		"node-e": "node-e-incarnation",
	})
	if _, ok := retired["node-d"]; ok {
		t.Fatalf("cleanly retired independent migration still requires its removed source: %v", retired)
	}
	if _, ok := retired["node-e"]["active-migration"]; !ok {
		t.Fatalf("retirement record hid an active source dependency: %v", retired)
	}
	wrongIncarnation := distributedMutationSourceTransactions(transactions, info, "", map[string]string{
		"node-d": "replacement-incarnation",
	})
	if _, ok := wrongIncarnation["node-d"]["independent-migration"]; !ok {
		t.Fatalf("replacement node incarnation hid old migration cleanup: %v", wrongIncarnation)
	}
	child := distributedMutationSourceTransactions(transactions, info, "child", nil)
	if len(child) != 3 {
		t.Fatalf("branch settlement nodes=%v, want incoming, outgoing, and active migration sources", child)
	}
	if got := distributedMutationSourceTransactions(transactions, ttcbundle.ContainerInfo{ID: "other"}, "", nil); len(got) != 0 {
		t.Fatalf("unrelated family selected transactions: %v", got)
	}
	settle, cancel, discard, _ := distributedRemovalSourceTransactions(transactions, nil, info, "")
	if len(settle) != 1 || len(cancel) != 2 || len(discard) != 2 {
		t.Fatalf("removal transaction split settle=%v cancel=%v discard=%v", settle, cancel, discard)
	}
	if _, ok := cancel["node-a"]["incoming"]; !ok {
		t.Fatalf("family removal would materialize obsolete incoming fork: %v", cancel)
	}
	if _, ok := cancel["node-b"]["outgoing"]; !ok {
		t.Fatalf("family removal would materialize obsolete outgoing fork: %v", cancel)
	}
	if _, ok := discard["node-d"]["independent-migration"]; !ok {
		t.Fatalf("removal did not defer independent migration to discard postflight: %v", discard)
	}
	if _, ok := discard["node-e"]["active-migration"]; !ok {
		t.Fatalf("removal omitted active migration from discard postflight: %v", discard)
	}
	if _, ok := settle["node-d"]; ok {
		t.Fatalf("removal would publish an obsolete independent migration: %v", settle)
	}
	branchSettle, branchCancel, _, _ := distributedRemovalSourceTransactions(transactions, nil, info, "child")
	if _, ok := branchCancel["node-a"]["incoming"]; !ok {
		t.Fatalf("target branch removal did not cancel its incoming fork: %v", branchCancel)
	}
	if _, ok := branchSettle["node-b"]["outgoing"]; !ok {
		t.Fatalf("source branch removal did not preserve its surviving child: %v", branchSettle)
	}
	sharedRuntime := []cluster.PlacementRecord{
		{FamilyID: "family-id", BranchID: "child", PhysicalContainer: "fork-runtime", PhysicalBranch: "child", OwnerNode: "node-a"},
		{FamilyID: "family-id", BranchID: "sibling", PhysicalContainer: "fork-runtime", PhysicalBranch: "sibling", OwnerNode: "node-a"},
	}
	sharedTransactions := append([]cluster.Transaction(nil), transactions...)
	sharedTransactions[0].Identity.TargetNodeID = "node-a"
	sharedSettle, sharedCancel, _, _ := distributedRemovalSourceTransactions(sharedTransactions, sharedRuntime, info, "child")
	if _, ok := sharedSettle["node-a"]["incoming"]; !ok {
		t.Fatalf("shared physical descendant did not retain its source handoff: settle=%v cancel=%v", sharedSettle, sharedCancel)
	}
	if _, ok := sharedCancel["node-a"]["incoming"]; ok {
		t.Fatalf("shared physical descendant allowed target-family cancellation: %v", sharedCancel)
	}

	// Rename leaves immutable transaction identities untouched. Reusing the
	// old branch name must not associate the new placement with the migration
	// that now backs the renamed branch.
	oldMigration := transaction("old-migration", "node-a", "remote", "remote", cluster.TransactionIndependent, false, true)
	newFork := transaction("new-fork", "node-a", "main", "remote", cluster.TransactionPublished, true, false)
	newFork.Identity.TargetNodeID = "node-b"
	renameReusePlacements := []cluster.PlacementRecord{
		{FamilyID: "family-id", BranchID: "moved", PhysicalContainer: "old-runtime", OwnerNode: "node-b", LastOperationID: "old-migration"},
		{FamilyID: "family-id", BranchID: "remote", PhysicalContainer: "new-runtime", OwnerNode: "node-b", OperationID: "new-fork"},
	}
	_, reusedCancel, reusedDiscard, _ := distributedRemovalSourceTransactions([]cluster.Transaction{oldMigration, newFork}, renameReusePlacements, info, "remote")
	if _, ok := reusedCancel["node-a"]["new-fork"]; !ok {
		t.Fatalf("reused branch removal did not select its current fork: %v", reusedCancel)
	}
	if _, ok := reusedDiscard["node-a"]["old-migration"]; ok {
		t.Fatalf("reused branch removal selected renamed migration history: %v", reusedDiscard)
	}
	if got := distributedTransactionLogicalBranch(oldMigration, renameReusePlacements); got != "moved" {
		t.Fatalf("renamed migration resolves to %q, want moved", got)
	}

	// Target cleanup happens before the cancellation CAS. Recovery must keep
	// selecting metadata-only removal after the transaction becomes terminal,
	// including when the public branch was renamed in the meantime.
	canceledFork := transaction("canceled-fork", "node-a", "main", "child", cluster.TransactionCanceled, false, false)
	canceledFork.Identity.TargetNodeID = "node-b"
	canceledPlacements := []cluster.PlacementRecord{{
		FamilyID: "family-id", BranchID: "renamed-child", OwnerNode: "node-b",
		PhysicalContainer: "fork-runtime", LastOperationID: "canceled-fork",
	}}
	_, recoveredCancel, _, _ := distributedRemovalSourceTransactions([]cluster.Transaction{canceledFork}, canceledPlacements, info, "renamed-child")
	if _, ok := recoveredCancel["node-a"]["canceled-fork"]; !ok {
		t.Fatalf("terminal canceled target did not retain metadata-only removal: %v", recoveredCancel)
	}

	corruptedPlacements := []cluster.PlacementRecord{
		{FamilyID: "family-id", BranchID: "child", State: cluster.PlacementCorrupted},
		{FamilyID: "family-id", BranchID: "grandchild", State: cluster.PlacementRunning},
	}
	corruptSettle, corruptCancel, corruptDiscard, _ := distributedRemovalSourceTransactions(transactions, corruptedPlacements, info, "child")
	if _, ok := corruptCancel["node-a"]; ok {
		t.Fatalf("corrupted target removal attempted remote cancellation: %v", corruptCancel)
	}
	if _, ok := corruptSettle["node-b"]["outgoing"]; !ok {
		t.Fatalf("corrupted source removal did not preserve a healthy child: %v", corruptSettle)
	}
	if _, ok := corruptDiscard["node-e"]["active-migration"]; ok {
		t.Fatalf("corrupted migration target retained physical cleanup: %v", corruptDiscard)
	}

	// A committed migration whose branch is removed with its target's runtime
	// is canceled after the target instead of drained into it; one whose
	// target runtime a surviving branch shares is still drained.
	committed := transaction("committed-migration", "node-f", "moved", "moved", cluster.TransactionDraining, true, true)
	committed.OwnershipCommitted = true
	committed.Identity.TargetNodeID = "node-g"
	committed.TargetPhysicalContainer = "move-runtime"
	movedPlacements := []cluster.PlacementRecord{
		{FamilyID: "family-id", BranchID: "moved", PhysicalContainer: "move-runtime", OwnerNode: "node-g", LastOperationID: "committed-migration"},
	}
	abortSettle, _, abortDiscard, abort := distributedRemovalSourceTransactions([]cluster.Transaction{committed}, movedPlacements, info, "moved")
	if _, ok := abort["node-f"]["committed-migration"]; !ok || len(abortSettle) != 0 || len(abortDiscard) != 0 {
		t.Fatalf("removing a migrated branch: settle=%v discard=%v abort=%v, want it aborted", abortSettle, abortDiscard, abort)
	}
	sharedMoved := append(movedPlacements, cluster.PlacementRecord{FamilyID: "family-id", BranchID: "local-fork", PhysicalContainer: "move-runtime", OwnerNode: "node-g"})
	keptSettle, _, _, keptAbort := distributedRemovalSourceTransactions([]cluster.Transaction{committed}, sharedMoved, info, "moved")
	if _, ok := keptSettle["node-f"]["committed-migration"]; !ok || len(keptAbort) != 0 {
		t.Fatalf("a migration a surviving branch reads was aborted: settle=%v abort=%v", keptSettle, keptAbort)
	}
}

// A remote fork's publication places its branch without naming the fork in
// the placement. Removing the branch before the fork has finished cancels the
// fork, which deletes the branch's runtime; the removal then removes the
// branch from the authority only, if the fork's runtime still holds it.
func TestUnfinishedRemoteForkHoldsThePlacementItPublished(t *testing.T) {
	fork := cluster.Transaction{
		Identity: cluster.ForkIdentity{
			TransactionID: "fork-1", SourceNodeID: "node-a", SourceContainer: "family-id", SourceBranch: "main",
			TargetNodeID: "node-b", TargetContainer: "family-id", TargetBranch: "fork-1", TargetVisibleBranch: "child",
		},
		State: cluster.TransactionPublished, SourceNeeded: true,
	}
	canceled := fork
	canceled.State = cluster.TransactionCanceled
	published := cluster.PlacementRecord{FamilyID: "family-id", BranchID: "child", PhysicalContainer: "fork-1", PhysicalBranch: "child", OwnerNode: "node-b"}
	imported := published
	imported.PhysicalContainer = "family-runtime"
	migrating := published
	migrating.OperationID = "migration"
	migrated := published
	migrated.OwnerNode, migrated.PhysicalContainer, migrated.LastOperationID = "node-c", "migration", "migration"
	recovered := published
	recovered.LastOperationID = "migration"
	for _, test := range []struct {
		name      string
		tx        cluster.Transaction
		placement cluster.PlacementRecord
		want      bool
	}{
		{"in a runtime of its own", fork, published, true},
		{"canceled, in a runtime of its own", canceled, published, true},
		{"imported into the family's runtime", fork, imported, true},
		{"canceled, a branch of its name in the family's runtime", canceled, imported, false},
		{"migrating", fork, migrating, false},
		{"migrated on", fork, migrated, false},
		{"after a failed migration", fork, recovered, true},
	} {
		if got := transactionOwnsCurrentPlacement(test.tx, []cluster.PlacementRecord{test.placement}); got != test.want {
			t.Errorf("%s: transactionOwnsCurrentPlacement = %t, want %t", test.name, got, test.want)
		}
	}
	// Removing the whole family also removes a branch forked from the
	// fork's target on its node, through the runtime they share, which
	// canceling the fork would delete: the fork is finished instead.
	info := ttcbundle.ContainerInfo{ID: "family-id"}
	if _, cancel, _, _ := distributedRemovalSourceTransactions([]cluster.Transaction{fork}, []cluster.PlacementRecord{published}, info, ""); len(cancel["node-a"]) != 1 {
		t.Fatalf("removing the family did not cancel its unfinished fork: %v", cancel)
	}
	descendant := published
	descendant.BranchID, descendant.PhysicalBranch = "grandchild", "grandchild"
	settle, cancel, _, _ := distributedRemovalSourceTransactions([]cluster.Transaction{fork}, []cluster.PlacementRecord{published, descendant}, info, "")
	if _, ok := settle["node-a"]["fork-1"]; !ok || len(cancel) != 0 {
		t.Fatalf("removing a family whose fork's runtime runs another branch: settle=%v cancel=%v, want the fork finished", settle, cancel)
	}
}

func TestOrderFamilyRemovalPlacementsTreatsEmptyPhysicalContainerAsCanonical(t *testing.T) {
	records := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "migrated", PhysicalContainer: "family", PhysicalBranch: "migration-private", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "", PhysicalBranch: "", State: cluster.PlacementCorrupted},
		{FamilyID: "family", BranchID: "remote", PhysicalContainer: "fork-runtime", PhysicalBranch: "remote", State: cluster.PlacementRunning},
	}
	orderFamilyRemovalPlacements(records, "family")
	if got := records[len(records)-1].BranchID; got != "main" {
		t.Fatalf("final removal branch=%q, want canonical main", got)
	}
}

// A family's removal removes the branches in its canonical container last,
// one after another, so that their node removes them in one command, after
// the branches of other containers.
func TestOrderFamilyRemovalPlacementsRemovesTheCanonicalContainerTogether(t *testing.T) {
	records := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "a", PhysicalContainer: "family", PhysicalBranch: "a", OwnerNode: "n2"},
		{FamilyID: "family", BranchID: "main", PhysicalContainer: "family", PhysicalBranch: "main", OwnerNode: "n2"},
		{FamilyID: "family", BranchID: "remote", PhysicalContainer: "fork-runtime", PhysicalBranch: "remote", OwnerNode: "n3"},
		{FamilyID: "family", BranchID: "z", PhysicalContainer: "family", PhysicalBranch: "z", OwnerNode: "n2"},
		{FamilyID: "family", BranchID: "zremote", PhysicalContainer: "fork-runtime", PhysicalBranch: "zremote", OwnerNode: "n3"},
	}
	orderFamilyRemovalPlacements(records, "family")
	var got []string
	for _, record := range records {
		got = append(got, record.BranchID)
	}
	if want := []string{"remote", "zremote", "main", "z", "a"}; !slices.Equal(got, want) {
		t.Fatalf("removal order %v, want %v", got, want)
	}
}

func TestFilesystemAccountingDefersZeroByteMetadataDependency(t *testing.T) {
	branch := ttcbundle.BranchInfo{DistributedForkTransactionID: "fork-1"}
	if !deferFilesystemAccounting(branch, map[string]bool{"fork-1": true}) {
		t.Fatal("zero-byte volatile filesystem dependency was treated as independent")
	}
	if deferFilesystemAccounting(branch, map[string]bool{"fork-1": false}) {
		t.Fatal("independent filesystem remained deferred")
	}
	branch.DependentFilesystemBytes = 1
	if !deferFilesystemAccounting(branch, nil) {
		t.Fatal("nonzero filesystem dependency was not deferred")
	}
}

func TestRunRenameNamesTheBranchAfterItsNewName(t *testing.T) {
	backend := &fakeBackend{}
	var out bytes.Buffer
	if err := runRename([]string{"demo@try-a", "main"}, &out, backend); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(backend.renamed) != 1 || backend.renamed[0] != "demo@try-a->main" {
		t.Fatalf("backend saw %v", backend.renamed)
	}
	if got := strings.TrimSpace(out.String()); got != "demo@main" {
		t.Fatalf("printed %q, want the branch under its new name", got)
	}
	if err := runRename([]string{"demo@try-a"}, &out, backend); err == nil {
		t.Fatal("a rename with no new name was accepted")
	}
}

func TestReconcileStandaloneMemoryGraphReclaimsCrashResidue(t *testing.T) {
	root := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(root, "catalog", "snapshot-graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	rootObject, err := graph.CreateObject(origin, "memory/crash/root-pin", ttcsnapshot.ObjectKindMetadata, []byte("root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("memory/crash/resource-0000", rootObject); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("memory/crash/root-pin"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CreateObject(origin, "memory/crash/construction-pin", ttcsnapshot.ObjectKindMetadata, []byte("partial"), nil); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{dataRoot: root, localRoot: root, snapshotGraph: graph, snapshotData: data}
	if err := backend.reconcileStandaloneMemoryGraph(); err != nil {
		t.Fatal(err)
	}
	stats := graph.Stats()
	if stats.Snapshots != 0 || stats.Objects != 0 || stats.Roots != 0 || stats.Pins != 0 || stats.Reclaiming != 0 {
		t.Fatalf("graph after reconciliation = %+v, want empty", stats)
	}
	if got := graph.JournalBytes(); got > 1024 {
		t.Fatalf("compacted journal has %d bytes, want at most 1024", got)
	}
}

func TestReconcileStandaloneMemoryGraphRetainsRuntimeBacking(t *testing.T) {
	root := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(root, "catalog", "snapshot-graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := graph.CreateObject(origin, "memory/runtime/pin", ttcsnapshot.ObjectKindMemoryNode, []byte("root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const rootName = "memory/runtime/resource-0000"
	if err := graph.SetRoot(rootName, object); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("memory/runtime/pin"); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(root, "containers", "container-id")
	if err := os.MkdirAll(filepath.Join(containerDir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "bundle", "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ttcbundle.ContainerInfo{
		ID: "container-id", Name: "example", BundleDir: filepath.Join(containerDir, "bundle"), State: "exited", CreatedAt: time.Now(), LiveForkEligible: true,
		Branches: []ttcbundle.BranchInfo{{ID: "main", FilesystemBranch: "main", State: "exited", StorageState: "local", CreatedAt: time.Now()}},
		RuntimeMemoryBackings: []ttcbundle.RuntimeMemoryBacking{{
			ID: "runtime-1", SnapshotID: uint64(origin), MemoryRoots: []string{rootName}, MemoryRootObjects: []string{object.String()},
		}},
	}
	encoded, err := json.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{dataRoot: root, localRoot: root, snapshotGraph: graph, snapshotData: data}
	if err := backend.reconcileStandaloneMemoryGraph(); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root(rootName); !ok {
		t.Fatal("runtime backing root was reclaimed")
	}
	if err := backend.containerStore().RemoveRuntimeMemoryBacking("example", "runtime-1"); err != nil {
		t.Fatal(err)
	}
	if err := backend.reconcileStandaloneMemoryGraph(); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root(rootName); ok {
		t.Fatal("retired runtime backing root was retained")
	}
}

// A cluster node releases the runtime memory backings it parked, and no
// other node's. Once it has read the records of its containers for an earlier
// daemon's, it reads only those of the containers whose backings it parks.
func TestClusterReleasesOnlyItsParkedRuntimeMemoryBackings(t *testing.T) {
	dir := t.TempDir()
	shared, local := filepath.Join(dir, "shared"), filepath.Join(dir, "local")
	graph, err := ttcsnapshot.Open(filepath.Join(local, "catalog", "snapshot-graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(local, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	root := func(name string) ttcsnapshot.ObjectID {
		t.Helper()
		object, err := graph.CreateObject(origin, name+"/pin", ttcsnapshot.ObjectKindMemoryNode, []byte(name), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.SetRoot(name, object); err != nil {
			t.Fatal(err)
		}
		if err := graph.DeletePin(name + "/pin"); err != nil {
			t.Fatal(err)
		}
		return object
	}
	write := func(id string, backings ...ttcbundle.RuntimeMemoryBacking) {
		t.Helper()
		containerDir := filepath.Join(shared, "containers", id)
		if err := os.MkdirAll(filepath.Join(containerDir, "bundle"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(local, "containers", id), 0700); err != nil {
			t.Fatal(err)
		}
		info := ttcbundle.ContainerInfo{
			ID: id, Name: id, BundleDir: filepath.Join(containerDir, "bundle"), State: "running", CreatedAt: time.Now(), LiveForkEligible: true,
			Branches:              []ttcbundle.BranchInfo{{ID: "main", NodeID: "n1", FilesystemBranch: "main", State: "running", StorageState: "local", CreatedAt: time.Now()}},
			RuntimeMemoryBackings: backings,
		}
		encoded, err := json.Marshal(&info)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	backings := func(id string) int {
		t.Helper()
		info, err := ttcbundle.Store{Root: shared}.LoadContainer(id)
		if err != nil {
			t.Fatal(err)
		}
		return len(info.RuntimeMemoryBackings)
	}
	ours := root("memory/runtime/ours")
	write("family", ttcbundle.RuntimeMemoryBacking{ID: "runtime-1", SnapshotID: uint64(origin), MemoryRoots: []string{"memory/runtime/ours"}, MemoryRootObjects: []string{ours.String()}})
	// Parked by another node, whose graph holds its roots.
	write("elsewhere", ttcbundle.RuntimeMemoryBacking{ID: "runtime-2", SnapshotID: uint64(origin), MemoryRoots: []string{"memory/runtime/theirs"}, MemoryRootObjects: []string{ours.String()}})
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\nfor a; do [ \"$a\" = list ] && { echo '[]'; exit 0; }; done\necho \"unexpected: $*\" >&2\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	source := &distributedSourceRuntime{}
	b := &runtimeBackend{root: filepath.Join(dir, "runtime"), runsc: runsc, dataRoot: shared, localRoot: local, nodeID: "n1", distributedSource: source, snapshotGraph: graph, snapshotData: data}
	if err := b.reconcileConsumedMemoryBacking(); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root("memory/runtime/ours"); ok {
		t.Error("this node's parked backing was not released")
	}
	if got := backings("family"); got != 0 {
		t.Errorf("family has %d backings, want its own released", got)
	}
	if got := backings("elsewhere"); got != 1 {
		t.Errorf("another node's parked backing was released: %d left", got)
	}

	// A pass with nothing parked reads no record.
	if err := os.WriteFile(filepath.Join(shared, "containers", "elsewhere", "container.json"), []byte("not a record"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.reconcileConsumedMemoryBacking(); err != nil {
		t.Fatalf("a pass with nothing parked read a record: %v", err)
	}

	// Nor does it but for what it parks since.
	again := root("memory/runtime/again")
	write("family", ttcbundle.RuntimeMemoryBacking{ID: "runtime-3", SnapshotID: uint64(origin), MemoryRoots: []string{"memory/runtime/again"}, MemoryRootObjects: []string{again.String()}})
	source.noteParked("family")
	if err := b.reconcileConsumedMemoryBacking(); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root("memory/runtime/again"); ok {
		t.Error("a backing parked since was not released")
	}
	if got := backings("family"); got != 0 {
		t.Errorf("family has %d backings, want them released", got)
	}
}

// The reconciliation of volatile origins runs under the lifecycle lock, so it
// must not wait for an origin's lock: a publication holds its origin's lock
// while it waits for the handoff gate, which a capture holds while it waits
// for the lifecycle lock. It passes over an origin whose lock is held.
func TestOriginCleanupPassesOverALockedOrigin(t *testing.T) {
	graph, err := ttcsnapshot.Open(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	empty, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	source := &distributedSourceRuntime{authority: &cluster.AuthorityCatalog{}, incarnation: "incarnation-1"}
	b := &runtimeBackend{nodeID: "n1", distributedSource: source, snapshotGraph: graph}
	unlock := source.lockSnapshotOrigin(empty)
	defer unlock()
	type result struct {
		abandoned int
		err       error
	}
	done := make(chan result, 2)
	go func() {
		abandoned, err := b.abandonEmptyVolatileMemoryOrigins()
		done <- result{abandoned, err}
	}()
	go func() { done <- result{0, b.abandonStaleOrigin("n1", uint64(empty))} }()
	for range 2 {
		select {
		case got := <-done:
			if got.err != nil || got.abandoned != 0 {
				t.Fatalf("cleanup of a locked origin: abandoned %d, %v", got.abandoned, got.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the origin cleanup waited for an origin's lock")
		}
	}
}

// A removal ends with the node's reconciliation of its volatile origins,
// which reads every repository of the node. One the node could not read
// failed the removal after its branch's memory and filesystems were gone,
// and left the branch recorded without them. The removal finishes, and the
// node's maintenance tries the reconciliation again.
func TestRemovalLeavesAFailedOriginReconciliationToTheNode(t *testing.T) {
	// No node catalog: renewing the node's authority, the reconciliation's
	// first step, fails.
	source := &distributedSourceRuntime{authority: &cluster.AuthorityCatalog{}, incarnation: "incarnation-1"}
	var stderr bytes.Buffer
	b := &runtimeBackend{nodeID: "n1", distributedSource: source, stderr: &stderr}
	if err := b.removingBranches(func() error {
		b.removals.originsDue = true
		return nil
	}); err != nil {
		t.Fatalf("removal failed on the node's origin reconciliation: %v", err)
	}
	if !source.reconcileOrigins.Load() {
		t.Fatal("the failed reconciliation was not left to the node's maintenance")
	}
	if !strings.Contains(stderr.String(), "reconciling snapshot providers") {
		t.Fatalf("the removal did not warn of the failed reconciliation: %q", stderr.String())
	}
}

// The origin reconciliation takes the locks of all the stale origins it
// found before it looks at the repositories again: origins that share a lock
// are all taken with it, once, and those whose lock is held are passed over.
func TestTryLockSnapshotOrigins(t *testing.T) {
	source := &distributedSourceRuntime{}
	stripes := uint64(len(source.originLocks))
	releaseBusy := source.lockSnapshotOrigin(ttcsnapshot.SnapshotID(3))
	locked, unlock := source.tryLockSnapshotOrigins([]uint64{1, 1 + stripes, 3, 2})
	if want := []uint64{1, 1 + stripes, 2}; !slices.Equal(locked, want) {
		t.Fatalf("tryLockSnapshotOrigins locked %v, want %v", locked, want)
	}
	if release, ok := source.tryLockSnapshotOrigin(ttcsnapshot.SnapshotID(1)); ok {
		release()
		t.Fatal("an origin's lock was free while the reconciliation held it")
	}
	unlock()
	releaseBusy()
	for _, origin := range []uint64{1, 2, 3} {
		release, ok := source.tryLockSnapshotOrigin(ttcsnapshot.SnapshotID(origin))
		if !ok {
			t.Fatalf("origin %d's lock is still held", origin)
		}
		release()
	}
}

func TestDistributedGraphHandoffRequiresLiveRuntime(t *testing.T) {
	for _, test := range []struct {
		name   string
		branch ttcbundle.BranchInfo
		want   bool
	}{
		{name: "running-dependent", branch: ttcbundle.BranchInfo{State: "running", DependentMemoryBytes: 4096}, want: true},
		{name: "paused-roots", branch: ttcbundle.BranchInfo{State: "paused", SnapshotID: 17, MemoryRootObjects: []string{"0000000000000011-03-0000000000000001"}}, want: true},
		{name: "running-independent", branch: ttcbundle.BranchInfo{State: "running"}},
		{name: "dehydrating", branch: ttcbundle.BranchInfo{State: "dehydrating"}},
		{name: "dehydrated", branch: ttcbundle.BranchInfo{State: "dehydrated", Unplaced: true}},
		{name: "exited", branch: ttcbundle.BranchInfo{State: "exited"}},
		{name: "unplaced-running", branch: ttcbundle.BranchInfo{State: "running", Unplaced: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := distributedGraphHandoffHasLiveRuntime(test.branch); got != test.want {
				t.Fatalf("distributedGraphHandoffHasLiveRuntime(%+v)=%t, want %t", test.branch, got, test.want)
			}
		})
	}
}

func TestReconcileInterruptedMemoryGraphSealsOnlyReferencedResidue(t *testing.T) {
	root := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()

	interrupted, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	child, err := graph.CreateObject(interrupted, "capture/construction-child", ttcsnapshot.ObjectKindMetadata, []byte("child"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("capture/partial-root", child); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("capture/construction-child"); err != nil {
		t.Fatal(err)
	}

	completed, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := graph.CreateObject(completed, "capture/completed-parent", ttcsnapshot.ObjectKindMetadata, []byte("parent"), []ttcsnapshot.Edge{{Slot: 0, Child: child}})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/completed-root", parent); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("capture/completed-parent"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(completed, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}

	empty, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	dead, err := graph.CreateObject(empty, "capture/dead", ttcsnapshot.ObjectKindMetadata, []byte("dead"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("capture/dead-root", dead); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("capture/dead"); err != nil {
		t.Fatal(err)
	}

	if err := reconcileInterruptedMemoryGraph(graph, data, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Root("capture/partial-root"); ok {
		t.Fatal("interrupted construction root survived reconciliation")
	}
	if _, ok := graph.Root("capture/dead-root"); ok {
		t.Fatal("unreferenced construction root survived reconciliation")
	}
	if got, ok := graph.Root("branch/completed-root"); !ok || got != parent {
		t.Fatalf("completed root = %v, %t; want %v", got, ok, parent)
	}
	if record, ok := graph.Snapshot(interrupted); !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
		t.Fatalf("referenced interrupted snapshot = %+v, %t; want sealed volatile", record, ok)
	}
	if objects, err := graph.SnapshotObjects(empty); err != nil || len(objects) != 0 {
		t.Fatalf("empty interrupted snapshot objects = %v, %v; want none", objects, err)
	}
}

func TestReconcileInterruptedMemoryGraphReleasesCaptureHolds(t *testing.T) {
	root := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()

	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	object, err := graph.CreateObject(origin, "capture/object", ttcsnapshot.ObjectKindMetadata, []byte("object"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("branch/root", object); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("capture/object"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Seal(origin, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	// A capture of another origin that shared the object, and did not finish,
	// left its hold. No origin is working.
	held := "remote/9/fork-1/held/" + object.String()
	if err := graph.SetPin(held, object); err != nil {
		t.Fatal(err)
	}

	if err := reconcileInterruptedMemoryGraph(graph, data, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		t.Fatal(err)
	}
	if graph.HasPin(held) {
		t.Fatal("an interrupted capture's hold on a sealed origin survived reconciliation")
	}
	if got, ok := graph.Root("branch/root"); !ok || got != object {
		t.Fatalf("root = %v, %t; want %v", got, ok, object)
	}
}

// A kernel that accounts swap to cgroups gives a memory cgroup memory.swap.max
// beside memory.max; one that does not gives only memory.max. With neither, as
// on cgroup v1, runsc decides.
func TestCgroupSwapAccounted(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "delegated")
	if err := os.MkdirAll(filepath.Join(parent, "daemon"), 0700); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(proc, []byte("0::/delegated/daemon\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !cgroupSwapAccounted(proc, root) {
		t.Error("a cgroup with no memory interface was judged unaccounted")
	}
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if cgroupSwapAccounted(proc, root) {
		t.Error("a memory cgroup without memory.swap.max was judged accounted")
	}
	if err := os.WriteFile(filepath.Join(parent, "memory.swap.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !cgroupSwapAccounted(proc, root) {
		t.Error("a memory cgroup with memory.swap.max was judged unaccounted")
	}
}

// A branch's record keeps its exit code, and its console the raw wait status:
// the two agree however the process ended.
func TestRetainedConsoleExitCodeMatchesItsWaitStatus(t *testing.T) {
	dir := t.TempDir()
	paths := ttcbundle.ConsolePaths{WaitStatus: filepath.Join(dir, "main.wait-status")}
	if err := ttcconsole.WriteWaitStatus(paths.WaitStatus, 7<<8); err != nil {
		t.Fatal(err)
	}
	if err := installRetainedConsoleExitCode(paths, 7); err != nil {
		t.Fatalf("exit code 7 against a status of exit 7: %v", err)
	}
	if err := installRetainedConsoleExitCode(paths, 3); err == nil {
		t.Fatal("exit code 3 was taken for a status of exit 7")
	}
	for code, want := range map[int]uint32{3: 3 << 8, 137: 9, 0: 0} {
		if err := os.Remove(paths.WaitStatus); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := installRetainedConsoleExitCode(paths, code); err != nil {
			t.Fatalf("installing exit code %d: %v", code, err)
		}
		if status, found, err := readConsoleWaitStatus(paths.WaitStatus); err != nil || !found || status != want || waitStatusExitCode(status) != code {
			t.Fatalf("exit code %d installed status %d (found %t, %v), want %d", code, status, found, err, want)
		}
	}
}

// statesBackend is a fakeBackend whose inspection of states alone is
// counted apart from its whole inspection.
type statesBackend struct {
	*fakeBackend
	states int
}

func (b *statesBackend) inspectStates(name string) (InspectInfo, error) {
	b.states++
	return b.fakeBackend.inspect(name)
}

// A command resolves its targets, and a stop its policy, by the states of
// its family's branches alone, where the backend can inspect them alone.
func TestBranchOperationTargetsInspectStatesAlone(t *testing.T) {
	inspected := InspectInfo{}
	inspected.Config.StopSignal = "SIGINT"
	inspected.TTC.Branches = []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "fork", State: "paused"}},
	}
	backend := &statesBackend{fakeBackend: &fakeBackend{inspectInfo: inspected}}
	targets, info, err := inspectBranchOperationTargets("web", backend, true)
	if err != nil || len(targets) != 2 || info.Config.StopSignal != "SIGINT" {
		t.Fatalf("inspectBranchOperationTargets = (%+v, %+v, %v)", targets, info.Config, err)
	}
	if backend.states != 1 {
		t.Errorf("the states were inspected %d times, want once", backend.states)
	}
	if targets, err := branchOperationTargets("web@fork", backend, false); err != nil || len(targets) != 1 || targets[0].state != "paused" {
		t.Errorf("branchOperationTargets(web@fork) = (%+v, %v)", targets, err)
	}
	if backend.states != 2 {
		t.Errorf("the states were inspected %d times, want twice", backend.states)
	}
}

func TestRuntimeListsBranch(t *testing.T) {
	families := []Family{
		{ID: "other", Branches: []Branch{{ID: "x"}}},
		{ID: "family", Branches: []Branch{{ID: "main"}, {ID: "x"}}},
	}
	if !runtimeListsBranch(families, "family", "x") {
		t.Error("a runtime listing the branch does not run it")
	}
	if runtimeListsBranch(families, "family", "y") || runtimeListsBranch(nil, "family", "x") {
		t.Error("a runtime not listing the branch, or gone, runs it")
	}
	unknown := []Family{{ID: "family", Error: "connection refused", SandboxPID: os.Getpid(), Branches: []Branch{{ID: "main"}}}}
	if !runtimeListsBranch(unknown, "family", "x") {
		t.Error("a live runtime that could not list its branches does not run the branch")
	}
	gone := []Family{{ID: "family", Error: "connection refused", SandboxPID: 0x7fffffff, Branches: []Branch{{ID: "x"}}}}
	if runtimeListsBranch(gone, "family", "x") {
		t.Error("a runtime whose Sentry is gone runs the branch")
	}
}

func TestTriggerBindMountSourcesLooksUpBindSources(t *testing.T) {
	bundle := t.TempDir()
	spec := specs.Spec{Mounts: []specs.Mount{
		{Destination: "/a", Type: "bind", Source: filepath.Join(bundle, "missing-a")},
		{Destination: "/b", Source: filepath.Join(bundle, "missing-b"), Options: []string{"rbind", "ro"}},
		{Destination: "/proc", Type: "proc", Source: "proc"},
	}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// Missing sources and other mounts are passed over: runsc reports them.
	triggerBindMountSources(bundle)
	triggerBindMountSources(filepath.Join(bundle, "no-such-bundle"))
	if got := bundleOf([]string{"create", "--bundle", bundle, "id"}); got != bundle {
		t.Fatalf("bundleOf() = %q, want %q", got, bundle)
	}
	if got := bundleOf([]string{"start", "id"}); got != "" {
		t.Fatalf("bundleOf() without a bundle = %q", got)
	}
}
