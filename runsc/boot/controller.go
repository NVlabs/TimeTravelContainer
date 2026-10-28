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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	gtime "time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/control/server"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostthread"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/erofs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/seccheck"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	"gvisor.dev/gvisor/pkg/sentry/socket/plugin"
	"gvisor.dev/gvisor/pkg/sentry/state"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot/procfs"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/starttime"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	// ContMgrCheckpoint checkpoints a container.
	ContMgrCheckpoint = "containerManager.Checkpoint"

	// ContMgrCheckpointBranch checkpoints one TimeTravelContainer branch.
	ContMgrCheckpointBranch = "containerManager.CheckpointBranch"

	// ContMgrCreateSubcontainer creates a sub-container.
	ContMgrCreateSubcontainer = "containerManager.CreateSubcontainer"

	// ContMgrDestroySubcontainer is used to stop a sub-container and free all
	// associated resources in the sandbox.
	ContMgrDestroySubcontainer = "containerManager.DestroySubcontainer"

	// ContMgrEvent gets stats about the container used by "runsc events".
	ContMgrEvent = "containerManager.Event"

	// ContMgrExecuteAsync executes a command in a container.
	ContMgrExecuteAsync = "containerManager.ExecuteAsync"

	// ContMgrFSSave saves a filesystem checkpoint.
	ContMgrFSSave = "containerManager.FSSave"

	// ContMgrGetSavings gets the savings for restored sandboxes.
	ContMgrGetSavings = "containerManager.GetSavings"

	// ContMgrPortForward starts port forwarding with the sandbox.
	ContMgrPortForward = "containerManager.PortForward"

	// ContMgrProcesses lists processes running in a container.
	ContMgrProcesses = "containerManager.Processes"

	// ContMgrRestore restores a container from a statefile.
	ContMgrRestore = "containerManager.Restore"

	// ContMgrRestoreSubcontainer restores a container from a statefile.
	ContMgrRestoreSubcontainer = "containerManager.RestoreSubcontainer"

	// ContMgrPause pauses all tasks, blocking until they are stopped.
	ContMgrPause = "containerManager.Pause"

	// ContMgrResume resumes all tasks.
	ContMgrResume = "containerManager.Resume"

	// ContMgrForkBranch creates an in-memory branch of a paused instant.
	ContMgrForkBranch = "containerManager.ForkBranch"

	// ContMgrForkBranches creates sibling branches from one paused instant.
	ContMgrForkBranches = "containerManager.ForkBranches"

	// ContMgrCaptureDistributedFork starts a remote page snapshot session.
	ContMgrCaptureDistributedFork = "containerManager.CaptureDistributedFork"

	// ContMgrCaptureDistributedForkMany starts sibling sessions from one capture.
	ContMgrCaptureDistributedForkMany = "containerManager.CaptureDistributedForkMany"

	// ContMgrServeDistributedForkConnection serves a host-accepted stream.
	ContMgrServeDistributedForkConnection = "containerManager.ServeDistributedForkConnection"

	// ContMgrServeDistributedFilesystemConnection serves a host-accepted TTC
	// filesystem stream from the live Sentry repository graph.
	ContMgrServeDistributedFilesystemConnection = "containerManager.ServeDistributedFilesystemConnection"

	// ContMgrExportDistributedForkSnapshot persists a session's immutable page stream.
	ContMgrExportDistributedForkSnapshot = "containerManager.ExportDistributedForkSnapshot"

	// ContMgrReclaimImageChunks frees the rehydrated chunks a family no
	// longer uses.
	ContMgrReclaimImageChunks = "containerManager.ReclaimImageChunks"

	// ContMgrDistributedForkDependencyStatus reports target loader progress.
	ContMgrDistributedForkDependencyStatus = "containerManager.DistributedForkDependencyStatus"

	// ContMgrSetDistributedForkPrefetchWindow changes target fault prefetching.
	ContMgrSetDistributedForkPrefetchWindow = "containerManager.SetDistributedForkPrefetchWindow"

	// ContMgrDrainDistributedForkMemory makes a target source-independent.
	ContMgrDrainDistributedForkMemory = "containerManager.DrainDistributedForkMemory"

	// ContMgrDrainDistributedForkFilesystems materializes only remote files.
	ContMgrDrainDistributedForkFilesystems = "containerManager.DrainDistributedForkFilesystems"

	// ContMgrDistributedForkFilesystemDrainStatus reports asynchronous filesystem materialization progress.
	ContMgrDistributedForkFilesystemDrainStatus = "containerManager.DistributedForkFilesystemDrainStatus"

	// ContMgrHandoffDistributedForkMemory replaces live paging with sparse chunks.
	ContMgrHandoffDistributedForkMemory = "containerManager.HandoffDistributedForkMemory"

	// ContMgrDistributedForkMemoryHandoffStatus reports asynchronous reader-switch progress.
	ContMgrDistributedForkMemoryHandoffStatus = "containerManager.DistributedForkMemoryHandoffStatus"

	// ContMgrDistributedForkSessionStatus reports remote snapshot progress.
	ContMgrDistributedForkSessionStatus = "containerManager.DistributedForkSessionStatus"

	// ContMgrCancelDistributedForkSession releases a remote snapshot session.
	ContMgrCancelDistributedForkSession = "containerManager.CancelDistributedForkSession"

	// ContMgrCompleteDistributedForkSession releases a drained snapshot.
	ContMgrCompleteDistributedForkSession = "containerManager.CompleteDistributedForkSession"

	// ContMgrRestoreDistributedFork installs a remotely paged target Kernel.
	ContMgrRestoreDistributedFork = "containerManager.RestoreDistributedFork"
	// ContMgrRestoreDurableDehydration installs a process-free branch image.
	ContMgrRestoreDurableDehydration = "containerManager.RestoreDurableDehydration"

	// ContMgrListBranches lists all in-memory branches in the sandbox family.
	ContMgrListBranches = "containerManager.ListBranches"

	// ContMgrPauseBranch pauses one in-memory branch.
	ContMgrPauseBranch = "containerManager.PauseBranch"

	// ContMgrResumeBranch resumes one in-memory branch.
	ContMgrResumeBranch = "containerManager.ResumeBranch"

	// ContMgrSnapshotFilesystemBranch snapshots one persistent filesystem
	// resource while preserving the selected branch's run/pause state.
	ContMgrSnapshotFilesystemBranch = "containerManager.SnapshotFilesystemBranch"

	// ContMgrPublishBranchFilesystemMetadata publishes completed branch-local
	// TTC filesystem mutations for manager-side observation.
	ContMgrPublishBranchFilesystemMetadata = "containerManager.PublishBranchFilesystemMetadata"

	// ContMgrSetBranchQuarantine changes one branch's external-connectivity bit.
	ContMgrSetBranchQuarantine = "containerManager.SetBranchQuarantine"

	// ContMgrDehydrateBranch hibernates one live branch.
	ContMgrDehydrateBranch = "containerManager.DehydrateBranch"
	// ContMgrExportDehydratedBranch exports a dehydrated branch into the
	// snapshot graph's immutable memory objects.
	ContMgrExportDehydratedBranch = "containerManager.ExportDehydratedBranch"

	// ContMgrRehydrateBranch reconstructs one dehydrated branch.
	ContMgrRehydrateBranch = "containerManager.RehydrateBranch"

	// ContMgrRenameBranch changes one in-memory branch's visible name.
	ContMgrRenameBranch = "containerManager.RenameBranch"

	// ContMgrDestroyBranch destroys one in-memory branch.
	ContMgrDestroyBranch = "containerManager.DestroyBranch"
	// ContMgrDestroyBranchAndReclaim destroys one in-memory branch and releases
	// its persistent filesystem roots after all runtime references are gone.
	ContMgrDestroyBranchAndReclaim = "containerManager.DestroyBranchAndReclaim"
	// ContMgrPrepareStopBranch moves branch-unaware loader bookkeeping away
	// from a branch that is about to terminate, when a peer survives.
	ContMgrPrepareStopBranch = "containerManager.PrepareStopBranch"

	// ContMgrBranchProcesses lists processes in one in-memory branch.
	ContMgrBranchProcesses = "containerManager.BranchProcesses"

	// ContMgrBranchRootfsSize gets writable rootfs usage in one branch.
	ContMgrBranchRootfsSize = "containerManager.BranchRootfsSize"
	// ContMgrFamilyResidentMemory accounts mapped resident memory across all
	// branches in one Sentry family.
	ContMgrFamilyResidentMemory = "containerManager.FamilyResidentMemory"

	// ContMgrBranchSignal sends a signal within one in-memory branch.
	ContMgrBranchSignal = "containerManager.BranchSignal"

	// ContMgrBranchWait waits for a process in one in-memory branch.
	ContMgrBranchWait = "containerManager.BranchWait"

	// ContMgrExecSessions lists managed exec sessions in one branch.
	ContMgrExecSessions = "containerManager.ExecSessions"

	// ContMgrExecWait waits for a managed exec session.
	ContMgrExecWait = "containerManager.ExecWait"

	// ContMgrExecSignal signals a managed exec session.
	ContMgrExecSignal = "containerManager.ExecSignal"

	// ContMgrConsolePush has the Sentry push a process's console to a file.
	ContMgrConsolePush = "containerManager.ConsolePush"

	// ContMgrConsoleRead reads branch-local internal console output.
	ContMgrConsoleRead = "containerManager.ConsoleRead"

	// ContMgrConsoleObservation reads branch-local terminal state without I/O.
	ContMgrConsoleObservation = "containerManager.ConsoleObservation"

	// ContMgrConsoleWatch streams Sentry-driven terminal observations.
	ContMgrConsoleWatch = "containerManager.ConsoleWatch"

	// ContMgrConsoleWait waits for branch-local internal console readiness.
	ContMgrConsoleWait = "containerManager.ConsoleWait"

	// ContMgrConsoleWrite writes branch-local internal console input.
	ContMgrConsoleWrite = "containerManager.ConsoleWrite"

	// ContMgrConsoleCloseStdin closes branch-local internal console input.
	ContMgrConsoleCloseStdin = "containerManager.ConsoleCloseStdin"

	// ContMgrConsoleInput has the Sentry copy a file into branch-local
	// internal console input.
	ContMgrConsoleInput = "containerManager.ConsoleInput"

	// ContMgrConsoleOutput has the Sentry copy branch-local internal console
	// output to files.
	ContMgrConsoleOutput = "containerManager.ConsoleOutput"

	// ContMgrConsoleResize changes a branch-local PTY console's window size.
	ContMgrConsoleResize = "containerManager.ConsoleResize"

	// ContMgrConsoleRemove unregisters a transient branch-local console.
	ContMgrConsoleRemove = "containerManager.ConsoleRemove"

	// ContMgrSignal sends a signal to a container.
	ContMgrSignal = "containerManager.Signal"

	// ContMgrStartSubcontainer starts a sub-container inside a running sandbox.
	ContMgrStartSubcontainer = "containerManager.StartSubcontainer"

	// ContMgrWait waits on the init process of the container and returns its
	// ExitStatus.
	ContMgrWait = "containerManager.Wait"

	// ContMgrWaitPID waits on a process with a certain PID in the sandbox and
	// return its ExitStatus.
	ContMgrWaitPID = "containerManager.WaitPID"

	// ContMgrWaitCheckpoint waits for the next Kernel checkpoint to complete.
	ContMgrWaitCheckpoint = "containerManager.WaitCheckpoint"

	// ContMgrWaitRestore waits for the Kernel restore to complete.
	ContMgrWaitRestore = "containerManager.WaitRestore"

	// ContMgrWaitFSCheckpoint waits for the next filesystem checkpoint save to
	// complete.
	ContMgrWaitFSCheckpoint = "containerManager.WaitFSCheckpoint"

	// ContMgrWaitFSRestore waits for filesystem checkpoint restore to complete
	// for all current containers.
	ContMgrWaitFSRestore = "containerManager.WaitFSRestore"

	// ContMgrRootContainerStart starts a new sandbox with a root container.
	ContMgrRootContainerStart = "containerManager.StartRoot"

	// ContMgrCreateTraceSession starts a trace session.
	ContMgrCreateTraceSession = "containerManager.CreateTraceSession"

	// ContMgrDeleteTraceSession deletes a trace session.
	ContMgrDeleteTraceSession = "containerManager.DeleteTraceSession"

	// ContMgrListTraceSessions lists a trace session.
	ContMgrListTraceSessions = "containerManager.ListTraceSessions"

	// ContMgrProcfsDump dumps sandbox procfs state.
	ContMgrProcfsDump = "containerManager.ProcfsDump"

	// ContMgrMount mounts a filesystem in a container.
	ContMgrMount = "containerManager.Mount"

	// ContMgrContainerRuntimeState returns the runtime state of a container.
	ContMgrContainerRuntimeState = "containerManager.ContainerRuntimeState"

	// ContMgrSetNetworkArgs sets network args in loader without creating links.
	ContMgrSetNetworkArgs = "containerManager.SetNetworkArgs"

	// ContMgrGetNetworkConfig returns the network interfaces and routes applied
	// during the creation of root container.
	ContMgrGetNetworkConfig = "containerManager.GetNetworkConfig"
)

const (
	// NetworkInitPluginStack initializes third-party network stack.
	NetworkInitPluginStack = "Network.InitPluginStack"

	// DebugStacks collects sandbox stacks for debugging.
	DebugStacks = "debug.Stacks"
)

// Profiling related commands (see pprof.go for more details).
const (
	ProfileCPU       = "Profile.CPU"
	ProfileHeap      = "Profile.Heap"
	ProfileGoroutine = "Profile.Goroutine"
	ProfileBlock     = "Profile.Block"
	ProfileMutex     = "Profile.Mutex"
	ProfileTrace     = "Profile.Trace"
)

// Logging related commands (see logging.go for more details).
const (
	LoggingChange = "Logging.Change"
)

// Usage related commands (see usage.go for more details).
const (
	UsageCollect = "Usage.Collect"
	UsageUsageFD = "Usage.UsageFD"
)

// Metrics related commands (see metrics.go).
const (
	MetricsGetRegistered = "Metrics.GetRegisteredMetrics"
	MetricsExport        = "Metrics.Export"
)

// Commands for interacting with cgroupfs within the sandbox.
const (
	CgroupsReadControlFiles  = "Cgroups.ReadControlFiles"
	CgroupsWriteControlFiles = "Cgroups.WriteControlFiles"
)

// FS-related commands (see fs.go for more details).
const (
	FsTarRootfsUpperLayer = "Fs.TarRootfsUpperLayer"
	FsRead                = "Fs.Read"
)

// controller holds the control server, and is used for communication into the
// sandbox.
type controller struct {
	// srv is the control server.
	srv *server.Server

	// manager holds the containerManager methods.
	manager *containerManager

	// stopRPCTimeout is the grace period given to in-flight RPCs when the
	// control server is shut down. See the documentation of urpc.Server.Stop
	// for the precise semantics. Configured via --control-rpc-stop-timeout.
	stopRPCTimeout gtime.Duration
}

// newController creates a new controller. The caller must call
// controller.srv.StartServing() to start the controller.
func newController(fd int, l *Loader) (*controller, error) {
	srv, err := server.CreateFromFD(fd)
	if err != nil {
		return nil, err
	}

	ctrl := &controller{
		manager: &containerManager{
			startChan:       make(chan struct{}),
			startResultChan: make(chan error),
			l:               l,
		},
		srv:            srv,
		stopRPCTimeout: l.root.conf.ControlRPCStopTimeout,
	}
	ctrl.registerHandlers()
	return ctrl, nil
}

func (c *controller) registerHandlers() {
	l := c.manager.l
	c.srv.Register(c.manager)
	c.srv.Register(&control.Cgroups{Kernel: l.k})
	c.srv.Register(&control.Fs{Kernel: l.k})
	c.srv.Register(&control.Lifecycle{Kernel: l.k})
	c.srv.Register(&control.Logging{})
	c.srv.Register(&control.Proc{Kernel: l.k})
	c.srv.Register(&control.State{Kernel: l.k})
	c.srv.Register(&control.Usage{Kernel: l.k})
	c.srv.Register(&control.Metrics{})
	c.srv.Register(&debug{})

	if eps, ok := l.k.RootNetworkNamespace().Stack().(*netstack.Stack); ok {
		c.srv.Register(&Network{
			Stack:  eps.Stack,
			Kernel: l.k,
		})
	}

	if pluginStack, ok := l.k.RootNetworkNamespace().Stack().(plugin.PluginStack); ok {
		c.srv.Register(&Network{PluginStack: pluginStack})
	}

	if l.root.conf.ProfileEnable {
		c.srv.Register(control.NewProfile(l.k))
	}
}

// refreshHandlers resets the server and re-registers all handlers using l.
// Useful when l.k has been replaced (e.g. during a restore).
func (c *controller) refreshHandlers() {
	c.srv.ResetServer()
	c.registerHandlers()
}

func (c *controller) stop() {
	c.srv.Stop(c.stopRPCTimeout)
}

// containerManager manages sandbox containers.
type containerManager struct {
	// startChan is used to signal when the root container process should
	// be started.
	startChan chan struct{}

	// startResultChan is used to signal when the root container has
	// started. Any errors encountered during startup will be sent to the
	// channel. A nil value indicates success.
	startResultChan chan error

	// l is the loader that creates containers and sandboxes.
	l *Loader

	// restorer is set when the sandbox in being restored. It stores the state
	// of all containers and perform all actions required by restore.
	restorer *restorer

	filesystemDrainMu sync.Mutex
	// filesystemDrains is keyed by the branch whose source dependency is being
	// released. A process-wide completed/in-progress flag is not a sufficient
	// barrier: another distributed restore may attach a new remote catalog while
	// an older branch is draining. The later branch must execute a fresh pass
	// after its restore has completed, even when the repository-level operation
	// lock serializes that pass behind an existing one.
	filesystemDrains map[string]*distributedForkFilesystemDrain
}

type distributedForkFilesystemDrain struct {
	done chan struct{}
	err  error
}

// DistributedForkFilesystemDrainStatus reports completion of asynchronous
// filesystem materialization. The work is process-wide because all branch
// repositories in a Sentry share the same immutable remote object stores.
type DistributedForkFilesystemDrainStatus struct {
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// StartRoot will start the root container process.
func (cm *containerManager) StartRoot(cid *string, _ *struct{}) error {
	log.Debugf("containerManager.StartRoot, cid: %s", *cid)
	cm.l.mu.Lock()
	state := cm.l.state
	cm.l.mu.Unlock()
	if state != created && !(state == restoringUnstarted && (cm.l.distributedRestore || cm.l.dehydrationRestore)) {
		return fmt.Errorf("sandbox is not in created state, cannot start root container: state=%s", state)
	}
	// Tell the root container to start and wait for the result.
	return cm.onStart()
}

// RestoreDistributedFork replaces the unstarted placeholder Kernel with a
// paused remote-memory target.
func (cm *containerManager) RestoreDistributedFork(args *DistributedForkRestoreArgs, out *DistributedForkRestoreInfo) error {
	info, err := cm.l.restoreDistributedFork(args)
	if err != nil {
		return err
	}
	*out = *info
	return nil
}

// RestoreDurableDehydration replaces the unstarted placeholder Kernel with a
// locally paged process-free branch image.
func (cm *containerManager) RestoreDurableDehydration(args *DurableDehydrationRestoreArgs, out *DurableDehydrationRestoreInfo) error {
	info, err := cm.l.restoreDurableDehydration(args)
	if err != nil {
		return err
	}
	*out = *info
	return nil
}

// onStart notifies that sandbox is ready to start and wait for the result.
func (cm *containerManager) onStart() error {
	cm.startChan <- struct{}{}
	if err := <-cm.startResultChan; err != nil {
		return fmt.Errorf("starting sandbox: %v", err)
	}
	return nil
}

// Processes retrieves information about processes running in the sandbox.
func (cm *containerManager) Processes(cid *string, out *[]*control.Process) error {
	log.Debugf("containerManager.Processes, cid: %s", *cid)
	return control.Processes(cm.l.k, *cid, out)
}

// CreateArgs contains arguments to the Create method.
type CreateArgs struct {
	// CID is the ID of the container to start.
	CID string

	// FilePayload may contain a TTY file for the terminal, if enabled.
	urpc.FilePayload
}

// CreateSubcontainer creates a container within a sandbox.
func (cm *containerManager) CreateSubcontainer(args *CreateArgs, _ *struct{}) error {
	log.Debugf("containerManager.CreateSubcontainer: %s", args.CID)

	if len(args.Files) > 1 {
		return fmt.Errorf("start arguments must have at most 1 files for TTY")
	}
	var tty *fd.FD
	if len(args.Files) == 1 {
		var err error
		tty, err = fd.NewFromFile(args.Files[0])
		if err != nil {
			return fmt.Errorf("error dup'ing TTY file: %w", err)
		}
	}
	return cm.l.createSubcontainer(args.CID, tty)
}

// StartArgs contains arguments to the Start method.
type StartArgs struct {
	// Spec is the spec of the container to start.
	Spec *specs.Spec

	// Config is the runsc-specific configuration for the sandbox.
	Conf *config.Config

	// CID is the ID of the container to start.
	CID string

	// NumGoferFilestoreFDs is the number of gofer filestore FDs donated.
	NumGoferFilestoreFDs int

	// IsDevIoFilePresent indicates whether the dev gofer FD is present.
	IsDevIoFilePresent bool

	// GoferMountConfs contains information about how the gofer mounts have been
	// configured. The first entry is for rootfs and the following entries are
	// for bind mounts in Spec.Mounts (in the same order).
	GoferMountConfs []specutils.GoferMountConf

	// IsRootfsUpperTarFilePresent indicates whether the rootfs upper tar file is present.
	IsRootfsUpperTarFilePresent bool

	// FilePayload contains, in order:
	//   * stdin, stdout, and stderr (optional: if terminal is disabled).
	//   * file descriptors to gofer-backing host files (optional).
	//   * file descriptor for /dev gofer connection (optional)
	//   * file descriptor for rootfs upper tar file (optional)
	//   * file descriptors to connect to gofer to serve the root filesystem.
	urpc.FilePayload
}

// StartSubcontainer runs a created container within a sandbox.
func (cm *containerManager) StartSubcontainer(args *StartArgs, _ *struct{}) error {
	// Validate arguments.
	if args == nil {
		return errors.New("start missing arguments")
	}
	log.Debugf("containerManager.StartSubcontainer, cid: %s, args: %+v", args.CID, args)
	if args.Spec == nil {
		return errors.New("start arguments missing spec")
	}
	if args.Conf == nil {
		return errors.New("start arguments missing config")
	}
	if args.CID == "" {
		return errors.New("start argument missing container ID")
	}
	cm.l.mu.Lock()
	state := cm.l.state
	cm.l.mu.Unlock()
	if state != started && state != restored {
		if state == restoringUnstarted {
			// Translate the `runsc start` to `runsc restore`.
			// TODO(b/441106898): Move this to the shim once single-shim-per-pod is implemented.
			log.Warningf("StartSubcontainer called on a restoring sandbox, restoring subcontainer instead: id=%s", args.CID)
			return cm.RestoreSubcontainer(args, nil)
		}
		return fmt.Errorf("sandbox is not in started state, cannot start subcontainer: state=%s", state)
	}
	expectedFDs := 1 // At least one FD for the root filesystem.
	expectedFDs += args.NumGoferFilestoreFDs
	if args.IsDevIoFilePresent {
		expectedFDs++
	}
	if !args.Spec.Process.Terminal {
		expectedFDs += 3
	}
	if args.IsRootfsUpperTarFilePresent {
		if cm.l.fsRestore != nil {
			return fmt.Errorf("rootfs upper tar file is mutually exclusive with filesystem checkpoint restore")
		}
		expectedFDs++
	}
	if len(args.Files) < expectedFDs {
		return fmt.Errorf("start arguments must contain at least %d FDs, but only got %d", expectedFDs, len(args.Files))
	}

	// All validation passed, logs the spec for debugging.
	specutils.LogSpecDebug(args.Spec, args.Conf.OCISeccomp)

	goferFiles := args.Files
	var stdios []*fd.FD
	if !args.Spec.Process.Terminal {
		// When not using a terminal, stdios come as the first 3 files in the
		// payload.
		var err error
		stdios, err = fd.NewFromFiles(goferFiles[:3])
		if err != nil {
			return fmt.Errorf("error dup'ing stdio files: %w", err)
		}
		goferFiles = goferFiles[3:]
	}
	defer func() {
		for _, fd := range stdios {
			_ = fd.Close()
		}
	}()

	var goferFilestoreFDs []*fd.FD
	for i := 0; i < args.NumGoferFilestoreFDs; i++ {
		goferFilestoreFD, err := fd.NewFromFile(goferFiles[i])
		if err != nil {
			return fmt.Errorf("error dup'ing gofer filestore file: %w", err)
		}
		goferFilestoreFDs = append(goferFilestoreFDs, goferFilestoreFD)
	}
	goferFiles = goferFiles[args.NumGoferFilestoreFDs:]
	defer func() {
		for _, fd := range goferFilestoreFDs {
			_ = fd.Close()
		}
	}()

	var devGoferFD *fd.FD
	if args.IsDevIoFilePresent {
		var err error
		devGoferFD, err = fd.NewFromFile(goferFiles[0])
		if err != nil {
			return fmt.Errorf("error dup'ing dev gofer file: %w", err)
		}
		goferFiles = goferFiles[1:]
		defer devGoferFD.Close()
	}

	var rootfsUpperTarFD *fd.FD
	if args.IsRootfsUpperTarFilePresent {
		var err error
		rootfsUpperTarFD, err = fd.NewFromFile(goferFiles[0])
		if err != nil {
			return fmt.Errorf("error dup'ing rootfs upper tar file: %w", err)
		}
		goferFiles = goferFiles[1:]
		defer rootfsUpperTarFD.Close()
	}

	goferFDs, err := fd.NewFromFiles(goferFiles)
	if err != nil {
		return fmt.Errorf("error dup'ing gofer files: %w", err)
	}
	defer func() {
		for _, fd := range goferFDs {
			_ = fd.Close()
		}
	}()

	if err := cm.l.startSubcontainer(args.Spec, args.Conf, args.CID, stdios, goferFDs, goferFilestoreFDs, devGoferFD, args.GoferMountConfs, rootfsUpperTarFD); err != nil {
		log.Debugf("containerManager.StartSubcontainer failed, cid: %s, args: %+v, err: %v", args.CID, args, err)
		return err
	}
	log.Debugf("Container started, cid: %s", args.CID)
	return nil
}

// DestroySubcontainer stops a container if it is still running and cleans up
// its filesystem.
func (cm *containerManager) DestroySubcontainer(cid *string, _ *struct{}) error {
	log.Debugf("containerManager.DestroySubcontainer, cid: %s", *cid)
	return cm.l.destroySubcontainer(*cid)
}

// syscallsEnd declares that the calling RPC method's host system calls end,
// so that the thread running it may keep a parked HVF vCPU across them; see
// pkg/hostthread. Those of the methods that call it do, being file I/O,
// gofer requests, and imports of descriptors, apart from contended TTC
// repository locks and donated connections, which withdraw the declaration
// while they wait. urpc restores its own setting when a method returns.
func syscallsEnd() {
	hostthread.SyscallsEnd()
}

// ExecuteAsync starts running a command on a created or running sandbox. It
// returns the PID of the new process.
func (cm *containerManager) ExecuteAsync(args *control.ExecArgs, pid *int32) error {
	syscallsEnd()
	log.Debugf("containerManager.ExecuteAsync, cid: %s, args: %+v", args.ContainerID, args)
	tgid, err := cm.l.executeAsync(args)
	if err != nil {
		log.Debugf("containerManager.ExecuteAsync failed, cid: %s, args: %+v, err: %v", args.ContainerID, args, err)
		return err
	}
	*pid = int32(tgid)
	return nil
}

// Checkpoint pauses a sandbox and saves its state.
func (cm *containerManager) Checkpoint(o *control.SaveOpts, _ *struct{}) error {
	log.Debugf("containerManager.Checkpoint")
	o.RunscVersion = version.Version()
	return cm.l.save(o)
}

// CheckpointBranch saves one branch. Disk checkpoint writers are introduced
// only through this explicit RPC and are not part of live fork.
func (cm *containerManager) CheckpointBranch(o *BranchCheckpointArgs, _ *struct{}) error {
	log.Debugf("containerManager.CheckpointBranch: branch=%q", o.BranchID)
	saveOpts, err := control.ConvertToStateSaveOpts(&o.SaveOpts)
	if err != nil {
		return err
	}
	defer saveOpts.Close()
	return cm.l.family.Checkpoint(o.BranchID, cm.l.watchdog, func(k *kernel.Kernel, dog *watchdog.Watchdog, sessions []ExecSessionInfo) error {
		return cm.l.saveKernelWithOpts(k, dog, sessions, saveOpts, &o.ExecOpts)
	})
}

// PortForwardOpts contains options for port forwarding to a port in a
// container.
type PortForwardOpts struct {
	// FilePayload contains one fd for a UDS (or local port) used for port
	// forwarding.
	urpc.FilePayload

	// ContainerID is the container for the process being executed.
	ContainerID string
	// BranchID selects the TimeTravel branch. Empty means the default branch.
	BranchID string
	// AllowStoppedContainer permits TTC to address a rehydrated family after
	// the original OCI init task was intentionally released.
	AllowStoppedContainer bool
	// Port is the port to to forward.
	Port uint16
}

// PortForward initiates a port forward to the container.
func (cm *containerManager) PortForward(opts *PortForwardOpts, _ *struct{}) error {
	log.Debugf("containerManager.PortForward, cid: %s, branch: %s, port: %d", opts.ContainerID, opts.BranchID, opts.Port)
	if err := cm.l.portForward(opts); err != nil {
		log.Debugf("containerManager.PortForward failed, opts: %+v, err: %v", opts, err)
		return err
	}
	return nil
}

// RestoreOpts contains options related to restoring a container's file system.
type RestoreOpts struct {
	// FilePayload contains, in order:
	// 1. checkpoint state file.
	// 2. optional checkpoint pages metadata file.
	// 3. optional checkpoint pages file.
	// 4. optional platform device file.
	urpc.FilePayload
	HavePagesFile  bool
	HaveDeviceFile bool
	Background     bool

	// If UseCheckpointGofer is true, the first file in FilePayload is a Unix
	// domain socket connected to a URPC server implementing
	// stateipc.AsyncFileServer and providing checkpoint files. In this case,
	// RestoreOpts.HavePagesFile is unknown and must be determined by
	// containerManager.Restore.
	UseCheckpointGofer bool `json:"use_checkpoint_gofer"`

	// SplitFSRestore indicates if we should restore the filesystem from a
	// split filesystem checkpoint.
	SplitFSRestore bool `json:"split_fsrestore"`
}

// BranchCheckpointArgs selects one branch and carries the ordinary gVisor
// checkpoint writers.
type BranchCheckpointArgs struct {
	control.SaveOpts
	BranchID string
}

// FilesystemBranchSnapshotArgs selects a live branch, one persistent
// filesystem resource, and the new repository branch pointer to create.
type FilesystemBranchSnapshotArgs struct {
	BranchID      string
	ResourceID    checkpoint.ResourceID
	Child         string
	SourceWorking uint64
	ChildWorking  uint64
}

// Restore loads a container from a statefile.
// The container's current kernel is destroyed, a restore environment is
// created, and the kernel is recreated with the restore state file. The
// container then sends the signal to start.
func (cm *containerManager) Restore(o *RestoreOpts, _ *struct{}) (retErr error) {
	timer := starttime.Timer("Restore")
	timer.Reached("cm.Restore RPC")
	log.Debugf("containerManager.Restore")

	cm.l.mu.Lock()
	cu := cleanup.Make(cm.l.mu.Unlock)
	defer cu.Clean()

	if cm.l.state != created {
		return fmt.Errorf("cannot restore a container in state=%s", cm.l.state)
	}
	defer func() {
		if retErr != nil {
			cu.Clean() // Release `cm.l.mu` as onRestoreFailed will acquire it.
			cm.onRestoreFailed(fmt.Errorf("Restore failed: %w", retErr))
		}
	}()

	// If filesystem restore files were donated to the loader during sandbox
	// creation, we must perform a split filesystem restore. Restoring a split
	// checkpoint without split-fsrestore enabled is not supported.
	if (cm.l.fsRestore != nil) != o.SplitFSRestore {
		if o.SplitFSRestore {
			return fmt.Errorf("split filesystem restore requested, but sandbox was created without filesystem restore files")
		}
		return fmt.Errorf("filesystem restore files were donated during sandbox creation, but split filesystem restore was not requested")
	}

	if len(o.Files) == 0 {
		return fmt.Errorf("at least one file must be passed to Restore")
	}

	stateFile, pagesMetadata, pagesFile, err := getRestoreReaders(o)
	if err != nil {
		return err
	}
	defer func() {
		if stateFile != nil {
			stateFile.Close()
		}
		if pagesMetadata != nil {
			pagesMetadata.Close()
		}
		if pagesFile != nil {
			pagesFile.Close()
		}
	}()
	timer.Reached("got restore readers")

	cm.restorer = &restorer{
		cm:         cm,
		background: o.Background,
		timer:      timer,
	}

	// Create the main MemoryFile.
	if _, ok := cm.l.k.Platform.(platform.RestoreReuser); ok {
		cm.restorer.mainMF, err = createMemoryFileForPlatform(cm.l.k.Platform, cm.l.root.conf.AppHugePages, cm.l.hostTHP)
	} else {
		cm.restorer.mainMF, err = createMemoryFile(cm.l.root.conf.AppHugePages, cm.l.hostTHP)
	}
	if err != nil {
		return fmt.Errorf("creating memory file: %v", err)
	}
	timer.Reached("created MemoryFile")

	if o.HavePagesFile {
		// This immediately starts loading the main MemoryFile asynchronously.
		cm.restorer.asyncMFLoader = kernel.NewAsyncMFLoader(pagesMetadata, pagesFile, cm.restorer.mainMF, timer.Fork("PagesFileLoader")) // transfers ownership
		pagesMetadata = nil
		pagesFile = nil
		timer.Reached("created async MF loader")
	}

	cm.restorer.stateFile, cm.restorer.metadata, err = state.NewStatefileReader(stateFile /* transfers ownership on success */, nil)
	if err != nil {
		return fmt.Errorf("creating statefile reader: %w", err)
	}
	stateFile = nil
	timer.Reached("created statefile reader")

	cm.l.restoreDone = sync.NewCond(&cm.l.mu)
	cm.l.state = restoringUnstarted

	// Release `cm.l.mu`.
	cu.Clean()

	if o.HaveDeviceFile {
		cm.restorer.deviceFile, err = o.ReleaseFD(len(o.Files) - 1)
		if err != nil {
			return err
		}
	}

	// Pause the kernel while we build a new one.
	cm.l.k.Pause()

	countStr, ok := cm.restorer.metadata[ContainerCountKey]
	if !ok {
		return errors.New("container count not present in state file")
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return fmt.Errorf("invalid container count: %w", err)
	}
	if count < 1 {
		return fmt.Errorf("invalid container count value: %v", count)
	}
	cm.restorer.totalContainers = count
	log.Infof("Restoring a total of %d containers", cm.restorer.totalContainers)

	containerSpecs, ok := cm.restorer.metadata[ContainerSpecsKey]
	if !ok {
		return fmt.Errorf("container specs not found in metadata during restore")
	}
	specs, err := specutils.GetSpecsFromString(containerSpecs)
	if err != nil {
		return err
	}
	cm.restorer.checkpointedSpecs = specs

	contract, err := checkpoint.PortableContractFromMetadata(cm.restorer.metadata)
	if err != nil {
		return fmt.Errorf("invalid portable checkpoint: %w", err)
	}
	if err := contract.ValidateDestination(
		version.Version(),
		runtime.GOARCH,
		strings.Fields(cpuid.HostFeatureSet().FlagString()),
		portableSentryCapabilities,
	); err != nil {
		return fmt.Errorf("portable checkpoint is incompatible: %w", err)
	}
	if !contract.IsCrossHostPortable() {
		log.Warningf("Restoring a host-bound checkpoint; cross-host restore is not supported for its resource profile")
	}
	checkpointVersion := cm.restorer.metadata[VersionKey]
	if checkpointVersion != "" && checkpointVersion != version.Version() {
		log.Infof("Checkpoint diagnostic runsc version is %q; current build is %q", checkpointVersion, version.Version())
	}
	timer.Reached("restorer initialized")
	return cm.restorer.restoreContainerInfo(cm.l, &cm.l.root)
}

func getRestoreReaders(o *RestoreOpts) (io.ReadCloser, io.ReadCloser, stateio.AsyncReader, error) {
	if o.UseCheckpointGofer {
		return getRestoreReadersForCheckpointGofer(o)
	}
	return getRestoreReadersForLocalCheckpointFiles(o)
}

func getRestoreReadersForLocalCheckpointFiles(o *RestoreOpts) (io.ReadCloser, io.ReadCloser, stateio.AsyncReader, error) {
	stateFile, err := o.ReleaseFD(0)
	if err != nil {
		return nil, nil, nil, err
	}
	cu := cleanup.Make(func() { stateFile.Close() })
	defer cu.Clean()
	var stat unix.Stat_t
	if err := unix.Fstat(stateFile.FD(), &stat); err != nil {
		return nil, nil, nil, err
	}
	if stat.Size == 0 {
		return nil, nil, nil, fmt.Errorf("statefile cannot be empty")
	}

	if !o.HavePagesFile {
		cu.Release()
		return stateFile, nil, nil, nil
	}
	pagesMetadataFile, err := o.ReleaseFD(1)
	if err != nil {
		return nil, nil, nil, err
	}
	cu.Add(func() { pagesMetadataFile.Close() })
	pagesFile, err := o.ReleaseFD(2)
	if err != nil {
		return nil, nil, nil, err
	}
	cu.Release()
	// //pkg/state/wire reads one byte at a time; buffer reads from
	// pagesMetadataFile to avoid making one syscall per read. For the state
	// file, this buffering is handled by statefile.NewReader() =>
	// compressio.Reader or compressio.NewSimpleReader().
	return stateFile,
		stateio.NewBufioReadCloser(pagesMetadataFile),
		stateio.NewPagesFileFDReaderDefault(int32(pagesFile.Release())),
		nil
}

func getRestoreReadersForCheckpointGofer(o *RestoreOpts) (io.ReadCloser, io.ReadCloser, stateio.AsyncReader, error) {
	clientFD, err := unix.Dup(int(o.Files[0].Fd()))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to dup checkpoint gofer client FD: %w", err)
	}
	clientSock, err := unet.NewSocket(clientFD)
	if err != nil {
		unix.Close(clientFD)
		return nil, nil, nil, fmt.Errorf("failed to create unet.Socket for checkpoint gofer client FD: %w", err)
	}
	afc, err := stateipc.NewAsyncFileClient(urpc.NewClient(clientSock) /* transfers ownership */)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create stateipc client: %w", err)
	}
	defer afc.DecRef()

	stateFileAsync, err := afc.OpenRead(checkpointfiles.StateFileName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open state file: %w", err)
	}
	stateFile, err := stateio.NewBufReader(stateFileAsync /* transfers ownership */, 8<<20 /* size = 8 MiB */)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to buffer state file: %w", err)
	}

	pagesMetadataAsync, err := afc.OpenRead(checkpointfiles.PagesMetadataFileName)
	if err != nil {
		// This might be fs.ErrNotExist or unix.ENOENT, but this detail is lost
		// by URPC (which only preserves the error string), so log and continue
		// under the assumption that it is.
		log.Infof("Failed to open pages metadata file: %v", err)
		o.HavePagesFile = false
		return stateFile, nil, nil, nil
	}
	pagesMetadata, err := stateio.NewBufReader(pagesMetadataAsync /* transfers ownership */, 8<<20 /* size = 8 MiB */)
	if err != nil {
		stateFile.Close()
		return nil, nil, nil, fmt.Errorf("failed to buffer pages metadata file: %w", err)
	}
	pagesFile, err := afc.OpenRead(checkpointfiles.PagesFileName)
	if err != nil {
		pagesMetadata.Close()
		stateFile.Close()
		return nil, nil, nil, fmt.Errorf("failed to open pages file: %w", err)
	}
	o.HavePagesFile = true
	return stateFile, pagesMetadata, pagesFile, nil
}

func (cm *containerManager) onRestoreFailed(err error) {
	cm.l.mu.Lock()
	cm.l.state = restoreFailed
	cm.l.restoreErr = err
	cm.l.mu.Unlock()
	cm.l.restoreDone.Broadcast()
	cm.restorer = nil
}

func (cm *containerManager) onRestoreDone(s Savings) {
	cm.l.mu.Lock()
	cm.l.state = restored
	cm.l.savings = s
	cm.l.mu.Unlock()
	cm.l.restoreDone.Broadcast()
	cm.restorer = nil
}

func (cm *containerManager) RestoreSubcontainer(args *StartArgs, _ *struct{}) (retErr error) {
	log.Debugf("containerManager.RestoreSubcontainer, cid: %s, args: %+v", args.CID, args)
	cm.l.mu.Lock()
	state := cm.l.state
	cm.l.mu.Unlock()
	if state != restoringUnstarted {
		return fmt.Errorf("sandbox is not being restored, cannot restore subcontainer: state=%s", state)
	}
	defer func() {
		if retErr != nil {
			cm.onRestoreFailed(fmt.Errorf("RestoreSubcontainer failed: %w", retErr))
		}
	}()

	// Validate arguments.
	if args.Spec == nil {
		return errors.New("start arguments missing spec")
	}
	if args.Conf == nil {
		return errors.New("start arguments missing config")
	}
	if args.CID == "" {
		return errors.New("start argument missing container ID")
	}
	expectedFDs := 1 // At least one FD for the root filesystem.
	expectedFDs += args.NumGoferFilestoreFDs
	if args.IsDevIoFilePresent {
		expectedFDs++
	}
	if !args.Spec.Process.Terminal {
		expectedFDs += 3
	}
	if args.IsRootfsUpperTarFilePresent {
		return errors.New("rootfs upper tar file is not supported during restore")
	}
	if len(args.Files) < expectedFDs {
		return fmt.Errorf("restore arguments must contain at least %d FDs, but only got %d", expectedFDs, len(args.Files))
	}

	// All validation passed, logs the spec for debugging.
	specutils.LogSpecDebug(args.Spec, args.Conf.OCISeccomp)

	goferFiles := args.Files
	var stdios []*fd.FD
	if !args.Spec.Process.Terminal {
		// When not using a terminal, stdios come as the first 3 files in the
		// payload.
		var err error
		stdios, err = fd.NewFromFiles(goferFiles[:3])
		if err != nil {
			return fmt.Errorf("error dup'ing stdio files: %w", err)
		}
		goferFiles = goferFiles[3:]
	}

	var goferFilestoreFDs []*fd.FD
	for i := 0; i < args.NumGoferFilestoreFDs; i++ {
		overlayFilestoreFD, err := fd.NewFromFile(goferFiles[i])
		if err != nil {
			return fmt.Errorf("error dup'ing overlay filestore file: %w", err)
		}
		goferFilestoreFDs = append(goferFilestoreFDs, overlayFilestoreFD)
	}
	goferFiles = goferFiles[args.NumGoferFilestoreFDs:]

	var devGoferFD *fd.FD
	if args.IsDevIoFilePresent {
		var err error
		devGoferFD, err = fd.NewFromFile(goferFiles[0])
		if err != nil {
			return fmt.Errorf("error dup'ing dev gofer file: %w", err)
		}
		goferFiles = goferFiles[1:]
	}

	goferFDs, err := fd.NewFromFiles(goferFiles)
	if err != nil {
		return fmt.Errorf("error dup'ing gofer files: %w", err)
	}

	err = cm.restorer.restoreSubcontainer(args.Spec, args.Conf, cm.l, args.CID, stdios, goferFDs, goferFilestoreFDs, devGoferFD, args.GoferMountConfs)
	if err != nil {
		log.Debugf("containerManager.RestoreSubcontainer failed, cid: %s, args: %+v, err: %v", args.CID, args, err)
		return err
	}
	log.Debugf("Container restored, cid: %s", args.CID)
	return nil
}

// Pause pauses all tasks, blocking until they are stopped.
func (cm *containerManager) Pause(_, _ *struct{}) error {
	return cm.l.family.Pause(cm.l.family.LoaderBranchID())
}

// Resume resumes all tasks.
func (cm *containerManager) Resume(_, _ *struct{}) error {
	return cm.l.family.Resume(cm.l.family.LoaderBranchID())
}

// ForkBranch creates an in-memory child branch.
func (cm *containerManager) ForkBranch(args *ForkBranchArgs, out *BranchInfo) error {
	syscallsEnd()
	info, err := cm.l.family.Fork(args)
	if err != nil {
		return err
	}
	*out = *info
	return nil
}

// ForkBranches creates in-memory sibling branches.
func (cm *containerManager) ForkBranches(args *ForkBranchesArgs, out *[]BranchInfo) error {
	infos, err := cm.l.family.ForkMany(args)
	if err != nil {
		return err
	}
	*out = infos
	return nil
}

// CaptureDistributedFork starts a remotely readable, metadata-only fork.
func (cm *containerManager) CaptureDistributedFork(args *DistributedForkCaptureArgs, out *DistributedForkSessionInfo) error {
	info, err := cm.l.family.CaptureDistributedFork(args)
	if err != nil {
		return err
	}
	*out = *info
	return nil
}

// CaptureDistributedForkMany starts remotely readable sibling snapshots.
func (cm *containerManager) CaptureDistributedForkMany(args *DistributedForkCaptureManyArgs, out *[]DistributedForkSessionInfo) error {
	infos, err := cm.l.family.CaptureDistributedForkMany(args)
	if err != nil {
		return err
	}
	*out = infos
	return nil
}

// ServeDistributedForkConnection transfers one stream donated by ttcd into
// the Sentry. Serving continues asynchronously after the RPC returns.
func (cm *containerManager) ServeDistributedForkConnection(args *DistributedForkServeArgs, _ *struct{}) error {
	return cm.l.family.ServeDistributedForkConnection(args)
}

// ServeDistributedFilesystemConnection handles one filesystem stream donated
// by ttcd. Serving in the Sentry is essential for multihop forks: its
// repositories own the transient upstream resolvers for inherited objects.
func (cm *containerManager) ServeDistributedFilesystemConnection(args *DistributedForkServeArgs, _ *struct{}) error {
	if args == nil || args.SessionID == "" || len(args.FilePayload.Files) != 1 {
		return errors.New("distributed filesystem serving requires a session and one connected socket")
	}
	token, branch, sourceBranch, err := cm.l.family.DistributedForkFilesystemSource(args.SessionID)
	if err != nil {
		return err
	}
	conn, err := ttcremote.NewDonatedTCPConn(args.FilePayload.Files[0])
	if err != nil {
		return err
	}
	sources := make([]ttcvolume.RemoteObjectSource, 0, len(cm.l.ttcVolumeRepositories))
	for source, volume := range cm.l.ttcVolumeRepositories {
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(source)))
		sources = append(sources, ttcvolume.RemoteObjectSource{Key: key, Repository: volume.repository, Branch: branch, BaseOf: sourceBranch})
	}
	go func() {
		if err := ttcvolume.ServeRemoteObjects(conn, token, sources); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Warningf("Distributed filesystem stream failed: %v", err)
		}
	}()
	return nil
}

// ExportDistributedForkSnapshot writes a durable shared-filesystem snapshot.
func (cm *containerManager) ExportDistributedForkSnapshot(args *DistributedForkExportArgs, out *DistributedForkExportInfo) error {
	info, err := cm.l.family.ExportDistributedForkSnapshot(args)
	if err != nil {
		return err
	}
	if info != nil {
		*out = *info
	}
	return nil
}

// ReclaimImageChunks frees the rehydrated chunks no branch of the family
// uses any more, and returns every chunk freed so, in hexadecimal.
func (cm *containerManager) ReclaimImageChunks(args *DistributedForkDependencyArgs, out *[]string) error {
	objects, err := cm.l.family.ReclaimImageChunks(args.BranchID)
	if err != nil {
		return err
	}
	*out = make([]string, len(objects))
	for i, object := range objects {
		(*out)[i] = hex.EncodeToString(object[:])
	}
	return nil
}

// DistributedForkDependencyStatus reports target-side remote-memory progress.
func (cm *containerManager) DistributedForkDependencyStatus(args *DistributedForkDependencyArgs, out *pgalloc.AsyncPagesFileLoadStats) error {
	var status pgalloc.AsyncPagesFileLoadStats
	var err error
	if args.IncludeDurable {
		status, err = cm.l.family.PageLoadStatus(args.BranchID)
	} else {
		status, err = cm.l.family.DistributedForkDependencyStatus(args.BranchID)
	}
	if err != nil {
		return err
	}
	*out = status
	return nil
}

// SetDistributedForkPrefetchWindow updates future target remote-memory reads.
func (cm *containerManager) SetDistributedForkPrefetchWindow(args *DistributedForkDependencyArgs, _ *struct{}) error {
	return cm.l.family.SetDistributedForkPrefetchWindow(args.BranchID, args.PrefetchBehindBytes, args.PrefetchBytes)
}

// DrainDistributedForkMemory eagerly loads all remote pages and filesystem
// objects for one branch. A source relay may be released only after both are
// locally complete.
func (cm *containerManager) DrainDistributedForkMemory(args *DistributedForkDependencyArgs, _ *struct{}) error {
	if err := cm.l.family.DrainDistributedForkMemory(args.BranchID); err != nil {
		return err
	}
	return cm.materializeDistributedFilesystems()
}

// DrainDistributedForkFilesystems starts materializing filesystem dependencies
// without forcing memory already backed by a durable sparse repository to
// become resident. Materialization is asynchronous so it cannot monopolize the
// Sentry's control server while ordinary exec and branch operations continue.
func (cm *containerManager) DrainDistributedForkFilesystems(args *DistributedForkDependencyArgs, _ *struct{}) error {
	if args == nil || args.BranchID == "" {
		return errors.New("distributed filesystem drain requires a branch")
	}
	cm.filesystemDrainMu.Lock()
	if cm.filesystemDrains == nil {
		cm.filesystemDrains = make(map[string]*distributedForkFilesystemDrain)
	}
	if current := cm.filesystemDrains[args.BranchID]; current != nil {
		select {
		case <-current.done:
			// A repeated request for the same branch establishes a fresh barrier.
			// This makes retry correct even if a newer remote source was attached
			// to the branch after an earlier transaction completed.
			delete(cm.filesystemDrains, args.BranchID)
		default:
			cm.filesystemDrainMu.Unlock()
			return nil
		}
	}
	state := &distributedForkFilesystemDrain{done: make(chan struct{})}
	cm.filesystemDrains[args.BranchID] = state
	cm.filesystemDrainMu.Unlock()
	go func() {
		err := cm.materializeDistributedFilesystems()
		cm.filesystemDrainMu.Lock()
		state.err = err
		close(state.done)
		cm.filesystemDrainMu.Unlock()
	}()
	return nil
}

// DistributedForkFilesystemDrainStatus observes asynchronous filesystem
// materialization without waiting for it on the control server.
func (cm *containerManager) DistributedForkFilesystemDrainStatus(args *DistributedForkDependencyArgs, out *DistributedForkFilesystemDrainStatus) error {
	if args == nil || args.BranchID == "" {
		return errors.New("distributed filesystem drain status requires a branch")
	}
	cm.filesystemDrainMu.Lock()
	defer cm.filesystemDrainMu.Unlock()
	state := cm.filesystemDrains[args.BranchID]
	if state == nil {
		return fmt.Errorf("distributed filesystem drain for branch %q has not been started", args.BranchID)
	}
	select {
	case <-state.done:
		out.Done = true
		if state.err != nil {
			out.Error = state.err.Error()
		}
	default:
	}
	return nil
}

// HandoffDistributedForkMemory atomically replaces the target page reader.
func (cm *containerManager) HandoffDistributedForkMemory(args *DistributedForkHandoffArgs, _ *struct{}) error {
	return cm.l.family.HandoffDistributedForkMemory(args)
}

// DistributedForkMemoryHandoffStatus observes asynchronous reader-switch progress.
func (cm *containerManager) DistributedForkMemoryHandoffStatus(args *BranchArgs, out *DistributedForkHandoffStatus) error {
	status, err := cm.l.family.DistributedForkMemoryHandoffStatus(args.ID)
	if err != nil {
		return err
	}
	*out = status
	return nil
}

func (cm *containerManager) materializeDistributedFilesystems() error {
	// A Sentry whose last init has exited stays for this drain; it may go
	// once the drain has replaced the sources it stayed for.
	defer cm.l.family.TransactionSourcesChanged()
	for source, volume := range cm.l.ttcVolumeRepositories {
		if err := volume.repository.MaterializeRemoteObjects(); err != nil {
			return fmt.Errorf("materializing distributed filesystem %q: %w", source, err)
		}
	}
	return nil
}

// DistributedForkSessionStatus returns token-free transfer progress.
func (cm *containerManager) DistributedForkSessionStatus(args *DistributedForkSessionArgs, out *DistributedForkSessionStatus) error {
	if args == nil {
		return errors.New("missing distributed fork session arguments")
	}
	status, err := cm.l.family.DistributedForkSessionStatus(args.SessionID)
	if err != nil {
		return err
	}
	*out = *status
	return nil
}

// CancelDistributedForkSession releases pages retained for a remote target.
func (cm *containerManager) CancelDistributedForkSession(args *DistributedForkSessionArgs, _ *struct{}) error {
	if args == nil {
		return errors.New("missing distributed fork session arguments")
	}
	return cm.l.family.CancelDistributedForkSession(args.SessionID)
}

// CompleteDistributedForkSession releases drained memory and source-only
// filesystem generations.
func (cm *containerManager) CompleteDistributedForkSession(args *DistributedForkSessionArgs, _ *struct{}) error {
	if args == nil {
		return errors.New("missing distributed fork session arguments")
	}
	return cm.l.family.CompleteDistributedForkSession(args.SessionID)
}

// ListBranches returns the sandbox branch family.
func (cm *containerManager) ListBranches(_ *struct{}, out *[]BranchInfo) error {
	*out = cm.l.family.List()
	return nil
}

// PauseBranch pauses one branch.
func (cm *containerManager) PauseBranch(args *BranchArgs, _ *struct{}) error {
	return cm.l.family.Pause(args.ID)
}

// ResumeBranch resumes one branch.
func (cm *containerManager) ResumeBranch(args *BranchArgs, _ *struct{}) error {
	return cm.l.family.Resume(args.ID)
}

// SnapshotFilesystemBranch creates a consistent filesystem-only snapshot.
func (cm *containerManager) SnapshotFilesystemBranch(args *FilesystemBranchSnapshotArgs, _ *struct{}) error {
	if args == nil {
		return errors.New("missing filesystem snapshot arguments")
	}
	return cm.l.family.SnapshotFilesystemBranch(args.BranchID, args.ResourceID, args.Child, args.SourceWorking, args.ChildWorking)
}

// PublishBranchFilesystemMetadata makes completed filesystem mutations
// visible to manager-side graph readers without imposing sync(2) durability.
func (cm *containerManager) PublishBranchFilesystemMetadata(args *BranchArgs, _ *struct{}) error {
	if args == nil || args.ID == "" {
		return errors.New("missing branch filesystem metadata arguments")
	}
	branchID, err := cm.l.family.PublishFilesystemMetadata(args.ID)
	if err != nil {
		return err
	}
	// A remotely restored branch may still have graph metadata that is lazy
	// behind the Sentry's peer connection. Manager-side observers reopen the
	// repository and cannot use that process-local connection. Resolve both the
	// current namespace and its immutable diff base here, but leave bulk file
	// contents lazy.
	seen := make(map[*ttcvolume.Repository]struct{}, len(cm.l.ttcVolumeRepositories))
	for source, volume := range cm.l.ttcVolumeRepositories {
		if _, duplicate := seen[volume.repository]; duplicate {
			continue
		}
		seen[volume.repository] = struct{}{}
		if err := volume.repository.ResolveBranchMetadata(branchID); err != nil {
			return fmt.Errorf("publishing TTC volume %q metadata for branch %q: %w", source, args.ID, err)
		}
	}
	return nil
}

// SetBranchQuarantine changes one branch's external-connectivity bit.
func (cm *containerManager) SetBranchQuarantine(args *BranchQuarantineArgs, _ *struct{}) error {
	if args == nil || args.ID == "" {
		return errors.New("missing branch quarantine arguments")
	}
	return cm.l.family.SetQuarantine(args.ID, args.Enabled)
}

// DehydrateBranch hibernates one branch and releases its live Kernel graph.
func (cm *containerManager) DehydrateBranch(args *BranchDehydrateArgs, out *BranchDehydrateInfo) error {
	if args == nil || len(args.FilePayload.Files) > 1 {
		return errors.New("dehydration requires at most one metadata file")
	}
	var metadataFile *os.File
	if len(args.FilePayload.Files) == 1 {
		dup, err := unix.Dup(int(args.FilePayload.Files[0].Fd()))
		if err != nil {
			return fmt.Errorf("duplicating dehydration metadata file: %w", err)
		}
		metadataFile = os.NewFile(uintptr(dup), "dehydration-metadata")
	}
	if err := cm.l.family.Dehydrate(args.ID, metadataFile); err != nil {
		return err
	}
	info, err := cm.l.family.DehydrationInfo(args.ID)
	if err != nil {
		return err
	}
	*out = *info
	return nil
}

// ExportDehydratedBranch writes a process-independent graph memory stream.
func (cm *containerManager) ExportDehydratedBranch(args *BranchDehydrationExportArgs, out *BranchDehydrationExportInfo) error {
	info, err := cm.l.family.ExportDehydration(args)
	if err != nil {
		return err
	}
	if info != nil {
		*out = *info
	}
	return nil
}

// RehydrateBranch reconstructs one dehydrated branch.
func (cm *containerManager) RehydrateBranch(args *BranchArgs, _ *struct{}) error {
	return cm.l.family.Rehydrate(args.ID)
}

// RenameBranch changes one branch's visible name.
func (cm *containerManager) RenameBranch(args *BranchRenameArgs, _ *struct{}) error {
	if args == nil {
		return errors.New("missing branch rename arguments")
	}
	return cm.l.family.Rename(args.ID, args.NewID)
}

// DestroyBranch destroys one branch.
func (cm *containerManager) DestroyBranch(args *BranchArgs, _ *struct{}) error {
	return cm.l.family.Destroy(args.ID)
}

// DestroyBranchAndReclaim destroys one branch and its persistent filesystem
// roots. This is reserved for logical branch deletion; stop and migration use
// DestroyBranch so that the filesystem can be restarted or handed off.
func (cm *containerManager) DestroyBranchAndReclaim(args *BranchArgs, _ *struct{}) error {
	syscallsEnd()
	return cm.l.family.DestroyAndReclaim(args.ID)
}

// PrepareStopBranch makes stopping one branch independent from its peers.
func (cm *containerManager) PrepareStopBranch(args *BranchArgs, _ *struct{}) error {
	return cm.l.family.PrepareStop(args.ID)
}

// BranchProcesses returns processes from one branch.
func (cm *containerManager) BranchProcesses(args *BranchProcessesArgs, out *[]*control.Process) error {
	return cm.l.family.Processes(args.BranchID, args.CID, out)
}

// BranchRootfsSize returns writable rootfs usage from one branch.
func (cm *containerManager) BranchRootfsSize(args *BranchRootfsSizeArgs, out *control.RootfsSize) error {
	return cm.l.family.RootfsSize(args.BranchID, args.CID, out)
}

// FamilyResidentMemory accounts mapped resident guest pages across branches.
func (cm *containerManager) FamilyResidentMemory(args *FamilyResidentMemoryArgs, out *FamilyResidentMemory) error {
	return cm.l.family.ResidentMemory(args, out)
}

// BranchSignal sends a signal within one branch.
func (cm *containerManager) BranchSignal(args *BranchSignalArgs, _ *struct{}) error {
	return cm.l.family.Signal(args.BranchID, args.CID, args.PID, args.Signo, args.Mode)
}

// BranchWait waits for a process in one branch.
func (cm *containerManager) BranchWait(args *BranchWaitArgs, waitStatus *uint32) error {
	status, err := cm.l.family.Wait(args.BranchID, args.CID, args.PID)
	if err != nil {
		return err
	}
	*waitStatus = status
	return nil
}

// ExecSessions lists managed exec sessions in one branch.
func (cm *containerManager) ExecSessions(args *ExecSessionsArgs, out *[]ExecSessionInfo) error {
	sessions, err := cm.l.family.ExecSessions(args)
	if err != nil {
		return err
	}
	*out = sessions
	return nil
}

// ExecWait waits for a managed exec session.
func (cm *containerManager) ExecWait(args *ExecWaitArgs, waitStatus *uint32) error {
	syscallsEnd()
	if args == nil || len(args.FilePayload.Files) != 1 {
		return fmt.Errorf("exec wait requires one client-liveness file")
	}
	cancel := make(chan struct{})
	go func() {
		var probe [1]byte
		_, _ = args.FilePayload.Files[0].Read(probe[:])
		close(cancel)
	}()
	status, err := cm.l.family.WaitExecSession(&args.ExecSessionArgs, cancel)
	if err != nil {
		return err
	}
	*waitStatus = status
	return nil
}

// ExecSignal signals a managed exec session.
func (cm *containerManager) ExecSignal(args *ExecSessionArgs, _ *struct{}) error {
	return cm.l.family.SignalExecSession(args)
}

// ConsolePush pushes a process's console, in the background, to the one file
// given.
func (cm *containerManager) ConsolePush(args *ConsolePushArgs, _ *struct{}) error {
	syscallsEnd()
	if len(args.FilePayload.Files) != 1 {
		return fmt.Errorf("console push received %d files, want its output", len(args.FilePayload.Files))
	}
	// urpc closes the files it received when this returns: the push writes
	// a copy, which the follower made blocking.
	output, err := args.FilePayload.ReleaseFD(0)
	if err != nil {
		return fmt.Errorf("taking console push output: %w", err)
	}
	if err := cm.l.family.PushConsole(args.BranchID, args.CID, args.ExecID, args.ConsoleID, output); err != nil {
		_ = output.Close()
		return err
	}
	return nil
}

// ConsoleInput has the Sentry copy the one file given into a branch-local
// internal console's input: see PullConsoleInput.
func (cm *containerManager) ConsoleInput(args *ConsoleInputArgs, _ *struct{}) error {
	syscallsEnd()
	if len(args.FilePayload.Files) != 1 {
		return fmt.Errorf("console input received %d files, want its input", len(args.FilePayload.Files))
	}
	// urpc closes the files it received when this returns: the copy reads a
	// copy, which the caller made blocking.
	input, err := args.FilePayload.ReleaseFD(0)
	if err != nil {
		return fmt.Errorf("taking console input: %w", err)
	}
	if err := cm.l.family.PullConsoleInput(args.BranchID, args.ConsoleID, input); err != nil {
		_ = input.Close()
		return err
	}
	return nil
}

// ConsoleOutput has the Sentry copy a branch-local internal console's stdout
// and stderr to the two files given until both end, and returns then: see
// CopyConsoleOutput.
func (cm *containerManager) ConsoleOutput(args *ConsoleOutputArgs, _ *struct{}) error {
	syscallsEnd()
	if len(args.FilePayload.Files) != 2 {
		return fmt.Errorf("console output received %d files, want stdout and stderr", len(args.FilePayload.Files))
	}
	// The copy's writes, to files the caller made blocking, need not end:
	// they are a goroutine's of their own.
	copied := make(chan error, 1)
	go func() {
		copied <- cm.l.family.CopyConsoleOutput(args.BranchID, args.ConsoleID, args.FilePayload.Files[0], args.FilePayload.Files[1])
	}()
	return <-copied
}

// ConsoleRead reads output from a branch-local internal console.
func (cm *containerManager) ConsoleRead(args *ConsoleReadArgs, out *ConsoleReadResult) error {
	return cm.l.family.ConsoleRead(args, out)
}

// ConsoleObservation returns terminal state without consuming its streams.
func (cm *containerManager) ConsoleObservation(args *ConsoleObservationArgs, out *kernel.InternalConsoleObservation) error {
	return cm.l.family.ConsoleObservation(args, out)
}

// ConsoleWatch streams newline-delimited terminal observation objects.
func (cm *containerManager) ConsoleWatch(args *ConsoleWatchArgs, _ *struct{}) error {
	if args == nil || len(args.FilePayload.Files) != 1 {
		return fmt.Errorf("console watch requires exactly one output file")
	}
	encoder := json.NewEncoder(args.FilePayload.Files[0])
	return cm.l.family.ConsoleWatch(&args.ConsoleObservationArgs, func(observation kernel.InternalConsoleObservation) error {
		return encoder.Encode(&observation)
	})
}

// ConsoleWait waits for branch-local internal console I/O readiness.
func (cm *containerManager) ConsoleWait(args *ConsoleWaitArgs, out *ConsoleWaitResult) error {
	return cm.l.family.ConsoleWait(args, out)
}

// ConsoleWrite writes input to a branch-local internal console.
func (cm *containerManager) ConsoleWrite(args *ConsoleWriteArgs, out *ConsoleWriteResult) error {
	return cm.l.family.ConsoleWrite(args, out)
}

// ConsoleCloseStdin closes input to a branch-local internal console.
func (cm *containerManager) ConsoleCloseStdin(args *ConsoleCloseStdinArgs, _ *struct{}) error {
	return cm.l.family.ConsoleCloseStdin(args)
}

// ConsoleResize changes a branch-local PTY console's window size.
func (cm *containerManager) ConsoleResize(args *ConsoleResizeArgs, _ *struct{}) error {
	return cm.l.family.ConsoleResize(args)
}

// ConsoleRemove unregisters a transient branch-local internal console.
func (cm *containerManager) ConsoleRemove(args *ConsoleRemoveArgs, _ *struct{}) error {
	return cm.l.family.ConsoleRemove(args)
}

// Wait waits for the init process in the given container.
func (cm *containerManager) Wait(cid *string, waitStatus *uint32) error {
	log.Debugf("containerManager.Wait, cid: %s", *cid)
	err := cm.l.waitContainer(*cid, waitStatus)
	log.Debugf("containerManager.Wait returned, cid: %s, waitStatus: %#x, err: %v", *cid, *waitStatus, err)
	return err
}

// WaitPIDArgs are arguments to the WaitPID method.
type WaitPIDArgs struct {
	// PID is the PID in the container's PID namespace.
	PID int32

	// CID is the container ID.
	CID string
}

// WaitPID waits for the process with PID 'pid' in the sandbox.
func (cm *containerManager) WaitPID(args *WaitPIDArgs, waitStatus *uint32) error {
	log.Debugf("containerManager.Wait, cid: %s, pid: %d", args.CID, args.PID)
	err := cm.l.waitPID(kernel.ThreadID(args.PID), args.CID, waitStatus)
	log.Debugf("containerManager.Wait, cid: %s, pid: %d, waitStatus: %#x, err: %v", args.CID, args.PID, *waitStatus, err)
	return err
}

// WaitCheckpoint waits for the Kernel to have been successfully checkpointed.
func (cm *containerManager) WaitCheckpoint(*struct{}, *struct{}) error {
	log.Debugf("containerManager.WaitCheckpoint")
	err := cm.l.k.WaitForCheckpoint()
	log.Debugf("containerManager.WaitCheckpoint done, err = %v", err)
	return err
}

func (cm *containerManager) WaitRestore(*struct{}, *struct{}) error {
	log.Debugf("containerManager.WaitRestore")
	err := cm.l.waitRestore()
	log.Debugf("containerManager.WaitRestore done, err = %v", err)
	return err
}

func (cm *containerManager) WaitFSCheckpoint(*struct{}, *struct{}) error {
	log.Debugf("containerManager.WaitFSCheckpoint")
	err := cm.l.k.WaitForFSSave()
	log.Debugf("containerManager.WaitFSCheckpoint done, err = %v", err)
	return err
}

// WaitFSRestoreArgs holds arguments to containerManager.WaitFSRestore.
type WaitFSRestoreArgs struct {
	// CID is the container ID.
	CID string
}

func (cm *containerManager) WaitFSRestore(args *WaitFSRestoreArgs, _ *struct{}) error {
	log.Debugf("containerManager.WaitFSRestore")
	err := cm.l.fsRestore.wait(args.CID)
	log.Debugf("containerManager.WaitFSRestore done, err = %v", err)
	return err
}

// SignalDeliveryMode enumerates different signal delivery modes.
type SignalDeliveryMode int

const (
	// DeliverToProcess delivers the signal to the container process with
	// the specified PID. If PID is 0, then the container init process is
	// signaled.
	DeliverToProcess SignalDeliveryMode = iota

	// DeliverToAllProcesses delivers the signal to all processes in the
	// container. PID must be 0.
	DeliverToAllProcesses

	// DeliverToForegroundProcessGroup delivers the signal to the
	// foreground process group in the same TTY session as the specified
	// process. If PID is 0, then the signal is delivered to the foreground
	// process group for the TTY for the init process.
	DeliverToForegroundProcessGroup

	// DeliverToProcessGroup delivers the signal to all processes in the
	// process group identified by a PGID.
	DeliverToProcessGroup
)

func (s SignalDeliveryMode) String() string {
	switch s {
	case DeliverToProcess:
		return "Process"
	case DeliverToAllProcesses:
		return "All"
	case DeliverToForegroundProcessGroup:
		return "Foreground Process Group"
	case DeliverToProcessGroup:
		return "Process Group"
	}
	return fmt.Sprintf("unknown signal delivery mode: %d", s)
}

// SignalArgs are arguments to the Signal method.
type SignalArgs struct {
	// BranchID selects a TimeTravel branch. Empty preserves the legacy default branch.
	BranchID string

	// CID is the container ID.
	CID string

	// Signo is the signal to send to the process.
	Signo int32

	// PID is the process ID in the given container that will be signaled,
	// relative to the root PID namespace, not the container's.
	// If 0, the root container will be signalled.
	PID int32

	// Mode is the signal delivery mode.
	Mode SignalDeliveryMode
}

// Signal sends a signal to one or more processes in a container. If args.PID
// is 0, then the container init process is used. Depending on the
// args.SignalDeliveryMode option, the signal may be sent directly to the
// indicated process, to all processes in the container, or to the foreground
// process group.
func (cm *containerManager) Signal(args *SignalArgs, _ *struct{}) error {
	log.Debugf("containerManager.Signal: branch: %s, cid: %s, PID: %d, signal: %d, mode: %v", args.BranchID, args.CID, args.PID, args.Signo, args.Mode)
	if args.BranchID != "" {
		return cm.l.family.Signal(args.BranchID, args.CID, args.PID, args.Signo, args.Mode)
	}
	return cm.l.signal(args.CID, args.PID, args.Signo, args.Mode)
}

// CreateTraceSessionArgs are arguments to the CreateTraceSession method.
type CreateTraceSessionArgs struct {
	Config seccheck.SessionConfig
	Force  bool
	urpc.FilePayload
}

// CreateTraceSession creates a new trace session.
func (cm *containerManager) CreateTraceSession(args *CreateTraceSessionArgs, _ *struct{}) error {
	log.Debugf("containerManager.CreateTraceSession: config: %+v", args.Config)
	for i, sinkFile := range args.Files {
		if sinkFile != nil {
			fd, err := fd.NewFromFile(sinkFile)
			if err != nil {
				return err
			}
			args.Config.Sinks[i].FD = fd
		}
	}
	return seccheck.Create(&args.Config, args.Force)
}

// DeleteTraceSession deletes an existing trace session.
func (cm *containerManager) DeleteTraceSession(name *string, _ *struct{}) error {
	log.Debugf("containerManager.DeleteTraceSession: name: %q", *name)
	return seccheck.Delete(*name)
}

// ListTraceSessions lists trace sessions.
func (cm *containerManager) ListTraceSessions(_ *struct{}, out *[]seccheck.SessionConfig) error {
	log.Debugf("containerManager.ListTraceSessions")
	seccheck.List(out)
	return nil
}

// ProcfsDump dumps procfs state of the sandbox.
//
// Callers must not hold any thread-group leader's Task.mu in the sandbox.
// checklocks cannot name the leader mutexes in the slice returned by
// PIDNamespace.ThreadGroups.
func (cm *containerManager) ProcfsDump(_ *struct{}, out *[]procfs.ProcessProcfsDump) error {
	log.Debugf("containerManager.ProcfsDump")
	ts := cm.l.k.TaskSet()
	pidns := ts.Root
	tgs := pidns.ThreadGroups()
	*out = make([]procfs.ProcessProcfsDump, 0, len(tgs))
	for _, tg := range tgs {
		pid := pidns.IDOfThreadGroup(tg)
		procDump, err := procfs.Dump(tg.Leader(), pid, pidns)
		if err != nil {
			log.Warningf("skipping procfs dump for PID %s: %v", pid, err)
			continue
		}
		*out = append(*out, procDump)
	}
	return nil
}

// MountArgs contains arguments to the Mount method.
type MountArgs struct {
	// ContainerID is the container in which we will mount the filesystem.
	ContainerID string

	// Source is the mount source.
	Source string

	// Destination is the mount target.
	Destination string

	// FsType is the filesystem type.
	FsType string

	// FilePayload contains the source image FD, if required by the filesystem.
	urpc.FilePayload
}

const initTID kernel.ThreadID = 1

// Mount mounts a filesystem in a container.
func (cm *containerManager) Mount(args *MountArgs, _ *struct{}) error {
	log.Debugf("containerManager.Mount, cid: %s, args: %+v", args.ContainerID, args)

	var cu cleanup.Cleanup
	defer cu.Clean()

	cm.l.mu.Lock()
	defer cm.l.mu.Unlock()
	eid := execID{cid: args.ContainerID}
	ep, ok := cm.l.processes[eid]
	if !ok {
		return fmt.Errorf("container %v is deleted", args.ContainerID)
	}
	if ep.tg == nil {
		return fmt.Errorf("container %v isn't started", args.ContainerID)
	}

	t := ep.tg.PIDNamespace().TaskWithID(initTID)
	if t == nil {
		return fmt.Errorf("failed to find init process")
	}

	source := args.Source
	dest := path.Clean(args.Destination)
	fstype := args.FsType

	if dest[0] != '/' {
		return fmt.Errorf("absolute path must be provided for destination")
	}

	var opts vfs.MountOptions
	switch fstype {
	case erofs.Name:
		if len(args.FilePayload.Files) != 1 {
			return fmt.Errorf("exactly one image file must be provided")
		}

		imageFD, err := unix.Dup(int(args.FilePayload.Files[0].Fd()))
		if err != nil {
			return fmt.Errorf("failed to dup image FD: %v", err)
		}
		cu.Add(func() { unix.Close(imageFD) })

		opts = vfs.MountOptions{
			ReadOnly: true,
			GetFilesystemOptions: vfs.GetFilesystemOptions{
				InternalMount: true,
				Data:          fmt.Sprintf("ifd=%d", imageFD),
			},
		}

	default:
		return fmt.Errorf("unsupported filesystem type: %v", fstype)
	}

	ctx := context.Background()
	root := t.FSContext().RootDirectory()
	defer root.DecRef(ctx)

	pop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(dest),
	}

	if _, err := t.Kernel().VFS().MountAt(ctx, t.Credentials(), source, &pop, fstype, &opts); err != nil {
		return err
	}
	log.Infof("Mounted %q to %q type: %s, internal-options: %q, in container %q", source, dest, fstype, opts.GetFilesystemOptions.Data, args.ContainerID)
	cu.Release()
	return nil
}

// ContainerRuntimeState returns the runtime state of a container.
func (cm *containerManager) ContainerRuntimeState(cid *string, state *ContainerRuntimeState) error {
	log.Debugf("containerManager.ContainerRuntimeState: cid: %s", *cid)
	*state = cm.l.containerRuntimeState(*cid)
	return nil
}

// FSSaveArgs holds arguments to FSSave.
type FSSaveArgs struct {
	// FilePayload contains the following fscheckpoint files in order:
	// 1. manifest file
	// 2. multi-tar file
	// 3. pages metadata file
	// 4. pages file
	urpc.FilePayload

	// Paths are the paths inside the containers to save to the checkpoint.
	Paths []checkpoint.ResourceID `json:"paths"`

	// Equivalent to kernel.FSSaveOpts fields.
	ExitAfterSaving bool `json:"exit_after_saving"`

	// If UseCheckpointGofer is true, FSSaveArgs.FilePayload should contain
	// exactly one FD, which is a Unix domain socket connected to a URPC server
	// implementing stateipc.AsyncFileServer.
	UseCheckpointGofer bool `json:"use_checkpoint_gofer"`
}

// FSSave collects a filesystem checkpoint.
func (cm *containerManager) FSSave(args *FSSaveArgs, _ *struct{}) error {
	log.Debugf("containerManager.FSSave")
	kopts, err := convertToKernelFSSaveOpts(args)
	if err != nil {
		return err
	}
	return cm.l.k.FSSave(cm.l.k.SupervisorContext(), &kopts)
}

// Savings holds the savings with restore.
type Savings struct {
	// CPUTimeSaved is the CPU time saved at restore.
	CPUTimeSaved gtime.Duration
	// WallTimeSaved is the wall time saved at restore.
	WallTimeSaved gtime.Duration
}

// GetSavings returns the savings for restored sandboxes.
func (cm *containerManager) GetSavings(_ *struct{}, s *Savings) error {
	log.Debugf("containerManager.GetSavings")
	cm.l.mu.Lock()
	*s = cm.l.savings
	cm.l.mu.Unlock()
	return nil
}

// SetNetworkArgs sets the network arguments. It configures host sockets
// (packet fanout groups) before seccomp is installed, but does not create
// the netstack links and routes.
func (cm *containerManager) SetNetworkArgs(args *CreateLinksAndRoutesArgs, _ *struct{}) error {
	log.Debugf("containerManager.SetNetworkArgs")
	if args == nil {
		return fmt.Errorf("cannot set nil networkArgs")
	}

	cm.l.mu.Lock()
	state := cm.l.state
	cm.l.mu.Unlock()
	if state == started || state == restored || state == restoringStarted {
		log.Warningf("SetNetworkArgs called after sandbox started (state=%s), ignoring", state)
		return nil
	}

	// Create a new CreateLinksAndRoutesArgs variable to store in the loader
	// as the FDs associated with the original argument passed to this urpc
	// will be closed when it returns.
	networkArgs := *args
	dupedFDs, err := fd.NewFromFiles(args.FilePayload.Files)
	if err != nil {
		return fmt.Errorf("failed to dup network FDs: %w", err)
	}
	c := cleanup.Make(func() {
		for _, f := range dupedFDs {
			_ = f.Close()
		}
	})
	defer c.Clean()

	// Configure FDs before seccomp is installed.
	fdIdx := 0
	for i := range networkArgs.FDBasedLinks {
		link := &networkArgs.FDBasedLinks[i]
		link.IsPacket = make([]bool, link.NumChannels)

		fid := int32(-1)
		for ch := 0; ch < link.NumChannels; ch++ {
			if fdIdx >= len(dupedFDs) {
				return fmt.Errorf("insufficient FDs provided for link %q", link.Name)
			}
			f := dupedFDs[fdIdx]
			fdIdx++
			cm.l.pinRing.Add(f.FD())

			isSocket, err := fdbased.IsSocketFD(f.FD())
			if err != nil {
				return err
			}

			isPacket, err := fdbased.IsPacketSocket(f.FD(), isSocket)
			if err != nil {
				return err
			}
			link.IsPacket[ch] = isPacket

			if isPacket {
				if fid < 0 {
					fid, err = fdbased.CreatePacketFanoutGroup(f.FD())
				} else {
					err = fdbased.JoinPacketFanoutGroup(f.FD(), fid)
				}
				if err != nil {
					return fmt.Errorf("pre-configuring PACKET_FANOUT failed for link %q: %w", link.Name, err)
				}
			}
		}
		if networkArgs.PCAP {
			// Skip PCAP fd.
			fdIdx++
		}
		link.PreConfigured = true
	}

	// Release the duplicated FDs back to os.File objects and store them in the copy.
	networkArgs.FilePayload.Files = fd.ReleaseToFiles(dupedFDs, "network-fd")
	c.Release()

	cm.l.mu.Lock()
	cm.l.networkArgs = &networkArgs
	cm.l.mu.Unlock()
	return nil
}

// GetNetworkConfig returns the network interfaces and routes.
func (cm *containerManager) GetNetworkConfig(_ *struct{}, networkArgs *CreateLinksAndRoutesArgs) error {
	log.Debugf("containerManager.GetNetworkConfig")
	cm.l.mu.Lock()
	if cm.l.networkArgs != nil {
		*networkArgs = *cm.l.networkArgs
	}
	cm.l.mu.Unlock()

	if networkArgs == nil {
		log.Debugf("networks args is nil")
	}
	return nil
}
