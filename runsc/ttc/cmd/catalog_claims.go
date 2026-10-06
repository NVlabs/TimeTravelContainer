// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

// A branch rehydrated from a shared image expands its filesystem from the
// image lazily, as its Sentry reads it, and the branch's publication record
// keeps the image at the shared ledger. A local fork of the branch shares the
// expansion: its repository branch names the same remote catalog. Nothing
// counted the fork's need of the image, which the ledger let go of once the
// rehydrated branch was gone, and the fork's unread files with it.
//
// So a fork of a branch that expands a shared image first claims the image
// for the node's repository. The claim is kept while a branch that the family
// places on the node has inherited the catalog: until each such branch has
// been published, whose publication keeps what it reaches of the image, or
// has gone.

// catalogClaimPrefix begins the operation of every catalog claim.
const catalogClaimPrefix = "catalog-"

// claimForkCatalogs claims, for a fork of filesystemBranch of the family in
// info, each shared image that one of the family's repositories on this node
// expands lazily for that branch.
func (b *runtimeBackend) claimForkCatalogs(info ttcbundle.ContainerInfo, filesystemBranch string) error {
	if b.distributedSource == nil || b.distributedSource.authority == nil || b.sharedReferences == nil {
		return nil
	}
	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if seen[path] {
			continue
		}
		seen[path] = true
		catalogs, err := ttcvolume.RemoteCatalogs(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		branch := catalogs[repositoryPhysicalBranch(config, filesystemBranch)]
		catalog := branch.RemoteCatalog
		if catalog == "" {
			continue
		}
		root, err := ttcsnapshot.ParseObjectID(remoteCatalogRoot(catalog))
		if err != nil {
			return fmt.Errorf("repository %q names remote catalog %q: %w", config.Source, catalog, err)
		}
		_, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(root.Snapshot))
		if claim, err := forkCatalogClaimed(branch, volatile, err); err != nil {
			return fmt.Errorf("finding the image that repository %q expands: %w", config.Source, err)
		} else if !claim {
			continue
		}
		if err := b.claimCatalog(info.ID, path, catalog, root); err != nil {
			return fmt.Errorf("claiming the image that repository %q expands: %w", config.Source, err)
		}
	}
	return nil
}

// forkCatalogClaimed reports whether a fork of branch claims the image that
// its remote catalog expands, from what the authority says of the image's
// origin: whether it is volatile, or the error of the lookup.
func forkCatalogClaimed(branch ttcvolume.Branch, volatile bool, err error) (bool, error) {
	switch {
	case errors.Is(err, os.ErrNotExist) && !branch.RemoteShared:
		// The source of a remote fork or a migration whose transaction
		// ended once the target no longer needed it, and retired the
		// origin. The branch names the catalog until its next publication:
		// a fork of a remote fork's target was refused, "finding the image
		// that repository ... expands: file does not exist".
		return false, nil
	case err != nil:
		return false, err
	case volatile:
		// The source of a remote fork or a migration, which its
		// transaction keeps until the target no longer needs it.
		return false, nil
	}
	return true, nil
}

// claimCatalog retains, at the shared ledger, the node's claim on root, the
// image that branches of container's repository at path expand by catalog,
// unless the node claims it already.
func (b *runtimeBackend) claimCatalog(container, path, catalog string, root ttcsnapshot.ObjectID) error {
	dir := filepath.Join(b.activeDataRoot(), "graph-references")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), catalogClaimPrefix) || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		claimPath := filepath.Join(dir, entry.Name())
		intent, err := readGraphReferenceIntent(claimPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if intent.Purpose != graphReferencePurposeCatalog || intent.Repository != path || intent.Catalog != catalog || intent.State == "releasing" {
			continue
		}
		if intent.State == "active" {
			return nil
		}
		return b.resumeGraphReferenceRetain(claimPath, &intent)
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	operation := fmt.Sprintf("%s%016x-%s", catalogClaimPrefix, uint64(root.Snapshot), id)
	claimPath, err := graphReferenceIntentPath(b.activeDataRoot(), operation)
	if err != nil {
		return err
	}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: operation, Operation: operation,
		Purpose: graphReferencePurposeCatalog, Container: container, State: "retaining", CreatedAt: time.Now().UTC(),
		Groups:     []graphReferenceGroup{{Origin: uint64(root.Snapshot), Roots: []cluster.SnapshotRootBinding{{Slot: 0, Object: root.String()}}}},
		Repository: path, Catalog: catalog,
	}
	if err := writeDurableJSON(claimPath, &intent); err != nil {
		return err
	}
	return b.resumeGraphReferenceRetain(claimPath, &intent)
}

// reconcileCatalogClaim releases the catalog claim at path once no branch
// needs it, as catalogClaimNeeded tells, checking one still needed ever less
// often, up to every two minutes.
func (b *runtimeBackend) reconcileCatalogClaim(path string, intent graphReferenceIntent) error {
	if intent.State == "releasing" {
		return b.releaseDistributedGraphReferences(intent.Operation)
	}
	now := time.Now()
	check := providerCheck{}
	if value, ok := b.distributedSource.providerChecks.Load(intent.Operation); ok {
		check = value.(providerCheck)
		if now.Before(check.next) {
			return nil
		}
	}
	needed, err := b.catalogClaimNeeded(intent)
	if err != nil {
		return err
	}
	if needed {
		if intent.State == "retaining" {
			// Its fork stopped with the claim half made.
			return b.resumeGraphReferenceRetain(path, &intent)
		}
		check.interval = min(max(2*check.interval, 5*time.Second), 2*time.Minute)
		check.next = now.Add(check.interval)
		b.distributedSource.providerChecks.Store(intent.Operation, check)
		return nil
	}
	b.distributedSource.providerChecks.Delete(intent.Operation)
	if err := abandonGraphReferenceClaim(path, intent); err != nil {
		return err
	}
	return b.releaseDistributedGraphReferences(intent.Operation)
}

// catalogClaimNeeded reports whether a branch that the family of intent, a
// catalog claim, places on this node still names the claim's catalog in the
// claim's repository, having inherited it: the branch that imported the image
// is kept by its own publications. A copy of a branch placed elsewhere, as a
// fork of a dehydrated branch leaves of its source, does not keep the image.
func (b *runtimeBackend) catalogClaimNeeded(intent graphReferenceIntent) (bool, error) {
	catalogs, err := ttcvolume.RemoteCatalogs(intent.Repository)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	inherits := func(branch ttcvolume.Branch) bool {
		return branch.RemoteCatalog == intent.Catalog && branch.RemoteInherited
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(catalogs)), inherits) {
		return false, nil
	}
	info, err := b.containerStore().LoadContainer(intent.Container)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return false, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return false, err
	}
	for _, config := range configs {
		if filepath.Clean(config.Path) != intent.Repository {
			continue
		}
		for _, branch := range info.Branches {
			if branch.Unplaced || branch.NodeID != b.nodeID {
				continue
			}
			if inherits(catalogs[repositoryPhysicalBranch(config, branch.FilesystemBranch)]) {
				return true, nil
			}
		}
	}
	return false, nil
}
