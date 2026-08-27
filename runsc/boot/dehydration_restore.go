// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package boot

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/urpc"
)

// DurableDehydrationRestoreArgs installs one process-independent branch into
// an unstarted Sentry. Files contains metadata.state followed by one packed
// memory snapshot for each metadata memory entry.
type DurableDehydrationRestoreArgs struct {
	urpc.FilePayload
	ID               string
	ParentID         string
	FilesystemBranch string
	Resume           bool
	ExecSessions     []ExecSessionInfo
	// NetworkArgs reconstructs target-local links after the saved netstack has
	// been loaded. Cold restore runs before Loader.Run, so it cannot rely on
	// SetNetworkArgs having reached the Loader yet.
	NetworkArgs *CreateLinksAndRoutesArgs
	// UseDonatedMemoryFiles selects peer import into an already-running Sentry.
	// Files then contains metadata, N snapshots, and N empty target files. A
	// sparse restore substitutes its repository for snapshots; when application
	// memory is reused, it donates only N-1 private target files.
	UseDonatedMemoryFiles bool
	// ReusePeerApplicationMemoryFile installs sparse application chunks into
	// the active family's shared MemoryFile instead of a newly donated memfd.
	// This lets siblings restored from the same durable manifest share the
	// same resident host pages and retain 4 KiB CoW behavior.
	ReusePeerApplicationMemoryFile bool
	// Sparse uses Files as metadata, memory repository, and (for peer import)
	// one empty target file per metadata MemoryFile.
	Sparse bool
}

// DurableDehydrationRestoreInfo describes the newly installed runtime root.
type DurableDehydrationRestoreInfo struct {
	ID          string `json:"id"`
	MemoryFiles int    `json:"memory_files"`
}

func (l *Loader) restoreDurableDehydration(args *DurableDehydrationRestoreArgs) (*DurableDehydrationRestoreInfo, error) {
	if args == nil || args.ID == "" || args.FilesystemBranch == "" || len(args.FilePayload.Files) < 2 {
		return nil, errors.New("durable dehydration restore is missing branch, filesystem, metadata, or memory state")
	}
	if args.UseDonatedMemoryFiles {
		l.mu.Lock()
		state := l.state
		l.mu.Unlock()
		if state != started && state != restored {
			return nil, fmt.Errorf("sandbox is %s, not available for dehydration peer import", state)
		}
		return l.restoreDurableDehydrationPeer(args)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != created || l.distributedRestore || l.dehydrationRestore {
		return nil, fmt.Errorf("sandbox is %s, not an unstarted dehydration target", l.state)
	}
	metadataFD, err := unix.Dup(int(args.FilePayload.Files[0].Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicating dehydration metadata: %w", err)
	}
	// The duplicated descriptor is the only one used below. Close the donated
	// descriptor before replying so a client may remove the consumed control
	// directory immediately even when it resides on NFS. Waiting for uRPC's
	// deferred payload cleanup leaves a small response/unlink race in which NFS
	// materializes an open metadata.state as a .nfs placeholder.
	_ = args.FilePayload.Files[0].Close()
	metadataFile := os.NewFile(uintptr(metadataFD), "dehydration-metadata")
	metadata, err := kernel.ReadDurableDehydrationMetadata(metadataFile)
	metadataFile.Close()
	if err != nil {
		return nil, err
	}
	if !args.Sparse {
		if got, want := len(args.FilePayload.Files)-1, len(metadata.Memories); got != want {
			return nil, fmt.Errorf("got %d dehydration snapshots, want %d", got, want)
		}
	} else if len(args.FilePayload.Files) != 2 || metadata.SparseManifest == (ttcchunk.ManifestID{}) {
		return nil, fmt.Errorf("sparse dehydration restore requires metadata and one repository")
	}
	var (
		p        platform.Platform
		reusable platform.RestoreReuser
		mainMF   *pgalloc.MemoryFile
	)
	if candidate, ok := l.k.Platform.(platform.RestoreReuser); ok {
		reusable = candidate
		p = candidate
		// Process-scoped VM platforms use GuestRAM rather than the reserved
		// memfd. The latter is retained for Linux platforms and older targets.
		if l.distributedMemoryFile != nil {
			_ = l.distributedMemoryFile.Close()
			l.distributedMemoryFile = nil
		}
		mainMF, err = createMemoryFileForPlatform(p, l.root.conf.AppHugePages, l.hostTHP)
	} else {
		if l.distributedMemoryFile == nil {
			return nil, errors.New("dehydration target has no reserved application memory file")
		}
		reservedMemoryFile := l.distributedMemoryFile
		l.distributedMemoryFile = nil
		mainMF, err = createMemoryFileFromFile(l.root.conf.AppHugePages, l.hostTHP, reservedMemoryFile, false)
		if err == nil {
			p, err = createPlatform(l.root.conf, l.root.applicationCores, nil, l.sandboxID)
		}
	}
	if err != nil {
		if mainMF != nil {
			mainMF.Destroy()
		}
		return nil, fmt.Errorf("creating dehydration platform and application memory: %w", err)
	}
	restoreMnts := restoreMounts{
		fdmap:     make(map[checkpoint.ResourceID]int),
		mfmap:     make(map[checkpoint.ResourceID]*pgalloc.MemoryFile),
		sharedMfs: make(map[string]bool),
	}
	mounter := l.newContainerMounter(&l.root)
	if err := mounter.configureRestore(&restoreMnts); err != nil {
		mainMF.Destroy()
		return nil, fmt.Errorf("configuring dehydration filesystems: %w", err)
	}
	fdmapOwned := true
	defer func() {
		if fdmapOwned {
			for _, hostFD := range restoreMnts.fdmap {
				_ = unix.Close(hostFD)
			}
		}
	}()

	loadMemories := make([]kernel.DurableDehydrationLoadMemory, len(metadata.Memories))
	loadMemories[0].MemoryFile = mainMF
	memoryFiles := map[checkpoint.ResourceID]*pgalloc.MemoryFile{{}: mainMF}
	for i := 1; i < len(metadata.Memories); i++ {
		resourceID := metadata.Memories[i].ResourceID
		mf := restoreMnts.mfmap[resourceID]
		if mf == nil {
			mainMF.Destroy()
			for _, private := range restoreMnts.mfmap {
				private.Destroy()
			}
			return nil, fmt.Errorf("dehydration target is missing private memory file %q", resourceID)
		}
		loadMemories[i].MemoryFile = mf
		memoryFiles[resourceID] = mf
		delete(restoreMnts.mfmap, resourceID)
	}
	if len(restoreMnts.mfmap) != 0 {
		mainMF.Destroy()
		for _, private := range restoreMnts.mfmap {
			private.Destroy()
		}
		for i := 1; i < len(loadMemories); i++ {
			loadMemories[i].MemoryFile.Destroy()
		}
		return nil, fmt.Errorf("dehydration target has %d unexpected private memory files", len(restoreMnts.mfmap))
	}
	for i := range loadMemories {
		if args.Sparse {
			continue
		}
		fd, err := unix.Dup(int(args.FilePayload.Files[i+1].Fd()))
		if err != nil {
			for j := 0; j < i; j++ {
				_ = loadMemories[j].Reader.Close()
			}
			for j := range loadMemories {
				loadMemories[j].MemoryFile.Destroy()
			}
			return nil, fmt.Errorf("duplicating dehydration snapshot %d: %w", i, err)
		}
		loadMemories[i].Reader = stateio.NewFDReader(int32(fd), 1<<20, 64, 4)
	}
	resources := make(vfs.RestoreFilesystemResources, len(l.ttcVolumeRepositories))
	for _, donated := range l.ttcVolumeRepositories {
		info := donated.repository.Info()
		restoreID := donated.config.RestoreID
		if restoreID == "" {
			restoreID = info.ID
		}
		key := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}
		resources[key] = vfs.RestoreFilesystemResource{Resource: donated.repository, Branch: donated.config.Branch}
	}
	loadOptions := kernel.DurableDehydrationLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(), BranchID: args.FilesystemBranch,
		SparseBranch:               args.FilesystemBranch,
		RestoreFilesystemResources: resources, RestoreFilesystemFDMap: restoreMnts.fdmap,
		ConfigureNetwork: func(k *kernel.Kernel, stack inet.Stack) error {
			return l.configureNetworkWithArgs(k, stack, args.NetworkArgs)
		},
	}
	loadCtx := l.k.SupervisorContext()
	old := l.k
	bootstrapReleased := false
	if reusable != nil {
		// Hypervisor.framework permits one VM per process. Release every
		// bootstrap AddressSpace, then reuse the existing VM/monitor cache for
		// the durable Kernel just as ordinary checkpoint restore does.
		old.Release()
		bootstrapReleased = true
		if err := reusable.PrepareRestore(); err != nil {
			for i := range loadMemories {
				if loadMemories[i].Reader != nil {
					_ = loadMemories[i].Reader.Close()
				}
				loadMemories[i].MemoryFile.Destroy()
			}
			return nil, fmt.Errorf("preparing platform for dehydration restore: %w", err)
		}
	}
	fdmapOwned = false // Kernel load and VFS CompleteRestore consume descriptors.
	var child *kernel.Kernel
	if args.Sparse {
		repository, openErr := ttcchunk.OpenMemoryRepositoryFD(args.FilePayload.Files[1])
		if openErr != nil {
			for _, mf := range memoryFiles {
				mf.Destroy()
			}
			return nil, openErr
		}
		loadOptions.OwnSparseRepository = true
		child, err = kernel.LoadSparseDurableDehydration(loadCtx, p, metadata, memoryFiles, repository, loadOptions)
	} else {
		child, err = kernel.LoadDurableDehydration(loadCtx, p, metadata, loadMemories, loadOptions)
	}
	if err != nil {
		return nil, err
	}
	childOwned := true
	defer func() {
		if childOwned {
			if err := child.StartPaused(); err == nil {
				child.Release()
			}
		}
	}()
	for _, task := range child.TaskSet().Root.Tasks() {
		task.RestoreContainerID(l.sandboxID)
	}
	child.RegisterContainerName(l.sandboxID, l.root.containerName)
	dogOpts := watchdog.DefaultOpts
	if err := dogOpts.TaskTimeoutAction.Set(l.root.conf.WatchdogAction); err != nil {
		return nil, err
	}
	newWatchdog := watchdog.New(child, dogOpts)
	if !bootstrapReleased {
		old.Pause()
		if err := old.StartPaused(); err != nil {
			return nil, fmt.Errorf("starting dehydration placeholder for release: %w", err)
		}
	}
	if err := l.family.ReplaceLoaderBranchFromDehydration(args.ID, args.ParentID, args.FilesystemBranch, child, newWatchdog, args.ExecSessions); err != nil {
		if !bootstrapReleased {
			old.Release()
		}
		return nil, err
	}
	l.k = child
	l.root.procArgs = kernel.CreateProcessArgs{}
	l.watchdog.Stop()
	l.watchdog = newWatchdog
	l.restoreDone = sync.NewCond(&l.mu)
	l.dehydrationRestore = true
	l.state = restoringUnstarted
	if !bootstrapReleased {
		old.Release()
	}
	childOwned = false
	return &DurableDehydrationRestoreInfo{ID: args.ID, MemoryFiles: len(metadata.Memories)}, nil
}

func (l *Loader) restoreDurableDehydrationPeer(args *DurableDehydrationRestoreArgs) (*DurableDehydrationRestoreInfo, error) {
	metadataFD, err := unix.Dup(int(args.FilePayload.Files[0].Fd()))
	if err != nil {
		return nil, err
	}
	_ = args.FilePayload.Files[0].Close()
	metadataFile := os.NewFile(uintptr(metadataFD), "dehydration-peer-metadata")
	metadata, err := kernel.ReadDurableDehydrationMetadata(metadataFile)
	metadataFile.Close()
	if err != nil {
		return nil, err
	}
	n := len(metadata.Memories)
	wantFiles := 1 + 2*n
	if args.Sparse {
		wantFiles = 2 + n
		if args.ReusePeerApplicationMemoryFile {
			wantFiles--
		}
	}
	if got := len(args.FilePayload.Files); got != wantFiles {
		return nil, fmt.Errorf("dehydration peer import got %d files, want %d", got, wantFiles)
	}
	loadMemories := make([]kernel.DurableDehydrationLoadMemory, n)
	cleanupMemories := func() {
		for i := range loadMemories {
			if loadMemories[i].Reader != nil {
				_ = loadMemories[i].Reader.Close()
			}
			if loadMemories[i].MemoryFile != nil {
				loadMemories[i].MemoryFile.Destroy()
			}
		}
	}
	for i := 0; i < n; i++ {
		if !args.Sparse {
			snapshotFD, err := unix.Dup(int(args.FilePayload.Files[1+i].Fd()))
			if err != nil {
				cleanupMemories()
				return nil, err
			}
			loadMemories[i].Reader = stateio.NewFDReader(int32(snapshotFD), 1<<20, 64, 4)
		}
		if args.Sparse && args.ReusePeerApplicationMemoryFile && i == 0 {
			continue
		}
		targetStart := 1 + n
		if args.Sparse {
			targetStart = 2
		}
		targetIndex := targetStart + i
		if args.Sparse && args.ReusePeerApplicationMemoryFile {
			targetIndex--
		}
		targetFD, err := unix.Dup(int(args.FilePayload.Files[targetIndex].Fd()))
		if err != nil {
			cleanupMemories()
			return nil, err
		}
		target := os.NewFile(uintptr(targetFD), fmt.Sprintf("dehydration-peer-memory-%d", i))
		if i == 0 {
			loadMemories[i].MemoryFile, err = createPrePrimedMemoryFileFromFile(l.root.conf.AppHugePages, l.hostTHP, target, true)
		} else {
			loadMemories[i].MemoryFile, err = createEmptyPrivateMemoryFile(target, metadata.Memories[i].ResourceID)
		}
		if err != nil {
			cleanupMemories()
			return nil, fmt.Errorf("creating dehydration peer memory %d: %w", i, err)
		}
	}
	consumed := false
	defer func() {
		if !consumed {
			cleanupMemories()
		}
	}()
	err = l.family.RestoreDurablePeer(args, func(source *kernel.Kernel, resources vfs.LiveForkFilesystemResources) (*kernel.Kernel, error) {
		// Branches in one Sentry share the platform. In particular, systrap's
		// stub mapping and source subprocess are process-global; constructing a
		// second platform here can collide with the active platform's mappings.
		loadOptions := kernel.DurableDehydrationLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), BranchID: args.FilesystemBranch,
			SparseBranch:        args.FilesystemBranch,
			FilesystemResources: resources, ConfigureNetwork: func(k *kernel.Kernel, stack inet.Stack) error {
				return l.configureNetworkWithArgs(k, stack, args.NetworkArgs)
			},
		}
		var child *kernel.Kernel
		var err error
		if args.Sparse {
			repository, openErr := ttcchunk.OpenMemoryRepositoryFD(args.FilePayload.Files[1])
			if openErr != nil {
				return nil, openErr
			}
			if args.ReusePeerApplicationMemoryFile {
				if source.MemoryFile() == nil {
					_ = repository.Close()
					return nil, errors.New("active peer has no application MemoryFile")
				}
				source.MemoryFile().IncOwner()
				loadMemories[0].MemoryFile = source.MemoryFile()
			}
			memoryFiles := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, len(loadMemories))
			for i := range loadMemories {
				memoryFiles[metadata.Memories[i].ResourceID] = loadMemories[i].MemoryFile
				loadMemories[i].MemoryFile = nil
			}
			consumed = true
			loadOptions.OwnSparseRepository = true
			child, err = kernel.LoadSparseDurableDehydration(source.SupervisorContext(), source.Platform, metadata, memoryFiles, repository, loadOptions)
		} else {
			consumed = true
			child, err = kernel.LoadDurableDehydration(source.SupervisorContext(), source.Platform, metadata, loadMemories, loadOptions)
		}
		if err != nil {
			return nil, err
		}
		// A durable peer may have been captured by a short-lived bootstrap
		// Sentry. Container IDs are runtime-local task metadata, not part of the
		// guest-visible state, so retarget them to the receiving sandbox before
		// branch controls attempt to locate the init process.
		for _, task := range child.TaskSet().Root.Tasks() {
			task.RestoreContainerID(l.sandboxID)
		}
		child.RegisterContainerName(l.sandboxID, l.root.containerName)
		return child, nil
	})
	if err != nil {
		return nil, err
	}
	return &DurableDehydrationRestoreInfo{ID: args.ID, MemoryFiles: n}, nil
}

// RestoreDurablePeer imports a cold branch next to any available active peer.
// ParentID is copied only for UI provenance; it is never consulted here.
func (f *SandboxFamily) RestoreDurablePeer(args *DurableDehydrationRestoreArgs, load func(*kernel.Kernel, vfs.LiveForkFilesystemResources) (*kernel.Kernel, error)) error {
	if args == nil || args.ID == "" || load == nil {
		return errors.New("durable peer restore is missing arguments")
	}
	return f.restorePeer(args.ID, args.ParentID, args.FilesystemBranch, args.Resume, args.ExecSessions, load)
}

// RestoreDistributedPeer imports a remotely captured branch into the existing
// Sentry and application MemoryFile. BranchID is the user-visible identity;
// filesystemBranch is the globally unique repository branch prepared by the
// source transaction.
func (f *SandboxFamily) RestoreDistributedPeer(branchID, filesystemBranch string, load func(*kernel.Kernel, vfs.LiveForkFilesystemResources) (*kernel.Kernel, error)) error {
	if branchID == "" || filesystemBranch == "" || load == nil {
		return errors.New("distributed peer restore is missing branch, filesystem, or loader")
	}
	return f.restorePeer(branchID, "", filesystemBranch, false, nil, load)
}

func (f *SandboxFamily) restorePeer(id, parentID, filesystemBranch string, resume bool, sessions []ExecSessionInfo, load func(*kernel.Kernel, vfs.LiveForkFilesystemResources) (*kernel.Kernel, error)) error {
	f.mu.Lock()
	if _, ok := f.branches[id]; ok {
		f.mu.Unlock()
		return fmt.Errorf("branch %q already exists in the runtime", id)
	}
	ids := make([]string, 0, len(f.branches))
	for id, branch := range f.branches {
		if branch.k != nil && (branch.state == BranchRunning || branch.state == BranchPaused) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		f.mu.Unlock()
		return errors.New("no active peer is available for dehydration restore")
	}
	source := f.branches[ids[0]]
	f.mu.Unlock()
	source.opMu.Lock()
	defer source.opMu.Unlock()
	f.mu.Lock()
	if source.k == nil || (source.state != BranchRunning && source.state != BranchPaused) {
		state := source.state
		f.mu.Unlock()
		return fmt.Errorf("dehydration restore peer %q became %s", source.id, state)
	}
	wasRunning := source.state == BranchRunning
	f.mu.Unlock()
	if wasRunning {
		source.k.Pause()
		defer func() {
			source.k.Unpause()
			_ = control.PostResume(source.k, nil)
			f.mu.Lock()
			source.state = kernelBranchState(source.k)
			f.mu.Unlock()
		}()
	}
	var child *kernel.Kernel
	if err := source.k.WithDehydrationFilesystemResources(source.k.SupervisorContext(), filesystemBranch, func(resources vfs.LiveForkFilesystemResources) error {
		var err error
		child, err = load(source.k, resources)
		return err
	}); err != nil {
		return err
	}
	if err := child.StartPaused(); err != nil {
		child.Release()
		return err
	}
	newSessions := make(map[string]*execSession)
	for _, info := range sessions {
		if info.State != ExecSessionRunning {
			continue
		}
		if tg := child.RootPIDNamespace().ThreadGroupWithID(info.PID); tg != nil {
			newSessions[info.ID] = &execSession{info: cloneExecSessionInfo(info), tg: tg}
		}
	}
	var dog *watchdog.Watchdog
	if f.watchdogOpts != nil {
		dog = watchdog.New(child, *f.watchdogOpts)
		dog.Start()
	}
	cleanupChild := func() {
		if dog != nil {
			dog.Stop()
		}
		child.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
		if child.IsPaused() {
			child.Unpause()
		}
		child.WaitExited()
		child.Release()
	}
	state := BranchPaused
	if resume {
		child.Unpause()
		if err := control.PostResume(child, nil); err != nil {
			cleanupChild()
			return err
		}
		state = kernelBranchState(child)
	}
	f.mu.Lock()
	if _, ok := f.branches[id]; ok {
		f.mu.Unlock()
		cleanupChild()
		return fmt.Errorf("branch %q appeared during peer restore", id)
	}
	b := &sandboxBranch{
		id: id, parentID: parentID, filesystemBranch: filesystemBranch,
		k: child, watchdog: dog, state: state, createdAt: time.Now().UTC(), execSessions: newSessions,
	}
	f.branches[id] = b
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	for id, session := range newSessions {
		f.watchExecSession(b.id, id, session)
	}
	return nil
}
