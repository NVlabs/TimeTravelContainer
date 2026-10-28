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
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
)

// DistributedForkRestoreArgs is delivered through a private inherited FD by
// runsc's internal control command. SnapshotToken must not be placed in argv,
// logs, or persistent container metadata.
type DistributedForkRestoreArgs struct {
	urpc.FilePayload
	SessionID           string
	FamilyID            string
	TargetBranchID      string
	Quarantined         bool
	Address             string
	SnapshotToken       ttcremote.SnapshotToken
	MetadataBytes       uint64
	MaxTransferBytes    uint64
	MaxParallel         int
	Timeout             time.Duration
	RetryLimit          int
	PrefetchBytes       uint64
	PrefetchBehindBytes uint64
	AckBatchPages       int
	AckInterval         time.Duration
	PushPolicy          ttcremote.TCPPushPolicy
	// SharedMetadata selects the first donated file as an immutable
	// SessionMetadata envelope. This removes the source-process dependency for
	// forks of dehydrated branches.
	SharedMetadata bool
	// DurableDehydration interprets SharedMetadata using the durable
	// dehydration format and restores all captured private MemoryFiles.
	DurableDehydration bool
	// GraphSparse selects a graph-native manifest, local complete-object
	// directory, and optional resolver relay.
	GraphSparse      bool
	GraphRemote      bool
	GraphRemoteToken ttcsnapshot.ObjectRelayToken
	// Peer imports the captured branch into an already-running SandboxFamily
	// instead of replacing a newly-created placeholder kernel.
	Peer bool
	// VisibleBranchID is the target-local CLI branch name. TargetBranchID remains
	// the immutable transaction identity recorded in captured kernel metadata.
	VisibleBranchID string
	// FilesystemBranch is the prepared target branch in each TTC repository.
	FilesystemBranch string
	// FilesystemConnections is the prefix of donated TCP streams used for
	// authenticated TTC-volume range requests.
	FilesystemConnections int
	// FilesystemRestoreIDs maps each target-local repository identity to the
	// repository identity serialized in the cold source metadata.
	FilesystemRestoreIDs map[string]string
	// ExecSessions preserves branch-local control identities for exec task
	// groups already present in the captured Kernel.
	ExecSessions []ExecSessionInfo
}

// pinRemoteFilesystemRestore protects freshly attached lazy sources until the
// complete peer restore has finished. Repositories may appear more than once
// in the mount table, so pin each physical repository exactly once.
func pinRemoteFilesystemRestore(volumes map[string]ttcVolumeRepository) func() {
	seen := make(map[*ttcvolume.Repository]struct{}, len(volumes))
	releases := make([]func(), 0, len(volumes))
	for _, volume := range volumes {
		if volume.repository == nil {
			continue
		}
		if _, ok := seen[volume.repository]; ok {
			continue
		}
		seen[volume.repository] = struct{}{}
		releases = append(releases, volume.repository.PinRemoteRestore())
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}

// DistributedForkRestoreInfo reports the hidden target's initial dependency.
type DistributedForkRestoreInfo struct {
	SessionID      string
	FamilyID       string
	TargetBranchID string
	RemotePages    uint64
	RemoteBytes    uint64
}

func (l *Loader) restoreDistributedFork(args *DistributedForkRestoreArgs) (*DistributedForkRestoreInfo, error) {
	if args == nil || args.SessionID == "" || args.FamilyID == "" || args.TargetBranchID == "" || (!args.SharedMetadata && args.Address == "") {
		return nil, errors.New("distributed target restore is missing session, family, branch, or source address")
	}
	if args.MetadataBytes == 0 || args.MaxTransferBytes == 0 || args.MaxParallel <= 0 || args.Timeout <= 0 || args.RetryLimit < 0 || args.AckBatchPages <= 0 || args.AckInterval < 0 {
		return nil, errors.New("distributed target restore has invalid transfer bounds")
	}
	if args.Peer {
		if args.VisibleBranchID == "" || args.FilesystemBranch == "" {
			return nil, errors.New("distributed peer restore is missing visible or filesystem branch identity")
		}
		l.mu.Lock()
		state := l.state
		l.mu.Unlock()
		if state != started && state != restored {
			return nil, fmt.Errorf("sandbox is %s, not available for distributed peer import", state)
		}
		return l.restoreDistributedForkPeer(args)
	}
	l.restoring.Store(true)
	defer l.restoring.Store(false)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.distributedRestore && l.distributedRestoreSessionID == args.SessionID {
		info := l.distributedRestoreInfo
		return &info, nil
	}
	if l.state != created || l.distributedRestore {
		return nil, fmt.Errorf("sandbox is %s, not an unstarted distributed target", l.state)
	}

	files := args.FilePayload.Files
	var sharedMetadata *os.File
	if args.SharedMetadata {
		if len(files) == 0 {
			return nil, errors.New("distributed target is missing its shared metadata")
		}
		sharedMetadata = files[0]
		files = files[1:]
	}
	var graphManifest ttcchunk.Manifest
	var graphChunks map[ttcchunk.ID]ttcsnapshot.ObjectID
	var graphData *ttcsnapshot.LocalDataStore
	var graphRemote *ttcsnapshot.ObjectRelayClient
	graphOwned := false
	if args.GraphSparse {
		want := 2
		if args.GraphRemote {
			want++
		}
		if len(files) < want {
			return nil, errors.New("distributed target is missing graph memory descriptors")
		}
		encodedManifest, readErr := io.ReadAll(files[0])
		if readErr != nil {
			return nil, fmt.Errorf("reading distributed graph manifest: %w", readErr)
		}
		graphManifest, readErr = ttcchunk.DecodeManifest(encodedManifest)
		if readErr != nil {
			return nil, fmt.Errorf("decoding distributed graph manifest: %w", readErr)
		}
		graphChunks, readErr = ttcchunk.GraphChunkObjects(graphManifest)
		if readErr != nil {
			return nil, readErr
		}
		graphData, readErr = ttcsnapshot.OpenLocalDataStoreFD(files[1])
		if readErr != nil {
			return nil, readErr
		}
		files = files[2:]
		if args.GraphRemote {
			fd, dupErr := unix.Dup(int(files[0].Fd()))
			if dupErr != nil {
				graphData.Close()
				return nil, dupErr
			}
			graphRemote, readErr = ttcsnapshot.NewObjectRelayClient(os.NewFile(uintptr(fd), "distributed-graph-memory-stream"), args.GraphRemoteToken)
			if readErr != nil {
				graphData.Close()
				return nil, readErr
			}
			files = files[1:]
		}
		graphOwned = true
		defer func() {
			if graphOwned {
				_ = graphData.Close()
				if graphRemote != nil {
					_ = graphRemote.Close()
				}
			}
		}()
	}
	var donated []net.Conn
	for _, file := range files {
		conn, err := ttcremote.NewDonatedTCPConn(file)
		if err != nil {
			for _, opened := range donated {
				opened.Close()
			}
			return nil, fmt.Errorf("opening donated distributed page connection: %w", err)
		}
		donated = append(donated, conn)
	}
	donatedOwned := true
	defer func() {
		if donatedOwned {
			for _, conn := range donated {
				conn.Close()
			}
		}
	}()
	if args.FilesystemConnections < 0 || args.FilesystemConnections > len(donated) || args.FilesystemConnections != len(l.ttcVolumeRepositories) {
		return nil, fmt.Errorf("distributed target has %d filesystem streams for %d repositories", args.FilesystemConnections, len(l.ttcVolumeRepositories))
	}
	var filesystemToken ttcvolume.RemoteToken
	copy(filesystemToken[:], args.SnapshotToken[:])
	connectionIndex := 0
	for _, volume := range l.ttcVolumeRepositories {
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(volume.config.Source)))
		if err := volume.repository.AttachRemoteObjectSource(donated[connectionIndex], filesystemToken, key, volume.config.Branch); err != nil {
			return nil, fmt.Errorf("attaching distributed filesystem source %q: %w", volume.config.Source, err)
		}
		connectionIndex++
	}
	donated = donated[args.FilesystemConnections:]
	donatedMode := len(donated) != 0
	var pushConn net.Conn
	if !args.GraphSparse && args.PushPolicy != "" && args.PushPolicy != ttcremote.TCPPushDemandOnly {
		if !donatedMode || len(donated) < 2 {
			return nil, errors.New("distributed reverse push requires a dedicated donated stream")
		}
		pushConn = donated[len(donated)-1]
		donated = donated[:len(donated)-1]
		defer func() {
			if pushConn != nil {
				pushConn.Close()
			}
		}()
	}
	var metadata *ttcremote.SessionMetadata
	var durableMetadata kernel.DurableDehydrationMetadata
	var encoded []byte
	var err error
	if args.DurableDehydration {
		if sharedMetadata == nil || !args.GraphSparse {
			return nil, errors.New("distributed durable dehydration requires shared metadata and graph memory")
		}
		durableMetadata, err = kernel.ReadDurableDehydrationMetadata(sharedMetadata)
	} else if sharedMetadata != nil {
		encoded, err = io.ReadAll(io.LimitReader(sharedMetadata, int64(args.MetadataBytes)+1))
	} else if donatedMode {
		encoded, err = ttcremote.FetchTCPMetadataConn(l.k.SupervisorContext(), donated[0], args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
		donated[0].Close()
		donated = donated[1:]
	} else {
		encoded, err = ttcremote.FetchTCPMetadata(l.k.SupervisorContext(), args.Address, args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching distributed metadata: %w", err)
	}
	if !args.DurableDehydration {
		if uint64(len(encoded)) != args.MetadataBytes {
			return nil, fmt.Errorf("distributed metadata size is %d, want %d", len(encoded), args.MetadataBytes)
		}
		metadata, err = ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimitsFor(args.MetadataBytes))
		if err != nil {
			return nil, err
		}
		if metadata.FamilyID != args.FamilyID || metadata.TargetBranchID != args.TargetBranchID {
			return nil, fmt.Errorf("distributed metadata identity %q/%q does not match %q/%q", metadata.FamilyID, metadata.TargetBranchID, args.FamilyID, args.TargetBranchID)
		}
	}
	readerOpts := ttcremote.TCPRemoteReaderOpts{
		MaxReadBytes: args.MaxTransferBytes, MaxRanges: pgalloc.PagesPerSparseChunk(), MaxParallel: args.MaxParallel,
		Timeout: args.Timeout, RetryLimit: args.RetryLimit, RetryBackoff: 10 * time.Millisecond,
		AckBatchPages: args.AckBatchPages, AckInterval: args.AckInterval,
	}
	var reader stateio.AsyncReader
	if args.GraphSparse {
		if len(donated) != 0 {
			return nil, fmt.Errorf("graph distributed snapshot has %d unused page connections", len(donated))
		}
	} else if donatedMode {
		var tcpReader *ttcremote.TCPRemoteReader
		tcpReader, err = ttcremote.NewTCPRemoteReaderWithConns(args.Address, args.SnapshotToken, readerOpts, donated)
		reader = tcpReader
		if err == nil {
			donatedOwned = false
		}
	} else {
		reader, err = ttcremote.NewTCPRemoteReaderWithOpts(args.Address, args.SnapshotToken, readerOpts)
	}
	if err != nil {
		return nil, err
	}
	readerOwned := reader != nil
	defer func() {
		if readerOwned && reader != nil {
			_ = reader.Close()
		}
	}()
	var (
		p        platform.Platform
		reusable platform.RestoreReuser
		targetMF *pgalloc.MemoryFile
	)
	if candidate, ok := l.k.Platform.(platform.RestoreReuser); ok {
		// VM platforms are process-scoped. Reuse the bootstrap VM and allocate
		// restored application memory from its GuestRAM instead of constructing a
		// second VM around a host-backed reserved memfd.
		reusable = candidate
		p = candidate
		if l.distributedMemoryFile != nil {
			_ = l.distributedMemoryFile.Close()
			l.distributedMemoryFile = nil
		}
		targetMF, err = createMemoryFileForPlatform(p, l.root.conf.AppHugePages, l.hostTHP)
	} else {
		if l.distributedMemoryFile == nil {
			return nil, errors.New("distributed target has no reserved application memory file")
		}
		reservedMemoryFile, reservedHuge := l.distributedMemoryFile, l.distributedMemoryFileHuge
		l.distributedMemoryFile, l.distributedMemoryFileHuge = nil, false
		targetMF, err = createMemoryFileFromFile(l.root.conf.AppHugePages, hugeTmpfsTHP(l.hostTHP, reservedHuge), reservedMemoryFile, false)
		if err == nil {
			p, err = l.restoredKernelPlatform()
		}
	}
	if err != nil {
		if targetMF != nil {
			targetMF.Destroy()
		}
		return nil, fmt.Errorf("creating distributed target platform and application memory: %w", err)
	}
	targetMFOwned := true
	defer func() {
		if targetMFOwned {
			targetMF.Destroy()
		}
	}()
	restoreMnts := restoreMounts{
		fdmap:     make(map[checkpoint.ResourceID]int),
		mfmap:     make(map[checkpoint.ResourceID]*pgalloc.MemoryFile),
		sharedMfs: make(map[string]bool),
	}
	fdmap := restoreMnts.fdmap
	mfmap := restoreMnts.mfmap
	mfmapOwned := true
	defer func() {
		if mfmapOwned {
			for _, mf := range mfmap {
				mf.Destroy()
			}
		}
	}()
	mounter := l.newContainerMounter(&l.root)
	if err := mounter.configureRestore(&restoreMnts); err != nil {
		return nil, fmt.Errorf("configuring distributed target filesystems: %w", err)
	}
	if len(mfmap) != 0 && !args.DurableDehydration {
		for _, mf := range mfmap {
			mf.Destroy()
		}
		mfmapOwned = false
		for _, hostFD := range fdmap {
			_ = unix.Close(hostFD)
		}
		return nil, fmt.Errorf("distributed target unexpectedly requires %d private filesystem memory files", len(mfmap))
	}
	fdmapOwned := true
	defer func() {
		if fdmapOwned {
			for _, hostFD := range fdmap {
				_ = unix.Close(hostFD)
			}
		}
	}()
	resources := make(vfs.RestoreFilesystemResources, len(l.ttcVolumeRepositories))
	filesystemBranch := ""
	for _, donated := range l.ttcVolumeRepositories {
		info := donated.repository.Info()
		resource := vfs.RestoreFilesystemResource{Resource: donated.repository, Branch: donated.config.Branch}
		// Always publish the physical node-local identity. The bundle config can
		// carry the identity used by an earlier restore, so retain that as an
		// alias rather than letting it replace the current identity.
		resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: info.ID}] = resource
		if restoreID := donated.config.RestoreID; restoreID != "" {
			resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = resource
		}
		if filesystemBranch == "" {
			filesystemBranch = donated.config.Branch
		} else if filesystemBranch != donated.config.Branch {
			return nil, fmt.Errorf("distributed target repositories use inconsistent branches %q and %q", filesystemBranch, donated.config.Branch)
		}
	}
	resources, err = aliasRestoreFilesystemResources(resources, args.FilesystemRestoreIDs)
	if err != nil {
		return nil, err
	}
	fdmapOwned = false // Load/CompleteRestore owns or consumes these descriptors.
	loadOpts := kernel.DistributedForkLoadOptions{
		Clocks: l.newCalibratedClocks(), ChildBranchID: args.TargetBranchID,
		Quarantined: args.Quarantined, OverrideQuarantine: true,
		RestoreFilesystemResources: resources, RestoreFilesystemFDMap: fdmap,
		DemandOnly: true, PrefetchBytes: args.PrefetchBytes, PrefetchBehindBytes: args.PrefetchBehindBytes,
		LogicalChunkDemand: !args.GraphSparse, ConfigureNetwork: l.configureNetwork,
	}
	loadCtx := l.k.SupervisorContext()
	old := l.k
	bootstrapReleased := false
	if reusable != nil {
		// Hypervisor.framework permits one VM per process. Releasing the
		// bootstrap Kernel drops all of its AddressSpaces; PrepareRestore then
		// resets the disposable monitor and execution caches for the imported
		// Kernel without changing the GuestRAM provider selected above.
		old.Release()
		bootstrapReleased = true
		if err := reusable.PrepareRestore(); err != nil {
			return nil, fmt.Errorf("preparing platform for distributed restore: %w", err)
		}
	}
	var child *kernel.Kernel
	if args.DurableDehydration {
		memoryFiles := map[checkpoint.ResourceID]*pgalloc.MemoryFile{{}: targetMF}
		for i := 1; i < len(durableMetadata.Memories); i++ {
			resource := durableMetadata.Memories[i].ResourceID
			mf := mfmap[resource]
			if mf == nil {
				return nil, fmt.Errorf("distributed dehydration target is missing private memory file %q", resource)
			}
			memoryFiles[resource] = mf
		}
		if len(mfmap) != len(memoryFiles)-1 {
			return nil, fmt.Errorf("distributed dehydration target has %d unexpected private memory files", len(mfmap))
		}
		targetMFOwned = false // LoadGraphDurableDehydration consumes all files.
		mfmapOwned = false    // LoadGraphDurableDehydration consumes all files.
		durableOpts := kernel.DurableDehydrationLoadOptions{
			Clocks: l.newCalibratedClocks(), BranchID: filesystemBranch,
			Quarantined: args.Quarantined, OverrideQuarantine: true,
			RestoreFilesystemResources: resources, RestoreFilesystemFDMap: fdmap,
			ConfigureNetwork: l.configureNetwork,
		}
		graphOwned = false
		child, err = kernel.LoadGraphDurableDehydration(loadCtx, p, durableMetadata,
			memoryFiles, graphManifest, graphChunks, graphData, graphRemote, durableOpts)
	} else {
		readerOwned = false
		child, err = kernel.LoadDistributedFork(loadCtx, p, targetMF,
			metadata.KernelMetadata, metadata.AllocatorMetadata, metadata.LoadRanges, metadata.MemoryBindings, reader, loadOpts)
	}
	if err != nil {
		return nil, err
	}
	var pushReceiver *ttcremote.TCPPushReceiver
	if pushConn != nil {
		pushReceiver, err = ttcremote.NewTCPPushStateSession(args.SnapshotToken, child.TryInstallRemotePagesState, args.MaxTransferBytes)
		if err != nil {
			return nil, err
		}
	}
	childOwned := true
	defer func() {
		if childOwned {
			child.Pause()
			if err := child.StartPaused(); err == nil {
				child.Release()
			}
		}
	}()
	for _, task := range child.TaskSet().Root.Tasks() {
		task.RestoreContainerID(l.sandboxID)
	}
	for i := range args.ExecSessions {
		args.ExecSessions[i].ContainerID = l.sandboxID
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
			return nil, fmt.Errorf("starting placeholder kernel for release: %w", err)
		}
	}
	if err := l.family.ReplaceDefaultKernel(child); err != nil {
		if !bootstrapReleased {
			old.Release()
		}
		return nil, err
	}
	if err := l.family.RestoreLoaderExecSessions(args.ExecSessions); err != nil {
		if !bootstrapReleased {
			old.Release()
		}
		return nil, err
	}
	if filesystemBranch != "" {
		if err := l.family.SetDefaultFilesystemBranch(filesystemBranch); err != nil {
			if !bootstrapReleased {
				old.Release()
			}
			return nil, err
		}
	}
	l.k = child
	l.root.procArgs = kernel.CreateProcessArgs{}
	l.watchdog.Stop()
	l.watchdog = newWatchdog
	l.restoreDone = sync.NewCond(&l.mu)
	l.distributedRestore = true
	l.distributedRestoreSessionID = args.SessionID
	l.state = restoringUnstarted
	if pushReceiver != nil {
		l.distributedPushReceiver = pushReceiver
		conn := pushConn
		pushConn = nil // receiver owns it.
		go func() { _ = pushReceiver.ServeStream(conn) }()
	}
	if !bootstrapReleased {
		old.Release()
	}
	childOwned = false
	var remoteBytes uint64
	if metadata != nil {
		for _, load := range metadata.LoadRanges {
			remoteBytes += load.Target.Length()
		}
	}
	info := DistributedForkRestoreInfo{
		SessionID: args.SessionID, FamilyID: args.FamilyID, TargetBranchID: args.TargetBranchID,
		RemotePages: remoteBytes / hostarch.PageSize, RemoteBytes: remoteBytes,
	}
	l.distributedRestoreInfo = info
	return &info, nil
}

// aliasLiveForkFilesystemResources adds the repository identities recorded by
// an incoming checkpoint as aliases for the repositories already active in
// this Sentry. A branch that has crossed nodes more than once is serialized
// against the repository identity on its immediately previous node, which is
// not necessarily the identity cached in Loader.ttcVolumeRepositories from
// the first branch that bootstrapped the local Sentry.
func aliasLiveForkFilesystemResources(resources vfs.LiveForkFilesystemResources, restoreIDs map[string]string) (vfs.LiveForkFilesystemResources, error) {
	if len(restoreIDs) == 0 {
		return resources, nil
	}
	aliased := make(vfs.LiveForkFilesystemResources, len(resources)+len(restoreIDs))
	for id, resource := range resources {
		aliased[id] = resource
	}
	for current, restoreID := range restoreIDs {
		currentID := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: current}
		resource, ok := resources[currentID]
		if !ok {
			return nil, fmt.Errorf("active TTC volume resource %q was not exported", current)
		}
		aliased[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = resource
	}
	return aliased, nil
}

// aliasRestoreFilesystemResources is the process-bootstrap counterpart of
// aliasLiveForkFilesystemResources. A returning multi-hop branch may be
// serialized against node A's repository while node B's unstarted bundle
// still names an older node-B identity. The daemon supplies the current-local
// to incoming identity edge explicitly; accepting both keys makes the mapping
// independent of how many nodes the branch has crossed.
func aliasRestoreFilesystemResources(resources vfs.RestoreFilesystemResources, restoreIDs map[string]string) (vfs.RestoreFilesystemResources, error) {
	if len(restoreIDs) == 0 {
		return resources, nil
	}
	aliased := make(vfs.RestoreFilesystemResources, len(resources)+len(restoreIDs))
	for id, resource := range resources {
		aliased[id] = resource
	}
	for current, restoreID := range restoreIDs {
		currentID := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: current}
		resource, ok := resources[currentID]
		if !ok {
			return nil, fmt.Errorf("active TTC volume restore resource %q was not exported", current)
		}
		aliased[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = resource
	}
	return aliased, nil
}

// restoreDistributedForkPeer reconstructs a branch beside an active peer. It
// deliberately reuses the peer's Platform and application MemoryFile, which is
// what makes repeated imports of identical chunks share resident host pages in
// one Sentry instead of allocating another sandbox process.
func (l *Loader) restoreDistributedForkPeer(args *DistributedForkRestoreArgs) (_ *DistributedForkRestoreInfo, retErr error) {
	if args.DurableDehydration {
		return l.restoreDistributedDurablePeer(args)
	}
	if !l.family.HasActivePeer() {
		return nil, errNoActivePeer
	}
	releaseFilesystemRestore := pinRemoteFilesystemRestore(l.ttcVolumeRepositories)
	defer releaseFilesystemRestore()
	files := args.FilePayload.Files
	var sharedMetadata *os.File
	if args.SharedMetadata {
		if len(files) == 0 {
			return nil, errors.New("distributed peer is missing shared metadata")
		}
		sharedMetadata, files = files[0], files[1:]
	}
	var donated []net.Conn
	for _, file := range files {
		conn, err := ttcremote.NewDonatedTCPConn(file)
		if err != nil {
			for _, opened := range donated {
				opened.Close()
			}
			return nil, fmt.Errorf("opening donated distributed peer connection: %w", err)
		}
		donated = append(donated, conn)
	}
	donatedOwned := true
	defer func() {
		if donatedOwned {
			for _, conn := range donated {
				conn.Close()
			}
		}
	}()
	if args.FilesystemConnections < 0 || args.FilesystemConnections > len(donated) || args.FilesystemConnections != len(l.ttcVolumeRepositories) {
		return nil, fmt.Errorf("distributed peer has %d filesystem streams for %d repositories", args.FilesystemConnections, len(l.ttcVolumeRepositories))
	}
	var filesystemToken ttcvolume.RemoteToken
	copy(filesystemToken[:], args.SnapshotToken[:])
	// A restore that fails after the attachments undoes them: see
	// ttcvolume.Repository.AttachRemoteObjectSourceUndoable.
	var detachFilesystemSources []func()
	defer func() {
		if retErr != nil && len(detachFilesystemSources) != 0 {
			for _, detach := range detachFilesystemSources {
				detach()
			}
			l.family.TransactionSourcesChanged()
		}
	}()
	connectionIndex := 0
	for _, volume := range l.ttcVolumeRepositories {
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(volume.config.Source)))
		// Loader.ttcVolumeRepositories is initialized by the branch that first
		// bootstrapped this Sentry, so volume.config.Branch may name that older
		// branch after a remote branch migrates back to the same node. Attach the
		// donated source to the freshly imported generation named by the request.
		detach, err := volume.repository.AttachRemoteObjectSourceUndoable(donated[connectionIndex], filesystemToken, key, args.FilesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("attaching distributed peer filesystem source %q branch %q: %w", volume.config.Source, args.FilesystemBranch, err)
		}
		detachFilesystemSources = append(detachFilesystemSources, detach)
		connectionIndex++
	}
	donated = donated[args.FilesystemConnections:]
	var pushConn net.Conn
	if args.PushPolicy != "" && args.PushPolicy != ttcremote.TCPPushDemandOnly {
		if len(donated) < 2 {
			return nil, errors.New("distributed peer reverse push requires a dedicated donated stream")
		}
		pushConn = donated[len(donated)-1]
		donated = donated[:len(donated)-1]
		defer func() {
			if pushConn != nil {
				pushConn.Close()
			}
		}()
	}
	var encoded []byte
	var err error
	if sharedMetadata != nil {
		encoded, err = io.ReadAll(io.LimitReader(sharedMetadata, int64(args.MetadataBytes)+1))
	} else if len(donated) != 0 {
		encoded, err = ttcremote.FetchTCPMetadataConn(l.k.SupervisorContext(), donated[0], args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
		donated[0].Close()
		donated = donated[1:]
	} else {
		encoded, err = ttcremote.FetchTCPMetadata(l.k.SupervisorContext(), args.Address, args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching distributed peer metadata: %w", err)
	}
	if uint64(len(encoded)) != args.MetadataBytes {
		return nil, fmt.Errorf("distributed peer metadata size is %d, want %d", len(encoded), args.MetadataBytes)
	}
	metadata, err := ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimitsFor(args.MetadataBytes))
	if err != nil {
		return nil, err
	}
	if metadata.FamilyID != args.FamilyID || metadata.TargetBranchID != args.TargetBranchID {
		return nil, fmt.Errorf("distributed peer metadata identity %q/%q does not match %q/%q", metadata.FamilyID, metadata.TargetBranchID, args.FamilyID, args.TargetBranchID)
	}
	readerOpts := ttcremote.TCPRemoteReaderOpts{
		MaxReadBytes: args.MaxTransferBytes, MaxRanges: pgalloc.PagesPerSparseChunk(), MaxParallel: args.MaxParallel,
		Timeout: args.Timeout, RetryLimit: args.RetryLimit, RetryBackoff: 10 * time.Millisecond,
		AckBatchPages: args.AckBatchPages, AckInterval: args.AckInterval,
	}
	var reader stateio.AsyncReader
	if len(donated) != 0 {
		reader, err = ttcremote.NewTCPRemoteReaderWithConns(args.Address, args.SnapshotToken, readerOpts, donated)
		if err == nil {
			donatedOwned = false
		}
	} else {
		reader, err = ttcremote.NewTCPRemoteReaderWithOpts(args.Address, args.SnapshotToken, readerOpts)
	}
	if err != nil {
		return nil, err
	}
	readerOwned := reader != nil
	defer func() {
		if readerOwned && reader != nil {
			_ = reader.Close()
		}
	}()
	var child *kernel.Kernel
	for i := range args.ExecSessions {
		args.ExecSessions[i].ContainerID = l.sandboxID
	}
	err = l.family.RestoreDistributedPeer(args.VisibleBranchID, args.FilesystemBranch, args.Quarantined, args.ExecSessions, func(source *kernel.Kernel, resources vfs.LiveForkFilesystemResources) (*kernel.Kernel, error) {
		resources, err = aliasLiveForkFilesystemResources(resources, args.FilesystemRestoreIDs)
		if err != nil {
			return nil, err
		}
		// The live target filesystem records its node-local repository ID, while
		// the incoming Kernel was serialized against the source repository ID.
		// Both identities refer to the same imported generation. Make the source
		// identity an alias for this restore without changing either persistent
		// repository's identity.
		for _, volume := range l.ttcVolumeRepositories {
			restoreID := volume.config.RestoreID
			if restoreID == "" {
				continue
			}
			resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = volume.repository
		}
		loadOpts := kernel.DistributedForkLoadOptions{
			// ChildBranchID selects the prepared persistent filesystem branch
			// during an in-Sentry restore. TargetBranchID is the immutable remote
			// transaction identity and is deliberately not a repository branch.
			Clocks: l.newCalibratedClocks(), ChildBranchID: args.FilesystemBranch,
			Quarantined: args.Quarantined, OverrideQuarantine: true,
			FilesystemResources: resources, DemandOnly: true,
			PrefetchBytes: args.PrefetchBytes, PrefetchBehindBytes: args.PrefetchBehindBytes,
			LogicalChunkDemand: true, ConfigureNetwork: l.configureNetwork,
		}
		readerOwned = false
		child, loadErr := kernel.LoadDistributedFork(source.SupervisorContext(), source.Platform, source.MemoryFile(), metadata.KernelMetadata, metadata.AllocatorMetadata, metadata.LoadRanges, metadata.MemoryBindings, reader, loadOpts)
		if loadErr != nil {
			return nil, loadErr
		}
		// InstallRemoteBindings intentionally reuses identical chunks already
		// present in the family MemoryFile. Retain the peer's loaders as well as
		// its pages so an unresolved reused chunk cannot lose its only backing
		// reader when another branch or source transaction completes.
		child.InheritDistributedForkMemorySources(source, metadata.LoadRanges)
		for _, task := range child.TaskSet().Root.Tasks() {
			task.RestoreContainerID(l.sandboxID)
		}
		child.RegisterContainerName(l.sandboxID, l.root.containerName)
		return child, nil
	})
	if err != nil {
		return nil, err
	}
	if pushConn != nil {
		receiver, err := ttcremote.NewTCPPushStateSession(args.SnapshotToken, child.TryInstallRemotePagesState, args.MaxTransferBytes)
		if err != nil {
			return nil, err
		}
		conn := pushConn
		pushConn = nil
		go func() { _ = receiver.ServeStream(conn) }()
	}
	var remoteBytes uint64
	for _, load := range metadata.LoadRanges {
		remoteBytes += load.Target.Length()
	}
	return &DistributedForkRestoreInfo{
		SessionID: args.SessionID, FamilyID: args.FamilyID, TargetBranchID: args.TargetBranchID,
		RemotePages: remoteBytes / hostarch.PageSize, RemoteBytes: remoteBytes,
	}, nil
}

// restoreDistributedDurablePeer imports a process-free graph dehydration
// image into an existing family. Durable metadata is not a SessionMetadata
// envelope, so it must be dispatched before the live transport decoder.
func (l *Loader) restoreDistributedDurablePeer(args *DistributedForkRestoreArgs) (_ *DistributedForkRestoreInfo, retErr error) {
	if !l.family.HasActivePeer() {
		return nil, errNoActivePeer
	}
	releaseFilesystemRestore := pinRemoteFilesystemRestore(l.ttcVolumeRepositories)
	defer releaseFilesystemRestore()
	files := args.FilePayload.Files
	if !args.SharedMetadata || len(files) == 0 || !args.GraphSparse {
		return nil, errors.New("distributed durable peer requires shared metadata and graph memory")
	}
	sharedMetadata, files := files[0], files[1:]
	if args.FilesystemConnections < 0 || args.FilesystemConnections > len(files) || (args.FilesystemConnections != 0 && args.FilesystemConnections != len(l.ttcVolumeRepositories)) {
		return nil, fmt.Errorf("distributed durable peer has %d filesystem streams for %d repositories", args.FilesystemConnections, len(l.ttcVolumeRepositories))
	}
	memoryFiles := files[:len(files)-args.FilesystemConnections]
	filesystemFiles := files[len(memoryFiles):]
	var filesystemToken ttcvolume.RemoteToken
	copy(filesystemToken[:], args.SnapshotToken[:])
	detachFilesystemSources, err := l.attachColdFilesystemSources(filesystemFiles, filesystemToken, args.FilesystemBranch)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			detachFilesystemSources()
			l.family.TransactionSourcesChanged()
		}
	}()
	files = memoryFiles
	var graphManifest ttcchunk.Manifest
	var graphChunks map[ttcchunk.ID]ttcsnapshot.ObjectID
	var graphData *ttcsnapshot.LocalDataStore
	var graphRemote *ttcsnapshot.ObjectRelayClient
	want := 2 + boolInt(args.GraphRemote)
	if len(files) < want {
		return nil, errors.New("distributed durable peer is missing graph memory descriptors")
	}
	encoded, err := io.ReadAll(files[0])
	if err != nil {
		return nil, err
	}
	graphManifest, err = ttcchunk.DecodeManifest(encoded)
	if err != nil {
		return nil, err
	}
	graphChunks, err = ttcchunk.GraphChunkObjects(graphManifest)
	if err != nil {
		return nil, err
	}
	graphData, err = ttcsnapshot.OpenLocalDataStoreFD(files[1])
	if err != nil {
		return nil, err
	}
	files = files[2:]
	if args.GraphRemote {
		fd, err := unix.Dup(int(files[0].Fd()))
		if err != nil {
			graphData.Close()
			return nil, err
		}
		graphRemote, err = ttcsnapshot.NewObjectRelayClient(os.NewFile(uintptr(fd), "distributed-durable-peer-graph-stream"), args.GraphRemoteToken)
		if err != nil {
			graphData.Close()
			return nil, err
		}
		files = files[1:]
	}
	metadataFD, err := unix.Dup(int(sharedMetadata.Fd()))
	if err != nil {
		if graphData != nil {
			graphData.Close()
		}
		if graphRemote != nil {
			graphRemote.Close()
		}
		return nil, err
	}
	metadataFile := os.NewFile(uintptr(metadataFD), "distributed-durable-peer-metadata")
	durable, err := kernel.ReadDurableDehydrationMetadata(metadataFile)
	metadataFile.Close()
	if err != nil {
		if graphData != nil {
			graphData.Close()
		}
		if graphRemote != nil {
			graphRemote.Close()
		}
		return nil, err
	}
	if got, want := len(files), len(durable.Memories)-1; got != want {
		if graphData != nil {
			graphData.Close()
		}
		if graphRemote != nil {
			graphRemote.Close()
		}
		return nil, fmt.Errorf("distributed durable peer got %d private memory files, want %d", got, want)
	}
	private := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, len(files))
	for i, file := range files {
		resource := durable.Memories[i+1].ResourceID
		mf, err := createEmptyPrivateMemoryFile(file, resource)
		if err != nil {
			for _, opened := range private {
				opened.Destroy()
			}
			if graphData != nil {
				graphData.Close()
			}
			if graphRemote != nil {
				graphRemote.Close()
			}
			return nil, fmt.Errorf("creating distributed durable peer memory %q: %w", resource, err)
		}
		private[resource] = mf
	}
	consumed := false
	defer func() {
		if !consumed {
			for _, mf := range private {
				mf.Destroy()
			}
			if graphData != nil {
				_ = graphData.Close()
			}
			if graphRemote != nil {
				_ = graphRemote.Close()
			}
		}
	}()
	for i := range args.ExecSessions {
		args.ExecSessions[i].ContainerID = l.sandboxID
	}
	err = l.family.RestoreDistributedPeer(args.VisibleBranchID, args.FilesystemBranch, args.Quarantined, args.ExecSessions, func(source *kernel.Kernel, resources vfs.LiveForkFilesystemResources) (*kernel.Kernel, error) {
		if len(args.FilesystemRestoreIDs) != 0 {
			aliased := make(vfs.LiveForkFilesystemResources, len(resources)+len(args.FilesystemRestoreIDs))
			for id, resource := range resources {
				aliased[id] = resource
			}
			for current, restoreID := range args.FilesystemRestoreIDs {
				resource, ok := resources[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: current}]
				if !ok {
					return nil, fmt.Errorf("active TTC volume resource %q was not exported", current)
				}
				aliased[checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}] = resource
			}
			resources = aliased
		}
		memories := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, len(private)+1)
		// LoadGraphDurableDehydration consumes one ownership reference for
		// every MemoryFile. Application memory is shared with the active family,
		// so donate a new owner rather than transferring the source's owner.
		source.MemoryFile().IncOwner()
		memories[checkpoint.ResourceID{}] = source.MemoryFile()
		for resource, mf := range private {
			memories[resource] = mf
		}
		loadOpts := kernel.DurableDehydrationLoadOptions{
			Clocks: l.newCalibratedClocks(), BranchID: args.FilesystemBranch,
			Quarantined: args.Quarantined, OverrideQuarantine: true,
			FilesystemResources: resources, ConfigureNetwork: l.configureNetwork,
		}
		consumed = true
		child, err := kernel.LoadGraphDurableDehydration(source.SupervisorContext(), source.Platform, durable, memories, graphManifest, graphChunks, graphData, graphRemote, loadOpts)
		if err != nil {
			return nil, err
		}
		for _, task := range child.TaskSet().Root.Tasks() {
			task.RestoreContainerID(l.sandboxID)
		}
		child.RegisterContainerName(l.sandboxID, l.root.containerName)
		return child, nil
	})
	if err != nil {
		return nil, err
	}
	return &DistributedForkRestoreInfo{SessionID: args.SessionID, FamilyID: args.FamilyID, TargetBranchID: args.TargetBranchID}, nil
}
