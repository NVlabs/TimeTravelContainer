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

package boot

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
)

// DefaultBranchID is the branch used by existing, branch-unaware runsc RPCs.
const DefaultBranchID = "main"

// BranchState describes a live branch's lifecycle state.
type BranchState string

const (
	BranchCreating    BranchState = "creating"
	BranchPaused      BranchState = "paused"
	BranchRunning     BranchState = "running"
	BranchDehydrating BranchState = "dehydrating"
	BranchDehydrated  BranchState = "dehydrated"
	BranchRehydrating BranchState = "rehydrating"
	BranchDestroying  BranchState = "destroying"
	BranchFailed      BranchState = "failed"
)

// BranchInfo is the branch metadata exposed over the control socket.
type BranchInfo struct {
	ID           string
	ParentID     string
	State        BranchState
	CreatedAt    time.Time
	InitPID      kernel.ThreadID
	InitState    string
	SharesMemory bool
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
	Source           string
	FamilyID         string
	Children         []DistributedForkCaptureChildArgs
	MaxTransferBytes uint64
	PushPolicy       ttcremote.TCPPushPolicy
	PushHighWater    uint64
}

// DistributedForkSessionInfo is safe to return through the runsc control
// socket. Token is a bearer capability and must never appear in status or log
// output after initial creation.
type DistributedForkSessionInfo struct {
	SessionID     string
	Source        string
	Address       string
	Token         ttcremote.SnapshotToken
	MetadataBytes uint64
	StreamBytes   uint64
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
}

// BranchArgs selects an existing branch. An empty ID selects the default.
type BranchArgs struct {
	ID string
}

// BranchDehydrateArgs selects a branch and donates its durable metadata file.
type BranchDehydrateArgs struct {
	urpc.FilePayload
	ID string
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

// BranchWaitArgs selects a process inside one live branch. PID 0 selects the
// container init process.
type BranchWaitArgs struct {
	BranchID string
	CID      string
	PID      int32
}

// ConsoleFollowArgs streams one branch's init output and waits for init to
// exit. FilePayload contains the final-output writer, wait-status writer,
// release reader, and optional registration and attachment readiness writers.
type ConsoleFollowArgs struct {
	urpc.FilePayload
	BranchID  string
	CID       string
	ConsoleID string
	ExecID    string
}

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
}

type consoleWaitLease struct {
	cancel chan struct{}
	done   chan struct{}
}

type sandboxBranch struct {
	opMu sync.Mutex
	// controlOps holds Kernel lifetime while branch-local controls are active.
	// Destroy marks the branch destroying, terminates its tasks, then waits for
	// these operations before releasing the Kernel.
	controlOps sync.WaitGroup

	id        string
	parentID  string
	k         *kernel.Kernel
	watchdog  *watchdog.Watchdog
	state     BranchState
	createdAt time.Time
	metrics   SandboxForkMetrics

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

// SandboxFamily owns all in-memory branches in one sentry process.
//
// The default branch is Loader.k. Child branches share immutable and COW
// backing with their ancestors, but each branch owns its Kernel lifecycle.
type SandboxFamily struct {
	mu sync.Mutex
	// dehydrationMu serializes capture ownership changes across sibling
	// branches so collective page-cache eviction sees a stable reservation set.
	dehydrationMu sync.Mutex
	stateChanged  *sync.Cond

	branches         map[string]*sandboxBranch
	controlsQuiesced bool
	// defaultStarted is closed after Loader.Run has installed and started the
	// default branch's init thread group. It allows wait RPCs to be established
	// while the OCI container is still in the created state, eliminating the
	// race between a short-lived init and a post-start waiter.
	defaultStarted     chan struct{}
	defaultStartedOnce sync.Once
	// allowNetworkStack is true only when loader configuration proves that
	// network state has no host-facing links.
	allowNetworkStack              bool
	allowSharedFilesystemResources bool
	watchdogOpts                   *watchdog.Opts
	testOnlyForkFailureInjector    func(SandboxForkStage) error
	testOnlyExecFailureInjector    func(SandboxExecStage) error
	volumeBranchScope              string
	distributedSessions            map[string]*distributedForkSession
	configureNetwork               func(*kernel.Kernel, inet.Stack) error
	onDefaultKernelChanged         func(*kernel.Kernel)
}

type distributedForkSession struct {
	source           string
	address          string
	filesystemBranch string
	resources        vfs.LiveForkFilesystemResources
	server           *ttcremote.TCPPageServer
}

// SandboxFamilyOptions controls eligibility policy shared by all branches.
type SandboxFamilyOptions struct {
	AllowNetworkStack              bool
	AllowSharedFilesystemResources bool
	WatchdogOpts                   *watchdog.Opts
	// VolumeBranchScope distinguishes repository branch names created by this
	// sandbox from identically named live branches in other sandboxes.
	VolumeBranchScope      string
	ConfigureNetwork       func(*kernel.Kernel, inet.Stack) error
	OnDefaultKernelChanged func(*kernel.Kernel)
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
	f := &SandboxFamily{
		allowNetworkStack:              opts.AllowNetworkStack,
		allowSharedFilesystemResources: opts.AllowSharedFilesystemResources,
		watchdogOpts:                   opts.WatchdogOpts,
		testOnlyForkFailureInjector:    opts.TestOnlyForkFailureInjector,
		testOnlyExecFailureInjector:    opts.TestOnlyExecFailureInjector,
		volumeBranchScope:              opts.VolumeBranchScope,
		configureNetwork:               opts.ConfigureNetwork,
		onDefaultKernelChanged:         opts.OnDefaultKernelChanged,
		defaultStarted:                 make(chan struct{}),
		distributedSessions:            make(map[string]*distributedForkSession),
		branches: map[string]*sandboxBranch{
			DefaultBranchID: {
				id:           DefaultBranchID,
				k:            k,
				state:        kernelBranchState(k),
				createdAt:    createdAt,
				execSessions: make(map[string]*execSession),
			},
		},
	}
	f.stateChanged = sync.NewCond(&f.mu)
	return f
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
	infos, err := f.captureDistributedForkMany(args.Source, args.FamilyID, children, args.MaxTransferBytes, args.PushPolicy, args.PushHighWater, listener)
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
	return f.captureDistributedForkMany(args.Source, args.FamilyID, args.Children, args.MaxTransferBytes, args.PushPolicy, args.PushHighWater, nil)
}

func (f *SandboxFamily) captureDistributedForkMany(sourceID, familyID string, children []DistributedForkCaptureChildArgs, maxTransferBytes uint64, pushPolicy ttcremote.TCPPushPolicy, pushHighWater uint64, listener net.Listener) ([]DistributedForkSessionInfo, error) {
	if familyID == "" || len(children) == 0 || maxTransferBytes == 0 {
		return nil, errors.New("distributed fork capture is missing family, children, or transfer limit")
	}
	if len(children) != 1 && listener != nil {
		return nil, errors.New("a shared listener is valid only for a singular distributed capture")
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
		sourceID = DefaultBranchID
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
	defer func() {
		if committed {
			return
		}
		for _, server := range servers {
			_ = server.Close()
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
	infos := make([]DistributedForkSessionInfo, len(children))
	sessions := make([]*distributedForkSession, len(children))
	for i, child := range children {
		metadata, err := (&ttcremote.SessionMetadata{
			FamilyID: familyID, SourceBranchID: sourceID, TargetBranchID: child.TargetBranchID,
			KernelMetadata: capture.KernelMetadata, AllocatorMetadata: capture.AllocatorMetadata,
			LoadRanges: capture.LoadRanges,
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
		}
		infos[i] = DistributedForkSessionInfo{
			SessionID: child.SessionID, Source: sourceID, Address: address, Token: token,
			MetadataBytes: uint64(len(metadata)), StreamBytes: server.StreamBytes(),
		}
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
	if err := resume(); err != nil {
		f.mu.Lock()
		for _, child := range children {
			delete(f.distributedSessions, child.SessionID)
		}
		f.mu.Unlock()
		return nil, fmt.Errorf("resuming source branch %q: %w", sourceID, err)
	}
	f.mu.Lock()
	source.state = kernelBranchState(source.k)
	f.mu.Unlock()
	committed = true
	return infos, nil
}

// ServeDistributedForkConnection handles one connection accepted by ttcd.
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
	return session.server.ServeStream(conn)
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
	return errors.Join(session.server.Close(), removeDistributedFilesystemBranch(session.resources, session.filesystemBranch))
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

// CompleteDistributedForkSession releases memory ownership after a target has
// drained while preserving the filesystem branches now owned by that target.
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
	return session.server.Close()
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
	// A repository's initial branch is always named "main". Only descendants
	// are scoped, since they may be created by multiple sandbox families that
	// share a managed volume repository.
	if visibleBranchID == DefaultBranchID {
		return ttcvolume.MainBranch
	}
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
	if k.IsPaused() {
		return BranchPaused
	}
	return BranchRunning
}

// ReplaceDefaultKernel replaces the default branch during checkpoint restore.
// No child branches may exist because checkpoint restore cannot preserve them.
func (f *SandboxFamily) ReplaceDefaultKernel(k *kernel.Kernel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.branches) != 1 {
		return fmt.Errorf("cannot replace default kernel with %d live child branches", len(f.branches)-1)
	}
	b, ok := f.branches[DefaultBranchID]
	if !ok || b == nil {
		return errors.New("default branch is unavailable during kernel replacement")
	}
	b.k = k
	// The replacement Kernel has not been loaded yet. In particular, its
	// TaskSet is still uninitialized, so querying Kernel.IsPaused is invalid.
	// Loader.Run replaces this transitional value with the restored Kernel's
	// actual state after Kernel.Start.
	b.state = BranchPaused
	return nil
}

// HasChildren reports whether a checkpoint-incompatible child branch exists.
func (f *SandboxFamily) HasChildren() bool {
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

// RestoreDefaultExecSessions reinstates TTC control-plane identities after a
// standalone checkpoint is loaded as a new family. The task graph and internal
// consoles themselves were restored as Kernel state. Watchers are started
// separately after Kernel.Start initializes ThreadGroup.liveGoroutines.
func (f *SandboxFamily) RestoreDefaultExecSessions(sessions []ExecSessionInfo) error {
	f.mu.Lock()
	b := f.branches[DefaultBranchID]
	if b == nil || b.k == nil {
		f.mu.Unlock()
		return errors.New("default branch is unavailable during exec-session restore")
	}
	if len(b.execSessions) != 0 {
		f.mu.Unlock()
		return errors.New("default branch already has exec sessions during restore")
	}
	for _, info := range sessions {
		if info.State != ExecSessionRunning {
			continue
		}
		if info.ID == "" || info.PID <= 0 {
			f.mu.Unlock()
			return fmt.Errorf("invalid restored exec session %+v", info)
		}
		if _, ok := b.execSessions[info.ID]; ok {
			f.mu.Unlock()
			return fmt.Errorf("duplicate restored exec session %q", info.ID)
		}
		tg := b.k.RootPIDNamespace().ThreadGroupWithID(info.PID)
		if tg == nil || tg.Leader().Origin != kernel.OriginExec {
			f.mu.Unlock()
			return fmt.Errorf("restored exec session %q has no exec thread group with PID %d", info.ID, info.PID)
		}
		session := &execSession{info: cloneExecSessionInfo(info), tg: tg}
		b.execSessions[info.ID] = session
	}
	f.mu.Unlock()
	return nil
}

// WatchRestoredDefaultExecSessions starts exit tracking after the restored
// Kernel has started all task goroutines.
func (f *SandboxFamily) WatchRestoredDefaultExecSessions() error {
	f.mu.Lock()
	b := f.branches[DefaultBranchID]
	if b == nil || b.k == nil {
		f.mu.Unlock()
		return errors.New("default branch is unavailable while starting restored exec-session watchers")
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
		f.watchExecSession(DefaultBranchID, item.id, item.session)
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
	b := f.branches[DefaultBranchID]
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

// reserveExecSession registers a creating session. The caller must hold b.opMu
// so that fork and lifecycle transitions cannot observe a partial publication.
func (f *SandboxFamily) reserveExecSession(b *sandboxBranch, args *control.ExecArgs) error {
	if args.ExecID == "" {
		return nil
	}
	if args.InternalConsoleID == "" && !args.PausedManagement {
		return fmt.Errorf("exec session %q requires a Sentry-owned internal console", args.ExecID)
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
	}}
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
	go func() {
		session.tg.WaitExited()
		status := uint32(session.tg.ExitStatus())
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
			ID:           b.id,
			ParentID:     b.parentID,
			State:        b.state,
			CreatedAt:    b.createdAt,
			InitPID:      initPID,
			InitState:    initState,
			SharesMemory: b.k != nil && liveKernels > 1,
			ForkMetrics:  b.metrics,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func branchInitStatus(b *sandboxBranch) (kernel.ThreadID, string) {
	if b.k == nil {
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
		Source: args.Source,
		IDs:    []string{args.ID},
		Paused: args.Paused,
	})
	if err != nil {
		return nil, err
	}
	return &infos[0], nil
}

// ForkMany creates sibling children from one source snapshot. The source is
// paused once and becomes runnable again as soon as the primary child has
// pinned the captured MemoryFile ranges. Remaining siblings are reconstructed
// concurrently without extending source downtime.
func (f *SandboxFamily) ForkMany(args *ForkBranchesArgs) ([]BranchInfo, error) {
	forkStarted := time.Now()
	if args == nil {
		return nil, fmt.Errorf("missing live-fork arguments")
	}
	if len(args.IDs) == 0 {
		return nil, fmt.Errorf("at least one branch ID is required")
	}
	sourceID := args.Source
	if sourceID == "" {
		sourceID = DefaultBranchID
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
		childBranches[i] = &sandboxBranch{
			id:           id,
			parentID:     sourceID,
			state:        BranchCreating,
			createdAt:    time.Now().UTC(),
			execSessions: make(map[string]*execSession),
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
			Clocks:                         sentrytime.NewCalibratedClocks(),
			ChildBranchID:                  f.volumeBranchID(childBranch.id),
			AllowNetworkStack:              f.allowNetworkStack,
			AllowSharedFilesystemResources: f.allowSharedFilesystemResources,
			Metrics:                        &metrics[i].Kernel,
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
			clonedExecSessions[i][id] = &execSession{info: info, tg: childTG}
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
			ID:          childBranch.id,
			ParentID:    childBranch.parentID,
			State:       childBranch.state,
			CreatedAt:   childBranch.createdAt,
			ForkMetrics: metrics[i],
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
	f.stateChanged.Broadcast()
	k := b.k
	f.mu.Unlock()

	f.cancelConsoleWaits(b)
	if wasRunning {
		k.Pause()
	}
	execSessions := make([]ExecSessionInfo, 0, len(b.execSessions))
	f.mu.Lock()
	for _, session := range b.execSessions {
		execSessions = append(execSessions, cloneExecSessionInfo(session.info))
	}
	f.mu.Unlock()
	sort.Slice(execSessions, func(i, j int) bool { return execSessions[i].ID < execSessions[j].ID })
	image, err := k.CaptureDehydration(k.SupervisorContext(), kernel.DehydrationCaptureOptions{
		BranchID: f.volumeBranchID(b.id), MetadataFile: metadataFile,
	})
	if err != nil {
		if wasRunning {
			k.Unpause()
			_ = control.PostResume(k, nil)
		}
		f.mu.Lock()
		b.state = kernelBranchState(k)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return err
	}
	metadataOwned = true
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
	f.mu.Lock()
	b.state = BranchDehydrated
	f.stateChanged.Broadcast()
	f.mu.Unlock()
	if id == DefaultBranchID && f.onDefaultKernelChanged != nil {
		f.onDefaultKernelChanged(nil)
	}
	b.opMu.Unlock()
	dehydrated = true
	return nil
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

	child, err := kernel.LoadDehydration(context.Background(), d.platform, d.image, kernel.DehydrationLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(), BranchID: f.volumeBranchID(b.id),
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
	newSessions := make(map[string]*execSession)
	for _, info := range d.execSessions {
		if info.State != ExecSessionRunning {
			continue
		}
		tg := child.RootPIDNamespace().ThreadGroupWithID(info.PID)
		if tg != nil {
			newSessions[info.ID] = &execSession{info: cloneExecSessionInfo(info), tg: tg}
		}
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
	if id == DefaultBranchID && f.onDefaultKernelChanged != nil {
		f.onDefaultKernelChanged(child)
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
		b := f.branches[DefaultBranchID]
		for b != nil && b.k == nil && (b.state == BranchDehydrating || b.state == BranchDehydrated || b.state == BranchRehydrating) {
			f.stateChanged.Wait()
			b = f.branches[DefaultBranchID]
		}
		if b == nil || b.k == nil {
			f.mu.Unlock()
			return linux.WaitStatusExit(255)
		}
		k := b.k
		f.mu.Unlock()
		k.WaitExited()
		f.mu.Lock()
		if b.k != k || b.state == BranchDehydrating || b.state == BranchDehydrated || b.state == BranchRehydrating {
			f.mu.Unlock()
			continue
		}
		status := k.GlobalInit().ExitStatus()
		f.mu.Unlock()
		return status
	}
}

// Destroy destroys a non-default branch and removes it from the family.
func (f *SandboxFamily) Destroy(id string) error {
	if id == DefaultBranchID {
		return fmt.Errorf("the default branch is owned by the loader")
	}
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
	f.cancelConsoleWaits(b)
	b.opMu.Unlock()
	if image != nil {
		err := image.image.Close()
		f.mu.Lock()
		delete(f.branches, id)
		f.stateChanged.Broadcast()
		f.mu.Unlock()
		return err
	}

	if dog != nil {
		dog.Stop()
	}
	b.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	if paused {
		b.k.Unpause()
	}
	b.k.WaitExited()
	b.controlOps.Wait()
	b.k.Release()

	f.mu.Lock()
	delete(f.branches, id)
	f.mu.Unlock()
	return nil
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
	b, done, err := f.beginControl(id)
	if err != nil {
		return 0, err
	}
	defer done()
	if id == "" || id == DefaultBranchID {
		<-f.defaultStarted
	}
	tg, err := branchThreadGroup(b.k, cid, kernel.ThreadID(pid))
	if err != nil {
		return 0, err
	}
	tg.WaitExited()
	return uint32(tg.ExitStatus()), nil
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
	b, session, done, err := f.execSession(args)
	if err != nil {
		return 0, err
	}
	defer done()
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
	if session.info.ConsoleID == "" && b.execSessions[args.ExecID] == session {
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
	if session.tg == nil {
		return fmt.Errorf("exec session %q is not published", args.ExecID)
	}
	f.mu.Lock()
	exited := session.info.State == ExecSessionExited
	f.mu.Unlock()
	if exited {
		return fmt.Errorf("exec session %q has exited", args.ExecID)
	}
	return b.k.SendExternalSignalThreadGroup(session.tg, &linux.SignalInfo{Signo: args.Signo})
}

// FollowConsole coordinates a client-side console drainer with process lifetime.
// ready acknowledges that the lifetime lease exists, before OCI start may run
// guest code. attached acknowledges that the process and its console exist, so
// the client may begin ConsoleRead calls. The lease remains held until output is
// drained, preventing Sentry shutdown from racing the client's final reads.
func (f *SandboxFamily) FollowConsole(id, cid, execID, consoleID string, finalWriter io.Writer, ready func() error, attached func() error, exited func(uint32) error, released func() error) (uint32, error) {
	b, done, err := f.beginControl(id)
	if err != nil {
		return 0, err
	}
	defer done()
	if ready != nil {
		if err := ready(); err != nil {
			return 0, fmt.Errorf("acknowledging console follower registration: %w", err)
		}
	}
	var tg *kernel.ThreadGroup
	if execID == "" {
		if id == "" || id == DefaultBranchID {
			<-f.defaultStarted
		}
		tg, err = branchThreadGroup(b.k, cid, 0)
		if err != nil {
			return 0, err
		}
	} else {
		f.mu.Lock()
		session := b.execSessions[execID]
		if session != nil && session.info.ContainerID != cid {
			session = nil
		}
		if session != nil && session.info.ConsoleID != consoleID {
			f.mu.Unlock()
			return 0, fmt.Errorf("exec session %q uses console %q, not %q", execID, session.info.ConsoleID, consoleID)
		}
		if session != nil {
			tg = session.tg
		}
		f.mu.Unlock()
		if tg == nil {
			return 0, fmt.Errorf("exec session %q does not exist in branch %q", execID, id)
		}
	}
	if attached != nil {
		if err := attached(); err != nil {
			return 0, fmt.Errorf("acknowledging console attachment: %w", err)
		}
	}
	tg.WaitExited()
	status := uint32(tg.ExitStatus())
	if execID != "" {
		f.mu.Lock()
		if session := b.execSessions[execID]; session != nil && session.tg == tg {
			session.info.State = ExecSessionExited
			session.info.ExitStatus = &status
		}
		f.mu.Unlock()
	}
	if exited != nil {
		if err := exited(status); err != nil {
			return 0, fmt.Errorf("reporting console process exit: %w", err)
		}
	}
	if execID == "" {
		b.k.WaitExited()
	}
	if released != nil {
		if err := released(); err != nil {
			return 0, fmt.Errorf("waiting for client console-read handoff: %w", err)
		}
	}
	streams := []struct {
		stream kernel.InternalConsoleStream
		eof    bool
	}{
		{stream: kernel.InternalConsoleStdout},
		{stream: kernel.InternalConsoleStderr},
	}
	for {
		allEOF := true
		for i := range streams {
			stream := &streams[i]
			if stream.eof {
				continue
			}
			allEOF = false
			b.opMu.Lock()
			data, eof, wouldBlock, err := b.k.ReadInternalConsole(b.k.SupervisorContext(), consoleID, stream.stream, 65536)
			b.opMu.Unlock()
			if err != nil {
				return 0, fmt.Errorf("draining final internal console stream %d: %w", stream.stream, err)
			}
			if len(data) != 0 {
				if err := writeConsoleFrame(finalWriter, byte(stream.stream), data); err != nil {
					return 0, fmt.Errorf("writing final internal console stream %d: %w", stream.stream, err)
				}
			}
			if eof {
				if err := writeConsoleFrame(finalWriter, byte(stream.stream), nil); err != nil {
					return 0, fmt.Errorf("writing final internal console EOF %d: %w", stream.stream, err)
				}
				stream.eof = true
			}
			if wouldBlock {
				return 0, fmt.Errorf("internal console stream %d would block after process exit and client handoff", stream.stream)
			}
		}
		if allEOF {
			break
		}
	}
	return status, nil
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

// ConsoleWait blocks on Sentry endpoint readiness without retaining the branch
// operation mutex, so pause, resume, and live fork remain independent. The
// control lease keeps the selected Kernel alive until the bounded wait returns.
func (f *SandboxFamily) ConsoleWait(args *ConsoleWaitArgs, out *ConsoleWaitResult) error {
	if args == nil || out == nil {
		return fmt.Errorf("missing internal console wait arguments or result")
	}
	if args.Timeout < 0 || args.Timeout > 30*time.Second {
		return fmt.Errorf("internal console wait timeout %s is outside 0..30s", args.Timeout)
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
	b.opMu.Lock()
	f.mu.Lock()
	if f.controlsQuiesced {
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, nil, fmt.Errorf("sandbox control operations are quiescing")
	}
	if b.state != BranchRunning && b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, nil, fmt.Errorf("branch %q is %s", id, state)
	}
	lease := &consoleWaitLease{cancel: make(chan struct{}), done: make(chan struct{})}
	if b.consoleWaits == nil {
		b.consoleWaits = make(map[*consoleWaitLease]struct{})
	}
	b.consoleWaits[lease] = struct{}{}
	b.controlOps.Add(1)
	f.mu.Unlock()
	b.opMu.Unlock()
	finish := func() {
		f.mu.Lock()
		delete(b.consoleWaits, lease)
		f.mu.Unlock()
		close(lease.done)
		b.controlOps.Done()
	}
	return b, lease, finish, nil
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

// beginControl acquires a Kernel-lifetime lease for a branch-local operation.
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
	f.mu.Unlock()
	b.opMu.Unlock()
	return b, b.controlOps.Done, nil
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
	f.mu.Unlock()
	return b, func() {
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
	f.mu.Unlock()
	restorePause, err := b.k.BeginPausedManagementExecution()
	if err != nil {
		b.controlOps.Done()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("opening paused management execution in branch %q: %w", id, err)
	}
	return b, func() {
		restorePause()
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

	var initTG *kernel.ThreadGroup
	var initID kernel.ThreadID
	for _, tg := range k.TaskSet().Root.ThreadGroups() {
		if tg.Leader().ContainerID() != cid {
			continue
		}
		id := k.RootPIDNamespace().IDOfThreadGroup(tg)
		if initTG == nil || id < initID {
			initTG = tg
			initID = id
		}
	}
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

// DestroyChildren destroys all non-default branches during loader teardown.
func (f *SandboxFamily) DestroyChildren() {
	for {
		f.mu.Lock()
		var id string
		remaining := len(f.branches) - 1
		for candidateID := range f.branches {
			if candidateID == DefaultBranchID {
				continue
			}
			hasChild := false
			for _, possibleChild := range f.branches {
				if possibleChild.parentID == candidateID {
					hasChild = true
					break
				}
			}
			if !hasChild {
				id = candidateID
				break
			}
		}
		f.mu.Unlock()
		if id == "" {
			if remaining == 0 {
				return
			}
			panic("cycle in live branch family")
		}
		if err := f.Destroy(id); err != nil {
			// Creating branches are serialized with RPC teardown by the control
			// server. Treat any other state mismatch as a programming error.
			panic(fmt.Sprintf("destroying live branch %q: %v", id, err))
		}
	}
}

func (f *SandboxFamily) branch(id string) (*sandboxBranch, error) {
	if id == "" {
		id = DefaultBranchID
	}
	f.mu.Lock()
	b := f.branches[id]
	f.mu.Unlock()
	if b == nil {
		return nil, fmt.Errorf("branch %q does not exist", id)
	}
	return b, nil
}
