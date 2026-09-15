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

// daemonStartTimeout includes cluster Raft startup and the bidirectional
// topology admission check, whose own bounded retry window is 20 seconds.
const daemonStartTimeout = 45 * time.Second

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
}

type request struct {
	Version        int      `json:"version"`
	ClientBuildID  string   `json:"client_build_id,omitempty"`
	Action         string   `json:"action"`
	RequestID      string   `json:"request_id,omitempty"`
	ClientTERM     string   `json:"client_term,omitempty"`
	StderrTerminal bool     `json:"stderr_terminal,omitempty"`
	StderrWidth    int      `json:"stderr_width,omitempty"`
	Args           []string `json:"args,omitempty"`
	Stdin          bool     `json:"stdin,omitempty"`
	ManageStop     bool     `json:"manage_stop,omitempty"`
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
	if localHelp(args, stdout) {
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
	if args[0] == "daemon" {
		return runDaemon(opts, args[1:], stdout, stderr)
	}
	if !knownCommand(args[0]) {
		fmt.Fprintf(stderr, "ttc: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	args, err = normalizeHostPaths(args)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 2
	}
	status, err := runCommand(opts.socket, args, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: daemon transport: %v\n", err)
		return 1
	}
	return status
}

func parseGlobal(args []string, stdout io.Writer) (options, []string, bool, error) {
	opts := options{socket: defaultSocket()}
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
		case strings.HasPrefix(arg, "-"):
			return opts, nil, false, fmt.Errorf("unknown global option %q", arg)
		default:
			return opts, args, false, nil
		}
	}
	return opts, args, false, nil
}

func defaultSocket() string {
	if socket := os.Getenv("TTC_SOCKET"); socket != "" {
		return socket
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ttc", "ttcd.sock")
	}
	return filepath.Join(os.TempDir(), "ttc-"+strconv.Itoa(os.Geteuid()), "ttcd.sock")
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
	fmt.Fprint(w, `Usage: ttc [--daemon-socket PATH] COMMAND

Manage TimeTravelContainer container families and live branches.

The client connects only to TTC_SOCKET (or --daemon-socket). Storage roots and
runtime configuration belong exclusively to ttcd.

Commands:
  run, create, start, stop, restart, rm, exec, attach, logs, cp
  images, image, pull, load, save, commit, rmi
  ls, ps, inspect, top, stats, diff, events, wait
  fork, migrate, pause, resume, quarantine, unquarantine
  dehydrate, rehydrate, checkpoint, restore
  port, volume, node, tty, rename, kill
  daemon start|status|stop|requests|request
  completion bash|zsh|fish

Branch selectors use CONTAINER@BRANCH. Run 'ttc COMMAND --help' for command
usage. ttcd requires TTC_ROOT; clustered ttcd additionally requires
TTC_LOCAL_ROOT.
`)
}

var commandHelp = map[string]string{
	"daemon": `Usage: ttc daemon COMMAND [OPTIONS]

Manage the local ttcd service. These commands use TTC_SOCKET or the global
--daemon-socket override; only "start" reads TTC_ROOT and TTC_LOCAL_ROOT.

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
an explicitly configured, new-format data root. Cluster mode additionally
requires TTC_LOCAL_ROOT for node-local active state and cache data.

Options:
  --platform systrap|kvm          Select the gVisor execution platform
  --cpu-profile portable|host|explicit
                                  Select the immutable guest CPU contract
  --cpu-features FEATURE,...      Features for --cpu-profile=explicit
  --unsafe-no-host-isolation      Disable host namespace/chroot isolation
  --cluster-init                  Create a new distributed TTC cluster
  --cluster-join HOST:PORT        Join an existing cluster through a seed node
  --node NAME                     Set this cluster node's stable name
  --listen ADDR                   Listen for cluster traffic
  --advertise HOST:PORT           Address advertised to other cluster nodes
  --memory-target SIZE            Target resident memory for this node
  --cluster-auto-rebalance        Automatically relieve memory pressure
  --cluster-capacity-command PATH Invoke a trusted node provisioner
  --cluster-capacity-webhook URL  Invoke a trusted node provisioner webhook

Cluster membership, transport, and capacity options are fixed for the daemon
lifetime. --cluster-init and --cluster-join are mutually exclusive.`,
	"daemon status": `Usage: ttc daemon status

Show the running daemon's build ID, runtime mode, root format, and cluster
membership. The client and daemon must have exactly matching builds.`,
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
	"image ls":      "Usage: ttc image ls [-q|--quiet] [--no-trunc] [--json]",
	"image list":    "Usage: ttc image list [-q|--quiet] [--no-trunc] [--json]",
	"image pull":    "Usage: ttc image pull [OPTIONS] IMAGE",
	"image load":    "Usage: ttc image load [-i|--input FILE] [--platform PLATFORM] [-q|--quiet]",
	"image save":    "Usage: ttc image save [-o|--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
	"image inspect": "Usage: ttc image inspect IMAGE [IMAGE...]",
	"image rm":      "Usage: ttc image rm [-f|--force] IMAGE [IMAGE...]",
	"image remove":  "Usage: ttc image remove [-f|--force] IMAGE [IMAGE...]",

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
  -d                         Run detached
  -i                         Keep standard input open
  -t                         Allocate a terminal
  -e, --env KEY=VALUE        Set an environment variable
  -u, --user USER[:GROUP]    Select the guest user
  -w, --workdir DIR          Set the working directory
  --session NAME             Create a named, reattachable session
  --process FILE             Read an OCI process specification`,
	"exec ls":   "Usage: ttc exec ls [--json] CONTAINER@BRANCH",
	"exec wait": "Usage: ttc exec wait CONTAINER@BRANCH:EXEC_ID",
	"exec kill": "Usage: ttc exec kill [--signal SIGNAL] CONTAINER@BRANCH:EXEC_ID",

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
	"volume create":  "Usage: ttc volume create NAME",
	"volume ls":      "Usage: ttc volume ls",
	"volume list":    "Usage: ttc volume list",
	"volume inspect": "Usage: ttc volume inspect NAME [NAME...]",
	"volume rm":      "Usage: ttc volume rm [-f|--force] NAME [NAME...]",
	"volume remove":  "Usage: ttc volume remove [-f|--force] NAME [NAME...]",

	"node": `Usage: ttc node COMMAND [OPTIONS]

Inspect and manage nodes in a distributed TTC cluster.

Commands:
  ls [--json]                List nodes, load, capacity, state, and lease
  stop NODE [POLICY]         Evacuate or dispose of workloads and stop a node
  rebalance NODE             Relieve a node's configured memory pressure

Node stop policies are --migrate-all, --dehydrate-all, and --stop-all.`,
	"node ls":        "Usage: ttc node ls [--json]",
	"node stop":      "Usage: ttc node stop NODE [--stop-all|--dehydrate-all|--migrate-all]",
	"node rebalance": "Usage: ttc node rebalance NODE",

	"tty": `Usage: ttc tty COMMAND CONTAINER@BRANCH --session NAME [--json]

Observe named exec-session terminal state without attaching to it.

Commands:
  stat     Print one terminal observation
  watch    Stream coalesced terminal changes`,
	"tty stat":  "Usage: ttc tty stat CONTAINER@BRANCH --session NAME [--json]",
	"tty watch": "Usage: ttc tty watch CONTAINER@BRANCH --session NAME [--json]",

	"run":          "Usage: ttc run [--node NODE] [OPTIONS] IMAGE [COMMAND] [ARG...]",
	"create":       "Usage: ttc create [OPTIONS] IMAGE [COMMAND] [ARG...]",
	"start":        "Usage: ttc start [--node NODE] [OPTIONS] CONTAINER@BRANCH",
	"stop":         "Usage: ttc stop [OPTIONS] TARGET...",
	"restart":      "Usage: ttc restart [OPTIONS] TARGET...",
	"rm":           "Usage: ttc rm [OPTIONS] TARGET...",
	"attach":       "Usage: ttc attach [OPTIONS] CONTAINER@BRANCH",
	"logs":         "Usage: ttc logs [OPTIONS] CONTAINER@BRANCH",
	"cp":           "Usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH",
	"pull":         "Usage: ttc pull [OPTIONS] IMAGE",
	"load":         "Usage: ttc load [--input FILE] [--platform PLATFORM] [--quiet]",
	"save":         "Usage: ttc save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]",
	"commit":       "Usage: ttc commit [OPTIONS] CONTAINER@BRANCH REPOSITORY[:TAG]",
	"fork":         "Usage: ttc fork [--paused] [--quarantine] [--node NODE] CONTAINER@BRANCH DEST...",
	"migrate":      "Usage: ttc migrate --node NODE CONTAINER@BRANCH",
	"pause":        "Usage: ttc pause TARGET...",
	"resume":       "Usage: ttc resume TARGET...",
	"unpause":      "Usage: ttc unpause TARGET...",
	"quarantine":   "Usage: ttc quarantine CONTAINER@BRANCH...",
	"unquarantine": "Usage: ttc unquarantine CONTAINER@BRANCH...",
	"dehydrate":    "Usage: ttc dehydrate TARGET...",
	"rehydrate":    "Usage: ttc rehydrate [--node NODE] TARGET...",
	"checkpoint":   "Usage: ttc checkpoint [--compression MODE] CONTAINER@BRANCH FILE",
	"restore":      "Usage: ttc restore [--node NODE] [--name NAME] FILE",
	"ls":           "Usage: ttc ls [--tree|--json]",
	"ps":           "Usage: ttc ps [OPTIONS]",
	"inspect":      "Usage: ttc inspect [OPTIONS] CONTAINER...",
	"top":          "Usage: ttc top [--json] CONTAINER@BRANCH [PS OPTIONS]",
	"stats":        "Usage: ttc stats [--json] CONTAINER@BRANCH",
	"diff":         "Usage: ttc diff [--json] CONTAINER@BRANCH",
	"events":       "Usage: ttc events [OPTIONS]",
	"wait":         "Usage: ttc wait [--condition not-running|durable] TARGET...",
	"rename":       "Usage: ttc rename CONTAINER@BRANCH NEW_BRANCH",
	"kill":         "Usage: ttc kill [OPTIONS] TARGET...",
	"images":       "Usage: ttc images [OPTIONS]",
	"rmi":          "Usage: ttc rmi [OPTIONS] IMAGE...",
	"completion": `Usage: ttc completion bash|zsh|fish

Generate a shell completion script on standard output. This command is fully
client-local and does not require a running daemon.`,
}

func localHelp(args []string, stdout io.Writer) bool {
	if args[0] == "help" {
		if len(args) == 1 {
			printUsage(stdout)
			return true
		}
		printHelpTopic(args[1:], stdout)
		return true
	}
	if len(args) >= 2 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h") {
		printHelpTopic(args[:len(args)-1], stdout)
		return true
	}
	return false
}

func printHelpTopic(topic []string, stdout io.Writer) {
	key := strings.Join(topic, " ")
	if text, ok := commandHelp[key]; ok {
		fmt.Fprintln(stdout, text)
		return
	}
	if len(topic) != 0 {
		if text, ok := commandHelp[topic[0]]; ok {
			fmt.Fprintln(stdout, text)
			return
		}
	}
	fmt.Fprintf(stdout, "Usage: ttc %s [OPTIONS]\n", key)
}

func runCommand(socket string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	terminal, width := terminalWriterInfo(stderr)
	req := request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "command", Args: args,
		Stdin: commandUsesStdin(args), ClientTERM: os.Getenv("TERM"), StderrTerminal: terminal, StderrWidth: width}
	if commandCanRetry(args) && !req.Stdin {
		id, err := newRequestID()
		if err != nil {
			return 1, err
		}
		req.RequestID = id
	}
	attempts := 1
	if req.RequestID != "" {
		attempts = 2
	}
	var lastErr error
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
		status, err := exchange(conn, req, stdin, out, errOut)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if req.RequestID != "" {
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
			status, exchangeErr := exchange(conn, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, stderr)
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
	status, err := exchange(conn, req, strings.NewReader(""), &stdout, &stderr)
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
	status, err := exchange(conn, req, strings.NewReader(""), stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: %v\n", err)
		return 1
	}
	return status
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
		return fmt.Errorf("checking ttcd version: %w", err)
	}
	want := "ttcd version " + version.Version()
	if strings.TrimSpace(string(versionOutput)) != want {
		return fmt.Errorf("ttc build %q does not match %s", version.Version(), strings.TrimSpace(string(versionOutput)))
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(socket), "ttcd.log")
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
		status, err := exchange(conn, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, io.Discard)
		return err == nil && status == 0
	}
	deadline := time.Now().Add(daemonStartTimeout)
	for time.Now().Before(deadline) {
		if ready() {
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

func exchange(conn net.Conn, req request, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	header, err := json.Marshal(req)
	if err != nil {
		return 1, err
	}
	header = append(header, '\n')
	var writeMu sync.Mutex
	if err := writeBytes(conn, header); err != nil {
		return 1, err
	}
	flow := newStdinFlow()
	defer flow.close()
	if req.Stdin {
		go func() {
			buffer := make([]byte, 32<<10)
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

func writeFrame(w io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return errors.New("daemon frame is too large")
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	if err := writeBytes(w, header[:]); err != nil {
		return err
	}
	return writeBytes(w, payload)
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

func normalizeHostPaths(args []string) ([]string, error) {
	result := append([]string(nil), args...)
	command := result[0]
	index := 1
	if command == "image" && len(result) > 1 {
		command, index = result[1], 2
	}
	switch command {
	case "load":
		return normalizeOptionPath(result, index, "-i", "--input")
	case "save":
		return normalizeOptionPath(result, index, "-o", "--output")
	case "checkpoint":
		if len(result) >= 3 {
			return normalizeIndex(result, len(result)-1)
		}
	case "restore":
		if len(result) >= 2 {
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

func normalizeOptionPath(args []string, start int, names ...string) ([]string, error) {
	for i := start; i < len(args); i++ {
		for _, name := range names {
			if args[i] == name && i+1 < len(args) {
				return normalizeIndex(args, i+1)
			}
			if strings.HasPrefix(args[i], name+"=") {
				path := strings.TrimPrefix(args[i], name+"=")
				absolute, err := filepath.Abs(path)
				if err != nil {
					return nil, err
				}
				args[i] = name + "=" + absolute
				return args, nil
			}
		}
	}
	return args, nil
}

func normalizeIndex(args []string, index int) ([]string, error) {
	absolute, err := filepath.Abs(args[index])
	if err != nil {
		return nil, err
	}
	args[index] = absolute
	return args, nil
}

func writeCompletion(shell string, w io.Writer) error {
	var script string
	switch shell {
	case "bash":
		script = `complete -W "run create start stop restart rm exec attach logs cp images image pull load save commit ls ps inspect top stats diff events wait fork migrate pause resume dehydrate rehydrate checkpoint restore port volume node tty rename kill daemon" ttc` + "\n"
	case "zsh":
		script = `#compdef ttc
_arguments '1:command:(run create start stop restart rm exec attach logs cp images image pull load save commit ls ps inspect top stats diff events wait fork migrate pause resume dehydrate rehydrate checkpoint restore port volume node tty rename kill daemon)'` + "\n"
	case "fish":
		script = `complete -c ttc -f -a "run create start stop restart rm exec attach logs cp images image pull load save commit ls ps inspect top stats diff events wait fork migrate pause resume dehydrate rehydrate checkpoint restore port volume node tty rename kill daemon"` + "\n"
	default:
		return fmt.Errorf("unsupported completion shell %q", shell)
	}
	_, err := io.WriteString(w, script)
	return err
}
