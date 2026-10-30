// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"text/template"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"

	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/processutil"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
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
	cpuFeatures           string
	cpuTopology           string
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	daemonSocket          string
	direct                bool
	// disableSyscallPatching runs systrap without syscall patching, as a
	// cluster whose nodes need no CPUID faulting does.
	disableSyscallPatching bool
	// requester is who asked for the command, nil for the daemon's owner.
	requester *daemonRequester
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
		var finish func(bool) error
		args, stdin, stdout, finish, err = prepareCheckpointClientIO(args, stdin, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		if finish != nil {
			cleanup := finish
			completed := false
			defer func() {
				// The actual result is finalized immediately below. This fallback
				// only closes/removes a temporary file if a future early return is
				// added between preparation and transport.
				if !completed {
					_ = cleanup(false)
				}
			}()
			status, runErr := runViaDaemon(opts, args, stdin, stdout, stderr)
			finishErr := cleanup(runErr == nil && status == 0)
			completed = true
			if runErr != nil {
				fmt.Fprintf(stderr, "ttc: daemon transport: %v\n", runErr)
				return 1
			}
			if finishErr != nil {
				fmt.Fprintf(stderr, "ttc: finalizing checkpoint archive: %v\n", finishErr)
				return 1
			}
			return status
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
		daemonSocket:    daemonSocketFromEnvironment(),
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
	usage := `Usage: ttc [GLOBAL OPTIONS] COMMAND

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
  create [--node NODE] [OPTIONS] IMAGE [CMD]
                                 Prepare a container without starting it
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
  node rm NODE                    Forget an offline, expired node identity
  node stop [POLICY] NODE         Evacuate a node and stop its daemon
  node rebalance NODE             Relieve a node's configured memory pressure
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
  daemon start [--platform systrap|kvm] [--cpu-profile portable|host|explicit]
               [--cpu-features FEATURE,...] [--experimental-no-cpuid-faulting]
               [--cpu-topology single|host]
               [--unsafe-no-host-isolation] [--unsafe-access-host-group GROUP]
               [--control-only]
               [--node NAME] [--listen ADDR] [--advertise HOST:PORT]
               [--memory-target SIZE]
                                  Start the per-user service in one fixed mode
  daemon status|stop              Inspect or stop the per-user service
  daemon requests [OPTIONS]        List durable mutation requests
  daemon request inspect|resolve   Inspect or resolve an interrupted mutation

Branch selectors must use CONTAINER@BRANCH. Local fork destinations are branch
names in the source family. Forked branches run immediately unless --paused is
set; omitting DEST generates a branch ID.
`
	fmt.Fprint(w, strings.ReplaceAll(usage, "systrap|kvm", runtimePlatformFlagUsage()))
}

func printCustomCommandHelp(args []string, stdout io.Writer) bool {
	if len(args) == 2 && args[1] == "--help" {
		usage := map[string]string{
			"container":  "Usage: ttc container COMMAND",
			"image":      "Usage: ttc image ls|pull|load|save|inspect|rm [OPTIONS]",
			"load":       "Usage: ttc load [--input FILE] [--platform PLATFORM] [--quiet]",
			"save":       "Usage: ttc save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
			"commit":     "Usage: ttc commit [-a AUTHOR] [-c CHANGE] [-m MESSAGE] [-p=true|false] CONTAINER@BRANCH REPOSITORY[:TAG]",
			"port":       "Usage: ttc port publish|ls|unpublish [OPTIONS]",
			"volume":     "Usage: ttc volume create|ls|inspect|rm [OPTIONS]",
			"completion": "Usage: ttc completion bash|zsh|fish",
			"daemon": `Usage: ttc daemon start [OPTIONS]
       ttc daemon start --control-only [OPTIONS]
       ttc daemon status
       ttc daemon stop [--migrate-all|--dehydrate-all|--stop-all]

With --listen, --advertise, or --node, the daemon is a compute node of the
cluster whose shared root TTC_ROOT is, with its own TTC_LOCAL_ROOT. Every node
starts alike: it holds the cluster's authority, or joins the node that does.

--control-only starts an outbound-only login-node gateway. It accepts commands
on the socket in its TTC_LOCAL_ROOT but advertises no TCP endpoint, registers
no node, and accepts no container placement. It finds the cluster's compute
nodes in TTC_ROOT, the cluster's shared root.`,
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
			"stats":        "Usage: ttc stats [--json] [--no-stream] CONTAINER@BRANCH\n\nTTC stats is one-shot by default; --no-stream is accepted for Docker CLI compatibility.",
			"tty":          "Usage: ttc tty stat|watch CONTAINER@BRANCH --session NAME [--json]",
			"cp":           "Usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH",
			"checkpoint":   "Usage: ttc checkpoint [--compression none|flate-best-speed] CONTAINER@BRANCH FILE",
			"migrate":      "Usage: ttc migrate --node NODE CONTAINER@BRANCH",
			"node":         "Usage: ttc node ls [--json] | ttc node rm NODE | ttc node stop [--stop-all|--dehydrate-all|--migrate-all] NODE | ttc node rebalance NODE",
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
			"image inspect":     "Usage: ttc image inspect [--format TEMPLATE] IMAGE [IMAGE...]",
			"image load":        "Usage: ttc image load [--input FILE] [--platform PLATFORM] [--quiet]",
			"image save":        "Usage: ttc image save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
			"port publish":      "Usage: ttc port publish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",
			"port ls":           "Usage: ttc port ls [--json] CONTAINER@BRANCH",
			"port unpublish":    "Usage: ttc port unpublish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",
			"volume create":     "Usage: ttc volume create NAME",
			"volume ls":         "Usage: ttc volume ls [--json]",
			"volume list":       "Usage: ttc volume ls [--json]",
			"volume inspect":    "Usage: ttc volume inspect NAME [NAME...]",
			"volume rm":         "Usage: ttc volume rm [--force] NAME [NAME...]",
			"volume remove":     "Usage: ttc volume rm [--force] NAME [NAME...]",
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
	case "_processes-local":
		err = runProcessesLocal(args[1:], stdout, backend)
	case "_rootfs-sizes-local":
		err = runRootfsSizesLocal(args[1:], stdout, backend)
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
	case "_remove-family-record":
		removeVolumes := false
		operands := args[1:]
		if len(operands) != 0 && operands[0] == "--volumes" {
			removeVolumes = true
			operands = operands[1:]
		}
		if len(operands) != 1 {
			err = errors.New("internal family-record removal requires one family ID")
			break
		}
		remover, ok := backend.(interface {
			removeDistributedFamilyRecord(string, bool) error
		})
		if !ok {
			err = errors.New("this backend does not support distributed family-record removal")
			break
		}
		err = remover.removeDistributedFamilyRecord(operands[0], removeVolumes)
	case "_remove-corrupted-branch":
		if len(args) != 2 {
			err = errors.New("internal corrupted-branch removal requires one CONTAINER@BRANCH selector")
			break
		}
		target, parseErr := parseSelector(args[1])
		if parseErr != nil {
			err = parseErr
			break
		}
		remover, ok := backend.(interface{ removeCorruptedBranch(selector) error })
		if !ok {
			err = errors.New("this backend does not support corrupted-branch removal")
			break
		}
		err = remover.removeCorruptedBranch(target)
	case "_reclaim-removed-family-local":
		if len(args) < 3 {
			err = errors.New("internal removed-family cleanup requires a family ID and at least one physical container ID")
			break
		}
		reclaimer, ok := backend.(interface{ reclaimRemovedFamilyLocal(string, []string) error })
		if !ok {
			err = errors.New("this backend does not support removed-family local cleanup")
			break
		}
		err = reclaimer.reclaimRemovedFamilyLocal(args[1], args[2:])
	case "_settle-source-transactions":
		if len(args) < 2 {
			err = errors.New("internal source-dependency settlement requires at least one transaction ID")
			break
		}
		settler, ok := backend.(interface{ settleDistributedSourceTransactions([]string) error })
		if !ok {
			err = errors.New("this backend does not support distributed source-dependency settlement")
			break
		}
		err = settler.settleDistributedSourceTransactions(args[1:])
	case "_cancel-source-transactions":
		if len(args) < 2 {
			err = errors.New("internal source-dependency cancellation requires at least one transaction ID")
			break
		}
		canceler, ok := backend.(interface{ cancelDistributedForkTargets([]string) error })
		if !ok {
			err = errors.New("this backend does not support distributed source-dependency cancellation")
			break
		}
		err = canceler.cancelDistributedForkTargets(args[1:])
	case "_remove-canceled-fork-placement":
		if len(args) != 3 {
			err = errors.New("internal canceled-fork placement removal requires a branch selector and transaction ID")
			break
		}
		target, parseErr := parseSelector(args[1])
		if parseErr != nil {
			err = parseErr
			break
		}
		remover, ok := backend.(interface {
			removeCanceledForkPlacement(selector, string) error
		})
		if !ok {
			err = errors.New("this backend does not support canceled-fork placement removal")
			break
		}
		err = remover.removeCanceledForkPlacement(target, args[2])
	case "_discard-removed-migration-sources":
		if len(args) < 2 {
			err = errors.New("internal removed-migration cleanup requires at least one transaction ID")
			break
		}
		discarder, ok := backend.(interface{ discardRemovedMigrationSources([]string) error })
		if !ok {
			err = errors.New("this backend does not support removed-migration cleanup")
			break
		}
		err = discarder.discardRemovedMigrationSources(args[1:])
	case "_prepare-removed-migration-sources":
		if len(args) < 2 {
			err = errors.New("internal removed-migration preparation requires at least one transaction ID")
			break
		}
		preparer, ok := backend.(interface{ prepareRemovedMigrationSources([]string) error })
		if !ok {
			err = errors.New("this backend does not support removed-migration preparation")
			break
		}
		err = preparer.prepareRemovedMigrationSources(args[1:])
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
		err = runPauseResume(args[1:], stdout, stderr, backend, true)
	case "resume", "unpause":
		err = runPauseResume(args[1:], stdout, stderr, backend, false)
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
		if mover, ok := backend.(interface{ refusedAsMoved() bool }); ok && mover.refusedAsMoved() {
			fmt.Fprintf(stderr, "%s %v\n", protocol.StaleRouteRefusal, err)
			return 1
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
	removeNode(node string) error
	stopNode(sourceNode, mode string, local bool) ([]selector, error)
	rebalanceNode(sourceNode, targetNode string, local bool) (cluster.FixedTerminalPlan, error)
	plannerSnapshot(node string, local bool) (cluster.PlannerSnapshot, error)
}

func runNode(args []string, stdout io.Writer, backend backend) error {
	if len(args) == 0 {
		return errors.New("usage: ttc node ls [--json] | ttc node rm NODE | ttc node stop [--stop-all|--dehydrate-all|--migrate-all] NODE | ttc node rebalance NODE")
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
		fmt.Fprintln(writer, "NODE\tADDRESS\tSTATE\tMEMORY\tTARGET\tRESERVED\tLEASE")
		now := time.Now()
		for _, record := range records {
			lease := "expired"
			if remaining := record.LeaseExpiresAt.Sub(now); remaining > 0 {
				lease = remaining.Round(time.Second).String()
			}
			address := strings.TrimPrefix(strings.TrimPrefix(record.ControlURL, "http://"), "https://")
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", record.NodeID, address, record.State,
				formatImageSize(int64(record.MemoryCurrentBytes)), formatImageSize(int64(record.MemoryTargetBytes)),
				formatImageSize(int64(record.IncomingReservedBytes)), lease)
		}
		return writer.Flush()
	case "rm":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: ttc node rm NODE")
		}
		if err := nodes.removeNode(args[1]); err != nil {
			return err
		}
		fmt.Fprintln(stdout, args[1])
		return nil
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
		return runPauseResume(args[1:], stdout, stderr, backend, true)
	case "unpause":
		return runPauseResume(args[1:], stdout, stderr, backend, false)
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

// collectEnvironment collects a create's environment, filling a variable
// that it names without a value with lookup.
func collectEnvironment(fileNames, direct []string, lookup func(string) (string, bool)) ([]string, error) {
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
			if hostValue, ok := lookup(name); ok {
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
			return nil, fmt.Errorf("invalid volume specification %q; want HOST:CONTAINER[:ro|rw|copy|lazy_copy]", value)
		}
		readonly := false
		writable := false
		immutableSource := false
		managedCoW := false
		copySource := false
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro", "readonly":
					readonly = true
				case "rw":
					writable = true
				case "immutable-source":
					immutableSource = true
				case "lazy_copy", "lazy-copy", "cow", "managed-cow":
					managedCoW = true
				case "copy":
					copySource = true
				case "":
				default:
					return nil, fmt.Errorf("unsupported volume option %q in %q", option, value)
				}
			}
		}
		if readonly && writable {
			return nil, fmt.Errorf("volume %q is both ro and rw", value)
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
				case "lazy_copy", "lazy-copy", "cow", "managed-cow":
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
			case "lazy_copy", "lazy-copy", "cow", "managed-cow":
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid %s value %q in %q", key, raw, value)
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

// normalizeHostMount returns the host bind of source at destination: read-only
// if readonly; a branch-local layer over source, which copies a file on its
// first write, if managedCoW (lazy_copy); a copy of source if copySource; and
// otherwise, as Docker binds a host path, source itself, writable, which only a
// family that waives live forks may bind (see validateHostMountForkPolicy).
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
		return ttcbundle.HostMount{}, fmt.Errorf("host bind %q:%q cannot combine read-only with lazy_copy or copy", source, destination)
	}
	if managedCoW && copySource {
		return ttcbundle.HostMount{}, fmt.Errorf("host bind %q:%q cannot combine lazy_copy and copy", source, destination)
	}
	direct := !readonly && !managedCoW && !copySource
	if direct && immutableSource {
		return ttcbundle.HostMount{}, fmt.Errorf("host bind %q:%q is writable, so its source is not immutable; bind it :ro, :copy or :lazy_copy", source, destination)
	}
	return ttcbundle.HostMount{
		Source: filepath.Clean(source), Destination: filepath.Clean(destination),
		Readonly: readonly, ImmutableSource: immutableSource, ManagedCoW: managedCoW, Copy: copySource, Direct: direct,
	}, nil
}

// validateHostMountForkPolicy returns an error if a family that may live fork
// binds a host path writable, as Docker binds one: its branches would share the
// host path, where each must have a view of its own.
func validateHostMountForkPolicy(mounts []ttcbundle.HostMount, waived bool) error {
	if waived {
		return nil
	}
	for _, mount := range mounts {
		if mount.Direct {
			return fmt.Errorf("host bind %s:%s writes the host path, of which live forks cannot give each branch its own view; pass --waive-live-fork to bind it so, or bind it :ro, :copy or :lazy_copy", mount.Source, mount.Destination)
		}
	}
	return nil
}

func parseNetworkMode(value string) (string, error) {
	switch value {
	case "none", "host", "ttc":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q; supported modes are none, host, and ttc", value)
	}
}

func liveForkWaiverReason(waived bool, networkMode string, gpu ttcbundle.GPUOptions, hostMounts []ttcbundle.HostMount) string {
	if !waived {
		return ""
	}
	if networkMode == "host" {
		return "explicit waiver for host networking"
	}
	if gpu.GFX && gpu.GFXFork == "" {
		return "explicit waiver for the virtual GPU"
	}
	if gpu.NVProxy && gpu.NVProxyFork == "" {
		return "explicit waiver for NVIDIA GPUs"
	}
	for _, mount := range hostMounts {
		if mount.Direct {
			return "explicit waiver for a writable host bind"
		}
	}
	return "explicitly waived during container creation"
}

// gpuFlags are the GPU options of ttc run and ttc create.
type gpuFlags struct {
	gfx         *bool
	gfxFork     *string
	nvproxy     nvproxyFlag
	nvproxyFork *string
}

func addGPUFlags(fs *flag.FlagSet) *gpuFlags {
	f := &gpuFlags{}
	f.gfx = fs.Bool("gfx", false, "give the container a virtual GPU, /dev/dri/renderD128")
	f.gfxFork = fs.String("gfx-fork", "", "what a live fork does with the virtual GPU while it is in use: device-lost or snapshot; without it, a --gfx family must waive live forks")
	fs.Var(&f.nvproxy, "nvproxy", "give the container the host's NVIDIA GPUs; --nvproxy=CAPS limits the driver capabilities")
	f.nvproxyFork = fs.String("nvproxy-fork", "", "what a live fork does with NVIDIA GPUs while they are in use: device-lost or snapshot")
	return f
}

func (f *gpuFlags) options() ttcbundle.GPUOptions {
	return ttcbundle.GPUOptions{
		GFX:         *f.gfx,
		GFXFork:     *f.gfxFork,
		NVProxy:     f.nvproxy.enabled,
		NVProxyCaps: f.nvproxy.caps,
		NVProxyFork: *f.nvproxyFork,
	}
}

// nvproxyFlag is --nvproxy[=CAPS].
type nvproxyFlag struct {
	enabled bool
	caps    string
}

// String implements flag.Value.String.
func (f *nvproxyFlag) String() string {
	if f == nil || !f.enabled {
		return "false"
	}
	if f.caps != "" {
		return f.caps
	}
	return "true"
}

// Set implements flag.Value.Set.
func (f *nvproxyFlag) Set(value string) error {
	switch value {
	case "true":
		f.enabled, f.caps = true, ""
	case "false":
		f.enabled, f.caps = false, ""
	case "":
		return errors.New("--nvproxy= needs driver capabilities, such as compute,utility")
	default:
		f.enabled, f.caps = true, value
	}
	return nil
}

// IsBoolFlag implements flag's boolFlag, so that --nvproxy takes no value.
func (f *nvproxyFlag) IsBoolFlag() bool {
	return true
}

func validateLiveForkCreationPolicy(networkMode string, waived bool) error {
	if networkMode == "host" && !waived {
		return errors.New("--network=host is incompatible with live fork eligibility; pass --waive-live-fork to create a systematically non-forkable family")
	}
	return nil
}

func validateVirtualNICOptions(networkMode, ipValue, macValue, interfaceName string) error {
	if (ipValue != "" || macValue != "" || interfaceName != "") && networkMode != "ttc" {
		return errors.New("--ip, --mac-address, and --interface-name require --network=ttc")
	}
	if interfaceName != "" {
		if err := config.ValidateNetworkInterfaceName(interfaceName); err != nil {
			return fmt.Errorf("--interface-name: %w", err)
		}
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
		// Command-line publications belong to the initial branch. New-format
		// roots reject an empty branch identity instead of interpreting it as a
		// legacy spelling of main.
		bindings = append(bindings, ttcbundle.PortBinding{HostIP: address.String(), HostPort: hostPort, ContainerPort: containerPort, Protocol: protocol, BranchID: "main"})
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
	gpu                ttcbundle.GPUOptions
	networkNAT         bool
	networkIP          string
	networkMAC         string
	networkInterface   string
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
	memorySwap         *int64
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

// makesOnlyItsFamily reports whether making the family r asks for changes
// nothing on its node beside the family: a named volume it would make or
// attach, a port it would publish and a device it would take are shared with
// other families. An anonymous volume is the family's own.
func (r runRequest) makesOnlyItsFamily() bool {
	if len(r.publishedPorts) != 0 || r.gpu.GFX || r.gpu.NVProxy {
		return false
	}
	for _, volume := range r.managedVolumes {
		if volume.Name != "" {
			return false
		}
	}
	return true
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
	retain         bool
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

// eachTarget runs op on each of targets in turn, as docker's commands of
// several containers do: one that fails does not keep the rest from being
// tried. It writes each failure but the last to stderr as it happens and
// returns the last, so that the command fails once all are tried.
func eachTarget(targets []string, stderr io.Writer, op func(string) error) error {
	var last error
	for _, target := range targets {
		if err := op(target); err != nil {
			if last != nil {
				reportTargetError(stderr, last)
			}
			last = err
		}
	}
	return last
}

// reportTargetError writes err to stderr as runCommand would, a failure of one
// of several targets that is not the command's error.
func reportTargetError(stderr io.Writer, err error) {
	var status exitStatus
	if !errors.As(err, &status) {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
	}
}

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
	args = expandCombinedShortBools(args, "dit", "--name", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "-h", "--network", "--net", "--ip", "--mac-address", "--interface-name", "--publish", "-p", "--add-host", "--dns", "--dns-search", "--dns-option", "--dns-opt", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--memory", "-m", "--memory-swap", "--cpus", "--cpu-shares", "--cpu-period", "--cpu-quota", "--shm-size", "--group-add", "--cap-add", "--cap-drop", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-driver", "--log-opt", "--gfx-fork", "--nvproxy-fork")
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
	memorySwapValue := fs.String("memory-swap", "0", "memory plus swap limit; -1 is unlimited")
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
	gpu := addGPUFlags(fs)
	networkNAT := fs.Bool("nat", false, "allow outbound host networking from a ttc virtual NIC")
	networkIP := fs.String("ip", "", "IPv4 address for the ttc virtual NIC")
	networkMAC := fs.String("mac-address", "", "MAC address for the ttc virtual NIC")
	networkInterface := fs.String("interface-name", "", "guest's name for the ttc virtual NIC (default "+ttcbundle.DefaultNetworkInterface+")")
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
	fs.Var(&volumes, "volume", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy|lazy_copy]")
	fs.Var(&volumes, "v", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy|lazy_copy]")
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
	requester := backendRequester(backend)
	if err := memberFileRefusal(requester, envFiles, labelFiles); err != nil {
		return err
	}
	envValues, err := collectEnvironment(envFiles, env, requester.environmentLookup())
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
	gpuOptions := gpu.options()
	if err := ttcbundle.ValidateGPUOptions(gpuOptions, !*waiveLiveFork, runtime.GOOS); err != nil {
		return err
	}
	if *networkNAT && networkMode != "ttc" {
		return errors.New("--nat requires --network=ttc")
	}
	if err := validateVirtualNICOptions(networkMode, *networkIP, *networkMAC, *networkInterface); err != nil {
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
	if err := memberHostMountRefusal(requester, len(parsedMounts.host)); err != nil {
		return err
	}
	if err := validateHostMountForkPolicy(parsedMounts.host, *waiveLiveFork); err != nil {
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
	parsedMemorySwap, err := ttcbundle.ParseMemorySwap(*memorySwapValue)
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
	memorySwap := effectiveMemorySwap(effectiveMemoryLimit, parsedMemorySwap, flagWasSet(fs, "memory-swap"), stderr)
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
		liveForkWaiver:     liveForkWaiverReason(*waiveLiveFork, networkMode, gpuOptions, parsedMounts.host),
		gpu:                gpuOptions,
		networkNAT:         *networkNAT,
		networkIP:          *networkIP,
		networkMAC:         strings.ToLower(*networkMAC),
		networkInterface:   *networkInterface,
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
		memorySwap:         memorySwap,
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
	args = expandCombinedShortBools(args, "it", "--name", "--pull", "--workdir", "-w", "--user", "-u", "--hostname", "-h", "--network", "--net", "--ip", "--mac-address", "--interface-name", "--publish", "-p", "--add-host", "--dns", "--dns-search", "--dns-option", "--dns-opt", "--entrypoint", "--stop-signal", "--stop-timeout", "--pids-limit", "--memory", "-m", "--memory-swap", "--cpus", "--cpu-shares", "--cpu-period", "--cpu-quota", "--shm-size", "--group-add", "--cap-add", "--cap-drop", "--ulimit", "--env", "-e", "--env-file", "--label", "-l", "--label-file", "--volume", "-v", "--mount", "--tmpfs", "--log-driver", "--log-opt", "--gfx-fork", "--nvproxy-fork")
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
	memorySwapValue := fs.String("memory-swap", "0", "memory plus swap limit; -1 is unlimited")
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
	gpu := addGPUFlags(fs)
	networkNAT := fs.Bool("nat", false, "allow outbound host networking from a ttc virtual NIC")
	networkIP := fs.String("ip", "", "IPv4 address for the ttc virtual NIC")
	networkMAC := fs.String("mac-address", "", "MAC address for the ttc virtual NIC")
	networkInterface := fs.String("interface-name", "", "guest's name for the ttc virtual NIC (default "+ttcbundle.DefaultNetworkInterface+")")
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
	fs.Var(&volumes, "volume", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy|lazy_copy]")
	fs.Var(&volumes, "v", "attach VOLUME:CONTAINER[:ro] or HOST:CONTAINER[:ro|rw|copy|lazy_copy]")
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
	requester := backendRequester(backend)
	if err := memberFileRefusal(requester, envFiles, labelFiles); err != nil {
		return err
	}
	envValues, err := collectEnvironment(envFiles, env, requester.environmentLookup())
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
	gpuOptions := gpu.options()
	if err := ttcbundle.ValidateGPUOptions(gpuOptions, !*waiveLiveFork, runtime.GOOS); err != nil {
		return err
	}
	if *networkNAT && networkMode != "ttc" {
		return errors.New("--nat requires --network=ttc")
	}
	if err := validateVirtualNICOptions(networkMode, *networkIP, *networkMAC, *networkInterface); err != nil {
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
	if err := memberHostMountRefusal(requester, len(parsedMounts.host)); err != nil {
		return err
	}
	if err := validateHostMountForkPolicy(parsedMounts.host, *waiveLiveFork); err != nil {
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
	parsedMemorySwap, err := ttcbundle.ParseMemorySwap(*memorySwapValue)
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
	memorySwap := effectiveMemorySwap(effectiveMemoryLimit, parsedMemorySwap, flagWasSet(fs, "memory-swap"), stderr)
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
		liveForkWaiver:     liveForkWaiverReason(*waiveLiveFork, networkMode, gpuOptions, parsedMounts.host),
		gpu:                gpuOptions,
		networkNAT:         *networkNAT,
		networkIP:          *networkIP,
		networkMAC:         strings.ToLower(*networkMAC),
		networkInterface:   *networkInterface,
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
		memorySwap:         memorySwap,
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
		return errors.New("usage: ttc start [--attach] (CONTAINER|CONTAINER@BRANCH) [(CONTAINER|CONTAINER@BRANCH)...]")
	}
	if *interactive && !*attach {
		return errors.New("--interactive requires --attach")
	}
	if *attach && fs.NArg() != 1 {
		return errors.New("--attach supports exactly one container")
	}
	return eachTarget(fs.Args(), stderr, func(value string) error {
		target, err := parseSelectorOrMain(value)
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
			// Match Docker's output shape for an unqualified main-container
			// operand while retaining the explicit branch for TTC callers.
			fmt.Fprintln(stdout, value)
		}
		return nil
	})
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
	if err := memberExecRefusal(backendRequester(backend), *processPath); err != nil {
		return err
	}
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
	retain := execID != "" || *detach
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
		retain:         retain,
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
	MemorySwap     int64                           `json:"MemorySwap"`
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
	args = expandCombinedShortBools(args, "fv")
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
	// removeAll removes the targets, but for those removed already.
	removeAll := func(removed map[selector]bool) error {
		return eachTarget(fs.Args(), stderr, func(value string) error {
			if strings.Contains(value, "@") {
				if *volumes {
					return fmt.Errorf("--volumes applies only when removing a whole container family, not %q", value)
				}
				target, err := parseSelector(value)
				if err != nil {
					return fmt.Errorf("invalid target %q: %w", value, err)
				}
				if !removed[target] {
					if err := backend.destroy(target, *force); err != nil {
						return err
					}
				}
				fmt.Fprintln(stdout, target)
				return nil
			}
			if err := backend.remove(value, *force, *volumes); err != nil {
				return err
			}
			fmt.Fprintln(stdout, value)
			return nil
		})
	}
	if runtime, ok := backend.(*runtimeBackend); ok && fs.NArg() > 1 {
		return runtime.removingBranches(func() error {
			var removed map[selector]bool
			if !*volumes {
				var targets []selector
				for _, value := range fs.Args() {
					if target, err := parseSelector(value); err == nil && strings.Contains(value, "@") {
						targets = append(targets, target)
					}
				}
				var err error
				if removed, err = runtime.removeWholeContainers(targets, *force); err != nil {
					return err
				}
			}
			return removeAll(removed)
		})
	}
	return removeAll(nil)
}

// removeWholeContainers removes, each at once, the containers of targets whose
// branches the targets all name, when no branch of the container's family is
// placed anywhere else, and returns the targets it removed with them. A
// cluster removes a family branch by branch, and removing a branch that is not
// its container's last collected its filesystem in its runtime first, 4 s for
// a branch of 100,000 files, only for the container's storage to go whole with
// its last branch.
func (b *runtimeBackend) removeWholeContainers(targets []selector, force bool) (map[selector]bool, error) {
	if b.dataRoot == "" {
		return nil, nil
	}
	byContainer := make(map[string]map[string]selector)
	var containers []string
	for _, target := range targets {
		branches := byContainer[target.container]
		if branches == nil {
			branches = make(map[string]selector)
			byContainer[target.container] = branches
			containers = append(containers, target.container)
		}
		branches[target.branch] = target
	}
	removed := make(map[selector]bool)
	for _, container := range containers {
		branches := byContainer[container]
		info, err := b.containerStore().LoadContainer(container)
		if err != nil || len(branches) < 2 || len(info.Branches) != len(branches) ||
			info.IsDistributedPhysicalRuntime() || logicalFamilyID(info) != info.ID {
			continue
		}
		// The command changes this family: a cluster node reconciles its
		// placements after, and every family's if it resolved none.
		b.resolvedFamilies.resolved(container, info)
		named := true
		for _, branch := range info.Branches {
			if _, ok := branches[branch.ID]; !ok {
				named = false
				break
			}
		}
		if !named {
			continue
		}
		if placed, err := b.familyPlacedBeyond(info.ID, branches); err != nil {
			return removed, err
		} else if placed {
			continue
		}
		if err := b.remove(info.ID, force, false); err != nil {
			return removed, err
		}
		for branch, target := range branches {
			if err := b.removeLogicalBranchPlacement(info.ID, branch); err != nil {
				return removed, err
			}
			removed[target] = true
		}
	}
	return removed, nil
}

// familyPlacedBeyond reports whether a placement of the family names a branch
// that branches does not.
func (b *runtimeBackend) familyPlacedBeyond(familyID string, branches map[string]selector) (bool, error) {
	if b.nodeID == "" {
		return false, nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return false, err
	}
	records, err := placements.ListFamily(familyID)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if _, ok := branches[record.BranchID]; !ok {
			return true, nil
		}
	}
	return false, nil
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
	format, identifiers, err := parseImageInspectArgs(args)
	if err != nil {
		return err
	}
	if len(identifiers) == 0 {
		return errors.New("usage: ttc image inspect [--format TEMPLATE] IMAGE [IMAGE...]")
	}
	results := make([]ImageInspectInfo, 0, len(identifiers))
	for _, identifier := range identifiers {
		info, err := backend.inspectImage(identifier)
		if err != nil {
			return err
		}
		results = append(results, info)
	}
	if format == "" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(results)
	}
	return writeImageInspectFormat(stdout, results, format)
}

func parseImageInspectArgs(args []string) (string, []string, error) {
	format := ""
	identifiers := make([]string, 0, len(args))
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}
		if options {
			switch {
			case arg == "--format" || arg == "-f":
				if i+1 >= len(args) {
					return "", nil, fmt.Errorf("%s requires a template", arg)
				}
				i++
				format = args[i]
				continue
			case strings.HasPrefix(arg, "--format="):
				format = strings.TrimPrefix(arg, "--format=")
				continue
			case strings.HasPrefix(arg, "-f="):
				format = strings.TrimPrefix(arg, "-f=")
				continue
			case strings.HasPrefix(arg, "-"):
				return "", nil, fmt.Errorf("unknown image inspect option %q", arg)
			}
		}
		identifiers = append(identifiers, arg)
	}
	return format, identifiers, nil
}

func writeImageInspectFormat(w io.Writer, results []ImageInspectInfo, format string) error {
	if format == "json" {
		encoder := json.NewEncoder(w)
		for _, result := range results {
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
		return nil
	}
	tmpl, err := template.New("image-inspect").Funcs(template.FuncMap{
		"json": func(value any) (string, error) {
			data, err := marshalInspectTemplateJSON(value)
			return string(data), err
		},
		"join": strings.Join, "lower": strings.ToLower, "split": strings.Split, "upper": strings.ToUpper,
	}).Parse(format)
	if err != nil {
		return fmt.Errorf("parsing image inspect format: %w", err)
	}
	for _, result := range results {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var values map[string]any
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		var output bytes.Buffer
		if err := tmpl.Execute(&output, values); err != nil {
			return fmt.Errorf("executing image inspect format: %w", err)
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
	return eachTarget(fs.Args(), stderr, func(identifier string) error {
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
		return nil
	})
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
	writable := make([]uint64, len(rows))
	known := make([]bool, len(rows))
	// A branch another node runs is sized there: its Sentry is that node's.
	// Each node is asked once for each family's.
	type familyOnNode struct{ node, family string }
	elsewhere := make(map[familyOnNode][]int)
	var askOrder []familyOnNode
	for i := range rows {
		row := &rows[i]
		known[i] = row.State == "created"
		if row.State != "running" && row.State != "paused" {
			continue
		}
		size, err := backend.rootfsSize(selector{container: row.Container, branch: row.Branch})
		var remote *remoteBranchOwnershipError
		if errors.As(err, &remote) {
			key := familyOnNode{node: remote.owner, family: row.Container}
			if _, ok := elsewhere[key]; !ok {
				askOrder = append(askOrder, key)
			}
			elsewhere[key] = append(elsewhere[key], i)
			continue
		}
		if err != nil {
			return fmt.Errorf("getting size for %s: %w", rows[i].ID, err)
		}
		writable[i], known[i] = size, true
	}
	if len(askOrder) != 0 {
		sizer, ok := backend.(interface {
			remoteRootfsSizes(node, family string, branches []string) (map[string]branchRootfsSize, error)
		})
		if !ok {
			return errors.New("this backend cannot size branches another node runs")
		}
		for _, key := range askOrder {
			branches := make([]string, len(elsewhere[key]))
			for j, i := range elsewhere[key] {
				branches[j] = rows[i].Branch
			}
			sizes, err := sizer.remoteRootfsSizes(key.node, key.family, branches)
			if err != nil {
				return fmt.Errorf("getting sizes on node %q: %w", key.node, err)
			}
			for _, i := range elsewhere[key] {
				size, ok := sizes[rows[i].Branch]
				switch {
				case !ok:
					return fmt.Errorf("getting size for %s: node %q did not size it", rows[i].ID, key.node)
				case size.Error != "":
					return fmt.Errorf("getting size for %s: %s", rows[i].ID, size.Error)
				}
				writable[i], known[i] = size.WritableBytes, true
			}
		}
	}
	for i := range rows {
		row := &rows[i]
		imageSize := uint64(0)
		if row.imageSize > 0 {
			imageSize = uint64(row.imageSize)
		}
		row.SizeRootFS = imageSize + writable[i]
		if known[i] {
			row.SizeRW = writable[i]
			row.Size = fmt.Sprintf("%s (virtual %s)", formatImageSize(int64(writable[i])), formatImageSize(int64(row.SizeRootFS)))
		} else {
			row.Size = fmt.Sprintf("- (virtual %s)", formatImageSize(int64(imageSize)))
		}
	}
	return nil
}

// branchRootfsSize is a branch's writable size as the node that runs it
// sizes it for a listing on another, or why it could not.
type branchRootfsSize struct {
	WritableBytes uint64 `json:"writable_bytes"`
	Error         string `json:"error,omitempty"`
}

// runRootfsSizesLocal prints, as JSON by branch, the writable size of each of
// a family's branches that this node runs, for a listing on another node.
func runRootfsSizesLocal(args []string, stdout io.Writer, backend backend) error {
	if len(args) < 2 {
		return errors.New("usage: ttc _rootfs-sizes-local CONTAINER BRANCH [BRANCH ...]")
	}
	sizes := make(map[string]branchRootfsSize, len(args)-1)
	for _, branch := range args[1:] {
		size, err := backend.rootfsSize(selector{container: args[0], branch: branch})
		entry := branchRootfsSize{WritableBytes: size}
		if err != nil {
			entry.Error = err.Error()
		}
		sizes[branch] = entry
	}
	return json.NewEncoder(stdout).Encode(sizes)
}

// remoteRootfsSizes asks node for the writable sizes of family's branches.
func (b *runtimeBackend) remoteRootfsSizes(node, family string, branches []string) (map[string]branchRootfsSize, error) {
	if b.distributedSource == nil {
		return nil, errors.New("this daemon is not a cluster node")
	}
	peer, err := b.distributedSource.peer(node, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteProcessesTimeout)
	defer cancel()
	status, stdout, stderr, err := executeRemoteDaemonRequest(ctx, peer, daemonRequest{
		Version: daemonAPIVersion, Action: "command", Args: append([]string{"_rootfs-sizes-local", family}, branches...), DistributedLeaf: true,
	})
	if err != nil {
		return nil, err
	}
	if status != 0 {
		return nil, fmt.Errorf("status %d: %s", status, strings.TrimSpace(stderr))
	}
	var sizes map[string]branchRootfsSize
	if err := json.Unmarshal([]byte(stdout), &sizes); err != nil {
		return nil, fmt.Errorf("decoding the sizes: %w", err)
	}
	return sizes, nil
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
				if branch.ExitStatus != nil {
					status = fmt.Sprintf("Exited (%d)", *branch.ExitStatus)
				}
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
	targets, _, err := inspectBranchOperationTargets(value, backend, allowFamily)
	return targets, err
}

// inspectBranchOperationTargets is branchOperationTargets, with the
// inspection of the family that it read the targets from: what else a
// command needs of the family, as a stop its policy, inspecting it again was
// all of an inspect's reads again, of its record, its runtime and the
// processes of its branches.
func inspectBranchOperationTargets(value string, backend backend, allowFamily bool) ([]branchOperationTarget, InspectInfo, error) {
	inspect := backend.inspect
	if states, ok := backend.(interface {
		inspectStates(string) (InspectInfo, error)
	}); ok {
		inspect = states.inspectStates
	}
	if strings.Contains(value, "@") {
		target, err := parseSelector(value)
		if err != nil {
			return nil, InspectInfo{}, err
		}
		info, err := inspect(target.container)
		if err != nil {
			return nil, InspectInfo{}, err
		}
		for _, branch := range info.TTC.Branches {
			if branch.Metadata.ID == target.branch {
				return []branchOperationTarget{{selector: target, state: normalizeLifecycleState(branch.Metadata.State)}}, info, nil
			}
		}
		return nil, InspectInfo{}, fmt.Errorf("branch %q does not exist", target.branch)
	}
	if !allowFamily {
		return nil, InspectInfo{}, fmt.Errorf("branch selector %q must be CONTAINER@BRANCH", value)
	}
	info, err := inspect(value)
	if err != nil {
		return nil, InspectInfo{}, err
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
	return targets, info, nil
}

func runPauseResume(args []string, stdout, stderr io.Writer, backend backend, pause bool) error {
	if len(args) == 0 {
		if pause {
			return errors.New("usage: ttc pause TARGET [TARGET...]")
		}
		return errors.New("usage: ttc resume TARGET [TARGET...]")
	}
	return eachTarget(args, stderr, func(arg string) error {
		if !strings.Contains(arg, "@") {
			// A family pauses its running branches and resumes its paused
			// ones, and leaves the rest as they are.
			targets, err := branchOperationTargets(arg, backend, true)
			if err != nil {
				return fmt.Errorf("invalid target %q: %w", arg, err)
			}
			for _, target := range targets {
				switch {
				case pause && target.state == "running":
					err = backend.pause(target.selector)
				case !pause && target.state == "paused":
					err = backend.resume(target.selector)
				default:
					continue
				}
				if err != nil {
					return fmt.Errorf("updating %s: %w", target.selector, err)
				}
			}
			fmt.Fprintln(stdout, arg)
			return nil
		}
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
		return nil
	})
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
		return errors.New("usage: ttc kill [--signal SIGNAL] CONTAINER@BRANCH [CONTAINER@BRANCH...] | ttc kill CONTAINER@BRANCH SIGNAL")
	}
	signal, err := parseSignal(signalName)
	if err != nil {
		return err
	}
	return eachTarget(targets, stderr, func(arg string) error {
		target, err := parseSelector(arg)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", arg, err)
		}
		if err := backend.signal(target, signal, *all); err != nil {
			return fmt.Errorf("signaling %s: %w", target, err)
		}
		fmt.Fprintln(stdout, target)
		return nil
	})
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
	// Every target is resolved first: the running branches of all are sent
	// their stop signals before any is waited for, as docker stop's
	// containers stop together. A branch named twice is stopped once. A
	// target that cannot be stopped does not keep the others from stopping.
	var groups []stopGroup
	named := make(map[selector]bool)
	resolve := func(value string) (stopGroup, error) {
		targets, info, err := inspectBranchOperationTargets(value, backend, true)
		if err != nil {
			return stopGroup{}, fmt.Errorf("invalid target %q: %w", value, err)
		}
		if len(targets) == 0 {
			return stopGroup{}, fmt.Errorf("container %q has no branches", value)
		}
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return stopGroup{}, err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		group := stopGroup{signal: stopSignal, timeoutSeconds: stopTimeout, stopped: func() { fmt.Fprintln(stdout, value) }}
		for _, target := range targets {
			if !named[target.selector] {
				named[target.selector] = true
				group.targets = append(group.targets, target)
			}
		}
		return group, nil
	}
	resolveErr := eachTarget(fs.Args(), stderr, func(value string) error {
		group, err := resolve(value)
		if err != nil {
			return err
		}
		groups = append(groups, group)
		return nil
	})
	if err := stopGroupsLifecycle(groups, backend); err != nil {
		if resolveErr != nil {
			reportTargetError(stderr, resolveErr)
		}
		return err
	}
	return resolveErr
}

// stopGroup is the branches one target of a stop names, with its stop
// policy, and what to do once they are stopped.
type stopGroup struct {
	targets        []branchOperationTarget
	signal         int
	timeoutSeconds int
	stopped        func()
}

// stopGroupsLifecycle stops the branches of groups, in order, as
// stopBranchLifecycle stops each, with one grace period for each group's
// running branches: every running branch of every group is sent its stop
// signal before any branch is stopped, and is waited for until its group's
// period, which begins once the last is signaled, ends. Stopped one after
// another, each branch whose init ignores the signal, as an init does that
// does not handle it, held the stop for a period of its own.
func stopGroupsLifecycle(groups []stopGroup, backend backend) error {
	running := func(target branchOperationTarget) bool {
		return target.state == "running" || target.state == "restarting"
	}
	// stops holds the running branches signaled, nil for one that needed
	// nothing more.
	stops := make(map[selector]*runningStop)
	var err error
signal:
	for _, group := range groups {
		for _, target := range group.targets {
			if !running(target) {
				continue
			}
			var stop *runningStop
			if stop, err = beginRunningStop(target, group.signal, backend, true); err != nil {
				break signal
			}
			stops[target.selector] = stop
		}
	}
	deadlines := make([]time.Time, len(groups))
	for index, group := range groups {
		deadlines[index] = stopDeadline(group.timeoutSeconds)
	}
	for index, group := range groups {
		for _, target := range group.targets {
			// A branch signaled is stopped even after another failed.
			if stop, signaled := stops[target.selector]; signaled {
				if stop != nil {
					err = errors.Join(err, stop.finish(deadlines[index], backend))
				}
				continue
			}
			if err == nil && !running(target) {
				err = stopBranchLifecycle(target, group.signal, group.timeoutSeconds, backend, true)
			}
		}
		if err == nil {
			group.stopped()
		}
	}
	return err
}

// stopDeadline is when a grace period of timeoutSeconds that begins now
// ends: never, for -1.
func stopDeadline(timeoutSeconds int) time.Time {
	if timeoutSeconds == -1 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
}

// queueStopPublication queues the durable publication of a stopped branch's
// filesystem, if publish.
func queueStopPublication(target branchOperationTarget, backend backend, publish bool) error {
	if !publish {
		return nil
	}
	if publisher, ok := backend.(interface{ queueBranchPublication(selector, string) error }); ok {
		if err := publisher.queueBranchPublication(target.selector, "stop"); err != nil {
			return fmt.Errorf("queueing durable filesystem publication for %s: %w", target.selector, err)
		}
	}
	return nil
}

// runningStop is a running branch being stopped, sent its stop signal.
type runningStop struct {
	target  branchOperationTarget
	publish bool
	// exited is whether the branch had exited when it was signaled.
	exited             bool
	finishExplicitStop func(initTerminated bool)
}

// beginRunningStop prepares the running branch target to stop and sends it
// stopSignal. It returns nil if target needs nothing more: it had exited
// already, and was released.
func beginRunningStop(target branchOperationTarget, stopSignal int, backend backend, publish bool) (*runningStop, error) {
	stop := &runningStop{target: target, publish: publish, finishExplicitStop: func(bool) {}}
	if coordinator, ok := backend.(interface {
		beginExplicitInitStop(selector) (func(bool), error)
	}); ok {
		finish, err := coordinator.beginExplicitInitStop(target.selector)
		if err != nil {
			return nil, fmt.Errorf("coordinating explicit stop of %s: %w", target.selector, err)
		}
		stop.finishExplicitStop = finish
	}
	if err := backend.prepareStop(target.selector); err != nil {
		refreshed, refreshErr := branchOperationTargets(target.selector.String(), backend, false)
		if refreshErr == nil && len(refreshed) == 1 && refreshed[0].state == "exited" {
			stop.finishExplicitStop(true)
			if discardErr := backend.discard(target.selector); discardErr == nil {
				return nil, queueStopPublication(target, backend, publish)
			}
			return nil, fmt.Errorf("preparing %s for independent stop: %w", target.selector, err)
		}
		stop.finishExplicitStop(false)
		return nil, fmt.Errorf("preparing %s for independent stop: %w", target.selector, err)
	}
	exited, err := signalStop(target.selector, stopSignal, backend)
	if err != nil {
		stop.finishExplicitStop(false)
		return nil, err
	}
	stop.exited = exited
	return stop, nil
}

// finish waits for the branch to exit, killing it at deadline if it is not
// zero, and releases it.
func (stop *runningStop) finish(deadline time.Time, backend backend) error {
	target := stop.target
	initTerminated := false
	defer func() { stop.finishExplicitStop(initTerminated) }()
	// The grace period lasts as long as the stop policy allows, and other
	// commands go on meanwhile; a kill of this branch too.
	if !stop.exited {
		if err := withoutLifecycleLock(backend, target.selector, longOperation{branch: target.selector.branch, state: "stopping"}, func() error {
			return awaitStop(target.selector, deadline, backend)
		}); err != nil {
			// The final physical branch on an elastic node tears down its
			// control socket as the init exits. If reconciliation confirms the
			// requested terminal state, treat the lost wait reply as a
			// committed stop.
			refreshed, refreshErr := branchOperationTargets(target.selector.String(), backend, false)
			if refreshErr == nil && len(refreshed) == 1 && refreshed[0].state == "exited" {
				initTerminated = true
				if discardErr := backend.discard(target.selector); discardErr == nil {
					return queueStopPublication(target, backend, stop.publish)
				}
			}
			return err
		}
	}
	initTerminated = true
	if err := backend.discard(target.selector); err != nil {
		return fmt.Errorf("releasing stopped execution state for %s: %w", target.selector, err)
	}
	return queueStopPublication(target, backend, stop.publish)
}

func stopBranchLifecycle(target branchOperationTarget, stopSignal, timeoutSeconds int, backend backend, publish bool) error {
	queueExit := func() error {
		return queueStopPublication(target, backend, publish)
	}
	switch target.state {
	case "running", "restarting":
		stop, err := beginRunningStop(target, stopSignal, backend, publish)
		if err != nil || stop == nil {
			return err
		}
		return stop.finish(stopDeadline(timeoutSeconds), backend)
	case "paused":
		if err := backend.preparePausedStop(target.selector); err != nil {
			return fmt.Errorf("persisting paused filesystem state for %s: %w", target.selector, err)
		}
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("discarding execution state for %s: %w", target.selector, err)
		}
		return queueExit()
	case "dehydrating":
		// The in-flight dehydration publication owns the newest sealed filesystem
		// generation. Canceling it would remove that hidden branch and make stop
		// publish an older working generation. Let the immutable capture reach its
		// durability boundary first, then discard only its memory roots through the
		// ordinary dehydrated-stop path. The wait gives the lifecycle lock up, as
		// a running branch's stop does while its init exits: a publication that
		// could not complete held a stop, and with it the lock, for good, and
		// every command of the family on the node behind them.
		// The wait is bounded as a remote fork's of the branch is, rather
		// than hold the command for as long as the publication retries.
		var result waitResult
		if err := withoutLifecycleLock(backend, target.selector, longOperation{branch: target.selector.branch, state: "stopping"}, func() error {
			result = waitForDurableBranchUntil(target.selector, backend, time.Now().Add(sharedDehydrationWait))
			return nil
		}); err != nil {
			return fmt.Errorf("waiting for dehydration of %s: %w", target.selector, err)
		}
		if result.err != nil {
			// A start or a rehydration on another node took the branch while
			// the stop waited, and failed it, "... is running: only a stopped
			// or dehydrated branch has durable state to wait for": the stop,
			// which has done nothing, goes to stop the branch there, sent on
			// by its daemon (forwardMovedCommand).
			if mover, ok := backend.(movedBranchBackend); ok {
				mover.branchMovedAway(target.selector)
			}
			return fmt.Errorf("waiting for dehydration of %s: %w", target.selector, result.err)
		}
		target.state = "dehydrated"
		return stopBranchLifecycle(target, stopSignal, timeoutSeconds, backend, publish)
	case "dehydrated":
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("discarding execution state for %s: %w", target.selector, err)
		}
		if authority, ok := backend.(coldStopAuthorityBackend); ok {
			handled, err := authority.finalizeColdStopAuthority(target.selector, target.state)
			if err != nil {
				return err
			}
			if handled {
				return nil
			}
		}
		return queueExit()
	case "exited":
		if err := backend.discard(target.selector); err != nil {
			return fmt.Errorf("releasing exited execution state for %s: %w", target.selector, err)
		}
		if authority, ok := backend.(coldStopAuthorityBackend); ok {
			handled, err := authority.finalizeColdStopAuthority(target.selector, target.state)
			if err != nil {
				return err
			}
			if handled {
				return nil
			}
		}
		return queueExit()
	case "created":
		// A prepared branch has no runtime to terminate, but its writable
		// repository is still authoritative on this node. Publishing it makes a
		// later start independent of this node and lets graceful retirement
		// discard node-local scratch safely.
		return queueExit()
	case "failed", "dead":
		return nil
	default:
		return fmt.Errorf("cannot stop %s in state %q", target.selector, target.state)
	}
}

func runWait(args []string, stdout, stderr io.Writer, backend backend) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	condition := fs.String("condition", "not-running", "wait condition (not-running or durable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc wait [--condition not-running|durable] CONTAINER@BRANCH [CONTAINER@BRANCH...]")
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

type residentDehydrationBackend interface {
	persistResidentDehydration(selector) error
}

func waitForDurableBranch(target selector, backend backend) waitResult {
	return waitForDurableBranchUntil(target, backend, time.Time{})
}

// waitForDurableBranchUntil is waitForDurableBranch, which gives up at
// deadline, unless it is zero.
func waitForDurableBranchUntil(target selector, backend backend, deadline time.Time) waitResult {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return waitResult{err: fmt.Errorf("%s is not durable yet: its storage publication is still under way", target)}
		}
		// runtimeBackend implements both interfaces in every mode. Select the
		// cluster authority only for clustered storage; in standalone mode TTC_ROOT is
		// the authoritative store and a local capture is already durable. Merely
		// testing the authority interface here made standalone waits poll a
		// deliberately inapplicable method forever.
		localAuthoritative := false
		if durability, ok := backend.(localDurabilityBackend); ok {
			localAuthoritative = durability.localStorageIsDurable()
		}
		if authority, ok := backend.(authoritativeDurabilityBackend); ok && !localAuthoritative {
			durable, err := authority.authoritativeBranchDurable(target)
			if err != nil {
				return waitResult{err: err}
			}
			if durable {
				return waitResult{}
			}
			// A clustered backend's authority catalog is the complete lifecycle and
			// publication authority. Its persistent family view may deliberately
			// omit a transaction-scoped physical branch while publication is in
			// progress, so consulting inspect here can turn a normal pending state
			// into a spurious "branch does not exist" failure.
			<-ticker.C
			continue
		}
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
			// A live branch has no captured state for durability to describe,
			// so waiting for it would wait for ever.
			switch state := normalizeLifecycleState(branch.State); state {
			case "running", "paused":
				return waitResult{err: fmt.Errorf("%s is %s: only a stopped or dehydrated branch has durable state to wait for", target, state)}
			}
			switch branch.StorageState {
			case ttcbundle.StorageResident:
				// Nothing makes a resident capture durable until its family's
				// last live branch goes, so asking for durability does.
				if persister, ok := backend.(residentDehydrationBackend); ok {
					if err := persister.persistResidentDehydration(target); err != nil {
						return waitResult{err: err}
					}
				}
			case "shared":
				return waitResult{}
			case "local":
				// Standalone TTC has no cache tier: TTC_ROOT is authoritative,
				// so a local stopped/dehydrated capture is already durable.
				// Clustered local state remains node-authoritative and must wait
				// for publication to the shared root.
				if localAuthoritative {
					return waitResult{}
				}
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
	// Restart already escalates to SIGKILL after the configured timeout. Accept
	// Docker/TTC's historical --force spelling as an explicit acknowledgement
	// of that behavior.
	fs.Bool("force", false, "force termination after the graceful timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: ttc restart [--force] [--time SECONDS] (CONTAINER|CONTAINER@BRANCH) [(CONTAINER|CONTAINER@BRANCH)...]")
	}
	timeoutSet := flagWasSet(fs, "time") || flagWasSet(fs, "timeout") || flagWasSet(fs, "t")
	if timeoutSet && timeout < -1 {
		return errors.New("restart timeout must be -1 or greater")
	}
	return eachTarget(fs.Args(), stderr, func(value string) error {
		selected, err := parseSelectorOrMain(value)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		targets, info, err := inspectBranchOperationTargets(selected.String(), backend, false)
		if err != nil {
			return fmt.Errorf("invalid target %q: %w", value, err)
		}
		target := targets[0]
		stopSignal, stopTimeout, err := stopPolicy(info)
		if err != nil {
			return err
		}
		if timeoutSet {
			stopTimeout = timeout
		}
		switch target.state {
		case "running", "restarting", "paused", "dehydrated":
			// Restart consumes the stopped state immediately on the same node.
			// Publishing that transient state introduces an unnecessary race in
			// which the asynchronous publication can unplace the branch between
			// stop and the fresh start.
			if err := stopBranchLifecycle(target, stopSignal, stopTimeout, backend, false); err != nil {
				return err
			}
		case "created", "exited", "failed", "dead", "materializing":
			// Cluster admission claims an unplaced dehydrated branch as
			// materializing after converting it to an exited filesystem-only
			// snapshot. The node-local record is ready for the fresh start below.
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
		fmt.Fprintln(stdout, value)
		return nil
	})
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

// signalStop sends target stopSignal. It reports whether target had exited
// already.
func signalStop(target selector, stopSignal int, backend backend) (bool, error) {
	if err := backend.signal(target, stopSignal, false); err != nil {
		if _, waitErr := backend.wait(target); waitErr == nil {
			return true, nil
		}
		return false, fmt.Errorf("stopping %s with signal %d: %w", target, stopSignal, err)
	}
	return false, nil
}

// awaitStop waits for target, sent its stop signal, to exit, and kills it at
// deadline, unless that is zero.
func awaitStop(target selector, deadline time.Time, backend backend) error {
	const signalKill = 9
	waited := make(chan waitResult, 1)
	go func() {
		status, err := backend.wait(target)
		waited <- waitResult{status: status, err: err}
	}()
	if deadline.IsZero() {
		result := <-waited
		if result.err != nil {
			return fmt.Errorf("waiting for %s: %w", target, result.err)
		}
		return nil
	}
	timer := time.NewTimer(time.Until(deadline))
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
	if signal, err := strconv.Atoi(value); err == nil {
		if signal >= 1 && signal <= 64 {
			return signal, nil
		}
		return 0, fmt.Errorf("unknown signal %q", value)
	}
	name := strings.ToUpper(value)
	name = strings.TrimPrefix(name, "SIG")
	if signal, ok := linuxSignalNames[name]; ok {
		return int(signal), nil
	}
	return 0, fmt.Errorf("unknown signal %q", value)
}

// linuxSignalNames is deliberately independent of GOOS. TTC controls Linux
// guest processes even when its host process runs on Darwin, whose SIGUSR1,
// SIGUSR2, and several other signal numbers differ from Linux.
var linuxSignalNames = map[string]linux.Signal{
	"HUP": linux.SIGHUP, "INT": linux.SIGINT, "QUIT": linux.SIGQUIT,
	"ILL": linux.SIGILL, "TRAP": linux.SIGTRAP, "ABRT": linux.SIGABRT,
	"IOT": linux.SIGIOT, "BUS": linux.SIGBUS, "FPE": linux.SIGFPE,
	"KILL": linux.SIGKILL, "USR1": linux.SIGUSR1, "SEGV": linux.SIGSEGV,
	"USR2": linux.SIGUSR2, "PIPE": linux.SIGPIPE, "ALRM": linux.SIGALRM,
	"TERM": linux.SIGTERM, "STKFLT": linux.SIGSTKFLT, "CHLD": linux.SIGCHLD,
	"CLD": linux.SIGCLD, "CONT": linux.SIGCONT, "STOP": linux.SIGSTOP,
	"TSTP": linux.SIGTSTP, "TTIN": linux.SIGTTIN, "TTOU": linux.SIGTTOU,
	"URG": linux.SIGURG, "XCPU": linux.SIGXCPU, "XFSZ": linux.SIGXFSZ,
	"VTALRM": linux.SIGVTALRM, "PROF": linux.SIGPROF, "WINCH": linux.SIGWINCH,
	"IO": linux.SIGIO, "POLL": linux.SIGPOLL, "PWR": linux.SIGPWR,
	"SYS": linux.SIGSYS, "UNUSED": linux.SIGUNUSED,
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

// remoteBranchOwnershipError is the expected result of observing a stale
// local physical runtime after clustered placement has moved elsewhere. User
// commands still receive the ordinary diagnostic text, while asynchronous
// console-exit reconciliation can distinguish this benign cutover from a real
// finalization failure without parsing strings.
type remoteBranchOwnershipError struct {
	target selector
	owner  string
}

func (e *remoteBranchOwnershipError) Error() string {
	return fmt.Sprintf("branch %s belongs to cluster node %q", e.target, e.owner)
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

// parseSelectorOrMain preserves Docker's main-container lifecycle surface for
// commands such as `ttc start ID`, while parseSelector remains strict for TTC
// operations that must identify one branch explicitly.
func parseSelectorOrMain(value string) (selector, error) {
	if strings.Contains(value, "@") {
		return parseSelector(value)
	}
	if value == "" {
		return selector{}, errors.New("container name or ID must not be empty")
	}
	return selector{container: value, branch: defaultBranchID}, nil
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
	ID                          string    `json:"id"`
	Quarantined                 bool      `json:"quarantined,omitempty"`
	NodeID                      string    `json:"node_id,omitempty"`
	ParentID                    string    `json:"parent,omitempty"`
	State                       string    `json:"state"`
	ExitStatus                  *int      `json:"exit_status,omitempty"`
	StorageState                string    `json:"storage_state"`
	PublicationID               string    `json:"publication_id,omitempty"`
	StorageError                string    `json:"storage_error,omitempty"`
	StorageManifest             string    `json:"storage_manifest,omitempty"`
	StorageRetryAt              time.Time `json:"storage_retry_at,omitempty"`
	SnapshotID                  uint64    `json:"snapshot_id,omitempty"`
	MemoryRoots                 []string  `json:"memory_roots,omitempty"`
	MemoryRootObjects           []string  `json:"memory_root_objects,omitempty"`
	ControlRoot                 string    `json:"control_root,omitempty"`
	ControlRootObject           string    `json:"control_root_object,omitempty"`
	SourceNode                  string    `json:"source_node,omitempty"`
	DependentMemoryBytes        uint64    `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes    uint64    `json:"dependent_filesystem_bytes,omitempty"`
	ForkTotalNS                 int64     `json:"fork_total_ns,omitempty"`
	SourceDowntimeNS            int64     `json:"source_downtime_ns,omitempty"`
	KernelForkNS                int64     `json:"kernel_fork_ns,omitempty"`
	ForkValidationNS            int64     `json:"fork_validation_ns,omitempty"`
	ForkSharedMemoryNS          int64     `json:"fork_shared_memory_ns,omitempty"`
	ForkMappingInvalidationNS   int64     `json:"fork_mapping_invalidation_ns,omitempty"`
	ForkVFSPreparationNS        int64     `json:"fork_vfs_preparation_ns,omitempty"`
	ForkDevicePreparationNS     int64     `json:"fork_device_preparation_ns,omitempty"`
	ForkFSMetadataFlushNS       int64     `json:"fork_fs_metadata_flush_ns,omitempty"`
	ForkFSRuntimeInvalidationNS int64     `json:"fork_fs_runtime_invalidation_ns,omitempty"`
	ForkFSBranchSealNS          int64     `json:"fork_fs_branch_seal_ns,omitempty"`
	ForkFSNamespacePruneNS      int64     `json:"fork_fs_namespace_prune_ns,omitempty"`
	ForkMemoryPreparationNS     int64     `json:"fork_memory_preparation_ns,omitempty"`
	ForkMetadataStreamNS        int64     `json:"fork_metadata_stream_ns,omitempty"`
	ForkMetadataBytes           int64     `json:"fork_metadata_bytes,omitempty"`
	ForkMetadataRecordNS        int64     `json:"fork_metadata_record_ns,omitempty"`
	ForkMetadataObjects         int64     `json:"fork_metadata_objects,omitempty"`
	ForkChildRestoreNS          int64     `json:"fork_child_restore_ns,omitempty"`
	CreatedAt                   time.Time `json:"created_at"`
	InitPID                     int32     `json:"init_pid,omitempty"`
	InitState                   string    `json:"init_state,omitempty"`
	MemorySharing               string    `json:"memory_sharing,omitempty"`
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
	LocalCache   *ImageCacheInfo   `json:"local_cache,omitempty"`
	Containers   []string          `json:"containers,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ImageCacheInfo describes a clustered node's disposable, whole-file image
// cache. Total is the logical fully-populated size; Complete is immediately
// readable without another shared-pack range fetch.
type ImageCacheInfo struct {
	CompleteFiles uint64 `json:"complete_files"`
	TotalFiles    uint64 `json:"total_files"`
	CompleteBytes uint64 `json:"complete_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`
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
	LocalCache   *ImageCacheInfo    `json:"LocalCache,omitempty"`
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
	NodeID                   string        `json:"node_id,omitempty"`
	Quarantined              bool          `json:"quarantined,omitempty"`
	StorageState             string        `json:"storage_state"`
	PublicationID            string        `json:"publication_id,omitempty"`
	StorageError             string        `json:"storage_error,omitempty"`
	StorageRetryAt           time.Time     `json:"storage_retry_at,omitempty"`
	SnapshotID               uint64        `json:"snapshot_id,omitempty"`
	MemoryRoots              []string      `json:"memory_roots,omitempty"`
	MemoryRootObjects        []string      `json:"memory_root_objects,omitempty"`
	ControlRoot              string        `json:"control_root,omitempty"`
	ControlRootObject        string        `json:"control_root_object,omitempty"`
	FilesystemRoot           string        `json:"filesystem_root,omitempty"`
	SourceNode               string        `json:"source_node,omitempty"`
	DependentMemoryBytes     uint64        `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes uint64        `json:"dependent_filesystem_bytes,omitempty"`
	ResidentMemory           ResourceBytes `json:"resident_memory"`
	MemoryBacking            ResourceBytes `json:"memory_backing"`
	Filesystem               ResourceBytes `json:"filesystem"`
	FilesystemInodes         uint64        `json:"filesystem_inodes"`
}

// SnapshotGraphResourceStats is a physical-path-free node-local diagnostic.
// FamilyReachableObjects and PendingReferenceOperations are scoped to the
// reported family; the remaining counts describe the serving node's graph.
type SnapshotGraphResourceStats struct {
	Sequence                   uint64 `json:"sequence"`
	Snapshots                  int    `json:"snapshots"`
	Objects                    int    `json:"objects"`
	Roots                      int    `json:"roots"`
	Pins                       int    `json:"pins"`
	Reclaiming                 int    `json:"reclaiming"`
	JournalBytes               int64  `json:"journal_bytes"`
	FamilyReachableObjects     int    `json:"family_reachable_objects"`
	PendingReferenceOperations int    `json:"pending_reference_operations"`
}

// FamilyResourceStats includes branch attribution and non-double-counted totals.
type FamilyResourceStats struct {
	Container                 string                      `json:"container"`
	Branches                  []BranchResourceStats       `json:"branches"`
	ResidentMemoryUniqueBytes uint64                      `json:"resident_memory_unique_bytes"`
	MemoryBackingUniqueBytes  uint64                      `json:"memory_backing_unique_bytes"`
	FilesystemUniqueBytes     uint64                      `json:"filesystem_unique_bytes"`
	DependentMemoryBytes      uint64                      `json:"dependent_memory_bytes,omitempty"`
	DependentFilesystemBytes  uint64                      `json:"dependent_filesystem_bytes,omitempty"`
	ImageCacheBytes           uint64                      `json:"image_cache_bytes,omitempty"`
	ImageCacheTotalBytes      uint64                      `json:"image_cache_total_bytes,omitempty"`
	SnapshotGraph             *SnapshotGraphResourceStats `json:"snapshot_graph,omitempty"`
	// ApplicationMappedBytes is how much memory the family's applications
	// have mapped on this node, and ApplicationHugePageBytes how much of it
	// the host maps with huge pages.
	ApplicationMappedBytes   uint64 `json:"application_mapped_bytes,omitempty"`
	ApplicationHugePageBytes uint64 `json:"application_huge_page_bytes,omitempty"`
}

type resourceStatsBackend interface {
	resourceStats(target selector) (FamilyResourceStats, error)
}

func runStats(args []string, stdout, stderr io.Writer, b backend) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable JSON")
	fs.Bool("no-stream", false, "one-shot output (accepted for Docker CLI compatibility; this is the default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc stats [--json] [--no-stream] CONTAINER@BRANCH")
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
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "TARGET\tSTATE\tSTORAGE\tMEM PRIVATE\tMEM SHARED\tMEM BACKING\tDISK PRIVATE\tDISK SHARED\tREMOTE DEPENDENT\tIMAGE CACHE")
	for _, branch := range stats.Branches {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t-\n", branch.Target, branch.State, branch.StorageState,
			formatImageSize(int64(branch.ResidentMemory.PrivateBytes)), formatImageSize(int64(branch.ResidentMemory.SharedBytes)),
			formatImageSize(int64(branch.MemoryBacking.TotalBytes)), formatImageSize(int64(branch.Filesystem.PrivateBytes)), formatImageSize(int64(branch.Filesystem.SharedBytes)),
			formatImageSize(int64(branch.DependentMemoryBytes+branch.DependentFilesystemBytes)))
	}
	if stats.ImageCacheTotalBytes != 0 {
		fmt.Fprintf(table, "image cache\t-\t-\t-\t-\t-\t-\t-\t-\t%s / %s\n",
			formatImageSize(int64(stats.ImageCacheBytes)), formatImageSize(int64(stats.ImageCacheTotalBytes)))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if stats.ApplicationMappedBytes != 0 {
		_, err := fmt.Fprintf(stdout, "application memory in huge pages: %s of %s (%d%%)\n",
			formatImageSize(int64(stats.ApplicationHugePageBytes)), formatImageSize(int64(stats.ApplicationMappedBytes)),
			stats.ApplicationHugePageBytes*100/stats.ApplicationMappedBytes)
		return err
	}
	return nil
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

// volumeInspectTTC is TTC's own part of a volume's inspect output. A cluster
// node names itself: its volume is not another node's of the same name.
func volumeInspectTTC(info ttcvolume.VolumeInfo, b volumeCommandBackend) map[string]any {
	result := map[string]any{"ID": info.ID, "NextInode": info.NextInode}
	if runtime, ok := b.(*runtimeBackend); ok && runtime.nodeID != "" {
		result["Node"] = runtime.nodeID
	}
	return result
}

// clusterVolumeBackend lists the named volumes of every node of a cluster.
type clusterVolumeBackend interface {
	clusterVolumes() ([]clusterVolume, bool, error)
}

// clusterVolume is a named volume and the node whose local root holds it.
type clusterVolume struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Node   string `json:"node,omitempty"`
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
		fs := flag.NewFlagSet("volume ls", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "print the volumes as a JSON array")
		local := fs.Bool("_local", false, "list only the addressed daemon's volumes, as JSON")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: ttc volume ls [--json]")
		}
		if !*local {
			if clustered, ok := volumes.(clusterVolumeBackend); ok {
				list, inCluster, err := clustered.clusterVolumes()
				if err != nil {
					return err
				}
				if inCluster {
					sort.Slice(list, func(i, j int) bool {
						return list[i].Name < list[j].Name || list[i].Name == list[j].Name && list[i].Node < list[j].Node
					})
					if *asJSON {
						return json.NewEncoder(stdout).Encode(list)
					}
					tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "DRIVER\tVOLUME NAME\tNODE")
					for _, volume := range list {
						fmt.Fprintf(tw, "%s\t%s\t%s\n", volume.Driver, volume.Name, volume.Node)
					}
					return tw.Flush()
				}
			}
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
		if *local || *asJSON {
			listing := make([]clusterVolume, 0, len(list))
			for _, info := range list {
				listing = append(listing, clusterVolume{Name: info.Name, Driver: driver})
			}
			return json.NewEncoder(stdout).Encode(listing)
		}
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
				"TTC": volumeInspectTTC(info, volumes),
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
	preparePausedStop(target selector) error
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

type localDurabilityBackend interface {
	localStorageIsDurable() bool
}

type authoritativeDurabilityBackend interface {
	authoritativeBranchDurable(selector) (bool, error)
}

type coldStopAuthorityBackend interface {
	finalizeColdStopAuthority(selector, string) (bool, error)
}

func (b *runtimeBackend) localStorageIsDurable() bool {
	return b != nil && b.distributedSource == nil
}

// finalizeColdStopAuthority avoids recapturing an already durable, unplaced
// branch. In particular, stopping a dehydrated branch only drops its memory
// root; its immutable filesystem root is already authoritative in shared
// storage and needs no physical placement or second publication.
func (b *runtimeBackend) finalizeColdStopAuthority(target selector, previousState string) (bool, error) {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return false, nil
	}
	// Resolve through the authority as well as the shared store. A publication may be
	// authoritative before NFS directory and negative-entry caches on this
	// control node expose the family by name.
	info, err := b.resolveContainer(target.container)
	if err != nil {
		return false, err
	}
	family := logicalFamilyID(info)
	branch, err := b.distributedSource.authority.Branch(family, target.branch)
	if err != nil {
		return false, err
	}
	placement, err := b.distributedSource.authority.LookupPlacement(family, target.branch)
	if err != nil {
		return false, err
	}
	if previousState == "dehydrated" {
		if placement.State != cluster.PlacementUnplaced {
			return false, errors.New("dehydrated branch is no longer unplaced")
		}
		if _, err := b.distributedSource.authority.DiscardDehydratedMemory(family, target.branch); err != nil {
			return false, fmt.Errorf("discarding durable memory authority for %s: %w", target, err)
		}
		return true, nil
	}
	if previousState == "exited" && placement.State == cluster.PlacementUnplaced && branch.Lifecycle == "exited" && branch.Roots.FilesystemRoot != "" {
		return true, nil
	}
	return false, nil
}

func (b *runtimeBackend) authoritativeBranchDurable(target selector) (bool, error) {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return false, nil
	}
	// Resolve through the authority as well as the shared store. A publication may be
	// authoritative before NFS directory and negative-entry caches on this
	// control node expose the family by name.
	info, err := b.resolveContainer(target.container)
	if err != nil {
		return false, err
	}
	family := logicalFamilyID(info)
	branch, err := b.distributedSource.authority.Branch(family, target.branch)
	if err != nil {
		return false, err
	}
	if branch.Corrupted {
		return false, fmt.Errorf("branch %s is corrupted: %s", target, branch.CorruptionReason)
	}
	if branch.Lifecycle == "running" || branch.Lifecycle == "paused" {
		return false, fmt.Errorf("%s is %s: only a stopped or dehydrated branch has durable state to wait for", target, branch.Lifecycle)
	}
	if branch.Lifecycle != "exited" && branch.Lifecycle != "dehydrated" {
		return false, nil
	}
	if branch.PublicationID == "" {
		// A stopped dehydrated branch deliberately drops its memory root and
		// terminal publication history in one authority transition. Its filesystem
		// root remains authoritative, and an unplaced generation proves that no
		// mutable runtime can still supersede it. Do not require dead transaction
		// metadata merely to report this stable state as durable.
		placement, placementErr := b.distributedSource.authority.LookupPlacement(family, target.branch)
		if placementErr != nil {
			return false, placementErr
		}
		return branch.Lifecycle == "exited" && branch.Roots.FilesystemRoot != "" &&
			branch.Roots.MemoryRoot == "" && branch.Roots.MemorySnapshot == 0 && placement.State == cluster.PlacementUnplaced, nil
	}
	publication, ok, err := b.distributedSource.authority.Publication(family, target.branch, branch.PublicationID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	switch publication.State {
	case cluster.PublicationCommitted:
		// CommitPublication is the cluster durability boundary, but the
		// provider still has a short local completion tail: it changes local
		// snapshot origins to shared authority, removes their handoff freeze,
		// and finally publishes the matching container metadata. A command that
		// immediately forks the cold branch may execute on that same provider;
		// returning before the tail completes would expose the internal frozen
		// ledger as a dynamic fork failure. CompleteBranchPublication is ordered
		// after all local handoffs, so require the exact derived identity too.
		physical, physicalBranch, resolveErr := b.resolveDistributedPersistentBranch(info, target.branch, branch.PublicationID)
		if errors.Is(resolveErr, os.ErrNotExist) {
			return false, nil
		}
		if resolveErr != nil {
			return false, resolveErr
		}
		persistent, persistentErr := persistentBranchInfo(physical, physicalBranch)
		if persistentErr != nil {
			return false, persistentErr
		}
		return persistent.PublicationID == branch.PublicationID &&
			persistent.StorageState == "shared" && persistent.StorageManifest != "", nil
	case cluster.PublicationFailed:
		return false, fmt.Errorf("storage publication failed: %s", publication.Failure)
	default:
		return false, nil
	}
}

type consoleSidecarOwner interface {
	FollowFamilyConsole(dir string, start func() error, follow ttcconsole.Follow, lock *os.File, target selector, observeInitExit bool) error
	ObserveFamilyConsole(dir, key string, target selector, observeInitExit bool) bool
	StopFamilyConsole(dir, key string) (bool, error)
	RetireFamilyConsoles(dir string) (bool, error)
	OwnConsoleSidecar(key string, command *exec.Cmd, target selector, observeInitExit bool) error
	ObserveInheritedConsoleSidecar(key string, target selector, observeInitExit bool)
	SuppressConsoleExitOnce(key string) func(bool)
	StopConsoleSidecar(key string) (bool, error)
	OwnPortSidecar(key string, command *exec.Cmd) error
	StopPortSidecar(key string) (bool, error)
	RegisterConsoleClient(target selector, cancel func()) (func(), error)
	QuiesceConsoleClients(target selector) (func(), error)
}

// beginExplicitInitStop transfers exit-finalization ownership from the console
// follower to stopBranchLifecycle. The returned function must be called with
// true once the old init is known to have terminated, or false when the stop
// was abandoned while it could still exit naturally later.
func (b *runtimeBackend) beginExplicitInitStop(target selector) (func(bool), error) {
	stops := b.explicitInitStops
	if stops == nil {
		return nil, fmt.Errorf("stopping %s: this backend counts no explicit stops", target)
	}
	if b.consoleSidecars == nil {
		stops.Add(1)
		return func(bool) { stops.Add(-1) }, nil
	}
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	paths, err := b.containerStore().BranchConsolePathsIn(info, resolved.branch)
	if err != nil {
		return nil, err
	}
	finishConsole := b.consoleSidecars.SuppressConsoleExitOnce(paths.Lock)
	stops.Add(1)
	return func(commit bool) {
		stops.Add(-1)
		finishConsole(commit)
	}, nil
}

func (b *runtimeBackend) stopConsoleDrain(paths ttcbundle.ConsolePaths) error {
	if paths.Lock == "" {
		return nil
	}
	// A console no one follows any more keeps its wait status: its process
	// has ended.
	if active, err := consoleDrainActive(paths.Lock); err != nil {
		return err
	} else if active && paths.FamilyDir != "" {
		var err error
		if b.consoleSidecars != nil {
			_, err = b.consoleSidecars.StopFamilyConsole(paths.FamilyDir, paths.Lock)
		} else {
			err = stopFamilyConsoleOnce(paths.FamilyDir, paths.Lock)
		}
		if err != nil {
			return err
		}
		// Unless a follower of its own that an older ttcd started follows
		// it, it is stopped.
		if active, err := consoleDrainActive(paths.Lock); err == nil && !active {
			_ = os.Remove(paths.PID)
			_ = os.Remove(paths.WaitStatus)
			return nil
		}
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

// clearConsoleIncarnationStatus removes only the terminal markers owned by a
// previous init incarnation. Logs and retention metadata intentionally remain:
// Docker-style restart appends to the same branch log, but a stale wait status
// must never be mistaken for the freshly imported init's exit.
func clearConsoleIncarnationStatus(paths ttcbundle.ConsolePaths) error {
	var result error
	for _, path := range []string{paths.PID, paths.WaitStatus} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

// stopContainerConsoleDrains stops following the consoles of the family
// containerID, whose runtime is gone, and has its follower exit.
func (b *runtimeBackend) stopContainerConsoleDrains(containerID string) error {
	consoleDir := filepath.Join(b.activeDataRoot(), "containers", containerID, "console")
	var result error
	if b.consoleSidecars != nil {
		_, result = b.consoleSidecars.RetireFamilyConsoles(consoleDir)
	} else {
		result = retireFamilyConsolesOnce(consoleDir)
	}
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
	cpuFeatures           string
	cpuTopology           string
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
	portIngress           *portIngressManager
	gfx                   *gfxService
	nvproxy               *nvproxyService
	nodeID                string
	cancelClient          func()
	clientCanceled        <-chan struct{}
	// disableSyscallPatching runs systrap without syscall patching, whose
	// fast path owns GS: in a cluster whose nodes need no CPUID faulting, an
	// application may see FSGSBASE in the host's own CPUID and write GS
	// itself, which systrap then keeps for it on every switch.
	disableSyscallPatching bool
	// requester is who asked for the command, nil for the daemon's owner: a
	// member of the group the daemon is shared with may have it open no host
	// path, and is recorded as the creator of the containers it asks for.
	requester *daemonRequester
	// clientGone is closed once the client's connection breaks.
	clientGone <-chan struct{}
	// archiveFile, if not nil, is the archive the client passed with its
	// request, a regular file: a checkpoint or an image save is written into
	// it, and a restore or an image load reads it, where each would stream
	// it.
	archiveFile *os.File
	// lockLifecycle serializes background lifecycle completion with foreground
	// commands and periodic repository maintenance. Long-running transfer and
	// peer polling stay outside this lock; only the terminal mutation that
	// releases runtimes, graph roots, and private repositories enters it.
	lockLifecycle func() func()
	// lifecycleLockHeld is true for the synchronous portion of a daemon
	// request that already owns lockLifecycle. Helpers shared with background
	// completion paths use it to avoid recursively acquiring the non-reentrant
	// family lifecycle lock.
	lifecycleLockHeld bool
	// explicitInitStops counts the explicit stops in progress, which
	// distinguish the internal waits performed by stop and restart from a
	// user-facing `ttc wait`. The foreground lifecycle operation owns
	// discard/publication ordering and must not recursively invoke the
	// spontaneous-exit finalizer from its wait path. A family's stop stops
	// its branches together, and their waits read it as they go. A request's
	// backend (directBackend) has its own, which the backend's copies share;
	// another backend has none, and stops no branch.
	explicitInitStops *atomic.Int32
	// lifecycleReady releases the daemon lifecycle lock after an attached
	// run/start has finished creating its runtime but before it waits for the
	// guest. Detached commands release the lock when the command returns.
	lifecycleReady func()
	// inventoryReadOnly is set for a listing that runs without the lifecycle
	// lock. It lists what it finds and repairs nothing: a runtime missing now
	// may be one that a mutation holding the lock is rebuilding.
	inventoryReadOnly bool
	// inventoryRepairWanted is set by such a listing when it passes over a
	// family whose runtime is gone, which a listing holding the lock would
	// record.
	inventoryRepairWanted bool
	// mutating is set for a daemon request that changes lifecycle state; it
	// waits for a long operation of another request on its family to finish.
	// commandName is the request's command.
	mutating    bool
	commandName string
	// operations records the long operations in progress, and
	// suspendLifecycle lets this request's long operation give up the
	// lifecycle lock; see withoutLifecycleLock.
	operations       *familyOperations
	suspendLifecycle func(*familyOperation) (func(), error)
	// lifecycleHold is the request's hold on the lifecycle lock, which it
	// gives up while it waits for another request's long operation.
	lifecycleHold *lifecycleHold
	// heldFamilies are the IDs of the families whose locks the request
	// holds. On a standalone daemon, lockFamily takes a family's lock, and
	// lockSharedLifecycle the lifecycle lock shared, for background work on
	// one family; see family_locks.go.
	heldFamilies        []string
	lockFamily          func(id string) func()
	lockSharedLifecycle func() func()
	// resolvedFamilies, if not nil, collects the logical families the
	// request resolves, or creates as run does: those a command that changes
	// only those families acts on, and the placement reconciliation after it
	// covers.
	resolvedFamilies *familySet
	// claimedAuthority, if not nil, is what the authority said of the branch
	// that the claim a command was routed with names, as the claim was
	// validated: the command's first resolution, if of that branch, reads it.
	claimedAuthority *claimedBranchAuthority
	// makingFamily, if not nil, is how a request that makes a family, holding
	// the lifecycle lock shared, makes it beside other commands.
	makingFamily *familyMaking
	// moves, if not nil, records for the request's daemon whether the
	// command's first resolution of a branch found the branch on another
	// node; see forwardMovedCommand.
	moves            *commandMoves
	publicationQueue *ttcstorage.Queue
	snapshotGraph    *ttcsnapshot.Store
	snapshotData     *ttcsnapshot.LocalDataStore
	snapshotResolver *ttcsnapshot.Resolver
	snapshotShared   *ttcsnapshot.SharedPackStore
	sharedReferences *sharedReferenceService
	// daemonStopRequested is set only after a node-stop policy has completed.
	// The daemon sends the command result before closing its listener.
	daemonStopRequested bool
	// removals is set while a command removes several branches, which does
	// once what each removal would: renew the node's execution lease before
	// the first, and reconcile the node's snapshot providers after the last.
	removals *branchRemovals
}

// branchRemovals is what a command that removes several branches did once
// for them all, or has yet to.
type branchRemovals struct {
	// renewed is set once the node renewed its execution lease.
	renewed bool
	// originsDue is set once a removal left the providers to reconcile.
	originsDue bool
}

// removingBranches runs work, which removes branches, as branchRemovals
// describes.
func (b *runtimeBackend) removingBranches(work func() error) error {
	if b.removals != nil {
		return work()
	}
	removals := &branchRemovals{}
	b.removals = removals
	err := work()
	b.removals = nil
	if removals.originsDue {
		if _, reconcileErr := b.reconcileVolatileSnapshotOrigins(); reconcileErr != nil {
			b.deferOriginReconcile(reconcileErr)
		}
	}
	return err
}

// deferOriginReconcile leaves to the node's maintenance the reconciliation of
// its volatile origins that a removal could not finish, err why. The removed
// branches are gone, and the reconciliation is the node's, of every
// repository it has: one it could not read failed every removal on the node
// after it had removed the branch's memory and filesystems, and the branch,
// still recorded, was left running without a runtime, or dehydrated without
// an image, until a retry of the removal, which failed the same way.
func (b *runtimeBackend) deferOriginReconcile(err error) {
	b.distributedSource.requestOriginReconcile()
	if b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: warning: reconciling snapshot providers after a removal, which the node tries again: %v\n", err)
	}
}

func (b *runtimeBackend) lockBackgroundLifecycle() func() {
	if b == nil || b.lockLifecycle == nil {
		return func() {}
	}
	return b.lockLifecycle()
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
	store := ttcbundle.Store{Root: b.dataRoot, ActiveRoot: b.activeDataRoot(), CPUFeatures: b.cpuFeatures, CPUTopology: b.cpuTopology}
	if b.root != "" {
		store.TransientRoot = filepath.Join(b.root, transientBundleDirName)
	}
	return store
}

func (b *runtimeBackend) activeDataRoot() string {
	if b.localRoot != "" {
		return b.localRoot
	}
	return b.dataRoot
}

// queueBranchPublication is queueBranchPublicationOnce, run again while a
// snapshot the branch's repositories share is frozen for an authority
// handoff, as another branch's publication leaves it for a moment: waiting
// without the lifecycle lock between tries, as a fork does. A refused try
// changes nothing. A stop right after a dehydration of another branch of its
// family failed so, once that dehydration's publication no longer waited
// seconds for a shared reference coordinator.
func (b *runtimeBackend) queueBranchPublication(target selector, kind string) error {
	deadline := time.Now().Add(forkHandoffWait)
	delay := 100 * time.Millisecond
	for {
		err := b.queueBranchPublicationOnce(target, kind)
		if err == nil || !frozenForAuthorityHandoff(err) || time.Now().After(deadline) {
			return err
		}
		info, loadErr := b.containerStore().LoadContainer(target.container)
		if loadErr != nil {
			return err
		}
		state := "stopping"
		if kind == "dehydrate" {
			state = "dehydrating"
		}
		if waitErr := b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: state}, func() error {
			time.Sleep(delay)
			return nil
		}); waitErr != nil {
			return waitErr
		}
		delay = min(2*delay, 2*time.Second)
	}
}

func (b *runtimeBackend) queueBranchPublicationOnce(target selector, kind string) error {
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
	// address the same immutable publication even when the lifecycle command used a name
	// or ID prefix.
	resolved, container, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	previous, err := persistentBranchInfo(container, target.branch)
	if err != nil {
		return err
	}
	authorityContainer, authorityBranch := logicalFamilyID(container), target.branch
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		// The branch's placement is one of its logical family's, which the
		// record names: those are read, and the cluster's every placement only
		// if none of them is the branch's. Every stop and dehydration read
		// every placement of the cluster.
		var found bool
		for _, family := range []string{logicalFamilyID(container), ""} {
			state, err := b.distributedSource.authority.AuthorityStateOf(cluster.AuthorityQuery{Placements: cluster.All[string](), Family: family})
			if err != nil {
				return fmt.Errorf("resolving publication authority: %w", err)
			}
			if authorityContainer, authorityBranch, found = logicalPlacementForPhysicalRuntime(state, container.ID, target.branch); found {
				break
			}
		}
		if !found {
			return fmt.Errorf("physical branch %s has no unique logical authority placement", target)
		}
	}
	publicationID, err := store.NewBranchPublicationID()
	if err != nil {
		return err
	}
	captureBranch := "ttc-publish-" + strings.TrimPrefix(publicationID, "publication-")
	intent := ttcstorage.Intent{
		Container: target.container, Branch: target.branch, PublicationID: publicationID, Kind: kind,
		AuthorityContainer: authorityContainer, AuthorityBranch: authorityBranch, FilesystemBranch: captureBranch,
		SupersedesPublicationID: previous.PublicationID,
	}
	if err := b.publicationQueue.Enqueue(intent); err != nil {
		return err
	}
	captureComplete := false
	publicationInstalled := false
	originsCommitted := false
	finishFilesystemOrigins := func(bool) error { return nil }
	var forks []*ttcvolume.ForkTransaction
	var repositories []*ttcvolume.Repository
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
		}
		if !originsCommitted {
			cause = errors.Join(cause, finishFilesystemOrigins(false))
		}
		if !publicationInstalled {
			cause = errors.Join(cause, b.publicationQueue.Cancel(target.container, target.branch, publicationID))
		}
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
	spec, err := store.LoadSpecOf(info)
	if err != nil {
		return rollback(err)
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return rollback(err)
	}
	assignments, finishOrigins, err := b.allocateFilesystemForkOrigins(info.ID, 1)
	finishFilesystemOrigins = finishOrigins
	if err != nil {
		return rollback(err)
	}
	originsByRepository := make(map[string][]uint64, len(assignments))
	for _, assignment := range assignments {
		originsByRepository[assignment.ResourceID.Path] = assignment.Origins
	}
	// Sealing adds references to the repositories' origins, as a capture
	// does: it holds the snapshot handoff gate, as a capture does, until its
	// forks are committed, so that no final switch of an origin's references
	// to the shared ledger freezes the origin meanwhile. Without it, a stop
	// right after another branch's dehydration was refused, its origin
	// "frozen for authority handoff".
	finishCapture := func() {}
	if b.distributedSource != nil {
		state := "stopping"
		if kind == "dehydrate" {
			state = "dehydrating"
		}
		finish, err := b.beginForegroundCapture(info, longOperation{branch: target.branch, state: state})
		if err != nil {
			return rollback(err)
		}
		var once sync.Once
		finishCapture = func() { once.Do(finish) }
		defer finishCapture()
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
		var fork *ttcvolume.ForkTransaction
		if origins := originsByRepository[repository.Info().ID]; len(origins) != 0 {
			fork, err = repository.BeginForkWithOrigins(repositoryPhysicalBranch(config, filesystemBranch), captureBranch,
				ttcsnapshot.SnapshotID(origins[0]), ttcsnapshot.SnapshotID(origins[1]))
		} else {
			fork, err = repository.BeginFork(repositoryPhysicalBranch(config, filesystemBranch), captureBranch)
		}
		if err != nil {
			return rollback(fmt.Errorf("sealing repository %q: %w", config.Source, err))
		}
		forks = append(forks, fork)
	}
	if kind == "dehydrate" {
		if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
			return rollback(errors.New("new-format dehydrated branch has no graph memory roots"))
		}
		if err := validateGraphControlRoot(branch.SnapshotID, branch.ControlRoot, branch.ControlRootObject); err != nil {
			return rollback(fmt.Errorf("new-format dehydrated branch has no graph control root: %w", err))
		}
		record, ok := b.snapshotGraph.Snapshot(ttcsnapshot.SnapshotID(branch.SnapshotID))
		if !ok || (record.State != ttcsnapshot.SnapshotSealedLocal && record.State != ttcsnapshot.SnapshotSealedVolatile) {
			return rollback(fmt.Errorf("graph memory snapshot %d is not sealed", branch.SnapshotID))
		}
	}
	committedForks := 0
	for _, fork := range forks {
		if err := fork.Commit(); err != nil {
			if committedForks != 0 {
				originsCommitted = true
				_ = finishFilesystemOrigins(true)
			}
			return rollback(err)
		}
		committedForks++
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return rollback(err)
	}
	originsCommitted = true
	captureComplete = true
	finishCapture()
	if _, err := store.BeginBranchPublicationIf(target.container, target.branch, previous.PublicationID, publicationID); err != nil {
		return rollback(fmt.Errorf("installing captured publication: %w", err))
	}
	publicationInstalled = true
	// The replacement is visible only after its complete hidden capture exists.
	// Promptly cancel a same-node predecessor; a predecessor owned by another
	// node observes the identity fence and terminates through the ordinary queue
	// supersession path. CaptureAvailable also fences its authority before
	// authorizing this publication.
	if previous.PublicationID != "" {
		_ = b.publicationQueue.CancelPending(target.container, target.branch, previous.PublicationID)
	}
	if err := b.publicationQueue.Activate(target.container, target.branch, publicationID); err != nil {
		// The committed hidden roots are immutable and safe for retry. Keep the
		// pending intent so daemon restart can validate and activate it.
		_ = store.UpdateBranchStorage(target.container, target.branch, publicationID, "error", err.Error(), time.Time{})
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

// logicalFamilyID returns the cluster identity used by the authority. A branch that
// has migrated may be materialized in a node-local container whose physical
// ID differs from the family ID; physical IDs must never escape into the
// control-plane state machine.
func logicalFamilyID(info ttcbundle.ContainerInfo) string {
	if info.DistributedFamilyID != "" {
		return info.DistributedFamilyID
	}
	return info.ID
}

func (b *runtimeBackend) imageProgress() ttcimages.ProgressFunc {
	return newImageProgressWithTerminal(b.stderr, b.progressTerminal, b.progressWidth)
}

func (b *runtimeBackend) resolveContainer(identifier string) (ttcbundle.ContainerInfo, error) {
	if b.dataRoot == "" {
		return ttcbundle.ContainerInfo{ID: identifier, Name: identifier}, nil
	}
	store := b.containerStore()
	info, err := store.LoadContainer(identifier)
	if err != nil {
		if placements, placementErr := b.placementCatalog(); placementErr == nil {
			// Branch-scoped operations need the authority's family identity before they
			// resolve the exact physical runtime below; they do not need the
			// canonical shared catalog record. A multi-node family removal may
			// legitimately delete that record with its final canonical physical
			// branch while transaction-scoped siblings are still being removed.
			// Keep those siblings operable from placement authority instead of
			// waiting for metadata that is intentionally gone.
			info, err = loadDistributedFamilyIdentity(store, placements, identifier)
			if err == nil {
				b.resolvedFamilies.resolved(identifier, info)
			}
			return info, err
		}
		return ttcbundle.ContainerInfo{}, err
	}
	b.resolvedFamilies.resolved(identifier, info)
	return info, nil
}

// familySet is a set of family IDs, to which a request's goroutines may add,
// with the ID of the container each name the request resolved named.
type familySet struct {
	mu    sync.Mutex
	ids   []string
	names map[string]string
}

// add adds id to s, if s is not nil.
func (s *familySet) add(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.ids, id) {
		s.ids = append(s.ids, id)
	}
}

// resolved adds to s the family of info, which identifier named.
func (s *familySet) resolved(identifier string, info ttcbundle.ContainerInfo) {
	if s == nil {
		return
	}
	s.add(logicalFamilyID(info))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil {
		s.names = make(map[string]string)
	}
	s.names[identifier] = info.ID
}

// containerOf returns the ID of the container identifier named when the
// request resolved it.
func (s *familySet) containerOf(identifier string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.names[identifier]
	return id, ok
}

// list returns the IDs in s.
func (s *familySet) list() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ids)
}

// resolveDistributedPersistentBranch maps one logical family branch to the
// shared catalog record that contains its durable node-local/cold metadata.
//
// A distributed fork may place a branch in a physical container whose ID is
// deliberately hidden from the user. While the branch is live, authority placement
// names that physical container. Durable publication makes the branch
// unplaced and clears execution identity, but commands such as cp, rehydrate,
// and fork-from-dehydrated must still find its immutable publication record.
// The physical records are in TTC_ROOT, so every node can resolve them without
// assigning execution ownership first.
func (b *runtimeBackend) resolveDistributedPersistentBranch(logical ttcbundle.ContainerInfo, branchID, publicationID string) (ttcbundle.ContainerInfo, string, error) {
	// The caller may have obtained logical through Store.LoadContainer(name),
	// whose user-facing family view merges branches from hidden physical
	// runtimes. Such a synthesized branch is useful for ls/inspect, but its
	// containing bundle and repository paths still belong to the logical
	// family's original runtime. Reload the exact immutable family ID so only a
	// branch genuinely stored in that bundle can satisfy the fast path.
	logicalID := logicalFamilyID(logical)
	if exact, err := b.containerStore().LoadRuntimeContainer(logicalID); err == nil {
		logical = exact
	}
	if branch, err := persistentBranchInfo(logical, branchID); err == nil {
		if publicationID == "" || (branch.PublicationID == publicationID && (branch.StorageManifest != "" ||
			(branch.NodeID == b.nodeID && (branch.StorageState == "local" || branch.StorageState == "publishing")))) {
			return logical, branchID, nil
		}
	}
	// Once authority commands publication, its filesystem root is the canonical
	// ancestry-independent locator. Following the manifest avoids an NFS
	// directory scan and disambiguates stale physical incarnations left by
	// repeated migrations. It also closes the cutover race where placement is
	// already unplaced but the derived container record is still being updated.
	if publicationID != "" && b.distributedSource != nil && b.distributedSource.authority != nil {
		if authority, err := b.distributedSource.authority.Branch(logicalID, branchID); err == nil &&
			authority.PublicationID == publicationID && authority.Roots.FilesystemRoot != "" {
			// A start or rehydration here that the publication unplaced the
			// branch beneath went on from it, until it claims the branch again.
			if branch, err := persistentBranchInfo(logical, branchID); err == nil && activatedFromPublication(b.containerStore(), branch, *authority, b.nodeID) {
				return logical, branchID, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			physical, physicalBranch, waitErr := waitForCommittedColdBranchMetadata(ctx, b.containerStore(), logicalID, branchID, *authority)
			cancel()
			if waitErr != nil {
				return ttcbundle.ContainerInfo{}, "", waitErr
			}
			return physical, physicalBranch.ID, nil
		}
	}
	containers, err := familyContainersForPlacement(b.containerStore(), logicalID)
	if err != nil {
		return ttcbundle.ContainerInfo{}, "", err
	}
	type candidate struct {
		info   ttcbundle.ContainerInfo
		branch string
	}
	var candidates []candidate
	for _, container := range containers {
		for _, branch := range container.Branches {
			// A migrated runtime's physical branch is a transaction ID. Once
			// publication commits, the authority deliberately clears physical placement,
			// so the immutable publication ID is the only ancestry-independent
			// locator for its cold record. Match the logical name only when no
			// authoritative publication identity is available.
			if publicationID != "" {
				if branch.PublicationID != publicationID || branch.StorageManifest == "" {
					continue
				}
			} else if branch.ID != branchID {
				continue
			}
			candidates = append(candidates, candidate{info: container, branch: branch.ID})
		}
	}
	switch len(candidates) {
	case 0:
		return ttcbundle.ContainerInfo{}, "", fmt.Errorf("branch %q does not exist in distributed family %q: %w", branchID, logical.Name, os.ErrNotExist)
	case 1:
		return candidates[0].info, candidates[0].branch, nil
	default:
		return ttcbundle.ContainerInfo{}, "", fmt.Errorf("branch %q has %d durable physical records in distributed family %q", branchID, len(candidates), logical.Name)
	}
}

// familyContainersForPlacement is ListContainersForPlacement for the
// containers of the logical family familyID: it reads no other family's
// records.
func familyContainersForPlacement(store ttcbundle.Store, familyID string) ([]ttcbundle.ContainerInfo, error) {
	ids, err := store.FamilyContainerIDs(familyID)
	if err != nil {
		return nil, err
	}
	var containers []ttcbundle.ContainerInfo
	for _, id := range ids {
		info, ok, err := store.LoadContainerForPlacement(id)
		if err != nil {
			return nil, err
		}
		if ok && logicalFamilyID(info) == familyID {
			containers = append(containers, info)
		}
	}
	return containers, nil
}

func (b *runtimeBackend) resolveSelector(target selector) (selector, ttcbundle.ContainerInfo, error) {
	resolved, info, err := b.resolveSelectorOf(target)
	b.moves.note(err)
	return resolved, info, err
}

// commandMoves is what a command's resolutions of branches found of their
// placement, for the daemon that runs the command.
type commandMoves struct {
	mu       sync.Mutex
	resolved bool
	owner    string
	// refuse has the command's failure refused as routed stale, if its
	// first resolution found its branch on another node; see runCommand.
	refuse bool
}

// refusedAsMoved reports whether the command's failure is to be refused as
// routed stale: its daemon asked for that, and its first resolution found
// its branch on another node.
func (b *runtimeBackend) refusedAsMoved() bool {
	if b == nil || b.moves == nil || !b.moves.refuse {
		return false
	}
	return b.moves.movedTo() != ""
}

// note records the outcome of a resolution: the first of a command's that
// found its branch on another node leaves the command undone, which the
// daemon may then send there.
func (m *commandMoves) note(err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var moved *remoteBranchOwnershipError
	if !m.resolved && errors.As(err, &moved) {
		m.owner = moved.owner
	}
	m.resolved = true
}

// force records that the command, which has done nothing, finds its branch
// on owner, whatever it resolved before.
func (m *commandMoves) force(owner string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner, m.resolved = owner, true
}

// movedBranchBackend is a backend that can tell whether a branch, which a
// command has done nothing to, has moved to another node meanwhile.
type movedBranchBackend interface {
	branchMovedAway(selector) bool
}

// branchMovedAway reports whether target, which the command has done nothing
// to, now belongs to another node, and records so for the command's daemon,
// which sends the command there.
func (b *runtimeBackend) branchMovedAway(target selector) bool {
	if b.moves == nil {
		return false
	}
	_, _, err := b.resolveSelectorOf(target)
	var moved *remoteBranchOwnershipError
	if !errors.As(err, &moved) {
		return false
	}
	b.moves.force(moved.owner)
	return true
}

// movedTo names the node that owned the branch the command's first
// resolution resolved, if another node did.
func (m *commandMoves) movedTo() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner
}

func (b *runtimeBackend) resolveSelectorOf(target selector) (selector, ttcbundle.ContainerInfo, error) {
	info, err := b.resolveContainer(target.container)
	if err != nil {
		return selector{}, ttcbundle.ContainerInfo{}, err
	}
	if waited, err := b.waitBusy(info.ID); err != nil {
		return selector{}, ttcbundle.ContainerInfo{}, err
	} else if waited {
		// What the claim was validated against may have changed meanwhile.
		b.claimedAuthority.discard()
		if info, err = b.resolveContainer(target.container); err != nil {
			return selector{}, ttcbundle.ContainerInfo{}, err
		}
	}
	return b.resolveSelectorWaiting(info, target)
}

// resolveSelectorWaiting is resolveSelectorIn of target in info, waiting, as
// resolveSelector waits for the family's, for a long operation on the physical
// runtime the branch resolves to: a remote fork's or a migration's container
// is apart from its logical family, whose wait does not see the operations on
// it. A removal of a branch that a fork there was copying, its lifecycle lock
// given up, destroyed the runtime under the fork, which failed, "urpc method
// "containerManager.ForkBranch" failed: EOF".
func (b *runtimeBackend) resolveSelectorWaiting(info ttcbundle.ContainerInfo, target selector) (selector, ttcbundle.ContainerInfo, error) {
	for {
		resolved, physical, err := b.resolveSelectorIn(info, target, nil)
		if err != nil || physical.ID == "" || physical.ID == info.ID {
			return resolved, physical, err
		}
		waited, err := b.waitBusy(physical.ID)
		if err != nil {
			return selector{}, ttcbundle.ContainerInfo{}, err
		}
		if !waited {
			return resolved, physical, nil
		}
		// What the claim was validated against may have changed meanwhile.
		b.claimedAuthority.discard()
		if info, err = b.resolveContainer(target.container); err != nil {
			return selector{}, ttcbundle.ContainerInfo{}, err
		}
	}
}

// resolveSelectorIn is resolveSelector of target, whose container resolved to
// info, without waiting for the family. state, if not nil, is a snapshot of the
// placements and branch authorities of info's family, which it reads instead
// of querying the authority for the branch's.
func (b *runtimeBackend) resolveSelectorIn(info ttcbundle.ContainerInfo, target selector, state *cluster.AuthorityState) (selector, ttcbundle.ContainerInfo, error) {
	for retries := 0; ; retries++ {
		resolved, physical, err := b.resolveSelectorOnce(info, target, state)
		// A cold branch's record moved on from the publication the
		// authority named, as a command of the branch on another node moved
		// it, which moves the authority next; or the authority read had
		// already moved on, and no record named its publication. The branch
		// is resolved as it is once the authority has moved: a command that
		// failed instead, an exec beside a start, failed "branch ... does not
		// exist", and a resume beside a stop of the branch dehydrated, "branch
		// ... publication is "", worker has ...".
		var moved *coldRecordMovedError
		if err == nil || retries == 3 || !errors.As(err, &moved) || !b.awaitColdAuthorityMovedOn(moved.family, moved.branch, moved.authority) {
			return resolved, physical, err
		}
		state = nil
	}
}

// coldRecordMovedError is resolveSelectorOnce's error for a cold branch that
// it could not find by the publication that authority, the branch's as the
// resolution read it, names: its record has moved on from it, or no record
// names it.
type coldRecordMovedError struct {
	family, branch string
	authority      cluster.BranchAuthority
	err            error
}

func (e *coldRecordMovedError) Error() string { return e.err.Error() }

func (e *coldRecordMovedError) Unwrap() error { return e.err }

// resolveSelectorOnce is resolveSelectorIn's resolution, once.
func (b *runtimeBackend) resolveSelectorOnce(info ttcbundle.ContainerInfo, target selector, state *cluster.AuthorityState) (selector, ttcbundle.ContainerInfo, error) {
	if b.nodeID != "" {
		logicalFamily := info.ID
		if info.DistributedFamilyID != "" {
			logicalFamily = info.DistributedFamilyID
		}
		var publicationID string
		var branchAuthority *cluster.BranchAuthority
		// A command's first resolution is of the branch its claim names,
		// which the claim was validated against just before.
		claimed := b.claimedAuthority.take(info.ID, target.branch)
		if b.distributedSource != nil && b.distributedSource.authority != nil {
			if state == nil && logicalFamily == info.ID {
				state = claimed
			}
			if state == nil {
				// The branch's authority and placement, in one read.
				var err error
				if state, err = b.distributedSource.authority.AuthorityStateOf(cluster.ResolutionQuery(info.ID, logicalFamily, target.branch)); err != nil {
					return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("resolving authority for %s: %w", target, err)
				}
			}
			if authority, authorityErr := state.BranchOf(info.ID, target.branch); authorityErr == nil && authority.Corrupted {
				// Corruption clears physical placement because no node may execute
				// the branch, but its last materialization may still contain graph,
				// filesystem, and console objects that rm must reclaim. Resolve that
				// durable record without treating it as execution authority.
				if _, branchErr := persistentBranchInfo(info, target.branch); branchErr != nil {
					physical, physicalBranch, resolveErr := b.resolveDistributedPersistentBranch(info, target.branch, "")
					if resolveErr == nil {
						target.container = physical.ID
						target.branch = physicalBranch
						return target, physical, nil
					}
					if !errors.Is(resolveErr, os.ErrNotExist) {
						return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("resolving corrupted branch storage for %s: %w", target, resolveErr)
					}
				}
				target.container = info.ID
				return target, info, nil
			} else if authorityErr == nil {
				publicationID = authority.PublicationID
				branchAuthority = authority
			} else if !errors.Is(authorityErr, os.ErrNotExist) {
				return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("resolving authority for %s: %w", target, authorityErr)
			}
		}
		if placements, placementErr := b.placementCatalog(); placementErr == nil {
			lookup := placements.Lookup
			if state != nil {
				lookup = state.PlacementOf
			}
			if placement, lookupErr := lookup(logicalFamily, target.branch); lookupErr == nil {
				if placement.OwnerNode != "" && placement.OwnerNode != b.nodeID {
					return selector{}, ttcbundle.ContainerInfo{}, &remoteBranchOwnershipError{target: target, owner: placement.OwnerNode}
				}
				physicalContainer := placement.PhysicalContainer
				if physicalContainer == "" {
					physicalContainer = info.ID
				}
				physicalBranch := placement.PhysicalBranch
				if physicalBranch == "" {
					physicalBranch = target.branch
				}
				// Migration keeps the stable logical selector in the authority, but the
				// target Sentry uses the transaction ID as its physical branch.
				// This distinction also exists when the target Sentry already hosts
				// another branch of the family, so comparing only container IDs
				// incorrectly routed commands to the nonexistent logical name.
				if physicalContainer != info.ID || physicalBranch != target.branch {
					physical := info
					if physicalContainer != info.ID {
						var loadErr error
						physical, loadErr = b.containerStore().LoadRuntimeContainer(physicalContainer)
						if loadErr != nil {
							return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("loading physical runtime for %s: %w", target, loadErr)
						}
					}
					if _, branchErr := persistentBranchInfo(physical, physicalBranch); branchErr != nil {
						return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("loading physical runtime branch for %s: %w", target, branchErr)
					}
					target.container = physical.ID
					target.branch = physicalBranch
					return target, physical, nil
				}
				// Publication commit deliberately removes physical placement from
				// the authority: a cold branch has no execution owner. Its immutable
				// publication identity is then the only authoritative way to select
				// among the physical records retained by repeated migrations. Never
				// let an older family record win merely because it also contains a
				// stale branch with the same user-visible name.
				if placement.State == cluster.PlacementUnplaced && publicationID != "" {
					physical, physicalBranch, resolveErr := b.resolveDistributedPersistentBranch(info, target.branch, publicationID)
					if resolveErr != nil {
						err := fmt.Errorf("loading durable physical branch for %s: %w", target, resolveErr)
						if branchAuthority != nil && (errors.Is(resolveErr, errColdRecordMovedOn) || errors.Is(resolveErr, os.ErrNotExist)) {
							err = &coldRecordMovedError{family: logicalFamily, branch: target.branch, authority: *branchAuthority, err: err}
						}
						return selector{}, ttcbundle.ContainerInfo{}, err
					}
					target.container = physical.ID
					target.branch = physicalBranch
					return target, physical, nil
				}
				// A cold branch without a publication, as a stop of its
				// dehydration leaves it, is its filesystem root's, which names
				// the record of the branch: a migration leaves the branch's
				// entry in its family's record, the family's catalog entry of
				// the branch, which is not the branch's record. Taken for it,
				// it had a cp of the branch stopped refused, "cannot copy the
				// offline branch ... while another branch in its family is
				// live", and the branch, started again, placed on it.
				if placement.State == cluster.PlacementUnplaced && branchAuthority != nil && branchAuthority.Roots.FilesystemRoot != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					physical, physicalBranch, resolveErr := waitForCommittedColdBranchMetadata(ctx, b.containerStore(), info.ID, target.branch, *branchAuthority)
					cancel()
					if resolveErr != nil {
						return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("loading durable physical branch for %s: %w", target, resolveErr)
					}
					target.container = physical.ID
					target.branch = physicalBranch.ID
					return target, physical, nil
				}
				if _, branchErr := persistentBranchInfo(info, target.branch); branchErr != nil && placement.State == cluster.PlacementUnplaced {
					physical, physicalBranch, resolveErr := b.resolveDistributedPersistentBranch(info, target.branch, "")
					if resolveErr != nil {
						return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("loading durable physical branch for %s: %w", target, resolveErr)
					}
					target.container = physical.ID
					target.branch = physicalBranch
					return target, physical, nil
				}
				target.container = info.ID
				return target, info, nil
			} else if !errors.Is(lookupErr, os.ErrNotExist) {
				return selector{}, ttcbundle.ContainerInfo{}, fmt.Errorf("resolving placement for %s: %w", target, lookupErr)
			}
		}
		owner := info.BranchNodeID(target.branch)
		if owner != "" && owner != b.nodeID {
			return selector{}, ttcbundle.ContainerInfo{}, &remoteBranchOwnershipError{target: target, owner: owner}
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
		var cache *ImageCacheInfo
		if usage, present, err := store.ImageCacheUsage(record.Digest, record.Platform); err != nil {
			return nil, err
		} else if present {
			cache = &ImageCacheInfo{
				CompleteFiles: usage.CompleteFiles, TotalFiles: usage.TotalFiles,
				CompleteBytes: usage.CompleteBytes, TotalBytes: usage.TotalBytes,
			}
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
			LocalCache:   cache,
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
	spec, err := b.containerStore().LoadSpecOf(info)
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
		committedOrigins := false
		if err := b.drainDistributedTargetFilesystems(info, branch); err != nil {
			repository.Close()
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, err
		}
		// A cluster's snapshot origins are its authority's to hand out. A
		// snapshot that took its own sealed the branch's origin as a
		// standalone one, past which no publication of the branch could go,
		// "cluster publication reached standalone origin", and numbered the
		// new ones as the authority numbers the next it hands out: a fork of
		// the branch then found its origin taken, "fresh working snapshot ID
		// ... is already working".
		state := normalizeLifecycleState(branch.State)
		var cont *container.Container
		if state == "running" || state == "paused" {
			// The Sentry may still have a bounded metadata batch for this
			// repository. Snapshot through the mounted filesystem so the guest is
			// paused across metadata flush, mmap invalidation, and graph-root
			// publication. Forking from this manager-side Repository directly can
			// otherwise capture only the last durable batch and silently omit the
			// most recent creates and writes.
			var loadErr error
			cont, loadErr = container.Load(b.root, container.FullID{
				SandboxID:   resolved.container,
				ContainerID: resolved.container,
			}, container.LoadOpts{Exact: true, SkipCheck: true})
			if loadErr != nil {
				repository.Close()
				reader.Close()
				writer.Close()
				return ttcimages.Info{}, fmt.Errorf("loading live branch for commit: %w", loadErr)
			}
		}
		snapshot := func() error {
			var origins []ttcsnapshot.SnapshotID
			allocateOrigins, finishOrigins := b.newFilesystemOriginScope()
			if allocateOrigins != nil {
				var err error
				if origins, err = allocateOrigins(2); err != nil {
					return errors.Join(err, finishOrigins(false))
				}
			}
			var err error
			if cont != nil {
				args := &boot.FilesystemBranchSnapshotArgs{
					BranchID: resolved.branch, ResourceID: checkpoint.ResourceID{ContainerName: "ttc-volume", Path: repository.Info().ID},
					Child: temporaryBranch,
				}
				if len(origins) == 2 {
					args.SourceWorking, args.ChildWorking = uint64(origins[0]), uint64(origins[1])
				}
				if err = cont.Sandbox.SnapshotFilesystemBranch(args); err != nil {
					err = fmt.Errorf("snapshotting live rootfs upper: %w", err)
				}
			} else {
				if len(origins) == 2 {
					err = repository.ForkWithOrigins(filesystemBranch, temporaryBranch, uint64(origins[0]), uint64(origins[1]))
				} else {
					err = repository.Fork(filesystemBranch, temporaryBranch)
				}
				if err != nil {
					err = fmt.Errorf("snapshotting rootfs upper: %w", err)
				}
			}
			if err != nil {
				return errors.Join(err, finishOrigins(false))
			}
			committedOrigins = len(origins) != 0
			return finishOrigins(true)
		}
		// The snapshot adds references to the branch's origins, which a
		// publication of the branch, as its stop's, freezes while it hands
		// them to the shared store: a commit of a branch just stopped was
		// refused, "snapshot ... references are frozen for authority
		// handoff". A refused snapshot changes nothing.
		if err := b.waitOutHandoffs(info, longOperation{branch: resolved.branch, state: "committing"}, snapshot); err != nil {
			repository.Close()
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, err
		}
		attachErr := b.readSharedImageInProcess(repository, temporaryBranch)
		defer func() {
			cleanupErr := repository.RemoveBranch(temporaryBranch)
			if cleanupErr == nil {
				cleanupErr = repository.GarbageCollect()
			}
			cleanupErr = errors.Join(cleanupErr, repository.Close())
			// The snapshot's origin, emptied with its branch, goes as a
			// removed branch's does.
			if cleanupErr == nil && committedOrigins {
				if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
					cleanupErr = fmt.Errorf("reconciling its snapshot origins: %w", err)
				}
			}
			if cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("removing commit snapshot: %w", cleanupErr))
			}
		}()
		if attachErr != nil {
			reader.Close()
			writer.Close()
			return ttcimages.Info{}, attachErr
		}
		go func() {
			err := repository.WriteLayerTar(temporaryBranch, writer)
			_ = writer.CloseWithError(err)
			exportDone <- err
		}()
	}
	var commitErr, exportErr error
	// Writing the layer takes time in proportion to the files; other
	// commands go on meanwhile, and the image store has its own lock.
	if err := b.withoutLifecycleLock(info, longOperation{branch: resolved.branch, state: "committing"}, func() error {
		result, commitErr = ttcimages.Commit(context.Background(), ttcimages.CommitOptions{
			Root: b.dataRoot, BaseDigest: info.Digest, Reference: reference, Layer: reader,
			Author: options.Author, Message: options.Message, Changes: options.Changes,
		})
		_ = reader.Close()
		exportErr = <-exportDone
		return nil
	}); err != nil {
		_ = reader.Close()
		<-exportDone
		return ttcimages.Info{}, err
	}
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
	var localCache *ImageCacheInfo
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
		if info.LocalCache != nil && (localCache == nil || info.LocalCache.CompleteBytes > localCache.CompleteBytes) {
			copy := *info.LocalCache
			localCache = &copy
		}
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
		LocalCache:   localCache,
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
		// Removing the image's roots and packs takes time in proportion to
		// its files; the image roots have a lock of their own.
		if err := b.withoutLifecycleLock(b.imageOperation(), longOperation{state: "removing"}, func() error {
			if _, err := store.RemoveImageRoots(digest); err != nil {
				return err
			}
			if _, _, err := store.PruneSharedImagePacks(context.Background(), digest); err != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: pruning shared image packs: %v\n", err)
			}
			return nil
		}); err != nil {
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
	allocate, finish := b.newFilesystemOriginScope()
	var repository *ttcvolume.Repository
	if allocate == nil {
		repository, err = manager.Create(name)
	} else {
		origins, allocateErr := allocate(1)
		if allocateErr != nil {
			return ttcvolume.VolumeInfo{}, errors.Join(allocateErr, finish(false))
		}
		repository, err = manager.CreateWithOrigin(name, origins[0])
		if err != nil {
			return ttcvolume.VolumeInfo{}, errors.Join(err, finish(false))
		}
		if err := finish(true); err != nil {
			repository.Close()
			return ttcvolume.VolumeInfo{}, err
		}
	}
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

// clusterVolumes lists the named volumes of every node of the cluster this
// daemon belongs to, with the node each is on, and reports false for a
// standalone daemon. As a Docker Swarm node's local volumes do, a volume
// belongs to one node: its branches there use it, and the same name on
// another node is another volume. A node that does not answer is passed over
// with a warning.
func (b *runtimeBackend) clusterVolumes() ([]clusterVolume, bool, error) {
	if b.nodeID == "" || b.distributedSource == nil || b.distributedSource.directory == nil {
		return nil, false, nil
	}
	nodes, err := b.clusterNodes()
	if err != nil {
		return nil, true, err
	}
	var result []clusterVolume
	now := time.Now()
	for _, node := range nodes {
		if !node.LeaseActive(now) {
			continue
		}
		var listing []clusterVolume
		if node.NodeID == b.nodeID {
			listing, err = b.localVolumes()
		} else {
			listing, err = b.remoteVolumes(node.NodeID)
		}
		if err != nil {
			if b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: listing the volumes of node %s: %v\n", node.NodeID, err)
			}
			continue
		}
		for _, volume := range listing {
			volume.Node = node.NodeID
			result = append(result, volume)
		}
	}
	return result, true, nil
}

func (b *runtimeBackend) localVolumes() ([]clusterVolume, error) {
	list, err := b.listVolumes()
	if err != nil {
		return nil, err
	}
	driver, err := b.volumeDriver()
	if err != nil {
		return nil, err
	}
	listing := make([]clusterVolume, 0, len(list))
	for _, info := range list {
		listing = append(listing, clusterVolume{Name: info.Name, Driver: driver})
	}
	return listing, nil
}

func (b *runtimeBackend) remoteVolumes(node string) ([]clusterVolume, error) {
	peer, err := b.distributedSource.peer(node, false)
	if err != nil {
		return nil, err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	status, output, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, []string{"volume", "ls", "--_local"})
	if err != nil {
		return nil, err
	}
	if status != 0 {
		return nil, errors.New(strings.TrimSpace(diagnostic))
	}
	var listing []clusterVolume
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		return nil, fmt.Errorf("decoding its volumes: %w", err)
	}
	return listing, nil
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

func (b *runtimeBackend) snapshotManagedVolume(ownerContainer, ownerBranch, volumeID, childBranch string, sourceWorking, childWorking ttcsnapshot.SnapshotID) error {
	args := []string{"timetravel", ownerContainer, "filesystem-snapshot", ownerBranch, volumeID, childBranch}
	if sourceWorking != 0 || childWorking != 0 {
		args = append(args, strconv.FormatUint(uint64(sourceWorking), 10), strconv.FormatUint(uint64(childWorking), 10))
	}
	_, err := b.command(args...)
	return err
}

// prepareImageForFamily pulls the image a new family is created from, if its
// pull policy asks for that, and unpacks its root on this node, without the
// lifecycle lock: both take time in proportion to the image, and other
// commands go on meanwhile. It returns the pull policy that preparing the
// family should then apply, and the image, if it was ready, for preparing
// the family to take.
func (b *runtimeBackend) prepareImageForFamily(reference, pullPolicy string) (string, *ttcimages.Info, error) {
	store := b.containerStore()
	if image, ready, err := store.ImageReady(context.Background(), reference, "", pullPolicy); err != nil {
		return pullPolicy, nil, err
	} else if ready {
		return pullPolicy, &image, nil
	}
	if err := b.withoutLifecycleLock(b.imageOperation(), longOperation{state: "pulling"}, func() error {
		return store.PrepareImage(context.Background(), reference, "", pullPolicy, b.imageProgress())
	}); err != nil {
		return "", nil, err
	}
	// The image is here now, and preparing the family must not pull it again.
	return "missing", nil, nil
}

func (b *runtimeBackend) run(request runRequest) (containerRunResult, error) {
	made, err := b.makingFamily.begin(request)
	if err != nil {
		return containerRunResult{}, err
	}
	defer made()
	consoleSize, err := b.initialConsoleSize(request.terminal)
	if err != nil {
		return containerRunResult{}, err
	}
	store := b.containerStore()
	pullPolicy, image, err := b.prepareImageForFamily(request.reference, request.pullPolicy)
	if err != nil {
		return containerRunResult{}, err
	}
	deviceMounts, deviceEnv, err := b.gfx.guestEdits(request.gpu)
	if err != nil {
		return containerRunResult{}, err
	}
	nvidiaMounts, deviceNodes, err := b.nvproxy.edits(request.gpu)
	if err != nil {
		return containerRunResult{}, err
	}
	deviceMounts = append(deviceMounts, nvidiaMounts...)
	filesystemOrigins, finishFilesystemOrigins := b.newFilesystemOriginScope()
	persistedPorts := request.publishedPorts
	if b.portIngress.Clustered() {
		// Cluster ingress is deliberately local to the daemon that receives the
		// run request and does not survive its restart.
		persistedPorts = nil
	}
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
			MemorySwap:       request.memorySwap,
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
		Reference:               request.reference,
		Image:                   image,
		PullPolicy:              pullPolicy,
		LogDriver:               request.logDriver,
		LogOptions:              request.logOptions,
		ConsoleLogMaxBytes:      request.consoleLogMaxBytes,
		ConsoleLogMaxFiles:      request.consoleLogMaxFiles,
		OpenStdin:               request.interactive,
		AutoRemove:              request.autoRemove,
		StopTimeout:             &request.stopTimeout,
		NetworkNAT:              request.networkNAT,
		NetworkIP:               request.networkIP,
		NetworkMAC:              request.networkMAC,
		NetworkInterface:        request.networkInterface,
		PublishedPorts:          persistedPorts,
		WaiveLiveFork:           !request.liveForkEligible,
		LiveForkWaiver:          request.liveForkWaiver,
		GPUOptions:              request.gpu,
		DeviceMounts:            deviceMounts,
		DeviceEnv:               deviceEnv,
		DeviceNodes:             deviceNodes,
		Progress:                b.imageProgress(),
		VolumeCopyMode:          b.volumeCopyMode,
		NodeID:                  b.nodeID,
		AllocateSnapshotOrigins: filesystemOrigins,
		SnapshotManagedVolume:   b.snapshotManagedVolume,
		ClaimID:                 b.makingFamily.claimer(b),
		CreatedBy:               b.containerCreator(),
	})
	if err != nil {
		return containerRunResult{}, errors.Join(err, finishFilesystemOrigins(false))
	}
	id := prepared.Container.ID
	b.resolvedFamilies.resolved(prepared.Container.Name, prepared.Container)
	target := selector{container: id, branch: defaultBranchID}
	reservation, err := b.reserveNewContainerPlacement(prepared.Container)
	if err != nil {
		return containerRunResult{}, errors.Join(err, finishFilesystemOrigins(false), store.RemoveContainer(id))
	}
	placementActive := false
	cleanupPrepared := func(cause error) error {
		if placementActive {
			return cause
		}
		return errors.Join(cause, b.cancelNewContainerPlacement(reservation), store.RemoveContainer(id))
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return containerRunResult{}, cleanupPrepared(err)
	}
	var ingressBindings []ttcbundle.PortBinding
	if b.portIngress.Clustered() {
		for _, binding := range request.publishedPorts {
			if err := b.portIngress.PublishPrepared(prepared.Container, defaultBranchID, binding); err != nil {
				for _, installed := range ingressBindings {
					_ = b.portIngress.Unpublish(target, installed)
				}
				return containerRunResult{}, cleanupPrepared(fmt.Errorf("publishing ingress port: %w", err))
			}
			ingressBindings = append(ingressBindings, binding)
		}
	}
	status, running, err := b.launch(prepared.Container, target, prepared.Container.BundleDir, request.detach, request.interactive)
	if err != nil {
		for _, binding := range ingressBindings {
			_ = b.portIngress.Unpublish(target, binding)
		}
		return containerRunResult{ID: id, Status: status}, cleanupPrepared(err)
	}
	// Guest execution has started, so the materializing placement is now an
	// authoritative recovery record. From this point onward cleanup must use the
	// normal branch lifecycle; deleting the reservation on a later bookkeeping
	// failure would strand an existing runtime outside the cluster catalog.
	placementActive = true
	b.recordLaunchState(target, running, status)
	if running {
		if err := b.activateNewContainerPlacement(reservation); err != nil {
			// A materializing record is authoritative and recovery can still find
			// this runtime, but the initiating command must not report success
			// until the branch is normally routable.
			return containerRunResult{ID: id, Status: 1}, err
		}
	} else if !request.autoRemove {
		// A foreground exit has the same durable-storage obligations as `ttc
		// stop`: release its process state, seal the filesystem, and let
		// publication atomically replace the materializing placement with an
		// unplaced durable branch. This remains correct if publication finishes
		// after the client disconnects or the daemon restarts.
		if err := b.discard(target); err != nil {
			return containerRunResult{ID: id, Status: status}, fmt.Errorf("releasing foreground execution state for %s: %w", target, err)
		}
		if err := b.queueBranchPublication(target, "stop"); err != nil && !b.branchPublicationRecorded(target) {
			// The run gave its locks up once its container started, and the
			// exit's own finalization, which takes the lifecycle lock,
			// publishes a branch that exits by itself: one that published the
			// branch first refused the run's, "installing captured
			// publication: branch "main" publication changed", and a run
			// whose container had run as it should exited 1. The branch is
			// published either way.
			return containerRunResult{ID: id, Status: status}, fmt.Errorf("queueing foreground filesystem publication for %s: %w", target, err)
		}
	}
	if request.autoRemove {
		status, err = b.applyAutoRemove(target, running, status, err)
	}
	return containerRunResult{ID: id, Status: status}, err
}

// branchPublicationRecorded reports whether target's record names a
// publication of it under way or made, as one the exit's finalization made.
func (b *runtimeBackend) branchPublicationRecorded(target selector) bool {
	info, err := b.containerStore().LoadContainer(target.container)
	if err != nil {
		return false
	}
	branch, err := persistentBranchInfo(info, target.branch)
	return err == nil && (branch.PublicationID != "" || branch.StorageState == "publishing" || branch.StorageState == "shared")
}

func (b *runtimeBackend) create(request runRequest) (string, error) {
	made, err := b.makingFamily.begin(request)
	if err != nil {
		return "", err
	}
	defer made()
	consoleSize, err := b.initialConsoleSize(request.terminal)
	if err != nil {
		return "", err
	}
	store := b.containerStore()
	pullPolicy, image, err := b.prepareImageForFamily(request.reference, request.pullPolicy)
	if err != nil {
		return "", err
	}
	deviceMounts, deviceEnv, err := b.gfx.guestEdits(request.gpu)
	if err != nil {
		return "", err
	}
	nvidiaMounts, deviceNodes, err := b.nvproxy.edits(request.gpu)
	if err != nil {
		return "", err
	}
	deviceMounts = append(deviceMounts, nvidiaMounts...)
	filesystemOrigins, finishFilesystemOrigins := b.newFilesystemOriginScope()
	createdNodeID := b.nodeID
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
			MemorySwap:       request.memorySwap,
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
		Image:              image,
		PullPolicy:         pullPolicy,
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
		NetworkInterface:   request.networkInterface,
		PublishedPorts:     request.publishedPorts,
		WaiveLiveFork:      !request.liveForkEligible,
		LiveForkWaiver:     request.liveForkWaiver,
		GPUOptions:         request.gpu,
		DeviceMounts:       deviceMounts,
		DeviceEnv:          deviceEnv,
		DeviceNodes:        deviceNodes,
		Progress:           b.imageProgress(),
		VolumeCopyMode:     b.volumeCopyMode,
		// A created branch owns mutable local filesystem state even though it has
		// no runtime yet. Keep that state assigned to this node until an explicit
		// publication or migration transfers authority.
		NodeID:                  createdNodeID,
		AllocateSnapshotOrigins: filesystemOrigins,
		SnapshotManagedVolume:   b.snapshotManagedVolume,
		ClaimID:                 b.makingFamily.claimer(b),
		CreatedBy:               b.containerCreator(),
	})
	if err != nil {
		return "", errors.Join(err, finishFilesystemOrigins(false))
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return "", errors.Join(err, store.RemoveContainer(prepared.Container.ID))
	}
	if err := b.registerCreatedContainerPlacement(prepared.Container); err != nil {
		return "", errors.Join(err, store.RemoveContainer(prepared.Container.ID))
	}
	// The family is made, not resolved by name: recorded, it is the one the
	// reconciliation after the command looks at, as after a run.
	b.resolvedFamilies.resolved(prepared.Container.Name, prepared.Container)
	return prepared.Container.ID, nil
}

func (b *runtimeBackend) start(target selector, detach, interactive bool) (int, error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return 1, err
	}
	// A peer's long operation, as a dehydration, works without the lifecycle
	// lock: the start waits for it, as for the lock. It was refused instead,
	// "cannot start ... while peer ... is dehydrating", having claimed the
	// branch for its node, which every later command of the branch then
	// waited for and was refused, "only lifecycle recovery may retry it".
	if waited, err := b.waitBusy(info.ID); err != nil {
		return 1, err
	} else if waited {
		if resolved, info, err = b.resolveSelector(target); err != nil {
			return 1, err
		}
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
	// A stopped branch becomes locally restartable before its immutable capture
	// is necessarily shared. Fence that exact capture before executing a fresh
	// process incarnation. Otherwise its late completion could still release the
	// placement or overwrite storage metadata belonging to the new runtime.
	// A committed capture is shared, with its manifest, and has no active
	// worker; materialize it below before changing the local storage view.
	// Until then, this node's filesystem of the branch is its latest, and a
	// manifest the branch names is an older publication's, which the start
	// must not put back: taking it for the capture's, a start right after a
	// stop was refused.
	// A publication that committed before the fence is the capture's, and is
	// materialized below.
	keepLocalFilesystem := false
	if branch.PublicationID != "" && branch.StorageState != "shared" {
		if b.publicationQueue == nil {
			return 1, fmt.Errorf("branch %s has publication %q but no publication queue", target, branch.PublicationID)
		}
		fenceErr := b.publicationQueue.CancelUncommitted(info.ID, target.branch, branch.PublicationID)
		if fenceErr == nil {
			fenceErr = b.containerStore().AbandonBranchPublication(info.ID, target.branch, branch.PublicationID)
			if fenceErr != nil && !errors.Is(fenceErr, ttcbundle.ErrPublicationCommitted) {
				return 1, fmt.Errorf("retiring stopped snapshot publication for %s: %w", target, fenceErr)
			}
		} else if !errors.Is(fenceErr, ttcstorage.ErrPublicationCommitted) {
			return 1, fmt.Errorf("fencing stopped snapshot publication for %s: %w", target, fenceErr)
		} else if err := b.completeCommittedPublication(info, target.branch, branch.PublicationID); err != nil {
			return 1, fmt.Errorf("completing stopped snapshot publication for %s: %w", target, err)
		}
		keepLocalFilesystem = fenceErr == nil
		info, err = b.containerStore().LoadContainer(info.ID)
		if err != nil {
			return 1, err
		}
		branch, err = persistentBranchInfo(info, target.branch)
		if err != nil {
			return 1, err
		}
	}
	preservedPeers, err := freshStartPeers(info, target, b.nodeID)
	if err != nil {
		return 1, err
	}
	// The record's peers on this node run in the family's runtime here, if
	// it runs: a migration leaves the branch's entry in its family's record,
	// naming the node the branch runs on, in a runtime of its own. A start
	// beside such a branch on its node found no runtime of the family there,
	// "querying active peers before starting ...: loading container: file
	// does not exist", and kept its claim of the branch, which every later
	// command of the branch was then refused for.
	if len(preservedPeers) != 0 && !b.sandboxSentryRuns(info.ID) {
		preservedPeers = nil
	}
	startBesidePeers := func() (int, error) {
		info, branch, err = b.prepareFreshStartStorage(info, target, keepLocalFilesystem)
		if err != nil {
			return 1, err
		}
		if err := b.startFreshBranchBesidePeers(info, target, branch); err != nil {
			return 1, err
		}
		if err := b.containerStore().RecordBranchStart(target.container, target.branch, b.nodeID); err != nil {
			_, _ = b.command("timetravel", target.container, "destroy", target.branch)
			return 1, fmt.Errorf("recording fresh start for %s: %w", target, err)
		}
		return b.finishPreservedPeerStart(info, target, detach, interactive)
	}
	if len(preservedPeers) != 0 {
		return startBesidePeers()
	}
	// Replacing the runtime would take every capture only its Sentry holds.
	if err := b.persistResidentDehydrations(info, nil); err != nil {
		return 1, err
	}
	// Waiting for the branch init process and tearing down its sandbox are
	// separate events. In particular, a multi-process container may report its
	// init's exit just before runsc changes the old runtime from running to
	// stopped. A restart must not race that short teardown window, but an
	// ordinary attempt to start a persistently-running container should still
	// fail immediately.
	waitForTeardown := true
	if err := b.removeStoppedRuntime(info.ID, waitForTeardown); err != nil {
		// The runtime runs a branch its record does not name as running, as
		// one a remote fork imports into it: the start goes beside it.
		var running *runtimeStillRunsError
		if errors.As(err, &running) && running.branch != target.branch {
			return startBesidePeers()
		}
		return 1, fmt.Errorf("preparing a fresh runtime for %s: %w", target, err)
	}
	info, branch, err = b.prepareFreshStartStorage(info, target, keepLocalFilesystem)
	if err != nil {
		return 1, err
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
	status, running, err := b.launch(info, target, bundleDir, detach, interactive)
	if err == nil {
		b.recordLaunchState(target, running, status)
	}
	if info.AutoRemove {
		return b.applyAutoRemove(target, running, status, err)
	}
	return status, err
}

// prepareFreshStartStorage makes the target branch's immutable image and
// filesystem roots usable from this node's active repository. It is shared by
// full-sandbox replacement and bootstrap-beside-peers: a cold branch may be
// admitted on a node that has neither its image extraction nor its writable
// repository yet.
// prepareFreshStartStorage makes ready the filesystem a fresh start of target
// boots from: the branch's committed manifest's, unless keepLocal, when this
// node's filesystem of the branch is its latest and is kept.
func (b *runtimeBackend) prepareFreshStartStorage(info ttcbundle.ContainerInfo, target selector, keepLocal bool) (ttcbundle.ContainerInfo, ttcbundle.BranchInfo, error) {
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
	}
	if b.dataRoot == "" {
		return info, branch, nil
	}
	store := b.containerStore()
	if ready, err := store.KeepContainerImageRootOf(info); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
	} else if !ready {
		// Unpacking the image on a node that has not run it yet takes time
		// in proportion to the image; other commands go on meanwhile.
		if err := b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "starting"}, func() error {
			return store.PrepareContainerImageRootOf(context.Background(), info)
		}); err != nil {
			return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
		}
	}
	info, err = store.RefreshContainerImageRoot(context.Background(), info.ID)
	if err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
	}
	if _, err := store.PruneImageRoots(context.Background()); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("pruning superseded image roots: %w", err)
	}
	branch, err = persistentBranchInfo(info, target.branch)
	if err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
	}
	if branch.StorageManifest == "" {
		return info, branch, nil
	}
	if keepLocal {
		// The branch may still read files of the older publication lazily,
		// which a fresh start's Sentry has no relay for.
		if err := b.materializeRemoteFilesystemObjects(info, branch); err != nil {
			return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("materializing durable filesystem for %s: %w", target, err)
		}
		return info, branch, nil
	}
	if err := b.ensureLocalColdFilesystem(info, branch); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("materializing durable filesystem for %s: %w", target, err)
	}
	if err := b.ensureClusterFilesystemWorkingOrigins(info, branch.FilesystemBranch); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("activating durable filesystem for %s: %w", target, err)
	}
	// A fresh start does not restore serialized Sentry state: its VFS is
	// constructed directly from the newly materialized local repository. Retire
	// the one-shot source alias before this runtime can become a later migration
	// source.
	if err := store.CommitActiveRepositoryRestoreIDsOf(info); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("canonicalizing fresh filesystem identities for %s: %w", target, err)
	}
	return info, branch, nil
}

// freshStartPeers returns active branches that must survive replacement of the
// family's Sentry. Process-free peers need no work. Transient states are
// rejected because daemon mutation serialization should never expose one here
// unless an earlier operation was interrupted.
func freshStartPeers(info ttcbundle.ContainerInfo, target selector, localNodeID string) ([]selector, error) {
	peers := make([]selector, 0, len(info.Branches))
	for _, branch := range info.Branches {
		if branch.ID == target.branch {
			continue
		}
		// A distributed physical family may have live branches on several
		// nodes, each in a different Sentry. Only branches owned by this node
		// can be peers of the runtime that we are about to create here. Treating
		// a remote branch as a local peer makes start try to import into a
		// nonexistent local Sentry with the same physical container ID.
		// Standalone branches have no owner and continue to share one Sentry.
		if localNodeID != "" && branch.NodeID != localNodeID {
			continue
		}
		switch normalizeLifecycleState(branch.State) {
		case "running", "paused":
			peers = append(peers, selector{container: target.container, branch: branch.ID})
		case "created", "dehydrated", "exited", "failed", "dead":
		case "dehydrating":
			// A dehydration under way holds its family's lifecycle lock or
			// long operation, which the start waited for. A branch still
			// dehydrating is one whose publication is under way, from its
			// durable local capture, which needs no runtime: a start of its
			// peer was refused for as long as the publication took.
		case "corrupted":
			// Only removal is left to a corrupted branch: its runtime is
			// lost, or fenced to be. It refused every start in its family
			// as a peer of unknown state.
		case "creating", "rehydrating", "restarting":
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
	configured, err := b.configuredRunscArgsForBranch(info, branch)
	if err != nil {
		return err
	}
	configured, err = b.withRuntimeDiagnostics(configured, info.ID)
	if err != nil {
		return err
	}
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
	capture, metadata, err := b.captureTransientBranch(runtimeID, target.branch, stagingDir)
	if err != nil {
		return fmt.Errorf("capturing paused bootstrap for %s: %w", target, err)
	}
	defer metadata.Close()
	graphRoots := make([]string, len(capture.MemoryRoots))
	graphObjects := make([]string, len(capture.MemoryRoots))
	for i, root := range capture.MemoryRoots {
		graphRoots[i] = root.Name
		graphObjects[i] = root.Root
	}
	graphRecorded := false
	defer func() {
		if graphRecorded {
			return
		}
		_ = b.releaseGraphMemoryRoots(graphRoots)
		if b.distributedSource != nil {
			_ = b.discardFailedGraphSnapshot(ttcsnapshot.SnapshotID(capture.SnapshotID))
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
	if branch.SnapshotID != 0 || len(branch.MemoryRoots) != 0 || len(branch.MemoryRootObjects) != 0 || branch.ControlRoot != "" || branch.ControlRootObject != "" || branch.DistributedGraphReferenceOperation != "" {
		return fmt.Errorf("fresh branch %s unexpectedly retains an older memory snapshot", target)
	}
	if err := b.containerStore().UpdateBranchMemoryGraph(info.ID, target.branch, capture.SnapshotID, graphRoots, graphObjects, ""); err != nil {
		return fmt.Errorf("recording bootstrap memory graph for %s: %w", target, err)
	}
	defer func() {
		if !graphRecorded {
			_ = b.containerStore().UpdateBranchSnapshotGraph(info.ID, target.branch, 0, nil, nil, "", "", "")
		}
	}()
	consolePaths, err := b.containerStore().BranchConsolePathsIn(info, target.branch)
	if err != nil {
		return fmt.Errorf("resolving console state for fresh branch %s: %w", target, err)
	}
	if err := clearConsoleIncarnationStatus(consolePaths); err != nil {
		return fmt.Errorf("clearing prior console state for fresh branch %s: %w", target, err)
	}
	capture.Resume = true
	restoreIDs, err := b.filesystemRestoreIDs(info.ID)
	if err != nil {
		return err
	}
	if err := b.rehydrateColdPeer(target, info, branch, capture, stagingDir, metadata, nil, restoreIDs); err != nil {
		return fmt.Errorf("installing fresh branch %s beside active peers: %w", target, err)
	}
	graphRecorded = true
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
func (b *runtimeBackend) captureTransientBranch(runtimeID, branchID, stagingDir string) (_ boot.BranchDehydrateInfo, _ *os.File, retErr error) {
	metadataPath := filepath.Join(stagingDir, "metadata.state")
	metadata, err := os.OpenFile(metadataPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, err
	}
	output, err := b.commandWithFiles([]*os.File{metadata}, "timetravel", "--dehydration-fd=3", runtimeID, "dehydrate", branchID)
	if closeErr := metadata.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, err
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(output, &capture); err != nil {
		return boot.BranchDehydrateInfo{}, nil, fmt.Errorf("decoding bootstrap capture: %w", err)
	}
	if len(capture.Memories) == 0 {
		return boot.BranchDehydrateInfo{}, nil, errors.New("bootstrap capture contains no memory images")
	}
	origin, sealState, built, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, fmt.Errorf("allocating bootstrap snapshot origin: %w", err)
	}
	defer built()
	var roots []string
	defer func() {
		if retErr == nil {
			return
		}
		if len(roots) != 0 {
			_ = b.releaseGraphMemoryRoots(roots)
		}
		if b.distributedSource != nil {
			_ = b.discardFailedGraphSnapshot(origin)
		}
	}()
	reader, writer, err := graphCaptureStream()
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, err
	}
	type graphImportOutcome struct {
		result ttcchunk.GraphImportResult
		err    error
	}
	imported := make(chan graphImportOutcome, 1)
	prefix := fmt.Sprintf("memory/%016x", uint64(origin))
	go func() {
		result, importErr := b.importGraphCapture(reader, origin, prefix)
		_ = reader.Close()
		imported <- graphImportOutcome{result: result, err: importErr}
	}()
	_, exportErr := b.commandWithFiles([]*os.File{writer}, "timetravel", "--dehydration-graph-stream-fd=3", "--dehydration-graph-stream-filtered", runtimeID, "dehydrate-export", branchID)
	_ = writer.Close()
	outcome := <-imported
	if exportErr != nil {
		return boot.BranchDehydrateInfo{}, nil, fmt.Errorf("streaming graph-native bootstrap memory: %w", exportErr)
	}
	if outcome.err != nil {
		return boot.BranchDehydrateInfo{}, nil, fmt.Errorf("importing graph-native bootstrap memory: %w", outcome.err)
	}
	for _, root := range outcome.result.Roots {
		capture.MemoryRoots = append(capture.MemoryRoots, boot.BranchMemoryRoot{
			ResourceID: root.ResourceID,
			Name:       root.Name,
			Root:       root.Root.String(),
		})
		roots = append(roots, root.Name)
	}
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		return boot.BranchDehydrateInfo{}, nil, fmt.Errorf("sealing bootstrap snapshot origin %d: %w", origin, err)
	}
	capture.SnapshotID = uint64(origin)
	metadata, err = os.Open(metadataPath)
	if err != nil {
		return boot.BranchDehydrateInfo{}, nil, err
	}
	return capture, metadata, nil
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

// runtimeStillRunsError is removeStoppedRuntime's refusal of a runtime that
// still runs a branch other than main.
type runtimeStillRunsError struct {
	container, branch string
}

func (e *runtimeStillRunsError) Error() string {
	return fmt.Sprintf("container %q still runs branch %q", e.container, e.branch)
}

func (b *runtimeBackend) removeStoppedRuntime(name string, waitForTeardown bool) error {
	const (
		teardownTimeout = 5 * time.Second
		teardownPoll    = 10 * time.Millisecond
	)
	deadline := time.Now().Add(teardownTimeout)
	for {
		done, ok := b.operations.beginRelease(name)
		if !ok {
			// A remote fork is importing a branch into the runtime; see
			// familyOperations.releasing. Once listed, its branch keeps the
			// runtime.
			b.operations.waitMadeIn(name)
			continue
		}
		finished, err := b.removeStoppedRuntimeOnce(name, waitForTeardown && !time.Now().After(deadline))
		done()
		if finished {
			return err
		}
		time.Sleep(teardownPoll)
	}
}

// removeStoppedRuntimeOnce is one look of removeStoppedRuntime, which reports
// whether it is finished.
func (b *runtimeBackend) removeStoppedRuntimeOnce(name string, wait bool) (bool, error) {
	families, err := b.listRuntimeFamiliesOf(name)
	if err != nil {
		return true, fmt.Errorf("checking runtime state before starting %q: %w", name, err)
	}
	for _, family := range families {
		if family.ID != name {
			continue
		}
		// The runtime's state is its loader branch's: it was "exited"
		// while another branch of its ran, which went with it, and
		// "dehydrated" while it ran none, and a start waited for it to stop
		// and was refused. Its branches say whether it runs.
		live := runtimeFamilyLiveBranch(family)
		if live == "" || runtimeFamilyUnavailable(family) {
			if _, err := b.command("delete", "--force", name); err != nil {
				return true, fmt.Errorf("deleting unavailable runtime container %q: %w", name, err)
			}
			return true, nil
		}
		if wait {
			return false, nil
		}
		if live == defaultBranchID || family.Error != "" {
			return true, fmt.Errorf("container %q is already %s", name, family.State)
		}
		return true, &runtimeStillRunsError{container: name, branch: live}
	}
	return true, nil
}

func (b *runtimeBackend) exec(request execRequest) (retStatus int, retErr error) {
	resolved, info, err := b.resolveSelector(request.target)
	if err != nil {
		return 1, err
	}
	request.target = resolved
	if b.dataRoot != "" {
		branch, err := persistentBranchInfo(info, request.target.branch)
		if err != nil {
			return 1, err
		}
		if state := normalizeLifecycleState(branch.State); state != "running" {
			return 1, fmt.Errorf("cannot exec in branch %s: branch is %s", request.target, state)
		}
	}
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
	args := []string{"--ttc-allow-stopped-sandbox", "--branch", request.target.branch}
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
	if request.retain {
		args = append(args, "--retain-exec-session")
	}
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
	started := time.Now()
	status := 0
	if embeddedRuntime(b.runsc) {
		if request.terminal && !request.detach {
			stopResize := b.watchConsoleResize(request.target, consoleID)
			defer stopResize()
		}
		var input io.Reader
		if request.interactive && !request.detach {
			input = b.stdin
		}
		status = embeddedExec(b.runtimeControlArgs(), args, request.detach, input, b.stdout, b.stderr, nil)
	} else {
		command := runscCommand(b.runsc, append(append(b.runtimeControlArgs(), "exec"), args...)...)
		if !request.detach && (request.interactive || request.terminal) {
			closeCommandStdin, err := setCommandStdin(command, b.stdin)
			if err != nil {
				return 1, err
			}
			defer closeCommandStdin()
		}
		command.Stdout = b.stdout
		command.Stderr = b.stderr
		if err := command.Start(); err != nil {
			return 1, fmt.Errorf("starting runsc exec: %w", err)
		}
		if request.terminal && !request.detach {
			stopResize := b.watchConsoleResize(request.target, consoleID)
			defer stopResize()
		}
		if err := command.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return 1, fmt.Errorf("executing runsc: %w", err)
			}
			status = exitErr.ExitCode()
		}
	}
	if status != 0 {
		if diagnostic := b.runtimeDiagnosticSince(request.target.container, started); diagnostic != "" && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: sandbox diagnostic: %s\n", diagnostic)
		}
		// runsc exec fails with 128 when the runtime dies under it, as its
		// cgroup's out-of-memory killer may kill it. The process was killed
		// with the runtime: report it as docker exec reports one killed with
		// its container, and record the loss.
		if status == 128 {
			if _, lost := b.lostRuntimeWaitStatus(request.target); lost {
				return 128 + int(unix.SIGKILL), nil
			}
		}
		return status, nil
	}
	if request.detach {
		// The branch is the one resolved above, whose record info is.
		session, err := b.execSessionOf(request.target, request.execID)
		if err != nil {
			return 1, err
		}
		if _, err := b.ensureExecConsoleDrainIn(info, request.target, session, true); err != nil {
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
	return ttcconsole.ReadWaitStatus(path)
}

func (b *runtimeBackend) attach(request attachRequest) (status uint32, retErr error) {
	// The branch's record, which resolving it read, has what attaching needs.
	target, containerInfo, err := b.resolveSelector(request.target)
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
		paths, err = b.ensureConsoleDrainIn(containerInfo, target, false)
		if err != nil {
			return 0, err
		}
		terminal = containerInfo.Terminal
	} else {
		session, err := b.execSessionOf(target, execID)
		if err != nil {
			return 0, err
		}
		consoleID = session.ConsoleID
		terminal = session.Terminal
		paths, err = b.ensureExecConsoleDrainIn(containerInfo, target, session, false)
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
	_, maxFiles := consoleRetentionOf(containerInfo)
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
	drainStopped := false
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
				if !drainStopped {
					// What the drain wrote before it stopped, its output's
					// end and the exit status, may have come after the look
					// above: look once more before judging.
					drainStopped = true
					continue
				}
				if !follower.stdoutEOF || !follower.stderrEOF {
					return 0, consoleDrainStoppedError(paths.Error)
				}
				if waitResult == nil {
					if waitErr != nil {
						return 0, waitErr
					}
					return 0, consoleDrainStoppedError(paths.Error)
				}
			} else {
				drainStopped = false
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
	// The branch's record, which resolving it read, has what the logs need.
	resolved, container, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	releaseClient, err := b.registerConsoleClient(target)
	if err != nil {
		return err
	}
	defer releaseClient()
	if container.LogDriver == "none" {
		return fmt.Errorf("configured logging driver %q does not support reading", container.LogDriver)
	}
	var paths ttcbundle.ConsolePaths
	// A branch made and not started, or captured to be dehydrated, has no
	// process whose console a follower could follow, and starting one failed
	// the logs: what the branch wrote before is all there is.
	followed, created := true, false
	if options.ExecID == "" {
		if branch, branchErr := persistentBranchInfo(container, target.branch); branchErr == nil {
			switch normalizeLifecycleState(branch.State) {
			case "created":
				followed, created = false, true
			case "dehydrating", "dehydrated":
				followed = false
			}
		}
	}
	if !followed {
		paths = b.containerStore().BranchConsolePathsOf(container.ID, target.branch)
	} else if options.ExecID == "" {
		paths, err = b.ensureConsoleDrainIn(container, target, false)
	} else {
		session, sessionErr := b.execSessionOf(target, options.ExecID)
		if sessionErr != nil {
			return sessionErr
		}
		paths, err = b.ensureExecConsoleDrainIn(container, target, session, false)
	}
	if err != nil {
		return err
	}
	_, maxFiles := consoleRetentionOf(container)
	follower, err := openConsoleLogFollower(paths.Log, maxFiles)
	if created && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer follower.file.Close()
	output := newConsoleLogOutput(options, container.LogAttributes)
	initial := true
	drainStopped := false
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
		if !options.Follow || !followed || (follower.stdoutEOF && follower.stderrEOF) {
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
				if drainStopped {
					return consoleDrainStoppedError(paths.Error)
				}
				// Look once more first, as attach does.
				drainStopped = true
				continue
			}
			drainStopped = false
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
	maxBytes, maxFiles := consoleRetentionOf(info)
	return maxBytes, maxFiles, nil
}

// consoleRetentionOf is consoleRetention of the container whose record is
// info: its log options, which its creation fixed.
func consoleRetentionOf(info ttcbundle.ContainerInfo) (int64, int) {
	maxBytes := info.ConsoleLogMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultConsoleLogMax
	}
	maxFiles := info.ConsoleLogMaxFiles
	if maxFiles < 1 {
		maxFiles = defaultConsoleLogFiles
	}
	return maxBytes, maxFiles
}

// ensureConsoleDrainIn is ensureConsoleDrain of a branch of the container
// whose record, info, the caller has: it reads no record, if info is target's
// container's.
func (b *runtimeBackend) ensureConsoleDrainIn(info ttcbundle.ContainerInfo, target selector, reset bool) (ttcbundle.ConsolePaths, error) {
	if b.dataRoot == "" || info.ID != target.container {
		return b.ensureConsoleDrain(target, reset)
	}
	paths, err := b.containerStore().BranchConsolePathsIn(info, target.branch)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	// As ensureConsoleDrain does, from info.
	if !reset {
		if branch, err := persistentBranchInfo(info, target.branch); err == nil && branch.State == "exited" && branch.ExitStatus != nil {
			if err := installRetainedConsoleExitCode(paths, *branch.ExitStatus); err != nil {
				return ttcbundle.ConsolePaths{}, err
			}
			return paths, nil
		}
	}
	return b.ensureConsoleDrainPaths(target, "", "init", paths, reset, &info)
}

func (b *runtimeBackend) ensureConsoleDrain(target selector, reset bool) (ttcbundle.ConsolePaths, error) {
	store := b.containerStore()
	paths, err := store.BranchConsolePaths(target.container, target.branch)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	// A short-lived detached init may exit between runsc start and console
	// follower registration. Its retained log is already authoritative and its
	// durable exit status is sufficient to complete log/attach readers; trying
	// to start another follower can only fail because runsc has removed the
	// exited container state. This mirrors completed named-exec handling below.
	if !reset {
		info, infoErr := store.LoadContainer(target.container)
		if infoErr == nil {
			branch, branchErr := persistentBranchInfo(info, target.branch)
			if branchErr == nil && branch.State == "exited" && branch.ExitStatus != nil {
				if err := installRetainedConsoleExitCode(paths, *branch.ExitStatus); err != nil {
					return ttcbundle.ConsolePaths{}, err
				}
				return paths, nil
			}
		}
	}
	return b.ensureConsoleDrainPaths(target, "", "init", paths, reset, nil)
}

// ensureExecConsoleDrain makes sure session, as the caller has just listed
// it, has a console drain.
func (b *runtimeBackend) ensureExecConsoleDrain(target selector, session runscExecSession, reset bool) (ttcbundle.ConsolePaths, error) {
	store := b.containerStore()
	paths, err := store.ExecConsolePaths(target.container, target.branch, session.ID)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	// A restored completed session has no live ThreadGroup to follow. Its
	// retained ring and status are already authoritative, so materialize the
	// ordinary host-side completion marker and let attach/log readers consume
	// them without spawning a doomed console follower. This also repairs old
	// node-local caches after rehydration on demand.
	if !reset && session.State == string(boot.ExecSessionExited) && session.ExitStatus != nil {
		if err := installRetainedConsoleWaitStatus(paths, *session.ExitStatus); err != nil {
			return ttcbundle.ConsolePaths{}, err
		}
		return paths, nil
	}
	return b.ensureConsoleDrainPaths(target, session.ID, session.ConsoleID, paths, reset, nil)
}

// ensureExecConsoleDrainIn is ensureExecConsoleDrain of a branch of the
// container whose record, info, the caller has: it reads no record, if info
// is target's container's.
func (b *runtimeBackend) ensureExecConsoleDrainIn(info ttcbundle.ContainerInfo, target selector, session runscExecSession, reset bool) (ttcbundle.ConsolePaths, error) {
	if b.dataRoot == "" || info.ID != target.container {
		return b.ensureExecConsoleDrain(target, session, reset)
	}
	paths, err := b.containerStore().ExecConsolePathsIn(info, target.branch, session.ID)
	if err != nil {
		return ttcbundle.ConsolePaths{}, err
	}
	// As ensureExecConsoleDrain does.
	if !reset && session.State == string(boot.ExecSessionExited) && session.ExitStatus != nil {
		if err := installRetainedConsoleWaitStatus(paths, *session.ExitStatus); err != nil {
			return ttcbundle.ConsolePaths{}, err
		}
		return paths, nil
	}
	return b.ensureConsoleDrainPaths(target, session.ID, session.ConsoleID, paths, reset, &info)
}

// refreshExecSessions returns sessions as target lists them now, or as they
// were if it no longer lists them.
func (b *runtimeBackend) refreshExecSessions(target selector, sessions []runscExecSession) []runscExecSession {
	listed, err := b.execSessions(target)
	if err != nil {
		return sessions
	}
	byID := make(map[string]runscExecSession, len(listed))
	for _, session := range listed {
		byID[session.ID] = session
	}
	refreshed := make([]runscExecSession, len(sessions))
	for index, session := range sessions {
		if current, ok := byID[session.ID]; ok {
			session = current
		}
		refreshed[index] = session
	}
	return refreshed
}

// installRetainedConsoleExitCode is installRetainedConsoleWaitStatus for a
// process known by its exit code, as a branch's record keeps it: the wait
// status recorded must give that code.
func installRetainedConsoleExitCode(paths ttcbundle.ConsolePaths, code int) error {
	current, found, err := readConsoleWaitStatus(paths.WaitStatus)
	if err != nil {
		return err
	}
	if found {
		if waitStatusExitCode(current) != code {
			return fmt.Errorf("retained wait status %d is not of exit code %d", current, code)
		}
		return nil
	}
	status := uint32(code&0xff) << 8
	if code > 128 && code < 128+65 {
		// Killed by the signal.
		status = uint32(code - 128)
	}
	return installRetainedConsoleWaitStatus(paths, status)
}

func installRetainedConsoleWaitStatus(paths ttcbundle.ConsolePaths, status uint32) error {
	if current, found, err := readConsoleWaitStatus(paths.WaitStatus); err != nil {
		return err
	} else if found {
		if current != status {
			return fmt.Errorf("retained exec wait status is %d, want %d", current, status)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(paths.WaitStatus), 0700); err != nil {
		return err
	}
	data := []byte(strconv.FormatUint(uint64(status), 10) + "\n")
	if err := os.WriteFile(paths.WaitStatus, data, 0600); err != nil {
		return fmt.Errorf("restoring retained exec wait status: %w", err)
	}
	return nil
}

// ensureConsoleDrainPaths starts a drain of the console at paths. The log
// options of the container that info records, if it is not nil, bound the
// console's log; else those of target's container, read.
func (b *runtimeBackend) ensureConsoleDrainPaths(target selector, execID, consoleID string, paths ttcbundle.ConsolePaths, reset bool, info *ttcbundle.ContainerInfo) (ttcbundle.ConsolePaths, error) {
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
			// The console is followed: by its family's follower, or by a
			// follower of its own an older ttcd started.
			if b.consoleSidecars != nil && !b.consoleSidecars.ObserveFamilyConsole(paths.FamilyDir, paths.Lock, target, execID == "") {
				b.consoleSidecars.ObserveInheritedConsoleSidecar(paths.Lock, target, execID == "")
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
	var maxBytes int64
	var maxFiles int
	if info != nil {
		maxBytes, maxFiles = consoleRetentionOf(*info)
	} else if maxBytes, maxFiles, err = b.consoleRetention(target.container); err != nil {
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
	if err := errorFile.Close(); err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("closing prepared console error log: %w", err)
	}
	follow := ttcconsole.Follow{
		Branch: target.branch, ExecID: execID, ConsoleID: consoleID, Lock: paths.Lock,
		Log: paths.Log, LogMaxBytes: maxBytes, LogMaxFiles: maxFiles, RetentionStats: paths.RetentionStats,
		WaitStatus: paths.WaitStatus, Error: paths.Error,
	}
	start := func() error { return b.startFamilyConsoleFollower(target.container, paths.FamilyDir) }
	if b.consoleSidecars != nil {
		err = b.consoleSidecars.FollowFamilyConsole(paths.FamilyDir, start, follow, lockFile, target, execID == "")
	} else {
		err = followFamilyConsoleOnce(paths.FamilyDir, start, follow, lockFile)
	}
	if err != nil {
		return ttcbundle.ConsolePaths{}, fmt.Errorf("starting console drain for %s: %w: %v", target, err, consoleDrainStoppedError(paths.Error))
	}
	return paths, nil
}

// startFamilyConsoleFollower starts the console follower of container, whose
// console directory is dir, and waits until it listens: after the follower an
// older one was, if it is exiting.
func (b *runtimeBackend) startFamilyConsoleFollower(container, dir string) error {
	errorPath := filepath.Join(dir, ".follower.error.log")
	errorFile, err := os.OpenFile(errorPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("opening console follower error log: %w", err)
	}
	defer errorFile.Close()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating console follower readiness pipe: %w", err)
	}
	defer readyReader.Close()
	command := runscCommand(b.runsc, append(b.runtimeControlArgs(), "timetravel",
		"--console-socket", familyFollowerSocket(dir), "--console-lock", filepath.Join(dir, ttcconsole.FollowerLockName),
		"--console-ready-fd", "3", container, "console-follow-family")...)
	command.Stderr = errorFile
	// A follower mostly waits, polling pipes: one processor serves it.
	command.Env = append(os.Environ(), "GOMAXPROCS=1")
	command.ExtraFiles = []*os.File{readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return fmt.Errorf("starting the console follower of %s: %w", container, err)
	}
	readyWriter.Close()
	fail := func(err error) error {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("starting the console follower of %s: %w: %v", container, err, consoleDrainStoppedError(errorPath))
	}
	if err := readyReader.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return fail(err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyReader, ready[:]); err != nil {
		return fail(err)
	}
	if b.consoleSidecars != nil {
		// The daemon reaps it; it outlives the daemon otherwise. It lives as
		// long as its family: a Wait alone would hold a thread all that time.
		go func() {
			awaitExit(command.Process.Pid)
			_ = command.Wait()
		}()
		return nil
	}
	return command.Process.Release()
}

// followFamilyConsoleOnce is FollowFamilyConsole for a backend outside a
// daemon, which observes no console's end.
func followFamilyConsoleOnce(dir string, start func() error, follow ttcconsole.Follow, lock *os.File) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var client *familyFollowerClient
		client, err = dialFamilyFollower(dir, nil, nil)
		if err != nil {
			if err = start(); err != nil {
				return err
			}
			if client, err = dialFamilyFollower(dir, nil, nil); err != nil {
				return err
			}
		}
		err = client.call(ttcconsole.FollowerMessage{Op: ttcconsole.FollowerFollow, Follow: &follow}, lock)
		_ = client.conn.Close()
		if err == nil || (!errors.Is(err, errFollowerGone) && err.Error() != errFollowerExiting) {
			return err
		}
		<-client.gone
	}
	return err
}

// stopFamilyConsoleOnce is StopFamilyConsole for a backend outside a daemon.
func stopFamilyConsoleOnce(dir, key string) error {
	client, err := dialFamilyFollower(dir, nil, nil)
	if err != nil {
		return nil
	}
	defer client.conn.Close()
	if err := client.call(ttcconsole.FollowerMessage{Op: ttcconsole.FollowerStop, Lock: key}); err != nil && !errors.Is(err, errFollowerGone) {
		return err
	}
	return nil
}

// retireFamilyConsolesOnce is RetireFamilyConsoles for a ttc that owns no
// followers.
func retireFamilyConsolesOnce(dir string) error {
	client, err := dialFamilyFollower(dir, nil, nil)
	if err != nil {
		return nil
	}
	defer client.conn.Close()
	return retireFamilyFollower(client)
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
		if err := store.RecordBranchStart(target.container, target.branch, b.nodeID); err != nil && b.stderr != nil {
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
				mode += ",lazy_copy"
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
	if stateError == "" && state == "exited" {
		stateError = container.ExitError
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
			if mount.RW {
				// A writable bind of the host path itself.
				semantics = "shared-external-writable-view"
			}
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
	memorySwap := int64(0)
	if container.MemorySwap != nil {
		memorySwap = *container.MemorySwap
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
			OOMKilled: state == "exited" && container.OOMKilled, Dead: state == "failed" || state == "destroying", Pid: sandboxPID, ExitCode: exitCode,
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
			Binds: binds, AutoRemove: container.AutoRemove, NetworkMode: container.NetworkMode, PortBindings: portBindings, LogConfig: logConfig, ExtraHosts: container.ExtraHosts, DNS: container.DNS, DNSSearch: container.DNSSearch, DNSOptions: container.DNSOptions, ReadonlyRootfs: readonlyRootfs, StopTimeout: stopTimeout, Runtime: "runsc", PidsLimit: container.PidsLimit, Memory: memory, MemorySwap: memorySwap, NanoCPUs: nanoCPUs, CPUShares: cpuShares, CPUPeriod: cpuPeriod, CPUQuota: cpuQuota, ShmSize: shmSize, GroupAdd: container.GroupAdd, CapAdd: container.CapAdd, CapDrop: container.CapDrop, Ulimits: container.Ulimits, Tmpfs: tmpfs,
		},
		Mounts: mounts,
		TTC: TTCInspectInfo{
			Container: container, Runtime: family, Branches: branches, ExternalViews: externalViews, RuntimeError: runtimeErr,
		},
	}
}

func (b *runtimeBackend) inspect(name string) (InspectInfo, error) {
	return b.inspectFamily(name, true)
}

// inspectStates is inspect of the family's configuration and the states of
// its branches alone: a command that resolves its targets by them, as a stop
// does, had every running branch's processes listed, on other nodes too, and
// every branch's console output looked at.
func (b *runtimeBackend) inspectStates(name string) (InspectInfo, error) {
	return b.inspectFamily(name, false)
}

// inspectFamily is inspect, of the branches' processes and console output
// too if all.
func (b *runtimeBackend) inspectFamily(name string, all bool) (InspectInfo, error) {
	store := ttcbundle.Store{Root: b.dataRoot}
	containerInfo, err := store.LoadContainer(name)
	if err != nil {
		return InspectInfo{}, err
	}
	if containerInfo.IsDistributedPhysicalRuntime() {
		return InspectInfo{}, fmt.Errorf("container %q does not exist: %w", name, os.ErrNotExist)
	}
	b.resolvedFamilies.resolved(name, containerInfo)
	spec, err := store.LoadSpecOf(containerInfo)
	if err != nil {
		return InspectInfo{}, err
	}
	var reads familyReads
	families, err := b.listFamiliesReading(containerInfo.ID, &reads)
	if err != nil {
		return buildInspectInfo(containerInfo, spec, nil, err.Error()), nil
	}
	// listFamilies may reconcile persistent state after an asynchronous sandbox
	// exit. Its last read of the record, or else a reload, reflects that
	// update.
	if reads.record != nil {
		containerInfo = *reads.record
	} else if refreshed, err := store.LoadContainer(containerInfo.ID); err == nil {
		containerInfo = refreshed
	}
	// Each branch below resolves in the record as loaded, which the authority's
	// projection of its branches would mislead, and in the family's placements
	// and branch authorities, which a cluster node's listing read.
	record := containerInfo
	authority := reads.authority
	if b.nodeID != "" {
		if b.distributedSource != nil && b.distributedSource.authority != nil {
			if authority != nil {
				containerInfo.Branches = logicalPersistentBranchInfos(containerInfo.Branches, authority.PlacementList(), authority.OfflineNodes(), containerInfo.ID)
			}
		} else if placements, openErr := b.placementCatalog(); openErr == nil {
			if records, offlineOwners, listErr := placementsWithOfflineOwners(placements); listErr == nil {
				containerInfo.Branches = logicalPersistentBranchInfos(containerInfo.Branches, records, offlineOwners, containerInfo.ID)
			}
		}
	}
	var runtimeFamily *Family
	for i := range families {
		if families[i].ID == containerInfo.ID {
			runtimeFamily = &families[i]
			break
		}
	}
	projectContainerAuthority(&containerInfo, runtimeFamily)
	result := buildInspectInfo(containerInfo, spec, runtimeFamily, "")
	if !all {
		return result, nil
	}
	if transactionID := containerInfo.DistributedForkTransactionID; transactionID != "" {
		if store, err := b.transactionCatalog(); err == nil {
			if transaction, err := store.Load(transactionID); err == nil {
				result.TTC.DistributedFork = transaction
			}
		}
	}
	// The running branches of other nodes, by node.
	var remoteNodes []string
	remoteBranches := make(map[string][]int)
	for i := range result.TTC.Branches {
		branch := &result.TTC.Branches[i]
		// A standalone daemon's branches run in the family's own runtime,
		// whose record is loaded already; a cluster's may have moved to a
		// physical runtime of their own.
		physical := selector{container: containerInfo.ID, branch: branch.Metadata.ID}
		var resolveErr error
		if b.nodeID != "" {
			var resolved selector
			if resolved, _, resolveErr = b.resolveSelectorIn(record, physical, authority); resolveErr == nil {
				physical = resolved
			}
		}
		paths := b.containerStore().BranchConsolePathsOf(physical.container, physical.branch)
		if stats, statsErr := ttcconsole.ReadRetentionStats(paths.RetentionStats); statsErr != nil {
			branch.OutputRetentionError = statsErr.Error()
		} else if stats.EvictedGenerations != 0 || stats.EvictedBytes != 0 {
			branch.OutputRetention = &stats
		}
		execStats, execRetentionErr := b.containerStore().ListExecConsoleRetentionStatsOf(physical.container, physical.branch)
		if execRetentionErr != nil {
			branch.ExecOutputRetentionError = execRetentionErr.Error()
		} else {
			branch.ExecOutputRetention = execStats
		}
		if branch.Metadata.State != "running" && branch.Metadata.State != "paused" {
			continue
		}
		if resolveErr != nil {
			branch.ProcessError = resolveErr.Error()
			// Its node lists them below.
			var remote *remoteBranchOwnershipError
			if errors.As(resolveErr, &remote) {
				if _, ok := remoteBranches[remote.owner]; !ok {
					remoteNodes = append(remoteNodes, remote.owner)
				}
				remoteBranches[remote.owner] = append(remoteBranches[remote.owner], i)
			}
			continue
		}
		processes, err := b.processesOf(physical)
		if err != nil {
			branch.ProcessError = err.Error()
			continue
		}
		branch.Processes = processes
	}
	b.listRemoteProcesses(containerInfo.ID, result.TTC.Branches, remoteNodes, remoteBranches)
	return result, nil
}

// remoteProcessesTimeout bounds how long inspect waits for another node to
// list the processes of its branches.
const remoteProcessesTimeout = 10 * time.Second

// branchProcesses is the processes of a branch, or why they could not be
// listed.
type branchProcesses struct {
	Processes []ProcessInfo `json:"processes,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// listRemoteProcesses lists the processes of the branches of family, of
// branches, that run on other nodes: each node of nodes, at once, lists those
// of its branches, whose indexes byNode has. A branch whose node does not list
// them keeps its process error, and says why.
func (b *runtimeBackend) listRemoteProcesses(family string, branches []InspectBranch, nodes []string, byNode map[string][]int) {
	if len(nodes) == 0 || b.distributedSource == nil {
		return
	}
	forEachConcurrently(len(nodes), len(nodes), func(n int) {
		node, indexes := nodes[n], byNode[nodes[n]]
		listed, err := b.remoteProcesses(node, family, branches, indexes)
		for _, i := range indexes {
			branch := &branches[i]
			switch entry, ok := listed[branch.Metadata.ID]; {
			case err != nil:
				branch.ProcessError = fmt.Sprintf("%s; listing them there: %v", branch.ProcessError, err)
			case !ok:
			case entry.Error != "":
				branch.ProcessError = entry.Error
			default:
				branch.Processes, branch.ProcessError = entry.Processes, ""
			}
		}
	})
}

// remoteProcesses asks node for the processes of family's branches, of
// branches, that indexes names.
func (b *runtimeBackend) remoteProcesses(node, family string, branches []InspectBranch, indexes []int) (map[string]branchProcesses, error) {
	peer, err := b.distributedSource.peer(node, false)
	if err != nil {
		return nil, err
	}
	args := []string{"_processes-local", family}
	for _, i := range indexes {
		args = append(args, branches[i].Metadata.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteProcessesTimeout)
	defer cancel()
	status, stdout, stderr, err := executeRemoteDaemonRequest(ctx, peer, daemonRequest{
		Version: daemonAPIVersion, Action: "command", Args: args, DistributedLeaf: true,
	})
	if err != nil {
		return nil, err
	}
	if status != 0 {
		return nil, fmt.Errorf("status %d: %s", status, strings.TrimSpace(stderr))
	}
	var listed map[string]branchProcesses
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		return nil, fmt.Errorf("decoding the processes listed: %w", err)
	}
	return listed, nil
}

// runProcessesLocal lists, as JSON, the processes of branches of a family that
// run on this node, for a node that inspects the family: each branch's, or why
// they could not be listed.
func runProcessesLocal(args []string, stdout io.Writer, backend backend) error {
	if len(args) < 2 {
		return errors.New("usage: ttc _processes-local CONTAINER BRANCH [BRANCH ...]")
	}
	lister, ok := backend.(interface {
		processesOfBranches(container string, branches []string) map[string]branchProcesses
	})
	if !ok {
		return errors.New("this backend does not list processes for another node")
	}
	return json.NewEncoder(stdout).Encode(lister.processesOfBranches(args[0], args[1:]))
}

// processesOfBranches is processes of each of container's branches, which it
// resolves in one read of the family's record and, on a cluster node, of its
// placements and branch authorities.
func (b *runtimeBackend) processesOfBranches(container string, branches []string) map[string]branchProcesses {
	listed := make(map[string]branchProcesses, len(branches))
	info, err := b.resolveContainer(container)
	if err != nil {
		for _, branch := range branches {
			listed[branch] = branchProcesses{Error: err.Error()}
		}
		return listed
	}
	var authority *cluster.AuthorityState
	if b.nodeID != "" && b.distributedSource != nil && b.distributedSource.authority != nil {
		query := cluster.PlacementsWithOwners
		query.Family = info.ID
		// Without it, each branch's are read.
		authority, _ = b.distributedSource.authority.AuthorityStateOf(query)
	}
	for _, branch := range branches {
		var entry branchProcesses
		target, _, err := b.resolveSelectorIn(info, selector{container: container, branch: branch}, authority)
		if err == nil {
			entry.Processes, err = b.processesOf(target)
		}
		if err != nil {
			entry.Error = err.Error()
		}
		listed[branch] = entry
	}
	return listed
}

// projectContainerAuthority removes stale ownership and lifecycle fields from
// the shared physical materialization before it is exposed by inspect. A
// clustered container.json is deliberately not rewritten on every migration;
// listFamilies has already merged authority placement and branch authority into
// family, so it is the single public source for these duplicated summary
// fields. Branch details were projected above by logicalPersistentBranchInfos.
func projectContainerAuthority(container *ttcbundle.ContainerInfo, family *Family) {
	if container == nil || family == nil {
		return
	}
	container.NodeID = family.NodeID
	container.State = family.State
}

func (b *runtimeBackend) remove(name string, force, volumes bool) error {
	return b.removeFamily(name, force, volumes, nil)
}

// removeFamily is remove of the family name. If known, the caller's record of
// it, is name's, the caller has renewed the node's execution lease for its
// command, as a command's removals do once (branchRemovals), and the record
// is not read again.
func (b *runtimeBackend) removeFamily(name string, force, volumes bool, known *ttcbundle.ContainerInfo) error {
	if known != nil && known.ID != name {
		known = nil
	}
	if known == nil && b.distributedSource != nil && b.distributedSource.authority != nil && b.nodeID != "" {
		if _, err := b.renewDistributedExecutionAuthority(); err != nil {
			return fmt.Errorf("renewing removal execution authority: %w", err)
		}
	}
	store := b.containerStore()
	var info ttcbundle.ContainerInfo
	var err error
	if known != nil {
		info = *known
	} else {
		info, err = store.LoadContainer(name)
		if errors.Is(err, os.ErrNotExist) {
			if found, err := b.removeRecordlessRuntime(name, force); found || err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		b.resolvedFamilies.resolved(name, info)
	}
	if waited, err := b.waitBusy(info.ID); err != nil {
		return err
	} else if waited {
		if info, err = store.LoadContainer(name); err != nil {
			return err
		}
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
	// Fence asynchronous publication while the runtime and its local graph are
	// still intact. The authority atomically chooses cancellation or completed durable
	// publication; either outcome is safe to remove after the queue drains.
	if b.publicationQueue != nil {
		canceled := 0
		for _, branch := range info.Branches {
			n, err := b.publicationQueue.CancelBranch(info.ID, branch.ID)
			if err != nil {
				return fmt.Errorf("canceling publications for %s@%s: %w", info.Name, branch.ID, err)
			}
			canceled += n
		}
		// A publication under way may have changed the record.
		if canceled != 0 {
			info, err = store.LoadContainer(info.ID)
			if err != nil {
				return err
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
	families, err := b.listRuntimeFamiliesOf(runtimeID)
	if err != nil {
		return fmt.Errorf("checking runtime state before removing %q: %w", name, err)
	}
	// The host frees the family's memory as its Sentry exits, which takes
	// time in proportion to it: other commands go on meanwhile, as they do
	// while the graph lets the memory go.
	if err := b.withoutLifecycleLock(info, longOperation{state: "removing", shown: true}, func() error {
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
		return nil
	}); err != nil {
		return err
	}
	graphRoots := make([]string, 0)
	seenGraphRoots := make(map[string]struct{})
	for _, branch := range info.Branches {
		for _, root := range branchGraphRootNames(branch) {
			if _, exists := seenGraphRoots[root]; exists {
				continue
			}
			seenGraphRoots[root] = struct{}{}
			graphRoots = append(graphRoots, root)
		}
	}
	for _, backing := range info.RuntimeMemoryBackings {
		for _, root := range branchGraphRootNames(runtimeBackingAsBranch(backing)) {
			if _, exists := seenGraphRoots[root]; exists {
				continue
			}
			seenGraphRoots[root] = struct{}{}
			graphRoots = append(graphRoots, root)
		}
	}
	for _, branch := range info.Branches {
		if graphReferenceOperationForBranch(info, branch) == "" {
			if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(branch), branchGraphRootObjects(branch)); err != nil {
				return fmt.Errorf("releasing shared graph roots for %s@%s: %w", info.Name, branch.ID, err)
			}
		}
		if err := b.releaseDistributedGraphReferencesEventually(graphReferenceOperationForBranch(info, branch)); err != nil {
			return fmt.Errorf("releasing distributed graph origins for %s@%s: %w", info.Name, branch.ID, err)
		}
	}
	for _, backing := range info.RuntimeMemoryBackings {
		branch := runtimeBackingAsBranch(backing)
		if graphReferenceOperationForBranch(info, branch) == "" {
			if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(branch), branchGraphRootObjects(branch)); err != nil {
				return fmt.Errorf("releasing shared runtime memory backing %s for %s: %w", backing.ID, info.Name, err)
			}
		}
		if err := b.releaseDistributedGraphReferencesEventually(graphReferenceOperationForBranch(info, branch)); err != nil {
			return fmt.Errorf("releasing distributed runtime memory backing %s for %s: %w", backing.ID, info.Name, err)
		}
	}
	// Resolve shared-ledger references while the authority still identifies local
	// provider origins. Local collection may abandon an empty volatile origin;
	// after that absence deliberately means "read from shared" to consumers.
	// Both steps take time in proportion to the family's memory and storage,
	// and the family has no runtime left to change, so other commands go on
	// meanwhile.
	if err := b.withoutLifecycleLock(info, longOperation{state: "removing", shown: true}, func() error {
		if err := b.releaseGraphMemoryRootsThroughHandoffs(graphRoots); err != nil {
			return fmt.Errorf("reclaiming graph memory for family %q: %w", name, err)
		}
		return store.RemoveContainer(runtimeID)
	}); err != nil {
		return err
	}
	if err := b.removeContainerPlacements(info); err != nil {
		return fmt.Errorf("removing cluster placement for %q: %w", name, err)
	}
	if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
		return fmt.Errorf("reconciling filesystem origins after removing %q: %w", name, err)
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

// removeContainerPlacements commits logical deletion after physical cleanup.
// A migrated runtime may store transaction-scoped physical branch names, so
// candidates are matched by both logical branch and exact physical identity.
// Removal must never depend on a later reconciliation pass: daemon shutdown
// can immediately follow rm, and stale authority would then resurrect work in
// the drain planner.
func (b *runtimeBackend) removeContainerPlacements(info ttcbundle.ContainerInfo) error {
	if b.nodeID == "" {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	logicalFamily := logicalFamilyID(info)
	records, err := placements.ListFamily(logicalFamily)
	if err != nil {
		return err
	}
	physicalBranches := make(map[string]struct{}, len(info.Branches))
	for _, branch := range info.Branches {
		physicalBranches[branch.ID] = struct{}{}
	}
	var removed []cluster.PlacementRecord
	for _, record := range records {
		if record.FamilyID != logicalFamily {
			continue
		}
		_, logicalMatch := physicalBranches[record.BranchID]
		_, physicalMatch := physicalBranches[record.PhysicalBranch]
		exactPhysicalMatch := record.PhysicalContainer == info.ID && physicalMatch
		physicalRuntime := info.IsDistributedPhysicalRuntime()
		if physicalRuntime && !exactPhysicalMatch {
			continue
		}
		if !physicalRuntime && !logicalMatch && !exactPhysicalMatch {
			continue
		}
		if record.OwnerNode != "" && record.OwnerNode != b.nodeID {
			return fmt.Errorf("branch %s@%s is still owned by node %q", record.FamilyID, record.BranchID, record.OwnerNode)
		}
		removed = append(removed, record)
	}
	// Each removal is a proposal, and they are independent: they go at once,
	// for the authority to commit together, rather than one after another,
	// each listing every transaction.
	work := make(chan cluster.PlacementRecord)
	errs := make([]error, min(len(removed), staleOriginWorkers))
	var doneMu sync.Mutex
	var done []cluster.PlacementRecord
	var workers sync.WaitGroup
	for worker := range errs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for record := range work {
				var err error
				if record.State == cluster.PlacementCorrupted && b.distributedSource != nil && b.distributedSource.authority != nil {
					if err = b.distributedSource.authority.RemoveCorruptedBranch(record.FamilyID, record.BranchID); errors.Is(err, os.ErrNotExist) {
						err = nil
					}
				} else {
					err = placements.Remove(record.FamilyID, record.BranchID, record.Generation)
				}
				if err != nil {
					errs[worker] = errors.Join(errs[worker], err)
					continue
				}
				doneMu.Lock()
				done = append(done, record)
				doneMu.Unlock()
			}
		}()
	}
	for _, record := range removed {
		work <- record
	}
	close(work)
	workers.Wait()
	return errors.Join(append(errs, b.retireTransactionsForRemovedPlacements(done))...)
}

// removeLogicalBranchPlacement commits deletion of one user-visible branch.
//
// Most placement cleanup can recover the logical identity from the physical
// container and branch recorded in the authority. A durable publication deliberately
// clears that physical identity when it makes a branch unplaced, however. At
// that point the lifecycle command's original selector is the only exact
// logical identity available. Treating the published physical container as
// authoritative made removal appear to succeed while leaving its unplaced
// authority record behind.
func (b *runtimeBackend) removeLogicalBranchPlacement(familyID, branchID string) error {
	if b.nodeID == "" {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	record, err := placements.Lookup(familyID, branchID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.OwnerNode != "" && record.OwnerNode != b.nodeID {
		return fmt.Errorf("branch %s@%s is still owned by node %q", familyID, branchID, record.OwnerNode)
	}
	if record.State == cluster.PlacementCorrupted && b.distributedSource != nil && b.distributedSource.authority != nil {
		if err := b.distributedSource.authority.RemoveCorruptedBranch(familyID, branchID); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := placements.Remove(familyID, branchID, record.Generation); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return b.retireTransactionsForRemovedPlacement(*record)
}

// claimUnplacedForRemoval claims an unplaced branch, which any node may claim
// to start or rehydrate it, for the removal under way on this node, before
// the removal deletes any of it: a start elsewhere then waits for the removal
// as for any claim, and a claim another node made first is waited for. A
// removal that deleted the branch's record first, and then found the branch
// claimed, failed, and left a placement that no command could load or
// remove; a start that had read the record before could have run the branch
// from the storage the removal deleted.
func (b *runtimeBackend) claimUnplacedForRemoval(familyID, branchID, physicalContainer, physicalBranch string) error {
	if b.nodeID == "" || b.distributedSource == nil || b.distributedSource.directory == nil {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	delay := 25 * time.Millisecond
	for {
		record, err := placements.Lookup(familyID, branchID)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		target := selector{container: familyID, branch: branchID}
		switch {
		case record.State == cluster.PlacementUnplaced:
			node, err := b.renewDistributedExecutionAuthority()
			if err != nil {
				return fmt.Errorf("renewing placement authority to remove %s: %w", target, err)
			}
			// A node that takes no placement, a gateway or one draining,
			// cannot claim the branch, and removes it unclaimed, as before:
			// a gateway's removal of a dehydrated family was refused, "node
			// is not accepting placement".
			if !node.AcceptsPlacement(time.Now()) {
				return nil
			}
			_, claimErr := placements.ClaimUnplaced(familyID, branchID, node.NodeID, node.Incarnation, physicalContainer, physicalBranch, record.Generation, cluster.PlacementMaterializing, time.Now())
			if claimErr == nil {
				return nil
			}
			// Another node may have claimed it first; a claim that fails
			// with the placement as it was fails for good.
			current, lookupErr := placements.Lookup(familyID, branchID)
			if lookupErr == nil && current.State == cluster.PlacementUnplaced && current.Generation == record.Generation {
				return fmt.Errorf("claiming %s for removal: %w", target, claimErr)
			}
			continue
		case record.State == cluster.PlacementMaterializing && record.OwnerNode != b.nodeID:
			if time.Since(record.UpdatedAt) >= materializingClaimWait {
				return &branchMaterializingError{branch: target, owner: record.OwnerNode, claimedAt: record.UpdatedAt}
			}
		case (record.State == cluster.PlacementRunning || record.State == cluster.PlacementPaused) && record.OwnerNode != b.nodeID:
			// A start elsewhere ran it, having claimed it as the removal was
			// routed here.
			return &remoteBranchOwnershipError{target: target, owner: record.OwnerNode}
		default:
			return nil
		}
		time.Sleep(delay)
		delay = min(2*delay, 500*time.Millisecond)
	}
}

// distributedFamilyHasPeerPlacement reports whether a physical runtime's
// final branch is only locally final, while another logical branch in the same
// family still exists elsewhere in the cluster. Physical containers are an
// implementation detail of migration and remote fork: deleting a one-branch
// physical container must not delete the canonical family record while a
// sibling placement survives on another node.
func (b *runtimeBackend) distributedFamilyHasPeerPlacement(familyID, branchID string) (bool, error) {
	if b.nodeID == "" {
		return false, nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return false, err
	}
	records, err := placements.ListFamily(familyID)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.BranchID != branchID {
			return true, nil
		}
	}
	return false, nil
}

// removeCorruptedBranch removes a branch whose execution authority has
// already been permanently fenced by the authority. Corruption intentionally clears
// OwnerNode and every physical runtime identity: contacting the former owner
// would both violate that fence and make rm wait for a process that is known
// to be gone.
//
// The operation is idempotent across crashes. The authority is changed first; a retry
// can reconstruct the transaction match from the logical selector and finish
// pruning the shared family catalog even though the placement no longer
// exists. The family record itself remains until the coordinator has removed
// the final logical placement.
func (b *runtimeBackend) removeCorruptedBranch(target selector) error {
	if b.nodeID == "" || b.distributedSource == nil || b.distributedSource.authority == nil {
		return errors.New("corrupted-branch removal requires clustered authority")
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	removed := cluster.PlacementRecord{FamilyID: target.container, BranchID: target.branch}
	if record, lookupErr := placements.Lookup(target.container, target.branch); lookupErr == nil {
		if record.State != cluster.PlacementCorrupted {
			return fmt.Errorf("branch %s is %s, not corrupted", target, record.State)
		}
		removed = *record
	} else if !errors.Is(lookupErr, os.ErrNotExist) {
		return lookupErr
	}
	if err := b.distributedSource.authority.RemoveCorruptedBranch(target.container, target.branch); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := b.retireTransactionsForRemovedPlacement(removed); err != nil {
		return fmt.Errorf("retiring transactions for %s: %w", target, err)
	}

	// The shared catalog is display and restart metadata, not execution
	// authority. Bound it to the live branch set without deleting the family
	// record that a later child in the same family rm still needs to resolve.
	shared := ttcbundle.Store{Root: b.dataRoot}
	if info, loadErr := shared.LoadRuntimeContainer(target.container); loadErr == nil {
		found := false
		for _, branch := range info.Branches {
			if branch.ID == target.branch {
				found = true
				break
			}
		}
		if found && len(info.Branches) > 1 {
			if err := shared.RemoveBranchRecord(info.ID, target.branch); err != nil {
				return fmt.Errorf("removing shared branch metadata for %s: %w", target, err)
			}
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	// A branch of a remote fork's is recorded in its physical runtime's
	// record, where it stayed corrupted after its removal: a peer of unknown
	// state, it refused every start of the runtime's other branches.
	if physical := removed.PhysicalContainer; physical != "" && physical != target.container {
		physicalBranch := removed.PhysicalBranch
		if physicalBranch == "" {
			physicalBranch = target.branch
		}
		info, loadErr := shared.LoadRuntimeContainer(physical)
		if loadErr == nil && len(info.Branches) > 1 && slices.ContainsFunc(info.Branches, func(branch ttcbundle.BranchInfo) bool { return branch.ID == physicalBranch }) {
			if err := shared.RemoveBranchRecord(info.ID, physicalBranch); err != nil {
				return fmt.Errorf("removing physical branch metadata for %s: %w", target, err)
			}
		} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
	}
	_, err = b.reconcileVolatileSnapshotOrigins()
	return err
}

// removeDistributedFamilyRecord deletes the canonical, non-executing family
// record after every authority placement has been removed through its exact
// physical branch. A family whose canonical container hosted the final branch
// is already absent, making this operation naturally idempotent.
func (b *runtimeBackend) removeDistributedFamilyRecord(containerID string, volumes bool) error {
	store := b.containerStore()
	info, err := store.LoadRuntimeContainer(containerID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDistributedPhysicalRuntime() {
		return fmt.Errorf("container %q is a physical distributed runtime, not a canonical family record", containerID)
	}
	if b.nodeID != "" {
		placements, err := b.placementCatalog()
		if err != nil {
			return err
		}
		records, err := placements.ListFamily(info.ID)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			return fmt.Errorf("cannot remove family record %q while branch %q still has placement state %s", info.Name, records[0].BranchID, records[0].State)
		}
		containers, err := familyContainersForPlacement(store, info.ID)
		if err != nil {
			return err
		}
		for _, candidate := range containers {
			if candidate.ID != info.ID {
				// With no remaining authority placement, this is an inert physical
				// catalog left by a completed migration, canceled fork, or lost
				// runtime. Per-node cleanup has already fenced and removed any
				// corresponding local shell; prune the shared identity now.
				if err := store.RemoveContainer(candidate.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing stale physical runtime record %q: %w", candidate.ID, err)
				}
			}
		}
	}
	if err := store.RemoveContainer(info.ID); err != nil {
		return err
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

// reclaimRemovedFamilyLocal drops node-local runtime shells and cache entries
// only after the authority proves that the logical family has no placements anywhere.
// physicalIDs are captured before the shared family catalog is removed, so a
// remote node never has to infer ownership from an already-deleted record.
func (b *runtimeBackend) reclaimRemovedFamilyLocal(familyID string, physicalIDs []string) error {
	if b.nodeID == "" || b.localRoot == "" {
		return errors.New("removed-family local cleanup requires a clustered node-local root")
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	records, err := placements.ListFamily(familyID)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return fmt.Errorf("family %q still has placement for branch %q", familyID, records[0].BranchID)
	}
	live, err := b.runtimeSandboxIDs()
	if err != nil {
		return fmt.Errorf("listing runtimes before local family cleanup: %w", err)
	}
	seen := make(map[string]struct{}, len(physicalIDs))
	for _, physicalID := range physicalIDs {
		if physicalID == "" {
			continue
		}
		if _, ok := seen[physicalID]; ok {
			continue
		}
		seen[physicalID] = struct{}{}
		if _, ok := live[physicalID]; ok {
			if err := b.removeOrphanRuntime(physicalID); err != nil {
				return fmt.Errorf("removing unplaced runtime %q: %w", physicalID, err)
			}
			continue
		}
		if err := b.stopContainerConsoleDrains(physicalID); err != nil {
			return fmt.Errorf("stopping console followers for unplaced runtime %q: %w", physicalID, err)
		}
		if err := b.containerStore().RemoveActiveContainerState(physicalID); err != nil {
			return fmt.Errorf("removing local cache for unplaced runtime %q: %w", physicalID, err)
		}
	}
	return nil
}

// purgeRuntime removes only this node's physical Sentry/gofer shell. Shared
// family metadata and branch filesystems are intentionally untouched; elastic
// migration and family-removal coordinators use this after ownership moved.
func (b *runtimeBackend) purgeRuntime(containerID string) error {
	sandboxes, err := b.runtimeSandboxIDs()
	if err != nil {
		return fmt.Errorf("listing runtimes before purging %q: %w", containerID, err)
	}
	if _, ok := sandboxes[containerID]; ok {
		if _, err := b.command("delete", "--force", containerID); err != nil {
			return fmt.Errorf("purging physical runtime %q: %w", containerID, err)
		}
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
		// Moved aside before it is deleted, as any family's node-local state
		// is: a repository of it that an inventory of the node's opened
		// meanwhile made a file in the directory being deleted, and the
		// removal failed, "directory not empty".
		localErr = b.containerStore().RemoveActiveContainerState(containerID)
	}
	return errors.Join(consoleErr, deleteErr, localErr)
}

// removeRecordlessRuntime removes the sandbox that identifier names by its ID
// or a unique ID prefix, if runsc lists that sandbox but no container record
// describes it: its record went with the TTC_ROOT that held it, or was lost
// before a daemon reconciled the sandbox's exit. ps lists such a sandbox by
// its ID. A sandbox that still runs is removed only with force. It reports
// whether identifier named such a sandbox.
func (b *runtimeBackend) removeRecordlessRuntime(identifier string, force bool) (bool, error) {
	states, err := b.listRuntimeSandboxes()
	if err != nil {
		return false, fmt.Errorf("listing sandboxes: %w", err)
	}
	var matches []runtimeSandboxState
	for _, state := range states {
		if state.ID == identifier {
			matches = []runtimeSandboxState{state}
			break
		}
		if identifier != "" && strings.HasPrefix(state.ID, identifier) {
			matches = append(matches, state)
		}
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) > 1 {
		return true, fmt.Errorf("container ID prefix %q is ambiguous", identifier)
	}
	state := matches[0]
	if _, err := b.containerStore().LoadRuntimeContainer(state.ID); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if placed, err := b.runtimePlaced(state.ID); err != nil {
		return true, fmt.Errorf("checking the placements of sandbox %q: %w", state.ID, err)
	} else if placed {
		return false, nil
	}
	if _, busy := b.operations.lookup(state.ID); busy {
		return true, fmt.Errorf("sandbox %q is busy", state.ID)
	}
	if !force && normalizeLifecycleState(state.Status) != "exited" && processutil.IsRunning(state.PID) {
		return true, fmt.Errorf("container %q has no record, but its sandbox is %s; use --force to remove it", state.ID, normalizeLifecycleState(state.Status))
	}
	return true, b.removeOrphanRuntime(state.ID)
}

// runtimePlaced reports whether a cluster placement names id as the physical
// runtime of a branch.
func (b *runtimeBackend) runtimePlaced(id string) (bool, error) {
	if b.nodeID == "" {
		return false, nil
	}
	var records []cluster.PlacementRecord
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		snapshot, err := b.distributedSource.authority.AuthorityStateOf(cluster.PlacementsWithOwners)
		if err != nil {
			return false, err
		}
		records = snapshot.PlacementList()
	} else {
		placements, err := b.placementCatalog()
		if err != nil {
			return false, err
		}
		if records, _, err = placementsWithOfflineOwners(placements); err != nil {
			return false, err
		}
	}
	for _, record := range records {
		if record.PhysicalContainer == id {
			return true, nil
		}
	}
	return false, nil
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

// runtimeControlArgs returns the global arguments required by runsc commands
// that operate on an existing sandbox. The daemon's trusted-mode decision is
// process-wide configuration on Darwin, so it must be carried by sidecars and
// read-only lifecycle commands just as it is by create and start.
func (b *runtimeBackend) runtimeControlArgs() []string {
	args := []string{"--root", b.root}
	if b.unsafeNoHostIsolation {
		args = append(args, "--rootless=false", "--unsafe-no-host-isolation=true")
	}
	return args
}

func (b *runtimeBackend) configuredRunscArgsForNetwork(networkMode string) []string {
	return b.configuredRunscArgsForNetworkNAT(networkMode, false)
}

func (b *runtimeBackend) configuredRunscArgsForNetworkNAT(networkMode string, networkNAT bool) []string {
	return b.configuredRunscArgsForNetworkConfig(networkMode, networkNAT, "", "", "")
}

func (b *runtimeBackend) configuredRunscArgsForNetworkConfig(networkMode string, networkNAT bool, networkIP, networkMAC, networkInterface string) []string {
	runtimePlatform := b.runtimePlatform
	if runtimePlatform == "" {
		runtimePlatform = defaultRuntimePlatform
	}
	args := []string{
		"--root", b.root,
		"--platform=" + runtimePlatform,
		// Preserve Sentry runtime failures even when the OCI console and control
		// channel both disappear. Multiple sandboxes append complete Go runtime
		// reports to this daemon-local diagnostic file.
		"--panic-log=" + filepath.Join(b.root, "sentry-panic.log"),
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
		"--overlay2=all:memory",
		// Application memory on a huge-page tmpfs of the sandbox's own: a
		// memfd's pages are huge only where the host's shmem_enabled allows
		// it, which is never on the hosts EDA tools run on, and without huge
		// pages their large heaps lose a fifth of their speed to TLB misses.
		"--memory-file-huge-tmpfs=true")
	if size, known := dentryCacheSize(); known {
		// One cache for the sandbox rather than one per mount, so that what it
		// costs the gofer does not grow with the number of things mounted.
		args = append(args, "--dcache="+strconv.Itoa(size))
	}
	if runtimePlatform == "systrap" {
		// Private pages in address order: systrap maps each run with one
		// host mapping, of which the host allows a process only so many.
		args = append(args, "--memory-homes=true")
		// A family that runs out of memory loses the process holding the
		// most, and goes on: the host would kill that process's stub, which
		// frees none of its memory, and go on killing others.
		args = append(args, "--sentry-oom-kill=true")
	}
	if b.disableHostGSO {
		args = append(args, "--gso=false")
	}
	if b.disableSyscallPatching && runtimePlatform == "systrap" {
		args = append(args, "--systrap-disable-syscall-patching=true")
	}
	if networkMode == "ttc" {
		args = append(args, "--ttc-network-nat="+strconv.FormatBool(networkNAT))
		if networkIP != "" {
			args = append(args, "--ttc-network-ip="+networkIP)
		}
		if networkMAC != "" {
			args = append(args, "--ttc-network-mac="+networkMAC)
		}
		if networkInterface != "" {
			args = append(args, "--ttc-network-interface="+networkInterface)
		}
	}
	return append(args, testOnlyRunscFlags()...)
}

// testOnlyRunscFlags returns the runsc flags in TTC_TESTONLY_RUNSC_FLAGS that
// tests may set: limits that stand in for the host's where they are lower,
// so that tests reach them, which they cannot set on the host.
func testOnlyRunscFlags() []string {
	var flags []string
	for _, flag := range strings.Fields(os.Getenv("TTC_TESTONLY_RUNSC_FLAGS")) {
		if strings.HasPrefix(flag, "--fdlimit=") || strings.HasPrefix(flag, "--TESTONLY-host-mapping-limit=") {
			flags = append(flags, flag)
		}
	}
	return flags
}

// withRuntimeDiagnostics has the Sentry that args launch write its panics to
// a log of the container's own, in which runtimeDiagnosticSince finds them,
// on a host whose Sentries do so; see runtimeDiagnosticsPerContainer.
func (b *runtimeBackend) withRuntimeDiagnostics(args []string, containerID string) ([]string, error) {
	if !runtimeDiagnosticsPerContainer || b.dataRoot == "" || containerID == "" {
		return args, nil
	}
	directory := filepath.Join(b.dataRoot, "containers", containerID, "runtime")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("creating runtime diagnostic directory for %q: %w", containerID, err)
	}
	return append(args, "--panic-log="+filepath.Join(directory, "panic.log")), nil
}

func (b *runtimeBackend) runtimeDiagnosticSince(containerID string, since time.Time) string {
	if !runtimeDiagnosticsPerContainer || b.dataRoot == "" || containerID == "" {
		return ""
	}
	path := filepath.Join(b.dataRoot, "containers", containerID, "runtime", "panic.log")
	info, err := os.Stat(path)
	if err != nil || info.ModTime().Before(since.Add(-time.Second)) {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	const tailBytes = int64(64 << 10)
	start := max(int64(0), info.Size()-tailBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, tailBytes))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.HasPrefix(line, "ttc ring0 bridge error:") ||
			strings.HasPrefix(line, "ttc hvf application error:") {
			if len(line) > 4096 {
				line = line[:4096]
			}
			return line
		}
	}
	return ""
}

func (b *runtimeBackend) configuredRunscArgsForBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) ([]string, error) {
	args := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC, info.TTCNetworkInterface())
	if branch.Quarantined {
		args = append(args, "--ttc-network-quarantine=true")
	}
	if rawSocketsRequested(info.CapAdd, info.CapDrop) {
		args = append(args, "--net-raw=true")
	}
	gfxArgs, err := b.gfx.runscArgs(info.GPUOptions)
	if err != nil {
		return nil, err
	}
	nvproxyArgs, err := b.nvproxy.runscArgs(info.GPUOptions)
	if err != nil {
		return nil, err
	}
	args = append(args, gfxArgs...)
	return append(args, nvproxyArgs...), nil
}

// rawSocketsRequested returns whether a container's capability options ask
// for CAP_NET_RAW explicitly. runsc drops CAP_NET_RAW, which Docker's
// defaults include, unless raw sockets are on: TTC turns them on for
// containers that add the capability, by name or with ALL, as Android's
// netd, which requires it, needs.
func rawSocketsRequested(capAdd, capDrop []string) bool {
	added := false
	for _, c := range capAdd {
		if c == "ALL" || c == "NET_RAW" {
			added = true
		}
	}
	for _, c := range capDrop {
		if c == "ALL" || c == "NET_RAW" {
			return false
		}
	}
	return added
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
	status, _, err := b.runConfiguredCommandWithArgsAndDiagnostic(configured, args...)
	return status, err
}

// runConfiguredCommandWithArgsAndDiagnostic is the cluster-control variant of
// runConfiguredCommandWithArgs. Cluster target operations have no attached TTC
// client stream, so their peer response must retain the launch diagnostic
// instead of reducing every runtime failure to an opaque exit status.
func (b *runtimeBackend) runConfiguredCommandWithArgsAndDiagnostic(configured []string, args ...string) (int, string, error) {
	if bundle := bundleOf(args); bundle != "" {
		triggerBindMountSources(bundle)
	}
	args = append(configured, args...)
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return 1, "", fmt.Errorf("opening runsc launch stdout: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp("", "ttc-runsc-launch-*.log")
	if err != nil {
		return 1, "", fmt.Errorf("creating runsc launch stderr: %w", err)
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
		return 1, "", fmt.Errorf("rewinding runsc launch stderr: %w", err)
	}
	const maxLaunchDiagnosticBytes = 64 << 10
	diagnosticBytes, err := io.ReadAll(io.LimitReader(stderr, maxLaunchDiagnosticBytes))
	if err != nil {
		return 1, "", fmt.Errorf("reading runsc launch stderr: %w", err)
	}
	diagnostic := strings.TrimSpace(string(diagnosticBytes))
	if b.stderr != nil {
		if _, err := stderr.Seek(0, io.SeekStart); err != nil {
			return 1, diagnostic, fmt.Errorf("rewinding runsc launch stderr for replay: %w", err)
		}
		if _, err := io.Copy(b.stderr, stderr); err != nil {
			return 1, diagnostic, fmt.Errorf("replaying runsc launch stderr: %w", err)
		}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), diagnostic, nil
		}
		return 1, diagnostic, fmt.Errorf("starting runsc: %w", runErr)
	}
	return 0, diagnostic, nil
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

// preflightHostResourceLimits reports missing host cgroup delegation before
// runsc starts a sandbox. Hard memory and CPU limits are host-enforced; the
// Sentry cannot safely pretend that they are active when the daemon's cgroup
// does not permit child creation. runsc also fails closed, but its first
// failing ancestor otherwise leaks a low-level cgroup.subtree_control error to
// the user after doing most of container creation.
func preflightHostResourceLimits(bundleDir string) error {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return fmt.Errorf("reading OCI config for resource preflight: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("decoding OCI config for resource preflight: %w", err)
	}
	if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.CgroupsPath == "" {
		return nil
	}
	resources := spec.Linux.Resources
	required := make([]string, 0, 2)
	if resources.Memory != nil && resources.Memory.Limit != nil && *resources.Memory.Limit > 0 {
		required = append(required, "memory")
	}
	if resources.CPU != nil {
		required = append(required, "cpu")
	}
	if len(required) == 0 {
		return nil
	}
	if runtime.GOOS == "darwin" {
		// Without this, runsc starts the sandbox without the limits, and only
		// their verification, which reads Linux's /proc, fails.
		return fmt.Errorf("hard %s limits require host cgroups, which macOS does not have; omit --memory and CPU limit options", strings.Join(required, " and "))
	}
	return preflightCgroupV2Delegation("/proc/self/cgroup", "/sys/fs/cgroup", required)
}

// delegatedCgroupParent returns the cgroup v2 directory in which runsc creates
// a sandbox's relative cgroup: the parent of the daemon's own leaf cgroup.
func delegatedCgroupParent(procCgroupPath, mountRoot string) (string, error) {
	data, err := os.ReadFile(procCgroupPath)
	if err != nil {
		return "", fmt.Errorf("reading daemon cgroup membership: %w", err)
	}
	membership := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			membership = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if membership == "" {
		return "", errors.New("hard host resource limits require unified cgroup v2 delegation, but ttcd has no unified cgroup membership")
	}
	// Relative runsc cgroups are siblings of the daemon's leaf cgroup. In a
	// cgroup namespace the host membership may not exist below the visible
	// mount; in that case the mount root itself is the delegated parent.
	visibleMembership := filepath.Join(mountRoot, filepath.Clean("/"+membership))
	if _, err := os.Stat(visibleMembership); err == nil {
		return filepath.Dir(visibleMembership), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking daemon cgroup %q: %w", visibleMembership, err)
	}
	return mountRoot, nil
}

// effectiveMemorySwap is the memory-swap limit a new family gets, as Docker
// gives it: the one asked for, or twice its memory limit. A kernel that does
// not account swap to cgroups cannot hold a family to one, and, as Docker
// does, TTC then says so and limits only memory.
func effectiveMemorySwap(memoryLimit *int64, swap int64, swapSet bool, stderr io.Writer) *int64 {
	memory := int64(0)
	if memoryLimit != nil {
		memory = *memoryLimit
	}
	if memory == 0 && !swapSet {
		return nil
	}
	effective := ttcbundle.EffectiveMemorySwap(memory, swap)
	if memory > 0 && effective > 0 && !cgroupSwapAccounted("/proc/self/cgroup", "/sys/fs/cgroup") {
		fmt.Fprintln(stderr, "ttc: warning: this kernel does not account swap to cgroups, so memory is limited without limiting swap")
		effective = -1
	}
	return &effective
}

// cgroupSwapAccounted reports whether the kernel accounts swap to the cgroups
// runsc creates for sandboxes, as their parent's memory.swap.max shows. With
// no memory interface to judge by, as on cgroup v1, it lets runsc decide.
func cgroupSwapAccounted(procCgroupPath, mountRoot string) bool {
	parent, err := delegatedCgroupParent(procCgroupPath, mountRoot)
	if err != nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(parent, "memory.max")); err != nil {
		return true
	}
	_, err = os.Stat(filepath.Join(parent, "memory.swap.max"))
	return err == nil
}

func preflightCgroupV2Delegation(procCgroupPath, mountRoot string, required []string) error {
	if _, err := os.Stat(filepath.Join(mountRoot, "cgroup.controllers")); errors.Is(err, os.ErrNotExist) {
		// The host may use cgroup v1. Let runsc's native implementation decide
		// whether the requested controllers are available there.
		return nil
	} else if err != nil {
		return fmt.Errorf("checking unified cgroup v2: %w", err)
	}
	parent, err := delegatedCgroupParent(procCgroupPath, mountRoot)
	if err != nil {
		return err
	}
	controllersData, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("reading delegated cgroup controllers from %q: %w", parent, err)
	}
	available := make(map[string]struct{})
	for _, controller := range strings.Fields(string(controllersData)) {
		available[controller] = struct{}{}
	}
	for _, controller := range required {
		if _, ok := available[controller]; !ok {
			return fmt.Errorf("hard host resource limits require delegated cgroup v2 controller %q, but %q exposes %q", controller, parent, strings.TrimSpace(string(controllersData)))
		}
	}
	control := filepath.Join(parent, "cgroup.subtree_control")
	if err := unix.Access(control, unix.W_OK); err != nil {
		return fmt.Errorf("hard host resource limits require a writable delegated cgroup v2 subtree: ttcd cannot write %q: %w; start ttcd in a delegated cgroup (for example, a systemd unit with Delegate=yes) or omit --memory and CPU limit options", control, err)
	}
	if err := unix.Access(parent, unix.W_OK); err != nil {
		return fmt.Errorf("hard host resource limits require a writable delegated cgroup v2 subtree: ttcd cannot create child cgroups in %q: %w; start ttcd in a delegated cgroup (for example, a systemd unit with Delegate=yes) or omit --memory and CPU limit options", parent, err)
	}
	return nil
}

// launch starts target and reports whether it remains running when this call
// returns. The latter is intentionally distinct from the requested detach
// flag: an attached terminal may detach through its detach key sequence.
func (b *runtimeBackend) launch(info ttcbundle.ContainerInfo, target selector, bundleDir string, detach, interactive bool) (status int, running bool, err error) {
	name := info.ID
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return 1, false, err
	}
	if err := preflightHostResourceLimits(bundleDir); err != nil {
		return 1, false, err
	}
	configuredArgs, err := b.configuredRunscArgsForBranch(info, branch)
	if err != nil {
		return 1, false, err
	}
	configuredArgs, err = b.withRuntimeDiagnostics(configuredArgs, info.ID)
	if err != nil {
		return 1, false, err
	}
	status, err = b.runConfiguredCommandWithArgs(configuredArgs, "create", "--bundle", bundleDir, name)
	if err != nil || status != 0 {
		return status, false, err
	}
	if err := b.verifyHostResourceLimits(name, bundleDir); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, false, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, false, err
	}
	startDrain := func(phase string) error {
		if _, err := b.ensureConsoleDrainIn(info, target, true); err != nil {
			return fmt.Errorf("starting console drain %s container start: %w", phase, err)
		}
		return nil
	}
	if err := startDrain("before"); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, false, fmt.Errorf("%w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, false, err
	}
	status, err = b.runConfiguredCommandWithArgs(configuredArgs, "start", name)
	if err != nil || status != 0 {
		if err == nil {
			err = fmt.Errorf("runsc start exited with status %d", status)
		}
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return status, false, fmt.Errorf("starting container: %w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return status, false, err
	}
	if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
		_, cleanupErr := b.command("delete", "--force", name)
		if cleanupErr != nil {
			return 1, false, fmt.Errorf("publishing ports: %w (runtime cleanup also failed: %v)", err, cleanupErr)
		}
		return 1, false, fmt.Errorf("publishing ports: %w", err)
	}
	b.signalLifecycleReady()
	if detach {
		return 0, true, nil
	}
	waitStatus, err := b.attach(attachRequest{target: target, stdin: interactive, stdout: true, stderr: true, detachKeys: defaultDetachKeys})
	if err != nil {
		if errors.Is(err, errDetached) {
			return 0, true, nil
		}
		return 1, false, err
	}
	return waitStatusExitCode(waitStatus), false, nil
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
	if b.portIngress.Clustered() {
		// Cluster ingress listeners are explicit node-local ephemeral objects.
		// They are created by run -p or `ttc port publish`, not reconstructed
		// from shared container metadata on a target or daemon restart.
		return nil
	}
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
	// Listener locks and logs are live node-local state. Publishing them in the
	// shared catalog both creates unnecessary small-file traffic and exposes
	// removal to NFS open-unlink semantics.
	lockPath, errorPath := portSidecarPaths(b.activeDataRoot(), info.ID, binding)
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
	args := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC, info.TTCNetworkInterface())
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
	if err := recordPortSidecarPID(lockFile, command.Process.Pid); err != nil {
		readyWriter.Close()
		command.Process.Kill()
		command.Wait()
		return fmt.Errorf("recording published-port process: %w", err)
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
	families, err := b.listRuntimeFamiliesOf(target.container)
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
	if b.usesEphemeralPortIngress() {
		return b.portIngress.Publish(target, binding)
	}
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
	if b.usesEphemeralPortIngress() {
		ephemeral, err := b.portIngress.List(container)
		if err != nil {
			return nil, err
		}
		// Cluster ingress bindings are intentionally node-local and
		// ephemeral. A remote branch's physical bundle is not present on
		// this ingress node and must not be consulted for display.
		return ephemeral, nil
	}
	info, err := b.resolveContainer(container)
	if err != nil {
		return nil, err
	}
	return append([]ttcbundle.PortBinding(nil), info.PublishedPorts...), nil
}

func recordPortSidecarPID(lockFile *os.File, pid int) error {
	if err := lockFile.Truncate(0); err != nil {
		return err
	}
	if _, err := lockFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(lockFile, "%d\n", pid); err != nil {
		return err
	}
	return lockFile.Sync()
}

func portLockHolder(lockPath, container, branch, endpoint string) (int, error) {
	if data, err := os.ReadFile(lockPath); err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 && pid != os.Getpid() && portForwardProcessMatches(pid, container, branch, endpoint) {
			return pid, nil
		}
	}
	return legacyPortLockHolder(lockPath, container, branch, endpoint)
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
	lockPath, errorPath := portSidecarPaths(b.activeDataRoot(), info.ID, binding)
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
	if b.usesEphemeralPortIngress() {
		if err := b.portIngress.Unpublish(target, binding); err == nil {
			return nil
		} else if !errors.Is(err, errIngressPortNotFound) {
			return err
		}
	}
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

// usesEphemeralPortIngress reports whether published ports belong only to the
// daemon process that accepted the request. Cluster ingress is deliberately
// node-local: each accepted connection resolves the branch's current
// placement in the authority, so it follows migration without rewriting container metadata.
// Standalone TTC has no placement catalog and therefore persists bindings in
// the container record so daemon reconciliation can restore their listeners.
func (b *runtimeBackend) usesEphemeralPortIngress() bool {
	return b.portIngress.Clustered()
}

func (b *runtimeBackend) pull(reference, platform string, progress bool) (ttcimages.Info, error) {
	var progressFunc ttcimages.ProgressFunc
	if progress {
		progressFunc = b.imageProgress()
	}
	ctx := context.Background()
	var info ttcimages.Info
	// Downloading and unpacking take time in proportion to the image; the
	// image store and its roots have locks of their own.
	if err := b.withoutLifecycleLock(b.imageOperation(), longOperation{state: "pulling"}, func() error {
		var err error
		info, err = ttcimages.Pull(ctx, ttcimages.PullOptions{
			Root:      b.dataRoot,
			Reference: reference,
			Platform:  platform,
			Progress:  progressFunc,
		})
		if err != nil {
			return err
		}
		if _, _, err := b.containerStore().EnsureImageRoot(ctx, info, platform, progressFunc); err != nil {
			return fmt.Errorf("preparing pulled image root: %w", err)
		}
		return nil
	}); err != nil {
		return ttcimages.Info{}, err
	}
	if _, err := b.containerStore().PruneImageRoots(ctx); err != nil {
		return ttcimages.Info{}, fmt.Errorf("pruning superseded image roots: %w", err)
	}
	return info, nil
}

// loadImages loads the images of archive, or else of the file the client
// passed, or of its standard input.
func (b *runtimeBackend) loadImages(archive io.Reader, platform string) ([]ttcimages.Info, error) {
	if archive == nil && b.archiveFile != nil {
		info, err := b.archiveFile.Stat()
		if err != nil {
			return nil, err
		}
		archive = io.NewSectionReader(b.archiveFile, 0, info.Size())
	}
	if archive == nil {
		archive = b.stdin
	}
	if archive == nil {
		return nil, errors.New("no image archive on standard input")
	}
	ctx := context.Background()
	var imported []ttcimages.ImportedImage
	published := false
	defer func() {
		if !published {
			_ = ttcimages.DiscardLoaded(context.Background(), b.dataRoot)
			_, _ = b.containerStore().PruneImageRoots(context.Background())
		}
	}()
	infos := make([]ttcimages.Info, 0)
	// Importing and unpacking take time in proportion to the archive.
	if err := b.withoutLifecycleLock(b.imageOperation(), longOperation{state: "loading"}, func() error {
		var err error
		imported, err = ttcimages.Load(ctx, ttcimages.LoadOptions{
			Root: b.dataRoot, Archive: archive, Platform: platform,
		})
		if err != nil {
			return err
		}
		for _, image := range imported {
			if _, _, err := b.containerStore().EnsureImportedImageRoot(ctx, image, image.Info.Platform, nil); err != nil {
				return fmt.Errorf("preparing loaded image %q root: %w", image.Info.Reference, err)
			}
			infos = append(infos, image.Info)
		}
		return nil
	}); err != nil {
		return nil, err
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

// saveImages writes the images to archive, or to the file the client passed
// in place of its stream.
func (b *runtimeBackend) saveImages(archive io.Writer, identifiers []string, platform string) error {
	if b.archiveFile != nil {
		archive = io.NewOffsetWriter(b.archiveFile, 0)
	}
	// Exporting takes time in proportion to the images.
	return b.withoutLifecycleLock(b.imageOperation(), longOperation{state: "saving"}, func() error {
		return ttcimages.Save(context.Background(), ttcimages.SaveOptions{
			Root: b.dataRoot, Archive: archive, Images: identifiers, Platform: platform,
		})
	})
}

func (b *runtimeBackend) command(args ...string) ([]byte, error) {
	return b.commandWithFiles(nil, args...)
}

func (b *runtimeBackend) commandWithFiles(files []*os.File, args ...string) ([]byte, error) {
	if len(files) == 0 && embeddedRuntime(b.runsc) {
		if output, answered, err := embeddedRuntimeCommand(b.root, args); answered {
			return output, err
		}
	}
	command := runscCommand(b.runsc, append(b.runtimeControlArgs(), args...)...)
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
	return owner
}

func containerHasBranchOnNode(info ttcbundle.ContainerInfo, nodeID string) bool {
	if nodeID == "" {
		return true
	}
	for _, branch := range info.Branches {
		owner := info.BranchNodeID(branch.ID)
		if owner == nodeID {
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

// placementOwnersLister is a placement catalog that also reports, from the
// same snapshot, which nodes are offline.
type placementOwnersLister interface {
	ListWithOfflineOwners() ([]cluster.PlacementRecord, map[string]bool, error)
}

// placementsWithOfflineOwners lists placements and, where the catalog can say
// so from the same snapshot, which nodes are offline.
func placementsWithOfflineOwners(placements cluster.PlacementCatalog) ([]cluster.PlacementRecord, map[string]bool, error) {
	if withOwners, ok := placements.(placementOwnersLister); ok {
		return withOwners.ListWithOfflineOwners()
	}
	records, err := placements.List()
	return records, nil, err
}

// placementDisplayState is the state a listing shows for record: a running or
// paused branch of an offline node is unavailable until the node returns, or
// its removal gives the branch up.
func placementDisplayState(record cluster.PlacementRecord, offlineOwners map[string]bool) string {
	if offlineOwners[record.OwnerNode] && (record.State == cluster.PlacementRunning || record.State == cluster.PlacementPaused) {
		return "unavailable"
	}
	return string(record.State)
}

func (b *runtimeBackend) listFamilies() ([]Family, error) {
	return b.listFamiliesOf("")
}

// listFamiliesOf is listFamilies, but it lists only the family with the given
// ID, if it is not empty: it reads no other family's record and asks no other
// family's Sentry for its branches.
//
// A cluster node reads its placements once. Listing every family, it reads
// them after its runtimes, so that it knows the placement of every physical
// runtime it lists; listing one family, it reads them first, to find which
// physical runtimes of the family it has.
func (b *runtimeBackend) listFamiliesOf(familyID string) ([]Family, error) {
	return b.listFamiliesReading(familyID, nil)
}

// familyReads is what listing one family read of it that the lister may
// use again.
type familyReads struct {
	// record is the family's record as the listing last read it, if it did.
	record *ttcbundle.ContainerInfo
	// authority is a cluster node's snapshot of the family's placements,
	// branches and nodes.
	authority *cluster.AuthorityState
}

// listFamiliesReading is listFamiliesOf, which, listing one family, keeps in
// reads, if it is not nil, what it read of it.
func (b *runtimeBackend) listFamiliesReading(familyID string, reads *familyReads) ([]Family, error) {
	var snapshot *cluster.AuthorityState
	var authorityErr error
	var placementRecords []cluster.PlacementRecord
	var offlineOwners map[string]bool
	placementsRead := false
	readPlacements := func() {
		placementsRead = true
		if b.nodeID == "" {
			return
		}
		var records []cluster.PlacementRecord
		if b.distributedSource != nil && b.distributedSource.authority != nil {
			query := cluster.PlacementsWithOwners
			query.Family = familyID
			if snapshot, authorityErr = b.distributedSource.authority.AuthorityStateOf(query); authorityErr != nil {
				return
			}
			records, offlineOwners = snapshot.PlacementList(), snapshot.OfflineNodes()
		} else if placements, err := b.placementCatalog(); err == nil {
			if records, offlineOwners, err = placementsWithOfflineOwners(placements); err != nil {
				return
			}
		}
		for _, record := range records {
			if familyID == "" || record.FamilyID == familyID {
				placementRecords = append(placementRecords, record)
			}
		}
	}
	var runtimeFamilies []Family
	var runtimeErr error
	if familyID != "" {
		ids := []string{familyID}
		if b.nodeID != "" {
			readPlacements()
			for _, record := range placementRecords {
				if record.OwnerNode == b.nodeID && record.PhysicalContainer != "" && !slices.Contains(ids, record.PhysicalContainer) {
					ids = append(ids, record.PhysicalContainer)
				}
			}
		}
		runtimeFamilies, runtimeErr = b.listRuntimeFamiliesOf(ids...)
	} else {
		runtimeFamilies, runtimeErr = b.listRuntimeFamilies()
	}
	if b.dataRoot == "" {
		return runtimeFamilies, runtimeErr
	}
	store := ttcbundle.Store{Root: b.dataRoot}
	var containers []ttcbundle.ContainerInfo
	if familyID == "" {
		var storeErr error
		if b.inventoryReadOnly && b.nodeID != "" && b.distributedSource != nil && b.distributedSource.authority != nil {
			// A cluster node's listing that repairs nothing shows most
			// families from the authority's placements: reading the record of
			// every family of the cluster from the shared root made its time.
			readPlacements()
			if authorityErr != nil {
				return nil, fmt.Errorf("reading cluster branch authority: %w", authorityErr)
			}
			containers, storeErr = store.ListContainersOf(familiesShownFromRecords(b.nodeID, snapshot, placementRecords, runtimeFamilies))
		} else {
			containers, storeErr = store.ListContainers()
		}
		if storeErr != nil {
			return nil, storeErr
		}
	} else if info, err := store.LoadContainer(familyID); err == nil {
		if !info.IsDistributedPhysicalRuntime() {
			containers = []ttcbundle.ContainerInfo{info}
			if reads != nil {
				reads.record = &info
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Resolve physical migration identities before constructing the persistent
	// view. A target imported into an existing Sentry is stored under a private
	// transaction branch name, even though the authority exposes only the stable logical
	// name. The private name must never enter user-facing inventory.
	if !placementsRead {
		readPlacements()
	}
	physicalPlacements := make(map[string][]cluster.PlacementRecord)
	logicalPlacements := make(map[string][]cluster.PlacementRecord)
	for _, record := range placementRecords {
		logicalPlacements[record.FamilyID] = append(logicalPlacements[record.FamilyID], record)
		if record.PhysicalContainer != "" {
			physicalPlacements[record.PhysicalContainer] = append(physicalPlacements[record.PhysicalContainer], record)
		}
	}
	byID := make(map[string]Family, len(containers)+len(runtimeFamilies))
	for _, container := range containers {
		if container.IsDistributedPhysicalRuntime() {
			// Per-node target records are addressed through authority placement. They
			// must not appear as additional user-visible families.
			continue
		}
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
			Branches:         persistentBranches(logicalPersistentBranchInfos(container.Branches, physicalPlacements[container.ID], offlineOwners, container.ID)),
		}
	}
	// A single catalog snapshot is authoritative for elastically placed branches.
	// Persistent container metadata describes cold branches, but must not expose
	// a half-published target during ownership cutover.
	if b.nodeID != "" {
		// A family shown from its placements is named after one that names
		// it, not after a runtime of it, as one made before
		// familyDisplayRecord may; see there.
		named := make(map[string]cluster.PlacementRecord)
		for _, record := range placementRecords {
			if current, ok := named[record.FamilyID]; record.FamilyName != "" && (!ok || current.FamilyName == current.PhysicalContainer) {
				named[record.FamilyID] = record
			}
		}
		for _, record := range placementRecords {
			family, ok := byID[record.FamilyID]
			if display, found := named[record.FamilyID]; !ok && found {
				family = Family{
					ID: record.FamilyID, Name: display.FamilyName, Image: display.ImageReference,
					Digest: display.ImageDigest, ImageSize: display.ImageSize,
					LiveForkEligible: display.LiveForkEligible, LiveForkWaiver: display.LiveForkWaiver,
					CreatedAt: display.FamilyCreatedAt,
				}
				ok = true
			}
			if !ok {
				continue
			}
			foundBranch := false
			for i := range family.Branches {
				if family.Branches[i].ID == record.BranchID {
					family.Branches[i].NodeID = record.OwnerNode
					family.Branches[i].State = placementDisplayState(record, offlineOwners)
					family.Branches[i].Quarantined = record.Quarantined
					foundBranch = true
					break
				}
			}
			if !foundBranch {
				storageState := "local"
				if record.State == cluster.PlacementUnplaced {
					storageState = "shared"
				}
				family.Branches = append(family.Branches, Branch{
					ID: record.BranchID, NodeID: record.OwnerNode, ParentID: record.ParentBranch,
					Quarantined: record.Quarantined, State: placementDisplayState(record, offlineOwners), StorageState: storageState, CreatedAt: record.BranchCreatedAt,
				})
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
			family.State = familyStateFromBranches(family.Branches)
			byID[record.FamilyID] = family
		}
	}
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		// In clustered TTC, the authority decides existence for every logical
		// branch, including cold/unplaced ones. container.json is a physical
		// materialization record and can lag a rename or removal on another node;
		// retaining names absent from the placement snapshot manufactures zombie
		// branches in ps/ls and can make later commands appear routable.
		for familyID, family := range byID {
			records := logicalPlacements[familyID]
			if len(records) == 0 {
				delete(byID, familyID)
				continue
			}
			family.Branches = branchesInPlacementSnapshot(family.Branches, records)
			family.State = familyStateFromBranches(family.Branches)
			byID[familyID] = family
		}
	}
	if runtimeErr == nil {
		listed := make(map[string]struct{}, len(containers))
		for _, container := range containers {
			listed[container.ID] = struct{}{}
		}
		runtimeIDs := make(map[string]struct{}, len(runtimeFamilies))
		for _, family := range runtimeFamilies {
			// A listed container is no physical runtime, and one placed
			// needs no telling.
			physicalRuntime := false
			if _, ok := listed[family.ID]; !ok && len(physicalPlacements[family.ID]) == 0 {
				runtimeInfo, runtimeInfoErr := store.LoadRuntimeContainer(family.ID)
				physicalRuntime = runtimeInfoErr == nil && runtimeInfo.IsDistributedPhysicalRuntime()
			}
			if placements := physicalPlacements[family.ID]; len(placements) != 0 {
				physicalID := family.ID
				family.ID = placements[0].FamilyID
				// Unmapped runtime branches are paused sparse providers from an
				// older migration incarnation or provisional targets whose
				// placement has not committed. They are implementation state, not
				// additional user-visible branches.
				family.Branches = visiblePhysicalRuntimeBranches(family.Branches, placements, physicalID)
			} else if physicalRuntime {
				// The authority only retains the current physical placement for a logical
				// branch. A completed migration's source runtime can therefore
				// remain visible to runsc briefly while its asynchronous cleanup
				// is retrying. It is internal implementation state, never a new
				// user-visible family.
				continue
			}
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
					if b.nodeID == "" || owner == b.nodeID {
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
			displayID := container.ID
			if container.DistributedFamilyID != "" {
				displayID = container.DistributedFamilyID
			}
			if _, ok := runtimeIDs[displayID]; ok {
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
			if _, busy := b.operations.lookup(container.ID); busy {
				continue
			}
			if b.inventoryReadOnly {
				b.inventoryRepairWanted = true
				continue
			}
			if err := b.recordLostRuntime(container); err != nil {
				continue
			}
			if reads != nil && container.ID == familyID {
				reads.record = nil
			}
			if reconciled, err := store.LoadContainer(container.ID); err == nil {
				if reads != nil && container.ID == familyID {
					reads.record = &reconciled
				}
				family := byID[displayID]
				if family.ID == "" {
					// A per-node physical runtime record is not itself a
					// user-visible family. If the logical catalog is temporarily
					// unavailable, do not manufacture an empty zombie row for it.
					continue
				}
				family.State = normalizeLifecycleState(reconciled.State)
				family.Branches = persistentBranches(logicalPersistentBranchInfos(reconciled.Branches, physicalPlacements[reconciled.ID], offlineOwners, reconciled.ID))
				family.SandboxPID = 0
				byID[displayID] = family
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
	// In clustered TTC, the authority decides lifecycle. Shared container JSON
	// is a materialization catalog and may lag a lease-expiry or migration
	// decision; never let it make a fenced/corrupted branch appear runnable.
	if b.nodeID != "" && b.distributedSource != nil && b.distributedSource.authority != nil {
		if authorityErr != nil {
			return nil, fmt.Errorf("reading cluster branch authority: %w", authorityErr)
		}
		placementByBranch := make(map[string]cluster.PlacementRecord, len(placementRecords))
		for _, record := range placementRecords {
			placementByBranch[record.FamilyID+"\x00"+record.BranchID] = record
		}
		for _, authority := range snapshot.BranchList() {
			family, ok := byID[authority.FamilyID]
			if !ok {
				continue
			}
			placement, placed := placementByBranch[authority.FamilyID+"\x00"+authority.BranchID]
			family = mergeAuthoritativeBranch(family, authority, placement, placed, b.dataRoot)
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
	b.showLongOperations(families)
	if reads != nil {
		reads.authority = snapshot
	}
	return families, nil
}

// familiesShownFromRecords returns the IDs of the families whose records a
// listing on node reads, of those the authority's snapshot names: those with
// a branch neither running nor paused, whose exit and storage the record
// has; those whose placements do not name the family, its image and its
// creation, as an older authority's do not; and the node's own families whose
// Sentry it does not see run, which it checks for a runtime lost. It shows the
// others from their placements.
func familiesShownFromRecords(node string, snapshot *cluster.AuthorityState, placements []cluster.PlacementRecord, runtimeFamilies []Family) []string {
	live := make(map[string]bool, len(runtimeFamilies))
	for _, family := range runtimeFamilies {
		if !runtimeFamilyUnavailable(family) {
			live[family.ID] = true
		}
	}
	read := make(map[string]bool)
	for _, placement := range placements {
		switch {
		case placement.FamilyName == "" || placement.FamilyCreatedAt.IsZero():
			read[placement.FamilyID] = true
		case placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused:
			read[placement.FamilyID] = true
		case placement.OwnerNode == node:
			physical := placement.PhysicalContainer
			if physical == "" {
				physical = placement.FamilyID
			}
			if !live[physical] {
				read[placement.FamilyID] = true
			}
		}
	}
	if snapshot != nil {
		for _, authority := range snapshot.BranchList() {
			if state := normalizeLifecycleState(authority.Lifecycle); state != "running" && state != "paused" {
				read[authority.FamilyID] = true
			}
		}
	}
	ids := make([]string, 0, len(read))
	for id := range read {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func branchesInPlacementSnapshot(branches []Branch, placements []cluster.PlacementRecord) []Branch {
	visible := make(map[string]struct{}, len(placements))
	for _, placement := range placements {
		visible[placement.BranchID] = struct{}{}
	}
	filtered := make([]Branch, 0, min(len(branches), len(visible)))
	for _, branch := range branches {
		if _, ok := visible[branch.ID]; ok {
			filtered = append(filtered, branch)
		}
	}
	return filtered
}

// mergeAuthoritativeBranch projects one authority branch record into the
// public family view. The authority decides existence and lifecycle in a
// cluster, so
// an unplaced migrated branch must be synthesized even when its durable bytes
// live in a hidden transaction-scoped container. Requiring a matching entry in
// the logical family's container.json made cold migrated branches disappear
// from ls/inspect and consequently made rehydrate impossible.
func mergeAuthoritativeBranch(family Family, authority cluster.BranchAuthority, placement cluster.PlacementRecord, placed bool, dataRoot string) Family {
	index := -1
	for i := range family.Branches {
		if family.Branches[i].ID == authority.BranchID {
			index = i
			break
		}
	}
	if index == -1 {
		family.Branches = append(family.Branches, Branch{ID: authority.BranchID})
		index = len(family.Branches) - 1
	}
	branch := &family.Branches[index]
	branch.State = authority.Lifecycle
	branch.PublicationID = authority.PublicationID
	if authority.Roots.FilesystemRoot != "" {
		branch.StorageManifest = filepath.Join(dataRoot, authority.Roots.FilesystemRoot)
	}
	if placed {
		branch.ParentID = placement.ParentBranch
		branch.Quarantined = placement.Quarantined
		if !placement.BranchCreatedAt.IsZero() {
			branch.CreatedAt = placement.BranchCreatedAt
		}
	}
	if !placed || placement.State == cluster.PlacementUnplaced || placement.State == cluster.PlacementCorrupted {
		branch.NodeID = ""
		if authority.Roots.FilesystemRoot != "" {
			branch.StorageState = "shared"
		}
	} else {
		branch.NodeID = placement.OwnerNode
		if branch.StorageState == "" {
			branch.StorageState = "local"
		}
	}
	if authority.Corrupted {
		family.Error = fmt.Sprintf("branch %s is corrupted: %s", authority.BranchID, authority.CorruptionReason)
	}
	sort.Slice(family.Branches, func(i, j int) bool { return family.Branches[i].ID < family.Branches[j].ID })
	return family
}

func visiblePhysicalRuntimeBranches(branches []Branch, placements []cluster.PlacementRecord, physicalContainer string) []Branch {
	visible := make([]Branch, 0, len(branches))
	for _, branch := range branches {
		for _, placement := range placements {
			if placement.PhysicalContainer != physicalContainer || placement.PhysicalBranch != branch.ID {
				continue
			}
			branch.ID = placement.BranchID
			visible = append(visible, branch)
			break
		}
	}
	return visible
}

// runtimeFamilyUnavailable reports whether a runsc list entry is only stale
// on-disk runtime metadata. A failed branch RPC alone is insufficient because
// the control server can be temporarily unavailable while the Sentry remains
// alive; the process-liveness check is the second required signal.
func runtimeFamilyUnavailable(family Family) bool {
	return family.Error != "" && !processutil.IsRunning(family.SandboxPID)
}

// runtimeFamilyLiveBranch names a branch that a runtime runs, or may run: one
// that could not list its branches may run any. It is "" for a runtime whose
// branches have all exited, dehydrated or failed.
func runtimeFamilyLiveBranch(family Family) string {
	if family.Error != "" {
		return defaultBranchID
	}
	for _, branch := range family.Branches {
		if runtimeBranchLive(branch) {
			return branch.ID
		}
	}
	return ""
}

// runtimeBranchLive reports whether a branch its runtime lists may run.
func runtimeBranchLive(branch Branch) bool {
	switch normalizeLifecycleState(branch.State) {
	case "exited", "dehydrated", "failed":
		return false
	}
	return true
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
		if ttcbundle.ResidentDehydration(branch) {
			return true
		}
	}
	return false
}

func enrichPersistentBranchDiagnostics(family *Family) {
	liveKernelsByNode := make(map[string]int)
	for i := range family.Branches {
		branch := &family.Branches[i]
		switch normalizeLifecycleState(branch.State) {
		case "running", "paused":
			liveKernelsByNode[branch.NodeID]++
		}
	}
	for i := range family.Branches {
		branch := &family.Branches[i]
		if branch.InitState == "" {
			switch normalizeLifecycleState(branch.State) {
			case "created", "creating":
				branch.InitState = "not-started"
			case "exited", "stopped", "failed":
				branch.InitState = "exited"
			case "running", "paused":
				// The authority lifecycle and placement are authoritative after a remote
				// fork or migration. A control-only node may not have a local
				// Sentry from which to enrich these fields, but a branch cannot
				// commit either state without a live PID 1 on its owner.
				branch.InitState = normalizeLifecycleState(branch.State)
				branch.InitPID = 1
			default:
				branch.InitState = "unknown"
			}
		}
		if branch.MemorySharing == "" {
			switch normalizeLifecycleState(branch.State) {
			case "created", "creating":
				branch.MemorySharing = "unallocated"
			case "exited", "stopped", "failed":
				branch.MemorySharing = "released"
			case "running", "paused":
				branch.MemorySharing = "private"
				if liveKernelsByNode[branch.NodeID] > 1 {
					branch.MemorySharing = "cow"
				}
			default:
				branch.MemorySharing = "unknown"
			}
		}
	}
}

type runtimeSandboxState struct {
	ID     string `json:"id"`
	PID    int    `json:"pid"`
	Status string `json:"status"`
}

// listRuntimeSandboxes reads only runsc's host-side state. It deliberately
// does not contact a Sentry control server, so teardown can still discover and
// force-delete a sandbox whose branch RPC endpoint is unavailable.
func (b *runtimeBackend) listRuntimeSandboxes() ([]runtimeSandboxState, error) {
	output, err := b.command("list", "--format=json", "--sandbox")
	if err != nil {
		return nil, err
	}
	var states []runtimeSandboxState
	if err := json.Unmarshal(output, &states); err != nil {
		return nil, fmt.Errorf("decoding runsc container list: %w", err)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states, nil
}

// listRuntimeSandboxesOf is listRuntimeSandboxes of only the sandboxes with the
// given IDs. With the runtime embedded in ttcd, it reads only their state.
func (b *runtimeBackend) listRuntimeSandboxesOf(ids []string) ([]runtimeSandboxState, error) {
	if !embeddedRuntime(b.runsc) {
		states, err := b.listRuntimeSandboxes()
		if err != nil {
			return nil, err
		}
		kept := states[:0]
		for _, state := range states {
			if slices.Contains(ids, state.ID) {
				kept = append(kept, state)
			}
		}
		return kept, nil
	}
	var states []runtimeSandboxState
	for _, id := range ids {
		// Like runsc list, pass over a sandbox that cannot be loaded, as one
		// being deleted.
		c, err := container.Load(b.root, container.FullID{SandboxID: id, ContainerID: id}, container.LoadOpts{Exact: true})
		if err != nil {
			continue
		}
		state := c.State()
		states = append(states, runtimeSandboxState{ID: state.ID, PID: state.Pid, Status: string(state.Status)})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states, nil
}

func (b *runtimeBackend) listRuntimeFamilies() ([]Family, error) {
	return b.listRuntimeFamiliesOf()
}

// listRuntimeFamiliesOf is listRuntimeFamilies of only the sandboxes with the
// given IDs, or of every sandbox if none is given. It asks no other sandbox
// about itself.
func (b *runtimeBackend) listRuntimeFamiliesOf(ids ...string) ([]Family, error) {
	if embeddedRuntime(b.runsc) {
		return embeddedRuntimeFamilies(b.root, ids)
	}
	var states []runtimeSandboxState
	var err error
	if len(ids) != 0 {
		states, err = b.listRuntimeSandboxesOf(ids)
	} else {
		states, err = b.listRuntimeSandboxes()
	}
	if err != nil {
		return nil, err
	}
	families := make([]Family, 0, len(states))
	for _, state := range states {
		if state.ID == "" {
			continue
		}
		output, err := b.command("timetravel", state.ID, "branches")
		families = append(families, runtimeFamily(state, output, err))
	}
	return families, nil
}

// branchesUnknown reports whether f is the family of a live runtime that
// could not list its branches: runtimeFamily gives it its main branch alone,
// as a stopped runtime's are, but it may run any.
func (f Family) branchesUnknown() bool {
	return f.Error != "" && (f.State == "running" || f.State == "paused")
}

// runtimeFamily is the family of the sandbox in state, of the branches its
// runtime listed in output, or of its main branch alone if the runtime could
// not list them, err saying why.
func runtimeFamily(state runtimeSandboxState, output []byte, err error) Family {
	family := Family{ID: state.ID, State: normalizeLifecycleState(state.Status), SandboxPID: state.PID, Branches: []Branch{}}
	if err != nil {
		family.Error = err.Error()
		family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: normalizeLifecycleState(state.Status)})
		return family
	}
	var branches []runscBranchInfo
	if err := json.Unmarshal(output, &branches); err != nil {
		family.Error = fmt.Sprintf("decoding runsc branch list: %v", err)
		family.Branches = append(family.Branches, Branch{ID: defaultBranchID, State: normalizeLifecycleState(state.Status)})
		return family
	}
	for _, branch := range branches {
		family.Branches = append(family.Branches, branchView(branch))
		if branch.ID == defaultBranchID {
			family.State = normalizeLifecycleState(branch.State)
		}
	}
	sort.Slice(family.Branches, func(i, j int) bool { return family.Branches[i].ID < family.Branches[j].ID })
	return family
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
	return b.execSessionsOf(resolved)
}

// execSessionsOf is execSessions of a branch whose physical runtime and branch
// target names already.
func (b *runtimeBackend) execSessionsOf(target selector) ([]runscExecSession, error) {
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
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return runscExecSession{}, err
	}
	return b.execSessionOf(resolved, execID)
}

// execSessionOf is execSession of a branch whose physical runtime and
// branch target names already.
func (b *runtimeBackend) execSessionOf(target selector, execID string) (runscExecSession, error) {
	sessions, err := b.execSessionsOf(target)
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
			Validation                    time.Duration `json:"Validation"`
			SharedMemoryPreparation       time.Duration `json:"SharedMemoryPreparation"`
			MappingInvalidation           time.Duration `json:"MappingInvalidation"`
			VFSPreparation                time.Duration `json:"VFSPreparation"`
			DevicePreparation             time.Duration `json:"DevicePreparation"`
			FilesystemMetadataFlush       time.Duration `json:"FilesystemMetadataFlush"`
			FilesystemRuntimeInvalidation time.Duration `json:"FilesystemRuntimeInvalidation"`
			FilesystemBranchSeal          time.Duration `json:"FilesystemBranchSeal"`
			FilesystemNamespacePrune      time.Duration `json:"FilesystemNamespacePrune"`
			MemoryPreparation             time.Duration `json:"MemoryPreparation"`
			MetadataStream                time.Duration `json:"MetadataStream"`
			MetadataBytes                 int64         `json:"MetadataBytes"`
			MetadataRecord                time.Duration `json:"MetadataRecord"`
			MetadataObjects               int64         `json:"MetadataObjects"`
			ChildRestore                  time.Duration `json:"ChildRestore"`
			Total                         time.Duration `json:"Total"`
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

func persistentBranchView(info ttcbundle.BranchInfo) Branch {
	return Branch{
		ID: info.ID, NodeID: info.NodeID, ParentID: info.ParentID, Quarantined: info.Quarantined, State: normalizeLifecycleState(info.State), CreatedAt: info.CreatedAt,
		ExitStatus: info.ExitStatus, StorageState: info.StorageState, PublicationID: info.PublicationID, StorageError: info.StorageError,
		StorageManifest: info.StorageManifest, SnapshotID: info.SnapshotID, MemoryRoots: append([]string(nil), info.MemoryRoots...),
		MemoryRootObjects: append([]string(nil), info.MemoryRootObjects...),
		ControlRoot:       info.ControlRoot, ControlRootObject: info.ControlRootObject,
		StorageRetryAt: info.StorageRetryAt, SourceNode: info.SourceNode, DependentMemoryBytes: info.DependentMemoryBytes,
		DependentFilesystemBytes: info.DependentFilesystemBytes,
		ForkTotalNS:              info.ForkTotalNS, SourceDowntimeNS: info.SourceDowntimeNS, KernelForkNS: info.KernelForkNS,
		ForkValidationNS: info.ForkValidationNS, ForkSharedMemoryNS: info.ForkSharedMemoryNS,
		ForkMappingInvalidationNS: info.ForkMappingInvalidationNS, ForkVFSPreparationNS: info.ForkVFSPreparationNS,
		ForkMemoryPreparationNS: info.ForkMemoryPreparationNS, ForkMetadataStreamNS: info.ForkMetadataStreamNS,
		ForkMetadataBytes: info.ForkMetadataBytes, ForkChildRestoreNS: info.ForkChildRestoreNS,
		ForkMetadataRecordNS: info.ForkMetadataRecordNS, ForkMetadataObjects: info.ForkMetadataObjects,
		ForkDevicePreparationNS:     info.ForkDevicePreparationNS,
		ForkFSMetadataFlushNS:       info.ForkFSMetadataFlushNS,
		ForkFSRuntimeInvalidationNS: info.ForkFSRuntimeInvalidationNS,
		ForkFSBranchSealNS:          info.ForkFSBranchSealNS,
		ForkFSNamespacePruneNS:      info.ForkFSNamespacePruneNS,
	}
}

func persistentBranches(infos []ttcbundle.BranchInfo) []Branch {
	branches := make([]Branch, 0, len(infos))
	for _, info := range infos {
		branches = append(branches, persistentBranchView(info))
	}
	return branches
}

// logicalPersistentBranchInfos removes transaction-scoped migration branch
// names from a user-facing persistent view. A physical migration branch is
// visible only through the authority placement that currently maps it to a stable
// logical identity. Superseded physical branches are provider/cleanup state and
// remain hidden even while their local metadata genuinely still exists.
func logicalPersistentBranchInfos(infos []ttcbundle.BranchInfo, placements []cluster.PlacementRecord, offlineOwners map[string]bool, physicalContainer string) []ttcbundle.BranchInfo {
	byPhysical := make(map[string]cluster.PlacementRecord, len(placements))
	byLogical := make(map[string]cluster.PlacementRecord, len(placements))
	authoritativeFamily := false
	for _, placement := range placements {
		if placement.PhysicalContainer == physicalContainer && placement.PhysicalBranch != "" {
			byPhysical[placement.PhysicalBranch] = placement
		}
		if placement.FamilyID == physicalContainer {
			authoritativeFamily = true
			byLogical[placement.BranchID] = placement
		}
	}
	result := make([]ttcbundle.BranchInfo, 0, len(infos))
	resultByLogical := make(map[string]int, len(infos))
	for _, info := range infos {
		logical := info
		if info.DistributedMigration {
			placement, ok := byPhysical[info.ID]
			if !ok {
				continue
			}
			logical.ID = placement.BranchID
			logical.NodeID = placement.OwnerNode
			logical.Quarantined = placement.Quarantined
			logical.Unplaced = placement.State == cluster.PlacementUnplaced
			if placement.ParentBranch != "" || logical.ParentID == "" {
				logical.ParentID = placement.ParentBranch
			}
			if !placement.BranchCreatedAt.IsZero() {
				logical.CreatedAt = placement.BranchCreatedAt
			}
			logical.DistributedForkTransactionID = ""
			logical.DistributedGraphReferenceOperation = ""
			logical.DistributedMigration = false
		} else if authoritativeFamily {
			placement, ok := byLogical[info.ID]
			if !ok {
				// A rename/removal committed on another node may leave this
				// physical materialization record behind until asynchronous
				// cleanup. It is no longer a logical branch.
				continue
			}
			logical.NodeID = placement.OwnerNode
			logical.Quarantined = placement.Quarantined
			logical.Unplaced = placement.State == cluster.PlacementUnplaced
			logical.State = placementDisplayState(placement, offlineOwners)
			logical.ParentID = placement.ParentBranch
			if !placement.BranchCreatedAt.IsZero() {
				logical.CreatedAt = placement.BranchCreatedAt
			}
		}
		if index, exists := resultByLogical[logical.ID]; exists {
			// The currently placed physical incarnation is authoritative over a
			// stale persistent record carrying the same logical name.
			if info.DistributedMigration {
				result[index] = logical
			}
			continue
		}
		resultByLogical[logical.ID] = len(result)
		result = append(result, logical)
	}
	if authoritativeFamily {
		for _, placement := range byLogical {
			if _, ok := resultByLogical[placement.BranchID]; ok {
				continue
			}
			result = append(result, ttcbundle.BranchInfo{
				ID: placement.BranchID, ParentID: placement.ParentBranch,
				NodeID: placement.OwnerNode, State: string(placement.State),
				Quarantined: placement.Quarantined, Unplaced: placement.State == cluster.PlacementUnplaced,
				CreatedAt: placement.BranchCreatedAt,
			})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	}
	return result
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
			branch.PublicationID = recorded.PublicationID
			branch.StorageError = recorded.StorageError
			branch.StorageManifest = recorded.StorageManifest
			branch.StorageRetryAt = recorded.StorageRetryAt
			branch.SnapshotID = recorded.SnapshotID
			branch.MemoryRoots = append([]string(nil), recorded.MemoryRoots...)
			branch.MemoryRootObjects = append([]string(nil), recorded.MemoryRootObjects...)
			branch.ControlRoot = recorded.ControlRoot
			branch.ControlRootObject = recorded.ControlRootObject
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
		"restarting": 9, "running": 10, "removing": 11,
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
		ID:                          info.ID,
		ParentID:                    info.ParentID,
		Quarantined:                 info.Quarantined,
		State:                       normalizeLifecycleState(info.State),
		CreatedAt:                   info.CreatedAt,
		InitPID:                     info.InitPID,
		InitState:                   info.InitState,
		MemorySharing:               memorySharingMode(info.SharesMemory),
		ForkTotalNS:                 info.ForkMetrics.Total.Nanoseconds(),
		SourceDowntimeNS:            info.ForkMetrics.SourceDowntime.Nanoseconds(),
		KernelForkNS:                info.ForkMetrics.Kernel.Total.Nanoseconds(),
		ForkValidationNS:            info.ForkMetrics.Kernel.Validation.Nanoseconds(),
		ForkSharedMemoryNS:          info.ForkMetrics.Kernel.SharedMemoryPreparation.Nanoseconds(),
		ForkMappingInvalidationNS:   info.ForkMetrics.Kernel.MappingInvalidation.Nanoseconds(),
		ForkVFSPreparationNS:        info.ForkMetrics.Kernel.VFSPreparation.Nanoseconds(),
		ForkDevicePreparationNS:     info.ForkMetrics.Kernel.DevicePreparation.Nanoseconds(),
		ForkFSMetadataFlushNS:       info.ForkMetrics.Kernel.FilesystemMetadataFlush.Nanoseconds(),
		ForkFSRuntimeInvalidationNS: info.ForkMetrics.Kernel.FilesystemRuntimeInvalidation.Nanoseconds(),
		ForkFSBranchSealNS:          info.ForkMetrics.Kernel.FilesystemBranchSeal.Nanoseconds(),
		ForkFSNamespacePruneNS:      info.ForkMetrics.Kernel.FilesystemNamespacePrune.Nanoseconds(),
		ForkMemoryPreparationNS:     info.ForkMetrics.Kernel.MemoryPreparation.Nanoseconds(),
		ForkMetadataStreamNS:        info.ForkMetrics.Kernel.MetadataStream.Nanoseconds(),
		ForkMetadataBytes:           info.ForkMetrics.Kernel.MetadataBytes,
		ForkMetadataRecordNS:        info.ForkMetrics.Kernel.MetadataRecord.Nanoseconds(),
		ForkMetadataObjects:         info.ForkMetrics.Kernel.MetadataObjects,
		ForkChildRestoreNS:          info.ForkMetrics.Kernel.ChildRestore.Nanoseconds(),
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
	logicalSource := source
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
	if err := b.ensureForkPlacementDestinationsAvailable(containerInfo, destinations); err != nil {
		return nil, err
	}
	// The new branches run before their placements exist.
	defer b.operations.beginMaking(source.container, destinations...)()
	sourceRecord, err := persistentBranchInfo(containerInfo, source.branch)
	if err != nil {
		return nil, err
	}
	if err := lifecycleStateRefusal(containerInfo, source, logicalSource, "fork", "running", "paused", "dehydrated", "dehydrating"); err != nil {
		return nil, err
	}
	if state := normalizeLifecycleState(sourceRecord.State); state == "dehydrated" || state == "dehydrating" {
		// A dehydrated branch is forked from its durable image, which a
		// resident dehydration does not have yet.
		if ttcbundle.ResidentDehydration(sourceRecord) {
			if err := b.persistDehydration(source, containerInfo, sourceRecord, nil); err != nil {
				return nil, fmt.Errorf("making the dehydration of %s durable to fork it: %w", source, err)
			}
			if containerInfo, err = b.containerStore().LoadContainer(containerInfo.ID); err != nil {
				return nil, err
			}
			if sourceRecord, err = persistentBranchInfo(containerInfo, source.branch); err != nil {
				return nil, err
			}
		}
		// Away from the node that holds its capture, the branch is forked
		// from its image in the shared store, which its publication may still
		// be writing, as a remote fork's target reads it: a fork read the
		// image of the publication before, "materializing dehydrated source:
		// cold manifest publication is ..., branch publication is ...".
		if sourceRecord.StorageState != "shared" && !b.coldCaptureLocal(containerInfo, sourceRecord) {
			if waited, err := b.awaitSharedDehydratedSource(containerInfo, source, logicalSource, sourceRecord); err != nil {
				return nil, err
			} else if waited {
				return b.forkMany(logicalSource, destinations, paused, quarantine)
			}
		}
		return b.forkDehydratedMany(source, logicalSource.branch, containerInfo, sourceRecord, destinations, paused, sourceRecord.Quarantined || quarantine)
	}
	// A source that no transaction produced has shared-only roots, and its
	// forks' intents have no transaction: see graphReferenceIntent.
	referenceTransaction := transactionIDForBranch(containerInfo, sourceRecord)
	referenceOperations := make([]string, len(destinations))
	referenceRequired := referenceTransaction != ""
	if len(sourceRecord.MemoryRootObjects) != 0 && b.distributedSource != nil {
		localRoots, err := b.graphMemoryRootsLocal(sourceRecord.MemoryRoots)
		if err != nil {
			return nil, err
		}
		referenceRequired = referenceRequired || !localRoots
	}
	if referenceRequired && len(sourceRecord.MemoryRootObjects) != 0 && b.distributedSource != nil {
		for index := range referenceOperations {
			operation, err := newGraphReferenceOperation()
			if err != nil {
				return nil, fmt.Errorf("allocating graph reference identity for %q: %w", destinations[index], err)
			}
			referenceOperations[index] = operation
		}
	}
	// The forks share whatever of the shared image the source still expands.
	if err := b.claimForkCatalogs(containerInfo, sourceRecord.FilesystemBranch); err != nil {
		return nil, err
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
	fork := func() ([]byte, func(bool) error, error) {
		// The children's memory roots retain what the source's do, which a
		// snapshot frozen for a handoff refuses once the runtime has forked:
		// the fork destroyed its children and failed. It waits the handoff
		// out first, as for its filesystems, before it allocates anything.
		if err := b.graphMemoryRootsFrozen(sourceRecord.MemoryRootObjects); err != nil {
			return nil, nil, err
		}
		filesystemOrigins, finishFilesystemOrigins, err := b.allocateFilesystemForkOrigins(containerInfo.ID, len(destinations))
		if err != nil {
			return nil, nil, err
		}
		var output []byte
		if len(filesystemOrigins) == 0 {
			output, err = b.command(args...)
		} else if embeddedRuntime(b.runsc) {
			output, err = embeddedFork(b.root, source.container, source.branch, destinations, paused, quarantine, filesystemOrigins)
		} else {
			originsReader, pipeErr := filesystemOriginAssignmentsPipe(filesystemOrigins)
			if pipeErr != nil {
				return nil, nil, errors.Join(pipeErr, finishFilesystemOrigins(false))
			}
			withOrigins := append([]string{"timetravel", "--filesystem-origins-fd=3"}, args[1:]...)
			output, err = b.commandWithFiles([]*os.File{originsReader}, withOrigins...)
			closeErr := originsReader.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return nil, nil, errors.Join(err, finishFilesystemOrigins(false))
		}
		return output, finishFilesystemOrigins, nil
	}
	output, finishFilesystemOrigins, err := b.forkThroughHandoffs(containerInfo, source.branch, fork)
	if err != nil {
		return nil, err
	}
	if err := finishFilesystemOrigins(true); err != nil {
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
		branches[i].ParentID = logicalSource.branch
	}
	if b.dataRoot != "" {
		records := make([]ttcbundle.BranchInfo, len(branches))
		for i, branch := range branches {
			records[i] = ttcbundle.BranchInfo{
				ID: branch.ID, ParentID: branch.ParentID, FilesystemBranch: infos[i].FilesystemBranch,
				Quarantined: branch.Quarantined,
				NodeID:      b.nodeID,
				State:       branch.State, StorageState: "local", StorageManifest: sourceRecord.StorageManifest, CreatedAt: branch.CreatedAt,
				ForkTotalNS: branch.ForkTotalNS, SourceDowntimeNS: branch.SourceDowntimeNS, KernelForkNS: branch.KernelForkNS,
				ForkValidationNS: branch.ForkValidationNS, ForkSharedMemoryNS: branch.ForkSharedMemoryNS,
				ForkMappingInvalidationNS: branch.ForkMappingInvalidationNS, ForkVFSPreparationNS: branch.ForkVFSPreparationNS,
				ForkMemoryPreparationNS: branch.ForkMemoryPreparationNS, ForkMetadataStreamNS: branch.ForkMetadataStreamNS,
				ForkMetadataBytes: branch.ForkMetadataBytes, ForkChildRestoreNS: branch.ForkChildRestoreNS,
				ForkMetadataRecordNS: branch.ForkMetadataRecordNS, ForkMetadataObjects: branch.ForkMetadataObjects,
				ForkDevicePreparationNS:     branch.ForkDevicePreparationNS,
				ForkFSMetadataFlushNS:       branch.ForkFSMetadataFlushNS,
				ForkFSRuntimeInvalidationNS: branch.ForkFSRuntimeInvalidationNS,
				ForkFSBranchSealNS:          branch.ForkFSBranchSealNS,
				ForkFSNamespacePruneNS:      branch.ForkFSNamespacePruneNS,
			}
			if referenceOperations[i] != "" {
				if referenceTransaction != "" {
					records[i].DistributedForkTransactionID = referenceTransaction
				}
				records[i].DistributedGraphReferenceOperation = referenceOperations[i]
			}
		}
		// The image the source reads lazily may be frozen for a handoff, as a
		// publication of a branch that shares it makes one: the children's
		// roots of it wait the handoff out, as the fork itself does above. A
		// refused clone changes nothing, and the handoff's end takes neither
		// this lock nor anything the fork holds; the fork failed, and took
		// its children with it, "rooting graph memory for batch fork: snapshot
		// 92 references are frozen for authority handoff".
		var graphRoots map[string][]string
		err := throughAuthorityHandoffs(func() error {
			var cloneErr error
			graphRoots, cloneErr = b.cloneGraphMemoryRootsMany(sourceRecord.MemoryRoots, sourceRecord.MemoryRootObjects, sourceRecord.SnapshotID, records)
			return cloneErr
		})
		if err != nil {
			var cleanupErr error
			store := b.containerStore()
			for i, cleanupDestination := range destinations {
				if _, destroyErr := b.command("timetravel", source.container, "destroy-reclaim", cleanupDestination); destroyErr != nil {
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
			records[i].MemoryRootObjects = append([]string(nil), sourceRecord.MemoryRootObjects...)
		}
		if err := b.containerStore().RecordBranches(source.container, records); err != nil {
			var cleanupErr error
			store := b.containerStore()
			for _, roots := range graphRoots {
				cleanupErr = errors.Join(cleanupErr, b.releaseGraphMemoryRoots(roots))
			}
			for i, cleanupDestination := range destinations {
				if _, destroyErr := b.command("timetravel", source.container, "destroy-reclaim", cleanupDestination); destroyErr != nil {
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
			if records[i].DistributedGraphReferenceOperation == "" {
				continue
			}
			if err := b.retainDistributedGraphReferences(referenceTransaction, records[i].DistributedGraphReferenceOperation, containerInfo.ID, records[i].ID, branchGraphRootObjects(records[i])); err != nil {
				var cleanupErr error
				for _, cleanupDestination := range destinations {
					cleanupErr = errors.Join(cleanupErr, b.destroy(selector{container: source.container, branch: cleanupDestination}, true))
				}
				if cleanupErr != nil {
					return nil, fmt.Errorf("retaining graph origins for branch %q: %w; cleaning batch branches: %v", records[i].ID, err, cleanupErr)
				}
				return nil, fmt.Errorf("retaining graph origins for branch %q: %w; batch branches were destroyed", records[i].ID, err)
			}
		}
		if err := b.registerLocalForkPlacements(containerInfo, records); err != nil {
			var cleanupErr error
			for _, cleanupDestination := range destinations {
				cleanupErr = errors.Join(cleanupErr, b.destroy(selector{container: source.container, branch: cleanupDestination}, true))
			}
			if cleanupErr != nil {
				return nil, fmt.Errorf("registering batch fork placements: %w; cleaning batch branches: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("registering batch fork placements: %w; batch branches were destroyed", err)
		}
	}
	// The branches are the source's runtime's, under the names given: the
	// source's record, resolved, is theirs.
	for _, destination := range destinations {
		target := selector{container: source.container, branch: destination}
		if _, err := b.ensureConsoleDrainIn(containerInfo, target, true); err != nil {
			for _, cleanupDestination := range destinations {
				_ = b.destroy(selector{container: source.container, branch: cleanupDestination}, true)
			}
			return nil, fmt.Errorf("starting console drain after batch fork: %w; batch branches were destroyed", err)
		}
		sessions, err := b.execSessionsOf(target)
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
			if _, err := b.ensureExecConsoleDrainIn(containerInfo, target, session, true); err != nil {
				for _, cleanupDestination := range destinations {
					_ = b.destroy(selector{container: source.container, branch: cleanupDestination}, true)
				}
				return nil, fmt.Errorf("starting cloned exec console drain %q: %w; batch branches were destroyed", session.ID, err)
			}
		}
	}

	return branches, nil
}

// durableRestorePeerWait bounds how long a restore waits for a live Sentry
// that could not list its branches to answer.
var durableRestorePeerWait = 5 * time.Second

// durableRestorePeer reports whether a branch made from a durable image, a
// fork of a dehydrated branch or a rehydration, goes into the family's Sentry
// on this node, which then runs: the branch is imported beside its branches,
// rather than made a runtime of its own. A fully cold family may leave an OCI
// shell whose Sentry has exited; it holds no state, and is deleted, so that it
// does not keep the branch's runtime from being made. A Sentry that runs but
// could not list its branches, as a loaded one may not at once, runs them
// still: a fork took it for such a shell and deleted it, with every branch it
// ran. own are the branches the caller is making in the runtime itself.
func (b *runtimeBackend) durableRestorePeer(containerID string, own ...string) (bool, error) {
	deadline := time.Now().Add(durableRestorePeerWait)
	for {
		runtimes, err := b.listRuntimeFamiliesOf(containerID)
		if err != nil {
			return false, err
		}
		var runtime *Family
		for i := range runtimes {
			if runtimes[i].ID == containerID {
				runtime = &runtimes[i]
				break
			}
		}
		switch {
		case runtime == nil:
			return false, nil
		case runtime.Error == "" && runtimeFamilyLiveBranch(*runtime) != "":
			return true, nil
		case runtime.Error == "":
			// A Sentry that runs no branch has none to restore beside: a
			// restore beside one failed, "no active peer is available for
			// dehydration restore". It goes as a release would take it, its
			// captures made durable first, unless a remote fork is making a
			// branch in it, which the restore waits for.
			released, err := b.releaseIdleRuntime(containerID, own...)
			if err != nil {
				return false, err
			}
			if released {
				return false, nil
			}
			b.operations.waitMadeIn(containerID, own...)
			continue
		case runtimeFamilyUnavailable(*runtime):
			_, _ = b.command("delete", "--force", containerID)
			return false, nil
		case time.Now().After(deadline):
			return false, fmt.Errorf("runtime %q runs but cannot list its branches: %s", containerID, runtime.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// forkDehydratedMany creates local children directly from durable branch
// state. The source remains process-free and its dehydration image is never
// consumed. Filesystem metadata and graph roots are immutable, so preparing
// any number of siblings does not copy file or memory data.
func (b *runtimeBackend) forkDehydratedMany(source selector, logicalParent string, info ttcbundle.ContainerInfo, sourceBranch ttcbundle.BranchInfo, destinations []string, paused, quarantined bool) (_ []Branch, retErr error) {
	if b.dataRoot == "" {
		return nil, errors.New("forking a dehydrated branch requires a persistent TTC data root")
	}
	// Cold forks install new graph roots and filesystem edges. Keep the whole
	// path on the foreground side of the snapshot handoff gate so each edge is
	// accounted either by the provider ledger or by the shared ledger, never in
	// the frozen interval between them. The gate is a no-op in standalone mode.
	finishForegroundCapture, err := b.beginForegroundCapture(info, longOperation{branch: sourceBranch.ID, state: "forking"})
	if err != nil {
		return nil, err
	}
	defer finishForegroundCapture()
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
	// The forks share whatever of the shared image the source expands.
	if err := b.claimForkCatalogs(info, capture.FilesystemBranch); err != nil {
		return nil, err
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
			result = errors.Join(result, b.releaseBranchGraphRoots(child.branch))
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
	referenceTransaction := transactionIDForBranch(info, sourceBranch)
	externalMemoryRoots := false
	if len(sourceBranch.MemoryRootObjects) != 0 && b.distributedSource != nil {
		localRoots, err := b.graphMemoryRootsLocal(sourceBranch.MemoryRoots)
		if err != nil {
			return nil, err
		}
		externalMemoryRoots = !localRoots
	}
	for _, destination := range destinations {
		filesystemBranch, err := ttcvolume.NewScopedBranchID(info.ID, destination)
		if err != nil {
			return nil, fmt.Errorf("allocating filesystem identity for %s@%s: %w", source.container, destination, err)
		}
		branch := ttcbundle.BranchInfo{ID: destination, ParentID: logicalParent, FilesystemBranch: filesystemBranch, Quarantined: quarantined, State: "dehydrated", StorageState: "local", SnapshotID: sourceBranch.SnapshotID, NodeID: b.nodeID, CreatedAt: createdAt}
		if (referenceTransaction != "" || externalMemoryRoots) && len(sourceBranch.MemoryRootObjects) != 0 && b.distributedSource != nil {
			operation, err := newGraphReferenceOperation()
			if err != nil {
				return nil, fmt.Errorf("allocating graph reference identity for %q: %w", destination, err)
			}
			if referenceTransaction != "" {
				branch.DistributedForkTransactionID = referenceTransaction
			}
			branch.DistributedGraphReferenceOperation = operation
		}
		childDir := memoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
		children = append(children, preparedChild{target: selector{container: source.container, branch: destination}, branch: branch, dir: childDir})
		graphRoots, err := b.cloneGraphMemoryRoots(sourceBranch.MemoryRoots, sourceBranch.MemoryRootObjects, sourceBranch.SnapshotID, filesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("rooting graph memory for %s@%s: %w", source.container, destination, err)
		}
		branch.MemoryRoots = graphRoots
		branch.MemoryRootObjects = append([]string(nil), sourceBranch.MemoryRootObjects...)
		controlRoot, err := b.cloneGraphControlRoot(sourceBranch.ControlRoot, sourceBranch.ControlRootObject, sourceBranch.SnapshotID, filesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("rooting graph control state for %s@%s: %w", source.container, destination, err)
		}
		branch.ControlRoot = controlRoot
		branch.ControlRootObject = sourceBranch.ControlRootObject
		children[len(children)-1].branch = branch
		transactions, finishFilesystemOrigins, err := b.forkDehydratedDistributedFilesystems(info.ID, capture.FilesystemBranch, filesystemBranch)
		if err != nil {
			return nil, fmt.Errorf("forking filesystems for %s@%s: %w", source.container, destination, err)
		}
		for index, transaction := range transactions {
			if err := transaction.commit(); err != nil {
				for abortIndex := len(transactions) - 1; abortIndex > index; abortIndex-- {
					_ = transactions[abortIndex].abort()
				}
				return nil, errors.Join(fmt.Errorf("committing filesystem fork for %s@%s: %w", source.container, destination, err), finishFilesystemOrigins(false))
			}
		}
		if err := finishFilesystemOrigins(true); err != nil {
			return nil, fmt.Errorf("committing filesystem origins for %s@%s: %w", source.container, destination, err)
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
		if capture.SnapshotID == 0 || len(capture.MemoryRoots) == 0 {
			_ = os.RemoveAll(staging)
			return nil, errors.New("new-format dehydrated source is not graph-native")
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
	for i := range records {
		if records[i].DistributedGraphReferenceOperation == "" {
			continue
		}
		if err := b.retainDistributedGraphReferences(referenceTransaction, records[i].DistributedGraphReferenceOperation, info.ID, records[i].ID, branchGraphRootObjects(records[i])); err != nil {
			return nil, fmt.Errorf("retaining graph origins for dehydrated branch %q: %w", records[i].ID, err)
		}
	}

	// The fork is making its children in the runtime (forkMany): the runtime
	// is not kept for them, which it does not run yet. Waiting for them, a
	// fork that found the runtime idle waited for itself, holding the
	// family's lifecycle lock, and every later command of the family hung.
	peer, err := b.durableRestorePeer(info.ID, destinations...)
	if err != nil {
		return nil, fmt.Errorf("checking the runtime of %s before forking: %w", source, err)
	}
	result := make([]Branch, 0, len(children))
	for _, child := range children {
		// Building the child from the durable image takes time in proportion
		// to its memory; other commands go on meanwhile.
		if err := b.withoutLifecycleLock(info, longOperation{branch: child.branch.ID, state: "rehydrating", shown: true}, func() error {
			return b.rehydrateCold(child.target, info, child.branch, peer, sourceBranch)
		}); err != nil {
			return nil, fmt.Errorf("starting dehydrated fork child %s: %w", child.target, err)
		}
		peer = true
		state := "paused"
		if capture.Resume && !paused {
			state = "running"
		}
		result = append(result, Branch{ID: child.branch.ID, ParentID: logicalParent, Quarantined: child.branch.Quarantined, State: state, CreatedAt: createdAt, MemorySharing: "cow"})
	}
	// The children run before their placements exist, as a live fork's do,
	// and are placed as theirs are. The reconciliation after the command
	// looks for the runtimes of its family's placements, and a remote fork's
	// branch, dehydrated, has none in the runtime its children run in: they
	// had no placement, and each command of them was refused, "branch ...
	// does not exist", until one placed their source in that runtime again.
	placed := make([]ttcbundle.BranchInfo, 0, len(children))
	for index, child := range children {
		branch := child.branch
		branch.State = result[index].State
		placed = append(placed, branch)
	}
	if err := b.registerLocalForkPlacements(info, placed); err != nil {
		return nil, fmt.Errorf("registering dehydrated fork placements: %w", err)
	}
	return result, nil
}

// lifecycleStateRefusal refuses verb of the branch that target, as the client
// named it, resolved to, unless the branch's record calls it one of states.
// The runtime has no branch that is not live: of one exited or dehydrated it
// answered that the branch "does not exist", or failed loading the family's
// runtime, gone with its last live branch, or a node without the family's
// repositories failed opening them.
func lifecycleStateRefusal(info ttcbundle.ContainerInfo, resolved, target selector, verb string, states ...string) error {
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return nil
	}
	if state := normalizeLifecycleState(branch.State); !slices.Contains(states, state) {
		return fmt.Errorf("cannot %s %s: branch is %s", verb, target, state)
	}
	return nil
}

func (b *runtimeBackend) pause(target selector) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	if err := lifecycleStateRefusal(info, resolved, target, "pause", "running", "paused"); err != nil {
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
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	if err := lifecycleStateRefusal(info, resolved, target, "resume", "running", "paused"); err != nil {
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
	logicalBranch := target.branch
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
	logicalFamily := info.DistributedFamilyID
	if logicalFamily == "" {
		logicalFamily = info.ID
	}
	var placement *cluster.PlacementRecord
	var placements cluster.PlacementCatalog
	var ownerIncarnation string
	if b.nodeID != "" {
		owner, renewErr := b.renewDistributedExecutionAuthority()
		if renewErr != nil {
			return fmt.Errorf("renewing quarantine execution authority: %w", renewErr)
		}
		ownerIncarnation = owner.Incarnation
		placements, err = b.placementCatalog()
		if err != nil {
			return fmt.Errorf("opening quarantine placement authority: %w", err)
		}
		placement, err = placements.Lookup(logicalFamily, logicalBranch)
		if err != nil {
			return fmt.Errorf("resolving quarantine placement: %w", err)
		}
		if placement.OwnerNode != b.nodeID || placement.OwnerIncarnation != ownerIncarnation || placement.PhysicalContainer != target.container || placement.PhysicalBranch != target.branch {
			return errors.New("quarantine placement no longer names the resolved physical branch")
		}
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
	if placement != nil {
		if _, err := placements.SetQuarantine(logicalFamily, logicalBranch, b.nodeID, ownerIncarnation, placement.Generation, enabled, time.Now().UTC()); err != nil {
			rollbackErr := b.containerStore().UpdateBranchQuarantine(target.container, target.branch, branch.Quarantined)
			if !enabled && live {
				rollbackErr = errors.Join(rollbackErr, b.stopPublishedPortsForBranch(info, target.branch))
			}
			rollbackErr = errors.Join(rollbackErr, transitionRuntime(branch.Quarantined))
			if enabled && live {
				rollbackErr = errors.Join(rollbackErr, b.ensurePublishedPortsForBranch(infoWithBranchQuarantine(info, target.branch, false), target.branch))
			}
			if rollbackErr != nil {
				return fmt.Errorf("persisting cluster quarantine policy: %w; restoring local state: %v", err, rollbackErr)
			}
			return fmt.Errorf("persisting cluster quarantine policy: %w", err)
		}
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
	logicalBranch := target.branch
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
	logicalFamily := info.DistributedFamilyID
	if logicalFamily == "" {
		logicalFamily = info.ID
	}
	var placement *cluster.PlacementRecord
	var placements cluster.PlacementCatalog
	var ownerIncarnation string
	if b.nodeID != "" {
		placements, err = b.placementCatalog()
		if err != nil {
			return fmt.Errorf("opening rename placement authority: %w", err)
		}
		placement, err = placements.Lookup(logicalFamily, logicalBranch)
		if err != nil {
			return fmt.Errorf("resolving rename placement: %w", err)
		}
		if placement.State != cluster.PlacementUnplaced {
			owner, renewErr := b.renewDistributedExecutionAuthority()
			if renewErr != nil {
				return fmt.Errorf("renewing rename execution authority: %w", renewErr)
			}
			ownerIncarnation = owner.Incarnation
			if placement.OwnerNode != b.nodeID || placement.OwnerIncarnation != ownerIncarnation ||
				placement.PhysicalContainer != target.container || placement.PhysicalBranch != target.branch {
				return errors.New("rename placement no longer names the resolved physical branch")
			}
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
	if placement != nil {
		physicalBranch := ""
		if placement.State != cluster.PlacementUnplaced {
			physicalBranch = name
		}
		if _, err := placements.Rename(logicalFamily, logicalBranch, name, b.nodeID, ownerIncarnation, physicalBranch, placement.Generation, time.Now().UTC()); err != nil {
			rollbackErr := b.containerStore().RenameBranch(target.container, name, target.branch)
			if live {
				_, runtimeErr := b.command("timetravel", target.container, "rename", name, target.branch)
				rollbackErr = errors.Join(rollbackErr, runtimeErr)
			}
			if rollbackErr != nil {
				return fmt.Errorf("renaming cluster authority: %w; restoring local branch: %v", err, rollbackErr)
			}
			return fmt.Errorf("renaming cluster authority: %w", err)
		}
	}
	return nil
}

func (b *runtimeBackend) prepareStop(target selector) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return err
	}
	// A migrated branch may still read unchanged files from its source node.
	// Stop destroys the only live resolver before asynchronous cold publication,
	// so establish filesystem and memory independence while the Sentry can
	// still service the drain. Dehydration already uses the same fence.
	if err := b.ensureDistributedTargetMemoryIndependent(info, branch); err != nil {
		return fmt.Errorf("materializing remote state before stopping %s: %w", resolved, err)
	}
	_, err = b.command("timetravel", resolved.container, "prepare-stop", resolved.branch)
	return err
}

// preparePausedStop commits the Sentry's bounded filesystem metadata batches
// into each TTC repository before the paused Kernel is discarded. A manager-
// side repository fork is insufficient here: the mounted VFS may still own
// creates, writes, and mmap invalidations that are visible to the guest but
// have not reached the repository branch pointer. Running stop obtains this
// fence through orderly task/VFS teardown; paused stop deliberately executes
// no guest instruction and therefore needs an explicit filesystem snapshot.
//
// The temporary child is removed immediately. Its purpose is to drive the
// existing atomic live-filesystem snapshot protocol, which both flushes the
// mounted branch and installs a fresh working generation for the source.
//
// A snapshot refused because an origin the branch shares is frozen for an
// authority handoff changes nothing, and is taken again once the handoff
// ends, as a fork's is (forkThroughHandoffs): a stop of a paused branch was
// refused, "snapshot ... references are frozen for authority handoff", as
// another branch's publication handed off an origin the two share.
func (b *runtimeBackend) preparePausedStop(target selector) error {
	deadline := time.Now().Add(forkHandoffWait)
	delay := 100 * time.Millisecond
	for {
		err := b.preparePausedStopOnce(target)
		if err == nil || !frozenForAuthorityHandoff(err) || time.Now().After(deadline) {
			return err
		}
		info, resolveErr := b.resolveContainer(target.container)
		if resolveErr != nil {
			return err
		}
		if err := b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "snapshotting"}, func() error {
			time.Sleep(delay)
			return nil
		}); err != nil {
			return err
		}
		delay = min(2*delay, 2*time.Second)
	}
}

// preparePausedStopOnce is one attempt of preparePausedStop.
func (b *runtimeBackend) preparePausedStopOnce(target selector) (retErr error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return err
	}
	if normalizeLifecycleState(branch.State) != "paused" {
		return fmt.Errorf("branch %s is %s, not paused", resolved, normalizeLifecycleState(branch.State))
	}
	if err := b.ensureDistributedTargetMemoryIndependent(info, branch); err != nil {
		return fmt.Errorf("materializing remote state before stopping %s: %w", resolved, err)
	}
	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}
	cont, err := container.Load(b.root, container.FullID{
		SandboxID: info.ID, ContainerID: info.ID,
	}, container.LoadOpts{Exact: true, SkipCheck: true})
	if err != nil {
		return fmt.Errorf("loading paused runtime: %w", err)
	}
	assignments, finishOrigins, err := b.allocateFilesystemForkOrigins(info.ID, 1)
	if err != nil {
		return err
	}
	originsByRepository := make(map[string][]uint64, len(assignments))
	for _, assignment := range assignments {
		originsByRepository[assignment.ResourceID.Path] = assignment.Origins
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.Join(err, finishOrigins(false))
	}
	temporaryBranch := "paused-stop-" + hex.EncodeToString(random[:])
	type capturedRepository struct {
		repository *ttcvolume.Repository
		captured   bool
	}
	opened := make([]capturedRepository, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	committedAny := false
	originsAdopted := false
	finish := func(adopt bool) error {
		err := finishOrigins(adopt)
		originsAdopted = adopt && err == nil
		return err
	}
	cleanup := func() error {
		var cleanupErr error
		for i := len(opened) - 1; i >= 0; i-- {
			entry := opened[i]
			if entry.captured {
				if err := entry.repository.RemoveBranch(temporaryBranch); err != nil && !errors.Is(err, os.ErrNotExist) {
					cleanupErr = errors.Join(cleanupErr, err)
				} else if err == nil {
					cleanupErr = errors.Join(cleanupErr, entry.repository.GarbageCollect())
				}
			}
			cleanupErr = errors.Join(cleanupErr, entry.repository.Close())
		}
		return cleanupErr
	}
	defer func() {
		if len(opened) != 0 {
			retErr = errors.Join(retErr, cleanup())
		}
		if originsAdopted {
			if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("reconciling paused filesystem snapshot origins: %w", err))
			}
		}
	}()
	for _, config := range configs {
		repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			if committedAny {
				retErr = errors.Join(err, finish(true))
			} else {
				retErr = errors.Join(err, finish(false))
			}
			return retErr
		}
		repositoryID := repository.Info().ID
		if _, duplicate := seen[repositoryID]; duplicate {
			retErr = errors.Join(retErr, repository.Close())
			continue
		}
		seen[repositoryID] = struct{}{}
		opened = append(opened, capturedRepository{repository: repository})
		args := &boot.FilesystemBranchSnapshotArgs{
			BranchID:   resolved.branch,
			ResourceID: checkpoint.ResourceID{ContainerName: "ttc-volume", Path: repositoryID},
			Child:      temporaryBranch,
		}
		if origins := originsByRepository[repositoryID]; len(origins) != 0 {
			args.SourceWorking, args.ChildWorking = origins[0], origins[1]
		}
		if err := cont.Sandbox.SnapshotFilesystemBranch(args); err != nil {
			if committedAny {
				retErr = errors.Join(err, finish(true))
			} else {
				retErr = errors.Join(err, finish(false))
			}
			return retErr
		}
		opened[len(opened)-1].captured = true
		committedAny = true
	}
	if err := finish(true); err != nil {
		return err
	}
	return retErr
}

func graphMemoryRootNames(filesystemBranch string, count int) []string {
	result := make([]string, count)
	for index := range result {
		result[index] = fmt.Sprintf("memory/branch/%s/resource-%04x", filesystemBranch, index)
	}
	return result
}

func graphControlRootName(filesystemBranch string) string {
	return "control/branch/" + filesystemBranch
}

func graphCaptureControlRootName(snapshotID uint64) string {
	return fmt.Sprintf("control/capture/%016x", snapshotID)
}

func branchGraphRootObjects(branch ttcbundle.BranchInfo) []string {
	objects := append([]string(nil), branch.MemoryRootObjects...)
	if branch.ControlRootObject != "" {
		objects = append(objects, branch.ControlRootObject)
	}
	return objects
}

func branchGraphRootNames(branch ttcbundle.BranchInfo) []string {
	roots := append([]string(nil), branch.MemoryRoots...)
	if branch.ControlRoot != "" {
		roots = append(roots, branch.ControlRoot)
	}
	return roots
}

func runtimeBackingAsBranch(backing ttcbundle.RuntimeMemoryBacking) ttcbundle.BranchInfo {
	return ttcbundle.BranchInfo{
		SnapshotID:                         backing.SnapshotID,
		MemoryRoots:                        append([]string(nil), backing.MemoryRoots...),
		MemoryRootObjects:                  append([]string(nil), backing.MemoryRootObjects...),
		ControlRoot:                        backing.ControlRoot,
		ControlRootObject:                  backing.ControlRootObject,
		DistributedGraphReferenceOperation: backing.DistributedGraphReferenceOperation,
	}
}

func appendGraphRoot(roots []string, root string) []string {
	if root == "" {
		return roots
	}
	return append(roots, root)
}

func validateGraphControlRoot(snapshotID uint64, root, object string) error {
	if root == "" && object == "" {
		return nil
	}
	if snapshotID == 0 || root == "" || object == "" {
		return errors.New("control root requires a snapshot, logical name, and immutable object")
	}
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		return fmt.Errorf("parsing control root object: %w", err)
	}
	if id.Snapshot == 0 || uint64(id.Snapshot) != snapshotID || id.Kind != ttcsnapshot.ObjectKindMetadata {
		return fmt.Errorf("control root object %s is not a filesystem root", id)
	}
	return nil
}

// cloneGraphMemoryRoots gives a branch O(1)-with-respect-to-memory references
// to the same immutable radix roots as its source. No page or radix node is
// walked or copied.
func validateGraphRootObjects(snapshotID uint64, roots, objects []string) error {
	if snapshotID == 0 || len(roots) == 0 || len(roots) != len(objects) {
		return fmt.Errorf("memory snapshot %d has %d root names and %d root objects", snapshotID, len(roots), len(objects))
	}
	for index, encoded := range objects {
		id, err := ttcsnapshot.ParseObjectID(encoded)
		if err != nil {
			return fmt.Errorf("parsing memory root object %d: %w", index, err)
		}
		if id.Snapshot == 0 || id.Kind != ttcsnapshot.ObjectKindMemoryNode {
			return fmt.Errorf("memory root object %s for snapshot %d is not a memory radix root", id, snapshotID)
		}
	}
	return nil
}

func (b *runtimeBackend) validateExternalGraphRoots(snapshotID uint64, roots, objects []string) error {
	if b.nodeID == "" || b.distributedSource == nil || b.distributedSource.authority == nil {
		return errors.New("memory roots are absent from the authoritative local graph")
	}
	if err := validateGraphRootObjects(snapshotID, roots, objects); err != nil {
		return err
	}
	origins := make(map[ttcsnapshot.SnapshotID]struct{}, len(objects))
	for _, encoded := range objects {
		id, _ := ttcsnapshot.ParseObjectID(encoded)
		origins[id.Snapshot] = struct{}{}
	}
	for origin := range origins {
		_, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(origin))
		if err != nil {
			return fmt.Errorf("resolving memory origin %d authority: %w", origin, err)
		}
		if !volatile {
			if b.snapshotShared == nil {
				return errors.New("shared snapshot repository is unavailable")
			}
			if err := b.snapshotShared.CheckManifest(origin); err != nil {
				return fmt.Errorf("opening shared memory origin %d: %w", origin, err)
			}
		}
	}
	return nil
}

func (b *runtimeBackend) cloneGraphMemoryRoots(sourceRoots, sourceObjects []string, snapshotID uint64, targetFilesystemBranch string) ([]string, error) {
	if len(sourceRoots) == 0 {
		if len(sourceObjects) != 0 || snapshotID != 0 {
			return nil, errors.New("memory snapshot metadata has no logical roots")
		}
		return nil, nil
	}
	if b.snapshotGraph == nil || targetFilesystemBranch == "" {
		return nil, errors.New("snapshot graph is unavailable for memory-root fork")
	}
	if err := validateGraphRootObjects(snapshotID, sourceRoots, sourceObjects); err != nil {
		return nil, err
	}
	destinations := graphMemoryRootNames(targetFilesystemBranch, len(sourceRoots))
	copies := make(map[string]string, len(sourceRoots))
	for index, source := range sourceRoots {
		copies[destinations[index]] = source
	}
	local, err := b.graphMemoryRootsLocal(sourceRoots)
	if err != nil {
		return nil, err
	}
	if local {
		if err := b.snapshotGraph.CloneRoots(copies); err != nil {
			return nil, err
		}
		return destinations, nil
	}
	// Cache materialization never imports shared or peer reference authority
	// into the local graph. The branch-level object IDs are sufficient to add
	// another logical reference; the origin ledger milestone accounts for that
	// reference at its unique provider.
	if err := b.validateExternalGraphRoots(snapshotID, sourceRoots, sourceObjects); err != nil {
		return nil, err
	}
	return destinations, nil
}

// graphMemoryRootsLocal determines whether one logical root set is wholly
// represented in the node-local graph. A root set may instead be wholly
// external (peer- or shared-pack authoritative), but a mixed set is invalid:
// no single reference operation could then preserve it atomically.
func (b *runtimeBackend) graphMemoryRootsLocal(roots []string) (bool, error) {
	if len(roots) == 0 {
		return true, nil
	}
	if b.snapshotGraph == nil {
		return false, errors.New("snapshot graph is unavailable")
	}
	existing := 0
	for _, root := range roots {
		if _, ok := b.snapshotGraph.Root(root); ok {
			existing++
		}
	}
	if existing != 0 && existing != len(roots) {
		return false, errors.New("memory roots are split between local and external authority")
	}
	return existing == len(roots), nil
}

// cloneGraphMemoryRootsMany commits all roots for a batch fork in one graph
// transaction, so no sibling can be left partially rooted after a crash.
// graphMemoryRootsFrozen returns an error that frozenForAuthorityHandoff
// knows if a snapshot that one of objects, a branch's memory root objects,
// belongs to is frozen on this node for an authority handoff.
func (b *runtimeBackend) graphMemoryRootsFrozen(objects []string) error {
	if b.snapshotGraph == nil {
		return nil
	}
	for _, encoded := range objects {
		id, err := ttcsnapshot.ParseObjectID(encoded)
		if err != nil {
			continue
		}
		if b.snapshotGraph.SnapshotLedgerFrozen(id.Snapshot) {
			return fmt.Errorf("snapshot %d references are frozen for authority handoff: %w", id.Snapshot, ttcsnapshot.ErrSnapshotLedgerFrozen)
		}
	}
	return nil
}

func (b *runtimeBackend) cloneGraphMemoryRootsMany(sourceRoots, sourceObjects []string, snapshotID uint64, branches []ttcbundle.BranchInfo) (map[string][]string, error) {
	result := make(map[string][]string, len(branches))
	if len(sourceRoots) == 0 {
		return result, nil
	}
	if b.snapshotGraph == nil {
		return nil, errors.New("snapshot graph is unavailable for batch memory-root fork")
	}
	if err := validateGraphRootObjects(snapshotID, sourceRoots, sourceObjects); err != nil {
		return nil, err
	}
	copies := make(map[string]string, len(sourceRoots)*len(branches))
	for _, branch := range branches {
		destinations := graphMemoryRootNames(branch.FilesystemBranch, len(sourceRoots))
		result[branch.FilesystemBranch] = destinations
		for index, source := range sourceRoots {
			copies[destinations[index]] = source
		}
	}
	local, err := b.graphMemoryRootsLocal(sourceRoots)
	if err != nil {
		return nil, err
	}
	if local {
		if err := b.snapshotGraph.CloneRoots(copies); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err := b.validateExternalGraphRoots(snapshotID, sourceRoots, sourceObjects); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *runtimeBackend) cloneGraphControlRoot(sourceRoot, sourceObject string, snapshotID uint64, targetFilesystemBranch string) (string, error) {
	if sourceRoot == "" && sourceObject == "" {
		return "", nil
	}
	if b.snapshotGraph == nil || targetFilesystemBranch == "" {
		return "", errors.New("snapshot graph is unavailable for control-root fork")
	}
	if err := validateGraphControlRoot(snapshotID, sourceRoot, sourceObject); err != nil {
		return "", err
	}
	destination := graphControlRootName(targetFilesystemBranch)
	if _, ok := b.snapshotGraph.Root(sourceRoot); ok {
		if err := b.snapshotGraph.CloneRoots(map[string]string{destination: sourceRoot}); err != nil {
			return "", err
		}
		return destination, nil
	}
	if b.nodeID == "" || b.distributedSource == nil || b.distributedSource.authority == nil {
		return "", errors.New("control root is absent from the authoritative local graph")
	}
	id, _ := ttcsnapshot.ParseObjectID(sourceObject)
	_, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(id.Snapshot))
	if err != nil {
		return "", fmt.Errorf("resolving control origin %d authority: %w", id.Snapshot, err)
	}
	if !volatile {
		if b.snapshotShared == nil {
			return "", errors.New("shared snapshot repository is unavailable")
		}
		if err := b.snapshotShared.CheckManifest(id.Snapshot); err != nil {
			return "", fmt.Errorf("opening shared control origin %d: %w", id.Snapshot, err)
		}
	}
	return destination, nil
}

// holdsGraphRoots reports whether this node's graph holds a root of branch.
func (b *runtimeBackend) holdsGraphRoots(branch ttcbundle.BranchInfo) bool {
	if b.snapshotGraph == nil {
		return false
	}
	for _, name := range branchGraphRootNames(branch) {
		if _, ok := b.snapshotGraph.Root(name); ok {
			return true
		}
	}
	return false
}

func (b *runtimeBackend) releaseBranchGraphRoots(branch ttcbundle.BranchInfo) error {
	return b.releaseGraphMemoryRoots(branchGraphRootNames(branch))
}

// parkBranchMemoryGraph detaches immutable page backing from a branch before
// the Sentry transfers that branch's unresolved loader to a sibling. The
// returned branch is reloaded after the atomic metadata mutation and therefore
// no longer owns the parked roots.
func (b *runtimeBackend) parkBranchMemoryGraph(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) (ttcbundle.ContainerInfo, ttcbundle.BranchInfo, error) {
	if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
		return info, branch, nil
	}
	if _, _, err := b.containerStore().ParkBranchMemoryGraph(info.ID, branch.ID); err != nil {
		return info, branch, fmt.Errorf("parking runtime memory backing for %s@%s: %w", info.Name, branch.ID, err)
	}
	if b.nodeID != "" && b.distributedSource != nil {
		b.distributedSource.noteParked(info.ID)
	}
	refreshed, err := b.containerStore().LoadContainer(info.ID)
	if err != nil {
		return info, branch, fmt.Errorf("reloading parked runtime memory backing for %s@%s: %w", info.Name, branch.ID, err)
	}
	updated, err := persistentBranchInfo(refreshed, branch.ID)
	if err != nil {
		return info, branch, err
	}
	return refreshed, updated, nil
}

// reclaimBranchBacking releases every immutable graph reference and writable
// filesystem generation owned by one physical branch. Lifecycle callers keep
// the branch record until this method succeeds, which makes retries able to
// recover the exact reference-operation identity after a crash.
//
// Physical branches created for a distributed fork or migration use the same
// backing primitives as ordinary branches. Keeping their cleanup here avoids
// letting a migration-specific path forget one of the local graph, remote
// ledger, or filesystem references when the physical runtime is retired.
func (b *runtimeBackend) reclaimBranchBacking(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	operation := graphReferenceOperationForBranch(info, branch)
	if operation == "" {
		if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(branch), branchGraphRootObjects(branch)); err != nil {
			return fmt.Errorf("releasing producer shared-memory roots: %w", err)
		}
	}
	// A remote-origin branch may have both ordinary logical roots and retained
	// roots installed by its durable reference intent. Drop the retained roots
	// first so the subsequent local collection sees the true zero-reference
	// state and can retire the provider origin in the same operation.
	if err := b.releaseDistributedGraphReferencesEventually(operation); err != nil {
		return fmt.Errorf("releasing distributed memory references: %w", err)
	}
	err := b.releaseBranchGraphRoots(branch)
	if frozenForAuthorityHandoff(err) {
		// A publication is handing a snapshot the roots retain off, which
		// ends with it: wait for it without the lock, as a dehydration
		// releasing its superseded roots does. A removal failed, "snapshot 42
		// references are frozen for authority handoff".
		err = b.withoutLifecycleLock(info, longOperation{branch: branch.ID, state: "removing", shown: true}, func() error {
			return b.releaseGraphMemoryRootsThroughHandoffs(branchGraphRootNames(branch))
		})
	}
	if err != nil {
		return fmt.Errorf("releasing local memory roots: %w", err)
	}
	if err := b.containerStore().RemoveBranchFilesystems(info.ID, branch); err != nil {
		return fmt.Errorf("removing branch filesystems: %w", err)
	}
	if b.removals != nil {
		b.removals.originsDue = true
		return nil
	}
	if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
		b.deferOriginReconcile(err)
	}
	return nil
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
	if err != nil {
		return err
	}
	if _, err := b.abandonEmptyVolatileMemoryOrigins(); err != nil {
		return err
	}
	if b.snapshotGraph.CheckpointDue(snapshotGraphJournalCompactBytes) {
		if err := b.snapshotGraph.Checkpoint(); err != nil {
			return fmt.Errorf("compacting reclaimed snapshot graph: %w", err)
		}
	}
	return nil
}

// abandonEmptyVolatileMemoryOrigins removes cluster provider records for
// node-local memory origins that own no objects. Both sealed and working
// origins are eligible: a working origin with no objects is allocation residue
// from a canceled or fully reclaimed mutation, not mutable guest state. The
// daemon mutation lock excludes a concurrent graph builder while this runs.
//
// Keeping this rule in one place is important. Branch cleanup, crash
// reconciliation, and node retirement previously used subtly different state
// filters, which allowed an empty working origin to outlive its final branch
// and then make clean retirement impossible.
func (b *runtimeBackend) abandonEmptyVolatileMemoryOrigins() (int, error) {
	if b == nil || b.snapshotGraph == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return 0, nil
	}
	node := b.nodeID
	if node == "" {
		node = b.distributedSource.capabilities.NodeID
	}
	if node == "" || b.distributedSource.incarnation == "" {
		return 0, errors.New("cluster memory-origin cleanup lacks node incarnation")
	}
	inspection := b.snapshotGraph.InspectWithoutObjects()
	abandoned := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, snapshot := range inspection.Snapshots {
		if snapshot.State != ttcsnapshot.SnapshotWorking && snapshot.State != ttcsnapshot.SnapshotSealedVolatile {
			continue
		}
		// An origin whose lock is held is being published, so it is no
		// residue now; a later pass finds it if it becomes so.
		unlockOrigin, locked := b.distributedSource.tryLockSnapshotOrigin(snapshot.ID)
		if !locked {
			continue
		}
		objects, err := b.snapshotGraph.SnapshotObjects(snapshot.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			unlockOrigin()
			return len(abandoned), fmt.Errorf("inspecting memory origin %d: %w", snapshot.ID, err)
		}
		if len(objects) != 0 || b.distributedSource.buildingOrigin(snapshot.ID) {
			unlockOrigin()
			continue
		}
		provider, found, err := b.distributedSource.authority.SnapshotProvider(uint64(snapshot.ID))
		if errors.Is(err, os.ErrNotExist) {
			// Empty-origin cleanup is global to the node, while migration
			// completion is deliberately concurrent across families. Another
			// cleanup may therefore have installed the permanent abandoned or
			// retired tombstone after this pass inspected the local graph. The
			// tombstone proves that no provider authority remains; finish the
			// idempotent local collection instead of failing branch removal.
			abandoned[snapshot.ID] = struct{}{}
			unlockOrigin()
			continue
		}
		if err != nil {
			unlockOrigin()
			return len(abandoned), fmt.Errorf("resolving empty memory origin %d: %w", snapshot.ID, err)
		}
		if !found {
			// Shared authority makes an empty provider-side graph disposable.
			abandoned[snapshot.ID] = struct{}{}
			unlockOrigin()
			continue
		}
		if provider.NodeID != node {
			unlockOrigin()
			continue
		}
		if b.snapshotShared != nil {
			busy, err := b.snapshotShared.DiscardHandoff(snapshot.ID)
			if err != nil {
				unlockOrigin()
				return len(abandoned), fmt.Errorf("discarding uncommitted memory origin %d: %w", snapshot.ID, err)
			}
			if busy {
				unlockOrigin()
				return len(abandoned), fmt.Errorf("discarding uncommitted memory origin %d: pack is busy", snapshot.ID)
			}
		}
		if err := b.distributedSource.authority.AbandonSnapshotOrigin(node, b.distributedSource.incarnation, uint64(snapshot.ID)); err != nil {
			unlockOrigin()
			return len(abandoned), fmt.Errorf("abandoning empty memory origin %d: %w", snapshot.ID, err)
		}
		abandoned[snapshot.ID] = struct{}{}
		unlockOrigin()
	}
	if len(abandoned) == 0 {
		return 0, nil
	}
	retain := make([]ttcsnapshot.SnapshotID, 0, len(inspection.Snapshots)-len(abandoned))
	for _, snapshot := range inspection.Snapshots {
		if _, remove := abandoned[snapshot.ID]; !remove {
			retain = append(retain, snapshot.ID)
		}
	}
	if _, err := b.snapshotGraph.CollectEmptySnapshots(retain); err != nil {
		return len(abandoned), err
	}
	return len(abandoned), nil
}

// reconcileVolatileSnapshotOrigins removes provider-directory entries whose
// authoritative local graph no longer exists. Snapshot IDs are cluster-wide,
// so a single union of the memory graph and filesystem repository graphs is
// sufficient; the authority deliberately does not need page- or file-level state.
//
// Lifecycle mutations and this reconciliation share the daemon data-root
// lock. Thus an origin allocated for an in-progress mutation cannot be
// mistaken for crash residue between allocation and graph adoption. Any
// uncommitted shared handoff is discarded before the authority forgets its provider, so
// absence from the provider directory never exposes canceled bytes as shared
// authority.
func (b *runtimeBackend) reconcileVolatileSnapshotOrigins() (int, error) {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return 0, nil
	}
	node := b.nodeID
	if node == "" {
		node = b.distributedSource.capabilities.NodeID
	}
	if node == "" || b.distributedSource.incarnation == "" {
		return 0, errors.New("cluster snapshot reconciliation lacks node incarnation")
	}
	// Snapshot-provider mutations are fenced to the current authority holder term.
	// A node may remain healthy across a takeover while its periodic
	// heartbeat has not yet renewed that term. Reconcile synchronously so an
	// otherwise successful branch removal cannot fail in this transient window.
	if _, err := b.renewDistributedExecutionAuthority(); err != nil {
		return 0, fmt.Errorf("renewing snapshot reconciliation authority: %w", err)
	}
	abandoned, err := b.abandonEmptyVolatileMemoryOrigins()
	if err != nil {
		return 0, fmt.Errorf("reclaiming empty memory origins: %w", err)
	}
	live := make(map[ttcsnapshot.SnapshotID]struct{})
	if b.snapshotGraph != nil {
		for _, snapshot := range b.snapshotGraph.InspectWithoutObjects().Snapshots {
			live[snapshot.ID] = struct{}{}
		}
	}
	filesystem, err := b.containerStore().FilesystemSnapshotOrigins()
	if err != nil {
		return 0, fmt.Errorf("inventorying filesystem snapshot origins: %w", err)
	}
	for origin := range filesystem {
		live[origin] = struct{}{}
	}
	state, err := b.distributedSource.authority.AuthorityStateOf(cluster.AuthorityQuery{VolatileOrigins: cluster.All[uint64]()})
	if err != nil {
		return 0, err
	}
	var stale []uint64
	for id, provider := range state.VolatileOrigins {
		if provider.NodeID != node {
			continue
		}
		if _, ok := live[ttcsnapshot.SnapshotID(id)]; !ok {
			stale = append(stale, id)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] < stale[j] })
	if len(stale) == 0 {
		return abandoned, nil
	}
	// An origin a capture allocated before the inventory, and has yet to
	// adopt, is building by the time this returns.
	b.distributedSource.awaitOriginAllocations()
	// The inventory is only a candidate list: a concurrent publisher or
	// branch mutation may have made an origin live before its lock was
	// taken. Take the stale origins' locks, passing over those held, and
	// look at the repositories again once, with every lock held, rather than
	// once for each origin.
	locked, unlockOrigins := b.distributedSource.tryLockSnapshotOrigins(stale)
	defer unlockOrigins()
	orphans, err := staleOriginOrphans(locked,
		func(origin uint64) bool {
			return b.distributedSource.buildingOrigin(ttcsnapshot.SnapshotID(origin))
		},
		b.containerStore().FilesystemSnapshotOrigins,
		func(origin uint64, filesystem map[ttcsnapshot.SnapshotID]struct{}) (bool, error) {
			return b.lockedOriginLive(ttcsnapshot.SnapshotID(origin), filesystem)
		})
	if err != nil {
		return 0, err
	}
	// The orphans are independent, and each costs an authority proposal:
	// several go at once, and the authority commits theirs together.
	work := make(chan uint64)
	errs := make([]error, min(len(orphans), staleOriginWorkers))
	var workers sync.WaitGroup
	for worker := range errs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for encoded := range work {
				if err := b.abandonLockedOrigin(node, encoded); err != nil && errs[worker] == nil {
					errs[worker] = err
				}
			}
		}()
	}
	for _, encoded := range orphans {
		work <- encoded
	}
	close(work)
	workers.Wait()
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}
	return abandoned + len(stale), nil
}

// staleOriginOrphans returns those of locked, stale origins whose locks the
// caller holds, that nothing holds: neither a scope that builds one, nor a
// repository, as inventory lists them all once, nor what live finds. Which
// are building is read before the repositories are: one still building then
// is held, and one whose building ended before was installed in its
// repository, which inventory then lists. Read after, an origin a
// rehydration installed after the listing, and finished building before the
// read, was in neither, and was abandoned: the branch's next publication
// named an origin the authority no longer knew, and never completed.
func staleOriginOrphans(locked []uint64, building func(uint64) bool, inventory func() (map[ttcsnapshot.SnapshotID]struct{}, error), live func(uint64, map[ttcsnapshot.SnapshotID]struct{}) (bool, error)) ([]uint64, error) {
	if len(locked) == 0 {
		return nil, nil
	}
	built := make(map[uint64]bool, len(locked))
	for _, origin := range locked {
		built[origin] = building(origin)
	}
	filesystem, err := inventory()
	if err != nil {
		return nil, fmt.Errorf("rechecking orphan snapshot origins: %w", err)
	}
	var orphans []uint64
	for _, origin := range locked {
		if built[origin] {
			continue
		}
		held, err := live(origin, filesystem)
		if err != nil {
			return nil, fmt.Errorf("rechecking orphan snapshot origin %d: %w", origin, err)
		}
		if !held {
			orphans = append(orphans, origin)
		}
	}
	return orphans, nil
}

// staleOriginWorkers is how many stale origins a reconciliation abandons at
// once.
const staleOriginWorkers = 8

// abandonStaleOrigin abandons the snapshot origin that reconciliation found
// this node the provider of, and neither its memory graph nor a repository
// holds, unless that is no longer so.
func (b *runtimeBackend) abandonStaleOrigin(node string, encoded uint64) error {
	return b.settleStaleOrigin(node, encoded, false)
}

// settleStaleOrigin abandons encoded, a volatile origin of node that its
// stores do not hold, unless it turns out live or another's. Without wait,
// one whose lock is held, being published or handed off, or sharing its lock
// stripe with one that is, is left to a later reconciliation; with wait, it
// waits for the lock.
func (b *runtimeBackend) settleStaleOrigin(node string, encoded uint64, wait bool) error {
	origin := ttcsnapshot.SnapshotID(encoded)
	var unlockOrigin func()
	if wait {
		unlockOrigin = b.distributedSource.lockSnapshotOrigin(origin)
	} else {
		var locked bool
		if unlockOrigin, locked = b.distributedSource.tryLockSnapshotOrigin(origin); !locked {
			return nil
		}
	}
	defer unlockOrigin()
	// The inventory is only a candidate list. A concurrent publisher or
	// branch mutation may have made the origin live before this origin fence
	// was acquired, so repeat both data-plane and authority observations.
	live, err := b.lockedOriginLive(origin, nil)
	if err != nil {
		return fmt.Errorf("rechecking orphan snapshot origin %d: %w", origin, err)
	}
	if live {
		return nil
	}
	return b.abandonLockedOrigin(node, encoded)
}

// lockedOriginLive reports whether this node's stores hold origin, whose
// lock the caller holds: a capture building it, the memory graph, or a
// repository, which filesystem lists if it is not nil, as it was inventoried
// with the lock held.
func (b *runtimeBackend) lockedOriginLive(origin ttcsnapshot.SnapshotID, filesystem map[ttcsnapshot.SnapshotID]struct{}) (bool, error) {
	if b.distributedSource.buildingOrigin(origin) {
		return true, nil
	}
	if b.snapshotGraph != nil {
		if _, ok := b.snapshotGraph.Snapshot(origin); ok {
			return true, nil
		}
	}
	if filesystem != nil {
		_, ok := filesystem[origin]
		return ok, nil
	}
	return b.containerStore().WithFilesystemSnapshotOrigin(origin, func(*ttcvolume.Repository) error { return nil })
}

// abandonLockedOrigin abandons encoded, a volatile origin of node whose lock
// the caller holds and that its stores do not hold, unless the authority no
// longer names node its provider.
func (b *runtimeBackend) abandonLockedOrigin(node string, encoded uint64) error {
	origin := ttcsnapshot.SnapshotID(encoded)
	provider, volatile, providerErr := b.distributedSource.authority.SnapshotProvider(encoded)
	if errors.Is(providerErr, os.ErrNotExist) || (!volatile && providerErr == nil) {
		return nil
	}
	if providerErr != nil {
		return fmt.Errorf("rechecking orphan snapshot origin %d authority: %w", origin, providerErr)
	}
	if provider.NodeID != node {
		return nil
	}
	if b.snapshotShared != nil {
		busy, discardErr := b.snapshotShared.DiscardHandoff(origin)
		if discardErr != nil {
			return fmt.Errorf("discarding orphan snapshot origin %d handoff: %w", origin, discardErr)
		}
		if busy {
			return fmt.Errorf("discarding orphan snapshot origin %d handoff: pack is busy", origin)
		}
	}
	if err := b.distributedSource.authority.AbandonSnapshotOrigin(node, b.distributedSource.incarnation, encoded); err != nil {
		// Another serialized cleanup may have completed between the state
		// read and this proposal. Treat an already-retired origin as success.
		if _, found, lookupErr := b.distributedSource.authority.SnapshotProvider(encoded); lookupErr != nil || found {
			return fmt.Errorf("abandoning orphan snapshot origin %d: %w", origin, errors.Join(err, lookupErr))
		}
	}
	return nil
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
			for _, name := range branchGraphRootNames(branch) {
				liveRoots[name] = struct{}{}
				if _, ok := b.snapshotGraph.Root(name); !ok {
					return fmt.Errorf("branch %s@%s references missing snapshot root %q", container.Name, branch.ID, name)
				}
			}
		}
		for _, backing := range container.RuntimeMemoryBackings {
			if backing.SnapshotID != 0 {
				retainedSnapshots[ttcsnapshot.SnapshotID(backing.SnapshotID)] = struct{}{}
			}
			for _, name := range branchGraphRootNames(runtimeBackingAsBranch(backing)) {
				liveRoots[name] = struct{}{}
				if _, ok := b.snapshotGraph.Root(name); !ok {
					return fmt.Errorf("container %s runtime backing %s references missing snapshot root %q", container.Name, backing.ID, name)
				}
			}
		}
	}
	inspection := b.snapshotGraph.InspectWithoutObjects()
	var orphanRoots []string
	for name := range inspection.Roots {
		if strings.HasPrefix(name, "memory/") || strings.HasPrefix(name, "control/") {
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
		if strings.HasPrefix(name, "memory/") || strings.HasPrefix(name, "control/") || strings.HasPrefix(name, "import/") {
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
	if changed || b.snapshotGraph.CheckpointDue(snapshotGraphJournalCompactBytes) {
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
	// Retiring PublicationID below is the physical CAS that prevents a stale
	// worker from replaying this branch's preceding lifecycle. First wait out an
	// exact in-flight queue worker so its committed roots and data-plane
	// cleanup reach a terminal boundary. Otherwise daemon drain would retain an
	// intent whose next retry can only report "publication identity changed".
	if branch.PublicationID != "" && b.publicationQueue != nil {
		if err := b.publicationQueue.CancelPending(info.ID, target.branch, branch.PublicationID); err != nil {
			return fmt.Errorf("fencing branch publication before discarding %s: %w", target, err)
		}
	}
	// Natural init exit can reach discard without prepareStop. The Sentry still
	// exists until the runtime deletion below, so use that last opportunity to
	// resolve sparse migration backing before the branch becomes process-free.
	if err := b.ensureDistributedTargetMemoryIndependent(info, branch); err != nil {
		return fmt.Errorf("materializing remote state before discarding %s: %w", target, err)
	}
	store := b.containerStore()
	if paths, pathErr := store.BranchConsolePathsIn(info, target.branch); pathErr == nil {
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
	if sessions, sessionsErr := b.execSessionsOf(target); sessionsErr == nil {
		for _, session := range sessions {
			if paths, pathErr := store.ExecConsolePathsIn(info, target.branch, session.ID); pathErr == nil {
				if err := b.stopConsoleDrain(paths); err != nil {
					return fmt.Errorf("stopping exec console follower %q for %s: %w", session.ID, target, err)
				}
			}
		}
	}
	if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
		return err
	}

runtime:
	for {
		runtimeFamilies, listErr := b.listRuntimeFamiliesOf(info.ID)
		if listErr != nil {
			return fmt.Errorf("checking runtime state while discarding %s: %w", target, listErr)
		}
		for _, family := range runtimeFamilies {
			if family.ID != info.ID {
				continue
			}
			if runtimeFamilyUnavailable(family) {
				// The Sentry exited with the branch, its last, before it could
				// be asked: the shell it left, a state whose process is gone,
				// refused every later rehydration of the family's branches on the
				// node, "connecting to control server ...: connection refused".
				if _, err := b.command("delete", "--force", info.ID); err != nil {
					return fmt.Errorf("releasing the exited runtime of %s: %w", target, err)
				}
				break runtime
			}
			present, onlyBranch := runtimeFamilyBranchDisposition(family, target.branch)
			if !present {
				break runtime
			}
			if onlyBranch {
				// A remote fork imports its branch into the runtime without the
				// lifecycle lock, and the runtime lists the branch only once it
				// is made: deleting the runtime with its last listed branch
				// would take the import with it. As a release does, wait for an
				// import under way, and keep a runtime that lists a branch made
				// meanwhile, of which this branch alone then goes; see
				// familyOperations.releasing.
				done, ok := b.operations.beginRelease(info.ID)
				if !ok {
					b.operations.waitMadeIn(info.ID)
					continue runtime
				}
				if !b.runtimeListsOnly(info.ID, family.Branches) {
					done()
					continue runtime
				}
				_, deleteErr := b.command("delete", "--force", info.ID)
				done()
				if deleteErr != nil {
					return fmt.Errorf("releasing final runtime branch %s: %w", target, deleteErr)
				}
				break runtime
			}
			var updated ttcbundle.BranchInfo
			info, updated, err = b.parkBranchMemoryGraph(info, branch)
			if err != nil {
				return err
			}
			branch = updated
			if _, err := b.command("timetravel", info.ID, "destroy", target.branch); err != nil {
				// The runtime may have gone since it was listed, as a release
				// of it does once another branch's dehydration or migration
				// leaves it running nothing: the branch's state there went
				// with it, and the discard goes on. Refused, a stop of a
				// dehydrated branch left the record's memory parked, which
				// the authority still named as the branch's image: every
				// command of the branch failed for good, "memory snapshot is
				// 0, committed authority has ...".
				if families, listErr := b.listRuntimeFamiliesOf(info.ID); listErr == nil && !runtimeListsBranch(families, info.ID, target.branch) {
					break runtime
				}
				return fmt.Errorf("discarding live runtime state for %s: %w", target, err)
			}
			break runtime
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
	if graphReferenceOperationForBranch(info, branch) == "" {
		if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(branch), branchGraphRootObjects(branch)); err != nil {
			return fmt.Errorf("releasing shared graph roots for %s: %w", target, err)
		}
	}
	if err := b.releaseBranchGraphRoots(branch); err != nil {
		return fmt.Errorf("reclaiming graph memory for %s: %w", target, err)
	}
	if err := b.releaseDistributedGraphReferencesEventually(graphReferenceOperationForBranch(info, branch)); err != nil {
		return fmt.Errorf("releasing distributed graph origins for %s: %w", target, err)
	}
	if len(branch.MemoryRoots) != 0 {
		if err := store.UpdateBranchSnapshotGraph(info.ID, target.branch, 0, nil, nil, "", "", ""); err != nil {
			return err
		}
	}
	latest, err := store.LoadContainer(info.ID)
	if err != nil {
		return err
	}
	return b.deleteRuntimeIfProcessFree(latest)
}

// runtimeListsBranch reports whether the runtime with this ID, in families,
// runs, or might, branch: one whose Sentry is gone runs none, and one that
// could not list its branches may run any.
func runtimeListsBranch(families []Family, id, branch string) bool {
	for _, family := range families {
		if family.ID != id || runtimeFamilyUnavailable(family) {
			continue
		}
		if family.Error != "" || slices.ContainsFunc(family.Branches, func(listed Branch) bool { return listed.ID == branch }) {
			return true
		}
	}
	return false
}

// runtimeFamilyBranchDisposition decides whether discarding branch may delete
// its whole host sandbox. A sandbox's aggregate OCI status follows its loader
// branch and may be "exited" while another TTC branch is still running; that
// aggregate status must never authorize deletion of the peers.
func runtimeFamilyBranchDisposition(family Family, branchID string) (present, onlyBranch bool) {
	for _, branch := range family.Branches {
		if branch.ID == branchID {
			return true, len(family.Branches) == 1
		}
	}
	return false, false
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
	return b.releaseRuntime(info.ID, func() error {
		families, err := b.listRuntimeFamiliesOf(info.ID)
		if err != nil {
			return fmt.Errorf("checking process-free runtime %q: %w", info.Name, err)
		}
		for _, family := range families {
			if family.ID != info.ID {
				continue
			}
			// A live branch the record does not name, as a remote fork's
			// import, keeps the runtime.
			if family.Error == "" && slices.ContainsFunc(family.Branches, func(branch Branch) bool {
				return runtimeBranchLive(branch) &&
					!slices.ContainsFunc(info.Branches, func(recorded ttcbundle.BranchInfo) bool { return recorded.ID == branch.ID })
			}) {
				return nil
			}
			// The last live branch is gone, and with the runtime would go every
			// capture only its Sentry holds.
			if err := b.persistResidentDehydrations(info, nil); err != nil {
				return err
			}
			if _, err := b.command("delete", "--force", info.ID); err != nil {
				return fmt.Errorf("deleting process-free runtime %q: %w", info.Name, err)
			}
			break
		}
		return nil
	})
}

// allocateGraphSnapshotOrigin keeps the core graph independent of its ID
// allocator. Standalone roots allocate locally; clustered roots first commit
// the globally unique origin and exact volatile provider in the authority. The
// origin is building, and cleanup of empty origins leaves it alone, until
// the capture calls built, once it is over.
func (b *runtimeBackend) allocateGraphSnapshotOrigin() (_ ttcsnapshot.SnapshotID, _ ttcsnapshot.SnapshotState, built func(), _ error) {
	if b.snapshotGraph == nil {
		return 0, "", nil, errors.New("snapshot graph is unavailable")
	}
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		origin, err := b.snapshotGraph.AllocateWorking()
		return origin, ttcsnapshot.SnapshotSealedLocal, func() {}, err
	}
	node := b.nodeID
	if node == "" {
		node = b.distributedSource.capabilities.NodeID
	}
	if node == "" || b.distributedSource.incarnation == "" {
		return 0, "", nil, errors.New("cluster snapshot allocation lacks node incarnation")
	}
	b.distributedSource.allocating.RLock()
	allocation, err := b.distributedSource.authority.AllocateSnapshotOrigins(node, b.distributedSource.incarnation, 1)
	origin := ttcsnapshot.SnapshotID(allocation.First)
	if err == nil && allocation.Count == 1 && origin != 0 {
		built = b.distributedSource.buildOrigin(origin)
	}
	b.distributedSource.allocating.RUnlock()
	if err != nil {
		return 0, "", nil, fmt.Errorf("allocating snapshot origin: %w", err)
	}
	if built == nil {
		return 0, "", nil, fmt.Errorf("the authority returned invalid snapshot allocation %+v", allocation)
	}
	if err := b.snapshotGraph.AdoptWorking(origin); err != nil {
		built()
		return 0, "", nil, fmt.Errorf("adopting snapshot origin %d: %w", origin, err)
	}
	return origin, ttcsnapshot.SnapshotSealedVolatile, built, nil
}

// graphCaptureStream returns the two ends of a stream a Sentry writes a
// memory capture to: the daemon's, which is read and answered by
// importGraphCapture, and the one handed to the Sentry. It is two-way so that
// the daemon can answer the capture's manifest with the pages its graph
// lacks, and a fork still sharing most of its memory sends little.
func graphCaptureStream() (daemon, sentry *os.File, err error) {
	fds, err := cloexecStreamSocketpair()
	if err != nil {
		return nil, nil, fmt.Errorf("creating memory capture stream: %w", err)
	}
	// The daemon's end, alone, is nonblocking, so that Go polls it.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, nil, fmt.Errorf("creating memory capture stream: %w", err)
	}
	return os.NewFile(uintptr(fds[0]), "memory-capture"), os.NewFile(uintptr(fds[1]), "memory-capture-sentry"), nil
}

// importGraphCapture keeps the sharing of pages the graph already holds inside
// one reference-authority domain. Standalone origins may share other locally
// sealed objects. Clustered origins may share only sealed volatile objects
// owned by this daemon; SnapshotShared objects are caches whose references are
// owned by the shared ledger and must not acquire unreported local child
// edges.
//
// stream is read, and for a filtered capture also answered; see
// graphCaptureStream.
func (b *runtimeBackend) importGraphCapture(stream io.ReadWriter, origin ttcsnapshot.SnapshotID, prefix string, images ...captureImage) (ttcchunk.GraphImportResult, error) {
	reuse := ttcsnapshot.MemoryReuse{States: []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotWorking, ttcsnapshot.SnapshotSealedLocal}}
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		// Concurrent and crash-interrupted cluster captures may leave committed
		// objects in a working origin. Reusing them would let a completed capture
		// depend on an origin that is still mutable or was never sealed. Reuse only
		// origins whose immutability is already established, and the shared
		// images the branch was restored from, once claimed. Standalone captures
		// are serialized by the foreground gate and retain their local behavior.
		var release func()
		reuse, release = b.captureReuse(prefix, images)
		defer release()
	}
	result, err := ttcchunk.ImportGraphCaptureReusing(stream, b.snapshotGraph, b.snapshotData, origin, prefix, reuse)
	if err != nil {
		return ttcchunk.GraphImportResult{}, err
	}
	// A capture can append hundreds of megabytes of bounded page/radix
	// transactions. It is already a foreground durability operation, so publish
	// one compact generation before returning once the journal has outgrown
	// the checkpoint, rather than leave every later daemon restart to replay
	// more capture history than it loads.
	if b.snapshotGraph.CheckpointDue(snapshotGraphJournalCompactBytes) {
		if err := b.snapshotGraph.Checkpoint(); err != nil {
			return ttcchunk.GraphImportResult{}, fmt.Errorf("compacting imported memory graph: %w", err)
		}
	}
	return result, nil
}

// reconcileInterruptedClusterMemoryGraph converts crash residue into one of
// two unambiguous states before the node resumes execution: unreachable
// working origins are reclaimed and abandoned, while objects already reached
// from a later sealed root are themselves sealed. A graph import commits
// immutable objects and bounded radix updates transactionally, so removing its
// construction roots/pins leaves only references created by a completed
// capture. No new cluster import is admitted while startup reconciliation runs.
func (b *runtimeBackend) reconcileInterruptedClusterMemoryGraph() error {
	if b == nil || b.snapshotGraph == nil || b.snapshotData == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil
	}
	if err := reconcileInterruptedMemoryGraph(b.snapshotGraph, b.snapshotData, ttcsnapshot.SnapshotSealedVolatile); err != nil {
		return err
	}
	if b.snapshotGraph.CheckpointDue(snapshotGraphJournalCompactBytes) {
		if err := b.snapshotGraph.Checkpoint(); err != nil {
			return fmt.Errorf("compacting recovered cluster memory graph: %w", err)
		}
	}
	// Empty provider origins are abandoned by the ordinary volatile-origin
	// reconciliation after this node transitions from joining to ready. The authority
	// intentionally rejects execution-authority mutations while joining.
	return nil
}

func reconcileInterruptedMemoryGraph(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, sealState ttcsnapshot.SnapshotState) error {
	inspection := graph.InspectWithoutObjects()
	// A capture holds the pages it shares with sealed origins under pins
	// until its trees refer to them. Those of a capture that did not finish
	// would keep the origins referenced for good.
	var held []string
	for name := range inspection.Pins {
		if ttcchunk.IsHeldPagePin(name) {
			held = append(held, name)
		}
	}
	sort.Strings(held)
	if len(held) != 0 {
		if err := ttcchunk.ReleaseHeldPagePins(graph, held); err != nil {
			return fmt.Errorf("releasing interrupted memory capture holds: %w", err)
		}
	}
	working := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, snapshot := range inspection.Snapshots {
		if snapshot.State == ttcsnapshot.SnapshotWorking {
			working[snapshot.ID] = struct{}{}
		}
	}
	if len(working) == 0 {
		return nil
	}
	var roots, pins []string
	for name, object := range inspection.Roots {
		if _, ok := working[object.Snapshot]; ok {
			roots = append(roots, name)
		}
	}
	for name, object := range inspection.Pins {
		if _, ok := working[object.Snapshot]; ok && !ttcchunk.IsHeldPagePin(name) {
			pins = append(pins, name)
		}
	}
	sort.Strings(roots)
	sort.Strings(pins)
	if len(pins) != 0 {
		if err := graph.DeletePins(pins); err != nil {
			return fmt.Errorf("releasing interrupted memory construction pins: %w", err)
		}
	}
	if len(roots) != 0 {
		if err := graph.ReleaseRoots(roots); err != nil {
			return fmt.Errorf("releasing interrupted memory construction roots: %w", err)
		}
	}
	for {
		collected, err := graph.Collect(4096)
		if err != nil {
			return fmt.Errorf("collecting interrupted memory graph: %w", err)
		}
		if collected == 0 {
			break
		}
	}
	if _, _, err := data.ReclaimUnreferenced(graph); err != nil {
		return fmt.Errorf("reclaiming interrupted memory data: %w", err)
	}
	ordered := make([]ttcsnapshot.SnapshotID, 0, len(working))
	for origin := range working {
		ordered = append(ordered, origin)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, origin := range ordered {
		objects, err := graph.SnapshotObjects(origin)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspecting interrupted memory origin %d: %w", origin, err)
		}
		if len(objects) == 0 {
			continue
		}
		if err := graph.Seal(origin, sealState); err != nil {
			return fmt.Errorf("sealing referenced interrupted memory origin %d: %w", origin, err)
		}
	}
	return nil
}

// newFilesystemOriginScope returns a bundle allocator and a completion
// function. A failed bundle preparation abandons every otherwise-unreachable
// origin the authority allocated, without reusing its ID; success transfers those origins to
// the created repositories.
func (b *runtimeBackend) newFilesystemOriginScope() (func(uint32) ([]ttcsnapshot.SnapshotID, error), func(bool) error) {
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil, func(bool) error { return nil }
	}
	node := b.nodeID
	if node == "" {
		node = b.distributedSource.capabilities.NodeID
	}
	incarnation := b.distributedSource.incarnation
	var allocated []ttcsnapshot.SnapshotID
	// The origins are building, as a capture's are, until the scope
	// finishes: none is in a repository until the bundle installs it.
	var built []func()
	allocate := func(count uint32) ([]ttcsnapshot.SnapshotID, error) {
		if node == "" || incarnation == "" {
			return nil, errors.New("cluster filesystem snapshot allocation lacks node incarnation")
		}
		b.distributedSource.allocating.RLock()
		allocation, err := b.distributedSource.authority.AllocateSnapshotOrigins(node, incarnation, count)
		valid := err == nil && allocation.Count == count && allocation.First != 0
		if valid {
			for index := uint64(0); index < uint64(count); index++ {
				built = append(built, b.distributedSource.buildOrigin(ttcsnapshot.SnapshotID(allocation.First+index)))
			}
		}
		b.distributedSource.allocating.RUnlock()
		if err != nil {
			return nil, fmt.Errorf("allocating filesystem origins: %w", err)
		}
		if !valid {
			return nil, fmt.Errorf("the authority returned invalid filesystem snapshot allocation %+v", allocation)
		}
		origins := make([]ttcsnapshot.SnapshotID, count)
		for index := range origins {
			origins[index] = ttcsnapshot.SnapshotID(allocation.First + uint64(index))
		}
		allocated = append(allocated, origins...)
		return origins, nil
	}
	finish := func(committed bool) error {
		defer func() {
			for _, done := range built {
				done()
			}
			built = nil
		}()
		if committed {
			allocated = nil
			return nil
		}
		var result error
		for _, origin := range allocated {
			if err := b.distributedSource.authority.AbandonSnapshotOrigin(node, incarnation, uint64(origin)); err != nil {
				result = errors.Join(result, fmt.Errorf("abandoning filesystem origin %d: %w", origin, err))
			}
		}
		allocated = nil
		return result
	}
	return allocate, finish
}

// ensureClusterFilesystemWorkingOrigins upgrades immutable cold repository
// branches to fresh authority-owned working origins immediately before a runtime is
// admitted to mutate them. Repository open and offline inspection deliberately
// do not perform this transition.
func (b *runtimeBackend) ensureClusterFilesystemWorkingOrigins(info ttcbundle.ContainerInfo, filesystemBranch string) error {
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil
	}
	if filesystemBranch == "" {
		return errors.New("cluster filesystem branch is empty")
	}
	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return fmt.Errorf("opening clustered repository %q: %w", config.Source, err)
		}
		physicalBranch := repositoryPhysicalBranch(config, filesystemBranch)
		if !repository.Info().Clustered {
			repository.Close()
			return fmt.Errorf("repository %q was created without cluster snapshot authority", config.Source)
		}
		needs, err := repository.BranchNeedsWorkingOrigin(physicalBranch)
		if err != nil {
			repository.Close()
			return fmt.Errorf("checking clustered repository %q branch %q: %w", config.Source, physicalBranch, err)
		}
		if !needs {
			if err := repository.Close(); err != nil {
				return err
			}
			continue
		}
		allocate, finish := b.newFilesystemOriginScope()
		origins, err := allocate(1)
		if err != nil {
			repository.Close()
			return errors.Join(err, finish(false))
		}
		adopted, installErr := repository.InstallBranchWorkingOrigin(physicalBranch, origins[0])
		closeErr := repository.Close()
		finishErr := finish(adopted)
		if installErr != nil || closeErr != nil || finishErr != nil {
			if installErr != nil {
				installErr = fmt.Errorf("installing clustered repository %q working origin: %w", config.Source, installErr)
			}
			return errors.Join(installErr, closeErr, finishErr)
		}
	}
	return nil
}

// allocateFilesystemForkOrigins reserves the exact working origins consumed
// by one Sentry filesystem fork. Each repository consumes a source/child pair
// for every destination; the assignment is keyed by the repository identity
// embedded in the guest mount, never by a host path.
func (b *runtimeBackend) allocateFilesystemForkOrigins(containerID string, childCount int) ([]boot.FilesystemOriginAssignment, func(bool) error, error) {
	finishNoop := func(bool) error { return nil }
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil, finishNoop, nil
	}
	if childCount <= 0 || childCount > 2048 {
		return nil, finishNoop, fmt.Errorf("invalid filesystem fork fanout %d", childCount)
	}
	spec, err := b.containerStore().LoadSpec(containerID)
	if err != nil {
		return nil, finishNoop, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, finishNoop, err
	}
	allocate, finish := b.newFilesystemOriginScope()
	assignments := make([]boot.FilesystemOriginAssignment, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		info, err := ttcvolume.ReadRepositoryInfo(config.Path)
		if err != nil {
			return nil, finish, errors.Join(err, finish(false))
		}
		repositoryID := info.ID
		if _, ok := seen[repositoryID]; ok {
			continue
		}
		seen[repositoryID] = struct{}{}
		origins, err := allocate(uint32(2 * childCount))
		if err != nil {
			return nil, finish, errors.Join(err, finish(false))
		}
		encoded := make([]uint64, len(origins))
		for i, origin := range origins {
			encoded[i] = uint64(origin)
		}
		assignments = append(assignments, boot.FilesystemOriginAssignment{
			ResourceID: checkpoint.ResourceID{ContainerName: "ttc-volume", Path: repositoryID},
			Origins:    encoded,
		})
	}
	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].ResourceID.Path < assignments[j].ResourceID.Path
	})
	return assignments, finish, nil
}

func filesystemOriginAssignmentsPipe(assignments []boot.FilesystemOriginAssignment) (*os.File, error) {
	encoded, err := json.Marshal(assignments)
	if err != nil {
		return nil, err
	}
	return newConfigMemFD("ttc-filesystem-origin-assignments", encoded)
}

// discardFailedGraphSnapshot rolls back an origin that never became
// authoritative. Failed captures must not be published as empty snapshots:
// doing so bypasses the shared reference ledger and leaves a pack that no
// collector owns. Callers release every construction root before entering
// this method.
//
// If authority already switched to shared, publication passed the reference
// ledger fence and normal shared GC owns the origin. The only local repair
// needed in that case is finishing a possibly interrupted MarkShared step.
func (b *runtimeBackend) discardFailedGraphSnapshot(origin ttcsnapshot.SnapshotID) error {
	if origin == 0 || b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil
	}
	node := b.nodeID
	if node == "" {
		node = b.distributedSource.capabilities.NodeID
	}
	provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(origin))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolving failed snapshot origin %d: %w", origin, err)
	}
	abandoned := errors.Is(err, os.ErrNotExist)
	if volatile && provider.NodeID != node {
		return fmt.Errorf("failed snapshot origin %d is owned by node %q, not %q", origin, provider.NodeID, node)
	}
	if !volatile && !abandoned && b.snapshotShared != nil {
		if manifestErr := b.snapshotShared.CheckManifest(origin); manifestErr == nil {
			if record, ok := b.snapshotGraph.Snapshot(origin); ok && record.State != ttcsnapshot.SnapshotShared {
				if err := b.snapshotGraph.MarkShared(origin); err != nil {
					return fmt.Errorf("finishing shared state for snapshot origin %d: %w", origin, err)
				}
			}
			return nil
		} else if !errors.Is(manifestErr, os.ErrNotExist) {
			return fmt.Errorf("checking failed snapshot origin %d publication: %w", origin, manifestErr)
		}
	}
	if b.snapshotGraph == nil || b.snapshotData == nil {
		return errors.New("local graph snapshot repository is unavailable")
	}
	// A failed origin was never made visible to another branch. Reclaim every
	// construction reference rooted in that origin rather than relying on each
	// streaming caller to have observed the importer's final result. In
	// particular, a command may report failure after its stream was committed.
	inspection := b.snapshotGraph.InspectWithoutObjects()
	var roots, pins []string
	for name, object := range inspection.Roots {
		if object.Snapshot == origin {
			roots = append(roots, name)
		}
	}
	for name, object := range inspection.Pins {
		if object.Snapshot == origin {
			pins = append(pins, name)
		}
	}
	if len(pins) != 0 {
		if err := b.snapshotGraph.DeletePins(pins); err != nil {
			return fmt.Errorf("releasing failed snapshot origin %d pins: %w", origin, err)
		}
	}
	if len(roots) != 0 {
		if err := b.snapshotGraph.ReleaseRoots(roots); err != nil {
			return fmt.Errorf("releasing failed snapshot origin %d roots: %w", origin, err)
		}
	}
	for {
		collected, collectErr := b.snapshotGraph.Collect(4096)
		if collectErr != nil {
			return fmt.Errorf("collecting failed snapshot origin %d: %w", origin, collectErr)
		}
		if collected == 0 {
			break
		}
	}
	if _, _, err := b.snapshotData.ReclaimUnreferenced(b.snapshotGraph); err != nil {
		return fmt.Errorf("reclaiming failed snapshot origin %d data: %w", origin, err)
	}
	objects, err := b.snapshotGraph.SnapshotObjects(origin)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspecting failed snapshot origin %d: %w", origin, err)
	}
	if len(objects) != 0 {
		return fmt.Errorf("failed snapshot origin %d still owns %d reachable objects", origin, len(objects))
	}
	if b.snapshotShared != nil {
		busy, err := b.snapshotShared.DiscardHandoff(origin)
		if err != nil {
			return fmt.Errorf("discarding failed snapshot origin %d pack: %w", origin, err)
		}
		if busy {
			return fmt.Errorf("discarding failed snapshot origin %d pack: pack is busy", origin)
		}
	}
	if volatile {
		if err := b.distributedSource.authority.AbandonSnapshotOrigin(node, b.distributedSource.incarnation, uint64(origin)); err != nil {
			return fmt.Errorf("abandoning failed snapshot origin %d: %w", origin, err)
		}
	}
	if _, ok := b.snapshotGraph.Snapshot(origin); !ok {
		return nil
	}
	inspection = b.snapshotGraph.InspectWithoutObjects()
	retain := make([]ttcsnapshot.SnapshotID, 0, len(inspection.Snapshots)-1)
	for _, snapshot := range inspection.Snapshots {
		if snapshot.ID != origin {
			retain = append(retain, snapshot.ID)
		}
	}
	if _, err := b.snapshotGraph.CollectEmptySnapshots(retain); err != nil {
		return fmt.Errorf("forgetting failed snapshot origin %d: %w", origin, err)
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
	if b.snapshotGraph == nil || b.snapshotData == nil {
		return errors.New("new-format daemon has no graph-native memory store")
	}
	if err := b.ensureDistributedSourceIndependent(info.ID, target.branch); err != nil {
		return fmt.Errorf("settling remote branches before dehydrating %s: %w", target, err)
	}
	if err := b.ensureDistributedTargetMemoryIndependent(info, persistentBranch); err != nil {
		return fmt.Errorf("materializing remote memory before dehydrating %s: %w", target, err)
	}
	store := b.containerStore()
	initConsolePaths, _ := store.BranchConsolePathsIn(info, target.branch)
	execSessions, _ := b.execSessionsOf(target)
	dehydrated := false
	originalState := normalizeLifecycleState(persistentBranch.State)
	transitionPublished := false
	captured := false
	defer func() {
		if dehydrated {
			return
		}
		if transitionPublished {
			_ = b.containerStore().UpdateBranchState(info.ID, target.branch, originalState)
		}
		if captured {
			// Dehydration has already replaced the live Kernel. If making it
			// durable fails, reconstruct it while the Sentry still owns the
			// capture so the operation remains atomic from the user's point of
			// view.
			_, _ = b.command("timetravel", target.container, "rehydrate", target.branch)
		}
		// Capture failure leaves the branch in its original running or paused
		// state. Restore every sidecar stopped in preparation for the attempt.
		_, _ = b.ensureConsoleDrain(target, false)
		// The sessions may have ended since they were listed.
		current := b.refreshExecSessions(target, execSessions)
		for index, session := range execSessions {
			if session.State == "running" {
				_, _ = b.ensureExecConsoleDrain(target, current[index], false)
			}
		}
		_ = b.ensurePublishedPortsForBranch(info, target.branch)
	}()
	if err := b.stopConsoleDrain(initConsolePaths); err != nil {
		return fmt.Errorf("stopping init console follower before dehydration: %w", err)
	}
	for _, session := range execSessions {
		if paths, pathErr := store.ExecConsolePathsIn(info, target.branch, session.ID); pathErr == nil {
			if err := b.stopConsoleDrain(paths); err != nil {
				return fmt.Errorf("stopping exec console follower %q before dehydration: %w", session.ID, err)
			}
		}
	}
	if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
		return fmt.Errorf("stopping published ports before dehydration: %w", err)
	}
	if b.dataRoot != "" {
		if err := b.containerStore().UpdateBranchState(info.ID, target.branch, "dehydrating"); err != nil {
			return fmt.Errorf("publishing dehydration transition for %s: %w", target, err)
		}
		transitionPublished = true
	}
	if _, err := b.command("timetravel", target.container, "dehydrate", target.branch); err != nil {
		return err
	}
	captured = true
	// The old console followers observe the intentionally released Kernel as an
	// exit and record SIGKILL. That is not a container exit: discard those
	// transient statuses so rehydration can attach fresh followers.
	for _, path := range []string{initConsolePaths.WaitStatus, initConsolePaths.PID} {
		if path != "" {
			_ = os.Remove(path)
		}
	}
	for _, session := range execSessions {
		if paths, pathErr := store.ExecConsolePathsIn(info, target.branch, session.ID); pathErr == nil {
			_ = os.Remove(paths.WaitStatus)
			_ = os.Remove(paths.PID)
		}
	}
	if b.dataRoot != "" {
		latest, err := store.LoadContainer(info.ID)
		if err != nil {
			return err
		}
		if b.dehydrationMayStayResident(latest, target.branch) {
			if err := store.MarkBranchDehydratedResident(info.ID, target.branch); err != nil {
				return fmt.Errorf("recording resident dehydration of %s: %w", target, err)
			}
			dehydrated = true
			return nil
		}
	}
	// The Sentry's other branches may still load pages of the image this
	// branch was restored from: its forks share its memory and its loaders,
	// as imports do the units they reuse. The dehydration supersedes the
	// branch's roots of that image, and releasing them let the image go while
	// they loaded from it: the relay of its packs, retired, failed their
	// loader, and every process they started crashed. The roots go to the
	// family, as a stop's and a removal's do, which releases them once those
	// loaders are done.
	if b.dataRoot != "" && b.runtimeHasOtherBranches(target.container, target.branch) {
		if info, persistentBranch, err = b.parkBranchMemoryGraph(info, persistentBranch); err != nil {
			return err
		}
	}
	// The family's resident dehydrations below share pages with the image
	// this one supersedes; see persistResidentDehydrations.
	var superseded []func()
	defer func() {
		for _, release := range superseded {
			release()
		}
	}()
	if err := b.persistDehydration(target, info, persistentBranch, &superseded); err != nil {
		return err
	}
	dehydrated = true
	if b.dataRoot != "" {
		latest, loadErr := b.containerStore().LoadContainer(target.container)
		if loadErr != nil {
			return loadErr
		}
		allCold, localBranches := allBranchesColdOnNode(latest, b.nodeID)
		if allCold && localBranches != 0 {
			if err := b.releaseRuntime(info.ID, func() error {
				if !b.runtimeRunsNoBranch(info.ID) {
					return nil
				}
				if err := b.persistResidentDehydrations(latest, &superseded); err != nil {
					return err
				}
				if _, deleteErr := b.command("delete", "--force", info.ID); deleteErr != nil {
					return fmt.Errorf("releasing fully dehydrated runtime: %w", deleteErr)
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// runtimeHasOtherBranches reports whether the runtime with this ID has a branch
// other than branchID, or may have: a live Sentry that cannot list its branches
// may have any.
func (b *runtimeBackend) runtimeHasOtherBranches(id, branchID string) bool {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return true
	}
	for _, family := range families {
		if family.ID != id {
			continue
		}
		if family.Error != "" {
			return !runtimeFamilyUnavailable(family)
		}
		return slices.ContainsFunc(family.Branches, func(branch Branch) bool { return branch.ID != branchID })
	}
	return false
}

// runtimeRunsNoBranch reports whether the runtime with this ID runs no branch,
// as its Sentry lists them, or is gone. A family's record does not say all its
// runtime runs: a remote fork imports its branch into the family's Sentry on
// the node, without the node's lifecycle lock, before the record lists the
// branch, and a dehydration of the Sentry's last live branch that took the
// record's word deleted the Sentry under the import.
func (b *runtimeBackend) runtimeRunsNoBranch(id string) bool {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return false
	}
	for _, family := range families {
		if family.ID == id {
			return runtimeFamilyLiveBranch(family) == "" || runtimeFamilyUnavailable(family)
		}
	}
	return true
}

// releaseRuntime runs release, which deletes the runtime with this ID if it
// finds the runtime running nothing it is kept for, unless a command is making
// a branch in the runtime, which keeps it; see familyOperations.releasing.
// release must decide from what the runtime lists once it runs: a branch
// made before then is listed, and none is made until it returns. own are the
// branches the caller is making in the runtime itself; see beginRelease.
func (b *runtimeBackend) releaseRuntime(id string, release func() error, own ...string) error {
	done, ok := b.operations.beginRelease(id, own...)
	if !ok {
		return nil
	}
	defer done()
	return release()
}

// runtimeRunsLiveBranch reports whether the runtime with this ID lists a
// branch that may run, its Sentry able to say.
func (b *runtimeBackend) runtimeRunsLiveBranch(id string) bool {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return false
	}
	for _, family := range families {
		if family.ID == id {
			return family.Error == "" && runtimeFamilyLiveBranch(family) != ""
		}
	}
	return false
}

// releaseIdleRuntime deletes the runtime with this ID if it runs no branch,
// its resident dehydrations made durable first, unless a command is making a
// branch in it. It reports whether it deleted the runtime.
func (b *runtimeBackend) releaseIdleRuntime(id string, own ...string) (bool, error) {
	info, err := b.containerStore().LoadRuntimeContainer(id)
	recorded := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	released := false
	err = b.releaseRuntime(id, func() error {
		if !b.runtimeRunsNoBranch(id) {
			return nil
		}
		if recorded {
			if err := b.persistResidentDehydrations(info, nil); err != nil {
				return err
			}
		}
		if _, err := b.command("delete", "--force", id); err != nil {
			return fmt.Errorf("releasing idle runtime %q: %w", id, err)
		}
		released = true
		return nil
	}, own...)
	return released, err
}

// importReplacement is the hand-over between an import that waits for its
// runtime's replacement and the replacement, which may outlast the import's
// patience: the replacement acts only once it has begun, and the import gives
// up only before.
type importReplacement struct {
	mu     sync.Mutex
	gaveUp bool
	begun  bool
}

// begin reports whether the import still waits, and if so, that it will wait
// for the replacement to end.
func (h *importReplacement) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.begun = !h.gaveUp
	return h.begun
}

// giveUp reports whether the import may give up, as it may before the
// replacement has begun, and if so, that it has.
func (h *importReplacement) giveUp() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gaveUp = !h.begun
	return h.gaveUp
}

// abandoned reports whether the import has given up.
func (h *importReplacement) abandoned() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gaveUp
}

// errImportGaveUp is a replacement's end once its import has given up.
var errImportGaveUp = errors.New("the import gave up")

// replaceRuntimeForImport makes the runtime with this ID one that a remote
// fork's import of branch can be restored into, under the lifecycle lock,
// which an import does not otherwise take. A runtime that runs a branch is
// kept, for the import to go beside the branch. One that runs none goes, its
// resident dehydrations made durable first, as a release would take it, and
// create makes the import's own in its place, as it makes one where the
// family has none: a peer restore needs a live branch to restore beside. It
// goes only once the exit of each branch it ran is recorded: a branch's exit
// is finalized through its Sentry, which drains the files the branch imported
// itself and has yet to read.
//
// It reports whether create made the runtime, and returns what ends the mark
// that the import is making branch in it, taken before the lock is given up,
// as an import takes it before it looks at a runtime. b must hold no
// lifecycle lock, nor make a branch in the runtime: a command making one, as
// a fork of a dehydrated branch, is waited for.
func (b *runtimeBackend) replaceRuntimeForImport(id, branch string, create func() error, handover *importReplacement) (bool, func(), error) {
	for {
		if handover.abandoned() {
			return false, func() {}, errImportGaveUp
		}
		unlock := b.holdFamilyLifecycle(b.familyID(id))
		done, ok := b.operations.beginRelease(id)
		if !ok {
			unlock()
			b.operations.waitMadeIn(id)
			continue
		}
		disposition, err := b.runtimeDispositionForImport(id)
		if err == nil && disposition == runtimeExiting {
			done()
			unlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !handover.begin() {
			done()
			unlock()
			return false, func() {}, errImportGaveUp
		}
		created := false
		if err == nil && disposition != runtimeLive {
			err = b.replaceStoppedRuntime(id, disposition == runtimeStopped, create)
			created = err == nil
		}
		done()
		making := b.operations.beginMaking(id, branch)
		unlock()
		return created, making, err
	}
}

// runtimeDisposition is what an import makes of a runtime it would go into.
type runtimeDisposition int

const (
	// runtimeGone is a runtime that is not there.
	runtimeGone runtimeDisposition = iota
	// runtimeLive runs a branch, beside which the import goes.
	runtimeLive
	// runtimeStopped runs none, and every exit of its branches is recorded.
	runtimeStopped
	// runtimeExiting runs none, and a branch's exit is still to be recorded.
	runtimeExiting
)

// runtimeDispositionForImport says what the runtime with this ID is to an
// import, under the lifecycle lock.
func (b *runtimeBackend) runtimeDispositionForImport(id string) (runtimeDisposition, error) {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return runtimeGone, err
	}
	for _, family := range families {
		if family.ID != id {
			continue
		}
		if !runtimeFamilyUnavailable(family) && runtimeFamilyLiveBranch(family) != "" {
			return runtimeLive, nil
		}
		info, err := b.containerStore().LoadRuntimeContainer(id)
		if errors.Is(err, os.ErrNotExist) {
			return runtimeStopped, nil
		}
		if err != nil {
			return runtimeGone, err
		}
		for _, branch := range info.Branches {
			if b.nodeID != "" && info.BranchNodeID(branch.ID) != b.nodeID {
				continue
			}
			switch normalizeLifecycleState(branch.State) {
			case "created", "dehydrated", "exited", "failed", "dead":
			default:
				return runtimeExiting, nil
			}
		}
		return runtimeStopped, nil
	}
	return runtimeGone, nil
}

// replaceStoppedRuntime deletes the runtime with this ID, if stopped says it
// is there, its resident dehydrations made durable first, and has create make
// the import's in its place.
func (b *runtimeBackend) replaceStoppedRuntime(id string, stopped bool, create func() error) error {
	if stopped {
		info, err := b.containerStore().LoadRuntimeContainer(id)
		if err == nil {
			err = b.persistResidentDehydrations(info, nil)
		} else if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			return err
		}
		if _, err := b.command("delete", "--force", id); err != nil {
			return fmt.Errorf("releasing runtime %q, which runs no branch: %w", id, err)
		}
	}
	return create()
}

// runtimeListsOnly reports whether the runtime with this ID lists no branch
// but those in seen, or is gone: a command that decided from seen to delete
// the runtime must keep it for any other, as a remote fork's import.
func (b *runtimeBackend) runtimeListsOnly(id string, seen []Branch) bool {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return false
	}
	for _, family := range families {
		if family.ID != id {
			continue
		}
		for _, branch := range family.Branches {
			if !slices.ContainsFunc(seen, func(known Branch) bool { return known.ID == branch.ID }) {
				return false
			}
		}
	}
	return true
}

// dehydrationMayStayResident reports whether branchID's dehydration may leave
// its capture in the family's Sentry rather than making it durable: in a
// standalone daemon, while another branch of the family on this node is live.
//
// The Sentry must stay up for that branch anyway, and a capture shares every
// page that neither branch has written since, so a durable copy bought little
// and cost a write of the branch's whole memory: a fork dehydrated at once was
// the most expensive case, when it should have been the cheapest. A clustered
// daemon still publishes at once, because other nodes rehydrate and fork
// dehydrated branches only from what was published.
func (b *runtimeBackend) dehydrationMayStayResident(info ttcbundle.ContainerInfo, branchID string) bool {
	if b.dataRoot == "" || b.distributedSource != nil || b.publicationQueue != nil {
		return false
	}
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			continue
		}
		if owner := info.BranchNodeID(branch.ID); b.nodeID != "" && owner != "" && owner != b.nodeID {
			continue
		}
		switch normalizeLifecycleState(branch.State) {
		case "created", "dehydrating", "dehydrated", "exited", "failed", "dead":
		default:
			return true
		}
	}
	return false
}

// persistResidentDehydrations makes durable every resident dehydration of
// info's family on this node. It must run while the family's Sentry is still
// up, before anything deletes the runtime that holds those captures.
//
// The images a branch's dehydration supersedes are released only once every
// branch is durable, or, if superseded is not nil, appended to it for the
// caller to release: a sibling forked from or rehydrated beside the branch
// holds pages of those images, and the graph recognizes a page it holds only
// while an image names it. Released as each branch was done, a family whose
// first branch had rewritten much of its memory made its siblings send all of
// those pages again.
func (b *runtimeBackend) persistResidentDehydrations(info ttcbundle.ContainerInfo, superseded *[]func()) error {
	if superseded == nil {
		var releases []func()
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		superseded = &releases
	}
	for _, branch := range info.Branches {
		if !ttcbundle.ResidentDehydration(branch) {
			continue
		}
		if owner := info.BranchNodeID(branch.ID); b.nodeID != "" && owner != "" && owner != b.nodeID {
			continue
		}
		if err := b.persistDehydration(selector{container: info.ID, branch: branch.ID}, info, branch, superseded); err != nil {
			return fmt.Errorf("making the dehydration of %s@%s durable: %w", info.Name, branch.ID, err)
		}
	}
	return nil
}

// persistResidentDehydration makes one resident dehydration durable on
// request, as waiting for a dehydrated branch to be durable asks.
func (b *runtimeBackend) persistResidentDehydration(target selector) error {
	// Exporting a capture must not race a lifecycle command consuming it.
	unlock := func() {}
	if !b.lifecycleLockHeld {
		unlock = b.holdFamilyLifecycle(b.familyID(target.container))
	}
	defer unlock()
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return err
	}
	if !ttcbundle.ResidentDehydration(branch) {
		return nil
	}
	return b.persistDehydration(resolved, info, branch, nil)
}

// persistDehydration makes a captured dehydration durable: it exports the
// capture held by the family's Sentry into the snapshot graph with its control
// state, records the roots, and publishes the branch as durably dehydrated.
// The Sentry keeps the capture, so the branch can still be rehydrated from
// memory, and a failed attempt can be repeated.
//
// The roots the branch's new image supersedes are released at once, or, if
// superseded is not nil, appended to it to be released later.
func (b *runtimeBackend) persistDehydration(target selector, info ttcbundle.ContainerInfo, persistentBranch ttcbundle.BranchInfo, superseded *[]func()) error {
	if b.dataRoot == "" {
		return b.queueBranchPublication(target, "dehydrate")
	}
	// A graph capture installs roots and parent edges to immutable origins.
	// Exclude the short provider-to-shared ledger freeze so every new reference
	// is included on exactly one side of the authority handoff.
	finishGraphCapture, err := b.beginForegroundCapture(info, longOperation{branch: target.branch, state: "dehydrating", shown: true})
	if err != nil {
		return err
	}
	graphCaptureActive := true
	defer func() {
		if graphCaptureActive {
			finishGraphCapture()
		}
	}()
	persisted := false
	finalPublished := false
	var graphMemoryRoots []string
	var graphMemoryRootObjects []string
	var graphControlRoot string
	var graphControlRootObject string
	var graphMemorySnapshot uint64
	graphMemoryRecorded := false
	previousGraphMemorySnapshot := persistentBranch.SnapshotID
	previousGraphMemoryRoots := append([]string(nil), persistentBranch.MemoryRoots...)
	previousGraphMemoryRootObjects := append([]string(nil), persistentBranch.MemoryRootObjects...)
	previousGraphControlRoot := persistentBranch.ControlRoot
	previousGraphControlRootObject := persistentBranch.ControlRootObject
	previousGraphReferenceOperation := persistentBranch.DistributedGraphReferenceOperation
	metadataDir := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "dehydrated")
	encodedBranch := ttcbundle.MemoryBranchKey(persistentBranch)
	finalDir := filepath.Join(metadataDir, encodedBranch)
	defer func() {
		if persisted {
			return
		}
		if graphMemoryRecorded {
			_ = b.containerStore().UpdateBranchSnapshotGraph(info.ID, target.branch, previousGraphMemorySnapshot, previousGraphMemoryRoots, previousGraphMemoryRootObjects, previousGraphControlRoot, previousGraphControlRootObject, previousGraphReferenceOperation)
		}
		if len(graphMemoryRoots) != 0 || graphControlRoot != "" {
			if graphErr := b.releaseGraphMemoryRoots(appendGraphRoot(append([]string(nil), graphMemoryRoots...), graphControlRoot)); graphErr != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: rolling back graph memory roots for %s: %v\n", target, graphErr)
			}
		}
		if graphMemorySnapshot != 0 && b.distributedSource != nil {
			if graphErr := b.discardFailedGraphSnapshot(ttcsnapshot.SnapshotID(graphMemorySnapshot)); graphErr != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: discarding failed graph snapshot %d for %s: %v\n", graphMemorySnapshot, target, graphErr)
			}
		}
		if finalPublished {
			_ = os.RemoveAll(finalDir)
		}
	}()
	if ttcbundle.ResidentDehydration(persistentBranch) {
		// The record names no image, so any here is what an interrupted
		// attempt left, and nothing refers to it.
		if err := b.removeInterruptedDehydrationImages(info.ID, persistentBranch); err != nil {
			return fmt.Errorf("removing an interrupted dehydration image: %w", err)
		}
	}
	if err := os.MkdirAll(metadataDir, 0700); err != nil {
		return fmt.Errorf("creating dehydration metadata directory: %w", err)
	}
	stagingDir, err := os.MkdirTemp(metadataDir, "."+encodedBranch+"-*")
	if err != nil {
		return fmt.Errorf("creating dehydration staging directory: %w", err)
	}
	defer func() {
		if stagingDir != "" {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	metadataFile, err := os.OpenFile(filepath.Join(stagingDir, "metadata.state"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("creating dehydration metadata: %w", err)
	}
	defer func() {
		if metadataFile != nil {
			_ = metadataFile.Close()
		}
	}()
	origin, sealState, built, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return fmt.Errorf("allocating dehydration snapshot origin: %w", err)
	}
	defer built()
	graphMemorySnapshot = uint64(origin)
	prefix := fmt.Sprintf("memory/%016x", uint64(origin))
	reader, writer, err := graphCaptureStream()
	if err != nil {
		return fmt.Errorf("creating graph dehydration stream: %w", err)
	}
	type graphImportOutcome struct {
		result ttcchunk.GraphImportResult
		err    error
	}
	imported := make(chan graphImportOutcome, 1)
	var output []byte
	var exportErr error
	var outcome graphImportOutcome
	// Streaming the branch's memory into the graph, and writing and importing
	// its control state, take time in proportion to the branch; they are the
	// family's own, and other commands go on meanwhile.
	if err := b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "dehydrating", shown: true}, func() error {
		go func() {
			result, importErr := b.importGraphCapture(reader, origin, prefix, branchCaptureImages(info, persistentBranch.ID)...)
			_ = reader.Close()
			imported <- graphImportOutcome{result: result, err: importErr}
		}()
		output, exportErr = b.commandWithFiles([]*os.File{writer, metadataFile}, "timetravel", "--dehydration-graph-stream-fd=3", "--dehydration-graph-stream-filtered", "--dehydration-fd=4", target.container, "dehydrate-export", target.branch)
		_ = writer.Close()
		outcome = <-imported
		if exportErr != nil {
			return fmt.Errorf("streaming graph-native dehydration memory: %w", exportErr)
		}
		if outcome.err != nil {
			return fmt.Errorf("importing graph-native dehydration memory: %w", outcome.err)
		}
		var exported boot.BranchDehydrationExportInfo
		if err := json.Unmarshal(output, &exported); err != nil {
			return fmt.Errorf("decoding dehydration capture description: %w", err)
		}
		capture := exported.Capture
		for _, root := range outcome.result.Roots {
			capture.MemoryRoots = append(capture.MemoryRoots, boot.BranchMemoryRoot{
				ResourceID: root.ResourceID,
				Name:       root.Name,
				Root:       root.Root.String(),
			})
			graphMemoryRoots = append(graphMemoryRoots, root.Name)
			graphMemoryRootObjects = append(graphMemoryRootObjects, root.Root.String())
		}
		capture.SnapshotID = uint64(origin)
		if err := writeDurableJSON(filepath.Join(stagingDir, "control.json"), &capture); err != nil {
			return fmt.Errorf("persisting dehydration control state: %w", err)
		}
		if err := metadataFile.Sync(); err != nil {
			return fmt.Errorf("syncing dehydration metadata: %w", err)
		}
		if err := metadataFile.Close(); err != nil {
			metadataFile = nil
			return fmt.Errorf("closing dehydration metadata: %w", err)
		}
		metadataFile = nil
		graphControlRoot = graphCaptureControlRootName(uint64(origin))
		controlObject, err := ttcsnapshot.ImportDirectory(context.Background(), b.snapshotGraph, b.snapshotData, origin, graphControlRoot, stagingDir)
		if err != nil {
			return fmt.Errorf("importing graph-native dehydration control state: %w", err)
		}
		graphControlRootObject = controlObject.String()
		if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
			return fmt.Errorf("sealing dehydration snapshot origin %d: %w", origin, err)
		}
		return nil
	}); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return err
	}
	if graphCaptureActive {
		finishGraphCapture()
		graphCaptureActive = false
	}
	if _, err := os.Stat(finalDir); err == nil {
		// A live branch keeps no capture on its node: one there is a cache of
		// an earlier cold incarnation, as a fork of the branch materializes
		// while the branch is dehydrated, which a stop on another node did not
		// let go of before the branch ran here again. It refused every later
		// dehydration of the branch, "dehydration image already exists".
		if err := os.RemoveAll(finalDir); err != nil {
			return fmt.Errorf("discarding the stale dehydration image of %s: %w", target, err)
		}
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
	if err := b.containerStore().UpdateBranchSnapshotGraph(info.ID, target.branch, graphMemorySnapshot, graphMemoryRoots, graphMemoryRootObjects, graphControlRoot, graphControlRootObject, ""); err != nil {
		return fmt.Errorf("recording graph-native dehydration roots: %w", err)
	}
	graphMemoryRecorded = true
	// Remote dependencies remain live until the asynchronous shared
	// publication commits. The publisher advances the transaction only after
	// its manifest is authoritative.
	if err := b.queueBranchPublication(target, "dehydrate"); err != nil {
		return fmt.Errorf("queueing durable dehydration publication: %w", err)
	}
	if err := b.containerStore().MarkBranchDehydrationDurable(info.ID, target.branch); err != nil {
		return fmt.Errorf("recording durable dehydration of %s: %w", target, err)
	}
	persisted = true
	release := func() {
		if len(previousGraphMemoryRoots) == 0 {
			return
		}
		if graphReferenceOperationForBranch(info, persistentBranch) == "" {
			previousBranch := persistentBranch
			previousBranch.MemoryRoots = previousGraphMemoryRoots
			previousBranch.MemoryRootObjects = previousGraphMemoryRootObjects
			previousBranch.ControlRoot = previousGraphControlRoot
			previousBranch.ControlRootObject = previousGraphControlRootObject
			if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(previousBranch), branchGraphRootObjects(previousBranch)); err != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: releasing superseded shared memory roots for %s: %v\n", target, err)
			}
		}
		previousRoots := appendGraphRoot(append([]string(nil), previousGraphMemoryRoots...), previousGraphControlRoot)
		err := b.releaseGraphMemoryRoots(previousRoots)
		if frozenForAuthorityHandoff(err) {
			// A publication is handing the superseded snapshot off, as this
			// capture's does one that a rehydration's cancellation left
			// volatile: wait for it without the lock. In a cluster no
			// reconciliation releases the roots later.
			err = b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "dehydrating", shown: true}, func() error {
				return b.releaseGraphMemoryRootsThroughHandoffs(previousRoots)
			})
		}
		if err != nil && b.stderr != nil {
			// The new roots are already authoritative. Retaining the superseded
			// roots is a safe leak that standalone graph reconciliation retries;
			// it must not turn a successful dehydration into an ambiguous failure.
			fmt.Fprintf(b.stderr, "ttc: warning: reclaiming superseded memory roots for %s: %v\n", target, err)
		}
		if err := b.releaseDistributedGraphReferences(graphReferenceOperationForBranch(info, persistentBranch)); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: releasing superseded distributed memory origins for %s: %v\n", target, err)
		}
	}
	if superseded != nil {
		*superseded = append(*superseded, release)
	} else {
		release()
	}
	activeDir := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "active", persistentBranch)
	if removeErr := os.RemoveAll(activeDir); removeErr != nil {
		return fmt.Errorf("removing superseded active memory: %w", removeErr)
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
	// The Sentry may have exited with the branch, its last, or exit as it is
	// asked: it stays only while a remote fork's import still reads from its
	// provider (see SandboxFamily.WaitDefaultExit), and one that is gone left
	// the import's files local, unless it did not exit so. The branch's
	// memory went with it. Asking it to drain, a natural exit's discard
	// failed for good, "connecting to control server ...: connection
	// refused", the branch never recorded exited, nor its runtime released.
	if err := b.drainDistributedTarget(info, branch); err != nil {
		// A Sentry closes its control server some time before it exits.
		if !b.waitRuntimeGone(info.ID, 5*time.Second) {
			return err
		}
		if err := b.distributedTargetFilesystemsLocal(info, branch); err != nil {
			return fmt.Errorf("the runtime of %s exited before its import was drained: %w", branch.ID, err)
		}
	}
	if err := b.containerStore().UpdateDistributedForkDependencies(logicalFamilyID(info), transactionID, 0, 0); err != nil {
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

// waitDistributedBranchMemory waits until all volatile readers owned by one
// live branch have either materialized their pages or failed. Durable graph
// readers intentionally report no source dependency and do not get drained.
func (b *runtimeBackend) waitDistributedBranchMemory(containerID, branchID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		output, err := b.command("timetravel", containerID, "distributed-dependency", branchID)
		if err != nil {
			return err
		}
		var status pgalloc.AsyncPagesFileLoadStats
		if err := json.Unmarshal(output, &status); err != nil {
			return fmt.Errorf("decoding remote-memory dependency: %w", err)
		}
		if status.Done {
			if status.Error != "" {
				return errors.New(status.Error)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out with %d remote bytes outstanding", status.BytesOutstanding)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitDistributedBranchFilesystems waits until the filesystem drain started
// for one branch has replaced every source of a remote fork's import in its
// runtime's repositories, or failed.
func (b *runtimeBackend) waitDistributedBranchFilesystems(containerID, branchID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		output, err := b.command("timetravel", containerID, "distributed-filesystem-drain-status", branchID)
		if err != nil {
			return err
		}
		var status boot.DistributedForkFilesystemDrainStatus
		if err := json.Unmarshal(output, &status); err != nil {
			return fmt.Errorf("decoding distributed filesystem drain status: %w", err)
		}
		if status.Done {
			if status.Error != "" {
				return fmt.Errorf("distributed filesystem drain failed: %s", status.Error)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out materializing remote filesystems")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// drainDistributedTarget makes branch's runtime read what branch imported by
// a remote fork from nowhere but itself.
func (b *runtimeBackend) drainDistributedTarget(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	// An exited branch's memory went with its process, and its Sentry
	// refuses to drain it, "branch is exited": only its files are drained.
	memory := branch.DependentMemoryBytes != 0 && normalizeLifecycleState(branch.State) != "exited"
	action := "distributed-filesystem-drain"
	if memory {
		action = "distributed-drain"
	}
	if _, err := b.command("timetravel", info.ID, action, branch.ID); err != nil {
		return err
	}
	if memory {
		return b.waitDistributedBranchMemory(info.ID, branch.ID, 5*time.Minute)
	}
	// The action only starts the filesystem drain. Made independent before
	// the drain ends, the transaction would let the source close the relay
	// the drain still reads the files from.
	return b.waitDistributedBranchFilesystems(info.ID, branch.ID, 5*time.Minute)
}

// runtimeGone reports whether the runtime with this ID is gone, or is a shell
// whose Sentry has exited.
func (b *runtimeBackend) runtimeGone(id string) bool {
	families, err := b.listRuntimeFamiliesOf(id)
	if err != nil {
		return false
	}
	for _, family := range families {
		if family.ID == id {
			return runtimeFamilyUnavailable(family)
		}
	}
	return true
}

// waitRuntimeGone reports whether the runtime with this ID is gone within
// timeout; see runtimeGone.
func (b *runtimeBackend) waitRuntimeGone(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !b.runtimeGone(id) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// distributedTargetFilesystemsLocal reports, as an error, what of branch's
// filesystems its repositories still hold only remotely.
func (b *runtimeBackend) distributedTargetFilesystemsLocal(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return err
		}
		remote, err := repository.BranchRemoteSourceObjects(repositoryPhysicalBranch(config, branch.FilesystemBranch))
		if closeErr := repository.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("reading filesystem %q: %w", config.Source, err)
		}
		if len(remote) != 0 {
			return fmt.Errorf("%d files of filesystem %q are still remote", len(remote), config.Source)
		}
	}
	return nil
}

// drainDistributedTargetFilesystems makes local, through its Sentry, the
// files that branch, a remote fork's import, still reads from its source: a
// reader of the branch's repositories in this process, as a commit's or a
// checkpoint's export, has no source of them attached.
func (b *runtimeBackend) drainDistributedTargetFilesystems(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	if transactionIDForBranch(info, branch) == "" || branch.DependentFilesystemBytes == 0 {
		return nil
	}
	switch normalizeLifecycleState(branch.State) {
	case "running", "paused":
	default:
		return nil
	}
	if _, err := b.command("timetravel", info.ID, "distributed-filesystem-drain", branch.ID); err != nil {
		return fmt.Errorf("draining the files %s imports: %w", branch.ID, err)
	}
	return b.waitDistributedBranchFilesystems(info.ID, branch.ID, 5*time.Minute)
}

// readSharedImageInProcess has this process read what a lazy import of a
// shared image into branchID, the repository's branch or a fork of it, has
// yet to read, for an export of it, as a commit's or a checkpoint's: its
// metadata through the shared packs' catalog, as a diff's observation does,
// and its files' bytes from the shared packs, without installing either in
// the repository, which the branch's Sentry owns. An export of a branch
// rehydrated from its durable image failed, "deferred TTC catalog ... has no
// attached authoritative source", until every file had been read; and one
// that installed the files itself left the image's catalog changed under the
// publications that name it by reference: a fork of the branch after its next
// rehydration was refused, "owned object ... cannot be retained".
func (b *runtimeBackend) readSharedImageInProcess(repository *ttcvolume.Repository, branchID string) error {
	if b.snapshotShared == nil {
		return nil
	}
	if lazy, err := repository.Branch(branchID); err != nil {
		return err
	} else if lazy.RemoteCatalog == "" || !lazy.RemoteShared {
		return nil
	}
	if err := repository.AttachImmutableMetadataSource(branchID, b.snapshotShared.Object); err != nil {
		return fmt.Errorf("attaching the shared image's metadata: %w", err)
	}
	repository.ReadPendingObjectsFrom(sharedPackObjectReader{store: b.snapshotShared})
	return nil
}

// sharedPackObjectReader reads the objects of a node's shared packs by their
// own identities.
type sharedPackObjectReader struct {
	store *ttcsnapshot.SharedPackStore
}

func (r sharedPackObjectReader) ReadObjectRange(object string, destination []byte, offset int64) (int, error) {
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		return 0, err
	}
	return r.store.ReadObjectRange(context.Background(), id, destination, offset)
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

// republishAbandonedDehydration queues again the publication of target's
// capture of snapshot, which a rehydration canceled and then did not restore.
// The branch stays dehydrating, as it was, rather than dehydrating without a
// publication, which every command refuses. A runtime that runs the branch
// again holds its state, and the capture is not published.
func (b *runtimeBackend) republishAbandonedDehydration(target selector, snapshot uint64) error {
	info, err := b.containerStore().LoadContainer(target.container)
	if err != nil {
		return err
	}
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return err
	}
	if branch.State != "dehydrating" || branch.PublicationID != "" || branch.SnapshotID != snapshot {
		return nil
	}
	if output, err := b.command("timetravel", info.ID, "branches"); err == nil {
		var runtimeBranches []runscBranchInfo
		if err := json.Unmarshal(output, &runtimeBranches); err != nil {
			return fmt.Errorf("decoding runtime branches: %w", err)
		}
		for _, runtimeBranch := range runtimeBranches {
			if runtimeBranch.ID == target.branch && (runtimeBranch.State == "running" || runtimeBranch.State == "paused") {
				return nil
			}
		}
	}
	if err := b.queueBranchPublication(target, "dehydrate"); err != nil {
		return fmt.Errorf("publishing the capture of %s again: %w", target, err)
	}
	return nil
}

// lostResidentDehydration records that target's resident capture went with
// its runtime, as a running branch's memory does, and says so.
func (b *runtimeBackend) lostResidentDehydration(target selector, info ttcbundle.ContainerInfo) error {
	exitStatus := 128 + int(linux.SIGKILL)
	if err := b.containerStore().StopBranch(info.ID, target.branch, exitStatus); err != nil {
		return fmt.Errorf("recording the loss of %s: %w", target, err)
	}
	return fmt.Errorf("branch %s was dehydrated in the runtime it shared with a live branch, and that runtime is gone; start it to boot it from its filesystem", target)
}

func (b *runtimeBackend) rehydrate(target selector) (retErr error) {
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
	// A dehydrated branch whose publication is still under way is fenced as
	// one still dehydrating is. Rehydrated in place from the capture its
	// Sentry kept, its activation dropped the publication from its record
	// while the worker committed it: the commit unplaced the branch, which
	// the record that no longer named the publication could not be rolled
	// forward to, and every later command of the branch failed, "repairing
	// committed cold branch metadata: branch ... publication is "", worker
	// has ...". A publication that committed first completes, once the fence
	// returns, the record the activation then goes on from; see
	// completeCommittedPublication.
	pendingPublication := persistentBranch.State == "dehydrated" && b.publicationQueue != nil &&
		persistentBranch.PublicationID != "" && persistentBranch.StorageState != "shared"
	if persistentBranch.State == "dehydrating" || pendingPublication {
		if b.publicationQueue == nil || persistentBranch.PublicationID == "" {
			return fmt.Errorf("branch %s has not completed its local dehydration capture", target)
		}
		publicationID := persistentBranch.PublicationID
		fenceErr := b.publicationQueue.CancelUncommitted(info.ID, target.branch, publicationID)
		if fenceErr == nil {
			fenceErr = b.containerStore().AbandonBranchPublication(info.ID, target.branch, publicationID)
			if fenceErr != nil && !errors.Is(fenceErr, ttcbundle.ErrPublicationCommitted) {
				return fmt.Errorf("retiring dehydration publication for %s: %w", target, fenceErr)
			}
		} else if !errors.Is(fenceErr, ttcstorage.ErrPublicationCommitted) {
			return fmt.Errorf("fencing dehydration publication for %s: %w", target, fenceErr)
		} else if err := b.completeCommittedPublication(info, target.branch, publicationID); err != nil {
			return fmt.Errorf("completing dehydration publication for %s: %w", target, err)
		}
		// A publication that committed before the fence made the branch
		// durably dehydrated: it is rehydrated as such, from its record.
		if fenceErr == nil {
			snapshot := persistentBranch.SnapshotID
			defer func() {
				if retErr != nil {
					retErr = errors.Join(retErr, b.republishAbandonedDehydration(target, snapshot))
				}
			}()
		}
		// AbandonBranchPublication clears the immutable publication fence. Reload
		// so the cold restore cannot accidentally reuse the canceled identity.
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
	// Rebuilding a branch from its durable image takes time in proportion to
	// its memory; other commands go on meanwhile.
	rehydrateCold := func(peer bool) error {
		return b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "rehydrating", shown: true}, func() error {
			return b.rehydrateCold(target, info, *persistentBranch, peer)
		})
	}
	if persistentBranch.State == "dehydrated" {
		if _, stateErr := b.command("state", info.ID); stateErr != nil {
			if ttcbundle.ResidentDehydration(*persistentBranch) {
				return b.lostResidentDehydration(target, info)
			}
			return rehydrateCold(false)
		}
		output, branchesErr := b.command("timetravel", info.ID, "branches")
		if branchesErr != nil {
			if ttcbundle.ResidentDehydration(*persistentBranch) {
				return b.lostResidentDehydration(target, info)
			}
			// A fully dehydrated family has no live Sentry. runsc may briefly
			// retain a stopped OCI record after the last branch releases it;
			// state then succeeds even though its control server is gone. Remove
			// that stale shell and rebuild directly from the durable image. A
			// shell is one whose Sentry is gone: one the family's record named a
			// live branch of, on any node, refused the rehydration for good, and
			// one it named none of was deleted, whether its Sentry ran or not.
			live, err := b.durableRestorePeer(info.ID)
			if err != nil {
				return err
			}
			if !live {
				return rehydrateCold(false)
			}
			if output, branchesErr = b.command("timetravel", info.ID, "branches"); branchesErr != nil {
				return branchesErr
			}
		}
		var runtimeBranches []runscBranchInfo
		if err := json.Unmarshal(output, &runtimeBranches); err != nil {
			return fmt.Errorf("decoding runtime branches: %w", err)
		}
		present, activePeer := false, false
		for _, runtimeBranch := range runtimeBranches {
			if runtimeBranch.ID == target.branch {
				present = true
			} else if runtimeBranch.State == "running" || runtimeBranch.State == "paused" {
				activePeer = true
			}
		}
		// A runtime that runs no branch any more, whose container stopped with
		// the last branch that ran here, can run none again: runsc refuses exec
		// and the like on a stopped container, and a peer restore needs a live
		// branch to restore beside. Its Sentry may still be exiting and answer,
		// as a stale shell's does not, or keep the captures of other dehydrated
		// branches. Make those durable, remove it, and rebuild from the image.
		// A remote fork's import making a branch in it, as one that made the
		// runtime anew for want of a branch to go beside, keeps it, as it
		// keeps it from a release: the rehydration waits for the import, and
		// goes beside its branch once it runs.
		rebuild := func() error {
			done, ok := b.operations.beginRelease(info.ID)
			for !ok {
				b.operations.waitMadeIn(info.ID)
				if b.runtimeRunsLiveBranch(info.ID) {
					return rehydrateCold(true)
				}
				done, ok = b.operations.beginRelease(info.ID)
			}
			err := b.persistResidentDehydrations(info, nil)
			if err == nil {
				_, _ = b.command("delete", "--force", info.ID)
			}
			done()
			if err != nil {
				return err
			}
			return rehydrateCold(false)
		}
		if !present {
			if ttcbundle.ResidentDehydration(*persistentBranch) {
				return b.lostResidentDehydration(target, info)
			}
			if !activePeer {
				return rebuild()
			}
			return rehydrateCold(true)
		}
		// The runtime keeps the capture it made durable, and the branch may
		// have run on another node since and been captured there: resuming
		// this one would take the branch back to its older state. The capture
		// is current only if the image it made here is still the one the
		// record names, as ensureLocalColdBranch judges a cached image.
		if b.nodeID != "" && !ttcbundle.ResidentDehydration(*persistentBranch) &&
			!localGraphCaptureMatches(existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", *persistentBranch), *persistentBranch) {
			if _, err := b.command("timetravel", info.ID, "destroy", target.branch); err != nil {
				return fmt.Errorf("discarding the superseded capture of %s: %w", target, err)
			}
			if !activePeer {
				return rebuild()
			}
			return rehydrateCold(true)
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
	families, err := b.listRuntimeFamiliesOf(target.container)
	if err != nil {
		return err
	}
	for _, family := range families {
		if family.ID != target.container {
			continue
		}
		for _, branch := range family.Branches {
			if branch.ID == target.branch {
				if err := b.containerStore().RecordBranchActivation(target.container, target.branch, branch.State, b.nodeID); err != nil {
					return err
				}
				b.releaseConsumedStandaloneMemoryBacking(target, *persistentBranch)
				b.retainRehydrationPageSource(info.ID, *persistentBranch, "")
				if _, err := b.ensureConsoleDrain(target, false); err != nil {
					return fmt.Errorf("reattaching init console: %w", err)
				}
				sessions, err := b.execSessionsOf(target)
				if err != nil {
					return fmt.Errorf("listing rehydrated exec sessions: %w", err)
				}
				for _, session := range sessions {
					if session.State == "running" {
						if _, err := b.ensureExecConsoleDrainIn(info, target, session, false); err != nil {
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

// releaseConsumedStandaloneMemoryBacking drops a rehydrated branch's control
// root once the restored Kernel has no outstanding reads from its image. The
// branch keeps its memory roots: its next dehydration finds in them the pages
// it has not changed since, and writes only the others, and that dehydration,
// or the branch's end, releases them. The durable branch CAS is the commit
// point: if graph collection then fails or the daemon crashes, standalone
// startup reconciliation finds and releases the orphan root. A clustered
// branch instead uses its origin ledger and publication protocol; it must
// never take this local-only shortcut.
func (b *runtimeBackend) releaseConsumedStandaloneMemoryBacking(target selector, branch ttcbundle.BranchInfo) {
	if b.nodeID != "" || branch.SnapshotID == 0 || branch.ControlRoot == "" {
		return
	}
	output, err := b.command("timetravel", target.container, "page-load-status", target.branch)
	if err != nil {
		return
	}
	var status pgalloc.AsyncPagesFileLoadStats
	if err := json.Unmarshal(output, &status); err != nil || !status.Done || status.Error != "" {
		return
	}
	cleared, err := b.containerStore().ClearBranchControlGraphIf(
		target.container, target.branch, branch.SnapshotID, branch.ControlRoot, branch.ControlRootObject)
	if err == nil && cleared {
		err = b.releaseGraphMemoryRoots([]string{branch.ControlRoot})
	}
	if err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: warning: releasing consumed control state for %s: %v\n", target, err)
	}
}

// reconcileConsumedMemoryBacking eventually releases roots whose
// demand loader completed after rehydrate returned. Polling is daemon-side and
// infrequent; ordinary guest faults never mutate the graph or storage catalog.
func (b *runtimeBackend) reconcileConsumedMemoryBacking() error {
	// A cluster node parks the backings of the families whose runtimes it
	// has, whose state is on it, and it releases only its own. It reads the
	// records of the containers whose backings this daemon parked, once it
	// has read those of all with state on it, for an earlier daemon's.
	source := b.distributedSource
	if b.nodeID == "" {
		source = nil
	}
	var counted map[string]uint64
	all := true
	if source != nil {
		var known bool
		counted, known = source.parkedContainers()
		all = !known
	}
	var containers []ttcbundle.ContainerInfo
	if all {
		var err error
		if containers, err = b.containerStore().ContainersWithActiveState(); err != nil {
			return err
		}
	} else {
		for id := range counted {
			info, err := b.containerStore().LoadRuntimeContainer(id)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			containers = append(containers, info)
		}
	}
	// The containers left with none of this node's backings.
	released := make(map[string]uint64)
	for id, count := range counted {
		released[id] = count
	}
	var parked []string
	for _, container := range containers {
		if len(container.RuntimeMemoryBackings) != 0 {
			parked = append(parked, container.ID)
		}
	}
	runtimeByID := make(map[string]Family, len(parked))
	if len(parked) != 0 {
		runtimeFamilies, err := b.listRuntimeFamiliesOf(parked...)
		if err != nil {
			return err
		}
		for _, family := range runtimeFamilies {
			runtimeByID[family.ID] = family
		}
	}
	for _, container := range containers {
		if b.nodeID == "" {
			for _, branch := range container.Branches {
				if len(branch.MemoryRoots) == 0 {
					continue
				}
				switch normalizeLifecycleState(branch.State) {
				case "running", "paused":
					if err := b.pruneRetainedImage(container.ID, branch); err != nil && b.stderr != nil {
						fmt.Fprintf(b.stderr, "ttc: warning: pruning the retained image of %s@%s: %v\n", container.Name, branch.ID, err)
					}
					if branch.ControlRoot != "" {
						b.releaseConsumedStandaloneMemoryBacking(selector{container: container.ID, branch: branch.ID}, branch)
					}
				}
			}
		}
		if len(container.RuntimeMemoryBackings) == 0 {
			continue
		}
		allDone := true
		family, local := runtimeByID[container.ID]
		if local {
			for _, branch := range family.Branches {
				switch normalizeLifecycleState(branch.State) {
				case "running", "paused", "restarting":
				default:
					continue
				}
				output, statusErr := b.command("timetravel", container.ID, "page-load-status", branch.ID)
				if statusErr != nil {
					allDone = false
					break
				}
				var status pgalloc.AsyncPagesFileLoadStats
				if json.Unmarshal(output, &status) != nil || !status.Done || status.Error != "" {
					allDone = false
					break
				}
			}
		}
		if !allDone {
			if source != nil {
				delete(released, container.ID)
				if all {
					source.noteParked(container.ID)
				}
			}
			continue
		}
		for _, backing := range container.RuntimeMemoryBackings {
			branch := runtimeBackingAsBranch(backing)
			if b.nodeID != "" && !local && !b.holdsGraphRoots(branch) {
				// Parked by the node whose runtime still loads from it.
				continue
			}
			operation := graphReferenceOperationForBranch(container, branch)
			if operation == "" {
				if err := b.releaseSharedGraphMemoryRoots(branchGraphRootNames(branch), branchGraphRootObjects(branch)); err != nil {
					return fmt.Errorf("releasing shared runtime memory backing %s for %s: %w", backing.ID, container.Name, err)
				}
			}
			if err := b.releaseDistributedGraphReferencesEventually(operation); err != nil {
				return fmt.Errorf("releasing distributed runtime memory backing %s for %s: %w", backing.ID, container.Name, err)
			}
			if err := b.releaseBranchGraphRoots(branch); err != nil {
				return fmt.Errorf("releasing local runtime memory backing %s for %s: %w", backing.ID, container.Name, err)
			}
			if err := b.containerStore().RemoveRuntimeMemoryBacking(container.ID, backing.ID); err != nil {
				return fmt.Errorf("retiring runtime memory backing %s for %s: %w", backing.ID, container.Name, err)
			}
		}
	}
	if source != nil {
		source.releasedParked(released, all)
	}
	return nil
}

// retainRehydrationPageSource removes a consumed per-branch control image
// after the graph-backed runtime is committed. All demand-loaded bulk memory
// lives in the central graph object store, so no branch-local page source must
// remain open or be moved beneath the active branch.
//
// Failure is post-commit cleanup: report it without rolling back a live branch.
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
	if err := os.RemoveAll(sourceDir); err != nil {
		if b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: removing consumed dehydration image for %s@%s: %v\n", containerID, branchID, err)
		}
		return
	}
	if dir, err := os.Open(filepath.Dir(sourceDir)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
}
func (b *runtimeBackend) rehydrateCold(target selector, info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, peer bool, filesystemSource ...ttcbundle.BranchInfo) (retErr error) {
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
	if localImageErr != nil && !errors.Is(localImageErr, os.ErrNotExist) {
		return localImageErr
	}
	if err := b.ensureLocalColdBranch(info, branch); err != nil {
		return fmt.Errorf("materializing cold branch: %w", err)
	}
	if err := b.ensureClusterFilesystemWorkingOrigins(info, branch.FilesystemBranch); err != nil {
		return fmt.Errorf("activating cold branch filesystem: %w", err)
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
	if capture.SnapshotID == 0 || len(capture.MemoryRoots) == 0 {
		return errors.New("new-format dehydration control state is not graph-native")
	}
	metadata, err := os.Open(filepath.Join(imageDir, "metadata.state"))
	if err != nil {
		return fmt.Errorf("opening dehydration metadata: %w", err)
	}
	defer metadata.Close()
	relaySource := branch
	if len(filesystemSource) != 0 {
		relaySource = filesystemSource[0]
	}
	var relays *coldFilesystemRelays
	// A local control image says nothing about whether the imported filesystem
	// repository still contains lazy whole-file placeholders. This is common
	// after a cold branch was materialized once, and is guaranteed for a child
	// forked from that branch. Donate shared-pack relays whenever a durable
	// backing manifest exists; AttachRemoteObjectSource closes an unused relay
	// immediately when all files are already local.
	if relaySource.StorageManifest != "" {
		relays, err = b.prepareColdFilesystemRelays(info, relaySource)
		if err != nil {
			return fmt.Errorf("preparing lazy cold filesystem: %w", err)
		}
	}
	if relays != nil {
		defer relays.close()
	}
	// A fork's child restores the capture its source made.
	restoreIDs, err := b.dehydrationRestoreIDs(info, relaySource)
	if err != nil {
		return err
	}
	if peer {
		return b.rehydrateColdPeer(target, info, branch, capture, imageDir, metadata, relays, restoreIDs)
	}
	// The memory image is read while the sandbox is made.
	type graphRestoreOpening struct {
		restore *graphMemoryRestore
		err     error
	}
	graphRestoreOpened := make(chan graphRestoreOpening, 1)
	go func() {
		restore, err := b.openGraphMemoryRestore(capture)
		graphRestoreOpened <- graphRestoreOpening{restore: restore, err: err}
	}()
	var graphRestore *graphMemoryRestore
	graphRestoreTaken := false
	takeGraphRestore := func() error {
		graphRestoreTaken = true
		opened := <-graphRestoreOpened
		graphRestore = opened.restore
		return opened.err
	}
	defer func() {
		if !graphRestoreTaken {
			_ = takeGraphRestore()
		}
		graphRestore.close()
	}()
	bundleDir, cleanupBundle, err := b.containerStore().PrepareDehydrationRestoreBundle(info.ID, capture.FilesystemBranch)
	if err != nil {
		return fmt.Errorf("preparing dehydration restore bundle: %w", err)
	}
	defer cleanupBundle()
	configured := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC, info.TTCNetworkInterface())
	configured, err = b.withRuntimeDiagnostics(configured, info.ID)
	if err != nil {
		return err
	}
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
		Resume: capture.Resume, ExecSessions: capture.ExecSessions, Quarantined: branch.Quarantined,
		FilesystemRestoreIDs: restoreIDs,
	}
	if relays != nil {
		restoreArgs.FilesystemConnections = len(relays.clients)
		restoreArgs.FilesystemToken = relays.token
	}
	restoreArgs.NetworkArgs, err = dehydrationNetworkArgs(info)
	if err != nil {
		return err
	}
	if err := takeGraphRestore(); err != nil {
		return err
	}
	if graphRestore.remote != nil {
		restoreArgs.GraphRemote = true
		restoreArgs.GraphRemoteToken = graphRestore.token
	}
	encoded, err := json.Marshal(&restoreArgs)
	if err != nil {
		return err
	}
	configReader, err := newConfigMemFD("ttc-dehydration-restore-config", encoded)
	if err != nil {
		return err
	}
	files := []*os.File{configReader, metadata}
	commandArgs := []string{"timetravel", "--dehydration-config-fd=3", "--dehydration-fd=4"}
	manifestFD := 3 + len(files)
	files = append(files, graphRestore.manifest)
	objectsFD := 3 + len(files)
	files = append(files, graphRestore.objects)
	commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-graph-manifest-fd=%d", manifestFD), fmt.Sprintf("--dehydration-graph-objects-fd=%d", objectsFD))
	if graphRestore.remote != nil {
		remoteFD := 3 + len(files)
		files = append(files, graphRestore.remote)
		commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-memory-remote-fd=%d", remoteFD))
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
	if err := b.containerStore().CommitActiveRepositoryRestoreIDsOf(info); err != nil {
		return fmt.Errorf("committing rehydrated filesystem identities: %w", err)
	}
	state := "paused"
	if capture.Resume {
		if _, err := b.command("timetravel", info.ID, "resume", target.branch); err != nil {
			return fmt.Errorf("resuming rehydrated branch: %w", err)
		}
		state = "running"
	}
	if err := b.containerStore().RecordBranchActivation(info.ID, target.branch, state, b.nodeID); err != nil {
		return err
	}
	// The live runtime and its persistent state are now committed. Failures in
	// sidecar attachment or page-source bookkeeping must not delete it.
	runtimeCreated = false
	b.releaseConsumedStandaloneMemoryBacking(target, branch)
	b.retainRehydrationPageSource(info.ID, branch, imageDir)
	// The branch was dehydrated, not exited, in info: what else the drain
	// reads of the record, its log options, never changes.
	if _, err := b.ensureConsoleDrainIn(info, target, false); err != nil {
		return fmt.Errorf("reattaching init console: %w", err)
	}
	sessions, err := b.execSessionsOf(target)
	if err != nil {
		return fmt.Errorf("listing rehydrated exec sessions: %w", err)
	}
	for _, session := range sessions {
		if session.State == "running" {
			if _, err := b.ensureExecConsoleDrainIn(info, target, session, false); err != nil {
				return fmt.Errorf("reattaching exec console %q: %w", session.ID, err)
			}
		}
	}
	if err := b.ensurePublishedPortsForBranch(info, target.branch); err != nil {
		return fmt.Errorf("restoring published ports: %w", err)
	}
	return nil
}

type graphMemoryRestore struct {
	manifest *os.File
	objects  *os.File
	remote   *os.File
	token    ttcsnapshot.ObjectRelayToken
}

func (restore *graphMemoryRestore) close() {
	if restore == nil {
		return
	}
	for _, file := range []*os.File{restore.manifest, restore.objects, restore.remote} {
		if file != nil {
			_ = file.Close()
		}
	}
}

type graphRestoreObjectSource struct {
	resolver *ttcsnapshot.Resolver
	objects  map[ttcsnapshot.ObjectID]ttcsnapshot.Object
}

func (source graphRestoreObjectSource) Fetch(ctx context.Context, id ttcsnapshot.ObjectID) (io.ReadCloser, uint64, error) {
	object, ok := source.objects[id]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	file, err := source.resolver.ResolveKnown(ctx, object)
	if err != nil {
		return nil, 0, err
	}
	return file, object.BulkLength, nil
}

// graphHoldsRoots reports whether this node's graph roots each of roots.
func (b *runtimeBackend) graphHoldsRoots(roots []ttcchunk.GraphMemoryRoot) bool {
	if b.snapshotGraph == nil {
		return false
	}
	for _, root := range roots {
		if current, ok := b.snapshotGraph.Root(root.Name); !ok || current != root.Root {
			return false
		}
	}
	return true
}

// readGraphMemoryImage reads from catalog the manifest of the memory image
// roots select, and the chunks its restore relays: those this node's store
// lacks, missing, and in a cluster those it has too, as spares. The daemon's
// sweep of the store may take one while the branch still loads it, even of an
// image this node's graph holds, once the publication that hands it to the
// shared store releases its roots. A reader that finds a chunk gone fetches
// it again.
func (b *runtimeBackend) readGraphMemoryImage(catalog ttcsnapshot.MemoryCatalog, roots []ttcchunk.GraphMemoryRoot) (ttcchunk.GraphManifestResult, map[ttcsnapshot.ObjectID]ttcsnapshot.Object, map[ttcsnapshot.ObjectID]bool, error) {
	adapted, err := ttcchunk.BuildGraphManifest(catalog, roots)
	if err != nil {
		return ttcchunk.GraphManifestResult{}, nil, nil, err
	}
	objectCatalog, ok := catalog.(interface {
		Object(ttcsnapshot.ObjectID) (ttcsnapshot.Object, bool)
	})
	if !ok {
		return ttcchunk.GraphManifestResult{}, nil, nil, errors.New("graph memory catalog cannot describe bulk objects")
	}
	relayed := make(map[ttcsnapshot.ObjectID]ttcsnapshot.Object)
	missing := make(map[ttcsnapshot.ObjectID]bool)
	for _, objectID := range adapted.Chunks {
		if file, err := b.snapshotData.Open(objectID, ttcsnapshot.MemoryChunkBytes); err == nil {
			file.Close()
			if b.snapshotResolver == nil {
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return ttcchunk.GraphManifestResult{}, nil, nil, fmt.Errorf("opening local memory chunk %s: %w", objectID, err)
		} else {
			missing[objectID] = true
		}
		object, ok := objectCatalog.Object(objectID)
		if !ok {
			return ttcchunk.GraphManifestResult{}, nil, nil, fmt.Errorf("memory chunk %s has no immutable metadata", objectID)
		}
		if object.BulkLength != ttcsnapshot.MemoryChunkBytes {
			return ttcchunk.GraphManifestResult{}, nil, nil, fmt.Errorf("memory chunk %s has length %d, want %d", objectID, object.BulkLength, ttcsnapshot.MemoryChunkBytes)
		}
		relayed[objectID] = object
	}
	return adapted, relayed, missing, nil
}

func (b *runtimeBackend) openGraphMemoryRestore(capture boot.BranchDehydrateInfo) (*graphMemoryRestore, error) {
	if b.snapshotData == nil || len(capture.MemoryRoots) == 0 || capture.SnapshotID == 0 {
		return nil, errors.New("graph-native dehydration roots are unavailable")
	}
	roots := make([]ttcchunk.GraphMemoryRoot, len(capture.MemoryRoots))
	rootObjects := make([]ttcsnapshot.ObjectID, len(capture.MemoryRoots))
	for index, recorded := range capture.MemoryRoots {
		id, err := ttcsnapshot.ParseObjectID(recorded.Root)
		if err != nil {
			return nil, fmt.Errorf("parsing graph memory root %q: %w", recorded.Name, err)
		}
		roots[index] = ttcchunk.GraphMemoryRoot{ResourceID: recorded.ResourceID, Name: recorded.Name, Root: id}
		rootObjects[index] = id
	}
	var (
		adapted ttcchunk.GraphManifestResult
		relayed map[ttcsnapshot.ObjectID]ttcsnapshot.Object
		missing map[ttcsnapshot.ObjectID]bool
		err     error
	)
	localRoots := b.graphHoldsRoots(roots)
	if localRoots {
		adapted, relayed, missing, err = b.readGraphMemoryImage(b.snapshotGraph, roots)
		// A publication completing hands the image to the shared store, then
		// releases this node's roots of it, and the graph collects the objects
		// even as they are read: the image is then read from the shared store.
		if err != nil && !b.graphHoldsRoots(roots) {
			localRoots = false
		}
	}
	if !localRoots {
		immutable, catalogErr := b.immutableSnapshotCatalog(context.Background(), rootObjects, "")
		if catalogErr != nil {
			return nil, fmt.Errorf("resolving immutable memory metadata: %w", catalogErr)
		}
		adapted, relayed, missing, err = b.readGraphMemoryImage(immutable, roots)
	}
	if err != nil {
		return nil, err
	}
	encoded, err := ttcchunk.EncodeManifest(adapted.Manifest)
	if err != nil {
		return nil, err
	}
	manifestFD, err := memutil.CreateMemFD("ttc-graph-memory-manifest", 0)
	if err != nil {
		return nil, err
	}
	manifest := os.NewFile(uintptr(manifestFD), "ttc-graph-memory-manifest")
	if _, err := manifest.Write(encoded); err != nil {
		manifest.Close()
		return nil, err
	}
	if _, err := manifest.Seek(0, io.SeekStart); err != nil {
		manifest.Close()
		return nil, err
	}
	objects, err := os.Open(filepath.Join(b.activeDataRoot(), "objects"))
	if err != nil {
		manifest.Close()
		return nil, err
	}
	restore := &graphMemoryRestore{manifest: manifest, objects: objects}
	if len(relayed) == 0 {
		return restore, nil
	}
	if _, err := rand.Read(restore.token[:]); err != nil {
		restore.close()
		return nil, err
	}
	// Cold graph objects that are already authoritative in shared storage are
	// served by a detached helper. Do not make a live Sentry's future page
	// faults depend on the lifetime of the control daemon that rehydrated it.
	// The helper serves the spares too, shared by the time one goes missing;
	// a relay of spares alone is the daemon's own.
	shared := len(missing) != 0 && b.snapshotShared != nil
	if shared {
		ids := make([]ttcsnapshot.ObjectID, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		stored, err := b.snapshotShared.Objects(ids)
		shared = err == nil
		for id := range missing {
			if !shared {
				break
			}
			shared = stored[id].BulkLength == relayed[id].BulkLength
		}
	}
	if shared {
		spare := make(map[ttcsnapshot.ObjectID]bool, len(relayed)-len(missing))
		for id := range relayed {
			if !missing[id] {
				spare[id] = true
			}
		}
		client, _, err := startSharedMemoryRelay(b.dataRoot, relayed, spare, restore.token)
		if err != nil {
			restore.close()
			return nil, fmt.Errorf("starting shared graph memory relay: %w", err)
		}
		restore.remote = client
		return restore, nil
	}
	if b.snapshotResolver == nil {
		restore.close()
		return nil, errors.New("graph memory has non-local chunks and no distributed resolver")
	}
	fds, err := cloexecStreamSocketpair()
	if err != nil {
		restore.close()
		return nil, err
	}
	server := os.NewFile(uintptr(fds[0]), "ttc-graph-memory-server")
	restore.remote = os.NewFile(uintptr(fds[1]), "ttc-graph-memory-client")
	allowed := make(map[ttcsnapshot.ObjectID]uint64, len(relayed))
	for id, object := range relayed {
		allowed[id] = object.BulkLength
	}
	go func() {
		err := ttcsnapshot.ServeObjectRelay(server, restore.token, graphRestoreObjectSource{resolver: b.snapshotResolver, objects: relayed}, allowed)
		b.reportColdRelayError("graph memory", err)
	}()
	return restore, nil
}

func (b *runtimeBackend) rehydrateColdPeer(target selector, info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, capture boot.BranchDehydrateInfo, sourceDir string, metadata *os.File, relays *coldFilesystemRelays, restoreIDs map[string]string) error {
	var err error
	targets := make([]*os.File, 0, len(capture.Memories))
	defer func() {
		for _, file := range targets {
			_ = file.Close()
		}
	}()
	if capture.SnapshotID == 0 || len(capture.MemoryRoots) == 0 {
		return errors.New("new-format peer restore is not graph-native")
	}
	// Graph restores install application chunks into the family's existing
	// MemoryFile. Only private filesystem MemoryFiles need new donated backing
	// files.
	firstTarget := 1
	for i := firstTarget; i < len(capture.Memories); i++ {
		fd, err := memutil.CreateMemFD(fmt.Sprintf("ttc-rehydration-memory-%04d", i), 0)
		if err != nil {
			return fmt.Errorf("creating anonymous active dehydration memory %d: %w", i, err)
		}
		file := os.NewFile(uintptr(fd), fmt.Sprintf("ttc-rehydration-memory-%04d", i))
		// Peer restore constructs its MemoryFiles after systrap has reserved the
		// Sentry's application address range. Perform the executable mmap used
		// to prime host IMA here, before donating the file to the sandbox.
		hostmm.IMAWorkAroundForMemFile(file.Fd())
		targets = append(targets, file)
	}
	restoreArgs := boot.DurableDehydrationRestoreArgs{
		ID: target.branch, ParentID: branch.ParentID, FilesystemBranch: capture.FilesystemBranch,
		Resume: capture.Resume, ExecSessions: capture.ExecSessions, UseDonatedMemoryFiles: true,
		ReusePeerApplicationMemoryFile: true, Quarantined: branch.Quarantined, FilesystemRestoreIDs: restoreIDs,
	}
	if relays != nil {
		restoreArgs.FilesystemConnections = len(relays.clients)
		restoreArgs.FilesystemToken = relays.token
	}
	networkArgs, err := dehydrationNetworkArgs(info)
	if err != nil {
		return err
	}
	restoreArgs.NetworkArgs = networkArgs
	graphRestore, err := b.openGraphMemoryRestore(capture)
	if err != nil {
		return err
	}
	defer graphRestore.close()
	if graphRestore.remote != nil {
		restoreArgs.GraphRemote = true
		restoreArgs.GraphRemoteToken = graphRestore.token
	}
	encoded, err := json.Marshal(&restoreArgs)
	if err != nil {
		return err
	}
	configReader, err := newConfigMemFD("ttc-dehydration-restore-config", encoded)
	if err != nil {
		return err
	}
	files := []*os.File{configReader, metadata}
	commandArgs := []string{"timetravel", "--dehydration-config-fd=3", "--dehydration-fd=4"}
	manifestFD := 3 + len(files)
	files = append(files, graphRestore.manifest)
	objectsFD := 3 + len(files)
	files = append(files, graphRestore.objects)
	commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-graph-manifest-fd=%d", manifestFD), fmt.Sprintf("--dehydration-graph-objects-fd=%d", objectsFD))
	if graphRestore.remote != nil {
		remoteFD := 3 + len(files)
		files = append(files, graphRestore.remote)
		commandArgs = append(commandArgs, fmt.Sprintf("--dehydration-memory-remote-fd=%d", remoteFD))
	}
	targetStartFD := 3 + len(files)
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
	if err := b.containerStore().CommitActiveRepositoryRestoreIDsOf(info); err != nil {
		return fmt.Errorf("committing rehydrated peer filesystem identities: %w", err)
	}
	_ = metadata.Close()
	state := "paused"
	if capture.Resume {
		state = "running"
	}
	if err := b.containerStore().RecordBranchActivation(info.ID, target.branch, state, b.nodeID); err != nil {
		if _, destroyErr := b.command("timetravel", info.ID, "destroy", target.branch); destroyErr != nil {
			return fmt.Errorf("recording rehydrated peer: %w (rolling it back also failed: %v)", err, destroyErr)
		}
		return err
	}
	// Persistent state now names the live peer. Keep its on-demand snapshot
	// readers reachable and do not roll the peer back for artifact bookkeeping.
	b.releaseConsumedStandaloneMemoryBacking(target, branch)
	b.retainRehydrationPageSource(info.ID, branch, sourceDir)
	if _, err := b.ensureConsoleDrainIn(info, target, false); err != nil {
		return fmt.Errorf("reattaching init console: %w", err)
	}
	sessions, err := b.execSessionsOf(target)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if session.State == "running" {
			if _, err := b.ensureExecConsoleDrainIn(info, target, session, false); err != nil {
				return err
			}
		}
	}
	return b.ensurePublishedPortsForBranch(info, target.branch)
}

func dehydrationNetworkArgs(info ttcbundle.ContainerInfo) (*sentryapi.CreateLinksAndRoutesArgs, error) {
	link := sentryapi.DefaultLoopbackLink
	args := &sentryapi.CreateLinksAndRoutesArgs{LoopbackLinks: []sentryapi.LoopbackLink{link}, IsRestore: true}
	switch info.NetworkMode {
	case "none":
		return args, nil
	case "ttc":
		cfg := &sentryapi.TimeTravelNetworkConfig{NAT: info.NetworkNAT, InterfaceName: info.TTCNetworkInterface()}
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
	if b.distributedSource != nil && b.distributedSource.authority != nil && b.nodeID != "" && (b.removals == nil || !b.removals.renewed) {
		if _, err := b.renewDistributedExecutionAuthority(); err != nil {
			return fmt.Errorf("renewing branch removal execution authority: %w", err)
		}
		if b.removals != nil {
			b.removals.renewed = true
		}
	}
	requested := target
	logicalInfo, err := b.resolveContainer(requested.container)
	if errors.Is(err, os.ErrNotExist) && requested.branch == defaultBranchID {
		// A sandbox without a record lists as a family with only this branch.
		if found, err := b.removeRecordlessRuntime(requested.container, force); found || err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	logicalFamily := logicalFamilyID(logicalInfo)
	corrupted := false
	if b.distributedSource != nil && b.distributedSource.authority != nil {
		if authority, authorityErr := b.distributedSource.authority.Branch(logicalFamily, requested.branch); authorityErr == nil {
			corrupted = authority.Corrupted
		} else if !errors.Is(authorityErr, os.ErrNotExist) {
			return fmt.Errorf("loading branch authority for %s: %w", requested, authorityErr)
		}
	}
	// The family as resolved above, as resolveSelector would resolve it again.
	if waited, err := b.waitBusy(logicalInfo.ID); err != nil {
		return err
	} else if waited {
		if logicalInfo, err = b.resolveContainer(requested.container); err != nil {
			return err
		}
	}
	resolved, info, err := b.resolveSelectorWaiting(logicalInfo, target)
	if err != nil {
		return err
	}
	target = resolved
	if !corrupted {
		if err := b.ensureDistributedSourceIndependent(info.ID, target.branch); err != nil {
			return fmt.Errorf("settling remote branches before removing %s: %w", target, err)
		}
	}
	var recorded *ttcbundle.BranchInfo
	for i := range info.Branches {
		if info.Branches[i].ID == target.branch {
			recorded = &info.Branches[i]
			break
		}
	}
	if recorded == nil {
		// A failed node may lose its only physical catalog before the authority can mark
		// the branch corrupted. The control-plane tombstone is then the only
		// remaining object, and removing it is both necessary and sufficient.
		if corrupted {
			return b.distributedSource.authority.RemoveCorruptedBranch(logicalFamily, requested.branch)
		}
		return fmt.Errorf("branch %q does not exist", target.branch)
	}
	if !force && branchRemovalRequiresForce(recorded.State, corrupted) {
		return fmt.Errorf("branch %s is %s; use -f to force removal", target, normalizeLifecycleState(recorded.State))
	}
	if b.publicationQueue != nil {
		canceled, err := b.publicationQueue.CancelBranch(info.ID, recorded.ID)
		if err != nil {
			return fmt.Errorf("canceling pending publications for %s: %w", target, err)
		}
		// A publication under way may have changed the record.
		if canceled != 0 {
			info, err = b.containerStore().LoadContainer(info.ID)
			if err != nil {
				return err
			}
			recorded = nil
			for i := range info.Branches {
				if info.Branches[i].ID == target.branch {
					recorded = &info.Branches[i]
					break
				}
			}
			if recorded == nil {
				return fmt.Errorf("branch %q disappeared while fencing publication", target.branch)
			}
		}
	}
	if err := b.claimUnplacedForRemoval(logicalFamily, requested.branch, info.ID, target.branch); err != nil {
		return err
	}
	remaining := len(info.Branches)
	logicalPeers, err := b.distributedFamilyHasPeerPlacement(logicalFamily, requested.branch)
	if err != nil {
		return fmt.Errorf("checking logical peers before removing %s: %w", requested, err)
	}
	// An import going into the container, as a migration's or a remote
	// fork's target beside its last branch does, is recorded apart from its
	// branches until it is published, and runs in the container's runtime
	// by then: the container stays for it, and only the branch goes. Imports
	// take no lifecycle lock, so the record is read again. Removed whole, the
	// container took a migrated branch's runtime with it, and the branch was
	// lost, "loading physical runtime for ...: container ... does not exist",
	// in a run of the four-client probe with migrations.
	importing := len(info.DistributedForkImports) != 0
	if latest, loadErr := b.containerStore().LoadRuntimeContainer(info.ID); loadErr == nil && len(latest.DistributedForkImports) != 0 {
		importing = true
	}
	// A transaction-scoped physical container may always disappear with its
	// final local branch: the canonical family record is a different object.
	// The canonical physical container, however, also carries the logical
	// family identity and must survive while a peer placement exists remotely.
	if remaining == 1 && !importing && (!logicalPeers || info.ID != logicalFamily) {
		// Removing the family's final branch also removes the now-empty family.
		// Named volumes remain independently managed, matching a normal rm
		// without --volumes.
		if err := b.removeFamily(target.container, true, false, &info); err != nil {
			return err
		}
		return b.removeLogicalBranchPlacement(logicalFamily, requested.branch)
	}
	releaseClients, err := b.quiesceConsoleClients(target)
	if err != nil {
		return fmt.Errorf("detaching console clients for %s: %w", target, err)
	}
	defer releaseClients()
	store := b.containerStore()
	if paths, pathErr := store.BranchConsolePathsIn(info, target.branch); pathErr == nil {
		if err := b.stopConsoleDrain(paths); err != nil {
			return fmt.Errorf("stopping console follower for %s: %w", target, err)
		}
	}
	// The runtime's family as it is now, for the steps below. A branch it
	// does not run has no exec sessions, and one it does not have has nothing
	// to destroy: a stop leaves no exited branch behind in a runtime that
	// keeps others.
	var runtimeFamily *Family
	runtimeBranchState := ""
	if families, listErr := b.listRuntimeFamiliesOf(target.container); listErr == nil {
		runtimeFamily = &Family{}
		for _, family := range families {
			if family.ID != target.container {
				continue
			}
			// What it runs is unknown, as if it could not be listed: taken
			// for main's alone, a removal took the branch for gone, and
			// removed its record without destroying it.
			if family.branchesUnknown() {
				runtimeFamily = nil
				break
			}
			*runtimeFamily = family
			for _, branch := range family.Branches {
				if branch.ID == target.branch {
					runtimeBranchState = normalizeLifecycleState(branch.State)
				}
			}
			break
		}
	}
	if runtimeFamily == nil || runtimeBranchState == "running" || runtimeBranchState == "paused" {
		if sessions, sessionsErr := b.execSessionsOf(target); sessionsErr == nil {
			for _, session := range sessions {
				paths, pathErr := store.ExecConsolePathsIn(info, target.branch, session.ID)
				if pathErr == nil {
					if err := b.stopConsoleDrain(paths); err != nil {
						return fmt.Errorf("stopping exec console follower %q for %s: %w", session.ID, target, err)
					}
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
	// A branch its Sentry no longer has is past preparing: a removal that
	// destroyed it, and failed after, leaves it recorded as it was, and its
	// retry must go on, as destroyRuntime does below.
	switch normalizeLifecycleState(recorded.State) {
	case "running", "restarting":
		if runtimeFamily != nil && runtimeBranchState == "" {
			break
		}
		if _, err := b.command("timetravel", target.container, "prepare-stop", target.branch); err != nil {
			return fmt.Errorf("preparing %s for removal: %w", target, err)
		}
	}
	// Branch destruction transfers unresolved page loaders to an in-process
	// sibling. Move immutable graph authority to the family before that
	// transfer; otherwise reclaiming the removed branch can unlink chunks that
	// the sibling will demand later.
	if runtimeFamily != nil && len(runtimeFamily.Branches) >= 2 {
		var updated ttcbundle.BranchInfo
		info, updated, err = b.parkBranchMemoryGraph(info, *recorded)
		if err != nil {
			return err
		}
		recorded = &updated
	}
	destroyRuntime := func() error {
		if runtimeFamily != nil && runtimeBranchState == "" {
			return nil
		}
		// Elastic placement can leave this as the last physical branch on this
		// node while persistent siblings run elsewhere. runsc's branch destroy
		// correctly refuses to remove a runtime's final branch, so delete this
		// node-local runtime shell instead; shared family metadata is removed
		// separately below only for the selected branch. As a release does,
		// wait for a remote fork's import into the runtime under way, which
		// the runtime lists only once it is made, and keep a runtime that
		// lists a branch made meanwhile, of which this branch alone then goes;
		// see familyOperations.releasing.
	final:
		for {
			families, listErr := b.listRuntimeFamiliesOf(target.container)
			if listErr != nil {
				break
			}
			for _, family := range families {
				if family.ID != target.container || family.branchesUnknown() || len(family.Branches) != 1 || family.Branches[0].ID != target.branch {
					continue
				}
				done, ok := b.operations.beginRelease(target.container)
				if !ok {
					b.operations.waitMadeIn(target.container)
					continue final
				}
				if !b.runtimeListsOnly(target.container, family.Branches) {
					done()
					continue final
				}
				_, err := b.command("delete", "--force", target.container)
				done()
				if err != nil {
					return fmt.Errorf("deleting final physical runtime branch %s: %w", target, err)
				}
				return nil
			}
			break
		}
		if _, err := b.command("timetravel", target.container, "destroy-reclaim", target.branch); err == nil {
			return nil
		} else {
			destroyErr := err
			families, listErr := b.listRuntimeFamiliesOf(target.container)
			if listErr != nil {
				return destroyErr
			}
			for _, family := range families {
				if family.ID != target.container {
					continue
				}
				if family.branchesUnknown() {
					return destroyErr
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
		if err := b.reclaimBranchBacking(info, *recorded); err != nil {
			if portErr != nil {
				return fmt.Errorf("reclaiming filesystems for %s: %w (stopping published ports also failed: %v)", target, err, portErr)
			}
			return fmt.Errorf("reclaiming branch backing for %s: %w", target, err)
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
		// A migration that brought the branch back into its family's own
		// runtime left the branch's own record beside the migration's. It goes
		// with the branch: this node's reconciliation, finding it running here
		// while the family's runtime lives on, placed the removed branch anew.
		if target.container == logicalFamily && target.branch != requested.branch {
			if former, err := persistentBranchInfo(info, requested.branch); err == nil {
				if err := b.reclaimBranchBacking(info, former); err != nil {
					return fmt.Errorf("reclaiming the former record of %s: %w", requested, err)
				}
				if err := store.RemoveBranchArtifacts(target.container, former); err != nil {
					return fmt.Errorf("removing the former artifacts of %s: %w", requested, err)
				}
				if err := store.RemoveBranchRecord(target.container, requested.branch); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("persisting deletion of the former record of %s: %w", requested, err)
				}
			}
		}
		if err := b.removeLogicalBranchPlacement(logicalFamily, requested.branch); err != nil {
			return fmt.Errorf("removing cluster placement for %s: %w", requested, err)
		}
		latest, err := store.LoadContainer(target.container)
		if err != nil {
			return err
		}
		if err := b.deleteRuntimeIfProcessFree(latest); err != nil {
			return err
		}
		// The family's node-local state goes with its last runtime on the
		// node. While one still runs there, the state stays until the node
		// reclaims it as orphaned.
		if err := b.removeLocalFamilyStateIfUnowned(info.ID); err != nil && !errors.Is(err, errRuntimeFamilyLive) {
			return fmt.Errorf("reclaiming node-local state for %s: %w", target, err)
		}
	}
	return portErr
}

func branchRemovalRequiresForce(state string, corrupted bool) bool {
	if corrupted {
		// Corruption fences execution authority and clears physical placement.
		// Node-local metadata from the lost incarnation may still say running,
		// but no live runtime remains for -f to protect against.
		return false
	}
	switch normalizeLifecycleState(state) {
	case "running", "paused", "creating", "dehydrating", "rehydrating", "restarting":
		return true
	default:
		return false
	}
}

func (b *runtimeBackend) processes(target selector) ([]ProcessInfo, error) {
	resolved, _, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	return b.processesOf(resolved)
}

// processesOf is processes of a branch whose physical runtime and branch
// target names already.
func (b *runtimeBackend) processesOf(target selector) ([]ProcessInfo, error) {
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

// publishLiveFilesystemMetadata establishes a manager-observation boundary
// for branch filesystems that keep a bounded metadata tail inside the Sentry.
// This deliberately does not fsync file payloads: diff and accounting require
// a current coherent graph view, not a guest-visible durability operation.
func (b *runtimeBackend) publishLiveFilesystemMetadata(container string, branches ...ttcbundle.BranchInfo) error {
	for _, branch := range branches {
		state := normalizeLifecycleState(branch.State)
		if state != "running" && state != "paused" {
			continue
		}
		if _, err := b.command("timetravel", container, "filesystem-publish", branch.ID); err != nil {
			return fmt.Errorf("publishing live filesystem metadata for %s@%s: %w", container, branch.ID, err)
		}
	}
	return nil
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
	if err := b.publishLiveFilesystemMetadata(resolved.container, branch); err != nil {
		return nil, err
	}
	filesystemBranch := branch.FilesystemBranch
	if filesystemBranch == "" {
		filesystemBranch = offlineCopyBranch(info, resolved.branch)
	}
	spec, err := b.containerStore().LoadSpecOf(info)
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
		physicalBranch := repositoryPhysicalBranch(config, filesystemBranch)
		if b.snapshotShared != nil {
			if err := repository.AttachImmutableMetadataSource(physicalBranch, b.snapshotShared.Object); err != nil {
				repository.Close()
				return nil, fmt.Errorf("attaching shared metadata for %s: %w", mount, err)
			}
		}
		diff, diffErr := repository.BranchDiff(physicalBranch)
		closeErr := repository.Close()
		if diffErr != nil {
			return nil, fmt.Errorf("reading repository diff for %s: %w", mount, diffErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for _, change := range diff {
			if syntheticRootfsMountChange(spec, mount, change.Path) {
				continue
			}
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

// syntheticRootfsMountChange hides rootfs entries created solely to host OCI
// mounts. The mounted repository reports its own guest-visible changes; also
// returning the covered rootfs directory produces duplicate paths such as
// /data and leaks runtime scaffolding such as /proc and /sys into ttc diff.
func syntheticRootfsMountChange(spec *specs.Spec, repositoryMount, repositoryPath string) bool {
	if repositoryMount != "/" {
		return false
	}
	path := filepath.Clean(repositoryPath)
	if path == "/" {
		return true
	}
	for _, mount := range spec.Mounts {
		if filepath.Clean(mount.Destination) == path {
			return true
		}
	}
	return false
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
	physicalToLogical := make(map[string]string)
	if b.nodeID != "" {
		// The family's placements alone: listing the cluster's grew with
		// every family of it.
		placements, placementErr := b.placementCatalog()
		var records []cluster.PlacementRecord
		if placementErr == nil {
			records, placementErr = placements.ListFamily(logicalFamilyID(info))
		}
		filtered := branches[:0]
		for _, branch := range branches {
			if placementErr != nil {
				if info.BranchNodeID(branch.ID) == b.nodeID {
					physicalToLogical[branch.ID] = branch.ID
					filtered = append(filtered, branch)
				}
				continue
			}
			for _, record := range records {
				if record.PhysicalContainer != info.ID || record.PhysicalBranch != branch.ID || record.OwnerNode != b.nodeID ||
					(record.State != cluster.PlacementRunning && record.State != cluster.PlacementPaused) {
					continue
				}
				physicalToLogical[branch.ID] = record.BranchID
				branch.NodeID = record.OwnerNode
				branch.Quarantined = record.Quarantined
				filtered = append(filtered, branch)
				break
			}
		}
		branches = filtered
	}
	logicalBranchID := func(physical string) string {
		if logical := physicalToLogical[physical]; logical != "" {
			return logical
		}
		return physical
	}
	if err := b.publishLiveFilesystemMetadata(info.ID, branches...); err != nil {
		return FamilyResourceStats{}, err
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].ID < branches[j].ID })
	result.Branches = make([]BranchResourceStats, 0, len(branches))
	byID := make(map[string]*BranchResourceStats, len(branches))
	filesystemBranches := make(map[string]string, len(branches))
	volatileTransactions := make(map[string]bool)
	if b.nodeID != "" {
		// The transactions of the branches' forks alone, as the cluster's
		// placements.
		if transactions, txErr := b.transactionCatalog(); txErr == nil {
			for _, branch := range branches {
				id := branch.DistributedForkTransactionID
				if _, loaded := volatileTransactions[id]; id == "" || loaded {
					continue
				}
				if transaction, loadErr := transactions.Load(id); loadErr == nil {
					volatileTransactions[id] = transaction.SourceNeeded
				}
			}
		}
	}
	for _, branch := range branches {
		logicalID := logicalBranchID(branch.ID)
		account := BranchResourceStats{
			Target: result.Container + "@" + logicalID, State: normalizeLifecycleState(branch.State),
			NodeID: info.BranchNodeID(branch.ID), Quarantined: branch.Quarantined,
			StorageState: branch.StorageState, PublicationID: branch.PublicationID,
			StorageError: branch.StorageError, StorageRetryAt: branch.StorageRetryAt,
			SnapshotID: branch.SnapshotID, MemoryRoots: append([]string(nil), branch.MemoryRoots...),
			MemoryRootObjects: append([]string(nil), branch.MemoryRootObjects...), FilesystemRoot: branch.StorageManifest,
			ControlRoot: branch.ControlRoot, ControlRootObject: branch.ControlRootObject,
			SourceNode:           branch.SourceNode,
			DependentMemoryBytes: branch.DependentMemoryBytes, DependentFilesystemBytes: branch.DependentFilesystemBytes,
		}
		result.DependentMemoryBytes += branch.DependentMemoryBytes
		result.DependentFilesystemBytes += branch.DependentFilesystemBytes
		result.Branches = append(result.Branches, account)
		byID[logicalID] = &result.Branches[len(result.Branches)-1]
		// A remotely backed filesystem deliberately keeps its deferred catalog
		// and provider connection inside the live Sentry. The manager-side
		// repository handle has neither, and traversing it would either fail or
		// turn an observation into an eager independence barrier. Report the
		// already persisted dependent byte count until the Sentry completes the
		// drain; the next snapshot can then account exact private/shared local
		// object identity without touching the provider.
		// A dependency can consist entirely of filesystem metadata and
		// therefore account for zero payload bytes. SourceNeeded is the
		// authoritative lifetime fence; using the byte counter as a boolean
		// briefly traversed an incompletely attached deferred catalog directly
		// after remote fork.
		if deferFilesystemAccounting(branch, volatileTransactions) {
			continue
		}
		filesystemBranch := branch.FilesystemBranch
		if filesystemBranch == "" {
			filesystemBranch = offlineCopyBranch(info, branch.ID)
		}
		filesystemBranches[logicalID] = filesystemBranch
	}
	if b.snapshotGraph != nil {
		graph := b.snapshotGraph.Stats()
		result.SnapshotGraph = &SnapshotGraphResourceStats{
			Sequence: graph.Sequence, Snapshots: graph.Snapshots, Objects: graph.Objects,
			Roots: graph.Roots, Pins: graph.Pins, Reclaiming: graph.Reclaiming,
			JournalBytes: b.snapshotGraph.JournalBytes(),
		}
		for _, branch := range branches {
			if branch.DistributedGraphReferenceOperation != "" {
				result.SnapshotGraph.PendingReferenceOperations++
			}
		}
	}

	spec, err := b.containerStore().LoadSpecOf(info)
	if err != nil {
		return FamilyResourceStats{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return FamilyResourceStats{}, err
	}
	for _, config := range repositories {
		if len(filesystemBranches) == 0 {
			break
		}
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
		visibleByFilesystem := make(map[string][]string)
		for visible, filesystemBranch := range filesystemBranches {
			physicalBranch := repositoryPhysicalBranch(config, filesystemBranch)
			visibleByFilesystem[physicalBranch] = append(visibleByFilesystem[physicalBranch], visible)
			if _, ok := seenFilesystemIDs[physicalBranch]; ok {
				continue
			}
			seenFilesystemIDs[physicalBranch] = struct{}{}
			filesystemIDs = append(filesystemIDs, physicalBranch)
		}
		if b.snapshotShared != nil {
			for _, physicalBranch := range filesystemIDs {
				if err := repository.AttachImmutableMetadataSource(physicalBranch, b.snapshotShared.Object); err != nil {
					repository.Close()
					return FamilyResourceStats{}, fmt.Errorf("attaching shared filesystem metadata for %s: %w", physicalBranch, err)
				}
			}
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
		for _, branchUsage := range usage.Branches {
			for _, visible := range visibleByFilesystem[branchUsage.ID] {
				account := byID[visible]
				addResourceBytes(&account.Filesystem, branchUsage.PrivateBytes, branchUsage.SharedBytes)
				account.FilesystemInodes += branchUsage.Inodes
			}
		}
	}

	graphMemory := make(map[string]map[ttcsnapshot.ObjectID]uint64)
	objectBranches := make(map[ttcsnapshot.ObjectID]uint64)
	objectSizes := make(map[ttcsnapshot.ObjectID]uint64)
	familyReachableObjects := make(map[ttcsnapshot.ObjectID]struct{})
	for _, branch := range branches {
		logicalID := logicalBranchID(branch.ID)
		if len(branch.MemoryRootObjects) == 0 {
			continue
		}
		if err := validateGraphRootObjects(branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects); err != nil {
			return FamilyResourceStats{}, fmt.Errorf("accounting %s@%s memory roots: %w", result.Container, branch.ID, err)
		}
		roots := make([]ttcsnapshot.ObjectID, len(branch.MemoryRootObjects))
		allLocal := b.snapshotGraph != nil
		for index, encoded := range branch.MemoryRootObjects {
			id, err := ttcsnapshot.ParseObjectID(encoded)
			if err != nil {
				return FamilyResourceStats{}, err
			}
			roots[index] = id
			if allLocal {
				_, allLocal = b.snapshotGraph.Object(id)
			}
		}
		var catalog ttcsnapshot.ObjectCatalog
		if allLocal {
			catalog = b.snapshotGraph
		} else {
			immutable, err := b.immutableSnapshotCatalog(context.Background(), roots, "")
			if err != nil {
				return FamilyResourceStats{}, fmt.Errorf("resolving immutable memory catalog for %s@%s: %w", result.Container, branch.ID, err)
			}
			catalog = immutable
		}
		objects, err := ttcsnapshot.ReachableBulkObjects(catalog, roots)
		if err != nil {
			return FamilyResourceStats{}, fmt.Errorf("traversing memory roots for %s@%s: %w", result.Container, branch.ID, err)
		}
		memoryObjects := make(map[ttcsnapshot.ObjectID]uint64)
		for id, size := range objects {
			if id.Kind != ttcsnapshot.ObjectKindMemoryChunk {
				return FamilyResourceStats{}, fmt.Errorf("memory root for %s@%s reaches non-memory bulk object %s", result.Container, branch.ID, id)
			}
			memoryObjects[id] = size
			objectBranches[id]++
			objectSizes[id] = size
		}
		graphMemory[logicalID] = memoryObjects
		if result.SnapshotGraph != nil {
			reachable, err := ttcsnapshot.ReachableObjects(catalog, roots)
			if err != nil {
				return FamilyResourceStats{}, fmt.Errorf("traversing graph roots for %s@%s: %w", result.Container, branch.ID, err)
			}
			for _, object := range reachable {
				familyReachableObjects[object.ID] = struct{}{}
			}
		}
	}
	if result.SnapshotGraph != nil {
		result.SnapshotGraph.FamilyReachableObjects = len(familyReachableObjects)
	}
	if len(graphMemory) != 0 {
		for branch, objects := range graphMemory {
			account := byID[branch]
			for id, size := range objects {
				if objectBranches[id] > 1 {
					account.MemoryBacking.SharedBytes += size
				} else {
					account.MemoryBacking.PrivateBytes += size
				}
			}
			account.MemoryBacking.TotalBytes = account.MemoryBacking.PrivateBytes + account.MemoryBacking.SharedBytes
		}
		for _, size := range objectSizes {
			result.MemoryBackingUniqueBytes += size
		}
	}

	if mapped, huge, ok := b.applicationHugePages(resolved.container); ok {
		result.ApplicationMappedBytes, result.ApplicationHugePageBytes = mapped, huge
	}
	output, residentErr := b.command("timetravel", resolved.container, "stats", resolved.branch)
	if residentErr == nil {
		var resident boot.FamilyResidentMemory
		if err := json.Unmarshal(output, &resident); err != nil {
			return FamilyResourceStats{}, fmt.Errorf("decoding resident memory accounting: %w", err)
		}
		result.ResidentMemoryUniqueBytes = resident.UniqueBytes
		for _, branchMemory := range resident.Branches {
			branchID := branchMemory.ID
			if logical := physicalToLogical[branchID]; logical != "" {
				branchID = logical
			}
			if account := byID[branchID]; account != nil {
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
	if usage, present, cacheErr := b.containerStore().ImageCacheUsage(info.Digest, info.Platform); cacheErr != nil {
		return FamilyResourceStats{}, cacheErr
	} else if present {
		result.ImageCacheBytes = usage.CompleteBytes
		result.ImageCacheTotalBytes = usage.TotalBytes
	}
	return result, nil
}

func deferFilesystemAccounting(branch ttcbundle.BranchInfo, volatileTransactions map[string]bool) bool {
	return branch.DependentFilesystemBytes != 0 || volatileTransactions[branch.DistributedForkTransactionID]
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
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return 0, err
	}
	target = resolved
	// A completed branch remains waitable after its runtime and console helper
	// are discarded. Avoid invoking runsc for a process-free cold branch: doing
	// so both obscures the real status and produces a misleading FetchSpec error.
	if branch, branchErr := persistentBranchInfo(info, target.branch); branchErr == nil && normalizeLifecycleState(branch.State) == "exited" {
		var status uint32
		if branch.ExitStatus != nil {
			status = uint32(*branch.ExitStatus) << 8
			if b.publicationQueue != nil && branch.PublicationID == "" && branch.StorageState != "shared" {
				if err := b.completeWaitedInitExit(target, status); err != nil {
					return status, err
				}
			}
			return status, nil
		}
		// Compatibility for records written earlier in the same root format,
		// before exits became branch-local. This is sound only when the whole
		// family is terminal.
		if normalizeLifecycleState(info.State) == "exited" && info.ExitStatus != nil {
			return uint32(*info.ExitStatus) << 8, nil
		}
	}
	output, commandErr := b.command("timetravel", target.container, "wait", target.branch)
	if commandErr == nil {
		var result struct {
			Status uint32 `json:"wait_status"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return 0, fmt.Errorf("decoding runsc wait result: %w", err)
		}
		if err := b.completeWaitedInitExit(target, result.Status); err != nil {
			return result.Status, err
		}
		return result.Status, nil
	}

	status, found, err := b.waitForConsoleStatus(target)
	if err != nil {
		return 0, fmt.Errorf("%w; checking persistent wait status: %v", commandErr, err)
	}
	if found {
		if err := b.completeWaitedInitExit(target, status); err != nil {
			return status, err
		}
		return status, nil
	}
	if status, lost := b.lostRuntimeWaitStatus(target); lost {
		return status, nil
	}
	return 0, commandErr
}

// completeWaitedInitExit closes the ordering gap between wait observing PID 1
// and the asynchronous console follower finalizing the branch. Once wait
// returns, a subsequent start must see a process-free lifecycle rather than a
// stale running placement. The wait status is immutable, so racing atomic
// installations by the follower and this synchronous path are equivalent.
func (b *runtimeBackend) completeWaitedInitExit(target selector, status uint32) error {
	b.recordWaitState(target, status)
	if stops := b.explicitInitStops; stops != nil && stops.Load() != 0 {
		return nil
	}
	// Standalone start already consumes the durable exited state directly and
	// has no asynchronous placement release to race. Preserve its lightweight
	// wait path; publicationQueue is present only on clustered compute daemons.
	if b.publicationQueue == nil {
		return nil
	}
	// Waiting for PID 1 itself must not hold the lifecycle lock, but the small
	// terminal transition after it exits must serialize with the console
	// follower and foreground lifecycle commands. In particular, the follower
	// may record the exited status just before wait enters its fast path; only
	// one of them should seal and publish the process-free branch.
	unlock := func() {}
	if !b.lifecycleLockHeld {
		unlock = b.holdFamilyLifecycle(b.familyID(target.container))
	}
	defer unlock()
	info, err := b.containerStore().LoadContainer(target.container)
	if err != nil {
		return err
	}
	paths, err := b.containerStore().BranchConsolePathsIn(info, target.branch)
	if err != nil {
		return err
	}
	if err := ttcconsole.WriteWaitStatus(paths.WaitStatus, status); err != nil {
		return err
	}
	_, err = b.finalizeObservedInitExit(target)
	return err
}

func (b *runtimeBackend) recordWaitState(target selector, status uint32) {
	store := b.containerStore()
	oomKilled := false
	if info, err := store.LoadContainer(target.container); err == nil {
		if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: stopping published ports for exited %s: %v\n", target, err)
		}
		// The Sentry, or the host's out-of-memory killer, may kill the
		// process that ran the family out of memory rather than the family's
		// runtime: an init killed so is reported as Docker reports a
		// container's.
		ws := syscall.WaitStatus(status)
		oomKilled = ws.Signaled() && ws.Signal() == syscall.SIGKILL &&
			(status&ttcconsole.OOMKilledStatus != 0 || runtimeCgroupOOMKilled(info.BundleDir))
	}
	exitStatus := waitStatusExitCode(status)
	if err := store.RecordBranchExit(target.container, target.branch, exitStatus, oomKilled); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: warning: recording branch exit state for %s: %v\n", target, err)
	}
}

// finalizeObservedInitExit converts a detached init's completed wait status
// into the same process-free, persistable state as a foreground exit. The
// console follower writes WaitStatus only after runsc has observed init
// termination and drained its output, so its presence is the durable local
// proof that this is a normal branch exit rather than unexplained runtime
// loss.
func (b *runtimeBackend) finalizeObservedInitExit(target selector) (bool, error) {
	// Console followers are attached to an exact physical runtime. Prefer that
	// record over logical placement resolution: a daemon recovery scan sees
	// every shared physical record, including committed cold publications whose
	// derived metadata may still be settling. Resolving such a record as a
	// logical selector can wait for the publication fence while holding the
	// daemon-wide reconciliation lock, blocking every client request.
	info, err := b.containerStore().LoadRuntimeContainer(target.container)
	branch, branchErr := persistentBranchInfo(info, target.branch)
	if err != nil || branchErr != nil {
		resolved, resolvedInfo, resolveErr := b.resolveSelector(target)
		if resolveErr != nil {
			return false, resolveErr
		}
		target, info = resolved, resolvedInfo
		if branch, err = persistentBranchInfo(info, target.branch); err != nil {
			// The branch was removed, as a remote fork's target is with its
			// runtime, whose followers then exit: there is no exit to record.
			return false, nil
		}
	}
	switch normalizeLifecycleState(branch.State) {
	case "running", "paused", "restarting", "rehydrating", "exited":
	default:
		return false, nil
	}
	if branch.PublicationID != "" || branch.StorageState == "publishing" || branch.StorageState == "shared" {
		return false, nil
	}
	paths, err := b.containerStore().BranchConsolePathsIn(info, target.branch)
	if err != nil {
		return false, err
	}
	status, found, err := readConsoleWaitStatus(paths.WaitStatus)
	if err != nil || !found {
		return false, err
	}
	// Detached --rm has its own durable daemon request, which waits for this
	// same status before removing the branch. Do not race it by publishing a
	// filesystem generation that is intended to be discarded.
	if info.AutoRemove {
		return false, nil
	}
	b.recordWaitState(target, status)
	if err := b.discard(target); err != nil {
		return false, fmt.Errorf("releasing naturally exited branch %s: %w", target, err)
	}
	if err := b.queueBranchPublication(target, "stop"); err != nil {
		return false, fmt.Errorf("queueing naturally exited branch %s for publication: %w", target, err)
	}
	return true, nil
}

// reconcileObservedInitExits retries exit finalization after daemon restart or
// a failure between the console follower's durable wait-status write and the
// asynchronous publication intent. It intentionally scans physical records:
// remote forks and migrations may be materialized under hidden IDs even though
// their user-facing identity is a logical branch in the authority.
func (b *runtimeBackend) reconcileObservedInitExits() error {
	if b == nil || b.dataRoot == "" {
		return nil
	}
	containers, err := b.containerStore().ListContainersForPlacement()
	if err != nil {
		return err
	}
	var result error
	for _, info := range containers {
		// A long operation working on the family finishes what it changes.
		if _, busy := b.operations.lookup(info.ID); busy {
			continue
		}
		for _, branch := range info.Branches {
			// Recovery is node-local. Foreign runtimes and process-free cold
			// publications are visible in the shared catalog but cannot have a
			// console follower owned by this daemon. Filtering them before selector
			// resolution prevents an unrelated incomplete publication from stalling
			// local command admission.
			if owner := info.BranchNodeID(branch.ID); b.nodeID != "" && owner != "" && owner != b.nodeID {
				continue
			}
			if branch.PublicationID != "" || branch.StorageState == "publishing" || branch.StorageState == "shared" {
				continue
			}
			if _, err := b.finalizeObservedInitExit(selector{container: info.ID, branch: branch.ID}); err != nil {
				result = errors.Join(result, err)
			}
		}
	}
	return result
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
