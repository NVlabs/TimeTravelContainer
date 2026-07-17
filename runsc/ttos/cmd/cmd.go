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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"text/template"
	"time"

	mobysignal "github.com/moby/sys/signal"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/term"
	"gvisor.dev/gvisor/pkg/abi/linux"

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
	root                  string
	runsc                 string
	containerd            string
	namespace             string
	dataRoot              string
	unsafeNoHostIsolation bool
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
	if opts.unsafeNoHostIsolation {
		fmt.Fprintln(stderr, "ttos: warning: --unsafe-no-host-isolation disables host namespaces and chroots; run trusted workloads only")
	}
	return runCommand(args, stdout, stderr, &runtimeBackend{
		root:                  opts.root,
		runsc:                 opts.runsc,
		containerd:            opts.containerd,
		namespace:             opts.namespace,
		dataRoot:              opts.dataRoot,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
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
		case arg == "--unsafe-no-host-isolation":
			opts.unsafeNoHostIsolation = true
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
  --unsafe-no-host-isolation     run without host namespaces/chroots (unsafe)

Commands:
  images [OPTIONS]               List local images
  image COMMAND                  Manage local images (ls, inspect, rm)
  container COMMAND              Docker-style container command aliases
  rmi [OPTIONS] IMAGE...         Remove one or more local images
  pull [OPTIONS] IMAGE           Pull an OCI/Docker image into local content
  run [OPTIONS] IMAGE [COMMAND]  Pull, create, and run a container
  create [OPTIONS] IMAGE [CMD]   Prepare a container without starting it
  start [OPTIONS] CONTAINER      Start a prepared or exited container
  attach [OPTIONS] TARGET        Attach to a branch init or managed exec console
  logs [OPTIONS] TARGET          Print or follow init or managed exec logs
  exec [OPTIONS] TARGET COMMAND  Execute a command in a running branch
  exec ls|wait|kill ...          Manage detached exec sessions
  inspect [OPTIONS] CONTAINER... Show Docker and TTOS branch/process state
  wait [OPTIONS] TARGET...       Wait for branches and print exit codes
  restart [OPTIONS] CONTAINER... Restart one or more container families
  rm [--force] CONTAINER...      Remove container metadata and runtime state
  ls [--tree|--json]              List container families and branch trees
  ps [OPTIONS]                    List and filter container branches
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
	case "container":
		err = runContainerNamespace(args[1:], stdout, stderr, backend)
	case "images":
		err = runImages(args[1:], stdout, stderr, backend)
	case "image":
		err = runImage(args[1:], stdout, stderr, backend)
	case "rmi":
		err = runImageRemove(args[1:], stdout, stderr, backend)
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
		err = runExec(args[1:], stdout, stderr, backend)
	case "inspect":
		err = runInspect(args[1:], stdout, stderr, backend)
	case "rm":
		err = runRemove(args[1:], stdout, stderr, backend)
	case "_auto-remove":
		err = runAutoRemove(args[1:], backend)
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
	case "wait":
		err = runWait(args[1:], stdout, stderr, backend)
	case "restart":
		err = runRestart(args[1:], stdout, stderr, backend)
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

func runContainerNamespace(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttos container COMMAND [OPTIONS]")
	}
	switch args[0] {
	case "run":
		return runContainer(args[1:], stdout, stderr, backend)
	case "create":
		return runCreate(args[1:], stdout, stderr, backend)
	case "start":
		return runStart(args[1:], stdout, stderr, backend)
	case "attach":
		return runAttach(args[1:], stderr, backend)
	case "logs":
		return runLogs(args[1:], stderr, backend)
	case "exec":
		return runExec(args[1:], stdout, stderr, backend)
	case "inspect":
		return runInspect(args[1:], stdout, stderr, backend)
	case "rm":
		return runRemove(args[1:], stdout, stderr, backend)
	case "ls", "ps":
		return runPs(args[1:], stdout, stderr, backend)
	case "top":
		return runTop(args[1:], stdout, stderr, backend)
	case "pause":
		return runPauseResume(args[1:], stdout, backend, true)
	case "unpause":
		return runPauseResume(args[1:], stdout, backend, false)
	case "kill":
		return runKill(args[1:], stdout, stderr, backend)
	case "stop":
		return runStop(args[1:], stdout, stderr, backend)
	case "wait":
		return runWait(args[1:], stdout, stderr, backend)
	case "restart":
		return runRestart(args[1:], stdout, stderr, backend)
	default:
		return fmt.Errorf("unknown container command %q", args[0])
	}
}

type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprintf("%q", *s) }
func (s *stringSlice) Get() any       { return s }
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func readKeyValueFiles(paths []string, kind string) ([]string, error) {
	var values []string
	for _, name := range paths {
		file, err := os.Open(name)
		if err != nil {
			return nil, fmt.Errorf("opening %s file %q: %w", kind, name, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if lineNumber == 1 {
				line = strings.TrimPrefix(line, "\ufeff")
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			values = append(values, line)
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("reading %s file %q: %w", kind, name, scanErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing %s file %q: %w", kind, name, closeErr)
		}
	}
	return values, nil
}

func collectEnvironment(fileNames, direct []string) ([]string, error) {
	values, err := readKeyValueFiles(fileNames, "environment")
	if err != nil {
		return nil, err
	}
	values = append(values, direct...)
	for i, value := range values {
		name, _, hasValue := strings.Cut(value, "=")
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("invalid environment variable %q", value)
		}
		if !hasValue {
			if hostValue, ok := os.LookupEnv(name); ok {
				values[i] = name + "=" + hostValue
			}
		}
	}
	return values, nil
}

func collectLabels(fileNames, direct []string) (map[string]string, error) {
	values, err := readKeyValueFiles(fileNames, "label")
	if err != nil {
		return nil, err
	}
	values = append(values, direct...)
	labels := make(map[string]string, len(values))
	for _, value := range values {
		name, labelValue, hasValue := strings.Cut(value, "=")
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("invalid label %q", value)
		}
		if !hasValue {
			labelValue = ""
		}
		labels[name] = labelValue
	}
	return labels, nil
}

// expandCombinedShortBools implements Docker-style boolean clusters such as
// -it and -dit while preserving everything after the image/container operand.
func expandCombinedShortBools(args []string, boolNames string, valueNames ...string) []string {
	bools := make(map[byte]bool, len(boolNames))
	for i := range boolNames {
		bools[boolNames[i]] = true
	}
	values := make(map[string]bool, len(valueNames))
	for _, name := range valueNames {
		values[name] = true
	}
	expanded := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			expanded = append(expanded, args[i:]...)
			break
		}
		if values[arg] {
			expanded = append(expanded, arg)
			if i+1 < len(args) {
				i++
				expanded = append(expanded, args[i])
			}
			continue
		}
		if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
			cluster := arg[1:]
			valid := true
			for j := range cluster {
				if !bools[cluster[j]] {
					valid = false
					break
				}
			}
			if valid {
				for j := range cluster {
					expanded = append(expanded, "-"+string(cluster[j]))
				}
				continue
			}
		}
		expanded = append(expanded, arg)
	}
	return expanded
}

type mountRequests struct {
	host       []ttosbundle.HostMount
	managed    []ttosbundle.ManagedVolume
	filesystem []ttosbundle.FilesystemMount
}

func parseMountRequests(volumeSpecs, mountSpecs, tmpfsSpecs []string) (mountRequests, error) {
	var request mountRequests
	var hostVolumes, hostMounts []string
	for _, value := range tmpfsSpecs {
		mount, err := parseTmpfsMount(value)
		if err != nil {
			return mountRequests{}, err
		}
		request.filesystem = append(request.filesystem, mount)
	}
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
				mountType = strings.ToLower(strings.TrimSpace(raw))
				break
			}
		}
		switch mountType {
		case "volume":
			volume, err := parseManagedLongMount(value)
			if err != nil {
				return mountRequests{}, err
			}
			request.managed = append(request.managed, volume)
		case "tmpfs", "proc", "procfs", "sysfs":
			mount, err := parseFilesystemLongMount(value)
			if err != nil {
				return mountRequests{}, err
			}
			request.filesystem = append(request.filesystem, mount)
		default:
			hostMounts = append(hostMounts, value)
		}
	}
	var err error
	request.host, err = parseHostMounts(hostVolumes, hostMounts)
	if err != nil {
		return mountRequests{}, err
	}
	return request, nil
}

func parseTmpfsMount(value string) (ttosbundle.FilesystemMount, error) {
	destination, rawOptions, hasOptions := strings.Cut(value, ":")
	if destination == "" || (hasOptions && rawOptions == "") {
		return ttosbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs specification %q; want CONTAINER[:OPTIONS]", value)
	}
	readonly := false
	options := []string{}
	if hasOptions {
		for _, option := range strings.Split(rawOptions, ",") {
			option = strings.TrimSpace(option)
			key, raw, found := strings.Cut(option, "=")
			if found {
				if err := validateTmpfsDataOption(key, raw); err != nil {
					return ttosbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs option %q in %q: %w", option, value, err)
				}
				options = append(options, option)
				continue
			}
			switch key {
			case "ro":
				readonly = true
			case "rw":
				readonly = false
			case "nosuid", "suid", "nodev", "dev", "exec", "noexec", "sync", "async", "dirsync", "atime", "noatime", "diratime", "nodiratime":
				options = append(options, key)
			default:
				return ttosbundle.FilesystemMount{}, fmt.Errorf("unsupported tmpfs option %q in %q", option, value)
			}
		}
	}
	mount, err := normalizeFilesystemMount("tmpfs", destination, readonly, options)
	if err != nil {
		return ttosbundle.FilesystemMount{}, err
	}
	mount.HostConfig = true
	mount.InspectOptions = rawOptions
	return mount, nil
}

func validateTmpfsDataOption(key, value string) error {
	switch key {
	case "size":
		return validateTmpfsAmount(value, true)
	case "nr_inodes", "nr_blocks":
		return validateTmpfsAmount(value, false)
	case "mode":
		_, err := normalizeTmpfsMode(value)
		return err
	case "uid", "gid":
		if _, err := strconv.ParseUint(value, 10, 32); err != nil {
			return fmt.Errorf("%s must be an unsigned 32-bit integer", key)
		}
		return nil
	default:
		return fmt.Errorf("unsupported data option %q", key)
	}
}

func validateTmpfsAmount(value string, allowPercent bool) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	number := value
	multiplier := uint64(1)
	suffix := value[len(value)-1]
	switch strings.ToLower(string(suffix)) {
	case "k":
		multiplier = 1 << 10
	case "m":
		multiplier = 1 << 20
	case "g":
		multiplier = 1 << 30
	case "t":
		multiplier = 1 << 40
	case "p":
		multiplier = 1 << 50
	case "e":
		multiplier = 1 << 60
	case "%":
		if !allowPercent {
			return errors.New("percentages are supported only for size")
		}
		multiplier = 0
	default:
		suffix = 0
	}
	if suffix != 0 {
		number = value[:len(value)-1]
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return errors.New("value must be an unsigned integer with an optional k, m, g, t, p, e, or % suffix")
	}
	if multiplier != 0 && parsed > math.MaxUint64/multiplier {
		return errors.New("value overflows uint64")
	}
	return nil
}

func normalizeTmpfsMode(value string) (string, error) {
	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0o"), 8, 16)
	if err != nil || parsed > 07777 {
		return "", fmt.Errorf("mode %q must be octal between 0000 and 7777", value)
	}
	return fmt.Sprintf("%04o", parsed), nil
}

func parseFilesystemLongMount(value string) (ttosbundle.FilesystemMount, error) {
	mountType := ""
	destination := ""
	readonly := false
	readonlySet := false
	size := ""
	mode := ""
	seen := make(map[string]struct{})
	for _, field := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if !found {
			switch key {
			case "ro", "readonly":
				if _, ok := seen["readonly"]; ok {
					return ttosbundle.FilesystemMount{}, fmt.Errorf("duplicate --mount field %q in %q", key, value)
				}
				seen["readonly"] = struct{}{}
				readonly = true
				readonlySet = true
				continue
			default:
				return ttosbundle.FilesystemMount{}, fmt.Errorf("invalid --mount field %q in %q", field, value)
			}
		}
		raw = strings.TrimSpace(raw)
		canonical := key
		switch key {
		case "dst", "target":
			canonical = "destination"
		case "ro":
			canonical = "readonly"
		case "src":
			canonical = "source"
		}
		if _, ok := seen[canonical]; ok {
			return ttosbundle.FilesystemMount{}, fmt.Errorf("duplicate --mount field %q in %q", key, value)
		}
		seen[canonical] = struct{}{}
		switch canonical {
		case "type":
			mountType = strings.ToLower(raw)
		case "destination":
			destination = raw
		case "readonly":
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return ttosbundle.FilesystemMount{}, fmt.Errorf("invalid readonly value %q in %q", raw, value)
			}
			readonly = parsed
			readonlySet = true
		case "tmpfs-size":
			size = raw
		case "tmpfs-mode":
			mode = raw
		case "source":
			return ttosbundle.FilesystemMount{}, fmt.Errorf("filesystem mount %q must not specify a source", value)
		default:
			return ttosbundle.FilesystemMount{}, fmt.Errorf("unsupported filesystem-mount field %q in %q", key, value)
		}
	}
	if mountType == "procfs" {
		mountType = "proc"
	}
	if mountType == "sysfs" && !readonlySet {
		readonly = true
	}
	options := []string{}
	if mountType == "tmpfs" {
		if size != "" {
			parsed, err := strconv.ParseUint(size, 10, 64)
			if err != nil {
				return ttosbundle.FilesystemMount{}, fmt.Errorf("tmpfs-size %q in %q must be an unsigned byte count", size, value)
			}
			options = append(options, "size="+strconv.FormatUint(parsed, 10))
		}
		if mode != "" {
			normalized, err := normalizeTmpfsMode(mode)
			if err != nil {
				return ttosbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs-mode in %q: %w", value, err)
			}
			options = append(options, "mode="+normalized)
		}
	} else if size != "" || mode != "" {
		return ttosbundle.FilesystemMount{}, fmt.Errorf("%s mount %q does not support tmpfs options", mountType, value)
	} else {
		options = []string{"nosuid", "nodev", "noexec"}
	}
	return normalizeFilesystemMount(mountType, destination, readonly, options)
}

func normalizeFilesystemMount(mountType, destination string, readonly bool, options []string) (ttosbundle.FilesystemMount, error) {
	if mountType != "tmpfs" && mountType != "proc" && mountType != "sysfs" {
		return ttosbundle.FilesystemMount{}, fmt.Errorf("unsupported filesystem mount type %q", mountType)
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == "/" {
		return ttosbundle.FilesystemMount{}, fmt.Errorf("%s mount destination %q must be an absolute non-root path", mountType, destination)
	}
	access := "rw"
	if readonly {
		access = "ro"
	}
	return ttosbundle.FilesystemMount{
		Type: mountType, Destination: filepath.Clean(destination), Readonly: readonly,
		Options: append([]string{access}, options...),
	}, nil
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

func parseNetworkMode(value string) (string, error) {
	switch value {
	case "none", "host":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q; supported modes are none and host", value)
	}
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
	pullPolicy         string
	args               []string
	env                []string
	labels             map[string]string
	cwd                string
	user               string
	hostname           string
	networkMode        string
	entrypoint         []string
	hostMounts         []ttosbundle.HostMount
	filesystemMounts   []ttosbundle.FilesystemMount
	consoleLogMaxBytes int64
	consoleLogMaxFiles int
	managedVolumes     []ttosbundle.ManagedVolume
	autoRemove         bool
	readonlyRootfs     bool
	stopSignal         string
	stopTimeout        int
	pidsLimit          *int64
	ulimits            []ttosbundle.Ulimit
	shmSize            int64
	detach             bool
	interactive        bool
	terminal           bool
}

type attachRequest struct {
	target     selector
	execID     string
	stdin      bool
	stdout     bool
	stderr     bool
	detachKeys []byte
}

type execRequest struct {
	detach         bool
	execID         string
	target         selector
	args           []string
	env            []string
	cwd            string
	user           string
	additionalGIDs []string
	capabilities   []string
	processPath    string
	interactive    bool
	terminal       bool
}

type containerRunResult struct {
	ID     string
	Status int
}

type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("container exited with status %d", e.code) }

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func runContainer(args []string, stdout, stderr io.Writer, backend backend) error {
	args = expandCombinedShortBools(args, "dit", "--name", "--platform", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "--network", "--net", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--shm-size", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-opt")
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	pullPolicy := fs.String("pull", "missing", "pull policy: missing, always, or never")
	detach := fs.Bool("detach", false, "run in the background")
	fs.BoolVar(detach, "d", false, "run in the background")
	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "keep stdin open")
	terminal := fs.Bool("tty", false, "allocate a terminal")
	fs.BoolVar(terminal, "t", false, "allocate a terminal")
	autoRemove := fs.Bool("rm", false, "automatically remove the container when it exits")
	readonlyRootfs := fs.Bool("read-only", false, "mount the container root filesystem as read only")
	stopSignal := fs.String("stop-signal", "", "signal to stop the container (defaults to image STOPSIGNAL or SIGTERM)")
	stopTimeout := fs.Int("stop-timeout", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait before killing the container; -1 waits forever")
	pidsLimit := fs.Int64("pids-limit", 0, "tune the container task limit; -1 or 0 means unlimited")
	shmSizeValue := fs.String("shm-size", "64m", "size of /dev/shm")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	networkModeValue := fs.String("network", "none", "network mode: none or host")
	fs.StringVar(networkModeValue, "net", "none", "network mode: none or host")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env, envFiles, labels, labelFiles stringSlice
	fs.Var(&env, "env", "set or import environment variable NAME[=VALUE]")
	fs.Var(&env, "e", "set or import environment variable NAME[=VALUE]")
	fs.Var(&envFiles, "env-file", "read environment variables from a file")
	fs.Var(&labels, "label", "set container metadata KEY[=VALUE]")
	fs.Var(&labels, "l", "set container metadata KEY[=VALUE]")
	fs.Var(&labelFiles, "label-file", "read container labels from a file")
	var volumes, mounts, tmpfsMounts stringSlice
	fs.Var(&volumes, "volume", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&volumes, "v", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&mounts, "mount", "attach type=volume|bind|tmpfs|proc|sysfs,target=CONTAINER[,OPTIONS]")
	fs.Var(&tmpfsMounts, "tmpfs", "mount tmpfs at CONTAINER[:OPTIONS]")
	var logOptions, ulimitValues stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option max-size=SIZE or max-file=COUNT")
	fs.Var(&ulimitValues, "ulimit", "set resource limit NAME=SOFT[:HARD]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttos run [OPTIONS] IMAGE [COMMAND...]")
	}
	if *stopTimeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	if *pidsLimit < -1 {
		return errors.New("pids limit must be -1 or greater")
	}
	if flagWasSet(fs, "stop-signal") {
		if _, err := parseSignal(*stopSignal); err != nil {
			return err
		}
	}
	envValues, err := collectEnvironment(envFiles, env)
	if err != nil {
		return err
	}
	labelValues, err := collectLabels(labelFiles, labels)
	if err != nil {
		return err
	}
	networkMode, err := parseNetworkMode(*networkModeValue)
	if err != nil {
		return err
	}
	if *pullPolicy != "missing" && *pullPolicy != "always" && *pullPolicy != "never" {
		return fmt.Errorf("unsupported pull policy %q; supported policies are missing, always, and never", *pullPolicy)
	}
	parsedMounts, err := parseMountRequests(volumes, mounts, tmpfsMounts)
	if err != nil {
		return err
	}
	logMaxBytes, logMaxFiles, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	parsedUlimits, err := ttosbundle.ParseUlimits(ulimitValues)
	if err != nil {
		return err
	}
	parsedShmSize, err := ttosbundle.ParseShmSize(*shmSizeValue)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	var effectivePidsLimit *int64
	if flagWasSet(fs, "pids-limit") {
		value := *pidsLimit
		effectivePidsLimit = &value
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		platform:           *platform,
		pullPolicy:         *pullPolicy,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                envValues,
		labels:             labelValues,
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		networkMode:        networkMode,
		hostMounts:         parsedMounts.host,
		filesystemMounts:   parsedMounts.filesystem,
		managedVolumes:     parsedMounts.managed,
		autoRemove:         *autoRemove,
		readonlyRootfs:     *readonlyRootfs,
		stopSignal:         *stopSignal,
		stopTimeout:        *stopTimeout,
		pidsLimit:          effectivePidsLimit,
		ulimits:            parsedUlimits,
		shmSize:            parsedShmSize,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
		detach:             *detach,
		interactive:        *interactive,
		terminal:           *terminal,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "entrypoint" {
			request.entrypoint = []string{*entrypoint}
			if *entrypoint == "" {
				request.entrypoint = []string{}
			}
		}
	})
	result, err := backend.run(request)
	if err != nil {
		return err
	}
	if result.Status != 0 {
		return exitStatus{code: result.Status}
	}
	if request.detach {
		fmt.Fprintln(stdout, result.ID)
	}
	return nil
}

func runCreate(args []string, stdout, stderr io.Writer, backend backend) error {
	args = expandCombinedShortBools(args, "it", "--name", "--platform", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "--network", "--net", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--shm-size", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-opt")
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	pullPolicy := fs.String("pull", "missing", "pull policy: missing, always, or never")
	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "keep stdin open")
	terminal := fs.Bool("tty", false, "allocate a terminal")
	fs.BoolVar(terminal, "t", false, "allocate a terminal")
	autoRemove := fs.Bool("rm", false, "automatically remove the container when it exits")
	readonlyRootfs := fs.Bool("read-only", false, "mount the container root filesystem as read only")
	stopSignal := fs.String("stop-signal", "", "signal to stop the container (defaults to image STOPSIGNAL or SIGTERM)")
	stopTimeout := fs.Int("stop-timeout", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait before killing the container; -1 waits forever")
	pidsLimit := fs.Int64("pids-limit", 0, "tune the container task limit; -1 or 0 means unlimited")
	shmSizeValue := fs.String("shm-size", "64m", "size of /dev/shm")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	networkModeValue := fs.String("network", "none", "network mode: none or host")
	fs.StringVar(networkModeValue, "net", "none", "network mode: none or host")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env, envFiles, labels, labelFiles stringSlice
	fs.Var(&env, "env", "set or import environment variable NAME[=VALUE]")
	fs.Var(&env, "e", "set or import environment variable NAME[=VALUE]")
	fs.Var(&envFiles, "env-file", "read environment variables from a file")
	fs.Var(&labels, "label", "set container metadata KEY[=VALUE]")
	fs.Var(&labels, "l", "set container metadata KEY[=VALUE]")
	fs.Var(&labelFiles, "label-file", "read container labels from a file")
	var volumes, mounts, tmpfsMounts stringSlice
	fs.Var(&volumes, "volume", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&volumes, "v", "attach NAME:CONTAINER[:ro] or HOST:CONTAINER:ro")
	fs.Var(&mounts, "mount", "attach type=volume|bind|tmpfs|proc|sysfs,target=CONTAINER[,OPTIONS]")
	fs.Var(&tmpfsMounts, "tmpfs", "mount tmpfs at CONTAINER[:OPTIONS]")
	var logOptions, ulimitValues stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option max-size=SIZE or max-file=COUNT")
	fs.Var(&ulimitValues, "ulimit", "set resource limit NAME=SOFT[:HARD]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttos create [OPTIONS] IMAGE [COMMAND...]")
	}
	if *stopTimeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	if *pidsLimit < -1 {
		return errors.New("pids limit must be -1 or greater")
	}
	if flagWasSet(fs, "stop-signal") {
		if _, err := parseSignal(*stopSignal); err != nil {
			return err
		}
	}
	envValues, err := collectEnvironment(envFiles, env)
	if err != nil {
		return err
	}
	labelValues, err := collectLabels(labelFiles, labels)
	if err != nil {
		return err
	}
	networkMode, err := parseNetworkMode(*networkModeValue)
	if err != nil {
		return err
	}
	if *pullPolicy != "missing" && *pullPolicy != "always" && *pullPolicy != "never" {
		return fmt.Errorf("unsupported pull policy %q; supported policies are missing, always, and never", *pullPolicy)
	}
	parsedMounts, err := parseMountRequests(volumes, mounts, tmpfsMounts)
	if err != nil {
		return err
	}
	logMaxBytes, logMaxFiles, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	parsedUlimits, err := ttosbundle.ParseUlimits(ulimitValues)
	if err != nil {
		return err
	}
	parsedShmSize, err := ttosbundle.ParseShmSize(*shmSizeValue)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttos-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	var effectivePidsLimit *int64
	if flagWasSet(fs, "pids-limit") {
		value := *pidsLimit
		effectivePidsLimit = &value
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		platform:           *platform,
		pullPolicy:         *pullPolicy,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                envValues,
		labels:             labelValues,
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		networkMode:        networkMode,
		hostMounts:         parsedMounts.host,
		filesystemMounts:   parsedMounts.filesystem,
		managedVolumes:     parsedMounts.managed,
		autoRemove:         *autoRemove,
		readonlyRootfs:     *readonlyRootfs,
		stopSignal:         *stopSignal,
		stopTimeout:        *stopTimeout,
		pidsLimit:          effectivePidsLimit,
		ulimits:            parsedUlimits,
		shmSize:            parsedShmSize,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
		interactive:        *interactive,
		terminal:           *terminal,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "entrypoint" {
			request.entrypoint = []string{*entrypoint}
			if *entrypoint == "" {
				request.entrypoint = []string{}
			}
		}
	})
	id, err := backend.create(request)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, id)
	return nil
}

func runStart(args []string, stdout, stderr io.Writer, backend backend) error {
	args = expandCombinedShortBools(args, "ai")
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	attach := fs.Bool("attach", false, "attach stdout and stderr and wait")
	fs.BoolVar(attach, "a", false, "attach stdout and stderr and wait")
	interactive := fs.Bool("interactive", false, "keep stdin open (requires --attach)")
	fs.BoolVar(interactive, "i", false, "keep stdin open (requires --attach)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos start [--attach] CONTAINER [CONTAINER...]")
	}
	if *interactive && !*attach {
		return errors.New("--interactive requires --attach")
	}
	if *attach && fs.NArg() != 1 {
		return errors.New("--attach supports exactly one container")
	}
	for _, name := range fs.Args() {
		if strings.Contains(name, "@") {
			return fmt.Errorf("start accepts container families, not branch selector %q", name)
		}
		status, err := backend.start(name, !*attach, *interactive)
		if err != nil {
			return err
		}
		if status != 0 {
			return exitStatus{code: status}
		}
		if !*attach {
			fmt.Fprintln(stdout, name)
		}
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

func hostTerminalSize(input io.Reader) (cols, rows int, ok bool, err error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return 0, 0, false, nil
	}
	cols, rows, err = term.GetSize(int(file.Fd()))
	if err != nil {
		return 0, 0, false, fmt.Errorf("reading attached terminal size: %w", err)
	}
	return cols, rows, true, nil
}

func runAttach(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interactive := fs.Bool("interactive", false, "forward stdin")
	fs.BoolVar(interactive, "i", false, "forward stdin")
	noStdin := fs.Bool("no-stdin", false, "do not forward stdin")
	noStdout := fs.Bool("no-stdout", false, "do not attach stdout")
	noStderr := fs.Bool("no-stderr", false, "do not attach stderr")
	execID := fs.String("exec", "", "attach to a managed exec session")
	detachKeysValue := fs.String("detach-keys", "ctrl-p,ctrl-q", "override the key sequence for detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos attach [-i|--no-stdin] [--no-stdout] [--no-stderr] [--exec EXEC_ID] [--detach-keys KEYS] CONTAINER[@BRANCH]")
	}
	if *interactive && *noStdin {
		return errors.New("--interactive and --no-stdin cannot be used together")
	}
	request := attachRequest{
		execID: *execID,
		stdin:  *interactive,
		stdout: !*noStdout,
		stderr: !*noStderr,
	}
	if !request.stdin && !request.stdout && !request.stderr {
		return errors.New("attach requires at least one of stdin, stdout, or stderr")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	request.target = target
	request.detachKeys, err = parseDetachKeys(*detachKeysValue)
	if err != nil {
		return err
	}
	if request.execID != "" {
		if _, err := parseExecSelector(target.String() + ":" + request.execID); err != nil {
			return err
		}
	}
	status, err := backend.attach(request)
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
	execID := fs.String("exec", "", "show logs for a managed exec session")
	tailValue := fs.String("tail", "all", "number of lines to show from the end of the logs")
	fs.StringVar(tailValue, "n", "all", "number of lines to show from the end of the logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos logs [-f] [--exec EXEC_ID] [--tail all|N] CONTAINER[@BRANCH]")
	}
	tail := -1
	if parsed, err := strconv.Atoi(*tailValue); err == nil && parsed >= 0 {
		tail = parsed
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	if *execID != "" {
		if _, err := parseExecSelector(target.String() + ":" + *execID); err != nil {
			return err
		}
	}
	if err := backend.logs(target, *execID, *follow, tail); err != nil {
		return fmt.Errorf("reading logs for %s: %w", target, err)
	}
	return nil
}

func runExec(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) != 0 {
		switch args[0] {
		case "ls":
			return runExecList(args[1:], stdout, stderr, backend)
		case "wait":
			return runExecWait(args[1:], stdout, stderr, backend)
		case "kill":
			return runExecKill(args[1:], stdout, stderr, backend)
		}
	}

	args = expandCombinedShortBools(args, "dit", "--workdir", "-w", "--user", "-u", "--env", "-e", "--additional-gids", "--cap", "--process")
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
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
	var env, additionalGIDs, capabilities stringSlice
	fs.Var(&env, "env", "set environment variable NAME=VALUE")
	fs.Var(&env, "e", "set environment variable NAME=VALUE")
	fs.Var(&additionalGIDs, "additional-gids", "add a supplementary numeric GID")
	fs.Var(&capabilities, "cap", "add a Linux capability to the process")
	processPath := fs.String("process", "", "OCI process JSON file (replaces command and process overrides)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	usingProcess := *processPath != ""
	if (!usingProcess && fs.NArg() < 2) || (usingProcess && fs.NArg() != 1) {
		return errors.New("usage: ttos exec [OPTIONS] CONTAINER[@BRANCH] COMMAND [ARG...] | ttos exec [OPTIONS] --process FILE CONTAINER[@BRANCH]")
	}
	if usingProcess && (len(env) != 0 || *cwd != "" || *user != "" || len(additionalGIDs) != 0 || len(capabilities) != 0 || *terminal) {
		return errors.New("--process cannot be combined with --env, --workdir, --user, --additional-gids, --cap, or --tty")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	resolvedProcessPath := ""
	processTerminal := false
	if usingProcess {
		resolvedProcessPath, processTerminal, err = loadExecProcess(*processPath)
		if err != nil {
			return err
		}
	}
	execID := ""
	if *detach {
		execID, err = newExecID()
		if err != nil {
			return err
		}
	}
	status, err := backend.exec(execRequest{
		detach:         *detach,
		execID:         execID,
		target:         target,
		args:           append([]string(nil), fs.Args()[1:]...),
		env:            append([]string(nil), env...),
		cwd:            *cwd,
		user:           *user,
		additionalGIDs: append([]string(nil), additionalGIDs...),
		capabilities:   append([]string(nil), capabilities...),
		processPath:    resolvedProcessPath,
		interactive:    *interactive,
		terminal:       *terminal || processTerminal,
	})
	if err != nil {
		return err
	}
	if status != 0 {
		return exitStatus{code: status}
	}
	if *detach {
		fmt.Fprintf(stdout, "%s@%s:%s\n", target.container, target.branch, execID)
	}
	return nil
}

func loadExecProcess(name string) (string, bool, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", false, fmt.Errorf("resolving process file %q: %w", name, err)
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", false, fmt.Errorf("opening process file %q: %w", name, err)
	}
	defer file.Close()
	var process specs.Process
	if err := json.NewDecoder(file).Decode(&process); err != nil {
		return "", false, fmt.Errorf("decoding process file %q: %w", name, err)
	}
	if process.Cwd == "" || !filepath.IsAbs(process.Cwd) {
		return "", false, fmt.Errorf("process file %q has non-absolute cwd %q", name, process.Cwd)
	}
	if len(process.Args) == 0 {
		return "", false, fmt.Errorf("process file %q has no arguments", name)
	}
	return abs, process.Terminal, nil
}

func runExecList(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("exec ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos exec ls [--json] CONTAINER[@BRANCH]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	sessions, err := backend.execSessions(target)
	if err != nil {
		return fmt.Errorf("listing exec sessions in %s: %w", target, err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(sessions)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID	PID	STATE	TTY	CREATED")
	for _, session := range sessions {
		fmt.Fprintf(table, "%s\t%d\t%s\t%t\t%s\n", session.ID, session.PID, session.State, session.Terminal, session.CreatedAt.Local().Format(time.RFC3339))
	}
	return table.Flush()
}

func runExecWait(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("exec wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos exec wait CONTAINER[@BRANCH]:EXEC_ID")
	}
	target, err := parseExecSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	status, err := backend.waitExecSession(target.target, target.execID)
	if err != nil {
		return fmt.Errorf("waiting for %s: %w", target, err)
	}
	fmt.Fprintln(stdout, waitStatusExitCode(status))
	return nil
}

func runExecKill(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("exec kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var signalName string
	fs.StringVar(&signalName, "signal", "KILL", "signal name or number")
	fs.StringVar(&signalName, "s", "KILL", "signal name or number")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttos exec kill [--signal SIGNAL] CONTAINER[@BRANCH]:EXEC_ID")
	}
	target, err := parseExecSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	signal, err := parseSignal(signalName)
	if err != nil {
		return err
	}
	if err := backend.signalExecSession(target.target, target.execID, signal); err != nil {
		return fmt.Errorf("signaling %s: %w", target, err)
	}
	fmt.Fprintln(stdout, target)
	return nil
}

func newExecID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generating exec session ID: %w", err)
	}
	return "exec-" + hex.EncodeToString(random[:]), nil
}

// InspectState is the Docker-compatible lifecycle view of the main branch.
type InspectState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// InspectConfig is the Docker-compatible process configuration view.
type InspectConfig struct {
	Hostname     string              `json:"Hostname"`
	User         string              `json:"User"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	Tty          bool                `json:"Tty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Image        string              `json:"Image"`
	Volumes      map[string]struct{} `json:"Volumes"`
	WorkingDir   string              `json:"WorkingDir"`
	Entrypoint   []string            `json:"Entrypoint"`
	Labels       map[string]string   `json:"Labels"`
	StopSignal   string              `json:"StopSignal"`
}

// InspectHostConfig is the subset of Docker host configuration implemented by TTOS.
type InspectHostConfig struct {
	Binds          []string            `json:"Binds"`
	NetworkMode    string              `json:"NetworkMode"`
	AutoRemove     bool                `json:"AutoRemove"`
	ReadonlyRootfs bool                `json:"ReadonlyRootfs"`
	StopTimeout    *int                `json:"StopTimeout"`
	PidsLimit      *int64              `json:"PidsLimit"`
	ShmSize        int64               `json:"ShmSize"`
	Ulimits        []ttosbundle.Ulimit `json:"Ulimits"`
	Runtime        string              `json:"Runtime"`
	Tmpfs          map[string]string   `json:"Tmpfs,omitempty"`
}

// InspectMount is a Docker-compatible user mount description.
type InspectMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver,omitempty"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

// InspectGraphDriver describes TTOS's immutable lower root and Sentry-memory upper layer.
type InspectGraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

// InspectBranch enriches stable lineage metadata with observational live state.
type InspectBranch struct {
	Metadata        Branch           `json:"metadata"`
	Processes       []ProcessInfo    `json:"processes,omitempty"`
	ProcessError    string           `json:"process_error,omitempty"`
	ForkEligibility *ForkEligibility `json:"fork_eligibility,omitempty"`
	ForkCheckError  string           `json:"fork_check_error,omitempty"`
}

// TTOSInspectInfo contains state with no direct Docker analogue.
type TTOSInspectInfo struct {
	Container    ttosbundle.ContainerInfo `json:"container"`
	Runtime      *Family                  `json:"runtime,omitempty"`
	Branches     []InspectBranch          `json:"branches"`
	RuntimeError string                   `json:"runtime_error,omitempty"`
}

// InspectInfo combines a Docker-compatible main-container view with TTOS lineage.
type InspectInfo struct {
	ID           string             `json:"Id"`
	Created      string             `json:"Created"`
	Path         string             `json:"Path"`
	Args         []string           `json:"Args"`
	State        InspectState       `json:"State"`
	Image        string             `json:"Image"`
	Name         string             `json:"Name"`
	RestartCount int                `json:"RestartCount"`
	Driver       string             `json:"Driver"`
	Platform     string             `json:"Platform"`
	GraphDriver  InspectGraphDriver `json:"GraphDriver"`
	Config       InspectConfig      `json:"Config"`
	HostConfig   InspectHostConfig  `json:"HostConfig"`
	Mounts       []InspectMount     `json:"Mounts"`
	TTOS         TTOSInspectInfo    `json:"TTOS"`
}

func runInspect(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", "format output using a Go template or 'json'")
	fs.StringVar(format, "f", "", "format output using a Go template or 'json'")
	objectType := fs.String("type", "container", "inspect object type (container only)")
	forkCheck := fs.Bool("fork-check", false, "include live-fork eligibility for each branch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *objectType != "container" {
		return fmt.Errorf("inspect type %q is not supported; use 'container'", *objectType)
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos inspect [--format TEMPLATE] [--fork-check] CONTAINER [CONTAINER...]")
	}
	results := make([]InspectInfo, 0, fs.NArg())
	for _, name := range fs.Args() {
		if strings.Contains(name, "@") {
			return fmt.Errorf("inspect accepts container families, not branch selector %q", name)
		}
		info, err := backend.inspect(name)
		if err != nil {
			return err
		}
		if *forkCheck {
			for i := range info.TTOS.Branches {
				branch := &info.TTOS.Branches[i]
				switch branch.Metadata.State {
				case "running", "paused":
					report, err := backend.checkFork(selector{container: name, branch: branch.Metadata.ID})
					if err != nil {
						branch.ForkCheckError = err.Error()
					} else {
						branch.ForkEligibility = &report
					}
				default:
					branch.ForkCheckError = fmt.Sprintf("branch is %s", branch.Metadata.State)
				}
			}
		}
		results = append(results, info)
	}
	if *format != "" {
		return writeInspectFormat(stdout, results, *format)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func writeInspectFormat(w io.Writer, results []InspectInfo, format string) error {
	if format == "json" {
		encoder := json.NewEncoder(w)
		for _, result := range results {
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
		return nil
	}
	tmpl, err := template.New("inspect").Funcs(template.FuncMap{
		"json": func(value any) (string, error) {
			data, err := marshalInspectTemplateJSON(value)
			return string(data), err
		},
		"join":  strings.Join,
		"lower": strings.ToLower,
		"split": strings.Split,
		"upper": strings.ToUpper,
	}).Parse(format)
	if err != nil {
		return fmt.Errorf("parsing inspect format: %w", err)
	}
	for _, result := range results {
		var output bytes.Buffer
		if err := tmpl.Execute(&output, inspectTemplateValuesFor(result)); err != nil {
			return fmt.Errorf("executing inspect format: %w", err)
		}
		if output.Len() == 0 || output.Bytes()[output.Len()-1] != '\n' {
			output.WriteByte('\n')
		}
		if _, err := io.Copy(w, &output); err != nil {
			return err
		}
	}
	return nil
}

type inspectTemplateValues map[string]any

func inspectTemplateValuesFor(result InspectInfo) inspectTemplateValues {
	return inspectTemplateValues{
		"Id":           result.ID,
		"ID":           result.ID,
		"Created":      result.Created,
		"Path":         result.Path,
		"Args":         result.Args,
		"State":        result.State,
		"Image":        result.Image,
		"Name":         result.Name,
		"RestartCount": result.RestartCount,
		"Driver":       result.Driver,
		"Platform":     result.Platform,
		"GraphDriver":  result.GraphDriver,
		"Config":       result.Config,
		"HostConfig":   result.HostConfig,
		"Mounts":       result.Mounts,
		"TTOS":         result.TTOS,
	}
}

func marshalInspectTemplateJSON(value any) ([]byte, error) {
	values, ok := value.(inspectTemplateValues)
	if !ok {
		return json.Marshal(value)
	}
	dockerValues := make(map[string]any, len(values)-1)
	for key, item := range values {
		if key != "ID" {
			dockerValues[key] = item
		}
	}
	return json.Marshal(dockerValues)
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

func runAutoRemove(args []string, backend backend) error {
	if len(args) != 1 || strings.Contains(args[0], "@") {
		return errors.New("internal auto-remove requires one container ID")
	}
	target := selector{container: args[0], branch: defaultBranchID}
	if _, err := backend.wait(target); err != nil {
		return fmt.Errorf("waiting to auto-remove %s: %w", args[0], err)
	}
	if err := backend.remove(args[0], true); err != nil {
		return fmt.Errorf("auto-removing %s: %w", args[0], err)
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

func runImage(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttos image ls|inspect|rm [OPTIONS]")
	}
	switch args[0] {
	case "ls", "list":
		return runImages(args[1:], stdout, stderr, backend)
	case "inspect":
		return runImageInspect(args[1:], stdout, backend)
	case "rm", "remove":
		return runImageRemove(args[1:], stdout, stderr, backend)
	default:
		return fmt.Errorf("unknown image command %q", args[0])
	}
}

func runImages(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	fs.SetOutput(stderr)
	quiet := fs.Bool("quiet", false, "only show image IDs")
	fs.BoolVar(quiet, "q", false, "only show image IDs")
	noTrunc := fs.Bool("no-trunc", false, "do not truncate image IDs")
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: ttos images [--quiet] [--no-trunc] [--json]")
	}
	infos, err := backend.listImages()
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(infos)
	}
	if *quiet {
		seen := make(map[string]bool)
		for _, info := range infos {
			if seen[info.Digest] {
				continue
			}
			seen[info.Digest] = true
			fmt.Fprintln(stdout, displayImageID(info.Digest, *noTrunc))
		}
		return nil
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "REPOSITORY\tTAG\tIMAGE ID\tPLATFORM\tROOTFS\tUNPACKED\tSIZE\tCREATED")
	now := time.Now()
	for _, info := range infos {
		repository, tag := splitImageReference(info.Reference)
		repository = familiarImageReference(repository)
		materialized := "no"
		if info.Materialized {
			materialized = "yes"
		}
		unpacked := "no"
		if info.Unpacked {
			unpacked = "yes"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			repository, tag, displayImageID(info.Digest, *noTrunc), info.Platform,
			materialized, unpacked, formatImageSize(info.Size), formatImageAge(now, info.CreatedAt))
	}
	return table.Flush()
}

func runImageInspect(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttos image inspect IMAGE [IMAGE...]")
	}
	results := make([]ImageInspectInfo, 0, len(args))
	for _, identifier := range args {
		info, err := backend.inspectImage(identifier)
		if err != nil {
			return err
		}
		results = append(results, info)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func runImageRemove(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("image rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "force removal of an image used by a container")
	fs.BoolVar(force, "f", false, "force removal of an image used by a container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos image rm [--force] IMAGE [IMAGE...]")
	}
	for _, identifier := range fs.Args() {
		result, err := backend.removeImage(identifier, *force)
		if err != nil {
			return err
		}
		for _, reference := range result.Untagged {
			fmt.Fprintf(stdout, "Untagged: %s\n", reference)
		}
		if result.Deleted != "" {
			fmt.Fprintf(stdout, "Deleted: %s\n", result.Deleted)
		}
	}
	return nil
}

func displayImageID(digest string, noTrunc bool) string {
	if noTrunc {
		return digest
	}
	id := strings.TrimPrefix(digest, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func splitImageReference(reference string) (string, string) {
	if at := strings.LastIndexByte(reference, '@'); at >= 0 {
		return reference[:at], "<none>"
	}
	slash := strings.LastIndexByte(reference, '/')
	colon := strings.LastIndexByte(reference, ':')
	if colon > slash {
		return reference[:colon], reference[colon+1:]
	}
	return reference, "<none>"
}

func familiarImageReference(reference string) string {
	if strings.HasPrefix(reference, "docker.io/library/") {
		return strings.TrimPrefix(reference, "docker.io/library/")
	}
	return strings.TrimPrefix(reference, "docker.io/")
}

func formatImageSize(size int64) string {
	if size < 1000 {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		value /= 1000
		if value < 1000 || unit == "TB" {
			return fmt.Sprintf("%.3g%s", value, unit)
		}
	}
	panic("unreachable")
}

func formatImageAge(now, created time.Time) string {
	if created.IsZero() {
		return "unknown"
	}
	age := now.Sub(created)
	if age < 0 {
		age = 0
	}
	for _, unit := range []struct {
		duration time.Duration
		name     string
	}{
		{365 * 24 * time.Hour, "year"},
		{30 * 24 * time.Hour, "month"},
		{7 * 24 * time.Hour, "week"},
		{24 * time.Hour, "day"},
		{time.Hour, "hour"},
		{time.Minute, "minute"},
	} {
		if age >= unit.duration {
			count := int(age / unit.duration)
			suffix := ""
			if count != 1 {
				suffix = "s"
			}
			return fmt.Sprintf("%d %s%s ago", count, unit.name, suffix)
		}
	}
	return "less than a minute ago"
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
	var filters stringSlice
	fs.Var(&filters, "filter", "filter output (name, id, status, ancestor, volume, or branch)")
	fs.Var(&filters, "f", "filter output (name, id, status, ancestor, volume, or branch)")
	format := fs.String("format", "", "format rows using a Go template or json")
	last := fs.Int("last", -1, "show the most recently created N branches")
	fs.IntVar(last, "n", -1, "show the most recently created N branches")
	latest := fs.Bool("latest", false, "show only the most recently created branch")
	fs.BoolVar(latest, "l", false, "show only the most recently created branch")
	noTrunc := fs.Bool("no-trunc", false, "do not truncate image IDs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: ttos ps [--all] [--quiet] [--filter KEY=VALUE] [--format TEMPLATE] [--latest|--last N]")
	}
	if *last < -1 {
		return errors.New("ps --last must be -1 or greater")
	}
	if *latest {
		if *last != -1 && *last != 1 {
			return errors.New("ps --latest conflicts with --last other than 1")
		}
		*last = 1
	}
	parsedFilters, err := parsePsFilters(filters)
	if err != nil {
		return err
	}
	families, err := backend.listFamilies()
	if err != nil {
		return fmt.Errorf("listing container families: %w", err)
	}
	rows := psRows(families, time.Now(), *noTrunc)
	includeAllStates := *all || *last >= 0
	selected := rows[:0]
	for _, row := range rows {
		if !includeAllStates && row.State != "running" && row.State != "paused" {
			continue
		}
		if !matchPsFilters(row, parsedFilters) {
			continue
		}
		selected = append(selected, row)
	}
	rows = selected
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i].created, rows[j].created
		if left.Equal(right) {
			return false
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.After(right)
	})
	if *last >= 0 && len(rows) > *last {
		rows = rows[:*last]
	}
	if *quiet {
		for _, row := range rows {
			fmt.Fprintln(stdout, row.ID)
		}
		return nil
	}
	if *format != "" {
		return writePsFormat(stdout, rows, *format)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "CONTAINER\tBRANCH\tIMAGE\tSTATE\tCREATED\tPARENT")
	for _, row := range rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Container, row.Branch, row.Image, row.State, row.RunningFor, row.Parent)
	}
	return table.Flush()
}

type psRow struct {
	ID         string   `json:"id"`
	Names      string   `json:"names"`
	Container  string   `json:"container"`
	Branch     string   `json:"branch"`
	Image      string   `json:"image"`
	ImageID    string   `json:"image_id,omitempty"`
	State      string   `json:"state"`
	Status     string   `json:"status"`
	Parent     string   `json:"parent"`
	CreatedAt  string   `json:"created_at"`
	RunningFor string   `json:"running_for"`
	Volumes    []string `json:"volumes,omitempty"`

	created time.Time
	digest  string
}

func familyDisplayName(family Family) string {
	if family.Name != "" {
		return family.Name
	}
	return family.ID
}

func displayContainerID(id string, noTrunc bool) string {
	if !noTrunc && len(id) > 12 {
		return id[:12]
	}
	return id
}

func psRows(families []Family, now time.Time, noTrunc bool) []psRow {
	var rows []psRow
	for _, family := range families {
		image := familiarImageReference(family.Image)
		if image == "" {
			image = "<unknown>"
		}
		displayName := familyDisplayName(family)
		for _, branch := range family.Branches {
			created := branch.CreatedAt
			if created.IsZero() {
				created = family.CreatedAt
			}
			parent := branch.ParentID
			if parent == "" {
				parent = "-"
			}
			id := displayContainerID(family.ID, noTrunc) + "@" + branch.ID
			name := displayName + "@" + branch.ID
			runningFor := formatImageAge(now, created)
			status := branch.State
			switch branch.State {
			case "running":
				status = "Up " + strings.TrimSuffix(runningFor, " ago")
			case "paused":
				status = "Paused"
			case "created":
				status = "Created"
			case "exited", "stopped":
				status = "Exited"
			}
			createdAt := ""
			if !created.IsZero() {
				createdAt = created.UTC().Format(time.RFC3339Nano)
			}
			rows = append(rows, psRow{
				ID:         id,
				Names:      name,
				Container:  displayName,
				Branch:     branch.ID,
				Image:      image,
				ImageID:    displayImageID(family.Digest, noTrunc),
				State:      branch.State,
				Status:     status,
				Parent:     parent,
				CreatedAt:  createdAt,
				RunningFor: runningFor,
				Volumes:    append([]string(nil), family.Volumes...),
				created:    created,
				digest:     family.Digest,
			})
		}
	}
	return rows
}

type psFilters map[string][]string

func parsePsFilters(values []string) (psFilters, error) {
	filters := make(psFilters)
	for _, value := range values {
		key, match, ok := strings.Cut(value, "=")
		if !ok || key == "" || match == "" {
			return nil, fmt.Errorf("invalid ps filter %q; want KEY=VALUE", value)
		}
		key = strings.ToLower(key)
		switch key {
		case "name":
			if _, err := regexp.Compile(match); err != nil {
				return nil, fmt.Errorf("invalid ps name filter %q: %w", match, err)
			}
		case "id", "status", "ancestor", "volume", "branch":
		default:
			return nil, fmt.Errorf("unsupported ps filter %q", key)
		}
		filters[key] = append(filters[key], match)
	}
	return filters, nil
}

func matchPsFilters(row psRow, filters psFilters) bool {
	for key, matches := range filters {
		matched := false
		for _, match := range matches {
			switch key {
			case "name":
				matched, _ = regexp.MatchString(match, row.Names)
			case "id":
				matched = strings.HasPrefix(row.ID, match) ||
					strings.HasPrefix(row.Container, match)
			case "status":
				matched = strings.EqualFold(row.State, match) ||
					(row.State == "stopped" && strings.EqualFold(match, "exited"))
			case "ancestor":
				matched = matchPsAncestor(row, match)
			case "volume":
				for _, volume := range row.Volumes {
					if volume == match {
						matched = true
						break
					}
				}
			case "branch":
				matched = row.Branch == match
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func matchPsAncestor(row psRow, value string) bool {
	if normalized, err := ttosimages.NormalizeReference(value); err == nil {
		if normalized == row.Image || familiarImageReference(normalized) == row.Image {
			return true
		}
	}
	prefix := strings.TrimPrefix(strings.ToLower(value), "sha256:")
	digest := strings.TrimPrefix(strings.ToLower(row.digest), "sha256:")
	return len(prefix) >= 4 && strings.HasPrefix(digest, prefix)
}

func writePsFormat(w io.Writer, rows []psRow, format string) error {
	if format == "json" {
		encoder := json.NewEncoder(w)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}
	withHeader := false
	if format == "table" {
		withHeader = true
		format = ""
	} else if strings.HasPrefix(format, "table ") {
		withHeader = true
		format = strings.TrimSpace(strings.TrimPrefix(format, "table "))
	}
	if format == "" {
		format = "{{.ID}}"
	}
	tmpl, err := template.New("ps").Funcs(template.FuncMap{
		"json": func(value any) (string, error) {
			data, err := json.Marshal(value)
			return string(data), err
		},
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
	}).Parse(format)
	if err != nil {
		return fmt.Errorf("parsing ps format: %w", err)
	}
	if withHeader {
		header := psRow{
			ID: "ID", Names: "NAMES", Container: "CONTAINER", Branch: "BRANCH",
			Image: "IMAGE", ImageID: "IMAGE ID", State: "STATE", Status: "STATUS",
			Parent: "PARENT", CreatedAt: "CREATED AT", RunningFor: "CREATED",
			Volumes: []string{"VOLUMES"},
		}
		if err := executePsTemplate(w, tmpl, header); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := executePsTemplate(w, tmpl, row); err != nil {
			return err
		}
	}
	return nil
}

func executePsTemplate(w io.Writer, tmpl *template.Template, row psRow) error {
	var output bytes.Buffer
	if err := tmpl.Execute(&output, row); err != nil {
		return fmt.Errorf("executing ps format: %w", err)
	}
	if output.Len() == 0 || output.Bytes()[output.Len()-1] != '\n' {
		output.WriteByte('\n')
	}
	_, err := io.Copy(w, &output)
	return err
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
	fs.IntVar(&timeout, "time", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "timeout", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "t", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos stop [--time SECONDS] TARGET [TARGET...]")
	}
	timeoutSet := flagWasSet(fs, "time") || flagWasSet(fs, "timeout") || flagWasSet(fs, "t")
	if timeoutSet && timeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	for _, arg := range fs.Args() {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		info, err := backend.inspect(target.container)
		if err != nil {
			return fmt.Errorf("inspecting %s before stop: %w", target.container, err)
		}
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		if err := stopBranch(target, stopSignal, stopTimeout, backend); err != nil {
			return err
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

func runWait(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	condition := fs.String("condition", "not-running", "wait condition (not-running only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos wait [--condition not-running] TARGET [TARGET...]")
	}
	if *condition != "not-running" {
		return fmt.Errorf("wait condition %q is not implemented; use 'not-running'", *condition)
	}
	targets := make([]selector, 0, fs.NArg())
	for _, arg := range fs.Args() {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		targets = append(targets, target)
	}
	waiters := make([]<-chan waitResult, len(targets))
	for i, target := range targets {
		waited := make(chan waitResult, 1)
		waiters[i] = waited
		go func(target selector, waited chan<- waitResult) {
			status, err := backend.wait(target)
			waited <- waitResult{status: status, err: err}
		}(target, waited)
	}
	for i, waited := range waiters {
		result := <-waited
		if result.err != nil {
			return fmt.Errorf("waiting for %s: %w", targets[i], result.err)
		}
		fmt.Fprintln(stdout, waitStatusExitCode(result.status))
	}
	return nil
}

func runRestart(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var timeout int
	fs.IntVar(&timeout, "time", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "timeout", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "t", ttosbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	force := fs.Bool("force", false, "discard live descendant branches and in-memory managed volumes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttos restart [--time SECONDS] CONTAINER [CONTAINER...]")
	}
	timeoutSet := flagWasSet(fs, "time") || flagWasSet(fs, "timeout") || flagWasSet(fs, "t")
	if timeoutSet && timeout < -1 {
		return errors.New("restart timeout must be -1 or greater")
	}
	for _, name := range fs.Args() {
		if strings.Contains(name, "@") {
			return fmt.Errorf("restart accepts container families, not branch selector %q", name)
		}
		info, err := backend.inspect(name)
		if err != nil {
			return fmt.Errorf("inspecting %s before restart: %w", name, err)
		}
		if reason := restartDataLossReason(info); reason != "" && !*force {
			return fmt.Errorf("restarting %q would discard %s; use --force", name, reason)
		}
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		target := selector{container: name, branch: defaultBranchID}
		switch info.State.Status {
		case "paused":
			if err := backend.resume(target); err != nil {
				return fmt.Errorf("resuming %s before restart: %w", target, err)
			}
			if err := stopBranch(target, stopSignal, stopTimeout, backend); err != nil {
				return err
			}
		case "running", "restarting":
			if err := stopBranch(target, stopSignal, stopTimeout, backend); err != nil {
				return err
			}
		case "created", "exited", "failed", "dead":
		default:
			return fmt.Errorf("cannot restart container %q in state %q", name, info.State.Status)
		}
		status, err := backend.start(name, true, false)
		if err != nil {
			return fmt.Errorf("starting %s after restart: %w", name, err)
		}
		if status != 0 {
			return exitStatus{code: status}
		}
		fmt.Fprintln(stdout, name)
	}
	return nil
}

func restartDataLossReason(info InspectInfo) string {
	switch info.State.Status {
	case "running", "paused", "restarting":
	default:
		return ""
	}
	for _, branch := range info.TTOS.Branches {
		if branch.Metadata.ID != defaultBranchID {
			return fmt.Sprintf("live descendant branch %q", branch.Metadata.ID)
		}
	}
	volumeNames := []string{}
	if info.TTOS.Runtime != nil {
		volumeNames = append(volumeNames, info.TTOS.Runtime.Volumes...)
	} else {
		seen := make(map[string]bool)
		for _, volume := range info.TTOS.Container.Volumes {
			if !seen[volume.Name] {
				volumeNames = append(volumeNames, volume.Name)
				seen[volume.Name] = true
			}
		}
		sort.Strings(volumeNames)
	}
	if len(volumeNames) != 0 {
		return fmt.Sprintf("in-memory managed volume state (%s)", strings.Join(volumeNames, ", "))
	}
	return ""
}

type waitResult struct {
	status uint32
	err    error
}

func stopPolicy(info InspectInfo) (int, int, error) {
	stopSignal := info.Config.StopSignal
	if stopSignal == "" {
		stopSignal = ttosbundle.DefaultStopSignal
	}
	signal, err := parseSignal(stopSignal)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid configured stop signal %q: %w", stopSignal, err)
	}
	timeout := ttosbundle.DefaultStopTimeoutSeconds
	if info.HostConfig.StopTimeout != nil {
		timeout = *info.HostConfig.StopTimeout
	}
	if timeout < -1 {
		return 0, 0, fmt.Errorf("invalid configured stop timeout %d", timeout)
	}
	return signal, timeout, nil
}

func stopBranch(target selector, stopSignal, timeoutSeconds int, backend backend) error {
	const signalKill = 9
	if err := backend.signal(target, stopSignal, false); err != nil {
		return fmt.Errorf("stopping %s with signal %d: %w", target, stopSignal, err)
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
	signal, err := mobysignal.ParseSignal(value)
	if err != nil || !mobysignal.ValidSignalForPlatform(signal) {
		return 0, fmt.Errorf("unknown signal %q", value)
	}
	return int(signal), nil
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

type execSelector struct {
	target selector
	execID string
}

func parseExecSelector(value string) (execSelector, error) {
	separator := strings.LastIndexByte(value, ':')
	if separator <= 0 || separator == len(value)-1 {
		return execSelector{}, errors.New("exec selector must be CONTAINER[@BRANCH]:EXEC_ID")
	}
	target, err := parseSelector(value[:separator])
	if err != nil {
		return execSelector{}, err
	}
	execID := value[separator+1:]
	for index := 0; index < len(execID); index++ {
		char := execID[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '.' || char == '_' || char == '-')) {
			continue
		}
		return execSelector{}, fmt.Errorf("invalid exec session ID %q", execID)
	}
	return execSelector{target: target, execID: execID}, nil
}

func (s execSelector) String() string {
	return s.target.String() + ":" + s.execID
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
	ID               string    `json:"id"`
	ParentID         string    `json:"parent_id,omitempty"`
	State            string    `json:"state"`
	ForkTotalNS      int64     `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS int64     `json:"source_downtime_ns,omitempty"`
	KernelForkNS     int64     `json:"kernel_fork_ns,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Family is stable family metadata returned by ttos ls --json.
type Family struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Image      string    `json:"image,omitempty"`
	Digest     string    `json:"digest,omitempty"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	SandboxPID int       `json:"sandbox_pid,omitempty"`
	Volumes    []string  `json:"volumes,omitempty"`
	Branches   []Branch  `json:"branches"`
	Error      string    `json:"error,omitempty"`
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

// ImageInfo is one named local image record enriched with TTOS cache usage.
type ImageInfo struct {
	Reference    string    `json:"reference"`
	Digest       string    `json:"digest"`
	MediaType    string    `json:"media_type"`
	Size         int64     `json:"size"`
	Platform     string    `json:"platform"`
	Snapshotter  string    `json:"snapshotter,omitempty"`
	Unpacked     bool      `json:"unpacked"`
	Materialized bool      `json:"materialized"`
	Containers   []string  `json:"containers,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ImageInspectInfo follows Docker's top-level image inspection naming while
// exposing TTOS-specific root and container use explicitly.
type ImageInspectInfo struct {
	ID           string    `json:"Id"`
	RepoTags     []string  `json:"RepoTags"`
	RepoDigests  []string  `json:"RepoDigests"`
	Created      time.Time `json:"Created"`
	Size         int64     `json:"Size"`
	Architecture string    `json:"Architecture"`
	OS           string    `json:"Os"`
	Variant      string    `json:"Variant,omitempty"`
	MediaType    string    `json:"MediaType"`
	Unpacked     bool      `json:"Unpacked"`
	Materialized bool      `json:"Materialized"`
	Containers   []string  `json:"Containers,omitempty"`
}

// ImageRemoveResult describes Docker-compatible removal output.
type ImageRemoveResult struct {
	Untagged []string
	Deleted  string
}

type backend interface {
	pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error)
	listImages() ([]ImageInfo, error)
	inspectImage(identifier string) (ImageInspectInfo, error)
	removeImage(identifier string, force bool) (ImageRemoveResult, error)
	create(request runRequest) (string, error)
	run(request runRequest) (containerRunResult, error)
	start(name string, detach, interactive bool) (int, error)
	attach(request attachRequest) (uint32, error)
	logs(target selector, execID string, follow bool, tail int) error
	exec(request execRequest) (int, error)
	execSessions(target selector) ([]runscExecSession, error)
	waitExecSession(target selector, execID string) (uint32, error)
	signalExecSession(target selector, execID string, signal int) error
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
	root                  string
	runsc                 string
	containerd            string
	namespace             string
	dataRoot              string
	unsafeNoHostIsolation bool
	stdin                 io.Reader
	stdout                io.Writer
	stderr                io.Writer
}

func (b *runtimeBackend) containerStore() ttosbundle.Store {
	return ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
}

func (b *runtimeBackend) resolveContainer(identifier string) (ttosbundle.ContainerInfo, error) {
	if b.dataRoot == "" {
		return ttosbundle.ContainerInfo{ID: identifier, Name: identifier}, nil
	}
	return b.containerStore().LoadContainer(identifier)
}

func (b *runtimeBackend) resolveSelector(target selector) (selector, ttosbundle.ContainerInfo, error) {
	info, err := b.resolveContainer(target.container)
	if err != nil {
		return selector{}, ttosbundle.ContainerInfo{}, err
	}
	target.container = info.ID
	return target, info, nil
}

func (b *runtimeBackend) imageRecords() ([]ttosimages.Info, error) {
	return ttosimages.List(context.Background(), ttosimages.ListOptions{
		Address:     b.containerd,
		Namespace:   b.namespace,
		Snapshotter: defaultSnapshotter(),
	})
}

func (b *runtimeBackend) listImages() ([]ImageInfo, error) {
	records, err := b.imageRecords()
	if err != nil {
		return nil, err
	}
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	containers, err := store.ListContainers()
	if err != nil {
		return nil, err
	}
	users := make(map[string][]string)
	for _, container := range containers {
		users[container.Digest] = append(users[container.Digest], container.Name)
	}
	infos := make([]ImageInfo, 0, len(records))
	for _, record := range records {
		materialized, err := store.HasImageRoot(record.Digest, record.Platform)
		if err != nil {
			return nil, err
		}
		containerNames := append([]string(nil), users[record.Digest]...)
		sort.Strings(containerNames)
		infos = append(infos, ImageInfo{
			Reference:    record.Reference,
			Digest:       record.Digest,
			MediaType:    record.MediaType,
			Size:         record.Size,
			Platform:     record.Platform,
			Snapshotter:  record.Snapshotter,
			Unpacked:     record.Unpacked,
			Materialized: materialized,
			Containers:   containerNames,
			CreatedAt:    record.CreatedAt,
			UpdatedAt:    record.UpdatedAt,
		})
	}
	return infos, nil
}

func (b *runtimeBackend) inspectImage(identifier string) (ImageInspectInfo, error) {
	infos, err := b.listImages()
	if err != nil {
		return ImageInspectInfo{}, err
	}
	return inspectImageFromInventory(infos, identifier)
}

func inspectImageFromInventory(infos []ImageInfo, identifier string) (ImageInspectInfo, error) {
	records := make([]ttosimages.Info, 0, len(infos))
	for _, info := range infos {
		records = append(records, ttosimages.Info{
			Reference: info.Reference,
			Digest:    info.Digest,
		})
	}
	matches, err := ttosimages.Resolve(records, identifier)
	if err != nil {
		return ImageInspectInfo{}, err
	}
	digest := matches[0].Digest
	var selected *ImageInfo
	var tags, repoDigests, containers []string
	seenRepoDigest := make(map[string]bool)
	seenContainer := make(map[string]bool)
	materialized := false
	unpacked := false
	for i := range infos {
		info := &infos[i]
		if info.Digest != digest {
			continue
		}
		if selected == nil {
			selected = info
		}
		if strings.Contains(info.Reference, "@") {
			repoDigest := familiarImageReference(info.Reference)
			if !seenRepoDigest[repoDigest] {
				repoDigests = append(repoDigests, repoDigest)
				seenRepoDigest[repoDigest] = true
			}
		} else {
			tags = append(tags, familiarImageReference(info.Reference))
			repository, _ := splitImageReference(info.Reference)
			repoDigest := familiarImageReference(repository) + "@" + digest
			if !seenRepoDigest[repoDigest] {
				repoDigests = append(repoDigests, repoDigest)
				seenRepoDigest[repoDigest] = true
			}
		}
		materialized = materialized || info.Materialized
		unpacked = unpacked || info.Unpacked
		for _, container := range info.Containers {
			if !seenContainer[container] {
				containers = append(containers, container)
				seenContainer[container] = true
			}
		}
	}
	if selected == nil {
		return ImageInspectInfo{}, fmt.Errorf("image %q does not exist", identifier)
	}
	sort.Strings(tags)
	sort.Strings(repoDigests)
	sort.Strings(containers)
	platform := strings.Split(selected.Platform, "/")
	osName, architecture, variant := "", "", ""
	if len(platform) > 0 {
		osName = platform[0]
	}
	if len(platform) > 1 {
		architecture = platform[1]
	}
	if len(platform) > 2 {
		variant = platform[2]
	}
	return ImageInspectInfo{
		ID:           digest,
		RepoTags:     tags,
		RepoDigests:  repoDigests,
		Created:      selected.CreatedAt,
		Size:         selected.Size,
		Architecture: architecture,
		OS:           osName,
		Variant:      variant,
		MediaType:    selected.MediaType,
		Unpacked:     unpacked,
		Materialized: materialized,
		Containers:   containers,
	}, nil
}

func (b *runtimeBackend) removeImage(identifier string, force bool) (ImageRemoveResult, error) {
	records, err := b.imageRecords()
	if err != nil {
		return ImageRemoveResult{}, err
	}
	matches, err := ttosimages.Resolve(records, identifier)
	if err != nil {
		return ImageRemoveResult{}, err
	}
	digest := matches[0].Digest
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	containers, err := store.ListContainers()
	if err != nil {
		return ImageRemoveResult{}, err
	}
	var users []string
	for _, container := range containers {
		if container.Digest == digest {
			users = append(users, container.Name)
		}
	}
	sort.Strings(users)
	if err := validateImageRemoval(digest, users, force); err != nil {
		return ImageRemoveResult{}, err
	}
	result := ImageRemoveResult{Untagged: make([]string, 0, len(matches))}
	for _, match := range matches {
		if err := ttosimages.Delete(context.Background(), ttosimages.DeleteOptions{
			Address:   b.containerd,
			Namespace: b.namespace,
			Reference: match.Reference,
			Digest:    match.Digest,
		}); err != nil {
			return result, err
		}
		result.Untagged = append(result.Untagged, familiarImageReference(match.Reference))
	}
	remaining, err := b.imageRecords()
	if err != nil {
		return result, err
	}
	for _, record := range remaining {
		if record.Digest == digest {
			return result, nil
		}
	}
	result.Deleted = digest
	if len(users) == 0 {
		if _, err := store.RemoveImageRoots(digest); err != nil {
			return result, err
		}
	}
	return result, nil
}

func validateImageRemoval(digest string, users []string, force bool) error {
	if len(users) != 0 && !force {
		return fmt.Errorf("image %s is used by container(s) %s; use --force to remove its image records", digest, strings.Join(users, ", "))
	}
	return nil
}

func (b *runtimeBackend) run(request runRequest) (containerRunResult, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	prepared, err := store.Prepare(context.Background(), ttosbundle.PrepareOptions{
		SpecOptions: ttosbundle.SpecOptions{
			Name:             request.name,
			Args:             request.args,
			Env:              request.env,
			Labels:           request.labels,
			Cwd:              request.cwd,
			User:             request.user,
			Hostname:         request.hostname,
			Terminal:         request.terminal,
			NetworkMode:      request.networkMode,
			ReadonlyRootfs:   request.readonlyRootfs,
			StopSignal:       request.stopSignal,
			PidsLimit:        request.pidsLimit,
			ShmSize:          &request.shmSize,
			Ulimits:          request.ulimits,
			Entrypoint:       request.entrypoint,
			ManagedVolumes:   request.managedVolumes,
			HostMounts:       request.hostMounts,
			FilesystemMounts: request.filesystemMounts,
		},
		Reference:          request.reference,
		Platform:           request.platform,
		PullPolicy:         request.pullPolicy,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
		OpenStdin:          request.interactive,
		AutoRemove:         request.autoRemove,
		StopTimeout:        &request.stopTimeout,
	})
	if err != nil {
		return containerRunResult{}, err
	}
	id := prepared.Container.ID
	status, err := b.launch(id, prepared.BundleDir, request.networkMode, request.detach, request.interactive, request.terminal)
	if err == nil {
		b.recordLaunchState(id, request.detach, status)
	}
	if request.autoRemove {
		status, err = b.applyAutoRemove(id, request.detach, status, err)
	}
	return containerRunResult{ID: id, Status: status}, err
}

func (b *runtimeBackend) create(request runRequest) (string, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	prepared, err := store.Prepare(context.Background(), ttosbundle.PrepareOptions{
		SpecOptions: ttosbundle.SpecOptions{
			Name:             request.name,
			Args:             request.args,
			Env:              request.env,
			Labels:           request.labels,
			Cwd:              request.cwd,
			User:             request.user,
			Hostname:         request.hostname,
			Terminal:         request.terminal,
			NetworkMode:      request.networkMode,
			ReadonlyRootfs:   request.readonlyRootfs,
			StopSignal:       request.stopSignal,
			PidsLimit:        request.pidsLimit,
			ShmSize:          &request.shmSize,
			Ulimits:          request.ulimits,
			Entrypoint:       request.entrypoint,
			HostMounts:       request.hostMounts,
			ManagedVolumes:   request.managedVolumes,
			FilesystemMounts: request.filesystemMounts,
		},
		Reference:          request.reference,
		Platform:           request.platform,
		PullPolicy:         request.pullPolicy,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
		OpenStdin:          request.interactive,
		AutoRemove:         request.autoRemove,
		StopTimeout:        &request.stopTimeout,
	})
	if err != nil {
		return "", err
	}
	return prepared.Container.ID, nil
}

func (b *runtimeBackend) start(name string, detach, interactive bool) (int, error) {
	info, err := b.resolveContainer(name)
	if err != nil {
		return 1, err
	}
	if err := b.removeStoppedRuntime(info.ID); err != nil {
		return 1, err
	}
	status, err := b.launch(info.ID, info.BundleDir, info.NetworkMode, detach, interactive, info.Terminal)
	if err == nil {
		b.recordLaunchState(info.ID, detach, status)
	}
	if info.AutoRemove {
		return b.applyAutoRemove(info.ID, detach, status, err)
	}
	return status, err
}

func (b *runtimeBackend) applyAutoRemove(id string, detach bool, status int, launchErr error) (int, error) {
	if detach && launchErr == nil && status == 0 {
		if err := b.scheduleAutoRemove(id); err == nil {
			return status, nil
		} else {
			cleanupErr := b.remove(id, true)
			if cleanupErr != nil {
				return 1, fmt.Errorf("scheduling auto-remove: %w (cleanup also failed: %v)", err, cleanupErr)
			}
			return 1, fmt.Errorf("scheduling auto-remove: %w; container was removed", err)
		}
	}
	cleanupErr := b.remove(id, true)
	if cleanupErr != nil {
		if launchErr != nil {
			return status, fmt.Errorf("%w (auto-remove also failed: %v)", launchErr, cleanupErr)
		}
		return 1, fmt.Errorf("auto-removing container after exit: %w", cleanupErr)
	}
	return status, launchErr
}

func (b *runtimeBackend) scheduleAutoRemove(id string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating ttos executable: %w", err)
	}
	args := []string{
		"--root", b.root,
		"--runsc", b.runsc,
		"--containerd", b.containerd,
		"--namespace", b.namespace,
		"--data-root", b.dataRoot,
	}
	if b.unsafeNoHostIsolation {
		args = append(args, "--unsafe-no-host-isolation")
	}
	args = append(args, "_auto-remove", id)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening auto-remove stdio: %w", err)
	}
	defer devNull.Close()
	errorPath := filepath.Join(b.dataRoot, "containers", id, "auto-remove.error.log")
	errorLog, err := os.OpenFile(errorPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening auto-remove error log: %w", err)
	}
	defer errorLog.Close()
	command := exec.Command(executable, args...)
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = errorLog
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("starting auto-remove helper: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("releasing auto-remove helper: %w", err)
	}
	return nil
}

func (b *runtimeBackend) removeStoppedRuntime(name string) error {
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("checking runtime state before starting %q: %w", name, err)
	}
	for _, family := range families {
		if family.ID != name {
			continue
		}
		if family.State != "stopped" && family.State != "exited" {
			return fmt.Errorf("container %q is already %s", name, family.State)
		}
		if _, err := b.command("delete", "--force", name); err != nil {
			return fmt.Errorf("deleting stopped runtime container %q: %w", name, err)
		}
		return nil
	}
	return nil
}

func (b *runtimeBackend) exec(request execRequest) (int, error) {
	resolved, _, err := b.resolveSelector(request.target)
	if err != nil {
		return 1, err
	}
	request.target = resolved
	args := []string{"--root", b.root, "exec", "--branch", request.target.branch}
	consoleID := ""
	if request.detach {
		if request.execID == "" {
			return 1, fmt.Errorf("detached exec requires a stable session ID")
		}
		consoleID = "ttos-" + request.execID
		args = append(args, "--detach", "--exec-id", request.execID, "--internal-console", consoleID)
	}

	for _, value := range request.env {
		args = append(args, "--env", value)
	}
	if request.cwd != "" {
		args = append(args, "--cwd", request.cwd)
	}
	if request.user != "" {
		args = append(args, "--user", request.user)
	}
	for _, gid := range request.additionalGIDs {
		args = append(args, "--additional-gids", gid)
	}
	for _, capability := range request.capabilities {
		args = append(args, "--cap", capability)
	}
	if request.processPath != "" {
		args = append(args, "--process", request.processPath)
	}
	if request.terminal {
		if !request.detach {
			consoleID = fmt.Sprintf("ttos-exec-%d-%d", os.Getpid(), time.Now().UnixNano())
			args = append(args, "--internal-console", consoleID)
		}
		args = append(args, "--internal-console-terminal")
		if request.interactive && !request.detach {
			args = append(args, "--internal-console-stdin")
		}
	}
	args = append(args, request.target.container)
	if request.processPath == "" {
		args = append(args, request.args...)
	}
	command := exec.Command(b.runsc, args...)
	if !request.detach && (request.interactive || request.terminal) {
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
	if request.detach {
		session, err := b.execSession(request.target, request.execID)
		if err != nil {
			return 1, err
		}
		if _, err := b.ensureExecConsoleDrain(request.target, session.ID, session.ConsoleID, true); err != nil {
			cleanupErr := b.signalExecSession(request.target, request.execID, int(linux.SIGKILL))
			if cleanupErr != nil {
				return 1, fmt.Errorf("starting exec console drain: %w (session cleanup also failed: %v)", err, cleanupErr)
			}
			return 1, fmt.Errorf("starting exec console drain: %w; detached session was killed", err)
		}
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

func readConsoleWaitStatus(path string) (uint32, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading console wait status: %w", err)
	}
	value := strings.TrimSpace(string(data))
	status, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("parsing console wait status %q: %w", value, err)
	}
	return uint32(status), true, nil
}

func (b *runtimeBackend) attach(request attachRequest) (status uint32, retErr error) {
	target, _, err := b.resolveSelector(request.target)
	if err != nil {
		return 0, err
	}
	execID := request.execID
	var paths ttosbundle.ConsolePaths
	consoleID := "init"
	terminal := false
	if execID == "" {
		paths, err = b.ensureConsoleDrain(target, false)
		if err != nil {
			return 0, err
		}
		store := ttosbundle.Store{Root: b.dataRoot}
		containerInfo, err := store.LoadContainer(target.container)
		if err != nil {
			return 0, err
		}
		terminal = containerInfo.Terminal
	} else {
		session, err := b.execSession(target, execID)
		if err != nil {
			return 0, err
		}
		consoleID = session.ConsoleID
		terminal = session.Terminal
		paths, err = b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
		if err != nil {
			return 0, err
		}
	}
	if terminal {
		if err := b.resizeConsoleFromHost(target, consoleID); err != nil {
			return 0, err
		}
		stopResize := b.watchConsoleResize(target, consoleID)
		defer stopResize()
	}
	_, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return 0, err
	}
	if request.stdin {
		inputLock, err := acquireConsoleStdin(paths.StdinLock, target)
		if err != nil {
			return 0, err
		}
		defer func() {
			if err := inputLock.Close(); retErr == nil && err != nil {
				retErr = fmt.Errorf("releasing stdin ownership for %s: %w", target, err)
			}
		}()
		var restore func() error
		if terminal {
			restore, err = makeRawTerminal(b.stdin)
			if err != nil {
				return 0, err
			}
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
	var waitResult *attachWaitResult
	if status, found, err := readConsoleWaitStatus(paths.WaitStatus); err != nil {
		return 0, err
	} else if found {
		waitResult = &attachWaitResult{status: status}
	}
	waitForExit := b.wait
	if execID != "" {
		waitForExit = func(target selector) (uint32, error) {
			return b.waitExecSession(target, execID)
		}
	}
	var waitCh chan attachWaitResult
	if waitResult == nil {
		ch := make(chan attachWaitResult, 1)
		waitCh = ch
		go func() {
			status, err := waitForExit(target)
			ch <- attachWaitResult{status: status, err: err}
		}()
	}
	inputErrCh := make(chan error, 1)
	if request.stdin {
		go func() { inputErrCh <- b.pumpConsoleInput(target, consoleID, request.detachKeys) }()
	}
	var stdout, stderr io.Writer
	if request.stdout {
		stdout = b.stdout
	}
	if request.stderr {
		stderr = b.stderr
	}
	var waitErr error
	for {
		progress, err := follower.drain(stdout, stderr)
		if err != nil {
			return 0, err
		}
		if waitResult == nil {
			if status, found, err := readConsoleWaitStatus(paths.WaitStatus); err != nil {
				return 0, err
			} else if found {
				waitResult = &attachWaitResult{status: status}
				waitCh = nil
			}
		}
		if waitResult != nil && follower.stdoutEOF && follower.stderrEOF {
			return waitResult.status, nil
		}
		select {
		case result := <-waitCh:
			if result.err != nil {
				waitErr = result.err
			} else {
				waitResult = &result
			}
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
			if !active {
				if !follower.stdoutEOF || !follower.stderrEOF {
					return 0, consoleDrainStoppedError(paths.Error)
				}
				if waitResult == nil {
					if waitErr != nil {
						return 0, waitErr
					}
					return 0, consoleDrainStoppedError(paths.Error)
				}
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

func (b *runtimeBackend) logs(target selector, execID string, follow bool, tail int) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	var paths ttosbundle.ConsolePaths
	if execID == "" {
		paths, err = b.ensureConsoleDrain(target, false)
	} else {
		session, sessionErr := b.execSession(target, execID)
		if sessionErr != nil {
			return sessionErr
		}
		paths, err = b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
	}
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
	return b.ensureConsoleDrainPaths(target, "", "init", paths, reset)
}

func (b *runtimeBackend) ensureExecConsoleDrain(target selector, execID, consoleID string, reset bool) (ttosbundle.ConsolePaths, error) {
	store := ttosbundle.Store{Root: b.dataRoot}
	paths, err := store.ExecConsolePaths(target.container, target.branch, execID)
	if err != nil {
		return ttosbundle.ConsolePaths{}, err
	}
	return b.ensureConsoleDrainPaths(target, execID, consoleID, paths, reset)
}

func (b *runtimeBackend) ensureConsoleDrainPaths(target selector, execID, consoleID string, paths ttosbundle.ConsolePaths, reset bool) (ttosbundle.ConsolePaths, error) {
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
	if !reset {
		if _, err := os.Stat(paths.WaitStatus); err == nil {
			return paths, nil
		} else if !os.IsNotExist(err) {
			return ttosbundle.ConsolePaths{}, fmt.Errorf("checking console wait status: %w", err)
		}
	}
	maxBytes, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return ttosbundle.ConsolePaths{}, err
	}
	if reset {
		if err := os.Remove(paths.WaitStatus); err != nil && !os.IsNotExist(err) {
			return ttosbundle.ConsolePaths{}, fmt.Errorf("removing stale console wait status: %w", err)
		}
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

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return ttosbundle.ConsolePaths{}, fmt.Errorf("creating console readiness pipe: %w", err)
	}
	defer readyReader.Close()
	commandArgs := []string{
		"--root", b.root,
		"timetravel", "--console", consoleID,
		"--console-log", paths.Log,
		"--console-log-max-bytes", strconv.FormatInt(maxBytes, 10),
		"--console-log-max-files", strconv.Itoa(maxFiles),
		"--console-wait-status", paths.WaitStatus,
		"--console-ready-fd", "4",
	}
	if execID != "" {
		commandArgs = append(commandArgs, "--exec-id", execID)
	}
	commandArgs = append(commandArgs, target.container, "console-follow", target.branch)
	command := exec.Command(b.runsc, commandArgs...)
	command.Stderr = errorFile
	// The inherited duplicate keeps the advisory lock held for exactly the
	// lifetime of the drainer, preventing multiple consumers of one pipe.
	command.ExtraFiles = []*os.File{lockFile, readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return ttosbundle.ConsolePaths{}, fmt.Errorf("starting console drain for %s: %w", target, err)
	}
	if err := readyWriter.Close(); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttosbundle.ConsolePaths{}, fmt.Errorf("closing console readiness writer for %s: %w", target, err)
	}
	if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttosbundle.ConsolePaths{}, fmt.Errorf("setting console readiness deadline for %s: %w", target, err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyReader, ready[:]); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttosbundle.ConsolePaths{}, fmt.Errorf("waiting for console drain readiness for %s: %w: %v", target, err, consoleDrainStoppedError(paths.Error))
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

func (b *runtimeBackend) resizeConsoleFromHost(target selector, consoleID string) error {
	cols, rows, ok, err := hostTerminalSize(b.stdin)
	if err != nil || !ok {
		return err
	}
	_, err = b.command(
		"timetravel", "--console", consoleID,
		"--rows", strconv.Itoa(rows), "--cols", strconv.Itoa(cols),
		target.container, "console-resize", target.branch,
	)
	return err
}

func (b *runtimeBackend) watchConsoleResize(target selector, consoleID string) func() {
	file, ok := b.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return func() {}
	}
	resizeSignals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-resizeSignals:
				if err := b.resizeConsoleFromHost(target, consoleID); err != nil && b.stderr != nil {
					fmt.Fprintf(b.stderr, "ttos: warning: resizing console for %s: %v\n", target, err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(resizeSignals)
		close(done)
	}
}

func (b *runtimeBackend) writeConsole(target selector, consoleID string, data []byte) (consoleWriteResult, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	output, err := b.command("timetravel", "--console", consoleID, "--data-base64", encoded, target.container, "console-write", target.branch)
	if err != nil {
		return consoleWriteResult{}, err
	}
	var result consoleWriteResult
	if err := json.Unmarshal(output, &result); err != nil {
		return consoleWriteResult{}, fmt.Errorf("decoding console write: %w", err)
	}
	return result, nil
}

func (b *runtimeBackend) writeConsoleAll(target selector, consoleID string, data []byte) error {
	for written := 0; written < len(data); {
		result, err := b.writeConsole(target, consoleID, data[written:])
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

func (b *runtimeBackend) closeConsoleStdin(target selector, consoleID string) error {
	_, err := b.command("timetravel", "--console", consoleID, target.container, "console-close-stdin", target.branch)
	return err
}

func (b *runtimeBackend) pumpConsoleInput(target selector, consoleID string, detachKeys []byte) error {
	if b.stdin == nil {
		return b.closeConsoleStdin(target, consoleID)
	}
	filter := detachKeyFilter{sequence: append([]byte(nil), detachKeys...)}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := b.stdin.Read(buf)
		data, detached := filter.push(buf[:n])
		if err := b.writeConsoleAll(target, consoleID, data); err != nil {
			return err
		}
		if detached {
			return errDetached
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := b.writeConsoleAll(target, consoleID, filter.flush()); err != nil {
					return err
				}
				return b.closeConsoleStdin(target, consoleID)
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

func inspectTimestamp(value *time.Time) string {
	if value == nil {
		return time.Time{}.UTC().Format(time.RFC3339Nano)
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func inspectUser(user specs.User) string {
	return strconv.FormatUint(uint64(user.UID), 10) + ":" + strconv.FormatUint(uint64(user.GID), 10)
}

func inspectMounts(spec *specs.Spec) ([]InspectMount, []string, map[string]struct{}, map[string]string) {
	mounts := []InspectMount{}
	binds := []string{}
	volumes := make(map[string]struct{})
	tmpfs := make(map[string]string)
	if spec == nil {
		return mounts, binds, volumes, tmpfs
	}
	dockerTmpfs := make(map[string]string)
	if encoded := spec.Annotations[ttosbundle.TmpfsConfigAnnotation]; encoded != "" {
		_ = json.Unmarshal([]byte(encoded), &dockerTmpfs)
	}
	for _, mount := range spec.Mounts {
		readonly := false
		for _, option := range mount.Options {
			if option == "ro" {
				readonly = true
			}
		}
		mode := "rw"
		if readonly {
			mode = "ro"
		}
		switch {
		case mount.Type == "bind":
			binds = append(binds, mount.Source+":"+mount.Destination+":"+mode)
			mounts = append(mounts, InspectMount{
				Type: "bind", Source: mount.Source, Destination: mount.Destination,
				Mode: mode, RW: !readonly, Propagation: "rprivate",
			})
		case mount.Type == "tmpfs" && strings.HasPrefix(mount.Source, "ttos-volume-"):
			name := strings.TrimPrefix(mount.Source, "ttos-volume-")
			volumes[mount.Destination] = struct{}{}
			mounts = append(mounts, InspectMount{
				Type: "volume", Name: name, Source: name, Destination: mount.Destination,
				Driver: "local-tmpfs", Mode: mode, RW: !readonly,
			})
		case mount.Type == "tmpfs" && mount.Source == "tmpfs" && mount.Destination != "/dev":
			if inspectOptions, ok := dockerTmpfs[mount.Destination]; ok {
				tmpfs[mount.Destination] = inspectOptions
			} else {
				mounts = append(mounts, InspectMount{
					Type: "tmpfs", Source: "", Destination: mount.Destination, RW: !readonly,
				})
			}
		case mount.Type == "proc" && mount.Destination != "/proc":
			mounts = append(mounts, InspectMount{
				Type: "proc", Source: mount.Source, Destination: mount.Destination,
				Mode: mode, RW: !readonly,
			})
		case mount.Type == "sysfs" && mount.Destination != "/sys":
			mounts = append(mounts, InspectMount{
				Type: "sysfs", Source: mount.Source, Destination: mount.Destination,
				Mode: mode, RW: !readonly,
			})
		}
	}
	return mounts, binds, volumes, tmpfs
}

func buildInspectInfo(container ttosbundle.ContainerInfo, spec *specs.Spec, family *Family, runtimeErr string) InspectInfo {
	state := container.State
	if state == "" {
		state = "created"
	}
	if family != nil {
		state = family.State
		for _, branch := range family.Branches {
			if branch.ID == defaultBranchID {
				state = branch.State
				break
			}
		}
	}
	if state == "stopped" {
		state = "exited"
	}
	exitCode := 0
	if container.ExitStatus != nil {
		exitCode = *container.ExitStatus
	}
	sandboxPID := 0
	stateError := runtimeErr
	if family != nil {
		sandboxPID = family.SandboxPID
		if family.Error != "" {
			stateError = family.Error
		}
	}
	process := &specs.Process{}
	if spec != nil && spec.Process != nil {
		process = spec.Process
	}
	path := ""
	args := []string{}
	if len(process.Args) != 0 {
		path = process.Args[0]
		args = append(args, process.Args[1:]...)
	}
	mounts, binds, volumes, tmpfs := inspectMounts(spec)
	hostname := container.Name
	configUser := inspectUser(process.User)
	configCmd := append([]string(nil), process.Args...)
	var entrypoint []string
	labels := make(map[string]string)
	if spec != nil {
		if spec.Hostname != "" {
			hostname = spec.Hostname
		}
		if value, ok := spec.Annotations["dev.timetravelos.config.user"]; ok {
			configUser = value
		}
		if value, ok := spec.Annotations["dev.timetravelos.config.cmd"]; ok {
			_ = json.Unmarshal([]byte(value), &configCmd)
		}
		if value, ok := spec.Annotations["dev.timetravelos.config.entrypoint"]; ok {
			_ = json.Unmarshal([]byte(value), &entrypoint)
		}
		if value, ok := spec.Annotations["dev.timetravelos.config.labels"]; ok {
			_ = json.Unmarshal([]byte(value), &labels)
		}
	}
	branches := []InspectBranch{}
	if family != nil {
		for _, branch := range family.Branches {
			branches = append(branches, InspectBranch{Metadata: branch})
		}
	}
	created := container.CreatedAt.UTC().Format(time.RFC3339Nano)
	if container.CreatedAt.IsZero() {
		created = inspectTimestamp(nil)
	}
	readonlyRootfs := spec != nil && spec.Root != nil && spec.Root.Readonly
	stopSignal := container.StopSignal
	if stopSignal == "" {
		stopSignal = ttosbundle.DefaultStopSignal
	}
	stopTimeout := container.StopTimeout
	if stopTimeout == nil {
		value := ttosbundle.DefaultStopTimeoutSeconds
		stopTimeout = &value
	}
	shmSize := ttosbundle.DefaultShmSizeBytes
	if container.ShmSize != nil {
		shmSize = *container.ShmSize
	}
	return InspectInfo{
		ID: container.ID, Created: created, Path: path, Args: args,
		State: InspectState{
			Status: state, Running: state == "running" || state == "paused", Paused: state == "paused",
			Dead: state == "failed" || state == "destroying", Pid: sandboxPID, ExitCode: exitCode,
			Error: stateError, StartedAt: inspectTimestamp(container.StartedAt), FinishedAt: inspectTimestamp(container.ExitedAt),
		},
		Image: container.Digest, Name: "/" + container.Name, Driver: "ttos-memory-overlay",
		Platform: container.Platform, RestartCount: 0,
		GraphDriver: InspectGraphDriver{
			Name: "ttos-memory-overlay",
			Data: map[string]string{"LowerDir": container.RootFS, "UpperDir": "sentry-memory"},
		},
		Config: InspectConfig{
			Hostname: hostname, User: configUser, AttachStdin: container.OpenStdin, AttachStdout: true, AttachStderr: true,
			Tty: process.Terminal, OpenStdin: container.OpenStdin, Env: append([]string(nil), process.Env...), Cmd: configCmd,
			Image: container.Reference, Volumes: volumes, WorkingDir: process.Cwd, Entrypoint: entrypoint, Labels: labels, StopSignal: stopSignal,
		},
		HostConfig: InspectHostConfig{
			Binds: binds, AutoRemove: container.AutoRemove, NetworkMode: container.NetworkMode, ReadonlyRootfs: readonlyRootfs, StopTimeout: stopTimeout, Runtime: "runsc", PidsLimit: container.PidsLimit, ShmSize: shmSize, Ulimits: container.Ulimits, Tmpfs: tmpfs,
		},
		Mounts: mounts,
		TTOS: TTOSInspectInfo{
			Container: container, Runtime: family, Branches: branches, RuntimeError: runtimeErr,
		},
	}
}

func (b *runtimeBackend) inspect(name string) (InspectInfo, error) {
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	containerInfo, err := store.LoadContainer(name)
	if err != nil {
		return InspectInfo{}, err
	}
	spec, err := store.LoadSpec(containerInfo.ID)
	if err != nil {
		return InspectInfo{}, err
	}
	families, err := b.listFamilies()
	if err != nil {
		return buildInspectInfo(containerInfo, spec, nil, err.Error()), nil
	}
	var runtimeFamily *Family
	for i := range families {
		if families[i].ID == containerInfo.ID {
			runtimeFamily = &families[i]
			break
		}
	}
	result := buildInspectInfo(containerInfo, spec, runtimeFamily, "")
	for i := range result.TTOS.Branches {
		branch := &result.TTOS.Branches[i]
		if branch.Metadata.State != "running" && branch.Metadata.State != "paused" {
			continue
		}
		processes, err := b.processes(selector{container: containerInfo.ID, branch: branch.Metadata.ID})
		if err != nil {
			branch.ProcessError = err.Error()
			continue
		}
		branch.Processes = processes
	}
	return result, nil
}

func (b *runtimeBackend) remove(name string, force bool) error {
	store := b.containerStore()
	info, err := store.LoadContainer(name)
	if err != nil {
		return err
	}
	runtimeID := info.ID
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("checking runtime state before removing %q: %w", name, err)
	}
	for _, family := range families {
		if family.ID != runtimeID {
			continue
		}
		if !force && family.State != "stopped" && family.State != "exited" {
			return fmt.Errorf("container %q is %s; stop it or use --force", name, family.State)
		}
		args := []string{"delete"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, runtimeID)
		if _, err := b.command(args...); err != nil {
			return fmt.Errorf("deleting runtime container %q: %w", name, err)
		}
		break
	}
	return store.RemoveContainer(runtimeID)
}

func (b *runtimeBackend) configuredRunscArgs() []string {
	return b.configuredRunscArgsForNetwork("none")
}

func (b *runtimeBackend) configuredRunscArgsForNetwork(networkMode string) []string {
	args := []string{
		"--root", b.root,
	}
	if b.unsafeNoHostIsolation {
		args = append(args, "--rootless=false", "--unsafe-no-host-isolation=true")
	} else {
		args = append(args, "--rootless=true")
	}
	args = append(args,
		"--network="+networkMode,
		"--directfs=false",
		"--overlay2=all:memory")
	return args
}

func (b *runtimeBackend) runConfiguredCommand(args ...string) (int, error) {
	return b.runConfiguredCommandForNetwork("none", args...)
}

func (b *runtimeBackend) runConfiguredCommandForNetwork(networkMode string, args ...string) (int, error) {
	args = append(b.configuredRunscArgsForNetwork(networkMode), args...)
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return 1, fmt.Errorf("opening runsc launch stdout: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp("", "ttos-runsc-launch-*.log")
	if err != nil {
		return 1, fmt.Errorf("creating runsc launch stderr: %w", err)
	}
	stderrName := stderr.Name()
	defer os.Remove(stderrName)
	defer stderr.Close()

	command := exec.Command(b.runsc, args...)
	// Use real files rather than the caller streams. The sandbox may inherit
	// these descriptors after runsc exits; an os/exec copy pipe would keep
	// `ttos run --detach` command substitutions and redirected callers open.
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if _, err := stderr.Seek(0, io.SeekStart); err != nil {
		return 1, fmt.Errorf("rewinding runsc launch stderr: %w", err)
	}
	if b.stderr != nil {
		if _, err := io.Copy(b.stderr, stderr); err != nil {
			return 1, fmt.Errorf("replaying runsc launch stderr: %w", err)
		}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("starting runsc: %w", runErr)
	}
	return 0, nil
}

func (b *runtimeBackend) launch(name, bundleDir, networkMode string, detach, interactive, terminal bool) (int, error) {
	status, err := b.runConfiguredCommandForNetwork(networkMode, "create", "--bundle", bundleDir, name)
	if err != nil || status != 0 {
		return status, err
	}
	target := selector{container: name, branch: defaultBranchID}
	startDrain := func(phase string) error {
		if _, err := b.ensureConsoleDrain(target, true); err != nil {
			return fmt.Errorf("starting console drain %s container start: %w", phase, err)
		}
		return nil
	}
	if !terminal {
		if err := startDrain("before"); err != nil {
			_, cleanupErr := b.command("delete", "--force", name)
			if cleanupErr != nil {
				return 1, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
			}
			return 1, err
		}
	}
	status, err = b.runConfiguredCommandForNetwork(networkMode, "start", name)
	if err != nil || status != 0 {
		if err == nil {
			err = fmt.Errorf("runsc start exited with status %d", status)
		}
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return status, fmt.Errorf("starting container: %w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return status, err
	}
	if terminal {
		if err := startDrain("after"); err != nil {
			_, cleanupErr := b.command("delete", "--force", name)
			if cleanupErr != nil {
				return 1, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
			}
			return 1, err
		}
	}
	if detach {
		return 0, nil
	}
	waitStatus, err := b.attach(attachRequest{target: target, stdin: interactive, stdout: true, stderr: true, detachKeys: defaultDetachKeys})
	if err != nil {
		if errors.Is(err, errDetached) {
			return 0, nil
		}
		return 1, err
	}
	return waitStatusExitCode(waitStatus), nil
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
		volumes := make([]string, 0, len(container.Volumes))
		seenVolumes := make(map[string]bool)
		for _, volume := range container.Volumes {
			if !seenVolumes[volume.Name] {
				volumes = append(volumes, volume.Name)
				seenVolumes[volume.Name] = true
			}
		}
		sort.Strings(volumes)
		byID[container.ID] = Family{
			ID:        container.ID,
			Name:      container.Name,
			Image:     container.Reference,
			Digest:    container.Digest,
			State:     state,
			CreatedAt: container.CreatedAt,
			Volumes:   volumes,
			Branches:  []Branch{{ID: defaultBranchID, State: state, CreatedAt: container.CreatedAt}},
		}
	}
	if runtimeErr == nil {
		runtimeIDs := make(map[string]struct{}, len(runtimeFamilies))
		for _, family := range runtimeFamilies {
			runtimeIDs[family.ID] = struct{}{}
			if persistent, ok := byID[family.ID]; ok {
				family.Name = persistent.Name
				family.Image = persistent.Image
				family.Digest = persistent.Digest
				family.CreatedAt = persistent.CreatedAt
				family.Volumes = persistent.Volumes
				for i := range family.Branches {
					if family.Branches[i].ID == defaultBranchID {
						family.Branches[i].CreatedAt = persistent.CreatedAt
					}
				}
			} else if len(family.Branches) != 0 {
				for _, branch := range family.Branches {
					if branch.ID == defaultBranchID {
						family.CreatedAt = branch.CreatedAt
						break
					}
				}
				if family.CreatedAt.IsZero() {
					family.CreatedAt = family.Branches[0].CreatedAt
				}
			}
			byID[family.ID] = family
		}
		for _, container := range containers {
			if container.State == "running" {
				if _, ok := runtimeIDs[container.ID]; ok {
					continue
				}
				_ = store.UpdateState(container.ID, "exited", nil)
				family := byID[container.ID]
				family.State = "exited"
				family.Branches = []Branch{{
					ID: defaultBranchID, State: "exited", CreatedAt: container.CreatedAt,
				}}
				byID[container.ID] = family
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
	sort.Slice(families, func(i, j int) bool {
		left, right := familyDisplayName(families[i]), familyDisplayName(families[j])
		if left != right {
			return left < right
		}
		return families[i].ID < families[j].ID
	})
	return families, nil
}

func (b *runtimeBackend) listRuntimeFamilies() ([]Family, error) {
	output, err := b.command("list", "--format=json", "--sandbox")
	if err != nil {
		return nil, err
	}
	var states []struct {
		ID     string `json:"id"`
		PID    int    `json:"pid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &states); err != nil {
		return nil, fmt.Errorf("decoding runsc container list: %w", err)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	families := make([]Family, 0, len(states))
	for _, state := range states {
		family := Family{ID: state.ID, State: state.Status, SandboxPID: state.PID, Branches: []Branch{}}
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

type runscExecSession struct {
	ID          string    `json:"id"`
	ContainerID string    `json:"container_id"`
	PID         int32     `json:"pid"`
	ConsoleID   string    `json:"console_id"`
	Terminal    bool      `json:"terminal"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	ExitStatus  *uint32   `json:"exit_status,omitempty"`
}

func (b *runtimeBackend) execSessions(target selector) ([]runscExecSession, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	target = resolved
	output, err := b.command("timetravel", target.container, "exec-list", target.branch)
	if err != nil {
		return nil, err
	}
	var sessions []runscExecSession
	if err := json.Unmarshal(output, &sessions); err != nil {
		return nil, fmt.Errorf("decoding exec sessions for %s: %w", target, err)
	}
	return sessions, nil
}

func (b *runtimeBackend) execSession(target selector, execID string) (runscExecSession, error) {
	sessions, err := b.execSessions(target)
	if err != nil {
		return runscExecSession{}, err
	}
	for _, session := range sessions {
		if session.ID == execID {
			return session, nil
		}
	}
	return runscExecSession{}, fmt.Errorf("exec session %q does not exist in %s", execID, target)
}

func (b *runtimeBackend) waitExecSession(target selector, execID string) (uint32, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return 0, err
	}
	target = resolved
	output, err := b.command("timetravel", "--exec-id", execID, target.container, "exec-wait", target.branch)
	if err != nil {
		return 0, err
	}
	var result struct {
		Status uint32 `json:"wait_status"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, fmt.Errorf("decoding exec wait result for %s:%s: %w", target, execID, err)
	}
	return result.Status, nil
}

func (b *runtimeBackend) signalExecSession(target selector, execID string, signal int) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	_, err = b.command("timetravel", "--exec-id", execID, "--signal", strconv.Itoa(signal), target.container, "exec-signal", target.branch)
	return err
}

type runscBranchInfo struct {
	ID          string    `json:"ID"`
	ParentID    string    `json:"ParentID"`
	State       string    `json:"State"`
	CreatedAt   time.Time `json:"CreatedAt"`
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
		CreatedAt:        info.CreatedAt,
		ForkTotalNS:      info.ForkMetrics.Total.Nanoseconds(),
		SourceDowntimeNS: info.ForkMetrics.SourceDowntime.Nanoseconds(),
		KernelForkNS:     info.ForkMetrics.Kernel.Total.Nanoseconds(),
	}
}

func (b *runtimeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	resolved, _, err := b.resolveSelector(source)
	if err != nil {
		return Branch{}, err
	}
	source = resolved
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
	sessions, err := b.execSessions(target)
	if err != nil {
		cleanupErr := b.destroy(target)
		if cleanupErr != nil {
			return Branch{}, fmt.Errorf("listing cloned exec sessions after fork: %w (cleanup also failed: %v)", err, cleanupErr)
		}
		return Branch{}, fmt.Errorf("listing cloned exec sessions after fork: %w; forked branch was destroyed", err)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, true); err != nil {
			cleanupErr := b.destroy(target)
			if cleanupErr != nil {
				return Branch{}, fmt.Errorf("starting cloned exec console drain %q: %w (cleanup also failed: %v)", session.ID, err, cleanupErr)
			}
			return Branch{}, fmt.Errorf("starting cloned exec console drain %q: %w; forked branch was destroyed", session.ID, err)
		}
	}

	return branchView(info), nil
}

func (b *runtimeBackend) pause(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	_, err = b.command("timetravel", target.container, "pause", target.branch)
	return err
}

func (b *runtimeBackend) resume(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	_, err = b.command("timetravel", target.container, "resume", target.branch)
	return err
}

func (b *runtimeBackend) destroy(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	if _, err := b.command("timetravel", target.container, "destroy", target.branch); err == nil {
		return nil
	} else {
		destroyErr := err
		families, listErr := b.listRuntimeFamilies()
		if listErr != nil {
			return destroyErr
		}
		for _, family := range families {
			if family.ID != target.container {
				continue
			}
			// Stopping main tears down the Sentry and all in-memory child
			// branches. Removing one of those branches afterwards is already
			// satisfied even though the control socket no longer exists.
			if family.State == "stopped" || family.State == "exited" {
				return nil
			}
			return destroyErr
		}
		// A known persistent family absent from runsc state has no in-memory
		// branch left to destroy. Do not turn a misspelled container name into
		// an apparent success.
		if b.dataRoot == "" {
			return destroyErr
		}
		if _, err := (ttosbundle.Store{Root: b.dataRoot}).LoadContainer(target.container); err != nil {
			return destroyErr
		}
		return nil
	}
}

func (b *runtimeBackend) processes(target selector) ([]ProcessInfo, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	target = resolved
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
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return ForkEligibility{}, err
	}
	target = resolved
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
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	args := []string{"timetravel", "--signal", strconv.Itoa(signal)}
	if all {
		args = append(args, "--all")
	}
	args = append(args, target.container, "signal", target.branch)
	_, err = b.command(args...)
	return err
}

func (b *runtimeBackend) wait(target selector) (uint32, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return 0, err
	}
	target = resolved
	output, commandErr := b.command("timetravel", target.container, "wait", target.branch)
	if commandErr == nil {
		var result struct {
			Status uint32 `json:"wait_status"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return 0, fmt.Errorf("decoding runsc wait result: %w", err)
		}
		b.recordWaitState(target, result.Status)
		return result.Status, nil
	}

	status, found, err := b.waitForConsoleStatus(target)
	if err != nil {
		return 0, fmt.Errorf("%w; checking persistent wait status: %v", commandErr, err)
	}
	if found {
		b.recordWaitState(target, status)
		return status, nil
	}
	return 0, commandErr
}

func (b *runtimeBackend) recordWaitState(target selector, status uint32) {
	if target.branch != defaultBranchID {
		return
	}
	exitStatus := waitStatusExitCode(status)
	store := ttosbundle.Store{Root: b.dataRoot, Address: b.containerd, Namespace: b.namespace}
	if err := store.UpdateState(target.container, "exited", &exitStatus); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttos: warning: recording exit state for %s: %v\n", target, err)
	}
}

func (b *runtimeBackend) waitForConsoleStatus(target selector) (uint32, bool, error) {
	store := ttosbundle.Store{Root: b.dataRoot}
	paths, err := store.BranchConsolePaths(target.container, target.branch)
	if err != nil {
		return 0, false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, found, err := readConsoleWaitStatus(paths.WaitStatus)
		if err != nil || found {
			return status, found, err
		}
		active, err := consoleDrainActive(paths.Lock)
		if err != nil {
			return 0, false, err
		}
		if !active || time.Now().After(deadline) {
			return 0, false, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func writeTree(w io.Writer, families []Family) {
	for _, family := range families {
		fmt.Fprintf(w, "%s (%s)\n", familyDisplayName(family), family.State)
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
