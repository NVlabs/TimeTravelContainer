// Copyright 2018 The gVisor Authors.
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
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	gsync "gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// DefaultBranchID is the branch used by existing, branch-unaware runsc RPCs.
const DefaultBranchID = "main"

// BranchState describes a live branch's lifecycle state.
type BranchState string

const (
	BranchCreating    BranchState = "creating"
	BranchPaused      BranchState = "paused"
	BranchRunning     BranchState = "running"
	BranchExited      BranchState = "exited"
	BranchDehydrating BranchState = "dehydrating"
	BranchDehydrated  BranchState = "dehydrated"
	BranchRehydrating BranchState = "rehydrating"
	BranchDestroying  BranchState = "destroying"
	BranchFailed      BranchState = "failed"
)

// BranchInfo is the branch metadata exposed over the control socket.
type BranchInfo struct {
	ID string
	// ParentID records which branch was forked to create this branch. It is
	// provenance for the CLI tree only; it imposes no ownership or lifecycle
	// relationship.
	ParentID         string
	FilesystemBranch string
	State            BranchState
	CreatedAt        time.Time
	InitPID          kernel.ThreadID
	InitState        string
	SharesMemory     bool
	Quarantined      bool
	// ForkMetrics is populated for branches created by Fork.
	ForkMetrics SandboxForkMetrics
}

// SandboxForkMetrics breaks total API latency into family and Kernel work.
type SandboxForkMetrics struct {
	SourcePause    time.Duration
	SourceDowntime time.Duration
	Kernel         kernel.LiveForkMetrics
	ChildStart     time.Duration
	ChildResume    time.Duration
	SourceResume   time.Duration
	Total          time.Duration
}

// ForkBranchArgs selects the source and destination of an in-memory fork.
type ForkBranchArgs struct {
	// Source defaults to DefaultBranchID.
	Source string
	// ID is the new branch unique identifier.
	ID string
	// Paused leaves the new branch stopped after creation.
	Paused bool
	// Quarantine cuts the new branch off from everything outside the sandbox.
	//
	// For a copy made to be asked a question and then destroyed. Its
	// connections are left up rather than reset, so nothing inside notices it
	// is a copy; what it writes is accepted and dropped and what it reads never
	// arrives, so nothing outside notices it exists.
	Quarantine        bool
	FilesystemOrigins []FilesystemOriginAssignment
}

// FilesystemOriginAssignment is the JSON/uRPC-safe form of one clustered
// persistent-filesystem origin plan.
type FilesystemOriginAssignment struct {
	ResourceID checkpoint.ResourceID `json:"resource_id"`
	Origins    []uint64              `json:"origins"`
}

func filesystemOriginPlan(assignments []FilesystemOriginAssignment) (vfs.LiveForkFilesystemOrigins, error) {
	if len(assignments) == 0 {
		return nil, nil
	}
	plan := make(vfs.LiveForkFilesystemOrigins, len(assignments))
	for _, assignment := range assignments {
		if !assignment.ResourceID.Ok() || len(assignment.Origins) == 0 {
			return nil, errors.New("filesystem origin assignment is empty")
		}
		if _, exists := plan[assignment.ResourceID]; exists {
			return nil, fmt.Errorf("filesystem origin assignment repeats %q", assignment.ResourceID)
		}
		origins := append([]uint64(nil), assignment.Origins...)
		for _, origin := range origins {
			if origin == 0 {
				return nil, fmt.Errorf("filesystem origin assignment for %q contains zero", assignment.ResourceID)
			}
		}
		plan[assignment.ResourceID] = origins
	}
	return plan, nil
}

// ForkBranchesArgs selects one source and multiple sibling destinations for a
// single coordinated in-memory fork.
type ForkBranchesArgs struct {
	// Source defaults to DefaultBranchID.
	Source string
	// IDs are the new, distinct branch identifiers.
	IDs []string
	// Paused leaves every new branch stopped after creation.
	Paused bool
	// Quarantine cuts every new branch off from outside the sandbox.
	Quarantine bool
	// FilesystemOrigins maps each persistent filesystem to
	// [newSource,newChild] pairs in IDs order.
	FilesystemOrigins []FilesystemOriginAssignment
}

// BranchQuarantineArgs changes the external-connectivity quarantine bit.
type BranchQuarantineArgs struct {
	ID      string
	Enabled bool
}

// DistributedForkCaptureArgs starts a remotely readable snapshot session.
// ListenAddress is a host TCP address accepted by net.Listen. AdvertiseAddress
// is returned to the peer and may differ when the listener is behind a routed
// or translated address; if empty, the listener's address is advertised.
type DistributedForkCaptureArgs struct {
	urpc.FilePayload
	SessionID        string
	Source           string
	FamilyID         string
	TargetBranchID   string
	ListenAddress    string
	AdvertiseAddress string
	MaxTransferBytes uint64
	ExternalListener bool
	PushPolicy       ttcremote.TCPPushPolicy
	PushHighWater    uint64
	// LeaveSourcePaused turns a fork capture into a migration cutover capture.
	// Errors still restore the source's original state.
	LeaveSourcePaused bool
	FilesystemOrigins []FilesystemOriginAssignment
}

// DistributedForkCaptureChildArgs identifies one target in a coordinated
// sibling capture. ttcd owns the advertised listener for every child.
type DistributedForkCaptureChildArgs struct {
	SessionID        string
	TargetBranchID   string
	AdvertiseAddress string
}

// DistributedForkCaptureManyArgs starts independent page sessions backed by
// one paused Kernel capture and one set of source page references.
type DistributedForkCaptureManyArgs struct {
	Source            string
	FamilyID          string
	Children          []DistributedForkCaptureChildArgs
	MaxTransferBytes  uint64
	PushPolicy        ttcremote.TCPPushPolicy
	PushHighWater     uint64
	FilesystemOrigins []FilesystemOriginAssignment
}

// DistributedForkSessionInfo is safe to return through the runsc control
// socket. Token is a bearer capability and must never appear in status or log
// output after initial creation.
type DistributedForkSessionInfo struct {
	SessionID        string
	Source           string
	FilesystemBranch string
	Address          string
	Token            ttcremote.SnapshotToken
	MetadataBytes    uint64
	StreamBytes      uint64
	ExecSessions     []ExecSessionInfo
}

// DistributedForkSessionArgs identifies an existing source snapshot.
type DistributedForkSessionArgs struct {
	SessionID string
}

// DistributedForkServeArgs donates one host-accepted TCP connection.
type DistributedForkServeArgs struct {
	urpc.FilePayload
	SessionID string
}

// DistributedForkExportArgs donates a destination for a graph capture stream.
type DistributedForkExportArgs struct {
	urpc.FilePayload
	SessionID string
	// FilteredGraphStream says the stream is two-way and the daemon answers
	// the manifest with the pages its graph lacks; see
	// ttcchunk.WriteFilteredGraphCapture. A Sentry predating it ignores it and
	// sends every page, which such a daemon still accepts.
	FilteredGraphStream bool
}

// DistributedForkExportInfo identifies the filesystem branch captured at the
// same instant as a distributed memory export.
type DistributedForkExportInfo struct {
	FilesystemBranch string
}

// DistributedForkSessionStatus reports token-free session progress.
type DistributedForkSessionStatus struct {
	SessionID string
	Source    string
	Address   string
	Transport ttcremote.TCPPageServerStats
	Snapshot  pgalloc.RemoteSnapshotStats
}

// DistributedForkDependencyArgs selects a target branch's remote loader.
type DistributedForkDependencyArgs struct {
	BranchID            string
	PrefetchBytes       uint64
	PrefetchBehindBytes uint64
	// IncludeDurable requests consumption progress for immutable graph-backed
	// readers as well as volatile source-node readers.
	IncludeDurable bool
}

// DistributedForkHandoffArgs replaces a target's live network page source
// with an immutable snapshot-graph manifest. FilePayload contains the
// manifest and object-store descriptors, followed by an optional relay.
type DistributedForkHandoffArgs struct {
	urpc.FilePayload
	BranchID         string
	GraphSnapshot    ttcsnapshot.SnapshotID
	GraphRemote      bool
	GraphRemoteToken ttcsnapshot.ObjectRelayToken
}

// BranchArgs selects an existing branch. An empty ID selects the default.
type BranchArgs struct {
	ID string
}

// BranchRenameArgs names a branch and the name it should answer to instead.
type BranchRenameArgs struct {
	ID    string
	NewID string
}

// BranchDehydrateArgs selects a branch and donates its durable metadata file.
type BranchDehydrateArgs struct {
	urpc.FilePayload
	ID string
}

// BranchDehydrationMemoryInfo describes one logical MemoryFile included in a
// graph capture before a fully dehydrated family can release its Sentry.
type BranchDehydrationMemoryInfo struct {
	ResourceID  checkpoint.ResourceID `json:"resource_id"`
	StreamBytes uint64                `json:"stream_bytes"`
}

// BranchDehydrateInfo is returned after durable Kernel metadata is published.
type BranchDehydrateInfo struct {
	Memories         []BranchDehydrationMemoryInfo `json:"memories"`
	Resume           bool                          `json:"resume"`
	ExecSessions     []ExecSessionInfo             `json:"exec_sessions,omitempty"`
	FilesystemBranch string                        `json:"filesystem_branch"`
	SnapshotID       uint64                        `json:"snapshot_id,omitempty"`
	MemoryRoots      []BranchMemoryRoot            `json:"memory_roots,omitempty"`
}

// BranchMemoryRoot binds one captured MemoryFile to an ancestry-independent
// snapshot-graph radix root.
type BranchMemoryRoot struct {
	ResourceID checkpoint.ResourceID `json:"resource_id"`
	Name       string                `json:"name"`
	Root       string                `json:"root"`
}

// BranchDehydrationExportArgs donates one graph capture stream destination.
type BranchDehydrationExportArgs struct {
	urpc.FilePayload
	ID string
	// FilteredGraphStream is as for DistributedForkExportArgs.
	FilteredGraphStream bool
}

// BranchDehydrationExportInfo describes the capture an export published, so
// that a daemon exporting a capture long after taking it need not have kept
// the description it got then.
type BranchDehydrationExportInfo struct {
	Capture BranchDehydrateInfo `json:"capture"`
}

// BranchProcessesArgs selects a container inside one live branch.
type BranchProcessesArgs struct {
	BranchID string
	CID      string
}

// BranchRootfsSizeArgs selects a container rootfs inside one live branch.
type BranchRootfsSizeArgs struct {
	BranchID string
	CID      string
}

// BranchResidentMemory is resident guest memory attributed to one live branch.
// Private and shared classify physical Sentry pages across this family.
type BranchResidentMemory struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	PrivateBytes uint64 `json:"private_bytes"`
	SharedBytes  uint64 `json:"shared_bytes"`
	TotalBytes   uint64 `json:"total_bytes"`
}

// FamilyResidentMemory is a family-wide resident-memory accounting snapshot.
type FamilyResidentMemory struct {
	Branches                 []BranchResidentMemory       `json:"branches"`
	SharingEdges             []ResidentMemorySharingEdge  `json:"sharing_edges,omitempty"`
	VersionRuns              []ResidentMemoryVersionRun   `json:"version_runs,omitempty"`
	VersionChunks            []ResidentMemoryVersionChunk `json:"version_chunks,omitempty"`
	SharingEdgesTruncated    bool                         `json:"sharing_edges_truncated,omitempty"`
	SharingEdgesOmittedBytes uint64                       `json:"sharing_edges_omitted_bytes,omitempty"`
	UniqueBytes              uint64                       `json:"unique_bytes"`
}

// ResidentMemoryVersionRun is a compressed stable identity range for resident
// mapped pages in one branch. Imported and remotely forked pages retain these
// identities, allowing a cluster planner to match CoW sharing across Sentries
// without exporting one control record per 4 KiB page.
type ResidentMemoryVersionRun struct {
	Branch        string `json:"branch"`
	Allocator     string `json:"allocator"`
	FirstSequence uint64 `json:"first_sequence"`
	Pages         uint64 `json:"pages"`
}

// ResidentMemoryVersionChunk is a bounded bitmap of stable content-version
// identities. Bitmap is 256 bits encoded as 64 lowercase hexadecimal digits;
// bit N represents version Chunk*256+N. Unlike contiguous runs, this remains
// compact when concurrent sibling CoW faults interleave allocator sequences.
type ResidentMemoryVersionChunk struct {
	Branch    string `json:"branch"`
	Allocator string `json:"allocator"`
	Chunk     uint64 `json:"chunk"`
	Bitmap    string `json:"bitmap"`
}

// ResidentMemorySharingEdge is an exact weighted hyperedge. It coalesces all
// resident physical pages referenced by the same set of branches; this is
// algebraically equivalent to exporting one 4 KiB edge per page for placement
// cost calculations, without producing a page-sized control response.
type ResidentMemorySharingEdge struct {
	ID       string   `json:"id"`
	Bytes    uint64   `json:"bytes"`
	Branches []string `json:"branches"`
}

const maxResidentMemorySharingEdges = 4096

// BranchWaitArgs selects a process inside one live branch. PID 0 selects the
// container init process.
type BranchWaitArgs struct {
	BranchID string
	CID      string
	PID      int32
}

// ConsolePushArgs selects a process whose console the Sentry pushes to the one
// file given: see PushConsole.
type ConsolePushArgs struct {
	urpc.FilePayload
	BranchID  string
	CID       string
	ConsoleID string
	ExecID    string
}

// A console push ends with one record in the console frame format, of a
// stream past stdout's and stderr's.
const (
	// ConsolePushExit carries the process's wait status, big-endian.
	ConsolePushExit byte = 3
	// ConsolePushError carries why the push ended without it.
	ConsolePushError byte = 4
	// ConsolePushOOMKilled, before ConsolePushExit, says the process was
	// killed to keep its family within its memory limit (see
	// kernel.SetMemoryLimit), as Linux's out-of-memory killer kills one.
	ConsolePushOOMKilled byte = 5
)

// BranchSignalArgs selects signal recipients inside one live branch.
type BranchSignalArgs struct {
	BranchID string
	CID      string
	Signo    int32
	PID      int32
	Mode     SignalDeliveryMode
}

// ConsoleReadArgs selects one output stream of a branch-local internal console.
type ConsoleReadArgs struct {
	BranchID  string
	ConsoleID string
	Stream    kernel.InternalConsoleStream
	MaxBytes  int
}

// ConsoleReadResult is a nonblocking internal-console read result.
type ConsoleReadResult struct {
	Data       []byte
	EOF        bool
	WouldBlock bool
}

// ConsoleObservationArgs selects a branch-local internal console without
// consuming its streams.
type ConsoleObservationArgs struct {
	BranchID  string
	ConsoleID string
}

// ConsoleWatchArgs streams terminal observations to the single file in
// FilePayload. The Sentry emits an initial record, coalesced change records,
// and heartbeats without client-side polling.
type ConsoleWatchArgs struct {
	urpc.FilePayload
	ConsoleObservationArgs
}

// MaxConsoleWait is the longest timeout ConsoleWait accepts.
const MaxConsoleWait = 30 * time.Second

// ConsoleWaitArgs waits for branch-local console I/O readiness. A zero timeout
// performs a nonblocking readiness check.
type ConsoleWaitArgs struct {
	BranchID  string
	ConsoleID string
	Want      kernel.InternalConsoleReadiness
	Timeout   time.Duration
}

// ConsoleWaitResult reports which requested operations are ready. TimedOut is
// true when no operation became ready before the requested timeout.
type ConsoleWaitResult struct {
	Ready    kernel.InternalConsoleReadiness
	TimedOut bool
}

// ConsoleOutputArgs selects a branch-local internal console whose stdout and
// stderr the Sentry copies to the two files given: see CopyConsoleOutput.
type ConsoleOutputArgs struct {
	urpc.FilePayload
	BranchID  string
	ConsoleID string
}

// ConsoleInputArgs selects a branch-local internal console whose stdin the
// Sentry copies the one file given to: see PullConsoleInput.
type ConsoleInputArgs struct {
	urpc.FilePayload
	BranchID  string
	ConsoleID string
}

// ConsoleWriteArgs writes to a branch-local internal console's stdin.
type ConsoleWriteArgs struct {
	BranchID  string
	ConsoleID string
	Data      []byte
}

// ConsoleWriteResult is a nonblocking internal-console write result.
type ConsoleWriteResult struct {
	N          int
	WouldBlock bool
}

// ConsoleCloseStdinArgs closes a branch-local internal console's stdin.
type ConsoleCloseStdinArgs struct {
	BranchID  string
	ConsoleID string
}

// ConsoleResizeArgs changes a branch-local PTY console's window size.
type ConsoleResizeArgs struct {
	BranchID  string
	ConsoleID string
	Rows      uint16
	Cols      uint16
	XPixels   uint16
	YPixels   uint16
}

// ConsoleRemoveArgs unregisters a transient branch-local internal console.
type ConsoleRemoveArgs struct {
	BranchID  string
	ConsoleID string
}

// ExecSessionState is the lifecycle state of one branch-local exec session.
type ExecSessionState string

const (
	ExecSessionCreating ExecSessionState = "creating"
	ExecSessionRunning  ExecSessionState = "running"
	ExecSessionExited   ExecSessionState = "exited"
)

// ExecSessionInfo is the stable control-plane identity of an exec process.
// IDs are scoped by branch; a live fork preserves the ID and virtual PID.
type ExecSessionInfo struct {
	ID          string           `json:"id"`
	ContainerID string           `json:"container_id"`
	PID         kernel.ThreadID  `json:"pid"`
	ConsoleID   string           `json:"console_id"`
	Terminal    bool             `json:"terminal"`
	State       ExecSessionState `json:"state"`
	CreatedAt   time.Time        `json:"created_at"`
	ExitStatus  *uint32          `json:"exit_status,omitempty"`
}

// ExecSessionsArgs selects exec sessions in one branch.
type ExecSessionsArgs struct {
	BranchID string
	CID      string
}

// ExecSessionArgs selects one exec session in one branch.
type ExecSessionArgs struct {
	BranchID string
	ExecID   string
	Signo    int32
}

// ExecWaitArgs waits for an exec session. FilePayload contains one liveness
// reader whose peer remains open for the lifetime of the low-level client.
type ExecWaitArgs struct {
	urpc.FilePayload
	ExecSessionArgs
}

// ErrExecWaitCancelled reports that the low-level wait client disappeared.
var ErrExecWaitCancelled = errors.New("exec wait client disconnected")

type execSession struct {
	info           ExecSessionInfo
	tg             *kernel.ThreadGroup
	managementDone <-chan struct{}
	retain         bool
}

type consoleWaitLease struct {
	cancel chan struct{}
	done   chan struct{}
}

type sandboxBranch struct {
	// Control leases may be released by the goroutine that waits for an exec
	// or console operation rather than the goroutine that created the lease.
	opMu gsync.CrossGoroutineMutex
	// controlOps holds Kernel lifetime while branch-local controls are active.
	// Destroy marks the branch destroying, terminates its tasks, then waits for
	// these operations before releasing the Kernel.
	controlOps sync.WaitGroup
	// compactionOps is the subset of controlOps that may access application
	// memory. Persistent process/console waiters hold Kernel lifetime without
	// blocking a stop-the-world GuestRAM compaction.
	compactionOps sync.WaitGroup

	id string
	// parentID is display-only fork provenance, never an ownership edge. When
	// this branch is removed, direct children are spliced to its parent.
	parentID string
	// filesystemBranch is the persistent ttcvol branch backing this Kernel.
	// It normally follows volumeBranchID(id), but a distributed target exposes
	// its restored branch as visible "main" while retaining the source-family
	// repository branch name.
	filesystemBranch string
	k                *kernel.Kernel
	watchdog         *watchdog.Watchdog
	state            BranchState
	createdAt        time.Time
	metrics          SandboxForkMetrics
	quarantined      bool

	execSessions map[string]*execSession
	consoleWaits map[*consoleWaitLease]struct{}
	dehydration  *branchDehydration
}

type branchDehydration struct {
	image        *kernel.DehydrationCapture
	platform     platform.Platform
	resume       bool
	execSessions []ExecSessionInfo
}

// SandboxFamily owns all in-memory branches in one Sentry process. Branches
// are lifecycle peers. loaderBranchID only identifies the Kernel reachable
// through Loader.k; parentID is mutable display provenance and confers no
// ownership, ordering, or storage relationship.
type SandboxFamily struct {
	mu sync.Mutex
	// dehydrationMu serializes capture ownership changes across sibling
	// branches so collective page-cache eviction sees a stable reservation set.
	dehydrationMu sync.Mutex
	stateChanged  *sync.Cond

	branches         map[string]*sandboxBranch
	loaderBranchID   string
	controlsQuiesced bool
	// defaultStarted is closed after Loader.Run has installed and started the
	// default branch's init thread group. It allows wait RPCs to be established
	// while the OCI container is still in the created state, eliminating the
	// race between a short-lived init and a post-start waiter.
	defaultStarted     chan struct{}
	defaultStartedOnce sync.Once
	// allowNetworkStack is true only when loader configuration proves that
	// network state has no host-facing links.
	liveForkEligible bool
	// cudaCheckpointForFork is, for --nvproxy-fork=snapshot, the path in
	// containers of the cuda-checkpoint tool with which a fork checkpoints the
	// processes that use NVIDIA GPUs.
	cudaCheckpointForFork          string
	allowNetworkStack              bool
	allowSharedFilesystemResources bool
	watchdogOpts                   *watchdog.Opts
	testOnlyForkFailureInjector    func(SandboxForkStage) error
	testOnlyExecFailureInjector    func(SandboxExecStage) error
	volumeBranchScope              string
	distributedSessions            map[string]*distributedForkSession
	distributedHandoffs            map[string]*distributedForkHandoff
	configureNetwork               func(*kernel.Kernel, inet.Stack) error
	onLoaderKernelChanged          func(*kernel.Kernel, *watchdog.Watchdog)
	forgetFilesystemBranch         func(string)
	discardFilesystemBranch        func(string)
	reclaimFilesystemBranch        func(string) error
	holdsTransactionSources        func() bool
	// clockMonotonicRaw is whether the clocks of new branches' Kernels
	// track host CLOCK_MONOTONIC_RAW, as the loader's do.
	clockMonotonicRaw bool
}

type distributedForkSession struct {
	mu               sync.Mutex
	source           string
	address          string
	filesystemBranch string
	resources        vfs.LiveForkFilesystemResources
	server           *ttcremote.TCPPageServer
	sparseSnapshot   *pgalloc.RemoteSnapshot
	memoryBindings   []pgalloc.CapturedPageBinding
	filesystemToken  ttcvolume.RemoteToken

	// sourceFilesystemBranch is the repository branch of source when it was
	// captured, whose diff base a migration's target receives.
	sourceFilesystemBranch string
}

type distributedForkHandoff struct {
	snapshot uint64
	done     chan struct{}
	err      error
}

// DistributedForkHandoffStatus reports completion of an asynchronous reader
// switch. Starting the switch must not occupy the Sentry control server: the
// old reader may have in-flight demand faults, while unrelated branch and exec
// controls must remain available.
type DistributedForkHandoffStatus struct {
	Snapshot uint64 `json:"snapshot"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// SandboxFamilyOptions controls eligibility policy shared by all branches.
type SandboxFamilyOptions struct {
	LiveForkEligible bool
	// CudaCheckpointForFork is, for --nvproxy-fork=snapshot, the path in
	// containers of the cuda-checkpoint tool with which a fork checkpoints the
	// processes that use NVIDIA GPUs.
	CudaCheckpointForFork          string
	AllowNetworkStack              bool
	AllowSharedFilesystemResources bool
	WatchdogOpts                   *watchdog.Opts
	// LoaderWatchdog is the watchdog already owned by Loader for k. Recording
	// it on the branch lets dehydration stop and replace the same watchdog
	// without giving that branch any lineage significance.
	LoaderWatchdog *watchdog.Watchdog
	// VolumeBranchScope distinguishes repository branch names created by this
	// sandbox from identically named live branches in other sandboxes.
	VolumeBranchScope     string
	ConfigureNetwork      func(*kernel.Kernel, inet.Stack) error
	OnLoaderKernelChanged func(*kernel.Kernel, *watchdog.Watchdog)
	// ForgetFilesystemBranch releases process-local TTC repository caches once
	// a branch becomes process-free but remains a live family member.
	ForgetFilesystemBranch func(string)
	// DiscardFilesystemBranch declares, before a branch's Kernel is released,
	// that ReclaimFilesystemBranch will follow: the filesystem's teardown need
	// not make the branch's writes durable.
	DiscardFilesystemBranch func(string)
	// ReclaimFilesystemBranch removes the persistent filesystem root after a
	// branch Kernel and all of its open file descriptions have been released.
	// It also performs runtime-side graph collection when this is the only
	// Sentry using the repository.
	ReclaimFilesystemBranch func(string) error
	// HoldsTransactionSources reports whether the Sentry's repositories still
	// read a remote fork's import from its provider: see WaitDefaultExit.
	HoldsTransactionSources func() bool
	// ClockMonotonicRaw is whether the clocks of new branches' Kernels track
	// host CLOCK_MONOTONIC_RAW, as the loader's do.
	ClockMonotonicRaw bool
	// InitialBranchID and InitialFilesystemBranch identify the branch represented
	// by the Loader's bootstrap Kernel. Empty values retain the canonical main
	// defaults used by ordinary OCI creation.
	InitialBranchID         string
	InitialBranchParentID   string
	InitialFilesystemBranch string
	// TestOnlyForkFailureInjector verifies family-level transaction cleanup.
	// Production callers should leave it nil.
	TestOnlyForkFailureInjector func(SandboxForkStage) error
	// TestOnlyExecFailureInjector verifies branch exec transaction cleanup.
	// Production callers should leave it nil.
	TestOnlyExecFailureInjector func(SandboxExecStage) error
}

// SandboxExecStage identifies a completed branch exec transaction stage.
type SandboxExecStage string

const (
	SandboxExecStageNamespaceSelected SandboxExecStage = "namespace-selected"
	SandboxExecStageSessionReserved   SandboxExecStage = "session-reserved"
	SandboxExecStageConsoleCreated    SandboxExecStage = "console-created"
	SandboxExecStageFilesImported     SandboxExecStage = "files-imported"
	SandboxExecStageProcessCreated    SandboxExecStage = "process-created"
	SandboxExecStageSessionPublished  SandboxExecStage = "session-published"
)

// SandboxForkStage identifies a completed family-level fork stage.
type SandboxForkStage string

const (
	SandboxForkStagePlaceholderRegistered SandboxForkStage = "placeholder-registered"
	SandboxForkStageSourcePaused          SandboxForkStage = "source-paused"
	SandboxForkStageKernelCloned          SandboxForkStage = "kernel-cloned"
	SandboxForkStageChildStarted          SandboxForkStage = "child-started"
	SandboxForkStageBranchReady           SandboxForkStage = "branch-ready"
)

// NewSandboxFamily creates a family containing k as its default branch.
func NewSandboxFamily(k *kernel.Kernel, opts SandboxFamilyOptions) *SandboxFamily {
	createdAt := time.Now().UTC()
	initialID := opts.InitialBranchID
	if initialID == "" {
		initialID = DefaultBranchID
	}
	initialFilesystemBranch := opts.InitialFilesystemBranch
	if initialFilesystemBranch == "" {
		initialFilesystemBranch = ttcvolume.MainBranch
	}
	f := &SandboxFamily{
		liveForkEligible:               opts.LiveForkEligible,
		cudaCheckpointForFork:          opts.CudaCheckpointForFork,
		allowNetworkStack:              opts.AllowNetworkStack,
		allowSharedFilesystemResources: opts.AllowSharedFilesystemResources,
		watchdogOpts:                   opts.WatchdogOpts,
		testOnlyForkFailureInjector:    opts.TestOnlyForkFailureInjector,
		testOnlyExecFailureInjector:    opts.TestOnlyExecFailureInjector,
		volumeBranchScope:              opts.VolumeBranchScope,
		configureNetwork:               opts.ConfigureNetwork,
		onLoaderKernelChanged:          opts.OnLoaderKernelChanged,
		forgetFilesystemBranch:         opts.ForgetFilesystemBranch,
		discardFilesystemBranch:        opts.DiscardFilesystemBranch,
		reclaimFilesystemBranch:        opts.ReclaimFilesystemBranch,
		clockMonotonicRaw:              opts.ClockMonotonicRaw,
		holdsTransactionSources:        opts.HoldsTransactionSources,
		defaultStarted:                 make(chan struct{}),
		distributedSessions:            make(map[string]*distributedForkSession),
		distributedHandoffs:            make(map[string]*distributedForkHandoff),
		branches: map[string]*sandboxBranch{
			initialID: {
				id:               initialID,
				parentID:         opts.InitialBranchParentID,
				filesystemBranch: initialFilesystemBranch,
				k:                k,
				watchdog:         opts.LoaderWatchdog,
				state:            kernelBranchState(k),
				createdAt:        createdAt,
				quarantined:      k.LiveForkQuarantine(),
				execSessions:     make(map[string]*execSession),
			},
		},
		loaderBranchID: initialID,
	}
	f.stateChanged = sync.NewCond(&f.mu)
	return f
}

// LoaderBranchID returns the visible branch currently referenced by Loader.k.
// This is a runtime bookkeeping role, not a lineage root: all branches remain
// peers and the recorded ParentID is display-only creation history.
func (f *SandboxFamily) LoaderBranchID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaderBranchID
}

// CaptureDistributedFork atomically captures a paused instant and publishes
// it through a bounded TCP page server. The source is restored to its original
// pause state before this method returns.
func (f *SandboxFamily) CaptureDistributedFork(args *DistributedForkCaptureArgs) (*DistributedForkSessionInfo, error) {
	if args == nil || args.SessionID == "" || args.FamilyID == "" || args.TargetBranchID == "" || (!args.ExternalListener && args.ListenAddress == "" && len(args.FilePayload.Files) == 0) {
		return nil, errors.New("distributed fork capture is missing session, family, branch, or listen identity")
	}
	if args.ExternalListener && (args.AdvertiseAddress == "" || args.ListenAddress != "" || len(args.FilePayload.Files) != 0) {
		return nil, errors.New("external distributed listener requires only an advertise address")
	}
	if len(args.FilePayload.Files) > 1 {
		return nil, fmt.Errorf("distributed fork capture received %d listener files, want at most one", len(args.FilePayload.Files))
	}
	var listener net.Listener
	var err error
	if !args.ExternalListener {
		if len(args.FilePayload.Files) == 1 {
			listener, err = net.FileListener(args.FilePayload.Files[0])
		} else {
			listener, err = net.Listen("tcp", args.ListenAddress)
		}
		if err != nil {
			return nil, fmt.Errorf("listening for distributed pages: %w", err)
		}
		if _, ok := listener.(*net.TCPListener); !ok {
			listener.Close()
			return nil, fmt.Errorf("distributed page listener is %T, want TCP", listener)
		}
	}
	children := []DistributedForkCaptureChildArgs{{
		SessionID: args.SessionID, TargetBranchID: args.TargetBranchID,
		AdvertiseAddress: args.AdvertiseAddress,
	}}
	infos, err := f.captureDistributedForkMany(args.Source, args.FamilyID, children, args.MaxTransferBytes, args.PushPolicy, args.PushHighWater, listener, args.LeaveSourcePaused, args.FilesystemOrigins)
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	return &infos[0], nil
}

// CaptureDistributedForkMany creates independent remote page sessions and
// filesystem branches from one source pause and one Kernel metadata stream.
func (f *SandboxFamily) CaptureDistributedForkMany(args *DistributedForkCaptureManyArgs) ([]DistributedForkSessionInfo, error) {
	if args == nil {
		return nil, errors.New("missing distributed fork batch arguments")
	}
	return f.captureDistributedForkMany(args.Source, args.FamilyID, args.Children, args.MaxTransferBytes, args.PushPolicy, args.PushHighWater, nil, false, args.FilesystemOrigins)
}

func (f *SandboxFamily) captureDistributedForkMany(sourceID, familyID string, children []DistributedForkCaptureChildArgs, maxTransferBytes uint64, pushPolicy ttcremote.TCPPushPolicy, pushHighWater uint64, listener net.Listener, leaveSourcePaused bool, filesystemAssignments []FilesystemOriginAssignment) ([]DistributedForkSessionInfo, error) {
	if familyID == "" || len(children) == 0 || maxTransferBytes == 0 {
		return nil, errors.New("distributed fork capture is missing family, children, or transfer limit")
	}
	if len(children) != 1 && listener != nil {
		return nil, errors.New("a shared listener is valid only for a singular distributed capture")
	}
	filesystemOrigins, err := filesystemOriginPlan(filesystemAssignments)
	if err != nil {
		return nil, err
	}
	seenSessions := make(map[string]struct{}, len(children))
	seenBranches := make(map[string]struct{}, len(children))
	for i, child := range children {
		if child.SessionID == "" || child.TargetBranchID == "" || (listener == nil && child.AdvertiseAddress == "") {
			return nil, fmt.Errorf("distributed fork child %d is missing session, branch, or advertise identity", i)
		}
		if _, ok := seenSessions[child.SessionID]; ok {
			return nil, fmt.Errorf("distributed fork batch repeats session %q", child.SessionID)
		}
		if _, ok := seenBranches[child.TargetBranchID]; ok {
			return nil, fmt.Errorf("distributed fork batch repeats target branch %q", child.TargetBranchID)
		}
		seenSessions[child.SessionID] = struct{}{}
		seenBranches[child.TargetBranchID] = struct{}{}
	}
	f.mu.Lock()
	if f.distributedSessions == nil {
		f.distributedSessions = make(map[string]*distributedForkSession)
	}
	for _, child := range children {
		if _, ok := f.distributedSessions[child.SessionID]; ok {
			f.mu.Unlock()
			return nil, fmt.Errorf("distributed fork session %q already exists", child.SessionID)
		}
	}
	f.mu.Unlock()
	if sourceID == "" {
		sourceID = f.LoaderBranchID()
	}
	source, err := f.branch(sourceID)
	if err != nil {
		return nil, err
	}
	source.opMu.Lock()
	defer source.opMu.Unlock()
	f.mu.Lock()
	if source.state != BranchRunning && source.state != BranchPaused {
		state := source.state
		f.mu.Unlock()
		return nil, fmt.Errorf("source branch %q is %s", sourceID, state)
	}
	sourceWasPaused := source.k.IsPaused()
	f.mu.Unlock()
	f.cancelConsoleWaits(source)
	if !sourceWasPaused {
		source.k.Pause()
	}
	f.mu.Lock()
	execSessions := make([]ExecSessionInfo, 0, len(source.execSessions))
	for _, session := range source.execSessions {
		execSessions = append(execSessions, cloneExecSessionInfo(session.info))
	}
	f.mu.Unlock()
	sort.Slice(execSessions, func(i, j int) bool { return execSessions[i].ID < execSessions[j].ID })
	resume := func() error {
		if sourceWasPaused {
			return nil
		}
		source.k.Unpause()
		return control.PostResume(source.k, nil)
	}
	volumeBranches := make([]string, len(children))
	for i := range children {
		volumeBranches[i] = f.volumeBranchID(children[i].TargetBranchID)
	}
	capture, err := source.k.CaptureDistributedFork(source.k.SupervisorContext(), kernel.DistributedForkCaptureOptions{
		ChildBranchIDs:                 volumeBranches,
		AllowSharedFilesystemResources: f.allowSharedFilesystemResources,
		FilesystemOrigins:              filesystemOrigins,
	})
	if err != nil {
		if resumeErr := resume(); resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return nil, err
	}
	rollbackFilesystem := func() error {
		var result error
		for _, branch := range volumeBranches {
			result = errors.Join(result, removeDistributedFilesystemBranch(capture.FilesystemResources, branch))
		}
		return result
	}
	committed := false
	var servers []*ttcremote.TCPPageServer
	sessions := make([]*distributedForkSession, len(children))
	defer func() {
		if committed {
			return
		}
		for _, server := range servers {
			_ = server.Close()
		}
		for _, session := range sessions {
			if session != nil && session.sparseSnapshot != nil {
				_ = session.sparseSnapshot.Close()
				session.sparseSnapshot = nil
			}
		}
		_ = capture.Close()
		_ = rollbackFilesystem()
	}()
	snapshots := capture.Snapshots
	if capture.Snapshot != nil {
		snapshots = []*pgalloc.RemoteSnapshot{capture.Snapshot}
	}
	if len(snapshots) != len(children) {
		_ = resume()
		return nil, fmt.Errorf("distributed capture returned %d snapshots for %d children", len(snapshots), len(children))
	}
	handoffSnapshots := capture.HandoffSnapshots
	if capture.HandoffSnapshot != nil {
		handoffSnapshots = []*pgalloc.RemoteSnapshot{capture.HandoffSnapshot}
	}
	if len(handoffSnapshots) != len(children) {
		_ = resume()
		return nil, fmt.Errorf("distributed capture returned %d handoff snapshots for %d children", len(handoffSnapshots), len(children))
	}
	infos := make([]DistributedForkSessionInfo, len(children))
	for i, child := range children {
		metadata, err := (&ttcremote.SessionMetadata{
			FamilyID: familyID, SourceBranchID: sourceID, TargetBranchID: child.TargetBranchID,
			KernelMetadata: capture.KernelMetadata, AllocatorMetadata: capture.AllocatorMetadata,
			LoadRanges: capture.LoadRanges, MemoryBindings: capture.MemoryBindings,
		}).MarshalBinary()
		if err != nil {
			_ = resume()
			return nil, fmt.Errorf("encoding distributed fork child %q metadata: %w", child.TargetBranchID, err)
		}
		var token ttcremote.SnapshotToken
		if _, err := rand.Read(token[:]); err != nil {
			_ = resume()
			return nil, fmt.Errorf("generating distributed snapshot token: %w", err)
		}
		var server *ttcremote.TCPPageServer
		if listener != nil {
			server, err = ttcremote.NewTCPPageServer(listener, token, snapshots[i], maxTransferBytes)
		} else {
			server, err = ttcremote.NewTCPPageSession(token, snapshots[i], maxTransferBytes)
		}
		if err != nil {
			_ = resume()
			return nil, err
		}
		servers = append(servers, server)
		if capture.Snapshot != nil {
			capture.Snapshot = nil
		} else {
			capture.Snapshots[i] = nil
		}
		if err := server.SetMetadata(metadata); err != nil {
			_ = resume()
			return nil, err
		}
		if pushPolicy != "" && pushPolicy != ttcremote.TCPPushDemandOnly {
			if err := server.ConfigurePush(ttcremote.TCPPushSenderOpts{
				MaxPush: maxTransferBytes, Timeout: 30 * time.Second, RetryLimit: 2,
				RetryBackoff: 10 * time.Millisecond, Policy: pushPolicy,
				RetainedHighWater: pushHighWater, ScanInterval: 10 * time.Millisecond,
				AtomicLogicalChunks: true,
			}); err != nil {
				_ = resume()
				return nil, fmt.Errorf("configuring distributed reverse push: %w", err)
			}
		}
		address := child.AdvertiseAddress
		if listener != nil {
			address = listener.Addr().String()
		}
		sessions[i] = &distributedForkSession{
			source: sourceID, address: address, filesystemBranch: volumeBranches[i],
			resources: capture.FilesystemResources, server: server,
			sparseSnapshot: handoffSnapshots[i], memoryBindings: capture.MemoryBindings,
			sourceFilesystemBranch: source.filesystemBranch,
		}
		copy(sessions[i].filesystemToken[:], token[:])
		if capture.HandoffSnapshot != nil {
			capture.HandoffSnapshot = nil
		} else {
			capture.HandoffSnapshots[i] = nil
		}
		infos[i] = DistributedForkSessionInfo{
			SessionID: child.SessionID, Source: sourceID, Address: address, Token: token,
			FilesystemBranch: volumeBranches[i],
			MetadataBytes:    uint64(len(metadata)), StreamBytes: server.StreamBytes(),
			ExecSessions: cloneExecSessionInfos(execSessions),
		}
	}
	// The live and durable branch-scoped reservations have transferred to their
	// servers/sessions. Close the extra direct-publication reservations.
	if err := capture.Close(); err != nil {
		_ = resume()
		return nil, fmt.Errorf("closing unused distributed capture reservations: %w", err)
	}
	f.mu.Lock()
	for _, child := range children {
		if _, ok := f.distributedSessions[child.SessionID]; ok {
			f.mu.Unlock()
			_ = resume()
			return nil, fmt.Errorf("distributed fork session %q was created concurrently", child.SessionID)
		}
	}
	for i, child := range children {
		f.distributedSessions[child.SessionID] = sessions[i]
	}
	f.mu.Unlock()
	if listener != nil {
		server := servers[0]
		go func() {
			if err := server.Serve(); err != nil {
				_ = server.Close()
			}
		}()
	}
	if !leaveSourcePaused {
		if err := resume(); err != nil {
			f.mu.Lock()
			for _, child := range children {
				delete(f.distributedSessions, child.SessionID)
			}
			f.mu.Unlock()
			return nil, fmt.Errorf("resuming source branch %q: %w", sourceID, err)
		}
	}
	f.mu.Lock()
	source.state = kernelBranchState(source.k)
	f.mu.Unlock()
	committed = true
	return infos, nil
}

// ServeDistributedForkConnection takes ownership of one connection accepted
// by ttcd and starts serving it. The stream may live for the entire remote
// dependency lifetime, so it must not occupy the Sentry control RPC handler.
func (f *SandboxFamily) ServeDistributedForkConnection(args *DistributedForkServeArgs) error {
	if args == nil || args.SessionID == "" || len(args.FilePayload.Files) != 1 {
		return errors.New("distributed page serving requires a session and one connected socket")
	}
	f.mu.Lock()
	session := f.distributedSessions[args.SessionID]
	f.mu.Unlock()
	if session == nil {
		return fmt.Errorf("distributed fork session %q does not exist", args.SessionID)
	}
	conn, err := ttcremote.NewDonatedTCPConn(args.FilePayload.Files[0])
	if err != nil {
		return err
	}
	go func() { _ = session.server.ServeStream(conn) }()
	return nil
}

// DistributedForkFilesystemSource returns the transaction-fenced token for a
// live capture, the repository branch the capture sealed, the one branch whose
// files the token reads, and the source's own repository branch, whose diff
// base the token reads too. The token remains valid exactly as long as the
// source session.
func (f *SandboxFamily) DistributedForkFilesystemSource(id string) (token ttcvolume.RemoteToken, branch, sourceBranch string, err error) {
	f.mu.Lock()
	session := f.distributedSessions[id]
	f.mu.Unlock()
	if session == nil {
		return ttcvolume.RemoteToken{}, "", "", fmt.Errorf("distributed fork session %q does not exist", id)
	}
	return session.filesystemToken, session.filesystemBranch, session.sourceFilesystemBranch, nil
}

// ExportDistributedForkSnapshot writes a session's immutable memory graph.
func (f *SandboxFamily) ExportDistributedForkSnapshot(args *DistributedForkExportArgs) (*DistributedForkExportInfo, error) {
	if args == nil || args.SessionID == "" || len(args.FilePayload.Files) != 1 {
		return nil, errors.New("distributed snapshot export requires a session and one destination")
	}
	f.mu.Lock()
	session := f.distributedSessions[args.SessionID]
	f.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("distributed fork session %q does not exist", args.SessionID)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.sparseSnapshot == nil {
		return nil, fmt.Errorf("distributed fork session %q sparse snapshot is unavailable", args.SessionID)
	}
	var err error
	if args.FilteredGraphStream {
		err = ttcchunk.WriteFilteredGraphCapture(args.FilePayload.Files[0], session.sparseSnapshot, session.memoryBindings)
	} else {
		err = ttcchunk.WriteGraphCapture(args.FilePayload.Files[0], session.sparseSnapshot, session.memoryBindings)
	}
	if err != nil {
		return nil, fmt.Errorf("writing distributed graph capture: %w", err)
	}
	return &DistributedForkExportInfo{FilesystemBranch: session.filesystemBranch}, nil
}

// DistributedForkSessionStatus returns progress without disclosing the bearer
// token required to read the snapshot.
func (f *SandboxFamily) DistributedForkSessionStatus(id string) (*DistributedForkSessionStatus, error) {
	f.mu.Lock()
	session := f.distributedSessions[id]
	f.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("distributed fork session %q does not exist", id)
	}
	return &DistributedForkSessionStatus{
		SessionID: id, Source: session.source, Address: session.address,
		Transport: session.server.Stats(), Snapshot: session.server.SnapshotStats(),
	}, nil
}

// CancelDistributedForkSession closes the transport and releases every page
// still retained for the target. It is idempotent for a session that already
// completed cancellation in this process.
func (f *SandboxFamily) CancelDistributedForkSession(id string) error {
	f.mu.Lock()
	session := f.distributedSessions[id]
	if session != nil {
		delete(f.distributedSessions, id)
	}
	f.mu.Unlock()
	if session == nil {
		return nil
	}
	session.mu.Lock()
	var sparseErr error
	if session.sparseSnapshot != nil {
		sparseErr = session.sparseSnapshot.Close()
		session.sparseSnapshot = nil
	}
	session.mu.Unlock()
	return errors.Join(session.server.Close(), sparseErr, removeDistributedFilesystemBranch(session.resources, session.filesystemBranch))
}

func removeDistributedFilesystemBranch(resources vfs.LiveForkFilesystemResources, branch string) error {
	var result error
	seen := make(map[vfs.LiveForkBranchingResource]struct{})
	for _, resource := range resources {
		branching, ok := resource.(vfs.LiveForkBranchingResource)
		if !ok {
			continue
		}
		comparable := reflect.TypeOf(branching).Comparable()
		if comparable {
			if _, ok := seen[branching]; ok {
				continue
			}
			seen[branching] = struct{}{}
		}
		if err := branching.RemoveBranch(branch); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// CompleteDistributedForkSession releases memory ownership and the sealed
// source-only filesystem generation after the target has materialized it.
func (f *SandboxFamily) CompleteDistributedForkSession(id string) error {
	f.mu.Lock()
	session := f.distributedSessions[id]
	if session != nil {
		delete(f.distributedSessions, id)
	}
	f.mu.Unlock()
	if session == nil {
		return nil
	}
	session.mu.Lock()
	var sparseErr error
	if session.sparseSnapshot != nil {
		sparseErr = session.sparseSnapshot.Close()
		session.sparseSnapshot = nil
	}
	session.mu.Unlock()
	return errors.Join(session.server.Close(), sparseErr, removeDistributedFilesystemBranch(session.resources, session.filesystemBranch))
}

// DistributedForkDependencyStatus reports target-side remote-memory progress.
func (f *SandboxFamily) DistributedForkDependencyStatus(branchID string) (pgalloc.AsyncPagesFileLoadStats, error) {
	b, release, err := f.beginControl(branchID)
	if err != nil {
		return pgalloc.AsyncPagesFileLoadStats{}, err
	}
	defer release()
	return b.k.DistributedForkDependencyStatus(), nil
}

// PageLoadStatus reports all asynchronous page readers, including immutable
// durable readers whose roots must remain pinned until consumption completes.
func (f *SandboxFamily) PageLoadStatus(branchID string) (pgalloc.AsyncPagesFileLoadStats, error) {
	b, release, err := f.beginControl(branchID)
	if err != nil {
		return pgalloc.AsyncPagesFileLoadStats{}, err
	}
	defer release()
	return b.k.PageLoadStatus(), nil
}

// ReclaimImageChunks frees the rehydrated chunks of the family's memory that
// no branch uses any more, as every page in them was since rewritten or
// freed, and returns every chunk freed so since it was installed: the parts
// of the family's images that no dehydration will share.
func (f *SandboxFamily) ReclaimImageChunks(branchID string) ([]pgalloc.SparseChunkObjectID, error) {
	b, release, err := f.beginControl(branchID)
	if err != nil {
		return nil, err
	}
	defer release()
	mf := b.k.MemoryFile()
	mf.ReclaimUnusedSparseChunks()
	return mf.ReclaimedSparseChunks(), nil
}

// SetDistributedForkPrefetchWindow changes future remote-memory fault reads.
func (f *SandboxFamily) SetDistributedForkPrefetchWindow(branchID string, behind, ahead uint64) error {
	b, release, err := f.beginControl(branchID)
	if err != nil {
		return err
	}
	defer release()
	return b.k.SetDistributedForkPrefetchWindow(behind, ahead)
}

// DrainDistributedForkMemory eagerly materializes a target branch's pages.
func (f *SandboxFamily) DrainDistributedForkMemory(branchID string) error {
	b, release, err := f.beginControl(branchID)
	if err != nil {
		return err
	}
	defer release()
	b.k.DrainDistributedForkMemory()
	return nil
}

// HandoffDistributedForkMemory switches future faults from the live source to
// a durable snapshot graph after all in-flight network reads quiesce.
func (f *SandboxFamily) HandoffDistributedForkMemory(args *DistributedForkHandoffArgs) error {
	if args == nil || args.BranchID == "" {
		return errors.New("distributed fork handoff requires a branch")
	}
	want := 2
	if args.GraphRemote {
		want++
	}
	if args.GraphSnapshot == 0 || len(args.FilePayload.Files) != want {
		return errors.New("distributed graph handoff requires a snapshot, manifest, object store, and optional relay")
	}
	f.mu.Lock()
	if current := f.distributedHandoffs[args.BranchID]; current != nil {
		select {
		case <-current.done:
			// A completed handoff records the immutable snapshot, but its relay
			// connection is deliberately replaceable. The helper serving that
			// connection may have failed or may predate daemon-independent relays.
			// Reinstalling the same snapshot switches only the unresolved-page
			// reader and is therefore both safe and necessary during recovery.
			delete(f.distributedHandoffs, args.BranchID)
		default:
			f.mu.Unlock()
			if current.snapshot == uint64(args.GraphSnapshot) {
				return nil
			}
			return fmt.Errorf("branch %q already has graph handoff %d in progress", args.BranchID, current.snapshot)
		}
	}
	f.mu.Unlock()
	encoded, err := io.ReadAll(args.FilePayload.Files[0])
	if err != nil {
		return fmt.Errorf("reading distributed graph handoff manifest: %w", err)
	}
	manifest, err := ttcchunk.DecodeManifest(encoded)
	if err != nil {
		return fmt.Errorf("decoding distributed graph handoff manifest: %w", err)
	}
	data, err := ttcsnapshot.OpenLocalDataStoreFD(args.FilePayload.Files[1])
	if err != nil {
		return fmt.Errorf("opening distributed graph handoff objects: %w", err)
	}
	var remote *ttcsnapshot.ObjectRelayClient
	if args.GraphRemote {
		fd, err := unix.Dup(int(args.FilePayload.Files[2].Fd()))
		if err != nil {
			data.Close()
			return err
		}
		remote, err = ttcsnapshot.NewObjectRelayClient(os.NewFile(uintptr(fd), "distributed-graph-handoff-relay"), args.GraphRemoteToken)
		if err != nil {
			data.Close()
			return err
		}
	}
	b, release, err := f.beginControl(args.BranchID)
	if err != nil {
		if remote != nil {
			_ = remote.Close()
		}
		_ = data.Close()
		return err
	}
	state := &distributedForkHandoff{snapshot: uint64(args.GraphSnapshot), done: make(chan struct{})}
	f.mu.Lock()
	if current := f.distributedHandoffs[args.BranchID]; current != nil {
		f.mu.Unlock()
		release()
		if remote != nil {
			_ = remote.Close()
		}
		_ = data.Close()
		return errors.New("distributed graph handoff was started concurrently")
	}
	f.distributedHandoffs[args.BranchID] = state
	f.mu.Unlock()
	go func() {
		err := b.k.HandoffDistributedForkMemoryGraph(data, remote, manifest, args.GraphSnapshot)
		release()
		f.mu.Lock()
		state.err = err
		close(state.done)
		f.mu.Unlock()
	}()
	return nil
}

// DistributedForkMemoryHandoffStatus observes a reader switch without taking
// the branch operation lock or waiting for its old in-flight reads.
func (f *SandboxFamily) DistributedForkMemoryHandoffStatus(branchID string) (DistributedForkHandoffStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.distributedHandoffs[branchID]
	if state == nil {
		return DistributedForkHandoffStatus{}, fmt.Errorf("branch %q has no distributed graph handoff", branchID)
	}
	status := DistributedForkHandoffStatus{Snapshot: state.snapshot}
	select {
	case <-state.done:
		status.Done = true
		if state.err != nil {
			status.Error = state.err.Error()
		}
	default:
	}
	return status, nil
}

// CloseDistributedForkSessions releases all remote dependencies during Sentry
// teardown.
func (f *SandboxFamily) CloseDistributedForkSessions() {
	f.mu.Lock()
	sessions := f.distributedSessions
	f.distributedSessions = make(map[string]*distributedForkSession)
	f.mu.Unlock()
	for _, session := range sessions {
		_ = session.server.Close()
	}
}

func (f *SandboxFamily) volumeBranchID(visibleBranchID string) string {
	// The initial loader branch explicitly records the repository's canonical
	// "main" pointer. Every branch created later is family-scoped, including a
	// reused visible name "main", so deleting and recreating a branch cannot
	// collide with a named volume's canonical branch.
	return ttcvolume.ScopedBranchID(f.volumeBranchScope, visibleBranchID)
}

func (f *SandboxFamily) injectForkFailure(stage SandboxForkStage) error {
	if f.testOnlyForkFailureInjector == nil {
		return nil
	}
	if err := f.testOnlyForkFailureInjector(stage); err != nil {
		return fmt.Errorf("sandbox fork aborted after %s: %w", stage, err)
	}
	return nil
}

func (f *SandboxFamily) injectExecFailure(stage SandboxExecStage) error {
	if f.testOnlyExecFailureInjector == nil {
		return nil
	}
	if err := f.testOnlyExecFailureInjector(stage); err != nil {
		return fmt.Errorf("sandbox exec aborted after %s: %w", stage, err)
	}
	return nil
}

func kernelBranchState(k *kernel.Kernel) BranchState {
	if k == nil || k.TaskSet() == nil {
		return BranchRunning
	}
	if k.IsPaused() {
		return BranchPaused
	}
	return BranchRunning
}

// ReplaceDefaultKernel replaces the loader-referenced branch during checkpoint
// restore. No peer branches may exist because checkpoint restore cannot
// preserve a multi-branch family.
func (f *SandboxFamily) ReplaceDefaultKernel(k *kernel.Kernel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.branches) != 1 {
		return fmt.Errorf("cannot replace loader kernel while %d peer branches exist", len(f.branches)-1)
	}
	b, ok := f.branches[f.loaderBranchID]
	if !ok || b == nil {
		return errors.New("loader branch is unavailable during kernel replacement")
	}
	b.k = k
	// The replacement Kernel has not been loaded yet. In particular, its
	// TaskSet is still uninitialized, so querying Kernel.IsPaused is invalid.
	// Loader.Run replaces this transitional value with the restored Kernel's
	// actual state after Kernel.Start.
	b.state = BranchPaused
	return nil
}

// ReplaceLoaderBranchFromDehydration replaces the unstarted placeholder with
// any durable branch image. The selected branch keeps its identity; becoming
// Loader.k does not alter its lineage or give it authority over peer branches.
func (f *SandboxFamily) ReplaceLoaderBranchFromDehydration(id, parentID, filesystemBranch string, k *kernel.Kernel, dog *watchdog.Watchdog, sessions []ExecSessionInfo) error {
	if id == "" || filesystemBranch == "" || k == nil {
		return errors.New("cold loader replacement is missing branch, filesystem, or Kernel state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.branches) != 1 {
		return fmt.Errorf("cannot replace cold loader while %d peer branches exist", len(f.branches)-1)
	}
	placeholder := f.branches[f.loaderBranchID]
	if placeholder == nil {
		return errors.New("loader placeholder is unavailable during cold replacement")
	}
	execSessions, err := restoredExecSessions(k, sessions)
	if err != nil {
		return err
	}
	createdAt := placeholder.createdAt
	f.branches = map[string]*sandboxBranch{
		id: {
			id:               id,
			parentID:         parentID,
			filesystemBranch: filesystemBranch,
			k:                k,
			watchdog:         dog,
			state:            BranchPaused,
			createdAt:        createdAt,
			quarantined:      k.LiveForkQuarantine(),
			execSessions:     execSessions,
		},
	}
	f.loaderBranchID = id
	return nil
}

// SetDefaultFilesystemBranch records the persistent repository branch used by
// a replacement default Kernel. Distributed restore needs this because the
// target is presented as visible branch "main", while its ttcvol branch keeps
// the globally unique source-family transaction name.
func (f *SandboxFamily) SetDefaultFilesystemBranch(branch string) error {
	if branch == "" {
		return errors.New("default filesystem branch is empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[f.loaderBranchID]
	if !ok || b == nil {
		return errors.New("default branch is unavailable")
	}
	b.filesystemBranch = branch
	return nil
}

// HasMultipleBranches reports whether this family cannot use gVisor's
// single-Kernel checkpoint format.
func (f *SandboxFamily) HasMultipleBranches() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.branches) != 1
}

// Checkpoint serializes one branch against concurrent branch lifecycle
// operations. The callback owns checkpoint I/O; Fork never calls this method.
func (f *SandboxFamily) Checkpoint(id string, defaultWatchdog *watchdog.Watchdog, save func(*kernel.Kernel, *watchdog.Watchdog, []ExecSessionInfo) error) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()

	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, cannot checkpoint", id, state)
	}
	dog := b.watchdog
	if dog == nil {
		dog = defaultWatchdog
	}
	sessions := make([]ExecSessionInfo, 0, len(b.execSessions))
	for _, session := range b.execSessions {
		sessions = append(sessions, cloneExecSessionInfo(session.info))
	}
	f.mu.Unlock()
	if dog == nil {
		return fmt.Errorf("branch %q has no watchdog", id)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return save(b.k, dog, sessions)
}

// RestoreLoaderExecSessions reinstates TTC control-plane identities after a
// standalone checkpoint is loaded as a new family. The task graph and internal
// consoles themselves were restored as Kernel state. Watchers are started
// separately after Kernel.Start initializes ThreadGroup.liveGoroutines.
func (f *SandboxFamily) RestoreLoaderExecSessions(sessions []ExecSessionInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.branches[f.loaderBranchID]
	if b == nil || b.k == nil {
		return errors.New("loader branch is unavailable during exec-session restore")
	}
	if len(b.execSessions) != 0 {
		return errors.New("loader branch already has exec sessions during restore")
	}
	restored, err := restoredExecSessions(b.k, sessions)
	if err != nil {
		return err
	}
	b.execSessions = restored
	return nil
}

func restoredExecSessions(k *kernel.Kernel, sessions []ExecSessionInfo) (map[string]*execSession, error) {
	restored := make(map[string]*execSession)
	for _, info := range sessions {
		if info.ID == "" || info.PID <= 0 {
			return nil, fmt.Errorf("invalid restored exec session %+v", info)
		}
		if _, ok := restored[info.ID]; ok {
			return nil, fmt.Errorf("duplicate restored exec session %q", info.ID)
		}
		if info.State == ExecSessionExited {
			if info.ExitStatus == nil {
				return nil, fmt.Errorf("restored exited exec session %q has no status", info.ID)
			}
			restored[info.ID] = &execSession{info: cloneExecSessionInfo(info), retain: true}
			continue
		}
		if info.State != ExecSessionRunning {
			return nil, fmt.Errorf("restored exec session %q has invalid state %q", info.ID, info.State)
		}
		tg := k.RootPIDNamespace().ThreadGroupWithID(info.PID)
		if tg == nil || tg.Leader().Origin != kernel.OriginExec {
			return nil, fmt.Errorf("restored exec session %q has no exec thread group with PID %d", info.ID, info.PID)
		}
		// Restored sessions have no foreground waiter. Keep their identity and
		// output available after they exit.
		session := &execSession{info: cloneExecSessionInfo(info), tg: tg, retain: true}
		restored[info.ID] = session
	}
	return restored, nil
}

// WatchRestoredLoaderExecSessions starts exit tracking after the restored
// Kernel has started all task goroutines.
func (f *SandboxFamily) WatchRestoredLoaderExecSessions() error {
	f.mu.Lock()
	loaderID := f.loaderBranchID
	b := f.branches[loaderID]
	if b == nil || b.k == nil {
		f.mu.Unlock()
		return errors.New("loader branch is unavailable while starting restored exec-session watchers")
	}
	restored := make([]struct {
		id      string
		session *execSession
	}, 0, len(b.execSessions))
	for id, session := range b.execSessions {
		if session.info.State == ExecSessionRunning {
			restored = append(restored, struct {
				id      string
				session *execSession
			}{id, session})
		}
	}
	f.mu.Unlock()
	for _, item := range restored {
		f.watchExecSession(loaderID, item.id, item.session)
	}
	return nil
}

// QuiesceControls rejects new Kernel-lifetime control operations and waits for
// those already registered on every branch. Boot shutdown uses this after the
// default init exits so final console frames and wait statuses reach clients
// before the control server is destroyed.
func (f *SandboxFamily) QuiesceControls() {
	f.mu.Lock()
	f.controlsQuiesced = true
	branches := make([]*sandboxBranch, 0, len(f.branches))
	for _, b := range f.branches {
		branches = append(branches, b)
	}
	f.mu.Unlock()
	for _, b := range branches {
		b.controlOps.Wait()
	}
}

// MarkDefaultStarted records the default kernel state after Loader.Run.
func (f *SandboxFamily) MarkDefaultStarted() {
	f.mu.Lock()
	b := f.branches[f.loaderBranchID]
	b.state = kernelBranchState(b.k)
	f.mu.Unlock()
	f.defaultStartedOnce.Do(func() { close(f.defaultStarted) })
}

func cloneExecSessionInfo(info ExecSessionInfo) ExecSessionInfo {
	cloned := info
	if info.ExitStatus != nil {
		status := *info.ExitStatus
		cloned.ExitStatus = &status
	}
	return cloned
}

func cloneExecSessionInfos(in []ExecSessionInfo) []ExecSessionInfo {
	out := make([]ExecSessionInfo, len(in))
	for i := range in {
		out[i] = cloneExecSessionInfo(in[i])
	}
	return out
}

// reserveExecSession registers a creating session. The caller must hold b.opMu
// so that fork and lifecycle transitions cannot observe a partial publication.
func (f *SandboxFamily) reserveExecSession(b *sandboxBranch, args *control.ExecArgs) error {
	if args.ExecID == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := b.execSessions[args.ExecID]; ok {
		return fmt.Errorf("exec session %q already exists in branch %q", args.ExecID, b.id)
	}
	if b.execSessions == nil {
		b.execSessions = make(map[string]*execSession)
	}
	b.execSessions[args.ExecID] = &execSession{info: ExecSessionInfo{
		ID:          args.ExecID,
		ContainerID: args.ContainerID,
		ConsoleID:   args.InternalConsoleID,
		Terminal:    args.StdioIsPty,
		State:       ExecSessionCreating,
		CreatedAt:   time.Now().UTC(),
	}, retain: args.RetainExecSession}
	return nil
}

func (f *SandboxFamily) abortExecSession(b *sandboxBranch, id string) {
	if id == "" {
		return
	}
	f.mu.Lock()
	delete(b.execSessions, id)
	f.mu.Unlock()
}

func (f *SandboxFamily) publishExecSession(b *sandboxBranch, id string, pid kernel.ThreadID, tg *kernel.ThreadGroup, managementDone <-chan struct{}) {
	if id == "" {
		return
	}
	f.mu.Lock()
	session := b.execSessions[id]
	if session != nil {
		session.info.PID = pid
		session.info.State = ExecSessionRunning
		session.tg = tg
		session.managementDone = managementDone
	}
	f.mu.Unlock()
	if session != nil {
		f.watchExecSession(b.id, id, session)
	}
}

func (f *SandboxFamily) watchExecSession(branchID, id string, session *execSession) {
	// Completed sessions are deliberately retained so clients can inspect their
	// status and output after a fork, checkpoint restore, or remote import. They
	// have no live ThreadGroup to watch. In particular, restorePeer installs
	// both running and completed session records before starting watchers.
	if session == nil || session.info.State != ExecSessionRunning || session.tg == nil {
		return
	}
	tg := session.tg
	go func() {
		tg.WaitExited()
		status := uint32(tg.ExitStatus())
		f.mu.Lock()
		if branch := f.branches[branchID]; branch != nil && branch.execSessions[id] == session {
			session.info.State = ExecSessionExited
			session.info.ExitStatus = &status
		}
		f.mu.Unlock()
	}()
}

// List returns a stable snapshot sorted by branch ID.
func (f *SandboxFamily) List() []BranchInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	liveKernels := 0
	for _, b := range f.branches {
		if b.k != nil {
			liveKernels++
		}
	}
	out := make([]BranchInfo, 0, len(f.branches))
	for _, b := range f.branches {
		initPID, initState := branchInitStatus(b)
		out = append(out, BranchInfo{
			ID:               b.id,
			ParentID:         b.parentID,
			FilesystemBranch: b.filesystemBranch,
			State:            b.state,
			CreatedAt:        b.createdAt,
			InitPID:          initPID,
			InitState:        initState,
			SharesMemory:     b.k != nil && liveKernels > 1,
			Quarantined:      b.quarantined,
			ForkMetrics:      b.metrics,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func branchInitStatus(b *sandboxBranch) (kernel.ThreadID, string) {
	if b.k == nil || b.k.TaskSet() == nil || b.k.RootPIDNamespace() == nil {
		return 0, "not-started"
	}
	if b.k.RootPIDNamespace().ThreadGroupWithID(1) != nil {
		return 1, string(b.state)
	}
	if b.k.GlobalInit() != nil {
		return 1, "exited"
	}
	return 0, "not-started"
}

// Fork creates one child from args.Source.
func (f *SandboxFamily) Fork(args *ForkBranchArgs) (*BranchInfo, error) {
	if args == nil {
		return nil, fmt.Errorf("missing live-fork arguments")
	}
	infos, err := f.ForkMany(&ForkBranchesArgs{
		Source:            args.Source,
		IDs:               []string{args.ID},
		Paused:            args.Paused,
		Quarantine:        args.Quarantine,
		FilesystemOrigins: args.FilesystemOrigins,
	})
	if err != nil {
		return nil, err
	}
	return &infos[0], nil
}

// ForkMany creates sibling children from one source snapshot. The source is
// paused once. A single primary child pins the captured MemoryFile ranges and
// can immediately release the source. For a sibling batch, the reusable child
// metadata is also captured before resuming the source because state capture
// uses process-wide quiescence; sibling decoding then proceeds concurrently
// with source execution.
func (f *SandboxFamily) ForkMany(args *ForkBranchesArgs) ([]BranchInfo, error) {
	forkStarted := time.Now()
	if args == nil {
		return nil, fmt.Errorf("missing live-fork arguments")
	}
	if len(args.IDs) == 0 {
		return nil, fmt.Errorf("at least one branch ID is required")
	}
	originPlan, err := filesystemOriginPlan(args.FilesystemOrigins)
	if err != nil {
		return nil, err
	}
	if !f.liveForkEligible {
		return nil, fmt.Errorf("container family explicitly waived live fork eligibility at creation")
	}
	sourceID := args.Source
	if sourceID == "" {
		sourceID = f.LoaderBranchID()
	}

	f.mu.Lock()
	source := f.branches[sourceID]
	if source == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("source branch %q does not exist", sourceID)
	}
	seen := make(map[string]struct{}, len(args.IDs))
	childBranches := make([]*sandboxBranch, len(args.IDs))
	for i, id := range args.IDs {
		if id == "" {
			f.mu.Unlock()
			return nil, fmt.Errorf("branch ID must not be empty")
		}
		if _, ok := seen[id]; ok {
			f.mu.Unlock()
			return nil, fmt.Errorf("branch %q is repeated", id)
		}
		seen[id] = struct{}{}
		if _, ok := f.branches[id]; ok {
			f.mu.Unlock()
			return nil, fmt.Errorf("branch %q already exists", id)
		}
		filesystemBranch, err := ttcvolume.NewScopedBranchID(f.volumeBranchScope, id)
		if err != nil {
			f.mu.Unlock()
			return nil, fmt.Errorf("allocating filesystem identity for branch %q: %w", id, err)
		}
		childBranches[i] = &sandboxBranch{
			id:               id,
			parentID:         sourceID,
			filesystemBranch: filesystemBranch,
			state:            BranchCreating,
			createdAt:        time.Now().UTC(),
			quarantined:      source.quarantined || args.Quarantine,
			execSessions:     make(map[string]*execSession),
		}
	}
	for _, childBranch := range childBranches {
		f.branches[childBranch.id] = childBranch
	}
	f.mu.Unlock()

	fail := func(err error) ([]BranchInfo, error) {
		f.mu.Lock()
		for _, childBranch := range childBranches {
			childBranch.state = BranchFailed
			delete(f.branches, childBranch.id)
		}
		f.mu.Unlock()
		return nil, err
	}
	if err := f.injectForkFailure(SandboxForkStagePlaceholderRegistered); err != nil {
		return fail(err)
	}

	source.opMu.Lock()
	defer source.opMu.Unlock()
	f.mu.Lock()
	if source.state != BranchRunning && source.state != BranchPaused {
		state := source.state
		f.mu.Unlock()
		return fail(fmt.Errorf("source branch %q is %s", sourceID, state))
	}
	sourceWasPaused := source.k.IsPaused()
	f.mu.Unlock()

	// Waiter queues are part of pipe and PTY state. Remove host-side console
	// waiters before cloning so no control callback is inherited by the child.
	f.cancelConsoleWaits(source)

	// With --nvproxy-fork=snapshot, the processes that use NVIDIA GPUs hold
	// the GPUs' state in their own memory, which the fork copies, while
	// cuda-checkpoint has them checkpointed. The resumes of the source and of
	// the children restore them.
	if f.cudaCheckpointForFork != "" && nvproxy.AnyFrontendDevicesOpen(source.k.VFS()) {
		if err := control.CheckpointCudaForFork(source.k, f.cudaCheckpointForFork); err != nil {
			return fail(fmt.Errorf("checkpointing the CUDA processes of branch %q: %w", sourceID, err))
		}
	}

	metrics := make([]SandboxForkMetrics, len(childBranches))
	if !sourceWasPaused {
		pauseStarted := time.Now()
		source.k.Pause()
		pauseDuration := time.Since(pauseStarted)
		for i := range metrics {
			metrics[i].SourcePause = pauseDuration
		}
	}
	sourcePausedAt := time.Now()
	sourceResumed := false
	resumeSource := func() error {
		if sourceWasPaused || sourceResumed {
			return nil
		}
		resumeStarted := time.Now()
		source.k.Unpause()
		err := control.PostResume(source.k, nil)
		sourceResumed = true
		resumeDuration := time.Since(resumeStarted)
		downtime := time.Since(sourcePausedAt)
		for i := range metrics {
			metrics[i].SourceResume += resumeDuration
			metrics[i].SourceDowntime = downtime
		}
		return err
	}
	if err := f.injectForkFailure(SandboxForkStageSourcePaused); err != nil {
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(err)
	}

	kernelOpts := make([]kernel.LiveForkOptions, len(childBranches))
	for i, childBranch := range childBranches {
		kernelOpts[i] = kernel.LiveForkOptions{
			Clocks:                         sentrytime.NewCalibratedClocks(f.clockMonotonicRaw),
			ChildBranchID:                  childBranch.filesystemBranch,
			AllowNetworkStack:              f.allowNetworkStack,
			AllowSharedFilesystemResources: f.allowSharedFilesystemResources,
			Quarantine:                     args.Quarantine,
			Metrics:                        &metrics[i].Kernel,
			FilesystemOrigins:              originPlan,
		}
	}
	children, err := source.k.LiveForkMany(source.k.SupervisorContext(), kernelOpts, resumeSource)
	if err != nil {
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(fmt.Errorf("forking %d branches from %q: %w", len(args.IDs), sourceID, err))
	}
	if err := f.injectForkFailure(SandboxForkStageKernelCloned); err != nil {
		for _, child := range children {
			child.Release()
		}
		return fail(err)
	}

	// The child Kernel contains the same virtual PIDs. Clone only sessions whose
	// thread groups were present in the memory snapshot; an exit racing the
	// watcher may leave source metadata briefly marked running.
	clonedExecSessions := make([]map[string]*execSession, len(children))
	for i, child := range children {
		childStartStarted := time.Now()
		if err := child.StartPaused(); err != nil {
			for j, startedChild := range children {
				if j < i {
					startedChild.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
					startedChild.Unpause()
					startedChild.WaitExited()
				}
				startedChild.Release()
			}
			return fail(fmt.Errorf("starting branch %q paused: %w", childBranches[i].id, err))
		}
		metrics[i].ChildStart = time.Since(childStartStarted)
		clonedExecSessions[i] = make(map[string]*execSession)
		f.mu.Lock()
		for id, sourceSession := range source.execSessions {
			if sourceSession.info.State != ExecSessionRunning {
				continue
			}
			childTG := child.RootPIDNamespace().ThreadGroupWithID(sourceSession.info.PID)
			if childTG == nil {
				continue
			}
			info := cloneExecSessionInfo(sourceSession.info)
			info.State = ExecSessionRunning
			info.ExitStatus = nil
			// A foreground waiter belongs only to the source command. Its cloned
			// process has no waiter, so expose the child copy as a retained session.
			clonedExecSessions[i][id] = &execSession{info: info, tg: childTG, retain: true}
		}
		f.mu.Unlock()
	}
	cleanupStartedChildren := func(dogs []*watchdog.Watchdog) {
		for i, child := range children {
			if dogs != nil && dogs[i] != nil {
				dogs[i].Stop()
			}
			child.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
			if child.IsPaused() {
				child.Unpause()
			}
			child.WaitExited()
			child.Release()
		}
	}
	if err := f.injectForkFailure(SandboxForkStageChildStarted); err != nil {
		cleanupStartedChildren(nil)
		return fail(err)
	}
	childWatchdogs := make([]*watchdog.Watchdog, len(children))
	if f.watchdogOpts != nil {
		for i := range children {
			childWatchdogs[i] = watchdog.New(children[i], *f.watchdogOpts)
			childWatchdogs[i].Start()
		}
	}
	f.mu.Lock()
	for i, childBranch := range childBranches {
		childBranch.k = children[i]
		childBranch.watchdog = childWatchdogs[i]
		childBranch.state = BranchPaused
		childBranch.execSessions = clonedExecSessions[i]
	}
	f.mu.Unlock()
	if err := f.injectForkFailure(SandboxForkStageBranchReady); err != nil {
		cleanupStartedChildren(childWatchdogs)
		return fail(err)
	}
	for i, sessions := range clonedExecSessions {
		for id, session := range sessions {
			f.watchExecSession(childBranches[i].id, id, session)
		}
	}
	if !args.Paused {
		for i, child := range children {
			childResumeStarted := time.Now()
			child.Unpause()
			if err := control.PostResume(child, nil); err != nil {
				child.Pause()
				cleanupStartedChildren(childWatchdogs)
				return fail(fmt.Errorf("resuming branch %q: %w", childBranches[i].id, err))
			}
			metrics[i].ChildResume = time.Since(childResumeStarted)
			f.mu.Lock()
			childBranches[i].state = kernelBranchState(child)
			f.mu.Unlock()
		}
	}
	if err := resumeSource(); err != nil {
		cleanupStartedChildren(childWatchdogs)
		return fail(fmt.Errorf("resuming source branch %q: %w", sourceID, err))
	}

	f.mu.Lock()
	source.state = kernelBranchState(source.k)
	infos := make([]BranchInfo, len(childBranches))
	total := time.Since(forkStarted)
	for i, childBranch := range childBranches {
		metrics[i].Total = total
		childBranch.metrics = metrics[i]
		infos[i] = BranchInfo{
			ID:               childBranch.id,
			ParentID:         childBranch.parentID,
			FilesystemBranch: childBranch.filesystemBranch,
			State:            childBranch.state,
			CreatedAt:        childBranch.createdAt,
			Quarantined:      childBranch.quarantined,
			ForkMetrics:      metrics[i],
		}
	}
	f.mu.Unlock()
	return infos, nil
}

// Pause pauses a branch.
func (f *SandboxFamily) Pause(id string) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not running", id, state)
	}
	f.mu.Unlock()
	b.k.Pause()
	f.mu.Lock()
	b.state = BranchPaused
	f.mu.Unlock()
	return nil
}

// SnapshotFilesystemBranch creates an O(1) repository branch for one
// persistent filesystem without cloning the Kernel. The guest branch is
// paused across metadata flushing, mmap invalidation, and branch publication,
// so an attachment in another Sentry cannot observe a torn filesystem or
// leave the source writing through a pre-fork object generation.
func (f *SandboxFamily) SnapshotFilesystemBranch(id string, resource checkpoint.ResourceID, child string, sourceWorking, childWorking uint64) (retErr error) {
	if !resource.Ok() || child == "" {
		return errors.New("filesystem snapshot requires a resource and child branch")
	}
	if (sourceWorking == 0) != (childWorking == 0) || (sourceWorking != 0 && sourceWorking == childWorking) {
		return errors.New("filesystem snapshot requires two distinct clustered working origins")
	}
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()

	f.mu.Lock()
	state := b.state
	if state != BranchRunning && state != BranchPaused {
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not running or paused", id, state)
	}
	wasRunning := state == BranchRunning
	f.mu.Unlock()
	if wasRunning {
		b.k.Pause()
	}
	defer func() {
		if !wasRunning {
			return
		}
		b.k.Unpause()
		if err := control.PostResume(b.k, nil); err != nil {
			b.k.Pause()
			if retErr == nil {
				retErr = fmt.Errorf("resuming branch %q after filesystem snapshot: %w", id, err)
			}
		}
	}()

	resources := make(vfs.LiveForkFilesystemResources)
	transactions := &vfs.LiveForkTransactions{}
	ctxValues := map[any]any{
		pgalloc.CtxLiveForkSource:          true,
		vfs.CtxLiveForkFilesystemResources: resources,
		vfs.CtxLiveForkChildBranchID:       child,
		vfs.CtxLiveForkTransactions:        transactions,
		vfs.CtxLiveForkFilesystemResource:  resource,
	}
	if sourceWorking != 0 {
		ctxValues[vfs.CtxLiveForkFilesystemOrigins] = vfs.LiveForkFilesystemOrigins{
			resource: []uint64{sourceWorking, childWorking},
		}
	}
	ctx := context.WithValues(b.k.SupervisorContext(), ctxValues)
	if err := b.k.VFS().PrepareSave(ctx); err != nil {
		_ = transactions.Abort()
		return fmt.Errorf("preparing filesystem resource %q: %w", resource, err)
	}
	defer b.k.VFS().BeforeResume(ctx)
	if _, ok := resources[resource]; !ok {
		_ = transactions.Abort()
		return fmt.Errorf("filesystem resource %q is not mounted in branch %q", resource, id)
	}
	if err := transactions.Commit(); err != nil {
		_ = transactions.Abort()
		return fmt.Errorf("committing filesystem snapshot: %w", err)
	}
	return nil
}

// Rename changes a branch's visible name, leaving everything else alone.
//
// A name is how a branch is asked for, not what it is: the Kernel, the tasks in
// it and the filesystem branch underneath are all reached through the record
// rather than through the string. So this is a re-keying of the table, plus the
// two other places a name is written down — the loader's own branch, and the
// provenance edge each child keeps to its parent.
//
// It exists because a family has to be able to move forward. Work happens on a
// fork and the good one has to become the branch everyone else forks from; the
// alternative is copying it back, which pauses the source and duplicates
// everything it holds to change one string.
func (f *SandboxFamily) Rename(id, newID string) error {
	if id == "" {
		id = f.LoaderBranchID()
	}
	if newID == "" {
		return fmt.Errorf("branch ID must not be empty")
	}
	if id == newID {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.branches[id]
	if b == nil {
		return fmt.Errorf("branch %q does not exist", id)
	}
	if _, taken := f.branches[newID]; taken {
		return fmt.Errorf("branch %q already exists", newID)
	}
	delete(f.branches, id)
	b.id = newID
	f.branches[newID] = b
	if handoff := f.distributedHandoffs[id]; handoff != nil {
		delete(f.distributedHandoffs, id)
		f.distributedHandoffs[newID] = handoff
	}
	if f.loaderBranchID == id {
		f.loaderBranchID = newID
	}
	for _, other := range f.branches {
		if other.parentID == id {
			other.parentID = newID
		}
	}
	return nil
}

// Resume resumes a paused branch.
func (f *SandboxFamily) Resume(id string) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not paused", id, state)
	}
	f.mu.Unlock()
	b.k.Unpause()
	if err := control.PostResume(b.k, nil); err != nil {
		b.k.Pause()
		return err
	}
	f.mu.Lock()
	b.state = kernelBranchState(b.k)
	f.mu.Unlock()
	return nil
}

// PublishFilesystemMetadata flushes process-local TTC filesystem metadata for
// one live branch and returns its authoritative repository branch. The Kernel
// lifetime lease allows the branch to remain running or paused while the short
// graph publication completes. Callers must not derive the repository branch
// from the visible ID: restored and migrated branches can intentionally retain
// a different physical filesystem identity.
func (f *SandboxFamily) PublishFilesystemMetadata(id string) (string, error) {
	b, done, err := f.beginControl(id)
	if err != nil {
		return "", err
	}
	defer done()
	if err := b.k.VFS().PublishFilesystemMetadata(b.k.SupervisorContext()); err != nil {
		return "", err
	}
	branch := b.filesystemBranch
	if branch == "" {
		branch = f.volumeBranchID(b.id)
	}
	return branch, nil
}

// SetQuarantine changes the external-connectivity quarantine bit. Live
// branches briefly pause while their runtime-owned link is replaced;
// dehydrated branches only update the bit used by rehydration.
func (f *SandboxFamily) SetQuarantine(id string, enabled bool) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state == BranchDehydrated {
		b.quarantined = enabled
		f.mu.Unlock()
		return nil
	}
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, cannot change quarantine", id, state)
	}
	wasPaused := b.state == BranchPaused
	k := b.k
	f.mu.Unlock()
	if !wasPaused {
		k.Pause()
	}
	changeErr := k.SetLiveForkQuarantine(enabled)
	if !wasPaused {
		k.Unpause()
		if err := control.PostResume(k, nil); changeErr == nil {
			changeErr = err
		}
	}
	if changeErr != nil {
		return changeErr
	}
	f.mu.Lock()
	b.quarantined = enabled
	f.mu.Unlock()
	return nil
}

// Dehydrate hibernates one branch. Its task and kernel-object graph is
// serialized as metadata, page ownership is replaced by compact reservations,
// and the live Kernel is released. The persistent filesystem branch is left in
// place.
func (f *SandboxFamily) Dehydrate(id string, metadataFile *os.File) error {
	f.dehydrationMu.Lock()
	defer f.dehydrationMu.Unlock()
	metadataOwned := false
	defer func() {
		if metadataFile != nil && !metadataOwned {
			_ = metadataFile.Close()
		}
	}()
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	dehydrated := false
	defer func() {
		if !dehydrated {
			b.opMu.Unlock()
		}
	}()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, cannot dehydrate", id, state)
	}
	wasRunning := b.state == BranchRunning
	b.state = BranchDehydrating
	// The Loader bookkeeping role must remain attached to a process-backed
	// Kernel while any hydrated peer survives. Otherwise releasing the current
	// loader below makes Loader.WaitForExit tear down the Sentry and strands all
	// sibling branches even though dehydration was requested for only one of
	// them.
	var loaderKernel *kernel.Kernel
	var loaderWatchdog *watchdog.Watchdog
	loaderChanged := id == f.loaderBranchID
	if loaderChanged {
		if replacement := f.liveLoaderReplacementLocked(id); replacement != nil {
			f.loaderBranchID = replacement.id
			loaderKernel = replacement.k
			loaderWatchdog = replacement.watchdog
		} else {
			loaderChanged = false
		}
	}
	f.stateChanged.Broadcast()
	k := b.k
	f.mu.Unlock()
	if loaderChanged && f.onLoaderKernelChanged != nil {
		f.onLoaderKernelChanged(loaderKernel, loaderWatchdog)
	}

	f.cancelConsoleWaits(b)
	if wasRunning {
		k.Pause()
	}
	restoreLiveBranch := func() {
		if wasRunning {
			k.Unpause()
			_ = control.PostResume(k, nil)
		}
		f.mu.Lock()
		b.state = kernelBranchState(k)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
	}
	// Dehydration's memory image and persistent filesystem must describe the
	// same paused instant. Disk-backed TTC filesystems deliberately batch inode
	// and write metadata inside the Sentry; sealing the manager-side repository
	// without publishing that tail can resurrect the previous contents when a
	// dehydrated branch is stopped and later started fresh.
	if err := k.VFS().PublishFilesystemMetadata(k.SupervisorContext()); err != nil {
		restoreLiveBranch()
		return fmt.Errorf("publishing filesystem metadata before dehydration: %w", err)
	}
	execSessions := make([]ExecSessionInfo, 0, len(b.execSessions))
	f.mu.Lock()
	for _, session := range b.execSessions {
		execSessions = append(execSessions, cloneExecSessionInfo(session.info))
	}
	f.mu.Unlock()
	sort.Slice(execSessions, func(i, j int) bool { return execSessions[i].ID < execSessions[j].ID })
	filesystemBranch := b.filesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = f.volumeBranchID(b.id)
	}
	image, err := k.CaptureDehydration(k.SupervisorContext(), kernel.DehydrationCaptureOptions{
		BranchID: filesystemBranch, MetadataFile: metadataFile,
	})
	if err != nil {
		restoreLiveBranch()
		return err
	}
	metadataOwned = true
	// A branch imported into an existing family can reuse ranges whose lazy
	// loader belongs to a peer Kernel, and releasing k would cancel its own:
	// either could leave a later export unable to read a page. Pages still
	// loading from the snapshot graph stay readable while the capture keeps
	// their loaders, and an export to the graph, which sends only what the
	// graph lacks, does not read them at all. If a page is still loading from
	// anything else, such as a live source, materialize the exact captured
	// graph now, while every loader is alive.
	retained, err := image.RetainGraphPageLoads()
	if err == nil && !retained {
		err = image.SparseSnapshot.EnsureResident()
	}
	if err != nil {
		_ = image.Close()
		restoreLiveBranch()
		return fmt.Errorf("materializing branch memory before dehydration: %w", err)
	}
	if err := image.ParkSparseSnapshot(); err != nil {
		_ = image.Close()
		restoreLiveBranch()
		return fmt.Errorf("parking dehydrated branch memory: %w", err)
	}
	if b.watchdog != nil {
		b.watchdog.Stop()
	}
	k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	k.Unpause()
	k.WaitExited()
	// Existing console followers take opMu for their authoritative final drain.
	// No new controls can begin in BranchDehydrating, so temporarily yield the
	// lifecycle lock and let those leases finish before releasing the Kernel.
	b.opMu.Unlock()
	b.controlOps.Wait()
	b.opMu.Lock()
	p := k.Platform
	k.Release()
	if f.forgetFilesystemBranch != nil {
		f.forgetFilesystemBranch(filesystemBranch)
	}
	f.mu.Lock()
	b.k = nil
	b.watchdog = nil
	b.dehydration = &branchDehydration{
		image: image, platform: p, resume: wasRunning, execSessions: execSessions,
	}
	var captures []*kernel.DehydrationCapture
	for _, branch := range f.branches {
		if branch.dehydration != nil {
			captures = append(captures, branch.dehydration.image)
		}
	}
	f.mu.Unlock()
	if err := kernel.SyncAndDropDehydrationCache(captures); err != nil {
		_ = image.Close()
		f.mu.Lock()
		b.state = BranchFailed
		b.dehydration = nil
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return fmt.Errorf("persisting dehydrated memory: %w", err)
	}
	if err := f.compactPlatformMemory(p, b); err != nil {
		_ = image.Close()
		f.mu.Lock()
		b.state = BranchFailed
		b.dehydration = nil
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return fmt.Errorf("compacting platform memory after dehydration: %w", err)
	}
	f.mu.Lock()
	b.state = BranchDehydrated
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	if id == f.LoaderBranchID() && f.onLoaderKernelChanged != nil {
		f.onLoaderKernelChanged(nil, nil)
	}
	b.opMu.Unlock()
	dehydrated = true
	return nil
}

// compactPlatformMemory pauses every live sibling sharing p, drops their
// unleased PMA host mappings, and invokes optional physical compaction. The
// caller holds dehydrationMu and excluded.opMu. Locking all other branch
// operation mutexes prevents new memory-accessing controls; compactionOps then
// drains those already in flight. dehydrationMu prevents a concurrent fork
// from publishing an unquiesced sibling into the shared VM.
func (f *SandboxFamily) compactPlatformMemory(p platform.Platform, excluded *sandboxBranch) error {
	compactor, ok := p.(platform.MemoryCompactor)
	if !ok {
		return nil
	}
	var branches []*sandboxBranch
	for {
		f.mu.Lock()
		branches = branches[:0]
		for _, branch := range f.branches {
			if branch != excluded && branch.k != nil && branch.k.Platform == p {
				branches = append(branches, branch)
			}
		}
		f.mu.Unlock()
		sort.Slice(branches, func(i, j int) bool { return branches[i].id < branches[j].id })
		for _, branch := range branches {
			branch.opMu.Lock()
		}
		f.mu.Lock()
		stable := 0
		for _, branch := range f.branches {
			if branch != excluded && branch.k != nil && branch.k.Platform == p {
				stable++
			}
		}
		f.mu.Unlock()
		if stable == len(branches) {
			break
		}
		for i := len(branches) - 1; i >= 0; i-- {
			branches[i].opMu.Unlock()
		}
	}
	defer func() {
		for i := len(branches) - 1; i >= 0; i-- {
			branches[i].opMu.Unlock()
		}
	}()
	for _, branch := range branches {
		// Cancel transient console accesses, then drain only operations that may
		// touch application memory. Process-lifetime waiters remain registered to
		// keep the Kernel alive, but are quiescent while blocked and final console
		// drains serialize on opMu.
		f.cancelConsoleWaits(branch)
		branch.compactionOps.Wait()
	}
	resume := make([]*kernel.Kernel, 0, len(branches))
	for _, branch := range branches {
		if !branch.k.IsPaused() {
			branch.k.Pause()
			resume = append(resume, branch.k)
		}
	}
	defer func() {
		for _, k := range resume {
			k.Unpause()
		}
	}()
	for _, branch := range branches {
		if err := branch.k.DropInternalMappingsForCompaction(); err != nil {
			return fmt.Errorf("dropping branch %q internal mappings: %w", branch.id, err)
		}
	}
	_, err := compactor.CompactMemory(context.Background())
	return err
}

// DehydrationInfo reports the durable memory streams for an already
// dehydrated branch.
func (f *SandboxFamily) DehydrationInfo(id string) (*BranchDehydrateInfo, error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dehydrationInfoLocked(b)
}

// Preconditions: b.opMu and f.mu are locked.
func (f *SandboxFamily) dehydrationInfoLocked(b *sandboxBranch) (*BranchDehydrateInfo, error) {
	if b.state != BranchDehydrated || b.dehydration == nil || b.dehydration.image == nil {
		return nil, fmt.Errorf("branch %q is not available for dehydration export", b.id)
	}
	info := &BranchDehydrateInfo{
		Memories:         make([]BranchDehydrationMemoryInfo, len(b.dehydration.image.MemoryImages)),
		Resume:           b.dehydration.resume,
		ExecSessions:     make([]ExecSessionInfo, len(b.dehydration.execSessions)),
		FilesystemBranch: b.filesystemBranch,
	}
	copy(info.ExecSessions, b.dehydration.execSessions)
	if info.FilesystemBranch == "" {
		info.FilesystemBranch = f.volumeBranchID(b.id)
	}
	for i, memory := range b.dehydration.image.MemoryImages {
		info.Memories[i] = BranchDehydrationMemoryInfo{ResourceID: memory.ResourceID, StreamBytes: memory.StreamBytes}
	}
	return info, nil
}

// ExportDehydration writes graph-native memory for an already dehydrated branch.
func (f *SandboxFamily) ExportDehydration(args *BranchDehydrationExportArgs) (*BranchDehydrationExportInfo, error) {
	if args == nil || args.ID == "" {
		return nil, errors.New("dehydration export requires a branch ID")
	}
	b, err := f.branch(args.ID)
	if err != nil {
		return nil, err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchDehydrated || b.dehydration == nil || b.dehydration.image == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("branch %q is not available for dehydration export", args.ID)
	}
	image := b.dehydration.image
	f.mu.Unlock()
	files := args.FilePayload.Files
	if len(files) != 1 && len(files) != 2 {
		return nil, fmt.Errorf("graph dehydration export has %d descriptors, want a stream and optionally a metadata file", len(files))
	}
	if len(files) == 2 {
		dup, err := unix.Dup(int(files[1].Fd()))
		if err != nil {
			return nil, fmt.Errorf("duplicating dehydration metadata file: %w", err)
		}
		if err := image.AttachMetadataFile(os.NewFile(uintptr(dup), "dehydration-metadata")); err != nil {
			return nil, err
		}
	}
	if err := image.PublishGraphStream(files[0], args.FilteredGraphStream); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := f.dehydrationInfoLocked(b)
	if err != nil {
		return nil, err
	}
	return &BranchDehydrationExportInfo{Capture: *info}, nil
}

// Rehydrate reconstructs a dehydrated branch. The branch returns to its
// pre-dehydration running/paused state.
func (f *SandboxFamily) Rehydrate(id string) error {
	f.dehydrationMu.Lock()
	defer f.dehydrationMu.Unlock()
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchDehydrated || b.dehydration == nil {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not dehydrated", id, state)
	}
	d := b.dehydration
	b.state = BranchRehydrating
	f.stateChanged.Broadcast()
	f.mu.Unlock()

	filesystemBranch := b.filesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = f.volumeBranchID(b.id)
	}
	child, err := kernel.LoadDehydration(context.Background(), d.platform, d.image, kernel.DehydrationLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(f.clockMonotonicRaw), BranchID: filesystemBranch,
		Quarantined: b.quarantined, OverrideQuarantine: true,
		ConfigureNetwork: f.configureNetwork,
	})
	if err != nil {
		f.mu.Lock()
		b.state = BranchDehydrated
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return err
	}
	if err := child.StartPaused(); err != nil {
		child.Release()
		f.mu.Lock()
		b.state = BranchFailed
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return fmt.Errorf("starting rehydrated branch %q: %w", id, err)
	}
	newSessions, err := restoredExecSessions(child, d.execSessions)
	if err != nil {
		child.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
		child.Unpause()
		child.WaitExited()
		child.Release()
		f.mu.Lock()
		b.dehydration = nil
		b.state = BranchFailed
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return fmt.Errorf("restoring dehydrated exec sessions: %w", err)
	}
	var dog *watchdog.Watchdog
	if f.watchdogOpts != nil {
		dog = watchdog.New(child, *f.watchdogOpts)
		dog.Start()
	}
	f.mu.Lock()
	b.k = child
	b.watchdog = dog
	b.execSessions = newSessions
	b.dehydration = nil
	b.state = BranchPaused
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	if id == f.LoaderBranchID() && f.onLoaderKernelChanged != nil {
		f.onLoaderKernelChanged(child, dog)
	}
	for sessionID, session := range newSessions {
		f.watchExecSession(id, sessionID, session)
	}
	if d.resume {
		child.Unpause()
		if err := control.PostResume(child, nil); err != nil {
			child.Pause()
			return fmt.Errorf("resuming rehydrated branch %q: %w", id, err)
		}
		f.mu.Lock()
		b.state = kernelBranchState(child)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
	}
	return nil
}

// WaitDefaultExit waits for the currently hydrated default Kernel to exit.
// A Kernel released as part of dehydration is skipped rather than interpreted
// as container exit.
func (f *SandboxFamily) WaitDefaultExit() linux.WaitStatus {
	for {
		f.mu.Lock()
		b := f.branches[f.loaderBranchID]
		for b != nil && b.k == nil && (b.state == BranchDehydrating || b.state == BranchDehydrated || b.state == BranchRehydrating) {
			f.stateChanged.Wait()
			b = f.branches[f.loaderBranchID]
		}
		if b == nil || b.k == nil {
			f.mu.Unlock()
			return linux.WaitStatusExit(255)
		}
		k := b.k
		f.mu.Unlock()
		k.WaitExited()
		f.mu.Lock()
		if f.loaderBranchID != b.id || b.k != k || b.state == BranchDehydrating || b.state == BranchDehydrated || b.state == BranchRehydrating {
			f.mu.Unlock()
			continue
		}
		b.state = BranchExited
		// The Loader must remain alive while any independent hydrated branch
		// still has a live init. The branch in the bookkeeping role may exit
		// naturally, not only through a TTC stop command, so transfer here as a
		// final invariant rather than relying solely on PrepareStop.
		replacement := f.liveLoaderReplacementLocked(b.id)
		// So must it while any branch is dehydrated: its capture may exist
		// nowhere but this Sentry's memory, because ttcd makes captures durable
		// only when the last live branch on the node goes. ttcd does that when
		// it sees this exit and then deletes the runtime. A branch it
		// rehydrates instead becomes the loader.
		// And so must it while it reads a remote fork's import from its
		// provider: ttcd makes the import independent when it sees this exit,
		// through this Sentry, whose relay connections the import's files
		// come by. A remote fork's target whose init exited at once took the
		// Sentry with it, and the files it had not read with that: ttcd's
		// drain found no Sentry, "connection refused", and the branch's exit
		// was never recorded, nor its runtime released.
		for replacement == nil && f.loaderBranchID == b.id && (f.holdsDehydrationLocked() || f.holdsTransactionSourcesLocked()) {
			f.stateChanged.Wait()
			replacement = f.liveLoaderReplacementLocked(b.id)
		}
		if f.loaderBranchID != b.id {
			f.mu.Unlock()
			continue
		}
		if replacement == nil {
			status := k.GlobalInit().ExitStatus()
			f.mu.Unlock()
			return status
		}
		f.loaderBranchID = replacement.id
		loaderKernel := replacement.k
		loaderWatchdog := replacement.watchdog
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		if f.onLoaderKernelChanged != nil {
			f.onLoaderKernelChanged(loaderKernel, loaderWatchdog)
		}
	}
}

// holdsDehydrationLocked reports whether any branch is dehydrated, or on its
// way into or out of that state.
//
// Preconditions: f.mu is locked.
func (f *SandboxFamily) holdsDehydrationLocked() bool {
	for _, b := range f.branches {
		switch b.state {
		case BranchDehydrating, BranchDehydrated, BranchRehydrating:
			return true
		}
	}
	return false
}

// holdsTransactionSourcesLocked reports whether the Sentry's repositories
// read a remote fork's import from its provider.
//
// Preconditions: f.mu is locked.
func (f *SandboxFamily) holdsTransactionSourcesLocked() bool {
	return f.holdsTransactionSources != nil && f.holdsTransactionSources()
}

// TransactionSourcesChanged wakes what waits on the sources of remote forks'
// imports, once a drain has replaced them: see WaitDefaultExit.
func (f *SandboxFamily) TransactionSourcesChanged() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.stateChanged.Broadcast()
	f.mu.Unlock()
}

// liveLoaderReplacementLocked returns an established hydrated branch whose
// init has not already exited. f.mu must be held. Parent relationships are
// intentionally not consulted because they have no lifecycle semantics.
func (f *SandboxFamily) liveLoaderReplacementLocked(exclude string) *sandboxBranch {
	ids := make([]string, 0, len(f.branches)-1)
	for candidateID, candidate := range f.branches {
		if candidateID == exclude || candidate.k == nil || candidate.state == BranchCreating || candidate.state == BranchDestroying || candidate.state == BranchFailed || candidate.state == BranchExited {
			continue
		}
		ids = append(ids, candidateID)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	return f.branches[ids[0]]
}

// pageLoadHeirsLocked returns the Kernels of the running and paused branches
// other than exclude that read one of files, to keep the loaders exclude's
// capture keeps alive. Each may read pages the loaders have yet to load
// without owning them; one alone, dehydrated later, would keep only those its
// own pages need.
func (f *SandboxFamily) pageLoadHeirsLocked(exclude string, files []*pgalloc.MemoryFile) []*kernel.Kernel {
	var heirs []*kernel.Kernel
	for id, b := range f.branches {
		if id != exclude && b.k != nil && (b.state == BranchRunning || b.state == BranchPaused) &&
			slices.Contains(files, b.k.MemoryFile()) {
			heirs = append(heirs, b.k)
		}
	}
	return heirs
}

// PrepareStop transfers the Loader bookkeeping role away from id when another
// branch will survive. Branch ancestry and visible names are unaffected.
func (f *SandboxFamily) PrepareStop(id string) error {
	f.dehydrationMu.Lock()
	defer f.dehydrationMu.Unlock()
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if id != f.loaderBranchID {
		f.mu.Unlock()
		return nil
	}
	replacement := f.liveLoaderReplacementLocked(id)
	if replacement == nil {
		f.mu.Unlock()
		return nil
	}
	f.loaderBranchID = replacement.id
	loaderKernel := replacement.k
	loaderWatchdog := replacement.watchdog
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	if f.onLoaderKernelChanged != nil {
		f.onLoaderKernelChanged(loaderKernel, loaderWatchdog)
	}
	return nil
}

func (f *SandboxFamily) reclaimReleasedFilesystem(branch string) error {
	if f.reclaimFilesystemBranch != nil {
		return f.reclaimFilesystemBranch(branch)
	}
	if f.forgetFilesystemBranch != nil {
		f.forgetFilesystemBranch(branch)
	}
	return nil
}

// Destroy destroys a branch and removes it from the family. If id currently
// occupies the loader bookkeeping role, that role moves to an established
// peer first. This role transfer has no guest-visible or lineage semantics.
func (f *SandboxFamily) Destroy(id string) error {
	return f.destroy(id, false)
}

// DestroyAndReclaim destroys a branch and releases its persistent filesystem
// roots after all Kernel and open-file references have been released. It must
// only be used for logical branch deletion, never stop or migration.
func (f *SandboxFamily) DestroyAndReclaim(id string) error {
	return f.destroy(id, true)
}

func (f *SandboxFamily) destroy(id string, reclaimFilesystem bool) error {
	f.dehydrationMu.Lock()
	defer f.dehydrationMu.Unlock()
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if b.state == BranchCreating || b.state == BranchDestroying {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return fmt.Errorf("branch %q is %s", id, state)
	}
	// Stable page/version chunks reused by another imported branch may still be
	// supplied by this branch's lazy reader. Transfer lifecycle ownership to a
	// surviving peer instead of materializing memory on the removal path.
	var remoteSourceTarget *kernel.Kernel
	if b.k != nil {
		familyMF := b.k.MemoryFile()
		for candidateID, candidate := range f.branches {
			if candidateID != id && candidate.k != nil && candidate.k.MemoryFile() == familyMF {
				remoteSourceTarget = candidate.k
				break
			}
		}
	}
	var loaderKernel *kernel.Kernel
	var loaderWatchdog *watchdog.Watchdog
	loaderChanged := id == f.loaderBranchID
	if loaderChanged {
		// A live hydrated branch is the only useful Loader bookkeeping target
		// while one exists. In particular, an alphabetically earlier exited
		// Kernel must not displace a live peer. Process-free and exited peers are
		// only fallbacks that allow the final Sentry teardown path to complete.
		replacement := f.liveLoaderReplacementLocked(id)
		ids := make([]string, 0, len(f.branches)-1)
		for candidateID, candidate := range f.branches {
			if candidateID == id || candidate.state == BranchCreating || candidate.state == BranchDestroying || candidate.state == BranchFailed {
				continue
			}
			ids = append(ids, candidateID)
		}
		sort.Strings(ids)
		if replacement == nil {
			// Prefer any process-backed fallback over a process-free one. All
			// live process-backed candidates were handled above, so these are
			// relevant only while the family itself is being torn down.
			for _, candidateID := range ids {
				candidate := f.branches[candidateID]
				if replacement == nil || (replacement.k == nil && candidate.k != nil) {
					replacement = candidate
				}
			}
		}
		if replacement == nil {
			f.mu.Unlock()
			b.opMu.Unlock()
			return fmt.Errorf("branch %q is the last branch in its container family", id)
		}
		f.loaderBranchID = replacement.id
		loaderKernel = replacement.k
		loaderWatchdog = replacement.watchdog
	}
	// Parent relationships are presentation-only. Splice this node out so the
	// tree never depends on a tombstone and branch names become immediately
	// reusable after deletion.
	for childID, child := range f.branches {
		if childID != id && child.parentID == id {
			child.parentID = b.parentID
		}
	}
	b.state = BranchDestroying
	image := b.dehydration
	if image != nil {
		b.dehydration = nil
	}
	var paused bool
	if b.k != nil {
		paused = b.k.IsPaused()
	}
	dog := b.watchdog
	f.mu.Unlock()
	if loaderChanged && f.onLoaderKernelChanged != nil {
		f.onLoaderKernelChanged(loaderKernel, loaderWatchdog)
	}
	f.cancelConsoleWaits(b)
	b.opMu.Unlock()
	if image != nil {
		// A capture keeps loading what its branch had not read of the image
		// it was restored from, and closing it stops that loader unless the
		// live peers still reading the same MemoryFile keep it, as one takes
		// a live branch's loaders below: a peer may read pages the loader
		// has yet to load without being one of its owners. Free the
		// chunks only the capture used, as a live branch's destruction does
		// below: a later restore of the same image into the family would
		// otherwise reuse them, waiting on the stopped loader for pages it
		// never loaded.
		files := image.image.MemoryFiles()
		f.mu.Lock()
		heirs := f.pageLoadHeirsLocked(id, files)
		f.mu.Unlock()
		image.image.LendPageLoads(heirs...)
		err := image.image.Close()
		for _, mf := range files {
			mf.ReclaimUnusedSparseChunks()
			mf.Destroy()
		}
		if reclaimFilesystem {
			err = errors.Join(err, f.reclaimReleasedFilesystem(b.filesystemBranch))
		} else if f.forgetFilesystemBranch != nil {
			f.forgetFilesystemBranch(b.filesystemBranch)
		}
		f.mu.Lock()
		delete(f.branches, id)
		delete(f.distributedHandoffs, id)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return err
	}
	if b.k == nil {
		var releaseErr error
		if reclaimFilesystem {
			releaseErr = f.reclaimReleasedFilesystem(b.filesystemBranch)
		} else if f.forgetFilesystemBranch != nil {
			f.forgetFilesystemBranch(b.filesystemBranch)
		}
		f.mu.Lock()
		delete(f.branches, id)
		delete(f.distributedHandoffs, id)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return releaseErr
	}

	if dog != nil {
		dog.Stop()
	}
	if reclaimFilesystem && f.discardFilesystemBranch != nil {
		f.discardFilesystemBranch(b.filesystemBranch)
	}
	b.k.TransferDistributedForkMemorySources(remoteSourceTarget)
	b.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	if paused {
		b.k.Unpause()
	}
	b.k.WaitExited()
	b.controlOps.Wait()
	familyMF := b.k.MemoryFile()
	familyMF.IncOwner()
	b.k.Release()
	familyMF.ReclaimUnusedSparseChunks()
	familyMF.Destroy()
	var releaseErr error
	if reclaimFilesystem {
		releaseErr = f.reclaimReleasedFilesystem(b.filesystemBranch)
	} else if f.forgetFilesystemBranch != nil {
		f.forgetFilesystemBranch(b.filesystemBranch)
	}

	f.mu.Lock()
	delete(f.branches, id)
	delete(f.distributedHandoffs, id)
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	return releaseErr
}

// Processes returns processes from one branch's kernel task graph.
func (f *SandboxFamily) Processes(id, cid string, out *[]*control.Process) error {
	b, done, err := f.beginControl(id)
	if err != nil {
		return err
	}
	defer done()
	return control.Processes(b.k, cid, out)
}

// RootfsSize returns branch-local logical writable-layer usage.
func (f *SandboxFamily) RootfsSize(id, cid string, out *control.RootfsSize) error {
	b, done, err := f.beginControl(id)
	if err != nil {
		return err
	}
	defer done()
	return control.RootfsUpperLayerSize(b.k, cid, out)
}

// residentRange is a range of resident pages of one MemoryFile, which it names
// by its index in a residentFiles.
type residentRange struct {
	file       int
	start, end uint64
}

// residentFiles numbers the MemoryFiles whose pages an accounting counts.
type residentFiles struct {
	files []*pgalloc.MemoryFile
	index map[*pgalloc.MemoryFile]int
}

// mergeResidentRanges returns ranges sorted by file and offset, with those that
// overlap or touch merged: the pages that one branch holds, each once. It
// numbers the ranges' files in files.
func mergeResidentRanges(ranges []pgalloc.CapturedMemoryRange, files *residentFiles) []residentRange {
	result := make([]residentRange, 0, len(ranges))
	for _, r := range ranges {
		if r.Range.Length() == 0 {
			continue
		}
		index, ok := files.index[r.MemoryFile]
		if !ok {
			index = len(files.files)
			files.files = append(files.files, r.MemoryFile)
			files.index[r.MemoryFile] = index
		}
		result = append(result, residentRange{file: index, start: r.Range.Start, end: r.Range.End})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].file != result[j].file {
			return result[i].file < result[j].file
		}
		return result[i].start < result[j].start
	})
	merged := result[:0]
	for _, r := range result {
		if last := len(merged) - 1; last >= 0 && merged[last].file == r.file && r.start <= merged[last].end {
			merged[last].end = max(merged[last].end, r.end)
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// residentMemoryAccounts accounts the resident pages of the branches named by
// branchIDs, whose merged ranges (see mergeResidentRanges) branchRanges holds
// in the same order. It returns, for each branch, the bytes of the pages that
// it alone holds and of those it shares with another branch; the bytes of the
// distinct pages of all of them; and the shared pages' weighted hyperedges, one
// per set of branches that share pages, at most limit of them, and whether
// others were left out, and their bytes. Within one MemoryFile a page is at one
// offset, so that branches share a page exactly where their ranges overlap: the
// cost follows the number of ranges, not of pages.
func residentMemoryAccounts(branchIDs []string, branchRanges [][]residentRange, limit int) (private, shared []uint64, unique uint64, edges []ResidentMemorySharingEdge, truncated bool, omitted uint64) {
	if len(branchIDs) != len(branchRanges) {
		panic("resident memory branch identity/range count mismatch")
	}
	type event struct {
		file   int
		offset uint64
		branch int
		start  bool
	}
	var events []event
	for branch, ranges := range branchRanges {
		for _, r := range ranges {
			events = append(events,
				event{file: r.file, offset: r.start, branch: branch, start: true},
				event{file: r.file, offset: r.end, branch: branch})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].file != events[j].file {
			return events[i].file < events[j].file
		}
		return events[i].offset < events[j].offset
	})
	private = make([]uint64, len(branchIDs))
	shared = make([]uint64, len(branchIDs))
	// covering is the set of branches that hold the pages from the current
	// offset on, one bit each.
	covering := make([]uint64, (len(branchIDs)+63)/64)
	count := 0
	weights := make(map[string]uint64)
	for i := 0; i < len(events); {
		at := events[i]
		for ; i < len(events) && events[i].file == at.file && events[i].offset == at.offset; i++ {
			word, bit := events[i].branch/64, uint64(1)<<(events[i].branch%64)
			if events[i].start {
				covering[word] |= bit
				count++
			} else {
				covering[word] &^= bit
				count--
			}
		}
		if count == 0 || i == len(events) || events[i].file != at.file {
			continue
		}
		length := events[i].offset - at.offset
		unique += length
		for word, set := range covering {
			for ; set != 0; set &= set - 1 {
				branch := word*64 + bits.TrailingZeros64(set)
				if count == 1 {
					private[branch] += length
				} else {
					shared[branch] += length
				}
			}
		}
		if count > 1 {
			key := make([]byte, 0, 8*len(covering))
			for _, set := range covering {
				key = binary.LittleEndian.AppendUint64(key, set)
			}
			weights[string(key)] += length
		}
	}

	type weightedMembership struct {
		sortKey  string
		branches []string
		bytes    uint64
	}
	memberships := make([]weightedMembership, 0, len(weights))
	for key, bytes := range weights {
		var builder strings.Builder
		var members []string
		for word := 0; word < len(key)/8; word++ {
			for set := binary.LittleEndian.Uint64([]byte(key[8*word:])); set != 0; set &= set - 1 {
				id := branchIDs[word*64+bits.TrailingZeros64(set)]
				builder.WriteByte(0)
				builder.WriteString(id)
				members = append(members, id)
			}
		}
		memberships = append(memberships, weightedMembership{sortKey: builder.String(), branches: members, bytes: bytes})
	}
	sort.Slice(memberships, func(i, j int) bool { return memberships[i].sortKey < memberships[j].sortKey })
	limit = max(limit, 0)
	if truncated = len(memberships) > limit; truncated {
		for _, membership := range memberships[limit:] {
			omitted += membership.bytes
		}
		memberships = memberships[:limit]
	}
	edges = make([]ResidentMemorySharingEdge, 0, len(memberships))
	for index, membership := range memberships {
		edges = append(edges, ResidentMemorySharingEdge{
			ID:       fmt.Sprintf("resident-share-%04d", index),
			Bytes:    membership.bytes,
			Branches: membership.branches,
		})
	}
	return private, shared, unique, edges, truncated, omitted
}

// residentVersionRun is a run of consecutive versions of one allocator.
type residentVersionRun struct {
	allocator pgalloc.LogicalAllocatorID
	first     uint64
	pages     uint64
}

// residentMemoryVersionRuns returns the versions of the pages in branchRanges,
// as runs, branch by branch.
func residentMemoryVersionRuns(files []*pgalloc.MemoryFile, branchRanges [][]residentRange) [][]residentVersionRun {
	result := make([][]residentVersionRun, len(branchRanges))
	for branch, ranges := range branchRanges {
		for _, r := range ranges {
			for _, run := range files[r.file].LogicalPageRanges(memmap.FileRange{Start: r.start, End: r.end}) {
				result[branch] = append(result[branch], residentVersionRun{
					allocator: run.FirstVersion.Allocator,
					first:     run.FirstVersion.Sequence,
					pages:     run.Range.Length() / hostarch.PageSize,
				})
			}
		}
	}
	return result
}

// residentMemoryVersionChunks returns the versions in branchRuns as bitmaps of
// 256 versions each, by branch, allocator and chunk: a fixed cost per 256
// versions however concurrent copies interleave their sequences, which keeps
// the planner's response bounded. A version that a branch holds twice, as two
// MemoryFiles may, is one bit.
func residentMemoryVersionChunks(branchIDs []string, branchRuns [][]residentVersionRun) []ResidentMemoryVersionChunk {
	if len(branchIDs) != len(branchRuns) {
		panic("resident memory branch identity/run count mismatch")
	}
	type chunkKey struct {
		allocator pgalloc.LogicalAllocatorID
		sequence  uint64
	}
	var result []ResidentMemoryVersionChunk
	for index, runs := range branchRuns {
		chunks := make(map[chunkKey]*[32]byte)
		for _, run := range runs {
			for sequence, end := run.first, run.first+run.pages; sequence < end; {
				key := chunkKey{allocator: run.allocator, sequence: sequence / 256}
				bitmap := chunks[key]
				if bitmap == nil {
					bitmap = new([32]byte)
					chunks[key] = bitmap
				}
				for chunkEnd := min(end, (key.sequence+1)*256); sequence < chunkEnd; sequence++ {
					slot := sequence % 256
					bitmap[slot/8] |= byte(1 << (slot % 8))
				}
			}
		}
		keys := make([]chunkKey, 0, len(chunks))
		for key := range chunks {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].allocator != keys[j].allocator {
				return string(keys[i].allocator[:]) < string(keys[j].allocator[:])
			}
			return keys[i].sequence < keys[j].sequence
		})
		for _, key := range keys {
			result = append(result, ResidentMemoryVersionChunk{
				Branch: branchIDs[index], Allocator: hex.EncodeToString(key.allocator[:]),
				Chunk: key.sequence, Bitmap: hex.EncodeToString(chunks[key][:]),
			})
		}
	}
	return result
}

// FamilyResidentMemoryArgs selects what ResidentMemory accounts.
type FamilyResidentMemoryArgs struct {
	// Versions asks for VersionChunks, which the cluster's placement planner
	// matches across Sentries. They cost a walk of the version of every
	// resident page, which the rest of the accounting does not.
	Versions bool
}

// ResidentMemory accounts resident guest pages across live branches. This
// includes task mappings and tmpfs/shmem file contents, including unmapped
// contents retained by /dev/shm files. It deliberately excludes the Sentry Go
// heap and host-backed filesystem page-cache objects. It works on the ranges of
// the MemoryFiles that hold the pages, as their address spaces and files hold
// them, rather than page by page: a branch with 70 GB resident in homes has a
// few hundred.
func (f *SandboxFamily) ResidentMemory(args *FamilyResidentMemoryArgs, out *FamilyResidentMemory) error {
	f.dehydrationMu.Lock()
	defer f.dehydrationMu.Unlock()

	f.mu.Lock()
	ids := make([]string, 0, len(f.branches))
	for id := range f.branches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	branches := make([]*sandboxBranch, 0, len(ids))
	for _, id := range ids {
		branches = append(branches, f.branches[id])
	}
	f.mu.Unlock()
	held := make([]*sandboxBranch, 0, len(ids))
	for _, branch := range branches {
		branch.opMu.Lock()
		f.mu.Lock()
		if branch.k != nil && branch.state != BranchCreating && branch.state != BranchDestroying && branch.state != BranchFailed {
			branch.controlOps.Add(1)
			branch.compactionOps.Add(1)
			held = append(held, branch)
		}
		f.mu.Unlock()
		branch.opMu.Unlock()
	}
	defer func() {
		for _, branch := range held {
			branch.compactionOps.Done()
			branch.controlOps.Done()
		}
	}()

	files := residentFiles{index: make(map[*pgalloc.MemoryFile]int)}
	heldIDs := make([]string, 0, len(held))
	heldRanges := make([][]residentRange, 0, len(held))
	for _, branch := range held {
		ranges, err := branch.k.ResidentFileRanges()
		if err != nil {
			return fmt.Errorf("accounting resident memory for branch %q: %w", branch.id, err)
		}
		heldIDs = append(heldIDs, branch.id)
		heldRanges = append(heldRanges, mergeResidentRanges(ranges, &files))
	}
	private, shared, unique, edges, truncated, omitted := residentMemoryAccounts(heldIDs, heldRanges, maxResidentMemorySharingEdges)
	out.SharingEdges, out.SharingEdgesTruncated, out.SharingEdgesOmittedBytes = edges, truncated, omitted
	if args != nil && args.Versions {
		out.VersionChunks = residentMemoryVersionChunks(heldIDs, residentMemoryVersionRuns(files.files, heldRanges))
	}
	out.Branches = make([]BranchResidentMemory, 0, len(ids))
	for _, id := range ids {
		f.mu.Lock()
		state := f.branches[id].state
		f.mu.Unlock()
		account := BranchResidentMemory{ID: id, State: string(state)}
		if index := slices.Index(heldIDs, id); index >= 0 {
			account.PrivateBytes, account.SharedBytes = private[index], shared[index]
			account.TotalBytes = account.PrivateBytes + account.SharedBytes
		}
		out.Branches = append(out.Branches, account)
	}
	out.UniqueBytes = unique
	return nil
}

// Signal sends a signal within one branch. Foreground process-group delivery
// resolves the controlling TTY from branch-local cloned Sentry state.
func (f *SandboxFamily) Signal(id, cid string, pid, signo int32, mode SignalDeliveryMode) error {
	b, done, err := f.beginControl(id)
	if err != nil {
		return err
	}
	defer done()

	k := b.k
	switch mode {
	case DeliverToProcess:
		tg, err := branchThreadGroup(k, cid, kernel.ThreadID(pid))
		if err != nil {
			return err
		}
		return k.SendExternalSignalThreadGroup(tg, &linux.SignalInfo{Signo: signo})
	case DeliverToAllProcesses:
		if pid != 0 {
			return fmt.Errorf("PID (%d) cannot be set when signaling all processes", pid)
		}
		if _, err := branchThreadGroup(k, cid, 0); err != nil {
			return err
		}
		k.Pause()
		defer k.Unpause()
		return k.SendContainerSignal(cid, &linux.SignalInfo{Signo: signo})
	case DeliverToProcessGroup:
		if pid <= 0 {
			return fmt.Errorf("PGID must be positive")
		}
		pgid := kernel.ProcessGroupID(pid)
		pg := k.RootPIDNamespace().ProcessGroupWithID(pgid)
		if pg == nil {
			return fmt.Errorf("no such process group with PGID %d", pgid)
		}
		for _, tg := range k.TaskSet().Root.ThreadGroups() {
			if tg.ProcessGroup() == pg && tg.Leader().ContainerID() == cid {
				return k.SendExternalSignalProcessGroup(pg, &linux.SignalInfo{Signo: signo})
			}
		}
		return fmt.Errorf("process group %d does not belong to container %q", pgid, cid)
	case DeliverToForegroundProcessGroup:
		tg, err := branchThreadGroup(k, cid, kernel.ThreadID(pid))
		if err != nil {
			return err
		}
		tty := tg.TTY()
		if tty == nil {
			return fmt.Errorf("no TTY attached")
		}
		ttyTG := tty.ThreadGroup()
		if ttyTG == nil {
			return k.SendExternalSignalThreadGroup(tg, &linux.SignalInfo{Signo: signo})
		}
		pg, _ := ttyTG.ForegroundProcessGroup(tty)
		if pg == nil {
			return k.SendExternalSignalThreadGroup(tg, &linux.SignalInfo{Signo: signo})
		}
		return k.SendExternalSignalProcessGroup(pg, &linux.SignalInfo{Signo: signo})
	default:
		return fmt.Errorf("unknown signal delivery mode %v", mode)
	}
}

// Wait waits for a process in one branch and returns its wait status. PID 0
// selects the container's init process.
func (f *SandboxFamily) Wait(id, cid string, pid int32) (uint32, error) {
	b, done, err := f.beginWaitControl(id)
	if err != nil {
		return 0, err
	}
	defer done()
	if id == "" || id == f.LoaderBranchID() {
		<-f.defaultStarted
	}
	tg, err := branchThreadGroup(b.k, cid, kernel.ThreadID(pid))
	if err != nil {
		return 0, err
	}
	tg.WaitExited()
	status := uint32(tg.ExitStatus())
	if pid == 0 {
		f.mu.Lock()
		if current := f.branches[b.id]; current == b && (b.state == BranchRunning || b.state == BranchPaused) {
			b.state = BranchExited
			f.stateChanged.Broadcast()
		}
		f.mu.Unlock()
	}
	return status, nil
}

// beginWaitControl acquires a Kernel-lifetime lease for process waiting.
// Unlike ordinary live control operations, waiting remains valid after init
// exits because Kernel retains GlobalInit and its terminal wait status until
// branch destruction.
func (f *SandboxFamily) beginWaitControl(id string) (*sandboxBranch, func(), error) {
	return f.beginLifetimeControl(id, true)
}

func (f *SandboxFamily) execSession(args *ExecSessionArgs) (*sandboxBranch, *execSession, func(), error) {
	if args == nil || args.ExecID == "" {
		return nil, nil, nil, fmt.Errorf("exec session ID must not be empty")
	}
	b, done, err := f.beginControl(args.BranchID)
	if err != nil {
		return nil, nil, nil, err
	}
	f.mu.Lock()
	session := b.execSessions[args.ExecID]
	f.mu.Unlock()
	if session == nil {
		done()
		return nil, nil, nil, fmt.Errorf("exec session %q does not exist in branch %q", args.ExecID, args.BranchID)
	}
	return b, session, done, nil
}

// ExecSessions returns stable exec-session metadata for one branch.
func (f *SandboxFamily) ExecSessions(args *ExecSessionsArgs) ([]ExecSessionInfo, error) {
	if args == nil {
		return nil, fmt.Errorf("missing exec-session list arguments")
	}
	b, done, err := f.beginControl(args.BranchID)
	if err != nil {
		return nil, err
	}
	defer done()
	f.mu.Lock()
	out := make([]ExecSessionInfo, 0, len(b.execSessions))
	for _, session := range b.execSessions {
		if args.CID == "" || session.info.ContainerID == args.CID {
			out = append(out, cloneExecSessionInfo(session.info))
		}
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// WaitExecSession waits for one exec process and retains its exit status.
func (f *SandboxFamily) WaitExecSession(args *ExecSessionArgs, cancel <-chan struct{}) (uint32, error) {
	if args == nil || args.ExecID == "" {
		return 0, fmt.Errorf("exec session ID must not be empty")
	}
	b, done, err := f.beginWaitControl(args.BranchID)
	if err != nil {
		return 0, err
	}
	defer done()
	f.mu.Lock()
	session := b.execSessions[args.ExecID]
	f.mu.Unlock()
	if session == nil {
		return 0, fmt.Errorf("exec session %q does not exist in branch %q", args.ExecID, args.BranchID)
	}
	f.mu.Lock()
	if session.info.State == ExecSessionExited && session.info.ExitStatus != nil {
		status := *session.info.ExitStatus
		// The background watcher can publish exit before a foreground runsc exec
		// reaches this waiter. Non-retained management sessions must be consumed in
		// that fast path too; otherwise every paused ttc cp leaves a completed
		// ttc-cp-* session in the branch forever.
		if !session.retain && b.execSessions[args.ExecID] == session {
			delete(b.execSessions, args.ExecID)
		}
		f.mu.Unlock()
		return status, nil
	}
	f.mu.Unlock()
	if session.tg == nil {
		return 0, fmt.Errorf("exec session %q is not published", args.ExecID)
	}
	exited := make(chan struct{})
	go func() {
		session.tg.WaitExited()
		close(exited)
	}()
	select {
	case <-exited:
	case <-cancel:
		return 0, ErrExecWaitCancelled
	}
	if session.managementDone != nil {
		select {
		case <-session.managementDone:
		case <-cancel:
			return 0, ErrExecWaitCancelled
		}
	}
	status := uint32(session.tg.ExitStatus())
	f.mu.Lock()
	session.info.State = ExecSessionExited
	session.info.ExitStatus = &status
	if !session.retain && b.execSessions[args.ExecID] == session {
		delete(b.execSessions, args.ExecID)
	}
	f.mu.Unlock()
	return status, nil
}

// SignalExecSession signals one exec process by stable session ID.
func (f *SandboxFamily) SignalExecSession(args *ExecSessionArgs) error {
	b, session, done, err := f.execSession(args)
	if err != nil {
		return err
	}
	defer done()
	f.mu.Lock()
	exited := session.info.State == ExecSessionExited
	f.mu.Unlock()
	if exited {
		return fmt.Errorf("exec session %q has exited", args.ExecID)
	}
	if session.tg == nil {
		return fmt.Errorf("exec session %q is not published", args.ExecID)
	}
	return b.k.SendExternalSignalThreadGroup(session.tg, &linux.SignalInfo{Signo: args.Signo})
}

// PushConsole pushes a process's console to output, in the background: the
// output of its streams as timestamped console frames, as it comes, and once
// both have ended, a ConsolePushExit record, or a ConsolePushError record if
// the push fails; then it closes output. The push holds the Kernel's lifetime
// from before PushConsole returns, which is before OCI start may run the init
// of a new container, until it ends. Unlike a console follower's reads, it
// holds no thread while the console is quiet.
func (f *SandboxFamily) PushConsole(id, cid, execID, consoleID string, output io.WriteCloser) error {
	// The push waits on Sentry-side console buffers rather than application
	// memory, so a quiet console must not hold off platform memory compaction.
	b, done, err := f.beginLifetimeControl(id, false)
	if err != nil {
		return err
	}
	go func() {
		defer done()
		defer output.Close()
		status, oomKilled, err := f.pushConsole(b, id, cid, execID, consoleID, output)
		if err != nil {
			_ = writeConsoleFrame(output, ConsolePushError, []byte(err.Error()))
			return
		}
		if oomKilled {
			_ = writeConsoleFrame(output, ConsolePushOOMKilled, nil)
		}
		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], status)
		_ = writeConsoleFrame(output, ConsolePushExit, encoded[:])
	}()
	return nil
}

func (f *SandboxFamily) pushConsole(b *sandboxBranch, id, cid, execID, consoleID string, output io.Writer) (uint32, bool, error) {
	var tg *kernel.ThreadGroup
	if execID == "" {
		if id == "" || id == f.LoaderBranchID() {
			<-f.defaultStarted
		}
		var err error
		if tg, err = branchThreadGroup(b.k, cid, 0); err != nil {
			return 0, false, err
		}
	} else {
		f.mu.Lock()
		session := b.execSessions[execID]
		if session != nil && session.info.ContainerID == cid && session.info.ConsoleID == consoleID {
			tg = session.tg
		}
		f.mu.Unlock()
		if tg == nil {
			return 0, false, fmt.Errorf("exec session %q with console %q does not exist in branch %q", execID, consoleID, id)
		}
	}
	exited := make(chan struct{})
	go func() {
		tg.WaitExited()
		close(exited)
	}()
	streams := [...]struct {
		kernel kernel.InternalConsoleStream
		frame  byte
		eof    bool
	}{
		{kernel: kernel.InternalConsoleStdout, frame: ttcconsole.StreamStdout},
		{kernel: kernel.InternalConsoleStderr, frame: ttcconsole.StreamStderr},
	}
	// drain pushes what the console holds. Once the process has ended,
	// nothing may be left to wait for.
	drain := func(final bool) (bool, error) {
		progress := false
		for i := range streams {
			stream := &streams[i]
			for !stream.eof {
				b.opMu.Lock()
				data, eof, wouldBlock, err := b.k.ReadInternalConsole(b.k.SupervisorContext(), consoleID, stream.kernel, 65536)
				b.opMu.Unlock()
				if err != nil {
					return false, fmt.Errorf("reading internal console stream %d: %w", stream.kernel, err)
				}
				if len(data) != 0 {
					if err := ttcconsole.WriteTimestampedFrame(output, stream.frame, time.Now().UTC(), data); err != nil {
						return false, fmt.Errorf("pushing internal console stream %d: %w", stream.kernel, err)
					}
					progress = true
				}
				if eof {
					if err := ttcconsole.WriteTimestampedFrame(output, stream.frame, time.Now().UTC(), nil); err != nil {
						return false, fmt.Errorf("pushing internal console EOF %d: %w", stream.kernel, err)
					}
					stream.eof = true
					progress = true
				}
				if wouldBlock {
					if final {
						return false, fmt.Errorf("internal console stream %d would block after process exit", stream.kernel)
					}
					break
				}
				if len(data) == 0 && !eof {
					break
				}
			}
		}
		return progress, nil
	}
	for !streams[0].eof || !streams[1].eof {
		progress, err := drain(false)
		if err != nil {
			return 0, false, err
		}
		if progress {
			continue
		}
		if closed(exited) {
			break
		}
		if err := f.waitConsolePush(b, consoleID, kernel.InternalConsoleReadiness{Stdout: !streams[0].eof, Stderr: !streams[1].eof}, exited); err != nil {
			return 0, false, err
		}
	}
	<-exited
	status := uint32(tg.ExitStatus())
	f.mu.Lock()
	if execID == "" {
		if current := f.branches[b.id]; current == b && (b.state == BranchRunning || b.state == BranchPaused) {
			b.state = BranchExited
			f.stateChanged.Broadcast()
		}
	} else if session := b.execSessions[execID]; session != nil && session.tg == tg {
		session.info.State = ExecSessionExited
		session.info.ExitStatus = &status
	}
	f.mu.Unlock()
	if execID == "" {
		b.k.WaitExited()
	}
	for !streams[0].eof || !streams[1].eof {
		if _, err := drain(true); err != nil {
			return 0, false, err
		}
	}
	return status, tg.OOMKilled(), nil
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// waitConsolePush waits until the console of a push is ready, its process
// has exited, or a lifecycle operation needs the branch, which cancels the
// wait for the push to take again once it is done.
func (f *SandboxFamily) waitConsolePush(b *sandboxBranch, consoleID string, want kernel.InternalConsoleReadiness, exited <-chan struct{}) error {
	lease, finish, err := f.beginBranchConsoleWait(b)
	if err != nil {
		// The branch is being dehydrated or removed, which kills its
		// Kernel and so ends the process.
		<-exited
		return nil
	}
	defer finish()
	cancel := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lease.cancel:
		case <-exited:
		case <-stop:
			return
		}
		close(cancel)
	}()
	_, _, err = b.k.WaitInternalConsole(b.k.SupervisorContext(), consoleID, want, MaxConsoleWait, cancel)
	return err
}

func writeConsoleFrame(writer io.Writer, stream byte, data []byte) error {
	encoded := make([]byte, 8+len(data))
	encoded[0] = stream
	binary.BigEndian.PutUint32(encoded[4:8], uint32(len(data)))
	copy(encoded[8:], data)
	for len(encoded) != 0 {
		n, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(encoded) {
			return io.ErrShortWrite
		}
		encoded = encoded[n:]
	}
	return nil
}

// ConsoleRead performs one nonblocking read while serializing against fork,
// pause, resume, and destroy of the selected branch.
func (f *SandboxFamily) ConsoleRead(args *ConsoleReadArgs, out *ConsoleReadResult) error {
	if args == nil || out == nil {
		return fmt.Errorf("missing internal console read arguments or result")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	data, eof, wouldBlock, err := b.k.ReadInternalConsole(b.k.SupervisorContext(), args.ConsoleID, args.Stream, args.MaxBytes)
	if err != nil {
		return err
	}
	*out = ConsoleReadResult{Data: data, EOF: eof, WouldBlock: wouldBlock}
	return nil
}

// ConsoleObservation returns one point-in-time observation while serializing
// against branch replacement. It does not consume terminal input or output.
func (f *SandboxFamily) ConsoleObservation(args *ConsoleObservationArgs, out *kernel.InternalConsoleObservation) error {
	if args == nil || out == nil {
		return fmt.Errorf("missing internal console observation arguments or result")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	observation, err := b.k.ObserveInternalConsole(b.k.SupervisorContext(), args.ConsoleID)
	if err != nil {
		return err
	}
	*out = observation
	return nil
}

// ConsoleWatch streams Sentry-driven observations while holding a cancellable
// Kernel-lifetime lease. Lifecycle operations cancel and join the transient
// waiter before replacing or destroying the branch.
func (f *SandboxFamily) ConsoleWatch(args *ConsoleObservationArgs, emit func(kernel.InternalConsoleObservation) error) error {
	if args == nil || emit == nil {
		return fmt.Errorf("missing internal console watch arguments or emitter")
	}
	for {
		b, lease, done, err := f.beginConsoleWait(args.BranchID)
		if err != nil {
			return err
		}
		err = b.k.WatchInternalConsole(b.k.SupervisorContext(), args.ConsoleID, 10*time.Millisecond, time.Second, lease.cancel, emit)
		done()
		if err != nil {
			return err
		}
		// A nil result means a lifecycle operation cancelled the transient
		// registration. Reacquire after the operation releases opMu so a watch
		// of a surviving fork source remains connected. Dehydration and removal
		// fail beginConsoleWait above and terminate the stream.
	}
}

// ConsoleWait blocks on Sentry endpoint readiness without retaining the branch
// operation mutex, so pause, resume, and live fork remain independent. The
// control lease keeps the selected Kernel alive until the bounded wait returns.
func (f *SandboxFamily) ConsoleWait(args *ConsoleWaitArgs, out *ConsoleWaitResult) error {
	if args == nil || out == nil {
		return fmt.Errorf("missing internal console wait arguments or result")
	}
	if args.Timeout < 0 || args.Timeout > MaxConsoleWait {
		return fmt.Errorf("internal console wait timeout %s is outside 0..%s", args.Timeout, MaxConsoleWait)
	}
	b, lease, done, err := f.beginConsoleWait(args.BranchID)
	if err != nil {
		return err
	}
	defer done()
	ready, timedOut, err := b.k.WaitInternalConsole(b.k.SupervisorContext(), args.ConsoleID, args.Want, args.Timeout, lease.cancel)
	if err != nil {
		return err
	}
	*out = ConsoleWaitResult{Ready: ready, TimedOut: timedOut}
	return nil
}

// ConsoleWrite performs one nonblocking write while serializing against fork,
// pause, resume, and destroy of the selected branch.
func (f *SandboxFamily) ConsoleWrite(args *ConsoleWriteArgs, out *ConsoleWriteResult) error {
	if args == nil || out == nil {
		return fmt.Errorf("missing internal console write arguments or result")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	n, wouldBlock, err := b.k.WriteInternalConsole(b.k.SupervisorContext(), args.ConsoleID, args.Data)
	if err != nil {
		return err
	}
	*out = ConsoleWriteResult{N: n, WouldBlock: wouldBlock}
	return nil
}

// PullConsoleInput copies input into a branch-local internal console's stdin,
// as writes and waits of the console's control would, until input ends, when
// it closes the console's stdin, or the console or its branch does. A read of
// input holds nothing of the branch, which may go meanwhile: the copy then
// ends at its next write.
func (f *SandboxFamily) PullConsoleInput(id, consoleID string, input io.ReadCloser) error {
	var observation kernel.InternalConsoleObservation
	if err := f.ConsoleObservation(&ConsoleObservationArgs{BranchID: id, ConsoleID: consoleID}, &observation); err != nil {
		return err
	}
	go func() {
		defer input.Close()
		buffer := make([]byte, consoleIOChunk)
		for {
			n, readErr := input.Read(buffer)
			for written := 0; written < n; {
				var result ConsoleWriteResult
				if err := f.ConsoleWrite(&ConsoleWriteArgs{BranchID: id, ConsoleID: consoleID, Data: buffer[written:n]}, &result); err != nil {
					return
				}
				written += result.N
				if result.WouldBlock || result.N == 0 {
					var waited ConsoleWaitResult
					if err := f.ConsoleWait(&ConsoleWaitArgs{BranchID: id, ConsoleID: consoleID, Want: kernel.InternalConsoleReadiness{Stdin: true}, Timeout: time.Second}, &waited); err != nil {
						return
					}
				}
			}
			if readErr != nil {
				_ = f.ConsoleCloseStdin(&ConsoleCloseStdinArgs{BranchID: id, ConsoleID: consoleID})
				return
			}
		}
	}()
	return nil
}

// consoleIOChunk is the most PullConsoleInput and CopyConsoleOutput move at
// once, as much as a console's pipe holds.
const consoleIOChunk = 1 << 20

// CopyConsoleOutput copies a branch-local internal console's stdout and stderr
// to the given writers, as reads and waits of the console's control would,
// until both end, or the console or its branch does. A write holds nothing of
// the branch.
func (f *SandboxFamily) CopyConsoleOutput(id, consoleID string, stdout, stderr io.Writer) error {
	streams := [...]struct {
		stream kernel.InternalConsoleStream
		output io.Writer
		eof    bool
	}{
		{stream: kernel.InternalConsoleStdout, output: stdout},
		{stream: kernel.InternalConsoleStderr, output: stderr},
	}
	for !streams[0].eof || !streams[1].eof {
		progress := false
		for i := range streams {
			stream := &streams[i]
			if stream.eof {
				continue
			}
			var result ConsoleReadResult
			if err := f.ConsoleRead(&ConsoleReadArgs{BranchID: id, ConsoleID: consoleID, Stream: stream.stream, MaxBytes: consoleIOChunk}, &result); err != nil {
				return err
			}
			if len(result.Data) != 0 {
				if _, err := stream.output.Write(result.Data); err != nil {
					return err
				}
				progress = true
			}
			if result.EOF {
				stream.eof = true
				progress = true
			}
		}
		if !progress {
			var waited ConsoleWaitResult
			if err := f.ConsoleWait(&ConsoleWaitArgs{BranchID: id, ConsoleID: consoleID, Want: kernel.InternalConsoleReadiness{Stdout: !streams[0].eof, Stderr: !streams[1].eof}, Timeout: time.Second}, &waited); err != nil {
				return err
			}
		}
	}
	return nil
}

// ConsoleCloseStdin closes input while serializing against branch lifecycle
// operations and live fork.
func (f *SandboxFamily) ConsoleCloseStdin(args *ConsoleCloseStdinArgs) error {
	if args == nil {
		return fmt.Errorf("missing internal console close arguments")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	return b.k.CloseInternalConsoleStdin(b.k.SupervisorContext(), args.ConsoleID)
}

// ConsoleResize changes a PTY window while serializing against branch
// lifecycle operations and live fork.
func (f *SandboxFamily) ConsoleResize(args *ConsoleResizeArgs) error {
	if args == nil {
		return fmt.Errorf("missing internal console resize arguments")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	return b.k.ResizeInternalConsole(b.k.SupervisorContext(), args.ConsoleID, linux.Winsize{
		Row:    args.Rows,
		Col:    args.Cols,
		Xpixel: args.XPixels,
		Ypixel: args.YPixels,
	})
}

// ConsoleRemove unregisters a transient console while serializing against
// branch lifecycle operations and live fork.
func (f *SandboxFamily) ConsoleRemove(args *ConsoleRemoveArgs) error {
	if args == nil {
		return fmt.Errorf("missing internal console remove arguments")
	}
	b, err := f.branch(args.BranchID)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", args.BranchID, state)
	}
	f.mu.Unlock()
	return b.k.RemoveInternalConsole(b.k.SupervisorContext(), args.ConsoleID)
}

// beginConsoleWait registers a cancellable Kernel-lifetime lease while holding
// opMu. Live fork and teardown can therefore cancel and join every endpoint
// waiter without racing a new registration.
func (f *SandboxFamily) beginConsoleWait(id string) (*sandboxBranch, *consoleWaitLease, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, nil, err
	}
	lease, finish, err := f.beginBranchConsoleWait(b)
	if err != nil {
		return nil, nil, nil, err
	}
	return b, lease, finish, nil
}

// beginBranchConsoleWait is beginConsoleWait for a branch already resolved.
// A caller that waits on b's Kernel must take the lease on b itself: a rename
// can give b's former ID to another branch, whose lifecycle operations would
// then cancel the wait instead of b's.
func (f *SandboxFamily) beginBranchConsoleWait(b *sandboxBranch) (*consoleWaitLease, func(), error) {
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s", b.id, state)
	}
	lease := &consoleWaitLease{cancel: make(chan struct{}), done: make(chan struct{})}
	if b.consoleWaits == nil {
		b.consoleWaits = make(map[*consoleWaitLease]struct{})
	}
	b.consoleWaits[lease] = struct{}{}
	b.controlOps.Add(1)
	b.compactionOps.Add(1)
	f.mu.Unlock()
	b.opMu.Unlock()
	finish := func() {
		f.mu.Lock()
		delete(b.consoleWaits, lease)
		f.mu.Unlock()
		close(lease.done)
		b.compactionOps.Done()
		b.controlOps.Done()
	}
	return lease, finish, nil
}

// cancelConsoleWaits cancels and joins active endpoint waiter registrations.
// The caller must hold b.opMu, preventing new registrations until it returns.
func (f *SandboxFamily) cancelConsoleWaits(b *sandboxBranch) {
	f.mu.Lock()
	leases := make([]*consoleWaitLease, 0, len(b.consoleWaits))
	for lease := range b.consoleWaits {
		leases = append(leases, lease)
		close(lease.cancel)
	}
	f.mu.Unlock()
	for _, lease := range leases {
		<-lease.done
	}
}

// beginLifetimeControl acquires a Kernel-lifetime lease that does not access
// application memory while blocked. Such waiters survive physical compaction.
func (f *SandboxFamily) beginLifetimeControl(id string, allowExited bool) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchRunning && b.state != BranchPaused && (!allowExited || b.state != BranchExited) {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s, unavailable for lifetime control", id, state)
	}
	b.controlOps.Add(1)
	f.mu.Unlock()
	b.opMu.Unlock()
	return b, b.controlOps.Done, nil
}

// beginControl acquires a Kernel-lifetime and memory-access lease for a
// branch-local operation.
func (f *SandboxFamily) beginControl(id string) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s", id, state)
	}
	b.controlOps.Add(1)
	b.compactionOps.Add(1)
	f.mu.Unlock()
	b.opMu.Unlock()
	return b, func() {
		b.compactionOps.Done()
		b.controlOps.Done()
	}, nil
}

// beginRunningControl acquires the branch operation lock and a Kernel-lifetime
// lease only if id is running. The caller holds the operation lock until done,
// serializing process creation against fork, pause, resume, and destroy.
func (f *SandboxFamily) beginRunningControl(id string) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchRunning {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s, not running", id, state)
	}
	b.controlOps.Add(1)
	b.compactionOps.Add(1)
	f.mu.Unlock()
	return b, func() {
		b.compactionOps.Done()
		b.controlOps.Done()
		b.opMu.Unlock()
	}, nil
}

// beginPausedManagementControl acquires the branch operation lock and opens a
// management-only execution window in a paused branch. Existing guest tasks
// remain stopped. The caller must retain the returned completion function
// until the management process has exited.
func (f *SandboxFamily) beginPausedManagementControl(id string) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s, not paused", id, state)
	}
	b.controlOps.Add(1)
	b.compactionOps.Add(1)
	f.mu.Unlock()
	restorePause, err := b.k.BeginPausedManagementExecution()
	if err != nil {
		b.compactionOps.Done()
		b.controlOps.Done()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("opening paused management execution in branch %q: %w", id, err)
	}
	return b, func() {
		restorePause()
		b.compactionOps.Done()
		b.controlOps.Done()
		b.opMu.Unlock()
	}, nil
}

func branchThreadGroup(k *kernel.Kernel, cid string, tgid kernel.ThreadID) (*kernel.ThreadGroup, error) {
	if tgid < 0 {
		return nil, fmt.Errorf("PID (%d) must be non-negative", tgid)
	}
	if tgid != 0 {
		tg := k.RootPIDNamespace().ThreadGroupWithID(tgid)
		if tg == nil {
			return nil, fmt.Errorf("no such process with PID %d", tgid)
		}
		if tg.Leader().ContainerID() != cid {
			return nil, fmt.Errorf("process %d belongs to a different container: %q", tgid, tg.Leader().ContainerID())
		}
		return tg, nil
	}

	// A child the init reaps meanwhile must not pass for it: a console push
	// and a wait that took one for the init reported the branch exited, with
	// the child's status, while its init ran on.
	initTG := k.RootPIDNamespace().LowestThreadGroupOfContainer(cid)
	if initTG == nil {
		// Kernel retains GlobalInit after PID 1 has left the live PID table so
		// external waiters can continue to observe its terminal status. This is
		// also the init thread group cloned into every TimeTravel branch.
		globalInit := k.GlobalInit()
		if globalInit == nil {
			return nil, fmt.Errorf("container %q has no init process", cid)
		}
		leader := globalInit.Leader()
		if leader == nil || leader.ContainerID() != cid {
			return nil, fmt.Errorf("container %q has no init process", cid)
		}
		initTG = globalInit
	}
	return initTG, nil
}

// DestroyNonLoaderBranches destroys every branch not directly referenced by
// Loader.k.
// ParentID deliberately does not affect ordering: lineage is display metadata,
// and each branch owns every reference required for independent destruction.
func (f *SandboxFamily) DestroyNonLoaderBranches() {
	for {
		f.mu.Lock()
		var id string
		for candidateID := range f.branches {
			if candidateID == f.loaderBranchID {
				continue
			}
			id = candidateID
			break
		}
		f.mu.Unlock()
		if id == "" {
			return
		}
		if err := f.Destroy(id); err != nil {
			// Creating branches are serialized with RPC teardown by the control
			// server. Treat any other state mismatch as a programming error.
			panic(fmt.Sprintf("destroying live branch %q: %v", id, err))
		}
	}
}

// CloseLoaderDehydration releases the final process-local capture during Sentry
// teardown. ttcd has already pinned the immutable snapshot-graph roots before
// it requests deletion of a fully dehydrated family.
func (f *SandboxFamily) CloseLoaderDehydration() {
	f.mu.Lock()
	b := f.branches[f.loaderBranchID]
	var image *kernel.DehydrationCapture
	if b != nil && b.dehydration != nil {
		image = b.dehydration.image
		b.dehydration = nil
	}
	f.mu.Unlock()
	if image != nil {
		_ = image.Close()
	}
}

func (f *SandboxFamily) branch(id string) (*sandboxBranch, error) {
	if id == "" {
		id = f.LoaderBranchID()
	}
	f.mu.Lock()
	b := f.branches[id]
	f.mu.Unlock()
	if b == nil {
		return nil, fmt.Errorf("branch %q does not exist", id)
	}
	return b, nil
}
