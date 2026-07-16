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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

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
	defaultConsoleLogFiles  = 2
)

var (
	defaultDetachKeys = []byte{0x10, 0x11} // Ctrl-P, Ctrl-Q.
	errDetached       = errors.New("detached from container")
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
  logs [-f] [--tail N] TARGET    Print or follow a branch's init-console logs
  exec [OPTIONS] TARGET COMMAND  Execute a command in a running branch
  inspect CONTAINER...           Show persistent and live branch state
  rm [--force] CONTAINER...      Remove container metadata and runtime state
  ls [--tree|--json]              List container families and branch trees
  ps [--all|--quiet]              List container branches in a flat table
  top [--json] TARGET             Show processes in one live branch
  fork [--paused] SOURCE [DEST]   Fork SOURCE into a new live branch
  fork --check [--json] SOURCE    Validate SOURCE without creating a branch
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
	case "ps":
		err = runPs(args[1:], stdout, stderr, backend)
	case "top":
		err = runTop(args[1:], stdout, stderr, backend)
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

func parseLogOptions(values []string) (int64, int, error) {
	maxBytes := defaultConsoleLogMax
	maxFiles := defaultConsoleLogFiles
	for _, option := range values {
		key, value, found := strings.Cut(option, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return 0, 0, fmt.Errorf("invalid --log-opt %q; want KEY=VALUE", option)
		}
		switch key {
		case "max-size":
			parsed, err := parseLogSize(value)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid --log-opt max-size=%q: %w", value, err)
			}
			maxBytes = parsed
		case "max-file":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 {
				return 0, 0, fmt.Errorf("invalid --log-opt max-file=%q: must be a positive integer", value)
			}
			maxFiles = parsed
		default:
			return 0, 0, fmt.Errorf("unsupported --log-opt %q; supported options are max-size and max-file", key)
		}
	}
	return maxBytes, maxFiles, nil
}

func parseLogSize(value string) (int64, error) {
	type unit struct {
		suffix     string
		multiplier float64
	}
	units := []unit{
		{suffix: "tib", multiplier: 1 << 40}, {suffix: "tb", multiplier: 1 << 40},
		{suffix: "gib", multiplier: 1 << 30}, {suffix: "gb", multiplier: 1 << 30},
		{suffix: "mib", multiplier: 1 << 20}, {suffix: "mb", multiplier: 1 << 20},
		{suffix: "kib", multiplier: 1 << 10}, {suffix: "kb", multiplier: 1 << 10},
		{suffix: "t", multiplier: 1 << 40}, {suffix: "g", multiplier: 1 << 30},
		{suffix: "m", multiplier: 1 << 20}, {suffix: "k", multiplier: 1 << 10},
		{suffix: "b", multiplier: 1},
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return 0, errors.New("size must not be empty")
	}
	number := normalized
	multiplier := float64(1)
	for _, candidate := range units {
		if strings.HasSuffix(normalized, candidate.suffix) {
			number = strings.TrimSpace(strings.TrimSuffix(normalized, candidate.suffix))
			multiplier = candidate.multiplier
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, errors.New("size must be a positive number with an optional k, m, g, or t suffix")
	}
	bytes := parsed * multiplier
	if bytes > float64(math.MaxInt64) {
		return 0, errors.New("size overflows int64 bytes")
	}
	result := int64(bytes)
	if result < 1 {
		return 0, errors.New("size must be at least one byte")
	}
	return result, nil
}

type runRequest struct {
	name               string
	reference          string
	platform           string
	args               []string
	env                []string
	cwd                string
	user               string
	hostname           string
	entrypoint         []string
	hostMounts         []ttosbundle.HostMount
	consoleLogMaxBytes int64
	consoleLogMaxFiles int
	managedVolumes     []ttosbundle.ManagedVolume
	detach             bool
	interactive        bool
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
	var logOptions stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option max-size=SIZE or max-file=COUNT")
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
	logMaxBytes, logMaxFiles, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		platform:           *platform,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                append([]string(nil), env...),
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		hostMounts:         parsedMounts.host,
		managedVolumes:     parsedMounts.managed,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
		detach:             *detach,
		interactive:        *interactive,
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
	var logOptions stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option max-size=SIZE or max-file=COUNT")
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
	logMaxBytes, logMaxFiles, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		platform:           *platform,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                append([]string(nil), env...),
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		hostMounts:         parsedMounts.host,
		managedVolumes:     parsedMounts.managed,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
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

func parseDetachKeys(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("detach key sequence must not be empty")
	}
	parts := strings.Split(strings.ToLower(value), ",")
	keys := make([]byte, 0, len(parts))
	for _, part := range parts {
		if len(part) == 1 {
			keys = append(keys, part[0])
			continue
		}
		if !strings.HasPrefix(part, "ctrl-") || len(part) != len("ctrl-")+1 {
			return nil, fmt.Errorf("invalid detach key %q", part)
		}
		key := part[len("ctrl-")]
		switch {
		case key == '@':
			keys = append(keys, 0)
		case key >= 'a' && key <= 'z':
			keys = append(keys, key-'a'+1)
		case key >= '[' && key <= '_':
			keys = append(keys, key-'['+27)
		default:
			return nil, fmt.Errorf("invalid control detach key %q", part)
		}
	}
	return keys, nil
}

type detachKeyFilter struct {
	sequence []byte
	pending  []byte
}

func (f *detachKeyFilter) push(data []byte) ([]byte, bool) {
	if len(f.sequence) == 0 {
		return append([]byte(nil), data...), false
	}
	combined := make([]byte, 0, len(f.pending)+len(data))
	combined = append(combined, f.pending...)
	combined = append(combined, data...)
	f.pending = f.pending[:0]
	if index := bytes.Index(combined, f.sequence); index >= 0 {
		return append([]byte(nil), combined[:index]...), true
	}
	keep := len(f.sequence) - 1
	if keep > len(combined) {
		keep = len(combined)
	}
	for keep > 0 && !bytes.Equal(combined[len(combined)-keep:], f.sequence[:keep]) {
		keep--
	}
	output := append([]byte(nil), combined[:len(combined)-keep]...)
	f.pending = append(f.pending, combined[len(combined)-keep:]...)
	return output, false
}

func (f *detachKeyFilter) flush() []byte {
	output := append([]byte(nil), f.pending...)
	f.pending = f.pending[:0]
	return output
}

func makeRawTerminal(input io.Reader) (func() error, error) {
	file, ok := input.(*os.File)
	if !ok {
		return nil, nil
	}
	fd := int(file.Fd())
	if !term.IsTerminal(fd) {
		return nil, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("making attached stdin terminal raw: %w", err)
	}
	return func() error {
		return term.Restore(fd, state)
	}, nil
}

func runAttach(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interactive := fs.Bool("interactive", false, "forward stdin")
	fs.BoolVar(interactive, "i", false, "forward stdin")
	detachKeysValue := fs.String("detach-keys", "ctrl-p,ctrl-q", "override the key sequence for detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos attach [-i] [--detach-keys KEYS] CONTAINER[@BRANCH]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	detachKeys, err := parseDetachKeys(*detachKeysValue)
	if err != nil {
		return err
	}
	status, err := backend.attach(target, *interactive, detachKeys)
	if err != nil {
		if errors.Is(err, errDetached) {
			return nil
		}
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
	tailValue := fs.String("tail", "all", "number of lines to show from the end of the logs")
	fs.StringVar(tailValue, "n", "all", "number of lines to show from the end of the logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos logs [-f] [--tail all|N] CONTAINER[@BRANCH]")
	}
	tail := -1
	if parsed, err := strconv.Atoi(*tailValue); err == nil && parsed >= 0 {
		tail = parsed
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := backend.logs(target, *follow, tail); err != nil {
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

func runPs(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show created and exited branches")
	fs.BoolVar(all, "a", false, "show created and exited branches")
	quiet := fs.Bool("quiet", false, "only display branch-qualified selectors")
	fs.BoolVar(quiet, "q", false, "only display branch-qualified selectors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: ttos ps [--all] [--quiet]")
	}
	families, err := backend.listFamilies()
	if err != nil {
		return fmt.Errorf("listing container families: %w", err)
	}
	var table *tabwriter.Writer
	if !*quiet {
		table = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "CONTAINER\tBRANCH\tSTATE\tPARENT")
	}
	for _, family := range families {
		for _, branch := range family.Branches {
			if !*all && branch.State != "running" && branch.State != "paused" {
				continue
			}
			if *quiet {
				fmt.Fprintf(stdout, "%s@%s\n", family.ID, branch.ID)
				continue
			}
			parent := branch.ParentID
			if parent == "" {
				parent = "-"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", family.ID, branch.ID, branch.State, parent)
		}
	}
	if table != nil {
		return table.Flush()
	}
	return nil
}

func runTop(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos top [--json] CONTAINER[@BRANCH]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	processes, err := backend.processes(target)
	if err != nil {
		return fmt.Errorf("listing processes in %s: %w", target, err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(processes)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "UID\tPID\tPPID\tPGID\tC\tTTY\tSTIME\tTIME\tCMD")
	for _, process := range processes {
		fmt.Fprintf(table, "%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			process.UID, process.PID, process.PPID, process.PGID, process.C,
			process.TTY, process.STime, process.Time, process.Cmd)
	}
	return table.Flush()
}

func runFork(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	paused := fs.Bool("paused", false, "leave the child branch paused")
	check := fs.Bool("check", false, "report source eligibility without forking")
	jsonOutput := fs.Bool("json", false, "emit eligibility as JSON (requires --check)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("usage: ttos fork [--paused] SOURCE [DEST] | ttos fork --check [--json] SOURCE")
	}
	source, err := parseSelector(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid source: %w", err)
	}
	if *check {
		if fs.NArg() != 1 || *paused {
			return errors.New("fork --check accepts one SOURCE and cannot be combined with --paused")
		}
		result, err := backend.checkFork(source)
		if err != nil {
			return fmt.Errorf("checking %s: %w", source, err)
		}
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(result); err != nil {
				return err
			}
		} else {
			if err := writeForkEligibility(stdout, source, result); err != nil {
				return err
			}
		}
		if !result.Eligible {
			return exitStatus{code: 1}
		}
		return nil
	}
	if *jsonOutput {
		return errors.New("--json requires --check")
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

func writeForkEligibility(w io.Writer, source selector, result ForkEligibility) error {
	state := "ineligible"
	if result.Eligible {
		state = "eligible"
	}
	if _, err := fmt.Fprintf(w, "%s %s\n", source, state); err != nil {
		return err
	}
	if result.Reason != "" {
		if _, err := fmt.Fprintf(w, "reason: %s\n", result.Reason); err != nil {
			return err
		}
	}
	if len(result.Issues) == 0 {
		return nil
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PID\tFD\tRESOURCE\tREASON")
	for _, issue := range result.Issues {
		fd := strconv.Itoa(int(issue.FD))
		if issue.FD < 0 {
			fd = "mapping"
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\n", issue.PID, fd, issue.Resource, issue.Reason)
	}
	return table.Flush()
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

// ForkResourceIssue identifies one resource that blocks a live fork.
type ForkResourceIssue struct {
	PID      int32  `json:"pid"`
	FD       int32  `json:"fd"`
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
}

// ForkEligibility is a stable read-only fork validation report.
type ForkEligibility struct {
	Eligible bool                `json:"eligible"`
	Issues   []ForkResourceIssue `json:"issues,omitempty"`
	Reason   string              `json:"reason,omitempty"`
}

// ProcessInfo is stable branch-local process information returned by ttos top.
type ProcessInfo struct {
	UID     uint32  `json:"uid"`
	PID     int32   `json:"pid"`
	PPID    int32   `json:"ppid"`
	PGID    int32   `json:"pgid"`
	Threads []int32 `json:"threads"`
	C       int32   `json:"c"`
	TTY     string  `json:"tty"`
	STime   string  `json:"stime"`
	Time    string  `json:"time"`
	Cmd     string  `json:"cmd"`
}

type backend interface {
	pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error)
	create(request runRequest) error
	run(request runRequest) (int, error)
	start(name string, detach, interactive bool) (int, error)
	attach(target selector, interactive bool, detachKeys []byte) (uint32, error)
	logs(target selector, follow bool, tail int) error
	exec(request execRequest) (int, error)
	inspect(name string) (InspectInfo, error)
	remove(name string, force bool) error
	listFamilies() ([]Family, error)
	processes(target selector) ([]ProcessInfo, error)
	fork(source selector, destination string, paused bool) (Branch, error)
	pause(target selector) error
	checkFork(target selector) (ForkEligibility, error)
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
		Reference:          request.reference,
		Platform:           request.platform,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
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
		Reference:          request.reference,
		Platform:           request.platform,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
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
	maxFiles  int
	pending   []byte
	stdoutEOF bool
	stderrEOF bool
}

type consoleLogGeneration struct {
	generation int
	path       string
	info       os.FileInfo
}

func listConsoleLogGenerations(path string, maxFiles int) ([]consoleLogGeneration, error) {
	var generations []consoleLogGeneration
	for generation := maxFiles - 1; generation >= 0; generation-- {
		generationPath := ttosconsole.GenerationPath(path, generation)
		info, err := os.Stat(generationPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stating console log generation %d: %w", generation, err)
		}
		generations = append(generations, consoleLogGeneration{
			generation: generation,
			path:       generationPath,
			info:       info,
		})
	}
	return generations, nil
}

func openConsoleLogFollower(path string, maxFilesValues ...int) (*consoleLogFollower, error) {
	maxFiles := defaultConsoleLogFiles
	if len(maxFilesValues) != 0 {
		maxFiles = maxFilesValues[0]
	}
	if maxFiles < 1 {
		return nil, fmt.Errorf("console log max files must be at least 1, got %d", maxFiles)
	}
	for attempts := 0; attempts < 4; attempts++ {
		generations, err := listConsoleLogGenerations(path, maxFiles)
		if err != nil {
			return nil, err
		}
		if len(generations) == 0 {
			return nil, fmt.Errorf("opening console log: %w", os.ErrNotExist)
		}
		oldest := generations[0]
		file, err := os.Open(oldest.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("opening console log generation %d: %w", oldest.generation, err)
		}
		info, err := file.Stat()
		if err == nil && os.SameFile(info, oldest.info) {
			return &consoleLogFollower{file: file, path: path, maxFiles: maxFiles}, nil
		}
		file.Close()
	}
	return nil, errors.New("console logs rotated repeatedly while opening the oldest generation")
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
	for attempts := 0; attempts < 4; attempts++ {
		openInfo, err := f.file.Stat()
		if err != nil {
			return false, fmt.Errorf("stating open console log: %w", err)
		}
		generations, err := listConsoleLogGenerations(f.path, f.maxFiles)
		if err != nil {
			return false, err
		}
		if len(generations) == 0 {
			return false, nil
		}
		next := -1
		for index := range generations {
			if os.SameFile(openInfo, generations[index].info) {
				if index+1 < len(generations) {
					next = index + 1
				}
				break
			}
		}
		if next < 0 {
			active := generations[len(generations)-1]
			if os.SameFile(openInfo, active.info) {
				return false, nil
			}
			return false, errors.New("console log reader fell behind retention; an unread generation was evicted")
		}
		if len(f.pending) != 0 {
			return false, fmt.Errorf("console log rotated with %d bytes of an incomplete frame", len(f.pending))
		}
		candidate := generations[next]
		file, err := os.Open(candidate.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("opening console log generation %d: %w", candidate.generation, err)
		}
		candidateInfo, err := file.Stat()
		if err != nil || !os.SameFile(candidateInfo, candidate.info) {
			file.Close()
			continue
		}
		if err := f.file.Close(); err != nil {
			file.Close()
			return false, fmt.Errorf("closing previous console log generation: %w", err)
		}
		f.file = file
		return true, nil
	}
	return false, errors.New("console logs rotated repeatedly while advancing generations")
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

func acquireConsoleStdin(lockPath string, target selector) (*os.File, error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening stdin ownership lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("stdin for %s is already attached", target)
		}
		return nil, fmt.Errorf("locking stdin for %s: %w", target, err)
	}
	return lockFile, nil
}

func (b *runtimeBackend) attach(target selector, interactive bool, detachKeys []byte) (status uint32, retErr error) {
	paths, err := b.ensureConsoleDrain(target, false)
	if err != nil {
		return 0, err
	}
	_, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return 0, err
	}
	if interactive {
		inputLock, err := acquireConsoleStdin(paths.StdinLock, target)
		if err != nil {
			return 0, err
		}
		defer func() {
			if err := inputLock.Close(); retErr == nil && err != nil {
				retErr = fmt.Errorf("releasing stdin ownership for %s: %w", target, err)
			}
		}()
		restore, err := makeRawTerminal(b.stdin)
		if err != nil {
			return 0, err
		}
		if restore != nil {
			defer func() {
				if err := restore(); retErr == nil && err != nil {
					retErr = fmt.Errorf("restoring attached stdin terminal: %w", err)
				}
			}()
		}
	}
	follower, err := openConsoleLogFollower(paths.Log, maxFiles)
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
		go func() { inputErrCh <- b.pumpConsoleInput(target, detachKeys) }()
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
			if errors.Is(err, errDetached) {
				return 0, errDetached
			}
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

type consoleLogLine struct {
	stream byte
	data   []byte
}

// consoleLogTail retains a globally ordered tail while preserving each line's
// stdout/stderr destination. Partial lines count as one line at snapshot time.
type consoleLogTail struct {
	limit int
	lines []consoleLogLine
	next  int
	seq   uint64

	stdoutPartial []byte
	stdoutSeq     uint64
	stderrPartial []byte
	stderrSeq     uint64
}

type consoleLogTailWriter struct {
	tail   *consoleLogTail
	stream byte
}

func (w consoleLogTailWriter) Write(data []byte) (int, error) {
	w.tail.write(w.stream, data)
	return len(data), nil
}

func (t *consoleLogTail) writer(stream byte) io.Writer {
	return consoleLogTailWriter{tail: t, stream: stream}
}

func (t *consoleLogTail) write(stream byte, data []byte) {
	if t.limit == 0 || len(data) == 0 {
		return
	}
	var partial *[]byte
	var partialSeq *uint64
	switch stream {
	case ttosconsole.StreamStdout:
		partial = &t.stdoutPartial
		partialSeq = &t.stdoutSeq
	case ttosconsole.StreamStderr:
		partial = &t.stderrPartial
		partialSeq = &t.stderrSeq
	default:
		return
	}
	for len(data) != 0 {
		t.seq++
		*partialSeq = t.seq
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			*partial = append(*partial, data...)
			return
		}
		line := make([]byte, 0, len(*partial)+newline+1)
		line = append(line, *partial...)
		line = append(line, data[:newline+1]...)
		*partial = nil
		t.push(consoleLogLine{stream: stream, data: line})
		data = data[newline+1:]
	}
}

func (t *consoleLogTail) push(line consoleLogLine) {
	if t.limit == 0 {
		return
	}
	if len(t.lines) < t.limit {
		t.lines = append(t.lines, line)
		return
	}
	t.lines[t.next] = line
	t.next = (t.next + 1) % t.limit
}

func (t *consoleLogTail) pushPartials() {
	if len(t.stdoutPartial) != 0 && (len(t.stderrPartial) == 0 || t.stdoutSeq <= t.stderrSeq) {
		t.push(consoleLogLine{stream: ttosconsole.StreamStdout, data: t.stdoutPartial})
		t.stdoutPartial = nil
	}
	if len(t.stderrPartial) != 0 {
		t.push(consoleLogLine{stream: ttosconsole.StreamStderr, data: t.stderrPartial})
		t.stderrPartial = nil
	}
	if len(t.stdoutPartial) != 0 {
		t.push(consoleLogLine{stream: ttosconsole.StreamStdout, data: t.stdoutPartial})
		t.stdoutPartial = nil
	}
}

func (t *consoleLogTail) flush(stdout, stderr io.Writer) error {
	t.pushPartials()
	start := 0
	if len(t.lines) == t.limit {
		start = t.next
	}
	for i := 0; i < len(t.lines); i++ {
		line := t.lines[(start+i)%len(t.lines)]
		output := stdout
		if line.stream == ttosconsole.StreamStderr {
			output = stderr
		}
		if output == nil {
			continue
		}
		n, err := output.Write(line.data)
		if err != nil {
			return fmt.Errorf("writing tailed stream %d: %w", line.stream, err)
		}
		if n != len(line.data) {
			return fmt.Errorf("writing tailed stream %d: %w", line.stream, io.ErrShortWrite)
		}
	}
	return nil
}

func (b *runtimeBackend) logs(target selector, follow bool, tail int) error {
	paths, err := b.ensureConsoleDrain(target, false)
	if err != nil {
		return err
	}
	_, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return err
	}
	follower, err := openConsoleLogFollower(paths.Log, maxFiles)
	if err != nil {
		return err
	}
	defer follower.file.Close()
	initial := true
	for {
		var progress bool
		if initial && tail >= 0 {
			collector := &consoleLogTail{limit: tail}
			progress, err = follower.drain(
				collector.writer(ttosconsole.StreamStdout),
				collector.writer(ttosconsole.StreamStderr))
			if err == nil {
				err = collector.flush(b.stdout, b.stderr)
			}
		} else {
			progress, err = follower.drain(b.stdout, b.stderr)
		}
		if err != nil {
			return err
		}
		initial = false
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

func (b *runtimeBackend) consoleRetention(container string) (int64, int, error) {
	store := ttosbundle.Store{Root: b.dataRoot}
	info, err := store.LoadContainer(container)
	if err != nil {
		return 0, 0, fmt.Errorf("loading console retention for %s: %w", container, err)
	}
	maxBytes := info.ConsoleLogMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultConsoleLogMax
	}
	maxFiles := info.ConsoleLogMaxFiles
	if maxFiles < 1 {
		maxFiles = defaultConsoleLogFiles
	}
	return maxBytes, maxFiles, nil
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
	maxBytes, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return ttosbundle.ConsolePaths{}, err
	}
	if reset {
		for generation := 1; ; generation++ {
			err := os.Remove(ttosconsole.GenerationPath(paths.Log, generation))
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return ttosbundle.ConsolePaths{}, fmt.Errorf("removing console log generation %d: %w", generation, err)
			}
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
		"--console-log-max-bytes", strconv.FormatInt(maxBytes, 10),
		"--console-log-max-files", strconv.Itoa(maxFiles),
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

func (b *runtimeBackend) writeConsoleAll(target selector, data []byte) error {
	for written := 0; written < len(data); {
		result, err := b.writeConsole(target, data[written:])
		if err != nil {
			return err
		}
		written += result.N
		if result.WouldBlock || result.N == 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return nil
}

func (b *runtimeBackend) closeConsoleStdin(target selector) error {
	_, err := b.command("timetravel", "--console", "init", target.container, "console-close-stdin", target.branch)
	return err
}

func (b *runtimeBackend) pumpConsoleInput(target selector, detachKeys []byte) error {
	if b.stdin == nil {
		return b.closeConsoleStdin(target)
	}
	filter := detachKeyFilter{sequence: append([]byte(nil), detachKeys...)}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := b.stdin.Read(buf)
		data, detached := filter.push(buf[:n])
		if err := b.writeConsoleAll(target, data); err != nil {
			return err
		}
		if detached {
			return errDetached
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := b.writeConsoleAll(target, filter.flush()); err != nil {
					return err
				}
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
	status, err := b.attach(target, interactive, defaultDetachKeys)
	if err != nil {
		if errors.Is(err, errDetached) {
			return 0, nil
		}
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

func (b *runtimeBackend) processes(target selector) ([]ProcessInfo, error) {
	output, err := b.command("timetravel", target.container, "ps", target.branch)
	if err != nil {
		return nil, err
	}
	var processes []ProcessInfo
	if err := json.Unmarshal(output, &processes); err != nil {
		return nil, fmt.Errorf("decoding branch process list: %w", err)
	}
	return processes, nil
}

func (b *runtimeBackend) checkFork(target selector) (ForkEligibility, error) {
	output, err := b.command("timetravel", target.container, "check", target.branch)
	if err != nil {
		return ForkEligibility{}, err
	}
	var result ForkEligibility
	if err := json.Unmarshal(output, &result); err != nil {
		return ForkEligibility{}, fmt.Errorf("decoding fork eligibility: %w", err)
	}
	return result, nil
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
