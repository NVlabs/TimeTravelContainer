// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/container"
)

// runscCommand invokes TTC's OCI runtime. Production installations embed that
// entry point in ttcd; tests may provide a dedicated runtime fixture.
func runscCommand(path string, args ...string) *exec.Cmd {
	if embeddedRuntime(path) {
		args = append([]string{"runsc"}, args...)
	}
	return exec.Command(path, args...)
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
		c, err := container.Load(root, container.FullID{ContainerID: rest[0]}, container.LoadOpts{SkipCheck: true})
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
		c, err := container.Load(root, container.FullID{ContainerID: rest[0]}, container.LoadOpts{SkipCheck: true})
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
		c, err := container.Load(root, container.FullID{ContainerID: args[1]}, container.LoadOpts{})
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
			mode := boot.DeliverToProcess
			if all {
				mode = boot.DeliverToAllProcesses
			}
			action = func(c *container.Container, branch string) (any, error) {
				return nil, c.Sandbox.SignalBranch(&boot.BranchSignalArgs{BranchID: branch, CID: c.ID, Signo: int32(signal), Mode: mode})
			}
		default:
			return nil, false, nil
		}
		c, err := container.Load(root, container.FullID{ContainerID: rest[0]}, container.LoadOpts{SkipCheck: true})
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
