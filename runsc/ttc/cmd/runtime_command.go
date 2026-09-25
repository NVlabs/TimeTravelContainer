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

// embeddedRuntimeQuery answers, in ttcd's own process, the read-only runtime
// commands it runs most: the sandboxes in root, and a family's branches, the
// processes of a branch and its exec sessions. It returns what the command
// would print, and false for any other command. Each is a state file read and
// a call to a Sentry, which is far cheaper than starting a copy of ttcd to
// make it. Errors are flattened to their text, as a command's are.
func embeddedRuntimeQuery(root string, args []string) ([]byte, bool, error) {
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
	case len(args) >= 3 && args[0] == "timetravel" && (args[2] == "branches" && len(args) == 3 ||
		(args[2] == "ps" || args[2] == "exec-list") && len(args) == 4):
		cont, err := container.Load(root, container.FullID{ContainerID: args[1]}, container.LoadOpts{SkipCheck: true})
		if err != nil {
			return nil, true, fmt.Errorf("loading container: %v", err)
		}
		switch args[2] {
		case "branches":
			output, err = cont.Sandbox.ListBranches()
		case "ps":
			output, err = cont.Sandbox.BranchProcesses(args[3], cont.ID)
		case "exec-list":
			output, err = cont.Sandbox.ExecSessions(&boot.ExecSessionsArgs{BranchID: args[3], CID: cont.ID})
		}
		if err != nil {
			return nil, true, fmt.Errorf("timetravel %s failed: %v", args[2], err)
		}
	default:
		return nil, false, nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, true, fmt.Errorf("encoding %v: %v", args, err)
	}
	return encoded, true, nil
}
