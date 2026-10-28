// Copyright 2023 The gVisor Authors.
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

package boot

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
)

// DurableDehydrationRestoreArgs installs one process-independent branch into
// an unstarted Sentry. Files contains metadata.state, a graph manifest, the
// graph bulk-object directory, and an optional object relay.
type DurableDehydrationRestoreArgs struct {
	urpc.FilePayload
	ID               string
	ParentID         string
	FilesystemBranch string
	Quarantined      bool
	Resume           bool
	ExecSessions     []ExecSessionInfo
	// NetworkArgs reconstructs target-local links after the saved netstack has
	// been loaded. Cold restore runs before Loader.Run, so it cannot rely on
	// SetNetworkArgs having reached the Loader yet.
	NetworkArgs *sentryapi.CreateLinksAndRoutesArgs
	// UseDonatedMemoryFiles selects peer import into an already-running Sentry.
	// Files then contains graph sources followed by empty target files. When
	// application memory is reused, only N-1 private target files are donated.
	UseDonatedMemoryFiles bool
	// ReusePeerApplicationMemoryFile installs graph application chunks into
	// the active family's shared MemoryFile instead of a newly donated memfd.
	// This lets siblings restored from the same durable manifest share the
	// same resident host pages and retain 4 KiB CoW behavior.
	ReusePeerApplicationMemoryFile bool
	GraphRemote                    bool
	GraphRemoteToken               ttcsnapshot.ObjectRelayToken
	// FilesystemConnections is the number of donated, fenced range streams at
	// the end of Files. They lazily populate sparse active object caches from a
	// committed cold pack chain.
	FilesystemConnections int
	FilesystemToken       ttcvolume.RemoteToken
	// FilesystemRestoreIDs maps the current identity of each local repository
	// to the one this particular branch was serialized against, as a
	// distributed fork's does. A long-lived Sentry may host cold branches
	// captured across several local materializations, whose physical
	// repository IDs differ while their logical mount is the same.
	FilesystemRestoreIDs map[string]string
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
	l.restoring.Store(true)
	defer l.restoring.Store(false)
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
	if args.FilesystemConnections < 0 || args.FilesystemConnections > len(args.FilePayload.Files)-1 || (args.FilesystemConnections != 0 && args.FilesystemConnections != len(l.ttcVolumeRepositories)) {
		return nil, fmt.Errorf("cold restore has %d filesystem streams for %d repositories", args.FilesystemConnections, len(l.ttcVolumeRepositories))
	}
	payloadMemoryFiles := args.FilePayload.Files[:len(args.FilePayload.Files)-args.FilesystemConnections]
	filesystemFiles := args.FilePayload.Files[len(payloadMemoryFiles):]
	if len(payloadMemoryFiles) != 3+boolInt(args.GraphRemote) {
		return nil, errors.New("graph dehydration restore requires metadata, manifest, object directory, and optional graph relay")
	}
	if _, err := l.attachColdFilesystemSources(filesystemFiles, args.FilesystemToken, args.FilesystemBranch); err != nil {
		return nil, err
	}
	args.FilePayload.Files = payloadMemoryFiles
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
		reservedMemoryFile, reservedHuge := l.distributedMemoryFile, l.distributedMemoryFileHuge
		l.distributedMemoryFile, l.distributedMemoryFileHuge = nil, false
		mainMF, err = createMemoryFileFromFile(l.root.conf.AppHugePages, hugeTmpfsTHP(l.hostTHP, reservedHuge), reservedMemoryFile, false)
		if err == nil {
			p, err = l.restoredKernelPlatform()
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
		memoryFiles[resourceID] = mf
		delete(restoreMnts.mfmap, resourceID)
	}
	if len(restoreMnts.mfmap) != 0 {
		mainMF.Destroy()
		for _, private := range restoreMnts.mfmap {
			private.Destroy()
		}
		return nil, fmt.Errorf("dehydration target has %d unexpected private memory files", len(restoreMnts.mfmap))
	}
	// As a distributed restore's: the physical identity of each repository,
	// the one the bundle carries as an alias, and those the daemon names for
	// the capture restored, which saved its filesystems under the identities
	// of the repositories its Sentry had.
	resources := make(vfs.RestoreFilesystemResources, len(l.ttcVolumeRepositories))
	for _, donated := range l.ttcVolumeRepositories {
		info := donated.repository.Info()
		resource := vfs.RestoreFilesystemResource{Resource: donated.repository, Branch: donated.config.Branch}
		resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: info.ID}] = resource
		if restoreID := donated.config.RestoreID; restoreID != "" {
			resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = resource
		}
	}
	resources, err = aliasRestoreFilesystemResources(resources, args.FilesystemRestoreIDs)
	if err != nil {
		for _, mf := range memoryFiles {
			mf.Destroy()
		}
		return nil, err
	}
	loadOptions := kernel.DurableDehydrationLoadOptions{
		Clocks: l.newCalibratedClocks(), BranchID: args.FilesystemBranch,
		Quarantined: args.Quarantined, OverrideQuarantine: true,
		RestoreFilesystemResources: resources, RestoreFilesystemFDMap: restoreMnts.fdmap,
		ConfigureNetwork: func(k *kernel.Kernel, stack inet.Stack) error {
			return l.configureNetworkWithArgs(k, stack, args.NetworkArgs)
		},
	}
	manifestData, readErr := io.ReadAll(payloadMemoryFiles[1])
	if readErr != nil {
		return nil, fmt.Errorf("reading graph memory manifest: %w", readErr)
	}
	manifest, decodeErr := ttcchunk.DecodeManifest(manifestData)
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding graph memory manifest: %w", decodeErr)
	}
	chunks, chunksErr := ttcchunk.GraphChunkObjects(manifest)
	if chunksErr != nil {
		return nil, chunksErr
	}
	data, openErr := ttcsnapshot.OpenLocalDataStoreFD(payloadMemoryFiles[2])
	if openErr != nil {
		return nil, openErr
	}
	var remote *ttcsnapshot.ObjectRelayClient
	if args.GraphRemote {
		fd, dupErr := unix.Dup(int(payloadMemoryFiles[3].Fd()))
		if dupErr != nil {
			_ = data.Close()
			return nil, dupErr
		}
		remote, openErr = ttcsnapshot.NewObjectRelayClient(os.NewFile(uintptr(fd), "ttc-graph-memory-stream"), args.GraphRemoteToken)
		if openErr != nil {
			_ = data.Close()
			return nil, openErr
		}
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
			_ = data.Close()
			if remote != nil {
				_ = remote.Close()
			}
			for _, mf := range memoryFiles {
				mf.Destroy()
			}
			return nil, fmt.Errorf("preparing platform for dehydration restore: %w", err)
		}
	}
	fdmapOwned = false // Kernel load and VFS CompleteRestore consume descriptors.
	child, err := kernel.LoadGraphDurableDehydration(loadCtx, p, metadata, memoryFiles, manifest, chunks, data, remote, loadOptions)
	if err != nil {
		return nil, err
	}
	// Treat the durable branch record as authoritative. The serialized Kernel
	// contains the value captured at dehydration time, but the user may change
	// quarantine while the branch is process-free.
	if child.LiveForkQuarantine() != args.Quarantined {
		if err := child.SetLiveForkQuarantine(args.Quarantined); err != nil {
			child.Release()
			return nil, fmt.Errorf("applying dehydration-time quarantine override: %w", err)
		}
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

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// attachColdFilesystemSources attaches files, one stream for each of the
// Sentry's repositories, as the sources of branch's remote files, and
// returns what undoes the attachments, for a peer restore that fails after
// them: see ttcvolume.Repository.AttachRemoteObjectSourceUndoable.
func (l *Loader) attachColdFilesystemSources(files []*os.File, token ttcvolume.RemoteToken, branch string) (func(), error) {
	if len(files) == 0 {
		return func() {}, nil
	}
	if len(files) != len(l.ttcVolumeRepositories) {
		return func() {}, fmt.Errorf("cold restore has %d filesystem streams for %d repositories", len(files), len(l.ttcVolumeRepositories))
	}
	volumes := make([]ttcVolumeRepository, 0, len(l.ttcVolumeRepositories))
	for _, volume := range l.ttcVolumeRepositories {
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].config.Source < volumes[j].config.Source })
	var undos []func()
	undo := func() {
		for _, undo := range undos {
			undo()
		}
	}
	for index, volume := range volumes {
		fd, err := unix.Dup(int(files[index].Fd()))
		if err != nil {
			undo()
			return func() {}, fmt.Errorf("duplicating cold filesystem stream %d: %w", index, err)
		}
		stream := os.NewFile(uintptr(fd), "ttc-cold-filesystem-stream")
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(volume.config.Source)))
		detach, err := volume.repository.AttachRemoteObjectSourceUndoable(stream, token, key, branch)
		if err != nil {
			stream.Close()
			undo()
			return func() {}, fmt.Errorf("attaching cold filesystem source %q: %w", volume.config.Source, err)
		}
		undos = append(undos, detach)
	}
	return undo, nil
}

func (l *Loader) restoreDurableDehydrationPeer(args *DurableDehydrationRestoreArgs) (_ *DurableDehydrationRestoreInfo, retErr error) {
	if !l.family.HasActivePeer() {
		return nil, errNoActivePeer
	}
	releaseFilesystemRestore := pinRemoteFilesystemRestore(l.ttcVolumeRepositories)
	defer releaseFilesystemRestore()
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
	if args.FilesystemConnections < 0 || args.FilesystemConnections > len(args.FilePayload.Files)-1 || (args.FilesystemConnections != 0 && args.FilesystemConnections != len(l.ttcVolumeRepositories)) {
		return nil, fmt.Errorf("cold peer restore has %d filesystem streams for %d repositories", args.FilesystemConnections, len(l.ttcVolumeRepositories))
	}
	memoryFiles := args.FilePayload.Files[:len(args.FilePayload.Files)-args.FilesystemConnections]
	filesystemFiles := args.FilePayload.Files[len(memoryFiles):]
	detachFilesystemSources, err := l.attachColdFilesystemSources(filesystemFiles, args.FilesystemToken, args.FilesystemBranch)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			detachFilesystemSources()
			l.family.TransactionSourcesChanged()
		}
	}()
	args.FilePayload.Files = memoryFiles
	n := len(metadata.Memories)
	memorySources := 2 + boolInt(args.GraphRemote)
	wantFiles := 1 + memorySources + n
	if args.ReusePeerApplicationMemoryFile {
		wantFiles--
	}
	if got := len(args.FilePayload.Files); got != wantFiles {
		return nil, fmt.Errorf("dehydration peer import got %d files, want %d", got, wantFiles)
	}
	memoryFilesByResource := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, n)
	cleanupMemories := func() {
		for resource, mf := range memoryFilesByResource {
			if mf != nil {
				mf.Destroy()
				delete(memoryFilesByResource, resource)
			}
		}
	}
	targetStart := 3 + boolInt(args.GraphRemote)
	for i := 0; i < n; i++ {
		resource := metadata.Memories[i].ResourceID
		if args.ReusePeerApplicationMemoryFile && i == 0 {
			continue
		}
		targetIndex := targetStart + i
		if args.ReusePeerApplicationMemoryFile {
			targetIndex--
		}
		targetFD, err := unix.Dup(int(args.FilePayload.Files[targetIndex].Fd()))
		if err != nil {
			cleanupMemories()
			return nil, err
		}
		target := os.NewFile(uintptr(targetFD), fmt.Sprintf("dehydration-peer-memory-%d", i))
		var mf *pgalloc.MemoryFile
		if i == 0 {
			mf, err = createPrePrimedMemoryFileFromFile(l.root.conf.AppHugePages, l.hostTHP, target, true)
		} else {
			mf, err = createEmptyPrivateMemoryFile(target, resource)
		}
		if err != nil {
			cleanupMemories()
			return nil, fmt.Errorf("creating dehydration peer memory %d: %w", i, err)
		}
		memoryFilesByResource[resource] = mf
	}
	consumed := false
	defer func() {
		if !consumed {
			cleanupMemories()
		}
	}()
	err = l.family.RestoreDurablePeer(args, func(source *kernel.Kernel, resources vfs.LiveForkFilesystemResources) (*kernel.Kernel, error) {
		resources, err := aliasLiveForkFilesystemResources(resources, args.FilesystemRestoreIDs)
		if err != nil {
			return nil, err
		}
		// Branches in one Sentry share the platform. In particular, systrap's
		// stub mapping and source subprocess are process-global; constructing a
		// second platform here can collide with the active platform's mappings.
		loadOptions := kernel.DurableDehydrationLoadOptions{
			Clocks: l.newCalibratedClocks(), BranchID: args.FilesystemBranch,
			Quarantined: args.Quarantined, OverrideQuarantine: true,
			FilesystemResources: resources, ConfigureNetwork: func(k *kernel.Kernel, stack inet.Stack) error {
				return l.configureNetworkWithArgs(k, stack, args.NetworkArgs)
			},
		}
		manifestData, err := io.ReadAll(args.FilePayload.Files[1])
		if err != nil {
			return nil, fmt.Errorf("reading graph peer memory manifest: %w", err)
		}
		graphManifest, err := ttcchunk.DecodeManifest(manifestData)
		if err != nil {
			return nil, fmt.Errorf("decoding graph peer memory manifest: %w", err)
		}
		graphChunks, err := ttcchunk.GraphChunkObjects(graphManifest)
		if err != nil {
			return nil, err
		}
		graphData, err := ttcsnapshot.OpenLocalDataStoreFD(args.FilePayload.Files[2])
		if err != nil {
			return nil, err
		}
		var graphRemote *ttcsnapshot.ObjectRelayClient
		if args.GraphRemote {
			fd, dupErr := unix.Dup(int(args.FilePayload.Files[3].Fd()))
			if dupErr != nil {
				_ = graphData.Close()
				return nil, dupErr
			}
			graphRemote, err = ttcsnapshot.NewObjectRelayClient(os.NewFile(uintptr(fd), "ttc-graph-peer-memory-stream"), args.GraphRemoteToken)
			if err != nil {
				_ = graphData.Close()
				return nil, err
			}
		}
		if args.ReusePeerApplicationMemoryFile {
			if source.MemoryFile() == nil {
				_ = graphData.Close()
				if graphRemote != nil {
					_ = graphRemote.Close()
				}
				return nil, errors.New("active peer has no application MemoryFile")
			}
			source.MemoryFile().IncOwner()
			memoryFilesByResource[checkpoint.ResourceID{}] = source.MemoryFile()
		}
		consumed = true
		child, err := kernel.LoadGraphDurableDehydration(source.SupervisorContext(), source.Platform, metadata, memoryFilesByResource, graphManifest, graphChunks, graphData, graphRemote, loadOptions)
		memoryFilesByResource = nil
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
	return f.restorePeer(args.ID, args.ParentID, args.FilesystemBranch, args.Quarantined, args.Resume, args.ExecSessions, load)
}

// RestoreDistributedPeer imports a remotely captured branch into the existing
// Sentry and application MemoryFile. BranchID is the user-visible identity;
// filesystemBranch is the globally unique repository branch prepared by the
// source transaction.
func (f *SandboxFamily) RestoreDistributedPeer(branchID, filesystemBranch string, quarantined bool, sessions []ExecSessionInfo, load func(*kernel.Kernel, vfs.LiveForkFilesystemResources) (*kernel.Kernel, error)) error {
	if branchID == "" || filesystemBranch == "" || load == nil {
		return errors.New("distributed peer restore is missing branch, filesystem, or loader")
	}
	return f.restorePeer(branchID, "", filesystemBranch, quarantined, false, sessions, load)
}

// errNoActivePeer is a peer restore's refusal for want of a running or paused
// branch to restore beside.
var errNoActivePeer = errors.New("no active peer is available for dehydration restore")

// HasActivePeer reports whether a branch runs, or is paused, that a peer
// restore can restore beside. A restore looks before it attaches anything
// that its failure would have to undo.
func (f *SandboxFamily) HasActivePeer() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, branch := range f.branches {
		if branch.k != nil && (branch.state == BranchRunning || branch.state == BranchPaused) {
			return true
		}
	}
	return false
}

func (f *SandboxFamily) restorePeer(id, parentID, filesystemBranch string, quarantined, resume bool, sessions []ExecSessionInfo, load func(*kernel.Kernel, vfs.LiveForkFilesystemResources) (*kernel.Kernel, error)) error {
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
		return errNoActivePeer
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
	newSessions, err := restoredExecSessions(child, sessions)
	if err != nil {
		child.Release()
		return fmt.Errorf("restoring dehydrated exec sessions: %w", err)
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
		quarantined: quarantined,
	}
	f.branches[id] = b
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	for id, session := range newSessions {
		f.watchExecSession(b.id, id, session)
	}
	return nil
}
