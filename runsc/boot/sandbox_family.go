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
	"fmt"
	"sort"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
)

// DefaultBranchID is the branch used by existing, branch-unaware runsc RPCs.
const DefaultBranchID = "main"

// BranchState describes a live branch's lifecycle state.
type BranchState string

const (
	BranchCreating   BranchState = "creating"
	BranchPaused     BranchState = "paused"
	BranchRunning    BranchState = "running"
	BranchDestroying BranchState = "destroying"
	BranchFailed     BranchState = "failed"
)

// BranchInfo is the branch metadata exposed over the control socket.
type BranchInfo struct {
	ID       string
	ParentID string
	State    BranchState
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

// BranchArgs selects an existing branch. An empty ID selects the default.
type BranchArgs struct {
	ID string
}

// BranchProcessesArgs selects a container inside one live branch.
type BranchProcessesArgs struct {
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

type sandboxBranch struct {
	opMu sync.Mutex
	// controlOps holds Kernel lifetime while branch-local controls are active.
	// Destroy marks the branch destroying, terminates its tasks, then waits for
	// these operations before releasing the Kernel.
	controlOps sync.WaitGroup

	id       string
	parentID string
	k        *kernel.Kernel
	watchdog *watchdog.Watchdog
	state    BranchState
	metrics  SandboxForkMetrics
}

// SandboxFamily owns all in-memory branches in one sentry process.
//
// The default branch is Loader.k. Child branches share immutable and COW
// backing with their ancestors, but each branch owns its Kernel lifecycle.
type SandboxFamily struct {
	mu sync.Mutex

	branches map[string]*sandboxBranch
	// allowNetworkStack is true only when loader configuration proves that
	// network state has no host-facing links.
	allowNetworkStack              bool
	allowSharedFilesystemResources bool
	watchdogOpts                   *watchdog.Opts
	testOnlyForkFailureInjector    func(SandboxForkStage) error
}

// SandboxFamilyOptions controls eligibility policy shared by all branches.
type SandboxFamilyOptions struct {
	AllowNetworkStack              bool
	AllowSharedFilesystemResources bool
	WatchdogOpts                   *watchdog.Opts
	// TestOnlyForkFailureInjector verifies family-level transaction cleanup.
	// Production callers should leave it nil.
	TestOnlyForkFailureInjector func(SandboxForkStage) error
}

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
	return &SandboxFamily{
		allowNetworkStack:              opts.AllowNetworkStack,
		allowSharedFilesystemResources: opts.AllowSharedFilesystemResources,
		watchdogOpts:                   opts.WatchdogOpts,
		testOnlyForkFailureInjector:    opts.TestOnlyForkFailureInjector,
		branches: map[string]*sandboxBranch{
			DefaultBranchID: {
				id:    DefaultBranchID,
				k:     k,
				state: kernelBranchState(k),
			},
		},
	}
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
	b := f.branches[DefaultBranchID]
	b.k = k
	b.state = kernelBranchState(k)
	return nil
}

// HasChildren reports whether a checkpoint-incompatible child branch exists.
func (f *SandboxFamily) HasChildren() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.branches) != 1
}

// MarkDefaultStarted records the default kernel state after Loader.Run.
func (f *SandboxFamily) MarkDefaultStarted() {
	f.mu.Lock()
	b := f.branches[DefaultBranchID]
	b.state = kernelBranchState(b.k)
	f.mu.Unlock()
}

// List returns a stable snapshot sorted by branch ID.
func (f *SandboxFamily) List() []BranchInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BranchInfo, 0, len(f.branches))
	for _, b := range f.branches {
		out = append(out, BranchInfo{ID: b.id, ParentID: b.parentID, State: b.state, ForkMetrics: b.metrics})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Fork creates a child from args.Source. Both branches remain paused unless
// their requested/prior state says otherwise.
func (f *SandboxFamily) Fork(args *ForkBranchArgs) (*BranchInfo, error) {
	forkStarted := time.Now()
	var metrics SandboxForkMetrics
	if args == nil {
		return nil, fmt.Errorf("missing live-fork arguments")
	}
	if args.ID == "" {
		return nil, fmt.Errorf("branch ID must not be empty")
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
	if _, ok := f.branches[args.ID]; ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("branch %q already exists", args.ID)
	}
	childBranch := &sandboxBranch{id: args.ID, parentID: sourceID, state: BranchCreating}
	f.branches[args.ID] = childBranch
	f.mu.Unlock()

	fail := func(err error) (*BranchInfo, error) {
		f.mu.Lock()
		childBranch.state = BranchFailed
		delete(f.branches, args.ID)
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

	if !sourceWasPaused {
		pauseStarted := time.Now()
		source.k.Pause()
		metrics.SourcePause = time.Since(pauseStarted)
	}
	sourcePausedAt := time.Now()
	resumeSource := func() error {
		if sourceWasPaused {
			return nil
		}
		resumeStarted := time.Now()
		source.k.Unpause()
		err := control.PostResume(source.k, nil)
		metrics.SourceResume += time.Since(resumeStarted)
		metrics.SourceDowntime = time.Since(sourcePausedAt)
		return err
	}
	if err := f.injectForkFailure(SandboxForkStageSourcePaused); err != nil {
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(err)
	}

	child, err := source.k.LiveFork(source.k.SupervisorContext(), kernel.LiveForkOptions{
		Clocks:                         sentrytime.NewCalibratedClocks(),
		AllowNetworkStack:              f.allowNetworkStack,
		AllowSharedFilesystemResources: f.allowSharedFilesystemResources,
		Metrics:                        &metrics.Kernel,
	})
	if err != nil {
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(fmt.Errorf("forking branch %q from %q: %w", args.ID, sourceID, err))
	}
	if err := f.injectForkFailure(SandboxForkStageKernelCloned); err != nil {
		child.Release()
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(err)
	}
	childStartStarted := time.Now()
	if err := child.StartPaused(); err != nil {
		// StartPaused failure is an invariant violation, but ensure ownership of
		// the child's shared MemoryFile is still released.
		child.Release()
		_ = resumeSource()
		return fail(fmt.Errorf("starting branch %q paused: %w", args.ID, err))
	}
	metrics.ChildStart = time.Since(childStartStarted)
	cleanupStartedChild := func(dog *watchdog.Watchdog) {
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
	if err := f.injectForkFailure(SandboxForkStageChildStarted); err != nil {
		cleanupStartedChild(nil)
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(err)
	}
	var childWatchdog *watchdog.Watchdog
	if f.watchdogOpts != nil {
		childWatchdog = watchdog.New(child, *f.watchdogOpts)
		childWatchdog.Start()
	}

	f.mu.Lock()
	childBranch.k = child
	childBranch.watchdog = childWatchdog
	childBranch.state = BranchPaused
	f.mu.Unlock()
	if err := f.injectForkFailure(SandboxForkStageBranchReady); err != nil {
		cleanupStartedChild(childWatchdog)
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(err)
	}
	if !args.Paused {
		childResumeStarted := time.Now()
		child.Unpause()
		if err := control.PostResume(child, nil); err != nil {
			child.Pause()
			cleanupStartedChild(childWatchdog)
			_ = resumeSource()
			return fail(fmt.Errorf("resuming branch %q: %w", args.ID, err))
		}
		metrics.ChildResume = time.Since(childResumeStarted)
		f.mu.Lock()
		childBranch.state = kernelBranchState(child)
		f.mu.Unlock()
	}
	if err := resumeSource(); err != nil {
		cleanupStartedChild(childWatchdog)
		return fail(fmt.Errorf("resuming source branch %q: %w", sourceID, err))
	}

	f.mu.Lock()
	source.state = kernelBranchState(source.k)
	metrics.Total = time.Since(forkStarted)
	childBranch.metrics = metrics
	info := BranchInfo{ID: childBranch.id, ParentID: childBranch.parentID, State: childBranch.state, ForkMetrics: metrics}
	f.mu.Unlock()
	return &info, nil
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

// Destroy destroys a non-default branch and removes it from the family.
func (f *SandboxFamily) Destroy(id string) error {
	if id == DefaultBranchID {
		return fmt.Errorf("the default branch is owned by the loader")
	}
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
	paused := b.k.IsPaused()
	dog := b.watchdog
	f.mu.Unlock()
	b.opMu.Unlock()

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

// Signal sends a signal within one branch. Foreground TTY process-group
// delivery is excluded because branch eligibility currently excludes external
// TTY resources and Loader's host-side TTY bookkeeping is not cloned.
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
		return fmt.Errorf("foreground process-group signaling is not supported for live branches")
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
	tg, err := branchThreadGroup(b.k, cid, kernel.ThreadID(pid))
	if err != nil {
		return 0, err
	}
	tg.WaitExited()
	return uint32(tg.ExitStatus()), nil
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

// beginControl acquires a Kernel-lifetime lease for a branch-local operation.
func (f *SandboxFamily) beginControl(id string) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
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

// beginRunningControl acquires a Kernel-lifetime lease only if id is running.
// It is used by operations, such as exec, that must not implicitly resume a
// paused branch.
func (f *SandboxFamily) beginRunningControl(id string) (*sandboxBranch, func(), error) {
	b, err := f.branch(id)
	if err != nil {
		return nil, nil, err
	}
	b.opMu.Lock()
	f.mu.Lock()
	if b.state != BranchRunning {
		state := b.state
		f.mu.Unlock()
		b.opMu.Unlock()
		return nil, nil, fmt.Errorf("branch %q is %s, not running", id, state)
	}
	b.controlOps.Add(1)
	f.mu.Unlock()
	b.opMu.Unlock()
	return b, b.controlOps.Done, nil
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
		return nil, fmt.Errorf("container %q has no init process", cid)
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
