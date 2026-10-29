// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// DistributedForkCaptureOptions controls portable source capture.
type DistributedForkCaptureOptions struct {
	ChildBranchID string
	// ChildBranchIDs requests sibling filesystem and memory snapshots from
	// one paused instant. When non-empty, ChildBranchID must be empty or equal
	// the first element. A singular caller may continue using ChildBranchID.
	ChildBranchIDs                 []string
	AllowSharedFilesystemResources bool
	FilesystemOrigins              vfs.LiveForkFilesystemOrigins
}

// DistributedForkCapture is the source-side product of a distributed fork.
// Snapshot owns source page references and must be closed if no target takes
// responsibility for completing or canceling the transfer.
type DistributedForkCapture struct {
	KernelMetadata []byte
	// AllocatorMetadata is retained for the legacy no-binding loader. New
	// captures always describe the branch-scoped graph with MemoryBindings and
	// leave this empty; serializing the family-wide allocator would retain
	// unrelated branches and sparse-chunk cache holes across migrations.
	AllocatorMetadata []byte
	LoadRanges        []pgalloc.RemoteLoadRange
	// MemoryBindings is the exact branch-scoped page graph. LoadRanges describes
	// the same pages in their chunk-ordered packed stream. AllocatorMetadata is
	// retained for wire compatibility; relocated restore uses MemoryBindings.
	MemoryBindings []pgalloc.CapturedPageBinding
	Snapshot       *pgalloc.RemoteSnapshot
	Snapshots      []*pgalloc.RemoteSnapshot
	// SparseSnapshot independently pins exactly MemoryBindings for callers that
	// publish a durable branch image instead of transferring the live stream.
	SparseSnapshot  *pgalloc.RemoteSnapshot
	SparseSnapshots []*pgalloc.RemoteSnapshot
	// HandoffSnapshot mirrors the branch-scoped TCP stream exactly. It is a
	// second fork-time reservation used to publish a reader-compatible sparse
	// manifest after a live target is already visible.
	HandoffSnapshot  *pgalloc.RemoteSnapshot
	HandoffSnapshots []*pgalloc.RemoteSnapshot
	// HandoffBindings is an alias of MemoryBindings retained in the in-process
	// API while callers transition to the branch-scoped name.
	HandoffBindings []pgalloc.CapturedPageBinding

	// FilesystemResources contains process-local handles. A same-process target
	// may use them directly; a remote ttcd reopens equivalent handles from its
	// shared repository before calling LoadDistributedFork.
	FilesystemResources vfs.LiveForkFilesystemResources
}

// Close cancels all outstanding remote pages.
func (c *DistributedForkCapture) Close() error {
	if c == nil {
		return nil
	}
	var result error
	if c.Snapshot != nil {
		result = errors.Join(result, c.Snapshot.Close())
		c.Snapshot = nil
	}
	for i, snapshot := range c.Snapshots {
		if snapshot != nil {
			result = errors.Join(result, snapshot.Close())
			c.Snapshots[i] = nil
		}
	}
	if c.SparseSnapshot != nil {
		result = errors.Join(result, c.SparseSnapshot.Close())
		c.SparseSnapshot = nil
	}
	for i, snapshot := range c.SparseSnapshots {
		if snapshot != nil {
			result = errors.Join(result, snapshot.Close())
			c.SparseSnapshots[i] = nil
		}
	}
	if c.HandoffSnapshot != nil {
		result = errors.Join(result, c.HandoffSnapshot.Close())
		c.HandoffSnapshot = nil
	}
	for i, snapshot := range c.HandoffSnapshots {
		if snapshot != nil {
			result = errors.Join(result, snapshot.Close())
			c.HandoffSnapshots[i] = nil
		}
	}
	return result
}

// CaptureDistributedFork captures portable Kernel and allocator metadata while
// pinning page contents in a RemoteSnapshot. No page contents are serialized.
// The source remains paused on return and may be resumed immediately.
func (k *Kernel) CaptureDistributedFork(ctx context.Context, opts DistributedForkCaptureOptions) (*DistributedForkCapture, error) {
	if !k.IsPaused() {
		return nil, fmt.Errorf("distributed fork capture requires a paused kernel")
	}
	childBranchIDs := opts.ChildBranchIDs
	if len(childBranchIDs) == 0 {
		childBranchIDs = []string{opts.ChildBranchID}
	} else if opts.ChildBranchID != "" && opts.ChildBranchID != childBranchIDs[0] {
		return nil, fmt.Errorf("distributed fork primary branch %q differs from first sibling %q", opts.ChildBranchID, childBranchIDs[0])
	}
	seenBranchIDs := make(map[string]struct{}, len(childBranchIDs))
	for i, id := range childBranchIDs {
		if id == "" {
			return nil, fmt.Errorf("distributed fork capture child %d has an empty branch ID", i)
		}
		if _, ok := seenBranchIDs[id]; ok {
			return nil, fmt.Errorf("distributed fork capture repeats child branch ID %q", id)
		}
		seenBranchIDs[id] = struct{}{}
	}
	k.tasks.waitForExternalStop()
	k.ReceiveTaskStates()
	var capture *DistributedForkCapture
	err := k.quiescePausedAnd(ctx, func() (retErr error) {
		if err := k.flushPlatformForStateCapture(ctx); err != nil {
			return err
		}
		resumeNetwork, err := k.pauseLiveForkNetworkStacks(true, true)
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
		k.prepareSharedMemoryForLiveFork(ctx)
		if err := k.invalidateUnsavableMappings(ctx); err != nil {
			return fmt.Errorf("invalidating unsavable mappings for distributed fork: %w", err)
		}

		mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
		memoryCapture := pgalloc.NewMemoryRangeCapture()
		fsResources := make(vfs.LiveForkFilesystemResources)
		fsTransactions := &vfs.LiveForkTransactions{}
		committed := false
		defer func() {
			if committed {
				return
			}
			if err := fsTransactions.Abort(); err != nil && retErr == nil {
				retErr = fmt.Errorf("aborting distributed-fork filesystem transactions: %w", err)
			}
		}()
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           mfs,
			pgalloc.CtxMemoryRangeCapture:      memoryCapture,
			pgalloc.CtxLiveForkSource:          true,
			vfs.CtxLiveForkFilesystemResources: fsResources,
			vfs.CtxLiveForkChildBranchID:       childBranchIDs[0],
			vfs.CtxLiveForkChildBranchIDs:      childBranchIDs,
			vfs.CtxLiveForkTransactions:        fsTransactions,
			vfs.CtxLiveForkFilesystemOrigins:   opts.FilesystemOrigins,
		})
		if err := k.vfs.PrepareSave(saveCtx, vfs.PrepareSaveOptions{}); err != nil {
			return fmt.Errorf("preparing VFS for distributed fork: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		if len(mfs) != 0 {
			return fmt.Errorf("distributed fork does not yet support %d private filesystem MemoryFiles", len(mfs))
		}

		if err := k.prepareMemoryManagersForDistributedFork(memoryCapture); err != nil {
			return fmt.Errorf("capturing distributed-fork memory graph: %w", err)
		}
		if err := k.captureFileDescriptionMemory(memoryCapture); err != nil {
			return fmt.Errorf("capturing distributed-fork file memory graph: %w", err)
		}
		if err := k.captureSharedMemoryRanges(ctx, memoryCapture); err != nil {
			return fmt.Errorf("capturing distributed-fork SysV SHM graph: %w", err)
		}
		if err := k.captureFilesystemMemory(ctx, memoryCapture); err != nil {
			return fmt.Errorf("capturing distributed-fork filesystem memory graph: %w", err)
		}
		if err := k.captureKernelOwnedMemory(memoryCapture); err != nil {
			return fmt.Errorf("capturing distributed-fork kernel memory graph: %w", err)
		}
		k.mf.MarkSavable()
		// A 2 MiB page travels whole, and its target installs it whole.
		layout, err := memoryCapture.HugePageSnapshotLayout()
		if err != nil {
			return fmt.Errorf("resolving sparse distributed-fork memory graph: %w", err)
		}
		// The target installs the pages the bindings name. Only a capture
		// without any needs the family's allocator, whose export holds the
		// family MemoryFile's lock while it serializes all of it, and first
		// evicts every branch's evictable file cache.
		var allocatorMetadata []byte
		if len(layout.Bindings) == 0 {
			if allocatorMetadata, _, _, err = pgalloc.ExportMemoryFileMetadataForRemote(ctx, k.mf); err != nil {
				return err
			}
		}
		layout, err = layout.ChunkOrdered()
		if err != nil {
			return fmt.Errorf("ordering distributed-fork memory chunks: %w", err)
		}
		loads := make([]pgalloc.RemoteLoadRange, len(layout.Regions))
		var streamOffset uint64
		for i, region := range layout.Regions {
			loads[i] = pgalloc.RemoteLoadRange{Target: region.Range, StreamOffset: streamOffset}
			streamOffset += region.Range.Length()
		}
		// Keep independent reservations for the live stream, direct sparse
		// publication, and asynchronous shared-filesystem handoff. All three are
		// branch-scoped: allocator-wide snapshots can race sibling destruction and
		// retain pages that the restored state graph can never reference.
		snapshots, err := pgalloc.NewRemoteSnapshots(layout.Regions, len(childBranchIDs))
		if err != nil {
			return err
		}
		keepSnapshots := false
		defer func() {
			if !keepSnapshots {
				for _, snapshot := range snapshots {
					_ = snapshot.Close()
				}
			}
		}()
		sparseSnapshots, err := pgalloc.NewRemoteSnapshots(layout.Regions, len(childBranchIDs))
		if err != nil {
			return err
		}
		keepSparseSnapshots := false
		defer func() {
			if !keepSparseSnapshots {
				for _, snapshot := range sparseSnapshots {
					_ = snapshot.Close()
				}
			}
		}()
		handoffSnapshots, err := pgalloc.NewRemoteSnapshots(layout.Regions, len(childBranchIDs))
		if err != nil {
			return err
		}
		keepHandoffSnapshots := false
		defer func() {
			if !keepHandoffSnapshots {
				for _, snapshot := range handoffSnapshots {
					_ = snapshot.Close()
				}
			}
		}()
		// A remotely backed source may be destroyed as soon as migration cuts
		// over, while these snapshots must remain readable until their targets
		// become independent. Retain the upstream loaders in each independently
		// owned snapshot group so page pins also retain their data source.
		pageLoads := k.distributedForkMemorySources()
		for _, group := range [][]*pgalloc.RemoteSnapshot{snapshots, sparseSnapshots, handoffSnapshots} {
			if err := group[0].RetainPageLoads(pageLoads); err != nil {
				return fmt.Errorf("retaining distributed-fork upstream page sources: %w", err)
			}
		}
		var kernelMetadata bytes.Buffer
		buffered := bufio.NewWriterSize(&kernelMetadata, 64<<10)
		if _, err := k.saveStateGraph(saveCtx, buffered, false); err != nil {
			return fmt.Errorf("saving distributed Kernel metadata: %w", err)
		}
		if err := buffered.Flush(); err != nil {
			return err
		}
		// The state stream and remote snapshot refer to the fork-time VVAR
		// page. resumeTimeLocked updates the source Timekeeper after this
		// callback returns, so first move the source onto a new parameter page.
		// Otherwise the source overwrites the supposedly immutable page before
		// the remote target can read it.
		if err := k.privatizeLiveForkVDSOParamPage(); err != nil {
			return fmt.Errorf("privatizing distributed-fork source VDSO parameter page: %w", err)
		}
		if err := fsTransactions.Commit(); err != nil {
			return fmt.Errorf("committing distributed-fork filesystem transactions: %w", err)
		}
		committed = true
		capture = &DistributedForkCapture{
			KernelMetadata: kernelMetadata.Bytes(), AllocatorMetadata: allocatorMetadata,
			LoadRanges: loads, MemoryBindings: layout.Bindings, HandoffBindings: layout.Bindings, FilesystemResources: fsResources,
		}
		if len(snapshots) == 1 {
			capture.Snapshot = snapshots[0]
			capture.SparseSnapshot = sparseSnapshots[0]
			capture.HandoffSnapshot = handoffSnapshots[0]
		} else {
			capture.Snapshots = snapshots
			capture.SparseSnapshots = sparseSnapshots
			capture.HandoffSnapshots = handoffSnapshots
		}
		keepSnapshots = true
		keepSparseSnapshots = true
		keepHandoffSnapshots = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return capture, nil
}

func (k *Kernel) distributedForkMemorySources() []*pgalloc.AsyncPagesFileLoad {
	k.remoteLoadsMu.Lock()
	defer k.remoteLoadsMu.Unlock()
	loads := make([]*pgalloc.AsyncPagesFileLoad, 0, 1+len(k.auxiliaryRemotePageLoads))
	if k.remotePageLoad != nil {
		loads = append(loads, k.remotePageLoad)
	}
	loads = append(loads, k.auxiliaryRemotePageLoads...)
	return loads
}

// InheritDistributedForkMemorySources retains page loaders that back sparse
// chunks reused from source's family MemoryFile. Reusing a chunk is useful
// only if its unresolved contents remain reachable: page references keep the
// allocation alive, but they do not own the asynchronous reader. The first
// inherited loader remains the branch's primary control target; additional
// loaders are dependencies inherited from earlier imports.
func (k *Kernel) InheritDistributedForkMemorySources(source *Kernel, packedLoadRanges []pgalloc.RemoteLoadRange) {
	if k == nil || source == nil || k == source {
		return
	}
	source.remoteLoadsMu.Lock()
	sourcePrimary := source.remotePageLoad
	sourceAuxiliary := append([]*pgalloc.AsyncPagesFileLoad(nil), source.auxiliaryRemotePageLoads...)
	source.remoteLoadsMu.Unlock()
	k.remoteLoadsMu.Lock()
	defer k.remoteLoadsMu.Unlock()
	has := func(load *pgalloc.AsyncPagesFileLoad) bool {
		if load == nil || k.remotePageLoad == load {
			return true
		}
		for _, existing := range k.auxiliaryRemotePageLoads {
			if existing == load {
				return true
			}
		}
		return false
	}
	inherit := func(load *pgalloc.AsyncPagesFileLoad, primary bool) {
		if has(load) {
			return
		}
		load.IncOwner()
		if primary && k.remotePageLoad == nil {
			k.remotePageLoad = load
			k.remoteLoadRanges = append([]pgalloc.RemoteLoadRange(nil), packedLoadRanges...)
			return
		}
		k.auxiliaryRemotePageLoads = append(k.auxiliaryRemotePageLoads, load)
	}
	inherit(sourcePrimary, true)
	for _, load := range sourceAuxiliary {
		inherit(load, k.remotePageLoad == nil)
	}
}

// DistributedForkLoadOptions controls target reconstruction.
type DistributedForkLoadOptions struct {
	Clocks                     sentrytime.Clocks
	ChildBranchID              string
	Quarantined                bool
	OverrideQuarantine         bool
	RestoreOptions             vfs.CompleteRestoreOptions
	FilesystemResources        vfs.LiveForkFilesystemResources
	RestoreFilesystemResources vfs.RestoreFilesystemResources
	RestoreFilesystemFDMap     map[checkpoint.ResourceID]int
	DemandOnly                 bool
	PrefetchBytes              uint64
	PrefetchBehindBytes        uint64
	LogicalChunkDemand         bool

	// ConfigureNetwork recreates target-local link/NAT plumbing omitted from
	// Kernel state. It is required when the captured Kernel has a netstack.
	ConfigureNetwork func(*Kernel, inet.Stack) error
}

// LoadDistributedFork reconstructs a Kernel from portable metadata and a
// remote page reader. The Kernel takes an owner of targetMF of its own; the
// caller keeps its own, as a peer import loads into the family's MemoryFile.
// It consumes reader on both success and failure.
func LoadDistributedFork(ctx context.Context, p platform.Platform, targetMF *pgalloc.MemoryFile, kernelMetadata, allocatorMetadata []byte, loadRanges []pgalloc.RemoteLoadRange, bindings []pgalloc.CapturedPageBinding, reader stateio.AsyncReader, opts DistributedForkLoadOptions) (*Kernel, error) {
	if p == nil || targetMF == nil || reader == nil || opts.Clocks == nil || opts.ChildBranchID == "" {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, fmt.Errorf("distributed fork load is missing required target state")
	}
	if len(bindings) != 0 {
		return loadRelocatedDistributedFork(p, targetMF, kernelMetadata, loadRanges, bindings, reader, opts)
	}
	// The Kernel's owner, which the failures below release.
	targetMF.IncOwner()
	if err := pgalloc.ImportMemoryFileMetadataForRemote(ctx, targetMF, allocatorMetadata); err != nil {
		_ = reader.Close()
		targetMF.Destroy()
		return nil, err
	}
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{
		DemandOnly: opts.DemandOnly, PrefetchBytes: opts.PrefetchBytes,
		PrefetchBehindBytes: opts.PrefetchBehindBytes, LogicalChunkDemand: opts.LogicalChunkDemand,
	})
	if err != nil {
		targetMF.Destroy()
		return nil, err
	}
	child := &Kernel{Platform: p}
	child.SetMemoryFile(targetMF)
	child.initRuntimeFields()
	// Attach the remote loader before restoring Kernel metadata. Restoring
	// address spaces calls MemoryFile.DataFD while installing application
	// mappings into the platform. If the loader is attached afterwards, those
	// mappings can observe the newly allocated MemoryFile's zero-filled pages
	// without ever passing through the remote-load interlock.
	if err := targetMF.AttachRemotePageLoad(pages, loadRanges, nil, nil); err != nil {
		pages.Cancel()
		_ = pages.Wait()
		targetMF.Destroy()
		return nil, err
	}
	pages.MemoryFilesDone()
	loaded := false
	defer func() {
		if loaded {
			return
		}
		pages.Cancel()
		_ = pages.Wait()
		targetMF.Destroy()
	}()
	mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
	loadValues := map[any]any{
		pgalloc.CtxMemoryFile:                targetMF,
		pgalloc.CtxMemoryFileMap:             mfs,
		pgalloc.CtxLiveForkClone:             true,
		pgalloc.CtxLiveForkRemoteMemoryClone: true,
		vfs.CtxLiveForkChildBranchID:         opts.ChildBranchID,
		devutil.CtxDevGoferClientProvider:    child,
	}
	if opts.FilesystemResources != nil {
		loadValues[vfs.CtxLiveForkFilesystemResources] = opts.FilesystemResources
	}
	if opts.RestoreFilesystemResources != nil {
		loadValues[vfs.CtxRestoreFilesystemResources] = opts.RestoreFilesystemResources
	}
	if opts.RestoreFilesystemFDMap != nil {
		loadValues[vfs.CtxRestoreFilesystemFDMap] = opts.RestoreFilesystemFDMap
	}
	loadCtx := context.WithValues(child.SupervisorContext(), loadValues)
	if _, err := state.LoadWithOptions(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child, state.LoadOptions{NoStats: true}); err != nil {
		return nil, fmt.Errorf("loading distributed Kernel metadata: %w", err)
	}
	if opts.OverrideQuarantine {
		child.liveForkQuarantine = opts.Quarantined
	}
	child.remotePageLoad = pages
	child.remoteLoadRanges = append([]pgalloc.RemoteLoadRange(nil), loadRanges...)
	loaded = true
	// The restored VVAR mapping still refers to its fork-time remote page, but
	// privatizeLiveForkVDSOParamPage replaces that mapping with a new local
	// allocation. Materialize the old page first so its pull is acknowledged;
	// otherwise a target that never faults VVAR retains exactly one source page
	// forever and cannot complete a drain.
	if err := child.vdsoParams.materialize(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, fmt.Errorf("materializing distributed-fork VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if err := child.restoreReconfiguredNetworkStacks(func(stack inet.Stack) error {
		if opts.ConfigureNetwork == nil {
			return errors.New("distributed target network configurator was not provided")
		}
		if err := opts.ConfigureNetwork(child, stack); err != nil {
			return fmt.Errorf("configuring distributed target network: %w", err)
		}
		return nil
	}); err != nil {
		child.Release()
		return nil, err
	}
	if got := vfs.RestoreFilesystemResourcesFromContext(loadCtx); len(got) != len(opts.RestoreFilesystemResources) {
		child.Release()
		return nil, fmt.Errorf("relocated distributed restore filesystem resources changed from %d to %d during load", len(opts.RestoreFilesystemResources), len(got))
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS distributed-fork restore failed", err)
	}
	child.Pause()
	return child, nil
}

func loadRelocatedDistributedFork(p platform.Platform, targetMF *pgalloc.MemoryFile, kernelMetadata []byte, packedLoadRanges []pgalloc.RemoteLoadRange, bindings []pgalloc.CapturedPageBinding, reader stateio.AsyncReader, opts DistributedForkLoadOptions) (*Kernel, error) {
	installation, relocation, loadRanges, err := targetMF.InstallRemoteBindings(bindings, usage.Anonymous, 0)
	if err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("installing live distributed memory chunks: %w", err)
	}
	installationCommitted := false
	defer func() {
		if !installationCommitted {
			_ = installation.Abort()
		}
	}()
	var pages *pgalloc.AsyncPagesFileLoad
	if len(loadRanges) != 0 {
		pages, err = pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{
			DemandOnly: opts.DemandOnly, PrefetchBytes: opts.PrefetchBytes,
			PrefetchBehindBytes: opts.PrefetchBehindBytes, LogicalChunkDemand: true,
		})
		if err != nil {
			return nil, err
		}
		if err := targetMF.AttachSparseChunkPageLoad(pages, loadRanges, nil, nil); err != nil {
			pages.Cancel()
			_ = pages.Wait()
			return nil, err
		}
		pages.MemoryFilesDone()
	} else if err := reader.Close(); err != nil {
		return nil, err
	}
	pagesOwned := pages != nil
	defer func() {
		if pagesOwned {
			pages.Cancel()
			_ = pages.Wait()
		}
	}()
	targetMF.IncOwner()
	childOwner := true
	defer func() {
		if childOwner {
			targetMF.Destroy()
		}
	}()
	child := &Kernel{Platform: p}
	child.SetMemoryFile(targetMF)
	child.initRuntimeFields()
	references := pgalloc.NewSparseReferenceTransaction()
	loadValues := map[any]any{
		pgalloc.CtxMemoryFile: targetMF, pgalloc.CtxMemoryFileMap: make(map[checkpoint.ResourceID]*pgalloc.MemoryFile),
		pgalloc.CtxLiveForkClone: true, pgalloc.CtxSparseRelocation: relocation,
		pgalloc.CtxSparseRestoreRefs: references, vfs.CtxLiveForkChildBranchID: opts.ChildBranchID,
		devutil.CtxDevGoferClientProvider: child,
	}
	if opts.FilesystemResources != nil {
		loadValues[vfs.CtxLiveForkFilesystemResources] = opts.FilesystemResources
	}
	if opts.RestoreFilesystemResources != nil {
		loadValues[vfs.CtxRestoreFilesystemResources] = opts.RestoreFilesystemResources
	}
	if opts.RestoreFilesystemFDMap != nil {
		loadValues[vfs.CtxRestoreFilesystemFDMap] = opts.RestoreFilesystemFDMap
	}
	loadCtx := context.WithValues(child.SupervisorContext(), loadValues)
	if _, err := state.LoadWithOptions(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child, state.LoadOptions{NoStats: true}); err != nil {
		references.Abort()
		return nil, fmt.Errorf("loading relocated distributed Kernel metadata: %w", err)
	}
	if opts.OverrideQuarantine {
		child.liveForkQuarantine = opts.Quarantined
	}
	if err := references.Commit(); err != nil {
		return nil, err
	}
	// The units the installation reused may still be loading for the owners
	// that installed them, whose going would stop their loaders: the child,
	// which reads them too, keeps those loaders, as a sparse dehydrated
	// branch's restore does. Committing let them go. A branch's dehydration
	// that its Sentry kept was then its loader's last owner, and dropping it
	// once it was made durable stopped the loader with pages of units that
	// imports had reused still to load: no dehydration of theirs could
	// capture them, "materializing branch memory before dehydration: context
	// canceled".
	for _, load := range installation.TakePageLoads() {
		child.adoptPageLoad(load)
	}
	if err := installation.Commit(); err != nil {
		return nil, err
	}
	installationCommitted = true
	childOwner = false
	if pages != nil {
		child.remotePageLoad = pages
		child.remoteLoadRanges = append([]pgalloc.RemoteLoadRange(nil), packedLoadRanges...)
		pagesOwned = false
	}
	if err := child.vdsoParams.materialize(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, fmt.Errorf("materializing relocated distributed VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if err := child.restoreReconfiguredNetworkStacks(func(stack inet.Stack) error {
		if opts.ConfigureNetwork == nil {
			return errors.New("relocated distributed target network configurator was not provided")
		}
		if err := opts.ConfigureNetwork(child, stack); err != nil {
			return fmt.Errorf("configuring relocated distributed target network: %w", err)
		}
		return nil
	}); err != nil {
		child.Release()
		return nil, err
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS relocated distributed-fork restore failed", err)
	}
	child.Pause()
	return child, nil
}

// HandoffDistributedForkMemoryGraph replaces a live remote page source with
// immutable graph-native 1 MiB objects. The existing target offsets and all
// resident pages are preserved; only future faults change their source.
func (k *Kernel) HandoffDistributedForkMemoryGraph(data *ttcsnapshot.LocalDataStore, remote *ttcsnapshot.ObjectRelayClient, manifest ttcchunk.Manifest, snapshot ttcsnapshot.SnapshotID) error {
	closeInputs := func() {
		if remote != nil {
			_ = remote.Close()
		}
		if data != nil {
			_ = data.Close()
		}
	}
	if snapshot == 0 {
		closeInputs()
		return errors.New("distributed graph handoff has no snapshot identity")
	}
	if k.remoteHandoffSnapshot != 0 && k.remoteHandoffSnapshot != uint64(snapshot) {
		closeInputs()
		return fmt.Errorf("distributed memory was already handed to graph snapshot %d", k.remoteHandoffSnapshot)
	}
	// A branch may own a new loader while also referring to sparse chunks
	// supplied by an older family loader. The new transaction cannot become
	// independent until every inherited loader is either fully resident or has
	// itself switched to durable graph backing.
	k.remoteLoadsMu.Lock()
	primary := k.remotePageLoad
	inherited := append([]*pgalloc.AsyncPagesFileLoad(nil), k.auxiliaryRemotePageLoads...)
	k.remoteLoadsMu.Unlock()
	if primary == nil && len(inherited) == 0 {
		closeInputs()
		return errors.New("kernel has no active distributed remote-memory load")
	}
	for _, load := range inherited {
		if load.HasDurableBacking() {
			continue
		}
		stats := load.Stats()
		if stats.Done && stats.Error == "" {
			continue
		}
		closeInputs()
		if stats.Error != "" {
			return fmt.Errorf("inherited distributed memory load failed before graph handoff: %s", stats.Error)
		}
		return errors.New("inherited distributed memory load is not durable yet")
	}
	if primary == nil {
		// The import gave the branch no loader of its own: its pages were all
		// of units that earlier imports installed, whose loaders it inherited,
		// and that are durable or resident now. Nothing of the transaction's
		// source is left to read.
		closeInputs()
		k.remoteHandoffSnapshot = uint64(snapshot)
		return nil
	}
	if stats := primary.Stats(); stats.Done {
		closeInputs()
		if stats.Error != "" {
			return fmt.Errorf("distributed memory load stopped before graph handoff: %s", stats.Error)
		}
		k.remoteHandoffSnapshot = uint64(snapshot)
		return nil
	}
	geometry := primary.ReaderGeometry()
	if geometry.NeedsRegisterDestinationFD {
		closeInputs()
		return errors.New("distributed graph memory handoff does not support registered destination files")
	}
	packed, err := ttcchunk.NewGraphPackedManifestReader(data, remote, manifest, k.remoteLoadRanges, true)
	if err != nil {
		closeInputs()
		return err
	}
	reader := stateio.NewIOReader(packed, geometry.MaxReadBytes, geometry.MaxRanges, geometry.MaxParallel)
	if err := switchRemotePageReader(primary, reader); err != nil {
		return err
	}
	k.remoteHandoffSnapshot = uint64(snapshot)
	return nil
}

// switchRemotePageReader hands load's unresolved pages to reader, which
// SwitchReader owns even when it fails. A load that finished its last read
// after its caller looked needs no new source.
func switchRemotePageReader(load *pgalloc.AsyncPagesFileLoad, reader stateio.AsyncReader) error {
	err := load.SwitchReader(reader)
	if err == nil {
		return nil
	}
	if stats := load.Stats(); stats.Done && stats.Error == "" {
		return nil
	}
	return err
}

// SetDistributedForkPrefetchWindow changes the active remote-memory prefetch
// window for a distributed child. It affects future faults only.
func (k *Kernel) SetDistributedForkPrefetchWindow(behind, ahead uint64) error {
	if k.remotePageLoad == nil {
		return fmt.Errorf("kernel has no active distributed remote-memory load")
	}
	return k.remotePageLoad.SetPrefetchWindow(behind, ahead)
}

// TryInstallRemotePages atomically installs a proactive push unless a pull or
// another push already owns the range.
func (k *Kernel) TryInstallRemotePages(offset uint64, data []byte) (bool, error) {
	if k.remotePageLoad == nil {
		return false, fmt.Errorf("kernel has no active distributed remote-memory load")
	}
	return k.remotePageLoad.TryInstallRemotePages(offset, data)
}

// TryInstallRemotePagesState is TryInstallRemotePages with enough state for a
// transport to distinguish a pull race from a range already materialized by
// restore.
func (k *Kernel) TryInstallRemotePagesState(offset uint64, data []byte) (pgalloc.RemotePageInstallResult, error) {
	if k.remotePageLoad == nil {
		return pgalloc.RemotePageInstallBusy, fmt.Errorf("kernel has no active distributed remote-memory load")
	}
	return k.remotePageLoad.TryInstallRemotePagesState(offset, data)
}

// DistributedForkDependencyStatus reports whether this Kernel still depends
// on a remote memory source.
func (k *Kernel) DistributedForkDependencyStatus() pgalloc.AsyncPagesFileLoadStats {
	return k.pageLoadStatus(false)
}

// PageLoadStatus reports progress for every asynchronous page loader,
// including loaders backed by immutable durable graph objects. Lifecycle code
// uses this stronger status before releasing those objects; durable backing
// removes a node dependency, but it does not mean the pages were consumed.
func (k *Kernel) PageLoadStatus() pgalloc.AsyncPagesFileLoadStats {
	return k.pageLoadStatus(true)
}

func (k *Kernel) pageLoadStatus(includeDurable bool) pgalloc.AsyncPagesFileLoadStats {
	loads := k.distributedForkMemorySources()
	if len(loads) == 0 {
		return pgalloc.AsyncPagesFileLoadStats{Done: true}
	}
	result := pgalloc.AsyncPagesFileLoadStats{Done: true}
	var diagnostics []string
	for _, load := range loads {
		status := load.Stats()
		// Durable backing removes the source-node dependency, but it does not
		// make a failed immutable reader healthy. Always surface terminal reader
		// errors so a later shared-pack/relay failure cannot masquerade as a
		// successfully independent branch.
		if status.Error != "" {
			diagnostics = append(diagnostics, status.Error)
		}
		if load.HasDurableBacking() && !includeDurable {
			continue
		}
		result.Done = result.Done && status.Done
		result.DemandOnly = result.DemandOnly || status.DemandOnly
		result.BytesLoaded += status.BytesLoaded
		result.BytesOutstanding += status.BytesOutstanding
		result.BytesWaited += status.BytesWaited
		result.WaitDuration = max(result.WaitDuration, status.WaitDuration)
		result.AggregateWaitDuration += status.AggregateWaitDuration
		result.PrefetchBytes = max(result.PrefetchBytes, status.PrefetchBytes)
		result.PrefetchBehindBytes = max(result.PrefetchBehindBytes, status.PrefetchBehindBytes)
		result.Waiters += status.Waiters
		result.TotalWaiters += status.TotalWaiters
		result.Faults += status.Faults
		result.SequentialFaults += status.SequentialFaults
	}
	result.Error = strings.Join(diagnostics, "; ")
	return result
}

// DrainDistributedForkMemory eagerly materializes all remaining remote pages.
func (k *Kernel) DrainDistributedForkMemory() {
	for _, load := range k.distributedForkMemorySources() {
		if !load.HasDurableBacking() {
			load.Drain()
		}
	}
}

// TransferDistributedForkMemorySources moves ownership of unresolved remote
// readers to a surviving peer Kernel. The readers are already attached to the
// shared MemoryFiles, so transferring lifecycle ownership requires no page
// copy and keeps branch removal independent of resident-memory size.
func (k *Kernel) TransferDistributedForkMemorySources(target *Kernel) {
	if target == nil || target == k {
		return
	}
	k.remoteLoadsMu.Lock()
	var loads []*pgalloc.AsyncPagesFileLoad
	if k.remotePageLoad != nil {
		loads = append(loads, k.remotePageLoad)
		k.remotePageLoad = nil
	}
	loads = append(loads, k.auxiliaryRemotePageLoads...)
	k.auxiliaryRemotePageLoads = nil
	k.remoteLoadsMu.Unlock()
	for _, load := range loads {
		target.adoptPageLoad(load)
	}
	k.remoteLoadRanges = nil
	k.remoteHandoffSnapshot = 0
}
