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

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	daemonAPIVersion       = 5
	defaultRuntimePlatform = "systrap"
	kvmGetAPIVersion       = uintptr(0xae00)
	kvmCreateVM            = uintptr(0xae01)
	kvmAPIVersion          = uintptr(12)
	daemonFrameStdin       = byte('I')
	daemonFrameStdinAck    = byte('A')
	daemonFrameEOF         = byte('C')
	daemonFrameResize      = byte('W')
	daemonFrameStdout      = byte('O')
	daemonFrameStderr      = byte('E')
	daemonFrameStatus      = byte('X')
	daemonFrameTerminal    = byte('T')
	daemonFrameTTYState    = byte('S')
	daemonMaxFrame         = 1 << 20
	daemonRootlessProbeArg = "_rootless-userns-probe"
)

const repositoryMaintenanceInterval = 5 * time.Minute

const daemonRootFormatVersion = 1

var (
	createDaemonRootlessProbe = createRootlessProbe
	daemonEffectiveUID        = os.Geteuid
	daemonKernelRelease       = currentHostKernelRelease
	daemonPlatformPreflight   = preflightDaemonPlatform
	daemonStatfs              = unix.Statfs
)

type daemonWireOptions struct {
}

type daemonRequest struct {
	Version         int                    `json:"version"`
	ClientBuildID   string                 `json:"client_build_id,omitempty"`
	Action          string                 `json:"action"`
	RequestID       string                 `json:"request_id,omitempty"`
	ClientTERM      string                 `json:"client_term,omitempty"`
	StderrTerminal  bool                   `json:"stderr_terminal,omitempty"`
	StderrWidth     int                    `json:"stderr_width,omitempty"`
	Options         daemonWireOptions      `json:"options,omitempty"`
	Args            []string               `json:"args,omitempty"`
	Stdin           bool                   `json:"stdin,omitempty"`
	ManageStop      bool                   `json:"manage_stop,omitempty"`
	Route           []string               `json:"route,omitempty"`
	PlacementNode   string                 `json:"placement_node,omitempty"`
	PlacementClaims []daemonPlacementClaim `json:"placement_claims,omitempty"`
	// DistributedLeaf marks a coordinator-generated owner-local command. It
	// prevents a family mutation from being expanded again on the destination.
	DistributedLeaf bool `json:"distributed_leaf,omitempty"`
}

// daemonPlacementClaim fences one routed branch command to the catalog
// generation observed by the entry daemon. The owner validates it again after
// taking its local lifecycle lock, so a delayed proxy cannot mutate a newer
// incarnation after migration.
type daemonPlacementClaim struct {
	FamilyID         string `json:"family_id"`
	BranchID         string `json:"branch_id"`
	OwnerNode        string `json:"owner_node"`
	OwnerIncarnation string `json:"owner_incarnation"`
	Generation       uint64 `json:"generation"`
}

type daemonServer struct {
	listener              net.Listener
	socket                string
	runtimePlatform       string
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	hostKernelRelease     string
	dataRoot              string
	localRoot             string
	root                  string
	runsc                 string
	clientExecutable      string
	volumeCapability      ttcvolume.Capability
	clusterCapabilities   *cluster.Capabilities
	clusterListen         string
	distributedSource     *distributedSourceRuntime
	clusterStore          *cluster.Store
	clusterDirectory      *cluster.NodeDirectory
	publicationQueue      *ttcstorage.Queue

	stopOnce  sync.Once
	cleanStop atomic.Bool

	reconcileMu sync.Mutex
	reconciled  map[string]bool

	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex

	eventMu  sync.Mutex
	eventSeq uint64

	stdinMu     sync.Mutex
	stdinLeases map[string]*daemonStdinLease

	sidecarMu sync.Mutex
	sidecars  map[string]*exec.Cmd

	consoleClientMu      sync.Mutex
	consoleClients       map[string]map[*daemonConsoleClient]struct{}
	closingConsoleClient map[string]int
}

type daemonConsoleClient struct {
	cancel      func()
	done        chan struct{}
	cancelOnce  sync.Once
	releaseOnce sync.Once
}

type daemonClusterOptions struct {
	Configured      bool
	Listen          string
	NodeID          string
	RepositoryID    string
	CertFile        string
	KeyFile         string
	CAFile          string
	Advertise       string
	MemoryTarget    uint64
	AutoRebalance   bool
	CapacityCommand string
	CapacityWebhook string
	LeaseDuration   time.Duration
	MaxTransactions int
	PageListen      string
	PageAdvertise   string
	Peers           []string
	PagingPolicy    ttcpolicy.Policy
}

func defaultDaemonClusterOptions() daemonClusterOptions {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	policy.AckInterval = time.Millisecond
	return daemonClusterOptions{MaxTransactions: 32, PageListen: "0.0.0.0:0", LeaseDuration: 15 * time.Second, PagingPolicy: policy}
}

type daemonClusterRuntime struct {
	listen        string
	tlsConfig     *tls.Config
	controlPlane  *cluster.ControlPlane
	source        *distributedSourceRuntime
	directory     *cluster.NodeDirectory
	nodeRecord    cluster.NodeRecord
	lease         time.Duration
	autoRebalance bool
	capacitySink  elasticCapacitySink
}

type daemonTargetRuntime struct {
	backend     *runtimeBackend
	nodeID      string
	incarnation string
	directory   *cluster.NodeDirectory
	store       *cluster.Store
	done        <-chan struct{}
	watch       sync.Map
	locks       sync.Map
}

func (r *daemonTargetRuntime) lockTarget(name string) func() {
	value, _ := r.locks.LoadOrStore(name, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (r *daemonTargetRuntime) Reserve(ctx context.Context, tx *cluster.Transaction) error {
	if r == nil || r.backend == nil || tx == nil {
		return errors.New("distributed target runtime is unavailable")
	}
	if r.directory != nil {
		record, err := r.directory.Lookup(r.nodeID)
		if err != nil {
			return fmt.Errorf("checking elastic target admission: %w", err)
		}
		if record.Incarnation != r.incarnation || !record.AcceptsPlacement(time.Now()) {
			return fmt.Errorf("cluster node %q is not accepting placement (state=%s)", r.nodeID, record.State)
		}
	}
	defer r.lockTarget(tx.Identity.TargetContainer)()
	_, err := r.backend.containerStore().PrepareDistributedFork(ctx, ttcbundle.DistributedForkOptions{
		SourceContainer: tx.Identity.SourceContainer, TargetName: tx.Identity.TargetContainer,
		TargetBranch: tx.Identity.TargetBranch, VisibleBranch: tx.Identity.VisibleTargetBranch(),
		TransactionID: tx.Identity.TransactionID, NodeID: r.nodeID,
		SourceNode: tx.Identity.SourceNodeID, Migration: tx.Identity.Migration,
	})
	return err
}

func (r *daemonTargetRuntime) Prepare(ctx context.Context, tx *cluster.Transaction, session cluster.RemoteSessionDescriptor) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, targetBranch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	info, err = r.backend.containerStore().RefreshContainerImageRoot(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("materializing target image cache: %w", err)
	}
	filesystemConnections, err := r.backend.materializeDistributedFilesystemMetadata(info, targetBranch.FilesystemBranch, tx.Identity.TransactionID, session.FilesystemMetadataPack, peer)
	if err != nil {
		return fmt.Errorf("materializing distributed filesystem metadata: %w", err)
	}
	memoryDependencyBytes := session.StreamBytes
	if session.SparseManifest != "" || session.SharedSnapshot != "" {
		memoryDependencyBytes = 0
	}
	if err := r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TransactionID, memoryDependencyBytes, session.FilesystemBytes); err != nil {
		return fmt.Errorf("recording distributed source dependency: %w", err)
	}
	var token ttcremote.SnapshotToken
	if session.SharedMetadata == "" || session.FilesystemMetadataPack != "" {
		tokenBytes, err := hex.DecodeString(session.SnapshotToken)
		if err != nil || len(tokenBytes) != 16 {
			return errors.New("invalid distributed snapshot token")
		}
		copy(token[:], tokenBytes)
	}
	restoreArgs := boot.DistributedForkRestoreArgs{
		SessionID: tx.Identity.TransactionID, FamilyID: tx.Identity.SourceContainer,
		TargetBranchID: tx.Identity.TargetBranch, Address: session.Address, SnapshotToken: token,
		Peer: peer, VisibleBranchID: targetBranch.ID, FilesystemBranch: targetBranch.FilesystemBranch,
		MetadataBytes: session.MetadataBytes, MaxTransferBytes: session.MaxTransferBytes,
		MaxParallel: tx.Negotiated.PagingPolicy.MaxConcurrency, Timeout: 30 * time.Second, RetryLimit: 2,
		AckBatchPages: tx.Negotiated.PagingPolicy.AckBatchPages, AckInterval: tx.Negotiated.PagingPolicy.AckInterval,
		FilesystemConnections: filesystemConnections,
		PushPolicy:            ttcremote.TCPPushPolicy(tx.Negotiated.PagingPolicy.Push),
		SharedSnapshot:        session.SharedSnapshot != "",
		SharedMetadata:        session.SharedMetadata != "",
		DurableDehydration:    session.DurableDehydration,
	}
	if session.SparseManifest != "" {
		manifest, err := ttcchunk.ParseManifestID(session.SparseManifest)
		if err != nil {
			return fmt.Errorf("parsing distributed sparse manifest: %w", err)
		}
		restoreArgs.SparseManifest = manifest
		restoreArgs.SparseBranch = session.SparseBranch
		if err := r.backend.containerStore().UpdateDistributedForkSparseRoot(tx.Identity.TransactionID, session.SparseRepository, session.SparseBranch); err != nil {
			return fmt.Errorf("recording distributed sparse memory root: %w", err)
		}
	}
	switch tx.Negotiated.PagingPolicy.Prefetch {
	case ttcpolicy.PrefetchSequential, ttcpolicy.PrefetchAdaptive:
		restoreArgs.PrefetchBytes = tx.Negotiated.PagingPolicy.TransferBytes - tx.Negotiated.PagingPolicy.PageSize
	case ttcpolicy.PrefetchFixed:
		windowPages := tx.Negotiated.PagingPolicy.TransferPages() - 1
		restoreArgs.PrefetchBehindBytes = (windowPages / 2) * tx.Negotiated.PagingPolicy.PageSize
		restoreArgs.PrefetchBytes = (windowPages - windowPages/2) * tx.Negotiated.PagingPolicy.PageSize
	}
	configured := r.backend.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	runtimeCreated := false
	var runtimeState struct {
		Status string `json:"status"`
	}
	stateOutput, stateErr := r.backend.command("state", info.ID)
	if stateErr == nil {
		if err := json.Unmarshal(stateOutput, &runtimeState); err != nil {
			return fmt.Errorf("decoding target runtime state: %w", err)
		}
	} else {
		bundleDir := info.BundleDir
		var cleanup func()
		if peer {
			if !tx.Identity.Migration {
				return fmt.Errorf("distributed peer target runtime is unavailable: %w", stateErr)
			}
			bundleDir, cleanup, err = r.backend.containerStore().PrepareDistributedMigrationRestoreBundle(info.ID, targetBranch.ID, targetBranch.FilesystemBranch)
			if err != nil {
				return fmt.Errorf("preparing distributed migration target bundle: %w", err)
			}
			defer cleanup()
		}
		status, err := r.backend.runConfiguredCommandWithArgs(configured, "create", "--bundle", bundleDir, info.ID)
		if err != nil {
			return fmt.Errorf("creating distributed target runtime: %w", err)
		}
		if status != 0 {
			return fmt.Errorf("creating distributed target runtime exited with status %d", status)
		}
		runtimeState.Status = "created"
		runtimeCreated = true
	}
	if peer && runtimeState.Status != "running" && runtimeState.Status != "created" {
		return fmt.Errorf("distributed peer target runtime is %q, want running", runtimeState.Status)
	}
	if runtimeState.Status == "created" || peer {
		// A metadata peer restored into a newly created empty Sentry is its
		// physical loader branch, not an import into an already-running family.
		restoreArgs.Peer = peer && !runtimeCreated
		encoded, err := json.Marshal(&restoreArgs)
		if err != nil {
			return err
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			reader.Close()
			writer.Close()
			return err
		}
		if err := writer.Close(); err != nil {
			reader.Close()
			return err
		}
		connectionCount := restoreArgs.FilesystemConnections
		if !restoreArgs.SharedMetadata {
			connectionCount++
		}
		if !restoreArgs.SharedSnapshot && restoreArgs.SparseManifest == (ttcchunk.ManifestID{}) {
			connectionCount += (restoreArgs.MaxParallel + 1) * (restoreArgs.RetryLimit + 1)
			if restoreArgs.PushPolicy != ttcremote.TCPPushDemandOnly {
				connectionCount++
			}
		}
		connectionFiles := make([]*os.File, 0, connectionCount)
		for i := 0; i < connectionCount; i++ {
			conn, err := (&net.Dialer{Timeout: restoreArgs.Timeout}).DialContext(ctx, "tcp", session.Address)
			if err != nil {
				for _, file := range connectionFiles {
					file.Close()
				}
				reader.Close()
				return fmt.Errorf("connecting donated distributed page stream %d/%d: %w", i+1, connectionCount, err)
			}
			tcp, ok := conn.(*net.TCPConn)
			if !ok {
				conn.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				reader.Close()
				return fmt.Errorf("distributed page connection is %T, want TCP", conn)
			}
			file, err := tcp.File()
			conn.Close()
			if err != nil {
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return err
			}
			// The isolated Sentry intentionally treats donated streams as plain
			// byte FDs and performs no host socket control operations. Install
			// kernel-enforced per-I/O timeouts before donating the descriptor so
			// an unreachable source cannot block a guest page fault forever.
			timeout := unix.NsecToTimeval(restoreArgs.Timeout.Nanoseconds())
			if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
				file.Close()
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return fmt.Errorf("setting donated page stream receive timeout: %w", err)
			}
			if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
				file.Close()
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return fmt.Errorf("setting donated page stream send timeout: %w", err)
			}
			connectionFiles = append(connectionFiles, file)
		}
		files := []*os.File{reader}
		connectionsStartFD := 4
		var sharedMetadata *os.File
		var sharedSnapshot *os.File
		var sparseRepository *os.File
		var durableTargets []*os.File
		durableActiveDir := ""
		keepDurableTargets := false
		defer func() {
			for _, file := range durableTargets {
				_ = file.Close()
			}
			if durableActiveDir != "" && !keepDurableTargets {
				_ = os.RemoveAll(durableActiveDir)
			}
		}()
		if restoreArgs.SharedMetadata {
			metadataPath := filepath.Join(r.backend.dataRoot, session.SharedMetadata)
			sharedMetadata, err = os.Open(metadataPath)
			if err != nil {
				reader.Close()
				return fmt.Errorf("opening shared distributed metadata: %w", err)
			}
			if stat, err := sharedMetadata.Stat(); err != nil || uint64(stat.Size()) != session.MetadataBytes {
				sharedMetadata.Close()
				reader.Close()
				if err != nil {
					return fmt.Errorf("stating shared distributed metadata: %w", err)
				}
				return fmt.Errorf("shared distributed metadata size is %d, want %d", stat.Size(), session.MetadataBytes)
			}
			files = append(files, sharedMetadata)
			connectionsStartFD++
		}
		if restoreArgs.SharedSnapshot {
			sharedPath := filepath.Join(r.backend.dataRoot, session.SharedSnapshot)
			sharedSnapshot, err = os.Open(sharedPath)
			if err != nil {
				reader.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				return fmt.Errorf("opening shared distributed memory snapshot: %w", err)
			}
			if stat, err := sharedSnapshot.Stat(); err != nil || uint64(stat.Size()) != session.StreamBytes {
				sharedSnapshot.Close()
				reader.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				if err != nil {
					return fmt.Errorf("stating shared distributed memory snapshot: %w", err)
				}
				return fmt.Errorf("shared distributed memory snapshot size is %d, want %d", stat.Size(), session.StreamBytes)
			}
			files = append(files, sharedSnapshot)
			connectionsStartFD++
		}
		if restoreArgs.SparseManifest != (ttcchunk.ManifestID{}) {
			sparsePath := filepath.Join(r.backend.dataRoot, session.SparseRepository)
			sparseRepository, err = os.Open(sparsePath)
			if err != nil {
				reader.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				return fmt.Errorf("opening shared sparse memory repository: %w", err)
			}
			files = append(files, sparseRepository)
			connectionsStartFD++
		}
		if peer && restoreArgs.DurableDehydration {
			metadataPath := filepath.Join(r.backend.dataRoot, session.SharedMetadata)
			metadataFile, err := os.Open(metadataPath)
			if err != nil {
				return fmt.Errorf("opening durable peer metadata: %w", err)
			}
			durable, readErr := kernel.ReadDurableDehydrationMetadata(metadataFile)
			metadataFile.Close()
			if readErr != nil {
				return fmt.Errorf("reading durable peer metadata: %w", readErr)
			}
			durableActiveDir = memoryBranchDir(r.backend.activeDataRoot(), info.ID, "active", targetBranch)
			if err := os.RemoveAll(durableActiveDir); err != nil {
				return err
			}
			if err := os.MkdirAll(durableActiveDir, 0700); err != nil {
				return err
			}
			for i := 1; i < len(durable.Memories); i++ {
				file, err := os.OpenFile(filepath.Join(durableActiveDir, fmt.Sprintf("memory-%04d.mem", i)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				if err != nil {
					return fmt.Errorf("creating durable distributed peer memory %d: %w", i, err)
				}
				pgalloc.IMAWorkAroundForMemFile(file.Fd())
				durableTargets = append(durableTargets, file)
			}
			files = append(files, durableTargets...)
			connectionsStartFD += len(durableTargets)
		}
		files = append(files, connectionFiles...)
		commandArgs := []string{"timetravel", "--distributed-config-fd=3"}
		if restoreArgs.SharedMetadata {
			commandArgs = append(commandArgs, "--distributed-metadata-fd=4")
		}
		if restoreArgs.SharedSnapshot {
			commandArgs = append(commandArgs, "--distributed-snapshot-fd="+strconv.Itoa(connectionsStartFD-1))
		}
		if restoreArgs.SparseManifest != (ttcchunk.ManifestID{}) {
			commandArgs = append(commandArgs, "--dehydration-memory-repository-fd="+strconv.Itoa(connectionsStartFD-1))
		}
		commandArgs = append(commandArgs,
			"--distributed-connections-start-fd="+strconv.Itoa(connectionsStartFD), "--distributed-connections-count="+strconv.Itoa(len(connectionFiles)),
			info.ID, "distributed-restore", tx.Identity.TransactionID)
		_, err = r.backend.commandWithFiles(files, commandArgs...)
		reader.Close()
		if sharedMetadata != nil {
			sharedMetadata.Close()
		}
		if sharedSnapshot != nil {
			sharedSnapshot.Close()
		}
		if sparseRepository != nil {
			sparseRepository.Close()
		}
		for _, file := range connectionFiles {
			file.Close()
		}
		if err != nil {
			return err
		}
		keepDurableTargets = true
		if runtimeCreated {
			status, err := r.backend.runConfiguredCommandWithArgs(configured, "start", info.ID)
			if err != nil {
				return fmt.Errorf("starting paused distributed target: %w", err)
			}
			if status != 0 {
				return fmt.Errorf("starting paused distributed target exited with status %d", status)
			}
		}
	} else if runtimeState.Status != "running" {
		return fmt.Errorf("distributed target runtime is unexpectedly %q", runtimeState.Status)
	}
	return r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "paused")
}

func (r *daemonTargetRuntime) Publish(_ context.Context, tx *cluster.Transaction, paused bool) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	if !paused && branch.State != "running" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", branch.ID); err != nil {
			return fmt.Errorf("resuming distributed target: %w", err)
		}
		if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "running"); err != nil {
			return err
		}
	}
	if !tx.Identity.Migration {
		info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TransactionID)
		if err != nil {
			return err
		}
	} else {
		// A migration target is deliberately invisible until the source has
		// committed the placement record. Starting any sidecars here would make
		// them consult the still-source-owned placement and route back to the old
		// node. Activate performs all externally visible publication work after
		// that compare-and-swap succeeds.
		return nil
	}
	if _, err := r.backend.ensureConsoleDrain(selector{container: info.ID, branch: branch.ID}, true); err != nil {
		return fmt.Errorf("starting distributed target console drain: %w", err)
	}
	sessions, err := r.backend.execSessions(selector{container: info.ID, branch: branch.ID})
	if err != nil {
		return fmt.Errorf("listing distributed target exec sessions: %w", err)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		if _, err := r.backend.ensureExecConsoleDrain(selector{container: info.ID, branch: branch.ID}, session.ID, session.ConsoleID, true); err != nil {
			return fmt.Errorf("starting distributed target exec console %q: %w", session.ID, err)
		}
	}
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) Activate(_ context.Context, tx *cluster.Transaction) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	state, stateErr := r.backend.runtimeBranchLifecycle(info.ID, branch.ID)
	if stateErr != nil {
		return stateErr
	}
	if tx.DesiredPaused {
		if state != "paused" {
			return fmt.Errorf("distributed migration target is %q, want paused", state)
		}
	} else if state != "running" && state != "paused" {
		return fmt.Errorf("distributed migration target is %q, want paused or running", state)
	}
	info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return fmt.Errorf("publishing distributed migration ownership: %w", err)
	}
	if !tx.DesiredPaused && state == "paused" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", branch.ID); err != nil {
			return fmt.Errorf("resuming distributed migration target: %w", err)
		}
	}
	targetState := "running"
	if tx.DesiredPaused {
		targetState = "paused"
	}
	if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, targetState); err != nil {
		return err
	}
	if err := r.backend.ensurePublishedPortsForBranch(info, branch.ID); err != nil {
		return fmt.Errorf("starting migrated published ports: %w", err)
	}
	if _, err := r.backend.ensureConsoleDrain(selector{container: info.ID, branch: branch.ID}, true); err != nil {
		return fmt.Errorf("starting migrated target console drain: %w", err)
	}
	sessions, err := r.backend.execSessions(selector{container: info.ID, branch: branch.ID})
	if err != nil {
		return fmt.Errorf("listing migrated target exec sessions: %w", err)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		if _, err := r.backend.ensureExecConsoleDrain(selector{container: info.ID, branch: branch.ID}, session.ID, session.ConsoleID, true); err != nil {
			return fmt.Errorf("starting migrated target exec console %q: %w", session.ID, err)
		}
	}
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) Drain(_ context.Context, tx *cluster.Transaction) error {
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	action := "distributed-drain"
	if branch.DependentMemoryBytes == 0 {
		action = "distributed-filesystem-drain"
	}
	_, err = r.backend.command("timetravel", info.ID, action, branch.ID)
	return err
}

func (r *daemonTargetRuntime) Handoff(_ context.Context, tx *cluster.Transaction, sparse cluster.SparseHandoffDescriptor) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	manifest, err := ttcchunk.ParseManifestID(sparse.Manifest)
	if err != nil {
		return fmt.Errorf("parsing distributed sparse manifest: %w", err)
	}
	if err := r.backend.containerStore().UpdateDistributedForkSparseRoot(tx.Identity.TransactionID, sparse.Repository, sparse.Branch); err != nil {
		return fmt.Errorf("recording distributed sparse memory root: %w", err)
	}
	path := filepath.Join(r.backend.dataRoot, sparse.Repository)
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening distributed sparse repository: %w", err)
	}
	defer directory.Close()
	_, err = r.backend.commandWithFiles([]*os.File{directory},
		"timetravel", "--dehydration-memory-repository-fd=3", "--distributed-session="+manifest.String(),
		info.ID, "distributed-handoff", branch.ID)
	if err != nil {
		return fmt.Errorf("switching distributed target to sparse memory: %w", err)
	}
	return nil
}

func (r *daemonTargetRuntime) Cancel(_ context.Context, tx *cluster.Transaction) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err == nil {
		if tx.Identity.Migration && branch.DistributedMigration {
			if _, pending := info.DistributedForkImports[tx.Identity.TransactionID]; !pending {
				return fmt.Errorf("published migration %q cannot be canceled", tx.Identity.TransactionID)
			}
		}
		if peer {
			// Preparation can fail before a target Sentry exists. The
			// runtime-only release helper is idempotent in that case. It also
			// handles a pending import that is the only physical branch in this
			// node's Sentry, where DestroyBranch deliberately rejects removal
			// of the last branch.
			if destroyErr := r.backend.releaseMigratedSourceRuntime(info.ID, branch.ID); destroyErr != nil {
				return fmt.Errorf("destroying canceled distributed target branch %q: %w", branch.ID, destroyErr)
			}
		} else {
			if _, deleteErr := r.backend.command("delete", "--force", info.ID); deleteErr != nil {
				return fmt.Errorf("deleting canceled distributed target family %q: %w", info.ID, deleteErr)
			}
		}
		if releaseErr := r.backend.releaseDistributedSparseRootTransaction(tx.Identity.TransactionID); releaseErr != nil {
			return releaseErr
		}
	}
	return r.backend.containerStore().RemoveDistributedForkTarget(tx.Identity.TransactionID)
}

// releaseDistributedSparseRoot drops the durable root only after the target
// loader has installed every chunk, or after the target runtime is gone. It is
// idempotent so recovery may repeat it after any crash boundary.
func (b *runtimeBackend) releaseDistributedSparseRoot(info ttcbundle.ContainerInfo) error {
	var result error
	if info.DistributedSparseRepository != "" || info.DistributedSparseBranch != "" {
		result = errors.Join(result, b.releaseSparseRoot(info.DistributedSparseRepository, info.DistributedSparseBranch))
		if result == nil && info.DistributedForkTransactionID != "" {
			result = errors.Join(result, b.containerStore().UpdateDistributedForkSparseRoot(info.DistributedForkTransactionID, "", ""))
		}
	}
	for _, branch := range info.Branches {
		if branch.DistributedSparseRepository == "" && branch.DistributedSparseBranch == "" {
			continue
		}
		if err := b.releaseSparseRoot(branch.DistributedSparseRepository, branch.DistributedSparseBranch); err != nil {
			result = errors.Join(result, err)
			continue
		}
		if branch.DistributedForkTransactionID != "" {
			result = errors.Join(result, b.containerStore().UpdateDistributedForkSparseRoot(branch.DistributedForkTransactionID, "", ""))
		}
	}
	return result
}

func (b *runtimeBackend) releaseDistributedSparseRootTransaction(transactionID string) error {
	_, branch, _, err := b.containerStore().DistributedForkTargetBranch(transactionID)
	if err != nil {
		return err
	}
	if branch.DistributedSparseRepository == "" && branch.DistributedSparseBranch == "" {
		return nil
	}
	if err := b.releaseSparseRoot(branch.DistributedSparseRepository, branch.DistributedSparseBranch); err != nil {
		return err
	}
	return b.containerStore().UpdateDistributedForkSparseRoot(transactionID, "", "")
}

func (b *runtimeBackend) releaseSparseRoot(repositoryPath, branch string) error {
	if repositoryPath == "" || branch == "" {
		return errors.New("distributed sparse memory root is incomplete")
	}
	clean := filepath.Clean(repositoryPath)
	if filepath.IsAbs(clean) || clean == "." || clean != repositoryPath || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid distributed sparse repository %q", repositoryPath)
	}
	repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.dataRoot, clean))
	if err != nil {
		return fmt.Errorf("opening distributed sparse repository: %w", err)
	}
	deletionErr := repository.DeleteBranch(branch)
	if errors.Is(deletionErr, os.ErrNotExist) {
		deletionErr = nil
	}
	if deletionErr == nil {
		_, deletionErr = repository.CollectGarbage(ttcchunk.GCOptions{})
	}
	closeErr := repository.Close()
	if deletionErr != nil || closeErr != nil {
		return errors.Join(deletionErr, closeErr)
	}
	return nil
}

func (r *daemonTargetRuntime) MarkIndependent(_ context.Context, tx *cluster.Transaction) error {
	if r == nil || r.backend == nil || tx == nil {
		return nil
	}
	return r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TransactionID, 0, 0)
}

type adaptivePrefetchController struct {
	policy               ttcpolicy.Policy
	ahead                uint64
	behind               uint64
	maxAhead             uint64
	lastLoaded           uint64
	lastWaited           uint64
	lastWaiters          int
	lastFaults           uint64
	lastSequentialFaults uint64
	localityFaults       uint64
	localitySequential   uint64
	lastChange           time.Time
	quietSamples         int
	initialized          bool
}

func newAdaptivePrefetchController(policy ttcpolicy.Policy) *adaptivePrefetchController {
	perWorkerBudget := policy.MaxOutstandingBytes / uint64(policy.MaxConcurrency)
	maxRead := min(perWorkerBudget, uint64(2<<20))
	maxRead -= maxRead % policy.PageSize
	if maxRead < policy.TransferBytes {
		maxRead = policy.TransferBytes
	}
	return &adaptivePrefetchController{
		policy:   policy,
		ahead:    policy.TransferBytes - policy.PageSize,
		maxAhead: maxRead - policy.PageSize,
	}
}

// observe applies deliberately slow hysteresis: blocking faults grow the
// window quickly, while sustained non-blocking throughput probes for a smaller
// window one step at a time. It returns a change only after the prior setting
// has had time to affect multiple fault/read cycles.
func (c *adaptivePrefetchController) observe(status pgalloc.AsyncPagesFileLoadStats, now time.Time) (uint64, uint64, bool) {
	if !c.initialized {
		c.initialized = true
		c.lastChange = now.Add(-50 * time.Millisecond)
		if status.PrefetchBytes != 0 || status.PrefetchBehindBytes != 0 {
			c.ahead, c.behind = status.PrefetchBytes, status.PrefetchBehindBytes
		}
		if status.Faults == 0 {
			c.lastLoaded = status.BytesLoaded
			c.lastWaited = status.BytesWaited
			c.lastWaiters = status.TotalWaiters
			c.lastFaults = status.Faults
			c.lastSequentialFaults = status.SequentialFaults
			return 0, 0, false
		}
	}
	loaded := status.BytesLoaded - min(status.BytesLoaded, c.lastLoaded)
	waited := status.BytesWaited - min(status.BytesWaited, c.lastWaited)
	waiters := status.TotalWaiters - min(status.TotalWaiters, c.lastWaiters)
	faults := status.Faults - min(status.Faults, c.lastFaults)
	sequentialFaults := status.SequentialFaults - min(status.SequentialFaults, c.lastSequentialFaults)
	c.lastLoaded, c.lastWaited, c.lastWaiters = status.BytesLoaded, status.BytesWaited, status.TotalWaiters
	c.lastFaults, c.lastSequentialFaults = status.Faults, status.SequentialFaults
	c.localityFaults += faults
	c.localitySequential += sequentialFaults

	// Random faults should reduce speculative amplification even when every
	// fault blocks. Exact adjacency is a deliberately conservative signal:
	// ambiguous or concurrent samples retain the current setting.
	if c.localityFaults >= 4 && c.localitySequential*4 <= c.localityFaults {
		c.quietSamples = 0
		if c.ahead == 0 {
			c.localityFaults, c.localitySequential = 0, 0
			return 0, 0, false
		}
		if now.Sub(c.lastChange) < 50*time.Millisecond {
			return 0, 0, false
		}
		total := (c.ahead + c.policy.PageSize) / 2
		if total < c.policy.PageSize {
			total = c.policy.PageSize
		}
		total -= total % c.policy.PageSize
		c.ahead = total - c.policy.PageSize
		c.behind = 0
		c.localityFaults, c.localitySequential = 0, 0
		c.lastChange = now
		return c.behind, c.ahead, true
	}

	if waited != 0 || waiters != 0 || status.Waiters != 0 {
		c.quietSamples = 0
		if c.localityFaults < 4 || c.localitySequential*4 < c.localityFaults*3 || now.Sub(c.lastChange) < 50*time.Millisecond {
			return 0, 0, false
		}
		if c.ahead >= c.maxAhead {
			c.localityFaults, c.localitySequential = 0, 0
			return 0, 0, false
		}
		// Once an adjacent run is established, request the full negotiated
		// transport window immediately. RTT-bound sequential workloads are too
		// short for a slow doubling ladder; later random or quiet samples still
		// shrink with hysteresis.
		c.ahead = c.maxAhead
		c.behind = 0
		c.localityFaults, c.localitySequential = 0, 0
		c.lastChange = now
		return c.behind, c.ahead, true
	}
	if loaded >= 4*(c.ahead+c.policy.PageSize) {
		c.quietSamples++
	} else {
		c.quietSamples = 0
	}
	if c.quietSamples < 8 || now.Sub(c.lastChange) < 2*time.Second || c.ahead <= c.policy.TransferBytes-c.policy.PageSize {
		return 0, 0, false
	}
	c.quietSamples = 0
	total := max((c.ahead+c.policy.PageSize)/2, c.policy.TransferBytes)
	total -= total % c.policy.PageSize
	c.ahead = total - c.policy.PageSize
	c.behind = 0
	c.lastChange = now
	return c.behind, c.ahead, true
}

func (r *daemonTargetRuntime) monitorDependency(transactionID, containerID string) {
	if r == nil || r.store == nil || transactionID == "" || containerID == "" {
		return
	}
	if _, loaded := r.watch.LoadOrStore(transactionID, struct{}{}); loaded {
		return
	}
	go func() {
		defer r.watch.Delete(transactionID)
		interval := 250 * time.Millisecond
		if tx, err := r.store.Load(transactionID); err == nil && tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
			interval = 50 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failedPolls := 0
		sourceFailedPolls := 0
		var adaptive *adaptivePrefetchController
		for {
			select {
			case <-r.done:
				return
			case <-ticker.C:
			}
			tx, err := r.store.Load(transactionID)
			if err != nil || tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
				return
			}
			if tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining && tx.State != cluster.TransactionIndependent {
				continue
			}
			if tx.SourceNeeded && r.directory != nil {
				if source, sourceErr := r.directory.Lookup(tx.Identity.SourceNodeID); sourceErr != nil || !source.LeaseActive(time.Now()) || source.State == cluster.NodeOffline {
					sourceFailedPolls++
					if sourceFailedPolls < 3 {
						continue
					}
					if info, loadErr := r.backend.containerStore().LoadDistributedForkTarget(transactionID); loadErr == nil {
						if branch, branchErr := persistentBranchInfo(info, tx.Identity.VisibleTargetBranch()); branchErr == nil &&
							(branch.DependentMemoryBytes != 0 || branch.DependentFilesystemBytes != 0) {
							_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
								"unrecoverable volatile-state loss: source node disappeared before memory and filesystem dependencies became durable", time.Now())
							return
						}
					}
				} else {
					sourceFailedPolls = 0
				}
			}
			// Migration publication precedes the placement compare-and-swap. Do
			// not let dependency completion race that cutover and advance the
			// transaction before the target has become the authoritative owner.
			if tx.Identity.Migration && !tx.Activated {
				continue
			}
			targetBranch := tx.Identity.VisibleTargetBranch()
			if adaptive == nil && tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
				adaptive = newAdaptivePrefetchController(tx.Negotiated.PagingPolicy)
			}
			output, statusErr := r.backend.command("timetravel", containerID, "distributed-dependency", targetBranch)
			if statusErr != nil {
				// Process-free dehydration intentionally removes the target Kernel
				// while exporting all of its pages. Do not confuse that durable
				// lifecycle transition with an unexpected target exit.
				if info, loadErr := r.backend.containerStore().LoadContainer(containerID); loadErr == nil {
					if branch, branchErr := persistentBranchInfo(info, targetBranch); branchErr == nil {
						switch normalizeLifecycleState(branch.State) {
						case "dehydrating":
							failedPolls = 0
							continue
						case "dehydrated":
							if tx.State == cluster.TransactionIndependent {
								return
							}
							failedPolls = 0
							continue
						}
					}
				}
				failedPolls++
				if failedPolls < 3 {
					continue
				}
				if _, stateErr := r.backend.command("state", containerID); stateErr == nil {
					continue
				}
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"dependent target exited before remote memory drain completed", time.Now())
				return
			}
			failedPolls = 0
			var status pgalloc.AsyncPagesFileLoadStats
			if err := json.Unmarshal(output, &status); err != nil {
				continue
			}
			if adaptive != nil && !status.Done {
				behind, ahead, changed := adaptive.observe(status, time.Now())
				if changed {
					_, _ = r.backend.command(
						"timetravel",
						"--distributed-prefetch-bytes", strconv.FormatUint(ahead, 10),
						"--distributed-prefetch-behind-bytes", strconv.FormatUint(behind, 10),
						containerID, "distributed-prefetch", targetBranch,
					)
				}
			}
			info, infoErr := r.backend.containerStore().LoadDistributedForkTarget(transactionID)
			var filesystemBytes uint64
			if infoErr == nil {
				if branch, branchErr := persistentBranchInfo(info, targetBranch); branchErr == nil {
					filesystemBytes = branch.DependentFilesystemBytes
				}
			}
			if !status.Done {
				_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, status.BytesOutstanding, filesystemBytes)
				continue
			}
			if status.Error != "" {
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"remote memory loader failed: "+status.Error, time.Now())
				return
			}
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, 0, filesystemBytes)
			if filesystemBytes != 0 && tx.State != cluster.TransactionIndependent {
				continue
			}
			if tx.State != cluster.TransactionIndependent {
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now())
			}
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, 0, 0)
			// The target MemoryFile now contains every page, so the immutable
			// shared snapshot no longer owns storage for this branch.
			_ = os.Remove(filepath.Join(r.backend.dataRoot, distributedSnapshotRelativePath(transactionID)))
			if _, err := r.backend.containerStore().LoadDistributedForkTarget(transactionID); err == nil {
				// A cleanup failure is deliberately non-fatal: retaining a root is
				// safe and startup recovery will retry it. Invalidating an already
				// independent, running target would be incorrect.
				_ = r.backend.releaseDistributedSparseRootTransaction(transactionID)
			}
			return
		}
	}()
}

type daemonFrameWriter struct {
	conn net.Conn
	mu   *sync.Mutex
	typ  byte
}

type daemonJSONFrameWriter struct {
	conn   net.Conn
	mu     *sync.Mutex
	typ    byte
	buffer []byte
}

func (w *daemonJSONFrameWriter) Write(data []byte) (int, error) {
	w.buffer = append(w.buffer, data...)
	for {
		newline := bytes.IndexByte(w.buffer, '\n')
		if newline < 0 {
			return len(data), nil
		}
		payload := append([]byte(nil), w.buffer[:newline]...)
		w.buffer = w.buffer[newline+1:]
		if len(payload) != 0 {
			if err := writeDaemonFrame(w.conn, w.mu, w.typ, payload); err != nil {
				return 0, err
			}
		}
	}
}

// OwnConsoleSidecar tracks and reaps one ready console follower. Followers are
// intentionally not killed when the daemon shuts down: they retain the advisory
// lock and continue draining until a restarted daemon reconciles the family.
func (s *daemonServer) OwnConsoleSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("console", key, command)
}

// StopConsoleSidecar terminates a console follower owned by this daemon.
func (s *daemonServer) StopConsoleSidecar(key string) (bool, error) {
	return s.stopOwnedSidecar(key)
}

// OwnPortSidecar tracks and reaps a published-port listener.
func (s *daemonServer) OwnPortSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("port", key, command)
}

// StopPortSidecar terminates a listener owned by this daemon. The boolean is
// false when the listener survived an older daemon and must be found through
// its advisory lock instead.
func (s *daemonServer) StopPortSidecar(key string) (bool, error) {
	return s.stopOwnedSidecar(key)
}

func consoleClientKey(target selector) string {
	return target.container + "@" + target.branch
}

func consoleClientFamilyKey(container string) string {
	return container + "@*"
}

// RegisterConsoleClient tracks a daemon request that has persistent console
// artifacts open. Registration precedes opening those files, so a concurrent
// branch/family removal either waits for this client or rejects the new reader
// after quiescing has begun.
func (s *daemonServer) RegisterConsoleClient(target selector, cancel func()) (func(), error) {
	if target.container == "" || target.branch == "" || cancel == nil {
		return nil, errors.New("console client requires a branch and cancellation function")
	}
	key := consoleClientKey(target)
	familyKey := consoleClientFamilyKey(target.container)
	client := &daemonConsoleClient{cancel: cancel, done: make(chan struct{})}
	s.consoleClientMu.Lock()
	if s.closingConsoleClient[key] != 0 || s.closingConsoleClient[familyKey] != 0 {
		s.consoleClientMu.Unlock()
		return nil, fmt.Errorf("console clients for %s are being detached", target)
	}
	if s.consoleClients == nil {
		s.consoleClients = make(map[string]map[*daemonConsoleClient]struct{})
	}
	clients := s.consoleClients[key]
	if clients == nil {
		clients = make(map[*daemonConsoleClient]struct{})
		s.consoleClients[key] = clients
	}
	clients[client] = struct{}{}
	s.consoleClientMu.Unlock()
	release := func() {
		client.releaseOnce.Do(func() {
			s.consoleClientMu.Lock()
			if clients := s.consoleClients[key]; clients != nil {
				delete(clients, client)
				if len(clients) == 0 {
					delete(s.consoleClients, key)
				}
			}
			s.consoleClientMu.Unlock()
			close(client.done)
		})
	}
	return release, nil
}

// QuiesceConsoleClients disconnects and waits for console artifact readers.
// An empty branch means the whole family. The returned release function lifts
// the registration fence after the caller has finished deleting artifacts.
func (s *daemonServer) QuiesceConsoleClients(target selector) (func(), error) {
	if target.container == "" {
		return nil, errors.New("console-client quiescing requires a container")
	}
	closingKey := consoleClientKey(target)
	if target.branch == "" {
		closingKey = consoleClientFamilyKey(target.container)
	}
	s.consoleClientMu.Lock()
	if s.closingConsoleClient == nil {
		s.closingConsoleClient = make(map[string]int)
	}
	s.closingConsoleClient[closingKey]++
	var clients []*daemonConsoleClient
	if target.branch == "" {
		prefix := target.container + "@"
		for key, registered := range s.consoleClients {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			for client := range registered {
				clients = append(clients, client)
			}
		}
	} else {
		for client := range s.consoleClients[consoleClientKey(target)] {
			clients = append(clients, client)
		}
	}
	s.consoleClientMu.Unlock()
	release := func() {
		s.consoleClientMu.Lock()
		if count := s.closingConsoleClient[closingKey]; count <= 1 {
			delete(s.closingConsoleClient, closingKey)
		} else {
			s.closingConsoleClient[closingKey] = count - 1
		}
		s.consoleClientMu.Unlock()
	}
	for _, client := range clients {
		client.cancelOnce.Do(client.cancel)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, client := range clients {
		select {
		case <-client.done:
		case <-deadline.C:
			release()
			return nil, fmt.Errorf("timed out waiting for %d console client(s) of %s to detach", len(clients), target)
		}
	}
	return release, nil
}

func (s *daemonServer) stopOwnedSidecar(key string) (bool, error) {
	s.sidecarMu.Lock()
	command := s.sidecars[key]
	s.sidecarMu.Unlock()
	if command == nil || command.Process == nil {
		return false, nil
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true, fmt.Errorf("sidecar %q did not exit", key)
}

func (s *daemonServer) ownSidecar(kind, key string, command *exec.Cmd) error {
	s.sidecarMu.Lock()
	if _, ok := s.sidecars[key]; ok {
		s.sidecarMu.Unlock()
		return fmt.Errorf("%s sidecar %q is already daemon-owned", kind, key)
	}
	if s.sidecars == nil {
		s.sidecars = make(map[string]*exec.Cmd)
	}
	s.sidecars[key] = command
	s.sidecarMu.Unlock()
	go func() {
		_ = command.Wait()
		s.sidecarMu.Lock()
		if s.sidecars[key] == command {
			delete(s.sidecars, key)
		}
		s.sidecarMu.Unlock()
	}()
	return nil
}

func (s *daemonServer) ownedConsoleSidecars() int {
	s.sidecarMu.Lock()
	defer s.sidecarMu.Unlock()
	return len(s.sidecars)
}

func (w daemonFrameWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, writeDaemonFrame(w.conn, w.mu, w.typ, nil)
	}
	for written := 0; written < len(data); {
		end := written + daemonMaxFrame
		if end > len(data) {
			end = len(data)
		}
		if err := writeDaemonFrame(w.conn, w.mu, w.typ, data[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return len(data), nil
}

func defaultDaemonSocket() string {
	if socket := os.Getenv("TTC_SOCKET"); socket != "" {
		return socket
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ttc", "ttcd.sock")
	}
	return filepath.Join(os.TempDir(), "ttc-"+strconv.Itoa(os.Geteuid()), "ttcd.sock")
}

func daemonIsolationMode(unsafeNoHostIsolation bool) string {
	if unsafeNoHostIsolation {
		return "unsafe-no-host-isolation"
	}
	return "rootless"
}

func directBackend(opts options, stdin io.Reader, stdout, stderr io.Writer) *runtimeBackend {
	progressTerminal, progressWidth := terminalWriterInfo(stderr)
	return &runtimeBackend{
		root:                  opts.root,
		runsc:                 opts.runsc,
		dataRoot:              opts.dataRoot,
		localRoot:             opts.localRoot,
		daemonSocket:          opts.daemonSocket,
		runtimePlatform:       opts.runtimePlatform,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		disableHostGSO:        opts.disableHostGSO,
		allowSlowVolumeCopy:   opts.allowSlowVolumeCopy,
		clientExecutable:      opts.clientExecutable,
		clientTERM:            os.Getenv("TERM"),
		progressTerminal:      progressTerminal,
		progressWidth:         progressWidth,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
	}
}

func (s *daemonServer) configureVolumeBackend(backend *runtimeBackend) {
	backend.volumeCopyMode = s.volumeCapability.Mode
	backend.allowSlowVolumeCopy = s.volumeCapability.Mode == ttcvolume.CopyModeFull
	backend.publicationQueue = s.publicationQueue
}

type daemonStopTarget struct {
	selector selector
}

func branchOwnedByBackend(backend backend, branch Branch) bool {
	runtime, ok := backend.(*runtimeBackend)
	return !ok || runtime.nodeID == "" || branch.NodeID == "" || branch.NodeID == runtime.nodeID
}

func stopDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var targets []daemonStopTarget
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused", "dehydrated", "restarting", "exited":
			default:
				continue
			}
			targets = append(targets, daemonStopTarget{
				selector: selector{container: family.ID, branch: branch.ID},
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].selector.container != targets[j].selector.container {
			return targets[i].selector.container < targets[j].selector.container
		}
		return targets[i].selector.branch < targets[j].selector.branch
	})
	var stopErrors []error
	for _, target := range targets {
		if err := runStop([]string{target.selector.String()}, io.Discard, io.Discard, backend); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stopping %s: %w", target.selector, err))
		}
	}
	return errors.Join(stopErrors...)
}

func dehydrateDaemonContainers(backend backend) error {
	db, ok := backend.(dehydrationBackend)
	if !ok {
		return errors.New("backend does not support dehydration")
	}
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused":
				if err := db.dehydrate(selector{container: family.ID, branch: branch.ID}); err != nil {
					return fmt.Errorf("dehydrating %s@%s: %w", family.ID, branch.ID, err)
				}
			case "dehydrated", "exited", "stopped", "created":
			default:
				return fmt.Errorf("branch %s@%s is %s", family.ID, branch.ID, branch.State)
			}
		}
	}
	return nil
}

func rejectActiveDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var active []string
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused", "restarting", "dehydrating", "rehydrating":
				active = append(active, selector{container: family.ID, branch: branch.ID}.String()+" ("+branch.State+")")
			}
		}
	}
	if len(active) != 0 {
		sort.Strings(active)
		return fmt.Errorf("managed branches are still active: %s; use --dehydrate-all or --stop-all", strings.Join(active, ", "))
	}
	return nil
}

func drainSourceDependencies(backend *runtimeBackend, source *distributedSourceRuntime, store *cluster.Store, nodeID string) error {
	return drainSourceDependenciesMatching(backend, source, store, nodeID, "", "")
}

func drainSourceDependenciesMatching(backend *runtimeBackend, source *distributedSourceRuntime, store *cluster.Store, nodeID, containerID, branchID string) error {
	if backend == nil || source == nil || store == nil || nodeID == "" {
		return nil
	}
	transactions, err := store.List()
	if err != nil {
		return fmt.Errorf("listing distributed dependencies: %w", err)
	}
	for i := range transactions {
		tx := transactions[i]
		if tx.Identity.SourceNodeID != nodeID || !tx.SourceNeeded ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining) {
			continue
		}
		if containerID != "" && tx.Identity.SourceContainer != containerID {
			continue
		}
		if branchID != "" && tx.Identity.SourceBranch != branchID {
			continue
		}
		peer, err := source.peer(tx.Identity.TargetNodeID, false)
		if err != nil {
			return fmt.Errorf("finding distributed target node %q for transaction %q: %w", tx.Identity.TargetNodeID, tx.Identity.TransactionID, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if tx.State == cluster.TransactionPublished {
			draining, err := peer.Drain(ctx, tx.Identity.TransactionID, tx.Revision)
			if err != nil {
				if current, statusErr := peer.Status(ctx, tx.Identity.TransactionID); statusErr != nil ||
					(current.State != cluster.TransactionDraining && current.State != cluster.TransactionIndependent) {
					cancel()
					return fmt.Errorf("draining distributed target %q: %w", tx.Identity.TargetContainer, err)
				} else {
					tx = *current
				}
			} else {
				tx = *draining
			}
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		for tx.State != cluster.TransactionIndependent {
			select {
			case <-ctx.Done():
				ticker.Stop()
				cancel()
				return fmt.Errorf("draining distributed target %q: %w", tx.Identity.TargetContainer, ctx.Err())
			case <-ticker.C:
			}
			current, err := peer.Status(ctx, tx.Identity.TransactionID)
			if err != nil {
				continue
			}
			tx = *current
			if tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
				ticker.Stop()
				cancel()
				return fmt.Errorf("distributed target %q became %s while draining: %s", tx.Identity.TargetContainer, tx.State, tx.Failure)
			}
		}
		ticker.Stop()
		cancel()
		_, _ = backend.command("timetravel", tx.Identity.SourceContainer, "distributed-complete", tx.Identity.TransactionID)
	}
	return nil
}

// ensureDistributedSourceIndependent prevents branch-destructive operations
// from invalidating an already-published remote child. In the normal path the
// asynchronous sparse handoff has already completed, so this is a no-op.
func (b *runtimeBackend) ensureDistributedSourceIndependent(containerID, branchID string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || b.dataRoot == "" {
		return nil
	}
	store, err := cluster.OpenStore(filepath.Join(b.dataRoot, "cluster", "transactions"))
	if err != nil {
		return err
	}
	return drainSourceDependenciesMatching(b, b.distributedSource, store, b.nodeID, containerID, branchID)
}

func wireOptions(opts options) daemonWireOptions {
	return daemonWireOptions{}
}

func optionsFromWire(w daemonWireOptions) options {
	return options{}
}

// optionsFromRequest applies daemon-owned configuration. Runtime state, the
// runtime executable, platform, isolation, and persistent data all belong to
// the daemon selected by the socket; accepting client overrides would let two
// independently configured daemons accidentally operate on the same runtime.
// The legacy wire fields remain decodable for API compatibility but are
// intentionally ignored.
func (s *daemonServer) optionsFromRequest(w daemonWireOptions) options {
	opts := optionsFromWire(w)
	opts.root = s.root
	opts.runsc = s.runsc
	opts.clientExecutable = s.clientExecutable
	opts.unsafeNoHostIsolation = s.unsafeNoHostIsolation
	opts.disableHostGSO = s.disableHostGSO
	opts.runtimePlatform = s.runtimePlatform
	opts.dataRoot = s.dataRoot
	opts.localRoot = s.localRoot
	opts.daemonSocket = s.socket
	return opts
}

type hostKernelVersion struct {
	major int
	minor int
	patch int
}

func (v hostKernelVersion) lessThan(other hostKernelVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

type hostKernelCompatibility struct {
	release        string
	version        hostKernelVersion
	disableHostGSO bool
}

func parseHostKernelVersion(release string) (hostKernelVersion, error) {
	if release == "" || release[0] < '0' || release[0] > '9' {
		return hostKernelVersion{}, fmt.Errorf("invalid host kernel release %q", release)
	}
	parseComponent := func(value string) (int, string, error) {
		end := 0
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, value, fmt.Errorf("invalid host kernel release %q", release)
		}
		component, err := strconv.Atoi(value[:end])
		if err != nil {
			return 0, value, fmt.Errorf("invalid host kernel release %q: %w", release, err)
		}
		return component, value[end:], nil
	}
	major, remaining, err := parseComponent(release)
	if err != nil || len(remaining) == 0 || remaining[0] != '.' {
		return hostKernelVersion{}, fmt.Errorf("invalid host kernel release %q", release)
	}
	minor, remaining, err := parseComponent(remaining[1:])
	if err != nil {
		return hostKernelVersion{}, err
	}
	patch := 0
	if len(remaining) != 0 && remaining[0] == '.' {
		patch, _, err = parseComponent(remaining[1:])
		if err != nil {
			return hostKernelVersion{}, err
		}
	}
	return hostKernelVersion{major: major, minor: minor, patch: patch}, nil
}

func evaluateHostKernelCompatibility(release string) (hostKernelCompatibility, error) {
	version, err := parseHostKernelVersion(release)
	if err != nil {
		return hostKernelCompatibility{}, err
	}
	minimum := hostKernelVersion{major: 3, minor: 17}
	if version.lessThan(minimum) {
		return hostKernelCompatibility{}, fmt.Errorf("host Linux kernel %s is unsupported; TTC requires Linux 3.17 or newer", release)
	}
	hostGSOMinimum := hostKernelVersion{major: 4, minor: 14, patch: 77}
	return hostKernelCompatibility{
		release:        release,
		version:        version,
		disableHostGSO: version.lessThan(hostGSOMinimum),
	}, nil
}

func currentHostKernelRelease() (string, error) {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return "", err
	}
	var release strings.Builder
	for _, value := range name.Release {
		if value == 0 {
			break
		}
		release.WriteByte(byte(value))
	}
	if release.Len() == 0 {
		return "", errors.New("uname returned an empty host kernel release")
	}
	return release.String(), nil
}

func siblingTTCExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	if filepath.Base(executable) == "ttc" {
		return executable
	}
	sibling := filepath.Join(filepath.Dir(executable), "ttc")
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
		return sibling
	}
	return ""
}

func normalizeRuntimePlatform(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "systrap", "kvm":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported runtime platform %q; supported platforms are systrap and kvm", value)
	}
}

func runtimePlatformFromEnvironment() string {
	value := os.Getenv("TTC_RUNTIME_PLATFORM")
	if normalized, err := normalizeRuntimePlatform(value); err == nil {
		return normalized
	}
	return defaultRuntimePlatform
}

func preflightDaemonPlatform(runtimePlatform string) error {
	if runtimePlatform != "kvm" {
		return nil
	}
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("runtime platform kvm requires read/write access to /dev/kvm: %w", err)
	}
	defer device.Close()
	version, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("querying /dev/kvm API version: %w", errno)
	}
	if version != kvmAPIVersion {
		return fmt.Errorf("unsupported /dev/kvm API version %d; want %d", version, kvmAPIVersion)
	}
	vmFD, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return fmt.Errorf("creating a KVM VM: %w", errno)
	}
	if err := unix.Close(int(vmFD)); err != nil {
		return fmt.Errorf("closing KVM preflight VM: %w", err)
	}
	return nil
}

// RunDaemon serves the versioned TTC local API from the ttcd binary.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == daemonRootlessProbeArg {
		return 0
	}
	socket := defaultDaemonSocket()
	runtimePlatform := defaultRuntimePlatform
	unsafeNoHostIsolation := false
	allowSlowVolumeCopy := false
	clusterOpts := defaultDaemonClusterOptions()
	for len(args) != 0 {
		switch {
		case args[0] == "--socket" && len(args) >= 2:
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		case args[0] == "--unsafe-no-host-isolation":
			unsafeNoHostIsolation = true
			args = args[1:]
		case args[0] == "--allow-slow-volume-copy":
			allowSlowVolumeCopy = true
			args = args[1:]
		case args[0] == "--platform":
			if len(args) < 2 || args[1] == "" {
				fmt.Fprintln(stderr, "ttcd: --platform requires systrap or kvm")
				return 2
			}
			runtimePlatform = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--platform="):
			runtimePlatform = strings.TrimPrefix(args[0], "--platform=")
			args = args[1:]
		default:
			consumed, err := consumeDaemonClusterOption(args, &clusterOpts)
			if err != nil {
				fmt.Fprintf(stderr, "ttcd: %v\n", err)
				return 2
			}
			if consumed == 0 {
				fmt.Fprintf(stderr, "ttcd: unknown option %q\n", args[0])
				return 2
			}
			args = args[consumed:]
		}
	}
	if socket == "" {
		fmt.Fprintln(stderr, "ttcd: socket path must not be empty")
		return 2
	}
	roots, err := daemonRootsFromEnvironment(clusterOpts.Configured)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	daemonDataRoot := roots.shared
	daemonRoot := roots.runtime
	daemonRunsc, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: finding embedded runtime executable: %v\n", err)
		return 1
	}
	runtimePlatform, err = normalizeRuntimePlatform(runtimePlatform)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	if err := daemonPlatformPreflight(runtimePlatform); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	kernelRelease, err := daemonKernelRelease()
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: detecting host Linux kernel release: %v\n", err)
		return 1
	}
	kernelCompatibility, err := evaluateHostKernelCompatibility(kernelRelease)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if kernelCompatibility.disableHostGSO {
		fmt.Fprintf(stderr, "ttcd: warning: host Linux kernel %s predates 4.14.77; disabling host GSO for compatibility; network performance, especially for large payloads, will be reduced\n", kernelRelease)
	}
	if unsafeNoHostIsolation {
		fmt.Fprintln(stderr, "ttcd: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
	} else if err := preflightDaemonIsolation(false); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if err := prepareDaemonStorageRoot(roots.shared, "shared", clusterOpts.RepositoryID, ""); err != nil {
		fmt.Fprintf(stderr, "ttcd: preparing TTC_ROOT: %v\n", err)
		return 1
	}
	if clusterOpts.Configured {
		if err := prepareDaemonStorageRoot(roots.local, "local", clusterOpts.RepositoryID, clusterOpts.NodeID); err != nil {
			fmt.Fprintf(stderr, "ttcd: preparing TTC_LOCAL_ROOT: %v\n", err)
			return 1
		}
	}
	localLock, err := lockDaemonLocalRoot(roots.local)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: locking TTC local root: %v\n", err)
		return 1
	}
	defer localLock.Close()
	if err := prepareDaemonRuntimeStorage(socket, daemonRoot); err != nil {
		fmt.Fprintf(stderr, "ttcd: preparing node-local runtime storage: %v\n", err)
		return 1
	}
	manager, err := ttcvolume.OpenManager(roots.local, allowSlowVolumeCopy)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: initializing disk-backed volumes: %v\n", err)
		return 1
	}
	store := ttcbundle.Store{Root: daemonDataRoot, ActiveRoot: roots.local}
	// Namespace recovery must precede every startup inventory. In particular,
	// image-root pruning uses a conservative container scan and should never be
	// reached with a half-deleted legacy directory still in containers/.
	if _, err := store.RecoverContainerRemovals(); err != nil {
		fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers before startup inventory: %v\n", err)
	}
	if _, err := store.RemoveObsoleteApplicationMemoryFiles(); err != nil {
		fmt.Fprintf(stderr, "ttcd: removing obsolete application memory files: %v\n", err)
		return 1
	}
	if _, err := store.PruneImageRoots(context.Background()); err != nil {
		fmt.Fprintf(stderr, "ttcd: pruning stale image roots: %v\n", err)
		return 1
	}
	clusterRuntime, err := prepareDaemonCluster(clusterOpts, daemonDataRoot)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: configuring cluster peer service: %v\n", err)
		return 1
	}
	if err := serveDaemonWithCluster(socket, runtimePlatform, unsafeNoHostIsolation, kernelCompatibility, daemonDataRoot, roots.local, daemonRoot, daemonRunsc, manager.Capability(), clusterRuntime, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	return 0
}

type daemonRoots struct {
	shared  string
	local   string
	runtime string
}

// daemonRootsFromEnvironment is the only source of daemon storage paths.
// Keeping this out of the client prevents client/daemon disagreement and makes
// it possible for a client to operate without access to either root.
func daemonRootsFromEnvironment(clustered bool) (daemonRoots, error) {
	if legacy := strings.TrimSpace(os.Getenv("TTC_DATA_ROOT")); legacy != "" {
		return daemonRoots{}, errors.New("TTC_DATA_ROOT is no longer supported; configure a fresh TTC_ROOT (existing data roots are not migrated)")
	}
	shared := strings.TrimSpace(os.Getenv("TTC_ROOT"))
	if shared == "" {
		return daemonRoots{}, errors.New("TTC_ROOT must be explicitly set to a fresh absolute path")
	}
	if !filepath.IsAbs(shared) {
		return daemonRoots{}, fmt.Errorf("TTC_ROOT %q is not an absolute path", shared)
	}
	shared = filepath.Clean(shared)
	local := shared
	if clustered {
		local = strings.TrimSpace(os.Getenv("TTC_LOCAL_ROOT"))
		if local == "" {
			return daemonRoots{}, errors.New("TTC_LOCAL_ROOT must be explicitly set for cluster mode")
		}
		if !filepath.IsAbs(local) {
			return daemonRoots{}, fmt.Errorf("TTC_LOCAL_ROOT %q is not an absolute path", local)
		}
		local = filepath.Clean(local)
		same, err := pathsReferToSameLocation(shared, local)
		if err != nil {
			return daemonRoots{}, err
		}
		if same {
			return daemonRoots{}, errors.New("TTC_LOCAL_ROOT must be distinct from shared TTC_ROOT in cluster mode")
		}
	}
	return daemonRoots{shared: shared, local: local, runtime: filepath.Join(local, "runtime", "runsc")}, nil
}

func pathsReferToSameLocation(first, second string) (bool, error) {
	resolve := func(path string) (string, error) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolving storage root %q: %w", path, err)
		}
		parent, base := filepath.Dir(path), filepath.Base(path)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, base), nil
		}
		return filepath.Clean(path), nil
	}
	a, err := resolve(first)
	if err != nil {
		return false, err
	}
	b, err := resolve(second)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

type daemonRootMarker struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	RepositoryID string `json:"repository_id,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
}

func prepareDaemonStorageRoot(root, kind, repositoryID, nodeID string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	markerPath := filepath.Join(root, ".ttc-root-format")
	want := daemonRootMarker{Version: daemonRootFormatVersion, Kind: kind, RepositoryID: repositoryID, NodeID: nodeID}
	data, err := os.ReadFile(markerPath)
	if err == nil {
		var got daemonRootMarker
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("invalid root marker: %w", err)
		}
		if got != want {
			return fmt.Errorf("root marker %+v does not match requested %+v", got, want)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "lost+found" {
			return fmt.Errorf("%q is not a fresh TTC root (found %q); existing TTC data roots are not migrated", root, entry.Name())
		}
	}
	data, err = json.Marshal(want)
	if err != nil {
		return err
	}
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		// Another node may have initialized a shared root concurrently. Validate
		// what it published instead of replacing its marker.
		published, readErr := os.ReadFile(markerPath)
		if readErr != nil {
			return readErr
		}
		var got daemonRootMarker
		if err := json.Unmarshal(published, &got); err != nil {
			return fmt.Errorf("invalid concurrently-created root marker: %w", err)
		}
		if got != want {
			return fmt.Errorf("root marker %+v does not match requested %+v", got, want)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := marker.Write(append(data, '\n')); err != nil {
		marker.Close()
		_ = os.Remove(markerPath)
		return err
	}
	if err := marker.Sync(); err != nil {
		marker.Close()
		_ = os.Remove(markerPath)
		return err
	}
	return marker.Close()
}

func lockDaemonLocalRoot(root string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(root, ".ttcd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another ttcd is already using %q: %w", root, err)
	}
	return lock, nil
}

func consumeDaemonClusterOption(args []string, opts *daemonClusterOptions) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if args[0] == "--cluster-auto-rebalance" {
		opts.AutoRebalance = true
		opts.Configured = true
		return 1, nil
	}
	setString := func(name string, dst *string) (int, error) {
		if args[0] == name {
			if len(args) < 2 || args[1] == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			*dst = args[1]
			return 2, nil
		}
		if strings.HasPrefix(args[0], name+"=") {
			*dst = strings.TrimPrefix(args[0], name+"=")
			if *dst == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			return 1, nil
		}
		return 0, nil
	}
	if consumed, err := setString("--cluster-peer", new(string)); consumed != 0 || err != nil {
		if err != nil {
			return consumed, err
		}
		var value string
		if args[0] == "--cluster-peer" {
			value = args[1]
		} else {
			value = strings.TrimPrefix(args[0], "--cluster-peer=")
		}
		opts.Peers = append(opts.Peers, value)
		opts.Configured = true
		return consumed, nil
	}
	for name, dst := range map[string]*string{
		"--cluster-listen": &opts.Listen, "--cluster-node-id": &opts.NodeID,
		"--cluster-repository-id": &opts.RepositoryID, "--cluster-cert": &opts.CertFile,
		"--cluster-key": &opts.KeyFile, "--cluster-ca": &opts.CAFile,
		"--cluster-advertise":        &opts.Advertise,
		"--cluster-capacity-command": &opts.CapacityCommand,
		"--cluster-capacity-webhook": &opts.CapacityWebhook,
		"--cluster-page-listen":      &opts.PageListen, "--cluster-page-advertise": &opts.PageAdvertise,
	} {
		if consumed, err := setString(name, dst); consumed != 0 || err != nil {
			if consumed != 0 {
				opts.Configured = true
			}
			return consumed, err
		}
	}
	optionValue := func(name string) (string, int, bool, error) {
		if args[0] == name {
			if len(args) < 2 || args[1] == "" {
				return "", 0, true, fmt.Errorf("%s requires a value", name)
			}
			return args[1], 2, true, nil
		}
		if strings.HasPrefix(args[0], name+"=") {
			value := strings.TrimPrefix(args[0], name+"=")
			if value == "" {
				return "", 0, true, fmt.Errorf("%s requires a value", name)
			}
			return value, 1, true, nil
		}
		return "", 0, false, nil
	}
	for name, dst := range map[string]*uint64{
		"--cluster-transfer-bytes":        &opts.PagingPolicy.TransferBytes,
		"--cluster-max-outstanding-bytes": &opts.PagingPolicy.MaxOutstandingBytes,
	} {
		value, consumed, matched, err := optionValue(name)
		if !matched {
			continue
		}
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("%s must be a positive byte count", name)
		}
		*dst = parsed
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-memory-target"); matched {
		if err != nil {
			return 0, err
		}
		parsed, err := ttcbundle.ParseShmSize(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("--cluster-memory-target must be a positive byte size")
		}
		opts.MemoryTarget = uint64(parsed)
		opts.Configured = true
		return consumed, nil
	}
	for name, dst := range map[string]*int{
		"--cluster-max-parallel":    &opts.PagingPolicy.MaxConcurrency,
		"--cluster-ack-batch-pages": &opts.PagingPolicy.AckBatchPages,
	} {
		value, consumed, matched, err := optionValue(name)
		if !matched {
			continue
		}
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		*dst = parsed
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-ack-interval"); matched {
		if err != nil {
			return 0, err
		}
		interval, err := time.ParseDuration(value)
		if err != nil || interval < 0 {
			return 0, fmt.Errorf("--cluster-ack-interval must be a non-negative duration")
		}
		opts.PagingPolicy.AckInterval = interval
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-lease"); matched {
		if err != nil {
			return 0, err
		}
		lease, err := time.ParseDuration(value)
		if err != nil || lease < time.Second || lease > 10*time.Minute {
			return 0, fmt.Errorf("--cluster-lease must be between 1s and 10m")
		}
		opts.LeaseDuration = lease
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-prefetch"); matched {
		if err != nil {
			return 0, err
		}
		opts.PagingPolicy.Prefetch = ttcpolicy.PrefetchPolicy(value)
		opts.PagingPolicy.Adaptive = opts.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-push"); matched {
		if err != nil {
			return 0, err
		}
		opts.PagingPolicy.Push = ttcpolicy.PushPolicy(value)
		opts.Configured = true
		return consumed, nil
	}
	const maxName = "--cluster-max-transactions"
	var value string
	consumed := 0
	if args[0] == maxName {
		if len(args) < 2 {
			return 0, fmt.Errorf("%s requires a value", maxName)
		}
		value, consumed = args[1], 2
	} else if strings.HasPrefix(args[0], maxName+"=") {
		value, consumed = strings.TrimPrefix(args[0], maxName+"="), 1
	}
	if consumed != 0 {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", maxName)
		}
		opts.MaxTransactions = parsed
		opts.Configured = true
		return consumed, nil
	}
	return 0, nil
}

func prepareDaemonCluster(opts daemonClusterOptions, dataRoot string) (*daemonClusterRuntime, error) {
	if !opts.Configured {
		return nil, nil
	}
	if opts.LeaseDuration == 0 {
		opts.LeaseDuration = 15 * time.Second
	}
	if opts.Listen == "" || opts.NodeID == "" || opts.RepositoryID == "" || opts.CertFile == "" || opts.KeyFile == "" || opts.CAFile == "" {
		return nil, errors.New("cluster mode requires listen address, node ID, repository ID, certificate, key, and CA")
	}
	capacitySink, err := newElasticCapacitySink(opts.CapacityCommand, opts.CapacityWebhook)
	if err != nil {
		return nil, err
	}
	if opts.PageListen == "" {
		return nil, errors.New("cluster page listen address is empty")
	}
	var advertiseURL, advertiseServerName string
	if opts.Advertise != "" {
		var err error
		advertiseURL, advertiseServerName, err = parseClusterAdvertise(opts.Advertise)
		if err != nil {
			return nil, err
		}
		if opts.PageAdvertise == "" {
			opts.PageAdvertise, err = advertisedPageAddress(advertiseURL)
			if err != nil {
				return nil, err
			}
		}
	}
	if (len(opts.Peers) != 0 || opts.Advertise != "") && opts.PageAdvertise == "" {
		return nil, errors.New("cluster nodes with peers require --cluster-page-advertise HOST:0")
	}
	if err := requireEphemeralClusterPagePort("--cluster-page-listen", opts.PageListen); err != nil {
		return nil, err
	}
	if opts.PageAdvertise != "" {
		if err := requireEphemeralClusterPagePort("--cluster-page-advertise", opts.PageAdvertise); err != nil {
			return nil, err
		}
	}
	opts.PagingPolicy.PageSize = hostarch.PageSize
	if err := opts.PagingPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("cluster paging policy: %w", err)
	}
	if opts.PagingPolicy.TransferBytes < pgalloc.SparseChunkBytes {
		return nil, fmt.Errorf("cluster transfer size %d is smaller than TTC's %d-byte atomic memory chunk", opts.PagingPolicy.TransferBytes, pgalloc.SparseChunkBytes)
	}
	tlsConfig, err := cluster.LoadServerTLSConfig(opts.CertFile, opts.KeyFile, opts.CAFile)
	if err != nil {
		return nil, err
	}
	store, err := cluster.OpenStore(filepath.Join(dataRoot, "cluster", "transactions"))
	if err != nil {
		return nil, err
	}
	capabilities := cluster.Capabilities{
		NodeID: opts.NodeID, ProtocolMin: cluster.ControlProtocolVersion, ProtocolMax: cluster.ControlProtocolVersion,
		BuildID: version.Version(), Architecture: runtime.GOARCH, BasePageSize: hostarch.PageSize,
		MaxTransferSize: 2 << 20, RepositoryID: opts.RepositoryID, PagingPolicy: opts.PagingPolicy,
		Features: []string{
			cluster.FeatureRemotePull, cluster.FeatureReversePush, cluster.FeatureAckBatch,
			cluster.FeatureNetworkRecreate, cluster.FeatureRuntimePrefetch, cluster.FeatureFilesystemBranch,
			cluster.FeatureSharedMemorySnapshot,
		},
	}
	if err := capabilities.Validate(); err != nil {
		return nil, err
	}
	var directory *cluster.NodeDirectory
	var nodeRecord cluster.NodeRecord
	if advertiseURL != "" {
		directory, err = cluster.OpenNodeDirectory(filepath.Join(dataRoot, "cluster", "nodes"))
		if err != nil {
			return nil, err
		}
		incarnation, err := cluster.NewNodeIncarnation()
		if err != nil {
			return nil, err
		}
		nodeRecord = cluster.NodeRecord{
			NodeID: opts.NodeID, Incarnation: incarnation,
			ControlURL: advertiseURL, ControlServerName: advertiseServerName,
			Capabilities: capabilities, MemoryTargetBytes: opts.MemoryTarget,
		}
	}
	clients := make(map[string]*cluster.Client, len(opts.Peers))
	for _, peerSpec := range opts.Peers {
		nodeID, baseURL, serverName, err := parseClusterPeer(peerSpec)
		if err != nil {
			return nil, err
		}
		if nodeID == opts.NodeID {
			return nil, fmt.Errorf("cluster peer %q is the local node", nodeID)
		}
		if _, ok := clients[nodeID]; ok {
			return nil, fmt.Errorf("cluster peer %q is configured more than once", nodeID)
		}
		clientTLS, err := cluster.LoadClientTLSConfig(opts.CertFile, opts.KeyFile, opts.CAFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("configuring cluster peer %q TLS: %w", nodeID, err)
		}
		client, err := cluster.NewClient(baseURL, clientTLS, 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("configuring cluster peer %q: %w", nodeID, err)
		}
		clients[nodeID] = client
	}
	return &daemonClusterRuntime{
		listen: opts.Listen, tlsConfig: tlsConfig,
		controlPlane: &cluster.ControlPlane{
			Capabilities: capabilities, Store: store, MaxTransactions: opts.MaxTransactions, Now: time.Now,
		},
		source: &distributedSourceRuntime{
			capabilities: capabilities, peers: clients,
			directory: directory, certFile: opts.CertFile, keyFile: opts.KeyFile, caFile: opts.CAFile,
			dynamicPeers: make(map[string]dynamicClusterPeer),
			pageListen:   opts.PageListen, pageAdvertise: opts.PageAdvertise,
		},
		directory: directory, nodeRecord: nodeRecord, lease: opts.LeaseDuration,
		autoRebalance: opts.AutoRebalance, capacitySink: capacitySink,
	}, nil
}

func requireEphemeralClusterPagePort(option, address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid %s address %q: %w", option, address, err)
	}
	if port != "0" {
		return fmt.Errorf("%s must use port 0; each concurrent remote fork requires an independent page listener", option)
	}
	return nil
}

func parseClusterPeer(value string) (nodeID, baseURL, serverName string, err error) {
	equals := strings.IndexByte(value, '=')
	comma := strings.LastIndexByte(value, ',')
	if equals <= 0 || comma <= equals+1 || comma == len(value)-1 {
		return "", "", "", fmt.Errorf("invalid --cluster-peer %q; want NODE=https://HOST:PORT,SERVER_NAME", value)
	}
	nodeID = value[:equals]
	baseURL = value[equals+1 : comma]
	serverName = value[comma+1:]
	if strings.TrimSpace(nodeID) != nodeID || strings.TrimSpace(serverName) != serverName {
		return "", "", "", fmt.Errorf("invalid whitespace in --cluster-peer %q", value)
	}
	return nodeID, baseURL, serverName, nil
}

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if unsafeNoHostIsolation || daemonEffectiveUID() == 0 {
		return nil
	}
	if err := createDaemonRootlessProbe(); err != nil {
		hint := "permit unprivileged user namespaces in the host kernel and LSM policy"
		if data, readErr := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); readErr == nil && strings.TrimSpace(string(data)) == "1" {
			hint = "the host AppArmor policy restricts unprivileged user namespaces; grant userns permission to TTC/runsc"
		}
		return fmt.Errorf("rootless mode is unavailable: creating a disposable user/mount namespace: %w; %s, or restart with --unsafe-no-host-isolation for trusted workloads", err, hint)
	}
	return nil
}

func createRootlessProbe() error {
	command := exec.Command("/proc/self/exe", daemonRootlessProbeArg)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

func secureDaemonDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket parent %q is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("socket directory %q is owned by UID %d, not %d", dir, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("socket directory %q permissions %04o allow access by other users", dir, info.Mode().Perm())
	}
	return nil
}

// prepareDaemonRuntimeStorage ensures that control sockets and runsc's live
// state remain on node-local storage. In cluster mode TTC_ROOT may be shared,
// but TTC_LOCAL_ROOT contains all paths checked here.
func prepareDaemonRuntimeStorage(socket, root string) error {
	paths := []struct {
		name string
		path string
	}{
		{name: "socket directory", path: filepath.Dir(socket)},
		{name: "runsc state root", path: root},
	}
	checked := make(map[string]struct{}, len(paths))
	for _, candidate := range paths {
		clean := filepath.Clean(candidate.path)
		if _, ok := checked[clean]; ok {
			continue
		}
		checked[clean] = struct{}{}
		if err := os.MkdirAll(clean, 0700); err != nil {
			return fmt.Errorf("creating %s %q: %w", candidate.name, clean, err)
		}
		var statfs unix.Statfs_t
		if err := daemonStatfs(clean, &statfs); err != nil {
			return fmt.Errorf("checking %s filesystem %q: %w", candidate.name, clean, err)
		}
		if int64(statfs.Type) == int64(unix.NFS_SUPER_MAGIC) {
			return fmt.Errorf("%s %q is on NFS; TTC_SOCKET and TTC_LOCAL_ROOT runtime state must be node-local (only TTC_ROOT may be shared)", candidate.name, clean)
		}
	}
	return nil
}

func serveDaemon(socket, runtimePlatform string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, root, runsc string, volumeCapability ttcvolume.Capability, stdout, stderr io.Writer) error {
	return serveDaemonWithCluster(socket, runtimePlatform, unsafeNoHostIsolation, kernelCompatibility, dataRoot, dataRoot, root, runsc, volumeCapability, nil, stdout, stderr)
}

func serveDaemonWithCluster(socket, runtimePlatform string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, localRoot, root, runsc string, volumeCapability ttcvolume.Capability, clusterRuntime *daemonClusterRuntime, stdout, stderr io.Writer) error {
	dir := filepath.Dir(socket)
	if err := secureDaemonDirectory(dir); err != nil {
		return err
	}
	startLock, err := os.OpenFile(socket+".start.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening daemon startup lock: %w", err)
	}
	defer startLock.Close()
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking daemon startup: %w", err)
	}
	if conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("daemon is already listening on %s", socket)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("securing daemon socket: %w", err)
	}
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlocking daemon startup: %w", err)
	}
	server := &daemonServer{
		listener:              listener,
		socket:                socket,
		runtimePlatform:       runtimePlatform,
		unsafeNoHostIsolation: unsafeNoHostIsolation,
		disableHostGSO:        kernelCompatibility.disableHostGSO,
		hostKernelRelease:     kernelCompatibility.release,
		dataRoot:              dataRoot,
		localRoot:             localRoot,
		root:                  root,
		runsc:                 runsc,
		clientExecutable:      siblingTTCExecutable(),
		volumeCapability:      volumeCapability,
		reconciled:            make(map[string]bool),
		mutationLocks:         make(map[string]*sync.Mutex),
	}
	publisher := &branchPublisher{sharedRoot: dataRoot, localRoot: localRoot}
	publisher.event = func(event DaemonEvent) { server.recordEvent(dataRoot, event) }
	if clusterRuntime != nil {
		publisher.nodeID = clusterRuntime.nodeRecord.NodeID
		publisher.incarnation = clusterRuntime.nodeRecord.Incarnation
	}
	publicationQueue, err := ttcstorage.Open(localRoot, publisher)
	if err != nil {
		return fmt.Errorf("opening durable publication queue: %w", err)
	}
	server.publicationQueue = publicationQueue
	defer publicationQueue.Close()
	// Recover namespace-atomic deletions before announcing readiness. Failure
	// to physically remove one tombstone is non-fatal: it is outside the active
	// container namespace and periodic maintenance will retry it.
	unlockRecovery := server.lockDataRoot(dataRoot)
	_, recoveryErr := (ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}).RecoverContainerRemovals()
	unlockRecovery()
	if recoveryErr != nil && stderr != nil {
		fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers: %v\n", recoveryErr)
	}
	maintenanceDone := make(chan struct{})
	defer close(maintenanceDone)
	go maintainRepositories(dataRoot, localRoot, publicationQueue, stderr, maintenanceDone, func() func() {
		return server.lockDataRoot(dataRoot)
	})
	clusterDone := make(chan struct{})
	var elasticHeartbeatStopped <-chan struct{}
	var elasticRecord *cluster.NodeRecord
	defer func() {
		close(clusterDone)
		if elasticHeartbeatStopped != nil {
			<-elasticHeartbeatStopped
		}
		if elasticRecord != nil && clusterRuntime != nil && clusterRuntime.directory != nil {
			if _, err := clusterRuntime.directory.Transition(elasticRecord.NodeID, elasticRecord.Incarnation, cluster.NodeOffline, time.Now(), clusterRuntime.lease); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: publishing cluster node offline state: %v\n", err)
			}
			if server.cleanStop.Load() {
				if err := removeCompletedElasticDrainIntent(dataRoot, elasticRecord.NodeID); err != nil && stderr != nil {
					fmt.Fprintf(stderr, "ttcd: warning: consuming completed drain intent: %v\n", err)
				}
			}
		}
	}()
	var clusterServer *http.Server
	clusterErr := make(chan error, 1)
	if clusterRuntime != nil {
		targetBackend := directBackend(options{
			root: server.root, runsc: server.runsc, dataRoot: server.dataRoot, localRoot: server.localRoot,
			runtimePlatform:       server.runtimePlatform,
			unsafeNoHostIsolation: server.unsafeNoHostIsolation,
			disableHostGSO:        server.disableHostGSO,
		}, strings.NewReader(""), io.Discard, io.Discard)
		server.configureVolumeBackend(targetBackend)
		targetBackend.consoleSidecars = server
		targetBackend.nodeID = clusterRuntime.controlPlane.Capabilities.NodeID
		targetBackend.distributedSource = clusterRuntime.source
		targetRuntime := &daemonTargetRuntime{
			backend: targetBackend, nodeID: clusterRuntime.controlPlane.Capabilities.NodeID,
			incarnation: clusterRuntime.nodeRecord.Incarnation, directory: clusterRuntime.directory,
			store: clusterRuntime.controlPlane.Store, done: clusterDone,
		}
		clusterRuntime.controlPlane.TargetRuntime = targetRuntime
		clusterRuntime.controlPlane.CommandProxy = daemonClusterProxy{socket: socket}
		clusterListener, err := net.Listen("tcp", clusterRuntime.listen)
		if err != nil {
			return fmt.Errorf("listening for cluster peers on %s: %w", clusterRuntime.listen, err)
		}
		clusterRuntime.listen = clusterListener.Addr().String()
		if clusterRuntime.directory != nil {
			resolved, err := resolveClusterAdvertise(clusterRuntime.nodeRecord.ControlURL, clusterRuntime.listen)
			if err != nil {
				clusterListener.Close()
				return err
			}
			clusterRuntime.nodeRecord.ControlURL = resolved
			clusterRuntime.nodeRecord.MemoryCurrentBytes, _ = currentNodeMemoryBytes()
			drainIntent, err := loadElasticDrainIntent(dataRoot, clusterRuntime.nodeRecord.NodeID)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("loading elastic drain recovery state: %w", err)
			}
			registered, err := clusterRuntime.directory.Register(clusterRuntime.nodeRecord, time.Now(), clusterRuntime.lease)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("registering elastic cluster node: %w", err)
			}
			admittedState := cluster.NodeReady
			if drainIntent != nil {
				admittedState = cluster.NodeDraining
			}
			ready, err := clusterRuntime.directory.Transition(registered.NodeID, registered.Incarnation, admittedState, time.Now(), clusterRuntime.lease)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("admitting elastic cluster node: %w", err)
			}
			if ready.State == cluster.NodeReady && ready.MemoryTargetBytes != 0 && ready.MemoryCurrentBytes > ready.MemoryTargetBytes {
				ready, err = clusterRuntime.directory.Transition(ready.NodeID, ready.Incarnation, cluster.NodePressured, time.Now(), clusterRuntime.lease)
				if err != nil {
					clusterListener.Close()
					return fmt.Errorf("marking elastic cluster node pressured: %w", err)
				}
			}
			elasticRecord = ready
		}
		server.clusterCapabilities = &clusterRuntime.controlPlane.Capabilities
		server.clusterStore = clusterRuntime.controlPlane.Store
		server.clusterDirectory = clusterRuntime.directory
		server.clusterListen = clusterRuntime.listen
		server.distributedSource = clusterRuntime.source
		clusterServer = &http.Server{
			Handler: clusterRuntime.controlPlane, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		}
		tlsListener := tls.NewListener(clusterListener, clusterRuntime.tlsConfig)
		go func() {
			err := clusterServer.Serve(tlsListener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			clusterErr <- err
			listener.Close()
		}()
		defer clusterServer.Close()
		if elasticRecord != nil {
			heartbeatFatal := make(chan error, 1)
			heartbeatStopped := make(chan struct{})
			elasticHeartbeatStopped = heartbeatStopped
			go func(record cluster.NodeRecord) {
				defer close(heartbeatStopped)
				maintainElasticNode(clusterRuntime.directory, record, clusterRuntime.lease, stderr, clusterDone, heartbeatFatal)
			}(*elasticRecord)
			go func() {
				select {
				case err := <-heartbeatFatal:
					clusterErr <- err
					listener.Close()
					clusterServer.Close()
				case <-clusterDone:
				}
			}()
		}
		resumeDistributedForkMonitors(targetRuntime, targetBackend, clusterRuntime)
		if elasticRecord != nil {
			go maintainElasticMigrationRecovery(targetBackend, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRoot(server.dataRoot)
				defer unlock()
				server.reconcile(targetBackend)
				return run()
			})
			go maintainElasticDrain(targetBackend, elasticRecord.NodeID, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRoot(server.dataRoot)
				defer unlock()
				server.reconcile(targetBackend)
				return run()
			})
		}
		if clusterRuntime.autoRebalance && elasticRecord != nil {
			go maintainElasticRebalancer(targetBackend, elasticRecord.NodeID, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRoot(server.dataRoot)
				defer unlock()
				return run()
			})
		}
		if elasticRecord != nil && clusterRuntime.capacitySink != nil && clusterRuntime.capacitySink.Configured() {
			go maintainElasticCapacityRequests(dataRoot, elasticRecord.NodeID, clusterRuntime.directory, clusterRuntime.capacitySink, stderr, clusterDone)
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "ttcd listening on %s (API v%d, platform=%s, isolation=%s, volume-copy=%s)\n", socket, daemonAPIVersion, runtimePlatform, daemonIsolationMode(unsafeNoHostIsolation), volumeCapability.Mode)
		if clusterRuntime != nil {
			fmt.Fprintf(stdout, "ttcd cluster node %s listening on %s (repository=%s)\n", clusterRuntime.controlPlane.Capabilities.NodeID, clusterRuntime.listen, clusterRuntime.controlPlane.Capabilities.RepositoryID)
		}
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				if clusterServer != nil {
					select {
					case clusterServeErr := <-clusterErr:
						if clusterServeErr != nil {
							return fmt.Errorf("serving cluster peers: %w", clusterServeErr)
						}
					default:
					}
				}
				return nil
			}
			return fmt.Errorf("accepting client: %w", err)
		}
		go server.handle(conn, stderr)
	}
}

func maintainRepositories(dataRoot, localRoot string, publications *ttcstorage.Queue, stderr io.Writer, done <-chan struct{}, lock func() func()) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}
		unlock := func() {}
		if lock != nil {
			unlock = lock()
		}
		store := ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}
		if _, err := store.RecoverContainerRemovals(); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers: %v\n", err)
		}
		if _, err := store.GarbageCollectRepositories(); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: collecting unreachable volume data: %v\n", err)
		}
		if err := collectSparseMemoryRepositories(dataRoot, localRoot); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: collecting consumed sparse memory: %v\n", err)
		}
		if err := collectColdStorage(dataRoot, localRoot, publications); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: collecting cold packs: %v\n", err)
		}
		unlock()
		timer.Reset(repositoryMaintenanceInterval)
	}
}

func collectSparseMemoryRepositories(dataRoot, localRoot string) error {
	paths := []string{filepath.Join(dataRoot, "cluster", "memory-chunks")}
	containersRoot := filepath.Join(localRoot, "containers")
	entries, err := os.ReadDir(containersRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			paths = append(paths, filepath.Join(containersRoot, entry.Name(), "memory", "chunks"))
		}
	}
	var result error
	for _, path := range paths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			result = errors.Join(result, err)
			continue
		}
		repository, err := ttcchunk.OpenMemoryRepository(path)
		if err == nil {
			_, err = repository.CollectGarbage(ttcchunk.GCOptions{})
		}
		if repository != nil {
			err = errors.Join(err, repository.Close())
		}
		result = errors.Join(result, err)
	}
	return result
}

func checkDaemonPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("non-Unix daemon connection %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil {
		return errors.New("daemon peer credentials are unavailable")
	}
	if int(credential.Uid) != os.Geteuid() {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", credential.Uid, os.Geteuid())
	}
	return nil
}

func (s *daemonServer) handle(conn net.Conn, serverErrors io.Writer) {
	defer conn.Close()
	if err := checkDaemonPeer(conn); err != nil {
		if serverErrors != nil {
			fmt.Fprintf(serverErrors, "ttcd: rejecting peer: %v\n", err)
		}
		return
	}
	reader := bufio.NewReader(conn)
	header, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	var request daemonRequest
	if err := json.Unmarshal([]byte(header), &request); err != nil {
		return
	}
	var writeMu sync.Mutex
	writeError := func(err error) {
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStderr, []byte("ttcd: "+err.Error()+"\n"))
		_ = writeDaemonStatus(conn, &writeMu, 1)
	}
	// Stop is deliberately a minimal, stable escape hatch. In particular, a
	// newly installed client must be able to stop an older daemon before
	// starting its matching sibling. Peer-UID validation above still applies.
	if request.Action != "stop" && request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	if request.Action != "stop" && request.ClientBuildID != "" && request.ClientBuildID != version.Version() {
		writeError(fmt.Errorf("client build %q does not match daemon build %q; only 'ttc daemon stop' is allowed across versions", request.ClientBuildID, version.Version()))
		return
	}
	switch request.Action {
	case "ping":
		status := map[string]any{
			"version":                  daemonAPIVersion,
			"build_id":                 version.Version(),
			"pid":                      os.Getpid(),
			"runtime_platform":         s.runtimePlatform,
			"isolation_mode":           daemonIsolationMode(s.unsafeNoHostIsolation),
			"unsafe_no_host_isolation": s.unsafeNoHostIsolation,
			"host_kernel_release":      s.hostKernelRelease,
			"host_gso_enabled":         !s.disableHostGSO,
			"data_root":                s.dataRoot,
			"shared_root":              s.dataRoot,
			"local_root":               s.localRoot,
			"volume_copy_mode":         s.volumeCapability.Mode,
			"volume_filesystem_type":   s.volumeCapability.FilesystemType,
			"volume_device":            s.volumeCapability.Device,
		}
		if s.clusterCapabilities != nil {
			clusterStatus := map[string]any{
				"listen": s.clusterListen, "capabilities": s.clusterCapabilities,
			}
			if s.clusterDirectory != nil {
				if nodes, err := s.clusterDirectory.List(); err == nil {
					clusterStatus["nodes"] = nodes
				} else {
					clusterStatus["directory_error"] = err.Error()
				}
			}
			status["cluster"] = clusterStatus
		}
		payload, _ := json.Marshal(status)
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		if request.ManageStop {
			opts := s.optionsFromRequest(request.Options)
			backend := directBackend(opts, strings.NewReader(""), io.Discard,
				daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
			s.configureVolumeBackend(backend)
			backend.consoleSidecars = s
			if s.clusterCapabilities != nil {
				backend.nodeID = s.clusterCapabilities.NodeID
			}
			unlock := s.lockDataRoot(opts.dataRoot)
			var err error
			if s.clusterCapabilities != nil {
				err = drainSourceDependencies(backend, s.distributedSource, s.clusterStore, s.clusterCapabilities.NodeID)
			}
			if err == nil {
				switch {
				case slices.Contains(request.Args, "--dehydrate-all"):
					err = dehydrateDaemonContainers(backend)
				case slices.Contains(request.Args, "--stop-all"):
					err = stopDaemonContainers(backend)
				default:
					err = rejectActiveDaemonContainers(backend)
				}
			}
			unlock()
			if err != nil {
				writeError(fmt.Errorf("stopping managed containers: %w", err))
				return
			}
			if s.publicationQueue != nil {
				if err := s.publicationQueue.Drain(context.Background()); err != nil {
					writeError(fmt.Errorf("draining durable publications: %w", err))
					return
				}
			}
		}
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.cleanStop.Store(true)
		s.stopOnce.Do(func() { s.listener.Close() })
		return
	case "mutation-control":
		status := s.handleMutationControl(s.dataRoot, request.Args, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout}, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	case "command":
	default:
		writeError(fmt.Errorf("unknown daemon action %q", request.Action))
		return
	}
	if len(request.Args) == 0 {
		writeError(errors.New("empty command"))
		return
	}
	if s.distributedSource != nil && s.clusterCapabilities != nil && !request.DistributedLeaf {
		handled, status, stdout, stderr, err := s.executeDistributedFamilyMutation(request)
		if err != nil {
			writeError(err)
			return
		}
		if handled {
			writeDaemonBufferedResult(conn, &writeMu, stdout, stderr, status)
			return
		}
	}
	if s.distributedSource != nil && s.clusterCapabilities != nil {
		owner := ""
		var err error
		if len(request.PlacementClaims) == 0 {
			owner, request.PlacementClaims, err = daemonCommandPlacement(s.dataRoot, request.Args)
		} else {
			for _, claim := range request.PlacementClaims {
				if owner != "" && owner != claim.OwnerNode {
					err = fmt.Errorf("routed command claims span cluster nodes %q and %q", owner, claim.OwnerNode)
					break
				}
				owner = claim.OwnerNode
			}
		}
		if err != nil {
			writeError(err)
			return
		}
		if owner == "" && daemonCommandNeedsPlacementAdmission(request.Args) {
			if request.PlacementNode != "" {
				owner = request.PlacementNode
			} else {
				records, listErr := s.clusterDirectory.List()
				if listErr != nil {
					writeError(fmt.Errorf("selecting cluster placement: %w", listErr))
					return
				}
				selected, selectErr := selectAdmissionNode(records, *s.clusterCapabilities, time.Now())
				if selectErr != nil {
					writeError(selectErr)
					return
				}
				owner = selected.NodeID
				request.PlacementNode = owner
			}
		}
		if owner != "" && owner != s.clusterCapabilities.NodeID {
			if slices.Contains(request.Route, owner) || slices.Contains(request.Route, s.clusterCapabilities.NodeID) {
				writeError(fmt.Errorf("cluster command routing loop through %v to node %q", request.Route, owner))
				return
			}
			peer, err := s.distributedSource.peer(owner, false)
			if err != nil {
				writeError(err)
				return
			}
			request.Route = append(request.Route, s.clusterCapabilities.NodeID)
			encoded, err := json.Marshal(&request)
			if err != nil {
				writeError(err)
				return
			}
			var input io.Reader = bytes.NewReader(append(encoded, '\n'))
			if request.Stdin {
				input = io.MultiReader(input, reader)
			}
			if err := peer.ProxyDaemon(context.Background(), input, conn); err != nil {
				writeError(fmt.Errorf("proxying command to node %q: %w", owner, err))
			}
			return
		}
	}
	if s.clusterDirectory != nil && daemonCommandNeedsPlacementAdmission(request.Args) {
		record, err := s.clusterDirectory.Lookup(s.clusterCapabilities.NodeID)
		if err != nil {
			writeError(fmt.Errorf("checking cluster admission: %w", err))
			return
		}
		if !record.AcceptsPlacement(time.Now()) {
			writeError(fmt.Errorf("cluster node %q is not accepting new work (state=%s)", record.NodeID, record.State))
			return
		}
	}

	stdinReader, stdinWriter := io.Pipe()
	if !request.Stdin {
		stdinWriter.Close()
	}
	terminalInput := newDaemonTerminalInput(stdinReader, conn, &writeMu)
	go receiveDaemonInput(reader, stdinWriter, request.Stdin, terminalInput)

	opts := s.optionsFromRequest(request.Options)
	cleanArgs, leaseKey, takeover, err := daemonAttachStdinLease(opts, request.Args, request.Stdin)
	if err != nil {
		stdinReader.Close()
		writeError(err)
		return
	}
	request.Args = cleanArgs
	if leaseKey != "" {
		release, err := s.acquireStdinLease(leaseKey, takeover, func() {
			_ = stdinWriter.CloseWithError(errDetached)
			_ = conn.Close()
		})
		if err != nil {
			stdinReader.Close()
			writeError(err)
			return
		}
		defer release()
	}
	backend := directBackend(opts, terminalInput,
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout},
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
	s.configureVolumeBackend(backend)
	backend.clientTERM = request.ClientTERM
	backend.progressTerminal = request.StderrTerminal
	backend.progressWidth = request.StderrWidth
	backend.consoleSidecars = s
	clientCanceled := make(chan struct{})
	var cancelClientOnce sync.Once
	backend.clientCanceled = clientCanceled
	backend.cancelClient = func() {
		cancelClientOnce.Do(func() {
			close(clientCanceled)
			_ = conn.Close()
		})
	}
	backend.distributedSource = s.distributedSource
	if s.clusterCapabilities != nil {
		backend.nodeID = s.clusterCapabilities.NodeID
	}
	if request.Args[0] == "events" {
		status := 0
		if err := s.streamEvents(opts.dataRoot, request.Args[1:], backend.stdout); err != nil {
			fmt.Fprintf(backend.stderr, "ttc: %v\n", err)
			status = 1
		}
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	if len(request.Args) >= 2 && request.Args[0] == "tty" && request.Args[1] == "watch" {
		backend.stdout = &daemonJSONFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameTTYState}
		status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	rawUnlock := s.lockMutation(opts.dataRoot, request.Args)
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(rawUnlock) }
	if daemonCommandReleasesLifecycleAfterStart(request.Args) {
		backend.lifecycleReady = unlock
	}
	// Reconciliation lists runsc state and may reap runtimes that have lost
	// their Sentry. Keep it in the same lifecycle critical section as the
	// command: otherwise a read-only `ttc ps` can observe `runsc create` after
	// its state file is published but before its control server is ready and
	// mistake the in-progress runtime for stale state.
	s.reconcile(backend)
	if err := validateDaemonPlacementClaims(opts.dataRoot, backend.nodeID, request.PlacementClaims); err != nil {
		unlock()
		stdinReader.Close()
		writeError(err)
		return
	}
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		record, replay, err := beginDaemonMutation(opts.dataRoot, request)
		if err != nil {
			unlock()
			stdinReader.Close()
			writeError(err)
			return
		}
		if replay {
			unlock()
			stdinReader.Close()
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
			return
		}
		var mutationStdout, mutationStderr bytes.Buffer
		mutationBackend := directBackend(opts, stdinReader, &mutationStdout, &mutationStderr)
		s.configureVolumeBackend(mutationBackend)
		mutationBackend.consoleSidecars = s
		mutationBackend.distributedSource = s.distributedSource
		if s.clusterCapabilities != nil {
			mutationBackend.nodeID = s.clusterCapabilities.NodeID
		}
		status := runCommand(request.Args, mutationBackend.stdout, mutationBackend.stderr, mutationBackend)
		s.recordCommandEvents(opts.dataRoot, request.Args, status)
		if status == 0 {
			if err := mutationBackend.reconcilePlacementCatalog(); err != nil {
				fmt.Fprintf(&mutationStderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
			}
		}
		completed, err := completeDaemonMutation(opts.dataRoot, record, status, mutationStdout.Bytes(), mutationStderr.Bytes())
		if err != nil {
			fmt.Fprintf(&mutationStderr, "ttcd: %v; command outcome is indeterminate\n", err)
			status = 1
		} else {
			record = completed
		}
		unlock()
		stdinReader.Close()
		if err == nil {
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
		} else {
			writeDaemonBufferedResult(conn, &writeMu, mutationStdout.Bytes(), mutationStderr.Bytes(), status)
		}
		return
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	if status == 0 {
		if err := backend.reconcilePlacementCatalog(); err != nil {
			fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
		}
	}
	unlock()
	stdinReader.Close()
	_ = writeDaemonStatus(conn, &writeMu, status)
}

func writeDaemonBufferedResult(conn net.Conn, writeMu *sync.Mutex, stdout, stderr []byte, status int) {
	stdoutWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStdout}
	stderrWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStderr}
	_, _ = stdoutWriter.Write(stdout)
	_, _ = stderrWriter.Write(stderr)
	_ = writeDaemonStatus(conn, writeMu, status)
}

func daemonCommandNeedsMutationLock(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "create", "pull", "load", "save", "commit", "rmi", "rm", "_auto-remove", "_purge-runtime", "pause", "resume", "unpause", "dehydrate", "rehydrate", "kill", "stop", "restart", "fork", "checkpoint", "restore", "migrate":
		return true
	case "node":
		return len(args) > 1 && args[1] != "ls" && args[1] != "_planner-resources"
	case "cp":
		_, _, _, err := parseCopyCommandArgs(args[1:])
		return err == nil
	case "run":
		return daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !daemonCommandBoolFlag(args[1:], 'a', "--attach")
	case "image":
		return len(args) > 1 && (args[1] == "pull" || args[1] == "load" || args[1] == "save" || args[1] == "rm" || args[1] == "remove")
	case "port":
		return len(args) > 1 && (args[1] == "publish" || args[1] == "unpublish")
	default:
		return false
	}
}

// daemonCommandReadsLifecycle reports commands whose runtime inventory must be
// a coherent snapshot. They do not enter the mutation journal, but must not
// probe or reconcile runsc state while create/delete or another lifecycle
// mutation is only partially complete.
func daemonCommandReadsLifecycle(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "ps", "ls", "inspect", "top", "stats", "diff":
		return true
	default:
		return daemonCommandReleasesLifecycleAfterStart(args)
	}
}

// daemonCommandReleasesLifecycleAfterStart identifies attached lifecycle
// commands. They need exclusion only through runtime publication; holding the
// lock while the client remains attached would block the daemon for the whole
// lifetime of the guest process.
func daemonCommandReleasesLifecycleAfterStart(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "run":
		return !daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return daemonCommandBoolFlag(args[1:], 'a', "--attach")
	default:
		return false
	}
}

// daemonCommandNeedsMutationJournal identifies state-changing commands whose
// small output can be durably buffered for retry. Container-to-host cp still
// takes the lifecycle lock, but its arbitrarily large archive is streamed.
func daemonCommandNeedsMutationJournal(args []string) bool {
	if !daemonCommandNeedsMutationLock(args) {
		return false
	}
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	// Explicit image pulls are content-addressed and naturally idempotent.
	// Stream their progress live instead of buffering it for mutation replay.
	if len(args) != 0 && args[0] == "pull" {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "pull" {
		return false
	}
	// A load consumes a stream the client holds. Replaying it would need the
	// archive a second time, and a pipe cannot supply one.
	if len(args) != 0 && args[0] == "load" {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "load" {
		return false
	}
	// Save is read-only but holds the lifecycle lock so image replacement and
	// garbage collection cannot race the archive traversal. Commit reads live
	// branch state and may temporarily pause it. Neither operation is suitable
	// for replay from a buffered mutation record.
	if len(args) != 0 && (args[0] == "save" || args[0] == "commit") {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "save" {
		return false
	}
	// Checkpoint publishes a caller-named host file and therefore cannot be
	// safely replayed by the data-root mutation journal.
	if len(args) != 0 && args[0] == "checkpoint" {
		return false
	}
	if len(args) == 0 || args[0] != "cp" {
		return true
	}
	_, _, destination, err := parseCopyCommandArgs(args[1:])
	if err != nil {
		return false
	}
	_, destinationContainer, err := splitCopyContainerEndpoint(destination)
	return err == nil && destinationContainer
}

func daemonCommandBoolFlag(args []string, short byte, long string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long {
			value = true
			continue
		}
		if strings.HasPrefix(arg, long+"=") {
			if parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, long+"=")); err == nil {
				value = parsed
			}
			continue
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(short)) {
			value = true
		}
	}
	return value
}

func (s *daemonServer) lockMutation(dataRoot string, args []string) func() {
	if !daemonCommandNeedsMutationLock(args) && !daemonCommandReadsLifecycle(args) {
		return func() {}
	}
	return s.lockDataRoot(dataRoot)
}

func (s *daemonServer) lockDataRoot(dataRoot string) func() {
	key := filepath.Clean(dataRoot)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	s.mutationMu.Lock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.Mutex)
	}
	lock := s.mutationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.mutationLocks[key] = lock
	}
	s.mutationMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func receiveDaemonInput(reader *bufio.Reader, writer *io.PipeWriter, allowStdin bool, terminal *daemonTerminalInput) {
	var stdinQueue *daemonStdinQueue
	if allowStdin {
		stdinQueue = newDaemonStdinQueue(writer, func(bytes int) error {
			return writeDaemonFrame(terminal.conn, terminal.writeMu, daemonFrameStdinAck, encodeDaemonStdinAcknowledgement(bytes))
		})
	}
	stdinOpen := allowStdin
	fail := func(err error) {
		if stdinQueue != nil {
			stdinQueue.closeWithError(err)
		}
	}
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			// The client is gone — not "done sending input", which is the EOF
			// frame below, but disconnected: a closed laptop, a dropped ssh
			// pipe, a process killed mid-turn.
			//
			// Reported as a detach rather than as the read error it is,
			// because that is what the commands on the other side understand.
			// An attach told only that its stdin ended keeps running: it sits
			// in its drain loop waiting for output that an idle session never
			// produces, holding the terminal's stdin lock against everyone,
			// including the same client coming back. A tool with a window open
			// is silent for minutes at a time, and one lost connection made
			// its session unreachable for good.
			fail(errDetached)
			return
		}
		switch typ {
		case daemonFrameStdin:
			if !stdinOpen {
				fail(errors.New("unexpected stdin frame"))
				return
			}
			if !stdinQueue.enqueue(payload) {
				fail(fmt.Errorf("daemon stdin exceeded the %d-byte flow-control window", daemonStdinWindowBytes))
				return
			}
		case daemonFrameEOF:
			if stdinOpen {
				stdinOpen = false
				stdinQueue.closeWithError(nil)
			}
		case daemonFrameResize:
			rows, cols, err := decodeDaemonTerminalSize(payload)
			if err != nil {
				fail(err)
				return
			}
			terminal.updateSize(rows, cols)
		default:
			fail(fmt.Errorf("unexpected client frame %q", typ))
			return
		}
	}
}

func (s *daemonServer) reconcile(backend *runtimeBackend) {
	if backend.dataRoot == "" {
		return
	}
	key := backend.dataRoot + "\x00" + backend.root
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconciled[key] {
		return
	}
	// A daemon may have died after branch ownership moved but before deleting
	// its old physical shell. Reclaim only runtimes and branches this node no
	// longer owns; shared metadata remains authoritative.
	_ = backend.cleanupNonOwnedRuntimeBranches()
	// listFamilies reconciles stale persisted running/paused state against runsc.
	families, err := backend.listFamilies()
	if err != nil {
		return
	}
	for _, family := range families {
		container, containerErr := backend.containerStore().LoadContainer(family.ID)
		for _, branch := range family.Branches {
			if branch.State != "running" && branch.State != "paused" {
				continue
			}
			target := selector{container: family.ID, branch: branch.ID}
			_, _ = backend.ensureConsoleDrain(target, false)
			if containerErr == nil {
				_ = backend.ensurePublishedPortsForBranch(container, branch.ID)
			}
			sessions, err := backend.execSessions(target)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if session.State == "running" {
					_, _ = backend.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
				}
			}
		}
	}
	// A crash can strand a cluster memory root between immutable publication
	// and target metadata ownership. Reconcile only after runtime/container
	// state has been refreshed, and fail conservatively inside the helper.
	_ = backend.reconcileClusterSparseMemoryRoots()
	_ = backend.reconcilePlacementCatalog()
	s.reconciled[key] = true
}

func writeDaemonFrame(writer io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return fmt.Errorf("daemon frame is too large: %d", len(payload))
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	if err := writeDaemonBytes(writer, header[:]); err != nil {
		return err
	}
	return writeDaemonBytes(writer, payload)
}

func writeDaemonBytes(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readDaemonFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > daemonMaxFrame {
		return 0, nil, fmt.Errorf("daemon frame is too large: %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeDaemonStatus(writer io.Writer, mu *sync.Mutex, status int) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(status))
	return writeDaemonFrame(writer, mu, daemonFrameStatus, payload[:])
}

// imageLoadReadsStdin reports whether Docker-style load reads the client's
// standard input rather than a daemon-host file named by --input.
func imageLoadReadsStdin(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-i" || arg == "--input" || arg == "-input":
			return false
		case strings.HasPrefix(arg, "--input=") || strings.HasPrefix(arg, "-input="):
			return false
		case arg == "--platform" || arg == "-platform":
			i++
		}
	}
	return true
}

func daemonCommandUsesStdin(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "cp":
		_, source, _, err := parseCopyCommandArgs(args[1:])
		return err == nil && source == "-"
	case "load":
		return imageLoadReadsStdin(args[1:])
	case "image":
		return len(args) > 1 && args[1] == "load" && imageLoadReadsStdin(args[2:])
	case "attach", "run", "start":
	case "exec":
		if len(args) > 1 && (args[1] == "ls" || args[1] == "wait" || args[1] == "kill") {
			return false
		}
	default:
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "-i" || arg == "--interactive" {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.TrimPrefix(arg, "-"), "i") {
			return true
		}
	}
	return false
}

func runViaDaemon(opts options, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var err error
	args, err = normalizeDaemonImageArchivePaths(args)
	if err != nil {
		return 1, err
	}
	stderrTerminal, stderrWidth := terminalWriterInfo(stderr)
	request := daemonRequest{
		Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "command", Options: wireOptions(opts),
		Args: append([]string(nil), args...), Stdin: daemonCommandUsesStdin(args), ClientTERM: os.Getenv("TERM"),
		StderrTerminal: stderrTerminal, StderrWidth: stderrWidth,
	}
	if daemonCommandNeedsMutationJournal(args) {
		requestID, err := newDaemonRequestID()
		if err != nil {
			return 1, err
		}
		request.RequestID = requestID
	}
	attempts := 1
	if request.RequestID != "" && !request.Stdin {
		attempts = 2
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			return 1, err
		}
		commandStdout, commandStderr := stdout, stderr
		var bufferedStdout, bufferedStderr bytes.Buffer
		if request.RequestID != "" {
			commandStdout, commandStderr = &bufferedStdout, &bufferedStderr
		}
		status, err := exchangeDaemon(conn, request, stdin, commandStdout, commandStderr)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if request.RequestID != "" {
			if _, err := stdout.Write(bufferedStdout.Bytes()); err != nil {
				return 1, err
			}
			if _, err := stderr.Write(bufferedStderr.Bytes()); err != nil {
				return 1, err
			}
		}
		return status, nil
	}
	if request.RequestID != "" {
		return 1, fmt.Errorf("daemon mutation request %s: %w", request.RequestID, lastErr)
	}
	return 1, lastErr
}

// normalizeDaemonImageArchivePaths gives load/save the same client-relative
// path behavior as Docker. The command itself runs in ttcd, whose working
// directory may be unrelated to the shell that invoked ttc.
func normalizeDaemonImageArchivePaths(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	commandIndex := 0
	if args[0] == "image" {
		if len(args) < 2 {
			return args, nil
		}
		commandIndex = 1
	}
	var short, long string
	switch args[commandIndex] {
	case "load":
		short, long = "-i", "--input"
	case "save":
		short, long = "-o", "--output"
	default:
		return args, nil
	}
	result := append([]string(nil), args...)
	for i := commandIndex + 1; i < len(result); i++ {
		arg := result[i]
		if arg == short || arg == long || arg == "-"+strings.TrimPrefix(long, "--") {
			if i+1 >= len(result) {
				return result, nil // Let flag parsing produce the canonical error.
			}
			if result[i+1] == "" {
				return result, nil
			}
			absolute, err := filepath.Abs(result[i+1])
			if err != nil {
				return nil, fmt.Errorf("resolving image archive path: %w", err)
			}
			result[i+1] = absolute
			return result, nil
		}
		for _, prefix := range []string{short + "=", long + "=", "-" + strings.TrimPrefix(long, "--") + "="} {
			if !strings.HasPrefix(arg, prefix) {
				continue
			}
			path := strings.TrimPrefix(arg, prefix)
			if path == "" {
				return result, nil
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				return nil, fmt.Errorf("resolving image archive path: %w", err)
			}
			result[i] = prefix + absolute
			return result, nil
		}
	}
	return result, nil
}

func connectRunningDaemon(socket string) (net.Conn, error) {
	conn, err := connectDaemon(socket)
	if err != nil {
		return nil, fmt.Errorf("ttcd is not running; start it explicitly with 'ttc daemon start': %w", err)
	}
	return conn, nil
}

func connectDaemon(socket string) (net.Conn, error) {
	return net.DialTimeout("unix", socket, 250*time.Millisecond)
}

func exchangeDaemon(conn net.Conn, request daemonRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	header, err := json.Marshal(request)
	if err != nil {
		return 1, err
	}
	header = append(header, '\n')
	var writeMu sync.Mutex
	writeMu.Lock()
	_, err = conn.Write(header)
	writeMu.Unlock()
	if err != nil {
		return 1, err
	}
	stdinFlow := newDaemonStdinFlow()
	defer stdinFlow.close()
	if request.Stdin {
		go func() {
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := stdin.Read(buffer)
				if n != 0 {
					if err := stdinFlow.reserve(n); err != nil {
						return
					}
					if err := writeDaemonFrame(conn, &writeMu, daemonFrameStdin, buffer[:n]); err != nil {
						return
					}
				}
				if readErr != nil {
					_ = writeDaemonFrame(conn, &writeMu, daemonFrameEOF, nil)
					return
				}
			}
		}()
	}

	terminal := newDaemonClientTerminal(stdin, func(typ byte, payload []byte) error {
		return writeDaemonFrame(conn, &writeMu, typ, payload)
	})
	defer terminal.Close()

	reader := bufio.NewReader(conn)
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			return 1, err
		}
		switch typ {
		case daemonFrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStdinAck:
			bytes, err := decodeDaemonStdinAcknowledgement(payload)
			if err != nil {
				return 1, err
			}
			if err := stdinFlow.acknowledge(bytes); err != nil {
				return 1, err
			}
		case daemonFrameTerminal:
			if err := terminal.configure(payload); err != nil {
				return 1, err
			}
		case daemonFrameTTYState:
			if _, err := stdout.Write(append(payload, '\n')); err != nil {
				return 1, err
			}
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 1, fmt.Errorf("invalid daemon status frame length %d", len(payload))
			}
			return int(binary.BigEndian.Uint32(payload)), nil
		default:
			return 1, fmt.Errorf("unexpected daemon frame %q", typ)
		}
	}
}

func daemonLogPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "ttcd.log")
}

func startDaemonProcess(socket, runtimePlatform string, unsafeNoHostIsolation, allowSlowVolumeCopy bool, clusterOpts daemonClusterOptions) (string, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return "", nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("finding ttc executable: %w", err)
	}
	commandName := filepath.Join(filepath.Dir(executable), "ttcd")
	info, err := os.Stat(commandName)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return "", nil, fmt.Errorf("matching executable ttcd was not found beside %s", executable)
	}
	output, err := exec.Command(commandName, "--version").Output()
	if err != nil {
		return "", nil, fmt.Errorf("checking ttcd version: %w", err)
	}
	wantVersion := "ttcd version " + version.Version()
	if strings.TrimSpace(string(output)) != wantVersion {
		return "", nil, fmt.Errorf("ttc build %q does not match %s", version.Version(), strings.TrimSpace(string(output)))
	}
	commandArgs := []string{"--socket", socket, "--platform", runtimePlatform}
	if unsafeNoHostIsolation {
		commandArgs = append(commandArgs, "--unsafe-no-host-isolation")
	}
	if allowSlowVolumeCopy {
		commandArgs = append(commandArgs, "--allow-slow-volume-copy")
	}
	if clusterOpts.Configured {
		commandArgs = append(commandArgs,
			"--cluster-listen", clusterOpts.Listen,
			"--cluster-node-id", clusterOpts.NodeID,
			"--cluster-repository-id", clusterOpts.RepositoryID,
			"--cluster-cert", clusterOpts.CertFile,
			"--cluster-key", clusterOpts.KeyFile,
			"--cluster-ca", clusterOpts.CAFile,
			"--cluster-lease", clusterOpts.LeaseDuration.String(),
			"--cluster-max-transactions", strconv.Itoa(clusterOpts.MaxTransactions),
			"--cluster-page-listen", clusterOpts.PageListen,
			"--cluster-transfer-bytes", strconv.FormatUint(clusterOpts.PagingPolicy.TransferBytes, 10),
			"--cluster-max-outstanding-bytes", strconv.FormatUint(clusterOpts.PagingPolicy.MaxOutstandingBytes, 10),
			"--cluster-max-parallel", strconv.Itoa(clusterOpts.PagingPolicy.MaxConcurrency),
			"--cluster-ack-batch-pages", strconv.Itoa(clusterOpts.PagingPolicy.AckBatchPages),
			"--cluster-ack-interval", clusterOpts.PagingPolicy.AckInterval.String(),
			"--cluster-prefetch", string(clusterOpts.PagingPolicy.Prefetch),
			"--cluster-push", string(clusterOpts.PagingPolicy.Push),
		)
		if clusterOpts.Advertise != "" {
			commandArgs = append(commandArgs, "--cluster-advertise", clusterOpts.Advertise)
		}
		if clusterOpts.MemoryTarget != 0 {
			commandArgs = append(commandArgs, "--cluster-memory-target", strconv.FormatUint(clusterOpts.MemoryTarget, 10))
		}
		if clusterOpts.AutoRebalance {
			commandArgs = append(commandArgs, "--cluster-auto-rebalance")
		}
		if clusterOpts.CapacityCommand != "" {
			commandArgs = append(commandArgs, "--cluster-capacity-command", clusterOpts.CapacityCommand)
		}
		if clusterOpts.CapacityWebhook != "" {
			commandArgs = append(commandArgs, "--cluster-capacity-webhook", clusterOpts.CapacityWebhook)
		}
		if clusterOpts.PageAdvertise != "" {
			commandArgs = append(commandArgs, "--cluster-page-advertise", clusterOpts.PageAdvertise)
		}
		for _, peer := range clusterOpts.Peers {
			commandArgs = append(commandArgs, "--cluster-peer", peer)
		}
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer devNull.Close()
	logPath := daemonLogPath(socket)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("opening ttcd log %q: %w", logPath, err)
	}
	defer logFile.Close()
	command := exec.Command(commandName, commandArgs...)
	command.Stdin = devNull
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return logPath, nil, fmt.Errorf("starting ttcd: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	return logPath, done, nil
}

func reportDaemonStartupFailure(stderr io.Writer, logPath string, processErr error) {
	fmt.Fprintf(stderr, "ttc: ttcd did not become ready; startup log: %s\n", logPath)
	if processErr != nil {
		fmt.Fprintf(stderr, "ttc: ttcd exited: %v\n", processErr)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: reading startup log: %v\n", err)
		return
	}
	if diagnostic := strings.TrimSpace(string(log)); diagnostic != "" {
		fmt.Fprintln(stderr, diagnostic)
	}
}

func reportDaemonStartupWarnings(stderr io.Writer, logPath string) {
	log, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "disabling host GSO") {
			fmt.Fprintln(stderr, line)
		}
	}
}

func runDaemonControl(opts options, args []string, stdout, stderr io.Writer) int {
	if opts.unsafeNoHostIsolation && (len(args) == 0 || args[0] != "start") {
		fmt.Fprintln(stderr, "ttc: --unsafe-no-host-isolation is valid with 'daemon start' only")
		return 2
	}
	clusterOpts := defaultDaemonClusterOptions()
	if len(args) != 0 && args[0] == "start" {
		startArgs := args[1:]
		for len(startArgs) != 0 {
			switch {
			case startArgs[0] == "--platform":
				if len(startArgs) < 2 || startArgs[1] == "" {
					fmt.Fprintln(stderr, "ttc: --platform requires systrap or kvm")
					return 2
				}
				opts.runtimePlatform = startArgs[1]
				startArgs = startArgs[2:]
			case strings.HasPrefix(startArgs[0], "--platform="):
				opts.runtimePlatform = strings.TrimPrefix(startArgs[0], "--platform=")
				startArgs = startArgs[1:]
			case startArgs[0] == "--unsafe-no-host-isolation":
				opts.unsafeNoHostIsolation = true
				startArgs = startArgs[1:]
			case startArgs[0] == "--allow-slow-volume-copy":
				opts.allowSlowVolumeCopy = true
				startArgs = startArgs[1:]
			default:
				consumed, err := consumeDaemonClusterOption(startArgs, &clusterOpts)
				if err != nil {
					fmt.Fprintf(stderr, "ttc: %v\n", err)
					return 2
				}
				if consumed == 0 {
					fmt.Fprintf(stderr, "ttc: unknown daemon start option %q\n", startArgs[0])
					return 2
				}
				startArgs = startArgs[consumed:]
			}
		}
		runtimePlatform, err := normalizeRuntimePlatform(opts.runtimePlatform)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		opts.runtimePlatform = runtimePlatform
		args = args[:1]
	}
	if len(args) != 0 && (args[0] == "requests" || args[0] == "request") {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		defer conn.Close()
		status, err := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "mutation-control", Options: wireOptions(opts), Args: append([]string(nil), args...)}, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	}
	stopMode := ""
	if len(args) == 2 && args[0] == "stop" && (args[1] == "--dehydrate-all" || args[1] == "--stop-all") {
		stopMode = args[1]
		args = args[:1]
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "ttc: usage: ttc daemon start [--platform systrap|kvm] [isolation/volume/cluster options]|status|stop|requests|request inspect|request resolve")
		return 2
	}
	action := args[0]
	switch action {
	case "start":
		if conn, err := connectDaemon(opts.daemonSocket); err == nil {
			status, checkErr := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, stderr)
			conn.Close()
			if checkErr != nil || status != 0 {
				if checkErr != nil {
					fmt.Fprintf(stderr, "ttc: checking running daemon: %v\n", checkErr)
				}
				return 1
			}
			fmt.Fprintln(stdout, "ttcd already running; use 'ttc daemon status' to inspect its mode")
			return 0
		}
		if opts.unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttc: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
		}
		logPath, processDone, err := startDaemonProcess(opts.daemonSocket, opts.runtimePlatform, opts.unsafeNoHostIsolation, opts.allowSlowVolumeCopy, clusterOpts)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if conn, err := connectDaemon(opts.daemonSocket); err == nil {
				conn.Close()
				reportDaemonStartupWarnings(stderr, logPath)
				fmt.Fprintln(stdout, "ttcd started")
				return 0
			}
			select {
			case processErr := <-processDone:
				reportDaemonStartupFailure(stderr, logPath, processErr)
				return 1
			case <-time.After(25 * time.Millisecond):
			}
		}
		reportDaemonStartupFailure(stderr, logPath, nil)
		return 1
	case "status", "stop":
		conn, err := connectDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintln(stderr, "ttc: ttcd is not running")
			return 1
		}
		defer conn.Close()
		request := daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: map[string]string{"status": "ping", "stop": "stop"}[action]}
		if action == "stop" {
			// Stop intentionally omits the build ID so it works across releases.
			request.ClientBuildID = ""
			request.ManageStop = true
			if stopMode != "" {
				request.Args = []string{stopMode}
			}
		}
		status, err := exchangeDaemon(conn, request, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	default:
		fmt.Fprintf(stderr, "ttc: unknown daemon command %q\n", action)
		return 2
	}
}
