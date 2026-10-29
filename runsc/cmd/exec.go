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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/cmd/sandboxsetup"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/console"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
)

// Exec implements subcommands.Command for the "exec" command.
type Exec struct {
	containerLoader
	cwd string
	env stringSlice
	// user contains the UID and GID with which to run the new process.
	user            user
	extraKGIDs      stringSlice
	caps            stringSlice
	branch          string
	detach          bool
	execID          string
	processPath     string
	pidFile         string
	internalPidFile string

	// consoleSocket is the path to an AF_UNIX socket which will receive a
	// file descriptor referencing the master end of the console's
	// pseudoterminal.
	consoleSocket string

	// internalConsole requests Sentry-owned branch-local pipe or PTY stdio. It
	// is used by TTC instead of donating host descriptors into the sandbox.
	internalConsole         string
	internalConsoleTerminal bool
	internalConsoleStdin    bool
	internalConsoleRows     int
	internalConsoleCols     int

	// passFDs are user-supplied FDs from the host to be exposed to the
	// sandboxed app.
	passFDs sandboxsetup.FDMappings

	// execFD is the host file descriptor used for program execution.
	execFD int

	// pausedManagement admits a TTC management helper while all pre-existing
	// tasks in the selected paused branch remain stopped.
	pausedManagement bool

	// retainExecSession preserves the session and console after process exit.
	retainExecSession bool

	// allowStoppedSandbox permits TTC to address a rehydrated family whose OCI
	// init process was intentionally released during dehydration.
	allowStoppedSandbox bool

	// stdin, stdout and stderr are the exec's stdio, or what its internal
	// console's input is read from and its output written to: this
	// process's own, unless another program runs the exec in its process
	// (see ExecInProcess).
	stdin, stdout, stderr *os.File

	// execPath is the path of the program to execute. If empty, the program is
	// resolved from argv[0]. Mutually exclusive with --exec-fd.
	execPath string
}

// Name implements subcommands.Command.Name.
func (*Exec) Name() string {
	return "exec"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*Exec) Synopsis() string {
	return "execute new process inside the container"
}

// Usage implements subcommands.Command.Usage.
func (*Exec) Usage() string {
	return `exec [command options] <container-id> <command> [command options] || --process process.json <container-id>


Where "<container-id>" is the name for the instance of the container and
"<command>" is the command to be executed in the container.
"<command>" can't be empty unless a "-process" flag provided.

EXAMPLE:
If the container is configured to run /bin/ps the following will
output a list of processes running in the container:

       # runsc exec <container-id> ps

OPTIONS:
`
}

// SetFlags implements subcommands.Command.SetFlags.
func (ex *Exec) SetFlags(f *flag.FlagSet) {
	f.StringVar(&ex.cwd, "cwd", "", "current working directory")
	f.Var(&ex.env, "env", "set environment variables (e.g. '-env PATH=/bin -env TERM=xterm')")
	f.Var(&ex.user, "user", "UID (format: <uid>[:<gid>])")
	f.Var(&ex.extraKGIDs, "additional-gids", "additional gids")
	f.Var(&ex.caps, "cap", "add a capability to the bounding set for the process")
	f.StringVar(&ex.branch, "branch", boot.DefaultBranchID, "TimeTravel branch in which to execute")
	f.BoolVar(&ex.detach, "detach", false, "detach from the container's process")
	f.StringVar(&ex.execID, "exec-id", "", "stable branch-local exec session ID")
	f.BoolVar(&ex.retainExecSession, "retain-exec-session", false, "retain TTC exec session metadata and console after exit")
	f.StringVar(&ex.processPath, "process", "", "path to the process.json")
	f.StringVar(&ex.pidFile, "pid-file", "", "filename that the container pid will be written to")
	f.StringVar(&ex.internalPidFile, "internal-pid-file", "", "filename that the container-internal pid will be written to")
	f.StringVar(&ex.consoleSocket, "console-socket", "", "path to an AF_UNIX socket which will receive a file descriptor referencing the master end of the console's pseudoterminal")
	f.StringVar(&ex.internalConsole, "internal-console", "", "Sentry-owned branch-local pipe or PTY console ID")
	f.BoolVar(&ex.internalConsoleTerminal, "internal-console-terminal", false, "make --internal-console a PTY")
	f.BoolVar(&ex.internalConsoleStdin, "internal-console-stdin", false, "forward stdin to --internal-console")
	f.IntVar(&ex.internalConsoleRows, "internal-console-rows", 0, "initial internal PTY rows")
	f.IntVar(&ex.internalConsoleCols, "internal-console-cols", 0, "initial internal PTY columns")
	f.Var(&ex.passFDs, "pass-fd", "file descriptor passed to the container in M:N format, where M is the host and N is the guest descriptor (can be supplied multiple times)")
	f.IntVar(&ex.execFD, "exec-fd", -1, "host file descriptor used for program execution")
	f.BoolVar(&ex.pausedManagement, "paused-management", false, "TTC-internal: run a management helper while existing branch tasks remain paused")
	f.BoolVar(&ex.allowStoppedSandbox, "ttc-allow-stopped-sandbox", false, "TTC-internal: connect to a rehydrated family after its original OCI init exited")
	f.StringVar(&ex.execPath, "exec-path", "", "path of the program to execute; if unset, it is resolved from argv[0]. Mutually exclusive with --exec-fd")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (ex *Exec) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := ex.loadContainer(conf, f, container.LoadOpts{SkipCheck: ex.allowStoppedSandbox})
	if err != nil {
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute. It starts a process in an
// already created container.
func (ex *Exec) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	conf := args[0].(*config.Config)
	waitStatus := args[1].(*unix.WaitStatus)
	ex.stdin, ex.stdout, ex.stderr = os.Stdin, os.Stdout, os.Stderr

	c, e, err := ex.prepare(conf, f)
	if err != nil {
		util.Fatalf("%v", err)
	}

	log.Debugf("Exec arguments: %+v", e)
	log.Debugf("Exec capabilities: %+v", e.Capabilities)

	// Create the file descriptor map for the process in the container.
	fdMap := make(map[int]*os.File)
	if e.InternalConsoleID == "" {
		fdMap[0] = os.Stdin
		fdMap[1] = os.Stdout
		fdMap[2] = os.Stderr
	}

	// Add custom file descriptors to the map.
	for _, mapping := range ex.passFDs {
		file := os.NewFile(uintptr(mapping.Host), "")
		if file == nil {
			util.Fatalf("failed to create file from file descriptor %d", mapping.Host)
		}
		fdMap[mapping.Guest] = file
	}

	var execFile *os.File
	if ex.execFD >= 0 {
		execFile = os.NewFile(uintptr(ex.execFD), "exec-fd")
	}

	// Close the underlying file descriptors after we have passed them.
	defer func() {
		for _, file := range fdMap {
			fd := file.Fd()
			if file.Close() != nil {
				log.Debugf("Failed to close FD %d", fd)
			}
		}

		if execFile != nil && execFile.Close() != nil {
			log.Debugf("Failed to close exec FD")
		}
	}()

	e.FilePayload = controlapi.NewFilePayload(fdMap, execFile)

	// containerd expects an actual process to represent the container being
	// executed. If detach was specified, starts a child in non-detach mode,
	// write the child's PID to the pid file. So when the container returns, the
	// child process will also return and signal containerd.
	if ex.detach {
		if e.ExecID != "" {
			if ex.pidFile != "" {
				util.Fatalf("--pid-file is unsupported for managed detached exec sessions")
			}
			return ex.execManagedDetached(conf, c, e, waitStatus)
		}
		return ex.execChildAndWait(waitStatus)
	}
	return ex.exec(conf, c, e, waitStatus)
}

// ExecInProcess runs, in the calling process, the exec `runsc exec args...`
// would run, with stdin, stdout and stderr in place of runsc's own: on an
// internal console (--internal-console), its input, if it takes any, is read
// from stdin and its output written to stdout and stderr; otherwise they are
// the process's stdio, a nil stdin standing for /dev/null. execFile is the
// executable --exec-fd names, if args name one. A detached exec, which must be
// a managed session (--exec-id) on an internal console, takes no stdio. conf
// is the configuration runsc would run with, with the container's annotations
// applied. It returns the process's wait status, zero for a detached exec, or
// why it could not be run, which runsc exec would print before exiting with
// 128.
func ExecInProcess(conf *config.Config, args []string, stdin, stdout, stderr, execFile *os.File) (unix.WaitStatus, error) {
	ex := &Exec{stdin: stdin, stdout: stdout, stderr: stderr}
	f := flag.NewFlagSet(ex.Name(), flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Usage = func() {}
	ex.SetFlags(f)
	if err := f.Parse(args); err != nil {
		return 0, err
	}
	switch {
	case ex.pidFile != "" || ex.internalPidFile != "":
		return 0, errors.New("an exec run in another process has no pid files")
	case ex.consoleSocket != "" || len(ex.passFDs) != 0:
		return 0, errors.New("an exec run in another process takes no console socket or passed descriptors")
	case (ex.execFD >= 0) != (execFile != nil):
		return 0, errors.New("an exec run in another process is given the file --exec-fd names, and only it")
	case ex.detach && (ex.execID == "" || ex.internalConsole == ""):
		return 0, errors.New("a detached exec run in another process must be a managed session on an internal console")
	case !ex.detach && (stdout == nil || stderr == nil || (ex.internalConsoleStdin && stdin == nil)):
		return 0, errors.New("an exec run in another process needs its input and output")
	}
	c, e, err := ex.prepare(conf, f)
	if err != nil {
		return 0, err
	}
	// A host terminal's exec forwards this process's signals to the process.
	if e.StdioIsPty && e.InternalConsoleID == "" {
		return 0, errors.New("an exec run in another process cannot have a host terminal")
	}
	var fdMap map[int]*os.File
	if e.InternalConsoleID == "" && !ex.detach {
		if stdin == nil {
			devNull, err := os.Open(os.DevNull)
			if err != nil {
				return 0, err
			}
			defer devNull.Close()
			stdin = devNull
		}
		fdMap = map[int]*os.File{0: stdin, 1: stdout, 2: stderr}
	}
	e.FilePayload = controlapi.NewFilePayload(fdMap, execFile)
	if ex.detach {
		return 0, ex.startManagedDetached(conf, c, e)
	}
	return ex.run(conf, c, e)
}

// prepare loads the container and builds, from the command's flags, the
// arguments of the process to execute in it.
func (ex *Exec) prepare(conf *config.Config, f *flag.FlagSet) (*container.Container, *controlapi.ExecArgs, error) {
	c, err := ex.loadContainer(conf, f, container.LoadOpts{SkipCheck: ex.allowStoppedSandbox})
	if err != nil {
		return nil, nil, fmt.Errorf("loading container failed: %v", err)
	}

	if c.Spec.Process == nil {
		return nil, nil, fmt.Errorf("cannot exec in %q: it is a sandbox booted without a root container; exec into its containers by their own IDs instead", c.ID)
	}

	e, err := ex.parseArgs(f, c.Spec.Process, conf.EnableRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing process spec: %v", err)
	}
	e.BranchID = ex.branch
	e.ExecID = ex.execID
	e.RetainExecSession = ex.retainExecSession
	e.PausedManagement = ex.pausedManagement
	e.AllowStoppedContainer = ex.allowStoppedSandbox
	if ex.pausedManagement && ex.execFD < 0 {
		return nil, nil, errors.New("--paused-management requires --exec-fd")
	}
	if ex.pausedManagement && ex.detach {
		return nil, nil, errors.New("--paused-management cannot be detached")
	}
	if ex.internalConsole != "" {
		if ex.consoleSocket != "" {
			return nil, nil, errors.New("--internal-console and --console-socket cannot be combined")
		}
		if len(ex.passFDs) != 0 || ex.execFD >= 0 {
			return nil, nil, errors.New("--internal-console cannot be combined with --pass-fd or --exec-fd")
		}
		e.InternalConsoleID = ex.internalConsole
		e.StdioIsPty = ex.internalConsoleTerminal
		if ex.internalConsoleRows < 0 || ex.internalConsoleRows > 1<<16-1 || ex.internalConsoleCols < 0 || ex.internalConsoleCols > 1<<16-1 {
			return nil, nil, errors.New("--internal-console-rows and --internal-console-cols must fit in uint16")
		}
		if (ex.internalConsoleRows == 0) != (ex.internalConsoleCols == 0) {
			return nil, nil, errors.New("--internal-console-rows and --internal-console-cols must be specified together")
		}
		e.ConsoleRows = uint16(ex.internalConsoleRows)
		e.ConsoleCols = uint16(ex.internalConsoleCols)
	}
	if ex.pausedManagement && ex.execID == "" {
		return nil, nil, errors.New("--paused-management requires --exec-id")
	}
	if ex.branch != boot.DefaultBranchID && e.StdioIsPty && e.InternalConsoleID == "" {
		return nil, nil, errors.New("branch exec with a host PTY is unsupported; use --internal-console")
	}
	return c, e, nil
}

// execManagedDetached creates a managed Sentry-owned session without leaving a
// host shim attached to its console. TTC starts the sole persistent drainer.
func (ex *Exec) execManagedDetached(conf *config.Config, c *container.Container, e *controlapi.ExecArgs, waitStatus *unix.WaitStatus) subcommands.ExitStatus {
	if err := ex.startManagedDetached(conf, c, e); err != nil {
		return util.Errorf("%v", err)
	}
	*waitStatus = 0
	return subcommands.ExitSuccess
}

func (ex *Exec) startManagedDetached(conf *config.Config, c *container.Container, e *controlapi.ExecArgs) error {
	pid, err := c.Execute(conf, e)
	if err != nil {
		return fmt.Errorf("executing managed detached process: %v", err)
	}
	if ex.internalPidFile != "" {
		if err := WritePidFile(ex.internalPidFile, int(pid)); err != nil {
			return fmt.Errorf("writing internal pid file: %v", err)
		}
	}
	return nil
}

func (ex *Exec) exec(conf *config.Config, c *container.Container, e *controlapi.ExecArgs, waitStatus *unix.WaitStatus) subcommands.ExitStatus {
	ws, err := ex.run(conf, c, e)
	if err != nil {
		return util.Errorf("%v", err)
	}
	*waitStatus = ws
	return subcommands.ExitSuccess
}

// run starts the process and waits for it to exit.
func (ex *Exec) run(conf *config.Config, c *container.Container, e *controlapi.ExecArgs) (unix.WaitStatus, error) {
	// Start the new process and get its pid.
	pid, err := c.Execute(conf, e)
	if err != nil {
		return 0, fmt.Errorf("executing processes for container: %v", err)
	}

	var consoleOutput <-chan error
	if e.InternalConsoleID != "" {
		output, cleanup, err := ex.startInternalConsole(c, e)
		if err != nil {
			branchID := e.BranchID
			if branchID == "" {
				branchID = boot.DefaultBranchID
			}
			if removeErr := c.Sandbox.RemoveConsole(&boot.ConsoleRemoveArgs{BranchID: branchID, ConsoleID: e.InternalConsoleID}); removeErr != nil {
				log.Warningf("removing internal exec console after attach failure: %v", removeErr)
			}
			return 0, fmt.Errorf("attaching internal exec console: %v", err)
		}
		consoleOutput = output
		defer cleanup()
	}

	if e.StdioIsPty && e.InternalConsoleID == "" {
		// Forward signals sent to this process to the foreground
		// process in the sandbox.
		stopForwarding := c.ForwardSignals(pid, true /* fgProcess */)
		defer stopForwarding()
	}

	// Write the sandbox-internal pid if required.
	if ex.internalPidFile != "" {
		if err := WritePidFile(ex.internalPidFile, int(pid)); err != nil {
			return 0, fmt.Errorf("writing internal pid file: %v", err)
		}
	}

	// Generate the pid file after the internal pid file is generated, so that
	// users can safely assume that the internal pid file is ready after
	// `runsc exec -d` returns.
	if ex.pidFile != "" {
		if err := WritePidFile(ex.pidFile, os.Getpid()); err != nil {
			return 0, fmt.Errorf("writing pid file: %v", err)
		}
	}

	// Wait for the process to exit.
	var ws unix.WaitStatus
	if e.ExecID != "" {
		status, waitErr := c.Sandbox.WaitExecSession(&boot.ExecSessionArgs{
			BranchID: e.BranchID,
			ExecID:   e.ExecID,
		})
		ws = unix.WaitStatus(status)
		err = waitErr
	} else if e.BranchID == "" {
		ws, err = c.WaitPID(pid)
	} else {
		// A cold-rehydrated family may have any visible branch referenced by
		// Loader.k. Branch names, including "main", do not identify that
		// implementation role, so every explicit branch uses the branch-aware
		// waiter.
		ws, err = c.WaitBranchPID(e.BranchID, pid)
	}
	if err != nil {
		return 0, fmt.Errorf("waiting on pid %d: %v", pid, err)
	}
	if consoleOutput != nil {
		if err := <-consoleOutput; err != nil {
			return 0, fmt.Errorf("reading internal exec console: %v", err)
		}
	}
	return ws, nil
}

func (ex *Exec) startInternalConsole(c *container.Container, e *controlapi.ExecArgs) (<-chan error, func(), error) {
	branchID := e.BranchID
	if branchID == "" {
		branchID = boot.DefaultBranchID
	}
	target := &boot.ConsoleResizeArgs{BranchID: branchID, ConsoleID: e.InternalConsoleID, Rows: e.ConsoleRows, Cols: e.ConsoleCols}
	stdinFD, stdinIsTerminal := -1, false
	if ex.stdin != nil {
		stdinFD = int(ex.stdin.Fd())
		stdinIsTerminal = e.StdioIsPty && term.IsTerminal(stdinFD)
	}
	if stdinIsTerminal {
		cols, rows, err := term.GetSize(stdinFD)
		if err != nil {
			return nil, nil, fmt.Errorf("reading host terminal size: %w", err)
		}
		target.Rows = uint16(rows)
		target.Cols = uint16(cols)
		if err := c.Sandbox.ResizeConsole(target); err != nil {
			return nil, nil, err
		}
	}

	var restore func() error
	if ex.internalConsoleStdin && stdinIsTerminal {
		state, err := term.MakeRaw(stdinFD)
		if err != nil {
			return nil, nil, fmt.Errorf("making host terminal raw: %w", err)
		}
		restore = func() error { return term.Restore(stdinFD, state) }
	}

	resizeSignals := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	if stdinIsTerminal {
		signal.Notify(resizeSignals, unix.SIGWINCH)
		go func() {
			for {
				select {
				case <-resizeSignals:
					cols, rows, err := term.GetSize(stdinFD)
					if err == nil {
						target.Rows = uint16(rows)
						target.Cols = uint16(cols)
						err = c.Sandbox.ResizeConsole(target)
					}
					if err != nil {
						log.Warningf("resizing internal exec console: %v", err)
					}
				case <-resizeDone:
					return
				}
			}
		}()
	}

	if ex.internalConsoleStdin {
		// The Sentry reads stdin itself, blocking.
		err := unix.SetNonblock(stdinFD, false)
		if err == nil {
			err = c.Sandbox.ConsoleInput(&boot.ConsoleInputArgs{
				BranchID: branchID, ConsoleID: e.InternalConsoleID,
				FilePayload: urpc.FilePayload{Files: []*os.File{ex.stdin}},
			})
		}
		if err != nil {
			if restore != nil {
				_ = restore()
			}
			if stdinIsTerminal {
				signal.Stop(resizeSignals)
				close(resizeDone)
			}
			return nil, nil, err
		}
	} else if err := c.Sandbox.CloseConsoleStdin(&boot.ConsoleCloseStdinArgs{BranchID: branchID, ConsoleID: e.InternalConsoleID}); err != nil {
		if restore != nil {
			_ = restore()
		}
		if stdinIsTerminal {
			signal.Stop(resizeSignals)
			close(resizeDone)
		}
		return nil, nil, err
	}

	output := make(chan error, 1)
	go func() { output <- ex.pumpInternalConsoleOutput(c, branchID, e.InternalConsoleID) }()
	cleanup := func() {
		if stdinIsTerminal {
			signal.Stop(resizeSignals)
			close(resizeDone)
		}
		if restore != nil {
			if err := restore(); err != nil {
				log.Warningf("restoring host terminal: %v", err)
			}
		}
		if err := c.Sandbox.RemoveConsole(&boot.ConsoleRemoveArgs{BranchID: branchID, ConsoleID: e.InternalConsoleID}); err != nil {
			log.Warningf("removing internal exec console: %v", err)
		}
	}
	return output, cleanup, nil
}

// pumpInternalConsoleOutput has the Sentry write the console's stdout and
// stderr to the exec's, blocking, until both end.
func (ex *Exec) pumpInternalConsoleOutput(c *container.Container, branchID, consoleID string) error {
	for _, file := range []*os.File{ex.stdout, ex.stderr} {
		if err := unix.SetNonblock(int(file.Fd()), false); err != nil {
			return err
		}
	}
	return c.Sandbox.ConsoleOutput(&boot.ConsoleOutputArgs{
		BranchID: branchID, ConsoleID: consoleID,
		FilePayload: urpc.FilePayload{Files: []*os.File{ex.stdout, ex.stderr}},
	})
}

func (ex *Exec) execChildAndWait(waitStatus *unix.WaitStatus) subcommands.ExitStatus {
	var args []string
	var pidFileArg string

	// The command needs to write a pid file so that execChildAndWait can tell
	// when it has started. If no pid-file was provided, we should use a
	// filename in a temp directory.
	pidFile := ex.pidFile
	if pidFile == "" {
		tmpDir, err := os.MkdirTemp("", "exec-pid-")
		if err != nil {
			util.Fatalf("creating TempDir: %v", err)
		}
		defer os.RemoveAll(tmpDir)
		pidFile = filepath.Join(tmpDir, "pid")
		pidFileArg = "--pid-file=" + pidFile
	}

	for _, a := range os.Args[1:] {
		if !strings.Contains(a, "detach") {
			if a == ex.Name() && pidFileArg != "" {
				args = append(args, a, pidFileArg)
			} else {
				args = append(args, a)
			}
		}
	}

	cmd := exec.Command(specutils.ExePath, args...)
	cmd.Args[0] = "runsc-exec"

	// Exec stdio defaults to current process stdio.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// If the console control socket file is provided, then create a new
	// pty master/replica pair and set the TTY on the sandbox process.
	if ex.consoleSocket != "" {
		// Create a new TTY pair and send the master on the provided socket.
		tty, err := console.NewWithSocket(ex.consoleSocket)
		if err != nil {
			util.Fatalf("setting up console with socket %q: %v", ex.consoleSocket, err)
		}
		defer tty.Close()

		// Set stdio to the new TTY replica.
		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty
		cmd.SysProcAttr = &unix.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			// The Ctty FD must be the FD in the child process's FD
			// table. Since we set cmd.Stdin/Stdout/Stderr to the
			// tty FD, we can use any of 0, 1, or 2 here.
			// See https://github.com/golang/go/issues/29458.
			Ctty: 0,
		}
	}

	if err := cmd.Start(); err != nil {
		util.Fatalf("failure to start child exec process, err: %v", err)
	}

	log.Infof("Started child (PID: %d) to exec and wait: %s %s", cmd.Process.Pid, specutils.ExePath, args)

	// Wait for PID file to ensure that child process has started. Otherwise,
	// '--process' file is deleted as soon as this process returns and the child
	// may fail to read it.
	ready := func() (bool, error) {
		pidb, err := os.ReadFile(pidFile)
		if err == nil {
			// File appeared, check whether pid is fully written.
			pid, err := strconv.Atoi(string(pidb))
			if err != nil {
				return false, nil
			}
			return pid == cmd.Process.Pid, nil
		}
		if pe, ok := err.(*os.PathError); !ok || pe.Err != unix.ENOENT {
			return false, err
		}
		// No file yet, continue to wait...
		return false, nil
	}
	if err := specutils.WaitForReady(cmd.Process.Pid, 10*time.Second, ready); err != nil {
		// Don't log fatal error here, otherwise it will override the error logged
		// by the child process that has failed to start.
		log.Warningf("Unexpected error waiting for PID file, err: %v", err)
		return subcommands.ExitFailure
	}

	*waitStatus = 0
	return subcommands.ExitSuccess
}

// parseArgs parses exec information from the command line or a JSON file
// depending on whether the --process flag was used.
func (ex *Exec) parseArgs(f *flag.FlagSet, p *specs.Process, enableRaw bool) (*controlapi.ExecArgs, error) {
	if ex.execPath != "" && ex.execFD >= 0 {
		return nil, fmt.Errorf("--exec-path and --exec-fd are mutually exclusive")
	}
	if ex.processPath == "" {
		// Requires at least a container ID and command.
		if f.NArg() < 2 {
			f.Usage()
			return nil, fmt.Errorf("both a container-id and command are required")
		}
		return ex.argsFromCLI(p, f.Args()[1:], enableRaw)
	}
	if ex.execPath != "" {
		return nil, fmt.Errorf("--exec-path cannot be used with --process, the process file already contains the program to execute")
	}
	// Requires only the container ID.
	if f.NArg() != 1 {
		f.Usage()
		return nil, fmt.Errorf("only the container-id is required")
	}
	e, err := ex.argsFromProcessFile(p, enableRaw)
	return e, err
}

func (ex *Exec) argsFromCLI(p *specs.Process, argv []string, enableRaw bool) (*controlapi.ExecArgs, error) {
	extraKGIDs := make([]auth.KGID, 0, len(p.User.AdditionalGids)+len(ex.extraKGIDs))
	for _, kgid := range p.User.AdditionalGids {
		extraKGIDs = append(extraKGIDs, auth.KGID(kgid))
	}
	for _, s := range ex.extraKGIDs {
		kgid, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("parsing GID: %s, %v", s, err)
		}
		extraKGIDs = append(extraKGIDs, auth.KGID(kgid))
	}

	cwd := p.Cwd
	if ex.cwd != "" {
		cwd = ex.cwd
	}

	envv := append(p.Env, ex.env...)

	kuid := auth.KUID(p.User.UID)
	if ex.user.kuidSet {
		kuid = ex.user.kuid
	}

	kgid := auth.KGID(p.User.GID)
	if ex.user.kgidSet {
		kgid = ex.user.kgid
	}

	capProcess := p
	if ex.user.kuidSet {
		copied := *p
		copied.Capabilities = capabilitiesForUserTransition(p.Capabilities, p.User.UID, uint32(kuid))
		capProcess = &copied
	}
	caps, err := capabilities(capProcess, ex.caps, enableRaw)
	if err != nil {
		return nil, fmt.Errorf("capabilities error: %v", err)
	}

	return &controlapi.ExecArgs{
		Filename:         ex.execPath,
		Argv:             argv,
		Envv:             envv,
		WorkingDirectory: cwd,
		KUID:             kuid,
		KGID:             kgid,
		ExtraKGIDs:       extraKGIDs,
		Capabilities:     caps,
		StdioIsPty:       ex.consoleSocket != "" || ex.stdioIsPty(),
		NoNewPrivileges:  p.NoNewPrivileges,
		SupportTTYs:      true,
	}, nil
}

// stdioIsPty reports whether the exec's stdio are all terminals.
func (ex *Exec) stdioIsPty() bool {
	for _, file := range []*os.File{ex.stdin, ex.stdout, ex.stderr} {
		if file == nil || !term.IsTerminal(int(file.Fd())) {
			return false
		}
	}
	return true
}

func (ex *Exec) argsFromProcessFile(specProc *specs.Process, enableRaw bool) (*controlapi.ExecArgs, error) {
	f, err := os.Open(ex.processPath)
	if err != nil {
		return nil, fmt.Errorf("error opening process file: %s, %v", ex.processPath, err)
	}
	defer f.Close()
	var p specs.Process
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return nil, fmt.Errorf("error parsing process file: %s, %v", ex.processPath, err)
	}
	if validateProcessSpec(&p) != nil {
		return nil, fmt.Errorf("invalid process spec: %w", err)
	}
	return argsFromProcess(specProc, &p, enableRaw)
}

func validateProcessSpec(p *specs.Process) error {
	if p.Cwd == "" {
		return fmt.Errorf("cwd must not be empty")
	}
	if !filepath.IsAbs(p.Cwd) {
		return fmt.Errorf("cwd %q must be an absolute path", p.Cwd)
	}
	if len(p.Args) == 0 {
		return fmt.Errorf("args must not be empty")
	}
	return nil
}

// argsFromProcess performs all the non-IO conversion from the Process struct
// to ExecArgs.
func argsFromProcess(specProc *specs.Process, p *specs.Process, enableRaw bool) (*controlapi.ExecArgs, error) {
	// Create capabilities.
	procCaps := p.Capabilities
	if procCaps == nil {
		// If p doesn't have capabilities specified, fallback to the capabilities
		// specified in the container spec, adjusted for a root/non-root user
		// transition requested by the exec process.
		procCaps = capabilitiesForUserTransition(specProc.Capabilities, specProc.User.UID, p.User.UID)
	}
	// Starting from Docker 19, capabilities are explicitly set for exec (instead
	// of nil like before). So we can't distinguish 'exec' from
	// 'exec --privileged', as both specify CAP_NET_RAW. Therefore, filter
	// CAP_NET_RAW in the same way as container start.
	caps, err := specutils.Capabilities(enableRaw, procCaps)
	if err != nil {
		return nil, fmt.Errorf("error creating capabilities: %v", err)
	}

	// Convert the spec's additional GIDs to KGIDs.
	extraKGIDs := make([]auth.KGID, 0, len(p.User.AdditionalGids))
	for _, GID := range p.User.AdditionalGids {
		extraKGIDs = append(extraKGIDs, auth.KGID(GID))
	}

	return &controlapi.ExecArgs{
		Argv:             p.Args,
		Envv:             p.Env,
		WorkingDirectory: p.Cwd,
		KUID:             auth.KUID(p.User.UID),
		KGID:             auth.KGID(p.User.GID),
		ExtraKGIDs:       extraKGIDs,
		Capabilities:     caps,
		StdioIsPty:       p.Terminal,
		NoNewPrivileges:  p.NoNewPrivileges,
		SupportTTYs:      true,
	}, nil
}

// capabilitiesForUserTransition derives default exec capability sets when the
// requested user crosses the root/non-root boundary. The bounding set remains
// available to SUID executables. Root gets the bounded capabilities effective
// and permitted; non-root starts without inherited effective, permitted,
// inheritable, or ambient capabilities. Explicit exec capabilities are applied
// by capabilities after this default transition.
func capabilitiesForUserTransition(caps *specs.LinuxCapabilities, oldUID, newUID uint32) *specs.LinuxCapabilities {
	if caps == nil || (oldUID == 0) == (newUID == 0) {
		return caps
	}
	adjusted := *caps
	adjusted.Inheritable = nil
	adjusted.Ambient = nil
	if newUID == 0 {
		adjusted.Effective = append([]string(nil), caps.Bounding...)
		adjusted.Permitted = append([]string(nil), caps.Bounding...)
	} else {
		adjusted.Effective = nil
		adjusted.Permitted = nil
	}
	return &adjusted
}

// capabilities takes a list of capabilities as strings and returns an
// auth.TaskCapabilities struct with those capabilities in every capability set.
// This mimics runc's behavior.
func capabilities(p *specs.Process, cs []string, enableRaw bool) (*auth.TaskCapabilities, error) {
	specCaps := specs.LinuxCapabilities{}
	if p.Capabilities != nil {
		specCaps = *p.Capabilities
	}
	for _, cap := range cs {
		specCaps.Bounding = append(specCaps.Bounding, cap)
		specCaps.Effective = append(specCaps.Effective, cap)
		specCaps.Permitted = append(specCaps.Permitted, cap)
		// Consistent with runc, don't set inheritable. Only set ambient if we
		// already have some inheritable bits set from spec.
		if specCaps.Inheritable != nil {
			specCaps.Ambient = append(specCaps.Ambient, cap)
		}
	}
	// Starting from Docker 19, capabilities are explicitly set for exec (instead
	// of nil like before). So we can't distinguish 'exec' from
	// 'exec --privileged', as both specify CAP_NET_RAW. Therefore, filter
	// CAP_NET_RAW in the same way as container start.
	return specutils.Capabilities(enableRaw, &specCaps)
}

// stringSlice allows a flag to be used multiple times, where each occurrence
// adds a value to the flag. For example, a flag called "x" could be invoked
// via "runsc exec -x foo -x bar", and the corresponding stringSlice would be
// {"x", "y"}.
type stringSlice []string

// String implements flag.Value.String.
func (ss *stringSlice) String() string {
	return fmt.Sprintf("%q", *ss)
}

// Get implements flag.Value.Get.
func (ss *stringSlice) Get() any {
	return ss
}

// Set implements flag.Value.Set.
func (ss *stringSlice) Set(s string) error {
	*ss = append(*ss, s)
	return nil
}

// user allows -user to convey a UID and, optionally, a GID separated by a
// colon.
type user struct {
	kuid    auth.KUID
	kuidSet bool
	kgid    auth.KGID
	kgidSet bool
}

// String implements flag.Value.String.
func (u *user) String() string {
	return fmt.Sprintf("%d:%d", u.kuid, u.kgid)
}

// Get implements flag.Value.Get.
func (u *user) Get() any {
	return u
}

// Set implements flag.Value.Set. Set(String()) should be idempotent.
func (u *user) Set(s string) error {
	parts := strings.SplitN(s, ":", 2)
	kuid, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("couldn't parse UID: %s", parts[0])
	}
	u.kuid = auth.KUID(kuid)
	u.kuidSet = true
	if len(parts) > 1 {
		kgid, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("couldn't parse GID: %s", parts[1])
		}
		u.kgid = auth.KGID(kgid)
		u.kgidSet = true
	}
	return nil
}
