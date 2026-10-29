// Copyright 2020 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	goContext "context"
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

var _ vfs.FilesystemImplSaveRestoreExtension = (*filesystem)(nil)

// saveMf is called by stateify.
func (fs *filesystem) saveMf() checkpoint.ResourceID {
	if !fs.mf.IsSavable() {
		panic(fmt.Sprintf("Can't save tmpfs filesystem because its MemoryFile is not savable: %v", fs.mf))
	}
	return fs.mf.ResourceID()
}

// loadMf is called by stateify.
func (fs *filesystem) loadMf(ctx goContext.Context, resourceID checkpoint.ResourceID) {
	if !resourceID.Ok() {
		fs.mf = pgalloc.MemoryFileFromContext(ctx)
		return
	}
	mfmap := pgalloc.MemoryFileMapFromContext(ctx)
	if mfmap == nil {
		panic("CtxMemoryFileMap was not provided")
	}
	mf, ok := mfmap[resourceID]
	if !ok {
		panic(fmt.Sprintf("Memory file for %q not found in CtxMemoryFileMap", resourceID))
	}
	fs.mf = mf
}

// saveParent is called by stateify.
func (d *dentry) saveParent() *dentry {
	return d.parent.Load()
}

// loadParent is called by stateify.
func (d *dentry) loadParent(_ goContext.Context, parent *dentry) {
	d.parent.Store(parent)
}

// saveAccessACL is called by stateify.
func (i *inode) saveAccessACL() *vfs.PosixACL {
	return i.accessACL.Load()
}

// loadAccessACL is called by stateify.
func (i *inode) loadAccessACL(_ goContext.Context, acl *vfs.PosixACL) {
	i.accessACL.Store(acl)
}

// saveDefaultACL is called by stateify.
func (i *inode) saveDefaultACL() *vfs.PosixACL {
	return i.defaultACL.Load()
}

// loadDefaultACL is called by stateify.
func (i *inode) loadDefaultACL(_ goContext.Context, defaultACL *vfs.PosixACL) {
	i.defaultACL.Store(defaultACL)
}

// PrepareSave implements vfs.FilesystemImplSaveRestoreExtension.PrepareSave.
func (fs *filesystem) PrepareSave(ctx context.Context, _ vfs.PrepareSaveOptions) error {
	if pgalloc.IsLiveForkSource(ctx) {
		prepareStarted := time.Now()
		preparationMetrics := vfs.LiveForkPreparationMetricsFromContext(ctx)
		if resource, filtered := vfs.LiveForkFilesystemResourceFromContext(ctx); filtered &&
			(fs.diskVolume == nil || fs.diskVolume.ResourceID != resource) {
			return nil
		}
		if err := fs.flushDiskWriteMetadataRaw(); err != nil {
			return fmt.Errorf("flushing TTC volume metadata before live fork: %w", err)
		}
		metadataDone := time.Now()
		fs.liveForkMu.Lock()
		var files []*regularFile
		if fs.diskVolume == nil {
			files = make([]*regularFile, 0, len(fs.liveForkFiles))
			for file := range fs.liveForkFiles {
				files = append(files, file)
			}
		} else {
			files = make([]*regularFile, 0, len(fs.diskForkFiles))
			for file := range fs.diskForkFiles {
				files = append(files, file)
			}
			fs.diskForkFiles = nil
		}
		fs.liveForkMu.Unlock()
		// A fork whose children are in this process shares the pages of the
		// objects that it seals with them.
		share := memmap.IsLiveForkInProcess(ctx)
		var kept []*regularFile
		for _, file := range files {
			keep, err := file.prepareLiveFork(share)
			if err != nil {
				return err
			}
			if keep {
				kept = append(kept, file)
			}
		}
		if fs.diskVolume != nil && len(kept) != 0 {
			// The next fork that leaves the process must invalidate them.
			fs.liveForkMu.Lock()
			if fs.diskForkFiles == nil {
				fs.diskForkFiles = make(map[*regularFile]struct{}, len(kept))
			}
			for _, file := range kept {
				fs.diskForkFiles[file] = struct{}{}
			}
			fs.liveForkMu.Unlock()
		}
		filesDone := time.Now()
		if fs.diskVolume != nil && vfs.LiveForkBranchesPreparedFromContext(ctx) {
			resources := vfs.LiveForkFilesystemResourcesFromContext(ctx)
			if resources == nil {
				return fmt.Errorf("CtxLiveForkFilesystemResources was not provided for TTC volume")
			}
			if resource, ok := resources[fs.diskVolume.ResourceID]; ok && resource != fs.diskVolume.Repository {
				return fmt.Errorf("live-fork resource for TTC volume %q conflicts with prepared repository", fs.diskVolume.ResourceID)
			}
			resources[fs.diskVolume.ResourceID] = fs.diskVolume.Repository
		}
		if fs.diskVolume != nil && !vfs.LiveForkBranchesPreparedFromContext(ctx) {
			resources := vfs.LiveForkFilesystemResourcesFromContext(ctx)
			if resources == nil {
				return fmt.Errorf("CtxLiveForkFilesystemResources was not provided for TTC volume")
			}
			if _, ok := resources[fs.diskVolume.ResourceID]; ok {
				return fmt.Errorf("live-fork resource for TTC volume %q already exists", fs.diskVolume.ResourceID)
			}
			childBranches := vfs.LiveForkChildBranchIDsFromContext(ctx)
			if len(childBranches) == 0 {
				return fmt.Errorf("live-fork child branch ID was not provided for TTC volume")
			}
			transactions := vfs.LiveForkTransactionsFromContext(ctx)
			if transactions == nil {
				return fmt.Errorf("CtxLiveForkTransactions was not provided for TTC volume")
			}
			originPlan := vfs.LiveForkFilesystemOriginsFromContext(ctx)
			origins, clustered := originPlan[fs.diskVolume.ResourceID]
			if clustered && len(origins) != 2*len(childBranches) {
				return fmt.Errorf("TTC volume %q has %d clustered origins, want %d", fs.diskVolume.ResourceID, len(origins), 2*len(childBranches))
			}
			for index, childBranch := range childBranches {
				var tx *ttcvolume.ForkTransaction
				var err error
				if clustered {
					tx, err = fs.diskVolume.Repository.BeginForkWithOrigins(fs.diskVolume.Branch, childBranch,
						ttcsnapshot.SnapshotID(origins[2*index]), ttcsnapshot.SnapshotID(origins[2*index+1]))
				} else {
					tx, err = fs.diskVolume.Repository.BeginFork(fs.diskVolume.Branch, childBranch)
				}
				if err != nil {
					return fmt.Errorf("forking TTC volume %q as branch %q: %w", fs.diskVolume.ResourceID, childBranch, err)
				}
				transactions.Add(tx)
			}
			resources[fs.diskVolume.ResourceID] = fs.diskVolume.Repository
		}
		if fs.diskVolume != nil && preparationMetrics != nil {
			preparationMetrics.MetadataFlush += metadataDone.Sub(prepareStarted)
			preparationMetrics.RuntimeInvalidation += filesDone.Sub(metadataDone)
			preparationMetrics.BranchSeal += time.Since(filesDone)
		}
	}
	resourceID := fs.mf.ResourceID()
	if !resourceID.Ok() {
		pruneStarted := time.Now()
		candidates := fs.pruneDiskNamespaceForSave()
		if fs.diskVolume != nil {
			if preparationMetrics := vfs.LiveForkPreparationMetricsFromContext(ctx); preparationMetrics != nil {
				preparationMetrics.NamespacePrune += time.Since(pruneStarted)
				preparationMetrics.NamespaceCandidates += int64(candidates)
			}
		}
		return nil
	}
	mfmap := pgalloc.MemoryFileMapFromContext(ctx)
	if mfmap == nil {
		return fmt.Errorf("CtxMemoryFileMap was not provided")
	}
	if _, ok := mfmap[resourceID]; ok {
		return fmt.Errorf("memory file for %q already exists in CtxMemoryFileMap", resourceID)
	}
	mfmap[resourceID] = fs.mf
	// The image's inodes must match the repository it is restored against.
	if err := fs.flushDiskWriteMetadataRaw(); err != nil {
		return fmt.Errorf("flushing TTC volume metadata before save: %w", err)
	}
	fs.pruneDiskNamespaceForSave()
	return nil
}

// BeforeResume implements vfs.FilesystemImplSaveRestoreExtension.BeforeResume.
func (fs *filesystem) BeforeResume(ctx context.Context) {
	if resource, filtered := vfs.LiveForkFilesystemResourceFromContext(ctx); filtered &&
		(fs.diskVolume == nil || fs.diskVolume.ResourceID != resource) {
		return
	}
	if fs.diskVolume == nil {
		fs.liveForkMu.Lock()
		files := make([]*regularFile, 0, len(fs.liveForkFiles))
		for file := range fs.liveForkFiles {
			files = append(files, file)
		}
		fs.liveForkMu.Unlock()
		for _, file := range files {
			file.releaseLiveForkCopies()
		}
	}
	fs.restoreDiskNamespaceAfterSave()
}

// CompleteRestore implements
// vfs.FilesystemImplSaveRestoreExtension.CompleteRestore.
func (fs *filesystem) CompleteRestore(ctx context.Context, opts vfs.CompleteRestoreOptions) error {
	if fs.diskVolume == nil {
		return nil
	}
	if pgalloc.IsLiveForkClone(ctx) {
		resources := vfs.LiveForkFilesystemResourcesFromContext(ctx)
		if resource, ok := resources[fs.diskVolume.ResourceID]; ok {
			repository, ok := resource.(*ttcvolume.Repository)
			if !ok {
				return fmt.Errorf("TTC volume resource %q has type %T", fs.diskVolume.ResourceID, resource)
			}
			childBranch := vfs.LiveForkChildBranchIDFromContext(ctx)
			if childBranch == "" {
				return fmt.Errorf("live-fork child branch ID was not provided for TTC volume")
			}
			if _, err := repository.Branch(childBranch); err != nil {
				return fmt.Errorf("TTC volume child branch %q was not prepared: %w", childBranch, err)
			}
			fs.installDiskVolumeRepository(repository, childBranch)
			fs.registerDiskMaterializationHook()
			fs.rebuildDiskInodeCache()
			return nil
		}
	}
	resources := vfs.RestoreFilesystemResourcesFromContext(ctx)
	resource, ok := resources[fs.diskVolume.ResourceID]
	if !ok {
		available := make([]checkpoint.ResourceID, 0, len(resources))
		for id := range resources {
			available = append(available, id)
		}
		return fmt.Errorf("TTC volume restore resource %q was not provided (available count=%d: %#v)", fs.diskVolume.ResourceID, len(available), available)
	}
	repository, ok := resource.Resource.(*ttcvolume.Repository)
	if !ok {
		return fmt.Errorf("TTC volume restore resource %q has type %T", fs.diskVolume.ResourceID, resource.Resource)
	}
	if resource.Branch == "" {
		return fmt.Errorf("TTC volume restore resource %q has no branch", fs.diskVolume.ResourceID)
	}
	fs.installDiskVolumeRepository(repository, resource.Branch)
	fs.registerDiskMaterializationHook()
	fs.rebuildDiskInodeCache()
	return nil
}

// installDiskVolumeRepository canonicalizes a restored filesystem onto the
// identity of the repository that is authoritative in this Sentry. In
// particular, a distributed peer is serialized with the source node's
// repository identity, but must export the target-local identity if it is
// later selected as the source for another fork. Retaining the incoming ID
// makes a multi-sibling restore depend on which peer the SandboxFamily happens
// to select and eventually causes the daemon's local-to-incoming alias to miss.
func (fs *filesystem) installDiskVolumeRepository(repository *ttcvolume.Repository, branch string) {
	fs.diskVolume.Repository = repository
	fs.diskVolume.Branch = branch
	fs.diskVolume.ResourceID = checkpoint.ResourceID{ContainerName: "ttc-volume", Path: repository.Info().ID}
}
