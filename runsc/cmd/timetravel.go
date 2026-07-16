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
	"encoding/json"
	"fmt"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
)

// TimeTravel controls experimental in-memory sandbox branches.
type TimeTravel struct {
	containerLoader
	source string
	paused bool
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
		"timetravel <container-id> pause|resume|destroy <branch-id>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (t *TimeTravel) SetFlags(f *flag.FlagSet) {
	f.StringVar(&t.source, "source", boot.DefaultBranchID, "source branch for fork")
	f.BoolVar(&t.paused, "paused", false, "leave a forked branch paused")
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
