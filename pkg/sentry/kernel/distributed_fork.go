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
	"fmt"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
)

// DistributedForkCaptureOptions controls portable source capture.
type DistributedForkCaptureOptions struct {
	ChildBranchID                  string
	AllowSharedFilesystemResources bool
}

// DistributedForkCapture is the source-side product of a distributed fork.
// Snapshot owns source page references and must be closed if no target takes
// responsibility for completing or canceling the transfer.
type DistributedForkCapture struct {
	KernelMetadata    []byte
	AllocatorMetadata []byte
	LoadRanges        []pgalloc.RemoteLoadRange
	Snapshot          *pgalloc.RemoteSnapshot

	// FilesystemResources contains process-local handles. A same-process target
	// may use them directly; a remote ttcd reopens equivalent handles from its
	// shared repository before calling LoadDistributedFork.
	FilesystemResources vfs.LiveForkFilesystemResources
}

// Close cancels all outstanding remote pages.
func (c *DistributedForkCapture) Close() error {
	if c == nil || c.Snapshot == nil {
		return nil
	}
	err := c.Snapshot.Close()
	c.Snapshot = nil
	return err
}

// CaptureDistributedFork captures portable Kernel and allocator metadata while
// pinning page contents in a RemoteSnapshot. No page contents are serialized.
// The source remains paused on return and may be resumed immediately.
func (k *Kernel) CaptureDistributedFork(ctx context.Context, opts DistributedForkCaptureOptions) (*DistributedForkCapture, error) {
	if !k.IsPaused() {
		return nil, fmt.Errorf("distributed fork capture requires a paused kernel")
	}
	if opts.ChildBranchID == "" {
		return nil, fmt.Errorf("distributed fork capture requires a child branch ID")
	}
	k.tasks.waitForExternalStop()
	k.ReceiveTaskStates()
	if rootNS := k.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		return nil, fmt.Errorf("distributed fork network reconstruction is not yet configured")
	}

	var capture *DistributedForkCapture
	err := k.quiescePausedAnd(ctx, func() (retErr error) {
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
			pgalloc.CtxLiveForkSource:          true,
			vfs.CtxLiveForkFilesystemResources: fsResources,
			vfs.CtxLiveForkChildBranchID:       opts.ChildBranchID,
			vfs.CtxLiveForkChildBranchIDs:      []string{opts.ChildBranchID},
			vfs.CtxLiveForkTransactions:        fsTransactions,
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
			return fmt.Errorf("preparing VFS for distributed fork: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		if len(mfs) != 0 {
			return fmt.Errorf("distributed fork does not yet support %d private filesystem MemoryFiles", len(mfs))
		}

		k.prepareMemoryManagersForDistributedFork()
		k.mf.MarkSavable()
		allocatorMetadata, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(ctx, k.mf)
		if err != nil {
			return err
		}
		snapshot, err := pgalloc.NewRemoteSnapshot(regions)
		if err != nil {
			return err
		}
		keepSnapshot := false
		defer func() {
			if !keepSnapshot {
				_ = snapshot.Close()
			}
		}()

		var kernelMetadata bytes.Buffer
		buffered := bufio.NewWriterSize(&kernelMetadata, 64<<10)
		if _, err := state.Save(saveCtx, buffered, k); err != nil {
			return fmt.Errorf("saving distributed Kernel metadata: %w", err)
		}
		if err := buffered.Flush(); err != nil {
			return err
		}
		if err := fsTransactions.Commit(); err != nil {
			return fmt.Errorf("committing distributed-fork filesystem transactions: %w", err)
		}
		committed = true
		capture = &DistributedForkCapture{
			KernelMetadata: kernelMetadata.Bytes(), AllocatorMetadata: allocatorMetadata,
			LoadRanges: loads, Snapshot: snapshot, FilesystemResources: fsResources,
		}
		keepSnapshot = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return capture, nil
}

// DistributedForkLoadOptions controls target reconstruction.
type DistributedForkLoadOptions struct {
	Clocks              sentrytime.Clocks
	ChildBranchID       string
	RestoreOptions      vfs.CompleteRestoreOptions
	FilesystemResources vfs.LiveForkFilesystemResources
	DemandOnly          bool
}

// LoadDistributedFork reconstructs a Kernel from portable metadata and a
// remote page reader. It consumes targetMF and reader on both success and
// failure.
func LoadDistributedFork(ctx context.Context, p platform.Platform, targetMF *pgalloc.MemoryFile, kernelMetadata, allocatorMetadata []byte, loadRanges []pgalloc.RemoteLoadRange, reader stateio.AsyncReader, opts DistributedForkLoadOptions) (*Kernel, error) {
	if p == nil || targetMF == nil || reader == nil || opts.Clocks == nil || opts.ChildBranchID == "" {
		if reader != nil {
			_ = reader.Close()
		}
		if targetMF != nil {
			targetMF.Destroy()
		}
		return nil, fmt.Errorf("distributed fork load is missing required target state")
	}
	if err := pgalloc.ImportMemoryFileMetadataForRemote(ctx, targetMF, allocatorMetadata); err != nil {
		_ = reader.Close()
		targetMF.Destroy()
		return nil, err
	}
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: opts.DemandOnly})
	if err != nil {
		targetMF.Destroy()
		return nil, err
	}
	child := &Kernel{Platform: p}
	child.SetMemoryFile(targetMF)
	child.initRuntimeFields()
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
	loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
		pgalloc.CtxMemoryFile:                targetMF,
		pgalloc.CtxMemoryFileMap:             mfs,
		pgalloc.CtxLiveForkClone:             true,
		pgalloc.CtxLiveForkRemoteMemoryClone: true,
		vfs.CtxLiveForkFilesystemResources:   opts.FilesystemResources,
		vfs.CtxLiveForkChildBranchID:         opts.ChildBranchID,
	})
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(kernelMetadata), 64<<10), child); err != nil {
		return nil, fmt.Errorf("loading distributed Kernel metadata: %w", err)
	}
	child.remotePageLoad = pages
	loaded = true
	if err := targetMF.AttachRemotePageLoad(pages, loadRanges, nil, nil); err != nil {
		child.Release()
		return nil, err
	}
	pages.MemoryFilesDone()
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS distributed-fork restore failed", err)
	}
	child.Pause()
	return child, nil
}
