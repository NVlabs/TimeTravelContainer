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
	"gvisor.dev/gvisor/runsc/version"
)

const (
	daemonAPIVersion      = 5
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
	"pause": {}, "port": {}, "ps": {}, "pull": {}, "rehydrate": {}, "rename": {}, "restart": {},
	"restore": {}, "resume": {}, "rm": {}, "rmi": {}, "run": {}, "save": {}, "start": {},
	"stats": {}, "stop": {}, "top": {}, "tty": {}, "unpause": {}, "volume": {}, "wait": {},
	"_auto-remove": {}, "_complete": {},
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
  fork, migrate, pause, resume, dehydrate, rehydrate, checkpoint, restore
  port, volume, node, tty, rename, kill
  daemon start|status|stop|requests|request
  completion bash|zsh|fish

Branch selectors use CONTAINER@BRANCH. Run 'ttc COMMAND --help' for command
usage. ttcd requires TTC_ROOT; clustered ttcd additionally requires
TTC_LOCAL_ROOT.
`)
}

var commandHelp = map[string]string{
	"run": "Usage: ttc run [OPTIONS] IMAGE [COMMAND] [ARG...]", "create": "Usage: ttc create [OPTIONS] IMAGE [COMMAND] [ARG...]",
	"start": "Usage: ttc start [OPTIONS] CONTAINER@BRANCH", "stop": "Usage: ttc stop [OPTIONS] TARGET...",
	"restart": "Usage: ttc restart [OPTIONS] TARGET...", "rm": "Usage: ttc rm [OPTIONS] TARGET...",
	"exec": "Usage: ttc exec [OPTIONS] CONTAINER@BRANCH COMMAND [ARG...]", "attach": "Usage: ttc attach [OPTIONS] CONTAINER@BRANCH",
	"logs": "Usage: ttc logs [OPTIONS] CONTAINER@BRANCH", "cp": "Usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH",
	"pull": "Usage: ttc pull [OPTIONS] IMAGE", "load": "Usage: ttc load [--input FILE] [--platform PLATFORM] [--quiet]",
	"save": "Usage: ttc save [--output FILE] [--platform PLATFORM] IMAGE [IMAGE...]", "commit": "Usage: ttc commit [OPTIONS] CONTAINER@BRANCH REPOSITORY[:TAG]",
	"fork": "Usage: ttc fork [--paused] [--quarantine] SOURCE [DEST...]", "migrate": "Usage: ttc migrate --target NODE CONTAINER@BRANCH",
	"pause": "Usage: ttc pause TARGET...", "resume": "Usage: ttc resume TARGET...", "unpause": "Usage: ttc unpause TARGET...",
	"dehydrate": "Usage: ttc dehydrate TARGET...", "rehydrate": "Usage: ttc rehydrate TARGET...",
	"checkpoint": "Usage: ttc checkpoint [--compression MODE] CONTAINER@BRANCH FILE", "restore": "Usage: ttc restore [--name NAME] FILE",
	"ls": "Usage: ttc ls [--tree|--json]", "ps": "Usage: ttc ps [OPTIONS]", "inspect": "Usage: ttc inspect [OPTIONS] CONTAINER...",
	"top": "Usage: ttc top [--json] CONTAINER@BRANCH [PS OPTIONS]", "stats": "Usage: ttc stats [--json] CONTAINER@BRANCH",
	"diff": "Usage: ttc diff [--json] CONTAINER@BRANCH", "events": "Usage: ttc events [OPTIONS]",
	"wait": "Usage: ttc wait [--condition CONDITION] TARGET...", "port": "Usage: ttc port publish|ls|unpublish [OPTIONS]",
	"volume": "Usage: ttc volume create|ls|inspect|rm [OPTIONS]", "node": "Usage: ttc node ls|drain|rebalance [OPTIONS]",
	"tty": "Usage: ttc tty stat|watch CONTAINER@BRANCH --session NAME [--json]", "rename": "Usage: ttc rename CONTAINER@BRANCH NEW_BRANCH",
	"kill": "Usage: ttc kill [OPTIONS] TARGET...", "image": "Usage: ttc image ls|pull|load|save|inspect|rm [OPTIONS]",
	"images": "Usage: ttc images [OPTIONS]", "rmi": "Usage: ttc rmi [OPTIONS] IMAGE...", "container": "Usage: ttc container COMMAND",
}

func localHelp(args []string, stdout io.Writer) bool {
	if args[0] == "help" {
		printUsage(stdout)
		return true
	}
	if len(args) >= 2 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h") {
		key := strings.Join(args[:len(args)-1], " ")
		if text, ok := commandHelp[key]; ok {
			fmt.Fprintln(stdout, text)
			return true
		}
		if text, ok := commandHelp[args[0]]; ok {
			fmt.Fprintln(stdout, text)
			return true
		}
		fmt.Fprintf(stdout, "Usage: ttc %s [OPTIONS]\n", key)
		return true
	}
	return false
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
		if len(args) > 2 || (len(args) == 2 && args[1] != "--dehydrate-all" && args[1] != "--stop-all") {
			fmt.Fprintln(stderr, "ttc: usage: ttc daemon stop [--dehydrate-all|--stop-all]")
			return 2
		}
		return exchangeControl(opts.socket, request{Action: "stop", ManageStop: true, Args: args[1:]}, stdout, stderr)
	case "requests", "request":
		return exchangeControl(opts.socket, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "mutation-control", Args: args}, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ttc: unknown daemon command %q\n", args[0])
		return 2
	}
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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
			status, exchangeErr := exchange(conn, request{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, io.Discard)
			conn.Close()
			if exchangeErr == nil && status == 0 {
				fmt.Fprintln(stdout, "ttcd started")
				return nil
			}
		}
		select {
		case err := <-done:
			data, _ := os.ReadFile(logPath)
			return fmt.Errorf("ttcd did not become ready: %v\n%s", err, strings.TrimSpace(string(data)))
		case <-time.After(25 * time.Millisecond):
		}
	}
	data, _ := os.ReadFile(logPath)
	return fmt.Errorf("ttcd did not become ready; startup log: %s\n%s", logPath, strings.TrimSpace(string(data)))
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
	case "create", "rmi", "rm", "_auto-remove", "pause", "resume", "unpause", "dehydrate", "rehydrate", "kill", "stop", "restart", "fork", "restore", "migrate":
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
