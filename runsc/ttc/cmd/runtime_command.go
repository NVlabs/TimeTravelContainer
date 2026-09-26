// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"

	specs "github.com/opencontainers/runtime-spec/specs-go"
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
// branches, a signal to a branch, and the branch actions above. Starting a
// copy of ttcd to run one costs more than the call. It returns what the
// command would print, and false for any other command, which must be run as
// a command. Errors are flattened to their text, as a command's are.
func embeddedRuntimeCommand(root string, args []string) ([]byte, bool, error) {
	var output any
	switch {
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
