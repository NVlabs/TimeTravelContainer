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

// Package cmd implements the TimeTravelContainer command-line client.
package cmd

import (
	"archive/tar"
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
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"text/template"
	"time"

	mobysignal "github.com/moby/sys/signal"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/term"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"

	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/processutil"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	defaultBranchID        = "main"
	defaultConsoleLogMax   = int64(16 << 20)
	defaultConsoleLogFiles = 2
)

var (
	defaultDetachKeys = []byte{0x10, 0x11} // Ctrl-P, Ctrl-Q.
	errDetached       = errors.New("detached from container")
)

type options struct {
	root                  string
	runsc                 string
	dataRoot              string
	localRoot             string
	runtimePlatform       string
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	daemonSocket          string
	direct                bool
}

// Run executes ttc with args and returns a process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, args, done, err := parseGlobalOptions(args, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 2
	}
	if done {
		return 0
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	if opts.unsafeNoHostIsolation && args[0] != "daemon" {
		fmt.Fprintln(stderr, "ttc: --unsafe-no-host-isolation configures 'daemon start' only; ordinary commands inherit the running daemon's mode")
		return 2
	}
	if printCustomCommandHelp(args, stdout) {
		return 0
	}
	if len(args) == 2 && args[1] == "--help" {
		return runCommand(args, stdout, stdout, nil)
	}
	if len(args) == 3 && args[2] == "--help" && (args[0] == "container" || args[0] == "image" || args[0] == "volume" || args[0] == "exec") {
		return runCommand(args, stdout, stdout, nil)
	}
	if args[0] == "help" {
		printUsage(stdout)
		return 0
	}
	if args[0] == "completion" {
		return runCommand(args, stdout, stderr, nil)
	}
	if args[0] == "daemon" {
		return runDaemonControl(opts, args[1:], stdout, stderr)
	}
	if args[0] == "cp" {
		args, err = normalizeCopyCommandArgs(args)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
	}
	if args[0] == "checkpoint" || args[0] == "restore" {
		args, err = normalizeCheckpointCommandArgs(args)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
	}
	status, err := runViaDaemon(opts, args, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: daemon transport: %v\n", err)
		return 1
	}
	return status
}

func parseGlobalOptions(args []string, stdout io.Writer) (options, []string, bool, error) {
	opts := options{
		runtimePlatform: runtimePlatformFromEnvironment(),
		daemonSocket:    defaultDaemonSocket(),
	}
	for len(args) != 0 {
		arg := args[0]
		switch {
		case arg == "--help" || arg == "-h":
			printUsage(stdout)
			return opts, nil, true, nil
		case arg == "--version":
			fmt.Fprintf(stdout, "ttc version %s\n", version.Version())
			return opts, nil, true, nil
		case arg == "--daemon-socket":
			if len(args) < 2 || args[1] == "" {
				return opts, nil, false, errors.New("--daemon-socket requires a path")
			}
			opts.daemonSocket = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--daemon-socket="):
			opts.daemonSocket = strings.TrimPrefix(arg, "--daemon-socket=")
			if opts.daemonSocket == "" {
				return opts, nil, false, errors.New("--daemon-socket requires a path")
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

func currentExecutable() string {
	executable, _ := os.Executable()
	return executable
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: ttc [GLOBAL OPTIONS] COMMAND

Manage TimeTravelContainer container families and live branches.

Global options:
  --daemon-socket PATH           per-user ttcd Unix socket
  --unsafe-no-host-isolation     disable host isolation for daemon start

Commands:
  images [OPTIONS]               List local images
  image COMMAND                  Manage local images (ls, pull, load, save, inspect, rm)
  container COMMAND              Docker-style container command aliases
  rmi [OPTIONS] IMAGE...         Remove one or more local images
  pull [OPTIONS] IMAGE           Pull an OCI/Docker image into local content
  load [OPTIONS]                 Load images from a tar archive or standard input
  save [OPTIONS] IMAGE...        Save images to a tar archive or standard output
  commit [OPTIONS] TARGET IMAGE  Commit a branch rootfs as a new OCI image
  run [--node NODE] [OPTIONS] IMAGE [COMMAND] Pull, create, and run a container
  create [OPTIONS] IMAGE [CMD]   Prepare a container without starting it
  start [--node NODE] [OPTIONS] TARGET Start a prepared or exited branch
  attach [OPTIONS] TARGET        Attach to a branch init or managed exec console
  logs [OPTIONS] TARGET          Print or follow init or managed exec logs
  exec [OPTIONS] TARGET COMMAND  Execute a command in a running branch
  exec ls|wait|kill ...          Manage detached exec sessions
  cp [OPTIONS] SRC_PATH DEST_PATH Copy files between host and a container branch
  diff [--json] TARGET           Show paths changed in a branch's writable layer
  inspect [OPTIONS] CONTAINER... Show Docker and TTC branch/process state
  events [OPTIONS]               Stream durable manager lifecycle events
  wait [OPTIONS] TARGET...       Wait for branches and print exit codes
  restart [OPTIONS] TARGET...    Restart one or more branches
  rm [--force] TARGET...         Remove branches or whole container families
  ls [--tree|--json]              List container families and branch trees
  ps [OPTIONS]                    List and filter container branches
  top [--json] TARGET [PS OPTS]   Show processes in one live branch
  stats [--json] TARGET           Show branch/family CoW resource accounting
  tty stat|watch TARGET --session NAME Observe or stream a named terminal
  fork [--node NODE] [--paused] SOURCE [DEST...] Fork SOURCE into live branches
  migrate --node NODE TARGET     Move a running branch to another cluster node
  node ls [--json]                 List elastic cluster nodes and memory load
  node stop [POLICY] NODE         Evacuate a node and stop its daemon
  node rebalance [--node NODE] NODE Relieve a node's configured memory pressure
  checkpoint TARGET FILE          Save one branch as a standalone checkpoint
  restore [--name NAME] FILE      Restore a checkpoint as a new container
  rename CONTAINER@BRANCH NAME    Give a branch another name
  pause TARGET...                 Pause one or more branches
  resume TARGET...                Resume one or more branches
  quarantine TARGET...            Disconnect branches from external networking
  unquarantine TARGET...          Reconnect branches to external networking
  dehydrate TARGET...             Dehydrate branches or whole container families
  rehydrate TARGET...             Reconstruct one or more dehydrated branches
  unpause TARGET...               Resume branches (Docker alias)
  kill [OPTIONS] TARGET...        Send a signal to branch init processes
  stop [OPTIONS] TARGET...        Stop branches or whole container families
  port COMMAND                    Publish, list, or unpublish branch TCP ports
  volume COMMAND                  Manage persistent volumes (create, ls, inspect, rm)
  completion SHELL                 Generate Bash, Zsh, or Fish completion
  daemon start [--platform systrap|kvm] [--unsafe-no-host-isolation]
               [--cluster-init | --cluster-join HOST:PORT]
               [--node NAME] [--listen ADDR] [--advertise HOST:PORT]
               [--memory-target SIZE]
                                  Start the per-user service in one fixed mode
  daemon status|stop              Inspect or stop the per-user service
  daemon requests [OPTIONS]        List durable mutation requests
  daemon request inspect|resolve   Inspect or resolve an interrupted mutation

Branch selectors must use CONTAINER@BRANCH. Local fork destinations are branch
names in the source family. Forked branches run immediately unless --paused is
set; omitting DEST generates a branch ID.
`)
}

func printCustomCommandHelp(args []string, stdout io.Writer) bool {
	if len(args) == 2 && args[1] == "--help" {
		usage := map[string]string{
			"container":    "Usage: ttc container COMMAND",
			"image":        "Usage: ttc image ls|pull|load|save|inspect|rm [OPTIONS]",
			"load":         "Usage: ttc load [--input FILE] [--platform PLATFORM] [--quiet]",
			"save":         "Usage: ttc save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
			"commit":       "Usage: ttc commit [-a AUTHOR] [-c CHANGE] [-m MESSAGE] [-p=true|false] CONTAINER@BRANCH REPOSITORY[:TAG]",
			"port":         "Usage: ttc port publish|ls|unpublish [OPTIONS]",
			"completion":   "Usage: ttc completion bash|zsh|fish",
			"daemon":       "Usage: ttc daemon start [--cluster-init|--cluster-join HOST:PORT] [OPTIONS] | status | stop [--migrate-all|--dehydrate-all|--stop-all]",
			"events":       "Usage: ttc events [--json] [-f|--follow] [--since TIME] [--until TIME] [--filter KEY=VALUE]",
			"pause":        "Usage: ttc pause TARGET [TARGET...]",
			"rename":       "Usage: ttc rename CONTAINER@BRANCH NEW_BRANCH",
			"resume":       "Usage: ttc resume TARGET [TARGET...]",
			"quarantine":   "Usage: ttc quarantine CONTAINER@BRANCH [CONTAINER@BRANCH...]",
			"unquarantine": "Usage: ttc unquarantine CONTAINER@BRANCH [CONTAINER@BRANCH...]",
			"dehydrate":    "Usage: ttc dehydrate TARGET [TARGET...]",
			"rehydrate":    "Usage: ttc rehydrate TARGET [TARGET...]",
			"unpause":      "Usage: ttc unpause TARGET [TARGET...]",
			"top":          "Usage: ttc top [--json] CONTAINER@BRANCH [PS OPTIONS]",
			"stats":        "Usage: ttc stats [--json] CONTAINER@BRANCH",
			"tty":          "Usage: ttc tty stat|watch CONTAINER@BRANCH --session NAME [--json]",
			"cp":           "Usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH",
			"checkpoint":   "Usage: ttc checkpoint [--compression none|flate-best-speed] CONTAINER@BRANCH FILE",
			"migrate":      "Usage: ttc migrate --node NODE CONTAINER@BRANCH",
			"node":         "Usage: ttc node ls [--json] | ttc node stop [--stop-all|--dehydrate-all|--migrate-all] NODE | ttc node rebalance [--node NODE] NODE",
			"restore":      "Usage: ttc restore [--node NODE] [--name NAME] FILE",
		}
		if text, ok := usage[args[0]]; ok {
			fmt.Fprintln(stdout, text)
			return true
		}
	}
	if len(args) == 3 && args[2] == "--help" {
		usage := map[string]string{
			"container pause":   "Usage: ttc container pause CONTAINER@BRANCH [CONTAINER@BRANCH...]",
			"container unpause": "Usage: ttc container unpause CONTAINER@BRANCH [CONTAINER@BRANCH...]",
			"container top":     "Usage: ttc container top CONTAINER@BRANCH [PS OPTIONS]",
			"image inspect":     "Usage: ttc image inspect IMAGE [IMAGE...]",
			"image load":        "Usage: ttc image load [--input FILE] [--platform PLATFORM] [--quiet]",
			"image save":        "Usage: ttc image save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
			"port publish":      "Usage: ttc port publish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",
			"port ls":           "Usage: ttc port ls [--json] CONTAINER@BRANCH",
			"port unpublish":    "Usage: ttc port unpublish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",
			"exec wait":         "Usage: ttc exec wait CONTAINER@BRANCH:EXEC_ID",
		}
		if text, ok := usage[args[0]+" "+args[1]]; ok {
			fmt.Fprintln(stdout, text)
			return true
		}
	}
	return false
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
	case "load":
		err = runImageLoad(args[1:], stdout, stderr, backend)
	case "save":
		err = runImageSave(args[1:], stdout, stderr, backend)
	case "commit":
		err = runCommit(args[1:], stdout, stderr, backend)
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
	case "cp":
		err = runCopy(args[1:], stdout, stderr, backend)
	case "diff":
		err = runDiff(args[1:], stdout, stderr, backend)
	case "stats":
		err = runStats(args[1:], stdout, stderr, backend)
	case "_stats-local":
		err = runStats(args[1:], stdout, stderr, backend)
	case "inspect":
		err = runInspect(args[1:], stdout, stderr, backend)
	case "rm":
		err = runRemove(args[1:], stdout, stderr, backend)
	case "_auto-remove-now":
		err = runAutoRemoveNow(args[1:], backend)
	case "_purge-runtime":
		if len(args) != 2 {
			err = errors.New("internal runtime purge requires one family ID")
			break
		}
		purger, ok := backend.(interface{ purgeRuntime(string) error })
		if !ok {
			err = errors.New("this backend does not support runtime purge")
			break
		}
		err = purger.purgeRuntime(args[1])
	case "ls":
		err = runList(args[1:], stdout, stderr, backend)
	case "ps":
		err = runPs(args[1:], stdout, stderr, backend)
	case "top":
		err = runTop(args[1:], stdout, stderr, backend)
	case "tty":
		err = runTTY(args[1:], stdout, backend)
	case "fork":
		err = runFork(args[1:], stdout, stderr, backend)
	case "migrate":
		err = runMigrate(args[1:], stdout, backend)
	case "node":
		err = runNode(args[1:], stdout, backend)
	case "checkpoint":
		err = runCheckpoint(args[1:], stdout, backend)
	case "restore":
		err = runRestoreCheckpoint(args[1:], stdout, backend)
	case "rename":
		err = runRename(args[1:], stdout, backend)
	case "pause":
		err = runPauseResume(args[1:], stdout, backend, true)
	case "resume", "unpause":
		err = runPauseResume(args[1:], stdout, backend, false)
	case "quarantine":
		err = runQuarantine(args[1:], stdout, backend, true)
	case "unquarantine":
		err = runQuarantine(args[1:], stdout, backend, false)
	case "dehydrate":
		err = runDehydrateRehydrate(args[1:], stdout, backend, true)
	case "rehydrate":
		err = runDehydrateRehydrate(args[1:], stdout, backend, false)
	case "kill":
		err = runKill(args[1:], stdout, stderr, backend)
	case "stop":
		err = runStop(args[1:], stdout, stderr, backend)
	case "wait":
		err = runWait(args[1:], stdout, stderr, backend)
	case "restart":
		err = runRestart(args[1:], stdout, stderr, backend)
	case "port":
		err = runPort(args[1:], stdout, stderr, backend)
	case "volume":
		err = runVolume(args[1:], stdout, backend)
	case "completion":
		err = runCompletion(args[1:], stdout)
	case "_complete":
		err = runDynamicCompletion(args[1:], stdout, backend)
	case "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ttc: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var status exitStatus
		if errors.As(err, &status) {
			return status.code
		}
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 1
	}
	return 0
}

type elasticMigrationBackend interface {
	migrate(selector, string) error
}

type elasticNodeBackend interface {
	clusterNodes() ([]cluster.NodeRecord, error)
	stopNode(sourceNode, mode string, local bool) ([]selector, error)
	rebalanceNode(sourceNode, targetNode string, local bool) (cluster.FixedTerminalPlan, error)
	plannerSnapshot(node string, local bool) (cluster.PlannerSnapshot, error)
}

func runNode(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc node ls [--json] | ttc node stop [--stop-all|--dehydrate-all|--migrate-all] NODE | ttc node rebalance NODE")
	}
	nodes, ok := backend.(elasticNodeBackend)
	if !ok {
		return errors.New("backend does not support elastic cluster nodes")
	}
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("node ls", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOutput := fs.Bool("json", false, "write JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: ttc node ls [--json]")
		}
		records, err := nodes.clusterNodes()
		if err != nil {
			return err
		}
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(records)
		}
		writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NODE\tSTATE\tMEMORY\tTARGET\tRESERVED\tLEASE")
		now := time.Now()
		for _, record := range records {
			lease := "expired"
			if remaining := record.LeaseExpiresAt.Sub(now); remaining > 0 {
				lease = remaining.Round(time.Second).String()
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", record.NodeID, record.State,
				formatImageSize(int64(record.MemoryCurrentBytes)), formatImageSize(int64(record.MemoryTargetBytes)),
				formatImageSize(int64(record.IncomingReservedBytes)), lease)
		}
		return writer.Flush()
	case "stop":
		var sourceNode string
		var stopAll, dehydrateAll, migrateAll, local bool
		for _, arg := range args[1:] {
			switch arg {
			case "--stop-all":
				stopAll = true
			case "--dehydrate-all":
				dehydrateAll = true
			case "--migrate-all":
				migrateAll = true
			case "--_local":
				local = true
			default:
				if strings.HasPrefix(arg, "-") {
					return fmt.Errorf("unknown node stop option %q", arg)
				}
				if sourceNode != "" {
					return errors.New("usage: ttc node stop NODE [--stop-all|--dehydrate-all|--migrate-all]")
				}
				sourceNode = arg
			}
		}
		if sourceNode == "" {
			return errors.New("usage: ttc node stop [--stop-all|--dehydrate-all|--migrate-all] NODE")
		}
		selected := 0
		for _, enabled := range []bool{stopAll, dehydrateAll, migrateAll} {
			if enabled {
				selected++
			}
		}
		if selected > 1 {
			return errors.New("--stop-all, --dehydrate-all, and --migrate-all are mutually exclusive")
		}
		mode := ""
		switch {
		case stopAll:
			mode = "stop"
		case dehydrateAll:
			mode = "dehydrate"
		case migrateAll:
			mode = "migrate"
		}
		moved, err := nodes.stopNode(sourceNode, mode, local)
		if err != nil {
			return err
		}
		for _, branch := range moved {
			fmt.Fprintln(stdout, branch)
		}
		return nil
	case "rebalance":
		fs := flag.NewFlagSet("node rebalance", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		local := fs.Bool("_local", false, "execute only on the addressed daemon")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: ttc node rebalance NODE")
		}
		plan, err := nodes.rebalanceNode(fs.Arg(0), "", *local)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(plan)
	case "_planner-resources":
		fs := flag.NewFlagSet("node _planner-resources", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		local := fs.Bool("_local", false, "execute only on the addressed daemon")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: ttc node _planner-resources --_local NODE")
		}
		snapshot, err := nodes.plannerSnapshot(fs.Arg(0), *local)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(snapshot)
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}

func runMigrate(args []string, stdout io.Writer, backend backend) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	targetNode := fs.String("node", "", "destination cluster node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *targetNode == "" || fs.NArg() != 1 {
		return errors.New("usage: ttc migrate --node NODE CONTAINER@BRANCH")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	migration, ok := backend.(elasticMigrationBackend)
	if !ok {
		return errors.New("backend does not support elastic migration")
	}
	if err := migration.migrate(target, *targetNode); err != nil {
		return err
	}
	fmt.Fprintln(stdout, target)
	return nil
}

func runContainerNamespace(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc container COMMAND [OPTIONS]")
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

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
func (s *stringSlice) Get() any { return s }
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
	host       []ttcbundle.HostMount
	filesystem []ttcbundle.FilesystemMount
	managed    []ttcbundle.ManagedVolume
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
		if len(parts) == 1 {
			if parts[0] == "" {
				return mountRequests{}, fmt.Errorf("invalid volume specification %q", value)
			}
			request.managed = append(request.managed, ttcbundle.ManagedVolume{Destination: parts[0], Anonymous: true})
			continue
		}
		if len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return mountRequests{}, fmt.Errorf("invalid volume specification %q", value)
		}
		if isHostMountSource(parts[0]) {
			hostVolumes = append(hostVolumes, value)
			continue
		}
		volume := ttcbundle.ManagedVolume{Name: parts[0], Destination: parts[1]}
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro", "readonly":
					volume.Readonly = true
				case "rw", "":
					volume.Readonly = false
				case "nocopy", "volume-nocopy":
					volume.NoCopy = true
				default:
					return mountRequests{}, fmt.Errorf("unsupported managed-volume option %q in %q", option, value)
				}
			}
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
			volume, err := parseManagedVolumeLongMount(value)
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

func parseManagedVolumeLongMount(value string) (ttcbundle.ManagedVolume, error) {
	var volume ttcbundle.ManagedVolume
	seen := make(map[string]bool)
	for _, field := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(strings.TrimSpace(field), "=")
		canonical := key
		switch key {
		case "src":
			canonical = "source"
		case "dst":
			canonical = "target"
		case "readonly":
			canonical = "ro"
		}
		if seen[canonical] {
			return ttcbundle.ManagedVolume{}, fmt.Errorf("duplicate --mount field %q in %q", key, value)
		}
		seen[canonical] = true
		if !found {
			switch canonical {
			case "ro":
				volume.Readonly = true
			case "volume-nocopy":
				volume.NoCopy = true
			default:
				return ttcbundle.ManagedVolume{}, fmt.Errorf("invalid --mount field %q in %q", field, value)
			}
			continue
		}
		raw = strings.TrimSpace(raw)
		switch canonical {
		case "type":
			if raw != "volume" {
				return ttcbundle.ManagedVolume{}, fmt.Errorf("--mount %q is not a volume", value)
			}
		case "source":
			volume.Name = raw
		case "target":
			volume.Destination = raw
		case "ro":
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return ttcbundle.ManagedVolume{}, err
			}
			volume.Readonly = parsed
		case "volume-nocopy":
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return ttcbundle.ManagedVolume{}, err
			}
			volume.NoCopy = parsed
		default:
			return ttcbundle.ManagedVolume{}, fmt.Errorf("unsupported --mount field %q in %q", key, value)
		}
	}
	if volume.Destination == "" {
		return ttcbundle.ManagedVolume{}, fmt.Errorf("managed volume mount %q requires target", value)
	}
	if volume.Name == "" {
		volume.Anonymous = true
	}
	return volume, nil
}

func managedVolumesUnavailable() error {
	return errors.New("managed volume syntax was used where a host bind mount was required")
}

func parseTmpfsMount(value string) (ttcbundle.FilesystemMount, error) {
	destination, rawOptions, hasOptions := strings.Cut(value, ":")
	if destination == "" || (hasOptions && rawOptions == "") {
		return ttcbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs specification %q; want CONTAINER[:OPTIONS]", value)
	}
	readonly := false
	options := []string{}
	if hasOptions {
		for _, option := range strings.Split(rawOptions, ",") {
			option = strings.TrimSpace(option)
			key, raw, found := strings.Cut(option, "=")
			if found {
				if err := validateTmpfsDataOption(key, raw); err != nil {
					return ttcbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs option %q in %q: %w", option, value, err)
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
				return ttcbundle.FilesystemMount{}, fmt.Errorf("unsupported tmpfs option %q in %q", option, value)
			}
		}
	}
	mount, err := normalizeFilesystemMount("tmpfs", destination, readonly, options)
	if err != nil {
		return ttcbundle.FilesystemMount{}, err
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

func parseFilesystemLongMount(value string) (ttcbundle.FilesystemMount, error) {
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
					return ttcbundle.FilesystemMount{}, fmt.Errorf("duplicate --mount field %q in %q", key, value)
				}
				seen["readonly"] = struct{}{}
				readonly = true
				readonlySet = true
				continue
			default:
				return ttcbundle.FilesystemMount{}, fmt.Errorf("invalid --mount field %q in %q", field, value)
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
			return ttcbundle.FilesystemMount{}, fmt.Errorf("duplicate --mount field %q in %q", key, value)
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
				return ttcbundle.FilesystemMount{}, fmt.Errorf("invalid readonly value %q in %q", raw, value)
			}
			readonly = parsed
			readonlySet = true
		case "tmpfs-size":
			size = raw
		case "tmpfs-mode":
			mode = raw
		case "source":
			return ttcbundle.FilesystemMount{}, fmt.Errorf("filesystem mount %q must not specify a source", value)
		default:
			return ttcbundle.FilesystemMount{}, fmt.Errorf("unsupported filesystem-mount field %q in %q", key, value)
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
				return ttcbundle.FilesystemMount{}, fmt.Errorf("tmpfs-size %q in %q must be an unsigned byte count", size, value)
			}
			options = append(options, "size="+strconv.FormatUint(parsed, 10))
		}
		if mode != "" {
			normalized, err := normalizeTmpfsMode(mode)
			if err != nil {
				return ttcbundle.FilesystemMount{}, fmt.Errorf("invalid tmpfs-mode in %q: %w", value, err)
			}
			options = append(options, "mode="+normalized)
		}
	} else if size != "" || mode != "" {
		return ttcbundle.FilesystemMount{}, fmt.Errorf("%s mount %q does not support tmpfs options", mountType, value)
	} else {
		options = []string{"nosuid", "nodev", "noexec"}
	}
	return normalizeFilesystemMount(mountType, destination, readonly, options)
}

func normalizeFilesystemMount(mountType, destination string, readonly bool, options []string) (ttcbundle.FilesystemMount, error) {
	if mountType != "tmpfs" && mountType != "proc" && mountType != "sysfs" {
		return ttcbundle.FilesystemMount{}, fmt.Errorf("unsupported filesystem mount type %q", mountType)
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == "/" {
		return ttcbundle.FilesystemMount{}, fmt.Errorf("%s mount destination %q must be an absolute non-root path", mountType, destination)
	}
	access := "rw"
	if readonly {
		access = "ro"
	}
	return ttcbundle.FilesystemMount{
		Type: mountType, Destination: filepath.Clean(destination), Readonly: readonly,
		Options: append([]string{access}, options...),
	}, nil
}

func isHostMountSource(source string) bool {
	return filepath.IsAbs(source) || source == "." || strings.HasPrefix(source, "."+string(filepath.Separator))
}

func parseHostMounts(volumeSpecs, mountSpecs []string) ([]ttcbundle.HostMount, error) {
	mounts := make([]ttcbundle.HostMount, 0, len(volumeSpecs)+len(mountSpecs))
	for _, value := range volumeSpecs {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid volume specification %q; want HOST:CONTAINER[:ro|rw|copy]", value)
		}
		readonly := false
		immutableSource := false
		managedCoW := false
		copySource := false
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro", "readonly":
					readonly = true
				case "rw":
					readonly = false
				case "immutable-source":
					immutableSource = true
				case "cow", "managed-cow":
					managedCoW = true
				case "copy":
					copySource = true
				case "":
				default:
					return nil, fmt.Errorf("unsupported volume option %q in %q", option, value)
				}
			}
		}
		mount, err := normalizeHostMount(parts[0], parts[1], readonly, immutableSource, managedCoW, copySource)
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
		immutableSource := false
		managedCoW := false
		copySource := false
		seen := make(map[string]struct{})
		for _, field := range strings.Split(value, ",") {
			key, raw, found := strings.Cut(field, "=")
			key = strings.TrimSpace(key)
			if !found {
				switch key {
				case "ro", "readonly":
					readonly = true
					continue
				case "immutable-source":
					immutableSource = true
					continue
				case "cow", "managed-cow":
					managedCoW = true
					continue
				case "copy":
					copySource = true
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
			case "immutable-source":
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid immutable-source value %q in %q", raw, value)
				}
				immutableSource = parsed
			case "cow", "managed-cow":
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid managed-cow value %q in %q", raw, value)
				}
				managedCoW = parsed
			case "copy":
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid copy value %q in %q", raw, value)
				}
				copySource = parsed
			default:
				return nil, fmt.Errorf("unsupported --mount field %q in %q", key, value)
			}
		}
		if mountType != "bind" {
			if mountType == "volume" || (mountType == "" && source != "" && !filepath.IsAbs(source)) {
				return nil, managedVolumesUnavailable()
			}
			return nil, fmt.Errorf("--mount %q must specify type=bind", value)
		}
		mount, err := normalizeHostMount(source, destination, readonly, immutableSource, managedCoW, copySource)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

func normalizeHostMount(source, destination string, readonly, immutableSource, managedCoW, copySource bool) (ttcbundle.HostMount, error) {
	if source == "" || destination == "" {
		return ttcbundle.HostMount{}, errors.New("host bind mount requires both source and destination")
	}
	if !filepath.IsAbs(source) {
		if source != "." && !strings.HasPrefix(source, "."+string(filepath.Separator)) {
			return ttcbundle.HostMount{}, managedVolumesUnavailable()
		}
		absolute, err := filepath.Abs(source)
		if err != nil {
			return ttcbundle.HostMount{}, fmt.Errorf("resolving host mount source %q: %w", source, err)
		}
		source = absolute
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == "/" {
		return ttcbundle.HostMount{}, fmt.Errorf("host mount destination %q must be an absolute non-root path", destination)
	}
	if readonly && (managedCoW || copySource) {
		return ttcbundle.HostMount{}, fmt.Errorf("host bind %q:%q cannot combine read-only with cow or copy", source, destination)
	}
	if managedCoW && copySource {
		return ttcbundle.HostMount{}, fmt.Errorf("host bind %q:%q cannot combine cow and copy", source, destination)
	}
	if !readonly && !copySource {
		managedCoW = true
	}
	return ttcbundle.HostMount{
		Source: filepath.Clean(source), Destination: filepath.Clean(destination),
		Readonly: readonly, ImmutableSource: immutableSource, ManagedCoW: managedCoW, Copy: copySource,
	}, nil
}

func parseNetworkMode(value string) (string, error) {
	switch value {
	case "none", "host", "ttc":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q; supported modes are none, host, and ttc", value)
	}
}

func liveForkWaiverReason(waived bool, networkMode string) string {
	if !waived {
		return ""
	}
	if networkMode == "host" {
		return "explicit waiver for host networking"
	}
	return "explicitly waived during container creation"
}

func validateLiveForkCreationPolicy(networkMode string, waived bool) error {
	if networkMode == "host" && !waived {
		return errors.New("--network=host is incompatible with live fork eligibility; pass --waive-live-fork to create a systematically non-forkable family")
	}
	return nil
}

func validateVirtualNICOptions(networkMode, ipValue, macValue string) error {
	if (ipValue != "" || macValue != "") && networkMode != "ttc" {
		return errors.New("--ip and --mac-address require --network=ttc")
	}
	if ipValue != "" {
		ip, err := netip.ParseAddr(ipValue)
		if err != nil || !ip.Is4() || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("--ip must be a unicast IPv4 address, got %q", ipValue)
		}
		last := ip.As4()[3]
		if last == 0 || last == 2 || last == 255 {
			return fmt.Errorf("--ip %q conflicts with the /24 network, gateway, or broadcast address", ipValue)
		}
	}
	if macValue != "" {
		mac, err := net.ParseMAC(macValue)
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
			return fmt.Errorf("--mac-address must be a unicast 48-bit MAC address, got %q", macValue)
		}
	}
	return nil
}

func parsePublishedPorts(values []string) ([]ttcbundle.PortBinding, error) {
	bindings := make([]ttcbundle.PortBinding, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		original := value
		protocol := "tcp"
		if before, after, ok := strings.Cut(value, "/"); ok {
			value, protocol = before, strings.ToLower(after)
		}
		if protocol != "tcp" {
			return nil, fmt.Errorf("published port %q uses unsupported protocol %q; only tcp is currently supported", original, protocol)
		}
		parts := strings.Split(value, ":")
		hostIP := "0.0.0.0"
		if len(parts) == 3 {
			hostIP = parts[0]
			parts = parts[1:]
		}
		if len(parts) != 2 || hostIP == "" {
			return nil, fmt.Errorf("invalid published port %q; use [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]", original)
		}
		address, err := netip.ParseAddr(hostIP)
		if err != nil || !address.Is4() {
			return nil, fmt.Errorf("published port %q has invalid IPv4 host address %q", original, hostIP)
		}
		parsePort := func(text, role string) (uint16, error) {
			port, err := strconv.ParseUint(text, 10, 16)
			if err != nil || port == 0 {
				return 0, fmt.Errorf("published port %q has invalid %s port %q", original, role, text)
			}
			return uint16(port), nil
		}
		hostPort, err := parsePort(parts[0], "host")
		if err != nil {
			return nil, err
		}
		containerPort, err := parsePort(parts[1], "container")
		if err != nil {
			return nil, err
		}
		key := netip.AddrPortFrom(address, hostPort).String() + "/" + protocol
		if seen[key] {
			return nil, fmt.Errorf("published host endpoint %s is specified more than once", key)
		}
		seen[key] = true
		bindings = append(bindings, ttcbundle.PortBinding{HostIP: address.String(), HostPort: hostPort, ContainerPort: containerPort, Protocol: protocol})
	}
	return bindings, nil
}

func parseLogDriver(value string, options []string) (string, error) {
	switch value {
	case "local", "json-file":
		return value, nil
	case "none":
		if len(options) != 0 {
			return "", errors.New("logging options are not supported with the none log driver")
		}
		return value, nil
	default:
		return "", fmt.Errorf("unsupported log driver %q; supported drivers are local, json-file, and none", value)
	}
}

func parseLogOptions(values []string) (int64, int, map[string]string, error) {
	config := make(map[string]string)

	maxBytes := defaultConsoleLogMax
	maxFiles := defaultConsoleLogFiles
	for _, option := range values {
		key, value, found := strings.Cut(option, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return 0, 0, nil, fmt.Errorf("invalid --log-opt %q; want KEY=VALUE", option)
		}
		switch key {
		case "max-size":
			parsed, err := parseLogSize(value)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("invalid --log-opt max-size=%q: %w", value, err)
			}
			maxBytes = parsed
		case "max-file":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 {
				return 0, 0, nil, fmt.Errorf("invalid --log-opt max-file=%q: must be a positive integer", value)
			}
			maxFiles = parsed
		case "labels", "label", "env":
			for _, name := range strings.Split(value, ",") {
				if strings.TrimSpace(name) == "" {
					return 0, 0, nil, fmt.Errorf("invalid --log-opt %s=%q: attribute names must not be empty", key, value)
				}
			}
			config[key] = value
		case "labels-regex", "label-regex", "env-regex":
			if _, err := regexp.Compile(value); err != nil {
				return 0, 0, nil, fmt.Errorf("invalid --log-opt %s=%q: %w", key, value, err)
			}
			config[key] = value
		default:
			return 0, 0, nil, fmt.Errorf("unsupported --log-opt %q; supported options are max-size, max-file, labels, labels-regex, env, and env-regex", key)
		}
	}
	return maxBytes, maxFiles, config, nil
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

func parseNanoCPUs(value string) (int64, error) {
	cpus, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus < 0 {
		return 0, fmt.Errorf("invalid CPU limit %q", value)
	}
	if cpus > float64(math.MaxInt64)/1_000_000_000 {
		return 0, fmt.Errorf("CPU limit %q is too large", value)
	}
	nano := int64(math.Round(cpus * 1_000_000_000))
	if nano > 0 && nano < 10_000_000 {
		return 0, fmt.Errorf("CPU limit %q is below Docker minimum 0.01", value)
	}
	return nano, nil
}

type runRequest struct {
	name               string
	reference          string
	pullPolicy         string
	args               []string
	env                []string
	labels             map[string]string
	cwd                string
	user               string
	hostname           string
	networkMode        string
	liveForkEligible   bool
	liveForkWaiver     string
	networkNAT         bool
	networkIP          string
	networkMAC         string
	publishedPorts     []ttcbundle.PortBinding
	logDriver          string
	extraHosts         []string
	dns                []string
	dnsSearch          []string
	dnsOptions         []string
	entrypoint         []string
	hostMounts         []ttcbundle.HostMount
	filesystemMounts   []ttcbundle.FilesystemMount
	managedVolumes     []ttcbundle.ManagedVolume
	consoleLogMaxBytes int64
	consoleLogMaxFiles int
	logOptions         map[string]string
	autoRemove         bool
	readonlyRootfs     bool
	stopSignal         string
	stopTimeout        int
	pidsLimit          *int64
	memoryLimit        *int64
	nanoCPUs           *int64
	cpuShares          *uint64
	cpuPeriod          *uint64
	cpuQuota           *int64
	ulimits            []ttcbundle.Ulimit
	shmSize            int64
	groupAdd           []string
	capAdd             []string
	capDrop            []string
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
	args = expandCombinedShortBools(args, "dit", "--name", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "-h", "--network", "--net", "--ip", "--mac-address", "--publish", "-p", "--add-host", "--dns", "--dns-search", "--dns-option", "--dns-opt", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--memory", "-m", "--cpus", "--cpu-shares", "--cpu-period", "--cpu-quota", "--shm-size", "--group-add", "--cap-add", "--cap-drop", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-driver", "--log-opt")
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
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
	stopTimeout := fs.Int("stop-timeout", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait before killing the container; -1 waits forever")
	pidsLimit := fs.Int64("pids-limit", 0, "tune the container task limit; -1 or 0 means unlimited")
	memoryValue := fs.String("memory", "0", "memory limit")
	fs.StringVar(memoryValue, "m", "0", "memory limit")
	cpusValue := fs.String("cpus", "0", "number of CPUs")
	cpuShares := fs.Uint64("cpu-shares", 0, "CPU shares (relative weight)")
	cpuPeriod := fs.Uint64("cpu-period", 0, "limit CPU CFS period")
	cpuQuota := fs.Int64("cpu-quota", 0, "limit CPU CFS quota; -1 means unlimited")
	shmSizeValue := fs.String("shm-size", "64m", "size of /dev/shm")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	fs.StringVar(hostname, "h", "", "container hostname")
	networkModeValue := fs.String("network", "none", "network mode: none, host, or ttc")
	waiveLiveFork := fs.Bool("waive-live-fork", false, "explicitly create a family that is not eligible for live fork")
	networkNAT := fs.Bool("nat", false, "allow outbound host networking from a ttc virtual NIC")
	networkIP := fs.String("ip", "", "IPv4 address for the ttc virtual NIC")
	networkMAC := fs.String("mac-address", "", "MAC address for the ttc virtual NIC")
	logDriverValue := fs.String("log-driver", ttcbundle.DefaultLogDriver, "logging driver: local, json-file, or none")
	fs.StringVar(networkModeValue, "net", "none", "network mode: none, host, or ttc")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env, envFiles, labels, labelFiles, publishedPortValues stringSlice
	fs.Var(&publishedPortValues, "publish", "publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
	fs.Var(&publishedPortValues, "p", "publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
	fs.Var(&env, "env", "set or import environment variable NAME[=VALUE]")
	fs.Var(&env, "e", "set or import environment variable NAME[=VALUE]")
	fs.Var(&envFiles, "env-file", "read environment variables from a file")
	fs.Var(&labels, "label", "set container metadata KEY[=VALUE]")
	fs.Var(&labels, "l", "set container metadata KEY[=VALUE]")
	fs.Var(&labelFiles, "label-file", "read container labels from a file")
	var volumes, mounts, tmpfsMounts stringSlice
	fs.Var(&volumes, "volume", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy]")
	fs.Var(&volumes, "v", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy]")
	fs.Var(&mounts, "mount", "attach type=bind|tmpfs|proc|sysfs,target=CONTAINER[,OPTIONS]")
	fs.Var(&tmpfsMounts, "tmpfs", "mount tmpfs at CONTAINER[:OPTIONS]")
	var logOptions, ulimitValues, groupAddValues, capAddValues, capDropValues stringSlice
	var extraHostValues, dnsValues, dnsSearchValues, dnsOptionValues stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option (max-size, max-file, labels, labels-regex, env, or env-regex)")
	fs.Var(&ulimitValues, "ulimit", "set resource limit NAME=SOFT[:HARD]")
	fs.Var(&groupAddValues, "group-add", "add an additional group by name or GID")
	fs.Var(&capAddValues, "cap-add", "add a Linux capability")
	fs.Var(&capDropValues, "cap-drop", "drop a Linux capability")
	fs.Var(&extraHostValues, "add-host", "add a custom host-to-IP mapping")
	fs.Var(&dnsValues, "dns", "set a custom DNS server")
	fs.Var(&dnsSearchValues, "dns-search", "set a custom DNS search domain")
	fs.Var(&dnsOptionValues, "dns-option", "set a DNS resolver option")
	fs.Var(&dnsOptionValues, "dns-opt", "set a DNS resolver option")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttc run [OPTIONS] IMAGE [COMMAND...]")
	}
	if *stopTimeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	if *pidsLimit < -1 {
		return errors.New("pids limit must be -1 or greater")
	}
	if *cpuShares != 0 && (*cpuShares < 2 || *cpuShares > 262144) {
		return errors.New("CPU shares must be between 2 and 262144")
	}
	if *cpuPeriod != 0 && (*cpuPeriod < 1000 || *cpuPeriod > 1000000) {
		return errors.New("CPU period must be between 1000 and 1000000 microseconds")
	}
	if *cpuQuota != 0 && *cpuQuota != -1 && *cpuQuota < 1000 {
		return errors.New("CPU quota must be -1 or at least 1000 microseconds")
	}
	if flagWasSet(fs, "cpus") && (flagWasSet(fs, "cpu-period") || flagWasSet(fs, "cpu-quota")) {
		return errors.New("--cpus cannot be combined with --cpu-period or --cpu-quota")
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
	if err := validateLiveForkCreationPolicy(networkMode, *waiveLiveFork); err != nil {
		return err
	}
	if *networkNAT && networkMode != "ttc" {
		return errors.New("--nat requires --network=ttc")
	}
	if err := validateVirtualNICOptions(networkMode, *networkIP, *networkMAC); err != nil {
		return err
	}
	publishedPorts, err := parsePublishedPorts(publishedPortValues)
	if err != nil {
		return err
	}
	if len(publishedPorts) != 0 && networkMode != "ttc" {
		return errors.New("--publish requires --network=ttc")
	}
	if *pullPolicy != "missing" && *pullPolicy != "always" && *pullPolicy != "never" {
		return fmt.Errorf("unsupported pull policy %q; supported policies are missing, always, and never", *pullPolicy)
	}
	parsedMounts, err := parseMountRequests(volumes, mounts, tmpfsMounts)
	if err != nil {
		return err
	}
	logDriver, err := parseLogDriver(*logDriverValue, logOptions)
	if err != nil {
		return err
	}
	logMaxBytes, logMaxFiles, parsedLogOptions, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	parsedUlimits, err := ttcbundle.ParseUlimits(ulimitValues)
	if err != nil {
		return err
	}
	parsedShmSize, err := ttcbundle.ParseShmSize(*shmSizeValue)
	if err != nil {
		return err
	}
	parsedMemoryLimit, err := ttcbundle.ParseMemoryLimit(*memoryValue)
	if err != nil {
		return err
	}
	parsedNanoCPUs, err := parseNanoCPUs(*cpusValue)
	if err != nil {
		return err
	}
	parsedCapAdd, err := ttcbundle.NormalizeCapabilities(capAddValues)
	if err != nil {
		return err
	}
	parsedCapDrop, err := ttcbundle.NormalizeCapabilities(capDropValues)
	if err != nil {
		return err
	}
	parsedExtraHosts, err := ttcbundle.ParseExtraHosts(extraHostValues)
	if err != nil {
		return err
	}
	parsedDNS, err := ttcbundle.ParseDNS(dnsValues)
	if err != nil {
		return err
	}
	parsedDNSSearch, err := ttcbundle.ParseDNSSearch(dnsSearchValues)
	if err != nil {
		return err
	}
	parsedDNSOptions, err := ttcbundle.ParseDNSOptions(dnsOptionValues)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttc-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	var effectivePidsLimit *int64
	if flagWasSet(fs, "pids-limit") {
		value := *pidsLimit
		effectivePidsLimit = &value
	}
	var effectiveMemoryLimit, effectiveNanoCPUs, effectiveCPUQuota *int64
	var effectiveCPUShares, effectiveCPUPeriod *uint64
	if flagWasSet(fs, "memory") || flagWasSet(fs, "m") {
		effectiveMemoryLimit = &parsedMemoryLimit
	}
	if flagWasSet(fs, "cpus") {
		effectiveNanoCPUs = &parsedNanoCPUs
	}
	if flagWasSet(fs, "cpu-shares") {
		effectiveCPUShares = cpuShares
	}
	if flagWasSet(fs, "cpu-period") {
		effectiveCPUPeriod = cpuPeriod
	}
	if flagWasSet(fs, "cpu-quota") {
		effectiveCPUQuota = cpuQuota
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		pullPolicy:         *pullPolicy,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                envValues,
		labels:             labelValues,
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		networkMode:        networkMode,
		liveForkEligible:   !*waiveLiveFork,
		liveForkWaiver:     liveForkWaiverReason(*waiveLiveFork, networkMode),
		networkNAT:         *networkNAT,
		networkIP:          *networkIP,
		networkMAC:         strings.ToLower(*networkMAC),
		publishedPorts:     publishedPorts,
		logDriver:          logDriver,
		extraHosts:         parsedExtraHosts,
		dns:                parsedDNS,
		dnsSearch:          parsedDNSSearch,
		dnsOptions:         parsedDNSOptions,
		hostMounts:         parsedMounts.host,
		filesystemMounts:   parsedMounts.filesystem,
		managedVolumes:     parsedMounts.managed,
		autoRemove:         *autoRemove,
		readonlyRootfs:     *readonlyRootfs,
		stopSignal:         *stopSignal,
		stopTimeout:        *stopTimeout,
		pidsLimit:          effectivePidsLimit,
		memoryLimit:        effectiveMemoryLimit,
		nanoCPUs:           effectiveNanoCPUs,
		cpuShares:          effectiveCPUShares,
		cpuPeriod:          effectiveCPUPeriod,
		cpuQuota:           effectiveCPUQuota,
		ulimits:            parsedUlimits,
		shmSize:            parsedShmSize,
		groupAdd:           append([]string(nil), groupAddValues...),
		capAdd:             parsedCapAdd,
		capDrop:            parsedCapDrop,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
		logOptions:         parsedLogOptions,
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
	args = expandCombinedShortBools(args, "it", "--name", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "-h", "--network", "--net", "--ip", "--mac-address", "--publish", "-p", "--add-host", "--dns", "--dns-search", "--dns-option", "--dns-opt", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--memory", "-m", "--cpus", "--cpu-shares", "--cpu-period", "--cpu-quota", "--shm-size", "--group-add", "--cap-add", "--cap-drop", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-driver", "--log-opt")
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "assign a container name")
	pullPolicy := fs.String("pull", "missing", "pull policy: missing, always, or never")
	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "keep stdin open")
	terminal := fs.Bool("tty", false, "allocate a terminal")
	fs.BoolVar(terminal, "t", false, "allocate a terminal")
	autoRemove := fs.Bool("rm", false, "automatically remove the container when it exits")
	readonlyRootfs := fs.Bool("read-only", false, "mount the container root filesystem as read only")
	stopSignal := fs.String("stop-signal", "", "signal to stop the container (defaults to image STOPSIGNAL or SIGTERM)")
	stopTimeout := fs.Int("stop-timeout", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait before killing the container; -1 waits forever")
	pidsLimit := fs.Int64("pids-limit", 0, "tune the container task limit; -1 or 0 means unlimited")
	memoryValue := fs.String("memory", "0", "memory limit")
	fs.StringVar(memoryValue, "m", "0", "memory limit")
	cpusValue := fs.String("cpus", "0", "number of CPUs")
	cpuShares := fs.Uint64("cpu-shares", 0, "CPU shares (relative weight)")
	cpuPeriod := fs.Uint64("cpu-period", 0, "limit CPU CFS period")
	cpuQuota := fs.Int64("cpu-quota", 0, "limit CPU CFS quota; -1 means unlimited")
	shmSizeValue := fs.String("shm-size", "64m", "size of /dev/shm")
	cwd := fs.String("workdir", "", "working directory inside the container")
	fs.StringVar(cwd, "w", "", "working directory inside the container")
	user := fs.String("user", "", "username or UID[:GID]")
	fs.StringVar(user, "u", "", "username or UID[:GID]")
	hostname := fs.String("hostname", "", "container hostname")
	fs.StringVar(hostname, "h", "", "container hostname")
	networkModeValue := fs.String("network", "none", "network mode: none, host, or ttc")
	waiveLiveFork := fs.Bool("waive-live-fork", false, "explicitly create a family that is not eligible for live fork")
	networkNAT := fs.Bool("nat", false, "allow outbound host networking from a ttc virtual NIC")
	networkIP := fs.String("ip", "", "IPv4 address for the ttc virtual NIC")
	networkMAC := fs.String("mac-address", "", "MAC address for the ttc virtual NIC")
	logDriverValue := fs.String("log-driver", ttcbundle.DefaultLogDriver, "logging driver: local, json-file, or none")
	fs.StringVar(networkModeValue, "net", "none", "network mode: none, host, or ttc")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var env, envFiles, labels, labelFiles, publishedPortValues stringSlice
	fs.Var(&publishedPortValues, "publish", "publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
	fs.Var(&publishedPortValues, "p", "publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
	fs.Var(&env, "env", "set or import environment variable NAME[=VALUE]")
	fs.Var(&env, "e", "set or import environment variable NAME[=VALUE]")
	fs.Var(&envFiles, "env-file", "read environment variables from a file")
	fs.Var(&labels, "label", "set container metadata KEY[=VALUE]")
	fs.Var(&labels, "l", "set container metadata KEY[=VALUE]")
	fs.Var(&labelFiles, "label-file", "read container labels from a file")
	var volumes, mounts, tmpfsMounts stringSlice
	fs.Var(&volumes, "volume", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy]")
	fs.Var(&volumes, "v", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy]")
	fs.Var(&mounts, "mount", "attach type=bind|tmpfs|proc|sysfs,target=CONTAINER[,OPTIONS]")
	fs.Var(&tmpfsMounts, "tmpfs", "mount tmpfs at CONTAINER[:OPTIONS]")
	var logOptions, ulimitValues, groupAddValues, capAddValues, capDropValues stringSlice
	var extraHostValues, dnsValues, dnsSearchValues, dnsOptionValues stringSlice
	fs.Var(&logOptions, "log-opt", "set logging option (max-size, max-file, labels, labels-regex, env, or env-regex)")
	fs.Var(&ulimitValues, "ulimit", "set resource limit NAME=SOFT[:HARD]")
	fs.Var(&groupAddValues, "group-add", "add an additional group by name or GID")
	fs.Var(&capAddValues, "cap-add", "add a Linux capability")
	fs.Var(&capDropValues, "cap-drop", "drop a Linux capability")
	fs.Var(&extraHostValues, "add-host", "add a custom host-to-IP mapping")
	fs.Var(&dnsValues, "dns", "set a custom DNS server")
	fs.Var(&dnsSearchValues, "dns-search", "set a custom DNS search domain")
	fs.Var(&dnsOptionValues, "dns-option", "set a DNS resolver option")
	fs.Var(&dnsOptionValues, "dns-opt", "set a DNS resolver option")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttc create [OPTIONS] IMAGE [COMMAND...]")
	}
	if *stopTimeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	if *pidsLimit < -1 {
		return errors.New("pids limit must be -1 or greater")
	}
	if *cpuShares != 0 && (*cpuShares < 2 || *cpuShares > 262144) {
		return errors.New("CPU shares must be between 2 and 262144")
	}
	if *cpuPeriod != 0 && (*cpuPeriod < 1000 || *cpuPeriod > 1000000) {
		return errors.New("CPU period must be between 1000 and 1000000 microseconds")
	}
	if *cpuQuota != 0 && *cpuQuota != -1 && *cpuQuota < 1000 {
		return errors.New("CPU quota must be -1 or at least 1000 microseconds")
	}
	if flagWasSet(fs, "cpus") && (flagWasSet(fs, "cpu-period") || flagWasSet(fs, "cpu-quota")) {
		return errors.New("--cpus cannot be combined with --cpu-period or --cpu-quota")
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
	if err := validateLiveForkCreationPolicy(networkMode, *waiveLiveFork); err != nil {
		return err
	}
	if *networkNAT && networkMode != "ttc" {
		return errors.New("--nat requires --network=ttc")
	}
	if err := validateVirtualNICOptions(networkMode, *networkIP, *networkMAC); err != nil {
		return err
	}
	publishedPorts, err := parsePublishedPorts(publishedPortValues)
	if err != nil {
		return err
	}
	if len(publishedPorts) != 0 && networkMode != "ttc" {
		return errors.New("--publish requires --network=ttc")
	}
	if *pullPolicy != "missing" && *pullPolicy != "always" && *pullPolicy != "never" {
		return fmt.Errorf("unsupported pull policy %q; supported policies are missing, always, and never", *pullPolicy)
	}
	parsedMounts, err := parseMountRequests(volumes, mounts, tmpfsMounts)
	if err != nil {
		return err
	}
	logDriver, err := parseLogDriver(*logDriverValue, logOptions)
	if err != nil {
		return err
	}
	logMaxBytes, logMaxFiles, parsedLogOptions, err := parseLogOptions(logOptions)
	if err != nil {
		return err
	}
	parsedUlimits, err := ttcbundle.ParseUlimits(ulimitValues)
	if err != nil {
		return err
	}
	parsedShmSize, err := ttcbundle.ParseShmSize(*shmSizeValue)
	if err != nil {
		return err
	}
	parsedMemoryLimit, err := ttcbundle.ParseMemoryLimit(*memoryValue)
	if err != nil {
		return err
	}
	parsedNanoCPUs, err := parseNanoCPUs(*cpusValue)
	if err != nil {
		return err
	}
	parsedCapAdd, err := ttcbundle.NormalizeCapabilities(capAddValues)
	if err != nil {
		return err
	}
	parsedCapDrop, err := ttcbundle.NormalizeCapabilities(capDropValues)
	if err != nil {
		return err
	}
	parsedExtraHosts, err := ttcbundle.ParseExtraHosts(extraHostValues)
	if err != nil {
		return err
	}
	parsedDNS, err := ttcbundle.ParseDNS(dnsValues)
	if err != nil {
		return err
	}
	parsedDNSSearch, err := ttcbundle.ParseDNSSearch(dnsSearchValues)
	if err != nil {
		return err
	}
	parsedDNSOptions, err := ttcbundle.ParseDNSOptions(dnsOptionValues)
	if err != nil {
		return err
	}
	containerName := *name
	if containerName == "" {
		containerName = "ttc-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	var effectivePidsLimit *int64
	if flagWasSet(fs, "pids-limit") {
		value := *pidsLimit
		effectivePidsLimit = &value
	}
	var effectiveMemoryLimit, effectiveNanoCPUs, effectiveCPUQuota *int64
	var effectiveCPUShares, effectiveCPUPeriod *uint64
	if flagWasSet(fs, "memory") || flagWasSet(fs, "m") {
		effectiveMemoryLimit = &parsedMemoryLimit
	}
	if flagWasSet(fs, "cpus") {
		effectiveNanoCPUs = &parsedNanoCPUs
	}
	if flagWasSet(fs, "cpu-shares") {
		effectiveCPUShares = cpuShares
	}
	if flagWasSet(fs, "cpu-period") {
		effectiveCPUPeriod = cpuPeriod
	}
	if flagWasSet(fs, "cpu-quota") {
		effectiveCPUQuota = cpuQuota
	}
	request := runRequest{
		name:               containerName,
		reference:          fs.Arg(0),
		pullPolicy:         *pullPolicy,
		args:               append([]string(nil), fs.Args()[1:]...),
		env:                envValues,
		labels:             labelValues,
		cwd:                *cwd,
		user:               *user,
		hostname:           *hostname,
		networkMode:        networkMode,
		liveForkEligible:   !*waiveLiveFork,
		liveForkWaiver:     liveForkWaiverReason(*waiveLiveFork, networkMode),
		networkNAT:         *networkNAT,
		networkIP:          *networkIP,
		networkMAC:         strings.ToLower(*networkMAC),
		publishedPorts:     publishedPorts,
		logDriver:          logDriver,
		extraHosts:         parsedExtraHosts,
		dns:                parsedDNS,
		dnsSearch:          parsedDNSSearch,
		dnsOptions:         parsedDNSOptions,
		hostMounts:         parsedMounts.host,
		filesystemMounts:   parsedMounts.filesystem,
		managedVolumes:     parsedMounts.managed,
		autoRemove:         *autoRemove,
		readonlyRootfs:     *readonlyRootfs,
		stopSignal:         *stopSignal,
		stopTimeout:        *stopTimeout,
		pidsLimit:          effectivePidsLimit,
		memoryLimit:        effectiveMemoryLimit,
		nanoCPUs:           effectiveNanoCPUs,
		cpuShares:          effectiveCPUShares,
		cpuPeriod:          effectiveCPUPeriod,
		cpuQuota:           effectiveCPUQuota,
		ulimits:            parsedUlimits,
		shmSize:            parsedShmSize,
		groupAdd:           append([]string(nil), groupAddValues...),
		capAdd:             parsedCapAdd,
		capDrop:            parsedCapDrop,
		consoleLogMaxBytes: logMaxBytes,
		consoleLogMaxFiles: logMaxFiles,
		logOptions:         parsedLogOptions,
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
		return errors.New("usage: ttc start [--attach] CONTAINER@BRANCH [CONTAINER@BRANCH...]")
	}
	if *interactive && !*attach {
		return errors.New("--interactive requires --attach")
	}
	if *attach && fs.NArg() != 1 {
		return errors.New("--attach supports exactly one container")
	}
	for _, value := range fs.Args() {
		target, err := parseSelector(value)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		status, err := backend.start(target, !*attach, *interactive)
		if err != nil {
			return err
		}
		if status != 0 {
			return exitStatus{code: status}
		}
		if !*attach {
			fmt.Fprintln(stdout, target)
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

type remoteTerminalInput interface {
	io.Reader
	setRawTerminal(bool) error
	terminalSize() (cols, rows int, ok bool, err error)
	terminalResizeEvents() <-chan struct{}
}

func makeRawTerminal(input io.Reader) (func() error, error) {
	if remote, ok := input.(remoteTerminalInput); ok {
		if err := remote.setRawTerminal(true); err != nil {
			return nil, err
		}
		return func() error { return remote.setRawTerminal(false) }, nil
	}
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
	if remote, ok := input.(remoteTerminalInput); ok {
		return remote.terminalSize()
	}
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
	takeover := fs.Bool("takeover", false, "replace the current daemon-owned stdin attachment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc attach [-i|--no-stdin] [--takeover] [--no-stdout] [--no-stderr] [--exec EXEC_ID] [--detach-keys KEYS] CONTAINER@BRANCH")
	}
	if *interactive && *noStdin {
		return errors.New("--interactive and --no-stdin cannot be used together")
	}
	if *takeover {
		return errors.New("--takeover is unavailable for this attachment")
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

type logsOptions struct {
	ExecID     string
	Follow     bool
	Tail       int
	Since      *time.Time
	Until      *time.Time
	Timestamps bool
	Details    bool
}

func parseLogTime(value string, now time.Time) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return time.Time{}, fmt.Errorf("duration must not be negative")
		}
		return now.Add(-duration), nil
	}
	secondsText, fractionText, hasFraction := strings.Cut(value, ".")
	seconds, err := strconv.ParseInt(secondsText, 10, 64)
	if err == nil {
		nanoseconds := int64(0)
		if hasFraction {
			if fractionText == "" || len(fractionText) > 9 || strings.Trim(fractionText, "0123456789") != "" {
				err = errors.New("invalid fractional seconds")
			} else {
				fractionText += strings.Repeat("0", 9-len(fractionText))
				nanoseconds, err = strconv.ParseInt(fractionText, 10, 32)
			}
		}
		if err == nil {
			if seconds < 0 {
				nanoseconds = -nanoseconds
			}
			return time.Unix(seconds, nanoseconds).UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("must be an RFC3339 timestamp, Unix timestamp, or Go duration")
}

func runLogs(args []string, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", false, "follow log output")
	fs.BoolVar(follow, "f", false, "follow log output")
	execID := fs.String("exec", "", "show logs for a managed exec session")
	sinceValue := fs.String("since", "", "show logs since timestamp or relative duration")
	untilValue := fs.String("until", "", "show logs before timestamp or relative duration")
	timestamps := fs.Bool("timestamps", false, "prefix each log line with its timestamp")
	fs.BoolVar(timestamps, "t", false, "prefix each log line with its timestamp")
	details := fs.Bool("details", false, "show configured log attributes")
	tailValue := fs.String("tail", "all", "number of lines to show from the end of the logs")
	fs.StringVar(tailValue, "n", "all", "number of lines to show from the end of the logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc logs [-f] [--exec EXEC_ID] [--tail all|N] [--since TIME] [--until TIME] [-t] [--details] CONTAINER@BRANCH")
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
	now := time.Now()
	var since, until *time.Time
	if *sinceValue != "" {
		parsed, err := parseLogTime(*sinceValue, now)
		if err != nil {
			return fmt.Errorf("invalid --since %q: %w", *sinceValue, err)
		}
		since = &parsed
	}
	if *untilValue != "" {
		parsed, err := parseLogTime(*untilValue, now)
		if err != nil {
			return fmt.Errorf("invalid --until %q: %w", *untilValue, err)
		}
		until = &parsed
	}
	options := logsOptions{ExecID: *execID, Follow: *follow, Tail: tail, Since: since, Until: until, Timestamps: *timestamps, Details: *details}
	if err := backend.logs(target, options); err != nil {
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

	args = expandCombinedShortBools(args, "dit", "--workdir", "-w", "--user", "-u", "--env", "-e", "--additional-gids", "--cap", "--process", "--session")
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
	sessionID := fs.String("session", "", "durable branch-local exec session name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	usingProcess := *processPath != ""
	if (!usingProcess && fs.NArg() < 2) || (usingProcess && fs.NArg() != 1) {
		return errors.New("usage: ttc exec [OPTIONS] CONTAINER@BRANCH COMMAND [ARG...] | ttc exec [OPTIONS] --process FILE CONTAINER@BRANCH")
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
	execID := *sessionID
	if execID == "" {
		execID, err = newExecID()
		if err != nil {
			return err
		}
	} else if _, err := parseExecSelector(target.String() + ":" + execID); err != nil {
		return fmt.Errorf("invalid --session: %w", err)
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
		return errors.New("usage: ttc exec ls [--json] CONTAINER@BRANCH")
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
		return errors.New("usage: ttc exec wait CONTAINER@BRANCH:EXEC_ID")
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
		return errors.New("usage: ttc exec kill [--signal SIGNAL] CONTAINER@BRANCH:EXEC_ID")
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

// InspectLogConfig is the Docker-compatible logging configuration view.
type InspectLogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

// InspectPortBinding is Docker's HostConfig.PortBindings value shape.
type InspectPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// InspectHostConfig is the subset of Docker host configuration implemented by TTC.
type InspectHostConfig struct {
	Binds          []string                        `json:"Binds"`
	NetworkMode    string                          `json:"NetworkMode"`
	PortBindings   map[string][]InspectPortBinding `json:"PortBindings"`
	LogConfig      InspectLogConfig                `json:"LogConfig"`
	ExtraHosts     []string                        `json:"ExtraHosts"`
	DNS            []string                        `json:"Dns"`
	DNSSearch      []string                        `json:"DnsSearch"`
	DNSOptions     []string                        `json:"DnsOptions"`
	AutoRemove     bool                            `json:"AutoRemove"`
	ReadonlyRootfs bool                            `json:"ReadonlyRootfs"`
	StopTimeout    *int                            `json:"StopTimeout"`
	PidsLimit      *int64                          `json:"PidsLimit"`
	Memory         int64                           `json:"Memory"`
	NanoCPUs       int64                           `json:"NanoCpus"`
	CPUShares      uint64                          `json:"CpuShares"`
	CPUPeriod      uint64                          `json:"CpuPeriod"`
	CPUQuota       int64                           `json:"CpuQuota"`
	ShmSize        int64                           `json:"ShmSize"`
	GroupAdd       []string                        `json:"GroupAdd"`
	CapAdd         []string                        `json:"CapAdd"`
	CapDrop        []string                        `json:"CapDrop"`
	Ulimits        []ttcbundle.Ulimit              `json:"Ulimits"`
	Runtime        string                          `json:"Runtime"`
	Tmpfs          map[string]string               `json:"Tmpfs,omitempty"`
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

// InspectExternalView describes a host-backed lower view and its branch semantics.
type InspectExternalView struct {
	Source                  string `json:"source"`
	Destination             string `json:"destination"`
	Readonly                bool   `json:"readonly"`
	ImmutableSourceAsserted bool   `json:"immutable_source_asserted,omitempty"`
	ManagedCoW              bool   `json:"managed_cow,omitempty"`
	Semantics               string `json:"semantics"`
}

// InspectGraphDriver describes TTC's immutable image lower and persistent,
// branchable userspace upper layer.
type InspectGraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

// InspectBranch enriches stable lineage metadata with observational live state.
type InspectBranch struct {
	Metadata                 Branch                               `json:"metadata"`
	Processes                []ProcessInfo                        `json:"processes,omitempty"`
	ProcessError             string                               `json:"process_error,omitempty"`
	OutputRetention          *ttcconsole.RetentionStats           `json:"output_retention,omitempty"`
	OutputRetentionError     string                               `json:"output_retention_error,omitempty"`
	ExecOutputRetention      map[string]ttcconsole.RetentionStats `json:"exec_output_retention,omitempty"`
	ExecOutputRetentionError string                               `json:"exec_output_retention_error,omitempty"`
}

// TTCInspectInfo contains state with no direct Docker analogue.
type TTCInspectInfo struct {
	Container       ttcbundle.ContainerInfo `json:"container"`
	Runtime         *Family                 `json:"runtime,omitempty"`
	Branches        []InspectBranch         `json:"branches"`
	ExternalViews   []InspectExternalView   `json:"external_views,omitempty"`
	DistributedFork *cluster.Transaction    `json:"distributed_fork,omitempty"`
	RuntimeError    string                  `json:"runtime_error,omitempty"`
}

// InspectInfo combines a Docker-compatible main-container view with TTC lineage.
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
	TTC          TTCInspectInfo     `json:"TTC"`
}

func runInspect(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", "format output using a Go template or 'json'")
	fs.StringVar(format, "f", "", "format output using a Go template or 'json'")
	objectType := fs.String("type", "container", "inspect object type (container only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *objectType != "container" {
		return fmt.Errorf("inspect type %q is not supported; use 'container'", *objectType)
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc inspect [--format TEMPLATE] CONTAINER [CONTAINER...]")
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

func inspectHostConfigTemplateValues(config InspectHostConfig) map[string]any {
	// Docker templates address serialized API keys, including Dns rather than
	// the Go initialism DNS. Keep values in their original Go types so integer
	// limits and structured slices retain Docker-compatible template rendering.
	return map[string]any{
		"Binds":          config.Binds,
		"NetworkMode":    config.NetworkMode,
		"PortBindings":   config.PortBindings,
		"LogConfig":      config.LogConfig,
		"ExtraHosts":     config.ExtraHosts,
		"Dns":            config.DNS,
		"DnsSearch":      config.DNSSearch,
		"DnsOptions":     config.DNSOptions,
		"AutoRemove":     config.AutoRemove,
		"ReadonlyRootfs": config.ReadonlyRootfs,
		"StopTimeout":    config.StopTimeout,
		"PidsLimit":      config.PidsLimit,
		"Memory":         config.Memory,
		"NanoCpus":       config.NanoCPUs,
		"CpuShares":      config.CPUShares,
		"CpuPeriod":      config.CPUPeriod,
		"CpuQuota":       config.CPUQuota,
		"ShmSize":        config.ShmSize,
		"GroupAdd":       config.GroupAdd,
		"CapAdd":         config.CapAdd,
		"CapDrop":        config.CapDrop,
		"Ulimits":        config.Ulimits,
		"Runtime":        config.Runtime,
		"Tmpfs":          config.Tmpfs,
	}
}

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
		"HostConfig":   inspectHostConfigTemplateValues(result.HostConfig),
		"Mounts":       result.Mounts,
		"TTC":          result.TTC,
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
	force := fs.Bool("force", false, "force removal of running branches")
	fs.BoolVar(force, "f", false, "force removal of running branches")
	volumes := fs.Bool("volumes", false, "remove anonymous volumes attached to the container")
	fs.BoolVar(volumes, "v", false, "remove anonymous volumes attached to the container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc rm [--force] [--volumes] (CONTAINER|CONTAINER@BRANCH) [TARGET...]")
	}
	for _, value := range fs.Args() {
		if strings.Contains(value, "@") {
			if *volumes {
				return fmt.Errorf("--volumes applies only when removing a whole container family, not %q", value)
			}
			target, err := parseSelector(value)
			if err != nil {
				return fmt.Errorf("invalid target %q: %w", value, err)
			}
			if err := backend.destroy(target, *force); err != nil {
				return err
			}
			fmt.Fprintln(stdout, target)
			continue
		}
		if err := backend.remove(value, *force, *volumes); err != nil {
			return err
		}
		fmt.Fprintln(stdout, value)
	}
	return nil
}

func runAutoRemoveNow(args []string, backend backend) error {
	if len(args) != 1 {
		return errors.New("internal auto-remove-now requires one branch selector")
	}
	target, err := parseSelector(args[0])
	if err != nil {
		return fmt.Errorf("internal auto-remove target: %w", err)
	}
	if err := autoRemoveTarget(target, backend); err != nil {
		return fmt.Errorf("auto-removing %s: %w", target, err)
	}
	return nil
}

// autoRemoveTarget preserves peer branches that may have been forked after a
// container was launched with --rm. The family (and its anonymous volumes) is
// removed only while the selected branch is still its sole member.
func autoRemoveTarget(target selector, backend backend) error {
	info, err := backend.inspect(target.container)
	if err != nil {
		return err
	}
	if len(info.TTC.Branches) <= 1 {
		return backend.remove(target.container, true, true)
	}
	return backend.destroy(target, true)
}

func runPull(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "target platform, such as linux/amd64")
	quiet := fs.Bool("quiet", false, "only print the pulled digest")
	fs.BoolVar(quiet, "q", false, "only print the pulled digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc pull [OPTIONS] IMAGE")
	}
	reference, err := ttcimages.NormalizeReference(fs.Arg(0))
	if err != nil {
		return err
	}
	info, err := backend.pull(reference, *platform, !*quiet)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Fprintln(stdout, info.Digest)
		return nil
	}
	fmt.Fprintf(stdout, "Pulled %s\nDigest: %s\nPlatform: %s\n", info.Reference, info.Digest, info.Platform)
	if info.PullStats != nil {
		fmt.Fprintf(stdout, "Content: %d bytes downloaded, %d bytes reused\n", info.PullStats.DownloadedBytes, info.PullStats.ReusedBytes)
	}
	return nil
}

func runImageLoad(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "load only the given platform")
	input := fs.String("input", "", "read from a tar archive file instead of standard input")
	fs.StringVar(input, "i", "", "read from a tar archive file instead of standard input")
	quiet := fs.Bool("quiet", false, "suppress load output")
	fs.BoolVar(quiet, "q", false, "suppress load output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: ttc load [--input FILE] [--platform PLATFORM] [--quiet]")
	}
	var archive io.Reader
	if *input != "" {
		file, err := os.Open(*input)
		if err != nil {
			return fmt.Errorf("opening image archive: %w", err)
		}
		defer file.Close()
		archive = file
	}
	infos, err := backend.loadImages(archive, *platform)
	if err != nil {
		return err
	}
	if !*quiet {
		for _, info := range infos {
			fmt.Fprintf(stdout, "Loaded image: %s\n", familiarImageReference(info.Reference))
		}
	}
	return nil
}

func runImageSave(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "save only the given platform")
	output := fs.String("output", "", "write to a file instead of standard output")
	fs.StringVar(output, "o", "", "write to a file instead of standard output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]")
	}
	if *output != "" {
		file, err := os.OpenFile(*output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return fmt.Errorf("opening image archive output: %w", err)
		}
		saveErr := backend.saveImages(file, fs.Args(), *platform)
		return errors.Join(saveErr, file.Close())
	}
	return backend.saveImages(stdout, fs.Args(), *platform)
}

func runImage(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc image ls|pull|load|save|inspect|rm [OPTIONS]")
	}
	switch args[0] {
	case "ls", "list":
		return runImages(args[1:], stdout, stderr, backend)
	case "pull":
		return runPull(args[1:], stdout, stderr, backend)
	case "load":
		return runImageLoad(args[1:], stdout, stderr, backend)
	case "save":
		return runImageSave(args[1:], stdout, stderr, backend)
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
		return errors.New("usage: ttc images [--quiet] [--no-trunc] [--json]")
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
	fmt.Fprintln(table, "REPOSITORY\tTAG\tIMAGE ID\tPLATFORM\tROOTFS\tSIZE\tCREATED")
	now := time.Now()
	for _, info := range infos {
		repository, tag := splitImageReference(info.Reference)
		repository = familiarImageReference(repository)
		materialized := "no"
		if info.Materialized {
			materialized = "yes"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			repository, tag, displayImageID(info.Digest, *noTrunc), info.Platform,
			materialized, formatImageSize(info.Size), formatImageAge(now, info.CreatedAt))
	}
	return table.Flush()
}

func runImageInspect(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc image inspect IMAGE [IMAGE...]")
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
		return errors.New("usage: ttc image rm [--force] IMAGE [IMAGE...]")
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

func shortLayerID(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	if digest == "" {
		return "image"
	}
	return digest
}

type imageLayerProgress struct {
	id             string
	phase          string
	current        int64
	total          int64
	index          int
	count          int
	paintedCurrent int64
}

type imageProgressRenderer struct {
	w           io.Writer
	terminal    bool
	width       int
	nextPercent map[string]int
	layers      map[string]*imageLayerProgress
	order       []string
	rendered    int
	lastPaint   time.Time
	mu          sync.Mutex
}

func terminalWriterInfo(w io.Writer) (bool, int) {
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false, 0
	}
	file, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return false, 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		width = 80
	}
	return true, width
}

// newImageProgress turns concurrent structured pull events into human-facing
// output. Terminals receive one repaintable line per layer; redirected output
// remains stable and line-oriented.
func newImageProgress(w io.Writer) ttcimages.ProgressFunc {
	terminal, width := terminalWriterInfo(w)
	return newImageProgressWithTerminal(w, terminal, width)
}

func newImageProgressWithTerminal(w io.Writer, terminal bool, width int) ttcimages.ProgressFunc {
	if w == nil {
		return nil
	}
	renderer := &imageProgressRenderer{
		w:           w,
		terminal:    terminal,
		width:       width,
		nextPercent: make(map[string]int),
		layers:      make(map[string]*imageLayerProgress),
	}
	return renderer.update
}

func (r *imageProgressRenderer) update(event ttcimages.ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.terminal {
		r.updatePlain(event)
		return
	}
	if event.Phase == ttcimages.ProgressResolving {
		fmt.Fprintln(r.w, "Resolving image metadata")
		return
	}
	digest := event.Descriptor.Digest.String()
	key := digest
	if key == "" {
		key = "image"
	}
	layer := r.layers[key]
	added := false
	if layer == nil {
		layer = &imageLayerProgress{id: shortLayerID(digest)}
		r.layers[key] = layer
		r.order = append(r.order, key)
		added = true
	}
	phaseChanged := layer.phase != event.Phase
	previousCurrent := layer.current
	layer.phase = event.Phase
	layer.current = event.Current
	layer.total = event.Total
	layer.index = event.Index
	layer.count = event.Count
	force := added || phaseChanged || event.Current == 0 ||
		(previousCurrent == 0 && event.Current > 0) ||
		event.Phase == ttcimages.ProgressDownloadDone ||
		event.Phase == ttcimages.ProgressAlreadyExists ||
		event.Phase == ttcimages.ProgressExtracted
	if !force {
		if !imageProgressMustPaint(layer, event) || (!r.lastPaint.IsZero() && time.Since(r.lastPaint) < 80*time.Millisecond) {
			return
		}
	}
	r.paint()
}

func imageProgressMustPaint(layer *imageLayerProgress, event ttcimages.ProgressEvent) bool {
	switch event.Phase {
	case ttcimages.ProgressDownloadDone, ttcimages.ProgressAlreadyExists, ttcimages.ProgressExtracted:
		return true
	}
	if event.Current == 0 {
		return true
	}
	threshold := int64(128 << 10)
	if event.Total > 0 && event.Total/100 > threshold {
		threshold = event.Total / 100
	}
	return event.Current-layer.paintedCurrent >= threshold
}

func (r *imageProgressRenderer) paint() {
	if r.rendered > 0 {
		fmt.Fprintf(r.w, "\x1b[%dA", r.rendered)
	}
	for _, key := range r.order {
		layer := r.layers[key]
		line := formatInteractiveLayerProgress(layer, r.width)
		fmt.Fprintf(r.w, "\r\x1b[2K%s\n", line)
		layer.paintedCurrent = layer.current
	}
	r.rendered = len(r.order)
	r.lastPaint = time.Now()
}

func formatInteractiveLayerProgress(layer *imageLayerProgress, width int) string {
	prefix := layer.id + ": "
	switch layer.phase {
	case ttcimages.ProgressDownloading:
		if layer.current == 0 {
			return fitTerminalLine(prefix+"Pulling fs layer ("+formatImageSize(layer.total)+")", width)
		}
		return fitTerminalLine(prefix+formatImageTransfer("Downloading", layer.current, layer.total, width-len(prefix)), width)
	case ttcimages.ProgressDownloadDone:
		return fitTerminalLine(prefix+"Download complete", width)
	case ttcimages.ProgressAlreadyExists:
		return fitTerminalLine(prefix+"Already exists", width)
	case ttcimages.ProgressExtracting:
		status := "Extracting"
		if layer.count > 0 {
			status = fmt.Sprintf("Extracting %d/%d", layer.index, layer.count)
		}
		return fitTerminalLine(prefix+formatImageTransfer(status, layer.current, layer.total, width-len(prefix)), width)
	case ttcimages.ProgressExtracted:
		return fitTerminalLine(prefix+"Pull complete", width)
	default:
		return fitTerminalLine(prefix+layer.phase, width)
	}
}

func formatImageTransfer(status string, current, total int64, available int) string {
	detail := formatImageSize(current)
	if total > 0 {
		detail += " / " + formatImageSize(total)
	}
	if total <= 0 {
		return status + " " + detail
	}
	barWidth := available - len(status) - len(detail) - 5
	if barWidth < 10 {
		return status + " " + detail
	}
	if barWidth > 30 {
		barWidth = 30
	}
	completed := int(float64(current) * float64(barWidth) / float64(total))
	if completed < 0 {
		completed = 0
	}
	if completed > barWidth {
		completed = barWidth
	}
	bar := strings.Repeat("=", completed)
	if completed < barWidth {
		bar += ">"
		bar += strings.Repeat(" ", barWidth-completed-1)
	}
	return fmt.Sprintf("%s [%s] %s", status, bar, detail)
}

func fitTerminalLine(line string, width int) string {
	if width <= 0 || len(line) <= width {
		return line
	}
	if width == 1 {
		return "~"
	}
	return line[:width-1] + "~"
}

func (r *imageProgressRenderer) updatePlain(event ttcimages.ProgressEvent) {
	id := shortLayerID(event.Descriptor.Digest.String())
	switch event.Phase {
	case ttcimages.ProgressResolving:
		fmt.Fprintln(r.w, "Resolving image metadata")
	case ttcimages.ProgressDownloading:
		key := "download:" + id
		if event.Current == 0 {
			fmt.Fprintf(r.w, "%s: Pulling fs layer (%s)\n", id, formatImageSize(event.Total))
			r.nextPercent[key] = 10
			return
		}
		percent := 100
		if event.Total > 0 {
			percent = int(event.Current * 100 / event.Total)
		}
		if percent >= r.nextPercent[key] && percent < 100 {
			fmt.Fprintf(r.w, "%s: Downloading %s / %s (%d%%)\n", id, formatImageSize(event.Current), formatImageSize(event.Total), percent)
			r.nextPercent[key] = (percent/10 + 1) * 10
		}
	case ttcimages.ProgressDownloadDone:
		fmt.Fprintf(r.w, "%s: Download complete\n", id)
		delete(r.nextPercent, "download:"+id)
	case ttcimages.ProgressAlreadyExists:
		fmt.Fprintf(r.w, "%s: Already exists\n", id)
	case ttcimages.ProgressExtracting:
		key := "extract:" + id
		if event.Current == 0 {
			fmt.Fprintf(r.w, "%s: Extracting layer %d/%d (%s)\n", id, event.Index, event.Count, formatImageSize(event.Total))
			r.nextPercent[key] = 25
			return
		}
		percent := 100
		if event.Total > 0 {
			percent = int(event.Current * 100 / event.Total)
		}
		if percent >= r.nextPercent[key] && percent < 100 {
			fmt.Fprintf(r.w, "%s: Extracting %s / %s (%d%%)\n", id, formatImageSize(event.Current), formatImageSize(event.Total), percent)
			r.nextPercent[key] = (percent/25 + 1) * 25
		}
	case ttcimages.ProgressExtracted:
		fmt.Fprintf(r.w, "%s: Pull complete\n", id)
		delete(r.nextPercent, "extract:"+id)
	}
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
	args = expandCombinedShortBools(args, "aqls", "--filter", "-f", "--format", "--last", "-n")
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show created and exited branches")
	fs.BoolVar(all, "a", false, "show created and exited branches")
	quiet := fs.Bool("quiet", false, "only display branch-qualified selectors")
	fs.BoolVar(quiet, "q", false, "only display branch-qualified selectors")
	var filters stringSlice
	fs.Var(&filters, "filter", "filter output (name, id, status, ancestor, or branch)")
	fs.Var(&filters, "f", "filter output (name, id, status, ancestor, or branch)")
	format := fs.String("format", "", "format rows using a Go template or json")
	last := fs.Int("last", -1, "show the most recently created N branches")
	fs.IntVar(last, "n", -1, "show the most recently created N branches")
	latest := fs.Bool("latest", false, "show only the most recently created branch")
	fs.BoolVar(latest, "l", false, "show only the most recently created branch")
	noTrunc := fs.Bool("no-trunc", false, "do not truncate image IDs")
	sizeOutput := fs.Bool("size", false, "display total file sizes")
	fs.BoolVar(sizeOutput, "s", false, "display total file sizes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: ttc ps [--all] [--quiet] [--size] [--filter KEY=VALUE] [--format TEMPLATE] [--latest|--last N]")
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
	if *sizeOutput {
		if err := populatePsSizes(rows, backend); err != nil {
			return err
		}
	}
	if *format != "" {
		return writePsFormat(stdout, rows, *format)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if *sizeOutput {
		fmt.Fprintln(table, "CONTAINER\tBRANCH\tIMAGE\tSTATE\tCREATED\tPARENT\tSIZE")
	} else {
		fmt.Fprintln(table, "CONTAINER\tBRANCH\tIMAGE\tSTATE\tCREATED\tPARENT")
	}
	for _, row := range rows {
		if *sizeOutput {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Container, row.Branch, row.Image, row.State, row.RunningFor, row.Parent, row.Size)
		} else {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Container, row.Branch, row.Image, row.State, row.RunningFor, row.Parent)
		}
	}
	return table.Flush()
}

type psRow struct {
	ID         string `json:"id"`
	Names      string `json:"names"`
	Container  string `json:"container"`
	Branch     string `json:"branch"`
	Image      string `json:"image"`
	ImageID    string `json:"image_id,omitempty"`
	State      string `json:"state"`
	Status     string `json:"status"`
	Parent     string `json:"parent"`
	CreatedAt  string `json:"created_at"`
	RunningFor string `json:"running_for"`
	Size       string `json:"size,omitempty"`
	SizeRW     uint64 `json:"size_rw,omitempty"`
	SizeRootFS uint64 `json:"size_root_fs,omitempty"`

	created   time.Time
	digest    string
	imageSize int64
}

func populatePsSizes(rows []psRow, backend backend) error {
	for i := range rows {
		row := &rows[i]
		imageSize := uint64(0)
		if row.imageSize > 0 {
			imageSize = uint64(row.imageSize)
		}
		known := row.State == "created"
		writable := uint64(0)
		if row.State == "running" || row.State == "paused" {
			var err error
			writable, err = backend.rootfsSize(selector{container: row.Container, branch: row.Branch})
			if err != nil {
				return fmt.Errorf("getting size for %s: %w", row.ID, err)
			}
			known = true
		}
		row.SizeRootFS = imageSize + writable
		if known {
			row.SizeRW = writable
			row.Size = fmt.Sprintf("%s (virtual %s)", formatImageSize(int64(writable)), formatImageSize(int64(row.SizeRootFS)))
		} else {
			row.Size = fmt.Sprintf("- (virtual %s)", formatImageSize(int64(imageSize)))
		}
	}
	return nil
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
				created:    created,
				digest:     family.Digest,
				imageSize:  family.ImageSize,
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
		case "id", "status", "ancestor", "branch":
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
	if normalized, err := ttcimages.NormalizeReference(value); err == nil {
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
			Size: "SIZE",
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

type topColumn struct {
	key   string
	title string
}

func defaultTopColumns() []topColumn {
	return []topColumn{{"uid", "UID"}, {"pid", "PID"}, {"ppid", "PPID"}, {"pgid", "PGID"}, {"c", "C"}, {"tty", "TTY"}, {"stime", "STIME"}, {"time", "TIME"}, {"cmd", "CMD"}}
}

func fullTopColumns() []topColumn {
	return []topColumn{{"uid", "UID"}, {"pid", "PID"}, {"ppid", "PPID"}, {"c", "C"}, {"stime", "STIME"}, {"tty", "TTY"}, {"time", "TIME"}, {"cmd", "CMD"}}
}

func parseTopColumnList(value string) ([]topColumn, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("top -o requires at least one column")
	}
	var specs []string
	if strings.Contains(value, ",") {
		specs = strings.Split(value, ",")
	} else {
		specs = strings.Fields(value)
	}
	columns := make([]topColumn, 0, len(specs))
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		name, title, hasTitle := strings.Cut(spec, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		canonical, defaultTitle := "", ""
		switch name {
		case "uid", "euid":
			canonical, defaultTitle = "uid", "UID"
		case "user":
			canonical, defaultTitle = "uid", "USER"
		case "pid":
			canonical, defaultTitle = "pid", "PID"
		case "ppid":
			canonical, defaultTitle = "ppid", "PPID"
		case "pgid", "pgrp":
			canonical, defaultTitle = "pgid", "PGID"
		case "nlwp", "thcount":
			canonical, defaultTitle = "nlwp", "NLWP"
		case "c":
			canonical, defaultTitle = "c", "C"
		case "pcpu", "%cpu":
			canonical, defaultTitle = "c", "%CPU"
		case "tty", "tt":
			canonical, defaultTitle = "tty", "TTY"
		case "stime":
			canonical, defaultTitle = "stime", "STIME"
		case "start":
			canonical, defaultTitle = "stime", "START"
		case "time", "cputime":
			canonical, defaultTitle = "time", "TIME"
		case "cmd":
			canonical, defaultTitle = "cmd", "CMD"
		case "comm", "command", "args":
			canonical, defaultTitle = "cmd", "COMMAND"
		default:
			return nil, fmt.Errorf("unsupported top column %q; Sentry reports uid,pid,ppid,pgid,nlwp,c,tty,stime,time,cmd", name)
		}
		if hasTitle {
			defaultTitle = strings.TrimSpace(title)
		}
		columns = append(columns, topColumn{key: canonical, title: defaultTitle})
	}
	return columns, nil
}

func parseTopProcessOptions(args []string) ([]topColumn, error) {
	columns := defaultTopColumns()
	explicit := false
	appendColumns := func(value string) error {
		parsed, err := parseTopColumnList(strings.TrimPrefix(value, "="))
		if err != nil {
			return err
		}
		if !explicit {
			columns = nil
			explicit = true
		}
		columns = append(columns, parsed...)
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-e" || arg == "-A":
			// The Sentry process RPC already returns every process in the branch.
		case arg == "-f" || arg == "-ef" || arg == "-fe":
			if !explicit {
				columns = fullTopColumns()
			}
		case arg == "-o" || arg == "--format" || arg == "-eo" || arg == "-Ao":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("top %s requires a column list", arg)
			}
			i++
			if err := appendColumns(args[i]); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "--format="):
			if err := appendColumns(strings.TrimPrefix(arg, "--format=")); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "-eo") && len(arg) > 3:
			if err := appendColumns(strings.TrimPrefix(arg, "-eo")); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "-o") && len(arg) > 2:
			if err := appendColumns(strings.TrimPrefix(arg, "-o")); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported top process option %q; use -e, -A, -f, -ef, or -o COLUMN[,COLUMN...]", arg)
		}
	}
	return columns, nil
}

func topColumnValue(process ProcessInfo, key string) string {
	switch key {
	case "uid":
		return strconv.FormatUint(uint64(process.UID), 10)
	case "pid":
		return strconv.FormatInt(int64(process.PID), 10)
	case "ppid":
		return strconv.FormatInt(int64(process.PPID), 10)
	case "pgid":
		return strconv.FormatInt(int64(process.PGID), 10)
	case "nlwp":
		return strconv.Itoa(len(process.Threads))
	case "c":
		return strconv.FormatInt(int64(process.C), 10)
	case "tty":
		return process.TTY
	case "stime":
		return process.STime
	case "time":
		return process.Time
	case "cmd":
		return process.Cmd
	default:
		panic("unvalidated top column " + key)
	}
}

func writeTopTable(w io.Writer, processes []ProcessInfo, columns []topColumn) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, byte(32), 0)
	for i, column := range columns {
		if i != 0 {
			fmt.Fprint(table, "\t")
		}
		fmt.Fprint(table, column.title)
	}
	fmt.Fprintln(table)
	for _, process := range processes {
		for i, column := range columns {
			if i != 0 {
				fmt.Fprint(table, "\t")
			}
			fmt.Fprint(table, topColumnValue(process, column.key))
		}
		fmt.Fprintln(table)
	}
	return table.Flush()
}

func runTop(args []string, stdout, stderr io.Writer, backend backend) error {
	jsonOutput := false
	if len(args) != 0 && args[0] == "--json" {
		jsonOutput = true
		args = args[1:]
	}
	if len(args) < 1 {
		return errors.New("usage: ttc top [--json] CONTAINER@BRANCH [PS OPTIONS]")
	}
	target, err := parseSelector(args[0])
	if err != nil {
		return err
	}
	processOptions := args[1:]
	if jsonOutput && len(processOptions) != 0 {
		return errors.New("top --json cannot be combined with process formatting options")
	}
	columns, err := parseTopProcessOptions(processOptions)
	if err != nil {
		return err
	}
	processes, err := backend.processes(target)
	if err != nil {
		return fmt.Errorf("listing processes in %s: %w", target, err)
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(processes)
	}
	return writeTopTable(stdout, processes, columns)
}

func runTTY(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 || (args[0] != "stat" && args[0] != "watch") {
		return errors.New("usage: ttc tty stat|watch CONTAINER@BRANCH --session NAME [--json]")
	}
	action := args[0]
	var targetValue, session string
	jsonOutput := false
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--json":
			jsonOutput = true
		case "--session":
			index++
			if index == len(args) {
				return errors.New("--session requires a name")
			}
			session = args[index]
		default:
			if strings.HasPrefix(args[index], "--session=") {
				session = strings.TrimPrefix(args[index], "--session=")
			} else if strings.HasPrefix(args[index], "-") {
				return fmt.Errorf("unknown tty stat option %q", args[index])
			} else if targetValue == "" {
				targetValue = args[index]
			} else {
				return fmt.Errorf("unexpected tty stat argument %q", args[index])
			}
		}
	}
	if targetValue == "" || session == "" {
		return errors.New("usage: ttc tty stat|watch CONTAINER@BRANCH --session NAME [--json]")
	}
	target, err := parseSelector(targetValue)
	if err != nil {
		return err
	}
	if action == "watch" {
		return backend.watchConsole(target, session, stdout)
	}
	observation, err := backend.consoleObservation(target, session)
	if err != nil {
		return fmt.Errorf("observing terminal session %q in %s: %w", session, target, err)
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(&observation)
	}
	_, err = fmt.Fprintf(stdout, "version=%d session_epoch=%d input_pending=%d input_readable=%t input_consumed=%d blocked_readers=%d read_waiters=%d canonical=%t echo=%t termios_generation=%d foreground_pgid=%d foreground_generation=%d session_leader_pid=%d output_bytes=%d input_flush_generation=%d replica_open_count=%d monotonic_ns=%d\n",
		observation.Version, observation.SessionEpoch, observation.InputPendingBytes,
		observation.InputReadable, observation.InputConsumedBytes, observation.ReplicaBlockedReaders,
		observation.ReplicaReadWaiters, observation.Canonical, observation.Echo,
		observation.TermiosGeneration, observation.ForegroundPGID,
		observation.ForegroundPGIDGeneration, observation.SessionLeaderPID,
		observation.OutputBytes, observation.InputFlushGeneration,
		observation.ReplicaOpenCount, observation.MonotonicNS)
	return err
}

func runFork(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	paused := fs.Bool("paused", false, "leave the child branch paused")
	quarantine := fs.Bool("quarantine", false, "cut the child off from outside the sandbox: its connections stay up and lead nowhere, and nothing outside can tell it exists")
	targetNode := fs.String("node", "", "fork into a container on another configured ttcd node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: ttc fork [--paused] [--quarantine] [--node NODE] SOURCE [DEST...]")
	}
	source, err := parseSelector(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid source: %w", err)
	}
	destinationValues := fs.Args()[1:]
	if len(destinationValues) == 0 {
		generated := "fork-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		destinationValues = []string{generated}
	}
	selectedNode := *targetNode
	remote := selectedNode != ""
	if placer, ok := backend.(distributedForkPlacementBackend); ok && !*quarantine {
		selectedNode, remote, err = placer.selectForkNode(selectedNode)
		if err != nil {
			return fmt.Errorf("selecting a node for fork: %w", err)
		}
	}
	if selectedNode != "" && *quarantine {
		return errors.New("a quarantined fork stays on this node: omit --node")
	}
	if remote {
		if *quarantine {
			return errors.New("a quarantined fork stays on this node: it exists to be asked something and thrown away")
		}
		distributed, ok := backend.(distributedForkBackend)
		if !ok {
			return errors.New("this backend does not support distributed forks")
		}
		targetContainers := make([]string, len(destinationValues))
		for i, value := range destinationValues {
			target, err := parseDestination(source.container, value)
			if err != nil {
				return fmt.Errorf("invalid destination %q: %w", value, err)
			}
			targetContainers[i] = target.String()
		}
		targets, err := distributed.forkRemoteMany(source, selectedNode, targetContainers, *paused)
		if err != nil {
			return fmt.Errorf("forking %s to node %s as %s: %w", source, selectedNode, strings.Join(destinationValues, ", "), err)
		}
		for _, target := range targets {
			fmt.Fprintln(stdout, target)
		}
		return nil
	}
	destinations := make([]selector, len(destinationValues))
	destinationBranches := make([]string, len(destinationValues))
	for i, value := range destinationValues {
		destination, err := parseDestination(source.container, value)
		if err != nil {
			return fmt.Errorf("invalid destination %q: %w", value, err)
		}
		destinations[i] = destination
		destinationBranches[i] = destination.branch
	}
	branches, err := backend.forkMany(source, destinationBranches, *paused, *quarantine)
	if err != nil {
		return fmt.Errorf("forking %s as %s: %w", source, strings.Join(destinationValues, ", "), err)
	}
	for i, branch := range branches {
		fmt.Fprintf(stdout, "%s@%s\n", destinations[i].container, branch.ID)
	}
	return nil
}

type branchOperationTarget struct {
	selector selector
	state    string
}

func branchOperationTargets(value string, backend backend, allowFamily bool) ([]branchOperationTarget, error) {
	if strings.Contains(value, "@") {
		target, err := parseSelector(value)
		if err != nil {
			return nil, err
		}
		info, err := backend.inspect(target.container)
		if err != nil {
			return nil, err
		}
		for _, branch := range info.TTC.Branches {
			if branch.Metadata.ID == target.branch {
				return []branchOperationTarget{{selector: target, state: normalizeLifecycleState(branch.Metadata.State)}}, nil
			}
		}
		return nil, fmt.Errorf("branch %q does not exist", target.branch)
	}
	if !allowFamily {
		return nil, fmt.Errorf("branch selector %q must be CONTAINER@BRANCH", value)
	}
	info, err := backend.inspect(value)
	if err != nil {
		return nil, err
	}
	targets := make([]branchOperationTarget, 0, len(info.TTC.Branches))
	for _, branch := range info.TTC.Branches {
		state := normalizeLifecycleState(branch.Metadata.State)
		targets = append(targets, branchOperationTarget{
			selector: selector{container: value, branch: branch.Metadata.ID},
			state:    state,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].selector.branch < targets[j].selector.branch })
	return targets, nil
}

func runPauseResume(args []string, stdout io.Writer, backend backend, pause bool) error {
	if len(args) == 0 {
		if pause {
			return errors.New("usage: ttc pause TARGET [TARGET...]")
		}
		return errors.New("usage: ttc resume TARGET [TARGET...]")
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

func runQuarantine(args []string, stdout io.Writer, backend backend, enabled bool) error {
	if len(args) == 0 {
		command := "quarantine"
		if !enabled {
			command = "unquarantine"
		}
		return fmt.Errorf("usage: ttc %s CONTAINER@BRANCH [CONTAINER@BRANCH...]", command)
	}
	qb, ok := backend.(quarantineBackend)
	if !ok {
		return errors.New("this backend does not support quarantine")
	}
	for _, value := range args {
		target, err := parseSelector(value)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		if err := qb.setQuarantine(target, enabled); err != nil {
			return fmt.Errorf("updating %s quarantine: %w", target, err)
		}
		fmt.Fprintln(stdout, target)
	}
	return nil
}

func runRename(args []string, stdout io.Writer, backend backend) error {
	if len(args) != 2 {
		return errors.New("usage: ttc rename CONTAINER@BRANCH NEW_BRANCH")
	}
	target, err := parseSelector(args[0])
	if err != nil {
		return fmt.Errorf("invalid target %q: %w", args[0], err)
	}
	if err := backend.renameBranch(target, args[1]); err != nil {
		return fmt.Errorf("renaming %s: %w", target, err)
	}
	fmt.Fprintf(stdout, "%s@%s\n", target.container, args[1])
	return nil
}

func runDehydrateRehydrate(args []string, stdout io.Writer, backend backend, dehydrate bool) error {
	if len(args) == 0 {
		if dehydrate {
			return errors.New("usage: ttc dehydrate (CONTAINER|CONTAINER@BRANCH) [TARGET...]")
		}
		return errors.New("usage: ttc rehydrate CONTAINER@BRANCH [CONTAINER@BRANCH...]")
	}
	db, ok := backend.(dehydrationBackend)
	if !ok {
		return errors.New("this backend does not support dehydration")
	}
	for _, value := range args {
		targets, err := branchOperationTargets(value, backend, dehydrate)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		for _, target := range targets {
			if dehydrate {
				switch target.state {
				case "running", "paused":
					err = db.dehydrate(target.selector)
				case "created", "dehydrated", "exited", "failed", "dead":
					continue
				default:
					return fmt.Errorf("cannot dehydrate %s in state %q", target.selector, target.state)
				}
			} else {
				if target.state != "dehydrated" && target.state != "dehydrating" && target.state != "materializing" {
					return fmt.Errorf("cannot rehydrate %s in state %q", target.selector, target.state)
				}
				err = db.rehydrate(target.selector)
			}
			if err != nil {
				return fmt.Errorf("updating %s: %w", target.selector, err)
			}
		}
		fmt.Fprintln(stdout, value)
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
	targets := fs.Args()
	if !flagWasSet(fs, "signal") && !flagWasSet(fs, "s") && len(targets) > 1 {
		if _, err := parseSignal(targets[len(targets)-1]); err == nil {
			signalName = targets[len(targets)-1]
			targets = targets[:len(targets)-1]
		}
	}
	if len(targets) == 0 {
		return errors.New("usage: ttc kill [--signal SIGNAL] TARGET [TARGET...] | ttc kill TARGET SIGNAL")
	}
	signal, err := parseSignal(signalName)
	if err != nil {
		return err
	}
	for _, arg := range targets {
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
	fs.IntVar(&timeout, "time", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "timeout", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "t", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc stop [--time SECONDS] (CONTAINER|CONTAINER@BRANCH) [TARGET...]")
	}
	timeoutSet := flagWasSet(fs, "time") || flagWasSet(fs, "timeout") || flagWasSet(fs, "t")
	if timeoutSet && timeout < -1 {
		return errors.New("stop timeout must be -1 or greater")
	}
	for _, value := range fs.Args() {
		targets, err := branchOperationTargets(value, backend, true)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		if len(targets) == 0 {
			return fmt.Errorf("container %q has no branches", value)
		}
		info, err := backend.inspect(targets[0].selector.container)
		if err != nil {
			return fmt.Errorf("inspecting %s before stop: %w", value, err)
		}
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		for _, target := range targets {
			if err := stopBranchLifecycle(target, stopSignal, stopTimeout, backend); err != nil {
				return err
			}
		}
		fmt.Fprintln(stdout, value)
	}
	return nil
}

func stopBranchLifecycle(target branchOperationTarget, stopSignal, timeoutSeconds int, backend backend) error {
	queueExit := func() error {
		if publisher, ok := backend.(interface{ queueBranchPublication(selector, string) error }); ok {
			if err := publisher.queueBranchPublication(target.selector, "stop"); err != nil {
				return fmt.Errorf("queueing durable filesystem publication for %s: %w", target.selector, err)
			}
		}
		return nil
	}
	switch target.state {
	case "running", "restarting":
		if err := backend.prepareStop(target.selector); err != nil {
			refreshed, refreshErr := branchOperationTargets(target.selector.String(), backend, false)
			if refreshErr == nil && len(refreshed) == 1 && refreshed[0].state == "exited" {
				if discardErr := backend.discard(target.selector); discardErr == nil {
					return nil
				}
			}
			return fmt.Errorf("preparing %s for independent stop: %w", target.selector, err)
		}
		if err := stopBranch(target.selector, stopSignal, timeoutSeconds, backend); err != nil {
			// The final physical branch on an elastic node tears down its control
			// socket as the init exits. If reconciliation confirms the requested
			// terminal state, treat the lost wait reply as a committed stop.
			refreshed, refreshErr := branchOperationTargets(target.selector.String(), backend, false)
			if refreshErr == nil && len(refreshed) == 1 && refreshed[0].state == "exited" {
				if discardErr := backend.discard(target.selector); discardErr == nil {
					return nil
				}
			}
			return err
		}
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("releasing stopped execution state for %s: %w", target.selector, err)
		}
		return queueExit()
	case "paused", "dehydrated":
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("discarding execution state for %s: %w", target.selector, err)
		}
		return queueExit()
	case "exited":
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("releasing exited execution state for %s: %w", target.selector, err)
		}
		return queueExit()
	case "created", "failed", "dead":
		return nil
	default:
		return fmt.Errorf("cannot stop %s in state %q", target.selector, target.state)
	}
	return nil
}

func runWait(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	condition := fs.String("condition", "not-running", "wait condition (not-running or durable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc wait [--condition not-running|durable] TARGET [TARGET...]")
	}
	if *condition != "not-running" && *condition != "durable" {
		return fmt.Errorf("wait condition %q is not implemented; use 'not-running' or 'durable'", *condition)
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
			if *condition == "durable" {
				waited <- waitForDurableBranch(target, backend)
				return
			}
			status, err := backend.wait(target)
			waited <- waitResult{status: status, err: err}
		}(target, waited)
	}
	for i, waited := range waiters {
		result := <-waited
		if result.err != nil {
			return fmt.Errorf("waiting for %s: %w", targets[i], result.err)
		}
		if *condition == "durable" {
			fmt.Fprintln(stdout, targets[i])
		} else {
			fmt.Fprintln(stdout, waitStatusExitCode(result.status))
		}
	}
	return nil
}

func waitForDurableBranch(target selector, backend backend) waitResult {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		family, err := backend.inspect(target.container)
		if err != nil {
			return waitResult{err: err}
		}
		found := false
		for _, inspected := range family.TTC.Branches {
			branch := inspected.Metadata
			if branch.ID != target.branch {
				continue
			}
			found = true
			switch branch.StorageState {
			case "shared":
				return waitResult{}
			case "materializing":
				if branch.StorageManifest != "" {
					return waitResult{}
				}
			case "error":
				if branch.StorageRetryAt.IsZero() {
					return waitResult{err: fmt.Errorf("storage publication failed: %s", branch.StorageError)}
				}
			}
			break
		}
		if !found {
			return waitResult{err: fmt.Errorf("branch %s does not exist", target)}
		}
		<-ticker.C
	}
}

func runRestart(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var timeout int
	fs.IntVar(&timeout, "time", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "timeout", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	fs.IntVar(&timeout, "t", ttcbundle.DefaultStopTimeoutSeconds, "seconds to wait for graceful termination; -1 waits forever")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc restart [--time SECONDS] CONTAINER@BRANCH [CONTAINER@BRANCH...]")
	}
	timeoutSet := flagWasSet(fs, "time") || flagWasSet(fs, "timeout") || flagWasSet(fs, "t")
	if timeoutSet && timeout < -1 {
		return errors.New("restart timeout must be -1 or greater")
	}
	for _, value := range fs.Args() {
		targets, err := branchOperationTargets(value, backend, false)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		target := targets[0]
		info, err := backend.inspect(target.selector.container)
		if err != nil {
			return fmt.Errorf("inspecting %s before restart: %w", target.selector, err)
		}
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		switch target.state {
		case "running", "restarting", "paused", "dehydrated":
			if err := stopBranchLifecycle(target, stopSignal, stopTimeout, backend); err != nil {
				return err
			}
		case "created", "exited", "failed", "dead":
		default:
			return fmt.Errorf("cannot restart %s in state %q", target.selector, target.state)
		}
		status, err := backend.start(target.selector, true, false)
		if err != nil {
			return fmt.Errorf("starting %s after restart: %w", target.selector, err)
		}
		if status != 0 {
			return exitStatus{code: status}
		}
		fmt.Fprintln(stdout, target.selector)
	}
	return nil
}

type waitResult struct {
	status uint32
	err    error
}

func stopPolicy(info InspectInfo) (int, int, error) {
	stopSignal := info.Config.StopSignal
	if stopSignal == "" {
		stopSignal = ttcbundle.DefaultStopSignal
	}
	signal, err := parseSignal(stopSignal)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid configured stop signal %q: %w", stopSignal, err)
	}
	timeout := ttcbundle.DefaultStopTimeoutSeconds
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
		if _, waitErr := backend.wait(target); waitErr == nil {
			return nil
		}
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
			// The graceful signal may have won the timeout race and torn down
			// the control socket before SIGKILL was delivered. Accept a
			// concurrently successful wait instead of reporting a false failure.
			select {
			case result := <-waited:
				if result.err == nil {
					return nil
				}
				return fmt.Errorf("stopping %s with SIGKILL: %w (wait also failed: %v)", target, err, result.err)
			case <-time.After(250 * time.Millisecond):
				return fmt.Errorf("stopping %s with SIGKILL: %w", target, err)
			}
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

func portBindingSpec(binding ttcbundle.PortBinding) string {
	return fmt.Sprintf("%s:%d:%d/%s", binding.HostIP, binding.HostPort, binding.ContainerPort, binding.Protocol)
}

func parsePortCommandBinding(value, branch string) (ttcbundle.PortBinding, error) {
	bindings, err := parsePublishedPorts([]string{value})
	if err != nil {
		return ttcbundle.PortBinding{}, err
	}
	binding := bindings[0]
	binding.BranchID = branch
	return binding, nil
}

func runPort(args []string, stdout, stderr io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc port publish|ls|unpublish [OPTIONS]")
	}
	switch args[0] {
	case "publish":
		if len(args) != 3 {
			return errors.New("usage: ttc port publish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
		}
		target, err := parseSelector(args[1])
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", args[1], err)
		}
		binding, err := parsePortCommandBinding(args[2], target.branch)
		if err != nil {
			return err
		}
		if err := backend.publishPort(target, binding); err != nil {
			return fmt.Errorf("publishing port for %s: %w", target, err)
		}
		fmt.Fprintf(stdout, "%s %s\n", target, portBindingSpec(binding))
		return nil
	case "ls":
		fs := flag.NewFlagSet("port ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		jsonOutput := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: ttc port ls [--json] CONTAINER@BRANCH")
		}
		value := fs.Arg(0)
		target, err := parseSelector(value)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		bindings, err := backend.listPorts(target.container)
		if err != nil {
			return err
		}
		filtered := bindings[:0]
		for _, binding := range bindings {
			if binding.Branch() == target.branch {
				filtered = append(filtered, binding)
			}
		}
		bindings = filtered
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(bindings)
		}
		table := tabwriter.NewWriter(stdout, 0, 4, 2, 32, 0)
		fmt.Fprintln(table, "TARGET\tHOST ENDPOINT\tCONTAINER PORT\tPROTOCOL")
		for _, binding := range bindings {
			fmt.Fprintf(table, "%s@%s\t%s:%d\t%d\t%s\n", target.container, binding.Branch(), binding.HostIP, binding.HostPort, binding.ContainerPort, binding.Protocol)
		}
		return table.Flush()
	case "unpublish":
		if len(args) != 3 {
			return errors.New("usage: ttc port unpublish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]")
		}
		target, err := parseSelector(args[1])
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", args[1], err)
		}
		binding, err := parsePortCommandBinding(args[2], target.branch)
		if err != nil {
			return err
		}
		if err := backend.unpublishPort(target, binding); err != nil {
			return fmt.Errorf("unpublishing port for %s: %w", target, err)
		}
		fmt.Fprintf(stdout, "%s %s\n", target, portBindingSpec(binding))
		return nil
	default:
		return fmt.Errorf("unknown port command %q", args[0])
	}
}

type selector struct {
	container string
	branch    string
}

func parseSelector(value string) (selector, error) {
	if value == "" {
		return selector{}, errors.New("selector must not be empty")
	}
	if strings.Count(value, "@") == 0 {
		return selector{}, fmt.Errorf("branch selector %q must be CONTAINER@BRANCH (for example, %s@%s)", value, value, defaultBranchID)
	}
	if strings.Count(value, "@") > 1 {
		return selector{}, errors.New("selector contains multiple '@' separators")
	}
	parts := strings.SplitN(value, "@", 2)
	if parts[0] == "" {
		return selector{}, errors.New("container name must not be empty")
	}
	if parts[1] == "" {
		return selector{}, errors.New("branch name must not be empty")
	}
	return selector{container: parts[0], branch: parts[1]}, nil
}

type execSelector struct {
	target selector
	execID string
}

func parseExecSelector(value string) (execSelector, error) {
	separator := strings.LastIndexByte(value, ':')
	if separator <= 0 || separator == len(value)-1 {
		return execSelector{}, errors.New("exec selector must be CONTAINER@BRANCH:EXEC_ID")
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
	if value == "" {
		return selector{}, errors.New("branch name must not be empty")
	}
	if strings.Contains(value, "@") {
		return selector{}, errors.New("fork destination must be a branch name, not CONTAINER@BRANCH")
	}
	return selector{container: containerID, branch: value}, nil
}

func (s selector) String() string {
	return s.container + "@" + s.branch
}

// Branch is stable branch metadata returned by ttc ls --json.
type Branch struct {
	ID                       string    `json:"id"`
	Quarantined              bool      `json:"quarantined,omitempty"`
	NodeID                   string    `json:"node_id,omitempty"`
	ParentID                 string    `json:"parent,omitempty"`
	State                    string    `json:"state"`
	StorageState             string    `json:"storage_state"`
	StorageGeneration        uint64    `json:"storage_generation,omitempty"`
	StorageError             string    `json:"storage_error,omitempty"`
	StorageManifest          string    `json:"storage_manifest,omitempty"`
	StorageRetryAt           time.Time `json:"storage_retry_at,omitempty"`
	SourceNode               string    `json:"source_node,omitempty"`
	DependentMemoryBytes     uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64    `json:"dependent_filesystem_bytes,omitempty"`
	ForkTotalNS              int64     `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS         int64     `json:"source_downtime_ns,omitempty"`
	KernelForkNS             int64     `json:"kernel_fork_ns,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
	InitPID                  int32     `json:"init_pid,omitempty"`
	InitState                string    `json:"init_state,omitempty"`
	MemorySharing            string    `json:"memory_sharing,omitempty"`
}

// Family is stable family metadata returned by ttc ls --json.
type Family struct {
	ID               string    `json:"id"`
	NodeID           string    `json:"node_id,omitempty"`
	Name             string    `json:"name,omitempty"`
	Image            string    `json:"image,omitempty"`
	Digest           string    `json:"digest,omitempty"`
	ImageSize        int64     `json:"image_size,omitempty"`
	LiveForkEligible bool      `json:"live_fork_eligible"`
	LiveForkWaiver   string    `json:"live_fork_waiver,omitempty"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	SandboxPID       int       `json:"sandbox_pid,omitempty"`
	Branches         []Branch  `json:"branches"`
	Error            string    `json:"error,omitempty"`
}

// ProcessInfo is stable branch-local process information returned by ttc top.
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

// ImageInfo is one named local image record enriched with TTC cache usage.
type ImageInfo struct {
	Reference    string            `json:"reference"`
	Digest       string            `json:"digest"`
	MediaType    string            `json:"media_type"`
	Size         int64             `json:"size"`
	Platform     string            `json:"platform"`
	Materialized bool              `json:"materialized"`
	Containers   []string          `json:"containers,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ImageInspectInfo follows Docker's top-level image inspection naming while
// exposing TTC-specific root and container use explicitly.
type ImageInspectInfo struct {
	ID           string             `json:"Id"`
	RepoTags     []string           `json:"RepoTags"`
	RepoDigests  []string           `json:"RepoDigests"`
	Created      time.Time          `json:"Created"`
	Size         int64              `json:"Size"`
	Architecture string             `json:"Architecture"`
	OS           string             `json:"Os"`
	Variant      string             `json:"Variant,omitempty"`
	MediaType    string             `json:"MediaType"`
	Materialized bool               `json:"Materialized"`
	Containers   []string           `json:"Containers,omitempty"`
	Config       ImageInspectConfig `json:"Config"`
}

// ImageInspectConfig is the Docker-compatible inspect subset TTC currently
// guarantees to round-trip when committing images.
type ImageInspectConfig struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

// ImageRemoveResult describes Docker-compatible removal output.
type ImageRemoveResult struct {
	Untagged []string
	Deleted  string
}

type imageCommitOptions struct {
	Author  string
	Message string
	Changes []string
	Pause   bool
}

func runCommit(args []string, stdout, stderr io.Writer, b backend) error {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	author := fs.String("author", "", "author (for example, Name <email@example.com>)")
	fs.StringVar(author, "a", "", "author (for example, Name <email@example.com>)")
	message := fs.String("message", "", "commit message")
	fs.StringVar(message, "m", "", "commit message")
	var changes stringSlice
	fs.Var(&changes, "change", "apply a Dockerfile instruction to the created image")
	fs.Var(&changes, "c", "apply a Dockerfile instruction to the created image")
	pause := fs.Bool("pause", true, "pause the branch during commit")
	fs.BoolVar(pause, "p", true, "pause the branch during commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: ttc commit [OPTIONS] CONTAINER@BRANCH REPOSITORY[:TAG]")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	info, err := b.commit(target, fs.Arg(1), imageCommitOptions{
		Author: *author, Message: *message, Changes: append([]string(nil), changes...), Pause: *pause,
	})
	if err != nil {
		return fmt.Errorf("committing %s: %w", target, err)
	}
	fmt.Fprintln(stdout, info.Digest)
	return nil
}

// BranchPathChange is one guest-visible path changed in a branch's writable
// TTC filesystem layer.
type BranchPathChange struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Type  string `json:"type,omitempty"`
	Mount string `json:"mount"`
}

// ResourceBytes classifies branch-attributed bytes by CoW sharing.
type ResourceBytes struct {
	PrivateBytes uint64 `json:"private_bytes"`
	SharedBytes  uint64 `json:"shared_bytes"`
	TotalBytes   uint64 `json:"total_bytes"`
}

// BranchResourceStats is one branch's resource-accounting snapshot.
type BranchResourceStats struct {
	Target                   string        `json:"target"`
	State                    string        `json:"state"`
	StorageState             string        `json:"storage_state"`
	StorageGeneration        uint64        `json:"storage_generation,omitempty"`
	SourceNode               string        `json:"source_node,omitempty"`
	DependentMemoryBytes     uint64        `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64        `json:"dependent_filesystem_bytes,omitempty"`
	ResidentMemory           ResourceBytes `json:"resident_memory"`
	MemoryBacking            ResourceBytes `json:"memory_backing"`
	Filesystem               ResourceBytes `json:"filesystem"`
	FilesystemInodes         uint64        `json:"filesystem_inodes"`
}

// FamilyResourceStats includes branch attribution and non-double-counted totals.
type FamilyResourceStats struct {
	Container                 string                `json:"container"`
	Branches                  []BranchResourceStats `json:"branches"`
	ResidentMemoryUniqueBytes uint64                `json:"resident_memory_unique_bytes"`
	MemoryBackingUniqueBytes  uint64                `json:"memory_backing_unique_bytes"`
	FilesystemUniqueBytes     uint64                `json:"filesystem_unique_bytes"`
	DependentMemoryBytes      uint64                `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes  uint64                `json:"dependent_filesystem_bytes,omitempty"`
}

type resourceStatsBackend interface {
	resourceStats(target selector) (FamilyResourceStats, error)
}

func runStats(args []string, stdout, stderr io.Writer, b backend) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc stats [--json] CONTAINER@BRANCH")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	accountant, ok := b.(resourceStatsBackend)
	if !ok {
		return errors.New("resource accounting is not supported by this backend")
	}
	stats, err := accountant.resourceStats(target)
	if err != nil {
		return fmt.Errorf("accounting %s: %w", target, err)
	}
	if *jsonOutput {
		return writeResourceStats(stdout, stats, true)
	}
	return writeResourceStats(stdout, stats, false)
}

func writeResourceStats(stdout io.Writer, stats FamilyResourceStats, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(stats)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, 24, 0)
	fmt.Fprintln(table, "TARGET\tSTATE\tSTORAGE\tMEM PRIVATE\tMEM SHARED\tMEM BACKING\tDISK PRIVATE\tDISK SHARED\tREMOTE DEPENDENT")
	for _, branch := range stats.Branches {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", branch.Target, branch.State, branch.StorageState,
			formatImageSize(int64(branch.ResidentMemory.PrivateBytes)), formatImageSize(int64(branch.ResidentMemory.SharedBytes)),
			formatImageSize(int64(branch.MemoryBacking.TotalBytes)), formatImageSize(int64(branch.Filesystem.PrivateBytes)), formatImageSize(int64(branch.Filesystem.SharedBytes)),
			formatImageSize(int64(branch.DependentMemoryBytes+branch.DependentFilesystemBytes)))
	}
	return table.Flush()
}

func runDiff(args []string, stdout, stderr io.Writer, b backend) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc diff [--json] CONTAINER@BRANCH")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	changes, err := b.diff(target)
	if err != nil {
		return fmt.Errorf("diffing %s: %w", target, err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(changes)
	}
	code := map[string]string{"add": "A", "modify": "C", "delete": "D"}
	for _, change := range changes {
		fmt.Fprintf(stdout, "%s %s\n", code[change.Kind], change.Path)
	}
	return nil
}

type volumeCommandBackend interface {
	createVolume(name string) (ttcvolume.VolumeInfo, error)
	listVolumes() ([]ttcvolume.VolumeInfo, error)
	removeVolume(name string, force bool) error
	volumeDriver() (string, error)
}

func runVolume(args []string, stdout io.Writer, b backend) error {
	volumes, ok := b.(volumeCommandBackend)
	if !ok {
		return errors.New("volume commands are not supported by this backend")
	}
	if len(args) == 0 {
		return errors.New("usage: ttc volume create|ls|inspect|rm")
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return errors.New("usage: ttc volume create NAME")
		}
		info, err := volumes.createVolume(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, info.Name)
		return nil
	case "ls", "list":
		if len(args) != 1 {
			return errors.New("usage: ttc volume ls")
		}
		list, err := volumes.listVolumes()
		if err != nil {
			return err
		}
		driver, err := volumes.volumeDriver()
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "DRIVER\tVOLUME NAME")
		for _, info := range list {
			fmt.Fprintf(tw, "%s\t%s\n", driver, info.Name)
		}
		return tw.Flush()
	case "inspect":
		if len(args) < 2 {
			return errors.New("usage: ttc volume inspect NAME [NAME...]")
		}
		list, err := volumes.listVolumes()
		if err != nil {
			return err
		}
		driver, err := volumes.volumeDriver()
		if err != nil {
			return err
		}
		byName := make(map[string]ttcvolume.VolumeInfo, len(list))
		for _, info := range list {
			byName[info.Name] = info
			byName[info.ID] = info
		}
		result := make([]map[string]any, 0, len(args)-1)
		for _, name := range args[1:] {
			info, ok := byName[name]
			if !ok {
				return fmt.Errorf("volume %q does not exist", name)
			}
			result = append(result, map[string]any{
				"Name": info.Name, "Driver": driver,
				"CreatedAt": info.CreatedAt, "Scope": "local",
				"Labels": map[string]string{}, "Options": map[string]string{},
				"TTC": map[string]any{"ID": info.ID, "NextInode": info.NextInode},
			})
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	case "rm", "remove":
		fs := flag.NewFlagSet("volume rm", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		force := fs.Bool("force", false, "do not report an error if the volume is already absent")
		fs.BoolVar(force, "f", false, "remove even when referenced")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("usage: ttc volume rm [--force] NAME [NAME...]")
		}
		for _, name := range fs.Args() {
			if err := volumes.removeVolume(name, *force); err != nil {
				return err
			}
			fmt.Fprintln(stdout, name)
		}
		return nil
	default:
		return fmt.Errorf("unknown volume command %q", args[0])
	}
}

type backend interface {
	pull(reference, platform string, progress bool) (ttcimages.Info, error)
	loadImages(archive io.Reader, platform string) ([]ttcimages.Info, error)
	saveImages(archive io.Writer, identifiers []string, platform string) error
	listImages() ([]ImageInfo, error)
	inspectImage(identifier string) (ImageInspectInfo, error)
	removeImage(identifier string, force bool) (ImageRemoveResult, error)
	commit(target selector, reference string, options imageCommitOptions) (ttcimages.Info, error)
	create(request runRequest) (string, error)
	run(request runRequest) (containerRunResult, error)
	start(target selector, detach, interactive bool) (int, error)
	attach(request attachRequest) (uint32, error)
	logs(target selector, options logsOptions) error
	exec(request execRequest) (int, error)
	execSessions(target selector) ([]runscExecSession, error)
	waitExecSession(target selector, execID string) (uint32, error)
	signalExecSession(target selector, execID string, signal int) error
	inspect(name string) (InspectInfo, error)
	remove(name string, force, volumes bool) error
	listFamilies() ([]Family, error)
	processes(target selector) ([]ProcessInfo, error)
	consoleObservation(target selector, session string) (kernel.InternalConsoleObservation, error)
	watchConsole(target selector, session string, output io.Writer) error
	rootfsSize(target selector) (uint64, error)
	diff(target selector) ([]BranchPathChange, error)
	fork(source selector, destination string, paused bool) (Branch, error)
	forkMany(source selector, destinations []string, paused, quarantine bool) ([]Branch, error)
	publishPort(target selector, binding ttcbundle.PortBinding) error
	listPorts(container string) ([]ttcbundle.PortBinding, error)
	unpublishPort(target selector, binding ttcbundle.PortBinding) error
	pause(target selector) error
	resume(target selector) error
	renameBranch(target selector, name string) error
	prepareStop(target selector) error
	discard(target selector) error
	destroy(target selector, force bool) error
	signal(target selector, signal int, all bool) error
	wait(target selector) (uint32, error)
}

type dehydrationBackend interface {
	dehydrate(target selector) error
	rehydrate(target selector) error
}

type quarantineBackend interface {
	setQuarantine(target selector, enabled bool) error
}

type consoleSidecarOwner interface {
	OwnConsoleSidecar(key string, command *exec.Cmd) error
	StopConsoleSidecar(key string) (bool, error)
	OwnPortSidecar(key string, command *exec.Cmd) error
	StopPortSidecar(key string) (bool, error)
	RegisterConsoleClient(target selector, cancel func()) (func(), error)
	QuiesceConsoleClients(target selector) (func(), error)
}

func (b *runtimeBackend) stopConsoleDrain(paths ttcbundle.ConsolePaths) error {
	if paths.Lock == "" {
		return nil
	}
	if b.consoleSidecars != nil {
		stopped, err := b.consoleSidecars.StopConsoleSidecar(paths.Lock)
		if err != nil {
			return err
		}
		if stopped {
			_ = os.Remove(paths.PID)
			_ = os.Remove(paths.WaitStatus)
			return nil
		}
	}
	pidData, err := os.ReadFile(paths.PID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid console follower PID %q", strings.TrimSpace(string(pidData)))
	}
	lockFile, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = os.Remove(paths.PID)
		_ = os.Remove(paths.WaitStatus)
		return nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if err := waitForPortLock(lockFile, 5*time.Second); err != nil {
		return fmt.Errorf("waiting for console follower %d: %w", pid, err)
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = os.Remove(paths.PID)
	_ = os.Remove(paths.WaitStatus)
	return nil
}

func (b *runtimeBackend) stopContainerConsoleDrains(containerID string) error {
	consoleDir := filepath.Join(b.activeDataRoot(), "containers", containerID, "console")
	var result error
	err := filepath.WalkDir(consoleDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			return nil
		}
		base := strings.TrimSuffix(path, ".pid")
		if stopErr := b.stopConsoleDrain(ttcbundle.ConsolePaths{
			Lock:       base + ".lock",
			WaitStatus: base + ".wait-status",
			PID:        path,
		}); stopErr != nil {
			result = errors.Join(result, fmt.Errorf("stopping console follower %q: %w", path, stopErr))
		}
		return nil
	})
	if os.IsNotExist(err) {
		return result
	}
	return errors.Join(result, err)
}

type runtimeBackend struct {
	root                  string
	runsc                 string
	dataRoot              string
	localRoot             string
	daemonSocket          string
	runtimePlatform       string
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	volumeCopyMode        ttcvolume.CopyMode
	clientTERM            string
	progressTerminal      bool
	progressWidth         int
	stdin                 io.Reader
	stdout                io.Writer
	stderr                io.Writer
	consoleSidecars       consoleSidecarOwner
	distributedSource     *distributedSourceRuntime
	nodeID                string
	cancelClient          func()
	clientCanceled        <-chan struct{}
	// lifecycleReady releases the daemon lifecycle lock after an attached
	// run/start has finished creating its runtime but before it waits for the
	// guest. Detached commands release the lock when the command returns.
	lifecycleReady   func()
	publicationQueue *ttcstorage.Queue
	snapshotGraph    *ttcsnapshot.Store
	snapshotData     *ttcsnapshot.LocalDataStore
	snapshotResolver *ttcsnapshot.Resolver
	// daemonStopRequested is set only after a node-stop policy has completed.
	// The daemon sends the command result before closing its listener.
	daemonStopRequested bool
}

func (b *runtimeBackend) signalLifecycleReady() {
	if b.lifecycleReady != nil {
		b.lifecycleReady()
		b.lifecycleReady = nil
	}
}

func (b *runtimeBackend) registerConsoleClient(target selector) (func(), error) {
	if b.consoleSidecars == nil || b.cancelClient == nil {
		return func() {}, nil
	}
	return b.consoleSidecars.RegisterConsoleClient(target, b.cancelClient)
}

func (b *runtimeBackend) quiesceConsoleClients(target selector) (func(), error) {
	if b.consoleSidecars == nil {
		return func() {}, nil
	}
	return b.consoleSidecars.QuiesceConsoleClients(target)
}

func (b *runtimeBackend) consoleClientCanceled() bool {
	if b.clientCanceled == nil {
		return false
	}
	select {
	case <-b.clientCanceled:
		return true
	default:
		return false
	}
}

func terminalType(value string) string {
	if value == "" {
		return "xterm"
	}
	return value
}

func environmentHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, assignment := range env {
		if strings.HasPrefix(assignment, prefix) {
			return true
		}
	}
	return false
}

func (b *runtimeBackend) containerStore() ttcbundle.Store {
	store := ttcbundle.Store{Root: b.dataRoot, ActiveRoot: b.activeDataRoot()}
	if b.root != "" {
		store.TransientRoot = filepath.Join(b.root, "ttc-transient-bundles")
	}
	return store
}

func (b *runtimeBackend) activeDataRoot() string {
	if b.localRoot != "" {
		return b.localRoot
	}
	return b.dataRoot
}

func (b *runtimeBackend) queueBranchPublication(target selector, kind string) error {
	if b.publicationQueue == nil {
		if kind == "dehydrate" && b.dataRoot != "" {
			return b.containerStore().UpdateBranchState(target.container, target.branch, "dehydrated")
		}
		return nil
	}
	store := b.containerStore()
	// Publication intents are durable and may outlive the user-facing name or
	// the daemon process that created them. Key them by the immutable container
	// ID so publish completion, branch removal fencing, and restart recovery all
	// address the same generation even when the lifecycle command used a name
	// or ID prefix.
	container, err := store.LoadContainer(target.container)
	if err != nil {
		return err
	}
	target.container = container.ID
	previous, err := persistentBranchInfo(container, target.branch)
	if err != nil {
		return err
	}
	if previous.StorageGeneration != 0 {
		if err := b.publicationQueue.CancelPending(target.container, target.branch, previous.StorageGeneration); err != nil {
			return fmt.Errorf("canceling superseded publication for %s: %w", target, err)
		}
	}
	generation, err := store.BeginBranchPublication(target.container, target.branch)
	if err != nil {
		return err
	}
	prePublication, err := persistentBranchInfoFromStore(store, target)
	if err != nil {
		return err
	}
	captureBranch := fmt.Sprintf("ttc-publish-%016x", generation)
	intent := ttcstorage.Intent{
		Container: target.container, Branch: target.branch, Generation: generation, Kind: kind,
		FilesystemBranch: captureBranch,
		ParentManifest:   prePublication.StorageManifest,
	}
	if intent.ParentManifest == "" && prePublication.ParentID != "" {
		if parent, parentErr := persistentBranchInfoFromStore(store, selector{container: target.container, branch: prePublication.ParentID}); parentErr == nil {
			intent.ParentManifest = parent.StorageManifest
		}
	}
	if kind == "dehydrate" {
		intent.MemoryBranch = captureBranch
		intent.MemoryControl = captureBranch
	}
	if err := b.publicationQueue.Enqueue(intent); err != nil {
		_ = store.UpdateBranchStorage(target.container, target.branch, generation, "error", err.Error(), time.Time{})
		return err
	}
	captureComplete := false
	var forks []*ttcvolume.ForkTransaction
	var repositories []*ttcvolume.Repository
	memoryPinned := false
	memoryControlCaptured := false
	defer func() {
		for _, repository := range repositories {
			_ = repository.Close()
		}
	}()
	rollback := func(cause error) error {
		if !captureComplete {
			for i := len(forks) - 1; i >= 0; i-- {
				cause = errors.Join(cause, forks[i].Abort())
			}
			if memoryPinned {
				info, loadErr := store.LoadContainer(target.container)
				if loadErr == nil {
					repository, openErr := ttcchunk.OpenMemoryRepository(filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks"))
					if openErr == nil {
						removeErr := repository.DeleteBranch(captureBranch)
						if errors.Is(removeErr, os.ErrNotExist) {
							removeErr = nil
						}
						cause = errors.Join(cause, removeErr, repository.Close())
					}
				}
			}
			if memoryControlCaptured {
				cause = errors.Join(cause, os.RemoveAll(publicationMemoryControlPath(b.activeDataRoot(), captureBranch)))
			}
		}
		cause = errors.Join(cause, b.publicationQueue.Cancel(target.container, target.branch, generation))
		_ = store.UpdateBranchStorage(target.container, target.branch, generation, "local", "", time.Time{})
		return cause
	}
	info, err := store.LoadContainer(target.container)
	if err != nil {
		return rollback(err)
	}
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return rollback(err)
	}
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = offlineCopyBranch(info, branch.ID)
	}
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return rollback(err)
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return rollback(err)
	}
	seen := make(map[string]struct{})
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return rollback(err)
		}
		repositories = append(repositories, repository)
		fork, err := repository.BeginFork(filesystemBranch, captureBranch)
		if err != nil {
			return rollback(fmt.Errorf("sealing repository %q: %w", config.Source, err))
		}
		forks = append(forks, fork)
	}
	if kind == "dehydrate" {
		repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks"))
		if err != nil {
			return rollback(err)
		}
		manifest, branchErr := repository.Branch(filesystemBranch)
		if branchErr == nil {
			branchErr = repository.SetBranch(captureBranch, manifest)
			memoryPinned = branchErr == nil
		}
		closeErr := repository.Close()
		if branchErr != nil || closeErr != nil {
			return rollback(errors.Join(branchErr, closeErr))
		}
		source := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
		destination := publicationMemoryControlPath(b.activeDataRoot(), captureBranch)
		if err := snapshotImmutableTree(source, destination); err != nil {
			return rollback(fmt.Errorf("sealing dehydration control state: %w", err))
		}
		memoryControlCaptured = true
	}
	for _, fork := range forks {
		if err := fork.Commit(); err != nil {
			return rollback(err)
		}
	}
	captureComplete = true
	if err := b.publicationQueue.Activate(target.container, target.branch, generation); err != nil {
		// The committed hidden roots are immutable and safe for retry. Keep the
		// pending intent so daemon restart can validate and activate it.
		_ = store.UpdateBranchStorage(target.container, target.branch, generation, "error", err.Error(), time.Time{})
		return err
	}
	return nil
}

func persistentBranchInfoFromStore(store ttcbundle.Store, target selector) (ttcbundle.BranchInfo, error) {
	info, err := store.LoadContainer(target.container)
	if err != nil {
		return ttcbundle.BranchInfo{}, err
	}
	return persistentBranchInfo(info, target.branch)
}

func (b *runtimeBackend) imageProgress() ttcimages.ProgressFunc {
	return newImageProgressWithTerminal(b.stderr, b.progressTerminal, b.progressWidth)
}

func (b *runtimeBackend) resolveContainer(identifier string) (ttcbundle.ContainerInfo, error) {
	if b.dataRoot == "" {
		return ttcbundle.ContainerInfo{ID: identifier, Name: identifier}, nil
	}
	info, err := b.containerStore().LoadContainer(identifier)
	if err != nil {
		return ttcbundle.ContainerInfo{}, err
	}
	return info, nil
}

func (b *runtimeBackend) resolveSelector(target selector) (selector, ttcbundle.ContainerInfo, error) {
	info, err := b.resolveContainer(target.container)
	if err != nil {
		return selector{}, ttcbundle.ContainerInfo{}, err
	}
	if b.nodeID != "" {
		if b.distributedSource != nil && b.distributedSource.authority != nil {
			if authority, authorityErr := b.distributedSource.authority.Branch(info.ID, target.branch); authorityErr == nil && authority.Corrupted {
				target.container = info.ID
				return target, info, nil
			} else if authorityErr != nil && !errors.Is(authorityErr, os.ErrNotExist) {
				return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("resolving authority for %s: %w", target, authorityErr)
			}
		}
		owner := info.BranchNodeID(target.branch)
		if owner != "" && owner != b.nodeID {
			return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("branch %s belongs to cluster node %q", target, owner)
		}
	}
	target.container = info.ID
	return target, info, nil
}

func (b *runtimeBackend) imageRecords() ([]ttcimages.Info, error) {
	return ttcimages.List(context.Background(), ttcimages.ListOptions{
		Root: b.dataRoot,
	})
}

func (b *runtimeBackend) listImages() ([]ImageInfo, error) {
	records, err := b.imageRecords()
	if err != nil {
		return nil, err
	}
	store := b.containerStore()
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
			Materialized: materialized,
			Containers:   containerNames,
			Labels:       cloneStringMap(record.Labels),
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

func (b *runtimeBackend) commit(target selector, reference string, options imageCommitOptions) (result ttcimages.Info, retErr error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return ttcimages.Info{}, err
	}
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return ttcimages.Info{}, err
	}
	if options.Pause && normalizeLifecycleState(branch.State) == "running" {
		if err := b.pause(resolved); err != nil {
			return ttcimages.Info{}, fmt.Errorf("pausing branch for commit: %w", err)
		}
		defer func() {
			if err := b.resume(resolved); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("resuming branch after commit: %w", err))
			}
		}()
	}
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = offlineCopyBranch(info, resolved.branch)
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return ttcimages.Info{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return ttcimages.Info{}, err
	}
	var rootfs *specutils.TTCVolumeRepository
	for i := range repositories {
		if repositories[i].Source == specutils.TTCRootfsUpperSource {
			rootfs = &repositories[i]
			break
		}
	}

	reader, writer := io.Pipe()
	exportDone := make(chan error, 1)
	var repository *ttcvolume.Repository
	temporaryBranch := ""
	if rootfs == nil {
		go func() {
			tarWriter := tar.NewWriter(writer)
			err := tarWriter.Close()
			_ = writer.CloseWithError(err)
			exportDone <- err
		}()
	} else {
		repository, err = ttcvolume.OpenRepository(rootfs.Path, ttcvolume.CopyMode(rootfs.CopyMode))
		if err != nil {
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, fmt.Errorf("opening rootfs upper: %w", err)
		}
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			repository.Close()
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, err
		}
		temporaryBranch = "commit-" + hex.EncodeToString(random[:])
		if err := repository.Fork(filesystemBranch, temporaryBranch); err != nil {
			repository.Close()
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, fmt.Errorf("snapshotting rootfs upper: %w", err)
		}
		defer func() {
			cleanupErr := repository.RemoveBranch(temporaryBranch)
			if cleanupErr == nil {
				cleanupErr = repository.GarbageCollect()
			}
			cleanupErr = errors.Join(cleanupErr, repository.Close())
			if cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("removing commit snapshot: %w", cleanupErr))
			}
		}()
		go func() {
			err := repository.WriteLayerTar(temporaryBranch, writer)
			_ = writer.CloseWithError(err)
			exportDone <- err
		}()
	}
	result, commitErr := ttcimages.Commit(context.Background(), ttcimages.CommitOptions{
		Root: b.dataRoot, BaseDigest: info.Digest, Reference: reference, Layer: reader,
		Author: options.Author, Message: options.Message, Changes: options.Changes,
	})
	_ = reader.Close()
	exportErr := <-exportDone
	if commitErr != nil || exportErr != nil {
		return ttcimages.Info{}, errors.Join(commitErr, exportErr)
	}
	return result, nil
}

func inspectImageFromInventory(infos []ImageInfo, identifier string) (ImageInspectInfo, error) {
	records := make([]ttcimages.Info, 0, len(infos))
	for _, info := range infos {
		records = append(records, ttcimages.Info{
			Reference: info.Reference,
			Digest:    info.Digest,
		})
	}
	matches, err := ttcimages.Resolve(records, identifier)
	if err != nil {
		return ImageInspectInfo{}, err
	}
	digest := matches[0].Digest
	var selected *ImageInfo
	var tags, repoDigests, containers []string
	seenRepoDigest := make(map[string]bool)
	seenContainer := make(map[string]bool)
	materialized := false
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
		Materialized: materialized,
		Containers:   containers,
		Config:       ImageInspectConfig{Labels: cloneStringMap(selected.Labels)},
	}, nil
}

func (b *runtimeBackend) removeImage(identifier string, force bool) (ImageRemoveResult, error) {
	records, err := b.imageRecords()
	if err != nil {
		return ImageRemoveResult{}, err
	}
	matches, err := ttcimages.Resolve(records, identifier)
	if err != nil {
		return ImageRemoveResult{}, err
	}
	digest := matches[0].Digest
	store := b.containerStore()
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
		if err := ttcimages.Delete(context.Background(), ttcimages.DeleteOptions{
			Root:      b.dataRoot,
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

func (b *runtimeBackend) volumeManager() (*ttcvolume.Manager, error) {
	manager, err := ttcvolume.OpenManager(b.activeDataRoot())
	if err != nil {
		return nil, err
	}
	if b.volumeCopyMode != "" && manager.Capability().Mode != b.volumeCopyMode {
		return nil, fmt.Errorf("volume backing capability changed while ttcd was running: startup mode was %s, current mode is %s", b.volumeCopyMode, manager.Capability().Mode)
	}
	return manager, nil
}

func (b *runtimeBackend) createVolume(name string) (ttcvolume.VolumeInfo, error) {
	manager, err := b.volumeManager()
	if err != nil {
		return ttcvolume.VolumeInfo{}, err
	}
	repository, err := manager.Create(name)
	if err != nil {
		return ttcvolume.VolumeInfo{}, err
	}
	defer repository.Close()
	return repository.Info(), nil
}

func (b *runtimeBackend) listVolumes() ([]ttcvolume.VolumeInfo, error) {
	manager, err := b.volumeManager()
	if err != nil {
		return nil, err
	}
	return manager.List()
}

func (b *runtimeBackend) volumeDriver() (string, error) {
	manager, err := b.volumeManager()
	if err != nil {
		return "", err
	}
	return "ttc-" + string(manager.Capability().Mode), nil
}

func (b *runtimeBackend) removeVolume(name string, force bool) error {
	manager, err := b.volumeManager()
	if err != nil {
		return err
	}
	repository, err := manager.Open(name)
	if err != nil {
		if force && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	info := repository.Info()
	repository.Close()
	store := ttcbundle.Store{Root: b.dataRoot}
	containers, err := store.ListContainers()
	if err != nil {
		return err
	}
	var users []string
	for _, container := range containers {
		for _, volume := range container.Volumes {
			if volume.ID == info.ID {
				users = append(users, container.Name)
				break
			}
		}
	}
	if len(users) != 0 {
		sort.Strings(users)
		return fmt.Errorf("volume %q is used by container(s) %s and cannot be removed", info.Name, strings.Join(users, ", "))
	}
	return manager.Remove(info.ID)
}

func validateImageRemoval(digest string, users []string, force bool) error {
	if len(users) != 0 && !force {
		return fmt.Errorf("image %s is used by container(s) %s; use --force to remove its image records", digest, strings.Join(users, ", "))
	}
	return nil
}

func (b *runtimeBackend) initialConsoleSize(enabled bool) (*specs.Box, error) {
	if !enabled {
		return nil, nil
	}
	cols, rows, ok, err := hostTerminalSize(b.stdin)
	if err != nil || !ok {
		return nil, err
	}
	return &specs.Box{Height: uint(rows), Width: uint(cols)}, nil
}

func (b *runtimeBackend) run(request runRequest) (containerRunResult, error) {
	consoleSize, err := b.initialConsoleSize(request.terminal)
	if err != nil {
		return containerRunResult{}, err
	}
	store := b.containerStore()
	prepared, err := store.Prepare(context.Background(), ttcbundle.PrepareOptions{
		SpecOptions: ttcbundle.SpecOptions{
			Name:             request.name,
			Args:             request.args,
			Env:              request.env,
			Labels:           request.labels,
			Cwd:              request.cwd,
			User:             request.user,
			Hostname:         request.hostname,
			Terminal:         request.terminal,
			TerminalType:     terminalType(b.clientTERM),
			ConsoleSize:      consoleSize,
			NetworkMode:      request.networkMode,
			ExtraHosts:       request.extraHosts,
			DNS:              request.dns,
			DNSSearch:        request.dnsSearch,
			DNSOptions:       request.dnsOptions,
			ReadonlyRootfs:   request.readonlyRootfs,
			StopSignal:       request.stopSignal,
			PidsLimit:        request.pidsLimit,
			MemoryLimit:      request.memoryLimit,
			NanoCPUs:         request.nanoCPUs,
			CPUShares:        request.cpuShares,
			CPUPeriod:        request.cpuPeriod,
			CPUQuota:         request.cpuQuota,
			ShmSize:          &request.shmSize,
			GroupAdd:         request.groupAdd,
			CapAdd:           request.capAdd,
			CapDrop:          request.capDrop,
			Ulimits:          request.ulimits,
			Entrypoint:       request.entrypoint,
			HostMounts:       request.hostMounts,
			FilesystemMounts: request.filesystemMounts,
			ManagedVolumes:   request.managedVolumes,
		},
		Reference:          request.reference,
		PullPolicy:         request.pullPolicy,
		LogDriver:          request.logDriver,
		LogOptions:         request.logOptions,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
		OpenStdin:          request.interactive,
		AutoRemove:         request.autoRemove,
		StopTimeout:        &request.stopTimeout,
		NetworkNAT:         request.networkNAT,
		NetworkIP:          request.networkIP,
		NetworkMAC:         request.networkMAC,
		PublishedPorts:     request.publishedPorts,
		WaiveLiveFork:      !request.liveForkEligible,
		LiveForkWaiver:     request.liveForkWaiver,
		Progress:           b.imageProgress(),
		VolumeCopyMode:     b.volumeCopyMode,
		NodeID:             b.nodeID,
	})
	if err != nil {
		return containerRunResult{}, err
	}
	id := prepared.Container.ID
	target := selector{container: id, branch: defaultBranchID}
	status, err := b.launch(prepared.Container, target, prepared.Container.BundleDir, request.detach, request.interactive)
	if err == nil {
		b.recordLaunchState(target, request.detach, status)
	}
	if request.autoRemove {
		status, err = b.applyAutoRemove(target, request.detach, status, err)
	}
	return containerRunResult{ID: id, Status: status}, err
}

func (b *runtimeBackend) create(request runRequest) (string, error) {
	consoleSize, err := b.initialConsoleSize(request.terminal)
	if err != nil {
		return "", err
	}
	store := b.containerStore()
	prepared, err := store.Prepare(context.Background(), ttcbundle.PrepareOptions{
		SpecOptions: ttcbundle.SpecOptions{
			Name:             request.name,
			Args:             request.args,
			Env:              request.env,
			Labels:           request.labels,
			Cwd:              request.cwd,
			User:             request.user,
			Hostname:         request.hostname,
			Terminal:         request.terminal,
			TerminalType:     terminalType(b.clientTERM),
			ConsoleSize:      consoleSize,
			NetworkMode:      request.networkMode,
			ExtraHosts:       request.extraHosts,
			DNS:              request.dns,
			DNSSearch:        request.dnsSearch,
			DNSOptions:       request.dnsOptions,
			ReadonlyRootfs:   request.readonlyRootfs,
			StopSignal:       request.stopSignal,
			PidsLimit:        request.pidsLimit,
			MemoryLimit:      request.memoryLimit,
			NanoCPUs:         request.nanoCPUs,
			CPUShares:        request.cpuShares,
			CPUPeriod:        request.cpuPeriod,
			CPUQuota:         request.cpuQuota,
			ShmSize:          &request.shmSize,
			GroupAdd:         request.groupAdd,
			CapAdd:           request.capAdd,
			CapDrop:          request.capDrop,
			Ulimits:          request.ulimits,
			Entrypoint:       request.entrypoint,
			HostMounts:       request.hostMounts,
			FilesystemMounts: request.filesystemMounts,
			ManagedVolumes:   request.managedVolumes,
		},
		Reference:          request.reference,
		PullPolicy:         request.pullPolicy,
		LogDriver:          request.logDriver,
		LogOptions:         request.logOptions,
		ConsoleLogMaxBytes: request.consoleLogMaxBytes,
		ConsoleLogMaxFiles: request.consoleLogMaxFiles,
		OpenStdin:          request.interactive,
		AutoRemove:         request.autoRemove,
		StopTimeout:        &request.stopTimeout,
		NetworkNAT:         request.networkNAT,
		NetworkIP:          request.networkIP,
		NetworkMAC:         request.networkMAC,
		PublishedPorts:     request.publishedPorts,
		WaiveLiveFork:      !request.liveForkEligible,
		LiveForkWaiver:     request.liveForkWaiver,
		Progress:           b.imageProgress(),
		VolumeCopyMode:     b.volumeCopyMode,
		NodeID:             b.nodeID,
	})
	if err != nil {
		return "", err
	}
	return prepared.Container.ID, nil
}

func (b *runtimeBackend) start(target selector, detach, interactive bool) (int, error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return 1, err
	}
	target = resolved
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return 1, err
	}
	switch normalizeLifecycleState(branch.State) {
	case "created", "exited", "failed", "dead":
	case "dehydrated":
		return 1, fmt.Errorf("branch %s is dehydrated; use rehydrate to preserve it or stop it before starting fresh", target)
	default:
		return 1, fmt.Errorf("branch %s is %s", target, normalizeLifecycleState(branch.State))
	}
	preservedPeers, err := freshStartPeers(info, target)
	if err != nil {
		return 1, err
	}
	if len(preservedPeers) != 0 {
		if err := b.startFreshBranchBesidePeers(info, target, branch); err != nil {
			return 1, err
		}
		if err := b.containerStore().RecordBranchStart(target.container, target.branch); err != nil {
			_, _ = b.command("timetravel", target.container, "destroy", target.branch)
			return 1, fmt.Errorf("recording fresh start for %s: %w", target, err)
		}
		return b.finishPreservedPeerStart(info, target, detach, interactive)
	}
	// Waiting for the branch init process and tearing down its sandbox are
	// separate events. In particular, a multi-process container may report its
	// init's exit just before runsc changes the old runtime from running to
	// stopped. A restart must not race that short teardown window, but an
	// ordinary attempt to start a persistently-running container should still
	// fail immediately.
	waitForTeardown := true
	if err := b.removeStoppedRuntime(info.ID, waitForTeardown); err != nil {
		return 1, fmt.Errorf("preparing a fresh runtime for %s: %w", target, err)
	}
	if b.dataRoot != "" {
		store := b.containerStore()
		info, err = store.RefreshContainerImageRoot(context.Background(), info.ID)
		if err != nil {
			return 1, err
		}
		if _, err := store.PruneImageRoots(context.Background()); err != nil {
			return 1, fmt.Errorf("pruning superseded image roots: %w", err)
		}
		branch, err = persistentBranchInfo(info, target.branch)
		if err != nil {
			return 1, err
		}
		if branch.StorageManifest != "" {
			if err := b.ensureLocalColdFilesystem(info, branch); err != nil {
				return 1, fmt.Errorf("materializing durable filesystem for %s: %w", target, err)
			}
		}
	}
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		if target.branch == defaultBranchID {
			filesystemBranch = ttcvolume.MainBranch
		} else {
			filesystemBranch = ttcvolume.ScopedBranchID(info.ID, target.branch)
		}
	}
	bundleDir, cleanupBundle, err := b.containerStore().PrepareBranchStartBundle(info.ID, target.branch, filesystemBranch, false)
	if err != nil {
		return 1, err
	}
	defer cleanupBundle()
	status, err := b.launch(info, target, bundleDir, detach, interactive)
	if err == nil {
		b.recordLaunchState(target, detach, status)
	}
	if info.AutoRemove {
		return b.applyAutoRemove(target, detach, status, err)
	}
	return status, err
}

// freshStartPeers returns active branches that must survive replacement of the
// family's Sentry. Process-free peers need no work. Transient states are
// rejected because daemon mutation serialization should never expose one here
// unless an earlier operation was interrupted.
func freshStartPeers(info ttcbundle.ContainerInfo, target selector) ([]selector, error) {
	peers := make([]selector, 0, len(info.Branches))
	for _, branch := range info.Branches {
		if branch.ID == target.branch {
			continue
		}
		switch normalizeLifecycleState(branch.State) {
		case "running", "paused":
			peers = append(peers, selector{container: target.container, branch: branch.ID})
		case "created", "dehydrated", "exited", "failed", "dead":
		case "creating", "dehydrating", "rehydrating", "restarting":
			return nil, fmt.Errorf("cannot start %s while peer %s@%s is %s", target, target.container, branch.ID, normalizeLifecycleState(branch.State))
		default:
			return nil, fmt.Errorf("cannot start %s while peer %s@%s has unknown state %q", target, target.container, branch.ID, branch.State)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].branch < peers[j].branch })
	return peers, nil
}

// startFreshBranchBesidePeers constructs target in a short-lived paused Sentry
// and imports that never-executed Kernel into the family's existing Sentry.
// Active peers are neither serialized nor replaced; the work is bounded by the
// fresh target's bootstrap state rather than the aggregate size of its peers.
func (b *runtimeBackend) startFreshBranchBesidePeers(info ttcbundle.ContainerInfo, target selector, branch ttcbundle.BranchInfo) error {
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		if target.branch == defaultBranchID {
			filesystemBranch = ttcvolume.MainBranch
		} else {
			filesystemBranch = ttcvolume.ScopedBranchID(info.ID, target.branch)
		}
	}
	memoryRoot := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory")
	if err := os.MkdirAll(memoryRoot, 0700); err != nil {
		return fmt.Errorf("creating branch bootstrap memory root: %w", err)
	}
	stagingDir, err := os.MkdirTemp(memoryRoot, ".fresh-start-")
	if err != nil {
		return fmt.Errorf("creating branch bootstrap directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	bundleDir, cleanupBundle, err := b.containerStore().PrepareTransientBranchStartBundle(info.ID, target.branch, filesystemBranch)
	if err != nil {
		return err
	}
	defer cleanupBundle()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("creating branch bootstrap runtime ID: %w", err)
	}
	runtimeID := "ttc-bootstrap-" + hex.EncodeToString(random)
	configured := b.configuredRunscArgsForBranch(info, branch)
	status, err := b.runConfiguredCommandWithArgs(configured, "create", "--bundle", bundleDir, runtimeID)
	if err != nil {
		return fmt.Errorf("creating paused bootstrap for %s: %w", target, err)
	}
	if status != 0 {
		return fmt.Errorf("creating paused bootstrap for %s exited with status %d", target, status)
	}
	bootstrapCreated := true
	defer func() {
		if bootstrapCreated {
			_, _ = b.command("delete", "--force", runtimeID)
		}
	}()
	status, err = b.runConfiguredCommandWithArgs(configured, "start", runtimeID)
	if err != nil {
		return fmt.Errorf("starting paused bootstrap for %s: %w", target, err)
	}
	if status != 0 {
		return fmt.Errorf("starting paused bootstrap for %s exited with status %d", target, status)
	}
	if err := b.waitForPausedBootstrap(runtimeID, target.branch, 5*time.Second); err != nil {
		return fmt.Errorf("waiting for paused bootstrap for %s: %w", target, err)
	}
	capture, metadata, snapshots, err := b.captureTransientBranch(runtimeID, target.branch, stagingDir)
	if err != nil {
		return fmt.Errorf("capturing paused bootstrap for %s: %w", target, err)
	}
	defer metadata.Close()
	defer func() {
		for _, snapshot := range snapshots {
			_ = snapshot.Close()
		}
	}()
	if _, err := b.command("delete", "--force", runtimeID); err != nil {
		return fmt.Errorf("releasing paused bootstrap for %s: %w", target, err)
	}
	bootstrapCreated = false

	// An exited incarnation may still have a branch record in the live Sentry.
	// Remove only that dead Kernel; its persistent filesystem branch is owned by
	// ttcd and remains available to the imported replacement.
	if output, err := b.command("timetravel", info.ID, "branches"); err == nil {
		var runtimeBranches []runscBranchInfo
		if err := json.Unmarshal(output, &runtimeBranches); err != nil {
			return fmt.Errorf("decoding active branches before starting %s: %w", target, err)
		}
		for _, runtimeBranch := range runtimeBranches {
			if runtimeBranch.ID == target.branch {
				if _, err := b.command("timetravel", info.ID, "destroy", target.branch); err != nil {
					return fmt.Errorf("releasing the exited incarnation of %s: %w", target, err)
				}
				break
			}
		}
	} else {
		return fmt.Errorf("querying active peers before starting %s: %w", target, err)
	}
	capture.Resume = true
	if err := b.rehydrateColdPeer(target, info, branch, capture, stagingDir, metadata, snapshots, false); err != nil {
		return fmt.Errorf("installing fresh branch %s beside active peers: %w", target, err)
	}
	return nil
}

func (b *runtimeBackend) waitForPausedBootstrap(runtimeID, branchID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastState string
	for {
		output, err := b.command("timetravel", runtimeID, "branches")
		if err == nil {
			var branches []runscBranchInfo
			if err := json.Unmarshal(output, &branches); err != nil {
				return fmt.Errorf("decoding bootstrap branches: %w", err)
			}
			for _, branch := range branches {
				if branch.ID != branchID {
					continue
				}
				lastState = fmt.Sprintf("state=%s init=%s pid=%d", branch.State, branch.InitState, branch.InitPID)
				if branch.State == "paused" && branch.InitPID > 0 {
					return nil
				}
			}
		} else {
			lastState = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out (%s)", lastState)
		}
		time.Sleep(time.Millisecond)
	}
}

// captureTransientBranch exports one already-paused bootstrap branch into
// short-lived files suitable for peer import. The caller owns the returned
// open files and directory.
func (b *runtimeBackend) captureTransientBranch(runtimeID, branchID, stagingDir string) (boot.BranchDehydrateInfo, *os.File, []*os.File, error) {
	metadataPath := filepath.Join(stagingDir, "metadata.state")
	metadata, err := os.OpenFile(metadataPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, nil, err
	}
	output, err := b.commandWithFiles([]*os.File{metadata}, "timetravel", "--dehydration-fd=3", runtimeID, "dehydrate", branchID)
	if closeErr := metadata.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, nil, err
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(output, &capture); err != nil {
		return boot.BranchDehydrateInfo{}, nil, nil, fmt.Errorf("decoding bootstrap capture: %w", err)
	}
	if len(capture.Memories) == 0 {
		return boot.BranchDehydrateInfo{}, nil, nil, errors.New("bootstrap capture contains no memory images")
	}
	exportFiles := make([]*os.File, 0, len(capture.Memories))
	for i := range capture.Memories {
		file, err := os.OpenFile(filepath.Join(stagingDir, fmt.Sprintf("memory-%04d.snapshot", i)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			for _, opened := range exportFiles {
				_ = opened.Close()
			}
			return boot.BranchDehydrateInfo{}, nil, nil, err
		}
		exportFiles = append(exportFiles, file)
	}
	exportArgs := []string{"timetravel", "--dehydration-snapshots-start-fd=3", fmt.Sprintf("--dehydration-snapshots-count=%d", len(exportFiles))}
	if b.volumeCopyMode == ttcvolume.CopyModeFull {
		exportArgs = append(exportArgs, "--dehydration-snapshot-allow-copy")
	}
	exportArgs = append(exportArgs, runtimeID, "dehydrate-export", branchID)
	_, exportErr := b.commandWithFiles(exportFiles, exportArgs...)
	for _, file := range exportFiles {
		if closeErr := file.Close(); exportErr == nil && closeErr != nil {
			exportErr = closeErr
		}
	}
	if exportErr != nil {
		return boot.BranchDehydrateInfo{}, nil, nil, exportErr
	}
	metadata, err = os.Open(metadataPath)
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, nil, err
	}
	snapshots := make([]*os.File, 0, len(capture.Memories))
	for i := range capture.Memories {
		file, err := os.Open(filepath.Join(stagingDir, fmt.Sprintf("memory-%04d.snapshot", i)))
		if err != nil {
			metadata.Close()
			for _, opened := range snapshots {
				_ = opened.Close()
			}
			return boot.BranchDehydrateInfo{}, nil, nil, err
		}
		snapshots = append(snapshots, file)
	}
	return capture, metadata, snapshots, nil
}

func (b *runtimeBackend) finishPreservedPeerStart(info ttcbundle.ContainerInfo, target selector, detach, interactive bool) (int, error) {
	b.signalLifecycleReady()
	if detach {
		if info.AutoRemove {
			return b.applyAutoRemove(target, true, 0, nil)
		}
		return 0, nil
	}
	waitStatus, err := b.attach(attachRequest{target: target, stdin: interactive, stdout: true, stderr: true, detachKeys: defaultDetachKeys})
	if errors.Is(err, errDetached) {
		if info.AutoRemove {
			return b.applyAutoRemove(target, true, 0, nil)
		}
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	status := waitStatusExitCode(waitStatus)
	b.recordLaunchState(target, false, status)
	if info.AutoRemove {
		return b.applyAutoRemove(target, false, status, nil)
	}
	return status, nil
}

func (b *runtimeBackend) applyAutoRemove(target selector, detach bool, status int, launchErr error) (int, error) {
	if detach && launchErr == nil && status == 0 {
		if err := b.scheduleAutoRemove(target); err == nil {
			return status, nil
		} else {
			cleanupErr := autoRemoveTarget(target, b)
			if cleanupErr != nil {
				return 1, fmt.Errorf("scheduling auto-remove: %w (cleanup also failed: %v)", err, cleanupErr)
			}
			return 1, fmt.Errorf("scheduling auto-remove: %w; container was removed", err)
		}
	}
	cleanupErr := autoRemoveTarget(target, b)
	if cleanupErr != nil {
		if launchErr != nil {
			return status, fmt.Errorf("%w (auto-remove also failed: %v)", launchErr, cleanupErr)
		}
		return 1, fmt.Errorf("auto-removing container after exit: %w", cleanupErr)
	}
	return status, launchErr
}

func (b *runtimeBackend) scheduleAutoRemove(target selector) error {
	executable := b.runsc
	if executable == "" {
		executable = currentExecutable()
	}
	if executable == "" || b.daemonSocket == "" {
		return errors.New("locating ttcd auto-remove helper")
	}
	args := []string{"_auto-remove-client", "--socket", b.daemonSocket, target.String()}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening auto-remove stdio: %w", err)
	}
	defer devNull.Close()
	command := exec.Command(executable, args...)
	command.Stdin = devNull
	command.Stdout = devNull
	// The helper is the process that ultimately unlinks the container tree. Its
	// own stderr must therefore not be a file inside that tree: on NFS this
	// deterministically silly-renames the open log to .nfs* and makes every
	// successful --rm require deferred cleanup. Helper failures leave the
	// container record intact and are visible through normal lifecycle state.
	command.Stderr = devNull
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("starting auto-remove helper: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("releasing auto-remove helper: %w", err)
	}
	return nil
}

func (b *runtimeBackend) removeStoppedRuntime(name string, waitForTeardown bool) error {
	const (
		teardownTimeout = 5 * time.Second
		teardownPoll    = 10 * time.Millisecond
	)
	deadline := time.Now().Add(teardownTimeout)
	for {
		families, err := b.listRuntimeFamilies()
		if err != nil {
			return fmt.Errorf("checking runtime state before starting %q: %w", name, err)
		}
		found := false
		for _, family := range families {
			if family.ID != name {
				continue
			}
			found = true
			if family.State == "stopped" || family.State == "exited" || runtimeFamilyUnavailable(family) {
				if _, err := b.command("delete", "--force", name); err != nil {
					return fmt.Errorf("deleting unavailable runtime container %q: %w", name, err)
				}
				return nil
			}
			if !waitForTeardown || time.Now().After(deadline) {
				return fmt.Errorf("container %q is already %s", name, family.State)
			}
			break
		}
		if !found {
			return nil
		}
		time.Sleep(teardownPoll)
	}
}

func (b *runtimeBackend) exec(request execRequest) (retStatus int, retErr error) {
	resolved, _, err := b.resolveSelector(request.target)
	if err != nil {
		return 1, err
	}
	request.target = resolved
	if request.terminal && request.processPath == "" && !environmentHasKey(request.env, "TERM") {
		request.env = append(request.env, "TERM="+terminalType(b.clientTERM))
	}
	cols, rows, hasConsoleSize := 0, 0, false
	if request.terminal {
		cols, rows, hasConsoleSize, err = hostTerminalSize(b.stdin)
		if err != nil {
			return 1, err
		}
	}
	args := []string{"--root", b.root, "exec", "--ttc-allow-stopped-sandbox", "--branch", request.target.branch}
	consoleID := ""
	execID := request.execID
	if execID == "" {
		var err error
		execID, err = newExecID()
		if err != nil {
			return 1, fmt.Errorf("creating stable exec session ID: %w", err)
		}
	}
	args = append(args, "--exec-id", execID)
	consoleID = "ttc-" + execID
	args = append(args, "--internal-console", consoleID)
	if request.detach {
		args = append(args, "--detach")
	} else if request.interactive {
		args = append(args, "--internal-console-stdin")
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
		args = append(args, "--internal-console-terminal")
		if hasConsoleSize {
			args = append(args, "--internal-console-rows", strconv.Itoa(rows), "--internal-console-cols", strconv.Itoa(cols))
		}
	}
	args = append(args, request.target.container)
	if request.processPath == "" {
		args = append(args, request.args...)
	}
	command := runscCommand(b.runsc, args...)
	var closeCommandStdin func()
	if !request.detach && (request.interactive || request.terminal) {
		closeCommandStdin, err = setCommandStdin(command, b.stdin)
		if err != nil {
			return 1, err
		}
		defer closeCommandStdin()
	}
	command.Stdout = b.stdout
	command.Stderr = b.stderr
	if request.terminal && request.interactive && !request.detach {
		restore, err := makeRawTerminal(b.stdin)
		if err != nil {
			return 1, err
		}
		if restore != nil {
			defer func() {
				if err := restore(); retErr == nil && err != nil {
					retErr = fmt.Errorf("restoring exec stdin terminal: %w", err)
				}
			}()
		}
	}
	if err := command.Start(); err != nil {
		return 1, fmt.Errorf("starting runsc exec: %w", err)
	}
	if request.terminal && !request.detach {
		stopResize := b.watchConsoleResize(request.target, consoleID)
		defer stopResize()
	}
	if err := command.Wait(); err != nil {
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

// setCommandStdin passes real files directly and bridges other readers through
// an OS pipe. os/exec otherwise creates an internal copy goroutine and Wait
// waits for it; a daemon-side interactive reader can remain blocked after the
// child exits, causing a completed exec to hang forever.
func setCommandStdin(command *exec.Cmd, input io.Reader) (func(), error) {
	if input == nil {
		return func() {}, nil
	}
	if file, ok := input.(*os.File); ok {
		command.Stdin = file
		return func() {}, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating command stdin pipe: %w", err)
	}
	command.Stdin = reader
	go func() {
		_, _ = io.Copy(writer, input)
		_ = writer.Close()
	}()
	return func() {
		_ = reader.Close()
		_ = writer.Close()
	}, nil
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
		generationPath := ttcconsole.GenerationPath(path, generation)
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
	return f.drainFrames(func(frame ttcconsole.Frame) error {
		var output io.Writer
		switch frame.Stream {
		case ttcconsole.StreamStdout:
			output = stdout
		case ttcconsole.StreamStderr:
			output = stderr
		}
		return writeConsoleFrameData(frame, output)
	})
}

func (f *consoleLogFollower) drainFrames(handler func(ttcconsole.Frame) error) (bool, error) {
	progress := false
	buf := make([]byte, 64*1024)
	for {
		n, err := f.file.Read(buf)
		if n != 0 {
			f.pending = append(f.pending, buf[:n]...)
			progress = true
			if err := f.consumeFrames(handler); err != nil {
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
	return progress, f.consumeFrames(handler)
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

func (f *consoleLogFollower) consumeFrames(handler func(ttcconsole.Frame) error) error {
	for {
		frame, consumed, complete, err := ttcconsole.DecodeFrame(f.pending)
		if err != nil {
			return fmt.Errorf("decoding console log: %w", err)
		}
		if !complete {
			break
		}
		f.pending = f.pending[consumed:]
		switch frame.Stream {
		case ttcconsole.StreamStdout:
			f.stdoutEOF = frame.EOF
		case ttcconsole.StreamStderr:
			f.stderrEOF = frame.EOF
		}
		if len(frame.Data) != 0 && handler != nil {
			if err := handler(frame); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeConsoleFrameData(frame ttcconsole.Frame, output io.Writer) error {
	if output == nil || len(frame.Data) == 0 {
		return nil
	}
	n, err := output.Write(frame.Data)
	if err != nil {
		return fmt.Errorf("writing attached stream %d: %w", frame.Stream, err)
	}
	if n != len(frame.Data) {
		return fmt.Errorf("writing attached stream %d: %w", frame.Stream, io.ErrShortWrite)
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
	releaseClient, err := b.registerConsoleClient(target)
	if err != nil {
		return 0, err
	}
	defer releaseClient()
	execID := request.execID
	var paths ttcbundle.ConsolePaths
	consoleID := "init"
	terminal := false
	if execID == "" {
		paths, err = b.ensureConsoleDrain(target, false)
		if err != nil {
			return 0, err
		}
		store := b.containerStore()
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
		if b.consoleClientCanceled() {
			return 0, errDetached
		}
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
	case ttcconsole.StreamStdout:
		partial = &t.stdoutPartial
		partialSeq = &t.stdoutSeq
	case ttcconsole.StreamStderr:
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
		t.push(consoleLogLine{stream: ttcconsole.StreamStdout, data: t.stdoutPartial})
		t.stdoutPartial = nil
	}
	if len(t.stderrPartial) != 0 {
		t.push(consoleLogLine{stream: ttcconsole.StreamStderr, data: t.stderrPartial})
		t.stderrPartial = nil
	}
	if len(t.stdoutPartial) != 0 {
		t.push(consoleLogLine{stream: ttcconsole.StreamStdout, data: t.stdoutPartial})
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
		if line.stream == ttcconsole.StreamStderr {
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

type consoleLogOutput struct {
	since      *time.Time
	until      *time.Time
	timestamps bool
	details    string
	lineStart  map[byte]bool
}

func newConsoleLogOutput(options logsOptions, attributes map[string]string) *consoleLogOutput {
	attributeNames := make([]string, 0, len(attributes))
	for name := range attributes {
		attributeNames = append(attributeNames, name)
	}
	sort.Strings(attributeNames)
	details := make([]string, 0, len(attributeNames))
	if options.Details {
		for _, name := range attributeNames {
			details = append(details, name+"="+attributes[name])
		}
	}
	return &consoleLogOutput{
		since: options.Since, until: options.Until, timestamps: options.Timestamps, details: strings.Join(details, ","),
		lineStart: map[byte]bool{ttcconsole.StreamStdout: true, ttcconsole.StreamStderr: true},
	}
}

func (o *consoleLogOutput) includes(frame ttcconsole.Frame) bool {
	if frame.Timestamp.IsZero() {
		// Legacy records remain readable, but cannot honestly satisfy a time filter.
		return o.since == nil && o.until == nil
	}
	if o.since != nil && frame.Timestamp.Before(*o.since) {
		return false
	}
	if o.until != nil && frame.Timestamp.After(*o.until) {
		return false
	}
	return true
}

func (o *consoleLogOutput) write(frame ttcconsole.Frame, stdout, stderr io.Writer) error {
	if !o.includes(frame) {
		return nil
	}
	output := stdout
	if frame.Stream == ttcconsole.StreamStderr {
		output = stderr
	}
	if output == nil || len(frame.Data) == 0 {
		return nil
	}
	if !o.timestamps && o.details == "" {
		return writeConsoleFrameData(frame, output)
	}
	data := frame.Data
	for len(data) != 0 {
		if o.lineStart[frame.Stream] {
			var prefix strings.Builder
			if o.timestamps {
				prefix.WriteString(frame.Timestamp.UTC().Format(time.RFC3339Nano))
				prefix.WriteByte(' ')
			}
			if o.details != "" {
				prefix.WriteString(o.details)
				prefix.WriteByte(' ')
			}
			if err := writeLogData(output, frame.Stream, []byte(prefix.String())); err != nil {
				return err
			}
			o.lineStart[frame.Stream] = false
		}
		end := len(data)
		if newline := bytes.IndexByte(data, byte('\n')); newline >= 0 {
			end = newline + 1
			o.lineStart[frame.Stream] = true
		}
		if err := writeLogData(output, frame.Stream, data[:end]); err != nil {
			return err
		}
		data = data[end:]
	}
	return nil
}

func writeLogData(output io.Writer, stream byte, data []byte) error {
	n, err := output.Write(data)
	if err != nil {
		return fmt.Errorf("writing log stream %d: %w", stream, err)
	}
	if n != len(data) {
		return fmt.Errorf("writing log stream %d: %w", stream, io.ErrShortWrite)
	}
	return nil
}

func (b *runtimeBackend) logs(target selector, options logsOptions) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	releaseClient, err := b.registerConsoleClient(target)
	if err != nil {
		return err
	}
	defer releaseClient()
	container, err := b.containerStore().LoadContainer(target.container)
	if err != nil {
		return err
	}
	if container.LogDriver == "none" {
		return fmt.Errorf("configured logging driver %q does not support reading", container.LogDriver)
	}
	var paths ttcbundle.ConsolePaths
	if options.ExecID == "" {
		paths, err = b.ensureConsoleDrain(target, false)
	} else {
		session, sessionErr := b.execSession(target, options.ExecID)
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
	output := newConsoleLogOutput(options, container.LogAttributes)
	initial := true
	for {
		if b.consoleClientCanceled() {
			return nil
		}
		var progress bool
		if initial && options.Tail >= 0 {
			collector := &consoleLogTail{limit: options.Tail}
			progress, err = follower.drainFrames(func(frame ttcconsole.Frame) error {
				return output.write(frame, collector.writer(ttcconsole.StreamStdout), collector.writer(ttcconsole.StreamStderr))
			})
			if err == nil {
				err = collector.flush(b.stdout, b.stderr)
			}
		} else {
			progress, err = follower.drainFrames(func(frame ttcconsole.Frame) error {
				return output.write(frame, b.stdout, b.stderr)
			})
		}
		if err != nil {
			return err
		}
		initial = false
		if !options.Follow || (follower.stdoutEOF && follower.stderrEOF) {
			return nil
		}
		if options.Until != nil && !time.Now().Before(*options.Until) {
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
	store := b.containerStore()
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

func (b *runtimeBackend) ensureConsoleDrain(target selector, reset bool) (ttcbundle.ConsolePaths, error) {
	store := b.containerStore()
	paths, err := store.BranchConsolePaths(target.container, target.branch)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	return b.ensureConsoleDrainPaths(target, "", "init", paths, reset)
}

func (b *runtimeBackend) ensureExecConsoleDrain(target selector, execID, consoleID string, reset bool) (ttcbundle.ConsolePaths, error) {
	store := b.containerStore()
	paths, err := store.ExecConsolePaths(target.container, target.branch, execID)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	return b.ensureConsoleDrainPaths(target, execID, consoleID, paths, reset)
}

func (b *runtimeBackend) ensureConsoleDrainPaths(target selector, execID, consoleID string, paths ttcbundle.ConsolePaths, reset bool) (ttcbundle.ConsolePaths, error) {
	lockFile, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("opening console drain lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if reset {
				return ttcbundle.ConsolePaths{}, fmt.Errorf("console drain for %s is still active", target)
			}
			return paths, nil
		}
		return ttcbundle.ConsolePaths{}, fmt.Errorf("locking console drain: %w", err)
	}
	if err := os.Remove(paths.PID); err != nil && !os.IsNotExist(err) {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("removing stale console follower PID: %w", err)
	}
	if !reset {
		if _, err := os.Stat(paths.WaitStatus); err == nil {
			return paths, nil
		} else if !os.IsNotExist(err) {
			return ttcbundle.ConsolePaths{}, fmt.Errorf("checking console wait status: %w", err)
		}
	}
	maxBytes, maxFiles, err := b.consoleRetention(target.container)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	if reset {
		if err := os.Remove(paths.RetentionStats); err != nil && !os.IsNotExist(err) {
			return ttcbundle.ConsolePaths{}, fmt.Errorf("removing stale console retention stats: %w", err)
		}
		if err := os.Remove(paths.WaitStatus); err != nil && !os.IsNotExist(err) {
			return ttcbundle.ConsolePaths{}, fmt.Errorf("removing stale console wait status: %w", err)
		}
		for generation := 1; ; generation++ {
			err := os.Remove(ttcconsole.GenerationPath(paths.Log, generation))
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return ttcbundle.ConsolePaths{}, fmt.Errorf("removing console log generation %d: %w", generation, err)
			}
		}
	}
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if reset {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	logFile, err := os.OpenFile(paths.Log, logFlags, 0600)
	if err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("opening console log: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("closing prepared console log: %w", err)
	}
	errorFlags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if reset {
		errorFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	errorFile, err := os.OpenFile(paths.Error, errorFlags, 0600)
	if err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("opening console error log: %w", err)
	}
	defer errorFile.Close()

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("creating console readiness pipe: %w", err)
	}
	defer readyReader.Close()
	commandArgs := []string{
		"--root", b.root,
		"timetravel", "--console", consoleID,
		"--console-log", paths.Log,
		"--console-log-max-bytes", strconv.FormatInt(maxBytes, 10),
		"--console-log-max-files", strconv.Itoa(maxFiles),
		"--console-wait-status", paths.WaitStatus,
		"--console-retention-stats", paths.RetentionStats,
		"--console-pid-file", paths.PID,
		"--console-ready-fd", "4",
	}
	if execID != "" {
		commandArgs = append(commandArgs, "--exec-id", execID)
	}
	commandArgs = append(commandArgs, target.container, "console-follow", target.branch)
	command := runscCommand(b.runsc, commandArgs...)
	command.Stderr = errorFile
	// The inherited duplicate keeps the advisory lock held for exactly the
	// lifetime of the drainer, preventing multiple consumers of one pipe.
	command.ExtraFiles = []*os.File{lockFile, readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return ttcbundle.ConsolePaths{}, fmt.Errorf("starting console drain for %s: %w", target, err)
	}
	if err := readyWriter.Close(); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttcbundle.ConsolePaths{}, fmt.Errorf("closing console readiness writer for %s: %w", target, err)
	}
	if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttcbundle.ConsolePaths{}, fmt.Errorf("setting console readiness deadline for %s: %w", target, err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyReader, ready[:]); err != nil {
		command.Process.Kill()
		command.Wait()
		return ttcbundle.ConsolePaths{}, fmt.Errorf("waiting for console drain readiness for %s: %w: %v", target, err, consoleDrainStoppedError(paths.Error))
	}
	if b.consoleSidecars != nil {
		if err := b.consoleSidecars.OwnConsoleSidecar(paths.Lock, command); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return ttcbundle.ConsolePaths{}, fmt.Errorf("transferring console drain ownership for %s: %w", target, err)
		}
	} else if err := command.Process.Release(); err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("releasing console drain process for %s: %w", target, err)
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
	var resizeEvents <-chan struct{}
	var resizeSignals chan os.Signal
	if remote, ok := b.stdin.(remoteTerminalInput); ok {
		resizeEvents = remote.terminalResizeEvents()
	} else {
		file, ok := b.stdin.(*os.File)
		if !ok || !term.IsTerminal(int(file.Fd())) {
			return func() {}
		}
		resizeSignals = make(chan os.Signal, 1)
		signal.Notify(resizeSignals, syscall.SIGWINCH)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-resizeEvents:
				if err := b.resizeConsoleFromHost(target, consoleID); err != nil && b.stderr != nil {
					fmt.Fprintf(b.stderr, "ttc: warning: resizing console for %s: %v\n", target, err)
				}
			case <-resizeSignals:
				if err := b.resizeConsoleFromHost(target, consoleID); err != nil && b.stderr != nil {
					fmt.Fprintf(b.stderr, "ttc: warning: resizing console for %s: %v\n", target, err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		if resizeSignals != nil {
			signal.Stop(resizeSignals)
		}
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

func (b *runtimeBackend) waitConsoleWritable(target selector, consoleID string) error {
	_, err := b.command("timetravel", "--console", consoleID, "--wait-stdin", "--wait-timeout=1s", target.container, "console-wait", target.branch)
	return err
}

func (b *runtimeBackend) writeConsoleAll(target selector, consoleID string, data []byte) error {
	for written := 0; written < len(data); {
		result, err := b.writeConsole(target, consoleID, data[written:])
		if err != nil {
			return err
		}
		written += result.N
		if result.WouldBlock || result.N == 0 {
			if err := b.waitConsoleWritable(target, consoleID); err != nil {
				return err
			}
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

func (b *runtimeBackend) recordLaunchState(target selector, detach bool, status int) {
	store := ttcbundle.Store{Root: b.dataRoot}
	if detach && status == 0 {
		if err := store.RecordBranchStart(target.container, target.branch); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: recording start state for %s: %v\n", target, err)
		}
	} else {
		if err := store.StopBranch(target.container, target.branch, status); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: recording exit state for %s: %v\n", target, err)
		}
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

func immutableHostMountDestinations(spec *specs.Spec) map[string]struct{} {
	result := make(map[string]struct{})
	if spec == nil || spec.Annotations == nil {
		return result
	}
	var destinations []string
	if json.Unmarshal([]byte(ttcbundle.AnnotationValue(spec.Annotations, ttcbundle.ImmutableHostMountsAnnotation)), &destinations) != nil {
		return result
	}
	for _, destination := range destinations {
		result[filepath.Clean(destination)] = struct{}{}
	}
	return result
}

func managedCoWHostMountDestinations(spec *specs.Spec) map[string]struct{} {
	result := make(map[string]struct{})
	if spec == nil || spec.Annotations == nil {
		return result
	}
	var destinations []string
	if json.Unmarshal([]byte(ttcbundle.AnnotationValue(spec.Annotations, ttcbundle.ManagedCoWHostMountsAnnotation)), &destinations) != nil {
		return result
	}
	for _, destination := range destinations {
		result[filepath.Clean(destination)] = struct{}{}
	}
	return result
}

func copiedHostMountSources(spec *specs.Spec) map[string]string {
	result := make(map[string]string)
	if spec == nil || spec.Annotations == nil {
		return result
	}
	if json.Unmarshal([]byte(ttcbundle.AnnotationValue(spec.Annotations, ttcbundle.CopiedHostMountsAnnotation)), &result) != nil {
		return make(map[string]string)
	}
	return result
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
	managedCoWHostMounts := managedCoWHostMountDestinations(spec)
	copiedHostMounts := copiedHostMountSources(spec)
	if encoded := ttcbundle.AnnotationValue(spec.Annotations, ttcbundle.TmpfsConfigAnnotation); encoded != "" {
		_ = json.Unmarshal([]byte(encoded), &dockerTmpfs)
	}
	internalMounts := make(map[string]struct{})
	if encoded := ttcbundle.AnnotationValue(spec.Annotations, ttcbundle.InternalMountsAnnotation); encoded != "" {
		var destinations []string
		if json.Unmarshal([]byte(encoded), &destinations) == nil {
			for _, destination := range destinations {
				internalMounts[filepath.Clean(destination)] = struct{}{}
			}
		}
	}
	for _, mount := range spec.Mounts {
		if _, internal := internalMounts[filepath.Clean(mount.Destination)]; internal {
			continue
		}
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
			if _, managedCoW := managedCoWHostMounts[filepath.Clean(mount.Destination)]; managedCoW {
				mode += ",cow"
			}
			binds = append(binds, mount.Source+":"+mount.Destination+":"+mode)
			mounts = append(mounts, InspectMount{
				Type: "bind", Source: mount.Source, Destination: mount.Destination,
				Mode: mode, RW: !readonly, Propagation: "rprivate",
			})
		case mount.Type == "ttcvol":
			if original, copied := copiedHostMounts[filepath.Clean(mount.Destination)]; copied {
				mode = "rw,copy"
				binds = append(binds, original+":"+mount.Destination+":"+mode)
				mounts = append(mounts, InspectMount{
					Type: "bind", Source: original, Destination: mount.Destination,
					Mode: mode, RW: true, Propagation: "rprivate",
				})
				continue
			}
			name := filepath.Base(mount.Source)
			volumes[mount.Destination] = struct{}{}
			mounts = append(mounts, InspectMount{
				Type: "volume", Name: name, Source: name,
				Destination: mount.Destination, Driver: "ttc-volume",
				Mode: mode, RW: !readonly, Propagation: "",
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

func buildInspectInfo(container ttcbundle.ContainerInfo, spec *specs.Spec, family *Family, runtimeErr string) InspectInfo {
	container.State = normalizeLifecycleState(container.State)
	for i := range container.Branches {
		container.Branches[i].State = normalizeLifecycleState(container.Branches[i].State)
	}
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
	state = normalizeLifecycleState(state)
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
	volumeByDestination := make(map[string]ttcbundle.ManagedVolume, len(container.Volumes))
	for _, volume := range container.Volumes {
		volumeByDestination[filepath.Clean(volume.Destination)] = volume
		volumes[filepath.Clean(volume.Destination)] = struct{}{}
	}
	for i := range mounts {
		if mounts[i].Type != "volume" {
			continue
		}
		if volume, ok := volumeByDestination[filepath.Clean(mounts[i].Destination)]; ok {
			mounts[i].Name = volume.Name
			mounts[i].Source = volume.Name
		}
	}
	externalViews := make([]InspectExternalView, 0)
	immutableHostMounts := immutableHostMountDestinations(spec)
	managedCoWHostMounts := managedCoWHostMountDestinations(spec)
	copiedHostMounts := copiedHostMountSources(spec)
	for _, mount := range mounts {
		if mount.Type == "bind" {
			if _, copied := copiedHostMounts[filepath.Clean(mount.Destination)]; copied {
				continue
			}
			_, immutableSource := immutableHostMounts[filepath.Clean(mount.Destination)]
			_, managedCoW := managedCoWHostMounts[filepath.Clean(mount.Destination)]
			semantics := "shared-external-view"
			if immutableSource {
				semantics = "user-asserted-immutable-external-view"
			}
			if managedCoW {
				semantics = "branch-local-persistent-cow-over-live-host-lower"
				if immutableSource {
					semantics = "branch-local-persistent-cow-over-user-asserted-immutable-host-lower"
				}
			}
			externalViews = append(externalViews, InspectExternalView{Source: mount.Source, Destination: mount.Destination, Readonly: !mount.RW, ImmutableSourceAsserted: immutableSource, ManagedCoW: managedCoW, Semantics: semantics})
		}
	}
	hostname := container.Name
	configUser := inspectUser(process.User)
	configCmd := append([]string(nil), process.Args...)
	var entrypoint []string
	labels := make(map[string]string)
	if spec != nil {
		if spec.Hostname != "" {
			hostname = spec.Hostname
		}
		if value := ttcbundle.AnnotationValue(spec.Annotations, "dev.timetravelcontainer.config.user"); value != "" {
			configUser = value
		}
		if value := ttcbundle.AnnotationValue(spec.Annotations, "dev.timetravelcontainer.config.cmd"); value != "" {
			_ = json.Unmarshal([]byte(value), &configCmd)
		}
		if value := ttcbundle.AnnotationValue(spec.Annotations, "dev.timetravelcontainer.config.entrypoint"); value != "" {
			_ = json.Unmarshal([]byte(value), &entrypoint)
		}
		if value := ttcbundle.AnnotationValue(spec.Annotations, "dev.timetravelcontainer.config.labels"); value != "" {
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
		stopSignal = ttcbundle.DefaultStopSignal
	}
	stopTimeout := container.StopTimeout
	if stopTimeout == nil {
		value := ttcbundle.DefaultStopTimeoutSeconds
		stopTimeout = &value
	}
	memory, nanoCPUs, cpuQuota := int64(0), int64(0), int64(0)
	cpuShares, cpuPeriod := uint64(0), uint64(0)
	if container.MemoryLimit != nil {
		memory = *container.MemoryLimit
	}
	if container.NanoCPUs != nil {
		nanoCPUs = *container.NanoCPUs
	}
	if container.CPUShares != nil {
		cpuShares = *container.CPUShares
	}
	if container.CPUPeriod != nil {
		cpuPeriod = *container.CPUPeriod
	}
	if container.CPUQuota != nil {
		cpuQuota = *container.CPUQuota
	}
	shmSize := ttcbundle.DefaultShmSizeBytes
	if container.ShmSize != nil {
		shmSize = *container.ShmSize
	}
	logConfig := InspectLogConfig{Type: container.LogDriver, Config: map[string]string{}}
	for key, value := range container.LogOptions {
		logConfig.Config[key] = value
	}
	if container.LogDriver != "none" {
		maxBytes := container.ConsoleLogMaxBytes
		if maxBytes <= 0 {
			maxBytes = defaultConsoleLogMax
		}
		maxFiles := container.ConsoleLogMaxFiles
		if maxFiles < 1 {
			maxFiles = defaultConsoleLogFiles
		}
		logConfig.Config["max-size"] = strconv.FormatInt(maxBytes, 10)
		logConfig.Config["max-file"] = strconv.Itoa(maxFiles)
	}
	portBindings := make(map[string][]InspectPortBinding)
	for _, binding := range container.PublishedPorts {
		if binding.Branch() != defaultBranchID {
			continue
		}
		key := fmt.Sprintf("%d/%s", binding.ContainerPort, binding.Protocol)
		portBindings[key] = append(portBindings[key], InspectPortBinding{
			HostIP: binding.HostIP, HostPort: strconv.Itoa(int(binding.HostPort)),
		})
	}
	driver := "ttc-disk-overlay"
	if container.RootfsUpperCopyMode == string(ttcvolume.CopyModeReflink) {
		driver = "ttc-reflink-overlay"
	}
	upper := ""
	if container.RootfsUpperID != "" {
		upper = "ttcvol:" + container.RootfsUpperID
	}
	return InspectInfo{
		ID: container.ID, Created: created, Path: path, Args: args,
		State: InspectState{
			Status: state, Running: state == "running" || state == "paused", Paused: state == "paused",
			Dead: state == "failed" || state == "destroying", Pid: sandboxPID, ExitCode: exitCode,
			Error: stateError, StartedAt: inspectTimestamp(container.StartedAt), FinishedAt: inspectTimestamp(container.ExitedAt),
		},
		Image: container.Digest, Name: "/" + container.Name, Driver: driver,
		Platform: container.Platform, RestartCount: 0,
		GraphDriver: InspectGraphDriver{
			Name: driver,
			Data: map[string]string{"LowerDir": container.RootFS, "UpperDir": upper},
		},
		Config: InspectConfig{
			Hostname: hostname, User: configUser, AttachStdin: container.OpenStdin, AttachStdout: true, AttachStderr: true,
			Tty: process.Terminal, OpenStdin: container.OpenStdin, Env: append([]string(nil), process.Env...), Cmd: configCmd,
			Image: container.Reference, Volumes: volumes, WorkingDir: process.Cwd, Entrypoint: entrypoint, Labels: labels, StopSignal: stopSignal,
		},
		HostConfig: InspectHostConfig{
			Binds: binds, AutoRemove: container.AutoRemove, NetworkMode: container.NetworkMode, PortBindings: portBindings, LogConfig: logConfig, ExtraHosts: container.ExtraHosts, DNS: container.DNS, DNSSearch: container.DNSSearch, DNSOptions: container.DNSOptions, ReadonlyRootfs: readonlyRootfs, StopTimeout: stopTimeout, Runtime: "runsc", PidsLimit: container.PidsLimit, Memory: memory, NanoCPUs: nanoCPUs, CPUShares: cpuShares, CPUPeriod: cpuPeriod, CPUQuota: cpuQuota, ShmSize: shmSize, GroupAdd: container.GroupAdd, CapAdd: container.CapAdd, CapDrop: container.CapDrop, Ulimits: container.Ulimits, Tmpfs: tmpfs,
		},
		Mounts: mounts,
		TTC: TTCInspectInfo{
			Container: container, Runtime: family, Branches: branches, ExternalViews: externalViews, RuntimeError: runtimeErr,
		},
	}
}

func (b *runtimeBackend) inspect(name string) (InspectInfo, error) {
	store := ttcbundle.Store{Root: b.dataRoot}
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
	// listFamilies may reconcile persistent state after an asynchronous sandbox
	// exit. Reload so this inspect response reflects that update immediately.
	if refreshed, err := store.LoadContainer(containerInfo.ID); err == nil {
		containerInfo = refreshed
	}
	var runtimeFamily *Family
	for i := range families {
		if families[i].ID == containerInfo.ID {
			runtimeFamily = &families[i]
			break
		}
	}
	result := buildInspectInfo(containerInfo, spec, runtimeFamily, "")
	if transactionID := containerInfo.DistributedForkTransactionID; transactionID != "" {
		if store, err := b.transactionCatalog(); err == nil {
			if transaction, err := store.Load(transactionID); err == nil {
				result.TTC.DistributedFork = transaction
			}
		}
	}
	for i := range result.TTC.Branches {
		branch := &result.TTC.Branches[i]
		paths, retentionErr := store.BranchConsolePaths(containerInfo.ID, branch.Metadata.ID)
		if retentionErr == nil {
			stats, statsErr := ttcconsole.ReadRetentionStats(paths.RetentionStats)
			if statsErr != nil {
				retentionErr = statsErr
			} else if stats.EvictedGenerations != 0 || stats.EvictedBytes != 0 {
				branch.OutputRetention = &stats
			}
		}
		if retentionErr != nil {
			branch.OutputRetentionError = retentionErr.Error()
		}
		execStats, execRetentionErr := store.ListExecConsoleRetentionStats(containerInfo.ID, branch.Metadata.ID)
		if execRetentionErr != nil {
			branch.ExecOutputRetentionError = execRetentionErr.Error()
		} else {
			branch.ExecOutputRetention = execStats
		}
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

func (b *runtimeBackend) remove(name string, force, volumes bool) error {
	store := b.containerStore()
	info, err := store.LoadContainer(name)
	if err != nil {
		return err
	}
	if err := b.ensureDistributedSourceIndependent(info.ID, ""); err != nil {
		return fmt.Errorf("settling remote branches before removing %q: %w", name, err)
	}
	if !force {
		for _, branch := range info.Branches {
			switch normalizeLifecycleState(branch.State) {
			case "running", "paused", "creating", "dehydrating", "rehydrating", "restarting":
				return fmt.Errorf("branch %s@%s is %s; stop it or use --force", info.Name, branch.ID, normalizeLifecycleState(branch.State))
			}
		}
	}
	releaseClients, err := b.quiesceConsoleClients(selector{container: info.ID})
	if err != nil {
		return fmt.Errorf("detaching console clients for %q: %w", name, err)
	}
	defer releaseClients()
	if err := b.stopContainerConsoleDrains(info.ID); err != nil {
		return fmt.Errorf("stopping console followers for %q: %w", name, err)
	}
	if err := b.stopPublishedPorts(info); err != nil {
		return fmt.Errorf("stopping published ports for %q: %w", name, err)
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
		args := []string{"delete"}
		if force || persistentFamilyProcessFree(info) {
			args = append(args, "--force")
		}
		args = append(args, runtimeID)
		if _, err := b.command(args...); err != nil {
			return fmt.Errorf("deleting runtime container %q: %w", name, err)
		}
		break
	}
	if err := b.releaseDistributedSparseRoot(info); err != nil {
		return fmt.Errorf("releasing distributed sparse memory: %w", err)
	}
	if b.publicationQueue != nil {
		for _, branch := range info.Branches {
			if err := b.publicationQueue.CancelBranch(info.ID, branch.ID); err != nil {
				return fmt.Errorf("canceling publications for %s@%s: %w", info.Name, branch.ID, err)
			}
		}
	}
	if err := store.RemoveContainer(runtimeID); err != nil {
		return err
	}
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		for _, branch := range info.Branches {
			authority, err := b.distributedSource.authority.Branch(info.ID, branch.ID)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("loading removed branch authority for %s@%s: %w", info.Name, branch.ID, err)
			}
			if authority.Corrupted {
				if err := b.distributedSource.authority.RemoveCorruptedBranch(info.ID, branch.ID); err != nil {
					return fmt.Errorf("removing corrupted branch authority for %s@%s: %w", info.Name, branch.ID, err)
				}
			}
		}
	}
	if volumes {
		manager, err := b.volumeManager()
		if err != nil {
			return err
		}
		for _, volume := range info.Volumes {
			if volume.Anonymous {
				if err := manager.Remove(volume.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing anonymous volume %q: %w", volume.Name, err)
				}
			}
		}
	}
	if _, err := store.PruneImageRoots(context.Background()); err != nil {
		return fmt.Errorf("pruning unreferenced image roots: %w", err)
	}
	return nil
}

// purgeRuntime removes only this node's physical Sentry/gofer shell. Shared
// family metadata and branch filesystems are intentionally untouched; elastic
// migration and family-removal coordinators use this after ownership moved.
func (b *runtimeBackend) purgeRuntime(containerID string) error {
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("listing runtimes before purging %q: %w", containerID, err)
	}
	for _, family := range families {
		if family.ID != containerID {
			continue
		}
		if _, err := b.command("delete", "--force", containerID); err != nil {
			return fmt.Errorf("purging physical runtime %q: %w", containerID, err)
		}
		break
	}
	return nil
}

// removeOrphanRuntime force-removes a local physical runtime after its shared
// family metadata has disappeared. This is a last-resort daemon-shutdown path:
// without metadata there is no durable branch state left to preserve or
// publish, but the Sentry and its console followers must not survive forever.
func (b *runtimeBackend) removeOrphanRuntime(containerID string) error {
	consoleErr := b.stopContainerConsoleDrains(containerID)
	_, deleteErr := b.command("delete", "--force", containerID)
	if deleteErr != nil {
		deleteErr = fmt.Errorf("deleting physical runtime: %w", deleteErr)
	}
	var localErr error
	if deleteErr == nil {
		localErr = os.RemoveAll(filepath.Join(b.activeDataRoot(), "containers", containerID))
	}
	return errors.Join(consoleErr, deleteErr, localErr)
}

// The gofer holds host descriptors for the dentries the sentry is holding —
// close to two apiece — and it inherits this process's descriptor limit.
//
// Left alone the sentry caches a thousand dentries per mount, which is a budget
// with no relationship to that limit. A sandbox with seven mounts may cache
// seven thousand dentries and so ask its gofer for some twelve thousand
// descriptors; on a login node whose hard limit is 8192 the gofer reaches
// EMFILE long before the caches are full. There is no way back from that.
// Nothing evicts on a descriptor shortage, so from then until the sandbox is
// restarted every path in it fails — and fails as a missing file rather than as
// a shortage, because a lookup that cannot reach the gofer is indistinguishable
// from one that found nothing.
//
// So the cache is sized from the budget rather than from the number of mounts:
// half the descriptors for cached dentries, the other half left for the files
// the workload is actually holding open.
const (
	goferFDsPerDentry = 2
	// Room for the workload, in the same units: the cache may have one of
	// every dentryCacheShare descriptors.
	dentryCacheShare = 2
	// Below this a cache is not worth having, and above it the sandbox is
	// bounded by something other than descriptors. Both ends only bite on
	// hosts far away from the one this exists for.
	minDentryCache = 256
	maxDentryCache = 8000
)

// dentryCacheSize is how many dentries a sandbox may cache, in descriptors it
// can actually have.
//
// The second result is whether the budget could be read at all. When it cannot
// be, nothing is passed and the per-mount default stands: a number invented
// here would be worse than the one that is already there.
func dentryCacheSize() (int, bool) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, false
	}
	size := int(limit.Cur) / (goferFDsPerDentry * dentryCacheShare)
	if size < minDentryCache {
		size = minDentryCache
	}
	if size > maxDentryCache {
		size = maxDentryCache
	}
	return size, true
}

func (b *runtimeBackend) configuredRunscArgs() []string {
	return b.configuredRunscArgsForNetwork("none")
}

func (b *runtimeBackend) configuredRunscArgsForNetwork(networkMode string) []string {
	return b.configuredRunscArgsForNetworkNAT(networkMode, false)
}

func (b *runtimeBackend) configuredRunscArgsForNetworkNAT(networkMode string, networkNAT bool) []string {
	return b.configuredRunscArgsForNetworkConfig(networkMode, networkNAT, "", "")
}

func (b *runtimeBackend) configuredRunscArgsForNetworkConfig(networkMode string, networkNAT bool, networkIP, networkMAC string) []string {
	runtimePlatform := b.runtimePlatform
	if runtimePlatform == "" {
		runtimePlatform = defaultRuntimePlatform
	}
	args := []string{
		"--root", b.root,
		"--platform=" + runtimePlatform,
	}
	if b.unsafeNoHostIsolation {
		args = append(args, "--rootless=false", "--unsafe-no-host-isolation=true")
	} else {
		args = append(args, "--rootless=true")
	}
	args = append(args,
		"--network="+networkMode,
		"--directfs=false",
		// TTC treats container identities as guest-only state and prioritizes
		// compatibility with Docker images that use helpers such as sudo.
		"--allow-suid=true",
		"--overlay2=all:memory")
	if size, known := dentryCacheSize(); known {
		// One cache for the sandbox rather than one per mount, so that what it
		// costs the gofer does not grow with the number of things mounted.
		args = append(args, "--dcache="+strconv.Itoa(size))
	}
	if b.disableHostGSO {
		args = append(args, "--gso=false")
	}
	if networkMode == "ttc" {
		args = append(args, "--ttc-network-nat="+strconv.FormatBool(networkNAT))
		if networkIP != "" {
			args = append(args, "--ttc-network-ip="+networkIP)
		}
		if networkMAC != "" {
			args = append(args, "--ttc-network-mac="+networkMAC)
		}
	}
	return args
}

func (b *runtimeBackend) configuredRunscArgsForBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) []string {
	args := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	if branch.Quarantined {
		args = append(args, "--ttc-network-quarantine=true")
	}
	return args
}

func (b *runtimeBackend) runConfiguredCommand(args ...string) (int, error) {
	return b.runConfiguredCommandForNetwork("none", args...)
}

func (b *runtimeBackend) runConfiguredCommandForNetwork(networkMode string, args ...string) (int, error) {
	return b.runConfiguredCommandForNetworkNAT(networkMode, false, args...)
}

func (b *runtimeBackend) runConfiguredCommandForNetworkNAT(networkMode string, networkNAT bool, args ...string) (int, error) {
	return b.runConfiguredCommandWithArgs(b.configuredRunscArgsForNetworkNAT(networkMode, networkNAT), args...)
}

func (b *runtimeBackend) runConfiguredCommandWithArgs(configured []string, args ...string) (int, error) {
	args = append(configured, args...)
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return 1, fmt.Errorf("opening runsc launch stdout: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp("", "ttc-runsc-launch-*.log")
	if err != nil {
		return 1, fmt.Errorf("creating runsc launch stderr: %w", err)
	}
	stderrName := stderr.Name()
	defer os.Remove(stderrName)
	defer stderr.Close()

	command := runscCommand(b.runsc, args...)
	// Use real files rather than the caller streams. The sandbox may inherit
	// these descriptors after runsc exits; an os/exec copy pipe would keep
	// `ttc run --detach` command substitutions and redirected callers open.
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

func verifyCgroupV2Resources(pid int, procCgroup []byte, mountRoot, expectedPath string, resources *specs.LinuxResources) error {
	var actualPath string
	for _, line := range strings.Split(string(procCgroup), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			actualPath = parts[2]
			break
		}
	}
	if actualPath == "" {
		return errors.New("host does not expose a unified cgroup v2 membership")
	}
	actualClean := filepath.Clean("/" + strings.TrimPrefix(actualPath, "/"))
	expectedClean := filepath.Clean("/" + strings.TrimPrefix(expectedPath, "/"))
	if actualClean != expectedClean && !strings.HasSuffix(actualClean, expectedClean) {
		return fmt.Errorf("sandbox is in cgroup %q, want suffix %q", actualClean, expectedClean)
	}
	dir := filepath.Join(mountRoot, strings.TrimPrefix(actualClean, "/"))
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", name, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	procs, err := read("cgroup.procs")
	if err != nil {
		return err
	}
	wantPID := strconv.Itoa(pid)
	foundPID := false
	for _, candidate := range strings.Fields(procs) {
		if candidate == wantPID {
			foundPID = true
			break
		}
	}
	if !foundPID {
		return fmt.Errorf("sandbox PID %d is not a member of cgroup %q", pid, actualClean)
	}
	if resources.Memory != nil && resources.Memory.Limit != nil && *resources.Memory.Limit > 0 {
		got, err := read("memory.max")
		if err != nil {
			return err
		}
		want := strconv.FormatInt(*resources.Memory.Limit, 10)
		if got != want {
			return fmt.Errorf("memory.max=%q, want %q", got, want)
		}
	}
	if resources.CPU != nil {
		cpu := resources.CPU
		if cpu.Quota != nil || cpu.Period != nil {
			period := uint64(100000)
			if cpu.Period != nil && *cpu.Period != 0 {
				period = *cpu.Period
			}
			quota := "max"
			if cpu.Quota != nil && *cpu.Quota > 0 {
				quota = strconv.FormatInt(*cpu.Quota, 10)
			}
			want := quota + " " + strconv.FormatUint(period, 10)
			got, err := read("cpu.max")
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("cpu.max=%q, want %q", got, want)
			}
		}
		if cpu.Shares != nil && *cpu.Shares > 0 {
			want := 1 + ((*cpu.Shares-2)*9999)/262142
			got, err := read("cpu.weight")
			if err != nil {
				return err
			}
			if got != strconv.FormatUint(want, 10) {
				return fmt.Errorf("cpu.weight=%q, want %d", got, want)
			}
		}
	}
	return nil
}

func (b *runtimeBackend) verifyHostResourceLimits(name, bundleDir string) error {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return fmt.Errorf("reading OCI config for resource verification: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("decoding OCI config for resource verification: %w", err)
	}
	if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.CgroupsPath == "" {
		return nil
	}
	resources := spec.Linux.Resources
	hasLimit := resources.Memory != nil && resources.Memory.Limit != nil && *resources.Memory.Limit > 0 || resources.CPU != nil
	if !hasLimit {
		return nil
	}
	stateJSON, err := b.command("state", name)
	if err != nil {
		return fmt.Errorf("loading sandbox state for resource verification: %w", err)
	}
	var state specs.State
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return fmt.Errorf("decoding sandbox state for resource verification: %w", err)
	}
	if state.Pid <= 0 {
		return fmt.Errorf("sandbox state has invalid PID %d", state.Pid)
	}
	procCgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", state.Pid))
	if err != nil {
		return fmt.Errorf("reading sandbox cgroup membership: %w", err)
	}
	if err := verifyCgroupV2Resources(state.Pid, procCgroup, "/sys/fs/cgroup", spec.Linux.CgroupsPath, resources); err != nil {
		return fmt.Errorf("hard host resource enforcement unavailable: %w", err)
	}
	return nil
}

func (b *runtimeBackend) launch(info ttcbundle.ContainerInfo, target selector, bundleDir string, detach, interactive bool) (int, error) {
	name := info.ID
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return 1, err
	}
	configuredArgs := b.configuredRunscArgsForBranch(info, branch)
	status, err := b.runConfiguredCommandWithArgs(configuredArgs, "create", "--bundle", bundleDir, name)
	if err != nil || status != 0 {
		return status, err
	}
	if err := b.verifyHostResourceLimits(name, bundleDir); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, err
	}
	startDrain := func(phase string) error {
		if _, err := b.ensureConsoleDrain(target, true); err != nil {
			return fmt.Errorf("starting console drain %s container start: %w", phase, err)
		}
		return nil
	}
	if err := startDrain("before"); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, err
	}
	status, err = b.runConfiguredCommandWithArgs(configuredArgs, "start", name)
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
	if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, fmt.Errorf("publishing ports: %w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, fmt.Errorf("publishing ports: %w", err)
	}
	b.signalLifecycleReady()
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

func portSidecarPaths(dataRoot, container string, binding ttcbundle.PortBinding) (lockPath, errorPath string) {
	name := strings.NewReplacer(".", "_", ":", "_", "/", "_").Replace(binding.HostIP)
	prefix := ""
	if binding.Branch() != defaultBranchID {
		prefix = "branch-" + binding.Branch() + "-"
	}
	base := filepath.Join(dataRoot, "containers", container, "ports", fmt.Sprintf("%s%s-%d-%d-%s", prefix, name, binding.HostPort, binding.ContainerPort, binding.Protocol))
	return base + ".lock", base + ".log"
}

func (b *runtimeBackend) ensurePublishedPortsForBranch(info ttcbundle.ContainerInfo, branch string) error {
	record, err := persistentBranchInfo(info, branch)
	if err != nil {
		return err
	}
	if record.Quarantined {
		return nil
	}
	for _, binding := range info.PublishedPorts {
		if binding.Branch() != branch {
			continue
		}
		if err := b.ensurePublishedPort(info, binding); err != nil {
			return err
		}
	}
	return nil
}

func (b *runtimeBackend) ensurePublishedPort(info ttcbundle.ContainerInfo, binding ttcbundle.PortBinding) error {
	if info.NetworkMode != "ttc" {
		return fmt.Errorf("container %s publishes ports with network mode %q", info.ID, info.NetworkMode)
	}
	lockPath, errorPath := portSidecarPaths(b.dataRoot, info.ID, binding)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return fmt.Errorf("creating published-port state: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening published-port lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return fmt.Errorf("locking published port: %w", err)
	}
	errorFile, err := os.OpenFile(errorPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening published-port log: %w", err)
	}
	defer errorFile.Close()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating published-port readiness pipe: %w", err)
	}
	defer readyReader.Close()
	endpoint := fmt.Sprintf("%s:%d:%d", binding.HostIP, binding.HostPort, binding.ContainerPort)
	args := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	args = append(args, "port-forward", "--ttc-allow-stopped-sandbox", "--branch="+binding.Branch(), "--ready-fd=4", info.ID, endpoint)
	command := runscCommand(b.runsc, args...)
	command.Stdout = errorFile
	command.Stderr = errorFile
	command.ExtraFiles = []*os.File{lockFile, readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return fmt.Errorf("starting published port %s: %w", endpoint, err)
	}
	if err := readyWriter.Close(); err != nil {
		command.Process.Kill()
		command.Wait()
		return fmt.Errorf("closing published-port readiness writer: %w", err)
	}
	if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		command.Process.Kill()
		command.Wait()
		return fmt.Errorf("setting published-port readiness deadline: %w", err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyReader, ready[:]); err != nil {
		command.Process.Kill()
		command.Wait()
		diagnostic, _ := os.ReadFile(errorPath)
		return fmt.Errorf("waiting for published port %s: %w: %s", endpoint, err, strings.TrimSpace(string(diagnostic)))
	}
	if b.consoleSidecars != nil {
		if err := b.consoleSidecars.OwnPortSidecar(lockPath, command); err != nil {
			command.Process.Kill()
			command.Wait()
			return fmt.Errorf("transferring published-port ownership: %w", err)
		}
	} else if err := command.Process.Release(); err != nil {
		return fmt.Errorf("releasing published-port process: %w", err)
	}
	return nil
}

func (b *runtimeBackend) branchRuntimeState(target selector) (string, error) {
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return "", err
	}
	for _, family := range families {
		if family.ID != target.container {
			continue
		}
		for _, branch := range family.Branches {
			if branch.ID == target.branch {
				return branch.State, nil
			}
		}
		return "", fmt.Errorf("branch %q does not exist", target.branch)
	}
	return "", fmt.Errorf("container %q is not running", target.container)
}

func (b *runtimeBackend) publishPort(target selector, binding ttcbundle.PortBinding) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	binding.BranchID = target.branch
	state, err := b.branchRuntimeState(target)
	if err != nil {
		return err
	}
	if state != "running" && state != "paused" {
		return fmt.Errorf("branch %q is %s", target.branch, state)
	}
	store := b.containerStore()
	if err := store.AddPortBinding(target.container, binding); err != nil {
		return err
	}
	info.PublishedPorts = append(info.PublishedPorts, binding)
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		_ = store.RemovePortBinding(target.container, binding)
		return err
	}
	if branch.Quarantined {
		// Keep the desired binding in branch metadata. Unquarantine activates
		// it, without opening an external listener while the flag is set.
		return nil
	}
	if err := b.ensurePublishedPort(info, binding); err != nil {
		rollbackErr := store.RemovePortBinding(target.container, binding)
		if rollbackErr != nil {
			return fmt.Errorf("starting listener: %w (metadata rollback also failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("starting listener: %w", err)
	}
	return nil
}

func (b *runtimeBackend) listPorts(container string) ([]ttcbundle.PortBinding, error) {
	info, err := b.resolveContainer(container)
	if err != nil {
		return nil, err
	}
	return append([]ttcbundle.PortBinding(nil), info.PublishedPorts...), nil
}

func portForwardProcessMatches(pid int, container, branch, endpoint string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	fields := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := map[string]bool{
		"port-forward":       false,
		"--branch=" + branch: false,
		container:            false,
		endpoint:             false,
	}
	for _, field := range fields {
		if _, ok := want[field]; ok {
			want[field] = true
		}
	}
	for _, found := range want {
		if !found {
			return false
		}
	}
	return true
}

func portLockHolder(lockPath, container, branch, endpoint string) (int, error) {
	want, err := os.Stat(lockPath)
	if err != nil {
		return 0, err
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", process.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			info, err := os.Stat(filepath.Join("/proc", process.Name(), "fd", fd.Name()))
			if err == nil && os.SameFile(want, info) && portForwardProcessMatches(pid, container, branch, endpoint) {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("published-port lock is held by an unknown process")
}

func waitForPortLock(file *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for published-port listener to exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cleanupPortSidecarFiles(lockPath, errorPath string) error {
	var errs []error
	for _, name := range []string{lockPath, errorPath} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *runtimeBackend) stopPublishedPort(info ttcbundle.ContainerInfo, binding ttcbundle.PortBinding) error {
	lockPath, errorPath := portSidecarPaths(b.dataRoot, info.ID, binding)
	if b.consoleSidecars != nil {
		stopped, err := b.consoleSidecars.StopPortSidecar(lockPath)
		if err != nil {
			return err
		}
		if stopped {
			return cleanupPortSidecarFiles(lockPath, errorPath)
		}
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		return cleanupPortSidecarFiles(lockPath, errorPath)
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return err
	}
	endpoint := fmt.Sprintf("%s:%d:%d", binding.HostIP, binding.HostPort, binding.ContainerPort)
	pid, err := portLockHolder(lockPath, info.ID, binding.Branch(), endpoint)
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if err := waitForPortLock(lockFile, 2*time.Second); err != nil {
		if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return fmt.Errorf("%w; killing listener also failed: %v", err, killErr)
		}
		if lockErr := waitForPortLock(lockFile, 3*time.Second); lockErr != nil {
			return lockErr
		}
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return cleanupPortSidecarFiles(lockPath, errorPath)
}

func (b *runtimeBackend) stopPublishedPortsForBranch(info ttcbundle.ContainerInfo, branch string) error {
	var errs []error
	for _, binding := range info.PublishedPorts {
		if binding.Branch() != branch {
			continue
		}
		if err := b.stopPublishedPort(info, binding); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", portBindingSpec(binding), err))
		}
	}
	return errors.Join(errs...)
}

func (b *runtimeBackend) stopPublishedPorts(info ttcbundle.ContainerInfo) error {
	var errs []error
	for _, binding := range info.PublishedPorts {
		if err := b.stopPublishedPort(info, binding); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", portBindingSpec(binding), err))
		}
	}
	return errors.Join(errs...)
}

func (b *runtimeBackend) unpublishPort(target selector, binding ttcbundle.PortBinding) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	binding.BranchID = target.branch
	found := false
	for _, existing := range info.PublishedPorts {
		if existing.Branch() == binding.Branch() && existing.HostIP == binding.HostIP && existing.HostPort == binding.HostPort && existing.ContainerPort == binding.ContainerPort && existing.Protocol == binding.Protocol {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("published port is not configured on branch %q", target.branch)
	}
	if err := b.stopPublishedPort(info, binding); err != nil {
		return fmt.Errorf("stopping listener: %w", err)
	}
	if err := b.containerStore().RemovePortBinding(target.container, binding); err != nil {
		restoreErr := b.ensurePublishedPort(info, binding)
		if restoreErr != nil {
			return fmt.Errorf("persisting removal: %w (listener restoration also failed: %v)", err, restoreErr)
		}
		return fmt.Errorf("persisting removal: %w; listener was restored", err)
	}
	return nil
}

func (b *runtimeBackend) pull(reference, platform string, progress bool) (ttcimages.Info, error) {
	var progressFunc ttcimages.ProgressFunc
	if progress {
		progressFunc = b.imageProgress()
	}
	ctx := context.Background()
	info, err := ttcimages.Pull(ctx, ttcimages.PullOptions{
		Root:      b.dataRoot,
		Reference: reference,
		Platform:  platform,
		Progress:  progressFunc,
	})
	if err != nil {
		return ttcimages.Info{}, err
	}
	if _, _, err := b.containerStore().EnsureImageRoot(ctx, info, platform, progressFunc); err != nil {
		return ttcimages.Info{}, fmt.Errorf("preparing pulled image root: %w", err)
	}
	if _, err := b.containerStore().PruneImageRoots(ctx); err != nil {
		return ttcimages.Info{}, fmt.Errorf("pruning superseded image roots: %w", err)
	}
	return info, nil
}

func (b *runtimeBackend) loadImages(archive io.Reader, platform string) ([]ttcimages.Info, error) {
	if archive == nil {
		archive = b.stdin
	}
	if archive == nil {
		return nil, errors.New("no image archive on standard input")
	}
	ctx := context.Background()
	imported, err := ttcimages.Load(ctx, ttcimages.LoadOptions{
		Root: b.dataRoot, Archive: archive, Platform: platform,
	})
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = ttcimages.DiscardLoaded(context.Background(), b.dataRoot)
			_, _ = b.containerStore().PruneImageRoots(context.Background())
		}
	}()
	infos := make([]ttcimages.Info, 0, len(imported))
	for _, image := range imported {
		if _, _, err := b.containerStore().EnsureImportedImageRoot(ctx, image, image.Info.Platform, nil); err != nil {
			return nil, fmt.Errorf("preparing loaded image %q root: %w", image.Info.Reference, err)
		}
		infos = append(infos, image.Info)
	}
	if err := ttcimages.PublishLoaded(ctx, b.dataRoot, imported); err != nil {
		return nil, fmt.Errorf("publishing loaded images: %w", err)
	}
	published = true
	if _, err := b.containerStore().PruneImageRoots(ctx); err != nil {
		// Tags are already atomically published and their roots are usable.
		// Stale-root cleanup must not turn a successful load into a reported
		// failure that appears partially committed.
		fmt.Fprintf(b.stderr, "ttc: warning: pruning superseded image roots: %v\n", err)
	}
	return infos, nil
}

func (b *runtimeBackend) saveImages(archive io.Writer, identifiers []string, platform string) error {
	return ttcimages.Save(context.Background(), ttcimages.SaveOptions{
		Root: b.dataRoot, Archive: archive, Images: identifiers, Platform: platform,
	})
}

func (b *runtimeBackend) command(args ...string) ([]byte, error) {
	return b.commandWithFiles(nil, args...)
}

func (b *runtimeBackend) commandWithFiles(files []*os.File, args ...string) ([]byte, error) {
	global := []string{"--root", b.root}
	command := runscCommand(b.runsc, append(global, args...)...)
	command.ExtraFiles = files
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

func familyNodeID(info ttcbundle.ContainerInfo) string {
	owner := ""
	for _, branch := range info.Branches {
		branchOwner := info.BranchNodeID(branch.ID)
		if branchOwner == "" {
			continue
		}
		if owner != "" && owner != branchOwner {
			return ""
		}
		owner = branchOwner
	}
	if owner == "" {
		return info.NodeID
	}
	return owner
}

func containerHasBranchOnNode(info ttcbundle.ContainerInfo, nodeID string) bool {
	if nodeID == "" {
		return true
	}
	for _, branch := range info.Branches {
		owner := info.BranchNodeID(branch.ID)
		if owner == "" || owner == nodeID {
			return true
		}
	}
	return false
}

func allBranchesColdOnNode(info ttcbundle.ContainerInfo, nodeID string) (allCold bool, count int) {
	allCold = true
	for _, branch := range info.Branches {
		if nodeID != "" && info.BranchNodeID(branch.ID) != nodeID {
			continue
		}
		count++
		switch normalizeLifecycleState(branch.State) {
		case "created", "dehydrating", "dehydrated", "exited", "failed", "dead":
		default:
			allCold = false
		}
	}
	return allCold, count
}

func (b *runtimeBackend) listFamilies() ([]Family, error) {
	runtimeFamilies, runtimeErr := b.listRuntimeFamilies()
	if b.dataRoot == "" {
		return runtimeFamilies, runtimeErr
	}
	store := ttcbundle.Store{Root: b.dataRoot}
	containers, storeErr := store.ListContainers()
	if storeErr != nil {
		return nil, storeErr
	}
	byID := make(map[string]Family, len(containers)+len(runtimeFamilies))
	for _, container := range containers {
		state := normalizeLifecycleState(container.State)
		if state == "" {
			state = "created"
		}
		byID[container.ID] = Family{
			ID:               container.ID,
			NodeID:           familyNodeID(container),
			Name:             container.Name,
			Image:            container.Reference,
			Digest:           container.Digest,
			ImageSize:        container.ImageSize,
			LiveForkEligible: container.LiveForkEligible,
			LiveForkWaiver:   container.LiveForkWaiver,
			State:            state,
			CreatedAt:        container.CreatedAt,
			Branches:         persistentBranches(container.Branches, container.NodeID),
		}
	}
	// A single catalog snapshot is authoritative for branches that have entered
	// elastic placement. Persistent container metadata remains the compatibility
	// source for older/cold branches, but must not expose a half-published target
	// during ownership cutover.
	if b.nodeID != "" {
		if placements, err := b.placementCatalog(); err == nil {
			if records, err := placements.List(); err == nil {
				for _, record := range records {
					family, ok := byID[record.FamilyID]
					if !ok || record.State == cluster.PlacementUnplaced {
						continue
					}
					for i := range family.Branches {
						if family.Branches[i].ID == record.BranchID {
							family.Branches[i].NodeID = record.OwnerNode
							break
						}
					}
					owner := ""
					for _, branch := range family.Branches {
						if branch.NodeID == "" {
							continue
						}
						if owner != "" && owner != branch.NodeID {
							owner = ""
							break
						}
						owner = branch.NodeID
					}
					family.NodeID = owner
					byID[record.FamilyID] = family
				}
			}
		}
	}
	if runtimeErr == nil {
		runtimeIDs := make(map[string]struct{}, len(runtimeFamilies))
		for _, family := range runtimeFamilies {
			// A runsc state directory can outlive its Sentry after SIGKILL, which
			// is common when a batch scheduler terminates a whole job. Merely
			// appearing in `runsc list` does not make that runtime available.
			// Keep a live Sentry authoritative during a transient control-server
			// failure, but reconcile a failed RPC backed by a dead (or zombie)
			// sandbox process as runtime loss.
			if !runtimeFamilyUnavailable(family) {
				runtimeIDs[family.ID] = struct{}{}
			}
			if persistent, ok := byID[family.ID]; ok {
				family.NodeID = persistent.NodeID
				family.Name = persistent.Name
				family.Image = persistent.Image
				family.Digest = persistent.Digest
				family.ImageSize = persistent.ImageSize
				family.LiveForkEligible = persistent.LiveForkEligible
				family.LiveForkWaiver = persistent.LiveForkWaiver
				family.CreatedAt = persistent.CreatedAt
				liveLocal := make([]Branch, 0, len(family.Branches))
				persistentByID := make(map[string]Branch, len(persistent.Branches))
				for _, branch := range persistent.Branches {
					persistentByID[branch.ID] = branch
				}
				for _, branch := range family.Branches {
					owner := persistentByID[branch.ID].NodeID
					if b.nodeID == "" || owner == "" || owner == b.nodeID {
						liveLocal = append(liveLocal, branch)
					}
				}
				family.Branches = mergeBranchHistory(persistent.Branches, liveLocal)
				family.State = familyStateFromBranches(family.Branches)
				for i := range family.Branches {
					if family.Branches[i].ID == defaultBranchID {
						family.Branches[i].CreatedAt = persistent.CreatedAt
					}
				}
			} else if len(family.Branches) != 0 {
				family.NodeID = b.nodeID
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
			if b.nodeID != "" && !containerHasBranchOnNode(container, b.nodeID) {
				continue
			}
			if _, ok := runtimeIDs[container.ID]; ok {
				continue
			}
			// Process-free states intentionally have no live Sentry. This also
			// covers the publication window for `runsc create` and the short
			// interval after a foreground init exits but before its waiter records
			// the result: persistent state is still "created" in both cases.
			// Inventory must not force-delete runtime metadata in either window.
			if !persistentFamilyRequiredRuntime(container, b.nodeID) {
				continue
			}
			var exitStatus *int
			// A wait-status file belongs to one incarnation of main. Do not
			// attribute an old main exit to a different live branch that was
			// killed with the sandbox.
			if persistentBranchRequiredRuntime(container, defaultBranchID) {
				if paths, err := store.BranchConsolePaths(container.ID, defaultBranchID); err == nil {
					if status, found, err := readConsoleWaitStatus(paths.WaitStatus); err == nil && found {
						code := waitStatusExitCode(status)
						exitStatus = &code
					}
				}
			}
			// Once the Sentry is known to be gone, no console follower or gofer
			// may retain files from the persistent container directory. Reap the
			// sidecars before recording process-free state so later removal is
			// also safe on NFS, where unlinking an open file creates .nfs files.
			_ = b.stopContainerConsoleDrains(container.ID)
			_, _ = b.command("delete", "--force", container.ID)
			if err := store.ReconcileNodeRuntimeUnavailable(container.ID, b.nodeID, exitStatus); err != nil {
				continue
			}
			if reconciled, err := store.LoadContainer(container.ID); err == nil {
				// No process can fault the imported backing after the family
				// runtime is gone. Persist the exited state first, then release
				// the cluster root; a failure remains conservative and a later
				// listing retries the idempotent cleanup.
				if persistentFamilyProcessFree(reconciled) {
					if err := b.releaseDistributedSparseRoot(reconciled); err == nil {
						if latest, err := store.LoadContainer(container.ID); err == nil {
							reconciled = latest
						}
					}
				}
				family := byID[container.ID]
				family.State = normalizeLifecycleState(reconciled.State)
				family.Branches = persistentBranches(reconciled.Branches, reconciled.NodeID)
				family.SandboxPID = 0
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
	// In clustered TTC, Raft is the lifecycle authority. Shared container JSON
	// is a materialization catalog and may lag a lease-expiry or migration
	// decision; never let it make a fenced/corrupted branch appear runnable.
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		authorities, err := b.distributedSource.authority.Branches()
		if err != nil {
			return nil, fmt.Errorf("reading cluster branch authority: %w", err)
		}
		placementByBranch := make(map[string]cluster.PlacementRecord)
		if placements, err := b.placementCatalog(); err == nil {
			if records, err := placements.List(); err == nil {
				for _, record := range records {
					placementByBranch[record.FamilyID+"\x00"+record.BranchID] = record
				}
			}
		}
		for _, authority := range authorities {
			family, ok := byID[authority.FamilyID]
			if !ok {
				continue
			}
			for i := range family.Branches {
				if family.Branches[i].ID != authority.BranchID {
					continue
				}
				family.Branches[i].State = authority.Lifecycle
				if authority.Roots.FilesystemRoot != "" {
					family.Branches[i].StorageManifest = authority.Roots.FilesystemRoot
				}
				placement, placed := placementByBranch[authority.FamilyID+"\x00"+authority.BranchID]
				if !placed || placement.State == cluster.PlacementUnplaced || placement.State == cluster.PlacementCorrupted {
					family.Branches[i].NodeID = ""
				} else {
					family.Branches[i].NodeID = placement.OwnerNode
				}
				if authority.Corrupted {
					family.Error = fmt.Sprintf("branch %s is corrupted: %s", authority.BranchID, authority.CorruptionReason)
				}
				break
			}
			family.State = familyStateFromBranches(family.Branches)
			byID[authority.FamilyID] = family
		}
	}
	families := make([]Family, 0, len(byID))
	for _, family := range byID {
		family.State = normalizeLifecycleState(family.State)
		for i := range family.Branches {
			family.Branches[i].State = normalizeLifecycleState(family.Branches[i].State)
		}
		enrichPersistentBranchDiagnostics(&family)
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

// runtimeFamilyUnavailable reports whether a runsc list entry is only stale
// on-disk runtime metadata. A failed branch RPC alone is insufficient because
// the control server can be temporarily unavailable while the Sentry remains
// alive; the process-liveness check is the second required signal.
func runtimeFamilyUnavailable(family Family) bool {
	return family.Error != "" && !processutil.IsRunning(family.SandboxPID)
}

func persistentBranchRequiredRuntime(container ttcbundle.ContainerInfo, branchID string) bool {
	state := ""
	for _, branch := range container.Branches {
		if branch.ID == branchID {
			state = branch.State
			break
		}
	}
	if state == "" && branchID == defaultBranchID {
		state = container.State
	}
	switch state {
	case "creating", "running", "paused", "dehydrating", "rehydrating":
		return true
	default:
		return false
	}
}

func persistentFamilyRequiredRuntime(container ttcbundle.ContainerInfo, nodeID string) bool {
	for _, branch := range container.Branches {
		owner := container.BranchNodeID(branch.ID)
		if nodeID != "" && owner != "" && owner != nodeID {
			continue
		}
		switch normalizeLifecycleState(branch.State) {
		case "creating", "running", "paused", "dehydrating", "rehydrating", "restarting":
			return true
		}
	}
	return false
}

func enrichPersistentBranchDiagnostics(family *Family) {
	for i := range family.Branches {
		branch := &family.Branches[i]
		if branch.InitState == "" {
			switch branch.State {
			case "created", "creating":
				branch.InitState = "not-started"
			case "exited", "stopped", "failed":
				branch.InitState = "exited"
			default:
				branch.InitState = "unknown"
			}
		}
		if branch.MemorySharing == "" {
			switch branch.State {
			case "created", "creating":
				branch.MemorySharing = "unallocated"
			case "exited", "stopped", "failed":
				branch.MemorySharing = "released"
			default:
				branch.MemorySharing = "unknown"
			}
		}
	}
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
		if state.ID == "" {
			continue
		}
		family := Family{ID: state.ID, State: normalizeLifecycleState(state.Status), SandboxPID: state.PID, Branches: []Branch{}}
		output, err := b.command("timetravel", state.ID, "branches")
		if err != nil {
			family.Error = err.Error()
			family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: normalizeLifecycleState(state.Status)})
			families = append(families, family)
			continue
		}
		var branches []runscBranchInfo
		if err := json.Unmarshal(output, &branches); err != nil {
			family.Error = fmt.Sprintf("decoding runsc branch list: %v", err)
			family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: normalizeLifecycleState(state.Status)})
			families = append(families, family)
			continue
		}
		for _, branch := range branches {
			family.Branches = append(family.Branches, branchView(branch))
			if branch.ID == defaultBranchID {
				family.State = normalizeLifecycleState(branch.State)
			}
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

func (b *runtimeBackend) consoleObservation(target selector, session string) (kernel.InternalConsoleObservation, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return kernel.InternalConsoleObservation{}, err
	}
	consoleID := "init"
	if session != "init" {
		execSession, err := b.execSession(resolved, session)
		if err != nil {
			return kernel.InternalConsoleObservation{}, err
		}
		if !execSession.Terminal {
			return kernel.InternalConsoleObservation{}, fmt.Errorf("exec session %q is not a terminal", session)
		}
		consoleID = execSession.ConsoleID
	}
	output, err := b.command("timetravel", "--console", consoleID, resolved.container, "console-stat", resolved.branch)
	if err != nil {
		return kernel.InternalConsoleObservation{}, err
	}
	var observation kernel.InternalConsoleObservation
	if err := json.Unmarshal(output, &observation); err != nil {
		return kernel.InternalConsoleObservation{}, fmt.Errorf("decoding terminal observation: %w", err)
	}
	return observation, nil
}

func (b *runtimeBackend) watchConsole(target selector, session string, stdout io.Writer) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	cont, err := container.Load(b.root, container.FullID{SandboxID: resolved.container, ContainerID: resolved.container}, container.LoadOpts{Exact: true, SkipCheck: true})
	if err != nil {
		return fmt.Errorf("loading sandbox for terminal watch: %w", err)
	}
	consoleID := "init"
	if session != "init" {
		sessions, err := cont.Sandbox.ExecSessions(&boot.ExecSessionsArgs{BranchID: resolved.branch, CID: resolved.container})
		if err != nil {
			return err
		}
		var execSession *boot.ExecSessionInfo
		for i := range sessions {
			if sessions[i].ID == session {
				execSession = &sessions[i]
				break
			}
		}
		if execSession == nil {
			return fmt.Errorf("exec session %q does not exist in %s", session, resolved)
		}
		if !execSession.Terminal {
			return fmt.Errorf("exec session %q is not a terminal", session)
		}
		consoleID = execSession.ConsoleID
	}
	return cont.Sandbox.WatchConsole(&boot.ConsoleObservationArgs{
		BranchID: resolved.branch, ConsoleID: consoleID,
	}, stdout)
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
	ID               string    `json:"ID"`
	ParentID         string    `json:"ParentID"`
	FilesystemBranch string    `json:"FilesystemBranch"`
	State            string    `json:"State"`
	CreatedAt        time.Time `json:"CreatedAt"`
	InitPID          int32     `json:"InitPID"`
	InitState        string    `json:"InitState"`
	SharesMemory     bool      `json:"SharesMemory"`
	Quarantined      bool      `json:"Quarantined"`
	ForkMetrics      struct {
		SourceDowntime time.Duration `json:"SourceDowntime"`
		Kernel         struct {
			Total time.Duration `json:"Total"`
		} `json:"Kernel"`
		Total time.Duration `json:"Total"`
	} `json:"ForkMetrics"`
}

// normalizeLifecycleState keeps OCI's low-level "stopped" spelling out of
// TTC's public state model. Like Docker, TTC exposes every terminal container
// or branch as "exited", regardless of whether termination was requested.
func normalizeLifecycleState(state string) string {
	if state == "stopped" {
		return "exited"
	}
	return state
}

func persistentBranchView(info ttcbundle.BranchInfo, fallbackNode string) Branch {
	nodeID := info.NodeID
	if nodeID == "" && !info.Unplaced {
		nodeID = fallbackNode
	}
	return Branch{
		ID: info.ID, NodeID: nodeID, ParentID: info.ParentID, Quarantined: info.Quarantined, State: normalizeLifecycleState(info.State), CreatedAt: info.CreatedAt,
		StorageState: info.StorageState, StorageGeneration: info.StorageGeneration, StorageError: info.StorageError,
		StorageManifest: info.StorageManifest,
		StorageRetryAt:  info.StorageRetryAt, SourceNode: info.SourceNode, DependentMemoryBytes: info.DependentMemoryBytes,
		DependentFilesystemBytes: info.DependentFilesystemBytes,
		ForkTotalNS:              info.ForkTotalNS, SourceDowntimeNS: info.SourceDowntimeNS, KernelForkNS: info.KernelForkNS,
	}
}

func persistentBranches(infos []ttcbundle.BranchInfo, fallbackNode string) []Branch {
	branches := make([]Branch, 0, len(infos))
	for _, info := range infos {
		branches = append(branches, persistentBranchView(info, fallbackNode))
	}
	return branches
}

func persistentBranchInfo(info ttcbundle.ContainerInfo, branchID string) (ttcbundle.BranchInfo, error) {
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			return branch, nil
		}
	}
	return ttcbundle.BranchInfo{}, fmt.Errorf("branch %q does not exist", branchID)
}

func memoryBranchDir(dataRoot, containerID, kind string, branch ttcbundle.BranchInfo) string {
	return filepath.Join(dataRoot, "containers", containerID, "memory", kind, ttcbundle.MemoryBranchKey(branch))
}

// existingMemoryBranchDir returns the only memory layout accepted by the
// current root format. Older visible-name layouts are rejected at the root
// boundary and must never be consulted from a new-format root.
func existingMemoryBranchDir(dataRoot, containerID, kind string, branch ttcbundle.BranchInfo) string {
	return memoryBranchDir(dataRoot, containerID, kind, branch)
}

func mergeBranchHistory(history, live []Branch) []Branch {
	byID := make(map[string]Branch, len(history)+len(live))
	for _, branch := range history {
		byID[branch.ID] = branch
	}
	for _, branch := range live {
		if recorded, ok := byID[branch.ID]; ok {
			// ParentID is durable display metadata. The runtime may retain the
			// branch's bootstrap hint after that parent is removed while the
			// parent itself was already process-free, so it is not authoritative
			// for lineage reconciliation.
			branch.ParentID = recorded.ParentID
			// Quarantine is user-controlled durable policy. In particular, it can
			// change while a process-free branch remains listed by a live peer's
			// Sentry, whose cached BranchInfo is not authoritative.
			branch.Quarantined = recorded.Quarantined
			branch.NodeID = recorded.NodeID
			branch.StorageState = recorded.StorageState
			branch.StorageGeneration = recorded.StorageGeneration
			branch.StorageError = recorded.StorageError
			branch.StorageManifest = recorded.StorageManifest
			branch.StorageRetryAt = recorded.StorageRetryAt
			branch.SourceNode = recorded.SourceNode
			branch.DependentMemoryBytes = recorded.DependentMemoryBytes
			branch.DependentFilesystemBytes = recorded.DependentFilesystemBytes
		}
		byID[branch.ID] = branch
	}
	merged := make([]Branch, 0, len(byID))
	for _, branch := range byID {
		merged = append(merged, branch)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged
}

func familyStateFromBranches(branches []Branch) string {
	priority := map[string]int{
		"exited": 1, "failed": 2, "dead": 2, "dehydrated": 3, "created": 4,
		"paused": 5, "dehydrating": 6, "rehydrating": 7, "creating": 8,
		"restarting": 9, "running": 10,
	}
	state, best := "exited", 0
	for _, branch := range branches {
		if rank := priority[normalizeLifecycleState(branch.State)]; rank > best {
			state, best = normalizeLifecycleState(branch.State), rank
		}
	}
	return state
}

func memorySharingMode(shared bool) string {
	if shared {
		return "cow"
	}
	return "private"
}

func branchView(info runscBranchInfo) Branch {
	return Branch{
		ID:               info.ID,
		ParentID:         info.ParentID,
		Quarantined:      info.Quarantined,
		State:            normalizeLifecycleState(info.State),
		CreatedAt:        info.CreatedAt,
		InitPID:          info.InitPID,
		InitState:        info.InitState,
		MemorySharing:    memorySharingMode(info.SharesMemory),
		ForkTotalNS:      info.ForkMetrics.Total.Nanoseconds(),
		SourceDowntimeNS: info.ForkMetrics.SourceDowntime.Nanoseconds(),
		KernelForkNS:     info.ForkMetrics.Kernel.Total.Nanoseconds(),
	}
}

func (b *runtimeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	branches, err := b.forkMany(source, []string{destination}, paused, false)
	if err != nil {
		return Branch{}, err
	}
	return branches[0], nil
}

func (b *runtimeBackend) forkMany(source selector, destinations []string, paused, quarantine bool) ([]Branch, error) {
	if len(destinations) == 0 {
		return nil, errors.New("at least one fork destination is required")
	}
	resolved, containerInfo, err := b.resolveSelector(source)
	if err != nil {
		return nil, err
	}
	source = resolved
	if !containerInfo.LiveForkEligible {
		return nil, fmt.Errorf("container family %q is not live fork eligible: %s", containerInfo.Name, containerInfo.LiveForkWaiver)
	}
	seen := make(map[string]struct{}, len(destinations))
	for _, destination := range destinations {
		if _, ok := seen[destination]; ok {
			return nil, fmt.Errorf("branch name %q is repeated", destination)
		}
		seen[destination] = struct{}{}
		for _, existing := range containerInfo.Branches {
			if existing.ID == destination {
				return nil, fmt.Errorf("branch %q already exists", destination)
			}
		}
	}
	sourceRecord, err := persistentBranchInfo(containerInfo, source.branch)
	if err != nil {
		return nil, err
	}
	if state := normalizeLifecycleState(sourceRecord.State); state == "dehydrated" || state == "dehydrating" {
		return b.forkDehydratedMany(source, containerInfo, sourceRecord, destinations, paused, sourceRecord.Quarantined || quarantine)
	}
	args := []string{"timetravel", "--source", source.branch}
	if paused {
		args = append(args, "--paused")
	}
	if quarantine {
		args = append(args, "--quarantine")
	}
	args = append(args, source.container, "fork")
	args = append(args, destinations...)
	output, err := b.command(args...)
	if err != nil {
		return nil, err
	}
	infos := make([]runscBranchInfo, 0, len(destinations))
	if len(destinations) == 1 {
		var info runscBranchInfo
		if err := json.Unmarshal(output, &info); err != nil {
			return nil, fmt.Errorf("decoding runsc fork result: %w", err)
		}
		infos = append(infos, info)
	} else if err := json.Unmarshal(output, &infos); err != nil {
		return nil, fmt.Errorf("decoding runsc batch-fork result: %w", err)
	}
	branches := make([]Branch, len(infos))
	for i, info := range infos {
		branches[i] = branchView(info)
	}
	if b.dataRoot != "" {
		records := make([]ttcbundle.BranchInfo, len(branches))
		for i, branch := range branches {
			records[i] = ttcbundle.BranchInfo{
				ID: branch.ID, ParentID: branch.ParentID, FilesystemBranch: infos[i].FilesystemBranch,
				Quarantined: branch.Quarantined,
				State:       branch.State, StorageState: "local", StorageManifest: sourceRecord.StorageManifest, CreatedAt: branch.CreatedAt,
				ForkTotalNS: branch.ForkTotalNS, SourceDowntimeNS: branch.SourceDowntimeNS, KernelForkNS: branch.KernelForkNS,
			}
		}
		graphRoots, err := b.cloneGraphMemoryRootsMany(sourceRecord.MemoryRoots, records)
		if err != nil {
			var cleanupErr error
			store := b.containerStore()
			for i, cleanupDestination := range destinations {
				if _, destroyErr := b.command("timetravel", source.container, "destroy", cleanupDestination); destroyErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("destroying %q: %w", cleanupDestination, destroyErr))
					continue
				}
				record := ttcbundle.BranchInfo{ID: cleanupDestination, FilesystemBranch: infos[i].FilesystemBranch}
				cleanupErr = errors.Join(cleanupErr, store.RemoveBranchFilesystems(source.container, record))
				cleanupErr = errors.Join(cleanupErr, store.RemoveBranchArtifacts(source.container, record))
			}
			if cleanupErr != nil {
				return nil, fmt.Errorf("rooting graph memory for batch fork: %w; cleanup: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("rooting graph memory for batch fork: %w", err)
		}
		for i := range records {
			records[i].SnapshotID = sourceRecord.SnapshotID
			records[i].MemoryRoots = graphRoots[records[i].FilesystemBranch]
		}
		if err := b.containerStore().RecordBranches(source.container, records); err != nil {
			var cleanupErr error
			store := b.containerStore()
			for _, roots := range graphRoots {
				cleanupErr = errors.Join(cleanupErr, b.releaseGraphMemoryRoots(roots))
			}
			for i, cleanupDestination := range destinations {
				if _, destroyErr := b.command("timetravel", source.container, "destroy", cleanupDestination); destroyErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("destroying %q: %w", cleanupDestination, destroyErr))
					continue
				}
				record := ttcbundle.BranchInfo{ID: cleanupDestination, FilesystemBranch: infos[i].FilesystemBranch}
				if filesystemErr := store.RemoveBranchFilesystems(source.container, record); filesystemErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("reclaiming %q filesystems: %w", cleanupDestination, filesystemErr))
				}
				if artifactErr := store.RemoveBranchArtifacts(source.container, record); artifactErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("removing %q artifacts: %w", cleanupDestination, artifactErr))
				}
			}
			if cleanupErr != nil {
				return nil, fmt.Errorf("persisting batch fork lineage: %w; cleaning unrecorded branches: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("persisting batch fork lineage: %w; batch branches were destroyed", err)
		}
		for i := range records {
			if err := b.cloneSparseMemoryRoot(containerInfo.ID, sourceRecord.FilesystemBranch, records[i].FilesystemBranch, sourceRecord.StorageManifest != ""); err != nil {
				var cleanupErr error
				for _, cleanupDestination := range destinations {
					cleanupErr = errors.Join(cleanupErr, b.destroy(selector{container: source.container, branch: cleanupDestination}, true))
				}
				if cleanupErr != nil {
					return nil, fmt.Errorf("rooting sparse memory for branch %q: %w; cleaning batch branches: %v", records[i].ID, err, cleanupErr)
				}
				return nil, fmt.Errorf("rooting sparse memory for branch %q: %w; batch branches were destroyed", records[i].ID, err)
			}
		}
	}
	for _, destination := range destinations {
		target := selector{container: source.container, branch: destination}
		if _, err := b.ensureConsoleDrain(target, true); err != nil {
			for _, cleanupDestination := range destinations {
				_ = b.destroy(selector{container: source.container, branch: cleanupDestination}, true)
			}
			return nil, fmt.Errorf("starting console drain after batch fork: %w; batch branches were destroyed", err)
		}
		sessions, err := b.execSessions(target)
		if err != nil {
			for _, cleanupDestination := range destinations {
				_ = b.destroy(selector{container: source.container, branch: cleanupDestination}, true)
			}
			return nil, fmt.Errorf("listing cloned exec sessions after batch fork: %w; batch branches were destroyed", err)
		}
		for _, session := range sessions {
			if session.State != "running" {
				continue
			}
			if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, true); err != nil {
				for _, cleanupDestination := range destinations {
					_ = b.destroy(selector{container: source.container, branch: cleanupDestination}, true)
				}
				return nil, fmt.Errorf("starting cloned exec console drain %q: %w; batch branches were destroyed", session.ID, err)
			}
		}
	}

	return branches, nil
}

// forkDehydratedMany creates local children directly from durable branch
// state. The source remains process-free and its dehydration image is never
// consumed. Filesystem metadata and sparse-memory manifests are immutable, so
// preparing any number of siblings does not copy file or memory data.
func (b *runtimeBackend) forkDehydratedMany(source selector, info ttcbundle.ContainerInfo, sourceBranch ttcbundle.BranchInfo, destinations []string, paused, quarantined bool) (_ []Branch, retErr error) {
	if b.dataRoot == "" {
		return nil, errors.New("forking a dehydrated branch requires a persistent TTC data root")
	}
	if err := b.ensureLocalColdBranch(info, sourceBranch); err != nil {
		return nil, fmt.Errorf("materializing dehydrated source: %w", err)
	}
	sourceDir := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", sourceBranch)
	controlData, err := os.ReadFile(filepath.Join(sourceDir, "control.json"))
	if err != nil {
		return nil, fmt.Errorf("reading dehydrated source control state: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return nil, fmt.Errorf("decoding dehydrated source control state: %w", err)
	}
	if capture.FilesystemBranch == "" || len(capture.Memories) == 0 {
		return nil, errors.New("dehydrated source control state is incomplete")
	}
	if sourceBranch.FilesystemBranch != "" && sourceBranch.FilesystemBranch != capture.FilesystemBranch {
		return nil, fmt.Errorf("dehydrated source filesystem branch changed from %q to %q", sourceBranch.FilesystemBranch, capture.FilesystemBranch)
	}
	metadataData, err := os.ReadFile(filepath.Join(sourceDir, "metadata.state"))
	if err != nil {
		return nil, fmt.Errorf("reading dehydrated source metadata: %w", err)
	}

	type preparedChild struct {
		target selector
		branch ttcbundle.BranchInfo
		dir    string
	}
	children := make([]preparedChild, 0, len(destinations))
	rollback := func() error {
		var result error
		for i := len(children) - 1; i >= 0; i-- {
			child := children[i]
			latest, loadErr := b.containerStore().LoadContainer(info.ID)
			if loadErr == nil {
				if recorded, recordErr := persistentBranchInfo(latest, child.branch.ID); recordErr == nil {
					result = errors.Join(result, b.destroy(child.target, true))
					_ = recorded
					continue
				}
			}
			result = errors.Join(result, os.RemoveAll(child.dir))
			result = errors.Join(result, b.releaseSparseMemoryRoot(info.ID, child.branch.FilesystemBranch))
			result = errors.Join(result, b.releaseGraphMemoryRoots(child.branch.MemoryRoots))
			result = errors.Join(result, b.containerStore().RemoveBranchFilesystems(info.ID, child.branch))
		}
		return result
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, rollback())
		}
	}()

	createdAt := time.Now().UTC()
	records := make([]ttcbundle.BranchInfo, 0, len(destinations))
	for _, destination := range destinations {
		filesystemBranch := ttcvolume.ScopedBranchID(info.ID, destination)
		childDir := filepath.Join(filepath.Dir(sourceDir), hex.EncodeToString([]byte(destination)))
		branch := ttcbundle.BranchInfo{ID: destination, ParentID: source.branch, FilesystemBranch: filesystemBranch, Quarantined: quarantined, State: "dehydrated", StorageState: "local", StorageManifest: sourceBranch.StorageManifest, SnapshotID: sourceBranch.SnapshotID, CreatedAt: createdAt}
		children = append(children, preparedChild{target: selector{container: source.container, branch: destination}, branch: branch, dir: childDir})
		graphRoots, err := b.cloneGraphMemoryRoots(sourceBranch.MemoryRoots, filesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("rooting graph memory for %s@%s: %w", source.container, destination, err)
		}
		branch.MemoryRoots = graphRoots
		children[len(children)-1].branch = branch
		transactions, err := b.forkDehydratedDistributedFilesystems(info.ID, capture.FilesystemBranch, filesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("forking filesystems for %s@%s: %w", source.container, destination, err)
		}
		for index, transaction := range transactions {
			if err := transaction.commit(); err != nil {
				for abortIndex := len(transactions) - 1; abortIndex > index; abortIndex-- {
					_ = transactions[abortIndex].abort()
				}
				return nil, fmt.Errorf("committing filesystem fork for %s@%s: %w", source.container, destination, err)
			}
		}
		if capture.SparseManifest != (ttcchunk.ManifestID{}) {
			repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks"))
			if err != nil {
				return nil, err
			}
			if sourceBranch.StorageManifest != "" {
				err = repository.SetBranchLazy(filesystemBranch, capture.SparseManifest)
			} else {
				err = repository.SetBranch(filesystemBranch, capture.SparseManifest)
			}
			closeErr := repository.Close()
			if err != nil {
				return nil, fmt.Errorf("rooting sparse memory for %s@%s: %w", source.container, destination, err)
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}

		childCapture := capture
		childCapture.FilesystemBranch = filesystemBranch
		childCapture.Resume = capture.Resume && !paused
		if len(graphRoots) != 0 {
			if len(childCapture.MemoryRoots) != len(graphRoots) {
				return nil, fmt.Errorf("dehydrated source has %d graph control roots and %d branch roots", len(childCapture.MemoryRoots), len(graphRoots))
			}
			childCapture.MemoryRoots = append([]boot.BranchMemoryRoot(nil), childCapture.MemoryRoots...)
			for index := range childCapture.MemoryRoots {
				childCapture.MemoryRoots[index].Name = graphRoots[index]
			}
		}
		if _, err := os.Stat(childDir); err == nil {
			return nil, fmt.Errorf("dehydration image already exists for branch %q", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		staging, err := os.MkdirTemp(filepath.Dir(sourceDir), ".fork-"+hex.EncodeToString([]byte(destination))+"-")
		if err != nil {
			return nil, err
		}
		if err := writeDurableJSON(filepath.Join(staging, "control.json"), &childCapture); err != nil {
			_ = os.RemoveAll(staging)
			return nil, err
		}
		if err := writeDurableDistributedFile(filepath.Join(staging, "metadata.state"), metadataData); err != nil {
			_ = os.RemoveAll(staging)
			return nil, err
		}
		if capture.SparseManifest == (ttcchunk.ManifestID{}) && len(capture.MemoryRoots) == 0 {
			for index := range capture.Memories {
				name := fmt.Sprintf("memory-%04d.snapshot", index)
				if err := copyCheckpointHostFile(filepath.Join(sourceDir, name), filepath.Join(staging, name), 0600); err != nil {
					_ = os.RemoveAll(staging)
					return nil, err
				}
			}
		}
		if err := os.Rename(staging, childDir); err != nil {
			_ = os.RemoveAll(staging)
			return nil, err
		}
		records = append(records, branch)
	}
	if err := b.containerStore().RecordBranches(info.ID, records); err != nil {
		return nil, fmt.Errorf("recording dehydrated fork children: %w", err)
	}

	peer := false
	if output, err := b.command("timetravel", info.ID, "branches"); err == nil && len(output) != 0 {
		peer = true
	} else {
		// A fully cold family may leave an OCI shell whose control server has
		// already exited. It has no state and must not block creation of the
		// first child runtime.
		_, _ = b.command("delete", "--force", info.ID)
	}
	result := make([]Branch, 0, len(children))
	for _, child := range children {
		if err := b.rehydrateCold(child.target, info, child.branch, peer); err != nil {
			return nil, fmt.Errorf("starting dehydrated fork child %s: %w", child.target, err)
		}
		peer = true
		state := "paused"
		if capture.Resume && !paused {
			state = "running"
		}
		result = append(result, Branch{ID: child.branch.ID, ParentID: source.branch, Quarantined: child.branch.Quarantined, State: state, CreatedAt: createdAt, MemorySharing: "cow"})
	}
	return result, nil
}

func (b *runtimeBackend) pause(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	_, err = b.command("timetravel", target.container, "pause", target.branch)
	if err == nil && b.dataRoot != "" {
		err = b.containerStore().UpdateBranchState(target.container, target.branch, "paused")
	}
	return err
}

func (b *runtimeBackend) resume(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	_, err = b.command("timetravel", target.container, "resume", target.branch)
	if err == nil && b.dataRoot != "" {
		err = b.containerStore().UpdateBranchState(target.container, target.branch, "running")
	}
	return err
}

func (b *runtimeBackend) setQuarantine(target selector, enabled bool) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return err
	}
	if enabled && info.NetworkMode == "host" {
		return fmt.Errorf("container %q uses host networking, which cannot be quarantined", info.Name)
	}
	if branch.Quarantined == enabled {
		return nil
	}
	state := normalizeLifecycleState(branch.State)
	live := state == "running" || state == "paused"
	runtimeMutable := live
	if state == "dehydrated" {
		// A dehydrated branch may still have a process-free record in a Sentry
		// that owns live siblings. Keep that cache coherent when it exists; an
		// entirely cold family has no control server and needs only the durable
		// update below.
		if output, listErr := b.command("timetravel", info.ID, "branches"); listErr == nil {
			var runtimeBranches []runscBranchInfo
			if err := json.Unmarshal(output, &runtimeBranches); err != nil {
				return fmt.Errorf("decoding runtime branches before changing quarantine: %w", err)
			}
			for _, runtimeBranch := range runtimeBranches {
				if runtimeBranch.ID == target.branch {
					runtimeMutable = true
					break
				}
			}
		} else {
			for _, sibling := range info.Branches {
				state := normalizeLifecycleState(sibling.State)
				if state == "running" || state == "paused" {
					return fmt.Errorf("checking the live family before changing dehydrated branch quarantine: %w", listErr)
				}
			}
		}
	}
	for i := range info.Branches {
		if info.Branches[i].ID == target.branch {
			info.Branches[i].Quarantined = enabled
			break
		}
	}
	if enabled && live {
		if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
			return fmt.Errorf("closing published ports before quarantining %s: %w", target, err)
		}
	}
	transitionRuntime := func(value bool) error {
		if !runtimeMutable {
			return nil
		}
		action := "quarantine"
		if !value {
			action = "unquarantine"
		}
		_, err := b.command("timetravel", target.container, action, target.branch)
		return err
	}
	if runtimeMutable {
		if err := transitionRuntime(enabled); err != nil {
			if enabled {
				_ = b.ensurePublishedPortsForBranch(infoWithBranchQuarantine(info, target.branch, false), target.branch)
			}
			return err
		}
	}
	if !enabled && live {
		if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
			_ = b.stopPublishedPortsForBranch(info, target.branch)
			_ = transitionRuntime(true)
			return fmt.Errorf("opening published ports after unquarantining %s: %w", target, err)
		}
	}
	if b.dataRoot == "" {
		return nil
	}
	if err := b.containerStore().UpdateBranchQuarantine(target.container, target.branch, enabled); err != nil {
		var rollbackErr error
		if !enabled && live {
			rollbackErr = errors.Join(rollbackErr, b.stopPublishedPortsForBranch(info, target.branch))
		}
		rollbackErr = errors.Join(rollbackErr, transitionRuntime(branch.Quarantined))
		if enabled && live {
			rollbackErr = errors.Join(rollbackErr, b.ensurePublishedPortsForBranch(infoWithBranchQuarantine(info, target.branch, false), target.branch))
		}
		if rollbackErr != nil {
			return fmt.Errorf("persisting quarantine: %w; restoring runtime state: %v", err, rollbackErr)
		}
		return err
	}
	return nil
}

func infoWithBranchQuarantine(info ttcbundle.ContainerInfo, branchID string, quarantined bool) ttcbundle.ContainerInfo {
	info.Branches = append([]ttcbundle.BranchInfo(nil), info.Branches...)
	for i := range info.Branches {
		if info.Branches[i].ID == branchID {
			info.Branches[i].Quarantined = quarantined
			break
		}
	}
	return info
}

// renameBranch gives a live branch another name.
//
// The name is how a branch is asked for and nothing else: the memory, the
// processes and the filesystem branch underneath are all reached through the
// record. So the sandbox is told first — it is the thing that can refuse, for a
// name already in use — and the durable record follows. If the record cannot be
// written the sandbox is told to change back, because a family whose two halves
// disagree about a branch's name is worse than one that never renamed it.
func (b *runtimeBackend) renameBranch(target selector, name string) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	if target.branch == name {
		return nil
	}
	for _, branch := range info.Branches {
		if branch.ID == name {
			return fmt.Errorf("branch %q already exists", name)
		}
	}
	// Every console follower is stopped first — the branch's own, and one per
	// exec session in it.
	//
	// A follower is a process launched with the branch it follows named on its
	// command line, so one left running after a rename is following a branch
	// that no longer exists. It holds its log open, so nothing looks wrong: the
	// log is still there, the lock is still held, and the console still counts
	// the bytes written to it — while the program on the other end waits for
	// input that never arrives. Stopped here, the next caller starts one
	// against the new name, and the logs it appends to are the same files.
	store := b.containerStore()
	if paths, err := store.BranchConsolePaths(target.container, target.branch); err == nil {
		if err := b.stopConsoleDrain(paths); err != nil {
			return fmt.Errorf("stopping console follower for %s: %w", target, err)
		}
	}
	if sessions, err := b.execSessions(target); err == nil {
		for _, session := range sessions {
			paths, pathErr := store.ExecConsolePaths(target.container, target.branch, session.ID)
			if pathErr != nil {
				continue
			}
			if err := b.stopConsoleDrain(paths); err != nil {
				return fmt.Errorf("stopping exec console follower %q for %s: %w", session.ID, target, err)
			}
		}
	}
	live := false
	state := ""
	for _, branch := range info.Branches {
		if branch.ID != target.branch {
			continue
		}
		state = normalizeLifecycleState(branch.State)
		switch branch.State {
		case "running", "paused":
			live = true
		}
	}
	// A dehydrated branch may either be a process-free durable image or a cold
	// peer retained inside a Sentry that still runs other branches. Only the
	// latter has an in-memory branch map to rename.
	if state == "dehydrated" {
		output, branchesErr := b.command("timetravel", target.container, "branches")
		if branchesErr == nil {
			var runtimeBranches []runscBranchInfo
			if err := json.Unmarshal(output, &runtimeBranches); err != nil {
				return fmt.Errorf("decoding runtime branches before rename: %w", err)
			}
			for _, branch := range runtimeBranches {
				if branch.ID == target.branch {
					live = true
					break
				}
			}
		} else if !persistentFamilyProcessFree(info) {
			return fmt.Errorf("checking dehydrated branch runtime before rename: %w", branchesErr)
		}
	}
	if live {
		if _, err := b.command("timetravel", target.container, "rename", target.branch, name); err != nil {
			return err
		}
	}
	if b.dataRoot == "" {
		return nil
	}
	if err := b.containerStore().RenameBranch(target.container, target.branch, name); err != nil {
		if live {
			_, _ = b.command("timetravel", target.container, "rename", name, target.branch)
		}
		return err
	}
	return nil
}

func (b *runtimeBackend) prepareStop(target selector) error {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	_, err = b.command("timetravel", resolved.container, "prepare-stop", resolved.branch)
	return err
}

// cloneSparseMemoryRoot gives a newly forked branch an independent durable
// root for any chunks that its source still faults on demand. A branch that
// has never been restored from sparse storage has no root to clone.
func (b *runtimeBackend) cloneSparseMemoryRoot(containerID, sourceFilesystemBranch, targetFilesystemBranch string, lazy bool) error {
	if b.dataRoot == "" || sourceFilesystemBranch == "" || targetFilesystemBranch == "" {
		return nil
	}
	repositoryPath := filepath.Join(b.activeDataRoot(), "containers", containerID, "memory", "chunks")
	if _, err := os.Stat(repositoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	repository, err := ttcchunk.OpenMemoryRepository(repositoryPath)
	if err != nil {
		return err
	}
	defer repository.Close()
	manifest, err := repository.Branch(sourceFilesystemBranch)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if lazy {
		return repository.SetBranchLazy(targetFilesystemBranch, manifest)
	}
	return repository.SetBranch(targetFilesystemBranch, manifest)
}

func graphMemoryRootNames(filesystemBranch string, count int) []string {
	result := make([]string, count)
	for index := range result {
		result[index] = fmt.Sprintf("memory/branch/%s/resource-%04x", filesystemBranch, index)
	}
	return result
}

// cloneGraphMemoryRoots gives a branch O(1)-with-respect-to-memory references
// to the same immutable radix roots as its source. No page or radix node is
// walked or copied.
func (b *runtimeBackend) cloneGraphMemoryRoots(sourceRoots []string, targetFilesystemBranch string) ([]string, error) {
	if len(sourceRoots) == 0 {
		return nil, nil
	}
	if b.snapshotGraph == nil || targetFilesystemBranch == "" {
		return nil, errors.New("snapshot graph is unavailable for memory-root fork")
	}
	destinations := graphMemoryRootNames(targetFilesystemBranch, len(sourceRoots))
	copies := make(map[string]string, len(sourceRoots))
	for index, source := range sourceRoots {
		copies[destinations[index]] = source
	}
	if err := b.snapshotGraph.CloneRoots(copies); err != nil {
		return nil, err
	}
	return destinations, nil
}

// cloneGraphMemoryRootsMany commits all roots for a batch fork in one graph
// transaction, so no sibling can be left partially rooted after a crash.
func (b *runtimeBackend) cloneGraphMemoryRootsMany(sourceRoots []string, branches []ttcbundle.BranchInfo) (map[string][]string, error) {
	result := make(map[string][]string, len(branches))
	if len(sourceRoots) == 0 {
		return result, nil
	}
	if b.snapshotGraph == nil {
		return nil, errors.New("snapshot graph is unavailable for batch memory-root fork")
	}
	copies := make(map[string]string, len(sourceRoots)*len(branches))
	for _, branch := range branches {
		destinations := graphMemoryRootNames(branch.FilesystemBranch, len(sourceRoots))
		result[branch.FilesystemBranch] = destinations
		for index, source := range sourceRoots {
			copies[destinations[index]] = source
		}
	}
	if err := b.snapshotGraph.CloneRoots(copies); err != nil {
		return nil, err
	}
	return result, nil
}

// releaseSparseMemoryRoot removes one branch's durable chunk reachability
// only after its runtime has stopped using the on-demand loader, then reclaims
// objects no surviving branch, lease, or publication transaction references.
func (b *runtimeBackend) releaseSparseMemoryRoot(containerID, filesystemBranch string) error {
	if b.dataRoot == "" || filesystemBranch == "" {
		return nil
	}
	repositoryPath := filepath.Join(b.activeDataRoot(), "containers", containerID, "memory", "chunks")
	if _, err := os.Stat(repositoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	repository, err := ttcchunk.OpenMemoryRepository(repositoryPath)
	if err != nil {
		return err
	}
	defer repository.Close()
	if err := repository.DeleteBranch(filesystemBranch); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = repository.CollectGarbage(ttcchunk.GCOptions{})
	return err
}

// releaseGraphMemoryRoots drops logical branch reachability, cascades exact
// reference counts, and then removes only bulk objects absent from the graph.
func (b *runtimeBackend) releaseGraphMemoryRoots(roots []string) error {
	if len(roots) == 0 || b.snapshotGraph == nil || b.snapshotData == nil {
		return nil
	}
	if err := b.snapshotGraph.ReleaseRoots(roots); err != nil {
		return err
	}
	for {
		collected, err := b.snapshotGraph.Collect(4096)
		if err != nil {
			return err
		}
		if collected == 0 {
			break
		}
	}
	_, _, err := b.snapshotData.ReclaimUnreferenced(b.snapshotGraph)
	return err
}

const snapshotGraphJournalCompactBytes = 8 << 20

// reconcileStandaloneMemoryGraph makes durable branch records the sole root
// directory for standalone memory. It runs only while the daemon's mutation
// lock excludes capture/fork/removal, so every construction pin is necessarily
// crash residue. Cluster graphs require their origin-owned ledger instead.
func (b *runtimeBackend) reconcileStandaloneMemoryGraph() error {
	if b == nil || b.nodeID != "" || b.snapshotGraph == nil || b.snapshotData == nil {
		return nil
	}
	containers, err := b.containerStore().ListContainersForGC()
	if err != nil {
		return fmt.Errorf("listing authoritative memory roots: %w", err)
	}
	liveRoots := make(map[string]struct{})
	retainedSnapshots := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, container := range containers {
		for _, branch := range container.Branches {
			if branch.SnapshotID != 0 {
				retainedSnapshots[ttcsnapshot.SnapshotID(branch.SnapshotID)] = struct{}{}
			}
			for _, name := range branch.MemoryRoots {
				liveRoots[name] = struct{}{}
				if _, ok := b.snapshotGraph.Root(name); !ok {
					return fmt.Errorf("branch %s@%s references missing memory root %q", container.Name, branch.ID, name)
				}
			}
		}
	}
	inspection := b.snapshotGraph.Inspect()
	var orphanRoots []string
	for name := range inspection.Roots {
		if strings.HasPrefix(name, "memory/") {
			if _, live := liveRoots[name]; !live {
				orphanRoots = append(orphanRoots, name)
			}
		}
	}
	sort.Strings(orphanRoots)
	if err := b.snapshotGraph.ReleaseRoots(orphanRoots); err != nil {
		return fmt.Errorf("releasing orphan memory roots: %w", err)
	}
	var orphanPins []string
	for name := range inspection.Pins {
		if strings.HasPrefix(name, "memory/") {
			orphanPins = append(orphanPins, name)
		}
	}
	sort.Strings(orphanPins)
	if err := b.snapshotGraph.DeletePins(orphanPins); err != nil {
		return fmt.Errorf("releasing orphan memory construction pins: %w", err)
	}
	collectedTotal := 0
	for {
		collected, err := b.snapshotGraph.Collect(4096)
		if err != nil {
			return err
		}
		collectedTotal += collected
		if collected == 0 {
			break
		}
	}
	retained := make([]ttcsnapshot.SnapshotID, 0, len(retainedSnapshots))
	for id := range retainedSnapshots {
		retained = append(retained, id)
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i] < retained[j] })
	emptySnapshots, err := b.snapshotGraph.CollectEmptySnapshots(retained)
	if err != nil {
		return err
	}
	reclaimedObjects, _, err := b.snapshotData.ReclaimUnreferenced(b.snapshotGraph)
	if err != nil {
		return err
	}
	changed := len(orphanRoots) != 0 || len(orphanPins) != 0 || collectedTotal != 0 || emptySnapshots != 0 || reclaimedObjects != 0
	if changed || b.snapshotGraph.JournalBytes() >= snapshotGraphJournalCompactBytes {
		if err := b.snapshotGraph.Checkpoint(); err != nil {
			return fmt.Errorf("compacting standalone memory graph: %w", err)
		}
	}
	return b.snapshotGraph.Verify()
}

// discard releases one branch's process/kernel/dehydration state while
// retaining its persistent filesystem, logs, identity, and peer branches.
func (b *runtimeBackend) discard(target selector) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	if err := b.ensureDistributedSourceIndependent(info.ID, target.branch); err != nil {
		return fmt.Errorf("settling remote branches before stopping %s: %w", target, err)
	}
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return err
	}
	store := b.containerStore()
	if paths, pathErr := store.BranchConsolePaths(info.ID, target.branch); pathErr == nil {
		if err := b.stopConsoleDrain(paths); err != nil {
			return fmt.Errorf("stopping console follower for %s: %w", target, err)
		}
		for _, path := range []string{paths.PID} {
			if path != "" {
				_ = os.Remove(path)
			}
		}
		if _, err := os.Stat(paths.WaitStatus); os.IsNotExist(err) {
			status := uint32(linux.WaitStatusTerminationSignal(linux.SIGKILL))
			if err := os.WriteFile(paths.WaitStatus, []byte(strconv.FormatUint(uint64(status), 10)+"\n"), 0600); err != nil {
				return fmt.Errorf("recording discarded wait status for %s: %w", target, err)
			}
		} else if err != nil {
			return fmt.Errorf("checking wait status for %s: %w", target, err)
		}
	}
	if sessions, sessionsErr := b.execSessions(target); sessionsErr == nil {
		for _, session := range sessions {
			if paths, pathErr := store.ExecConsolePaths(info.ID, target.branch, session.ID); pathErr == nil {
				if err := b.stopConsoleDrain(paths); err != nil {
					return fmt.Errorf("stopping exec console follower %q for %s: %w", session.ID, target, err)
				}
			}
		}
	}
	if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
		return err
	}

	runtimeFamilies, listErr := b.listRuntimeFamilies()
	if listErr != nil {
		return fmt.Errorf("checking runtime state while discarding %s: %w", target, listErr)
	}
	for _, family := range runtimeFamilies {
		if family.ID != info.ID {
			continue
		}
		present := false
		for _, branch := range family.Branches {
			if branch.ID == target.branch {
				present = true
				break
			}
		}
		if !present {
			break
		}
		if len(family.Branches) == 1 || runtimeFamilyUnavailable(family) {
			if _, err := b.command("delete", "--force", info.ID); err != nil {
				return fmt.Errorf("releasing final runtime branch %s: %w", target, err)
			}
		} else if _, err := b.command("timetravel", info.ID, "destroy", target.branch); err != nil {
			return fmt.Errorf("discarding live runtime state for %s: %w", target, err)
		}
		break
	}

	// Runtime destruction is the lifecycle commit point. Persist it before
	// reclaiming the dehydration image or sparse roots: cleanup may fail (for
	// example because an NFS server briefly delays an unlink), but a retry must
	// see an exited branch rather than a dehydrated branch whose control image
	// has already been removed.
	if err := store.StopBranch(info.ID, target.branch, 128+int(linux.SIGKILL)); err != nil {
		return err
	}

	for _, kind := range []string{"active", "dehydrated"} {
		path := existingMemoryBranchDir(b.activeDataRoot(), info.ID, kind, branch)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing %s memory for %s: %w", kind, target, err)
		}
	}
	if err := b.releaseSparseMemoryRoot(info.ID, branch.FilesystemBranch); err != nil {
		return fmt.Errorf("reclaiming sparse memory for %s: %w", target, err)
	}
	if err := b.releaseGraphMemoryRoots(branch.MemoryRoots); err != nil {
		return fmt.Errorf("reclaiming graph memory for %s: %w", target, err)
	}
	if len(branch.MemoryRoots) != 0 {
		if err := store.UpdateBranchMemoryGraph(info.ID, target.branch, 0, nil); err != nil {
			return err
		}
	}
	latest, err := store.LoadContainer(info.ID)
	if err != nil {
		return err
	}
	return b.deleteRuntimeIfProcessFree(latest)
}

func persistentFamilyProcessFree(info ttcbundle.ContainerInfo) bool {
	for _, branch := range info.Branches {
		switch normalizeLifecycleState(branch.State) {
		case "created", "dehydrated", "exited", "failed", "dead":
		default:
			return false
		}
	}
	return true
}

func (b *runtimeBackend) deleteRuntimeIfProcessFree(info ttcbundle.ContainerInfo) error {
	if !persistentFamilyProcessFree(info) {
		return nil
	}
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("checking process-free runtime %q: %w", info.Name, err)
	}
	for _, family := range families {
		if family.ID != info.ID {
			continue
		}
		if _, err := b.command("delete", "--force", info.ID); err != nil {
			return fmt.Errorf("deleting process-free runtime %q: %w", info.Name, err)
		}
		break
	}
	return nil
}

func (b *runtimeBackend) dehydrate(target selector) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	persistentBranch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return err
	}
	if err := b.ensureDistributedSourceIndependent(info.ID, target.branch); err != nil {
		return fmt.Errorf("settling remote branches before dehydrating %s: %w", target, err)
	}
	if err := b.ensureDistributedTargetMemoryIndependent(info, persistentBranch); err != nil {
		return fmt.Errorf("materializing remote memory before dehydrating %s: %w", target, err)
	}
	store := b.containerStore()
	initConsolePaths, _ := store.BranchConsolePaths(info.ID, target.branch)
	execSessions, _ := b.execSessions(target)
	dehydrated := false
	originalState := normalizeLifecycleState(persistentBranch.State)
	transitionPublished := false
	captured := false
	var stagingDir, finalDir string
	finalPublished := false
	sparseRootChanged := false
	hadPreviousSparseRoot := false
	var previousSparseRoot ttcchunk.ManifestID
	var sparseRepositoryPath string
	var sparseFilesystemBranch string
	var memoryRepository *ttcchunk.MemoryRepository
	var graphMemoryRoots []string
	var graphMemorySnapshot uint64
	graphMemoryRecorded := false
	previousGraphMemorySnapshot := persistentBranch.SnapshotID
	previousGraphMemoryRoots := append([]string(nil), persistentBranch.MemoryRoots...)
	defer func() {
		if dehydrated {
			return
		}
		if transitionPublished {
			_ = b.containerStore().UpdateBranchState(info.ID, target.branch, originalState)
		}
		if sparseRootChanged {
			if repository, openErr := ttcchunk.OpenMemoryRepository(sparseRepositoryPath); openErr == nil {
				if hadPreviousSparseRoot {
					openErr = repository.SetBranch(sparseFilesystemBranch, previousSparseRoot)
				} else {
					openErr = repository.DeleteBranch(sparseFilesystemBranch)
					if errors.Is(openErr, os.ErrNotExist) {
						openErr = nil
					}
				}
				if openErr == nil {
					_, openErr = repository.CollectGarbage(ttcchunk.GCOptions{})
				}
				_ = repository.Close()
				if openErr != nil && b.stderr != nil {
					fmt.Fprintf(b.stderr, "ttc: warning: rolling back sparse memory root for %s: %v\n", target, openErr)
				}
			} else if b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: opening sparse memory repository to roll back %s: %v\n", target, openErr)
			}
		}
		if graphMemoryRecorded {
			_ = b.containerStore().UpdateBranchMemoryGraph(info.ID, target.branch, previousGraphMemorySnapshot, previousGraphMemoryRoots)
		}
		if len(graphMemoryRoots) != 0 {
			if graphErr := b.releaseGraphMemoryRoots(graphMemoryRoots); graphErr != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: rolling back graph memory roots for %s: %v\n", target, graphErr)
			}
		}
		if finalPublished {
			_ = os.RemoveAll(finalDir)
		}
		if captured {
			// Dehydration has already replaced the live Kernel. If durable export
			// fails, reconstruct it while the Sentry still owns the capture so the
			// operation remains atomic from the user's point of view.
			_, _ = b.command("timetravel", target.container, "rehydrate", target.branch)
		}
		// Capture failure leaves the branch in its original running or paused
		// state. Restore every sidecar stopped in preparation for the attempt.
		_, _ = b.ensureConsoleDrain(target, false)
		for _, session := range execSessions {
			if session.State == "running" {
				_, _ = b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
			}
		}
		_ = b.ensurePublishedPortsForBranch(info, target.branch)
	}()
	if err := b.stopConsoleDrain(initConsolePaths); err != nil {
		return fmt.Errorf("stopping init console follower before dehydration: %w", err)
	}
	for _, session := range execSessions {
		if paths, pathErr := store.ExecConsolePaths(info.ID, target.branch, session.ID); pathErr == nil {
			if err := b.stopConsoleDrain(paths); err != nil {
				return fmt.Errorf("stopping exec console follower %q before dehydration: %w", session.ID, err)
			}
		}
	}
	if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
		return fmt.Errorf("stopping published ports before dehydration: %w", err)
	}
	var metadataFile *os.File
	if b.dataRoot != "" {
		metadataDir := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "dehydrated")
		if err := os.MkdirAll(metadataDir, 0700); err != nil {
			return fmt.Errorf("creating dehydration metadata directory: %w", err)
		}
		encodedBranch := ttcbundle.MemoryBranchKey(persistentBranch)
		finalDir = filepath.Join(metadataDir, encodedBranch)
		stagingDir, err = os.MkdirTemp(metadataDir, "."+encodedBranch+"-*")
		if err != nil {
			return fmt.Errorf("creating dehydration staging directory: %w", err)
		}
		defer os.RemoveAll(stagingDir)
		metadataFile, err = os.OpenFile(filepath.Join(stagingDir, "metadata.state"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return fmt.Errorf("creating dehydration metadata: %w", err)
		}
		defer metadataFile.Close()
	}
	var output []byte
	if b.dataRoot != "" {
		if err := b.containerStore().UpdateBranchState(info.ID, target.branch, "dehydrating"); err != nil {
			return fmt.Errorf("publishing dehydration transition for %s: %w", target, err)
		}
		transitionPublished = true
	}
	if metadataFile != nil {
		output, err = b.commandWithFiles([]*os.File{metadataFile}, "timetravel", "--dehydration-fd=3", target.container, "dehydrate", target.branch)
	} else {
		output, err = b.command("timetravel", target.container, "dehydrate", target.branch)
	}
	if err != nil {
		return err
	}
	captured = true
	if metadataFile != nil {
		var capture boot.BranchDehydrateInfo
		if err := json.Unmarshal(output, &capture); err != nil {
			return fmt.Errorf("decoding dehydration capture description: %w", err)
		}
		if b.snapshotGraph != nil && b.snapshotData != nil && b.nodeID == "" {
			origin, err := b.snapshotGraph.AllocateWorking()
			if err != nil {
				return fmt.Errorf("allocating dehydration snapshot origin: %w", err)
			}
			prefix := fmt.Sprintf("memory/%016x", uint64(origin))
			reader, writer, err := os.Pipe()
			if err != nil {
				return fmt.Errorf("creating graph dehydration stream: %w", err)
			}
			type graphImportOutcome struct {
				result ttcchunk.GraphImportResult
				err    error
			}
			imported := make(chan graphImportOutcome, 1)
			go func() {
				result, importErr := ttcchunk.ImportGraphCapture(reader, b.snapshotGraph, b.snapshotData, origin, prefix)
				_ = reader.Close()
				imported <- graphImportOutcome{result: result, err: importErr}
			}()
			_, exportErr := b.commandWithFiles([]*os.File{writer}, "timetravel", "--dehydration-graph-stream-fd=3", target.container, "dehydrate-export", target.branch)
			_ = writer.Close()
			outcome := <-imported
			if exportErr != nil {
				return fmt.Errorf("streaming graph-native dehydration memory: %w", exportErr)
			}
			if outcome.err != nil {
				return fmt.Errorf("importing graph-native dehydration memory: %w", outcome.err)
			}
			for _, root := range outcome.result.Roots {
				capture.MemoryRoots = append(capture.MemoryRoots, boot.BranchMemoryRoot{
					ResourceID: root.ResourceID,
					Name:       root.Name,
					Root:       root.Root.String(),
				})
				graphMemoryRoots = append(graphMemoryRoots, root.Name)
			}
			if err := b.snapshotGraph.Seal(origin, ttcsnapshot.SnapshotSealedLocal); err != nil {
				return fmt.Errorf("sealing dehydration snapshot origin %d: %w", origin, err)
			}
			capture.SnapshotID = uint64(origin)
			graphMemorySnapshot = uint64(origin)
			capture.SparseManifest = ttcchunk.ManifestID{}
		} else {
			sparseRepositoryPath = filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks")
			memoryRepository, err = ttcchunk.OpenMemoryRepository(sparseRepositoryPath)
			if err != nil {
				return fmt.Errorf("opening sparse dehydration memory repository: %w", err)
			}
			defer memoryRepository.Close()
			previousSparseRoot, err = memoryRepository.Branch(capture.FilesystemBranch)
			if err == nil {
				hadPreviousSparseRoot = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("reading previous sparse dehydration branch root: %w", err)
			}
			repositoryDir, err := os.Open(sparseRepositoryPath)
			if err != nil {
				return err
			}
			exportOutput, exportErr := b.commandWithFiles([]*os.File{repositoryDir}, "timetravel", "--dehydration-memory-repository-fd=3", target.container, "dehydrate-export", target.branch)
			_ = repositoryDir.Close()
			if exportErr != nil {
				return fmt.Errorf("persisting dehydration memory: %w", exportErr)
			}
			var exportInfo boot.BranchDehydrationExportInfo
			if err := json.Unmarshal(exportOutput, &exportInfo); err != nil {
				return fmt.Errorf("decoding sparse dehydration root: %w", err)
			}
			if exportInfo.Manifest == (ttcchunk.ManifestID{}) {
				return errors.New("sparse dehydration export returned an empty manifest ID")
			}
			sparseFilesystemBranch = capture.FilesystemBranch
			if err := memoryRepository.SetBranch(capture.FilesystemBranch, exportInfo.Manifest); err != nil {
				return fmt.Errorf("publishing sparse dehydration branch root: %w", err)
			}
			sparseRootChanged = true
			capture.SparseManifest = exportInfo.Manifest
		}
		if err := writeDurableJSON(filepath.Join(stagingDir, "control.json"), &capture); err != nil {
			return fmt.Errorf("persisting dehydration control state: %w", err)
		}
		if err := metadataFile.Sync(); err != nil {
			return fmt.Errorf("syncing dehydration metadata: %w", err)
		}
		if err := metadataFile.Close(); err != nil {
			return fmt.Errorf("closing dehydration metadata: %w", err)
		}
		metadataFile = nil
		if _, err := os.Stat(finalDir); err == nil {
			return fmt.Errorf("dehydration image already exists for branch %q", target.branch)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(stagingDir, finalDir); err != nil {
			return fmt.Errorf("publishing dehydration image: %w", err)
		}
		stagingDir = ""
		finalPublished = true
		if dir, err := os.Open(filepath.Dir(finalDir)); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
		if graphMemorySnapshot != 0 {
			if err := b.containerStore().UpdateBranchMemoryGraph(info.ID, target.branch, graphMemorySnapshot, graphMemoryRoots); err != nil {
				return fmt.Errorf("recording graph-native dehydration roots: %w", err)
			}
			graphMemoryRecorded = true
		}
		// Remote dependencies remain live until the asynchronous shared
		// publication commits. The publisher advances the transaction only after
		// its manifest is authoritative.
	}
	// The old console followers observe the intentionally released Kernel as an
	// exit and record SIGKILL. That is not a container exit: discard those
	// transient statuses so rehydration can attach fresh followers.
	for _, path := range []string{initConsolePaths.WaitStatus, initConsolePaths.PID} {
		if path != "" {
			_ = os.Remove(path)
		}
	}
	for _, session := range execSessions {
		if paths, pathErr := store.ExecConsolePaths(info.ID, target.branch, session.ID); pathErr == nil {
			_ = os.Remove(paths.WaitStatus)
			_ = os.Remove(paths.PID)
		}
	}
	if err := b.queueBranchPublication(target, "dehydrate"); err != nil {
		return fmt.Errorf("queueing durable dehydration publication: %w", err)
	}
	if graphMemorySnapshot != 0 && memoryRepository != nil {
		if err := memoryRepository.DeleteBranch(sparseFilesystemBranch); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("releasing compatibility dehydration root: %w", err)
		}
		if _, err := memoryRepository.CollectGarbage(ttcchunk.GCOptions{}); err != nil {
			return fmt.Errorf("reclaiming compatibility dehydration objects: %w", err)
		}
		if err := os.RemoveAll(sparseRepositoryPath); err != nil {
			return fmt.Errorf("removing compatibility dehydration repository: %w", err)
		}
		sparseRootChanged = false
	}
	dehydrated = true
	graphMemoryRoots = nil
	if graphMemorySnapshot != 0 && len(previousGraphMemoryRoots) != 0 {
		if err := b.releaseGraphMemoryRoots(previousGraphMemoryRoots); err != nil && b.stderr != nil {
			// The new roots are already authoritative. Retaining the superseded
			// roots is a safe leak that standalone graph reconciliation retries;
			// it must not turn a successful dehydration into an ambiguous failure.
			fmt.Fprintf(b.stderr, "ttc: warning: reclaiming superseded memory roots for %s: %v\n", target, err)
		}
	}
	if b.dataRoot != "" {
		activeDir := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "active", persistentBranch)
		if removeErr := os.RemoveAll(activeDir); removeErr != nil {
			return fmt.Errorf("removing superseded active memory: %w", removeErr)
		}
	}
	if b.dataRoot != "" {
		latest, loadErr := b.containerStore().LoadContainer(target.container)
		if loadErr != nil {
			return loadErr
		}
		allCold, localBranches := allBranchesColdOnNode(latest, b.nodeID)
		if allCold && localBranches != 0 {
			if _, deleteErr := b.command("delete", "--force", info.ID); deleteErr != nil {
				return fmt.Errorf("releasing fully dehydrated runtime: %w", deleteErr)
			}
		}
	}
	return nil
}

// ensureDistributedTargetMemoryIndependent drains a live target's original
// peer-backed memory and filesystem readers before capturing a process-free
// image. A sparse dehydration export can read outstanding memory through its
// loader, but filesystem object holes have no process-free network reader.
// Declaring the transaction independent while either remained would turn a
// later source loss into EPIPE from an otherwise healthy rehydrated branch.
func (b *runtimeBackend) ensureDistributedTargetMemoryIndependent(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	transactionID := transactionIDForBranch(info, branch)
	if transactionID == "" || (branch.DependentMemoryBytes == 0 && branch.DependentFilesystemBytes == 0) {
		return nil
	}
	readStatus := func() (pgalloc.AsyncPagesFileLoadStats, error) {
		output, err := b.command("timetravel", info.ID, "distributed-dependency", branch.ID)
		if err != nil {
			return pgalloc.AsyncPagesFileLoadStats{}, err
		}
		var status pgalloc.AsyncPagesFileLoadStats
		if err := json.Unmarshal(output, &status); err != nil {
			return pgalloc.AsyncPagesFileLoadStats{}, fmt.Errorf("decoding remote-memory dependency: %w", err)
		}
		return status, nil
	}
	action := "distributed-filesystem-drain"
	if branch.DependentMemoryBytes != 0 {
		action = "distributed-drain"
	}
	if _, err := b.command("timetravel", info.ID, action, branch.ID); err != nil {
		return err
	}
	if branch.DependentMemoryBytes != 0 {
		deadline := time.Now().Add(5 * time.Minute)
		for {
			status, err := readStatus()
			if err != nil {
				return err
			}
			if status.Done {
				if status.Error != "" {
					return errors.New(status.Error)
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out with %d remote bytes outstanding", status.BytesOutstanding)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := b.containerStore().UpdateDistributedForkDependencies(transactionID, 0, 0); err != nil {
		return err
	}
	transactions, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	tx, err := transactions.Load(transactionID)
	if err != nil {
		return err
	}
	switch tx.State {
	case cluster.TransactionPublished, cluster.TransactionDraining:
		_, err = transactions.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now())
		return err
	case cluster.TransactionIndependent:
		return nil
	case cluster.TransactionCanceled, cluster.TransactionFailed:
		return fmt.Errorf("distributed fork transaction %q is %s: %s", transactionID, tx.State, tx.Failure)
	default:
		return fmt.Errorf("distributed fork transaction %q is unexpectedly %s", transactionID, tx.State)
	}
}

func (b *runtimeBackend) completeDistributedTargetDehydration(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	transactionID := transactionIDForBranch(info, branch)
	if transactionID == "" || b.dataRoot == "" {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	tx, err := store.Load(transactionID)
	if err != nil {
		return err
	}
	switch tx.State {
	case cluster.TransactionPublished, cluster.TransactionDraining:
		if _, err := store.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now()); err != nil {
			return err
		}
	case cluster.TransactionIndependent:
		// Idempotent retry after the durable image was already published.
	case cluster.TransactionCanceled, cluster.TransactionFailed:
		return fmt.Errorf("distributed fork transaction %q is %s: %s", transactionID, tx.State, tx.Failure)
	default:
		return fmt.Errorf("distributed fork transaction %q is unexpectedly %s", transactionID, tx.State)
	}
	return nil
}

func transactionIDForBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) string {
	if branch.DistributedForkTransactionID != "" {
		return branch.DistributedForkTransactionID
	}
	return info.DistributedForkTransactionID
}

func (b *runtimeBackend) rehydrate(target selector) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	var persistentBranch *ttcbundle.BranchInfo
	for i := range info.Branches {
		if info.Branches[i].ID == target.branch {
			persistentBranch = &info.Branches[i]
			break
		}
	}
	if persistentBranch == nil {
		return fmt.Errorf("branch %q is not recorded", target.branch)
	}
	if persistentBranch.State == "dehydrating" {
		if b.publicationQueue == nil || persistentBranch.StorageGeneration == 0 {
			return fmt.Errorf("branch %s has not completed its local dehydration capture", target)
		}
		generation := persistentBranch.StorageGeneration
		if err := b.containerStore().AbandonBranchPublication(info.ID, target.branch, generation); err != nil {
			return fmt.Errorf("fencing dehydration publication for %s: %w", target, err)
		}
		if err := b.publicationQueue.Cancel(info.ID, target.branch, generation); err != nil {
			return fmt.Errorf("canceling dehydration publication for %s: %w", target, err)
		}
		// AbandonBranchPublication advances the generation fence. The cold
		// restore must carry that new generation into its final storage update;
		// using the BranchInfo loaded before cancellation would successfully
		// restore the runtime and then report a stale-worker failure.
		info, err = b.containerStore().LoadContainer(info.ID)
		if err != nil {
			return fmt.Errorf("reloading canceled dehydration for %s: %w", target, err)
		}
		persistentBranch = nil
		for i := range info.Branches {
			if info.Branches[i].ID == target.branch {
				persistentBranch = &info.Branches[i]
				break
			}
		}
		if persistentBranch == nil {
			return fmt.Errorf("branch %q disappeared while canceling dehydration", target.branch)
		}
		persistentBranch.State = "dehydrated"
	}
	if persistentBranch.State == "dehydrated" {
		if _, stateErr := b.command("state", info.ID); stateErr != nil {
			return b.rehydrateCold(target, info, *persistentBranch, false)
		}
		output, branchesErr := b.command("timetravel", info.ID, "branches")
		if branchesErr != nil {
			// A fully dehydrated family has no live Sentry. runsc may briefly
			// retain a stopped OCI record after the last branch releases it;
			// state then succeeds even though its control server is gone. Remove
			// that stale shell and rebuild directly from the durable image.
			allCold := true
			for _, branch := range info.Branches {
				if branch.State != "dehydrated" && branch.State != "exited" && branch.State != "stopped" {
					allCold = false
					break
				}
			}
			if !allCold {
				return branchesErr
			}
			_, _ = b.command("delete", "--force", info.ID)
			return b.rehydrateCold(target, info, *persistentBranch, false)
		}
		var runtimeBranches []runscBranchInfo
		if err := json.Unmarshal(output, &runtimeBranches); err != nil {
			return fmt.Errorf("decoding runtime branches: %w", err)
		}
		present := false
		for _, runtimeBranch := range runtimeBranches {
			if runtimeBranch.ID == target.branch {
				present = true
				break
			}
		}
		if !present {
			return b.rehydrateCold(target, info, *persistentBranch, true)
		}
		// The durable record is authoritative across daemon crashes. Reconcile
		// the process-free runtime record before it recreates the Kernel.
		action := "unquarantine"
		if persistentBranch.Quarantined {
			action = "quarantine"
		}
		if _, err := b.command("timetravel", info.ID, action, target.branch); err != nil {
			return fmt.Errorf("reconciling dehydration-time quarantine for %s: %w", target, err)
		}
	}
	_, err = b.command("timetravel", target.container, "rehydrate", target.branch)
	if err != nil || b.dataRoot == "" {
		return err
	}
	families, err := b.listRuntimeFamilies()
	if err != nil {
		return err
	}
	for _, family := range families {
		if family.ID != target.container {
			continue
		}
		for _, branch := range family.Branches {
			if branch.ID == target.branch {
				if err := b.containerStore().UpdateBranchState(target.container, target.branch, branch.State); err != nil {
					return err
				}
				b.retainRehydrationPageSource(info.ID, *persistentBranch, "")
				if _, err := b.ensureConsoleDrain(target, false); err != nil {
					return fmt.Errorf("reattaching init console: %w", err)
				}
				sessions, err := b.execSessions(target)
				if err != nil {
					return fmt.Errorf("listing rehydrated exec sessions: %w", err)
				}
				for _, session := range sessions {
					if session.State == "running" {
						if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false); err != nil {
							return fmt.Errorf("reattaching exec console %q: %w", session.ID, err)
						}
					}
				}
				if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
					return fmt.Errorf("restoring published ports: %w", err)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("rehydrated branch %s disappeared from runtime state", target)
}

// retainRehydrationPageSource finalizes a consumed dehydration image after the
// restored branch is committed. Sparse restores read immutable objects from
// the family repository, so their per-branch control directory is removed.
// Legacy packed restores retain their snapshot files under the active branch
// because the on-demand readers may still have those files open.
//
// Failure is a post-commit cleanup warning and must never roll back a live
// branch or change its persistent state.
func (b *runtimeBackend) retainRehydrationPageSource(containerID string, branch ttcbundle.BranchInfo, sourceDir string) {
	branchID := branch.ID
	if sourceDir == "" {
		sourceDir = existingMemoryBranchDir(b.activeDataRoot(), containerID, "dehydrated", branch)
	}
	if _, err := os.Lstat(sourceDir); os.IsNotExist(err) {
		return
	} else if err != nil {
		if b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: checking consumed dehydration image for %s@%s: %v\n", containerID, branchID, err)
		}
		return
	}
	if control, err := os.ReadFile(filepath.Join(sourceDir, "control.json")); err == nil {
		var capture boot.BranchDehydrateInfo
		if json.Unmarshal(control, &capture) == nil && (capture.SparseManifest != (ttcchunk.ManifestID{}) || len(capture.MemoryRoots) != 0) {
			if err := os.RemoveAll(sourceDir); err != nil {
				if b.stderr != nil {
					fmt.Fprintf(b.stderr, "ttc: warning: removing consumed sparse dehydration image for %s@%s: %v\n", containerID, branchID, err)
				}
				return
			}
			if dir, err := os.Open(filepath.Dir(sourceDir)); err == nil {
				_ = dir.Sync()
				_ = dir.Close()
			}
			return
		}
	}
	activeDir := memoryBranchDir(b.activeDataRoot(), containerID, "active", branch)
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		if b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: retaining dehydration page source for %s@%s: %v\n", containerID, branchID, err)
		}
		return
	}
	destination := filepath.Join(activeDir, "dehydration-source")
	if err := os.Rename(sourceDir, destination); err != nil {
		if b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: retaining dehydration page source for %s@%s: %v\n", containerID, branchID, err)
		}
		return
	}
	for _, directory := range []string{filepath.Dir(sourceDir), activeDir} {
		if dir, err := os.Open(directory); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
}

func (b *runtimeBackend) rehydrateCold(target selector, info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, peer bool) (retErr error) {
	refreshed, err := b.containerStore().RefreshContainerImageRoot(context.Background(), info.ID)
	if err != nil {
		return fmt.Errorf("materializing immutable image cache: %w", err)
	}
	info = refreshed
	// RefreshContainerImageRoot reloads the authoritative container record.
	// Do not keep using the BranchInfo snapshot supplied by a merged runtime
	// view: quarantine may have changed while this branch was process-free.
	foundBranch := false
	for _, current := range info.Branches {
		if current.ID == branch.ID {
			branch = current
			foundBranch = true
			break
		}
	}
	if !foundBranch {
		return fmt.Errorf("branch %q disappeared before cold rehydration", branch.ID)
	}
	localImageBefore := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
	_, localImageErr := os.Stat(localImageBefore)
	needsColdMaterialization := errors.Is(localImageErr, os.ErrNotExist)
	if localImageErr != nil && !needsColdMaterialization {
		return localImageErr
	}
	if err := b.ensureLocalColdBranch(info, branch); err != nil {
		return fmt.Errorf("materializing cold branch: %w", err)
	}
	imageDir := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
	controlData, err := os.ReadFile(filepath.Join(imageDir, "control.json"))
	if err != nil {
		return fmt.Errorf("reading dehydration control state: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return fmt.Errorf("decoding dehydration control state: %w", err)
	}
	if capture.FilesystemBranch == "" || len(capture.Memories) == 0 {
		return errors.New("dehydration control state is incomplete")
	}
	graphSparse := b.nodeID == "" && len(capture.MemoryRoots) != 0
	sparse := capture.SparseManifest != (ttcchunk.ManifestID{}) || graphSparse
	metadata, err := os.Open(filepath.Join(imageDir, "metadata.state"))
	if err != nil {
		return fmt.Errorf("opening dehydration metadata: %w", err)
	}
	defer metadata.Close()
	snapshots := make([]*os.File, 0, len(capture.Memories))
	defer func() {
		for _, file := range snapshots {
			_ = file.Close()
		}
	}()
	for i := range capture.Memories {
		if sparse {
			break
		}
		file, err := os.Open(filepath.Join(imageDir, fmt.Sprintf("memory-%04d.snapshot", i)))
		if err != nil {
			return fmt.Errorf("opening dehydration memory image %d: %w", i, err)
		}
		if stat, err := file.Stat(); err != nil {
			file.Close()
			return err
		} else if uint64(stat.Size()) != capture.Memories[i].StreamBytes {
			file.Close()
			return fmt.Errorf("dehydration memory image %d is %d bytes, want %d", i, stat.Size(), capture.Memories[i].StreamBytes)
		}
		snapshots = append(snapshots, file)
	}
	if peer {
		return b.rehydrateColdPeer(target, info, branch, capture, imageDir, metadata, snapshots, needsColdMaterialization)
	}
	var relays *coldFilesystemRelays
	if needsColdMaterialization {
		relays, err = b.prepareColdFilesystemRelays(info, branch)
		if err != nil {
			return fmt.Errorf("preparing lazy cold filesystem: %w", err)
		}
	}
	if relays != nil {
		defer relays.close()
	}
	bundleDir, cleanupBundle, err := b.containerStore().PrepareDehydrationRestoreBundle(info.ID, capture.FilesystemBranch)
	if err != nil {
		return fmt.Errorf("preparing dehydration restore bundle: %w", err)
	}
	defer cleanupBundle()
	configured := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	status, err := b.runConfiguredCommandWithArgs(configured, "create", "--bundle", bundleDir, info.ID)
	if err != nil {
		return fmt.Errorf("creating dehydration runtime: %w", err)
	}
	if status != 0 {
		return fmt.Errorf("creating dehydration runtime exited with status %d", status)
	}
	runtimeCreated := true
	defer func() {
		if retErr != nil && runtimeCreated {
			_, _ = b.command("delete", "--force", info.ID)
		}
	}()
	restoreArgs := boot.DurableDehydrationRestoreArgs{
		ID: target.branch, ParentID: branch.ParentID, FilesystemBranch: capture.FilesystemBranch,
		Resume: capture.Resume, ExecSessions: capture.ExecSessions, Sparse: sparse, GraphSparse: graphSparse, Quarantined: branch.Quarantined,
	}
	restoreArgs.FilesystemRestoreIDs, err = b.filesystemRestoreIDs(info.ID)
	if err != nil {
		return err
	}
	if relays != nil {
		restoreArgs.FilesystemConnections = len(relays.clients)
		restoreArgs.FilesystemToken = relays.token
		if relays.memoryClient != nil {
			restoreArgs.SparseRemote = true
			restoreArgs.SparseRemoteToken = relays.memoryToken
		}
	}
	restoreArgs.NetworkArgs, err = dehydrationNetworkArgs(info)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(&restoreArgs)
	if err != nil {
		return err
	}
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	if _, err := configWriter.Write(encoded); err != nil {
		configReader.Close()
		configWriter.Close()
		return err
	}
	if err := configWriter.Close(); err != nil {
		configReader.Close()
		return err
	}
	files := []*os.File{configReader, metadata}
	commandArgs := []string{"timetravel", "--dehydration-config-fd=3", "--dehydration-fd=4"}
	if sparse {
		if graphSparse {
			manifest, objects, graphErr := b.openGraphMemoryRestore(capture)
			if graphErr != nil {
				configReader.Close()
				return graphErr
			}
			defer manifest.Close()
			defer objects.Close()
			manifestFD := 3 + len(files)
			files = append(files, manifest)
			objectsFD := 3 + len(files)
			files = append(files, objects)
			commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-graph-manifest-fd=%d", manifestFD), fmt.Sprintf("--dehydration-memory-repository-fd=%d", objectsFD))
		} else {
			repositoryPath := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks")
			repository, err := os.Open(repositoryPath)
			if err != nil {
				configReader.Close()
				return fmt.Errorf("opening sparse dehydration repository: %w", err)
			}
			defer repository.Close()
			files = append(files, repository)
			commandArgs = append(commandArgs, "--dehydration-memory-repository-fd=5")
			if relays != nil && relays.memoryClient != nil {
				memoryRemoteFD := 3 + len(files)
				files = append(files, relays.memoryClient)
				commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-memory-remote-fd=%d", memoryRemoteFD))
			}
		}
	} else {
		files = append(files, snapshots...)
		commandArgs = append(commandArgs, "--dehydration-snapshots-start-fd=5", fmt.Sprintf("--dehydration-snapshots-count=%d", len(snapshots)))
	}
	if relays != nil {
		filesystemStartFD := 3 + len(files)
		files = append(files, relays.clients...)
		commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-filesystems-start-fd=%d", filesystemStartFD), fmt.Sprintf("--dehydration-filesystems-count=%d", len(relays.clients)))
	}
	commandArgs = append(commandArgs, info.ID, "dehydrate-restore", target.branch)
	_, err = b.commandWithFiles(files, commandArgs...)
	configReader.Close()
	if err != nil {
		return fmt.Errorf("installing dehydration image: %w", err)
	}
	// commandWithFiles has transferred the metadata to the Sentry. Close our
	// source descriptor before sparse cleanup; otherwise NFS must silly-rename
	// metadata.state and RemoveAll observes a transient non-empty directory.
	_ = metadata.Close()
	status, err = b.runConfiguredCommandWithArgs(configured, "start", info.ID)
	if err != nil {
		return fmt.Errorf("starting dehydration runtime: %w", err)
	}
	if status != 0 {
		return fmt.Errorf("starting dehydration runtime exited with status %d", status)
	}
	if err := b.containerStore().CommitActiveRepositoryRestoreIDs(info.ID); err != nil {
		return fmt.Errorf("committing rehydrated filesystem identities: %w", err)
	}
	state := "paused"
	if capture.Resume {
		if _, err := b.command("timetravel", info.ID, "resume", target.branch); err != nil {
			return fmt.Errorf("resuming rehydrated branch: %w", err)
		}
		state = "running"
	}
	if err := b.containerStore().UpdateBranchState(info.ID, target.branch, state); err != nil {
		return err
	}
	if relays == nil {
		if err := b.containerStore().UpdateBranchStorage(info.ID, target.branch, branch.StorageGeneration, "local", "", time.Time{}); err != nil {
			return err
		}
	}
	// The live runtime and its persistent state are now committed. Failures in
	// sidecar attachment or page-source bookkeeping must not delete it.
	runtimeCreated = false
	b.retainRehydrationPageSource(info.ID, branch, imageDir)
	if _, err := b.ensureConsoleDrain(target, false); err != nil {
		return fmt.Errorf("reattaching init console: %w", err)
	}
	sessions, err := b.execSessions(target)
	if err != nil {
		return fmt.Errorf("listing rehydrated exec sessions: %w", err)
	}
	for _, session := range sessions {
		if session.State == "running" {
			if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false); err != nil {
				return fmt.Errorf("reattaching exec console %q: %w", session.ID, err)
			}
		}
	}
	if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
		return fmt.Errorf("restoring published ports: %w", err)
	}
	return nil
}

func (b *runtimeBackend) openGraphMemoryRestore(capture boot.BranchDehydrateInfo) (*os.File, *os.File, error) {
	if b.snapshotGraph == nil || len(capture.MemoryRoots) == 0 {
		return nil, nil, errors.New("graph-native dehydration roots are unavailable")
	}
	roots := make([]ttcchunk.GraphMemoryRoot, len(capture.MemoryRoots))
	for index, recorded := range capture.MemoryRoots {
		id, err := ttcsnapshot.ParseObjectID(recorded.Root)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing graph memory root %q: %w", recorded.Name, err)
		}
		current, ok := b.snapshotGraph.Root(recorded.Name)
		if !ok || current != id {
			return nil, nil, fmt.Errorf("graph memory root %q is no longer authoritative", recorded.Name)
		}
		roots[index] = ttcchunk.GraphMemoryRoot{ResourceID: recorded.ResourceID, Name: recorded.Name, Root: id}
	}
	adapted, err := ttcchunk.BuildGraphManifest(b.snapshotGraph, roots)
	if err != nil {
		return nil, nil, err
	}
	encoded, _, err := ttcchunk.EncodeManifest(adapted.Manifest)
	if err != nil {
		return nil, nil, err
	}
	manifestFD, err := memutil.CreateMemFD("ttc-graph-memory-manifest", 0)
	if err != nil {
		return nil, nil, err
	}
	manifest := os.NewFile(uintptr(manifestFD), "ttc-graph-memory-manifest")
	if _, err := manifest.Write(encoded); err != nil {
		manifest.Close()
		return nil, nil, err
	}
	if _, err := manifest.Seek(0, io.SeekStart); err != nil {
		manifest.Close()
		return nil, nil, err
	}
	objects, err := os.Open(filepath.Join(b.activeDataRoot(), "objects"))
	if err != nil {
		manifest.Close()
		return nil, nil, err
	}
	return manifest, objects, nil
}

func (b *runtimeBackend) rehydrateColdPeer(target selector, info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, capture boot.BranchDehydrateInfo, sourceDir string, metadata *os.File, snapshots []*os.File, needsColdMaterialization bool) error {
	var relays *coldFilesystemRelays
	var err error
	if needsColdMaterialization {
		relays, err = b.prepareColdFilesystemRelays(info, branch)
		if err != nil {
			return fmt.Errorf("preparing lazy cold peer filesystem: %w", err)
		}
	}
	if relays != nil {
		defer relays.close()
	}
	graphSparse := b.nodeID == "" && len(capture.MemoryRoots) != 0
	sparse := capture.SparseManifest != (ttcchunk.ManifestID{}) || graphSparse
	targets := make([]*os.File, 0, len(capture.Memories))
	defer func() {
		for _, file := range targets {
			_ = file.Close()
		}
	}()
	firstTarget := 0
	if sparse {
		// Sparse peer restores install application chunks into the family's
		// existing MemoryFile. Only private filesystem MemoryFiles need new
		// donated backing files.
		firstTarget = 1
	}
	for i := firstTarget; i < len(capture.Memories); i++ {
		fd, err := memutil.CreateMemFD(fmt.Sprintf("ttc-rehydration-memory-%04d", i), 0)
		if err != nil {
			return fmt.Errorf("creating anonymous active dehydration memory %d: %w", i, err)
		}
		file := os.NewFile(uintptr(fd), fmt.Sprintf("ttc-rehydration-memory-%04d", i))
		// Peer restore constructs its MemoryFiles after systrap has reserved the
		// Sentry's application address range. Perform the executable mmap used
		// to prime host IMA here, before donating the file to the sandbox.
		pgalloc.IMAWorkAroundForMemFile(file.Fd())
		targets = append(targets, file)
	}
	restoreArgs := boot.DurableDehydrationRestoreArgs{
		ID: target.branch, ParentID: branch.ParentID, FilesystemBranch: capture.FilesystemBranch,
		Resume: capture.Resume, ExecSessions: capture.ExecSessions, UseDonatedMemoryFiles: true, Sparse: sparse, GraphSparse: graphSparse,
		ReusePeerApplicationMemoryFile: sparse, Quarantined: branch.Quarantined,
	}
	restoreArgs.FilesystemRestoreIDs, err = b.filesystemRestoreIDs(info.ID)
	if err != nil {
		return err
	}
	if relays != nil {
		restoreArgs.FilesystemConnections = len(relays.clients)
		restoreArgs.FilesystemToken = relays.token
		if relays.memoryClient != nil {
			restoreArgs.SparseRemote = true
			restoreArgs.SparseRemoteToken = relays.memoryToken
		}
	}
	networkArgs, err := dehydrationNetworkArgs(info)
	if err != nil {
		return err
	}
	restoreArgs.NetworkArgs = networkArgs
	encoded, err := json.Marshal(&restoreArgs)
	if err != nil {
		return err
	}
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	if _, err := configWriter.Write(encoded); err != nil {
		configReader.Close()
		configWriter.Close()
		return err
	}
	if err := configWriter.Close(); err != nil {
		configReader.Close()
		return err
	}
	files := []*os.File{configReader, metadata}
	commandArgs := []string{"timetravel", "--dehydration-config-fd=3", "--dehydration-fd=4"}
	targetStartFD := 5 + len(snapshots)
	if sparse {
		if graphSparse {
			manifest, objects, graphErr := b.openGraphMemoryRestore(capture)
			if graphErr != nil {
				configReader.Close()
				return graphErr
			}
			defer manifest.Close()
			defer objects.Close()
			files = append(files, manifest, objects)
			targetStartFD = 7
			commandArgs = append(commandArgs, "--dehydration-graph-manifest-fd=5", "--dehydration-memory-repository-fd=6")
		} else {
			repositoryPath := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks")
			repository, err := os.Open(repositoryPath)
			if err != nil {
				configReader.Close()
				return err
			}
			defer repository.Close()
			files = append(files, repository)
			targetStartFD = 6
			commandArgs = append(commandArgs, "--dehydration-memory-repository-fd=5")
			if relays != nil && relays.memoryClient != nil {
				memoryRemoteFD := 3 + len(files)
				files = append(files, relays.memoryClient)
				commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-memory-remote-fd=%d", memoryRemoteFD))
				targetStartFD++
			}
		}
	} else {
		files = append(files, snapshots...)
		commandArgs = append(commandArgs, "--dehydration-snapshots-start-fd=5", fmt.Sprintf("--dehydration-snapshots-count=%d", len(snapshots)))
	}
	files = append(files, targets...)
	commandArgs = append(commandArgs,
		fmt.Sprintf("--dehydration-targets-start-fd=%d", targetStartFD), fmt.Sprintf("--dehydration-targets-count=%d", len(targets)),
	)
	if relays != nil {
		filesystemStartFD := 3 + len(files)
		files = append(files, relays.clients...)
		commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-filesystems-start-fd=%d", filesystemStartFD), fmt.Sprintf("--dehydration-filesystems-count=%d", len(relays.clients)))
	}
	commandArgs = append(commandArgs, info.ID, "dehydrate-restore", target.branch)
	_, err = b.commandWithFiles(files, commandArgs...)
	configReader.Close()
	if err != nil {
		return fmt.Errorf("installing dehydrated peer: %w", err)
	}
	if err := b.containerStore().CommitActiveRepositoryRestoreIDs(info.ID); err != nil {
		return fmt.Errorf("committing rehydrated peer filesystem identities: %w", err)
	}
	_ = metadata.Close()
	state := "paused"
	if capture.Resume {
		state = "running"
	}
	if err := b.containerStore().UpdateBranchState(info.ID, target.branch, state); err != nil {
		if _, destroyErr := b.command("timetravel", info.ID, "destroy", target.branch); destroyErr != nil {
			return fmt.Errorf("recording rehydrated peer: %w (rolling it back also failed: %v)", err, destroyErr)
		}
		return err
	}
	if relays == nil {
		if err := b.containerStore().UpdateBranchStorage(info.ID, target.branch, branch.StorageGeneration, "local", "", time.Time{}); err != nil {
			return err
		}
	}
	// Persistent state now names the live peer. Keep its on-demand snapshot
	// readers reachable and do not roll the peer back for artifact bookkeeping.
	b.retainRehydrationPageSource(info.ID, branch, sourceDir)
	if _, err := b.ensureConsoleDrain(target, false); err != nil {
		return fmt.Errorf("reattaching init console: %w", err)
	}
	sessions, err := b.execSessions(target)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if session.State == "running" {
			if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false); err != nil {
				return err
			}
		}
	}
	return b.ensurePublishedPortsForBranch(info, target.branch)
}

func dehydrationNetworkArgs(info ttcbundle.ContainerInfo) (*boot.CreateLinksAndRoutesArgs, error) {
	link := boot.DefaultLoopbackLink
	args := &boot.CreateLinksAndRoutesArgs{LoopbackLinks: []boot.LoopbackLink{link}, IsRestore: true}
	switch info.NetworkMode {
	case "none":
		return args, nil
	case "ttc":
		cfg := &boot.TimeTravelNetworkConfig{NAT: info.NetworkNAT}
		if info.NetworkIP != "" {
			cfg.GuestIP = net.ParseIP(info.NetworkIP)
			if cfg.GuestIP == nil {
				return nil, fmt.Errorf("invalid persisted TTC network IP %q", info.NetworkIP)
			}
		}
		if info.NetworkMAC != "" {
			mac, err := net.ParseMAC(info.NetworkMAC)
			if err != nil {
				return nil, fmt.Errorf("invalid persisted TTC network MAC %q: %w", info.NetworkMAC, err)
			}
			cfg.GuestMAC = mac
		}
		args.TimeTravel = cfg
		return args, nil
	default:
		return nil, fmt.Errorf("network mode %q cannot be reconstructed from a process-free dehydration image", info.NetworkMode)
	}
}

func (b *runtimeBackend) destroy(target selector, force bool) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	if err := b.ensureDistributedSourceIndependent(info.ID, target.branch); err != nil {
		return fmt.Errorf("settling remote branches before removing %s: %w", target, err)
	}
	var recorded *ttcbundle.BranchInfo
	for i := range info.Branches {
		if info.Branches[i].ID == target.branch {
			recorded = &info.Branches[i]
			break
		}
	}
	if recorded == nil {
		return fmt.Errorf("branch %q does not exist", target.branch)
	}
	switch normalizeLifecycleState(recorded.State) {
	case "running", "paused", "creating", "dehydrating", "rehydrating", "restarting":
		if !force {
			return fmt.Errorf("branch %s is %s; use -f to force removal", target, normalizeLifecycleState(recorded.State))
		}
	}
	remaining := len(info.Branches)
	if remaining == 1 {
		// Removing the family's final branch also removes the now-empty family.
		// Named volumes remain independently managed, matching a normal rm
		// without --volumes.
		return b.remove(target.container, true, false)
	}
	releaseClients, err := b.quiesceConsoleClients(target)
	if err != nil {
		return fmt.Errorf("detaching console clients for %s: %w", target, err)
	}
	defer releaseClients()
	store := b.containerStore()
	if paths, pathErr := store.BranchConsolePaths(info.ID, target.branch); pathErr == nil {
		if err := b.stopConsoleDrain(paths); err != nil {
			return fmt.Errorf("stopping console follower for %s: %w", target, err)
		}
	}
	if sessions, sessionsErr := b.execSessions(target); sessionsErr == nil {
		for _, session := range sessions {
			paths, pathErr := store.ExecConsolePaths(info.ID, target.branch, session.ID)
			if pathErr == nil {
				if err := b.stopConsoleDrain(paths); err != nil {
					return fmt.Errorf("stopping exec console follower %q for %s: %w", session.ID, target, err)
				}
			}
		}
	}
	// Move the loader's bookkeeping off this branch before destroying it, the
	// same way stopping one does. Without it, removing the branch the Sentry
	// happens to keep its loader state on takes its siblings' terminals with
	// it: a shell a fork inherited is left holding a console that has just been
	// torn down, and it exits. Which made removing a family's first branch —
	// exactly what promoting another one has to do — quietly kill the sessions
	// in the branch being promoted.
	switch normalizeLifecycleState(recorded.State) {
	case "running", "restarting":
		if _, err := b.command("timetravel", target.container, "prepare-stop", target.branch); err != nil {
			return fmt.Errorf("preparing %s for removal: %w", target, err)
		}
	}
	destroyRuntime := func() error {
		// Elastic placement can leave this as the last physical branch on this
		// node while persistent siblings run elsewhere. runsc's branch destroy
		// correctly refuses to remove a runtime's final branch, so delete this
		// node-local runtime shell instead; shared family metadata is removed
		// separately below only for the selected branch.
		if families, listErr := b.listRuntimeFamilies(); listErr == nil {
			for _, family := range families {
				if family.ID != target.container || len(family.Branches) != 1 || family.Branches[0].ID != target.branch {
					continue
				}
				if _, err := b.command("delete", "--force", target.container); err != nil {
					return fmt.Errorf("deleting final physical runtime branch %s: %w", target, err)
				}
				return nil
			}
		}
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
				found := false
				for _, branch := range family.Branches {
					if branch.ID == target.branch {
						found = true
						break
					}
				}
				if !found {
					// The destroy may have committed before its control reply was
					// lost, or a prior cleanup attempt may already have removed the
					// runtime branch. Continue the idempotent persistent cleanup.
					return nil
				}
				// Stopping main tears down the Sentry and all in-memory children.
				if family.State == "stopped" || family.State == "exited" {
					return nil
				}
				return destroyErr
			}
			if b.dataRoot == "" {
				return destroyErr
			}
			if _, err := b.containerStore().LoadContainer(target.container); err != nil {
				return destroyErr
			}
			return nil
		}
	}
	if err := destroyRuntime(); err != nil {
		return err
	}
	portErr := b.stopPublishedPortsForBranch(info, target.branch)
	if b.dataRoot != "" {
		if b.publicationQueue != nil {
			if err := b.publicationQueue.CancelBranch(info.ID, recorded.ID); err != nil {
				return fmt.Errorf("canceling pending publications for %s: %w", target, err)
			}
		}
		if recorded.DistributedSparseRepository != "" || recorded.DistributedSparseBranch != "" {
			if err := b.releaseSparseRoot(recorded.DistributedSparseRepository, recorded.DistributedSparseBranch); err != nil {
				return fmt.Errorf("releasing distributed sparse memory for %s: %w", target, err)
			}
		}
		if err := b.releaseSparseMemoryRoot(info.ID, recorded.FilesystemBranch); err != nil {
			if portErr != nil {
				return fmt.Errorf("reclaiming sparse memory for %s: %w (stopping published ports also failed: %v)", target, err, portErr)
			}
			return fmt.Errorf("reclaiming sparse memory for %s: %w", target, err)
		}
		if err := b.releaseGraphMemoryRoots(recorded.MemoryRoots); err != nil {
			return fmt.Errorf("reclaiming graph memory for %s: %w", target, err)
		}
		if err := store.RemoveBranchFilesystems(target.container, *recorded); err != nil {
			if portErr != nil {
				return fmt.Errorf("reclaiming filesystems for %s: %w (stopping published ports also failed: %v)", target, err, portErr)
			}
			return fmt.Errorf("reclaiming filesystems for %s: %w", target, err)
		}
		if err := store.RemoveBranchArtifacts(target.container, *recorded); err != nil {
			return fmt.Errorf("removing persistent artifacts for %s: %w", target, err)
		}
		if err := store.RemoveBranchRecord(target.container, target.branch); err != nil {
			if portErr != nil {
				return fmt.Errorf("persisting deletion of %s: %w (stopping published ports also failed: %v)", target, err, portErr)
			}
			return fmt.Errorf("persisting deletion of %s: %w", target, err)
		}
		if b.distributedSource != nil && b.distributedSource.authority != nil {
			if authority, err := b.distributedSource.authority.Branch(info.ID, target.branch); err == nil && authority.Corrupted {
				if err := b.distributedSource.authority.RemoveCorruptedBranch(info.ID, target.branch); err != nil {
					return fmt.Errorf("removing corrupted branch authority for %s: %w", target, err)
				}
			}
		}
		latest, err := store.LoadContainer(target.container)
		if err != nil {
			return err
		}
		if err := b.deleteRuntimeIfProcessFree(latest); err != nil {
			return err
		}
	}
	return portErr
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

func (b *runtimeBackend) rootfsSize(target selector) (uint64, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return 0, err
	}
	if b.dataRoot != "" {
		spec, specErr := b.containerStore().LoadSpec(resolved.container)
		if specErr == nil && spec.Root != nil && spec.Root.Readonly {
			return 0, nil
		}
	}
	output, err := b.command("timetravel", resolved.container, "size", resolved.branch)
	if err != nil {
		return 0, err
	}
	var size struct {
		WritableBytes uint64 `json:"writable_bytes"`
	}
	if err := json.Unmarshal(output, &size); err != nil {
		return 0, fmt.Errorf("decoding branch rootfs size: %w", err)
	}
	return size.WritableBytes, nil
}

func (b *runtimeBackend) diff(target selector) ([]BranchPathChange, error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return nil, err
	}
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = offlineCopyBranch(info, resolved.branch)
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return nil, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, err
	}
	changes := make([]BranchPathChange, 0)
	for _, config := range repositories {
		mount, err := repositoryGuestMount(spec, config)
		if err != nil {
			return nil, err
		}
		dir, err := os.Open(config.Path)
		if err != nil {
			return nil, fmt.Errorf("opening repository for %s: %w", mount, err)
		}
		repository, err := ttcvolume.OpenRepositoryFD(dir, ttcvolume.CopyMode(config.CopyMode))
		_ = dir.Close()
		if err != nil {
			return nil, fmt.Errorf("opening repository for %s: %w", mount, err)
		}
		diff, diffErr := repository.BranchDiff(filesystemBranch)
		closeErr := repository.Close()
		if diffErr != nil {
			return nil, fmt.Errorf("reading repository diff for %s: %w", mount, diffErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for _, change := range diff {
			path := mount
			if change.Path != "/" {
				path = filepath.Join(mount, strings.TrimPrefix(change.Path, "/"))
			}
			changes = append(changes, BranchPathChange{
				Path: path, Kind: change.Kind, Type: change.Type, Mount: mount,
			})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].Mount < changes[j].Mount
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func addResourceBytes(target *ResourceBytes, privateBytes, sharedBytes uint64) {
	target.PrivateBytes += privateBytes
	target.SharedBytes += sharedBytes
	target.TotalBytes = target.PrivateBytes + target.SharedBytes
}

func (b *runtimeBackend) resourceStats(target selector) (FamilyResourceStats, error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return FamilyResourceStats{}, err
	}
	if _, err := persistentBranchInfo(info, resolved.branch); err != nil {
		return FamilyResourceStats{}, err
	}
	result := FamilyResourceStats{Container: info.Name}
	if result.Container == "" {
		result.Container = target.container
	}
	branches := append([]ttcbundle.BranchInfo(nil), info.Branches...)
	if b.nodeID != "" {
		placements, placementErr := b.placementCatalog()
		filtered := branches[:0]
		for _, branch := range branches {
			owned := false
			if placementErr == nil {
				if record, err := placements.Lookup(info.ID, branch.ID); err == nil {
					owned = record.OwnerNode == b.nodeID && (record.State == cluster.PlacementRunning || record.State == cluster.PlacementPaused)
				}
			} else {
				owned = info.BranchNodeID(branch.ID) == b.nodeID
			}
			if owned {
				filtered = append(filtered, branch)
			}
		}
		branches = filtered
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].ID < branches[j].ID })
	result.Branches = make([]BranchResourceStats, 0, len(branches))
	byID := make(map[string]*BranchResourceStats, len(branches))
	filesystemBranches := make(map[string]string, len(branches))
	for _, branch := range branches {
		account := BranchResourceStats{
			Target: result.Container + "@" + branch.ID, State: normalizeLifecycleState(branch.State),
			StorageState: branch.StorageState, StorageGeneration: branch.StorageGeneration, SourceNode: branch.SourceNode,
			DependentMemoryBytes: branch.DependentMemoryBytes, DependentFilesystemBytes: branch.DependentFilesystemBytes,
		}
		result.DependentMemoryBytes += branch.DependentMemoryBytes
		result.DependentFilesystemBytes += branch.DependentFilesystemBytes
		result.Branches = append(result.Branches, account)
		byID[branch.ID] = &result.Branches[len(result.Branches)-1]
		filesystemBranch := branch.FilesystemBranch
		if filesystemBranch == "" {
			filesystemBranch = offlineCopyBranch(info, branch.ID)
		}
		filesystemBranches[branch.ID] = filesystemBranch
	}

	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return FamilyResourceStats{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return FamilyResourceStats{}, err
	}
	for _, config := range repositories {
		dir, err := os.Open(config.Path)
		if err != nil {
			return FamilyResourceStats{}, err
		}
		repository, openErr := ttcvolume.OpenRepositoryFD(dir, ttcvolume.CopyMode(config.CopyMode))
		_ = dir.Close()
		if openErr != nil {
			return FamilyResourceStats{}, openErr
		}
		filesystemIDs := make([]string, 0, len(filesystemBranches))
		seenFilesystemIDs := make(map[string]struct{}, len(filesystemBranches))
		for _, filesystemBranch := range filesystemBranches {
			if _, ok := seenFilesystemIDs[filesystemBranch]; ok {
				continue
			}
			seenFilesystemIDs[filesystemBranch] = struct{}{}
			filesystemIDs = append(filesystemIDs, filesystemBranch)
		}
		usage, usageErr := repository.BranchSetUsage(filesystemIDs)
		closeErr := repository.Close()
		if usageErr != nil {
			return FamilyResourceStats{}, usageErr
		}
		if closeErr != nil {
			return FamilyResourceStats{}, closeErr
		}
		result.FilesystemUniqueBytes += usage.UniqueBytes
		visibleByFilesystem := make(map[string][]string)
		for visible, filesystem := range filesystemBranches {
			visibleByFilesystem[filesystem] = append(visibleByFilesystem[filesystem], visible)
		}
		for _, branchUsage := range usage.Branches {
			for _, visible := range visibleByFilesystem[branchUsage.ID] {
				account := byID[visible]
				addResourceBytes(&account.Filesystem, branchUsage.PrivateBytes, branchUsage.SharedBytes)
				account.FilesystemInodes += branchUsage.Inodes
			}
		}
	}

	memoryPath := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks")
	if _, statErr := os.Stat(memoryPath); statErr == nil {
		memoryRepository, openErr := ttcchunk.OpenMemoryRepository(memoryPath)
		if openErr != nil {
			return FamilyResourceStats{}, openErr
		}
		roots, rootsErr := memoryRepository.ListBranches()
		if rootsErr != nil {
			memoryRepository.Close()
			return FamilyResourceStats{}, rootsErr
		}
		memoryBranches := make(map[string]string)
		for visible, filesystem := range filesystemBranches {
			if _, ok := roots[filesystem]; ok {
				memoryBranches[visible] = filesystem
			}
		}
		usage, usageErr := memoryRepository.BranchSetUsage(memoryBranches)
		closeErr := memoryRepository.Close()
		if usageErr != nil {
			return FamilyResourceStats{}, usageErr
		}
		if closeErr != nil {
			return FamilyResourceStats{}, closeErr
		}
		result.MemoryBackingUniqueBytes = usage.UniqueBytes
		for _, branchUsage := range usage.Branches {
			if account := byID[branchUsage.ID]; account != nil {
				addResourceBytes(&account.MemoryBacking, branchUsage.PrivateBytes, branchUsage.SharedBytes)
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return FamilyResourceStats{}, statErr
	}

	output, residentErr := b.command("timetravel", resolved.container, "stats", resolved.branch)
	if residentErr == nil {
		var resident boot.FamilyResidentMemory
		if err := json.Unmarshal(output, &resident); err != nil {
			return FamilyResourceStats{}, fmt.Errorf("decoding resident memory accounting: %w", err)
		}
		result.ResidentMemoryUniqueBytes = resident.UniqueBytes
		for _, branchMemory := range resident.Branches {
			if account := byID[branchMemory.ID]; account != nil {
				addResourceBytes(&account.ResidentMemory, branchMemory.PrivateBytes, branchMemory.SharedBytes)
			}
		}
	} else {
		for _, branch := range branches {
			state := normalizeLifecycleState(branch.State)
			if state == "running" || state == "paused" {
				return FamilyResourceStats{}, residentErr
			}
		}
	}
	return result, nil
}

func repositoryGuestMount(spec *specs.Spec, repository specutils.TTCVolumeRepository) (string, error) {
	if repository.Source == specutils.TTCRootfsUpperSource {
		return "/", nil
	}
	if repository.OverlayDestination != "" {
		return repository.OverlayDestination, nil
	}
	for _, mount := range spec.Mounts {
		if mount.Source == repository.Source {
			return mount.Destination, nil
		}
	}
	return "", fmt.Errorf("repository source %q has no guest mount", repository.Source)
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
	store := b.containerStore()
	if info, err := store.LoadContainer(target.container); err == nil {
		if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: stopping published ports for exited %s: %v\n", target, err)
		}
	}
	exitStatus := waitStatusExitCode(status)
	if err := store.RecordBranchExit(target.container, target.branch, exitStatus); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: warning: recording branch exit state for %s: %v\n", target, err)
	}
}

func (b *runtimeBackend) waitForConsoleStatus(target selector) (uint32, bool, error) {
	store := b.containerStore()
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
			suffix = fmt.Sprintf(" parent=%s (missing)", branch.ParentID)
		}
	}
	if branch.InitState != "" {
		pid := "-"
		if branch.InitPID != 0 {
			pid = strconv.FormatInt(int64(branch.InitPID), 10)
		}
		suffix += fmt.Sprintf(" pid1=%s(%s)", branch.InitState, pid)
	}
	if branch.MemorySharing != "" {
		suffix += " memory=" + branch.MemorySharing
	}
	if !branch.CreatedAt.IsZero() {
		suffix += " created=" + branch.CreatedAt.UTC().Format(time.RFC3339Nano)
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
