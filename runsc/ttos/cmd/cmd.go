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
	"syscall"
	"time"

	"gvisor.dev/gvisor/runsc/flag"
	ttosbundle "gvisor.dev/gvisor/runsc/ttos/bundle"
	ttosconsole "gvisor.dev/gvisor/runsc/ttos/console"
	ttosimages "gvisor.dev/gvisor/runsc/ttos/images"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	defaultRuntimeRoot      = "/var/run/runsc"
	defaultContainerdSocket = "/run/containerd/containerd.sock"
	defaultImageNamespace   = "ttos"
	defaultBranchID         = "main"
	defaultConsoleLogMax    = int64(16 << 20)
)

type options struct {
	root       string
	runsc      string
	containerd string
	namespace  string
	dataRoot   string
}

// Run executes ttos with args and returns a process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		dataRoot:   opts.dataRoot,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
	})
}

func parseGlobalOptions(args []string, stdout io.Writer) (options, []string, bool, error) {
	opts := options{
		root:       runtimeRoot(),
		runsc:      runscPath(),
		containerd: containerdAddress(),
		namespace:  defaultImageNamespace,
		dataRoot:   dataRoot(),
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
		case arg == "--data-root":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--data-root requires a path")
			}
			opts.dataRoot = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--data-root="):
			opts.dataRoot = strings.TrimPrefix(arg, "--data-root=")
			if opts.dataRoot == "" {
				return opts, nil, false, errors.New("--data-root requires a path")
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

func dataRoot() string {
	if root := os.Getenv("TTOS_DATA_ROOT"); root != "" {
		return root
	}
	if root := os.Getenv("XDG_DATA_HOME"); root != "" {
		return filepath.Join(root, "ttos")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "ttos")
	}
	return filepath.Join(os.TempDir(), "ttos")
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: ttos [GLOBAL OPTIONS] COMMAND

Manage TimeTravelOS container families and live branches.

Global options:
  --root PATH                    runsc state root
  --runsc PATH                   TimeTravel-enabled runsc executable
  --containerd PATH              containerd socket
  --namespace NAME               containerd namespace (default: ttos)
  --data-root PATH               image, bundle, and metadata directory

Commands:
  pull [OPTIONS] IMAGE           Pull an OCI/Docker image into local content
  run [OPTIONS] IMAGE [COMMAND]  Pull, create, and run a container
  create [OPTIONS] IMAGE [CMD]   Prepare a container without starting it
  start [OPTIONS] CONTAINER      Start a prepared or exited container
  attach [OPTIONS] TARGET        Attach to a branch's init console
  logs [OPTIONS] TARGET          Print or follow a branch's init-console logs
  exec [OPTIONS] TARGET COMMAND  Execute a command in a running branch
  inspect CONTAINER...           Show persistent and live branch state
  rm [--force] CONTAINER...      Remove container metadata and runtime state
  ls [--tree|--json]              List container families and branch trees
  fork [--paused] SOURCE [DEST]   Fork SOURCE into a new live branch
  pause TARGET...                 Pause one or more branches
  resume TARGET...                Resume one or more branches
  kill [OPTIONS] TARGET...        Send a signal to branch init processes
  stop [OPTIONS] TARGET...        Stop branches with TERM then KILL
  branch rm TARGET...             Destroy one or more non-main branches
  volume COMMAND                   Manage family-local tmpfs volumes

Selectors use CONTAINER@BRANCH. Omitting @BRANCH selects main. Forked branches
run immediately unless --paused is set; omitting DEST generates a branch ID.
`)
}

func runCommand(args []string, stdout, stderr io.Writer, backend backend) int {
	var err error
	switch args[0] {
	case "pull":
		err = runPull(args[1:], stdout, stderr, backend)
	case "run":
		err = runContainer(args[1:], stdout, stderr, backend)
	case "create":
		err = runCreate(args[1:], stdout, stderr, backend)
	case "start":
		err = runStart(args[1:], stdout, stderr, backend)
	case "attach":
		err = runAttach(args[1:], stderr, backend)
	case "logs":
		err = runLogs(args[1:], stderr, backend)
	case "exec":
		err = runExec(args[1:], stderr, backend)
	case "inspect":
		err = runInspect(args[1:], stdout, backend)
	case "rm":
		err = runRemove(args[1:], stdout, stderr, backend)
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
	case "volume":
		err = runVolume(args[1:], stdout, stderr, backend)
	case "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ttos: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err != nil {
		var status exitStatus
		if errors.As(err, &status) {
			return status.code
		}
		fmt.Fprintf(stderr, "ttos: %v\n", err)
		return 1
	}
	return 0
}

type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprintf("%q", *s) }
func (s *stringSlice) Get() any       { return s }
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type mountRequests struct {
	host    []ttosbundle.HostMount
	managed []ttosbundle.ManagedVolume
}

func parseMountRequests(volumeSpecs, mountSpecs []string) (mountRequests, error) {
	var request mountRequests
	var hostVolumes, hostMounts []string
	for _, value := range volumeSpecs {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return mountRequests{}, fmt.Errorf("invalid volume specification %q; want SOURCE:CONTAINER[:ro]", value)
		}
		if isHostMountSource(parts[0]) {
			hostVolumes = append(hostVolumes, value)
			continue
		}
		readonly, err := parseVolumeOptions(value, parts[2:])
		if err != nil {
			return mountRequests{}, err
		}
		volume, err := normalizeManagedVolume(parts[0], parts[1], readonly)
		if err != nil {
			return mountRequests{}, err
		}
		request.managed = append(request.managed, volume)
	}
	for _, value := range mountSpecs {
		mountType := ""
		for _, field := range strings.Split(value, ",") {
			key, raw, found := strings.Cut(field, "=")
			if found && strings.TrimSpace(key) == "type" {
				mountType = strings.TrimSpace(raw)
				break
			}
		}
		if mountType != "volume" {
			hostMounts = append(hostMounts, value)
			continue
		}
		volume, err := parseManagedLongMount(value)
		if err != nil {
			return mountRequests{}, err
		}
		request.managed = append(request.managed, volume)
	}
	var err error
	request.host, err = parseHostMounts(hostVolumes, hostMounts)
	if err != nil {
		return mountRequests{}, err
	}
	return request, nil
}

func isHostMountSource(source string) bool {
	return filepath.IsAbs(source) || source == "." || strings.HasPrefix(source, "."+string(filepath.Separator))
}

func parseVolumeOptions(spec string, optionParts []string) (bool, error) {
	readonly := false
	if len(optionParts) == 0 {
		return readonly, nil
	}
	for _, option := range strings.Split(optionParts[0], ",") {
		switch option {
		case "ro", "readonly":
			readonly = true
		case "rw", "":
			readonly = false
		default:
			return false, fmt.Errorf("unsupported volume option %q in %q", option, spec)
		}
	}
	return readonly, nil
}

func parseManagedLongMount(value string) (ttosbundle.ManagedVolume, error) {
	name := ""
	destination := ""
	readonly := false
	for _, field := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if !found {
			if key == "ro" || key == "readonly" {
				readonly = true
				continue
			}
			return ttosbundle.ManagedVolume{}, fmt.Errorf("unsupported managed-volume field %q in %q", field, value)
		}
		raw = strings.TrimSpace(raw)
		switch key {
		case "type":
			if raw != "volume" {
				return ttosbundle.ManagedVolume{}, fmt.Errorf("managed mount %q has type %q", value, raw)
			}
		case "source", "src":
			name = raw
		case "destination", "dst", "target":
			destination = raw
		case "readonly", "ro":
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return ttosbundle.ManagedVolume{}, fmt.Errorf("invalid readonly value %q in %q", raw, value)
			}
			readonly = parsed
		default:
			return ttosbundle.ManagedVolume{}, fmt.Errorf("unsupported managed-volume field %q in %q", key, value)
		}
	}
	return normalizeManagedVolume(name, destination, readonly)
}

func normalizeManagedVolume(name, destination string, readonly bool) (ttosbundle.ManagedVolume, error) {
	if name == "" {
		return ttosbundle.ManagedVolume{}, errors.New("managed volume name must not be empty")
	}
	if isHostMountSource(name) {
		return ttosbundle.ManagedVolume{}, fmt.Errorf("managed volume name %q looks like a host path", name)
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == "/" {
		return ttosbundle.ManagedVolume{}, fmt.Errorf("managed volume destination %q must be an absolute non-root path", destination)
	}
	return ttosbundle.ManagedVolume{Name: name, Destination: filepath.Clean(destination), Readonly: readonly}, nil
}

func parseHostMounts(volumeSpecs, mountSpecs []string) ([]ttosbundle.HostMount, error) {
	mounts := make([]ttosbundle.HostMount, 0, len(volumeSpecs)+len(mountSpecs))
	for _, value := range volumeSpecs {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid volume specification %q; want HOST:CONTAINER:ro", value)
		}
		readonly := false
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro", "readonly":
					readonly = true
				case "rw":
					readonly = false
				case "":
				default:
					return nil, fmt.Errorf("unsupported volume option %q in %q", option, value)
				}
			}
		}
		mount, err := normalizeHostMount(parts[0], parts[1], readonly)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	for _, value := range mountSpecs {
		mountType := ""
		source := ""
		destination := ""
		readonly := false
		seen := make(map[string]struct{})
		for _, field := range strings.Split(value, ",") {
			key, raw, found := strings.Cut(field, "=")
			key = strings.TrimSpace(key)
			if !found {
				switch key {
				case "ro", "readonly":
					readonly = true
					continue
				default:
					return nil, fmt.Errorf("invalid --mount field %q in %q", field, value)
				}
			}
			raw = strings.TrimSpace(raw)
			canonical := key
			switch key {
			case "src":
				canonical = "source"
			case "dst", "target":
				canonical = "destination"
			case "ro":
				canonical = "readonly"
			}
			if _, ok := seen[canonical]; ok {
				return nil, fmt.Errorf("duplicate --mount field %q in %q", key, value)
			}
			seen[canonical] = struct{}{}
			switch canonical {
			case "type":
				mountType = raw
			case "source":
				source = raw
			case "destination":
				destination = raw
			case "readonly":
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid readonly value %q in %q", raw, value)
				}
				readonly = parsed
			default:
				return nil, fmt.Errorf("unsupported --mount field %q in %q", key, value)
			}
		}
		if mountType != "bind" {
			if mountType == "volume" || (mountType == "" && source != "" && !filepath.IsAbs(source)) {
				return nil, fmt.Errorf("managed volume %q is not implemented yet", source)
			}
			return nil, fmt.Errorf("--mount %q must specify type=bind", value)
		}
		mount, err := normalizeHostMount(source, destination, readonly)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

func normalizeHostMount(source, destination string, readonly bool) (ttosbundle.HostMount, error) {
	if source == "" || destination == "" {
		return ttosbundle.HostMount{}, errors.New("host bind mount requires both source and destination")
	}
	if !filepath.IsAbs(source) {
		if source != "." && !strings.HasPrefix(source, "."+string(filepath.Separator)) {
			return ttosbundle.HostMount{}, fmt.Errorf("managed volume %q is not implemented yet", source)
		}
		absolute, err := filepath.Abs(source)
		if err != nil {
			return ttosbundle.HostMount{}, fmt.Errorf("resolving host mount source %q: %w", source, err)
		}
		source = absolute
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == "/" {
		return ttosbundle.HostMount{}, fmt.Errorf("host mount destination %q must be an absolute non-root path", destination)
	}
	if !readonly {
		return ttosbundle.HostMount{}, fmt.Errorf("writable host bind %q:%q requires a managed CoW volume; add :ro for a shared read-only view", source, destination)
	}
	return ttosbundle.HostMount{Source: filepath.Clean(source), Destination: filepath.Clean(destination), Readonly: true}, nil
}

type runRequest struct {
	name           string
	reference      string
	platform       string
	args           []string
	env            []string
	cwd            string
	user           string
	hostname       string
	entrypoint     []string
	hostMounts     []ttosbundle.HostMount
	managedVolumes []ttosbundle.ManagedVolume
	detach         bool
	interactive    bool
}

type execRequest struct {
	target      selector
	args        []string
	env         []string
	cwd         string
	user        string
	interactive bool
}

type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("container exited with status %d", e.code) }

func runContainer(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	detach := fs.Bool("detach", false, "run in the background")
	fs.BoolVar(detach, "d", false, "run in the background")
	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "keep stdin open")
	terminal := fs.Bool("tty", false, "allocate a terminal")
	fs.BoolVar(terminal, "t", false, "allocate a terminal")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env stringSlice
	fs.Var(&env, "env", "set environment variable NAME=VALUE")
	fs.Var(&env, "e", "set environment variable NAME=VALUE")
	var volumes, mounts stringSlice
	fs.Var(&volumes, "volume", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&volumes, "v", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&mounts, "mount", "attach type=volume|bind,source=SOURCE,target=CONTAINER[,readonly]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttos run [OPTIONS] IMAGE [COMMAND...]")
	}
	if *terminal {
		return errors.New("-t/--tty requires the detachable internal console, which is not implemented yet")
	}
	if *detach && *interactive {
		return errors.New("detached interactive containers require the internal console, which is not implemented yet")
	}
	parsedMounts, err := parseMountRequests(volumes, mounts)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	request := runRequest{
		name:           containerName,
		reference:      fs.Arg(0),
		platform:       *platform,
		args:           append([]string(nil), fs.Args()[1:]...),
		env:            append([]string(nil), env...),
		cwd:            *cwd,
		user:           *user,
		hostname:       *hostname,
		hostMounts:     parsedMounts.host,
		managedVolumes: parsedMounts.managed,
		detach:         *detach,
		interactive:    *interactive,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "entrypoint" {
			request.entrypoint = []string{*entrypoint}
			if *entrypoint == "" {
				request.entrypoint = []string{}
			}
		}
	})
	status, err := backend.run(request)
	if err != nil {
		return err
	}
	if status != 0 {
		return exitStatus{code: status}
	}
	if request.detach {
		fmt.Fprintln(stdout, request.name)
	}
	return nil
}

func runCreate(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env stringSlice
	fs.Var(&env, "env", "set environment variable NAME=VALUE")
	fs.Var(&env, "e", "set environment variable NAME=VALUE")
	var volumes, mounts stringSlice
	fs.Var(&volumes, "volume", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&volumes, "v", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&mounts, "mount", "attach type=volume|bind,source=SOURCE,target=CONTAINER[,readonly]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttos create [OPTIONS] IMAGE [COMMAND...]")
	}
	parsedMounts, err := parseMountRequests(volumes, mounts)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	request := runRequest{
		name:           containerName,
		reference:      fs.Arg(0),
		platform:       *platform,
		args:           append([]string(nil), fs.Args()[1:]...),
		env:            append([]string(nil), env...),
		cwd:            *cwd,
		user:           *user,
		hostname:       *hostname,
		hostMounts:     parsedMounts.host,
		managedVolumes: parsedMounts.managed,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "entrypoint" {
			request.entrypoint = []string{*entrypoint}
			if *entrypoint == "" {
				request.entrypoint = []string{}
			}
		}
	})
	if err := backend.create(request); err != nil {
		return err
	}
	fmt.Fprintln(stdout, request.name)
	return nil
}

func runStart(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	attach := fs.Bool("attach", false, "attach stdout and stderr and wait")
	fs.BoolVar(attach, "a", false, "attach stdout and stderr and wait")
	interactive := fs.Bool("interactive", false, "keep stdin open (requires --attach)")
	fs.BoolVar(interactive, "i", false, "keep stdin open (requires --attach)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos start [--attach] CONTAINER")
	}
	if *interactive && !*attach {
		return errors.New("--interactive requires --attach")
	}
	status, err := backend.start(fs.Arg(0), !*attach, *interactive)
	if err != nil {
		return err
	}
	if status != 0 {
		return exitStatus{code: status}
	}
	if !*attach {
		fmt.Fprintln(stdout, fs.Arg(0))
	}
	return nil
}

func runAttach(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interactive := fs.Bool("interactive", false, "forward stdin")
	fs.BoolVar(interactive, "i", false, "forward stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos attach [-i] CONTAINER[@BRANCH]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	status, err := backend.attach(target, *interactive)
	if err != nil {
		return err
	}
	if code := waitStatusExitCode(status); code != 0 {
		return exitStatus{code: code}
	}
	return nil
}

func runLogs(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", false, "follow log output")
	fs.BoolVar(follow, "f", false, "follow log output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos logs [-f] CONTAINER[@BRANCH]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := backend.logs(target, *follow); err != nil {
		return fmt.Errorf("reading logs for %s: %w", target, err)
	}
	return nil
}

func runExec(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "keep stdin open")
	terminal := fs.Bool("tty", false, "allocate a terminal")
	fs.BoolVar(terminal, "t", false, "allocate a terminal")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	var env stringSlice
	fs.Var(&env, "env", "set environment variable NAME=VALUE")
	fs.Var(&env, "e", "set environment variable NAME=VALUE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: ttos exec [OPTIONS] CONTAINER[@BRANCH] COMMAND [ARG...]")
	}
	if *terminal {
		return errors.New("-t/--tty requires the detachable internal console, which is not implemented yet")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	status, err := backend.exec(execRequest{
		target:      target,
		args:        append([]string(nil), fs.Args()[1:]...),
		env:         append([]string(nil), env...),
		cwd:         *cwd,
		user:        *user,
		interactive: *interactive,
	})
	if err != nil {
		return err
	}
	if status != 0 {
		return exitStatus{code: status}
	}
	return nil
}

// InspectInfo combines persistent image/bundle configuration with live state.
type InspectInfo struct {
	Container    ttosbundle.ContainerInfo `json:"container"`
	Runtime      *Family                  `json:"runtime,omitempty"`
	RuntimeError string                   `json:"runtime_error,omitempty"`
}

func runInspect(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttos inspect CONTAINER [CONTAINER...]")
	}
	results := make([]InspectInfo, 0, len(args))
	for _, name := range args {
		info, err := backend.inspect(name)
		if err != nil {
			return err
		}
		results = append(results, info)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func runRemove(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "force removal of a running container family")
	fs.BoolVar(force, "f", false, "force removal of a running container family")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos rm [--force] CONTAINER [CONTAINER...]")
	}
	for _, name := range fs.Args() {
		if strings.Contains(name, "@") {
			return fmt.Errorf("rm removes a whole container family; use 'branch rm' for %q", name)
		}
		if err := backend.remove(name, *force); err != nil {
			return err
		}
		fmt.Fprintln(stdout, name)
	}
	return nil
}

func runPull(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	unpack := fs.Bool("unpack", false, "also unpack through a containerd snapshotter (may require mount privileges)")
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

func runVolume(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttos volume create|ls|inspect|rm [OPTIONS]")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("volume create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		driver := fs.String("driver", "local-tmpfs", "volume driver")
		fs.StringVar(driver, "d", "local-tmpfs", "volume driver")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() > 1 {
			return errors.New("usage: ttos volume create [--driver local-tmpfs] [NAME]")
		}
		if *driver != "local-tmpfs" {
			return fmt.Errorf("unsupported volume driver %q", *driver)
		}
		name := "ttos-volume-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if fs.NArg() == 1 {
			name = fs.Arg(0)
		}
		info, err := backend.ensureVolume(name)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, info.Name)
		return nil
	case "ls", "list":
		fs := flag.NewFlagSet("volume ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		quiet := fs.Bool("quiet", false, "only display volume names")
		fs.BoolVar(quiet, "q", false, "only display volume names")
		jsonOutput := fs.Bool("json", false, "emit stable JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: ttos volume ls [--quiet|--json]")
		}
		volumes, err := backend.listVolumes()
		if err != nil {
			return err
		}
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(volumes)
		}
		if !*quiet {
			fmt.Fprintln(stdout, "DRIVER\tVOLUME NAME")
		}
		for _, volume := range volumes {
			if *quiet {
				fmt.Fprintln(stdout, volume.Name)
			} else {
				fmt.Fprintf(stdout, "%s\t%s\n", volume.Driver, volume.Name)
			}
		}
		return nil
	case "inspect":
		if len(args) < 2 {
			return errors.New("usage: ttos volume inspect VOLUME [VOLUME...]")
		}
		volumes := make([]ttosbundle.VolumeInfo, 0, len(args)-1)
		for _, name := range args[1:] {
			volume, err := backend.inspectVolume(name)
			if err != nil {
				return err
			}
			volumes = append(volumes, volume)
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(volumes)
	case "rm", "remove":
		if len(args) < 2 {
			return errors.New("usage: ttos volume rm VOLUME [VOLUME...]")
		}
		for _, name := range args[1:] {
			if err := backend.removeVolume(name); err != nil {
				return err
			}
			fmt.Fprintln(stdout, name)
		}
		return nil
	default:
		return fmt.Errorf("unknown volume command %q", args[0])
	}
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
	create(request runRequest) error
	run(request runRequest) (int, error)
	start(name string, detach, interactive bool) (int, error)
	attach(target selector, interactive bool) (uint32, error)
	logs(target selector, follow bool) error
	exec(request execRequest) (int, error)
	inspect(name string) (InspectInfo, error)
	remove(name string, force bool) error
	listFamilies() ([]Family, error)
	fork(source selector, destination string, paused bool) (Branch, error)
	pause(target selector) error
	resume(target selector) error
	destroy(target selector) error
	signal(target selector, signal int, all bool) error
	wait(target selector) (uint32, error)
	ensureVolume(name string) (ttosbundle.VolumeInfo, error)
	listVolumes() ([]ttosbundle.VolumeInfo, error)
	inspectVolume(name string) (ttosbundle.VolumeInfo, error)
	removeVolume(name string) error
}

type runtimeBackend struct {
	root       string
	runsc      string
	containerd string
	namespace  string
	dataRoot   string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

func (b *runtimeBackend) run(request runRequest) (int, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	prepared, err := store.Prepare(context.Background(), ttosbundle.PrepareOptions{
		SpecOptions: ttosbundle.SpecOptions{
			Name:           request.name,
			Args:           request.args,
			Env:            request.env,
			Cwd:            request.cwd,
			User:           request.user,
			Hostname:       request.hostname,
			Entrypoint:     request.entrypoint,
			ManagedVolumes: request.managedVolumes,
			HostMounts:     request.hostMounts,
		},
		Reference: request.reference,
		Platform:  request.platform,
	})
	if err != nil {
		return 1, err
	}
	status, err := b.launch(request.name, prepared.BundleDir, request.detach, request.interactive)
	if err == nil {
		b.recordLaunchState(request.name, request.detach, status)
	}
	return status, err
}

func (b *runtimeBackend) create(request runRequest) error {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	_, err := store.Prepare(context.Background(), ttosbundle.PrepareOptions{
		SpecOptions: ttosbundle.SpecOptions{
			Name:           request.name,
			Args:           request.args,
			Env:            request.env,
			Cwd:            request.cwd,
			User:           request.user,
			Hostname:       request.hostname,
			Entrypoint:     request.entrypoint,
			HostMounts:     request.hostMounts,
			ManagedVolumes: request.managedVolumes,
		},
		Reference: request.reference,
		Platform:  request.platform,
	})
	return err
}

func (b *runtimeBackend) start(name string, detach, interactive bool) (int, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	info, err := store.LoadContainer(name)
	if err != nil {
		return 1, err
	}
	status, err := b.launch(name, info.BundleDir, detach, interactive)
	if err == nil {
		b.recordLaunchState(name, detach, status)
	}
	return status, err
}

func (b *runtimeBackend) exec(request execRequest) (int, error) {
	args := []string{"--root", b.root, "exec", "--branch", request.target.branch}
	for _, value := range request.env {
		args = append(args, "--env", value)
	}
	if request.cwd != "" {
		args = append(args, "--cwd", request.cwd)
	}
	if request.user != "" {
		args = append(args, "--user", request.user)
	}
	args = append(args, request.target.container)
	args = append(args, request.args...)
	command := exec.Command(b.runsc, args...)
	if request.interactive {
		command.Stdin = b.stdin
	}
	command.Stdout = b.stdout
	command.Stderr = b.stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("executing runsc: %w", err)
	}
	return 0, nil
}

type consoleWriteResult struct {
	N          int  `json:"N"`
	WouldBlock bool `json:"WouldBlock"`
}

type attachWaitResult struct {
	status uint32
	err    error
}

type consoleLogFollower struct {
	file      *os.File
	path      string
	nextPath  string
	retained  os.FileInfo
	pending   []byte
	stdoutEOF bool
	stderrEOF bool
}

func openConsoleLogFollower(path string) (*consoleLogFollower, error) {
	activePath := path
	nextPath := ""
	var retained os.FileInfo
	if info, err := os.Stat(path + ttosconsole.RotatedSuffix); err == nil {
		activePath = path + ttosconsole.RotatedSuffix
		nextPath = path
		retained = info
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stating rotated console log: %w", err)
	}
	file, err := os.Open(activePath)
	if err != nil {
		return nil, fmt.Errorf("opening console log: %w", err)
	}
	return &consoleLogFollower{file: file, path: path, nextPath: nextPath, retained: retained}, nil
}

func (f *consoleLogFollower) drain(stdout, stderr io.Writer) (bool, error) {
	progress := false
	buf := make([]byte, 64*1024)
	for {
		n, err := f.file.Read(buf)
		if n != 0 {
			f.pending = append(f.pending, buf[:n]...)
			progress = true
			if err := f.consume(stdout, stderr); err != nil {
				return false, err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				advanced, advanceErr := f.advance()
				if advanceErr != nil {
					return false, advanceErr
				}
				if advanced {
					progress = true
					continue
				}
				break
			}
			return false, fmt.Errorf("reading console log: %w", err)
		}
		if n == 0 {
			break
		}
	}
	return progress, f.consume(stdout, stderr)
}

func (f *consoleLogFollower) advance() (bool, error) {
	if f.nextPath != "" {
		if len(f.pending) != 0 {
			return false, fmt.Errorf("rotated console log ended with %d bytes of an incomplete frame", len(f.pending))
		}
		file, err := os.Open(f.nextPath)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("opening next console log generation: %w", err)
		}
		if err := f.file.Close(); err != nil {
			file.Close()
			return false, fmt.Errorf("closing previous console log generation: %w", err)
		}
		f.file = file
		f.nextPath = ""
		return true, nil
	}
	openInfo, err := f.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stating open console log: %w", err)
	}
	rotatedPath := f.path + ttosconsole.RotatedSuffix
	rotatedInfo, rotatedErr := os.Stat(rotatedPath)
	if rotatedErr == nil && !os.SameFile(openInfo, rotatedInfo) &&
		(f.retained == nil || !os.SameFile(f.retained, rotatedInfo)) {
		if len(f.pending) != 0 {
			return false, fmt.Errorf("console log rotated with %d bytes of an incomplete frame", len(f.pending))
		}
		file, err := os.Open(rotatedPath)
		if err != nil {
			return false, fmt.Errorf("opening retained console log generation: %w", err)
		}
		if err := f.file.Close(); err != nil {
			file.Close()
			return false, fmt.Errorf("closing superseded console log generation: %w", err)
		}
		f.file = file
		f.nextPath = f.path
		f.retained = rotatedInfo
		return true, nil
	} else if rotatedErr != nil && !os.IsNotExist(rotatedErr) {
		return false, fmt.Errorf("stating retained console log generation: %w", rotatedErr)
	}
	pathInfo, err := os.Stat(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stating current console log: %w", err)
	}
	if os.SameFile(openInfo, pathInfo) {
		return false, nil
	}
	if len(f.pending) != 0 {
		return false, fmt.Errorf("console log rotated with %d bytes of an incomplete frame", len(f.pending))
	}
	if rotatedErr == nil && os.SameFile(openInfo, rotatedInfo) {
		f.retained = rotatedInfo
	}
	file, err := os.Open(f.path)
	if err != nil {
		return false, fmt.Errorf("opening rotated console log: %w", err)
	}
	if err := f.file.Close(); err != nil {
		file.Close()
		return false, fmt.Errorf("closing rotated console log: %w", err)
	}
	f.file = file
	return true, nil
}

func (f *consoleLogFollower) consume(stdout, stderr io.Writer) error {
	for {
		frame, consumed, complete, err := ttosconsole.DecodeFrame(f.pending)
		if err != nil {
			return fmt.Errorf("decoding console log: %w", err)
		}
		if !complete {
			break
		}
		f.pending = f.pending[consumed:]
		var output io.Writer
		switch frame.Stream {
		case ttosconsole.StreamStdout:
			output = stdout
			f.stdoutEOF = frame.EOF
		case ttosconsole.StreamStderr:
			output = stderr
			f.stderrEOF = frame.EOF
		}
		if len(frame.Data) != 0 && output != nil {
			n, err := output.Write(frame.Data)
			if err != nil {
				return fmt.Errorf("writing attached stream %d: %w", frame.Stream, err)
			}
			if n != len(frame.Data) {
				return fmt.Errorf("writing attached stream %d: %w", frame.Stream, io.ErrShortWrite)
			}
		}
	}
	return nil
}

func (b *runtimeBackend) attach(target selector, interactive bool) (uint32, error) {
	paths, err := b.ensureConsoleDrain(target, false)
	if err != nil {
		return 0, err
	}
	follower, err := openConsoleLogFollower(paths.Log)
	if err != nil {
		return 0, err
	}
	defer follower.file.Close()
	waitCh := make(chan attachWaitResult, 1)
	go func() {
		status, err := b.wait(target)
		waitCh <- attachWaitResult{status: status, err: err}
	}()
	inputErrCh := make(chan error, 1)
	if interactive {
		go func() { inputErrCh <- b.pumpConsoleInput(target) }()
	}
	var waitResult *attachWaitResult
	for {
		progress, err := follower.drain(b.stdout, b.stderr)
		if err != nil {
			return 0, err
		}
		if waitResult != nil && follower.stdoutEOF && follower.stderrEOF {
			return waitResult.status, nil
		}
		select {
		case result := <-waitCh:
			if result.err != nil {
				return 0, result.err
			}
			waitResult = &result
			waitCh = nil
		case err := <-inputErrCh:
			if err != nil {
				return 0, err
			}
			inputErrCh = nil
		default:
		}
		if !progress {
			active, err := consoleDrainActive(paths.Lock)
			if err != nil {
				return 0, err
			}
			if !active && (!follower.stdoutEOF || !follower.stderrEOF) {
				return 0, consoleDrainStoppedError(paths.Error)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (b *runtimeBackend) logs(target selector, follow bool) error {
	paths, err := b.ensureConsoleDrain(target, false)
	if err != nil {
		return err
	}
	follower, err := openConsoleLogFollower(paths.Log)
	if err != nil {
		return err
	}
	defer follower.file.Close()
	for {
		progress, err := follower.drain(b.stdout, b.stderr)
		if err != nil {
			return err
		}
		if !follow || (follower.stdoutEOF && follower.stderrEOF) {
			return nil
		}
		if !progress {
			active, err := consoleDrainActive(paths.Lock)
			if err != nil {
				return err
			}
			if !active {
				return consoleDrainStoppedError(paths.Error)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (b *runtimeBackend) ensureConsoleDrain(target selector, reset bool) (ttosbundle.ConsolePaths, error) {
	store := ttosbundle.Store{Root: b.dataRoot}
	paths, err := store.BranchConsolePaths(target.container, target.branch)
	if err != nil {
		return ttosbundle.ConsolePaths{}, err
	}
	lockFile, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("opening console drain lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if reset {
				return ttosbundle.ConsolePaths{}, fmt.Errorf("console drain for %s is still active", target)
			}
			return paths, nil
		}
		return ttosbundle.ConsolePaths{}, fmt.Errorf("locking console drain: %w", err)
	}
	if reset {
		if err := os.Remove(paths.Log + ttosconsole.RotatedSuffix); err != nil && !os.IsNotExist(err) {
			return ttosbundle.ConsolePaths{}, fmt.Errorf("removing rotated console log: %w", err)
		}
	}
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if reset {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	logFile, err := os.OpenFile(paths.Log, logFlags, 0600)
	if err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("opening console log: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("closing prepared console log: %w", err)
	}
	errorFlags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if reset {
		errorFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	errorFile, err := os.OpenFile(paths.Error, errorFlags, 0600)
	if err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("opening console error log: %w", err)
	}
	defer errorFile.Close()

	command := exec.Command(b.runsc,
		"--root", b.root,
		"timetravel", "--console", "init",
		"--console-log", paths.Log,
		"--console-log-max-bytes", strconv.FormatInt(defaultConsoleLogMax, 10),
		target.container, "console-follow", target.branch,
	)
	command.Stderr = errorFile
	// The inherited duplicate keeps the advisory lock held for exactly the
	// lifetime of the drainer, preventing multiple consumers of one pipe.
	command.ExtraFiles = []*os.File{lockFile}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("starting console drain for %s: %w", target, err)
	}
	if err := command.Process.Release(); err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("releasing console drain process for %s: %w", target, err)
	}
	return paths, nil
}

func consoleDrainActive(lockPath string) (bool, error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, fmt.Errorf("opening console drain lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("probing console drain lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		return false, fmt.Errorf("unlocking console drain probe: %w", err)
	}
	return false, nil
}

func consoleDrainStoppedError(errorPath string) error {
	data, err := os.ReadFile(errorPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("console drain stopped before output EOF; reading diagnostics: %w", err)
	}
	message := strings.TrimSpace(string(data))
	const maxDiagnostic = 4096
	if len(message) > maxDiagnostic {
		message = message[len(message)-maxDiagnostic:]
	}
	if message == "" {
		return errors.New("console drain stopped before output EOF")
	}
	return fmt.Errorf("console drain stopped before output EOF: %s", message)
}

func (b *runtimeBackend) writeConsole(target selector, data []byte) (consoleWriteResult, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	output, err := b.command("timetravel", "--console", "init", "--data-base64", encoded, target.container, "console-write", target.branch)
	if err != nil {
		return consoleWriteResult{}, err
	}
	var result consoleWriteResult
	if err := json.Unmarshal(output, &result); err != nil {
		return consoleWriteResult{}, fmt.Errorf("decoding console write: %w", err)
	}
	return result, nil
}

func (b *runtimeBackend) closeConsoleStdin(target selector) error {
	_, err := b.command("timetravel", "--console", "init", target.container, "console-close-stdin", target.branch)
	return err
}

func (b *runtimeBackend) pumpConsoleInput(target selector) error {
	if b.stdin == nil {
		return b.closeConsoleStdin(target)
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := b.stdin.Read(buf)
		for written := 0; written < n; {
			result, err := b.writeConsole(target, buf[written:n])
			if err != nil {
				return err
			}
			written += result.N
			if result.WouldBlock || result.N == 0 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return b.closeConsoleStdin(target)
			}
			return fmt.Errorf("reading attached stdin: %w", readErr)
		}
	}
}

func waitStatusExitCode(status uint32) int {
	ws := syscall.WaitStatus(status)
	if ws.Exited() {
		return ws.ExitStatus()
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return 1
}

func (b *runtimeBackend) recordLaunchState(name string, detach bool, status int) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	state := "exited"
	var exitStatus *int
	if detach && status == 0 {
		state = "running"
	} else {
		exitStatus = &status
	}
	if err := store.UpdateState(name, state, exitStatus); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttos: warning: recording state for %s: %v\n", name, err)
	}
}

func (b *runtimeBackend) inspect(name string) (InspectInfo, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	containerInfo, err := store.LoadContainer(name)
	if err != nil {
		return InspectInfo{}, err
	}
	result := InspectInfo{Container: containerInfo}
	families, err := b.listFamilies()
	if err != nil {
		result.RuntimeError = err.Error()
		return result, nil
	}
	for i := range families {
		if families[i].ID == name {
			result.Runtime = &families[i]
			break
		}
	}
	return result, nil
}

func (b *runtimeBackend) remove(name string, force bool) error {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	if _, err := store.LoadContainer(name); err != nil {
		return err
	}
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("checking runtime state before removing %q: %w", name, err)
	}
	for _, family := range families {
		if family.ID != name {
			continue
		}
		if !force && family.State != "stopped" && family.State != "exited" {
			return fmt.Errorf("container %q is %s; stop it or use --force", name, family.State)
		}
		args := []string{"delete"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, name)
		if _, err := b.command(args...); err != nil {
			return fmt.Errorf("deleting runtime container %q: %w", name, err)
		}
		break
	}
	return store.RemoveContainer(name)
}

func (b *runtimeBackend) launch(name, bundleDir string, detach, interactive bool) (int, error) {
	args := []string{
		"--root", b.root,
		"--rootless=true",
		"--network=none",
		"--directfs=false",
		"--overlay2=all:memory",
		"run", "--detach", "--bundle", bundleDir,
	}
	args = append(args, name)
	command := exec.Command(b.runsc, args...)
	command.Stdout = b.stdout
	command.Stderr = b.stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("starting runsc: %w", err)
	}
	target := selector{container: name, branch: defaultBranchID}
	if _, err := b.ensureConsoleDrain(target, true); err != nil {
		return 1, err
	}
	if detach {
		return 0, nil
	}
	status, err := b.attach(target, interactive)
	if err != nil {
		return 1, err
	}
	return waitStatusExitCode(status), nil
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

func (b *runtimeBackend) ensureVolume(name string) (ttosbundle.VolumeInfo, error) {
	return (ttosbundle.Store{Root: b.dataRoot}).EnsureVolume(name)
}

func (b *runtimeBackend) listVolumes() ([]ttosbundle.VolumeInfo, error) {
	return (ttosbundle.Store{Root: b.dataRoot}).ListVolumes()
}

func (b *runtimeBackend) inspectVolume(name string) (ttosbundle.VolumeInfo, error) {
	return (ttosbundle.Store{Root: b.dataRoot}).LoadVolume(name)
}

func (b *runtimeBackend) removeVolume(name string) error {
	store := ttosbundle.Store{Root: b.dataRoot}
	if err := store.RemoveVolume(name); err != nil {
		return err
	}
	return nil
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
	runtimeFamilies, runtimeErr := b.listRuntimeFamilies()
	if b.dataRoot == "" {
		return runtimeFamilies, runtimeErr
	}
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	containers, storeErr := store.ListContainers()
	if storeErr != nil {
		return nil, storeErr
	}
	byID := make(map[string]Family, len(containers)+len(runtimeFamilies))
	for _, container := range containers {
		state := container.State
		if state == "" {
			state = "created"
		}
		byID[container.Name] = Family{
			ID:       container.Name,
			State:    state,
			Branches: []Branch{{ID: defaultBranchID, State: state}},
		}
	}
	if runtimeErr == nil {
		runtimeIDs := make(map[string]struct{}, len(runtimeFamilies))
		for _, family := range runtimeFamilies {
			runtimeIDs[family.ID] = struct{}{}
			byID[family.ID] = family
		}
		for _, container := range containers {
			if container.State == "running" {
				if _, ok := runtimeIDs[container.Name]; ok {
					continue
				}
				_ = store.UpdateState(container.Name, "exited", nil)
				byID[container.Name] = Family{ID: container.Name, State: "exited", Branches: []Branch{{ID: defaultBranchID, State: "exited"}}}
			}
		}
	} else {
		if len(byID) == 0 {
			return nil, runtimeErr
		}
		for id, family := range byID {
			family.Error = runtimeErr.Error()
			byID[id] = family
		}
	}
	families := make([]Family, 0, len(byID))
	for _, family := range byID {
		families = append(families, family)
	}
	sort.Slice(families, func(i, j int) bool { return families[i].ID < families[j].ID })
	return families, nil
}

func (b *runtimeBackend) listRuntimeFamilies() ([]Family, error) {
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
	target := selector{container: source.container, branch: destination}
	if _, err := b.ensureConsoleDrain(target, true); err != nil {
		cleanupErr := b.destroy(target)
		if cleanupErr != nil {
			return Branch{}, fmt.Errorf("starting console drain after fork: %w (cleanup also failed: %v)", err, cleanupErr)
		}
		return Branch{}, fmt.Errorf("starting console drain after fork: %w; forked branch was destroyed", err)
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
