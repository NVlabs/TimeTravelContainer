// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

func TestRehomeInternalMountsSurviveSourceRemoval(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	hostname := filepath.Join(source, "hostname")
	if err := os.WriteFile(hostname, []byte("source-name\n"), 0444); err != nil {
		t.Fatal(err)
	}
	internal, err := json.Marshal([]string{"/etc/hostname"})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{
		Annotations: map[string]string{InternalMountsAnnotation: string(internal)},
		Mounts:      []specs.Mount{{Source: hostname, Destination: "/etc/hostname"}},
	}
	if err := rehomeInternalMounts(spec, target); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(spec.Mounts[0].Source)
	if err != nil {
		t.Fatalf("reading target-owned mount after source removal: %v", err)
	}
	if string(got) != "source-name\n" || !strings.HasPrefix(spec.Mounts[0].Source, target+string(filepath.Separator)) {
		t.Fatalf("re-homed mount path=%q contents=%q", spec.Mounts[0].Source, got)
	}
}

func TestMaterializePortableInternalMountsDoesNotReadSourcePath(t *testing.T) {
	bundleDir := t.TempDir()
	internal, err := json.Marshal([]string{"/etc/hostname"})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{
		Annotations: map[string]string{InternalMountsAnnotation: string(internal)},
		Mounts: []specs.Mount{{
			Source: "/source-node-private-tmp/hostname", Destination: "/etc/hostname",
		}},
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		t.Fatal(err)
	}
	if err := MaterializePortableInternalMounts(bundleDir, []PortableInternalMount{{
		Destination: "/etc/hostname", Contents: []byte("portable\n"), Mode: 0444,
	}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got specs.Spec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 1 || !strings.HasPrefix(got.Mounts[0].Source, bundleDir+string(filepath.Separator)) {
		t.Fatalf("rewritten mount=%+v", got.Mounts)
	}
	contents, err := os.ReadFile(got.Mounts[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "portable\n" {
		t.Fatalf("portable contents=%q", contents)
	}
	// Repeated imports into one physical distributed family reuse these
	// read-only files. A changed value must replace rather than truncate the
	// immutable inode.
	if err := MaterializePortableInternalMounts(bundleDir, []PortableInternalMount{{
		Destination: "/etc/hostname", Contents: []byte("second\n"), Mode: 0444,
	}}); err != nil {
		t.Fatalf("replacing read-only portable mount: %v", err)
	}
	contents, err = os.ReadFile(filepath.Join(bundleDir, "internal-mounts", "0-hostname"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "second\n" {
		t.Fatalf("replacement contents=%q", contents)
	}
}

func TestInstallPortableInternalMountsPersistsNodeLocalSources(t *testing.T) {
	root := t.TempDir()
	active := t.TempDir()
	containerDir := filepath.Join(root, "containers", "target")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	internal, err := json.Marshal([]string{"/etc/hostname"})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{
		Annotations: map[string]string{InternalMountsAnnotation: string(internal)},
		Mounts:      []specs.Mount{{Destination: "/etc/hostname", Source: "/source-node/private/hostname", Type: "bind"}},
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "target", Name: "target", BundleDir: bundleDir, State: "running", LiveForkEligible: true,
		Branches: []BranchInfo{{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: "running", StorageState: "local"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root, ActiveRoot: active}
	if err := store.InstallPortableInternalMounts("target", []PortableInternalMount{{
		Destination: "/etc/hostname", Contents: []byte("target-host\n"), Mode: 0644,
	}}); err != nil {
		t.Fatal(err)
	}
	var canonical specs.Spec
	canonicalData, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(canonicalData, &canonical); err != nil {
		t.Fatal(err)
	}
	if got := canonical.Mounts[0].Source; got != "/source-node/private/hostname" {
		t.Fatalf("canonical shared mount source = %q, want original source", got)
	}
	got, err := store.LoadSpec("target")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 1 || !strings.HasPrefix(got.Mounts[0].Source, filepath.Join(active, "containers", "target", "internal-mounts")) {
		t.Fatalf("persistent mount source = %q, want node-local active root", got.Mounts[0].Source)
	}
	contents, err := os.ReadFile(got.Mounts[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "target-host\n" {
		t.Fatalf("persistent mount contents = %q", contents)
	}
}

// A container's image root is ready on a node once it is unpacked there, and
// preparing it leaves the container's records as they were.
func TestContainerImageRootKeptOnceUnpacked(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	info := ContainerInfo{
		ID: "web-id", Name: "web", Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64",
		BundleDir: filepath.Join(root, "containers", "web-id", "bundle"),
	}
	imageRoot := filepath.Join(store.imageRootDir(), imageRootKey(info.Digest, info.Platform))
	info.RootFS = filepath.Join(imageRoot, "rootfs")
	if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "containers", info.ID, "container.json")
	if err := writeCurrentContainerFixture(record, &info); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.KeepContainerImageRoot(info.ID); err != nil || ready {
		t.Fatalf("KeepContainerImageRoot before the root is unpacked=(%t, %v)", ready, err)
	}
	if err := os.MkdirAll(info.RootFS, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageRoot, "image.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(info.RootFS), manifest); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.KeepContainerImageRoot(info.ID); err != nil || !ready {
		t.Fatalf("KeepContainerImageRoot once unpacked=(%t, %v)", ready, err)
	}
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareContainerImageRoot(context.Background(), info.ID); err != nil {
		t.Fatalf("PrepareContainerImageRoot with the root unpacked: %v", err)
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatalf("PrepareContainerImageRoot changed the container's record: %v", err)
	}
}

func TestRefreshContainerImageRootAcceptsExactHiddenTargetID(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	info := ContainerInfo{
		ID: "tx-hidden", Name: "hidden-target", Digest: "sha256:" + strings.Repeat("a", 64),
		Platform: "linux/amd64", DistributedForkHidden: true,
		DistributedForkTransactionID: "tx-hidden", DistributedFamilyID: "logical-family",
		BundleDir: filepath.Join(root, "containers", "tx-hidden", "bundle"),
	}
	info.RootFS = filepath.Join(store.imageRootDir(), imageRootKey(info.Digest, info.Platform), "rootfs")
	if err := os.MkdirAll(info.RootFS, 0700); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(root, "containers", info.ID)
	if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContainer(info.ID); err == nil {
		t.Fatal("ordinary lookup unexpectedly exposed hidden target")
	}
	if listed, err := store.ListContainersForPlacement(); err != nil || len(listed) != 0 {
		t.Fatalf("prepared target placement inventory=(%+v, %v), want empty", listed, err)
	}
	if runtime, err := store.LoadRuntimeContainer(info.ID); err != nil || runtime.ID != info.ID {
		t.Fatalf("exact runtime lookup=(%q, %v), want hidden target", runtime.ID, err)
	}
	if _, err := store.LoadSpec(info.ID); err != nil {
		t.Fatalf("loading hidden target runtime spec: %v", err)
	}
	if paths, err := store.BranchConsolePaths(info.ID, "main"); err != nil || paths.Log == "" {
		t.Fatalf("preparing hidden target branch console paths: %+v, %v", paths, err)
	}
	if paths, err := store.ExecConsolePaths(info.ID, "main", "build"); err != nil || paths.Log == "" {
		t.Fatalf("preparing hidden target exec console paths: %+v, %v", paths, err)
	}
	targetBundle, cleanup, err := store.PrepareDistributedTargetBundle(info.ID, "remote-filesystem")
	if err != nil {
		t.Fatalf("preparing hidden target runtime bundle: %v", err)
	}
	defer cleanup()
	var targetSpec specs.Spec
	encoded, err := os.ReadFile(filepath.Join(targetBundle, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &targetSpec); err != nil {
		t.Fatal(err)
	}
	if !specutils.TTCDistributedTarget(&targetSpec) {
		t.Fatal("hidden target runtime bundle is not marked as a distributed target")
	}
	if err := store.mutateContainer(info.ID, func(current *ContainerInfo) (bool, error) {
		current.State = "paused"
		return true, nil
	}); err != nil {
		t.Fatalf("mutating hidden target by exact ID: %v", err)
	}
	refreshed, err := store.RefreshContainerImageRoot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != info.ID || !refreshed.DistributedForkHidden {
		t.Fatalf("refreshed target=%+v", refreshed)
	}
	if refreshed.State != "paused" {
		t.Fatalf("refreshed target state=%q, want paused", refreshed.State)
	}
	published, err := store.PublishDistributedForkTarget("logical-family", "tx-hidden")
	if err != nil {
		t.Fatal(err)
	}
	if published.DistributedForkHidden {
		t.Fatal("target was not temporarily exposed for activation sidecars")
	}
	if exact, err := store.LoadContainer(info.ID); err != nil || exact.ID != info.ID {
		t.Fatalf("published physical runtime exact lookup=(%q, %v)", exact.ID, err)
	}
	listed, err := store.ListContainers()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("ordinary inventory exposed physical runtime: %+v", listed)
	}
	placementInventory, err := store.ListContainersForPlacement()
	if err != nil {
		t.Fatal(err)
	}
	if len(placementInventory) != 1 || placementInventory[0].ID != info.ID {
		t.Fatalf("placement inventory = %+v, want published physical runtime %q", placementInventory, info.ID)
	}
}

func TestMigrationImportsBranchIntoSiblingPhysicalFamily(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	created := time.Now().UTC().Add(-time.Hour)
	source := ContainerInfo{ID: "source-runtime", Name: "family", DistributedFamilyID: "family-id", NodeID: "node-a",
		Branches: []BranchInfo{{ID: "old-incarnation", ParentID: "ancestor", NodeID: "node-a", State: "running", CreatedAt: created, Quarantined: true}}}
	target := ContainerInfo{ID: "target-runtime", Name: "target-runtime", DistributedFamilyID: "family-id", NodeID: "node-b",
		BundleDir: filepath.Join(root, "containers", "target-runtime", "bundle"),
		Branches:  []BranchInfo{{ID: "sibling", NodeID: "node-b", State: "running", CreatedAt: created}}}
	if err := os.MkdirAll(target.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", target.ID, "container.json"), &target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDistributedFork(context.Background(), DistributedForkOptions{
		SourceContainer: "family-id", SourcePhysicalBranch: "old-incarnation", TargetName: target.ID, TargetBranch: "tx-move", VisibleBranch: "moving",
		ParentBranch: "moving", TransactionID: "tx-move", NodeID: "node-b", SourceNode: "node-a", Migration: true, Source: &source,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.LoadContainer(target.ID)
	if err != nil || len(pending.Branches) != 1 {
		t.Fatalf("reservation exposed migrating branch: %+v, %v", pending.Branches, err)
	}
	if _, branch, _, err := store.DistributedForkTargetBranch("family-id", "tx-move"); err != nil || !branch.Quarantined {
		t.Fatalf("pending migration lost quarantine: %+v, %v", branch, err)
	}
	if err := store.UpdateDistributedForkTargetState("family-id", "tx-move", "paused"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		published, err := store.PublishDistributedForkTarget("family-id", "tx-move")
		if err != nil {
			t.Fatal(err)
		}
		if len(published.Branches) != 2 {
			t.Fatalf("publication lost sibling or duplicated migration: %+v", published.Branches)
		}
		branch := published.Branches[1]
		if branch.ID != "tx-move" || branch.ParentID != "ancestor" || branch.NodeID != "node-b" ||
			!branch.CreatedAt.Equal(created) || !branch.Quarantined || !branch.DistributedMigration || branch.State != "paused" {
			t.Fatalf("migration lost branch metadata: %+v", branch)
		}
	}
}

func TestAuthoritativeMigrationReplacesStalePhysicalOwner(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	created := time.Now().UTC().Add(-time.Hour)
	source := ContainerInfo{ID: "source-runtime", Name: "family", DistributedFamilyID: "family-id", NodeID: "node-b",
		Branches: []BranchInfo{{ID: "moving", ParentID: "ancestor", NodeID: "node-b", State: "running", CreatedAt: created}}}
	target := ContainerInfo{ID: "target-runtime", Name: "target-runtime", DistributedFamilyID: "family-id", NodeID: "node-a",
		BundleDir: filepath.Join(root, "containers", "target-runtime", "bundle"),
		Branches: []BranchInfo{
			{ID: "sibling", NodeID: "node-a", State: "running", CreatedAt: created},
			// Model a branch that migrated away earlier. The physical family kept
			// derived metadata for its former local branch, while the authority now
			// authorizes importing that branch back from node-b.
			{ID: "moving", ParentID: "ancestor", NodeID: "node-a", State: "running", CreatedAt: created},
		}}
	if err := os.MkdirAll(target.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", target.ID, "container.json"), &target); err != nil {
		t.Fatal(err)
	}
	base := DistributedForkOptions{
		SourceContainer: "family-id", TargetName: target.ID, TargetBranch: "tx-move", VisibleBranch: "moving",
		ParentBranch: "moving", TransactionID: "tx-move", NodeID: "node-a", SourceNode: "node-b", Migration: true, Source: &source,
	}
	if _, err := store.PrepareDistributedFork(context.Background(), base); err == nil {
		t.Fatal("non-authoritative migration ignored stale physical owner")
	}
	base.AuthoritativeMigration = true
	if _, err := store.PrepareDistributedFork(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDistributedForkTargetState("family-id", "tx-move", "paused"); err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishDistributedForkTarget("family-id", "tx-move")
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Branches) != 3 {
		t.Fatalf("publication lost stale provider or new incarnation: %+v", published.Branches)
	}
	for _, branch := range published.Branches {
		if branch.ID == "tx-move" {
			if branch.NodeID != "node-a" || branch.FilesystemBranch != ttcvolume.ScopedBranchID(source.ID, "tx-move") || !branch.DistributedMigration {
				t.Fatalf("authoritative migration did not replace stale metadata: %+v", branch)
			}
			return
		}
	}
	t.Fatal("published family lost moving branch")
}

func TestOriginalDistributedTargetRemainsAddressableAfterSiblingImport(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	info := ContainerInfo{
		ID: "physical", Name: "physical", State: "running",
		BundleDir:                    filepath.Join(root, "containers", "physical", "bundle"),
		DistributedForkTransactionID: "fork-original",
		Branches: []BranchInfo{
			{ID: "original", State: "running", DistributedForkTransactionID: "fork-original"},
			{ID: "sibling", State: "running", DistributedForkTransactionID: "fork-sibling", DistributedMigration: true},
		},
	}
	if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", info.ID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	resolved, branch, peer, err := store.DistributedForkTargetBranch("physical", "fork-original")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != info.ID || branch.ID != "original" || !peer {
		t.Fatalf("resolved=%q branch=%+v peer=%t", resolved.ID, branch, peer)
	}
	if err := store.UpdateDistributedForkDependencies("physical", "fork-original", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDistributedForkTargetState("physical", "fork-original", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDistributedForkMemoryGraph("physical", "fork-original", 9, []string{"0000000000000009-03-0000000000000001"}, "fork-original"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.LoadContainer(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Branches[0].State != "paused" || updated.Branches[0].SnapshotID != 9 || updated.Branches[1].State != "running" || updated.Branches[1].SnapshotID != 0 {
		t.Fatalf("branch states changed incorrectly: %+v", updated.Branches)
	}
}

func TestDistributedForkTargetReadsOnlyItsFamily(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	target := ContainerInfo{
		ID: "tx-target", Name: "tx-target", State: "running", DistributedFamilyID: "family",
		BundleDir:                    filepath.Join(root, "containers", "tx-target", "bundle"),
		DistributedForkTransactionID: "tx-target", DistributedForkHidden: true,
		Branches: []BranchInfo{{ID: "tx-target", State: "running", DistributedForkTransactionID: "tx-target", DependentMemoryBytes: 7}},
	}
	if err := os.MkdirAll(target.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", target.ID, "container.json"), &target); err != nil {
		t.Fatal(err)
	}
	// Another family's record, which the lookup must not read.
	if err := os.MkdirAll(filepath.Join(root, "containers", "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "containers", "other", "container.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	found, err := store.LoadDistributedForkTarget("family", "tx-target")
	if err != nil || found.ID != target.ID {
		t.Fatalf("target = %q, %v", found.ID, err)
	}
	// Unchanged counters leave the record as it is.
	path := filepath.Join(root, "containers", target.ID, "container.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDistributedForkDependencies("family", "tx-target", 7, 0); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged dependency counters rewrote the record")
	}
	if err := store.UpdateDistributedForkDependencies("family", "tx-target", 0, 0); err != nil {
		t.Fatal(err)
	}
	if updated, err := store.LoadDistributedForkTarget("family", "tx-target"); err != nil || updated.Branches[0].DependentMemoryBytes != 0 {
		t.Fatalf("updated target = %+v, %v", updated.Branches, err)
	}
}

func TestDistributedForkImportUsesExistingFamily(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	now := time.Now().UTC()
	for _, info := range []ContainerInfo{
		{ID: "source-id", Name: "source", BundleDir: filepath.Join(root, "containers", "source-id", "bundle"), DistributedFamilyID: "global-family", State: "running", CreatedAt: now, Branches: []BranchInfo{{ID: "main", State: "running", CreatedAt: now}}},
		{ID: "target-id", Name: "target", DistributedFamilyID: "global-family", DistributedForkNodeID: "node-b", NodeID: "node-a", State: "running", CreatedAt: now, BundleDir: filepath.Join(root, "containers", "target-id", "bundle"), Branches: []BranchInfo{{ID: "main", FilesystemBranch: "volume-main", State: "running", CreatedAt: now}}},
	} {
		dir := filepath.Join(root, "containers", info.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentContainerFixture(filepath.Join(dir, "container.json"), &info); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := store.PrepareDistributedFork(context.Background(), DistributedForkOptions{
		SourceContainer: "source", TargetName: "target", TargetBranch: "tx-physical",
		VisibleBranch: "second", ParentBranch: "main", TransactionID: "tx-import", NodeID: "node-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Container.ID != "target-id" {
		t.Fatalf("prepared container=%q, want existing target-id", prepared.Container.ID)
	}
	_, branch, peer, err := store.DistributedForkTargetBranch("global-family", "tx-import")
	if err != nil {
		t.Fatal(err)
	}
	if !peer || branch.ID != "second" || branch.ParentID != "main" || branch.FilesystemBranch != ttcvolume.ScopedBranchID("source-id", "tx-physical") {
		t.Fatalf("pending peer=%t branch=%+v", peer, branch)
	}
	bundleDir, cleanup, err := store.PrepareDistributedBranchRestoreBundle("target", branch.ID, branch.FilesystemBranch)
	if err != nil {
		t.Fatalf("preparing first physical peer bundle: %v", err)
	}
	defer cleanup()
	var bootstrap specs.Spec
	encoded, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &bootstrap); err != nil {
		t.Fatal(err)
	}
	if got := bootstrap.Annotations[specutils.AnnotationTTCInitialBranchID]; got != "second" {
		t.Fatalf("peer bootstrap branch annotation=%q, want second", got)
	}
	if got := bootstrap.Annotations[specutils.AnnotationTTCInitialFilesystemBranch]; got != branch.FilesystemBranch {
		t.Fatalf("peer bootstrap filesystem annotation=%q, want %q", got, branch.FilesystemBranch)
	}
	graphObjects := []string{"0000000000000007-03-0000000000000001", "0000000000000006-03-0000000000000002"}
	if err := store.UpdateDistributedForkMemoryGraph("global-family", "tx-import", 7, graphObjects, "tx-import"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDistributedForkTargetState("global-family", "tx-import", "paused"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishDistributedForkTarget("global-family", "tx-import"); err != nil {
		t.Fatal(err)
	}
	info, branch, peer, err := store.DistributedForkTargetBranch("global-family", "tx-import")
	if err != nil {
		t.Fatal(err)
	}
	if !peer || branch.State != "paused" || branch.NodeID != "node-b" || len(info.Branches) != 2 ||
		branch.SnapshotID != 7 || !slices.Equal(branch.MemoryRootObjects, graphObjects) || len(branch.MemoryRoots) != 2 ||
		branch.DistributedGraphReferenceOperation != "tx-import" ||
		branch.MemoryRoots[0] != fmt.Sprintf("memory/branch/%s/resource-0000", branch.FilesystemBranch) {
		t.Fatalf("published peer=%t branch=%+v family=%+v", peer, branch, info.Branches)
	}
	if err := store.RemoveDistributedForkTarget("global-family", "tx-import"); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.LoadContainer("target")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Branches) != 1 || remaining.Branches[0].ID != "main" {
		t.Fatalf("remaining branches=%+v, want main only", remaining.Branches)
	}
}

func TestDistributedMigrationReplacesBranchAndPreservesIdentity(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	created := time.Unix(1234, 0).UTC()
	info := ContainerInfo{
		ID: "family-id", Name: "family", DistributedFamilyID: "global-family",
		NodeID: "node-a", State: "running", CreatedAt: created,
		BundleDir: filepath.Join(root, "containers", "family-id", "bundle"),
		Branches: []BranchInfo{
			{ID: "main", NodeID: "node-a", FilesystemBranch: "fs-main", State: "running", CreatedAt: created},
			{ID: "child", ParentID: "main", NodeID: "node-a", FilesystemBranch: "fs-child-old", State: "running", CreatedAt: created.Add(time.Second)},
		},
	}
	dir := filepath.Join(root, "containers", info.ID)
	if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(dir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}

	prepare := func(transaction string) BranchInfo {
		t.Helper()
		if _, err := store.PrepareDistributedFork(context.Background(), DistributedForkOptions{
			SourceContainer: "family", TargetName: "family", TargetBranch: transaction,
			VisibleBranch: "child", TransactionID: transaction, NodeID: "node-b",
			SourceNode: "node-a", Migration: true,
		}); err != nil {
			t.Fatal(err)
		}
		_, branch, peer, err := store.DistributedForkTargetBranch("global-family", transaction)
		if err != nil {
			t.Fatal(err)
		}
		if !peer || !branch.DistributedMigration || branch.ParentID != "main" || !branch.CreatedAt.Equal(created.Add(time.Second)) {
			t.Fatalf("pending migration lost branch identity: peer=%t branch=%+v", peer, branch)
		}
		return branch
	}

	pending := prepare("tx-cancel")
	bundleDir, cleanup, err := store.PrepareDistributedBranchRestoreBundle("family", pending.ID, pending.FilesystemBranch)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var spec specs.Spec
	encoded, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	if got := spec.Annotations[specutils.AnnotationTTCInitialBranchID]; got != "tx-cancel" {
		t.Fatalf("cold migration branch annotation=%q, want tx-cancel", got)
	}
	if got := spec.Annotations[specutils.AnnotationTTCInitialBranchParentID]; got != "main" {
		t.Fatalf("cold migration parent annotation=%q, want main", got)
	}
	if err := store.RemoveDistributedForkTarget("global-family", "tx-cancel"); err != nil {
		t.Fatal(err)
	}
	afterCancel, err := store.LoadContainer("family")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCancel.Branches) != 2 || afterCancel.BranchNodeID("child") != "node-a" {
		t.Fatalf("cancel changed source branch: %+v", afterCancel.Branches)
	}

	prepare("tx-publish")
	if err := store.UpdateDistributedForkTargetState("global-family", "tx-publish", "paused"); err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishDistributedForkTarget("global-family", "tx-publish")
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Branches) != 3 {
		t.Fatalf("migration did not retain its sparse source incarnation: %+v", published.Branches)
	}
	for _, branch := range published.Branches {
		if branch.ID == "tx-publish" {
			if branch.ParentID != "main" || !branch.CreatedAt.Equal(created.Add(time.Second)) || branch.NodeID != "node-b" || branch.FilesystemBranch == "fs-child-old" || !branch.DistributedMigration {
				t.Fatalf("published migration branch=%+v", branch)
			}
			return
		}
	}
	t.Fatal("published migration lost physical target branch")
}

func TestConcurrentDistributedMigrationReservationsPreserveImports(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	bundleDir := filepath.Join(root, "containers", "family-id", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	const imports = 32
	branches := make([]BranchInfo, 0, imports)
	for i := 0; i < imports; i++ {
		branches = append(branches, BranchInfo{
			ID: fmt.Sprintf("branch-%02d", i), NodeID: "node-a",
			FilesystemBranch: fmt.Sprintf("fs-%02d", i), State: "running",
		})
	}
	info := ContainerInfo{
		ID: "family-id", Name: "family", DistributedFamilyID: "family-id",
		NodeID: "node-a", BundleDir: bundleDir, State: "running", Branches: branches,
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", info.ID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByImport := make([]error, imports)
	var wg sync.WaitGroup
	for i := 0; i < imports; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, errorsByImport[index] = store.PrepareDistributedFork(context.Background(), DistributedForkOptions{
				SourceContainer: "family", TargetName: "family",
				TargetBranch:  fmt.Sprintf("transaction-branch-%02d", index),
				VisibleBranch: fmt.Sprintf("branch-%02d", index),
				TransactionID: fmt.Sprintf("transaction-%02d", index),
				NodeID:        "node-b", SourceNode: "node-a", Migration: true,
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errorsByImport {
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	got, err := store.LoadContainer(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DistributedForkImports) != imports {
		t.Fatalf("retained %d/%d concurrent imports: %+v", len(got.DistributedForkImports), imports, got.DistributedForkImports)
	}
}

func TestDistributedForkAddsRemoteBranchToExistingFamily(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	bundleDir := filepath.Join(root, "containers", "family-id", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{
		ID: "family-id", Name: "family", DistributedFamilyID: "family-id", NodeID: "node-a", BundleDir: bundleDir,
		State: "running", Branches: []BranchInfo{{ID: "main", NodeID: "node-a", FilesystemBranch: "main", State: "running"}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(root, "containers", info.ID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDistributedFork(context.Background(), DistributedForkOptions{
		SourceContainer: "family", TargetName: "family", TargetBranch: "physical-child", VisibleBranch: "child",
		TransactionID: "transaction-child", NodeID: "node-b", SourceNode: "node-a",
	}); err != nil {
		t.Fatalf("remote branch reservation: %v", err)
	}
	updated, err := store.LoadContainer("family")
	if err != nil {
		t.Fatal(err)
	}
	pending, ok := updated.DistributedForkImports["transaction-child"]
	if !ok || pending.BranchID != "child" || pending.TargetNode != "node-b" || pending.Migration {
		t.Fatalf("remote family import=%+v present=%t", pending, ok)
	}
}

// TestBuildSpecReadsAccountsInsideTheImage links an image's account files
// out of it: the links resolve inside the image, as in the container, and no
// account of the host's files is found.
func TestBuildSpecReadsAccountsInsideTheImage(t *testing.T) {
	host := filepath.Join(t.TempDir(), "accounts")
	root := t.TempDir()
	for dir, files := range map[string][2]string{
		host:                      {"hostonly:x:4242:4343::/:/bin/sh\n", "hostgroup:x:4545:hostonly,app\n"},
		filepath.Join(root, host): {"app:x:123:456::/:/bin/sh\n", "app:x:456:\nextra:x:789:app\n"},
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"passwd", "group"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(files[i]), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"passwd", "group"} {
		if err := os.Symlink(filepath.Join(host, name), filepath.Join(root, "etc", name)); err != nil {
			t.Fatal(err)
		}
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(root), manifest); err != nil {
		t.Fatal(err)
	}
	build := func(user string, groupAdd ...string) (*specs.Spec, error) {
		return BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{User: user, Cmd: []string{"/bin/true"}}},
			SpecOptions{Name: "accounts", RootFS: root, GroupAdd: groupAdd})
	}
	if spec, err := build("app"); err != nil {
		t.Fatal(err)
	} else if got := spec.Process.User; got.UID != 123 || got.GID != 456 || !slices.Equal(got.AdditionalGids, []uint32{789}) {
		t.Fatalf("the image's account: %+v", got)
	}
	if spec, err := build("hostonly"); err == nil {
		t.Errorf("a host account resolved: %+v", spec.Process.User)
	}
	if spec, err := build("app", "hostgroup"); err == nil {
		t.Errorf("a host group resolved: %+v", spec.Process.User)
	}
}

// TestBuildSpecSkipsAccountFIFOs makes the image's /etc/passwd a FIFO, which
// an open would wait on for a writer.
func TestBuildSpecSkipsAccountFIFOs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "etc", "passwd"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(root), manifest); err != nil {
		t.Fatal(err)
	}
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"/bin/true"}}}, SpecOptions{Name: "fifo", RootFS: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Process.User; got.UID != 0 || got.GID != 0 {
		t.Fatalf("user %+v, want root", got)
	}
}

// TestSeedManagedVolumeStaysInsideTheImage declares a volume below an image
// symbolic link out of the image: the volume is seeded from where the link
// leads inside the image, as the container mounts it, and not from the host.
func TestSeedManagedVolumeStaysInsideTheImage(t *testing.T) {
	host := filepath.Join(t.TempDir(), "outside")
	rootfs := t.TempDir()
	for dir, data := range map[string]string{
		filepath.Join(host, "data"):         "host secret",
		filepath.Join(rootfs, host, "data"): "image data",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(host, filepath.Join(rootfs, "pivot")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("seed-inside")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := seedManagedVolume(repository, rootfs, "/pivot/data", manifest); err != nil {
		t.Fatal(err)
	}
	ino, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "file")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 64)
	n, err := repository.ReadAt(ttcvolume.MainBranch, ino, got, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(got[:n]) != "image data" {
		t.Fatalf("the volume holds %q, want the image's data", got[:n])
	}
}

func TestSeedManagedVolumeCopiesImageTreeDirectly(t *testing.T) {
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	data := filepath.Join(rootfs, "var", "lib", "app")
	if err := os.MkdirAll(filepath.Join(data, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "nested", "payload"), []byte("persistent image data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(data, "nested", "payload"), "user.ttc-seed", []byte("xattr-data"), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(data, "nested", "payload"), filepath.Join(data, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/payload", filepath.Join(data, "symlink")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	manifest.Entries["var/lib/app"] = ownership.Owner{UID: 10, GID: 20}
	manifest.Entries["var/lib/app/nested"] = ownership.Owner{UID: 11, GID: 21}
	manifest.Entries["var/lib/app/nested/payload"] = ownership.Owner{UID: 12, GID: 22}
	manifest.Entries["var/lib/app/hardlink"] = ownership.Owner{UID: 12, GID: 22}
	manifest.Entries["var/lib/app/symlink"] = ownership.Owner{UID: 13, GID: 23}
	manifest.Xattrs["var/lib/app/nested/payload"] = map[string][]byte{"security.capability": []byte("virtual-capability")}
	manifest.Xattrs["var/lib/app/hardlink"] = map[string][]byte{"security.capability": []byte("virtual-capability")}

	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("seed-test")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := seedManagedVolume(repository, rootfs, "/var/lib/app", manifest); err != nil {
		t.Fatal(err)
	}

	root, err := repository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if root.UID != 10 || root.GID != 20 || root.Mode != 0750 {
		t.Fatalf("root metadata = uid %d gid %d mode %#o, want 10:20 0750", root.UID, root.GID, root.Mode)
	}
	nested, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "nested")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := repository.Lookup(ttcvolume.MainBranch, nested, "payload")
	if err != nil {
		t.Fatal(err)
	}
	hardlink, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "hardlink")
	if err != nil {
		t.Fatal(err)
	}
	if hardlink != payload {
		t.Fatalf("hardlink inode = %d, want payload inode %d", hardlink, payload)
	}
	got := make([]byte, len("persistent image data"))
	if _, err := repository.ReadAt(ttcvolume.MainBranch, payload, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "persistent image data" {
		t.Fatalf("payload = %q", got)
	}
	inode, err := repository.Inode(ttcvolume.MainBranch, payload)
	if err != nil {
		t.Fatal(err)
	}
	if inode.UID != 12 || inode.GID != 22 || inode.Mode != 0640 || inode.Nlink != 2 {
		t.Fatalf("payload metadata = %+v", inode)
	}
	if value, ok := inode.Xattrs["user.ttc-seed"]; ok && value != "xattr-data" {
		t.Fatalf("payload xattr = %q, want xattr-data", value)
	}
	if value := inode.Xattrs["security.capability"]; value != "virtual-capability" {
		t.Fatalf("payload virtual capability = %q, want virtual-capability", value)
	}
	link, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "symlink")
	if err != nil {
		t.Fatal(err)
	}
	linkInode, err := repository.Inode(ttcvolume.MainBranch, link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInode.Type != "symlink" || linkInode.Target != "nested/payload" {
		t.Fatalf("symlink inode = %+v", linkInode)
	}
}

func TestSeedCopiedHostMountDirectoryAndFile(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(source, "nested", "payload")
	if err := os.WriteFile(payloadPath, []byte("host-data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(payloadPath, filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("copied-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCopiedHostMount(repository, source, info); err != nil {
		t.Fatal(err)
	}
	nested, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "nested")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := repository.Lookup(ttcvolume.MainBranch, nested, "payload")
	if err != nil {
		t.Fatal(err)
	}
	hardlink, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "hardlink")
	if err != nil {
		t.Fatal(err)
	}
	if payload != hardlink {
		t.Fatalf("hardlink inode=%d, want %d", hardlink, payload)
	}
	got := make([]byte, len("host-data"))
	if _, err := repository.ReadAt(ttcvolume.MainBranch, payload, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "host-data" {
		t.Fatalf("copied payload=%q", got)
	}

	fileSource := filepath.Join(t.TempDir(), "single")
	if err := os.WriteFile(fileSource, []byte("single-file"), 0600); err != nil {
		t.Fatal(err)
	}
	fileRepository, err := manager.Create("copied-file")
	if err != nil {
		t.Fatal(err)
	}
	defer fileRepository.Close()
	fileInfo, err := os.Stat(fileSource)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := hostFileOwner(fileInfo)
	if err := fileRepository.InitializeRootRegular(uint32(fileInfo.Mode().Perm()), uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := seedCopiedHostMount(fileRepository, fileSource, fileInfo); err != nil {
		t.Fatal(err)
	}
	root, err := fileRepository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if root.Type != "regular" || root.Mode != 0600 || root.Size != int64(len("single-file")) {
		t.Fatalf("copied file root=%+v", root)
	}
	got = make([]byte, len("single-file"))
	if _, err := fileRepository.ReadAt(ttcvolume.MainBranch, ttcvolume.RootInode, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "single-file" {
		t.Fatalf("copied file=%q", got)
	}
}

func TestBuildSpecDonatesPersistentRootfsUpper(t *testing.T) {
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	repository := filepath.Join(t.TempDir(), "rootfs-upper")
	spec, err := BuildSpec(
		ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}},
		SpecOptions{
			Name:                      "persistent",
			RootFS:                    rootfs,
			RootfsUpperRepositoryPath: repository,
			RootfsUpperCopyMode:       string(ttcvolume.CopyModeReflink),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 {
		t.Fatalf("repositories = %+v, want one rootfs upper", repositories)
	}
	got := repositories[0]
	if got.Source != specutils.TTCRootfsUpperSource || got.Path != repository || got.Branch != ttcvolume.MainBranch || got.CopyMode != string(ttcvolume.CopyModeReflink) {
		t.Fatalf("rootfs upper repository = %+v", got)
	}
	if _, ok := spec.Annotations[specutils.AnnotationRootfsUpperTar]; ok {
		t.Fatal("generated TTC spec still has a rootfs upper tar annotation")
	}
	if _, ok := spec.Annotations[specutils.AnnotationMountTarOnExit]; ok {
		t.Fatal("generated TTC spec still has an exit-time tar annotation")
	}
}

func TestBuildSpecRecordsImmutableLiveForkEligibility(t *testing.T) {
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	for _, tc := range []struct {
		name  string
		waive bool
		want  string
	}{
		{name: "eligible by default", want: "true"},
		{name: "explicitly waived", waive: true, want: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := BuildSpec(image, SpecOptions{
				RootFS:        t.TempDir(),
				WaiveLiveFork: tc.waive,
			})
			if err != nil {
				t.Fatalf("BuildSpec: %v", err)
			}
			if got := spec.Annotations[specutils.AnnotationTTCLiveForkEligible]; got != tc.want {
				t.Fatalf("live-fork eligibility annotation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadRejectsLegacyHostNetworkMetadata(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "legacy-host")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["true"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "legacy-host", Name: "legacy-host", BundleDir: bundleDir,
		NetworkMode: "host", State: "exited", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Root: root}).LoadContainer("legacy-host"); err == nil {
		t.Fatal("legacy host family metadata was accepted")
	}
}

func TestValidateManagedVolumeAttachment(t *testing.T) {
	containers := []ContainerInfo{{
		Name: "writer",
		Volumes: []ManagedVolume{{
			ID: "volume-id",
		}},
	}}
	if err := validateManagedVolumeAttachment("volume-id", ManagedVolume{Name: "data"}, containers); err == nil {
		t.Fatal("second writable attachment succeeded")
	}
	if err := validateManagedVolumeAttachment("volume-id", ManagedVolume{Name: "data", Readonly: true}, containers); err != nil {
		t.Fatalf("read-only attachment failed: %v", err)
	}
	if err := validateManagedVolumeAttachment("other-id", ManagedVolume{Name: "other"}, containers); err != nil {
		t.Fatalf("unrelated writable attachment failed: %v", err)
	}
}

func TestBuildSpecCombinesImageAndOverrides(t *testing.T) {
	pidsLimit := int64(23)
	memoryLimit := int64(256 * 1024 * 1024)
	nanoCPUs := int64(1_500_000_000)
	cpuShares := uint64(512)
	shmSize := int64(32 * 1024 * 1024)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:123:456::/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:456:\nextra:x:789:app\nmanual:x:900:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(root), manifest); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "fixture"), []byte("host-data"), 0644); err != nil {
		t.Fatal(err)
	}
	image := ocispec.Image{Config: ocispec.ImageConfig{
		User:       "app",
		Env:        []string{"A=image", "B=first", "B=keep"},
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		WorkingDir: "/work",
		StopSignal: "SIGQUIT",
		Labels:     map[string]string{"keep": "yes", "suite": "image"},
	}}
	spec, err := BuildSpec(image, SpecOptions{
		Name:           "demo",
		RootFS:         root,
		ReadonlyRootfs: true,
		Terminal:       true,
		TerminalType:   "screen-256color",
		ConsoleSize:    &specs.Box{Height: 37, Width: 91},
		StopSignal:     "SIGUSR1",
		PidsLimit:      &pidsLimit,
		MemoryLimit:    &memoryLimit,
		NanoCPUs:       &nanoCPUs,
		CPUShares:      &cpuShares,
		CgroupsPath:    "ttc/demo-id",
		ShmSize:        &shmSize,
		GroupAdd:       []string{"manual", "901", "manual"},
		CapAdd:         []string{"sys_admin"},
		CapDrop:        []string{"CAP_NET_RAW"},
		Ulimits:        []Ulimit{{Name: "core", Soft: 0, Hard: -1}, {Name: "nofile", Soft: 64, Hard: 128}},
		Args:           []string{"override", "arg"},
		Env:            []string{"A=run", "B", "C=new"},
		Labels:         map[string]string{"empty": "", "extra": "value", "suite": "run"},
		HostMounts:     []HostMount{{Source: host, Destination: "/mnt/fixture", Readonly: true, ImmutableSource: true}},
		FilesystemMounts: []FilesystemMount{
			{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
			{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := spec.Annotations[ImmutableHostMountsAnnotation], `["/mnt/fixture"]`; got != want {
		t.Errorf("immutable host mounts annotation=%q, want %q", got, want)
	}
	if got, want := spec.Annotations[specutils.ContainerdContainerNameAnnotation], "demo"; got != want {
		t.Errorf("container name annotation=%q, want %q", got, want)
	}

	if !spec.Root.Readonly {
		t.Error("root filesystem is writable")
	}
	if !spec.Process.Terminal || spec.Process.ConsoleSize == nil || spec.Process.ConsoleSize.Height != 37 || spec.Process.ConsoleSize.Width != 91 {
		t.Errorf("terminal configuration=%+v size=%+v", spec.Process.Terminal, spec.Process.ConsoleSize)
	}
	if got, want := spec.Process.Args, []string{"/entry", "override", "arg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args=%q, want %q", got, want)
	}
	if got, want := spec.Process.Env, []string{"A=run", "C=new", "TERM=screen-256color"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env=%q, want %q", got, want)
	}
	if spec.Process.Cwd != "/work" || spec.Hostname != "demo" {
		t.Errorf("cwd=%q hostname=%q", spec.Process.Cwd, spec.Hostname)
	}
	if spec.Process.User.UID != 123 || spec.Process.User.GID != 456 || !reflect.DeepEqual(spec.Process.User.AdditionalGids, []uint32{789, 900, 901}) {
		t.Errorf("user=%+v", spec.Process.User)
	}
	wantCapabilities := []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE", "CAP_SYS_ADMIN"}
	if !reflect.DeepEqual(spec.Process.Capabilities.Bounding, wantCapabilities) {
		t.Errorf("bounding capabilities=%+v, want %q", spec.Process.Capabilities.Bounding, wantCapabilities)
	}
	var wantEffectiveCapabilities []string
	if !reflect.DeepEqual(spec.Process.Capabilities.Effective, wantEffectiveCapabilities) || !reflect.DeepEqual(spec.Process.Capabilities.Permitted, wantEffectiveCapabilities) {
		t.Errorf("non-root effective/permitted capabilities=%+v, want %q", spec.Process.Capabilities, wantEffectiveCapabilities)
	}
	wantMounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=33554432"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/scratch", Type: "tmpfs", Source: "tmpfs", Options: []string{"rw", "mode=0700", "size=1m"}},
		{Destination: "/alt-proc", Type: "proc", Source: "proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/alt-sys", Type: "sysfs", Source: "sysfs", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/mnt/fixture", Type: "bind", Source: host, Options: []string{"rbind", "ro"}},
	}
	if !reflect.DeepEqual(spec.Mounts, wantMounts) {
		t.Errorf("mounts=%+v, want %+v", spec.Mounts, wantMounts)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.overlay"]; got != "memory" {
		t.Errorf("root overlay=%q, want memory", got)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.source"]; got != root {
		t.Errorf("root source=%q, want %q", got, root)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.type"]; got != "bind" {
		t.Errorf("root type=%q, want bind", got)
	}
	if got := spec.Annotations[TmpfsConfigAnnotation]; got != `{"/scratch":"mode=0700,size=1m"}` {
		t.Errorf("tmpfs configuration annotation=%q", got)
	}
	if got := spec.Annotations[ownership.Annotation]; got != ownership.ManifestPath(root) {
		t.Errorf("ownership annotation=%q, want %q", got, ownership.ManifestPath(root))
	}
	if got := spec.Annotations["dev.timetravelcontainer.internal-console"]; got != "init" {
		t.Errorf("internal console=%q, want init", got)
	}
	if got := spec.Annotations["dev.timetravelcontainer.config.network"]; got != "none" {
		t.Errorf("network annotation=%q, want none", got)
	}
	if spec.Linux.Resources == nil || spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != pidsLimit {
		t.Errorf("pids resources=%+v, want limit %d", spec.Linux.Resources, pidsLimit)
	}
	if spec.Linux.CgroupsPath != "ttc/demo-id" {
		t.Errorf("cgroups path=%q, want ttc/demo-id", spec.Linux.CgroupsPath)
	}
	if spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil || *spec.Linux.Resources.Memory.Limit != memoryLimit {
		t.Errorf("memory resources=%+v, want limit %d", spec.Linux.Resources.Memory, memoryLimit)
	}
	if cpu := spec.Linux.Resources.CPU; cpu == nil || cpu.Quota == nil || *cpu.Quota != 150000 || cpu.Period == nil || *cpu.Period != 100000 || cpu.Shares == nil || *cpu.Shares != cpuShares {
		t.Errorf("CPU resources=%+v", cpu)
	}
	wantRlimits := []specs.POSIXRlimit{
		{Type: "RLIMIT_CORE", Soft: 0, Hard: ^uint64(0)},
		{Type: "RLIMIT_NOFILE", Soft: 64, Hard: 128},
	}
	if !reflect.DeepEqual(spec.Process.Rlimits, wantRlimits) {
		t.Errorf("rlimits=%+v, want %+v", spec.Process.Rlimits, wantRlimits)
	}
	if got := spec.Annotations[PidsLimitAnnotation]; got != "sentry" {
		t.Errorf("pids enforcement annotation=%q, want sentry", got)
	}
	wantConfigAnnotations := map[string]string{
		"dev.timetravelcontainer.config.entrypoint": `["/entry"]`,
		"dev.timetravelcontainer.config.cmd":        `["override","arg"]`,
		"dev.timetravelcontainer.config.user":       "app",
		"dev.timetravelcontainer.config.labels":     `{"empty":"","extra":"value","keep":"yes","suite":"run"}`,
		StopSignalAnnotation:                        "SIGUSR1",
	}
	for key, want := range wantConfigAnnotations {
		if got := spec.Annotations[key]; got != want {
			t.Errorf("annotation %q=%q, want %q", key, got, want)
		}
	}
	if !hasNamespace(spec, specs.NetworkNamespace) {
		t.Error("default spec omitted isolated network namespace")
	}
	hostSpec, err := BuildSpec(image, SpecOptions{RootFS: root, NetworkMode: "host"})
	if err != nil {
		t.Fatalf("BuildSpec host network: %v", err)
	}
	if hasNamespace(hostSpec, specs.NetworkNamespace) {
		t.Error("host-network spec retained an isolated network namespace")
	}
	if got := hostSpec.Annotations["dev.timetravelcontainer.config.network"]; got != "host" {
		t.Errorf("host network annotation=%q, want host", got)
	}
	if got := hostSpec.Annotations[StopSignalAnnotation]; got != "SIGQUIT" {
		t.Errorf("image stop signal annotation=%q, want SIGQUIT", got)
	}
}

func TestBuildSpecPreservesConfiguredTerminalType(t *testing.T) {
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{
		Cmd: []string{"sh"},
		Env: []string{"TERM=vt100"},
	}}, SpecOptions{RootFS: t.TempDir(), Terminal: true, TerminalType: "xterm-256color"})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := spec.Process.Env, []string{"TERM=vt100"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env=%q, want configured image terminal type %q", got, want)
	}
}

func TestBuildRlimitsDefaultNofile(t *testing.T) {
	got, err := buildRlimits(nil)
	if err != nil {
		t.Fatalf("buildRlimits(nil): %v", err)
	}
	want := []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 524288}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default rlimits=%+v, want %+v", got, want)
	}
	got, err = buildRlimits([]Ulimit{{Name: "nofile", Soft: 64, Hard: 128}})
	if err != nil {
		t.Fatalf("buildRlimits(nofile=64:128): %v", err)
	}
	want = []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Soft: 64, Hard: 128}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("configured rlimits=%+v, want %+v", got, want)
	}
}

func hasNamespace(spec *specs.Spec, namespace specs.LinuxNamespaceType) bool {
	if spec.Linux == nil {
		return false
	}
	for _, candidate := range spec.Linux.Namespaces {
		if candidate.Type == namespace {
			return true
		}
	}
	return false
}

func TestDockerCapabilitySemantics(t *testing.T) {
	normalized, err := NormalizeCapabilities([]string{"sys_admin", "CAP_CHOWN", "all"})
	if err != nil || !reflect.DeepEqual(normalized, []string{"SYS_ADMIN", "CHOWN", "ALL"}) {
		t.Fatalf("NormalizeCapabilities=(%q, %v)", normalized, err)
	}
	if _, err := NormalizeCapabilities([]string{"not_a_capability"}); err == nil {
		t.Fatal("NormalizeCapabilities accepted an unknown capability")
	}
	onlyChown, err := buildCapabilities([]string{"chown"}, []string{"all"})
	if err != nil || !reflect.DeepEqual(onlyChown, []string{"CAP_CHOWN"}) {
		t.Errorf("drop ALL/add CHOWN=(%q, %v)", onlyChown, err)
	}
	allButAdmin, err := buildCapabilities([]string{"all"}, []string{"sys_admin"})
	if err != nil || containsString(allButAdmin, "CAP_SYS_ADMIN") || len(allButAdmin) != len(allCapabilityNames())-1 {
		t.Errorf("add ALL/drop SYS_ADMIN=(%q, %v)", allButAdmin, err)
	}
	all, err := buildCapabilities([]string{"all"}, []string{"all"})
	if err != nil || !reflect.DeepEqual(all, allCapabilityNames()) {
		t.Errorf("add ALL/drop ALL=(%q, %v), want all", all, err)
	}
}

func TestBuildProcessCapabilitiesAreUserAware(t *testing.T) {
	rootCaps, err := buildProcessCapabilities(0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootCaps.Bounding, defaultCapabilities) ||
		!reflect.DeepEqual(rootCaps.Effective, defaultCapabilities) ||
		!reflect.DeepEqual(rootCaps.Permitted, defaultCapabilities) {
		t.Errorf("root capabilities=%+v, want defaults in bounding/effective/permitted", rootCaps)
	}

	nonRootCaps, err := buildProcessCapabilities(1000, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nonRootCaps.Bounding, defaultCapabilities) {
		t.Errorf("non-root bounding capabilities=%q, want %q", nonRootCaps.Bounding, defaultCapabilities)
	}
	if len(nonRootCaps.Effective) != 0 || len(nonRootCaps.Permitted) != 0 {
		t.Errorf("non-root capabilities=%+v, want empty effective/permitted sets", nonRootCaps)
	}
	if !containsString(nonRootCaps.Bounding, "CAP_SETUID") || !containsString(nonRootCaps.Bounding, "CAP_SETGID") {
		t.Errorf("non-root bounding set %q does not retain SUID helper capabilities", nonRootCaps.Bounding)
	}

	explicitCaps, err := buildProcessCapabilities(1000, []string{"SYS_PTRACE"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(explicitCaps.Bounding, "CAP_SYS_PTRACE") {
		t.Errorf("non-root --cap-add capability missing from bounding set: %+v", explicitCaps)
	}
	if len(explicitCaps.Effective) != 0 || len(explicitCaps.Permitted) != 0 {
		t.Errorf("non-root --cap-add capabilities=%+v, want empty effective/permitted sets", explicitCaps)
	}

	droppedCaps, err := buildProcessCapabilities(1000, nil, []string{"SETUID", "SETGID"})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(droppedCaps.Bounding, "CAP_SETUID") || containsString(droppedCaps.Bounding, "CAP_SETGID") {
		t.Errorf("explicitly dropped capabilities remain in bounding set: %q", droppedCaps.Bounding)
	}
}

func TestParseShmSize(t *testing.T) {
	for value, want := range map[string]int64{"32m": 32 * 1024 * 1024, "1.5g": 1536 * 1024 * 1024, "0": 0} {
		got, err := ParseShmSize(value)
		if err != nil || got != want {
			t.Errorf("ParseShmSize(%q)=(%d, %v), want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "invalid", "-1"} {
		if _, err := ParseShmSize(value); err == nil {
			t.Errorf("ParseShmSize(%q) succeeded", value)
		}
	}
}

func TestParseUlimits(t *testing.T) {
	got, err := ParseUlimits([]string{"nofile=32:64", "core=0:-1", "nofile=128"})
	if err != nil {
		t.Fatalf("ParseUlimits: %v", err)
	}
	want := []Ulimit{{Name: "core", Soft: 0, Hard: -1}, {Name: "nofile", Soft: 128, Hard: 128}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseUlimits=%+v, want %+v", got, want)
	}
	for _, value := range []string{"unknown=1", "nofile", "nofile=2:1"} {
		if _, err := ParseUlimits([]string{value}); err == nil {
			t.Errorf("ParseUlimits(%q) succeeded", value)
		}
	}
}

func TestDockerNetworkConfiguration(t *testing.T) {
	extraHosts, err := ParseExtraHosts([]string{"db:192.0.2.10", "v6=[2001:0db8::10]"})
	if err != nil {
		t.Fatalf("ParseExtraHosts: %v", err)
	}
	if want := []string{"db:192.0.2.10", "v6:2001:db8::10"}; !reflect.DeepEqual(extraHosts, want) {
		t.Errorf("extra hosts=%q, want %q", extraHosts, want)
	}
	dns, err := ParseDNS([]string{"192.0.2.53", "2001:0db8::53"})
	if err != nil {
		t.Fatalf("ParseDNS: %v", err)
	}
	if want := []string{"192.0.2.53", "2001:db8::53"}; !reflect.DeepEqual(dns, want) {
		t.Errorf("DNS=%q, want %q", dns, want)
	}
	search, err := ParseDNSSearch([]string{"example.test", "."})
	if err != nil || !reflect.DeepEqual(search, []string{"example.test", "."}) {
		t.Errorf("DNS search=(%q, %v)", search, err)
	}
	options, err := ParseDNSOptions([]string{"ndots:2", "rotate"})
	if err != nil || !reflect.DeepEqual(options, []string{"ndots:2", "rotate"}) {
		t.Errorf("DNS options=(%q, %v)", options, err)
	}
	for _, value := range []string{"missing-address", ":192.0.2.1", "bad host:192.0.2.1", "host:not-an-ip", "host:host-gateway"} {
		if _, err := ParseExtraHosts([]string{value}); err == nil {
			t.Errorf("ParseExtraHosts(%q) succeeded", value)
		}
	}
	if _, err := ParseDNS([]string{"not-an-ip"}); err == nil {
		t.Error("ParseDNS accepted an invalid address")
	}
	for _, value := range []string{"-bad.example", "bad..example", "bad domain"} {
		if _, err := ParseDNSSearch([]string{value}); err == nil {
			t.Errorf("ParseDNSSearch(%q) succeeded", value)
		}
	}
	for _, value := range []string{"", "ndots:2 rotate", "rotate\noptions timeout:1"} {
		if _, err := ParseDNSOptions([]string{value}); err == nil {
			t.Errorf("ParseDNSOptions(%q) succeeded", value)
		}
	}
}

func TestNetworkFileMaterialization(t *testing.T) {
	containerDir := t.TempDir()
	opts := SpecOptions{
		Name: "demo", Hostname: "custom-host",
		ExtraHosts: []string{"db:192.0.2.10", "v6:2001:db8::10"},
		DNS:        []string{"203.0.113.53"}, DNSSearch: []string{"example.test"}, DNSOptions: []string{"ndots:2", "rotate"},
	}
	mounts, err := materializeNetworkFiles(containerDir, opts, "")
	if err != nil {
		t.Fatalf("materializeNetworkFiles: %v", err)
	}
	if len(mounts) != 3 {
		t.Fatalf("mounts=%+v, want three", mounts)
	}
	wantFiles := map[string]string{
		"/etc/hostname":    "custom-host\n",
		"/etc/hosts":       "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\nfe00::\tip6-localnet\nff00::\tip6-mcastprefix\nff02::1\tip6-allnodes\nff02::2\tip6-allrouters\n192.0.2.10\tdb\n2001:db8::10\tv6\n",
		"/etc/resolv.conf": "nameserver 203.0.113.53\nsearch example.test\noptions ndots:2 rotate\n",
	}
	for _, mount := range mounts {
		if !mount.Readonly || !mount.Internal {
			t.Errorf("network mount is not immutable/internal: %+v", mount)
		}
		data, err := os.ReadFile(mount.Source)
		if err != nil {
			t.Errorf("reading %s: %v", mount.Source, err)
			continue
		}
		if got, want := string(data), wantFiles[mount.Destination]; got != want {
			t.Errorf("%s contents=%q, want %q", mount.Destination, got, want)
		}
		if info, err := os.Stat(mount.Source); err != nil || info.Mode().Perm() != 0444 {
			t.Errorf("%s mode: info=%v err=%v", mount.Source, info, err)
		}
	}
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}, SpecOptions{RootFS: t.TempDir(), HostMounts: mounts})
	if err != nil {
		t.Fatalf("BuildSpec with network files: %v", err)
	}
	if got, want := spec.Annotations[InternalMountsAnnotation], `["/etc/hostname","/etc/hosts","/etc/resolv.conf"]`; got != want {
		t.Errorf("internal mounts annotation=%q, want %q", got, want)
	}
}

func TestEnsureNetworkMountpoints(t *testing.T) {
	rootfs := t.TempDir()
	etc := filepath.Join(rootfs, "etc")
	if err := os.Mkdir(etc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "hostname"), []byte("image-value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run/hosts", filepath.Join(etc, "hosts")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(rootfs), manifest); err != nil {
		t.Fatal(err)
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		t.Fatalf("ensureNetworkMountpoints: %v", err)
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		t.Fatalf("idempotent ensureNetworkMountpoints: %v", err)
	}
	for _, base := range []string{"hostname", "hosts", "resolv.conf"} {
		name := filepath.Join(etc, base)
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
			t.Errorf("mountpoint %s: info=%v err=%v", base, info, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(etc, "hostname")); err != nil || string(data) != "image-value" {
		t.Errorf("existing hostname contents=%q, err=%v", data, err)
	}
	updated, err := ownership.Read(ownership.ManifestPath(rootfs))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"etc/hostname", "etc/hosts", "etc/resolv.conf"} {
		if owner, ok := updated.Entries[name]; !ok || owner != (ownership.Owner{}) {
			t.Errorf("mountpoint owner %s=(%+v, %t), want root", name, owner, ok)
		}
	}
}

// Android's images link /etc to /system/etc.
func TestEnsureNetworkMountpointsFollowsEtcLink(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "system", "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "system", "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/system/etc", filepath.Join(rootfs, "etc")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(rootfs), manifest); err != nil {
		t.Fatal(err)
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		t.Fatalf("ensureNetworkMountpoints: %v", err)
	}
	for _, base := range []string{"hostname", "hosts", "resolv.conf"} {
		if info, err := os.Lstat(filepath.Join(rootfs, "system", "etc", base)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("mountpoint /system/etc/%s: info=%v err=%v", base, info, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(rootfs, "etc")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("/etc is no longer a link: info=%v err=%v", info, err)
	}
	updated, err := ownership.Read(ownership.ManifestPath(rootfs))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"system/etc/hostname", "system/etc/hosts", "system/etc/resolv.conf"} {
		if _, ok := updated.Entries[name]; !ok {
			t.Errorf("no ownership of mountpoint %s", name)
		}
	}
}

func TestResolveInRootfs(t *testing.T) {
	rootfs := t.TempDir()
	for _, dir := range []string{"system/etc", "vendor"} {
		if err := os.MkdirAll(filepath.Join(rootfs, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"etc":        "/system/etc",
		"relative":   "system/./etc",
		"escape":     "../../../../vendor",
		"loop":       "loop",
		"vendor/sys": "../system",
		"dangling":   "/nowhere/at/all",
	} {
		if err := os.Symlink(target, filepath.Join(rootfs, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, want string
	}{
		{"etc", "system/etc"},
		{"etc/hosts", "system/etc/hosts"},
		{"/relative/", "system/etc"},
		{"escape", "vendor"},
		{"vendor/sys/etc", "system/etc"},
		{"dangling/x", "nowhere/at/all/x"},
		{"missing/../a", "a"},
		{"missing/../../../x", "x"},
	} {
		got, err := resolveInRootfs(rootfs, test.name)
		if err != nil || got != test.want {
			t.Errorf("resolveInRootfs(%q) = %q, %v; want %q", test.name, got, err, test.want)
		}
	}
	if _, err := resolveInRootfs(rootfs, "loop"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("resolveInRootfs(loop) = %v, want ELOOP", err)
	}
}

func TestBuildResolvConf(t *testing.T) {
	host := []byte("# generated\nnameserver 192.0.2.1\ndomain host.test\noptions timeout:1 rotate\n")
	if got, want := buildResolvConf(host, nil, nil, nil), "nameserver 192.0.2.1\nsearch host.test\noptions timeout:1 rotate\n"; got != want {
		t.Errorf("inherited resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConf(host, []string{"203.0.113.53"}, []string{"custom.test"}, []string{"ndots:3"}), "nameserver 203.0.113.53\nsearch custom.test\noptions ndots:3\n"; got != want {
		t.Errorf("overridden resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConf(nil, nil, nil, nil), "nameserver 127.0.0.1\n"; got != want {
		t.Errorf("fallback resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConfForNetwork([]byte("nameserver 127.0.0.53\n"), nil, nil, nil, "ttc", ""), "nameserver 10.0.2.53\n"; got != want {
		t.Errorf("TTC host stub resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConfForNetwork(nil, []string{"127.0.0.1", "203.0.113.53"}, nil, nil, "ttc", ""), "nameserver 10.0.2.2\nnameserver 203.0.113.53\n"; got != want {
		t.Errorf("TTC mixed resolver=%q, want %q", got, want)
	}
	// TTC's NAT gives the host's loopback addresses to the guest's own /24,
	// whatever --ip puts it in.
	if got, want := buildResolvConfForNetwork([]byte("nameserver 127.0.0.53\n"), nil, nil, nil, "ttc", "192.0.2.44"), "nameserver 192.0.2.53\n"; got != want {
		t.Errorf("TTC host stub resolver with --ip=%q, want %q", got, want)
	}
	if got, want := buildResolvConfForNetwork([]byte("nameserver 127.0.0.44\nnameserver 127.0.0.15\n"), nil, nil, nil, "ttc", "192.0.2.44"), "nameserver 192.0.2.2\nnameserver 192.0.2.15\n"; got != want {
		t.Errorf("TTC resolver at the guest's own address=%q, want %q", got, want)
	}
}

func TestBuildSpecRejectsInvalidOverrides(t *testing.T) {
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), Env: []string{"=INVALID"}}); err == nil {
		t.Error("BuildSpec accepted invalid environment override")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), Cwd: "relative"}); err == nil {
		t.Error("BuildSpec accepted relative working directory")
	}
	host := t.TempDir()
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: host, Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted writable host mount")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: "relative", Destination: "/data", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted relative host source")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: host, Destination: "relative", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted relative host destination")
	}
	missing := filepath.Join(host, "missing")
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: missing, Destination: "/data", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted missing host source")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "ramfs", Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted unsupported filesystem mount type")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "relative"}}}); err == nil {
		t.Error("BuildSpec accepted relative filesystem mount destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/data", Readonly: true, Options: []string{"rw"}}}}); err == nil {
		t.Error("BuildSpec accepted a conflicting filesystem access option")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/data", Options: []string{"rw", "ro"}}}}); err == nil {
		t.Error("BuildSpec accepted multiple filesystem access options")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/proc"}}}); err == nil {
		t.Error("BuildSpec accepted a filesystem mount over a default destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "proc", Destination: "/alt-proc", HostConfig: true}}}); err == nil {
		t.Error("BuildSpec accepted Docker tmpfs projection for procfs")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), NetworkMode: "bridge"}); err == nil {
		t.Error("BuildSpec accepted unsupported network mode")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), StopSignal: "SIGINVALID"}); err == nil {
		t.Error("BuildSpec accepted an invalid stop signal")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), GroupAdd: []string{"missing-group"}}); err == nil {
		t.Error("BuildSpec accepted an unknown additional group")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), CapAdd: []string{"not-a-capability"}}); err == nil {
		t.Error("BuildSpec accepted an unknown capability")
	}
	invalidShmSize := int64(-1)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), ShmSize: &invalidShmSize}); err == nil {
		t.Error("BuildSpec accepted a negative shared memory size")
	}
	invalidPidsLimit := int64(-2)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), PidsLimit: &invalidPidsLimit}); err == nil {
		t.Error("BuildSpec accepted an invalid pids limit")
	}
	invalidMemoryLimit := int64(-1)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), MemoryLimit: &invalidMemoryLimit}); err == nil {
		t.Error("BuildSpec accepted an invalid memory limit")
	}
	unlimitedPids := int64(-1)
	unlimitedSpec, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), PidsLimit: &unlimitedPids})
	if err != nil {
		t.Fatalf("BuildSpec rejected unlimited pids: %v", err)
	}
	if unlimitedSpec.Linux.Resources != nil || unlimitedSpec.Annotations[PidsLimitAnnotation] != "" {
		t.Errorf("unlimited pids unexpectedly enabled enforcement: resources=%+v annotation=%q", unlimitedSpec.Linux.Resources, unlimitedSpec.Annotations[PidsLimitAnnotation])
	}
}

func TestBuildSpecRequiresResolvedImageVolumes(t *testing.T) {
	image := ocispec.Image{Config: ocispec.ImageConfig{
		Cmd:     []string{"true"},
		Volumes: map[string]struct{}{"/data": {}},
	}}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("BuildSpec image volume error=%v, want unresolved error", err)
	}
	if _, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		FilesystemMounts: []FilesystemMount{{
			Type: "tmpfs", Destination: "/data", Options: []string{"rw"},
		}},
	}); err != nil {
		t.Fatalf("BuildSpec with explicit replacement mount: %v", err)
	}
	spec, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		ManagedVolumes: []ManagedVolume{{
			Name: "data", ID: "0123456789abcdef0123456789abcdef",
			Destination: "/data", RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatalf("BuildSpec with resolved managed volume: %v", err)
	}
	if got := spec.Annotations["dev.timetravelcontainer.volume-repositories"]; !strings.Contains(got, `"/repository"`) {
		t.Fatalf("volume repository annotation=%q", got)
	}
}

func TestBuildSpecRejectsOverflowingNumericIDsBeforeImageLookup(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	overflow := strings.Repeat("9", 64)
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(fmt.Sprintf("%s:x:0:0::/root:/bin/sh\n", overflow)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte(fmt.Sprintf("%s:x:0:\n", overflow)), 0644); err != nil {
		t.Fatal(err)
	}
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	for _, tc := range []struct {
		name string
		opts SpecOptions
	}{
		{
			name: "user",
			opts: SpecOptions{User: overflow},
		},
		{
			name: "primary group",
			opts: SpecOptions{User: "123:" + overflow},
		},
		{
			name: "additional group",
			opts: SpecOptions{GroupAdd: []string{overflow}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.RootFS = root
			if _, err := BuildSpec(image, tc.opts); err == nil {
				t.Fatalf("BuildSpec accepted overflowing numeric ID %q through image name lookup", overflow)
			}
		})
	}
}

func TestBuildSpecAcceptsMaximumNumericUserAndGroupIDs(t *testing.T) {
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{
		Cmd:  []string{"true"},
		User: "2147483647:2147483647",
	}}, SpecOptions{RootFS: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildSpec rejected maximum numeric IDs: %v", err)
	}
	if got, want := spec.Process.User.UID, uint32(maxContainerID); got != want {
		t.Errorf("UID=%d, want %d", got, want)
	}
	if got, want := spec.Process.User.GID, uint32(maxContainerID); got != want {
		t.Errorf("GID=%d, want %d", got, want)
	}
}

func TestPullPolicyAndStoredImageSelection(t *testing.T) {
	for _, policy := range []string{"", "missing", "always", "never"} {
		got, err := normalizePullPolicy(policy)
		if err != nil {
			t.Errorf("normalizePullPolicy(%q): %v", policy, err)
		}
		if policy == "" && got != "missing" || policy != "" && got != policy {
			t.Errorf("normalizePullPolicy(%q)=%q", policy, got)
		}
	}
	if _, err := normalizePullPolicy("sometimes"); err == nil {
		t.Fatal("normalizePullPolicy accepted an unsupported policy")
	}

	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:aaaa" + strings.Repeat("b", 60)
	stored := []ttcimages.Info{
		{Reference: "docker.io/library/alpine:3.20", Digest: digestA, Platform: "linux/amd64"},
		{Reference: "docker.io/library/alpine:latest", Digest: digestA, Platform: "linux/amd64"},
		{Reference: "docker.io/library/alternate:latest", Digest: digestB, Platform: "linux/arm64"},
	}
	got, found, err := selectStoredImage(stored, "alpine:3.20", "linux/amd64")
	if err != nil || !found || got.Reference != "docker.io/library/alpine:3.20" {
		t.Fatalf("selectStoredImage by name=(%+v, %t, %v)", got, found, err)
	}
	got, found, err = selectStoredImage(stored, strings.Repeat("a", 12), "linux/amd64")
	if err != nil || !found || got.Digest != digestA {
		t.Fatalf("selectStoredImage by digest=(%+v, %t, %v)", got, found, err)
	}
	if _, found, err := selectStoredImage(stored, "alpine:3.20", "linux/arm64"); err != nil || found {
		t.Errorf("platform mismatch=(found=%t, err=%v)", found, err)
	}
	if _, found, err := selectStoredImage(stored, "missing:latest", ""); err != nil || found {
		t.Errorf("missing image=(found=%t, err=%v)", found, err)
	}
	if _, _, err := selectStoredImage(stored, "aaaa", ""); !errors.Is(err, ttcimages.ErrAmbiguous) {
		t.Errorf("ambiguous prefix error=%v", err)
	}
}

// An image is ready to prepare a container from when it is stored and its root
// is unpacked on this node; only then does preparing neither pull nor unpack.
func TestImageReadyNeedsTheImageAndItsRoot(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	digest := "sha256:" + strings.Repeat("a", 64)
	stored := []ttcimages.Info{{Reference: "docker.io/library/alpine:3.20", Digest: digest, Platform: "linux/amd64"}}
	ready := func(reference, platform string) bool {
		t.Helper()
		image, got, err := store.imageReadyAmong(stored, reference, platform)
		if err != nil {
			t.Fatalf("imageReadyAmong(%q, %q): %v", reference, platform, err)
		}
		if got != (image.Digest == digest) {
			t.Fatalf("imageReadyAmong(%q, %q) = %+v, %t: a ready image is the one stored, and only it", reference, platform, image, got)
		}
		return got
	}
	if ready("alpine:3.20", "") {
		t.Error("an image whose root is not unpacked is ready")
	}
	dir := filepath.Join(root, "images", imageRootKey(digest, "linux/amd64"))
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(filepath.Join(dir, "rootfs")), manifest); err != nil {
		t.Fatal(err)
	}
	if !ready("alpine:3.20", "") || !ready("alpine:3.20", "linux/amd64") {
		t.Error("a stored image with its root unpacked is not ready")
	}
	if ready("alpine:3.20", "linux/arm64") {
		t.Error("an image is ready for a platform it was not stored for")
	}
	if ready("busybox:latest", "") {
		t.Error("an image that is not stored is ready")
	}
	if _, got, err := store.ImageReady(context.Background(), "alpine:3.20", "", "always"); err != nil || got {
		t.Errorf("ImageReady under the pull policy always=(%t, %v), want a pull", got, err)
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"web", "web-1", "Web_1.test"} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "-bad", "bad/name", "bad@branch"} {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) succeeded", name)
		}
	}
}

func TestWriteJSONAtomicallyReplacesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.json")
	if err := writeJSON(path, map[string]int{"generation": 1}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := writeJSON(path, map[string]int{"generation": 2}); err != nil {
		t.Fatal(err)
	}
	oldData, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	newData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oldValue, newValue map[string]int
	if err := json.Unmarshal(oldData, &oldValue); err != nil {
		t.Fatalf("old metadata is incomplete: %v", err)
	}
	if err := json.Unmarshal(newData, &newValue); err != nil {
		t.Fatalf("new metadata is incomplete: %v", err)
	}
	if oldValue["generation"] != 1 || newValue["generation"] != 2 {
		t.Errorf("metadata generations old=%v new=%v", oldValue, newValue)
	}
}

func TestSelectLogAttributes(t *testing.T) {
	spec := &specs.Spec{
		Process:     &specs.Process{Env: []string{"APP_ENV=image", "TRACE_ID=abc", "APP_ENV=runtime"}},
		Annotations: map[string]string{"dev.timetravelcontainer.config.labels": `{"suite":"fast","owner":"ttc","skip":"no"}`},
	}
	got, err := selectLogAttributes(map[string]string{
		"labels": "suite", "labels-regex": "^owner$", "env": "APP_ENV", "env-regex": "^TRACE_",
	}, spec)
	if err != nil {
		t.Fatalf("selectLogAttributes: %v", err)
	}
	want := map[string]string{"suite": "fast", "owner": "ttc", "APP_ENV": "runtime", "TRACE_ID": "abc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attributes=%q, want %q", got, want)
	}
	if _, err := selectLogAttributes(map[string]string{"env-regex": "["}, spec); err == nil {
		t.Fatal("selectLogAttributes accepted an invalid regex")
	}
}

func TestStoreLifecycleMetadata(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["true"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{Name: "demo", BundleDir: bundleDir, ConsoleLogMaxBytes: 8 << 20, ConsoleLogMaxFiles: 5, State: "created", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	status := 17
	if err := store.UpdateState("demo", "exited", &status); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	containers, err := store.ListContainers()
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].State != "exited" || containers[0].ExitStatus == nil || *containers[0].ExitStatus != 17 || containers[0].ExitedAt == nil {
		t.Fatalf("containers=%+v", containers)
	}
	spec, err := store.LoadSpec("demo")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if spec.Process == nil || !reflect.DeepEqual(spec.Process.Args, []string{"true"}) {
		t.Errorf("loaded spec=%+v", spec)
	}
	if len(containers[0].Branches) != 1 || containers[0].Branches[0].ID != "main" || containers[0].Branches[0].State != "exited" {
		t.Errorf("legacy branch history=%+v, want exited main", containers[0].Branches)
	}
	childCreated := time.Now().UTC()
	child := BranchInfo{ID: "child", ParentID: "main", FilesystemBranch: ttcvolume.ScopedBranchID("demo", "child"), State: "running", StorageState: "local", CreatedAt: childCreated, ForkTotalNS: 123}
	if err := store.RecordBranches("demo", []BranchInfo{
		child,
		{ID: "grandchild", ParentID: "child", FilesystemBranch: ttcvolume.ScopedBranchID("demo", "grandchild"), State: "paused", StorageState: "local", DistributedGraphReferenceOperation: "branch-operation", CreatedAt: childCreated.Add(time.Second)},
	}); err != nil {
		t.Fatalf("RecordBranches: %v", err)
	}
	if err := store.RemoveBranchRecord("demo", "child"); err != nil {
		t.Fatalf("RemoveBranchRecord: %v", err)
	}
	withHistory, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("LoadContainer with lineage: %v", err)
	}
	if len(withHistory.Branches) != 2 || withHistory.Branches[1].ID != "grandchild" || withHistory.Branches[1].ParentID != "main" || withHistory.Branches[1].DistributedGraphReferenceOperation != "branch-operation" {
		t.Fatalf("persisted lineage=%+v", withHistory.Branches)
	}
	if err := store.RecordBranch("demo", BranchInfo{ID: "child", ParentID: "grandchild", FilesystemBranch: ttcvolume.ScopedBranchID("demo", "child-reused"), State: "running", StorageState: "local", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("RecordBranch did not allow reuse: %v", err)
	}
	if err := store.UpdateState("demo", "exited", &status); err != nil {
		t.Fatalf("UpdateState with descendants: %v", err)
	}
	withHistory, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("LoadContainer after family exit: %v", err)
	}
	states := make(map[string]string)
	for _, branch := range withHistory.Branches {
		states[branch.ID] = branch.State
	}
	if states["main"] != "exited" || states["child"] != "running" || states["grandchild"] != "paused" || withHistory.State != "running" || withHistory.ExitStatus != nil {
		t.Errorf("branch-specific UpdateState produced family state=%q exit=%v branches=%+v", withHistory.State, withHistory.ExitStatus, withHistory.Branches)
	}
	if err := store.RemoveBranchRecord("demo", "main"); err != nil {
		t.Fatalf("RemoveBranchRecord(main): %v", err)
	}
	withHistory, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("LoadContainer after main deletion: %v", err)
	}
	if len(withHistory.Branches) != 2 || withHistory.Branches[0].ID != "grandchild" || withHistory.Branches[0].ParentID != "" || withHistory.State != "running" {
		t.Errorf("main deletion state=%q branches=%+v, want reparented peers", withHistory.State, withHistory.Branches)
	}
	if containers[0].ConsoleLogMaxBytes != 8<<20 || containers[0].ConsoleLogMaxFiles != 5 {
		t.Errorf("retention after metadata update=(%d, %d), want (%d, 5)", containers[0].ConsoleLogMaxBytes, containers[0].ConsoleLogMaxFiles, 8<<20)
	}
	if containers[0].LogDriver != DefaultLogDriver {
		t.Errorf("legacy log driver=%q, want %q", containers[0].LogDriver, DefaultLogDriver)
	}
	if containers[0].NetworkMode != "none" {
		t.Errorf("legacy network mode=%q, want none", containers[0].NetworkMode)
	}
	if !containers[0].LiveForkEligible || containers[0].LiveForkWaiver != "" {
		t.Errorf("legacy contained family eligibility=(%t, %q), want eligible without waiver", containers[0].LiveForkEligible, containers[0].LiveForkWaiver)
	}
	if containers[0].StopSignal != DefaultStopSignal || containers[0].StopTimeout == nil || *containers[0].StopTimeout != DefaultStopTimeoutSeconds {
		t.Errorf("legacy stop policy=(%q, %v), want (%q, %d)", containers[0].StopSignal, containers[0].StopTimeout, DefaultStopSignal, DefaultStopTimeoutSeconds)
	}
	if containers[0].ShmSize == nil || *containers[0].ShmSize != DefaultShmSizeBytes {
		t.Errorf("legacy shared memory size=%v, want %d", containers[0].ShmSize, DefaultShmSizeBytes)
	}
	paths, err := store.BranchConsolePaths("demo", "test-a")
	if err != nil {
		t.Fatalf("BranchConsolePaths: %v", err)
	}
	wantBase := filepath.Join(containerDir, "console", "test-a")
	if paths.Log != wantBase+".log" || paths.Lock != wantBase+".lock" ||
		paths.StdinLock != wantBase+".stdin.lock" || paths.Error != wantBase+".error.log" ||
		paths.WaitStatus != wantBase+".wait-status" {
		t.Errorf("console paths=%+v, want base %q", paths, wantBase)
	}
	if info, err := os.Stat(filepath.Dir(paths.Log)); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Errorf("console directory: info=%v err=%v", info, err)
	}
	execPaths, err := store.ExecConsolePaths("demo", "test-a", "exec-1")
	if err != nil {
		t.Fatalf("ExecConsolePaths: %v", err)
	}
	wantExecBase := filepath.Join(containerDir, "console", "exec", "test-a", "exec-1")
	if execPaths.Log != wantExecBase+".log" || execPaths.WaitStatus != wantExecBase+".wait-status" {
		t.Errorf("exec console paths=%+v, want base %q", execPaths, wantExecBase)
	}
	if _, err := store.ExecConsolePaths("demo", "test-a", "../escape"); err == nil {
		t.Fatal("ExecConsolePaths accepted invalid exec ID")
	}

	if _, err := store.BranchConsolePaths("demo", "../escape"); err == nil {
		t.Fatal("BranchConsolePaths accepted invalid branch")
	}
	if _, err := store.BranchConsolePaths("missing", "main"); err == nil {
		t.Fatal("BranchConsolePaths accepted missing container")
	}
	if err := store.RemoveContainer("demo"); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if containers, err := store.ListContainers(); err != nil || len(containers) != 0 {
		t.Fatalf("containers after remove=%+v, err=%v", containers, err)
	}
}

func TestListContainersSkipsUnreadableDirectory(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	goodDir := filepath.Join(root, "containers", "good")
	if err := os.MkdirAll(filepath.Join(goodDir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "bundle", "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(goodDir, "container.json"), &ContainerInfo{
		ID: "good", Name: "good", BundleDir: filepath.Join(goodDir, "bundle"),
		State: "created", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "containers", "incomplete"), 0700); err != nil {
		t.Fatal(err)
	}

	containers, err := store.ListContainers()
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].ID != "good" {
		t.Fatalf("containers=%+v, want only good", containers)
	}
	if _, err := store.listContainers(true); err == nil {
		t.Fatal("strict container scan ignored incomplete metadata")
	}

	if _, err := store.RecoverContainerRemovals(); err != nil {
		t.Fatalf("RecoverContainerRemovals: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "containers", "incomplete")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("incomplete active directory remains: %v", err)
	}
	if containers, err := store.listContainers(true); err != nil || len(containers) != 1 {
		t.Fatalf("strict containers after recovery=%+v, err=%v", containers, err)
	}
	creatingDir := filepath.Join(root, "containers", "being-created")
	if err := os.Mkdir(creatingDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(creatingDir, containerCreationMarker), []byte("in progress\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if containers, err := store.listContainers(true); err != nil || len(containers) != 1 {
		t.Fatalf("strict containers during marked creation=%+v, err=%v", containers, err)
	}
	if _, err := store.RecoverContainerRemovals(); err != nil {
		t.Fatalf("recovery during marked creation: %v", err)
	}
	if _, err := os.Stat(creatingDir); err != nil {
		t.Fatalf("recovery moved an in-progress cluster-node creation: %v", err)
	}
}

func TestContainerPublicationIsAtomicAgainstRecovery(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	staging, err := store.createContainerStagingDir("target")
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(staging, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	finalBundle := filepath.Join(root, "containers", "target", "bundle")
	info := ContainerInfo{ID: "target", Name: "target", BundleDir: finalBundle, State: "created", CreatedAt: time.Now()}
	if err := writeCurrentContainerFixture(filepath.Join(staging, "container.json"), &info); err != nil {
		t.Fatal(err)
	}

	// A maintenance pass from another daemon must neither expose nor reclaim a
	// container before the single directory-rename publication boundary.
	if _, err := store.RecoverContainerRemovals(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("recovery reclaimed unpublished staging directory: %v", err)
	}
	if containers, err := store.ListContainers(); err != nil || len(containers) != 0 {
		t.Fatalf("unpublished containers=%+v, err=%v", containers, err)
	}

	if err := store.publishContainerDir(info.ID, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverContainerRemovals(); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadContainer(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BundleDir != finalBundle {
		t.Fatalf("published bundle=%q, want %q", loaded.BundleDir, finalBundle)
	}
}

// A console's directory is made only below a container directory that
// exists: made all the way down after a removal, it made the container's
// directory again, with neither its record nor its marker.
func TestConsolePathsDoNotRemakeARemovedContainer(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root, ActiveRoot: root}
	info := ContainerInfo{ID: strings.Repeat("a", 64)}
	if _, err := store.BranchConsolePathsIn(info, "main"); err == nil {
		t.Fatal("console paths of a removed container were made")
	}
	if _, err := store.ExecConsolePathsIn(info, "main", "session"); err == nil {
		t.Fatal("exec console paths of a removed container were made")
	}
	if _, err := os.Lstat(filepath.Join(root, "containers", info.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed container's directory was made again: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "containers", info.ID), 0700); err != nil {
		t.Fatal(err)
	}
	paths, err := store.ExecConsolePathsIn(info, "main", "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(paths.Log)); err != nil {
		t.Fatalf("exec console directory: %v", err)
	}
	if _, err := store.BranchConsolePathsIn(info, "main"); err != nil {
		t.Fatal(err)
	}
}

// A strict listing never finds a container being made in place without its
// marker: made first and marked after, a directory was for a moment neither
// a record nor one being made, and the listing failed on it.
func TestInPlaceConstructionIsMarkedWhenListed(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root, ActiveRoot: root}
	done := make(chan struct{})
	listed := make(chan error, 1)
	go func() {
		defer close(listed)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := store.listContainers(true); err != nil {
				listed <- err
				return
			}
		}
	}()
	for i := 0; i < 500; i++ {
		if _, _, err := store.createContainerDir(); err != nil {
			close(done)
			t.Fatal(err)
		}
	}
	close(done)
	if err := <-listed; err != nil {
		t.Fatalf("a strict listing failed on a container being made: %v", err)
	}
}

func TestContainerPublicationWithUnifiedActiveRoot(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root, ActiveRoot: root}
	id, building, err := store.createContainerDir()
	if err != nil {
		t.Fatal(err)
	}
	finalDir := filepath.Join(root, "containers", id)
	if building != finalDir {
		t.Fatalf("building directory=%q, want in-place %q", building, finalDir)
	}
	if _, err := os.Stat(filepath.Join(building, containerCreationMarker)); err != nil {
		t.Fatalf("in-place construction has no marker: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, containerStagingDir, id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("in-place construction left its staging directory: %v", err)
	}
	if containers, err := store.listContainers(true); err != nil || len(containers) != 0 {
		t.Fatalf("marked construction is visible: containers=%+v err=%v", containers, err)
	}
	bundle := filepath.Join(building, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(building, "container.json"), &ContainerInfo{
		ID: id, Name: "unified", BundleDir: bundle, State: "created", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.publishContainerDir(id, building); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(finalDir, containerCreationMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("publication marker remains: %v", err)
	}
	loaded, err := store.LoadContainer(id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "unified" {
		t.Fatalf("published container=%+v, want unified", loaded)
	}
}

func TestRemoveContainerCommitsBeforePhysicalCleanup(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, State: "exited", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	allowCleanup := false
	store := Store{Root: root}
	store.removeAllPath = func(path string) error {
		if filepath.Dir(path) == store.removedContainersRoot() && !allowCleanup {
			// Model RemoveAll having already unlinked metadata before NFS rejects
			// the final rmdir because another descriptor is still open.
			if err := os.Remove(filepath.Join(path, "container.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			return unix.ENOTEMPTY
		}
		return os.RemoveAll(path)
	}
	if err := store.RemoveContainer("demo"); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := os.Stat(containerDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("container remained visible after committed delete: %v", err)
	}
	tombstone := filepath.Join(store.removedContainersRoot(), "demo")
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("failed cleanup did not retain tombstone: %v", err)
	}
	if containers, err := store.ListContainers(); err != nil || len(containers) != 0 {
		t.Fatalf("listing after deferred delete=%+v, err=%v", containers, err)
	}
	allowCleanup = true
	removed, err := store.SweepRemovedContainers()
	if err != nil || removed != 1 {
		t.Fatalf("SweepRemovedContainers=(%d, %v), want (1, nil)", removed, err)
	}
	if _, err := os.Stat(tombstone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retried tombstone remains: %v", err)
	}
}

func TestRemoveContainerRetriesTransientNetworkFilesystemCleanup(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, State: "exited", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	store := Store{Root: root}
	store.removeAllPath = func(path string) error {
		if filepath.Dir(path) == store.removedContainersRoot() {
			attempts++
			if attempts == 1 {
				return unix.ENOTEMPTY
			}
		}
		return os.RemoveAll(path)
	}
	if err := store.RemoveContainer("demo"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("cleanup attempts=%d, want 2", attempts)
	}
	if _, err := os.Stat(filepath.Join(store.removedContainersRoot(), "demo")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient tombstone remains: %v", err)
	}
}

func TestSweepRemovedContainersIsolatesFailures(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	for _, name := range []string{"busy", "free"} {
		path := filepath.Join(store.removedContainersRoot(), name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	store.removeAllPath = func(path string) error {
		if filepath.Base(path) == "busy" {
			return unix.ENOTEMPTY
		}
		return os.RemoveAll(path)
	}
	removed, err := store.SweepRemovedContainers()
	if err == nil || removed != 1 {
		t.Fatalf("SweepRemovedContainers=(%d, %v), want one removal and one isolated error", removed, err)
	}
	if _, err := os.Stat(filepath.Join(store.removedContainersRoot(), "busy")); err != nil {
		t.Fatalf("busy tombstone disappeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.removedContainersRoot(), "free")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("free tombstone was blocked by peer failure: %v", err)
	}
}

func TestPrepareBranchStartBundlePreservesBranchIdentity(t *testing.T) {
	root := t.TempDir()
	transientRoot := t.TempDir()
	store := Store{Root: root, TransientRoot: transientRoot}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	repositories := []specutils.TTCVolumeRepository{{
		Source:   specutils.TTCRootfsUpperSource,
		Path:     filepath.Join(containerDir, "repository"),
		Branch:   ttcvolume.MainBranch,
		CopyMode: string(ttcvolume.CopyModeFull),
	}}
	encoded, err := json.Marshal(repositories)
	if err != nil {
		t.Fatal(err)
	}
	spec := specs.Spec{
		Root:    &specs.Root{Path: "rootfs"},
		Process: &specs.Process{Args: []string{"true"}},
		Annotations: map[string]string{
			specutils.AnnotationTTCVolumeRepositories: string(encoded),
		},
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &spec); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, State: "exited", CreatedAt: time.Now(),
		Branches: []BranchInfo{{ID: "child", ParentID: "main", State: "exited", FilesystemBranch: "fs-child"}},
	}); err != nil {
		t.Fatal(err)
	}
	temp, cleanup, err := store.PrepareBranchStartBundle("demo", "child", "fs-child", true)
	if err != nil {
		t.Fatalf("PrepareBranchStartBundle: %v", err)
	}
	defer cleanup()
	if filepath.Dir(temp) != transientRoot {
		t.Fatalf("branch bundle directory=%q, want parent %q", temp, transientRoot)
	}
	data, err := os.ReadFile(filepath.Join(temp, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got specs.Spec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[specutils.AnnotationTTCInitialBranchID] != "child" || got.Annotations[specutils.AnnotationTTCInitialBranchParentID] != "main" || got.Annotations[specutils.AnnotationTTCInitialFilesystemBranch] != "fs-child" {
		t.Fatalf("initial identity annotations=%q", got.Annotations)
	}
	if got.Annotations[specutils.AnnotationTTCInitialStartPaused] != "true" {
		t.Fatalf("initial start-paused annotation=%q", got.Annotations)
	}
	gotRepositories, err := specutils.TTCVolumeRepositories(&got)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRepositories) != 1 || gotRepositories[0].Branch != "fs-child" {
		t.Fatalf("repositories=%+v", gotRepositories)
	}
	if got.Root == nil || got.Root.Path != filepath.Join(bundleDir, "rootfs") {
		t.Fatalf("root path=%+v", got.Root)
	}

	transient, transientCleanup, err := store.PrepareTransientBranchStartBundle("demo", "child", "fs-child")
	if err != nil {
		t.Fatalf("PrepareTransientBranchStartBundle: %v", err)
	}
	defer transientCleanup()
	if filepath.Dir(transient) != transientRoot {
		t.Fatalf("transient bundle directory=%q, want parent %q", transient, transientRoot)
	}
	data, err = os.ReadFile(filepath.Join(transient, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	got = specs.Spec{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[specutils.AnnotationTTCMemoryFile]; ok {
		t.Fatalf("transient bundle retained obsolete memory-file annotation: %q", got.Annotations[specutils.AnnotationTTCMemoryFile])
	}
	if got.Annotations[specutils.AnnotationTTCInitialStartPaused] != "true" {
		t.Fatalf("transient start-paused annotation=%q", got.Annotations[specutils.AnnotationTTCInitialStartPaused])
	}
}

func TestRemoveFinalCanonicalBranchLeavesReloadableCatalogIdentity(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "family")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	info := ContainerInfo{
		ID: "family", Name: "web", BundleDir: bundleDir, State: "dehydrated", CreatedAt: now,
		Branches: []BranchInfo{{ID: "main", FilesystemBranch: "root-main", State: "dehydrated", StorageState: "shared", CreatedAt: now}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveBranchRecord("family", "main"); err != nil {
		t.Fatal(err)
	}
	anchor, err := store.LoadContainer("web")
	if err != nil {
		t.Fatalf("loading catalog-only family: %v", err)
	}
	if !anchor.DistributedCatalogOnly || len(anchor.Branches) != 0 || anchor.State != "exited" {
		t.Fatalf("catalog-only family = %+v", anchor)
	}
	if err := store.RecordBranch("family", BranchInfo{ID: "next", FilesystemBranch: "root-next", State: "running", StorageState: "local", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	revived, err := store.LoadContainer("family")
	if err != nil {
		t.Fatal(err)
	}
	if revived.DistributedCatalogOnly || len(revived.Branches) != 1 || revived.Branches[0].ID != "next" {
		t.Fatalf("revived family = %+v", revived)
	}
}

func TestStoreBranchLifecycleDoesNotDependOnMain(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["sh"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	info := ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, State: "running", CreatedAt: now,
		Branches: []BranchInfo{
			{ID: "main", State: "running", CreatedAt: now},
			{ID: "child", ParentID: "main", State: "running", CreatedAt: now},
		},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveBranchRecord("demo", "main"); err != nil {
		t.Fatalf("RemoveBranchRecord(main): %v", err)
	}
	if err := store.UpdateBranchState("demo", "child", "dehydrated"); err != nil {
		t.Fatalf("UpdateBranchState(child): %v", err)
	}
	got, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "dehydrated" || len(got.Branches) != 1 || got.Branches[0].ID != "child" || got.Branches[0].ParentID != "" || got.Branches[0].State != "dehydrated" {
		t.Fatalf("after deleting main and dehydrating child: %+v", got)
	}
	if err := store.ReconcileRuntimeUnavailable("demo", RuntimeLoss{}); err != nil {
		t.Fatalf("ReconcileRuntimeUnavailable: %v", err)
	}
	got, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "dehydrated" || got.Branches[0].State != "dehydrated" || got.ExitStatus != nil {
		t.Fatalf("runtime reconciliation corrupted durable child: %+v", got)
	}
	if err := store.UpdateBranchState("demo", "child", "running"); err != nil {
		t.Fatalf("rehydrating child: %v", err)
	}
	if err := store.RecordBranchExit("demo", "main", 137, false); err == nil {
		t.Fatal("recording removed main exit succeeded")
	}
	got, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "running" || got.Branches[0].State != "running" {
		t.Fatalf("removed main exit changed live child: %+v", got)
	}
}

func TestStoreBranchPortBindings(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["true"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, NetworkMode: "ttc", State: "running", CreatedAt: time.Now(),
		Branches: []BranchInfo{{ID: "main", State: "running"}, {
			ID: "child", ParentID: "main", State: "running", StorageState: "publishing",
			PublicationID: "publication-inflight", StorageManifest: "/shared/base-manifest",
		}},
		PublishedPorts: []PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", BranchID: "main"}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.PublishedPorts[0].BranchID; got != "main" {
		t.Fatalf("binding branch=%q, want main", got)
	}
	child := PortBinding{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 80, Protocol: "tcp", BranchID: "child"}
	if err := store.AddPortBinding("demo", child); err != nil {
		t.Fatalf("AddPortBinding: %v", err)
	}
	if err := store.AddPortBinding("demo", PortBinding{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 81, Protocol: "tcp", BranchID: "main"}); err == nil {
		t.Fatal("AddPortBinding accepted a duplicate host endpoint")
	}
	if err := store.RemovePortBinding("demo", child); err != nil {
		t.Fatalf("RemovePortBinding: %v", err)
	}
	if err := store.AddPortBinding("demo", child); err != nil {
		t.Fatalf("re-adding child binding: %v", err)
	}
	if err := store.StopBranch("demo", "child", 137); err != nil {
		t.Fatalf("StopBranch: %v", err)
	}
	stopped, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := stopped.Branches[1]; got.PublicationID != "" || got.StorageManifest != "/shared/base-manifest" {
		t.Fatalf("StopBranch did not fence the publisher while retaining the filesystem ancestor: %+v", got)
	}
	if err := store.mutateContainer("demo", func(info *ContainerInfo) (bool, error) {
		info.Branches[1].PublicationID = "publication-prior-incarnation"
		info.Branches[1].StorageState = "shared"
		info.Branches[1].StorageManifest = "/shared/prior-manifest"
		return true, nil
	}); err != nil {
		t.Fatalf("recording prior publication: %v", err)
	}
	loaded, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.PublishedPorts) != 2 || loaded.Branches[0].State != "running" || loaded.Branches[1].State != "exited" {
		t.Fatalf("branch stop rewrote peers or bindings: branches=%+v bindings=%+v", loaded.Branches, loaded.PublishedPorts)
	}
	if loaded.Branches[1].ExitStatus == nil || *loaded.Branches[1].ExitStatus != 137 || loaded.Branches[1].ExitedAt == nil {
		t.Fatalf("branch stop did not persist branch-local wait status: %+v", loaded.Branches[1])
	}
	if err := store.RecordBranchStart("demo", "child", "node-a"); err != nil {
		t.Fatalf("RecordBranchStart: %v", err)
	}
	loaded, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.PublishedPorts) != 2 || loaded.Branches[0].State != "running" || loaded.Branches[1].State != "running" {
		t.Fatalf("branch start rewrote peers or bindings: branches=%+v bindings=%+v", loaded.Branches, loaded.PublishedPorts)
	}
	if loaded.Branches[1].ExitStatus != nil || loaded.Branches[1].ExitedAt != nil {
		t.Fatalf("branch start retained the previous incarnation's wait status: %+v", loaded.Branches[1])
	}
	if loaded.Branches[1].PublicationID != "" || loaded.Branches[1].StorageState != "local" || loaded.Branches[1].StorageManifest != "/shared/prior-manifest" {
		t.Fatalf("branch start retained a stale publication fence or lost its immutable ancestor: %+v", loaded.Branches[1])
	}
	if loaded.Branches[1].NodeID != "node-a" || loaded.Branches[1].Unplaced {
		t.Fatalf("branch start did not atomically record its execution owner: %+v", loaded.Branches[1])
	}
	if err := store.RecordBranchActivation("demo", "child", "paused", "node-a"); err != nil {
		t.Fatalf("RecordBranchActivation(paused): %v", err)
	}
	loaded, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Branches[1].State != "paused" {
		t.Fatalf("branch activation state=%q, want paused", loaded.Branches[1].State)
	}
	if err := store.RecordBranchActivation("demo", "child", "exited", "node-a"); err == nil {
		t.Fatal("RecordBranchActivation accepted exited state")
	}
	if err := store.RemoveBranchRecord("demo", "child"); err != nil {
		t.Fatalf("RemoveBranchRecord: %v", err)
	}
	loaded, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.PublishedPorts) != 1 || loaded.PublishedPorts[0].Branch() != "main" {
		t.Fatalf("bindings after branch deletion=%+v, want main only", loaded.PublishedPorts)
	}
}

func TestContainerIdentityResolution(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	generatedID, generatedDir, err := store.createContainerDir()
	if err != nil {
		t.Fatalf("createContainerDir: %v", err)
	}
	if len(generatedID) != 2*containerIDBytes {
		t.Fatalf("generated ID length=%d, want %d", len(generatedID), 2*containerIDBytes)
	}
	if _, err := hex.DecodeString(generatedID); err != nil {
		t.Fatalf("generated ID %q is not hexadecimal: %v", generatedID, err)
	}
	if filepath.Base(generatedDir) != generatedID {
		t.Fatalf("generated directory=%q, want ID basename", generatedDir)
	}
	if err := os.RemoveAll(generatedDir); err != nil {
		t.Fatal(err)
	}

	writeRecord := func(key, id, name string) string {
		t.Helper()
		containerDir := filepath.Join(root, "containers", key)
		bundleDir := filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(bundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentContainerFixture(filepath.Join(containerDir, "container.json"), &ContainerInfo{
			ID: id, Name: name, BundleDir: bundleDir, State: "created", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		return containerDir
	}
	idAlpha := "abc1" + strings.Repeat("a", 60)
	idBeta := "abc2" + strings.Repeat("b", 60)
	idNamedPrefix := strings.Repeat("c", 64)
	alphaDir := writeRecord(idAlpha, idAlpha, "alpha")
	betaDir := writeRecord(idBeta, idBeta, "beta")
	writeRecord(idNamedPrefix, idNamedPrefix, "abc")
	nameKeyDir := writeRecord("name-key", "", "name-key")

	for identifier, wantID := range map[string]string{
		"alpha": idAlpha, idAlpha: idAlpha, idAlpha[:4]: idAlpha,
		"abc": idNamedPrefix, "name-key": "name-key", idNamedPrefix: idNamedPrefix, idNamedPrefix[:8]: idNamedPrefix,
	} {
		info, err := store.LoadContainer(identifier)
		if err != nil {
			t.Errorf("LoadContainer(%q): %v", identifier, err)
		} else if info.ID != wantID {
			t.Errorf("LoadContainer(%q).ID=%q, want %q", identifier, info.ID, wantID)
		}
	}
	if _, err := store.LoadContainer("ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous ID prefix error=%v", err)
	}
	if _, err := store.LoadContainer("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing container error=%v, want os.ErrNotExist", err)
	}
	paths, err := store.BranchConsolePaths("alpha", "main")
	if err != nil {
		t.Fatalf("BranchConsolePaths by name: %v", err)
	}
	if !strings.HasPrefix(paths.Log, filepath.Join(alphaDir, "console")+string(filepath.Separator)) {
		t.Errorf("console path=%q, want under ID directory %q", paths.Log, alphaDir)
	}
	status := 9
	if err := store.UpdateState(idAlpha[:4], "exited", &status); err != nil {
		t.Fatalf("UpdateState by prefix: %v", err)
	}
	if info, err := store.LoadContainer("alpha"); err != nil || info.ExitStatus == nil || *info.ExitStatus != status {
		t.Fatalf("updated container=%+v, err=%v", info, err)
	}
	if err := store.RemoveContainer("beta"); err != nil {
		t.Fatalf("RemoveContainer by name: %v", err)
	}
	if _, err := os.Stat(betaDir); !os.IsNotExist(err) {
		t.Errorf("removed ID directory still exists: %v", err)
	}
	if info, err := store.LoadContainer("name-key"); err != nil || info.ID != "name-key" {
		t.Fatalf("name-key record=%+v, err=%v", info, err)
	}
	if _, err := os.Stat(nameKeyDir); err != nil {
		t.Errorf("name-key directory changed: %v", err)
	}
}

func TestImageRootInventoryAndRemoval(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	makeRoot := func(imageDigest, platform string) string {
		t.Helper()
		dir := filepath.Join(root, "images", imageRootKey(imageDigest, platform))
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		manifest := ownership.NewManifest()
		manifest.Entries["."] = ownership.Owner{}
		if err := ownership.Write(ownership.ManifestPath(filepath.Join(dir, "rootfs")), manifest); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	amd64 := makeRoot(digest, "linux/amd64")
	arm64 := makeRoot(digest, "linux/arm64/v8")
	other := makeRoot(otherDigest, "linux/amd64")
	for _, dir := range []string{amd64, arm64} {
		restricted := filepath.Join(dir, "rootfs", "restricted")
		if err := os.Mkdir(restricted, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restricted, "payload"), []byte("x"), 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(restricted, 0550); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(root, "images", imageRootKey(digest, "linux/amd64")+".tmp-stale")
	staleRestricted := filepath.Join(stale, "rootfs", "restricted")
	if err := os.MkdirAll(staleRestricted, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleRestricted, "payload"), []byte("x"), 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staleRestricted, 0550); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleImageRootTemps(filepath.Join(root, "images"), imageRootKey(digest, "linux/amd64")); err != nil {
		t.Fatalf("removeStaleImageRootTemps: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale restrictive image root still exists: %v", err)
	}

	for _, platform := range []string{"linux/amd64", "linux/arm64/v8"} {
		found, err := store.HasImageRoot(digest, platform)
		if err != nil || !found {
			t.Fatalf("HasImageRoot(%q)=(%t, %v), want true", platform, found, err)
		}
	}
	if found, err := store.HasImageRoot(digest, "linux/s390x"); err != nil || found {
		t.Fatalf("HasImageRoot(missing)=(%t, %v), want false", found, err)
	}
	removed, err := store.RemoveImageRoots(digest)
	if err != nil {
		t.Fatalf("RemoveImageRoots: %v", err)
	}
	if removed != 2 {
		t.Errorf("RemoveImageRoots removed %d roots, want 2", removed)
	}
	for _, dir := range []string{amd64, arm64} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("removed image root %q still exists: %v", dir, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated image root was removed: %v", err)
	}
	if _, err := store.RemoveImageRoots(""); err == nil {
		t.Fatal("RemoveImageRoots accepted an empty digest")
	}
}

func TestSharedImagePackRoundTrip(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	store := Store{Root: shared, ActiveRoot: local}
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "usr", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("image payload\n", 100000)
	if err := os.WriteFile(filepath.Join(rootfs, "usr", "bin", "tool"), []byte(payload), 0555); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	manifest.Entries["usr/bin/tool"] = ownership.Owner{UID: 123, GID: 456}
	if err := ownership.Write(ownership.ManifestPath(rootfs), manifest); err != nil {
		t.Fatal(err)
	}
	info := ttcimages.RootFSInfo{
		Reference: "docker.io/library/test:latest", ConfigDigest: "sha256:" + strings.Repeat("a", 64),
		Platform: "linux/amd64", LayerCount: 2,
	}
	key := imageRootKey("sha256:"+strings.Repeat("b", 64), info.Platform)
	if err := store.publishSharedImagePack(key, imageDir, info); err != nil {
		t.Fatalf("publishSharedImagePack: %v", err)
	}
	destination := filepath.Join(local, "images", key)
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	gotInfo, found, err := store.materializeSharedImagePack(key, destination)
	if err != nil || !found {
		t.Fatalf("materializeSharedImagePack = (%+v, %t, %v), want found", gotInfo, found, err)
	}
	if !reflect.DeepEqual(gotInfo, info) {
		t.Fatalf("materialized info = %+v, want %+v", gotInfo, info)
	}
	target := filepath.Join(destination, "rootfs", "usr", "bin", "tool")
	if stat, err := os.Stat(target); err != nil || stat.Size() != 0 {
		t.Fatalf("lazy placeholder size = %v, %v; want zero", stat, err)
	}
	if usage, present, err := store.ImageCacheUsage("sha256:"+strings.Repeat("b", 64), info.Platform); err != nil || !present || usage.CompleteBytes != 0 || usage.TotalBytes != uint64(len(payload)) {
		t.Fatalf("initial image cache usage = (%+v, %t, %v)", usage, present, err)
	}
	descriptor, err := ttcpack.ReadLazyDescriptor(filepath.Join(destination, "rootfs"))
	if err != nil {
		t.Fatal(err)
	}
	lazy, err := ttcpack.OpenLazyDirectory(filepath.Join(destination, "rootfs"), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if size, mode, ok := lazy.Metadata("usr/bin/tool"); !ok || size != int64(len(payload)) || mode&0777 != 0555 {
		t.Fatalf("lazy metadata = (%d, %#o, %t)", size, mode, ok)
	}
	if err := lazy.Ensure("usr/bin/tool"); err != nil {
		t.Fatalf("populating lazy file: %v", err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	if usage, present, err := store.ImageCacheUsage("sha256:"+strings.Repeat("b", 64), info.Platform); err != nil || !present || usage.CompleteBytes != uint64(len(payload)) || usage.TotalBytes != uint64(len(payload)) {
		t.Fatalf("populated image cache usage = (%+v, %t, %v)", usage, present, err)
	}
	// A live gofer pins the whole immutable root. Capacity enforcement must
	// skip it rather than invalidating an already-donated host descriptor.
	lazy, err = ttcpack.OpenLazyDirectory(filepath.Join(destination, "rootfs"), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneImageCache(0)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.EvictedBytes != 0 || pruned.BusyBytes != uint64(len(payload)) || pruned.AfterBytes != uint64(len(payload)) {
		t.Fatalf("active capacity pass = %+v", pruned)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	pruned, err = store.PruneImageCache(0)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.EvictedFiles != 1 || pruned.EvictedBytes != uint64(len(payload)) || pruned.AfterBytes != 0 {
		t.Fatalf("inactive capacity pass = %+v", pruned)
	}
	if stat, err := os.Stat(target); err != nil || stat.Size() != 0 {
		t.Fatalf("evicted image file = %v, %v", stat, err)
	}
	lazy, err = ttcpack.OpenLazyDirectory(filepath.Join(destination, "rootfs"), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := lazy.Ensure("usr/bin/tool"); err != nil {
		t.Fatal(err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != payload {
		t.Fatalf("materialized payload length=%d err=%v", len(got), err)
	}
	owners, err := ownership.Read(ownership.ManifestPath(filepath.Join(destination, "rootfs")))
	if err != nil {
		t.Fatal(err)
	}
	if owner := owners.Entries["usr/bin/tool"]; owner.UID != 123 || owner.GID != 456 {
		t.Fatalf("materialized owner = %+v", owner)
	}
	entries, err := os.ReadDir(filepath.Join(store.sharedImagePackRoot(), "packs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("shared packs = %d, %v, want one", len(entries), err)
	}
}

func TestSharedImagePackPublicationIsIdempotent(t *testing.T) {
	shared := t.TempDir()
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	if err := os.Mkdir(rootfs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "payload"), []byte("same immutable bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	owners := ownership.NewManifest()
	owners.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(rootfs), owners); err != nil {
		t.Fatal(err)
	}
	info := ttcimages.RootFSInfo{ConfigDigest: "sha256:" + strings.Repeat("c", 64), Platform: "linux/amd64"}
	key := imageRootKey("sha256:"+strings.Repeat("d", 64), info.Platform)
	for i := 0; i < 2; i++ {
		store := Store{Root: shared, ActiveRoot: t.TempDir()}
		if err := store.publishSharedImagePack(key, imageDir, info); err != nil {
			t.Fatalf("publication %d: %v", i, err)
		}
	}
	loaded, err := loadSharedImagePackManifest(Store{Root: shared}.sharedImagePackManifestPath(key))
	if err != nil {
		t.Fatal(err)
	}
	id, err := ttcpack.ParseID(loaded.Pack)
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := ttcpack.Open(sharedImagePackRootForTest(shared), id)
	if err != nil {
		t.Fatalf("opening published pack: %v", err)
	}
	file.Close()
}

func sharedImagePackRootForTest(root string) string {
	return Store{Root: root}.sharedImagePackRoot()
}

func TestPruneImageRoots(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	staleDigest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	currentName := imageRootKey(digest, "linux/amd64")
	referencedName := imageRootKey(oldDigest, "linux/amd64")
	staleName := imageRootKey(staleDigest, "linux/amd64")
	tempName := currentName + ".tmp-interrupted"

	makeRoot := func(name string) string {
		t.Helper()
		rootfs := filepath.Join(root, "images", name, "rootfs")
		if err := os.MkdirAll(rootfs, 0700); err != nil {
			t.Fatal(err)
		}
		restricted := filepath.Join(rootfs, "restricted")
		if err := os.Mkdir(restricted, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restricted, "payload"), []byte("x"), 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(restricted, 0550); err != nil {
			t.Fatal(err)
		}
		return rootfs
	}
	currentRoot := makeRoot(currentName)
	referencedRoot := makeRoot(referencedName)
	staleRoot := makeRoot(staleName)
	tempRoot := makeRoot(tempName)
	t.Cleanup(func() {
		_ = removeImageRoot(filepath.Join(root, "images", currentName))
		_ = removeImageRoot(filepath.Join(root, "images", referencedName))
	})
	unrelated := filepath.Join(root, "images", "operator-notes")
	if err := os.MkdirAll(unrelated, 0700); err != nil {
		t.Fatal(err)
	}

	removed, err := store.pruneImageRoots(
		[]ttcimages.Info{{Digest: digest, Platform: "linux/amd64"}},
		[]ContainerInfo{{Name: "referenced", Digest: oldDigest, Platform: "linux/amd64"}},
	)
	if err != nil {
		t.Fatalf("pruneImageRoots: %v", err)
	}
	if removed != 2 {
		t.Fatalf("pruneImageRoots removed %d roots, want 2", removed)
	}
	for _, path := range []string{currentRoot, referencedRoot, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("preserved path %q is unavailable: %v", path, err)
		}
	}
	for _, path := range []string{staleRoot, tempRoot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale path %q remains: %v", path, err)
		}
	}
	if removed, err := store.pruneImageRoots(
		[]ttcimages.Info{{Digest: digest, Platform: "linux/amd64"}},
		[]ContainerInfo{{Name: "referenced", Digest: oldDigest, Platform: "linux/amd64"}},
	); err != nil || removed != 0 {
		t.Errorf("second pruneImageRoots = (%d, %v), want (0, nil)", removed, err)
	}
}

func TestClusterImageCacheKeepsOnlyNodeLocalContainers(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	store := Store{Root: shared, ActiveRoot: local}
	digest := "sha256:" + strings.Repeat("e", 64)
	platform := "linux/amd64"
	key := imageRootKey(digest, platform)
	root := filepath.Join(local, "images", key, "rootfs")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(filepath.Dir(root), "image.json"), &ttcimages.RootFSInfo{ConfigDigest: digest, Platform: platform}); err != nil {
		t.Fatal(err)
	}
	owners := ownership.NewManifest()
	owners.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(root), owners); err != nil {
		t.Fatal(err)
	}
	containers := []ContainerInfo{{ID: "remote", Digest: digest, Platform: platform}}
	if removed, err := store.pruneImageRoots([]ttcimages.Info{{Digest: digest, Platform: platform}}, containers); err != nil || removed != 0 {
		t.Fatalf("prune newly-created remote-only cache = (%d, %v), want (0, nil)", removed, err)
	}
	old := time.Now().Add(-2 * nodeLocalImageCacheGrace)
	if err := os.Chtimes(filepath.Dir(root), old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.pruneImageRoots([]ttcimages.Info{{Digest: digest, Platform: platform}}, containers); err != nil || removed != 1 {
		t.Fatalf("prune remote-only cache = (%d, %v), want (1, nil)", removed, err)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(filepath.Dir(root), "image.json"), &ttcimages.RootFSInfo{ConfigDigest: digest, Platform: platform}); err != nil {
		t.Fatal(err)
	}
	if err := ownership.Write(ownership.ManifestPath(root), owners); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.activeContainerDir("local"), 0700); err != nil {
		t.Fatal(err)
	}
	containers = []ContainerInfo{{ID: "local", Digest: digest, Platform: platform}}
	if removed, err := store.pruneImageRoots(nil, containers); err != nil || removed != 0 {
		t.Fatalf("prune locally-used cache = (%d, %v), want (0, nil)", removed, err)
	}
}

// A container about to start on a node from an image root the node unpacked
// longer ago than the grace, which no container on the node runs from yet,
// keeps it from the pruning that comes before the container has state there.
func TestStartingContainerKeepsTheNodesOldImageRoot(t *testing.T) {
	shared := t.TempDir()
	store := Store{Root: shared, ActiveRoot: t.TempDir()}
	info := ContainerInfo{
		ID: "web-id", Name: "web", Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64",
		BundleDir: filepath.Join(shared, "containers", "web-id", "bundle"),
	}
	if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", info.ID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	imageRoot := filepath.Join(store.imageRootDir(), imageRootKey(info.Digest, info.Platform))
	unpack := func() {
		t.Helper()
		rootfs := filepath.Join(imageRoot, "rootfs")
		if err := os.MkdirAll(rootfs, 0755); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(imageRoot, "image.json"), &ttcimages.RootFSInfo{ConfigDigest: info.Digest, Platform: info.Platform}); err != nil {
			t.Fatal(err)
		}
		owners := ownership.NewManifest()
		owners.Entries["."] = ownership.Owner{}
		if err := ownership.Write(ownership.ManifestPath(rootfs), owners); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * nodeLocalImageCacheGrace)
		if err := os.Chtimes(imageRoot, old, old); err != nil {
			t.Fatal(err)
		}
	}
	unpack()
	if removed, err := store.PruneImageRoots(context.Background()); err != nil || removed != 1 {
		t.Fatalf("PruneImageRoots of an old root no container runs from = (%d, %v), want (1, nil)", removed, err)
	}
	unpack()
	if ready, err := store.KeepContainerImageRoot(info.ID); err != nil || !ready {
		t.Fatalf("KeepContainerImageRoot = (%t, %v), want (true, nil)", ready, err)
	}
	if removed, err := store.PruneImageRoots(context.Background()); err != nil || removed != 0 {
		t.Fatalf("PruneImageRoots of the root a container is starting from = (%d, %v), want (0, nil)", removed, err)
	}
	if _, err := loadImageRoot(imageRoot); err != nil {
		t.Fatalf("the root a container is starting from: %v", err)
	}
}

// An inventory of a node's filesystem origins passes over a repository
// removed while it had it open, as a removed family's cleanup on the node
// removes its repositories, rather than failing the removal it is part of.
func TestOriginInventoryPassesOverARepositoryRemovedMeanwhile(t *testing.T) {
	local := t.TempDir()
	store := Store{Root: t.TempDir(), ActiveRoot: local}
	manager, err := ttcvolume.OpenManager(local)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("removed")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	afterOriginInventoryOpen = func(path string) {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	}
	defer func() { afterOriginInventoryOpen = nil }()
	if _, err := store.FilesystemSnapshotOrigins(); err != nil {
		t.Fatalf("inventory with a repository removed meanwhile: %v", err)
	}
}

func TestPruneUnreferencedSharedImagePack(t *testing.T) {
	shared := t.TempDir()
	store := Store{Root: shared, ActiveRoot: t.TempDir()}
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	if err := os.Mkdir(rootfs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "payload"), []byte("unused image"), 0644); err != nil {
		t.Fatal(err)
	}
	owners := ownership.NewManifest()
	owners.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(rootfs), owners); err != nil {
		t.Fatal(err)
	}
	info := ttcimages.RootFSInfo{ConfigDigest: "sha256:" + strings.Repeat("f", 64), Platform: "linux/amd64"}
	key := imageRootKey("sha256:"+strings.Repeat("1", 64), info.Platform)
	if err := store.publishSharedImagePack(key, imageDir, info); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadSharedImagePackManifest(store.sharedImagePackManifestPath(key))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ttcpack.ParseID(manifest.Pack)
	// No tag names the image yet, as when another node loads it and has yet
	// to publish its tags: the pack is kept for a grace period.
	manifests, packs, err := store.PruneSharedImagePacks(context.Background())
	if err != nil || manifests != 0 || packs != 0 {
		t.Fatalf("PruneSharedImagePacks of a young pack = (%d, %d, %v), want (0, 0, nil)", manifests, packs, err)
	}
	if _, err := os.Stat(ttcpack.Path(store.sharedImagePackRoot(), id)); err != nil {
		t.Fatalf("a young pack after a prune: %v", err)
	}
	old := time.Now().Add(-2 * sharedImagePackGrace)
	if err := os.Chtimes(store.sharedImagePackManifestPath(key), old, old); err != nil {
		t.Fatal(err)
	}
	manifests, packs, err = store.PruneSharedImagePacks(context.Background())
	if err != nil || manifests != 1 || packs != 1 {
		t.Fatalf("PruneSharedImagePacks of an old pack = (%d, %d, %v), want (1, 1, nil)", manifests, packs, err)
	}
	if _, err := os.Stat(ttcpack.Path(store.sharedImagePackRoot(), id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced old pack remains after a prune: %v", err)
	}
	// The removal of an image prunes its pack at once, however young.
	if err := store.publishSharedImagePack(key, imageDir, info); err != nil {
		t.Fatal(err)
	}
	manifests, packs, err = store.PruneSharedImagePacks(context.Background(), "sha256:"+strings.Repeat("2", 64))
	if err != nil || manifests != 0 || packs != 0 {
		t.Fatalf("PruneSharedImagePacks after another image's removal = (%d, %d, %v), want (0, 0, nil)", manifests, packs, err)
	}
	manifests, packs, err = store.PruneSharedImagePacks(context.Background(), "sha256:"+strings.Repeat("1", 64))
	if err != nil || manifests != 1 || packs != 1 {
		t.Fatalf("PruneSharedImagePacks after the image's removal = (%d, %d, %v), want (1, 1, nil)", manifests, packs, err)
	}
	if _, err := os.Stat(ttcpack.Path(store.sharedImagePackRoot(), id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a removed image's pack remains after a prune: %v", err)
	}
}

func TestGarbageCollectRepositoriesIncludesNamedVolumes(t *testing.T) {
	root := t.TempDir()
	manager, err := ttcvolume.OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("gc-volume")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "data", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, []byte("main"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fork(ttcvolume.MainBranch, "discarded"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt("discarded", ino, []byte("child"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch("discarded"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	countObjects := func() int {
		count := 0
		volumes := filepath.Join(root, "filesystems", "volumes")
		if err := filepath.WalkDir(volumes, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.Contains(path, string(filepath.Separator)+"data"+string(filepath.Separator)+"files"+string(filepath.Separator)) && strings.HasSuffix(path, ".data") {
				count++
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countObjects()
	collected, err := (Store{Root: root}).GarbageCollectRepositories()
	if err != nil {
		t.Fatal(err)
	}
	after := countObjects()
	if collected != 1 || before < 2 || after >= before {
		t.Fatalf("collected=%d objects before=%d after=%d", collected, before, after)
	}
}

func TestGarbageCollectRepositoriesSkipsUnmaterializedPrivateRepository(t *testing.T) {
	sharedRoot := t.TempDir()
	localRoot := t.TempDir()
	containerID := strings.Repeat("a", 64)
	bundleDir := filepath.Join(sharedRoot, "containers", containerID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	repositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
		Source:   specutils.TTCRootfsUpperSource,
		Path:     filepath.Join(localRoot, "containers", containerID, "rootfs-upper"),
		Branch:   ttcvolume.MainBranch,
		CopyMode: string(ttcvolume.CopyModeFull),
		Private:  true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{Annotations: map[string]string{
		specutils.AnnotationTTCVolumeRepositories: string(repositories),
	}}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{
		ID: containerID, Name: "remote-only", BundleDir: bundleDir, State: "running",
		Branches: []BranchInfo{{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: "running", NodeID: "other-node"}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(sharedRoot, "containers", containerID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}

	collected, err := (Store{Root: sharedRoot, ActiveRoot: localRoot}).GarbageCollectRepositories()
	if err != nil {
		t.Fatalf("GarbageCollectRepositories: %v", err)
	}
	if collected != 0 {
		t.Fatalf("collected=%d, want 0 for an unmaterialized remote repository", collected)
	}
}

func TestFilesystemSnapshotOriginsIncludesNamedAndPrivateRepositories(t *testing.T) {
	sharedRoot := t.TempDir()
	localRoot := t.TempDir()
	manager, err := ttcvolume.OpenManager(localRoot)
	if err != nil {
		t.Fatal(err)
	}
	named, err := manager.CreateWithOrigin("data", 41)
	if err != nil {
		t.Fatal(err)
	}
	if err := named.Close(); err != nil {
		t.Fatal(err)
	}
	containerID := strings.Repeat("b", 64)
	privatePath := filepath.Join(localRoot, "containers", containerID, "rootfs-upper")
	if err := os.MkdirAll(filepath.Dir(privatePath), 0700); err != nil {
		t.Fatal(err)
	}
	private, err := manager.CreatePrivateWithOrigin(privatePath, "rootfs", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := private.Close(); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(sharedRoot, "containers", containerID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	repositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: privatePath, Branch: ttcvolume.MainBranch,
		CopyMode: string(ttcvolume.CopyModeFull), Private: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{Annotations: map[string]string{
		specutils.AnnotationTTCVolumeRepositories: string(repositories),
	}}); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{ID: containerID, Name: "inventory", BundleDir: bundleDir, Branches: []BranchInfo{{ID: "main"}}}
	if err := writeCurrentContainerFixture(filepath.Join(sharedRoot, "containers", containerID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: sharedRoot, ActiveRoot: localRoot}
	origins, err := store.FilesystemSnapshotOrigins()
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []ttcsnapshot.SnapshotID{41, 42} {
		if _, ok := origins[origin]; !ok {
			t.Errorf("origin inventory does not contain %d: %v", origin, origins)
		}
	}
	if err := manager.RemovePrivate(privatePath); err != nil {
		t.Fatal(err)
	}
	origins, err = store.FilesystemSnapshotOrigins()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := origins[42]; ok {
		t.Errorf("removed private repository origin remains in inventory: %v", origins)
	}
	if _, ok := origins[41]; !ok {
		t.Errorf("named-volume origin disappeared with private repository: %v", origins)
	}
}

// A container's repositories, which its spec fixes when it is made, are read
// once: its record and spec are not read again, its repository map is.
func TestFilesystemSnapshotOriginsReadsAContainersSpecOnce(t *testing.T) {
	sharedRoot := t.TempDir()
	localRoot := t.TempDir()
	manager, err := ttcvolume.OpenManager(localRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Root: sharedRoot, ActiveRoot: localRoot}
	// container makes a container whose rootfs upper is a repository of
	// origin, and returns its ID.
	container := func(digit string, origin ttcsnapshot.SnapshotID) string {
		t.Helper()
		id := strings.Repeat(digit, 64)
		path := filepath.Join(localRoot, "containers", id, rootfsUpperRepositoryDir)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		repository, err := manager.CreatePrivateWithOrigin(path, "rootfs", origin)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		bundleDir := filepath.Join(sharedRoot, "containers", id, "bundle")
		if err := os.MkdirAll(bundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		repositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
			Source: specutils.TTCRootfsUpperSource, Path: "/elsewhere", Branch: ttcvolume.MainBranch,
			CopyMode: string(ttcvolume.CopyModeFull), Private: true,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{Annotations: map[string]string{
			specutils.AnnotationTTCVolumeRepositories: string(repositories),
		}}); err != nil {
			t.Fatal(err)
		}
		info := ContainerInfo{ID: id, Name: "c" + digit, BundleDir: bundleDir, Branches: []BranchInfo{{ID: "main"}}}
		if err := writeCurrentContainerFixture(filepath.Join(sharedRoot, "containers", id, "container.json"), &info); err != nil {
			t.Fatal(err)
		}
		return id
	}
	want := func(listed ...ttcsnapshot.SnapshotID) {
		t.Helper()
		origins, err := store.FilesystemSnapshotOrigins()
		if err != nil {
			t.Fatal(err)
		}
		if len(origins) != len(listed) {
			t.Fatalf("origins %v, want %v", origins, listed)
		}
		for _, origin := range listed {
			if _, ok := origins[origin]; !ok {
				t.Fatalf("origins %v, want %v", origins, listed)
			}
		}
	}
	first := container("c", 51)
	want(51)
	// Neither the record nor the spec is read again, and the repositories
	// are those of the node-local state, while it is there.
	if err := os.WriteFile(filepath.Join(sharedRoot, "containers", first, "bundle", "config.json"), []byte("not a spec"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sharedRoot, "containers", first, "container.json")); err != nil {
		t.Fatal(err)
	}
	want(51)
	// The repository map is read each time.
	mapped := filepath.Join(localRoot, "containers", first, "restored")
	repository, err := manager.CreatePrivateWithOrigin(mapped, "rootfs", 52)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(localRoot, "containers", first, "repository-map.json"), map[string]activeRepositoryMapping{
		specutils.TTCRootfsUpperSource: {Path: mapped},
	}); err != nil {
		t.Fatal(err)
	}
	want(52)
	// A container made since is read; one whose state is gone is not listed.
	container("d", 53)
	want(52, 53)
	if err := os.RemoveAll(filepath.Join(localRoot, "containers", first)); err != nil {
		t.Fatal(err)
	}
	want(53)
}

func TestFrozenFilesystemOriginsListsEveryRepositorysFreezes(t *testing.T) {
	localRoot := t.TempDir()
	manager, err := ttcvolume.OpenManager(localRoot)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := manager.CreateWithOrigin("idle", 71)
	if err != nil {
		t.Fatal(err)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	handing, err := manager.CreateWithOrigin("handing", 72)
	if err != nil {
		t.Fatal(err)
	}
	ino, err := handing.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "payload", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := handing.WriteAt(ttcvolume.MainBranch, ino, []byte("payload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := handing.ForkWithOrigins(ttcvolume.MainBranch, "peer", 73, 74); err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := handing.PublishOriginHandoff(context.Background(), shared, 72); err != nil {
		t.Fatal(err)
	}
	if _, err := handing.FreezeOriginHandoff(72); err != nil {
		t.Fatal(err)
	}
	if err := handing.Close(); err != nil {
		t.Fatal(err)
	}

	store := Store{Root: t.TempDir(), ActiveRoot: localRoot}
	frozen, err := store.FrozenFilesystemOrigins()
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 1 || frozen[0] != 72 {
		t.Fatalf("frozen filesystem origins = %v, want [72]", frozen)
	}
	if _, err := store.WithFilesystemSnapshotOrigin(72, func(repository *ttcvolume.Repository) error {
		return repository.CompleteOriginHandoff(72)
	}); err != nil {
		t.Fatal(err)
	}
	if frozen, err := store.FrozenFilesystemOrigins(); err != nil || len(frozen) != 0 {
		t.Fatalf("frozen filesystem origins after completion = %v, %v", frozen, err)
	}
}

func TestWithFilesystemSnapshotOriginFindsUniqueRepository(t *testing.T) {
	localRoot := t.TempDir()
	manager, err := ttcvolume.OpenManager(localRoot)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.CreateWithOrigin("first", 71)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := manager.CreateWithOrigin("second", 72)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	store := Store{Root: t.TempDir(), ActiveRoot: localRoot}
	visited := 0
	found, err := store.WithFilesystemSnapshotOrigin(72, func(repository *ttcvolume.Repository) error {
		visited++
		origins := repository.SnapshotOrigins()
		if !slices.Contains(origins, ttcsnapshot.SnapshotID(72)) || slices.Contains(origins, ttcsnapshot.SnapshotID(71)) {
			t.Fatalf("visited wrong repository origins: %v", origins)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found || visited != 1 {
		t.Fatalf("found=%t visited=%d, want true/1", found, visited)
	}
	found, err = store.WithFilesystemSnapshotOrigin(73, func(*ttcvolume.Repository) error {
		t.Fatal("visited repository for absent origin")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("reported absent origin as found")
	}
}

func TestRemoveBranchFilesystemsDeletesFinalDistributedVolumeMaterialization(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	manager, err := ttcvolume.OpenManager(local)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := manager.List()
	if err != nil || len(volumes) != 1 {
		t.Fatalf("listing volume: entries=%+v err=%v", volumes, err)
	}
	physicalBranch := ttcvolume.ScopedBranchID("logical-family", "migration")
	if err := repository.Fork(ttcvolume.MainBranch, physicalBranch); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch(ttcvolume.MainBranch); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(local, "filesystems", "volumes", volumes[0].ID)
	containerID := "fork-physical"
	bundleDir := filepath.Join(shared, "containers", containerID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	encodedRepositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
		Source: "/run/ttc/volumes/" + volumes[0].ID, Path: repositoryPath,
		Branch: physicalBranch, CopyMode: string(ttcvolume.CopyModeFull),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{Annotations: map[string]string{
		specutils.AnnotationTTCVolumeRepositories: string(encodedRepositories),
	}}); err != nil {
		t.Fatal(err)
	}
	branch := BranchInfo{ID: "migration", FilesystemBranch: physicalBranch, State: "running"}
	info := ContainerInfo{
		ID: containerID, Name: containerID, BundleDir: bundleDir,
		// Completed migrations clear their transaction ID while retaining the
		// physical-to-logical family identity. Final-local-branch cleanup must
		// continue treating this repository as a disposable materialization.
		DistributedFamilyID: "logical-family",
		Branches:            []BranchInfo{branch},
	}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", containerID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := (Store{Root: shared, ActiveRoot: local}).RemoveBranchFilesystems(containerID, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repositoryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete distributed volume repository still exists: %v", err)
	}
}

// The runtime removes a branch of a family-private repository before the
// daemon cleans up after it. The daemon learns from the branch pointers what
// is left to do, without opening the repository: here its graph is unreadable.
func TestRemoveBranchFilesystemsReadsBranchPointers(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	manager, err := ttcvolume.OpenManager(local)
	if err != nil {
		t.Fatal(err)
	}
	containerID := "pointers"
	path := filepath.Join(local, "containers", containerID, "rootfs-upper")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreatePrivate(path, "rootfs-upper")
	if err != nil {
		t.Fatal(err)
	}
	scoped := ttcvolume.ScopedBranchID(containerID, "gone")
	if err := repository.Fork(ttcvolume.MainBranch, scoped); err != nil {
		t.Fatal(err)
	}
	// What the runtime's removal of the branch leaves.
	if err := repository.RemoveBranch(scoped); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "graph", "graph.head"), []byte("unreadable"), 0600); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(shared, "containers", containerID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	encodedRepositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: path, Branch: ttcvolume.MainBranch,
		CopyMode: string(ttcvolume.CopyModeFull), Private: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{Annotations: map[string]string{
		specutils.AnnotationTTCVolumeRepositories: string(encodedRepositories),
	}}); err != nil {
		t.Fatal(err)
	}
	gone := BranchInfo{ID: "gone", FilesystemBranch: scoped, State: "exited"}
	main := BranchInfo{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: "exited"}
	info := ContainerInfo{ID: containerID, Name: containerID, BundleDir: bundleDir, Branches: []BranchInfo{main, gone}}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", containerID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: shared, ActiveRoot: local}
	if err := store.RemoveBranchFilesystems(containerID, gone); err != nil {
		t.Fatalf("removing a branch the runtime removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "branches", ttcvolume.MainBranch+".json")); err != nil {
		t.Fatalf("the remaining branch: %v", err)
	}
	if err := store.RemoveBranchFilesystems(containerID, main); err != nil {
		t.Fatalf("removing the last branch: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repository of no branch remains: %v", err)
	}
}

func TestRemoveBranchFilesystemsAcceptsAlreadyReclaimedActiveRepository(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	containerID := "retired-physical"
	bundleDir := filepath.Join(shared, "containers", containerID, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(local, "containers", containerID, "rootfs-upper")
	encodedRepositories, err := json.Marshal([]specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: missing, Branch: ttcvolume.MainBranch,
		CopyMode: string(ttcvolume.CopyModeFull), Private: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{Annotations: map[string]string{
		specutils.AnnotationTTCVolumeRepositories: string(encodedRepositories),
	}}); err != nil {
		t.Fatal(err)
	}
	branch := BranchInfo{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: "exited"}
	info := ContainerInfo{ID: containerID, Name: containerID, BundleDir: bundleDir, Branches: []BranchInfo{branch}}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", containerID, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := (Store{Root: shared, ActiveRoot: local}).RemoveBranchFilesystems(containerID, branch); err != nil {
		t.Fatalf("already reclaimed active repository: %v", err)
	}
}

func TestBuildSpecManagedCoWHostMount(t *testing.T) {
	host := t.TempDir()
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	spec, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		HostMounts: []HostMount{{
			Source: host, Destination: "/data", ImmutableSource: true, ManagedCoW: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	mount := spec.Mounts[len(spec.Mounts)-1]
	if got, want := mount.Options, []string{"rbind", "rw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("managed CoW mount options=%q, want %q", got, want)
	}
	if got, want := spec.Annotations[ImmutableHostMountsAnnotation], `["/data"]`; got != want {
		t.Errorf("immutable annotation=%q, want %q", got, want)
	}
	if got, want := spec.Annotations[ManagedCoWHostMountsAnnotation], `["/data"]`; got != want {
		t.Errorf("managed CoW annotation=%q, want %q", got, want)
	}
	for _, invalid := range []HostMount{
		{Source: host, Destination: "/data", ManagedCoW: true},
		{
			Source: host, Destination: "/data", Readonly: true, ManagedCoW: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		},
	} {
		if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{invalid}}); err == nil {
			t.Errorf("BuildSpec accepted invalid managed CoW mount %+v", invalid)
		}
	}
}

// A direct host mount binds the host path itself, writable, with no TTC
// repository, and the annotation that keeps runsc from overlaying it.
func TestBuildSpecDirectHostMount(t *testing.T) {
	host := t.TempDir()
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	spec, err := BuildSpec(image, SpecOptions{
		RootFS:     t.TempDir(),
		HostMounts: []HostMount{{Source: host, Destination: "/data", Direct: true}},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	mount := spec.Mounts[len(spec.Mounts)-1]
	if mount.Type != "bind" || mount.Source != host || mount.Destination != "/data" || !reflect.DeepEqual(mount.Options, []string{"rbind", "rw"}) {
		t.Errorf("direct mount=%+v", mount)
	}
	direct, err := specutils.TTCDirectHostMounts(spec)
	if err != nil || len(direct) != 1 || !direct["/data"] {
		t.Errorf("direct host mounts=%v, %v", direct, err)
	}
	if repositories, err := specutils.TTCVolumeRepositories(spec); err != nil || len(repositories) != 0 {
		t.Errorf("direct mount's repositories=%+v, %v", repositories, err)
	}
	for _, invalid := range []HostMount{
		{Source: host, Destination: "/data"},
		{Source: host, Destination: "/data", Direct: true, Readonly: true},
		{
			Source: host, Destination: "/data", Direct: true, ManagedCoW: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		},
	} {
		if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{invalid}}); err == nil {
			t.Errorf("BuildSpec accepted invalid host mount %+v", invalid)
		}
	}
}

func TestBuildSpecCopiedHostMount(t *testing.T) {
	host := t.TempDir()
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}, SpecOptions{
		RootFS: t.TempDir(),
		HostMounts: []HostMount{{
			Source: host, Destination: "/data", Copy: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount := spec.Mounts[len(spec.Mounts)-1]
	if mount.Type != "ttcvol" || mount.Destination != "/data" || !reflect.DeepEqual(mount.Options, []string{"rw"}) {
		t.Fatalf("copied mount=%+v", mount)
	}
	if got, want := spec.Annotations[CopiedHostMountsAnnotation], `{"`+"/data"+`":"`+host+`"}`; got != want {
		t.Errorf("copied annotation=%q, want %q", got, want)
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || !repositories[0].Private || repositories[0].OverlayDestination != "" {
		t.Errorf("copied repositories=%+v", repositories)
	}
}

func TestCopyRestoreHostDirectory(t *testing.T) {
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
	destination := filepath.Join(t.TempDir(), "restored")
	if err := copyRestoreHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("restored data=%q", data)
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "nested/data" {
		t.Fatalf("restored symlink=%q", target)
	}
}

func TestRepositoryCanonicalBranchBelongsToFamily(t *testing.T) {
	tests := []struct {
		name   string
		config specutils.TTCVolumeRepository
		want   bool
	}{
		{
			name:   "rootfs upper",
			config: specutils.TTCVolumeRepository{Source: specutils.TTCRootfsUpperSource},
			want:   true,
		},
		{
			name:   "copied host mount",
			config: specutils.TTCVolumeRepository{Source: "/run/ttc/copied-host/0", Private: true},
			want:   true,
		},
		{
			name:   "named volume",
			config: specutils.TTCVolumeRepository{Source: "/run/ttc/volumes/data"},
			want:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := repositoryCanonicalBranchBelongsToFamily(test.config); got != test.want {
				t.Fatalf("repositoryCanonicalBranchBelongsToFamily(%+v)=%t, want %t", test.config, got, test.want)
			}
		})
	}
}

func TestLoadSpecResolvesNodeLocalImageAndRepositoryPaths(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	store := Store{Root: shared, ActiveRoot: local}
	id := strings.Repeat("a", 64)
	bundleDir := filepath.Join(shared, "containers", id, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	repositories := []specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: filepath.Join(shared, "old-upper"),
		Branch: ttcvolume.MainBranch, CopyMode: string(ttcvolume.CopyModeFull), Private: true,
	}}
	encoded, err := json.Marshal(repositories)
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{
		Root: &specs.Root{Path: filepath.Join(shared, "images", "source", "rootfs")},
		Annotations: map[string]string{
			specutils.AnnotationTTCVolumeRepositories: string(encoded),
			ttcpack.LazyAnnotation:                    filepath.Join(shared, "images", "source", ttcpack.LazyDescriptorName),
			ownership.Annotation:                      filepath.Join(shared, "images", "source", ownership.ManifestName),
			"dev.gvisor.spec.rootfs.source":           filepath.Join(shared, "images", "source", "rootfs"),
		},
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		t.Fatal(err)
	}
	// The old node's path may still be visible to this process. It must not be
	// selected in preference to the current node's local active root.
	if err := os.MkdirAll(repositories[0].Path, 0700); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	info := ContainerInfo{
		ID: id, Name: "demo", BundleDir: bundleDir, RootFS: spec.Root.Path, Digest: digest, Platform: "linux/amd64",
		State: "created", LiveForkEligible: true, Branches: []BranchInfo{{ID: "main", FilesystemBranch: ttcvolume.MainBranch, State: "created"}},
	}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", id, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Annotations[ttcpack.LazyAnnotation]; got != "" {
		t.Fatalf("stale lazy image annotation = %q, want empty", got)
	}
	wantRoot := filepath.Join(local, "images", imageRootKey(digest, "linux/amd64"), "rootfs")
	if err := os.MkdirAll(wantRoot, 0700); err != nil {
		t.Fatal(err)
	}
	packID, err := ttcpack.ParseID(strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := ttcpack.WriteLazyDescriptor(wantRoot, filepath.Join(shared, "image-packs"), packID, "rootfs"); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Root == nil || loaded.Root.Path != wantRoot {
		t.Fatalf("root path = %+v, want %q", loaded.Root, wantRoot)
	}
	if got, want := loaded.Annotations["dev.gvisor.spec.rootfs.source"], wantRoot; got != want {
		t.Fatalf("rootfs source annotation = %q, want %q", got, want)
	}
	if got, want := loaded.Annotations[ownership.Annotation], ownership.ManifestPath(wantRoot); got != want {
		t.Fatalf("ownership annotation = %q, want %q", got, want)
	}
	if got, want := loaded.Annotations[ttcpack.LazyAnnotation], ttcpack.LazyDescriptorPath(wantRoot); got != want {
		t.Fatalf("lazy image annotation = %q, want %q", got, want)
	}
	resolved, err := specutils.TTCVolumeRepositories(loaded)
	if err != nil {
		t.Fatal(err)
	}
	wantRepository := filepath.Join(local, "containers", id, rootfsUpperRepositoryDir)
	if len(resolved) != 1 || resolved[0].Path != wantRepository {
		t.Fatalf("repositories = %+v, want path %q", resolved, wantRepository)
	}
}

func TestCommitActiveRepositoryRestoreIDsAdvancesOneShotAlias(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	store := Store{Root: shared, ActiveRoot: local}
	id := strings.Repeat("c", 64)
	bundleDir := filepath.Join(shared, "containers", id, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := ttcvolume.OpenManager(local)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(local, "containers", id, rootfsUpperRepositoryDir)
	if err := os.MkdirAll(filepath.Dir(repositoryPath), 0700); err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreatePrivate(repositoryPath, "target-rootfs")
	if err != nil {
		t.Fatal(err)
	}
	localID := repository.Info().ID
	copyMode := string(manager.Capability().Mode)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repositories := []specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: "/old-node/rootfs-upper",
		Branch: ttcvolume.MainBranch, CopyMode: copyMode, RestoreID: strings.Repeat("d", 32), Private: true,
	}}
	encoded, err := json.Marshal(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), &specs.Spec{
		Annotations: map[string]string{specutils.AnnotationTTCVolumeRepositories: string(encoded)},
	}); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{ID: id, Name: "demo", BundleDir: bundleDir, State: "paused"}
	if err := writeCurrentContainerFixture(filepath.Join(shared, "containers", id, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteActiveRepositoryMap(id, []specutils.TTCVolumeRepository{{
		Source: specutils.TTCRootfsUpperSource, Path: repositoryPath, CopyMode: copyMode,
		RestoreID: strings.Repeat("d", 32),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitActiveRepositoryRestoreIDs(id); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := specutils.TTCVolumeRepositories(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != repositoryPath || got[0].RestoreID != localID {
		t.Fatalf("repositories=%+v, want path %q and restore ID %q", got, repositoryPath, localID)
	}
}

// A memory limit brings Docker's swap limit with it: twice the memory unless
// --memory-swap says otherwise, -1 for unlimited swap, never less than the
// memory, and never without a memory limit.
func TestMemorySwapFollowsDocker(t *testing.T) {
	const mib = int64(1) << 20
	for value, want := range map[string]int64{"-1": -1, "0": 0, "256m": 256 * mib} {
		if got, err := ParseMemorySwap(value); err != nil || got != want {
			t.Errorf("ParseMemorySwap(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"-2", "lots"} {
		if _, err := ParseMemorySwap(value); err == nil {
			t.Errorf("ParseMemorySwap(%q) succeeded", value)
		}
	}
	for _, test := range []struct{ memory, swap, want int64 }{
		{64 * mib, 0, 128 * mib}, {64 * mib, -1, -1}, {64 * mib, 100 * mib, 100 * mib}, {0, 0, 0},
	} {
		if got := EffectiveMemorySwap(test.memory, test.swap); got != test.want {
			t.Errorf("EffectiveMemorySwap(%d, %d) = %d, want %d", test.memory, test.swap, got, test.want)
		}
	}
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	memory := 64 * mib
	for _, swap := range []int64{128 * mib, 64 * mib} {
		spec, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), MemoryLimit: &memory, MemorySwap: &swap})
		if err != nil {
			t.Fatalf("BuildSpec with swap %d: %v", swap, err)
		}
		if got := spec.Linux.Resources.Memory.Swap; got == nil || *got != swap {
			t.Errorf("spec swap = %v, want %d", got, swap)
		}
	}
	// Unlimited swap, asked for or recorded before swap limits, sets none.
	unlimited := int64(-1)
	for _, swap := range []*int64{&unlimited, nil} {
		spec, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), MemoryLimit: &memory, MemorySwap: swap})
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.Linux.Resources.Memory.Swap; got != nil {
			t.Errorf("unlimited swap set a limit: %d", *got)
		}
	}
	short, zero := 32*mib, int64(0)
	for name, opts := range map[string]SpecOptions{
		"less than memory": {MemoryLimit: &memory, MemorySwap: &short},
		"without memory":   {MemorySwap: &short},
		"zero memory":      {MemoryLimit: &zero, MemorySwap: &short},
		"below unlimited":  {MemoryLimit: &memory, MemorySwap: func() *int64 { v := int64(-2); return &v }()},
	} {
		opts.RootFS = t.TempDir()
		if _, err := BuildSpec(image, opts); err == nil {
			t.Errorf("BuildSpec accepted a memory-swap limit %s", name)
		}
	}
}

func TestAddDefaultEnv(t *testing.T) {
	env := []string{"PATH=/bin", "VK_LOADER_DRIVERS_DISABLE=mine"}
	got := addDefaultEnv(env, []string{"VK_LOADER_DRIVERS_DISABLE=virtio_icd*", "MESA_DEBUG=silent"})
	want := []string{"PATH=/bin", "VK_LOADER_DRIVERS_DISABLE=mine", "MESA_DEBUG=silent"}
	if !slices.Equal(got, want) {
		t.Errorf("addDefaultEnv = %q, want %q", got, want)
	}
	if got := addDefaultEnv([]string{"PATH=/bin"}, nil); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Errorf("addDefaultEnv without defaults = %q", got)
	}
}

// A root that a container is being made of is not pruned though no image or
// container record names it yet.
func TestPruneImageRootsPassesOverARootInUse(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	name := imageRootKey("sha256:"+strings.Repeat("d", 64), "linux/amd64")
	dir := filepath.Join(root, "images", name)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	imageRootsInUse.mu.Lock()
	if imageRootsInUse.count == nil {
		imageRootsInUse.count = make(map[string]int)
	}
	imageRootsInUse.count[dir]++
	imageRootsInUse.mu.Unlock()
	if removed, err := store.pruneImageRoots(nil, nil); err != nil || removed != 0 {
		t.Fatalf("pruneImageRoots with the root in use = (%d, %v), want (0, nil)", removed, err)
	}
	imageRootsInUse.mu.Lock()
	delete(imageRootsInUse.count, dir)
	imageRootsInUse.mu.Unlock()
	if removed, err := store.pruneImageRoots(nil, nil); err != nil || removed != 1 {
		t.Fatalf("pruneImageRoots once the root is not in use = (%d, %v), want (1, nil)", removed, err)
	}
}
