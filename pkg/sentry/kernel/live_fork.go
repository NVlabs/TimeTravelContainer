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
	"sort"

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

// LiveForkResourceIssue describes one resource that prevents an in-memory
// fork. PID and FD identify the descriptor-table root through which Resource
// was reached; Resource may be a transitively referenced object.
type LiveForkResourceIssue struct {
	PID      ThreadID
	FD       int32
	Resource string
	Reason   string
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
		return fmt.Sprintf("live fork resource validation failed: pid %d fd %d resource %s: %s", i.PID, i.FD, i.Resource, i.Reason)
	}
	return fmt.Sprintf("live fork resource validation failed with %d issues; first: pid %d fd %d resource %s: %s", len(e.Issues), i.PID, i.FD, i.Resource, i.Reason)
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
		if err := k.validateLiveForkResources(); err != nil {
			return err
		}
		k.prepareSharedMemoryForLiveFork(ctx)
		// Discard PMAs backed by files that state serialization cannot encode,
		// such as a gofer MmapCachedFile. The VMAs remain and refault from their
		// branch-local cloned filesystem graph. This must precede VFS.PrepareSave
		// for the same reason as in Kernel.SaveTo.
		if err := k.invalidateUnsavableMappings(ctx); err != nil {
			return fmt.Errorf("invalidating unsavable mappings for live fork: %w", err)
		}
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
			pgalloc.CtxLiveForkSource:          true,
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
		root.table.ForEach(k.SupervisorContext(), func(fd int32, file *vfs.FileDescription, _ FDFlags) bool {
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
						issues = append(issues, LiveForkResourceIssue{root.pid, fd, fmt.Sprintf("%T", impl), "anonymous or socket resource has not opted in"})
					case "host", "fuse":
						issues = append(issues, LiveForkResourceIssue{root.pid, fd, fmt.Sprintf("%T", impl), fmt.Sprintf("filesystem %q is host-backed", fsType)})
					}
					return
				}
				referenced, err := validator.ValidateLiveFork()
				if err != nil {
					issues = append(issues, LiveForkResourceIssue{root.pid, fd, fmt.Sprintf("%T", impl), err.Error()})
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
