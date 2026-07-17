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

package cmd

import (
	"bufio"
	"encoding/binary"
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
)

const (
	daemonAPIVersion  = 1
	daemonFrameStdin  = byte('I')
	daemonFrameEOF    = byte('C')
	daemonFrameStdout = byte('O')
	daemonFrameStderr = byte('E')
	daemonFrameStatus = byte('X')
	daemonMaxFrame    = 1 << 20
)

type daemonWireOptions struct {
	Root                  string `json:"root"`
	Runsc                 string `json:"runsc"`
	Containerd            string `json:"containerd"`
	Namespace             string `json:"namespace"`
	DataRoot              string `json:"data_root"`
	UnsafeNoHostIsolation bool   `json:"unsafe_no_host_isolation"`
}

type daemonRequest struct {
	Version int               `json:"version"`
	Action  string            `json:"action"`
	Options daemonWireOptions `json:"options,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Stdin   bool              `json:"stdin,omitempty"`
}

type daemonServer struct {
	listener net.Listener

	stopOnce sync.Once

	reconcileMu sync.Mutex
	reconciled  map[string]bool

	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex

	eventMu  sync.Mutex
	eventSeq uint64
}

type daemonFrameWriter struct {
	conn net.Conn
	mu   *sync.Mutex
	typ  byte
}

func (w daemonFrameWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, writeDaemonFrame(w.conn, w.mu, w.typ, nil)
	}
	for written := 0; written < len(data); {
		end := written + daemonMaxFrame
		if end > len(data) {
			end = len(data)
		}
		if err := writeDaemonFrame(w.conn, w.mu, w.typ, data[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return len(data), nil
}

func defaultDaemonSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ttos", "ttosd.sock")
	}
	return filepath.Join(os.TempDir(), "ttos-"+strconv.Itoa(os.Geteuid()), "ttosd.sock")
}

func directBackend(opts options, stdin io.Reader, stdout, stderr io.Writer) *runtimeBackend {
	return &runtimeBackend{
		root:                  opts.root,
		runsc:                 opts.runsc,
		containerd:            opts.containerd,
		namespace:             opts.namespace,
		dataRoot:              opts.dataRoot,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
	}
}

func wireOptions(opts options) daemonWireOptions {
	return daemonWireOptions{
		Root: opts.root, Runsc: opts.runsc, Containerd: opts.containerd,
		Namespace: opts.namespace, DataRoot: opts.dataRoot,
		UnsafeNoHostIsolation: opts.unsafeNoHostIsolation,
	}
}

func optionsFromWire(w daemonWireOptions) options {
	return options{
		root: w.Root, runsc: w.Runsc, containerd: w.Containerd,
		namespace: w.Namespace, dataRoot: w.DataRoot,
		unsafeNoHostIsolation: w.UnsafeNoHostIsolation,
	}
}

// RunDaemon serves the versioned TTOS local API. It is used by the standalone
// ttosd binary and by ttos's private autostart entry point.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	socket := defaultDaemonSocket()
	for len(args) != 0 {
		switch {
		case args[0] == "--socket" && len(args) >= 2:
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		default:
			fmt.Fprintf(stderr, "ttosd: unknown option %q\n", args[0])
			return 2
		}
	}
	if socket == "" {
		fmt.Fprintln(stderr, "ttosd: socket path must not be empty")
		return 2
	}
	if err := serveDaemon(socket, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttosd: %v\n", err)
		return 1
	}
	return 0
}

func secureDaemonDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket parent %q is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("socket directory %q is owned by UID %d, not %d", dir, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("socket directory %q permissions %04o allow access by other users", dir, info.Mode().Perm())
	}
	return nil
}

func serveDaemon(socket string, stdout, stderr io.Writer) error {
	dir := filepath.Dir(socket)
	if err := secureDaemonDirectory(dir); err != nil {
		return err
	}
	startLock, err := os.OpenFile(socket+".start.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening daemon startup lock: %w", err)
	}
	defer startLock.Close()
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking daemon startup: %w", err)
	}
	if conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("daemon is already listening on %s", socket)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("securing daemon socket: %w", err)
	}
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlocking daemon startup: %w", err)
	}
	server := &daemonServer{
		listener:      listener,
		reconciled:    make(map[string]bool),
		mutationLocks: make(map[string]*sync.Mutex),
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "ttosd listening on %s (API v%d)\n", socket, daemonAPIVersion)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accepting client: %w", err)
		}
		go server.handle(conn, stderr)
	}
}

func checkDaemonPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("non-Unix daemon connection %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil {
		return errors.New("daemon peer credentials are unavailable")
	}
	if int(credential.Uid) != os.Geteuid() {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", credential.Uid, os.Geteuid())
	}
	return nil
}

func (s *daemonServer) handle(conn net.Conn, serverErrors io.Writer) {
	defer conn.Close()
	if err := checkDaemonPeer(conn); err != nil {
		if serverErrors != nil {
			fmt.Fprintf(serverErrors, "ttosd: rejecting peer: %v\n", err)
		}
		return
	}
	reader := bufio.NewReader(conn)
	header, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	var request daemonRequest
	if err := json.Unmarshal([]byte(header), &request); err != nil {
		return
	}
	var writeMu sync.Mutex
	writeError := func(err error) {
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStderr, []byte("ttosd: "+err.Error()+"\n"))
		_ = writeDaemonStatus(conn, &writeMu, 1)
	}
	if request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	switch request.Action {
	case "ping":
		payload, _ := json.Marshal(map[string]any{"version": daemonAPIVersion, "pid": os.Getpid()})
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.stopOnce.Do(func() { s.listener.Close() })
		return
	case "command":
	default:
		writeError(fmt.Errorf("unknown daemon action %q", request.Action))
		return
	}
	if len(request.Args) == 0 {
		writeError(errors.New("empty command"))
		return
	}

	stdinReader, stdinWriter := io.Pipe()
	if !request.Stdin {
		stdinWriter.Close()
	}
	if request.Stdin {
		go receiveDaemonInput(reader, stdinWriter)
	}

	opts := optionsFromWire(request.Options)
	backend := directBackend(opts, stdinReader,
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout},
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
	if request.Args[0] == "events" {
		status := 0
		if err := s.streamEvents(opts.dataRoot, request.Args[1:], backend.stdout); err != nil {
			fmt.Fprintf(backend.stderr, "ttos: %v\n", err)
			status = 1
		}
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	s.reconcile(backend)
	unlock := s.lockMutation(opts.dataRoot, request.Args)
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	unlock()
	stdinReader.Close()
	_ = writeDaemonStatus(conn, &writeMu, status)
}

func daemonCommandNeedsMutationLock(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "create", "pull", "rmi", "rm", "_auto-remove", "pause", "resume", "unpause", "kill", "stop", "restart", "fork":
		return true
	case "run":
		return daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !daemonCommandBoolFlag(args[1:], 'a', "--attach")
	case "image":
		return len(args) > 1 && (args[1] == "rm" || args[1] == "remove")
	case "branch":
		return len(args) > 1 && (args[1] == "rm" || args[1] == "remove")
	case "volume":
		return len(args) > 1 && (args[1] == "create" || args[1] == "rm" || args[1] == "remove")
	default:
		return false
	}
}

func daemonCommandBoolFlag(args []string, short byte, long string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long {
			value = true
			continue
		}
		if strings.HasPrefix(arg, long+"=") {
			if parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, long+"=")); err == nil {
				value = parsed
			}
			continue
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(short)) {
			value = true
		}
	}
	return value
}

func (s *daemonServer) lockMutation(dataRoot string, args []string) func() {
	if !daemonCommandNeedsMutationLock(args) {
		return func() {}
	}
	key := filepath.Clean(dataRoot)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	s.mutationMu.Lock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.Mutex)
	}
	lock := s.mutationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.mutationLocks[key] = lock
	}
	s.mutationMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func receiveDaemonInput(reader *bufio.Reader, writer *io.PipeWriter) {
	defer writer.Close()
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			writer.CloseWithError(err)
			return
		}
		switch typ {
		case daemonFrameStdin:
			if _, err := writer.Write(payload); err != nil {
				return
			}
		case daemonFrameEOF:
			return
		default:
			writer.CloseWithError(fmt.Errorf("unexpected client frame %q", typ))
			return
		}
	}
}

func (s *daemonServer) reconcile(backend *runtimeBackend) {
	if backend.dataRoot == "" {
		return
	}
	key := backend.dataRoot + "\x00" + backend.root
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconciled[key] {
		return
	}
	// listFamilies reconciles stale persisted running/paused state against runsc.
	families, err := backend.listFamilies()
	if err != nil {
		return
	}
	for _, family := range families {
		for _, branch := range family.Branches {
			if branch.State != "running" && branch.State != "paused" {
				continue
			}
			target := selector{container: family.ID, branch: branch.ID}
			_, _ = backend.ensureConsoleDrain(target, false)
			sessions, err := backend.execSessions(target)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if session.State == "running" {
					_, _ = backend.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
				}
			}
		}
	}
	s.reconciled[key] = true
}

func writeDaemonFrame(writer io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return fmt.Errorf("daemon frame is too large: %d", len(payload))
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	if err := writeDaemonBytes(writer, header[:]); err != nil {
		return err
	}
	return writeDaemonBytes(writer, payload)
}

func writeDaemonBytes(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readDaemonFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > daemonMaxFrame {
		return 0, nil, fmt.Errorf("daemon frame is too large: %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeDaemonStatus(writer io.Writer, mu *sync.Mutex, status int) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(status))
	return writeDaemonFrame(writer, mu, daemonFrameStatus, payload[:])
}

func daemonCommandUsesStdin(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "attach", "run", "start":
	case "exec":
		if len(args) > 1 && (args[1] == "ls" || args[1] == "wait" || args[1] == "kill") {
			return false
		}
	default:
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "-i" || arg == "--interactive" {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.TrimPrefix(arg, "-"), "i") {
			return true
		}
	}
	return false
}

func runViaDaemon(opts options, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	conn, err := connectDaemon(opts.daemonSocket)
	if err != nil {
		if err := startDaemonProcess(opts.daemonSocket); err != nil {
			return 1, err
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err = connectDaemon(opts.daemonSocket)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return 1, fmt.Errorf("connecting to autostarted daemon: %w", err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	defer conn.Close()
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", Options: wireOptions(opts),
		Args: append([]string(nil), args...), Stdin: daemonCommandUsesStdin(args),
	}
	return exchangeDaemon(conn, request, stdin, stdout, stderr)
}

func connectDaemon(socket string) (net.Conn, error) {
	return net.DialTimeout("unix", socket, 250*time.Millisecond)
}

func exchangeDaemon(conn net.Conn, request daemonRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	header, err := json.Marshal(request)
	if err != nil {
		return 1, err
	}
	header = append(header, '\n')
	var writeMu sync.Mutex
	writeMu.Lock()
	_, err = conn.Write(header)
	writeMu.Unlock()
	if err != nil {
		return 1, err
	}
	if request.Stdin {
		go func() {
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := stdin.Read(buffer)
				if n != 0 {
					if err := writeDaemonFrame(conn, &writeMu, daemonFrameStdin, buffer[:n]); err != nil {
						return
					}
				}
				if readErr != nil {
					_ = writeDaemonFrame(conn, &writeMu, daemonFrameEOF, nil)
					return
				}
			}
		}()
	}

	reader := bufio.NewReader(conn)
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			return 1, err
		}
		switch typ {
		case daemonFrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 1, fmt.Errorf("invalid daemon status frame length %d", len(payload))
			}
			return int(binary.BigEndian.Uint32(payload)), nil
		default:
			return 1, fmt.Errorf("unexpected daemon frame %q", typ)
		}
	}
}

func startDaemonProcess(socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return fmt.Errorf("creating daemon directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding ttos executable: %w", err)
	}
	commandName := executable
	commandArgs := []string{"_daemon-serve", "--socket", socket}
	if sibling := filepath.Join(filepath.Dir(executable), "ttosd"); sibling != executable {
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			commandName = sibling
			commandArgs = []string{"--socket", socket}
		}
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	command := exec.Command(commandName, commandArgs...)
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("starting ttosd: %w", err)
	}
	return command.Process.Release()
}

func runDaemonControl(opts options, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "ttos: usage: ttos daemon start|status|stop")
		return 2
	}
	action := args[0]
	switch action {
	case "start":
		if conn, err := connectDaemon(opts.daemonSocket); err == nil {
			conn.Close()
			fmt.Fprintln(stdout, "ttosd already running")
			return 0
		}
		if err := startDaemonProcess(opts.daemonSocket); err != nil {
			fmt.Fprintf(stderr, "ttos: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if conn, err := connectDaemon(opts.daemonSocket); err == nil {
				conn.Close()
				fmt.Fprintln(stdout, "ttosd started")
				return 0
			}
			time.Sleep(25 * time.Millisecond)
		}
		fmt.Fprintln(stderr, "ttos: ttosd did not become ready")
		return 1
	case "status", "stop":
		conn, err := connectDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintln(stderr, "ttos: ttosd is not running")
			return 1
		}
		defer conn.Close()
		status, err := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, Action: map[string]string{"status": "ping", "stop": "stop"}[action]}, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttos: %v\n", err)
			return 1
		}
		return status
	default:
		fmt.Fprintf(stderr, "ttos: unknown daemon command %q\n", action)
		return 2
	}
}
