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

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/sandbox"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// TimeTravel controls experimental in-memory sandbox branches.
type TimeTravel struct {
	containerLoader
	source                        string
	paused                        bool
	quarantine                    bool
	pid                           int
	execID                        string
	signal                        int
	all                           bool
	consoleID                     string
	consoleStream                 string
	consoleMaxBytes               int
	consoleData                   string
	consoleWaitStdout             bool
	consoleWaitStderr             bool
	consoleWaitStdin              bool
	consoleWaitTimeout            time.Duration
	consoleRows                   int
	consoleCols                   int
	consoleXPixels                int
	consoleYPixels                int
	consoleLog                    string
	consoleReadyFD                int
	consoleWaitStatus             string
	consoleRetentionStats         string
	consolePIDFile                string
	consoleLogMax                 int64
	consoleLogFiles               int
	checkpointImagePath           string
	checkpointCompression         CheckpointCompression
	dehydrationFD                 int
	dehydrationConfigFD           int
	dehydrationSnapshotsStartFD   int
	dehydrationSnapshotsCount     int
	dehydrationSnapshotAllowCopy  bool
	dehydrationMemoryRepositoryFD int
	dehydrationGraphStreamFD      int
	dehydrationGraphManifestFD    int
	dehydrationMemoryRemoteFD     int
	dehydrationTargetsStartFD     int
	dehydrationTargetsCount       int
	dehydrationFilesystemsStartFD int
	dehydrationFilesystemsCount   int
	distributedSessionID          string
	distributedFamilyID           string
	distributedListen             string
	distributedAdvertise          string
	distributedMaxTransfer        uint64
	distributedConfigFD           int
	distributedMetadataFD         int
	distributedExternal           bool
	distributedConnectionFD       int
	distributedConnectionsStartFD int
	distributedConnectionsCount   int
	distributedSnapshotFD         int
	distributedSnapshotAllowCopy  bool
	distributedPrefetchBytes      uint64
	distributedPrefetchBehind     uint64
	distributedPushPolicy         string
	distributedPushHighWater      uint64
	distributedLeaveSourcePaused  bool
}

// Name implements subcommands.Command.Name.
func (*TimeTravel) Name() string { return "timetravel" }

// Synopsis implements subcommands.Command.Synopsis.
func (*TimeTravel) Synopsis() string {
	return "manage experimental in-memory TimeTravelContainer branches"
}

// Usage implements subcommands.Command.Usage.
func (*TimeTravel) Usage() string {
	return "timetravel [flags] <container-id> branches\n" +
		"timetravel [flags] <container-id> fork <new-branch-id> [<new-branch-id> ...]\n" +
		"timetravel [flags] <container-id> distributed-capture <target-branch-id>\n" +
		"timetravel [flags] <container-id> distributed-capture-many <batch-id>\n" +
		"timetravel [flags] <container-id> distributed-serve <session-id>\n" +
		"timetravel [flags] <container-id> distributed-export <session-id>\n" +
		"timetravel [flags] <container-id> distributed-dependency <branch-id>\n" +
		"timetravel [flags] <container-id> distributed-prefetch <branch-id>\n" +
		"timetravel [flags] <container-id> distributed-drain <branch-id>\n" +
		"timetravel [flags] <container-id> distributed-filesystem-drain <branch-id>\n" +
		"timetravel [flags] <container-id> distributed-handoff <branch-id>\n" +
		"timetravel [flags] <container-id> distributed-status|distributed-cancel|distributed-complete <session-id>\n" +
		"timetravel [flags] <container-id> distributed-restore <session-id>\n" +
		"timetravel [flags] <container-id> pause|resume|quarantine|unquarantine|destroy <branch-id>\n" +
		"timetravel [flags] <container-id> rename <branch-id> <new-branch-id>\n" +
		"timetravel [flags] <container-id> checkpoint <branch-id>\n" +
		"timetravel [flags] <container-id> dehydrate-export <branch-id>\n" +
		"timetravel [flags] <container-id> dehydrate-restore <branch-id>\n" +
		"timetravel [flags] <container-id> ps|size|signal|wait|prepare-stop <branch-id>\n" +
		"timetravel [flags] <container-id> exec-list|exec-wait|exec-signal <branch-id>\n" +
		"timetravel [flags] <container-id> console-read|console-stat|console-watch|console-wait|console-write|console-close-stdin|console-resize|console-follow <branch-id>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (t *TimeTravel) SetFlags(f *flag.FlagSet) {
	f.StringVar(&t.source, "source", boot.DefaultBranchID, "source branch for fork")
	f.BoolVar(&t.paused, "paused", false, "leave a forked branch paused")
	f.BoolVar(&t.quarantine, "quarantine", false, "fork a branch that is cut off from outside the sandbox: connections stay up and go nowhere, and nothing outside can tell it exists")
	f.IntVar(&t.pid, "pid", 0, "virtual process ID; 0 selects the container init")
	f.StringVar(&t.execID, "exec-id", "", "stable branch-local exec session ID")
	f.IntVar(&t.signal, "signal", int(linux.SIGTERM), "signal number for the signal action")
	f.BoolVar(&t.all, "all", false, "signal every process in the container")
	f.StringVar(&t.consoleID, "console", "init", "internal console session ID")
	f.StringVar(&t.consoleStream, "stream", "stdout", "console output stream: stdout or stderr")
	f.IntVar(&t.consoleMaxBytes, "max-bytes", 65536, "maximum bytes returned by console-read")
	f.StringVar(&t.consoleData, "data-base64", "", "base64-encoded bytes written by console-write")
	f.BoolVar(&t.consoleWaitStdout, "wait-stdout", false, "wait for console stdout readability")
	f.BoolVar(&t.consoleWaitStderr, "wait-stderr", false, "wait for console stderr readability")
	f.BoolVar(&t.consoleWaitStdin, "wait-stdin", false, "wait for console stdin writability")
	f.DurationVar(&t.consoleWaitTimeout, "wait-timeout", time.Second, "maximum console readiness wait")
	f.IntVar(&t.consoleRows, "rows", 0, "terminal window rows for console-resize")
	f.IntVar(&t.consoleCols, "cols", 0, "terminal window columns for console-resize")
	f.IntVar(&t.consoleXPixels, "x-pixels", 0, "terminal window width in pixels for console-resize")
	f.IntVar(&t.consoleYPixels, "y-pixels", 0, "terminal window height in pixels for console-resize")
	f.StringVar(&t.consoleLog, "console-log", "", "host path for multiplexed console-follow output")
	f.IntVar(&t.consoleReadyFD, "console-ready-fd", -1, "internal FD signaled after console-follow connects")
	f.StringVar(&t.consoleWaitStatus, "console-wait-status", "", "host path where console-follow atomically records init wait status")
	f.StringVar(&t.consoleRetentionStats, "console-retention-stats", "", "host path for cumulative console retention accounting")
	f.StringVar(&t.consolePIDFile, "console-pid-file", "", "host path for the console-follow process identity")
	f.Int64Var(&t.consoleLogMax, "console-log-max-bytes", 0, "rotate console-follow output after this many bytes; 0 disables rotation")
	f.IntVar(&t.consoleLogFiles, "console-log-max-files", 2, "number of console-follow log generations to retain")
	f.StringVar(&t.checkpointImagePath, "image-path", "", "directory path for branch checkpoint state")
	f.Var(newCheckpointCompressionValue(statefile.CompressionLevelDefault, &t.checkpointCompression), "compression", "checkpoint compression: none or flate-best-speed")
	f.IntVar(&t.dehydrationFD, "dehydration-fd", -1, "inherited file for durable dehydration metadata")
	f.IntVar(&t.dehydrationConfigFD, "dehydration-config-fd", -1, "inherited FD containing durable dehydration restore arguments")
	f.IntVar(&t.dehydrationSnapshotsStartFD, "dehydration-snapshots-start-fd", -1, "first inherited dehydration snapshot destination")
	f.IntVar(&t.dehydrationSnapshotsCount, "dehydration-snapshots-count", 0, "number of inherited dehydration snapshot destinations")
	f.BoolVar(&t.dehydrationSnapshotAllowCopy, "dehydration-snapshot-allow-copy", false, "allow full copying when dehydration snapshot reflinks are unsupported")
	f.IntVar(&t.dehydrationMemoryRepositoryFD, "dehydration-memory-repository-fd", -1, "inherited sparse dehydration memory repository directory")
	f.IntVar(&t.dehydrationGraphStreamFD, "dehydration-graph-stream-fd", -1, "inherited graph-native dehydration capture stream")
	f.IntVar(&t.dehydrationGraphManifestFD, "dehydration-graph-manifest-fd", -1, "inherited graph-native dehydration memory manifest")
	f.IntVar(&t.dehydrationMemoryRemoteFD, "dehydration-memory-remote-fd", -1, "inherited cold memory chunk stream")
	f.IntVar(&t.dehydrationTargetsStartFD, "dehydration-targets-start-fd", -1, "first inherited empty dehydration target memory file")
	f.IntVar(&t.dehydrationTargetsCount, "dehydration-targets-count", 0, "number of inherited dehydration target memory files")
	f.IntVar(&t.dehydrationFilesystemsStartFD, "dehydration-filesystems-start-fd", -1, "first inherited cold filesystem range stream")
	f.IntVar(&t.dehydrationFilesystemsCount, "dehydration-filesystems-count", 0, "number of inherited cold filesystem range streams")
	f.StringVar(&t.distributedSessionID, "distributed-session", "", "distributed fork transaction/session ID")
	f.StringVar(&t.distributedFamilyID, "distributed-family", "", "portable distributed fork family identity")
	f.StringVar(&t.distributedListen, "distributed-listen", "", "host TCP address for the source page server")
	f.StringVar(&t.distributedAdvertise, "distributed-advertise", "", "page server address advertised to the target")
	f.Uint64Var(&t.distributedMaxTransfer, "distributed-max-transfer", 0, "maximum bytes in one remote page transfer")
	f.IntVar(&t.distributedConfigFD, "distributed-config-fd", -1, "inherited FD containing a secret distributed restore request")
	f.IntVar(&t.distributedMetadataFD, "distributed-metadata-fd", -1, "inherited immutable distributed restore metadata")
	f.BoolVar(&t.distributedExternal, "distributed-external-listener", false, "ttcd owns and relays the page listener")
	f.IntVar(&t.distributedConnectionFD, "distributed-connection-fd", -1, "inherited connected page stream")
	f.IntVar(&t.distributedConnectionsStartFD, "distributed-connections-start-fd", -1, "first inherited target page stream")
	f.IntVar(&t.distributedConnectionsCount, "distributed-connections-count", 0, "number of inherited target page streams")
	f.IntVar(&t.distributedSnapshotFD, "distributed-snapshot-fd", -1, "inherited durable distributed-memory snapshot")
	f.BoolVar(&t.distributedSnapshotAllowCopy, "distributed-snapshot-allow-copy", false, "allow full copying when distributed snapshot reflinks are unsupported")
	f.Uint64Var(&t.distributedPrefetchBytes, "distributed-prefetch-bytes", 0, "remote-memory bytes to prefetch after a fault")
	f.Uint64Var(&t.distributedPrefetchBehind, "distributed-prefetch-behind-bytes", 0, "remote-memory bytes to prefetch before a fault")
	f.StringVar(&t.distributedPushPolicy, "distributed-push-policy", "demand-only", "reverse push policy")
	f.Uint64Var(&t.distributedPushHighWater, "distributed-push-high-water", 0, "retained source bytes that trigger pressure push")
	f.BoolVar(&t.distributedLeaveSourcePaused, "distributed-leave-source-paused", false, "leave a singular distributed capture paused for migration")
}

func publishConsoleFollowerPID(path string) (func(), error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".console-pid-*")
	if err != nil {
		return nil, fmt.Errorf("creating console follower PID file: %w", err)
	}
	tempName := temp.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return nil, fmt.Errorf("setting console follower PID permissions: %w", err)
	}
	if _, err := fmt.Fprintf(temp, "%d\n", os.Getpid()); err != nil {
		temp.Close()
		return nil, fmt.Errorf("writing console follower PID: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return nil, fmt.Errorf("syncing console follower PID: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("closing console follower PID: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return nil, fmt.Errorf("publishing console follower PID: %w", err)
	}
	cleanupTemp = false
	return func() { _ = os.Remove(path) }, nil
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (t *TimeTravel) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := t.loadContainer(conf, f, container.LoadOpts{SkipCheck: true})
	if err != nil {
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (t *TimeTravel) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() < 2 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	action := f.Arg(1)
	wantArgs := 3
	switch action {
	case "branches":
		wantArgs = 2
	case "rename":
		// The old name and the new one.
		wantArgs = 4
	}
	if (action == "fork" && f.NArg() < wantArgs) || (action != "fork" && f.NArg() != wantArgs) {
		f.Usage()
		return subcommands.ExitUsageError
	}

	conf := args[0].(*config.Config)
	cont, err := t.loadContainer(conf, f, container.LoadOpts{SkipCheck: true})
	if err != nil {
		util.Fatalf("loading container: %v", err)
	}

	var output any
	switch action {
	case "branches":
		output, err = cont.Sandbox.ListBranches()
	case "fork":
		destinations := f.Args()[2:]
		if len(destinations) == 1 {
			output, err = cont.Sandbox.ForkBranch(&boot.ForkBranchArgs{
				Source:     t.source,
				ID:         destinations[0],
				Paused:     t.paused,
				Quarantine: t.quarantine,
			})
		} else {
			output, err = cont.Sandbox.ForkBranches(&boot.ForkBranchesArgs{
				Source:     t.source,
				IDs:        destinations,
				Paused:     t.paused,
				Quarantine: t.quarantine,
			})
		}
	case "distributed-capture":
		if t.distributedSessionID == "" || t.distributedFamilyID == "" || t.distributedMaxTransfer == 0 {
			util.Fatalf("--distributed-session, --distributed-family, and --distributed-max-transfer are required")
		}
		if t.distributedExternal {
			if t.distributedAdvertise == "" {
				util.Fatalf("--distributed-advertise is required with --distributed-external-listener")
			}
			output, err = cont.Sandbox.CaptureDistributedFork(&boot.DistributedForkCaptureArgs{
				SessionID: t.distributedSessionID, Source: t.source, FamilyID: t.distributedFamilyID,
				TargetBranchID: f.Arg(2), AdvertiseAddress: t.distributedAdvertise,
				MaxTransferBytes: t.distributedMaxTransfer, ExternalListener: true,
				PushPolicy: ttcremote.TCPPushPolicy(t.distributedPushPolicy), PushHighWater: t.distributedPushHighWater,
				LeaveSourcePaused: t.distributedLeaveSourcePaused,
			})
			break
		}
		if t.distributedListen == "" {
			util.Fatalf("--distributed-listen is required")
		}
		listener, listenErr := net.Listen("tcp", t.distributedListen)
		if listenErr != nil {
			util.Fatalf("opening host distributed page listener: %v", listenErr)
		}
		tcpListener, ok := listener.(*net.TCPListener)
		if !ok {
			listener.Close()
			util.Fatalf("distributed page listener is %T, want TCP", listener)
		}
		listenerFile, fileErr := tcpListener.File()
		if fileErr != nil {
			listener.Close()
			util.Fatalf("duplicating distributed page listener: %v", fileErr)
		}
		advertise, advertiseErr := distributedAdvertiseAddress(t.distributedAdvertise, listener.Addr().String())
		if advertiseErr != nil {
			listenerFile.Close()
			listener.Close()
			util.Fatalf("resolving distributed advertise address: %v", advertiseErr)
		}
		output, err = cont.Sandbox.CaptureDistributedFork(&boot.DistributedForkCaptureArgs{
			FilePayload: urpc.FilePayload{Files: []*os.File{listenerFile}},
			SessionID:   t.distributedSessionID, Source: t.source, FamilyID: t.distributedFamilyID,
			TargetBranchID: f.Arg(2), AdvertiseAddress: advertise, MaxTransferBytes: t.distributedMaxTransfer,
			PushPolicy: ttcremote.TCPPushPolicy(t.distributedPushPolicy), PushHighWater: t.distributedPushHighWater,
			LeaveSourcePaused: t.distributedLeaveSourcePaused,
		})
		listenerFile.Close()
		listener.Close()
	case "distributed-capture-many":
		if t.distributedConfigFD < 0 {
			util.Fatalf("--distributed-config-fd is required")
		}
		configFile := os.NewFile(uintptr(t.distributedConfigFD), "distributed-fork-batch-config")
		if configFile == nil {
			util.Fatalf("invalid distributed fork batch config FD")
		}
		decoder := json.NewDecoder(io.LimitReader(configFile, 1<<20))
		decoder.DisallowUnknownFields()
		var captureArgs boot.DistributedForkCaptureManyArgs
		if err := decoder.Decode(&captureArgs); err != nil {
			configFile.Close()
			util.Fatalf("decoding distributed fork batch config: %v", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			configFile.Close()
			util.Fatalf("distributed fork batch config contains trailing data")
		}
		if err := configFile.Close(); err != nil {
			util.Fatalf("closing distributed fork batch config: %v", err)
		}
		output, err = cont.Sandbox.CaptureDistributedForkMany(&captureArgs)
	case "distributed-serve":
		if t.distributedConnectionFD < 0 {
			util.Fatalf("--distributed-connection-fd is required")
		}
		connection := os.NewFile(uintptr(t.distributedConnectionFD), "distributed-page-connection")
		if connection == nil {
			util.Fatalf("invalid distributed page connection FD")
		}
		err = cont.Sandbox.ServeDistributedForkConnection(&boot.DistributedForkServeArgs{
			FilePayload: urpc.FilePayload{Files: []*os.File{connection}}, SessionID: f.Arg(2),
		})
		connection.Close()
	case "distributed-export":
		sparse := t.dehydrationMemoryRepositoryFD >= 0
		fd := t.distributedSnapshotFD
		name := "distributed-memory-snapshot"
		if sparse {
			fd = t.dehydrationMemoryRepositoryFD
			name = "distributed-memory-repository"
		}
		if fd < 0 {
			util.Fatalf("a distributed snapshot or sparse repository FD is required")
		}
		snapshot := os.NewFile(uintptr(fd), name)
		if snapshot == nil {
			util.Fatalf("invalid distributed export FD")
		}
		output, err = cont.Sandbox.ExportDistributedForkSnapshot(&boot.DistributedForkExportArgs{
			FilePayload: urpc.FilePayload{Files: []*os.File{snapshot}}, SessionID: f.Arg(2),
			AllowCopy: t.distributedSnapshotAllowCopy, Sparse: sparse,
		})
		snapshot.Close()
	case "distributed-status":
		output, err = cont.Sandbox.DistributedForkSessionStatus(f.Arg(2))
	case "distributed-dependency":
		output, err = cont.Sandbox.DistributedForkDependencyStatus(f.Arg(2))
	case "distributed-prefetch":
		err = cont.Sandbox.SetDistributedForkPrefetchWindow(f.Arg(2), t.distributedPrefetchBehind, t.distributedPrefetchBytes)
	case "distributed-drain":
		err = cont.Sandbox.DrainDistributedForkMemory(f.Arg(2))
	case "distributed-filesystem-drain":
		err = cont.Sandbox.DrainDistributedForkFilesystems(f.Arg(2))
	case "distributed-handoff":
		if t.dehydrationMemoryRepositoryFD < 0 || t.distributedSessionID == "" {
			util.Fatalf("--dehydration-memory-repository-fd and --distributed-session=<manifest> are required")
		}
		manifest, parseErr := ttcchunk.ParseManifestID(t.distributedSessionID)
		if parseErr != nil {
			util.Fatalf("parsing distributed sparse manifest: %v", parseErr)
		}
		repository := os.NewFile(uintptr(t.dehydrationMemoryRepositoryFD), "distributed-handoff-memory-repository")
		if repository == nil {
			util.Fatalf("invalid distributed handoff repository FD")
		}
		err = cont.Sandbox.HandoffDistributedForkMemory(&boot.DistributedForkHandoffArgs{
			FilePayload: urpc.FilePayload{Files: []*os.File{repository}}, BranchID: f.Arg(2), Manifest: manifest,
		})
		repository.Close()
	case "distributed-cancel":
		err = cont.Sandbox.CancelDistributedForkSession(f.Arg(2))
	case "distributed-complete":
		err = cont.Sandbox.CompleteDistributedForkSession(f.Arg(2))
	case "distributed-restore":
		if t.distributedConfigFD < 0 {
			util.Fatalf("--distributed-config-fd is required")
		}
		configFile := os.NewFile(uintptr(t.distributedConfigFD), "distributed-fork-config")
		if configFile == nil {
			util.Fatalf("invalid distributed restore config FD")
		}
		decoder := json.NewDecoder(io.LimitReader(configFile, 1<<20))
		decoder.DisallowUnknownFields()
		var restoreArgs boot.DistributedForkRestoreArgs
		if err := decoder.Decode(&restoreArgs); err != nil {
			configFile.Close()
			util.Fatalf("decoding distributed restore config: %v", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			configFile.Close()
			util.Fatalf("distributed restore config contains trailing data")
		}
		if err := configFile.Close(); err != nil {
			util.Fatalf("closing distributed restore config: %v", err)
		}
		if restoreArgs.SessionID != f.Arg(2) {
			util.Fatalf("distributed restore session identity mismatch")
		}
		if restoreArgs.SharedMetadata {
			if t.distributedMetadataFD < 0 {
				util.Fatalf("--distributed-metadata-fd is required for shared distributed metadata")
			}
			metadata := os.NewFile(uintptr(t.distributedMetadataFD), "distributed-shared-metadata")
			if metadata == nil {
				util.Fatalf("invalid inherited distributed metadata FD")
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, metadata)
			defer metadata.Close()
		}
		if restoreArgs.SharedSnapshot {
			if t.distributedSnapshotFD < 0 {
				util.Fatalf("--distributed-snapshot-fd is required for shared distributed restore")
			}
			snapshot := os.NewFile(uintptr(t.distributedSnapshotFD), "distributed-shared-memory-snapshot")
			if snapshot == nil {
				util.Fatalf("invalid inherited distributed snapshot FD")
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, snapshot)
			defer snapshot.Close()
		}
		if restoreArgs.SparseManifest != (ttcchunk.ManifestID{}) {
			if t.dehydrationMemoryRepositoryFD < 0 {
				util.Fatalf("--dehydration-memory-repository-fd is required for sparse distributed restore")
			}
			repository := os.NewFile(uintptr(t.dehydrationMemoryRepositoryFD), "distributed-sparse-memory-repository")
			if repository == nil {
				util.Fatalf("invalid inherited distributed sparse repository FD")
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, repository)
			defer repository.Close()
		}
		if t.distributedConnectionsCount < 0 || (t.distributedConnectionsCount != 0 && t.distributedConnectionsStartFD < 0) {
			util.Fatalf("invalid inherited distributed connection range")
		}
		for i := 0; i < t.distributedConnectionsCount; i++ {
			file := os.NewFile(uintptr(t.distributedConnectionsStartFD+i), "distributed-target-connection")
			if file == nil {
				util.Fatalf("invalid inherited distributed connection FD %d", t.distributedConnectionsStartFD+i)
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, file)
			defer file.Close()
		}
		output, err = cont.Sandbox.RestoreDistributedFork(&restoreArgs)
	case "pause":
		err = cont.Sandbox.PauseBranch(f.Arg(2))
	case "resume":
		err = cont.Sandbox.ResumeBranch(f.Arg(2))
	case "quarantine":
		err = cont.Sandbox.SetBranchQuarantine(f.Arg(2), true)
	case "unquarantine":
		err = cont.Sandbox.SetBranchQuarantine(f.Arg(2), false)
	case "rename":
		err = cont.Sandbox.RenameBranch(f.Arg(2), f.Arg(3))
	case "dehydrate":
		var metadataFile *os.File
		if t.dehydrationFD >= 0 {
			metadataFile = os.NewFile(uintptr(t.dehydrationFD), "dehydration-metadata")
			if metadataFile == nil {
				util.Fatalf("invalid dehydration metadata FD")
			}
			defer metadataFile.Close()
		}
		output, err = cont.Sandbox.DehydrateBranch(f.Arg(2), metadataFile)
	case "dehydrate-export":
		if t.dehydrationGraphStreamFD >= 0 {
			if t.dehydrationMemoryRepositoryFD >= 0 || t.dehydrationSnapshotsCount != 0 {
				util.Fatalf("graph dehydration stream cannot be combined with another export destination")
			}
			stream := os.NewFile(uintptr(t.dehydrationGraphStreamFD), "dehydration-graph-stream")
			if stream == nil {
				util.Fatalf("invalid dehydration graph stream FD")
			}
			defer stream.Close()
			output, err = cont.Sandbox.ExportDehydratedBranch(&boot.BranchDehydrationExportArgs{
				FilePayload: urpc.FilePayload{Files: []*os.File{stream}}, ID: f.Arg(2), GraphStream: true,
			})
			break
		}
		if t.dehydrationMemoryRepositoryFD >= 0 {
			repository := os.NewFile(uintptr(t.dehydrationMemoryRepositoryFD), "dehydration-memory-repository")
			if repository == nil {
				util.Fatalf("invalid dehydration memory repository FD")
			}
			defer repository.Close()
			output, err = cont.Sandbox.ExportDehydratedBranch(&boot.BranchDehydrationExportArgs{
				FilePayload: urpc.FilePayload{Files: []*os.File{repository}}, ID: f.Arg(2), Sparse: true,
			})
			break
		}
		if t.dehydrationSnapshotsCount < 0 || (t.dehydrationSnapshotsCount != 0 && t.dehydrationSnapshotsStartFD < 0) {
			util.Fatalf("invalid inherited dehydration snapshot range")
		}
		files := make([]*os.File, 0, t.dehydrationSnapshotsCount)
		for i := 0; i < t.dehydrationSnapshotsCount; i++ {
			file := os.NewFile(uintptr(t.dehydrationSnapshotsStartFD+i), fmt.Sprintf("dehydration-snapshot-%d", i))
			if file == nil {
				util.Fatalf("invalid inherited dehydration snapshot FD %d", t.dehydrationSnapshotsStartFD+i)
			}
			files = append(files, file)
			defer file.Close()
		}
		_, err = cont.Sandbox.ExportDehydratedBranch(&boot.BranchDehydrationExportArgs{
			FilePayload: urpc.FilePayload{Files: files},
			ID:          f.Arg(2),
			AllowCopy:   t.dehydrationSnapshotAllowCopy,
		})
	case "dehydrate-restore":
		if t.dehydrationConfigFD < 0 || t.dehydrationFD < 0 || t.dehydrationSnapshotsCount < 0 || (t.dehydrationSnapshotsCount != 0 && t.dehydrationSnapshotsStartFD < 0) || t.dehydrationTargetsCount < 0 || (t.dehydrationTargetsCount != 0 && t.dehydrationTargetsStartFD < 0) || t.dehydrationFilesystemsCount < 0 || (t.dehydrationFilesystemsCount != 0 && t.dehydrationFilesystemsStartFD < 0) {
			util.Fatalf("--dehydration-config-fd, --dehydration-fd, and a valid snapshot range are required")
		}
		configFile := os.NewFile(uintptr(t.dehydrationConfigFD), "dehydration-restore-config")
		if configFile == nil {
			util.Fatalf("invalid dehydration restore config FD")
		}
		decoder := json.NewDecoder(io.LimitReader(configFile, 1<<20))
		decoder.DisallowUnknownFields()
		var restoreArgs boot.DurableDehydrationRestoreArgs
		if err := decoder.Decode(&restoreArgs); err != nil {
			configFile.Close()
			util.Fatalf("decoding dehydration restore config: %v", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			configFile.Close()
			util.Fatalf("dehydration restore config contains trailing data")
		}
		configFile.Close()
		if restoreArgs.ID != f.Arg(2) {
			util.Fatalf("dehydration restore branch identity mismatch")
		}
		metadata := os.NewFile(uintptr(t.dehydrationFD), "dehydration-restore-metadata")
		if metadata == nil {
			util.Fatalf("invalid dehydration metadata FD")
		}
		restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, metadata)
		defer metadata.Close()
		if restoreArgs.Sparse {
			if t.dehydrationMemoryRepositoryFD < 0 || t.dehydrationSnapshotsCount != 0 || (restoreArgs.GraphSparse != (t.dehydrationGraphManifestFD >= 0)) {
				util.Fatalf("sparse dehydration restore requires a repository and no packed snapshots")
			}
			if restoreArgs.GraphSparse {
				manifest := os.NewFile(uintptr(t.dehydrationGraphManifestFD), "dehydration-restore-graph-manifest")
				if manifest == nil {
					util.Fatalf("invalid dehydration graph manifest FD")
				}
				restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, manifest)
				defer manifest.Close()
			}
			repository := os.NewFile(uintptr(t.dehydrationMemoryRepositoryFD), "dehydration-restore-memory-repository")
			if repository == nil {
				util.Fatalf("invalid dehydration memory repository FD")
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, repository)
			defer repository.Close()
			if restoreArgs.SparseRemote {
				if t.dehydrationMemoryRemoteFD < 0 {
					util.Fatalf("sparse remote dehydration restore requires a cold memory stream")
				}
				remote := os.NewFile(uintptr(t.dehydrationMemoryRemoteFD), "dehydration-restore-memory-remote")
				if remote == nil {
					util.Fatalf("invalid inherited cold memory FD %d", t.dehydrationMemoryRemoteFD)
				}
				restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, remote)
				defer remote.Close()
			}
		}
		for i := 0; i < t.dehydrationSnapshotsCount; i++ {
			file := os.NewFile(uintptr(t.dehydrationSnapshotsStartFD+i), fmt.Sprintf("dehydration-restore-snapshot-%d", i))
			if file == nil {
				util.Fatalf("invalid inherited dehydration snapshot FD %d", t.dehydrationSnapshotsStartFD+i)
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, file)
			defer file.Close()
		}
		for i := 0; i < t.dehydrationTargetsCount; i++ {
			file := os.NewFile(uintptr(t.dehydrationTargetsStartFD+i), fmt.Sprintf("dehydration-target-memory-%d", i))
			if file == nil {
				util.Fatalf("invalid inherited dehydration target FD %d", t.dehydrationTargetsStartFD+i)
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, file)
			defer file.Close()
		}
		if restoreArgs.FilesystemConnections != t.dehydrationFilesystemsCount {
			util.Fatalf("dehydration restore configuration has %d filesystem streams, command inherited %d", restoreArgs.FilesystemConnections, t.dehydrationFilesystemsCount)
		}
		for i := 0; i < t.dehydrationFilesystemsCount; i++ {
			file := os.NewFile(uintptr(t.dehydrationFilesystemsStartFD+i), fmt.Sprintf("dehydration-filesystem-stream-%d", i))
			if file == nil {
				util.Fatalf("invalid inherited dehydration filesystem FD %d", t.dehydrationFilesystemsStartFD+i)
			}
			restoreArgs.FilePayload.Files = append(restoreArgs.FilePayload.Files, file)
			defer file.Close()
		}
		output, err = cont.Sandbox.RestoreDurableDehydration(&restoreArgs)
	case "rehydrate":
		err = cont.Sandbox.RehydrateBranch(f.Arg(2))
	case "destroy":
		err = cont.Sandbox.DestroyBranch(f.Arg(2))
	case "prepare-stop":
		err = cont.Sandbox.PrepareStopBranch(f.Arg(2))
	case "checkpoint":
		if t.checkpointImagePath == "" {
			util.Fatalf("--image-path is required for checkpoint")
		}
		if err := os.MkdirAll(t.checkpointImagePath, 0700); err != nil {
			util.Fatalf("creating checkpoint image path: %v", err)
		}
		err = cont.Sandbox.CheckpointBranch(conf, cont.ID, f.Arg(2), t.checkpointImagePath, sandbox.CheckpointOpts{
			Compression:                t.checkpointCompression.Level(),
			Resume:                     false,
			SaveRestoreExecContainerID: cont.ID,
		})
	case "ps":
		output, err = cont.Sandbox.BranchProcesses(f.Arg(2), cont.ID)
	case "size":
		output, err = cont.Sandbox.BranchRootfsSize(f.Arg(2), cont.ID)
	case "stats":
		output, err = cont.Sandbox.FamilyResidentMemory()
	case "signal":
		mode := boot.DeliverToProcess
		if t.all {
			mode = boot.DeliverToAllProcesses
		}
		err = cont.Sandbox.SignalBranch(&boot.BranchSignalArgs{
			BranchID: f.Arg(2),
			CID:      cont.ID,
			PID:      int32(t.pid),
			Signo:    int32(t.signal),
			Mode:     mode,
		})
	case "wait":
		var status uint32
		status, err = cont.Sandbox.WaitBranch(&boot.BranchWaitArgs{
			BranchID: f.Arg(2),
			CID:      cont.ID,
			PID:      int32(t.pid),
		})
		output = map[string]uint32{"wait_status": status}
	case "exec-list":
		output, err = cont.Sandbox.ExecSessions(&boot.ExecSessionsArgs{
			BranchID: f.Arg(2),
			CID:      cont.ID,
		})
	case "exec-wait":
		if t.execID == "" {
			util.Fatalf("--exec-id is required for exec-wait")
		}
		var status uint32
		status, err = cont.Sandbox.WaitExecSession(&boot.ExecSessionArgs{
			BranchID: f.Arg(2),
			ExecID:   t.execID,
		})
		output = map[string]uint32{"wait_status": status}
	case "exec-signal":
		if t.execID == "" {
			util.Fatalf("--exec-id is required for exec-signal")
		}
		err = cont.Sandbox.SignalExecSession(&boot.ExecSessionArgs{
			BranchID: f.Arg(2),
			ExecID:   t.execID,
			Signo:    int32(t.signal),
		})
	case "console-read":
		stream := kernel.InternalConsoleStdout
		switch t.consoleStream {
		case "stdout":
		case "stderr":
			stream = kernel.InternalConsoleStderr
		default:
			util.Fatalf("unknown console stream %q", t.consoleStream)
		}
		output, err = cont.Sandbox.ReadConsole(&boot.ConsoleReadArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
			Stream:    stream,
			MaxBytes:  t.consoleMaxBytes,
		})
	case "console-stat":
		output, err = cont.Sandbox.ObserveConsole(&boot.ConsoleObservationArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
		})
	case "console-watch":
		err = cont.Sandbox.WatchConsole(&boot.ConsoleObservationArgs{
			BranchID: f.Arg(2), ConsoleID: t.consoleID,
		}, &util.Writer{})
	case "console-wait":
		output, err = cont.Sandbox.WaitConsole(&boot.ConsoleWaitArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
			Want: kernel.InternalConsoleReadiness{
				Stdout: t.consoleWaitStdout,
				Stderr: t.consoleWaitStderr,
				Stdin:  t.consoleWaitStdin,
			},
			Timeout: t.consoleWaitTimeout,
		})
	case "console-write":
		data, decodeErr := base64.StdEncoding.DecodeString(t.consoleData)
		if decodeErr != nil {
			util.Fatalf("decoding --data-base64: %v", decodeErr)
		}
		output, err = cont.Sandbox.WriteConsole(&boot.ConsoleWriteArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
			Data:      data,
		})
	case "console-close-stdin":
		err = cont.Sandbox.CloseConsoleStdin(&boot.ConsoleCloseStdinArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
		})
	case "console-resize":
		values := []struct {
			name  string
			value int
		}{
			{"rows", t.consoleRows},
			{"cols", t.consoleCols},
			{"x-pixels", t.consoleXPixels},
			{"y-pixels", t.consoleYPixels},
		}
		for _, value := range values {
			if value.value < 0 || value.value > 65535 {
				util.Fatalf("--%s=%d is outside 0..65535", value.name, value.value)
			}
		}
		err = cont.Sandbox.ResizeConsole(&boot.ConsoleResizeArgs{
			BranchID:  f.Arg(2),
			ConsoleID: t.consoleID,
			Rows:      uint16(t.consoleRows),
			Cols:      uint16(t.consoleCols),
			XPixels:   uint16(t.consoleXPixels),
			YPixels:   uint16(t.consoleYPixels),
		})
	case "console-follow":
		if t.consolePIDFile != "" {
			cleanupPID, pidErr := publishConsoleFollowerPID(t.consolePIDFile)
			if pidErr != nil {
				err = pidErr
				break
			}
			defer cleanupPID()
		}
		type branchWaitResult struct {
			err error
		}
		var writer io.Writer = &util.Writer{}
		var logWriter *ttcconsole.RotatingWriter
		if t.consoleLog != "" {
			logWriter, err = ttcconsole.NewRotatingWriterWithStats(t.consoleLog, t.consoleLogMax, t.consoleLogFiles, t.consoleRetentionStats)
			if err != nil {
				break
			}
			writer = logWriter
		}
		var ready func() error
		var readyFile *os.File
		if t.consoleReadyFD >= 0 {
			readyFile = os.NewFile(uintptr(t.consoleReadyFD), "console-ready")
			if readyFile == nil {
				err = fmt.Errorf("invalid console readiness FD %d", t.consoleReadyFD)
				break
			}
			defer readyFile.Close()
			ready = func() error {
				_, err := readyFile.Write([]byte{1})
				return err
			}
		}
		if t.consoleWaitStatus != "" {
			finalReader, finalWriter, pipeErr := os.Pipe()
			if pipeErr != nil {
				err = fmt.Errorf("creating final console-output pipe: %w", pipeErr)
				break
			}
			statusReader, statusWriter, pipeErr := os.Pipe()
			if pipeErr != nil {
				_ = finalReader.Close()
				_ = finalWriter.Close()
				err = fmt.Errorf("creating console wait-status pipe: %w", pipeErr)
				break
			}
			releaseReader, releaseWriter, pipeErr := os.Pipe()
			if pipeErr != nil {
				_ = finalReader.Close()
				_ = finalWriter.Close()
				_ = statusReader.Close()
				_ = statusWriter.Close()
				err = fmt.Errorf("creating console drain-release pipe: %w", pipeErr)
				break
			}
			registrationWriter := readyFile
			if registrationWriter == nil {
				registrationWriter, pipeErr = os.OpenFile("/dev/null", os.O_WRONLY, 0)
				if pipeErr != nil {
					_ = finalReader.Close()
					_ = finalWriter.Close()
					_ = statusReader.Close()
					_ = statusWriter.Close()
					_ = releaseReader.Close()
					_ = releaseWriter.Close()
					err = fmt.Errorf("opening registration readiness sink: %w", pipeErr)
					break
				}
				defer registrationWriter.Close()
			}
			attachedReader, attachedWriter, pipeErr := os.Pipe()
			if pipeErr != nil {
				_ = finalReader.Close()
				_ = finalWriter.Close()
				_ = statusReader.Close()
				_ = statusWriter.Close()
				_ = releaseReader.Close()
				_ = releaseWriter.Close()
				err = fmt.Errorf("creating console attachment pipe: %w", pipeErr)
				break
			}
			files := []*os.File{finalWriter, statusWriter, releaseReader, registrationWriter, attachedWriter}
			waitCh := make(chan branchWaitResult, 1)
			go func() {
				_, waitErr := cont.Sandbox.FollowConsole(&boot.ConsoleFollowArgs{
					FilePayload: urpc.FilePayload{Files: files},
					BranchID:    f.Arg(2),
					CID:         cont.ID,
					ExecID:      t.execID,
					ConsoleID:   t.consoleID,
				})
				_ = finalWriter.Close()
				_ = statusWriter.Close()
				_ = releaseReader.Close()
				_ = attachedWriter.Close()
				waitCh <- branchWaitResult{err: waitErr}
			}()
			var attached [1]byte
			if _, attachErr := io.ReadFull(attachedReader, attached[:]); attachErr != nil {
				err = fmt.Errorf("waiting for console attachment readiness: %w", attachErr)
			}
			_ = attachedReader.Close()
			drain := consoleDrainResult{}
			if err == nil {
				drain = drainConsole(cont.Sandbox, f.Arg(2), t.consoleID, writer, nil)
			}
			// A per-read uRPC connection may lose the shutdown race after init
			// exits. The coordinator still owns the Kernel lease and performs the
			// authoritative final drain below. Report the client error only when
			// that handoff also fails.
			if _, releaseErr := releaseWriter.Write([]byte{1}); err == nil && releaseErr != nil {
				err = fmt.Errorf("handing console reads to coordinator: %w", releaseErr)
			}
			_ = releaseWriter.Close()
			copyErr := ttcconsole.CopyFinalFrames(writer, finalReader, drain.stdoutEOF, drain.stderrEOF)
			_ = finalReader.Close()
			var encodedStatus [4]byte
			_, statusErr := io.ReadFull(statusReader, encodedStatus[:])
			_ = statusReader.Close()
			result := <-waitCh
			if err == nil {
				switch {
				case copyErr != nil:
					err = copyErr
				case statusErr != nil && result.err != nil:
					err = fmt.Errorf("console coordinator failed: %v; reading wait status: %w", result.err, statusErr)
				case statusErr != nil:
					err = fmt.Errorf("reading console wait status: %w", statusErr)
				case result.err != nil:
					err = result.err
				default:
					err = writeConsoleWaitStatus(t.consoleWaitStatus, binary.BigEndian.Uint32(encodedStatus[:]))
				}
			}
			if err != nil && drain.err != nil {
				err = fmt.Errorf("%w (client console drain also failed: %v)", err, drain.err)
			}
		} else {
			err = followConsole(cont.Sandbox, f.Arg(2), t.consoleID, writer, ready)
		}
		if logWriter != nil {
			if closeErr := logWriter.Close(); err == nil {
				err = closeErr
			}
		}
	default:
		f.Usage()
		return subcommands.ExitUsageError
	}
	if err != nil {
		util.Fatalf("timetravel %s failed: %v", action, err)
	}
	if output != nil {
		encoder := json.NewEncoder(&util.Writer{})
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(output); err != nil {
			util.Fatalf("encoding branch information: %v", err)
		}
	}
	return subcommands.ExitSuccess
}

func distributedAdvertiseAddress(configured, bound string) (string, error) {
	if configured == "" {
		return bound, nil
	}
	host, port, err := net.SplitHostPort(configured)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", fmt.Errorf("advertise host is empty")
	}
	if port == "0" || port == "" {
		_, boundPort, err := net.SplitHostPort(bound)
		if err != nil {
			return "", err
		}
		port = boundPort
	}
	return net.JoinHostPort(host, port), nil
}

func writeConsoleWaitStatus(path string, status uint32) error {
	dir := filepath.Dir(path)
	tempFile, err := os.CreateTemp(dir, ".wait-status-*")
	if err != nil {
		return fmt.Errorf("creating temporary console wait status in %q: %w", dir, err)
	}
	tempPath := tempFile.Name()
	renamed := false
	defer func() {
		_ = tempFile.Close()
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tempFile.Chmod(0600); err != nil {
		return fmt.Errorf("setting console wait status permissions: %w", err)
	}
	if _, err := tempFile.WriteString(strconv.FormatUint(uint64(status), 10) + "\n"); err != nil {
		return fmt.Errorf("writing console wait status: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("closing console wait status: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("installing console wait status at %q: %w", path, err)
	}
	renamed = true
	return nil
}

type consoleDrainResult struct {
	stdoutEOF bool
	stderrEOF bool
	err       error
}

// followConsole drains both output streams into versioned TTC multiplexed
// frames. DecodeFrame also accepts the legacy Docker-compatible representation.
// It remains connected until both guest writers close.
func followConsole(s *sandbox.Sandbox, branchID, consoleID string, writer io.Writer, ready func() error) error {
	return drainConsole(s, branchID, consoleID, writer, ready).err
}

func drainConsole(s *sandbox.Sandbox, branchID, consoleID string, writer io.Writer, ready func() error) consoleDrainResult {
	type streamState struct {
		kernelStream kernel.InternalConsoleStream
		frameStream  byte
		eof          bool
	}
	streams := []streamState{
		{kernelStream: kernel.InternalConsoleStdout, frameStream: ttcconsole.StreamStdout},
		{kernelStream: kernel.InternalConsoleStderr, frameStream: ttcconsole.StreamStderr},
	}
	finish := func(err error) consoleDrainResult {
		return consoleDrainResult{
			stdoutEOF: streams[0].eof,
			stderrEOF: streams[1].eof,
			err:       err,
		}
	}
	for {
		progress := false
		allEOF := true
		for i := range streams {
			stream := &streams[i]
			if stream.eof {
				continue
			}
			allEOF = false
			readResult, err := s.ReadConsole(&boot.ConsoleReadArgs{
				BranchID:  branchID,
				ConsoleID: consoleID,
				Stream:    stream.kernelStream,
				MaxBytes:  65536,
			})
			if err != nil {
				return finish(err)
			}
			if len(readResult.Data) != 0 {
				if err := ttcconsole.WriteTimestampedFrame(writer, stream.frameStream, time.Now().UTC(), readResult.Data); err != nil {
					return finish(fmt.Errorf("writing internal console stream %d: %w", stream.frameStream, err))
				}
				progress = true
			}
			if readResult.EOF {
				if err := ttcconsole.WriteTimestampedFrame(writer, stream.frameStream, time.Now().UTC(), nil); err != nil {
					return finish(fmt.Errorf("writing internal console EOF %d: %w", stream.frameStream, err))
				}
				stream.eof = true
				progress = true
			}
		}
		if ready != nil {
			if err := ready(); err != nil {
				return finish(fmt.Errorf("signaling console readiness: %w", err))
			}
			ready = nil
		}
		if allEOF {
			return finish(nil)
		}
		if !progress {
			_, err := s.WaitConsole(&boot.ConsoleWaitArgs{
				BranchID:  branchID,
				ConsoleID: consoleID,
				Want: kernel.InternalConsoleReadiness{
					Stdout: !streams[0].eof,
					Stderr: !streams[1].eof,
				},
				Timeout: time.Second,
			})
			if err != nil {
				return finish(err)
			}
		}
	}
}
