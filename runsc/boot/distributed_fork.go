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
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
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
	if args == nil || args.SessionID == "" || args.FamilyID == "" || args.TargetBranchID == "" || args.Address == "" {
		return nil, errors.New("distributed target restore is missing session, family, branch, or source address")
	}
	if args.MetadataBytes == 0 || args.MaxTransferBytes == 0 || args.MaxParallel <= 0 || args.Timeout <= 0 || args.RetryLimit < 0 {
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

	var donated []net.Conn
	donatedMode := len(args.FilePayload.Files) != 0
	for _, file := range args.FilePayload.Files {
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
	var encoded []byte
	var err error
	if donatedMode {
		encoded, err = ttcremote.FetchTCPMetadataConn(l.k.SupervisorContext(), donated[0], args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
		donated[0].Close()
		donated = donated[1:]
	} else {
		encoded, err = ttcremote.FetchTCPMetadata(l.k.SupervisorContext(), args.Address, args.SnapshotToken, args.MaxTransferBytes, args.MetadataBytes, args.Timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching distributed metadata: %w", err)
	}
	if uint64(len(encoded)) != args.MetadataBytes {
		return nil, fmt.Errorf("distributed metadata size is %d, want %d", len(encoded), args.MetadataBytes)
	}
	metadata, err := ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimits{
		MaxTotalBytes: args.MetadataBytes, MaxKernelBytes: args.MetadataBytes,
		MaxAllocatorBytes: args.MetadataBytes, MaxLoadRanges: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	if metadata.FamilyID != args.FamilyID || metadata.TargetBranchID != args.TargetBranchID {
		return nil, fmt.Errorf("distributed metadata identity %q/%q does not match %q/%q", metadata.FamilyID, metadata.TargetBranchID, args.FamilyID, args.TargetBranchID)
	}
	readerOpts := ttcremote.TCPRemoteReaderOpts{
		MaxReadBytes: args.MaxTransferBytes, MaxRanges: 64, MaxParallel: args.MaxParallel,
		Timeout: args.Timeout, RetryLimit: args.RetryLimit, RetryBackoff: 10 * time.Millisecond,
		AckBatchPages: 256, AckInterval: time.Millisecond,
	}
	var reader *ttcremote.TCPRemoteReader
	if donatedMode {
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
	readerOwned := true
	defer func() {
		if readerOwned {
			_ = reader.Close()
		}
	}()
	targetMF, err := createMemoryFile(l.root.conf.AppHugePages, l.hostTHP)
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
	fdmap := make(map[checkpoint.ResourceID]int)
	mfmap := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
	mounter := l.newContainerMounter(&l.root)
	if err := mounter.configureRestore(fdmap, mfmap); err != nil {
		return nil, fmt.Errorf("configuring distributed target filesystems: %w", err)
	}
	if len(mfmap) != 0 {
		for _, mf := range mfmap {
			mf.Destroy()
		}
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
	for _, donated := range l.ttcVolumeRepositories {
		info := donated.repository.Info()
		restoreID := donated.config.RestoreID
		if restoreID == "" {
			restoreID = info.ID
		}
		key := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: restoreID}
		resources[key] = vfs.RestoreFilesystemResource{Resource: donated.repository, Branch: donated.config.Branch}
	}
	readerOwned = false
	targetMFOwned = false
	fdmapOwned = false // Load/CompleteRestore owns or consumes these descriptors.
	child, err := kernel.LoadDistributedFork(l.k.SupervisorContext(), p, targetMF,
		metadata.KernelMetadata, metadata.AllocatorMetadata, metadata.LoadRanges, reader,
		kernel.DistributedForkLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: args.TargetBranchID,
			RestoreFilesystemResources: resources, RestoreFilesystemFDMap: fdmap,
			DemandOnly: true, PrefetchBytes: args.PrefetchBytes, PrefetchBehindBytes: args.PrefetchBehindBytes,
			ConfigureNetwork: l.configureNetwork,
		})
	if err != nil {
		return nil, err
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
	l.k = child
	l.root.procArgs = kernel.CreateProcessArgs{}
	l.watchdog.Stop()
	l.watchdog = newWatchdog
	l.restoreDone = sync.NewCond(&l.mu)
	l.distributedRestore = true
	l.distributedRestoreSessionID = args.SessionID
	l.state = restoringUnstarted
	old.Release()
	childOwned = false
	var remoteBytes uint64
	for _, load := range metadata.LoadRanges {
		remoteBytes += load.Target.Length()
	}
	info := DistributedForkRestoreInfo{
		SessionID: args.SessionID, FamilyID: args.FamilyID, TargetBranchID: args.TargetBranchID,
		RemotePages: remoteBytes / hostarch.PageSize, RemoteBytes: remoteBytes,
	}
	l.distributedRestoreInfo = info
	return &info, nil
}
