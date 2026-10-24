// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package client implements the root-unaware TTC command-line client.
package client

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
	"gvisor.dev/gvisor/runsc/version"
)

// daemonStartTimeout includes cluster admission and crash recovery. New TTC
// roots keep journals bounded, but a daemon upgraded from a build containing
// the old unbounded-WAL bug may need several minutes to replay and compact its
// final large journal once. Killing it at the ordinary topology timeout makes
// that otherwise recoverable root impossible to open through the CLI.
const daemonStartTimeout = 10 * time.Minute

const (
	daemonAPIVersion      = protocol.DaemonAPIVersion
	daemonFrameStdin      = byte('I')
	daemonFrameStdinAck   = byte('A')
	daemonFrameEOF        = byte('C')
	daemonFrameResize     = byte('W')
	daemonFrameStdout     = byte('O')
	daemonFrameStderr     = byte('E')
	daemonFrameStatus     = byte('X')
	daemonFrameTerminal   = byte('T')
	daemonFrameTTYState   = byte('S')
	daemonMaxFrame        = 1 << 20
	daemonStdinWindow     = 1 << 20
	daemonTerminalSizeLen = 8
)

type options struct {
	socket string
	// requestID names a mutating command, so that ttcd runs it at most once.
	requestID string
}

type request struct {
	Version         int      `json:"version"`
	ClientBuildID   string   `json:"client_build_id,omitempty"`
	Action          string   `json:"action"`
	RequestID       string   `json:"request_id,omitempty"`
	ClientTERM      string   `json:"client_term,omitempty"`
	StderrTerminal  bool     `json:"stderr_terminal,omitempty"`
	StderrWidth     int      `json:"stderr_width,omitempty"`
	Args            []string `json:"args,omitempty"`
	Stdin           bool     `json:"stdin,omitempty"`
	ArchiveFile     bool     `json:"archive_file,omitempty"`
	TerminalControl bool     `json:"terminal_control,omitempty"`
	ManageStop      bool     `json:"manage_stop,omitempty"`
}

// Run executes the thin ttc client and returns a process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, args, done, err := parseGlobal(args, stdout)
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
	if handled, err := localHelp(args, stdout); handled {
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		return 0
	}
	if args[0] == "completion" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, "ttc: usage: ttc completion bash|zsh|fish")
			return 2
		}
		if err := writeCompletion(args[1], stdout); err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		return 0
	}
	if args[0] != "daemon" && !knownCommand(args[0]) {
		fmt.Fprintf(stderr, "ttc: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	if opts.socket == "" {
		root, err := protocol.DaemonRoot(os.Getenv)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		opts.socket = protocol.DaemonSocket(root)
	} else if len(args) >= 2 && args[0] == "daemon" && args[1] == "start" {
		fmt.Fprintln(stderr, "ttc: --daemon-socket does not apply to daemon start: a daemon's socket is in its root, TTC_ROOT or TTC_LOCAL_ROOT")
		return 2
	}
	if args[0] == "daemon" {
		if opts.requestID != "" {
			fmt.Fprintln(stderr, "ttc: --request-id does not apply to daemon commands")
			return 2
		}
		return runDaemon(opts, args[1:], stdout, stderr)
	}
	args, stdin, stdout, archive, closeArchive, err := prepareImageArchiveIO(args, stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 2
	}
	args, stdin, stdout, checkpoint, closeCheckpoint, err := prepareCheckpointArchiveIO(args, stdin, stdout)
	if err != nil {
		_ = closeArchive()
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 2
	}
	if archive == nil {
		archive = checkpoint
	}
	args = normalizeEnvironmentImports(args)
	args, err = normalizeHostPaths(args)
	if err == nil {
		args, err = readKeyValueFileOptions(args)
	}
	// ttcd opens no host path for a member of the group a daemon is shared
	// with: ttc copies a member's files itself.
	var finishCopy func(int) int
	if err == nil && sharedDaemonMember(opts.socket) {
		args, stdin, stdout, finishCopy, err = prepareMemberCopy(args, stdin, stdout, stderr)
	}
	if err != nil {
		_ = closeCheckpoint()
		_ = closeArchive()
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 2
	}
	if opts.requestID != "" && (!commandCanRetry(args) || commandUsesStdin(args) && archive == nil) {
		_ = closeCheckpoint()
		_ = closeArchive()
		fmt.Fprintf(stderr, "ttc: --request-id applies to a command that ttcd can answer again from its record, not to %q\n", args[0])
		return 2
	}
	status, err := runCommand(opts.socket, args, stdin, stdout, stderr, archive, opts.requestID)
	closeErr := errors.Join(closeCheckpoint(), closeArchive())
	if err != nil {
		fmt.Fprintf(stderr, "ttc: daemon transport: %v\n", err)
		return 1
	}
	if finishCopy != nil {
		status = finishCopy(status)
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "ttc: closing image archive: %v\n", closeErr)
		return 1
	}
	return status
}

// prepareCheckpointArchiveIO keeps checkpoint and restore operands on the
// client host: the daemon never opens a client's path, which is essential when
// a login-node client routes work to a compute node with an unrelated
// filesystem namespace. A regular file is opened here and passed to the
// daemon, which writes or reads it in place, and streams it on to a node it
// routes the command to; anything else is streamed as standard output/input.
func prepareCheckpointArchiveIO(args []string, stdin io.Reader, stdout io.Writer) ([]string, io.Reader, io.Writer, *os.File, func() error, error) {
	result := append([]string(nil), args...)
	closeArchive := func() error { return nil }
	if len(result) == 0 {
		return result, stdin, stdout, nil, closeArchive, nil
	}
	switch result[0] {
	case "checkpoint":
		if len(result) < 3 || result[len(result)-1] == "-" {
			return result, stdin, stdout, nil, closeArchive, nil
		}
		path := result[len(result)-1]
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("opening checkpoint output: %w", err)
		}
		result[len(result)-1] = "-"
		if regularFile(file) {
			return result, stdin, stdout, file, file.Close, nil
		}
		return result, stdin, file, nil, file.Close, nil
	case "restore":
		if len(result) < 2 || result[len(result)-1] == "-" {
			return result, stdin, stdout, nil, closeArchive, nil
		}
		path := result[len(result)-1]
		file, err := os.Open(path)
		if err != nil {
			return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("opening checkpoint input: %w", err)
		}
		result[len(result)-1] = "-"
		if regularFile(file) {
			return result, stdin, stdout, file, file.Close, nil
		}
		return result, file, stdout, nil, file.Close, nil
	default:
		return result, stdin, stdout, nil, closeArchive, nil
	}
}

// regularFile reports whether file is a regular file.
func regularFile(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode().IsRegular()
}

// prepareImageArchiveIO implements Docker's client-local -i/-o semantics.
// The daemon must never interpret a CLI path in its own mount namespace,
// especially through a control-only gateway whose filesystem differs from
// the user's login node: a regular file is opened here and passed to it, as
// prepareCheckpointArchiveIO passes one, and anything else is streamed.
func prepareImageArchiveIO(args []string, stdin io.Reader, stdout io.Writer) ([]string, io.Reader, io.Writer, *os.File, func() error, error) {
	result := append([]string(nil), args...)
	command, start := result[0], 1
	if command == "image" && len(result) > 1 {
		command, start = result[1], 2
	}
	closeArchive := func() error { return nil }
	var short, long string
	switch command {
	case "load":
		short, long = "-i", "--input"
	case "save":
		short, long = "-o", "--output"
	default:
		return result, stdin, stdout, nil, closeArchive, nil
	}
	option := -1
	value := ""
	consume := 1
	for i := start; i < len(result); i++ {
		arg := result[i]
		switch {
		case arg == short || arg == long:
			if i+1 >= len(result) {
				return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("%s requires a path", arg)
			}
			option, value, consume = i, result[i+1], 2
		case strings.HasPrefix(arg, short+"="):
			option, value = i, strings.TrimPrefix(arg, short+"=")
		case strings.HasPrefix(arg, long+"="):
			option, value = i, strings.TrimPrefix(arg, long+"=")
		}
		if option >= 0 {
			break
		}
	}
	if option < 0 {
		return result, stdin, stdout, nil, closeArchive, nil
	}
	result = append(result[:option], result[option+consume:]...)
	if value == "" {
		return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("%s requires a path", long)
	}
	if value == "-" {
		return result, stdin, stdout, nil, closeArchive, nil
	}
	if command == "load" {
		file, err := os.Open(value)
		if err != nil {
			return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("opening image archive: %w", err)
		}
		if regularFile(file) {
			return result, stdin, stdout, file, file.Close, nil
		}
		return result, file, stdout, nil, file.Close, nil
	}
	file, err := os.OpenFile(value, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, stdin, stdout, nil, closeArchive, fmt.Errorf("opening image archive output: %w", err)
	}
	if regularFile(file) {
		return result, stdin, stdout, file, file.Close, nil
	}
	return result, stdin, file, nil, file.Close, nil
}

func parseGlobal(args []string, stdout io.Writer) (options, []string, bool, error) {
	var opts options
	for len(args) != 0 {
		switch arg := args[0]; {
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
			opts.socket, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--daemon-socket="):
			opts.socket, args = strings.TrimPrefix(arg, "--daemon-socket="), args[1:]
			if opts.socket == "" {
				return opts, nil, false, errors.New("--daemon-socket requires a path")
			}
		case arg == "--request-id" || strings.HasPrefix(arg, "--request-id="):
			if value, ok := strings.CutPrefix(arg, "--request-id="); ok {
				opts.requestID, args = value, args[1:]
			} else if len(args) >= 2 {
				opts.requestID, args = args[1], args[2:]
			} else {
				args = args[1:]
			}
			if id, err := hex.DecodeString(opts.requestID); err != nil || len(id) != 16 {
				return opts, nil, false, errors.New("--request-id requires 32 hexadecimal digits")
			}
		case strings.HasPrefix(arg, "-"):
			return opts, nil, false, fmt.Errorf("unknown global option %q", arg)
		default:
			return opts, args, false, nil
		}
	}
	return opts, args, false, nil
}

var commands = map[string]struct{}{
	"attach": {}, "checkpoint": {}, "commit": {}, "container": {}, "cp": {}, "create": {},
	"dehydrate": {}, "diff": {}, "events": {}, "exec": {}, "fork": {}, "image": {}, "images": {},
	"inspect": {}, "kill": {}, "load": {}, "logs": {}, "ls": {}, "migrate": {}, "node": {},
	"pause": {}, "port": {}, "ps": {}, "pull": {}, "quarantine": {}, "rehydrate": {}, "rename": {}, "restart": {},
	"restore": {}, "resume": {}, "rm": {}, "rmi": {}, "run": {}, "save": {}, "start": {},
	"stats": {}, "stop": {}, "top": {}, "tty": {}, "unpause": {}, "unquarantine": {}, "volume": {}, "wait": {},
}

func knownCommand(command string) bool {
	_, ok := commands[command]
	return ok
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: ttc [--daemon-socket PATH] [--request-id ID] COMMAND

Manage TimeTravelContainer container families and live branches.

The client connects to the daemon whose root the environment names: through
$TTC_LOCAL_ROOT/ttcd.sock on a node of a cluster, and $TTC_ROOT/ttcd.sock
otherwise. Storage and runtime configuration belong exclusively to ttcd.

Commands:
  run, create, start, stop, restart, rm, exec, attach, logs, cp
  images, image, pull, load, save, commit, rmi
  ls, ps, inspect, top, stats, diff, events, wait
  fork, migrate, pause, resume, quarantine, unquarantine
  dehydrate, rehydrate, checkpoint, restore
  port, volume, node, tty, rename, kill
  daemon start|status|stop|requests|request
  completion bash|zsh|fish

Branch selectors use CONTAINER@BRANCH. SIZE values accept raw bytes or Docker
units such as 512MiB and 8GiB. DURATION values use forms such as 250ms, 15s,
and 2m. Defaults are stated in command-specific help.

Global options:
  --daemon-socket PATH   Unix socket of the daemon to use (default: ttcd.sock
                         in TTC_LOCAL_ROOT if set, and otherwise in TTC_ROOT)
  --request-id ID        Name a mutating command, by 32 hexadecimal digits:
                         ttcd runs a command of an ID once, and answers it
                         again with its first result (default: a new ID)

Run 'ttc COMMAND --help' for command usage. ttcd requires TTC_ROOT; clustered
compute daemons additionally require TTC_LOCAL_ROOT.
`)
}

var commandHelp = map[string]string{
	"daemon": `Usage: ttc daemon COMMAND [OPTIONS]

Manage the local ttcd service: the daemon whose root is TTC_LOCAL_ROOT on a
node of a cluster, and TTC_ROOT otherwise. The global --daemon-socket selects
another daemon's socket for every command but start.

Commands:
  start [OPTIONS]        Start ttcd and wait until it is ready
  status                 Show daemon build, mode, roots, and cluster state
  stop [POLICY]          Stop ttcd, optionally disposing of live branches
  requests [OPTIONS]     List durable mutation requests
  request inspect ID     Inspect an interrupted mutation
  request resolve ID     Record an operator-verified outcome

Run 'ttc daemon COMMAND --help' for command-specific usage.`,
	"daemon start": `Usage: ttc daemon start [OPTIONS]

Start the sibling ttcd binary and wait until it is ready. TTC_ROOT must name
an explicitly configured, new-format data root. A standalone daemon keeps all
of its files there, including its socket and log. A daemon of a cluster shares
TTC_ROOT with the cluster and keeps its node's own files in TTC_LOCAL_ROOT.
The daemon's root, TTC_ROOT or TTC_LOCAL_ROOT, must be on a local filesystem.

With --listen, --advertise, or --node, the daemon is a compute node of the
cluster whose shared root TTC_ROOT is. Every node starts alike, in any order:
it holds the cluster's authority, or joins the node that does.

Options:
  --platform systrap|kvm          Execution platform (default: systrap)
  --cpu-profile portable|host|explicit
                                  Guest CPU contract (default: portable)
  --cpu-features NAME,...         Required with explicit profile; no default
  --cpu-topology single|host      CPUs and memory nodes guests see: single,
                                  one socket and one node, their affinity
                                  and memory policies ignored; or host, the
                                  host's own, with affinity and memory
                                  policies applied to the host's CPUs and
                                  nodes (default: single; a cluster's node
                                  takes its cluster's)
  --unsafe-access-host-group GROUP
                                  Share the daemon with the members of a Unix
                                  group, by name or ID: its root becomes
                                  searchable and its socket usable by the
                                  group. A member can do through the daemon
                                  what its user can, but open host paths,
                                  stop it, restore checkpoints, or resolve
                                  requests (default: none)
  --experimental-no-cpuid-faulting
                                  Create a cluster whose systrap nodes need no
                                  host CPUID faulting. Requires --cpu-profile
                                  host; every node must then have identical
                                  CPUID feature registers, and systrap runs
                                  without syscall patching (default: false)
  --unsafe-no-host-isolation      Disable host isolation: user namespaces
                                  and chroots on Linux, the Seatbelt sandbox
                                  on macOS (default: false)
  --allow-network-filesystem-root Start although the daemon's root is on a
                                  network filesystem such as NFS or Lustre
                                  (default: false)
  --control-only                  Outbound-only login-node gateway
                                  (default: false)
  --node NAME                     Node name: [A-Za-z0-9._-]+ (default: hostname;
                                  control-only default: control-HOSTNAME)
  --listen IP:PORT                Cluster control listener
                                  (default: 0.0.0.0:7443)
  --advertise HOST:PORT           Peer-reachable address (default: hostname and
                                  the --listen port)
  --memory-target SIZE            Resident-memory placement target; SIZE is raw
                                  bytes or e.g. 512MiB/8GiB (default: disabled;
                                  an explicitly supplied value must be positive)
  --cluster-auto-rebalance        Relieve memory pressure automatically
                                  (default: false)
  --cluster-capacity-command PATH Trusted provisioner executable
                                  (default: disabled)
  --cluster-capacity-webhook URL  Trusted provisioner HTTP endpoint
                                  (default: disabled)

Advanced cluster policy:
  --cluster-lease DURATION        Execution lease, 1s..10m (default: 30s)
  --cluster-max-transactions N    Concurrent transaction bound (default: 4096)
  --cluster-transfer-bytes N      Preferred transfer bytes (default: 2097152)
  --cluster-max-outstanding-bytes N
                                  In-flight byte bound (default: 67108864)
  --cluster-max-parallel N        Parallel transfers (default: 4)
  --cluster-ack-interval DURATION ACK coalescing interval (default: 1ms)
  --cluster-ack-batch-pages N     ACK batch size in 4KiB pages (default: 256)
  --cluster-prefetch MODE         none|sequential|fixed-window|adaptive
                                  (default: none)
  --cluster-push MODE             demand-only|dirty-orphan|memory-pressure|
                                  eager-drain (default: demand-only)

Cluster membership, transport, and capacity options are fixed for the daemon
lifetime. Operational policy may change only while the cluster has no live
node leases. The CPU profile remains immutable.

Environment:
  TTC_ROOT          Required data root; shared root in cluster mode. A
                    standalone daemon keeps all of its files there
  TTC_LOCAL_ROOT    Required node-local root of a daemon of a cluster, which
                    keeps its node's files there

A cluster's daemons authenticate one another with a token the first of them
makes in TTC_ROOT, readable by the root's owner alone.

With --control-only, the daemon finds the cluster's compute nodes in TTC_ROOT.
It does not advertise a TCP endpoint, register a node, or accept container
placement; it opens only its socket in TTC_LOCAL_ROOT.`,
	"daemon status": `Usage: ttc daemon status

Show the running daemon's build ID, API version, roots, runtime platform, host
isolation, and cluster membership. The client and daemon must have exactly
matching builds.`,
	"daemon stop": `Usage: ttc daemon stop [--dehydrate-all|--stop-all|--migrate-all]

Stop the local daemon. With live branches, select exactly one policy:
  --dehydrate-all  Persist complete process state, then stop
  --stop-all       Terminate all local branches, then stop
  --migrate-all    Move local branches to other cluster nodes, then stop

This command remains available across client/daemon API or build mismatches.`,
	"daemon requests": `Usage: ttc daemon requests [--json] [--state pending|completed]

List durable mutation requests, including requests whose outcome needs manual
inspection after a client disconnect or daemon failure.`,
	"daemon request": `Usage: ttc daemon request inspect|resolve [OPTIONS] REQUEST_ID

Inspect an interrupted durable mutation or record its verified outcome.`,
	"daemon request inspect": `Usage: ttc daemon request inspect [--json] REQUEST_ID

Show the command, state, timestamps, result, and operator note for one durable
mutation request.`,
	"daemon request resolve": `Usage: ttc daemon request resolve --outcome OUTCOME [--note TEXT] REQUEST_ID

Record an operator-verified outcome without re-executing the mutation.
OUTCOME must be succeeded, failed, or abandoned.`,

	"image": `Usage: ttc image COMMAND [OPTIONS]

Manage Docker/OCI images.

Commands:
  ls                         List images (alias: images)
  pull IMAGE                 Pull an image from a registry
  load [OPTIONS]             Load images from a Docker-compatible tar archive
  save [OPTIONS] IMAGE...    Save images to a Docker-compatible tar archive
  inspect IMAGE...           Show image configuration and TTC metadata
  rm [OPTIONS] IMAGE...      Remove images (aliases: remove, rmi)

Run 'ttc image COMMAND --help' for command-specific usage.`,
	"image ls": `Usage: ttc image ls [-q|--quiet] [--no-trunc] [--json]

All options default to false. --quiet prints IDs only; --no-trunc prints full
digests; --json emits stable machine-readable records.`,
	"image list": `Usage: ttc image list [-q|--quiet] [--no-trunc] [--json]

Alias for 'ttc image ls'.`,
	"image pull": `Usage: ttc image pull [--platform OS/ARCH] [-q|--quiet] IMAGE

The registry defaults to Docker Hub and an omitted tag defaults to latest.
--platform defaults to the daemon's native OCI platform; --quiet defaults to
false and prints only the pulled digest when enabled.`,
	"image load": `Usage: ttc image load [-i|--input FILE] [--platform OS/ARCH] [-q|--quiet]

Read a Docker-compatible tar from FILE (default: standard input). Platform
defaults to all compatible archive content; --quiet defaults to false.`,
	"image save": `Usage: ttc image save [-o|--output FILE] [--platform OS/ARCH] IMAGE [IMAGE...]

Write a Docker-compatible tar to FILE (default: standard output). Platform
defaults to each image's stored platform.`,
	"image inspect": "Usage: ttc image inspect IMAGE [IMAGE...]\n\nPrint Docker-compatible JSON metadata for each image.",
	"image rm": `Usage: ttc image rm [-f|--force] IMAGE [IMAGE...]

--force defaults to false and is required when a container still references
the image.`,
	"image remove": "Usage: ttc image remove [-f|--force] IMAGE [IMAGE...]\n\nAlias for 'ttc image rm'.",

	"container": `Usage: ttc container COMMAND [OPTIONS]

Use Docker-style container command aliases. Branch-taking commands require a
full CONTAINER@BRANCH selector.

Commands:
  run, create, start, stop, restart, rm
  exec, attach, logs, cp, inspect, ls, ps, top
  pause, unpause, kill, wait

The same commands are also available without the "container" prefix.`,

	"exec": `Usage: ttc exec [OPTIONS] CONTAINER@BRANCH COMMAND [ARG...]
       ttc exec [OPTIONS] --process FILE CONTAINER@BRANCH
       ttc exec ls|wait|kill [OPTIONS]

Execute a command in a running branch. A named detached session remains
reattachable after the client disconnects and retains output in its ring
buffer.

Common options:
  -d, --detach               Run detached (default: false)
  -i, --interactive          Keep standard input open (default: false)
  -t, --tty                  Allocate a terminal (default: false)
  -e, --env NAME=VALUE       Set an environment variable; repeatable
  -u, --user USER[:GROUP]    Guest name/UID and optional group/GID
                             (default: container user)
  -w, --workdir DIR          Guest working directory (default: container cwd)
  --session NAME             Durable branch-local session name (default: none)
  --process FILE             OCI process JSON; replaces command overrides`,
	"exec ls":   "Usage: ttc exec ls [--json] CONTAINER@BRANCH",
	"exec wait": "Usage: ttc exec wait CONTAINER@BRANCH:EXEC_ID",
	"exec kill": "Usage: ttc exec kill [--signal SIGNAL] CONTAINER@BRANCH:EXEC_ID\n\nSIGNAL accepts a name such as TERM or SIGKILL, or a number (default: KILL).",

	"port": `Usage: ttc port COMMAND [OPTIONS]

Manage user-mode TCP forwarding for existing branches.

Commands:
  publish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]
  ls [--json] CONTAINER@BRANCH
  unpublish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]`,
	"port publish":   "Usage: ttc port publish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",
	"port ls":        "Usage: ttc port ls [--json] CONTAINER@BRANCH",
	"port unpublish": "Usage: ttc port unpublish TARGET [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]",

	"volume": `Usage: ttc volume COMMAND [OPTIONS]

Manage TTC-owned persistent volumes.

Commands:
  create NAME          Create a volume
  ls                   List volumes
  inspect NAME...      Inspect volumes
  rm [-f] NAME...      Remove volumes (alias: remove)`,
	"volume create":  "Usage: ttc volume create NAME\n\nCreate an empty TTC-managed persistent volume.",
	"volume ls":      "Usage: ttc volume ls [--json]\n\nList all managed volumes (alias: volume list); --json prints them as a JSON\narray of their names, drivers, and in a cluster nodes.",
	"volume list":    "Usage: ttc volume list",
	"volume inspect": "Usage: ttc volume inspect NAME [NAME...]",
	"volume rm":      "Usage: ttc volume rm [-f|--force] NAME [NAME...]\n\n--force removes referenced volumes and defaults to false.",
	"volume remove":  "Usage: ttc volume remove [-f|--force] NAME [NAME...]",

	"node": `Usage: ttc node COMMAND [OPTIONS]

Inspect and manage nodes in a distributed TTC cluster.

Commands:
  ls [--json]                List nodes, load, capacity, state, and lease
  rm NODE                    Forget a fenced, offline, expired node identity
  stop NODE [POLICY]         Evacuate or dispose of workloads and stop a node
  rebalance NODE             Relieve a node's configured memory pressure

Node stop policies are --migrate-all, --dehydrate-all, and --stop-all. With no
policy, stop rejects a node that still owns branches. 'node rm' never fences a
live daemon; it succeeds only after lease expiry/offline fencing and after all
placements, volatile snapshots, transactions, reservations, and GC authority
have been released.`,
	"node ls": `Usage: ttc node ls [--json]

Options:
  --json   Emit complete NodeRecord objects (default: human-readable table)`,
	"node rm": `Usage: ttc node rm NODE

Permanently remove an already-offline node from cluster discovery. A later
join using the same node name is a new incarnation. There is no force option:
live nodes and nodes that still provide authoritative state are rejected.`,
	"node stop": `Usage: ttc node stop NODE [--stop-all|--dehydrate-all|--migrate-all]

Stop NODE after applying at most one branch policy (default: reject if work
remains). --migrate-all lets the scheduler choose destinations.`,
	"node rebalance": `Usage: ttc node rebalance NODE

Move a pressure-relieving subset of branches away from NODE. Destinations are
selected by the cluster scheduler; the resulting plan is printed as JSON.`,

	"tty": `Usage: ttc tty COMMAND CONTAINER@BRANCH --session NAME [--json]

Observe named exec-session terminal state without attaching to it.

Commands:
  stat     Print one terminal observation
  watch    Stream coalesced terminal changes`,
	"tty stat":  "Usage: ttc tty stat CONTAINER@BRANCH --session NAME [--json]",
	"tty watch": "Usage: ttc tty watch CONTAINER@BRANCH --session NAME [--json]",

	"run": `Usage: ttc run [--node NODE] [OPTIONS] IMAGE [COMMAND] [ARG...]

Create and start a container. In cluster mode, omit --node to let the scheduler
choose a compute node. Important options:
  --name NAME                 Container family name (default: generated ID)
  --pull missing|always|never Image pull policy (default: missing)
  -d, --detach                Run in background (default: false)
  -i, --interactive           Keep standard input open (default: false)
  -t, --tty                   Allocate a terminal (default: false)
  --rm                        Remove after exit (default: false)
  --read-only                 Read-only rootfs (default: false)
  --stop-signal SIGNAL        Stop signal (default: image STOPSIGNAL, then TERM)
  --stop-timeout SECONDS      Grace period; -1 waits forever (default: 10)
  --pids-limit N              Maximum tasks; -1 or 0 is unlimited (default: 0)
  -m, --memory SIZE           Memory limit; 0 is unlimited (default: 0)
  --memory-swap SIZE          Memory plus swap limit; -1 is unlimited swap
                              (default: twice --memory)
  --cpus DECIMAL              CPU quota, e.g. 0.5 or 2 (default: 0, unlimited)
  --shm-size SIZE             Size of memory-backed /dev/shm (default: 64m)
  --network none|host|ttc     Network mode (default: none)
  --nat                       Permit outbound NAT for --network=ttc (default: false)
  --ip IPv4                   Virtual NIC address (default: 10.0.2.15)
  --mac-address MAC           Virtual NIC address (default: 02:42:0a:00:02:0f)
  -p, --publish SPEC          [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp]; repeatable
  -v, --volume SPEC           VOLUME:PATH[:ro] or HOST:PATH[:ro|rw|copy|lazy_copy];
                              repeatable. HOST:PATH alone, or :rw, binds the host
                              path writable, which needs --waive-live-fork
  --tmpfs PATH[:OPTIONS]      Add an explicitly memory-backed mount; repeatable
  -e, --env NAME[=VALUE]      Set/import an environment variable; repeatable
  -u, --user USER[:GROUP]     Guest user (default: image user)
  -w, --workdir DIR           Guest cwd (default: image working directory)
  --hostname NAME             Guest hostname (default: container name)
  --log-driver DRIVER         local|json-file|none (default: local)
  --gfx                       Virtual Vulkan GPU, /dev/dri/renderD128 (default: false)
  --nvproxy[=CAPS]            The host's NVIDIA GPUs, for CUDA, on Linux
                              (default: none)
  --gfx-fork, --nvproxy-fork device-lost|snapshot
                              What a live fork does with the GPU while it is in
                              use; without one, the family must waive live forks
  --waive-live-fork           Permit a non-forkable configuration (default: false)`,
	"create": `Usage: ttc create [OPTIONS] IMAGE [COMMAND] [ARG...]

Create but do not start a container. It accepts the same configuration options
as 'ttc run' except --detach and --node. Defaults include --pull=missing,
--network=none, --shm-size=64m, --stop-timeout=10, and --log-driver=local.`,
	"start": `Usage: ttc start [--node NODE] [-a|--attach] [-i|--interactive] CONTAINER@BRANCH...

Start an exited branch. In cluster mode, omit --node to let the scheduler pick
a compute node. --attach defaults to false; --interactive requires --attach,
and attaching accepts exactly one branch.`,
	"stop": `Usage: ttc stop [-t|--time|--timeout SECONDS] TARGET...

TARGET may be a family or CONTAINER@BRANCH. The timeout defaults to the
container stop policy (normally 10 seconds); -1 waits forever.`,
	"restart": `Usage: ttc restart [-t|--time|--timeout SECONDS] (CONTAINER|CONTAINER@BRANCH)...

Restart branches using their configured stop signal; a CONTAINER alone names
its main branch. The timeout defaults to the container stop policy (normally
10 seconds); -1 waits forever.`,
	"rm": `Usage: ttc rm [-f|--force] [-v|--volumes] TARGET...

Without --force (default), running branches are rejected. --volumes applies
only to whole-family removal; both options default to false.`,
	"attach": `Usage: ttc attach [OPTIONS] CONTAINER@BRANCH

Options:
  -i, --interactive       Forward standard input (default: false)
  --no-stdin              Never forward input (default: false)
  --no-stdout             Do not attach stdout (default: false)
  --no-stderr             Do not attach stderr (default: false)
  --exec EXEC_ID          Attach to a managed exec (default: init process)
  --detach-keys KEYS      Comma-separated key sequence (default: ctrl-p,ctrl-q)
  --takeover              Replace an existing input attachment (default: false)`,
	"logs": `Usage: ttc logs [OPTIONS] CONTAINER@BRANCH

Options:
  -f, --follow            Follow new records (default: false)
  --exec EXEC_ID          Select managed exec logs (default: init process)
  -n, --tail all|N        Lines from the end (default: all)
  --since TIME            RFC3339 timestamp, Unix time, or relative duration
  --until TIME            RFC3339 timestamp, Unix time, or relative duration
  -t, --timestamps        Prefix timestamps (default: false)
  --details               Include configured attributes (default: false)`,
	"cp": `Usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH

Exactly one endpoint must use CONTAINER@BRANCH:PATH. Use '-' for a tar stream.
--archive preserves ownership and --follow-link follows the source symlink;
both default to false. Paused, dehydrated, stopped, and exited branches are
supported.`,
	"pull": `Usage: ttc pull [--platform OS/ARCH] [-q|--quiet] IMAGE

Alias for 'ttc image pull'. Docker Hub and tag latest are the defaults;
--platform defaults to the daemon's native OCI platform.`,
	"load": `Usage: ttc load [-i|--input FILE] [--platform OS/ARCH] [-q|--quiet]

Alias for 'ttc image load'. FILE defaults to standard input and --quiet to
false.`,
	"save": `Usage: ttc save [-o|--output FILE] [--platform OS/ARCH] IMAGE [IMAGE...]

Alias for 'ttc image save'. FILE defaults to standard output.`,
	"commit": `Usage: ttc commit [OPTIONS] CONTAINER@BRANCH REPOSITORY[:TAG]

Options:
  -a, --author TEXT       Image author (default: empty)
  -m, --message TEXT      Commit message (default: empty)
  -c, --change DIRECTIVE  Dockerfile-style metadata change; repeatable
  -p, --pause=BOOL        Pause during capture (default: true)`,
	"fork": `Usage: ttc fork [--paused] [--quarantine] [--node NODE] CONTAINER@BRANCH [DEST...]

DEST values are new branch names in the same family; omit them to generate one
name. Multiple destinations share one source pause. --paused and --quarantine
default to false. In cluster mode, omitted --node invokes placement; a
quarantined fork always remains local and therefore rejects --node.`,
	"migrate": `Usage: ttc migrate --node NODE CONTAINER@BRANCH

Move the branch to the named compute node. --node is required; use 'ttc node
ls' to discover valid node names.`,
	"pause":        "Usage: ttc pause TARGET...\n\nTARGET may be a family or CONTAINER@BRANCH; no options are required.",
	"resume":       "Usage: ttc resume TARGET...\n\nTARGET may be a family or CONTAINER@BRANCH; no options are required.",
	"unpause":      "Usage: ttc unpause TARGET...",
	"quarantine":   "Usage: ttc quarantine CONTAINER@BRANCH...",
	"unquarantine": "Usage: ttc unquarantine CONTAINER@BRANCH...",
	"dehydrate":    "Usage: ttc dehydrate TARGET...\n\nTARGET may be a family or CONTAINER@BRANCH; no options are required.",
	"rehydrate": `Usage: ttc rehydrate [--node NODE] CONTAINER@BRANCH...

Reconstruct dehydrated branches. In cluster mode, omitted --node lets the
scheduler choose a compute node.`,
	"checkpoint": `Usage: ttc checkpoint [--compression none|flate-best-speed] CONTAINER@BRANCH FILE

Write a portable standalone checkpoint. Compression defaults to
flate-best-speed.`,
	"restore": `Usage: ttc restore [--node NODE] [--name NAME] FILE

Restore as a new family unrelated to the checkpoint source. NAME defaults to
restored-<UTC timestamp>; in cluster mode an omitted --node uses placement.`,
	"ls": "Usage: ttc ls [--tree|--json]\n\nTree output is the default; --json emits stable machine-readable records.",
	"ps": `Usage: ttc ps [OPTIONS]

By default show running and paused branches. Options: --all (all states),
--quiet (selectors only), --size, --no-trunc, --filter KEY=VALUE (repeatable),
--format TEMPLATE|json, --latest, and --last N. All are disabled by default;
--last defaults to -1 (no count limit).`,
	"inspect": `Usage: ttc inspect [--format TEMPLATE|json] [--type container] CONTAINER...

Inspect whole families; branch selectors are rejected. The default is indented
JSON for all requested containers.`,
	"top":   "Usage: ttc top [--json] CONTAINER@BRANCH [PS OPTIONS]\n\n--json defaults to false; remaining arguments are guest ps options.",
	"stats": "Usage: ttc stats [--json] CONTAINER@BRANCH\n\nShow branch and family private/shared resource accounting; --json defaults to false.",
	"diff":  "Usage: ttc diff [--json] CONTAINER@BRANCH\n\nShow writable-layer changes against the branch base; --json defaults to false.",
	"events": `Usage: ttc events [--json] [-f|--follow] [--since TIME] [--until TIME] [--filter KEY=VALUE]

Without --follow, replay existing matching events and return. TIME accepts the
same timestamp/relative forms as 'ttc logs'. Filters are repeatable and accept
type, event, action, subject, or container. Options default to unset/false.`,
	"wait": `Usage: ttc wait [--condition not-running|durable] CONTAINER@BRANCH...

Wait for each branch concurrently. The default condition is not-running and
prints exit codes; durable waits for authoritative persistent storage.`,
	"rename": "Usage: ttc rename CONTAINER@BRANCH NEW_BRANCH",
	"kill": `Usage: ttc kill [--signal SIGNAL] [--all] CONTAINER@BRANCH...

SIGNAL is a name or number (default: KILL). --all signals every process in the
branch instead of only init (default: false).`,
	"images": "Usage: ttc images [-q|--quiet] [--no-trunc] [--json]\n\nAlias for 'ttc image ls'; all options default to false.",
	"rmi":    "Usage: ttc rmi [-f|--force] IMAGE...\n\nAlias for 'ttc image rm'; --force defaults to false.",
	"completion": `Usage: ttc completion bash|zsh|fish

Generate a shell completion script on standard output. This command is fully
client-local and does not require a running daemon.`,
}

func localHelp(args []string, stdout io.Writer) (bool, error) {
	if args[0] == "help" {
		if len(args) == 1 {
			printUsage(stdout)
			return true, nil
		}
		if !knownHelpCommand(args[1]) {
			return true, fmt.Errorf("unknown command %q", args[1])
		}
		printHelpTopic(args[1:], stdout)
		return true, nil
	}
	if len(args) >= 2 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h") {
		if !knownHelpCommand(args[0]) {
			return true, fmt.Errorf("unknown command %q", args[0])
		}
		printHelpTopic(args[:len(args)-1], stdout)
		return true, nil
	}
	return false, nil
}

func knownHelpCommand(command string) bool {
	return command == "daemon" || command == "completion" || knownCommand(command)
}

func printHelpTopic(topic []string, stdout io.Writer) {
	key := strings.Join(topic, " ")
	if text, ok := commandHelp[key]; ok {
		fmt.Fprintln(stdout, hostHelpText(text))
		return
	}
	if len(topic) != 0 {
		if text, ok := commandHelp[topic[0]]; ok {
			fmt.Fprintln(stdout, hostHelpText(text))
			return
		}
	}
	fmt.Fprintf(stdout, "Usage: ttc %s [OPTIONS]\n", key)
}

// runCommand runs a command on the daemon at socket, passing it archive, a
// checkpoint's or a restore's, or an image load's or save's, if not nil. A
// command that ttcd can answer again from its record goes under requestID, or
// a new one if empty, which a lost answer retries.
func runCommand(socket string, args []string, stdin io.Reader, stdout, stderr io.Writer, archive *os.File, requestID string) (int, error) {
	terminal, width := terminalWriterInfo(stderr)
	req := request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "command", Args: args,
		Stdin: commandUsesStdin(args) && archive == nil, ArchiveFile: archive != nil, TerminalControl: protocol.CommandUsesTerminalControl(args), ClientTERM: os.Getenv("TERM"), StderrTerminal: terminal, StderrWidth: width}
	if commandCanRetry(args) && !req.Stdin {
		req.RequestID = requestID
		if req.RequestID == "" {
			id, err := newRequestID()
			if err != nil {
				return 1, err
			}
			req.RequestID = id
		}
	}
	attempts := 1
	if req.RequestID != "" {
		attempts = 2
	}
	var lastErr error
	var staleUntil time.Time
	staleDelay := 25 * time.Millisecond
	for attempt := 0; attempt < attempts; attempt++ {
		conn, err := connect(socket)
		if err != nil {
			return 1, err
		}
		out, errOut := stdout, stderr
		var bufferedOut, bufferedErr bytes.Buffer
		if req.RequestID != "" {
			out, errOut = &bufferedOut, &bufferedErr
		}
		status, err := exchange(conn, req, stdin, out, errOut, archive)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if req.RequestID != "" {
			// The node that owned the command's branch refused it before it
			// ran or journaled it, as another command had moved, unplaced or
			// claimed the branch. It goes again under its request ID, at once
			// and then less often, waiting out the other command.
			if status != 0 && bufferedOut.Len() == 0 && protocol.StaleRouteAnswer(bufferedErr.String()) {
				now := time.Now()
				if staleUntil.IsZero() {
					staleUntil = now.Add(protocol.StaleRouteWait)
					attempt--
					continue
				}
				if now.Before(staleUntil) {
					time.Sleep(staleDelay)
					staleDelay = min(2*staleDelay, time.Second)
					attempt--
					continue
				}
			}
			_, _ = stdout.Write(bufferedOut.Bytes())
			_, _ = stderr.Write(bufferedErr.Bytes())
		}
		return status, nil
	}
	return 1, fmt.Errorf("daemon mutation request %s: %w", req.RequestID, lastErr)
}

func runDaemon(opts options, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "ttc: usage: ttc daemon start|status|stop|requests|request")
		return 2
	}
	switch args[0] {
	case "start":
		if conn, err := net.DialTimeout("unix", opts.socket, 150*time.Millisecond); err == nil {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			status, exchangeErr := exchange(conn, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, stderr, nil)
			conn.Close()
			if exchangeErr != nil {
				fmt.Fprintf(stderr, "ttc: checking running daemon: %v\n", exchangeErr)
				return 1
			}
			if status != 0 {
				return status
			}
			fmt.Fprintln(stdout, "ttcd already running; use 'ttc daemon status' to inspect its mode")
			return 0
		}
		if err := startDaemon(opts.socket, args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return 0
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "ttc: usage: ttc daemon status")
			return 2
		}
		return exchangeControl(opts.socket, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, stdout, stderr)
	case "stop":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--dehydrate-all" && args[1] != "--stop-all" && args[1] != "--migrate-all") {
			fmt.Fprintln(stderr, "ttc: usage: ttc daemon stop [--dehydrate-all|--stop-all|--migrate-all]")
			return 2
		}
		return stopDaemon(opts.socket, args[1:], stdout, stderr)
	case "requests", "request":
		return exchangeControl(opts.socket, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "mutation-control", Args: args}, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ttc: unknown daemon command %q\n", args[0])
		return 2
	}
}

// stopDaemon keeps daemon shutdown available while upgrading from daemons
// that predate the version-independent stop action. Current daemons accept the
// first request without checking Version or ClientBuildID. A legacy daemon may
// reject it before dispatch and report its own API version; retrying the exact
// same stop action with that version is safe because the rejected request made
// no state change.
func stopDaemon(socket string, args []string, stdout, stderr io.Writer) int {
	req := request{Version: daemonAPIVersion, Action: "stop", ManageStop: true, Args: append([]string(nil), args...)}
	status, out, diagnostic, err := exchangeControlCapture(socket, req)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 1
	}
	if status != 0 {
		if legacyVersion, ok := reportedDaemonAPIVersion(string(diagnostic)); ok && legacyVersion != req.Version {
			req.Version = legacyVersion
			status, out, diagnostic, err = exchangeControlCapture(socket, req)
			if err != nil {
				fmt.Fprintf(stderr, "ttc: retrying daemon stop with API version %d: %v\n", legacyVersion, err)
				return 1
			}
		}
	}
	_, _ = stdout.Write(out)
	_, _ = stderr.Write(diagnostic)
	return status
}

func exchangeControlCapture(socket string, req request) (int, []byte, []byte, error) {
	conn, err := connect(socket)
	if err != nil {
		return 1, nil, nil, err
	}
	defer conn.Close()
	var stdout, stderr bytes.Buffer
	status, err := exchange(conn, req, strings.NewReader(""), &stdout, &stderr, nil)
	return status, stdout.Bytes(), stderr.Bytes(), err
}

func reportedDaemonAPIVersion(diagnostic string) (int, bool) {
	const marker = "(server uses "
	start := strings.LastIndex(diagnostic, marker)
	if start < 0 {
		return 0, false
	}
	value := diagnostic[start+len(marker):]
	end := strings.IndexByte(value, ')')
	if end < 0 {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value[:end]))
	if err != nil || parsed <= 0 || parsed > 1<<16 {
		return 0, false
	}
	return parsed, true
}

func exchangeControl(socket string, req request, stdout, stderr io.Writer) int {
	conn, err := connect(socket)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 1
	}
	defer conn.Close()
	status, err := exchange(conn, req, strings.NewReader(""), stdout, stderr, nil)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 1
	}
	return status
}

// reportStartupWarnings repeats what a daemon that has just become ready
// warned of in its log as it started, such as the topology of CPUs it was
// not told how to show, or the isolation it was told to go without: the
// user who starts it reads the terminal, not the log.
func reportStartupWarnings(stderr io.Writer, logPath string) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	for _, line := range startupWarnings(string(data)) {
		fmt.Fprintln(stderr, line)
	}
}

// startupWarnings returns the warnings in the log of a daemon that has just
// started.
func startupWarnings(log string) []string {
	var warnings []string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.HasPrefix(line, "ttcd: warning: ") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func startDaemon(socket string, args []string, stdout, stderr io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	daemon := filepath.Join(filepath.Dir(executable), "ttcd")
	info, err := os.Stat(daemon)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return fmt.Errorf("matching executable ttcd was not found beside %s", executable)
	}
	versionOutput, err := exec.Command(daemon, "--version").Output()
	if err != nil {
		return fmt.Errorf("checking ttcd version: %w", explainDaemonExecFailure(daemon, err))
	}
	want := "ttcd version " + version.Version()
	if strings.TrimSpace(string(versionOutput)) != want {
		return fmt.Errorf("ttc build %q does not match %s", version.Version(), strings.TrimSpace(string(versionOutput)))
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	logPath := protocol.DaemonLog(filepath.Dir(socket))
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	command := exec.Command(daemon, append([]string{"--socket", socket}, args...)...)
	command.Stdin, command.Stdout, command.Stderr = devNull, log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ready := func() bool {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
		status, err := exchange(conn, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, io.Discard, nil)
		return err == nil && status == 0
	}
	deadline := time.Now().Add(daemonStartTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			reportStartupWarnings(stderr, logPath)
			fmt.Fprintln(stdout, "ttcd started")
			return nil
		}
		select {
		case err := <-done:
			data, _ := os.ReadFile(logPath)
			return fmt.Errorf("ttcd did not become ready: %v\n%s", err, strings.TrimSpace(string(data)))
		case <-time.After(25 * time.Millisecond):
		}
	}
	// Close the deadline race with a daemon that became ready during the final
	// sleep. If it is still unavailable, do not leave a late-starting orphan:
	// slow or wedged shared storage can otherwise make `daemon start` report
	// failure while ttcd appears several seconds (or minutes) later.
	if ready() {
		reportStartupWarnings(stderr, logPath)
		fmt.Fprintln(stdout, "ttcd started")
		return nil
	}
	terminateStartedDaemon(command, done)
	_ = os.Remove(socket)
	data, _ := os.ReadFile(logPath)
	return fmt.Errorf("ttcd did not become ready; startup log: %s\n%s", logPath, strings.TrimSpace(string(data)))
}

func terminateStartedDaemon(command *exec.Cmd, done <-chan error) {
	if command == nil || command.Process == nil {
		return
	}
	// ttcd is a session leader. Signal the entire startup session so any helper
	// it managed to create before readiness cannot survive the failed launch.
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		// A task in uninterruptible kernel I/O cannot be reaped yet, but SIGKILL
		// remains pending and prevents it from becoming a daemon after recovery.
	}
}

func connect(socket string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("ttcd is not running; start it explicitly with 'ttc daemon start': %w", err)
	}
	return conn, nil
}

// exchange sends req on conn, with archive, if not nil, passed with its
// header, and relays the daemon's frames until its status.
func exchange(conn net.Conn, req request, stdin io.Reader, stdout, stderr io.Writer, archive *os.File) (int, error) {
	header, err := json.Marshal(req)
	if err != nil {
		return 1, err
	}
	header = append(header, '\n')
	var writeMu sync.Mutex
	if archive != nil {
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			return 1, fmt.Errorf("cannot pass a file over %T", conn)
		}
		// The file goes with the header's first bytes.
		n, _, err := unixConn.WriteMsgUnix(header, syscall.UnixRights(int(archive.Fd())), nil)
		if err != nil {
			return 1, err
		}
		header = header[n:]
	}
	if err := writeBytes(conn, header); err != nil {
		return 1, err
	}
	flow := newStdinFlow()
	defer flow.close()
	if req.Stdin {
		go func() {
			buffer := make([]byte, 256<<10)
			for {
				n, readErr := stdin.Read(buffer)
				if n != 0 {
					if flow.reserve(n) != nil || writeFrame(conn, &writeMu, daemonFrameStdin, buffer[:n]) != nil {
						return
					}
				}
				if readErr != nil {
					_ = writeFrame(conn, &writeMu, daemonFrameEOF, nil)
					return
				}
			}
		}()
	}
	terminal := newClientTerminal(stdin, func(typ byte, payload []byte) error { return writeFrame(conn, &writeMu, typ, payload) })
	defer terminal.close()
	reader := bufio.NewReader(conn)
	for {
		typ, payload, err := readFrame(reader)
		if err != nil {
			return 1, err
		}
		switch typ {
		case daemonFrameStdout:
			_, err = stdout.Write(payload)
		case daemonFrameStderr:
			_, err = stderr.Write(payload)
		case daemonFrameStdinAck:
			if len(payload) != 4 {
				return 1, errors.New("invalid stdin acknowledgement")
			}
			err = flow.ack(int(binary.BigEndian.Uint32(payload)))
		case daemonFrameTerminal:
			err = terminal.configure(payload)
		case daemonFrameTTYState:
			_, err = stdout.Write(append(payload, '\n'))
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 1, errors.New("invalid daemon status")
			}
			return int(binary.BigEndian.Uint32(payload)), nil
		default:
			return 1, fmt.Errorf("unexpected daemon frame %q", typ)
		}
		if err != nil {
			return 1, err
		}
	}
}

// writeFrame writes a frame, its header and payload in one write where w
// takes several buffers at once.
func writeFrame(w io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return errors.New("daemon frame is too large")
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	buffers := net.Buffers{header[:], payload}
	_, err := buffers.WriteTo(w)
	return err
}

func writeBytes(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > daemonMaxFrame {
		return 0, nil, errors.New("daemon frame is too large")
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(r, payload)
	return header[0], payload, err
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
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

func commandUsesStdin(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" && len(args) > 1 {
		args = args[1:]
	}
	switch args[0] {
	case "load":
		return !hasFileOption(args[1:], "-i", "--input")
	case "image":
		return len(args) > 1 && args[1] == "load" && !hasFileOption(args[2:], "-i", "--input")
	case "cp":
		for _, arg := range args[1:] {
			if arg == "-" {
				return true
			}
		}
		return false
	case "restore":
		return len(args) > 1 && args[len(args)-1] == "-"
	case "attach", "run", "start", "exec":
	default:
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "-i" || arg == "--interactive" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "i")) {
			return true
		}
	}
	return false
}

func hasFileOption(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func commandCanRetry(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" && len(args) > 1 {
		args = args[1:]
	}
	switch args[0] {
	case "create", "rmi", "rm", "pause", "resume", "unpause", "quarantine", "unquarantine", "dehydrate", "rehydrate", "kill", "stop", "restart", "fork", "restore", "migrate":
		return true
	case "node":
		return len(args) > 1 && args[1] != "ls"
	case "port":
		return len(args) > 1 && (args[1] == "publish" || args[1] == "unpublish")
	case "image":
		return len(args) > 1 && (args[1] == "rm" || args[1] == "remove")
	case "cp":
		return len(args) > 2 && strings.Contains(args[len(args)-1], "@") && strings.Contains(args[len(args)-1], ":")
	case "run":
		return hasBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !hasBoolFlag(args[1:], 'a', "--attach")
	default:
		return false
	}
}

func hasBoolFlag(args []string, short byte, long string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsRune(arg[1:], rune(short))) {
			return true
		}
	}
	return false
}

// normalizeEnvironmentImports resolves run/create --env NAME in this process,
// as Docker does. The command itself runs in ttcd, which may have been started
// before NAME was exported and must not substitute its own environment for
// the client's.
func normalizeEnvironmentImports(args []string) []string {
	command := 0
	if len(args) != 0 && args[0] == "container" {
		command = 1
	}
	if command >= len(args) || (args[command] != "run" && args[command] != "create") {
		return args
	}
	result := append([]string(nil), args...)
	valueFlags := map[string]bool{
		"--name": true, "--pull": true, "--workdir": true, "-w": true,
		"--user": true, "-u": true, "--hostname": true, "-h": true,
		"--network": true, "--net": true, "--ip": true, "--mac-address": true,
		"--publish": true, "-p": true, "--add-host": true, "--dns": true,
		"--dns-search": true, "--dns-option": true, "--dns-opt": true,
		"--entrypoint": true, "--stop-signal": true, "--stop-timeout": true,
		"--pids-limit": true, "--memory": true, "-m": true, "--memory-swap": true,
		"--cpus": true, "--cpu-shares": true, "--cpu-period": true, "--cpu-quota": true,
		"--shm-size": true, "--group-add": true, "--cap-add": true,
		"--cap-drop": true, "--ulimit": true, "--env": true, "-e": true,
		"--env-file": true, "--label": true, "-l": true, "--label-file": true,
		"--volume": true, "-v": true, "--mount": true, "--tmpfs": true,
		"--log-driver": true, "--log-opt": true, "--node": true, "--platform": true,
	}
	resolve := func(value string) string {
		if strings.Contains(value, "=") {
			return value
		}
		if hostValue, ok := os.LookupEnv(value); ok {
			return value + "=" + hostValue
		}
		return value
	}
	for i := command + 1; i < len(result); i++ {
		arg := result[i]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}
		switch {
		case arg == "--env" || arg == "-e":
			if i+1 < len(result) {
				i++
				result[i] = resolve(result[i])
			}
		case strings.HasPrefix(arg, "--env="):
			result[i] = "--env=" + resolve(strings.TrimPrefix(arg, "--env="))
		case strings.HasPrefix(arg, "-e="):
			result[i] = "-e=" + resolve(strings.TrimPrefix(arg, "-e="))
		case valueFlags[arg]:
			i++
		}
	}
	return result
}

func normalizeHostPaths(args []string) ([]string, error) {
	result := append([]string(nil), args...)
	command := result[0]
	switch command {
	case "checkpoint":
		if len(result) >= 3 && result[len(result)-1] != "-" {
			return normalizeIndex(result, len(result)-1)
		}
	case "restore":
		if len(result) >= 2 && result[len(result)-1] != "-" {
			return normalizeIndex(result, len(result)-1)
		}
	case "cp":
		var operands []int
		for i := 1; i < len(result); i++ {
			if result[i] == "--" {
				continue
			}
			if strings.HasPrefix(result[i], "-") && len(operands) == 0 {
				continue
			}
			operands = append(operands, i)
		}
		if len(operands) == 2 {
			firstContainer := strings.Contains(result[operands[0]], "@") && strings.Contains(result[operands[0]], ":")
			secondContainer := strings.Contains(result[operands[1]], "@") && strings.Contains(result[operands[1]], ":")
			if firstContainer == secondContainer {
				return nil, errors.New("exactly one cp operand must name CONTAINER@BRANCH:PATH")
			}
			host := operands[0]
			if firstContainer {
				host = operands[1]
			}
			if result[host] != "-" {
				return normalizeIndex(result, host)
			}
		}
	}
	return result, nil
}

func normalizeIndex(args []string, index int) ([]string, error) {
	absolute, err := filepath.Abs(args[index])
	if err != nil {
		return nil, err
	}
	args[index] = absolute
	return args, nil
}

// completionCommands are the commands that shell completion offers: those that
// 'ttc --help' lists.
const completionCommands = "run create start stop restart rm exec attach logs cp images image pull load save commit rmi ls ps inspect top stats diff events wait fork migrate pause resume quarantine unquarantine dehydrate rehydrate checkpoint restore port volume node tty rename kill daemon completion"

func writeCompletion(shell string, w io.Writer) error {
	var script string
	switch shell {
	case "bash":
		script = `complete -W "` + completionCommands + `" ttc` + "\n"
	case "zsh":
		script = "#compdef ttc\n_arguments '1:command:(" + completionCommands + ")'\n"
	case "fish":
		script = `complete -c ttc -f -a "` + completionCommands + `"` + "\n"
	default:
		return fmt.Errorf("unsupported completion shell %q", shell)
	}
	_, err := io.WriteString(w, script)
	return err
}
