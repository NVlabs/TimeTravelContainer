// Copyright 2018 The gVisor Authors.
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

package kernel

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
)

// LiveForkOptions controls an experimental in-memory Kernel clone.
type LiveForkOptions struct {
	// Quarantine cuts the child off from everything outside the sandbox.
	//
	// Its links are rebuilt with nothing on the other end: what its programs
	// write is accepted and discarded, what they read never comes, and no new
	// connection can be made. Existing connections are left up rather than
	// reset, because a child forked to be asked one question and then destroyed
	// should not be able to tell that it is a copy — and nothing outside should
	// be able to tell that it existed.
	Quarantine bool
	// Clocks is the host clock source installed in the cloned Timekeeper.
	Clocks sentrytime.Clocks

	// ChildBranchID identifies the destination branch to branch-aware
	// filesystems.
	ChildBranchID string

	// FilesystemOrigins contains Raft-allocated [newSource,newChild] pairs for
	// every persistent filesystem. Nil selects standalone local allocation.
	FilesystemOrigins vfs.LiveForkFilesystemOrigins

	// RestoreOptions are passed to filesystem restore completion hooks.
	RestoreOptions vfs.CompleteRestoreOptions

	// AllowNetworkStack permits cloning an initialized network stack. Callers
	// must first prove that every link and endpoint is contained in the sandbox.
	AllowNetworkStack bool

	// AllowSharedFilesystemResources permits clone-aware filesystems to borrow
	// resources that the caller has proven immutable or otherwise share-safe.
	AllowSharedFilesystemResources bool

	// TestOnlyFailureInjector may return an error to abort immediately after a
	// transaction stage. It exists to verify rollback and source reusability;
	// production callers should leave it nil.
	TestOnlyFailureInjector func(LiveForkStage) error

	// Metrics, when non-nil, receives a timing breakdown for this attempt.
	Metrics *LiveForkMetrics

	// RemoteMemoryFile, when non-nil, receives a metadata-only clone of the
	// source allocator and is populated on demand through an in-process remote
	// snapshot provider. Ownership transfers to the returned Kernel on success
	// and is consumed on failure. It must be newly created and empty.
	//
	// This is the local transport integration point for distributed live fork;
	// cross-process orchestration supplies the same page-loader protocol through
	// a network reader.
	RemoteMemoryFile *pgalloc.MemoryFile

	// RemotePageReaderFactory optionally replaces the in-process loopback page
	// reader when RemoteMemoryFile is set. It takes ownership of snapshot if it
	// returns a non-nil reader. This permits TCP transport testing without
	// coupling Kernel to a particular distributed transport.
	RemotePageReaderFactory func(snapshot *pgalloc.RemoteSnapshot) (stateio.AsyncReader, error)

	// batchChildBranchIDs and batchFilesystemResources are used by
	// LiveForkMany to prepare its primary filesystem branch and retain the
	// resources needed for post-resume sibling branching. batchLoads are the
	// loads of the siblings, which load after the primary, until which the
	// source keeps what it saved for its children. They are deliberately
	// not exported: callers should use LiveForkMany.
	batchChildBranchIDs      []string
	batchFilesystemResources vfs.LiveForkFilesystemResources
	batchLoads               *vfs.LiveForkLoads
}

// LiveForkValidationOptions controls live-fork resource validation.
type LiveForkValidationOptions struct {
	// AllowNetworkStack permits validation of an initialized, contained stack.
	AllowNetworkStack bool

	// AllowSharedFilesystemResources permits read-only filesystems that have
	// explicitly classified their external resources as shareable.
	AllowSharedFilesystemResources bool
}

// LiveForkMetrics separates parent preparation, metadata, and child restore
// costs. Durations include only work performed by Kernel.LiveFork.
type LiveForkMetrics struct {
	Validation                    time.Duration
	ParentPreparation             time.Duration
	SharedMemoryPreparation       time.Duration
	MappingInvalidation           time.Duration
	VFSPreparation                time.Duration
	FilesystemMetadataFlush       time.Duration
	FilesystemRuntimeInvalidation time.Duration
	FilesystemBranchSeal          time.Duration
	FilesystemNamespacePrune      time.Duration
	FilesystemNamespaceCandidates int64
	// DevicePreparation is how much of VFSPreparation devices took, such
	// as a GPU to take its snapshot.
	DevicePreparation time.Duration
	MemoryPreparation time.Duration
	MetadataStream    time.Duration
	MetadataBytes     int64
	// MetadataRecord is how long recording the source's metadata, for
	// in-memory copies, took: part of MetadataStream for a child of
	// LiveFork, and before it for the siblings of LiveForkMany, which copy
	// one recording. MetadataObjects is the number of objects copied.
	MetadataRecord  time.Duration
	MetadataObjects int64
	ChildRestore    time.Duration
	Total           time.Duration
}

// LiveForkStage identifies a completed live-fork transaction stage.
type LiveForkStage string

const (
	LiveForkStageValidated            LiveForkStage = "validated"
	LiveForkStageSharedMemoryPrepared LiveForkStage = "shared-memory-prepared"
	LiveForkStageMappingsInvalidated  LiveForkStage = "mappings-invalidated"
	LiveForkStageVFSPrepared          LiveForkStage = "vfs-prepared"
	LiveForkStageMemoryPrepared       LiveForkStage = "memory-prepared"
	LiveForkStageOwnerAcquired        LiveForkStage = "owner-acquired"
	LiveForkStageChildInitialized     LiveForkStage = "child-initialized"
	LiveForkStageMetadataLoaded       LiveForkStage = "metadata-loaded"
	LiveForkStageVDSOPrivatized       LiveForkStage = "vdso-privatized"
	LiveForkStageNetworkRestored      LiveForkStage = "network-restored"
	LiveForkStageVFSRestored          LiveForkStage = "vfs-restored"
	LiveForkStageChildPaused          LiveForkStage = "child-paused"
)

func injectLiveForkFailure(opts LiveForkOptions, stage LiveForkStage) error {
	if opts.TestOnlyFailureInjector == nil {
		return nil
	}
	if err := opts.TestOnlyFailureInjector(stage); err != nil {
		return fmt.Errorf("live fork aborted after %s: %w", stage, err)
	}
	return nil
}

// LiveForkResourceIssue describes one resource that prevents an in-memory
// fork. PID and FD identify the descriptor-table root through which Resource
// was reached; Resource may be a transitively referenced object.
type LiveForkResourceIssue struct {
	PID            ThreadID `json:"pid"`
	FD             int32    `json:"fd"`
	MountPath      string   `json:"mount_path,omitempty"`
	FilesystemType string   `json:"filesystem_type,omitempty"`
	SourceClass    string   `json:"source_class,omitempty"`
	Resource       string   `json:"resource"`
	Reason         string   `json:"reason"`
}

// LiveForkResourceError is returned before the parent is changed when its
// open-resource graph is not closed within the Sentry.
type LiveForkResourceError struct {
	Issues []LiveForkResourceIssue
}

// Error implements error.Error.
func (e *LiveForkResourceError) Error() string {
	if len(e.Issues) == 0 {
		return "live fork resource validation failed"
	}
	i := e.Issues[0]
	if len(e.Issues) == 1 {
		if i.FD < 0 {
			return fmt.Sprintf("live fork resource validation failed: pid %d mapping resource %s: %s", i.PID, i.Resource, i.Reason)
		}
		return fmt.Sprintf("live fork resource validation failed: pid %d fd %d resource %s: %s", i.PID, i.FD, i.Resource, i.Reason)
	}
	if i.FD < 0 {
		return fmt.Sprintf("live fork resource validation failed with %d issues; first: pid %d mapping resource %s: %s", len(e.Issues), i.PID, i.Resource, i.Reason)
	}
	return fmt.Sprintf("live fork resource validation failed with %d issues; first: pid %d fd %d resource %s: %s", len(e.Issues), i.PID, i.FD, i.Resource, i.Reason)
}

// ValidateLiveFork reports whether k's current resource graph can be live
// forked. It performs no CoW, VFS save, state streaming, or child allocation.
// Preconditions: k must be paused for the duration of the call.
func (k *Kernel) ValidateLiveFork(ctx context.Context, opts LiveForkValidationOptions) error {
	if !k.IsPaused() {
		return fmt.Errorf("live fork validation requires a paused kernel")
	}
	k.tasks.waitForExternalStop()
	return k.quiescePausedAnd(ctx, func() error {
		resumeNetwork, err := k.pauseLiveForkNetworkStacks(opts.AllowNetworkStack, true)
		if err != nil {
			return err
		}
		defer resumeNetwork()
		if err := k.validateLiveForkFilesystems(ctx, opts.AllowSharedFilesystemResources); err != nil {
			return err
		}
		return k.validateLiveForkResourcesAndMappings()
	})
}

// validateLiveForkResourcesAndMappings applies the authoritative resource
// checks. k must be paused and inside quiescePausedAnd.
func (k *Kernel) validateLiveForkResourcesAndMappings() error {
	if err := k.validateLiveForkResources(); err != nil {
		return err
	}
	return k.validateLiveForkMappings()
}

// LiveFork creates a paused Kernel by streaming only state metadata from k.
// Application pages remain in the shared MemoryFile and are made copy-on-write.
// The returned Kernel has not executed any task instructions; callers must call
// StartPaused and then Unpause to make it runnable.
//
// This API is intentionally experimental. Eligible TTC kernels prevent known
// unsupported resources from entering the graph; the frozen validation and
// private-MemoryFile checks below remain fail-closed invariant guards.
//
// Preconditions: k must be paused for the duration of the call.
func (k *Kernel) LiveFork(ctx context.Context, opts LiveForkOptions) (*Kernel, error) {
	started := time.Now()
	if opts.Metrics != nil {
		*opts.Metrics = LiveForkMetrics{}
		defer func() {
			opts.Metrics.Total = time.Since(started)
		}()
	}
	if !k.IsPaused() {
		return nil, fmt.Errorf("live fork requires a paused kernel")
	}
	k.tasks.waitForExternalStop()
	k.ReceiveTaskStates()
	if opts.Clocks == nil {
		return nil, fmt.Errorf("live fork requires a clock source")
	}
	var child *Kernel
	err := k.quiescePausedAnd(ctx, func() (retErr error) {
		if err := k.flushPlatformForStateCapture(ctx); err != nil {
			return err
		}
		validationStarted := time.Now()
		resumeNetwork, err := k.pauseLiveForkNetworkStacks(opts.AllowNetworkStack, true)
		if err != nil {
			return err
		}
		defer resumeNetwork()
		if err := k.validateLiveForkFilesystems(ctx, opts.AllowSharedFilesystemResources); err != nil {
			return err
		}
		if err := k.validateLiveForkResourcesAndMappings(); err != nil {
			return err
		}
		if opts.Metrics != nil {
			opts.Metrics.Validation = time.Since(validationStarted)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageValidated); err != nil {
			return err
		}
		preparationStarted := time.Now()
		sharedMemoryStarted := time.Now()
		k.prepareSharedMemoryForLiveFork(ctx)
		if opts.Metrics != nil {
			opts.Metrics.SharedMemoryPreparation = time.Since(sharedMemoryStarted)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageSharedMemoryPrepared); err != nil {
			return err
		}
		// Discard PMAs backed by files that state serialization cannot encode,
		// such as a gofer MmapCachedFile. The VMAs remain and refault from their
		// branch-local cloned filesystem graph. This must precede VFS.PrepareSave
		// for the same reason as in Kernel.SaveTo.
		mappingInvalidationStarted := time.Now()
		invalidateCtx := ctx
		if opts.RemoteMemoryFile == nil {
			// The child shares the MemoryFile, and with it the pages of
			// files that cannot change, which their mappings keep.
			invalidateCtx = context.WithValue(ctx, memmap.CtxLiveForkInProcess, true)
		}
		if err := k.invalidateUnsavableMappings(invalidateCtx); err != nil {
			return fmt.Errorf("invalidating unsavable mappings for live fork: %w", err)
		}
		if opts.Metrics != nil {
			opts.Metrics.MappingInvalidation = time.Since(mappingInvalidationStarted)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageMappingsInvalidated); err != nil {
			return err
		}
		mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
		fsResources := opts.batchFilesystemResources
		if fsResources == nil && opts.AllowSharedFilesystemResources {
			fsResources = make(vfs.LiveForkFilesystemResources)
		}
		fsTransactions := &vfs.LiveForkTransactions{}
		fsPreparationMetrics := &vfs.LiveForkPreparationMetrics{}
		fsTransactionsCommitted := false
		defer func() {
			if fsTransactionsCommitted {
				return
			}
			if err := fsTransactions.Abort(); err != nil {
				if retErr == nil {
					retErr = fmt.Errorf("aborting live-fork filesystem transactions: %w", err)
				} else {
					retErr = fmt.Errorf("%w; aborting live-fork filesystem transactions: %v", retErr, err)
				}
			}
		}()
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           mfs,
			pgalloc.CtxLiveForkSource:          true,
			memmap.CtxLiveForkInProcess:        opts.RemoteMemoryFile == nil,
			vfs.CtxLiveForkFilesystemResources: fsResources,
			vfs.CtxLiveForkChildBranchID:       opts.ChildBranchID,
			vfs.CtxLiveForkChildBranchIDs:      opts.batchChildBranchIDs,
			vfs.CtxLiveForkTransactions:        fsTransactions,
			vfs.CtxLiveForkFilesystemOrigins:   opts.FilesystemOrigins,
			vfs.CtxLiveForkPreparationMetrics:  fsPreparationMetrics,
		})
		if opts.batchLoads != nil {
			saveCtx = context.WithValue(saveCtx, vfs.CtxLiveForkLoads, opts.batchLoads)
		}
		vfsPreparationStarted := time.Now()
		if err := k.vfs.PrepareSave(saveCtx, vfs.PrepareSaveOptions{}); err != nil {
			return fmt.Errorf("preparing VFS for live fork: %w", err)
		}
		if opts.Metrics != nil {
			opts.Metrics.VFSPreparation = time.Since(vfsPreparationStarted)
			opts.Metrics.FilesystemMetadataFlush = fsPreparationMetrics.MetadataFlush
			opts.Metrics.FilesystemRuntimeInvalidation = fsPreparationMetrics.RuntimeInvalidation
			opts.Metrics.FilesystemBranchSeal = fsPreparationMetrics.BranchSeal
			opts.Metrics.FilesystemNamespacePrune = fsPreparationMetrics.NamespacePrune
			opts.Metrics.FilesystemNamespaceCandidates = fsPreparationMetrics.NamespaceCandidates
			opts.Metrics.DevicePreparation = fsPreparationMetrics.Devices
		}
		defer k.vfs.BeforeResume(saveCtx)
		if err := injectLiveForkFailure(opts, LiveForkStageVFSPrepared); err != nil {
			return err
		}
		if len(mfs) != 0 {
			return fmt.Errorf("live fork does not yet support %d private filesystem MemoryFiles", len(mfs))
		}

		memoryPreparationStarted := time.Now()
		pmaSnapshots, clearPMASnapshots, err := k.prepareMemoryManagersForLiveFork()
		defer clearPMASnapshots()
		if err != nil {
			return fmt.Errorf("preparing memory for live fork: %w", err)
		}
		k.mf.MarkSavable()
		if opts.Metrics != nil {
			opts.Metrics.MemoryPreparation = time.Since(memoryPreparationStarted)
		}
		childMemoryFile := k.mf
		var remotePageLoad *pgalloc.AsyncPagesFileLoad
		var remoteLoadRanges []pgalloc.RemoteLoadRange
		var remoteSnapshot *pgalloc.RemoteSnapshot
		if opts.RemoteMemoryFile != nil {
			childMemoryFile = opts.RemoteMemoryFile
			allocatorMetadata, _, loads, snapshots, err := pgalloc.ExportMemoryFileMetadataAndSnapshotsForRemote(ctx, k.mf, 1)
			if err != nil {
				childMemoryFile.Destroy()
				return fmt.Errorf("exporting remote MemoryFile metadata: %w", err)
			}
			if err := pgalloc.ImportMemoryFileMetadataForRemote(ctx, childMemoryFile, allocatorMetadata); err != nil {
				childMemoryFile.Destroy()
				return fmt.Errorf("importing remote MemoryFile metadata: %w", err)
			}
			snapshot := snapshots[0]
			var reader stateio.AsyncReader
			if opts.RemotePageReaderFactory != nil {
				reader, err = opts.RemotePageReaderFactory(snapshot)
				if err != nil {
					_ = snapshot.Close()
					childMemoryFile.Destroy()
					return fmt.Errorf("creating remote page reader: %w", err)
				}
				if reader == nil {
					_ = snapshot.Close()
					childMemoryFile.Destroy()
					return errors.New("remote page reader factory returned nil")
				}
			} else {
				reader = pgalloc.NewLoopbackRemoteReader(snapshot, 256<<10, 16, 4)
			}
			remotePageLoad, err = pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
			if err != nil {
				childMemoryFile.Destroy()
				return fmt.Errorf("starting remote page loader: %w", err)
			}
			remoteLoadRanges = loads
			remoteSnapshot = snapshot
		}
		if opts.Metrics != nil {
			opts.Metrics.ParentPreparation = time.Since(preparationStarted)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageMemoryPrepared); err != nil {
			return err
		}

		if opts.RemoteMemoryFile == nil {
			k.mf.IncOwner()
		}
		ownerTransferred := false
		childLoaded := false
		defer func() {
			if ownerTransferred {
				return
			}
			if childLoaded {
				child.releaseLoaded(opts.Clocks)
			} else {
				if child != nil {
					child.cleanupDevGofers()
				}
				if remotePageLoad != nil {
					remotePageLoad.Cancel()
					_ = remotePageLoad.Wait()
				}
				childMemoryFile.Destroy()
			}
		}()
		if err := injectLiveForkFailure(opts, LiveForkStageOwnerAcquired); err != nil {
			return err
		}

		child = &Kernel{Platform: k.Platform}
		child.SetMemoryFile(childMemoryFile)
		child.initRuntimeFields()
		child.inheritDevGofers(k)
		if err := injectLiveForkFailure(opts, LiveForkStageChildInitialized); err != nil {
			return err
		}
		loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:                childMemoryFile,
			pgalloc.CtxMemoryFileMap:             mfs,
			pgalloc.CtxLiveForkClone:             true,
			pgalloc.CtxLiveForkRemoteMemoryClone: opts.RemoteMemoryFile != nil,
			vfs.CtxLiveForkFilesystemResources:   fsResources,
			vfs.CtxLiveForkChildBranchID:         opts.ChildBranchID,
			vfs.CtxLiveForkTransactions:          fsTransactions,
			mm.CtxLiveForkPMASnapshots:           pmaSnapshots,
		})
		metadataStarted := time.Now()
		if err := streamLiveForkState(saveCtx, loadCtx, k, child, opts.Metrics); err != nil {
			return err
		}
		if remotePageLoad != nil {
			if err := childMemoryFile.AttachRemotePageLoad(remotePageLoad, remoteLoadRanges, nil, nil); err != nil {
				return fmt.Errorf("attaching remote page loader: %w", err)
			}
			remotePageLoad.MemoryFilesDone()
			child.remotePageLoad = remotePageLoad
			if opts.RemotePageReaderFactory == nil {
				pgalloc.StartLoopbackRemotePush(remoteSnapshot, remotePageLoad, 256<<10)
			}
		} else {
			// An ordinary local fork shares the source MemoryFile, including any
			// not-yet-resident chunks. Give the child an explicit owner of those
			// readers; page references alone do not preserve their lifetime.
			child.InheritDistributedForkMemorySources(k, k.remoteLoadRanges)
		}
		if opts.Metrics != nil {
			opts.Metrics.MetadataStream = time.Since(metadataStarted)
		}
		childLoaded = true
		if err := injectLiveForkFailure(opts, LiveForkStageMetadataLoaded); err != nil {
			return err
		}

		restoreStarted := time.Now()
		if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
			return err
		}
		child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
		if err := injectLiveForkFailure(opts, LiveForkStageVDSOPrivatized); err != nil {
			return err
		}
		// Quarantine is inherited. A child cannot regain an external edge merely
		// by forking a quarantined source; it may be explicitly unquarantined by
		// a later branch operation.
		child.liveForkQuarantine = k.liveForkQuarantine || opts.Quarantine
		if err := child.restoreLiveForkNetworkStacks(k.rootNetworkNamespace.Stack()); err != nil {
			return err
		}
		if err := injectLiveForkFailure(opts, LiveForkStageNetworkRestored); err != nil {
			return err
		}
		if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
			return vfs.PrependErrMsg("VFS live-fork restore failed", err)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageVFSRestored); err != nil {
			return err
		}
		child.Pause()
		if opts.Metrics != nil {
			opts.Metrics.ChildRestore = time.Since(restoreStarted)
		}
		if err := injectLiveForkFailure(opts, LiveForkStageChildPaused); err != nil {
			return err
		}
		if err := fsTransactions.Commit(); err != nil {
			return fmt.Errorf("committing live-fork filesystem transactions: %w", err)
		}
		fsTransactionsCommitted = true
		ownerTransferred = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return child, nil
}

// LiveForkMany creates multiple sibling Kernels from one paused source
// snapshot. The first child is loaded while the source is paused, which pins
// all shared MemoryFile ranges. The paused first child is then recorded, and
// sourceReady is called; the remaining children are copies of the recording,
// made concurrently.
//
// All options must name distinct non-empty ChildBranchIDs and use compatible
// resource-policy settings. The returned children correspond to opts by index.
// sourceReady may be nil. If it succeeds, callers must assume that the source
// may be running when this method subsequently returns an error.
func (k *Kernel) LiveForkMany(ctx context.Context, opts []LiveForkOptions, sourceReady func() error) ([]*Kernel, error) {
	if len(opts) == 0 {
		return nil, fmt.Errorf("live fork batch requires at least one child")
	}
	ids := make([]string, len(opts))
	seen := make(map[string]struct{}, len(opts))
	for i := range opts {
		if opts[i].ChildBranchID == "" {
			return nil, fmt.Errorf("live fork batch child %d has an empty branch ID", i)
		}
		if _, ok := seen[opts[i].ChildBranchID]; ok {
			return nil, fmt.Errorf("live fork batch repeats child branch ID %q", opts[i].ChildBranchID)
		}
		seen[opts[i].ChildBranchID] = struct{}{}
		ids[i] = opts[i].ChildBranchID
		if i != 0 && (opts[i].AllowNetworkStack != opts[0].AllowNetworkStack ||
			opts[i].AllowSharedFilesystemResources != opts[0].AllowSharedFilesystemResources) {
			return nil, fmt.Errorf("live fork batch child %q uses incompatible resource policy", opts[i].ChildBranchID)
		}
	}
	if len(opts) == 1 {
		singleOpts := opts[0]
		singleOpts.batchChildBranchIDs = ids
		child, err := k.LiveFork(ctx, singleOpts)
		if err != nil {
			return nil, err
		}
		if sourceReady != nil {
			if err := sourceReady(); err != nil {
				child.Release()
				return nil, fmt.Errorf("making live-fork source ready: %w", err)
			}
		}
		return []*Kernel{child}, nil
	}

	fsResources := make(vfs.LiveForkFilesystemResources)
	// The siblings load what the capture of the primary saves once the
	// capture ends: what the source's devices keep for the primary, which the
	// primary's devices may pass on to the siblings, and what the primary's
	// keep for the siblings, last until the loads end.
	loads := &vfs.LiveForkLoads{}
	primaryOpts := opts[0]
	primaryOpts.batchChildBranchIDs = ids[:1]
	primaryOpts.batchFilesystemResources = fsResources
	primaryOpts.batchLoads = loads
	if primaryOpts.FilesystemOrigins != nil {
		primaryOpts.FilesystemOrigins = make(vfs.LiveForkFilesystemOrigins, len(opts[0].FilesystemOrigins))
		for resource, origins := range opts[0].FilesystemOrigins {
			if len(origins) != 2*len(opts) {
				return nil, fmt.Errorf("filesystem resource %q has %d clustered origins, want %d", resource, len(origins), 2*len(opts))
			}
			primaryOpts.FilesystemOrigins[resource] = append([]uint64(nil), origins[:2]...)
		}
	}
	primary, err := k.LiveFork(ctx, primaryOpts)
	if err != nil {
		loads.Done()
		return nil, err
	}
	children := make([]*Kernel, len(opts))
	children[0] = primary
	cleanup := func() {
		for _, child := range children {
			if child != nil {
				child.Release()
			}
		}
		// No child has been started yet, so it is safe to discard all
		// filesystem branch pointers published by either batch phase.
		for _, resource := range fsResources {
			remover, ok := resource.(interface{ RemoveBranch(string) error })
			if !ok {
				continue
			}
			for _, id := range ids {
				_ = remover.RemoveBranch(id)
			}
		}
	}
	// The primary child is already the requested result for a one-child fork.
	// Recording it exists only to amortize metadata recording across
	// additional siblings.
	if len(opts) == 1 {
		loads.Done()
		if sourceReady != nil {
			if err := sourceReady(); err != nil {
				cleanup()
				return nil, fmt.Errorf("making live-fork source ready: %w", err)
			}
		}
		return children, nil
	}
	// captureLiveForkMetadata uses process-wide quiescence mechanisms such as
	// fdnotifier.Pause. Finish the reusable child capture before sourceReady
	// resumes another Kernel in this Sentry. Only copying the additional
	// children is safe to overlap with source execution: copies read only
	// what the recording took.
	if err := forkPreparedFilesystemBranches(fsResources, ids[0], ids[1:], opts[0].FilesystemOrigins); err != nil {
		loads.Done()
		cleanup()
		return nil, err
	}
	recording, pmaSnapshots, err := primary.captureLiveForkMetadata(fsResources, loads)
	if err != nil {
		loads.Done()
		cleanup()
		return nil, fmt.Errorf("capturing primary live-fork child: %w", err)
	}
	if sourceReady != nil {
		if err := sourceReady(); err != nil {
			loads.Done()
			cleanup()
			return nil, fmt.Errorf("making live-fork source ready: %w", err)
		}
	}
	var networkConfigSource inet.Stack
	if rootNS := primary.rootNetworkNamespace; rootNS != nil {
		networkConfigSource = rootNS.Stack()
	}
	errs := make([]error, len(opts))
	var wg sync.WaitGroup
	for i := 1; i < len(opts); i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			child, err := k.loadCapturedLiveForkChild(
				ctx,
				recording,
				fsResources,
				pmaSnapshots,
				networkConfigSource,
				opts[i],
			)
			children[i] = child
			errs[i] = err
		}()
	}
	wg.Wait()
	loads.Done()
	for i := 1; i < len(errs); i++ {
		if errs[i] != nil {
			cleanup()
			return nil, fmt.Errorf("loading live-fork batch child %q: %w", opts[i].ChildBranchID, errs[i])
		}
	}
	return children, nil
}

// forkPreparedFilesystemBranches creates remaining persistent siblings from
// the paused primary child. Its metadata root is the exact root captured from
// the original source, but this work no longer contributes to source downtime.
func forkPreparedFilesystemBranches(resources vfs.LiveForkFilesystemResources, primary string, siblings []string, originPlan vfs.LiveForkFilesystemOrigins) error {
	for resourceID, resource := range resources {
		brancher, ok := resource.(vfs.LiveForkBranchingResource)
		if !ok {
			continue
		}
		origins, clustered := originPlan[resourceID]
		if clustered && len(origins) != 2*(len(siblings)+1) {
			return fmt.Errorf("filesystem resource %q has %d clustered origins, want %d", resourceID, len(origins), 2*(len(siblings)+1))
		}
		for index, sibling := range siblings {
			var err error
			if clustered {
				originBrancher, ok := resource.(vfs.LiveForkOriginBranchingResource)
				if !ok {
					return fmt.Errorf("filesystem resource %q cannot accept clustered snapshot origins", resourceID)
				}
				offset := 2 * (index + 1)
				err = originBrancher.ForkWithOrigins(primary, sibling, origins[offset], origins[offset+1])
			} else {
				err = brancher.Fork(primary, sibling)
			}
			if err != nil {
				return fmt.Errorf("forking prepared filesystem resource %q from %q as %q: %w", resourceID, primary, sibling, err)
			}
		}
	}
	return nil
}

// captureLiveForkMetadata prepares paused child k as an additional live-fork
// source and records its metadata in memory, for copies. Persistent
// filesystem branches were created before this pass, so it only establishes
// CoW for in-memory filesystem data. The siblings that copy the recording end
// loads.
func (k *Kernel) captureLiveForkMetadata(fsResources vfs.LiveForkFilesystemResources, loads *vfs.LiveForkLoads) (*state.Recording, mm.LiveForkPMASnapshots, error) {
	if !k.IsPaused() {
		return nil, nil, fmt.Errorf("capturing live-fork metadata requires a paused kernel")
	}
	var recording *state.Recording
	var snapshots mm.LiveForkPMASnapshots
	err := k.quiescePausedAnd(k.SupervisorContext(), func() error {
		if err := k.flushPlatformForStateCapture(k.SupervisorContext()); err != nil {
			return err
		}
		// Endpoint beforeSave hooks freeze packet queues and register them for
		// resumption. Pair the metadata-only save with the same stack lifecycle
		// used by LiveFork and Kernel.SaveTo; otherwise the primary batch child
		// retains frozen endpoints while only the decoded siblings are thawed.
		resumeNetwork, err := k.pauseLiveForkNetworkStacks(true, false)
		if err != nil {
			return err
		}
		defer resumeNetwork()
		k.prepareSharedMemoryForLiveFork(k.SupervisorContext())

		mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           mfs,
			pgalloc.CtxLiveForkSource:          true,
			memmap.CtxLiveForkInProcess:        true,
			vfs.CtxLiveForkFilesystemResources: fsResources,
			vfs.CtxLiveForkBranchesPrepared:    true,
			vfs.CtxLiveForkLoads:               loads,
		})
		if err := k.vfs.PrepareSave(saveCtx, vfs.PrepareSaveOptions{}); err != nil {
			return fmt.Errorf("preparing primary VFS for sibling capture: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		if len(mfs) != 0 {
			return fmt.Errorf("live fork does not yet support %d private filesystem MemoryFiles", len(mfs))
		}

		// The paused child that this records has pinned nothing for DMA, so
		// the snapshots hold no copies, which their clearing would release
		// before the siblings load.
		var clearSnapshots func()
		snapshots, clearSnapshots, err = k.prepareMemoryManagersForLiveFork()
		defer clearSnapshots()
		if err != nil {
			return fmt.Errorf("preparing memory for live fork: %w", err)
		}
		k.mf.MarkSavable()

		if recording, err = k.recordStateGraph(saveCtx); err != nil {
			return fmt.Errorf("recording live-fork metadata: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return recording, snapshots, nil
}

// loadCapturedLiveForkChild reconstructs one sibling, a copy of the metadata
// that LiveForkMany recorded. The primary child remains paused and owns
// references to every shared MemoryFile range, so the source may run
// concurrently with this load.
func (k *Kernel) loadCapturedLiveForkChild(
	ctx context.Context,
	recording *state.Recording,
	fsResources vfs.LiveForkFilesystemResources,
	pmaSnapshots mm.LiveForkPMASnapshots,
	networkConfigSource inet.Stack,
	opts LiveForkOptions,
) (*Kernel, error) {
	started := time.Now()
	if opts.Metrics != nil {
		*opts.Metrics = LiveForkMetrics{}
		defer func() {
			opts.Metrics.Total = time.Since(started)
		}()
	}
	if opts.Clocks == nil {
		return nil, fmt.Errorf("live fork requires a clock source")
	}

	k.mf.IncOwner()
	childLoaded := false
	child := &Kernel{Platform: k.Platform}
	child.SetMemoryFile(k.mf)
	child.initRuntimeFields()
	child.inheritDevGofers(k)
	defer func() {
		if childLoaded {
			return
		}
		child.cleanupDevGofers()
		k.mf.Destroy()
	}()
	mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
	loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
		pgalloc.CtxMemoryFile:              k.mf,
		pgalloc.CtxMemoryFileMap:           mfs,
		pgalloc.CtxLiveForkClone:           true,
		vfs.CtxLiveForkFilesystemResources: fsResources,
		vfs.CtxLiveForkChildBranchID:       opts.ChildBranchID,
		mm.CtxLiveForkPMASnapshots:         pmaSnapshots,
	})
	metadataStarted := time.Now()
	stats, err := recording.Copy(loadCtx, child)
	if err != nil {
		return nil, fmt.Errorf("copying live-fork metadata: %w", err)
	}
	childLoaded = true
	if opts.Metrics != nil {
		opts.Metrics.MetadataStream = time.Since(metadataStarted)
		opts.Metrics.MetadataRecord = stats.Record
		opts.Metrics.MetadataObjects = int64(stats.Objects)
	}

	restoreStarted := time.Now()
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if err := child.restoreLiveForkNetworkStacks(networkConfigSource); err != nil {
		child.Release()
		return nil, err
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS live-fork restore failed", err)
	}
	child.Pause()
	if opts.Metrics != nil {
		opts.Metrics.ChildRestore = time.Since(restoreStarted)
	}
	return child, nil
}

// validateLiveForkFilesystems rejects every mounted filesystem that has not
// explicitly opted into cloning, or whose external resource is not authorized.
// It runs before any live-fork CoW or VFS save mutation.
func (k *Kernel) validateLiveForkFilesystems(ctx context.Context, allowShared bool) error {
	var issues []LiveForkResourceIssue
	for _, eligibility := range k.vfs.LiveForkFilesystemEligibilities(ctx) {
		reason := ""
		switch eligibility.Decision {
		case vfs.LiveForkFilesystemClone:
			continue
		case vfs.LiveForkFilesystemShareExternal:
			if allowShared {
				continue
			}
			reason = "sharing an external filesystem resource was not authorized"
		case vfs.LiveForkFilesystemReject:
			reason = eligibility.Reason
		default:
			reason = fmt.Sprintf("invalid live-fork decision %q", eligibility.Decision)
		}
		resource := fmt.Sprintf("mount %d filesystem %q identity %q", eligibility.MountID, eligibility.FilesystemType, eligibility.StableIdentity)
		if eligibility.MountPath != "" {
			resource = fmt.Sprintf("mount %q (id %d) filesystem %q identity %q", eligibility.MountPath, eligibility.MountID, eligibility.FilesystemType, eligibility.StableIdentity)
		}
		if !eligibility.Mounted {
			resource = fmt.Sprintf("referenced filesystem %q identity %q", eligibility.FilesystemType, eligibility.StableIdentity)
		}
		if eligibility.Reason != "" && reason != eligibility.Reason {
			reason = fmt.Sprintf("%s: %s", reason, eligibility.Reason)
		}
		issues = append(issues, LiveForkResourceIssue{
			PID:            0,
			FD:             -1,
			MountPath:      eligibility.MountPath,
			FilesystemType: eligibility.FilesystemType,
			SourceClass:    eligibility.SourceClass,
			Resource:       resource,
			Reason:         reason,
		})
	}
	if len(issues) == 0 {
		return nil
	}
	return &LiveForkResourceError{Issues: issues}
}

// validateLiveForkNetwork verifies that stack and its link graph are contained
// within the Sentry.
func (k *Kernel) validateLiveForkNetwork(stack inet.Stack) error {
	validator, ok := stack.(inet.LiveForkStackValidator)
	if !ok {
		return fmt.Errorf("live fork network stack %T has not opted into containment validation", stack)
	}
	if _, ok := stack.(inet.LiveForkDanglingEndpointOwner); !ok {
		return fmt.Errorf("live fork network stack %T cannot identify branch-owned dangling endpoints", stack)
	}
	if err := validator.ValidateLiveFork(); err != nil {
		return fmt.Errorf("live fork network containment validation failed: %w", err)
	}
	return nil
}

// pauseLiveForkNetworkStacks pauses every stack owned by k's network namespace
// family. The complete set is tracked independently of tasks so namespaces
// retained only by nsfs descriptors are included as well. k must be paused and
// protected by quiescePausedAnd.
func (k *Kernel) pauseLiveForkNetworkStacks(allow, validate bool) (func(), error) {
	if k.rootNetworkNamespace == nil {
		return func() {}, nil
	}
	var paused []inet.Stack
	resume := func() {
		for i := len(paused) - 1; i >= 0; i-- {
			if saver, ok := paused[i].(inet.LiveForkStackConfigSaver); ok {
				saver.SetLiveForkSaveConfig(false)
			}
			paused[i].Resume()
		}
	}
	seen := make(map[inet.Stack]struct{})
	for _, ns := range k.rootNetworkNamespace.RelatedNamespaces() {
		stack := ns.Stack()
		if stack == nil {
			continue
		}
		if _, ok := seen[stack]; ok {
			continue
		}
		seen[stack] = struct{}{}
		if !allow {
			resume()
			return func() {}, fmt.Errorf("live fork does not support this container's network mode")
		}
		stack.SetRemoveConf(false)
		if saver, ok := stack.(inet.LiveForkStackConfigSaver); ok {
			saver.SetLiveForkSaveConfig(true)
		}
		stack.Pause()
		paused = append(paused, stack)
		if validate {
			if err := k.validateLiveForkNetwork(stack); err != nil {
				resume()
				return func() {}, err
			}
		}
	}
	return resume, nil
}

// restoreLiveForkNetworkStacks thaws all decoded Sentry-owned stacks. The root
// stack additionally receives runtime-managed TTC NIC configuration, which is
// intentionally reconstructed rather than borrowed from the source branch.
func (k *Kernel) restoreLiveForkNetworkStacks(rootConfigSource inet.Stack) error {
	if k.rootNetworkNamespace == nil {
		return nil
	}
	seen := make(map[inet.Stack]struct{})
	for _, ns := range k.rootNetworkNamespace.RelatedNamespaces() {
		stack := ns.Stack()
		if stack == nil {
			continue
		}
		if _, ok := seen[stack]; ok {
			continue
		}
		seen[stack] = struct{}{}
		if restorer, ok := stack.(inet.LiveForkStackConfigRestorer); ok {
			if err := restorer.PrepareLiveForkRestore(); err != nil {
				return fmt.Errorf("preparing live-fork network configuration: %w", err)
			}
		}
		if ns.IsRoot() {
			if rootConfigSource == nil {
				return fmt.Errorf("live fork network configuration source is unavailable")
			}
			// Said before the links are rebuilt, because it decides what they
			// are rebuilt as.
			if quarantinable, ok := stack.(inet.LiveForkQuarantinable); ok {
				quarantinable.SetLiveForkQuarantine(k.liveForkQuarantine)
			}
			cloner, ok := rootConfigSource.(inet.LiveForkStackConfigCloner)
			if !ok {
				return fmt.Errorf("live fork network stack %T cannot clone unsaved configuration", rootConfigSource)
			}
			if err := cloner.CloneLiveForkConfig(stack); err != nil {
				return fmt.Errorf("cloning live-fork network configuration: %w", err)
			}
		}
		stack.Restore()
		if resetter, ok := stack.(inet.LiveForkStackChildResetter); ok {
			if err := resetter.ResetLiveForkChild(); err != nil {
				return fmt.Errorf("resetting child external network state: %w", err)
			}
		}
	}
	return nil
}

// restoreReconfiguredNetworkStacks restores a decoded network namespace graph
// when the root's host-facing TTC NIC must be recreated by the destination
// runtime (distributed fork and rehydration). Secondary namespace stacks retain
// their serialized, Sentry-owned configuration.
func (k *Kernel) restoreReconfiguredNetworkStacks(configureRoot func(inet.Stack) error) error {
	if k.rootNetworkNamespace == nil {
		return nil
	}
	seen := make(map[inet.Stack]struct{})
	for _, ns := range k.rootNetworkNamespace.RelatedNamespaces() {
		stack := ns.Stack()
		if stack == nil {
			continue
		}
		if _, ok := seen[stack]; ok {
			continue
		}
		seen[stack] = struct{}{}
		if restorer, ok := stack.(inet.LiveForkStackConfigRestorer); ok {
			if err := restorer.PrepareLiveForkRestore(); err != nil {
				return fmt.Errorf("preparing restored network configuration: %w", err)
			}
		}
		if ns.IsRoot() {
			if configureRoot == nil {
				return errors.New("network root configurator was not provided")
			}
			if err := configureRoot(stack); err != nil {
				return err
			}
		}
		stack.Restore()
		if resetter, ok := stack.(inet.LiveForkStackChildResetter); ok {
			if err := resetter.ResetLiveForkChild(); err != nil {
				return fmt.Errorf("resetting child external network state: %w", err)
			}
		}
	}
	return nil
}

// LiveForkQuarantine reports whether this Kernel's runtime-managed external
// network edge is quarantined.
func (k *Kernel) LiveForkQuarantine() bool {
	return k.liveForkQuarantine
}

// SetLiveForkQuarantine changes quarantine for a paused Kernel. It changes
// only the runtime-owned root link; all Sentry-owned network state remains in
// place. Callers are responsible for pausing and resuming tasks.
func (k *Kernel) SetLiveForkQuarantine(quarantine bool) error {
	if k.liveForkQuarantine == quarantine {
		return nil
	}
	if k.rootNetworkNamespace == nil || k.rootNetworkNamespace.Stack() == nil {
		k.liveForkQuarantine = quarantine
		return nil
	}
	stack := k.rootNetworkNamespace.Stack()
	controller, ok := stack.(inet.LiveForkQuarantineController)
	if !ok {
		return fmt.Errorf("network stack %T cannot change TTC quarantine", stack)
	}
	stack.Pause()
	err := controller.SetLiveForkQuarantineState(quarantine)
	stack.Resume()
	if err != nil {
		return err
	}
	k.liveForkQuarantine = quarantine
	return nil
}

// validateLiveForkMappings walks each unique task address space and rejects
// special or host-backed VMAs that cannot yet be made branch-local. It runs
// before any live-fork CoW mutation.
func (k *Kernel) validateLiveForkMappings() error {
	type memoryRoot struct {
		pid ThreadID
		mm  *mm.MemoryManager
	}
	var roots []memoryRoot
	k.tasks.mu.RLock()
	for task, tid := range k.tasks.Root.tids {
		if task.image.MemoryManager != nil {
			roots = append(roots, memoryRoot{tid, task.image.MemoryManager})
		}
		if r, ok := task.runState.(*runExecveAfterSiblingExitStop); ok && r.image.MemoryManager != nil {
			roots = append(roots, memoryRoot{tid, r.image.MemoryManager})
		}
	}
	k.tasks.mu.RUnlock()
	sort.Slice(roots, func(i, j int) bool { return roots[i].pid < roots[j].pid })

	seen := make(map[*mm.MemoryManager]struct{})
	var issues []LiveForkResourceIssue
	for _, root := range roots {
		if _, ok := seen[root.mm]; ok {
			continue
		}
		seen[root.mm] = struct{}{}
		for _, issue := range root.mm.ValidateLiveForkMappings() {
			issues = append(issues, LiveForkResourceIssue{
				PID:      root.pid,
				FD:       -1,
				Resource: issue.Resource,
				Reason:   fmt.Sprintf("virtual address range %v: %s", issue.AddrRange, issue.Reason),
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].PID != issues[j].PID {
			return issues[i].PID < issues[j].PID
		}
		if issues[i].Resource != issues[j].Resource {
			return issues[i].Resource < issues[j].Resource
		}
		return issues[i].Reason < issues[j].Reason
	})
	return &LiveForkResourceError{Issues: issues}
}

// prepareSharedMemoryForLiveFork establishes page CoW for every SysV SHM
// registry reachable from a task, including the root IPC namespace. Tasks are
// paused, so their ipcns pointers are stable without Task.mu.
func (k *Kernel) prepareSharedMemoryForLiveFork(ctx context.Context) {
	namespaces := make(map[*IPCNamespace]struct{})
	if k.rootIPCNamespace != nil {
		namespaces[k.rootIPCNamespace] = struct{}{}
	}
	k.tasks.mu.RLock()
	for task := range k.tasks.Root.tids {
		if task.ipcns != nil {
			namespaces[task.ipcns] = struct{}{}
		}
	}
	k.tasks.mu.RUnlock()
	for ns := range namespaces {
		ns.shms.PrepareLiveFork(ctx)
	}
}

// captureSharedMemoryRanges records every SysV SHM registry reachable from
// the Kernel state graph. RecordOwned deduplicates segments also reached via a
// task VMA.
func (k *Kernel) captureSharedMemoryRanges(ctx context.Context, capture *pgalloc.MemoryRangeCapture) error {
	namespaces := make(map[*IPCNamespace]struct{})
	if k.rootIPCNamespace != nil {
		namespaces[k.rootIPCNamespace] = struct{}{}
	}
	k.tasks.mu.RLock()
	for task := range k.tasks.Root.tids {
		if task.ipcns != nil {
			namespaces[task.ipcns] = struct{}{}
		}
	}
	k.tasks.mu.RUnlock()
	for ns := range namespaces {
		if err := ns.shms.CaptureMemoryFileRanges(ctx, capture); err != nil {
			return err
		}
	}
	return nil
}

// captureFilesystemMemory records MemoryFile ownership held by serialized
// filesystems independently of VMAs and file descriptions.
func (k *Kernel) captureFilesystemMemory(ctx context.Context, capture *pgalloc.MemoryRangeCapture) error {
	filesystems := k.vfs.GetFilesystems()
	defer func() {
		for _, fs := range filesystems {
			fs.DecRef(ctx)
		}
	}()
	for _, fs := range filesystems {
		if err := tmpfs.CaptureMemoryFileRanges(fs, capture); err != nil {
			return fmt.Errorf("capturing filesystem %T memory: %w", fs.Impl(), err)
		}
		if capturer, ok := fs.Impl().(vfs.MemoryFileRangeCapturer); ok {
			if err := capturer.CaptureMemoryFileRanges(capture); err != nil {
				return fmt.Errorf("capturing filesystem %T memory: %w", fs.Impl(), err)
			}
		}
	}
	return nil
}

// captureKernelOwnedMemory records MemoryFile ownership rooted directly in
// Kernel and Task objects rather than mappings, descriptors, or filesystems.
func (k *Kernel) captureKernelOwnedMemory(capture *pgalloc.MemoryRangeCapture) error {
	if k.vdso != nil {
		if err := k.vdso.CaptureMemoryFileRanges(capture); err != nil {
			return fmt.Errorf("capturing VDSO memory: %w", err)
		}
	}
	seenKcov := make(map[*Kcov]struct{})
	k.tasks.mu.RLock()
	for task := range k.tasks.Root.tids {
		kcov := task.kcov
		if kcov == nil {
			continue
		}
		if _, ok := seenKcov[kcov]; ok {
			continue
		}
		seenKcov[kcov] = struct{}{}
	}
	k.tasks.mu.RUnlock()
	for kcov := range seenKcov {
		if err := kcov.CaptureMemoryFileRanges(capture); err != nil {
			return err
		}
	}
	return nil
}

// validateLiveForkResources walks all descriptor-table roots and any
// file-description edges exposed by vfs.LiveForkFileDescription. It runs
// before VFS save hooks and before private mappings are transitioned to CoW.
// Filesystem-backed descriptions are validated by VFS.PrepareSave; anonymous,
// socket, and inherently host-backed descriptions must opt in here.
func (k *Kernel) validateLiveForkResources() error {
	type tableRoot struct {
		pid   ThreadID
		table *FDTable
	}
	k.tasks.mu.RLock()
	roots := make([]tableRoot, 0, len(k.tasks.Root.tids))
	for task, tid := range k.tasks.Root.tids {
		if table := task.FDTable(); table != nil {
			roots = append(roots, tableRoot{pid: tid, table: table})
		}
	}
	k.tasks.mu.RUnlock()
	sort.Slice(roots, func(i, j int) bool { return roots[i].pid < roots[j].pid })

	seenTables := make(map[*FDTable]struct{})
	var issues []LiveForkResourceIssue
	for _, root := range roots {
		if _, ok := seenTables[root.table]; ok {
			continue
		}
		seenTables[root.table] = struct{}{}
		root.table.mu.Lock()
		root.table.forEach(k.SupervisorContext(), func(fd int32, file *vfs.FileDescription, _ FDFlags) bool {
			visited := make(map[*vfs.FileDescription]struct{})
			var validate func(*vfs.FileDescription)
			validate = func(current *vfs.FileDescription) {
				if _, ok := visited[current]; ok {
					return
				}
				visited[current] = struct{}{}
				impl := current.Impl()
				validator, ok := impl.(vfs.LiveForkFileDescription)
				if !ok {
					fsType := current.Mount().Filesystem().FilesystemType().Name()
					switch fsType {
					case "none", "sockfs":
						issues = append(issues, LiveForkResourceIssue{PID: root.pid, FD: fd, Resource: fmt.Sprintf("%T", impl), Reason: "anonymous or socket resource has not opted in"})
					case "host", "fuse":
						issues = append(issues, LiveForkResourceIssue{PID: root.pid, FD: fd, Resource: fmt.Sprintf("%T", impl), Reason: fmt.Sprintf("filesystem %q is host-backed", fsType)})
					}
					return
				}
				referenced, err := validator.ValidateLiveFork()
				if err != nil {
					issues = append(issues, LiveForkResourceIssue{PID: root.pid, FD: fd, Resource: fmt.Sprintf("%T", impl), Reason: err.Error()})
					return
				}
				for _, next := range referenced {
					validate(next)
				}
			}
			validate(file)
			return true
		})
		root.table.mu.Unlock()
	}
	if len(issues) == 0 {
		return nil
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].PID != issues[j].PID {
			return issues[i].PID < issues[j].PID
		}
		if issues[i].FD != issues[j].FD {
			return issues[i].FD < issues[j].FD
		}
		if issues[i].Resource != issues[j].Resource {
			return issues[i].Resource < issues[j].Resource
		}
		return issues[i].Reason < issues[j].Reason
	})
	return &LiveForkResourceError{Issues: issues}
}

// privatizeLiveForkVDSOParamPage gives the child Timekeeper a branch-private
// VVAR page. The VDSO code remains shared and immutable.
func (k *Kernel) privatizeLiveForkVDSOParamPage() error {
	fr, err := k.mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.System,
		Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		return fmt.Errorf("allocating live-fork VDSO parameter page: %w", err)
	}
	blocks, mappingLease, err := memmap.MapInternalWithLease(k.mf, fr, hostarch.ReadWrite)
	if err != nil {
		k.mf.DecRef(fr)
		return fmt.Errorf("mapping live-fork VDSO parameter page: %w", err)
	}
	if mappingLease != nil {
		defer mappingLease.Release()
	}
	if n, err := safemem.ZeroSeq(blocks); err != nil {
		k.mf.DecRef(fr)
		return fmt.Errorf("zeroing live-fork VDSO parameter page after %d bytes: %w", n, err)
	} else if n != hostarch.PageSize {
		k.mf.DecRef(fr)
		return fmt.Errorf("zeroing live-fork VDSO parameter page wrote %d bytes, want %d", n, hostarch.PageSize)
	}
	k.vdso.ParamPage.RebindMemoryFileRange(k.mf, fr)
	// Rebinding the SpecialMappable changes future translations, but restored
	// address spaces may already have PMAs and host mappings for the fork-time
	// page. Drop those cached translations before the target starts. Otherwise
	// vDSO clock reads continue using source-host calibration even though the
	// child Timekeeper updates the new target-local page.
	seen := make(map[*mm.MemoryManager]struct{})
	invalidate := func(memMgr *mm.MemoryManager) {
		if memMgr == nil {
			return
		}
		if _, ok := seen[memMgr]; ok {
			return
		}
		seen[memMgr] = struct{}{}
		memMgr.InvalidateMappingsForMappable(k.vdso.ParamPage)
	}
	k.tasks.mu.RLock()
	for task := range k.tasks.Root.tids {
		invalidate(task.image.MemoryManager)
		if r, ok := task.runState.(*runExecveAfterSiblingExitStop); ok {
			invalidate(r.image.MemoryManager)
		}
	}
	k.tasks.mu.RUnlock()
	// Do not mutate the deserialized wrapper in place. State graphs created by
	// older TTC branches may contain a stale cross-branch wrapper reference,
	// and VDSO sequence state is an execution cache rather than branch state.
	k.vdsoParams = NewVDSOParamPage(k.mf, fr)
	return nil
}

// prepareMemoryManagersForLiveFork transitions all private PMAs to COW.
// Tasks are paused, so Task.image and runState may be read without Task.mu.
// The returned function, which must be called even if it fails, clears what
// it prepared.
func (k *Kernel) prepareMemoryManagersForLiveFork() (mm.LiveForkPMASnapshots, func(), error) {
	prepared := make(map[*mm.MemoryManager]struct{})
	snapshots := make(mm.LiveForkPMASnapshots)
	nextKey := uint64(1)
	prepare := func(memMgr *mm.MemoryManager) error {
		if memMgr == nil {
			return nil
		}
		if _, ok := prepared[memMgr]; ok {
			return nil
		}
		snapshot, err := memMgr.PrepareLiveForkSnapshot(nextKey)
		if err != nil {
			return err
		}
		snapshots[nextKey] = snapshot
		nextKey++
		prepared[memMgr] = struct{}{}
		return nil
	}
	clear := func() {
		for memMgr := range prepared {
			memMgr.ClearLiveForkPMASnapshot()
		}
	}

	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t := range k.tasks.Root.tids {
		if err := prepare(t.image.MemoryManager); err != nil {
			return nil, clear, err
		}
		if r, ok := t.runState.(*runExecveAfterSiblingExitStop); ok {
			if err := prepare(r.image.MemoryManager); err != nil {
				return nil, clear, err
			}
		}
	}
	return snapshots, clear, nil
}

// prepareMemoryManagersForDistributedFork establishes CoW while leaving PMAs
// in the ordinary state stream, which is required when the loader cannot share
// in-process LiveForkPMASnapshot pointers.
func (k *Kernel) prepareMemoryManagersForDistributedFork(capture *pgalloc.MemoryRangeCapture) error {
	prepared := make(map[*mm.MemoryManager]struct{})
	prepare := func(memMgr *mm.MemoryManager) error {
		if memMgr == nil {
			return nil
		}
		if _, ok := prepared[memMgr]; ok {
			return nil
		}
		memMgr.PrepareDistributedForkCOW()
		if err := memMgr.CaptureMemoryFileRanges(capture); err != nil {
			return err
		}
		prepared[memMgr] = struct{}{}
		return nil
	}
	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t := range k.tasks.Root.tids {
		if err := prepare(t.image.MemoryManager); err != nil {
			return err
		}
		if r, ok := t.runState.(*runExecveAfterSiblingExitStop); ok {
			if err := prepare(r.image.MemoryManager); err != nil {
				return err
			}
		}
	}
	return nil
}

// captureFileDescriptionMemory records MemoryFile ranges owned by every file
// description reachable from task FD tables, including recursively referenced
// descriptions such as epoll interests. Shared descriptions are captured once.
func (k *Kernel) captureFileDescriptionMemory(capture *pgalloc.MemoryRangeCapture) error {
	type tableRoot struct {
		pid   ThreadID
		table *FDTable
	}
	var roots []tableRoot
	k.tasks.mu.RLock()
	for task, tid := range k.tasks.Root.tids {
		if table := task.FDTable(); table != nil {
			roots = append(roots, tableRoot{pid: tid, table: table})
		}
	}
	k.tasks.mu.RUnlock()
	sort.Slice(roots, func(i, j int) bool { return roots[i].pid < roots[j].pid })

	visitedTables := make(map[*FDTable]struct{})
	visitedFiles := make(map[*vfs.FileDescription]struct{})
	var captureFile func(*vfs.FileDescription) error
	captureFile = func(file *vfs.FileDescription) error {
		if _, ok := visitedFiles[file]; ok {
			return nil
		}
		visitedFiles[file] = struct{}{}
		impl := file.Impl()
		if capturer, ok := impl.(vfs.MemoryFileRangeCapturer); ok {
			if err := capturer.CaptureMemoryFileRanges(capture); err != nil {
				return fmt.Errorf("capturing file description %T memory: %w", impl, err)
			}
		}
		if validator, ok := impl.(vfs.LiveForkFileDescription); ok {
			referenced, err := validator.ValidateLiveFork()
			if err != nil {
				return fmt.Errorf("enumerating file description %T references: %w", impl, err)
			}
			for _, next := range referenced {
				if err := captureFile(next); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		if _, ok := visitedTables[root.table]; ok {
			continue
		}
		visitedTables[root.table] = struct{}{}
		var captureErr error
		root.table.mu.Lock()
		root.table.forEach(k.SupervisorContext(), func(_ int32, file *vfs.FileDescription, _ FDFlags) bool {
			if err := captureFile(file); err != nil {
				captureErr = err
				return false
			}
			return true
		})
		root.table.mu.Unlock()
		if captureErr != nil {
			return captureErr
		}
	}
	return nil
}

// streamLiveForkState makes child a copy of parent's state, in memory: the
// child is in the same process, so that its state need not be encoded.
func streamLiveForkState(saveCtx, loadCtx context.Context, parent, child *Kernel, metrics *LiveForkMetrics) error {
	stats, err := parent.cloneStateGraph(saveCtx, loadCtx, child)
	if err != nil {
		return fmt.Errorf("cloning live-fork metadata: %w", err)
	}
	if metrics != nil {
		metrics.MetadataRecord = stats.Record
		metrics.MetadataObjects = int64(stats.Objects)
	}
	return nil
}
