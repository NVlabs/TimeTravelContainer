// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	runsccmd "gvisor.dev/gvisor/runsc/cmd"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
)

// runscCommand invokes TTC's OCI runtime. Production installations embed that
// entry point in ttcd; tests may provide a dedicated runtime fixture.
func runscCommand(path string, args ...string) *exec.Cmd {
	if embeddedRuntime(path) {
		args = append([]string{"runsc"}, args...)
	}
	return exec.Command(path, args...)
}

// loadEmbeddedContainer loads the container with id, the ID of its sandbox's
// own container, as TTC names its families', by that exact ID, and searches
// the runtime root for an abbreviated ID only if there is none: the search
// read every name in the root, and every command on a family paid for it.
func loadEmbeddedContainer(root, id string, opts container.LoadOpts) (*container.Container, error) {
	exact := opts
	exact.Exact = true
	c, err := container.Load(root, container.FullID{SandboxID: id, ContainerID: id}, exact)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	return container.Load(root, container.FullID{ContainerID: id}, opts)
}

// runtimeListConcurrency bounds the sandboxes a listing asks at once.
const runtimeListConcurrency = 8

// embeddedRuntimeFamilies is listRuntimeFamiliesOf with the runtime embedded:
// it reads each sandbox's state once, by its exact ID, and asks the sandboxes
// for their branches at once. Through runsc's commands a listing read each
// state twice, the second time searching the runtime root for it, and asked
// one sandbox after another: `ps -a` took 37 ms with 40 families.
func embeddedRuntimeFamilies(root string, ids []string) ([]Family, error) {
	var sandboxes []container.FullID
	if len(ids) == 0 {
		var err error
		if sandboxes, err = container.ListSandboxes(root); err != nil {
			return nil, err
		}
	} else {
		for _, id := range ids {
			sandboxes = append(sandboxes, container.FullID{SandboxID: id, ContainerID: id})
		}
	}
	families := make([]Family, len(sandboxes))
	forEachConcurrently(len(sandboxes), runtimeListConcurrency, func(i int) {
		// Like runsc list, pass over a sandbox that cannot be loaded, as one
		// being deleted.
		c, err := container.Load(root, sandboxes[i], container.LoadOpts{Exact: true})
		if err != nil {
			return
		}
		state := c.State()
		if state.ID == "" {
			return
		}
		var output []byte
		branches, err := c.Sandbox.ListBranches()
		if err != nil {
			err = fmt.Errorf("timetravel branches failed: %v", err)
		} else {
			output, err = json.Marshal(branches)
		}
		families[i] = runtimeFamily(runtimeSandboxState{ID: state.ID, PID: state.Pid, Status: string(state.Status)}, output, err)
	})
	listed := families[:0]
	for _, family := range families {
		if family.ID != "" {
			listed = append(listed, family)
		}
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].ID < listed[j].ID })
	return listed, nil
}

// forEachConcurrently calls do with each index below n, on at most limit
// goroutines at once, and returns once every call has.
func forEachConcurrently(n, limit int, do func(int)) {
	if limit = min(limit, n); limit <= 1 {
		for i := range n {
			do(i)
		}
		return
	}
	indexes := make(chan int)
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexes {
				do(i)
			}
		}()
	}
	for i := range n {
		indexes <- i
	}
	close(indexes)
	wg.Wait()
}

// embeddedRuntime reports whether path is the OCI runtime embedded in ttcd,
// whose code ttcd shares.
func embeddedRuntime(path string) bool {
	return filepath.Base(path) == "ttcd"
}

// embeddedBranchActions are the runtime's timetravel actions on one branch
// that take no flags or files: each is one call to the family's Sentry, and
// returns what the action prints, if anything.
var embeddedBranchActions = map[string]func(c *container.Container, branch string) (any, error){
	"ps": func(c *container.Container, branch string) (any, error) {
		return c.Sandbox.BranchProcesses(branch, c.ID)
	},
	// A stop waited for its branch's init in a process of ttcd's binary of
	// its own, which took 7 ms to start.
	"wait": func(c *container.Container, branch string) (any, error) {
		status, err := c.Sandbox.WaitBranch(&boot.BranchWaitArgs{BranchID: branch, CID: c.ID})
		if err != nil {
			return nil, err
		}
		return map[string]uint32{"wait_status": status}, nil
	},
	"exec-list": func(c *container.Container, branch string) (any, error) {
		return c.Sandbox.ExecSessions(&boot.ExecSessionsArgs{BranchID: branch, CID: c.ID})
	},
	"dehydrate": func(c *container.Container, branch string) (any, error) {
		return c.Sandbox.DehydrateBranch(branch, nil)
	},
	"pause": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.PauseBranch(branch)
	},
	"resume": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.ResumeBranch(branch)
	},
	"quarantine": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.SetBranchQuarantine(branch, true)
	},
	"unquarantine": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.SetBranchQuarantine(branch, false)
	},
	"rehydrate": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.RehydrateBranch(branch)
	},
	"destroy": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.DestroyBranch(branch)
	},
	"destroy-reclaim": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.DestroyBranchAndReclaim(branch)
	},
	"prepare-stop": func(c *container.Container, branch string) (any, error) {
		return nil, c.Sandbox.PrepareStopBranch(branch)
	},
	"reclaim-image-chunks": func(c *container.Container, branch string) (any, error) {
		return c.Sandbox.ReclaimImageChunks(branch)
	},
}

// embeddedRuntimeCommand runs, in ttcd's own process, the runtime commands
// ttcd runs most, each a read of runsc's state and at most one call to a
// Sentry: listing the sandboxes in root, a sandbox's state, a family's
// branches, a signal to a branch, the branch actions above, a fork, and the
// calls on a branch's internal console that its attached clients make.
// Starting a copy of ttcd to run one costs more than the call. It returns
// what the command would print, and false for any other command, which must
// be run as a command. Errors are flattened to their text, as a command's are.
func embeddedRuntimeCommand(root string, args []string) ([]byte, bool, error) {
	var output any
	switch {
	case len(args) >= 6 && args[0] == "timetravel" && args[1] == "--console":
		// timetravel --console ID [--data-base64 DATA] [--wait-stdin]
		// [--wait-timeout=DURATION] CONTAINER ACTION BRANCH
		consoleID, rest := args[2], args[3:]
		var data []byte
		waitStdin, waitTimeout := false, time.Second
		for len(rest) != 0 && strings.HasPrefix(rest[0], "--") {
			switch {
			case rest[0] == "--data-base64" && len(rest) > 1:
				decoded, err := base64.StdEncoding.DecodeString(rest[1])
				if err != nil {
					return nil, true, fmt.Errorf("decoding --data-base64: %v", err)
				}
				data, rest = decoded, rest[2:]
			case rest[0] == "--wait-stdin":
				waitStdin, rest = true, rest[1:]
			case strings.HasPrefix(rest[0], "--wait-timeout="):
				timeout, err := time.ParseDuration(strings.TrimPrefix(rest[0], "--wait-timeout="))
				if err != nil {
					return nil, false, nil
				}
				waitTimeout, rest = timeout, rest[1:]
			default:
				return nil, false, nil
			}
		}
		if len(rest) != 3 {
			return nil, false, nil
		}
		action, branch := rest[1], rest[2]
		var call func(*container.Container) (any, error)
		switch action {
		case "console-write":
			call = func(c *container.Container) (any, error) {
				return c.Sandbox.WriteConsole(&boot.ConsoleWriteArgs{BranchID: branch, ConsoleID: consoleID, Data: data})
			}
		case "console-wait":
			call = func(c *container.Container) (any, error) {
				return c.Sandbox.WaitConsole(&boot.ConsoleWaitArgs{
					BranchID: branch, ConsoleID: consoleID,
					Want: kernel.InternalConsoleReadiness{Stdin: waitStdin}, Timeout: waitTimeout,
				})
			}
		case "console-close-stdin":
			call = func(c *container.Container) (any, error) {
				return nil, c.Sandbox.CloseConsoleStdin(&boot.ConsoleCloseStdinArgs{BranchID: branch, ConsoleID: consoleID})
			}
		case "console-stat":
			call = func(c *container.Container) (any, error) {
				return c.Sandbox.ObserveConsole(&boot.ConsoleObservationArgs{BranchID: branch, ConsoleID: consoleID})
			}
		default:
			return nil, false, nil
		}
		c, err := loadEmbeddedContainer(root, rest[0], container.LoadOpts{SkipCheck: true})
		if err != nil {
			return nil, true, fmt.Errorf("loading container: %v", err)
		}
		if output, err = call(c); err != nil {
			return nil, true, fmt.Errorf("timetravel %s failed: %v", action, err)
		}
	case len(args) >= 5 && args[0] == "timetravel" && args[1] == "--source":
		// timetravel --source BRANCH [--paused] [--quarantine] ID fork
		// DESTINATION...
		source, rest := args[2], args[3:]
		var paused, quarantine bool
		for len(rest) != 0 && (rest[0] == "--paused" || rest[0] == "--quarantine") {
			paused = paused || rest[0] == "--paused"
			quarantine = quarantine || rest[0] == "--quarantine"
			rest = rest[1:]
		}
		if len(rest) < 3 || rest[1] != "fork" {
			return nil, false, nil
		}
		c, err := loadEmbeddedContainer(root, rest[0], container.LoadOpts{SkipCheck: true})
		if err != nil {
			return nil, true, fmt.Errorf("loading container: %v", err)
		}
		destinations := rest[2:]
		if len(destinations) == 1 {
			output, err = c.Sandbox.ForkBranch(&boot.ForkBranchArgs{Source: source, ID: destinations[0], Paused: paused, Quarantine: quarantine})
		} else {
			output, err = c.Sandbox.ForkBranches(&boot.ForkBranchesArgs{Source: source, IDs: destinations, Paused: paused, Quarantine: quarantine})
		}
		if err != nil {
			return nil, true, fmt.Errorf("timetravel fork failed: %v", err)
		}
	case len(args) == 3 && args[0] == "list" && args[1] == "--format=json" && args[2] == "--sandbox":
		ids, err := container.ListSandboxes(root)
		if err != nil {
			return nil, true, fmt.Errorf("%v", err)
		}
		var states []specs.State
		for _, id := range ids {
			// Like runsc list, skip a sandbox that is being deleted.
			if c, err := container.Load(root, id, container.LoadOpts{Exact: true}); err == nil {
				states = append(states, c.State())
			}
		}
		output = states
	case len(args) == 2 && args[0] == "state":
		c, err := loadEmbeddedContainer(root, args[1], container.LoadOpts{})
		if err != nil {
			return nil, true, fmt.Errorf("loading container: %v", err)
		}
		output = c.State()
	case len(args) >= 3 && args[0] == "timetravel":
		// timetravel ID ACTION [BRANCH], or timetravel --signal N [--all] ID
		// signal BRANCH.
		rest := args[1:]
		signal, all := -1, false
		if rest[0] == "--signal" && len(rest) >= 2 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				return nil, false, nil
			}
			signal, rest = n, rest[2:]
			if len(rest) != 0 && rest[0] == "--all" {
				all, rest = true, rest[1:]
			}
		}
		var action func(*container.Container, string) (any, error)
		switch {
		case signal < 0 && len(rest) == 2 && rest[1] == "branches":
			action = func(c *container.Container, _ string) (any, error) { return c.Sandbox.ListBranches() }
		case signal < 0 && len(rest) == 3 && embeddedBranchActions[rest[1]] != nil:
			action = embeddedBranchActions[rest[1]]
		case signal >= 0 && len(rest) == 3 && rest[1] == "signal":
			mode := sentryapi.DeliverToProcess
			if all {
				mode = sentryapi.DeliverToAllProcesses
			}
			action = func(c *container.Container, branch string) (any, error) {
				return nil, c.Sandbox.SignalBranch(&boot.BranchSignalArgs{BranchID: branch, CID: c.ID, Signo: int32(signal), Mode: mode})
			}
		default:
			return nil, false, nil
		}
		c, err := loadEmbeddedContainer(root, rest[0], container.LoadOpts{SkipCheck: true})
		if err != nil {
			return nil, true, fmt.Errorf("loading container: %v", err)
		}
		branch := ""
		if len(rest) == 3 {
			branch = rest[2]
		}
		if output, err = action(c, branch); err != nil {
			return nil, true, fmt.Errorf("timetravel %s failed: %v", rest[1], err)
		}
	default:
		return nil, false, nil
	}
	if output == nil {
		return nil, true, nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, true, fmt.Errorf("encoding %v: %v", args, err)
	}
	return encoded, true, nil
}

// embeddedFork forks the branch source of the family whose sandbox's own
// container is id as destinations, in ttcd's process, with the filesystem
// snapshot origins the authority assigned the new branches, and returns what
// runsc's fork prints. A copy of ttcd, which read them from a pipe, took
// longer to start than the fork of a small branch takes its Sentry.
func embeddedFork(root, id, source string, destinations []string, paused, quarantine bool, origins []boot.FilesystemOriginAssignment) ([]byte, error) {
	c, err := loadEmbeddedContainer(root, id, container.LoadOpts{SkipCheck: true})
	if err != nil {
		return nil, fmt.Errorf("loading container: %v", err)
	}
	var output any
	if len(destinations) == 1 {
		output, err = c.Sandbox.ForkBranch(&boot.ForkBranchArgs{Source: source, ID: destinations[0], Paused: paused, Quarantine: quarantine, FilesystemOrigins: origins})
	} else {
		output, err = c.Sandbox.ForkBranches(&boot.ForkBranchesArgs{Source: source, IDs: destinations, Paused: paused, Quarantine: quarantine, FilesystemOrigins: origins})
	}
	if err != nil {
		return nil, fmt.Errorf("timetravel fork failed: %v", err)
	}
	return json.Marshal(output)
}

// embeddedExec runs, in ttcd's process, the runsc exec that runtimeArgs, the
// runtime's flags, and execArgs, the exec's, describe: one whose input, if
// stdin is not nil, is read from stdin, and whose output is written to stdout
// and stderr, or discarded if they are nil, whether through its internal
// console or as its own stdio; a detached exec takes none. execFile is the
// executable execArgs' --exec-fd names, if any. Starting a copy of ttcd to
// run it costs more than the rest of the exec. It returns the status runsc
// exec would exit with, having written to stderr what it would.
func embeddedExec(runtimeArgs, execArgs []string, detach bool, stdin io.Reader, stdout, stderr io.Writer, execFile *os.File) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	status, err := runEmbeddedExec(runtimeArgs, execArgs, detach, stdin, stdout, stderr, execFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		// What runsc exits with when a command fails.
		return 128
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

func runEmbeddedExec(runtimeArgs, execArgs []string, detach bool, stdin io.Reader, stdout, stderr io.Writer, execFile *os.File) (unix.WaitStatus, error) {
	// The flags are the whole configuration: no spec TTC makes carries
	// annotations that change it.
	flags := flag.NewFlagSet("runsc", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config.RegisterFlags(flags)
	if err := flags.Parse(runtimeArgs); err != nil {
		return 0, err
	}
	conf, err := config.NewFromFlags(flags)
	if err != nil {
		return 0, err
	}
	if detach {
		return runsccmd.ExecInProcess(conf, execArgs, nil, nil, nil, execFile)
	}
	// The input and output go through pipes, as they did through runsc
	// exec's own, which the Sentry copies from and to the console or gives
	// the process; ttcd copies them from and to the client.
	var input *os.File
	if stdin != nil {
		reader, writer, err := os.Pipe()
		if err != nil {
			return 0, err
		}
		// Closing the writer once the exec ends also ends the copy.
		defer reader.Close()
		defer writer.Close()
		go func() {
			_, _ = io.Copy(writer, stdin)
			_ = writer.Close()
		}()
		input = reader
	}
	var outputs []*os.File
	copied := make(chan struct{}, 2)
	defer func() {
		for _, output := range outputs {
			_ = output.Close()
		}
		for range outputs {
			<-copied
		}
	}()
	for _, w := range []io.Writer{stdout, stderr} {
		reader, writer, err := os.Pipe()
		if err != nil {
			return 0, err
		}
		outputs = append(outputs, writer)
		go func() {
			// A read takes what the pipe holds, 64 KiB; the wrapper keeps
			// io.CopyBuffer from reading 32 KiB at a time through
			// File.WriteTo.
			_, _ = io.CopyBuffer(w, struct{ io.Reader }{reader}, make([]byte, 64<<10))
			_ = reader.Close()
			copied <- struct{}{}
		}()
	}
	return runsccmd.ExecInProcess(conf, execArgs, input, outputs[0], outputs[1], execFile)
}
