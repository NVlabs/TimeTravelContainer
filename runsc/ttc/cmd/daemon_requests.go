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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func (s *daemonServer) handleMutationControl(dataRoot string, args []string, stdout, stderr io.Writer) int {
	if dataRoot == "" {
		fmt.Fprintln(stderr, "ttcd: mutation inspection requires a data root")
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "ttcd: usage: ttc daemon requests|request inspect|request resolve")
		return 2
	}
	unlock := s.lockDataRoot(dataRoot)
	defer unlock()
	switch args[0] {
	case "requests":
		return runDaemonMutationList(dataRoot, args[1:], stdout, stderr)
	case "request":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "ttcd: usage: ttc daemon request inspect|resolve")
			return 2
		}
		switch args[1] {
		case "inspect":
			return runDaemonMutationInspect(dataRoot, args[2:], stdout, stderr)
		case "resolve":
			return runDaemonMutationResolve(dataRoot, args[2:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "ttcd: unknown daemon request command %q\n", args[1])
			return 2
		}
	default:
		fmt.Fprintf(stderr, "ttcd: unknown mutation-control command %q\n", args[0])
		return 2
	}
}

func runDaemonMutationList(dataRoot string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon requests", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	state := flags.String("state", "", "filter by pending or completed state")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ttcd: usage: ttc daemon requests [--json] [--state pending|completed]")
		return 2
	}
	records, err := listDaemonMutations(dataRoot, *state)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeDaemonMutationJSON(stdout, records)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, 32, 0)
	fmt.Fprintln(table, "REQUEST ID\tSTATE\tOUTCOME\tSTATUS\tCREATED\tCOMMAND")
	for _, record := range records {
		status := "-"
		if record.State == "completed" {
			status = strconv.Itoa(record.Status)
		}
		outcome := record.Resolution
		if outcome == "" {
			outcome = "-"
		}
		command := strings.Join(record.Command, " ")
		if command == "" {
			command = "<legacy record>"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", record.RequestID, record.State, outcome, status, record.CreatedAt.Format(time.RFC3339), command)
	}
	if err := table.Flush(); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	return 0
}

func runDaemonMutationInspect(dataRoot string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon request inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "ttcd: usage: ttc daemon request inspect [--json] REQUEST_ID")
		return 2
	}
	record, err := inspectDaemonMutation(dataRoot, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeDaemonMutationJSON(stdout, record)
	}
	command := strings.Join(record.Command, " ")
	if command == "" {
		command = "<legacy record>"
	}
	fmt.Fprintf(stdout, "Request ID: %s\nState: %s\nCommand: %s\nCreated: %s\n", record.RequestID, record.State, command, record.CreatedAt.Format(time.RFC3339Nano))
	if record.State == "completed" {
		fmt.Fprintf(stdout, "Status: %d\nCompleted: %s\n", record.Status, record.CompletedAt.Format(time.RFC3339Nano))
	}
	if record.Resolution != "" {
		fmt.Fprintf(stdout, "Resolution: %s\n", record.Resolution)
	}
	if record.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", record.Note)
	}
	return 0
}

func runDaemonMutationResolve(dataRoot string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon request resolve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	outcome := flags.String("outcome", "", "verified outcome: succeeded, failed, or abandoned")
	note := flags.String("note", "", "operator audit note")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *outcome == "" {
		fmt.Fprintln(stderr, "ttcd: usage: ttc daemon request resolve --outcome succeeded|failed|abandoned [--note TEXT] REQUEST_ID")
		return 2
	}
	record, err := resolveDaemonMutation(dataRoot, flags.Arg(0), *outcome, *note)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s resolved as %s (status %d); mutation was not re-executed\n", record.RequestID, record.Resolution, record.Status)
	return 0
}

func writeDaemonMutationJSON(stdout io.Writer, value any) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return 1
	}
	data = append(data, byte(10))
	if _, err := stdout.Write(data); err != nil {
		return 1
	}
	return 0
}
