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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/sandbox"
	ttosconsole "gvisor.dev/gvisor/runsc/ttos/console"
)

// TimeTravel controls experimental in-memory sandbox branches.
type TimeTravel struct {
	containerLoader
	source                string
	path                  string
	outputPath            string
	paused                bool
	pid                   int
	execID                string
	signal                int
	all                   bool
	consoleID             string
	consoleStream         string
	consoleMaxBytes       int
	consoleData           string
	consoleWaitStdout     bool
	consoleWaitStderr     bool
	consoleWaitStdin      bool
	consoleWaitTimeout    time.Duration
	consoleRows           int
	consoleCols           int
	consoleXPixels        int
	consoleYPixels        int
	consoleLog            string
	consoleReadyFD        int
	consoleWaitStatus     string
	consoleRetentionStats string
	consolePIDFile        string
	consoleLogMax         int64
	consoleLogFiles       int
}

// Name implements subcommands.Command.Name.
func (*TimeTravel) Name() string { return "timetravel" }

// Synopsis implements subcommands.Command.Synopsis.
func (*TimeTravel) Synopsis() string {
	return "manage experimental in-memory TimeTravelOS branches"
}

// Usage implements subcommands.Command.Usage.
func (*TimeTravel) Usage() string {
	return "timetravel [flags] <container-id> branches\n" +
		"timetravel [flags] <container-id> check <branch-id>\n" +
		"timetravel [flags] <container-id> fork <new-branch-id>\n" +
		"timetravel [flags] <container-id> pause|resume|destroy <branch-id>\n" +
		"timetravel [flags] <container-id> ps|size|filesystem-size|filesystem-tar|signal|wait <branch-id>\n" +
		"timetravel [flags] <container-id> exec-list|exec-wait|exec-signal <branch-id>\n" +
		"timetravel [flags] <container-id> console-read|console-wait|console-write|console-close-stdin|console-resize|console-follow <branch-id>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (t *TimeTravel) SetFlags(f *flag.FlagSet) {
	f.StringVar(&t.source, "source", boot.DefaultBranchID, "source branch for fork")
	f.StringVar(&t.path, "path", "", "container path for filesystem-size or filesystem-tar")
	f.StringVar(&t.outputPath, "output", "", "host tar output path for filesystem-tar")
	f.BoolVar(&t.paused, "paused", false, "leave a forked branch paused")
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
	if action == "branches" {
		wantArgs = 2
	}
	if f.NArg() != wantArgs {
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
	case "check":
		output, err = cont.Sandbox.CheckFork(f.Arg(2))
	case "fork":
		output, err = cont.Sandbox.ForkBranch(&boot.ForkBranchArgs{
			Source: t.source,
			ID:     f.Arg(2),
			Paused: t.paused,
		})
	case "pause":
		err = cont.Sandbox.PauseBranch(f.Arg(2))
	case "resume":
		err = cont.Sandbox.ResumeBranch(f.Arg(2))
	case "destroy":
		err = cont.Sandbox.DestroyBranch(f.Arg(2))
	case "ps":
		output, err = cont.Sandbox.BranchProcesses(f.Arg(2), cont.ID)
	case "size":
		output, err = cont.Sandbox.BranchRootfsSize(f.Arg(2), cont.ID)
	case "filesystem-size":
		if t.path == "" {
			err = fmt.Errorf("filesystem-size requires --path")
		} else {
			output, err = cont.Sandbox.BranchFilesystemSize(f.Arg(2), cont.ID, t.path)
		}
	case "filesystem-tar":
		if t.path == "" || t.outputPath == "" {
			err = fmt.Errorf("filesystem-tar requires --path and --output")
			break
		}
		archive, openErr := os.OpenFile(t.outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if openErr != nil {
			err = fmt.Errorf("opening filesystem tar output: %w", openErr)
			break
		}
		err = cont.Sandbox.BranchFilesystemTar(f.Arg(2), cont.ID, t.path, archive)
		if closeErr := archive.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("closing filesystem tar output: %w", closeErr)
		}
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
		var logWriter *ttosconsole.RotatingWriter
		if t.consoleLog != "" {
			logWriter, err = ttosconsole.NewRotatingWriterWithStats(t.consoleLog, t.consoleLogMax, t.consoleLogFiles, t.consoleRetentionStats)
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
				if drain.err != nil {
					err = drain.err
				}
			}
			if _, releaseErr := releaseWriter.Write([]byte{1}); err == nil && releaseErr != nil {
				err = fmt.Errorf("handing console reads to coordinator: %w", releaseErr)
			}
			_ = releaseWriter.Close()
			if copyErr := ttosconsole.CopyFinalFrames(writer, finalReader, drain.stdoutEOF, drain.stderrEOF); err == nil && copyErr != nil {
				err = copyErr
			}
			_ = finalReader.Close()
			var encodedStatus [4]byte
			_, statusErr := io.ReadFull(statusReader, encodedStatus[:])
			_ = statusReader.Close()
			result := <-waitCh
			if err == nil {
				if statusErr != nil {
					if result.err != nil {
						err = result.err
					} else {
						err = fmt.Errorf("reading console wait status: %w", statusErr)
					}
				} else {
					err = writeConsoleWaitStatus(t.consoleWaitStatus, binary.BigEndian.Uint32(encodedStatus[:]))
				}
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

// followConsole drains both output streams into versioned TTOS multiplexed
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
		{kernelStream: kernel.InternalConsoleStdout, frameStream: ttosconsole.StreamStdout},
		{kernelStream: kernel.InternalConsoleStderr, frameStream: ttosconsole.StreamStderr},
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
				if err := ttosconsole.WriteTimestampedFrame(writer, stream.frameStream, time.Now().UTC(), readResult.Data); err != nil {
					return finish(fmt.Errorf("writing internal console stream %d: %w", stream.frameStream, err))
				}
				progress = true
			}
			if readResult.EOF {
				if err := ttosconsole.WriteTimestampedFrame(writer, stream.frameStream, time.Now().UTC(), nil); err != nil {
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
