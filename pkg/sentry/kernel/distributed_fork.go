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

package kernel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/hostarch"
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
)

// DistributedForkCaptureOptions controls portable source capture.
type DistributedForkCaptureOptions struct {
	ChildBranchID string
	// ChildBranchIDs requests sibling filesystem and memory snapshots from
	// one paused instant. When non-empty, ChildBranchID must be empty or equal
	// the first element. A singular caller may continue using ChildBranchID.
	ChildBranchIDs                 []string
	AllowSharedFilesystemResources bool
}

// DistributedForkCapture is the source-side product of a distributed fork.
// Snapshot owns source page references and must be closed if no target takes
// responsibility for completing or canceling the transfer.
type DistributedForkCapture struct {
	KernelMetadata    []byte
	AllocatorMetadata []byte
	LoadRanges        []pgalloc.RemoteLoadRange
	// MemoryBindings is the branch-scoped page graph. AllocatorMetadata and
	// LoadRanges remain allocator-wide until the sparse merge importer is
	// connected; new transport code must use MemoryBindings instead.
	MemoryBindings []pgalloc.CapturedPageBinding
	Snapshot       *pgalloc.RemoteSnapshot
	Snapshots      []*pgalloc.RemoteSnapshot
	// SparseSnapshot pins exactly MemoryBindings. Snapshot is retained only for
	// the legacy allocator-wide transport and must never be paired with sparse
	// branch bindings, whose packed stream has a different layout.
	SparseSnapshot  *pgalloc.RemoteSnapshot
	SparseSnapshots []*pgalloc.RemoteSnapshot
	// HandoffSnapshot mirrors the allocator-wide TCP stream exactly. It is a
	// second fork-time reservation used to publish a reader-compatible sparse
	// manifest after a live target is already visible. MemoryBindings remains
	// branch-scoped; HandoffBindings describes this compatibility stream.
	HandoffSnapshot  *pgalloc.RemoteSnapshot
	HandoffSnapshots []*pgalloc.RemoteSnapshot
	HandoffBindings  []pgalloc.CapturedPageBinding

	// FilesystemResources contains process-local handles. A same-process target
	// may use them directly; a remote ttcd reopens equivalent handles from its
	// shared repository before calling LoadDistributedFork.
	FilesystemResources vfs.LiveForkFilesystemResources
}

// PublishSparse durably publishes this capture in repository. Page
// reservations are released only after all objects and the manifest are
// durable; the coordinator subsequently installs the mutable branch root.
func (c *DistributedForkCapture) PublishSparse(repository *ttcchunk.MemoryRepository) (ttcchunk.ManifestID, error) {
	if c == nil || repository == nil || c.SparseSnapshot == nil || len(c.SparseSnapshots) != 0 {
		return ttcchunk.ManifestID{}, fmt.Errorf("sparse publication requires one live snapshot")
	}
	objects, bindings, err := ttcchunk.ReadSnapshot(c.SparseSnapshot, c.MemoryBindings)
	if err != nil {
		return ttcchunk.ManifestID{}, err
	}
	manifestID, err := repository.Publish(objects, bindings)
	if err != nil {
		return ttcchunk.ManifestID{}, err
	}
	if err := c.SparseSnapshot.Close(); err != nil {
		return ttcchunk.ManifestID{}, err
	}
	c.SparseSnapshot = nil
	return manifestID, nil
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
		if rootNS := k.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
			stack := rootNS.Stack()
			stack.SetRemoveConf(false)
			stack.Pause()
			defer stack.Resume()
			if err := k.validateLiveForkNetwork(stack); err != nil {
				return err
			}
		}
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
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
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
		allocatorMetadata, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(ctx, k.mf)
		if err != nil {
			return err
		}
		snapshots, err := pgalloc.NewRemoteSnapshots(regions, len(childBranchIDs))
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
		layout, err := memoryCapture.SnapshotLayout()
		if err != nil {
			return fmt.Errorf("resolving sparse distributed-fork memory graph: %w", err)
		}
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
		handoffSnapshots, err := pgalloc.NewRemoteSnapshots(regions, len(childBranchIDs))
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
		handoffBindings := make([]pgalloc.CapturedPageBinding, 0)
		for _, load := range loads {
			for old, stream := load.Target.Start, load.StreamOffset; old < load.Target.End; old, stream = old+hostarch.PageSize, stream+hostarch.PageSize {
				page, version, ok := k.mf.LogicalPageVersionAt(old)
				if !ok {
					return fmt.Errorf("allocator-wide distributed page %#x has no logical identity", old)
				}
				chunk, slot := page.Chunk()
				handoffBindings = append(handoffBindings, pgalloc.CapturedPageBinding{
					ResourceID: k.mf.ResourceID(), OldOffset: old, StreamOffset: stream, References: 1,
					Page: page, Version: version, Chunk: chunk, Slot: slot,
				})
			}
		}
		var kernelMetadata bytes.Buffer
		buffered := bufio.NewWriterSize(&kernelMetadata, 64<<10)
		if _, err := k.saveStateGraph(saveCtx, buffered); err != nil {
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
			LoadRanges: loads, MemoryBindings: layout.Bindings, HandoffBindings: handoffBindings, FilesystemResources: fsResources,
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

// DistributedForkLoadOptions controls target reconstruction.
type DistributedForkLoadOptions struct {
	Clocks                     sentrytime.Clocks
	ChildBranchID              string
	RestoreOptions             vfs.CompleteRestoreOptions
	FilesystemResources        vfs.LiveForkFilesystemResources
	RestoreFilesystemResources vfs.RestoreFilesystemResources
	RestoreFilesystemFDMap     map[checkpoint.ResourceID]int
	DemandOnly                 bool
	PrefetchBytes              uint64
	PrefetchBehindBytes        uint64
	LogicalChunkDemand         bool
	OwnSparseRepository        bool
	// SparseBranch names the durable root whose objects may be released after
	// their complete residency units are installed.
	SparseBranch string

	// ConfigureNetwork recreates target-local link/NAT plumbing omitted from
	// Kernel state. It is required when the captured Kernel has a netstack.
	ConfigureNetwork func(*Kernel, inet.Stack) error
}

// LoadSparseDistributedFork restores a branch into an existing family
// MemoryFile from a shared immutable chunk repository. Identical resident
// objects are reused; missing objects are demand-loaded transactionally in
// 1 MiB units.
func LoadSparseDistributedFork(ctx context.Context, p platform.Platform, targetMF *pgalloc.MemoryFile, kernelMetadata []byte, repository *ttcchunk.MemoryRepository, manifestID ttcchunk.ManifestID, opts DistributedForkLoadOptions) (*Kernel, error) {
	if p == nil || targetMF == nil || repository == nil || opts.Clocks == nil || opts.ChildBranchID == "" || len(kernelMetadata) == 0 {
		return nil, fmt.Errorf("sparse distributed fork load is missing required target state")
	}
	manifest, err := repository.LoadManifest(manifestID)
	if err != nil {
		return nil, fmt.Errorf("loading sparse branch manifest %s: %w", manifestID, err)
	}
	installation, err := repository.InstallManifest(targetMF, manifest, usage.Anonymous, 0)
	if err != nil {
		return nil, fmt.Errorf("installing sparse branch manifest: %w", err)
	}
	installationCommitted := false
	defer func() {
		if !installationCommitted {
			_ = installation.Abort()
		}
	}()

	var pages *pgalloc.AsyncPagesFileLoad
	var chunkReader *ttcchunk.ChunkReader
	consumptionActivated := false
	defer func() {
		if chunkReader != nil && !consumptionActivated {
			chunkReader.AbortConsumption()
		}
	}()
	if len(installation.Chunks) != 0 {
		if opts.OwnSparseRepository && opts.SparseBranch != "" {
			chunkReader, err = ttcchunk.NewOwnedConsumingChunkReader(repository, installation.Chunks, opts.SparseBranch, manifestID)
		} else if opts.OwnSparseRepository {
			chunkReader, err = ttcchunk.NewOwnedChunkReader(repository, installation.Chunks)
		} else {
			chunkReader, err = ttcchunk.NewChunkReader(repository, installation.Chunks)
		}
		if err != nil {
			return nil, err
		}
		pages, err = pgalloc.StartAsyncPagesFileLoadWithOpts(chunkReader.AsyncReader(4), nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true, RangeLoaded: chunkReader.RangeLoaded})
		if err != nil {
			chunkReader.AbortConsumption()
			return nil, err
		}
		loadRanges := make([]pgalloc.RemoteLoadRange, len(installation.Chunks))
		for i, chunk := range installation.Chunks {
			loadRanges[i] = pgalloc.RemoteLoadRange{Target: chunk.Target, StreamOffset: chunk.StreamOffset}
		}
		if err := targetMF.AttachSparseChunkPageLoad(pages, loadRanges, nil, nil); err != nil {
			pages.Cancel()
			_ = pages.Wait()
			return nil, err
		}
		pages.MemoryFilesDone()
	}
	repositoryOwned := len(installation.Chunks) == 0 && opts.OwnSparseRepository
	defer func() {
		if repositoryOwned {
			_ = repository.Close()
		}
	}()
	pagesOwned := pages != nil
	defer func() {
		if pagesOwned {
			if chunkReader != nil {
				chunkReader.AbortConsumption()
			}
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
	referenceTransaction := pgalloc.NewSparseReferenceTransaction()
	loadValues := map[any]any{
		pgalloc.CtxMemoryFile:             targetMF,
		pgalloc.CtxMemoryFileMap:          make(map[checkpoint.ResourceID]*pgalloc.MemoryFile),
		pgalloc.CtxLiveForkClone:          true,
		pgalloc.CtxSparseRelocation:       installation.Relocation,
		pgalloc.CtxSparseRestoreRefs:      referenceTransaction,
		vfs.CtxLiveForkChildBranchID:      opts.ChildBranchID,
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
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child); err != nil {
		referenceTransaction.Abort()
		return nil, fmt.Errorf("loading sparse distributed Kernel metadata: %w", err)
	}
	if err := referenceTransaction.Commit(); err != nil {
		return nil, err
	}
	if err := installation.Commit(); err != nil {
		return nil, err
	}
	installationCommitted = true
	childOwner = false
	if pages != nil {
		child.remotePageLoad = pages
		pagesOwned = false
	}
	if _, err := child.vdsoParams.access(); err != nil {
		child.Release()
		return nil, fmt.Errorf("materializing sparse distributed-fork VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, fmt.Errorf("sparse distributed target network configurator was not provided")
		}
		// Network devices and routes are target-local runtime state. As in the
		// ordinary checkpoint restore path, discard the saved configuration before
		// recreating it. This is also required for repeated imports: state fields
		// such as netstack's NIC map are intentionally not serialized and may be nil
		// in a newly loaded peer.
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring sparse distributed target network: %w", err)
		}
		rootNS.Stack().Restore()
		if resetter, ok := rootNS.Stack().(inet.LiveForkStackChildResetter); ok {
			if err := resetter.ResetLiveForkChild(); err != nil {
				child.Release()
				return nil, fmt.Errorf("resetting sparse distributed child external network state: %w", err)
			}
		}
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS sparse distributed-fork restore failed", err)
	}
	if chunkReader != nil {
		chunkReader.ActivateConsumption(installation.ResidentObjects())
		consumptionActivated = true
	} else if opts.SparseBranch != "" {
		// Every object was already resident in this family MemoryFile. This
		// update is advisory reclamation: failure only retains backing.
		_ = repository.MarkConsumed(opts.SparseBranch, manifestID, installation.Objects)
	}
	child.Pause()
	return child, nil
}

// LoadDistributedFork reconstructs a Kernel from portable metadata and a
// remote page reader. It consumes targetMF and reader on both success and
// failure.
func LoadDistributedFork(ctx context.Context, p platform.Platform, targetMF *pgalloc.MemoryFile, kernelMetadata, allocatorMetadata []byte, loadRanges []pgalloc.RemoteLoadRange, bindings []pgalloc.CapturedPageBinding, reader stateio.AsyncReader, opts DistributedForkLoadOptions) (*Kernel, error) {
	if p == nil || targetMF == nil || reader == nil || opts.Clocks == nil || opts.ChildBranchID == "" {
		if reader != nil {
			_ = reader.Close()
		}
		if targetMF != nil {
			targetMF.Destroy()
		}
		return nil, fmt.Errorf("distributed fork load is missing required target state")
	}
	if len(bindings) != 0 {
		return loadRelocatedDistributedFork(p, targetMF, kernelMetadata, loadRanges, bindings, reader, opts)
	}
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
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child); err != nil {
		return nil, fmt.Errorf("loading distributed Kernel metadata: %w", err)
	}
	child.remotePageLoad = pages
	child.remoteLoadRanges = append([]pgalloc.RemoteLoadRange(nil), loadRanges...)
	loaded = true
	// The restored VVAR mapping still refers to its fork-time remote page, but
	// privatizeLiveForkVDSOParamPage replaces that mapping with a new local
	// allocation. Materialize the old page first so its pull is acknowledged;
	// otherwise a target that never faults VVAR retains exactly one source page
	// forever and cannot complete a drain.
	if _, err := child.vdsoParams.access(); err != nil {
		child.Release()
		return nil, fmt.Errorf("materializing distributed-fork VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, fmt.Errorf("distributed target network configurator was not provided")
		}
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring distributed target network: %w", err)
		}
		rootNS.Stack().Restore()
		if resetter, ok := rootNS.Stack().(inet.LiveForkStackChildResetter); ok {
			if err := resetter.ResetLiveForkChild(); err != nil {
				child.Release()
				return nil, fmt.Errorf("resetting distributed child external network state: %w", err)
			}
		}
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
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child); err != nil {
		references.Abort()
		return nil, fmt.Errorf("loading relocated distributed Kernel metadata: %w", err)
	}
	if err := references.Commit(); err != nil {
		return nil, err
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
	if _, err := child.vdsoParams.access(); err != nil {
		child.Release()
		return nil, fmt.Errorf("materializing relocated distributed VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, errors.New("relocated distributed target network configurator was not provided")
		}
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring relocated distributed target network: %w", err)
		}
		rootNS.Stack().Restore()
		if resetter, ok := rootNS.Stack().(inet.LiveForkStackChildResetter); ok {
			if err := resetter.ResetLiveForkChild(); err != nil {
				child.Release()
				return nil, fmt.Errorf("resetting relocated distributed child network: %w", err)
			}
		}
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS relocated distributed-fork restore failed", err)
	}
	child.Pause()
	return child, nil
}

// HandoffDistributedForkMemory replaces a live remote page source with an
// immutable shared-filesystem manifest without eagerly faulting remaining
// pages. repository ownership transfers to the loader.
func (k *Kernel) HandoffDistributedForkMemory(repository *ttcchunk.MemoryRepository, manifest ttcchunk.ManifestID) error {
	if k.remoteHandoffManifest != (ttcchunk.ManifestID{}) {
		_ = repository.Close()
		if k.remoteHandoffManifest == manifest {
			return nil
		}
		return fmt.Errorf("distributed memory was already handed to manifest %s", k.remoteHandoffManifest)
	}
	if k.remotePageLoad == nil {
		_ = repository.Close()
		return fmt.Errorf("kernel has no active distributed remote-memory load")
	}
	if stats := k.remotePageLoad.Stats(); stats.Done {
		_ = repository.Close()
		if stats.Error != "" {
			return fmt.Errorf("distributed memory load stopped before handoff: %s", stats.Error)
		}
		k.remoteHandoffManifest = manifest
		return nil
	}
	geometry := k.remotePageLoad.ReaderGeometry()
	if geometry.NeedsRegisterDestinationFD {
		_ = repository.Close()
		return errors.New("distributed memory handoff does not support registered destination files")
	}
	packed, err := ttcchunk.NewPackedManifestReader(repository, manifest, k.remoteLoadRanges)
	if err != nil {
		return err
	}
	reader := stateio.NewIOReader(packed, geometry.MaxReadBytes, geometry.MaxRanges, geometry.MaxParallel)
	if err := k.remotePageLoad.SwitchReader(reader); err != nil {
		return err
	}
	k.remoteHandoffManifest = manifest
	return nil
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
	if k.remotePageLoad == nil {
		return pgalloc.AsyncPagesFileLoadStats{Done: true}
	}
	return k.remotePageLoad.Stats()
}

// DrainDistributedForkMemory eagerly materializes all remaining remote pages.
func (k *Kernel) DrainDistributedForkMemory() {
	if k.remotePageLoad != nil {
		k.remotePageLoad.Drain()
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
	if k.remotePageLoad != nil {
		target.auxiliaryRemotePageLoads = append(target.auxiliaryRemotePageLoads, k.remotePageLoad)
		k.remotePageLoad = nil
	}
	target.auxiliaryRemotePageLoads = append(target.auxiliaryRemotePageLoads, k.auxiliaryRemotePageLoads...)
	k.auxiliaryRemotePageLoads = nil
	k.remoteLoadRanges = nil
	k.remoteHandoffManifest = ttcchunk.ManifestID{}
}
