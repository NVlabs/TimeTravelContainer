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
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
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
	// SharedSnapshot selects the first donated file as an immutable packed
	// memory stream. Network connections are then used only to fetch metadata.
	SharedSnapshot bool
	// SharedMetadata selects the first donated file as an immutable
	// SessionMetadata envelope. This removes the source-process dependency for
	// forks of dehydrated branches.
	SharedMetadata bool
	// DurableDehydration interprets SharedMetadata using the durable
	// dehydration format and restores all captured private MemoryFiles.
	DurableDehydration bool
	// SparseManifest selects the first donated file as a TTC sparse chunk
	// repository. Network connections are then used only to fetch metadata.
	SparseManifest ttcchunk.ManifestID
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
	var sharedSnapshot *os.File
	var sparseRepository *ttcchunk.MemoryRepository
	sparse := args.SparseManifest != (ttcchunk.ManifestID{})
	if sparse && args.SharedSnapshot {
		return nil, errors.New("packed and sparse distributed snapshots are mutually exclusive")
	}
	if args.SharedSnapshot {
		if len(files) == 0 {
			return nil, errors.New("distributed target is missing its shared memory snapshot")
		}
		sharedSnapshot = files[0]
		files = files[1:]
	}
	if sparse {
		if len(files) == 0 {
			return nil, errors.New("distributed target is missing its sparse memory repository")
		}
		var err error
		sparseRepository, err = ttcchunk.OpenMemoryRepositoryFD(files[0])
		if err != nil {
			return nil, fmt.Errorf("opening distributed sparse memory repository: %w", err)
		}
		files = files[1:]
	}
	var donated []net.Conn
	donatedMode := len(files) != 0
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
	var pushConn net.Conn
	if !args.SharedSnapshot && !sparse && args.PushPolicy != "" && args.PushPolicy != ttcremote.TCPPushDemandOnly {
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
		if sharedMetadata == nil || !sparse {
			return nil, errors.New("distributed durable dehydration requires shared metadata and sparse memory")
		}
		durableMetadata, err = kernel.ReadDurableDehydrationMetadata(sharedMetadata)
		if err == nil && durableMetadata.SparseManifest != args.SparseManifest {
			err = fmt.Errorf("durable dehydration manifest %s does not match requested %s", durableMetadata.SparseManifest, args.SparseManifest)
		}
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
		metadata, err = ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimits{
			MaxTotalBytes: args.MetadataBytes, MaxKernelBytes: args.MetadataBytes,
			MaxAllocatorBytes: args.MetadataBytes, MaxLoadRanges: 1 << 20,
		})
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
	if args.SharedSnapshot {
		if len(donated) != 0 {
			return nil, fmt.Errorf("shared distributed snapshot has %d unused page connections", len(donated))
		}
		fd, err := unix.Dup(int(sharedSnapshot.Fd()))
		if err != nil {
			return nil, fmt.Errorf("duplicating shared distributed snapshot: %w", err)
		}
		reader = stateio.NewFDReader(int32(fd), args.MaxTransferBytes, pgalloc.PagesPerSparseChunk(), args.MaxParallel)
	} else if sparse {
		if len(donated) != 0 {
			return nil, fmt.Errorf("sparse distributed snapshot has %d unused page connections", len(donated))
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
	if l.distributedMemoryFile == nil {
		return nil, errors.New("distributed target has no reserved persistent application memory file")
	}
	persistentMemoryFile := l.distributedMemoryFile
	l.distributedMemoryFile = nil
	targetMF, err := createMemoryFileFromFile(l.root.conf.AppHugePages, l.hostTHP, persistentMemoryFile, true)
	if err != nil {
		return nil, fmt.Errorf("creating distributed target memory file: %w", err)
	}
	targetMFOwned := true
	defer func() {
		if targetMFOwned {
			targetMF.Destroy()
		}
	}()
	p, err := createPlatform(l.root.conf, l.root.applicationCores, nil, l.sandboxID)
	if err != nil {
		return nil, fmt.Errorf("creating distributed target platform: %w", err)
	}
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
		restoreID := donated.config.RestoreID
		if restoreID == "" {
			restoreID = info.ID
		}
		key := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}
		resources[key] = vfs.RestoreFilesystemResource{Resource: donated.repository, Branch: donated.config.Branch}
		if filesystemBranch == "" {
			filesystemBranch = donated.config.Branch
		} else if filesystemBranch != donated.config.Branch {
			return nil, fmt.Errorf("distributed target repositories use inconsistent branches %q and %q", filesystemBranch, donated.config.Branch)
		}
	}
	targetMFOwned = false
	fdmapOwned = false // Load/CompleteRestore owns or consumes these descriptors.
	loadOpts := kernel.DistributedForkLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: args.TargetBranchID,
		RestoreFilesystemResources: resources, RestoreFilesystemFDMap: fdmap,
		DemandOnly: true, PrefetchBytes: args.PrefetchBytes, PrefetchBehindBytes: args.PrefetchBehindBytes,
		LogicalChunkDemand: !sparse, ConfigureNetwork: l.configureNetwork, OwnSparseRepository: sparse,
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
		targetMFOwned = false // LoadSparseDurableDehydration consumes all files.
		mfmapOwned = false    // LoadSparseDurableDehydration consumes all files.
		child, err = kernel.LoadSparseDurableDehydration(l.k.SupervisorContext(), p, durableMetadata,
			memoryFiles, sparseRepository, kernel.DurableDehydrationLoadOptions{
				Clocks: sentrytime.NewCalibratedClocks(), BranchID: filesystemBranch,
				RestoreFilesystemResources: resources, RestoreFilesystemFDMap: fdmap,
				ConfigureNetwork: l.configureNetwork, OwnSparseRepository: true,
			})
	} else if sparse {
		child, err = kernel.LoadSparseDistributedFork(l.k.SupervisorContext(), p, targetMF,
			metadata.KernelMetadata, sparseRepository, args.SparseManifest, loadOpts)
	} else {
		readerOwned = false
		child, err = kernel.LoadDistributedFork(l.k.SupervisorContext(), p, targetMF,
			metadata.KernelMetadata, metadata.AllocatorMetadata, metadata.LoadRanges, reader, loadOpts)
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
	child.RegisterContainerName(l.sandboxID, l.root.containerName)
	dogOpts := watchdog.DefaultOpts
	if err := dogOpts.TaskTimeoutAction.Set(l.root.conf.WatchdogAction); err != nil {
		return nil, err
	}
	newWatchdog := watchdog.New(child, dogOpts)
	old := l.k
	old.Pause()
	if err := old.StartPaused(); err != nil {
		return nil, fmt.Errorf("starting placeholder kernel for release: %w", err)
	}
	if err := l.family.ReplaceDefaultKernel(child); err != nil {
		old.Release()
		return nil, err
	}
	if filesystemBranch != "" {
		if err := l.family.SetDefaultFilesystemBranch(filesystemBranch); err != nil {
			old.Release()
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
	old.Release()
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
