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
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
)

// LiveForkOptions controls an experimental in-memory Kernel clone.
type LiveForkOptions struct {
	// Clocks is the host clock source installed in the cloned Timekeeper.
	Clocks sentrytime.Clocks

	// RestoreOptions are passed to filesystem restore completion hooks.
	RestoreOptions vfs.CompleteRestoreOptions

	// AllowNetworkStack permits cloning an initialized network stack. Callers
	// must first prove that every link and endpoint is contained in the sandbox.
	AllowNetworkStack bool

	// AllowSharedFilesystemResources permits clone-aware filesystems to borrow
	// resources that the caller has proven immutable or otherwise share-safe.
	AllowSharedFilesystemResources bool
}

// LiveFork creates a paused Kernel by streaming only state metadata from k.
// Application pages remain in the shared MemoryFile and are made copy-on-write.
// The returned Kernel has not executed any task instructions; callers must call
// StartPaused and then Unpause to make it runnable.
//
// This API is intentionally experimental. It currently rejects private
// filesystem MemoryFiles and an initialized network stack.
//
// Preconditions: k must be paused for the duration of the call.
func (k *Kernel) LiveFork(ctx context.Context, opts LiveForkOptions) (*Kernel, error) {
	if !k.IsPaused() {
		return nil, fmt.Errorf("live fork requires a paused kernel")
	}
	if opts.Clocks == nil {
		return nil, fmt.Errorf("live fork requires a clock source")
	}
	if rootNS := k.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil && !opts.AllowNetworkStack {
		return nil, fmt.Errorf("live fork does not yet support a network stack")
	}

	var child *Kernel
	err := k.quiescePausedAnd(ctx, func() error {
		if rootNS := k.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
			stack := rootNS.Stack()
			stack.SetRemoveConf(false)
			stack.Pause()
			defer stack.Resume()
		}

		mfs := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
		var fsResources vfs.LiveForkFilesystemResources
		if opts.AllowSharedFilesystemResources {
			fsResources = make(vfs.LiveForkFilesystemResources)
		}
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           mfs,
			vfs.CtxLiveForkFilesystemResources: fsResources,
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
			return fmt.Errorf("preparing VFS for live fork: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		if len(mfs) != 0 {
			return fmt.Errorf("live fork does not yet support %d private filesystem MemoryFiles", len(mfs))
		}

		k.prepareMemoryManagersForLiveFork()
		k.mf.MarkSavable()

		k.mf.IncOwner()
		ownerTransferred := false
		childLoaded := false
		defer func() {
			if ownerTransferred {
				return
			}
			if childLoaded {
				child.Release()
			} else {
				k.mf.Destroy()
			}
		}()

		child = &Kernel{Platform: k.Platform}
		child.SetMemoryFile(k.mf)
		child.initRuntimeFields()
		loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           mfs,
			pgalloc.CtxLiveForkClone:           true,
			vfs.CtxLiveForkFilesystemResources: fsResources,
		})
		if err := streamLiveForkState(saveCtx, loadCtx, k, child); err != nil {
			return err
		}
		childLoaded = true

		if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
			return err
		}
		child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
		if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
			rootNS.Stack().Restore()
		}
		if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
			return vfs.PrependErrMsg("VFS live-fork restore failed", err)
		}
		child.Pause()
		ownerTransferred = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return child, nil
}

// privatizeLiveForkVDSOParamPage gives the child Timekeeper a branch-private
// VVAR page. The VDSO code remains shared and immutable.
func (k *Kernel) privatizeLiveForkVDSOParamPage() error {
	fr, err := k.mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.System})
	if err != nil {
		return fmt.Errorf("allocating live-fork VDSO parameter page: %w", err)
	}
	k.vdso.ParamPage.RebindMemoryFileRange(k.mf, fr)
	k.vdsoParams.mf = k.mf
	k.vdsoParams.fr = fr
	k.vdsoParams.seq = 0
	return nil
}

// prepareMemoryManagersForLiveFork transitions all private PMAs to COW.
// Tasks are paused, so Task.image and runState may be read without Task.mu.
func (k *Kernel) prepareMemoryManagersForLiveFork() {
	prepared := make(map[*mm.MemoryManager]struct{})
	prepare := func(memMgr *mm.MemoryManager) {
		if memMgr == nil {
			return
		}
		if _, ok := prepared[memMgr]; ok {
			return
		}
		memMgr.PrepareLiveFork()
		prepared[memMgr] = struct{}{}
	}

	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t := range k.tasks.Root.tids {
		prepare(t.image.MemoryManager)
		if r, ok := t.runState.(*runExecveAfterSiblingExitStop); ok {
			prepare(r.image.MemoryManager)
		}
	}
}

// streamLiveForkState connects state.Save directly to state.Load. The pipe
// bounds temporary memory use and ensures that no checkpoint file is created.
func streamLiveForkState(saveCtx, loadCtx context.Context, parent, child *Kernel) error {
	reader, writer := io.Pipe()
	saveErrCh := make(chan error, 1)
	go func() {
		_, err := state.Save(saveCtx, writer, parent)
		writer.CloseWithError(err)
		saveErrCh <- err
	}()

	_, loadErr := state.Load(loadCtx, reader, child)
	if loadErr != nil {
		reader.CloseWithError(loadErr)
	} else {
		reader.Close()
	}
	saveErr := <-saveErrCh
	if loadErr != nil {
		return fmt.Errorf("loading live-fork metadata: %w", loadErr)
	}
	if saveErr != nil {
		return fmt.Errorf("saving live-fork metadata: %w", saveErr)
	}
	return nil
}
