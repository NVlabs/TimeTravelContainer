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

// Package cmd implements the TimeTravelOS command-line client.
package cmd

import (
	"context"
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
	"time"

	"gvisor.dev/gvisor/runsc/flag"
	ttosimages "gvisor.dev/gvisor/runsc/ttos/images"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	defaultRuntimeRoot      = "/var/run/runsc"
	defaultContainerdSocket = "/run/containerd/containerd.sock"
	defaultImageNamespace   = "ttos"
	defaultBranchID         = "main"
)

type options struct {
	root       string
	runsc      string
	containerd string
	namespace  string
}

// Run executes ttos with args and returns a process exit status.
func Run(args []string, stdout, stderr io.Writer) int {
	opts, args, done, err := parseGlobalOptions(args, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "ttos: %v\n", err)
		return 2
	}
	if done {
		return 0
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	return runCommand(args, stdout, stderr, &runtimeBackend{
		root:       opts.root,
		runsc:      opts.runsc,
		containerd: opts.containerd,
		namespace:  opts.namespace,
	})
}

func parseGlobalOptions(args []string, stdout io.Writer) (options, []string, bool, error) {
	opts := options{
		root:       runtimeRoot(),
		runsc:      runscPath(),
		containerd: containerdAddress(),
		namespace:  defaultImageNamespace,
	}
	for len(args) != 0 {
		arg := args[0]
		switch {
		case arg == "--help" || arg == "-h":
			printUsage(stdout)
			return opts, nil, true, nil
		case arg == "--version":
			fmt.Fprintf(stdout, "ttos version %s\n", version.Version())
			return opts, nil, true, nil
		case arg == "--root":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--root requires a path")
			}
			opts.root = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--root="):
			opts.root = strings.TrimPrefix(arg, "--root=")
			if opts.root == "" {
				return opts, nil, false, errors.New("--root requires a path")
			}
			args = args[1:]
		case arg == "--runsc":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--runsc requires a path")
			}
			opts.runsc = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--runsc="):
			opts.runsc = strings.TrimPrefix(arg, "--runsc=")
			if opts.runsc == "" {
				return opts, nil, false, errors.New("--runsc requires a path")
			}
			args = args[1:]
		case arg == "--containerd":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--containerd requires a socket path")
			}
			opts.containerd = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--containerd="):
			opts.containerd = strings.TrimPrefix(arg, "--containerd=")
			if opts.containerd == "" {
				return opts, nil, false, errors.New("--containerd requires a socket path")
			}
			args = args[1:]
		case arg == "--namespace":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--namespace requires a name")
			}
			opts.namespace = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--namespace="):
			opts.namespace = strings.TrimPrefix(arg, "--namespace=")
			if opts.namespace == "" {
				return opts, nil, false, errors.New("--namespace requires a name")
			}
			args = args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, nil, false, fmt.Errorf("unknown global option %q", arg)
		default:
			return opts, args, false, nil
		}
	}
	return opts, args, false, nil
}

func runtimeRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "runsc")
	}
	return defaultRuntimeRoot
}

func runscPath() string {
	if path := os.Getenv("TTOS_RUNSC"); path != "" {
		return path
	}
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), "runsc")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling
		}
	}
	return "runsc"
}

func containerdAddress() string {
	if address := os.Getenv("CONTAINERD_ADDRESS"); address != "" {
		return address
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "containerd", "containerd.sock")
	}
	return defaultContainerdSocket
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: ttos [GLOBAL OPTIONS] COMMAND

Manage TimeTravelOS container families and live branches.

Global options:
  --root PATH                    runsc state root
  --runsc PATH                   TimeTravel-enabled runsc executable
  --containerd PATH              containerd socket
  --namespace NAME               containerd namespace (default: ttos)

Commands:
  pull [OPTIONS] IMAGE           Pull and unpack an OCI/Docker image
  ls [--tree|--json]              List container families and branch trees
  fork [--paused] SOURCE [DEST]   Fork SOURCE into a new live branch
  pause TARGET...                 Pause one or more branches
  resume TARGET...                Resume one or more branches
  kill [OPTIONS] TARGET...        Send a signal to branch init processes
  stop [OPTIONS] TARGET...        Stop branches with TERM then KILL
  branch rm TARGET...             Destroy one or more non-main branches

Selectors use CONTAINER@BRANCH. Omitting @BRANCH selects main. Forked branches
run immediately unless --paused is set; omitting DEST generates a branch ID.
`)
}

func runCommand(args []string, stdout, stderr io.Writer, backend backend) int {
	var err error
	switch args[0] {
	case "pull":
		err = runPull(args[1:], stdout, stderr, backend)
	case "ls":
		err = runList(args[1:], stdout, stderr, backend)
	case "fork":
		err = runFork(args[1:], stdout, stderr, backend)
	case "pause":
		err = runPauseResume(args[1:], stdout, backend, true)
	case "resume":
		err = runPauseResume(args[1:], stdout, backend, false)
	case "kill":
		err = runKill(args[1:], stdout, stderr, backend)
	case "stop":
		err = runStop(args[1:], stdout, stderr, backend)
	case "branch":
		err = runBranch(args[1:], stdout, backend)
	case "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ttos: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "ttos: %v\n", err)
		return 1
	}
	return 0
}

func runPull(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	unpack := fs.Bool("unpack", true, "unpack the selected platform after pulling")
	snapshotter := fs.String("snapshotter", defaultSnapshotter(), "containerd snapshotter used for unpacking")
	quiet := fs.Bool("quiet", false, "only print the pulled digest")
	fs.BoolVar(quiet, "q", false, "only print the pulled digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos pull [OPTIONS] IMAGE")
	}
	reference, err := ttosimages.NormalizeReference(fs.Arg(0))
	if err != nil {
		return err
	}
	info, err := backend.pull(reference, *platform, *snapshotter, *unpack)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Fprintln(stdout, info.Digest)
		return nil
	}
	fmt.Fprintf(stdout, "Pulled %s\nDigest: %s\nPlatform: %s\n", info.Reference, info.Digest, info.Platform)
	return nil
}

func defaultSnapshotter() string {
	if snapshotter := os.Getenv("CONTAINERD_SNAPSHOTTER"); snapshotter != "" {
		return snapshotter
	}
	return "native"
}

func runList(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tree := fs.Bool("tree", false, "render the branch hierarchy (default)")
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("ls takes no arguments")
	}
	if *tree && *jsonOutput {
		return errors.New("--tree and --json are mutually exclusive")
	}
	families, err := backend.listFamilies()
	if err != nil {
		return fmt.Errorf("listing container families: %w", err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(families)
	}
	writeTree(stdout, families)
	return nil
}

func runFork(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	paused := fs.Bool("paused", false, "leave the child branch paused")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("usage: ttos fork [--paused] SOURCE [DEST]")
	}
	source, err := parseSelector(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid source: %w", err)
	}
	destinationValue := "fork-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if fs.NArg() == 2 {
		destinationValue = fs.Arg(1)
	}
	destination, err := parseDestination(source.container, destinationValue)
	if err != nil {
		return fmt.Errorf("invalid destination: %w", err)
	}
	branch, err := backend.fork(source, destination.branch, *paused)
	if err != nil {
		return fmt.Errorf("forking %s as %s: %w", source, destination, err)
	}
	fmt.Fprintf(stdout, "%s@%s\n", source.container, branch.ID)
	return nil
}

func runPauseResume(args []string, stdout io.Writer, backend backend, pause bool) error {
	if len(args) == 0 {
		if pause {
			return errors.New("usage: ttos pause TARGET [TARGET...]")
		}
		return errors.New("usage: ttos resume TARGET [TARGET...]")
	}
	for _, arg := range args {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		if pause {
			err = backend.pause(target)
		} else {
			err = backend.resume(target)
		}
		if err != nil {
			return fmt.Errorf("updating %s: %w", target, err)
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

func runKill(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var signalName string
	fs.StringVar(&signalName, "signal", "KILL", "signal name or number")
	fs.StringVar(&signalName, "s", "KILL", "signal name or number")
	all := fs.Bool("all", false, "signal every process in the selected branch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos kill [--signal SIGNAL] TARGET [TARGET...]")
	}
	signal, err := parseSignal(signalName)
	if err != nil {
		return err
	}
	for _, arg := range fs.Args() {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		if err := backend.signal(target, signal, *all); err != nil {
			return fmt.Errorf("signaling %s: %w", target, err)
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

func runStop(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var timeout int
	fs.IntVar(&timeout, "time", 10, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "timeout", 10, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "t", 10, "seconds to wait for graceful termination; -1 waits forever")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos stop [--time SECONDS] TARGET [TARGET...]")
	}
	if timeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	for _, arg := range fs.Args() {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		if err := stopBranch(target, timeout, backend); err != nil {
			return err
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

type waitResult struct {
	status uint32
	err    error
}

func stopBranch(target selector, timeoutSeconds int, backend backend) error {
	const (
		signalKill = 9
		signalTerm = 15
	)
	if err := backend.signal(target, signalTerm, false); err != nil {
		return fmt.Errorf("stopping %s with SIGTERM: %w", target, err)
	}
	waited := make(chan waitResult, 1)
	go func() {
		status, err := backend.wait(target)
		waited <- waitResult{status: status, err: err}
	}()
	if timeoutSeconds == -1 {
		result := <-waited
		if result.err != nil {
			return fmt.Errorf("waiting for %s: %w", target, result.err)
		}
		return nil
	}
	timer := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
	defer timer.Stop()
	select {
	case result := <-waited:
		if result.err != nil {
			return fmt.Errorf("waiting for %s: %w", target, result.err)
		}
		return nil
	case <-timer.C:
		if err := backend.signal(target, signalKill, false); err != nil {
			return fmt.Errorf("stopping %s with SIGKILL: %w", target, err)
		}
		result := <-waited
		if result.err != nil {
			return fmt.Errorf("waiting for %s after SIGKILL: %w", target, result.err)
		}
		return nil
	}
}

func parseSignal(value string) (int, error) {
	if number, err := strconv.Atoi(value); err == nil {
		if number < 1 || number > 64 {
			return 0, fmt.Errorf("signal number %d is outside 1..64", number)
		}
		return number, nil
	}
	name := strings.TrimPrefix(strings.ToUpper(value), "SIG")
	signals := map[string]int{
		"HUP": 1, "INT": 2, "QUIT": 3, "KILL": 9, "USR1": 10,
		"USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "CONT": 18,
		"STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22,
	}
	if signal, ok := signals[name]; ok {
		return signal, nil
	}
	return 0, fmt.Errorf("unknown signal %q", value)
}

func runBranch(args []string, stdout io.Writer, backend backend) error {
	if len(args) < 2 || args[0] != "rm" {
		return errors.New("usage: ttos branch rm TARGET [TARGET...]")
	}
	for _, arg := range args[1:] {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		if target.branch == defaultBranchID {
			return fmt.Errorf("cannot remove main branch with 'branch rm'")
		}
		if err := backend.destroy(target); err != nil {
			return fmt.Errorf("removing %s: %w", target, err)
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

type selector struct {
	container string
	branch    string
}

func parseSelector(value string) (selector, error) {
	if value == "" {
		return selector{}, errors.New("selector must not be empty")
	}
	if strings.Count(value, "@") > 1 {
		return selector{}, errors.New("selector contains multiple '@' separators")
	}
	parts := strings.SplitN(value, "@", 2)
	if parts[0] == "" {
		return selector{}, errors.New("container name must not be empty")
	}
	branch := defaultBranchID
	if len(parts) == 2 {
		if parts[1] == "" {
			return selector{}, errors.New("branch name must not be empty")
		}
		branch = parts[1]
	}
	return selector{container: parts[0], branch: branch}, nil
}

func parseDestination(containerID, value string) (selector, error) {
	if !strings.Contains(value, "@") {
		if value == "" {
			return selector{}, errors.New("branch name must not be empty")
		}
		return selector{container: containerID, branch: value}, nil
	}
	destination, err := parseSelector(value)
	if err != nil {
		return selector{}, err
	}
	if destination.container != containerID {
		return selector{}, errors.New("live branches must remain in the source container family")
	}
	return destination, nil
}

func (s selector) String() string {
	return s.container + "@" + s.branch
}

// Branch is stable branch metadata returned by ttos ls --json.
type Branch struct {
	ID               string `json:"id"`
	ParentID         string `json:"parent_id,omitempty"`
	State            string `json:"state"`
	ForkTotalNS      int64  `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS int64  `json:"source_downtime_ns,omitempty"`
	KernelForkNS     int64  `json:"kernel_fork_ns,omitempty"`
}

// Family is stable family metadata returned by ttos ls --json.
type Family struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	Branches []Branch `json:"branches"`
	Error    string   `json:"error,omitempty"`
}

type backend interface {
	pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error)
	listFamilies() ([]Family, error)
	fork(source selector, destination string, paused bool) (Branch, error)
	pause(target selector) error
	resume(target selector) error
	destroy(target selector) error
	signal(target selector, signal int, all bool) error
	wait(target selector) (uint32, error)
}

type runtimeBackend struct {
	root       string
	runsc      string
	containerd string
	namespace  string
}

func (b *runtimeBackend) pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error) {
	return ttosimages.Pull(context.Background(), ttosimages.PullOptions{
		Address:     b.containerd,
		Namespace:   b.namespace,
		Reference:   reference,
		Platform:    platform,
		Snapshotter: snapshotter,
		Unpack:      unpack,
	})
}

func (b *runtimeBackend) command(args ...string) ([]byte, error) {
	global := []string{"--root", b.root}
	command := exec.Command(b.runsc, append(global, args...)...)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return nil, fmt.Errorf("%s: %w", message, err)
		}
		return nil, err
	}
	return output, nil
}

func (b *runtimeBackend) listFamilies() ([]Family, error) {
	output, err := b.command("list", "--format=json", "--sandbox")
	if err != nil {
		return nil, err
	}
	var states []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &states); err != nil {
		return nil, fmt.Errorf("decoding runsc container list: %w", err)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	families := make([]Family, 0, len(states))
	for _, state := range states {
		family := Family{ID: state.ID, State: state.Status, Branches: []Branch{}}
		if state.Status == "stopped" {
			family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: "exited"})
			families = append(families, family)
			continue
		}
		output, err := b.command("timetravel", state.ID, "branches")
		if err != nil {
			family.Error = err.Error()
			family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: state.Status})
			families = append(families, family)
			continue
		}
		var branches []runscBranchInfo
		if err := json.Unmarshal(output, &branches); err != nil {
			family.Error = fmt.Sprintf("decoding runsc branch list: %v", err)
			family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: state.Status})
			families = append(families, family)
			continue
		}
		for _, branch := range branches {
			family.Branches = append(family.Branches, branchView(branch))
		}
		sort.Slice(family.Branches, func(i, j int) bool { return family.Branches[i].ID < family.Branches[j].ID })
		families = append(families, family)
	}
	return families, nil
}

type runscBranchInfo struct {
	ID          string `json:"ID"`
	ParentID    string `json:"ParentID"`
	State       string `json:"State"`
	ForkMetrics struct {
		SourceDowntime time.Duration `json:"SourceDowntime"`
		Kernel         struct {
			Total time.Duration `json:"Total"`
		} `json:"Kernel"`
		Total time.Duration `json:"Total"`
	} `json:"ForkMetrics"`
}

func branchView(info runscBranchInfo) Branch {
	return Branch{
		ID:               info.ID,
		ParentID:         info.ParentID,
		State:            info.State,
		ForkTotalNS:      info.ForkMetrics.Total.Nanoseconds(),
		SourceDowntimeNS: info.ForkMetrics.SourceDowntime.Nanoseconds(),
		KernelForkNS:     info.ForkMetrics.Kernel.Total.Nanoseconds(),
	}
}

func (b *runtimeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	args := []string{"timetravel", "--source", source.branch}
	if paused {
		args = append(args, "--paused")
	}
	args = append(args, source.container, "fork", destination)
	output, err := b.command(args...)
	if err != nil {
		return Branch{}, err
	}
	var info runscBranchInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return Branch{}, fmt.Errorf("decoding runsc fork result: %w", err)
	}
	return branchView(info), nil
}

func (b *runtimeBackend) pause(target selector) error {
	_, err := b.command("timetravel", target.container, "pause", target.branch)
	return err
}

func (b *runtimeBackend) resume(target selector) error {
	_, err := b.command("timetravel", target.container, "resume", target.branch)
	return err
}

func (b *runtimeBackend) destroy(target selector) error {
	_, err := b.command("timetravel", target.container, "destroy", target.branch)
	return err
}

func (b *runtimeBackend) signal(target selector, signal int, all bool) error {
	args := []string{"timetravel", "--signal", strconv.Itoa(signal)}
	if all {
		args = append(args, "--all")
	}
	args = append(args, target.container, "signal", target.branch)
	_, err := b.command(args...)
	return err
}

func (b *runtimeBackend) wait(target selector) (uint32, error) {
	output, err := b.command("timetravel", target.container, "wait", target.branch)
	if err != nil {
		return 0, err
	}
	var result struct {
		Status uint32 `json:"wait_status"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, fmt.Errorf("decoding runsc wait result: %w", err)
	}
	return result.Status, nil
}

func writeTree(w io.Writer, families []Family) {
	for _, family := range families {
		fmt.Fprintf(w, "%s (%s)\n", family.ID, family.State)
		if family.Error != "" {
			fmt.Fprintf(w, "    ! %s\n", family.Error)
		}
		writeFamilyBranches(w, family.Branches)
	}
}

func writeFamilyBranches(w io.Writer, branches []Branch) {
	byID := make(map[string]Branch, len(branches))
	children := make(map[string][]Branch)
	for _, branch := range branches {
		byID[branch.ID] = branch
		children[branch.ParentID] = append(children[branch.ParentID], branch)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].ID < children[parent][j].ID })
	}
	var roots []Branch
	for _, branch := range branches {
		if branch.ParentID == "" {
			roots = append(roots, branch)
			continue
		}
		if _, ok := byID[branch.ParentID]; !ok {
			roots = append(roots, branch)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].ID == defaultBranchID {
			return true
		}
		if roots[j].ID == defaultBranchID {
			return false
		}
		return roots[i].ID < roots[j].ID
	})
	visited := make(map[string]bool, len(branches))
	for i, root := range roots {
		writeBranchNode(w, root, "", i == len(roots)-1, byID, children, visited)
	}
	// Corrupt or cyclic lineage should remain visible rather than disappearing.
	var remaining []Branch
	for _, branch := range branches {
		if !visited[branch.ID] {
			remaining = append(remaining, branch)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].ID < remaining[j].ID })
	for i, branch := range remaining {
		writeBranchNode(w, branch, "", i == len(remaining)-1, byID, children, visited)
	}
}

func writeBranchNode(w io.Writer, branch Branch, prefix string, last bool, byID map[string]Branch, children map[string][]Branch, visited map[string]bool) {
	if visited[branch.ID] {
		return
	}
	visited[branch.ID] = true
	connector := "├── "
	childPrefix := prefix + "│   "
	if last {
		connector = "└── "
		childPrefix = prefix + "    "
	}
	suffix := ""
	if branch.ParentID != "" {
		if _, ok := byID[branch.ParentID]; !ok {
			suffix = fmt.Sprintf(" parent=%s (deleted)", branch.ParentID)
		}
	}
	if branch.ForkTotalNS != 0 {
		suffix += fmt.Sprintf(" fork=%s downtime=%s", time.Duration(branch.ForkTotalNS), time.Duration(branch.SourceDowntimeNS))
	}
	fmt.Fprintf(w, "%s%s%s %s%s\n", prefix, connector, branch.ID, branch.State, suffix)
	branchChildren := children[branch.ID]
	for i, child := range branchChildren {
		writeBranchNode(w, child, childPrefix, i == len(branchChildren)-1, byID, children, visited)
	}
}
