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
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
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
	source          string
	paused          bool
	pid             int
	signal          int
	all             bool
	consoleID       string
	consoleStream   string
	consoleMaxBytes int
	consoleData     string
	consoleLog      string
	consoleLogMax   int64
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
		"timetravel [flags] <container-id> fork <new-branch-id>\n" +
		"timetravel [flags] <container-id> pause|resume|destroy <branch-id>\n" +
		"timetravel [flags] <container-id> ps|signal|wait <branch-id>\n" +
		"timetravel [flags] <container-id> console-read|console-write|console-close-stdin|console-follow <branch-id>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (t *TimeTravel) SetFlags(f *flag.FlagSet) {
	f.StringVar(&t.source, "source", boot.DefaultBranchID, "source branch for fork")
	f.BoolVar(&t.paused, "paused", false, "leave a forked branch paused")
	f.IntVar(&t.pid, "pid", 0, "virtual process ID; 0 selects the container init")
	f.IntVar(&t.signal, "signal", int(linux.SIGTERM), "signal number for the signal action")
	f.BoolVar(&t.all, "all", false, "signal every process in the container")
	f.StringVar(&t.consoleID, "console", "init", "internal console session ID")
	f.StringVar(&t.consoleStream, "stream", "stdout", "console output stream: stdout or stderr")
	f.IntVar(&t.consoleMaxBytes, "max-bytes", 65536, "maximum bytes returned by console-read")
	f.StringVar(&t.consoleData, "data-base64", "", "base64-encoded bytes written by console-write")
	f.StringVar(&t.consoleLog, "console-log", "", "host path for multiplexed console-follow output")
	f.Int64Var(&t.consoleLogMax, "console-log-max-bytes", 0, "rotate console-follow output after this many bytes; 0 disables rotation")
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
	case "console-follow":
		var writer io.Writer = &util.Writer{}
		var logWriter *ttosconsole.RotatingWriter
		if t.consoleLog != "" {
			logWriter, err = ttosconsole.NewRotatingWriter(t.consoleLog, t.consoleLogMax)
			if err != nil {
				break
			}
			writer = logWriter
		}
		err = followConsole(cont.Sandbox, f.Arg(2), t.consoleID, writer)
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

// followConsole drains both output streams into Docker-compatible multiplexed
// frames. It remains connected until both guest writers close.
func followConsole(s *sandbox.Sandbox, branchID, consoleID string, writer io.Writer) error {
	type streamState struct {
		kernelStream kernel.InternalConsoleStream
		frameStream  byte
		eof          bool
	}
	streams := []streamState{
		{kernelStream: kernel.InternalConsoleStdout, frameStream: ttosconsole.StreamStdout},
		{kernelStream: kernel.InternalConsoleStderr, frameStream: ttosconsole.StreamStderr},
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
			result, err := s.ReadConsole(&boot.ConsoleReadArgs{
				BranchID:  branchID,
				ConsoleID: consoleID,
				Stream:    stream.kernelStream,
				MaxBytes:  65536,
			})
			if err != nil {
				return err
			}
			if len(result.Data) != 0 {
				if err := ttosconsole.WriteFrame(writer, stream.frameStream, result.Data); err != nil {
					return fmt.Errorf("writing internal console stream %d: %w", stream.frameStream, err)
				}
				progress = true
			}
			if result.EOF {
				if err := ttosconsole.WriteFrame(writer, stream.frameStream, nil); err != nil {
					return fmt.Errorf("writing internal console EOF %d: %w", stream.frameStream, err)
				}
				stream.eof = true
				progress = true
			}
		}
		if allEOF {
			return nil
		}
		if !progress {
			time.Sleep(2 * time.Millisecond)
		}
	}
}
