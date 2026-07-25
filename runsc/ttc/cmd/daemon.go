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
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/ttcvolume"
)

const (
	daemonAPIVersion       = 4
	daemonFrameStdin       = byte('I')
	daemonFrameStdinAck    = byte('A')
	daemonFrameEOF         = byte('C')
	daemonFrameResize      = byte('W')
	daemonFrameStdout      = byte('O')
	daemonFrameStderr      = byte('E')
	daemonFrameStatus      = byte('X')
	daemonFrameTerminal    = byte('T')
	daemonMaxFrame         = 1 << 20
	daemonRootlessProbeArg = "_rootless-userns-probe"
)

var (
	createDaemonRootlessProbe = createRootlessProbe
	daemonEffectiveUID        = os.Geteuid
)

type daemonWireOptions struct {
	Root             string `json:"root"`
	Runsc            string `json:"runsc"`
	DataRoot         string `json:"data_root"`
	ClientExecutable string `json:"client_executable"`
}

type daemonRequest struct {
	Version        int               `json:"version"`
	Action         string            `json:"action"`
	RequestID      string            `json:"request_id,omitempty"`
	ClientTERM     string            `json:"client_term,omitempty"`
	StderrTerminal bool              `json:"stderr_terminal,omitempty"`
	StderrWidth    int               `json:"stderr_width,omitempty"`
	Options        daemonWireOptions `json:"options,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Stdin          bool              `json:"stdin,omitempty"`
}

type daemonServer struct {
	listener              net.Listener
	unsafeNoHostIsolation bool
	dataRoot              string
	volumeCapability      ttcvolume.Capability

	stopOnce sync.Once

	reconcileMu sync.Mutex
	reconciled  map[string]bool

	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex

	eventMu  sync.Mutex
	eventSeq uint64

	stdinMu     sync.Mutex
	stdinLeases map[string]*daemonStdinLease

	sidecarMu sync.Mutex
	sidecars  map[string]*exec.Cmd
}

type daemonFrameWriter struct {
	conn net.Conn
	mu   *sync.Mutex
	typ  byte
}

// OwnConsoleSidecar tracks and reaps one ready console follower. Followers are
// intentionally not killed when the daemon shuts down: they retain the advisory
// lock and continue draining until a restarted daemon reconciles the family.
func (s *daemonServer) OwnConsoleSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("console", key, command)
}

// OwnPortSidecar tracks and reaps a published-port listener.
func (s *daemonServer) OwnPortSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("port", key, command)
}

// StopPortSidecar terminates a listener owned by this daemon. The boolean is
// false when the listener survived an older daemon and must be found through
// its advisory lock instead.
func (s *daemonServer) StopPortSidecar(key string) (bool, error) {
	s.sidecarMu.Lock()
	command := s.sidecars[key]
	s.sidecarMu.Unlock()
	if command == nil || command.Process == nil {
		return false, nil
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true, fmt.Errorf("port sidecar %q did not exit", key)
}

func (s *daemonServer) ownSidecar(kind, key string, command *exec.Cmd) error {
	s.sidecarMu.Lock()
	if _, ok := s.sidecars[key]; ok {
		s.sidecarMu.Unlock()
		return fmt.Errorf("%s sidecar %q is already daemon-owned", kind, key)
	}
	if s.sidecars == nil {
		s.sidecars = make(map[string]*exec.Cmd)
	}
	s.sidecars[key] = command
	s.sidecarMu.Unlock()
	go func() {
		_ = command.Wait()
		s.sidecarMu.Lock()
		if s.sidecars[key] == command {
			delete(s.sidecars, key)
		}
		s.sidecarMu.Unlock()
	}()
	return nil
}

func (s *daemonServer) ownedConsoleSidecars() int {
	s.sidecarMu.Lock()
	defer s.sidecarMu.Unlock()
	return len(s.sidecars)
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
		return filepath.Join(dir, "ttc", "ttcd.sock")
	}
	return filepath.Join(os.TempDir(), "ttc-"+strconv.Itoa(os.Geteuid()), "ttcd.sock")
}

func daemonIsolationMode(unsafeNoHostIsolation bool) string {
	if unsafeNoHostIsolation {
		return "unsafe-no-host-isolation"
	}
	return "rootless"
}

func directBackend(opts options, stdin io.Reader, stdout, stderr io.Writer) *runtimeBackend {
	progressTerminal, progressWidth := terminalWriterInfo(stderr)
	return &runtimeBackend{
		root:                  opts.root,
		runsc:                 opts.runsc,
		dataRoot:              opts.dataRoot,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		allowSlowVolumeCopy:   opts.allowSlowVolumeCopy,
		clientExecutable:      opts.clientExecutable,
		clientTERM:            os.Getenv("TERM"),
		progressTerminal:      progressTerminal,
		progressWidth:         progressWidth,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
	}
}

func (s *daemonServer) configureVolumeBackend(backend *runtimeBackend) {
	backend.volumeCopyMode = s.volumeCapability.Mode
	backend.allowSlowVolumeCopy = s.volumeCapability.Mode == ttcvolume.CopyModeFull
}

type daemonStopTarget struct {
	selector selector
	state    string
	depth    int
}

func stopDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var targets []daemonStopTarget
	for _, family := range families {
		parents := make(map[string]string, len(family.Branches))
		for _, branch := range family.Branches {
			parents[branch.ID] = branch.ParentID
		}
		for _, branch := range family.Branches {
			switch branch.State {
			case "running", "paused", "restarting":
			default:
				continue
			}
			depth := 0
			seen := map[string]bool{branch.ID: true}
			for parent := branch.ParentID; parent != "" && !seen[parent]; parent = parents[parent] {
				seen[parent] = true
				depth++
			}
			targets = append(targets, daemonStopTarget{
				selector: selector{container: family.ID, branch: branch.ID},
				state:    branch.State,
				depth:    depth,
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].selector.container != targets[j].selector.container {
			return targets[i].selector.container < targets[j].selector.container
		}
		if targets[i].selector.branch == defaultBranchID || targets[j].selector.branch == defaultBranchID {
			return targets[j].selector.branch == defaultBranchID
		}
		if targets[i].depth != targets[j].depth {
			return targets[i].depth > targets[j].depth
		}
		return targets[i].selector.branch < targets[j].selector.branch
	})
	var stopErrors []error
	for _, target := range targets {
		if target.state == "paused" {
			if err := backend.resume(target.selector); err != nil {
				stopErrors = append(stopErrors, fmt.Errorf("resuming %s before stop: %w", target.selector, err))
				continue
			}
		}
		if err := runStop([]string{target.selector.String()}, io.Discard, io.Discard, backend); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stopping %s: %w", target.selector, err))
		}
	}
	return errors.Join(stopErrors...)
}

func wireOptions(opts options) daemonWireOptions {
	return daemonWireOptions{
		Root: opts.root, Runsc: opts.runsc, DataRoot: opts.dataRoot,
		ClientExecutable: opts.clientExecutable,
	}
}

func optionsFromWire(w daemonWireOptions) options {
	return options{
		root: w.Root, runsc: w.Runsc, dataRoot: w.DataRoot,
		clientExecutable: w.ClientExecutable,
	}
}

// RunDaemon serves the versioned TTC local API. It is used by the standalone
// ttcd binary and by ttc's private daemon entry point.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == daemonRootlessProbeArg {
		return 0
	}
	socket := defaultDaemonSocket()
	unsafeNoHostIsolation := false
	allowSlowVolumeCopy := false
	daemonDataRoot := dataRoot()
	for len(args) != 0 {
		switch {
		case args[0] == "--socket" && len(args) >= 2:
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		case args[0] == "--unsafe-no-host-isolation":
			unsafeNoHostIsolation = true
			args = args[1:]
		case args[0] == "--allow-slow-volume-copy":
			allowSlowVolumeCopy = true
			args = args[1:]
		case args[0] == "--data-root" && len(args) >= 2:
			daemonDataRoot = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--data-root="):
			daemonDataRoot = strings.TrimPrefix(args[0], "--data-root=")
			args = args[1:]
		default:
			fmt.Fprintf(stderr, "ttcd: unknown option %q\n", args[0])
			return 2
		}
	}
	if socket == "" {
		fmt.Fprintln(stderr, "ttcd: socket path must not be empty")
		return 2
	}
	if daemonDataRoot == "" {
		fmt.Fprintln(stderr, "ttcd: data root must not be empty")
		return 2
	}
	if unsafeNoHostIsolation {
		fmt.Fprintln(stderr, "ttcd: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
	} else if err := preflightDaemonIsolation(false); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	manager, err := ttcvolume.OpenManager(daemonDataRoot, allowSlowVolumeCopy)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: initializing disk-backed volumes: %v\n", err)
		return 1
	}
	if err := serveDaemon(socket, unsafeNoHostIsolation, daemonDataRoot, manager.Capability(), stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	return 0
}

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if unsafeNoHostIsolation || daemonEffectiveUID() == 0 {
		return nil
	}
	if err := createDaemonRootlessProbe(); err != nil {
		hint := "permit unprivileged user namespaces in the host kernel and LSM policy"
		if data, readErr := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); readErr == nil && strings.TrimSpace(string(data)) == "1" {
			hint = "the host AppArmor policy restricts unprivileged user namespaces; grant userns permission to TTC/runsc"
		}
		return fmt.Errorf("rootless mode is unavailable: creating a disposable user/mount namespace: %w; %s, or restart with --unsafe-no-host-isolation for trusted workloads", err, hint)
	}
	return nil
}

func createRootlessProbe() error {
	command := exec.Command("/proc/self/exe", daemonRootlessProbeArg)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := command.Run(); err != nil {
		return err
	}
	return nil
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

func serveDaemon(socket string, unsafeNoHostIsolation bool, dataRoot string, volumeCapability ttcvolume.Capability, stdout, stderr io.Writer) error {
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
		listener:              listener,
		unsafeNoHostIsolation: unsafeNoHostIsolation,
		dataRoot:              dataRoot,
		volumeCapability:      volumeCapability,
		reconciled:            make(map[string]bool),
		mutationLocks:         make(map[string]*sync.Mutex),
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "ttcd listening on %s (API v%d, isolation=%s, volume-copy=%s)\n", socket, daemonAPIVersion, daemonIsolationMode(unsafeNoHostIsolation), volumeCapability.Mode)
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
			fmt.Fprintf(serverErrors, "ttcd: rejecting peer: %v\n", err)
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
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStderr, []byte("ttcd: "+err.Error()+"\n"))
		_ = writeDaemonStatus(conn, &writeMu, 1)
	}
	if request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	switch request.Action {
	case "ping":
		payload, _ := json.Marshal(map[string]any{
			"version":                  daemonAPIVersion,
			"pid":                      os.Getpid(),
			"isolation_mode":           daemonIsolationMode(s.unsafeNoHostIsolation),
			"unsafe_no_host_isolation": s.unsafeNoHostIsolation,
			"data_root":                s.dataRoot,
			"volume_copy_mode":         s.volumeCapability.Mode,
			"volume_filesystem_type":   s.volumeCapability.FilesystemType,
			"volume_device":            s.volumeCapability.Device,
		})
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		if request.Options != (daemonWireOptions{}) {
			opts := optionsFromWire(request.Options)
			opts.unsafeNoHostIsolation = s.unsafeNoHostIsolation
			opts.dataRoot = s.dataRoot
			backend := directBackend(opts, strings.NewReader(""), io.Discard,
				daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
			s.configureVolumeBackend(backend)
			backend.consoleSidecars = s
			unlock := s.lockDataRoot(opts.dataRoot)
			err := stopDaemonContainers(backend)
			unlock()
			if err != nil {
				writeError(fmt.Errorf("stopping managed containers: %w", err))
				return
			}
		}
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.stopOnce.Do(func() { s.listener.Close() })
		return
	case "mutation-control":
		status := s.handleMutationControl(request.Options.DataRoot, request.Args, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout}, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
		_ = writeDaemonStatus(conn, &writeMu, status)
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
	terminalInput := newDaemonTerminalInput(stdinReader, conn, &writeMu)
	go receiveDaemonInput(reader, stdinWriter, request.Stdin, terminalInput)

	opts := optionsFromWire(request.Options)
	opts.unsafeNoHostIsolation = s.unsafeNoHostIsolation
	opts.dataRoot = s.dataRoot
	cleanArgs, leaseKey, takeover, err := daemonAttachStdinLease(opts, request.Args, request.Stdin)
	if err != nil {
		stdinReader.Close()
		writeError(err)
		return
	}
	request.Args = cleanArgs
	if leaseKey != "" {
		release, err := s.acquireStdinLease(leaseKey, takeover, func() {
			_ = stdinWriter.CloseWithError(errDetached)
			_ = conn.Close()
		})
		if err != nil {
			stdinReader.Close()
			writeError(err)
			return
		}
		defer release()
	}
	backend := directBackend(opts, terminalInput,
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout},
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
	s.configureVolumeBackend(backend)
	backend.clientTERM = request.ClientTERM
	backend.progressTerminal = request.StderrTerminal
	backend.progressWidth = request.StderrWidth
	backend.consoleSidecars = s
	if request.Args[0] == "events" {
		status := 0
		if err := s.streamEvents(opts.dataRoot, request.Args[1:], backend.stdout); err != nil {
			fmt.Fprintf(backend.stderr, "ttc: %v\n", err)
			status = 1
		}
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	s.reconcile(backend)
	unlock := s.lockMutation(opts.dataRoot, request.Args)
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		record, replay, err := beginDaemonMutation(opts.dataRoot, request)
		if err != nil {
			unlock()
			stdinReader.Close()
			writeError(err)
			return
		}
		if replay {
			unlock()
			stdinReader.Close()
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
			return
		}
		var mutationStdout, mutationStderr bytes.Buffer
		mutationBackend := directBackend(opts, stdinReader, &mutationStdout, &mutationStderr)
		s.configureVolumeBackend(mutationBackend)
		mutationBackend.consoleSidecars = s
		status := runCommand(request.Args, mutationBackend.stdout, mutationBackend.stderr, mutationBackend)
		s.recordCommandEvents(opts.dataRoot, request.Args, status)
		completed, err := completeDaemonMutation(opts.dataRoot, record, status, mutationStdout.Bytes(), mutationStderr.Bytes())
		if err != nil {
			fmt.Fprintf(&mutationStderr, "ttcd: %v; command outcome is indeterminate\n", err)
			status = 1
		} else {
			record = completed
		}
		unlock()
		stdinReader.Close()
		if err == nil {
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
		} else {
			writeDaemonBufferedResult(conn, &writeMu, mutationStdout.Bytes(), mutationStderr.Bytes(), status)
		}
		return
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	unlock()
	stdinReader.Close()
	_ = writeDaemonStatus(conn, &writeMu, status)
}

func writeDaemonBufferedResult(conn net.Conn, writeMu *sync.Mutex, stdout, stderr []byte, status int) {
	stdoutWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStdout}
	stderrWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStderr}
	_, _ = stdoutWriter.Write(stdout)
	_, _ = stderrWriter.Write(stderr)
	_ = writeDaemonStatus(conn, writeMu, status)
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
	case "cp":
		_, _, _, err := parseCopyCommandArgs(args[1:])
		return err == nil
	case "run":
		return daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !daemonCommandBoolFlag(args[1:], 'a', "--attach")
	case "image":
		return len(args) > 1 && (args[1] == "pull" || args[1] == "rm" || args[1] == "remove")
	case "branch":
		return len(args) > 1 && (args[1] == "rm" || args[1] == "remove")
	case "port":
		return len(args) > 1 && (args[1] == "publish" || args[1] == "unpublish")
	default:
		return false
	}
}

// daemonCommandNeedsMutationJournal identifies state-changing commands whose
// small output can be durably buffered for retry. Container-to-host cp still
// takes the lifecycle lock, but its arbitrarily large archive is streamed.
func daemonCommandNeedsMutationJournal(args []string) bool {
	if !daemonCommandNeedsMutationLock(args) {
		return false
	}
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	// Explicit image pulls are content-addressed and naturally idempotent.
	// Stream their progress live instead of buffering it for mutation replay.
	if len(args) != 0 && args[0] == "pull" {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "pull" {
		return false
	}
	if len(args) == 0 || args[0] != "cp" {
		return true
	}
	_, _, destination, err := parseCopyCommandArgs(args[1:])
	if err != nil {
		return false
	}
	_, destinationContainer, err := splitCopyContainerEndpoint(destination)
	return err == nil && destinationContainer
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
	return s.lockDataRoot(dataRoot)
}

func (s *daemonServer) lockDataRoot(dataRoot string) func() {
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

func receiveDaemonInput(reader *bufio.Reader, writer *io.PipeWriter, allowStdin bool, terminal *daemonTerminalInput) {
	var stdinQueue *daemonStdinQueue
	if allowStdin {
		stdinQueue = newDaemonStdinQueue(writer, func(bytes int) error {
			return writeDaemonFrame(terminal.conn, terminal.writeMu, daemonFrameStdinAck, encodeDaemonStdinAcknowledgement(bytes))
		})
	}
	stdinOpen := allowStdin
	fail := func(err error) {
		if stdinQueue != nil {
			stdinQueue.closeWithError(err)
		}
	}
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			fail(err)
			return
		}
		switch typ {
		case daemonFrameStdin:
			if !stdinOpen {
				fail(errors.New("unexpected stdin frame"))
				return
			}
			if !stdinQueue.enqueue(payload) {
				fail(fmt.Errorf("daemon stdin exceeded the %d-byte flow-control window", daemonStdinWindowBytes))
				return
			}
		case daemonFrameEOF:
			if stdinOpen {
				stdinOpen = false
				stdinQueue.closeWithError(nil)
			}
		case daemonFrameResize:
			rows, cols, err := decodeDaemonTerminalSize(payload)
			if err != nil {
				fail(err)
				return
			}
			terminal.updateSize(rows, cols)
		default:
			fail(fmt.Errorf("unexpected client frame %q", typ))
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
		container, containerErr := backend.containerStore().LoadContainer(family.ID)
		for _, branch := range family.Branches {
			if branch.State != "running" && branch.State != "paused" {
				continue
			}
			target := selector{container: family.ID, branch: branch.ID}
			_, _ = backend.ensureConsoleDrain(target, false)
			if containerErr == nil {
				_ = backend.ensurePublishedPortsForBranch(container, branch.ID)
			}
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
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "cp":
		_, source, _, err := parseCopyCommandArgs(args[1:])
		return err == nil && source == "-"
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
	stderrTerminal, stderrWidth := terminalWriterInfo(stderr)
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", Options: wireOptions(opts),
		Args: append([]string(nil), args...), Stdin: daemonCommandUsesStdin(args), ClientTERM: os.Getenv("TERM"),
		StderrTerminal: stderrTerminal, StderrWidth: stderrWidth,
	}
	if daemonCommandNeedsMutationJournal(args) && opts.dataRoot != "" {
		requestID, err := newDaemonRequestID()
		if err != nil {
			return 1, err
		}
		request.RequestID = requestID
	}
	attempts := 1
	if request.RequestID != "" && !request.Stdin {
		attempts = 2
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			return 1, err
		}
		commandStdout, commandStderr := stdout, stderr
		var bufferedStdout, bufferedStderr bytes.Buffer
		if request.RequestID != "" {
			commandStdout, commandStderr = &bufferedStdout, &bufferedStderr
		}
		status, err := exchangeDaemon(conn, request, stdin, commandStdout, commandStderr)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if request.RequestID != "" {
			if _, err := stdout.Write(bufferedStdout.Bytes()); err != nil {
				return 1, err
			}
			if _, err := stderr.Write(bufferedStderr.Bytes()); err != nil {
				return 1, err
			}
		}
		return status, nil
	}
	if request.RequestID != "" {
		return 1, fmt.Errorf("daemon mutation request %s: %w", request.RequestID, lastErr)
	}
	return 1, lastErr
}

func connectRunningDaemon(socket string) (net.Conn, error) {
	conn, err := connectDaemon(socket)
	if err != nil {
		return nil, fmt.Errorf("ttcd is not running; start it explicitly with 'ttc daemon start': %w", err)
	}
	return conn, nil
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
	stdinFlow := newDaemonStdinFlow()
	defer stdinFlow.close()
	if request.Stdin {
		go func() {
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := stdin.Read(buffer)
				if n != 0 {
					if err := stdinFlow.reserve(n); err != nil {
						return
					}
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

	terminal := newDaemonClientTerminal(stdin, func(typ byte, payload []byte) error {
		return writeDaemonFrame(conn, &writeMu, typ, payload)
	})
	defer terminal.Close()

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
		case daemonFrameStdinAck:
			bytes, err := decodeDaemonStdinAcknowledgement(payload)
			if err != nil {
				return 1, err
			}
			if err := stdinFlow.acknowledge(bytes); err != nil {
				return 1, err
			}
		case daemonFrameTerminal:
			if err := terminal.configure(payload); err != nil {
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

func daemonLogPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "ttcd.log")
}

func startDaemonProcess(socket, dataRoot string, unsafeNoHostIsolation, allowSlowVolumeCopy bool) (string, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return "", nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("finding ttc executable: %w", err)
	}
	commandName := executable
	commandArgs := []string{"_daemon-serve", "--socket", socket, "--data-root", dataRoot}
	if sibling := filepath.Join(filepath.Dir(executable), "ttcd"); sibling != executable {
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			commandName = sibling
			commandArgs = []string{"--socket", socket, "--data-root", dataRoot}
		}
	}
	if unsafeNoHostIsolation {
		commandArgs = append(commandArgs, "--unsafe-no-host-isolation")
	}
	if allowSlowVolumeCopy {
		commandArgs = append(commandArgs, "--allow-slow-volume-copy")
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer devNull.Close()
	logPath := daemonLogPath(socket)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("opening ttcd log %q: %w", logPath, err)
	}
	defer logFile.Close()
	command := exec.Command(commandName, commandArgs...)
	command.Stdin = devNull
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return logPath, nil, fmt.Errorf("starting ttcd: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	return logPath, done, nil
}

func reportDaemonStartupFailure(stderr io.Writer, logPath string, processErr error) {
	fmt.Fprintf(stderr, "ttc: ttcd did not become ready; startup log: %s\n", logPath)
	if processErr != nil {
		fmt.Fprintf(stderr, "ttc: ttcd exited: %v\n", processErr)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: reading startup log: %v\n", err)
		return
	}
	if diagnostic := strings.TrimSpace(string(log)); diagnostic != "" {
		fmt.Fprintln(stderr, diagnostic)
	}
}

func runDaemonControl(opts options, args []string, stdout, stderr io.Writer) int {
	if opts.unsafeNoHostIsolation && (len(args) == 0 || args[0] != "start") {
		fmt.Fprintln(stderr, "ttc: --unsafe-no-host-isolation is valid with 'daemon start' only")
		return 2
	}
	if len(args) != 0 && args[0] == "start" {
		for _, arg := range args[1:] {
			switch arg {
			case "--unsafe-no-host-isolation":
				opts.unsafeNoHostIsolation = true
			case "--allow-slow-volume-copy":
				opts.allowSlowVolumeCopy = true
			default:
				fmt.Fprintf(stderr, "ttc: unknown daemon start option %q\n", arg)
				return 2
			}
		}
		args = args[:1]
	}
	if len(args) != 0 && (args[0] == "requests" || args[0] == "request") {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		defer conn.Close()
		status, err := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, Action: "mutation-control", Options: wireOptions(opts), Args: append([]string(nil), args...)}, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "ttc: usage: ttc daemon start [--unsafe-no-host-isolation] [--allow-slow-volume-copy]|status|stop|requests|request inspect|request resolve")
		return 2
	}
	action := args[0]
	switch action {
	case "start":
		if conn, err := connectDaemon(opts.daemonSocket); err == nil {
			conn.Close()
			fmt.Fprintln(stdout, "ttcd already running; use 'ttc daemon status' to inspect its mode")
			return 0
		}
		if opts.unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttc: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
		} else if err := preflightDaemonIsolation(false); err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		logPath, processDone, err := startDaemonProcess(opts.daemonSocket, opts.dataRoot, opts.unsafeNoHostIsolation, opts.allowSlowVolumeCopy)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if conn, err := connectDaemon(opts.daemonSocket); err == nil {
				conn.Close()
				fmt.Fprintln(stdout, "ttcd started")
				return 0
			}
			select {
			case processErr := <-processDone:
				reportDaemonStartupFailure(stderr, logPath, processErr)
				return 1
			case <-time.After(25 * time.Millisecond):
			}
		}
		reportDaemonStartupFailure(stderr, logPath, nil)
		return 1
	case "status", "stop":
		conn, err := connectDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintln(stderr, "ttc: ttcd is not running")
			return 1
		}
		defer conn.Close()
		request := daemonRequest{Version: daemonAPIVersion, Action: map[string]string{"status": "ping", "stop": "stop"}[action]}
		if action == "stop" {
			request.Options = wireOptions(opts)
		}
		status, err := exchangeDaemon(conn, request, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	default:
		fmt.Fprintf(stderr, "ttc: unknown daemon command %q\n", action)
		return 2
	}
}
